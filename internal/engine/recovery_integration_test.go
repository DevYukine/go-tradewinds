package engine

// These tests drive the real Runner seams (refresh, work, loadMission,
// dispatch) against an httptest game API and a real store in a disposable
// isolated PostgreSQL schema. They cover durable mission recovery: persisted
// cooldowns after definite 429s, growth build rejection recovery, immutable
// action replay across restarts, auth-stop retention with key repair, and
// sunk-ship name reservation. They skip when TEST_DATABASE_URL is absent.

import (
	"bytes"
	"context"
	"crypto/rand"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/DevYukine/go-tradewinds/internal/gameapi"
	"github.com/DevYukine/go-tradewinds/internal/names"
	"github.com/DevYukine/go-tradewinds/internal/store"
	"github.com/google/uuid"
	"go.uber.org/zap"
)

const (
	recoveryAccountID = "recovery-account"
	recoveryCompanyID = "11111111-1111-4111-8111-111111111111"
	recoveryPlayerID  = "22222222-2222-4222-8222-222222222222"
	recoveryHomePort  = "laport"
	recoveryFarPort   = "portb"
	recoveryHull      = "hoy"
	recoveryGrain     = "grain"

	recoveryShipBlastoise = "33333333-3333-4333-8333-000000000001"
	recoveryShipCharizard = "33333333-3333-4333-8333-000000000002"
	recoveryShipLapras    = "33333333-3333-4333-8333-000000000003"
	recoveryBuiltShipID   = "44444444-4444-4444-8444-444444444444"

	primaryTestKey  = "tw_test_primary_synthetic_key"
	repairedTestKey = "tw_test_repaired_synthetic_key"
)

// recoveryIntegrationStore mirrors internal/store's isolated schema helper:
// one random schema per test, dropped again on cleanup, never the live
// database. A fixed synthetic master key encrypts the test journals.
func recoveryIntegrationStore(t *testing.T) (*store.Store, string, string) {
	t.Helper()
	dsn := os.Getenv("TEST_DATABASE_URL")
	if dsn == "" {
		t.Skip("set TEST_DATABASE_URL to run isolated real PostgreSQL integration tests")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	admin, err := sql.Open("pgx", dsn)
	if err != nil {
		t.Fatal(err)
	}
	var random [8]byte
	if _, err := rand.Read(random[:]); err != nil {
		_ = admin.Close()
		t.Fatal(err)
	}
	schema := "tradewinds_engine_recovery_" + hex.EncodeToString(random[:])
	if _, err := admin.ExecContext(ctx, "CREATE SCHEMA "+schema); err != nil {
		_ = admin.Close()
		t.Fatalf("create isolated test schema: %v", err)
	}
	var db *store.Store
	t.Cleanup(func() {
		if db != nil {
			if err := db.Close(); err != nil {
				t.Errorf("close test store: %v", err)
			}
		}
		cleanupCtx, cleanupCancel := context.WithTimeout(context.Background(), 15*time.Second)
		defer cleanupCancel()
		if _, err := admin.ExecContext(cleanupCtx, "DROP SCHEMA "+schema+" CASCADE"); err != nil {
			t.Errorf("drop isolated test schema: %v", err)
		}
		if err := admin.Close(); err != nil {
			t.Errorf("close test administration connection: %v", err)
		}
	})
	parsed, err := url.Parse(dsn)
	if err == nil && (parsed.Scheme == "postgres" || parsed.Scheme == "postgresql") {
		query := parsed.Query()
		query.Set("search_path", schema)
		parsed.RawQuery = query.Encode()
		dsn = parsed.String()
	} else {
		dsn += " search_path=" + schema
	}
	key := bytes.Repeat([]byte{0x5A}, 32)
	db, err = store.Open(ctx, dsn, key, zap.NewNop())
	if err != nil {
		t.Fatalf("open migrated test store: %v", err)
	}
	return db, dsn, schema
}

type recordedMutation struct {
	Method         string
	Path           string
	IdempotencyKey string
	Authorization  string
	Body           []byte
	Status         int
}

// fakeGameAPI is a stateful stand-in for the official game API. It serves the
// full dataset surface refresh() reads, records every mutation attempt it
// receives (method, path, idempotency key, authorization, body, status), and
// parses real request payloads instead of echoing test constants. Mutation
// endpoints switch between ok/throttle/forbid/transport modes as each test
// drives rejection and recovery scenarios.
type fakeGameAPI struct {
	t      *testing.T
	server *httptest.Server

	mu           sync.Mutex
	validKeys    map[string]bool
	autoRemit    bool
	minReserve   int64
	vaultBalance map[string]int64
	fleet        []map[string]any
	factorPorts  []string
	rates        []Rate
	settingsMode string
	remitMode    string
	buildMode    string
	mutations    []recordedMutation
}

func newFakeGameAPI(t *testing.T) *fakeGameAPI {
	t.Helper()
	api := &fakeGameAPI{
		t:            t,
		validKeys:    map[string]bool{primaryTestKey: true},
		autoRemit:    true,
		minReserve:   200_000,
		vaultBalance: map[string]int64{"north": 50_000_000, "south": 500_000},
		factorPorts:  []string{},
		rates: []Rate{
			{FromRegion: "north", ToRegion: "south", FeeBPS: 100},
			{FromRegion: "south", ToRegion: "north", FeeBPS: 100},
		},
		fleet: []map[string]any{
			sailingShipRow(recoveryShipBlastoise, "Blastoise", recoveryHomePort, recoveryFarPort),
			sailingShipRow(recoveryShipCharizard, "Charizard", recoveryFarPort, recoveryHomePort),
		},
		settingsMode: "ok",
		remitMode:    "ok",
		buildMode:    "ok",
		mutations:    make([]recordedMutation, 0),
	}
	api.server = httptest.NewServer(http.HandlerFunc(api.handle))
	t.Cleanup(api.server.Close)
	return api
}

func sailingShipRow(id, name, currentPort, destination string) map[string]any {
	return map[string]any{
		"ship": map[string]any{
			"id":                    id,
			"name":                  name,
			"ship_class":            recoveryHull,
			"status":                "sailing",
			"current_port_id":       currentPort,
			"cannons_count":         2,
			"convoy_id":             nil,
			"turnaround_unlocks_at": nil,
			"dock_queued_at":        nil,
			"berthed_at":            nil,
			"updated_at":            time.Now().UTC().Format(time.RFC3339Nano),
		},
		"hold_capacity_tons":           60,
		"effective_hold_capacity_tons": 56,
		"cargo_used_tons":              0,
		"cargo_manifest":               map[string]any{},
		"stores":                       map[string]any{"provisions_tons": 2, "sea_days_remaining": 5},
		"route_legs": []any{map[string]any{
			"from_node":     currentPort,
			"to_node":       destination,
			"distance_nm":   24,
			"edge_class":    "coastal",
			"is_sound_toll": false,
		}},
		"voyage": map[string]any{
			"estimated_arrival_at": time.Now().UTC().Add(2 * time.Minute).Format(time.RFC3339Nano),
		},
	}
}

func sunkShipRow(id, name string) map[string]any {
	return map[string]any{
		"ship": map[string]any{
			"id":                    id,
			"name":                  name,
			"ship_class":            recoveryHull,
			"status":                "sunk",
			"current_port_id":       nil,
			"cannons_count":         0,
			"convoy_id":             nil,
			"turnaround_unlocks_at": nil,
			"dock_queued_at":        nil,
			"berthed_at":            nil,
			"updated_at":            time.Now().UTC().Format(time.RFC3339Nano),
		},
		"hold_capacity_tons":           60,
		"effective_hold_capacity_tons": 60,
		"cargo_used_tons":              0,
		"cargo_manifest":               map[string]any{},
		"stores":                       map[string]any{"provisions_tons": 0, "sea_days_remaining": 0},
		"route_legs":                   []any{},
		"voyage":                       nil,
	}
}

func (f *fakeGameAPI) handle(writer http.ResponseWriter, request *http.Request) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if !f.authorized(request) {
		f.writeProblem(writer, http.StatusUnauthorized, "invalid_api_key", "the API key is unknown or revoked")
		return
	}
	if request.Method == http.MethodGet {
		f.handleGet(writer, request)
		return
	}
	body, err := io.ReadAll(request.Body)
	if err != nil {
		f.writeProblem(writer, http.StatusBadRequest, "unreadable_request", err.Error())
		return
	}
	switch {
	case request.Method == http.MethodPatch && request.URL.Path == "/v1/company/settings":
		f.handleSettings(writer, request, body)
	case request.Method == http.MethodPost && request.URL.Path == "/v1/treasury/remit":
		f.handleRemit(writer, request, body)
	case request.Method == http.MethodPost && request.URL.Path == "/v1/shipyard/build-ship":
		f.handleBuild(writer, request, body)
	case request.Method == http.MethodPost && request.URL.Path == "/v1/market/place-order":
		f.handlePurchase(writer, request, body)
	case request.Method == http.MethodPost && request.URL.Path == "/v1/fleet/depart":
		f.handleDeparture(writer, request, body)
	default:
		f.record(request, body, http.StatusConflict)
		f.writeProblem(writer, http.StatusConflict, "unsupported_in_test", "this test fake does not implement "+request.URL.Path)
	}
}

func (f *fakeGameAPI) authorized(request *http.Request) bool {
	header := request.Header.Get("Authorization")
	for key := range f.validKeys {
		if header == "Bearer "+key {
			return true
		}
	}
	return false
}

