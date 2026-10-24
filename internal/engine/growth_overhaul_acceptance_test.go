package engine

// Failure-first acceptance contracts for the growth overhaul (plan E3),
// written against the current EvaluateGrowth/considerGrowth seams before the
// engine changes land:
//
//   - A larger supported hull lot scales marginal throughput beyond the
//     incumbent's absolute proven rate instead of being capped at it, while
//     thin measured demand and negative marginal upkeep still reject.
//   - An incremental funded partial lot below the proven lot size builds when
//     it still pays back inside the cap; the arbitrary 80%-of-proven-lot floor
//     is replaced by the payback and marginal-upkeep arithmetic, not removed
//     without a substitute.
//   - Execution evidence is permanent: the oldest loss beyond the bounded
//     500-row settlement display window still fails the loss-inclusive lane
//     net, so no bounded history read may forget it.
//
// Every fixture supplies generous depth, contract capacity and reserves so
// the only possible failure is the targeted behavior.

import (
	"encoding/json"
	"fmt"
	"math"
	"net/http"
	"testing"
	"time"

	"github.com/DevYukine/go-tradewinds/internal/store"
	"github.com/google/uuid"
)

// growthScaledThroughputFixture doubles the proven 20t lot: the sized plan
// estimates 200000p per game day and the measured source depth exactly covers
// two cycles of incumbent plus candidate demand (2*(20+40)).
func growthScaledThroughputFixture() GrowthInput {
	scaled := fundedGrowthFixture()
	scaled.Hull = Hull{HoldCapacityTons: 60, MaxCannons: 4}
	scaled.Plan.Quantity = 40
	scaled.Plan.Economics.NetPencePerGameDay = 200_000
	scaled.ObservedAskCapacityTons = 120
	return scaled
}

func TestGrowthScalesSupportedThroughputBeyondIncumbentRate(t *testing.T) {
	decision := EvaluateGrowth(growthScaledThroughputFixture())
	if !decision.Accepted || math.Abs(decision.IncrementalNetPencePerGameDay-199_000) > 1e-9 {
		t.Fatalf("a supported 40t lot (2x the proven 20t lot, 120t depth, 200000p/day sized, 1000p/day marginal upkeep) must scale marginal throughput beyond the incumbent's absolute 100000p/day rate instead of capping it: %+v", decision)
	}
	thin := growthScaledThroughputFixture()
	thin.ObservedAskCapacityTons = 119
	if decision := EvaluateGrowth(thin); decision.Accepted {
		t.Fatalf("thin measured demand funded a doubled hull lot: %+v", decision)
	}
	lossMaking := growthScaledThroughputFixture()
	lossMaking.IncrementalDailyUpkeepPence = 250_000
	if decision := EvaluateGrowth(lossMaking); decision.Accepted {
		t.Fatalf("marginal upkeep above the scaled contribution eliminated the profit but still funded construction: %+v", decision)
	}
}

func TestGrowthFundedPartialLotBelowProvenLotStillPaysBack(t *testing.T) {
	partial := fundedGrowthFixture()
	partial.Plan.Quantity = 10
	decision := EvaluateGrowth(partial)
	if !decision.Accepted || math.Abs(decision.IncrementalNetPencePerGameDay-49_000) > 1e-9 ||
		decision.PaybackGameDays <= 0 || decision.PaybackGameDays > partial.MaxPaybackDays {
		t.Fatalf("a funded incremental 10t lot (half the proven 20t lot) that still pays back inside the cap must build without an arbitrary 80%%-lot floor: %+v", decision)
	}
	unsized := partial
	unsized.IncrementalDailyUpkeepPence = 50_000
	if decision := EvaluateGrowth(unsized); decision.Accepted {
		t.Fatalf("a partial lot whose marginal upkeep consumes its scaled contribution funded construction: %+v", decision)
	}
}

