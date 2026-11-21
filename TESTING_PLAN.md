# Testing plan

This plan describes how `loa` should be tested: what the current suite already
pins, which failure modes are still unprotected, and the order in which to close
the gaps. It is deliberately specific to the packages and invariants in this
repository, not a generic Go testing checklist.

The guiding rule is that a test exists to protect a claim. Every sentence in
`README.md` about leases, scopes, dispatch, judgement, reconciliation or reports
should either have a test behind it or be listed here as a manual check.

## 1. Baseline

Measured on this worktree with Go 1.27.3:

```sh
CGO_ENABLED=0 go test ./...
CGO_ENABLED=0 go test -coverpkg=./... -coverprofile=/tmp/cover.out ./...
go tool cover -func=/tmp/cover.out
```

`CGO_ENABLED=0` is only needed in a bare shell without a C compiler; `nix
develop` provides the normal toolchain. The race detector needs cgo, so CI must
not force it off.

| package | tests | statement coverage | assessment |
|---|---:|---:|---|
| `internal/store` | 73 | 82.2% | lifecycle and migration chains are covered; remaining gaps are error injection and long-run soak |
| `internal/store` migrations (subset of the 73) | 10 | part of store | the shipped SQL is run from every version with data; rollback, failure atomicity and the schema golden are covered |
| `internal/herdr` | 43 | 98.3% | transport commands and recorded fixtures are covered; the live session stays manual |
| `internal/runner` | 57 | 84.8% | runner logic and `herdr_backend.go` are exercised; remaining gaps are rare cleanup/error combinations |
| `internal/live` | 5 | 82.4% | join is good; duplicate-label and degraded-session cases are still thin |
| `internal/report` | 5 | 91.9% | golden and boundary cases landed; the `TotalRun` double count stays pinned by its test |
| `internal/taskspec` | 14 | 91.0% | strong parser coverage; long-running fuzz smoke is still a nightly job |
| `internal/triage` | 8 | 84.1% | modes and write failures are covered; the CLI flow is covered in `cmd/loa` |
| `internal/daemon` | 35 | 79.0% | socket, runs, reconciliation and the scheduler are covered; live Herdr recovery stays manual |
| `workflows` | 4 | 84.0% | strict parsing and merge policy are covered |
| `cmd/loa` | 73 CLI tests | 21.1% instrumented | the tests mostly build and run the binary, so child-process coverage is not collected; the direct unit helpers are measured |
| **total** | **317** | **66.8%** | enough to trust the queue's invariants; the live Herdr transport and soak runs remain manual/nightly |

The low instrumented number for `cmd/loa` is mostly an instrumentation artefact:
the CLI has 73 process-level tests, principally in `main_test.go` and
`daemon_test.go`. Run the child-process coverage collection in §4.11 to see the
remaining branches; do not read the package figure as those tests being absent.

## 2. Philosophy and risk-based priorities

### Principles

1. **Test the queue's invariants in SQL, not in test-only ordering.** The store
   is the contract. Tests should call `Store` methods and assert database state,
   not assume the runner happens to call things in a safe order.
2. **The fake is a seam, not a simulation of Herdr.** `runner.Backend`,
   `live.Session` and the recorded `herdr` CLI fixtures already let logic run
   without a session. Do not build a fake Herdr server.
3. **Determinism is part of correctness.** Use `t.TempDir()`, fixed timestamps
   where the code accepts them, and no wall-clock sleeps except where the test
   is specifically measuring time. Every test must pass offline, with no
   `HERDR_ENV`, and from any working directory.
4. **One dependency is still the rule.** Fuzz tests, golden files, temp
   directories and process execution use the standard library. Do not add
   `testify`, `go-cmp`, `teatest` or a SQLite helper module. The existing
   `modernc.org/sqlite` is the only non-test dependency.
5. **Coverage is a map, not a target.** Do not add assertions that only execute
   lines. Raise coverage where an uncovered branch corresponds to a user-visible
   failure; leave trivial getters alone.
6. **A test must be able to fail.** Before adding one, identify the bug it
   catches and confirm the failure message says what broke. Prefer asserting the
   stored row and the observable output over asserting internal call order.

### Risk register

| risk | impact | current evidence | planned evidence |
|---|---|---|---|
| a task is leased twice | two agents edit one scope; lost work | `TestConcurrentLeasesNeverDuplicate` | race/shuffle soak, reaper boundaries, multi-connection tests |
| a migration damages an existing database | data loss on upgrade | only synthetic migrations | real v1→v4 fixtures, data-preservation and rollback tests |
| the runner creates a worktree for the wrong label | work happens in the wrong repository | `herdr.resolve` untested | backend resolve/Open tests with a scripted fake CLI |
| a blocked agent is torn down | the human loses the approval dialog | `TestBlockedAgentIsLeftForTheHuman` (fake backend only) | `Herdr.Prompt` blocked-code test and manual check |
| success is judged from the agent's summary | false `done`, missing commit | commit-count tests | failure matrix, git edge cases, result truncation |
| a failed attempt is retried in the same run | burns attempts and starves the queue | store retry-delay test only | runner-level budget/retry test |
| the CLI parses a flag differently than scripts expect | wrong task changed | 73 CLI tests | full command/flag matrix, env precedence |
| `report` silently lies about a quiet day | unsupervised week looks normal | 4 report tests | fixed-snapshot golden output and boundary cases |
| the real Herdr transport drifts on upgrade | run fails or misbehaves | client fixtures + manual checklist | expand fixtures; run backend against scripted Herdr; manual end-to-end |

### Priority order

- **P0** — anything that can corrupt the queue, lose work, or dispatch to the
  wrong place: store lifecycle, migrations, Herdr label resolution, dispatch
  cleanup, success judgement.
- **P1** — contract regressions that make the tool lie or break scripts: CLI
  flags/output, `ps`/`report`/prompt goldens, parser edge cases.
- **P2** — operability: triage file handling.
- **P3** — confidence and durability: race/shuffle/soak, fuzzing, coverage
  collection, testability seams that P0/P1 tests actually need.

## 3. Test layers and doubles

### Layers

| layer | what runs | where |
|---|---|---|
| package unit | pure functions, `Store` against a real SQLite file, `Model.Update` | `internal/*/*_test.go` |
| fixture contract | `herdr.Client` against a fake `herdr` script and `testdata/*.json` | `internal/herdr` |
| backend integration | `runner.Herdr` against the same fake CLI plus real git worktrees | `internal/runner` |
| CLI integration | the built `loa` binary, a temp DB, a temp `PATH`, fake Herdr for session commands | `cmd/loa` |
| manual | real Herdr, real terminal, real agent | checklist in §10 |

### Herdr fake binary

