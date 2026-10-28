package store

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strings"
	"time"

	"gorm.io/gorm"
)

// ErrReplayEvidenceBudget means a complete replay history cannot be loaded
// safely. Callers must reject the history, never use a truncated prefix.
var ErrReplayEvidenceBudget = errors.New("replay evidence exceeds the safe read budget")

const (
	// MaxReplayEvidenceCiphertextBytes caps the combined request/response or
	// settlement context fetched for one row, before any authentication/decryption.
	MaxReplayEvidenceCiphertextBytes = 128 << 10
	// MaxReplayEvidenceMetadataBytes caps variable-length row metadata before
	// fetching it. Metadata pages contain only guarded IDs, times and byte counts.
	MaxReplayEvidenceMetadataBytes = 16 << 10
	// MaxReplayEvidenceRetainedBytes caps each history result's projected JSON
	// backing buffers and metadata strings, charging another 512 bytes per row
	// for fixed fields. The row cap also bounds slice storage and page overhead.
	MaxReplayEvidenceRetainedBytes = 16 << 20
	MaxReplayEvidenceRows          = 10000

	replayEvidencePageSize    = 100
	replayEvidenceRowOverhead = 512

	replayActionCiphertextBytesSQL = "COALESCE(octet_length(body_cipher), 0)::bigint + COALESCE(octet_length(response_cipher), 0)"
	replayActionMetadataBytesSQL   = "octet_length(id)::bigint + octet_length(account_id) + octet_length(method) + octet_length(path) + octet_length(state)"

	replaySettlementCiphertextBytesSQL = "octet_length(context_cipher)::bigint"
	replaySettlementMetadataBytesSQL   = "octet_length(id)::bigint + octet_length(account_id) + octet_length(mission_id) + COALESCE(octet_length(action_id), 0) + COALESCE(octet_length(event_id), 0) + octet_length(lane_lookup) + octet_length(kind)"
)

type replayAccountingSnapshotRow struct {
	SizeBytes int64
	Data      json.RawMessage
}

// ReplayAccountingSnapshot reads only the current accounting evidence used by
// replay validation. PostgreSQL checks the complete source's rendered JSON size
// before building or returning the projection; oversized rows return only size
// metadata. The normal live Snapshot reader is intentionally unchanged.
func (s *Store) ReplayAccountingSnapshot(ctx context.Context, accountID string) (map[string]json.RawMessage, error) {
	if !validID(accountID) {
		return nil, fmt.Errorf("replay accounting account ID: %w", ErrInvalidInput)
	}
	db, cancel := s.query(ctx)
	defer cancel()
	var row replayAccountingSnapshotRow
	err := db.Table("snapshots").Select(`octet_length(data::text) AS size_bytes,
		CASE WHEN octet_length(data::text) <= ? THEN jsonb_build_object(
			'_expense_allocations', data -> '_expense_allocations',
			'ledger', data -> 'ledger',
			'vaults', data -> 'vaults',
			'contracts', data -> 'contracts'
		) END AS data`, MaxSnapshotBytes).
		Where("account_id = ?", accountID).Take(&row).Error
	if errors.Is(err, gorm.ErrRecordNotFound) {
		return make(map[string]json.RawMessage), nil
	}
	if err != nil {
		return nil, err
	}
	if row.SizeBytes > MaxSnapshotBytes {
		return nil, fmt.Errorf("replay accounting snapshot exceeds the %d-byte read budget: %w", MaxSnapshotBytes, ErrReplayEvidenceBudget)
	}
	data := make(map[string]json.RawMessage)
	if err := json.Unmarshal(row.Data, &data); err != nil {
		return nil, errors.New("replay accounting snapshot contains invalid JSON")
	}
	return data, nil
}

type replayEvidenceRowMeta struct {
	ID              string
	At              time.Time
	CiphertextBytes int64
	MetadataBytes   int64
}

func (row replayEvidenceRowMeta) check(kind string) error {
	if row.CiphertextBytes > MaxReplayEvidenceCiphertextBytes {
		return fmt.Errorf("%s ciphertext exceeds the %d-byte row budget: %w", kind, MaxReplayEvidenceCiphertextBytes, ErrReplayEvidenceBudget)
	}
	if row.MetadataBytes > MaxReplayEvidenceMetadataBytes {
		return fmt.Errorf("%s metadata exceeds the %d-byte row budget: %w", kind, MaxReplayEvidenceMetadataBytes, ErrReplayEvidenceBudget)
	}
	return nil
}

type replayEvidenceBudget struct {
	rows     int
	retained int64
}

