package engine

import (
	"encoding/json"
	"fmt"
	"io"
	"math"
	"net/http"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/DevYukine/go-tradewinds/internal/store"
	"github.com/google/uuid"
)

func expansionFactorDecision(t *testing.T, h *recoveryHarness, region string) store.Decision {
	t.Helper()
	history, err := h.db.History(h.ctx, h.account.ID, "decisions", store.MaxHistoryLimit)
	if err != nil {
		t.Fatal(err)
	}
	for _, row := range history.([]store.Decision) {
		if row.Kind == "expansion_agent:"+region {
			return row
		}
	}
	t.Fatalf("no expansion_agent verdict for %s", region)
	return store.Decision{}
}

func expansionFactorFixture() AccountState {
	xReward, yReward := int64(200), int64(150)
	now, ask := time.Now().UTC().Add(-time.Second), int64(100)
	// Paid overseas targets are phase-2 yards; the locked yard stays one
	// phase above the unlock so the phase-limit rejection stays exercised.
	return AccountState{
		Clock: Clock{UnlockedPhase: 2},
		Catalog: Catalog{
			Ports: []Port{
				{ID: "yard-b", BankingRegion: "caribbean", SeaZoneID: "fixture_sea", HasShipyard: true, Phase: 2, Produces: []string{"x"}},
				{ID: "yard-a", BankingRegion: "caribbean", SeaZoneID: "fixture_sea", HasShipyard: true, Phase: 2, Produces: []string{"x"}, Manufactures: []string{"y"}},
				{ID: "demand", BankingRegion: "caribbean", SeaZoneID: "fixture_sea", Phase: 2},
				{ID: "cove", BankingRegion: "caribbean", SeaZoneID: "fixture_sea", HasShipyard: true, IsCove: true, Phase: 2, Produces: []string{"x", "y"}},
				{ID: "locked", BankingRegion: "caribbean", SeaZoneID: "fixture_sea", HasShipyard: true, Phase: 3, Produces: []string{"x", "y"}},
			},
			SeaZones:    []Zone{{ID: "fixture_sea", ZoneClass: "regional"}},
			Commodities: []Commodity{{ID: "x", BasePricePence: 100}, {ID: "y", BasePricePence: 100}},
		},
		Contracts: []Contract{
			{ID: "charter-x", ContractType: "charter", Status: "open", OriginPortID: "demand", CommodityID: "x", RemainingQuantity: 10, RewardPerTonPence: &xReward},
			{ID: "charter-y", ContractType: "charter", Status: "open", OriginPortID: "demand", CommodityID: "y", RemainingQuantity: 10, RewardPerTonPence: &yReward},
		},
		Ports: map[string]PortDetail{
			recoveryHomePort: {CoffeeHouseGossip: []Gossip{
				{SubjectPortID: "yard-a", SnapshotAt: now, Commodities: []GossipCommodity{
					{CommodityID: "x", SnapshotAt: now, BestAskPence: &ask},
					{CommodityID: "y", SnapshotAt: now, BestAskPence: &ask},
				}},
				{SubjectPortID: "yard-b", SnapshotAt: now, Commodities: []GossipCommodity{
					{CommodityID: "x", SnapshotAt: now, BestAskPence: &ask},
				}},
				{SubjectPortID: "demand", SnapshotAt: now, Commodities: []GossipCommodity{
					{CommodityID: "x", SnapshotAt: now, BestBidPence: &xReward},
					{CommodityID: "y", SnapshotAt: now, BestBidPence: &yReward},
				}},
			}},
		},
	}
}

func expansionFactorInput(state AccountState) PlanningInput {
	now := time.Now().UTC()
	clock := state.Clock
	clock.RealUTC, clock.Calendar.TimeScale = now, 60
	return PlanningInput{
		Now: now, Clock: clock, Generation: "expansion",
		Catalog: state.Catalog, Markets: state.Markets, Ports: state.Ports,
		Metadata: map[string]DatasetMeta{
			"clock":                     {UpdatedAt: now, AttemptedAt: now, Generation: "expansion", Complete: true},
			"ports/" + recoveryHomePort: {UpdatedAt: now, AttemptedAt: now, Generation: "expansion", Complete: true},
		},
	}
}

func TestExpansionFactorScoring(t *testing.T) {
	t.Parallel()
	for _, test := range []struct {
		name   string
		change func(*AccountState)
		port   string
		score  int64
	}{
		{name: "positive observed spot spread", port: "yard-a", score: 100},
		{name: "observed supply need not be catalog production", change: func(state *AccountState) {
			for i := range state.Catalog.Ports {
				state.Catalog.Ports[i].Produces, state.Catalog.Ports[i].Manufactures = nil, nil
			}
		}, port: "yard-a", score: 100},
		{name: "newer phase does not beat a richer older region", change: func(state *AccountState) {
			ask, bid := int64(100), int64(140)
			state.Clock.UnlockedPhase = 3
			state.Catalog.Ports = append(state.Catalog.Ports,
				Port{ID: "new-yard", BankingRegion: "america", SeaZoneID: "fixture_sea", HasShipyard: true, Phase: 3},
				Port{ID: "new-demand", BankingRegion: "america", SeaZoneID: "fixture_sea", Phase: 3},
			)
			detail := state.Ports[recoveryHomePort]
			now := time.Now().UTC().Add(-time.Second)
			detail.CoffeeHouseGossip = append(detail.CoffeeHouseGossip,
				Gossip{SubjectPortID: "new-yard", SnapshotAt: now, Commodities: []GossipCommodity{{CommodityID: "x", SnapshotAt: now, BestAskPence: &ask}}},
				Gossip{SubjectPortID: "new-demand", SnapshotAt: now, Commodities: []GossipCommodity{{CommodityID: "x", SnapshotAt: now, BestBidPence: &bid}}},
			)
			state.Ports[recoveryHomePort] = detail
		}, port: "yard-a", score: 100},
		{name: "oceanic water cannot host a regional build", change: func(state *AccountState) {
			state.Catalog.SeaZones[0].ZoneClass = "oceanic"
		}},
		{name: "demand at yard itself", change: func(state *AccountState) {
			state.Catalog.Ports = state.Catalog.Ports[1:2]
		}},
		{name: "commodity missing from catalog", change: func(state *AccountState) {
			state.Catalog.Commodities = []Commodity{{ID: "z"}}
		}},
		{name: "coves and locked phases", change: func(state *AccountState) {
			state.Catalog.Ports = state.Catalog.Ports[2:]
		}},
		{name: "charter-only evidence", change: func(state *AccountState) { state.Ports = nil }},
		{name: "no charters needed", change: func(state *AccountState) { state.Contracts = nil }, port: "yard-a", score: 100},
		{name: "finite rewards cannot increase score", change: func(state *AccountState) {
			reward := int64(1_000_000)
			for i := range state.Contracts {
				state.Contracts[i].RewardPerTonPence = &reward
			}
		}, port: "yard-a", score: 100},
		{name: "another region", change: func(state *AccountState) {
			state.Catalog.Ports[2].BankingRegion = "america"
		}},
		{name: "no positive stressed spread", change: func(state *AccountState) {
			ask := int64(1_000)
			for _, report := range state.Ports[recoveryHomePort].CoffeeHouseGossip {
				for i := range report.Commodities {
					report.Commodities[i].BestAskPence = &ask
				}
			}
		}},
		{name: "missing observed supply", change: func(state *AccountState) {
			for _, report := range state.Ports[recoveryHomePort].CoffeeHouseGossip {
				for i := range report.Commodities {
					report.Commodities[i].BestAskPence = nil
				}
			}
		}},
		{name: "stale report", change: func(state *AccountState) {
			detail := state.Ports[recoveryHomePort]
			for i := range detail.CoffeeHouseGossip {
				detail.CoffeeHouseGossip[i].SnapshotAt = time.Now().Add(-13 * time.Hour)
			}
			state.Ports[recoveryHomePort] = detail
		}},
		{name: "stale commodity", change: func(state *AccountState) {
			for _, report := range state.Ports[recoveryHomePort].CoffeeHouseGossip {
				for i := range report.Commodities {
					report.Commodities[i].SnapshotAt = time.Now().Add(-13 * time.Hour)
				}
			}
		}},
		{name: "factor presence", change: func(state *AccountState) { state.Factors = []string{"demand"} }},
		{name: "ship presence", change: func(state *AccountState) {
			ship := ShipState{}
			ship.Ship.Status, ship.Ship.CurrentPortID = "sailing", "demand"
			state.Ships = []ShipState{ship}
		}},
		{name: "sunk ship is not presence", change: func(state *AccountState) {
			ship := ShipState{}
			ship.Ship.Status, ship.Ship.CurrentPortID = "sunk", "demand"
			state.Ships = []ShipState{ship}
		}, port: "yard-a", score: 100},
		{name: "port ID breaks ties", port: "yard-a", score: 100},
	} {
		t.Run(test.name, func(t *testing.T) {
			state := expansionFactorFixture()
			if test.change != nil {
				test.change(&state)
			}
			var port Port
			var score int64
			if pairs := expansionAgentYards(state, expansionFactorInput(state)); len(pairs) > 0 {
				port, score = pairs[0].yard, pairs[0].score
			}
			if port.ID != test.port || score != test.score {
				t.Fatalf("got %s score %d, want %s score %d", port.ID, score, test.port, test.score)
			}
		})
	}
}

