package sim

// Assumed API: Backfill(context.Context, *store.Store, accountID string) (BackfillReport, error).
// BackfillReport has FramesAdded/PayloadsAdded int64 and WriteTo(io.Writer) (int64, error).
// The store replay API/types are documented in internal/store/replay_acceptance_test.go.
// Backfill sets ReplayFrame.Source="diagnostic_backfill" and AssumedCatalog=true;
// ReplayDataset.Meta is JSON with engine.DatasetMeta's field names and time semantics.
// Diagnostic bytes mean the complete bytes retained by RecordRequest (JSONB's
// response::text), never the original pre-redaction response or a truncated preview.
// Fixture timestamps are request starts; created_at stores start+duration_ms.
// Backfill generations are diagnostic:<clock-boundary request id>.

import (
	"bytes"
	"context"
	"crypto/rand"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net/url"
	"os"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/DevYukine/go-tradewinds/internal/store"
	"go.uber.org/zap"
)

func backfillAcceptanceStore(t *testing.T) (*store.Store, *sql.DB) {
	t.Helper()
	dsn := os.Getenv("TEST_DATABASE_URL")
	if dsn == "" {
		t.Skip("set TEST_DATABASE_URL to run isolated real PostgreSQL integration tests")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	admin, err := sql.Open("pgx", dsn)
	if err != nil {
		t.Fatal(err)
	}
	var random [8]byte
	if _, err := rand.Read(random[:]); err != nil {
		_ = admin.Close()
		t.Fatal(err)
	}
	schema := "tradewinds_backfill_" + hex.EncodeToString(random[:])
	if _, err := admin.ExecContext(ctx, "CREATE SCHEMA "+schema); err != nil {
		_ = admin.Close()
		t.Fatal(err)
	}
	var db *store.Store
	var raw *sql.DB
	t.Cleanup(func() {
		if db != nil {
			_ = db.Close()
		}
		if raw != nil {
			_ = raw.Close()
		}
		cleanupCtx, cleanupCancel := context.WithTimeout(context.Background(), 15*time.Second)
		defer cleanupCancel()
		if _, err := admin.ExecContext(cleanupCtx, "DROP SCHEMA "+schema+" CASCADE"); err != nil {
			t.Errorf("drop isolated backfill schema: %v", err)
		}
		_ = admin.Close()
	})
	parsed, err := url.Parse(dsn)
	if err == nil && (parsed.Scheme == "postgres" || parsed.Scheme == "postgresql") {
		query := parsed.Query()
		query.Set("search_path", schema)
		query.Set("synchronous_commit", "off")
		parsed.RawQuery = query.Encode()
		dsn = parsed.String()
	} else {
		dsn += " search_path=" + schema + " synchronous_commit=off"
	}
	db, err = store.Open(ctx, dsn, bytes.Repeat([]byte{0x42}, 32), zap.NewNop())
	if err != nil {
		t.Fatal(err)
	}
	raw, err = sql.Open("pgx", dsn)
	if err != nil {
		t.Fatal(err)
	}
	for _, account := range []string{"backfill", "other"} {
		if err := db.SaveAccount(ctx, store.Account{ID: account, CompanyID: "company-" + account, Label: account, Enabled: true}, store.Credentials{Handle: account, APIKey: "synthetic-" + account}); err != nil {
			t.Fatal(err)
		}
	}
	if err := db.SaveSnapshot(ctx, "backfill", map[string]json.RawMessage{"catalog": json.RawMessage(`{"ports":[{"id":"quay-port"}],"version":"assumed-current"}`)}); err != nil {
		t.Fatal(err)
	}
	return db, raw
}

func backfillAcceptanceRequest(t *testing.T, db *store.Store, raw *sql.DB, account, method, path string, status int, at time.Time, response string) []byte {
	t.Helper()
	return backfillAcceptanceTimedRequest(t, db, raw, account, method, path, status, at, 10*time.Millisecond, response)
}

func backfillAcceptanceTimedRequest(t *testing.T, db *store.Store, raw *sql.DB, account, method, path string, status int, start time.Time, duration time.Duration, response string) []byte {
	t.Helper()
	if err := db.RecordRequest(context.Background(), account, method, path, status, duration, nil, []byte(response), ""); err != nil {
		t.Fatal(err)
	}
	var retained []byte
	if err := raw.QueryRowContext(context.Background(), "UPDATE requests SET created_at = $1 WHERE id = (SELECT max(id) FROM requests WHERE account_id = $2) RETURNING response::text", start.Add(duration), account).Scan(&retained); err != nil {
		t.Fatal(err)
	}
	return retained
}

func backfillAcceptanceFrames(t *testing.T, db *store.Store) []store.ReplayFrame {
	t.Helper()
	var frames []store.ReplayFrame
	if err := db.ScanReplayFrames(context.Background(), "backfill", func(frame store.ReplayFrame) error {
		if frame.Source == "diagnostic_backfill" {
			frames = append(frames, frame)
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	return frames
}

func backfillAcceptanceMeta(t *testing.T, frame store.ReplayFrame, key string, complete bool) {
	t.Helper()
	dataset, ok := frame.Datasets[key]
	var meta struct {
		Generation  string    `json:"generation"`
		AttemptedAt time.Time `json:"attempted_at"`
		UpdatedAt   time.Time `json:"updated_at"`
		Complete    bool      `json:"complete"`
		Error       string    `json:"error"`
		Stale       bool      `json:"stale"`
	}
	if err := json.Unmarshal(dataset.Meta, &meta); err != nil || !ok || meta.Generation != frame.Generation || meta.AttemptedAt.IsZero() || meta.Complete != complete {
		t.Fatalf("%s has incompatible DatasetMeta: %+v, %v", key, dataset, err)
	}
	if complete && (len(dataset.Data) == 0 || meta.UpdatedAt.IsZero() || meta.Error != "" || meta.Stale) {
		t.Fatalf("%s is not a complete observation: %+v", key, dataset)
	}
	if !complete && (len(dataset.Data) != 0 || meta.Error == "") {
		t.Fatalf("%s fabricated an empty payload instead of a reasoned gap: %+v", key, dataset)
	}
}

func backfillAcceptanceJSONEqual(t *testing.T, got, want []byte) bool {
	t.Helper()
	var actual, expected any
	if err := json.Unmarshal(got, &actual); err != nil {
		t.Fatalf("invalid imported JSON %q: %v", got, err)
	}
	if err := json.Unmarshal(want, &expected); err != nil {
		t.Fatal(err)
	}
	return reflect.DeepEqual(actual, expected)
}

func TestBackfillAcceptanceTwoPassesMappingGapsBoundaryAndRerun(t *testing.T) {
	db, raw := backfillAcceptanceStore(t)
	ctx := context.Background()
	at := time.Date(2026, 10, 26, 12, 0, 0, 0, time.UTC)
	expected := make([]map[string][]byte, 2)
	for i, offset := range []time.Duration{0, 3 * time.Minute} {
		start := at.Add(offset)
		expected[i] = make(map[string][]byte)
		expected[i]["clock"] = backfillAcceptanceRequest(t, db, raw, "backfill", "GET", "/v1/world/clock", 200, start, fmt.Sprintf(`{"total_game_hours":%d,"unlocked_phase":1,"paused":false}`, 100+i))
		for j, row := range []struct{ key, path, body string }{
			{"ships", "/v1/fleet/ships?limit=200", `[{"ship":{"id":"ship-a","current_port_id":"quay-port","status":"berthed"}}]`},
			{"books/quay-port", "/v1/market/book?port_id=quay-port", `{"port_id":"quay-port","books":[{"commodity_id":"grain","npc_stock":17}]}`},
			{"ports/quay-port", "/v1/world/port?port_id=quay-port", `{"id":"quay-port","visibility":"quay"}`},
			{"routes/quay-port|remote-port|hoy", "/v1/world/route?from=quay-port&ship_class=hoy&to=remote-port", `{"from_port_id":"quay-port","to_port_id":"remote-port","ship_class":"hoy","legs":[]}`},
		} {
			expected[i][row.key] = backfillAcceptanceRequest(t, db, raw, "backfill", "GET", row.path, 200, start.Add(time.Duration(j+1)*time.Second), row.body)
		}
		backfillAcceptanceRequest(t, db, raw, "backfill", "GET", "/v1/market/book?port_id=london", 200, start.Add(6*time.Second), `{"truncated":true,"original_bytes":100000,"preview":"not a complete book"}`)
		backfillAcceptanceRequest(t, db, raw, "backfill", "GET", "/v1/market/book?port_id=absent-port", 403, start.Add(7*time.Second), `{"code":"no_port_presence","message":"requires quay presence"}`)
	}
	if err := db.RecordEvent(ctx, "backfill", "backfill-event-one", "ship_sailed", json.RawMessage(`{"ship_id":"foreign-ship"}`)); err != nil {
		t.Fatal(err)
	}
	if _, err := raw.ExecContext(ctx, "UPDATE events SET created_at = $1 WHERE account_id = $2 AND event_id = $3", at.Add(30*time.Second), "backfill", "backfill-event-one"); err != nil {
		t.Fatal(err)
	}
	if err := db.RecordEvent(ctx, "other", "other-event", "market_tick", json.RawMessage(`{"other":true}`)); err != nil {
		t.Fatal(err)
	}
	backfillAcceptanceRequest(t, db, raw, "backfill", "POST", "/v1/market/place-order", 200, at.Add(3*time.Minute+8*time.Second), `{"filled":1}`)
	backfillAcceptanceRequest(t, db, raw, "backfill", "PATCH", "/v1/company/settings", 200, at.Add(3*time.Minute+9*time.Second), `{"ok":true}`)
	for i, path := range []string{"/v1/treasury/ledger?limit=200", "/v1/world/events?limit=200", "/v1/insurance/list?limit=200", "/v1/world/catalog"} {
		backfillAcceptanceRequest(t, db, raw, "backfill", "GET", path, 200, at.Add(time.Duration(10+i)*time.Second), `[{"must_not_import":true}]`)
	}
	backfillAcceptanceRequest(t, db, raw, "backfill", "GET", "/v1/company/profile", 200, at.Add(15*time.Second), `{"truncated":true,"preview":"partial profile"}`)
	backfillAcceptanceRequest(t, db, raw, "other", "GET", "/v1/world/clock", 200, at.Add(time.Second), `{"total_game_hours":999}`)
	// Nothing at or after the first live frame may be backdated into diagnostics.
	liveAt := at.Add(4 * time.Minute)
	liveID, err := db.RecordReplayFrame(ctx, store.ReplayFrame{AccountID: "backfill", Generation: "live", Source: "live", ObservedAt: liveAt, Datasets: map[string]store.ReplayDataset{}})
	if err != nil {
		t.Fatal(err)
	}
	backfillAcceptanceRequest(t, db, raw, "backfill", "GET", "/v1/world/clock", 200, liveAt, `{"total_game_hours":888}`)
	backfillAcceptanceRequest(t, db, raw, "backfill", "GET", "/v1/market/book?port_id=future-port", 200, liveAt.Add(time.Second), `{"future":true}`)
	report, err := Backfill(ctx, db, "backfill")
	if err != nil || report.FramesAdded != 2 {
		t.Fatalf("two historical passes: report=%+v, err=%v", report, err)
	}
	frames := backfillAcceptanceFrames(t, db)
	if len(frames) != 2 || frames[0].ID == liveID || !frames[0].ObservedAt.Equal(at) || !frames[1].ObservedAt.Equal(at.Add(3*time.Minute)) || !frames[1].Decision {
		t.Fatalf("clock grouping or decision preservation failed: %+v", frames)
	}
	var boundaries []int64
	rows, err := raw.QueryContext(ctx, "SELECT id FROM requests WHERE account_id = $1 AND path = '/v1/world/clock' AND created_at - duration_ms * interval '1 millisecond' < $2 ORDER BY created_at - duration_ms * interval '1 millisecond', id", "backfill", liveAt)
	if err != nil {
		t.Fatal(err)
	}
	for rows.Next() {
		var id int64
		if err := rows.Scan(&id); err != nil {
			_ = rows.Close()
			t.Fatal(err)
		}
		boundaries = append(boundaries, id)
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	_ = rows.Close()
	if len(boundaries) != 2 {
		t.Fatalf("fixture has %d clock boundaries, want 2", len(boundaries))
	}
	snapshot, err := db.Snapshot(ctx, "backfill")
	if err != nil {
		t.Fatal(err)
	}
	for i, frame := range frames {
		if frame.Generation != fmt.Sprintf("diagnostic:%d", boundaries[i]) {
			t.Fatalf("backfill generation is not deterministic: %q", frame.Generation)
		}
		if !frame.AssumedCatalog || !bytes.Equal(frame.Datasets["catalog"].Data, snapshot["catalog"]) {
			t.Fatalf("historical catalog was not explicitly assumed: %+v", frame)
		}
		for key, want := range expected[i] {
			if strings.HasPrefix(key, "routes/") && i != 0 {
				continue // Unchanged routes may be retained by reference or omitted by decimation.
			}
			if !backfillAcceptanceJSONEqual(t, frame.Datasets[key].Data, want) {
				t.Fatalf("pass %d %s bytes=%q, want retained bytes=%q", i, key, frame.Datasets[key].Data, want)
			}
			backfillAcceptanceMeta(t, frame, key, true)
		}
		backfillAcceptanceMeta(t, frame, "books/london", false)
		backfillAcceptanceMeta(t, frame, "books/absent-port", false)
		for _, key := range []string{"ledger", "world_events", "insurance", "profile", "books/future-port"} {
			if _, exists := frame.Datasets[key]; exists {
				t.Fatalf("backfill imported excluded or post-live %s", key)
			}
		}
	}
	var events []struct {
		EventID   string          `json:"event_id"`
		Type      string          `json:"type"`
		Payload   json.RawMessage `json:"payload"`
		CreatedAt time.Time       `json:"created_at"`
	}
	if err := json.Unmarshal(frames[0].Datasets["events"].Data, &events); err != nil || len(events) != 1 || events[0].EventID != "backfill-event-one" || events[0].Type != "ship_sailed" || !events[0].CreatedAt.Equal(at.Add(30*time.Second)) {
		t.Fatalf("pass-window SSE event envelope missing or leaked accounts: %+v, %v", events, err)
	}
	if len(frames[1].Datasets["events"].Data) != 0 {
		var later []json.RawMessage
		if err := json.Unmarshal(frames[1].Datasets["events"].Data, &later); err != nil || len(later) != 0 {
			t.Fatalf("event was attached to the wrong pass: %q, %v", frames[1].Datasets["events"].Data, err)
		}
	}
	var printed bytes.Buffer
	if _, err := report.WriteTo(&printed); err != nil {
		t.Fatal(err)
	}
	text := strings.ToLower(printed.String())
	for _, term := range []string{"quay-port", "hours", "london", "truncated", "gap", "2026-10-26"} {
		if !strings.Contains(text, term) {
			t.Fatalf("coverage report omitted %q: %s", term, text)
		}
	}
	var beforeFrames, beforePayloads int64
	if err := raw.QueryRowContext(ctx, "SELECT (SELECT count(*) FROM replay_frames), (SELECT count(*) FROM replay_payloads)").Scan(&beforeFrames, &beforePayloads); err != nil {
		t.Fatal(err)
	}
	again, err := Backfill(ctx, db, "backfill")
	if err != nil || again.FramesAdded != 0 || again.PayloadsAdded != 0 {
		t.Fatalf("rerun was not idempotent: %+v, %v", again, err)
	}
	var afterFrames, afterPayloads int64
	if err := raw.QueryRowContext(ctx, "SELECT (SELECT count(*) FROM replay_frames), (SELECT count(*) FROM replay_payloads)").Scan(&afterFrames, &afterPayloads); err != nil || afterFrames != beforeFrames || afterPayloads != beforePayloads {
		t.Fatalf("rerun added physical rows: frames %d->%d payloads %d->%d, %v", beforeFrames, afterFrames, beforePayloads, afterPayloads, err)
	}
}

func TestBackfillAcceptanceContractsMergeInReadContractsOrder(t *testing.T) {
	db, raw := backfillAcceptanceStore(t)
	at := time.Date(2026, 10, 26, 12, 0, 0, 0, time.UTC)
	backfillAcceptanceRequest(t, db, raw, "backfill", "GET", "/v1/world/clock", 200, at, `{"total_game_hours":100}`)
	// Arrival order is intentionally different from readContracts' status order.
	for i, row := range []struct{ status, response string }{
		{"defaulted", `[{"id":"shared","status":"defaulted"},{"id":"default-only","status":"defaulted"}]`},
		{"active", `[{"id":"shared","status":"active"},{"id":"active-only","status":"active"}]`},
		{"open", `[{"id":"open-only","status":"open"},{"id":"shared","status":"open"}]`},
		{"completed", `{"truncated":true,"preview":"not usable"}`},
		{"cancelled", `{"truncated":true,"preview":"not usable"}`},
	} {
		backfillAcceptanceRequest(t, db, raw, "backfill", "GET", "/v1/contract/list?limit=200&status="+row.status, 200, at.Add(time.Duration(i+1)*time.Second), row.response)
	}
	if _, err := Backfill(context.Background(), db, "backfill"); err != nil {
		t.Fatal(err)
	}
	frames := backfillAcceptanceFrames(t, db)
	if len(frames) != 1 {
		t.Fatalf("one clock produced %d frames", len(frames))
	}
	var contracts []struct{ ID, Status string }
	if err := json.Unmarshal(frames[0].Datasets["contracts"].Data, &contracts); err != nil {
		t.Fatal(err)
	}
	var ids, statuses []string
	for _, contract := range contracts {
		ids = append(ids, contract.ID)
		statuses = append(statuses, contract.Status)
	}
	if !reflect.DeepEqual(ids, []string{"open-only", "shared", "active-only", "default-only"}) || !reflect.DeepEqual(statuses, []string{"open", "defaulted", "active", "defaulted"}) {
		t.Fatalf("contract order/dedup precedence differs from readContracts: ids=%v statuses=%v", ids, statuses)
	}
	backfillAcceptanceMeta(t, frames[0], "contracts", true)
}

func TestBackfillAcceptanceRouteVersionsAndNonPresencePortBins(t *testing.T) {
	db, raw := backfillAcceptanceStore(t)
	at := time.Date(2026, 10, 26, 12, 0, 0, 0, time.UTC)
	for i, offset := range []time.Duration{0, time.Minute, 5 * time.Minute} {
		start := at.Add(offset)
		backfillAcceptanceRequest(t, db, raw, "backfill", "GET", "/v1/world/clock", 200, start, fmt.Sprintf(`{"total_game_hours":%d}`, 100+i))
		backfillAcceptanceRequest(t, db, raw, "backfill", "GET", "/v1/world/port?port_id=remote-port", 200, start.Add(time.Second), fmt.Sprintf(`{"id":"remote-port","visibility":"public","version":%d}`, i))
		backfillAcceptanceRequest(t, db, raw, "backfill", "GET", "/v1/world/port?port_id=quay-port", 200, start.Add(2*time.Second), fmt.Sprintf(`{"id":"quay-port","visibility":"quay","version":%d}`, i))
		version := 0
		if i == 2 {
			version = 1
		}
		for repeat := range 3 {
			backfillAcceptanceRequest(t, db, raw, "backfill", "GET", "/v1/world/route?from=quay-port&ship_class=hoy&to=remote-port", 200, start.Add(time.Duration(3+repeat)*time.Second), fmt.Sprintf(`{"from_port_id":"quay-port","to_port_id":"remote-port","ship_class":"hoy","version":%d}`, version))
		}
	}
	if _, err := Backfill(context.Background(), db, "backfill"); err != nil {
		t.Fatal(err)
	}
	versions := make(map[string]map[int]bool)
	for _, frame := range backfillAcceptanceFrames(t, db) {
		for _, key := range []string{"ports/remote-port", "ports/quay-port", "routes/quay-port|remote-port|hoy"} {
			data := frame.Datasets[key].Data
			if len(data) == 0 {
				continue
			}
			var value struct {
				Version int `json:"version"`
			}
			if err := json.Unmarshal(data, &value); err != nil {
				t.Fatal(err)
			}
			if versions[key] == nil {
				versions[key] = make(map[int]bool)
			}
			versions[key][value.Version] = true
		}
	}
	if !reflect.DeepEqual(versions["routes/quay-port|remote-port|hoy"], map[int]bool{0: true, 1: true}) {
		t.Fatalf("route versions were lost: %v", versions)
	}
	remote := versions["ports/remote-port"]
	if len(remote) != 2 || !remote[2] || remote[0] == remote[1] {
		t.Fatalf("remote port must have exactly one version per 5-minute bin: %v", remote)
	}
	if !reflect.DeepEqual(versions["ports/quay-port"], map[int]bool{0: true, 1: true, 2: true}) {
		t.Fatalf("quay port was incorrectly decimated: %v", versions)
	}
	// Two route versions + three clocks + two public-port versions + three
	// quay-port versions + one assumed catalog, plus an optional empty events payload.
	var payloads int64
	if err := raw.QueryRowContext(context.Background(), "SELECT count(*) FROM replay_payloads WHERE account_id = $1", "backfill").Scan(&payloads); err != nil {
		t.Fatal(err)
	}
	if payloads != 11 && payloads != 12 {
		t.Fatalf("route dedupe or port decimation stored %d payloads, want 11 (or 12 with empty events)", payloads)
	}
	again, err := Backfill(context.Background(), db, "backfill")
	if err != nil || again.PayloadsAdded != 0 {
		t.Fatalf("route rerun duplicated payloads: %+v, %v", again, err)
	}
}

func TestBackfillAcceptanceRequestStartAndFailedClockBoundaries(t *testing.T) {
	db, raw := backfillAcceptanceStore(t)
	at := time.Date(2026, 10, 26, 12, 0, 0, 0, time.UTC)
	// First-wave requests start before the clock, which finishes last. Later
	// profiles/vaults must not be folded into the previous mutation's pass.
	for i, offset := range []time.Duration{0, 3 * time.Minute} {
		start := at.Add(offset)
		backfillAcceptanceTimedRequest(t, db, raw, "backfill", "GET", "/v1/company/profile", 200, start, 10*time.Millisecond, fmt.Sprintf(`{"pass":%d}`, i))
		backfillAcceptanceTimedRequest(t, db, raw, "backfill", "GET", "/v1/world/clock", 200, start.Add(10*time.Millisecond), 5*time.Second, fmt.Sprintf(`{"total_game_hours":%d}`, 100+i))
		backfillAcceptanceTimedRequest(t, db, raw, "backfill", "GET", "/v1/treasury/vaults", 200, start.Add(20*time.Millisecond), 10*time.Millisecond, fmt.Sprintf(`[{"pass":%d}]`, i))
		backfillAcceptanceRequest(t, db, raw, "backfill", "GET", "/v1/market/book?port_id=quay-port", 200, start.Add(time.Second), fmt.Sprintf(`{"port_id":"quay-port","pass":%d}`, i))
		backfillAcceptanceRequest(t, db, raw, "backfill", "GET", "/v1/market/book?port_id=quay-port", 403, start.Add(2*time.Second), `{"code":"no_port_presence"}`)
	}
	backfillAcceptanceRequest(t, db, raw, "backfill", "POST", "/v1/market/place-order", 200, at.Add(10*time.Second), `{"filled":1}`)
	failedAt := at.Add(6 * time.Minute)
	backfillAcceptanceTimedRequest(t, db, raw, "backfill", "GET", "/v1/world/clock", 503, failedAt, time.Second, `{"code":"temporarily_unavailable"}`)
	backfillAcceptanceRequest(t, db, raw, "backfill", "GET", "/v1/company/profile", 200, failedAt.Add(2*time.Second), `{"pass":2}`)
	backfillAcceptanceRequest(t, db, raw, "backfill", "GET", "/v1/market/book?port_id=absent-port", 403, failedAt.Add(3*time.Second), `{"code":"no_port_presence"}`)
	if _, err := Backfill(context.Background(), db, "backfill"); err != nil {
		t.Fatal(err)
	}
	frames := backfillAcceptanceFrames(t, db)
	if len(frames) != 3 {
		t.Fatalf("failed clock merged adjacent pass windows: %d frames", len(frames))
	}
	for i, frame := range frames {
		if !frame.ObservedAt.Equal(at.Add(time.Duration(i) * 3 * time.Minute)) {
			t.Fatalf("pass %d starts at completion time instead of first-wave start: %v", i, frame.ObservedAt)
		}
		if !backfillAcceptanceJSONEqual(t, frame.Datasets["profile"].Data, []byte(fmt.Sprintf(`{"pass":%d}`, i))) {
			t.Fatalf("pass %d contains another pass's profile", i)
		}
		if i < 2 {
			if !backfillAcceptanceJSONEqual(t, frame.Datasets["vaults"].Data, []byte(fmt.Sprintf(`[{"pass":%d}]`, i))) {
				t.Fatalf("pass %d contains another pass's vaults", i)
			}
			backfillAcceptanceMeta(t, frame, "books/quay-port", true)
		}
	}
	if !frames[0].Decision || frames[1].Decision {
		t.Fatal("mutation marker crossed a request-start pass boundary")
	}
	backfillAcceptanceMeta(t, frames[2], "clock", false)
	backfillAcceptanceMeta(t, frames[2], "books/absent-port", false)
}

func TestBackfillAcceptanceProductionRoutesAndCursorPages(t *testing.T) {
	db, raw := backfillAcceptanceStore(t)
	at := time.Date(2026, 10, 26, 12, 0, 0, 0, time.UTC)
	backfillAcceptanceRequest(t, db, raw, "backfill", "GET", "/v1/world/clock", 200, at, `{"total_game_hours":100}`)
	routes := map[string]string{
		"/v1/world/route?from=quay-port&ship_class=hoy&to=remote-port":                      "routes/quay-port|remote-port|hoy",
		"/v1/world/route?from=remote-port&to=quay-port":                                     "routes/remote-port|quay-port|",
		"/v1/world/route?from_port_id=legacy-port&ship_class=cutter&to_port_id=remote-port": "routes/legacy-port|remote-port|cutter",
	}
	for path, key := range routes {
		parts := strings.Split(strings.TrimPrefix(key, "routes/"), "|")
		body := fmt.Sprintf(`{"from_port_id":%q,"to_port_id":%q,"ship_class":%q,"legs":[{"distance_nm":24}]}`, parts[0], parts[1], parts[2])
		backfillAcceptanceRequest(t, db, raw, "backfill", "GET", path, 200, at.Add(time.Second), body)
	}
	firstPage := make([]map[string]any, 200)
	for i := range firstPage {
		firstPage[i] = map[string]any{"id": fmt.Sprintf("ship-%03d", i), "name": "synthetic"}
	}
	page, err := json.Marshal(firstPage)
	if err != nil {
		t.Fatal(err)
	}
	backfillAcceptanceRequest(t, db, raw, "backfill", "GET", "/v1/fleet/ships?limit=200", 200, at.Add(2*time.Second), string(page))
	backfillAcceptanceRequest(t, db, raw, "backfill", "GET", "/v1/fleet/ships?cursor=ship-199&limit=200", 200, at.Add(3*time.Second), `[{"id":"ship-200","name":"last-page"}]`)
	report, err := Backfill(context.Background(), db, "backfill")
	if err != nil {
		t.Fatal(err)
	}
	frames := backfillAcceptanceFrames(t, db)
	if len(frames) != 1 {
		t.Fatalf("one clock produced %d frames", len(frames))
	}
	for _, key := range routes {
		backfillAcceptanceMeta(t, frames[0], key, true)
	}
	var ships []struct {
		ID string `json:"id"`
	}
	if err := json.Unmarshal(frames[0].Datasets["ships"].Data, &ships); err != nil || len(ships) != 201 || ships[0].ID != "ship-000" || ships[200].ID != "ship-200" {
		t.Fatalf("cursor pages were overwritten or concatenated incorrectly: count=%d, err=%v", len(ships), err)
	}
	var printed bytes.Buffer
	if _, err := report.WriteTo(&printed); err != nil {
		t.Fatal(err)
	}
	for _, term := range []string{"mapped", "unmapped", "route", "ships"} {
		if !strings.Contains(strings.ToLower(printed.String()), term) {
			t.Fatalf("path-family coverage report omitted %s: %s", term, printed.String())
		}
	}
}

func TestBackfillAcceptanceCompleteContractHistoryStatuses(t *testing.T) {
	db, raw := backfillAcceptanceStore(t)
	at := time.Date(2026, 10, 26, 12, 0, 0, 0, time.UTC)
	backfillAcceptanceRequest(t, db, raw, "backfill", "GET", "/v1/world/clock", 200, at, `{"total_game_hours":100}`)
	for i, status := range []string{"open", "active", "completed", "cancelled", "defaulted"} {
		body := fmt.Sprintf(`[{"id":"shared","status":%q},{"id":%q,"status":%q}]`, status, status+"-only", status)
		backfillAcceptanceRequest(t, db, raw, "backfill", "GET", "/v1/contract/list?limit=200&status="+status, 200, at.Add(time.Duration(i+1)*time.Second), body)
	}
	if _, err := Backfill(context.Background(), db, "backfill"); err != nil {
		t.Fatal(err)
	}
	frames := backfillAcceptanceFrames(t, db)
	if len(frames) != 1 {
		t.Fatalf("one clock produced %d frames", len(frames))
	}
	var contracts []struct{ ID, Status string }
	if err := json.Unmarshal(frames[0].Datasets["contracts"].Data, &contracts); err != nil {
		t.Fatal(err)
	}
	var ids []string
	for _, contract := range contracts {
		ids = append(ids, contract.ID)
	}
	want := []string{"shared", "open-only", "active-only", "completed-only", "cancelled-only", "defaulted-only"}
	if !reflect.DeepEqual(ids, want) || contracts[0].Status != "defaulted" {
		t.Fatalf("complete completed/cancelled pages were excluded or merged out of order: %+v", contracts)
	}
}
