package engine

import (
	"encoding/json"
	"net/http"
	"reflect"
	"testing"
	"time"

	"github.com/DevYukine/go-tradewinds/internal/store"
)

// Both refresh harnesses serve documented R&D documents instead of treating
// these reads as unsupported endpoints. The register deliberately contains
// fields outside RndStatus: capture must retain it without projecting it.
func rndStatusFixture() map[string]any {
	return map[string]any{
		"can_start":                 false,
		"cannot_start_reason":       "net worth below entry requirement",
		"player_net_worth_pence":    int64(101_000_000),
		"entry_net_worth_pence":     int64(200_000_000),
		"start_fee_pence":           int64(5_000_000),
		"upkeep_per_game_day_pence": int64(45_000),
		"current_game_day":          int64(123),
		"project":                   nil,
	}
}

func rndRegisterFixture() []any {
	return []any{map[string]any{
		"component_id":        "rival-sails",
		"name":                "Rival Sails",
		"slot":                "sails",
		"grade":               1,
		"effects":             []any{"improves speed"},
		"status":              "held",
		"granted_at":          "2026-10-01T00:00:00Z",
		"holder_company_id":   "55555555-5555-4555-8555-555555555555",
		"holder_company_name": "Rival Company",
	}}
}

func rndDecisionHistory(t *testing.T, h *recoveryHarness) []store.Decision {
	t.Helper()
	rows, err := h.db.History(h.ctx, h.account.ID, "decisions", 500)
	if err != nil {
		t.Fatalf("load R&D decisions: %v", err)
	}
	var result []store.Decision
	for _, row := range rows.([]store.Decision) {
		if row.Kind == "rnd" {
			result = append(result, row)
		}
	}
	return result
}

func assertRndRawSnapshot(t *testing.T, h *recoveryHarness) {
	t.Helper()
	snapshot, err := h.db.Snapshot(h.ctx, h.account.ID)
	if err != nil {
		t.Fatalf("load R&D snapshot: %v", err)
	}
	for key, want := range map[string]any{
		"rnd_status": rndStatusFixture(), "rnd_register": rndRegisterFixture(),
	} {
		var got, expected any
		if err := json.Unmarshal(snapshot[key], &got); err != nil {
			t.Fatalf("decode persisted %s: %v", key, err)
		}
		if err := json.Unmarshal(encode(want), &expected); err != nil {
			t.Fatal(err)
		}
		if !reflect.DeepEqual(got, expected) {
			t.Fatalf("persisted %s = %#v, want raw document %#v", key, got, expected)
		}
	}
}

func TestRndObservationPersistsReadOnlyDecision(t *testing.T) {
	t.Parallel()
	h := newRecoveryHarness(t)
	runner := h.runner(primaryTestKey)
	runner.generation = "rnd-persisted"
	if err := runner.refresh(h.ctx); err != nil {
		t.Fatalf("refresh R&D observation: %v", err)
	}
	rows := rndDecisionHistory(t, h)
	if len(rows) != 1 {
		t.Fatalf("persisted R&D decisions = %d, want 1", len(rows))
	}
	row := rows[0]
	if row.AccountID != h.account.ID || row.ShipID != "" || row.Reason != "R&D waits: net worth below entry requirement" {
		t.Fatalf("unexpected persisted R&D decision: %+v", row)
	}
	var got map[string]json.RawMessage
	if err := json.Unmarshal(row.Details, &got); err != nil {
		t.Fatalf("decode persisted R&D details: %v", err)
	}
	want := map[string]any{
		"can_start":                    false,
		"cannot_start_reason":          "net worth below entry requirement",
		"player_net_worth_pence":       int64(101_000_000),
		"entry_net_worth_pence":        int64(200_000_000),
		"start_fee_pence":              int64(5_000_000),
		"upkeep_per_game_day_pence":    int64(45_000),
		"estimated_project_game_days":  int64(150),
		"estimated_project_cost_pence": int64(11_750_000),
		"no_start_reason":              "design value is unknown and fleet growth competes for the same cash",
	}
	if len(got) != len(want) {
		t.Fatalf("persisted R&D detail count = %d, want %d", len(got), len(want))
	}
	for key, expected := range want {
		if string(got[key]) != string(encode(expected)) {
			t.Errorf("persisted %s = %s, want %s", key, got[key], encode(expected))
		}
	}
	assertRndRawSnapshot(t, h)
	for _, key := range []string{"rnd_status", "rnd_register"} {
		meta := runner.meta[key]
		if !meta.Complete || meta.Stale || meta.Error != "" || meta.Generation != runner.generation || meta.AttemptedAt.IsZero() || meta.UpdatedAt.IsZero() {
			t.Fatalf("R&D fixture failed capture for %s: %+v", key, meta)
		}
	}
	h.api.mu.Lock()
	defer h.api.mu.Unlock()
	if len(h.api.mutations) != 0 {
		t.Fatalf("observation issued %d mutations", len(h.api.mutations))
	}
}

