-- FTR.HMR.DEV-0002 R13: links of a feature to other features from its product
-- specification — "extends" («Опирается на») and "changes" (the section
-- «Изменения в других решениях», the parent of a fix). The rows of a feature
-- leave with its product document.

-- +goose Up
-- +goose StatementBegin
CREATE TABLE spec_links (
  source_key   TEXT NOT NULL,
  target_key   TEXT NOT NULL,                   -- may be outside the index (R17)
  kind         TEXT NOT NULL CHECK (kind IN ('extends', 'changes')),
  requirements TEXT[] NOT NULL DEFAULT '{}',
  source_path  TEXT NOT NULL REFERENCES spec_documents(path) ON DELETE CASCADE,
  PRIMARY KEY (source_key, target_key, kind)
);
CREATE INDEX spec_links_target ON spec_links (target_key);
CREATE INDEX spec_links_source_path ON spec_links (source_path);
-- +goose StatementEnd

-- +goose Down
-- +goose StatementBegin
DROP TABLE spec_links;
-- +goose StatementEnd