func (f *fakeGameAPI) handleGet(writer http.ResponseWriter, request *http.Request) {
	switch request.URL.Path {
	case "/v1/auth/whoami":
		f.writeJSON(writer, http.StatusOK, map[string]any{
			"api_key_id": "aaaaaaaa-aaaa-4aaa-8aaa-aaaaaaaaaaaa",
			"company_id": recoveryCompanyID,
			"player_id":  recoveryPlayerID,
			"scopes":     []any{"company:trade"},
		})
	case "/v1/player/profile":
		f.writeJSON(writer, http.StatusOK, map[string]any{
			"player":    map[string]any{"id": recoveryPlayerID},
			"companies": []any{},
		})
	case "/v1/company/profile":
		f.writeJSON(writer, http.StatusOK, map[string]any{
			"company": map[string]any{
				"id":             recoveryCompanyID,
				"player_id":      recoveryPlayerID,
				"name":           "The Pokemon Pirates",
				"home_port_id":   recoveryHomePort,
				"crown_prestige": 0,
				"created_at":     time.Now().UTC().Format(time.RFC3339Nano),
				"updated_at":     time.Now().UTC().Format(time.RFC3339Nano),
			},
			"handle":       "Yukine",
			"fleet_count":  len(f.fleet),
			"factor_ports": f.factorPorts,
			"vaults":       []any{},
		})
	case "/v1/company/settings":
		f.writeJSON(writer, http.StatusOK, f.settingsDocument())
	case "/v1/world/catalog":
		f.writeJSON(writer, http.StatusOK, f.catalogDocument())
	case "/v1/world/clock":
		f.writeJSON(writer, http.StatusOK, f.clockDocument())
	case "/v1/world/zones":
		f.writeJSON(writer, http.StatusOK, []any{
			map[string]any{"sea_zone_id": "zone-north", "net_piracy_bps": 0},
			map[string]any{"sea_zone_id": "zone-south", "net_piracy_bps": 0},
		})
	case "/v1/world/expansion", "/v1/world/leaderboard":
		f.writeJSON(writer, http.StatusOK, map[string]any{})
	case "/v1/world/events", "/v1/treasury/ledger", "/v1/fleet/convoys",
		"/v1/fleet/marques", "/v1/fleet/patrols", "/v1/warehouse/list",
		"/v1/market/orders", "/v1/market/fills", "/v1/insurance/list",
		"/v1/future/list", "/v1/contract/list":
		f.writeJSON(writer, http.StatusOK, []any{})
	case "/v1/company/factors":
		f.writeJSON(writer, http.StatusOK, f.factorPorts)
	case "/v1/treasury/vaults":
		vaults := []Vault{
			{AccountID: "vault-north", BankingRegion: "north", AccountType: "vault", BalancePence: f.vaultBalance["north"]},
			{AccountID: "vault-south", BankingRegion: "south", AccountType: "vault", BalancePence: f.vaultBalance["south"]},
		}
		for region, balance := range f.vaultBalance {
			if region != "north" && region != "south" {
				vaults = append(vaults, Vault{AccountID: "vault-" + region, BankingRegion: region, AccountType: "vault", BalancePence: balance})
			}
		}
		f.writeJSON(writer, http.StatusOK, map[string]any{"company_id": recoveryCompanyID, "vaults": vaults})
	case "/v1/treasury/rates":
		f.writeJSON(writer, http.StatusOK, f.rates)
	case "/v1/fleet/ships":
		f.writeJSON(writer, http.StatusOK, f.fleet)
	case "/v1/fleet/ship":
		shipID := request.URL.Query().Get("ship_id")
		for _, row := range f.fleet {
			ship := row["ship"].(map[string]any)
			if ship["id"] == shipID {
				f.writeJSON(writer, http.StatusOK, row)
				return
			}
		}
		f.writeProblem(writer, http.StatusNotFound, "ship_not_found", "no such ship: "+shipID)
	case "/v1/world/port":
		f.writeJSON(writer, http.StatusOK, f.portDocument(request.URL.Query().Get("port_id")))
	case "/v1/market/book":
		f.writeJSON(writer, http.StatusOK, f.bookDocument(request.URL.Query().Get("port_id")))
	case "/v1/shipyard/quote":
		f.writeJSON(writer, http.StatusOK, map[string]any{
			"port_id":           request.URL.Query().Get("port_id"),
			"ship_class":        request.URL.Query().Get("ship_class"),
			"build_mode":        "turnkey_cash",
			"available":         true,
			"labour_pence":      10_000,
			"materials":         []any{},
			"materials_pence":   0,
			"yard_markup_bps":   0,
			"yard_markup_pence": 0,
			"total_cost_pence":  60_000,
		})
	case "/v1/world/route":
		f.writeJSON(writer, http.StatusOK, f.routeDocument(
			request.URL.Query().Get("from"),
			request.URL.Query().Get("to"),
		))
	default:
		f.writeProblem(writer, http.StatusNotFound, "not_found", "this test fake does not implement "+request.URL.Path)
	}
}

func (f *fakeGameAPI) handleSettings(writer http.ResponseWriter, request *http.Request, body []byte) {
	switch f.settingsMode {
	case "transport":
		f.record(request, body, 0)
		conn, _, err := writer.(http.Hijacker).Hijack()
		if err == nil {
			_ = conn.Close()
		}
		return
	case "throttle":
		f.record(request, body, http.StatusTooManyRequests)
		f.writeProblem(writer, http.StatusTooManyRequests, "rate_limit_exceeded", "settings write bucket exhausted")
		return
	case "forbid":
		f.record(request, body, http.StatusForbidden)
		f.writeProblem(writer, http.StatusForbidden, "auth_forbidden", "company operations are frozen for this credential")
		return
	}
	var payload struct {
		AutoRemit  bool  `json:"auto_remit_for_bills"`
		MinReserve int64 `json:"auto_remit_min_reserve_pence"`
	}
	if err := json.Unmarshal(body, &payload); err != nil {
		f.record(request, body, http.StatusBadRequest)
		f.writeProblem(writer, http.StatusBadRequest, "bad_request", err.Error())
		return
	}
	f.autoRemit = payload.AutoRemit
	f.minReserve = payload.MinReserve
	f.record(request, body, http.StatusOK)
	f.writeJSON(writer, http.StatusOK, f.settingsDocument())
}

func (f *fakeGameAPI) handleRemit(writer http.ResponseWriter, request *http.Request, body []byte) {
	if f.remitMode == "throttle" {
		f.record(request, body, http.StatusTooManyRequests)
		f.writeProblem(writer, http.StatusTooManyRequests, "rate_limit_exceeded", "treasury write bucket exhausted")
		return
	}
	var payload struct {
		FromRegion string `json:"from_region"`
		ToRegion   string `json:"to_region"`
		Gross      int64  `json:"gross_amount_pence"`
	}
	if err := json.Unmarshal(body, &payload); err != nil || payload.Gross <= 0 {
		f.record(request, body, http.StatusBadRequest)
		f.writeProblem(writer, http.StatusBadRequest, "bad_request", "unusable remittance request")
		return
	}
	fee := payload.Gross * 100 / 10_000
	f.vaultBalance[payload.FromRegion] -= payload.Gross
	f.vaultBalance[payload.ToRegion] += payload.Gross - fee
	f.record(request, body, http.StatusOK)
	f.writeJSON(writer, http.StatusOK, map[string]any{
		"tx_group_id":        uuid.NewString(),
		"from_region":        payload.FromRegion,
		"to_region":          payload.ToRegion,
		"gross_amount_pence": payload.Gross,
		"fee_bps":            100,
		"fee_pence":          fee,
		"net_credited_pence": payload.Gross - fee,
	})
}

func (f *fakeGameAPI) handleBuild(writer http.ResponseWriter, request *http.Request, body []byte) {
	switch f.buildMode {
	case "throttle":
		f.record(request, body, http.StatusTooManyRequests)
		f.writeProblem(writer, http.StatusTooManyRequests, "rate_limit_exceeded", "shipyard write bucket exhausted")
		return
	case "forbid":
		f.record(request, body, http.StatusForbidden)
		f.writeProblem(writer, http.StatusForbidden, "auth_forbidden", "shipyard refuses this credential")
		return
	}
	var payload struct {
		PortID    string `json:"port_id"`
		Name      string `json:"name"`
		ShipClass string `json:"ship_class"`
		BuildMode string `json:"build_mode"`
	}
	if err := json.Unmarshal(body, &payload); err != nil || payload.Name == "" || payload.ShipClass == "" {
		f.record(request, body, http.StatusBadRequest)
		f.writeProblem(writer, http.StatusBadRequest, "bad_request", "unusable build request")
		return
	}
	f.record(request, body, http.StatusOK)
	f.writeJSON(writer, http.StatusOK, map[string]any{
		"ship_id":          recoveryBuiltShipID,
		"name":             payload.Name,
		"ship_class":       payload.ShipClass,
		"port_id":          payload.PortID,
		"build_mode":       payload.BuildMode,
		"total_cost_pence": 60_000,
		"status":           "berthed",
	})
}