func TestExpansionFactorRequiresFreshObservationAuthority(t *testing.T) {
	t.Parallel()
	for _, dataset := range []string{"clock", "ports/" + recoveryHomePort} {
		t.Run(dataset, func(t *testing.T) {
			state := expansionFactorFixture()
			input := expansionFactorInput(state)
			meta := input.Metadata[dataset]
			meta.Stale = true
			input.Metadata[dataset] = meta
			if pairs := expansionAgentYards(state, input); len(pairs) != 0 {
				t.Fatalf("stale %s still selected %+v", dataset, pairs)
			}
		})
	}
}

type expansionFactorAPI struct {
	h               *recoveryHarness
	downstream      http.Handler
	status          int
	code            string
	uninterpretable bool
}

func newExpansionFactorAPI(h *recoveryHarness) *expansionFactorAPI {
	proxy := &expansionFactorAPI{h: h, downstream: h.api.server.Config.Handler}
	h.api.server.Config.Handler = proxy
	h.api.mu.Lock()
	if !slices.Contains(h.api.factorPorts, recoveryHomePort) {
		h.api.factorPorts = append(h.api.factorPorts, recoveryHomePort)
	}
	for _, row := range h.api.fleet {
		row["ship"].(map[string]any)["created_at"] = time.Now().UTC().Add(-30 * 24 * time.Hour)
	}
	h.api.mu.Unlock()
	h.api.setVaultBalance("caribbean", 500_000)
	return proxy
}

func newMeasuredExpansionFactorAPI(h *recoveryHarness) *expansionFactorAPI {
	proxy := newExpansionFactorAPI(h)
	persistGrowthWindowSettlement(h.api.t, h, store.Settlement{
		ID: "expansion-measured-income", Kind: "spot", CostsKnown: true,
		QuantityTons: 1, ProceedsPence: 30_000_000, GameSeconds: 3_600,
		CompletedAt: time.Now().UTC().Add(-time.Minute),
	})
	return proxy
}

func persistExpansionRegionalIncome(t *testing.T, h *recoveryHarness, id string, proceeds int64) {
	t.Helper()
	row := store.Settlement{
		ID: id, AccountID: h.account.ID, ShipID: recoveryShipBlastoise,
		MissionID: id + "-mission", ActionID: id + "-action",
		Kind: "spot", Origin: "yard-a", Destination: "demand", Commodity: "x", QuantityTons: 1,
		Lane:       laneKey(Plan{Kind: "spot", Origin: "yard-a", Destination: "demand", Commodity: "x"}),
		CostsKnown: true, ProceedsPence: proceeds, GameSeconds: 86_400,
		CompletedAt: time.Now().UTC().Add(-time.Second),
	}
	if _, err := h.db.BeginAction(h.ctx, &store.Action{
		ID: row.ActionID, AccountID: h.account.ID, Method: http.MethodPost, Path: "/v1/market/place-order",
		Body: encode(map[string]any{"ship_id": row.ShipID, "commodity_id": row.Commodity, "side": "ask", "quantity": row.QuantityTons}),
	}); err != nil {
		t.Fatal(err)
	}
	if err := h.db.CompleteAction(h.ctx, row.ActionID, http.StatusOK, json.RawMessage(`{"filled_quantity":1}`), nil); err != nil {
		t.Fatal(err)
	}
	if err := h.db.PutSettlement(h.ctx, &row); err != nil {
		t.Fatal(err)
	}
}

