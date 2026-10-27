package engine

import (
	"strings"
	"testing"
	"time"

	"github.com/DevYukine/go-tradewinds/internal/store"
	"go.uber.org/zap"
)

func TestHoldStatusTransitionsAndBoundedHeartbeat(t *testing.T) {
	t.Parallel()
	h := newRecoveryHarness(t)
	runner, logs := observedStatusRunner(h)
	runner.stateCache.Ships = []ShipState{
		operationShip(recoveryShipLapras, "Lapras", "berthed", recoveryHomePort),
	}
	start := time.Now().UTC()
	cycle := func(at time.Time, assessed bool, reasons ...string) {
		t.Helper()
		runner.holding = runner.holding[:0]
		clear(runner.assessedHoldIDs)
		runner.assessHold("")
		if assessed {
			runner.assessShip(recoveryShipLapras)
		}
		for _, reason := range reasons {
			if err := runner.hold(h.ctx, recoveryShipLapras, reason); err != nil {
				t.Fatal(err)
			}
		}
		runner.logStatus(h.ctx, at)
	}
	cycle(start, true, "no profitable executable voyage (best net -100p)")
	cycle(start.Add(15*time.Second), true, "no profitable executable voyage (best net -101p)")
	if logs.FilterMessage("work blocked").Len() != 0 {
		t.Fatal("a transient blocker must stay quiet")
	}
	cycle(start.Add(5*time.Minute), true, "no profitable executable voyage (best net -102p)")
	cycle(start.Add(35*time.Minute-time.Second), true, "no profitable executable voyage (best net -103p)")
	if logs.FilterMessage("work blocked").Len() != 1 || logs.FilterMessage("work still blocked").Len() != 0 {
		t.Fatal("an unchanged blocker must not repeat before its heartbeat")
	}
	cycle(start.Add(35*time.Minute), true, "no profitable executable voyage (best net -104p)")
	cycle(start.Add(35*time.Minute+15*time.Second), true, "no profitable executable voyage (best net -105p)")
	cycle(start.Add(65*time.Minute), true, "no profitable executable voyage (best net -106p)")
	if entries := logs.FilterMessage("work still blocked").All(); len(entries) != 2 {
		t.Fatalf("expected one heartbeat per 30 real minutes, got %+v", entries)
	} else {
		fields := entries[0].ContextMap()
		if fields["ship"] != "Lapras" || fields["port_id"] != recoveryHomePort || fields["reason"] != "no profitable executable voyage (best net -104p)" || fields["blocked_since"] != start {
			t.Fatalf("heartbeat lost blocking context: %+v", fields)
		}
	}
	cycle(start.Add(66*time.Minute), true, "turnaround lock")
	cycle(start.Add(71*time.Minute), true, "turnaround lock")
	entries := logs.FilterMessage("work blocked").All()
	if len(entries) != 2 || entries[1].ContextMap()["reason"] != "turnaround lock" || entries[1].ContextMap()["blocked_since"] != start {
		t.Fatalf("a reason transition must log once without restarting the blocked interval: %+v", entries)
	}
	cycle(start.Add(72*time.Minute), false)
	if logs.FilterMessage("work unblocked").Len() != 0 {
		t.Fatal("an early work return must not clear an unassessed ship")
	}
	clearedAt := start.Add(72*time.Minute + 15*time.Second)
	cycle(clearedAt, true)
	cycle(clearedAt.Add(15*time.Second), true)
	cleared := logs.FilterMessage("work unblocked").All()
	if len(cleared) != 1 || cleared[0].ContextMap()["blocked_for"] != clearedAt.Sub(start) {
		t.Fatalf("clear must log the complete real blocked duration once: %+v", cleared)
	}
	cycle(start.Add(73*time.Minute), true, "turnaround lock")
	cycle(start.Add(78*time.Minute), true, "turnaround lock")
	entries = logs.FilterMessage("work blocked").All()
	if len(entries) != 3 || entries[2].ContextMap()["blocked_since"] != start.Add(73*time.Minute) {
		t.Fatalf("a new blocked interval must start after a clear: %+v", entries)
	}
	for _, message := range []string{"work blocked", "work still blocked", "work unblocked"} {
		for _, entry := range logs.FilterMessage(message).All() {
			if entry.Level != zap.InfoLevel {
				t.Fatalf("%s must be INFO, got %s", message, entry.Level)
			}
		}
	}
}

