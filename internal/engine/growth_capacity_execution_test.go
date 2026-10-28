package engine

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"math"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/DevYukine/go-tradewinds/internal/store"
	"go.uber.org/zap"
	"go.uber.org/zap/zaptest/observer"
)

func capacityExecutionFixture(t *testing.T, refuseBuild bool) (*recoveryHarness, *Runner) {
	t.Helper()
	h := newRecoveryHarness(t)
	row := sailingShipRow(recoveryShipBlastoise, "Blastoise", recoveryHomePort, recoveryFarPort)
	row["ship"].(map[string]any)["created_at"] = time.Now().UTC().Add(-30 * 24 * time.Hour)
	h.api.setFleet([]map[string]any{row})
	h.api.setFactorPorts(recoveryHomePort, recoveryFarPort)
	persistGrowthWindowSettlement(t, h, store.Settlement{
		ID: "capacity-positive-delivery", Kind: "spot", CostsKnown: true,
		QuantityTons: 48, GameSeconds: 43_200, AcquisitionPence: 4_800, ProceedsPence: 2_000_000,
		CompletedAt: time.Now().UTC().Add(-2 * time.Hour),
	})
	persistGrowthWindowSettlement(t, h, store.Settlement{
		ID: "capacity-unknown-build-reconciliation", Kind: "aborted", CostsKnown: false,
		GameSeconds: 3_600, CompletedAt: time.Now().UTC().Add(-time.Hour),
	}, growthSettlementAction{Path: "/v1/shipyard/build-ship"})
	base := h.api.server.Config.Handler
	h.api.server.Config.Handler = http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		if request.Method != http.MethodPost || request.URL.Path != "/v1/shipyard/build-ship" && request.URL.Path != "/v1/shipyard/modify-ship" {
			base.ServeHTTP(writer, request)
			return
		}
		body, err := io.ReadAll(request.Body)
		if err != nil {
			t.Error(err)
			writer.WriteHeader(http.StatusBadRequest)
			return
		}
		mission, err := h.db.GetMission(request.Context(), h.account.ID, "__growth")
		var data missionData
		if err != nil || mission == nil || json.Unmarshal(mission.Data, &data) != nil || data.Capacity == nil || data.Action == nil ||
			!bytes.Equal(data.Action.Body, body) || data.Action.ID != request.Header.Get("Idempotency-Key") {
			t.Error("capacity request did not match its durable immutable intent")
			writer.WriteHeader(http.StatusConflict)
			return
		}
		journaled, err := h.db.HasAction(request.Context(), h.account.ID, data.Action.ID)
		if err != nil || !journaled {
			t.Error("capacity request reached the server before journaling")
			writer.WriteHeader(http.StatusConflict)
			return
		}
		var payload growthExecutionRequest
		if err := json.Unmarshal(body, &payload); err != nil {
			t.Error(err)
			writer.WriteHeader(http.StatusBadRequest)
			return
		}
		h.api.mu.Lock()
		defer h.api.mu.Unlock()
		if request.URL.Path == "/v1/shipyard/build-ship" {
			if refuseBuild {
				h.api.record(request, body, http.StatusConflict)
				h.api.writeProblem(writer, http.StatusConflict, "turnkey_unavailable", "yard cannot supply this hull")
				return
			}
			built := sailingShipRow(recoveryBuiltShipID, payload.Name, payload.PortID, recoveryFarPort)
			ship := built["ship"].(map[string]any)
			ship["status"] = "berthed"
			ship["cannons_count"] = int64(0)
			ship["created_at"] = time.Now().UTC()
			built["effective_hold_capacity_tons"] = int64(60)
			built["voyage"] = nil
			built["route_legs"] = []any{}
			built["stores"] = map[string]any{"provisions_tons": float64(0)}
			h.api.fleet = append(h.api.fleet, built)
			h.api.vaultBalance["north"] -= 60_000
			h.api.record(request, body, http.StatusOK)
			h.api.writeJSON(writer, http.StatusOK, map[string]any{
				"ship_id": recoveryBuiltShipID, "name": payload.Name, "ship_class": payload.ShipClass, "total_cost_pence": 60_000,
			})
			return
		}
		for _, built := range h.api.fleet {
			ship := built["ship"].(map[string]any)
			if ship["id"] == payload.ShipID {
				oldCannons := ship["cannons_count"].(int64)
				h.api.vaultBalance["north"] -= (payload.Cannons - oldCannons) * 15_000
				ship["cannons_count"] = payload.Cannons
				built["effective_hold_capacity_tons"] = 60 - 2*payload.Cannons
				h.api.record(request, body, http.StatusOK)
				h.api.writeJSON(writer, http.StatusOK, map[string]any{"ship_id": payload.ShipID, "cannons_count": payload.Cannons})
				return
			}
		}
		t.Error("capacity refit addressed an unbuilt hull")
		writer.WriteHeader(http.StatusNotFound)
	})
	runner := growthWindowRunner(t, h)
	return h, runner
}

func TestCapacityGrowthProductionBuildArmsAndReleasesHull(t *testing.T) {
	t.Parallel()
	h, runner := capacityExecutionFixture(t, false)
	if changed, err := runner.considerGrowth(h.ctx); err != nil || !changed {
		t.Fatalf("capacity fallback admission: changed %t error %v holds %v", changed, err, runner.holding)
	}
	mission, data := h.mission("__growth")
	if data.Capacity == nil || data.GrowthLane != nil || data.ProvenLane != "" || data.Action == nil || data.Action.Stage != "build" {
		t.Fatalf("capacity build retained a lane requirement: %+v", data)
	}
	if data.Capacity.QuoteLimitPence != 66_000 || data.Capacity.AdmittedAt.IsZero() {
		t.Fatalf("capacity quote ceiling or admission time missing: %+v", data.Capacity)
	}
	if err := runner.newActionPreflight(h.ctx, mission, &data); err != nil {
		t.Fatalf("lane-less capacity preflight: %v", err)
	}
	if changed, err := runner.dispatch(h.ctx, mission, &data); err != nil || !changed {
		t.Fatalf("capacity build dispatch: changed %t error %v", changed, err)
	}
	actionLogRefresh(t, h, runner)
	mission, data = h.mission("__growth")
	if changed, err := runner.driveGrowth(h.ctx, mission, &data); err != nil || !changed {
		t.Fatalf("capacity cannon staging: changed %t error %v", changed, err)
	}
	mission, data = h.mission("__growth")
	if data.Action == nil || data.Action.Stage != "refit" {
		t.Fatalf("capacity hull did not stage armament: %+v", data.Action)
	}
	if changed, err := runner.dispatch(h.ctx, mission, &data); err != nil || !changed {
		t.Fatalf("capacity cannon dispatch: changed %t error %v", changed, err)
	}
	actionLogRefresh(t, h, runner)
	for range 3 {
		mission, data = h.mission("__growth")
		if changed, err := runner.driveGrowth(h.ctx, mission, &data); err != nil || !changed {
			t.Fatalf("capacity settlement transition: changed %t error %v", changed, err)
		}
	}
	h.missingMission("__growth")
	h.missingMission(recoveryBuiltShipID)
	ship, exists := runner.ship(recoveryBuiltShipID)
	if !exists || ship.Ship.CannonsCount != 4 || ship.Stores.ProvisionsTons != 0 {
		t.Fatalf("capacity commissioning must only arm the actual hull: %+v", ship)
	}
	if h.api.mutationCount("/v1/shipyard/build-ship") != 1 || h.api.mutationCount("/v1/shipyard/modify-ship") != 1 || h.api.mutationCount("/v1/market/place-order") != 0 {
		t.Fatal("capacity commissioning rebuilt, skipped armament or bought a lane-specific cargo/store allocation")
	}
	settlements, err := h.db.SettlementHistory(h.ctx, h.account.ID, "", 10)
	if err != nil || len(settlements) != 3 || settlements[0].ShipID != recoveryBuiltShipID || settlements[0].QuantityTons != 0 {
		t.Fatalf("capacity control closeout settlement missing: %+v, %v", settlements, err)
	}
}

