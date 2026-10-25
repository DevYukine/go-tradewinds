package engine

// Consumer-visible idle replenishment regressions. Every HTTP request targets
// the recovery harness's httptest server and every journal uses its disposable
// TEST_DATABASE_URL schema. Roadstead visibility deliberately contains no asks.

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"sync/atomic"
	"testing"

	"github.com/DevYukine/go-tradewinds/internal/store"
)

type idleMaintenanceAPI struct {
	h               *recoveryHarness
	downstream      http.Handler
	price           int64 // protected by h.api.mu
	customs         int64
	quoteMode       string
	rateMode        string
	fillLimit       *int64
	rejectProvision bool
	loseDockReceipt atomic.Bool
	loseRemit       atomic.Bool
}

func idleMaintenanceFixture(t *testing.T, roadstead bool) (*recoveryHarness, *Runner, *idleMaintenanceAPI, map[string]int64) {
	t.Helper()
	h := newRecoveryHarness(t)
	status := "berthed"
	if roadstead {
		status = "anchored_roadstead"
	}
	row := stationaryShipRow(recoveryShipBlastoise, "Blastoise", recoveryHomePort, status, 0, 4)
	row["stores"] = map[string]any{"provisions_tons": 1.05, "sea_days_remaining": 10.5}
	h.api.setFleet([]map[string]any{
		row,
		sailingShipRow(recoveryShipCharizard, "Charizard", recoveryHomePort, recoveryFarPort),
	})
	h.api.setAutoRemit(true, 10_000_000)
	h.putMission(recoveryShipCharizard, "idle-maintenance-other-voyage", "sailing", missionData{
		Plan:             Plan{Kind: "recovery", ShipID: recoveryShipCharizard, Origin: recoveryHomePort, Destination: recoveryFarPort},
		InitialLedgerIDs: map[string]bool{}, Eligible: map[string]int64{},
	})
	proxy := &idleMaintenanceAPI{h: h, downstream: h.api.server.Config.Handler, price: 12_345, customs: 250}
	h.api.server.Config.Handler = proxy
	runner := refreshedRunner(t, h)
	input, err := runner.committedPlanningInput(h.ctx, harnessShip(t, runner, recoveryShipBlastoise))
	if err != nil {
		t.Fatal(err)
	}
	reserves := clonePence(input.ReserveByRegion)
	if reserves["north"] <= 0 || reserves["south"] <= 0 {
		t.Fatalf("fixture did not establish real local and donor obligations: %v", reserves)
	}
	h.api.setVaultBalance("north", reserves["north"])
	h.api.setVaultBalance("south", reserves["south"])
	actionLogRefresh(t, h, runner)
	return h, runner, proxy, reserves
}

func (p *idleMaintenanceAPI) ServeHTTP(writer http.ResponseWriter, request *http.Request) {
	api := p.h.api
	api.mu.Lock()
	authorized := api.authorized(request)
	api.mu.Unlock()
	if !authorized {
		p.downstream.ServeHTTP(writer, request)
		return
	}
	if request.Method == http.MethodGet && request.URL.Path == "/v1/market/book" && request.URL.Query().Get("port_id") == recoveryHomePort {
		api.mu.Lock()
		mode, price, customs := p.quoteMode, p.price, p.customs
		status := api.fleet[0]["ship"].(map[string]any)["status"]
		api.mu.Unlock()
		if mode == "failed" {
			api.writeProblem(writer, http.StatusForbidden, "quay_unavailable", "synthetic quote outage")
			return
		}
		book := api.bookDocument(recoveryHomePort)
		book["customs_bps"] = customs
		book["books"] = []any{}
		if status == "anchored_roadstead" {
			book["visibility"] = "roadstead"
		} else if mode != "missing" {
			book["books"] = []any{map[string]any{
				"commodity_id": "provisions",
				"depth": map[string]any{"bids": []any{}, "asks": []any{map[string]any{
					"order_id": "idle-maintenance-quay-ask", "price_pence": price, "remaining_quantity": 100,
				}}},
			}}
		}
		api.writeJSON(writer, http.StatusOK, book)
		return
	}
	if request.Method == http.MethodGet && request.URL.Path == "/v1/treasury/rates" {
		api.mu.Lock()
		mode := p.rateMode
		api.mu.Unlock()
		if mode == "failed" {
			api.writeProblem(writer, http.StatusForbidden, "rates_unavailable", "synthetic rates outage")
			return
		}
		if mode == "missing" {
			api.writeJSON(writer, http.StatusOK, []Rate{})
			return
		}
	}
	if request.Method == http.MethodPost && request.URL.Path == "/v1/treasury/remit" && p.loseRemit.Load() {
		body, err := io.ReadAll(request.Body)
		if err != nil {
			p.h.t.Errorf("read lost maintenance remittance: %v", err)
			writer.WriteHeader(http.StatusBadRequest)
			return
		}
		uwExecServeLost(func(request *http.Request, body []byte, status int) {
			api.mu.Lock()
			api.record(request, body, status)
			api.mu.Unlock()
		})(writer, request, body)
		return
	}
	if request.Method != http.MethodPost || request.URL.Path != "/v1/fleet/dock" && request.URL.Path != "/v1/fleet/offshore" && request.URL.Path != "/v1/market/place-order" {
		p.downstream.ServeHTTP(writer, request)
		return
	}
	body, err := io.ReadAll(request.Body)
	if err != nil {
		p.h.t.Errorf("read maintenance request: %v", err)
		writer.WriteHeader(http.StatusBadRequest)
		return
	}
	var payload struct {
		ShipID      string `json:"ship_id"`
		Port        string `json:"port_id"`
		Commodity   string `json:"commodity_id"`
		Side        string `json:"side"`
		Quantity    int64  `json:"quantity"`
		Price       int64  `json:"price_pence"`
		IntoStores  bool   `json:"into_stores"`
		TimeInForce string `json:"time_in_force"`
	}
	if err := json.Unmarshal(body, &payload); err != nil {
		p.h.t.Errorf("decode maintenance request: %v", err)
		writer.WriteHeader(http.StatusBadRequest)
		return
	}
	api.mu.Lock()
	defer api.mu.Unlock()
	row := api.fleet[0]
	ship := row["ship"].(map[string]any)
	if payload.ShipID != recoveryShipBlastoise {
		api.record(request, body, http.StatusConflict)
		api.writeProblem(writer, http.StatusConflict, "unobserved_maintenance_ship", "no matching idle hull")
		return
	}
	if request.URL.Path == "/v1/fleet/dock" && p.loseDockReceipt.Load() {
		// The berth transition committed, but its receipt is unavailable.
		// Every transport retry keeps the same logical dock identity.
		ship["status"] = "berthed"
		api.record(request, body, 0)
		connection, _, err := writer.(http.Hijacker).Hijack()
		if err != nil {
			p.h.t.Errorf("lose dock receipt: %v", err)
			return
		}
		if err := connection.Close(); err != nil {
			p.h.t.Errorf("close dock receipt connection: %v", err)
		}
		return
	}
	if request.URL.Path == "/v1/fleet/dock" || request.URL.Path == "/v1/fleet/offshore" {
		before, after := "anchored_roadstead", "berthed"
		if request.URL.Path == "/v1/fleet/offshore" {
			before, after = after, before
		}
		if ship["status"] != before {
			api.record(request, body, http.StatusConflict)
			api.writeProblem(writer, http.StatusConflict, "invalid_maintenance_navigation", "navigation disagrees with observed presence")
			return
		}
		ship["status"] = after
		api.record(request, body, http.StatusOK)
		api.writeJSON(writer, http.StatusOK, map[string]any{
			"ship_id": payload.ShipID, "status": after,
			"berth_dues_pence": 0, "berth_dues_debt_pence": 0,
		})
		return
	}
	gross := payload.Quantity * p.price
	customs := ceilBPS(gross, p.customs)
	if ship["status"] != "berthed" || payload.Port != recoveryHomePort || payload.Commodity != "provisions" ||
		payload.Side != "bid" || !payload.IntoStores || payload.TimeInForce != "ioc" ||
		payload.Quantity <= 0 || payload.Quantity > 100 || payload.Price < p.price || api.vaultBalance["north"] < gross+customs {
		api.record(request, body, http.StatusConflict)
		api.writeProblem(writer, http.StatusConflict, "invalid_maintenance_purchase", "purchase lacks observed quay stock or funding")
		return
	}
	if p.rejectProvision {
		api.record(request, body, http.StatusConflict)
		api.writeProblem(writer, http.StatusConflict, "store_order_rejected", "synthetic definitive refusal without a fill")
		return
	}
	filled := payload.Quantity
	if p.fillLimit != nil {
		filled = min(filled, *p.fillLimit)
	}
	gross = filled * p.price
	customs = ceilBPS(gross, p.customs)
	stores := row["stores"].(map[string]any)
	stores["provisions_tons"] = stores["provisions_tons"].(float64) + float64(filled)
	api.vaultBalance["north"] -= gross + customs
	api.record(request, body, http.StatusOK)
	api.writeJSON(writer, http.StatusOK, map[string]any{
		"commodity_id": "provisions", "filled_quantity": filled,
		"total_gross_pence": gross, "customs_duty_pence": customs,
	})
}

func idleMaintenanceFundDonor(t *testing.T, h *recoveryHarness, runner *Runner, proxy *idleMaintenanceAPI) (map[string]int64, int64) {
	t.Helper()
	actionLogRefresh(t, h, runner)
	input, err := runner.committedPlanningInput(h.ctx, harnessShip(t, runner, recoveryShipBlastoise))
	if err != nil {
		t.Fatal(err)
	}
	h.api.mu.Lock()
	cost := proxy.price + ceilBPS(proxy.price, proxy.customs)
	h.api.mu.Unlock()
	gross, err := GrossRemittance(cost+1, 100)
	if err != nil {
		t.Fatal(err)
	}
	balance := input.ReserveByRegion["south"] + gross + 1_000
	h.api.setVaultBalance("south", balance)
	actionLogRefresh(t, h, runner)
	return input.ReserveByRegion, balance
}

