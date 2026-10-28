package store

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"strings"
	"time"

	"gorm.io/gorm"
	"gorm.io/gorm/clause"
)

// PassengerEvidence retains a measured ladder outcome and any observed promise
// leeway or late-payment cut. Lane and kind are private commercial context.
type PassengerEvidence struct {
	ID                     string    `json:"id"`
	AccountID              string    `json:"account_id"`
	Lane                   string    `json:"lane"`
	Kind                   string    `json:"kind"`
	FarePerHeadPence       int64     `json:"fare_per_head_pence"`
	Outcome                string    `json:"outcome"`
	Heads                  int64     `json:"heads"`
	GameHour               float64   `json:"game_hour"`
	LeewayGameHours        *float64  `json:"leeway_game_hours,omitempty"`
	LateCutPerGameDayPence *int64    `json:"late_cut_per_game_day_pence,omitempty"`
	CreatedAt              time.Time `json:"created_at"`
}

func samePassengerEvidence(a, b PassengerEvidence) bool {
	return a.ID == b.ID && a.AccountID == b.AccountID && a.Lane == b.Lane && a.Kind == b.Kind &&
		a.FarePerHeadPence == b.FarePerHeadPence && a.Outcome == b.Outcome && a.Heads == b.Heads &&
		a.GameHour == b.GameHour && sameOptional(a.LeewayGameHours, b.LeewayGameHours) &&
		sameOptional(a.LateCutPerGameDayPence, b.LateCutPerGameDayPence)
}

func (s *Store) PutPassengerEvidence(ctx context.Context, evidence *PassengerEvidence) error {
	if evidence == nil || !validID(evidence.ID) || !validID(evidence.AccountID) ||
		strings.TrimSpace(evidence.Lane) == "" || len(evidence.Lane) > 1024 ||
		evidence.Kind != "travellers" && evidence.Kind != "emigrants" ||
		evidence.Outcome != "boarded" && evidence.Outcome != "refused" ||
		evidence.FarePerHeadPence <= 0 || evidence.Heads <= 0 || evidence.GameHour < 0 ||
		math.IsNaN(evidence.GameHour) || math.IsInf(evidence.GameHour, 0) ||
		evidence.LeewayGameHours != nil && (math.IsNaN(*evidence.LeewayGameHours) || math.IsInf(*evidence.LeewayGameHours, 0)) ||
		evidence.LateCutPerGameDayPence != nil && *evidence.LateCutPerGameDayPence < 0 {
		return fmt.Errorf("passenger evidence fields: %w", ErrInvalidInput)
	}
	row := *evidence
	providedCreatedAt := !row.CreatedAt.IsZero()
	err := s.writeOwned(ctx, row.AccountID, func(tx *gorm.DB) error {
		if row.CreatedAt.IsZero() {
			row.CreatedAt = time.Now().UTC().Truncate(time.Microsecond)
		}
		stored, err := s.encodePassengerEvidence(row)
		if err != nil {
			return err
		}
		result := tx.Clauses(clause.OnConflict{Columns: []clause.Column{{Name: "id"}}, DoNothing: true}).Create(&stored)
		if result.Error != nil {
			return result.Error
		}
		if result.RowsAffected != 0 {
			return nil
		}
		if err := tx.Where("id = ?", row.ID).Take(&stored).Error; err != nil {
			return err
		}
		persisted, err := s.decodePassengerEvidence(stored)
		if err != nil {
			return err
		}
		if !samePassengerEvidence(persisted, row) || providedCreatedAt &&
			!persisted.CreatedAt.Truncate(time.Microsecond).Equal(row.CreatedAt.Truncate(time.Microsecond)) {
			return ErrActionConflict
		}
		row.CreatedAt = persisted.CreatedAt
		return nil
	})
	if err == nil {
		evidence.CreatedAt = row.CreatedAt
	}
	return err
}

