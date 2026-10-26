package engine

import (
	"bytes"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"

	"github.com/DevYukine/go-tradewinds/internal/store"
)

// The oracle is the world's committed receipts, not missionData copied across
// the handoff. Hull and cannon purchases are capital, not operating expenses.
func growthCycle2Cash(t *testing.T, w *growthExecutionWorld) store.Settlement {
	t.Helper()
	var cash store.Settlement
	seen := make(map[string]bool)
	for _, path := range []string{"/v1/market/place-order", "/v1/treasury/remit"} {
		for _, call := range w.h.api.mutationsFor(path) {
			if seen[call.IdempotencyKey] {
				continue
			}
			seen[call.IdempotencyKey] = true
			w.h.api.mu.Lock()
			receipt := bytes.Clone(w.receipts[call.IdempotencyKey])
			w.h.api.mu.Unlock()
			if len(receipt) == 0 {
				continue
			}
			var request growthExecutionRequest
			var result struct {
				Gross   int64 `json:"total_gross_pence"`
				Customs int64 `json:"customs_duty_pence"`
				Fee     int64 `json:"fee_pence"`
			}
			if err := json.Unmarshal(call.Body, &request); err != nil {
				t.Fatal(err)
			}
			if err := json.Unmarshal(receipt, &result); err != nil {
				t.Fatal(err)
			}
			switch {
			case path == "/v1/treasury/remit":
				cash.FeesPence += result.Fee
			case request.IntoStores:
				cash.ProvisionsPence += result.Gross + result.Customs
			case request.Side == "bid":
				cash.AcquisitionPence += result.Gross
				cash.FeesPence += result.Customs
			case request.Side == "ask":
				cash.ProceedsPence += result.Gross
			}
		}
	}
	return cash
}

