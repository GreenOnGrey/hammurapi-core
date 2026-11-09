-- FTR.HMR.DEV-0003 R20: templates of the default branch that break the document
-- contract — a commit in git past Hammurapi. A row per file, removed when the
-- file is fixed; the administrators of the area see a banner.

-- +goose Up
-- +goose StatementBegin
CREATE TABLE rules_violations (
  area        TEXT NOT NULL,
  file        TEXT NOT NULL CHECK (file IN ('template', 'fix-template')),
  sha         TEXT NOT NULL,
  violations  JSONB NOT NULL,
  checked_at  TIMESTAMPTZ NOT NULL DEFAULT now(),
  PRIMARY KEY (area, file)
);
-- +goose StatementEnd

-- +goose Down
-- +goose StatementBegin
DROP TABLE rules_violations;
-- +goose StatementEnd
