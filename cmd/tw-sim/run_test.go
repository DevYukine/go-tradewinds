package main

import (
	"bytes"
	"context"
	"crypto/rand"
	"database/sql"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/DevYukine/go-tradewinds/internal/engine"
	"github.com/DevYukine/go-tradewinds/internal/sim"
	"github.com/DevYukine/go-tradewinds/internal/store"
	"go.uber.org/zap"
)

func commandTestStore(t *testing.T) (*store.Store, string, []byte) {
	t.Helper()
	dsn := os.Getenv("TEST_DATABASE_URL")
	if dsn == "" {
		t.Skip("set TEST_DATABASE_URL for isolated simulator command integration")
	}
	admin, err := sql.Open("pgx", dsn)
	if err != nil {
		t.Fatal(err)
	}
	var random [8]byte
	if _, err := rand.Read(random[:]); err != nil {
		t.Fatal(err)
	}
	schema := "tradewinds_sim_command_" + hex.EncodeToString(random[:])
	if _, err := admin.Exec("CREATE SCHEMA " + schema); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if _, err := admin.Exec("DROP SCHEMA " + schema + " CASCADE"); err != nil {
			t.Error(err)
		}
		_ = admin.Close()
	})
	endpoint, err := url.Parse(dsn)
	if err == nil && (endpoint.Scheme == "postgres" || endpoint.Scheme == "postgresql") {
		query := endpoint.Query()
		query.Set("search_path", schema)
		endpoint.RawQuery = query.Encode()
		dsn = endpoint.String()
	} else {
		dsn += " search_path=" + schema
	}
	key := bytes.Repeat([]byte{0x63}, 32)
	db, err := store.Open(context.Background(), dsn, key, zap.NewNop())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	return db, dsn, key
}

