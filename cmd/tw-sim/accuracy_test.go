package main

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/DevYukine/go-tradewinds/internal/engine"
	"github.com/DevYukine/go-tradewinds/internal/sim"
	"github.com/DevYukine/go-tradewinds/internal/store"
)

func TestAccuracyCommandHelpAndArgumentValidation(t *testing.T) {
	t.Chdir(t.TempDir())
	var help bytes.Buffer
	if err := commandTo([]string{"--help"}, &help); err != nil || !strings.Contains(help.String(), "tw-sim accuracy") || !strings.Contains(help.String(), "--fit-until") {
		t.Fatalf("accuracy usage is missing: %s, %v", help.String(), err)
	}
	from, to := "2026-10-27T00:00:00Z", "2026-10-28T00:00:00Z"
	for _, test := range []struct {
		args []string
		want string
	}{
		{[]string{"--from", "bad"}, "--from must be RFC3339"},
		{[]string{"--to", "bad"}, "--to must be RFC3339"},
		{[]string{"--fit-until", "bad"}, "--fit-until must be RFC3339"},
		{[]string{"--from", to, "--to", from}, "--to must follow --from"},
		{[]string{"--from", from, "--to", to, "--fit-until", from}, "--fit-until must be strictly between"},
		{[]string{"--from", from, "--to", to, "--fit-until", to}, "--fit-until must be strictly between"},
		{[]string{"extra"}, "usage:"},
	} {
		var output bytes.Buffer
		err := commandTo(append([]string{"accuracy"}, test.args...), &output)
		if err == nil || !strings.Contains(err.Error(), test.want) {
			t.Fatalf("accuracy args %v reached configuration or lost their error: %v", test.args, err)
		}
	}
}

func seedAccuracyCommand(t *testing.T) (*store.Store, string, []byte, time.Time, time.Time) {
	t.Helper()
	db, dsn, key := commandTestStore(t)
	ctx := context.Background()
	const account = "accuracy-command"
	if err := db.SaveAccount(ctx, store.Account{ID: account, Label: account, Enabled: true}, store.Credentials{Handle: account, APIKey: "accuracy-private-secret"}); err != nil {
		t.Fatal(err)
	}
	from := time.Date(2026, 10, 27, 0, 0, 0, 0, time.UTC)
	last := from.Add(4 * time.Hour)
	for i, at := range []time.Time{from, last} {
		clock := json.RawMessage(fmt.Sprintf(`{"real_utc":%q,"calendar":{"time_scale":60}}`, at.Format(time.RFC3339Nano)))
		if _, err := db.RecordReplayFrame(ctx, store.ReplayFrame{AccountID: account, Generation: fmt.Sprintf("accuracy-frame-%d", i), Source: "live", Phase: 1, ObservedAt: at, Clock: clock}); err != nil {
			t.Fatal(err)
		}
	}
	for i, actual := range []int64{100, -100} {
		completed := from.Add(time.Duration(1+2*i) * time.Hour)
		actionID := fmt.Sprintf("accuracy-action-%d", i)
		if _, err := db.BeginAction(ctx, &store.Action{ID: actionID, AccountID: account, Method: "POST",
			Path: "/v1/market/place-order", Body: []byte(`{"side":"ask","quantity":10}`)}); err != nil {
			t.Fatal(err)
		}
		if err := db.CompleteAction(ctx, actionID, 200, []byte(`{"filled_quantity":10}`), nil); err != nil {
			t.Fatal(err)
		}
		row := store.Settlement{ID: fmt.Sprintf("accuracy-settlement-%d", i), AccountID: account, MissionID: fmt.Sprintf("accuracy-mission-%d", i),
			ActionID: actionID,
			Kind:     "spot", Origin: "a", Destination: "b", Commodity: "coffee_cocoa", Lane: "a|b|coffee_cocoa", QuantityTons: 10,
			CostsKnown: true, AcquisitionPence: 2000, FeesPence: 20, ProvisionsPence: 30, ExpensesPence: 50,
			ProceedsPence: 2100 + actual, StartedAt: completed.Add(-10 * time.Minute), CompletedAt: completed}
		if err := db.PutSettlement(ctx, &row); err != nil {
			t.Fatal(err)
		}
		plan, err := json.Marshal(engine.Plan{Kind: "spot", Commodity: row.Commodity, Quantity: 10, ExpectedArrival: completed,
			Economics: engine.Economics{ExpectedNetPence: 1000, RevenuePence: 3100, RevenueQuantityTons: 10, GameDays: 2}})
		if err != nil {
			t.Fatal(err)
		}
		if err := db.RecordReplayPlan(ctx, store.ReplayPlan{AccountID: account, MissionID: row.MissionID, Generation: row.MissionID,
			ObservedAt: row.StartedAt.Add(time.Millisecond), Plan: plan, Policy: []byte(`{}`)}); err != nil {
			t.Fatal(err)
		}
	}
	if err := db.RecordReplayPlan(ctx, store.ReplayPlan{AccountID: account, MissionID: "unrelated", Generation: "unrelated",
		ObservedAt: from, Plan: []byte(`{}`), Policy: []byte(`{}`)}); err != nil {
		t.Fatal(err)
	}
	sqlDB, err := sql.Open("pgx", dsn)
	if err != nil {
		t.Fatal(err)
	}
	defer sqlDB.Close()
	if _, err := sqlDB.ExecContext(ctx, "UPDATE replay_plans SET plan_cipher = decode('aa', 'hex') WHERE account_id = $1 AND mission_id = 'unrelated'", account); err != nil {
		t.Fatal(err)
	}
	return db, dsn, key, from, last.Add(time.Nanosecond)
}