func (f *fakeGameAPI) handlePurchase(writer http.ResponseWriter, request *http.Request, body []byte) {
	var payload struct {
		ShipID      string `json:"ship_id"`
		PortID      string `json:"port_id"`
		CommodityID string `json:"commodity_id"`
		Side        string `json:"side"`
		Quantity    int64  `json:"quantity"`
		PricePence  int64  `json:"price_pence"`
		TimeInForce string `json:"time_in_force"`
	}
	if err := json.Unmarshal(body, &payload); err != nil {
		f.writeProblem(writer, http.StatusBadRequest, "bad_request", err.Error())
		return
	}
	for _, row := range f.fleet {
		ship := row["ship"].(map[string]any)
		if ship["id"] != payload.ShipID {
			continue
		}
		gross := payload.Quantity * 100
		customs := ceilBPS(gross, 200)
		if ship["status"] != "berthed" || ship["current_port_id"] != payload.PortID ||
			payload.PortID != recoveryHomePort || payload.CommodityID != recoveryGrain ||
			payload.Side != "bid" || payload.TimeInForce != "ioc" ||
			payload.Quantity <= 0 || payload.Quantity > int64(row["effective_hold_capacity_tons"].(int)) ||
			payload.PricePence < 100 || f.vaultBalance["north"] < gross+customs {
			f.record(request, body, http.StatusConflict)
			f.writeProblem(writer, http.StatusConflict, "invalid_purchase", "purchase cannot fill against the observed local book and ship")
			return
		}
		row["cargo_manifest"] = map[string]any{payload.CommodityID: payload.Quantity}
		row["cargo_used_tons"] = payload.Quantity
		f.vaultBalance["north"] -= gross + customs
		f.record(request, body, http.StatusOK)
		f.writeJSON(writer, http.StatusOK, map[string]any{
			"commodity_id": payload.CommodityID, "filled_quantity": payload.Quantity,
			"total_gross_pence": gross, "customs_duty_pence": customs,
		})
		return
	}
	f.writeProblem(writer, http.StatusNotFound, "ship_not_found", "purchase ship does not exist")
}

func (f *fakeGameAPI) handleDeparture(writer http.ResponseWriter, request *http.Request, body []byte) {
	var payload struct {
		ShipID      string `json:"ship_id"`
		Destination string `json:"dest_port_id"`
	}
	if err := json.Unmarshal(body, &payload); err != nil {
		f.writeProblem(writer, http.StatusBadRequest, "bad_request", err.Error())
		return
	}
	for _, row := range f.fleet {
		ship := row["ship"].(map[string]any)
		if ship["id"] != payload.ShipID {
			continue
		}
		cargo := row["cargo_manifest"].(map[string]any)
		if ship["status"] != "berthed" || ship["current_port_id"] != recoveryHomePort ||
			payload.Destination != recoveryFarPort || cargo[recoveryGrain] == nil ||
			row["stores"].(map[string]any)["provisions_tons"].(int) < 2 {
			f.record(request, body, http.StatusConflict)
			f.writeProblem(writer, http.StatusConflict, "invalid_departure", "ship lacks purchased cargo or safe stores for the observed route")
			return
		}
		voyage := sailingShipRow(payload.ShipID, ship["name"].(string), recoveryHomePort, payload.Destination)
		ship["status"] = "sailing"
		row["route_legs"] = voyage["route_legs"]
		row["voyage"] = voyage["voyage"]
		f.record(request, body, http.StatusOK)
		f.writeJSON(writer, http.StatusOK, map[string]any{
			"ship_id": payload.ShipID, "dest_port_id": payload.Destination,
			"berth_dues_pence": 0, "berth_dues_debt_pence": 0,
		})
		return
	}
	f.writeProblem(writer, http.StatusNotFound, "ship_not_found", "departure ship does not exist")
}

func (f *fakeGameAPI) settingsDocument() map[string]any {
	return map[string]any{
		"auto_remit_for_bills":         f.autoRemit,
		"auto_remit_min_reserve_pence": f.minReserve,
	}
}

func (f *fakeGameAPI) catalogDocument() map[string]any {
	return map[string]any{
		"commodities": []any{},
		"ship_classes": []any{map[string]any{
			"id":                    recoveryHull,
			"hull_category":         "coastal",
			"phase":                 1,
			"base_speed_centiknots": 850,
			"hold_capacity_tons":    60,
			"max_cannons":           4,
			"crew_required":         10,
		}},
		"sea_zones": []any{
			map[string]any{"id": "zone-north", "zone_class": "coastal", "base_storm_bps": 100, "base_piracy_bps": 0},
			map[string]any{"id": "zone-south", "zone_class": "coastal", "base_storm_bps": 100, "base_piracy_bps": 0},
		},
		"ports": []any{
			map[string]any{
				"id": recoveryHomePort, "name": "Laport", "banking_region": "north",
				"sea_zone_id": "zone-north", "phase": 1, "is_hub": true, "has_shipyard": true,
				"customs_bps": 200, "base_berths": 10, "effective_berths": 10,
				"total_warehouse_cap": 1000,
				"produces":            []any{recoveryGrain}, "manufactures": []any{}, "imports": []any{"provisions"},
			},
			map[string]any{
				"id": recoveryFarPort, "name": "Portb", "banking_region": "south",
				"sea_zone_id": "zone-south", "phase": 1, "is_hub": false, "has_shipyard": false,
				"customs_bps": 200, "base_berths": 10, "effective_berths": 10,
				"total_warehouse_cap": 1000,
				"produces":            []any{}, "manufactures": []any{}, "imports": []any{recoveryGrain, "provisions"},
			},
		},
	}
}

func (f *fakeGameAPI) clockDocument() map[string]any {
	return map[string]any{
		"real_utc":                 time.Now().UTC().Format(time.RFC3339Nano),
		"season_number":            2,
		"unlocked_phase":           1,
		"global_health_bps":        10_000,
		"active_player_volume_24h": 1,
		"paused":                   false,
		"calendar": map[string]any{
			"year": 1700, "month": 5, "day": 10, "hour": 8, "minute": 0,
			"season": "spring", "total_game_hours": 2_400, "time_scale": 1,
		},
	}
}

func (f *fakeGameAPI) portDocument(portID string) map[string]any {
	region, zone := "north", "zone-north"
	if portID == recoveryFarPort {
		region, zone = "south", "zone-south"
	}
	return map[string]any{
		"id": portID, "name": portID, "banking_region": region, "sea_zone_id": zone,
		"phase": 1, "is_hub": portID == recoveryHomePort, "has_shipyard": portID == recoveryHomePort,
		"customs_bps": 200, "base_customs_bps": 200, "health_score_bps": 10_000,
		"rolling_player_volume_tons":    0,
		"rolling_player_turnover_pence": 0,
		"base_berths":                   10,
		"effective_berths":              10,
		"occupied_berths":               0,
		"queued_ships":                  0,
		"berth_dues_per_day_pence":      100,
		"total_warehouse_cap":           1000,
		"total_leased_warehouse":        0,
		"counting_house_min_lease_tons": 10,
		"visibility":                    "quay",
		"coffee_house_gossip":           []any{},
	}
}

func (f *fakeGameAPI) bookDocument(portID string) map[string]any {
	ask := func(price int64) map[string]any {
		return map[string]any{
			"order_id":           fmt.Sprintf("ask-%s-%d", portID, price),
			"price_pence":        price,
			"remaining_quantity": 10_000,
		}
	}
	bid := func(price int64) map[string]any {
		return map[string]any{
			"order_id":           fmt.Sprintf("bid-%s-%d", portID, price),
			"price_pence":        price,
			"remaining_quantity": 10_000,
		}
	}
	var books []any
	if portID == recoveryHomePort {
		books = []any{
			map[string]any{"commodity_id": recoveryGrain, "depth": map[string]any{"bids": []any{}, "asks": []any{ask(100)}}},
			map[string]any{"commodity_id": "provisions", "depth": map[string]any{"bids": []any{}, "asks": []any{ask(50)}}},
		}
	} else {
		books = []any{
			map[string]any{"commodity_id": recoveryGrain, "depth": map[string]any{"bids": []any{bid(500)}, "asks": []any{}}},
			map[string]any{"commodity_id": "provisions", "depth": map[string]any{"bids": []any{}, "asks": []any{ask(50)}}},
		}
	}
	return map[string]any{
		"port_id":     portID,
		"visibility":  "quay",
		"customs_bps": 200,
		"as_of":       time.Now().UTC().Format(time.RFC3339Nano),
		"books":       books,
	}
}

func (f *fakeGameAPI) routeDocument(from, to string) map[string]any {
	return map[string]any{
		"from_port": from, "to_port": to, "ship_class": recoveryHull,
		"total_distance_nm": 24, "expected_provisions_tons": 0.2,
		"safe_provisions_tons": 1, "port_provisions_tons_per_day": 0.05,
		"legs": []any{map[string]any{
			"from_node": from, "to_node": to, "distance_nm": 24,
			"edge_class": "coastal", "is_sound_toll": false,
		}},
	}
}

func (f *fakeGameAPI) writeJSON(writer http.ResponseWriter, status int, document any) {
	payload, err := json.Marshal(document)
	if err != nil {
		f.t.Fatalf("fake game API cannot encode %T: %v", document, err)
	}
	writer.Header().Set("Content-Type", "application/json")
	writer.WriteHeader(status)
	_, _ = writer.Write(payload)
}

func (f *fakeGameAPI) writeProblem(writer http.ResponseWriter, status int, code, detail string) {
	f.writeJSON(writer, status, map[string]any{
		"type":   "about:blank",
		"title":  http.StatusText(status),
		"status": status,
		"code":   code,
		"detail": detail,
	})
}

func (f *fakeGameAPI) record(request *http.Request, body []byte, status int) {
	f.mutations = append(f.mutations, recordedMutation{
		Method:         request.Method,
		Path:           request.URL.Path,
		IdempotencyKey: request.Header.Get("Idempotency-Key"),
		Authorization:  request.Header.Get("Authorization"),
		Body:           body,
		Status:         status,
	})
}

func (f *fakeGameAPI) mutationsFor(path string) []recordedMutation {
	f.mu.Lock()
	defer f.mu.Unlock()
	matches := make([]recordedMutation, 0)
	for _, mutation := range f.mutations {
		if mutation.Path == path {
			matches = append(matches, mutation)
		}
	}
	return matches
}