func TestRunCommandSyntheticTapeWritesFifteenDeterministicUntrustedRows(t *testing.T) {
	db, dsn, key := commandTestStore(t)
	ctx := context.Background()
	const secret = "synthetic-command-private-key"
	if err := db.SaveAccount(ctx, store.Account{ID: "sim-command", Label: "sim-command", CompanyID: "ours", Enabled: true}, store.Credentials{Handle: "sim-command", APIKey: secret}); err != nil {
		t.Fatal(err)
	}
	at := time.Date(2026, 10, 20, 0, 0, 0, 0, time.UTC)
	for i := range 3 {
		when := at.Add(time.Duration(i) * time.Minute)
		clock := fmt.Sprintf(`{"real_utc":%q,"unlocked_phase":1,"calendar":{"time_scale":60,"total_game_hours":%d}}`, when.Format(time.RFC3339Nano), 100+i)
		data := map[string]string{
			"clock":     clock,
			"catalog":   `{"ports":[{"id":"a","phase":1,"sea_zone_id":"coast","banking_region":"north"}],"ship_classes":[{"id":"hoy","phase":1,"hull_category":"coastal","base_speed_centiknots":600,"hold_capacity_tons":40,"crew_required":10}],"commodities":[],"sea_zones":[{"id":"coast","zone_class":"coastal"}]}`,
			"ships":     `[{"ship":{"id":"fixed-ship","ship_class":"hoy","status":"anchored_roadstead","current_port_id":"a"},"hold_capacity_tons":40,"effective_hold_capacity_tons":40,"cargo_manifest":{},"stores":{"provisions_tons":8}}]`,
			"vaults":    `{"vaults":[{"account_id":"north","account_type":"vault","banking_region":"north","balance_pence":100000}]}`,
			"contracts": `[]`, "factors": `[]`, "rates": `[]`, "fills": `[]`, "orders": `[]`,
			"ports/a": `{"id":"a","effective_berths":4}`, "profile": `{"company":{"home_port_id":"a"}}`,
		}
		frame := store.ReplayFrame{AccountID: "sim-command", Generation: fmt.Sprintf("command-%d", i), Source: "live", ObservedAt: when, Clock: json.RawMessage(clock), Phase: 1, Datasets: make(map[string]store.ReplayDataset)}
		for name, value := range data {
			meta, err := json.Marshal(engine.DatasetMeta{Complete: true, Generation: frame.Generation, UpdatedAt: when, AttemptedAt: when})
			if err != nil {
				t.Fatal(err)
			}
			frame.Datasets[name] = store.ReplayDataset{Data: []byte(value), Meta: meta}
		}
		if _, err := db.RecordReplayFrame(ctx, frame); err != nil {
			t.Fatal(err)
		}
	}
	seedCommandWindowEvidence(t, db, dsn, "sim-command", at)
	loaded, err := sim.LoadTape(ctx, db, "sim-command", at, at.Add(2*time.Minute))
	if err != nil {
		t.Fatal(err)
	}
	firstFrame, lastFrame := loaded.FrameBounds()
	if len(loaded.Frames) != 0 || !firstFrame.Equal(at) || !lastFrame.Equal(at.Add(time.Minute)) {
		t.Fatalf("database loader retained full frames or crossed exclusive --to: retained=%d first=%s last=%s", len(loaded.Frames), firstFrame, lastFrame)
	}
	if len(loaded.Settlements) != 1 || len(loaded.Actions) != 3 || len(loaded.Links) != 1 {
		t.Fatalf("window history loaded lifetime bodies or lost related receipts: settlements=%d actions=%d links=%d",
			len(loaded.Settlements), len(loaded.Actions), len(loaded.Links))
	}
	foundOpaque := false
	for _, action := range loaded.Actions {
		if action.Status == 429 {
			foundOpaque = string(action.Response) == `"opaque"` && action.RetryAfter != nil
		}
	}
	if !foundOpaque {
		t.Fatal("raw 429 response was rejected, discarded or interpreted as a receipt")
	}
	reader := loaded.Reader()
	if view, err := reader.Advance(at); err != nil || len(view.State.Ships) != 1 || view.State.Ships[0].Ship.ID != "fixed-ship" {
		t.Fatalf("streamed first frame differs from recorded fleet: %+v, %v", view.State.Ships, err)
	}
	t.Chdir(t.TempDir())
	t.Setenv("DATABASE_URL", dsn)
	t.Setenv("CREDENTIAL_KEY", base64.StdEncoding.EncodeToString(key))
	t.Setenv("HTTP_ADDR", "127.0.0.1:8585")
	t.Setenv("RESERVE_PENCE", "75000")
	var first []byte
	for iteration := range 2 {
		path := filepath.Join(t.TempDir(), "comparison.json")
		var text bytes.Buffer
		args := []string{"run", "--fit-until", at.Add(time.Minute).Format(time.RFC3339Nano), "--to", at.Add(2 * time.Minute).Format(time.RFC3339Nano), "--out", path, "--cpuprofile", path + ".pprof"}
		if iteration == 0 {
			args = append(args, "--from", at.Format(time.RFC3339Nano))
		}
		err := commandTo(args, &text)
		var untrusted gateError
		if !errors.As(err, &untrusted) {
			t.Fatalf("expected untrusted gate with usable comparison, got %v; %s", err, text.String())
		}
		if info, err := os.Stat(path + ".pprof"); err != nil || info.Size() == 0 {
			t.Fatalf("CPU profile was not flushed: %v", err)
		}
		encoded, err := os.ReadFile(path)
		if err != nil {
			t.Fatal(err)
		}
		var report comparisonReport
		if err := json.Unmarshal(encoded, &report); err != nil {
			t.Fatal(err)
		}
		if len(report.Rows) != 15 {
			t.Fatalf("got %d rows, want five policies under three scenarios", len(report.Rows))
		}
		for _, row := range report.Rows {
			if !strings.Contains(row.Status, "UNTRUSTED") {
				t.Fatalf("failed gate leaked a trusted result: %+v", row)
			}
		}
		if !strings.Contains(text.String(), "no recorded Lloyd's quotes") ||
			!strings.Contains(strings.Join(report.Validation.FailedRules, ";"), "no recorded Lloyd's quotes") {
			t.Fatal("pre-release tape did not explicitly disclose missing hazard pricing")
		}
		for _, row := range report.Rows {
			if row.HazardAvailable {
				t.Fatal("absent Lloyd's quotes became available hazard evidence")
			}
		}
		if strings.Contains(text.String(), secret) || bytes.Contains(encoded, []byte(secret)) || !strings.Contains(text.String(), "uncalibrated prior") {
			t.Fatal("output leaks credentials or hides the prior")
		}
		if iteration == 0 {
			first = encoded
		} else if !bytes.Equal(first, encoded) {
			t.Fatal("explicit and default frame-scoped windows did not produce byte-identical JSON")
		}
	}
	var validation bytes.Buffer
	err = commandTo([]string{"validate", "--to", at.Add(2 * time.Minute).Format(time.RFC3339Nano)}, &validation)
	var untrusted gateError
	if !errors.As(err, &untrusted) || !strings.Contains(validation.String(), "Validation gate: UNTRUSTED") || strings.Contains(validation.String(), "Evidence budget:") {
		t.Fatalf("default validation loaded irrelevant lifetime bodies: err=%v output=%s", err, validation.String())
	}
	var frames int
	if err := db.ScanReplayFrames(ctx, "sim-command", func(store.ReplayFrame) error { frames++; return nil }); err != nil || frames != 3 {
		t.Fatalf("read-only run changed replay evidence: frames=%d err=%v", frames, err)
	}
	readOnly, err := store.OpenReadOnly(ctx, dsn, key, zap.NewNop())
	if err != nil {
		t.Fatal(err)
	}
	defer readOnly.Close()
	if err := readOnly.SaveAccount(ctx, store.Account{ID: "forbidden"}, store.Credentials{Handle: "forbidden"}); err == nil {
		t.Fatal("read-only simulator connection accepted a write")
	}
}

