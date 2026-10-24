package engine

import (
	"math"
	"net/http"
	"testing"
	"time"

	"github.com/DevYukine/go-tradewinds/internal/store"
)

func procurementReviewBook(port string, bids []Level) Market {
	book := Market{PortID: port, Visibility: "quay", Books: []CommodityBook{
		{CommodityID: recoveryGrain}, {CommodityID: "provisions"},
	}}
	for i := range book.Books {
		book.Books[i].Depth = &struct {
			Bids []Level `json:"bids"`
			Asks []Level `json:"asks"`
		}{Asks: []Level{{PricePence: 50, RemainingQuantity: 10_000}}}
	}
	book.Books[0].Depth.Bids = bids
	book.Books[0].Depth.Asks = []Level{{PricePence: 100, RemainingQuantity: 10_000}}
	return book
}

func procurementReviewRunner(quantity int64) (*Runner, ShipState, missionData) {
	now := time.Now().UTC()
	berthedAt := now.Add(-12 * time.Minute)
	var ship ShipState
	ship.Ship.ID = recoveryShipBlastoise
	ship.Ship.ShipClass = recoveryHull
	ship.Ship.Status = "berthed"
	ship.Ship.CurrentPortID = recoveryHomePort
	ship.Ship.BerthedAt = &berthedAt
	ship.Ship.CannonsCount = 4
	ship.CargoManifest = map[string]int64{recoveryGrain: quantity}
	ship.CargoUsedTons = quantity
	ship.EffectiveHoldCapacityTons = 400
	ship.Stores.ProvisionsTons = 3
	runner := New(nil, nil, store.Account{CompanyID: recoveryCompanyID}, nil, Options{})
	runner.generation = "procurement-review"
	runner.stateCache.Catalog = Catalog{
		Ports: []Port{
			{ID: recoveryHomePort, BankingRegion: "north", Phase: 1},
			{ID: recoveryFarPort, BankingRegion: "south", Phase: 1, Imports: []string{recoveryGrain}},
		},
		ShipClasses: []Hull{{ID: recoveryHull, HullCategory: "coastal", BaseSpeedCentiknots: 600,
			HoldCapacityTons: 400, CrewRequired: 10, MaxCannons: 4}},
	}
	runner.stateCache.Clock = Clock{RealUTC: now, UnlockedPhase: 1}
	runner.stateCache.Clock.Calendar.TimeScale = 60
	runner.stateCache.Clock.Calendar.Month = 3
	runner.stateCache.Clock.Calendar.Day = 1
	runner.stateCache.Ships = []ShipState{ship}
	runner.stateCache.Markets = map[string]Market{
		recoveryHomePort: procurementReviewBook(recoveryHomePort, []Level{{PricePence: 300, RemainingQuantity: quantity}}),
		recoveryFarPort:  procurementReviewBook(recoveryFarPort, []Level{{PricePence: 475, RemainingQuantity: quantity}}),
	}
	runner.stateCache.Ports = map[string]PortDetail{
		recoveryHomePort: {ID: recoveryHomePort, EffectiveBerths: 10},
		recoveryFarPort:  {ID: recoveryFarPort, EffectiveBerths: 10, BerthDuesPerDayPence: 4_000},
	}
	key := routeKey(recoveryHomePort, recoveryFarPort, recoveryHull)
	runner.stateCache.Routes = map[string]Route{key: {
		FromPort: recoveryHomePort, ToPort: recoveryFarPort, ShipClass: recoveryHull,
		TotalDistanceNM: 24, SafeProvisionsTons: 1,
		Legs: []RouteLeg{{FromNode: recoveryHomePort, ToNode: recoveryFarPort, DistanceNM: 24, EdgeClass: "coastal"}},
	}}
	for _, dataset := range []string{"catalog", "clock", "zones", "vaults", "contracts", "ships",
		"books/" + recoveryHomePort, "books/" + recoveryFarPort,
		"ports/" + recoveryHomePort, "ports/" + recoveryFarPort, "routes/" + key} {
		runner.meta[dataset] = DatasetMeta{Complete: true, UpdatedAt: now, AttemptedAt: now, Generation: runner.generation}
	}
	data := missionData{
		Plan: Plan{Kind: "spot", ShipID: ship.Ship.ID, Origin: recoveryHomePort, Destination: recoveryFarPort,
			Commodity: recoveryGrain, Quantity: quantity, Accepted: true,
			PriceSource: "observed destination full depth", Economics: Economics{BerthDuesPence: 1_000}},
		BoughtQuantity: quantity, BuyDone: true, AcquisitionPence: 10_000, CostsKnown: true,
	}
	return runner, ship, data
}

