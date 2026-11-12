-- FTR.HMR.DEV-0004 R17, tech §15: when the sync of open PRs last compared a PR with the provider;
-- a pass takes the PRs compared longest ago first.
-- +goose Up
ALTER TABLE pull_requests ADD COLUMN synced_at TIMESTAMPTZ;
CREATE INDEX pull_requests_open_sync ON pull_requests (synced_at NULLS FIRST) WHERE state = 'open';

-- +goose Down
DROP INDEX pull_requests_open_sync;
ALTER TABLE pull_requests DROP COLUMN synced_at;
