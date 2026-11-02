-- FTR.HMR.DEV-0001 tech §4: the writer of specifications: the stages of a
-- feature, its topics in the chat, the questions by gate and the instructions.
-- +goose Up
ALTER TABLE features
  ADD COLUMN spec_flow     TEXT   NOT NULL DEFAULT 'legacy' CHECK (spec_flow IN ('writer','legacy')),
  ADD COLUMN spec_stage    TEXT   CHECK (spec_stage IN ('product','design','arch','generated')),
  ADD COLUMN skipped_areas TEXT[] NOT NULL DEFAULT '{}' CHECK (skipped_areas <@ ARRAY['design','arch']::TEXT[]);

CREATE TABLE feature_topics (
  user_id         UUID NOT NULL REFERENCES users(id) ON DELETE CASCADE,
  feature_id      UUID NOT NULL REFERENCES features(id) ON DELETE CASCADE,
  conversation_id UUID NOT NULL,
  created_at      TIMESTAMPTZ NOT NULL DEFAULT now(),
  PRIMARY KEY (user_id, feature_id)
);
CREATE INDEX feature_topics_feature ON feature_topics (feature_id);
CREATE INDEX feature_topics_conversation ON feature_topics (conversation_id);

CREATE TABLE spec_gate_questions (
  feature_id     UUID NOT NULL REFERENCES features(id) ON DELETE CASCADE,
  area           area NOT NULL,
  questions      JSONB   NOT NULL DEFAULT '[]',
  question_seq   INTEGER NOT NULL DEFAULT 0,
  question_round INTEGER NOT NULL DEFAULT 0,
  updated_at     TIMESTAMPTZ NOT NULL DEFAULT now(),
  PRIMARY KEY (feature_id, area)
);

CREATE TABLE spec_requests (
  id              UUID PRIMARY KEY DEFAULT gen_random_uuid(),
  feature_id      UUID NOT NULL REFERENCES features(id) ON DELETE CASCADE,
  author_id       UUID NOT NULL REFERENCES users(id),
  conversation_id UUID,
  areas           TEXT[] NOT NULL DEFAULT '{}',
  skip_areas      TEXT[] NOT NULL DEFAULT '{}',
  restore_areas   TEXT[] NOT NULL DEFAULT '{}',
  instructions    TEXT NOT NULL DEFAULT '' CHECK (char_length(instructions) <= 4000),
  context         TEXT CHECK (context IS NULL OR char_length(context) <= 4000),
  answers         JSONB NOT NULL DEFAULT '[]',
  state           TEXT NOT NULL DEFAULT 'queued' CHECK (state IN ('queued','running','done','failed')),
  run_id          UUID REFERENCES workflow_runs(id),
  commits         JSONB NOT NULL DEFAULT '{}',
  error           TEXT,
  created_at      TIMESTAMPTZ NOT NULL DEFAULT now(),
  finished_at     TIMESTAMPTZ,
  CONSTRAINT spec_requests_brief_check CHECK (instructions <> '' OR jsonb_array_length(answers) > 0
    OR cardinality(areas) > 0 OR cardinality(skip_areas) > 0 OR cardinality(restore_areas) > 0)
);
CREATE INDEX spec_requests_feature_state ON spec_requests (feature_id, state, created_at);
CREATE INDEX spec_requests_run ON spec_requests (run_id);

-- +goose Down
DROP TABLE spec_requests;
DROP TABLE spec_gate_questions;
DROP TABLE feature_topics;
ALTER TABLE features DROP COLUMN skipped_areas, DROP COLUMN spec_stage, DROP COLUMN spec_flow;