func TestCapacityGrowthFreshPreflightRechecksQuoteAndFunding(t *testing.T) {
	t.Parallel()
	for _, changedState := range []string{"quote", "future completion", "negative vault", "idle ship"} {
		t.Run(changedState, func(t *testing.T) {
			h, runner := capacityExecutionFixture(t, false)
			if changed, err := runner.considerGrowth(h.ctx); err != nil || !changed {
				t.Fatalf("admit capacity build: changed %t error %v", changed, err)
			}
			mission, data := h.mission("__growth")
			switch changedState {
			case "quote":
				quote := runner.stateCache.Quotes[recoveryHomePort+"|"+recoveryHull]
				higher := data.Capacity.QuoteLimitPence + 1
				quote.TotalCostPence = &higher
				runner.stateCache.Quotes[recoveryHomePort+"|"+recoveryHull] = quote
			case "future completion":
				persistGrowthWindowSettlement(t, h, store.Settlement{
					ID: "capacity-invalid-window", Kind: "spot", QuantityTons: 1, CostsKnown: true, ProceedsPence: 10_000,
					GameSeconds: 3_600, CompletedAt: time.Now().UTC().Add(time.Minute),
				})
			case "negative vault":
				runner.stateCache.Vaults[1].BalancePence = -1
			case "idle ship":
				runner.stateCache.Ships[0].Voyage = nil
				runner.stateCache.Ships[0].Ship.Status = "berthed"
				runner.stateCache.Factors = nil
			}
			if err := runner.newActionPreflight(h.ctx, mission, &data); err == nil {
				t.Fatalf("capacity build ignored changed %s", changedState)
			}
			if h.api.mutationCount("/v1/shipyard/build-ship") != 0 {
				t.Fatal("preflight sent a construction request")
			}
		})
	}
}

func TestCapacityGrowthBuildRefusalBackoffSurvivesIntentDeletion(t *testing.T) {
	t.Parallel()
	h, runner := capacityExecutionFixture(t, true)
	if changed, err := runner.considerGrowth(h.ctx); err != nil || !changed {
		t.Fatalf("admit refused capacity build: changed %t error %v", changed, err)
	}
	mission, data := h.mission("__growth")
	before := time.Now().UTC()
	if changed, err := runner.dispatch(h.ctx, mission, &data); err != nil || !changed {
		t.Fatalf("consume capacity refusal: changed %t error %v", changed, err)
	}
	snapshot, err := h.db.Snapshot(h.ctx, h.account.ID)
	if err != nil {
		t.Fatal(err)
	}
	until := decode[map[string]time.Time](snapshot, "_growth_build_backoffs")[recoveryHomePort+"|"+recoveryHull]
	if until.Before(before.Add(6*time.Hour)) || until.After(time.Now().UTC().Add(6*time.Hour)) {
		t.Fatalf("refusal did not persist a six-real-hour yard/class backoff: %s", until)
	}
	h.expireCooldown("__growth")
	for range 3 {
		mission, data = h.mission("__growth")
		if _, err := runner.driveGrowth(h.ctx, mission, &data); err != nil {
			t.Fatal(err)
		}
	}
	h.missingMission("__growth")
	restarted := growthWindowRunner(t, h)
	if changed, err := restarted.considerGrowth(h.ctx); err != nil || changed {
		t.Fatalf("restarted controller bypassed yard refusal backoff: changed %t error %v", changed, err)
	}
	h.missingMission("__growth")
	if h.api.mutationCount("/v1/shipyard/build-ship") != 1 {
		t.Fatal("definitively refused capacity hull was rebuilt during backoff")
	}
}

func TestCapacityGrowthInputsUseLatestDeliveryLotMedian(t *testing.T) {
	t.Parallel()
	h, runner := capacityExecutionFixture(t, false)
	evidence := growthEvidence{workingByRegion: make(map[string]int64)}
	if reason, err := runner.growthCashFunding(h.ctx, &evidence); err != nil || reason != "" {
		t.Fatalf("capacity funding: %s, %v", reason, err)
	}
	persistGrowthWindowSettlement(t, h, store.Settlement{
		ID: "capacity-old-unknown-delivery", Kind: "spot", QuantityTons: 1,
		AcquisitionPence: 999_999, GameSeconds: 3_600, CompletedAt: evidence.asOf.Add(-31 * 24 * time.Hour),
	})
	input, reason, err := runner.capacityFleetInput(h.ctx, &evidence)
	if err != nil || reason != "" || input.FleetSize != 1 || input.LotCashPence != 4_800 {
		t.Fatalf("capacity evidence did not use delivered lots: %+v, %s, %v", input, reason, err)
	}
	for i := range 12 {
		persistGrowthWindowSettlement(t, h, store.Settlement{
			ID: fmt.Sprintf("capacity-recent-lot-%02d", i), Kind: "spot", CostsKnown: true,
			QuantityTons: 1, GameSeconds: 3_600, AcquisitionPence: int64(i+1) * 100, ProceedsPence: 10_000,
			CompletedAt: evidence.asOf.Add(-time.Duration(i+1) * time.Minute),
		})
	}
	input, reason, err = runner.capacityFleetInput(h.ctx, &evidence)
	if err != nil || reason != "" || input.LotCashPence != 550 {
		t.Fatalf("capacity lot cash did not use exactly the latest ten deliveries: %+v, %s, %v", input, reason, err)
	}
	second := runner.stateCache.Ships[0]
	second.Ship.ID = recoveryShipCharizard
	createdAt := evidence.asOf
	second.Ship.CreatedAt = &createdAt
	runner.stateCache.Ships = append(runner.stateCache.Ships, second)
	input, reason, err = runner.capacityFleetInput(h.ctx, &evidence)
	if err != nil || reason != "" || input.FleetSize != 2 || input.LotCashPence != 550 {
		t.Fatalf("new unproven hull unexpectedly paused growth or changed lot cash: %+v, %s, %v", input, reason, err)
	}
}