func idleMaintenanceUntilRemit(t *testing.T, h *recoveryHarness, runner *Runner) (*store.Mission, missionData) {
	t.Helper()
	for range 12 {
		mission, data, err := runner.loadMission(h.ctx, recoveryShipBlastoise)
		if err != nil {
			t.Fatal(err)
		}
		if mission != nil && data.Action != nil && data.Action.Path == "/v1/treasury/remit" {
			return mission, data
		}
		if _, err := h.tick(runner); err != nil {
			t.Fatal(err)
		}
	}
	t.Fatalf("idle replenishment never staged observed-fee funding: holds %+v remits %+v", runner.holding, h.api.mutationsFor("/v1/treasury/remit"))
	return nil, missionData{}
}

func idleMaintenanceUntilPurchase(t *testing.T, h *recoveryHarness, runner *Runner) (*store.Mission, missionData) {
	t.Helper()
	for range 12 {
		if _, err := h.tick(runner); err != nil {
			t.Fatal(err)
		}
		if h.api.mutationCount("/v1/market/place-order") > 0 {
			return h.mission(recoveryShipBlastoise)
		}
	}
	t.Fatalf("funded maintenance did not buy stores: holds %+v", runner.holding)
	return nil, missionData{}
}

func idleMaintenanceRequireRetired(t *testing.T, h *recoveryHarness, runner *Runner) {
	t.Helper()
	for range 3 {
		actionLogRefresh(t, h, runner)
		mission, data, err := runner.loadMission(h.ctx, recoveryShipBlastoise)
		if err != nil {
			t.Fatal(err)
		}
		if mission == nil {
			return
		}
		if _, err := runner.driveMission(h.ctx, harnessShip(t, runner, recoveryShipBlastoise), mission, &data); err != nil {
			t.Fatal(err)
		}
	}
	t.Fatalf("satisfied maintenance retained an undrivable controller: holds %+v", runner.holding)
}

func TestIdleMaintenanceRoadsteadInspectsOnceThenWaitsDurably(t *testing.T) {
	h, runner, _, reserves := idleMaintenanceFixture(t, true)
	if book := runner.stateCache.Markets[recoveryHomePort]; book.Visibility != "roadstead" || len(book.Books) != 0 {
		t.Fatalf("roadstead fixture leaked executable prices: %+v", book)
	}
	for tick := range 10 {
		if tick == 5 {
			runner = h.runner(primaryTestKey)
		}
		if _, err := h.tick(runner); err != nil {
			t.Fatal(err)
		}
	}
	if h.api.mutationCount("/v1/fleet/dock") != 1 || h.api.mutationCount("/v1/fleet/offshore") != 0 ||
		h.api.mutationCount("/v1/market/place-order") != 0 || h.api.mutationCount("/v1/treasury/remit") != 0 {
		t.Fatalf("unfunded one-time inspection became a navigation/spending loop: docks %+v offshore %+v buys %+v remits %+v",
			h.api.mutationsFor("/v1/fleet/dock"), h.api.mutationsFor("/v1/fleet/offshore"), h.api.mutationsFor("/v1/market/place-order"), h.api.mutationsFor("/v1/treasury/remit"))
	}
	_, data := h.mission(recoveryShipBlastoise)
	if data.Plan.Kind != "maintenance" || data.Action != nil || data.ProvisionsPence != 0 || data.BoughtQuantity != 0 {
		t.Fatalf("inspection lost durable pending maintenance or invented spending: %+v", data)
	}
	input, err := runner.committedPlanningInput(h.ctx, harnessShip(t, runner, recoveryShipBlastoise))
	if err != nil {
		t.Fatal(err)
	}
	if input.Balances["north"] != reserves["north"] || input.Balances["south"] != reserves["south"] ||
		!runner.fresh("books/"+recoveryHomePort) || runner.stateCache.Markets[recoveryHomePort].Visibility != "quay" {
		t.Fatalf("wait spent protected funds or discarded the observed quay quote: balances %v reserves %v", input.Balances, reserves)
	}
}

func TestIdleMaintenanceFreshDonorFundsExactCurrentStoresOnce(t *testing.T) {
	h, runner, proxy, reserves := idleMaintenanceFixture(t, true)
	for range 4 {
		if _, err := h.tick(runner); err != nil {
			t.Fatal(err)
		}
	}
	// The eventual purchase must use the new authoritative quay quote,
	// never a guessed roadstead price or the earlier inspection's price.
	h.api.mu.Lock()
	proxy.price, proxy.customs = 20_000, 333
	h.api.mu.Unlock()
	_, donorBefore := idleMaintenanceFundDonor(t, h, runner, proxy)
	_, _ = idleMaintenanceUntilRemit(t, h, runner)
	mission, data := idleMaintenanceUntilPurchase(t, h, runner)
	remits := h.api.mutationsFor("/v1/treasury/remit")
	buys := h.api.mutationsFor("/v1/market/place-order")
	if len(remits) != 1 || remits[0].Status != http.StatusOK || len(buys) != 1 || buys[0].Status != http.StatusOK {
		t.Fatalf("fresh funded maintenance did not execute once: remits %+v buys %+v", remits, buys)
	}
	var transfer struct {
		From  string `json:"from_region"`
		To    string `json:"to_region"`
		Gross int64  `json:"gross_amount_pence"`
	}
	var order struct {
		Quantity   int64 `json:"quantity"`
		Price      int64 `json:"price_pence"`
		IntoStores bool  `json:"into_stores"`
	}
	if err := json.Unmarshal(remits[0].Body, &transfer); err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(buys[0].Body, &order); err != nil {
		t.Fatal(err)
	}
	cost := int64(20_666)
	// The store quote reserves one penny per ton for purchase/customs
	// rounding, independently of remittance-fee rounding.
	requiredStoreCash := cost + order.Quantity
	fee := transfer.Gross * 100 / 10_000
	net := transfer.Gross - fee
	expectedGross, err := GrossRemittance(requiredStoreCash, 100)
	if err != nil {
		t.Fatal(err)
	}
	// GrossRemittance protects against a ceiling-rounded fee; this
	// observed receipt charges the floor-rounded fee instead.
	receiptRoundingAllowance := ceilBPS(expectedGross, 100) - fee
	if transfer.From != "south" || transfer.To != "north" || transfer.Gross != expectedGross ||
		receiptRoundingAllowance < 0 || receiptRoundingAllowance > 1 || net != requiredStoreCash+receiptRoundingAllowance ||
		order.Quantity != 1 || order.Price != 20_000 || !order.IntoStores {
		t.Fatalf("maintenance funding/purchase did not match the exact observed cost and fee: remit %+v order %+v", transfer, order)
	}
	if data.FeesPence != fee || data.ProvisionsPence != cost || data.ExpensesPence != 0 || data.AcquisitionPence != 0 || data.BoughtQuantity != 0 {
		t.Fatalf("maintenance treated remittance principal as expense or stores as cargo: %+v", data)
	}
	h.api.mu.Lock()
	local, donor := h.api.vaultBalance["north"], h.api.vaultBalance["south"]
	h.api.mu.Unlock()
	if local != reserves["north"]+net-cost || donor != donorBefore-transfer.Gross || local < reserves["north"] || donor < reserves["south"] {
		t.Fatalf("maintenance crossed protected regional reserves: local %d donor %d reserves %v", local, donor, reserves)
	}
	if data.Action != nil || mission.ShipID != recoveryShipBlastoise {
		t.Fatalf("completed stores did not consume their immutable receipt: %+v", data)
	}
	idleMaintenanceRequireRetired(t, h, runner)
	if h.api.mutationCount("/v1/fleet/dock") != 1 || h.api.mutationCount("/v1/fleet/offshore") != 0 || len(h.pending()) != 0 {
		t.Fatal("maintenance completion restarted navigation or retained unresolved spending")
	}
}

func TestIdleMaintenanceIncompleteQuotesAndRatesFailClosed(t *testing.T) {
	for _, unavailable := range []string{"missing quote", "failed quote", "missing rates", "failed rates"} {
		t.Run(unavailable, func(t *testing.T) {
			h, runner, proxy, _ := idleMaintenanceFixture(t, false)
			// Start from a legitimate durable wait: stale global rates may
			// correctly stop the work loop before admitting any new intent.
			h.putMission(recoveryShipBlastoise, "idle-evidence-wait", "maintenance", missionData{
				Plan:       Plan{Kind: "maintenance", ShipID: recoveryShipBlastoise},
				CostsKnown: true, InitialLedgerIDs: map[string]bool{}, Eligible: map[string]int64{},
			})
			idleMaintenanceFundDonor(t, h, runner, proxy)
			h.api.mu.Lock()
			switch unavailable {
			case "missing quote":
				proxy.quoteMode = "missing"
			case "failed quote":
				proxy.quoteMode = "failed"
			case "missing rates":
				proxy.rateMode = "missing"
			case "failed rates":
				proxy.rateMode = "failed"
			}
			h.api.mu.Unlock()
			for range 4 {
				if _, err := h.tick(runner); err != nil {
					t.Fatal(err)
				}
			}
			if h.api.mutationCount("/v1/treasury/remit") != 0 || h.api.mutationCount("/v1/market/place-order") != 0 ||
				h.api.mutationCount("/v1/fleet/offshore") != 0 || h.api.mutationCount("/v1/fleet/dock") != 0 {
				t.Fatalf("unavailable %s caused spending or speculative navigation", unavailable)
			}
			_, data := h.mission(recoveryShipBlastoise)
			if data.Plan.Kind != "maintenance" || data.Action != nil || len(h.pending()) != 0 {
				t.Fatalf("unavailable evidence lost durable maintenance: %+v", data)
			}
		})
	}
}

