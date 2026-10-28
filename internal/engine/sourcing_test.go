package engine

import (
	"encoding/json"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/DevYukine/go-tradewinds/internal/store"
)

func TestSourcingValidatesTargetBeforePruningThreePairs(t *testing.T) {
	t.Parallel()
	in := sourcingInputFixture(t)
	addSourcingSource(t, &in, "b-source", 31, 900)
	addSourcingSource(t, &in, "c-source", 32, 900)
	addSourcingSource(t, &in, "d-legal", 33, 900)
	for _, source := range []string{"amsterdam", "b-source", "c-source"} {
		key := routeKey(source, "portsmouth", "hoy")
		route := in.Routes[key]
		route.Legs[0].EdgeClass = "oceanic"
		in.Routes[key] = route
	}
	candidates := acceptedRelocations(sourcingCandidates(in))
	if len(candidates) != 1 || candidates[0].Destination != "d-legal" {
		t.Fatalf("known illegal charter legs consumed the discovery shortlist: %+v", candidates)
	}
	accepted := acceptedRelocations(planSourcingRelocation(in))
	if len(accepted) != 1 || accepted[0].Destination != "d-legal" {
		t.Fatalf("the first three invalid target legs hid the later funded legal source: %+v", accepted)
	}
	if accepted[0].Economics.RevenuePence != 0 {
		t.Fatalf("a route inspection invented source revenue: %+v", accepted[0])
	}
}

func TestSourcingSkipsUnreachableChartersBeforePublicRouteInspection(t *testing.T) {
	t.Parallel()
	in := sourcingInputFixture(t)
	legal := in.Contracts[0]
	in.Contracts = nil
	for _, target := range []string{"havana", "cape_verde", "cadiz"} {
		in.Catalog.Ports = append(in.Catalog.Ports, Port{
			ID: target, BankingRegion: "channel", SeaZoneID: "english_channel", Phase: 1,
		})
		in.Ports[target] = PortDetail{ID: target, EffectiveBerths: 4}
		in.Metadata["ports/"+target] = sourcingFreshMeta(in.Now)
		contract := legal
		contract.ID = "charter-" + target
		contract.OriginPortID = target
		in.Contracts = append(in.Contracts, contract)
		key := routeKey("amsterdam", target, "hoy")
		route := sourcingRoute("amsterdam", target, 1000)
		route.Legs[0].EdgeClass = "oceanic"
		in.Routes[key] = route
		// Successful geometry remains a definitive hull rejection even when
		// it did not come from the current observation generation.
		in.Metadata["routes/"+key] = DatasetMeta{UpdatedAt: in.Now.Add(-time.Hour), Complete: true}
	}
	in.Contracts = append(in.Contracts, legal)
	candidates := sourcingCandidates(in)
	accepted := acceptedRelocations(candidates)
	if len(accepted) != 1 || accepted[0].ContractID != legal.ID {
		t.Fatalf("unreachable oceanic charters hid the legal regional source: %+v", candidates)
	}
	rejected := 0
	for _, plan := range candidates {
		if plan.Destination != "amsterdam" || plan.ContractID == legal.ID {
			continue
		}
		if !strings.Contains(plan.Reason, "below route edge") {
			t.Fatalf("unreachable charter did not retain its permanent hull rejection: %+v", plan)
		}
		rejected++
	}
	if rejected != 3 {
		t.Fatalf("oceanic source-to-charter rejections = %d, want 3", rejected)
	}
}

func TestSourcingUsesLegalSourceDetourInsteadOfUnusedDirectRoute(t *testing.T) {
	t.Parallel()
	in := sourcingInputFixture(t)
	key := routeKey("le_havre", "portsmouth", "hoy")
	route := in.Routes[key]
	route.Legs[0].EdgeClass = "oceanic"
	in.Routes[key] = route
	// The relocation sails le_havre -> amsterdam -> portsmouth, not the
	// hull-agnostic shortest path directly from le_havre to portsmouth.
	accepted := acceptedRelocations(planSourcingRelocation(in))
	if len(accepted) != 1 || accepted[0].Destination != "amsterdam" {
		t.Fatalf("an unused direct route hid the legal coastal sourcing legs: %+v", planSourcingRelocation(in))
	}
}

