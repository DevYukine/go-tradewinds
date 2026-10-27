package engine

import (
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/DevYukine/go-tradewinds/internal/store"
)

func requireReviewHold(t *testing.T, runner *Runner, reason string, expected bool) {
	t.Helper()
	for _, hold := range runner.holding {
		if strings.Contains(hold.Reason, reason) {
			if hold.expected != expected {
				t.Fatalf("hold %q expected=%t, want %t", hold.Reason, hold.expected, expected)
			}
			return
		}
	}
	t.Fatalf("no hold containing %q in %+v", reason, runner.holding)
}

func TestHoldReviewCapacitySavingsAndAdmissionAreExpected(t *testing.T) {
	t.Parallel()
	t.Run("capacity savings admission", func(t *testing.T) {
		h, runner, _, _ := capacitySavingsRunnerFixture(t, true)
		if _, err := runner.considerGrowth(h.ctx); err != nil {
			t.Fatal(err)
		}
		requireReviewHold(t, runner, capacitySavingsReason, true)
	})
	t.Run("no eligible hull", func(t *testing.T) {
		h, runner := capacityExecutionFixture(t, false)
		for i := range runner.stateCache.Catalog.ShipClasses {
			runner.stateCache.Catalog.ShipClasses[i].Phase = runner.stateCache.Clock.UnlockedPhase + 1
		}
		evidence := growthEvidence{workingByRegion: make(map[string]int64)}
		if reason, err := runner.growthCashFunding(h.ctx, &evidence); err != nil || reason != "" {
			t.Fatalf("cash authority: %q, %v", reason, err)
		}
		if _, err := runner.considerCapacityGrowthAt(h.ctx, &evidence, recoveryHomePort, ""); err != nil {
			t.Fatal(err)
		}
		requireReviewHold(t, runner, "no phase-unlocked trading hull", true)
	})
	t.Run("expansion respects savings", func(t *testing.T) {
		h := newRecoveryHarness(t)
		runner := h.runner(primaryTestKey)
		runner.snapshot["_capacity_savings_target"] = encode(capacitySavingsTarget{
			PortID: recoveryHomePort, HullID: recoveryHull, RequiredPence: 200_000,
			CapitalPence: 100_000, DepthNetPencePerGameDay: 10_000,
			ExpiresAt: time.Now().UTC().Add(time.Hour),
		})
		if _, err := runner.considerExpansionAgent(h.ctx); err != nil {
			t.Fatal(err)
		}
		requireReviewHold(t, runner, capacitySavingsReason, true)
	})
	t.Run("expansion awaits an observed quote", func(t *testing.T) {
		h := newRecoveryHarness(t)
		newExpansionFactorAPI(h)
		runner := growthWindowRunner(t, h)
		runner.stateCache.Quotes = nil
		if _, err := runner.considerExpansionAgent(h.ctx); err != nil {
			t.Fatal(err)
		}
		requireReviewHold(t, runner, "needs a previously observed regional hull quote", true)
	})
}