// persistGrowthOverhaulSettlement writes one durable cost-known settlement
// with its own completed journal action as the evidence anchor, mirroring the
// production settlement shape of the recovery fixtures.
func persistGrowthOverhaulSettlement(t *testing.T, h *recoveryHarness, id string, completedAt time.Time, quantityTons, acquisitionPence, proceedsPence int64) {
	t.Helper()
	actionID := id + "-action"
	if _, err := h.db.BeginAction(h.ctx, &store.Action{
		ID: actionID, AccountID: h.account.ID, Method: http.MethodPost,
		Path: "/v1/market/place-order",
		Body: encode(map[string]any{"ship_id": recoveryShipBlastoise, "commodity_id": recoveryGrain, "side": "ask", "quantity": quantityTons}),
	}); err != nil {
		t.Fatalf("begin settlement action %s: %v", id, err)
	}
	if err := h.db.CompleteAction(h.ctx, actionID, http.StatusOK, json.RawMessage(`{"filled_quantity":0}`), nil); err != nil {
		t.Fatalf("complete settlement action %s: %v", id, err)
	}
	if err := h.db.PutSettlement(h.ctx, &store.Settlement{
		ID: id, AccountID: h.account.ID, ShipID: recoveryShipBlastoise, MissionID: id + "-mission",
		ActionID: actionID,
		Lane:     laneKey(Plan{Kind: "spot", Origin: recoveryHomePort, Destination: recoveryFarPort, Commodity: recoveryGrain}),
		Kind:     "spot", Origin: recoveryHomePort, Destination: recoveryFarPort, Commodity: recoveryGrain,
		QuantityTons: quantityTons, GameSeconds: 43_200, CostsKnown: true,
		AcquisitionPence: acquisitionPence, ProceedsPence: proceedsPence,
		CompletedAt: completedAt,
	}); err != nil {
		t.Fatalf("persist settlement %s: %v", id, err)
	}
}

// growthOverhaulHistory persists the 500 newest known-cost profitable lane
// settlements (20t, +20000p each) and, optionally, one strictly older
// settlement whose -10000000p loss makes the loss-inclusive lane net exactly
// zero. The bounded 500-row display window shows only the profits.
func growthOverhaulHistory(t *testing.T, h *recoveryHarness, withOldestLoss bool) {
	t.Helper()
	base := time.Now().UTC().Add(-720 * time.Hour)
	for i := range 500 {
		persistGrowthOverhaulSettlement(t, h, fmt.Sprintf("growth-history-profit-%03d", i),
			base.Add(time.Duration(i+2)*time.Hour), 20, 10_000, 30_000)
	}
	if withOldestLoss {
		persistGrowthOverhaulSettlement(t, h, "growth-history-oldest-loss", base.Add(time.Hour), 0, 10_010_000, 10_000)
	}
}

func TestGrowthRemembersOldestLossBeyondBoundedHistory(t *testing.T) {
	control := newRecoveryHarness(t)
	control.api.setFactorPorts(recoveryHomePort, recoveryFarPort)
	growthOverhaulHistory(t, control, false)
	controlRunner := control.runner(primaryTestKey)
	controlRunner.generation = uuid.NewString()
	if err := controlRunner.refresh(control.ctx); err != nil {
		t.Fatalf("refresh control history world: %v", err)
	}
	if changed, err := controlRunner.considerGrowth(control.ctx); err != nil || !changed {
		t.Fatalf("the 500 newest profitable settlements must still stage the funded build: changed %t error %v holds %v", changed, err, controlRunner.holding)
	}
	mission, data := growthMissionWithHoldDiagnostics(t, control, controlRunner)
	if mission.Phase != "growth_build" || data.Action == nil || data.Action.Stage != "build" {
		t.Fatalf("control leg lost its durable build intent: phase %s action %+v", mission.Phase, data.Action)
	}

	remembered := newRecoveryHarness(t)
	remembered.api.setFactorPorts(recoveryHomePort, recoveryFarPort)
	growthOverhaulHistory(t, remembered, true)
	runner := remembered.runner(primaryTestKey)
	runner.generation = uuid.NewString()
	if err := runner.refresh(remembered.ctx); err != nil {
		t.Fatalf("refresh loss-inclusive history world: %v", err)
	}
	if _, err := runner.considerGrowth(remembered.ctx); err != nil {
		t.Fatalf("considerGrowth with the durable oldest loss: %v", err)
	}
	remembered.missingMission("__growth")
	if remembered.api.mutationCount("/v1/shipyard/build-ship") != 0 {
		remembered.t.Fatal("a build request was sent although the loss-inclusive lane net is not positive")
	}
}
