-- Which plan a task came from. A plan is a proposal with phases; its tasks are
-- imported together, and the id is what lets `report` name the plan rather
-- than showing a pile of unrelated rows. Empty for tasks that were not planned.

ALTER TABLE tasks ADD COLUMN plan_id TEXT NOT NULL DEFAULT '';
