package engine

import (
	"encoding/json"
	"math"
	"net/http"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/DevYukine/go-tradewinds/internal/store"
)

func realizedRelocationInput(t *testing.T) (PlanningInput, realizedSourcingLane) {
	t.Helper()
	in := sourcingInputFixture(t)
	in.Contracts = nil
	in.SourcingIdleEligible = true
	in.SourcingProbeHistory = probeHistoryFixture()
	lane := realizedSourcingLane{Origin: "amsterdam", Destination: "portsmouth", Commodity: "sawn_planks",
		QuantityTons: 24, NetPence: 10_000, GameDays: 0.25, Samples: 2, CompletedAt: in.Now.Add(-time.Hour)}
	in.SourcingProbeHistory.RealizedLanes = []realizedSourcingLane{lane}
	return in, lane
}

func TestRealizedRelocationUsesNetOfEveryKnownSpotReceipt(t *testing.T) {
	in, _ := realizedRelocationInput(t)
	base := store.Settlement{Kind: "spot", CostsKnown: true, Origin: "amsterdam", Destination: "portsmouth", Commodity: "sawn_planks",
		QuantityTons: 10, GameSeconds: 21_600, ProceedsPence: 20_000, AcquisitionPence: 10_000,
		FeesPence: 1000, ProvisionsPence: 2000, ExpensesPence: 3000,
		StartedAt: in.Now.Add(-3 * time.Hour), CompletedAt: in.Now.Add(-2 * time.Hour)}
	loss := base
	loss.ProceedsPence, loss.QuantityTons, loss.GameSeconds = 14_000, 11, 43_200
	loss.CompletedAt = in.Now.Add(-time.Hour)
	rows := []store.Settlement{base, loss}
	for _, kind := range []string{"charter", "freight", "spot+freight", "charter+freight", "recovery"} {
		finite := base
		finite.Kind, finite.ProceedsPence = kind, 1_000_000
		rows = append(rows, finite)
	}
	for _, invalid := range []string{"unknown cost", "unknown proceeds", "old", "future", "no duration", "empty", "reversed"} {
		row := base
		switch invalid {
		case "unknown cost":
			row.CostsKnown = false
		case "unknown proceeds":
			row.ProceedsUnknown = true
		case "old":
			row.StartedAt, row.CompletedAt = in.Now.Add(-74*time.Hour), in.Now.Add(-73*time.Hour)
		case "future":
			row.CompletedAt = in.Now.Add(time.Second)
		case "no duration":
			row.GameSeconds = 0
		case "empty":
			row.QuantityTons = 0
		case "reversed":
			row.StartedAt = row.CompletedAt.Add(time.Second)
		}
		rows = append(rows, row)
	}
	lanes, err := recentRealizedSourcingLanes(rows, in.Now)
	if err != nil || len(lanes) != 1 {
		t.Fatalf("realized lanes=%+v error=%v", lanes, err)
	}
	lane := lanes[0]
	if lane.NetPence != 1000 || lane.QuantityTons != 11 || lane.Samples != 2 || lane.GameDays != 0.375 || !lane.CompletedAt.Equal(loss.CompletedAt) {
		t.Fatalf("lane selected only winners, reused finite demand, or overstated per-ton net: %+v", lane)
	}
	for _, outcome := range []string{"arrived", "origin abort", "wrong destination", "future"} {
		probe := store.Settlement{Kind: "relocation", Origin: "le_havre", Destination: "amsterdam", Commodity: "sawn_planks",
			Lane: realizedRelocationKey("amsterdam", "portsmouth", "sawn_planks"), CompletedAt: in.Now.Add(-30 * time.Minute)}
		switch outcome {
		case "origin abort":
			probe.Destination = probe.Origin
		case "wrong destination":
			probe.Destination = "elsewhere"
		case "future":
			probe.CompletedAt = in.Now.Add(time.Second)
		}
		observed, err := recentRealizedSourcingLanes(append(rows, probe), in.Now)
		if err != nil || len(observed) != 1 || observed[0].RelocatedAt.IsZero() != (outcome != "arrived") {
			t.Fatalf("%s incorrectly consumed measured lane evidence: %+v error=%v", outcome, observed, err)
		}
	}
	base.AcquisitionPence, base.ProceedsPence, base.FeesPence = math.MaxInt64, math.MinInt64, 0
	if _, err := recentRealizedSourcingLanes([]store.Settlement{base}, in.Now); err == nil {
		t.Fatal("overflowing measured net authorized relocation")
	}
}