func TestSourcingKnownIllegalRouteRejectsBeforeFreshness(t *testing.T) {
	t.Parallel()
	in := sourcingInputFixture(t)
	key := routeKey("amsterdam", "portsmouth", "hoy")
	route := in.Routes[key]
	route.Legs[0].EdgeClass = "oceanic"
	in.Routes[key] = route
	meta := in.Metadata["routes/"+key]
	meta.Stale = true
	in.Metadata["routes/"+key] = meta
	hull, _ := findHull(in.Catalog, "hoy")
	_, err := sourcingRouteReady(in, "amsterdam", "portsmouth", hull)
	if err == nil || !strings.Contains(err.Error(), "below route edge") || strings.Contains(err.Error(), "fresh public route") {
		t.Fatalf("known oceanic route became a renewable freshness hold: %v", err)
	}
}

func TestRelocationConsumedStoreRiskPreservesActualPurchaseCash(t *testing.T) {
	t.Parallel()
	data := missionData{
		Plan:          Plan{Kind: "relocation", ShipID: "ship-a"},
		CostsKnown:    true,
		FeesPence:     200,
		ExpensesPence: 300,
		Relocation: &relocationIntent{
			ExpenseCapPence: 18_500,
			StoreBasis: &relocationStoreBasis{
				ReplacementPencePerTon: 12_300,
			},
		},
	}
	quantity, gross, customs := int64(2), int64(24_000), int64(600)
	result := tradeResult{FilledQuantity: &quantity, TotalGrossPence: &gross, CustomsDutyPence: &customs}
	recordTradeCosts(&data, "provision", result)
	recordRelocationStores(&data, result)
	ship := ShipState{}
	ship.Stores.ProvisionsTons = 1.75
	if expense, known := relocationSpentExpense(&data, ship); !known || expense != 3_575 {
		t.Fatalf("quarter-ton consumed risk plus actual non-store costs = %dp, known=%v; want 3575p", expense, known)
	}
	if data.ProvisionsPence != 24_600 || relocationCashCost(&data) != 25_100 {
		t.Fatalf("inventory risk allocation changed the actual purchase cash: %+v", data)
	}
	body, err := json.Marshal(data)
	if err != nil {
		t.Fatal(err)
	}
	var restarted missionData
	if err := json.Unmarshal(body, &restarted); err != nil {
		t.Fatal(err)
	}
	if expense, known := relocationSpentExpense(&restarted, ship); !known || expense != 3_575 || restarted.ProvisionsPence != 24_600 {
		t.Fatalf("restart lost physical-store accounting: expense=%dp known=%v cash=%dp", expense, known, restarted.ProvisionsPence)
	}
	ship.Stores.ProvisionsTons = 3
	if _, known := relocationSpentExpense(&restarted, ship); known {
		t.Fatal("unexplained additional stores were treated as a known risk credit")
	}
	ship.Stores.ProvisionsTons = 1.75
	restarted.CostsKnown = false
	if _, known := relocationSpentExpense(&restarted, ship); known {
		t.Fatal("unknown actual costs funded more discretionary risk")
	}
}

func putSourcingBudgetSettlement(t *testing.T, h *recoveryHarness, row store.Settlement) {
	t.Helper()
	row.AccountID = h.account.ID
	row.ActionID = row.ID + "-action"
	row.ShipID = recoveryShipBlastoise
	row.MissionID = row.ID
	row.CompletedAt = time.Now().UTC()
	row.StartedAt = row.CompletedAt.Add(-time.Hour)
	row.GameSeconds = 3_600
	if _, err := h.db.BeginAction(h.ctx, &store.Action{
		ID: row.ActionID, AccountID: h.account.ID, Method: http.MethodPost,
		Path: "/v1/test", Body: json.RawMessage(`{}`),
	}); err != nil {
		t.Fatal(err)
	}
	if err := h.db.CompleteAction(h.ctx, row.ActionID, http.StatusOK, json.RawMessage(`{}`), nil); err != nil {
		t.Fatal(err)
	}
	if err := h.db.PutSettlement(h.ctx, &row); err != nil {
		t.Fatal(err)
	}
}

