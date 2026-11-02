package engine

import (
	"encoding/json"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/DevYukine/go-tradewinds/internal/store"
)

func capacityHazardDecisionCount(t *testing.T, h *recoveryHarness, portID, hullID string) int {
	t.Helper()
	rows, err := h.db.History(h.ctx, h.account.ID, "decisions", store.MaxHistoryLimit)
	if err != nil {
		t.Fatal(err)
	}
	count := 0
	for _, row := range rows.([]store.Decision) {
		if strings.HasPrefix(row.Kind, "capacity_") && strings.HasSuffix(row.Kind, ":"+portID+"|"+hullID) {
			count++
		}
	}
	return count
}

func TestCapacityHazardBackoffPersistsWithoutCandidateOrPreflightSpam(t *testing.T) {
	for _, scale := range []int64{30, 60, 120} {
		t.Run(fmt.Sprintf("scale=%d", scale), func(t *testing.T) {
			h, runner, yard, hull, quote := overseasCapacityFixture(t)
			runner.stateCache.Clock.Calendar.TimeScale = scale
			runner.stateCache.Factors = append(runner.stateCache.Factors, yard.ID)
			evidence, input := overseasCapacityEvidence(t, h, runner, time.Now().UTC())
			before := time.Now().UTC()
			decision, _, err := runner.capacityAtYard(h.ctx, yard, hull, quote, input, &evidence)
			if err != nil || decision.Accepted || decision.Reason != hazardUnavailable {
				t.Fatalf("missing hazard control: %+v, %v", decision, err)
			}
			after := time.Now().UTC()
			key := yard.ID + "|" + hull.ID
			persisted, err := h.db.Snapshot(h.ctx, h.account.ID)
			if err != nil {
				t.Fatal(err)
			}
			rejection := decode[map[string]capacityHazardRejection](persisted, "_growth_hazard_backoffs")[key]
			delay := time.Duration(6 * int64(time.Hour) / scale)
			if rejection.RetryAt.Before(before.Add(delay)) || rejection.RetryAt.After(after.Add(delay)) {
				t.Fatalf("hazard retry is not six game hours at scale %d: %s", scale, rejection.RetryAt)
			}
			if rejection.Decision.Accepted || rejection.Decision.Reason != decision.Reason ||
				!decode[map[string]time.Time](persisted, "_growth_build_backoffs")[key].IsZero() {
				t.Fatalf("hazard retry lost its rejection or altered yard refusal backoff: %+v", rejection)
			}
			count := capacityHazardDecisionCount(t, h, yard.ID, hull.ID)
			if count != 1 {
				t.Fatalf("initial hazard rejection recorded %d decisions, want one candidate decision", count)
			}

			restarted := h.runner(primaryTestKey)
			restarted.stateCache, restarted.meta, restarted.generation = runner.stateCache, runner.meta, runner.generation
			data := missionData{
				BuildPort: yard.ID, BuildClass: hull.ID, TargetCannons: 4,
				Capacity: &capacityIntent{BuildPort: yard.ID, BuildClass: hull.ID, QuoteLimitPence: 1_000_000, AdmittedAt: before},
			}
			for range 3 {
				clear(restarted.decisionHashes)
				got, _, err := restarted.capacityAtYard(h.ctx, yard, hull, quote, input, &evidence)
				if err != nil || got.Accepted || got.Reason != decision.Reason {
					t.Fatalf("restarted candidate bypassed hazard retry: %+v, %v", got, err)
				}
				if err := restarted.capacityBuildReady(h.ctx, &data); err == nil || err.Error() != decision.Reason {
					t.Fatalf("first-send preflight bypassed persisted hazard retry: %v", err)
				}
				if changed, err := restarted.considerCapacityGrowthAt(h.ctx, &evidence, yard.ID, hull.ID); err != nil || changed {
					t.Fatalf("candidate loop bypassed persisted hazard retry: changed=%t err=%v", changed, err)
				}
			}
			if got := capacityHazardDecisionCount(t, h, yard.ID, hull.ID); got != count {
				t.Fatalf("candidate or first-send preflight spammed decisions across restart: before=%d after=%d", count, got)
			}
			persisted, err = h.db.Snapshot(h.ctx, h.account.ID)
			if err != nil {
				t.Fatal(err)
			}
			if got := decode[map[string]capacityHazardRejection](persisted, "_growth_hazard_backoffs")[key]; !got.RetryAt.Equal(rejection.RetryAt) {
				t.Fatalf("a cached rejection extended the retry deadline: before=%s after=%s", rejection.RetryAt, got.RetryAt)
			}
			if h.api.mutationCount("/v1/shipyard/build-ship") != 0 {
				t.Fatal("hazard-backoff evaluation sent a build")
			}
		})
	}
}