func TestCommandEvidenceBudgetIsExplicitlyUntrustedWithoutPartialComparison(t *testing.T) {
	db, dsn, key := commandTestStore(t)
	ctx := context.Background()
	const account = "sim-budget"
	if err := db.SaveAccount(ctx, store.Account{ID: account, Label: account, Enabled: true}, store.Credentials{Handle: account, APIKey: "budget-secret"}); err != nil {
		t.Fatal(err)
	}
	from := time.Now().UTC().Add(-time.Minute)
	clock := json.RawMessage(fmt.Sprintf(`{"real_utc":%q,"calendar":{"time_scale":60,"total_game_hours":100}}`, from.Format(time.RFC3339Nano)))
	if _, err := db.RecordReplayFrame(ctx, store.ReplayFrame{AccountID: account, Generation: "budget-frame", Source: "live", Phase: 1, ObservedAt: from, Clock: clock}); err != nil {
		t.Fatal(err)
	}
	body, err := json.Marshal(map[string]string{"padding": strings.Repeat("x", store.MaxReplayEvidenceCiphertextBytes)})
	if err != nil {
		t.Fatal(err)
	}
	action, err := db.BeginAction(ctx, &store.Action{ID: "budget-action", AccountID: account, Method: "POST", Path: "/v1/market/place-order", Body: body})
	if err != nil {
		t.Fatal(err)
	}
	t.Chdir(t.TempDir())
	t.Setenv("DATABASE_URL", dsn)
	t.Setenv("CREDENTIAL_KEY", base64.StdEncoding.EncodeToString(key))
	t.Setenv("HTTP_ADDR", "127.0.0.1:8585")
	for _, operation := range []string{"validate", "run"} {
		var text bytes.Buffer
		path := filepath.Join(t.TempDir(), "must-not-exist.json")
		args := []string{operation, "--from", from.Format(time.RFC3339Nano), "--to", action.CreatedAt.Add(time.Minute).Format(time.RFC3339Nano)}
		if operation == "run" {
			args = append(args, "--out", path)
		}
		err := commandTo(args, &text)
		var untrusted gateError
		if !errors.As(err, &untrusted) || !strings.Contains(text.String(), "Validation gate: UNTRUSTED") || !strings.Contains(text.String(), "Evidence budget:") || !strings.Contains(text.String(), "ciphertext") {
			t.Fatalf("%s hid budget failure or used a partial history: err=%v text=%s", operation, err, text.String())
		}
		if _, err := os.Stat(path); !errors.Is(err, os.ErrNotExist) {
			t.Fatalf("partial comparison was produced after evidence budget failure: %v", err)
		}
		if strings.Contains(text.String(), "budget-secret") || strings.Contains(text.String(), strings.Repeat("x", 64)) {
			t.Fatal("budget failure exposed secret or oversized payload")
		}
	}
}

