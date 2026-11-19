-- A review task's deliverable is a verdict, not a commit. The verdict is one
-- of approve, changes or blocked; the notes are the JSON array a person reads
-- with `loa show` or `loa report`. Both are empty until a review finishes.

ALTER TABLE tasks ADD COLUMN verdict TEXT NOT NULL DEFAULT '';
ALTER TABLE tasks ADD COLUMN verdict_notes TEXT NOT NULL DEFAULT '';