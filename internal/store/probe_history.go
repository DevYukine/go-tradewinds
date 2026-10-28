package store

import (
	"context"
	"fmt"
	"time"

	"gorm.io/gorm"
)

// ProbeHistoryCursor advances by insertion order, not mission completion time:
// recovery can record a settlement long after its mission completed.
type ProbeHistoryCursor struct {
	SettlementAt time.Time `json:"settlement_at"`
	SettlementID string    `json:"settlement_id"`
	DepartureID  int64     `json:"departure_id"`
}

// ProbeHistorySince supplies verified private accounting and retained departure
// evidence to the engine's durable, incremental sourcing summary.
func (s *Store) ProbeHistorySince(ctx context.Context, accountID string, cursor ProbeHistoryCursor) ([]Settlement, []Event, error) {
	if !validID(accountID) || cursor.DepartureID < 0 || cursor.SettlementID != "" && !validID(cursor.SettlementID) {
		return nil, nil, fmt.Errorf("probe history query: %w", ErrInvalidInput)
	}
	db, cancel := s.query(ctx)
	defer cancel()
	query := db.Where("account_id = ? AND kind = ?", accountID, "relocation")
	if !cursor.SettlementAt.IsZero() {
		query = query.Where("(created_at, id) > (?, ?)", cursor.SettlementAt, cursor.SettlementID)
	}
	var stored []settlementRow
	if err := query.Order("created_at, id").Find(&stored).Error; err != nil {
		return nil, nil, err
	}
	settlements := make([]Settlement, 0, len(stored))
	for _, row := range stored {
		value, err := s.decodeSettlement(row)
		if err != nil {
			return nil, nil, err
		}
		settlements = append(settlements, value)
	}
	var departures []Event
	if err := db.Where("account_id = ? AND event_type = ? AND id > ?", accountID, "ship_departed", cursor.DepartureID).
		Order("id").Find(&departures).Error; err != nil {
		return nil, nil, err
	}
	return settlements, departures, nil
}

// SourcingBudgetSettlements returns the accounting window plus earlier probes
// that can precede a delivery's start by at most seven real days. It rebuilds
// attribution without decrypting an account's entire lifetime history.
func (s *Store) SourcingBudgetSettlements(ctx context.Context, accountID string, from, asOf time.Time) ([]Settlement, error) {
	if !validID(accountID) || from.IsZero() || asOf.IsZero() || from.After(asOf) {
		return nil, fmt.Errorf("sourcing budget query: %w", ErrInvalidInput)
	}
	db, cancel := s.query(ctx)
	defer cancel()
	probeFrom, err := sourcingBudgetProbeFrom(db, accountID, from, asOf)
	if err != nil {
		return nil, err
	}
	var stored []settlementRow
	if err := db.Where("account_id = ? AND completed_at <= ? AND (completed_at >= ? OR (kind = ? AND completed_at >= ?))",
		accountID, asOf, from, "relocation", probeFrom).Order("completed_at, id").Find(&stored).Error; err != nil {
		return nil, err
	}
	settlements := make([]Settlement, 0, len(stored))
	for _, row := range stored {
		value, err := s.decodeSettlement(row)
		if err != nil {
			return nil, err
		}
		settlements = append(settlements, value)
	}
	return settlements, nil
}

func sourcingBudgetProbeFrom(db *gorm.DB, accountID string, from, asOf time.Time) (time.Time, error) {
	earliestStart := from
	if err := db.Model(&settlementRow{}).Select("COALESCE(MIN(started_at), ?)", from).
		Where("account_id = ? AND completed_at >= ? AND completed_at <= ? AND started_at > ? AND costs_known AND quantity_tons > 0 AND kind IN ?",
			accountID, from, asOf, time.Time{}, []string{"spot", "charter", "freight", "spot+freight", "charter+freight", "recovery"}).
		Row().Scan(&earliestStart); err != nil {
		return time.Time{}, err
	}
	probeFrom := earliestStart.Add(-7 * 24 * time.Hour)
	if from.Before(probeFrom) {
		probeFrom = from
	}
	return probeFrom, nil
}