The current `fake` in `internal/herdr/client_test.go` is a shell script that
concatenates `"$*"`. That proves the command text but not argument boundaries.
Extend the convention rather than replacing it:

- Log each argument on its own line with NUL separation:
  `for a in "$@"; do printf '%s\0' "$a"; done >> "$ARGS_FILE"`. Tests split on
  `\0` and can prove that a label containing a space, newline or `--evil` stayed
  one argument.
- Keep fixtures in `internal/herdr/testdata/`, named for the command
  (`status.json`, `workspace_list.json`, `worktree_create.json`,
  `workspace_create.json`, `agent_list.json`) and for failures
  (`error_blocked.json`, `error_agent_not_ready.json`,
  `status_not_running.json`, `status_incompatible.json`,
  `malformed_result.json`, `worktree_create_missing_ids.json`).
- Add `testdata/README.md` stating what each fixture is, that it is recorded
  server output, the protocol it was captured against (22), and the exact
  command used to capture it. Fixtures must not be hand-edited to make a test
  pass; a schema change is the signal the gate exists to catch.
- Failure injection belongs in environment variables (`FAIL_COMMAND`,
  `BLOCK_PROMPT`, `STATUS_FIXTURE`) in the script, not in production code.
- Do not let the fake read task text or write into the repository. It is a
  transport double, not an agent.

### `fakeBackend`

`internal/runner/runner_test.go` already creates real git worktrees, which is
the right level of realism. Extend it with fields the missing tests need:

- `promptErr` (separate from `agent`), `readErr`, `interruptErr`, `closeErr`,
  `taskWorkspacesErr`;
- `prompts []string` so the runner's rendered contract can be asserted;
- `openHook` in addition to the existing `onOpen` store-disturbance hook;
- an explicit `closeCalls` list so `Close` after a failed `Interrupt` is visible.

The backend's own logic (`runner/herdr_backend.go`) cannot use this fake because
it is concrete against `*herdr.Client`. Test it with the scripted fake `herdr`
binary and a real temporary git repository; `worktree create` must actually run
`git worktree add` and print a `worktree_create` JSON document whose paths point
at that checkout. That is a small shell script plus `git`, both already required
by the gate.

### SQLite and temporary directories

- Every store test uses `filepath.Join(t.TempDir(), "tasks.db")` as
  `store.Open` requires a path and the file mode assertions need a real file.
- Do **not** use `:memory:`: `Store` sets `SetMaxOpenConns(1)` and migrations
  use transactions, and an in-memory database is per connection. A file in
  `t.TempDir()` is both faster to reason about and closer to production.
- Raw migration tests use the existing pattern in
  `internal/store/migrate_test.go`: `sql.Open("sqlite", "file:"+path)` in
  `t.TempDir()`. Reuse `rawDB`, `version` and `hasTable`; add `execSQL` and
  `queryOne` only if a test needs them.
- Cleanup is automatic. Do not add a `TestMain` that deletes shared state.
- For tests that need two connections to one file (the CLI next to the runner),
  open two `Store`s on the same path, as `TestConcurrentLeasesNeverDuplicate`
  does.

### Clock and concurrency seams

The store already accepts `now` in `Lease`, which makes reap/retry tests
deterministic. `Complete`, `Fail`, `Resolve` and `Unblock` call `time.Now()`
internally, which is why `TestFailRequeuesUntilAttemptsAreSpent` has to mix
clocks. Add an unexported `now func() time.Time` field to `Store` (default
`time.Now`) and an unexported test-only constructor, or set the field directly
in white-box tests in `package store`. This is a testability change with no
production behaviour change; it is justified only when the first imprecise test
needs it (roadmap D3), not before.

The same applies to `runner.Herdr.startAgent`'s hard-coded 500 ms retry. Test
the retry-once path with a fake that succeeds on the second call (about 500 ms),
or introduce a `retryDelay` field alongside `ReadyTimeout` so the test can set
10 ms. Prefer the seam if the retry tests become a CI delay.

## 4. Per-package strategy

### 4.1 `internal/store` (P0)

Existing coverage is strong on the lease predicate and weak on the human
override paths and input validation. The missing tests:

| test | case |
|---|---|
| `TestUnblockBuysExactlyOneAttempt` | blocked task (`MaxTries: 2`, attempts 2) → `Unblock` → `new`, `NotBefore` zero, attempts unchanged; next `Lease` increments to 3; a failure blocks again |
| `TestUnblockRefusesNonBlocked` | `new`, `leased`, `done`, missing id → `ErrNotLeased` (pin this; consider whether a missing id should be `ErrNotFound`) |
| `TestResolveSettlesOutsideTheLease` | `new` → done, blocked → blocked/done, done → blocked; `RunMS` unchanged; missing id → `ErrNotLeased` |
| `TestResolveRefusesALeasedTask` | lease held → `ErrNotLeased`, row untouched |
| `TestAddRollsBackOnScopeOrDependencyFailure` | duplicate scope, duplicate dep, dep id 999, empty scope: no task row is left behind |
| `TestAddDefaultsMaxAttempts` | `MaxTries` 0 and negative → 2; `MaxTries` 1 stays 1 |
| `TestAddRejectsMissingWorkspace` | blank and whitespace workspace |
| `TestLeaseValidatesOwnerAndTTL` | empty owner, zero/negative TTL |
| `TestLeaseReapsAtTheExpiryBoundary` | `lease_expires_at == now` is reaped; `now + 1ns` is held |
| `TestLeaseRetryBoundary` | `not_before == now` is claimable; `now - 1s` is not |
| `TestLeaseSharedScopeSymmetry` | tasks sharing 1 or 2 scopes; a task with no scopes is never excluded; a task already leased is not leased twice |
| `TestCompleteSetsRunTime` | complete after >1 s sets `RunMS`; `Resolve` does not overwrite it (extend the existing test) |
| `TestFailSetsRunTimeAndNotBefore` | failure records `run_ms`, `not_before ≈ now + RetryDelay`; blocked failure clears `not_before` |
| `TestListMultipleStatuses` | `List(New, Blocked)` returns both, ordered priority then id; unknown status returns empty without error |
| `TestDepsOrderAndMissingTask` | dependencies by id order; missing task has no deps, not an error |
| `TestBlockersExactMessages` | leased, done, blocked, retry, dependency, scope contender; multiple reasons in a stable order |
| `TestClaimsEmptyAndMultiple` | no rows → nil/empty; multiple claims returned without relying on order |
| `TestOpenRejectsBadPaths` | empty path, missing parent directory, path is a directory, corrupt file, non-SQLite file; a future `user_version` is refused through `store.Open`, not only `migrate` |
| `TestOpenIsIdempotent` | two `Open`s and two `Close`s on one file |
| `TestDatabaseSidecarModes` | after a write, `tasks.db-wal`/`-shm`, if present, are not world-readable |

