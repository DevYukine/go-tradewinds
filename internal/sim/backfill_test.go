package sim

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/DevYukine/go-tradewinds/internal/engine"
	"github.com/DevYukine/go-tradewinds/internal/store"
)

func TestBackfillMissingCursorPageDoesNotPublishPartialFleet(t *testing.T) {
	db, raw := backfillAcceptanceStore(t)
	at := time.Date(2026, 10, 26, 12, 0, 0, 0, time.UTC)
	backfillAcceptanceRequest(t, db, raw, "backfill", "GET", "/v1/world/clock", 200, at, `{"total_game_hours":100}`)
	ships := make([]map[string]string, 200)
	for i := range ships {
		ships[i] = map[string]string{"id": fmt.Sprintf("ship-%03d", i)}
	}
	page, err := json.Marshal(ships)
	if err != nil {
		t.Fatal(err)
	}
	backfillAcceptanceRequest(t, db, raw, "backfill", "GET", "/v1/fleet/ships?limit=200", 200, at.Add(time.Second), string(page))
	backfillAcceptanceRequest(t, db, raw, "backfill", "GET", "/v1/fleet/ships?cursor=ship-199&limit=200", 503, at.Add(2*time.Second), `{"code":"unavailable"}`)
	if _, err := Backfill(context.Background(), db, "backfill"); err != nil {
		t.Fatal(err)
	}
	frames := backfillAcceptanceFrames(t, db)
	if len(frames) != 1 {
		t.Fatalf("clock pass produced %d frames", len(frames))
	}
	if partial := frames[0].Datasets["ships"]; len(partial.Data) != 0 {
		t.Fatal("missing terminal cursor page was imported as a complete fleet")
	}
}

func TestBackfillFirstWaveAnchorHandlesStaggeredStarts(t *testing.T) {
	db, raw := backfillAcceptanceStore(t)
	at := time.Date(2026, 10, 26, 12, 0, 0, 0, time.UTC)
	for i := range 2 {
		start := at.Add(time.Duration(i) * 3 * time.Minute)
		backfillAcceptanceRequest(t, db, raw, "backfill", "GET", "/v1/auth/whoami", 200, start, `{"company_id":"company-backfill"}`)
		backfillAcceptanceRequest(t, db, raw, "backfill", "GET", "/v1/company/profile", 200, start.Add(100*time.Millisecond), fmt.Sprintf(`{"pass":%d}`, i))
		backfillAcceptanceTimedRequest(t, db, raw, "backfill", "GET", "/v1/world/clock", 200, start.Add(2*time.Second), time.Second, `{"total_game_hours":100}`)
		if i == 0 {
			backfillAcceptanceRequest(t, db, raw, "backfill", "POST", "/v1/market/place-order", 200, start.Add(10*time.Second), `{"filled":1}`)
		}
	}
	if _, err := Backfill(context.Background(), db, "backfill"); err != nil {
		t.Fatal(err)
	}
	frames := backfillAcceptanceFrames(t, db)
	if len(frames) != 2 || !frames[0].Decision || frames[1].Decision {
		t.Fatalf("staggered first-wave starts crossed the mutation boundary: %+v", frames)
	}
	for i, frame := range frames {
		if !frame.ObservedAt.Equal(at.Add(time.Duration(i)*3*time.Minute)) ||
			!backfillAcceptanceJSONEqual(t, frame.Datasets["profile"].Data, []byte(fmt.Sprintf(`{"pass":%d}`, i))) {
			t.Fatalf("pass %d did not start at its first-wave anchor", i)
		}
	}
}