func TestCapacityGrowthPreservesSourcingAndMaintenanceCash(t *testing.T) {
	t.Parallel()
	for _, kind := range []string{"relocation", "sourcing", "maintenance", "maintenance_done"} {
		t.Run(kind, func(t *testing.T) {
			h, runner := capacityExecutionFixture(t, false)
			ship := runner.stateCache.Ships[0]
			ship.Ship.Status = "berthed"
			ship.Voyage = nil
			runner.stateCache.Ships[0] = ship
			data := missionData{
				Plan: Plan{
					Kind: "spot", ShipID: ship.Ship.ID, Origin: recoveryHomePort, Destination: recoveryFarPort,
					Commodity: recoveryGrain, Quantity: 1, BuyLimitPence: 1_000,
					Economics: Economics{PurchasePence: 1_000},
				},
				CostsKnown: true,
			}
			phase := "sailing"
			switch kind {
			case "relocation":
				data.Plan.Kind = "relocation"
				data.Relocation = &relocationIntent{}
			case "sourcing":
				data.SourcingHandoff = &relocationIntent{}
			case "maintenance", "maintenance_done":
				phase = kind
				data.Plan.Kind = "maintenance"
				data.Plan.Quantity = 0
				data.Plan.TargetStoresTons = 3
				data.Plan.StoresToBuy = 1
				data.Plan.StoresLimitPence = 1_000
			}
			if kind == "relocation" || kind == "sourcing" {
				data.CooldownUntil = time.Now().UTC().Add(time.Hour)
			}
			h.putMission(ship.Ship.ID, "capacity-incumbent-"+kind, phase, data)
			mission, retained := h.mission(ship.Ship.ID)
			working := make(map[string]int64)
			runner.reserveMissionCash(working, mission, &retained, ship)
			if kind != "maintenance_done" && working["north"] < 1_000 {
				t.Fatalf("incumbent %s lost its unpaid cash allocation: %v", kind, working)
			}
			if kind == "maintenance" || kind == "maintenance_done" {
				if changed, err := runner.considerGrowth(h.ctx); err != nil || changed {
					t.Fatalf("capacity growth replaced an uncommitted maintenance hull: changed=%t err=%v", changed, err)
				}
				h.missingMission("__growth")
				_, after := h.mission(ship.Ship.ID)
				if !bytes.Equal(encode(retained), encode(after)) {
					t.Fatal("capacity deferral changed its maintenance incumbent")
				}
				return
			}
			if changed, err := runner.work(h.ctx); err != nil || !changed {
				t.Fatalf("capacity growth did not run alongside %s: changed %t error %v holds %v", kind, changed, err, runner.holding)
			}
			_, growth := h.mission("__growth")
			if growth.Capacity == nil || growth.GrowthLane != nil {
				t.Fatalf("%s did not admit independent capacity growth: %+v", kind, growth)
			}
			_, after := h.mission(ship.Ship.ID)
			if !bytes.Equal(encode(retained), encode(after)) {
				t.Fatalf("capacity admission changed its %s incumbent mission", kind)
			}
		})
	}
}

func TestCapacityGrowthCannotSpendRelocationCash(t *testing.T) {
	t.Parallel()
	h, runner := capacityExecutionFixture(t, false)
	h.putMission(recoveryShipBlastoise, "capacity-protected-relocation", "sourcing", missionData{
		Plan: Plan{
			Kind: "relocation", Origin: recoveryHomePort, Destination: recoveryFarPort,
			Economics: Economics{PurchasePence: 50_000_000},
		},
		Relocation: &relocationIntent{}, CostsKnown: true,
	})
	if changed, err := runner.considerGrowth(h.ctx); err != nil || changed {
		t.Fatalf("capacity admission spent protected relocation cash: changed %t error %v", changed, err)
	}
	h.missingMission("__growth")
	if h.api.mutationCount("/v1/shipyard/build-ship") != 0 {
		t.Fatal("capacity admission sent a build with underfunded incumbent working capital")
	}
}

func TestCapacityGrowthKeepsFleetCashBufferGlobal(t *testing.T) {
	t.Parallel()
	h, runner := capacityExecutionFixture(t, false)
	evidence := growthEvidence{workingByRegion: make(map[string]int64)}
	if reason, err := runner.growthCashFunding(h.ctx, &evidence); err != nil || reason != "" {
		t.Fatalf("capacity funding: %s, %v", reason, err)
	}
	input, reason, err := runner.capacityFleetInput(h.ctx, &evidence)
	if err != nil || reason != "" {
		t.Fatalf("capacity fleet input: %s, %v", reason, err)
	}
	port, _ := findPort(runner.stateCache.Catalog, recoveryHomePort)
	hull, _ := findHull(runner.stateCache.Catalog, recoveryHull)
	quote, err := runner.growthQuote(port, hull)
	if err != nil {
		t.Fatal(err)
	}
	decision, required, err := runner.capacityAtYard(h.ctx, port, hull, quote, input, &evidence)
	capital := growthCashTotal(decision.QuotedHullPence, decision.SlippageReservePence, decision.CannonsPence, decision.InitialStoresPence)
	if err != nil || !decision.Accepted || decision.WorkingCapitalPence <= 0 ||
		required[port.BankingRegion] != evidence.required[port.BankingRegion]+capital {
		t.Fatalf("yard requirement included the global fleet cash buffer: %+v, %v, %v", decision, required, err)
	}
	evidence.balances[port.BankingRegion] = required[port.BankingRegion]
	decision, _, err = runner.capacityAtYard(h.ctx, port, hull, quote, input, &evidence)
	if err != nil || !decision.Accepted || decision.FundingFeesPence != 0 {
		t.Fatalf("yard remitted an already-funded global buffer: %+v, %v", decision, err)
	}
}

func TestCapacityGrowthPlanRateAndLotCashFloor(t *testing.T) {
	t.Parallel()
	h, runner := capacityExecutionFixture(t, false)
	evidence := growthEvidence{workingByRegion: make(map[string]int64)}
	if reason, err := runner.growthCashFunding(h.ctx, &evidence); err != nil || reason != "" {
		t.Fatalf("capacity funding: %s, %v", reason, err)
	}
	input, reason, err := runner.capacityFleetInput(h.ctx, &evidence)
	if err != nil || reason != "" {
		t.Fatalf("capacity fleet input: %s, %v", reason, err)
	}
	input.LotCashPence = 0
	input.IncrementalDailyUpkeepPence = 100
	port, _ := findPort(runner.stateCache.Catalog, recoveryHomePort)
	hull, _ := findHull(runner.stateCache.Catalog, recoveryHull)
	quote, err := runner.growthQuote(port, hull)
	if err != nil {
		t.Fatal(err)
	}
	planning, err := runner.committedPlanningInputExcept(h.ctx, growthCandidate(port, hull), "__growth")
	if err != nil {
		t.Fatal(err)
	}
	planning.FleetDailyUpkeepPence = 0
	var rate float64
	var stores, lot int64
	for _, plan := range PlanVoyages(planning) {
		basis := plan.Economics.RevenueBasis
		if !plan.Accepted || len(plan.FreightIDs) > 0 {
			continue
		}
		lot = max(lot, plan.Economics.PurchasePence+plan.Economics.CustomsPence+plan.Economics.StoreCashPence)
		if plan.ContractID != "" || basis != "depth" && basis != "gossip" && basis != "retained" {
			continue
		}
		candidateRate := float64(plan.Economics.ExpectedNetPence) / (2 * plan.Economics.GameDays)
		if plan.Economics.BallastRepeatGameDays > 0 {
			candidateRate = float64(plan.Economics.BallastRepeatNetPence) / plan.Economics.BallastRepeatGameDays
		}
		if candidateRate > rate {
			rate = candidateRate
			stores = max(plan.Economics.StoreCashPence, plan.Economics.ProvisionsPence)
		}
	}
	if rate <= 0 || lot <= 0 {
		t.Fatalf("fixture lacks a positive earning rate and cargo lot: %v, %d", rate, lot)
	}
	decision, _, err := runner.capacityAtYard(h.ctx, port, hull, quote, input, &evidence)
	buffer := max(input.ReservePence, ceilBPS(*quote.TotalCostPence, 1_500)) + int64(input.FleetSize+1)*lot
	if err != nil || !decision.Accepted || decision.InitialStoresPence != stores ||
		math.Abs(decision.IncrementalNetPencePerGameDay-(rate-100)) > 1e-9 || decision.WorkingCapitalPence != buffer {
		t.Fatalf("capacity quote did not use best plan rate, its stores and the plan cash floor: %+v, %v", decision, err)
	}
}

