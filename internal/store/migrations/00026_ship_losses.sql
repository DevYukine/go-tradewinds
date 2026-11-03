-- +goose Up
CREATE TABLE ship_losses (
    account_id text NOT NULL REFERENCES accounts(id) ON DELETE CASCADE,
    ship_id text NOT NULL CHECK (length(ship_id) BETWEEN 1 AND 255),
    ship_name text NOT NULL DEFAULT '',
    ship_class text NOT NULL DEFAULT '',
    fate text NOT NULL CHECK (length(fate) BETWEEN 1 AND 255),
    status text NOT NULL CHECK (length(status) BETWEEN 1 AND 255),
    observed_at timestamptz NOT NULL,
    recovered_at timestamptz CHECK (recovered_at IS NULL OR (fate = 'missing' AND recovered_at > observed_at)),
    last_observed_at timestamptz,
    last_port_id text,
    last_sea_zone_id text,
    voyage_origin_id text,
    voyage_destination_id text,
    cargo_manifest jsonb CHECK (jsonb_typeof(cargo_manifest) = 'object'),
    cargo_unavailable_reason text NOT NULL DEFAULT '',
    stores jsonb CHECK (jsonb_typeof(stores) = 'object'),
    stores_unavailable_reason text NOT NULL DEFAULT '',
    hazard_cause text,
    hazard_unavailable_reason text NOT NULL DEFAULT '',
    insurance_claim_paid_pence bigint CHECK (insurance_claim_paid_pence >= 0),
    PRIMARY KEY (account_id, ship_id)
);
CREATE INDEX ship_losses_account_observed_idx ON ship_losses (account_id, observed_at, ship_id);
CREATE INDEX events_ship_loss_account_id_idx ON events (account_id, id)
    WHERE event_type IN ('voyage_hazard', 'insurance_claim_paid', 'insurance_policy_closed');

-- +goose Down
DROP INDEX events_ship_loss_account_id_idx;
DROP TABLE ship_losses;