func TestBackfillWhoamiAfterClockKeepsConsecutivePassesSeparate(t *testing.T) {
	db, raw := backfillAcceptanceStore(t)
	ctx := context.Background()
	at := time.Date(2026, 10, 26, 12, 0, 0, 0, time.UTC)
	var clockIDs []int64
	for i := range 2 {
		start := at.Add(time.Duration(i) * 3 * time.Minute)
		backfillAcceptanceTimedRequest(t, db, raw, "backfill", "GET", "/v1/company/profile", 200, start, 10*time.Millisecond, fmt.Sprintf(`{"pass":%d}`, i))
		backfillAcceptanceTimedRequest(t, db, raw, "backfill", "GET", "/v1/world/clock", 200, start.Add(100*time.Microsecond), 6*time.Millisecond, fmt.Sprintf(`{"total_game_hours":%d}`, 100+i))
		var clockID int64
		if err := raw.QueryRowContext(ctx, "SELECT max(id) FROM requests WHERE account_id = $1", "backfill").Scan(&clockID); err != nil {
			t.Fatal(err)
		}
		clockIDs = append(clockIDs, clockID)
		// The first wave runs concurrently, so whoami can start after clock
		// in every pass, not only during the first recorded refresh.
		backfillAcceptanceTimedRequest(t, db, raw, "backfill", "GET", "/v1/auth/whoami", 200, start.Add(200*time.Microsecond), 11*time.Millisecond, `{"company_id":"company-backfill"}`)
		backfillAcceptanceRequest(t, db, raw, "backfill", "GET", "/v1/treasury/vaults", 200, start.Add(time.Second), fmt.Sprintf(`[{"pass":%d}]`, i))
		backfillAcceptanceRequest(t, db, raw, "backfill", "GET", "/v1/fleet/ships?limit=200", 200, start.Add(2*time.Second), fmt.Sprintf(`[{"id":"ship-%d","pass":%d}]`, i, i))
		backfillAcceptanceRequest(t, db, raw, "backfill", "GET", "/v1/market/book?port_id=quay-port", 200, start.Add(3*time.Second), fmt.Sprintf(`{"port_id":"quay-port","pass":%d}`, i))
		if i == 0 {
			backfillAcceptanceRequest(t, db, raw, "backfill", "POST", "/v1/market/place-order", 200, start.Add(4*time.Second), `{"filled":1}`)
			backfillAcceptanceRequest(t, db, raw, "backfill", "PATCH", "/v1/company/settings", 200, start.Add(5*time.Second), `{"ok":true}`)
		}
	}
	report, err := Backfill(ctx, db, "backfill")
	if err != nil || report.FramesAdded != 2 {
		t.Fatalf("concurrent first-wave passes: report=%+v, err=%v", report, err)
	}
	frames := backfillAcceptanceFrames(t, db)
	if len(frames) != 2 {
		t.Fatalf("two clock passes produced %d frames", len(frames))
	}
	for i, frame := range frames {
		if !frame.ObservedAt.Equal(at.Add(time.Duration(i)*3*time.Minute)) ||
			frame.Generation != fmt.Sprintf("diagnostic:%d", clockIDs[i]) || frame.Decision != (i == 0) {
			t.Fatalf("pass %d reused another pass's anchor or mutation: %+v", i, frame)
		}
		for key, expected := range map[string]string{
			"profile":         fmt.Sprintf(`{"pass":%d}`, i),
			"vaults":          fmt.Sprintf(`[{"pass":%d}]`, i),
			"ships":           fmt.Sprintf(`[{"id":"ship-%d","pass":%d}]`, i, i),
			"books/quay-port": fmt.Sprintf(`{"port_id":"quay-port","pass":%d}`, i),
		} {
			backfillAcceptanceMeta(t, frame, key, true)
			if !backfillAcceptanceJSONEqual(t, frame.Datasets[key].Data, []byte(expected)) {
				t.Fatalf("pass %d %s crossed its request-start boundary", i, key)
			}
		}
	}
}

