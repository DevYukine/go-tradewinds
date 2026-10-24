package store

import (
	"context"
	"errors"
	"fmt"
	"math"
	"sort"
	"time"

	"gorm.io/gorm"
	"gorm.io/gorm/clause"
)

// Settlement is realized mission accounting. Principal transfers and returned
// collateral are not proceeds; unknown acquisition costs must set CostsKnown=false.
// The journal action is the evidence anchor, not a second source of HTTP outcomes.
type Settlement struct {
	ID               string `json:"id" gorm:"primaryKey"`
	AccountID        string `json:"account_id"`
	ShipID           string `json:"ship_id"`
	MissionID        string `json:"mission_id"`
	Lane             string `json:"lane"`
	ActionID         string `json:"action_id"`
	Kind             string `json:"kind"`
	Origin           string `json:"origin"`
	Destination      string `json:"destination"`
	Commodity        string `json:"commodity"`
	QuantityTons     int64  `json:"quantity_tons"`
	GameSeconds      int64  `json:"game_seconds"`
	CostsKnown       bool   `json:"costs_known"`
	AcquisitionPence int64  `json:"acquisition_pence"`
	ProceedsPence    int64  `json:"proceeds_pence"`
	FeesPence        int64  `json:"fees_pence"`
	ProvisionsPence  int64  `json:"provisions_pence"`
	ExpensesPence    int64  `json:"expenses_pence"`
	// Exploration expense prices consumed stores and probe costs separately
	// from actual cash P&L. Nil legacy rows retain the full-cash upper bound.
	ExplorationExpensePence *int64    `json:"exploration_expense_pence,omitempty"`
	StartedAt               time.Time `json:"started_at"`
	CompletedAt             time.Time `json:"completed_at"`
	CreatedAt               time.Time `json:"created_at"`
}

func (Settlement) TableName() string { return "settlements" }

func sameSettlement(a, b Settlement) bool {
	sameExplorationExpense := a.ExplorationExpensePence == nil && b.ExplorationExpensePence == nil ||
		a.ExplorationExpensePence != nil && b.ExplorationExpensePence != nil &&
			*a.ExplorationExpensePence == *b.ExplorationExpensePence
	return a.ID == b.ID && a.AccountID == b.AccountID && a.ShipID == b.ShipID &&
		a.MissionID == b.MissionID && a.Lane == b.Lane && a.ActionID == b.ActionID &&
		a.Kind == b.Kind && a.Origin == b.Origin && a.Destination == b.Destination &&
		a.Commodity == b.Commodity && a.QuantityTons == b.QuantityTons &&
		a.GameSeconds == b.GameSeconds && a.CostsKnown == b.CostsKnown &&
		a.AcquisitionPence == b.AcquisitionPence && a.ProceedsPence == b.ProceedsPence &&
		a.FeesPence == b.FeesPence && a.ProvisionsPence == b.ProvisionsPence &&
		a.ExpensesPence == b.ExpensesPence && sameExplorationExpense &&
		a.StartedAt.Truncate(time.Microsecond).Equal(b.StartedAt.Truncate(time.Microsecond)) &&
		a.CompletedAt.Truncate(time.Microsecond).Equal(b.CompletedAt.Truncate(time.Microsecond))
}

func (s *Store) PutSettlement(ctx context.Context, settlement *Settlement) error {
	if settlement == nil || !validID(settlement.ID) || !validID(settlement.AccountID) || !validID(settlement.ActionID) ||
		settlement.QuantityTons < 0 || settlement.GameSeconds < 0 || settlement.AcquisitionPence < 0 ||
		settlement.ProceedsPence < 0 || settlement.FeesPence < 0 || settlement.ProvisionsPence < 0 || settlement.ExpensesPence < 0 ||
		settlement.ExplorationExpensePence != nil && *settlement.ExplorationExpensePence < 0 ||
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
		privateRow, err := s.encodeSettlement(row)
		if err != nil {
			return err
		}
		result := tx.Clauses(clause.OnConflict{Columns: []clause.Column{{Name: "id"}}, DoNothing: true}).Create(&privateRow)
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
		var stored settlementRow
		if err := tx.First(&stored, "id = ?", row.ID).Error; err != nil {
			return err
		}
		persisted, err := s.decodeSettlement(stored)
		if err != nil {
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
		db = db.Where("lane_lookup = ?", s.privateLookup("settlement-lane", "", lane))
	}
	privateRows := make([]settlementRow, 0)
	if err := db.Order("completed_at DESC, id").Limit(limit).Find(&privateRows).Error; err != nil {
		return nil, err
	}
	rows := make([]Settlement, 0, len(privateRows))
	for _, stored := range privateRows {
		value, err := s.decodeSettlement(stored)
		if err != nil {
			return nil, err
		}
		if lane != "" && value.Lane != lane {
			return nil, ErrActionConflict
		}
		rows = append(rows, value)
	}
	return rows, nil
}

// EconomicHistory is permanent execution evidence, not a page of display
// history. Commercial losses include zero-ton failed procurements. Probes are
// charged separately so exploration cannot replenish its budget by restarting.
type EconomicHistory struct {
	CommercialNetPence   int64
	NetPence             int64
	PositiveCycles       int64
	PositiveQuantityTons int64
	PositiveNetPence     int64
	GameSeconds          int64
	PositiveGameSeconds  int64
	UnknownCosts         bool
	UnmeasuredDuration   bool
	ProbeExpensePence    int64
	UnknownProbeCosts    bool
	LastCompletion       *time.Time
	LastProbeCompletion  *time.Time
}