func TestGrowthCycle2CommissioningCashSurvivesPermanentCloseout(t *testing.T) {
	t.Parallel()
	for _, scenario := range []struct {
		name    string
		abort   bool
		unknown bool
	}{
		{name: "delivered"},
		{name: "late_abort", abort: true},
		{name: "unknown_delivered", unknown: true},
		{name: "unknown_late_abort", abort: true, unknown: true},
	} {
		t.Run(scenario.name, func(t *testing.T) {
			w, runner := newGrowthExecutionWorld(t)
			w.stopAfterSales = 1
			before, err := w.h.db.EconomicHistory(w.h.ctx, w.h.account.ID, "")
			if err != nil {
				t.Fatal(err)
			}
			w.until(runner, func(_ *store.Mission, _ missionData) bool {
				w.h.api.mu.Lock()
				defer w.h.api.mu.Unlock()
				return w.built
			})
			if err := runner.refresh(w.h.ctx); err != nil {
				t.Fatal(err)
			}
			control, commissioning := w.h.mission("__growth")
			ship, exists := runner.ship(recoveryBuiltShipID)
			if !exists {
				t.Fatal("paid commissioning hull missing from authoritative fleet")
			}
			// Pay the real capital refit through its production journal before
			// inducing an operating-cash shortfall. A capital-sized transfer
			// fee must not accidentally disprove this shallow marginal lane.
			if err := runner.stage(w.h.ctx, control, &commissioning, ship, "refit", "/v1/shipyard/modify-ship",
				map[string]any{"ship_id": ship.Ship.ID, "target_cannons_count": commissioning.TargetCannons}); err != nil {
				t.Fatal(err)
			}
			w.tick(runner)
			if err := runner.refresh(w.h.ctx); err != nil {
				t.Fatal(err)
			}
			ship, _ = runner.ship(recoveryBuiltShipID)
			input, err := runner.committedPlanningInput(w.h.ctx, ship)
			if err != nil {
				t.Fatal(err)
			}
			// Leave precisely the protected regional reserve, not an assumed
			// balance. The remaining stores/cargo need a measured paid remit.
			w.h.api.setVaultBalance("north", input.ReserveByRegion["north"])
			w.h.api.setVaultBalance("south", 5_000_000)
			w.until(runner, func(_ *store.Mission, _ missionData) bool {
				control, data, err := runner.loadMission(w.h.ctx, "__growth")
				if err != nil {
					t.Fatal(err)
				}
				ship, exists := runner.ship(recoveryBuiltShipID)
				return control != nil && control.Phase == "commissioning" && data.Plan.Kind != "aborted" &&
					data.Action == nil && exists && ship.Ship.CannonsCount >= data.TargetCannons
			})
			paid := growthCycle2Cash(t, w)
			if paid.ProvisionsPence <= 0 || paid.FeesPence <= 0 || w.h.api.mutationCount("/v1/shipyard/modify-ship") != 1 {
				t.Fatalf("fixture did not actually pay commissioning stores, remittance and refit: %+v", paid)
			}
			if scenario.unknown {
				control, data := w.h.mission("__growth")
				// A prior reconciled expense can be unknown even though these
				// new receipts have measured costs. Handoff cannot upgrade it.
				data.CostsKnown = false
				if err := runner.saveMission(w.h.ctx, control, &data); err != nil {
					t.Fatal(err)
				}
			}
			if scenario.abort {
				w.h.api.mu.Lock()
				w.source = nil
				w.h.api.mu.Unlock()
			}
			runner = w.h.runner(primaryTestKey)
			handedOff := false
			for range 90 {
				w.tick(runner)
				control, _, err := runner.loadMission(w.h.ctx, "__growth")
				if err != nil {
					t.Fatal(err)
				}
				if control == nil && !handedOff {
					handedOff = true
					runner = w.h.runner(primaryTestKey)
				}
				mission, _, err := runner.loadMission(w.h.ctx, recoveryBuiltShipID)
				if err != nil {
					t.Fatal(err)
				}
				if handedOff && len(w.settlementRows()) > 0 && (mission == nil || mission.Phase == "settled") {
					break
				}
			}
			rows := w.settlementRows()
			if !handedOff || len(rows) == 0 {
				t.Fatalf("paid commissioning never reached permanent closeout: rows %+v; holds %v", rows, runner.holding)
			}
			var got store.Settlement
			var net int64
			for _, row := range rows {
				got.AcquisitionPence += row.AcquisitionPence
				got.ProceedsPence += row.ProceedsPence
				got.ProvisionsPence += row.ProvisionsPence
				got.FeesPence += row.FeesPence
				got.ExpensesPence += row.ExpensesPence
				if row.CostsKnown == scenario.unknown {
					t.Fatalf("handoff changed cost certainty in permanent history: %+v", row)
				}
				if scenario.abort && (row.Kind != "aborted" || row.QuantityTons != 0) {
					t.Fatalf("failed commissioning invented a delivered lane: %+v", row)
				}
				if row.CostsKnown {
					net += row.ProceedsPence - row.AcquisitionPence - row.ProvisionsPence - row.FeesPence - row.ExpensesPence
				}
			}
			want := growthCycle2Cash(t, w)
			if got.AcquisitionPence != want.AcquisitionPence || got.ProceedsPence != want.ProceedsPence ||
				got.ProvisionsPence != want.ProvisionsPence || got.FeesPence != want.FeesPence || got.ExpensesPence != 0 {
				t.Fatalf("permanent cash omitted/doubled operating payments or expensed hull/refit capital: got %+v; receipts %+v", got, want)
			}
			after, err := w.h.db.EconomicHistory(w.h.ctx, w.h.account.ID, "")
			if err != nil {
				t.Fatal(err)
			}
			if after.CommercialNetPence-before.CommercialNetPence != net || after.UnknownCosts != scenario.unknown {
				t.Fatalf("permanent economic evidence disagrees with paid closeout: before %+v; after %+v; net %d", before, after, net)
			}
			// Replaying closing after another restart must not charge again.
			runner = w.h.runner(primaryTestKey)
			for range 3 {
				w.tick(runner)
			}
			replayed := w.settlementRows()
			if !bytes.Equal(encode(rows), encode(replayed)) {
				t.Fatalf("restart duplicated or changed permanent accounting: before %+v; after %+v", rows, replayed)
			}
		})
	}
}

