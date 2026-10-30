-- +goose Up
-- Pilot reservations and outcomes are recovery/accounting state, not diagnostics.
CREATE TABLE futures_intents (
    account_id text NOT NULL REFERENCES accounts(id) ON DELETE CASCADE,
    intent_id text NOT NULL,
    action_id text NOT NULL,
    cohort_id text NOT NULL,
    port_id text NOT NULL CHECK (port_id = 'amsterdam'),
    commodity_id text NOT NULL CHECK (commodity_id = 'water_beer'),
    side text NOT NULL CHECK (side IN ('ask', 'bid')),
    lot_tons bigint NOT NULL CHECK (lot_tons = 1),
    strike_price_pence bigint NOT NULL CHECK (strike_price_pence > 0),
    reserved_tons bigint NOT NULL CHECK (reserved_tons >= 0),
    reserved_cash_pence bigint NOT NULL CHECK (reserved_cash_pence >= 0),
    margin_pence bigint NOT NULL CHECK (margin_pence >= 0),
    exposure_pence bigint NOT NULL CHECK (exposure_pence > 0 AND exposure_pence <= 30000),
    status text NOT NULL CHECK (status IN ('intent', 'open', 'active', 'settled', 'defaulted', 'cancelled', 'rejected')),
    matures_at timestamptz NOT NULL,
    created_at timestamptz NOT NULL,
    futures_id text NOT NULL DEFAULT '',
    observed_matures_at timestamptz,
    contract jsonb,
    outcome jsonb,
    realized_pnl_pence bigint,
    cash_delta_pence bigint,
    pnl_basis text NOT NULL DEFAULT '',
    updated_at timestamptz NOT NULL,
    PRIMARY KEY (account_id, intent_id),
    UNIQUE (account_id, action_id)
);
CREATE UNIQUE INDEX futures_intents_identity_idx ON futures_intents (account_id, futures_id) WHERE futures_id <> '';

CREATE TABLE futures_cash_flows (
    account_id text NOT NULL,
    intent_id text NOT NULL,
    ledger_id text NOT NULL,
    cash_delta_pence bigint NOT NULL,
    evidence jsonb NOT NULL,
    PRIMARY KEY (account_id, ledger_id),
    FOREIGN KEY (account_id, intent_id) REFERENCES futures_intents(account_id, intent_id) ON DELETE CASCADE
);

CREATE TABLE futures_credit_evidence (
    account_id text PRIMARY KEY REFERENCES accounts(id) ON DELETE CASCADE,
    last_attempt_at timestamptz NOT NULL,
    attempted_server_at timestamptz NOT NULL,
    observed_at timestamptz,
    claims_open_pence bigint CHECK (claims_open_pence >= 0 AND claims_open_pence <= 1000000000000),
    CHECK ((observed_at IS NULL) = (claims_open_pence IS NULL))
);

-- +goose Down
DROP TABLE futures_credit_evidence;
DROP TABLE futures_cash_flows;
DROP TABLE futures_intents;
