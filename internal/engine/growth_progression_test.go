package engine

// Acceptance tests for fleet progression (design G1-G3 and PROOF). They pin
// consumer-visible growth behavior and deliberately fail against the current
// implementation where the design changes it:
//
//   - G1: no fleet-size ceiling gates viable growth; the integration test
//     below stages a viable build for a 13-ship fleet on an unconfigured
//     runner.
//   - G2: rolling source and spot-destination depth (two lots per cycle after
//     incumbent demand) replaces the whole-payback stock requirement, while
//     finite charter capacity and expiry must still cover payback. Each
//     overlap pool (other source, other destination, same contract) binds
//     exactly its own dimension at one-ton boundaries.
//   - G3: the exact single-region funding boundary (293000p passes,
//     292999p fails) is preserved, and a negative vault in any region blocks
//     construction even when the yard region is rich.
//   - PROOF: loss-inclusive accounting is unchanged; cumulative net zero
//     fails the positive-proof gate and +6193p clears only that gate.
//
// Seams the current code cannot express (documented, not worked around with
// new harnesses):
//   - Per-lane depth attribution ("unrelated fleet capacity must not consume
//     this lane's depth") lives in considerGrowth's incumbent reservation
//     loop; EvaluateGrowth only receives the pre-reserved ExistingCycleTons,
//     so no unit seam can distinguish related from unrelated hulls.
//   - An obligated region that stays underfunded while the yard region is
//     rich is unreachable through the recovery fixture: manageTreasury always
//     finds a feasible north->south remittance for the small shortfalls this
//     world can produce (each ship's planning destination makes south an
//     obligated region), and a remittance that cannot succeed stages a
//     __treasury mission that starves considerGrowth. The negative-vault
//     clause of the same G3 guard is therefore covered by invoking the
//     existing considerGrowth seam directly on coherent refreshed state,
//     which leaves manageTreasury's beneficial repair enabled elsewhere.
//   - Remittance-fee funding of incremental upkeep (G3) has no single-region
//     input; the existing 293000p boundary is pinned instead.

import (
	"encoding/json"
	"fmt"
	"testing"

	"github.com/DevYukine/go-tradewinds/internal/store"
	"github.com/google/uuid"
)

// fundedGrowthFixture mirrors the proven 20t-candidate + 20t-incumbent lane
// fixture in planner_test.go: hull capital 175000p (100000 hull + 10000
// slippage + 60000 cannons + 5000 stores), net 99000p/game day after marginal
// upkeep, one-day cycle, so payback is 175000/99000 game days.
func fundedGrowthFixture() GrowthInput {
	price := int64(100_000)
	return GrowthInput{
		Plan:         Plan{Accepted: true, Quantity: 20, Economics: Economics{PurchasePence: 10_000, GameDays: 1, NetPencePerGameDay: 100_000}},
		Quote:        YardQuote{Available: true, BuildMode: "turnkey_cash", TotalCostPence: &price},
		Hull:         Hull{HoldCapacityTons: 40, MaxCannons: 4},
		Proofs:       []LaneProof{{QuantityTons: 20, GameSeconds: 86_400, NetPence: 100_000, CostsKnown: true}, {QuantityTons: 20, GameSeconds: 86_400, NetPence: 100_000, CostsKnown: true}},
		BalancePence: 1_000_000, ReservePence: 75_000,
		ExistingWorkingCapitalPence: 20_000, ExistingCycleTons: 20,
		ObservedAskCapacityTons: 200, ContractRemainingTons: 1000,
		IncrementalDailyUpkeepPence: 1000, InitialStoresPence: 5000, MaxPaybackDays: 12,
	}
}

// TestGrowthRollingSourceDepthReplacesWholePaybackStock covers G2: measured
// source asks must spare two lots per cycle after incumbent demand (2*(20t
// candidate + 20t incumbent) = 80t), and must not be required to cover the
// whole payback (3 cycles * 40t = 120t under the old rule).
func TestGrowthRollingSourceDepthReplacesWholePaybackStock(t *testing.T) {
	rolling := fundedGrowthFixture()
	rolling.ObservedAskCapacityTons = 100
	if decision := EvaluateGrowth(rolling); !decision.Accepted {
		t.Fatalf("rolling source depth of 100t was rejected as if the whole payback needed 120t of stock: %s", decision.Reason)
	}
	scarce := rolling
	scarce.ObservedAskCapacityTons = 79
	if decision := EvaluateGrowth(scarce); decision.Accepted {
		t.Fatal("79t of source depth funded two 40t cycle lots")
	}
}

