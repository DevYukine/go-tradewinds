package engine

import (
	"encoding/json"
	"fmt"
	"net/http"
	"reflect"
	"testing"
	"time"

	"github.com/DevYukine/go-tradewinds/internal/store"
	"github.com/google/uuid"
)

// These fixtures use recorded host UTC settlement times, not accelerated game
// time or the server clock. The generous synthetic treasury is not live cash.
func growthWindowRunner(t *testing.T, h *recoveryHarness) *Runner {
	t.Helper()
	h.api.setFactorPorts(recoveryHomePort, recoveryFarPort)
	runner := h.runner(primaryTestKey)
	runner.generation = uuid.NewString()
	if err := runner.refresh(h.ctx); err != nil {
		t.Fatalf("refresh growth world: %v", err)
	}
	return runner
}

type growthSettlementAction struct {
	Path   string
	ShipID string
}

func persistGrowthWindowSettlement(t *testing.T, h *recoveryHarness, row store.Settlement, overrides ...growthSettlementAction) {
	t.Helper()
	row.AccountID = h.account.ID
	source := growthSettlementAction{Path: "/v1/market/place-order", ShipID: recoveryShipBlastoise}
	if len(overrides) > 0 {
		source = overrides[0]
	}
	row.ShipID = source.ShipID
	row.MissionID = row.ID + "-mission"
	row.ActionID = row.ID + "-action"
	row.Origin = recoveryHomePort
	row.Destination = recoveryFarPort
	row.Commodity = recoveryGrain
	row.Lane = laneKey(Plan{Kind: "spot", Origin: row.Origin, Destination: row.Destination, Commodity: row.Commodity})
	if _, err := h.db.BeginAction(h.ctx, &store.Action{
		ID: row.ActionID, AccountID: h.account.ID, Method: http.MethodPost,
		Path: source.Path,
		Body: encode(map[string]any{"ship_id": row.ShipID, "commodity_id": row.Commodity, "side": "ask", "quantity": row.QuantityTons}),
	}); err != nil {
		t.Fatalf("begin terminal evidence: %v", err)
	}
	if err := h.db.CompleteAction(h.ctx, row.ActionID, http.StatusOK, json.RawMessage(`{"filled_quantity":0}`), nil); err != nil {
		t.Fatalf("complete terminal evidence: %v", err)
	}
	if err := h.db.PutSettlement(h.ctx, &row); err != nil {
		t.Fatalf("persist terminal settlement: %v", err)
	}
}

// Admission must not repair, relabel or erase the permanent history consumed by
// sourcing and reporting, including its unknown-cost and duration flags.
func preserveGrowthWindowHistory(t *testing.T, h *recoveryHarness) {
	t.Helper()
	lane := laneKey(Plan{Kind: "spot", Origin: recoveryHomePort, Destination: recoveryFarPort, Commodity: recoveryGrain})
	for _, scope := range []string{"", lane} {
		before, err := h.db.EconomicHistory(h.ctx, h.account.ID, scope)
		if err != nil {
			t.Fatalf("read permanent history: %v", err)
		}
		t.Cleanup(func() {
			after, err := h.db.EconomicHistory(h.ctx, h.account.ID, scope)
			if err != nil {
				t.Errorf("reread permanent history: %v", err)
			} else if !reflect.DeepEqual(before, after) {
				t.Errorf("growth changed permanent history for %q: before %+v after %+v", scope, before, after)
			}
		})
	}
}