// Store migration acceptance owns the v4-to-v5 transformation. This test starts
// at its explicit zero-allocation output, not at an arbitrary missing lane.
func TestGrowthCycle2MigratedPaidBuildClosesWithoutInventingAllocation(t *testing.T) {
	t.Parallel()
	for _, ambiguous := range []bool{false, true} {
		t.Run(fmt.Sprintf("ambiguous_%t", ambiguous), func(t *testing.T) {
			w, runner := newGrowthExecutionWorld(t)
			var dropping atomic.Bool
			if ambiguous {
				dropping.Store(true)
				w.h.api.server.Config.Handler = http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
					if request.Method == http.MethodPost && request.URL.Path == "/v1/shipyard/build-ship" && dropping.Load() {
						// Commit the actual build and retain its idempotent receipt,
						// but lose every response until the simulated crash.
						recorder := httptest.NewRecorder()
						w.handle(recorder, request)
						connection, _, err := writer.(http.Hijacker).Hijack()
						if err != nil {
							t.Error(err)
							return
						}
						_ = connection.Close()
						return
					}
					w.handle(writer, request)
				})
				w.until(runner, func(_ *store.Mission, _ missionData) bool {
					control, data, err := runner.loadMission(w.h.ctx, "__growth")
					if err != nil {
						t.Fatal(err)
					}
					return control != nil && data.Action != nil && data.Action.Stage == "build"
				})
				w.tick(runner)
				dropping.Store(false)
			} else {
				w.until(runner, func(_ *store.Mission, _ missionData) bool {
					control, data, err := runner.loadMission(w.h.ctx, "__growth")
					if err != nil {
						t.Fatal(err)
					}
					return control != nil && control.Phase == "commissioning" && data.Action == nil
				})
			}
			control, data := w.h.mission("__growth")
			if ambiguous && (data.Action == nil || len(w.h.pending()) != 1 || data.BuiltShipID != "") {
				t.Fatalf("fixture did not retain an ambiguous paid build: %+v", data)
			}
			beforeCash := growthCycle2Cash(t, w)
			beforeHistory, err := w.h.db.EconomicHistory(w.h.ctx, w.h.account.ID, "")
			if err != nil {
				t.Fatal(err)
			}
			if !ambiguous && beforeCash.ProvisionsPence <= 0 {
				t.Fatal("paid legacy commissioning has no actual operating expenditure")
			}
			calls := w.h.api.mutationsFor("/v1/shipyard/build-ship")
			if len(calls) == 0 {
				t.Fatal("fixture never actually built its paid hull")
			}
			originalKey := calls[0].IdempotencyKey
			originalBody := bytes.Clone(calls[0].Body)
			// This is precisely the approved one-time v5 migration result.
			// No quantity, limit, contract or profitable evidence is invented.
			data.GrowthLane = &growthLaneIntent{
				AdmissionID: control.Generation, QuantityLimit: 0,
				InitialProcurementLimitPence: 0, InitialProcurementComplete: true,
			}
			data.CooldownUntil = time.Now().UTC().Add(-time.Second)
			if err := runner.saveMission(w.h.ctx, control, &data); err != nil {
				t.Fatal(err)
			}
			marketBefore := w.h.api.mutationCount("/v1/market/place-order")
			refitBefore := w.h.api.mutationCount("/v1/shipyard/modify-ship")
			runner = w.h.runner(primaryTestKey)
			released := false
			for range 45 {
				w.tick(runner)
				growth, _, err := runner.loadMission(w.h.ctx, "__growth")
				if err != nil {
					t.Fatal(err)
				}
				mission, _, err := runner.loadMission(w.h.ctx, recoveryBuiltShipID)
				if err != nil {
					t.Fatal(err)
				}
				if growth == nil && (mission == nil || mission.Phase == "settled") {
					released = true
					break
				}
			}
			if !released {
				t.Fatalf("explicitly withdrawn legacy allocation stranded its paid hull: %v", runner.holding)
			}
			if w.h.api.mutationCount("/v1/market/place-order") != marketBefore ||
				w.h.api.mutationCount("/v1/shipyard/modify-ship") != refitBefore ||
				w.h.api.mutationCount("/v1/fleet/depart") != 0 {
				t.Fatal("withdrawn legacy allocation invented new procurement, hardware or voyage work")
			}
			calls = w.h.api.mutationsFor("/v1/shipyard/build-ship")
			if ambiguous && len(calls) < 2 {
				t.Fatal("ambiguous build was not resolved through its real idempotent replay")
			}
			for _, call := range calls {
				if call.IdempotencyKey != originalKey || !bytes.Equal(call.Body, originalBody) {
					t.Fatal("legacy recovery changed the immutable build identity or request body")
				}
			}
			w.h.api.mu.Lock()
			builtCount := 0
			for _, row := range w.h.api.fleet {
				if row["ship"].(map[string]any)["id"] == recoveryBuiltShipID {
					builtCount++
				}
			}
			w.h.api.mu.Unlock()
			if builtCount != 1 {
				t.Fatalf("legacy recovery lost or duplicated the actual hull: %d", builtCount)
			}
			var provisions, expenses int64
			for _, row := range w.settlementRows() {
				if row.QuantityTons != 0 || row.ProceedsPence != 0 || row.AcquisitionPence != 0 {
					t.Fatalf("legacy closing invented delivery evidence or expensed capital as cargo: %+v", row)
				}
				provisions += row.ProvisionsPence
				expenses += row.ExpensesPence
			}
			if provisions != beforeCash.ProvisionsPence || expenses != 0 {
				t.Fatalf("legacy closing erased paid stores or expensed hull/refit twice: provisions %d want %d; expenses %d", provisions, beforeCash.ProvisionsPence, expenses)
			}
			afterHistory, err := w.h.db.EconomicHistory(w.h.ctx, w.h.account.ID, "")
			if err != nil {
				t.Fatal(err)
			}
			if afterHistory.CommercialNetPence-beforeHistory.CommercialNetPence != -beforeCash.ProvisionsPence-beforeCash.FeesPence ||
				afterHistory.PositiveCycles != beforeHistory.PositiveCycles || afterHistory.PositiveQuantityTons != beforeHistory.PositiveQuantityTons ||
				afterHistory.UnknownCosts {
				t.Fatalf("withdrawn legacy build lost operating costs or invented positive execution evidence: before %+v; after %+v", beforeHistory, afterHistory)
			}
		})
	}
}

