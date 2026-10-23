package engine

// Consumer regressions for the bounded INFO fleet-status line. Each test
// calls logStatus directly with deterministic timestamps and asserts the
// emitted entries — cadence, freshness separation, redaction-safe reason
// caps, fail-closed credential handling and the disabled-logger path. The
// Run-loop wiring itself is exercised by the CLI smoke, not here.

import (
	"context"
	"strings"
	"testing"
	"time"
	"unicode/utf8"

	"github.com/DevYukine/go-tradewinds/internal/store"
	"go.uber.org/zap"
	"go.uber.org/zap/zaptest/observer"
)

func statusShip(id, status string) ShipState {
	state := ShipState{}
	state.Ship.ID = id
	state.Ship.Name = id
	state.Ship.Status = status
	return state
}

func statusEntries(t *testing.T, logs *observer.ObservedLogs) []observer.LoggedEntry {
	t.Helper()
	entries := logs.FilterMessage("fleet status").All()
	for _, entry := range entries {
		if entry.Level != zap.InfoLevel {
			t.Fatalf("fleet status must stay at INFO: %+v", entry)
		}
	}
	return entries
}

func requireFields(t *testing.T, entry observer.LoggedEntry, keys ...string) map[string]any {
	t.Helper()
	fields := entry.ContextMap()
	for _, key := range keys {
		if _, ok := fields[key]; !ok {
			t.Fatalf("status entry missing %s: %v", key, fields)
		}
	}
	return fields
}

// statusCount reads a numeric status field in the representation the zap
// observer hands back (ContextMap normalizes integers to int64).
func statusCount(t *testing.T, fields map[string]any, key string) int64 {
	t.Helper()
	value, ok := fields[key].(int64)
	if !ok {
		t.Fatalf("%s = %v (%T), want an integer count", key, fields[key], fields[key])
	}
	return value
}

func TestStatusFirstCallAndMinuteCadence(t *testing.T) {
	h := newRecoveryHarness(t)
	runner := h.runner(primaryTestKey)
	core, logs := observer.New(zap.DebugLevel)
	runner.log = zap.New(core)
	runner.generation = "status-generation"
	observedAt := time.Now().UTC()
	runner.meta["ships"] = DatasetMeta{
		Generation:  "status-generation",
		AttemptedAt: observedAt,
		UpdatedAt:   observedAt,
		Complete:    true,
	}
	runner.stateCache.Ships = []ShipState{
		statusShip("lapras", "berthed"),
		statusShip("charizard", "sailing"),
		statusShip("blastoise", "sunk"),
		statusShip("snorlax", "frozen"),
	}

	first := time.Now().UTC().Truncate(time.Second)
	runner.logStatus(h.ctx, first)
	entries := statusEntries(t, logs)
	if len(entries) != 1 {
		t.Fatalf("first status must emit exactly once: %+v", entries)
	}
	fields := requireFields(t, entries[0],
		"mode", "reconciled_at", "fleet_fresh", "fleet_observed_at",
		"ships", "sailing", "in_port", "sunk", "other", "holding_reasons")
	if fields["mode"] != "execute" {
		t.Fatalf("status mode = %v, want the configured execution mode", fields["mode"])
	}
	reconciled, ok := fields["reconciled_at"].(time.Time)
	if !ok || !reconciled.Equal(first) {
		t.Fatalf("reconciled_at = %v, want the emitting call's time %v", fields["reconciled_at"], first)
	}
	if fields["fleet_fresh"] != true {
		t.Fatalf("a fresh observed fleet must be marked fresh: %v", fields["fleet_fresh"])
	}
	if observed, ok := fields["fleet_observed_at"].(time.Time); !ok || !observed.Equal(observedAt) {
		t.Fatalf("fleet_observed_at = %v, want the ships observation %v", fields["fleet_observed_at"], observedAt)
	}
	for key, want := range map[string]int64{"ships": 4, "sailing": 1, "in_port": 1, "sunk": 1, "other": 1} {
		if got := statusCount(t, fields, key); got != want {
			t.Fatalf("%s = %d, want %d", key, got, want)
		}
	}
	if got := statusCount(t, fields, "holding_reasons"); got != 0 {
		t.Fatalf("holding_reasons = %d, want the recorded reason count", got)
	}
	if _, claimed := fields["holding_reason"]; claimed {
		t.Fatal("empty holdings must not produce a reason or imply trading success")
	}

	runner.logStatus(h.ctx, first.Add(59*time.Second))
	if entries := statusEntries(t, logs); len(entries) != 1 {
		t.Fatalf("status within one real minute must stay silent: %+v", entries)
	}

	runner.logStatus(h.ctx, first.Add(60*time.Second))
	entries = statusEntries(t, logs)
	if len(entries) != 2 {
		t.Fatalf("the first successful cycle at least one minute later must emit: %+v", entries)
	}
	reconciled, ok = entries[1].ContextMap()["reconciled_at"].(time.Time)
	if !ok || !reconciled.Equal(first.Add(60*time.Second)) {
		t.Fatalf("periodic reconciled_at = %v, want %v", entries[1].ContextMap()["reconciled_at"], first.Add(60*time.Second))
	}
}