func TestProcurementReviewUsesCurrentDifferentialBills(t *testing.T) {
	cases := []struct {
		name            string
		quantity        int64
		plannedTons     int64
		localPrice      int64
		originDues      int64
		destinationDues int64
		plannedBills    int64
		paidBills       int64
		destinationHub  bool
		roadstead       bool
		wantProceed     bool
	}{
		{name: "paid10000 destination9500 future1000 local6000", quantity: 20, localPrice: 300, destinationDues: 4_000, plannedBills: 1_000, wantProceed: true},
		{name: "current lower tariff beats frozen full voyage", quantity: 20, localPrice: 450, destinationDues: 1_000, plannedBills: 1_000, wantProceed: true},
		{name: "current tariff makes local exit better", quantity: 20, localPrice: 450, destinationDues: 4_000, plannedBills: 1, wantProceed: false},
		{name: "higher current dues are not frozen at purchase", quantity: 20, localPrice: 300, destinationDues: 16_000, plannedBills: 1, wantProceed: false},
		{name: "paid and common origin bills do not recur", quantity: 20, localPrice: 300, originDues: 1_000_000, destinationDues: 4_000, plannedBills: 1_000_000, paidBills: 900_000, wantProceed: true},
		{name: "local handling can extend an accrued stay", quantity: 20, localPrice: 450, originDues: 1_000_000, destinationDues: 4_000, plannedBills: 1_000, wantProceed: true},
		{name: "roadstead local exit needs a new paid berth", quantity: 20, localPrice: 450, originDues: 4_000, destinationDues: 4_000, plannedBills: 1_000, roadstead: true, wantProceed: true},
		{name: "remaining handling uses actual partial purchase", quantity: 100, plannedTons: 300, localPrice: 460, destinationDues: 4_100, plannedBills: 100_000, wantProceed: true},
		{name: "nonhub handling can exceed minimum berth", quantity: 200, localPrice: 469, destinationDues: 4_100, plannedBills: 1, wantProceed: false},
		{name: "hub handling uses one minute per ton", quantity: 200, localPrice: 469, destinationDues: 4_100, destinationHub: true, plannedBills: 100_000, wantProceed: true},
	}
	for _, test := range cases {
		t.Run(test.name, func(t *testing.T) {
			runner, ship, data := procurementReviewRunner(test.quantity)
			runner.stateCache.Markets[recoveryHomePort] = procurementReviewBook(recoveryHomePort, []Level{{PricePence: test.localPrice, RemainingQuantity: test.quantity}})
			origin := runner.stateCache.Ports[recoveryHomePort]
			origin.BerthDuesPerDayPence = test.originDues
			runner.stateCache.Ports[recoveryHomePort] = origin
			destination := runner.stateCache.Ports[recoveryFarPort]
			destination.BerthDuesPerDayPence = test.destinationDues
			runner.stateCache.Ports[recoveryFarPort] = destination
			runner.stateCache.Catalog.Ports[1].IsHub = test.destinationHub
			data.Plan.Economics.BerthDuesPence = test.plannedBills
			data.Plan.Economics.TollPence = test.paidBills
			data.ExpensesPence = test.paidBills
			data.ProvisionsPence = test.paidBills
			if test.plannedTons > 0 {
				data.Plan.Quantity = test.plannedTons
			}
			if test.roadstead {
				ship.Ship.Status = "anchored_roadstead"
				ship.Ship.BerthedAt = nil
			}
			proceed, err := runner.procurementDepartureDecision(ship, recoveryFarPort, &data)
			if err != nil || proceed != test.wantProceed {
				t.Fatalf("recovery decision = %t, %v; want %t using current differential bills", proceed, err, test.wantProceed)
			}
			if data.AcquisitionPence != 10_000 || data.ExpensesPence != test.paidBills || data.BoughtQuantity != test.quantity {
				t.Fatalf("economic comparison rewrote actual paid costs or cargo: %+v", data)
			}
		})
	}
}

