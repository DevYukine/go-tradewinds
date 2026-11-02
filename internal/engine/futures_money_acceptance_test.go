package engine

import (
	"bytes"
	"database/sql"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/DevYukine/go-tradewinds/internal/gameapi"
	"github.com/DevYukine/go-tradewinds/internal/store"
	"github.com/google/uuid"
)

type futuresMoneyAPI struct {
	mu         sync.Mutex
	h          *recoveryHarness
	downstream http.Handler
	now        time.Time
	quantity   int64
	locked     int64
	leased     int64
	cash       int64
	rows       map[string]map[string]any
	creates    []map[string]any
	settles    []string
	acceptKeys []string
	accepts    int
	mutations  int
	hide       bool
}

func newFuturesMoneyAPI(t *testing.T) (*recoveryHarness, *futuresMoneyAPI) {
	t.Helper()
	h := newRecoveryHarness(t)
	p := &futuresMoneyAPI{h: h, downstream: h.api.server.Config.Handler, now: time.Now().UTC(), quantity: 1,
		leased: 20, cash: 1_000_000, rows: make(map[string]map[string]any)}
	h.api.setFleet([]map[string]any{})
	h.api.mu.Lock()
	h.api.factorPorts = []string{"amsterdam"}
	h.api.vaultBalance["north_sea"] = p.cash
	h.api.mu.Unlock()
	h.api.server.Config.Handler = p
	return h, p
}

func (p *futuresMoneyAPI) warehouse() map[string]any {
	return map[string]any{"port_id": "amsterdam", "has_factor": true, "leased_tonnage": p.leased,
		"reserved_tonnage": 0, "used_tonnage": p.quantity, "free_tonnage": p.leased - p.quantity,
		"inventory": []any{map[string]any{"company_id": recoveryCompanyID, "port_id": "amsterdam", "commodity_id": "water_beer",
			"quantity": p.quantity, "locked_quantity": p.locked, "created_at": p.now.Add(-time.Hour), "updated_at": p.now}}}
}

