# loa

Goal: a daily workflow where a deterministic runner drains a task queue by
handing one task at a time to an agent in its own worktree, and where the result
of a day is legible afterwards without reading logs — including while it is
running, from the terminal the human already has open.

The store is the only shared state. A worker does not write to it; the
coordinator may (see "The coordinator" below). That boundary is policy, not a
sandbox: a worker with `bash` can run `loa` too.

Execution is Herdr: the runner itself lives in a pane, and each task becomes a
worktree-backed workspace with one agent in it, so a stalled or blocked worker
is something to look at rather than a timeout to interpret.

## Where things stand

    cmd/loa/main.go             444   add | list | show | lease | done | fail | run
    internal/store/store.go     440   lease, deps, scopes, retries
    internal/store/explain.go   121   deps, blockers ("why is this not running")
    internal/store/migrate.go   119   numbered, transactional migrations
    internal/store/migrations/   57   0001_initial.sql, 0002_task_workspace.sql
    internal/herdr/client.go    337   typed CLI client; JSON is the contract
    internal/runner/runner.go   314   rounds, judgement, reconciliation
    internal/runner/backend.go   63   Backend interface (the seam the fake sits on)
    internal/runner/herdr_backend.go 207   resolve, worktree, agent, blocked, close
    internal/runner/git.go       80   commit count, screen-to-summary
    internal/runner/prompt.go    34
    internal/live/live.go       187   queue joined with the session (`ps`)
    internal/dash/dash.go       364   the tab: model, keys, view (bubbletea)
    internal/report/report.go   187   the day: done, blocked, queued, open, gap
    internal/taskspec/taskspec.go 204   a proposal: strict parse, resolve, validate
    internal/triage/triage.go   148   what the triage agent reads and writes back
    internal/store/migrations/   91   0001–0005, including runs and its active index
    tests                     3,124   93 tests, `-race` clean

`nix flake check` builds the binary and runs the tests and the linter in the
sandbox, so the gate is one command on a clean machine. Verified end to end in a
live Herdr session: lease → worktree workspace → agent started and prompted →
commit check → done → workspace closed, branch kept. Thrice, two of them with a
real `pi` agent writing and committing a file.

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

Everything above survives the change below. What changes is where the agent
runs and who owns the worktree.

## Why the runner moves into Herdr

The runner was built headless: `pi --print` in a worktree it created itself,
output captured to memory, the process group killed on timeout. That is the
right shape for a timer and the wrong shape for a human who wants to watch. A
worker mid-flight is invisible until it exits; a worker waiting at an approval
dialog looks exactly like a worker that hung; and the worktree handling was
`exec git` code in this repository duplicating something Herdr already does.

Herdr (0.9.5, protocol 22) already owns the parts that were hand-rolled:

| need | Herdr |
|---|---|
| a worktree per task | `worktree create --workspace --cwd --branch --base --path --label` |
| a place for the agent | the root pane the worktree comes with |
| start / prompt / observe | `agent start\|prompt\|read\|wait` with `working`, `idle`, `blocked`, `done` |
| stop | `agent send-keys ctrl+c`, then `workspace close` |
| a human view | the tab bar; the pane is right there |

What that buys:

- **Blocked becomes visible.** `agent prompt` rejects an agent that is waiting
  at an approval dialog, and `agent wait --until blocked` reports it. The task
  fails with the pane named, and the human answers the dialog in the pane.
  Herdr already marks an agent that needs input; loa does not duplicate
  that with its own alerts.
- **The worktree code goes away.** No `git worktree add`, no stale registration
  recovery, no pruning. That machinery moves into the tool that owns it.
- **A worker can be watched, not reconstructed.** No `--print` transcript to
  read after the fact; the terminal is the record, and one tab indexes the rest
  (Phase 3).
- **Nothing is exec'd from task text.** The task names a *workspace label*;
  Herdr resolves it to a directory. A label that matches nothing fails the
  task. No path from task text reaches a filesystem call, so the `repo:`
  name validation added in Phase 1 stops being the only line of defence.

What it costs, and why the trade still holds:

- The flake gate can no longer exercise the real transport: no Herdr session
  exists inside a Nix sandbox. The runner logic stays under test behind an
  interface; the thin Herdr client is verified by hand, once per Herdr upgrade
  (step 17).
- Herdr's CLI JSON becomes a contract. Version skew is now a failure mode that
  did not exist, so the runner checks `herdr status` before it does anything.
- The runner can leave state behind — workspaces, panes, agents — where a
  subprocess could not. That state is not merely garbage: it is the point. It
  still has to be reconciled (step 15).

## Constraints

- **Two dependencies:** `modernc.org/sqlite` (pure Go, no cgo, static binary)
  and `golang.org/x/term` for raw mode and the window size. The tab used to
  carry the TUI framework `bubbletea` and, measured with `go list -deps`,
  eighteen packages of renderer behind it; step 34 removes them, because what
  the framework bought here was async commands and a select loop is the same
  thing with fewer moving parts. The view stays a plain string built by a pure
  function, so it is testable without a terminal either way.
- **One binary:** the runner and the dashboard are `loa` subcommands, not
  separate services, and the dashboard *is* where a run starts.
