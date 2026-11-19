-- A merge is a recorded transition on the implementation task. Both columns
-- are empty until `loa merge` succeeds; `merged_at` is what makes a second
-- merge a refusal instead of another merge commit.

ALTER TABLE tasks ADD COLUMN merged_at TEXT NOT NULL DEFAULT '';
ALTER TABLE tasks ADD COLUMN merge_commit TEXT NOT NULL DEFAULT '';