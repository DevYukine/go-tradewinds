package engine

import (
	"context"
	"database/sql"
	"encoding/json"
	"net/http"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/DevYukine/go-tradewinds/internal/gameapi"
	"go.uber.org/zap"
	"go.uber.org/zap/zaptest/observer"
)

const underworldMoneyKey = "separate-underworld-observer-31751"

type underworldMoneyGetter struct{ client *gameapi.Client }

func (g underworldMoneyGetter) Get(ctx context.Context, path string, out any) error {
	return g.client.Get(ctx, path, out)
}

type underworldMoneyAPI struct {
	mu          sync.Mutex
	h           *recoveryHarness
	downstream  http.Handler
	forbidden   bool
	wants       map[string]int
	books       map[string]int
	nonGET      int
	unsafePaths []string
}

func underworldMoneyShip(id, name, port string, at time.Time) map[string]any {
	row := sailingShipRow(id, name, recoveryHomePort, port)
	ship := row["ship"].(map[string]any)
	ship["status"], ship["current_port_id"], ship["berthed_at"], ship["updated_at"] = "berthed", port, at, at
	row["voyage"], row["route_legs"] = nil, []any{}
	return row
}

func newUnderworldMoneyAPI(t *testing.T) (*recoveryHarness, *underworldMoneyAPI) {
	t.Helper()
	h := newRecoveryHarness(t)
	p := &underworldMoneyAPI{h: h, downstream: h.api.server.Config.Handler, wants: map[string]int{}, books: map[string]int{}}
	at := time.Date(2026, 10, 28, 15, 0, 0, 0, time.UTC)
	h.api.setFleet([]map[string]any{underworldMoneyShip(recoveryShipBlastoise, "Blastoise", recoveryHomePort, at), underworldMoneyShip(recoveryShipLapras, "Lapras", recoveryFarPort, at)})
	h.api.server.Config.Handler = p
	return h, p
}

func (p *underworldMoneyAPI) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		p.mu.Lock()
		p.nonGET++
		p.mu.Unlock()
		p.h.t.Errorf("read-only observer attempted %s %s", r.Method, r.URL.Path)
		w.WriteHeader(http.StatusMethodNotAllowed)
		return
	}
	if r.URL.Path == "/v1/world/catalog" {
		p.h.api.mu.Lock()
		doc := p.h.api.catalogDocument()
		p.h.api.mu.Unlock()
		for _, value := range doc["ports"].([]any) {
			port := value.(map[string]any)
			if port["id"] == recoveryFarPort {
				port["is_cove"] = true
			}
		}
		doc["commodities"] = []any{map[string]any{"id": "illegal_firearms", "category": "contraband", "base_price_pence": 90000}, map[string]any{"id": "untaxed_tea", "category": "contraband", "base_price_pence": 30000}, map[string]any{"id": "counterfeit_papers", "category": "contraband", "base_price_pence": 100000}, map[string]any{"id": "grog", "category": "contraband", "base_price_pence": 5000}}
		p.h.api.writeJSON(w, http.StatusOK, doc)
		return
	}
	if !strings.HasPrefix(r.URL.Path, "/v1/underworld/") {
		p.downstream.ServeHTTP(w, r)
		return
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	if r.URL.Path != "/v1/underworld/wants" && r.URL.Path != "/v1/underworld/book" {
		p.unsafePaths = append(p.unsafePaths, r.URL.Path)
		w.WriteHeader(405)
		return
	}
	port := r.URL.Query().Get("port_id")
	if r.URL.Path == "/v1/underworld/wants" {
		p.wants[port]++
	} else {
		p.books[port+"/"+r.URL.Query().Get("commodity_id")]++
	}
	if r.Header.Get("Authorization") != "Bearer "+underworldMoneyKey {
		p.h.t.Error("observer used the main automation key instead of its separate scope-limited key")
	}
	if p.forbidden {
		p.h.api.writeProblem(w, 403, "missing_scope", "API key lacks required scope: 'underworld'")
		return
	}
	if r.URL.Path == "/v1/underworld/wants" {
		wants, board := []any{}, []any{}
		if port == recoveryHomePort {
			wants = []any{map[string]any{"commodity_id": "untaxed_tea", "multiple_bps": 22500, "remaining_tons": 7}}
		} else {
			board = []any{map[string]any{"port_id": recoveryHomePort, "commodity_id": "untaxed_tea", "multiple_bps": 22500, "observed_at": "2026-10-28T15:00:00Z", "age_game_hours": 1}}
		}
		p.h.api.writeJSON(w, http.StatusOK, map[string]any{"port_id": port, "is_cove": port == recoveryFarPort, "wants": wants, "board": board})
		return
	}
	if port != recoveryFarPort {
		p.h.t.Error("observer fetched a cove book without a ship at a cove")
	}
	good := r.URL.Query().Get("commodity_id")
	p.h.api.writeJSON(w, http.StatusOK, map[string]any{"commodity_id": good, "house_ask": map[string]any{"price_pence": 31000, "quantity": 3}, "bids": []any{}, "asks": []any{}})
}