func TestSourcingRollingExpenseSurvivesRestartAndOnlyConvertedNetReplenishes(t *testing.T) {
	t.Parallel()
	h := newRecoveryHarness(t)
	exploration := int64(20_000)
	now := time.Now().UTC()
	idleFallbackSettlement(t, h, store.Settlement{
		ID: "source-budget-probe", Kind: "relocation", CostsKnown: true,
		Origin: "le_havre", Destination: "amsterdam", Commodity: "sawn_planks",
		CompletedAt: now.Add(-3 * time.Hour), ProvisionsPence: 100_000, ExplorationExpensePence: &exploration,
	})
	for range 2 {
		runner := h.runner(primaryTestKey)
		in := probeHistoryLiveInput(t)
		if err := runner.loadSourcingHistory(h.ctx, &in); err != nil {
			t.Fatal(err)
		}
		if in.SourcingAllowancePence == nil || *in.SourcingAllowancePence != 0 || len(acceptedRelocations(planSourcingRelocation(in))) != 0 {
			t.Fatalf("restarting replenished exhausted rolling exploration risk: %+v", in.SourcingAllowancePence)
		}
	}
	idleFallbackSettlement(t, h, store.Settlement{
		ID: "source-budget-unrelated", Kind: "charter", CostsKnown: true, QuantityTons: 10,
		Origin: "unprobed-port", Commodity: "sawn_planks", ProceedsPence: 1_000_000, CompletedAt: now.Add(-time.Hour),
	})
	idleFallbackSettlement(t, h, store.Settlement{
		ID: "source-budget-loss", Kind: "spot", CostsKnown: true, ExpensesPence: 10_000, QuantityTons: 1,
		Origin: "amsterdam", Commodity: "sawn_planks", CompletedAt: now.Add(-time.Hour),
	})
	idleFallbackSettlement(t, h, store.Settlement{
		ID: "source-budget-profit", Kind: "charter", CostsKnown: true, QuantityTons: 10, ProceedsPence: 110_000,
		Origin: "amsterdam", Commodity: "sawn_planks", CompletedAt: now.Add(-time.Hour),
	})
	runner := h.runner(primaryTestKey)
	in := probeHistoryLiveInput(t)
	if err := runner.loadSourcingHistory(h.ctx, &in); err != nil {
		t.Fatal(err)
	}
	if in.SourcingAllowancePence == nil || *in.SourcingAllowancePence != 50_000 {
		t.Fatalf("converted loss-inclusive 100000p net did not replenish exactly 50000p: %+v", in.SourcingAllowancePence)
	}
	if len(acceptedRelocations(planSourcingRelocation(in))) == 0 {
		t.Fatal("known converted net could not fund a different legal inspection")
	}
}

func TestSourcingFailedEvidenceDoesNotRepeatOnRestartOrNewTimestamps(t *testing.T) {
	t.Parallel()
	h := newRecoveryHarness(t)
	in := sourcingInputFixture(t)
	charter := in.Contracts[0]
	exploration := int64(1_000)
	putSourcingBudgetSettlement(t, h, store.Settlement{
		ID: "source-evidence-probe", Kind: "relocation", CostsKnown: true, ExpensesPence: 1_000,
		Lane: sourcingEvidenceKey("amsterdam", charter, 900), ExplorationExpensePence: &exploration,
	})
	for range 2 {
		runner := h.runner(primaryTestKey)
		in = sourcingInputFixture(t)
		sourcingRestampGossip(t, &in, in.Now, in.Now)
		if err := runner.loadSourcingHistory(h.ctx, &in); err != nil {
			t.Fatal(err)
		}
		if accepted := acceptedRelocations(planSourcingRelocation(in)); len(accepted) != 0 {
			t.Fatalf("unchanged failed public evidence launched another inspection: %+v", accepted)
		}
	}
	sourcingRestampGossipAsk(t, &in, 800)
	if err := h.runner(primaryTestKey).loadSourcingHistory(h.ctx, &in); err != nil {
		t.Fatal(err)
	}
	if len(acceptedRelocations(planSourcingRelocation(in))) == 0 {
		t.Fatal("materially changed public supply price was not reconsidered within the remaining permanent allowance")
	}
}