func TestIdleMaintenanceFundingPreservesOtherMissionDonorCommitments(t *testing.T) {
	h, runner, proxy, _ := idleMaintenanceFixture(t, false)
	baseline, err := runner.committedPlanningInput(h.ctx, harnessShip(t, runner, recoveryShipBlastoise))
	if err != nil {
		t.Fatal(err)
	}
	h.api.setFleet([]map[string]any{
		stationaryShipRow(recoveryShipBlastoise, "Blastoise", recoveryHomePort, "berthed", 0, 4),
		stationaryShipRow(recoveryShipCharizard, "Charizard", recoveryFarPort, "berthed", 0, 4),
	})
	h.api.mu.Lock()
	h.api.fleet[0]["stores"] = map[string]any{"provisions_tons": 1.05, "sea_days_remaining": 10.5}
	h.api.mu.Unlock()
	h.putMission(recoveryShipCharizard, "other-reserved-procurement", "buy", missionData{
		Plan: Plan{Kind: "spot", ShipID: recoveryShipCharizard, Origin: recoveryFarPort, Destination: recoveryHomePort,
			Commodity: recoveryGrain, Quantity: 10, BuyLimitPence: 500, TargetStoresTons: 4,
			Economics: Economics{GameDays: 1}},
		CostsKnown: true,
	})
	actionLogRefresh(t, h, runner)
	input, err := runner.committedPlanningInput(h.ctx, harnessShip(t, runner, recoveryShipBlastoise))
	if err != nil {
		t.Fatal(err)
	}
	if input.ReserveByRegion["south"] <= baseline.ReserveByRegion["south"] {
		t.Fatal("fixture did not establish the other hull's donor-side procurement commitment")
	}
	h.api.setVaultBalance("north", input.ReserveByRegion["north"])
	h.api.setVaultBalance("south", input.ReserveByRegion["south"]-1)
	actionLogRefresh(t, h, runner)
	ship := harnessShip(t, runner, recoveryShipBlastoise)
	if _, err := runner.maintain(h.ctx, ship, nil, nil); err != nil {
		t.Fatal(err)
	}
	mission, data := h.mission(ship.Ship.ID)
	if data.Action != nil || h.api.mutationCount("/v1/treasury/remit") != 0 || h.api.mutationCount("/v1/market/place-order") != 0 {
		t.Fatal("maintenance borrowed money protected for another hull's purchase")
	}
	protected, donorBefore := idleMaintenanceFundDonor(t, h, runner, proxy)
	for range 8 {
		actionLogRefresh(t, h, runner)
		mission, data = h.mission(ship.Ship.ID)
		if _, err := runner.driveMission(h.ctx, harnessShip(t, runner, ship.Ship.ID), mission, &data); err != nil {
			t.Fatal(err)
		}
		if h.api.mutationCount("/v1/market/place-order") > 0 {
			break
		}
	}
	remits := h.api.mutationsFor("/v1/treasury/remit")
	if len(remits) != 1 || remits[0].Status != http.StatusOK || h.api.mutationCount("/v1/market/place-order") != 1 {
		t.Fatalf("new truly unreserved donor cash did not fund maintenance: remits %+v buys %+v", remits, h.api.mutationsFor("/v1/market/place-order"))
	}
	h.api.mu.Lock()
	donor := h.api.vaultBalance["south"]
	h.api.mu.Unlock()
	if donor < protected["south"] || donor >= donorBefore {
		t.Fatalf("maintenance consumed donor obligations or recorded no actual debit: donor %d protected %d before %d", donor, protected["south"], donorBefore)
	}
	other, retained := h.mission(recoveryShipCharizard)
	if other.Quantity != 10 || retained.BuyDone || retained.BoughtQuantity != 0 || retained.Plan.BuyLimitPence != 500 {
		t.Fatalf("maintenance funding rewrote another ship's procurement: %+v", retained)
	}
}

func TestIdleMaintenanceFirstSendRechecksChangedDonor(t *testing.T) {
	for _, restart := range []bool{false, true} {
		name := "same process"
		if restart {
			name = "restart"
		}
		t.Run(name, func(t *testing.T) {
			h, runner, proxy, _ := idleMaintenanceFixture(t, false)
			protected, _ := idleMaintenanceFundDonor(t, h, runner, proxy)
			mission, data := idleMaintenanceUntilRemit(t, h, runner)
			id := data.Action.ID
			h.api.setVaultBalance("south", protected["south"])
			if restart {
				runner = h.runner(primaryTestKey)
			}
			actionLogRefresh(t, h, runner)
			if _, err := runner.dispatch(h.ctx, mission, &data); err != nil {
				t.Fatal(err)
			}
			_, retained := h.mission(recoveryShipBlastoise)
			if retained.Action != nil || len(h.pending()) != 0 || h.api.mutationCount("/v1/treasury/remit") != 0 || h.api.mutationCount("/v1/market/place-order") != 0 {
				t.Fatalf("never-attempted maintenance transfer spent changed donor reserves: %+v", retained)
			}
			if retained.LastActionID == id || retained.FeesPence != 0 || retained.ExpensesPence != 0 {
				t.Fatalf("cancelled transfer invented a financial outcome: %+v", retained)
			}
		})
	}
}

func TestIdleMaintenanceImmutableFundingRecovery(t *testing.T) {
	for _, state := range []string{"attempted", "completed"} {
		t.Run(state, func(t *testing.T) {
			h, runner, proxy, _ := idleMaintenanceFixture(t, false)
			idleMaintenanceFundDonor(t, h, runner, proxy)
			mission, data := idleMaintenanceUntilRemit(t, h, runner)
			original := data
			action := *data.Action
			proxy.loseRemit.Store(state == "attempted")
			if _, err := runner.dispatch(h.ctx, mission, &data); err != nil {
				t.Fatal(err)
			}
			before := h.api.mutationsFor(action.Path)
			if len(before) == 0 {
				t.Fatal("funding recovery fixture never crossed real HTTP")
			}
			if state == "completed" {
				if data.Action != nil || data.FeesPence <= 0 {
					t.Fatalf("completed funding fixture did not consume its receipt: %+v", data)
				}
				if err := runner.saveMission(h.ctx, mission, &original); err != nil {
					t.Fatal(err)
				}
			} else {
				if len(h.pending()) != 1 {
					t.Fatalf("ambiguous transfer was not retained: %+v", h.pending())
				}
				h.expireCooldown(recoveryShipBlastoise)
			}
			proxy.loseRemit.Store(false)
			h.api.mu.Lock()
			proxy.quoteMode, proxy.rateMode = "failed", "failed"
			h.api.mu.Unlock()
			restarted := h.runner(primaryTestKey)
			actionLogRefresh(t, h, restarted)
			mission, data = h.mission(recoveryShipBlastoise)
			if _, err := restarted.dispatch(h.ctx, mission, &data); err != nil {
				t.Fatal(err)
			}
			calls := h.api.mutationsFor(action.Path)
			want := len(before)
			if state == "attempted" {
				want++
			}
			if len(calls) != want {
				t.Fatalf("funding recovery replaced or resent an immutable outcome: %+v", calls)
			}
			for _, call := range calls {
				if call.IdempotencyKey != action.ID || !bytes.Equal(call.Body, action.Body) {
					t.Fatalf("maintenance recovery changed the original request: %+v", call)
				}
			}
			var transfer struct {
				Gross int64 `json:"gross_amount_pence"`
			}
			if err := json.Unmarshal(action.Body, &transfer); err != nil {
				t.Fatal(err)
			}
			_, retained := h.mission(recoveryShipBlastoise)
			if retained.Action != nil || retained.LastActionID != action.ID || retained.FeesPence != transfer.Gross*100/10_000 ||
				retained.ExpensesPence != 0 || retained.ProvisionsPence != 0 || len(h.pending()) != 0 {
				t.Fatalf("funding recovery lost actual fees or expensed principal: %+v", retained)
			}
		})
	}
}

func TestIdleMaintenanceExternallySatisfiedAfterRemitRetires(t *testing.T) {
	h, runner, proxy, _ := idleMaintenanceFixture(t, false)
	idleMaintenanceFundDonor(t, h, runner, proxy)
	mission, data := idleMaintenanceUntilRemit(t, h, runner)
	if _, err := runner.dispatch(h.ctx, mission, &data); err != nil {
		t.Fatal(err)
	}
	if data.FeesPence <= 0 || data.Action != nil {
		t.Fatalf("fixture did not complete its maintenance remittance: %+v", data)
	}
	fee := data.FeesPence
	h.api.mu.Lock()
	h.api.fleet[0]["stores"].(map[string]any)["provisions_tons"] = 2.0
	h.api.mu.Unlock()
	actionLogRefresh(t, h, runner)
	_, retained := h.mission(recoveryShipBlastoise)
	if retained.FeesPence != fee || retained.ExpensesPence != 0 || retained.ProvisionsPence != 0 {
		t.Fatalf("external stores invented procurement or erased actual transfer fees: %+v", retained)
	}
	idleMaintenanceRequireRetired(t, h, runner)
	if h.api.mutationCount("/v1/treasury/remit") != 1 || h.api.mutationCount("/v1/market/place-order") != 0 ||
		h.api.mutationCount("/v1/fleet/offshore") != 0 || len(h.pending()) != 0 {
		t.Fatal("externally satisfied maintenance spent again instead of retiring")
	}
}

func idleMaintenanceUntilProvision(t *testing.T, h *recoveryHarness, runner *Runner) (*store.Mission, missionData) {
	t.Helper()
	for range 14 {
		mission, data, err := runner.loadMission(h.ctx, recoveryShipBlastoise)
		if err != nil {
			t.Fatal(err)
		}
		if mission != nil && data.Action != nil && data.Action.Path == "/v1/market/place-order" {
			var order struct {
				IntoStores bool `json:"into_stores"`
			}
			if err := json.Unmarshal(data.Action.Body, &order); err != nil {
				t.Fatal(err)
			}
			if !order.IntoStores {
				t.Fatal("maintenance staged a cargo purchase instead of stores")
			}
			return mission, data
		}
		if _, err := h.tick(runner); err != nil {
			t.Fatal(err)
		}
	}
	t.Fatalf("maintenance never staged its fresh store order: holds %+v", runner.holding)
	return nil, missionData{}
}

