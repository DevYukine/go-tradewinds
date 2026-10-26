package engine

// Acceptance tests for plan R2: private market datasets need actual
// stationary presence, short sealed freight must not generate impossible new
// tenders, terminal freight states need explicit fresh REST authority, and
// delivery accounting stays bound to the immutable journaled request. The
// tests drive the real Runner seams against the existing fakeGameAPI harness;
// a thin wrapper around the fake adds contract rows and delivery outcomes the
// stock fake does not serve.

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/DevYukine/go-tradewinds/internal/store"
	"github.com/google/uuid"
)

type recordedGet struct {
	path   string
	portID string
}

// reconciliationProxy decorates the fake game API: it answers contract list
// queries from test-provided rows, records every GET path so presence-driven
// private fetches are countable, and implements the deliver-freight mutation
// outcomes the stock fake leaves unsupported.
type reconciliationProxy struct {
	mu                 sync.Mutex
	downstream         http.Handler
	recordMutation     func(request *http.Request, body []byte, status int)
	gets               []recordedGet
	contractRows       []map[string]any
	shipsUnavailable   bool
	factorsUnavailable bool
	factorOverride     bool
	factorPorts        []string
	deliverMode        string
	deliverLoseKey     string
}

func installReconciliationProxy(t *testing.T, h *recoveryHarness) *reconciliationProxy {
	t.Helper()
	proxy := &reconciliationProxy{downstream: h.api.server.Config.Handler}
	proxy.recordMutation = func(request *http.Request, body []byte, status int) {
		h.api.mu.Lock()
		h.api.record(request, body, status)
		h.api.mu.Unlock()
	}
	h.api.server.Config.Handler = proxy
	return proxy
}

func (p *reconciliationProxy) ServeHTTP(writer http.ResponseWriter, request *http.Request) {
	if request.Method == http.MethodGet {
		p.noteGet(request)
		if p.serveGet(writer, request) {
			return
		}
	}
	if request.Method == http.MethodPost && request.URL.Path == "/v1/contract/deliver-freight" {
		p.serveDeliverFreight(writer, request)
		return
	}
	p.downstream.ServeHTTP(writer, request)
}

func (p *reconciliationProxy) noteGet(request *http.Request) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.gets = append(p.gets, recordedGet{path: request.URL.Path, portID: request.URL.Query().Get("port_id")})
}

func (p *reconciliationProxy) serveGet(writer http.ResponseWriter, request *http.Request) bool {
	p.mu.Lock()
	shipsUnavailable := p.shipsUnavailable
	factorsUnavailable := p.factorsUnavailable
	factorOverride := p.factorOverride
	factorPorts := append([]string(nil), p.factorPorts...)
	contractRows := append([]map[string]any(nil), p.contractRows...)
	p.mu.Unlock()

	switch request.URL.Path {
	case "/v1/contract/list":
		status := request.URL.Query().Get("status")
		rows := make([]any, 0)
		for _, row := range contractRows {
			if row["status"] == status {
				rows = append(rows, row)
			}
		}
		p.writeJSON(writer, http.StatusOK, rows)
		return true
	case "/v1/fleet/ships":
		if shipsUnavailable {
			p.writeProblem(writer, http.StatusInternalServerError, "fleet_unavailable", "the fleet list refused this refresh")
			return true
		}
	case "/v1/company/factors":
		if factorsUnavailable {
			p.writeProblem(writer, http.StatusInternalServerError, "factors_unavailable", "the factor list refused this refresh")
			return true
		}
		if factorOverride {
			p.writeJSON(writer, http.StatusOK, factorPorts)
			return true
		}
	}
	return false
}

func (p *reconciliationProxy) serveDeliverFreight(writer http.ResponseWriter, request *http.Request) {
	body, err := io.ReadAll(request.Body)
	if err != nil {
		p.writeProblem(writer, http.StatusBadRequest, "unreadable_request", err.Error())
		return
	}
	p.mu.Lock()
	mode := p.deliverMode
	lose := mode == "transport_lost"
	if mode == "lose_first_key" {
		// The native client retries a transport loss with the identical
		// idempotency key, so the quarantined request must lose every attempt
		// of its key while every other key succeeds.
		key := request.Header.Get("Idempotency-Key")
		if p.deliverLoseKey == "" {
			p.deliverLoseKey = key
		}
		lose = key != "" && key == p.deliverLoseKey
	}
	p.mu.Unlock()

	if lose {
		p.recordMutation(request, body, 0)
		connection, _, err := writer.(http.Hijacker).Hijack()
		if err != nil {
			p.writeProblem(writer, http.StatusInternalServerError, "hijack_failed", err.Error())
			return
		}
		_ = connection.Close()
		return
	}

	var requested struct {
		ContractID string `json:"contract_id"`
	}
	_ = json.Unmarshal(body, &requested)
	switch mode {
	case "conflict":
		p.recordMutation(request, body, http.StatusConflict)
		p.writeProblem(writer, http.StatusConflict, "contract_defaulted", "contract freight-a is defaulted by the issuer; further delivery attempts are refused")
	case "capacity_conflict":
		p.recordMutation(request, body, http.StatusConflict)
		p.writeProblem(writer, http.StatusConflict, "warehouse_capacity", "the issuer warehouse capacity is exhausted for contract freight-a")
	case "wrong_contract":
		p.recordMutation(request, body, http.StatusOK)
		p.writeJSON(writer, http.StatusOK, deliverFreightSuccess("freight-b"))
	default:
		p.recordMutation(request, body, http.StatusOK)
		p.writeJSON(writer, http.StatusOK, deliverFreightSuccess(requested.ContractID))
	}
}

func (p *reconciliationProxy) countGets(path, portID string) int {
	p.mu.Lock()
	defer p.mu.Unlock()
	count := 0
	for _, get := range p.gets {
		if get.path == path && get.portID == portID {
			count++
		}
	}
	return count
}

func (p *reconciliationProxy) setContractRows(rows []map[string]any) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.contractRows = rows
}

func (p *reconciliationProxy) setShipRefreshFailed(unavailable bool) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.shipsUnavailable = unavailable
}

func (p *reconciliationProxy) setFactorOverride(ports []string) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.factorOverride = true
	p.factorPorts = ports
}

func (p *reconciliationProxy) setFactorRefreshFailed(unavailable bool) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.factorsUnavailable = unavailable
}

func (p *reconciliationProxy) setDeliverMode(mode string) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if mode == "lose_first_key" {
		p.deliverMode = "lose_first_key"
		p.deliverLoseKey = ""
		return
	}
	p.deliverMode = mode
}

func (p *reconciliationProxy) writeJSON(writer http.ResponseWriter, status int, document any) {
	payload, err := json.Marshal(document)
	if err != nil {
		http.Error(writer, err.Error(), http.StatusInternalServerError)
		return
	}
	writer.Header().Set("Content-Type", "application/json")
	writer.WriteHeader(status)
	_, _ = writer.Write(payload)
}

func (p *reconciliationProxy) writeProblem(writer http.ResponseWriter, status int, code, detail string) {
	p.writeJSON(writer, status, map[string]any{
		"type":   "about:blank",
		"title":  http.StatusText(status),
		"status": status,
		"code":   code,
		"detail": detail,
	})
}

