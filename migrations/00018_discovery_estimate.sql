-- FTR.HMR.DISC-0001 arch §4.5: the draft estimate of the work in the
-- Discovery document and its revisions, the summary of the changes of a
-- revision and the instructions it answered; the estimate goes with the
-- accepted issue to its feature.
-- +goose Up
ALTER TABLE discovery_docs      ADD COLUMN estimate JSONB, ADD COLUMN change_summary TEXT;
ALTER TABLE discovery_revisions ADD COLUMN estimate JSONB, ADD COLUMN change_summary TEXT,
                                ADD COLUMN request_ids UUID[] NOT NULL DEFAULT '{}';
ALTER TABLE features            ADD COLUMN estimate JSONB;
-- +goose Down
ALTER TABLE features            DROP COLUMN estimate;
ALTER TABLE discovery_revisions DROP COLUMN request_ids, DROP COLUMN change_summary, DROP COLUMN estimate;
ALTER TABLE discovery_docs      DROP COLUMN change_summary, DROP COLUMN estimate;
