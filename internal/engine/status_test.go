package engine

import (
	"strings"
	"testing"
	"time"
	"unicode/utf8"

	"go.uber.org/zap"
	"go.uber.org/zap/zaptest/observer"
)

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

func observedStatusRunner(h *recoveryHarness) (*Runner, *observer.ObservedLogs) {
	runner := h.runner(primaryTestKey)
	core, logs := observer.New(zap.DebugLevel)
	runner.log = zap.New(core)
	runner.generation = "status-generation"
	now := time.Now().UTC()
	runner.meta["ships"] = DatasetMeta{
		Generation: runner.generation, AttemptedAt: now, UpdatedAt: now, Complete: true,
	}
	return runner, logs
}

func TestStartupStatusRetainsFleetCountsHoldsAndConfiguredMode(t *testing.T) {
	t.Parallel()
	for _, execute := range []bool{true, false} {
		mode := "observe"
		if execute {
			mode = "execute"
		}
		t.Run(mode, func(t *testing.T) {
			h := newRecoveryHarness(t)
			runner, logs := observedStatusRunner(h)
			runner.opts.Execute = execute
			runner.stateCache.Ships = []ShipState{
				operationShip(recoveryShipLapras, "Lapras", "sailing", recoveryHomePort),
				operationShip(recoveryShipCharizard, "Charizard", "berthed", recoveryFarPort),
				operationShip(recoveryShipBlastoise, "Blastoise", "anchored_roadstead", recoveryHomePort),
				operationShip("sunk-hull", "Pikachu", "sunk", ""),
				operationShip("building-hull", "Raichu", "building", recoveryHomePort),
			}
			runner.holding = []currentHold{
				{ShipID: recoveryShipLapras, Reason: "authoritative voyage still in flight"},
				{Reason: "working capital is committed"},
			}
			at := time.Now().UTC()
			runner.logStatus(h.ctx, at)
			entries := statusEntries(t, logs)
			if len(entries) != 1 {
				t.Fatalf("expected one startup fleet observation: %+v", entries)
			}
			fields := entries[0].ContextMap()
			if fields["mode"] != mode {
				t.Fatalf("startup mode = %v, want %s", fields["mode"], mode)
			}
			for key, want := range map[string]int64{
				"ships": 5, "sailing": 1, "in_port": 2,
				"sunk": 1, "other": 1, "holding_reasons": 2,
			} {
				if fields[key] != want {
					t.Fatalf("startup %s = %v, want %d", key, fields[key], want)
				}
			}
			later := at.Add(2 * time.Minute)
			operationObservedAt(runner, later)
			runner.logStatus(h.ctx, later)
			if entries := statusEntries(t, logs); len(entries) != 1 {
				t.Fatalf("unchanged fleet must not restart periodic logging: %+v", entries)
			}
		})
	}

}

func TestStatusRedactsBeforeCappingHoldingReasons(t *testing.T) {
	t.Parallel()
	h := newRecoveryHarness(t)
	crossing := strings.Repeat("a", 500) + primaryTestKey + strings.Repeat("b", 30)
	scrubbed := strings.ReplaceAll(crossing, primaryTestKey, "[REDACTED]")
	for _, test := range []struct {
		name      string
		reason    string
		want      string
		truncated bool
	}{
		{
			name: "credential crossing the cap", reason: crossing,
			want: string([]rune(scrubbed)[:512]), truncated: true,
		},
		{
			name: "multibyte boundary", reason: strings.Repeat("é", 600),
			want: strings.Repeat("é", 512), truncated: true,
		},
		{name: "complete short reason", reason: "short waiting reason", want: "short waiting reason"},
	} {
		t.Run(test.name, func(t *testing.T) {
			runner, logs := observedStatusRunner(h)
			runner.holding = []currentHold{{ShipID: recoveryShipLapras, Reason: test.reason}}
			runner.logStatus(h.ctx, time.Now().UTC())
			entries := statusEntries(t, logs)
			if len(entries) != 1 {
				t.Fatalf("expected one coherent startup entry: %+v", entries)
			}
			fields := entries[0].ContextMap()
			reason, ok := fields["holding_reason"].(string)
			if !ok || reason != test.want {
				t.Fatalf("scrubbed reason = %q, want %q", reason, test.want)
			}
			if !utf8.ValidString(reason) || strings.Contains(reason, primaryTestKey[:8]) {
				t.Fatal("startup reason split a rune or leaked a credential prefix")
			}
			if truncated, _ := fields["holding_reason_truncated"].(bool); truncated != test.truncated {
				t.Fatalf("truncation = %t, want %t", truncated, test.truncated)
			}
		})
	}

}

func TestStatusFailsClosedWhenCredentialsUnreadable(t *testing.T) {
	t.Parallel()
	h := newRecoveryHarness(t)
	runner, logs := observedStatusRunner(h)
	runner.holding = []currentHold{{Reason: "waiting near " + primaryTestKey}}
	if err := h.db.Close(); err != nil {
		t.Fatal(err)
	}
	runner.logStatus(h.ctx, time.Now().UTC())
	entries := statusEntries(t, logs)
	if len(entries) != 1 {
		t.Fatalf("unreadable credentials must redact the startup reason: %+v", entries)
	}
	if reason := entries[0].ContextMap()["holding_reason"]; reason != "[REDACTED]" {
		t.Fatalf("holding_reason = %v, want fail-closed redaction", reason)
	}

}

func TestShipTransitionRedactsActorContext(t *testing.T) {
	t.Parallel()
	h := newRecoveryHarness(t)
	runner, logs := observedStatusRunner(h)
	ship := ShipState{}
	ship.Ship.ID = recoveryShipLapras
	ship.Ship.Name = primaryTestKey
	ship.Ship.Status = "sailing"
	runner.stateCache.Ships = []ShipState{ship}
	runner.logStatus(h.ctx, time.Now().UTC())
	ship.Ship.Status = "berthed"
	ship.Ship.CurrentPortID = recoveryHomePort
	runner.stateCache.Ships = []ShipState{ship}
	runner.logStatus(h.ctx, time.Now().UTC())
	entries := logs.FilterMessage("ship arrived").All()
	if len(entries) != 1 || entries[0].ContextMap()["ship"] != "[REDACTED]" {
		t.Fatalf("arrival actor context was not redacted: %+v", entries)
	}

}

func TestCompanyControlHoldDoesNotInventAMissingShip(t *testing.T) {
	t.Parallel()
	h := newRecoveryHarness(t)
	runner, _ := observedStatusRunner(h)
	changed, err := runner.considerUnderwriting(h.ctx)
	if err != nil || changed {
		t.Fatalf("missing underwriting observations must defer admission: changed=%t, error=%v", changed, err)
	}
	if len(runner.holding) != 1 || runner.holding[0].ShipID != "" {
		t.Fatalf("underwriting admission must publish a company notice, not a phantom ship: %+v", runner.holding)
	}
	if err := runner.hold(h.ctx, recoveryShipLapras, "waiting for authoritative ship state"); err != nil {
		t.Fatal(err)
	}
	if len(runner.holding) != 2 || runner.holding[1].ShipID != recoveryShipLapras {
		t.Fatalf("a real unobserved ship must retain its own hold: %+v", runner.holding)
	}

}

func joinedHoldReasons(holds []currentHold) string {
	var result strings.Builder
	for i, hold := range holds {
		if i > 0 {
			result.WriteString("; ")
		}
		result.WriteString(hold.Reason)
	}
	return result.String()
}