func TestIdleMaintenanceDockReconciliationContinuesUnfundedIntent(t *testing.T) {
	h, runner, proxy, _ := idleMaintenanceFixture(t, true)
	proxy.loseDockReceipt.Store(true)
	var mission *store.Mission
	var data missionData
	for range 6 {
		var err error
		mission, data, err = runner.loadMission(h.ctx, recoveryShipBlastoise)
		if err != nil {
			t.Fatal(err)
		}
		if mission != nil && data.Action != nil && data.Action.Path == "/v1/fleet/dock" {
			break
		}
		if _, err := h.tick(runner); err != nil {
			t.Fatal(err)
		}
	}
	if data.Action == nil || data.Action.Path != "/v1/fleet/dock" {
		t.Fatalf("roadstead inspection did not stage one dock request: %+v", data)
	}
	action := *data.Action
	if _, err := runner.dispatch(h.ctx, mission, &data); err != nil {
		t.Fatal(err)
	}
	attempts := h.api.mutationsFor("/v1/fleet/dock")
	if len(attempts) == 0 || len(h.pending()) != 1 {
		t.Fatalf("lost docking receipt did not leave an attempted immutable journal: %+v", attempts)
	}
	proxy.loseDockReceipt.Store(false)
	h.expireCooldown(recoveryShipBlastoise)
	restarted := h.runner(primaryTestKey)
	actionLogRefresh(t, h, restarted)
	mission, data = h.mission(recoveryShipBlastoise)
	if _, err := restarted.dispatch(h.ctx, mission, &data); err != nil {
		t.Fatal(err)
	}
	if len(h.api.mutationsFor("/v1/fleet/dock")) != len(attempts) || data.Action != nil || len(h.pending()) != 0 {
		t.Fatal("observed docking reconciliation resent navigation or retained the ambiguous action")
	}
	for _, call := range attempts {
		if call.IdempotencyKey != action.ID || !bytes.Equal(call.Body, action.Body) {
			t.Fatalf("docking retries changed the original identity: %+v", call)
		}
	}
	journaled, err := h.db.BeginAction(h.ctx, &store.Action{ID: action.ID, AccountID: h.account.ID, Method: action.Method, Path: action.Path, Body: action.Body})
	if err != nil || journaled.State != store.ActionReconciled {
		t.Fatalf("authoritative berth observation did not durably reconcile docking: %+v error %v", journaled, err)
	}
	for range 3 {
		mission, data = h.mission(recoveryShipBlastoise)
		if _, err := restarted.driveMission(h.ctx, harnessShip(t, restarted, recoveryShipBlastoise), mission, &data); err != nil {
			t.Fatal(err)
		}
	}
	_, retained := h.mission(recoveryShipBlastoise)
	if retained.Plan.Kind != "maintenance" || retained.ProvisionsPence != 0 || h.api.mutationCount("/v1/fleet/offshore") != 0 {
		t.Fatalf("docking reconciliation discarded replenishment instead of holding its unfunded continuation: %+v", retained)
	}
}

func TestIdleMaintenanceUnfilledAndPartialProvisionContinue(t *testing.T) {
	for _, outcome := range []string{"zero", "partial", "rejected"} {
		t.Run(outcome, func(t *testing.T) {
			h, runner, proxy, reserves := idleMaintenanceFixture(t, false)
			h.api.mu.Lock()
			h.api.fleet[0]["stores"].(map[string]any)["provisions_tons"] = 0.0
			h.api.vaultBalance["north"] = reserves["north"] + 100_000
			limit := int64(0)
			if outcome == "partial" {
				limit = 1
			}
			if outcome == "rejected" {
				proxy.rejectProvision = true
			} else {
				proxy.fillLimit = &limit
			}
			h.api.mu.Unlock()
			actionLogRefresh(t, h, runner)
			mission, data := idleMaintenanceUntilProvision(t, h, runner)
			if _, err := runner.dispatch(h.ctx, mission, &data); err != nil {
				t.Fatal(err)
			}
			calls := h.api.mutationsFor("/v1/market/place-order")
			if len(calls) != 1 {
				t.Fatalf("first store outcome was not a real single HTTP mutation: %+v", calls)
			}
			firstCost := int64(0)
			if outcome == "partial" {
				firstCost = 12_654
			}
			if data.ProvisionsPence != firstCost || data.Action != nil || data.BoughtQuantity != 0 {
				t.Fatalf("nonterminal store outcome lost its exact observed fill: %+v", data)
			}
			h.expireCooldown(recoveryShipBlastoise)
			h.api.mu.Lock()
			proxy.rejectProvision = false
			proxy.fillLimit = nil
			h.api.mu.Unlock()
			// A fresh continuation must retain the intent rather than
			// deleting it on the old successful/rejected-stage callback.
			actionLogRefresh(t, h, runner)
			mission, data = h.mission(recoveryShipBlastoise)
			if _, err := runner.driveMission(h.ctx, harnessShip(t, runner, recoveryShipBlastoise), mission, &data); err != nil {
				t.Fatal(err)
			}
			_, continued := h.mission(recoveryShipBlastoise)
			if continued.ProvisionsPence != firstCost {
				t.Fatalf("continuing maintenance erased an earlier partial receipt: %+v", continued)
			}
			for range 6 {
				actionLogRefresh(t, h, runner)
				mission, data = h.mission(recoveryShipBlastoise)
				if _, err := runner.driveMission(h.ctx, harnessShip(t, runner, recoveryShipBlastoise), mission, &data); err != nil {
					t.Fatal(err)
				}
				if len(h.api.mutationsFor("/v1/market/place-order")) == 2 {
					break
				}
			}
			_, completed := h.mission(recoveryShipBlastoise)
			calls = h.api.mutationsFor("/v1/market/place-order")
			if len(calls) != 2 || calls[1].Status != http.StatusOK || completed.ProvisionsPence != 25_308 ||
				completed.ExpensesPence != 0 || completed.BoughtQuantity != 0 {
				t.Fatalf("maintenance did not complete only the still-missing stores: calls %+v data %+v", calls, completed)
			}
			if calls[0].IdempotencyKey == calls[1].IdempotencyKey {
				t.Fatal("a new residual store order reused the completed/rejected request identity")
			}
			actionLogRefresh(t, h, runner)
			idleMaintenanceRequireRetired(t, h, runner)
			if h.api.mutationCount("/v1/fleet/offshore") != 0 {
				t.Fatal("partial/rejected replenishment returned to an offshore loop")
			}
		})
	}
}

func TestIdleMaintenanceUnsentInvalidationPreservesActualFees(t *testing.T) {
	h, runner, proxy, _ := idleMaintenanceFixture(t, false)
	idleMaintenanceFundDonor(t, h, runner, proxy)
	mission, data := idleMaintenanceUntilRemit(t, h, runner)
	if _, err := runner.dispatch(h.ctx, mission, &data); err != nil {
		t.Fatal(err)
	}
	fee, fundedAction := data.FeesPence, data.LastActionID
	if fee <= 0 || fundedAction == "" {
		t.Fatalf("fixture did not persist actual transfer accounting: %+v", data)
	}
	mission, data = idleMaintenanceUntilProvision(t, h, runner)
	unsentID := data.Action.ID
	h.api.mu.Lock()
	proxy.price = 20_000
	h.api.mu.Unlock()
	runner.publishedFinancial.Add(1)
	runner.capturedFinancial = runner.publishedFinancial.Load()
	actionLogRefresh(t, h, runner)
	if _, err := runner.dispatch(h.ctx, mission, &data); err != nil {
		t.Fatal(err)
	}
	_, retained := h.mission(recoveryShipBlastoise)
	if retained.Action != nil || retained.FeesPence != fee || retained.LastActionID != fundedAction ||
		retained.ProvisionsPence != 0 || retained.ExpensesPence != 0 || h.api.mutationCount("/v1/market/place-order") != 0 || len(h.pending()) != 0 {
		t.Fatalf("proven-unsent invalidation discarded maintenance or actual transfer provenance: %+v", retained)
	}
	if retained.LastActionID == unsentID {
		t.Fatal("unsent invalidation claimed a store-purchase outcome")
	}
	idleMaintenanceFundDonor(t, h, runner, proxy)
	mission, data = idleMaintenanceUntilProvision(t, h, runner)
	if _, err := runner.dispatch(h.ctx, mission, &data); err != nil {
		t.Fatal(err)
	}
	_, completed := h.mission(recoveryShipBlastoise)
	var actualFees int64
	for _, call := range h.api.mutationsFor("/v1/treasury/remit") {
		if call.Status != http.StatusOK {
			t.Fatalf("fresh funding did not complete: %+v", call)
		}
		var transfer struct {
			Gross int64 `json:"gross_amount_pence"`
		}
		if err := json.Unmarshal(call.Body, &transfer); err != nil {
			t.Fatal(err)
		}
		actualFees += transfer.Gross * 100 / 10_000
	}
	if completed.FeesPence != actualFees || completed.ProvisionsPence != 20_500 ||
		completed.ExpensesPence != 0 || h.api.mutationCount("/v1/market/place-order") != 1 {
		t.Fatalf("resumed maintenance lost earlier fees or duplicated procurement: %+v", completed)
	}
}

func TestIdleMaintenanceFundingRechecksNeedQuoteAndCapacityBeforeFirstSend(t *testing.T) {
	for _, changed := range []string{"already satisfied", "quote unavailable", "rates unavailable", "hold full"} {
		t.Run(changed, func(t *testing.T) {
			h, runner, proxy, _ := idleMaintenanceFixture(t, false)
			idleMaintenanceFundDonor(t, h, runner, proxy)
			mission, data := idleMaintenanceUntilRemit(t, h, runner)
			h.api.mu.Lock()
			switch changed {
			case "already satisfied":
				h.api.fleet[0]["stores"].(map[string]any)["provisions_tons"] = 2.0
			case "quote unavailable":
				proxy.quoteMode = "failed"
			case "rates unavailable":
				proxy.rateMode = "failed"
			case "hold full":
				h.api.fleet[0]["cargo_manifest"] = map[string]any{recoveryGrain: int64(56)}
				h.api.fleet[0]["cargo_used_tons"] = int64(56)
				h.api.fleet[0]["effective_hold_capacity_tons"] = 56
			}
			h.api.mu.Unlock()
			actionLogRefresh(t, h, runner)
			if _, err := runner.dispatch(h.ctx, mission, &data); err != nil {
				t.Fatal(err)
			}
			if h.api.mutationCount("/v1/treasury/remit") != 0 || h.api.mutationCount("/v1/market/place-order") != 0 || len(h.pending()) != 0 {
				t.Fatalf("new maintenance funding crossed transport despite changed %s", changed)
			}
			retainedMission, err := h.db.GetMission(h.ctx, h.account.ID, recoveryShipBlastoise)
			if err != nil {
				t.Fatal(err)
			}
			if retainedMission == nil {
				if changed != "already satisfied" {
					t.Fatalf("invalidated funding discarded a still-needed replenishment intent after %s", changed)
				}
				return
			}
			var retained missionData
			if err := json.Unmarshal(retainedMission.Data, &retained); err != nil {
				t.Fatal(err)
			}
			if retained.Action != nil || retained.FeesPence != 0 || retained.ProvisionsPence != 0 {
				t.Fatalf("changed %s retained a sendable transfer or invented expense: %+v", changed, retained)
			}
		})
	}
}

