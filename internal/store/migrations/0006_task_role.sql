-- What a task is for: which prompt file and tool set its worker gets. The
-- default keeps tasks written before roles existed on the implementer path.
--
-- `prompt` and `prompt_hash` are the instructions the last dispatch used, so a
-- result or verdict can be attributed to the exact prompt that produced it.
-- They are empty until a dispatch fills them.

ALTER TABLE tasks ADD COLUMN role TEXT NOT NULL DEFAULT 'implement';
ALTER TABLE tasks ADD COLUMN prompt TEXT NOT NULL DEFAULT '';
ALTER TABLE tasks ADD COLUMN prompt_hash TEXT NOT NULL DEFAULT '';