func TestProcurementReviewUnknownBerthTimingUsesOnlyDecisiveBounds(t *testing.T) {
	for _, test := range []struct {
		name        string
		localPrice  int64
		wantProceed bool
		wantError   bool
	}{
		{name: "destination wins even with no extra local bill", localPrice: 300, wantProceed: true},
		{name: "local wins even with maximum extra local bill", localPrice: 500},
		{name: "unknown handling bill can change winner", localPrice: 450, wantError: true},
	} {
		t.Run(test.name, func(t *testing.T) {
			runner, ship, data := procurementReviewRunner(20)
			ship.Ship.BerthedAt = nil
			origin := runner.stateCache.Ports[recoveryHomePort]
			origin.BerthDuesPerDayPence = 20_000
			runner.stateCache.Ports[recoveryHomePort] = origin
			runner.stateCache.Markets[recoveryHomePort] = procurementReviewBook(recoveryHomePort, []Level{{PricePence: test.localPrice, RemainingQuantity: 20}})
			proceed, err := runner.procurementDepartureDecision(ship, recoveryFarPort, &data)
			if proceed != test.wantProceed || (err != nil) != test.wantError || ship.Ship.BerthedAt != nil {
				t.Fatalf("unknown berth recovery = %t, %v; want %t, error %t without invented berth time", proceed, err, test.wantProceed, test.wantError)
			}
		})
	}
}

func TestProcurementReviewKeepsMeasuredPartialLiquidity(t *testing.T) {
	for _, test := range []struct {
		name        string
		bids        []Level
		wantRevenue int64
		wantFull    bool
		wantProceed bool
	}{
		{name: "empty", wantProceed: true},
		{name: "partial", bids: []Level{{PricePence: 500, RemainingQuantity: 3}, {PricePence: 600, RemainingQuantity: 2}}, wantRevenue: 2_700, wantProceed: true},
		{name: "partial is not a full exit even with higher known cash", bids: []Level{{PricePence: 3_000, RemainingQuantity: 5}}, wantRevenue: 15_000, wantProceed: true},
		{name: "full better local exit", bids: []Level{{PricePence: 450, RemainingQuantity: 20}}, wantRevenue: 9_000, wantFull: true},
	} {
		t.Run(test.name, func(t *testing.T) {
			runner, ship, data := procurementReviewRunner(20)
			runner.stateCache.Markets[recoveryHomePort] = procurementReviewBook(recoveryHomePort, test.bids)
			revenue, full, err := procurementBidRevenue(runner.planningInput(ship), recoveryHomePort, recoveryGrain, 20)
			if err != nil || revenue != test.wantRevenue || full != test.wantFull {
				t.Fatalf("measured local liquidity = %dp, full %t, %v; want %dp, full %t", revenue, full, err, test.wantRevenue, test.wantFull)
			}
			proceed, err := runner.procurementDepartureDecision(ship, recoveryFarPort, &data)
			if err != nil || proceed != test.wantProceed {
				t.Fatalf("partial liquidity changed destination admission = %t, %v; want %t", proceed, err, test.wantProceed)
			}
		})
	}
}

func TestProcurementReviewMissingOrInvalidEvidenceStillHolds(t *testing.T) {
	for _, gate := range []string{"stale local book", "missing local depth", "invalid local bids", "overflow local bids", "missing route", "unknown toll", "stale harbour", "missing harbour record", "stale clock", "invalid time scale", "negative current dues"} {
		t.Run(gate, func(t *testing.T) {
			runner, ship, data := procurementReviewRunner(20)
			key := routeKey(recoveryHomePort, recoveryFarPort, recoveryHull)
			switch gate {
			case "stale local book":
				meta := runner.meta["books/"+recoveryHomePort]
				meta.Stale = true
				runner.meta["books/"+recoveryHomePort] = meta
			case "missing local depth":
				runner.stateCache.Markets[recoveryHomePort].Books[0].Depth = nil
			case "invalid local bids":
				runner.stateCache.Markets[recoveryHomePort] = procurementReviewBook(recoveryHomePort, []Level{{PricePence: -1, RemainingQuantity: 20}})
			case "overflow local bids":
				data.BoughtQuantity = math.MaxInt64
				ship.CargoManifest[recoveryGrain] = math.MaxInt64
				runner.stateCache.Markets[recoveryHomePort] = procurementReviewBook(recoveryHomePort, []Level{{PricePence: maximumPence, RemainingQuantity: math.MaxInt64}})
			case "missing route":
				delete(runner.stateCache.Routes, key)
			case "unknown toll":
				runner.stateCache.Routes[key].Legs[0].IsSoundToll = true
			case "missing harbour record":
				delete(runner.stateCache.Ports, recoveryFarPort)
			case "stale harbour":
				meta := runner.meta["ports/"+recoveryFarPort]
				meta.Stale = true
				runner.meta["ports/"+recoveryFarPort] = meta
			case "stale clock":
				meta := runner.meta["clock"]
				meta.Stale = true
				runner.meta["clock"] = meta
			case "invalid time scale":
				runner.stateCache.Clock.Calendar.TimeScale = 0
			case "negative current dues":
				detail := runner.stateCache.Ports[recoveryFarPort]
				detail.BerthDuesPerDayPence = -1
				runner.stateCache.Ports[recoveryFarPort] = detail
			}
			if proceed, err := runner.procurementDepartureDecision(ship, recoveryFarPort, &data); proceed || err == nil {
				t.Fatalf("unknown or invalid %s permitted an irreversible recovery choice: proceed %t error %v", gate, proceed, err)
			}
		})
	}
}

