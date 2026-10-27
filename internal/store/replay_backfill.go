package store

import (
	"cmp"
	"context"
	"encoding/json"
	"fmt"
	"slices"
	"time"

	"gorm.io/gorm"
)

// gameapi.Client.send bounds each REST attempt to 45 seconds. Permit one second
// of timer wake-up/logging slack, without letting SSE lifetimes or retained REST
// outliers enlarge the request-start sort frontier.
const replayDiagnosticRequestTimeout = 46 * time.Second

// ReplayBackfillBoundary freezes a run, even before live recording is enabled.
// At is the first live frame (exclusive), or the completion of the last frozen
// diagnostic request. MaxRequestID prevents a moving tail on an active account.
type ReplayBackfillBoundary struct {
	MaxRequestID int64
	At           time.Time
	MaxDuration  time.Duration
	DurationGaps int64
}

type ReplayDiagnostic struct {
	ID         int64
	Method     string
	Path       string
	Status     int
	StartedAt  time.Time
	CreatedAt  time.Time
	Response   json.RawMessage `gorm:"type:jsonb"`
	Error      string
	LastSeenAt time.Time
	Count      int64
	SeenAt     []time.Time `gorm:"-"`
}

type ReplayEvent struct {
	ID           int64           `json:"-"`
	EventID      string          `json:"event_id"`
	Type         string          `json:"type"`
	Payload      json.RawMessage `json:"payload" gorm:"type:jsonb"`
	CreatedAt    time.Time       `json:"created_at"`
	Oversize     bool            `json:"-"`
	PayloadBytes int64           `json:"-"`
}

func (s *Store) ReplayBackfillBoundary(ctx context.Context, accountID string) (ReplayBackfillBoundary, error) {
	db, cancel := s.query(ctx)
	defer cancel()
	var bounds struct {
		MaxRequestID  int64
		MaxDurationMS int64
		DurationGaps  int64
		LastRequest   *time.Time
		FirstLive     *time.Time
	}
	err := db.Transaction(func(tx *gorm.DB) error {
		if err := tx.Exec("SET LOCAL statement_timeout = '9s'").Error; err != nil {
			return err
		}
		return tx.Raw(`SELECT COALESCE(max(id), 0) AS max_request_id,
			max(created_at) FILTER (WHERE duration_ms <= ?) AS last_request,
			COALESCE(max(duration_ms) FILTER (WHERE duration_ms <= ?), 0) AS max_duration_ms,
			count(*) FILTER (WHERE duration_ms > ? AND
				(live.first_live IS NULL OR created_at - duration_ms * interval '1 millisecond' < live.first_live)) AS duration_gaps,
			live.first_live
			FROM requests CROSS JOIN (
				SELECT min(observed_at) AS first_live FROM replay_frames WHERE account_id = ? AND source = 'live'
			) AS live
			WHERE account_id = ? AND method IN ('GET', 'POST', 'PATCH')
			AND split_part(path, chr(63), 1) NOT LIKE '/v1/event/%'
			GROUP BY live.first_live`,
			replayDiagnosticRequestTimeout.Milliseconds(), replayDiagnosticRequestTimeout.Milliseconds(),
			replayDiagnosticRequestTimeout.Milliseconds(), accountID, accountID).Scan(&bounds).Error
	})
	if err != nil {
		return ReplayBackfillBoundary{}, fmt.Errorf("freeze backfill: %w", databaseError{cause: err})
	}
	boundary := ReplayBackfillBoundary{
		MaxRequestID: bounds.MaxRequestID,
		MaxDuration:  time.Duration(bounds.MaxDurationMS) * time.Millisecond,
		DurationGaps: bounds.DurationGaps,
	}
	if bounds.FirstLive != nil {
		boundary.At = *bounds.FirstLive
	} else if bounds.LastRequest != nil {
		boundary.At = bounds.LastRequest.Add(time.Microsecond)
	}
	return boundary, nil
}