func TestStatusOmitsCountsForMissingAndStaleShips(t *testing.T) {
	h := newRecoveryHarness(t)
	runner := h.runner(primaryTestKey)
	core, logs := observer.New(zap.DebugLevel)
	runner.log = zap.New(core)
	runner.generation = "status-generation"
	base := time.Now().UTC().Truncate(time.Second)

	t.Run("missing ships dataset omits counts and observation time", func(t *testing.T) {
		runner.stateCache.Ships = nil
		runner.logStatus(h.ctx, base)
		entries := statusEntries(t, logs)
		if len(entries) != 1 {
			t.Fatalf("missing fleet must still emit its status: %+v", entries)
		}
		fields := requireFields(t, entries[0], "mode", "reconciled_at", "fleet_fresh", "holding_reasons")
		if fields["fleet_fresh"] != false {
			t.Fatal("a missing fleet must never be reported as a fresh empty one")
		}
		for _, key := range []string{"ships", "sailing", "in_port", "sunk", "other", "fleet_observed_at"} {
			if _, claimed := fields[key]; claimed {
				t.Fatalf("missing fleet must omit %s", key)
			}
		}
	})

	t.Run("stale ships keep observation time but omit counts", func(t *testing.T) {
		staleAt := time.Now().UTC().Add(-time.Hour)
		runner.meta["ships"] = DatasetMeta{
			Generation:  "status-generation",
			AttemptedAt: staleAt,
			UpdatedAt:   staleAt,
			Complete:    true,
			Stale:       true,
		}
		runner.stateCache.Ships = []ShipState{statusShip("lapras", "berthed")}
		runner.logStatus(h.ctx, base.Add(61*time.Second))
		entries := statusEntries(t, logs)
		if len(entries) != 2 {
			t.Fatalf("stale fleet must still emit its status: %+v", entries)
		}
		fields := entries[1].ContextMap()
		if fields["fleet_fresh"] != false {
			t.Fatal("a stale fleet must not be marked fresh")
		}
		for _, key := range []string{"ships", "sailing", "in_port", "sunk", "other"} {
			if _, claimed := fields[key]; claimed {
				t.Fatalf("stale fleet must omit %s instead of claiming a count", key)
			}
		}
		observed, ok := fields["fleet_observed_at"].(time.Time)
		if !ok || !observed.Equal(staleAt) {
			t.Fatalf("fleet_observed_at = %v, want the stale observation %v", fields["fleet_observed_at"], staleAt)
		}
	})

	t.Run("observe mode is labeled", func(t *testing.T) {
		observe, observeLogs := observer.New(zap.DebugLevel)
		idle := New(nil, h.db, h.account, zap.New(observe), Options{})
		idle.generation = runner.generation
		idle.logStatus(h.ctx, base.Add(122*time.Second))
		entries := observeLogs.FilterMessage("fleet status").All()
		if len(entries) != 1 {
			t.Fatalf("observe runner must emit its status: %+v", entries)
		}
		if entries[0].ContextMap()["mode"] != "observe" {
			t.Fatalf("status mode = %v, want the observe mode", entries[0].ContextMap()["mode"])
		}
	})
}

