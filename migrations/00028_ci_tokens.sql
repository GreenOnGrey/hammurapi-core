-- FTR.HMR.SPEC-0003 tech §3.3: the installation token for CI without OIDC. One
-- active token; the OIDC token of a CI job needs no row. service_tokens stays
-- (migrations only expand) but is no longer read.

-- +goose Up
-- +goose StatementBegin
CREATE TABLE ci_tokens (
  id            UUID PRIMARY KEY DEFAULT gen_random_uuid(),
  token_hash    BYTEA NOT NULL UNIQUE,          -- sha256 of the token; the token is shown once
  prefix        TEXT NOT NULL,
  created_by    UUID NOT NULL REFERENCES users(id),
  created_at    TIMESTAMPTZ NOT NULL DEFAULT now(),
  last_used_at  TIMESTAMPTZ,
  revoked_at    TIMESTAMPTZ
);
CREATE UNIQUE INDEX ci_tokens_active ON ci_tokens ((true)) WHERE revoked_at IS NULL;
-- +goose StatementEnd

-- +goose Down
-- +goose StatementBegin
DROP TABLE ci_tokens;
-- +goose StatementEnd