func (f *fakeGameAPI) mutationCount(path string) int {
	return len(f.mutationsFor(path))
}

func (f *fakeGameAPI) setAutoRemit(enabled bool, minReserve int64) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.autoRemit = enabled
	f.minReserve = minReserve
}

func (f *fakeGameAPI) setVaultBalance(region string, balance int64) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.vaultBalance[region] = balance
}

func (f *fakeGameAPI) setFleet(rows []map[string]any) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.fleet = rows
}

func (f *fakeGameAPI) setFactorPorts(ports ...string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.factorPorts = append(f.factorPorts[:0], ports...)
}

func (f *fakeGameAPI) setRates(rates ...Rate) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.rates = append(f.rates[:0], rates...)
}

func (f *fakeGameAPI) setSettingsMode(mode string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.settingsMode = mode
}

func (f *fakeGameAPI) setRemitMode(mode string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.remitMode = mode
}

func (f *fakeGameAPI) setBuildMode(mode string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.buildMode = mode
}

func (f *fakeGameAPI) replaceValidKeys(key string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.validKeys = map[string]bool{key: true}
}

// recoveryHarness owns one isolated store, one test account and one fake game
// API. tick() reproduces the Runner.Run loop body (new generation, REST
// refresh, identity check, work) so tests exercise the production seams
// without starting the SSE stream.
type recoveryHarness struct {
	t       *testing.T
	ctx     context.Context
	db      *store.Store
	account store.Account
	api     *fakeGameAPI
	dsn     string
	schema  string
	// marques enables MARQUES_ENABLED for runners built by this harness.
	marques bool
}

func newRecoveryHarness(t *testing.T) *recoveryHarness {
	t.Helper()
	db, dsn, schema := recoveryIntegrationStore(t)
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
	t.Cleanup(cancel)
	account := store.Account{
		ID:        recoveryAccountID,
		Label:     "Recovery Account",
		CompanyID: recoveryCompanyID,
		Enabled:   true,
	}
	credentials := store.Credentials{
		Handle:     "Yukine",
		Passphrase: "synthetic passphrase, never used against the real game",
		APIKey:     primaryTestKey,
	}
	if err := db.SaveAccount(ctx, account, credentials); err != nil {
		t.Fatalf("save test account: %v", err)
	}
	return &recoveryHarness{t: t, ctx: ctx, db: db, account: account, api: newFakeGameAPI(t), dsn: dsn, schema: schema}
}

func (h *recoveryHarness) runner(apiKey string) *Runner {
	h.t.Helper()
	client := gameapi.New(h.api.server.URL, apiKey, zap.NewNop())
	instance := New(client, h.db, h.account, zap.NewNop(), Options{Execute: true, MarquesEnabled: h.marques})
	snapshot, err := h.db.Snapshot(h.ctx, h.account.ID)
	if err != nil {
		h.t.Fatalf("load persisted snapshot: %v", err)
	}
	if snapshot != nil {
		instance.snapshot = snapshot
		instance.meta = decode[map[string]DatasetMeta](snapshot, "_meta")
		if instance.meta == nil {
			instance.meta = make(map[string]DatasetMeta)
		}
	}
	instance.expenseOwners = decode[map[string]string](instance.snapshot, "_expense_allocations")
	if instance.expenseOwners == nil {
		instance.expenseOwners = make(map[string]string)
	}
	client.SetObserver(func(method, path string, status int, duration time.Duration, request, response []byte, errText string) {
		if method != http.MethodGet {
			instance.lastMutationStatus.Store(int64(status))
		}
		if err := h.db.RecordRequest(h.ctx, h.account.ID, method, path, status, duration, request, response, errText); err != nil && h.ctx.Err() == nil {
			h.t.Logf("record request %s %s: %v", method, path, err)
		}
	})
	return instance
}

func (h *recoveryHarness) tick(instance *Runner) (bool, error) {
	h.t.Helper()
	instance.generation = uuid.NewString()
	if err := instance.refresh(h.ctx); err != nil {
		return false, err
	}
	if err := instance.verifyIdentity(); err != nil {
		return false, err
	}
	return instance.work(h.ctx)
}

func (h *recoveryHarness) mission(shipID string) (*store.Mission, missionData) {
	h.t.Helper()
	mission, err := h.db.GetMission(h.ctx, h.account.ID, shipID)
	if err != nil {
		h.t.Fatalf("load mission %s: %v", shipID, err)
	}
	if mission == nil {
		h.t.Fatalf("mission %s is missing", shipID)
	}
	var data missionData
	if err := json.Unmarshal(mission.Data, &data); err != nil {
		h.t.Fatalf("decode mission %s: %v", shipID, err)
	}
	return mission, data
}

func (h *recoveryHarness) missingMission(shipID string) {
	h.t.Helper()
	mission, err := h.db.GetMission(h.ctx, h.account.ID, shipID)
	if err != nil {
		h.t.Fatalf("load mission %s: %v", shipID, err)
	}
	if mission != nil {
		h.t.Fatalf("mission %s still exists in phase %s", shipID, mission.Phase)
	}
}

func (h *recoveryHarness) pending() []store.Action {
	h.t.Helper()
	pending, err := h.db.PendingActions(h.ctx, h.account.ID)
	if err != nil {
		h.t.Fatalf("load pending actions: %v", err)
	}
	return pending
}

func (h *recoveryHarness) putMission(shipID, generation, phase string, data missionData) {
	h.t.Helper()
	payload, err := json.Marshal(data)
	if err != nil {
		h.t.Fatalf("encode mission data: %v", err)
	}
	mission := &store.Mission{
		AccountID:   h.account.ID,
		ShipID:      shipID,
		Phase:       phase,
		Generation:  generation,
		Destination: data.Plan.Destination,
		Commodity:   data.Plan.Commodity,
		Quantity:    data.Plan.Quantity,
		PricePence:  data.Plan.BuyLimitPence,
		Data:        payload,
	}
	if err := h.db.PutMission(h.ctx, mission); err != nil {
		h.t.Fatalf("persist mission %s: %v", shipID, err)
	}
}

// expireCooldown advances the persisted retry deadline past its boundary.
// Tests can exercise both sides deterministically without racing a short
// wall-clock timer against REST refreshes and database writes.
func (h *recoveryHarness) expireCooldown(shipID string) {
	h.t.Helper()
	mission, data := h.mission(shipID)
	data.CooldownUntil = time.Now().UTC().Add(-time.Second)
	payload, err := json.Marshal(data)
	if err != nil {
		h.t.Fatalf("encode mission data: %v", err)
	}
	mission.Data = payload
	if err := h.db.PutMission(h.ctx, mission); err != nil {
		h.t.Fatalf("expire mission cooldown: %v", err)
	}
}

// seedLaneProofs persists two known-cost profitable settlements on the home
// lane so considerGrowth has durable evidence to evaluate.
func (h *recoveryHarness) seedLaneProofs() {
	h.t.Helper()
	// Growth needs real private-data authority while incumbent ships sail.
	// This setup is opt-in; the default recovery fleet has no factors.
	h.api.setFactorPorts(recoveryHomePort, recoveryFarPort)
	lane := laneKey(Plan{
		Kind:        "spot",
		Origin:      recoveryHomePort,
		Destination: recoveryFarPort,
		Commodity:   recoveryGrain,
	})
	sold := ShipState{}
	sold.Ship.ID = recoveryShipBlastoise
	sold.Ship.CurrentPortID = recoveryFarPort
	saleBody := encode(orderBody(sold, recoveryGrain, "ask", 48, 625, false))
	saleResult := json.RawMessage(`{"commodity_id":"grain","filled_quantity":48,"total_gross_pence":30000,"customs_duty_pence":0}`)
	for i := range 2 {
		proof := i + 1
		settlement := &store.Settlement{
			ID:               fmt.Sprintf("recovery-proof-%d", proof),
			AccountID:        h.account.ID,
			ShipID:           recoveryShipBlastoise,
			MissionID:        fmt.Sprintf("recovery-proof-mission-%d", proof),
			Lane:             lane,
			ActionID:         fmt.Sprintf("recovery-proof-action-%d", proof),
			Kind:             "spot",
			Origin:           recoveryHomePort,
			Destination:      recoveryFarPort,
			Commodity:        recoveryGrain,
			QuantityTons:     48,
			GameSeconds:      43_200,
			CostsKnown:       true,
			AcquisitionPence: 4_800,
			ProceedsPence:    30_000,
			ProvisionsPence:  208,
			ExpensesPence:    350,
			StartedAt:        time.Now().UTC().Add(-2 * time.Hour),
			CompletedAt:      time.Now().UTC().Add(-time.Hour),
		}
		if _, err := h.db.BeginAction(h.ctx, &store.Action{
			ID:        settlement.ActionID,
			AccountID: h.account.ID,
			Method:    http.MethodPost,
			Path:      "/v1/market/place-order",
			Body:      saleBody,
		}); err != nil {
			h.t.Fatalf("seed settlement action: %v", err)
		}
		if err := h.db.CompleteAction(h.ctx, settlement.ActionID, http.StatusOK, saleResult, nil); err != nil {
			h.t.Fatalf("complete settlement evidence: %v", err)
		}
		if err := h.db.PutSettlement(h.ctx, settlement); err != nil {
			h.t.Fatalf("seed settlement proof: %v", err)
		}
	}
}

func (h *recoveryHarness) usedShipNames() []string {
	h.t.Helper()
	snapshot, err := h.db.Snapshot(h.ctx, h.account.ID)
	if err != nil {
		h.t.Fatalf("load snapshot: %v", err)
	}
	used := decode[[]string](snapshot, "_used_ship_names")
	if used == nil {
		h.t.Fatal("snapshot has no _used_ship_names dataset")
	}
	return used
}