- **One run at a time, enforced by SQL.** A partial unique index on `runs`
  allows only one active run, so two dashboards, two panes or two processes
  cannot both spend the budget. The run itself is a row; `loa runs` is the
  record.
- **The queue is the contract:** every rule above is enforced in SQL, not in Go
  call ordering, so a second process cannot bypass it.
- **Herdr is the execution substrate.** The runner does not exec agents and
  does not create worktrees. Herdr's *state* is a cache of the queue, never the
  other way round: a task's fate is decided by its branch, not by an agent
  saying `done`. What the queue does store about an attempt is what the attempt
  produced — `branch`, `workspace_id`, `pane_id` — not what Herdr currently
  thinks about it.
- **Never close a workspace the runner cannot prove it created.** A stored id is
  the proof; a matching label is the fallback for the one window where the
  workspace exists but no row does. A label match with no stored id anywhere is
  somebody else's workspace.
- **Never `workspace close --group`.** The worktree workspace is linked to the
  primary one it came from; `--group` would close the human's project.
- **`nixpkgs` unstable** for Go and the shell; `go.mod` pins the toolchain
  version that the shell provides.

## Phase 1 — quality gate (done)

Nothing here added a feature. The point was to make the existing surface safe to
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

- [x] **3. `loa show ID`.** "Why is nothing running?" was a `sqlite3`
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
  bodyclose, gosec; `goimports` as the formatter with `loa` as a local
  import prefix. Clean today. Exemptions, each commented: errcheck on discarded
  handles, gosec `G104` (covered by errcheck) and `G204` (no shell), one
  `//nolint` where only SQL placeholders are concatenated. It runs inside the
  flake check (step 6), so it cannot rot.

- [x] **8. Process-group kill.** The agent is started with `Setpgid` and
  cancellation kills the group (`procgroup_unix.go`), so a worker's dev server
  or test run dies with it. Test: the fake agent spawns a `sleep`, the timeout
  fires, and the child pid no longer exists. *Retired by step 16*: once the
  agent lives in a pane, killing its process group is Herdr's business, not
  ours.

- [x] **9. Written threat model.** `README.md` now has "What bounds a worker,
  and what does not": the worktree, allowlist, no-push and scope rules on one
  side; network access, reads outside the worktree, prompt injection and the
  absence of any sandbox on the other.

### Your additions

The two things you added to this plan — run inside Herdr, and one tab that shows
the state and starts the run — are Phase 2 and Phase 3. Further additions go
here.

### What the gate found

- The agent's process group had to be killed, not just the agent; the timeout
  test spawns a child that a naive kill leaves behind.
- `golangci-lint` in the sandbox needs `GOLANGCI_LINT_CACHE` exported inside
  `checkPhase`; as a derivation attribute the path is not shell-expanded.
- `loa show` needed deps to be readable. Dependencies were enforced in SQL
  but invisible, so explaining a stalled queue meant opening `sqlite3`.
- `gosec` flags `os.OpenFile` on the database path (`G304`) and every
  `fmt.Fprintf` in a writer chain (`errcheck`). The former is a `//nolint` with
  the reason "the caller chose this path"; the latter was a real signal —
  `show` now builds its lines and writes them once.

## Phase 2 — run inside Herdr (done)

The order mattered: the backend interface and the fake existed before the
dispatch rewrite, so the logic kept its coverage while the real transport was
written and checked by hand against a live session.

- [x] **10. A task knows its workspace.** Migration `0002_task_workspace.sql`
  adds `workspace TEXT NOT NULL DEFAULT ''`; `loa add -workspace LABEL`
  requires it, `list` shows it, `show` prints it. A workspace is a **label**, not
  an id: `w1` is an opaque handle that means nothing to a human writing a task,
  and it is not the human's to type. Resolution happens at dispatch, exact
  match only, ambiguity or no match is a task failure with a clear message.
  `repo:` stops meaning "where the code is" — that comes from the workspace.
  It stays a contention scope, so existing tasks keep working; the `repo:` name
  check remains for that role. Test: a task with `repo:demo` and workspace
  `loa` is rejected when no workspace carries that label;
  `TestBlockersAgreeWithLease` grew no case: the claim predicate deliberately
  ignores the workspace, so a task with an unset one is still leased and then
  fails at dispatch, which is visible. `show` prints `workspace: (unset)`.
  `TestAddRoundTrip` pins the round trip and `TestAddRejectsBlankFields` the
  rejection.

- [x] **11. `internal/herdr`.** A thin typed client over the CLI: `Status`,
  `Workspaces`, `Agents`, `CreateWorktree`, `RemoveWorktree`, `CloseWorkspace`,
  `StartAgent`, `Prompt`, `Read`, `Wait`, `SendKeys`. Structs for the JSON, no
  interpretation, and a server-reported failure comes back as `*herdr.Error`
  with its code (`agent_blocked`, `agent_not_found`, …) so callers switch on a
  code rather than on text. Every call passes `--no-focus`. `Panes` was written
  and then removed: it existed only to discover a workspace's checkout, which
  Herdr resolves itself. Test: eleven tests drive a fake `herdr` binary with
  recorded output from `testdata/`, which pins argument construction and
  decoding together.

