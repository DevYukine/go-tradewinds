-- +goose Up
CREATE TABLE underworld_visits (
    account_id text NOT NULL REFERENCES accounts(id) ON DELETE CASCADE,
    ship_id text NOT NULL,
    port_id text NOT NULL,
    visit_id text NOT NULL,
    PRIMARY KEY (account_id, ship_id)
);

-- A reservation consumes its allowance even if the HTTP read fails or the
-- process dies before recording the response. These are not raw diagnostics.
CREATE TABLE underworld_read_attempts (
    account_id text NOT NULL REFERENCES accounts(id) ON DELETE CASCADE,
    ship_id text NOT NULL,
    visit_id text NOT NULL,
    kind text NOT NULL CHECK (kind IN ('wants', 'book')),
    commodity_id text NOT NULL,
    attempted_at timestamptz NOT NULL,
    PRIMARY KEY (account_id, ship_id, visit_id, kind, commodity_id),
    CHECK ((kind = 'wants' AND commodity_id = '') OR (kind = 'book' AND commodity_id <> ''))
);

CREATE TABLE underworld_observations (
    account_id text NOT NULL REFERENCES accounts(id) ON DELETE CASCADE,
    ship_id text NOT NULL,
    visit_id text NOT NULL,
    port_id text NOT NULL,
    kind text NOT NULL CHECK (kind IN ('wants', 'book')),
    commodity_id text NOT NULL,
    observed_at timestamptz NOT NULL,
    payload jsonb NOT NULL,
    PRIMARY KEY (account_id, ship_id, visit_id, kind, commodity_id),
    CHECK ((kind = 'wants' AND commodity_id = '') OR (kind = 'book' AND commodity_id <> ''))
);
CREATE INDEX underworld_observations_port_idx ON underworld_observations (account_id, port_id, observed_at DESC);

-- +goose Down
DROP TABLE underworld_observations;
DROP TABLE underworld_read_attempts;
DROP TABLE underworld_visits;