func TestRealizedRelocationRequiresPositiveNetAfterCostAndConservativeSizing(t *testing.T) {
	in, lane := realizedRelocationInput(t)
	baseline := realizedRelocationPlan(in, lane)
	if !baseline.Accepted {
		t.Fatal(baseline.Reason)
	}
	cost := -baseline.Economics.ExpectedNetPence
	for _, margin := range []int64{-1, 0, 1} {
		lane.NetPence = cost + margin
		plan := realizedRelocationPlan(in, lane)
		if plan.Accepted != (margin > 0) {
			t.Fatalf("cost=%d measured=%d admitted=%t reason=%s", cost, lane.NetPence, plan.Accepted, plan.Reason)
		}
	}
	lane.NetPence = 10_000
	for _, hold := range []int64{12, 24, 48} {
		in.Ship.EffectiveHoldCapacityTons = hold
		plan := realizedRelocationPlan(in, lane)
		want := int64(10_000) * min(hold, 24) / 24
		if !plan.Accepted || plan.Continuation.NetPence != want || plan.Quantity != 0 || plan.Economics.RevenuePence != 0 || plan.Economics.ExpectedNetPence >= 0 {
			t.Fatalf("historical net invented a larger lot or current income: hold=%d plan=%+v", hold, plan)
		}
	}
}

func TestRealizedRelocationPreservesEvidenceRouteCashAndIdleGuards(t *testing.T) {
	for _, guard := range []string{"idle", "expired", "future", "used evidence", "locked", "locked continuation", "outward hull", "continuation hull", "missing continuation", "stale continuation", "unfunded", "budget", "unknown cost", "occupied"} {
		t.Run(guard, func(t *testing.T) {
			in, lane := realizedRelocationInput(t)
			blocked := map[string]string{}
			switch guard {
			case "idle":
				in.SourcingIdleEligible = false
			case "expired":
				lane.CompletedAt = in.Now.Add(-73 * time.Hour)
			case "future":
				lane.CompletedAt = in.Now.Add(time.Second)
			case "used evidence":
				lane.RelocatedAt = lane.CompletedAt
			case "locked":
				in.Catalog.Ports[1].Phase = 2
			case "locked continuation":
				in.Catalog.Ports[2].Phase = 2
			case "outward hull", "continuation hull":
				key := routeKey("le_havre", "amsterdam", "hoy")
				if guard == "continuation hull" {
					key = routeKey("amsterdam", "portsmouth", "hoy")
				}
				route := in.Routes[key]
				route.Legs[0].EdgeClass = "oceanic"
				in.Routes[key] = route
			case "missing continuation":
				delete(in.Routes, routeKey("amsterdam", "portsmouth", "hoy"))
			case "stale continuation":
				delete(in.Metadata, "routes/"+routeKey("amsterdam", "portsmouth", "hoy"))
			case "unfunded":
				in.Balances["channel"] = in.ReserveByRegion["channel"]
			case "budget":
				zero := int64(0)
				in.SourcingAllowancePence = &zero
			case "unknown cost":
				in.SourcingProbeCostsUnknown = true
			case "occupied":
				blocked[lane.Origin] = "our ship already targets this port"
			}
			in.SourcingProbeHistory.RealizedLanes = []realizedSourcingLane{lane}
			plans := realizedRelocationPlans(in, blocked)
			if len(plans) != 1 || plans[0].Accepted || plans[0].Reason == "" {
				t.Fatalf("%s failed open: %+v", guard, plans)
			}
		})
	}
}

func TestRealizedRelocationRanksMeasuredNetMinusRelocationPerGameDay(t *testing.T) {
	in, lane := realizedRelocationInput(t)
	faster := lane
	faster.Origin, faster.Destination, faster.GameDays = "portsmouth", "amsterdam", 0.1
	faster.NetPence = lane.NetPence - 100
	in.SourcingProbeHistory.RealizedLanes = []realizedSourcingLane{lane, faster}
	plans := realizedRelocationPlans(in, nil)
	if len(plans) != 2 || !plans[0].Accepted || plans[0].Destination != faster.Origin {
		t.Fatalf("did not rank measured net after expense and sailing duration: %+v", plans)
	}
}

