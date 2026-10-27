package store

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"

	"gorm.io/gorm"
)

func TestReplayDiagnosticsExcludeLongEventStreamsWithoutHoldingTheFrontier(t *testing.T) {
	db, _, _ := integrationStore(t)
	const accountID = "stream-frontier"
	saveTestAccount(t, db, accountID)
	ctx := context.Background()
	at := time.Date(2026, 10, 26, 0, 0, 0, 0, time.UTC)
	if err := db.db.Exec(`INSERT INTO requests (account_id, method, path, status, duration_ms, request, response, created_at)
		SELECT ?, 'GET', '/v1/company/profile', 200, 10, '{}',
		jsonb_build_object('company_id', 'company', 'padding', repeat('x', 1024)),
		CAST(? AS timestamptz) + n * interval '1 second' + interval '10 milliseconds'
		FROM generate_series(1, 10500) AS n`, accountID, at).Error; err != nil {
		t.Fatal(err)
	}
	if err := db.db.Exec(`INSERT INTO requests (account_id, method, path, status, duration_ms, request, response, created_at)
		VALUES (?, 'GET', '/v1/world/clock', 200, 45001, '{}', '{"total_game_hours":100}', ?)`,
		accountID, at.Add(5005*time.Second)).Error; err != nil {
		t.Fatal(err)
	}
	for i, path := range []string{"/v1/event/sse", "/v1/event/sse?cursor=retained", "/v1/event/subscribe?channel=world"} {
		if err := db.db.Exec(`INSERT INTO requests (account_id, method, path, status, duration_ms, request, response, created_at)
			VALUES (?, 'GET', ?, 200, ?, '{}', '{}', ?)`, accountID, path, (8 * 24 * time.Hour).Milliseconds(), at.Add(48*time.Hour+time.Duration(i)*time.Second)).Error; err != nil {
			t.Fatal(err)
		}
	}
	boundary, err := db.ReplayBackfillBoundary(ctx, accountID)
	if err != nil {
		t.Fatal(err)
	}
	if boundary.MaxDuration != 45001*time.Millisecond || boundary.DurationGaps != 0 || !boundary.At.Equal(at.Add(10500*time.Second+10*time.Millisecond+time.Microsecond)) {
		t.Fatalf("event lifetimes poisoned the frozen REST boundary: %+v", boundary)
	}
	readPages := 0
	const callback = "test:replay_stream_frontier_pages"
	if err := db.db.Callback().Row().Before("gorm:row").Register(callback, func(*gorm.DB) {
		readPages++
	}); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := db.db.Callback().Row().Remove(callback); err != nil {
			t.Error(err)
		}
	})
	count := 0
	clocks := 0
	var previous ReplayDiagnostic
	err = db.ScanReplayDiagnostics(ctx, accountID, boundary, func(row ReplayDiagnostic) error {
		if count == 0 && readPages != 1 {
			t.Fatalf("reader held the first page until %d completion pages were buffered", readPages)
		}
		if strings.HasPrefix(row.Path, "/v1/event/") {
			t.Fatalf("event transport diagnostic leaked into the REST scan: %+v", row)
		}
		if count > 0 && (row.StartedAt.Before(previous.StartedAt) || row.StartedAt.Equal(previous.StartedAt) && row.ID <= previous.ID) {
			t.Fatalf("request-start callback ordering regressed at %d: %+v after %+v", count, row, previous)
		}
		if row.Path == "/v1/world/clock" {
			clocks++
			if !row.StartedAt.Equal(at.Add(5005*time.Second - 45001*time.Millisecond)) {
				t.Fatalf("long REST request start was changed: %+v", row)
			}
		}
		previous = row
		count++
		return nil
	})
	if err != nil || count != 10501 || clocks != 1 || readPages != 3 {
		t.Fatalf("stream-safe scan: rows=%d clocks=%d pages=%d err=%v", count, clocks, readPages, err)
	}
}