func TestCapacityGrowthCountsGossipForEveryFleetSize(t *testing.T) {
	t.Parallel()
	for _, playerBid := range []bool{false, true} {
		t.Run(fmt.Sprint("player_bid_", playerBid), func(t *testing.T) {
			h, runner := capacityExecutionFixture(t, false)
			evidence := growthEvidence{workingByRegion: make(map[string]int64)}
			if reason, err := runner.growthCashFunding(h.ctx, &evidence); err != nil || reason != "" {
				t.Fatalf("capacity funding: %s, %v", reason, err)
			}
			input, reason, err := runner.capacityFleetInput(h.ctx, &evidence)
			if err != nil || reason != "" {
				t.Fatalf("capacity fleet input: %s, %v", reason, err)
			}
			port, _ := findPort(runner.stateCache.Catalog, recoveryHomePort)
			hull, _ := findHull(runner.stateCache.Catalog, recoveryHull)
			quote, err := runner.growthQuote(port, hull)
			if err != nil {
				t.Fatal(err)
			}
			market := runner.stateCache.Markets[recoveryFarPort]
			market.Visibility = "gossip"
			runner.stateCache.Markets[recoveryFarPort] = market
			delete(runner.snapshot, "_quay_book_memory")
			runner.stateCache.Contracts = nil
			bid, playerPrice, playerQuantity := int64(50_000), int64(80_000), int64(50)
			observed := runner.stateCache.Clock.RealUTC.Add(-time.Minute)
			commodity := GossipCommodity{CommodityID: recoveryGrain, SnapshotAt: observed, BestBidPence: &bid}
			if playerBid {
				commodity.BestPlayerBidPence = &playerPrice
				commodity.BestPlayerBidQuantity = &playerQuantity
			}
			detail := runner.stateCache.Ports[recoveryHomePort]
			detail.CoffeeHouseGossip = []Gossip{{
				SubjectPortID: recoveryFarPort, SnapshotAt: observed, Commodities: []GossipCommodity{commodity},
			}}
			runner.stateCache.Ports[recoveryHomePort] = detail
			planning, err := runner.committedPlanningInputExcept(h.ctx, growthCandidate(port, hull), "__growth")
			if err != nil {
				t.Fatal(err)
			}
			planning.FleetDailyUpkeepPence = 0
			acceptedGossip := false
			for _, plan := range PlanVoyages(planning) {
				acceptedGossip = acceptedGossip || plan.Accepted && plan.Economics.RevenueBasis == "gossip"
			}
			if !acceptedGossip {
				t.Fatal("fixture did not provide an accepted gossip-valued plan")
			}
			for _, fleetSize := range []int{1, 5} {
				input.FleetSize = fleetSize
				decision, _, err := runner.capacityAtYard(h.ctx, port, hull, quote, input, &evidence)
				if err != nil || !decision.Accepted {
					t.Fatalf("%d-ship fleet ignored a profitable gossip-priced first voyage: %+v, %v", fleetSize, decision, err)
				}
			}
		})
	}
}

func TestCapacityGrowthOneShipWithoutEarningsHistoryAdmitsBuild(t *testing.T) {
	t.Parallel()
	h := newRecoveryHarness(t)
	row := sailingShipRow(recoveryShipBlastoise, "Blastoise", recoveryHomePort, recoveryFarPort)
	h.api.setFleet([]map[string]any{row})
	h.api.setFactorPorts(recoveryHomePort, recoveryFarPort)
	runner := growthWindowRunner(t, h)
	history, err := h.db.EconomicHistory(h.ctx, h.account.ID, "")
	if err != nil || history.NetPence != 0 {
		t.Fatalf("fixture has earnings history: %+v, %v", history, err)
	}
	if changed, err := runner.considerGrowth(h.ctx); err != nil || !changed {
		t.Fatalf("one-ship fleet left idle cash unspent: changed %t error %v holds %v", changed, err, runner.holding)
	}
	_, data := h.mission("__growth")
	if data.Capacity == nil || data.Action == nil || data.Action.Stage != "build" {
		t.Fatalf("one-ship fleet did not stage a capacity build: %+v", data)
	}
}

func TestCapacityGrowthReleasesHullAfterRefitRefusalOrThreeAttempts(t *testing.T) {
	t.Parallel()
	for _, refusal := range []bool{true, false} {
		t.Run(fmt.Sprint("domain_refusal_", refusal), func(t *testing.T) {
			h, runner := capacityExecutionFixture(t, false)
			if changed, err := runner.considerGrowth(h.ctx); err != nil || !changed {
				t.Fatalf("admit capacity build: changed %t error %v", changed, err)
			}
			mission, data := h.mission("__growth")
			if changed, err := runner.dispatch(h.ctx, mission, &data); err != nil || !changed {
				t.Fatalf("capacity build: changed %t error %v", changed, err)
			}
			actionLogRefresh(t, h, runner)
			base := h.api.server.Config.Handler
			h.api.server.Config.Handler = http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
				if request.Method != http.MethodPost || request.URL.Path != "/v1/shipyard/modify-ship" {
					base.ServeHTTP(writer, request)
					return
				}
				body, err := io.ReadAll(request.Body)
				if err != nil {
					t.Error(err)
					writer.WriteHeader(http.StatusBadRequest)
					return
				}
				h.api.mu.Lock()
				defer h.api.mu.Unlock()
				if refusal {
					h.api.record(request, body, http.StatusConflict)
					h.api.writeProblem(writer, http.StatusConflict, "refit_unavailable", "yard cannot refit this hull")
				} else {
					h.api.record(request, body, http.StatusOK)
					h.api.writeJSON(writer, http.StatusOK, map[string]any{"ship_id": recoveryBuiltShipID, "cannons_count": 0})
				}
			})
			attempts := 3
			if refusal {
				attempts = 1
			}
			for range attempts {
				mission, data = h.mission("__growth")
				if changed, err := runner.driveGrowth(h.ctx, mission, &data); err != nil || !changed {
					t.Fatalf("stage capacity refit: changed %t error %v", changed, err)
				}
				mission, data = h.mission("__growth")
				if data.Action == nil || data.Action.Stage != "refit" {
					t.Fatalf("expected a staged refit: %+v", data.Action)
				}
				if changed, err := runner.dispatch(h.ctx, mission, &data); err != nil || !changed {
					t.Fatalf("capacity refit result: changed %t error %v", changed, err)
				}
			}
			for range 3 {
				mission, data = h.mission("__growth")
				if changed, err := runner.driveGrowth(h.ctx, mission, &data); err != nil || !changed {
					t.Fatalf("release capacity control: changed %t error %v", changed, err)
				}
			}
			h.missingMission("__growth")
			h.missingMission(recoveryBuiltShipID)
			if h.api.mutationCount("/v1/shipyard/modify-ship") != attempts {
				t.Fatal("capacity commissioning kept retrying armament")
			}
		})
	}
}

