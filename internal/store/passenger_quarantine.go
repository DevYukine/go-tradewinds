package store

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"gorm.io/gorm"
	"gorm.io/gorm/clause"
)

const PassengerQuarantinePrefix = "__passage_quarantine:"

type passengerQuarantineRow struct {
	Mission            missionRow `gorm:"embedded"`
	RecoveryShipLookup string
	ClosedAt           *time.Time
}

func (passengerQuarantineRow) TableName() string { return "passenger_quarantine_archives" }

// PutPassengerQuarantineArchive retains the first encrypted accounting record.
// Open archives are recovery state and are never removed by diagnostic retention.
func (s *Store) PutPassengerQuarantineArchive(ctx context.Context, shipID string, mission *Mission) error {
	if !validID(shipID) || !validMission(mission) || mission.ShipID != PassengerQuarantinePrefix+mission.Generation || mission.Generation == "" || len(mission.Data) > MaxSnapshotBytes {
		return fmt.Errorf("passenger quarantine archive: %w", ErrInvalidInput)
	}
	value := *mission
	value.UpdatedAt = time.Now().UTC()
	encoded, err := s.encodeMission(value)
	if err != nil {
		return err
	}
	row := passengerQuarantineRow{Mission: encoded, RecoveryShipLookup: s.privateLookup("passenger-recovery-ship", value.AccountID, shipID)}
	return s.writeOwned(ctx, value.AccountID, func(tx *gorm.DB) error {
		if err := lockMissionAccount(tx, value.AccountID); err != nil {
			return err
		}
		return tx.Clauses(clause.OnConflict{DoNothing: true}).Create(&row).Error
	})
}

// PassengerQuarantineArchive reads exactly one archive, never a fleet-wide list.
func (s *Store) PassengerQuarantineArchive(ctx context.Context, accountID, generation string) (*Mission, error) {
	if !validID(accountID) || !validID(generation) {
		return nil, fmt.Errorf("passenger quarantine lookup: %w", ErrInvalidInput)
	}
	db, cancel := s.query(ctx)
	defer cancel()
	var row passengerQuarantineRow
	lookup := s.privateLookup("mission-ship", accountID, PassengerQuarantinePrefix+generation)
	if err := db.Where("account_id = ? AND ship_lookup = ?", accountID, lookup).Take(&row).Error; err != nil {
		if errors.Is(err, gorm.ErrRecordNotFound) {
			return nil, nil
		}
		return nil, err
	}
	mission, err := s.decodeMission(row.Mission)
	if err == nil && (mission.Generation != generation || mission.ShipID != PassengerQuarantinePrefix+generation) {
		return nil, ErrActionConflict
	}
	return mission, err
}

// ReleaseQuarantinedPassengerHull atomically releases the operating slot and
// closes its archives after authoritative empty-aboard proof. Retention never
// races an unresolved operating mission or changes the retained private bytes.
func (s *Store) ReleaseQuarantinedPassengerHull(ctx context.Context, accountID, shipID string) error {
	if !validID(accountID) || !validID(shipID) {
		return fmt.Errorf("passenger quarantine closure: %w", ErrInvalidInput)
	}
	return s.writeOwned(ctx, accountID, func(tx *gorm.DB) error {
		if err := lockMissionAccount(tx, accountID); err != nil {
			return err
		}
		if err := tx.Where("account_id = ? AND ship_lookup = ?", accountID, s.privateLookup("mission-ship", accountID, shipID)).Delete(&missionRow{}).Error; err != nil {
			return err
		}
		return tx.Model(&passengerQuarantineRow{}).Where("account_id = ? AND recovery_ship_lookup = ? AND closed_at IS NULL", accountID, s.privateLookup("passenger-recovery-ship", accountID, shipID)).Update("closed_at", time.Now().UTC()).Error
	})
}

// Legacy archive migration needs only the owning hull, not an interpretation of
// damaged receipt fields. Unreadable ownership is retained for account recovery.
func passengerQuarantineShip(data json.RawMessage) (string, error) {
	var value struct {
		Plan struct {
			ShipID string `json:"ship_id"`
		} `json:"plan"`
	}
	if err := json.Unmarshal(data, &value); err != nil {
		return "", err
	}
	if !validID(value.Plan.ShipID) {
		return "", ErrInvalidInput
	}
	return value.Plan.ShipID, nil
}