The last one is a security check, not a formatting check: `createPrivate`
protects the main file, but SQLite creates WAL and SHM files itself.

Add a small assertion helper only if it removes repetition without hiding
semantics: `mustAdd`, `mustGet`, `mustLease` already exist in the package test
file; keep them local to `store_test.go`. Do not export test helpers from the
production package.

### 4.2 Migrations (P0)

`migrate_test.go` is white-box (`package store`) and can access `migrationsFS`.
Use both the real embedded migrations and synthetic `fstest.MapFS` cases:

| test | case |
|---|---|
| `TestRealMigrationsFromEveryVersion` | for each `n` in 1..10: create a DB, apply the real migrations up to `n` with `applyMigration`, insert a representative task (plus a scope and a dependency, so there is more than the task row to preserve), then `migrate(ctx, db, migrationsFS)`. Assert `user_version == 10`, the row, the scope and the dependency edge still exist, and the columns added after `n` have their defaults (`workspace = ''`, `workspace_id = ''`, `pane_id = ''`, `run_ms = 0`, `role = 'implement'`, `prompt = ''`, `prompt_hash = ''`, `verdict = ''`, `verdict_notes = ''`, `check_command = ''`, `check_passed = 0`, `check_output = ''`, `merged_at = ''`, `merge_commit = ''`, `plan_id = ''`) |
| `TestRealSchemaSnapshot` | fresh DB → `SELECT type, name, sql FROM sqlite_master ORDER BY type, name` compared with `testdata/schema.golden`. Any schema change without a new migration changes this file, forcing a review |
| `TestMigrationRollsBackOnFailure` | synthetic 0001 creates a table; 0002 has a valid statement followed by invalid SQL. Assert `user_version == 1`, the table from 0001 exists and the partial table from 0002 does not |
| `TestMigrateStopsAtTheFirstFailure` | 0002 invalid, 0003 valid: only 0001 is applied |
| `TestConcurrentOpenMigratesOnce` | two goroutines call `store.Open` on the same fresh path; both succeed, the final version is 10, and no "table already exists" error escapes. If this reveals a real race, fix migration serialization rather than loosening the test |
| `TestMigrationNameEdgeCases` | no underscore, no `.sql`, non-numeric and zero version, duplicate version, nested directory ignored, empty set; `loadMigrations` rejects each with its stated reason |
| `TestAppliedMigrationIsNeverReapplied` | existing case stays; add a counter row or a failing second `CREATE TABLE` to prove no silent re-run |

Do not edit shipped migration files. A new migration adds a case to
`TestRealMigrationsFromEveryVersion` and updates the schema golden.

### 4.3 `internal/herdr` (P0 for transport, P1 for completeness)

`labels.go` is fully covered; leave it. The client needs the commands that the
runner actually calls:

| test | case |
|---|---|
| `TestCheckStatusRejectsStoppedServer` | fixture `running:false` → "no server is running" |
| `TestCheckStatusRejectsIncompatibleServer` | fixture `compatible:false` → error names client/server versions and protocol |
| `TestCheckStatusRejectsMalformedStatus` | invalid JSON and non-JSON stderr → a plain error, not a panic |
| `TestCallDecodesErrorFromStdout` | `{"error":{...}}` on stdout with exit 0 → `*herdr.Error` with `CodeOf` and the command name |
| `TestCallRejectsMalformedResult` | result is a string where an object is expected → decode error names the command |
| `TestRemoveWorktreeCommand` | `worktree remove --workspace w3 --force`; server error surfaces |
| `TestCloseWorkspaceCommand` | `workspace close w3`; error surfaces |
| `TestWaitPassesEveryStateAndTimeout` | `agent wait a --until blocked --until done --timeout 5000`; no timeout omits the flag |
| `TestSendKeysPassesEachKey` | `agent send-keys a ctrl+c esc`; each key is a separate argv entry |
| `TestPromptWithoutTimeout` / `TestStartAgentWithoutArgsOrTimeout` | optional flags are omitted |
| `TestCreateWorktreeRejectsMissingIds` | fixture without workspace/pane → error, but the path is still decoded. `Herdr.Open` discards it on error, which leaks the created workspace; pin this as a known leak until `Open` can remove it |
| `TestCreateWorkspaceRejectsMissingIds` | same for triage |
| `TestInSession` | table: unset, `0`, `1`, `true` |
| `TestBinDefaultsToHerdr` | `Client{}` finds `herdr` on a temp `PATH` |
| `TestArgumentsAreNeverShellInterpreted` | labels, agent args and prompt text containing spaces, newlines, quotes, `$(touch pwned)` and `--label` arrive as exactly one argv element; no file is created |
| `TestExecUsesCommandContext` | a fake that sleeps exits when the context is cancelled; keep the timeout tens of milliseconds, not seconds |
| `TestTailTruncates` | short string unchanged; long string starts with `…` and keeps the rune count |

Keep the fake's command dispatch table explicit: an unrecognised command must
exit non-zero with a message, so a new client call cannot silently pass.

### 4.4 `internal/runner/herdr_backend.go` (P0)

This file is the highest-risk 0% in the repository: it resolves a label, creates
the worktree, starts the agent, maps `agent_blocked`, and decides what to close.
It must be tested against the scripted `herdr` fake and a real git repository.

| test | case |
|---|---|
| `TestResolveExactMatchOnly` | one match → id; zero → `no Herdr workspace is labelled %q`; two with the same label → `N ... are labelled`; case/whitespace differences do not match |
| `TestOpenCreatesWorktreeAndStartsAgent` | fake logs `worktree create` with workspace id, branch, base, `loa #N`, `--no-focus`; `Handle` has workspace, pane, path, repo, base commit, branch; agent start uses `loa-N-M`, kind `pi`, the allowlist args and the pane |
| `TestOpenRemovesWorktreeWhenBaseIsUnreadable` | fake returns a path that is not a git repo; assert `worktree remove` is called and no handle is returned |
| `TestOpenRemovesWorktreeWhenAgentFails` | `agent start` fails with a non-retryable code; assert cleanup and the original error |
| `TestStartAgentRetriesAgentNotReady` | fake returns `agent_not_ready` twice, then success; assert three calls and success |
| `TestStartAgentStopsAtDeadline` | always `agent_not_ready` with a 20 ms `ReadyTimeout`; assert a prompt error, not an infinite loop |
| `TestPromptMapsAgentBlocked` | `herdr.CodeOf(err) == "agent_blocked"` → `errors.Is(err, runner.ErrBlocked)` is true; other errors are not mapped |
| `TestCloseFallsBackToWorkspaceClose` | `worktree remove` fails, `workspace close` succeeds → nil; both fail → error mentions both; empty workspace id → no calls |
| `TestTaskWorkspacesKeepsTaskLabels` | `loa #7`, legacy `taskflow #7` kept; `nixos`, `loa #0`, `loa #x` dropped |
| `TestAgentKindAndReadyTimeoutDefaults` | zero values resolve to `pi` and 60 s |