type procurementReviewGossipAPI struct {
	h          *recoveryHarness
	downstream http.Handler
	reportAt   time.Time // protected by h.api.mu
}

func (p *procurementReviewGossipAPI) destinationPresent() bool {
	p.h.api.mu.Lock()
	defer p.h.api.mu.Unlock()
	for _, port := range p.h.api.factorPorts {
		if port == recoveryFarPort {
			return true
		}
	}
	for _, row := range p.h.api.fleet {
		ship := row["ship"].(map[string]any)
		if ship["current_port_id"] == recoveryFarPort &&
			(ship["status"] == "berthed" || ship["status"] == "anchored_roadstead") {
			return true
		}
	}
	return false
}

func (p *procurementReviewGossipAPI) ServeHTTP(writer http.ResponseWriter, request *http.Request) {
	if request.Method == http.MethodGet {
		switch request.URL.Path {
		case "/v1/contract/list":
			p.h.api.writeJSON(writer, http.StatusOK, []Contract{})
			return
		case "/v1/market/book":
			if request.URL.Query().Get("port_id") == recoveryFarPort && !p.destinationPresent() {
				p.h.api.writeProblem(writer, http.StatusForbidden, "no_port_presence", "no destination ship or factor is present")
				return
			}
		case "/v1/world/port":
			port := request.URL.Query().Get("port_id")
			detail := p.h.api.portDocument(port)
			detail["berth_dues_per_day_pence"] = 0
			if port == recoveryHomePort {
				p.h.api.mu.Lock()
				reportAt := p.reportAt
				p.h.api.mu.Unlock()
				detail["coffee_house_gossip"] = []any{map[string]any{
					"subject_port_id": recoveryFarPort, "snapshot_at": reportAt,
					"commodities": []any{map[string]any{"commodity_id": recoveryGrain, "snapshot_at": reportAt, "best_bid_pence": 500}},
				}}
			} else if !p.destinationPresent() {
				detail["visibility"] = "none"
			}
			p.h.api.writeJSON(writer, http.StatusOK, detail)
			return
		}
	}
	p.downstream.ServeHTTP(writer, request)
}

func procurementReviewExpireGossip(t *testing.T, runner *Runner, proxy *procurementReviewGossipAPI) {
	t.Helper()
	_, upper, ok := runner.serverBounds()
	scale := runner.stateCache.Clock.Calendar.TimeScale
	if !ok || scale <= 0 {
		t.Fatal("cannot establish game-time gossip age without current server clock authority")
	}
	proxy.h.api.mu.Lock()
	proxy.reportAt = upper.Add(-gameDuration(7.0/24, scale))
	proxy.h.api.mu.Unlock()
}

