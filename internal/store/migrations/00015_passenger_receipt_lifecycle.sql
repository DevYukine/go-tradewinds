-- +goose Up
ALTER TABLE passenger_receipts
    ADD COLUMN next_scan_at timestamptz,
    ADD COLUMN expires_at timestamptz,
    ADD COLUMN closed_at timestamptz,
    ADD COLUMN close_reason text NOT NULL DEFAULT '';
UPDATE passenger_receipts SET next_scan_at = created_at, expires_at = created_at + INTERVAL '168 hours';
ALTER TABLE passenger_receipts
    ALTER COLUMN next_scan_at SET NOT NULL,
    ALTER COLUMN expires_at SET NOT NULL,
    ADD CONSTRAINT passenger_receipts_expiry_check CHECK (expires_at = created_at + INTERVAL '168 hours'),
    ADD CONSTRAINT passenger_receipts_scan_check CHECK (next_scan_at >= created_at AND next_scan_at <= expires_at),
    ADD CONSTRAINT passenger_receipts_closed_check CHECK (
        (closed_at IS NULL AND close_reason = '') OR
        (closed_at IS NOT NULL AND close_reason IN ('expired', 'quarantined') AND NOT resolved)
    );
DROP INDEX passenger_receipts_pending_idx;
CREATE INDEX passenger_receipts_pending_idx ON passenger_receipts (account_id, next_scan_at, created_at, id)
    WHERE NOT resolved AND closed_at IS NULL;

-- +goose StatementBegin
CREATE OR REPLACE FUNCTION guard_passenger_receipt_immutability() RETURNS trigger LANGUAGE plpgsql AS $$
BEGIN
    IF ROW(NEW.id, NEW.account_id, NEW.context_cipher, NEW.created_at, NEW.expires_at)
       IS DISTINCT FROM ROW(OLD.id, OLD.account_id, OLD.context_cipher, OLD.created_at, OLD.expires_at)
       OR OLD.resolved AND NOT NEW.resolved
       OR NEW.next_scan_at < OLD.next_scan_at
       OR OLD.closed_at IS NOT NULL AND
          ROW(NEW.closed_at, NEW.close_reason) IS DISTINCT FROM ROW(OLD.closed_at, OLD.close_reason) THEN
        RAISE EXCEPTION 'passenger receipt context and closure are immutable';
    END IF;
    RETURN NEW;
END;
$$;
-- +goose StatementEnd

-- +goose Down
-- +goose StatementBegin
DO $$
BEGIN
    IF EXISTS (SELECT 1 FROM passenger_receipts) THEN
        RAISE EXCEPTION 'passenger receipt lifecycle requires restoring a pre-migration backup to roll back';
    END IF;
END;
$$;
CREATE OR REPLACE FUNCTION guard_passenger_receipt_immutability() RETURNS trigger LANGUAGE plpgsql AS $$
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
DROP INDEX passenger_receipts_pending_idx;
CREATE INDEX passenger_receipts_pending_idx ON passenger_receipts (account_id, created_at, id) WHERE NOT resolved;
ALTER TABLE passenger_receipts
    DROP COLUMN next_scan_at,
    DROP COLUMN expires_at,
    DROP COLUMN closed_at,
    DROP COLUMN close_reason;