// TestGrowthSpotBidsNeedRollingDeliverableDepth covers G2: a spot lane needs
// measured spare destination bids of 2*(20t candidate + 20t incumbent) = 80t,
// not 120t of whole-payback bids, and 79t must still fail.
func TestGrowthSpotBidsNeedRollingDeliverableDepth(t *testing.T) {
	spot := fundedGrowthFixture()
	spot.Plan.Kind = "spot"
	spot.ContractRemainingTons = -1
	spot.ObservedBidCapacityTons = 80
	if decision := EvaluateGrowth(spot); !decision.Accepted {
		t.Fatalf("80t of measured spot bids was rejected as if the whole payback needed 120t: %s", decision.Reason)
	}
	scarce := spot
	scarce.ObservedBidCapacityTons = 79
	if decision := EvaluateGrowth(scarce); decision.Accepted {
		t.Fatal("79t of spot bids funded two 40t cycle lots")
	}
}

// TestGrowthOtherSourceOverlapConsumesOnlySourceAsks covers G2 allocation: a
// same-origin same-commodity incumbent on another lane consumes source asks
// only. The exact two-cycle ask boundary rises from 80t to 2*(20+20+20)=120t
// while the charter and destination pools stay untouched.
func TestGrowthOtherSourceOverlapConsumesOnlySourceAsks(t *testing.T) {
	overlap := fundedGrowthFixture()
	overlap.OtherSourceCycleTons = 20
	overlap.ObservedAskCapacityTons = 120
	if decision := EvaluateGrowth(overlap); !decision.Accepted {
		t.Fatalf("source depth exactly covering both cycles of competing demand was rejected: %s", decision.Reason)
	}
	short := overlap
	short.ObservedAskCapacityTons = 119
	if decision := EvaluateGrowth(short); decision.Accepted {
		t.Fatal("119t of source depth covered three 40t cycle lots")
	}
}

// TestGrowthOtherDestinationOverlapConsumesOnlySpotBids covers G2 allocation:
// a same-destination same-commodity incumbent consumes destination bids only.
// The exact two-cycle bid boundary rises from 80t to 120t while source asks
// keep their own 80t boundary.
func TestGrowthOtherDestinationOverlapConsumesOnlySpotBids(t *testing.T) {
	overlap := fundedGrowthFixture()
	overlap.Plan.Kind = "spot"
	overlap.ContractRemainingTons = -1
	overlap.OtherDestinationCycleTons = 20
	overlap.ObservedBidCapacityTons = 120
	if decision := EvaluateGrowth(overlap); !decision.Accepted {
		t.Fatalf("spot bids exactly covering both cycles of competing destination demand were rejected: %s", decision.Reason)
	}
	short := overlap
	short.ObservedBidCapacityTons = 119
	if decision := EvaluateGrowth(short); decision.Accepted {
		t.Fatal("119t of spot bids covered three 40t cycle lots")
	}
}

// TestGrowthOtherContractOverlapConsumesOnlyCharterCapacity covers G2
// allocation: an incumbent on the same finite contract consumes contract
// capacity only. The exact remaining-ton boundary rises from 120t to
// max(2,ceil(P/C)+1)*(20+20+20)=180t while asks stay at their 80t boundary.
func TestGrowthOtherContractOverlapConsumesOnlyCharterCapacity(t *testing.T) {
	overlap := fundedGrowthFixture()
	overlap.OtherContractCycleTons = 20
	overlap.ContractRemainingTons = 180
	if decision := EvaluateGrowth(overlap); !decision.Accepted {
		t.Fatalf("contract capacity exactly covering payback demand with a same-contract incumbent was rejected: %s", decision.Reason)
	}
	short := overlap
	short.ContractRemainingTons = 179
	if decision := EvaluateGrowth(short); decision.Accepted {
		t.Fatal("179t of contract capacity covered the 180t payback demand")
	}
}