func TestGrowthCycle2AccruingBerthDuesDoNotStarveProcurement(t *testing.T) {
	t.Parallel()
	w, runner := newGrowthExecutionWorld(t)
	w.until(runner, func(mission *store.Mission, data missionData) bool {
		return mission != nil && mission.Phase == "provision" && data.Action == nil
	})
	base := time.Now().UTC()
	w.h.api.mu.Lock()
	w.builtRow()["ship"].(map[string]any)["berthed_at"] = base.Add(-24 * time.Hour).Format(time.RFC3339Nano)
	w.h.api.mu.Unlock()
	var captures atomic.Int64
	w.h.api.server.Config.Handler = http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		if request.Method == http.MethodGet && request.URL.Path == "/v1/world/clock" {
			w.h.api.mu.Lock()
			defer w.h.api.mu.Unlock()
			clock := w.h.api.clockDocument()
			clock["real_utc"] = base.Add(time.Duration(captures.Add(1)) * 15 * time.Minute).Format(time.RFC3339Nano)
			clock["calendar"].(map[string]any)["total_game_hours"] = w.gameHours
			w.h.api.writeJSON(writer, http.StatusOK, clock)
			return
		}
		if request.Method == http.MethodGet && request.URL.Path == "/v1/market/book" {
			w.h.api.mu.Lock()
			defer w.h.api.mu.Unlock()
			book := w.book(request.URL.Query().Get("port_id"))
			book["as_of"] = base.Add(time.Duration(captures.Load()) * 15 * time.Minute).Format(time.RFC3339Nano)
			w.h.api.writeJSON(writer, http.StatusOK, book)
			return
		}
		w.handle(writer, request)
	})
	runner = w.h.runner(primaryTestKey)
	var firstDues, lastDues int64
	for range 35 {
		w.tick(runner)
		_, data, err := runner.loadMission(w.h.ctx, recoveryBuiltShipID)
		if err != nil {
			t.Fatal(err)
		}
		if !data.BuyDone && data.Plan.Economics.BerthDuesPence > 0 {
			if firstDues == 0 {
				firstDues = data.Plan.Economics.BerthDuesPence
			}
			lastDues = data.Plan.Economics.BerthDuesPence
		}
		if w.h.api.mutationCount("/v1/fleet/depart") > 0 {
			break
		}
	}
	if firstDues <= 0 || lastDues <= firstDues || captures.Load() < 2 {
		t.Fatalf("fixture did not accrue positive berth dues across fresh server captures: first %d last %d captures %d", firstDues, lastDues, captures.Load())
	}
	var purchases int
	for _, call := range w.h.api.mutationsFor("/v1/market/place-order") {
		var request growthExecutionRequest
		if err := json.Unmarshal(call.Body, &request); err != nil {
			t.Fatal(err)
		}
		if !request.IntoStores && request.Side == "bid" {
			purchases++
		}
	}
	if purchases != 1 || w.h.api.mutationCount("/v1/fleet/depart") != 1 {
		t.Fatalf("changing dues caused a quote-only loop instead of one funded purchase/departure: purchases %d departures %d; holds %v", purchases, w.h.api.mutationCount("/v1/fleet/depart"), runner.holding)
	}
}