func TestAccuracyCommandDefaultsAreDeterministicAndBadForecastIsNotAGate(t *testing.T) {
	db, dsn, key, from, to := seedAccuracyCommand(t)
	t.Chdir(t.TempDir())
	t.Setenv("DATABASE_URL", dsn)
	t.Setenv("CREDENTIAL_KEY", base64.StdEncoding.EncodeToString(key))
	t.Setenv("HTTP_ADDR", "127.0.0.1:8585")
	fit := from.Add(to.Sub(from) / 2)
	var first []byte
	for iteration := range 3 {
		path := filepath.Join(t.TempDir(), "accuracy.json")
		args := []string{"accuracy", "--out", path}
		if iteration == 0 {
			args = append(args, "--account", "accuracy-command", "--from", from.Format(time.RFC3339Nano), "--to", to.Format(time.RFC3339Nano), "--fit-until", fit.Format(time.RFC3339Nano))
		}
		var output bytes.Buffer
		if err := commandTo(args, &output); err != nil {
			t.Fatalf("inaccurate forecasts or missing validation data became a gate: %v; %s", err, output.String())
		}
		encoded, err := os.ReadFile(path)
		if err != nil {
			t.Fatal(err)
		}
		var report sim.AccuracyReport
		if err := json.Unmarshal(encoded, &report); err != nil {
			t.Fatal(err)
		}
		if !report.From.Equal(from) || !report.To.Equal(to) || !report.FitUntil.Equal(fit) || report.ByKind["spot"].Covered != 2 ||
			report.ByKind["spot"].ErrorPence != -2000 || report.ByKind["spot"].SelectedRevenueMissPence != -2000 ||
			report.ByKind["spot"].SelectedCostMissPence != 0 || report.Calibration["spot"].Median != 0.1 ||
			report.Holdout["spot"].ErrorPence != -200 || report.Holdout["spot"].FalseLossRate != 1 {
			t.Fatalf("default bounds, bounded plans, ratio fit or holdout score changed: %+v", report)
		}
		if strings.Contains(output.String(), "Validation gate:") || strings.Contains(output.String(), "accuracy-private-secret") || bytes.Contains(encoded, []byte("accuracy-private-secret")) ||
			!strings.Contains(output.String(), "Admission forecast") || !strings.Contains(output.String(), "Latest forecast") ||
			!strings.Contains(output.String(), "Selected-cost-miss(Cr)") || !strings.Contains(output.String(), "cost/reprice bridge") {
			t.Fatalf("accuracy ran validation, leaked a credential or hid separate cohorts: %s", output.String())
		}
		if iteration == 0 {
			first = encoded
		} else if !bytes.Equal(first, encoded) {
			t.Fatal("explicit and default accuracy windows produced different JSON")
		}
	}
	rows, err := db.SettlementsBetween(context.Background(), "accuracy-command", from, to)
	if err != nil || len(rows) != 2 || rows[1].ProceedsPence != 2000 {
		t.Fatalf("read-only accuracy changed settled evidence: %+v, %v", rows, err)
	}
	// PostgreSQL timestamps have microsecond precision. Include the final
	// frame strictly before this loader bound, not one nanosecond after it.
	loaded, err := sim.LoadAccuracyTape(context.Background(), db, "accuracy-command", from, to.Add(time.Microsecond))
	if err != nil || loaded == nil || len(loaded.Plans) != 2 || len(loaded.Settlements) != 2 {
		t.Fatalf("accuracy tape did not carry only settlement mission plans: %+v, %v", loaded, err)
	}
	firstFrame, lastFrame := loaded.FrameBounds()
	if !firstFrame.Equal(from) || !lastFrame.Equal(to.Add(-time.Nanosecond)) ||
		len(loaded.Actions) != 0 || len(loaded.Links) != 0 || len(loaded.AccountingSnapshot) != 0 {
		t.Fatalf("accuracy loader lost frame bounds or retained unrelated evidence: %+v", loaded)
	}
	loaded, err = sim.LoadTape(context.Background(), db, "accuracy-command", from, to)
	if err != nil || loaded == nil || len(loaded.Plans) != 0 {
		t.Fatalf("run/validate loader acquired replay-plan evidence: %+v, %v", loaded, err)
	}
}