func TestBackfillLoansRequireCompleteStatusesOrHistory(t *testing.T) {
	db, raw := backfillAcceptanceStore(t)
	ctx := context.Background()
	at := time.Date(2026, 10, 26, 12, 0, 0, 0, time.UTC)
	fullPage := make([]json.RawMessage, 200)
	for i := range fullPage {
		fullPage[i] = json.RawMessage(fmt.Sprintf(`{"id":"loan-%03d","status":"defaulted"}`, i))
	}
	page, err := json.Marshal(fullPage)
	if err != nil {
		t.Fatal(err)
	}
	history := append(slices.Clone(fullPage), json.RawMessage(`{"id":"loan-200","status":"settled"}`))
	historyData, err := json.Marshal(history)
	if err != nil {
		t.Fatal(err)
	}
	type request struct {
		path   string
		status int
		body   string
	}
	active := request{"/v1/loan/list?limit=200&role=borrower&status=active", 200, `[{"id":"active-only","status":"active"}]`}
	failedDefault := request{"/v1/loan/list?limit=200&role=borrower&status=defaulted", 503, `{"code":"unavailable"}`}
	cases := []struct {
		name     string
		requests []request
		expected []byte
	}{
		{name: "missing defaulted", requests: []request{active}},
		{name: "failed defaulted", requests: []request{active, failedDefault}},
		{name: "missing defaulted cursor", requests: []request{
			active,
			{"/v1/loan/list?limit=200&role=borrower&status=defaulted", 200, string(page)},
		}},
		{name: "complete history fallback", requests: []request{
			active, failedDefault,
			{"/v1/loan/list?limit=200&role=borrower", 200, string(page)},
			{"/v1/loan/list?cursor=loan-199&limit=200&role=borrower", 200, `[{"id":"loan-200","status":"settled"}]`},
		}, expected: historyData},
		{name: "incomplete history", requests: []request{
			active, failedDefault,
			{"/v1/loan/list?limit=200&role=borrower", 200, string(page)},
		}},
		{name: "complete statuses", requests: []request{
			{"/v1/loan/list?limit=200&role=borrower&status=active", 200, `[{"id":"shared","status":"active"},{"id":"active-only","status":"active"}]`},
			{"/v1/loan/list?limit=200&role=borrower&status=defaulted", 200, `[{"id":"shared","status":"defaulted"},{"id":"defaulted-only","status":"defaulted"}]`},
		}, expected: []byte(`[{"id":"shared","status":"defaulted"},{"id":"active-only","status":"active"},{"id":"defaulted-only","status":"defaulted"}]`)},
	}
	var expectedGaps []string
	for i, test := range cases {
		start := at.Add(time.Duration(i) * 3 * time.Minute)
		backfillAcceptanceRequest(t, db, raw, "backfill", "GET", "/v1/world/clock", 200, start, fmt.Sprintf(`{"total_game_hours":%d}`, 100+i))
		if len(test.expected) == 0 {
			var clockID int64
			if err := raw.QueryRowContext(ctx, "SELECT max(id) FROM requests WHERE account_id = $1", "backfill").Scan(&clockID); err != nil {
				t.Fatal(err)
			}
			expectedGaps = append(expectedGaps, fmt.Sprintf("diagnostic:%d", clockID))
		}
		for j, row := range test.requests {
			backfillAcceptanceRequest(t, db, raw, "backfill", "GET", row.path, row.status, start.Add(time.Duration(j+1)*time.Second), row.body)
		}
	}
	report, err := Backfill(ctx, db, "backfill")
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(report.IncompleteLoanPasses, expectedGaps) {
		t.Fatalf("incomplete loan generations = %q, want %q", report.IncompleteLoanPasses, expectedGaps)
	}
	frames := backfillAcceptanceFrames(t, db)
	if len(frames) != len(cases) {
		t.Fatalf("loan passes produced %d frames, want %d", len(frames), len(cases))
	}
	for i, test := range cases {
		frame := frames[i]
		backfillAcceptanceMeta(t, frame, "loans", len(test.expected) != 0)
		if len(test.expected) != 0 && !backfillAcceptanceJSONEqual(t, frame.Datasets["loans"].Data, test.expected) {
			t.Fatalf("%s did not preserve the complete loan observation", test.name)
		}
		if len(test.expected) == 0 && !strings.Contains(string(frame.Datasets["loans"].Meta), "complete active and defaulted") {
			t.Fatalf("%s omitted the reason for its loan gap: %s", test.name, frame.Datasets["loans"].Meta)
		}
	}
	var printed bytes.Buffer
	if _, err := report.WriteTo(&printed); err != nil {
		t.Fatal(err)
	}
	for _, generation := range expectedGaps {
		if !strings.Contains(printed.String(), strconv.Quote(generation)) {
			t.Fatalf("loan coverage report omitted %q", generation)
		}
	}
	again, err := Backfill(ctx, db, "backfill")
	if err != nil || again.FramesAdded != 0 || !slices.Equal(again.IncompleteLoanPasses, expectedGaps) {
		t.Fatalf("loan gap reporting changed on rerun: %+v, %v", again, err)
	}
}

func TestBackfillReportsOutOfTimeoutDiagnosticGaps(t *testing.T) {
	db, raw := backfillAcceptanceStore(t)
	at := time.Date(2026, 10, 26, 12, 0, 0, 0, time.UTC)
	backfillAcceptanceRequest(t, db, raw, "backfill", "GET", "/v1/world/clock", 200, at, `{"total_game_hours":100}`)
	backfillAcceptanceTimedRequest(t, db, raw, "backfill", "GET", "/v1/company/profile", 200, at.Add(time.Second), 90*time.Second, `{"must_not_import":true}`)
	report, err := Backfill(context.Background(), db, "backfill")
	if err != nil || report.DurationGaps != 1 {
		t.Fatalf("out-of-timeout diagnostic gap: %+v, %v", report, err)
	}
	frames := backfillAcceptanceFrames(t, db)
	if len(frames) != 1 || !frames[0].ObservedAt.Equal(at) {
		t.Fatalf("outlier changed the clock pass: %+v", frames)
	}
	if _, exists := frames[0].Datasets["profile"]; exists {
		t.Fatal("overlong diagnostic fabricated an in-window profile")
	}
	var printed bytes.Buffer
	if _, err := report.WriteTo(&printed); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(printed.String(), "Diagnostic duration gaps (requests over 46 seconds, including client-timeout slack): 1") {
		t.Fatalf("skipped outlier was not reported as a gap: %s", printed.String())
	}
}

