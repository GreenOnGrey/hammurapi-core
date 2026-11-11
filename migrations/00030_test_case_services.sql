-- FTR.HMR.SPEC-0005 R1: the services of a test case of qa; empty — every service of the feature.
-- +goose Up
ALTER TABLE test_cases ADD COLUMN services TEXT[] NOT NULL DEFAULT '{}';

-- +goose Down
ALTER TABLE test_cases DROP COLUMN services;
