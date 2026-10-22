-- FTR.HMR.DISC-0001 arch §4.3: the instructions experts give the researcher
-- through their personal agents; a run of Discovery takes every queued one.
-- +goose Up
CREATE TABLE discovery_requests (
  id              UUID PRIMARY KEY DEFAULT gen_random_uuid(),
  issue_id        UUID NOT NULL REFERENCES issues(id) ON DELETE CASCADE,
  author_id       UUID NOT NULL REFERENCES users(id),
  conversation_id UUID NOT NULL,
  instructions    TEXT NOT NULL CHECK (char_length(instructions) BETWEEN 1 AND 4000),
  state           TEXT NOT NULL DEFAULT 'queued' CHECK (state IN ('queued','running','done','failed')),
  run_id          UUID REFERENCES workflow_runs(id),
  revision        INTEGER,
  error           TEXT,
  created_at      TIMESTAMPTZ NOT NULL DEFAULT now(),
  finished_at     TIMESTAMPTZ
);
CREATE INDEX discovery_requests_issue_state ON discovery_requests (issue_id, state, created_at);
CREATE INDEX discovery_requests_run ON discovery_requests (run_id);
-- +goose Down
DROP TABLE discovery_requests;
