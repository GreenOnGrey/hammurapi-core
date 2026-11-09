-- FTR.HMR.DEV-0003: the document contract of GreenOnGrey/speclint.
-- spec_links get a layer: «product» from the product document, «arch» from the
-- arch document (R15); every link written so far is of product. Decisions A<n>
-- of arch live in spec_requirements beside R<n>: their IDs never collide, the
-- kind tells them apart (R10). spec_index_state keeps the version of the
-- contract the index was written by: a new version reindexes product and arch.

-- +goose Up
-- +goose StatementBegin
ALTER TABLE spec_links ADD COLUMN layer TEXT NOT NULL DEFAULT 'product' CHECK (layer IN ('product', 'arch'));
ALTER TABLE spec_links DROP CONSTRAINT spec_links_pkey;
ALTER TABLE spec_links ADD PRIMARY KEY (source_key, target_key, kind, layer);
ALTER TABLE spec_requirements ADD COLUMN kind TEXT NOT NULL DEFAULT 'requirement' CHECK (kind IN ('requirement', 'decision'));
CREATE TABLE spec_index_state (
  key   TEXT PRIMARY KEY,
  value TEXT NOT NULL
);
-- +goose StatementEnd

-- +goose Down
-- +goose StatementBegin
DROP TABLE spec_index_state;
DELETE FROM spec_requirements WHERE kind = 'decision';
ALTER TABLE spec_requirements DROP COLUMN kind;
DELETE FROM spec_links WHERE layer = 'arch';
ALTER TABLE spec_links DROP CONSTRAINT spec_links_pkey;
ALTER TABLE spec_links ADD PRIMARY KEY (source_key, target_key, kind);
ALTER TABLE spec_links DROP COLUMN layer;
-- +goose StatementEnd
