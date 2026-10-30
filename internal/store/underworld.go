package store

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/google/uuid"
	"gorm.io/gorm"
	"gorm.io/gorm/clause"
)

const UnderworldBooksPerVisit = 4

type UnderworldObservation struct {
	AccountID   string          `json:"account_id"`
	ShipID      string          `json:"ship_id"`
	VisitID     string          `json:"visit_id"`
	PortID      string          `json:"port_id"`
	Kind        string          `json:"kind"`
	CommodityID string          `json:"commodity_id"`
	ObservedAt  time.Time       `json:"observed_at"`
	Payload     json.RawMessage `json:"payload" gorm:"type:jsonb"`
}

func (UnderworldObservation) TableName() string { return "underworld_observations" }

type underworldVisit struct {
	AccountID string
	ShipID    string
	PortID    string
	VisitID   string
}

func (underworldVisit) TableName() string { return "underworld_visits" }

type underworldReadAttempt struct {
	AccountID   string
	ShipID      string
	VisitID     string
	Kind        string
	CommodityID string
	AttemptedAt time.Time
}

func (underworldReadAttempt) TableName() string { return "underworld_read_attempts" }

// TrackUnderworldVisit retains one identity while a ship remains at a port,
// including berth/roadstead changes and updated_at changes. An authoritative
// departure (empty portID) or a different port starts the next visit.
func (s *Store) TrackUnderworldVisit(ctx context.Context, accountID, shipID, portID string) (string, error) {
	if !validID(accountID) || !validID(shipID) || portID != "" && !validID(portID) {
		return "", fmt.Errorf("underworld visit identity: %w", ErrInvalidInput)
	}
	// Most refreshes observe the same port. Avoid a locking write transaction
	// unless a departure or arrival actually changes the durable visit.
	db, cancel := s.query(ctx)
	var current underworldVisit
	err := db.Where("account_id = ? AND ship_id = ?", accountID, shipID).Take(&current).Error
	cancel()
	if err == nil && current.PortID == portID {
		return current.VisitID, nil
	}
	if err != nil && !errors.Is(err, gorm.ErrRecordNotFound) {
		return "", err
	}
	var visitID string
	err = s.writeOwned(ctx, accountID, func(tx *gorm.DB) error {
		row := underworldVisit{AccountID: accountID, ShipID: shipID}
		if err := tx.Clauses(clause.OnConflict{DoNothing: true}).Create(&row).Error; err != nil {
			return err
		}
		if err := tx.Clauses(clause.Locking{Strength: "UPDATE"}).Where("account_id = ? AND ship_id = ?", accountID, shipID).Take(&row).Error; err != nil {
			return err
		}
		visitID = row.VisitID
		if row.PortID == portID {
			return nil
		}
		visitID = ""
		if portID != "" {
			visitID = uuid.NewString()
		}
		return tx.Model(&underworldVisit{}).Where("account_id = ? AND ship_id = ?", accountID, shipID).
			Updates(map[string]any{"port_id": portID, "visit_id": visitID}).Error
	})
	return visitID, err
}

func validUnderworldKind(kind, commodityID string) bool {
	return kind == "wants" && commodityID == "" || kind == "book" && validID(commodityID)
}

// ReserveUnderworldRead commits before HTTP. Failed or ambiguous sends are not
// refunded, so a restart cannot repeat a read or exceed the arrival's allowance.
func (s *Store) ReserveUnderworldRead(ctx context.Context, accountID, shipID, visitID, kind, commodityID string) (bool, error) {
	if !validID(accountID) || !validID(shipID) || !validID(visitID) || !validUnderworldKind(kind, commodityID) {
		return false, fmt.Errorf("underworld read identity: %w", ErrInvalidInput)
	}
	reserved := false
	err := s.writeOwned(ctx, accountID, func(tx *gorm.DB) error {
		var visit underworldVisit
		if err := tx.Clauses(clause.Locking{Strength: "UPDATE"}).Where("account_id = ? AND ship_id = ? AND visit_id = ?", accountID, shipID, visitID).Take(&visit).Error; err != nil {
			if errors.Is(err, gorm.ErrRecordNotFound) {
				return ErrNotFound
			}
			return err
		}
		if kind == "book" {
			var count int64
			if err := tx.Model(&underworldReadAttempt{}).Where("account_id = ? AND ship_id = ? AND visit_id = ? AND kind = 'book'", accountID, shipID, visitID).Count(&count).Error; err != nil {
				return err
			}
			if count >= UnderworldBooksPerVisit {
				return nil
			}
		}
		row := underworldReadAttempt{
			AccountID: accountID, ShipID: shipID, VisitID: visitID,
			Kind: kind, CommodityID: commodityID, AttemptedAt: time.Now().UTC(),
		}
		result := tx.Clauses(clause.OnConflict{DoNothing: true}).Create(&row)
		reserved = result.RowsAffected == 1
		return result.Error
	})
	return reserved, err
}

// RecordUnderworldObservation keeps the first successful response per visit and
// good permanently. It is independent of the seven-day diagnostic retention.
func (s *Store) RecordUnderworldObservation(ctx context.Context, observation *UnderworldObservation) error {
	if observation == nil || !validID(observation.AccountID) || !validID(observation.ShipID) ||
		!validID(observation.VisitID) || !validID(observation.PortID) || !validUnderworldKind(observation.Kind, observation.CommodityID) ||
		observation.ObservedAt.IsZero() || !json.Valid(observation.Payload) || !bytes.HasPrefix(bytes.TrimSpace(observation.Payload), []byte("{")) {
		return fmt.Errorf("underworld observation: %w", ErrInvalidInput)
	}
	if len(observation.Payload) > MaxSnapshotBytes {
		return ErrResponseTooLarge
	}
	secrets, err := s.secrets(ctx, observation.AccountID)
	if err != nil {
		return err
	}
	payload, err := redactJSON(BoundEventPayload(observation.Payload), secrets, MaxSnapshotBytes)
	if err != nil {
		return err
	}
	if len(payload) > MaxSnapshotBytes {
		return ErrResponseTooLarge
	}
	row := *observation
	row.ObservedAt = row.ObservedAt.UTC()
	row.Payload = payload
	return s.writeOwned(ctx, row.AccountID, func(tx *gorm.DB) error {
		return tx.Clauses(clause.OnConflict{DoNothing: true}).Create(&row).Error
	})
}