func (h *recoveryHarness) hasRecordedRequest(method, path string, status int) bool {
	h.t.Helper()
	rows, err := h.db.History(h.ctx, h.account.ID, "requests", 500)
	if err != nil {
		h.t.Fatalf("load request history: %v", err)
	}
	requests, ok := rows.([]store.Request)
	if !ok {
		h.t.Fatalf("unexpected request history payload %T", rows)
	}
	for _, row := range requests {
		if row.Method == method && row.Path == path && row.Status == status {
			return true
		}
	}
	return false
}

// A treasury 429 is a definite outcome: the journal settles it, the mission
// carries a persisted cooldown, nothing is sent while the cooldown holds (even
// across a runner restart), and the rejected intent is discarded in favour of
// a freshly planned action instead of freezing account work. Covers both the
// settings and the remittance stage.
func TestRecoveryTreasuryThrottlePersistsCooldownThenReplans(t *testing.T) {
	h := newRecoveryHarness(t)
	h.api.setSettingsMode("throttle")
	h.api.setAutoRemit(false, 0)
	h.api.setVaultBalance("south", 0)
	runner := h.runner(primaryTestKey)

	if _, err := h.tick(runner); err != nil {
		t.Fatalf("stage treasury settings intent: %v", err)
	}
	mission, data := h.mission("__treasury")
	if mission.Phase != "settings" || data.Action == nil || data.Action.Stage != "settings" {
		t.Fatalf("unexpected staged intent: phase %s action %+v", mission.Phase, data.Action)
	}
	if pending := h.pending(); len(pending) != 0 {
		t.Fatalf("staging must not create journal rows: %+v", pending)
	}
	if h.api.mutationCount("/v1/company/settings") != 0 {
		t.Fatal("staging must not send the settings request")
	}

	if _, err := h.tick(runner); err != nil {
		t.Fatalf("dispatch throttled settings intent: %v", err)
	}
	throttledCalls := h.api.mutationsFor("/v1/company/settings")
	if len(throttledCalls) != 1 || throttledCalls[0].Status != http.StatusTooManyRequests {
		t.Fatalf("expected exactly one 429 settings request, got %+v", throttledCalls)
	}
	throttledKey := throttledCalls[0].IdempotencyKey
	mission, data = h.mission("__treasury")
	if mission.Phase != "done" || data.Action != nil || data.LastActionID != throttledKey {
		t.Fatalf("429 did not settle the intent: phase %s action %+v", mission.Phase, data.Action)
	}
	if !data.CooldownUntil.After(time.Now().Add(50 * time.Millisecond)) {
		t.Fatalf("429 did not persist a retry cooldown: %s", data.CooldownUntil)
	}
	if pending := h.pending(); len(pending) != 0 {
		t.Fatalf("a definite 429 must not stay pending: %+v", pending)
	}

	// No settings request may cross the persisted cooldown, including after a
	// runner restart.
	if _, err := h.tick(runner); err != nil {
		t.Fatalf("cooldown tick: %v", err)
	}
	if _, err := h.tick(h.runner(primaryTestKey)); err != nil {
		t.Fatalf("restarted cooldown tick: %v", err)
	}
	if h.api.mutationCount("/v1/company/settings") != 1 {
		t.Fatal("a settings request crossed the persisted cooldown")
	}

	h.expireCooldown("__treasury")
	if _, err := h.tick(runner); err != nil {
		t.Fatalf("post-cooldown tick: %v", err)
	}
	h.missingMission("__treasury")

	if _, err := h.tick(runner); err != nil {
		t.Fatalf("replan tick: %v", err)
	}
	mission, data = h.mission("__treasury")
	if data.Action == nil || data.Action.ID == throttledKey {
		t.Fatalf("rejected intent was retried instead of replanned: %+v", data.Action)
	}
	h.api.setSettingsMode("ok")
	if _, err := h.tick(runner); err != nil {
		t.Fatalf("replanned settings dispatch: %v", err)
	}
	settingsCalls := h.api.mutationsFor("/v1/company/settings")
	if len(settingsCalls) != 2 || settingsCalls[1].Status != http.StatusOK {
		t.Fatalf("replanned settings outcome: %+v", settingsCalls)
	}

	// The recovered settings allow a real remittance to the other ship's
	// region. Its drained vault must be funded before ordinary fleet work
	// can resume.
	if _, err := h.tick(runner); err != nil {
		t.Fatalf("settle settings intent: %v", err)
	}
	h.missingMission("__treasury")
	if _, err := h.tick(runner); err != nil {
		t.Fatalf("stage regional remittance: %v", err)
	}
	// Treasury planning uses actual fleet presence before fleet recovery,
	// so the next account action is the staged remittance.
	mission, data = h.mission("__treasury")
	if mission.Phase != "remit" || data.Action == nil {
		t.Fatalf("no remittance intent staged: phase %s action %+v", mission.Phase, data.Action)
	}
	remitIntent := data.Action.ID

	h.api.setRemitMode("throttle")
	if _, err := h.tick(runner); err != nil {
		t.Fatalf("dispatch throttled remittance: %v", err)
	}
	mission, data = h.mission("__treasury")
	remitCalls := h.api.mutationsFor("/v1/treasury/remit")
	if len(remitCalls) != 1 || remitCalls[0].Status != http.StatusTooManyRequests || remitCalls[0].IdempotencyKey != remitIntent {
		t.Fatalf("throttled remittance outcome: %+v", remitCalls)
	}
	if mission.Phase != "done" || data.Action != nil {
		t.Fatalf("429 did not settle the remittance intent: phase %s", mission.Phase)
	}
	if _, err := h.tick(runner); err != nil {
		t.Fatalf("remittance cooldown tick: %v", err)
	}
	if h.api.mutationCount("/v1/treasury/remit") != 1 {
		t.Fatal("a remittance request crossed the persisted cooldown")
	}

	h.expireCooldown("__treasury")
	if _, err := h.tick(runner); err != nil {
		t.Fatalf("post-cooldown remittance tick: %v", err)
	}
	h.missingMission("__treasury")
	if _, err := h.tick(runner); err != nil {
		t.Fatalf("replan remittance tick: %v", err)
	}
	mission, data = h.mission("__treasury")
	if data.Action == nil || data.Action.ID == remitIntent {
		t.Fatalf("rejected remittance was retried instead of replanned: %+v", data.Action)
	}
	h.api.setRemitMode("ok")
	if _, err := h.tick(runner); err != nil {
		t.Fatalf("replanned remittance dispatch: %v", err)
	}
	remitCalls = h.api.mutationsFor("/v1/treasury/remit")
	if len(remitCalls) != 2 || remitCalls[1].Status != http.StatusOK {
		t.Fatalf("replanned remittance outcome: %+v", remitCalls)
	}

	// A fresh berthed ship must earn again, not merely leave the account quiet
	// after the treasury intents disappear.
	berthed := sailingShipRow(recoveryShipLapras, "Lapras", recoveryHomePort, recoveryFarPort)
	berthed["ship"].(map[string]any)["status"] = "berthed"
	berthed["ship"].(map[string]any)["cannons_count"] = 4
	berthed["effective_hold_capacity_tons"] = 52
	berthed["stores"] = map[string]any{"provisions_tons": 4, "sea_days_remaining": 5}
	berthed["route_legs"] = []any{}
	berthed["voyage"] = nil
	h.api.setFleet([]map[string]any{
		berthed,
		sailingShipRow(recoveryShipCharizard, "Charizard", recoveryFarPort, recoveryHomePort),
	})
	h.api.setFactorPorts(recoveryFarPort)
	for tick := range 16 {
		if _, err := h.tick(runner); err != nil {
			t.Fatalf("fleet recovery tick %d: %v", tick, err)
		}
		if h.api.mutationCount("/v1/fleet/depart") != 0 {
			break
		}
	}
	purchases := h.api.mutationsFor("/v1/market/place-order")
	departures := h.api.mutationsFor("/v1/fleet/depart")
	if len(purchases) != 1 || purchases[0].Status != http.StatusOK ||
		len(departures) != 1 || departures[0].Status != http.StatusOK {
		t.Fatalf("recovered fleet did not purchase and depart: purchases %+v, departures %+v", purchases, departures)
	}
	mission, data = h.mission(recoveryShipLapras)
	if mission.Phase != "sailing" || !data.BuyDone || data.BoughtQuantity <= 0 ||
		data.Plan.Economics.ExpectedNetPence <= 0 || data.Plan.Destination != recoveryFarPort ||
		data.DepartedCargo[recoveryGrain] != data.BoughtQuantity {
		t.Fatalf("recovered voyage did not retain its executed profitable plan: phase %s, data %+v", mission.Phase, data)
	}
	if _, err := h.tick(runner); err != nil {
		t.Fatalf("observe recovered voyage underway: %v", err)
	}
	underway, exists := runner.ship(recoveryShipLapras)
	if !exists || underway.Ship.Status != "sailing" || underway.CargoManifest[recoveryGrain] != data.BoughtQuantity ||
		len(underway.RouteLegs) == 0 || underway.RouteLegs[len(underway.RouteLegs)-1].ToNode != recoveryFarPort {
		t.Fatalf("authoritative fleet does not confirm the purchased cargo underway: %+v", underway)
	}
	h.missingMission("__treasury")
	rows, err := h.db.History(h.ctx, h.account.ID, "decisions", 500)
	if err != nil {
		t.Fatalf("load decisions: %v", err)
	}
	decisions, ok := rows.([]store.Decision)
	if !ok {
		t.Fatalf("unexpected decision history payload %T", rows)
	}
	rejections := 0
	for _, row := range decisions {
		if row.Kind == "action_rejected" {
			rejections++
		}
	}
	if rejections < 2 {
		t.Fatalf("expected recorded rejections for both definite 429s, got %d", rejections)
	}
}

