package engine

import (
	"encoding/json"
	"fmt"
	"io"
	"math"
	"net/http"
	"testing"
	"time"

	"github.com/DevYukine/go-tradewinds/internal/store"
	"github.com/google/uuid"
)

func TestGrowthReservesWholeOutstandingCharterLotBeforeRepeatDemand(t *testing.T) {
	t.Parallel()
	h := newRecoveryHarness(t)
	h.api.setFactorPorts(recoveryHomePort, recoveryFarPort)
	runner := h.runner(primaryTestKey)
	runner.generation = uuid.NewString()
	if err := runner.refresh(h.ctx); err != nil {
		t.Fatalf("refresh growth state: %v", err)
	}
	state := &runner.stateCache
	state.Catalog.ShipClasses[0].HoldCapacityTons = 32
	state.Catalog.ShipClasses[0].BaseSpeedCentiknots = 750
	for key, route := range state.Routes {
		route.TotalDistanceNM = 63
		state.Routes[key] = route
	}
	destination := state.Ports[recoveryFarPort]
	destination.EffectiveBerths = 9
	state.Ports[recoveryFarPort] = destination
	market := state.Markets[recoveryFarPort]
	bookCommodity(market, recoveryGrain).Depth.Bids = nil
	state.Markets[recoveryFarPort] = market
	reward := int64(10_000)
	state.Contracts = []Contract{{
		ID: "finite-charter", ContractType: "charter", OriginPortID: recoveryFarPort,
		CommodityID: recoveryGrain, QuantityRequired: 100, RemainingQuantity: 100,
		RewardPerTonPence: &reward, Status: "open", IsPublic: true,
	}}
	for i := range 2 {
		id := fmt.Sprintf("whole-lot-proof-%d", i)
		if _, err := h.db.BeginAction(h.ctx, &store.Action{
			ID: id, AccountID: h.account.ID, Method: http.MethodPost,
			Path: "/v1/contract/deliver-charter",
			Body: encode(map[string]any{"contract_id": "finite-charter", "ship_id": recoveryShipBlastoise, "quantity": 20}),
		}); err != nil {
			t.Fatalf("begin charter evidence: %v", err)
		}
		if err := h.db.CompleteAction(h.ctx, id, http.StatusOK, json.RawMessage(`{"delivered_quantity":20,"reward_pence":110000}`), nil); err != nil {
			t.Fatalf("complete charter evidence: %v", err)
		}
		if err := h.db.PutSettlement(h.ctx, &store.Settlement{
			ID: id, AccountID: h.account.ID, ShipID: recoveryShipBlastoise, MissionID: id, ActionID: id,
			Lane: laneKey(Plan{Kind: "charter", Origin: recoveryHomePort, Destination: recoveryFarPort, Commodity: recoveryGrain}),
			Kind: "charter", Origin: recoveryHomePort, Destination: recoveryFarPort, Commodity: recoveryGrain,
			QuantityTons: 20, GameSeconds: 86_400, CostsKnown: true,
			AcquisitionPence: 10_000, ProceedsPence: 110_000,
			CompletedAt: time.Now().UTC().Add(-time.Hour),
		}); err != nil {
			t.Fatalf("persist charter proof: %v", err)
		}
	}
	if changed, err := runner.considerGrowth(h.ctx); err != nil || !changed {
		t.Fatalf("unallocated 100t charter must support the proven candidate: changed %t error %v; holds %v", changed, err, runner.holding)
	}
	_, control := growthMissionWithHoldDiagnostics(t, h, runner)
	if control.Plan.Quantity != 20 || control.Plan.Economics.GameDays <= 0 ||
		control.Plan.Economics.BallastRepeatGameDays <= control.Plan.Economics.GameDays {
		t.Fatalf("fixture must offer a 20t candidate with an explicitly costed repeat, got %+v", control.Plan)
	}
	if err := h.db.DeleteMission(h.ctx, h.account.ID, "__growth"); err != nil {
		t.Fatalf("release control build intent: %v", err)
	}

	incumbent := control.Plan
	incumbent.ShipID = recoveryShipBlastoise
	incumbent.Quantity = 100
	incumbent.Economics.GameDays = 10
	incumbent.Economics.BallastRepeatGameDays = 10
	h.putMission(recoveryShipBlastoise, "whole-outstanding-lot", "sailing", missionData{Plan: incumbent, CostsKnown: true})
	state.Catalog.ShipClasses = append(state.Catalog.ShipClasses, Hull{ID: "incumbent-barge", HoldCapacityTons: 120, CrewRequired: 10})
	state.Ships[0].Ship.ShipClass = "incumbent-barge"
	state.Ships[0].HoldCapacityTons = 120
	state.Ships[0].EffectiveHoldCapacityTons = 116
	state.Ships[0].CargoUsedTons = 100
	state.Ships[0].CargoManifest = map[string]int64{recoveryGrain: 100}
	if _, err := runner.considerGrowth(h.ctx); err != nil {
		t.Fatalf("allocate committed charter lot: %v", err)
	}
	h.missingMission("__growth")
	if h.api.mutationCount("/v1/shipyard/build-ship") != 0 {
		t.Fatal("growth spent money against a charter the incumbent can exhaust on its next arrival")
	}
	_, retained := h.mission(recoveryShipBlastoise)
	if retained.Plan.Quantity != 100 || retained.Plan.Economics.GameDays != 10 {
		t.Fatalf("growth rejection rewrote the outstanding delivery commitment: %+v", retained.Plan)
	}

	incumbent.Kind = "charter+freight"
	incumbent.FreightIDs = []string{"sealed-freight"}
	incumbent.Economics.GameDays = 100
	incumbent.Economics.BallastRepeatGameDays = 100
	state.Catalog.ShipClasses[1].HoldCapacityTons = 240
	state.Ships[0].Ship.Status = "berthed"
	state.Ships[0].Ship.CurrentPortID = recoveryFarPort
	state.Ships[0].HoldCapacityTons = 240
	state.Ships[0].EffectiveHoldCapacityTons = 236
	mixed := missionData{
		Plan: incumbent, BoughtQuantity: 100, DeliveredQuantity: 100, CostsKnown: true,
		Freight: []freightObligation{{ContractID: "sealed-freight", Commodity: recoveryGrain, Quantity: 100, Destination: recoveryFarPort, Settled: true}},
	}
	h.putMission(recoveryShipBlastoise, "freight-before-charter", "arrival", mixed)
	if changed, err := runner.considerGrowth(h.ctx); err != nil || changed {
		t.Fatalf("sealed freight delivery released the ordinary charter reservation: changed %t error %v", changed, err)
	}
	h.missingMission("__growth")
	_, retained = h.mission(recoveryShipBlastoise)
	if retained.DeliveredQuantity != 100 || len(retained.Freight) != 1 || !retained.Freight[0].Settled || retained.Freight[0].Quantity != 100 {
		t.Fatalf("growth changed durable freight settlement accounting: %+v", retained)
	}

	state.Ships[0].CargoUsedTons = 10
	state.Ships[0].CargoManifest[recoveryGrain] = 10
	for _, legacy := range []bool{false, true} {
		mixed.DeliveredQuantity = 190
		if legacy {
			mixed.DeliveredQuantity = 90
			mixed.Freight = nil
		}
		h.putMission(recoveryShipBlastoise, "partially-delivered-charter", "arrival", mixed)
		if changed, err := runner.considerGrowth(h.ctx); err != nil || !changed {
			t.Fatalf("partial ordinary delivery was not released from charter capacity (legacy %t): changed %t error %v holds %v", legacy, changed, err, runner.holding)
		}
		mission, data := growthMissionWithHoldDiagnostics(t, h, runner)
		if mission.Phase != "growth_build" || data.Action == nil || data.Action.Stage != "build" {
			t.Fatalf("partial charter capacity did not stage a real build intent: phase %s action %+v", mission.Phase, data.Action)
		}
		if err := h.db.DeleteMission(h.ctx, h.account.ID, "__growth"); err != nil {
			t.Fatalf("release partial-delivery build intent: %v", err)
		}
	}
}