func TestStatusRedactsCapsAndLabelsHoldingReasons(t *testing.T) {
	h := newRecoveryHarness(t)
	runner := h.runner(primaryTestKey)
	core, logs := observer.New(zap.DebugLevel)
	runner.log = zap.New(core)
	now := time.Now().UTC().Truncate(time.Second)

	t.Run("credential crossing the cap is scrubbed before the 512-rune cap", func(t *testing.T) {
		crossing := strings.Repeat("a", 500) + primaryTestKey + strings.Repeat("b", 30)
		runner.holding = []string{crossing, "second waiting reason"}
		runner.logStatus(h.ctx, now)
		entries := statusEntries(t, logs)
		if len(entries) != 1 {
			t.Fatalf("expected the first status: %+v", entries)
		}
		fields := entries[0].ContextMap()
		if got := statusCount(t, fields, "holding_reasons"); got != 2 {
			t.Fatalf("holding_reasons = %d, want the recorded reason count", got)
		}
		// The full reason must be scrubbed first and only then capped, so the
		// expected line is the redacted string truncated to its first 512
		// runes — any key prefix surviving proves the order was reversed.
		scrubbed := strings.ReplaceAll(crossing, primaryTestKey, "[REDACTED]")
		capped := string([]rune(scrubbed)[:512])
		reason, ok := fields["holding_reason"].(string)
		if !ok {
			t.Fatalf("holding_reason = %v, want the scrubbed capped first reason", fields["holding_reason"])
		}
		if reason != capped {
			t.Fatalf("holding_reason deviates from scrub-then-cap: got %q want %q", reason, capped)
		}
		if strings.Contains(reason, primaryTestKey[:8]) || strings.Contains(reason, primaryTestKey) {
			t.Fatal("a credential crossing the cap boundary leaked into the status line")
		}
		if !utf8.ValidString(reason) {
			t.Fatal("the character cap split a multi-byte rune")
		}
		if got := utf8.RuneCountInString(reason); got != 512 {
			t.Fatalf("capped reason length = %d runes, want the 512-character cap", got)
		}
		if fields["holding_reason_truncated"] != true {
			t.Fatalf("a capped reason must be labeled truncated: %v", fields["holding_reason_truncated"])
		}
	})

	t.Run("multi-byte text is capped on rune boundaries", func(t *testing.T) {
		runner.holding = []string{strings.Repeat("é", 600)}
		runner.logStatus(h.ctx, now.Add(61*time.Second))
		entries := statusEntries(t, logs)
		if len(entries) != 2 {
			t.Fatalf("expected the periodic status: %+v", entries)
		}
		fields := entries[1].ContextMap()
		expected := strings.Repeat("é", 512)
		reason, ok := fields["holding_reason"].(string)
		if !ok {
			t.Fatalf("holding_reason = %v, want the rune-capped reason", fields["holding_reason"])
		}
		if reason != expected {
			t.Fatalf("multi-byte cap produced %q (%d runes), want the first 512 runes", reason, utf8.RuneCountInString(reason))
		}
		if fields["holding_reason_truncated"] != true {
			t.Fatalf("a capped reason must be labeled truncated: %v", fields["holding_reason_truncated"])
		}
	})

	t.Run("uncapped reason stays complete without a truncation label", func(t *testing.T) {
		runner.holding = []string{"short waiting reason"}
		runner.logStatus(h.ctx, now.Add(122*time.Second))
		entries := statusEntries(t, logs)
		if len(entries) != 3 {
			t.Fatalf("expected the third status: %+v", entries)
		}
		fields := entries[2].ContextMap()
		if fields["holding_reason"] != "short waiting reason" {
			t.Fatalf("uncapped reason = %v, want the full scrubbed reason", fields["holding_reason"])
		}
		if truncated, present := fields["holding_reason_truncated"]; present && truncated == true {
			t.Fatal("an uncapped reason must not be labeled truncated")
		}
	})
}

func TestStatusFailsClosedWhenCredentialsUnreadable(t *testing.T) {
	h := newRecoveryHarness(t)
	runner := h.runner(primaryTestKey)
	core, logs := observer.New(zap.DebugLevel)
	runner.log = zap.New(core)
	runner.holding = []string{"waiting near " + primaryTestKey}

	if err := h.db.Close(); err != nil {
		t.Fatal(err)
	}
	runner.logStatus(h.ctx, time.Now().UTC())

	entries := statusEntries(t, logs)
	if len(entries) != 1 {
		t.Fatalf("unreadable credentials must redact, not suppress, the status: %+v", entries)
	}
	reason, ok := entries[0].ContextMap()["holding_reason"].(string)
	if !ok || reason != "[REDACTED]" {
		t.Fatalf("holding_reason = %v, want the fail-closed redaction marker", entries[0].ContextMap()["holding_reason"])
	}
	if strings.Contains(reason, primaryTestKey) {
		t.Fatal("unreadable credentials leaked into the status line")
	}
}

func TestStatusDisabledNeverTouchesDatabase(t *testing.T) {
	account := store.Account{ID: recoveryAccountID, CompanyID: recoveryCompanyID}
	runner := New(nil, nil, account, zap.NewNop(), Options{Execute: true})
	runner.holding = append(runner.holding, "would need credential lookup")

	runner.logStatus(context.Background(), time.Now().UTC())
}