- [x] **12. A `Backend` interface and a fake.** (Named `Backend` rather than
  `Session`: it places one task's work, it is not one task's conversation.)
  `Open`, `Prompt`, `Read`, `Interrupt`, `Close`, `Label`, `TaskWorkspaces`. The
  fake makes real git worktrees in a temp directory, so runner tests still
  exercise rounds, scopes, commit judgement, blocked handling and cleanup —
  fourteen tests, no Herdr needed.

- [x] **13. Dispatch rewrite.** For each leased task: resolve the label to a
  workspace id; `worktree create --workspace <id> --path <dir>/<id>.<n>
  --branch loa/<id>.<n> --base <ref> --label "loa #<id>" --no-focus`;
  `agent start tf-<id>-<n> --kind pi --pane <root pane> -- <agent args>` (retried
  while the pane is not ready); `agent prompt tf-<id>-<n> <prompt> --wait
  --timeout <ms>`. Success is still `git rev-list --count <base>..HEAD`, with
  `<base>` read from the fresh worktree — the branch starts there, so `HEAD` is
  the base before the agent runs.

- [x] **14. A blocked agent is the human's.** `agent_blocked` (and a prompt
  rejected because the agent is already waiting) becomes `runner.ErrBlocked`.
  The runner fails the task with the workspace id and pane in `result`, leaves
  the workspace open, and does not interrupt it. The human answers the dialog
  and finishes with `loa done`/`loa fail`; the next run's
  reconciliation closes whatever is left over. Test:
  `TestBlockedAgentIsLeftForTheHuman` asserts no close and no interrupt.

- [x] **15. Cleanup and reconciliation.** `worktree remove --workspace <id>
  --force` both deletes the checkout and closes the workspace (measured, not
  assumed); if it fails, the workspace is closed on its own so at least the pane
  disappears. Never `--group`. At the start of a run, any workspace labelled
  `loa #N` whose task is missing, finished, or holding an expired lease is
  closed. Test: `TestReconcileClosesOrphanWorkspacesOnly` — an orphan is closed,
  a leased task's workspace is not. *(Superseded in part by step 18: what
  shipped is the label scan; the ownership check arrives with the stored ids.)*

- [x] **16. Retire the subprocess backend.** `Pi`, `killProcessGroup`,
  `procgroup_*.go`, `repoFor`/`checkRepoName`, `addWorktree`/`removeWorktree`,
  the `-repos` and `-pi` flags: gone. The allowlist moves to `agent start --`.
  Two git calls survive, deliberately: the commit count (the judgement) and
  `git branch --delete` for a branch whose attempt committed nothing — the
  runner asked Herdr for that branch name, so it cleans it up. Tests:
  `TestRunFailsWhenNothingIsCommitted` asserts no branch is left behind.

- [x] **17. Keep the gate honest.** Recorded JSON in
  `internal/herdr/testdata/` covers every shape the client parses, so a server
  change breaks the gate rather than a run. `README.md` carries the manual
  checklist (protocol match, one task end to end, one blocked task) for the part
  a sandbox cannot reach. Test: `nix flake check` passes with no Herdr present.

### What Phase 2 found, live

- **`--workspace` and `--cwd` are mutually exclusive.** The first real run
  failed with a usage error (exit 2, not 1, and the message is on stderr as
  plain text, not as an error document). The client now passes the workspace
  only, and Herdr resolves the repository.
- **`worktree remove` closes its workspace too**, so cleanup is one call. A
  failed agent is interrupted first, because removing the checkout from under a
  working process is not a stop.
- **A worktree's base is the commit it starts at.** Reading `HEAD` in the fresh
  worktree is simpler and safer than resolving the ref in the source checkout
  and carrying it across.
- **The worktree directory belongs to Herdr.** Passing `--path` made the runner
  responsible for a layout decision with no upside; without it the checkout
  lands where the session is configured to put it, and the path is read back
  from the result. One flag and one default directory disappeared with it.
- **`agent read` is a screen, not a transcript.** The first stored result was
  the whole pi TUI: skills banner, prompt, box drawing, status bar. `SummaryOf`
  drops decoration and keeps the last dozen lines, which is where the agent's
  own words are. The status bar still leaks in; fitting a summariser to one
  agent's TUI is not worth more.
- **A failed task delays its retry by 30 minutes**, so a re-run after fixing the
  transport found an empty queue and did nothing. That is `not_before` working
  as designed, and it is why the live check adds a fresh task instead of
  retrying.

### Starting a run

Decision: no timer. A run starts when a person or a coordinator starts it —
`s` in the dashboard, `loa run`, or `loa run -json`. Three reasons, in order of
weight:

1. The failure mode that matters is a blocked agent, and that needs a person.
   A scheduler delivers work to nobody.
2. A session started by a unit is a session the human is not looking at, which
   is the exact problem the dashboard exists to solve.
3. "Daily" is a habit plus a legible record, not an alarm clock.

What it costs: a forgotten day is a day with no work done, and nothing says so
until someone looks. `report` (step 24) covers that by naming the gap —
"nothing leased since Tuesday" is a finding, and it is the right place for it.
Revisit only if the workflow stops being supervised.

## Phase 3 — the dashboard (done)

The tab answers one question — what is running, and where is it — and every key
on it is a call that already exists. Nothing here adds policy to the queue, and
nothing here alerts: Herdr already marks an agent that needs input.