func TestIdleUnderarmedShipRefitsBeforeEconomicPlanAndPurchase(t *testing.T) {
	t.Parallel()
	h := newRecoveryHarness(t)
	h.api.setFactorPorts(recoveryFarPort)
	idle := sailingShipRow(recoveryShipBlastoise, "Blastoise", recoveryHomePort, recoveryFarPort)
	idle["ship"].(map[string]any)["status"] = "berthed"
	idle["ship"].(map[string]any)["cannons_count"] = 0
	idle["effective_hold_capacity_tons"] = 60
	idle["stores"] = map[string]any{"provisions_tons": 4}
	idle["route_legs"] = []any{}
	idle["voyage"] = nil
	h.api.setFleet([]map[string]any{idle, sailingShipRow(recoveryShipCharizard, "Charizard", recoveryFarPort, recoveryHomePort)})
	h.api.setVaultBalance("north", 200_000)
	downstream := h.api.server.Config.Handler
	h.api.server.Config.Handler = http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		if request.Method != http.MethodPost || request.URL.Path != "/v1/shipyard/modify-ship" {
			downstream.ServeHTTP(writer, request)
			return
		}
		body, err := io.ReadAll(request.Body)
		var payload struct {
			ShipID string `json:"ship_id"`
			Target int64  `json:"target_cannons_count"`
		}
		if err != nil || json.Unmarshal(body, &payload) != nil {
			h.api.writeProblem(writer, http.StatusBadRequest, "bad_refit", "invalid refit request")
			return
		}
		h.api.mu.Lock()
		defer h.api.mu.Unlock()
		ship := idle["ship"].(map[string]any)
		cost := (payload.Target - int64(ship["cannons_count"].(int))) * 15_000
		if payload.ShipID != recoveryShipBlastoise || payload.Target != 4 || cost <= 0 || h.api.vaultBalance["north"] < cost {
			h.api.record(request, body, http.StatusConflict)
			h.api.writeProblem(writer, http.StatusConflict, "unfunded_refit", "refit cannot be applied to the actual hull")
			return
		}
		h.api.vaultBalance["north"] -= cost
		ship["cannons_count"] = int(payload.Target)
		idle["effective_hold_capacity_tons"] = 60 - 2*int(payload.Target)
		h.api.record(request, body, http.StatusOK)
		h.api.writeJSON(writer, http.StatusOK, map[string]any{"ship_id": payload.ShipID, "cannons_count": payload.Target})
	})
	runner := h.runner(primaryTestKey)
	if _, err := h.tick(runner); err != nil {
		t.Fatalf("idle maintenance tick: %v", err)
	}
	mission, data := h.mission(recoveryShipBlastoise)
	if data.Plan.Kind != "maintenance" || data.Action == nil || data.Action.Stage != "refit" || mission.Quantity != 0 {
		t.Fatalf("unarmed ship received a pre-refit economic commitment: phase %s quantity %d data %+v", mission.Phase, mission.Quantity, data)
	}
	if _, err := h.tick(runner); err != nil {
		t.Fatalf("dispatch funded cannon installation: %v", err)
	}
	h.api.mu.Lock()
	afterRefit := h.api.vaultBalance["north"]
	h.api.mu.Unlock()
	if afterRefit != 200_000-4*15_000 || h.api.mutationCount("/v1/market/place-order") != 0 {
		t.Fatalf("refit did not precede cargo spending: cash %d mutations %+v", afterRefit, h.api.mutationsFor("/v1/market/place-order"))
	}
	// Once a voyage is committed, legitimate spare-work growth may run before
	// its purchase. Isolate procurement here, after the unguarded refit checks.
	runner.opts.CapacityPaybackDays = 0.000001
	for range 8 {
		if _, err := h.tick(runner); err != nil {
			t.Fatalf("plan and purchase after refit: %v", err)
		}
		if h.api.mutationCount("/v1/market/place-order") > 0 {
			break
		}
	}
	calls := h.api.mutationsFor("/v1/market/place-order")
	if len(calls) != 1 || calls[0].Status != http.StatusOK {
		t.Fatalf("post-refit plan did not produce one executable cargo purchase: %+v", calls)
	}
	_, data = h.mission(recoveryShipBlastoise)
	if data.Plan.Quantity <= 0 || data.Plan.Quantity > 60-2*4 || data.BoughtQuantity != data.Plan.Quantity {
		t.Fatalf("economic plan was not sized against the armed hull: %+v", data)
	}
	h.api.mu.Lock()
	afterPurchase := h.api.vaultBalance["north"]
	h.api.mu.Unlock()
	purchase := data.BoughtQuantity * 100
	customs := (purchase*200 + 9_999) / 10_000
	if afterPurchase != afterRefit-purchase-customs || afterPurchase < runner.opts.ReservePence {
		t.Fatalf("post-refit purchase failed cash/reserve identity: refit cash %d purchase %d customs %d remaining %d", afterRefit, purchase, customs, afterPurchase)
	}
}