// A legacy actionless treasury intent restored from an older build must wait
// out its persisted cooldown, then be discarded so fresh planning takes over;
// the replanned remittance then follows the ordinary definite-rejection path.
func TestRecoveryLegacyActionlessTreasuryIntentRecovers(t *testing.T) {
	h := newRecoveryHarness(t)
	runner := h.runner(primaryTestKey)
	for i := range 2 {
		if _, err := h.tick(runner); err != nil {
			t.Fatalf("fleet recovery tick %d: %v", i+1, err)
		}
	}
	h.api.setVaultBalance("south", 0)

	legacyData := missionData{
		Plan:          Plan{Kind: "treasury"},
		CooldownUntil: time.Now().UTC().Add(time.Hour),
	}
	h.putMission("__treasury", "legacy-treasury-generation", "remit", legacyData)

	if _, err := h.tick(runner); err != nil {
		t.Fatalf("legacy cooldown tick: %v", err)
	}
	retained, _ := h.mission("__treasury")
	if retained.Generation != "legacy-treasury-generation" {
		t.Fatalf("legacy intent replaced during its cooldown: %s", retained.Generation)
	}
	if h.api.mutationCount("/v1/treasury/remit") != 0 {
		t.Fatal("legacy intent was dispatched during its cooldown")
	}

	legacyData.CooldownUntil = time.Now().UTC().Add(-time.Second)
	h.putMission("__treasury", "legacy-treasury-generation", "remit", legacyData)
	if _, err := h.tick(runner); err != nil {
		t.Fatalf("legacy recovery tick: %v", err)
	}
	h.missingMission("__treasury")
	if h.api.mutationCount("/v1/treasury/remit") != 0 {
		t.Fatal("discarded legacy intent was retried on the wire")
	}

	if _, err := h.tick(runner); err != nil {
		t.Fatalf("fresh remittance planning: %v", err)
	}
	mission, data := h.mission("__treasury")
	if mission.Generation == "legacy-treasury-generation" || data.Action == nil {
		t.Fatalf("fresh remittance was not planned: generation %s action %+v", mission.Generation, data.Action)
	}
	replannedID := data.Action.ID

	h.api.setRemitMode("throttle")
	if _, err := h.tick(runner); err != nil {
		t.Fatalf("fresh remittance dispatch: %v", err)
	}
	calls := h.api.mutationsFor("/v1/treasury/remit")
	if len(calls) != 1 || calls[0].Status != http.StatusTooManyRequests || calls[0].IdempotencyKey != replannedID {
		t.Fatalf("restored intent did not follow the normal rejection path: %+v", calls)
	}
	mission, _ = h.mission("__treasury")
	if mission.Phase != "done" {
		t.Fatalf("replanned intent phase after 429: %s", mission.Phase)
	}
}

// Authorization and throttling retain their outcome and cooldown, then release
// the control intent for fresh planning with a new action identity.
func TestRecoveryGrowthBuildRejectionReplansThroughFreshPlanning(t *testing.T) {
	h := newRecoveryHarness(t)
	runner := h.runner(primaryTestKey)
	for i := range 2 {
		if _, err := h.tick(runner); err != nil {
			t.Fatalf("fleet recovery tick %d: %v", i+1, err)
		}
	}
	h.seedLaneProofs()

	if _, err := h.tick(runner); err != nil {
		t.Fatalf("stage growth build: %v", err)
	}
	mission, data := h.mission("__growth")
	if mission.Phase != "growth_build" || data.Action == nil || data.Action.Stage != "build" || data.BuildName == "" {
		t.Fatalf("no durable build intent: phase %s action %+v", mission.Phase, data.Action)
	}
	firstAction := data.Action.ID
	if pending := h.pending(); len(pending) != 0 {
		t.Fatalf("staging must not create journal rows: %+v", pending)
	}
	if h.api.mutationCount("/v1/shipyard/build-ship") != 0 {
		t.Fatal("staging must not send the build request")
	}

	h.api.setBuildMode("throttle")
	if _, err := h.tick(runner); err != nil {
		t.Fatalf("dispatch throttled build: %v", err)
	}
	mission, data = h.mission("__growth")
	calls := h.api.mutationsFor("/v1/shipyard/build-ship")
	if len(calls) != 1 || calls[0].Status != http.StatusTooManyRequests || calls[0].IdempotencyKey != firstAction {
		t.Fatalf("throttled build outcome: %+v", calls)
	}
	if mission.Phase != "growth_rejected" || data.Action != nil {
		t.Fatalf("429 did not settle the build intent: phase %s", mission.Phase)
	}
	if !data.CooldownUntil.After(time.Now().Add(50 * time.Millisecond)) {
		t.Fatalf("429 did not persist a build cooldown: %s", data.CooldownUntil)
	}

	if _, err := h.tick(runner); err != nil {
		t.Fatalf("build cooldown tick: %v", err)
	}
	if h.api.mutationCount("/v1/shipyard/build-ship") != 1 {
		t.Fatal("a build request crossed the persisted cooldown")
	}

	h.expireCooldown("__growth")
	for pass := 0; pass < 8; pass++ {
		if _, err := h.tick(runner); err != nil {
			t.Fatalf("settle rejected build after 429: %v", err)
		}
		remaining, err := h.db.GetMission(h.ctx, h.account.ID, "__growth")
		if err != nil {
			t.Fatalf("read rejected build after 429: %v", err)
		}
		if remaining == nil {
			break
		}
	}
	h.missingMission("__growth")
	if _, err := h.tick(runner); err != nil {
		t.Fatalf("replan build after 429: %v", err)
	}
	mission, data = h.mission("__growth")
	if data.Action == nil {
		t.Fatal("considerGrowth did not replan the rejected build")
	}
	secondAction := data.Action.ID
	if secondAction == firstAction {
		t.Fatal("rejected build intent was retried with the same action identity")
	}

	// A definitive 403 stops execution but retains the journal and mission.
	h.api.setBuildMode("forbid")
	_, workErr := h.tick(runner)
	if workErr == nil || !strings.Contains(workErr.Error(), "403") {
		t.Fatalf("definitive 403 did not stop work: %v", workErr)
	}
	mission, data = h.mission("__growth")
	calls = h.api.mutationsFor("/v1/shipyard/build-ship")
	if len(calls) != 2 || calls[1].Status != http.StatusForbidden || calls[1].IdempotencyKey != secondAction {
		t.Fatalf("forbidden build outcome: %+v", calls)
	}
	if mission.Phase != "growth_rejected" || data.Action != nil {
		t.Fatalf("403 did not settle the build intent: phase %s", mission.Phase)
	}
	if pending := h.pending(); len(pending) != 0 {
		t.Fatalf("definitive 403 left a pending journal row: %+v", pending)
	}

	if _, err := h.tick(runner); err != nil {
		t.Fatalf("403 cooldown tick: %v", err)
	}
	if h.api.mutationCount("/v1/shipyard/build-ship") != 2 {
		t.Fatal("a build request crossed the 403 cooldown")
	}

	h.expireCooldown("__growth")
	for pass := 0; pass < 8; pass++ {
		if _, err := h.tick(runner); err != nil {
			t.Fatalf("settle rejected build after 403: %v", err)
		}
		remaining, err := h.db.GetMission(h.ctx, h.account.ID, "__growth")
		if err != nil {
			t.Fatalf("read rejected build after 403: %v", err)
		}
		if remaining == nil {
			break
		}
	}
	h.missingMission("__growth")
	if _, err := h.tick(runner); err != nil {
		t.Fatalf("third build planning: %v", err)
	}
	mission, data = h.mission("__growth")
	if data.Action == nil {
		t.Fatal("considerGrowth did not replan after the 403")
	}
	thirdAction := data.Action.ID
	if thirdAction == firstAction || thirdAction == secondAction {
		t.Fatal("no fresh build identity after two rejections")
	}

	h.api.setBuildMode("ok")
	if _, err := h.tick(runner); err != nil {
		t.Fatalf("repaired build dispatch: %v", err)
	}
	calls = h.api.mutationsFor("/v1/shipyard/build-ship")
	if len(calls) != 3 || calls[2].Status != http.StatusOK || calls[2].IdempotencyKey != thirdAction {
		t.Fatalf("repaired build outcome: %+v", calls)
	}
	var builtBody struct {
		PortID    string `json:"port_id"`
		Name      string `json:"name"`
		ShipClass string `json:"ship_class"`
		BuildMode string `json:"build_mode"`
	}
	if err := json.Unmarshal(calls[2].Body, &builtBody); err != nil {
		t.Fatal(err)
	}
	if builtBody.PortID != recoveryHomePort || builtBody.Name != data.BuildName ||
		builtBody.ShipClass != recoveryHull || builtBody.BuildMode != "turnkey_cash" {
		t.Fatalf("build request does not carry the durable intent: %+v", builtBody)
	}
	mission, data = h.mission("__growth")
	if mission.Phase != "growth_refit" || data.BuiltShipID != recoveryBuiltShipID {
		t.Fatalf("successful build did not advance the mission: phase %s built %s", mission.Phase, data.BuiltShipID)
	}
}