func TestReplayDiagnosticDurationCapSkipsAndReportsRESTOutliers(t *testing.T) {
	db, _, _ := integrationStore(t)
	const accountID = "duration-gaps"
	saveTestAccount(t, db, accountID)
	ctx := context.Background()
	at := time.Date(2026, 10, 26, 0, 0, 0, 0, time.UTC)
	for _, row := range []struct {
		path     string
		status   int
		start    time.Duration
		duration time.Duration
	}{
		{"/v1/world/clock", 0, time.Second, 45001 * time.Millisecond},
		{"/v1/world/clock?allowance=max", 200, 3 * time.Second, 46 * time.Second},
		{"/v1/company/profile", 200, 50 * time.Second, 10 * time.Millisecond},
		{"/v1/company/profile?outlier=true", 200, 2 * time.Second, 46001 * time.Millisecond},
		{"/v1/world/route?from=from&to=to", 200, 3 * time.Second, time.Hour},
	} {
		if err := db.db.Exec(`INSERT INTO requests (account_id, method, path, status, duration_ms, request, response, created_at)
			VALUES (?, 'GET', ?, ?, ?, '{}', '{}', ?)`, accountID, row.path, row.status, row.duration.Milliseconds(), at.Add(row.start+row.duration)).Error; err != nil {
			t.Fatal(err)
		}
	}
	boundary, err := db.ReplayBackfillBoundary(ctx, accountID)
	if err != nil || boundary.MaxDuration != 46*time.Second || boundary.DurationGaps != 2 ||
		!boundary.At.Equal(at.Add(50*time.Second+10*time.Millisecond+time.Microsecond)) {
		t.Fatalf("REST outliers poisoned the boundary or were not counted: %+v, %v", boundary, err)
	}
	var rows []ReplayDiagnostic
	if err := db.ScanReplayDiagnostics(ctx, accountID, boundary, func(row ReplayDiagnostic) error {
		rows = append(rows, row)
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	if len(rows) != 3 || rows[0].Path != "/v1/world/clock" || rows[0].Status != 0 ||
		!rows[0].StartedAt.Equal(at.Add(time.Second)) || rows[1].Path != "/v1/world/clock?allowance=max" ||
		rows[2].Path != "/v1/company/profile" {
		t.Fatalf("overlong REST diagnostics were imported or valid starts were clamped: %+v", rows)
	}
	routes, err := db.ReplayDiagnosticRoutes(ctx, accountID, boundary)
	if err != nil || len(routes) != 0 {
		t.Fatalf("route outlier bypassed the REST timeout cap: %+v, %v", routes, err)
	}
}

func TestReplayDiagnosticRoutesMergeHourlyVersionsWithoutChangingPathHashes(t *testing.T) {
	db, _, _ := integrationStore(t)
	const accountID = "hourly-routes"
	saveTestAccount(t, db, accountID)
	saveTestAccount(t, db, "other-routes")
	ctx := context.Background()
	at := time.Date(2026, 10, 25, 23, 59, 30, 0, time.UTC)
	const path = "/v1/world/route?from=from&ship_class=hoy&to=to"
	const reorderedPath = "/v1/world/route?to=to&from=from&ship_class=hoy"
	const versionA = `{"from_port_id":"from","to_port_id":"to","legs":[{"distance_nm":24}]}`
	const reformattedA = `{ "legs": [{"distance_nm":24}], "to_port_id": "to", "from_port_id": "from" }`
	const versionB = `{"from_port_id":"from","to_port_id":"to","legs":[{"distance_nm":36}]}`
	type expectedVersion struct {
		firstID   int64
		startedAt time.Time
		createdAt time.Time
		lastSeen  time.Time
		count     int64
		response  []byte
	}
	type versionKey struct {
		path     string
		distance int
	}
	expected := make(map[versionKey]expectedVersion)
	var maxID int64
	for _, fixture := range []struct {
		path       string
		body       string
		distance   int
		completion time.Duration
		duration   time.Duration
	}{
		{path, versionA, 24, 2*time.Hour + 20*time.Second, 10 * time.Millisecond},
		{path, reformattedA, 24, 20 * time.Second, 10 * time.Millisecond},
		{path, versionA, 24, time.Hour + 20*time.Second, 45 * time.Second},
		{path, versionA, 24, 2*time.Hour + 40*time.Second, 10 * time.Millisecond},
		{path, versionA, 24, 9 * time.Second, 20 * time.Second},
		{path, versionB, 36, time.Hour + 10*time.Second, 10 * time.Millisecond},
		{path, versionB, 36, 2*time.Hour + 10*time.Second, 10 * time.Millisecond},
		{reorderedPath, versionA, 24, 3 * time.Minute, 10 * time.Millisecond},
	} {
		var inserted struct {
			ID       int64
			Response []byte
		}
		createdAt := at.Add(fixture.completion)
		startedAt := createdAt.Add(-fixture.duration)
		if err := db.db.Raw(`INSERT INTO requests (account_id, method, path, status, duration_ms, request, response, created_at)
			VALUES (?, 'GET', ?, 200, ?, '{}', CAST(? AS jsonb), ?)
			RETURNING id, convert_to(response::text, 'UTF8') AS response`,
			accountID, fixture.path, fixture.duration.Milliseconds(), fixture.body, createdAt).Scan(&inserted).Error; err != nil {
			t.Fatal(err)
		}
		maxID = max(maxID, inserted.ID)
		key := versionKey{fixture.path, fixture.distance}
		want, exists := expected[key]
		if !exists {
			want = expectedVersion{inserted.ID, startedAt, createdAt, createdAt, 0, inserted.Response}
		}
		want.count++
		if startedAt.Before(want.startedAt) {
			want.startedAt = startedAt
		}
		if createdAt.Before(want.createdAt) {
			want.createdAt = createdAt
		}
		if createdAt.After(want.lastSeen) {
			want.lastSeen = createdAt
		}
		expected[key] = want
	}
	boundary := ReplayBackfillBoundary{MaxRequestID: maxID, At: at.Add(3 * time.Hour), MaxDuration: 45 * time.Second}
	// Backdated rows added after freezing and rows from another account must
	// not change a version's response, extrema or occurrence count.
	for _, account := range []string{accountID, "other-routes"} {
		if err := db.db.Exec(`INSERT INTO requests (account_id, method, path, status, duration_ms, request, response, created_at)
			VALUES (?, 'GET', ?, 200, 10, '{}', CAST(? AS jsonb), ?)`, account, path, versionA, at.Add(time.Minute)).Error; err != nil {
			t.Fatal(err)
		}
	}
	queries := 0
	const callback = "test:replay_hourly_route_windows"
	if err := db.db.Callback().Row().Before("gorm:row").Register(callback, func(*gorm.DB) {
		queries++
	}); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := db.db.Callback().Row().Remove(callback); err != nil {
			t.Error(err)
		}
	})
	rows, err := db.ReplayDiagnosticRoutes(ctx, accountID, boundary)
	if err != nil || len(rows) != len(expected) || queries < 4 {
		t.Fatalf("hourly route versions: rows=%d windows=%d err=%v", len(rows), queries, err)
	}
	var previous ReplayDiagnostic
	for i, row := range rows {
		var body struct {
			Legs []struct {
				Distance int `json:"distance_nm"`
			} `json:"legs"`
		}
		if err := json.Unmarshal(row.Response, &body); err != nil || len(body.Legs) != 1 {
			t.Fatalf("route response was not retained: %s, %v", row.Response, err)
		}
		key := versionKey{row.Path, body.Legs[0].Distance}
		want, exists := expected[key]
		if !exists || row.ID != want.firstID || row.Count != want.count || !row.StartedAt.Equal(want.startedAt) ||
			!row.CreatedAt.Equal(want.createdAt) || !row.LastSeenAt.Equal(want.lastSeen) || !bytes.Equal(row.Response, want.response) {
			t.Fatalf("hourly merge changed version %+v: got=%+v want=%+v", key, row, want)
		}
		if len(row.SeenAt) == 0 || len(row.SeenAt)%2 != 0 {
			t.Fatalf("hourly route sightings were lost: %+v", row)
		}
		var firstSeen, lastSeen time.Time
		for _, seenAt := range row.SeenAt {
			if firstSeen.IsZero() || seenAt.Before(firstSeen) {
				firstSeen = seenAt
			}
			if seenAt.After(lastSeen) {
				lastSeen = seenAt
			}
		}
		if !firstSeen.Equal(row.StartedAt) || !lastSeen.Equal(row.LastSeenAt) {
			t.Fatalf("hourly route sightings changed their endpoints: %+v", row)
		}
		if i > 0 && (row.StartedAt.Before(previous.StartedAt) || row.StartedAt.Equal(previous.StartedAt) && row.ID <= previous.ID) {
			t.Fatalf("merged route ordering regressed: %+v after %+v", row, previous)
		}
		delete(expected, key)
		previous = row
	}
	if len(expected) != 0 {
		t.Fatalf("route path/hash versions were collapsed: %+v", expected)
	}
}