If the shell fake becomes unwieldy, a tiny Go fake binary under
`internal/testdata/fakeherdr/` is still a test binary, not a production
dependency. Keep it offline and deterministic. Do not introduce a Herdr server
mock.

### 4.5 `internal/runner` core (P0/P1)

The existing fake-backend tests cover the happy path, scopes, rounds, budget,
blocked handling, cancellation and reconciliation. Add the failure matrix:

| test | case |
|---|---|
| `TestNewDefaultsAndValidation` | nil store/backend; defaults (`HEAD`, `loa/`, workers 1, max 1, TTL 2h, timeout 45m); `LeaseTTL <= TaskTimeout` refused; owner is `host/pid` |
| `TestRunReturnsContextErrorBeforeLeasing` | cancelled context → `context.Canceled`, no backend open |
| `TestEmptyQueueDoesNothing` | zero tasks, zero stats, reconcile still ran |
| `TestNoWorkspaceFailsBeforeOpen` | task with blank `Workspace` → `Fail` result names `-workspace`, `Open` never called |
| `TestTaskTimeoutClosesWorkspaceAndRequeues` | fake agent blocks on `ctx.Done()`; `TaskTimeout` 50 ms → failed, interrupted, closed, task `new` with `not_before`; no second attempt in the same run |
| `TestFailureIsNotRetriedInTheSameRun` | `MaxTries 2`, agent commits nothing, workers 2, max 10 → one attempt only; task `new` with `not_before`; run returns |
| `TestMixedRoundCountsBothOutcomes` | two tasks, one commits, one does not → `Tasks 2, Completed 1, Failed 1` |
| `TestBackendReadErrorDoesNotFailACommittedTask` | `Read` errors after a commit → task `done`, result may be empty; the branch is still judged |
| `TestCloseErrorIsLoggedButDoesNotHideSuccess` | commits ahead → `Close` returns an error → still `Complete`; assert the log contains "close workspace" |
| `TestInterruptErrorStillCloses` | `Interrupt` fails → `Close` still called |
| `TestBranchDeleteFailureIsLoggedAndTaskFails` | branch is checked out elsewhere so `git branch --delete` fails → task still `Fail` with "without committing", log names the kept branch |
| `TestFailureResultIsTruncated` | 10 KB reason → stored result is about 4000 chars; log about 500 |
| `TestReconcileSurvivesSessionError` | `TaskWorkspaces` errors → run continues and leases work |
| `TestReconcileCloseFailureIsLogged` | `Close` errors during reconcile → other workspaces still processed |
| `TestReconcileHoldsExpiredLeaseOnlyUntilExpiry` | expired lease is closed; unexpired is not; the cache returns the same answer for a repeated id |
| `TestRunStopsOnStoreError` | a closed store or a failed `Attach` returns a runner error, not a task failure |
| `TestWorkersGreaterThanRunnableTasks` | batch smaller than workers; a freed scope is picked up next round |

A narrow `taskStore` interface (`Lease`, `Attach`, `Complete`, `Fail`, `Get`,
`Claims`, `Unblock`, `Resolve`) would make store-error injection easy, but it is
not required for the cases above: the fake backend's `onOpen` and a closed store
cover most of them. Introduce it only when a test cannot be written otherwise
(roadmap D3).

Git and prompt helpers:

| test | case |
|---|---|
| `TestCommitsAheadCountsOnlyAhead` | base at commit 1, branch with one commit → 1; empty branch → 0 |
| `TestGitOutputReportsStderr` | non-repo directory → error contains the git stderr, not just the exit code |
| `TestDeleteBranchCheckedOutFails` | branch checked out in the repo → error; no panic |
| `TestSummaryOfDropsChrome` | table of blank, box-drawing-only, tab-only lines dropped; a line with one text rune kept |
| `TestSummaryOfKeepsLastLinesAndCapsRunes` | 20 lines → last 12; 5000 runes → leading `…`, 2000 runes |
| `TestPromptGolden` | a fixed task with body and scopes → `testdata/prompt.golden`; a task without a body omits the blank block; scopes line appears only when scopes exist |

The prompt is the agent's contract. A golden file is appropriate because a
wording change can change agent behaviour. The golden test must read the file
and show a readable diff, not just `bytes.Equal`.

### 4.6 `internal/live` (P1)

| test | case |
|---|---|
| `TestSessionCellStates` | table: none → `-`; closed → `closed`; live with no agent → `w1 no agent`; live with state only → `w1 blocked`; live with name and state → `w1 blocked loa-1-1` |
| `TestReadRecordedIDWinsOverLabel` | a recorded live id and a second workspace labelled for the same task: the row is the recorded workspace; the extra labelled workspace is reported as an orphan (or explicitly pinned otherwise) |
| `TestReadAgentsErrorKeepsWorkspaces` | `Agents` errors but `Workspaces` works: rows still join by id, `Unavailable` says "agents unavailable", orphan scan is skipped |
| `TestReadAgentsAreMatchedByWorkspace` | an agent with the right name in another workspace is not attached to the row |
| `TestReadDuplicateLabels` | two workspaces share a task label: deterministic result (last wins today) and no orphan for the chosen one; this pins a server-side anomaly |
| `TestReadStoreError` | a closed store → error, not a partial snapshot |
| `TestSortTiesAndUnknownStatus` | same rank/priority → id ascending; unknown status last; stable order |
| `TestReadOrphanClaimedByStore` | a workspace id claimed by a task that is not in `Rows` is not an orphan by the current rule; pin or change deliberately |

`Read` calls `s.List` and `s.Claims` directly on a concrete `*store.Store`, so a
`Claims` failure is not injectable. Do not refactor for that alone; if a bug
appears there, close the DB between calls or add a small store interface in the
same change.

### 4.7 `internal/report` (P1)

The current test builds a snapshot through the store and `time.Now()`, which
makes exact output impossible. Add a literal-snapshot test with fixed times:

```go
snap := live.Snapshot{Rows: []live.Row{{Task: store.Task{ID: 1, ... UpdatedAt: fixed}, ...}}}
r := report.Build(snap, since, until)
```