// Seed more than one lifetime budget of irrelevant history, plus a mission
// crossing --from. Invalid/oversized old ciphertext must never be fetched.
func seedCommandWindowEvidence(t *testing.T, db *store.Store, dsn, account string, at time.Time) {
	t.Helper()
	ctx := context.Background()
	sqlDB, err := sql.Open("pgx", dsn)
	if err != nil {
		t.Fatal(err)
	}
	defer sqlDB.Close()
	old := at.Add(-8 * 24 * time.Hour)
	count := store.MaxReplayEvidenceRows + 1
	for _, statement := range []string{
		`INSERT INTO actions (id, account_id, method, path, body_cipher, response_cipher, state, status, created_at, updated_at)
			SELECT 'old-action-' || n, $1, 'POST', '/v1/company/settings',
				CASE WHEN n = 1 THEN decode(repeat('aa', 131073), 'hex') ELSE decode('aa', 'hex') END,
				decode('aa', 'hex'), 'complete', 200, $2, $2 FROM generate_series(1, $3) n`,
		`INSERT INTO settlements (id, account_id, mission_id, action_id, lane_lookup, context_cipher, kind,
			quantity_tons, game_seconds, costs_known, acquisition_pence, proceeds_pence, fees_pence, provisions_pence, expenses_pence,
			started_at, completed_at, created_at)
			SELECT 'old-settlement-' || n, $1, 'old-mission-' || n, 'old-action-' || n, lpad(n::text, 64, '0'),
				CASE WHEN n = 1 THEN decode(repeat('aa', 131073), 'hex') ELSE decode(repeat('aa', 28), 'hex') END,
				'spot', 1, 3600, true, 1, 6, 0, 0, 0, $2, $2, $2 FROM generate_series(1, $3) n`,
		`INSERT INTO replay_action_links (account_id, action_id, mission_id, stage, sequence, generation)
			SELECT $1, 'old-action-' || n, 'old-mission-' || n, 'buy', n,
				CASE WHEN n = 1 THEN repeat('x', 16385) ELSE 'old' END FROM generate_series(1, $3) n
			WHERE $2::timestamptz IS NOT NULL`,
	} {
		if _, err := sqlDB.ExecContext(ctx, statement, account, old, count); err != nil {
			t.Fatal(err)
		}
	}
	const mission = "cross-window"
	sellID := ""
	for _, receipt := range []struct {
		stage, path, body, response string
		when                        time.Time
		status                      int
	}{
		{"buy", "/v1/market/place-order", `{"ship_id":"fixed-ship","side":"bid","commodity_id":"cloth","quantity":1}`, `{"filled_quantity":1,"total_gross_pence":100,"customs_duty_pence":0}`, at.Add(-4 * time.Minute), 200},
		{"sell", "/v1/market/place-order", `{"ship_id":"fixed-ship","side":"ask","commodity_id":"cloth","quantity":1}`, `{"filled_quantity":1,"total_gross_pence":200,"customs_duty_pence":0}`, at.Add(20 * time.Second), 200},
		{"settings", "/v1/company/settings", `{}`, `Too Many Requests: private response text`, at.Add(45 * time.Second), 429},
	} {
		id := engine.ActionID(account, mission, receipt.stage, 0)
		if _, err := db.BeginAction(ctx, &store.Action{ID: id, AccountID: account, Method: "POST", Path: receipt.path, Body: json.RawMessage(receipt.body)}); err != nil {
			t.Fatal(err)
		}
		var retry *time.Time
		if receipt.status == 429 {
			deadline := receipt.when.Add(time.Minute)
			retry = &deadline
		}
		if err := db.CompleteAction(ctx, id, receipt.status, json.RawMessage(receipt.response), retry); err != nil {
			t.Fatal(err)
		}
		// Restore the encrypted fixture at its historical timestamp using an
		// insert, without disabling or bypassing the immutable-update guard.
		if _, err := sqlDB.ExecContext(ctx, `WITH original AS (
			DELETE FROM actions WHERE id=$2 RETURNING *
		) INSERT INTO actions (id, account_id, company_id, method, path, body_cipher, state, status,
			response_cipher, retry_after, created_at, updated_at)
			SELECT id, account_id, company_id, method, path, body_cipher, state, status,
				response_cipher, retry_after, $1, $1 FROM original`, receipt.when, id); err != nil {
			t.Fatal(err)
		}
		if receipt.stage == "sell" {
			sellID = id
			if err := db.LinkReplayAction(ctx, store.ReplayActionLink{AccountID: account, ActionID: id, MissionID: mission, Stage: "sell"}); err != nil {
				t.Fatal(err)
			}
		}
	}
	row := store.Settlement{ID: "window-settlement", AccountID: account, ActionID: sellID, MissionID: mission,
		ShipID: "fixed-ship", Kind: "spot", Lane: "a|b|cloth", Origin: "a", Destination: "b", Commodity: "cloth",
		QuantityTons: 1, GameSeconds: 3600, CostsKnown: true, AcquisitionPence: 100, ProceedsPence: 200,
		StartedAt: at.Add(-5 * time.Minute), CompletedAt: at.Add(30 * time.Second), CreatedAt: at.Add(30 * time.Second)}
	if err := db.PutSettlement(ctx, &row); err != nil {
		t.Fatal(err)
	}
}