// A restored legacy growth_build intent without an action and without a built
// ship must not hold growth hostage: it waits out its cooldown and is then
// discarded so considerGrowth can run again. An attached ambiguous action, in
// contrast, is never discarded: it is replayed with its exact persisted
// identity and payload.
func TestRecoveryLegacyGrowthIntentRecoversAndKeepsAttachedActions(t *testing.T) {
	h := newRecoveryHarness(t)
	runner := h.runner(primaryTestKey)
	for i := range 2 {
		if _, err := h.tick(runner); err != nil {
			t.Fatalf("fleet recovery tick %d: %v", i+1, err)
		}
	}

	legacyData := missionData{
		Plan:             Plan{Kind: "spot", ShipID: "__growth", Origin: recoveryHomePort, Destination: recoveryFarPort, Commodity: recoveryGrain},
		BuildName:        "Bulbasaur",
		BuildClass:       recoveryHull,
		BuildPort:        recoveryHomePort,
		CooldownUntil:    time.Now().UTC().Add(time.Hour),
		CostsKnown:       true,
		InitialLedgerIDs: runner.ledgerIDs(),
		StartedAt:        time.Now().UTC(),
		StartedGameHours: runner.stateCache.Clock.Calendar.TotalGameHours,
	}
	h.putMission("__growth", "legacy-growth-generation", "growth_build", legacyData)

	if _, err := h.tick(runner); err != nil {
		t.Fatalf("legacy cooldown tick: %v", err)
	}
	retained, _ := h.mission("__growth")
	if retained.Generation != "legacy-growth-generation" {
		t.Fatalf("legacy intent replaced during its cooldown: %s", retained.Generation)
	}
	if h.api.mutationCount("/v1/shipyard/build-ship") != 0 {
		t.Fatal("legacy intent was dispatched during its cooldown")
	}

	legacyData.CooldownUntil = time.Now().UTC().Add(-time.Second)
	h.putMission("__growth", "legacy-growth-generation", "growth_build", legacyData)
	for pass := 0; pass < 8; pass++ {
		if _, err := h.tick(runner); err != nil {
			t.Fatalf("legacy recovery tick: %v", err)
		}
		remaining, err := h.db.GetMission(h.ctx, h.account.ID, "__growth")
		if err != nil {
			t.Fatalf("read legacy recovery intent: %v", err)
		}
		if remaining == nil {
			break
		}
	}
	h.missingMission("__growth")
	if h.api.mutationCount("/v1/shipyard/build-ship") != 0 {
		t.Fatal("discarded legacy intent was retried on the wire")
	}

	// With durable lane proofs available, fresh planning takes over.
	h.seedLaneProofs()
	if _, err := h.tick(runner); err != nil {
		t.Fatalf("fresh growth planning: %v", err)
	}
	fresh, freshData := h.mission("__growth")
	if fresh.Generation == "legacy-growth-generation" || freshData.Action == nil {
		t.Fatalf("considerGrowth did not replan: generation %s action %+v", fresh.Generation, freshData.Action)
	}

	// Replace the fresh intent with a legacy mission that carries an attached
	// ambiguous build action plus its pending journal row.
	h.api.setBuildMode("ok")
	attachedID := actionID(h.account.ID, "legacy-attached-generation", "build", 0)
	attachedBody := json.RawMessage(`{"port_id":"` + recoveryHomePort + `","name":"Bulbasaur","ship_class":"` + recoveryHull + `","build_mode":"turnkey_cash"}`)
	attachedData := missionData{
		Plan:          Plan{Kind: "spot", ShipID: "__growth", Origin: recoveryHomePort, Destination: recoveryFarPort, Commodity: recoveryGrain},
		BuildName:     "Bulbasaur",
		BuildClass:    recoveryHull,
		BuildPort:     recoveryHomePort,
		CooldownUntil: time.Now().UTC().Add(-time.Second),
		Action: &durableAction{
			ID:        attachedID,
			Stage:     "build",
			Method:    http.MethodPost,
			Path:      "/v1/shipyard/build-ship",
			Body:      attachedBody,
			CreatedAt: time.Now().UTC(),
		},
	}
	h.putMission("__growth", "legacy-attached-generation", "growth_build", attachedData)
	// Production stages and restores the mission before opening its journal
	// entry. JSONB normalizes nested JSON, so seed the immutable request from
	// that restored representation rather than the pre-storage fixture.
	_, restoredData := h.mission("__growth")
	attachedBody = restoredData.Action.Body
	if _, err := h.db.BeginAction(h.ctx, &store.Action{
		ID:        attachedID,
		AccountID: h.account.ID,
		Method:    http.MethodPost,
		Path:      "/v1/shipyard/build-ship",
		Body:      attachedBody,
	}); err != nil {
		t.Fatalf("seed legacy journal row: %v", err)
	}

	if _, err := h.tick(runner); err != nil {
		t.Fatalf("attached action dispatch: %v", err)
	}
	calls := h.api.mutationsFor("/v1/shipyard/build-ship")
	if len(calls) != 1 || calls[0].IdempotencyKey != attachedID || !bytes.Equal(calls[0].Body, attachedBody) {
		t.Fatalf("attached legacy action was not replayed verbatim: %+v", calls)
	}
	mission, data := h.mission("__growth")
	if mission.Phase != "growth_refit" || data.Action != nil {
		t.Fatalf("attached action outcome not consumed: phase %s action %+v", mission.Phase, data.Action)
	}
	if pending := h.pending(); len(pending) != 0 {
		t.Fatalf("attached action left journal state behind: %+v", pending)
	}

	t.Run("completed rejection with known accounting replans without replay", func(t *testing.T) {
		completed := newRecoveryHarness(t)
		oldGeneration := "legacy-completed-generation"
		oldID := actionID(completed.account.ID, oldGeneration, "build", 0)
		oldData := missionData{
			Plan: Plan{
				Kind:        "spot",
				ShipID:      "__growth",
				Origin:      recoveryHomePort,
				Destination: recoveryFarPort,
				Commodity:   recoveryGrain,
			},
			BuildName:        "Bulbasaur",
			BuildClass:       recoveryHull,
			BuildPort:        recoveryHomePort,
			CostsKnown:       true,
			InitialLedgerIDs: map[string]bool{},
			StartedAt:        time.Now().UTC().Add(-time.Minute),
			StartedGameHours: 2_400,
			Action: &durableAction{
				ID:        oldID,
				Stage:     "build",
				Method:    http.MethodPost,
				Path:      "/v1/shipyard/build-ship",
				Body:      json.RawMessage(`{"port_id":"` + recoveryHomePort + `","name":"Bulbasaur","ship_class":"` + recoveryHull + `","build_mode":"turnkey_cash"}`),
				CreatedAt: time.Now().UTC(),
			},
		}
		completed.putMission("__growth", oldGeneration, "growth_build", oldData)
		_, persistedData := completed.mission("__growth")
		oldRequest := &store.Action{
			ID:        oldID,
			AccountID: completed.account.ID,
			Method:    http.MethodPost,
			Path:      "/v1/shipyard/build-ship",
			Body:      persistedData.Action.Body,
		}
		if _, err := completed.db.BeginAction(completed.ctx, oldRequest); err != nil {
			t.Fatalf("seed completed request: %v", err)
		}
		oldResponse := json.RawMessage(`{"code":"auth_forbidden","detail":"old key lacked build permission"}`)
		if err := completed.db.CompleteAction(completed.ctx, oldID, http.StatusForbidden, oldResponse, nil); err != nil {
			t.Fatalf("seed completed refusal: %v", err)
		}

		restored := completed.runner(primaryTestKey)
		if _, err := completed.tick(restored); err == nil || !strings.Contains(err.Error(), "403") {
			t.Fatalf("completed authorization refusal did not retain auth stop: %v", err)
		}
		rejected, rejectedData := completed.mission("__growth")
		if rejected.Phase != "growth_rejected" || rejectedData.Action != nil || rejectedData.LastActionID != oldID {
			t.Fatalf("completed refusal was not consumed locally: phase %s action %+v", rejected.Phase, rejectedData.Action)
		}
		if completed.api.mutationCount("/v1/shipyard/build-ship") != 0 {
			t.Fatal("completed request was replayed over HTTP")
		}

		completed.seedLaneProofs()
		completed.api.replaceValidKeys(repairedTestKey)
		completed.expireCooldown("__growth")
		repaired := completed.runner(repairedTestKey)
		for pass := 0; pass < 8; pass++ {
			if _, err := completed.tick(repaired); err != nil {
				t.Fatalf("close completed legacy intent: %v", err)
			}
			remaining, err := completed.db.GetMission(completed.ctx, completed.account.ID, "__growth")
			if err != nil {
				t.Fatalf("read completed legacy intent: %v", err)
			}
			if remaining == nil {
				break
			}
		}
		completed.missingMission("__growth")
		if _, err := completed.tick(repaired); err != nil {
			t.Fatalf("replan completed legacy intent: %v", err)
		}
		_, freshData := completed.mission("__growth")
		if freshData.Action == nil || freshData.Action.ID == oldID {
			t.Fatalf("completed legacy action identity was reused: %+v", freshData.Action)
		}
		freshID := freshData.Action.ID
		if _, err := completed.tick(repaired); err != nil {
			t.Fatalf("dispatch fresh build intent: %v", err)
		}
		calls := completed.api.mutationsFor("/v1/shipyard/build-ship")
		if len(calls) != 1 || calls[0].IdempotencyKey != freshID || calls[0].Authorization != "Bearer "+repairedTestKey {
			t.Fatalf("repaired build did not use the new intent and credential: %+v", calls)
		}
		original, err := completed.db.BeginAction(completed.ctx, oldRequest)
		if err != nil || original.Status != http.StatusForbidden || !bytes.Equal(original.Response, oldResponse) {
			t.Fatalf("original completed journal outcome changed: %+v, %v", original, err)
		}
	})
}