func (s *Store) EconomicHistory(ctx context.Context, accountID, lane string) (EconomicHistory, error) {
	var result EconomicHistory
	if !validID(accountID) || len(lane) > 1024 {
		return result, fmt.Errorf("economic history query: %w", ErrInvalidInput)
	}
	db, cancel := s.query(ctx)
	defer cancel()
	// PostgreSQL numeric keeps both each row's arithmetic and the cumulative
	// sums exact. Scanning an out-of-range aggregate into int64 returns an
	// error instead of wrapping a loss into positive underwriting evidence.
	err := db.Raw(`
		WITH evidence AS (
			SELECT *,
				proceeds_pence::numeric - acquisition_pence - fees_pence - provisions_pence - expenses_pence AS net,
				kind IN ('spot', 'charter', 'freight', 'spot+freight', 'charter+freight', 'recovery', 'aborted', 'unknown') AS commercial,
				kind IN ('spot', 'charter', 'freight', 'spot+freight', 'charter+freight', 'recovery') AS delivery
			FROM settlements
			WHERE account_id = ? AND (? = '' OR lane_lookup = ?)
		)
		SELECT
			COALESCE(SUM(net) FILTER (WHERE commercial AND costs_known), 0) AS commercial_net_pence,
			COALESCE(SUM(net) FILTER (WHERE (commercial OR kind = 'relocation') AND costs_known), 0) AS net_pence,
			COUNT(*) FILTER (WHERE delivery AND costs_known AND net > 0 AND quantity_tons > 0 AND game_seconds > 0) AS positive_cycles,
			COALESCE(SUM(quantity_tons::numeric) FILTER (WHERE delivery AND costs_known AND net > 0 AND quantity_tons > 0 AND game_seconds > 0), 0) AS positive_quantity_tons,
			COALESCE(SUM(net) FILTER (WHERE delivery AND costs_known AND net > 0 AND quantity_tons > 0 AND game_seconds > 0), 0) AS positive_net_pence,
			COALESCE(SUM(game_seconds::numeric) FILTER (WHERE commercial OR kind = 'relocation'), 0) AS game_seconds,
			COALESCE(SUM(game_seconds::numeric) FILTER (WHERE delivery AND costs_known AND net > 0 AND quantity_tons > 0 AND game_seconds > 0), 0) AS positive_game_seconds,
			COALESCE(BOOL_OR(NOT costs_known OR kind = 'unknown') FILTER (WHERE commercial OR kind = 'relocation'), false) AS unknown_costs,
			COALESCE(BOOL_OR(game_seconds <= 0) FILTER (WHERE commercial OR kind = 'relocation'), false) AS unmeasured_duration,
			COALESCE(SUM(COALESCE(exploration_expense_pence::numeric,
				acquisition_pence::numeric + fees_pence + provisions_pence + expenses_pence))
				FILTER (WHERE kind = 'relocation' AND costs_known), 0) AS probe_expense_pence,
			COALESCE(BOOL_OR(NOT costs_known) FILTER (WHERE kind = 'relocation'), false) AS unknown_probe_costs,
			MAX(completed_at) FILTER (WHERE commercial OR kind = 'relocation') AS last_completion,
			MAX(completed_at) FILTER (WHERE kind = 'relocation') AS last_probe_completion
		FROM evidence`, accountID, lane, s.privateLookup("settlement-lane", "", lane)).Scan(&result).Error
	return result, err
}

// RelocationProbes looks up only the caller's bounded evidence shortlist.
func (s *Store) RelocationProbes(ctx context.Context, accountID string, lanes []string) ([]string, error) {
	if !validID(accountID) || len(lanes) > 1024 {
		return nil, fmt.Errorf("relocation probe query: %w", ErrInvalidInput)
	}
	for _, lane := range lanes {
		if lane == "" || len(lane) > 1024 {
			return nil, fmt.Errorf("relocation probe lane: %w", ErrInvalidInput)
		}
	}
	rows := make([]string, 0)
	if len(lanes) == 0 {
		return rows, nil
	}
	originals := make(map[string]string, len(lanes))
	lookups := make([]string, 0, len(lanes))
	for _, lane := range lanes {
		lookup := s.privateLookup("settlement-lane", "", lane)
		if original, exists := originals[lookup]; exists {
			if original != lane {
				return nil, ErrActionConflict
			}
			continue
		}
		originals[lookup] = lane
		lookups = append(lookups, lookup)
	}
	db, cancel := s.query(ctx)
	defer cancel()
	var stored []settlementRow
	if err := db.Select("DISTINCT ON (lane_lookup) *").
		Where("account_id = ? AND kind = ? AND lane_lookup IN ?", accountID, "relocation", lookups).
		Order("lane_lookup, completed_at DESC, id").Find(&stored).Error; err != nil {
		return nil, err
	}
	found := make(map[string]bool, len(stored))
	for _, row := range stored {
		value, err := s.decodeSettlement(row)
		if err != nil {
			return nil, err
		}
		if value.Lane != originals[row.LaneLookup] {
			return nil, ErrActionConflict
		}
		found[value.Lane] = true
	}
	for lane := range found {
		rows = append(rows, lane)
	}
	sort.Strings(rows)
	return rows, nil
}
