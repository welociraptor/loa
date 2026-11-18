-- Wall time the attempt held the task, in milliseconds.
--
-- Measured at completion from the row's previous update, which is when the
-- attempt was leased (recording the workspace touches it again a moment later).
-- It is an approximation of agent time that costs no extra write and still
-- counts an attempt that crashed; it is not a token count.

ALTER TABLE tasks ADD COLUMN run_ms INTEGER NOT NULL DEFAULT 0;
