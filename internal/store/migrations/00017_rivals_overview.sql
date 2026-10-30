-- +goose Up
CREATE TABLE rising_publications (
    account_id text NOT NULL REFERENCES accounts(id) ON DELETE CASCADE,
    season_number integer NOT NULL CHECK (season_number > 0),
    refreshed_at timestamptz NOT NULL,
    next_refresh_at timestamptz,
    board jsonb NOT NULL,
    observed_at timestamptz NOT NULL,
    PRIMARY KEY (account_id, season_number, refreshed_at)
);
CREATE INDEX rising_publications_latest_idx ON rising_publications (season_number DESC, refreshed_at DESC);

CREATE TABLE rival_companies (
    season_number integer NOT NULL CHECK (season_number > 0),
    company_id text NOT NULL,
    company_name text NOT NULL DEFAULT '',
    observed_at timestamptz NOT NULL,
    PRIMARY KEY (season_number, company_id)
);
CREATE INDEX rival_companies_name_idx ON rival_companies (season_number, company_name);

CREATE TABLE rival_credit_reports (
    season_number integer NOT NULL CHECK (season_number > 0),
    company_id text NOT NULL,
    observed_at timestamptz NOT NULL,
    report jsonb NOT NULL,
    PRIMARY KEY (season_number, company_id, observed_at)
);

-- Reservations are committed before sending. A crash or a failed request still
-- consumes the real-time allowance; this history is not diagnostic retention.
CREATE TABLE rival_credit_attempts (
    id bigserial PRIMARY KEY,
    account_id text NOT NULL REFERENCES accounts(id) ON DELETE CASCADE,
    company_id text NOT NULL,
    attempted_at timestamptz NOT NULL,
    recorded_at timestamptz
);
CREATE INDEX rival_credit_attempts_hour_idx ON rival_credit_attempts (attempted_at DESC);
CREATE INDEX rival_credit_attempts_company_idx ON rival_credit_attempts (company_id, attempted_at DESC);

CREATE TABLE configured_company_identities (
    company_id text PRIMARY KEY,
    name text NOT NULL,
    observed_at timestamptz NOT NULL
);

-- +goose Down
DROP TABLE configured_company_identities;
DROP TABLE rival_credit_attempts;
DROP TABLE rival_credit_reports;
DROP TABLE rival_companies;
DROP TABLE rising_publications;