func (p *expansionFactorAPI) ServeHTTP(writer http.ResponseWriter, request *http.Request) {
	path := request.URL.Path
	if request.Method == http.MethodGet && path == "/v1/world/catalog" {
		p.h.api.mu.Lock()
		catalog := p.h.api.catalogDocument()
		catalog["ship_classes"].([]any)[0].(map[string]any)["hull_category"] = "regional"
		p.h.api.mu.Unlock()
		fixture := expansionFactorFixture()
		for _, port := range fixture.Catalog.Ports {
			catalog["ports"] = append(catalog["ports"].([]any), port)
		}
		catalog["commodities"] = append(fixture.Catalog.Commodities, Commodity{ID: "provisions", BasePricePence: 100})
		for _, zone := range fixture.Catalog.SeaZones {
			catalog["sea_zones"] = append(catalog["sea_zones"].([]any), zone)
		}
		p.h.api.writeJSON(writer, http.StatusOK, catalog)
		return
	}
	if request.Method == http.MethodGet && path == "/v1/world/clock" {
		p.h.api.mu.Lock()
		clock := p.h.api.clockDocument()
		p.h.api.mu.Unlock()
		// The paid overseas targets in this fake are phase-2 yards, so the
		// fake clock reports the matching unlocked phase.
		clock["unlocked_phase"] = 2
		p.h.api.writeJSON(writer, http.StatusOK, clock)
		return
	}
	if request.Method == http.MethodGet && path == "/v1/contract/list" {
		p.h.api.writeJSON(writer, http.StatusOK, expansionFactorFixture().Contracts)
		return
	}
	if request.Method == http.MethodGet && path == "/v1/world/port" && request.URL.Query().Get("port_id") == recoveryHomePort {
		p.h.api.mu.Lock()
		detail := p.h.api.portDocument(recoveryHomePort)
		p.h.api.mu.Unlock()
		detail["coffee_house_gossip"] = expansionFactorFixture().Ports[recoveryHomePort].CoffeeHouseGossip
		p.h.api.writeJSON(writer, http.StatusOK, detail)
		return
	}
	if request.Method != http.MethodPost || path != "/v1/company/hire-factor" && path != "/v1/company/dismiss-factor" {
		p.downstream.ServeHTTP(writer, request)
		return
	}
	body, err := io.ReadAll(request.Body)
	if err != nil {
		p.h.t.Errorf("read factor request: %v", err)
		writer.WriteHeader(http.StatusBadRequest)
		return
	}
	var terms struct {
		PortID string `json:"port_id"`
	}
	if json.Unmarshal(body, &terms) != nil || !slices.Contains([]string{"yard-a", "demand"}, terms.PortID) || request.Header.Get("Idempotency-Key") == "" {
		p.h.t.Errorf("factor action lost its best yard or durable identity: %s", body)
		writer.WriteHeader(http.StatusBadRequest)
		return
	}
	p.h.api.mu.Lock()
	defer p.h.api.mu.Unlock()
	if p.status != 0 {
		p.h.api.record(request, body, p.status)
		p.h.api.writeProblem(writer, p.status, p.code, "factor domain refusal")
		return
	}
	hire := path == "/v1/company/hire-factor"
	if hire {
		if !slices.Contains(p.h.api.factorPorts, terms.PortID) {
			p.h.api.factorPorts = append(p.h.api.factorPorts, terms.PortID)
			p.h.api.vaultBalance["caribbean"] -= 75_000
		}
	} else {
		p.h.api.factorPorts = slices.DeleteFunc(p.h.api.factorPorts, func(port string) bool { return port == terms.PortID })
	}
	p.h.api.record(request, body, http.StatusOK)
	if p.uninterpretable && hire {
		p.h.api.writeJSON(writer, http.StatusOK, map[string]any{})
		return
	}
	p.h.api.writeJSON(writer, http.StatusOK, map[string]any{"port_id": terms.PortID, "has_factor": hire})
}

func refreshExpansionFactor(t *testing.T, h *recoveryHarness, runner *Runner) {
	t.Helper()
	runner.generation = uuid.NewString()
	if err := runner.refresh(h.ctx); err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC()
	for portID, detail := range runner.stateCache.Ports {
		for i := range detail.CoffeeHouseGossip {
			for j := range detail.CoffeeHouseGossip[i].Commodities {
				quote := &detail.CoffeeHouseGossip[i].Commodities[j]
				if quote.BestBidPence != nil {
					bid := *quote.BestBidPence * 100
					quote.BestBidPence = &bid
				}
			}
		}
		runner.stateCache.Ports[portID] = detail
	}
	ship := runner.stateCache.Ships[0].Ship
	for i := range runner.stateCache.Catalog.SeaZones {
		runner.stateCache.Catalog.SeaZones[i].BaseStormBPS = max(100, runner.stateCache.Catalog.SeaZones[i].BaseStormBPS)
	}
	zones := decode[[]ZoneObservation](runner.snapshot, "zones")
	zones = append(zones, ZoneObservation{SeaZoneID: "fixture_sea", NetPiracyBPS: 0})
	runner.snapshot["zones"] = encode(zones)
	for i := range 10 {
		portID := fmt.Sprintf("expansion-calibration-%02d", i)
		if _, exists := findPort(runner.stateCache.Catalog, portID); !exists {
			runner.stateCache.Catalog.Ports = append(runner.stateCache.Catalog.Ports, Port{ID: portID, SeaZoneID: "zone-north"})
		}
		route := Route{FromPort: ship.CurrentPortID, ToPort: portID, ShipClass: ship.ShipClass,
			Legs: []RouteLeg{{ToNode: "zone-north", DistanceNM: 100}, {ToNode: portID, DistanceNM: 100}}}
		runner.stateCache.Routes[routeKey(route.FromPort, route.ToPort, route.ShipClass)] = route
		runner.stateCache.HazardQuotes[fmt.Sprintf("expansion-calibration-%02d", i)] = HazardQuote{
			ShipID: ship.ID, Origin: route.FromPort, Destination: portID, ShipClass: ship.ShipClass, Laden: true,
			ExpectedLossBPS: 2, Month: runner.stateCache.Clock.Calendar.Month, ObservedAt: now,
			CannonsCount: 4, NetPiracyBPS: map[string]int64{"zone-north": 0},
		}
	}
	if err := h.db.RecordMetric(h.ctx, runner.account.ID, "net_worth_pence", 10_000_000); err != nil {
		t.Fatal(err)
	}
}

func TestExpansionFactorWithoutGossipBuysEvidenceNotCharterBackedHull(t *testing.T) {
	t.Parallel()
	for _, probe := range []bool{true, false} {
		t.Run(map[bool]string{true: "no gossip spread", false: "positive repeatable spot evidence"}[probe], func(t *testing.T) {
			h := newRecoveryHarness(t)
			newMeasuredExpansionFactorAPI(h)
			runner := h.runner(primaryTestKey)
			refreshExpansionFactor(t, h, runner)
			if probe {
				runner.stateCache.Ports = nil
			}
			changed, err := runner.considerExpansionAgent(h.ctx)
			if err != nil || !changed {
				t.Fatalf("probe=%t changed=%t err=%v", probe, changed, err)
			}
			mission, data := h.mission("__treasury")
			if mission.Destination != "yard-a" || data.Action == nil || data.Action.Stage != "hire-factor" {
				t.Fatalf("evidence admission did not select the yard hire: %+v %+v", mission, data.Action)
			}
			agents := decode[[]expansionAgent](runner.snapshot, "_expansion_agents")
			if len(agents) != 2 || agents[0].Probe != probe {
				t.Fatalf("gossip-free admission was not distinguishable from a priced pair: %+v", agents)
			}
			h.missingMission("__growth")
			if h.api.mutationCount("/v1/company/hire-factor") != 0 || h.api.mutationCount("/v1/shipyard/build-ship") != 0 {
				t.Fatal("admission sent an unjournaled hire or built on finite charter income")
			}
		})
	}
}

