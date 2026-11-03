package store

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	"gorm.io/gorm"
)

const MaxShipLossBatch = 100

const (
	shipLossCargoUnavailable  = "Cargo evidence was invalid or exceeded storage limits."
	shipLossStoresUnavailable = "Stores evidence was invalid or exceeded storage limits."
	shipLossHazardUnavailable = "Hazard evidence was invalid or exceeded storage limits."
)

// ShipLoss retains the first observation of a terminal hull or inferred absence.
// Nullable evidence describes the last known afloat observation, when available.
type ShipLoss struct {
	AccountID               string          `json:"account_id" gorm:"primaryKey"`
	ShipID                  string          `json:"ship_id" gorm:"primaryKey"`
	ShipName                string          `json:"ship_name"`
	ShipClass               string          `json:"ship_class"`
	Fate                    string          `json:"fate"`
	Status                  string          `json:"status"`
	ObservedAt              time.Time       `json:"observed_at"`
	RecoveredAt             *time.Time      `json:"recovered_at"`
	LastObservedAt          *time.Time      `json:"last_observed_at"`
	LastPortID              *string         `json:"last_port_id"`
	LastSeaZoneID           *string         `json:"last_sea_zone_id"`
	VoyageOriginID          *string         `json:"voyage_origin_id"`
	VoyageDestinationID     *string         `json:"voyage_destination_id"`
	CargoManifest           json.RawMessage `json:"cargo_manifest" gorm:"type:jsonb"`
	CargoUnavailableReason  string          `json:"cargo_unavailable_reason,omitempty" gorm:"not null;default:''"`
	Stores                  json.RawMessage `json:"stores" gorm:"type:jsonb"`
	StoresUnavailableReason string          `json:"stores_unavailable_reason,omitempty" gorm:"not null;default:''"`
	HazardCause             *string         `json:"hazard_cause"`
	HazardUnavailableReason string          `json:"hazard_unavailable_reason,omitempty" gorm:"not null;default:''"`
	InsuranceClaimPaidPence *int64          `json:"insurance_claim_paid_pence"`
}

func (ShipLoss) TableName() string { return "ship_losses" }

