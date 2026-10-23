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

func (s *Store) RecordRequest(ctx context.Context, accountID, method, path string, status int, duration time.Duration, request, response []byte, errText string) error {
	if !validID(accountID) || len(method) > 16 || duration < 0 || status != 0 && (status < 100 || status > 599) {
		return fmt.Errorf("request metadata: %w", ErrInvalidInput)
	}
	secrets, err := s.secrets(ctx, accountID)
	if err != nil {
		return err
	}
	requestJSON, err := redactJSON(request, secrets, maxDiagnosticBytes)
	if err != nil {
		return err
	}
	responseJSON, err := redactJSON(response, secrets, maxDiagnosticBytes)
	if err != nil {
		return err
	}
	now := time.Now().UTC()
	row := Request{
		RequestSummary: RequestSummary{
			AccountID:  accountID,
			Method:     strings.ToUpper(method),
			Path:       redactPath(path, secrets),
			Status:     status,
			DurationMS: duration.Milliseconds(),
			Error:      boundedText(scrubText(errText, secrets), 4096),
			CreatedAt:  now,
		},
		Request:  requestJSON,
		Response: responseJSON,
	}
	counters := []metricCounter{
		{AccountID: accountID, Name: "http_requests_total", Value: 1, RecordedAt: now},
		{AccountID: accountID, Name: "http_request_duration_ms_total", Value: float64(row.DurationMS), RecordedAt: now},
	}
	if status == 0 || status >= 400 || errText != "" {
		counters = append(counters, metricCounter{AccountID: accountID, Name: "http_request_errors_total", Value: 1, RecordedAt: now})
	}
	db, cancel := s.query(ctx)
	defer cancel()
	return db.Transaction(func(tx *gorm.DB) error {
		if err := tx.Create(&row).Error; err != nil {
			return err
		}
		return tx.Clauses(clause.OnConflict{
			Columns: []clause.Column{{Name: "account_id"}, {Name: "name"}},
			DoUpdates: clause.Assignments(map[string]any{
				"value":       gorm.Expr("metric_counters.value + EXCLUDED.value"),
				"recorded_at": now,
			}),
		}).Create(&counters).Error
	})
}

func (s *Store) RecordEvent(ctx context.Context, accountID, id, eventType string, payload json.RawMessage) error {
	if !validID(accountID) || !validID(id) || eventType == "" || len(eventType) > 255 {
		return fmt.Errorf("event metadata: %w", ErrInvalidInput)
	}
	secrets, err := s.secrets(ctx, accountID)
	if err != nil {
		return err
	}
	redactedPayload, err := redactJSON(payload, secrets, maxDiagnosticBytes)
	if err != nil {
		return err
	}
	return s.writeOwned(ctx, accountID, func(tx *gorm.DB) error {
		now := time.Now().UTC()
		row := Event{AccountID: accountID, EventID: id, EventType: eventType, Payload: redactedPayload, CreatedAt: now}
		result := tx.Clauses(clause.OnConflict{
			Columns:   []clause.Column{{Name: "account_id"}, {Name: "event_id"}},
			DoNothing: true,
		}).Create(&row)
		if result.Error != nil {
			return result.Error
		}
		if result.RowsAffected == 0 {
			if err := tx.Select("id").Where("account_id = ? AND event_id = ?", accountID, id).Take(&row).Error; err != nil {
				return err
			}
		}
		// Compare local insertion sequence, never the opaque upstream frame ID.
		// This also permits retained duplicates to advance a deliberately reset cursor.
		return tx.Clauses(clause.OnConflict{
			Columns:   []clause.Column{{Name: "account_id"}},
			DoUpdates: clause.AssignmentColumns([]string{"event_id", "event_sequence", "updated_at"}),
			Where: clause.Where{Exprs: []clause.Expression{clause.Expr{
				SQL: "event_cursors.event_sequence < EXCLUDED.event_sequence",
			}}},
		}).Create(&cursorRow{AccountID: accountID, EventID: id, EventSequence: row.ID, UpdatedAt: now}).Error
	})
}

func (s *Store) Cursor(ctx context.Context, accountID string) (string, error) {
	if !validID(accountID) {
		return "", fmt.Errorf("account ID: %w", ErrInvalidInput)
	}
	db, cancel := s.query(ctx)
	defer cancel()
	var row cursorRow
	if err := db.First(&row, "account_id = ?", accountID).Error; err != nil {
		if errors.Is(err, gorm.ErrRecordNotFound) {
			return "", nil
		}
		return "", err
	}
	return row.EventID, nil
}

// ResetCursor replaces a rejected cursor with the upstream retained-log origin.
// Event IDs are opaque: never increment, parse, or sort them as local numbers.
func (s *Store) ResetCursor(ctx context.Context, accountID string) error {
	if !validID(accountID) {
		return fmt.Errorf("account ID: %w", ErrInvalidInput)
	}
	return s.writeOwned(ctx, accountID, func(tx *gorm.DB) error {
		return tx.Clauses(clause.OnConflict{
			Columns:   []clause.Column{{Name: "account_id"}},
			DoUpdates: clause.AssignmentColumns([]string{"event_id", "event_sequence", "updated_at"}),
		}).Create(&cursorRow{AccountID: accountID, EventID: "0", UpdatedAt: time.Now().UTC()}).Error
	})
}

func (s *Store) RecordDecision(ctx context.Context, accountID, shipID, kind, reason string, details any) error {
	if !validID(accountID) || len(shipID) > 255 || kind == "" || len(kind) > 255 {
		return fmt.Errorf("decision metadata: %w", ErrInvalidInput)
	}
	secrets, err := s.secrets(ctx, accountID)
	if err != nil {
		return err
	}
	encoded, err := json.Marshal(details)
	if err != nil {
		return fmt.Errorf("encode decision: %w", err)
	}
	encoded, err = redactJSON(encoded, secrets, maxDiagnosticBytes)
	if err != nil {
		return err
	}
	row := Decision{
		AccountID: accountID,
		ShipID:    shipID,
		Kind:      scrubText(kind, secrets),
		Reason:    boundedText(scrubText(reason, secrets), 8192),
		Details:   encoded,
		CreatedAt: time.Now().UTC(),
	}
	db, cancel := s.query(ctx)
	defer cancel()
	return db.Create(&row).Error
}

func (s *Store) RecordMetric(ctx context.Context, accountID, name string, value float64) error {
	if !validID(accountID) || strings.TrimSpace(name) == "" || len(name) > 128 || math.IsNaN(value) || math.IsInf(value, 0) {
		return fmt.Errorf("metric fields: %w", ErrInvalidInput)
	}
	db, cancel := s.query(ctx)
	defer cancel()
	return db.Create(&Metric{AccountID: accountID, Name: name, Value: value, RecordedAt: time.Now().UTC()}).Error
}

func (s *Store) Prune(ctx context.Context) error {
	ctx, cancel := context.WithTimeout(ctx, time.Minute)
	defer cancel()
	// Only diagnostic tables are eligible. Action journals, mission state,
	// snapshots, cursors, settlements and metric history are operational data.
	for _, table := range []string{"requests", "events", "decisions"} {
		query := "DELETE FROM " + table + " WHERE id IN (SELECT id FROM " + table +
			" WHERE created_at < CURRENT_TIMESTAMP - INTERVAL '7 days' ORDER BY created_at LIMIT 10000)"
		for {
			result := s.db.WithContext(ctx).Exec(query)
			if result.Error != nil {
				return fmt.Errorf("prune %s: %w", table, result.Error)
			}
			if result.RowsAffected < 10000 {
				break
			}
		}
	}
	return nil
}
