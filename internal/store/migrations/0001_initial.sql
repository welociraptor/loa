-- The initial queue. Later changes go in a new numbered file; this one is
-- never edited, because databases in the field have already run it.

CREATE TABLE tasks (
    id               INTEGER PRIMARY KEY,
    title            TEXT    NOT NULL,
    body             TEXT    NOT NULL DEFAULT '',
    status           TEXT    NOT NULL DEFAULT 'new'
                     CHECK (status IN ('new', 'leased', 'done', 'blocked')),
    priority         INTEGER NOT NULL DEFAULT 0,
    attempts         INTEGER NOT NULL DEFAULT 0,
    max_attempts     INTEGER NOT NULL DEFAULT 2,
    branch           TEXT,
    result           TEXT,
    created_at       TEXT    NOT NULL,
    updated_at       TEXT    NOT NULL,
    -- When a failed or abandoned task becomes claimable again. Retries are
    -- deliberately delayed: an immediate retry would spend every attempt on the
    -- same transient condition and starve the rest of the queue.
    not_before       TEXT,
    lease_owner      TEXT,
    lease_expires_at TEXT,
    -- A lease is all-or-nothing: it is either held by someone with an expiry,
    -- or not held at all. Anything else means a writer bypassed the store.
    CHECK (status <> 'leased' OR (lease_owner IS NOT NULL AND lease_expires_at IS NOT NULL)),
    CHECK (status = 'leased' OR (lease_owner IS NULL AND lease_expires_at IS NULL))
);

CREATE INDEX tasks_claimable ON tasks (status, priority DESC, id);

-- Scopes are coarse mutual-exclusion domains (a repo, a path, a service).
-- Two leased tasks may never share one, which is what keeps parallel workers
-- from colliding.
CREATE TABLE task_scopes (
    task_id INTEGER NOT NULL REFERENCES tasks (id) ON DELETE CASCADE,
    scope   TEXT    NOT NULL CHECK (scope <> ''),
    PRIMARY KEY (task_id, scope)
);

CREATE INDEX task_scopes_scope ON task_scopes (scope);

CREATE TABLE task_deps (
    task_id    INTEGER NOT NULL REFERENCES tasks (id) ON DELETE CASCADE,
    depends_on INTEGER NOT NULL REFERENCES tasks (id) ON DELETE CASCADE,
    PRIMARY KEY (task_id, depends_on),
    CHECK (task_id <> depends_on)
);