package engine

// Consumer regressions for overseas debt holds:
//   - an expansion pair still awaiting its fresh capacity evaluation must hold
//     every savings and direct debt call before any quote or acceptance
//     mutation, and release ordinary financing once a real yard evaluation
//     lands;
//   - a capacity loan-accept must revalidate the identical zone-estimate guard
//     as the financed build preflight before its immutable request is sent.

import (
	"strings"
	"testing"
	"time"
)

// overseasDebtYardCandidate shapes the shared recovery world into the proven
// overseas savings balance pattern: every region keeps exactly its protected
// reserve and the yard region holds the whole build need minus the requested
// shortfall, so the estimate-backed yard candidate keeps a real executable
// estimated lane while being genuinely cash-short.
func overseasDebtYardCandidate(t *testing.T, h *recoveryHarness, runner *Runner, yard Port, hull Hull, quote YardQuote, shortfall int64) (growthEvidence, GrowthDecision) {
	t.Helper()
	at := time.Now().UTC()
	evidence, input := overseasCapacityEvidence(t, h, runner, at)
	sized, _, err := runner.capacityAtYard(h.ctx, yard, hull, quote, input, &evidence)
	if err != nil || !sized.Accepted || !strings.HasPrefix(sized.HazardSource, "zone estimate") {
		t.Fatalf("overseas debt fixture lacks an executable estimated lane: %+v %v", sized, err)
	}
	need := growthCashTotal(sized.QuotedHullPence, sized.SlippageReservePence, sized.CannonsPence, sized.InitialStoresPence, sized.WorkingCapitalPence)
	reserves, err := runner.treasuryReserves(h.ctx)
	if err != nil {
		t.Fatal(err)
	}
	for i := range runner.stateCache.Vaults {
		vault := &runner.stateCache.Vaults[i]
		vault.BalancePence = reserves[vault.BankingRegion]
		if vault.BankingRegion == yard.BankingRegion {
			vault.BalancePence += need - shortfall
		}
	}
	evidence, input = overseasCapacityEvidence(t, h, runner, time.Now().UTC())
	candidate, _, err := runner.capacityAtYard(h.ctx, yard, hull, quote, input, &evidence)
	if err != nil || candidate.Accepted || !candidate.EconomicallyViable || candidate.CashShortfallPence != shortfall ||
		candidate.DepthNetPencePerGameDay <= 0 || candidate.HazardDestination != "new_york" {
		t.Fatalf("overseas debt fixture is not genuinely cash-short on its estimated lane: %+v %v", candidate, err)
	}
	return evidence, candidate
}

