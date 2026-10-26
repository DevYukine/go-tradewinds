package engine

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"math"
	"net/http"
	"testing"
	"time"

	"github.com/DevYukine/go-tradewinds/internal/store"
)

// This world advances only departures received through the real HTTP journal.
// It never inserts a return intent or moves an idle hull to another port.
type growthExecutionWorld struct {
	h                *recoveryHarness
	source           []Level
	bids             []Level
	provisions       map[string][]Level
	customs          int64
	built            bool
	localWork        bool
	unsafeReturn     bool
	staleSource      bool
	sourceNeedsBerth bool
	stopAfterSales   int
	sales            int
	firstFill        *int64
	lostReturn       bool
	lostOnce         bool
	gameHours        int64
	arrival          string
	arrivalTicks     int
	ledger           []map[string]any
	receipts         map[string]json.RawMessage
	admitted         *growthLaneIntent
	firstMission     string
}

type growthExecutionRequest struct {
	ShipID      string `json:"ship_id"`
	PortID      string `json:"port_id"`
	Commodity   string `json:"commodity_id"`
	Side        string `json:"side"`
	Quantity    int64  `json:"quantity"`
	Price       int64  `json:"price_pence"`
	IntoStores  bool   `json:"into_stores"`
	TimeInForce string `json:"time_in_force"`
	Destination string `json:"dest_port_id"`
	Cannons     int64  `json:"target_cannons_count"`
	Name        string `json:"name"`
	ShipClass   string `json:"ship_class"`
	From        string `json:"from_region"`
	To          string `json:"to_region"`
	Gross       int64  `json:"gross_amount_pence"`
}

func newGrowthExecutionWorld(t *testing.T) (*growthExecutionWorld, *Runner) {
	t.Helper()
	h := newRecoveryHarness(t)
	h.seedLaneProofs()
	h.api.setFactorPorts(recoveryHomePort, recoveryFarPort)
	h.api.setAutoRemit(true, 1_000_000)
	world := &growthExecutionWorld{
		h: h, customs: 200, gameHours: 2_400, stopAfterSales: 2,
		source: []Level{{OrderID: "cheap-twenty", PricePence: 100, RemainingQuantity: 20}, {OrderID: "expensive-depth", PricePence: 900, RemainingQuantity: 10_000}},
		bids:   []Level{{OrderID: "destination-demand", PricePence: 1_000, RemainingQuantity: 10_000}},
		provisions: map[string][]Level{
			recoveryHomePort: {{OrderID: "home-stores", PricePence: 50, RemainingQuantity: 10_000}},
			recoveryFarPort:  {{OrderID: "away-stores", PricePence: 50, RemainingQuantity: 10_000}},
		},
		ledger:   make([]map[string]any, 0),
		receipts: make(map[string]json.RawMessage),
	}
	h.api.server.Config.Handler = http.HandlerFunc(world.handle)
	return world, h.runner(primaryTestKey)
}

func (w *growthExecutionWorld) builtRow() map[string]any {
	for _, row := range w.h.api.fleet {
		if row["ship"].(map[string]any)["id"] == recoveryBuiltShipID {
			return row
		}
	}
	return nil
}

func (w *growthExecutionWorld) tick(runner *Runner) {
	w.h.t.Helper()
	w.h.api.mu.Lock()
	if w.arrival != "" {
		w.arrivalTicks--
		if w.arrivalTicks <= 0 {
			row := w.builtRow()
			ship := row["ship"].(map[string]any)
			ship["status"] = "anchored_roadstead"
			ship["current_port_id"] = w.arrival
			ship["berthed_at"] = nil
			row["voyage"] = nil
			row["route_legs"] = []any{}
			stores := row["stores"].(map[string]any)["provisions_tons"].(float64) - 0.1
			row["stores"] = map[string]any{"provisions_tons": max(0, stores)}
			w.arrival = ""
			w.gameHours += 5
		}
	}
	w.h.api.mu.Unlock()
	if _, err := w.h.tick(runner); err != nil {
		w.h.t.Fatalf("production growth tick: %v; holds %v", err, runner.holding)
	}
}

func (w *growthExecutionWorld) until(runner *Runner, predicate func(*store.Mission, missionData) bool) (*store.Mission, missionData) {
	w.h.t.Helper()
	for range 100 {
		mission, data, err := runner.loadMission(w.h.ctx, recoveryBuiltShipID)
		if err != nil {
			w.h.t.Fatal(err)
		}
		if predicate(mission, data) {
			return mission, data
		}
		w.tick(runner)
	}
	w.h.t.Fatalf("production growth transition did not arrive; holds %v", runner.holding)
	return nil, missionData{}
}

func (w *growthExecutionWorld) book(port string) map[string]any {
	var asks, bids []Level
	if port == recoveryHomePort {
		asks = w.source
		if w.localWork {
			bids = []Level{{OrderID: "better-home-demand", PricePence: 5_000, RemainingQuantity: 10_000}}
		}
	} else {
		bids = w.bids
		if w.localWork {
			asks = []Level{{OrderID: "better-away-work", PricePence: 1, RemainingQuantity: 10_000}}
		}
	}
	asOf := time.Now().UTC()
	if port == recoveryHomePort && w.staleSource {
		asOf = asOf.Add(-10 * time.Minute)
	}
	visibility := "quay"
	books := []any{
		map[string]any{"commodity_id": recoveryGrain, "depth": map[string]any{"asks": asks, "bids": bids}},
		map[string]any{"commodity_id": "provisions", "depth": map[string]any{"asks": w.provisions[port], "bids": []any{}}},
	}
	if port == recoveryHomePort && w.sourceNeedsBerth {
		row := w.builtRow()
		if row == nil || row["ship"].(map[string]any)["current_port_id"] != port ||
			row["ship"].(map[string]any)["status"] != "berthed" {
			visibility = "roadstead"
			books = []any{}
		}
	}
	return map[string]any{
		"port_id": port, "visibility": visibility, "customs_bps": w.customs,
		"as_of": asOf.Format(time.RFC3339Nano), "books": books,
	}
}

