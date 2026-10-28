-- +goose Up
ALTER TABLE settlements
    ADD COLUMN proceeds_unknown boolean NOT NULL DEFAULT false,
    DROP CONSTRAINT settlements_evidence_anchor_check,
    ADD CONSTRAINT settlements_evidence_anchor_check CHECK (
        (kind = 'passage' AND (
            (NOT proceeds_unknown AND event_id IS NOT NULL AND length(event_id) BETWEEN 1 AND 255)
            OR (proceeds_unknown AND event_id IS NULL AND proceeds_pence = 0 AND NOT costs_known)
        ))
        OR (kind <> 'passage' AND action_id IS NOT NULL AND event_id IS NULL AND NOT proceeds_unknown)
    );

CREATE TABLE passenger_receipts (
    id text PRIMARY KEY CHECK (length(id) BETWEEN 1 AND 255),
    account_id text NOT NULL REFERENCES accounts(id) ON DELETE CASCADE,
    context_cipher bytea NOT NULL CHECK (octet_length(context_cipher) >= 28),
    resolved boolean NOT NULL DEFAULT false,
    created_at timestamptz NOT NULL DEFAULT now()
);
CREATE INDEX passenger_receipts_pending_idx ON passenger_receipts (account_id, created_at, id) WHERE NOT resolved;
CREATE TABLE passenger_receipt_events (
    account_id text NOT NULL REFERENCES accounts(id) ON DELETE CASCADE,
    event_id text NOT NULL,
    settlement_id text NOT NULL REFERENCES settlements(id) ON DELETE CASCADE,
    PRIMARY KEY (account_id, event_id),
    FOREIGN KEY (account_id, event_id) REFERENCES events(account_id, event_id)
);
CREATE INDEX passenger_receipt_events_settlement_idx ON passenger_receipt_events (settlement_id);

-- +goose StatementBegin
CREATE FUNCTION guard_passenger_receipt_immutability() RETURNS trigger LANGUAGE plpgsql AS $$
BEGIN
    IF ROW(NEW.id, NEW.account_id, NEW.context_cipher, NEW.created_at)
       IS DISTINCT FROM ROW(OLD.id, OLD.account_id, OLD.context_cipher, OLD.created_at)
       OR OLD.resolved AND NOT NEW.resolved THEN
        RAISE EXCEPTION 'passenger receipt context is immutable';
    END IF;
    RETURN NEW;
END;
$$;
-- +goose StatementEnd
CREATE TRIGGER passenger_receipts_immutable BEFORE UPDATE ON passenger_receipts
    FOR EACH ROW EXECUTE FUNCTION guard_passenger_receipt_immutability();

-- +goose Down
-- +goose StatementBegin
DO $$
BEGIN
    IF EXISTS (SELECT 1 FROM passenger_receipts) THEN
        RAISE EXCEPTION 'archived passenger receipts require restoring a pre-migration backup to roll back';
    END IF;
END;
$$;
-- +goose StatementEnd
DROP TRIGGER passenger_receipts_immutable ON passenger_receipts;
DROP FUNCTION guard_passenger_receipt_immutability();
DROP TABLE passenger_receipt_events;
DROP TABLE passenger_receipts;
ALTER TABLE settlements
    DROP CONSTRAINT settlements_evidence_anchor_check,
    DROP COLUMN proceeds_unknown,
    ADD CONSTRAINT settlements_evidence_anchor_check CHECK (
        (kind = 'passage' AND event_id IS NOT NULL AND length(event_id) BETWEEN 1 AND 255)
        OR (kind <> 'passage' AND action_id IS NOT NULL AND event_id IS NULL)
    );
