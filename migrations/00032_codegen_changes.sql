-- FTR.HMR.DEV-0004 arch §4.5: the archive of the changes of an agent kept for «Повторить коммит».
-- +goose Up
ALTER TABLE agent_tasks
  ADD COLUMN changes_key   TEXT,
  ADD COLUMN changes_until TIMESTAMPTZ;
CREATE INDEX agent_tasks_changes ON agent_tasks (changes_until) WHERE changes_key IS NOT NULL;

-- +goose Down
DROP INDEX agent_tasks_changes;
ALTER TABLE agent_tasks DROP COLUMN changes_until, DROP COLUMN changes_key;