func (w *growthExecutionWorld) handle(writer http.ResponseWriter, request *http.Request) {
	api := w.h.api
	api.mu.Lock()
	defer api.mu.Unlock()
	if !api.authorized(request) {
		api.writeProblem(writer, http.StatusUnauthorized, "invalid_api_key", "isolated fixture credential rejected")
		return
	}
	if request.Method == http.MethodGet {
		switch request.URL.Path {
		case "/v1/market/book":
			api.writeJSON(writer, http.StatusOK, w.book(request.URL.Query().Get("port_id")))
		case "/v1/shipyard/quote":
			api.writeJSON(writer, http.StatusOK, map[string]any{
				"port_id": request.URL.Query().Get("port_id"), "ship_class": recoveryHull,
				"build_mode": "turnkey_cash", "available": !w.built, "total_cost_pence": 20_000,
			})
		case "/v1/world/clock":
			clock := api.clockDocument()
			clock["calendar"].(map[string]any)["total_game_hours"] = w.gameHours
			api.writeJSON(writer, http.StatusOK, clock)
		case "/v1/treasury/ledger":
			api.writeJSON(writer, http.StatusOK, w.ledger)
		case "/v1/world/route":
			from, to := request.URL.Query().Get("from"), request.URL.Query().Get("to")
			route := api.routeDocument(from, to)
			if w.unsafeReturn && from == recoveryFarPort && to == recoveryHomePort {
				route["legs"].([]any)[0].(map[string]any)["edge_class"] = "oceanic"
			}
			api.writeJSON(writer, http.StatusOK, route)
		default:
			api.handleGet(writer, request)
		}
		return
	}
	body, err := io.ReadAll(request.Body)
	if err != nil {
		w.h.t.Error(err)
		writer.WriteHeader(http.StatusBadRequest)
		return
	}
	var payload growthExecutionRequest
	if json.Unmarshal(body, &payload) != nil {
		w.h.t.Error("unreadable production mutation")
		writer.WriteHeader(http.StatusBadRequest)
		return
	}
	key := request.Header.Get("Idempotency-Key")
	if prior, exists := w.receipts[key]; exists {
		api.record(request, bytes.Clone(body), http.StatusOK)
		api.writeJSON(writer, http.StatusOK, prior)
		return
	}
	missionID := payload.ShipID
	if missionID == "" {
		missionID = "__growth"
	}
	var mission *store.Mission
	var data missionData
	var loadErr error
	for _, candidate := range []string{missionID, "__growth", recoveryBuiltShipID, "__treasury"} {
		mission, loadErr = w.h.db.GetMission(request.Context(), w.h.account.ID, candidate)
		if loadErr != nil {
			break
		}
		if mission != nil {
			loadErr = json.Unmarshal(mission.Data, &data)
			if loadErr != nil || data.Action != nil && data.Action.ID == key {
				break
			}
		}
	}
	journaled, journalErr := w.h.db.HasAction(request.Context(), w.h.account.ID, key)
	if loadErr != nil || journalErr != nil || !journaled || mission == nil || data.Action == nil ||
		data.Action.ID != key || !bytes.Equal(data.Action.Body, body) || data.Action.Path != request.URL.Path {
		w.h.t.Error("wire mutation was not its persisted immutable journal request")
		writer.WriteHeader(http.StatusConflict)
		return
	}
	reply := func(document any) {
		w.receipts[key] = encode(document)
		api.record(request, bytes.Clone(body), http.StatusOK)
		api.writeJSON(writer, http.StatusOK, document)
	}
	row := w.builtRow()
	switch request.URL.Path {
	case "/v1/shipyard/build-ship":
		if w.built || data.GrowthLane == nil || data.GrowthLane.QuantityLimit <= 0 || data.GrowthLane.QuantityLimit >= 20 {
			w.h.t.Error("build did not preserve the shallow marginal admission")
			writer.WriteHeader(http.StatusConflict)
			return
		}
		lane := *data.GrowthLane
		w.admitted = &lane
		w.built = true
		row = sailingShipRow(recoveryBuiltShipID, payload.Name, payload.PortID, recoveryFarPort)
		ship := row["ship"].(map[string]any)
		ship["status"] = "berthed"
		ship["cannons_count"] = int64(0)
		row["effective_hold_capacity_tons"] = int64(60)
		row["stores"] = map[string]any{"provisions_tons": float64(0)}
		row["route_legs"] = []any{}
		row["voyage"] = nil
		api.fleet = append(api.fleet, row)
		api.vaultBalance["north"] -= 20_000
		reply(map[string]any{"ship_id": recoveryBuiltShipID, "name": payload.Name, "ship_class": payload.ShipClass, "total_cost_pence": 20_000})
	case "/v1/shipyard/modify-ship":
		ship := row["ship"].(map[string]any)
		old := ship["cannons_count"].(int64)
		api.vaultBalance["north"] -= (payload.Cannons - old) * 15_000
		ship["cannons_count"] = payload.Cannons
		row["effective_hold_capacity_tons"] = 60 - 2*payload.Cannons
		reply(map[string]any{"ship_id": payload.ShipID, "cannons_count": payload.Cannons})
	case "/v1/market/place-order":
		if row == nil || payload.Quantity <= 0 || payload.TimeInForce != "ioc" || row["ship"].(map[string]any)["current_port_id"] != payload.PortID {
			w.h.t.Error("illegal market request")
			writer.WriteHeader(http.StatusConflict)
			return
		}
		quantity := payload.Quantity
		levels := w.source
		if payload.IntoStores {
			levels = w.provisions[payload.PortID]
		} else if payload.Side == "ask" {
			levels = w.bids
		} else if w.firstMission == "" {
			w.firstMission = mission.Generation
			if w.firstFill != nil {
				quantity = min(quantity, *w.firstFill)
			}
		}
		var fill DepthFill
		if quantity > 0 {
			fill, err = WalkDepth(levels, quantity, payload.Side == "bid")
			if err != nil || payload.Side == "bid" && fill.LimitPence > payload.Price || payload.Side == "ask" && fill.LimitPence < payload.Price {
				w.h.t.Error("IOC exceeded actual executable depth or immutable limit")
				writer.WriteHeader(http.StatusConflict)
				return
			}
		}
		customs := int64(0)
		region := "north"
		if payload.PortID == recoveryFarPort {
			region = "south"
		}
		manifest := row["cargo_manifest"].(map[string]any)
		if payload.IntoStores {
			customs = ceilBPS(fill.GrossPence, w.customs)
			api.vaultBalance[region] -= fill.GrossPence + customs
			stores := row["stores"].(map[string]any)["provisions_tons"].(float64)
			row["stores"] = map[string]any{"provisions_tons": stores + float64(fill.Quantity)}
			w.provisions[payload.PortID] = depleteDepth(levels, fill.Quantity, true)
		} else if payload.Side == "bid" {
			customs = ceilBPS(fill.GrossPence, w.customs)
			api.vaultBalance[region] -= fill.GrossPence + customs
			manifest[payload.Commodity] = fill.Quantity
			row["cargo_used_tons"] = fill.Quantity
			w.source = depleteDepth(w.source, fill.Quantity, true)
		} else {
			carried, _ := manifest[payload.Commodity].(int64)
			if carried < fill.Quantity {
				w.h.t.Error("sale exceeded authoritative cargo")
			}
			manifest[payload.Commodity] = carried - fill.Quantity
			row["cargo_used_tons"] = carried - fill.Quantity
			api.vaultBalance[region] += fill.GrossPence
			w.bids = depleteDepth(w.bids, fill.Quantity, false)
			w.sales++
			if w.stopAfterSales > 0 && w.sales >= w.stopAfterSales {
				w.source = nil
			}
		}
		reply(map[string]any{"commodity_id": payload.Commodity, "filled_quantity": fill.Quantity, "total_gross_pence": fill.GrossPence, "customs_duty_pence": customs})
	case "/v1/fleet/depart":
		ship := row["ship"].(map[string]any)
		from := ship["current_port_id"].(string)
		if ship["status"] == "sailing" || from == payload.Destination || row["stores"].(map[string]any)["provisions_tons"].(float64) < 2 {
			w.h.t.Error("departure was not safe stationary navigation")
			writer.WriteHeader(http.StatusConflict)
			return
		}
		if from == recoveryFarPort && (data.GrowthLane == nil || data.GrowthLane.NextPlan == nil || !data.ReturnJourney || mission.Phase != "return") {
			w.h.t.Error("return was not selected by the production continuation controller")
		}
		ship["status"] = "sailing"
		ship["berthed_at"] = nil
		voyage := sailingShipRow(payload.ShipID, ship["name"].(string), from, payload.Destination)
		row["voyage"] = voyage["voyage"]
		row["route_legs"] = voyage["route_legs"]
		w.arrival, w.arrivalTicks = payload.Destination, 2
		dues := int64(0)
		if from == recoveryFarPort {
			dues = 17
			w.bill(key+"-return", "south", dues)
		}
		result := map[string]any{"ship_id": payload.ShipID, "dest_port_id": payload.Destination, "berth_dues_pence": dues, "berth_dues_debt_pence": 0}
		if from == recoveryFarPort && w.lostReturn && !w.lostOnce {
			w.lostOnce = true
			w.receipts[key] = encode(result)
			api.record(request, bytes.Clone(body), 0)
			connection, _, hijackErr := writer.(http.Hijacker).Hijack()
			if hijackErr == nil {
				_ = connection.Close()
			}
			return
		}
		reply(result)
	case "/v1/fleet/dock", "/v1/fleet/offshore":
		ship := row["ship"].(map[string]any)
		ship["status"] = "berthed"
		if request.URL.Path == "/v1/fleet/offshore" {
			ship["status"] = "anchored_roadstead"
		}
		reply(map[string]any{"ship_id": payload.ShipID, "status": ship["status"]})
	case "/v1/treasury/remit":
		feeBPS := int64(-1)
		for _, rate := range api.rates {
			if rate.FromRegion == payload.From && rate.ToRegion == payload.To {
				feeBPS = rate.FeeBPS
			}
		}
		if feeBPS < 0 || payload.Gross <= 0 || api.vaultBalance[payload.From] < payload.Gross {
			w.h.t.Error("remittance lacked real money or measured fees")
			writer.WriteHeader(http.StatusConflict)
			return
		}
		fee := payload.Gross * feeBPS / 10_000
		api.vaultBalance[payload.From] -= payload.Gross
		api.vaultBalance[payload.To] += payload.Gross - fee
		reply(map[string]any{"gross_amount_pence": payload.Gross, "fee_pence": fee, "net_credited_pence": payload.Gross - fee})
	default:
		w.h.t.Errorf("unexpected production endpoint %s", request.URL.Path)
		writer.WriteHeader(http.StatusConflict)
	}
}