func TestBackfillReportQuotesExternalIdentifiers(t *testing.T) {
	port := "harbor\x1b[31m\nspoof"
	truncatedPort := "broken\rport\a"
	path := "/v1/unknown/\x1b]0;title\a\nrow"
	report := BackfillReport{
		CompleteBookHours:  map[string]float64{port: 1},
		TruncatedOnlyPorts: []string{truncatedPort},
		Paths:              map[string]BackfillPathCount{path: {Unmapped: 1}},
	}
	var printed bytes.Buffer
	if _, err := report.WriteTo(&printed); err != nil {
		t.Fatal(err)
	}
	text := printed.String()
	for _, identifier := range []string{port, truncatedPort, path} {
		if !strings.Contains(text, strconv.Quote(identifier)) || strings.Contains(text, identifier) {
			t.Fatalf("report did not quote an external identifier: %q", text)
		}
	}
	if strings.ContainsAny(text, "\x1b\r\a") {
		t.Fatalf("report emitted terminal control characters: %q", text)
	}
}

func TestBackfillFirstWaveOnlyPassDoesNotReuseItsWhoamiAnchor(t *testing.T) {
	db, raw := backfillAcceptanceStore(t)
	at := time.Date(2026, 10, 26, 12, 0, 0, 0, time.UTC)
	backfillAcceptanceRequest(t, db, raw, "backfill", "GET", "/v1/world/clock", 200, at, `{"total_game_hours":100}`)
	backfillAcceptanceRequest(t, db, raw, "backfill", "GET", "/v1/auth/whoami", 200, at.Add(time.Millisecond), `{"company_id":"company-backfill"}`)
	backfillAcceptanceRequest(t, db, raw, "backfill", "GET", "/v1/company/profile", 200, at.Add(2*time.Millisecond), `{"pass":0}`)
	backfillAcceptanceRequest(t, db, raw, "backfill", "GET", "/v1/world/clock", 200, at.Add(3*time.Minute), `{"total_game_hours":101}`)
	report, err := Backfill(context.Background(), db, "backfill")
	if err != nil || report.FramesAdded != 2 {
		t.Fatalf("first-wave-only pass boundaries: %+v, %v", report, err)
	}
	frames := backfillAcceptanceFrames(t, db)
	if len(frames) != 2 || !frames[0].ObservedAt.Equal(at) || !frames[1].ObservedAt.Equal(at.Add(3*time.Minute)) {
		t.Fatalf("the previous pass's whoami moved the next boundary: %+v", frames)
	}
	if !backfillAcceptanceJSONEqual(t, frames[0].Datasets["profile"].Data, []byte(`{"pass":0}`)) {
		t.Fatal("the first-wave profile moved to another pass")
	}
	if _, exists := frames[1].Datasets["profile"]; exists {
		t.Fatal("the next pass inherited the previous first-wave profile")
	}
}

func TestBackfillFailedClockWithinTimeoutSlackStillSeparatesPasses(t *testing.T) {
	db, raw := backfillAcceptanceStore(t)
	at := time.Date(2026, 10, 26, 12, 0, 0, 0, time.UTC)
	backfillAcceptanceRequest(t, db, raw, "backfill", "GET", "/v1/world/clock", 200, at, `{"total_game_hours":100}`)
	backfillAcceptanceRequest(t, db, raw, "backfill", "GET", "/v1/company/profile", 200, at.Add(time.Second), `{"pass":0}`)
	backfillAcceptanceTimedRequest(t, db, raw, "backfill", "GET", "/v1/world/clock", 0, at.Add(3*time.Minute), 45001*time.Millisecond, `null`)
	backfillAcceptanceRequest(t, db, raw, "backfill", "GET", "/v1/company/profile", 200, at.Add(4*time.Minute), `{"pass":1}`)
	backfillAcceptanceRequest(t, db, raw, "backfill", "GET", "/v1/world/clock", 200, at.Add(6*time.Minute), `{"total_game_hours":102}`)
	report, err := Backfill(context.Background(), db, "backfill")
	if err != nil || report.FramesAdded != 3 || report.DurationGaps != 0 {
		t.Fatalf("timed-out clock was discarded instead of retained as a boundary: %+v, %v", report, err)
	}
	frames := backfillAcceptanceFrames(t, db)
	if len(frames) != 3 {
		t.Fatalf("three clock attempts produced %d frames", len(frames))
	}
	for i, frame := range frames {
		if !frame.ObservedAt.Equal(at.Add(time.Duration(i) * 3 * time.Minute)) {
			t.Fatalf("clock pass %d lost its actual request start: %s", i, frame.ObservedAt)
		}
	}
	backfillAcceptanceMeta(t, frames[1], "clock", false)
	if !backfillAcceptanceJSONEqual(t, frames[1].Datasets["profile"].Data, []byte(`{"pass":1}`)) {
		t.Fatal("the timed-out clock's later inputs merged into its neighbour")
	}
}

