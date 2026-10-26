package engine

// These acceptance regressions use the existing opt-in TEST_DATABASE_URL
// harness, with one disposable PostgreSQL schema per scenario. The proxy adds
// executable liquidation/destination books and authoritative sale/harbour
// outcomes without changing the shared recovery fixtures or production APIs.

import (
	"encoding/json"
	"io"
	"net/http"
	"testing"
	"time"

	"github.com/DevYukine/go-tradewinds/internal/store"
)

type procurementRecoveryAPI struct {
	h          *recoveryHarness
	downstream http.Handler
	contracts  []Contract
	berthPaid  bool // protected by h.api.mu, together with fleet and vaults
}

func installProcurementRecoveryAPI(h *recoveryHarness, contracts []Contract) {
	proxy := &procurementRecoveryAPI{h: h, downstream: h.api.server.Config.Handler, contracts: contracts}
	h.api.server.Config.Handler = proxy
}

func (p *procurementRecoveryAPI) ServeHTTP(writer http.ResponseWriter, request *http.Request) {
	api := p.h.api
	if request.Method == http.MethodGet {
		switch request.URL.Path {
		case "/v1/market/book":
			port := request.URL.Query().Get("port_id")
			book := api.bookDocument(port)
			book["customs_bps"] = 0
			price := int64(300)
			if port == recoveryFarPort {
				price = 475
			}
			for _, raw := range book["books"].([]any) {
				entry := raw.(map[string]any)
				if entry["commodity_id"] == recoveryGrain {
					entry["depth"].(map[string]any)["bids"] = []any{map[string]any{
						"order_id":    "procurement-recovery-bid-" + port,
						"price_pence": price, "remaining_quantity": 100,
					}}
				}
			}
			api.writeJSON(writer, http.StatusOK, book)
			return
		case "/v1/world/port":
			port := request.URL.Query().Get("port_id")
			detail := api.portDocument(port)
			detail["customs_bps"] = 0
			detail["base_customs_bps"] = 0
			detail["berth_dues_per_day_pence"] = 0
			if port == recoveryFarPort {
				detail["berth_dues_per_day_pence"] = 4_000
			}
			api.writeJSON(writer, http.StatusOK, detail)
			return
		case "/v1/contract/list":
			rows := make([]Contract, 0)
			for _, contract := range p.contracts {
				if contract.Status == request.URL.Query().Get("status") {
					rows = append(rows, contract)
				}
			}
			api.writeJSON(writer, http.StatusOK, rows)
			return
		case "/v1/treasury/ledger":
			api.mu.Lock()
			paid := p.berthPaid
			api.mu.Unlock()
			rows := []any{}
			if paid {
				rows = append(rows, map[string]any{
					"id": "procurement-recovery-harbour-bill", "debit_account_id": "vault-south",
					"credit_account_id": "harbour", "amount_pence": 1_000, "reason": "berth_dues",
				})
			}
			api.writeJSON(writer, http.StatusOK, rows)
			return
		}
	}
	if request.Method != http.MethodPost || request.URL.Path != "/v1/market/place-order" && request.URL.Path != "/v1/fleet/offshore" {
		p.downstream.ServeHTTP(writer, request)
		return
	}
	body, err := io.ReadAll(request.Body)
	if err != nil {
		api.writeProblem(writer, http.StatusBadRequest, "unreadable_request", err.Error())
		return
	}
	var payload struct {
		ShipID    string `json:"ship_id"`
		PortID    string `json:"port_id"`
		Commodity string `json:"commodity_id"`
		Side      string `json:"side"`
		Quantity  int64  `json:"quantity"`
		Price     int64  `json:"price_pence"`
		TIF       string `json:"time_in_force"`
	}
	if err := json.Unmarshal(body, &payload); err != nil {
		api.writeProblem(writer, http.StatusBadRequest, "bad_request", err.Error())
		return
	}
	api.mu.Lock()
	defer api.mu.Unlock()
	for _, row := range api.fleet {
		ship := row["ship"].(map[string]any)
		if ship["id"] != payload.ShipID || ship["status"] != "berthed" || ship["current_port_id"] != recoveryFarPort {
			continue
		}
		cargo := row["cargo_manifest"].(map[string]any)
		if request.URL.Path == "/v1/market/place-order" {
			if payload.PortID != recoveryFarPort || payload.Commodity != recoveryGrain || payload.Side != "ask" ||
				payload.Quantity != 20 || payload.Price != 475 || payload.TIF != "ioc" || len(p.contracts) != 0 || cargo[recoveryGrain] != int64(20) {
				break
			}
			delete(cargo, recoveryGrain)
			row["cargo_used_tons"] = 0
			api.vaultBalance["south"] += 9_500
			api.record(request, body, http.StatusOK)
			api.writeJSON(writer, http.StatusOK, map[string]any{
				"commodity_id": recoveryGrain, "filled_quantity": 20,
				"total_gross_pence": 9_500, "customs_duty_pence": 0,
			})
			return
		}
		if len(cargo) != 0 || len(p.contracts) != 0 || p.berthPaid {
			break
		}
		ship["status"] = "anchored_roadstead"
		api.vaultBalance["south"] -= 1_000
		p.berthPaid = true
		api.record(request, body, http.StatusOK)
		api.writeJSON(writer, http.StatusOK, map[string]any{
			"ship_id": payload.ShipID, "status": "anchored_roadstead",
			"berth_dues_pence": 1_000, "berth_dues_debt_pence": 0,
		})
		return
	}
	api.record(request, body, http.StatusConflict)
	api.writeProblem(writer, http.StatusConflict, "invalid_recovery_action", "request has no matching executable cargo or stationary harbour state")
}