func procurementReviewGossipHarness(t *testing.T) (*recoveryHarness, *Runner, *procurementReviewGossipAPI, ShipState, Plan) {
	t.Helper()
	h := newRecoveryHarness(t)
	proxy := &procurementReviewGossipAPI{h: h, downstream: h.api.server.Config.Handler, reportAt: time.Now().UTC()}
	h.api.server.Config.Handler = proxy
	row := destinationShipRow(recoveryShipBlastoise, "Blastoise", recoveryHomePort, "berthed", 3, nil, nil)
	row["stores"] = map[string]any{"provisions_tons": 3, "sea_days_remaining": 5}
	row["ship"].(map[string]any)["berthed_at"] = time.Now().UTC().Add(-time.Minute)
	h.api.setFleet([]map[string]any{row})
	runner := refreshedRunner(t, h)
	ship := harnessShip(t, runner, recoveryShipBlastoise)
	plans := PlanVoyages(runner.planningInput(ship))
	if len(plans) == 0 || !plans[0].Accepted || plans[0].Kind != "spot" || plans[0].Quantity != 1 {
		t.Fatalf("fresh gossip did not admit exactly a one-ton spot probe: %+v", plans)
	}
	plan := plans[0]
	revenue, _, stress, source, reason := destinationRevenue(runner.planningInput(ship), recoveryFarPort, recoveryGrain, 1)
	if reason != "" || revenue != 500 || stress <= 0 || plan.PriceSource != source {
		t.Fatalf("probe must retain the planner's estimate provenance and separate price stress: %+v, revenue %d stress %d reason %s", plan, revenue, stress, reason)
	}
	if book, exists := runner.stateCache.Markets[recoveryFarPort]; exists && book.Visibility == "quay" {
		t.Fatal("gossip regression unexpectedly acquired a private destination book")
	}
	return h, runner, proxy, ship, plan
}

func TestProcurementReviewFreshGossipCompletesPurchaseToDeparture(t *testing.T) {
	h, runner, _, ship, plan := procurementReviewGossipHarness(t)
	if _, err := runner.startVoyage(h.ctx, ship, plan); err != nil {
		t.Fatal(err)
	}
	for range 16 {
		if _, err := h.tick(runner); err != nil {
			t.Fatalf("advance gossip-backed purchase and departure: %v", err)
		}
		if h.api.mutationCount("/v1/fleet/depart") > 0 {
			break
		}
	}
	orders := h.api.mutationsFor("/v1/market/place-order")
	departures := h.api.mutationsFor("/v1/fleet/depart")
	mission, data := h.mission(ship.Ship.ID)
	if len(orders) != 1 || orders[0].Status != http.StatusOK || len(departures) != 1 || departures[0].Status != http.StatusOK || mission.Phase != "sailing" {
		t.Fatalf("gossip-only procurement stranded or repeated its purchase: orders %+v departures %+v phase %s", orders, departures, mission.Phase)
	}
	if data.BoughtQuantity != 1 || data.AcquisitionPence != 100 || data.FeesPence != 2 || data.Plan.PriceSource != plan.PriceSource || data.DepartedCargo[recoveryGrain] != 1 {
		t.Fatalf("departure overstated gossip quantity or lost actual cost/estimate provenance: %+v", data)
	}
}

