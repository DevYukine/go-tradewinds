package engine

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"testing"
)

func installExcludedHullCatalog(h *recoveryHarness, category string) {
	base := h.api.server.Config.Handler
	h.api.server.Config.Handler = http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		if request.Method != http.MethodGet || request.URL.Path != "/v1/world/catalog" {
			base.ServeHTTP(writer, request)
			return
		}
		h.api.mu.Lock()
		catalog := h.api.catalogDocument()
		h.api.mu.Unlock()
		catalog["ship_classes"] = append(catalog["ship_classes"].([]any),
			map[string]any{"id": "excluded-barque", "hull_category": category})
		h.api.writeJSON(writer, http.StatusOK, catalog)
	})
}

func TestExcludedOwnedHullStillProtectsUpkeepBeforeRefit(t *testing.T) {
	t.Parallel()
	h := newRecoveryHarness(t)
	installExcludedHullCatalog(h, "oceanic")
	valid := localIntentTradingShipRow(recoveryShipBlastoise, "Blastoise")
	valid["ship"].(map[string]any)["cannons_count"] = 2
	valid["effective_hold_capacity_tons"] = 56
	excluded := sailingShipRow(recoveryShipCharizard, "Charizard", recoveryFarPort, recoveryHomePort)
	excluded["ship"].(map[string]any)["ship_class"] = "excluded-barque"
	excluded["hold_capacity_tons"] = 220
	h.api.setFleet([]map[string]any{valid, excluded})
	runner := h.runner(primaryTestKey)
	loanCapacityRefresh(t, h, runner)
	if _, exists := findHull(runner.stateCache.Catalog, "excluded-barque"); exists {
		t.Fatal("excluded class returned to capacity planning")
	}
	billingCatalog := runner.stateCache.Catalog
	billingCatalog.ShipClasses = append(append([]Hull(nil), billingCatalog.ShipClasses...), Hull{ID: "excluded-barque", HullCategory: "oceanic", HoldCapacityTons: 220})
	want := DailyUpkeep(runner.stateCache.Ships, billingCatalog, 0)
	if got := DailyUpkeep(runner.stateCache.Ships, runner.stateCache.Catalog, 0); got != want || got < 22_000 {
		t.Fatalf("excluded owned oceanic upkeep disappeared: got=%d want=%d", got, want)
	}
	required, err := runner.treasuryReserves(h.ctx)
	if err != nil {
		t.Fatal(err)
	}
	if required["north"] < runner.opts.ReservePence+3*want {
		t.Fatalf("protected home bills omitted the excluded hull: %+v upkeep=%d", required, want)
	}
	ship := harnessShip(t, runner, recoveryShipBlastoise)
	if changed, err := runner.startMaintenance(h.ctx, ship, "refit", "/v1/shipyard/modify-ship", map[string]any{"ship_id": ship.Ship.ID, "target_cannons_count": 4}); err != nil || !changed {
		t.Fatalf("stage boundary refit: %t %v", changed, err)
	}
	mission, data := h.mission(ship.Ship.ID)
	const cost = int64(30_000)
	for _, delta := range []int64{-1, 0, 1} {
		h.api.setVaultBalance("north", required["north"]+cost+delta)
		loanCapacityRefresh(t, h, runner)
		err := runner.newActionPreflight(h.ctx, mission, &data)
		if (err == nil) != (delta >= 0) {
			t.Fatalf("excluded-hull reserve boundary %+dp: %v", delta, err)
		}
		if delta < 0 && !strings.Contains(err.Error(), "protected regional reserves") {
			t.Fatalf("refit failed before its reserve boundary: %v", err)
		}
	}
	h.api.setVaultBalance("north", required["north"]+cost-1)
	loanCapacityRefresh(t, h, runner)
	if _, err := runner.dispatch(h.ctx, mission, &data); err != nil {
		t.Fatal(err)
	}
	if h.api.mutationCount("/v1/shipyard/modify-ship") != 0 {
		t.Fatal("refit spent the excluded hull's protected fleet bills")
	}
}