func procurementRecoveryFixture(t *testing.T, sealed bool) (*recoveryHarness, *Runner, *store.Mission, missionData, ShipState) {
	t.Helper()
	h := newRecoveryHarness(t)
	var contracts []Contract
	var obligations []freightObligation
	cargoTons := int64(20)
	if sealed {
		deadline := time.Now().UTC().Add(7 * 24 * time.Hour)
		contract := Contract{
			ID: "procurement-recovery-seal", ContractType: "freight", Status: "active",
			OriginPortID: recoveryHomePort, DestPortID: recoveryFarPort, CommodityID: recoveryGrain,
			QuantityRequired: 2, RemainingQuantity: 2, RewardPence: 100,
			CarrierCompanyID: recoveryCompanyID, CarrierShipID: recoveryShipBlastoise,
			Metadata: encode(map[string]any{"delivery_deadline": deadline}),
		}
		contracts = append(contracts, contract)
		obligations = append(obligations, obligationFromContract(contract))
		cargoTons += 2
	}
	installProcurementRecoveryAPI(h, contracts)
	h.api.setFactorPorts(recoveryFarPort)
	row := destinationShipRow(recoveryShipBlastoise, "Blastoise", recoveryHomePort, "berthed", 3, nil, map[string]int64{recoveryGrain: cargoTons})
	// The shared fake's successful departure endpoint uses integer stores.
	row["stores"] = map[string]any{"provisions_tons": 3, "sea_days_remaining": 5}
	h.api.setFleet([]map[string]any{row})
	runner := refreshedRunner(t, h)
	data := missionData{
		Plan: Plan{
			Kind: "spot", ShipID: recoveryShipBlastoise, Origin: recoveryHomePort,
			Destination: recoveryFarPort, Commodity: recoveryGrain, Quantity: 20,
			BuyLimitPence: 500, SellLimitPence: 475, Accepted: true, TargetStoresTons: 2,
			Economics: Economics{RevenuePence: 9_500, PurchasePence: 10_000, BerthDuesPence: 1_000, ExpectedNetPence: -1_500},
		},
		FinishAtDestination: true, BoughtQuantity: 20, BuyDone: true, AcquisitionPence: 10_000,
		CostsKnown: true, Freight: obligations, StartedAt: time.Now().UTC().Add(-time.Hour),
		StartedGameHours: 2_399, InitialLedgerIDs: map[string]bool{}, Eligible: map[string]int64{},
		RejectedCharterIDs: map[string]bool{}, ProvenLane: "laport|portb|grain|spot",
	}
	h.putMission(recoveryShipBlastoise, "procurement-recovery-mission", "depart", data)
	mission, data := h.mission(recoveryShipBlastoise)
	ship, exists := runner.ship(recoveryShipBlastoise)
	if !exists {
		t.Fatal("refreshed recovery ship is absent")
	}
	return h, runner, mission, data, ship
}

func procurementRecoveryRequireDeparture(t *testing.T, h *recoveryHarness, runner *Runner, mission *store.Mission, data *missionData) {
	t.Helper()
	if data.Action == nil || data.Action.Stage != "depart" || mission.Destination != recoveryFarPort || data.Plan.Kind != "spot" {
		t.Fatalf("better destination recovery was unwound or held on sunk costs: phase=%s destination=%s plan=%+v action=%+v", mission.Phase, mission.Destination, data.Plan, data.Action)
	}
	if _, err := runner.dispatch(h.ctx, mission, data); err != nil {
		t.Fatalf("dispatch recovery departure: %v", err)
	}
	calls := h.api.mutationsFor("/v1/fleet/depart")
	if len(calls) != 1 || calls[0].Status != http.StatusOK || mission.Phase != "sailing" {
		t.Fatalf("first-send preflight stranded the better recovery: calls=%+v phase=%s", calls, mission.Phase)
	}
	if data.AcquisitionPence != 10_000 || data.BoughtQuantity != 20 {
		t.Fatalf("recovery erased actual sunk procurement: %+v", data)
	}
}