func underworldMoneyRunner(t *testing.T, h *recoveryHarness, key bool, log *zap.Logger) *Runner {
	t.Helper()
	base := h.runner(primaryTestKey)
	credentials, err := h.db.Credentials(h.ctx, h.account.ID)
	if err != nil {
		t.Fatal(err)
	}
	if key {
		raw, _ := json.Marshal(map[string]string{"handle": credentials.Handle, "passphrase": credentials.Passphrase, "api_key": credentials.APIKey, "underworld_api_key": underworldMoneyKey})
		if err := json.Unmarshal(raw, &credentials); err != nil {
			t.Fatal(err)
		}
		if err := h.db.SaveAccount(h.ctx, h.account, credentials); err != nil {
			t.Fatal(err)
		}
	}
	opts := base.opts
	opts.Execute = false
	field := reflect.ValueOf(&opts).Elem().FieldByName("UnderworldAPI")
	if !field.IsValid() || field.Kind() != reflect.Interface || field.Type().NumMethod() != 1 || field.Type().Method(0).Name != "Get" {
		t.Fatal("Options.UnderworldAPI must expose exactly Get, not Do/order/run/cancel capabilities")
	}
	if key {
		field.Set(reflect.ValueOf(underworldMoneyGetter{gameapi.New(h.api.server.URL, underworldMoneyKey, zap.NewNop())}))
	}
	runner := New(base.api, h.db, h.account, log, opts)
	runner.snapshot, runner.meta = base.snapshot, base.meta
	return runner
}

func underworldMoneyRefresh(t *testing.T, h *recoveryHarness, runner *Runner, n int) {
	t.Helper()
	for range n {
		refreshMarqueExecution(t, h, runner)
	}
}

func TestUnderworldMoneyReadOnlyArrivalBudgetAndPermanentObservations(t *testing.T) {
	h, p := newUnderworldMoneyAPI(t)
	runner := underworldMoneyRunner(t, h, true, zap.NewNop())
	underworldMoneyRefresh(t, h, runner, 4)
	// Restart cannot spend a second visit's request budget for the same arrival.
	runner = underworldMoneyRunner(t, h, true, zap.NewNop())
	underworldMoneyRefresh(t, h, runner, 3)
	p.mu.Lock()
	ordinary, cove := p.wants[recoveryHomePort], p.wants[recoveryFarPort]
	books := 0
	for _, count := range p.books {
		books += count
		if count > 1 {
			t.Errorf("same commodity read %d times in one cove visit", count)
		}
	}
	nonGET, unsafe := p.nonGET, len(p.unsafePaths)
	p.mu.Unlock()
	if ordinary != 1 || cove != 1 || books != 4 {
		t.Fatalf("arrival budget missing/duplicated: wants=%d/%d cove books=%d", ordinary, cove, books)
	}
	if nonGET != 0 || unsafe != 0 {
		t.Fatalf("observer acquired mutation/order capabilities: writes=%d unsafe paths=%d", nonGET, unsafe)
	}
	admin, err := sql.Open("pgx", h.dsn)
	if err != nil {
		t.Fatal(err)
	}
	defer admin.Close()
	var count int
	if err := admin.QueryRowContext(h.ctx, "SELECT count(*) FROM underworld_observations WHERE account_id=$1", h.account.ID).Scan(&count); err != nil || count != 6 {
		t.Fatalf("successful wants/books were not stored permanently: rows=%d err=%v", count, err)
	}
	var payload string
	if err := admin.QueryRowContext(h.ctx, "SELECT payload::text FROM underworld_observations WHERE account_id=$1 AND kind='wants' AND port_id=$2", h.account.ID, recoveryHomePort).Scan(&payload); err != nil || !strings.Contains(payload, "22500") || !strings.Contains(payload, "remaining_tons") {
		t.Fatalf("consumer-visible wants facts lost: %s %v", payload, err)
	}
	// One genuine departure and new berth is a new cove visit, not an updated
	// request timestamp on the same ship snapshot.
	h.api.setFleet([]map[string]any{sailingShipRow(recoveryShipLapras, "Lapras", recoveryHomePort, recoveryFarPort)})
	underworldMoneyRefresh(t, h, runner, 1)
	h.api.setFleet([]map[string]any{underworldMoneyShip(recoveryShipLapras, "Lapras", recoveryFarPort, time.Date(2026, 10, 28, 16, 0, 0, 0, time.UTC))})
	underworldMoneyRefresh(t, h, runner, 3)
	p.mu.Lock()
	defer p.mu.Unlock()
	books = 0
	for _, n := range p.books {
		books += n
	}
	if p.wants[recoveryHomePort] != 1 || p.wants[recoveryFarPort] != 2 || books != 8 {
		t.Fatalf("new arrival did not receive exactly one new observation budget: wants=%v books=%v", p.wants, p.books)
	}
}

func TestUnderworldMoneyMissingKeyOr403DisablesWithOneWarning(t *testing.T) {
	for _, key := range []bool{false, true} {
		t.Run(map[bool]string{false: "missing_key", true: "403_missing_scope"}[key], func(t *testing.T) {
			h, p := newUnderworldMoneyAPI(t)
			p.forbidden = true
			core, logs := observer.New(zap.WarnLevel)
			runner := underworldMoneyRunner(t, h, key, zap.New(core))
			underworldMoneyRefresh(t, h, runner, 5)
			p.mu.Lock()
			requests := 0
			for _, n := range p.wants {
				requests += n
			}
			for _, n := range p.books {
				requests += n
			}
			nonGET := p.nonGET
			p.mu.Unlock()
			if !key && requests != 0 {
				t.Fatalf("missing observer key fell back to main credential: reads=%d", requests)
			}
			if key && requests != 1 {
				t.Fatalf("403 did not disable observer after its first request: reads=%d", requests)
			}
			warns := 0
			for _, entry := range logs.All() {
				if entry.Level != zap.WarnLevel {
					continue
				}
				fields, _ := json.Marshal(entry.ContextMap())
				if strings.Contains(strings.ToLower(entry.Message), "underworld") || strings.Contains(string(fields), "underworld") {
					warns++
				}
			}
			if warns != 1 || nonGET != 0 {
				t.Fatalf("observer disable must emit one WARN and no mutations: warnings=%d nonGET=%d logs=%+v", warns, nonGET, logs.All())
			}
		})
	}
}
