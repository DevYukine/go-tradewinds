-- +goose Up
ALTER TABLE settlements
    ADD COLUMN planned_net_pence bigint,
    ADD COLUMN planned_revenue_pence bigint,
    ADD COLUMN revenue_basis text,
    ADD COLUMN revenue_age_game_hours double precision,
    ADD COLUMN hazard_bps bigint,
    ADD COLUMN hazard_source text,
    ADD COLUMN cargo_shortfall_pence bigint,
    ADD COLUMN departed_cargo_pence bigint;
ALTER TABLE settlements ADD CONSTRAINT settlements_plan_values_check CHECK (
    (planned_revenue_pence IS NULL OR planned_revenue_pence >= 0) AND
    (revenue_age_game_hours IS NULL OR (revenue_age_game_hours >= 0 AND revenue_age_game_hours < 'Infinity'::double precision)) AND
    (hazard_bps IS NULL OR hazard_bps BETWEEN 0 AND 10000) AND
    (cargo_shortfall_pence IS NULL OR cargo_shortfall_pence >= 0) AND
    (departed_cargo_pence IS NULL OR departed_cargo_pence >= 0)
);

-- +goose Down
ALTER TABLE settlements DROP CONSTRAINT settlements_plan_values_check;
ALTER TABLE settlements
    DROP COLUMN departed_cargo_pence,
    DROP COLUMN cargo_shortfall_pence,
    DROP COLUMN hazard_source,
    DROP COLUMN hazard_bps,
    DROP COLUMN revenue_age_game_hours,
    DROP COLUMN revenue_basis,
    DROP COLUMN planned_revenue_pence,
    DROP COLUMN planned_net_pence;