func TestTreasuryCombinesPartialDonorsWithoutSpendingTheirReserves(t *testing.T) {
	t.Parallel()
	h := newRecoveryHarness(t)
	runner := h.runner(primaryTestKey)
	rates := []Rate{
		{FromRegion: "north", ToRegion: "yard", FeeBPS: 100},
		{FromRegion: "south", ToRegion: "yard", FeeBPS: 100},
	}
	h.api.setRates(rates...)
	required := map[string]int64{"north": 100_000, "south": 100_000, "yard": 150_000}
	h.api.setVaultBalance("north", 200_000)
	h.api.setVaultBalance("south", 200_000)
	h.api.setVaultBalance("yard", 0)
	balances := func() map[string]int64 {
		h.api.mu.Lock()
		defer h.api.mu.Unlock()
		return map[string]int64{"north": h.api.vaultBalance["north"], "south": h.api.vaultBalance["south"], "yard": h.api.vaultBalance["yard"]}
	}
	var fees int64
	donors := make(map[string]bool)
	for range 2 {
		runner.generation = uuid.NewString()
		if err := runner.refresh(h.ctx); err != nil {
			t.Fatalf("refresh observed funding rates and balances: %v", err)
		}
		before := balances()
		if changed, err := runner.fundTreasury(h.ctx, before, required); err != nil || !changed {
			t.Fatalf("aggregate surplus cannot fund yard sequentially: changed %t error %v balances %v holds %v", changed, err, before, runner.holding)
		}
		mission, data := h.mission("__treasury")
		if data.Action == nil || data.Action.Stage != "remit" {
			t.Fatalf("partial funding did not persist a remittance: %+v", data)
		}
		var request struct {
			From  string `json:"from_region"`
			To    string `json:"to_region"`
			Gross int64  `json:"gross_amount_pence"`
		}
		if err := json.Unmarshal(data.Action.Body, &request); err != nil {
			t.Fatalf("decode remittance intent: %v", err)
		}
		if request.To != "yard" || request.Gross <= 0 || request.Gross > before[request.From]-required[request.From] || donors[request.From] {
			t.Fatalf("remittance overspent a donor or failed to combine independent surpluses: %+v before %v", request, before)
		}
		donors[request.From] = true
		if _, err := runner.dispatch(h.ctx, mission, &data); err != nil {
			t.Fatalf("dispatch partial funding: %v", err)
		}
		after := balances()
		fee := request.Gross * 100 / 10_000
		fees += fee
		if after[request.From] != before[request.From]-request.Gross || after["yard"] != before["yard"]+request.Gross-fee || after[request.From] < required[request.From] {
			t.Fatalf("partial remittance violated cash/reserve identity: request %+v before %v after %v fee %d", request, before, after, fee)
		}
		if err := h.db.DeleteMission(h.ctx, h.account.ID, "__treasury"); err != nil {
			t.Fatalf("release completed remittance: %v", err)
		}
	}
	final := balances()
	if len(donors) != 2 || final["yard"] < required["yard"] || final["north"] < required["north"] || final["south"] < required["south"] || final["north"]+final["south"]+final["yard"] != 400_000-fees {
		t.Fatalf("two partial donors did not fund 150000p plus real fees while preserving reserves: balances %v fees %d", final, fees)
	}
	if changed, err := runner.fundTreasury(h.ctx, final, required); err != nil || changed {
		t.Fatalf("fully funded yard requested another transfer: changed %t error %v", changed, err)
	}
}

