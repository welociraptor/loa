-- A task names the Herdr workspace it belongs to, by label. The label is
-- resolved to a workspace id at dispatch time; nothing in the queue stores an
-- id, because ids are Herdr's handles and labels are what a human writes.
--
-- Empty means "unset": a task written before this column existed, or one added
-- without a workspace. Such a task is claimable and then fails at dispatch with
-- that reason, which is visible, rather than silently unclaimable forever.

ALTER TABLE tasks ADD COLUMN workspace TEXT NOT NULL DEFAULT '';