// SaveShipLosses retains the earliest observation and first known evidence.
// A recovered missing hull starts a new episode only after its recovery.
func (s *Store) SaveShipLosses(ctx context.Context, accountID string, losses []ShipLoss) error {
	if !validID(accountID) || len(losses) > MaxShipLossBatch {
		return fmt.Errorf("ship loss account or batch: %w", ErrInvalidInput)
	}
	for _, loss := range losses {
		if loss.AccountID != "" && loss.AccountID != accountID || !validID(loss.ShipID) ||
			len(loss.ShipName) > 255 || len(loss.ShipClass) > 255 || !validID(loss.Fate) || !validID(loss.Status) ||
			loss.ObservedAt.IsZero() || loss.LastObservedAt != nil && loss.LastObservedAt.IsZero() ||
			loss.Fate == "missing" && loss.Status != "sunk" ||
			loss.InsuranceClaimPaidPence != nil && *loss.InsuranceClaimPaidPence < 0 {
			return fmt.Errorf("ship loss fields: %w", ErrInvalidInput)
		}
		if loss.RecoveredAt != nil && (loss.Fate != "missing" ||
			!loss.RecoveredAt.UTC().Truncate(time.Microsecond).After(loss.ObservedAt.UTC().Truncate(time.Microsecond))) {
			return fmt.Errorf("ship loss recovery: %w", ErrInvalidInput)
		}
		for _, id := range []*string{loss.LastPortID, loss.LastSeaZoneID, loss.VoyageOriginID, loss.VoyageDestinationID} {
			if id != nil && !validID(*id) {
				return fmt.Errorf("ship loss location: %w", ErrInvalidInput)
			}
		}
	}
	if len(losses) == 0 {
		return nil
	}
	secrets, err := s.secrets(ctx, accountID)
	if err != nil {
		return err
	}
	rows := make([]ShipLoss, len(losses))
	for i, loss := range losses {
		loss.AccountID = accountID
		loss.ObservedAt = loss.ObservedAt.UTC().Truncate(time.Microsecond)
		if loss.RecoveredAt != nil {
			recoveredAt := loss.RecoveredAt.UTC().Truncate(time.Microsecond)
			loss.RecoveredAt = &recoveredAt
		}
		loss.ShipName = boundedText(scrubText(loss.ShipName, secrets), 255)
		loss.ShipClass = boundedText(scrubText(loss.ShipClass, secrets), 255)
		loss.Fate = boundedText(scrubText(loss.Fate, secrets), 255)
		loss.Status = boundedText(scrubText(loss.Status, secrets), 255)
		loss.CargoManifest, err = shipLossJSON(loss.CargoManifest, secrets)
		if err != nil || loss.CargoManifest == nil && loss.CargoUnavailableReason != "" {
			loss.CargoUnavailableReason = shipLossCargoUnavailable
		} else {
			loss.CargoUnavailableReason = ""
		}
		loss.Stores, err = shipLossJSON(loss.Stores, secrets)
		if err != nil || loss.Stores == nil && loss.StoresUnavailableReason != "" {
			loss.StoresUnavailableReason = shipLossStoresUnavailable
		} else {
			loss.StoresUnavailableReason = ""
		}
		err = nil
		if loss.HazardCause != nil {
			loss.HazardCause, err = NormalizeShipLossHazard(*loss.HazardCause)
			if err == nil {
				loss.HazardCause, err = NormalizeShipLossHazard(scrubText(*loss.HazardCause, secrets))
			}
		}
		if err != nil || loss.HazardCause == nil && loss.HazardUnavailableReason != "" {
			loss.HazardUnavailableReason = shipLossHazardUnavailable
		} else {
			loss.HazardUnavailableReason = ""
		}
		rows[i] = loss
	}
	encoded, err := json.Marshal(rows)
	if err != nil {
		return err
	}
	if len(encoded) > MaxSnapshotBytes {
		return fmt.Errorf("ship loss batch size: %w", ErrInvalidInput)
	}
	return s.writeOwned(ctx, accountID, func(tx *gorm.DB) error {
		return tx.Exec(`INSERT INTO ship_losses (
				account_id, ship_id, ship_name, ship_class, fate, status, observed_at, recovered_at, last_observed_at,
				last_port_id, last_sea_zone_id, voyage_origin_id, voyage_destination_id,
				cargo_manifest, cargo_unavailable_reason, stores, stores_unavailable_reason,
				hazard_cause, hazard_unavailable_reason, insurance_claim_paid_pence
			)
			SELECT ?, ship_id, ship_name, ship_class, fate, status, observed_at, recovered_at, last_observed_at,
				last_port_id, last_sea_zone_id, voyage_origin_id, voyage_destination_id,
				cargo_manifest, COALESCE(cargo_unavailable_reason, ''), stores, COALESCE(stores_unavailable_reason, ''),
				hazard_cause, COALESCE(hazard_unavailable_reason, ''), insurance_claim_paid_pence
			FROM jsonb_to_recordset(?::jsonb) AS losses (
				ship_id text, ship_name text, ship_class text, fate text, status text,
				observed_at timestamptz, recovered_at timestamptz, last_observed_at timestamptz,
				last_port_id text, last_sea_zone_id text, voyage_origin_id text, voyage_destination_id text,
				cargo_manifest jsonb, cargo_unavailable_reason text, stores jsonb, stores_unavailable_reason text,
				hazard_cause text, hazard_unavailable_reason text, insurance_claim_paid_pence bigint
			)
			ON CONFLICT (account_id, ship_id) DO UPDATE SET
				(ship_name, ship_class, fate, status, observed_at, recovered_at, last_observed_at,
				 last_port_id, last_sea_zone_id, voyage_origin_id, voyage_destination_id,
				 cargo_manifest, cargo_unavailable_reason, stores, stores_unavailable_reason,
				 hazard_cause, hazard_unavailable_reason, insurance_claim_paid_pence) = (
					SELECT EXCLUDED.ship_name, EXCLUDED.ship_class, EXCLUDED.fate, EXCLUDED.status,
						EXCLUDED.observed_at, EXCLUDED.recovered_at, EXCLUDED.last_observed_at,
						EXCLUDED.last_port_id, EXCLUDED.last_sea_zone_id, EXCLUDED.voyage_origin_id, EXCLUDED.voyage_destination_id,
						EXCLUDED.cargo_manifest, EXCLUDED.cargo_unavailable_reason, EXCLUDED.stores, EXCLUDED.stores_unavailable_reason,
						EXCLUDED.hazard_cause, EXCLUDED.hazard_unavailable_reason, EXCLUDED.insurance_claim_paid_pence
					WHERE ship_losses.recovered_at IS NOT NULL AND EXCLUDED.recovered_at IS NULL
						AND EXCLUDED.observed_at > ship_losses.recovered_at
					UNION ALL
					SELECT COALESCE(NULLIF(ship_losses.ship_name, ''), EXCLUDED.ship_name),
						COALESCE(NULLIF(ship_losses.ship_class, ''), EXCLUDED.ship_class),
						CASE WHEN ship_losses.recovered_at IS NULL AND ship_losses.fate IN ('missing', 'unknown')
							AND (EXCLUDED.fate NOT IN ('missing', 'unknown')
								OR (ship_losses.fate = 'missing' AND EXCLUDED.fate = 'unknown' AND EXCLUDED.status = 'sunk'))
							THEN EXCLUDED.fate ELSE ship_losses.fate END,
						CASE WHEN ship_losses.recovered_at IS NULL AND ship_losses.fate IN ('missing', 'unknown')
							AND (EXCLUDED.fate NOT IN ('missing', 'unknown')
								OR (ship_losses.fate = 'missing' AND EXCLUDED.fate = 'unknown' AND EXCLUDED.status = 'sunk'))
							THEN EXCLUDED.status ELSE ship_losses.status END,
						LEAST(ship_losses.observed_at, EXCLUDED.observed_at),
						COALESCE(ship_losses.recovered_at, CASE WHEN EXCLUDED.recovered_at > ship_losses.observed_at
							THEN EXCLUDED.recovered_at END),
						COALESCE(ship_losses.last_observed_at, EXCLUDED.last_observed_at),
						COALESCE(ship_losses.last_port_id, EXCLUDED.last_port_id),
						COALESCE(ship_losses.last_sea_zone_id, EXCLUDED.last_sea_zone_id),
						COALESCE(ship_losses.voyage_origin_id, EXCLUDED.voyage_origin_id),
						COALESCE(ship_losses.voyage_destination_id, EXCLUDED.voyage_destination_id),
						COALESCE(ship_losses.cargo_manifest, EXCLUDED.cargo_manifest),
						CASE WHEN ship_losses.cargo_manifest IS NOT NULL OR EXCLUDED.cargo_manifest IS NOT NULL THEN ''
							ELSE COALESCE(NULLIF(ship_losses.cargo_unavailable_reason, ''), EXCLUDED.cargo_unavailable_reason) END,
						COALESCE(ship_losses.stores, EXCLUDED.stores),
						CASE WHEN ship_losses.stores IS NOT NULL OR EXCLUDED.stores IS NOT NULL THEN ''
							ELSE COALESCE(NULLIF(ship_losses.stores_unavailable_reason, ''), EXCLUDED.stores_unavailable_reason) END,
						COALESCE(ship_losses.hazard_cause, EXCLUDED.hazard_cause),
						CASE WHEN ship_losses.hazard_cause IS NOT NULL OR EXCLUDED.hazard_cause IS NOT NULL THEN ''
							ELSE COALESCE(NULLIF(ship_losses.hazard_unavailable_reason, ''), EXCLUDED.hazard_unavailable_reason) END,
						COALESCE(ship_losses.insurance_claim_paid_pence, EXCLUDED.insurance_claim_paid_pence)
					WHERE NOT (ship_losses.recovered_at IS NOT NULL AND EXCLUDED.recovered_at IS NULL
						AND EXCLUDED.observed_at > ship_losses.recovered_at)
				)
				WHERE EXCLUDED.recovered_at IS NULL OR (ship_losses.fate = 'missing'
					AND EXCLUDED.recovered_at > ship_losses.observed_at)`,
			accountID, string(encoded)).Error
	})
}

