package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"strings"
	"time"

	"go.uber.org/zap"
	"gorm.io/driver/postgres"
	"gorm.io/gorm"
	gormlog "gorm.io/gorm/logger"
)

const passengerQuarantineMigrationVersion = 16

func (s *Store) migratePassengerQuarantineArchives(ctx context.Context, sqlTx *sql.Tx) error {
	tx, err := gorm.Open(postgres.New(postgres.Config{Conn: sqlTx}), &gorm.Config{
		DisableAutomaticPing:   true,
		SkipDefaultTransaction: true,
		Logger:                 gormlog.Default.LogMode(gormlog.Silent),
	})
	if err != nil {
		return err
	}
	tx = tx.WithContext(ctx)
	if err := tx.Exec(`CREATE TABLE passenger_quarantine_archives (
 account_id text NOT NULL,
 ship_lookup text NOT NULL,
 metadata_cipher bytea NOT NULL,
 data_cipher bytea NOT NULL,
 updated_at timestamptz NOT NULL,
 recovery_ship_lookup text NOT NULL,
 closed_at timestamptz,
 PRIMARY KEY(account_id,ship_lookup)
 );
 CREATE INDEX passenger_quarantine_recovery ON passenger_quarantine_archives(account_id,recovery_ship_lookup) WHERE closed_at IS NULL;
 CREATE INDEX passenger_quarantine_retention ON passenger_quarantine_archives(closed_at) WHERE closed_at IS NOT NULL;`).Error; err != nil {
		return err
	}
	// The previous in-development representation used encrypted mission slots.
	// Move those once, in bounded pages; executor reads never scan these archives.
	var account, lookup string
	skipped := 0
	for {
		var rows []missionRow
		if err := tx.Where("(account_id,ship_lookup) > (?,?)", account, lookup).Order("account_id,ship_lookup").Limit(100).Find(&rows).Error; err != nil {
			return err
		}
		for _, row := range rows {
			mission, err := s.decodeMission(row)
			if err != nil {
				skipped++
				continue
			}
			if !strings.HasPrefix(mission.ShipID, PassengerQuarantinePrefix) {
				continue
			}
			shipID, err := passengerQuarantineShip(mission.Data)
			if err != nil {
				skipped++
				continue
			}
			archive := passengerQuarantineRow{Mission: row, RecoveryShipLookup: s.privateLookup("passenger-recovery-ship", row.AccountID, shipID)}
			operating, err := s.mission(tx, row.AccountID, shipID)
			if err != nil {
				skipped++
				continue
			}
			var current struct {
				Passage *struct {
					QuarantineReason string `json:"quarantine_reason"`
					RecoveryOnly     bool   `json:"recovery_only"`
				} `json:"passage"`
			}
			if operating != nil {
				if err := json.Unmarshal(operating.Data, &current); err != nil {
					skipped++
					continue
				}
			}
			if operating == nil || current.Passage == nil || current.Passage.QuarantineReason == "" && !current.Passage.RecoveryOnly {
				closed := time.Now().UTC()
				archive.ClosedAt = &closed
			}
			if err := tx.Create(&archive).Error; err != nil {
				return err
			}
			if err := tx.Where("account_id = ? AND ship_lookup = ?", row.AccountID, row.ShipLookup).Delete(&missionRow{}).Error; err != nil {
				return err
			}
		}
		if len(rows) < 100 {
			if skipped > 0 {
				s.log.Warn("passenger quarantine migration retained unreadable recovery rows", zap.Int("skipped_rows", skipped))
			}
			return nil
		}
		last := rows[len(rows)-1]
		account, lookup = last.AccountID, last.ShipLookup
	}
}
