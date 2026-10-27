-- The events table holds about seven days of SSE envelopes (thousands of
-- rows), so a transactional build holds its write lock for milliseconds.
-- Deploys stop the previous bot before migrating. A transactional build is
-- atomic: it never leaves an invalid index behind.
-- +goose Up
CREATE INDEX IF NOT EXISTS events_rival_evidence_idx ON events (account_id, created_at DESC, id DESC)
    WHERE event_type IN ('monopoly_granted', 'loan_request_posted');

-- +goose Down
DROP INDEX IF EXISTS events_rival_evidence_idx;