func (p *futuresMoneyAPI) ServeHTTP(w http.ResponseWriter, request *http.Request) {
	path := request.URL.Path
	if !strings.HasPrefix(path, "/v1/future/") && path != "/v1/world/catalog" && path != "/v1/world/clock" && path != "/v1/warehouse/list" &&
		path != "/v1/warehouse/inventory" && path != "/v1/warehouse/set-lease" &&
		!(request.URL.Query().Get("port_id") == "amsterdam" && (path == "/v1/world/port" || path == "/v1/market/book")) {
		p.downstream.ServeHTTP(w, request)
		return
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	if strings.HasPrefix(path, "/v1/future/") && request.Method != http.MethodGet {
		p.mutations++
	}
	switch path {
	case "/v1/world/catalog":
		p.h.api.mu.Lock()
		doc := p.h.api.catalogDocument()
		p.h.api.mu.Unlock()
		doc["commodities"] = []any{map[string]any{"id": "water_beer", "base_price_pence": 1500}, map[string]any{"id": "grain", "base_price_pence": 8000}}
		doc["ports"] = append(doc["ports"].([]any), map[string]any{"id": "amsterdam", "banking_region": "north_sea", "sea_zone_id": "zone-north", "phase": 1,
			"is_hub": true, "customs_bps": 0, "total_warehouse_cap": 1000, "imports": []string{"water_beer"}})
		p.h.api.writeJSON(w, http.StatusOK, doc)
	case "/v1/world/clock":
		p.h.api.mu.Lock()
		doc := p.h.api.clockDocument()
		p.h.api.mu.Unlock()
		doc["real_utc"] = p.now
		doc["calendar"].(map[string]any)["time_scale"] = 60
		p.h.api.writeJSON(w, http.StatusOK, doc)
	case "/v1/warehouse/list":
		p.h.api.writeJSON(w, http.StatusOK, []any{p.warehouse()})
	case "/v1/warehouse/inventory":
		p.h.api.writeJSON(w, http.StatusOK, p.warehouse()["inventory"])
	case "/v1/warehouse/set-lease":
		var body struct {
			Port   string `json:"port_id"`
			Target int64  `json:"target_leased_tonnage"`
		}
		if err := json.NewDecoder(request.Body).Decode(&body); err != nil {
			p.h.t.Error(err)
		}
		if body.Port != "amsterdam" || body.Target < p.quantity || request.Header.Get("Idempotency-Key") == "" {
			p.h.t.Errorf("unsafe warehouse lease request: %+v", body)
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		p.leased = body.Target
		p.h.api.writeJSON(w, http.StatusOK, p.warehouse())
	case "/v1/world/port":
		p.h.api.mu.Lock()
		doc := p.h.api.portDocument("amsterdam")
		p.h.api.mu.Unlock()
		doc["banking_region"], doc["customs_bps"], doc["is_hub"] = "north_sea", 0, true
		doc["total_warehouse_cap"], doc["total_leased_warehouse"] = int64(1000), p.leased
		p.h.api.writeJSON(w, http.StatusOK, doc)
	case "/v1/market/book":
		p.h.api.writeJSON(w, http.StatusOK, map[string]any{"port_id": "amsterdam", "as_of": p.now, "visibility": "quay", "customs_bps": 0,
			"books": []any{map[string]any{"commodity_id": "water_beer", "depth": map[string]any{"npc_current_stock": 100,
				"bids": []any{map[string]any{"order_id": "npc-bid", "price_pence": 1000, "remaining_quantity": 100, "is_npc": true}},
				"asks": []any{map[string]any{"order_id": "npc-ask", "price_pence": 2000, "remaining_quantity": 100, "is_npc": true}}}},
				map[string]any{"commodity_id": "provisions", "depth": map[string]any{"npc_current_stock": 100,
					"bids": []any{map[string]any{"order_id": "stores-bid", "price_pence": 500, "remaining_quantity": 100, "is_npc": true}},
					"asks": []any{map[string]any{"order_id": "stores-ask", "price_pence": 1000, "remaining_quantity": 100, "is_npc": true}}}}}})
	case "/v1/future/list":
		rows := make([]map[string]any, 0, len(p.rows))
		if !p.hide {
			for _, row := range p.rows {
				rows = append(rows, row)
			}
		}
		p.h.api.writeJSON(w, http.StatusOK, rows)
	case "/v1/future/create", "/v1/future/accept", "/v1/future/accept-batch":
		body, err := io.ReadAll(request.Body)
		if err != nil {
			p.h.t.Error(err)
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		key := request.Header.Get("Idempotency-Key")
		pending, err := p.h.db.PendingActions(p.h.ctx, p.h.account.ID)
		if err != nil {
			p.h.t.Error(err)
		}
		journaled := false
		for _, action := range pending {
			journaled = journaled || action.ID == key && action.Path == path && bytes.Equal(action.Body, body)
		}
		if request.Method != http.MethodPost || key == "" || !journaled {
			p.h.t.Error("futures mutation reached network before its exact body and key were durable")
		}
		if path == "/v1/future/create" {
			var order map[string]any
			if err := json.Unmarshal(body, &order); err != nil {
				p.h.t.Error(err)
				return
			}
			side, _ := order["side"].(string)
			strike, _ := order["strike_price_pence"].(float64)
			q, _ := order["lot_tons"].(float64)
			hours, _ := order["matures_in_game_hours"].(float64)
			p.assertReservedBeforeSend(key, 1)
			row, exists := p.rows[key]
			if !exists {
				row = map[string]any{"id": uuid.NewString(), "creator_company_id": recoveryCompanyID, "port_id": order["port_id"], "commodity_id": order["commodity_id"],
					"side": side, "lot_tons": int64(q), "strike_price_pence": int64(strike), "buyer_company_id": nil, "seller_company_id": nil,
					"buyer_margin_pence": 0, "seller_margin_pence": 0, "status": "open", "created_at": p.now, "updated_at": p.now,
					"matures_at": p.now.Add(time.Duration(hours * float64(time.Minute)))}
				row[futuresMoneyParty(side)], row[futuresMoneyMargin(side)] = recoveryCompanyID, int64(strike*q)/4
				p.rows[key] = row
				p.creates = append(p.creates, order)
			}
			p.h.api.writeJSON(w, http.StatusCreated, row)
			return
		}
		var ids []string
		if path == "/v1/future/accept-batch" {
			var batch struct {
				IDs []string `json:"futures_ids"`
			}
			if err := json.Unmarshal(body, &batch); err != nil {
				p.h.t.Error(err)
			}
			ids = batch.IDs
		} else {
			var single struct {
				ID string `json:"futures_id"`
			}
			if err := json.Unmarshal(body, &single); err != nil {
				p.h.t.Error(err)
			}
			ids = []string{single.ID}
		}
		p.assertReservedBeforeSend(key, len(ids))
		p.acceptKeys = append(p.acceptKeys, key)
		p.accepts++
		results := make([]map[string]any, 0, len(ids))
		for _, id := range ids {
			row := p.rows[id]
			if row == nil {
				p.h.t.Errorf("accept named unknown listing %s", id)
				return
			}
			ownSide := "bid"
			if row["side"] == "bid" {
				ownSide = "ask"
			}
			row[futuresMoneyParty(ownSide)] = recoveryCompanyID
			strike := row["strike_price_pence"].(int64)
			q := row["lot_tons"].(int32)
			row[futuresMoneyMargin(ownSide)] = strike * int64(q) / 4
			row["status"], row["updated_at"] = "active", p.now
			results = append(results, row)
		}
		if path == "/v1/future/accept-batch" {
			p.h.api.writeJSON(w, http.StatusOK, results)
		} else {
			p.h.api.writeJSON(w, http.StatusOK, results[0])
		}
	case "/v1/future/settle":
		var body struct {
			ID string `json:"futures_id"`
		}
		if err := json.NewDecoder(request.Body).Decode(&body); err != nil {
			p.h.t.Error(err)
		}
		p.settles = append(p.settles, body.ID)
		tons := int32(1)
		if row := p.rows[body.ID]; row != nil {
			tons = row["lot_tons"].(int32)
			if row["status"] != "settled" {
				if row["side"] == "ask" {
					if p.leased-p.quantity < int64(tons) {
						p.h.t.Error("purchase settlement lacks leased receiving space")
						w.WriteHeader(http.StatusBadRequest)
						return
					}
					p.quantity += int64(tons)
				} else {
					if p.quantity-p.locked < int64(tons) {
						p.h.t.Error("sale settlement lacks delivered stock")
						w.WriteHeader(http.StatusBadRequest)
						return
					}
					p.quantity -= int64(tons)
				}
				row["status"] = "settled"
			}
		}
		p.h.api.writeJSON(w, http.StatusOK, gameapi.SettleFuturesResult{FuturesID: body.ID, Status: "settled", SettledTons: tons})
	default:
		w.WriteHeader(http.StatusMethodNotAllowed)
	}
}

func futuresMoneyParty(side string) string {
	if side == "bid" {
		return "buyer_company_id"
	}
	return "seller_company_id"
}

func futuresMoneyMargin(side string) string {
	if side == "bid" {
		return "buyer_margin_pence"
	}
	return "seller_margin_pence"
}

func (p *futuresMoneyAPI) assertReservedBeforeSend(key string, count int) {
	admin, err := sql.Open("pgx", p.h.dsn)
	if err != nil {
		p.h.t.Error(err)
		return
	}
	defer admin.Close()
	rows, err := admin.QueryContext(p.h.ctx, "SELECT side, lot_tons, strike_price_pence, reserved_tons, reserved_cash_pence, margin_pence, exposure_pence FROM futures_intents WHERE account_id = $1 AND action_id = $2", p.h.account.ID, key)
	if err != nil {
		p.h.t.Error(err)
		return
	}
	defer rows.Close()
	seen := 0
	for rows.Next() {
		var side string
		var q, strike, tons, cash, margin, exposure int64
		if err := rows.Scan(&side, &q, &strike, &tons, &cash, &margin, &exposure); err != nil {
			p.h.t.Error(err)
			return
		}
		seen++
		if exposure <= 0 || exposure > store.FuturesExposureLimitPence || margin != strike*q/4 || side == "ask" && tons != q || side == "bid" && cash < strike*q+margin {
			p.h.t.Errorf("uncovered durable reservation: side=%s q=%d tons=%d cash=%d margin=%d exposure=%d", side, q, tons, cash, margin, exposure)
		}
	}
	if err := rows.Err(); err != nil {
		p.h.t.Error(err)
	}
	if seen != count {
		p.h.t.Errorf("send had %d durable intent legs, want %d", seen, count)
	}
}

func futuresMoneyRunner(t *testing.T, h *recoveryHarness) *Runner {
	t.Helper()
	runner := h.runner(primaryTestKey)
	runner.api.SetObserver(func(method, path string, status int, duration time.Duration, request, response []byte, errText string) {
		runner.observeRequest(h.ctx, method, path, status, duration, request, response, errText)
	})
	return runner
}

func futuresMoneyPasses(t *testing.T, h *recoveryHarness, runner *Runner, n int) {
	t.Helper()
	for range n {
		if _, err := h.tick(runner); err != nil {
			t.Fatal(err)
		}
	}
}

func futuresListingFixture(runner *Runner, id, creator, side string, strike int64, tons int32) gameapi.FuturesContract {
	_, upper, _ := runner.serverBounds()
	upper = upper.UTC().Truncate(time.Microsecond)
	return gameapi.FuturesContract{ID: id, CreatorCompanyID: creator, PortID: "amsterdam", CommodityID: "water_beer", Side: side,
		StrikePricePence: strike, LotTons: tons, Status: "open", CreatedAt: upper.Add(-time.Minute), UpdatedAt: upper, MaturesAt: upper.Add(12 * time.Minute)}
}

func futuresCounterpartyFixture(runner *Runner, company string) {
	key := "futures_credit/" + company
	runner.snapshot[key] = encode(map[string]any{"company_id": company, "futures_claims_open_pence": 0, "loans_defaulted": 0, "futures_claims_total": 0})
	runner.meta[key] = DatasetMeta{Generation: runner.generation, Complete: true, UpdatedAt: time.Now().UTC()}
}

func TestFuturesMoneyCoveredSaleRequiresStockAndExactPositiveEdge(t *testing.T) {
	h, p := newFuturesMoneyAPI(t)
	runner := futuresMoneyRunner(t, h)
	refreshMarqueExecution(t, h, runner)
	futuresCounterpartyFixture(runner, "buyer")
	listing := futuresListingFixture(runner, "profitable-bid", "buyer", "bid", 1187, 1)
	warehouse, _ := runner.futuresWarehouse("amsterdam", "water_beer")
	warehouse.Inventory[0].LockedQuantity = 1
	runner.snapshot["warehouses"] = encode([]futuresWarehouse{warehouse})
	if intent, err := runner.evaluateFuturesListing(h.ctx, listing, nil, 100_000, runner.futuresCover(nil, listing.PortID, listing.CommodityID, nil)); err != nil || intent != nil {
		t.Fatalf("locked stock admitted an uncovered forward sale: %+v %v", intent, err)
	}
	warehouse.Inventory[0].LockedQuantity = 0
	runner.snapshot["warehouses"] = encode([]futuresWarehouse{warehouse})
	intent, err := runner.evaluateFuturesListing(h.ctx, listing, nil, 100_000, runner.futuresCover(nil, listing.PortID, listing.CommodityID, nil))
	if err != nil || intent == nil {
		t.Fatalf("covered sale at the exact edge was refused: %+v %v", intent, err)
	}
	var admission futuresAdmission
	if err := json.Unmarshal(intent.Admission, &admission); err != nil {
		t.Fatal(err)
	}
	if admission.SpotAlternative != 1000 || admission.ExpectedNetPence != 100 || intent.ReservedTons != 1 || intent.Side != "ask" {
		t.Fatalf("sale did not price actual depth, carry and counterparty risk: %+v %+v", intent, admission)
	}
	listing.StrikePricePence--
	if intent, err := runner.evaluateFuturesListing(h.ctx, listing, nil, 100_000, runner.futuresCover(nil, listing.PortID, listing.CommodityID, nil)); err != nil || intent != nil {
		t.Fatalf("sale one penny below its minimum edge was admitted: %+v %v", intent, err)
	}
	if p.mutations != 0 {
		t.Fatal("evaluation mutated the game")
	}
}

func TestFuturesMoneyLonePurchaseDoesNotHoldAnAdmittedShip(t *testing.T) {
	h, _ := newFuturesMoneyAPI(t)
	runner := futuresMoneyRunner(t, h)
	refreshMarqueExecution(t, h, runner)
	futuresCounterpartyFixture(runner, "seller")
	listing := futuresListingFixture(runner, "cheap-ask", "seller", "ask", 500, 1)
	mission := &store.Mission{AccountID: h.account.ID, ShipID: recoveryShipLapras, Generation: "admitted-need", Phase: "buying"}
	data := missionData{Plan: Plan{Accepted: true, Origin: "amsterdam", Destination: "london", Commodity: "water_beer", Quantity: 1, ContractID: "freight-one",
		Economics: Economics{GameDays: 1, NetPencePerGameDay: 100}}}
	if err := runner.saveMission(h.ctx, mission, &data); err != nil {
		t.Fatal(err)
	}
	if intent, err := runner.evaluateFuturesListing(h.ctx, listing, nil, 100_000, 1); err != nil || intent != nil {
		t.Fatalf("standalone purchase admitted a ship handoff: %+v %v", intent, err)
	}
}

func TestFuturesMoneyCrossRequiresEarlierPurchaseAndPositiveNetWithoutStock(t *testing.T) {
	h, p := newFuturesMoneyAPI(t)
	p.quantity = 0
	runner := futuresMoneyRunner(t, h)
	refreshMarqueExecution(t, h, runner)
	futuresCounterpartyFixture(runner, "buyer")
	futuresCounterpartyFixture(runner, "seller")
	sale := futuresListingFixture(runner, "sale", "buyer", "bid", 2000, 2)
	purchase := futuresListingFixture(runner, "purchase", "seller", "ask", 500, 2)
	for _, gap := range []time.Duration{-time.Minute, 0, time.Minute - time.Nanosecond, time.Minute, 2 * time.Minute} {
		t.Run(gap.String(), func(t *testing.T) {
			sale.MaturesAt = purchase.MaturesAt.Add(gap)
			pair, err := runner.evaluateFuturesCross(h.ctx, sale, purchase, nil, 100_000)
			if err != nil || (len(pair) == 2) != (gap >= time.Minute) {
				t.Fatalf("purchase-before-sale admission: gap=%s pair=%+v err=%v", gap, pair, err)
			}
			if len(pair) == 0 {
				return
			}
			var admission futuresAdmission
			if err := json.Unmarshal(pair[1].Admission, &admission); err != nil {
				t.Fatal(err)
			}
			days := sale.MaturesAt.Sub(pair[0].CreatedAt).Hours() * 60 / 24
			rent, carry := futuresCarry(5000, 2, days)
			wantNet := int64(4000-1000-200-50) - rent - carry
			if admission.ExpectedNetPence != wantNet || admission.ExpectedNetPence <= 0 ||
				admission.CreditLossPence != 250 || admission.RentPence != rent || admission.FundingPence != carry ||
				admission.Lease == nil || admission.Lease.ReceivingTons != 2 || pair[0].ReservedTons != 2 ||
				pair[0].ReservedCashPence != 1000 || pair[1].ReservedCashPence != 1250+rent {
				t.Fatalf("cross omitted credit, margins, rent or physical delivery: %+v %+v", pair, admission)
			}
			stage, path, body := futuresAdmissionStage(pair)
			if stage != "futures-accept-batch" || path != "/v1/future/accept-batch" ||
				!jsonEqual(encode(body), encode(map[string]any{"futures_ids": []string{sale.ID, purchase.ID}})) {
				t.Fatalf("cross split its atomic acceptance: %s %s %s", stage, path, encode(body))
			}
		})
	}
	sale.MaturesAt = purchase.MaturesAt.Add(time.Minute)
	for _, strike := range []int64{501, 600} {
		sale.StrikePricePence = strike
		if pair, err := runner.evaluateFuturesCross(h.ctx, sale, purchase, nil, 100_000); err != nil || len(pair) != 0 {
			t.Fatalf("spread that loses money after credit and carry was admitted: %+v %v", pair, err)
		}
	}
	sale.StrikePricePence = 2000
	rows := []store.FuturesIntent{{Status: "active", CounterpartyID: "buyer", ExposurePence: 24_001}}
	if pair, err := runner.evaluateFuturesCross(h.ctx, sale, purchase, rows, 100_000); err != nil || len(pair) != 0 {
		t.Fatalf("cross bypassed existing counterparty cap: %+v %v", pair, err)
	}
	if pair, err := runner.evaluateFuturesCross(h.ctx, sale, purchase, nil, 2000); err != nil || len(pair) != 0 {
		t.Fatalf("cross bypassed regional book budget: %+v %v", pair, err)
	}
	purchase.CreatorCompanyID = sale.CreatorCompanyID
	if pair, err := runner.evaluateFuturesCross(h.ctx, sale, purchase, nil, 100_000); err != nil || len(pair) != 0 {
		t.Fatalf("cross reused one counterparty: %+v %v", pair, err)
	}
	if p.mutations != 0 {
		t.Fatal("evaluation mutated the game")
	}
}

func futuresStagedCross(t *testing.T) (*recoveryHarness, *futuresMoneyAPI, *Runner, *store.Mission, missionData) {
	t.Helper()
	h, p := newFuturesMoneyAPI(t)
	p.quantity = 0
	runner := futuresMoneyRunner(t, h)
	refreshMarqueExecution(t, h, runner)
	futuresCounterpartyFixture(runner, "buyer")
	futuresCounterpartyFixture(runner, "seller")
	sale := futuresListingFixture(runner, "sale", "buyer", "bid", 2000, 2)
	purchase := futuresListingFixture(runner, "purchase", "seller", "ask", 500, 2)
	sale.MaturesAt = purchase.MaturesAt.Add(time.Minute)
	for _, listing := range []gameapi.FuturesContract{sale, purchase} {
		p.rows[listing.ID] = map[string]any{"id": listing.ID, "creator_company_id": listing.CreatorCompanyID, "port_id": listing.PortID, "commodity_id": listing.CommodityID,
			"side": listing.Side, "lot_tons": listing.LotTons, "strike_price_pence": listing.StrikePricePence, "status": "open", "created_at": listing.CreatedAt,
			"updated_at": listing.UpdatedAt, "matures_at": listing.MaturesAt, "seller_margin_pence": int64(0), "buyer_margin_pence": int64(0)}
		p.rows[listing.ID][futuresMoneyParty(listing.Side)] = listing.CreatorCompanyID
	}
	runner.snapshot["futures"] = encode([]gameapi.FuturesContract{sale, purchase})
	pair, err := runner.evaluateFuturesCross(h.ctx, sale, purchase, nil, 100_000)
	if err != nil || len(pair) != 2 {
		t.Fatalf("fixture pair was not admissible: %+v %v", pair, err)
	}
	mission := &store.Mission{AccountID: h.account.ID, ShipID: "__futures", Generation: "atomic-cross", Phase: "evaluating"}
	data := missionData{Futures: &futuresState{}}
	if changed, err := runner.stageFuturesAdmission(h.ctx, mission, &data, pair); err != nil || !changed || data.Action == nil {
		t.Fatalf("pair was not durably staged: %t %v %+v", changed, err, data.Action)
	}
	return h, p, runner, mission, data
}

func TestFuturesMoneyAtomicAcceptanceRestartRetainsJournalKey(t *testing.T) {
	h, p, runner, mission, data := futuresStagedCross(t)
	action := *data.Action
	if _, err := h.db.BeginAction(h.ctx, &store.Action{ID: action.ID, AccountID: h.account.ID, Method: action.Method, Path: action.Path, Body: action.Body}); err != nil {
		t.Fatal(err)
	}
	request := httptest.NewRequest(action.Method, action.Path, bytes.NewReader(action.Body))
	request.Header.Set("Idempotency-Key", action.ID)
	capture := httptest.NewRecorder()
	p.ServeHTTP(capture, request)
	if capture.Code != http.StatusOK {
		t.Fatalf("atomic accept failed: %d %s", capture.Code, capture.Body.String())
	}
	// The game committed, but the process died before recording its receipt.
	runner = futuresMoneyRunner(t, h)
	meta := runner.meta["futures"]
	meta.Stale = true
	runner.meta["futures"] = meta
	refreshMarqueExecution(t, h, runner)
	mission, data = h.mission("__futures")
	if data.Action == nil || data.Action.ID != action.ID || !bytes.Equal(data.Action.Body, action.Body) {
		t.Fatal("restart replaced the ambiguous acceptance request")
	}
	if _, err := runner.dispatch(h.ctx, mission, &data); err != nil {
		t.Fatal(err)
	}
	if data.Action != nil {
		t.Fatalf("authoritative accepted contracts did not resolve the ambiguous pair: %+v", data.Action)
	}
	rows, err := h.db.FuturesIntents(h.ctx, h.account.ID)
	if err != nil || len(rows) != 2 {
		t.Fatalf("accepted pair ledger = %+v, error = %v", rows, err)
	}
	for _, row := range rows {
		if row.ActionID != action.ID || row.Status != "active" || row.FuturesID != row.ListingID {
			t.Fatalf("restart split or readmitted an accepted pair: %+v", row)
		}
	}
	for _, key := range p.acceptKeys {
		if key != action.ID {
			t.Fatalf("acceptance retried under a new key: %s", key)
		}
	}
	if p.accepts != 1 {
		t.Fatalf("observed committed pair was resent %d times", p.accepts)
	}
}

func TestFuturesMoneyCrossDeliversPurchaseBeforeSaleAfterDelayedPass(t *testing.T) {
	h, p, runner, mission, data := futuresStagedCross(t)
	data.Futures.LegacyClosed = true
	if changed, err := runner.dispatch(h.ctx, mission, &data); err != nil || !changed || data.Action != nil || p.accepts != 1 {
		t.Fatalf("empty-warehouse atomic cross failed first-send preflight: %t %v %+v", changed, err, data.Action)
	}
	// Both maturities have passed before the controller resumes.
	p.now = p.now.Add(14 * time.Minute)
	for i, expected := range []struct {
		id    string
		stock int64
	}{{"purchase", 2}, {"sale", 0}} {
		refreshMarqueExecution(t, h, runner)
		if changed, err := runner.driveFutures(h.ctx, mission, &data); err != nil || !changed || data.Action == nil ||
			data.Action.Stage != "futures-settle" || !jsonEqual(data.Action.Body, encode(map[string]string{"futures_id": expected.id})) {
			t.Fatalf("mature cross settled out of physical order at leg %d: %t %v %+v", i, changed, err, data.Action)
		}
		if changed, err := runner.dispatch(h.ctx, mission, &data); err != nil || !changed || data.Action != nil || p.quantity != expected.stock {
			t.Fatalf("cross delivery failed at leg %d: changed=%t stock=%d err=%v action=%+v", i, changed, p.quantity, err, data.Action)
		}
	}
	rows, err := h.db.FuturesIntents(h.ctx, h.account.ID)
	if err != nil || len(rows) != 2 {
		t.Fatalf("cross lost its durable legs: %+v %v", rows, err)
	}
	if remaining, known := futuresRemainingStock(rows, rows[0].CohortID, data.Futures); !known || remaining != 0 {
		t.Fatalf("purchase failed to cover exactly its sale: remaining=%d known=%t", remaining, known)
	}
}