func TestReplayDiagnosticRouteWindowFailureReturnsNoPartialVersions(t *testing.T) {
	db, _, _ := integrationStore(t)
	const accountID = "route-window-error"
	saveTestAccount(t, db, accountID)
	ctx := context.Background()
	at := time.Date(2026, 10, 26, 0, 0, 0, 0, time.UTC)
	if err := db.db.Exec(`INSERT INTO requests (account_id, method, path, status, duration_ms, request, response, created_at)
		SELECT ?, 'GET', ?, 200, 10, '{}', '{"legs":[]}',
		CAST(? AS timestamptz) + n * interval '1 hour' + interval '1 second'
		FROM generate_series(0, 1) AS n`, accountID, "/v1/world/route?from=from&to=to", at).Error; err != nil {
		t.Fatal(err)
	}
	boundary, err := db.ReplayBackfillBoundary(ctx, accountID)
	if err != nil {
		t.Fatal(err)
	}
	failure := errors.New("synthetic private database details")
	queries := 0
	const callback = "test:replay_route_window_error"
	if err := db.db.Callback().Row().Before("gorm:row").Register(callback, func(tx *gorm.DB) {
		queries++
		if queries == 3 {
			tx.AddError(failure)
		}
	}); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := db.db.Callback().Row().Remove(callback); err != nil {
			t.Error(err)
		}
	})
	rows, err := db.ReplayDiagnosticRoutes(ctx, accountID, boundary)
	if !errors.Is(err, failure) || len(rows) != 0 || queries != 3 || strings.Contains(err.Error(), "private database details") {
		t.Fatalf("window failure exposed partial routes or unsafe details: rows=%d queries=%d err=%v", len(rows), queries, err)
	}
	if ctx.Err() != nil {
		t.Fatal("route query failure canceled the caller context")
	}
}

