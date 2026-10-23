-- +goose Up
CREATE TABLE accounts (
    id text PRIMARY KEY,
    label text NOT NULL,
    company_id text NOT NULL DEFAULT '',
    enabled boolean NOT NULL DEFAULT true,
    owner_generation bigint NOT NULL DEFAULT 0,
    created_at timestamptz NOT NULL DEFAULT now(),
    CHECK (length(id) BETWEEN 1 AND 255)
);
CREATE UNIQUE INDEX accounts_company_unique_idx ON accounts (company_id)
    WHERE company_id <> '';

CREATE TABLE account_secrets (
    account_id text PRIMARY KEY REFERENCES accounts(id) ON DELETE CASCADE,
    ciphertext bytea NOT NULL
);

CREATE TABLE snapshots (
    account_id text PRIMARY KEY REFERENCES accounts(id) ON DELETE CASCADE,
    data jsonb NOT NULL,
    updated_at timestamptz NOT NULL DEFAULT now()
);

CREATE TABLE missions (
    account_id text NOT NULL REFERENCES accounts(id) ON DELETE CASCADE,
    ship_id text NOT NULL,
    phase text NOT NULL,
    destination text NOT NULL DEFAULT '',
    commodity text NOT NULL DEFAULT '',
    contract_id text NOT NULL DEFAULT '',
    quantity bigint NOT NULL DEFAULT 0,
    price_pence bigint NOT NULL DEFAULT 0,
    generation text NOT NULL DEFAULT '',
    data jsonb NOT NULL DEFAULT 'null'::jsonb,
    updated_at timestamptz NOT NULL DEFAULT now(),
    PRIMARY KEY (account_id, ship_id)
);

CREATE TABLE actions (
    id text PRIMARY KEY,
    account_id text NOT NULL REFERENCES accounts(id) ON DELETE CASCADE,
    company_id text NOT NULL DEFAULT '',
    method text NOT NULL,
    path text NOT NULL,
    body_cipher bytea NOT NULL,
    state text NOT NULL DEFAULT 'pending',
    status integer NOT NULL DEFAULT 0,
    response_cipher bytea,
    created_at timestamptz NOT NULL DEFAULT now(),
    updated_at timestamptz NOT NULL DEFAULT now(),
    CHECK (length(id) BETWEEN 1 AND 255),
    CHECK (
        (state = 'pending' AND status = 0 AND response_cipher IS NULL)
        OR (state = 'complete' AND ((status >= 200 AND status < 300)
            OR (status >= 400 AND status < 500)) AND response_cipher IS NOT NULL)
        OR (state = 'reconciled' AND status = 0 AND response_cipher IS NOT NULL)
    )
);
CREATE INDEX actions_pending_account_idx ON actions (account_id, created_at)
    WHERE state = 'pending';

-- The request fingerprint must survive every process restart and response update.
-- +goose StatementBegin
CREATE FUNCTION guard_action_immutability() RETURNS trigger LANGUAGE plpgsql AS $$
BEGIN
    IF ROW(NEW.id, NEW.account_id, NEW.company_id, NEW.method, NEW.path,
           NEW.body_cipher, NEW.created_at)
       IS DISTINCT FROM
       ROW(OLD.id, OLD.account_id, OLD.company_id, OLD.method, OLD.path,
           OLD.body_cipher, OLD.created_at) THEN
        RAISE EXCEPTION 'action request is immutable';
    END IF;
    IF OLD.state <> 'pending' AND
       ROW(NEW.state, NEW.status, NEW.response_cipher)
       IS DISTINCT FROM ROW(OLD.state, OLD.status, OLD.response_cipher) THEN
        RAISE EXCEPTION 'action result is immutable';
    END IF;
    RETURN NEW;
END;
$$;
-- +goose StatementEnd
CREATE TRIGGER actions_immutable BEFORE UPDATE ON actions
    FOR EACH ROW EXECUTE FUNCTION guard_action_immutability();

CREATE TABLE requests (
    id bigserial PRIMARY KEY,
    account_id text NOT NULL REFERENCES accounts(id) ON DELETE CASCADE,
    method text NOT NULL,
    path text NOT NULL,
    status integer NOT NULL,
    duration_ms bigint NOT NULL,
    request jsonb NOT NULL,
    response jsonb NOT NULL,
    error text NOT NULL DEFAULT '',
    created_at timestamptz NOT NULL DEFAULT now(),
    CHECK (status = 0 OR status BETWEEN 100 AND 599),
    CHECK (duration_ms >= 0)
);
CREATE INDEX requests_account_created_idx ON requests (account_id, created_at DESC, id DESC);
CREATE INDEX requests_retention_idx ON requests (created_at);