// NormalizeShipLossEvidence also bounds snapshot-backed observations and retries.
// Decimal numbers and both JSON encodings must remain safe after a JSONB reload.
func NormalizeShipLossEvidence(raw json.RawMessage) (json.RawMessage, error) {
	return shipLossJSON(raw, nil)
}

// NormalizeShipLossHazard rejects optional text that PostgreSQL cannot retain.
func NormalizeShipLossHazard(value string) (*string, error) {
	if len(value) > 4096 || !utf8.ValidString(value) || strings.IndexByte(value, 0) >= 0 {
		return nil, fmt.Errorf("ship loss hazard outside storage bounds: %w", ErrInvalidInput)
	}
	return &value, nil
}

func shipLossJSON(raw json.RawMessage, secrets []string) (json.RawMessage, error) {
	if len(raw) > maxDiagnosticBytes {
		return nil, fmt.Errorf("ship loss evidence size: %w", ErrInvalidInput)
	}
	raw = bytes.TrimSpace(raw)
	if len(raw) == 0 || bytes.Equal(raw, []byte("null")) {
		return nil, nil
	}
	if !utf8.Valid(raw) || !json.Valid(raw) || raw[0] != '{' {
		return nil, fmt.Errorf("ship loss evidence must be a JSON object: %w", ErrInvalidInput)
	}
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.UseNumber()
	var value map[string]any
	if err := decoder.Decode(&value); err != nil {
		return nil, fmt.Errorf("decode ship loss evidence: %w", err)
	}
	normalized, _, err := normalizeShipLossValue(redactValue(value, secrets), 0)
	if err != nil {
		return nil, err
	}
	clean, err := json.Marshal(normalized)
	if err != nil {
		return nil, fmt.Errorf("encode ship loss evidence: %w", err)
	}
	// Go's canonical HTML/Unicode quoting can exceed PostgreSQL's JSON text.
	// Neither form may exceed the limit used by the next save after restart.
	if len(clean) > maxDiagnosticBytes {
		return nil, fmt.Errorf("ship loss evidence size: %w", ErrInvalidInput)
	}
	return clean, nil
}