func TestHoldReviewCapacityIntegrityFailuresRemainReal(t *testing.T) {
	t.Parallel()
	for _, failure := range []string{"future settlement", "missing closeout hull", "zones", "profile", "negative vault", "missing home"} {
		t.Run(failure, func(t *testing.T) {
			h, runner := capacityExecutionFixture(t, false)
			reason := ""
			switch failure {
			case "future settlement":
				persistGrowthWindowSettlement(t, h, store.Settlement{
					ID: "hold-review-future", Kind: "spot", CostsKnown: true,
					ProceedsPence: 100, CompletedAt: time.Now().UTC().Add(time.Hour),
				})
				reason = "future-dated settlement evidence"
			case "missing closeout hull":
				persistGrowthWindowSettlement(t, h, store.Settlement{
					ID: "hold-review-missing-hull", ShipID: "missing-built-hull", Kind: "aborted",
					CostsKnown: false, CompletedAt: time.Now().UTC().Add(-time.Hour),
				}, growthSettlementAction{Path: "/v1/shipyard/build-ship", ShipID: "missing-built-hull"})
				reason = "surviving hull for an unknown-cost shipyard closeout"
			case "zones", "profile":
				meta := runner.meta[failure]
				meta.Stale = true
				runner.meta[failure] = meta
				reason = "current successful " + failure + " authority"
			case "negative vault":
				runner.stateCache.Vaults[0].BalancePence = -1
				reason = "any regional vault is negative"
			case "missing home":
				runner.stateCache.HomePortID = "missing-home"
				reason = "authoritative home billing region"
			}
			if failure == "negative vault" || failure == "missing home" {
				if _, err := runner.considerGrowth(h.ctx); err != nil {
					t.Fatal(err)
				}
			} else {
				evidence := growthEvidence{workingByRegion: make(map[string]int64)}
				if reason, err := runner.growthCashFunding(h.ctx, &evidence); err != nil || reason != "" {
					t.Fatalf("cash authority: %q, %v", reason, err)
				}
				if _, err := runner.considerCapacityGrowthAt(h.ctx, &evidence, "", ""); err != nil {
					t.Fatal(err)
				}
			}
			requireReviewHold(t, runner, reason, false)
		})
	}
}

func TestHoldReviewExpansionAndExecutionFailuresRemainReal(t *testing.T) {
	t.Parallel()
	t.Run("expansion authority", func(t *testing.T) {
		h := newRecoveryHarness(t)
		newExpansionFactorAPI(h)
		runner := growthWindowRunner(t, h)
		meta := runner.meta["profile"]
		meta.Stale = true
		runner.meta["profile"] = meta
		if _, err := runner.considerExpansionAgent(h.ctx); err != nil {
			t.Fatal(err)
		}
		requireReviewHold(t, runner, "current successful profile authority", false)
	})
	t.Run("unobserved built hull", func(t *testing.T) {
		h := newRecoveryHarness(t)
		runner := h.runner(primaryTestKey)
		mission := &store.Mission{AccountID: h.account.ID, ShipID: "__growth", Phase: "growth_refit", Generation: "hold-review-build"}
		data := missionData{BuiltShipID: "unobserved-built-hull"}
		if _, err := runner.driveGrowth(h.ctx, mission, &data); err != nil {
			t.Fatal(err)
		}
		requireReviewHold(t, runner, "build outcome not yet present", false)
	})
	t.Run("procurement recovery authority", func(t *testing.T) {
		h, runner, mission, data, ship := procurementRecoveryFixture(t, false)
		meta := runner.meta["books/"+recoveryHomePort]
		meta.Stale = true
		runner.meta["books/"+recoveryHomePort] = meta
		if _, err := runner.prepareDeparture(h.ctx, ship, mission, &data); err != nil {
			t.Fatal(err)
		}
		if len(runner.holding) == 0 {
			t.Fatal("stale procurement recovery authority did not hold")
		}
		for _, hold := range runner.holding {
			if hold.expected {
				t.Fatalf("procurement recovery execution stall was silenced: %+v", hold)
			}
		}
	})
}