func TestGrowthWindowOldTerminalEvidenceAllowsFundedBuild(t *testing.T) {
	t.Parallel()
	for _, test := range []struct {
		name    string
		kind    string
		known   bool
		seconds int64
		cost    int64
	}{
		{name: "old_loss", kind: "aborted", known: true, seconds: 3_600, cost: 100_000_000},
		{name: "old_unknown_cost", kind: "aborted", seconds: 3_600},
		{name: "old_unknown_outcome", kind: "unknown", known: true, seconds: 3_600},
		{name: "old_unmeasured_duration", kind: "aborted", known: true},
	} {
		t.Run(test.name, func(t *testing.T) {
			h := newRecoveryHarness(t)
			h.seedLaneProofs()
			persistGrowthWindowSettlement(t, h, store.Settlement{
				ID: "old-terminal", Kind: test.kind, CostsKnown: test.known,
				GameSeconds: test.seconds, AcquisitionPence: test.cost,
				CompletedAt: time.Now().UTC().Add(-13 * time.Hour),
			})
			permanent, err := h.db.EconomicHistory(h.ctx, h.account.ID, "")
			if err != nil {
				t.Fatalf("read all-time disclosure: %v", err)
			}
			if test.cost > 0 && permanent.NetPence >= 0 ||
				(!test.known || test.kind == "unknown") && !permanent.UnknownCosts ||
				test.seconds == 0 && !permanent.UnmeasuredDuration {
				t.Fatalf("permanent history omitted the old loss or uncertainty: %+v", permanent)
			}
			preserveGrowthWindowHistory(t, h)
			runner := growthWindowRunner(t, h)
			evidence := growthEvidence{workingByRegion: make(map[string]int64)}
			if reason, err := runner.growthFunding(h.ctx, &evidence); err != nil || reason != "" {
				t.Errorf("terminal history older than 12 real hours vetoed two recent profitable delivered cycles: reason %q error %v", reason, err)
			}
			if changed, err := runner.considerGrowth(h.ctx); err != nil || !changed {
				t.Fatalf("fully funded measured growth must remain possible: changed %t error %v holds %v", changed, err, runner.holding)
			}
			mission, data := growthMissionWithHoldDiagnostics(t, h, runner)
			if mission.Phase != "growth_build" || data.Action == nil || data.Action.Stage != "build" || data.Plan.Quantity <= 0 {
				t.Fatalf("no executable durable build intent: phase %s data %+v", mission.Phase, data)
			}
			if h.api.mutationCount("/v1/shipyard/build-ship") != 0 {
				t.Fatal("admission sent construction instead of staging its durable request")
			}
		})
	}

}

func TestGrowthWindowRecentAndFutureEvidenceVetoesBuild(t *testing.T) {
	t.Parallel()
	for _, test := range []struct {
		name    string
		kind    string
		known   bool
		seconds int64
		cost    int64
		offset  time.Duration
	}{
		{name: "recent_loss", kind: "aborted", known: true, seconds: 3_600, cost: 100_000_000, offset: -time.Minute},
		{name: "recent_net_zero", kind: "aborted", known: true, seconds: 3_600, cost: 49_284, offset: -time.Minute},
		{name: "recent_unknown_cost", kind: "aborted", seconds: 3_600, offset: -time.Minute},
		{name: "recent_unknown_outcome", kind: "unknown", known: true, seconds: 3_600, offset: -time.Minute},
		{name: "recent_unmeasured_duration", kind: "aborted", known: true, offset: -time.Minute},
		{name: "future_loss", kind: "aborted", known: true, seconds: 3_600, cost: 100_000_000, offset: time.Hour},
		{name: "future_unknown_cost", kind: "aborted", seconds: 3_600, offset: time.Hour},
		{name: "future_known_completion", kind: "spot", known: true, seconds: 3_600, offset: time.Hour},
	} {
		t.Run(test.name, func(t *testing.T) {
			h := newRecoveryHarness(t)
			h.seedLaneProofs()
			persistGrowthWindowSettlement(t, h, store.Settlement{
				ID: "veto-evidence", Kind: test.kind, CostsKnown: test.known,
				GameSeconds: test.seconds, AcquisitionPence: test.cost,
				CompletedAt: time.Now().UTC().Add(test.offset),
			})
			preserveGrowthWindowHistory(t, h)
			runner := growthWindowRunner(t, h)
			evidence := growthEvidence{workingByRegion: make(map[string]int64)}
			if reason, err := runner.growthFunding(h.ctx, &evidence); err != nil || reason == "" {
				t.Errorf("unsafe cohort cleared account funding proof: reason %q error %v", reason, err)
			}
			if _, err := runner.considerGrowth(h.ctx); err != nil {
				t.Fatalf("consider unsafe cohort: %v", err)
			}
			h.missingMission("__growth")
			if h.api.mutationCount("/v1/shipyard/build-ship") != 0 {
				t.Fatal("unsafe cohort sent a construction request")
			}
		})
	}

}

