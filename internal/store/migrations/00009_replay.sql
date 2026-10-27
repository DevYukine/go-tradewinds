-- +goose Up
CREATE TABLE replay_payloads (
    account_id text NOT NULL REFERENCES accounts(id) ON DELETE CASCADE,
    digest text NOT NULL,
    payload_cipher bytea NOT NULL,
    stored_bytes bigint NOT NULL,
    last_seen_at timestamptz NOT NULL,
    PRIMARY KEY (account_id, digest)
);
CREATE INDEX replay_payloads_last_seen ON replay_payloads(last_seen_at);

CREATE TABLE replay_frames (
    id bigserial PRIMARY KEY,
    capture_id text NOT NULL UNIQUE,
    account_id text NOT NULL REFERENCES accounts(id) ON DELETE CASCADE,
    generation text NOT NULL,
    source text NOT NULL,
    observed_at timestamptz NOT NULL,
    clock jsonb,
    phase integer NOT NULL,
    catalog_digest text NOT NULL,
    decision boolean NOT NULL,
    assumed_catalog boolean NOT NULL,
    manifest_cipher bytea NOT NULL,
    stored_bytes bigint NOT NULL
);
CREATE INDEX replay_frames_account_time ON replay_frames(account_id, observed_at, id);
CREATE INDEX replay_frames_time ON replay_frames(observed_at, id);
CREATE UNIQUE INDEX replay_frames_diagnostic_generation ON replay_frames(account_id, generation)
    WHERE source = 'diagnostic_backfill';

CREATE TABLE replay_plans (
    id bigserial PRIMARY KEY,
    account_id text NOT NULL REFERENCES accounts(id) ON DELETE CASCADE,
    mission_id text NOT NULL,
    generation text NOT NULL,
    sequence bigint NOT NULL,
    observed_at timestamptz NOT NULL,
    digest text NOT NULL,
    plan_cipher bytea NOT NULL,
    UNIQUE (account_id, mission_id, digest)
);
CREATE INDEX replay_plans_account_time ON replay_plans(account_id, observed_at, id);

CREATE TABLE replay_action_links (
    account_id text NOT NULL REFERENCES accounts(id) ON DELETE CASCADE,
    action_id text NOT NULL,
    mission_id text NOT NULL,
    stage text NOT NULL,
    sequence bigint NOT NULL,
    generation text NOT NULL,
    PRIMARY KEY (account_id, action_id)
);

-- +goose Down
DROP TABLE replay_action_links;
DROP TABLE replay_plans;
DROP TABLE replay_frames;
DROP TABLE replay_payloads;
