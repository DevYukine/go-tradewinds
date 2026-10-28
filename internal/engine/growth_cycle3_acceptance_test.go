package engine

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"

	"github.com/DevYukine/go-tradewinds/internal/store"
)

func TestGrowthCycle3LegacyHullLossRetainsUnknownAnchoredHistory(t *testing.T) {
	t.Parallel()
	for _, missingBaseline := range []bool{true, false} {
		t.Run(fmt.Sprintf("missing_baseline_%t", missingBaseline), func(t *testing.T) {
			w, runner := newGrowthExecutionWorld(t)
			w.until(runner, func(_ *store.Mission, _ missionData) bool {
				w.h.api.mu.Lock()
				defer w.h.api.mu.Unlock()
				return w.built
			})
			control, data := w.h.mission("__growth")
			terminalAction := data.LastActionID
			if terminalAction == "" || data.BuildCostPence <= 0 || data.ProvisionsPence != 0 || data.FeesPence != 0 {
				t.Fatal("fixture lacks a paid build's terminal evidence before operating purchases")
			}
			w.h.api.mu.Lock()
			w.bill("cycle3-unattributed-old-bill", "north", 999)
			w.builtRow()["ship"].(map[string]any)["status"] = "sunk"
			w.h.api.mu.Unlock()
			if err := runner.refresh(w.h.ctx); err != nil {
				t.Fatal(err)
			}
			data.InitialLedgerIDs = runner.ledgerIDs()
			data.CostsKnown = false
			if missingBaseline {
				data.InitialLedgerIDs = nil
				// A legacy true flag cannot restore an absent accounting basis.
				data.CostsKnown = true
			}
			if err := runner.saveMission(w.h.ctx, control, &data); err != nil {
				t.Fatal(err)
			}
			expectedTerminalBill := int64(0)
			if !missingBaseline {
				// A retained accounting boundary still owns later eligible
				// debits, even when all direct measured counters remain zero.
				expectedTerminalBill = 17
				w.h.api.mu.Lock()
				w.bill("cycle3-terminal-after-baseline-bill", "north", expectedTerminalBill)
				w.h.api.mu.Unlock()
			}
			before, err := w.h.db.EconomicHistory(w.h.ctx, w.h.account.ID, "")
			if err != nil {
				t.Fatal(err)
			}
			runner = w.h.runner(primaryTestKey)
			restartedSettling := false
			for range 12 {
				w.tick(runner)
				current, _, err := runner.loadMission(w.h.ctx, "__growth")
				if err != nil {
					t.Fatal(err)
				}
				if current == nil || current.Phase == "settled" {
					break
				}
				if current.Phase == "settling" && !restartedSettling {
					restartedSettling = true
					runner = w.h.runner(primaryTestKey)
				}
			}
			rows := w.settlementRows()
			if len(rows) != 1 {
				t.Errorf("terminal paid hull lost its permanent unknown-cost accounting: %+v", rows)
			} else {
				row := rows[0]
				if row.ActionID != terminalAction || row.MissionID != control.Generation || row.CostsKnown ||
					row.Kind != "aborted" || row.QuantityTons != 0 || row.ProceedsPence != 0 ||
					row.AcquisitionPence != 0 || row.ProvisionsPence != 0 || row.FeesPence != 0 || row.ExpensesPence != expectedTerminalBill {
					t.Errorf("hull loss changed terminal evidence, invented measured costs or claimed an old unallocated bill: %+v", row)
				}
			}
			after, err := w.h.db.EconomicHistory(w.h.ctx, w.h.account.ID, "")
			if err != nil {
				t.Fatal(err)
			}
			if !after.UnknownCosts || after.CommercialNetPence != before.CommercialNetPence ||
				after.PositiveCycles != before.PositiveCycles || after.PositiveQuantityTons != before.PositiveQuantityTons {
				t.Errorf("legacy hull loss disappeared from permanent uncertainty or invented positive evidence: before %+v; after %+v", before, after)
			}
			// A later, independently bounded ship mission may claim its new
			// harbour bill, never the old bill preceding its accounting basis.
			later := sailingShipRow(recoveryShipLapras, "Lapras", recoveryHomePort, recoveryFarPort)
			later["ship"].(map[string]any)["status"] = "berthed"
			later["voyage"] = nil
			later["route_legs"] = []any{}
			w.h.api.mu.Lock()
			w.h.api.fleet = append(w.h.api.fleet, later)
			w.h.api.mu.Unlock()
			w.h.api.server.Config.Handler = http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
				if request.Method == http.MethodPost && request.URL.Path == "/v1/fleet/offshore" {
					body, err := io.ReadAll(request.Body)
					if err != nil {
						t.Error(err)
						writer.WriteHeader(http.StatusBadRequest)
						return
					}
					var payload growthExecutionRequest
					if err := json.Unmarshal(body, &payload); err != nil || payload.ShipID != recoveryShipLapras {
						t.Errorf("unexpected later offshore request: %s", body)
						writer.WriteHeader(http.StatusConflict)
						return
					}
					w.h.api.mu.Lock()
					defer w.h.api.mu.Unlock()
					later["ship"].(map[string]any)["status"] = "anchored_roadstead"
					w.h.api.record(request, body, http.StatusOK)
					w.h.api.writeJSON(writer, http.StatusOK, map[string]any{"ship_id": payload.ShipID, "status": "anchored_roadstead", "berth_dues_pence": 0})
					return
				}
				w.handle(writer, request)
			})
			runner = w.h.runner(primaryTestKey)
			if err := runner.refresh(w.h.ctx); err != nil {
				t.Fatal(err)
			}
			ship, _ := runner.ship(recoveryShipLapras)
			mission, laterData := runner.newVoyage(ship, Plan{Kind: "aborted", ShipID: ship.Ship.ID, Destination: recoveryHomePort})
			mission.Phase = "closing"
			if err := runner.saveMission(w.h.ctx, mission, &laterData); err != nil {
				t.Fatal(err)
			}
			w.h.api.mu.Lock()
			w.bill("cycle3-later-ship-bill", "north", 37)
			w.h.api.mu.Unlock()
			for range 16 {
				w.tick(runner)
				current, _, err := runner.loadMission(w.h.ctx, recoveryShipLapras)
				if err != nil {
					t.Fatal(err)
				}
				if current != nil && current.Phase == "settling" {
					runner = w.h.runner(primaryTestKey)
				}
				if current == nil || current.Phase == "settled" {
					break
				}
			}
			all, err := w.h.db.SettlementHistory(w.h.ctx, w.h.account.ID, "", 100)
			if err != nil {
				t.Fatal(err)
			}
			laterRows := 0
			for _, row := range all {
				if row.MissionID == mission.Generation {
					laterRows++
					if !row.CostsKnown || row.ExpensesPence != 37 {
						t.Errorf("later closeout lost its own bill or absorbed legacy overhead: %+v", row)
					}
				}
			}
			if laterRows != 1 {
				t.Errorf("later independently accounted ship did not settle exactly once: %d", laterRows)
			}
			if !bytes.Equal(encode(rows), encode(w.settlementRows())) {
				t.Error("later billing changed or duplicated the terminal hull-loss accounting")
			}
		})
	}
}