| test | case |
|---|---|
| `TestTextGolden` | fixed `since`/`until`, one done, one blocked, one queued, one open, one orphan, `RunMS` values; compare to `testdata/report.golden` |
| `TestJSONGolden` | same snapshot → `testdata/report.json.golden`; pins field names and `idle_ns` |
| `TestWindowBoundary` | `UpdatedAt == since` included; 1 ns before excluded; `new` always queued; done/blocked outside window omitted |
| `TestLastActivityUsesAllRows` | a queued task sets `LastActivity` even though it is not in `done`/`blocked`; `Idle == until - last` |
| `TestGapStates` | never worked; `Idle == 0` → "just now"; exactly 1 minute; future `until` (negative idle) → decide and pin |
| `TestTotalRunSumsUniqueTasks` | done + blocked + open; queued excluded. **Investigate the double count**: a done task whose workspace is still live currently appears in both `Done` and `Open`, and `TotalRun` adds `RunMS` twice; add the test, then fix `Build` or `TotalRun` if confirmed |
| `TestTextOmitsEmptySectionsButNamesTheGap` | only the gap line for an empty snapshot |
| `TestOpenWorkspaceWinsOverWindow` | open section ignores the window (existing case extended with a fixed snapshot) |
| `TestBranchAndRunFormatting` | empty branch → `(no branch)`; `RunMS 0` → `ran ?`; 499 ms → `ran 0s` (pin, then decide); 1500 ms → `ran 2s` |
| `TestOneLineTruncation` | whitespace collapsed; 100-char boundary; 101 chars ends with `…` |
| `TestSessionUnavailableRendered` | `session: ...` appears before the gap |

### 4.8 `internal/taskspec` (P1)

The parser is the boundary for model-authored input, so fuzz it with the
standard library:

| test | case |
|---|---|
| `FuzzParse` | arbitrary bytes: `Parse` returns an error or a document; never panics. Seed with the valid object, bare array, empty, malformed, duplicate keys |
| `FuzzValidate` | `Parse` success → `Validate` returns an error or `Apply`; never panics or partially resolves `After` |
| `TestParseTopLevelShapes` | object, array, empty object (valid, zero tasks), `null`, scalar, trailing second document, BOM, CRLF |
| `TestParseRejectsUnknownNestedAndTopLevelFields` | unknown task field and unknown top-level field, including on a bare array |
| `TestParseReaderError` | `testing/iotest.ErrReader` → `taskspec: read:` error |
| `TestValidateSelfAndThreeCycles` | self-reference and 3-node cycle produce a cycle error naming the loop; multiple cycles deterministic |
| `TestValidateDuplicateAfter` | `after:["a","a"]` produces one position twice? Pin; a duplicate dependency should probably be rejected or deduplicated |
| `TestValidatePrecedence` | title before workspace before max attempts before scopes before deps before duplicate titles |
| `TestValidateMaxAttemptsZero` | 0 is accepted by `taskspec` and means the store default 2; negative is rejected |
| `TestValidatePriorityValues` | negative and zero priorities are accepted; only the id is checked |
| `TestValidateNotesAndReprioritise` | empty note, duplicate id, id 0/negative; all rejected with index |
| `TestParseLargeDocument` | a few thousand tasks parses within a sane time and every `after` resolves |

Do not “fix” duplicate `after` in the parser until a test states the intended
behaviour. The test is the place to make that decision.

### 4.9 `internal/triage` (P2)

| test | case |
|---|---|
| `TestContextFileMode` | `queue.json` is owner-only (0600 under the default umask, never group/other writable); the directory is the caller's `t.TempDir()` |
| `TestContextWriteFailure` | missing/non-writable directory → error naming the operation |
| `TestContextJSONGolden` | fixed `Now`, one task with body/scopes/deps and one without → `testdata/queue.json.golden`; `omitempty` fields absent |
| `TestBuildIncludesStateAndDeps` | bodies, status, attempts/max, result, scopes, deps statuses; nil deps map works |
| `TestReadValidAndInvalidProposal` | valid → document; unknown field, missing workspace, cycle, empty file → error; a directory at `proposal.json` → error |
| `TestSaveCreatesPrivateDirAndFile` | nested proposals dir is owner-only (0700 under the default umask), file is 0600, name is UTC `YYYYMMDDTHHMMSSZ.json` |
| `TestSaveSameSecondCollision` | two saves with the same `now` — today the second overwrites the first; pin it, then decide whether a suffix is needed (a proposal is user-visible and losing one is bad) |
| `TestSaveMkdirFailure` | a file where the directory should be → error |
| `TestPromptGolden` | fixed queue path → `testdata/triage-prompt.golden`; the prompt names `proposal.json`, forbids `bash`, and forbids rewriting existing tasks |

### 4.10 `cmd/loa` (P0/P1)

The existing subprocess harness is the right shape. Improve it before adding
cases:

- set `cmd.Dir` to `t.TempDir()` so a missing `-db` cannot write `loa.db` into
  the repository;
- keep the environment scrub (`HERDR_`, `LOA_DB`, `PATH`), but add a `runEnv`
  variant that prepends a temp fake-Herdr directory to a `PATH` containing
  `git`, for `run`/`triage` session tests;
- set `XDG_STATE_HOME` to a temp directory for every test, so
  `defaultProposalDir` and triage never touch `$HOME`;
- build once in `TestMain` as today; do not add a second binary.

Command matrix (each row is a test or a table case):

| command | cases |
|---|---|
| `add` | missing title and workspace → exit 1, no row; `-dep abc` → exit 1; `-dep 999` → FK failure, no row; `-max 0` → default 2; `-scope` repeated; flags after positional; unknown flag → exit 2 |
| `list` | no rows prints only the header; multiple `-status`; unknown status prints nothing; extra positional → exit 1 |
| `show` | non-numeric id, missing id, extra args; `workspace: (unset)`; leased line and `not before`; `ran:` only when >0 |
| `lease` | `-ttl 0`/negative → exit 1; empty `-owner` → exit 1; null JSON for an empty queue (exists); owner default shape |
| `done`/`fail` | missing lease (exists); branch optional; result default empty; unknown id → `not leased`; `fail` sets `not before` (exists) |
| `unblock`/`resolve` | missing id, non-numeric, unknown id, wrong state; `resolve` on `new` succeeds and moves it off the queue; `resolve -fail` on done moves it to blocked (pin the semantics) |
| `ps` | with no Herdr, `session_unavailable` is set and rows still print; `-json` orphans; table headers; extra args |
| `report` | `-since 0h`, negative, `24h`, `2026-01-01`, `tuesday`; `-json` shape; unavailable session line; empty queue gap |
| `import` | stdin `-`; empty stdin; missing file; `-dry-run` with notes; unknown field; cycle; missing `after`; reprioritise nonexistent → nothing created; two positional files → exit 1 |
| `run` | `-db` → exit 2 and no file (`TestRunRefusesADatabaseFlagAndCreatesNoFile`); `-owner` → exit 2 (`TestRunRefusesTheOwnerFlag`); daemon refuses an invalid run budget (`TestDaemonRefusesAnInvalidRunBudget`); a request cannot widen the daemon's workers/max, lengthen timeout/ttl, or replace its check (`TestRunRequestCannotWidenTheDaemonsBudget`, `TestRunRequestCannotLengthenTimeoutOrTTL`, `TestRunRequestCannotReplaceTheDaemonsCheck`); `-json` through the socket stays one document (`TestRunJSONThroughTheDaemonKeepsItsShape`); the first Ctrl-C asks for stop and the second kills a wedged client (`TestRunSecondInterruptKillsTheClient`) |
| `triage` | outside a Herdr pane → exit 1; with a scripted fake Herdr, a proposal file is written, saved under the temp state dir, and the queue is unchanged; blocked-prompt path names the workspace/pane |
| root | `-h`, `--help`, `help`, unknown command → exit 2 for unknown, 0 for help, no args → 2 |

