package store

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"time"

	"gorm.io/gorm"
	"gorm.io/gorm/clause"
)

const (
	PassengerReceiptRetryInterval = 15 * time.Minute
	PassengerReceiptLifetime      = 7 * 24 * time.Hour
	PassengerReceiptBatchLimit    = 32
)

// PassengerReceipt keeps the original accounting basis and private recovery
// context after REST proves completion but terminal passenger events are missing.
type PassengerReceipt struct {
	Settlement        Settlement
	CostsKnown        bool
	TimeScale         int64
	Context           json.RawMessage
	CreatedAt         time.Time
	NextScanAt        time.Time
	ExpiresAt         time.Time
	SettlementMissing bool
	ReadError         error // Due rows retain their trusted ID even when private context cannot be decoded.
}

type passengerReceiptRow struct {
	ID                string `gorm:"primaryKey"`
	AccountID         string
	ContextCipher     []byte
	Resolved          bool
	CreatedAt         time.Time
	NextScanAt        time.Time
	ExpiresAt         time.Time
	ClosedAt          *time.Time
	CloseReason       string
	SettlementMissing bool `gorm:"->"`
}

func (passengerReceiptRow) TableName() string { return "passenger_receipts" }

type passengerReceiptPayload struct {
	Settlement Settlement
	CostsKnown bool
	TimeScale  int64
	Context    []byte
}

type passengerReceiptEventRow struct {
	AccountID    string `gorm:"primaryKey"`
	EventID      string `gorm:"primaryKey"`
	SettlementID string
}

func (passengerReceiptEventRow) TableName() string { return "passenger_receipt_events" }

func (s *Store) decodePassengerReceipt(row passengerReceiptRow) (PassengerReceipt, error) {
	var payload passengerReceiptPayload
	if err := s.decodePrivate(row.ContextCipher, &payload, "passenger_receipts", "context", row.AccountID, row.ID); err != nil {
		return PassengerReceipt{}, err
	}
	if payload.Settlement.ID != row.ID || payload.Settlement.AccountID != row.AccountID {
		return PassengerReceipt{}, ErrActionConflict
	}
	return PassengerReceipt{Settlement: payload.Settlement, CostsKnown: payload.CostsKnown,
		TimeScale: payload.TimeScale, Context: payload.Context, CreatedAt: row.CreatedAt,
		NextScanAt: row.NextScanAt, ExpiresAt: row.ExpiresAt, SettlementMissing: row.SettlementMissing}, nil
}

// PutPendingPassengerReceipt archives before inserting the unknown settlement.
// A crash between these idempotent writes leaves the mission and archive able to
// finish the insert; callers release the mission only after both have succeeded.
func (s *Store) PutPendingPassengerReceipt(ctx context.Context, receipt *PassengerReceipt) error {
	if receipt == nil || !validID(receipt.Settlement.ID) || !validID(receipt.Settlement.AccountID) ||
		receipt.Settlement.Kind != "passage" || !receipt.Settlement.ProceedsUnknown || receipt.Settlement.CostsKnown ||
		receipt.Settlement.ProceedsPence != 0 || receipt.Settlement.EventID != "" || receipt.Settlement.ActionID != "" ||
		receipt.Settlement.StartedAt.IsZero() || receipt.Settlement.CompletedAt.Before(receipt.Settlement.StartedAt) || receipt.TimeScale <= 0 ||
		len(receipt.Context) > MaxSnapshotBytes || !json.Valid(receipt.Context) {
		return fmt.Errorf("pending passenger receipt fields: %w", ErrInvalidInput)
	}
	if _, err := validateSettlement(&receipt.Settlement); err != nil {
		return err
	}
	closed := false
	err := s.writeOwned(ctx, receipt.Settlement.AccountID, func(tx *gorm.DB) error {
		created := receipt.CreatedAt
		if created.IsZero() {
			created = time.Now().UTC()
		}
		payload := passengerReceiptPayload{Settlement: receipt.Settlement, CostsKnown: receipt.CostsKnown,
			TimeScale: receipt.TimeScale, Context: receipt.Context}
		row := passengerReceiptRow{ID: receipt.Settlement.ID, AccountID: receipt.Settlement.AccountID,
			CreatedAt: created, NextScanAt: created, ExpiresAt: created.Add(PassengerReceiptLifetime)}
		var err error
		row.ContextCipher, err = s.encryptPrivate(payload, "passenger_receipts", "context", row.AccountID, row.ID)
		if err != nil {
			return err
		}
		result := tx.Clauses(clause.OnConflict{Columns: []clause.Column{{Name: "id"}}, DoNothing: true}).Create(&row)
		if result.Error != nil {
			return result.Error
		}
		if result.RowsAffected == 0 {
			if err := tx.Where("id = ?", row.ID).Take(&row).Error; err != nil {
				return err
			}
			stored, err := s.decodePassengerReceipt(row)
			if err != nil {
				return err
			}
			if !sameSettlement(stored.Settlement, receipt.Settlement) || stored.CostsKnown != receipt.CostsKnown ||
				stored.TimeScale != receipt.TimeScale || !bytes.Equal(stored.Context, receipt.Context) ||
				!receipt.CreatedAt.IsZero() && !stored.CreatedAt.Truncate(time.Microsecond).Equal(receipt.CreatedAt.Truncate(time.Microsecond)) {
				return ErrActionConflict
			}
		}
		receipt.CreatedAt, receipt.NextScanAt, receipt.ExpiresAt = row.CreatedAt, row.NextScanAt, row.ExpiresAt
		closed = row.Resolved || row.ClosedAt != nil
		return nil
	})
	if err != nil || closed {
		return err
	}
	return s.PutSettlement(ctx, &receipt.Settlement)
}