func TestGrowthChargesOutstandingCharterLotOnceAcrossPaybackCycles(t *testing.T) {
	t.Parallel()
	input := fundedGrowthFixture()
	price := int64(10_000)
	input.Quote.TotalCostPence = &price
	input.ExistingCycleTons = 10
	input.OutstandingContractTons = 30
	input.ContractRemainingTons = 90
	decision := EvaluateGrowth(input)
	if decision.PaybackGameDays <= 0 || decision.PaybackGameDays > 1 {
		t.Fatalf("fixture must repay within one day so two delivery cycles bind: %+v", decision)
	}
	if !decision.Accepted {
		t.Fatalf("90t must cover two 30t repeat lots plus one outstanding 30t delivery, not a repeated outstanding lot: %+v", decision)
	}
	input.ContractRemainingTons--
	if decision := EvaluateGrowth(input); decision.Accepted {
		t.Fatal("89t charter capacity covered 60t repeat demand plus the whole outstanding 30t lot")
	}
}

func TestGrowthMixedFreightKeepsOrdinarySpotBidReservation(t *testing.T) {
	t.Parallel()
	h := newRecoveryHarness(t)
	h.api.setFactorPorts(recoveryHomePort, recoveryFarPort)
	runner := h.runner(primaryTestKey)
	runner.generation = uuid.NewString()
	if err := runner.refresh(h.ctx); err != nil {
		t.Fatalf("refresh growth state: %v", err)
	}
	h.seedLaneProofs()
	if changed, err := runner.considerGrowth(h.ctx); err != nil || !changed {
		t.Fatalf("funded spot control did not stage growth: changed %t error %v holds %v", changed, err, runner.holding)
	}
	_, control := growthMissionWithHoldDiagnostics(t, h, runner)
	if err := h.db.DeleteMission(h.ctx, h.account.ID, "__growth"); err != nil {
		t.Fatalf("release control build intent: %v", err)
	}

	incumbent := control.Plan
	incumbent.ShipID = recoveryShipBlastoise
	incumbent.Kind = "spot+freight"
	incumbent.FreightIDs = []string{"sealed-freight"}
	incumbent.Quantity = 100
	incumbent.Economics.GameDays = 100 * control.Plan.Economics.GameDays
	incumbent.Economics.BallastRepeatGameDays = 100 * control.Plan.Economics.BallastRepeatGameDays
	state := &runner.stateCache
	state.Catalog.ShipClasses = append(state.Catalog.ShipClasses, Hull{ID: "incumbent-barge", HoldCapacityTons: 240, CrewRequired: 10})
	state.Ships[0].Ship.ShipClass = "incumbent-barge"
	state.Ships[0].Ship.Status = "berthed"
	state.Ships[0].Ship.CurrentPortID = recoveryFarPort
	state.Ships[0].HoldCapacityTons = 240
	state.Ships[0].EffectiveHoldCapacityTons = 236
	state.Ships[0].CargoUsedTons = 100
	state.Ships[0].CargoManifest = map[string]int64{recoveryGrain: 100}
	bids := bookCommodity(state.Markets[recoveryFarPort], recoveryGrain).Depth.Bids
	if len(bids) != 1 {
		t.Fatalf("spot fixture needs one executable bid level, got %+v", bids)
	}
	repeatDemand := 2 * (control.Plan.Quantity + 1)
	bids[0].RemainingQuantity = repeatDemand + 99
	mixed := missionData{
		Plan: incumbent, BoughtQuantity: 100, DeliveredQuantity: 100, CostsKnown: true,
		Freight: []freightObligation{{ContractID: "sealed-freight", Commodity: recoveryGrain, Quantity: 100, Destination: recoveryFarPort, Settled: true}},
	}
	h.putMission(recoveryShipBlastoise, "freight-before-spot", "arrival", mixed)
	if changed, err := runner.considerGrowth(h.ctx); err != nil || !changed {
		t.Fatalf("one missing bid ton must permit a smaller funded package without releasing the ordinary lot: changed %t error %v", changed, err)
	}
	_, smaller := growthMissionWithHoldDiagnostics(t, h, runner)
	if smaller.Plan.Quantity >= control.Plan.Quantity ||
		2*(smaller.Plan.Quantity+1)+100 > bids[0].RemainingQuantity {
		t.Fatalf("mixed freight released ordinary outstanding bids instead of reducing the package: %+v", smaller.Plan)
	}
	if err := h.db.DeleteMission(h.ctx, h.account.ID, "__growth"); err != nil {
		t.Fatal(err)
	}
	_, retained := h.mission(recoveryShipBlastoise)
	if retained.DeliveredQuantity != 100 || len(retained.Freight) != 1 || retained.Freight[0].Quantity != 100 || !retained.Freight[0].Settled {
		t.Fatalf("growth changed mixed delivery accounting: %+v", retained)
	}

	mixed.DeliveredQuantity = 110
	state.Ships[0].CargoUsedTons = 90
	state.Ships[0].CargoManifest[recoveryGrain] = 90
	h.putMission(recoveryShipBlastoise, "partially-delivered-spot", "arrival", mixed)
	bids[0].RemainingQuantity = repeatDemand + 90
	if changed, err := runner.considerGrowth(h.ctx); err != nil || !changed {
		t.Fatalf("exact bid capacity did not cover repeat demand plus the remaining ordinary lot once: changed %t error %v holds %v", changed, err, runner.holding)
	}
	_, partial := growthMissionWithHoldDiagnostics(t, h, runner)
	if partial.Plan.Quantity != control.Plan.Quantity {
		t.Fatalf("bid boundary changed the candidate lot: before %d after %d", control.Plan.Quantity, partial.Plan.Quantity)
	}
	if err := h.db.DeleteMission(h.ctx, h.account.ID, "__growth"); err != nil {
		t.Fatalf("release partial-delivery build intent: %v", err)
	}
	bids[0].RemainingQuantity--
	if changed, err := runner.considerGrowth(h.ctx); err != nil || !changed {
		t.Fatalf("one missing bid ton should resize the best funded package: changed %t error %v", changed, err)
	}
	_, smaller = growthMissionWithHoldDiagnostics(t, h, runner)
	if smaller.Plan.Quantity >= control.Plan.Quantity ||
		2*(smaller.Plan.Quantity+1)+90 > bids[0].RemainingQuantity {
		t.Fatalf("partial package exceeded the remaining bid pool: %+v", smaller.Plan)
	}
	if err := h.db.DeleteMission(h.ctx, h.account.ID, "__growth"); err != nil {
		t.Fatal(err)
	}
	bids[0].RemainingQuantity = 90 + 2
	if changed, err := runner.considerGrowth(h.ctx); err != nil || changed {
		t.Fatalf("ordinary outstanding lot and incumbent cycles leave no candidate bid capacity: changed %t error %v", changed, err)
	}
	h.missingMission("__growth")
}