func TestBackfillSweepsRetainRoutesForLiveGeometryLifetime(t *testing.T) {
	db, raw := backfillAcceptanceStore(t)
	at := time.Date(2026, 10, 26, 0, 0, 0, 0, time.UTC)
	for i, offset := range []time.Duration{0, engine.RouteGeometryTTL - time.Hour, engine.RouteGeometryTTL + time.Hour, engine.RouteGeometryTTL + 2*time.Hour} {
		backfillAcceptanceRequest(t, db, raw, "backfill", "GET", "/v1/world/clock", 200, at.Add(offset), fmt.Sprintf(`{"total_game_hours":%d}`, 100+i))
	}
	for _, fixture := range []struct {
		to     string
		offset time.Duration
	}{
		{"expired", time.Second},
		{"recurring", 2 * time.Second},
		{"recent", engine.RouteGeometryTTL - time.Hour + time.Second},
		{"recurring", engine.RouteGeometryTTL + 2*time.Hour + time.Second},
	} {
		path := "/v1/world/route?from=quay-port&to=" + fixture.to + "&ship_class=hoy"
		body := fmt.Sprintf(`{"from_port_id":"quay-port","to_port_id":%q,"distance_nm":12}`, fixture.to)
		backfillAcceptanceRequest(t, db, raw, "backfill", "GET", path, 200, at.Add(fixture.offset), body)
	}
	backfillAcceptanceRequest(t, db, raw, "backfill", "GET", "/v1/company/profile", 200, at.Add(engine.RouteGeometryTTL+2*time.Hour+2*time.Second), `{}`)
	report, err := Backfill(context.Background(), db, "backfill")
	if err != nil || report.FramesAdded != 4 {
		t.Fatalf("retained-route sweeps: %+v, %v", report, err)
	}
	frames := backfillAcceptanceFrames(t, db)
	if len(frames) != 4 {
		t.Fatalf("four route sweeps produced %d frames", len(frames))
	}
	for i, expected := range []map[string]bool{
		{"expired": true, "recurring": true},
		{"expired": true, "recurring": true, "recent": true},
		{"recent": true},
		{"recurring": true, "recent": true},
	} {
		for _, to := range []string{"expired", "recurring", "recent"} {
			key := "routes/quay-port|" + to + "|hoy"
			_, exists := frames[i].Datasets[key]
			if exists != expected[to] {
				t.Fatalf("frame %d route %s present=%v, want %v", i, to, exists, expected[to])
			}
		}
	}
	if report.Paths["/v1/world/route"].Mapped != 4 {
		t.Fatalf("hourly sighting points counted requests more than once: %+v", report.Paths)
	}
}

func TestBackfillBoundsPassEventsAndDisclosesDrops(t *testing.T) {
	db, raw := backfillAcceptanceStore(t)
	ctx := context.Background()
	at := time.Date(2026, 10, 26, 12, 0, 0, 0, time.UTC)
	const countEvents = store.MaxReplayEventCount*4 + 5
	backfillAcceptanceRequest(t, db, raw, "backfill", "GET", "/v1/world/clock", 200, at, `{"total_game_hours":100}`)
	backfillAcceptanceRequest(t, db, raw, "backfill", "GET", "/v1/world/clock", 200, at.Add(3*time.Minute), `{"total_game_hours":101}`)
	backfillAcceptanceRequest(t, db, raw, "backfill", "GET", "/v1/company/profile", 200, at.Add(4*time.Minute), `{}`)
	if _, err := raw.ExecContext(ctx, `INSERT INTO events (account_id, event_id, event_type, payload, created_at)
		SELECT 'backfill', 'count-' || n, 'synthetic', '{"value":1}',
		$1::timestamptz + interval '30 seconds' + n * interval '1 millisecond'
		FROM generate_series(1, $2) AS n`, at, countEvents); err != nil {
		t.Fatal(err)
	}
	if _, err := raw.ExecContext(ctx, `INSERT INTO events (account_id, event_id, event_type, payload, created_at)
		SELECT 'backfill', 'bytes-' || n, 'synthetic', jsonb_build_object('padding', repeat('x', 31000)),
		$1::timestamptz + interval '30 seconds' + n * interval '1 millisecond'
		FROM generate_series(1, 40) AS n`, at.Add(3*time.Minute)); err != nil {
		t.Fatal(err)
	}
	if _, err := Backfill(ctx, db, "backfill"); err != nil {
		t.Fatal(err)
	}
	frames := backfillAcceptanceFrames(t, db)
	if len(frames) != 2 {
		t.Fatalf("two event passes produced %d frames", len(frames))
	}
	for i, total := range []int{countEvents, 40} {
		dataset := frames[i].Datasets["events"]
		var retained []store.ReplayEvent
		if err := json.Unmarshal(dataset.Data, &retained); err != nil {
			t.Fatal(err)
		}
		var meta struct {
			Complete      bool   `json:"complete"`
			Stale         bool   `json:"stale"`
			Error         string `json:"error"`
			DroppedEvents int64  `json:"dropped_events"`
			DroppedBytes  int64  `json:"dropped_bytes"`
		}
		if err := json.Unmarshal(dataset.Meta, &meta); err != nil {
			t.Fatal(err)
		}
		used := 0
		for _, event := range retained {
			used += len(event.Payload) + len(event.EventID) + len(event.Type) + 64
		}
		if len(retained) == 0 || len(retained) > store.MaxReplayEventCount || used > store.MaxReplayEventBytes ||
			meta.Complete || !meta.Stale || !strings.Contains(meta.Error, "overflow") ||
			meta.DroppedEvents != int64(total-len(retained)) || meta.DroppedBytes <= 0 {
			t.Fatalf("pass %d did not bound or disclose event evidence: retained=%d bytes=%d meta=%+v", i, len(retained), used, meta)
		}
		if i == 0 && len(retained) != store.MaxReplayEventCount {
			t.Fatalf("count cap retained %d events", len(retained))
		}
		if i == 1 && len(retained) >= total {
			t.Fatal("the byte cap did not drop any event")
		}
	}
}