func TestScoutDailyFloorCanFundOneMeasuredRelocationWithoutConversion(t *testing.T) {
	in := bookRefreshInput(t)
	in.Balances["north"] = 100_000_000
	target := in.Ports["amsterdam"]
	target.BerthDuesPerDayPence = 600_000
	in.Ports["amsterdam"] = target
	plan := scoutAcceptancePlan(t, bookRefreshCandidates(in, nil), "amsterdam")
	cost := -plan.Economics.ExpectedNetPence
	if cost < 25_000 || cost > 60_000 || !plan.Accepted {
		t.Fatalf("unconverted daily floor did not cover a 250-600 Cr measured relocation: cost=%d plan=%+v", cost, plan)
	}
	in.SourcingProbeHistory.ScoutExpensePence = 100_000 - cost
	if plan := scoutAcceptancePlan(t, bookRefreshCandidates(in, nil), "amsterdam"); !plan.Accepted {
		t.Fatalf("exact remaining allowance rejected relocation: %+v", plan)
	}
	in.SourcingProbeHistory.ScoutExpensePence++
	if plan := scoutAcceptancePlan(t, bookRefreshCandidates(in, nil), "amsterdam"); plan.Accepted {
		t.Fatal("daily floor ignored already-spent expense")
	}
}

func realizedRelocationRunner(t *testing.T) (*recoveryHarness, *Runner) {
	t.Helper()
	h, runner := scoutAcceptanceRunner(t)
	now := time.Now().UTC()
	putProbeHistorySettlement(t, h, store.Settlement{ID: "measured-spot", Kind: "spot", ShipID: recoveryShipLapras,
		Origin: "amsterdam", Destination: "hamburg", Commodity: "copper_sheathing", QuantityTons: 30, GameSeconds: 21_600,
		ProceedsPence: 60_000, AcquisitionPence: 10_000, CompletedAt: now.Add(-48 * time.Hour)})
	expense := int64(200_001)
	putProbeHistorySettlement(t, h, store.Settlement{ID: "daily-scout-spent", Kind: "relocation", ShipID: recoveryShipLapras,
		Origin: "elsewhere", Destination: "somewhere", Commodity: "provisions", Lane: "somewhere|elsewhere|provisions|scout",
		ExplorationExpensePence: &expense, CompletedAt: now.Add(-time.Hour)})
	runner.refreshSourcingHistory(h.ctx, runner.planningInput(ShipState{}))
	runner.refreshSourcingBudget(h.ctx, runner.planningInput(ShipState{}))
	in, err := runner.sourcingInput(h.ctx, harnessShip(t, runner, recoveryShipBlastoise))
	if err != nil || scoutAllowance(in) != 0 || len(in.SourcingProbeHistory.RealizedLanes) != 1 {
		t.Fatalf("fixture did not recover two-day-old measured income independently of exhausted scouting: input=%+v error=%v", in.SourcingProbeHistory, err)
	}
	return h, runner
}