func TestIdleMaintenanceFundedStoresRemainReservedAcrossOtherConsumers(t *testing.T) {
	h, runner, proxy, _ := idleMaintenanceFixture(t, false)
	idleMaintenanceFundDonor(t, h, runner, proxy)
	mission, data := idleMaintenanceUntilRemit(t, h, runner)
	if _, err := runner.dispatch(h.ctx, mission, &data); err != nil {
		t.Fatal(err)
	}
	actionLogRefresh(t, h, runner)
	owner, err := runner.committedPlanningInput(h.ctx, harnessShip(t, runner, recoveryShipBlastoise))
	if err != nil {
		t.Fatal(err)
	}
	other, err := runner.committedPlanningInput(h.ctx, harnessShip(t, runner, recoveryShipCharizard))
	if err != nil {
		t.Fatal(err)
	}
	company, err := runner.committedPlanningInput(h.ctx, ShipState{})
	if err != nil {
		t.Fatal(err)
	}
	// Both a different ship and the company controller must see the real
	// pending store cost protected; its owner must still be able to spend it.
	cost := int64(12_654)
	if other.ReserveByRegion["north"] < owner.ReserveByRegion["north"]+cost ||
		company.ReserveByRegion["north"] < owner.ReserveByRegion["north"]+cost ||
		owner.Balances["north"] < owner.ReserveByRegion["north"]+cost {
		t.Fatalf("funded maintenance cash leaked into another consumer's free balance: owner %v other %v company %v balances %v",
			owner.ReserveByRegion, other.ReserveByRegion, company.ReserveByRegion, owner.Balances)
	}
	// A fresh real shortfall elsewhere must not pull the store money back
	// out through the account controller before provisioning can execute.
	h.api.setVaultBalance("south", company.ReserveByRegion["south"]-cost/2)
	actionLogRefresh(t, h, runner)
	before := h.api.mutationCount("/v1/treasury/remit")
	if _, err := runner.manageTreasury(h.ctx); err != nil {
		t.Fatal(err)
	}
	treasury, treasuryData, err := runner.loadMission(h.ctx, "__treasury")
	if err != nil {
		t.Fatal(err)
	}
	if treasury != nil && treasuryData.Action != nil {
		var transfer struct {
			From  string `json:"from_region"`
			Gross int64  `json:"gross_amount_pence"`
		}
		if err := json.Unmarshal(treasuryData.Action.Body, &transfer); err != nil {
			t.Fatal(err)
		}
		free := owner.Balances["north"] - company.ReserveByRegion["north"]
		if transfer.From == "north" && transfer.Gross > max(int64(0), free) {
			t.Fatalf("treasury staged re-export of maintenance's funded stores: %+v free %dp", transfer, free)
		}
	}
	if h.api.mutationCount("/v1/treasury/remit") != before {
		t.Fatal("treasury mutated before an admitted durable transfer")
	}
}

func TestIdleMaintenanceHeldHullDoesNotJustifyReplacementGrowth(t *testing.T) {
	w, runner := newGrowthExecutionWorld(t)
	w.until(runner, func(_ *store.Mission, _ missionData) bool {
		control, intent, err := runner.loadMission(w.h.ctx, "__growth")
		if err != nil {
			t.Fatal(err)
		}
		return control != nil && intent.Action != nil && intent.Action.Stage == "build"
	})
	control, intent := w.h.mission("__growth")
	if journaled, err := w.h.db.HasAction(w.h.ctx, w.h.account.ID, intent.Action.ID); err != nil || journaled {
		t.Fatalf("positive growth control already crossed its immutable send boundary: journaled %t error %v", journaled, err)
	}
	if err := w.h.db.DeleteMission(w.h.ctx, w.h.account.ID, control.ShipID); err != nil {
		t.Fatal(err)
	}
	w.h.api.mu.Lock()
	row := stationaryShipRow(recoveryShipBlastoise, "Blastoise", recoveryHomePort, "berthed", 0, 4)
	row["stores"] = map[string]any{"provisions_tons": 1.05, "sea_days_remaining": 10.5}
	w.h.api.fleet[0] = row
	w.h.api.mu.Unlock()
	w.h.putMission(recoveryShipBlastoise, "growth-reusable-maintenance", "maintenance", missionData{
		Plan: Plan{Kind: "maintenance", ShipID: recoveryShipBlastoise, Origin: recoveryHomePort,
			TargetStoresTons: 1.1, StoresToBuy: 1, StoresLimitPence: 50, Economics: Economics{StoreCashPence: 52}},
		CostsKnown: true,
	})
	actionLogRefresh(t, w.h, runner)
	for range 2 {
		if _, err := runner.considerGrowth(w.h.ctx); err != nil {
			t.Fatal(err)
		}
		w.h.missingMission("__growth")
	}
	_, retained := w.h.mission(recoveryShipBlastoise)
	if retained.Plan.Kind != "maintenance" || w.h.api.mutationCount("/v1/shipyard/build-ship") != 0 {
		t.Fatalf("reusable hull awaiting maintenance was replaced by new capital: %+v", retained)
	}
}

func TestIdleMaintenanceEmbeddedProvisionKeepsEconomicMissionAndSeals(t *testing.T) {
	h, runner, _, reserves := idleMaintenanceFixture(t, false)
	proxy := installUWExecProxy(t, h)
	proxy.contractRows = []map[string]any{freightContractRow("economic-mission-seal", "active", false)}
	h.api.mu.Lock()
	h.api.fleet[0]["cargo_manifest"] = map[string]any{recoveryGrain: int64(44)}
	h.api.fleet[0]["cargo_used_tons"] = int64(44)
	h.api.vaultBalance["north"] = reserves["north"] + 100_000
	h.api.mu.Unlock()
	actionLogRefresh(t, h, runner)
	contract, exists := runner.contract("economic-mission-seal")
	if !exists {
		t.Fatal("economic provisioning fixture did not capture its actual accepted seal")
	}
	h.putMission(recoveryShipBlastoise, "economic-provision-continuation", "provision", missionData{
		Plan: Plan{Kind: "spot", ShipID: recoveryShipBlastoise, Origin: recoveryHomePort,
			Destination: recoveryFarPort, Commodity: recoveryGrain, Quantity: 20,
			BuyLimitPence: 100, TargetStoresTons: 2},
		Freight:        []freightObligation{obligationFromContract(contract)},
		BoughtQuantity: 20, BuyDone: true, AcquisitionPence: 2_000,
		InitialLedgerIDs: map[string]bool{}, Eligible: map[string]int64{},
	})
	mission, data := h.mission(recoveryShipBlastoise)
	if _, err := runner.driveMission(h.ctx, harnessShip(t, runner, recoveryShipBlastoise), mission, &data); err != nil {
		t.Fatal(err)
	}
	if data.Action == nil || data.Action.Path != "/v1/market/place-order" {
		t.Fatalf("embedded stores did not stage a legitimate order: %+v", data)
	}
	if _, err := runner.dispatch(h.ctx, mission, &data); err != nil {
		t.Fatal(err)
	}
	actionLogRefresh(t, h, runner)
	mission, data = h.mission(recoveryShipBlastoise)
	if _, err := runner.driveMission(h.ctx, harnessShip(t, runner, recoveryShipBlastoise), mission, &data); err != nil {
		t.Fatal(err)
	}
	mission, retained := h.mission(recoveryShipBlastoise)
	if mission.Quantity != 20 || retained.Plan.Kind != "spot" || retained.Plan.Destination != recoveryFarPort ||
		!retained.BuyDone || retained.BoughtQuantity != 20 || retained.AcquisitionPence != 2_000 ||
		len(retained.Freight) != 1 || retained.Freight[0].Settled || retained.ProvisionsPence != 12_654 {
		t.Fatalf("standalone maintenance handling retired or rewrote the enclosing economic mission: %+v", retained)
	}
	if calls := h.api.mutationsFor("/v1/market/place-order"); len(calls) != 1 || calls[0].Status != http.StatusOK {
		t.Fatalf("embedded replenishment did not execute once: %+v", calls)
	}
}

