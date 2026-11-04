-- FTR.HMR.CMN-0008 R1, R2: the readiness check of the code of the agent in a
-- service; its changes go to the history of the service.

-- +goose Up
ALTER TABLE services ADD COLUMN check_command TEXT, ADD COLUMN check_timeout_sec INTEGER;
ALTER TABLE activity DROP CONSTRAINT activity_subject_type_check;
ALTER TABLE activity ADD CONSTRAINT activity_subject_type_check CHECK (subject_type IN ('issue', 'feature', 'release', 'service'));

-- +goose Down
DELETE FROM activity WHERE subject_type = 'service';
ALTER TABLE activity DROP CONSTRAINT activity_subject_type_check;
ALTER TABLE activity ADD CONSTRAINT activity_subject_type_check CHECK (subject_type IN ('issue', 'feature', 'release'));
ALTER TABLE services DROP COLUMN check_timeout_sec, DROP COLUMN check_command;