// PendingPassengerReceipts returns a bounded batch of due receipts. Closed and
// resolved archives stay available in history but never enter the scan again.
func (s *Store) PendingPassengerReceipts(ctx context.Context, accountID string) ([]PassengerReceipt, error) {
	return s.DuePassengerReceipts(ctx, accountID, time.Now().UTC(), PassengerReceiptBatchLimit)
}

func (s *Store) DuePassengerReceipts(ctx context.Context, accountID string, now time.Time, limit int) ([]PassengerReceipt, error) {
	if !validID(accountID) || now.IsZero() || limit < 1 || limit > MaxHistoryLimit {
		return nil, fmt.Errorf("pending passenger receipts query: %w", ErrInvalidInput)
	}
	db, cancel := s.query(ctx)
	defer cancel()
	var rows []passengerReceiptRow
	if err := db.Model(&passengerReceiptRow{}).
		Select(`passenger_receipts.*, NOT EXISTS (
			SELECT 1 FROM settlements WHERE settlements.id = passenger_receipts.id
			AND settlements.account_id = passenger_receipts.account_id) AS settlement_missing`).
		Where("passenger_receipts.account_id = ? AND NOT resolved AND closed_at IS NULL AND next_scan_at <= ?", accountID, now).
		Order("next_scan_at, created_at, id").Limit(limit).Find(&rows).Error; err != nil {
		return nil, err
	}
	receipts := make([]PassengerReceipt, 0, len(rows))
	for _, row := range rows {
		// Expired and unreadable rows need only their trusted relational identity
		// to close. One poisoned ciphertext must not conceal the healthy batch.
		value := PassengerReceipt{Settlement: Settlement{ID: row.ID, AccountID: row.AccountID},
			CreatedAt: row.CreatedAt, NextScanAt: row.NextScanAt, ExpiresAt: row.ExpiresAt,
			SettlementMissing: row.SettlementMissing}
		if now.Before(row.ExpiresAt) || row.SettlementMissing {
			decoded, err := s.decodePassengerReceipt(row)
			if err != nil {
				value.ReadError = err
			} else {
				value = decoded
			}
		}
		receipts = append(receipts, value)
	}
	return receipts, nil
}

// DeferPassengerReceipt changes only scan metadata, never its encrypted basis.
func (s *Store) DeferPassengerReceipt(ctx context.Context, accountID, id string, now time.Time) error {
	if !validID(accountID) || !validID(id) || now.IsZero() {
		return fmt.Errorf("passenger receipt retry fields: %w", ErrInvalidInput)
	}
	return s.writeOwned(ctx, accountID, func(tx *gorm.DB) error {
		return tx.Model(&passengerReceiptRow{}).
			Where("id = ? AND account_id = ? AND NOT resolved AND closed_at IS NULL", id, accountID).
			Update("next_scan_at", gorm.Expr("GREATEST(next_scan_at, LEAST(?, expires_at))", now.Add(PassengerReceiptRetryInterval))).Error
	})
}