func TestAccuracyCommandEvidenceBudgetIsExitTwoWithoutGateOrPartialJSON(t *testing.T) {
	db, dsn, key, from, to := seedAccuracyCommand(t)
	sqlDB, err := sql.Open("pgx", dsn)
	if err != nil {
		t.Fatal(err)
	}
	defer sqlDB.Close()
	if _, err := sqlDB.Exec("UPDATE replay_plans SET generation = repeat('x', $1) WHERE mission_id = 'accuracy-mission-0'", store.MaxReplayEvidenceMetadataBytes+1); err != nil {
		t.Fatal(err)
	}
	loaded, err := sim.LoadTape(context.Background(), db, "accuracy-command", from, to)
	if err != nil || loaded == nil || len(loaded.Plans) != 0 {
		t.Fatalf("accuracy plan budget blocked the run/validate loader: %+v, %v", loaded, err)
	}
	t.Chdir(t.TempDir())
	t.Setenv("DATABASE_URL", dsn)
	t.Setenv("CREDENTIAL_KEY", base64.StdEncoding.EncodeToString(key))
	t.Setenv("HTTP_ADDR", "127.0.0.1:8585")
	for _, operation := range []string{"validate", "run"} {
		var text bytes.Buffer
		err := commandTo([]string{operation, "--from", from.Format(time.RFC3339Nano), "--to", to.Format(time.RFC3339Nano)}, &text)
		if errors.Is(err, store.ErrReplayEvidenceBudget) ||
			!strings.Contains(text.String(), "Validation gate:") || strings.Contains(text.String(), "Evidence budget:") {
			t.Fatalf("%s acquired the accuracy-only plan budget: %v; %s", operation, err, text.String())
		}
	}
	path := filepath.Join(t.TempDir(), "must-not-exist.json")
	var output bytes.Buffer
	err = commandTo([]string{"accuracy", "--from", from.Format(time.RFC3339Nano), "--to", to.Format(time.RFC3339Nano), "--out", path}, &output)
	var failedGate gateError
	if !errors.Is(err, store.ErrReplayEvidenceBudget) || errors.As(err, &failedGate) || !strings.Contains(output.String(), "Evidence budget:") ||
		strings.Contains(output.String(), "Validation gate:") || !strings.Contains(output.String(), "No accuracy report") {
		t.Fatalf("accuracy budget failure was not distinct from a forecast gate: %v; %s", err, output.String())
	}
	if _, err := os.Stat(path); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("accuracy wrote a partial report after an evidence budget failure: %v", err)
	}
	if strings.Contains(output.String(), strings.Repeat("x", 64)) || strings.Contains(output.String(), "accuracy-private-secret") {
		t.Fatal("accuracy evidence error exposed private row contents")
	}
}

func TestAccuracyLoaderSkipsActionAndAccountingEvidence(t *testing.T) {
	db, dsn, key, from, to := seedAccuracyCommand(t)
	ctx := context.Background()
	if err := db.SaveSnapshot(ctx, "accuracy-command", map[string]json.RawMessage{
		"ledger": json.RawMessage(`[{"id":"accounting-only"}]`),
	}); err != nil {
		t.Fatal(err)
	}
	sqlDB, err := sql.Open("pgx", dsn)
	if err != nil {
		t.Fatal(err)
	}
	defer sqlDB.Close()
	if _, err := sqlDB.ExecContext(ctx, `INSERT INTO actions
		(id, account_id, method, path, body_cipher, response_cipher, state, status, created_at, updated_at)
		VALUES ('accuracy-unneeded-action', 'accuracy-command', 'POST', '/v1/company/settings',
			decode(repeat('aa', $1), 'hex'), decode('aa', 'hex'), 'complete', 200, $2, $2)`,
		store.MaxReplayEvidenceCiphertextBytes+1, from.Add(time.Minute)); err != nil {
		t.Fatal(err)
	}
	if _, err := sim.LoadTape(ctx, db, "accuracy-command", from, to); !errors.Is(err, store.ErrReplayEvidenceBudget) {
		t.Fatalf("fixture did not require bounded action evidence for run/validate: %v", err)
	}
	tape, err := sim.LoadAccuracyTape(ctx, db, "accuracy-command", from, to)
	if err != nil || tape == nil || len(tape.Settlements) != 2 || len(tape.Plans) != 2 ||
		len(tape.Actions) != 0 || len(tape.Links) != 0 || len(tape.AccountingSnapshot) != 0 {
		t.Fatalf("accuracy loader depended on unused action/accounting evidence: %+v, %v", tape, err)
	}
	t.Chdir(t.TempDir())
	t.Setenv("DATABASE_URL", dsn)
	t.Setenv("CREDENTIAL_KEY", base64.StdEncoding.EncodeToString(key))
	t.Setenv("HTTP_ADDR", "127.0.0.1:8585")
	var text bytes.Buffer
	if err := commandTo([]string{"accuracy", "--from", from.Format(time.RFC3339Nano), "--to", to.Format(time.RFC3339Nano)}, &text); err != nil ||
		!strings.Contains(text.String(), "Forecast accuracy:") || strings.Contains(text.String(), "Evidence budget:") {
		t.Fatalf("accuracy command loaded unused action/accounting evidence: %v; %s", err, text.String())
	}
}
