package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"time"

	"gorm.io/driver/postgres"
	"gorm.io/gorm"
	gormlog "gorm.io/gorm/logger"
)

// Version 8 and its backfill share Goose's transaction. Only distinct retained
// hourly responses are decoded; diagnostics already pruned cannot be recovered.
func (s *Store) migrateStandingsHistory(ctx context.Context, sqlTx *sql.Tx) error {
	tx, err := gorm.Open(postgres.New(postgres.Config{Conn: sqlTx}), &gorm.Config{
		DisableAutomaticPing: true, SkipDefaultTransaction: true,
		Logger: gormlog.Default.LogMode(gormlog.Silent),
	})
	if err != nil {
		return err
	}
	tx = tx.WithContext(ctx)
	if err := tx.Exec(`CREATE TABLE standings_history (
		season_number integer NOT NULL,
		refreshed_at timestamptz NOT NULL,
		rank integer NOT NULL CHECK (rank > 0),
		company_name text NOT NULL,
		home_port_id text NOT NULL,
		net_worth_pence bigint NOT NULL,
		titles jsonb NOT NULL,
		next_refresh_at timestamptz,
		observed_at timestamptz NOT NULL,
		PRIMARY KEY (season_number, refreshed_at, rank),
		UNIQUE (season_number, refreshed_at, company_name)
	);
	CREATE INDEX standings_history_time_idx ON standings_history (refreshed_at);
	CREATE INDEX standings_history_identity_idx ON standings_history (season_number, company_name, refreshed_at);
	CREATE TABLE standings_accounts (
		account_id text NOT NULL REFERENCES accounts(id) ON DELETE CASCADE,
		season_number integer NOT NULL,
		refreshed_at timestamptz NOT NULL,
		rank integer NOT NULL,
		PRIMARY KEY (account_id, season_number, refreshed_at),
		FOREIGN KEY (season_number, refreshed_at, rank) REFERENCES standings_history (season_number, refreshed_at, rank)
	);`).Error; err != nil {
		return err
	}
	secrets, err := s.authenticatedSecrets(tx)
	if err != nil {
		return err
	}
	var retained []struct {
		AccountID  string
		Data       json.RawMessage
		ObservedAt time.Time
	}
	if err := tx.Raw(`SELECT account_id, response AS data, created_at AS observed_at FROM (
		SELECT DISTINCT ON (account_id, response->>'season_number', response->>'refreshed_at')
			account_id, response, created_at FROM requests
		WHERE method = 'GET' AND path = '/v1/world/leaderboard' AND status = 200
			AND response->>'refreshed_at' IS NOT NULL
		ORDER BY account_id, response->>'season_number', response->>'refreshed_at', created_at
	) AS responses
	UNION ALL SELECT account_id, data->'leaderboard', updated_at FROM snapshots
		WHERE data->'leaderboard' IS NOT NULL
	ORDER BY observed_at`).Scan(&retained).Error; err != nil {
		return err
	}
	for _, observation := range retained {
		raw, err := redactJSON(observation.Data, secrets[observation.AccountID], 0)
		if err != nil {
			return err
		}
		if err := recordStandings(tx, observation.AccountID, raw, observation.ObservedAt); err != nil {
			return err
		}
	}
	return nil
}
