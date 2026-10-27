package engine

import (
	"strings"
	"testing"

	"github.com/DevYukine/go-tradewinds/internal/store"
)

func idleHeadlineDecisions(t *testing.T, h *recoveryHarness) []store.Decision {
	t.Helper()
	rows, err := h.db.History(h.ctx, h.account.ID, "decisions", 500)
	if err != nil {
		t.Fatal(err)
	}
	decisions, ok := rows.([]store.Decision)
	if !ok {
		t.Fatalf("unexpected decision history type %T", rows)
	}
	var holds []store.Decision
	for _, row := range decisions {
		if row.ShipID == recoveryShipBlastoise && row.Kind == "hold" {
			holds = append(holds, row)
		}
	}
	return holds
}

func TestIdleHeadlineCombinesOrdinaryAndSourcingRejectionOnceAcrossPasses(t *testing.T) {
	h := newRecoveryHarness(t)
	h.api.setFleet([]map[string]any{sourcingReviewShipRow()})
	runner := refreshedRunner(t, h)
	runner.opts.Execute = false
	sourcingReviewInject(t, runner, []Contract{sourcingReviewCharter("headline-charter")})
	key := routeKey("amsterdam", recoveryFarPort, recoveryHull)
	route := runner.stateCache.Routes[key]
	route.Legs[0].EdgeClass = "oceanic"
	runner.stateCache.Routes[key] = route
	ship := harnessShip(t, runner, recoveryShipBlastoise)
	input, err := runner.committedPlanningInput(h.ctx, ship)
	if err != nil {
		t.Fatal(err)
	}
	ordinary := PlanVoyages(input)
	if len(ordinary) == 0 || ordinary[0].Accepted {
		t.Fatalf("fixture must have evaluated but unprofitable ordinary plans: %+v", ordinary)
	}
	sourcing, err := runner.sourcingInput(h.ctx, ship)
	if err != nil {
		t.Fatal(err)
	}
	candidates := planSourcingRelocation(sourcing)
	if len(candidates) == 0 || candidates[0].Accepted || !strings.HasPrefix(candidates[0].Reason, "hull coastal is below route edge") {
		t.Fatalf("fixture must lead with an illegal oceanic sourcing leg: %+v", candidates)
	}
	var firstReason string
	for pass := range 3 {
		runner.holding = nil
		clear(runner.assessedHoldIDs)
		if _, err := runner.work(h.ctx); err != nil {
			t.Fatal(err)
		}
		var shipHolds []currentHold
		for _, hold := range runner.holding {
			if hold.ShipID == ship.Ship.ID {
				shipHolds = append(shipHolds, hold)
			}
		}
		if len(shipHolds) != 1 {
			t.Fatalf("pass %d has %d ship holds, want one combined headline: %+v", pass, len(shipHolds), shipHolds)
		}
		reason := shipHolds[0].Reason
		if !strings.HasPrefix(reason, "no profitable executable voyage from "+recoveryHomePort) || !strings.Contains(reason, "; sourcing: "+candidates[0].Reason) {
			t.Errorf("pass %d lost the ordinary headline or top sourcing rejection: %q", pass, reason)
		}
		if pass == 0 {
			firstReason = reason
		} else if reason != firstReason {
			t.Errorf("identical pass changed hold reason: %q -> %q", firstReason, reason)
		}
		holds := idleHeadlineDecisions(t, h)
		if len(holds) != 1 || holds[0].Reason != reason {
			t.Errorf("identical pass %d must retain exactly one hold decision: %+v", pass, holds)
		}
		details, summaries := sourcingReviewCounts(t, h)
		if details != 1 || summaries != 1 {
			t.Errorf("headline change altered or duplicated sourcing diagnostics: details=%d summaries=%d", details, summaries)
		}
	}
}

func TestIdleHeadlineKeepsAcceptedSourcingHoldsUnchanged(t *testing.T) {
	for _, scenario := range []string{"observe mode", "stale insurance", "conflicting live policies"} {
		t.Run(scenario, func(t *testing.T) {
			h := newRecoveryHarness(t)
			h.api.setFleet([]map[string]any{sourcingReviewShipRow()})
			runner := refreshedRunner(t, h)
			sourcingReviewInject(t, runner, []Contract{sourcingReviewCharter("accepted-headline-charter")})
			// No unrelated refit or offshore mutation may preempt this idle pass.
			runner.stateCache.Ships[0].Ship.Status = "anchored_roadstead"
			for i := range runner.stateCache.Catalog.Ports {
				runner.stateCache.Catalog.Ports[i].HasShipyard = false
			}
			ship := harnessShip(t, runner, recoveryShipBlastoise)
			want := "observe mode: funded sourcing inspection is not executed"
			if scenario == "observe mode" {
				runner.opts.Execute = false
			} else {
				policy := piracyInsurancePolicy()
				policy.OriginPortID, policy.DestPortID = recoveryHomePort, "amsterdam"
				second := policy
				second.ID, second.DestPortID = "bbbbbbbb-bbbb-4bbb-8bbb-bbbbbbbbbbbb", recoveryFarPort
				runner.stateCache.InsurancePolicies = []InsurancePolicy{policy, second}
				runner.meta["insurance"] = runner.meta["ships"]
				if scenario == "stale insurance" {
					stale := runner.meta["insurance"]
					stale.Stale = true
					runner.meta["insurance"] = stale
				}
				if err := runner.insuranceActionReady(ship, "depart", "amsterdam"); err != nil {
					want = err.Error()
				} else {
					t.Fatal("fixture did not block insured departure")
				}
			}
			input, err := runner.sourcingInput(h.ctx, ship)
			if err != nil {
				t.Fatal(err)
			}
			plans := planSourcingRelocation(input)
			if len(plans) == 0 || !plans[0].Accepted {
				t.Fatalf("fixture needs an accepted sourcing plan before the execution hold: %+v", plans)
			}
			if _, err := runner.work(h.ctx); err != nil {
				t.Fatal(err)
			}
			var reasons []string
			for _, hold := range runner.holding {
				if hold.ShipID == ship.Ship.ID {
					reasons = append(reasons, hold.Reason)
				}
			}
			if len(reasons) != 1 || reasons[0] != want {
				t.Fatalf("accepted sourcing hold was replaced or accompanied by an idle headline: want %q, got %+v", want, reasons)
			}
			if holds := idleHeadlineDecisions(t, h); len(holds) != 1 || holds[0].Reason != want {
				t.Fatalf("accepted sourcing hold decision changed: %+v", holds)
			}
			details, summaries := sourcingReviewCounts(t, h)
			if details != 1 || summaries != 1 {
				t.Fatalf("accepted sourcing diagnostics changed: details=%d summaries=%d", details, summaries)
			}
		})
	}
}