func (w *growthExecutionWorld) bill(id, region string, amount int64) {
	w.h.api.vaultBalance[region] -= amount
	w.ledger = append(w.ledger, map[string]any{"id": id, "debit_account_id": "vault-" + region, "credit_account_id": "harbour", "amount_pence": amount, "reason": "berth dues"})
}

func (w *growthExecutionWorld) settlementRows() []store.Settlement {
	w.h.t.Helper()
	rows, err := w.h.db.SettlementHistory(w.h.ctx, w.h.account.ID, "", 100)
	if err != nil {
		w.h.t.Fatal(err)
	}
	var actual []store.Settlement
	for _, row := range rows {
		if row.ShipID == recoveryBuiltShipID {
			actual = append(actual, row)
		}
	}
	return actual
}

func TestGrowthAllocationSurvivesCommissioningAndTwoProductionSpotCycles(t *testing.T) {
	t.Parallel()
	w, runner := newGrowthExecutionWorld(t)
	restarted := make(map[string]bool)
	var admitted *growthLaneIntent
	for range 110 {
		mission, data, err := runner.loadMission(w.h.ctx, recoveryBuiltShipID)
		if err != nil {
			t.Fatal(err)
		}
		w.h.api.mu.Lock()
		admitted = w.admitted
		w.h.api.mu.Unlock()
		if mission != nil {
			if data.GrowthLane == nil || admitted == nil || data.GrowthLane.AdmissionID != admitted.AdmissionID || data.GrowthLane.QuantityLimit != admitted.QuantityLimit {
				t.Fatal("atomic handoff/restart lost the admitted own lot")
			}
			checkpoint := mission.Generation + ":" + mission.Phase
			if !restarted[checkpoint] && (mission.Phase == "return" || mission.Phase == "return_sailing" || mission.Phase == "settling" || mission.Phase == "settled") {
				restarted[checkpoint] = true
				runner = w.h.runner(primaryTestKey)
			}
		}
		if growth, _, err := runner.loadMission(w.h.ctx, "__growth"); err != nil {
			t.Fatal(err)
		} else if growth != nil && growth.Phase == "commissioning" && !restarted["handoff"] {
			restarted["handoff"] = true
			runner = w.h.runner(primaryTestKey)
		}
		if rows := w.settlementRows(); len(rows) == 2 {
			var returnExpense int64
			for _, row := range rows {
				if row.Lane != laneKey(Plan{Kind: "spot", Origin: recoveryHomePort, Destination: recoveryFarPort, Commodity: recoveryGrain}) || row.QuantityTons <= 0 {
					t.Fatalf("continuation lost actual first-lane delivery evidence: %+v", row)
				}
				returnExpense += row.ExpensesPence
			}
			if returnExpense != 17 {
				t.Fatalf("actual return/closing bills were omitted or charged twice: %d", returnExpense)
			}
			break
		}
		w.tick(runner)
	}
	w.h.api.mu.Lock()
	sales := w.sales
	w.h.api.mu.Unlock()
	if len(w.settlementRows()) != 2 || sales != 2 || !restarted["handoff"] {
		t.Fatalf("two real cycles/handoff did not finish: settlements %d sales %d", len(w.settlementRows()), sales)
	}
	var purchases, returns int
	for _, call := range w.h.api.mutationsFor("/v1/market/place-order") {
		var request growthExecutionRequest
		if json.Unmarshal(call.Body, &request) != nil {
			t.Fatal("invalid recorded purchase")
		}
		if !request.IntoStores && request.Side == "bid" {
			purchases++
			if request.Quantity > admitted.QuantityLimit || request.Quantity >= 20 {
				t.Fatal("commissioning or restart expanded a shallow admitted lot to the full hold")
			}
			if purchases == 1 && growthCargoCash(Plan{Quantity: request.Quantity, BuyLimitPence: request.Price}, w.customs) > admitted.InitialProcurementLimitPence {
				t.Fatal("first actual IOC exceeded its own frozen cargo/customs allocation")
			}
		}
	}
	for _, call := range w.h.api.mutationsFor("/v1/fleet/depart") {
		var request growthExecutionRequest
		_ = json.Unmarshal(call.Body, &request)
		if request.Destination == recoveryHomePort {
			returns++
		}
	}
	if purchases != 2 || returns != 1 || w.h.api.mutationCount("/v1/shipyard/build-ship") != 1 {
		t.Fatalf("real journal transitions were not two bounded buys and one return: buys %d returns %d", purchases, returns)
	}

}

