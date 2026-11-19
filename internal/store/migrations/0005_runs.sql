-- A run is one coordinator session: who started it, how many workers and
-- tasks it was allowed, and how much it settled. A run that is still going has
-- an empty ended_at; a finished one keeps the moment it stopped.
--
-- At most one run may be active at a time. The unique index below enforces
-- that in SQLite, so two processes racing to start a run cannot both succeed
-- regardless of how their calls are ordered.

CREATE TABLE runs (
    id         INTEGER PRIMARY KEY AUTOINCREMENT,
    owner      TEXT    NOT NULL,
    started_at TEXT    NOT NULL,
    ended_at   TEXT    NOT NULL DEFAULT '',
    workers    INTEGER NOT NULL DEFAULT 1,
    max_tasks  INTEGER NOT NULL DEFAULT 1,
    done       INTEGER NOT NULL DEFAULT 0,
    failed     INTEGER NOT NULL DEFAULT 0
);

CREATE UNIQUE INDEX runs_one_active ON runs (ended_at) WHERE ended_at = '';