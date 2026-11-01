package engine

import (
	"testing"
	"time"
)

// Match public estimates to the executable source book. A storeless return
// makes the round-trip and outward store targets round to the same bought tons.
func overseasMatchedLotEvidence(t *testing.T, runner *Runner, yard, partner Port, hull Hull) {
	t.Helper()
	for _, portID := range []string{yard.ID, partner.ID} {
		market := runner.stateCache.Markets[portID]
		market.CustomsBPS = 0
		if portID == yard.ID {
			bookCommodity(market, recoveryGrain).Depth.Asks = []Level{
				{OrderID: "matched-grain-ask", PricePence: 100, RemainingQuantity: 10_000},
			}
			bookCommodity(market, "provisions").Depth.Asks = []Level{
				{OrderID: "matched-store-ask", PricePence: 50, RemainingQuantity: 10_000},
			}
		}
		runner.stateCache.Markets[portID] = market
	}
	backKey := routeKey(partner.ID, yard.ID, hull.ID)
	back := runner.stateCache.Routes[backKey]
	back.SafeProvisionsTons = 0
	runner.stateCache.Routes[backKey] = back
}

func TestOverseasCapacityGateAndAdmissionShareCandidateLotBuffer(t *testing.T) {
	t.Parallel()
	h, runner, yard, hull, quote := overseasCapacityFixture(t)
	partner, found := findPort(runner.stateCache.Catalog, "new_york")
	if !found {
		t.Fatal("overseas partner port missing from the capacity fixture")
	}
	overseasMatchedLotEvidence(t, runner, yard, partner, hull)
	at := time.Now().UTC()
	overseasMetric(t, h, 10_000_000, at)
	evidence, input := overseasCapacityEvidence(t, h, runner, at)
	input.SurplusPence, input.LotCashPence, input.FleetSize = 100_000_000, 100, 6

	gate6, _, err := runner.capacityAtYard(h.ctx, yard, hull, quote, input, &evidence)
	if err != nil || !gate6.Accepted {
		t.Fatalf("overseas gate control with fleet lot 100: %+v %v", gate6, err)
	}
	input.FleetSize = 1
	gate1, _, err := runner.capacityAtYard(h.ctx, yard, hull, quote, input, &evidence)
	if err != nil || !gate1.Accepted {
		t.Fatalf("overseas single-ship gate control: %+v %v", gate1, err)
	}
	input.FleetSize = 6

	admitted, _, _, err := runner.expansionBuildEstimate(h.ctx, yard, partner, input, at)
	if err != nil || !admitted.Accepted || admitted.ExpansionEstimate == nil {
		t.Fatalf("overseas admission control: %+v %v", admitted, err)
	}
	estimate := *admitted.ExpansionEstimate
	// Matching cost evidence, asserted penny for penny: the admission lot from
	// the gossip estimate (cargo purchase + cargo customs + bought store cash)
	// must equal the gate's real accepted-plan candidate lot.
	admissionLot := growthCashTotal(estimate.AskPence*estimate.CargoTons, estimate.CustomsPence, estimate.ProvisionsPence)
	floor := max(input.ReservePence, ceilBPS(*quote.TotalCostPence, 1_500))
	gateLot := gate6.WorkingCapitalPence - floor - 6*input.LotCashPence
	if gateLot != admissionLot {
		t.Fatalf("admission and gate candidate lots disagree under matching cost evidence: %dp vs %dp", admissionLot, gateLot)
	}
	if gateLot <= input.LotCashPence {
		t.Fatalf("overseas candidate lot %dp does not dominate the existing fleet median %dp", gateLot, input.LotCashPence)
	}
	// The ONE shared buffer rule, asserted directly on the real decisions.
	if gate6.WorkingCapitalPence != admitted.WorkingCapitalPence {
		t.Fatalf("admission and gate overseas working buffers disagree: %dp vs %dp",
			admitted.WorkingCapitalPence, gate6.WorkingCapitalPence)
	}
	// Fleet scaling buys incumbent lots at the median only; the candidate lot
	// is charged once and never multiplied across the fleet.
	if delta := gate6.WorkingCapitalPence - gate1.WorkingCapitalPence; delta != 5*input.LotCashPence {
		t.Fatalf("overseas gate lot was multiplied across the fleet: grew %dp for five hulls, want %dp", delta, 5*input.LotCashPence)
	}
}

func TestCapacityEuropeanGateKeepsFleetLotBuffer(t *testing.T) {
	t.Parallel()
	h, runner := capacityExecutionFixture(t, false)
	evidence := capacityMeasuredEvidence(t, h, runner, time.Now().UTC())
	input, reason, err := runner.capacityFleetInput(h.ctx, &evidence)
	if err != nil || reason != "" {
		t.Fatalf("European fleet input: %s %v", reason, err)
	}
	yard, _ := findPort(runner.stateCache.Catalog, recoveryHomePort)
	hull, _ := findHull(runner.stateCache.Catalog, recoveryHull)
	if yard.Phase >= 2 {
		t.Fatalf("European control is not a Phase 1 yard: %+v", yard)
	}
	quote, err := runner.growthQuote(yard, hull)
	if err != nil {
		t.Fatal(err)
	}
	planning, err := runner.committedPlanningInputExcept(h.ctx, growthCandidate(yard, hull), "__growth")
	if err != nil {
		t.Fatal(err)
	}
	planning.FleetDailyUpkeepPence = 0
	var candidateLot int64
	for _, plan := range PlanVoyages(planning) {
		if plan.Accepted && len(plan.FreightIDs) == 0 {
			candidateLot = max(candidateLot, growthCashTotal(plan.Economics.PurchasePence, plan.Economics.CustomsPence, plan.Economics.StoreCashPence))
		}
	}
	input.SurplusPence, input.LotCashPence, input.FleetSize = 100_000_000, 1_000_000, 5
	if candidateLot <= 0 || candidateLot >= input.LotCashPence {
		t.Fatalf("European candidate lot %dp must be cheaper than the fleet lot %dp", candidateLot, input.LotCashPence)
	}
	floor := max(input.ReservePence, ceilBPS(*quote.TotalCostPence, 1_500))
	want := floor + 6*input.LotCashPence
	gate, _, err := runner.capacityAtYard(h.ctx, yard, hull, quote, input, &evidence)
	if err != nil || !gate.Accepted || gate.WorkingCapitalPence != want {
		t.Fatalf("European gate changed the existing fleet-lot buffer: %+v want=%dp error=%v", gate, want, err)
	}
}

