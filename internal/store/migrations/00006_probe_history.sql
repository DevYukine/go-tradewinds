-- +goose Up
CREATE INDEX settlements_probe_history_idx ON settlements (account_id, created_at, id) WHERE kind = 'relocation';
CREATE INDEX events_probe_departure_idx ON events (account_id, id) WHERE event_type = 'ship_departed';

-- +goose Down
DROP INDEX events_probe_departure_idx;
DROP INDEX settlements_probe_history_idx;