func TestGrowthWindowNeedsTwoRecentDeliveredCycles(t *testing.T) {
	t.Parallel()
	for _, recent := range []int{0, 1} {
		t.Run(fmt.Sprintf("recent_%d", recent), func(t *testing.T) {
			h := newRecoveryHarness(t)
			asOf := time.Now().UTC()
			for i := range 2 {
				completed := asOf.Add(-13 * time.Hour)
				if i < recent {
					completed = asOf.Add(-time.Hour)
				}
				persistGrowthWindowSettlement(t, h, store.Settlement{
					ID: fmt.Sprintf("delivered-%d", i), Kind: "spot", CostsKnown: true,
					QuantityTons: 48, GameSeconds: 43_200, AcquisitionPence: 4_800, ProceedsPence: 30_000,
					CompletedAt: completed,
				})
			}
			preserveGrowthWindowHistory(t, h)
			runner := growthWindowRunner(t, h)
			if _, err := runner.considerGrowth(h.ctx); err != nil {
				t.Fatalf("consider insufficient recent delivery proof: %v", err)
			}
			h.missingMission("__growth")
		})
	}

}

func TestGrowthWindowUsesHostUTCRegardlessOfServerSkew(t *testing.T) {
	t.Parallel()
	for _, skew := range []time.Duration{-48 * time.Hour, 48 * time.Hour} {
		t.Run(skew.String(), func(t *testing.T) {
			h := newRecoveryHarness(t)
			h.seedLaneProofs()
			persistGrowthWindowSettlement(t, h, store.Settlement{
				ID: "old-loss", Kind: "aborted", CostsKnown: true, GameSeconds: 3_600,
				AcquisitionPence: 100_000_000, CompletedAt: time.Now().UTC().Add(-13 * time.Hour),
			})
			runner := growthWindowRunner(t, h)
			runner.stateCache.Clock.RealUTC = runner.stateCache.Clock.RealUTC.Add(skew)
			evidence := growthEvidence{workingByRegion: make(map[string]int64)}
			if reason, err := runner.growthFunding(h.ctx, &evidence); err != nil || reason != "" {
				t.Errorf("server skew changed host-recorded settlement cohort: reason %q error %v", reason, err)
			}
			persistGrowthWindowSettlement(t, h, store.Settlement{
				ID: "future-completion", Kind: "spot", CostsKnown: true, GameSeconds: 3_600,
				CompletedAt: time.Now().UTC().Add(time.Hour),
			})
			preserveGrowthWindowHistory(t, h)
			evidence = growthEvidence{workingByRegion: make(map[string]int64)}
			if reason, err := runner.growthFunding(h.ctx, &evidence); err != nil || reason == "" {
				t.Errorf("server skew admitted a future host-recorded completion: reason %q error %v", reason, err)
			}
		})
	}

}

// BeginAction assigns host UTC at creation and request timestamps are immutable.
// This covers an unresolved journal action without aging it; the separate
// old-active-mission case covers a liability started outside the proof window.
func TestGrowthWindowOutstandingJournalStillBlocks(t *testing.T) {
	t.Parallel()
	h := newRecoveryHarness(t)
	h.seedLaneProofs()
	action, err := h.db.BeginAction(h.ctx, &store.Action{
		ID: "unresolved-liability", AccountID: h.account.ID, Method: http.MethodPost,
		Path: "/v1/market/place-order",
		Body: encode(map[string]any{"ship_id": recoveryShipBlastoise, "commodity_id": recoveryGrain, "side": "bid", "quantity": 48}),
	})
	if err != nil {
		t.Fatalf("begin unresolved action: %v", err)
	}
	preserveGrowthWindowHistory(t, h)
	runner := growthWindowRunner(t, h)
	if _, err := runner.work(h.ctx); err != nil {
		t.Fatalf("retain unresolved action: %v", err)
	}
	h.missingMission("__growth")
	pending := h.pending()
	if len(pending) != 1 || pending[0].ID != action.ID || pending[0].State != store.ActionPending ||
		pending[0].Method != action.Method || pending[0].Path != action.Path ||
		!pending[0].CreatedAt.Equal(action.CreatedAt) || !reflect.DeepEqual(pending[0].Body, action.Body) {
		t.Fatalf("unresolved action was dropped or replaced: %+v", pending)
	}
	if h.api.mutationCount("/v1/shipyard/build-ship") != 0 {
		t.Fatal("an unresolved journal action permitted construction")
	}

}

