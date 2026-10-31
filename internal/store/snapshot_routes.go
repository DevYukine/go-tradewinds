package store

import (
	"bytes"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"io"
	"strconv"
	"strings"
	"sync"

	"gorm.io/gorm"
	"gorm.io/gorm/clause"
)

type snapshotRouteCache struct {
	mu               sync.Mutex
	digests          map[string][sha256.Size]byte
	fingerprint      [sha256.Size]byte
	fingerprintValid bool
}

func (s *Store) snapshotRouteCache(accountID string) *snapshotRouteCache {
	if cached, ok := s.snapshotRoutes.Load(accountID); ok {
		return cached.(*snapshotRouteCache)
	}
	cached, _ := s.snapshotRoutes.LoadOrStore(accountID, &snapshotRouteCache{})
	return cached.(*snapshotRouteCache)
}

// ValidateSnapshotRouteNumbers bounds decimal rendering without rounding route
// extensions through float64. Expanded numbers must remain valid after restart.
func ValidateSnapshotRouteNumbers(raw json.RawMessage) error {
	if len(raw) > MaxSnapshotRoutesBytes {
		return ErrResponseTooLarge
	}
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.UseNumber()
	expandedBytes := len(raw)
	for {
		token, err := decoder.Token()
		if err == io.EOF {
			return nil
		}
		if err != nil {
			return fmt.Errorf("decode snapshot route numbers: %w", ErrInvalidInput)
		}
		number, ok := token.(json.Number)
		if !ok {
			continue
		}
		width := len(number)
		if index := strings.IndexAny(string(number), "eE"); index >= 0 {
			exponent, err := strconv.Atoi(string(number[index+1:]))
			if err != nil || exponent < -64 || exponent > 64 {
				return fmt.Errorf("snapshot route number outside storage bounds: %w", ErrResponseTooLarge)
			}
			if exponent < 0 {
				exponent = -exponent
			}
			width = index + exponent + 2
		}
		if width > 64 {
			return fmt.Errorf("snapshot route number outside storage bounds: %w", ErrResponseTooLarge)
		}
		expansion := max(0, width-len(number))
		if expansion > MaxSnapshotRoutesBytes-expandedBytes {
			return ErrResponseTooLarge
		}
		expandedBytes += expansion
	}
}

func encodeSnapshotRoutes(raw json.RawMessage, secrets []string) (map[string]json.RawMessage, error) {
	if len(raw) == 0 {
		return nil, nil
	}
	if len(raw) > MaxSnapshotRoutesBytes {
		return nil, ErrResponseTooLarge
	}
	trimmed := bytes.TrimSpace(raw)
	if !json.Valid(trimmed) || (trimmed[0] != '{' && !bytes.Equal(trimmed, []byte("null"))) {
		return nil, fmt.Errorf("snapshot routes must be a JSON object: %w", ErrInvalidInput)
	}
	encoded, err := redactJSON(raw, secrets, 0)
	if err != nil {
		return nil, err
	}
	if err := ValidateSnapshotRouteNumbers(encoded); err != nil {
		return nil, err
	}
	var routes map[string]json.RawMessage
	if err := json.Unmarshal(encoded, &routes); err != nil {
		return nil, fmt.Errorf("decode redacted snapshot routes: %w", err)
	}
	return routes, nil
}

func preflightSnapshotRoutes(tx *gorm.DB, accountID string) error {
	var encodedBytes int64
	// Numeric magnitude and scale are inspected without decimal rendering.
	// CASE must guard both scalar and whole-row text conversion for legacy rows.
	err := tx.Raw(`WITH sizes AS MATERIALIZED (
		SELECT CASE
			WHEN pg_column_size(data) > ? THEN ?::bigint
			WHEN EXISTS (
				SELECT 1 FROM jsonb_path_query(data, ?::jsonpath) AS numbers(value)
				WHERE CASE
					WHEN abs(value::numeric) >= 1e64 OR scale(value::numeric) > 64 THEN true
					ELSE length(value::text) > 64
				END
			) THEN ?::bigint
			ELSE octet_length(data::text)::bigint + octet_length(to_jsonb(route_key)::text) + 1
		END AS size_bytes
		FROM snapshot_routes WHERE account_id = ?
	) SELECT COALESCE(sum(size_bytes), 0) + 2 + GREATEST(count(*) - 1, 0) FROM sizes`,
		MaxSnapshotRoutesBytes, MaxSnapshotRoutesBytes+1,
		`strict $.** ? (@.type() == "number")`, MaxSnapshotRoutesBytes+1, accountID).Scan(&encodedBytes).Error
	if err != nil {
		return err
	}
	if encodedBytes > MaxSnapshotRoutesBytes {
		return ErrResponseTooLarge
	}
	return nil
}

func saveSnapshotRoutes(tx *gorm.DB, accountID string, routes map[string]json.RawMessage, cache *snapshotRouteCache) error {
	if cache.digests == nil {
		if err := preflightSnapshotRoutes(tx, accountID); err != nil {
			return err
		}
		var rows []snapshotRouteRow
		if err := tx.Where("account_id = ?", accountID).Find(&rows).Error; err != nil {
			return err
		}
		cache.digests = make(map[string][sha256.Size]byte, len(rows))
		remaining := MaxSnapshotRoutesBytes - 2
		for _, row := range rows {
			// PostgreSQL jsonb changes whitespace and object order. Match the
			// redactor's canonical encoding without changing stored values.
			decoder := json.NewDecoder(bytes.NewReader(row.Data))
			decoder.UseNumber()
			var value any
			if err := decoder.Decode(&value); err != nil {
				return fmt.Errorf("decode stored route: %w", err)
			}
			canonical, err := json.Marshal(value)
			if err != nil {
				return fmt.Errorf("encode stored route: %w", err)
			}
			key, err := json.Marshal(row.RouteKey)
			if err != nil {
				return fmt.Errorf("encode stored route key: %w", err)
			}
			size := len(key) + 1 + len(canonical)
			if len(cache.digests) > 0 {
				size++
			}
			if size > remaining {
				return ErrResponseTooLarge
			}
			remaining -= size
			cache.digests[row.RouteKey] = sha256.Sum256(canonical)
		}
	}

	digests := make(map[string][sha256.Size]byte, len(routes))
	var changed []snapshotRouteRow
	for key, raw := range routes {
		digest := sha256.Sum256(raw)
		digests[key] = digest
		if previous, exists := cache.digests[key]; !exists || previous != digest {
			changed = append(changed, snapshotRouteRow{AccountID: accountID, RouteKey: key, Data: raw})
		}
	}
	if len(changed) > 0 {
		if err := tx.Clauses(clause.OnConflict{
			Columns:   []clause.Column{{Name: "account_id"}, {Name: "route_key"}},
			DoUpdates: clause.AssignmentColumns([]string{"data"}),
		}).CreateInBatches(changed, 100).Error; err != nil {
			return err
		}
	}
	var removed []string
	for key := range cache.digests {
		if _, exists := digests[key]; !exists {
			removed = append(removed, key)
		}
	}
	for start := 0; start < len(removed); start += 100 {
		batch := removed[start:min(start+100, len(removed))]
		if err := tx.Where("account_id = ? AND route_key IN ?", accountID, batch).Delete(&snapshotRouteRow{}).Error; err != nil {
			return err
		}
	}
	if len(changed) > 0 || len(removed) > 0 {
		if err := preflightSnapshotRoutes(tx, accountID); err != nil {
			return err
		}
	}
	cache.digests = digests
	return nil
}