func (s *Store) PassengerEvidenceHistory(ctx context.Context, accountID, lane string, limit int) ([]PassengerEvidence, error) {
	if !validID(accountID) || len(lane) > 1024 || limit < 1 || limit > MaxHistoryLimit {
		return nil, fmt.Errorf("passenger evidence history query: %w", ErrInvalidInput)
	}
	db, cancel := s.query(ctx)
	defer cancel()
	db = db.Where("account_id = ?", accountID)
	if lane != "" {
		db = db.Where("lane_lookup = ?", s.privateLookup("passenger-lane", accountID, lane))
	}
	var stored []passengerEvidenceRow
	if err := db.Order("game_hour DESC, created_at DESC, id").Limit(limit).Find(&stored).Error; err != nil {
		return nil, err
	}
	rows := make([]PassengerEvidence, 0, len(stored))
	for _, privateRow := range stored {
		value, err := s.decodePassengerEvidence(privateRow)
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

// PassengerEventsSince bounds ordinary history to seven real days, extending
// that horizon for this account's open receipt or active passage recovery.
// Source-time attribution remains the caller's responsibility for replayed SSE.
func (s *Store) PassengerEventsSince(ctx context.Context, accountID string, since time.Time) ([]Event, error) {
	if !validID(accountID) {
		return nil, fmt.Errorf("passenger events query: %w", ErrInvalidInput)
	}
	through := time.Now().UTC()
	if cutoff := through.Add(-PassengerReceiptLifetime); since.Before(cutoff) {
		windows, err := s.passengerRecoveryWindows(ctx, accountID)
		if err != nil {
			return nil, err
		}
		if recovery, exists := windows[accountID]; exists && recovery.Before(cutoff) {
			cutoff = recovery
		}
		if since.Before(cutoff) {
			since = cutoff
		}
	}
	return s.passengerEventsRange(ctx, accountID, since, through)
}

func (s *Store) passengerEventsRange(ctx context.Context, accountID string, since, through time.Time) ([]Event, error) {
	if !validID(accountID) {
		return nil, fmt.Errorf("passenger events query: %w", ErrInvalidInput)
	}
	db, cancel := s.query(ctx)
	defer cancel()
	kinds := []string{"passengers_boarded", "passengers_paid", "passengers_lost"}
	highWater := db.Model(&Event{}).Select("COALESCE(MAX(id), 0)").
		Where("account_id = ? AND event_type IN ? AND created_at <= ?", accountID, kinds, through)
	if !since.IsZero() {
		highWater = highWater.Where("created_at >= ?", since)
	}
	var throughID int64
	if err := highWater.Scan(&throughID).Error; err != nil {
		return nil, err
	}
	events := make([]Event, 0)
	var afterAt time.Time
	var afterID int64
	for throughID != 0 {
		query := db.Where("account_id = ? AND event_type IN ? AND id <= ? AND created_at <= ?", accountID, kinds, throughID, through)
		if !since.IsZero() {
			query = query.Where("created_at >= ?", since)
		}
		if afterID != 0 {
			query = query.Where("(created_at, id) > (?, ?)", afterAt, afterID)
		}
		var batch []Event
		if err := query.Order("created_at, id").Limit(MaxHistoryLimit).Find(&batch).Error; err != nil {
			return nil, err
		}
		events = append(events, batch...)
		if len(batch) < MaxHistoryLimit {
			break
		}
		last := batch[len(batch)-1]
		afterAt, afterID = last.CreatedAt, last.ID
	}
	return events, nil
}

func validatePassageSettlement(tx *gorm.DB, settlement Settlement) error {
	var event Event
	if err := tx.Clauses(clause.Locking{Strength: "UPDATE"}).
		Where("account_id = ? AND event_id = ?", settlement.AccountID, settlement.EventID).Take(&event).Error; err != nil {
		if errors.Is(err, gorm.ErrRecordNotFound) {
			return ErrNotFound
		}
		return err
	}
	var linked int64
	if err := tx.Model(&passengerReceiptEventRow{}).
		Where("account_id = ? AND event_id = ? AND settlement_id <> ?", settlement.AccountID, settlement.EventID, settlement.ID).
		Count(&linked).Error; err != nil {
		return err
	}
	if linked != 0 {
		return ErrActionConflict
	}
	switch event.EventType {
	case "passengers_paid":
		// Some retained producers wrap the payload with its source timestamp.
		var envelope struct {
			Payload json.RawMessage `json:"payload"`
		}
		if err := json.Unmarshal(event.Payload, &envelope); err != nil {
			return fmt.Errorf("passage payment evidence: %w", ErrActionConflict)
		}
		payload := event.Payload
		if len(envelope.Payload) != 0 {
			payload = envelope.Payload
		}
		var paid struct {
			FarePaidPence *int64 `json:"fare_paid_pence"`
		}
		if err := json.Unmarshal(payload, &paid); err != nil || paid.FarePaidPence == nil ||
			*paid.FarePaidPence < 0 || settlement.ProceedsPence != *paid.FarePaidPence {
			return fmt.Errorf("passage proceeds differ from payment evidence: %w", ErrActionConflict)
		}
	case "passengers_lost":
		if settlement.ProceedsPence != 0 {
			return fmt.Errorf("lost passengers cannot produce fare proceeds: %w", ErrActionConflict)
		}
	default:
		return fmt.Errorf("passage requires a paid or lost passenger event: %w", ErrActionConflict)
	}
	return nil
}
