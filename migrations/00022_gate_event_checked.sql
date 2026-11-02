-- FTR.HMR.DEV-0001 R22: the writer checked a gate after a change above it and
-- changed nothing. A value of an enum cannot be removed: Down leaves it.
-- +goose NO TRANSACTION
-- +goose Up
ALTER TYPE gate_event_type ADD VALUE IF NOT EXISTS 'checked';

-- +goose Down
SELECT 1;