func TestProcurementReviewStaleGossipBlocksSpendingAndContinuation(t *testing.T) {
	for _, phase := range []string{"buy preparation", "buy first-send", "paid preparation", "paid first-send"} {
		t.Run(phase, func(t *testing.T) {
			h, runner, proxy, ship, plan := procurementReviewGossipHarness(t)
			mission, data := runner.newVoyage(ship, plan)
			mission.Phase = "buy"
			if err := runner.saveMission(h.ctx, mission, &data); err != nil {
				t.Fatal(err)
			}
			paid := phase == "paid preparation" || phase == "paid first-send"
			if paid {
				for range 8 {
					if _, err := h.tick(runner); err != nil {
						t.Fatal(err)
					}
					mission, data = h.mission(ship.Ship.ID)
					if data.BuyDone {
						break
					}
				}
				if !data.BuyDone || data.BoughtQuantity != 1 {
					t.Fatal("fixture did not execute the original one-ton purchase")
				}
				ship = harnessShip(t, runner, ship.Ship.ID)
				mission.Phase = "depart"
				if err := runner.saveMission(h.ctx, mission, &data); err != nil {
					t.Fatal(err)
				}
			}
			if phase == "buy first-send" {
				if err := runner.stage(h.ctx, mission, &data, ship, "buy", "/v1/market/place-order", orderBody(ship, recoveryGrain, "bid", 1, plan.BuyLimitPence, false)); err != nil {
					t.Fatal(err)
				}
			}
			if phase == "paid first-send" {
				if err := runner.stage(h.ctx, mission, &data, ship, "depart", "/v1/fleet/depart", map[string]any{"ship_id": ship.Ship.ID, "dest_port_id": recoveryFarPort}); err != nil {
					t.Fatal(err)
				}
			}
			procurementReviewExpireGossip(t, runner, proxy)
			actionLogRefresh(t, h, runner)
			ship = harnessShip(t, runner, ship.Ship.ID)
			input := runner.planningInput(ship)
			_, upper, clockOK := input.serverBounds()
			reports := input.Ports[recoveryHomePort].CoffeeHouseGossip
			if !clockOK || len(reports) != 1 || len(reports[0].Commodities) != 1 ||
				upper.Sub(reports[0].SnapshotAt).Seconds()*float64(input.Clock.Calendar.TimeScale) <= 6*3_600 ||
				upper.Sub(reports[0].Commodities[0].SnapshotAt).Seconds()*float64(input.Clock.Calendar.TimeScale) <= 6*3_600 {
				t.Fatal("refreshed fixture did not expose genuinely stale report and commodity timestamps")
			}
			if input.fresh("books/" + recoveryFarPort) {
				t.Fatal("stale-gossip-only fixture unexpectedly acquired independent destination depth")
			}
			for _, candidate := range PlanVoyages(runner.planningInput(ship)) {
				if candidate.Accepted && candidate.Kind == "spot" {
					t.Fatalf("stale gossip admitted another purchase: %+v", candidate)
				}
			}
			if _, err := procurementDestinationRevenue(runner.planningInput(ship), plan, 1); err == nil {
				t.Fatal("stale gossip retained destination receipt authority")
			}
			var err error
			if data.Action != nil {
				_, err = runner.dispatch(h.ctx, mission, &data)
			} else if paid {
				_, err = runner.prepareDeparture(h.ctx, ship, mission, &data)
			} else {
				_, err = runner.driveMission(h.ctx, ship, mission, &data)
			}
			if err != nil {
				t.Fatalf("stale receipt evidence should hold the mission: %v", err)
			}
			wantPurchases := 0
			if paid {
				wantPurchases = 1
			}
			if h.api.mutationCount("/v1/market/place-order") != wantPurchases || h.api.mutationCount("/v1/fleet/depart") != 0 {
				t.Fatalf("stale gossip spent or continued: purchases %+v departures %+v", h.api.mutationsFor("/v1/market/place-order"), h.api.mutationsFor("/v1/fleet/depart"))
			}
			if paid && (data.AcquisitionPence != 100 || data.BoughtQuantity != 1) {
				t.Fatalf("holding stale evidence reopened or erased the completed purchase: %+v", data)
			}
		})
	}
}

func TestProcurementReviewGossipNeverSuppliesMultiTonDepth(t *testing.T) {
	h, runner, _, ship, plan := procurementReviewGossipHarness(t)
	for _, quantity := range []int64{0, 2, 20} {
		if _, err := procurementDestinationRevenue(runner.planningInput(ship), plan, quantity); err == nil {
			t.Fatalf("gossip supplied unsupported %dt destination quantity", quantity)
		}
	}
	if h.api.mutationCount("/v1/market/place-order") != 0 {
		t.Fatal("evidence inspection spent funds")
	}
}

func procurementReviewSettlement(t *testing.T, data missionData, ledger []map[string]any) store.Settlement {
	t.Helper()
	h := newRecoveryHarness(t)
	h.api.setFleet([]map[string]any{destinationShipRow(recoveryShipBlastoise, "Blastoise", data.Plan.Destination, "anchored_roadstead", 3, nil, nil)})
	runner := refreshedRunner(t, h)
	ship := harnessShip(t, runner, recoveryShipBlastoise)
	destinationSeedAction(t, h, "procurement-review-terminal", "/v1/market/place-order", `{"ship_id":"33333333-3333-4333-8333-000000000001"}`, `{"filled_quantity":0,"total_gross_pence":0,"customs_duty_pence":0}`)
	data.LastActionID = "procurement-review-terminal"
	data.FinishAtDestination = true
	data.InitialLedgerIDs = map[string]bool{}
	data.StartedAt = time.Now().UTC().Add(-time.Minute)
	data.StartedGameHours = runner.stateCache.Clock.Calendar.TotalGameHours - 1
	data.ProvenLane = laneKey(data.Plan)
	h.putMission(ship.Ship.ID, "procurement-review-settlement", "closing", data)
	mission, data := h.mission(ship.Ship.ID)
	runner.snapshot["ledger"] = encode(ledger)
	for range 2 {
		if _, err := runner.finishMission(h.ctx, ship, mission, &data); err != nil {
			t.Fatalf("persist actual procurement settlement: %v", err)
		}
	}
	rows := destinationSettlements(t, h, data.ProvenLane)
	if len(rows) != 1 || !rows[0].CostsKnown {
		t.Fatalf("actual costs did not produce one known settlement: %+v", rows)
	}
	return rows[0]
}