func TestCapacityGrowthKeepsBuiltHullUntilArmed(t *testing.T) {
	t.Parallel()
	for _, state := range []string{"evicted", "unfunded"} {
		t.Run(state, func(t *testing.T) {
			h, runner := capacityExecutionFixture(t, false)
			if changed, err := runner.considerGrowth(h.ctx); err != nil || !changed {
				t.Fatalf("admit capacity build: changed %t error %v", changed, err)
			}
			mission, data := h.mission("__growth")
			if changed, err := runner.dispatch(h.ctx, mission, &data); err != nil || !changed {
				t.Fatalf("capacity build: changed %t error %v", changed, err)
			}
			actionLogRefresh(t, h, runner)
			built := -1
			for index := range runner.stateCache.Ships {
				if runner.stateCache.Ships[index].Ship.ID == recoveryBuiltShipID {
					built = index
				}
			}
			if built < 0 {
				t.Fatal("built hull is not in the fleet")
			}
			hull := &runner.stateCache.Ships[built]
			switch state {
			case "evicted":
				hull.Ship.Status, hull.Ship.DockQueuedAt = "anchored_roadstead", nil
			case "unfunded":
				port, _ := findPort(runner.stateCache.Catalog, hull.Ship.CurrentPortID)
				for index := range runner.stateCache.Vaults {
					if runner.stateCache.Vaults[index].BankingRegion == port.BankingRegion {
						runner.stateCache.Vaults[index].BalancePence = 0
					}
				}
			}
			mission, data = h.mission("__growth")
			changed, err := runner.driveGrowth(h.ctx, mission, &data)
			if err != nil {
				t.Fatal(err)
			}
			mission, data = h.mission("__growth")
			if state == "evicted" {
				if !changed || data.Action == nil || data.Action.Stage != "dock" {
					t.Fatalf("evicted capacity hull was not docked under growth control: changed %t action %+v holds %v", changed, data.Action, runner.holding)
				}
				if _, err := runner.dispatch(h.ctx, mission, &data); err != nil || h.api.mutationCount("/v1/fleet/dock") != 1 {
					t.Fatalf("growth-controlled dock was not sent: %v, holds %v", err, runner.holding)
				}
			}
			if state == "unfunded" && (changed || data.Action != nil) {
				t.Fatalf("unfunded capacity hull did not hold: changed %t action %+v", changed, data.Action)
			}
			if h.api.mutationCount("/v1/shipyard/modify-ship") != 0 {
				t.Fatal("capacity control armed a hull that was not berthed and funded")
			}
		})
	}
}

func TestCapacityGrowthUnknownCostsDoNotPauseCheapGrowth(t *testing.T) {
	t.Parallel()
	for _, test := range []struct {
		name        string
		path        string
		kind        string
		proceeds    int64
		acquisition int64
	}{
		{name: "procurement unwind", path: "/v1/shipyard/build-ship", kind: "aborted", acquisition: 1},
		{name: "dock abort", path: "/v1/fleet/dock", kind: "aborted"},
		{name: "market abort with cargo at risk", path: "/v1/market/place-order", kind: "aborted", acquisition: 50_000},
		{name: "freight with recorded gain", path: "/v1/market/place-order", kind: "freight", proceeds: 80_000, acquisition: 30_000},
	} {
		t.Run(test.name, func(t *testing.T) {
			h, runner := capacityExecutionFixture(t, false)
			evidence := growthEvidence{workingByRegion: make(map[string]int64)}
			if reason, err := runner.growthCashFunding(h.ctx, &evidence); err != nil || reason != "" {
				t.Fatalf("capacity funding: %s, %v", reason, err)
			}
			before, reason, err := runner.capacityFleetInput(h.ctx, &evidence)
			if err != nil || reason != "" {
				t.Fatalf("baseline fleet input: %s, %v", reason, err)
			}
			persistGrowthWindowSettlement(t, h, store.Settlement{
				ID: "capacity-unknown-non-control", Kind: test.kind, ProceedsPence: test.proceeds, AcquisitionPence: test.acquisition,
				GameSeconds: 3_600, CompletedAt: time.Now().UTC().Add(-time.Minute),
			}, growthSettlementAction{Path: test.path})
			after, reason, err := runner.capacityFleetInput(h.ctx, &evidence)
			if err != nil || reason != "" || after.MeasuredNetAvailable {
				t.Fatalf("unknown costs did not mark measured net unavailable: %+v, %s, %v", after, reason, err)
			}
			if after.LotCashPence != before.LotCashPence || after.FleetSize != before.FleetSize {
				t.Fatalf("unknown-cost settlement changed working-cash exposure: before %+v after %+v", before, after)
			}
			if changed, err := runner.considerCapacityGrowth(h.ctx, &evidence); err != nil || !changed {
				t.Fatalf("unknown costs blocked cheap-hull growth: changed %t error %v", changed, err)
			}
			_, data := h.mission("__growth")
			if data.Capacity == nil || data.BuildClass != "hoy" {
				t.Fatalf("cheap cash growth was not staged: %+v", data)
			}
		})
	}
}

func TestCapacityGrowthChecksExemptCloseoutHull(t *testing.T) {
	t.Parallel()
	for _, status := range []string{"alive", "missing", "sunk", "old sunk"} {
		t.Run(status, func(t *testing.T) {
			h, runner := capacityExecutionFixture(t, false)
			shipID := recoveryShipBlastoise
			if status != "alive" {
				shipID = recoveryShipCharizard
			}
			if status == "sunk" || status == "old sunk" {
				lost := runner.stateCache.Ships[0]
				lost.Ship.ID, lost.Ship.Status = shipID, "sunk"
				updated := time.Now().UTC().Add(-time.Hour)
				if status == "old sunk" {
					updated = time.Now().UTC().Add(-capacityHistoryWindow - time.Hour)
				}
				lost.Ship.UpdatedAt = &updated
				runner.stateCache.Ships = append(runner.stateCache.Ships, lost)
			}
			persistGrowthWindowSettlement(t, h, store.Settlement{
				ID: "capacity-exempt-hull-closeout", Kind: "aborted", FeesPence: 250,
				GameSeconds: 3_600, CompletedAt: time.Now().UTC().Add(-time.Minute),
			}, growthSettlementAction{Path: "/v1/shipyard/modify-ship", ShipID: shipID})
			evidence := growthEvidence{workingByRegion: make(map[string]int64)}
			if reason, err := runner.growthCashFunding(h.ctx, &evidence); err != nil || reason != "" {
				t.Fatalf("capacity funding: %s, %v", reason, err)
			}
			window, err := h.db.EconomicHistoryWindow(h.ctx, h.account.ID, "", evidence.asOf, capacityHistoryWindow)
			if err != nil || window.UnknownCosts || window.NetPence != 1_994_950 {
				t.Fatalf("shipyard closeout was not SQL-exempt with its recorded costs charged: %+v, %v", window, err)
			}
			input, reason, err := runner.capacityFleetInput(h.ctx, &evidence)
			if err != nil {
				t.Fatal(err)
			}
			switch status {
			case "alive":
				if reason != "" || input.FleetSize != 1 {
					t.Fatalf("surviving hull closeout paused capacity growth: %+v, %s", input, reason)
				}
			case "missing", "old sunk":
				if !strings.Contains(reason, "surviving hull") {
					t.Fatalf("SQL-exempt %s hull escaped exact fleet validation: %s", status, reason)
				}
			case "sunk":
				if reason != capacityShipLossReason {
					t.Fatalf("SQL-exempt lost hull escaped the sinking veto: %s", reason)
				}
			}
		})
	}
}