CREATE TABLE events (
    id bigserial PRIMARY KEY,
    account_id text NOT NULL REFERENCES accounts(id) ON DELETE CASCADE,
    event_id text NOT NULL,
    event_type text NOT NULL,
    payload jsonb NOT NULL,
    created_at timestamptz NOT NULL DEFAULT now(),
    UNIQUE (account_id, event_id)
);
CREATE INDEX events_account_created_idx ON events (account_id, created_at DESC, id DESC);
CREATE INDEX events_retention_idx ON events (created_at);

CREATE TABLE event_cursors (
    account_id text PRIMARY KEY REFERENCES accounts(id) ON DELETE CASCADE,
    event_id text NOT NULL,
    event_sequence bigint NOT NULL DEFAULT 0,
    updated_at timestamptz NOT NULL DEFAULT now()
);

CREATE TABLE decisions (
    id bigserial PRIMARY KEY,
    account_id text NOT NULL REFERENCES accounts(id) ON DELETE CASCADE,
    ship_id text NOT NULL DEFAULT '',
    kind text NOT NULL,
    reason text NOT NULL,
    details jsonb NOT NULL,
    created_at timestamptz NOT NULL DEFAULT now()
);
CREATE INDEX decisions_account_created_idx ON decisions (account_id, created_at DESC, id DESC);
CREATE INDEX decisions_retention_idx ON decisions (created_at);

CREATE TABLE metrics (
    id bigserial PRIMARY KEY,
    account_id text NOT NULL REFERENCES accounts(id) ON DELETE CASCADE,
    name text NOT NULL,
    value double precision NOT NULL,
    recorded_at timestamptz NOT NULL DEFAULT now(),
    CHECK (value NOT IN ('NaN'::float8, 'Infinity'::float8, '-Infinity'::float8))
);
CREATE INDEX metrics_account_recorded_idx ON metrics (account_id, recorded_at DESC, id DESC);
CREATE INDEX metrics_latest_idx ON metrics (account_id, name, recorded_at DESC, id DESC);
CREATE INDEX metrics_recorded_idx ON metrics (recorded_at DESC, id DESC);

CREATE TABLE metric_counters (
    account_id text NOT NULL REFERENCES accounts(id) ON DELETE CASCADE,
    name text NOT NULL,
    value double precision NOT NULL,
    recorded_at timestamptz NOT NULL DEFAULT now(),
    PRIMARY KEY (account_id, name)
);

CREATE TABLE settlements (
    id text PRIMARY KEY,
    account_id text NOT NULL REFERENCES accounts(id) ON DELETE CASCADE,
    ship_id text NOT NULL,
    mission_id text NOT NULL,
    lane text NOT NULL,
    action_id text NOT NULL UNIQUE REFERENCES actions(id),
    kind text NOT NULL,
    origin text NOT NULL,
    destination text NOT NULL,
    commodity text NOT NULL,
    quantity_tons bigint NOT NULL,
    game_seconds bigint NOT NULL,
    costs_known boolean NOT NULL,
    acquisition_pence bigint NOT NULL,
    proceeds_pence bigint NOT NULL,
    fees_pence bigint NOT NULL,
    provisions_pence bigint NOT NULL,
    expenses_pence bigint NOT NULL,
    started_at timestamptz NOT NULL,
    completed_at timestamptz NOT NULL,
    created_at timestamptz NOT NULL DEFAULT now(),
    CHECK (quantity_tons >= 0 AND game_seconds >= 0),
    CHECK (acquisition_pence >= 0 AND proceeds_pence >= 0 AND fees_pence >= 0
        AND provisions_pence >= 0 AND expenses_pence >= 0)
);
CREATE INDEX settlements_account_completed_idx ON settlements (account_id, completed_at DESC, id);
CREATE INDEX settlements_lane_idx ON settlements (account_id, lane, completed_at DESC, id);
CREATE INDEX settlements_completed_idx ON settlements (completed_at DESC, id);

-- +goose Down
DROP TABLE settlements;
DROP TABLE metric_counters;
DROP TABLE metrics;
DROP TABLE decisions;
DROP TABLE event_cursors;
DROP TABLE events;
DROP TABLE requests;
DROP TRIGGER actions_immutable ON actions;
DROP FUNCTION guard_action_immutability();
DROP TABLE actions;
DROP TABLE missions;
DROP TABLE snapshots;
DROP TABLE account_secrets;
DROP TABLE accounts;