func TestOverseasAwaitingPairHoldsEuropeanDebtUntilFreshEvaluation(t *testing.T) {
	t.Parallel()
	h, runner := loanCapacityFixture(t, 750_000, false)
	runner.stateCache.Clock.Calendar.TimeScale = 60
	runner.stateCache.Contracts = nil
	bookCommodity(runner.stateCache.Markets[recoveryFarPort], recoveryGrain).Depth.Bids[0].PricePence = 50_000
	loanCapacityCashShortfall(t, h, runner, 100_000)
	// The cash refresh restores the API books; reapply the executable depth.
	runner.stateCache.Clock.Calendar.TimeScale = 60
	runner.stateCache.Contracts = nil
	bookCommodity(runner.stateCache.Markets[recoveryFarPort], recoveryGrain).Depth.Bids[0].PricePence = 50_000
	yard, hull, _ := overseasAddCapacityWorld(t, h, runner)
	runner.stateCache.Factors = append(runner.stateCache.Factors, yard.ID, "new_york")
	// The proven cash-short shape: every region exactly its protected reserve,
	// and the European debt region holding the only free working cash, so its
	// grain lane funds at every size while the build stays a genuine debt case
	// and the unreachable overseas vault cannot erase the shortfall.
	reserves, err := runner.treasuryReserves(h.ctx)
	if err != nil {
		t.Fatal(err)
	}
	port, _ := findPort(runner.stateCache.Catalog, recoveryHomePort)
	for i := range runner.stateCache.Vaults {
		vault := &runner.stateCache.Vaults[i]
		if vault.BankingRegion == port.BankingRegion {
			vault.BalancePence = reserves[vault.BankingRegion] + 40_000
			continue
		}
		vault.BalancePence = reserves[vault.BankingRegion]
	}
	// Control: with the final world state and no pair seeded, an ordinary pass
	// stages a real quote-backed European financing intent without accepting
	// debt or building. Quote GETs before the pair incur no debt.
	evidence, _ := overseasCapacityEvidence(t, h, runner, time.Now().UTC())
	before := h.api.mutationCount("/v1/loan/quote")
	if changed, err := runner.considerCapacityGrowth(h.ctx, &evidence); err != nil || !changed {
		t.Fatalf("european control pass did not stage financing: changed=%t err=%v holds=%v", changed, err, runner.holding)
	}
	_, data := h.mission("__growth")
	if data.BuildPort != recoveryHomePort || data.Capacity == nil || data.Capacity.Financing == nil ||
		len(data.Capacity.Financing.Quotes) == 0 || data.Action != nil {
		t.Fatalf("control pass did not stage a real European financing intent: %+v", data)
	}
	if quotes := h.api.mutationCount("/v1/loan/quote") - before; quotes == 0 ||
		h.api.mutationCount("/v1/loan/accept-quote") != 0 || h.api.mutationCount("/v1/shipyard/build-ship") != 0 {
		t.Fatalf("control pass requested %d quotes; accept and build must stay unsent", quotes)
	}
	if err := h.db.DeleteMission(h.ctx, h.account.ID, "__growth"); err != nil {
		t.Fatal(err)
	}
	// Held pass: seed the awaiting pair and block its own yard's evaluation, so
	// the pair is genuinely awaiting when the growth decision reaches its debt
	// calls. The proven European candidate is unchanged; the hold alone must
	// prevent every debt mutation.
	hiredAt := time.Now().UTC().Add(-time.Hour)
	runner.snapshot["_expansion_agents"] = encode([]expansionAgent{
		{PortID: yard.ID, Partner: "new_york", Region: yard.BankingRegion, HiredAt: hiredAt},
		{PortID: "new_york", Partner: yard.ID, Region: yard.BankingRegion, HiredAt: hiredAt},
	})
	runner.snapshot["_growth_build_backoffs"] = encode(map[string]time.Time{
		yard.ID + "|" + hull.ID: time.Now().UTC().Add(time.Hour),
	})
	heldAt := h.api.mutationCount("/v1/loan/quote")
	evidence, _ = overseasCapacityEvidence(t, h, runner, time.Now().UTC())
	if changed, err := runner.considerCapacityGrowth(h.ctx, &evidence); err != nil || changed {
		t.Fatalf("awaiting pair staged debt: changed=%t err=%v holds=%v", changed, err, runner.holding)
	}
	if quotes := h.api.mutationCount("/v1/loan/quote") - heldAt; quotes != 0 ||
		h.api.mutationCount("/v1/loan/accept-quote") != 0 || h.api.mutationCount("/v1/shipyard/build-ship") != 0 {
		t.Fatalf("awaiting pair still mutated debt or construction: quotes=%d", quotes)
	}
	h.missingMission("__growth")
	h.missingMission("__treasury")
	requireReviewHold(t, runner, "growth waits for the expansion pair's fresh capacity evaluation", true)
	for _, agent := range decode[[]expansionAgent](runner.snapshot, "_expansion_agents") {
		if !agent.EvaluatedAt.IsZero() {
			t.Fatalf("blocked pass recorded a premature pair evaluation: %+v", agent)
		}
	}
	// Release pass: a real route-backed yard evaluation marks the pair, and the
	// proven European financing stages again in the same consumer pass.
	delete(runner.snapshot, "_growth_build_backoffs")
	evidence, _ = overseasCapacityEvidence(t, h, runner, time.Now().UTC())
	if changed, err := runner.considerCapacityGrowth(h.ctx, &evidence); err != nil || !changed {
		t.Fatalf("fresh evaluation did not release normal financing: changed=%t err=%v holds=%v", changed, err, runner.holding)
	}
	_, data = h.mission("__growth")
	if data.BuildPort != recoveryHomePort || data.Capacity == nil || data.Capacity.Financing == nil ||
		len(data.Capacity.Financing.Quotes) == 0 || data.Action != nil {
		t.Fatalf("released pass did not stage the European financing intent: %+v", data)
	}
	if h.api.mutationCount("/v1/loan/quote") == heldAt {
		t.Fatal("released pass staged financing without requesting real quotes")
	}
	if h.api.mutationCount("/v1/loan/accept-quote") != 0 {
		t.Fatal("staging alone accepted debt")
	}
	evaluated := false
	for _, agent := range decode[[]expansionAgent](runner.snapshot, "_expansion_agents") {
		if agent.PortID == yard.ID && agent.EvaluatedAt.After(hiredAt) {
			evaluated = true
		}
	}
	if !evaluated {
		t.Fatal("release pass did not record the pair's real yard evaluation")
	}
}