func TestBackfillOversizePassDemotesDatasetsAndContinues(t *testing.T) {
	db, raw := backfillAcceptanceStore(t)
	ctx := context.Background()
	at := time.Date(2026, 10, 26, 12, 0, 0, 0, time.UTC)
	for i := range 3 {
		backfillAcceptanceRequest(t, db, raw, "backfill", "GET", "/v1/world/clock", 200, at.Add(time.Duration(i)*3*time.Minute), fmt.Sprintf(`{"total_game_hours":%d}`, 100+i))
	}
	if _, err := raw.ExecContext(ctx, `INSERT INTO requests (account_id, method, path, status, duration_ms, request, response, created_at)
		SELECT 'backfill', 'GET', '/v1/market/book?port_id=destination-' || n,
		200, 10, '{}', jsonb_build_object('port_id', 'destination-' || n, 'padding', repeat('x', 32000)),
		$1::timestamptz + interval '1 second' + n * interval '1 millisecond'
		FROM generate_series(1, 530) AS n`, at); err != nil {
		t.Fatal(err)
	}
	report, err := Backfill(ctx, db, "backfill")
	if err != nil || report.FramesAdded != 3 || report.OversizeDatasets == 0 {
		t.Fatalf("oversize sweep stopped tape import: frames=%d gaps=%d err=%v", report.FramesAdded, report.OversizeDatasets, err)
	}
	frames := backfillAcceptanceFrames(t, db)
	if len(frames) != 3 {
		t.Fatalf("oversize sweep prevented later frames: %d", len(frames))
	}
	var gaps int64
	for i, frame := range frames {
		backfillAcceptanceMeta(t, frame, "clock", true)
		if !backfillAcceptanceJSONEqual(t, frame.Datasets["clock"].Data, []byte(fmt.Sprintf(`{"total_game_hours":%d}`, 100+i))) {
			t.Fatalf("frame %d lost its small clock dataset", i)
		}
		for key, dataset := range frame.Datasets {
			if !strings.HasPrefix(key, "books/") || len(dataset.Data) != 0 {
				continue
			}
			var meta struct {
				Complete      bool   `json:"complete"`
				Stale         bool   `json:"stale"`
				Error         string `json:"error"`
				OriginalBytes int64  `json:"original_bytes"`
			}
			if err := json.Unmarshal(dataset.Meta, &meta); err != nil {
				t.Fatal(err)
			}
			if meta.Complete || !meta.Stale || meta.Error == "" || meta.OriginalBytes < 32000 {
				t.Fatalf("oversize %s was not a reasoned gap: %s", key, dataset.Meta)
			}
			gaps++
		}
	}
	if gaps != report.OversizeDatasets {
		t.Fatalf("report counted %d oversize gaps, stored %d", report.OversizeDatasets, gaps)
	}
	var printed bytes.Buffer
	if _, err := report.WriteTo(&printed); err != nil || !strings.Contains(printed.String(), fmt.Sprintf("Oversize dataset gaps: %d", gaps)) {
		t.Fatalf("oversize gap report missing: %s, %v", printed.String(), err)
	}
	again, err := Backfill(ctx, db, "backfill")
	if err != nil || again.FramesAdded != 0 || again.PayloadsAdded != 0 || again.OversizeDatasets != 0 {
		t.Fatalf("oversize sweep did not remain idempotent: %+v, %v", again, err)
	}
}

