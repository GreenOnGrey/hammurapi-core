-- FTR.HMR.CMN-0007 tech §9.1 (R11, R15): code tasks run in the sandbox of
-- Nabu. A task keeps the run of Nabu, the class of its error and the warnings
-- of its changes. Tasks the runner was doing restart under the new scheme:
-- the branch of a task is the same, so no second PR appears.
-- +goose Up
ALTER TABLE agent_tasks
  ADD COLUMN nabu_run_id UUID,
  ADD COLUMN error_class TEXT,
  ADD COLUMN warnings JSONB NOT NULL DEFAULT '[]';
CREATE INDEX agent_tasks_nabu_run ON agent_tasks (nabu_run_id) WHERE nabu_run_id IS NOT NULL;

-- Effects of the runner nobody handles any more.
UPDATE outbox SET failed_at = now(), last_error = 'FTR.HMR.CMN-0007: the runner is gone'
  WHERE effect IN ('runner.start', 'runner.stop') AND sent_at IS NULL AND failed_at IS NULL;
UPDATE agent_tasks SET status = 'queued', executor_ref = NULL, token_hash = NULL, started_at = NULL
  WHERE status = 'running';
UPDATE workflow_runs SET state = 'queued', next_run_at = now()
  WHERE kind = 'codegen_task' AND state IN ('starting', 'running');
-- The settings of the agent of Hammurapi and its transfer are gone (R3).
UPDATE admin_settings SET value = value - 'migrated' WHERE key = 'nabu';
-- +goose Down
ALTER TABLE agent_tasks DROP COLUMN warnings, DROP COLUMN error_class, DROP COLUMN nabu_run_id;