func (budget *replayEvidenceBudget) addRows(count int, kind string) error {
	if count > MaxReplayEvidenceRows-budget.rows {
		return fmt.Errorf("%s exceeds the %d-row evidence budget: %w", kind, MaxReplayEvidenceRows, ErrReplayEvidenceBudget)
	}
	budget.rows += count
	return nil
}

func (budget *replayEvidenceBudget) retain(size int64, kind string) error {
	if size > MaxReplayEvidenceRetainedBytes-budget.retained {
		return fmt.Errorf("%s exceeds the %d-byte retained evidence budget: %w", kind, MaxReplayEvidenceRetainedBytes, ErrReplayEvidenceBudget)
	}
	budget.retained += size
	return nil
}

type replayEvidenceShape uint8

const (
	replayEvidenceRequest replayEvidenceShape = iota
	replayEvidenceResponse
	replayEvidenceVoyage
)

func replayEvidenceField(shape replayEvidenceShape, field string) bool {
	if shape == replayEvidenceVoyage {
		return field == "estimated_arrival_at"
	}
	if field == "commodity_id" {
		return true
	}
	if shape == replayEvidenceRequest {
		switch field {
		case "quantity", "ship_id", "port_id", "side", "price_pence", "contract_id", "dest_port_id":
			return true
		}
		return false
	}
	switch field {
	case "filled_quantity", "total_gross_pence", "customs_duty_pence", "reward_paid_pence", "fee_pence",
		"committed_uninterpreted", "contract_id", "collateral_pence", "delivered_quantity", "estimated_arrival_at", "voyage":
		return true
	}
	return false
}

// projectReplayEvidenceJSON retains only fields read by sim's accounting,
// validation, calibration and charter policy. Raw values preserve integer pence,
// timestamps, missing/null distinctions and invalid types; field order and
// duplicate keys survive so projection cannot turn a bad receipt into a good one.
// Nested voyage diagnostics are reduced to the arrival time calibration reads.
// Non-object or malformed bodies (including raw HTTP rejection text) become an
// opaque non-object marker: they remain present but cannot become valid receipts.
func projectReplayEvidenceJSON(raw []byte, shape replayEvidenceShape) json.RawMessage {
	if len(raw) == 0 {
		return nil
	}
	if bytes.Equal(bytes.TrimSpace(raw), []byte("null")) {
		return json.RawMessage("null")
	}
	decoder := json.NewDecoder(bytes.NewReader(raw))
	start, err := decoder.Token()
	if err != nil || start != json.Delim('{') {
		return json.RawMessage(`"opaque"`)
	}
	projected := make([]byte, 0, min(len(raw), 1024))
	projected = append(projected, '{')
	for decoder.More() {
		token, err := decoder.Token()
		if err != nil {
			return json.RawMessage(`"opaque"`)
		}
		field, ok := token.(string)
		if !ok {
			return json.RawMessage(`"opaque"`)
		}
		var value json.RawMessage
		if err := decoder.Decode(&value); err != nil {
			return json.RawMessage(`"opaque"`)
		}
		name := strings.ToLower(field)
		if !replayEvidenceField(shape, name) {
			continue
		}
		if shape == replayEvidenceResponse && name == "voyage" {
			value = projectReplayEvidenceJSON(value, replayEvidenceVoyage)
		}
		if len(projected) > 1 {
			projected = append(projected, ',')
		}
		// Retained names contain only ASCII letters and underscores, including
		// case variants accepted by encoding/json's struct-field matching.
		projected = append(projected, '"')
		projected = append(projected, field...)
		projected = append(projected, '"', ':')
		projected = append(projected, value...)
	}
	if end, err := decoder.Token(); err != nil || end != json.Delim('}') {
		return json.RawMessage(`"opaque"`)
	}
	var extra json.RawMessage
	if err := decoder.Decode(&extra); !errors.Is(err, io.EOF) {
		return json.RawMessage(`"opaque"`)
	}
	return append(projected, '}')
}

// WithReplaySnapshot keeps every replay pass on one read-only database snapshot.
// The view belongs to this callback and must not be closed or retained afterward.
func (s *Store) WithReplaySnapshot(ctx context.Context, read func(*Store) error) error {
	tx := s.db.WithContext(ctx).Begin(&sql.TxOptions{Isolation: sql.LevelRepeatableRead, ReadOnly: true})
	if tx.Error != nil {
		return fmt.Errorf("begin replay snapshot: %w", databaseError{cause: tx.Error})
	}
	defer tx.Rollback()
	view := &Store{db: tx, aead: s.aead, lookupKey: s.lookupKey, log: s.log}
	if err := read(view); err != nil {
		return err
	}
	if err := tx.Commit().Error; err != nil {
		return fmt.Errorf("commit replay snapshot: %w", databaseError{cause: err})
	}
	return nil
}

