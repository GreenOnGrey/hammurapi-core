-- FTR.HMR.DISC-0001 tech §5.6, §3.5, §4.2 (R26–R28): the open questions of
-- the researcher with the last number and round given out; the answers and
-- the context of a brief; answers from the page of the issue come without a
-- topic.
-- +goose Up
ALTER TABLE discovery_docs      ADD COLUMN questions JSONB NOT NULL DEFAULT '[]', ADD COLUMN question_seq INTEGER NOT NULL DEFAULT 0,
                                ADD COLUMN question_round INTEGER NOT NULL DEFAULT 0;
ALTER TABLE discovery_revisions ADD COLUMN questions JSONB NOT NULL DEFAULT '[]', ADD COLUMN question_seq INTEGER NOT NULL DEFAULT 0,
                                ADD COLUMN question_round INTEGER NOT NULL DEFAULT 0;
ALTER TABLE discovery_requests
  ADD COLUMN answers JSONB NOT NULL DEFAULT '[]', ADD COLUMN context TEXT,
  ALTER COLUMN conversation_id DROP NOT NULL,
  DROP CONSTRAINT discovery_requests_instructions_check,
  ADD CONSTRAINT discovery_requests_instructions_check CHECK (char_length(instructions) <= 4000),
  ADD CONSTRAINT discovery_requests_context_check CHECK (context IS NULL OR char_length(context) <= 4000),
  ADD CONSTRAINT discovery_requests_brief_check CHECK (instructions <> '' OR jsonb_array_length(answers) > 0);
-- +goose Down
-- The old schema holds neither instructions without a topic nor ones of
-- answers only: the answers of the page of the issue are lost (tech §6).
DELETE FROM discovery_requests WHERE conversation_id IS NULL OR instructions = '';
ALTER TABLE discovery_requests
  DROP CONSTRAINT discovery_requests_brief_check, DROP CONSTRAINT discovery_requests_context_check,
  DROP CONSTRAINT discovery_requests_instructions_check,
  ADD CONSTRAINT discovery_requests_instructions_check CHECK (char_length(instructions) BETWEEN 1 AND 4000),
  ALTER COLUMN conversation_id SET NOT NULL,
  DROP COLUMN context, DROP COLUMN answers;
ALTER TABLE discovery_revisions DROP COLUMN question_round, DROP COLUMN question_seq, DROP COLUMN questions;
ALTER TABLE discovery_docs      DROP COLUMN question_round, DROP COLUMN question_seq, DROP COLUMN questions;