// stationaryShipRow is a berthed or roadstead fleet row with optional sealed
// grain cargo, fully provisioned guns so idle refit maintenance never
// interferes with the scenario under test. Provisions stay an int because the
// fake's mutation handlers assert that concrete type.
func stationaryShipRow(id, name, port, status string, cargoTons int64, provisions int) map[string]any {
	cargo := map[string]any{}
	if cargoTons > 0 {
		cargo[recoveryGrain] = cargoTons
	}
	return map[string]any{
		"ship": map[string]any{
			"id":                    id,
			"name":                  name,
			"ship_class":            recoveryHull,
			"status":                status,
			"current_port_id":       port,
			"cannons_count":         4,
			"convoy_id":             nil,
			"turnaround_unlocks_at": nil,
			"dock_queued_at":        nil,
			"berthed_at":            nil,
		},
		"hold_capacity_tons":           60,
		"effective_hold_capacity_tons": 56,
		"cargo_used_tons":              cargoTons,
		"cargo_manifest":               cargo,
		"stores":                       map[string]any{"provisions_tons": provisions, "sea_days_remaining": 5},
		"route_legs":                   []any{},
		"voyage":                       nil,
	}
}

func freightContractRow(id, status string, stale bool) map[string]any {
	return freightContractRowFor(id, status, stale, recoveryShipBlastoise)
}

func freightContractRowFor(id, status string, stale bool, shipID string) map[string]any {
	return map[string]any{
		"id":                 id,
		"contract_type":      "freight",
		"origin_port_id":     recoveryHomePort,
		"dest_port_id":       recoveryFarPort,
		"commodity_id":       recoveryGrain,
		"quantity_required":  24,
		"quantity_delivered": 0,
		"remaining_quantity": 24,
		"reward_pence":       9_000,
		"collateral_pence":   5_000,
		"status":             status,
		"is_public":          false,
		"stale":              stale,
		"issuer_company_id":  "issuer-company",
		"carrier_company_id": recoveryCompanyID,
		"carrier_ship_id":    shipID,
		"expires_at":         nil,
		"metadata":           map[string]any{"delivery_deadline": time.Now().UTC().Add(6 * time.Hour).Format(time.RFC3339Nano)},
	}
}

func deliverFreightSuccess(contractID string) map[string]any {
	return map[string]any{
		"contract_id":        contractID,
		"commodity_id":       recoveryGrain,
		"delivered_quantity": 24,
		"reward_paid_pence":  9_000,
	}
}

func refreshedRunner(t *testing.T, h *recoveryHarness) *Runner {
	t.Helper()
	runner := h.runner(primaryTestKey)
	runner.generation = uuid.NewString()
	if err := runner.refresh(h.ctx); err != nil {
		t.Fatalf("refresh harness state: %v", err)
	}
	return runner
}

func harnessShip(t *testing.T, runner *Runner, shipID string) ShipState {
	t.Helper()
	ship, exists := runner.ship(shipID)
	if !exists {
		t.Fatalf("ship %s vanished from the refreshed fleet", shipID)
	}
	return ship
}

func findDecision(t *testing.T, h *recoveryHarness, shipID, needle string) store.Decision {
	t.Helper()
	rows, err := h.db.History(h.ctx, h.account.ID, "decisions", 200)
	if err != nil {
		t.Fatalf("load decisions: %v", err)
	}
	decisions, ok := rows.([]store.Decision)
	if !ok {
		t.Fatalf("unexpected decision history payload %T", rows)
	}
	for _, row := range decisions {
		if row.ShipID == shipID && strings.Contains(row.Reason, needle) {
			return row
		}
	}
	t.Fatalf("no %q decision recorded for %s across %d rows", needle, shipID, len(decisions))
	return store.Decision{}
}

// Presence is authorization for private datasets. It must come only from
// successful current-generation stationary ships (berthed or roadstead with a
// port and no voyage) or factor presence, never from a sailing ship's
// retained origin port, a durable mission's origin, or datasets retained from
// an earlier generation.
func TestPrivateDatasetsRequireCurrentStationaryPresence(t *testing.T) {
	t.Parallel()
	t.Run("a sailing ship does not authorize its retained origin port", func(t *testing.T) {
		h := newRecoveryHarness(t)
		proxy := installReconciliationProxy(t, h)
		h.api.setFleet([]map[string]any{
			sailingShipRow(recoveryShipBlastoise, "Blastoise", recoveryHomePort, recoveryFarPort),
		})
		refreshedRunner(t, h)

		if got := proxy.countGets("/v1/market/book", recoveryHomePort); got != 0 {
			t.Fatalf("sailing ship's retained port produced %d private book fetches", got)
		}
		if got := proxy.countGets("/v1/shipyard/quote", recoveryHomePort); got != 0 {
			t.Fatalf("sailing ship's retained port produced %d private yard quote fetches", got)
		}
	})

	t.Run("a durable mission origin does not authorize private books", func(t *testing.T) {
		h := newRecoveryHarness(t)
		proxy := installReconciliationProxy(t, h)
		h.api.setFleet([]map[string]any{
			stationaryShipRow(recoveryShipBlastoise, "Blastoise", recoveryFarPort, "berthed", 0, 3),
		})
		h.putMission(recoveryShipBlastoise, "presence-generation", "arrival", missionData{
			Plan:     Plan{Kind: "recovery", ShipID: recoveryShipBlastoise, Origin: recoveryHomePort, Destination: recoveryFarPort},
			Eligible: map[string]int64{},
		})
		refreshedRunner(t, h)

		if got := proxy.countGets("/v1/market/book", recoveryHomePort); got != 0 {
			t.Fatalf("durable mission origin produced %d private book fetches", got)
		}
		if got := proxy.countGets("/v1/market/book", recoveryFarPort); got < 1 {
			t.Fatal("the stationary ship's actual port lost its book capture")
		}
	})

	t.Run("a failed ships refresh stops authorizing retained berths", func(t *testing.T) {
		h := newRecoveryHarness(t)
		proxy := installReconciliationProxy(t, h)
		h.api.setFleet([]map[string]any{
			stationaryShipRow(recoveryShipBlastoise, "Blastoise", recoveryHomePort, "berthed", 0, 3),
		})
		runner := refreshedRunner(t, h)
		before := proxy.countGets("/v1/market/book", recoveryHomePort)
		if before < 1 {
			t.Fatal("the stationary berth did not capture its book in the healthy generation")
		}

		proxy.setShipRefreshFailed(true)
		runner.generation = uuid.NewString()
		if err := runner.refresh(h.ctx); err != nil {
			t.Fatalf("refresh with failed ships dataset: %v", err)
		}
		if after := proxy.countGets("/v1/market/book", recoveryHomePort); after != before {
			t.Fatalf("failed ships refresh still authorized %d new private fetches from retained berths", after-before)
		}
	})

	t.Run("a failed factor refresh stops authorizing factor ports", func(t *testing.T) {
		h := newRecoveryHarness(t)
		proxy := installReconciliationProxy(t, h)
		h.api.setFleet([]map[string]any{})
		proxy.setFactorOverride([]string{recoveryHomePort})
		runner := refreshedRunner(t, h)
		before := proxy.countGets("/v1/market/book", recoveryHomePort)
		if before < 1 {
			t.Fatal("declared factor presence did not capture the port book in the healthy generation")
		}

		proxy.setFactorRefreshFailed(true)
		runner.generation = uuid.NewString()
		if err := runner.refresh(h.ctx); err != nil {
			t.Fatalf("refresh with failed factors dataset: %v", err)
		}
		if after := proxy.countGets("/v1/market/book", recoveryHomePort); after != before {
			t.Fatalf("failed factor refresh still authorized %d new private fetches", after-before)
		}
	})

	t.Run("roadstead presence still authorizes books and yard quotes", func(t *testing.T) {
		h := newRecoveryHarness(t)
		proxy := installReconciliationProxy(t, h)
		h.api.setFleet([]map[string]any{
			stationaryShipRow(recoveryShipBlastoise, "Blastoise", recoveryHomePort, "anchored_roadstead", 0, 3),
		})
		refreshedRunner(t, h)

		if got := proxy.countGets("/v1/market/book", recoveryHomePort); got < 1 {
			t.Fatal("legitimate roadstead presence lost its book capture")
		}
		if got := proxy.countGets("/v1/shipyard/quote", recoveryHomePort); got < 1 {
			t.Fatal("legitimate roadstead presence lost its yard quote capture")
		}
	})
}