func TestRelocationExecutionBudgetFundsFullStoresWithoutCallingThemConsumed(t *testing.T) {
	t.Parallel()
	in := sourcingStorePurchaseFixture(t)
	plans := acceptedRelocations(planSourcingRelocation(in))
	if len(plans) == 0 {
		t.Fatal("funded inspection fixture was not admitted")
	}
	plan := plans[0]
	runner := &Runner{
		stateCache: accountState{Ships: []ShipState{in.Ship}, Clock: in.Clock},
		meta: map[string]DatasetMeta{
			"ships": {UpdatedAt: time.Now(), Complete: true, Generation: in.Generation},
		},
		generation: in.Generation,
	}
	data := missionData{
		Plan: plan, CostsKnown: true,
		Relocation: &relocationIntent{
			ExpenseCapPence: 18_500,
			StoreBasis: &relocationStoreBasis{
				ReplacementPencePerTon: 12_300,
			},
		},
	}
	if !runner.relocationProvisionBudget(&data, in.Ship, in, plan.StoresToBuy, plan.StoresLimitPence) {
		t.Fatal("execution gate treated the funded full safe-store purchase as consumed exploration expense")
	}
	quantity, gross, customs := int64(2), int64(24_000), int64(600)
	result := tradeResult{FilledQuantity: &quantity, TotalGrossPence: &gross, CustomsDutyPence: &customs}
	recordTradeCosts(&data, "provision", result)
	recordRelocationStores(&data, result)
	in.Ship.Stores.ProvisionsTons = 2
	runner.stateCache.Ships[0] = in.Ship
	freshPlan := costRelocation(in, plan)
	if !runner.relocationBudget(&data, 0) || !runner.relocationProjectedBudget(&data, in.Ship, in, freshPlan, 0) {
		t.Fatal("authoritative full stores exhausted the risk cap despite zero physical consumption")
	}
	if data.ProvisionsPence != 24_600 {
		t.Fatalf("execution gate changed paid store cash: %dp", data.ProvisionsPence)
	}
	if runner.relocationBudget(&data, 18_501) {
		t.Fatal("consumed-expense allocation removed the immutable risk ceiling")
	}
	data.CostsKnown = false
	if runner.relocationBudget(&data, 0) {
		t.Fatal("an unknown result funded more discretionary exploration")
	}
}

func TestSourcingFundsActualCharterContinuationStoreTarget(t *testing.T) {
	t.Parallel()
	in := sourcingStorePurchaseFixture(t)
	outwardKey := routeKey("le_havre", "amsterdam", "hoy")
	outward := in.Routes[outwardKey]
	outward.SafeProvisionsTons = 2
	in.Routes[outwardKey] = outward
	ladenKey := routeKey("amsterdam", "portsmouth", "hoy")
	laden := in.Routes[ladenKey]
	laden.SafeProvisionsTons = 3
	in.Routes[ladenKey] = laden
	accepted := acceptedRelocations(planSourcingRelocation(in))
	if len(accepted) == 0 {
		t.Fatal("funded actual continuation stores were rejected")
	}
	plan := accepted[0]
	if plan.TargetStoresTons < 6 || plan.StoresToBuy != 7 {
		t.Fatalf("actual charter continuation was replaced by the shorter retreat: %+v", plan)
	}
	stores := plan.StoresToBuy * plan.StoresLimitPence
	wantCash := stores + ceilBPS(stores, in.Markets[plan.Origin].CustomsBPS) + plan.StoresToBuy
	if plan.Economics.StoreCashPence != wantCash {
		t.Fatalf("raised continuation target was not fully funded: cash=%dp want=%dp", plan.Economics.StoreCashPence, wantCash)
	}
	in.Balances["channel"] = 130_000
	for id, port := range in.Ports {
		port.BerthDuesPerDayPence = 1
		in.Ports[id] = port
	}
	in.FleetDailyUpkeepPence = 0
	if accepted := acceptedRelocations(planSourcingRelocation(in)); len(accepted) != 0 {
		t.Fatalf("the smaller retreat store bill authorized an unfunded actual continuation target: %+v", accepted)
	}
}