// TestGrowthCharterCapacityCoversPaybackAndExpiry pins the finite-charter
// rules that survive G2: remaining contract tons must cover
// max(2, ceil(P/C)+1) lots of candidate+incumbent demand (3*40t = 120t at
// payback P=175000/99000 and cycle C=1), and expiry must not fall short of
// P+C. The boundaries are exact on both sides.
func TestGrowthCharterCapacityCoversPaybackAndExpiry(t *testing.T) {
	baseline := EvaluateGrowth(fundedGrowthFixture())
	if !baseline.Accepted {
		t.Fatalf("funded proven lane rejected before charter boundaries: %s", baseline.Reason)
	}
	expiryDays := baseline.PaybackGameDays + fundedGrowthFixture().Plan.Economics.GameDays

	funded := fundedGrowthFixture()
	funded.ContractRemainingTons = 120
	funded.ContractGameDaysRemaining = &expiryDays
	if decision := EvaluateGrowth(funded); !decision.Accepted {
		t.Fatalf("charter with exactly payback-and-one-cycle capacity and expiry was rejected: %s", decision.Reason)
	}
	shortCapacity := funded
	shortCapacity.ContractRemainingTons = 119
	if decision := EvaluateGrowth(shortCapacity); decision.Accepted {
		t.Fatal("charter capacity of 119t covered the 120t payback demand")
	}
	earlyExpiry := funded
	shortDays := expiryDays - 0.001
	earlyExpiry.ContractGameDaysRemaining = &shortDays
	if decision := EvaluateGrowth(earlyExpiry); decision.Accepted {
		t.Fatal("charter expiring before payback plus one delivery cycle was accepted")
	}
}

// TestGrowthFundingBoundaryIsExact covers G3: the single-region fixture
// requires exactly 293000p (175000 capital + 20000 new working capital +
// 20000 incumbent working capital + 3000 upkeep horizon + 75000 reserve).
func TestGrowthFundingBoundaryIsExact(t *testing.T) {
	funded := fundedGrowthFixture()
	funded.BalancePence = 293_000
	if decision := EvaluateGrowth(funded); !decision.Accepted {
		t.Fatalf("exactly-funded growth was rejected: %s", decision.Reason)
	}
	short := fundedGrowthFixture()
	short.BalancePence = 292_999
	if decision := EvaluateGrowth(short); decision.Accepted {
		t.Fatal("growth was authorized one pence short of protected working capital, upkeep horizon and reserve")
	}
}

// TestGrowthProofRequiresPositiveLossInclusiveNet covers PROOF: cumulative
// net stays loss-inclusive. The production settlement series (-18333p, then
// winners leaving -6192p cumulative) must fail at exactly zero, and one more
// +6193p settlement clears only the positive-proof gate — payback still
// fails, so proof alone never authorizes construction.
func TestGrowthProofRequiresPositiveLossInclusiveNet(t *testing.T) {
	zero := fundedGrowthFixture()
	zero.Proofs = []LaneProof{
		{GameSeconds: 86_400, NetPence: -18_333, CostsKnown: true},
		{QuantityTons: 20, GameSeconds: 43_200, NetPence: 12_141, CostsKnown: true},
		{QuantityTons: 20, GameSeconds: 43_200, NetPence: 6_192, CostsKnown: true},
	}
	if decision := EvaluateGrowth(zero); decision.Accepted {
		t.Fatal("growth was authorized on lane history whose loss-inclusive net is exactly zero")
	}
	positive := zero
	positive.Proofs = append(positive.Proofs, LaneProof{QuantityTons: 20, GameSeconds: 43_200, NetPence: 6_193, CostsKnown: true})
	decision := EvaluateGrowth(positive)
	if decision.Accepted {
		t.Fatal("a +6193p proof margin authorized construction despite failing payback")
	}
	if decision.PaybackGameDays <= positive.MaxPaybackDays {
		t.Fatalf("rejection did not reach the payback gate, so the positive-proof clearance was not isolated: %+v", decision)
	}
}