func TestGrowthIdleUnderarmedShipWithoutActionableRefitDoesNotVetoBuild(t *testing.T) {
	t.Parallel()
	for _, yard := range []bool{false, true} {
		t.Run(fmt.Sprintf("shipyard_%t", yard), func(t *testing.T) {
			h := newRecoveryHarness(t)
			h.api.setFactorPorts(recoveryHomePort, recoveryFarPort)
			idle := sailingShipRow(recoveryShipBlastoise, "Blastoise", recoveryFarPort, recoveryHomePort)
			idle["ship"].(map[string]any)["status"] = "berthed"
			idle["ship"].(map[string]any)["cannons_count"] = 0
			idle["effective_hold_capacity_tons"] = 60
			idle["stores"] = map[string]any{"provisions_tons": 4}
			idle["route_legs"] = []any{}
			idle["voyage"] = nil
			h.api.setFleet([]map[string]any{idle, sailingShipRow(recoveryShipCharizard, "Charizard", recoveryHomePort, recoveryFarPort)})
			h.api.setVaultBalance("south", 110_000)
			runner := h.runner(primaryTestKey)
			runner.generation = uuid.NewString()
			if err := runner.refresh(h.ctx); err != nil {
				t.Fatalf("refresh idle growth state: %v", err)
			}
			for i := range runner.stateCache.Catalog.Ports {
				if runner.stateCache.Catalog.Ports[i].ID == recoveryFarPort {
					runner.stateCache.Catalog.Ports[i].HasShipyard = yard
				}
			}
			for _, plan := range PlanVoyages(runner.planningInput(runner.stateCache.Ships[0])) {
				if plan.Accepted {
					t.Fatalf("idle no-work fixture unexpectedly has a profitable voyage: %+v", plan)
				}
			}
			h.seedLaneProofs()
			if changed, err := runner.considerGrowth(h.ctx); err != nil || !changed {
				t.Fatalf("idle underarmed ship vetoed a funded profitable build: changed %t error %v holds %v", changed, err, runner.holding)
			}
			mission, data := growthMissionWithHoldDiagnostics(t, h, runner)
			if mission.Phase != "growth_build" || data.Action == nil || data.Action.Stage != "build" || data.BuildPort != recoveryHomePort {
				t.Fatalf("viable yard did not receive a durable build intent: phase %s data %+v", mission.Phase, data)
			}
			h.missingMission(recoveryShipBlastoise)
			if h.api.mutationCount("/v1/market/place-order") != 0 || h.api.mutationCount("/v1/shipyard/modify-ship") != 0 || h.api.mutationCount("/v1/shipyard/build-ship") != 0 {
				t.Fatal("growth consideration sent a mutation instead of staging the build")
			}
		})
	}
}