func TestGrowthCycle3TerminalAnchorlessUncertaintyHoldsButKnownNoEffectReleases(t *testing.T) {
	t.Parallel()
	for _, knownNoEffect := range []bool{false, true} {
		t.Run(fmt.Sprintf("known_no_effect_%t", knownNoEffect), func(t *testing.T) {
			h := newRecoveryHarness(t)
			runner := h.runner(primaryTestKey)
			if err := runner.refresh(h.ctx); err != nil {
				t.Fatal(err)
			}
			generation := "cycle3-anchorless-terminal"
			h.putMission("__growth", generation, "growth_rejected", missionData{
				Plan:       Plan{Kind: "spot", Origin: recoveryHomePort, Destination: recoveryFarPort, Commodity: recoveryGrain},
				CostsKnown: knownNoEffect, StartedAt: time.Now().UTC(),
				StartedGameHours: runner.stateCache.Clock.Calendar.TotalGameHours, InitialLedgerIDs: runner.ledgerIDs(),
			})
			for range 4 {
				if _, err := h.tick(runner); err != nil {
					t.Fatal(err)
				}
			}
			runner = h.runner(primaryTestKey)
			if _, err := h.tick(runner); err != nil {
				t.Fatal(err)
			}
			control, data, err := runner.loadMission(h.ctx, "__growth")
			if err != nil {
				t.Fatal(err)
			}
			if knownNoEffect {
				if control != nil {
					t.Error("genuinely known no-effect never-sent control was not released")
				}
			} else if control == nil || control.Generation != generation || data.LastActionID != "" || data.Action != nil {
				t.Error("anchorless uncertainty was deleted or assigned invented terminal evidence instead of remaining durable")
			}
			rows, err := h.db.SettlementHistory(h.ctx, h.account.ID, "", 100)
			if err != nil {
				t.Fatal(err)
			}
			if len(rows) != 0 || len(h.pending()) != 0 {
				t.Error("anchorless control fabricated settlement or action evidence")
			}
		})
	}
	t.Run("paid_stores_without_terminal_anchor", func(t *testing.T) {
		w, runner := newGrowthExecutionWorld(t)
		w.until(runner, func(_ *store.Mission, _ missionData) bool {
			control, data, err := runner.loadMission(w.h.ctx, "__growth")
			if err != nil {
				t.Fatal(err)
			}
			return control != nil && control.Phase == "commissioning" && data.Action == nil && data.ProvisionsPence > 0
		})
		control, data := w.h.mission("__growth")
		data.LastActionID = ""
		w.h.api.mu.Lock()
		w.builtRow()["ship"].(map[string]any)["status"] = "sunk"
		w.h.api.mu.Unlock()
		if err := runner.saveMission(w.h.ctx, control, &data); err != nil {
			t.Fatal(err)
		}
		runner = w.h.runner(primaryTestKey)
		for range 6 {
			w.tick(runner)
		}
		retained, retainedData, err := runner.loadMission(w.h.ctx, "__growth")
		if err != nil {
			t.Fatal(err)
		}
		if retained == nil || retained.Generation != control.Generation || retainedData.LastActionID != "" ||
			retainedData.ProvisionsPence != data.ProvisionsPence || len(w.settlementRows()) != 0 {
			t.Error("actual paid stores without terminal evidence were deleted or settled against an invented anchor")
		}
	})
}

