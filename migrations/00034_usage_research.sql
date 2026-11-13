-- FTR.HMR.DEV-0004 R7: the cost of a feature reads the analysis of its issues (research) by issue.
-- +goose Up
CREATE INDEX agent_usage_research ON agent_usage (issue_id, created_at) WHERE feature_id IS NULL AND context = 'discovery';

-- +goose Down
DROP INDEX agent_usage_research;