func TestExpansionFactorLifecycle(t *testing.T) {
	t.Parallel()
	h := newRecoveryHarness(t)
	newMeasuredExpansionFactorAPI(h)
	runner := h.runner(primaryTestKey)
	refreshExpansionFactor(t, h, runner)
	if changed, err := runner.considerExpansionAgent(h.ctx); err != nil || !changed {
		t.Fatalf("hire staging: changed=%t err=%v holds=%v", changed, err, runner.holding)
	}
	mission, data := h.mission("__treasury")
	if data.Action == nil || data.Action.Stage != "hire-factor" || mission.Destination != "yard-a" {
		t.Fatalf("best yard not staged: %+v %+v", mission, data.Action)
	}
	if changed, err := runner.considerExpansionAgent(h.ctx); err != nil || changed {
		t.Fatalf("duplicate pending hire: changed=%t err=%v", changed, err)
	}
	key := data.Action.ID
	before := time.Now().UTC()
	if changed, err := runner.dispatch(h.ctx, mission, &data); err != nil || !changed {
		t.Fatalf("hire dispatch: changed=%t err=%v", changed, err)
	}
	after := time.Now().UTC()
	mission, data = h.mission("__treasury")
	if mission.Phase != "done" || data.Action != nil || data.LastActionID != key {
		t.Fatalf("hire did not close control action: %+v %+v", mission, data)
	}
	calls := h.api.mutationsFor("/v1/company/hire-factor")
	if len(calls) != 1 || calls[0].IdempotencyKey != key {
		t.Fatalf("expected exactly one journaled hire: %+v", calls)
	}
	snapshot, err := h.db.Snapshot(h.ctx, h.account.ID)
	if err != nil {
		t.Fatal(err)
	}
	agents := decode[[]expansionAgent](snapshot, "_expansion_agents")
	if len(agents) != 2 || agents[0].Region != "caribbean" || agents[0].PortID != "yard-a" ||
		agents[0].Partner != "demand" || agents[0].HiredAt.Before(before) || agents[0].HiredAt.After(after) || !agents[1].HiredAt.IsZero() {
		t.Fatalf("missing durable UTC hire latch: %+v", agents)
	}
	if err := h.db.DeleteMission(h.ctx, h.account.ID, "__treasury"); err != nil {
		t.Fatal(err)
	}
	runner = h.runner(primaryTestKey)
	refreshExpansionFactor(t, h, runner)
	if changed, err := runner.manageExpansionAgents(h.ctx); err != nil || !changed {
		t.Fatalf("partner hire did not resume after restart: changed=%t err=%v", changed, err)
	}
	mission, data = h.mission("__treasury")
	if data.Action == nil || data.Action.Stage != "hire-factor" || mission.Destination != "demand" {
		t.Fatalf("partner was not staged next: %+v %+v", mission, data.Action)
	}
	if changed, err := runner.dispatch(h.ctx, mission, &data); err != nil || !changed {
		t.Fatalf("partner hire dispatch: changed=%t err=%v", changed, err)
	}
	if err := h.db.DeleteMission(h.ctx, h.account.ID, "__treasury"); err != nil {
		t.Fatal(err)
	}
	refreshExpansionFactor(t, h, runner)
	if changed, err := runner.manageExpansionAgents(h.ctx); err != nil || changed {
		t.Fatalf("fresh pair dismissed early: changed=%t err=%v", changed, err)
	}
	agents = decode[[]expansionAgent](runner.snapshot, "_expansion_agents")
	for i := range agents {
		agents[i].HiredAt = time.Now().UTC().Add(-2*time.Hour - time.Second)
	}
	runner.snapshot["_expansion_agents"] = encode(agents)
	if err := h.db.SaveSnapshot(h.ctx, h.account.ID, runner.snapshot); err != nil {
		t.Fatal(err)
	}
	for _, capacity := range []bool{false, true} {
		growth := missionData{BuildPort: "yard-a"}
		if capacity {
			growth.BuildPort = ""
			growth.Capacity = &capacityIntent{BuildPort: "yard-a"}
		}
		h.putMission("__growth", uuid.NewString(), "growth_build", growth)
		if changed, err := runner.manageExpansionAgents(h.ctx); err != nil || changed {
			t.Fatalf("agent dismissed during yard build: changed=%t err=%v", changed, err)
		}
	}
	if err := h.db.DeleteMission(h.ctx, h.account.ID, "__growth"); err != nil {
		t.Fatal(err)
	}
	runner.snapshot["_capacity_growth_port"] = encode("yard-a")
	if changed, err := runner.manageExpansionAgents(h.ctx); err != nil || changed {
		t.Fatalf("agent dismissed while accepted capacity waits for funding: changed=%t err=%v", changed, err)
	}
	delete(runner.snapshot, "_capacity_growth_port")
	for _, plan := range []Plan{{Origin: "yard-a"}, {Destination: "yard-a"}} {
		h.putMission(recoveryShipBlastoise, uuid.NewString(), "buy", missionData{Plan: plan})
		if changed, err := runner.manageExpansionAgents(h.ctx); err != nil || changed {
			t.Fatalf("agent dismissed while an operating mission needs its port: changed=%t err=%v", changed, err)
		}
		if err := h.db.DeleteMission(h.ctx, h.account.ID, recoveryShipBlastoise); err != nil {
			t.Fatal(err)
		}
	}
	if changed, err := runner.manageExpansionAgents(h.ctx); err != nil || !changed {
		t.Fatalf("aged agent dismissal staging: changed=%t err=%v", changed, err)
	}
	mission, data = h.mission("__treasury")
	if data.Action == nil || data.Action.Stage != "dismiss-factor" {
		t.Fatalf("no journaled dismissal: %+v", data.Action)
	}
	if changed, err := runner.dispatch(h.ctx, mission, &data); err != nil || !changed {
		t.Fatalf("dismiss dispatch: changed=%t err=%v", changed, err)
	}
	mission, data = h.mission("__treasury")
	if mission.Phase != "done" || data.Action != nil || h.api.mutationCount("/v1/company/dismiss-factor") != 1 {
		t.Fatalf("dismissal did not close control action: %+v %+v", mission, data)
	}
	if err := h.db.DeleteMission(h.ctx, h.account.ID, "__treasury"); err != nil {
		t.Fatal(err)
	}
	refreshExpansionFactor(t, h, runner)
	if changed, err := runner.manageExpansionAgents(h.ctx); err != nil || !changed {
		t.Fatalf("second factor dismissal staging: changed=%t err=%v", changed, err)
	}
	mission, data = h.mission("__treasury")
	if changed, err := runner.dispatch(h.ctx, mission, &data); err != nil || !changed {
		t.Fatalf("second factor dismissal dispatch: changed=%t err=%v", changed, err)
	}
	if err := h.db.DeleteMission(h.ctx, h.account.ID, "__treasury"); err != nil {
		t.Fatal(err)
	}
	runner = h.runner(primaryTestKey)
	refreshExpansionFactor(t, h, runner)
	if changed, err := runner.considerExpansionAgent(h.ctx); err != nil || changed {
		t.Fatalf("dismissed region rehired: changed=%t err=%v", changed, err)
	}
	if changed, err := runner.manageExpansionAgents(h.ctx); err != nil || changed {
		t.Fatalf("absent agent dismissed twice: changed=%t err=%v", changed, err)
	}
	if h.api.mutationCount("/v1/company/hire-factor") != 2 || len(decode[[]expansionAgent](runner.snapshot, "_expansion_agents")) != 2 {
		t.Fatal("dismissal erased the pair's region retry history")
	}
}