The ownership record comes first (step 18) because everything after it reads it:
`ps` shows where an attempt ran, the actions address that workspace by id, and
reconciliation decides what it may close. A task's live state is joined as:

| what | from |
|---|---|
| the row | the store, which is authoritative about status |
| the workspace | `workspace_id` stored at dispatch; the `loa #N` label when the column is empty |
| the agent | `agent list` by `workspace_id`; its `name` and `agent_status` are decoration |

- [x] **18. Own the workspaces.** `0003_task_workspace_ids.sql` adds
  `workspace_id` and `pane_id`, both `TEXT NOT NULL DEFAULT ''`. `store.Attach`
  records them at dispatch, applying only while the task is leased; it does
  **not** clear them when the task finishes, so the row keeps the last attempt's
  ids as history and the next attempt overwrites them. Reconciliation stops
  being a label scan: it closes a workspace whose id matches a stored id for a
  task nobody holds, **or** whose label matches while no row claims an id (the
  window between `worktree create` and the row write, which is the only reason
  the label has to stay). Three cases, in full: an id recorded for a task nobody
  holds is closed; an id recorded for one task but carried by a workspace
  labelled for another is left alone and logged, because it was not this
  runner's to make; an id nobody recorded is closed only when the task its label
  names is not held, since a live runner may be inside exactly that window. Ids are not reused — three create/close cycles
  gave `wD`, `wE`, `wF` — so a stale id names a dead workspace, never a
  different one, which is what makes storing them safe at all. If `Attach`
  itself fails, the task is failed and the workspace is left where it is:
  closing it would throw away the only proof of what the attempt was doing, and
  the next run's label fallback will find it. The label parser lives in
  `internal/herdr` (`TaskLabel`, `ParseTaskLabel`) so the reconciler and `ps`
  cannot disagree about what a label means. Test:
  `TestAttachRecordsWhatTheAttemptOwns`, `TestClaimsListWhatTheStoreCanProve`,
  `TestDispatchRecordsTheWorkspaceItOwns`, `TestUnrecordableWorkspaceIsLeftAlone`,
  and `TestReconcileClosesOnlyWhatItOwns` — five cases, one per rule above.