func TestReplayDiagnosticEventPagesStayBoundedAndKeysetOrdered(t *testing.T) {
	db, _, _ := integrationStore(t)
	const accountID = "paged-replay-events"
	saveTestAccount(t, db, accountID)
	saveTestAccount(t, db, "other-replay-events")
	ctx := context.Background()
	at := time.Date(2026, 10, 26, 12, 0, 0, 0, time.UTC)
	const total = MaxReplayEventCount*2 + 7
	if err := db.db.Exec(`INSERT INTO events (account_id, event_id, event_type, payload, created_at)
		SELECT ?, 'page-' || n, 'synthetic', jsonb_build_object('padding', repeat('x', 1024)), ?
		FROM generate_series(1, ?) AS n`, accountID, at, total).Error; err != nil {
		t.Fatal(err)
	}
	before := at.Add(time.Minute)
	for _, fixture := range []struct {
		account string
		at      time.Time
	}{
		{"other-replay-events", at},
		{accountID, before},
	} {
		if err := db.db.Exec(`INSERT INTO events (account_id, event_id, event_type, payload, created_at)
			VALUES (?, 'excluded', 'synthetic', '{}', ?)`, fixture.account, fixture.at).Error; err != nil {
			t.Fatal(err)
		}
	}
	var after time.Time
	var afterID int64
	count, pages := 0, 0
	for {
		page, err := db.ReplayDiagnosticEventPage(ctx, accountID, before, after, afterID)
		if err != nil || len(page) == 0 || len(page) > MaxReplayEventCount {
			t.Fatalf("bounded event page: rows=%d err=%v", len(page), err)
		}
		pages++
		for _, event := range page {
			if !event.CreatedAt.Equal(at) || event.ID <= afterID || event.Type != "synthetic" || len(event.Payload) == 0 {
				t.Fatalf("event keyset lost a tie or crossed its account/window: %+v", event)
			}
			after, afterID = event.CreatedAt, event.ID
			count++
		}
		if len(page) < MaxReplayEventCount {
			break
		}
	}
	if count != total || pages != 3 {
		t.Fatalf("paged %d events in %d pages, want %d in 3", count, pages, total)
	}
	page, err := db.ReplayDiagnosticEventPage(ctx, accountID, before, after, afterID)
	if err != nil || len(page) != 0 {
		t.Fatalf("completed event keyset repeated data: rows=%d err=%v", len(page), err)
	}
	canceled, cancel := context.WithCancel(ctx)
	cancel()
	if _, err := db.ReplayDiagnosticEventPage(canceled, accountID, before, time.Time{}, 0); err == nil {
		t.Fatal("event page ignored cancellation")
	}
}