func TestRealizedRelocationJournalsOneDepartureAndSettlesOnlyActualExpense(t *testing.T) {
	h, runner := realizedRelocationRunner(t)
	mission, data := scoutAcceptanceAdmit(t, h, runner)
	lane := realizedRelocationKey("amsterdam", "hamburg", "copper_sheathing")
	if !isRealizedRelocation(data.Plan) || data.ProvenLane != lane || data.Relocation.TargetDestination != "hamburg" ||
		data.Relocation.ExpenseCapPence > data.Plan.Continuation.NetPence-1 || mission.Phase != "sourcing_fund" {
		t.Fatalf("measured relocation did not persist its capped evidence before spending: %+v %+v", mission, data)
	}
	changed, reason, err := runner.considerSourcing(h.ctx, harnessShip(t, runner, recoveryShipBlastoise))
	if err != nil || changed || !strings.Contains(reason, "ship already has a durable mission") {
		t.Fatalf("a second relocation replaced this hull's mission: changed=%t reason=%q error=%v", changed, reason, err)
	}
	decisions, err := h.db.History(h.ctx, h.account.ID, "decisions", 500)
	if err != nil {
		t.Fatal(err)
	}
	details := 0
	for _, decision := range decisions.([]store.Decision) {
		if decision.ShipID == recoveryShipBlastoise && strings.HasPrefix(decision.Kind, "sourcing:") {
			details++
		}
	}
	if details != 1 {
		t.Fatalf("one measured candidate wrote %d diagnostic details", details)
	}
	// A new executor reloads the durable mission; authority remains the same
	// observation, not freshly invented private books at the target.
	restarted := h.runner(primaryTestKey)
	restarted.snapshot, restarted.meta = runner.snapshot, runner.meta
	restarted.stateCache, restarted.generation = runner.stateCache, runner.generation
	restarted.refreshSourcingHistory(h.ctx, restarted.planningInput(ShipState{}))
	restarted.refreshSourcingBudget(h.ctx, restarted.planningInput(ShipState{}))
	runner = restarted
	scoutAcceptanceServeDeparture(t, h, runner)
	for range 10 {
		mission, data = h.mission(recoveryShipBlastoise)
		if _, err := runner.driveMission(h.ctx, harnessShip(t, runner, recoveryShipBlastoise), mission, &data); err != nil {
			t.Fatal(err)
		}
		if h.api.mutationCount("/v1/fleet/depart") > 0 {
			break
		}
	}
	calls := h.api.mutationsFor("/v1/fleet/depart")
	if len(calls) != 1 || calls[0].IdempotencyKey == "" {
		t.Fatalf("durable measured relocation did not depart exactly once: calls=%+v holds=%+v", calls, runner.holding)
	}
	var sent struct {
		Destination string `json:"dest_port_id"`
	}
	if json.Unmarshal(calls[0].Body, &sent) != nil || sent.Destination != "amsterdam" {
		t.Fatalf("did not sail toward measured profitable origin: %s", calls[0].Body)
	}
	for i := range runner.stateCache.Ships {
		ship := &runner.stateCache.Ships[i]
		if ship.Ship.ID == recoveryShipBlastoise {
			ship.Ship.CurrentPortID, ship.Ship.Status = "amsterdam", "anchored_roadstead"
			ship.Voyage = nil
			ship.Stores.ProvisionsTons -= 0.1
		}
	}
	runner.stateCache.Clock.Calendar.TotalGameHours++
	for range 2 {
		mission, data = h.mission(recoveryShipBlastoise)
		if _, err := runner.driveMission(h.ctx, harnessShip(t, runner, recoveryShipBlastoise), mission, &data); err != nil {
			t.Fatal(err)
		}
	}
	rows := destinationSettlements(t, h, lane)
	if len(rows) != 1 || rows[0].Kind != "relocation" || rows[0].QuantityTons != 0 || rows[0].ProceedsPence != 0 || rows[0].ExplorationExpensePence == nil || *rows[0].ExplorationExpensePence > 20_000 {
		t.Fatalf("arrival reused historical revenue or lost expense accounting: %+v", rows)
	}
	if h.api.mutationCount("/v1/market/place-order") != 0 || h.api.mutationCount("/v1/fleet/depart") != 1 {
		t.Fatal("historical lane bought speculative cargo or forced an empty return")
	}
	in, err := runner.sourcingInput(h.ctx, harnessShip(t, runner, recoveryShipCharizard))
	if err != nil {
		t.Fatal(err)
	}
	in.SourcingIdleEligible = true
	if err := runner.loadRollingSourcingBudget(h.ctx, &in); err != nil {
		t.Fatal(err)
	}
	plans := realizedRelocationPlans(in, nil)
	if len(plans) != 1 || plans[0].Accepted || !strings.Contains(plans[0].Reason, "no newer settlement") {
		t.Fatalf("same historical evidence authorized a second relocation: %+v", plans)
	}
	putProbeHistorySettlement(t, h, store.Settlement{ID: "new-converted-spot", Kind: "spot", ShipID: recoveryShipLapras,
		Origin: "amsterdam", Destination: "hamburg", Commodity: "copper_sheathing", QuantityTons: 30, GameSeconds: 21_600,
		ProceedsPence: 60_000, AcquisitionPence: 10_000, CompletedAt: time.Now().UTC()})
	in.Now = time.Now().UTC()
	if err := runner.loadRollingSourcingBudget(h.ctx, &in); err != nil {
		t.Fatal(err)
	}
	if plans := realizedRelocationPlans(in, nil); len(plans) != 1 || !plans[0].Accepted {
		t.Fatalf("newer measured income did not rearm the lane: %+v", plans)
	}
}