- [x] **19. `loa ps`.** One read that answers what is running: each task's
  store state, its workspace (from `workspace_id`, or from the label when the
  column is empty), and the agent Herdr reports in it (`working`, `idle`,
  `blocked`, `done`, `unknown`). One `agent list` and one `workspace list` per
  refresh, joined in Go — never one CLI call per row. The agent's name is
  decoration; the workspace id is the join, because a workspace outlives the
  agent inside it and that is precisely the state worth showing. Two unlinked
  cases are reported differently: leased with no workspace at all ("never
  started"), and a workspace with no live row ("orphan"). `--json` feeds the
  dashboard and `report`; the table is for a shell. It lives in
  `internal/live`, which depends on `internal/herdr` through a two-method
  `Session` interface, so the join is tested without a session. Test:
  `TestReadJoinsTasksWithTheirSession` (blocked agent / workspace with no agent /
  closed / never dispatched), `TestReadFindsAWorkspaceByLabelWhenNothingIsRecorded`,
  `TestReadReportsOrphanWorkspaces`, `TestReadDegradesWhenTheSessionIsUnreadable`,
  `TestSortPutsLiveWorkFirst`, plus CLI tests against the built binary.

  Two things came out of building it. A workspace labelled for a task that is
  merely undispatched is that task's row ("untracked"), not an orphan — the
  orphan section is only for labels nothing in the queue accounts for. And a
  recorded id whose workspace is gone reads "closed", which is why storing ids
  was worth doing: the alternative was claiming the attempt never ran.

- [x] **20. `loa dash`.** Full screen in one pane. Raw mode and the window
  size come from `golang.org/x/term`; everything else is plain ANSI. Rows sort
  by what is live first: leased, then blocked, then new by priority, then the
  day's finished work collapsed to a count. Redraw on a one-second tick and
  after every action. It shows state and never announces it. Test: the renderer
  is a pure `(rows, selection, size) → string` function, tested for sort order,
  selection at both edges, and 80×24 versus 200×50. What shipped is
  `internal/dash`: an 80-line-wide view with the header counts, a windowed row
  list, an orphan section, a message line and a key footer — plain text, no
  styling beyond what the column padding does. `Update` handles keys, ticks and
  results; every store or session call is a `tea.Cmd`, so the view never
  blocks. Refresh is a one-second tick, and `SnapshotMsg` is exported so a
  caller can prime the view with a read it already has (which is how the tests
  work without a terminal).

- [x] **21. Actions.** `enter` focuses the task's agent or workspace (`agent
  focus`, `workspace focus`); `x` interrupts a working agent (`send-keys
  ctrl+c`); `s`/`S` start and stop a run; `d`/`f` complete or fail the selected
  task with a one-line result prompt; `c` closes the task's workspace; `u`
  unblocks. Adding tasks stays in the CLI: a form in a TUI is not worth the
  keys. Focus uses the stored `workspace_id`/`pane_id` (falling back to the
  label), so it works for a blocked task whose agent has already exited. The
  keys are `enter` focus, `x` interrupt, `u` unblock, `c` remove the workspace,
  `d`/`f` resolve with a one-line result prompt, `s`/`S` start and stop a run,
  `q` quit.

  Two store methods were missing, and both are the *human's* override rather
  than the runner's: `Unblock` (blocked → new, `not_before` cleared, attempts
  kept, so it buys exactly one more attempt) and `Resolve` (done or blocked,
  unconditionally — `Complete`/`Fail` require a lease, and a human answering a
  blocked agent does so hours after the run let go, so the old path could not
  finish the task at all). Test: `TestUnblockOnlyAppliesToBlockedTasks`,
  `TestResolvePromptTakesAResult`, `TestEscapeAbandonsThePrompt`,
  `TestXInterruptsTheAgentOfTheSelectedTask`, `TestEnterFocusesAndXInterrupts`,
  and `TestControlsErrorsLandInTheMessageLine`.

- [x] **22. One run at a time.** The dashboard starts the runner in a goroutine
  with a cancel function. The UI never holds a lease or a transaction, and the
  store's single connection serialises the short statements, so a running task
  does not freeze the table. A second `s` while a run is in flight is refused
  and says so, rather than queued or duplicated. Test: the refusal, and that
  stopping a run leaves the in-flight task leased and expiring on its own.

- [x] **23. The tab.** Documented, not coded: create the control workspace once
  (`herdr workspace create --cwd $XDG_STATE_HOME/loa --label loa`),
  run `loa dash` in its root pane, and leave it open. Task worktrees are
  linked worktree workspaces under the repository's own workspace, so the
  dashboard is an index rather than a container — closing a task's workspace
  never touches the tab. `README.md` carries the sequence, and a live check
  against this session drew the tab in a real pane: header counts, the three
  rows in live-first order, and the key footer.

## Phase 4 — oversight (done)

- [x] **24. `loa report`.** Deterministic, no model: what was completed,
  failed, blocked and left new since a given time; the branch of each finished
  task; the workspace of anything left open; and the gap — how long since
  anything was leased, so an unsupervised week is visible rather than silent.
  The reporter *agent* is then only for prose, and the daily summary still
  exists on the day the model is unavailable. `-since` takes a duration (`24h`)
  or a date (`2026-11-18`); `-json` carries the same sections. Workspaces are
  reported regardless of the window — one left open yesterday is still open
  today — while tasks are counted only when they changed inside it. Test:
  `TestBuildGroupsTheDay`, `TestTextRendersEverySectionAndTheGap`,
  `TestAnEmptyQueueStillStatesTheGap`, `TestWindowExcludesOlderWorkButNotOpenWorkspaces`,
  plus CLI tests for the window parsing and the JSON.

- [x] **25. Spend visibility.** `0004_run_time.sql` adds `run_ms`, measured at
  completion inside the same UPDATE: SQLite evaluates the right-hand side
  against the row's pre-update values, so `updated_at` there is still the moment
  the attempt was leased. No extra write, no Go-side timing, and an attempt that
  crashed still shows as time spent. Resolution is one second, because that is
  what the stored timestamps carry; the first run measured 16 s against 16 s of
  wall clock in the log.

  It is read in `report` (per task, plus "time in agents" for the window) and in
  `show`. The plan guessed the dashboard's count line; that was wrong — a total
  for "the day" needs a window, and the tab shows *now*. Test:
  `TestRunTimeIsMeasuredAtFinish`.

## Phase 5 — triage (done)

- [x] **26. `loa import [FILE|-]`.** Read a proposal, validate it whole,
  apply it in one transaction or not at all. The proposal boundary exists so an
  agent can propose work without the ability to write the queue directly; a
  trusted coordinator may apply it, but the proposing agent cannot apply its own
  document. `internal/taskspec`
  parses strictly (unknown fields are rejected, not ignored) and resolves `after`
  references — a title in the same document — into positions, so a proposal can
  say "this comes after that" before either task has an id. It also rejects
  cycles: a task waiting on itself would sit in the queue forever and look like a
  runner bug. `store.Apply` creates the batch and reprioritises existing tasks in
  one transaction. `-dry-run` prints the validated document and creates nothing,
  which is the human's review step. Test: `TestParseAcceptsAnObjectOrABareArray`,
  `TestParseRejectsUnknownFieldsAndJunk`, `TestValidateResolvesAfterReferences`,
  `TestValidateRejectsUnknownAfterAndDuplicateTitles`, `TestValidateRejectsCycles`,
  `TestValidateRequiresWorkspaceAndSaneNumbers`,
  `TestApplyCreatesTheBatchInOneTransaction`, `TestApplyRollsBackWhenAnyPartFails`,
  plus CLI tests for apply, refusal and dry run.
- [x] **27. Triage agent.** `loa triage` writes the queue — bodies, states,
  dependencies — into a scratch directory, opens a workspace there, starts an
  agent with `read,grep,find,ls,write` (no `bash`) and the instruction to write
  `proposal.json`, then validates what came back and keeps it under
  `$XDG_STATE_HOME/loa/proposals/`. It never applies anything: the queue is
  only touched by `loa import`, run by a person or a trusted coordinator. A
  blocked agent is left open with its workspace named, as with a task.

  Live check on a two-task queue it had never seen: it split a task whose title
  and body disagreed, chained the parts with `after`, gave the first part the
  higher priority because it blocks the others, and produced four notes including
  the title/body mismatch and a bodiless task. `import` then created the three
  tasks with their dependency chain, and the workspace closed itself.

  Two honest limits: the "read-only" allowlist is a tool list, not a sandbox — an
  agent with `write` can write anywhere under its scratch directory, which is
  why the directory holds nothing else; and a triage run costs one model call,
  which is the only place in this design where a model talks to a model's
  output without a commit in between.

  Notes are printed by `import` as well as by `triage`, because they are the part
  a person has to act on.

## Phase 6 — what dogfooding found

`loa` was used to add `QUICKSTART.md` and the `unblock`/`resolve` commands. The
loop worked — two tasks, two commits, four minutes — and it surfaced one design
limit worth fixing rather than documenting.

- [ ] **28. Dependencies order work; they do not stack it.** A dependent task
  branches from the base commit, not from its dependency's branch, so it cannot
  see what the earlier task committed. A doc task that depends on a code task
  therefore describes the pre-merge world: the QUICKSTART written this way claims
  a blocked task is settled "from the dashboard" and does not mention the `loa
  unblock` / `loa resolve` that the task it depends on had just added. The
  branch names are deterministic and `Complete` records them, so the fix is to
  branch from the newest *done* dependency's branch when the task has one:
  `Base` is already a parameter, so this is a lookup, not a new concept. Test: a
  task with a done dependency opens a worktree whose HEAD contains that
  dependency's commit; a task whose dependency was re-queued still branches from
  the base.

## Phase 7 — the factory (planned)

A plan becomes a workflow: a planner turns a goal into phases of parallel
tasks, each change is implemented and reviewed, and an approved change with a
green check is merged without a person. The queue stays the only coordination
channel. What is new is that a task carries a *role*, the reviewer's deliverable
is a *verdict* instead of a commit, and `loa` performs a *merge* as a
deterministic transition.

Two documents, both data:

- **The workflow** is authored by a person and versioned with the repository:
  role → prompt file, the check command, the merge policy and the stage list.
  It changes rarely, so it is reviewable like code.
- **The plan** is authored by the planner from a goal: phases, and the tasks
  inside them.

Compilation is where a scheduler would otherwise go: every task in phase *N*
depends on every task in phase *N−1*, and every implementation task gets a
review task depending on it. The claim predicate is then the whole scheduler —
a phase is a barrier over dependencies, and a parallel phase is the absence of
edges between its tasks. `taskspec` already parses a proposal, resolves `after`
by title and rejects cycles; the plan is that format plus `phases` and `role`.

Three decisions hold the trust model:

- **A green check is mandatory for auto-merge.** The runner runs the workflow's
  check in the review worktree and judges it by exit status. A reviewer's
  opinion is not a build: Phase 2 shipped a branch whose tests were green in its
  own worktree and failed the gate, and nothing caught it until the merge.
  `approve` with a red check is `changes`.
- **`changes` needs a human.** A verdict that asks for changes carries a
  proposal in the plan format; the runner writes it under `proposals/` and
  nothing applies it. A person edits it, and applying it is the approval that
  starts the next round of implementation. The loop is human-paced on purpose:
  an agent that can turn its own objection into work is an LLM scheduler.
- **Prompts live in the repository**, as `prompts/<role>.md`. A prompt is code —
  reviewable, diffable, and the reason a verdict can be trusted to have been
  asked the right question. The task row records the prompt's name and content
  hash at dispatch, so a verdict is attributable to the instructions that
  produced it. The runner still appends its own contract (commit, no push, stay
  in scope), because that is the part the judgement enforces.

Roles are a column, not an execution path: `implement` commits, `review` writes
`verdict.json`, `plan` writes `plan.json`. The role selects the prompt file and
the tool set, and the other roles' deliverables are validated the way a
proposal is. A verdict is `approve`, `changes` or `blocked`; `blocked` is the
existing path — the pane stays open and a person answers it.

Merge is the only new capability, and it is deterministic: `loa merge ID` runs
one clean `git merge --no-ff` of the task's branch into the named branch of the
workspace checkout, refuses a dirty checkout, a wrong branch or a conflict,
records `merged_at` and `merge_commit`, and never pushes. Nothing an agent does
can merge, and nothing merges without an `approve` and a green check. A
workflow's `merge: auto` is acted on by the coordinator, which reads the run's
JSON and calls `loa merge` for each approved review: the guard lives in loa, the
policy in the workflow, and no model is trusted with either.

- [x] **29. A task branches from its newest done dependency.** (Step 28, as a
  prerequisite.) `Base` becomes the newest done dependency's branch with a
  recorded one, falling back to the configured base. Test: the worktree for a
  task with a done dependency contains that dependency's commit; a dependency
  that was re-queued still branches from the base.

- [x] **30. Roles and prompt files.** `0006_task_role.sql` adds `role`, `prompt`
  and `prompt_hash`, defaulting to `implement` so existing tasks keep working.
  `prompts/{planner,implementer,reviewer}.md` render the task fields and the
  role's instructions; the runner appends the contract and fails a task whose
  role has no file, naming it. `loa add -role`, and `list`/`show`/`ps` show it.
  Test: a prompt file renders; a missing one fails the task; the hash is stored.

- [x] **31. A verdict is the reviewer's deliverable.** `0007_task_verdict.sql`
  stores it. The runner hands a review
  task the diffs of the tasks it depends on, validates `.loa/verdict.json`
  (strict parse, unknown fields rejected), and stores the verdict. `approve`
  finishes the review; `changes` finishes it with the verdict recorded and the
  proposal written under `proposals/`, and queues nothing; `blocked` is the
  existing human path. A review never needs a commit, and a commit alone cannot
  finish it. Test: each verdict, a malformed document, a review with nothing to
  review.

- [x] **32. Merge, and the check it requires.** `0008_task_check.sql` records
  the check's outcome on the review task; `-check CMD` is run in the review
  worktree by the runner. `0009_task_merge.sql` records `merged_at` and
  `merge_commit` on the implementation task. `loa merge ID` merges only when the
  review verdict is `approve`, the check passed, and the workspace checkout is
  clean and on the named branch; a conflict fails it and leaves the branch for a
  person. Test: approve and green merges; a red check refuses; a conflict
  refuses; a wrong branch refuses.

- [ ] **33. The plan, and `loa apply`.** `taskspec` learns `phases` and compiles
  them into dependencies; `0010_task_plan.sql` gives them a plan id so `report`
  names the plan. `loa plan -workflow FILE GOAL` runs the planner agent like
  `triage` (a scratch workspace, `plan.json`, never applied) and `loa import`
  applies it, teaching the planner that its output is a dependency graph, not
  prose. Test: phases compile to a barrier; a plan naming a missing dependency
  is refused; `plan` writes and does not apply.

What it costs:

- Three agent calls per change (one plan per goal, one review per change), so a
  factory run is the most expensive thing in this design; cost is still visible
  only as wall time.
- The reviewer reads text an agent wrote. Prompt injection moves from "a worker
  wastes an attempt" to "a worker tries to talk a reviewer into a merge"; the
  check and the clean-merge rule bound what that can land, but `approve` is
  still a model deciding that code merges.
- Prompts are code, and a worker can edit them. The recorded hash makes drift
  visible; reviewing prompt changes is the only thing that prevents it.
- A `changes` verdict stops the workflow until a person applies the proposal, so
  a twenty-task plan halts at its first rejection. Conditional dependency edges
  (`when`) would unstick it and are deliberately deferred until this loop is
  proven.

## Phase 8 — the queue you can see (planned)

Two gaps, neither of them in the rules. The tab carries a framework it no longer
needs, and the graph the claim predicate enforces — the thing that decides what
runs next — is invisible outside `sqlite3`.

- [ ] **34. The tab loses its framework.** `bubbletea` bought async commands,
  resize handling and an alt screen, and cost twenty-odd modules on a binary
  whose other dependency is a SQLite driver, plus a `tea.Cmd` indirection in
  front of every call the tab makes. `internal/dash` keeps the state machine and
  drops the framework: `Update`/`View` stay pure values over plain messages, and
  the loop becomes one goroutine selecting over four channels — decoded keys, a
  one-second `time.Ticker`, action results and the window size — with
  `golang.org/x/term` for raw mode and `GetSize`, which is what step 20 always
  said the source would be. Alt screen, cursor and restore are plain escapes,
  restored by a `defer` and on SIGTERM, so a panic or a kill leaves a usable
  terminal; `dash` on a non-tty says so instead of emitting escapes. Nothing
  moves onto the render path: refresh, actions and the run stay in goroutines,
  because a tab that blocks on a store query is worse than no tab. `go.mod` ends
  with two direct requirements, `flake.nix`'s `vendorHash` moves, and the
  documents that justified the framework are corrected — the two duelling
  dependency bullets under Constraints, "No styling beyond bubbletea itself",
  step 20's account of what shipped, and README's dependency paragraph. Test:
  every existing `dash_test.go` case still drives the model directly — keys, the
  result prompt, `s`/`S`, the message line, selection at both edges, 80×24
  against 200×50 — with only the key type and the command plumbing changed, and
  none deleted on the grounds that it tested the framework. New: the restore
  path runs against a fake terminal that records the escapes, once on a normal
  exit and once on a panic.

- [ ] **35. `loa show` names the graph, in both directions.** Step 3 made a
  dependency readable as a *blocker* and nothing else, so a task whose
  dependencies are `done` reads exactly like a task with none, and "what is
  waiting on this?" has no answer short of SQL. `Task.Deps` becomes a field
  loaded the way `Scopes` is — one query per list, never one per row — and
  `store.Dependents` answers the other direction with the same `Dep` type.
  `show` prints `depends on:` and `needed by:` unconditionally, each edge
  carrying the other task's id, status and title, and `-` for the empty case.
  `Blockers` keeps its own wording, because it is the claim predicate's
  explanation and not an edge list; showing both is the point — one says why the
  queue is holding the task, the other says where it sits. Test: a dependency
  that is already `done` still appears; a task nothing waits on prints
  `needed by: -`; `TestBlockersAgreeWithLease` is untouched, so the explanation
  cannot drift from the claim.

- [ ] **36. `loa list` shows dependencies.** One `DEPS` column, `-` when there
  are none and `5,12` otherwise, joined the way `SCOPES` already is. A chain is
  the shape of the day's work, and a table that omits it asks a person to run
  `show` on every row to find out. Presentation only: the store work is step
  35's and the column reuses its formatter, so `list` gains no query. Test: the
  empty case and a three-task chain, through the built binary.

## The coordinator (decided)

`loa` may be driven by a coordinator agent, not only by a person. Three
decisions define that mode; each widens the old design deliberately.

- **A coordinator may write the queue.** Triage used to propose and a person
  apply, so an agent's opinion could not become work by itself. A coordinator
  exists to turn the day's state into the next queue, so it may run `loa add`
  and `loa import` itself. The boundary is now policy: a worker with `bash` can
  run `loa` too, and nothing sandboxes either. What still holds is that the
  queue is the only coordination channel, and every change is a recorded
  transition rather than a conversation.
- **The run record is a table, and one active run is SQL.** A run is not a log
  line or a flag in the dashboard: `runs` stores who started it, its budget,
  when it ended and what it settled. `loa runs` reads it, and `loa run -json`
  emits machine-readable results for a coordinator. The single-active-run
  invariant is the partial unique index `runs_one_active`, so a second process
  racing to start a run loses in SQLite, not in Go or in the tab.
- **There is no check command for a coordinator run.** Done still means a
  commit on the task's branch, and `loa run` judges that itself. A coordinator
  may read the result and decide what to queue next, but it cannot mark work
  done without a commit. Phase 7 adds a check only where a person is removed
  from the path: an auto-merge needs one. Everywhere a commit is the gate, a
  second definition of done would be free to disagree with the first.

## Not planned, on purpose

- **No timer, no daemon, no scheduler.** A run is something a person or a
  coordinator starts, and the dashboard is where a person starts it.
- **No second execution backend.** Herdr is the only way a task runs. A headless
  path would double every failure mode to save one attach.
- **No web UI.** The dashboard is a pane because the workers are panes; a
  browser tab would be further from the work than the terminal already is.
- **No TUI framework, and no styling.** No `bubbletea`, no `bubbles`, no
  `lipgloss`: the tab is padded strings drawn by our own loop (step 34), which
  is what keeps it testable and the dependency list at two.
- **No worktrees created by the runner.** Anything that touches `git worktree`
  belongs in Herdr, where the stale-registration bug is already someone else's.
- **No agent-to-agent messaging.** Coordination goes through the queue; two
  agents talking is a plan living in a context window, which is what this
  design exists to avoid.
- **No answering approval dialogs.** A blocked agent is the human's decision,
  and the dashboard makes it a two-second one.
- **No LLM scheduler.** Priority, dependency and scope rules are SQL.
- **No push, and no merge by an agent.** Loa merges an approved branch with one
  deterministic command (step 32); anything without an `approve` verdict and a
  green check is still a human's merge. No agent resolves a conflict.
- **No sandbox around the queue.** A coordinator may write it, and a worker
  with `bash` can too. The rules that matter are SQL transitions, not the
  absence of a command.
- **No retry storms.** `not_before` is a fixed delay, not exponential backoff —
  the simpler rule is the one that stays correct.

## Known limitations

- **Nothing runs unless someone starts it.** A person or a coordinator must
  start a run; a missed day is a real gap. Step 24 reports it, but nothing
  reminds you beforehand.
- **The dashboard is one terminal and one user.** `report-metadata` is
  display-only, and there is no remote view.
- **Herdr is a dependency of the run.** Version skew is checked at startup
  (`herdr status`, protocol 22 today) but not pinned: an upgrade that changes
  `agent prompt` flags will fail a run before it damages one.
- **Focus actions need a live session.** `ps` and `dash` still show the queue
  without Herdr; only the actions that address a pane are disabled.
- **A stored workspace id can outlive its workspace.** Then the row says
  "closed" rather than pretending it is live — the honest answer, and the reason
  ids are worth storing at all.
- **Leftover state is possible.** A runner that dies between `worktree create`
  and cleanup leaves a workspace open. Step 15 reconciles that at the next
  start, so the residue is one tab, not a stuck queue.
- **A blocked task waits for a human indefinitely.** That is the design: the
  run does not answer dialogs, and the lease keeps the task claimed until it
  expires.
- **The real transport is not in the gate.** Its JSON is (step 17); its
  behaviour with a live Herdr is a manual checklist.
- **Cost is visible only as wall time**, to the second, and not as tokens. It is
  also the attempt's duration, not the model's thinking time — a worker that
  spent three minutes running tests reports three minutes.
- **One active run, across sessions and dashboards.** The `runs` index refuses
  a second run sharing the database, so two sessions cannot split the queue.
  Two separate databases can still be driven independently; nothing joins
  them.
- **Dependencies order execution, not content** (step 28, fixed by step 29): two
  tasks in a chain do not share a base, so the later one cannot build on the
  earlier one's commit. Until step 29 lands, a chained plan needs a merge
  between runs.
- **The queue blocks a task after `max_attempts` without notifying anyone.** It
  is found by `report` (step 24) or by the dashboard's blocked row — both of
  which require someone to look.