func TestHoldReviewGrowthWaitEscalatesAcrossRestartAndResets(t *testing.T) {
	t.Parallel()
	h := newRecoveryHarness(t)
	runner, _ := observedStatusRunner(h)
	start := time.Now().UTC()
	target := capacitySavingsTarget{PortID: recoveryHomePort, HullID: recoveryHull, RequiredPence: 200_000, CapitalPence: 100_000}
	cycle := func(at time.Time) {
		t.Helper()
		runner.holding = nil
		clear(runner.assessedHoldIDs)
		runner.assessHold("__growth")
		if _, err := runner.holdCapacitySavings(h.ctx, target, GrowthDecision{}, 100_000, 1_000); err != nil {
			t.Fatal(err)
		}
		requireReviewHold(t, runner, capacitySavingsReason, true)
		runner.logStatus(h.ctx, at)
	}
	cycle(start)
	if err := h.db.SaveSnapshot(h.ctx, h.account.ID, runner.snapshot); err != nil {
		t.Fatal(err)
	}
	var logsEntries int
	runner, logs := observedStatusRunner(h)
	cycle(start.Add(65 * time.Minute))
	cycle(start.Add(6*time.Hour - time.Second))
	if logs.FilterMessage("work blocked").Len() != 0 {
		t.Fatal("economic waits became real before six continuous real hours")
	}
	cycle(start.Add(6 * time.Hour))
	cycle(start.Add(6*time.Hour + 5*time.Minute))
	cycle(start.Add(6*time.Hour + 35*time.Minute))
	if logs.FilterMessage("work blocked").Len() != 1 || logs.FilterMessage("work still blocked").Len() != 1 {
		t.Fatalf("overdue economic wait lacked bounded real-blocker logging: %+v", logs.All())
	}
	fields := logs.FilterMessage("work blocked").All()[0].ContextMap()
	if fields["blocked_since"] != start || fields["blocked_for"] != 6*time.Hour+5*time.Minute {
		t.Fatalf("overdue economic wait omitted its quiet interval: %+v", fields)
	}
	logsEntries = logs.FilterMessage("work blocked").Len()
	runner.holding = nil
	clear(runner.assessedHoldIDs)
	runner.assessHold("__growth")
	runner.logStatus(h.ctx, start.Add(7*time.Hour))
	if _, exists := runner.snapshot["_growth_wait_since"]; exists {
		t.Fatal("assessed progress did not clear the continuous growth-wait timer")
	}
	cycle(start.Add(8 * time.Hour))
	cycle(start.Add(9 * time.Hour))
	if logs.FilterMessage("work blocked").Len() != logsEntries || logs.FilterMessage("work unblocked").Len() != 1 {
		t.Fatal("a new economic wait inherited the prior overdue interval")
	}
	if since := decode[time.Time](runner.snapshot, "_growth_wait_since"); !since.Equal(start.Add(8 * time.Hour)) {
		t.Fatalf("new continuous wait started at %s, want %s", since, start.Add(8*time.Hour))
	}
}

func TestHoldReviewIdleHeadlineIgnoresChangingAlternativeDetails(t *testing.T) {
	t.Parallel()
	h := newRecoveryHarness(t)
	runner, logs := observedStatusRunner(h)
	runner.stateCache.Ships = []ShipState{operationShip(recoveryShipLapras, "Lapras", "berthed", recoveryHomePort)}
	start := time.Now().UTC()
	for i, elapsed := range []time.Duration{0, time.Minute, 5 * time.Minute, 35 * time.Minute} {
		runner.holding = nil
		clear(runner.assessedHoldIDs)
		runner.assessShip(recoveryShipLapras)
		reason := fmt.Sprintf("no profitable executable voyage from %s; best evaluated alternative commodity-%c to port-%c (net %dp); sourcing: route-%c is below zone-%c", recoveryHomePort, 'a'+i, 'a'+i, -100-i, 'a'+i, 'a'+i)
		if err := runner.hold(h.ctx, recoveryShipLapras, reason); err != nil {
			t.Fatal(err)
		}
		runner.logStatus(h.ctx, start.Add(elapsed))
	}
	if logs.FilterMessage("work blocked").Len() != 1 || logs.FilterMessage("work still blocked").Len() != 1 {
		t.Fatalf("changing alternatives reset the stable idle class: %+v", logs.All())
	}
	fields := logs.FilterMessage("work still blocked").All()[0].ContextMap()
	if fields["blocked_since"] != start || !strings.Contains(fields["reason"].(string), "route-d") {
		t.Fatalf("stable class lost the original interval or current detail: %+v", fields)
	}
}