func TestExpansionFactorUnpaidRefusalRetriesWithoutRealizedLoss(t *testing.T) {
	t.Parallel()
	for _, test := range []struct {
		code   string
		status int
	}{
		{code: "no_factor_at_cove", status: http.StatusForbidden},
		{code: "no_port_presence", status: http.StatusForbidden},
		{code: "insufficient_funds", status: http.StatusConflict},
	} {
		t.Run(test.code, func(t *testing.T) {
			h := newRecoveryHarness(t)
			proxy := newMeasuredExpansionFactorAPI(h)
			proxy.code, proxy.status = test.code, test.status
			runner := h.runner(primaryTestKey)
			refreshExpansionFactor(t, h, runner)
			if changed, err := runner.considerExpansionAgent(h.ctx); err != nil || !changed {
				t.Fatalf("stage refused hire: changed=%t err=%v", changed, err)
			}
			mission, data := h.mission("__treasury")
			if changed, err := runner.dispatch(h.ctx, mission, &data); err != nil || !changed {
				t.Fatalf("domain refusal stopped execution: changed=%t err=%v", changed, err)
			}
			mission, data = h.mission("__treasury")
			agents := decode[[]expansionAgent](runner.snapshot, "_expansion_agents")
			if len(agents) != 0 {
				t.Fatalf("refused hire retained unused awaiting records: %+v", agents)
			}
			if mission.Phase != "done" || data.Action != nil {
				t.Fatalf("refusal retained factor control: %+v %+v", mission, data)
			}
			if err := h.db.DeleteMission(h.ctx, h.account.ID, "__treasury"); err != nil {
				t.Fatal(err)
			}
			runner = h.runner(primaryTestKey)
			refreshExpansionFactor(t, h, runner)
			if changed, err := runner.considerExpansionAgent(h.ctx); err != nil || changed {
				t.Fatalf("refused region rehired after restart: changed=%t err=%v", changed, err)
			}
			outcomes := decode[map[string]expansionOutcome](runner.snapshot, "_expansion_agent_outcomes")
			outcome := outcomes["caribbean"]
			if outcome.Losses != 0 || !outcome.RetryAt.IsZero() || !time.Now().Before(runner.expansionRetryUntil("caribbean")) ||
				!decode[time.Time](runner.snapshot, "_expansion_last_started_at").IsZero() || expansionRegionsExhausted(runner) {
				t.Fatalf("never-paid refusal became an exhausting loss or lost its retry: %+v", outcome)
			}
			outcome.AttemptAt = time.Now().UTC().Add(-3*time.Hour - time.Second)
			outcomes["caribbean"] = outcome
			runner.snapshot["_expansion_agent_outcomes"] = encode(outcomes)
			runner.snapshot["_expansion_agent_retries"] = encode(map[string]time.Time{"caribbean": outcome.AttemptAt})
			if changed, err := runner.considerExpansionAgent(h.ctx); err != nil || !changed {
				t.Fatalf("never-paid region could not retry after its window plus one real hour: changed=%t err=%v", changed, err)
			}
		})
	}
}

func TestExpansionFactorFundsRegionalFeeFirst(t *testing.T) {
	t.Parallel()
	h := newRecoveryHarness(t)
	newMeasuredExpansionFactorAPI(h)
	h.api.setVaultBalance("caribbean", 0)
	h.api.mu.Lock()
	h.api.rates = append(h.api.rates, Rate{FromRegion: "north", ToRegion: "caribbean", FeeBPS: 100})
	h.api.mu.Unlock()
	runner := h.runner(primaryTestKey)
	refreshExpansionFactor(t, h, runner)
	if changed, err := runner.considerExpansionAgent(h.ctx); err != nil || !changed {
		t.Fatalf("regional funding: changed=%t err=%v holds=%v", changed, err, runner.holding)
	}
	mission, data := h.mission("__treasury")
	if data.Action == nil || data.Action.Stage != "remit" || mission.Destination != "caribbean" {
		t.Fatalf("hire preceded regional funding: %+v %+v", mission, data.Action)
	}
	if changed, err := runner.dispatch(h.ctx, mission, &data); err != nil || !changed {
		t.Fatalf("funding dispatch: changed=%t err=%v", changed, err)
	}
	if err := h.db.DeleteMission(h.ctx, h.account.ID, "__treasury"); err != nil {
		t.Fatal(err)
	}
	runner = h.runner(primaryTestKey)
	refreshExpansionFactor(t, h, runner)
	input, err := runner.committedPlanningInput(h.ctx, ShipState{})
	if err != nil {
		t.Fatal(err)
	}
	if input.Balances["caribbean"] < 2*expansionAgentHireFeePence+runner.protectedReservePence() {
		t.Fatalf("funding omitted both regional fees or reserve: %d", input.Balances["caribbean"])
	}
	if changed, err := runner.manageExpansionAgents(h.ctx); err != nil || !changed {
		t.Fatalf("funded hire staging: changed=%t err=%v", changed, err)
	}
	_, data = h.mission("__treasury")
	if data.Action == nil || data.Action.Stage != "hire-factor" || h.api.mutationCount("/v1/company/hire-factor") != 0 {
		t.Fatalf("funded hire was not staged separately: %+v", data.Action)
	}
}

func TestExpansionFactorGuards(t *testing.T) {
	t.Parallel()
	for _, guard := range []string{"observe", "factor limit", "pending treasury", "unfunded"} {
		t.Run(guard, func(t *testing.T) {
			h := newRecoveryHarness(t)
			newMeasuredExpansionFactorAPI(h)
			runner := h.runner(primaryTestKey)
			refreshExpansionFactor(t, h, runner)
			switch guard {
			case "observe":
				runner.opts.Execute = false
			case "factor limit":
				runner.stateCache.Factors = make([]string, 12)
			case "pending treasury":
				h.putMission("__treasury", uuid.NewString(), "settings", missionData{})
			case "unfunded":
				for i := range runner.stateCache.Vaults {
					runner.stateCache.Vaults[i].BalancePence = 0
				}
			}
			if changed, err := runner.considerExpansionAgent(h.ctx); err != nil || changed {
				t.Fatalf("%s did not prevent hire: changed=%t err=%v", guard, changed, err)
			}
			if guard != "pending treasury" {
				h.missingMission("__treasury")
			}
		})
	}
}

func TestExpansionFactorDismissRefusal(t *testing.T) {
	t.Parallel()
	h := newRecoveryHarness(t)
	proxy := newMeasuredExpansionFactorAPI(h)
	proxy.status, proxy.code = http.StatusForbidden, "no_port_presence"
	h.api.setFactorPorts("yard-a")
	runner := h.runner(primaryTestKey)
	refreshExpansionFactor(t, h, runner)
	agents := []expansionAgent{{PortID: "yard-a", Region: "caribbean", HiredAt: time.Now().UTC().Add(-3 * time.Hour)}}
	runner.snapshot["_expansion_agents"] = encode(agents)
	if err := h.db.SaveSnapshot(h.ctx, h.account.ID, runner.snapshot); err != nil {
		t.Fatal(err)
	}
	if changed, err := runner.manageExpansionAgents(h.ctx); err != nil || !changed {
		t.Fatalf("dismiss staging: changed=%t err=%v", changed, err)
	}
	mission, data := h.mission("__treasury")
	if changed, err := runner.dispatch(h.ctx, mission, &data); err != nil || !changed {
		t.Fatalf("dismiss refusal stopped execution: changed=%t err=%v", changed, err)
	}
	mission, data = h.mission("__treasury")
	if mission.Phase != "done" || data.Action != nil || len(decode[[]expansionAgent](runner.snapshot, "_expansion_agents")) != 1 {
		t.Fatalf("dismiss refusal retained control or erased latch: %+v %+v", mission, data)
	}
	if changed, err := runner.manageExpansionAgents(h.ctx); err != nil || changed {
		t.Fatalf("dismissal restaged during refusal cooldown: changed=%t err=%v", changed, err)
	}
	if calls := h.api.mutationCount("/v1/company/dismiss-factor"); calls != 1 {
		t.Fatalf("dismissal refusal retried before cooldown: %d calls", calls)
	}
}