func TestIdleMaintenanceQuoteVetoKeepsPreviouslyFundedCashProtected(t *testing.T) {
	for _, satisfied := range []bool{false, true} {
		name := "stores still needed"
		if satisfied {
			name = "external stores satisfied"
		}
		t.Run(name, func(t *testing.T) {
			h, runner, proxy, _ := idleMaintenanceFixture(t, false)
			idleMaintenanceFundDonor(t, h, runner, proxy)
			mission, data := idleMaintenanceUntilRemit(t, h, runner)
			if _, err := runner.dispatch(h.ctx, mission, &data); err != nil {
				t.Fatal(err)
			}
			fundedID, actualFee := data.LastActionID, data.FeesPence
			fundingCalls := h.api.mutationsFor("/v1/treasury/remit")
			if len(fundingCalls) != 1 || fundingCalls[0].Status != http.StatusOK || fundedID != fundingCalls[0].IdempotencyKey || actualFee <= 0 {
				t.Fatalf("fixture has no actual completed maintenance funding: calls %+v data %+v", fundingCalls, data)
			}
			funding := fundingCalls[0]
			fundingJournal, err := h.db.BeginAction(h.ctx, &store.Action{
				ID: fundedID, AccountID: h.account.ID, Method: funding.Method,
				Path: funding.Path, Body: funding.Body,
			})
			if err != nil {
				t.Fatal(err)
			}
			if fundingJournal.State != store.ActionComplete {
				t.Fatalf("observed funding has no immutable completed receipt: %+v", fundingJournal)
			}
			fundingResponse := append(json.RawMessage(nil), fundingJournal.Response...)
			mission, data = idleMaintenanceUntilProvision(t, h, runner)
			unsentID := data.Action.ID
			if exists, err := h.db.HasAction(h.ctx, h.account.ID, unsentID); err != nil || exists {
				t.Fatalf("store order is not a genuinely never-attempted intent: exists %t error %v", exists, err)
			}
			ownerBefore, err := runner.committedPlanningInput(h.ctx, harnessShip(t, runner, recoveryShipBlastoise))
			if err != nil {
				t.Fatal(err)
			}
			companyBefore, err := runner.committedPlanningInput(h.ctx, ShipState{})
			if err != nil {
				t.Fatal(err)
			}
			fundedCommitment := companyBefore.ReserveByRegion["north"] - ownerBefore.ReserveByRegion["north"]
			if fundedCommitment < 12_654 || ownerBefore.Balances["north"]-ownerBefore.ReserveByRegion["north"] < fundedCommitment {
				t.Fatalf("fixture did not establish already funded store cash before cancellation: commitment %d owner %+v company %v", fundedCommitment, ownerBefore, companyBefore.ReserveByRegion)
			}
			// Publish a genuinely higher fresh ask without changing financial
			// epochs: the immutable store IOC must be vetoed by its actual
			// price/depth preflight, not by an unrelated restart/event guard.
			h.api.mu.Lock()
			proxy.price = 20_000
			h.api.mu.Unlock()
			actionLogRefresh(t, h, runner)
			if _, err := runner.dispatch(h.ctx, mission, &data); err != nil {
				t.Fatal(err)
			}
			_, retained := h.mission(recoveryShipBlastoise)
			if retained.Action != nil || retained.LastActionID != fundedID || retained.FeesPence != actualFee ||
				retained.ProvisionsPence != 0 || retained.ExpensesPence != 0 || h.api.mutationCount("/v1/market/place-order") != 0 {
				t.Fatalf("price veto lost actual funding provenance or sent the obsolete store order: %+v", retained)
			}
			if satisfied {
				h.api.mu.Lock()
				h.api.fleet[0]["stores"].(map[string]any)["provisions_tons"] = 2.0
				h.api.mu.Unlock()
			}
			// No maintenance drive/requote occurs after cancellation. The
			// company and another ship consume the same fresh REST evidence
			// before the maintenance controller gets another scheduling turn.
			actionLogRefresh(t, h, runner)
			owner, err := runner.committedPlanningInput(h.ctx, harnessShip(t, runner, recoveryShipBlastoise))
			if err != nil {
				t.Fatal(err)
			}
			other, err := runner.committedPlanningInput(h.ctx, harnessShip(t, runner, recoveryShipCharizard))
			if err != nil {
				t.Fatal(err)
			}
			company, err := runner.committedPlanningInput(h.ctx, ShipState{})
			if err != nil {
				t.Fatal(err)
			}
			if satisfied {
				if company.ReserveByRegion["north"] != owner.ReserveByRegion["north"] || other.ReserveByRegion["north"] != owner.ReserveByRegion["north"] {
					t.Errorf("fresh externally satisfied stores left a phantom cash commitment: owner %v other %v company %v", owner.ReserveByRegion, other.ReserveByRegion, company.ReserveByRegion)
				}
			} else if company.ReserveByRegion["north"] < owner.ReserveByRegion["north"]+fundedCommitment ||
				other.ReserveByRegion["north"] < owner.ReserveByRegion["north"]+fundedCommitment {
				t.Errorf("quote veto released still-needed funded cash before maintenance could requote: funded %dp owner %v other %v company %v", fundedCommitment, owner.ReserveByRegion, other.ReserveByRegion, company.ReserveByRegion)
			}
			// An independent observed arrival reserve is now short. Actually
			// drive any newly staged treasury action, so a lost reservation
			// produces observable outbound cash and fee effects, not merely
			// a suspicious internal quote.
			h.api.setVaultBalance("south", company.ReserveByRegion["south"]-6_000)
			actionLogRefresh(t, h, runner)
			h.api.mu.Lock()
			localBefore, remoteBefore := h.api.vaultBalance["north"], h.api.vaultBalance["south"]
			h.api.mu.Unlock()
			beforeCount := h.api.mutationCount("/v1/treasury/remit")
			if _, err := runner.manageTreasury(h.ctx); err != nil {
				t.Fatal(err)
			}
			treasury, treasuryData, err := runner.loadMission(h.ctx, "__treasury")
			if err != nil {
				t.Fatal(err)
			}
			if treasury != nil && treasuryData.Action != nil {
				if _, err := runner.dispatch(h.ctx, treasury, &treasuryData); err != nil {
					t.Fatal(err)
				}
			}
			calls := h.api.mutationsFor("/v1/treasury/remit")
			var outbound, outboundFees int64
			for _, call := range calls[beforeCount:] {
				var transfer struct {
					From  string `json:"from_region"`
					To    string `json:"to_region"`
					Gross int64  `json:"gross_amount_pence"`
				}
				if err := json.Unmarshal(call.Body, &transfer); err != nil {
					t.Fatal(err)
				}
				if call.Status != http.StatusOK || transfer.From != "north" || transfer.To != "south" {
					t.Fatalf("fixture's treasury mutation did not execute its observed regional transfer: %+v", call)
				}
				outbound += transfer.Gross
				outboundFees += transfer.Gross * 100 / 10_000
			}
			h.api.mu.Lock()
			localAfter, remoteAfter := h.api.vaultBalance["north"], h.api.vaultBalance["south"]
			h.api.mu.Unlock()
			if localAfter != localBefore-outbound || remoteAfter != remoteBefore+outbound-outboundFees {
				t.Fatalf("treasury debit/credit did not match actual wire amounts and fees: before %d/%d after %d/%d gross %d fees %d", localBefore, remoteBefore, localAfter, remoteAfter, outbound, outboundFees)
			}
			if satisfied {
				if outbound <= 0 || outboundFees <= 0 || localAfter < owner.ReserveByRegion["north"] {
					t.Fatalf("actually satisfied stores did not release surplus for legitimate treasury funding: gross %d fees %d local %d reserve %d", outbound, outboundFees, localAfter, owner.ReserveByRegion["north"])
				}
			} else if localAfter < owner.ReserveByRegion["north"]+fundedCommitment || outboundFees != 0 {
				t.Fatalf("quote cancellation allowed a fee-draining re-export of already funded needed stores: gross %d fees %d local %d protected %d", outbound, outboundFees, localAfter, owner.ReserveByRegion["north"]+fundedCommitment)
			}
			_, retained = h.mission(recoveryShipBlastoise)
			if retained.Action != nil || retained.LastActionID != fundedID || retained.FeesPence != actualFee ||
				retained.ProvisionsPence != 0 || retained.ExpensesPence != 0 || len(h.pending()) != 0 {
				t.Fatalf("other consumers changed the maintenance controller's actual accounting or pending identity: %+v", retained)
			}
			fundingAfter, err := h.db.BeginAction(h.ctx, &store.Action{
				ID: fundedID, AccountID: h.account.ID, Method: funding.Method,
				Path: funding.Path, Body: funding.Body,
			})
			if err != nil {
				t.Fatal(err)
			}
			if fundingAfter.State != store.ActionComplete || !bytes.Equal(fundingAfter.Body, funding.Body) || !bytes.Equal(fundingAfter.Response, fundingResponse) {
				t.Fatalf("unsent store cancellation rewrote the already attempted funding journal: %+v", fundingAfter)
			}
		})
	}
}