func TestGrowthOptionalReturnYieldsToCurrentAlternativesAndAuthority(t *testing.T) {
	t.Parallel()
	for _, scenario := range []string{"better_local", "lost_source_presence", "stale_source_book", "depleted_source", "depleted_destination", "unsafe_return", "short_stores"} {
		t.Run(scenario, func(t *testing.T) {
			w, runner := newGrowthExecutionWorld(t)
			w.until(runner, func(mission *store.Mission, data missionData) bool {
				return mission != nil && mission.Phase == "arrival" && data.DeliveredQuantity > 0 && data.Action == nil
			})
			w.h.api.mu.Lock()
			switch scenario {
			case "better_local":
				w.localWork = true
			case "lost_source_presence":
				w.h.api.factorPorts = []string{recoveryFarPort}
			case "stale_source_book":
				w.staleSource = true
			case "depleted_source":
				w.source = nil
			case "depleted_destination":
				w.bids = nil
			case "unsafe_return":
				w.unsafeReturn = true
			case "short_stores":
				w.builtRow()["stores"] = map[string]any{"provisions_tons": float64(0)}
				w.provisions[recoveryFarPort] = nil
			}
			w.h.api.mu.Unlock()
			w.until(runner, func(mission *store.Mission, data missionData) bool {
				return mission != nil && mission.Phase == "settled"
			})
			for _, call := range w.h.api.mutationsFor("/v1/fleet/depart") {
				var request growthExecutionRequest
				_ = json.Unmarshal(call.Body, &request)
				if request.Destination == recoveryHomePort {
					t.Fatal("an old profitable delivery forced a now-inferior or unauthorized empty return")
				}
			}
		})
	}

}

func TestGrowthSuccessfulPartialAndZeroIOCStayCompletedAcrossRestart(t *testing.T) {
	t.Parallel()
	for _, fill := range []int64{0, 1} {
		t.Run(fmt.Sprintf("fill_%d", fill), func(t *testing.T) {
			w, runner := newGrowthExecutionWorld(t)
			w.firstFill = &fill
			mission, data := w.until(runner, func(mission *store.Mission, data missionData) bool {
				return mission != nil && data.BuyDone && data.Action == nil
			})
			if !data.GrowthLane.InitialProcurementComplete || data.BoughtQuantity != fill || data.Plan.Quantity != fill {
				t.Fatalf("successful bounded IOC was not terminal: %+v", data)
			}
			generation := mission.Generation
			runner = w.h.runner(primaryTestKey)
			for range 4 {
				w.tick(runner)
			}
			var buys int
			for _, call := range w.h.api.mutationsFor("/v1/market/place-order") {
				var request growthExecutionRequest
				_ = json.Unmarshal(call.Body, &request)
				if request.Side == "bid" && !request.IntoStores {
					buys++
				}
			}
			if buys != 1 {
				t.Fatalf("partial/zero IOC reopened after restart of %s: buys %d", generation, buys)
			}
		})
	}

}

