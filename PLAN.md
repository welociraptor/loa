# taskflow

Goal: a daily workflow where a deterministic runner drains a task queue by
handing one task at a time to an agent in its own worktree, and where the result
of a day is legible afterwards without reading logs.

The store is the only shared state. Agents never write to it, so a confused
worker can waste its own attempt and nothing else.

## Where things stand

    cmd/taskflow/main.go        403   add | list | show | lease | done | fail | run
    internal/store/store.go     435   lease, deps, scopes, retries
    internal/store/explain.go   121   deps, blockers ("why is this not running")
    internal/store/migrate.go   119   numbered, transactional migrations
    internal/store/migrations/   46   0001_initial.sql
    internal/runner/runner.go   343   rounds, worktrees, judgement
    internal/runner/agent.go     78   Agent interface + pi, process-group kill
    internal/runner/prompt.go    34
    tests                     1,505   37 tests, `-race` clean

`nix flake check` builds the binary and runs the tests and the linter in the
sandbox, so the gate is one command on a clean machine. Verified end to end with
a fake `pi` on disk: lease → worktree → agent → commit check → done → worktree
removed, branch kept. Verified the argv reaching `pi`, recovery from a stale
worktree registration, and that a timed-out worker's children die with it.

Invariants that have tests behind them:

- a task is leased exactly once under concurrent claimers (8 goroutines, 2
  connections);
- two leased tasks never share a scope; dependencies gate a claim;
- a lapsed lease is retried once, then blocks;
- a failure is not retried inside the same run;
- a run that adds no commit fails regardless of what the agent reported;
- an attempt that commits nothing leaves no branch behind;
- `show` agrees with the claim predicate on every state it can explain, so the
  reason it prints is the reason the queue is holding the task;
- a database is created owner-only and an existing mode is never narrowed;
- a migration applies once, in order, and a newer database is refused.

## Constraints

- **One dependency:** `modernc.org/sqlite`. Pure Go, no cgo, static binary.
- **One binary:** the runner is a `taskflow` subcommand, not a second service.
- **The queue is the contract:** every rule above is enforced in SQL, not in Go
  call ordering, so a second process cannot bypass it.
- **`nixpkgs` unstable** for Go and the shell; `go.mod` pins the toolchain
  version that the shell provides.

## Phase 1 — quality gate

Nothing below adds a feature. The point is to make the existing surface safe to
build on: the schema can change again, the failure paths are exercised, and
there is one command that says "this is green".

- [x] **1. Ordered migrations.** `migrate` could only apply the whole schema to
  a fresh database, and the next schema change lands on one holding real tasks.
  `schema.sql` became `migrations/0001_initial.sql`; `migrate.go` embeds the
  directory, applies every file newer than `PRAGMA user_version`, one
  transaction per step, and refuses a database newer than the binary. Test:
  `TestMigrationsApplyInOrderAndOnce`, `TestMigrationOnlyAppliesWhatIsMissing`
  (proves v1 is not re-run), `TestDatabaseNewerThanBinaryIsRejected`,
  `TestMigrationNamesAreChecked`.

- [x] **2. A 0600 database.** Task bodies and agent output are private, and
  SQLite created the file by umask. It is now created `O_CREATE|O_EXCL` at 0600
  before opening; an existing file's mode is left alone. Test:
  `TestDatabaseIsCreatedOwnerOnly`, `TestExistingDatabaseModeIsLeftAlone`.

- [x] **3. `taskflow show ID`.** "Why is nothing running?" was a `sqlite3`
  session, because dependencies were enforced but not readable.
  `store.Blockers` explains a task in the claim predicate's own terms (leased,
  done, blocked, retry delay, unfinished dependency, contending scope) and
  `show` prints the fields plus that verdict. Test:
  `TestBlockersAgreeWithLease` walks six states and asserts the explanation
  agrees with what `Lease` actually claims, so the two cannot drift.

