package engine

import (
	"testing"
	"time"

	"github.com/DevYukine/go-tradewinds/internal/store"
)

func TestExpectedHoldStatusWaitsStaySilentForSixtyFiveMinutes(t *testing.T) {
	for _, tc := range []struct {
		name, subject, reason string
	}{
		{"ordinary voyage", recoveryShipLapras, "actual fleet confirms voyage in flight; waiting for authoritative arrival"},
		{"sourcing voyage", recoveryShipLapras, "sourcing relocation waits for authoritative source arrival"},
		{"capacity savings", "__growth", capacitySavingsReason},
		{"capacity admission", "__growth", "capacity growth waits for every unresolved account action"},
		{"expansion admission", "__growth", "expansion agent requires current successful vaults authority"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			h := newRecoveryHarness(t)
			runner, logs := observedStatusRunner(h)
			runner.stateCache.Ships = []ShipState{operationShip(recoveryShipLapras, "Lapras", "sailing", recoveryHomePort)}
			start := time.Now().UTC()
			for _, elapsed := range []time.Duration{0, 5 * time.Minute, 35 * time.Minute, 65 * time.Minute} {
				runner.holding = nil
				clear(runner.assessedHoldIDs)
				runner.assessHold(tc.subject)
				shipID := tc.subject
				if tc.subject == "__growth" {
					shipID = "" // Account-wide callers inherit the assessed control.
				}
				if err := runner.holdExpected(h.ctx, shipID, tc.reason); err != nil {
					t.Fatal(err)
				}
				runner.logStatus(h.ctx, start.Add(elapsed))
			}
			for _, message := range []string{"work blocked", "work still blocked", "work unblocked"} {
				if entries := logs.FilterMessage(message).All(); len(entries) != 0 {
					t.Errorf("expected wait emitted %s: %+v", message, entries)
				}
			}
		})
	}
}

func TestExpectedHoldStatusRealStallsKeepFiveMinuteDelayAndThirtyMinuteHeartbeat(t *testing.T) {
	for _, tc := range []struct {
		name, subject, reason string
	}{
		{"destination procurement", recoveryShipLapras, "destination procurement recovery waits for confirmed market acquisition"},
		{"sealed freight", recoveryShipLapras, "sealed freight cargo shortfall requires authoritative recovery"},
		{"commissioning", "__growth", "commissioning waits for authoritative built hull"},
		{"loans", "__loans", "loan repayment waits for confirmed funds"},
		{"no profitable voyage", recoveryShipLapras, "no profitable executable voyage from laport"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			h := newRecoveryHarness(t)
			runner, logs := observedStatusRunner(h)
			runner.stateCache.Ships = []ShipState{operationShip(recoveryShipLapras, "Lapras", "berthed", recoveryHomePort)}
			start := time.Now().UTC()
			observe := func(elapsed time.Duration) {
				t.Helper()
				runner.holding = nil
				clear(runner.assessedHoldIDs)
				runner.assessHold(tc.subject)
				if err := runner.hold(h.ctx, tc.subject, tc.reason); err != nil {
					t.Fatal(err)
				}
				runner.logStatus(h.ctx, start.Add(elapsed))
			}
			observe(0)
			observe(5*time.Minute - time.Second)
			if logs.FilterMessage("work blocked").Len() != 0 {
				t.Fatal("real stall announced before five minutes")
			}
			observe(5 * time.Minute)
			observe(35*time.Minute - time.Second)
			if logs.FilterMessage("work blocked").Len() != 1 || logs.FilterMessage("work still blocked").Len() != 0 {
				t.Fatal("real stall must announce once at five minutes, with no early heartbeat")
			}
			observe(35 * time.Minute)
			observe(65*time.Minute - time.Second)
			if logs.FilterMessage("work still blocked").Len() != 1 {
				t.Fatal("real stall heartbeat must be bounded to thirty minutes")
			}
			observe(65 * time.Minute)
			if logs.FilterMessage("work blocked").Len() != 1 || logs.FilterMessage("work still blocked").Len() != 2 {
				t.Fatal("real stall lost its announcement or thirty-minute heartbeats")
			}
			for _, entry := range logs.FilterMessage("work still blocked").All() {
				if fields := entry.ContextMap(); fields["blocked_since"] != start || fields["reason"] != tc.reason || fields["ship_id"] != tc.subject {
					t.Fatalf("real stall heartbeat lost its ownership or interval: %+v", fields)
				}
			}
		})
	}
}