func TestGrowthCycle2ReturningLotProtectsOrdinaryPlanningAndFirstSend(t *testing.T) {
	t.Parallel()
	for _, boundary := range []string{"source_asks", "spot_destination_bids", "charter_quantity"} {
		t.Run(boundary, func(t *testing.T) {
			w, runner := newGrowthExecutionWorld(t)
			w.until(runner, func(mission *store.Mission, data missionData) bool {
				return mission != nil && mission.Phase == "arrival" && data.DeliveredQuantity > 0 && data.Action == nil
			})
			reward := int64(2_000)
			charter := Contract{
				ID: "cycle2-next-charter", ContractType: "charter", Status: "open",
				OriginPortID: recoveryFarPort, CommodityID: recoveryGrain,
				RemainingQuantity: 100, RewardPerTonPence: &reward,
			}
			var contractQuantity atomic.Int64
			contractQuantity.Store(charter.RemainingQuantity)
			if boundary == "charter_quantity" {
				w.h.api.server.Config.Handler = http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
					if request.Method == http.MethodGet && request.URL.Path == "/v1/contract/list" {
						current := charter
						current.RemainingQuantity = contractQuantity.Load()
						w.h.api.writeJSON(writer, http.StatusOK, []Contract{current})
						return
					}
					w.handle(writer, request)
				})
				if err := runner.refresh(w.h.ctx); err != nil {
					t.Fatal(err)
				}
				owner, _ := runner.ship(recoveryBuiltShipID)
				mission, delivered := w.h.mission(recoveryBuiltShipID)
				input, err := runner.committedPlanningInput(w.h.ctx, owner)
				if err != nil {
					t.Fatal(err)
				}
				back, err := growthReturnBasis(input, delivered.Plan.Origin)
				if err != nil {
					t.Fatal(err)
				}
				base := delivered.Plan
				base.Kind = "charter"
				base.ContractID = charter.ID
				quoted := quoteGrowthDelivery(input, base, delivered.GrowthLane, false, &back)
				if !quoted.plan.Accepted {
					t.Fatalf("observed charter cannot support a funded return lot: %+v", quoted.plan)
				}
				// Retain a measured selected charter in the existing bounded
				// admission; the controller still revalidates and pays its return.
				delivered.GrowthLane.NextPlan = &quoted.plan
				if err := runner.saveMission(w.h.ctx, mission, &delivered); err != nil {
					t.Fatal(err)
				}
			}
			_, data := w.until(runner, func(mission *store.Mission, data missionData) bool {
				return mission != nil && mission.Phase == "return_sailing" && data.Action == nil && data.GrowthLane.NextPlan != nil
			})
			lot := *data.GrowthLane.NextPlan
			if boundary == "charter_quantity" && (lot.Kind != "charter" || lot.ContractID != charter.ID) {
				t.Fatalf("fixture did not select a real observed charter before the paid return: %+v", lot)
			}
			const residual int64 = 2
			w.h.api.mu.Lock()
			switch boundary {
			case "source_asks":
				w.source = []Level{{OrderID: "reserved-and-two-free-asks", PricePence: 100, RemainingQuantity: lot.Quantity + residual}}
			case "spot_destination_bids":
				w.source = []Level{{OrderID: "plentiful-asks", PricePence: 100, RemainingQuantity: 10_000}}
				w.bids = []Level{{OrderID: "reserved-and-two-free-bids", PricePence: 1_000, RemainingQuantity: lot.Quantity + residual}}
			case "charter_quantity":
				w.source = []Level{{OrderID: "plentiful-charter-asks", PricePence: 100, RemainingQuantity: 10_000}}
				w.bids = nil
				contractQuantity.Store(lot.Quantity + residual)
			}
			ordinaryRow := sailingShipRow(recoveryShipLapras, "Lapras", recoveryHomePort, recoveryFarPort)
			ordinaryRow["ship"].(map[string]any)["status"] = "berthed"
			ordinaryRow["ship"].(map[string]any)["cannons_count"] = int64(4)
			ordinaryRow["voyage"] = nil
			ordinaryRow["route_legs"] = []any{}
			ordinaryRow["stores"] = map[string]any{"provisions_tons": float64(10)}
			w.h.api.fleet = append(w.h.api.fleet, ordinaryRow)
			w.h.api.mu.Unlock()
			if err := runner.refresh(w.h.ctx); err != nil {
				t.Fatal(err)
			}
			ordinary, exists := runner.ship(recoveryShipLapras)
			if !exists {
				t.Fatal("ordinary competing hull missing from authoritative fleet")
			}
			cacheBefore := encode(runner.stateCache)
			raw := PlanVoyages(runner.planningInput(ordinary))
			if len(raw) == 0 || !raw[0].Accepted || raw[0].Quantity <= residual {
				t.Fatalf("fixture lacks executable unreserved competing work: %+v", raw)
			}
			input, err := runner.committedPlanningInput(w.h.ctx, ordinary)
			if err != nil {
				t.Fatal(err)
			}
			residualAvailable := false
			for _, plan := range PlanVoyages(input) {
				if plan.Accepted && plan.Origin == lot.Origin && plan.Destination == lot.Destination && plan.Commodity == lot.Commodity && plan.Quantity > residual {
					t.Errorf("ordinary planning double-allocated a returning owner's %s: selected %d, only %d free", boundary, plan.Quantity, residual)
				}
				if plan.Accepted && plan.Kind == lot.Kind && plan.ContractID == lot.ContractID &&
					plan.Origin == lot.Origin && plan.Destination == lot.Destination &&
					plan.Quantity > 0 && plan.Quantity <= residual {
					residualAvailable = true
				}
			}
			if !residualAvailable {
				t.Error("quantity reservation hid independently profitable uncommitted residual work")
			}
			owner, _ := runner.ship(recoveryBuiltShipID)
			owner.Ship.Status = "berthed"
			owner.Ship.CurrentPortID = recoveryHomePort
			owner.Voyage = nil
			owner.Stores.ProvisionsTons = 10
			ownInput, err := runner.committedPlanningInput(w.h.ctx, owner)
			if err != nil {
				t.Fatal(err)
			}
			ownAvailable := false
			for _, plan := range PlanVoyages(ownInput) {
				if plan.Accepted && plan.Kind == lot.Kind && plan.ContractID == lot.ContractID && plan.Quantity >= lot.Quantity {
					ownAvailable = true
				}
			}
			if !ownAvailable {
				t.Error("the owner was excluded from its own executable lot")
			}
			if !bytes.Equal(cacheBefore, encode(runner.stateCache)) {
				t.Error("private quantity reservation mutated authoritative state cache")
			}
			// An already-staged ordinary IOC must also be fenced after a
			// restart; planning-only protection is not enough.
			plan := raw[0]
			plan.Quantity = residual + 1
			mission, ordinaryData := runner.newVoyage(ordinary, plan)
			mission.Phase = "buy"
			if err := runner.stage(w.h.ctx, mission, &ordinaryData, ordinary, "buy", "/v1/market/place-order",
				orderBody(ordinary, plan.Commodity, "bid", plan.Quantity, plan.BuyLimitPence, false)); err != nil {
				t.Fatal(err)
			}
			runner = w.h.runner(primaryTestKey)
			if err := runner.refresh(w.h.ctx); err != nil {
				t.Fatal(err)
			}
			mission, ordinaryData = w.h.mission(recoveryShipLapras)
			if err := runner.newActionPreflight(w.h.ctx, mission, &ordinaryData); err == nil {
				t.Errorf("ordinary first-send preflight admitted %d tons into only %d free %s", plan.Quantity, residual, boundary)
			}
		})
	}
}