func TestProcurementRecoveryDeliversBetterRecoveryAndSettlesLoss(t *testing.T) {
	t.Parallel()
	h, runner, mission, data, ship := procurementRecoveryFixture(t, false)
	if _, err := runner.prepareDeparture(h.ctx, ship, mission, &data); err != nil {
		t.Fatalf("prepare recovery departure: %v", err)
	}
	procurementRecoveryRequireDeparture(t, h, runner, mission, &data)

	// Arrival is a new authoritative fleet observation, never an ETA trigger.
	arrived := destinationShipRow(recoveryShipBlastoise, "Blastoise", recoveryFarPort, "berthed", 3, nil, map[string]int64{recoveryGrain: 20})
	h.api.setFleet([]map[string]any{arrived})
	for range 10 {
		if _, err := h.tick(runner); err != nil {
			t.Fatalf("recover cargo at destination and settle: %v", err)
		}
		rows, err := h.db.SettlementHistory(h.ctx, h.account.ID, "laport|portb|grain|spot", 10)
		if err != nil {
			t.Fatal(err)
		}
		if len(rows) == 0 {
			continue
		}
		if len(rows) != 1 {
			t.Fatalf("recovery published duplicate settlements: %+v", rows)
		}
		settled := rows[0]
		net := settled.ProceedsPence - settled.AcquisitionPence - settled.FeesPence - settled.ProvisionsPence - settled.ExpensesPence
		if !settled.CostsKnown || settled.QuantityTons != 20 || settled.AcquisitionPence != 10_000 ||
			settled.ProceedsPence != 9_500 || settled.ExpensesPence != 1_000 || net != -1_500 {
			t.Fatalf("8500p recovery must retain its actual 1500p loss, not invent a profitable voyage: %+v net=%d", settled, net)
		}
		orders := h.api.mutationsFor("/v1/market/place-order")
		if len(orders) != 1 || orders[0].Status != http.StatusOK {
			t.Fatalf("recovery repeated procurement or liquidated at the inferior local price: %+v", orders)
		}
		return
	}
	t.Fatal("delivered recovery never published its loss-inclusive settlement")

}

func TestProcurementRecoveryFirstSendDoesNotReintroduceSunkCostGate(t *testing.T) {
	t.Parallel()
	h, runner, mission, data, ship := procurementRecoveryFixture(t, false)
	// Persist the existing immutable departure intent directly so this case
	// still reaches journal preflight even while preparation is unfixed.
	if err := runner.stage(h.ctx, mission, &data, ship, "depart", "/v1/fleet/depart", map[string]any{
		"ship_id": ship.Ship.ID, "dest_port_id": recoveryFarPort,
	}); err != nil {
		t.Fatal(err)
	}
	procurementRecoveryRequireDeparture(t, h, runner, mission, &data)

}

func TestProcurementRecoveryPreservesAcceptedSealWithoutSunkCostIdle(t *testing.T) {
	t.Parallel()
	h, runner, mission, data, ship := procurementRecoveryFixture(t, true)
	if _, err := runner.prepareDeparture(h.ctx, ship, mission, &data); err != nil {
		t.Fatal(err)
	}
	procurementRecoveryRequireDeparture(t, h, runner, mission, &data)
	if len(data.Freight) != 1 || data.Freight[0].Settled || data.Freight[0].Quantity != 2 ||
		data.Freight[0].Destination != recoveryFarPort || data.DepartedCargo[recoveryGrain] != 22 {
		t.Fatalf("recovery dropped, diverted or liquidated accepted sealed cargo: %+v", data)
	}
	if orders := h.api.mutationsFor("/v1/market/place-order"); len(orders) != 0 {
		t.Fatalf("accepted seals were offered for liquidation: %+v", orders)
	}

}