func TestComparisonNeverRanksHazardUnavailableRowsAsBetter(t *testing.T) {
	for _, missing := range []string{"none", "variant", "baseline"} {
		t.Run(missing, func(t *testing.T) {
			report := comparisonReport{Validation: sim.ValidationReport{Trusted: true}}
			for _, scenario := range []string{"zero", "half", "full"} {
				report.Rows = append(report.Rows,
					comparisonRow{Scenario: scenario, Status: "simulated estimate",
						VariantResult: sim.VariantResult{Variant: "baseline", HazardAvailable: missing != "baseline"}},
					comparisonRow{Scenario: scenario, Status: "simulated estimate",
						VariantResult: sim.VariantResult{Variant: "probe50", DeltaVsBaselinePence: 10,
							HazardAvailable: missing != "variant", HazardUnavailablePlans: 1}},
				)
			}
			var output bytes.Buffer
			if err := writeComparison(&output, report); err != nil {
				t.Fatal(err)
			}
			text := output.String()
			if missing == "none" {
				if !strings.Contains(text, "probe50: better in all three simulated scenarios") {
					t.Fatalf("fully priced comparison lost its verdict: %s", text)
				}
			} else if !strings.Contains(text, "probe50: inconclusive") || !strings.Contains(text, "hazard unavailable") {
				t.Fatalf("missing recorded lane risk still produced a favorable verdict: %s", text)
			}
		})
	}
}