// A sealed obligation's full accepted quantity is the precondition for a new
// tender. Carrying 18 of 24 tons must produce zero new delivery requests,
// must keep safe maintenance running while held, must keep the full seal
// reservation (never lowered quantities, never partial tenders), and must
// report the aggregate deficit and collateral at risk when several
// same-commodity obligations share one hold.
func TestShortSealedFreightHoldsNewTendersAndKeepsMaintenance(t *testing.T) {
	t.Parallel()
	h := newRecoveryHarness(t)
	proxy := installReconciliationProxy(t, h)
	proxy.setContractRows([]map[string]any{freightContractRow("freight-a", "active", false)})

	obligationA := freightObligation{
		ContractID:      "freight-a",
		Commodity:       recoveryGrain,
		Quantity:        24,
		Destination:     recoveryFarPort,
		Deadline:        new(time.Now().UTC().Add(6 * time.Hour)),
		RewardPence:     9_000,
		CollateralPence: 5_000,
	}
	arrivalMission := func(freight []freightObligation) missionData {
		return missionData{
			Plan:     Plan{Kind: "recovery", ShipID: recoveryShipBlastoise, Destination: recoveryFarPort},
			Freight:  freight,
			Eligible: map[string]int64{},
		}
	}

	// Full accepted quantity still tenders normally.
	h.api.setFleet([]map[string]any{
		stationaryShipRow(recoveryShipBlastoise, "Blastoise", recoveryFarPort, "berthed", 24, 3),
	})
	runner := refreshedRunner(t, h)
	h.putMission(recoveryShipBlastoise, "freight-generation", "arrival", arrivalMission([]freightObligation{obligationA}))
	mission, data := h.mission(recoveryShipBlastoise)
	ship := harnessShip(t, runner, recoveryShipBlastoise)
	if _, err := runner.driveMission(h.ctx, ship, mission, &data); err != nil {
		t.Fatalf("drive full-cargo mission: %v", err)
	}
	if data.Action == nil || data.Action.Stage != "deliver-freight" {
		t.Fatalf("full sealed cargo did not stage its tender: %+v", data.Action)
	}

	// 18 of 24: maintenance replaces the impossible tender.
	h.api.setFleet([]map[string]any{
		stationaryShipRow(recoveryShipBlastoise, "Blastoise", recoveryFarPort, "berthed", 18, 0),
	})
	h.putMission(recoveryShipBlastoise, "freight-generation", "arrival", arrivalMission([]freightObligation{obligationA}))
	runner.generation = uuid.NewString()
	if err := runner.refresh(h.ctx); err != nil {
		t.Fatalf("refresh short-cargo state: %v", err)
	}
	mission, data = h.mission(recoveryShipBlastoise)
	ship = harnessShip(t, runner, recoveryShipBlastoise)
	if _, err := runner.driveMission(h.ctx, ship, mission, &data); err != nil {
		t.Fatalf("drive short-cargo mission: %v", err)
	}
	if h.api.mutationCount("/v1/contract/deliver-freight") != 0 {
		t.Fatal("short sealed cargo still produced a tender request")
	}
	if data.Action == nil || data.Action.Stage != "provision" {
		t.Fatalf("shortfall hold dropped safe maintenance: %+v", data.Action)
	}

	// With stores safe the ship holds with the explicit aggregate deficit of
	// both same-commodity obligations: required 36, carried 18, short 18.
	h.api.setFleet([]map[string]any{
		stationaryShipRow(recoveryShipBlastoise, "Blastoise", recoveryFarPort, "berthed", 18, 3),
	})
	obligationB := obligationA
	obligationB.ContractID = "freight-b"
	obligationB.Quantity = 12
	obligationB.CollateralPence = 3_000
	h.putMission(recoveryShipBlastoise, "freight-generation", "arrival", arrivalMission([]freightObligation{obligationA, obligationB}))
	runner.generation = uuid.NewString()
	if err := runner.refresh(h.ctx); err != nil {
		t.Fatalf("refresh aggregate state: %v", err)
	}
	mission, data = h.mission(recoveryShipBlastoise)
	ship = harnessShip(t, runner, recoveryShipBlastoise)
	if _, err := runner.driveMission(h.ctx, ship, mission, &data); err != nil {
		t.Fatalf("drive aggregate mission: %v", err)
	}
	if h.api.mutationCount("/v1/contract/deliver-freight") != 0 {
		t.Fatal("aggregate shortfall still produced a tender request")
	}
	if holding := joinedHoldReasons(runner.holding); !strings.Contains(holding, "shortfall") {
		t.Fatalf("aggregate shortfall was not held as a shortfall: %q", holding)
	}
	decision := findDecision(t, h, recoveryShipBlastoise, "shortfall")
	for _, needle := range []string{"36", "18"} {
		if !strings.Contains(decision.Reason, needle) {
			t.Fatalf("shortfall decision %q omits aggregate figure %s", decision.Reason, needle)
		}
	}

	// The seals stay whole: accepted quantities were never lowered.
	mission, data = h.mission(recoveryShipBlastoise)
	if data.Freight[0].Quantity != 24 || data.Freight[1].Quantity != 12 {
		t.Fatalf("shortfall hold lowered accepted quantities: %+v", data.Freight)
	}
	for _, obligation := range data.Freight {
		if obligation.Settled {
			t.Fatalf("shortfall hold settled %s without delivery", obligation.ContractID)
		}
	}
}