func TestExcludedRegionalAndUnknownHullBillsDoNotFreezeTrading(t *testing.T) {
	t.Parallel()
	for _, category := range []string{"regional", "unknown", ""} {
		for _, hold := range []int64{220, 0} {
			t.Run(category+"/"+fmt.Sprint(hold), func(t *testing.T) {
				h := newRecoveryHarness(t)
				installExcludedHullCatalog(h, category)
				base := h.api.server.Config.Handler
				h.api.server.Config.Handler = http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
					if request.Method == http.MethodGet && request.URL.Path == "/v1/market/book" && request.URL.Query().Get("port_id") == recoveryHomePort {
						h.api.mu.Lock()
						book := h.api.bookDocument(recoveryHomePort)
						h.api.mu.Unlock()
						grain := book["books"].([]any)[0].(map[string]any)
						grain["depth"].(map[string]any)["bids"] = []any{
							map[string]any{"order_id": "local-liquidation", "price_pence": 90, "remaining_quantity": 10_000},
						}
						h.api.writeJSON(writer, http.StatusOK, book)
						return
					}
					if request.Method == http.MethodPost && request.URL.Path == "/v1/market/place-order" {
						body, err := io.ReadAll(request.Body)
						if err != nil {
							t.Error(err)
							writer.WriteHeader(http.StatusBadRequest)
							return
						}
						var order struct {
							ShipID      string `json:"ship_id"`
							PortID      string `json:"port_id"`
							CommodityID string `json:"commodity_id"`
							Side        string `json:"side"`
							Quantity    int64  `json:"quantity"`
							Price       int64  `json:"price_pence"`
						}
						if err := json.Unmarshal(body, &order); err != nil {
							t.Error(err)
							writer.WriteHeader(http.StatusBadRequest)
							return
						}
						if order.Side == "ask" {
							h.api.mu.Lock()
							defer h.api.mu.Unlock()
							if order.ShipID != recoveryShipBlastoise || order.PortID != recoveryHomePort ||
								order.CommodityID != recoveryGrain || order.Quantity != 10 || order.Price > 90 {
								t.Errorf("invalid liquidation request: %+v", order)
								writer.WriteHeader(http.StatusConflict)
								return
							}
							loaded := h.api.fleet[0]
							loaded["cargo_manifest"] = map[string]any{}
							loaded["cargo_used_tons"] = int64(0)
							gross := order.Quantity * 90
							customs := ceilBPS(gross, 200)
							h.api.vaultBalance["north"] += gross - customs
							h.api.record(request, body, http.StatusOK)
							h.api.writeJSON(writer, http.StatusOK, map[string]any{
								"commodity_id": recoveryGrain, "filled_quantity": order.Quantity,
								"total_gross_pence": gross, "customs_duty_pence": customs,
							})
							return
						}
						request.Body = io.NopCloser(bytes.NewReader(body))
					}
					base.ServeHTTP(writer, request)
				})
				loaded := stationaryShipRow(recoveryShipBlastoise, "Blastoise", recoveryHomePort, "berthed", 10, 4)
				excluded := sailingShipRow(recoveryShipCharizard, "Charizard", recoveryFarPort, recoveryHomePort)
				excluded["ship"].(map[string]any)["ship_class"] = "excluded-barque"
				excluded["hold_capacity_tons"] = hold
				h.api.setFleet([]map[string]any{loaded, excluded})
				h.api.setFactorPorts(recoveryHomePort, recoveryFarPort)
				h.putMission(recoveryShipBlastoise, "paid-cargo-before-catalog-gap", "arrival", missionData{
					Plan: Plan{Kind: "spot", ShipID: recoveryShipBlastoise, Origin: recoveryFarPort,
						Destination: recoveryHomePort, Commodity: recoveryGrain, Quantity: 10,
						BuyLimitPence: 100, SellLimitPence: 90, TargetStoresTons: 4,
						Economics: Economics{Basis: "destination", GameDays: 1, BallastRepeatGameDays: 1}},
					BuyDone: true, BoughtQuantity: 10, AcquisitionPence: 1_000, CostsKnown: true,
					Eligible: map[string]int64{recoveryGrain: 10},
				})
				runner := h.runner(primaryTestKey)
				// Isolate existing trading from independently priced new hulls.
				runner.opts.CapacityPaybackDays = 0
				loanCapacityRefresh(t, h, runner)
				tons := hold
				if tons == 0 {
					for _, hull := range runner.stateCache.Catalog.ShipClasses {
						tons = max(tons, hull.HoldCapacityTons)
					}
				}
				var expected int64
				for _, billingCategory := range []string{"coastal", "oceanic"} {
					if category == "regional" && billingCategory == "oceanic" {
						continue
					}
					catalog := runner.stateCache.Catalog
					catalog.ShipClasses = append(append([]Hull(nil), catalog.ShipClasses...),
						Hull{ID: "excluded-barque", HullCategory: billingCategory, HoldCapacityTons: tons})
					expected = max(expected, DailyUpkeep(runner.stateCache.Ships, catalog, 0))
				}
				upkeep := DailyUpkeep(runner.stateCache.Ships, runner.stateCache.Catalog, 0)
				if upkeep < expected || upkeep == maximumPence {
					t.Fatalf("missing class facts did not produce a finite conservative bill: got=%d want-at-least=%d", upkeep, expected)
				}
				required, err := runner.treasuryReserves(h.ctx)
				if err != nil || required["north"] < runner.opts.ReservePence+3*upkeep {
					t.Fatalf("conservative bill was not reserved: reserves=%v err=%v", required, err)
				}
				h.api.setVaultBalance("north", required["north"]+200_000)
				h.api.setVaultBalance("south", required["south"]+200_000)
				for range 24 {
					if _, err := h.tick(runner); err != nil {
						t.Fatal(err)
					}
					mission, data := h.mission(recoveryShipBlastoise)
					if data.Action != nil {
						if err := runner.newActionPreflight(h.ctx, mission, &data); err != nil {
							t.Fatalf("ordinary %s fixture lost its first-send authority: %v", data.Action.Stage, err)
						}
					}
					if h.api.mutationCount("/v1/fleet/depart") > 0 {
						break
					}
				}
				sold := false
				for _, call := range h.api.mutationsFor("/v1/market/place-order") {
					var request struct {
						ShipID string `json:"ship_id"`
						Side   string `json:"side"`
					}
					if err := json.Unmarshal(call.Body, &request); err != nil {
						t.Fatal(err)
					}
					sold = sold || request.ShipID == recoveryShipBlastoise && request.Side == "ask" && call.Status == http.StatusOK
				}
				if !sold || h.api.mutationCount("/v1/fleet/depart") != 1 {
					t.Fatalf("conservative bill stranded ordinary selling/departure: sold=%t holds=%v", sold, runner.holding)
				}
			})
		}
	}
}