func TestHoldStatusKeepsControlReasonsDistinctAndCoalescesAlternatives(t *testing.T) {
	t.Parallel()
	h := newRecoveryHarness(t)
	runner, logs := observedStatusRunner(h)
	at := time.Now().UTC()
	for _, id := range []string{"__growth", "__treasury"} {
		runner.assessHold(id)
		if err := runner.hold(h.ctx, "", "first alternative"); err != nil {
			t.Fatal(err)
		}
		if err := runner.hold(h.ctx, "", id+" final blocker"); err != nil {
			t.Fatal(err)
		}
	}
	runner.logStatus(h.ctx, at)
	runner.logStatus(h.ctx, at.Add(15*time.Second))
	runner.logStatus(h.ctx, at.Add(5*time.Minute))
	entries := logs.FilterMessage("work blocked").All()
	if len(entries) != 2 {
		t.Fatalf("expected one final blocker for each control, not each alternative: %+v", entries)
	}
	for _, entry := range entries {
		fields := entry.ContextMap()
		if fields["reason"] != fields["ship_id"].(string)+" final blocker" || fields["ship"] != fields["ship_id"] {
			t.Fatalf("control ownership was lost: %+v", fields)
		}
	}
	runner.holding = nil
	clear(runner.assessedHoldIDs)
	runner.assessHold("__growth")
	runner.logStatus(h.ctx, at.Add(6*time.Minute))
	cleared := logs.FilterMessage("work unblocked").All()
	if len(cleared) != 1 || cleared[0].ContextMap()["ship_id"] != "__growth" {
		t.Fatalf("only the assessed control should clear: %+v", cleared)
	}
	runner.logStatus(h.ctx, at.Add(35*time.Minute))
	heartbeats := logs.FilterMessage("work still blocked").All()
	if len(heartbeats) != 1 || heartbeats[0].ContextMap()["ship_id"] != "__treasury" {
		t.Fatalf("unassessed control must retain its bounded heartbeat: %+v", heartbeats)
	}
}

func TestHoldStatusScrubsEveryUpstreamField(t *testing.T) {
	t.Parallel()
	for _, unavailable := range []bool{false, true} {
		h := newRecoveryHarness(t)
		runner, logs := observedStatusRunner(h)
		runner.stateCache.Ships = []ShipState{
			operationShip(recoveryShipLapras, primaryTestKey, "berthed", primaryTestKey),
		}
		if err := runner.hold(h.ctx, recoveryShipLapras, "waiting for "+primaryTestKey); err != nil {
			t.Fatal(err)
		}
		if unavailable {
			if err := h.db.Close(); err != nil {
				t.Fatal(err)
			}
		}
		at := time.Now().UTC()
		runner.logStatus(h.ctx, at)
		runner.logStatus(h.ctx, at.Add(5*time.Minute))
		runner.logStatus(h.ctx, at.Add(35*time.Minute))
		runner.holding = nil
		runner.assessShip(recoveryShipLapras)
		runner.logStatus(h.ctx, at.Add(36*time.Minute))
		for _, message := range []string{"work blocked", "work still blocked", "work unblocked"} {
			entries := logs.FilterMessage(message).All()
			if len(entries) != 1 {
				t.Fatalf("expected a redacted %s event: %+v", message, entries)
			}
			for _, key := range []string{"ship_id", "ship", "port_id", "reason"} {
				value := entries[0].ContextMap()[key].(string)
				if strings.Contains(value, primaryTestKey) || unavailable && value != "[REDACTED]" {
					t.Fatalf("%s %s was not safely scrubbed", message, key)
				}
			}
		}
	}
}

func TestHoldStatusSuppressesTransientClassesAndDroppedShips(t *testing.T) {
	t.Parallel()
	h := newRecoveryHarness(t)
	runner, logs := observedStatusRunner(h)
	runner.stateCache.Ships = []ShipState{
		operationShip(recoveryShipLapras, "Lapras", "berthed", recoveryHomePort),
	}
	start := time.Now().UTC()
	observe := func(at time.Time, reason string) {
		t.Helper()
		runner.holding = nil
		clear(runner.assessedHoldIDs)
		runner.assessShip(recoveryShipLapras)
		if reason != "" {
			if err := runner.hold(h.ctx, recoveryShipLapras, reason); err != nil {
				t.Fatal(err)
			}
		}
		runner.logStatus(h.ctx, at)
	}
	observe(start, "financial event: refresh balances")
	observe(start.Add(time.Minute), "")
	observe(start.Add(2*time.Minute), "turnaround lock")
	observe(start.Add(6*time.Minute), "sailing: wait for arrival")
	observe(start.Add(10*time.Minute), "sailing: wait for arrival")
	if logs.FilterMessage("work blocked").Len() != 0 || logs.FilterMessage("work unblocked").Len() != 0 {
		t.Fatal("short holds and reason classes younger than five minutes must stay quiet")
	}
	observe(start.Add(11*time.Minute), "sailing: wait for arrival")
	if logs.FilterMessage("work blocked").Len() != 1 {
		t.Fatal("the stable reason must be announced at five real minutes")
	}
	runner.stateCache.Ships = nil
	runner.holding = nil
	clear(runner.assessedHoldIDs)
	// A stale or failed fleet fetch does not prove the ship was removed.
	runner.logStatus(h.ctx, start.Add(41*time.Minute))
	if logs.FilterMessage("work still blocked").Len() != 1 {
		t.Fatal("stale fleet data must retain the last known blocker")
	}
	operationObservedAt(runner, start.Add(42*time.Minute))
	runner.logStatus(h.ctx, start.Add(42*time.Minute))
	runner.logStatus(h.ctx, start.Add(72*time.Minute))
	if logs.FilterMessage("work still blocked").Len() != 1 || logs.FilterMessage("work unblocked").Len() != 0 {
		t.Fatal("a ship absent from a fresh fleet must stop heartbeats without inventing a clear")
	}
}