func TestCapacityGrowthShipLossWindowAndFirstObservationSurviveRestart(t *testing.T) {
	t.Parallel()
	for _, timing := range []string{"undated", "future", "recent", "old"} {
		t.Run(timing, func(t *testing.T) {
			h, runner := capacityExecutionFixture(t, false)
			evidence := growthEvidence{workingByRegion: make(map[string]int64)}
			if reason, err := runner.growthCashFunding(h.ctx, &evidence); err != nil || reason != "" {
				t.Fatalf("capacity funding: %s, %v", reason, err)
			}
			lost := runner.stateCache.Ships[0]
			lost.Ship.ID, lost.Ship.Status, lost.Ship.UpdatedAt = recoveryShipCharizard, "sunk", nil
			updated := evidence.asOf.Add(-time.Hour)
			switch timing {
			case "future":
				updated = evidence.asOf.Add(time.Hour)
			case "old":
				updated = evidence.asOf.Add(-capacityHistoryWindow - time.Hour)
			}
			if timing != "undated" {
				lost.Ship.UpdatedAt = &updated
			}
			runner.stateCache.Ships = append(runner.stateCache.Ships, lost)
			_, reason, err := runner.capacityFleetInput(h.ctx, &evidence)
			wantReason := capacityShipLossReason
			switch timing {
			case "undated":
				wantReason = "capacity growth requires a reliable timestamp for every ship loss"
			case "future":
				wantReason = "capacity growth rejects future-dated ship-loss evidence"
			case "old":
				wantReason = ""
			}
			if err != nil || reason != wantReason {
				t.Fatalf("%s sinking admission: %s, %v; want %q", timing, reason, err, wantReason)
			}
			if reason != "" {
				if _, err := runner.holdCapacityGrowth(h.ctx, reason); err != nil {
					t.Fatal(err)
				}
				requireReviewHold(t, runner, reason, timing == "recent")
			}
			if timing == "old" {
				laterTouch := evidence.asOf.Add(-time.Minute)
				runner.stateCache.Ships[1].Ship.UpdatedAt = &laterTouch
				if _, reason, err := runner.capacityFleetInput(h.ctx, &evidence); err != nil || reason != "" {
					t.Fatalf("later server touch reset an old loss window: %s, %v", reason, err)
				}
			}
			if timing == "recent" {
				restarted := growthWindowRunner(t, h)
				if reason, err := restarted.capacityLosses(h.ctx, evidence.asOf); err != nil || reason != capacityShipLossReason {
					t.Fatalf("restart forgot a removed hull's recent loss: %s, %v", reason, err)
				}
				if loss := decode[map[string]time.Time](restarted.snapshot, "_ship_losses")[recoveryShipCharizard]; !loss.Equal(updated) {
					t.Fatalf("first observed loss time changed across restart: %s, want %s", loss, updated)
				}
			}
		})
	}
}

func TestCapacityGrowthTreatsMissingMissionHullsAsLosses(t *testing.T) {
	t.Parallel()
	h, runner := capacityExecutionFixture(t, false)
	h.putMission(recoveryShipCharizard, "capacity-missing-hull", "sailing", missionData{
		Plan: Plan{ShipID: recoveryShipCharizard},
	})
	evidence := growthEvidence{workingByRegion: make(map[string]int64)}
	if reason, err := runner.growthCashFunding(h.ctx, &evidence); err != nil || reason != "" {
		t.Fatalf("capacity funding: %s, %v", reason, err)
	}
	if _, reason, err := runner.capacityFleetInput(h.ctx, &evidence); err != nil || reason != capacityShipLossReason {
		t.Fatalf("fresh complete fleet omission did not veto capacity: %s, %v", reason, err)
	}
	if loss := decode[map[string]time.Time](runner.snapshot, "_ship_losses")[recoveryShipCharizard]; !loss.Equal(runner.meta["ships"].UpdatedAt) {
		t.Fatalf("missing hull was not timed by the authoritative fleet observation: %s", loss)
	}
}

func TestCapacityGrowthPreflightUsesLiveQuoteAndPlayerMaterialHeadroom(t *testing.T) {
	t.Parallel()
	for _, changedState := range []string{"higher allowed quote", "over ceiling", "mismatched yard", "player share", "regional funds within cushion", "regional funds above cushion"} {
		t.Run(changedState, func(t *testing.T) {
			h, runner := capacityExecutionFixture(t, false)
			if changed, err := runner.considerGrowth(h.ctx); err != nil || !changed {
				t.Fatalf("admit capacity build: changed %t error %v", changed, err)
			}
			mission, data := h.mission("__growth")
			total := data.Capacity.QuoteLimitPence - 1_000
			live := YardQuote{
				PortID: data.BuildPort, ShipClass: data.BuildClass, BuildMode: "turnkey_cash",
				Available: true, TotalCostPence: &total,
			}
			switch changedState {
			case "over ceiling":
				total = data.Capacity.QuoteLimitPence + 1
			case "mismatched yard":
				live.PortID = recoveryFarPort
			case "player share":
				live.MaterialsPence, live.NPCMaterialsPence = 2_000, 999
			case "regional funds within cushion", "regional funds above cushion":
				evidence := growthEvidence{workingByRegion: make(map[string]int64)}
				if reason, err := runner.growthCashFunding(h.ctx, &evidence); err != nil || reason != "" {
					t.Fatalf("capacity funding: %s, %v", reason, err)
				}
				input, reason, err := runner.capacityFleetInput(h.ctx, &evidence)
				if err != nil || reason != "" {
					t.Fatalf("capacity fleet input: %s, %v", reason, err)
				}
				port, _ := findPort(runner.stateCache.Catalog, data.BuildPort)
				hull, _ := findHull(runner.stateCache.Catalog, data.BuildClass)
				quote, err := runner.growthQuote(port, hull)
				if err != nil {
					t.Fatal(err)
				}
				// A lower cached quote leaves the admitted ceiling high enough
				// to test yard funding independently from the price ceiling.
				cached := *quote.TotalCostPence - 10_000
				quote.TotalCostPence = &cached
				runner.stateCache.Quotes[port.ID+"|"+hull.ID] = quote
				decision, required, err := runner.capacityAtYard(h.ctx, port, hull, quote, input, &evidence)
				if err != nil || !decision.Accepted {
					t.Fatalf("capacity yard input: %+v, %v", decision, err)
				}
				total = cached + decision.SlippageReservePence
				if changedState == "regional funds above cushion" {
					total++
				}
				for index := range runner.stateCache.Vaults {
					if runner.stateCache.Vaults[index].BankingRegion == port.BankingRegion {
						runner.stateCache.Vaults[index].BalancePence = required[port.BankingRegion]
					}
				}
			}
			base := h.api.server.Config.Handler
			var quoteReads atomic.Int64
			h.api.server.Config.Handler = http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
				if request.Method == http.MethodGet && request.URL.Path == "/v1/shipyard/quote" {
					quoteReads.Add(1)
					if request.URL.Query().Get("port_id") != data.BuildPort || request.URL.Query().Get("ship_class") != data.BuildClass ||
						request.URL.Query().Get("build_mode") != "turnkey_cash" {
						t.Error("live quote did not bind the immutable build terms")
					}
					h.api.writeJSON(writer, http.StatusOK, live)
					return
				}
				base.ServeHTTP(writer, request)
			})
			err := runner.newActionPreflight(h.ctx, mission, &data)
			accepted := changedState == "higher allowed quote" || changedState == "regional funds within cushion"
			if quoteReads.Load() != 1 || (err == nil) != accepted {
				t.Fatalf("live %s preflight: reads %d error %v", changedState, quoteReads.Load(), err)
			}
			if changedState == "regional funds above cushion" && !strings.Contains(err.Error(), "protected regional yard funding") {
				t.Fatalf("above-cushion quote failed outside the regional funds check: %v", err)
			}
			if h.api.mutationCount("/v1/shipyard/build-ship") != 0 {
				t.Fatal("live quote preflight dispatched a build")
			}
			if accepted {
				return
			}
			// A refusal must back off this yard and hull so the next pass does not readmit it.
			h.api.server.Config.Handler = base
			if err := h.db.DeleteMission(h.ctx, h.account.ID, "__growth"); err != nil {
				t.Fatal(err)
			}
			if _, err := runner.considerGrowth(h.ctx); err != nil {
				t.Fatal(err)
			}
			h.missingMission("__growth")
		})
	}
}