- [x] **4. The untested failure paths.** The four cases that decide whether
  this is trustworthy overnight now each have a test:
  `TestTimeoutKillsTheAgentAndItsChildren` (a hung worker is killed and its
  child with it; the task is retryable), `TestCancelledRunCleansUpAndLeavesTheLease`,
  `TestLostLeaseDoesNotClobberTheNewOwner` (a lost lease is logged, not fatal,
  and never overwrites the new owner's result),
  `TestStaleWorktreeRegistrationIsRecovered`.

- [x] **5. CLI tests.** `TestMain` builds the binary once and the tests drive it
  as a shell would: output, exit codes (0 / 1 / 2), flags after positional
  arguments, `done` without a lease, and `show` naming the blocking dependency
  and scope.

- [x] **6. `nix flake check` as the gate.** `packages.default` is a
  `buildGoModule` (vendored, hash pinned) whose `checkPhase` runs the tests and
  the linter; `checks.default` is that derivation. Verified with
  `nix build --rebuild .#checks.x86_64-linux.default`: build, 37 tests, then
  `0 issues` from the linter, all inside the sandbox with no network.

- [x] **7. `staticcheck`.** Superseded by `golangci-lint` 2.15 in the dev shell
  plus `.golangci.yml`, listing the linters one at a time instead of `standard`:
  errcheck, govet, ineffassign, staticcheck, unused, revive, gocritic, errorlint,
  bodyclose, gosec; `goimports` as the formatter with `taskflow` as a local
  import prefix. Clean today. Exemptions, each commented: errcheck on discarded
  handles, gosec `G104` (covered by errcheck) and `G204` (no shell), one
  `//nolint` where only SQL placeholders are concatenated. It runs inside the
  flake check (step 6), so it cannot rot.

- [x] **8. Process-group kill.** The agent is started with `Setpgid` and
  cancellation kills the group (`procgroup_unix.go`), so a worker's dev server
  or test run dies with it. Test: the fake agent spawns a `sleep`, the timeout
  fires, and the child pid no longer exists.

- [x] **9. Written threat model.** `README.md` now has "What bounds a worker,
  and what does not": the worktree, allowlist, no-push and scope rules on one
  side; network access, reads outside the worktree, prompt injection and the
  absence of any sandbox on the other.

### Your additions

Anything else you want in the gate before Phase 2 starts. Nothing here until
you list it.

### What the gate found

- The agent's process group had to be killed, not just the agent; the timeout
  test spawns a child that a naive kill leaves behind.
- `golangci-lint` in the sandbox needs `GOLANGCI_LINT_CACHE` exported inside
  `checkPhase`; as a derivation attribute the path is not shell-expanded.
- `taskflow show` needed deps to be readable. Dependencies were enforced in SQL
  but invisible, so explaining a stalled queue meant opening `sqlite3`.
- `gosec` flags `os.OpenFile` on the database path (`G304`) and every
  `fmt.Fprintf` in a writer chain (`errcheck`). The former is a `//nolint` with
  the reason "the caller chose this path"; the latter was a real signal —
  `show` now builds its lines and writes them once.

## Phase 2 — make it a daily thing

- [ ] **10. Timer.** A user service running `taskflow run` on a daily calendar,
  with the database in `$XDG_STATE_HOME/taskflow/tasks.db` and journald output.
  Depends on 6: the unit should run the store-path binary, not `go run`.
- [ ] **11. `taskflow report`.** Deterministic, no model: what was completed,
  failed, blocked and left new since a given time, plus the branch of each
  finished task. The reporter *agent* is then only for prose, and the daily
  summary still exists on the day the model is unavailable.
- [ ] **12. Spend visibility.** `-max` caps tasks per run, but not cost. Record
  the agent invocation's wall time per task in the store (one column, `runs`
  table if it earns its keep), so the first week shows what a day actually
  costs before any budget logic is designed.

## Phase 3 — triage

- [ ] **13. `taskflow import FILE`.** Read tasks from JSON, validate scopes and
  dependencies, create nothing on any error. The boundary exists so an agent
  can *propose* work without the ability to write the queue directly.
- [ ] **14. Triage agent.** Read-only allowlist, runs before the workers, writes
  a proposal file: reorder priorities, split oversized tasks, flag what needs a
  human, never touch `body` of an existing task. Human approval stays manual
  until the proposals have been boring for a week.

## Not planned, on purpose

- **No agent-to-agent messaging.** Coordination goes through the queue; two
  agents talking is a plan living in a context window, which is what this
  design exists to avoid.
- **No LLM scheduler.** Priority, dependency and scope rules are SQL.
- **No push, no merge, no branch deletion by an agent.** The runner keeps
  branches; a human merges them.
- **No retry storms.** `not_before` is a fixed delay, not exponential backoff —
  a daily timer makes the difference invisible and the simpler rule is the one
  that stays correct.

## Known limitations

- A hung agent is killed at `-timeout`, but its *children* are not (step 8).
- Cost is visible only as wall time (step 12); tokens are not counted.
- Only one host. Two runners pointed at the same database file would work, but
  the worktrees would collide by path; nothing prevents it.
- The queue removes a task from circulation after `max_attempts` without any
  notification; a blocked task is found by `report` (step 11) or `show`
  (step 3), not pushed to a human.
- `repo:` scopes name a directory under `-repos`. A task that genuinely spans
  two repositories has to be split by hand.