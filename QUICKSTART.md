# Getting started with loa

loa is a task queue for work you hand to coding agents. You write a task down,
loa gives it to one agent in a fresh git worktree, and records what happened. A
task is only considered done when its branch contains a commit; the agent's own
claim of success does not count.

The queue lives in a SQLite file. There is no daemon and no timer: nothing moves
until you start a run.

## The moving parts

- **Task**: a title, instructions for the agent, and a workspace label. The
  instructions are called the body.
- **Workspace**: a Herdr workspace that points at a git repository. A task names
  the workspace *label*, never a filesystem path.
- **Run**: a person or a trusted coordinator starts it inside a Herdr pane. loa
  leases runnable tasks, starts agents in worktree workspaces, and cleans the
  worktrees up afterwards.
- **Branch**: each attempt works on `loa/<task-id>.<attempt>`. That branch is
  the deliverable. Uncommitted work is discarded.

## What you need

- A running Herdr session. Everything that starts an agent must itself run
  inside a Herdr pane.
- `git`, and a repository with at least one commit to branch from.
- An agent CLI that Herdr can start. The default is `pi`; use `-kind` to choose
  another.
- Nix, or an already-built `loa` binary.

Commands that only read or change the queue (`add`, `list`, `show`, `ps`,
`report`, `import`, `lease`, `done`, `fail`) work outside Herdr. Commands that
need a session (`run`, `triage`) refuse to start outside a Herdr pane.

## Install

From this repository:

```sh
nix profile install .#default
loa help
```

Or build it and run it in place:

```sh
nix build
./result/bin/loa help
```

For the rest of this guide, `loa` means either the installed command or
`./result/bin/loa`.

## Choose the database

By default loa uses `loa.db` in the current directory. For real use, put it in
your state directory and set `LOA_DB` once in your shell profile:

```sh
mkdir -p ~/.local/state/loa
export LOA_DB="$HOME/.local/state/loa/tasks.db"
```

Every command also accepts `-db PATH`, which is useful for a separate queue.
The file is created owner-only and is safe to inspect with `sqlite3`.

loa's own state (triage proposals) lives in `$XDG_STATE_HOME/loa`, usually
`~/.local/state/loa`.

## 1. Give a repository a Herdr workspace

Create a workspace for the repository you want agents to work on. Its label is
how tasks will refer to it:

```sh
herdr workspace create --cwd "$HOME/code/my-project" --label my-project
herdr workspace list
```

Labels are exact and unique. `my-project`, `My-Project` and `my-project ` are
different labels, and two workspaces with the same label cannot be resolved.
The repository must have a commit: by default tasks branch from `HEAD`.

To try loa without touching a real repository, make a scratch one first:

```sh
DEMO=$(mktemp -d)/demo
git init -q "$DEMO"
git -C "$DEMO" config user.name demo
git -C "$DEMO" config user.email demo@example.invalid
git -C "$DEMO" commit --allow-empty -q -m init
herdr workspace create --cwd "$DEMO" --label demo
```

## 2. Add a task

```sh
loa add -workspace my-project -title "Document the build" \
  -body "Add a short Build section to README.md explaining how to build the project. Commit the change. Do not change anything else."
```

`loa add` prints the task id. The body is the prompt the agent receives, so
write it as instructions to a capable coding agent: what to change, what to
verify, and that the work must be committed. Keep one task to one coherent
change.

Useful options:

```sh
loa add -workspace my-project -title "..." -body "..." \
  -priority 5 \
  -max 3 \
  -scope repo:my-project \
  -dep 7
```

- `-priority` is a number; higher runs first. Default `0`.
- `-max` is how many attempts the task gets before it is blocked. Default `2`.
  Do not confuse it with `loa run -max`, which is a per-run budget.
- `-scope` is a mutual-exclusion domain. Two leased tasks never share a scope,
  so tasks that touch the same repository can carry the same
  `repo:my-project` scope. It is not a path and it is not a sandbox.
- `-dep` waits for another task to be `done` before this one can run. Repeat
  the flag for multiple dependencies.

## 3. Run the queue

In a Herdr pane:

```sh
loa run -max 1
```

The first run is deliberately limited to one task. For each task, loa:

1. leases the highest-priority runnable task;
2. resolves its workspace label to a Herdr workspace;
3. creates a worktree on a new branch from `HEAD`;
4. starts the agent in that worktree and sends it the task body plus the
   instructions that apply to every task;
5. waits for the agent, reads the branch, and removes the worktree;
6. records the branch and result, and marks the task done only if the branch
   gained a commit.