func TestCapacityHazardPreflightRetriesFreshQuoteWithoutAcceptingStaleQuote(t *testing.T) {
	h, runner, yard, hull, _ := overseasCapacityFixture(t)
	runner.stateCache.Factors = append(runner.stateCache.Factors, yard.ID)
	data := missionData{
		BuildPort: yard.ID, BuildClass: hull.ID, TargetCannons: 4,
		Capacity: &capacityIntent{BuildPort: yard.ID, BuildClass: hull.ID, QuoteLimitPence: 1_000_000, AdmittedAt: time.Now().UTC()},
	}
	if err := runner.capacityBuildReady(h.ctx, &data); err == nil || err.Error() != hazardUnavailable {
		t.Fatalf("first-send preflight did not reject missing hazard evidence: %v", err)
	}
	key := yard.ID + "|" + hull.ID
	initial := decode[map[string]capacityHazardRejection](runner.snapshot, "_growth_hazard_backoffs")[key]
	if initial.RetryAt.IsZero() {
		t.Fatal("first-send rejection did not persist its retry")
	}
	expire := func() {
		t.Helper()
		backoffs := decode[map[string]capacityHazardRejection](runner.snapshot, "_growth_hazard_backoffs")
		rejection := backoffs[key]
		rejection.RetryAt = time.Now().UTC().Add(-time.Second)
		backoffs[key] = rejection
		runner.snapshot["_growth_hazard_backoffs"] = encode(backoffs)
		if err := h.db.SaveSnapshot(h.ctx, h.account.ID, runner.snapshot); err != nil {
			t.Fatal(err)
		}
	}
	owner := runner.stateCache.Ships[0].Ship
	quoteKey := hazardHullKey(owner.ID, yard.ID, "new_york")
	quoted := HazardQuote{
		ShipID: owner.ID, Origin: yard.ID, Destination: "new_york", ShipClass: hull.ID,
		CannonsCount: owner.CannonsCount, Month: runner.stateCache.Clock.Calendar.Month,
		ExpectedLossBPS: 2, ObservedAt: time.Now().UTC().Add(-7 * time.Minute),
		NetPiracyBPS: map[string]int64{"america-water": 0},
	}
	runner.stateCache.HazardQuotes[quoteKey] = quoted
	expire()
	if err := runner.capacityBuildReady(h.ctx, &data); err == nil || err.Error() != hazardUnavailable {
		t.Fatalf("expired retry accepted a stale Lloyd's quote: %v", err)
	}
	if next := decode[map[string]capacityHazardRejection](runner.snapshot, "_growth_hazard_backoffs")[key]; !next.RetryAt.After(time.Now().UTC()) {
		t.Fatal("unsuccessful hazard retry did not establish a new cadence")
	}
	quoted.ObservedAt = time.Now().UTC()
	runner.stateCache.HazardQuotes[quoteKey] = quoted
	expire()
	if err := runner.capacityBuildReady(h.ctx, &data); err != nil {
		t.Fatalf("retry did not resume with a fresh eligible Lloyd's bound: %v", err)
	}
	if h.api.mutationCount("/v1/shipyard/build-ship") != 0 {
		t.Fatal("hazard preflight sent a build instead of only validating it")
	}
}