func TestOverseasLoanAcceptRevalidatesZoneEstimateBeforeDebt(t *testing.T) {
	t.Parallel()
	for _, change := range []string{
		"unchanged control", "piracy beyond calibration limit", "negative piracy observation",
		"missing piracy observation", "stale valuation", "insufficient valuation",
	} {
		t.Run(change, func(t *testing.T) {
			h, runner := loanCapacityFixture(t, 750_000, false)
			yard, hull, quote := overseasAddCapacityWorld(t, h, runner)
			runner.stateCache.Factors = append(runner.stateCache.Factors, yard.ID)
			quote, netWorth, _ := overseasCapacityBoundary(t, h, runner, yard, hull, quote)
			at := time.Now().UTC().Truncate(time.Microsecond)
			overseasMetric(t, h, netWorth, at)
			evidence, candidate := overseasDebtYardCandidate(t, h, runner, yard, hull, quote, 50_000)
			if change == "insufficient valuation" {
				// The frozen admission stays valid at the exact 35% boundary; a
				// fresher but lower publication must fail the live recheck.
				overseasMetric(t, h, netWorth-1, time.Now().UTC())
			}
			if changed, err := runner.considerCapacityFinancing(h.ctx, yard, hull, candidate, clonePence(evidence.required), &evidence); err != nil || !changed {
				t.Fatalf("frozen financing admission: changed=%t err=%v holds=%v", changed, err, runner.holding)
			}
			mission, data := h.mission("__growth")
			if data.Capacity == nil || data.Capacity.Financing == nil || len(data.Capacity.Financing.Quotes) != 1 || data.Action != nil {
				t.Fatalf("financing did not stage its real quote package: %+v", data)
			}
			if changed, err := runner.driveGrowth(h.ctx, mission, &data); err != nil || !changed {
				t.Fatalf("loan-accept staging: changed=%t err=%v holds=%v", changed, err, runner.holding)
			}
			mission, data = h.mission("__growth")
			if data.Action == nil || data.Action.Stage != "loan-accept" {
				t.Fatalf("financing did not stage its immutable loan acceptance: %+v", data.Action)
			}
			switch change {
			case "piracy beyond calibration limit", "negative piracy observation", "missing piracy observation":
				// Calibration samples record zero piracy in zone-north, so the
				// sampled arrival risk is that zone's storm rating.
				sampleRisk := int64(0)
				for _, zone := range runner.stateCache.Catalog.SeaZones {
					if zone.ID == "zone-north" {
						sampleRisk = zone.BaseStormBPS
						break
					}
				}
				limitPiracy := sampleRisk + sampleRisk/2
				for _, zone := range runner.stateCache.Catalog.SeaZones {
					if zone.ID == yard.SeaZoneID {
						limitPiracy += -zone.BaseStormBPS
						break
					}
				}
				limitPiracy++
				zones := decode[[]ZoneObservation](runner.snapshot, "zones")
				observed := make([]ZoneObservation, 0, len(zones))
				touched := false
				for _, zone := range zones {
					if zone.SeaZoneID != yard.SeaZoneID {
						observed = append(observed, zone)
						continue
					}
					touched = true
					if change == "piracy beyond calibration limit" {
						zone.NetPiracyBPS = limitPiracy
						observed = append(observed, zone)
					} else if change == "negative piracy observation" {
						zone.NetPiracyBPS = -1
						observed = append(observed, zone)
					}
					// "missing piracy observation" drops the arrival zone.
				}
				if !touched {
					t.Fatalf("arrival zone %s is not observed in the fixture", yard.SeaZoneID)
				}
				runner.snapshot["zones"] = encode(observed)
			case "stale valuation":
				overseasMetric(t, h, netWorth, at.Add(-6*time.Hour-time.Second))
			}
			runner.holding = nil
			changed, err := runner.dispatch(h.ctx, mission, &data)
			if err != nil {
				t.Fatal(err)
			}
			if h.api.mutationCount("/v1/shipyard/build-ship") != 0 {
				t.Fatal("loan dispatch reached construction")
			}
			if change == "unchanged control" {
				if !changed || h.api.mutationCount("/v1/loan/accept-quote") != 1 {
					t.Fatalf("valid admission did not dispatch its acceptance: changed=%t holds=%v", changed, runner.holding)
				}
				mission, data = h.mission("__growth")
				if len(data.Capacity.Financing.Loans) != 1 || data.Capacity.Financing.Cancel {
					t.Fatalf("accepted quote did not record its authoritative loan: %+v", data.Capacity.Financing)
				}
				return
			}
			if h.api.mutationCount("/v1/loan/accept-quote") != 0 {
				t.Fatalf("%s still accepted debt over an invalidated estimate", change)
			}
			wantReason := hazardUnavailable
			if change == "insufficient valuation" {
				wantReason = capacitySolvencyReason
			}
			requireReviewHold(t, runner, wantReason, false)
			mission, data = h.mission("__growth")
			if data.Action != nil || data.Capacity == nil || data.Capacity.Financing == nil ||
				len(data.Capacity.Financing.Quotes) != 1 || len(data.Capacity.Financing.Loans) != 0 {
				t.Fatalf("%s did not retain the frozen package while dropping the never-sent accept: %+v", change, data)
			}
			if len(h.pending()) != 0 {
				t.Fatalf("%s journaled its refused loan acceptance: %+v", change, h.pending())
			}
			if change != "insufficient valuation" {
				row := overseasCapacityDecision(t, h, yard.ID, hull.ID)
				if !strings.Contains(string(row.Details), hazardUnavailable) {
					t.Fatalf("%s lost its typed hazard verdict: %s", change, row.Details)
				}
			}
		})
	}
}
