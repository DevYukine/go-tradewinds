package store

import (
	"context"
	"errors"
	"fmt"
	"math"
	"time"

	"gorm.io/gorm"
	"gorm.io/gorm/clause"
)

// Settlement is realized mission accounting. Principal transfers and returned
// collateral are not proceeds; unknown acquisition costs must set CostsKnown=false.
// The journal action is the evidence anchor, not a second source of HTTP outcomes.
type Settlement struct {
	ID               string    `json:"id" gorm:"primaryKey"`
	AccountID        string    `json:"account_id"`
	ShipID           string    `json:"ship_id"`
	MissionID        string    `json:"mission_id"`
	Lane             string    `json:"lane"`
	ActionID         string    `json:"action_id"`
	Kind             string    `json:"kind"`
	Origin           string    `json:"origin"`
	Destination      string    `json:"destination"`
	Commodity        string    `json:"commodity"`
	QuantityTons     int64     `json:"quantity_tons"`
	GameSeconds      int64     `json:"game_seconds"`
	CostsKnown       bool      `json:"costs_known"`
	AcquisitionPence int64     `json:"acquisition_pence"`
	ProceedsPence    int64     `json:"proceeds_pence"`
	FeesPence        int64     `json:"fees_pence"`
	ProvisionsPence  int64     `json:"provisions_pence"`
	ExpensesPence    int64     `json:"expenses_pence"`
	StartedAt        time.Time `json:"started_at"`
	CompletedAt      time.Time `json:"completed_at"`
	CreatedAt        time.Time `json:"created_at"`
}

func (Settlement) TableName() string { return "settlements" }

func sameSettlement(a, b Settlement) bool {
	return a.ID == b.ID && a.AccountID == b.AccountID && a.ShipID == b.ShipID &&
		a.MissionID == b.MissionID && a.Lane == b.Lane && a.ActionID == b.ActionID &&
		a.Kind == b.Kind && a.Origin == b.Origin && a.Destination == b.Destination &&
		a.Commodity == b.Commodity && a.QuantityTons == b.QuantityTons &&
		a.GameSeconds == b.GameSeconds && a.CostsKnown == b.CostsKnown &&
		a.AcquisitionPence == b.AcquisitionPence && a.ProceedsPence == b.ProceedsPence &&
		a.FeesPence == b.FeesPence && a.ProvisionsPence == b.ProvisionsPence &&
		a.ExpensesPence == b.ExpensesPence &&
		a.StartedAt.Truncate(time.Microsecond).Equal(b.StartedAt.Truncate(time.Microsecond)) &&
		a.CompletedAt.Truncate(time.Microsecond).Equal(b.CompletedAt.Truncate(time.Microsecond))
}

func (s *Store) PutSettlement(ctx context.Context, settlement *Settlement) error {
	if settlement == nil || !validID(settlement.ID) || !validID(settlement.AccountID) || !validID(settlement.ActionID) ||
		settlement.QuantityTons < 0 || settlement.GameSeconds < 0 || settlement.AcquisitionPence < 0 ||
		settlement.ProceedsPence < 0 || settlement.FeesPence < 0 || settlement.ProvisionsPence < 0 || settlement.ExpensesPence < 0 ||
		settlement.CompletedAt.IsZero() {
		return fmt.Errorf("settlement fields: %w", ErrInvalidInput)
	}
	row := *settlement
	net := row.ProceedsPence
	for _, cost := range []int64{row.AcquisitionPence, row.FeesPence, row.ProvisionsPence, row.ExpensesPence} {
		if net < math.MinInt64+cost {
			return fmt.Errorf("settlement net overflows integer pence: %w", ErrInvalidInput)
		}
		net -= cost
	}
	err := s.writeOwned(ctx, row.AccountID, func(tx *gorm.DB) error {
		var action actionRow
		if err := tx.Where("id = ? AND account_id = ?", row.ActionID, row.AccountID).Take(&action).Error; err != nil {
			if errors.Is(err, gorm.ErrRecordNotFound) {
				return ErrNotFound
			}
			return err
		}
		if action.State == ActionPending {
			return fmt.Errorf("settlement evidence is unresolved: %w", ErrActionConflict)
		}
		if row.CreatedAt.IsZero() {
			row.CreatedAt = time.Now().UTC()
		}
		result := tx.Clauses(clause.OnConflict{Columns: []clause.Column{{Name: "id"}}, DoNothing: true}).Create(&row)
		if result.Error != nil {
			if errors.Is(result.Error, gorm.ErrDuplicatedKey) {
				return ErrActionConflict
			}
			return result.Error
		}
		if result.RowsAffected != 0 {
			if !row.CostsKnown {
				return nil
			}
			return tx.Create(&Metric{
				AccountID:  row.AccountID,
				Name:       "realized_profit_pence",
				Value:      float64(net),
				RecordedAt: row.CompletedAt,
			}).Error
		}
		var persisted Settlement
		if err := tx.First(&persisted, "id = ?", row.ID).Error; err != nil {
			return err
		}
		if !sameSettlement(persisted, row) {
			return ErrActionConflict
		}
		row.CreatedAt = persisted.CreatedAt
		return nil
	})
	if err == nil {
		settlement.CreatedAt = row.CreatedAt
	}
	return err
}