func normalizeShipLossValue(value any, depth int) (any, int, error) {
	if depth > 64 {
		return nil, 0, fmt.Errorf("ship loss evidence nesting: %w", ErrInvalidInput)
	}
	size := 0
	switch typed := value.(type) {
	case map[string]any:
		size = 2 + max(0, len(typed)-1)*2
		for key, child := range typed {
			normalized, childSize, err := normalizeShipLossValue(child, depth+1)
			if err != nil {
				return nil, 0, err
			}
			typed[key] = normalized
			size += shipLossJSONStringBytes(key) + 2 + childSize
			if size > maxDiagnosticBytes {
				return nil, 0, fmt.Errorf("ship loss evidence rendered size: %w", ErrInvalidInput)
			}
		}
	case []any:
		size = 2 + max(0, len(typed)-1)*2
		for i, child := range typed {
			normalized, childSize, err := normalizeShipLossValue(child, depth+1)
			if err != nil {
				return nil, 0, err
			}
			typed[i] = normalized
			size += childSize
			if size > maxDiagnosticBytes {
				return nil, 0, fmt.Errorf("ship loss evidence rendered size: %w", ErrInvalidInput)
			}
		}
	case json.Number:
		number, err := normalizeShipLossNumber(typed)
		if err != nil {
			return nil, 0, err
		}
		value, size = number, len(number)
	case string:
		size = shipLossJSONStringBytes(typed)
	case bool:
		size = 4
		if !typed {
			size = 5
		}
	case nil:
		size = 4
	}
	if size > maxDiagnosticBytes {
		return nil, 0, fmt.Errorf("ship loss evidence rendered size: %w", ErrInvalidInput)
	}
	return value, size, nil
}

// PostgreSQL quotes controls, quotes and backslashes, but leaves HTML and
// U+2028/U+2029 as UTF-8. Count its separators separately from Go's encoding.
func shipLossJSONStringBytes(value string) int {
	size := 2
	for _, char := range value {
		switch char {
		case '"', '\\', '\b', '\f', '\n', '\r', '\t':
			size += 2
		default:
			if char < 0x20 {
				size += 6
			} else {
				size += utf8.RuneLen(char)
			}
		}
	}
	return size
}