func TestGrowthChoosesCapitalReturnAcrossHullCatalogOrders(t *testing.T) {
	t.Parallel()
	for _, reverse := range []bool{false, true} {
		t.Run(fmt.Sprintf("reverse_%t", reverse), func(t *testing.T) {
			h := newRecoveryHarness(t)
			h.seedLaneProofs()
			runner := h.runner(primaryTestKey)
			runner.generation = uuid.NewString()
			if err := runner.refresh(h.ctx); err != nil {
				t.Fatal(err)
			}
			state := &runner.stateCache
			original := state.Catalog.ShipClasses[0]
			efficient := original
			efficient.ID = "capital-efficient-cutter"
			state.Catalog.ShipClasses = []Hull{original, efficient}
			if reverse {
				state.Catalog.ShipClasses[0], state.Catalog.ShipClasses[1] = state.Catalog.ShipClasses[1], state.Catalog.ShipClasses[0]
			}
			price := int64(20_000)
			for _, origin := range state.Catalog.Ports {
				oldQuoteKey := origin.ID + "|" + original.ID
				if quote, exists := state.Quotes[oldQuoteKey]; exists {
					key := origin.ID + "|" + efficient.ID
					quote.ShipClass = efficient.ID
					quote.TotalCostPence = &price
					state.Quotes[key] = quote
					runner.meta["yard_quotes/"+key] = runner.meta["yard_quotes/"+oldQuoteKey]
				}
				for _, destination := range state.Catalog.Ports {
					oldRouteKey := routeKey(origin.ID, destination.ID, original.ID)
					if route, exists := state.Routes[oldRouteKey]; exists {
						key := routeKey(origin.ID, destination.ID, efficient.ID)
						route.ShipClass = efficient.ID
						state.Routes[key] = route
						runner.meta["routes/"+key] = runner.meta["routes/"+oldRouteKey]
					}
				}
			}
			if changed, err := runner.considerGrowth(h.ctx); err != nil || !changed {
				t.Fatalf("funded hull comparison did not stage a build: changed %t error %v holds %v", changed, err, runner.holding)
			}
			_, data := growthMissionWithHoldDiagnostics(t, h, runner)
			if data.BuildClass != efficient.ID || data.BuildPort != recoveryHomePort {
				t.Fatalf("catalog order selected the poorer capital return: %+v", data)
			}
			if h.api.mutationCount("/v1/shipyard/build-ship") != 0 {
				t.Fatal("capital comparison sent a build instead of staging")
			}
		})
	}
}