func TestExpectedHoldStatusClearsAnnouncedStallOnceAndRestartsRealInterval(t *testing.T) {
	h := newRecoveryHarness(t)
	runner, logs := observedStatusRunner(h)
	runner.stateCache.Ships = []ShipState{operationShip(recoveryShipLapras, "Lapras", "berthed", recoveryHomePort)}
	start := time.Now().UTC()
	observe := func(elapsed time.Duration, expected bool) {
		t.Helper()
		runner.holding = nil
		clear(runner.assessedHoldIDs)
		runner.assessShip(recoveryShipLapras)
		var err error
		if expected {
			err = runner.holdExpected(h.ctx, recoveryShipLapras, "sourcing relocation waits for authoritative source arrival")
		} else {
			err = runner.hold(h.ctx, recoveryShipLapras, "no profitable executable voyage from laport")
		}
		if err != nil {
			t.Fatal(err)
		}
		runner.logStatus(h.ctx, start.Add(elapsed))
	}
	observe(0, false)
	observe(5*time.Minute, false)
	observe(6*time.Minute, true)
	observe(36*time.Minute, true)
	observe(71*time.Minute, true)
	cleared := logs.FilterMessage("work unblocked").All()
	if len(cleared) != 1 || cleared[0].ContextMap()["blocked_for"] != 6*time.Minute {
		t.Fatalf("expected wait must clear the announced real stall exactly once: %+v", cleared)
	}
	if logs.FilterMessage("work blocked").Len() != 1 || logs.FilterMessage("work still blocked").Len() != 0 {
		t.Fatal("expected wait was reclassified as a real stall or kept an old heartbeat alive")
	}
	observe(72*time.Minute, false)
	observe(77*time.Minute-time.Second, false)
	if logs.FilterMessage("work blocked").Len() != 1 {
		t.Fatal("new real stall reused the old blocked interval")
	}
	observe(77*time.Minute, false)
	entries := logs.FilterMessage("work blocked").All()
	if len(entries) != 2 || entries[1].ContextMap()["blocked_since"] != start.Add(72*time.Minute) {
		t.Fatalf("new real stall must start a new blocked_since: %+v", entries)
	}
}

func TestExpectedHoldStatusPersistsDashboardHoldsAndDecisionRows(t *testing.T) {
	h := newRecoveryHarness(t)
	runner, _ := observedStatusRunner(h)
	reason := "actual fleet confirms voyage in flight; waiting for authoritative arrival"
	if err := runner.holdExpected(h.ctx, recoveryShipLapras, reason); err != nil {
		t.Fatal(err)
	}
	runner.assessHold("__growth")
	if err := runner.holdExpected(h.ctx, "", capacitySavingsReason); err != nil {
		t.Fatal(err)
	}
	if len(runner.holding) != 2 || runner.holding[0].ShipID != recoveryShipLapras || runner.holding[1].ShipID != "" {
		t.Fatalf("expected holds lost their dashboard ownership: %+v", runner.holding)
	}
	// Use the same projection as Run: private status classification must not
	// remove either expected hold from the persisted dashboard document.
	runner.snapshot["_engine"] = encode(map[string]any{"holds": runner.holding})
	if err := h.db.SaveSnapshot(h.ctx, h.account.ID, runner.snapshot); err != nil {
		t.Fatal(err)
	}
	snapshot, err := h.db.Snapshot(h.ctx, h.account.ID)
	if err != nil {
		t.Fatal(err)
	}
	engine := decode[struct {
		Holds []struct {
			ShipID string `json:"ship_id"`
			Reason string `json:"reason"`
		} `json:"holds"`
	}](snapshot, "_engine")
	if len(engine.Holds) != 2 || engine.Holds[0].ShipID != recoveryShipLapras || engine.Holds[0].Reason != reason || engine.Holds[1].ShipID != "" || engine.Holds[1].Reason != capacitySavingsReason {
		t.Fatalf("persisted expected dashboard holds changed: %+v", engine.Holds)
	}
	rows, err := h.db.History(h.ctx, h.account.ID, "decisions", 100)
	if err != nil {
		t.Fatal(err)
	}
	found := make(map[string]int)
	for _, row := range rows.([]store.Decision) {
		if row.Kind == "hold" {
			found[row.ShipID+"|"+row.Reason]++
		}
	}
	if len(found) != 2 || found[recoveryShipLapras+"|"+reason] != 1 || found["|"+capacitySavingsReason] != 1 {
		t.Fatalf("expected waits must still write ordinary hold decision rows: %+v", found)
	}
}