// An ambiguous (transport-lost) request must be replayed across a restart with
// the exact original identity and payload, while a definitive 403 stops
// execution, retains journal and mission, and yields a fresh action identity
// once the credential is repaired.
func TestRecoveryRestartReplaysImmutableRequestThenFreshAfterAuthStop(t *testing.T) {
	h := newRecoveryHarness(t)
	runner := h.runner(primaryTestKey)
	for i := range 2 {
		if _, err := h.tick(runner); err != nil {
			t.Fatalf("fleet recovery tick %d: %v", i+1, err)
		}
	}

	h.api.setAutoRemit(false, 0)
	if _, err := h.tick(runner); err != nil {
		t.Fatalf("stage settings intent: %v", err)
	}
	_, stagedData := h.mission("__treasury")
	if stagedData.Action == nil {
		t.Fatal("no settings intent was staged")
	}
	stagedID := stagedData.Action.ID

	// Drop the connection mid-request so the outcome stays ambiguous.
	h.api.setSettingsMode("transport")
	mission, data := h.mission("__treasury")
	if _, err := runner.dispatch(h.ctx, mission, &data); err != nil {
		t.Fatalf("ambiguous dispatch returned an error: %v", err)
	}
	ambiguousCalls := h.api.mutationsFor("/v1/company/settings")
	if len(ambiguousCalls) == 0 {
		t.Fatal("the ambiguous attempt never reached the endpoint")
	}
	for _, call := range ambiguousCalls {
		if call.Status != 0 || call.IdempotencyKey != stagedID {
			t.Fatalf("unexpected settings traffic during the ambiguous window: %+v", ambiguousCalls)
		}
	}
	mission, data = h.mission("__treasury")
	if data.Action == nil || data.Action.ID != stagedID {
		t.Fatalf("ambiguous action not retained: %+v", data.Action)
	}
	pending := h.pending()
	if len(pending) != 1 || pending[0].ID != stagedID || !bytes.Equal(pending[0].Body, data.Action.Body) {
		t.Fatalf("journal row mismatch: %+v", pending)
	}

	// A restarted runner must replay the exact original request and key.
	h.api.setSettingsMode("ok")
	h.expireCooldown("__treasury")
	restarted := h.runner(primaryTestKey)
	mission, data = h.mission("__treasury")
	if _, err := restarted.dispatch(h.ctx, mission, &data); err != nil {
		t.Fatalf("replay dispatch: %v", err)
	}
	calls := h.api.mutationsFor("/v1/company/settings")
	replayed := calls[len(calls)-1]
	if replayed.Status != http.StatusOK || replayed.IdempotencyKey != stagedID || !bytes.Equal(replayed.Body, ambiguousCalls[0].Body) {
		t.Fatalf("replay did not reuse the immutable request: %+v", replayed)
	}
	mission, data = h.mission("__treasury")
	if mission.Phase != "done" || data.Action != nil || len(h.pending()) != 0 {
		t.Fatalf("replayed outcome not consumed: phase %s action %+v", mission.Phase, data.Action)
	}

	// A definitive 403 stops execution but retains mission and journal.
	h.api.setAutoRemit(false, 0)
	h.expireCooldown("__treasury")
	if _, err := h.tick(restarted); err != nil {
		t.Fatalf("settle replayed intent: %v", err)
	}
	h.missingMission("__treasury")
	if _, err := h.tick(restarted); err != nil {
		t.Fatalf("stage second settings intent: %v", err)
	}
	_, stagedData = h.mission("__treasury")
	if stagedData.Action == nil {
		t.Fatal("no second settings intent was staged")
	}
	secondID := stagedData.Action.ID
	if secondID == stagedID {
		t.Fatal("new intent reused the settled action identity")
	}

	h.api.setSettingsMode("forbid")
	_, workErr := h.tick(restarted)
	if workErr == nil || !strings.Contains(workErr.Error(), "403") {
		t.Fatalf("definitive 403 did not stop work: %v", workErr)
	}
	mission, data = h.mission("__treasury")
	calls = h.api.mutationsFor("/v1/company/settings")
	forbidden := calls[len(calls)-1]
	if forbidden.Status != http.StatusForbidden || forbidden.IdempotencyKey != secondID {
		t.Fatalf("forbidden settings outcome: %+v", forbidden)
	}
	if mission.Phase != "done" || data.Action != nil || len(h.pending()) != 0 {
		t.Fatalf("403 did not settle the intent: phase %s action %+v", mission.Phase, data.Action)
	}
	if !h.hasRecordedRequest(http.MethodPatch, "/v1/company/settings", http.StatusForbidden) {
		t.Fatal("the 403 journal evidence is not retained in request diagnostics")
	}

	// After the credential is repaired, the account replans with a fresh
	// identity on the new key while the old journal rows stay in place.
	h.api.replaceValidKeys(repairedTestKey)
	h.api.setSettingsMode("ok")
	h.expireCooldown("__treasury")
	repaired := h.runner(repairedTestKey)
	if _, err := h.tick(repaired); err != nil {
		t.Fatalf("repaired settle tick: %v", err)
	}
	h.missingMission("__treasury")
	if _, err := h.tick(repaired); err != nil {
		t.Fatalf("repaired staging tick: %v", err)
	}
	_, stagedData = h.mission("__treasury")
	if stagedData.Action == nil {
		t.Fatal("repaired runner staged no settings intent")
	}
	repairedID := stagedData.Action.ID
	if repairedID == stagedID || repairedID == secondID {
		t.Fatal("repaired runner reused an old action identity")
	}
	if _, err := h.tick(repaired); err != nil {
		t.Fatalf("repaired dispatch tick: %v", err)
	}
	calls = h.api.mutationsFor("/v1/company/settings")
	repairedCall := calls[len(calls)-1]
	if repairedCall.Status != http.StatusOK || repairedCall.IdempotencyKey != repairedID {
		t.Fatalf("repaired dispatch outcome: %+v", repairedCall)
	}
	if repairedCall.Authorization != "Bearer "+repairedTestKey {
		t.Fatalf("repaired dispatch did not use the repaired credential: %s", repairedCall.Authorization)
	}
}

// refresh() records every fleet name, including sunk hulls, in the persisted
// snapshot. The reservation survives a restart even after the sunk hull
// disappears from the later API fleet, and startGrowth's names.Next never
// hands that name to a new build.
func TestRecoverySunkShipNameSurvivesRestartAndIsNeverReused(t *testing.T) {
	h := newRecoveryHarness(t)
	h.api.setFleet([]map[string]any{
		sunkShipRow(recoveryShipLapras, "Lapras"),
		sailingShipRow(recoveryShipBlastoise, "Blastoise", recoveryHomePort, recoveryFarPort),
		sailingShipRow(recoveryShipCharizard, "Charizard", recoveryFarPort, recoveryHomePort),
	})
	runner := h.runner(primaryTestKey)
	for i := range 2 {
		if _, err := h.tick(runner); err != nil {
			t.Fatalf("fleet recovery tick %d: %v", i+1, err)
		}
	}

	used := h.usedShipNames()
	if !slices.Contains(used, "Lapras") {
		t.Fatalf("sunk ship name missing from the persisted snapshot: %v", used)
	}

	h.api.setFleet([]map[string]any{
		sailingShipRow(recoveryShipBlastoise, "Blastoise", recoveryHomePort, recoveryFarPort),
		sailingShipRow(recoveryShipCharizard, "Charizard", recoveryFarPort, recoveryHomePort),
	})
	h.seedLaneProofs()
	restarted := h.runner(primaryTestKey)
	if _, err := h.tick(restarted); err != nil {
		t.Fatalf("restarted growth tick: %v", err)
	}
	used = h.usedShipNames()
	if !slices.Contains(used, "Lapras") {
		t.Fatalf("restart recycled the sunk ship name: %v", used)
	}

	mission, data := h.mission("__growth")
	if data.Action == nil || data.BuildName == "" {
		t.Fatalf("no staged build intent: action %+v name %q", data.Action, data.BuildName)
	}
	// The vendored catalog starts at Lapras, then National Pokedex order, so
	// the first free name after Lapras/Blastoise/Charizard is Bulbasaur.
	expected := names.Next(slices.Concat(used, []string{"Blastoise", "Charizard"}))
	if data.BuildName != expected {
		t.Fatalf("build name %q, want %q from the actual catalog", data.BuildName, expected)
	}
	if data.BuildName == "Lapras" {
		t.Fatal("the sunk ship name was reused for a new build")
	}

	h.api.setBuildMode("ok")
	mission, data = h.mission("__growth")
	buildAction := data.Action.ID
	if _, err := restarted.dispatch(h.ctx, mission, &data); err != nil {
		t.Fatalf("build dispatch: %v", err)
	}
	calls := h.api.mutationsFor("/v1/shipyard/build-ship")
	if len(calls) != 1 || calls[0].IdempotencyKey != buildAction {
		t.Fatalf("build request identity: %+v", calls)
	}
	var builtBody struct {
		PortID    string `json:"port_id"`
		Name      string `json:"name"`
		ShipClass string `json:"ship_class"`
		BuildMode string `json:"build_mode"`
	}
	if err := json.Unmarshal(calls[0].Body, &builtBody); err != nil {
		t.Fatal(err)
	}
	if builtBody.Name != expected || builtBody.Name == "Lapras" {
		t.Fatalf("build request name %q does not respect the sunk reservation", builtBody.Name)
	}
	if builtBody.PortID != recoveryHomePort || builtBody.ShipClass != recoveryHull || builtBody.BuildMode != "turnkey_cash" {
		t.Fatalf("build request payload: %+v", builtBody)
	}
	mission, _ = h.mission("__growth")
	if mission.Phase != "growth_refit" {
		t.Fatalf("build outcome phase: %s", mission.Phase)
	}
}