func TestGrowthWindowOldActiveMissionStillReservesCapital(t *testing.T) {
	t.Parallel()
	h := newRecoveryHarness(t)
	h.seedLaneProofs()
	runner := growthWindowRunner(t, h)
	plan := Plan{
		Kind: "spot", ShipID: recoveryShipBlastoise, Origin: recoveryHomePort,
		Destination: recoveryFarPort, Commodity: recoveryGrain, Quantity: 48,
		BuyLimitPence: 1_250_000,
		Economics:     Economics{PurchasePence: 60_000_000, GameDays: 1},
	}
	h.putMission(recoveryShipBlastoise, "old-live-mission", "loading", missionData{
		Plan: plan, CostsKnown: true, StartedAt: time.Now().UTC().Add(-25 * time.Hour),
	})
	preserveGrowthWindowHistory(t, h)
	if _, err := runner.considerGrowth(h.ctx); err != nil {
		t.Fatalf("consider growth beside active cargo commitment: %v", err)
	}
	h.missingMission("__growth")
	_, retained := h.mission(recoveryShipBlastoise)
	if !reflect.DeepEqual(retained.Plan, plan) || retained.BuyDone {
		t.Fatalf("growth rewrote an old but active mission: %+v", retained)
	}

}

func TestGrowthWindowExpiredButActiveFreightStillProtectsDestination(t *testing.T) {
	t.Parallel()
	h := newRecoveryHarness(t)
	h.seedLaneProofs()
	h.api.setVaultBalance("south", 0)
	runner := growthWindowRunner(t, h)
	// Both actual arrivals bill north; only the still-active freight seal
	// makes south an obligated region in this account-funding comparison.
	for i := range runner.stateCache.Ships {
		runner.stateCache.Ships[i].RouteLegs = []RouteLeg{{ToNode: recoveryHomePort}}
	}
	evidence := growthEvidence{workingByRegion: make(map[string]int64)}
	if reason, err := runner.growthFunding(h.ctx, &evidence); err != nil || reason != "" {
		t.Fatalf("funded account without a south obligation rejected: reason %q error %v", reason, err)
	}
	deadline := time.Now().UTC().Add(-25 * time.Hour)
	contract := Contract{
		ID: "old-active-seal", ContractType: "freight", Status: "active",
		CarrierShipID: recoveryShipBlastoise, CarrierCompanyID: recoveryCompanyID,
		OriginPortID: recoveryHomePort, DestPortID: recoveryFarPort,
		CommodityID: recoveryGrain, QuantityRequired: 10, RemainingQuantity: 10,
		Metadata: encode(map[string]any{"delivery_deadline": deadline}),
	}
	runner.stateCache.Contracts = append(runner.stateCache.Contracts, contract)
	preserveGrowthWindowHistory(t, h)
	evidence = growthEvidence{workingByRegion: make(map[string]int64)}
	if reason, err := runner.growthFunding(h.ctx, &evidence); err != nil || reason == "" {
		t.Fatalf("old active freight lost its destination reserve: reason %q error %v", reason, err)
	}
	if _, err := runner.considerGrowth(h.ctx); err != nil {
		t.Fatalf("consider active freight liability: %v", err)
	}
	h.missingMission("__growth")
	if actual, exists := runner.contract(contract.ID); !exists || !reflect.DeepEqual(actual, contract) {
		t.Fatalf("active freight was discarded or relabelled: %+v", actual)
	}

}

func TestGrowthWindowFreshProofDoesNotBypassEconomicGuards(t *testing.T) {
	t.Parallel()
	for _, guard := range []string{"capital", "payback", "source_depth", "destination_depth"} {
		t.Run(guard, func(t *testing.T) {
			h := newRecoveryHarness(t)
			h.seedLaneProofs()
			runner := growthWindowRunner(t, h)
			switch guard {
			case "capital":
				for i := range runner.stateCache.Vaults {
					if runner.stateCache.Vaults[i].AccountType == "vault" {
						runner.stateCache.Vaults[i].BalancePence = runner.opts.ReservePence
					}
				}
			case "payback":
				runner.opts.MaxPaybackDays = 0.01
			case "source_depth":
				bookCommodity(runner.stateCache.Markets[recoveryHomePort], recoveryGrain).Depth.Asks = nil
			case "destination_depth":
				bookCommodity(runner.stateCache.Markets[recoveryFarPort], recoveryGrain).Depth.Bids = nil
			}
			preserveGrowthWindowHistory(t, h)
			if _, err := runner.considerGrowth(h.ctx); err != nil {
				t.Fatalf("consider growth at %s guard: %v", guard, err)
			}
			h.missingMission("__growth")
			if h.api.mutationCount("/v1/shipyard/build-ship") != 0 {
				t.Fatalf("recent evidence bypassed the %s guard", guard)
			}
		})
	}

}