func TestGrowthNeverSentCargoRevalidatesCurrentCashCustomsAndDepth(t *testing.T) {
	t.Parallel()
	for _, change := range []string{"customs", "one_penny_short", "residual_depth"} {
		t.Run(change, func(t *testing.T) {
			w, runner := newGrowthExecutionWorld(t)
			mission, data := w.until(runner, func(mission *store.Mission, data missionData) bool {
				return mission != nil && data.Action != nil && data.Action.Stage == "buy"
			})
			before := w.h.api.mutationCount("/v1/market/place-order")
			if err := runner.growthAllocationReady(w.h.ctx, data.Action.Before, &data, data.Action); err != nil {
				t.Fatalf("unchanged admitted first send was not ready: %v", err)
			}
			w.h.api.mu.Lock()
			switch change {
			case "customs":
				input := runner.planningInput(data.Action.Before)
				required := cargoFundingRequired(input, data.Plan)
				w.h.api.vaultBalance["north"] = required["north"]
				w.h.api.rates = nil
				w.customs = 9_000
			case "one_penny_short":
				input := runner.planningInput(data.Action.Before)
				required := cargoFundingRequired(input, data.Plan)
				w.h.api.vaultBalance["north"] = required["north"] - 1
				w.h.api.rates = nil
			case "residual_depth":
				w.source = []Level{{OrderID: "one-remaining", PricePence: data.Plan.BuyLimitPence, RemainingQuantity: max(0, data.Plan.Quantity-1)}}
			}
			w.h.api.mu.Unlock()
			w.tick(runner)
			if w.h.api.mutationCount("/v1/market/place-order") != before {
				t.Fatal("changed first-send terms reached HTTP transport")
			}
			after, _, err := runner.loadMission(w.h.ctx, recoveryBuiltShipID)
			if err != nil || after == nil || after.Generation != mission.Generation {
				t.Fatal("never-sent repricing lost the admitted mission identity")
			}
		})
	}

}

func TestGrowthAmbiguousReturnKeepsAttemptAndActualCostAfterMarketLoss(t *testing.T) {
	t.Parallel()
	w, runner := newGrowthExecutionWorld(t)
	w.lostReturn = true
	w.until(runner, func(mission *store.Mission, data missionData) bool {
		return mission != nil && data.Action != nil && data.Action.Stage == "depart" && data.ReturnJourney
	})
	w.tick(runner)
	w.h.api.mu.Lock()
	w.source = nil
	w.h.api.factorPorts = []string{recoveryFarPort}
	w.h.api.mu.Unlock()
	runner = w.h.runner(primaryTestKey)
	w.until(runner, func(mission *store.Mission, data missionData) bool {
		return mission != nil && mission.Phase == "settled"
	})
	rows := w.settlementRows()
	if len(rows) != 1 || rows[0].ExpensesPence != 17 || rows[0].QuantityTons <= 0 {
		t.Fatalf("committed return loss erased actual bills or delivered lane: %+v", rows)
	}
	keys := make(map[string]bool)
	for _, call := range w.h.api.mutationsFor("/v1/fleet/depart") {
		var request growthExecutionRequest
		_ = json.Unmarshal(call.Body, &request)
		if request.Destination == recoveryHomePort {
			keys[call.IdempotencyKey] = true
		}
	}
	w.h.api.mu.Lock()
	lostOnce := w.lostOnce
	w.h.api.mu.Unlock()
	if len(keys) != 1 || !lostOnce {
		t.Fatal("ambiguous return was abandoned or replaced by another attempted identity")
	}

}

func TestGrowthSelectedNextLotReservesOnceAndExcludesItsOwner(t *testing.T) {
	t.Parallel()
	w, runner := newGrowthExecutionWorld(t)
	mission, data := w.until(runner, func(mission *store.Mission, data missionData) bool {
		return mission != nil && mission.Phase == "return" && data.GrowthLane.NextPlan != nil && data.Action == nil
	})
	ship, _ := runner.ship(recoveryBuiltShipID)
	own, err := runner.committedPlanningInput(w.h.ctx, ship)
	if err != nil {
		t.Fatal(err)
	}
	company, err := runner.committedPlanningInput(w.h.ctx, ShipState{})
	if err != nil {
		t.Fatal(err)
	}
	expected := clonePence(own.ReserveByRegion)
	runner.reserveMissionCash(expected, mission, &data, ship)
	for region, required := range expected {
		if company.ReserveByRegion[region] != required {
			t.Fatalf("next cargo or paid first cargo was reserved twice in %s: company %d expected %d", region, company.ReserveByRegion[region], required)
		}
	}
	evidence := growthEvidence{workingByRegion: make(map[string]int64)}
	runner.reserveGrowthMission(&evidence, mission, &data)
	if len(evidence.incumbents) != 1 || evidence.incumbents[0].unbought != data.GrowthLane.NextPlan.Quantity || evidence.incumbents[0].outstanding != data.GrowthLane.NextPlan.Quantity {
		t.Fatalf("completed first lot was subtracted from or duplicated into selected next depth: %+v", evidence.incumbents)
	}
	if math.IsNaN(data.GrowthLane.NextPlan.Economics.NetPencePerGameDay) {
		t.Fatal("selected executable operating quote had no finite rate")
	}

}