func TestExpansionFactorPendingPairBlocksCascadingExploration(t *testing.T) {
	t.Parallel()
	h := newRecoveryHarness(t)
	newMeasuredExpansionFactorAPI(h)
	runner := h.runner(primaryTestKey)
	refreshExpansionFactor(t, h, runner)
	created := time.Now().UTC().Add(-25 * time.Hour)
	runner.snapshot["_expansion_agents"] = encode([]expansionAgent{
		{PortID: recoveryHomePort, Partner: recoveryFarPort, Region: "north", CreatedAt: created, HiredAt: time.Now().UTC()},
		{PortID: recoveryFarPort, Partner: recoveryHomePort, Region: "north", CreatedAt: created},
	})
	input, err := runner.committedPlanningInput(h.ctx, ShipState{})
	if err != nil {
		t.Fatal(err)
	}
	if pairs := expansionAgentYards(runner.stateCache, input); len(pairs) == 0 {
		t.Fatal("fixture has no second region worth exploring")
	}
	if changed, err := runner.considerExpansionAgent(h.ctx); err != nil || changed {
		t.Fatalf("existing factor allowed cascading paid exploration: changed=%t err=%v", changed, err)
	}
	h.missingMission("__treasury")
	if row := expansionFactorDecision(t, h, "north"); !strings.Contains(row.Reason, "existing exploration") {
		t.Fatalf("existing exploration guard was not persisted: %+v", row)
	}
}

func TestExpansionFactorReconcilesUninterpretableHireFromFactors(t *testing.T) {
	t.Parallel()
	h := newRecoveryHarness(t)
	proxy := newMeasuredExpansionFactorAPI(h)
	proxy.uninterpretable = true
	runner := h.runner(primaryTestKey)
	refreshExpansionFactor(t, h, runner)
	if changed, err := runner.considerExpansionAgent(h.ctx); err != nil || !changed {
		t.Fatalf("hire staging: changed=%t err=%v", changed, err)
	}
	mission, data := h.mission("__treasury")
	if _, err := runner.dispatch(h.ctx, mission, &data); err != nil {
		t.Fatal(err)
	}
	refreshExpansionFactor(t, h, runner)
	mission, data = h.mission("__treasury")
	if changed, err := runner.dispatch(h.ctx, mission, &data); err != nil || !changed {
		t.Fatalf("observed hire reconciliation: changed=%t err=%v", changed, err)
	}
	mission, data = h.mission("__treasury")
	agents := decode[[]expansionAgent](runner.snapshot, "_expansion_agents")
	if mission.Phase != "done" || data.Action != nil || len(agents) != 2 || agents[0].PortID != "yard-a" || agents[0].HiredAt.IsZero() || !agents[1].HiredAt.IsZero() {
		t.Fatalf("observed factor did not release control and persist agent: %+v %+v %+v", mission, data, agents)
	}
	if calls := h.api.mutationCount("/v1/company/hire-factor"); calls != 1 {
		t.Fatalf("reconciliation replayed hire: %d calls", calls)
	}
}

func TestExpansionFactorCanEstimateFromStaleRegionalQuote(t *testing.T) {
	t.Parallel()
	h := newRecoveryHarness(t)
	newMeasuredExpansionFactorAPI(h)
	runner := h.runner(primaryTestKey)
	refreshExpansionFactor(t, h, runner)
	for key, meta := range runner.meta {
		if strings.HasPrefix(key, "yard_quotes/") {
			meta.UpdatedAt = time.Now().Add(-24 * time.Hour)
			runner.meta[key] = meta
		}
	}
	if changed, err := runner.considerExpansionAgent(h.ctx); err != nil || !changed {
		t.Fatalf("stale regional capital estimate blocked exploration: changed=%t err=%v holds=%v", changed, err, runner.holding)
	}
	_, data := h.mission("__treasury")
	if data.Action == nil || data.Action.Stage != "hire-factor" {
		t.Fatalf("stale estimate did not stage a hire: %+v", data.Action)
	}
}

func TestExpansionFactorIgnoresTerminalAndSelfPresentMissions(t *testing.T) {
	t.Parallel()
	for _, phase := range []string{"done", "settled", "maintenance_done", "growth_rejected", "stationary"} {
		t.Run(phase, func(t *testing.T) {
			h := newRecoveryHarness(t)
			newMeasuredExpansionFactorAPI(h)
			h.api.setFactorPorts("yard-a")
			if phase == "stationary" {
				h.api.setFleet([]map[string]any{stationaryShipRow(recoveryShipBlastoise, "Blastoise", "yard-a", "berthed", 0, 4)})
			}
			runner := h.runner(primaryTestKey)
			refreshExpansionFactor(t, h, runner)
			runner.snapshot["_expansion_agents"] = encode([]expansionAgent{{PortID: "yard-a", Region: "caribbean", HiredAt: time.Now().Add(-3 * time.Hour)}})
			h.putMission(recoveryShipBlastoise, uuid.NewString(), phase, missionData{Plan: Plan{Origin: "yard-a", Destination: recoveryFarPort}})
			if changed, err := runner.manageExpansionAgents(h.ctx); err != nil || !changed {
				t.Fatalf("%s mission unnecessarily retained factor upkeep: changed=%t err=%v", phase, changed, err)
			}
			_, data := h.mission("__treasury")
			if data.Action == nil || data.Action.Stage != "dismiss-factor" {
				t.Fatalf("redundant factor was not dismissed: %+v", data.Action)
			}
		})
	}
}

func TestExpansionFactorGossipBuildPaybackBoundary(t *testing.T) {
	t.Parallel()
	h := newRecoveryHarness(t)
	newMeasuredExpansionFactorAPI(h)
	runner := h.runner(primaryTestKey)
	refreshExpansionFactor(t, h, runner)
	evidence := capacityMeasuredEvidence(t, h, runner, time.Now().UTC())
	input, reason, err := runner.capacityFleetInput(h.ctx, &evidence)
	if err != nil || reason != "" {
		t.Fatalf("regional fixture: %+v %q %v", input, reason, err)
	}
	yard, _ := findPort(runner.stateCache.Catalog, "yard-a")
	partner, _ := findPort(runner.stateCache.Catalog, "demand")
	control, _, _, err := runner.expansionBuildEstimate(h.ctx, yard, partner, input, evidence.asOf)
	if err != nil || !control.Accepted {
		t.Fatalf("fixture has no admissible observed quote: %+v %v", control, err)
	}
	for _, accepted := range []bool{true, false} {
		input.MaxPaybackDays = control.PaybackGameDays
		if !accepted {
			input.MaxPaybackDays = math.Nextafter(input.MaxPaybackDays, 0)
		}
		decision, _, _, err := runner.expansionBuildEstimate(h.ctx, yard, partner, input, evidence.asOf)
		if err != nil || decision.Accepted != accepted || !decision.Accepted && !strings.Contains(decision.Reason, "payback") {
			t.Fatalf("%.9f game-day payback: %+v %v", input.MaxPaybackDays, decision, err)
		}
	}
	input.MeasuredNetAvailable = false
	input.MeasuredRatePencePerGameDay = 0
	input.MaxPaybackDays = control.PaybackGameDays
	if decision, _, _, err := runner.expansionBuildEstimate(h.ctx, yard, partner, input, evidence.asOf); err != nil || !decision.Accepted || decision.EvidenceBasis != "gossip estimate" {
		t.Fatalf("European R gated a labelled regional gossip estimate: %+v %v", decision, err)
	}
}