func TestProcurementReviewBetterRecoveryStillSettlesActualLoss(t *testing.T) {
	runner, ship, data := procurementReviewRunner(20)
	proceed, err := runner.procurementDepartureDecision(ship, recoveryFarPort, &data)
	if err != nil || !proceed {
		t.Fatalf("9500p destination less current 1000p future bill must beat local 6000p: proceed %t error %v", proceed, err)
	}
	quantity, proceeds, customs := int64(20), int64(9_500), int64(0)
	recordTradeCosts(&data, "sell", tradeResult{
		CommodityID: recoveryGrain, FilledQuantity: &quantity,
		TotalGrossPence: &proceeds, CustomsDutyPence: &customs,
	})
	data.DeliveredQuantity = quantity
	settled := procurementReviewSettlement(t, data, []map[string]any{{
		"id": "actual-destination-berth", "debit_account_id": "vault-south",
		"credit_account_id": "harbour", "amount_pence": 1_000, "reason": "berth_dues",
	}})
	net := settled.ProceedsPence - settled.AcquisitionPence - settled.FeesPence - settled.ProvisionsPence - settled.ExpensesPence
	if settled.AcquisitionPence != 10_000 || settled.ProceedsPence != 9_500 ||
		settled.ExpensesPence != 1_000 || settled.QuantityTons != 20 || net != -1_500 {
		t.Fatalf("8500p recovery erased the actual 1500p procurement loss: %+v net %d", settled, net)
	}
}

func TestProcurementReviewActualUnwindFeedsPermanentEconomicHistory(t *testing.T) {
	for _, test := range []struct {
		name        string
		costsKnown  bool
		acquisition int64
		wantNet     int64
	}{
		{name: "known failed procurement loss", costsKnown: true, acquisition: 10_000, wantNet: -1_500},
		{name: "unknown procurement costs remain a funding gate", acquisition: 10_000},
		{name: "profitable liquidation is not delivered-cycle proof", costsKnown: true, acquisition: 1_000, wantNet: 7_500},
	} {
		t.Run(test.name, func(t *testing.T) {
			h := newRecoveryHarness(t)
			installProcurementRecoveryAPI(h, nil)
			h.api.setFactorPorts(recoveryHomePort, recoveryFarPort)
			row := destinationShipRow(recoveryShipBlastoise, "Blastoise", recoveryFarPort, "berthed", 3, nil, map[string]int64{recoveryGrain: 20})
			row["ship"].(map[string]any)["berthed_at"] = time.Now().UTC().Add(-time.Minute)
			h.api.setFleet([]map[string]any{row})
			runner := refreshedRunner(t, h)
			ship := harnessShip(t, runner, recoveryShipBlastoise)
			plan := Plan{
				Kind: "spot", ShipID: ship.Ship.ID, Origin: recoveryFarPort, Destination: recoveryHomePort,
				Commodity: recoveryGrain, Quantity: 20, BuyLimitPence: 500, SellLimitPence: 300, Accepted: true,
			}
			originalLane := laneKey(plan)
			data := missionData{
				Plan: plan, BoughtQuantity: 20, BuyDone: true, AcquisitionPence: test.acquisition,
				CostsKnown: test.costsKnown, FinishAtDestination: true, ProvenLane: originalLane,
				StartedAt:        time.Now().UTC().Add(-time.Hour),
				StartedGameHours: runner.stateCache.Clock.Calendar.TotalGameHours - 1,
				InitialLedgerIDs: map[string]bool{}, Eligible: map[string]int64{}, RejectedCharterIDs: map[string]bool{},
			}
			h.putMission(ship.Ship.ID, "procurement-review-unwind", "depart", data)
			mission, data := h.mission(ship.Ship.ID)
			if proceed, err := runner.procurementDepartureDecision(ship, recoveryHomePort, &data); err != nil || proceed {
				t.Fatalf("fresh superior local liquidation did not select unwind: proceed %t error %v", proceed, err)
			}
			if changed, err := runner.unwindProcurement(h.ctx, ship, mission, &data, "current executable local recovery is better"); err != nil || !changed {
				t.Fatalf("unwind actual purchased mission: changed %t error %v", changed, err)
			}
			if data.Plan.Kind != "aborted" || data.ProvenLane != originalLane ||
				data.Plan.Origin != "" || mission.Destination != recoveryFarPort {
				t.Fatalf("unwind lost the original permanent lane or local recovery destination: %+v %+v", mission, data)
			}
			var settled store.Settlement
			for range 10 {
				if _, err := h.tick(runner); err != nil {
					t.Fatalf("liquidate, close berth and finish actual aborted mission: %v", err)
				}
				rows := destinationSettlements(t, h, originalLane)
				if len(rows) > 1 {
					t.Fatalf("unwind published duplicate permanent settlements: %+v", rows)
				}
				if len(rows) == 1 {
					settled = rows[0]
					break
				}
			}
			if settled.Kind != "aborted" || settled.Lane != originalLane || settled.AcquisitionPence != test.acquisition ||
				settled.ProceedsPence != 9_500 || settled.ExpensesPence != 1_000 ||
				settled.CostsKnown != test.costsKnown || settled.GameSeconds <= 0 {
				t.Fatalf("actual unwind failed to retain permanent receipts, loss, costs or time: %+v", settled)
			}
			if h.api.mutationCount("/v1/market/place-order") != 1 || h.api.mutationCount("/v1/fleet/offshore") != 1 ||
				h.api.mutationCount("/v1/fleet/depart") != 0 {
				t.Fatalf("unwind did not execute exactly local sale and paid berth closure: %+v", h.api.mutations)
			}
			restarted := h.runner(primaryTestKey)
			for _, lane := range []string{"", originalLane} {
				history, err := restarted.db.EconomicHistory(h.ctx, h.account.ID, lane)
				if err != nil {
					t.Fatalf("read permanent post-restart economic history: %v", err)
				}
				if history.CommercialNetPence != test.wantNet || history.NetPence != test.wantNet ||
					history.GameSeconds != settled.GameSeconds || history.UnknownCosts != !test.costsKnown ||
					history.LastCompletion == nil || !history.LastCompletion.Equal(settled.CompletedAt) ||
					history.UnmeasuredDuration || history.PositiveCycles != 0 || history.PositiveQuantityTons != 0 ||
					history.PositiveNetPence != 0 || history.PositiveGameSeconds != 0 ||
					history.ProbeExpensePence != 0 || history.UnknownProbeCosts || history.LastProbeCompletion != nil {
					t.Fatalf("global/exact-lane history lost aborted money, uncertainty or time, or granted delivery proof: lane %q history %+v", lane, history)
				}
			}
		})
	}
}

