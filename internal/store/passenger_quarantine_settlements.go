package store

import (
	"context"
	"errors"
	"fmt"
	"time"

	"gorm.io/gorm"
	"gorm.io/gorm/clause"
)

// PutPassengerQuarantineSettlement records unavailable accounting, never fare
// income. The encrypted quarantine archive, not an invented terminal event,
// authenticates this marker. It remains unknown after raw archive retention.
func (s *Store) PutPassengerQuarantineSettlement(ctx context.Context, settlement *Settlement) error {
	if _, err := validateSettlement(settlement); err != nil {
		return err
	}
	if settlement.Kind != "passage" || settlement.CostsKnown || !settlement.ProceedsUnknown || settlement.ActionID != "" || settlement.EventID != "" || !validID(settlement.MissionID) || settlement.RevenueBasis == nil || (*settlement.RevenueBasis != "quarantined_passage_accounting" && *settlement.RevenueBasis != "quarantined_passage_recovery") {
		return fmt.Errorf("passenger quarantine settlement: %w", ErrInvalidInput)
	}
	row := *settlement
	if row.CreatedAt.IsZero() {
		row.CreatedAt = time.Now().UTC()
	}
	return s.writeOwned(ctx, row.AccountID, func(tx *gorm.DB) error {
		var existing settlementRow
		lookupErr := tx.Where("id = ? AND account_id = ?", row.ID, row.AccountID).Take(&existing).Error
		if lookupErr == nil {
			persisted, err := s.decodeSettlement(existing)
			if err != nil {
				return err
			}
			if !sameSettlement(persisted, row) {
				return ErrActionConflict
			}
			return nil
		}
		if !errors.Is(lookupErr, gorm.ErrRecordNotFound) {
			return lookupErr
		}
		var archive passengerQuarantineRow
		lookup := s.privateLookup("mission-ship", row.AccountID, PassengerQuarantinePrefix+row.MissionID)
		if err := tx.Where("account_id = ? AND ship_lookup = ?", row.AccountID, lookup).Take(&archive).Error; err != nil {
			if errors.Is(err, gorm.ErrRecordNotFound) {
				return ErrNotFound
			}
			return err
		}
		var metadata missionMetadata
		if err := s.decodePrivate(archive.Mission.MetadataCipher, &metadata, "missions", "metadata", row.AccountID, lookup); err != nil {
			return err
		}
		if metadata.Generation != row.MissionID || metadata.ShipID != PassengerQuarantinePrefix+row.MissionID {
			return ErrActionConflict
		}
		if archive.RecoveryShipLookup != s.privateLookup("passenger-recovery-ship", row.AccountID, row.ShipID) {
			return ErrActionConflict
		}
		private, err := s.encodeSettlement(row)
		if err != nil {
			return err
		}
		result := tx.Clauses(clause.OnConflict{Columns: []clause.Column{{Name: "id"}}, DoNothing: true}).Create(&private)
		if result.Error != nil {
			return result.Error
		}
		if result.RowsAffected == 0 {
			var existing settlementRow
			if err := tx.Where("id = ? AND account_id = ?", row.ID, row.AccountID).Take(&existing).Error; err != nil {
				return err
			}
			persisted, err := s.decodeSettlement(existing)
			if err != nil {
				return err
			}
			if !sameSettlement(persisted, row) {
				return ErrActionConflict
			}
		}
		return nil
	})
}

// PassengerSettlementCosts returns costs already carried by ordinary receipts
// for this generation, including receipts whose proceeds remain unknown.
func (s *Store) PassengerSettlementCosts(ctx context.Context, accountID, missionID string) (Settlement, error) {
	if !validID(accountID) || !validID(missionID) {
		return Settlement{}, fmt.Errorf("passenger settlement costs: %w", ErrInvalidInput)
	}
	db, cancel := s.query(ctx)
	defer cancel()
	var costs Settlement
	err := db.Model(&settlementRow{}).
		Where("account_id = ? AND mission_id = ? AND kind = ?", accountID, missionID, "passage").
		Where("COALESCE(revenue_basis, '') NOT IN ?", []string{"quarantined_passage_accounting", "quarantined_passage_recovery"}).
		Select("COALESCE(SUM(acquisition_pence), 0) AS acquisition_pence, COALESCE(SUM(fees_pence), 0) AS fees_pence, COALESCE(SUM(provisions_pence), 0) AS provisions_pence, COALESCE(SUM(expenses_pence), 0) AS expenses_pence").
		Scan(&costs).Error
	return costs, err
}