func (s *Store) ScanReplayDiagnostics(ctx context.Context, accountID string, boundary ReplayBackfillBoundary, yield func(ReplayDiagnostic) error) error {
	maxDuration := min(boundary.MaxDuration, replayDiagnosticRequestTimeout)
	var after time.Time
	var afterID int64
	var pending []ReplayDiagnostic
	for {
		db, cancel := s.query(ctx)
		var rows []ReplayDiagnostic
		err := db.Transaction(func(tx *gorm.DB) error {
			if err := tx.Exec("SET LOCAL statement_timeout = '9s'").Error; err != nil {
				return err
			}
			return tx.Raw(`SELECT id, method, path, status, created_at - duration_ms * interval '1 millisecond' AS started_at,
				created_at, convert_to(response::text, 'UTF8') AS response, error FROM requests
				WHERE account_id = ? AND id <= ? AND (method = 'GET' OR method IN ('POST', 'PATCH'))
				AND split_part(path, chr(63), 1) <> '/v1/world/route'
				AND split_part(path, chr(63), 1) NOT LIKE '/v1/event/%'
				AND duration_ms <= ?
				AND created_at - duration_ms * interval '1 millisecond' < ?
				AND created_at < ?
				AND (created_at, id) > (?, ?)
				ORDER BY created_at, id LIMIT 5000`,
				accountID, boundary.MaxRequestID, replayDiagnosticRequestTimeout.Milliseconds(), boundary.At, boundary.At.Add(maxDuration), after, afterID).Scan(&rows).Error
		})
		cancel()
		if err != nil {
			return fmt.Errorf("scan replay diagnostics: %w", databaseError{cause: err})
		}
		pending = append(pending, rows...)
		slices.SortFunc(pending, func(a, b ReplayDiagnostic) int {
			if order := a.StartedAt.Compare(b.StartedAt); order != 0 {
				return order
			}
			return cmp.Compare(a.ID, b.ID)
		})
		// Completion-time keysets use the existing account index. Retain only
		// the frozen REST-duration window so callbacks remain start-ordered,
		// including requests crossing a 5000-row completion boundary.
		last := len(rows) < 5000
		var frontier time.Time
		if len(rows) > 0 {
			after, afterID = rows[len(rows)-1].CreatedAt, rows[len(rows)-1].ID
			frontier = after.Add(-maxDuration)
		}
		emitted := 0
		for _, row := range pending {
			if !last && !row.StartedAt.Before(frontier) {
				break
			}
			if err := yield(row); err != nil {
				return err
			}
			emitted++
		}
		retained := copy(pending, pending[emitted:])
		clear(pending[retained:])
		pending = pending[:retained]
		if last {
			return nil
		}
	}
}

