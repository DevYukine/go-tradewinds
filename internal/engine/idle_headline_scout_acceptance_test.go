package engine

// Headline coverage uses the existing Runner.work, PlanVoyages, planSourcingRelocation and
// status seams unchanged; no new headline-specific production signature.

import (
	"regexp"
	"strings"
	"testing"
	"time"

	"go.uber.org/zap"
	"go.uber.org/zap/zaptest/observer"
)

func TestIdleHeadlineScoutPreservesExactOrdinaryAndSourcingReasons(t *testing.T) {
	h := newRecoveryHarness(t)
	h.api.setFleet([]map[string]any{sourcingReviewShipRow()})
	runner := refreshedRunner(t, h)
	runner.opts.Execute = false
	sourcingReviewInject(t, runner, []Contract{sourcingReviewCharter("combined-headline")})
	// A history admission veto is shared by charter and scout policy, so the
	// exact sourcing-only suffix remains independent of scout eligibility.
	runner.sourcingHistory.Error = "sourcing history unavailable: acceptance fixture"
	ship := harnessShip(t, runner, recoveryShipBlastoise)
	input, err := runner.committedPlanningInput(h.ctx, ship)
	if err != nil {
		t.Fatal(err)
	}
	ordinary := PlanVoyages(input)
	if len(ordinary) == 0 || ordinary[0].Accepted {
		t.Fatalf("headline fixture needs an evaluated ordinary rejection: %+v", ordinary)
	}
	input, err = runner.sourcingInput(h.ctx, ship)
	if err != nil {
		t.Fatal(err)
	}
	candidates := planSourcingRelocation(input)
	if len(candidates) == 0 || candidates[0].Accepted {
		t.Fatalf("headline fixture needs a sourcing admission rejection: %+v", candidates)
	}
	want := "no profitable executable voyage from laport; best evaluated alternative " + ordinary[0].Commodity + " to " + ordinary[0].Destination + ": " + ordinary[0].Reason + "; sourcing: " + candidates[0].Reason
	for pass := range 3 {
		runner.holding = nil
		clear(runner.assessedHoldIDs)
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
			t.Fatalf("pass %d: want exactly %q, got %+v", pass, want, reasons)
		}
		if strings.Contains(reasons[0], "expected cash net") || regexp.MustCompile(`\d+t `).MatchString(reasons[0]) {
			t.Fatalf("combined headline leaked quantity or estimated economics: %q", reasons[0])
		}
		if holds := idleHeadlineDecisions(t, h); len(holds) != 1 || holds[0].Reason != want {
			t.Fatalf("pass %d duplicated or changed the headline decision: %+v", pass, holds)
		}
		if details, summaries := sourcingReviewCounts(t, h); details != 1 || summaries != 1 {
			t.Fatalf("pass %d duplicated sourcing diagnostics: details=%d summaries=%d", pass, details, summaries)
		}
	}

	core, logs := observer.New(zap.DebugLevel)
	runner.log = zap.New(core)
	start := time.Now().UTC()
	for _, elapsed := range []time.Duration{0, 5*time.Minute - time.Second, 5 * time.Minute, 35*time.Minute - time.Second, 35 * time.Minute, 65 * time.Minute} {
		runner.logStatus(h.ctx, start.Add(elapsed))
		if elapsed < 5*time.Minute && logs.FilterMessage("work blocked").Len() != 0 {
			t.Fatal("combined headline announced a blocker before five real minutes")
		}
		if elapsed < 35*time.Minute && logs.FilterMessage("work still blocked").Len() != 0 {
			t.Fatal("combined headline repeated before the thirty-minute heartbeat")
		}
	}
	if status := runner.blockingStatuses[ship.Ship.ID]; status.reasonClass != "no profitable executable voyage from laport" {
		t.Fatalf("headline detail changed the stable status class: %+v", status)
	}
	if logs.FilterMessage("work blocked").Len() != 1 || logs.FilterMessage("work still blocked").Len() != 2 {
		t.Fatalf("combined headline changed blocker or heartbeat frequency: %+v", logs.All())
	}
}
