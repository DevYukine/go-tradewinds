-- +goose Up
-- One official valuation per account and publication time. Repeated
-- leaderboard polls of the same hourly publication must not add points.
CREATE UNIQUE INDEX metrics_net_worth_unique_idx ON metrics (account_id, recorded_at)
    WHERE name = 'net_worth_pence';

-- Backfill recorded own standings. standings_accounts links an account to the
-- player's best company, which can be another company of the same player, so
-- only standings under the account's own configured company name are copied.
-- Raw request diagnostics are not scanned: they are large, and the upgrade
-- would hold the metrics index lock for minutes.
INSERT INTO metrics (account_id, name, value, recorded_at)
SELECT own.account_id, 'net_worth_pence', history.net_worth_pence, history.refreshed_at
FROM standings_accounts AS own
JOIN standings_history AS history
    ON history.season_number = own.season_number
    AND history.refreshed_at = own.refreshed_at
    AND history.rank = own.rank
JOIN accounts ON accounts.id = own.account_id
JOIN configured_company_identities AS identity
    ON identity.company_id = accounts.company_id
    AND identity.name = history.company_name
ON CONFLICT (account_id, recorded_at) WHERE name = 'net_worth_pence' DO NOTHING;

-- +goose Down
-- Recorded valuations are permanent history; only the index is removed.
DROP INDEX metrics_net_worth_unique_idx;
