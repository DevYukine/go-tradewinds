package store

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"gorm.io/gorm"
	"gorm.io/gorm/clause"
)

func validMission(mission *Mission) bool {
	return mission != nil && validID(mission.AccountID) && validID(mission.ShipID) && mission.Phase != "" &&
		(len(mission.Data) == 0 || json.Valid(mission.Data))
}

func (s *Store) mission(tx *gorm.DB, accountID, shipID string) (*Mission, error) {
	var row missionRow
	lookup := s.privateLookup("mission-ship", accountID, shipID)
	if err := tx.Where("account_id = ? AND ship_lookup = ?", accountID, lookup).Take(&row).Error; err != nil {
		if errors.Is(err, gorm.ErrRecordNotFound) {
			return nil, nil
		}
		return nil, err
	}
	mission, err := s.decodeMission(row)
	if err != nil {
		return nil, err
	}
	if mission.ShipID != shipID {
		return nil, ErrActionConflict
	}
	return mission, nil
}

func (s *Store) GetMission(ctx context.Context, accountID, shipID string) (*Mission, error) {
	if !validID(accountID) || !validID(shipID) {
		return nil, fmt.Errorf("mission identity: %w", ErrInvalidInput)
	}
	db, cancel := s.query(ctx)
	defer cancel()
	return s.mission(db, accountID, shipID)
}

func lockMissionAccount(tx *gorm.DB, accountID string) error {
	return tx.Exec("SELECT pg_advisory_xact_lock(hashtextextended(?, 0))", "tradewinds:mission-slot:"+accountID).Error
}

func (s *Store) PutMission(ctx context.Context, mission *Mission) error {
	if !validMission(mission) {
		return fmt.Errorf("mission fields: %w", ErrInvalidInput)
	}
	value := *mission
	if len(value.Data) == 0 {
		value.Data = json.RawMessage("null")
	}
	value.UpdatedAt = time.Now().UTC().Truncate(time.Microsecond)
	row, err := s.encodeMission(value)
	if err != nil {
		return err
	}
	err = s.writeOwned(ctx, value.AccountID, func(tx *gorm.DB) error {
		if err := lockMissionAccount(tx, value.AccountID); err != nil {
			return err
		}
		if _, err := s.mission(tx, value.AccountID, value.ShipID); err != nil {
			return err
		}
		return tx.Clauses(clause.OnConflict{
			Columns:   []clause.Column{{Name: "account_id"}, {Name: "ship_lookup"}},
			DoUpdates: clause.AssignmentColumns([]string{"metadata_cipher", "data_cipher", "updated_at"}),
		}).Create(&row).Error
	})
	if err == nil {
		mission.UpdatedAt = value.UpdatedAt
	}
	return err
}

func (s *Store) DeleteMission(ctx context.Context, accountID, shipID string) error {
	if !validID(accountID) || !validID(shipID) {
		return fmt.Errorf("mission identity: %w", ErrInvalidInput)
	}
	return s.writeOwned(ctx, accountID, func(tx *gorm.DB) error {
		if err := lockMissionAccount(tx, accountID); err != nil {
			return err
		}
		if _, err := s.mission(tx, accountID, shipID); err != nil {
			return err
		}
		return tx.Where("account_id = ? AND ship_lookup = ?", accountID, s.privateLookup("mission-ship", accountID, shipID)).Delete(&missionRow{}).Error
	})
}

// This proof is decoded only at handoff time. Migration treats recovery data as
// opaque bytes, including members that this version of the engine does not know.
type growthHandoffProof struct {
	Action      json.RawMessage `json:"action"`
	BuiltShipID string          `json:"built_ship_id"`
	GrowthLane  *struct {
		AdmissionID string `json:"admission_id"`
	} `json:"growth_lane"`
}

func handoffProof(mission *Mission, generation string) (growthHandoffProof, error) {
	var proof growthHandoffProof
	if json.Unmarshal(mission.Data, &proof) != nil || proof.GrowthLane == nil || proof.GrowthLane.AdmissionID != generation {
		return proof, ErrActionConflict
	}
	return proof, nil
}

// HandoffGrowthMission commits commissioning and its first operating mission as
// one write. A retried handoff never overwrites an already advancing voyage.
func (s *Store) HandoffGrowthMission(ctx context.Context, accountID, expectedGrowthGeneration string, next *Mission) (*Mission, error) {
	if !validMission(next) || next.AccountID != accountID || next.ShipID == "__growth" ||
		!validID(expectedGrowthGeneration) || !validID(next.Generation) || next.Generation == expectedGrowthGeneration {
		return nil, fmt.Errorf("growth mission handoff: %w", ErrInvalidInput)
	}
	if _, err := handoffProof(next, expectedGrowthGeneration); err != nil {
		return nil, err
	}
	var persisted *Mission
	err := s.writeOwned(ctx, accountID, func(tx *gorm.DB) error {
		if err := lockMissionAccount(tx, accountID); err != nil {
			return err
		}
		control, err := s.mission(tx, accountID, "__growth")
		if err != nil {
			return err
		}
		if control != nil {
			proof, err := handoffProof(control, expectedGrowthGeneration)
			if err != nil || control.Generation != expectedGrowthGeneration || control.Phase != "commissioning" ||
				proof.BuiltShipID != next.ShipID || len(proof.Action) != 0 && !bytes.Equal(bytes.TrimSpace(proof.Action), []byte("null")) {
				return ErrActionConflict
			}
		}
		existing, err := s.mission(tx, accountID, next.ShipID)
		if err != nil {
			return err
		}
		if existing != nil {
			if _, err := handoffProof(existing, expectedGrowthGeneration); err != nil || existing.Generation != next.Generation {
				return ErrActionConflict
			}
			persisted = existing
		} else {
			if control == nil {
				return ErrActionConflict
			}
			value := *next
			value.UpdatedAt = time.Now().UTC().Truncate(time.Microsecond)
			row, err := s.encodeMission(value)
			if err != nil {
				return err
			}
			if err := tx.Create(&row).Error; err != nil {
				return err
			}
			persisted = &value
		}
		if control != nil {
			return tx.Where("account_id = ? AND ship_lookup = ?", accountID, s.privateLookup("mission-ship", accountID, "__growth")).Delete(&missionRow{}).Error
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	return persisted, nil
}