func normalizeShipLossNumber(number json.Number) (json.Number, error) {
	// Match the snapshot-route rendered-width policy, not the exponent lexeme:
	// a decimal accepted now must still be accepted when JSONB expands it.
	token := number.String()
	if len(token) > 64 {
		return "", fmt.Errorf("ship loss evidence number outside storage bounds: %w", ErrInvalidInput)
	}
	negative := token[0] == '-'
	index := strings.IndexAny(token, "eE")
	if index < 0 && (!negative || strings.Trim(token[1:], "0.") != "") {
		return number, nil
	}
	if negative {
		token = token[1:]
		if index >= 0 {
			index--
		}
	}
	exponent := 0
	if index >= 0 {
		var err error
		exponent, err = strconv.Atoi(token[index+1:])
		if err != nil || exponent < -64 || exponent > 64 {
			return "", fmt.Errorf("ship loss evidence number outside storage bounds: %w", ErrInvalidInput)
		}
		token = token[:index]
	}
	point := strings.IndexByte(token, '.')
	digits := token
	if point < 0 {
		point = len(token)
	} else {
		digits = token[:point] + token[point+1:]
	}
	point += exponent
	scale := max(0, len(digits)-point)
	trimmed := strings.TrimLeft(digits, "0")
	if trimmed == "" {
		negative = false
		digits, point = "0", 1-scale
	} else {
		point -= len(digits) - len(trimmed)
		digits = trimmed
	}
	width := len(digits) + 1
	switch {
	case point <= 0:
		width = 2 - point + len(digits)
	case point >= len(digits):
		width = point
	}
	if negative {
		width++
	}
	if width > 64 {
		return "", fmt.Errorf("ship loss evidence number outside storage bounds: %w", ErrInvalidInput)
	}
	var plain strings.Builder
	plain.Grow(width)
	if negative {
		plain.WriteByte('-')
	}
	switch {
	case point <= 0:
		plain.WriteString("0.")
		for range -point {
			plain.WriteByte('0')
		}
		plain.WriteString(digits)
	case point >= len(digits):
		plain.WriteString(digits)
		for range point - len(digits) {
			plain.WriteByte('0')
		}
	default:
		plain.WriteString(digits[:point])
		plain.WriteByte('.')
		plain.WriteString(digits[point:])
	}
	return json.Number(plain.String()), nil
}

// ShipLosses returns internal evidence without display-time changes to ship IDs.
func (s *Store) ShipLosses(ctx context.Context, accountID string) ([]ShipLoss, error) {
	if !validID(accountID) {
		return nil, fmt.Errorf("ship loss account: %w", ErrInvalidInput)
	}
	db, cancel := s.query(ctx)
	defer cancel()
	rows := make([]ShipLoss, 0)
	err := db.Where("account_id = ?", accountID).Order("observed_at, ship_id").Find(&rows).Error
	return rows, err
}

// ShipLossEvents exposes retained account evidence after a host observation sequence.
func (s *Store) ShipLossEvents(ctx context.Context, accountID string, afterID int64) ([]Event, error) {
	if !validID(accountID) || afterID < 0 {
		return nil, fmt.Errorf("ship loss event account: %w", ErrInvalidInput)
	}
	db, cancel := s.query(ctx)
	defer cancel()
	rows := make([]Event, 0)
	err := db.Where("account_id = ? AND id > ? AND event_type IN ?", accountID, afterID,
		[]string{"voyage_hazard", "insurance_claim_paid", "insurance_policy_closed"}).Order("id").Find(&rows).Error
	return rows, err
}

func (d *displayScrubber) shipLoss(value *ShipLoss) error {
	credentials := d.credentials(value.AccountID)
	value.ShipID = displayText(value.ShipID, credentials)
	value.ShipName = displayText(value.ShipName, credentials)
	value.ShipClass = displayText(value.ShipClass, credentials)
	value.Fate = displayText(value.Fate, credentials)
	value.Status = displayText(value.Status, credentials)
	for _, id := range []*string{value.LastPortID, value.LastSeaZoneID, value.VoyageOriginID, value.VoyageDestinationID, value.HazardCause} {
		if id != nil {
			*id = displayText(*id, credentials)
		}
	}
	var err error
	value.CargoManifest, err = displayJSON(value.CargoManifest, credentials)
	if err != nil {
		return err
	}
	value.Stores, err = displayJSON(value.Stores, credentials)
	return err
}