func TestGrowthContinuationFundsMeasuredRemitAndChargesFeeOnce(t *testing.T) {
	t.Parallel()
	w, runner := newGrowthExecutionWorld(t)
	w.until(runner, func(mission *store.Mission, data missionData) bool {
		return mission != nil && mission.Phase == "arrival" && data.DeliveredQuantity > 0 && data.Action == nil
	})
	ship, _ := runner.ship(recoveryBuiltShipID)
	input, err := runner.committedPlanningInput(w.h.ctx, ship)
	if err != nil {
		t.Fatal(err)
	}
	w.h.api.setVaultBalance("north", input.ReserveByRegion["north"])
	mission, data := w.until(runner, func(mission *store.Mission, data missionData) bool {
		return mission != nil && data.Action != nil && data.Action.Stage == "remit"
	})
	if mission.ShipID != recoveryBuiltShipID || data.GrowthLane.NextPlan == nil || mission.Phase != "return" {
		t.Fatal("selected return funding was detached from its operating mission")
	}
	initialFees := data.FeesPence
	w.tick(runner)
	_, funded := w.h.mission(recoveryBuiltShipID)
	if funded.Action != nil || funded.FeesPence <= initialFees || funded.ReturnJourney == false {
		t.Fatal("ship-owned remittance completed or lost the voyage instead of funding it")
	}
	fee := funded.FeesPence - initialFees
	runner = w.h.runner(primaryTestKey)
	w.until(runner, func(mission *store.Mission, data missionData) bool {
		return mission != nil && mission.Phase == "settled"
	})
	rows := w.settlementRows()
	if len(rows) != 1 || rows[0].FeesPence != initialFees+fee || rows[0].ExpensesPence != 17 {
		t.Fatalf("return/remittance cash was lost or duplicated after restart: %+v", rows)
	}

}

func TestGrowthReturnRepricesNeverSentItineraryButKeepsPaidReturnSunk(t *testing.T) {
	t.Parallel()
	t.Run("never_sent", func(t *testing.T) {
		w, runner := newGrowthExecutionWorld(t)
		w.until(runner, func(mission *store.Mission, data missionData) bool {
			return mission != nil && data.Action != nil && data.Action.Stage == "depart" && data.ReturnJourney
		})
		before := w.h.api.mutationCount("/v1/fleet/depart")
		w.h.api.mu.Lock()
		w.localWork = true
		w.h.api.mu.Unlock()
		w.tick(runner)
		if w.h.api.mutationCount("/v1/fleet/depart") != before {
			t.Fatal("a now-inferior never-sent return escaped journal preflight")
		}
		w.until(runner, func(mission *store.Mission, data missionData) bool {
			return mission != nil && mission.Phase == "settled"
		})
		if rows := w.settlementRows(); len(rows) != 1 || rows[0].ExpensesPence != 0 {
			t.Fatalf("an unattempted return invented a cash expense: %+v", rows)
		}
	})
	t.Run("paid_return_is_sunk", func(t *testing.T) {
		w, runner := newGrowthExecutionWorld(t)
		w.until(runner, func(mission *store.Mission, data missionData) bool {
			return mission != nil && mission.Phase == "return_sailing" && data.Action == nil
		})
		w.h.api.mu.Lock()
		w.source = []Level{{OrderID: "small-positive-owned-lot", PricePence: 950, RemainingQuantity: 4}}
		firstMission := w.firstMission
		w.h.api.mu.Unlock()
		w.until(runner, func(mission *store.Mission, data missionData) bool {
			return mission != nil && mission.Generation != firstMission && data.BuyDone && data.Action == nil
		})
		var purchases []growthExecutionRequest
		for _, call := range w.h.api.mutationsFor("/v1/market/place-order") {
			var request growthExecutionRequest
			_ = json.Unmarshal(call.Body, &request)
			if !request.IntoStores && request.Side == "bid" {
				purchases = append(purchases, request)
			}
		}
		if len(purchases) != 2 || purchases[1].Quantity <= 0 || purchases[1].Quantity > 4 || purchases[1].Price != 950 {
			t.Fatalf("sunk hull/return payback re-gated the smaller positive funded operating lot: %+v", purchases)
		}
		if rows := w.settlementRows(); len(rows) != 1 || rows[0].ExpensesPence != 17 {
			t.Fatalf("repricing the next purchase erased the real return expense: %+v", rows)
		}
	})

}

func TestGrowthContinuationCharterDeadlineIncludesRepositioning(t *testing.T) {
	t.Parallel()
	w, runner := newGrowthExecutionWorld(t)
	_, data := w.until(runner, func(mission *store.Mission, data missionData) bool {
		return mission != nil && mission.Phase == "arrival" && data.DeliveredQuantity > 0 && data.Action == nil
	})
	if err := runner.refresh(w.h.ctx); err != nil {
		t.Fatal(err)
	}
	ship, _ := runner.ship(recoveryBuiltShipID)
	input, err := runner.committedPlanningInput(w.h.ctx, ship)
	if err != nil {
		t.Fatal(err)
	}
	back, err := growthReturnBasis(input, data.Plan.Origin)
	if err != nil {
		t.Fatal(err)
	}
	base := data.Plan
	base.Kind = "charter"
	base.ContractID = "fresh-next-charter"
	reward := int64(1_000)
	input.Contracts = []Contract{{
		ID: base.ContractID, ContractType: "charter", Status: "open",
		OriginPortID: base.Destination, CommodityID: base.Commodity,
		RemainingQuantity: data.GrowthLane.QuantityLimit, RewardPerTonPence: &reward,
	}}
	withoutDeadline := quoteGrowthDelivery(input, base, data.GrowthLane, false, nil)
	if !withoutDeadline.plan.Accepted {
		t.Fatal("fixture lacks positive funded charter work before the deadline boundary")
	}
	_, upper, _ := input.serverBounds()
	expiry := upper.Add(gameDuration(withoutDeadline.plan.Economics.GameDays+1+back.days/2, input.Clock.Calendar.TimeScale))
	input.Contracts[0].ExpiresAt = &expiry
	if direct := quoteGrowthDelivery(input, base, data.GrowthLane, false, nil); !direct.plan.Accepted {
		t.Fatal("charter deadline should permit the direct delivery by itself")
	}
	if repositioned := quoteGrowthDelivery(input, base, data.GrowthLane, false, &back); repositioned.plan.Accepted {
		t.Fatal("charter expiry ignored the real empty repositioning leg")
	}

}