// Aggregate one indexed completion-time hour at a time, then merge the original
// (path, PostgreSQL response hash) versions without repeatedly detoasting a week
// of route responses in one statement.
func (s *Store) ReplayDiagnosticRoutes(ctx context.Context, accountID string, boundary ReplayBackfillBoundary) ([]ReplayDiagnostic, error) {
	end := boundary.At.Add(min(boundary.MaxDuration, replayDiagnosticRequestTimeout))
	var first struct {
		CreatedAt *time.Time
	}
	db, cancel := s.query(ctx)
	err := db.Transaction(func(tx *gorm.DB) error {
		if err := tx.Exec("SET LOCAL statement_timeout = '9s'").Error; err != nil {
			return err
		}
		return tx.Raw(`SELECT created_at FROM requests
			WHERE account_id = ? AND id <= ? AND method = 'GET'
			AND split_part(path, chr(63), 1) = '/v1/world/route'
			AND duration_ms <= ? AND created_at < ?
			AND created_at - duration_ms * interval '1 millisecond' < ?
			ORDER BY created_at, id LIMIT 1`,
			accountID, boundary.MaxRequestID, replayDiagnosticRequestTimeout.Milliseconds(), end, boundary.At).Scan(&first).Error
	})
	cancel()
	if err != nil {
		return nil, fmt.Errorf("read route versions: %w", databaseError{cause: err})
	}
	if first.CreatedAt == nil {
		return nil, nil
	}
	type versionKey struct {
		path string
		hash string
	}
	type routeVersion struct {
		ReplayDiagnostic
		Hash string
	}
	versions := make(map[versionKey]ReplayDiagnostic)
	for start := first.CreatedAt.UTC().Truncate(time.Hour); start.Before(end); {
		windowEnd := start.Add(time.Hour)
		if windowEnd.After(end) {
			windowEnd = end
		}
		db, cancel := s.query(ctx)
		var batch []routeVersion
		err := db.Transaction(func(tx *gorm.DB) error {
			if err := tx.Exec("SET LOCAL statement_timeout = '9s'").Error; err != nil {
				return err
			}
			return tx.Raw(`WITH versions AS (
				SELECT path, md5(response::text) AS hash, min(id) AS id,
				min(created_at - duration_ms * interval '1 millisecond') AS started_at,
				min(created_at) AS created_at, max(created_at) AS last_seen_at, count(*) AS count
				FROM requests WHERE account_id = ? AND id <= ? AND method = 'GET'
				AND split_part(path, chr(63), 1) = '/v1/world/route'
				AND created_at >= ? AND created_at < ? AND duration_ms <= ?
				AND created_at - duration_ms * interval '1 millisecond' < ?
				GROUP BY path, md5(response::text)
			) SELECT v.id, r.method, v.path, r.status, v.started_at, v.created_at, v.last_seen_at,
				v.hash, v.count, convert_to(r.response::text, 'UTF8') AS response, r.error
			FROM versions v JOIN requests r ON r.id = v.id`,
				accountID, boundary.MaxRequestID, start, windowEnd, replayDiagnosticRequestTimeout.Milliseconds(), boundary.At).Scan(&batch).Error
		})
		cancel()
		if err != nil {
			return nil, fmt.Errorf("read route versions: %w", databaseError{cause: err})
		}
		for _, version := range batch {
			key := versionKey{path: version.Path, hash: version.Hash}
			row, exists := versions[key]
			if !exists {
				row := version.ReplayDiagnostic
				row.SeenAt = []time.Time{version.StartedAt, version.LastSeenAt}
				versions[key] = row
				continue
			}
			firstID := min(row.ID, version.ID)
			startedAt := row.StartedAt
			if version.StartedAt.Before(startedAt) {
				startedAt = version.StartedAt
			}
			createdAt := row.CreatedAt
			if version.CreatedAt.Before(createdAt) {
				createdAt = version.CreatedAt
			}
			lastSeenAt := row.LastSeenAt
			if version.LastSeenAt.After(lastSeenAt) {
				lastSeenAt = version.LastSeenAt
			}
			count := row.Count + version.Count
			seenAt := append(row.SeenAt, version.StartedAt, version.LastSeenAt)
			if version.ID < row.ID {
				row = version.ReplayDiagnostic
			}
			row.ID, row.StartedAt, row.CreatedAt = firstID, startedAt, createdAt
			row.LastSeenAt, row.Count = lastSeenAt, count
			row.SeenAt = seenAt
			versions[key] = row
		}
		start = windowEnd
	}
	rows := make([]ReplayDiagnostic, 0, len(versions))
	for _, row := range versions {
		rows = append(rows, row)
	}
	slices.SortFunc(rows, func(a, b ReplayDiagnostic) int {
		if order := a.StartedAt.Compare(b.StartedAt); order != 0 {
			return order
		}
		return cmp.Compare(a.ID, b.ID)
	})
	return rows, nil
}