Progress and the final `done`/`failed` count go to stderr. A worktree is
scratch space; it is removed even when the task succeeds.

`loa run` defaults to two concurrent workers, five tasks per run, a 45-minute
task timeout, and a two-hour lease. The lease must outlive the task timeout. If
you change them, keep that true:

```sh
loa run -workers 2 -max 5 -timeout 30m -ttl 1h
```

Work is picked up in rounds. If one task finishes and frees another task's
scope, the next round can lease it. It is safe to interrupt a run; a lease whose
worker disappears is reclaimed when it expires.

## 4. Review the result

```sh
loa list
loa show 1
loa ps
loa report -since 1h
```

`loa list` shows the queue. `loa show ID` explains a task in full, including why
it is or is not runnable. `loa ps` joins the queue with what the Herdr session
says is happening now. `loa report` summarizes a time window.

The work is on the task's branch, not in a worktree left beside the repository:

```sh
git -C "$HOME/code/my-project" branch --list 'loa/1.*'
git -C "$HOME/code/my-project" log --oneline HEAD..loa/1.1
git -C "$HOME/code/my-project" diff HEAD...loa/1.1
```

Review that branch like any other contribution, then merge it yourself, or let
an approved review with a green check merge with `loa merge`. loa never pushes.

## Day-to-day use

With the queue trusted, run it normally:

```sh
loa run
```

## Watching a queue

`loa show ID` is the first thing to read when a task is not moving:

```sh
loa show 12
```

It prints the status, attempts, branch, result, and either `runnable: yes` or
the reasons it is waiting. Common reasons are:

- `retry delayed until ...`: the task failed and must wait 30 minutes before it
  can be claimed again;
- `waiting on #N (...)`: a dependency is not `done` yet;
- `scope "repo:my-project" is held by #N`: another task is running in the same
  exclusion domain;
- `leased by ... until ...`: a worker currently owns it.

A task that spends its attempts becomes `blocked`. A new run does not retry it
until you requeue or resolve it.

## When an agent needs a human

loa never answers an approval dialog or a question. If an agent blocks, the
attempt fails with the workspace and pane named in the result, and loa leaves
the pane open. The task is `blocked` if that was its last attempt; otherwise it
waits out the retry delay.

```sh
loa ps                 # find the task and its workspace
loa show 12            # read the failure reason
```

Answer the agent in its pane. Then choose what the task's outcome should be:

```sh
# The human answered the approval and the work is acceptable.
loa resolve 12 -result "approved and checked"

# The task needs a human decision before it is considered done.
loa resolve 12 -fail -result "needs a decision on the API"
```

If the task is `blocked` and you want it tried again now:

```sh
loa unblock 12
```

`loa resolve` marks a non-leased task `done` by default, or `blocked` with
`-fail`; it records the result text. `loa unblock` puts a blocked task back in
the queue and keeps the attempt count. If the task had spent its attempts, the
next failure blocks it again rather than looping.

`loa done` and `loa fail` are for a worker that still holds a lease. They are
not the way to settle a blocked task.

## Dependencies, priority and scopes

Use these to make a queue that can be worked safely in parallel:

```sh
API=$(loa add -workspace my-project -title "Add the API endpoint" \
  -body "..." -priority 5 -scope repo:my-project)

loa add -workspace my-project -title "Document the API endpoint" \
  -body "..." -dep "$API" -priority 4 -scope repo:my-project
```

The documentation task cannot run until the API task is `done`. Both carry the
same scope, so two workers will not edit the same repository at once.

A dependency orders work; it does not stack branches. A dependent task branches
from the configured base (`HEAD` by default), not from its dependency's branch.
Merge or otherwise make the dependency's work part of the base before expecting
the dependent task to see it. Use `-base` to choose a different ref.

## Proposals from an agent

`loa triage` runs a separate agent in a scratch directory and asks it to
propose work. The agent gets `read`, `grep`, `find`, `ls` and `write`, but no
`bash`. It writes a JSON proposal under `$XDG_STATE_HOME/loa/proposals/`
(usually `~/.local/state/loa/proposals/`) and never applies it:

```sh
loa triage
```

Review the proposal, then apply it deliberately:

```sh
loa import -dry-run ~/.local/state/loa/proposals/FILE.json
loa import ~/.local/state/loa/proposals/FILE.json
```

`-dry-run` validates and prints the result without creating anything. `import`
applies the whole document in one transaction. Unknown fields, a missing title
reference, or a dependency cycle rejects the entire document.

A proposal can create tasks and change priorities. `after` names another task
in the same proposal by title, so work can be ordered before ids exist:

