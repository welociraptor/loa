# loa

Task queue for a multi-agent daily workflow. The SQLite database is the only
shared state: the runner leases one task at a time, hands it to an agent in a
Herdr worktree workspace, and records the outcome. Workers do not write to the
store; a coordinator may (see [Coordinator](#coordinator)), and that boundary is
policy rather than a sandbox.

Named for the loa of Gibson's *Count Zero*: a spirit that takes up residence in
a host and works through it.

## The lease is the whole design

A task is claimed by an atomic transition to `leased` with an owner and an
expiry. Everything else follows from that:

- A dead worker's task returns to the queue when its lease lapses, so the
  runner needs no cleanup path of its own.
- A task is only claimable when its dependencies are `done` and no other leased
  task shares one of its scopes. Scopes are coarse mutual-exclusion domains
  (a repo, a path, a service), which is what keeps parallel workers from
  editing the same thing.
- Attempts are counted at lease time rather than on failure, so a worker that
  hangs still burns an attempt. A task that runs out of attempts becomes
  `blocked` and waits for a human instead of looping.

Timestamps are RFC3339 UTC strings, so the database stays readable with
`sqlite3` and needs no driver-side time handling. The database file is created
mode 0600; an existing file's mode is left alone. Schema changes are numbered
files in `internal/store/migrations/`, applied transactionally in version
order, and a database newer than the binary is refused rather than opened.

## Usage

```
nix develop
go build ./cmd/loa

loa add    -workspace nixos -title "..." -body "..." [-role implement|plan|review] [-priority 5] [-scope repo:loa]... [-dep ID]...
loa list   [-status new]...
loa show   ID                      # fields, and why it is not running
loa ps     [-json]                 # the queue joined with the live session
loa dash                           # the tab: state, and the keys to act on it
loa report [-since 24h|-json]      # what the day did, and how long it has been still
loa runs                           # the run record, newest first
loa triage                         # ask an agent to propose work; never applies it
loa import FILE [-dry-run]         # apply a proposal: tasks, and priority changes
loa merge ID [-into BRANCH]        # merge an approved, green review; never pushes
loa lease  -owner runner-1 -ttl 1h # prints the task as JSON, or null
loa done   ID -branch b/thing -result "shipped"
loa fail   ID -result "test suite red"
loa run    -workers 2 -max 5 [-check CMD] [-prompts DIR] [-json] # works the queue, inside Herdr
```

`LOA_DB` selects the database (default `./loa.db`); every command
also takes `-db`.

A task's `-role` selects its prompt and tool set. The default, `implement`,
uses `prompts/implementer.md` and the full tool set; `plan` uses
`prompts/planner.md`, and `review` uses `prompts/reviewer.md`, both read-only
plus `write`. The prompt is Markdown rendered with the task fields and then the
runner appends its contract, which a prompt file cannot weaken. The dispatch
records the prompt file and its content hash on the task, so a result is
attributable to the instructions that produced it. `loa list`, `show` and `ps`
show the role, and `show` shows the recorded prompt once a task has run.
`loa run -prompts DIR` overrides the directory; otherwise it is found by
walking up from the working directory to the repository root.

Dependencies: `modernc.org/sqlite` (pure Go, no cgo) and
`golang.org/x/term` for raw mode and the tab's window size. Everything else the
tab draws is plain strings and escapes, so the loop stays testable and the
dependency list stays at two.

## The runner

Everything runs inside Herdr: the runner itself is started in a pane, creates a
worktree workspace per task with `herdr worktree create`, starts the agent in
that workspace's pane, prompts it and reads its terminal. Nothing is exec'd
except the `herdr` client and `git`, and no worktree is created by this code.
It has to be safe to interrupt and safe to run twice:

- **Success is read from the repository, not from the agent.** An implementation
  task is done only if its branch gained a commit; an agent that reports success
  and commits nothing fails, and its terminal text is recorded as the failure
  reason. A review task is judged by its verdict instead (see [Reviews](#reviews)).
- **The workspace is where the work happens.** A task names a Herdr workspace
  *label*, resolved to an id at dispatch: exact match, or the task fails. No
  filesystem path comes from task text at all — Herdr owns the repository.
- **Scopes are the concurrency control.** Every scope, `repo:` included, is an
  exclusion domain: two leased tasks never share one, so two workers never
  fight over the same repository.
- **Each attempt gets its own worktree workspace and branch**
  (`loa/<id>.<attempt>`), branched from `-base` or, so that a chain stacks
  commits, from the newest done dependency's recorded branch; a re-queued or
  branchless dependency leaves `-base` in force. It is labelled
  `loa #<id>` so it can be found again. `herdr worktree remove` deletes the
  checkout and the workspace; the branch is the deliverable. Attempts that
  commit nothing also delete their branch, so a bad day does not litter the
  repository.
- **Rounds, not a worker pool.** Each round leases up to `-workers` runnable
  tasks and runs them together. A task freed by a finishing sibling is picked
  up by the next round instead of being missed by a worker that already gave
  up.
- **`-max` is the budget** for one run. Combined with the store's retry delay,
  a failure cannot be retried in the same run.
- **`-ttl` must exceed `-timeout`**, or a lease would lapse while the task is
  still running and a second worker would pick it up.
- **A blocked agent is left alone.** If Herdr reports `agent_blocked`, or the
  prompt is rejected because the agent is waiting for input, the task fails with
  the workspace and pane in its result and the pane stays open. The runner never
  answers an approval dialog; the human does, in that pane.
- **Every run reconciles first.** A workspace whose recorded id belongs to a
  task nobody is holding is closed before new work starts; so is one labelled
  `loa #N` that no row recorded, provided that task is not being worked on
  — the window a crashed run leaves behind. A workspace carrying an id recorded
  for a *different* task is left alone and logged: it was not this runner's to
  create, so removing it is not this runner's decision.
- **The ids are recorded, not derived.** `workspace_id` and `pane_id` are written
  at dispatch and kept after the task finishes, so `ps` can say where an attempt
  ran and reconciliation can prove it owns a workspace. The `loa #N` label
  is the fallback, not the authority.

Defaults: `HEAD` as the base, and Herdr's own configuration decides where the
checkout lives — the runner passes no `--path`, and reads the created path back
from the result.

`loa run` refuses to start outside a Herdr pane, and checks
`herdr status` first: a protocol or version mismatch fails the run before it
does anything.

## The tab

`loa dash` is a full-screen view of the queue joined with the session, and
the place a person starts a run from:

```
loa  1 running  0 blocked  2 queued

  ID   STATUS   WORKSPACE    SESSION            TITLE
> 1    leased   nixos        w3 working loa-1-1 wire the report command
  2    new      nixos        -                  needs a decision on the timer
  3    new      nixos        -                  bump the flake

enter focus  x interrupt  d/f resolve  u unblock  c close  s/S run  q quit
```

`enter` jumps to the task's pane (`agent focus`, or `workspace focus` when the
agent has already exited), `x` interrupts, `u` returns a blocked task to the
queue, `c` removes its workspace, `d`/`f` resolve it with a one-line result,
`s`/`S` start and stop a run, `q` quits. Outside the tab, `loa unblock ID` and
`loa resolve ID [-fail]` settle a blocked task the same way. It shows state and
never announces it: Herdr already marks an agent that needs input.

One tab, set up once:

```
herdr workspace create --cwd "$XDG_STATE_HOME/loa" --label loa
# then, in that workspace's root pane:
LOA_DB="$XDG_STATE_HOME/loa/tasks.db" loa dash
```

Task worktrees are separate linked workspaces under the repository's own
workspace, so the tab is an index rather than a container.

A run started from the tab writes its log to `$XDG_STATE_HOME/loa/dash.log`
rather than to the terminal the tab is drawing on; the tab itself only shows the
outcome.

## Proposals

An agent may propose work; a person, or a trusted coordinator agent, applies
it. `loa triage` writes the
queue into a scratch directory (bodies included, dependencies resolved), runs a
`read,grep,find,ls,write` agent there with no `bash`, and keeps whatever
`proposal.json` comes back under `$XDG_STATE_HOME/loa/proposals/`. It never
touches the queue.

```
loa triage          # -> proposals/20261118T185722Z.json, plus its notes
loa import -dry-run proposals/20261118T185722Z.json
loa import proposals/20261118T185722Z.json
```

The document is the same shape in both directions:

```json
{"tasks": [{"title": "...", "body": "...", "workspace": "nixos", "priority": 4,
            "after": ["another title in this file"]}],
 "reprioritise": [{"id": 4, "priority": 9}],
 "notes": ["#2 has no body, so it cannot be actioned"]}
```

`after` names a title in the same file, so a proposal can order tasks that do not
have ids yet. Validation is whole-document: unknown fields are rejected rather
than ignored, a title referenced but absent is an error, and a dependency cycle
is refused because it would sit in the queue forever looking like a runner bug.
`import` then applies tasks and priority changes in **one transaction**, so a
document with one bad task creates nothing.

## Reviews

A task whose role is `review` has a verdict as its deliverable, not a commit.
The runner hands it the diffs of the tasks it depends on — that dependency set
is the review subject — and reads `.loa/verdict.json` from its worktree:

```json
{"verdict": "approve|changes|blocked",
 "notes": [{"file": "...", "issue": "...", "suggestion": "..."}],
 "tasks": [{"title": "...", "workspace": "...", "body": "..."}]}
```

Parsing is strict: unknown fields, a missing verdict, or a verdict outside the
three accepted answers fails the task with the parser's reason. `tasks` is the
same plan format as a proposal and is optional.

- `approve` finishes the review with the verdict recorded; no commit is needed.
  If a check is configured, it is run in the review worktree first: only its
  exit status decides, and an `approve` with a red check is recorded as
  `changes`. A verdict is not a build.
- `changes` also finishes it, records the verdict, and writes its `tasks` under
  `$XDG_STATE_HOME/loa/proposals/`. Nothing applies that file — a person reads
  it and imports it, so an agent cannot turn its own objection into work.
- `blocked` is the existing human path: the task fails with its pane left open,
  and a person answers the reviewer there.

A commit alone cannot finish a review, and a review with no dependency diff
fails rather than approving nothing. `show` renders the verdict, the check and
its output; `report` includes the verdict with the day's work.

## Merge

`loa merge` is the only command that writes into a workspace checkout, and it
performs one deterministic transition rather than a review of its own. It
merges only when the review's verdict is `approve`, its check is recorded and
passed, and the current checkout is clean and on the branch named by `-into`
(default `main`). It runs one `git merge --no-ff`, records `merged_at` and
`merge_commit` on the implementation task, and never pushes or deletes a
branch. `ID` may be the review task or its implementation task.

Everything else is a refusal that names the reason: a dirty checkout, a wrong
branch, a conflict, a red or missing check, a verdict other than `approve`, an
unfinished task, or a task already merged. A conflict is aborted and the
branch is left for a person; no agent resolves it. A second merge for the same
task is refused, not a second merge commit. A workflow's `merge: auto` is the
coordinator calling `loa merge` per approved review; the guard stays in loa.

The runner runs `-check CMD` in the review worktree after an `approve`, bounded
by the same timeout as the task. Only the exit status is read as a pass; the
output is kept on the row for the person reading a refusal. Without `-check`,
an `approve` is still a verdict, but nothing merges: a review with no check
recorded is refused by `loa merge`.

## Coordinator

A run may be driven by a coordinator agent instead of a person. The coordinator
writes the queue with `loa add` and `loa import`, starts work with `loa run
-json`, reads the run record with `loa runs`, and escalates a blocked task to a
person rather than answering its pane. A worker still sees one task body; an
implementation is done when its branch gains a commit, and a review is done when
it writes a verdict.

A run that died without closing does not block the next one: `loa runs -close
ID` settles it without inventing what it settled.

The boundary is policy, not a sandbox. A coordinator may write the queue because
it is trusted to; a worker with `bash` can run `loa` too, and nothing in the
store tells them apart. Review what a coordinator queued as you would review a
worker's branch.

## The day

`loa report` is the page to read after a day away, and it needs no model:

```
since 2026-11-17T18:43:33Z, 24h0m0s ago

done: 1
  #1  add note.txt   loa/1.1   ran 16s   note.txt [loa/1.1 f640919] feat: add note.txt

queued: 1
  #2  decide the timer question

time in agents: 16s
activity: just now
```

Tasks are counted when they changed inside the window; workspaces are reported
whatever the window, because one left open yesterday is still open today. The
last line is the point: with no timer, an unsupervised week has to *say* that
nothing moved, or it looks exactly like a quiet one. `-since` takes a duration
or a date, `-json` gives the same sections.

## What bounds a worker, and what does not

A task body is instructions to an agent that holds `bash`. Anyone who can add a
task can direct a model, and a task written by an earlier agent can carry
instructions the human never wrote. The bounds are real but partial.

What is bounded:

- one worktree per attempt, deleted afterwards; uncommitted work is discarded
  and only the branch survives
- a named tool allowlist rather than whatever the human's settings happen to be:
  implementers get `read`, `edit`, `write`, `bash`, `grep`, `find`, `ls`;
  planners and reviewers get `read`, `write`, `grep`, `find`, `ls`, with no
  `bash` and no `edit`
- no push, no merge, no branch switching: a worker cannot publish or reach
  another checkout through the tools it is told to use. `loa merge` is a
  separate command with its own guard, not a worker tool
- the task cannot name a filesystem path: it names a workspace label, and Herdr
  resolves it
- `-timeout` and `-max` bound how long and how often a worker runs

What is not bounded:

- **network access.** A worker can fetch, upload, or call anything the machine
  can reach.
- **reads outside the worktree.** `bash` can read `~/.ssh`, `~/.config`, this
  database, and the session history of previous agents.
- **prompt injection.** Anything a worker reads — a file, an issue, a web page —
  can redirect it. The worktree limits the blast radius, not the intent.
- **sandboxing.** There are no namespaces, no separate user, no seccomp filter:
  the agent runs as you do.

So the posture that follows: run the runner as a user whose credentials you are
willing to have an agent use, point tasks at workspaces that hold source code,
and review branches before merging them.

## Checking it against a live session

The gate cannot run Herdr, so the transport is a manual check after each Herdr
upgrade:

1. `herdr status` reports the same protocol the client was written against (22).
2. One task end to end: `loa run -max 1` produces a branch, and its
   workspace disappears.
3. One blocked task: the agent waits for approval, the task fails naming the
   pane, and the workspace is still there to answer it in.

The rest — argument construction and every JSON shape the client parses — is
covered by recorded fixtures in `internal/herdr/testdata/`.

## Checks

```
nix flake check              # build + tests + lint, in the sandbox
golangci-lint fmt            # rewrite: gofmt + goimports, local imports grouped
golangci-lint fmt --diff     # read-only form of the above
golangci-lint run            # lint + formatting; must print "0 issues."
go test ./...
```

`.golangci.yml` holds the whole policy: `errcheck`, `govet`, `ineffassign`,
`staticcheck`, `unused`, `revive`, `gocritic`, `errorlint`, `bodyclose`,
`gosec` — named one at a time rather than via `standard`, so upgrading the
linter cannot silently change what runs. `goimports` runs as the formatter with
`loa` as a local prefix, so the module's own imports sit in their own
group. Exemptions are narrow and each has a reason:

- errcheck ignores cleanup calls (`Close`, `Rollback`) on handles that are
  already going away; everything else is enforced.
- gosec ignores `G104` (unhandled errors — errcheck already owns that) and
  `G204` (variable command lines — commands are run through `exec` with an
  argument slice and no shell).
- one `//nolint:gosec` marks the single place where SQL text is assembled from
  placeholders while the values stay bound parameters.