func TestBackfillStaggeredFirstWavesWithoutLaterWaveCompletion(t *testing.T) {
	db, raw := backfillAcceptanceStore(t)
	at := time.Date(2026, 10, 26, 12, 0, 0, 0, time.UTC)
	for i := range 2 {
		start := at.Add(time.Duration(i) * 3 * time.Minute)
		backfillAcceptanceRequest(t, db, raw, "backfill", "GET", "/v1/auth/whoami", 200, start, `{"company_id":"company-backfill"}`)
		backfillAcceptanceRequest(t, db, raw, "backfill", "GET", "/v1/company/profile", 200, start.Add(100*time.Millisecond), fmt.Sprintf(`{"pass":%d}`, i))
		backfillAcceptanceTimedRequest(t, db, raw, "backfill", "GET", "/v1/world/clock", 200, start.Add(2*time.Second), time.Second, fmt.Sprintf(`{"total_game_hours":%d}`, 100+i))
	}
	report, err := Backfill(context.Background(), db, "backfill")
	if err != nil || report.FramesAdded != 2 {
		t.Fatalf("staggered incomplete passes: %+v, %v", report, err)
	}
	frames := backfillAcceptanceFrames(t, db)
	if len(frames) != 2 {
		t.Fatalf("two staggered first waves produced %d frames", len(frames))
	}
	for i, frame := range frames {
		if !frame.ObservedAt.Equal(at.Add(time.Duration(i)*3*time.Minute)) ||
			!backfillAcceptanceJSONEqual(t, frame.Datasets["profile"].Data, []byte(fmt.Sprintf(`{"pass":%d}`, i))) {
			t.Fatalf("pass %d lost its non-overlapping current whoami or profile", i)
		}
	}
}

func TestBackfillDropsEventWhoseJSONBTextExceedsSQLByteBound(t *testing.T) {
	db, raw := backfillAcceptanceStore(t)
	ctx := context.Background()
	at := time.Date(2026, 10, 26, 12, 0, 0, 0, time.UTC)
	backfillAcceptanceRequest(t, db, raw, "backfill", "GET", "/v1/world/clock", 200, at, `{"total_game_hours":100}`)
	backfillAcceptanceRequest(t, db, raw, "backfill", "GET", "/v1/company/profile", 200, at.Add(2*time.Minute), `{}`)
	if _, err := raw.ExecContext(ctx, `INSERT INTO events (account_id, event_id, event_type, payload, created_at)
		VALUES ('backfill', 'text-overflow-event', 'synthetic', jsonb_build_object('padding', repeat('x', $1)), $2)`,
		2*store.MaxReplayEventBytes, at.Add(30*time.Second)); err != nil {
		t.Fatal(err)
	}
	if err := db.RecordEvent(ctx, "backfill", "healthy-event", "synthetic", json.RawMessage(`{"value":1}`)); err != nil {
		t.Fatal(err)
	}
	if _, err := raw.ExecContext(ctx, "UPDATE events SET created_at = $1 WHERE account_id = $2 AND event_id = $3",
		at.Add(31*time.Second), "backfill", "healthy-event"); err != nil {
		t.Fatal(err)
	}
	var storedBytes, normalizedBytes int64
	if err := raw.QueryRowContext(ctx, `SELECT pg_column_size(payload), octet_length(payload::text)
		FROM events WHERE account_id = $1 AND event_id = $2`,
		"backfill", "text-overflow-event").Scan(&storedBytes, &normalizedBytes); err != nil {
		t.Fatal(err)
	}
	if storedBytes > store.MaxReplayEventBytes || normalizedBytes <= store.MaxReplayEventBytes {
		t.Fatalf("text overflow fixture did not bypass the physical-size guard: stored=%d normalized=%d", storedBytes, normalizedBytes)
	}
	wantDroppedBytes := normalizedBytes + int64(len("text-overflow-event")+len("synthetic")+64)
	report, err := Backfill(ctx, db, "backfill")
	if err != nil || report.FramesAdded != 1 {
		t.Fatalf("SQL-bounded event import: %+v, %v", report, err)
	}
	frames := backfillAcceptanceFrames(t, db)
	if len(frames) != 1 {
		t.Fatalf("event pass produced %d frames", len(frames))
	}
	var retained []store.ReplayEvent
	var meta struct {
		Complete      bool   `json:"complete"`
		Stale         bool   `json:"stale"`
		DroppedEvents int64  `json:"dropped_events"`
		DroppedBytes  int64  `json:"dropped_bytes"`
		Error         string `json:"error"`
	}
	dataset := frames[0].Datasets["events"]
	if err := json.Unmarshal(dataset.Data, &retained); err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(dataset.Meta, &meta); err != nil {
		t.Fatal(err)
	}
	if len(retained) != 1 || retained[0].EventID != "healthy-event" || meta.Complete || !meta.Stale ||
		meta.DroppedEvents != 1 || meta.DroppedBytes != wantDroppedBytes || meta.Error == "" {
		t.Fatalf("oversize SQL gap was materialized or not counted: retained=%+v meta=%+v", retained, meta)
	}
}