// Only an explicit fresh REST record naming the persisted contract settles an
// unsettled obligation. open, missing, unknown and stale records stay
// unresolved and never authorize a new tender, a 409's prose never proves a
// terminal state, and a default's collateral loss is booked exactly once.
func TestFreightTerminalReconciliationRequiresExplicitFreshRESTState(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name          string
		rowStatus     string
		rowStale      bool
		includeRow    bool
		wantSettled   bool
		wantLossPence int64
	}{
		{name: "explicit defaulted records the collateral loss once", rowStatus: "defaulted", includeRow: true, wantSettled: true, wantLossPence: 5_000},
		{name: "explicit completed settles without invented proceeds", rowStatus: "completed", includeRow: true, wantSettled: true},
		{name: "explicit cancelled settles without invented proceeds", rowStatus: "cancelled", includeRow: true, wantSettled: true},
		{name: "open status is not terminal authority", rowStatus: "open", includeRow: true},
		{name: "missing contract record stays unresolved", includeRow: false},
		{name: "stale terminal record is not authoritative", rowStatus: "defaulted", rowStale: true, includeRow: true},
	}
	for _, test := range cases {
		t.Run(test.name, func(t *testing.T) {
			h := newRecoveryHarness(t)
			proxy := installReconciliationProxy(t, h)
			rows := []map[string]any{}
			if test.includeRow {
				rows = append(rows, freightContractRow("freight-a", test.rowStatus, test.rowStale))
			}
			proxy.setContractRows(rows)
			h.api.setFleet([]map[string]any{
				stationaryShipRow(recoveryShipBlastoise, "Blastoise", recoveryFarPort, "berthed", 24, 3),
			})
			runner := refreshedRunner(t, h)

			data := missionData{
				Plan: Plan{Kind: "recovery", ShipID: recoveryShipBlastoise, Destination: recoveryFarPort},
				Freight: []freightObligation{{
					ContractID:      "freight-a",
					Commodity:       recoveryGrain,
					Quantity:        24,
					Destination:     recoveryFarPort,
					Deadline:        new(time.Now().UTC().Add(6 * time.Hour)),
					RewardPence:     9_000,
					CollateralPence: 5_000,
				}},
				Eligible: map[string]int64{},
			}
			h.putMission(recoveryShipBlastoise, "terminal-generation", "arrival", data)
			mission, data := h.mission(recoveryShipBlastoise)
			ship := harnessShip(t, runner, recoveryShipBlastoise)
			if _, err := runner.driveMission(h.ctx, ship, mission, &data); err != nil {
				t.Fatalf("drive terminal mission: %v", err)
			}

			mission, data = h.mission(recoveryShipBlastoise)
			if data.Freight[0].Settled != test.wantSettled {
				t.Fatalf("obligation settled = %v, want %v", data.Freight[0].Settled, test.wantSettled)
			}
			if data.ExpensesPence != test.wantLossPence {
				t.Fatalf("booked loss = %d, want %d", data.ExpensesPence, test.wantLossPence)
			}
			if test.wantSettled && data.CostsKnown {
				t.Fatal("terminal settlement claimed the unobserved payment as a known cost")
			}
			if h.api.mutationCount("/v1/contract/deliver-freight") != 0 {
				t.Fatal("terminal reconciliation produced a new tender request")
			}

			if test.wantSettled && test.rowStatus == "defaulted" {
				restarted := refreshedRunner(t, h)
				mission, data = h.mission(recoveryShipBlastoise)
				ship = harnessShip(t, restarted, recoveryShipBlastoise)
				if _, err := restarted.driveMission(h.ctx, ship, mission, &data); err != nil {
					t.Fatalf("drive restarted terminal mission: %v", err)
				}
				row, err := h.db.GetMission(h.ctx, h.account.ID, recoveryShipBlastoise)
				if err != nil {
					t.Fatalf("reload settled mission: %v", err)
				}
				if row != nil {
					var retained missionData
					if err := json.Unmarshal(row.Data, &retained); err != nil {
						t.Fatal(err)
					}
					if retained.ExpensesPence != 5_000 || !retained.Freight[0].Settled {
						t.Fatalf("defaulted collateral was not booked exactly once: %+v", retained)
					}
				}
			}
		})
	}

	t.Run("misleading 409 prose never proves a default and never deadlocks the mission", func(t *testing.T) {
		h := newRecoveryHarness(t)
		proxy := installReconciliationProxy(t, h)
		proxy.setContractRows([]map[string]any{freightContractRow("freight-a", "active", false)})
		proxy.setDeliverMode("conflict")
		h.api.setFleet([]map[string]any{
			stationaryShipRow(recoveryShipBlastoise, "Blastoise", recoveryFarPort, "berthed", 24, 3),
		})
		runner := refreshedRunner(t, h)

		data := missionData{
			Plan: Plan{Kind: "recovery", ShipID: recoveryShipBlastoise, Destination: recoveryFarPort},
			Freight: []freightObligation{{
				ContractID:      "freight-a",
				Commodity:       recoveryGrain,
				Quantity:        24,
				Destination:     recoveryFarPort,
				Deadline:        new(time.Now().UTC().Add(6 * time.Hour)),
				RewardPence:     9_000,
				CollateralPence: 5_000,
			}},
			Eligible: map[string]int64{},
		}
		h.putMission(recoveryShipBlastoise, "prose-generation", "arrival", data)
		mission, data := h.mission(recoveryShipBlastoise)
		ship := harnessShip(t, runner, recoveryShipBlastoise)
		if _, err := runner.driveMission(h.ctx, ship, mission, &data); err != nil {
			t.Fatalf("stage prose-window tender: %v", err)
		}
		mission, data = h.mission(recoveryShipBlastoise)
		if _, err := runner.dispatch(h.ctx, mission, &data); err != nil {
			t.Fatalf("dispatch prose-window tender: %v", err)
		}
		mission, data = h.mission(recoveryShipBlastoise)
		if data.Freight[0].Settled || data.ExpensesPence != 0 || data.ProceedsPence != 0 {
			t.Fatalf("409 prose settled the obligation: %+v", data)
		}
		if data.Action != nil || data.LastActionID == "" {
			t.Fatalf("definite 409 was not consumed as a request rejection: %+v", data)
		}

		// Liveness with unchanged authoritative REST: the refused tender must
		// leave durable bounded memory, never re-stage the identical request,
		// and stay visible as a hold. Re-staging here was a permanent AFK
		// self-deadlock of hourly identical tenders.
		h.expireCooldown(recoveryShipBlastoise)
		runner.generation = uuid.NewString()
		if err := runner.refresh(h.ctx); err != nil {
			t.Fatalf("refresh after refusal: %v", err)
		}
		mission, data = h.mission(recoveryShipBlastoise)
		ship = harnessShip(t, runner, recoveryShipBlastoise)
		if _, err := runner.driveMission(h.ctx, ship, mission, &data); err != nil {
			t.Fatalf("drive refused mission: %v", err)
		}
		mission, data = h.mission(recoveryShipBlastoise)
		if data.Action != nil {
			t.Fatalf("refused tender was re-staged against unchanged state: %+v", data.Action)
		}
		if len(runner.holding) == 0 {
			t.Fatal("refused tender left no visible hold; the ship would silently stall")
		}
		if data.Freight[0].Settled || data.ExpensesPence != 0 || data.ProceedsPence != 0 {
			t.Fatalf("refusal mutated the obligation: %+v", data)
		}

		// The refusal memory survives a restart: still no new tender intent.
		restarted := refreshedRunner(t, h)
		h.expireCooldown(recoveryShipBlastoise)
		mission, data = h.mission(recoveryShipBlastoise)
		ship = harnessShip(t, restarted, recoveryShipBlastoise)
		if _, err := restarted.driveMission(h.ctx, ship, mission, &data); err != nil {
			t.Fatalf("drive restarted refused mission: %v", err)
		}
		mission, data = h.mission(recoveryShipBlastoise)
		if data.Action != nil {
			t.Fatalf("restart lost the refusal memory and re-staged the tender: %+v", data.Action)
		}
		if data.Freight[0].Settled || data.Freight[0].ContractID != "freight-a" {
			t.Fatalf("refused obligation was not retained across restart: %+v", data.Freight)
		}

		// A later authoritative default then progresses the mission to a
		// once-only settlement without any manual repair, and no further
		// tender ever reaches the wire.
		proxy.setContractRows([]map[string]any{freightContractRow("freight-a", "defaulted", false)})
		h.expireCooldown(recoveryShipBlastoise)
		restarted.generation = uuid.NewString()
		if err := restarted.refresh(h.ctx); err != nil {
			t.Fatalf("refresh after authoritative default: %v", err)
		}
		mission, data = h.mission(recoveryShipBlastoise)
		ship = harnessShip(t, restarted, recoveryShipBlastoise)
		if _, err := restarted.driveMission(h.ctx, ship, mission, &data); err != nil {
			t.Fatalf("drive post-default reconciliation: %v", err)
		}
		mission, data = h.mission(recoveryShipBlastoise)
		if !data.Freight[0].Settled || data.ExpensesPence != 5_000 {
			t.Fatalf("explicit fresh default did not settle exactly once: %+v", data)
		}
		ship = harnessShip(t, restarted, recoveryShipBlastoise)
		if _, err := restarted.driveMission(h.ctx, ship, mission, &data); err != nil {
			t.Fatalf("drive settled mission onward: %v", err)
		}
		mission, data = h.mission(recoveryShipBlastoise)
		if data.Action != nil && data.Action.Stage == "deliver-freight" {
			t.Fatalf("settled mission produced a new tender: %+v", data.Action)
		}
		if data.ExpensesPence != 5_000 {
			t.Fatalf("default loss was booked twice: %d", data.ExpensesPence)
		}
		if h.api.mutationCount("/v1/contract/deliver-freight") != 1 {
			t.Fatalf("liveness flow changed the tender traffic: %+v", h.api.mutationsFor("/v1/contract/deliver-freight"))
		}
	})
}