func TestGrowthResidualOperatingQuoteReservesOtherUnboughtAndOutstandingLotsOnce(t *testing.T) {
	t.Parallel()
	w, runner := newGrowthExecutionWorld(t)
	_, data := w.until(runner, func(mission *store.Mission, data missionData) bool {
		return mission != nil && data.Action != nil && data.Action.Stage == "buy"
	})
	ship, _ := runner.ship(recoveryBuiltShipID)
	other := data.Plan
	other.Quantity = max(1, 20-data.Plan.Quantity+1)
	other.StoresToBuy = 0
	other.TargetStoresTons = 0
	w.h.putMission(recoveryShipCharizard, "unbought-incumbent", "provision", missionData{Plan: other, CostsKnown: true})
	outstanding := data.Plan
	outstanding.Quantity = 3
	outstanding.StoresToBuy = 0
	outstanding.TargetStoresTons = 0
	w.h.putMission(recoveryShipBlastoise, "owned-outstanding-incumbent", "sailing",
		missionData{Plan: outstanding, BoughtQuantity: 3, BuyDone: true, CostsKnown: true})
	input, err := runner.committedPlanningInput(w.h.ctx, ship)
	if err != nil {
		t.Fatal(err)
	}
	asks := bookCommodity(input.Markets[recoveryHomePort], recoveryGrain)
	bids := bookCommodity(input.Markets[recoveryFarPort], recoveryGrain)
	if depthCapacity(asks.Depth.Asks, 100, true) != max(0, 20-other.Quantity) ||
		depthCapacity(bids.Depth.Bids, 1_000, false) != 10_000-other.Quantity-3 {
		t.Fatal("owned residual lot omitted or duplicated an incumbent source/destination reservation")
	}
	if err := runner.growthAllocationReady(w.h.ctx, ship, &data, data.Action); err == nil {
		t.Fatal("old cheap first-send allocation consumed asks already committed to an incumbent")
	}
	base := runner.planningInput(ship)
	incumbentCash := growthCashTotal(growthCargoCash(other, w.customs), other.Economics.BerthDuesPence,
		other.Economics.CollateralPence, other.Economics.TollPence, voyageFleetBills(base, other))
	if input.ReserveByRegion["north"]-base.ReserveByRegion["north"] != incumbentCash {
		t.Fatal("paid outstanding cargo was reserved as another purchase, or the unbought lot was not protected once")
	}

}

func TestGrowthInitialIOCExactFundingBoundary(t *testing.T) {
	t.Parallel()
	w, runner := newGrowthExecutionWorld(t)
	_, data := w.until(runner, func(mission *store.Mission, data missionData) bool {
		return mission != nil && data.Action != nil && data.Action.Stage == "buy"
	})
	ship, _ := runner.ship(recoveryBuiltShipID)
	input, err := runner.committedPlanningInput(w.h.ctx, ship)
	if err != nil {
		t.Fatal(err)
	}
	// Growth's separately funded commissioning quote protects incumbent
	// upkeep in its existing reserves, not a second ordinary fleet bill.
	input.FleetDailyUpkeepPence = 0
	required := cargoFundingRequired(input, data.Plan)
	w.h.api.setRates()
	w.h.api.setVaultBalance("north", required["north"])
	if err := runner.refresh(w.h.ctx); err != nil {
		t.Fatal(err)
	}
	ship, _ = runner.ship(recoveryBuiltShipID)
	if err := runner.growthAllocationReady(w.h.ctx, ship, &data, data.Action); err != nil {
		t.Fatalf("exact own cargo/customs/bills/reserve funding was rejected: %v", err)
	}
	w.h.api.setVaultBalance("north", required["north"]-1)
	if err := runner.refresh(w.h.ctx); err != nil {
		t.Fatal(err)
	}
	ship, _ = runner.ship(recoveryBuiltShipID)
	if err := runner.growthAllocationReady(w.h.ctx, ship, &data, data.Action); err == nil {
		t.Fatal("initial immutable IOC was admitted one pence short")
	}

}

func TestGrowthFreshlyDisprovedCommissioningReleasesThroughAtomicHandoff(t *testing.T) {
	t.Parallel()
	w, runner := newGrowthExecutionWorld(t)
	w.until(runner, func(mission *store.Mission, data missionData) bool {
		w.h.api.mu.Lock()
		defer w.h.api.mu.Unlock()
		return w.built
	})
	w.h.api.mu.Lock()
	w.source = nil
	w.h.api.mu.Unlock()
	mission, data := w.until(runner, func(mission *store.Mission, data missionData) bool {
		return mission != nil && mission.Phase == "closing"
	})
	w.h.api.mu.Lock()
	admitted := w.admitted
	w.h.api.mu.Unlock()
	if data.Plan.Kind != "aborted" || data.GrowthLane == nil ||
		data.GrowthLane.AdmissionID != admitted.AdmissionID ||
		mission.Generation != growthHandoffGeneration(admitted.AdmissionID, recoveryBuiltShipID) {
		t.Fatal("freshly disproved first lot exposed an unallocated built hull instead of the atomic closing handoff")
	}
	w.h.missingMission("__growth")
	if w.h.api.mutationCount("/v1/market/place-order") != 0 || w.h.api.mutationCount("/v1/shipyard/modify-ship") != 0 {
		t.Fatal("freshly disproved commissioning spent on the old procurement or hardware package")
	}

}

func TestGrowthReturnedContinuationRetainsBoundedIntentWhileSourceIsStale(t *testing.T) {
	t.Parallel()
	w, runner := newGrowthExecutionWorld(t)
	mission, data := w.until(runner, func(mission *store.Mission, data missionData) bool {
		return mission != nil && mission.Phase == "settled" && data.GrowthLane.NextPlan != nil
	})
	generation := mission.Generation
	quantityLimit := data.GrowthLane.QuantityLimit
	w.h.api.mu.Lock()
	w.staleSource = true
	w.h.api.mu.Unlock()
	runner = w.h.runner(primaryTestKey)
	for range 4 {
		w.tick(runner)
	}
	held, heldData := w.h.mission(recoveryBuiltShipID)
	rows := w.settlementRows()
	if held.Generation == generation || held.Phase != "provision" || heldData.GrowthLane == nil ||
		heldData.GrowthLane.QuantityLimit != quantityLimit || heldData.BuyDone || heldData.Settlement != nil ||
		len(rows) != 1 || rows[0].MissionID != generation || rows[0].ExpensesPence != 17 {
		t.Fatal("stale source inspection discarded the immutable settlement or bounded next delivery")
	}
	w.h.api.mu.Lock()
	w.staleSource = false
	w.h.api.mu.Unlock()
	_, next := w.until(runner, func(mission *store.Mission, data missionData) bool {
		return mission != nil && mission.Generation != generation && data.BuyDone && data.Action == nil
	})
	if next.BoughtQuantity <= 0 || next.BoughtQuantity > quantityLimit {
		t.Fatal("fresh source recovery bypassed the retained quantity ceiling")
	}

}