func TestBackfillSweepRoutesUseLiveNewestFirstRetentionBudgets(t *testing.T) {
	for _, test := range []struct {
		name     string
		padding  int
		sameTime bool
	}{
		{name: "count"},
		{name: "bytes", padding: 16000},
		{name: "ties", sameTime: true},
	} {
		t.Run(test.name, func(t *testing.T) {
			db, raw := backfillAcceptanceStore(t)
			ctx := context.Background()
			at := time.Date(2026, 10, 26, 12, 0, 0, 0, time.UTC)
			const count = engine.SourcingRouteRetentionLimit + 20
			backfillAcceptanceRequest(t, db, raw, "backfill", "GET", "/v1/world/clock", 200, at, `{"total_game_hours":100}`)
			backfillAcceptanceRequest(t, db, raw, "backfill", "GET", "/v1/fleet/ships?limit=200", 200, at.Add(100*time.Millisecond), `[]`)
			backfillAcceptanceRequest(t, db, raw, "backfill", "GET", "/v1/market/book?port_id=quay-port", 200, at.Add(200*time.Millisecond), `{"port_id":"quay-port"}`)
			backfillAcceptanceRequest(t, db, raw, "backfill", "GET", "/v1/world/clock", 200, at.Add(5*time.Minute), `{"total_game_hours":101}`)
			if _, err := raw.ExecContext(ctx, `INSERT INTO requests (account_id, method, path, status, duration_ms, request, response, created_at)
				SELECT 'backfill', 'GET', '/v1/world/route?from=quay-port&to=destination-' || to_char(n, 'FM0000') || '&ship_class=hoy',
				200, 10, '{}', jsonb_build_object('from_port_id', 'quay-port', 'to_port_id', 'destination-' || to_char(n, 'FM0000'), 'padding', repeat('x', $4)),
				$1::timestamptz + interval '1 second' + CASE WHEN $3 THEN interval '0' ELSE n * interval '1 millisecond' END
				FROM generate_series(1, $2) AS n`, at, count, test.sameTime, test.padding); err != nil {
				t.Fatal(err)
			}
			report, err := Backfill(ctx, db, "backfill")
			if err != nil || report.FramesAdded != 2 || report.OversizeDatasets != 0 {
				t.Fatalf("bounded sweep import: %+v, %v", report, err)
			}
			frames := backfillAcceptanceFrames(t, db)
			if len(frames) != 2 {
				t.Fatalf("two sweeps produced %d frames", len(frames))
			}
			backfillAcceptanceMeta(t, frames[0], "ships", true)
			backfillAcceptanceMeta(t, frames[0], "books/quay-port", true)
			for i, frame := range frames {
				backfillAcceptanceMeta(t, frame, "clock", true)
				if _, lost := frame.Datasets["_replay_gap"]; lost {
					t.Fatal("optional routes discarded the core frame")
				}
				retained, used := 0, 0
				for key, dataset := range frame.Datasets {
					if !strings.HasPrefix(key, "routes/") {
						continue
					}
					shortKey, _ := json.Marshal(strings.TrimPrefix(key, "routes/"))
					fullKey, _ := json.Marshal(key)
					body, err := json.Marshal(json.RawMessage(dataset.Data))
					if err != nil {
						t.Fatal(err)
					}
					used += len(body) + len(dataset.Meta) + len(shortKey) + len(fullKey) + 4
					retained++
				}
				if retained == 0 || retained > engine.SourcingRouteRetentionLimit || used > engine.SourcingRouteRetentionBytes {
					t.Fatalf("sweep %d escaped live retention caps: count=%d bytes=%d", i, retained, used)
				}
				if test.padding == 0 && retained != engine.SourcingRouteRetentionLimit ||
					test.padding != 0 && retained >= engine.SourcingRouteRetentionLimit {
					t.Fatalf("sweep %d did not exercise its count/byte cap: count=%d bytes=%d", i, retained, used)
				}
				for n := 1; n <= count; n++ {
					key := fmt.Sprintf("routes/quay-port|destination-%04d|hoy", n)
					_, exists := frame.Datasets[key]
					want := n > count-retained
					if test.sameTime {
						want = n <= retained
					}
					if exists != want {
						t.Fatalf("sweep %d violated newest-first/key-tie retention for %s: present=%v want=%v", i, key, exists, want)
					}
				}
			}
		})
	}
}