// ClosePassengerReceipt retains unknown accounting without inventing a terminal
// event or a zero payment. Closure is durable and cannot reopen the scan.
func (s *Store) ClosePassengerReceipt(ctx context.Context, accountID, id, reason string, now time.Time) error {
	if !validID(accountID) || !validID(id) || now.IsZero() || reason != "expired" && reason != "quarantined" {
		return fmt.Errorf("passenger receipt closure fields: %w", ErrInvalidInput)
	}
	return s.writeOwned(ctx, accountID, func(tx *gorm.DB) error {
		query := tx.Model(&passengerReceiptRow{}).
			Where("id = ? AND account_id = ? AND NOT resolved AND closed_at IS NULL", id, accountID)
		if reason == "expired" {
			query = query.Where("expires_at <= ?", now)
		}
		result := query.Updates(map[string]any{"closed_at": now, "close_reason": reason})
		if result.Error != nil || result.RowsAffected != 0 {
			return result.Error
		}
		var row passengerReceiptRow
		if err := tx.Select("closed_at", "close_reason").Where("id = ? AND account_id = ?", id, accountID).Take(&row).Error; err != nil {
			if errors.Is(err, gorm.ErrRecordNotFound) {
				return ErrNotFound
			}
			return err
		}
		if row.ClosedAt == nil || row.CloseReason != reason {
			return ErrActionConflict
		}
		return nil
	})
}

func passengerReceiptEventPayload(event Event, archivedStarted, archivedCompleted time.Time) (string, int64, time.Time, error) {
	var envelope struct {
		Payload   json.RawMessage `json:"payload"`
		Timestamp time.Time       `json:"timestamp"`
	}
	if err := json.Unmarshal(event.Payload, &envelope); err != nil {
		return "", 0, time.Time{}, ErrActionConflict
	}
	raw := event.Payload
	if len(envelope.Payload) != 0 {
		raw = envelope.Payload
	}
	var payload struct {
		ShipID        string    `json:"ship_id"`
		GroupID       string    `json:"group_id"`
		FarePaidPence *int64    `json:"fare_paid_pence"`
		Timestamp     time.Time `json:"timestamp"`
	}
	if err := json.Unmarshal(raw, &payload); err != nil {
		return "", 0, time.Time{}, ErrActionConflict
	}
	completed := payload.Timestamp
	if completed.IsZero() {
		completed = envelope.Timestamp
	}
	if completed.IsZero() {
		if payload.GroupID == "" {
			return "", 0, time.Time{}, ErrActionConflict
		}
		completed = event.CreatedAt
	}
	if payload.GroupID == "" && (!completed.After(archivedStarted) || completed.After(archivedCompleted)) {
		return "", 0, time.Time{}, ErrActionConflict
	}
	var proceeds int64
	switch event.EventType {
	case "passengers_paid":
		if payload.FarePaidPence == nil || *payload.FarePaidPence < 0 {
			return "", 0, time.Time{}, ErrActionConflict
		}
		proceeds = *payload.FarePaidPence
	case "passengers_lost":
	default:
		return "", 0, time.Time{}, ErrActionConflict
	}
	return payload.ShipID, proceeds, completed, nil
}