func TestCapacityGrowthPriceOverrunPersistsPauseAndYardBackoff(t *testing.T) {
	t.Parallel()
	h, runner := capacityExecutionFixture(t, false)
	core, logs := observer.New(zap.WarnLevel)
	runner.log = zap.New(core)
	const accountSecret = "synthetic passphrase, never used against the real game"
	const patternSecret = "tw_live_capacity_overrun_credential"
	if changed, err := runner.considerGrowth(h.ctx); err != nil || !changed {
		t.Fatalf("admit capacity build: changed %t error %v", changed, err)
	}
	mission, data := h.mission("__growth")
	stagedMission, stagedData := h.mission("__growth")
	actual := data.Capacity.QuoteLimitPence + 1
	base := h.api.server.Config.Handler
	h.api.server.Config.Handler = http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		if request.Method != http.MethodPost || request.URL.Path != "/v1/shipyard/build-ship" {
			base.ServeHTTP(writer, request)
			return
		}
		recorded := httptest.NewRecorder()
		base.ServeHTTP(recorded, request)
		var result map[string]any
		if err := json.Unmarshal(recorded.Body.Bytes(), &result); err != nil {
			t.Error(err)
			writer.WriteHeader(http.StatusInternalServerError)
			return
		}
		result["total_cost_pence"] = actual
		result["ship_id"] = "built-" + accountSecret + "-" + patternSecret
		h.api.mu.Lock()
		h.api.vaultBalance["north"] -= actual - 60_000
		h.api.mu.Unlock()
		h.api.writeJSON(writer, recorded.Code, result)
	})
	before := time.Now().UTC()
	if changed, err := runner.dispatch(h.ctx, mission, &data); err != nil || !changed {
		t.Fatalf("consume over-budget build receipt: changed %t error %v", changed, err)
	}
	mission, data = h.mission("__growth")
	if data.BuildCostPence != actual || data.BuiltShipID == "" || mission.Phase != "growth_refit" {
		t.Fatalf("price overrun lost the actual paid hull: phase %s data %+v", mission.Phase, data)
	}
	const warning = "capacity build receipt exceeded the admitted turnkey quote ceiling"
	warnings := logs.FilterMessage(warning).All()
	if len(warnings) != 1 || warnings[0].Level != zap.WarnLevel {
		t.Fatalf("fresh build overrun did not warn once with INFO disabled: %+v", warnings)
	}
	fields := warnings[0].ContextMap()
	if fields["quote_limit_pence"] != data.Capacity.QuoteLimitPence || fields["actual_cost_pence"] != actual ||
		fields["build_port"] != data.BuildPort || fields["build_class"] != data.BuildClass {
		t.Fatalf("overrun warning lost its admitted terms and actual cost: %+v", fields)
	}
	safeShipID, ok := fields["ship_id"].(string)
	if !ok || !strings.Contains(safeShipID, "[REDACTED]") || len([]rune(safeShipID)) > 515 {
		t.Fatalf("overrun ship identifier was not scrubbed and bounded: %q", safeShipID)
	}
	for _, entry := range logs.All() {
		encoded, err := json.Marshal(entry.ContextMap())
		if err != nil {
			t.Fatal(err)
		}
		for _, secret := range []string{accountSecret, patternSecret, primaryTestKey} {
			if strings.Contains(entry.Message, secret) || strings.Contains(string(encoded), secret) {
				t.Fatalf("build overrun warning exposed an upstream credential at %s", entry.Level)
			}
		}
	}
	if changed, err := runner.dispatch(h.ctx, stagedMission, &stagedData); err != nil || !changed {
		t.Fatalf("replay over-budget build receipt: changed %t error %v", changed, err)
	}
	if logs.FilterMessage(warning).Len() != 1 || h.api.mutationCount("/v1/shipyard/build-ship") != 1 {
		t.Fatal("completed-action replay repeated a build or its overrun warning")
	}
	restarted := growthWindowRunner(t, h)
	pause := decode[time.Time](restarted.snapshot, "_capacity_growth_paused_until")
	backoff := decode[map[string]time.Time](restarted.snapshot, "_growth_build_backoffs")[data.BuildPort+"|"+data.BuildClass]
	if pause.Before(before.Add(capacityHistoryWindow)) || backoff.Before(before.Add(6*time.Hour)) {
		t.Fatalf("build overrun lost its durable pause or yard backoff: pause %s backoff %s", pause, backoff)
	}
	evidence := growthEvidence{workingByRegion: make(map[string]int64)}
	if reason, err := restarted.growthCashFunding(h.ctx, &evidence); err != nil || reason != "" {
		t.Fatalf("capacity funding: %s, %v", reason, err)
	}
	if _, reason, err := restarted.capacityFleetInput(h.ctx, &evidence); err != nil || !strings.Contains(reason, "price overrun") {
		t.Fatalf("restart ignored the capacity-wide price-overrun pause: %s, %v", reason, err)
	}
}

func TestCapacityAuthorityDecode(t *testing.T) {
	t.Parallel()
	var ship ShipState
	if err := json.Unmarshal([]byte(`{"ship":{"id":"lost-hull","status":"sunk","updated_at":"2026-10-24T12:00:00Z"}}`), &ship); err != nil {
		t.Fatal(err)
	}
	expected := time.Date(2026, 10, 24, 12, 0, 0, 0, time.UTC)
	if ship.Ship.UpdatedAt == nil || !ship.Ship.UpdatedAt.Equal(expected) {
		t.Fatalf("ship loss timestamp did not decode: %+v", ship.Ship)
	}
	var quote YardQuote
	if err := json.Unmarshal([]byte(`{"materials_pence":1234,"npc_materials_pence":1000}`), &quote); err != nil {
		t.Fatal(err)
	}
	if quote.MaterialsPence != 1_234 || quote.NPCMaterialsPence != 1_000 {
		t.Fatalf("turnkey material provenance did not decode: %+v", quote)
	}
}

func TestCapacityGrowthInsufficientSurplusDoesNotHireExpansionAgent(t *testing.T) {
	t.Parallel()
	h, runner := capacityExecutionFixture(t, false)
	newExpansionFactorAPI(h)
	refreshExpansionFactor(t, h, runner)
	for i := range runner.stateCache.Vaults {
		runner.stateCache.Vaults[i].BalancePence = runner.opts.ReservePence + 40_000
	}
	if _, err := runner.considerGrowth(h.ctx); err != nil {
		t.Fatal(err)
	}
	h.missingMission("__growth")
	h.missingMission("__treasury")
	if h.api.mutationCount("/v1/company/hire-factor") != 0 {
		t.Fatal("unaffordable local capacity caused a paid overseas exploration")
	}
}