func TestGrowthFundsOneCycleAndInitialStoresOnce(t *testing.T) {
	t.Parallel()
	input := fundedGrowthFixture()
	input.Plan.Economics.Basis = "destination"
	input.Plan.Economics.PurchasePence = 10_000
	input.Plan.Economics.CustomsPence = 500
	input.Plan.Economics.ProvisionsPence = 100
	input.Plan.Economics.StoreCashPence = 5_000
	input.Plan.Economics.BallastRepeatGameDays = 1
	input.Plan.Economics.BallastRepeatNetPence = 100_000
	input.InitialStoresPence = 5_000
	input.BalancePence = 283_500
	decision := EvaluateGrowth(input)
	if !decision.Accepted || decision.WorkingCapitalPence != 10_500 || decision.InitialStoresPence != 5_000 {
		t.Fatalf("initial store cash was charged again as cycle expense: %+v", decision)
	}
	input.BalancePence--
	if decision := EvaluateGrowth(input); decision.Accepted {
		t.Fatalf("one-penny short commissioning package spent protected obligations: %+v", decision)
	}
}

func TestGrowthReservesFullRepeatUpkeepAndRejectsOverflow(t *testing.T) {
	t.Parallel()
	input := fundedGrowthFixture()
	input.Plan.Economics.Basis = "destination"
	input.Plan.Economics.BallastRepeatGameDays = 5
	input.Plan.Economics.BallastRepeatNetPence = 500_000
	decision := EvaluateGrowth(input)
	if !decision.Accepted || decision.UpkeepReservePence != 5_000 {
		t.Fatalf("five-day supported repeat lacked a full upkeep cash horizon: %+v", decision)
	}
	input.Proofs = []LaneProof{
		{QuantityTons: 20, GameSeconds: 86_400, NetPence: math.MaxInt64, CostsKnown: true},
		{QuantityTons: 20, GameSeconds: 86_400, NetPence: math.MaxInt64, CostsKnown: true},
	}
	if decision := EvaluateGrowth(input); decision.Accepted {
		t.Fatal("overflowing public proof slice became positive growth evidence")
	}
}