func TestGrowthPaidReturnInspectsSourceBerthBeforeRequotingCargo(t *testing.T) {
	t.Parallel()
	for _, negative := range []bool{false, true} {
		t.Run(fmt.Sprintf("fresh_negative_%t", negative), func(t *testing.T) {
			w, runner := newGrowthExecutionWorld(t)
			first, firstData := w.until(runner, func(mission *store.Mission, data missionData) bool {
				return mission != nil && mission.Phase == "return_sailing" && data.Action == nil
			})
			quantityLimit := firstData.GrowthLane.QuantityLimit
			w.h.api.mu.Lock()
			w.sourceNeedsBerth = true
			w.h.api.factorPorts = []string{recoveryFarPort}
			w.h.api.mu.Unlock()
			runner = w.h.runner(primaryTestKey)
			nextMission, nextData := w.until(runner, func(mission *store.Mission, data missionData) bool {
				return mission != nil && mission.Generation != first.Generation && data.Action != nil &&
					data.Action.Stage == "dock" && data.Action.Before.Ship.CurrentPortID == recoveryHomePort
			})
			if nextData.Plan.Accepted || nextData.BuyDone || nextData.GrowthLane == nil ||
				nextData.GrowthLane.QuantityLimit != quantityLimit || nextData.Plan.Quantity > quantityLimit {
				t.Fatal("missing source depth bypassed the conditional inspection or quantity bound")
			}
			rows := w.settlementRows()
			if len(rows) != 1 || rows[0].MissionID != first.Generation || rows[0].ExpensesPence != 17 {
				t.Fatal("opening the source inspection changed or repeated the paid-return settlement")
			}
			if negative {
				w.h.api.mu.Lock()
				w.source = nil
				w.h.api.mu.Unlock()
			}
			dockID := nextData.Action.ID
			w.tick(runner)
			var sourceDockRequests int
			for _, call := range w.h.api.mutationsFor("/v1/fleet/dock") {
				if call.IdempotencyKey == dockID {
					sourceDockRequests++
				}
			}
			if sourceDockRequests != 1 {
				t.Fatal("paid return did not perform one real journaled source inspection dock")
			}
			if negative {
				w.until(runner, func(mission *store.Mission, data missionData) bool {
					return mission != nil && mission.Generation == nextMission.Generation &&
						data.Plan.Kind == "aborted" && mission.Phase == "settled"
				})
			} else {
				_, purchased := w.until(runner, func(mission *store.Mission, data missionData) bool {
					return mission != nil && mission.Generation == nextMission.Generation && data.BuyDone && data.Action == nil
				})
				if purchased.BoughtQuantity <= 0 || purchased.BoughtQuantity > quantityLimit {
					t.Fatal("fresh post-dock procurement lost the original operating quantity ceiling")
				}
			}
			var cargoPurchases int
			for _, call := range w.h.api.mutationsFor("/v1/market/place-order") {
				var request growthExecutionRequest
				_ = json.Unmarshal(call.Body, &request)
				if request.Side == "bid" && !request.IntoStores {
					cargoPurchases++
				}
			}
			expectedPurchases := 2
			if negative {
				expectedPurchases = 1
			}
			if cargoPurchases != expectedPurchases {
				t.Fatalf("source inspection bypassed fresh procurement or reopened an IOC: got %d want %d", cargoPurchases, expectedPurchases)
			}
		})
	}

}

func TestGrowthSourceInspectionDockPreservesExactRegionalFunding(t *testing.T) {
	t.Parallel()
	w, runner := newGrowthExecutionWorld(t)
	first, _ := w.until(runner, func(mission *store.Mission, data missionData) bool {
		return mission != nil && mission.Phase == "return_sailing" && data.Action == nil
	})
	w.h.api.mu.Lock()
	w.sourceNeedsBerth = true
	w.h.api.factorPorts = []string{recoveryFarPort}
	w.h.api.mu.Unlock()
	_, data := w.until(runner, func(mission *store.Mission, data missionData) bool {
		return mission != nil && mission.Generation != first.Generation && data.Action != nil &&
			data.Action.Stage == "dock" && data.Action.Before.Ship.CurrentPortID == recoveryHomePort
	})
	input, err := runner.committedPlanningInput(w.h.ctx, data.Action.Before)
	if err != nil {
		t.Fatal(err)
	}
	bill := input.Ports[recoveryHomePort].BerthDuesPerDayPence / 4
	if input.Ports[recoveryHomePort].BerthDuesPerDayPence%4 != 0 {
		bill++
	}
	required := input.ReserveByRegion["north"] + bill
	before := w.h.api.mutationCount("/v1/fleet/dock")
	w.h.api.setVaultBalance("north", required-1)
	w.tick(runner)
	if w.h.api.mutationCount("/v1/fleet/dock") != before {
		t.Fatal("source inspection spent a harbour bill one pence inside protected reserves")
	}
	w.h.api.setVaultBalance("north", required)
	w.until(runner, func(mission *store.Mission, data missionData) bool {
		return mission != nil && mission.Generation != first.Generation && data.Action != nil && data.Action.Stage == "dock"
	})
	w.tick(runner)
	if w.h.api.mutationCount("/v1/fleet/dock") != before+1 {
		t.Fatal("exact funded source inspection could not dock without inventing cargo receipts")
	}
	rows := w.settlementRows()
	if len(rows) != 1 || rows[0].MissionID != first.Generation || rows[0].ExpensesPence != 17 {
		t.Fatal("inspection funding changed the immutable first return cost")
	}

}