Unit-test the pure helpers in the `main` package directly, because the
subprocess cannot cover them: `parseSince` (table), `taskID` (table),
`defaultDB` (env set/unset), `defaultStateDir`/`defaultProposalDir`
(`XDG_STATE_HOME`, `HOME` fallback), `workspaceOf`, `registerRunFlags`
defaults, and the `allowlist` contents (a change to the tool allowlist is a
security change).

For `newRunnerBackend` and `newRunnerConfig`, use the fake Herdr script and test:

- `newRunnerBackend` does not require a Herdr pane; the session check belongs to
  commands where a person is watching;
- the agent args contain `--no-session --tools read,edit,write,bash,grep,find,ls`
  and only add `--model` when set;
- `newRunnerConfig` copies the flags into `runner.Config` unchanged.

### 4.11 Coverage collection for the CLI

The existing `TestMain` builds with `go build`. To collect child-process
coverage, add a **separate** test mode (do not complicate normal runs):

```sh
go build -cover -coverpkg=./... -o "$bin" .
GOCOVERDIR="$dir" go test ./cmd/loa
go tool covdata textfmt -i="$dir" -o=cmd.cov
```

Merge with the package profiles using `go tool covdata merge`. This is the only
way to see which `cmd/loa` branches the 73 process tests actually reach. It uses
only the Go toolchain. Keep the normal `go test` fast; run the coverage variant
in CI or a nightly job, not on every save.

## 5. Conventions

### Table-driven tests

Use tables when the same assertion applies to many values, not when each case
sets up a different world. Existing examples to follow:
`TestMigrationNamesAreChecked`, `TestReconcileClosesOnlyWhatItOwns`,
`TestParseRejectsUnknownFieldsAndJunk`, `TestValidateRequiresWorkspaceAndSaneNumbers`.

- Name each case for the behaviour, e.g. `"recorded and held"`, not `"case 1"`.
- Give each case a short setup function when it needs one, as
  `TestBlockersAgreeWithLease` does.
- Do not call `t.Parallel()` in subtests that use `t.Setenv`. Go forbids it.
- Do not share a `*store.Store` or a git repository between parallel cases;
  `t.TempDir()` per case is cheap and removes ordering bugs.
- Keep failure messages specific: expected and actual IDs, statuses and
  reasons, not just "unexpected".

### Golden files

Use golden files for stable, human-facing contracts:

- `internal/herdr/testdata/*.json` — recorded server output (already);
- `internal/store/testdata/schema.golden` — `sqlite_master`;
- `internal/runner/testdata/prompt.golden` — worker contract;
- `internal/triage/testdata/queue.json.golden`, `triage-prompt.golden`;
- `internal/report/testdata/report.golden`, `report.json.golden`.

Convention: a single `golden(t, name, got)` helper in each package (or a small
test-only helper package if duplication becomes real) that reads the file and
supports update with `UPDATE_GOLDEN=1`. Do not add a dependency. Normalize
unstable data before comparing: absolute temp paths, hostnames, PIDs, real
timestamps. Never normalize the field names or section order being tested.

For `herdr` fixtures, do not use the update mechanism: they are captures, not
output. A change requires recapturing against a live session and updating the
protocol note in `testdata/README.md`.

### Failure and assertion style

- Use `errors.Is` for sentinel errors (`store.ErrNotFound`,
  `store.ErrNotLeased`, `runner.ErrBlocked`, `context.Canceled`). Do not match on
  stringified errors except to assert a user-facing message contains a key
  phrase.
- Assert stored rows after operations, not only the returned value. A test that
  only checks the `Task` returned by `Lease` misses a missing/dirty row.
- For CLI tests, assert stdout, stderr and exit code separately. The current
  `mustRun` hides stderr on success; use `run` when stderr matters.
- Do not use `time.Sleep` to wait for a goroutine. Use a channel or a context
  cancellation. The one legitimate sleep is measuring `run_ms`; keep it just
  over one second and assert a range, as `TestRunTimeIsMeasuredAtFinish` does.

## 6. Concurrency, time and flakiness controls

| source | control |
|---|---|
| lease races | two `Store`s on one file, 8 goroutines, loop until `Lease` returns nil; assert every task leased exactly once. Repeat under `-race` and `-count=20` in a soak job |
| SQLite busy/locked | `_txlock=immediate`, `busy_timeout(5000)`; add a test where one handle holds a write for ~50 ms and the other leases; it should wait, not fail. Keep under the timeout |
| migration races | concurrent `store.Open` on a fresh file (see §4.2); assert one schema, no error |
| wall clock in store | `Lease` already accepts `now`; add the `now` seam for `Complete`/`Fail`/`Resolve`/`Unblock` when a test needs exact `not_before`/`run_ms`. Until then, measure from `time.Now()` as `TestFailRequeuesUntilAttemptsAreSpent` does, and comment why |
| one-second timestamp resolution | `run_ms` tests sleep >1 s and assert a lower bound, not an exact value; never assert exact milliseconds |
| runner retries | prefer a configurable retry delay over waiting 500 ms; otherwise assert calls, not elapsed time |
| git worktree operations | `t.TempDir()`, one repo per case; configure `user.name`, `user.email`, `commit.gpgsign=false`; serialise worktree creation in a fake with a mutex (already done) |
| environment | CLI tests scrub `HERDR_*`, `LOA_DB`, `PATH`; session tests opt in explicitly via `runEnv`; `t.Setenv` never in parallel |
| temp paths in output | normalize before golden comparison; never bake `t.TempDir()` into a golden |
| shuffle | add `go test -shuffle=on ./...` to CI; tests must not depend on file/order or shared state |
| ordering maps | `live.Read` builds maps for labels/agents; add duplicate-label tests rather than relying on Go map iteration |

Soak commands (nightly or pre-release, not every save):