func TestRealizedRelocationRechecksMeasuredNetBeforeDeparture(t *testing.T) {
	for _, change := range []string{"loss", "occupied"} {
		t.Run(change, func(t *testing.T) {
			h, runner := realizedRelocationRunner(t)
			mission, data := scoutAcceptanceAdmit(t, h, runner)
			mission.Phase = "depart"
			if err := runner.saveMission(h.ctx, mission, &data); err != nil {
				t.Fatal(err)
			}
			if change == "loss" {
				putProbeHistorySettlement(t, h, store.Settlement{ID: "new-realized-loss", Kind: "spot", ShipID: recoveryShipLapras,
					Origin: "amsterdam", Destination: "hamburg", Commodity: "copper_sheathing", QuantityTons: 30, GameSeconds: 21_600,
					AcquisitionPence: 100_000, CompletedAt: time.Now().UTC().Add(-time.Minute)})
			} else {
				runner.stateCache.Ships[0].Ship.CurrentPortID = "amsterdam"
			}
			for range 3 {
				mission, data, err := runner.loadMission(h.ctx, recoveryShipBlastoise)
				if err != nil {
					t.Fatal(err)
				}
				if mission == nil {
					break
				}
				if _, err := runner.driveMission(h.ctx, harnessShip(t, runner, recoveryShipBlastoise), mission, &data); err != nil {
					t.Fatal(err)
				}
			}
			h.missingMission(recoveryShipBlastoise)
			if h.api.mutationCount("/v1/fleet/depart") != 0 {
				t.Fatal("invalidated measured relocation departed")
			}
		})
	}
}

func TestRealizedRelocationShrinkingNetBoundsPaidExpense(t *testing.T) {
	h, runner := realizedRelocationRunner(t)
	_, data := scoutAcceptanceAdmit(t, h, runner)
	data.FeesPence = 8_000
	putProbeHistorySettlement(t, h, store.Settlement{ID: "smaller-realized-mean", Kind: "spot", ShipID: recoveryShipLapras,
		Origin: "amsterdam", Destination: "hamburg", Commodity: "copper_sheathing", QuantityTons: 30, GameSeconds: 21_600,
		AcquisitionPence: 30_000, CompletedAt: time.Now().UTC().Add(-time.Minute)})
	// The new 10,000p lane mean still covers remaining modeled sailing cost,
	// but cannot cover 8,000p already paid plus a further 3,000p mutation.
	if err := runner.relocationSpendingReady(h.ctx, harnessShip(t, runner, recoveryShipBlastoise), &data, 3_000); err == nil {
		t.Fatal("fresh measured net bounded only future cost, not paid expense and new spending")
	}
}

func TestRealizedRelocationCapturesBoundedRemoteContinuationRoutes(t *testing.T) {
	w := newIdleFallbackWorld(t)
	runner := w.h.runner(primaryTestKey)
	w.observe(t, runner, 100)
	for index, source := range []string{"amsterdam", "rouen", "calais", "ostend"} {
		putProbeHistorySettlement(t, w.h, store.Settlement{ID: "measured-" + source, Kind: "spot", ShipID: recoveryShipLapras,
			Origin: source, Destination: recoveryFarPort, Commodity: "sawn_planks", QuantityTons: 10, GameSeconds: 21_600,
			ProceedsPence: int64(100_000 - index*10_000), CompletedAt: time.Now().UTC().Add(-time.Hour)})
	}
	var gets atomic.Int64
	base := w.h.api.server.Config.Handler
	w.h.api.server.Config.Handler = http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		if request.URL.Path == "/v1/world/route" && request.URL.Query().Get("from") != recoveryHomePort && request.URL.Query().Get("to") != recoveryHomePort {
			gets.Add(1)
		}
		if request.URL.Path == "/v1/contract/list" {
			w.h.api.writeJSON(writer, http.StatusOK, []Contract{})
			return
		}
		base.ServeHTTP(writer, request)
	})
	// Remove previously captured charter routes. Measured origins must request
	// their own continuation geometry through the existing inspection budget.
	routes := decode[map[string]json.RawMessage](runner.snapshot, "routes")
	for key := range routes {
		if !strings.HasPrefix(key, recoveryHomePort+"|") && !strings.Contains(key, "|"+recoveryHomePort+"|") {
			delete(routes, key)
		}
	}
	runner.snapshot["routes"] = encode(routes)
	w.observe(t, runner, 106)
	in := w.input(t, runner, recoveryShipBlastoise)
	plans := realizedRelocationPlans(in, nil)
	if len(plans) == 0 || !plans[0].Accepted || plans[0].Destination != "amsterdam" || gets.Load() != 6 {
		t.Fatalf("measured relocation did not inspect at most three source pairs: gets=%d plans=%+v", gets.Load(), plans)
	}
}