func TestExpansionFactorHirePreflightRechecksRegionalRealizedBuild(t *testing.T) {
	t.Parallel()
	h := newRecoveryHarness(t)
	newMeasuredExpansionFactorAPI(h)
	runner := h.runner(primaryTestKey)
	refreshExpansionFactor(t, h, runner)
	if changed, err := runner.considerExpansionAgent(h.ctx); err != nil || !changed {
		t.Fatalf("stage admissible hire: %t %v", changed, err)
	}
	mission, data := h.mission("__treasury")
	if err := runner.newActionPreflight(h.ctx, mission, &data); err != nil {
		t.Fatalf("unchanged admitted build lost its hire: %v", err)
	}
	persistExpansionRegionalIncome(t, h, "expansion-loss-before-hire", 1)
	if err := runner.newActionPreflight(h.ctx, mission, &data); err == nil || !strings.Contains(err.Error(), "upkeep") {
		t.Fatalf("regional realized earnings did not veto the staged hire: %v", err)
	}
	if h.api.mutationCount("/v1/company/hire-factor") != 0 {
		t.Fatal("hire reached transport after its build ceased to be admissible")
	}
}

func TestExpansionFactorWithoutQuoteProbesWithOrWithoutMeasuredIncome(t *testing.T) {
	t.Parallel()
	for _, measured := range []bool{false, true} {
		t.Run(map[bool]string{false: "unavailable measured rate", true: "available measured rate"}[measured], func(t *testing.T) {
			h := newRecoveryHarness(t)
			if measured {
				newMeasuredExpansionFactorAPI(h)
			} else {
				newExpansionFactorAPI(h)
			}
			runner := h.runner(primaryTestKey)
			refreshExpansionFactor(t, h, runner)
			runner.stateCache.Quotes = nil
			changed, err := runner.considerExpansionAgent(h.ctx)
			if err != nil || !changed {
				t.Fatalf("quote-free probe changed=%t, measured income=%t, error=%v", changed, measured, err)
			}
			agents := decode[[]expansionAgent](runner.snapshot, "_expansion_agents")
			if len(agents) != 2 || !agents[0].Probe || !agents[1].Probe {
				t.Fatalf("fresh safe evidence did not permit a quote-free probe: %+v", agents)
			}
			_, data := h.mission("__treasury")
			if data.Action == nil || data.Action.Stage != "hire-factor" {
				t.Fatalf("quote-free probe authorized a hull instead of evidence: %+v", data)
			}
			if h.api.mutationCount("/v1/company/hire-factor") != 0 || h.api.mutationCount("/v1/shipyard/build-ship") != 0 {
				t.Fatal("quote-free admission bypassed immutable staging or built a hull")
			}
		})
	}
}

func TestExpansionFactorHirePreflightPreservesMissingQuoteReason(t *testing.T) {
	t.Parallel()
	h := newRecoveryHarness(t)
	newMeasuredExpansionFactorAPI(h)
	runner := h.runner(primaryTestKey)
	refreshExpansionFactor(t, h, runner)
	if changed, err := runner.considerExpansionAgent(h.ctx); err != nil || !changed {
		t.Fatalf("stage admitted hire: %t %v", changed, err)
	}
	mission, data := h.mission("__treasury")
	runner.stateCache.Quotes = nil
	if err := runner.newActionPreflight(h.ctx, mission, &data); err == nil || err.Error() != expansionAgentQuoteReason {
		t.Fatalf("hire preflight masked the missing observed quote: %v", err)
	}
	if h.api.mutationCount("/v1/company/hire-factor") != 0 {
		t.Fatal("missing observed quote escaped hire preflight")
	}
}

func TestExpansionStrandedRegionalCashFundsAdmittedHomeBuild(t *testing.T) {
	t.Parallel()
	h, runner := capacityExecutionFixture(t, false)
	protected, err := runner.treasuryReserves(h.ctx)
	if err != nil {
		t.Fatal(err)
	}
	h.api.setVaultBalance("north", protected["north"])
	h.api.setVaultBalance("south", 50_000_000)
	refreshExpansionFactor(t, h, runner)
	evidence := capacityMeasuredEvidence(t, h, runner, time.Now().UTC())
	if changed, err := runner.considerCapacityGrowthAt(h.ctx, &evidence, recoveryHomePort, recoveryHull); err != nil || !changed {
		t.Fatalf("remote free cash was stranded instead of funding the build: %t %v", changed, err)
	}
	mission, data := h.mission("__treasury")
	var transfer struct {
		From string `json:"from_region"`
		To   string `json:"to_region"`
	}
	if data.Action == nil || data.Action.Stage != "remit" || json.Unmarshal(data.Action.Body, &transfer) != nil ||
		transfer.From != "south" || transfer.To != "north" {
		t.Fatalf("remote cash was not repatriated through the journal: %+v %+v", data.Action, transfer)
	}
	if changed, err := runner.dispatch(h.ctx, mission, &data); err != nil || !changed {
		t.Fatalf("repatriate remote cash: %t %v", changed, err)
	}
	refreshExpansionFactor(t, h, runner)
	evidence = capacityMeasuredEvidence(t, h, runner, time.Now().UTC())
	if changed, err := runner.considerCapacityGrowthAt(h.ctx, &evidence, recoveryHomePort, recoveryHull); err != nil || !changed {
		t.Fatalf("repatriated cash did not stage its admitted build: %t %v", changed, err)
	}
	_, build := h.mission("__growth")
	if build.Action == nil || build.Action.Stage != "build" {
		t.Fatalf("repatriation did not release construction: %+v", build.Action)
	}
}

func TestExpansionLosingRegionBackoffScheduleAndCap(t *testing.T) {
	t.Parallel()
	runner := &Runner{snapshot: make(map[string]json.RawMessage)}
	agent := expansionAgent{Region: "america", CreatedAt: time.Now().UTC(), HiredAt: time.Now().UTC()}
	for loss, delay := range []time.Duration{24 * time.Hour, 48 * time.Hour, 96 * time.Hour, 96 * time.Hour, 96 * time.Hour} {
		outcomes := decode[map[string]expansionOutcome](runner.snapshot, "_expansion_agent_outcomes")
		if outcomes == nil {
			outcomes = make(map[string]expansionOutcome)
		}
		outcome := outcomes[agent.Region]
		outcome.AttemptAt, outcome.Reason = time.Now().UTC(), ""
		agent.CreatedAt, agent.HiredAt = outcome.AttemptAt, outcome.AttemptAt
		outcomes[agent.Region] = outcome
		runner.snapshot["_expansion_agent_outcomes"] = encode(outcomes)
		start := time.Now().UTC()
		runner.markExpansionLoss(agent, "no profitable executable plan")
		result := decode[map[string]expansionOutcome](runner.snapshot, "_expansion_agent_outcomes")[agent.Region]
		if result.Losses != loss+1 || result.RetryAt.Before(start.Add(delay)) || result.RetryAt.After(time.Now().Add(delay)) ||
			!runner.expansionRetryUntil(agent.Region).Equal(result.RetryAt) {
			t.Fatalf("loss %d has no capped exponential regional backoff: %+v", loss+1, result)
		}
		runner.markExpansionLoss(agent, "second pair member")
		if repeated := decode[map[string]expansionOutcome](runner.snapshot, "_expansion_agent_outcomes")[agent.Region]; repeated.Losses != result.Losses || !repeated.RetryAt.Equal(result.RetryAt) {
			t.Fatalf("one losing pair counted twice: %+v %+v", result, repeated)
		}
	}
}