func TestGrowthCycle3OrdinaryDepthLossConvergesWithoutChangingAttemptedRequests(t *testing.T) {
	t.Parallel()
	for _, attempted := range []bool{false, true} {
		t.Run(fmt.Sprintf("already_attempted_%t", attempted), func(t *testing.T) {
			w, runner := newGrowthExecutionWorld(t)
			w.stopAfterSales = 1
			mission, data := w.until(runner, func(mission *store.Mission, data missionData) bool {
				return mission != nil && mission.Phase == "provision" && data.Action == nil
			})
			if data.ProvisionsPence <= 0 {
				t.Fatal("fixture has no already-paid operating stores to preserve")
			}
			data.GrowthLane = nil
			data.Plan.Quantity = 10
			data.Plan.BuyLimitPence = 100
			mission.Quantity = 10
			mission.PricePence = 100
			mission.Phase = "buy"
			if err := runner.saveMission(w.h.ctx, mission, &data); err != nil {
				t.Fatal(err)
			}
			_, staged := w.until(runner, func(mission *store.Mission, data missionData) bool {
				return mission != nil && data.Action != nil && data.Action.Stage == "buy"
			})
			originalID := staged.Action.ID
			originalBody := bytes.Clone(staged.Action.Body)
			var dropping atomic.Bool
			if attempted {
				dropping.Store(true)
				w.h.api.server.Config.Handler = http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
					if request.Method == http.MethodPost && request.URL.Path == "/v1/market/place-order" && request.Header.Get("Idempotency-Key") == originalID && dropping.Load() {
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
				w.tick(runner)
				dropping.Store(false)
				if len(w.h.pending()) != 1 {
					t.Fatal("fixture did not retain the genuinely attempted purchase")
				}
				w.h.expireCooldown(recoveryBuiltShipID)
			}
			w.h.api.mu.Lock()
			w.source = []Level{{OrderID: "only-five-residual-asks", PricePence: 100, RemainingQuantity: 5}}
			w.h.api.mu.Unlock()
			runner = w.h.runner(primaryTestKey)
			stagedBodies := map[string][]byte{originalID: originalBody}
			converged := false
			for range 28 {
				w.tick(runner)
				current, currentData, err := runner.loadMission(w.h.ctx, recoveryBuiltShipID)
				if err != nil {
					t.Fatal(err)
				}
				if currentData.Action != nil && currentData.Action.Stage == "buy" {
					action := currentData.Action
					if prior, exists := stagedBodies[action.ID]; exists && !bytes.Equal(prior, action.Body) {
						t.Error("a rejected or attempted action identity was reused for a different payload")
					}
					stagedBodies[action.ID] = bytes.Clone(action.Body)
				}
				if current != nil && currentData.BuyDone && currentData.Action == nil && currentData.BoughtQuantity > 0 {
					converged = true
					break
				}
			}
			if !converged {
				t.Fatalf("ordinary procurement repeatedly staged/rejected the stale 10t lot instead of resized work or accounted unwind plus usable planning: %v", runner.holding)
			}
			uniqueBuys := make(map[string][]byte)
			for _, call := range w.h.api.mutationsFor("/v1/market/place-order") {
				var request growthExecutionRequest
				if err := json.Unmarshal(call.Body, &request); err != nil {
					t.Fatal(err)
				}
				if request.IntoStores || request.Side != "bid" {
					continue
				}
				if prior, exists := uniqueBuys[call.IdempotencyKey]; exists && !bytes.Equal(prior, call.Body) {
					t.Error("wire replay changed its attempted body")
				}
				uniqueBuys[call.IdempotencyKey] = call.Body
				if attempted {
					if call.IdempotencyKey != originalID || !bytes.Equal(call.Body, originalBody) || request.Quantity != 10 {
						t.Error("current residual depth replaced an already-attempted immutable purchase")
					}
				} else if request.Quantity <= 0 || request.Quantity > 5 || call.IdempotencyKey == originalID {
					t.Errorf("fresh procurement used stale quantity or rejected identity: quantity %d", request.Quantity)
				}
			}
			if len(uniqueBuys) != 1 {
				t.Fatalf("procurement did not complete exactly one logical cargo IOC: %d", len(uniqueBuys))
			}
			for range 65 {
				w.tick(runner)
				current, _, err := runner.loadMission(w.h.ctx, recoveryBuiltShipID)
				if err != nil {
					t.Fatal(err)
				}
				if current != nil && current.Phase == "settling" {
					runner = w.h.runner(primaryTestKey)
				}
				if current != nil && current.Phase == "settled" && w.sales == 1 {
					break
				}
			}
			var paidStores, paidCargo, proceeds int64
			seen := make(map[string]bool)
			for _, call := range w.h.api.mutationsFor("/v1/market/place-order") {
				if seen[call.IdempotencyKey] {
					continue
				}
				seen[call.IdempotencyKey] = true
				var request growthExecutionRequest
				var result struct {
					Gross   int64 `json:"total_gross_pence"`
					Customs int64 `json:"customs_duty_pence"`
				}
				if err := json.Unmarshal(call.Body, &request); err != nil {
					t.Fatal(err)
				}
				if err := json.Unmarshal(w.receipts[call.IdempotencyKey], &result); err != nil {
					t.Fatal(err)
				}
				if request.IntoStores {
					paidStores += result.Gross + result.Customs
				} else if request.Side == "bid" {
					paidCargo += result.Gross + result.Customs
				} else {
					proceeds += result.Gross
				}
			}
			var settledStores, settledCargo, settledProceeds int64
			for _, row := range w.settlementRows() {
				settledStores += row.ProvisionsPence
				settledCargo += row.AcquisitionPence + row.FeesPence
				settledProceeds += row.ProceedsPence
			}
			if w.sales != 1 || settledStores != paidStores || settledCargo != paidCargo || settledProceeds != proceeds || proceeds <= 0 {
				t.Fatalf("replanning/unwind lost paid operating costs or failed to settle resumed work: stores %d/%d cargo %d/%d proceeds %d/%d sales %d", settledStores, paidStores, settledCargo, paidCargo, settledProceeds, proceeds, w.sales)
			}
		})
	}
}

func TestGrowthCycle3ReservedCoastalWorkDoesNotVetoOceanicBuild(t *testing.T) {
	t.Parallel()
	h := newRecoveryHarness(t)
	h.seedLaneProofs()
	const oceanPort = "cycle3-ocean-port"
	const oceanHull = "cycle3-ocean-hull"
	h.api.setFactorPorts(recoveryHomePort, recoveryFarPort, oceanPort)
	h.api.setAutoRemit(true, 1_000_000)
	incumbent := sailingShipRow(recoveryShipBlastoise, "Blastoise", recoveryHomePort, recoveryFarPort)
	incumbent["ship"].(map[string]any)["ship_class"] = oceanHull
	incumbent["cargo_manifest"] = map[string]any{recoveryGrain: int64(10)}
	incumbent["cargo_used_tons"] = int64(10)
	idle := sailingShipRow(recoveryShipLapras, "Lapras", recoveryHomePort, recoveryFarPort)
	idle["ship"].(map[string]any)["status"] = "berthed"
	idle["ship"].(map[string]any)["cannons_count"] = int64(4)
	idle["stores"] = map[string]any{"provisions_tons": 4}
	idle["effective_hold_capacity_tons"] = 52
	idle["voyage"] = nil
	idle["route_legs"] = []any{}
	h.api.setFleet([]map[string]any{incumbent, idle})
	h.putMission(recoveryShipBlastoise, "cycle3-incumbent-coastal-lot", "sailing", missionData{
		Plan: Plan{Kind: "spot", ShipID: recoveryShipBlastoise, Origin: recoveryHomePort, Destination: recoveryFarPort,
			Commodity: recoveryGrain, Quantity: 10, BuyLimitPence: 100, SellLimitPence: 1_000, TargetStoresTons: 2,
			Economics: Economics{Basis: "destination", GameDays: 1, BallastRepeatGameDays: 1}},
		BuyDone: true, BoughtQuantity: 10, AcquisitionPence: 1_000, CostsKnown: true,
	})
	h.api.server.Config.Handler = http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		if request.Method == http.MethodGet {
			h.api.mu.Lock()
			defer h.api.mu.Unlock()
			switch request.URL.Path {
			case "/v1/world/catalog":
				catalog := h.api.catalogDocument()
				catalog["ship_classes"] = append(catalog["ship_classes"].([]any), map[string]any{
					"id": oceanHull, "hull_category": "oceanic", "phase": 1,
					"base_speed_centiknots": 850, "hold_capacity_tons": 80, "max_cannons": 4, "crew_required": 10,
				})
				catalog["ports"] = append(catalog["ports"].([]any), map[string]any{
					"id": oceanPort, "name": "Ocean port", "banking_region": "south", "sea_zone_id": "zone-south",
					"phase": 1, "is_hub": false, "has_shipyard": false, "customs_bps": 200,
					"base_berths": 10, "effective_berths": 10, "imports": []any{recoveryGrain, "provisions"},
				})
				h.api.writeJSON(writer, http.StatusOK, catalog)
			case "/v1/world/port":
				port := h.api.portDocument(request.URL.Query().Get("port_id"))
				if request.URL.Query().Get("port_id") == oceanPort {
					port["banking_region"] = "south"
					port["sea_zone_id"] = "zone-south"
				}
				h.api.writeJSON(writer, http.StatusOK, port)
			case "/v1/market/book":
				port := request.URL.Query().Get("port_id")
				var asks, bids []Level
				switch port {
				case recoveryHomePort:
					asks = []Level{{OrderID: "finite-source", PricePence: 100, RemainingQuantity: 10_000}}
				case recoveryFarPort:
					bids = []Level{{OrderID: "all-ten-bids-owned-by-incumbent", PricePence: 1_000, RemainingQuantity: 10}}
				case oceanPort:
					bids = []Level{{OrderID: "separate-ocean-demand", PricePence: 2_000, RemainingQuantity: 5_000}}
				}
				h.api.writeJSON(writer, http.StatusOK, map[string]any{
					"port_id": port, "visibility": "quay", "customs_bps": 200, "as_of": time.Now().UTC(),
					"books": []any{
						map[string]any{"commodity_id": recoveryGrain, "depth": map[string]any{"asks": asks, "bids": bids}},
						map[string]any{"commodity_id": "provisions", "depth": map[string]any{"asks": []Level{{OrderID: "finite-stores-" + port, PricePence: 50, RemainingQuantity: 10_000}}, "bids": []any{}}},
					},
				})
			case "/v1/world/route":
				from, to := request.URL.Query().Get("from"), request.URL.Query().Get("to")
				route := h.api.routeDocument(from, to)
				route["ship_class"] = request.URL.Query().Get("ship_class")
				if from == oceanPort || to == oceanPort {
					route["legs"].([]any)[0].(map[string]any)["edge_class"] = "oceanic"
				}
				h.api.writeJSON(writer, http.StatusOK, route)
			case "/v1/shipyard/quote":
				h.api.writeJSON(writer, http.StatusOK, map[string]any{
					"port_id": request.URL.Query().Get("port_id"), "ship_class": request.URL.Query().Get("ship_class"),
					"build_mode": "turnkey_cash", "available": true, "total_cost_pence": 20_000,
				})
			default:
				h.api.handleGet(writer, request)
			}
			return
		}
		if request.Method == http.MethodPost && request.URL.Path == "/v1/shipyard/build-ship" {
			body, err := io.ReadAll(request.Body)
			if err != nil {
				t.Error(err)
				writer.WriteHeader(http.StatusBadRequest)
				return
			}
			var payload growthExecutionRequest
			if err := json.Unmarshal(body, &payload); err != nil || payload.ShipClass != oceanHull || payload.PortID != recoveryHomePort {
				t.Errorf("unexpected build package: %s", body)
				writer.WriteHeader(http.StatusConflict)
				return
			}
			h.api.mu.Lock()
			defer h.api.mu.Unlock()
			built := sailingShipRow(recoveryBuiltShipID, payload.Name, recoveryHomePort, oceanPort)
			built["ship"].(map[string]any)["ship_class"] = oceanHull
			built["ship"].(map[string]any)["status"] = "berthed"
			built["voyage"] = nil
			h.api.fleet = append(h.api.fleet, built)
			h.api.vaultBalance["north"] -= 20_000
			h.api.record(request, body, http.StatusOK)
			h.api.writeJSON(writer, http.StatusOK, map[string]any{
				"ship_id": recoveryBuiltShipID, "name": payload.Name, "ship_class": payload.ShipClass, "total_cost_pence": 20_000,
			})
			return
		}
		h.api.handle(writer, request)
	})
	runner := refreshedRunner(t, h)
	// The sailing ocean-capable incumbent observed this lane before departure;
	// the idle coastal hoy cannot obtain a quote for an ocean-only passage.
	observations := runner.planningInput(ShipState{})
	seedZeroHazardQuotes(&observations)
	runner.stateCache.HazardQuotes = observations.HazardQuotes
	runner.snapshot[hazardQuoteSnapshotKey] = encode(observations.HazardQuotes)
	idleShip, _ := runner.ship(recoveryShipLapras)
	raw := PlanVoyages(runner.planningInput(idleShip))
	if len(raw) == 0 || !raw[0].Accepted || raw[0].Destination != recoveryFarPort {
		t.Fatalf("fixture lacks the apparently profitable but wholly reserved coastal alternative: %+v", raw)
	}
	committed, err := runner.committedPlanningInput(h.ctx, idleShip)
	if err != nil {
		t.Fatal(err)
	}
	for _, plan := range PlanVoyages(committed) {
		if plan.Accepted {
			t.Fatalf("fixture's coastal idle ship still has uncommitted work: %+v", plan)
		}
	}
	if changed, err := runner.considerGrowth(h.ctx); err != nil || !changed {
		t.Fatalf("separately viable funded oceanic package did not reach consideration: changed %t error %v holds %v", changed, err, runner.holding)
	}
	control, proposed := h.mission("__growth")
	if proposed.Action == nil || proposed.BuildClass != oceanHull || proposed.Plan.Destination != oceanPort {
		t.Fatalf("consideration did not stage the separately funded oceanic build: %+v", proposed)
	}
	originalID := proposed.Action.ID
	originalBody := bytes.Clone(proposed.Action.Body)
	// The proposed control remains persisted during first-send validation:
	// incumbent-only idle alternatives must not double-count its own allocation.
	if _, err := h.tick(runner); err != nil {
		t.Fatal(err)
	}
	calls := h.api.mutationsFor("/v1/shipyard/build-ship")
	if len(calls) != 1 || calls[0].IdempotencyKey != originalID || !bytes.Equal(calls[0].Body, originalBody) {
		t.Fatalf("raw reserved-only idle work vetoed the valid build's immutable first send: calls %d; holds %v", len(calls), runner.holding)
	}
	completed, outcome := h.mission("__growth")
	if completed.Generation != control.Generation || outcome.BuiltShipID != recoveryBuiltShipID || outcome.Action != nil || outcome.BuildCostPence != 20_000 {
		t.Fatalf("oceanic build did not retain its accounted authoritative result: %+v", outcome)
	}
}