// Delivery accounting is bound to the immutable journaled request. A stored
// completed outcome is consumed before any current-state guard, a response
// naming another contract credits nothing, ambiguous attempts replay their
// original bytes and key even with an empty hold, and an orphaned pending
// request blocks every account mutation.
func TestDeliveryOutcomeAccountingBindsToImmutableRequest(t *testing.T) {
	t.Parallel()
	freightFixture := func() []freightObligation {
		return []freightObligation{
			{ContractID: "freight-a", Commodity: recoveryGrain, Quantity: 24, Destination: recoveryFarPort, Deadline: new(time.Now().UTC().Add(6 * time.Hour)), RewardPence: 9_000, CollateralPence: 5_000},
		}
	}
	arrivalData := func(action *durableAction, freight []freightObligation) missionData {
		return missionData{
			Plan:     Plan{Kind: "recovery", ShipID: recoveryShipBlastoise, Destination: recoveryFarPort},
			Freight:  freight,
			Eligible: map[string]int64{},
			Action:   action,
		}
	}

	t.Run("stored completed delivery is consumed before current-state guards", func(t *testing.T) {
		h := newRecoveryHarness(t)
		proxy := installReconciliationProxy(t, h)
		proxy.setContractRows([]map[string]any{freightContractRow("freight-a", "active", false)})
		// The hold is empty now: a fresh tender would be impossible, but the
		// stored success belongs to a request the game already answered.
		h.api.setFleet([]map[string]any{
			stationaryShipRow(recoveryShipBlastoise, "Blastoise", recoveryFarPort, "berthed", 0, 3),
		})
		runner := refreshedRunner(t, h)

		completed := &durableAction{
			ID:               actionID(h.account.ID, "freight-generation", "deliver-freight", 0),
			Stage:            "deliver-freight",
			Method:           http.MethodPost,
			Path:             "/v1/contract/deliver-freight",
			Body:             json.RawMessage(`{"contract_id":"freight-a","ship_id":"` + recoveryShipBlastoise + `"}`),
			CreatedAt:        time.Now().UTC(),
			FinancialSession: runner.financialSession,
			FinancialEpoch:   runner.capturedFinancial,
		}
		h.putMission(recoveryShipBlastoise, "freight-generation", "arrival", arrivalData(completed, freightFixture()))
		_, restored := h.mission(recoveryShipBlastoise)
		if _, err := h.db.BeginAction(h.ctx, &store.Action{
			ID:        completed.ID,
			AccountID: h.account.ID,
			Method:    http.MethodPost,
			Path:      "/v1/contract/deliver-freight",
			Body:      restored.Action.Body,
		}); err != nil {
			t.Fatalf("seed completed journal row: %v", err)
		}
		response, err := json.Marshal(deliverFreightSuccess("freight-a"))
		if err != nil {
			t.Fatal(err)
		}
		if err := h.db.CompleteAction(h.ctx, completed.ID, http.StatusOK, response, nil); err != nil {
			t.Fatalf("store completed outcome: %v", err)
		}

		mission, data := h.mission(recoveryShipBlastoise)
		if _, err := runner.dispatch(h.ctx, mission, &data); err != nil {
			t.Fatalf("consume stored delivery: %v", err)
		}
		mission, data = h.mission(recoveryShipBlastoise)
		if !data.Freight[0].Settled || data.ProceedsPence != 9_000 {
			t.Fatalf("stored success was not consumed before the empty-hold guard: %+v", data)
		}
		if h.api.mutationCount("/v1/contract/deliver-freight") != 0 {
			t.Fatal("a stored outcome was re-sent over the wire")
		}
	})

	t.Run("a response for another contract credits nothing", func(t *testing.T) {
		h := newRecoveryHarness(t)
		proxy := installReconciliationProxy(t, h)
		proxy.setContractRows([]map[string]any{freightContractRow("freight-a", "active", false)})
		proxy.setDeliverMode("wrong_contract")
		h.api.setFleet([]map[string]any{
			stationaryShipRow(recoveryShipBlastoise, "Blastoise", recoveryFarPort, "berthed", 24, 3),
		})
		runner := refreshedRunner(t, h)

		bundled := freightFixture()
		bundled = append(bundled, freightObligation{
			ContractID:      "freight-b",
			Commodity:       recoveryGrain,
			Quantity:        12,
			Destination:     recoveryFarPort,
			Deadline:        new(time.Now().UTC().Add(6 * time.Hour)),
			RewardPence:     4_000,
			CollateralPence: 3_000,
		})
		requested := &durableAction{
			ID:               actionID(h.account.ID, "freight-generation", "deliver-freight", 0),
			Stage:            "deliver-freight",
			Method:           http.MethodPost,
			Path:             "/v1/contract/deliver-freight",
			Body:             json.RawMessage(`{"contract_id":"freight-a","ship_id":"` + recoveryShipBlastoise + `"}`),
			CreatedAt:        time.Now().UTC(),
			FinancialSession: runner.financialSession,
			FinancialEpoch:   runner.capturedFinancial,
		}
		h.putMission(recoveryShipBlastoise, "freight-generation", "arrival", arrivalData(requested, bundled))
		mission, data := h.mission(recoveryShipBlastoise)
		if _, err := runner.dispatch(h.ctx, mission, &data); err != nil {
			t.Fatalf("dispatch mismatched delivery: %v", err)
		}

		mission, data = h.mission(recoveryShipBlastoise)
		if data.ProceedsPence != 0 {
			t.Fatalf("a response for freight-b credited %d pence to freight-a's request", data.ProceedsPence)
		}
		for _, obligation := range data.Freight {
			if obligation.Settled {
				t.Fatalf("mismatched response settled %s", obligation.ContractID)
			}
		}
		if h.api.mutationCount("/v1/contract/deliver-freight") != 1 {
			t.Fatalf("mismatched delivery did not reach the wire exactly once: %+v", h.api.mutationsFor("/v1/contract/deliver-freight"))
		}
	})

	t.Run("ambiguous attempt replays original bytes even with empty cargo", func(t *testing.T) {
		h := newRecoveryHarness(t)
		proxy := installReconciliationProxy(t, h)
		proxy.setContractRows([]map[string]any{freightContractRow("freight-a", "active", false)})
		proxy.setDeliverMode("transport_lost")
		h.api.setFleet([]map[string]any{
			stationaryShipRow(recoveryShipBlastoise, "Blastoise", recoveryFarPort, "berthed", 24, 3),
		})
		runner := refreshedRunner(t, h)
		h.putMission(recoveryShipBlastoise, "freight-generation", "arrival", arrivalData(nil, freightFixture()))
		mission, data := h.mission(recoveryShipBlastoise)
		ship := harnessShip(t, runner, recoveryShipBlastoise)
		if _, err := runner.driveMission(h.ctx, ship, mission, &data); err != nil {
			t.Fatalf("stage tender: %v", err)
		}
		mission, data = h.mission(recoveryShipBlastoise)
		originalID := data.Action.ID
		if _, err := runner.dispatch(h.ctx, mission, &data); err != nil {
			t.Fatalf("ambiguous delivery dispatch: %v", err)
		}
		pending := h.pending()
		if len(pending) != 1 || pending[0].ID != originalID {
			t.Fatalf("ambiguous request was not durably retained: %+v", pending)
		}
		originalBody := pending[0].Body

		// The hold empties before the outcome is learned; replay safety must
		// not depend on current cargo.
		h.api.setFleet([]map[string]any{
			stationaryShipRow(recoveryShipBlastoise, "Blastoise", recoveryFarPort, "berthed", 0, 3),
		})
		h.expireCooldown(recoveryShipBlastoise)
		proxy.setDeliverMode("success")
		restarted := refreshedRunner(t, h)
		mission, data = h.mission(recoveryShipBlastoise)
		if _, err := restarted.dispatch(h.ctx, mission, &data); err != nil {
			t.Fatalf("replay ambiguous delivery: %v", err)
		}

		calls := h.api.mutationsFor("/v1/contract/deliver-freight")
		// The client retries a transport loss under the same key, so every
		// attempt before the last must be an identical status-0 loss; the
		// final attempt is the replay that succeeded.
		if len(calls) < 2 {
			t.Fatalf("expected the lost attempt and its replay on the wire: %+v", calls)
		}
		for _, call := range calls[:len(calls)-1] {
			if call.Status != 0 || call.IdempotencyKey != originalID || !bytes.Equal(call.Body, originalBody) {
				t.Fatalf("lost attempts were not identical transport losses: %+v", calls)
			}
		}
		replayed := calls[len(calls)-1]
		if replayed.Status != http.StatusOK || replayed.IdempotencyKey != originalID || !bytes.Equal(replayed.Body, originalBody) {
			t.Fatalf("replay changed the immutable request: key %s body %s", replayed.IdempotencyKey, replayed.Body)
		}
		mission, data = h.mission(recoveryShipBlastoise)
		if !data.Freight[0].Settled || data.ProceedsPence != 9_000 {
			t.Fatalf("replayed delivery outcome was not consumed: %+v", data)
		}
	})

	t.Run("orphaned pending request blocks all account mutations", func(t *testing.T) {
		h := newRecoveryHarness(t)
		installReconciliationProxy(t, h)
		runner := refreshedRunner(t, h)
		orphanID := actionID(h.account.ID, "orphan-generation", "deliver-freight", 0)
		if _, err := h.db.BeginAction(h.ctx, &store.Action{
			ID:        orphanID,
			AccountID: h.account.ID,
			Method:    http.MethodPost,
			Path:      "/v1/contract/deliver-freight",
			Body:      json.RawMessage(`{"contract_id":"freight-a","ship_id":"ghost-ship"}`),
		}); err != nil {
			t.Fatalf("seed orphan journal row: %v", err)
		}

		if _, err := h.tick(runner); err != nil {
			t.Fatalf("tick with orphaned request: %v", err)
		}
		holding := joinedHoldReasons(runner.holding)
		if !strings.Contains(holding, "orphaned unresolved journal request") {
			t.Fatalf("orphaned request did not block work: %q", holding)
		}
		for _, path := range []string{"/v1/contract/deliver-freight", "/v1/fleet/depart", "/v1/fleet/dock", "/v1/market/place-order"} {
			if h.api.mutationCount(path) != 0 {
				t.Fatalf("orphaned request did not block %s mutations", path)
			}
		}
	})
}