// ResolvePassengerReceipt is the only permitted unknown-to-real transition.
// Every retained event is linked exactly once; private accounting identity and
// cost allocations remain unchanged, and profit is posted only on this upgrade.
func (s *Store) ResolvePassengerReceipt(ctx context.Context, settlement *Settlement, eventIDs []string) error {
	if settlement == nil || !validID(settlement.ID) || !validID(settlement.AccountID) || settlement.Kind != "passage" ||
		settlement.ProceedsUnknown || len(eventIDs) == 0 || settlement.EventID != eventIDs[0] {
		return fmt.Errorf("passenger receipt resolution fields: %w", ErrInvalidInput)
	}
	if _, err := validateSettlement(settlement); err != nil {
		return err
	}
	seen := make(map[string]bool, len(eventIDs))
	for _, id := range eventIDs {
		if !validID(id) || seen[id] {
			return fmt.Errorf("passenger receipt event identities: %w", ErrInvalidInput)
		}
		seen[id] = true
	}
	return s.writeOwned(ctx, settlement.AccountID, func(tx *gorm.DB) error {
		var archive passengerReceiptRow
		if err := tx.Clauses(clause.Locking{Strength: "UPDATE"}).Where("id = ? AND account_id = ?", settlement.ID, settlement.AccountID).Take(&archive).Error; err != nil {
			if errors.Is(err, gorm.ErrRecordNotFound) {
				return ErrNotFound
			}
			return err
		}
		if archive.ClosedAt != nil {
			return ErrActionConflict
		}
		receipt, err := s.decodePassengerReceipt(archive)
		if err != nil {
			return err
		}
		basis := receipt.Settlement
		candidate := *settlement
		candidate.EventID, candidate.ProceedsPence, candidate.ProceedsUnknown = basis.EventID, basis.ProceedsPence, basis.ProceedsUnknown
		candidate.CostsKnown, candidate.CompletedAt, candidate.GameSeconds = basis.CostsKnown, basis.CompletedAt, basis.GameSeconds
		if !sameSettlement(candidate, basis) || settlement.CostsKnown != receipt.CostsKnown {
			return ErrActionConflict
		}
		var stored settlementRow
		if err := tx.Clauses(clause.Locking{Strength: "UPDATE"}).Where("id = ? AND account_id = ?", settlement.ID, settlement.AccountID).Take(&stored).Error; err != nil {
			return err
		}
		persisted, err := s.decodeSettlement(stored)
		if err != nil {
			return err
		}
		if archive.Resolved {
			if !sameSettlement(persisted, *settlement) {
				return ErrActionConflict
			}
			var total, matched int64
			proofs := tx.Model(&passengerReceiptEventRow{}).Where("account_id = ? AND settlement_id = ?", settlement.AccountID, settlement.ID)
			if err := proofs.Count(&total).Error; err != nil {
				return err
			}
			if err := proofs.Session(&gorm.Session{}).Where("event_id IN ?", eventIDs).Count(&matched).Error; err != nil {
				return err
			}
			if total != int64(len(eventIDs)) || matched != total {
				return ErrActionConflict
			}
			settlement.CreatedAt = persisted.CreatedAt
			return nil
		}
		var retained []Event
		if err := tx.Clauses(clause.Locking{Strength: "UPDATE"}).
			Where("account_id = ? AND event_id IN ?", settlement.AccountID, eventIDs).
			Order("event_id").Find(&retained).Error; err != nil {
			return err
		}
		if len(retained) != len(eventIDs) {
			return ErrNotFound
		}
		events := make(map[string]Event, len(retained))
		for _, event := range retained {
			events[event.EventID] = event
		}
		var total int64
		var completed time.Time
		for _, id := range eventIDs {
			event := events[id]
			ship, proceeds, at, err := passengerReceiptEventPayload(event, basis.StartedAt, basis.CompletedAt)
			if err != nil || ship != basis.ShipID || at.Before(basis.StartedAt) || total > math.MaxInt64-proceeds {
				return ErrActionConflict
			}
			total += proceeds
			if completed.IsZero() || at.After(completed) {
				completed = at
			}
			var used int64
			if err := tx.Model(&settlementRow{}).Where("account_id = ? AND event_id = ? AND id <> ?", settlement.AccountID, id, settlement.ID).Count(&used).Error; err != nil {
				return err
			}
			if used != 0 {
				return ErrActionConflict
			}
			link := passengerReceiptEventRow{AccountID: settlement.AccountID, EventID: id, SettlementID: settlement.ID}
			if err := tx.Clauses(clause.OnConflict{DoNothing: true}).Create(&link).Error; err != nil {
				return err
			}
			if err := tx.Where("account_id = ? AND event_id = ?", settlement.AccountID, id).Take(&link).Error; err != nil {
				return err
			}
			if link.SettlementID != settlement.ID {
				return ErrActionConflict
			}
		}
		seconds := max(int64(0), int64(completed.Sub(basis.StartedAt).Seconds()*float64(receipt.TimeScale)))
		if settlement.ProceedsPence != total || !settlement.CompletedAt.Truncate(time.Microsecond).Equal(completed.Truncate(time.Microsecond)) || settlement.GameSeconds != seconds {
			return ErrActionConflict
		}
		var count int64
		if err := tx.Model(&passengerReceiptEventRow{}).Where("settlement_id = ?", settlement.ID).Count(&count).Error; err != nil {
			return err
		}
		if count != int64(len(eventIDs)) {
			return ErrActionConflict
		}
		if !sameSettlement(persisted, basis) {
			return ErrActionConflict
		}
		settlement.CreatedAt = persisted.CreatedAt
		encoded, err := s.encodeSettlement(*settlement)
		if err != nil {
			return err
		}
		if err := tx.Model(&settlementRow{}).Where("id = ? AND account_id = ?", settlement.ID, settlement.AccountID).Select("*").Updates(&encoded).Error; err != nil {
			if errors.Is(err, gorm.ErrDuplicatedKey) {
				return ErrActionConflict
			}
			return err
		}
		if err := tx.Model(&passengerReceiptRow{}).Where("id = ? AND account_id = ?", settlement.ID, settlement.AccountID).Update("resolved", true).Error; err != nil {
			return err
		}
		if !settlement.CostsKnown {
			return nil
		}
		net := total - settlement.AcquisitionPence - settlement.FeesPence - settlement.ProvisionsPence - settlement.ExpensesPence
		return tx.Create(&Metric{AccountID: settlement.AccountID, Name: "realized_profit_pence", Value: float64(net), RecordedAt: settlement.CompletedAt}).Error
	})
}