func TestReplayDiagnosticEventPageProjectsOversizePayloadsAsGaps(t *testing.T) {
	db, _, _ := integrationStore(t)
	const account = "bounded-event-bytes"
	saveTestAccount(t, db, account)
	ctx := context.Background()
	at := time.Date(2026, 10, 26, 12, 0, 0, 0, time.UTC)
	expanded := json.RawMessage(`{"values":[` + strings.Repeat("1e130000,", 8) + `1e130000]}`)
	if err := db.db.Exec(`INSERT INTO events (account_id, event_id, event_type, payload, created_at)
		VALUES (?, 'expanded', 'synthetic', CAST(? AS jsonb), ?)`, account, string(expanded), at).Error; err != nil {
		t.Fatal(err)
	}
	var expandedSize struct {
		Normalized int64
		Stored     int64
	}
	if err := db.db.Raw(`SELECT octet_length(payload::text) AS normalized, pg_column_size(payload) AS stored
		FROM events WHERE account_id = ? AND event_id = ?`, account, "expanded").Scan(&expandedSize).Error; err != nil {
		t.Fatal(err)
	}
	if expandedSize.Normalized <= MaxReplayEventBytes {
		t.Fatalf("compact exponent fixture did not expand beyond the cap: %d", expandedSize.Normalized)
	}
	if err := db.db.Exec(`INSERT INTO events (account_id, event_id, event_type, payload, created_at)
		VALUES (?, 'text-overflow', 'synthetic', jsonb_build_object('padding', repeat('x', ?)), ?)`,
		account, 2*MaxReplayEventBytes, at.Add(3*time.Second)).Error; err != nil {
		t.Fatal(err)
	}
	var textSize struct {
		Normalized int64
		Stored     int64
	}
	if err := db.db.Raw(`SELECT octet_length(payload::text) AS normalized, pg_column_size(payload) AS stored
		FROM events WHERE account_id = ? AND event_id = 'text-overflow'`, account).Scan(&textSize).Error; err != nil {
		t.Fatal(err)
	}
	if textSize.Stored > MaxReplayEventBytes || textSize.Normalized <= MaxReplayEventBytes {
		t.Fatalf("text-size fixture did not bypass the physical-size guard: %+v", textSize)
	}
	if err := db.db.Exec("ALTER TABLE events ALTER COLUMN payload SET STORAGE EXTERNAL").Error; err != nil {
		t.Fatal(err)
	}
	if err := db.db.Exec(`INSERT INTO events (account_id, event_id, event_type, payload, created_at)
		VALUES (?, 'physical', 'synthetic', jsonb_build_object('padding', repeat('x', ?)), ?),
		(?, 'healthy', 'synthetic', '{"value":1}', ?)`,
		account, MaxReplayEventBytes+1, at.Add(time.Second), account, at.Add(2*time.Second)).Error; err != nil {
		t.Fatal(err)
	}
	var physical int64
	if err := db.db.Raw("SELECT pg_column_size(payload) FROM events WHERE account_id = ? AND event_id = ?", account, "physical").Scan(&physical).Error; err != nil {
		t.Fatal(err)
	}
	if physical <= MaxReplayEventBytes {
		t.Fatalf("physical fixture did not exceed the storage bound: %d", physical)
	}
	gapBytes := map[string]int64{"expanded": expandedSize.Stored, "physical": physical, "text-overflow": textSize.Normalized}
	page, err := db.ReplayDiagnosticEventPage(ctx, account, at.Add(time.Minute), time.Time{}, 0)
	if err != nil || len(page) != 4 {
		t.Fatalf("bounded event projection: rows=%d err=%v", len(page), err)
	}
	for _, event := range page {
		if event.EventID == "healthy" {
			if event.Oversize || event.PayloadBytes != int64(len(event.Payload)) || len(event.Payload) == 0 {
				t.Fatalf("healthy event became a gap: %+v", event)
			}
		} else if !event.Oversize || len(event.Payload) != 0 || event.PayloadBytes != gapBytes[event.EventID] || event.Type != "synthetic" {
			t.Fatalf("oversize event was materialized instead of projected as a gap: %+v", event)
		}
	}
}

