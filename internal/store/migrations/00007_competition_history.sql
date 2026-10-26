-- +goose Up
CREATE INDEX events_competition_history_idx ON events (account_id, created_at DESC, id DESC)
    WHERE event_type IN ('ship_sailed', 'ship_entered_port');

-- +goose Down
DROP INDEX events_competition_history_idx;
