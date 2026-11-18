# taskflow

Task queue for a multi-agent daily workflow. The SQLite database is the only
shared state: the runner leases one task at a time, hands it to an agent in a
Herdr worktree workspace, and records the outcome. Agents never write to the
store, so a crashed or misbehaving worker cannot corrupt the queue.

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
go build ./cmd/taskflow

taskflow add -workspace nixos -title "..." -body "..." -priority 5 [-scope repo:taskflow]... [-dep ID]...
taskflow list [-status new]...
taskflow show ID                      # fields, and why it is not running
taskflow ps [-json]                   # the queue joined with the live session
taskflow dash                         # the tab: state, and the keys to act on it
taskflow report [-since 24h|-json]    # what the day did, and how long it has been still
taskflow lease -owner runner-1 -ttl 1h    # prints the task as JSON, or null
taskflow done ID -branch b/thing -result "shipped"
taskflow fail ID -result "test suite red"
taskflow run -workers 2 -max 5           # works the queue, inside Herdr
```

`TASKFLOW_DB` selects the database (default `./taskflow.db`); every command
also takes `-db`.

Dependencies: `modernc.org/sqlite` (pure Go, no cgo) and
`github.com/charmbracelet/bubbletea` for the tab. The second brings a renderer
stack with it — that is the price of a real terminal loop instead of hand-rolled
raw mode.

## The runner

Everything runs inside Herdr: the runner itself is started in a pane, creates a
worktree workspace per task with `herdr worktree create`, starts the agent in
that workspace's pane, prompts it and reads its terminal. Nothing is exec'd
except the `herdr` client and `git`, and no worktree is created by this code.
It has to be safe to interrupt and safe to run twice:

- **Success is read from the repository, not from the agent.** A task is done
  only if its branch gained a commit. An agent that reports success and
  commits nothing fails, and its review text is recorded as the failure
  reason.
- **The workspace is where the work happens.** A task names a Herdr workspace
  *label*, resolved to an id at dispatch: exact match, or the task fails. No
  filesystem path comes from task text at all — Herdr owns the repository.
- **Scopes are the concurrency control.** Every scope, `repo:` included, is an
  exclusion domain: two leased tasks never share one, so two workers never
  fight over the same repository.
- **Each attempt gets its own worktree workspace and branch**
  (`taskflow/<id>.<attempt>`), branched from `-base`, labelled
  `taskflow #<id>` so it can be found again. `herdr worktree remove` deletes the
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
  `taskflow #N` that no row recorded, provided that task is not being worked on
  — the window a crashed run leaves behind. A workspace carrying an id recorded
  for a *different* task is left alone and logged: it was not this runner's to
  create, so removing it is not this runner's decision.
- **The ids are recorded, not derived.** `workspace_id` and `pane_id` are written
  at dispatch and kept after the task finishes, so `ps` can say where an attempt
  ran and reconciliation can prove it owns a workspace. The `taskflow #N` label
  is the fallback, not the authority.

Defaults: `HEAD` as the base, and Herdr's own configuration decides where the
checkout lives — the runner passes no `--path`, and reads the created path back
from the result.

`taskflow run` refuses to start outside a Herdr pane, and checks
`herdr status` first: a protocol or version mismatch fails the run before it
does anything.

## The tab

`taskflow dash` is a full-screen view of the queue joined with the session, and
the only place a run is started from:

```
taskflow  1 running  0 blocked  2 queued

  ID   STATUS   WORKSPACE    SESSION            TITLE
> 1    leased   nixos        w3 working tf-1-1  wire the report command
  2    new      nixos        -                  needs a decision on the timer
  3    new      nixos        -                  bump the flake

enter focus  x interrupt  d/f resolve  u unblock  c close  s/S run  q quit
```

`enter` jumps to the task's pane (`agent focus`, or `workspace focus` when the
agent has already exited), `x` interrupts, `u` returns a blocked task to the
queue, `c` removes its workspace, `d`/`f` resolve it with a one-line result,
`s`/`S` start and stop a run, `q` quits. It shows state and never announces it:
Herdr already marks an agent that needs input.

One tab, set up once:

```
herdr workspace create --cwd "$XDG_STATE_HOME/taskflow" --label taskflow
# then, in that workspace's root pane:
TASKFLOW_DB="$XDG_STATE_HOME/taskflow/tasks.db" taskflow dash
```

Task worktrees are separate linked workspaces under the repository's own
workspace, so the tab is an index rather than a container.

## The day

`taskflow report` is the page to read after a day away, and it needs no model:

```
since 2026-11-17T18:43:33Z, 24h0m0s ago

done: 1
  #1  add note.txt   taskflow/1.1   ran 16s   note.txt [taskflow/1.1 f640919] feat: add note.txt

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
- a named tool allowlist (`read`, `edit`, `write`, `bash`, `grep`, `find`, `ls`)
  rather than whatever the human's settings happen to be
- no push, no merge, no branch switching: a worker cannot publish or reach
  another checkout through the tools it is told to use
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
2. One task end to end: `taskflow run -max 1` produces a branch, and its
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
`taskflow` as a local prefix, so the module's own imports sit in their own
group. Exemptions are narrow and each has a reason:

- errcheck ignores cleanup calls (`Close`, `Rollback`) on handles that are
  already going away; everything else is enforced.
- gosec ignores `G104` (unhandled errors — errcheck already owns that) and
  `G204` (variable command lines — commands are run through `exec` with an
  argument slice and no shell).
- one `//nolint:gosec` marks the single place where SQL text is assembled from
  placeholders while the values stay bound parameters.