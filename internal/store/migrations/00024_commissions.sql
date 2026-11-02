-- +goose Up
ALTER TABLE settlements
    DROP CONSTRAINT settlements_kind_check,
    ADD CONSTRAINT settlements_kind_check CHECK (
        kind IN ('spot', 'charter', 'freight', 'spot+freight', 'charter+freight', 'recovery', 'passage', 'commission', 'relocation', 'aborted', 'maintenance', 'unknown')
    );

-- +goose Down
-- +goose StatementBegin
DO $$
BEGIN
    IF EXISTS (SELECT 1 FROM settlements WHERE kind = 'commission') THEN
        RAISE EXCEPTION 'commission settlements require restoring a pre-migration backup to roll back';
    END IF;
END;
$$;
-- +goose StatementEnd
ALTER TABLE settlements
    DROP CONSTRAINT settlements_kind_check,
    ADD CONSTRAINT settlements_kind_check CHECK (
        kind IN ('spot', 'charter', 'freight', 'spot+freight', 'charter+freight', 'recovery', 'passage', 'relocation', 'aborted', 'maintenance', 'unknown')
    );
