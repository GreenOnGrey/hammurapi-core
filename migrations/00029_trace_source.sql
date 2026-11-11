-- FTR.HMR.SPEC-0002 R15, tech §3.6: the fingerprint of the documents an indexed
-- feature was projected from (requirements, services, test cases). Empty until
-- the first check after the upgrade projects the feature.

-- +goose Up
ALTER TABLE features ADD COLUMN trace_source TEXT;

-- +goose Down
ALTER TABLE features DROP COLUMN trace_source;
