# taskflow

Task queue for a multi-agent daily workflow. The SQLite database is the only
shared state: the runner leases one task at a time, hands it to an agent, and
records the outcome. Agents never write to the store, so a crashed or
misbehaving worker cannot corrupt the queue.

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

taskflow add -title "..." -body "..." -priority 5 -scope repo:taskflow [-dep ID]...
taskflow list [-status new]...
taskflow show ID                      # fields, and why it is not running
taskflow lease -owner runner-1 -ttl 1h    # prints the task as JSON, or null
taskflow done ID -branch b/thing -result "shipped"
taskflow fail ID -result "test suite red"
taskflow run -workers 2 -max 5           # works the queue
```

`TASKFLOW_DB` selects the database (default `./taskflow.db`); every command
also takes `-db`.

## The runner

`taskflow run` is meant to be started by a timer, not by hand, so it has to be
safe to interrupt and safe to run twice:

- **Success is read from the repository, not from the agent.** A task is done
  only if its branch gained a commit. An agent that reports success and
  commits nothing fails, and its review text is recorded as the failure
  reason.
- **Scopes are the concurrency control.** `repo:<name>` is the checkout the
  task works in; every other scope is a plain exclusion domain. A task needs
  exactly one `repo:` scope and is rejected otherwise. Names are validated, so a
  machine-authored task cannot name a path outside the repos directory.
- **Each attempt gets its own worktree and branch** (`taskflow/<id>.<attempt>`),
  branched from `-base`. The worktree is deleted afterwards; the branch is the
  deliverable. Attempts that commit nothing also delete their branch, so a bad
  day does not litter the repository.
- **Rounds, not a worker pool.** Each round leases up to `-workers` runnable
  tasks and runs them together. A task freed by a finishing sibling is picked
  up by the next round instead of being missed by a worker that already gave
  up.
- **`-max` is the budget** for one run, so a daily timer gets a daily cap.
  Combined with the store's retry delay, a failure cannot be retried in the
  same run.
- **`-ttl` must exceed `-timeout`**, or a lease would lapse while the task is
  still running and a second worker would pick it up.
- **The agent runs in its own process group**, and cancellation kills the
  group. Killing only the direct child would leave whatever it spawned
  (a dev server, a test run) holding the worktree.

Defaults: `~/Repositories` for checkouts,
`$XDG_STATE_HOME/taskflow/worktrees` for scratch checkouts, `HEAD` as the base.

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
- `repo:` names are validated, so a scope cannot point outside `-repos`
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
willing to have an agent use, keep `-repos` to source code, and review branches
before merging them.

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