// expireTenderRetry clears the durable bounded tender retry deadline from the
// persisted mission. The deadline lives inside each freight obligation, so the
// helper rewrites the "freight" array at the JSON level: the frozen acceptance
// test pins the agreed "tender_retry_at" key without compile-coupling to a
// source field that the liveness implementation introduces.
func expireTenderRetry(t *testing.T, h *recoveryHarness, shipID string) {
	t.Helper()
	mission, err := h.db.GetMission(h.ctx, h.account.ID, shipID)
	if err != nil {
		t.Fatalf("load mission %s: %v", shipID, err)
	}
	if mission == nil {
		t.Fatalf("mission %s is missing", shipID)
	}
	var payload map[string]json.RawMessage
	if err := json.Unmarshal(mission.Data, &payload); err != nil {
		t.Fatalf("decode mission %s: %v", shipID, err)
	}
	var obligations []map[string]json.RawMessage
	if raw, exists := payload["freight"]; exists {
		if err := json.Unmarshal(raw, &obligations); err != nil {
			t.Fatalf("decode freight obligations of %s: %v", shipID, err)
		}
	}
	if len(obligations) == 0 {
		t.Fatalf("mission %s carries no freight obligations to expire", shipID)
	}
	for _, obligation := range obligations {
		delete(obligation, "tender_retry_at")
	}
	freight, err := json.Marshal(obligations)
	if err != nil {
		t.Fatal(err)
	}
	payload["freight"] = freight
	mission.Data, err = json.Marshal(payload)
	if err != nil {
		t.Fatal(err)
	}
	if err := h.db.PutMission(h.ctx, mission); err != nil {
		t.Fatalf("expire tender retry deadline: %v", err)
	}
}

func charterContractRow(id string, remainingQuantity int64) map[string]any {
	return map[string]any{
		"id":                   id,
		"contract_type":        "charter",
		"origin_port_id":       recoveryFarPort,
		"dest_port_id":         recoveryHomePort,
		"commodity_id":         recoveryGrain,
		"quantity_required":    remainingQuantity,
		"quantity_delivered":   0,
		"remaining_quantity":   remainingQuantity,
		"reward_pence":         remainingQuantity * 3_000,
		"reward_per_ton_pence": 3_000,
		"collateral_pence":     0,
		"status":               "open",
		"is_public":            true,
		"stale":                false,
		"issuer_company_id":    "issuer-company",
		"carrier_company_id":   "",
		"carrier_ship_id":      "",
		"expires_at":           nil,
		"metadata":             map[string]any{},
	}
}

