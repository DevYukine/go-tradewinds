-- +goose Up
ALTER TABLE futures_intents
    DROP CONSTRAINT futures_intents_port_id_check,
    DROP CONSTRAINT futures_intents_commodity_id_check,
    DROP CONSTRAINT futures_intents_lot_tons_check,
    DROP CONSTRAINT futures_intents_exposure_pence_check,
    DROP CONSTRAINT futures_intents_account_id_action_id_key,
    ADD CONSTRAINT futures_intents_lot_tons_check CHECK (lot_tons > 0 AND lot_tons <= 2000000),
    ADD CONSTRAINT futures_intents_exposure_pence_check CHECK (exposure_pence > 0 AND exposure_pence <= 2000000),
    ADD COLUMN strategy text NOT NULL DEFAULT '',
    ADD COLUMN listing_id text NOT NULL DEFAULT '',
    ADD COLUMN counterparty_id text NOT NULL DEFAULT '',
    ADD COLUMN maturity_game_hours bigint NOT NULL DEFAULT 0,
    ADD COLUMN admission jsonb;
CREATE INDEX futures_intents_action_idx ON futures_intents (account_id, action_id);
CREATE UNIQUE INDEX futures_intents_listing_idx ON futures_intents (account_id, listing_id) WHERE listing_id <> '';

-- +goose Down
DROP INDEX futures_intents_listing_idx;
DROP INDEX futures_intents_action_idx;
ALTER TABLE futures_intents
    DROP COLUMN admission,
    DROP COLUMN maturity_game_hours,
    DROP COLUMN counterparty_id,
    DROP COLUMN listing_id,
    DROP COLUMN strategy,
    DROP CONSTRAINT futures_intents_lot_tons_check,
    DROP CONSTRAINT futures_intents_exposure_pence_check,
    ADD CONSTRAINT futures_intents_port_id_check CHECK (port_id = 'amsterdam'),
    ADD CONSTRAINT futures_intents_commodity_id_check CHECK (commodity_id = 'water_beer'),
    ADD CONSTRAINT futures_intents_lot_tons_check CHECK (lot_tons = 1),
    ADD CONSTRAINT futures_intents_exposure_pence_check CHECK (exposure_pence > 0 AND exposure_pence <= 30000),
    ADD UNIQUE (account_id, action_id);
