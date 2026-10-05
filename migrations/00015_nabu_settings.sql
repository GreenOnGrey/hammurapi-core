-- FTR.HMR.CMN-0006 arch §9 "+3": the connection to Nabu — bindings of the
-- scenarios to service agents of Nabu and the mark of the transfer.
-- +goose Up
INSERT INTO admin_settings (key, value) VALUES ('nabu', '{"scenarios":{},"migrated":false}') ON CONFLICT DO NOTHING;
-- +goose Down
DELETE FROM admin_settings WHERE key = 'nabu';