// ReplayDiagnosticEventPage returns one bounded keyset page. Backfill consumes
// and releases these rows while assembling pass windows instead of loading the
// retained event stream into memory.
func (s *Store) ReplayDiagnosticEventPage(ctx context.Context, accountID string, before, after time.Time, afterID int64) ([]ReplayEvent, error) {
	db, cancel := s.query(ctx)
	defer cancel()
	var batch []ReplayEvent
	err := db.Transaction(func(tx *gorm.DB) error {
		if err := tx.Exec("SET LOCAL statement_timeout = '9s'").Error; err != nil {
			return err
		}
		// Reject compact extreme JSONB numbers before any decimal rendering.
		return tx.Raw(`WITH page AS MATERIALIZED (
			SELECT id, event_id, event_type, payload, created_at, pg_column_size(payload) AS stored_bytes
			FROM events WHERE account_id = ? AND created_at < ? AND (created_at, id) > (?, ?)
			ORDER BY created_at, id LIMIT ?
		), guarded AS MATERIALIZED (
			SELECT *, CASE WHEN stored_bytes > ? THEN true
				ELSE jsonb_path_exists(payload, ?::jsonpath) END AS oversize FROM page
		), sized AS MATERIALIZED (
			SELECT *, CASE WHEN NOT oversize THEN octet_length(payload::text)
				ELSE stored_bytes END AS payload_bytes FROM guarded
		) SELECT id, event_id, event_type AS type, created_at,
			CASE WHEN NOT oversize AND payload_bytes <= ?
				THEN convert_to(payload::text, 'UTF8') END AS payload,
			oversize OR payload_bytes > ? AS oversize, payload_bytes
			FROM sized ORDER BY created_at, id`,
			accountID, before, after, afterID, MaxReplayEventCount, MaxReplayEventBytes,
			`strict $.** ? (@.type() == "number" && (@ > 1e100 || @ < -1e100 || (@ != 0 && @ < 1e-100 && @ > -1e-100)))`,
			MaxReplayEventBytes, MaxReplayEventBytes).Scan(&batch).Error
	})
	if err != nil {
		return nil, fmt.Errorf("read diagnostic events: %w", databaseError{cause: err})
	}
	return batch, nil
}

func (s *Store) RecordReplayBackfillFrame(ctx context.Context, frame ReplayFrame) (ReplayFrameWrite, error) {
	if frame.Source != "diagnostic_backfill" {
		return ReplayFrameWrite{}, ErrInvalidInput
	}
	return s.recordReplayFrame(ctx, frame)
}

// ReplayBytes includes logical tape bytes and physical tables/indexes/TOAST.
// Physical size can lag pruning until autovacuum reclaims dead rows.
func (s *Store) ReplayBytes(ctx context.Context) (int64, int64, int64, error) {
	db, cancel := s.query(ctx)
	defer cancel()
	var sizes struct {
		Stored   int64
		Daily    int64
		Physical int64
	}
	err := db.Raw(`SELECT
		(SELECT COALESCE(sum(stored_bytes), 0) FROM replay_payloads) +
		(SELECT COALESCE(sum(stored_bytes), 0) FROM replay_frames) AS stored,
		(SELECT COALESCE(sum(stored_bytes), 0) FROM replay_payloads WHERE last_seen_at >= CURRENT_TIMESTAMP - interval '1 day') +
		(SELECT COALESCE(sum(stored_bytes), 0) FROM replay_frames WHERE observed_at >= CURRENT_TIMESTAMP - interval '1 day') AS daily,
		pg_total_relation_size('replay_payloads') + pg_total_relation_size('replay_frames') +
		pg_total_relation_size('replay_plans') + pg_total_relation_size('replay_action_links') AS physical`).Scan(&sizes).Error
	if err != nil {
		return 0, 0, 0, fmt.Errorf("measure replay bytes: %w", databaseError{cause: err})
	}
	return sizes.Stored, sizes.Daily, sizes.Physical, nil
}