func TestIdleMaintenanceSatisfiedStoresReleaseUnaffordableOptionalRefit(t *testing.T) {
	h, runner, proxy, _ := idleMaintenanceFixture(t, false)
	h.api.mu.Lock()
	h.api.fleet[0]["ship"].(map[string]any)["cannons_count"] = 2
	h.api.fleet[0]["effective_hold_capacity_tons"] = 56
	h.api.mu.Unlock()
	h.api.setFactorPorts(recoveryFarPort)
	actionLogRefresh(t, h, runner)
	if observed, err := runner.committedPlanningInput(h.ctx, harnessShip(t, runner, recoveryShipBlastoise)); err != nil {
		t.Fatal(err)
	} else {
		// Preserve the fixture's reserve-only local liquidity after the
		// newly observed factor changes its actual upkeep obligation.
		h.api.setVaultBalance("north", observed.ReserveByRegion["north"])
	}
	idleMaintenanceFundDonor(t, h, runner, proxy)
	mission, data := idleMaintenanceUntilPurchase(t, h, runner)
	if data.ProvisionsPence != 12_654 || data.Action != nil {
		t.Fatalf("fixture did not finish a real standalone replenishment: %+v", data)
	}
	var incomeOpen atomic.Bool
	downstream := h.api.server.Config.Handler
	h.api.server.Config.Handler = http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		if incomeOpen.Load() && request.Method == http.MethodGet && request.URL.Path == "/v1/market/book" && request.URL.Query().Get("port_id") == recoveryHomePort {
			h.api.writeJSON(writer, http.StatusOK, h.api.bookDocument(recoveryHomePort))
			return
		}
		if incomeOpen.Load() && request.Method == http.MethodPost && request.URL.Path == "/v1/market/place-order" {
			body, err := io.ReadAll(request.Body)
			if err != nil {
				t.Errorf("read income order: %v", err)
				writer.WriteHeader(http.StatusBadRequest)
				return
			}
			request.Body = io.NopCloser(bytes.NewReader(body))
			var order struct {
				Commodity  string `json:"commodity_id"`
				IntoStores bool   `json:"into_stores"`
			}
			if err := json.Unmarshal(body, &order); err != nil {
				t.Errorf("decode income order: %v", err)
				writer.WriteHeader(http.StatusBadRequest)
				return
			}
			if order.Commodity == recoveryGrain && !order.IntoStores {
				proxy.downstream.ServeHTTP(writer, request)
				return
			}
		}
		downstream.ServeHTTP(writer, request)
	})
	incomeOpen.Store(true)
	actionLogRefresh(t, h, runner)
	input, err := runner.committedPlanningInput(h.ctx, harnessShip(t, runner, recoveryShipBlastoise))
	if err != nil {
		t.Fatal(err)
	}
	h.api.setVaultBalance("north", input.ReserveByRegion["north"]+12_000)
	actionLogRefresh(t, h, runner)
	ship := harnessShip(t, runner, recoveryShipBlastoise)
	input, err = runner.committedPlanningInput(h.ctx, ship)
	if err != nil {
		t.Fatal(err)
	}
	hull, exists := findHull(input.Catalog, ship.Ship.ShipClass)
	if !exists {
		t.Fatal("fixture lacks the ship's actual hull")
	}
	refitCost := (min(int64(4), hull.MaxCannons) - ship.Ship.CannonsCount) * 15_000
	plans := PlanVoyages(input)
	if refitCost <= 0 || input.Balances["north"]-refitCost >= input.ReserveByRegion["north"] ||
		len(plans) == 0 || !plans[0].Accepted || plans[0].Quantity <= 0 {
		t.Fatalf("fixture must offer real income while optional refit remains unaffordable: refit %d balances %v reserves %v plans %+v", refitCost, input.Balances, input.ReserveByRegion, plans)
	}
	idleMaintenanceRequireRetired(t, h, runner)
	foundIncome := false
	for range 8 {
		if _, err := h.tick(runner); err != nil {
			t.Fatal(err)
		}
		for _, call := range h.api.mutationsFor("/v1/market/place-order") {
			var order struct {
				Commodity  string `json:"commodity_id"`
				IntoStores bool   `json:"into_stores"`
			}
			if err := json.Unmarshal(call.Body, &order); err != nil {
				t.Fatal(err)
			}
			foundIncome = foundIncome || call.Status == http.StatusOK && order.Commodity == recoveryGrain && !order.IntoStores
		}
		if foundIncome {
			break
		}
	}
	if !foundIncome || h.api.mutationCount("/v1/shipyard/modify-ship") != 0 || h.api.mutationCount("/v1/shipyard/build-ship") != 0 {
		t.Fatalf("completed replenishment trapped the income-capable hull behind unaffordable optional guns: income %t refits %+v holds %+v", foundIncome, h.api.mutationsFor("/v1/shipyard/modify-ship"), runner.holding)
	}
	mission, data = h.mission(recoveryShipBlastoise)
	if mission.Quantity <= 0 || data.Plan.Kind == "maintenance" || !data.BuyDone || data.BoughtQuantity <= 0 {
		t.Fatalf("income purchase did not advance a real economic mission: %+v", data)
	}
}

func TestIdleMaintenanceSatisfiedStoresDoNotSkipCommissioningArmament(t *testing.T) {
	w, runner := newGrowthExecutionWorld(t)
	w.until(runner, func(_ *store.Mission, _ missionData) bool {
		control, data, err := runner.loadMission(w.h.ctx, "__growth")
		if err != nil {
			t.Fatal(err)
		}
		return control != nil && control.Phase == "growth_refit" && data.BuiltShipID != "" && data.Action == nil
	})
	control, data := w.h.mission("__growth")
	w.h.api.mu.Lock()
	row := w.builtRow()
	if row == nil {
		w.h.api.mu.Unlock()
		t.Fatal("commissioning control has no actually built hull")
	}
	row["stores"] = map[string]any{"provisions_tons": max(2.0, data.Plan.TargetStoresTons+1)}
	w.h.api.mu.Unlock()
	actionLogRefresh(t, w.h, runner)
	ship := harnessShip(t, runner, recoveryBuiltShipID)
	if ship.Ship.CannonsCount >= data.TargetCannons || data.TargetCannons <= 0 {
		t.Fatalf("commissioning fixture does not still require its admitted armament: ship %+v target %d", ship.Ship, data.TargetCannons)
	}
	if _, err := runner.driveGrowth(w.h.ctx, control, &data); err != nil {
		t.Fatal(err)
	}
	if data.Action == nil || data.Action.Stage != "refit" {
		t.Fatalf("already sufficient stores bypassed mandatory commissioning armament: %+v", data)
	}
	if _, err := runner.dispatch(w.h.ctx, control, &data); err != nil {
		t.Fatal(err)
	}
	calls := w.h.api.mutationsFor("/v1/shipyard/modify-ship")
	if len(calls) != 1 || calls[0].Status != http.StatusOK {
		t.Fatalf("commissioning did not actually install its admitted cannons: %+v", calls)
	}
	for _, call := range w.h.api.mutationsFor("/v1/market/place-order") {
		var order growthExecutionRequest
		if err := json.Unmarshal(call.Body, &order); err != nil {
			t.Fatal(err)
		}
		if order.ShipID == recoveryBuiltShipID && order.Side == "bid" && !order.IntoStores {
			t.Fatal("new hull bought cargo before mandatory commissioning armament")
		}
	}
}

type maintenanceReservationDepthAPI struct {
	h             *recoveryHarness
	downstream    http.Handler
	remaining     atomic.Int64
	loseFirstShip atomic.Bool
}

func (p *maintenanceReservationDepthAPI) ServeHTTP(writer http.ResponseWriter, request *http.Request) {
	api := p.h.api
	api.mu.Lock()
	authorized := api.authorized(request)
	api.mu.Unlock()
	if !authorized {
		p.downstream.ServeHTTP(writer, request)
		return
	}
	if request.Method == http.MethodGet && request.URL.Path == "/v1/market/book" && request.URL.Query().Get("port_id") == recoveryHomePort {
		book := api.bookDocument(recoveryHomePort)
		book["customs_bps"] = 250
		asks := []any{}
		if quantity := p.remaining.Load(); quantity > 0 {
			asks = append(asks, map[string]any{"order_id": "shared-idle-provisions", "price_pence": 12_345, "remaining_quantity": quantity})
		}
		book["books"] = []any{map[string]any{"commodity_id": "provisions", "depth": map[string]any{"bids": []any{}, "asks": asks}}}
		api.writeJSON(writer, http.StatusOK, book)
		return
	}
	if request.Method != http.MethodPost || request.URL.Path != "/v1/market/place-order" {
		p.downstream.ServeHTTP(writer, request)
		return
	}
	body, err := io.ReadAll(request.Body)
	if err != nil {
		p.h.t.Errorf("read shared store order: %v", err)
		writer.WriteHeader(http.StatusBadRequest)
		return
	}
	var order struct {
		ShipID     string `json:"ship_id"`
		PortID     string `json:"port_id"`
		Commodity  string `json:"commodity_id"`
		Side       string `json:"side"`
		Quantity   int64  `json:"quantity"`
		Price      int64  `json:"price_pence"`
		IntoStores bool   `json:"into_stores"`
	}
	if err := json.Unmarshal(body, &order); err != nil {
		p.h.t.Errorf("decode shared store order: %v", err)
		writer.WriteHeader(http.StatusBadRequest)
		return
	}
	if p.loseFirstShip.Load() && order.ShipID == recoveryShipBlastoise {
		uwExecServeLost(func(request *http.Request, body []byte, status int) {
			api.mu.Lock()
			api.record(request, body, status)
			api.mu.Unlock()
		})(writer, request, body)
		return
	}
	api.mu.Lock()
	defer api.mu.Unlock()
	for _, row := range api.fleet {
		ship := row["ship"].(map[string]any)
		if ship["id"] != order.ShipID {
			continue
		}
		gross := order.Quantity * 12_345
		customs := ceilBPS(gross, 250)
		if ship["status"] != "berthed" || order.PortID != recoveryHomePort || order.Commodity != "provisions" ||
			order.Side != "bid" || !order.IntoStores || order.Price < 12_345 || order.Quantity <= 0 ||
			order.Quantity > p.remaining.Load() || api.vaultBalance["north"] < gross+customs {
			api.record(request, body, http.StatusConflict)
			api.writeProblem(writer, http.StatusConflict, "unavailable_shared_stores", "the actual remaining store stock cannot fill this request")
			return
		}
		row["stores"].(map[string]any)["provisions_tons"] = row["stores"].(map[string]any)["provisions_tons"].(float64) + float64(order.Quantity)
		p.remaining.Add(-order.Quantity)
		api.vaultBalance["north"] -= gross + customs
		api.record(request, body, http.StatusOK)
		api.writeJSON(writer, http.StatusOK, map[string]any{
			"commodity_id": "provisions", "filled_quantity": order.Quantity,
			"total_gross_pence": gross, "customs_duty_pence": customs,
		})
		return
	}
	api.record(request, body, http.StatusNotFound)
	api.writeProblem(writer, http.StatusNotFound, "missing_store_ship", "no observed ship matches the request")
}

