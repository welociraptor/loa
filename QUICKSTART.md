# Quickstart

The short path: build it, run the gate, prove one task end to end inside Herdr.

## Where things are

- Repo: `~/Repositories/loa`.
- Binary: `nix build` writes `./result/bin/loa`. In the dev shell,
  `go build ./cmd/loa` works too.
- Database: every command takes `-db PATH`; otherwise `$LOA_DB`, otherwise
  `loa.db` in the current directory.
- State: `$XDG_STATE_HOME/loa`, usually `~/.local/state/loa`. Triage proposals
  land in `loa/proposals/` there; put a dashboard database there too.

## The gate

```sh
nix develop --command go test ./...
nix develop --command golangci-lint run
nix flake check
```

The tests use a fake `herdr` binary and a fake runner backend, so green proves
the code, not that a live session agrees. After a Herdr upgrade, also check
`herdr status` (protocol 22), run one task end to end, and exercise one blocked
agent as described in `README.md`.

## Smoke loop (inside a Herdr pane)

```sh
# In ~/Repositories/loa:
nix build
export PATH="$PWD/result/bin:$PATH"
mkdir -p ~/.local/state/loa
export LOA_DB=~/.local/state/loa/quickstart.db

# A scratch repository with a commit, so a worktree has a base.
SCRATCH=$(mktemp -d)/demo
git init -q "$SCRATCH"
git -C "$SCRATCH" config user.name quickstart
git -C "$SCRATCH" config user.email quickstart@example.invalid
git -C "$SCRATCH" commit --allow-empty -q -m init

# A named workspace is how a task finds its repository.
herdr workspace create --cwd "$SCRATCH" --label demo

ID=$(loa add -title "add note.txt" -workspace demo \
  -body "Create note.txt containing hello, and commit it. Change nothing else.")
echo "task $ID"
loa run -max 1
```

What proves it worked:

```sh
git -C "$SCRATCH" branch --list "loa/$ID.*"   # loa/<id>.1
herdr workspace list                          # no workspace labelled "loa #<id>"
loa ps                                        # status done; session shows closed
loa report -since 1h                          # the task, its branch, its runtime
```

The runner deletes the worktree workspace after judging the branch. The branch
is the deliverable; uncommitted work is discarded.

## Break these on purpose

- **A task that changes nothing fails.** Add one whose body says to do nothing,
  run it, and `loa show ID` reports "finished without committing". No branch is
  left behind. Success is read from the repository, not the agent's summary.
- **An unknown workspace label fails at dispatch.** `loa add -workspace nope ...`
  fails with `no Herdr workspace is labelled "nope"`. With `-max 1` on the task,
  one failure blocks it. With the default two attempts, the second is only
  claimable after the retry delay; `loa show ID` prints `not before` until then,
  and `loa list -status blocked` shows it once attempts are spent.
- **The retry delay is 30 minutes.** A failed task carries `not_before`; a rerun
  in the same window leases nothing. `loa show ID` is the explanation.
- **The dashboard keys.** `loa dash` starts nothing by itself: `enter` focus,
  `x` interrupt, `u` unblock, `c` remove workspace, `d`/`f` resolve done/failed,
  `s`/`S` start/stop a run, `q` quit.
- **No session, no runner.** `loa dash`, `loa run` and `loa triage` exit 1
  outside a Herdr pane (`HERDR_ENV` is not 1). `add`, `list`, `show`, `ps`,
  `report`, `import`, `lease`, `done` and `fail` do not need one.

## Proposals

`loa triage` runs a read-only agent, writes a proposal under
`$XDG_STATE_HOME/loa/proposals/`, and never applies it. Apply deliberately:

```sh
loa triage                              # needs a Herdr pane
loa import -dry-run proposals/FILE.json # validate and print, create nothing
loa import proposals/FILE.json          # create tasks and priority changes
```

`after` names another task's title in the same file, so a proposal can order
work before ids exist. Unknown fields, a missing title reference, or a cycle
reject the whole document; `import` applies everything in one transaction.

## Traps

- Workspace labels are exact and unique. `demo`, `Demo` and `demo ` are
  different; two workspaces with the same label also fail resolution.
- The worktree path is chosen by Herdr's configuration. The runner passes no
  `--path`, so do not look for checkouts beside the repository.
- You cannot inject keys into the dashboard with `herdr pane run`. Attach to the
  pane and press them.
- A failed task is not retried in the same run: `-max` is that run's budget, and
  the retry delay keeps the task unclaimable anyway.
- `loa done` and `loa fail` require a live lease. When a task is already
  `blocked`, a human settles it from the dashboard: `u` unblocks it for one more
  attempt, `d`/`f` resolve it with a one-line result. The runner is not the
  authority on a blocked task.