func TestGrowthPricesMarginalRepeatInsteadOfCloningCheapFirstStock(t *testing.T) {
	t.Parallel()
	h := newRecoveryHarness(t)
	h.seedLaneProofs()
	runner := h.runner(primaryTestKey)
	runner.generation = uuid.NewString()
	if err := runner.refresh(h.ctx); err != nil {
		t.Fatal(err)
	}
	state := &runner.stateCache
	asks := bookCommodity(state.Markets[recoveryHomePort], recoveryGrain)
	asks.Depth.Asks = []Level{
		{OrderID: "cheap-twenty", PricePence: 100, RemainingQuantity: 20},
		{OrderID: "deep-expensive", PricePence: 900, RemainingQuantity: 10_000},
	}
	bookCommodity(state.Markets[recoveryFarPort], recoveryGrain).Depth.Bids[0].PricePence = 1_000
	key := recoveryHomePort + "|" + recoveryHull
	quote := state.Quotes[key]
	price := int64(20_000)
	quote.TotalCostPence = &price
	state.Quotes[key] = quote
	if changed, err := runner.considerGrowth(h.ctx); err != nil || !changed {
		t.Fatalf("a funded marginal partial lot did not stage: changed %t error %v holds %v", changed, err, runner.holding)
	}
	_, data := growthMissionWithHoldDiagnostics(t, h, runner)
	if data.Plan.Quantity <= 0 || data.Plan.Quantity >= 20 {
		t.Fatalf("growth repeated the cheap first 20t rather than pricing the next supported lot: %+v", data.Plan)
	}
	cheapRemaining := max(int64(0), 20-data.Plan.Quantity)
	cheapTons := min(data.Plan.Quantity, cheapRemaining)
	marginalPurchase := cheapTons*100 + (data.Plan.Quantity-cheapTons)*900
	if data.Plan.Economics.PurchasePence != marginalPurchase {
		t.Fatalf("repeat purchase used the first ladder slice: got %d, marginal second lot %d", data.Plan.Economics.PurchasePence, marginalPurchase)
	}
	if asks.Depth.Asks[0].RemainingQuantity != 20 || asks.Depth.Asks[1].RemainingQuantity != 10_000 {
		t.Fatal("capital simulation depleted the authoritative source observation")
	}
}

func TestGrowthPackageChargesOnlyNewRegionalCommitments(t *testing.T) {
	t.Parallel()
	h := newRecoveryHarness(t)
	h.seedLaneProofs()
	runner := h.runner(primaryTestKey)
	runner.generation = uuid.NewString()
	if err := runner.refresh(h.ctx); err != nil {
		t.Fatal(err)
	}
	evidence := growthEvidence{workingByRegion: make(map[string]int64)}
	if reason, err := runner.growthFunding(h.ctx, &evidence); err != nil || reason != "" {
		t.Fatalf("funded package history unavailable: %s, %v", reason, err)
	}
	port, _ := findPort(runner.stateCache.Catalog, recoveryHomePort)
	hull, _ := findHull(runner.stateCache.Catalog, recoveryHull)
	quote, err := runner.growthQuote(port, hull)
	if err != nil {
		t.Fatal(err)
	}
	grown := append(append([]ShipState(nil), runner.stateCache.Ships...), growthCandidate(port, hull))
	deltaUpkeep := DailyUpkeep(grown, runner.stateCache.Catalog, len(runner.stateCache.Factors)) -
		DailyUpkeep(runner.stateCache.Ships, runner.stateCache.Catalog, len(runner.stateCache.Factors))
	existing, found, err := runner.growthAtYard(h.ctx, port, hull, quote, deltaUpkeep, &evidence, nil)
	if err != nil || !found {
		t.Fatalf("established-region package unavailable: found %t error %v", found, err)
	}
	destination, _ := findPort(runner.stateCache.Catalog, existing.plan.Destination)
	protected := evidence.required[destination.BankingRegion]
	if destination.BankingRegion == port.BankingRegion || protected <= 0 {
		t.Fatalf("fixture lacks an independently protected destination: %+v", evidence.required)
	}
	// Evaluate the same immutable lot when that destination floor is newly
	// committed, rather than already supporting an incumbent operation.
	newRegion := evidence
	newRegion.required = clonePence(evidence.required)
	delete(newRegion.required, destination.BankingRegion)
	newRegion.available += protected
	marginal, found, err := runner.growthAtYard(h.ctx, port, hull, quote, deltaUpkeep, &newRegion, &existing.plan)
	if err != nil || !found {
		t.Fatalf("newly dedicated regional package unavailable: found %t error %v", found, err)
	}
	if marginal.committedPence-existing.committedPence != protected {
		t.Fatalf("new floor was omitted or existing floor charged twice: existing %d, new %d, regional floor %d",
			existing.committedPence, marginal.committedPence, protected)
	}
	if !betterGrowthPackage(existing, marginal) || betterGrowthPackage(marginal, existing) {
		t.Fatal("equally productive package with extra dedicated regional cash ranked first")
	}
}