func TestOverseasCapacityCandidateLotRuleKeepsEuropeanEvaluatorUnchanged(t *testing.T) {
	t.Parallel()
	europe := capacityGrowthAcceptanceInput()
	decision := EvaluateCapacityGrowth(europe)
	want := max(europe.ReservePence, ceilBPS(europe.HullPricePence, 1_500)) + int64(europe.FleetSize+1)*europe.LotCashPence
	if !decision.Accepted || decision.WorkingCapitalPence != want {
		t.Fatalf("ordinary European capacity buffer changed: %+v want=%dp", decision, want)
	}
}

func TestOverseasCapacityFreshEvaluationResetsUnevaluatedBackoff(t *testing.T) {
	t.Parallel()
	h, runner, yard, hull, quote := overseasCapacityFixture(t)
	partner, found := findPort(runner.stateCache.Catalog, "new_york")
	if !found {
		t.Fatal("overseas partner port missing from the capacity fixture")
	}
	// The gate's pair evaluation needs both pair ports present as factors, so
	// seed them explicitly on top of the fixture's fresh quay books.
	runner.stateCache.Factors = append(runner.stateCache.Factors, yard.ID, partner.ID)
	at := time.Now().UTC()
	overseasMetric(t, h, 10_000_000, at)
	evidence, input := overseasCapacityEvidence(t, h, runner, at)
	input.SurplusPence, input.LotCashPence = 100_000_000, 100
	hiredAt := at.Add(-2 * time.Hour)
	runner.snapshot["_expansion_agents"] = encode([]expansionAgent{
		{PortID: yard.ID, Partner: partner.ID, Region: yard.BankingRegion, CreatedAt: hiredAt.Add(-time.Minute), HiredAt: hiredAt},
		{PortID: partner.ID, Partner: yard.ID, Region: yard.BankingRegion, CreatedAt: hiredAt.Add(-time.Minute), HiredAt: hiredAt},
	})
	runner.snapshot["_expansion_agent_outcomes"] = encode(map[string]expansionOutcome{
		yard.BankingRegion: {
			AttemptAt: hiredAt, RetryAt: at.Add(24 * time.Hour), Losses: 1,
			UnevaluatedAttempts: 2, UnevaluatedAttemptAt: hiredAt, UnevaluatedRetryAt: at.Add(48 * time.Hour),
		},
	})
	start := time.Now().UTC()
	gate, _, err := runner.capacityAtYard(h.ctx, yard, hull, quote, input, &evidence)
	if err != nil || !gate.Accepted {
		t.Fatalf("route-backed gate evaluation control: %+v %v", gate, err)
	}
	agents := decode[[]expansionAgent](runner.snapshot, "_expansion_agents")
	if len(agents) != 2 || agents[0].PortID != yard.ID || !agents[0].EvaluatedAt.After(start) {
		t.Fatalf("fresh route-backed evaluation did not stamp EvaluatedAt: %+v", agents)
	}
	outcome := decode[map[string]expansionOutcome](runner.snapshot, "_expansion_agent_outcomes")[yard.BankingRegion]
	if outcome.UnevaluatedAttempts != 0 || !outcome.UnevaluatedAttemptAt.IsZero() || !outcome.UnevaluatedRetryAt.IsZero() {
		t.Fatalf("fresh evaluation kept the unevaluated backoff: %+v", outcome)
	}
	if outcome.Losses != 1 || !outcome.RetryAt.Equal(at.Add(24*time.Hour)) {
		t.Fatalf("fresh evaluation reset the region's economic loss backoff: %+v", outcome)
	}
	persisted, err := h.db.Snapshot(h.ctx, h.account.ID)
	if err != nil {
		t.Fatal(err)
	}
	if durable := decode[map[string]expansionOutcome](persisted, "_expansion_agent_outcomes")[yard.BankingRegion]; durable.UnevaluatedAttempts != 0 ||
		!durable.UnevaluatedAttemptAt.IsZero() || !durable.UnevaluatedRetryAt.IsZero() || durable.Losses != 1 ||
		!durable.RetryAt.Equal(at.Add(24*time.Hour)) {
		t.Fatalf("persisted outcome lost the unevaluated reset or the economic backoff: %+v", durable)
	}
	durableAgents := decode[[]expansionAgent](persisted, "_expansion_agents")
	if len(durableAgents) != 2 || !durableAgents[0].EvaluatedAt.After(start) {
		t.Fatalf("persisted agents lost the fresh evaluation stamp: %+v", durableAgents)
	}
}
