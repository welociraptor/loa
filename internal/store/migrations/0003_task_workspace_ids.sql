-- Which workspace and pane a task's current attempt owns.
--
-- These are what the attempt produced, like `branch`, not what Herdr currently
-- thinks: the dashboard joins on them, and reconciliation uses the id as proof
-- that the runner created a workspace. They are not cleared when a task
-- finishes — the row keeps the last attempt's ids as history — and the next
-- attempt overwrites them.

ALTER TABLE tasks ADD COLUMN workspace_id TEXT NOT NULL DEFAULT '';
ALTER TABLE tasks ADD COLUMN pane_id TEXT NOT NULL DEFAULT '';