func (s *Store) SettlementHistory(ctx context.Context, accountID, lane string, limit int) ([]Settlement, error) {
	if accountID != "" && !validID(accountID) || len(lane) > 1024 || limit < 1 || limit > MaxHistoryLimit {
		return nil, fmt.Errorf("settlement history query: %w", ErrInvalidInput)
	}
	db, cancel := s.query(ctx)
	defer cancel()
	if accountID != "" {
		db = db.Where("account_id = ?", accountID)
	}
	if lane != "" {
		db = db.Where("lane = ?", lane)
	}
	rows := make([]Settlement, 0)
	err := db.Order("completed_at DESC, id").Limit(limit).Find(&rows).Error
	return rows, err
}

// RelocationRestart uses permanent settlements rather than the bounded display
// history. The caller also charges Latest's own loss before allowing a retry.
type RelocationRestart struct {
	Latest                *Settlement
	NetSincePence         int64
	UnknownCostSince      bool
	PositiveDeliverySince bool
}

func (s *Store) LatestRelocationRestart(ctx context.Context, accountID string) (RelocationRestart, error) {
	var result RelocationRestart
	if !validID(accountID) {
		return result, fmt.Errorf("relocation restart account: %w", ErrInvalidInput)
	}
	db, cancel := s.query(ctx)
	defer cancel()
	var latest Settlement
	err := db.Where("account_id = ? AND kind = ?", accountID, "relocation").
		Order("completed_at DESC, id DESC").Take(&latest).Error
	if errors.Is(err, gorm.ErrRecordNotFound) {
		return result, nil
	}
	if err != nil {
		return result, err
	}
	result.Latest = &latest
	var totals struct {
		NetSincePence         int64
		UnknownCostSince      bool
		PositiveDeliverySince bool
	}
	err = db.Raw(`
		SELECT COALESCE(SUM(proceeds_pence::numeric - acquisition_pence - fees_pence - provisions_pence - expenses_pence), 0) AS net_since_pence,
			COALESCE(BOOL_OR(NOT costs_known), false) AS unknown_cost_since,
			COALESCE(BOOL_OR(costs_known AND quantity_tons > 0 AND proceeds_pence::numeric - acquisition_pence - fees_pence - provisions_pence - expenses_pence > 0), false) AS positive_delivery_since
		FROM settlements
		WHERE account_id = ? AND (completed_at > ? OR (completed_at = ? AND id > ?))`,
		accountID, latest.CompletedAt, latest.CompletedAt, latest.ID).Scan(&totals).Error
	result.NetSincePence = totals.NetSincePence
	result.UnknownCostSince = totals.UnknownCostSince
	result.PositiveDeliverySince = totals.PositiveDeliverySince
	return result, err
}