// A capacity refusal is definite for that request but never terminal for the
// contract: the tender must stay bounded (no identical re-stage while the
// durable retry deadline holds, including across a restart) and must rearm
// with a fresh identity once the host retry deadline has passed and fresh
// REST still shows the matching active contract with the full manifest.
func TestRefusedTenderRetriesBoundedAndRearmsOnFreshState(t *testing.T) {
	t.Parallel()
	h := newRecoveryHarness(t)
	proxy := installReconciliationProxy(t, h)
	proxy.setContractRows([]map[string]any{freightContractRow("freight-a", "active", false)})
	proxy.setDeliverMode("capacity_conflict")
	h.api.setFleet([]map[string]any{
		stationaryShipRow(recoveryShipBlastoise, "Blastoise", recoveryFarPort, "berthed", 24, 3),
	})
	runner := refreshedRunner(t, h)

	data := missionData{
		Plan: Plan{Kind: "recovery", ShipID: recoveryShipBlastoise, Destination: recoveryFarPort},
		Freight: []freightObligation{{
			ContractID:      "freight-a",
			Commodity:       recoveryGrain,
			Quantity:        24,
			Destination:     recoveryFarPort,
			Deadline:        new(time.Now().UTC().Add(6 * time.Hour)),
			RewardPence:     9_000,
			CollateralPence: 5_000,
		}},
		Eligible: map[string]int64{},
	}
	h.putMission(recoveryShipBlastoise, "retry-generation", "arrival", data)
	mission, data := h.mission(recoveryShipBlastoise)
	ship := harnessShip(t, runner, recoveryShipBlastoise)
	if _, err := runner.driveMission(h.ctx, ship, mission, &data); err != nil {
		t.Fatalf("stage capacity-window tender: %v", err)
	}
	mission, data = h.mission(recoveryShipBlastoise)
	if _, err := runner.dispatch(h.ctx, mission, &data); err != nil {
		t.Fatalf("dispatch capacity-refused tender: %v", err)
	}
	mission, data = h.mission(recoveryShipBlastoise)
	if data.Action != nil || data.LastActionID == "" {
		t.Fatalf("capacity 409 was not consumed as a definite outcome: %+v", data)
	}
	if data.Freight[0].Settled || data.ExpensesPence != 0 || data.ProceedsPence != 0 {
		t.Fatalf("capacity refusal mutated the obligation: %+v", data)
	}
	if h.api.mutationCount("/v1/contract/deliver-freight") != 1 {
		t.Fatalf("capacity refusal traffic: %+v", h.api.mutationsFor("/v1/contract/deliver-freight"))
	}

	// Inside the bounded retry window the identical tender is not re-staged,
	// including after a restart.
	runner.generation = uuid.NewString()
	if err := runner.refresh(h.ctx); err != nil {
		t.Fatalf("refresh within retry window: %v", err)
	}
	mission, data = h.mission(recoveryShipBlastoise)
	ship = harnessShip(t, runner, recoveryShipBlastoise)
	if _, err := runner.driveMission(h.ctx, ship, mission, &data); err != nil {
		t.Fatalf("drive within retry window: %v", err)
	}
	mission, data = h.mission(recoveryShipBlastoise)
	if data.Action != nil {
		t.Fatalf("refused tender was re-staged inside the retry window: %+v", data.Action)
	}
	restarted := refreshedRunner(t, h)
	mission, data = h.mission(recoveryShipBlastoise)
	ship = harnessShip(t, restarted, recoveryShipBlastoise)
	if _, err := restarted.driveMission(h.ctx, ship, mission, &data); err != nil {
		t.Fatalf("drive restarted within retry window: %v", err)
	}
	mission, data = h.mission(recoveryShipBlastoise)
	if data.Action != nil {
		t.Fatalf("restart re-staged the tender inside the retry window: %+v", data.Action)
	}

	// Once the host retry deadline has passed, fresh active REST state and the
	// full manifest rearm the tender: the driver first clears the refusal
	// durably, then stages the fresh logical tender with a new identity. The
	// ship must progress instead of latching forever.
	expireTenderRetry(t, h, recoveryShipBlastoise)
	h.expireCooldown(recoveryShipBlastoise)
	restarted.generation = uuid.NewString()
	if err := restarted.refresh(h.ctx); err != nil {
		t.Fatalf("refresh after retry expiry: %v", err)
	}
	mission, data = h.mission(recoveryShipBlastoise)
	ship = harnessShip(t, restarted, recoveryShipBlastoise)
	if _, err := restarted.driveMission(h.ctx, ship, mission, &data); err != nil {
		t.Fatalf("drive latch clear after retry expiry: %v", err)
	}
	restarted.generation = uuid.NewString()
	if err := restarted.refresh(h.ctx); err != nil {
		t.Fatalf("refresh for rearm stage: %v", err)
	}
	mission, data = h.mission(recoveryShipBlastoise)
	ship = harnessShip(t, restarted, recoveryShipBlastoise)
	if _, err := restarted.driveMission(h.ctx, ship, mission, &data); err != nil {
		t.Fatalf("drive rearm stage: %v", err)
	}
	mission, data = h.mission(recoveryShipBlastoise)
	if data.Action == nil || data.Action.Stage != "deliver-freight" {
		t.Fatalf("expired retry deadline with fresh full state did not rearm the tender: %+v", data.Action)
	}
	if data.Action.ID == data.LastActionID {
		t.Fatal("rearmed tender reused the refused action identity")
	}
	if _, err := restarted.dispatch(h.ctx, mission, &data); err != nil {
		t.Fatalf("dispatch rearmed tender: %v", err)
	}
	mission, data = h.mission(recoveryShipBlastoise)
	if h.api.mutationCount("/v1/contract/deliver-freight") != 2 {
		t.Fatalf("repeated capacity refusal traffic: %+v", h.api.mutationsFor("/v1/contract/deliver-freight"))
	}
	if data.Action != nil || data.Freight[0].Settled || data.ExpensesPence != 0 {
		t.Fatalf("second capacity refusal was not bounded and consumed: %+v", data)
	}
}

// A charter that was definitively refused must not stay latched forever: the
// refusal holds while authoritative REST is unchanged (including across a
// restart), and changed contract terms must re-open that charter instead of
// suppressing it globally.
func TestCharterRefusalLatchRefreshesOnAuthoritativeChange(t *testing.T) {
	t.Parallel()
	h := newRecoveryHarness(t)
	proxy := installReconciliationProxy(t, h)
	proxy.setContractRows([]map[string]any{charterContractRow("charter-a", 24)})
	h.api.setFleet([]map[string]any{
		stationaryShipRow(recoveryShipBlastoise, "Blastoise", recoveryFarPort, "berthed", 24, 3),
	})
	runner := refreshedRunner(t, h)

	data := missionData{
		Plan:     Plan{Kind: "recovery", ShipID: recoveryShipBlastoise, Destination: recoveryFarPort},
		Eligible: map[string]int64{recoveryGrain: 24},
	}
	h.putMission(recoveryShipBlastoise, "charter-generation", "arrival", data)
	mission, data := h.mission(recoveryShipBlastoise)
	ship := harnessShip(t, runner, recoveryShipBlastoise)
	if _, err := runner.driveMission(h.ctx, ship, mission, &data); err != nil {
		t.Fatalf("stage charter tender: %v", err)
	}
	mission, data = h.mission(recoveryShipBlastoise)
	if data.Action == nil || data.Action.Stage != "deliver-charter" {
		t.Fatalf("eligible carried-in charter was not tendered: %+v", data.Action)
	}
	charterAttempt := data.Action.ID
	if _, err := runner.dispatch(h.ctx, mission, &data); err != nil {
		t.Fatalf("dispatch refused charter: %v", err)
	}
	mission, data = h.mission(recoveryShipBlastoise)
	if data.Action != nil || data.LastActionID != charterAttempt {
		t.Fatalf("charter refusal was not consumed as a definite outcome: %+v", data)
	}
	// The definitive refusal cooldown is host-domain scaffolding, not the
	// latch under test: expire it so the unchanged-offer latch itself gates
	// the following drives.
	h.expireCooldown(recoveryShipBlastoise)

	// With unchanged REST the latch holds across a restart: no identical
	// charter re-tender, while the ship still progresses on its unsealed
	// cargo.
	restarted := refreshedRunner(t, h)
	mission, data = h.mission(recoveryShipBlastoise)
	ship = harnessShip(t, restarted, recoveryShipBlastoise)
	if _, err := restarted.driveMission(h.ctx, ship, mission, &data); err != nil {
		t.Fatalf("drive latched charter mission: %v", err)
	}
	mission, data = h.mission(recoveryShipBlastoise)
	if data.Action != nil && data.Action.Stage == "deliver-charter" {
		t.Fatalf("latched charter was re-tendered against unchanged state: %+v", data.Action)
	}
	// The latch must not become a global 409 suppression: clear the
	// incidental unsealed-sale intent its hold produced without touching the
	// latch itself, and expire that sale's own refusal cooldown so the
	// changed-terms drive below is gated by the latch alone.
	if data.Action != nil {
		if _, err := restarted.dispatch(h.ctx, mission, &data); err != nil {
			t.Fatalf("consume incidental sale attempt: %v", err)
		}
	}
	h.expireCooldown(recoveryShipBlastoise)

	// Authoritative contract terms changed: the charter re-opens.
	proxy.setContractRows([]map[string]any{charterContractRow("charter-a", 30)})
	restarted.generation = uuid.NewString()
	if err := restarted.refresh(h.ctx); err != nil {
		t.Fatalf("refresh after contract change: %v", err)
	}
	mission, data = h.mission(recoveryShipBlastoise)
	ship = harnessShip(t, restarted, recoveryShipBlastoise)
	if _, err := restarted.driveMission(h.ctx, ship, mission, &data); err != nil {
		t.Fatalf("drive after contract change: %v", err)
	}
	mission, data = h.mission(recoveryShipBlastoise)
	if data.Action == nil || data.Action.Stage != "deliver-charter" {
		t.Fatalf("changed contract terms did not re-open the refused charter: %+v", data.Action)
	}
	if data.Action.ID == charterAttempt {
		t.Fatal("re-opened charter reused the refused action identity")
	}
}