// TestGrowthStagesBuildForThirteenShipFleetWithoutCeiling drives G1 through
// the existing recovery runner fixture: an unconfigured runner (whose former
// fallback ceiling was 4) must still authorize a viable build for a 13-ship
// fleet, and staging must persist an intent without sending it. One hull
// stays in flight from the destination port so refresh captures the south
// book the spot lane plans against.
func TestGrowthStagesBuildForThirteenShipFleetWithoutCeiling(t *testing.T) {
	h := newRecoveryHarness(t)
	fleet := make([]map[string]any, 0, 13)
	fleet = append(fleet, sailingShipRow(recoveryShipBlastoise, "Blastoise", recoveryFarPort, recoveryHomePort))
	for i := range 12 {
		fleet = append(fleet, sailingShipRow(
			fmt.Sprintf("55555555-5555-4555-8555-%012d", i),
			fmt.Sprintf("GrowthFleet%02d", i),
			recoveryHomePort, recoveryFarPort,
		))
	}
	h.api.setFleet(fleet)
	runner := h.runner(primaryTestKey)
	for range 6 {
		changed, err := h.tick(runner)
		if err != nil {
			t.Fatalf("warm-up tick: %v", err)
		}
		if !changed {
			break
		}
	}
	h.seedLaneProofs()
	if _, err := h.tick(runner); err != nil {
		t.Fatalf("growth tick with thirteen ships: %v", err)
	}
	mission, data := growthMissionWithHoldDiagnostics(t, h, runner)
	if mission.Phase != "growth_build" || data.Action == nil || data.BuildName == "" {
		t.Fatalf("thirteen-ship fleet produced no durable build intent: phase %s action %+v", mission.Phase, data.Action)
	}
	if h.api.mutationCount("/v1/shipyard/build-ship") != 0 {
		t.Fatal("staging must not send the build request")
	}
}

// growthMissionWithHoldDiagnostics loads the durable growth intent and, when
// production deferred or rejected instead of staging, fails with the runner's
// accumulated hold reasons so the binding gate is visible.
func growthMissionWithHoldDiagnostics(t *testing.T, h *recoveryHarness, runner *Runner) (*store.Mission, missionData) {
	t.Helper()
	mission, err := h.db.GetMission(h.ctx, h.account.ID, "__growth")
	if err != nil {
		t.Fatalf("load growth mission: %v", err)
	}
	if mission == nil {
		t.Fatalf("no durable growth intent was staged; runner holds: %v", runner.holding)
	}
	var data missionData
	if err := json.Unmarshal(mission.Data, &data); err != nil {
		t.Fatalf("decode growth mission: %v", err)
	}
	return mission, data
}

// TestGrowthBlockedWhileAnyVaultIsNegative drives the negative-vault clause
// of G3 through the existing considerGrowth seam on coherent refreshed
// state. The funded control leg proves the seam itself authorizes and stages
// a build while the south vault holds funds above its protected berth and
// upkeep requirement; the negative leg then proves the same state with a
// negative south vault stages nothing. Both incumbent hulls stay in flight
// on the seeded lane so no idle ship can absorb the plan before the build,
// and manageTreasury's beneficial repair is not run here, not prohibited.
func TestGrowthBlockedWhileAnyVaultIsNegative(t *testing.T) {
	control := newRecoveryHarness(t)
	control.api.setVaultBalance("south", 1_000_000)
	runner := control.runner(primaryTestKey)
	runner.generation = uuid.NewString()
	if err := runner.refresh(control.ctx); err != nil {
		t.Fatalf("refresh funded control state: %v", err)
	}
	control.seedLaneProofs()
	if _, err := runner.considerGrowth(control.ctx); err != nil {
		t.Fatalf("considerGrowth funded control: %v", err)
	}
	mission, data := growthMissionWithHoldDiagnostics(t, control, runner)
	if mission.Phase != "growth_build" || data.Action == nil || data.BuildName == "" {
		t.Fatalf("funded control staged no durable build intent: phase %s action %+v", mission.Phase, data.Action)
	}
	if control.api.mutationCount("/v1/shipyard/build-ship") != 0 {
		t.Fatal("staging must not send the build request")
	}

	blocked := newRecoveryHarness(t)
	blocked.api.setVaultBalance("south", -1)
	restricted := blocked.runner(primaryTestKey)
	restricted.generation = uuid.NewString()
	if err := restricted.refresh(blocked.ctx); err != nil {
		t.Fatalf("refresh state under a negative vault: %v", err)
	}
	blocked.seedLaneProofs()
	if _, err := restricted.considerGrowth(blocked.ctx); err != nil {
		t.Fatalf("considerGrowth under a negative vault: %v", err)
	}
	blocked.missingMission("__growth")
	if blocked.api.mutationCount("/v1/shipyard/build-ship") != 0 {
		blocked.t.Fatal("a build request was sent while a vault was negative")
	}
}