func TestProcurementRecoveryChoosesExecutableLocalRecoveryWhenBetter(t *testing.T) {
	t.Parallel()
	for _, firstSend := range []bool{false, true} {
		name := "preparation"
		if firstSend {
			name = "first-send"
		}
		t.Run(name, func(t *testing.T) {
			h, runner, mission, data, ship := procurementRecoveryFixture(t, false)
			book := runner.stateCache.Markets[recoveryHomePort]
			for i := range book.Books {
				if book.Books[i].CommodityID == recoveryGrain {
					// 20t at 450p recovers 9000p locally, exceeding the
					// 8500p recovery after the remaining destination bill.
					book.Books[i].Depth.Bids[0].PricePence = 450
				}
			}
			runner.stateCache.Markets[recoveryHomePort] = book
			if firstSend {
				if err := runner.stage(h.ctx, mission, &data, ship, "depart", "/v1/fleet/depart", map[string]any{
					"ship_id": ship.Ship.ID, "dest_port_id": recoveryFarPort,
				}); err != nil {
					t.Fatal(err)
				}
				if _, err := runner.dispatch(h.ctx, mission, &data); err != nil {
					t.Fatal(err)
				}
			} else {
				if _, err := runner.prepareDeparture(h.ctx, ship, mission, &data); err != nil {
					t.Fatal(err)
				}
				if mission.Destination != recoveryHomePort || mission.Phase != "arrival" {
					t.Fatalf("better executable local recovery was not selected: %+v", mission)
				}
			}
			if data.Action != nil && data.Action.Stage == "depart" || len(h.api.mutationsFor("/v1/fleet/depart")) != 0 {
				t.Fatal("recovery fix sailed merely because acquisition was sunk despite a better executable local exit")
			}
			if data.AcquisitionPence != 10_000 {
				t.Fatal("local recovery erased the historical acquisition loss")
			}
		})
	}

}

func TestProcurementRecoveryStillRequiresEvidenceAndSafeDeparture(t *testing.T) {
	t.Parallel()
	for _, gate := range []string{"missing destination depth", "stale destination book", "stale local book", "missing route", "illegal hull route", "short stores", "missing seal deadline", "expired seal deadline"} {
		for _, firstSend := range []bool{false, true} {
			name := gate + "/preparation"
			if firstSend {
				name = gate + "/first-send"
			}
			t.Run(name, func(t *testing.T) {
				sealed := gate == "missing seal deadline" || gate == "expired seal deadline"
				h, runner, mission, data, ship := procurementRecoveryFixture(t, sealed)
				if firstSend {
					if err := runner.stage(h.ctx, mission, &data, ship, "depart", "/v1/fleet/depart", map[string]any{
						"ship_id": ship.Ship.ID, "dest_port_id": recoveryFarPort,
					}); err != nil {
						t.Fatal(err)
					}
				}
				key := routeKey(recoveryHomePort, recoveryFarPort, recoveryHull)
				switch gate {
				case "missing destination depth":
					book := runner.stateCache.Markets[recoveryFarPort]
					for i := range book.Books {
						if book.Books[i].CommodityID == recoveryGrain {
							book.Books[i].Depth = nil
						}
					}
					runner.stateCache.Markets[recoveryFarPort] = book
				case "stale destination book":
					meta := runner.meta["books/"+recoveryFarPort]
					meta.Stale = true
					runner.meta["books/"+recoveryFarPort] = meta
				case "stale local book":
					meta := runner.meta["books/"+recoveryHomePort]
					meta.Stale = true
					runner.meta["books/"+recoveryHomePort] = meta
				case "missing route":
					delete(runner.stateCache.Routes, key)
					delete(runner.meta, "routes/"+key)
				case "illegal hull route":
					route := runner.stateCache.Routes[key]
					route.Legs[0].EdgeClass = "oceanic"
					runner.stateCache.Routes[key] = route
				case "short stores":
					ship.Stores.ProvisionsTons = 1.5
					runner.stateCache.Ships[0] = ship
				case "missing seal deadline":
					data.Freight[0].Deadline = nil
				case "expired seal deadline":
					deadline := time.Now().UTC().Add(-24 * time.Hour)
					data.Freight[0].Deadline = &deadline
				}
				var err error
				if firstSend {
					_, err = runner.dispatch(h.ctx, mission, &data)
				} else {
					_, err = runner.prepareDeparture(h.ctx, ship, mission, &data)
				}
				if err != nil {
					t.Fatalf("hold/replan unsafe recovery: %v", err)
				}
				if calls := h.api.mutationsFor("/v1/fleet/depart"); len(calls) != 0 {
					t.Fatalf("recovery bypassed %s: %+v", gate, calls)
				}
				if data.Action != nil && data.Action.Stage == "depart" {
					t.Fatalf("unsafe recovery retained a dispatchable departure despite %s", gate)
				}
				if (gate == "missing destination depth" || gate == "stale destination book" || gate == "stale local book") &&
					(mission.Destination != recoveryFarPort || data.Plan.Kind != "spot") {
					t.Fatalf("unknown recovery evidence must hold, not irreversibly select liquidation: destination=%s kind=%s", mission.Destination, data.Plan.Kind)
				}
				if len(h.api.mutationsFor("/v1/market/place-order")) != 0 {
					t.Fatal("missing recovery evidence triggered an unverified liquidation")
				}
			})
		}
	}

}