func TestExpansionPrehireRejectsUnsafeArrivalZone(t *testing.T) {
	t.Parallel()
	for _, portID := range []string{"yard-a", "demand"} {
		t.Run(portID, func(t *testing.T) {
			h := newRecoveryHarness(t)
			newMeasuredExpansionFactorAPI(h)
			runner := h.runner(primaryTestKey)
			refreshExpansionFactor(t, h, runner)
			control, reason, err := runner.expansionAdmission(h.ctx)
			if err != nil || reason != "" {
				t.Fatalf("safe regional control: %+v %q %v", control, reason, err)
			}
			unsafe := Zone{ID: "unsafe-arrival", ZoneClass: "regional", BaseStormBPS: 100}
			runner.stateCache.Catalog.SeaZones = append(runner.stateCache.Catalog.SeaZones, unsafe)
			for i := range runner.stateCache.Catalog.Ports {
				port := runner.stateCache.Catalog.Ports[i]
				if port.ID == portID || portID == "yard-a" && port.HasShipyard && port.BankingRegion == "caribbean" ||
					portID == "demand" && port.BankingRegion == "caribbean" && port.ID != "yard-a" {
					runner.stateCache.Catalog.Ports[i].SeaZoneID = unsafe.ID
				}
			}
			zones := decode[[]ZoneObservation](runner.snapshot, "zones")
			runner.snapshot["zones"] = encode(append(zones, ZoneObservation{SeaZoneID: unsafe.ID, NetPiracyBPS: 51}))
			if changed, err := runner.considerExpansionAgent(h.ctx); err != nil || changed {
				t.Fatalf("unsafe %s paid for expansion: %t %v", portID, changed, err)
			}
			if row := expansionFactorDecision(t, h, "caribbean"); !strings.Contains(row.Reason, "arrival-zone risk exceeds") {
				t.Fatalf("unsafe arrival did not retain its regional hazard rejection: %+v", row)
			}
			h.missingMission("__treasury")
		})
	}
}

func TestExpansionLosingVerdictSurvivesGossipRateAndQuoteChanges(t *testing.T) {
	t.Parallel()
	h := newRecoveryHarness(t)
	newMeasuredExpansionFactorAPI(h)
	runner := h.runner(primaryTestKey)
	refreshExpansionFactor(t, h, runner)
	admission, reason, err := runner.expansionAdmission(h.ctx)
	if err != nil || reason != "" {
		t.Fatalf("admissible expansion control: %q %v", reason, err)
	}
	deadline := time.Now().Add(24 * time.Hour)
	runner.snapshot["_expansion_agent_outcomes"] = encode(map[string]expansionOutcome{
		admission.region: {
			AttemptAt: time.Now().Add(-48 * time.Hour), RetryAt: deadline,
			Losses: 2, Reason: "no profitable executable plan",
		},
	})
	for key, quote := range runner.stateCache.Quotes {
		if quote.TotalCostPence == nil {
			continue
		}
		price := *quote.TotalCostPence + ceilBPS(*quote.TotalCostPence, 500)
		quote.TotalCostPence = &price
		runner.stateCache.Quotes[key] = quote
	}
	for portID, detail := range runner.stateCache.Ports {
		for i := range detail.CoffeeHouseGossip {
			for j := range detail.CoffeeHouseGossip[i].Commodities {
				quote := &detail.CoffeeHouseGossip[i].Commodities[j]
				if quote.BestBidPence != nil {
					bid := *quote.BestBidPence * 2
					quote.BestBidPence = &bid
				}
			}
		}
		runner.stateCache.Ports[portID] = detail
	}
	persistExpansionRegionalIncome(t, h, "changed-expansion-income", 50_000_000)
	if changed, err := runner.considerExpansionAgent(h.ctx); err != nil || changed {
		t.Fatalf("changing economics reopened a blocked losing region: %t %v", changed, err)
	}
	if row := expansionFactorDecision(t, h, admission.region); !strings.Contains(row.Reason, "retry deadline") {
		t.Fatalf("losing verdict was not persisted: %+v", row)
	}
	if outcome := decode[map[string]expansionOutcome](runner.snapshot, "_expansion_agent_outcomes")[admission.region]; outcome.Losses != 2 || !outcome.RetryAt.Equal(deadline) {
		t.Fatalf("economic changes reset the loss schedule: %+v", outcome)
	}
	h.missingMission("__treasury")
}

func TestExpansionSavingsRequiresReciprocalLivePair(t *testing.T) {
	t.Parallel()
	now := time.Now().UTC()
	agents := []expansionAgent{
		{PortID: "yard", Partner: "demand", HiredAt: now.Add(-3 * time.Hour)},
		{PortID: "demand", Partner: "yard", HiredAt: now.Add(-3 * time.Hour)},
	}
	runner := &Runner{}
	if runner.expansionPairLive(agents, "yard", now) {
		t.Fatal("dismissed historical pair still authorized paired savings")
	}
	runner.stateCache.Factors = []string{"yard", "demand"}
	if !runner.expansionPairLive(agents, "yard", now) {
		t.Fatal("live hired pair lost paired savings eligibility")
	}
	agents[1].Partner = ""
	if runner.expansionPairLive(agents, "yard", now) {
		t.Fatal("dangling mate authorized paired savings")
	}
	agents[1].Partner, agents[1].HiredAt = "yard", now
	runner.stateCache.Factors = nil
	if !runner.expansionPairLive(agents, "yard", now) {
		t.Fatal("a pair still inside its floor lost savings eligibility")
	}
}

func TestExpansionPartnerFundingCapsOtherMissionCommitments(t *testing.T) {
	t.Parallel()
	h := newRecoveryHarness(t)
	newMeasuredExpansionFactorAPI(h)
	runner := h.runner(primaryTestKey)
	refreshExpansionFactor(t, h, runner)
	base, err := runner.treasuryReserves(h.ctx)
	if err != nil {
		t.Fatal(err)
	}
	input := PlanningInput{
		Balances:        map[string]int64{"north": 100_000, "caribbean": 1_000_000},
		ReserveByRegion: map[string]int64{"north": 50_000_000, "caribbean": 200_000},
	}
	required, err := runner.expansionRequired(h.ctx, input, "caribbean", expansionAgentHireFeePence)
	if err != nil {
		t.Fatal(err)
	}
	if want := max(base["north"], input.Balances["north"]); required["north"] != want {
		t.Fatalf("partner controller tried funding another mission's raw shortfall: got=%d want=%d", required["north"], want)
	}
	want := growthCashTotal(max(base["caribbean"], input.ReserveByRegion["caribbean"], runner.protectedReservePence()), expansionAgentHireFeePence)
	if required["caribbean"] != want {
		t.Fatalf("partner fee or protected local reserve lost: got=%d want=%d", required["caribbean"], want)
	}
}
