package store

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"strconv"
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
	err = db.Transaction(func(tx *gorm.DB) error {
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
	if err != nil {
		return err
	}
	// Standings are derived from the committed raw response in their own
	// transaction, so a payload the parser rejects keeps the raw row needed
	// to diagnose it.
	if row.Method == "GET" && row.Path == "/v1/world/leaderboard" && status == 200 && errText == "" {
		err = db.Transaction(func(tx *gorm.DB) error {
			return recordStandings(tx, accountID, row.Response, now)
		})
		if err != nil {
			return fmt.Errorf("standings history: %w", err)
		}
	}
	return nil
}

// BoundEventPayload replaces numeric tokens or nesting that can exhaust
// PostgreSQL with a gap. Event identity, type and receipt time remain intact.
func BoundEventPayload(payload json.RawMessage) json.RawMessage {
	decoder := json.NewDecoder(bytes.NewReader(payload))
	decoder.UseNumber()
	depth := 0
	for {
		token, err := decoder.Token()
		if err != nil {
			// The caller's normal redaction still handles malformed JSON.
			return payload
		}
		if number, ok := token.(json.Number); ok && !numberWithinStorageBounds(number) {
			return json.RawMessage(`{"gap":true,"reason":"numeric token outside storage bounds"}`)
		}
		if delimiter, ok := token.(json.Delim); ok {
			switch delimiter {
			case '{', '[':
				depth++
				if depth > 64 {
					return json.RawMessage(`{"gap":true,"reason":"JSON nesting outside storage bounds"}`)
				}
			case '}', ']':
				depth--
			}
		}
	}
}

// numberWithinStorageBounds keeps ordinary floats such as 2.5e-7 while
// rejecting tokens like 1e130000 or 0e-16000, which PostgreSQL renders as
// one digit per unit of exponent.
func numberWithinStorageBounds(number json.Number) bool {
	if len(number) > 32 {
		return false
	}
	index := strings.IndexAny(string(number), "eE")
	if index < 0 {
		return true
	}
	exponent, err := strconv.Atoi(string(number[index+1:]))
	return err == nil && exponent >= -32 && exponent <= 32
}

func (s *Store) RecordEvent(ctx context.Context, accountID, id, eventType string, payload json.RawMessage) error {
	if !validID(accountID) || !validID(id) || eventType == "" || len(eventType) > 255 {
		return fmt.Errorf("event metadata: %w", ErrInvalidInput)
	}
	secrets, err := s.secrets(ctx, accountID)
	if err != nil {
		return err
	}
	redactedPayload, err := redactJSON(BoundEventPayload(payload), secrets, maxDiagnosticBytes)
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
		ShipID:    scrubText(shipID, secrets),
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
	for {
		result := s.db.WithContext(ctx).Exec("DELETE FROM passenger_quarantine_archives WHERE (account_id, ship_lookup) IN (SELECT account_id, ship_lookup FROM passenger_quarantine_archives WHERE closed_at < CURRENT_TIMESTAMP - INTERVAL '7 days' ORDER BY closed_at LIMIT 1000)")
		if result.Error != nil {
			return fmt.Errorf("prune passenger quarantine archives: %w", result.Error)
		}
		if result.RowsAffected < 1000 {
			break
		}
	}
	// Unresolved receipts and in-flight passages protect their account's
	// passenger event window, even before terminal proof links are written.
	protection, protectionArgs, err := s.passengerEventProtection(ctx)
	if err != nil {
		return fmt.Errorf("prune passenger event protection: %w", err)
	}
	for _, table := range []string{"requests", "events", "decisions"} {
		predicate := "created_at < CURRENT_TIMESTAMP - INTERVAL '7 days'"
		var args []any
		if table == "events" {
			predicate += protection
			args = protectionArgs
		}
		query := "DELETE FROM " + table + " WHERE id IN (SELECT id FROM " + table +
			" WHERE " + predicate + " ORDER BY created_at LIMIT 10000)"
		for {
			result := s.db.WithContext(ctx).Exec(query, args...)
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