// ReplaySourcingBudgetBefore retains the live rolling-budget prefix before the
// tape boundary, including probes attributed by deliveries through asOf. The same
// guarded row, ciphertext and retained-memory limits apply as to other tape readers.
func (s *Store) ReplaySourcingBudgetBefore(ctx context.Context, accountID string, from, before, asOf time.Time) ([]Settlement, error) {
	if !validID(accountID) || from.IsZero() || before.IsZero() || asOf.Before(from) || asOf.Before(before) {
		return nil, fmt.Errorf("replay sourcing budget window: %w", ErrInvalidInput)
	}
	db, cancel := s.query(ctx)
	probeFrom, err := sourcingBudgetProbeFrom(db, accountID, from, asOf)
	cancel()
	if err != nil {
		return nil, err
	}
	if !probeFrom.Before(before) {
		return nil, nil
	}
	return s.replaySettlementsWhere(ctx, accountID,
		"completed_at < ? AND (completed_at >= ? OR (kind = ? AND completed_at >= ?))", before, from, "relocation", probeFrom)
}

// ReplayCharterProceedsBefore uses the same settled-charter sum as the live
// taper, excluding historical deliveries after this variant's cutover.
func (s *Store) ReplayCharterProceedsBefore(ctx context.Context, accountID string, since, before time.Time) (int64, error) {
	if !validID(accountID) || before.IsZero() {
		return 0, fmt.Errorf("replay charter prefix: %w", ErrInvalidInput)
	}
	db, cancel := s.query(ctx)
	defer cancel()
	var total int64
	err := db.Model(&settlementRow{}).
		Where("account_id = ? AND kind = 'charter' AND completed_at >= ? AND completed_at <= ?", accountID, since, before).
		Select("COALESCE(SUM(proceeds_pence::numeric), 0)").Scan(&total).Error
	return total, err
}

// ReplayProbeCompletionsBefore reads only the latest necessary pre-cutover
// completion for a bounded live-policy shortlist, never all lifetime probes.
func (s *Store) ReplayProbeCompletionsBefore(ctx context.Context, accountID string, lanes []string, before time.Time) (map[string]time.Time, error) {
	if !validID(accountID) || before.IsZero() || len(lanes) > 1024 {
		return nil, fmt.Errorf("replay probe prefix: %w", ErrInvalidInput)
	}
	expected := make(map[string]bool, len(lanes))
	lookups := make([]string, 0, len(lanes))
	for _, lane := range lanes {
		if lane == "" || len(lane) > 1024 {
			return nil, fmt.Errorf("replay probe lane: %w", ErrInvalidInput)
		}
		if !expected[lane] {
			expected[lane] = true
			lookups = append(lookups, s.privateLookup("settlement-lane", "", lane))
		}
	}
	result := make(map[string]time.Time, len(lanes))
	if len(lookups) == 0 {
		return result, nil
	}
	db, cancel := s.query(ctx)
	var rows []replayEvidenceRowMeta
	err := db.Model(&settlementRow{}).
		Select("DISTINCT ON (lane_lookup) CASE WHEN "+replaySettlementMetadataBytesSQL+" <= ? THEN id ELSE '' END AS id, "+
			"completed_at AS at, "+replaySettlementCiphertextBytesSQL+" AS ciphertext_bytes, "+
			replaySettlementMetadataBytesSQL+" AS metadata_bytes", MaxReplayEvidenceMetadataBytes).
		Where("account_id = ? AND kind = 'relocation' AND lane_lookup IN ? AND completed_at <= ?", accountID, lookups, before).
		Order("lane_lookup, completed_at DESC, id").Find(&rows).Error
	cancel()
	if err != nil {
		return nil, err
	}
	var budget replayEvidenceBudget
	for _, meta := range rows {
		if err := meta.check("replay probe prefix"); err != nil {
			return nil, err
		}
		row, err := s.replaySettlementEvidence(ctx, accountID, meta.ID)
		if err != nil {
			return nil, err
		}
		if !expected[row.Lane] || row.Kind != "relocation" || row.CompletedAt.After(before) {
			return nil, ErrActionConflict
		}
		if err := budget.retain(int64(len(row.Lane)+64), "replay probe prefix"); err != nil {
			return nil, err
		}
		result[row.Lane] = row.CompletedAt.UTC()
	}
	return result, nil
}
