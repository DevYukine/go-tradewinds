package engine

import (
	"database/sql"
	"encoding/base64"
	"encoding/json"
	"io"
	"net/http"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/DevYukine/go-tradewinds/internal/config"
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
	cash       int64
	rows       map[string]map[string]any
	creates    []map[string]any
	settles    []string
	accepts    int
	mutations  int
	hide       bool
}

func newFuturesMoneyAPI(t *testing.T) (*recoveryHarness, *futuresMoneyAPI) {
	t.Helper()
	h := newRecoveryHarness(t)
	p := &futuresMoneyAPI{h: h, downstream: h.api.server.Config.Handler, now: time.Now().UTC().Add(-2 * time.Minute), quantity: 1,
		cash: 1_000_000, rows: make(map[string]map[string]any)}
	h.api.setFleet([]map[string]any{})
	h.api.mu.Lock()
	h.api.factorPorts = []string{"amsterdam"}
	h.api.vaultBalance["north_sea"] = p.cash
	h.api.mu.Unlock()
	h.api.server.Config.Handler = p
	return h, p
}

func (p *futuresMoneyAPI) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	path := r.URL.Path
	if !strings.HasPrefix(path, "/v1/future/") && path != "/v1/world/catalog" && path != "/v1/world/clock" && path != "/v1/warehouse/list" &&
		path != "/v1/warehouse/inventory" && !(r.URL.Query().Get("port_id") == "amsterdam" && (path == "/v1/world/port" || path == "/v1/market/book")) {
		p.downstream.ServeHTTP(w, r)
		return
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	if strings.HasPrefix(path, "/v1/future/") && r.Method != http.MethodGet {
		p.mutations++
	}
	switch path {
	case "/v1/world/catalog":
		p.h.api.mu.Lock()
		doc := p.h.api.catalogDocument()
		p.h.api.mu.Unlock()
		doc["commodities"] = []any{map[string]any{"id": "water_beer", "base_price_pence": 2500}, map[string]any{"id": "grain", "base_price_pence": 8000}}
		doc["ports"] = append(doc["ports"].([]any), map[string]any{"id": "amsterdam", "banking_region": "north_sea", "sea_zone_id": "zone-north", "phase": 1,
			"is_hub": true, "customs_bps": 268, "total_warehouse_cap": 1000, "imports": []string{"water_beer"}})
		p.h.api.writeJSON(w, http.StatusOK, doc)
	case "/v1/world/clock":
		p.h.api.mu.Lock()
		doc := p.h.api.clockDocument()
		p.h.api.mu.Unlock()
		doc["real_utc"] = p.now
		doc["calendar"].(map[string]any)["time_scale"] = 60
		p.h.api.writeJSON(w, http.StatusOK, doc)
	case "/v1/warehouse/list", "/v1/warehouse/inventory":
		inventory := []any{map[string]any{"company_id": recoveryCompanyID, "port_id": "amsterdam", "commodity_id": "water_beer", "quantity": p.quantity,
			"locked_quantity": p.locked, "created_at": p.now.Add(-time.Hour), "updated_at": p.now}}
		doc := map[string]any{"port_id": "amsterdam", "has_factor": true, "leased_tonnage": 2, "reserved_tonnage": 0,
			"used_tonnage": p.quantity, "free_tonnage": 2 - p.quantity, "inventory": inventory}
		if path == "/v1/warehouse/list" {
			p.h.api.writeJSON(w, http.StatusOK, []any{doc})
		} else {
			p.h.api.writeJSON(w, http.StatusOK, inventory)
		}
	case "/v1/world/port":
		p.h.api.mu.Lock()
		doc := p.h.api.portDocument("amsterdam")
		p.h.api.mu.Unlock()
		doc["banking_region"], doc["customs_bps"], doc["is_hub"] = "north_sea", 268, true
		p.h.api.writeJSON(w, http.StatusOK, doc)
	case "/v1/market/book":
		// Player prices are deliberately far from the NPC midpoint of 2,000p.
		p.h.api.writeJSON(w, http.StatusOK, map[string]any{"port_id": "amsterdam", "as_of": p.now, "visibility": "quay", "customs_bps": 268,
			"books": []any{map[string]any{"commodity_id": "water_beer", "depth": map[string]any{"npc_current_stock": 100,
				"bids": []any{map[string]any{"order_id": "player-bid", "price_pence": 9000, "remaining_quantity": 10, "is_npc": false}, map[string]any{"order_id": "npc-bid", "price_pence": 1900, "remaining_quantity": 10, "is_npc": true}},
				"asks": []any{map[string]any{"order_id": "player-ask", "price_pence": 100, "remaining_quantity": 10, "is_npc": false}, map[string]any{"order_id": "npc-ask", "price_pence": 2100, "remaining_quantity": 10, "is_npc": true}}}}}})
	case "/v1/future/list":
		rows := make([]map[string]any, 0, len(p.rows))
		if !p.hide {
			for _, row := range p.rows {
				rows = append(rows, row)
			}
		}
		p.h.api.writeJSON(w, http.StatusOK, rows)
	case "/v1/future/create", "/v1/future/create-batch":
		body, err := io.ReadAll(r.Body)
		if err != nil {
			p.h.t.Error(err)
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		var orders []map[string]any
		if path == "/v1/future/create-batch" {
			var batch struct {
				Orders []map[string]any `json:"orders"`
			}
			if err := json.Unmarshal(body, &batch); err != nil {
				p.h.t.Error(err)
				w.WriteHeader(http.StatusBadRequest)
				return
			}
			orders = batch.Orders
		} else {
			var order map[string]any
			if err := json.Unmarshal(body, &order); err != nil {
				p.h.t.Error(err)
				w.WriteHeader(http.StatusBadRequest)
				return
			}
			orders = []map[string]any{order}
		}
		key := r.Header.Get("Idempotency-Key")
		pending, err := p.h.db.PendingActions(p.h.ctx, p.h.account.ID)
		if err != nil {
			p.h.t.Error(err)
		}
		journaled := false
		for _, action := range pending {
			journaled = journaled || action.ID == key && action.Path == path && string(action.Body) == string(body)
		}
		if key == "" || !journaled {
			p.h.t.Error("futures send reached network before its exact journal body/key were durable")
		}
		var results []map[string]any
		for _, order := range orders {
			side, _ := order["side"].(string)
			strike, _ := order["strike_price_pence"].(float64)
			if r.Method != http.MethodPost || order["port_id"] != "amsterdam" || order["commodity_id"] != "water_beer" || order["lot_tons"] != float64(1) || order["matures_in_game_hours"] != float64(30) || side != "bid" && side != "ask" {
				p.h.t.Errorf("pilot escaped owner terms: %s %s %+v", r.Method, path, order)
				w.WriteHeader(http.StatusBadRequest)
				return
			}
			if side == "ask" && strike != 1700 || side == "bid" && strike != 2300 {
				p.h.t.Errorf("pilot did not quote 0.85/1.15 of NPC midpoint: %+v", order)
			}
			p.assertReservedBeforeSend(key, side, int64(strike))
			identity := key + "/" + side
			row, exists := p.rows[identity]
			if !exists {
				row = map[string]any{"id": uuid.NewString(), "creator_company_id": recoveryCompanyID, "port_id": "amsterdam", "commodity_id": "water_beer",
					"side": side, "lot_tons": 1, "strike_price_pence": int64(strike), "buyer_company_id": nil, "seller_company_id": nil,
					"buyer_margin_pence": 0, "seller_margin_pence": 0, "status": "open", "created_at": p.now, "updated_at": p.now, "matures_at": p.now.Add(30 * time.Minute)}
				row[futuresMoneyParty(side)], row[futuresMoneyMargin(side)] = recoveryCompanyID, int64(strike)/4
				p.rows[identity] = row
				p.creates = append(p.creates, order)
				p.h.api.mu.Lock()
				p.h.api.vaultBalance["north_sea"] -= int64(strike) / 4
				p.h.api.mu.Unlock()
			}
			results = append(results, row)
		}
		if path == "/v1/future/create-batch" {
			p.h.api.writeJSON(w, http.StatusCreated, results)
		} else {
			p.h.api.writeJSON(w, http.StatusCreated, results[0])
		}
	case "/v1/future/settle":
		var request struct {
			ID string `json:"futures_id"`
		}
		if err := json.NewDecoder(r.Body).Decode(&request); err != nil {
			p.h.t.Error(err)
		}
		p.settles = append(p.settles, request.ID)
		for _, row := range p.rows {
			if row["id"] == request.ID {
				if p.now.Before(row["matures_at"].(time.Time)) {
					p.h.t.Error("settle sent before authoritative server maturity")
				}
				row["status"], row["updated_at"] = "settled", p.now
			}
		}
		p.h.api.writeJSON(w, http.StatusOK, map[string]any{"futures_id": request.ID, "status": "settled", "settled_tons": 1, "penalty_paid_pence": 0, "claim_pence": 0})
	default:
		if strings.Contains(path, "accept") {
			p.accepts++
			p.h.t.Error("pilot called futures accept")
		}
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

func (p *futuresMoneyAPI) assertReservedBeforeSend(key, side string, strike int64) {
	admin, err := sql.Open("pgx", p.h.dsn)
	if err != nil {
		p.h.t.Error(err)
		return
	}
	defer admin.Close()
	var raw []byte
	err = admin.QueryRowContext(p.h.ctx, "SELECT to_jsonb(fi)::text FROM futures_intents fi WHERE account_id = $1 AND action_id = $2 AND side = $3", p.h.account.ID, key, side).Scan(&raw)
	if err != nil {
		p.h.t.Errorf("futures intent/reservations not durable before send: %v", err)
		return
	}
	var row struct {
		ReservedTons int64 `json:"reserved_tons"`
		ReservedCash int64 `json:"reserved_cash_pence"`
		Exposure     int64 `json:"exposure_pence"`
	}
	if err := json.Unmarshal(raw, &row); err != nil {
		p.h.t.Error(err)
		return
	}
	if row.Exposure <= 0 || row.Exposure > 30_000 || side == "ask" && row.ReservedTons < 1 || side == "bid" && row.ReservedCash < strike+(strike*268+9999)/10000+strike/4 {
		p.h.t.Errorf("uncovered pilot sent: side=%s strike=%d ledger=%s", side, strike, raw)
	}
}

func futuresMoneyRunner(t *testing.T, h *recoveryHarness, enabled bool) *Runner {
	t.Helper()
	base := h.runner(primaryTestKey)
	opts := base.opts
	field := reflect.ValueOf(&opts).Elem().FieldByName("FuturesPilotEnabled")
	if !field.IsValid() || field.Kind() != reflect.Bool {
		t.Fatal("Options.FuturesPilotEnabled must carry FUTURES_PILOT_ENABLED to the runner")
	}
	field.SetBool(enabled)
	runner := New(base.api, h.db, h.account, base.log, opts)
	runner.snapshot, runner.meta = base.snapshot, base.meta
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

func TestFuturesMoneyPilotGateAndCoveredOwnerTerms(t *testing.T) {
	for _, enabled := range []bool{false, true} {
		t.Run(map[bool]string{false: "disabled", true: "enabled"}[enabled], func(t *testing.T) {
			h, p := newFuturesMoneyAPI(t)
			runner := futuresMoneyRunner(t, h, enabled)
			futuresMoneyPasses(t, h, runner, 12)
			p.mu.Lock()
			defer p.mu.Unlock()
			if !enabled && p.mutations != 0 {
				t.Fatalf("disabled pilot sent %d futures mutations", p.mutations)
			}
			if enabled && len(p.creates) != 2 {
				t.Fatalf("covered pilot did not create exactly one bid and ask: %+v holds=%+v", p.creates, runner.holding)
			}
			if enabled {
				sides := map[string]int{}
				for _, order := range p.creates {
					sides[order["side"].(string)]++
				}
				if sides["ask"] != 1 || sides["bid"] != 1 {
					t.Fatalf("pilot side limits violated: %v", sides)
				}
				history, err := h.db.History(h.ctx, h.account.ID, "decisions", 500)
				if err != nil {
					t.Fatal(err)
				}
				for _, decision := range history.([]store.Decision) {
					if decision.Kind == "excluded_liabilities" && strings.Contains(strings.ToLower(decision.Reason), "futures, patrols and extra companies rejected") {
						t.Fatal("owner sees a blanket futures rejection while the covered pilot is enabled")
					}
				}
			}
			if p.accepts != 0 {
				t.Fatalf("pilot accepted %d external lots", p.accepts)
			}
		})
	}
}

func TestFuturesMoneyAskRequiresUnlockedAmsterdamWarehouseTon(t *testing.T) {
	for _, tc := range []struct {
		name         string
		tons, locked int64
		wantAsk      bool
	}{{"covered", 1, 0, true}, {"missing", 0, 0, false}, {"already_locked", 1, 1, false}} {
		t.Run(tc.name, func(t *testing.T) {
			h, p := newFuturesMoneyAPI(t)
			p.quantity, p.locked = tc.tons, tc.locked
			futuresMoneyPasses(t, h, futuresMoneyRunner(t, h, true), 12)
			p.mu.Lock()
			defer p.mu.Unlock()
			asks := 0
			for _, order := range p.creates {
				if order["side"] == "ask" {
					asks++
				}
			}
			if (asks == 1) != tc.wantAsk {
				t.Fatalf("warehouse tons=%d locked=%d sent asks=%d", tc.tons, tc.locked, asks)
			}
		})
	}
}

func TestFuturesMoneyBidReservesStrikeCustomsAndMargin(t *testing.T) {
	// mid=2,000p => ask=1,700p (425 margin), bid=2,300p
	// (575 margin + ceil(2,300*268/10,000)=62 customs): pair needs 3,362p.
	// At 2,936p even a bid-first implementation is one penny short.
	for _, free := range []int64{3362, 2936} {
		t.Run(map[int64]string{3362: "fully_covered_pair", 2936: "bid_one_penny_short"}[free], func(t *testing.T) {
			h, p := newFuturesMoneyAPI(t)
			runner := futuresMoneyRunner(t, h, true)
			refreshMarqueExecution(t, h, runner)
			input, err := runner.committedPlanningInput(h.ctx, ShipState{})
			if err != nil {
				t.Fatal(err)
			}
			h.api.setVaultBalance("north_sea", input.ReserveByRegion["north_sea"]+free)
			futuresMoneyPasses(t, h, runner, 12)
			p.mu.Lock()
			defer p.mu.Unlock()
			bids := 0
			for _, order := range p.creates {
				if order["side"] == "bid" {
					bids++
				}
			}
			if free == 3362 && bids != 1 {
				t.Fatalf("fully covered bid was refused: creates=%v holds=%v", p.creates, runner.holding)
			}
			if free == 2936 && bids != 0 {
				t.Fatal("bid sent with less free cash than strike plus customs plus margin")
			}
		})
	}
}

func TestFuturesMoneyServerMaturityVanishedListingsAndNoScaleUp(t *testing.T) {
	h, p := newFuturesMoneyAPI(t)
	// At 60 game-hours per real hour this cohort matures 30 real minutes
	// after server time, while the host is already well beyond maturity.
	p.now = time.Now().UTC().Add(-2 * time.Hour)
	runner := futuresMoneyRunner(t, h, true)
	futuresMoneyPasses(t, h, runner, 12)
	p.mu.Lock()
	if len(p.rows) != 2 {
		p.mu.Unlock()
		t.Fatal("pilot cohort was not created")
	}
	var maturity time.Time
	for _, row := range p.rows {
		row["status"] = "active"
		strike := row["strike_price_pence"].(int64)
		if row["side"] == "ask" {
			row["buyer_company_id"], row["buyer_margin_pence"] = "22222222-2222-4222-8222-222222222222", strike/4
		} else {
			row["seller_company_id"], row["seller_margin_pence"] = "22222222-2222-4222-8222-222222222222", strike/4
		}
		maturity = row["matures_at"].(time.Time)
	}
	// Host is beyond maturity, but server lower bound still is not. Do not
	// use host time or the upper server bound to authorize settlement.
	p.mu.Unlock()
	runner = futuresMoneyRunner(t, h, true)
	futuresMoneyPasses(t, h, runner, 4)
	p.mu.Lock()
	if len(p.settles) != 0 {
		p.mu.Unlock()
		t.Fatal("host-clock maturity authorized early settle")
	}
	p.now = maturity.Add(time.Hour)
	p.mu.Unlock()
	futuresMoneyPasses(t, h, runner, 8)
	p.mu.Lock()
	settles := len(p.settles)
	mutations := p.mutations
	p.hide = true
	p.mu.Unlock()
	if settles != 2 {
		t.Fatalf("mature covered cohort was not settled once per contract: %d", settles)
	}
	runner = futuresMoneyRunner(t, h, true)
	futuresMoneyPasses(t, h, runner, 12)
	p.mu.Lock()
	defer p.mu.Unlock()
	if len(p.creates) != 2 || len(p.settles) != 2 || p.mutations != mutations {
		t.Fatalf("vanished terminal listings were recreated/resettled or pilot scaled up: creates=%d settles=%d mutations=%d (before=%d)", len(p.creates), len(p.settles), p.mutations, mutations)
	}
	admin, err := sql.Open("pgx", h.dsn)
	if err != nil {
		t.Fatal(err)
	}
	defer admin.Close()
	var terminal int
	if err := admin.QueryRowContext(h.ctx, "SELECT count(*) FROM futures_intents WHERE account_id = $1 AND status = 'settled'", h.account.ID).Scan(&terminal); err != nil || terminal != 2 {
		t.Fatalf("vanished listings lost permanent outcomes: count=%d err=%v", terminal, err)
	}
}

func TestFuturesMoneyVanishedUnsettledListingResolvesFromPrivateEvent(t *testing.T) {
	h, p := newFuturesMoneyAPI(t)
	p.now = time.Now().UTC().Add(-2 * time.Hour)
	futuresMoneyPasses(t, h, futuresMoneyRunner(t, h, true), 12)
	p.mu.Lock()
	if len(p.rows) != 2 {
		p.mu.Unlock()
		t.Fatal("covered cohort was not created")
	}
	p.hide = true
	mutations := p.mutations
	// Private outcome payload schemas are unpublished. This fixture declares
	// the outcome contract using the documented SettleFuturesResult fields;
	// the company/timestamp envelope follows the published SSE format.
	events := make([]map[string]any, 0, len(p.rows))
	for _, row := range p.rows {
		p.now = row["matures_at"].(time.Time).Add(time.Hour)
		id := row["id"].(string)
		events = append(events, map[string]any{"id": "vanished-" + id, "event_type": "futures_settled", "company_id": recoveryCompanyID,
			"port_id": "amsterdam", "timestamp": p.now, "payload": map[string]any{"futures_id": id, "status": "settled",
				"settled_tons": 1, "penalty_paid_pence": 0, "claim_pence": 0}})
	}
	p.mu.Unlock()
	for _, event := range events {
		if err := h.db.RecordEvent(h.ctx, h.account.ID, event["id"].(string), "futures_settled", encode(event)); err != nil {
			t.Fatal(err)
		}
	}
	futuresMoneyPasses(t, h, futuresMoneyRunner(t, h, true), 12)
	p.mu.Lock()
	creates, settles, after := len(p.creates), len(p.settles), p.mutations
	p.mu.Unlock()
	if creates != 2 || settles != 0 || after != mutations {
		t.Fatalf("vanished terminal contracts were recreated or settled again: creates=%d settles=%d mutations=%d (before=%d)", creates, settles, after, mutations)
	}
	admin, err := sql.Open("pgx", h.dsn)
	if err != nil {
		t.Fatal(err)
	}
	defer admin.Close()
	var terminal int
	if err := admin.QueryRowContext(h.ctx, "SELECT count(*) FROM futures_intents WHERE account_id=$1 AND status='settled'", h.account.ID).Scan(&terminal); err != nil || terminal != 2 {
		t.Fatalf("private events did not resolve vanished listings into permanent outcomes: count=%d err=%v", terminal, err)
	}
}

func TestFuturesMoneyEnvironmentGateDefaultsOff(t *testing.T) {
	for _, tc := range []struct {
		name, value string
		want, bad   bool
	}{{"default", "", false, false}, {"explicit_false", "false", false, false}, {"explicit_true", "true", true, false}, {"invalid", "sometimes", false, true}} {
		t.Run(tc.name, func(t *testing.T) {
			t.Chdir(t.TempDir())
			for key, value := range map[string]string{
				"DATABASE_URL":   "postgres://localhost/futures_config_test",
				"CREDENTIAL_KEY": base64.StdEncoding.EncodeToString([]byte(strings.Repeat("k", 32))),
				"HTTP_ADDR":      "127.0.0.1:8585", "TRADEWINDS_API_URL": "https://tradewinds.kibb.io",
				"RESERVE_PENCE": "100000", "MAX_PAYBACK_GAME_DAYS": "30", "CAPACITY_PAYBACK_GAME_DAYS": "420",
				"MARQUES_ENABLED": "false", "REPLAY_RETENTION_DAYS": "90", "REPLAY_MAX_GIB": "20",
				"FUTURES_PILOT_ENABLED": tc.value,
			} {
				t.Setenv(key, value)
			}
			cfg, err := config.Load()
			if tc.bad {
				if err == nil || !strings.Contains(err.Error(), "FUTURES_PILOT_ENABLED") {
					t.Fatalf("invalid pilot switch was ignored or misclassified: %v", err)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			field := reflect.ValueOf(cfg).FieldByName("FuturesPilotEnabled")
			if !field.IsValid() || field.Kind() != reflect.Bool {
				t.Fatal("Config.FuturesPilotEnabled must decode FUTURES_PILOT_ENABLED")
			}
			if field.Bool() != tc.want {
				t.Fatalf("FUTURES_PILOT_ENABLED=%q became enabled=%t", tc.value, field.Bool())
			}
		})
	}
}
