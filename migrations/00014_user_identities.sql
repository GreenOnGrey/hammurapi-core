-- FTR.HMR.CMN-0006 arch §9 "+1": sign-in through a provider (OIDC or GitHub),
-- the git account as a linked account, the email as the key to Nabu.
-- The token of the git account stays in user_git_tokens (deviation recorded in
-- the tech spec): git_accounts holds the account and its verified emails.
-- +goose Up
CREATE TABLE user_identities (
  issuer TEXT NOT NULL, subject TEXT NOT NULL,
  user_id UUID NOT NULL REFERENCES users(id) ON DELETE CASCADE,
  created_at TIMESTAMPTZ NOT NULL DEFAULT now(),
  PRIMARY KEY (issuer, subject)
);
CREATE INDEX ON user_identities (user_id);
CREATE TABLE git_accounts (
  user_id UUID PRIMARY KEY REFERENCES users(id) ON DELETE CASCADE,
  provider TEXT NOT NULL, external_id TEXT NOT NULL, login TEXT NOT NULL,
  emails TEXT[] NOT NULL DEFAULT '{}', emails_checked_at TIMESTAMPTZ,
  linked_at TIMESTAMPTZ NOT NULL DEFAULT now(),
  UNIQUE (provider, external_id)
);
ALTER TABLE users
  ADD COLUMN IF NOT EXISTS email TEXT,
  ADD COLUMN IF NOT EXISTS created_via TEXT NOT NULL DEFAULT 'login',
  ADD COLUMN IF NOT EXISTS link_review BOOLEAN NOT NULL DEFAULT false;
CREATE UNIQUE INDEX IF NOT EXISTS users_email_unique ON users (lower(email)) WHERE email IS NOT NULL;
-- Every existing user signed in through the git provider: the git account is
-- the current identity (the provider is set by api at start: '' means "the
-- git provider of the instance").
INSERT INTO git_accounts (user_id, provider, external_id, login)
  SELECT id, '', provider_uid, username FROM users
  ON CONFLICT DO NOTHING;
-- +goose Down
DROP TABLE git_accounts;
DROP TABLE user_identities;
DROP INDEX IF EXISTS users_email_unique;
ALTER TABLE users DROP COLUMN IF EXISTS link_review, DROP COLUMN IF EXISTS created_via, DROP COLUMN IF EXISTS email;