func TestIdleMaintenanceShrinkingDepthReallocatesOnlyUnattemptedQuotes(t *testing.T) {
	for _, scenario := range []struct {
		name      string
		firstNeed int64
		attempted bool
	}{
		{name: "both quotes unattempted", firstNeed: 1},
		{name: "first order already attempted", firstNeed: 1, attempted: true},
		{name: "lower ID needs two tons higher ID needs one", firstNeed: 2},
	} {
		t.Run(scenario.name, func(t *testing.T) {
			attempted := scenario.attempted
			h := newRecoveryHarness(t)
			rows := []map[string]any{}
			for _, id := range []string{recoveryShipBlastoise, recoveryShipCharizard} {
				row := stationaryShipRow(id, id, recoveryHomePort, "berthed", 0, 4)
				row["stores"] = map[string]any{"provisions_tons": 1.05, "sea_days_remaining": 10.5}
				if id == recoveryShipBlastoise && scenario.firstNeed == 2 {
					row["stores"] = map[string]any{"provisions_tons": 0.0, "sea_days_remaining": 0}
				}
				rows = append(rows, row)
			}
			h.api.setFleet(rows)
			h.api.setAutoRemit(true, 10_000_000)
			proxy := &maintenanceReservationDepthAPI{h: h, downstream: h.api.server.Config.Handler}
			proxy.remaining.Store(scenario.firstNeed + 1)
			h.api.server.Config.Handler = proxy
			runner := refreshedRunner(t, h)
			base, err := runner.committedPlanningInput(h.ctx, harnessShip(t, runner, recoveryShipBlastoise))
			if err != nil {
				t.Fatal(err)
			}
			protected := base.ReserveByRegion["north"]
			h.api.setVaultBalance("north", protected)
			h.api.setVaultBalance("south", 0)
			actionLogRefresh(t, h, runner)
			for _, id := range []string{recoveryShipBlastoise, recoveryShipCharizard} {
				if _, err := runner.maintain(h.ctx, harnessShip(t, runner, id), nil, nil); err != nil {
					t.Fatal(err)
				}
				mission, data := h.mission(id)
				if _, err := runner.driveMission(h.ctx, harnessShip(t, runner, id), mission, &data); err != nil {
					t.Fatal(err)
				}
				_, quote := h.mission(id)
				needed := int64(1)
				if id == recoveryShipBlastoise {
					needed = scenario.firstNeed
				}
				quotedCost := needed*12_345 + ceilBPS(needed*12_345, 250)
				if quote.Action != nil || quote.Plan.StoresToBuy != needed || quote.Plan.StoresLimitPence != 12_345 || quote.Plan.Economics.StoreCashPence < quotedCost {
					t.Fatalf("fixture did not persist each unattempted executable store deficit: needed %dt data %+v", needed, quote)
				}
			}
			var original *durableAction
			var oldAttempts int
			h.api.setVaultBalance("north", protected+(scenario.firstNeed+1)*12_655+1_000)
			actionLogRefresh(t, h, runner)
			if attempted {
				mission, data := h.mission(recoveryShipBlastoise)
				if _, err := runner.driveMission(h.ctx, harnessShip(t, runner, recoveryShipBlastoise), mission, &data); err != nil {
					t.Fatal(err)
				}
				if data.Action == nil {
					t.Fatalf("two-ton control could not stage the first valid store order: %+v", data)
				}
				copy := *data.Action
				original = &copy
				proxy.loseFirstShip.Store(true)
				if _, err := runner.dispatch(h.ctx, mission, &data); err != nil {
					t.Fatal(err)
				}
				oldAttempts = h.api.mutationCount("/v1/market/place-order")
				if oldAttempts == 0 || len(h.pending()) != 1 {
					t.Fatalf("fixture never attempted the immutable first store order: pending %+v", h.pending())
				}
				proxy.loseFirstShip.Store(false)
				h.expireCooldown(recoveryShipBlastoise)
			}
			proxy.remaining.Store(1)
			actionLogRefresh(t, h, runner)
			cashBefore := runner.planningInput(harnessShip(t, runner, recoveryShipBlastoise)).Balances["north"]
			if attempted {
				other, err := runner.committedPlanningInput(h.ctx, harnessShip(t, runner, recoveryShipCharizard))
				if err != nil {
					t.Fatal(err)
				}
				entry := bookCommodity(other.Markets[recoveryHomePort], "provisions")
				if entry == nil || entry.Depth == nil {
					t.Fatal("attempted-order consumer lost the observable store book")
				}
				if _, err := WalkDepth(entry.Depth.Asks, 1, true); err == nil || other.ReserveByRegion["north"] < protected+12_654 {
					t.Fatalf("depth reallocation stole an already attempted order's stock or funds: reserves %v asks %+v", other.ReserveByRegion, entry.Depth.Asks)
				}
				restarted := h.runner(primaryTestKey)
				actionLogRefresh(t, h, restarted)
				mission, data := h.mission(recoveryShipBlastoise)
				if data.Action == nil || data.Action.ID != original.ID || !bytes.Equal(data.Action.Body, original.Body) {
					t.Fatalf("shrinking stock replaced the attempted request: %+v", data)
				}
				if _, err := restarted.dispatch(h.ctx, mission, &data); err != nil {
					t.Fatal(err)
				}
				runner = restarted
			} else {
				for range 10 {
					if _, err := h.tick(runner); err != nil {
						t.Fatal(err)
					}
					if h.api.mutationCount("/v1/market/place-order") > 0 {
						break
					}
				}
			}
			calls := h.api.mutationsFor("/v1/market/place-order")
			if len(calls) != oldAttempts+1 || calls[len(calls)-1].Status != http.StatusOK {
				t.Fatalf("shrinking two quotes to one real ton caused mutual blocking or duplicate attempts: calls %+v holds %+v", calls, runner.holding)
			}
			var winner struct {
				ShipID string `json:"ship_id"`
			}
			if err := json.Unmarshal(calls[len(calls)-1].Body, &winner); err != nil {
				t.Fatal(err)
			}
			if scenario.firstNeed == 2 && winner.ShipID != recoveryShipCharizard {
				t.Fatalf("an infeasible lower-ID two-ton quote prevented the higher-ID feasible one-ton fill: winner %s", winner.ShipID)
			}
			if attempted {
				for _, call := range calls {
					if call.IdempotencyKey != original.ID || !bytes.Equal(call.Body, original.Body) {
						t.Fatalf("attempted-order recovery lost immutable request identity: %+v", call)
					}
				}
			}
			_, filled := h.mission(winner.ShipID)
			if filled.ProvisionsPence != 12_654 || filled.FeesPence != 0 || filled.ExpensesPence != 0 || len(h.pending()) != 0 {
				t.Fatalf("one remaining ton did not produce one exact durable store receipt: %+v", filled)
			}
			h.api.mu.Lock()
			cashAfter := h.api.vaultBalance["north"]
			h.api.mu.Unlock()
			if proxy.remaining.Load() != 0 || cashAfter != cashBefore-12_654 || cashAfter < protected {
				t.Fatalf("depth reallocation double-spent stock or regional reserve: remaining %d cash %d -> %d reserve %d", proxy.remaining.Load(), cashBefore, cashAfter, protected)
			}
			loser := recoveryShipBlastoise
			if winner.ShipID == loser {
				loser = recoveryShipCharizard
			}
			for range 3 {
				actionLogRefresh(t, h, runner)
				mission, data := h.mission(loser)
				if _, err := runner.driveMission(h.ctx, harnessShip(t, runner, loser), mission, &data); err != nil {
					t.Fatal(err)
				}
			}
			if h.api.mutationCount("/v1/market/place-order") != len(calls) {
				t.Fatal("losing quote tried to purchase the ton already filled by another hull")
			}
		})
	}
}

func TestIdleMaintenanceRoadsteadHealthyStoresDoNotDockForUnaffordableGuns(t *testing.T) {
	h, runner, _, _ := idleMaintenanceFixture(t, true)
	h.api.mu.Lock()
	shipRow := h.api.fleet[0]["ship"].(map[string]any)
	shipRow["cannons_count"] = 2
	h.api.fleet[0]["effective_hold_capacity_tons"] = 56
	h.api.fleet[0]["stores"].(map[string]any)["provisions_tons"] = 2.0
	h.api.mu.Unlock()
	actionLogRefresh(t, h, runner)
	ship := harnessShip(t, runner, recoveryShipBlastoise)
	input, err := runner.committedPlanningInput(h.ctx, ship)
	if err != nil {
		t.Fatal(err)
	}
	h.api.setVaultBalance("north", input.ReserveByRegion["north"]+1_000)
	h.api.setVaultBalance("south", input.ReserveByRegion["south"])
	actionLogRefresh(t, h, runner)
	input, err = runner.committedPlanningInput(h.ctx, harnessShip(t, runner, recoveryShipBlastoise))
	if err != nil {
		t.Fatal(err)
	}
	hull, exists := findHull(input.Catalog, ship.Ship.ShipClass)
	if !exists {
		t.Fatal("gun-only roadstead fixture lacks its actual hull")
	}
	refitCost := (min(int64(4), hull.MaxCannons) - ship.Ship.CannonsCount) * 15_000
	if ship.Stores.ProvisionsTons < 1+float64(hull.CrewRequired)/200*2 || refitCost <= 0 ||
		input.Balances["north"]-refitCost >= input.ReserveByRegion["north"] ||
		runner.stateCache.Markets[recoveryHomePort].Visibility != "roadstead" {
		t.Fatalf("fixture does not isolate healthy stores and an unaffordable gun-only dock: ship %+v refit %d balances %v reserves %v", ship, refitCost, input.Balances, input.ReserveByRegion)
	}
	for tick := range 10 {
		if tick == 5 {
			runner = h.runner(primaryTestKey)
		}
		if _, err := h.tick(runner); err != nil {
			t.Fatal(err)
		}
	}
	if h.api.mutationCount("/v1/fleet/dock") != 0 || h.api.mutationCount("/v1/fleet/offshore") != 0 ||
		h.api.mutationCount("/v1/shipyard/modify-ship") != 0 || h.api.mutationCount("/v1/market/place-order") != 0 {
		t.Fatalf("known unaffordable optional guns caused speculative berth/refit traffic: docks %+v offshore %+v refits %+v buys %+v",
			h.api.mutationsFor("/v1/fleet/dock"), h.api.mutationsFor("/v1/fleet/offshore"), h.api.mutationsFor("/v1/shipyard/modify-ship"), h.api.mutationsFor("/v1/market/place-order"))
	}
	h.api.mu.Lock()
	local, donor := h.api.vaultBalance["north"], h.api.vaultBalance["south"]
	status := h.api.fleet[0]["ship"].(map[string]any)["status"]
	h.api.mu.Unlock()
	if local != input.Balances["north"] || donor != input.Balances["south"] || status != "anchored_roadstead" {
		t.Fatalf("gun-only wait moved the ship or spent protected liquidity: cash %d/%d initial %v status %v", local, donor, input.Balances, status)
	}
}