func TestCapacityGrowthComputedLosingRepeatDoesNotUseOutboundProfit(t *testing.T) {
	t.Parallel()
	for _, repeatNet := range []int64{-100, 0} {
		economics := Economics{
			ExpectedNetPence: 100_000, StressNetPence: -500_000, GameDays: 1,
			BallastRepeatNetPence: repeatNet, BallastRepeatGameDays: 2,
		}
		input := CapacityGrowthInput{
			Hull: Hull{HoldCapacityTons: 60, MaxCannons: 4}, HullPricePence: 60_000,
			SurplusPence: 1_000_000, FleetSize: 1, HasSpareWork: true,
			LotCashPence: 1_000, MaxPaybackDays: 420,
			CandidateRatePencePerGameDay: capacityVoyageRate(economics),
		}
		if decision := EvaluateCapacityGrowth(input); decision.Accepted {
			t.Fatalf("outbound profit financed a nonpositive computed repeat: %+v", decision)
		}
		economics.BallastRepeatGameDays = 0
		input.CandidateRatePencePerGameDay = capacityVoyageRate(economics)
		if decision := EvaluateCapacityGrowth(input); !decision.Accepted {
			t.Fatalf("no-repeat expected outbound fallback rejected despite an illustrative stress loss: %+v", decision)
		}
	}
}

func TestCapacityGrowthRejectedPaybackDoesNotFundInadmissibleExpansion(t *testing.T) {
	t.Parallel()
	h, runner := capacityExecutionFixture(t, false)
	newExpansionFactorAPI(h)
	refreshExpansionFactor(t, h, runner)
	runner.opts.CapacityPaybackDays = 0.000001
	evidence := growthEvidence{workingByRegion: make(map[string]int64)}
	if reason, err := runner.growthCashFunding(h.ctx, &evidence); err != nil || reason != "" {
		t.Fatalf("capacity funding: %s, %v", reason, err)
	}
	input, reason, err := runner.capacityFleetInput(h.ctx, &evidence)
	if err != nil || reason != "" {
		t.Fatalf("capacity input: %s, %v", reason, err)
	}
	port, _ := findPort(runner.stateCache.Catalog, recoveryHomePort)
	hull, _ := findHull(runner.stateCache.Catalog, recoveryHull)
	quote, err := runner.growthQuote(port, hull)
	if err != nil {
		t.Fatal(err)
	}
	decision, _, err := runner.capacityAtYard(h.ctx, port, hull, quote, input, &evidence)
	if err != nil || decision.Accepted || !strings.Contains(decision.Reason, "payback") {
		t.Fatalf("fixture must have spare local work rejected solely on payback: %+v, %v", decision, err)
	}
	if changed, err := runner.considerCapacityGrowth(h.ctx, &evidence); err != nil || changed {
		t.Fatalf("bad regional payback paid for an inadmissible overseas build: changed=%t err=%v holds=%v", changed, err, runner.holding)
	}
	h.missingMission("__treasury")
	found := false
	for _, hold := range runner.holding {
		found = found || hold.Reason == expansionAgentAdmissionReason
	}
	if !found || h.api.mutationCount("/v1/company/hire-factor") != 0 {
		t.Fatalf("inadmissible paid expansion did not preserve its measured-build hold: %v", runner.holding)
	}
	h.missingMission("__growth")
}

func TestCapacityGrowthIdleHullWithoutProfitableWorkExploresAnotherRegion(t *testing.T) {
	t.Parallel()
	h, runner := capacityExecutionFixture(t, false)
	newExpansionFactorAPI(h)
	row := stationaryShipRow(recoveryShipBlastoise, "Blastoise", recoveryHomePort, "berthed", 0, 4)
	row["ship"].(map[string]any)["created_at"] = time.Now().UTC().Add(-30 * 24 * time.Hour)
	h.api.setFleet([]map[string]any{row})
	refreshExpansionFactor(t, h, runner)
	bookCommodity(runner.stateCache.Markets[recoveryHomePort], recoveryGrain).Depth.Asks = nil
	ship := runner.stateCache.Ships[0]
	planning, err := runner.committedPlanningInput(h.ctx, ship)
	if err != nil {
		t.Fatal(err)
	}
	for _, plan := range PlanVoyages(planning) {
		if plan.Accepted {
			t.Fatalf("idle hull unexpectedly has profitable local work: %+v", plan)
		}
	}
	h.missingMission(ship.Ship.ID)
	if changed, err := runner.considerGrowth(h.ctx); err != nil || !changed {
		t.Fatalf("mission-less idle hull blocked overseas exploration: changed=%t err=%v holds=%v", changed, err, runner.holding)
	}
	mission, data := h.mission("__treasury")
	if mission.Destination != "yard-a" || data.Action == nil || data.Action.Stage != "hire-factor" {
		t.Fatalf("idle hull without local work did not stage the overseas factor: %+v %+v", mission, data.Action)
	}
	h.missingMission(ship.Ship.ID)
	h.missingMission("__growth")
}

func TestCapacityGrowthUnavailableHullStillRequiresAdmissibleExpansion(t *testing.T) {
	t.Parallel()
	for _, refusal := range []string{"unavailable", "non-turnkey", "unpriced", "missing", "stale"} {
		t.Run(refusal, func(t *testing.T) {
			h, runner := capacityExecutionFixture(t, false)
			newExpansionFactorAPI(h)
			refreshExpansionFactor(t, h, runner)
			runner.opts.CapacityPaybackDays = 0.000001
			hull := runner.stateCache.Catalog.ShipClasses[0]
			hull.ID = "unsold-hull"
			runner.stateCache.Catalog.ShipClasses = append(runner.stateCache.Catalog.ShipClasses, hull)
			for key, quote := range runner.stateCache.Quotes {
				if quote.ShipClass == hull.ID {
					continue
				}
				quote.ShipClass = hull.ID
				switch refusal {
				case "unavailable":
					quote.Available = false
				case "non-turnkey":
					quote.BuildMode = "materials"
				case "unpriced":
					quote.TotalCostPence = nil
				}
				if refusal != "missing" {
					otherKey := quote.PortID + "|" + hull.ID
					runner.stateCache.Quotes[otherKey] = quote
					meta := runner.meta["yard_quotes/"+key]
					if refusal == "stale" {
						meta.UpdatedAt = time.Now().Add(-24 * time.Hour)
					}
					runner.meta["yard_quotes/"+otherKey] = meta
				}
			}
			evidence := growthEvidence{workingByRegion: make(map[string]int64)}
			if reason, err := runner.growthCashFunding(h.ctx, &evidence); err != nil || reason != "" {
				t.Fatalf("funding: %s, %v", reason, err)
			}
			changed, err := runner.considerCapacityGrowth(h.ctx, &evidence)
			if err != nil || changed {
				t.Fatalf("quote %s licensed an inadmissible paid expansion: changed=%t err=%v holds=%v", refusal, changed, err, runner.holding)
			}
			h.missingMission("__treasury")
			h.missingMission("__growth")
			if refusal != "missing" && refusal != "stale" {
				found := false
				for _, hold := range runner.holding {
					found = found || hold.Reason == expansionAgentAdmissionReason
				}
				if !found {
					t.Fatalf("quote %s lost the measured-build expansion hold: %v", refusal, runner.holding)
				}
			}
		})
	}
}