func TestProcurementReviewStaleGossipAllowsFreshDestinationDepth(t *testing.T) {
	h, runner, proxy, ship, plan := procurementReviewGossipHarness(t)
	procurementReviewExpireGossip(t, runner, proxy)
	h.api.setFactorPorts(recoveryFarPort)
	actionLogRefresh(t, h, runner)
	ship = harnessShip(t, runner, ship.Ship.ID)
	input := runner.planningInput(ship)
	revenue, _, stress, source, reason := destinationRevenue(input, recoveryFarPort, recoveryGrain, 2)
	if reason != "" || revenue != 1_000 || stress != 0 || source == plan.PriceSource ||
		!input.fresh("books/"+recoveryFarPort) {
		t.Fatalf("independently observed destination depth was incorrectly rejected with stale gossip: revenue %d stress %d source %s reason %s", revenue, stress, source, reason)
	}
	if _, err := runner.startVoyage(h.ctx, ship, plan); err != nil {
		t.Fatal(err)
	}
	for range 16 {
		if _, err := h.tick(runner); err != nil {
			t.Fatalf("advance original one-ton plan using independent fresh destination depth: %v", err)
		}
		if h.api.mutationCount("/v1/fleet/depart") > 0 {
			break
		}
	}
	orders := h.api.mutationsFor("/v1/market/place-order")
	departures := h.api.mutationsFor("/v1/fleet/depart")
	mission, data := h.mission(ship.Ship.ID)
	if len(orders) != 1 || orders[0].Status != http.StatusOK ||
		len(departures) != 1 || departures[0].Status != http.StatusOK || mission.Phase != "sailing" ||
		data.BoughtQuantity != 1 || data.AcquisitionPence != 100 || data.Plan.PriceSource != plan.PriceSource {
		t.Fatalf("fresh executable fallback resized, repeated or stranded the original probe: orders %+v departures %+v phase %s data %+v", orders, departures, mission.Phase, data)
	}
}