```sh
go test -race -shuffle=on -count=20 ./internal/store ./internal/runner
go test -race -shuffle=on -count=5 ./internal/herdr ./internal/live
```

## 7. CLI integration harness

The existing harness in `cmd/loa/main_test.go` is the model:

1. `TestMain` compiles the binary once into a temp directory; tests invoke it
   with `exec.Command`.
2. Each test gets its own `-db` under `t.TempDir()`.
3. The environment is scrubbed of `HERDR_*`, `LOA_DB` and the caller's `PATH`, so
   a real `herdr` on the machine cannot leak into `ps`/`report`.
4. Exit codes are asserted: 0 success, 1 operational error, 2 usage error.

Extend it carefully:

- **Session tests are opt-in.** Add a `newSessionCLI(t)` helper that creates a
  fake `herdr` script in a temp dir, prepends it to a `PATH` that also contains
  `git` and the shell utilities, sets `HERDR_ENV=1`, and points
  `XDG_STATE_HOME` at a temp dir. Tests that do not need a session keep the
  current minimal environment.
- **The fake script is shared by copy, not by import.** `cmd/loa` cannot import
  test functions from `internal/herdr`. Keep a small `fakeHerdr` writer in
  `cmd/loa/main_test.go`, or move both to a `internal/testutil` package (code,
  not production; no dependency) once a third package needs it. Do not duplicate
  the fixture JSON: point `TESTDATA` at `internal/herdr/testdata` or copy the
  files into a temp dir.
- **No network and no real Herdr.** The fake handles `status`, `workspace list`,
  `worktree create`, `agent start`, `agent prompt`, `agent read`,
  `worktree remove`, `workspace close`, `agent focus` and `send-keys`.
- **Test the child process's coverage separately** (§4.11), not in normal runs.
- **Run each CLI command from a temp working directory** so default `./loa.db`
  and relative proposal paths are tested without polluting the repo.

## 8. Migration tests (expanded)

Covered in §4.2. The rules:

- Migrations are append-only. A test failure in the schema golden means a new
  migration is missing or a shipped one was edited.
- Every new migration adds a case to `TestRealMigrationsFromEveryVersion` and,
  if it changes schema, an updated `schema.golden` in the same commit.
- Test both directions of the version check: older DB migrates, newer DB is
  refused.
- Test failure atomicity, not just success: one transaction per file, so a
  failed step leaves the previous `user_version` intact.
- Test data preservation, not only table existence. A column added with the
  wrong default is a data bug that `hasTable` will not catch.
- Do not add a migration runner dependency. The embedded `fs.FS` and
  `PRAGMA user_version` are enough.

## 9. Security and failure-mode checks

These are targeted tests, not a penetration-test programme:

| check | test |
|---|---|
| database privacy | existing 0600 tests; add WAL/SHM mode check; triage `queue.json`/proposal 0600 and proposals dir 0700 |
| no shell interpretation | fake Herdr argv-boundary test with `$(...)`, newlines, quotes; `git` is invoked with an argument slice |
| parameter binding | `Add`/`List` with titles, scopes and statuses containing SQL metacharacters; values stay data, no table damage |
| symlink/path handling | `createPrivate` must not follow an existing symlink or truncate its target; importing a symlink path is the user's choice and is fine, but the DB creation is not |
| corrupt database | `Open` on a garbage file returns an error, no panic; `migrate` on a DB with an unrelated schema fails cleanly |
| read-only/full disk | `Open` on a read-only file returns an error; `Complete`/`Fail` error is surfaced to the runner and ends the run, not silently ignored |
| oversized input | a very large task body/results survive storage; results are truncated only at the agent-output boundary, not in the queue |
| JSON output validity | `ps -json`, `report -json`, `lease` output parse with `encoding/json` and carry the documented fields; validate against goldens |
| Unicode and width | `SummaryOf`, `pad`, `truncate`, `oneLine` handle multibyte runes without splitting them |
| agent output leakage | `SummaryOf` drops box drawing and status chrome but retains the agent's words; golden test on a recorded screen |

Prompt injection, network access and reads outside the worktree are design
limits documented in `README.md`; do not write tests that pretend to prevent
them.

## 10. Manual and exploratory checks for the live Herdr transport

The sandbox cannot run a Herdr session. These checks are required after a Herdr
upgrade or a change to `live.go`/`runner/herdr_backend.go`.

### Live Herdr transport (after every Herdr upgrade)

1. `herdr status --json` reports protocol 22 and `compatible: true`.
2. `loa run -max 1` on a scratch repo with a workspace label: a branch
   `loa/<id>.1` exists, the worktree workspace is gone, `loa ps` says `done`,
   and `loa report -since 1h` shows the task and its runtime.
3. A task that commits nothing fails with "finished without committing" and
   leaves no branch.
4. An unknown workspace label fails at dispatch with
   `no Herdr workspace is labelled "..."`.
5. A blocked agent (approval dialog) leaves the workspace and pane open and the
   result names both; answering the dialog and running `loa resolve ID` settles
   the task.
6. `loa triage` writes a proposal under `$XDG_STATE_HOME/loa/proposals/`, closes
   its workspace, and leaves the queue unchanged; `loa import -dry-run` creates
   nothing; `loa import` applies the document in one transaction.
7. Upgrade Herdr deliberately and repeat 1–6. Record the new protocol and
   fixtures in `internal/herdr/testdata/`.

## 11. CI pipeline expectations

The gate today is `nix flake check`, whose `checkPhase` runs `go test ./...` and
`golangci-lint run ./...` in the sandbox. Keep that as the one-command gate and
add checks in this order:

1. **Build:** `go build ./cmd/loa` (the Nix derivation already builds it).
2. **Tests:** `go test -count=1 -timeout 10m ./...`.
3. **Race and shuffle:** `go test -race -shuffle=on -count=1 -timeout 15m ./...`.
   Verify the flake sandbox has a C compiler; `buildGoModule`/stdenv normally
   does, but if it does not, make this a separate `checks` derivation with
   `CGO_ENABLED=1` and the stdenv C toolchain rather than dropping the check.
   Do not set `CGO_ENABLED=0` when using `-race`.
4. **Lint:** `golangci-lint run ./...` must print `0 issues.`; add
   `golangci-lint fmt --diff` if the installed version's `run` does not cover
   formatters. `GOLANGCI_LINT_CACHE` must stay in `$TMPDIR` inside the sandbox.
5. **Coverage:** collect per-package profiles with `-coverpkg=./...`, and merge
   child-process coverage from `cmd/loa` via `GOCOVERDIR`/`go tool covdata`
   (§4.11). Publish the profile; do not fail the build on a global percentage.
   Add a threshold only for a package after its P0 tests land, and only to catch
   a regression, not to chase a number.
