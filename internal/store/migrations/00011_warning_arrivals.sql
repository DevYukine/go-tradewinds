-- Only retained arrival/departure envelopes are needed for dock-clock fallback.
-- Deploys stop the previous bot before migrating. A transactional build is
-- atomic: it never leaves an invalid index behind.
-- +goose Up
CREATE INDEX IF NOT EXISTS events_warning_arrivals_idx ON events (account_id, created_at DESC, id DESC)
    WHERE event_type IN ('ship_arrived', 'ship_departed');

-- +goose Down
DROP INDEX IF EXISTS events_warning_arrivals_idx;