// An ambiguous action quarantines new spending account-wide, but it must not
// park the whole fleet: with one ship's tender transport-lost, another ship
// holding fresh, matching, full-cargo accepted freight still tenders for cash
// credit only, an explicit diagnostic names the quarantined ship, and the
// ambiguous request keeps its exact key and body. An orphaned pending request
// (unknown resources) still blocks everything.
func TestAmbiguousQuarantineBlocksSpendingButLetsOtherShipsDeliver(t *testing.T) {
	t.Parallel()
	h := newRecoveryHarness(t)
	proxy := installReconciliationProxy(t, h)
	proxy.setContractRows([]map[string]any{
		freightContractRow("freight-a", "active", false),
		freightContractRowFor("freight-b", "active", false, recoveryShipCharizard),
	})
	proxy.setDeliverMode("lose_first_key")
	h.api.setFleet([]map[string]any{
		stationaryShipRow(recoveryShipBlastoise, "Blastoise", recoveryFarPort, "berthed", 24, 3),
		stationaryShipRow(recoveryShipCharizard, "Charizard", recoveryFarPort, "berthed", 24, 3),
	})
	runner := refreshedRunner(t, h)

	quarantineFreight := func(contractID string) []freightObligation {
		return []freightObligation{{
			ContractID:      contractID,
			Commodity:       recoveryGrain,
			Quantity:        24,
			Destination:     recoveryFarPort,
			Deadline:        new(time.Now().UTC().Add(6 * time.Hour)),
			RewardPence:     9_000,
			CollateralPence: 5_000,
		}}
	}
	h.putMission(recoveryShipBlastoise, "quarantine-generation-a", "arrival", missionData{
		Plan:     Plan{Kind: "recovery", ShipID: recoveryShipBlastoise, Destination: recoveryFarPort},
		Freight:  quarantineFreight("freight-a"),
		Eligible: map[string]int64{},
	})
	h.putMission(recoveryShipCharizard, "quarantine-generation-b", "arrival", missionData{
		Plan:     Plan{Kind: "recovery", ShipID: recoveryShipCharizard, Destination: recoveryFarPort},
		Freight:  quarantineFreight("freight-b"),
		Eligible: map[string]int64{},
	})

	// Ship A's tender is transport-lost: ambiguous, retained with its exact
	// identity, and made visible as a hold.
	missionA, dataA := h.mission(recoveryShipBlastoise)
	shipA := harnessShip(t, runner, recoveryShipBlastoise)
	if _, err := runner.driveMission(h.ctx, shipA, missionA, &dataA); err != nil {
		t.Fatalf("stage quarantined tender: %v", err)
	}
	missionA, dataA = h.mission(recoveryShipBlastoise)
	ambiguousID := dataA.Action.ID
	ambiguousBody := dataA.Action.Body
	if _, err := runner.dispatch(h.ctx, missionA, &dataA); err != nil {
		t.Fatalf("dispatch transport-lost tender: %v", err)
	}
	missionA, dataA = h.mission(recoveryShipBlastoise)
	if dataA.Action == nil || dataA.Action.ID != ambiguousID {
		t.Fatalf("ambiguous action was not retained: %+v", dataA.Action)
	}
	pending := h.pending()
	if len(pending) != 1 || pending[0].ID != ambiguousID || !bytes.Equal(pending[0].Body, ambiguousBody) {
		t.Fatalf("ambiguous request was not durably retained: %+v", pending)
	}
	// Pin the quarantine window far beyond any scheduling jitter so the
	// scenario never depends on the short unresolved-action cooldown lapsing
	// mid-test.
	dataA.CooldownUntil = time.Now().UTC().Add(time.Hour)
	if err := runner.saveMission(h.ctx, missionA, &dataA); err != nil {
		t.Fatalf("pin quarantine window: %v", err)
	}

	// While ship A stays ambiguous, ship B's fresh full-cargo tender must
	// still deliver for cash credit; no spending mutation may cross the
	// account boundary.
	if _, err := h.tick(runner); err != nil {
		t.Fatalf("quarantine tick: %v", err)
	}
	if _, err := h.tick(runner); err != nil {
		t.Fatalf("quarantine dispatch tick: %v", err)
	}

	_, dataB := h.mission(recoveryShipCharizard)
	if dataB.Freight[0].ContractID != "freight-b" || !dataB.Freight[0].Settled || dataB.ProceedsPence != 9_000 {
		t.Fatalf("quarantine froze unrelated credit-only delivery: %+v", dataB)
	}
	calls := h.api.mutationsFor("/v1/contract/deliver-freight")
	quarantinedAttempts := 0
	for _, call := range calls {
		if call.IdempotencyKey == ambiguousID {
			quarantinedAttempts++
			if call.Status != 0 {
				t.Fatalf("quarantined tender gained a definite outcome on the wire: %+v", call)
			}
		}
	}
	if quarantinedAttempts == 0 {
		t.Fatal("quarantined tender never reached the wire")
	}
	delivered := calls[len(calls)-1]
	if len(calls)-quarantinedAttempts != 1 || delivered.Status != http.StatusOK || delivered.IdempotencyKey == ambiguousID {
		t.Fatalf("unexpected tender traffic during quarantine: %+v", calls)
	}
	if delivered.IdempotencyKey != dataB.LastActionID {
		t.Fatalf("credit-only delivery did not use ship B's own identity: %+v", delivered)
	}
	for _, path := range []string{"/v1/market/place-order", "/v1/fleet/dock", "/v1/treasury/remit", "/v1/shipyard/build-ship", "/v1/company/settings"} {
		if h.api.mutationCount(path) != 0 {
			t.Fatalf("quarantine allowed spending mutation %s", path)
		}
	}

	// The ambiguous ship keeps its exact pending identity and records a
	// visible diagnostic instead of stalling silently.
	missionA, dataA = h.mission(recoveryShipBlastoise)
	if dataA.Action == nil || dataA.Action.ID != ambiguousID || !bytes.Equal(dataA.Action.Body, ambiguousBody) {
		t.Fatalf("ambiguous action identity changed during quarantine: %+v", dataA.Action)
	}
	if dataA.Freight[0].Settled || dataA.ProceedsPence != 0 {
		t.Fatalf("ambiguous tender invented an outcome: %+v", dataA)
	}
	findDecision(t, h, recoveryShipBlastoise, "unresolved")
	if len(h.pending()) != 1 {
		t.Fatalf("quarantine left unexpected journal rows: %+v", h.pending())
	}
}
