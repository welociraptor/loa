# Herdr client fixtures

These are recorded `herdr` CLI documents at protocol 22, captured against herdr
0.9.5. They are the transport contract for `internal/herdr`: tests decode them,
they never generate or modify them. Values that identify a local checkout use
stable placeholders such as `/home/u`.

A few fixtures are derived from a recorded document because a healthy server
cannot produce them: `status_not_running.json`, `status_incompatible.json`,
`malformed_result.json`, `error_agent_not_ready.json` and the two
`*_missing_ids.json` files. They exist only to exercise the client's failure
paths; the envelope shape is unchanged.

| fixture | capture command |
|---|---|
| `status.json` | `herdr status --json` (server running and compatible) |
| `status_not_running.json` | `herdr status --json` (no server running) |
| `status_incompatible.json` | `herdr status --json` against a protocol-21 server |
| `workspace_list.json` | `herdr workspace list` |
| `agent_list.json` | `herdr agent list` |
| `worktree_create.json` | `herdr worktree create --workspace w1 --branch loa/7.1 --base HEAD --label 'loa #7' --no-focus` |
| `workspace_create.json` | `herdr workspace create --cwd ~/.local/state/loa/triage/1 --label 'loa triage'` |
| `worktree_create_missing_ids.json` | `worktree_create.json` result with the workspace and root pane ids removed |
| `workspace_create_missing_ids.json` | `workspace_create.json` result with the workspace and root pane ids removed |
| `malformed_result.json` | a `workspace list` envelope whose result is a string |
| `error_blocked.json` | `herdr agent prompt loa-7-1 'do it' --wait` (agent blocked) |
| `error_agent_not_found.json` | `herdr agent wait loa-7-1 --until done --timeout 1` |
| `error_agent_not_ready.json` | an `agent start` error envelope with the not-ready code |
| `error_workspace_not_found.json` | `herdr workspace close w3` |

Fixtures are never hand-edited to make a test pass. If the server schema changes,
re-record the fixture, update the protocol above, and review the decode failure.