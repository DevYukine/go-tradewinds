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

func (s *Store) SettlementsByID(ctx context.Context, accountID string, ids []string) ([]Settlement, error) {
	if !validID(accountID) {
		return nil, fmt.Errorf("settlement identity query: %w", ErrInvalidInput)
	}
	for _, id := range ids {
		if !validID(id) {
			return nil, fmt.Errorf("settlement identity query: %w", ErrInvalidInput)
		}
	}
	if len(ids) == 0 {
		return nil, nil
	}
	db, cancel := s.query(ctx)
	defer cancel()
	var privateRows []settlementRow
	if err := db.Where("account_id = ? AND id IN ?", accountID, ids).Find(&privateRows).Error; err != nil {
		return nil, err
	}
	if len(privateRows) != len(ids) {
		return nil, ErrNotFound
	}
	rows := make([]Settlement, 0, len(privateRows))
	for _, stored := range privateRows {
		value, err := s.decodeSettlement(stored)
		if err != nil {
			return nil, err
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
	UnknownLossPence     int64
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
	result, err := s.economicHistory(ctx, accountID, lane, time.Time{}, 0, false)
	return result.EconomicHistory, err
}

// EconomicHistoryWindow measures whole settlements completed inside a real-time
// window. Only cargo-free shipyard control closeouts can lack trading costs.
func (s *Store) EconomicHistoryWindow(ctx context.Context, accountID, lane string, asOf time.Time, window time.Duration) (GrowthHistory, error) {
	if asOf.IsZero() || window <= 0 {
		return GrowthHistory{}, fmt.Errorf("economic history window: %w", ErrInvalidInput)
	}
	return s.economicHistory(ctx, accountID, lane, asOf.UTC(), window, true)
}

func (s *Store) economicHistory(ctx context.Context, accountID, lane string, asOf time.Time, window time.Duration, excludeControlCloseoutUnknowns bool) (GrowthHistory, error) {
	var result GrowthHistory
	if !validID(accountID) || len(lane) > 1024 {
		return result, fmt.Errorf("economic history query: %w", ErrInvalidInput)
	}
	db, cancel := s.query(ctx)
	defer cancel()
	scope := db.Model(&settlementRow{}).Select("*, true AS in_window").Where("account_id = ?", accountID)
	if lane != "" {
		scope = scope.Where("lane_lookup = ?", s.privateLookup("settlement-lane", "", lane))
	}
	if !asOf.IsZero() {
		scope = scope.Select("*, completed_at <= ? AS in_window", asOf).
			Where("completed_at >= ?", asOf.Add(-window))
	}
	// PostgreSQL numeric keeps both each row's arithmetic and the cumulative
	// sums exact. Scanning an out-of-range aggregate into int64 returns an
	// error instead of wrapping a loss into positive underwriting evidence.
	err := db.Raw(`
		WITH evidence AS (
			SELECT *,
				proceeds_pence::numeric - acquisition_pence - fees_pence - provisions_pence - expenses_pence AS net,
				kind IN ('spot', 'charter', 'freight', 'spot+freight', 'charter+freight', 'recovery', 'aborted', 'unknown') AS commercial,
				kind IN ('spot', 'charter', 'freight', 'spot+freight', 'charter+freight', 'recovery') AS delivery,
				(? AND kind = 'aborted' AND quantity_tons = 0 AND acquisition_pence = 0 AND proceeds_pence = 0
					AND EXISTS (SELECT 1 FROM actions a WHERE a.id = scoped.action_id AND a.account_id = scoped.account_id
						AND a.path IN ('/v1/shipyard/build-ship', '/v1/shipyard/modify-ship'))) AS control_closeout
			FROM (?) AS scoped
		)
		SELECT
			COALESCE(SUM(net) FILTER (WHERE in_window AND commercial AND costs_known), 0) AS commercial_net_pence,
			COALESCE(SUM(net) FILTER (WHERE in_window AND (commercial OR kind = 'relocation') AND (costs_known AND kind <> 'unknown' OR control_closeout)), 0) AS net_pence,
			COALESCE(SUM(LEAST(net, 0)) FILTER (WHERE in_window AND (commercial OR kind = 'relocation') AND (NOT costs_known OR kind = 'unknown') AND NOT control_closeout), 0) AS unknown_loss_pence,
			COUNT(*) FILTER (WHERE in_window AND delivery AND costs_known AND net > 0 AND quantity_tons > 0 AND game_seconds > 0) AS positive_cycles,
			COALESCE(SUM(quantity_tons::numeric) FILTER (WHERE in_window AND delivery AND costs_known AND net > 0 AND quantity_tons > 0 AND game_seconds > 0), 0) AS positive_quantity_tons,
			COALESCE(SUM(net) FILTER (WHERE in_window AND delivery AND costs_known AND net > 0 AND quantity_tons > 0 AND game_seconds > 0), 0) AS positive_net_pence,
			COALESCE(SUM(game_seconds::numeric) FILTER (WHERE in_window AND (commercial OR kind = 'relocation')), 0) AS game_seconds,
			COALESCE(SUM(game_seconds::numeric) FILTER (WHERE in_window AND delivery AND costs_known AND net > 0 AND quantity_tons > 0 AND game_seconds > 0), 0) AS positive_game_seconds,
			COALESCE(BOOL_OR(NOT costs_known OR kind = 'unknown') FILTER (WHERE in_window AND (commercial OR kind = 'relocation') AND NOT control_closeout), false) AS unknown_costs,
			COALESCE(BOOL_OR(game_seconds <= 0) FILTER (WHERE in_window AND (commercial OR kind = 'relocation')), false) AS unmeasured_duration,
			COALESCE(SUM(COALESCE(exploration_expense_pence::numeric,
				acquisition_pence::numeric + fees_pence + provisions_pence + expenses_pence))
				FILTER (WHERE in_window AND kind = 'relocation' AND costs_known), 0) AS probe_expense_pence,
			COALESCE(BOOL_OR(NOT costs_known) FILTER (WHERE in_window AND kind = 'relocation'), false) AS unknown_probe_costs,
			MAX(completed_at) FILTER (WHERE in_window AND (commercial OR kind = 'relocation')) AS last_completion,
			MAX(completed_at) FILTER (WHERE in_window AND kind = 'relocation') AS last_probe_completion,
			COALESCE(BOOL_OR(NOT in_window) FILTER (WHERE commercial OR kind = 'relocation'), false) AS future_completion,
			COALESCE(JSONB_AGG(id) FILTER (WHERE in_window AND NOT costs_known AND control_closeout), '[]'::jsonb) AS control_closeout_ids
		FROM evidence`, excludeControlCloseoutUnknowns, scope).Scan(&result).Error
	return result, err
}

// CapacityDeliveryHistory retains every window delivery, the latest ten known
// lots, and positive proof since the newest ship's creation. Encrypted ship
// identities require decoding proof, but its time bound prevents a lifetime scan.
func (s *Store) CapacityDeliveryHistory(ctx context.Context, accountID string, windowStart, asOf time.Time, newestShipID string, newestCreatedAt time.Time) ([]Settlement, error) {
	if !validID(accountID) || windowStart.IsZero() || asOf.Before(windowStart) || newestShipID != "" && newestCreatedAt.IsZero() {
		return nil, fmt.Errorf("capacity delivery history: %w", ErrInvalidInput)
	}
	db, cancel := s.query(ctx)
	defer cancel()
	deliveryKinds := []string{"spot", "charter", "freight", "spot+freight", "charter+freight", "recovery"}
	latestLots := db.Model(&settlementRow{}).Select("id").
		Where("account_id = ? AND completed_at <= ? AND quantity_tons > 0 AND costs_known", accountID, asOf).
		Where("kind IN ?", deliveryKinds).Order("completed_at DESC, id").Limit(10)
	proofStart := windowStart
	if newestShipID != "" && newestCreatedAt.Before(proofStart) {
		proofStart = newestCreatedAt
	}
	rows, err := db.Model(&settlementRow{}).
		Where("account_id = ? AND completed_at <= ? AND quantity_tons > 0", accountID, asOf).
		Where("kind IN ?", deliveryKinds).
		Where("completed_at >= ? OR id IN (?)", proofStart, latestLots).
		Order("completed_at DESC, id").Rows()
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	deliveries := make([]Settlement, 0, 10)
	proven := newestShipID == ""
	knownLots := 0
	for rows.Next() {
		var stored settlementRow
		if err := db.ScanRows(rows, &stored); err != nil {
			return nil, err
		}
		withinWindow := !stored.CompletedAt.Before(windowStart)
		if !withinWindow && knownLots >= 10 && (proven || stored.CompletedAt.Before(newestCreatedAt)) {
			break
		}
		value, err := s.decodeSettlement(stored)
		if err != nil {
			return nil, err
		}
		var costs int64
		costsKnown := value.CostsKnown
		for _, cost := range []int64{value.AcquisitionPence, value.FeesPence, value.ProvisionsPence, value.ExpensesPence} {
			if cost < 0 || cost > math.MaxInt64-costs {
				costsKnown = false
				break
			}
			costs += cost
		}
		provesNewest := !proven && !value.CompletedAt.Before(newestCreatedAt) && value.ShipID == newestShipID && costsKnown && value.ProceedsPence > costs
		recentLot := value.CostsKnown && knownLots < 10
		if recentLot {
			knownLots++
		}
		if withinWindow || recentLot || provesNewest {
			deliveries = append(deliveries, value)
		}
		proven = proven || provesNewest
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	return deliveries, nil
}

// RelocationProbes looks up only the caller's bounded evidence shortlist.
func (s *Store) RelocationProbes(ctx context.Context, accountID string, lanes []string) ([]string, error) {
	completions, err := s.RelocationProbeCompletions(ctx, accountID, lanes)
	if err != nil {
		return nil, err
	}
	rows := make([]string, 0, len(completions))
	for lane := range completions {
		rows = append(rows, lane)
	}
	sort.Strings(rows)
	return rows, nil
}

// RelocationProbeCompletions returns the latest recorded host UTC completion
// for each requested lane after verifying its encrypted identity.
func (s *Store) RelocationProbeCompletions(ctx context.Context, accountID string, lanes []string) (map[string]time.Time, error) {
	if !validID(accountID) || len(lanes) > 1024 {
		return nil, fmt.Errorf("relocation probe query: %w", ErrInvalidInput)
	}
	for _, lane := range lanes {
		if lane == "" || len(lane) > 1024 {
			return nil, fmt.Errorf("relocation probe lane: %w", ErrInvalidInput)
		}
	}
	rows := make(map[string]time.Time)
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
	for _, row := range stored {
		value, err := s.decodeSettlement(row)
		if err != nil {
			return nil, err
		}
		if value.Lane != originals[row.LaneLookup] {
			return nil, ErrActionConflict
		}
		rows[value.Lane] = value.CompletedAt.UTC()
	}
	return rows, nil
}
