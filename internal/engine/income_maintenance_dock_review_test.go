package engine

import (
	"encoding/json"
	"io"
	"net/http"
	"testing"
)

func incomeArmamentDockFixture(t *testing.T) (*recoveryHarness, *Runner, int64) {
	t.Helper()
	h, runner, _, _ := idleMaintenanceFixture(t, true)
	h.api.mu.Lock()
	row := h.api.fleet[0]
	row["ship"].(map[string]any)["cannons_count"] = 0
	row["effective_hold_capacity_tons"] = 60
	row["stores"] = map[string]any{"provisions_tons": 4, "sea_days_remaining": 5}
	h.api.vaultBalance["north"] = 1_000_000
	h.api.mu.Unlock()
	// The shared maintenance API already applies real berth transitions.
	// Add only the existing observed 15000p-per-cannon hardware transaction.
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
		ship := row["ship"].(map[string]any)
		cost := (payload.Target - int64(ship["cannons_count"].(int))) * 15_000
		if payload.ShipID != recoveryShipBlastoise || ship["status"] != "berthed" ||
			ship["current_port_id"] != recoveryHomePort || payload.Target != 4 || cost <= 0 || h.api.vaultBalance["north"] < cost {
			h.api.record(request, body, http.StatusConflict)
			h.api.writeProblem(writer, http.StatusConflict, "unfunded_refit", "refit cannot be applied to the observed hull")
			return
		}
		h.api.vaultBalance["north"] -= cost
		ship["cannons_count"] = int(payload.Target)
		row["effective_hold_capacity_tons"] = 60 - 2*int(payload.Target)
		h.api.record(request, body, http.StatusOK)
		h.api.writeJSON(writer, http.StatusOK, map[string]any{"ship_id": payload.ShipID, "cannons_count": payload.Target})
	})
	insurance := installUWExecProxy(t, h)
	piracyInsuranceBoard(insurance, piracyInsurancePolicy())
	// This future cargo plan is still unattempted and insured. The current
	// maintenance book has provisions only, so it also lacks a live cargo quote.
	h.putMission(recoveryShipBlastoise, "insured-pending-armament", "provision", missionData{
		Plan: Plan{Kind: "spot", ShipID: recoveryShipBlastoise, Origin: recoveryHomePort,
			Destination: recoveryFarPort, Commodity: recoveryGrain, Quantity: 10,
			BuyLimitPence: 100, TargetStoresTons: 4},
		CostsKnown: true,
	})
	incomeReviewRefresh(t, h, runner)
	piracyInsuranceRequirePolicies(t, runner, piracyInsurancePolicy())
	return h, runner, 1_000_000
}

func TestIncomeMaintenanceDockReviewInsuredPendingIncomeAllowsIndependentArmament(t *testing.T) {
	t.Parallel()
	for _, restart := range []bool{false, true} {
		name := "first-send"
		if restart {
			name = "restart-and-limiter-wait"
		}
		t.Run(name, func(t *testing.T) {
			h, runner, initialCash := incomeArmamentDockFixture(t)
			ship, _ := runner.ship(recoveryShipBlastoise)
			mission, data := h.mission(ship.Ship.ID)
			if _, err := runner.driveMission(h.ctx, ship, mission, &data); err != nil {
				t.Fatal(err)
			}
			_, staged := h.mission(ship.Ship.ID)
			if staged.Action == nil || staged.Action.Stage != "dock" {
				t.Fatalf("affordable missing armament did not stage its independent maintenance berth: %+v", staged)
			}
			var waited *bool
			if restart {
				runner = h.runner(primaryTestKey)
				waited = incomeReviewDuringWait(t, h, runner, func() {
					// Loss of a future cargo receipt observation cannot invalidate
					// this independent, already-affordable hardware maintenance.
					meta := runner.meta["books/"+recoveryFarPort]
					meta.Stale = true
					runner.meta["books/"+recoveryFarPort] = meta
				})
			}
			for range 12 {
				if _, err := h.tick(runner); err != nil {
					t.Fatal(err)
				}
				if h.api.mutationCount("/v1/shipyard/modify-ship") > 0 {
					break
				}
			}
			if restart && !*waited {
				t.Fatal("restarted independent maintenance never reached the limiter boundary")
			}
			for _, path := range []string{"/v1/fleet/dock", "/v1/shipyard/modify-ship"} {
				calls := h.api.mutationsFor(path)
				if len(calls) != 1 || calls[0].Status != http.StatusOK {
					t.Fatalf("insured future cargo disabled or repeated independent armament maintenance: %s %+v", path, calls)
				}
			}
			incomeReviewRefresh(t, h, runner)
			armed, _ := runner.ship(ship.Ship.ID)
			input, err := runner.committedPlanningInput(h.ctx, armed)
			if err != nil {
				t.Fatal(err)
			}
			if armed.Ship.Status != "berthed" || armed.Ship.CannonsCount != 4 ||
				armed.Stores.ProvisionsTons != 4 || !emptyCargo(armed) ||
				input.Balances["north"] != initialCash-60_000 || input.Balances["north"] < input.ReserveByRegion["north"] {
				t.Fatalf("maintenance did not install hardware while preserving stores, cargo and regional cash: ship=%+v balances=%v reserves=%v", armed, input.Balances, input.ReserveByRegion)
			}
			// Keep driving the retained future plan after hardware completes.
			// Maintenance authorization must not become permission to buy cargo.
			for range 3 {
				if _, err := h.tick(runner); err != nil {
					t.Fatal(err)
				}
			}
			if h.api.mutationCount("/v1/market/place-order") != 0 || h.api.mutationCount("/v1/treasury/remit") != 0 || len(h.pending()) != 0 {
				t.Fatalf("independent armament authorization leaked into prohibited cargo funding or procurement: orders=%+v remits=%+v pending=%+v",
					h.api.mutationsFor("/v1/market/place-order"), h.api.mutationsFor("/v1/treasury/remit"), h.pending())
			}
		})
	}

}

func TestIncomeMaintenanceDockReviewExpiredVoyageStillCannotOpenDiscretionaryBerth(t *testing.T) {
	t.Parallel()
	h, runner, ship, plan := incomeReviewFixture(t, "charter", true, 4)
	h.api.mu.Lock()
	h.api.fleet[0]["ship"].(map[string]any)["status"] = "anchored_roadstead"
	h.api.mu.Unlock()
	incomeReviewRefresh(t, h, runner)
	ship, _ = runner.ship(ship.Ship.ID)
	mission, data := incomeReviewStage(t, h, runner, ship, plan, "dock")
	action := *data.Action
	waited := incomeReviewDuringWait(t, h, runner, func() { incomeReviewExpire(runner, "charter") })
	if _, err := runner.dispatch(h.ctx, mission, &data); err != nil {
		t.Fatal(err)
	}
	if !*waited {
		t.Fatal("live, already-armed voyage did not reach the limiter boundary")
	}
	incomeReviewRequireNoSend(t, h, action)

}