func TestHoldStatusVoyageArrivalWaitBecomesRealWhenOverdue(t *testing.T) {
	for _, tc := range []struct {
		name, phase string
		relocation  bool
	}{
		{"ordinary voyage", "sailing", false},
		{"return voyage", "return_sailing", false},
		{"sourcing voyage", "sailing", true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			h := newRecoveryHarness(t)
			runner, logs := observedStatusRunner(h)
			runner.opts.Execute = true
			start := time.Now().UTC()
			// A skewed host clock must not make an on-time voyage look overdue.
			serverNow := start.Add(-2 * time.Hour)
			ship := operationShip(recoveryShipLapras, "Lapras", "sailing", recoveryHomePort)
			ship.Voyage = &struct {
				EstimatedArrivalAt time.Time `json:"estimated_arrival_at"`
			}{EstimatedArrivalAt: serverNow.Add(time.Minute)}
			runner.stateCache.Ships = []ShipState{ship}
			mission := &store.Mission{AccountID: h.account.ID, ShipID: ship.Ship.ID, Phase: tc.phase, Generation: "arrival-status-test"}
			data := missionData{Plan: Plan{Origin: recoveryHomePort, Destination: recoveryFarPort}}
			if tc.relocation {
				data.Relocation = &relocationIntent{}
			}
			for _, elapsed := range []time.Duration{0, 5 * time.Minute, 7 * time.Minute, 12 * time.Minute, 42 * time.Minute} {
				runner.holding = nil
				clear(runner.assessedHoldIDs)
				runner.assessShip(ship.Ship.ID)
				captured := time.Now().UTC()
				runner.meta["clock"] = DatasetMeta{
					AttemptedAt: captured, UpdatedAt: captured,
					Generation: runner.generation, Complete: true,
				}
				runner.stateCache.Clock.RealUTC = serverNow.Add(elapsed)
				if elapsed >= 7*time.Minute {
					mission, data = h.mission(ship.Ship.ID)
					ship.Voyage.EstimatedArrivalAt = serverNow.Add(2 * time.Hour)
				}
				if changed, err := runner.driveMission(h.ctx, ship, mission, &data); changed || err != nil {
					t.Fatalf("arrival wait advanced without authoritative arrival: changed=%v err=%v", changed, err)
				}
				expected := elapsed < 7*time.Minute
				if len(runner.holding) != 1 || runner.holding[0].expected != expected {
					t.Fatalf("arrival classification at %s: expected=%v holds=%+v", elapsed, expected, runner.holding)
				}
				runner.logStatus(h.ctx, start.Add(elapsed))
				wantBlocked := 0
				if elapsed >= 12*time.Minute {
					wantBlocked = 1
				}
				if logs.FilterMessage("work blocked").Len() != wantBlocked {
					t.Fatalf("arrival wait logged before the grace and real-hold delays elapsed: %s", elapsed)
				}
			}
			blocked := logs.FilterMessage("work blocked").All()
			if blocked[0].ContextMap()["blocked_since"] != start.Add(7*time.Minute) ||
				logs.FilterMessage("work still blocked").Len() != 1 {
				t.Fatal("overdue voyage lost its real-hold interval or thirty-minute heartbeat")
			}
			if logs.FilterMessage("ship arrived").Len() != 0 {
				t.Fatal("an overdue ETA invented an authoritative arrival")
			}
		})
	}
}

func TestHoldStatusVoyageArrivalWithoutTimeAuthorityRemainsReal(t *testing.T) {
	for _, missing := range []string{"voyage", "ETA", "clock"} {
		t.Run(missing, func(t *testing.T) {
			h := newRecoveryHarness(t)
			runner, _ := observedStatusRunner(h)
			now := time.Now().UTC()
			runner.stateCache.Clock.RealUTC = now
			runner.meta["clock"] = DatasetMeta{
				AttemptedAt: now, UpdatedAt: now,
				Generation: runner.generation, Complete: true,
			}
			ship := operationShip(recoveryShipLapras, "Lapras", "sailing", recoveryHomePort)
			ship.Voyage = &struct {
				EstimatedArrivalAt time.Time `json:"estimated_arrival_at"`
			}{EstimatedArrivalAt: now.Add(time.Minute)}
			switch missing {
			case "voyage":
				ship.Voyage = nil
			case "ETA":
				ship.Voyage.EstimatedArrivalAt = time.Time{}
			case "clock":
				delete(runner.meta, "clock")
			}
			if err := runner.holdVoyageArrival(h.ctx, ship, &store.Mission{}, &missionData{}, "waiting for authoritative arrival"); err != nil {
				t.Fatal(err)
			}
			if len(runner.holding) != 1 || runner.holding[0].expected {
				t.Fatalf("missing %s must not hide a voyage indefinitely: %+v", missing, runner.holding)
			}
		})
	}
}