func TestCharterRelocationSpendingFailureAbortsFundingButHoldsDeparturePreparation(t *testing.T) {
	for _, gate := range []string{"funding", "departure preparation"} {
		t.Run(gate, func(t *testing.T) {
			h := newRecoveryHarness(t)
			h.api.setFleet([]map[string]any{sourcingReviewShipRow()})
			runner := refreshedRunner(t, h)
			sourcingReviewInject(t, runner, []Contract{sourcingReviewCharter("charter-spending-gate")})
			runner.stateCache.Ships[0].Ship.Status = "anchored_roadstead"
			runner.stateCache.Ships[0].Ship.BerthedAt = nil
			runner.snapshot["_ship_idle"] = encode(map[string]shipIdleState{
				recoveryShipBlastoise: {
					PortID: recoveryHomePort, EarnedGameHours: 6, Validated: true,
					LastValidatedGameHour: runner.stateCache.Clock.Calendar.TotalGameHours,
					LastClockUTC:          runner.stateCache.Clock.RealUTC,
				},
			})
			runner.refreshSourcingHistory(h.ctx, runner.planningInput(ShipState{}))
			runner.refreshSourcingBudget(h.ctx, runner.planningInput(ShipState{}))
			ship := harnessShip(t, runner, recoveryShipBlastoise)
			if changed, reason, err := runner.considerSourcing(h.ctx, ship); err != nil || !changed {
				t.Fatalf("charter relocation was not admitted: changed=%v reason=%q err=%v", changed, reason, err)
			}
			mission, data := h.mission(ship.Ship.ID)
			if data.Relocation == nil || data.Relocation.Scout {
				t.Fatalf("fixture must be a charter relocation: %+v", data)
			}
			meta := runner.meta["contracts"]
			meta.Stale = true
			runner.meta["contracts"] = meta
			if gate == "funding" {
				if _, err := runner.driveMission(h.ctx, ship, mission, &data); err != nil {
					t.Fatal(err)
				}
				h.missingMission(ship.Ship.ID)
			} else {
				if changed, err := runner.prepareDeparture(h.ctx, ship, mission, &data); err != nil || changed {
					t.Fatalf("charter departure preparation did not hold: changed=%v err=%v", changed, err)
				}
				retained, retainedData := h.mission(ship.Ship.ID)
				if retained.Phase != "sourcing_fund" || retainedData.Action != nil || retained.Destination != mission.Destination {
					t.Fatalf("departure preparation changed the held charter: mission=%+v data=%+v", retained, retainedData)
				}
			}
			found := false
			for _, hold := range runner.holding {
				if strings.Contains(hold.Reason, "idle exploration risk capital requires fresh contracts") {
					found = true
					cancelled := strings.Contains(hold.Reason, "cancelled before further spending")
					if cancelled != (gate == "funding") {
						t.Fatalf("wrong charter spending failure disposition: %+v", hold)
					}
				}
			}
			if !found {
				t.Fatalf("fixture did not reach the intended charter spending gate: %+v", runner.holding)
			}
			if h.api.mutationCount("/v1/treasury/remit")+h.api.mutationCount("/v1/fleet/depart") != 0 {
				t.Fatal("failed charter spending authority authorized a mutation")
			}
		})
	}
}

func TestSourcingRelocationDoesNotReserveUnavoidableFleetUpkeep(t *testing.T) {
	t.Parallel()
	in := sourcingInputFixture(t)
	in.Balances["channel"] = in.ReserveByRegion["channel"] + 605
	for _, fleetUpkeep := range []int64{0, 500_000, maximumPence} {
		in.FleetDailyUpkeepPence = fleetUpkeep
		plan := costRelocation(in, Plan{
			Kind: "relocation", ShipID: in.Ship.Ship.ID, Origin: "le_havre", Destination: "amsterdam",
			Commodity: "sawn_planks", ContractID: in.Contracts[0].ID,
		})
		if !plan.Accepted || plan.Economics.ExpectedNetPence != -613 || plan.Economics.UpkeepPence != 0 {
			t.Fatalf("unavoidable fleet upkeep %dp changed charter relocation cost/funding: %+v", fleetUpkeep, plan)
		}
	}
}

func TestCharterRelocationSpendingRechecksRollingBudgetWithoutIdlePilot(t *testing.T) {
	h := newRecoveryHarness(t)
	h.api.setFleet([]map[string]any{sourcingReviewShipRow()})
	runner := refreshedRunner(t, h)
	sourcingReviewInject(t, runner, []Contract{sourcingReviewCharter("charter-rolling-spending")})
	runner.refreshSourcingHistory(h.ctx, runner.planningInput(ShipState{}))
	runner.refreshSourcingBudget(h.ctx, runner.planningInput(ShipState{}))
	ship := harnessShip(t, runner, recoveryShipBlastoise)
	if changed, reason, err := runner.considerSourcing(h.ctx, ship); err != nil || !changed {
		t.Fatalf("charter relocation was not admitted: changed=%v reason=%q err=%v", changed, reason, err)
	}
	_, data := h.mission(ship.Ship.ID)
	if data.Relocation == nil || data.Relocation.Scout || data.Relocation.IdlePilot {
		t.Fatalf("fixture must be an ordinary first probe, not an idle pilot: %+v", data)
	}
	// Another hull completes a probe after this mission's immutable cap was
	// persisted. Spending must use today's B, not that earlier cap.
	idleFallbackExhaustSeed(t, h)
	if err := runner.relocationSpendingReadyAtPrice(h.ctx, ship, &data, 1, 0); err == nil ||
		!strings.Contains(err.Error(), "current rolling exploration budget 0p") {
		t.Fatalf("ordinary probe bypassed the exhausted rolling budget: %v", err)
	}
}
