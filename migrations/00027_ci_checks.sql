-- FTR.HMR.SPEC-0002 tech §3.4: the tokens of services for CI and the status of
-- the check of speclint in the repository of specifications and of services.

-- +goose Up
-- +goose StatementBegin
CREATE TABLE service_tokens (
  id            UUID PRIMARY KEY DEFAULT gen_random_uuid(),
  service_id    UUID NOT NULL REFERENCES services(id) ON DELETE CASCADE,
  token_hash    BYTEA NOT NULL UNIQUE,          -- sha256 of the token; the token is shown once
  prefix        TEXT NOT NULL,
  created_by    UUID NOT NULL REFERENCES users(id),
  created_at    TIMESTAMPTZ NOT NULL DEFAULT now(),
  last_used_at  TIMESTAMPTZ,
  revoked_at    TIMESTAMPTZ
);
-- one active token per service: issuing a new one revokes the old (tech §5, decision 3)
CREATE UNIQUE INDEX service_tokens_active ON service_tokens (service_id) WHERE revoked_at IS NULL;

CREATE TABLE ci_checks (
  target        TEXT PRIMARY KEY,               -- 'spec' | 'service:<key>'
  repo          TEXT NOT NULL,
  status        TEXT NOT NULL CHECK (status IN ('none', 'proposed', 'on', 'outdated', 'manual')),
  version       TEXT,
  path          TEXT,
  pr_number     INTEGER,
  pr_url        TEXT,
  pr_kind       TEXT CHECK (pr_kind IN ('propose', 'bump')),
  pr_version    TEXT,                           -- the version the open PR sets
  error         TEXT,                           -- the last error of the provider
  checked_at    TIMESTAMPTZ NOT NULL DEFAULT now()
);
-- +goose StatementEnd

-- +goose Down
-- +goose StatementBegin
DROP TABLE ci_checks;
DROP TABLE service_tokens;
-- +goose StatementEnd
