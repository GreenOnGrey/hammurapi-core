-- FTR.HMR.DEV-0004 arch §4.1: the attribution of a run of an agent — its run of Nabu, the gates of the writer,
-- the service of a code task, the reason and whether its price is known; the usage of a running task.
-- +goose Up
ALTER TABLE agent_usage
  ADD COLUMN nabu_run_id UUID,
  ADD COLUMN areas       TEXT[] NOT NULL DEFAULT '{}',
  ADD COLUMN service_id  UUID REFERENCES services(id) ON DELETE SET NULL,
  ADD COLUMN reason      TEXT CHECK (reason IN ('generate','regenerate','request','answers','impact',
                                                'first','retry','review','update_pr','revert','check')),
  ADD COLUMN cost_known  BOOLEAN NOT NULL DEFAULT true;
CREATE INDEX agent_usage_feature ON agent_usage (feature_id, created_at) WHERE feature_id IS NOT NULL;
CREATE INDEX agent_usage_run ON agent_usage (nabu_run_id) WHERE nabu_run_id IS NOT NULL;

-- R7: the old rows of code tasks get the service and the run of their task.
UPDATE agent_usage u SET service_id = t.service_id, nabu_run_id = t.nabu_run_id
  FROM agent_tasks t WHERE u.task_id = t.id AND u.service_id IS NULL;

ALTER TABLE agent_tasks ADD COLUMN usage JSONB;

-- +goose Down
ALTER TABLE agent_tasks DROP COLUMN usage;
DROP INDEX agent_usage_run;
DROP INDEX agent_usage_feature;
ALTER TABLE agent_usage DROP COLUMN cost_known, DROP COLUMN reason, DROP COLUMN service_id,
  DROP COLUMN areas, DROP COLUMN nabu_run_id;