```json
{
  "tasks": [
    {
      "title": "Add the API endpoint",
      "body": "...",
      "workspace": "my-project",
      "priority": 5,
      "scopes": ["repo:my-project"]
    },
    {
      "title": "Document the API endpoint",
      "body": "...",
      "workspace": "my-project",
      "after": ["Add the API endpoint"]
    }
  ],
  "reprioritise": [{"id": 4, "priority": 9}],
  "notes": ["#2 has no body, so it cannot be actioned"]
}
```

Notes are printed by both `triage` and `import`; they are for the human and are
never applied.

A trusted coordinator can apply and run by itself: `loa import FILE`, then
`loa run -json`, and read the record with `loa runs`. A blocked task still goes
to a person. That boundary is policy, not a sandbox.

## Command reference

| Command | Purpose |
| --- | --- |
| `loa add -title T -workspace LABEL [-body B] [-priority N] [-max N] [-scope S]... [-dep ID]...` | Add a task and print its id. |
| `loa list [-status STATUS]...` | List tasks, optionally filtered by `new`, `leased`, `done` or `blocked`. |
| `loa show ID` | Show one task and why it is or is not runnable. |
| `loa ps [-json]` | Show the queue joined with live session state. |
| `loa report [-since 24h\|YYYY-MM-DD] [-json]` | Show what happened in a window. |
| `loa run [flags] [-json]` | Work the queue inside Herdr; `-json` emits machine-readable results. |
| `loa runs` | List the run record, newest first. |
| `loa merge ID [-into BRANCH]` | Merge an approved, green review; refuses a dirty checkout, a wrong branch or a conflict. |
| `loa unblock ID` | Return a blocked task to the queue. |
| `loa resolve ID [-fail] [-result TEXT]` | Settle a non-leased task done, or blocked with `-fail`. |
| `loa triage [-timeout D] [-model M] [-kind K] [-out DIR]` | Ask an agent for a proposal; never applies it. |
| `loa import [FILE\|-] [-dry-run]` | Apply a proposal atomically. |
| `loa lease [-owner NAME] [-ttl D]` | Lease one task as JSON, or `null`; for a worker you drive yourself. |
| `loa done ID [-branch B] [-result TEXT]` | Mark a held lease done. |
| `loa fail ID [-result TEXT]` | Fail a held lease. |

`loa run` accepts `-workers`, `-max`, `-base`, `-check`, `-timeout`, `-ttl`,
`-owner`, `-model` and `-kind`. All commands accept `-db PATH`. `loa help`
prints the short reference.

## Limits and safety

A task body is instructions to an agent with `bash`, not a sandbox policy.
Anyone who can add a task can direct a model, and a task written by an earlier
agent can carry instructions you did not write.

What loa limits:

- one worktree per attempt, removed afterwards; only the branch survives;
- a fixed tool allowlist: `read`, `edit`, `write`, `bash`, `grep`, `find`, `ls`;
- the worker is told not to push, switch branches or touch other checkouts; the
  runner itself never does them;
- a workspace label, not a path supplied by the task;
- a task timeout and a per-run budget.

What it does not limit:

- network access;
- reads outside the worktree, including your home directory and credentials;
- prompt injection from files, issues or pages the agent reads;
- the agent's operating-system permissions: it runs as you do.

Run loa as a user whose credentials you are willing to let an agent use, point
workspaces at source repositories, and review branches before merging them.

## Troubleshooting

**`not inside a Herdr pane`**

Start `loa run` or `loa triage` from a Herdr pane. Reading commands do not need
one.

**`no Herdr workspace is labelled "..."`**

The label is exact. Check `herdr workspace list` and create the workspace if
needed. Two workspaces with the same label also fail resolution.

**`finished without committing`**

The agent made no commit, so the attempt failed and its branch was deleted. Make
the body explicitly require a commit and keep the task small enough to finish.

**A task is not running**

Run `loa show ID`. It prints the retry delay, dependency, scope or lease that is
holding it. A failed task with attempts left waits 30 minutes; one that has
spent its attempts is `blocked` until you resolve or unblock it. A run's `-max`
is a budget, so a failure cannot be retried inside the same run.

**An agent is waiting for approval**

Find it with `loa ps`, answer it in its pane, then `loa resolve` or `loa unblock`.
The workspace stays open on purpose.

**Herdr version or protocol mismatch**

loa checks `herdr status` before it starts. If the server protocol is different,
upgrade or rebuild loa against the Herdr version you are running.

For the design behind the queue, see `README.md`.