func TestReplayDiagnosticEventPageRejectsCompactNumericExpansionBeforeRendering(t *testing.T) {
	db, _, _ := integrationStore(t)
	const account = "compact-numeric-event"
	saveTestAccount(t, db, account)
	const count = 3600
	payload := json.RawMessage(`{"values":[` + strings.Repeat("1e130000,", count-1) + `1e130000]}`)
	at := time.Date(2026, 10, 26, 12, 0, 0, 0, time.UTC)
	if err := db.db.Exec(`INSERT INTO events (account_id, event_id, event_type, payload, created_at)
		VALUES (?, 'numeric-expansion', 'synthetic', CAST(? AS jsonb), ?)`, account, string(payload), at).Error; err != nil {
		t.Fatal(err)
	}
	var fixture struct {
		Stored int64
		Count  int
	}
	if err := db.db.Raw(`SELECT pg_column_size(payload) AS stored, jsonb_array_length(payload->'values') AS count
		FROM events WHERE account_id = ?`, account).Scan(&fixture).Error; err != nil {
		t.Fatal(err)
	}
	if fixture.Count != count || fixture.Stored >= MaxReplayEventBytes {
		t.Fatalf("numeric fixture was not compact: %+v", fixture)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	started := time.Now()
	page, err := db.ReplayDiagnosticEventPage(ctx, account, at.Add(time.Minute), time.Time{}, 0)
	if err != nil || ctx.Err() != nil || len(page) != 1 {
		t.Fatalf("compact numeric gap exceeded its deadline: rows=%d err=%v context=%v", len(page), err, ctx.Err())
	}
	event := page[0]
	if !event.Oversize || len(event.Payload) != 0 || event.PayloadBytes != fixture.Stored ||
		event.EventID != "numeric-expansion" || event.Type != "synthetic" || !event.CreatedAt.Equal(at) {
		t.Fatalf("numeric expansion was rendered instead of gapped: %+v, stored bytes=%d", event, fixture.Stored)
	}
	t.Logf("%d compact numeric exponents projected as a gap in %s", count, time.Since(started))
}
