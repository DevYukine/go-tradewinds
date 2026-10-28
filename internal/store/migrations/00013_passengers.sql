-- +goose Up
ALTER TABLE settlements
    ALTER COLUMN action_id DROP NOT NULL,
    DROP CONSTRAINT settlements_action_id_key,
    DROP CONSTRAINT settlements_kind_check,
    ADD CONSTRAINT settlements_kind_check CHECK (
        kind IN ('spot', 'charter', 'freight', 'spot+freight', 'charter+freight', 'recovery', 'passage', 'relocation', 'aborted', 'maintenance', 'unknown')
    ),
    ADD COLUMN event_id text,
    ADD CONSTRAINT settlements_evidence_anchor_check CHECK (
        (kind = 'passage' AND event_id IS NOT NULL AND length(event_id) BETWEEN 1 AND 255)
        OR (kind <> 'passage' AND action_id IS NOT NULL AND event_id IS NULL)
    );
CREATE UNIQUE INDEX settlements_action_evidence_idx ON settlements (action_id)
    WHERE action_id IS NOT NULL AND kind <> 'passage';
CREATE UNIQUE INDEX settlements_passenger_event_idx ON settlements (account_id, event_id)
    WHERE event_id IS NOT NULL;
CREATE INDEX events_passenger_history_idx ON events (account_id, created_at, id)
    WHERE event_type IN ('passengers_boarded', 'passengers_paid', 'passengers_lost');

CREATE TABLE passenger_evidence (
    id text PRIMARY KEY CHECK (length(id) BETWEEN 1 AND 255),
    account_id text NOT NULL REFERENCES accounts(id) ON DELETE CASCADE,
    lane_lookup text NOT NULL CHECK (length(lane_lookup) = 64),
    context_cipher bytea NOT NULL CHECK (octet_length(context_cipher) >= 28),
    fare_per_head_pence bigint NOT NULL CHECK (fare_per_head_pence > 0),
    outcome text NOT NULL CHECK (outcome IN ('boarded', 'refused')),
    heads bigint NOT NULL CHECK (heads > 0),
    game_hour double precision NOT NULL CHECK (game_hour >= 0 AND game_hour < 'Infinity'::float8),
    leeway_game_hours double precision CHECK (leeway_game_hours > '-Infinity'::float8 AND leeway_game_hours < 'Infinity'::float8),
    late_cut_per_game_day_pence bigint CHECK (late_cut_per_game_day_pence >= 0),
    created_at timestamptz NOT NULL DEFAULT now()
);
CREATE INDEX passenger_evidence_account_hour_idx ON passenger_evidence (account_id, game_hour DESC, created_at DESC, id);
CREATE INDEX passenger_evidence_lane_hour_idx ON passenger_evidence (account_id, lane_lookup, game_hour DESC, created_at DESC, id);

-- +goose StatementBegin
CREATE FUNCTION guard_passenger_evidence_immutability() RETURNS trigger LANGUAGE plpgsql AS $$
BEGIN
    IF NEW IS DISTINCT FROM OLD THEN
        RAISE EXCEPTION 'passenger evidence is immutable';
    END IF;
    RETURN NEW;
END;
$$;
-- +goose StatementEnd
CREATE TRIGGER passenger_evidence_immutable BEFORE UPDATE ON passenger_evidence
    FOR EACH ROW EXECUTE FUNCTION guard_passenger_evidence_immutability();

-- +goose Down
-- A passage has no substitute action anchor in the old schema.
-- +goose StatementBegin
DO $$
BEGIN
    IF EXISTS (SELECT 1 FROM settlements WHERE kind = 'passage') THEN
        RAISE EXCEPTION 'passage settlements require restoring a pre-migration backup to roll back';
    END IF;
END;
$$;
-- +goose StatementEnd
DROP TRIGGER passenger_evidence_immutable ON passenger_evidence;
DROP FUNCTION guard_passenger_evidence_immutability();
DROP TABLE passenger_evidence;
DROP INDEX events_passenger_history_idx;
DROP INDEX settlements_passenger_event_idx;
DROP INDEX settlements_action_evidence_idx;
ALTER TABLE settlements
    DROP CONSTRAINT settlements_evidence_anchor_check,
    DROP CONSTRAINT settlements_kind_check,
    ADD CONSTRAINT settlements_kind_check CHECK (
        kind IN ('spot', 'charter', 'freight', 'spot+freight', 'charter+freight', 'recovery', 'relocation', 'aborted', 'maintenance', 'unknown')
    ),
    DROP COLUMN event_id,
    ALTER COLUMN action_id SET NOT NULL,
    ADD CONSTRAINT settlements_action_id_key UNIQUE (action_id);