6. **Fuzz smoke:** `go test -run=XXX -fuzz=FuzzParse -fuzztime=10s
   ./internal/taskspec` (and `FuzzValidate`) in a nightly or pre-release job.
   Fuzz failures are committed as seed corpus entries; do not add a fuzzing
   dependency.
7. **Soak:** the `-race -count` commands in §6, nightly or pre-release.
8. **Sandbox checks:** the flake must still pass with no network, no Herdr and
   no `$HOME` write access; `GOLANGCI_LINT_CACHE` and all test temp dirs must
   live under `$TMPDIR`.

Local developer commands stay:

```sh
nix develop
go test ./...
go test -race ./...
golangci-lint run
golangci-lint fmt --diff
nix flake check
```

Do not add a CI service, a test container or a network-dependent integration
job. The design deliberately keeps the gate offline.

## 12. Prioritised implementation roadmap

Each step is independently mergeable, adds tests only, and leaves the suite
green. Effort is a rough guide, not a commitment.

### Phase A — P0: protect the queue and the transport

**A1. Migration integrity (1–2 days)**
Add the real-migration chain, schema golden, rollback, first-failure and
concurrent-`Open` tests from §4.2. These are the tests that would catch a bad
`0005_*.sql` before it reaches a real database.
Exit: a fresh DB, a v1–v4 DB with data, and a failed migration all behave as
described; two concurrent opens do not corrupt or fail.

**A2. Store lifecycle and validation (1–2 days)**
Add `Unblock`, `Resolve`, `Add` rollback/defaults, `Lease` boundaries/validation,
exact `Blockers`, and `Complete`/`Fail` run-time tests from §4.1.
Exit: every public `Store` method has at least one error-path test and one
state-transition test; no public method is at 0% coverage.

**A3. Herdr client completeness (1 day)**
Add the missing command tests and failure fixtures from §4.3, and switch the
fake to argv-boundary logging. Add `testdata/README.md`.
Exit: every `Client` method is exercised on success and at least one failure
shape; `CodeOf`, `InSession`, `tail` and argument boundaries are covered.

**A4. Herdr backend (2–3 days)**
Test `resolve`, `Open`, `startAgent` retry/deadline, blocked mapping, `Close`
fallback and `TaskWorkspaces` per §4.4, using the scripted fake CLI and a real
temp repo.
Exit: `internal/runner/herdr_backend.go` is no longer at 0%; a missing label, a
bad base, a failed agent start and a failed remove each have a test.

**A5. CLI session gate and run client (1 day)**
Add the `triage` refusal test and the `run` client tests from §4.10 (daemon
connection, flag refusals, clamp refusals, JSON, Ctrl-C), plus the
`newRunnerBackend`/`newRunnerConfig` unit tests.
Exit: `loa triage` cannot start without a compatible session; `loa run` fails
cleanly when the daemon is unreachable and reports the daemon's outcome
otherwise.

### Phase B — P1: stop the tool from lying

**B1. Runner failure matrix (2 days)**
Add per-task timeout, failure-not-retried, mixed round, read/close/interrupt
errors, branch-delete failure and result truncation from §4.5.
Exit: each branch in `dispatch` that records a result has a test; a committed
task is never failed by a read or close error; a failed task is not retried in
the same run.

**B2. Git and prompt contracts (1 day)**
Add `gitOutput` stderr, `commitsAhead` edge, `deleteBranch` failure, `SummaryOf`
golden/truncation and `prompt.golden` from §4.5.
Exit: the worker contract and the "success means a commit" rule have stable
golden evidence.

**B3. `ps`/`report` golden output and boundaries (1–2 days)**
Add the fixed-snapshot `report.golden`, `report.json.golden` and the boundary /
double-count investigation from §4.7, plus the `live` cases from §4.6.
Exit: the report for a fixed snapshot is byte-stable, and the double-count bug
is either fixed or explicitly pinned with a decision.

**B4. Taskspec fuzz and parser edges (1 day)**
Add `FuzzParse`, `FuzzValidate` and the shape/precedence/cycle tables from §4.8.
Exit: 10-second fuzz runs are clean; every rejection has a named reason.

**B5. CLI breadth (1–2 days)**
Fill the command matrix in §4.10, including stdin import, env precedence,
`parseSince`, `taskID`, `Xdg` state dirs and exit codes.
Exit: every command has success, usage and operational-failure tests; the
harness runs from a temp cwd with a scrubbed environment.

### Phase C — P2: operability

**C1. Triage file handling (1 day)**
Add modes, write failures, same-second collision, JSON/prompt goldens and the
`Read` valid/invalid table from §4.9.
Exit: triage cannot silently lose a proposal without a test pinning the
behaviour, and every file it writes has an asserted mode.

**C2. Manual scripts (half day)**
Turn §10 into a short checklist in `QUICKSTART.md` or keep it here; record the
Herdr version/protocol and the commands used. Do not automate a live session.
Exit: a Herdr upgrade has a repeatable manual checklist and captured fixtures.

### Phase D — P3: durability and only the seams that are needed

**D1. CI coverage and child-process coverage (1 day)**
Add the `-race`/`-shuffle`/`-count=1` check, the `GOCOVERDIR` merge for
`cmd/loa`, and the fuzz/soak jobs from §11.
Exit: CI reports command coverage and fails on a race; no network or live Herdr
is required.

**D2. Concurrency soak and migration race (ongoing)**
Run the §6 soak commands; fix any failure with a deterministic reproduction
before increasing counts.
Exit: 20 repeats of store/runner under `-race` are clean on two architectures.

**D3. Testability seams, only when a test demands them (as needed)**
In order of likely need: `Store.now` for exact `not_before`/`run_ms`;
`runner.Herdr.retryDelay`; a narrow store interface for `runner`/`live` error
injection; a `herdr`-client interface for `runner.Herdr`. Each is a small,
behaviour-preserving change justified by a failing test, not a refactor for its
own sake. Do not introduce a DI framework, mocks library or a second backend.

### What not to do

- Do not add a fake Herdr server or a network test. The JSON fixtures and the
  scripted CLI are the contract; the live session stays manual.
- Do not add `testify`, `gomock`, `teatest`, a golden-file library or a
  SQLite test helper.
- Do not chase 100% coverage. Some error paths (failed `Rows.Scan` on a healthy
  database, `json.Marshal` of a struct that cannot fail) are not worth forcing.
- Do not put `t.Parallel()` on store/CLI tests that share environment or a git
  repository.
- Do not weaken a test to accommodate flakiness. Fix the clock seam, the retry
  delay or the cleanup; a sleep in a test is a bug report about the design.
- Do not edit a shipped migration or a recorded fixture to make a test pass.