func TestRndObservationRealTimeGatingFailureAndRetry(t *testing.T) {
	t.Parallel()
	h := newRecoveryHarness(t)
	fake := newSnapshotConcurrencyAPI(t, nil)
	runner := concurrencyRunner(h, fake)
	refresh := func(generation string, statusCalls, registerCalls, decisions int) {
		t.Helper()
		runner.generation = generation
		// Do not let generic decision deduplication conceal an erroneous call
		// to decision() on a cached status, including in the same generation.
		delete(runner.decisionHashes, "|rnd")
		if err := runner.refresh(h.ctx); err != nil {
			t.Fatalf("refresh %s: %v", generation, err)
		}
		if got := fake.arrivedCount("/v1/rnd/status"); got != statusCalls {
			t.Fatalf("%s: status GETs = %d, want %d", generation, got, statusCalls)
		}
		if got := fake.arrivedCount("/v1/rnd/register"); got != registerCalls {
			t.Fatalf("%s: register GETs = %d, want %d", generation, got, registerCalls)
		}
		if got := len(rndDecisionHistory(t, h)); got != decisions {
			t.Fatalf("%s: R&D decisions = %d, want %d", generation, got, decisions)
		}
		if runner.lastMutationStatus.Load() != 0 {
			t.Fatal("R&D observation attempted a mutation")
		}
	}
	age := func(key string, attempted, updated time.Duration) {
		t.Helper()
		meta := runner.meta[key]
		now := time.Now()
		meta.AttemptedAt = now.Add(-attempted)
		meta.UpdatedAt = now.Add(-updated)
		runner.meta[key] = meta
	}

	refresh("rnd-first", 1, 1, 1)
	assertRndRawSnapshot(t, h)
	// Reload the persisted metadata to prove restarts do not reset the gate.
	runner = concurrencyRunner(h, fake)
	refresh("rnd-first", 1, 1, 1)
	refresh("rnd-new-generation-cached", 1, 1, 1)

	// Each dataset uses the later of its last attempt and last success, in
	// real time, irrespective of the game's accelerated calendar.
	age("rnd_status", 29*time.Minute, 31*time.Minute)
	age("rnd_register", 31*time.Minute, 29*time.Minute)
	refresh("rnd-before-thirty-minutes", 1, 1, 1)
	age("rnd_status", 30*time.Minute+time.Second, 31*time.Minute)
	refresh("rnd-status-only-due", 2, 1, 2)
	age("rnd_register", 31*time.Minute, 30*time.Minute+time.Second)
	refresh("rnd-register-only-due", 2, 2, 2)

	fake.setFailureStatus("/v1/rnd/status", http.StatusNotFound)
	fake.setFailureStatus("/v1/rnd/register", http.StatusNotFound)
	for _, key := range []string{"rnd_status", "rnd_register"} {
		age(key, 31*time.Minute, 31*time.Minute)
	}
	beforeFailure := time.Now()
	refresh("rnd-failed", 3, 3, 2)
	assertRndRawSnapshot(t, h)
	persisted, err := h.db.Snapshot(h.ctx, h.account.ID)
	if err != nil {
		t.Fatal(err)
	}
	metadata := decode[map[string]DatasetMeta](persisted, "_meta")
	for _, key := range []string{"rnd_status", "rnd_register"} {
		meta := metadata[key]
		if meta.Complete || !meta.Stale || meta.Error == "" || meta.Generation == "rnd-failed" || meta.AttemptedAt.Before(beforeFailure) {
			t.Fatalf("failed %s did not retain last-good generation and recent failed attempt: %+v", key, meta)
		}
	}

	fake.setFailureStatus("/v1/rnd/status", 0)
	fake.setFailureStatus("/v1/rnd/register", 0)
	runner = concurrencyRunner(h, fake)
	refresh("rnd-retry-not-yet-due", 3, 3, 2)
	for _, key := range []string{"rnd_status", "rnd_register"} {
		age(key, 29*time.Minute, time.Hour)
	}
	refresh("rnd-failure-before-thirty-minutes", 3, 3, 2)
	for _, key := range []string{"rnd_status", "rnd_register"} {
		age(key, 30*time.Minute+time.Second, time.Hour)
	}
	refresh("rnd-recovered", 4, 4, 3)
	assertRndRawSnapshot(t, h)
	for _, key := range []string{"rnd_status", "rnd_register"} {
		meta := runner.meta[key]
		if !meta.Complete || meta.Stale || meta.Error != "" || meta.Generation != "rnd-recovered" {
			t.Fatalf("recovered %s metadata = %+v", key, meta)
		}
	}
}
