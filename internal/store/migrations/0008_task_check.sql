-- A review's check is a deterministic command run in its worktree. Only its
-- exit status lets a merge happen, so the outcome belongs on the review row.
-- `check_command` is empty when no check ran, which is itself a refusal to
-- merge; the output is kept for the person reading why it failed.

ALTER TABLE tasks ADD COLUMN check_command TEXT NOT NULL DEFAULT '';
ALTER TABLE tasks ADD COLUMN check_passed INTEGER NOT NULL DEFAULT 0;
ALTER TABLE tasks ADD COLUMN check_output TEXT NOT NULL DEFAULT '';