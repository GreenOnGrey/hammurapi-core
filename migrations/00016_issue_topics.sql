-- FTR.HMR.DISC-0001 arch §4.1: the topic of a user in Nabu for an issue. The
-- link is by issue id, so it survives the move of the issue to another domain.
-- +goose Up
CREATE TABLE issue_topics (
  user_id         UUID NOT NULL REFERENCES users(id) ON DELETE CASCADE,
  issue_id        UUID NOT NULL REFERENCES issues(id) ON DELETE CASCADE,
  conversation_id UUID NOT NULL UNIQUE,
  created_at      TIMESTAMPTZ NOT NULL DEFAULT now(),
  PRIMARY KEY (user_id, issue_id)
);
CREATE INDEX issue_topics_issue ON issue_topics (issue_id);
-- +goose Down
DROP TABLE issue_topics;