func TestCapacityHazardEstimateBackoffRequiresFreshAuthority(t *testing.T) {
	for _, dataset := range []string{"catalog", "zones", "clock", "routes/boston|new_york|hoy"} {
		t.Run(dataset, func(t *testing.T) {
			h, runner, yard, hull, quote := overseasCapacityFixture(t)
			at := time.Now().UTC()
			overseasMetric(t, h, 10_000_000, at)
			evidence, input := overseasCapacityEvidence(t, h, runner, at)
			admitted, _, err := runner.capacityAtYard(h.ctx, yard, hull, quote, input, &evidence)
			if err != nil || !admitted.Accepted || !strings.HasPrefix(admitted.HazardSource, "zone estimate") {
				t.Fatalf("estimated financed admission control: %+v, %v", admitted, err)
			}
			data := missionData{
				BuildPort: yard.ID, BuildClass: hull.ID, TargetCannons: 4,
				Capacity: &capacityIntent{
					BuildPort: yard.ID, BuildClass: hull.ID, QuoteLimitPence: 1_000_000, AdmittedAt: at,
					Financing: &capacityFinancing{Decision: admitted},
				},
			}
			current := runner.meta[dataset]
			stale := current
			stale.Stale = true
			runner.meta[dataset] = stale
			if err := runner.capacityZoneEstimateReady(h.ctx, &data, admitted); err == nil || !strings.Contains(err.Error(), "authority") {
				t.Fatalf("stale %s did not fail closed on capture authority: %v", dataset, err)
			}
			key := yard.ID + "|" + hull.ID
			persisted, err := h.db.Snapshot(h.ctx, h.account.ID)
			if err != nil {
				t.Fatal(err)
			}
			for _, snapshot := range []map[string]json.RawMessage{runner.snapshot, persisted} {
				if rejection := decode[map[string]capacityHazardRejection](snapshot, "_growth_hazard_backoffs")[key]; !rejection.RetryAt.IsZero() {
					t.Fatalf("transient %s staleness created a hazard retry delay: %+v", dataset, rejection)
				}
			}
			runner.meta[dataset] = current
			if err := runner.capacityZoneEstimateReady(h.ctx, &data, admitted); err != nil {
				t.Fatalf("restored %s did not immediately release financed hazard revalidation: %v", dataset, err)
			}

			zones := decode[[]ZoneObservation](runner.snapshot, "zones")
			for index := range zones {
				if zones[index].SeaZoneID == yard.SeaZoneID {
					zones[index].NetPiracyBPS = -1
				}
			}
			runner.snapshot["zones"] = encode(zones)
			if err := runner.capacityZoneEstimateReady(h.ctx, &data, admitted); err == nil || err.Error() != hazardUnavailable {
				t.Fatalf("fresh invalid risk was not rejected as unavailable hazard: %v", err)
			}
			persisted, err = h.db.Snapshot(h.ctx, h.account.ID)
			if err != nil {
				t.Fatal(err)
			}
			rejection := decode[map[string]capacityHazardRejection](persisted, "_growth_hazard_backoffs")[key]
			if !rejection.RetryAt.After(time.Now().UTC()) || rejection.Decision.Accepted {
				t.Fatalf("fresh invalid risk lost its durable hazard retry: %+v", rejection)
			}
		})
	}
}

func TestCapacityHazardBackoffCannotExhaustOwnedOverseasRegion(t *testing.T) {
	h, runner, yard, hull, quote := overseasCapacityFixture(t)
	runner.stateCache.Factors = append(runner.stateCache.Factors, yard.ID, "new_york")
	ship := &runner.stateCache.Ships[0]
	ship.Ship.CurrentPortID, ship.Ship.Status = yard.ID, "berthed"
	ship.Voyage, ship.RouteLegs = nil, nil
	at := time.Now().UTC()
	overseasMetric(t, h, 10_000_000, at)
	evidence, input := overseasCapacityEvidence(t, h, runner, at)
	decision, _, err := runner.capacityAtYard(h.ctx, yard, hull, quote, input, &evidence)
	if err != nil || decision.Accepted || decision.Reason != hazardUnavailable {
		t.Fatalf("owned-region hazard gap became a measured negative: %+v %v", decision, err)
	}

	// A persisted rejection from before the fix can carry the generic
	// no-spare-work reason. Its active hazard retry still means unevaluated.
	key := yard.ID + "|" + hull.ID
	backoffs := decode[map[string]capacityHazardRejection](runner.snapshot, "_growth_hazard_backoffs")
	rejection := backoffs[key]
	rejection.Decision.Reason = capacityNoSpareWorkReason
	backoffs[key] = rejection
	runner.snapshot["_growth_hazard_backoffs"] = encode(backoffs)
	if err := h.db.SaveSnapshot(h.ctx, h.account.ID, runner.snapshot); err != nil {
		t.Fatal(err)
	}
	restarted := h.runner(primaryTestKey)
	restarted.stateCache, restarted.meta, restarted.generation = runner.stateCache, runner.meta, runner.generation
	for range 2 {
		if changed, err := restarted.considerCapacityGrowth(h.ctx, &evidence); err != nil || changed {
			t.Fatalf("transient hazard gap admitted growth: changed=%t error=%v", changed, err)
		}
		outcome := decode[map[string]expansionOutcome](restarted.snapshot, "_expansion_agent_outcomes")[yard.BankingRegion]
		if !outcome.CapacityRetryAt.IsZero() || outcome.CapacityReason != "" {
			t.Fatalf("hazard retry marked the overseas yard exhausted: %+v", outcome)
		}
		if exhausted, reason, _ := restarted.overseasGrowthStatus(); exhausted {
			t.Fatalf("transient Lloyd's gap released Europe: %s", reason)
		}
	}
	h.missingMission("__growth")
}
