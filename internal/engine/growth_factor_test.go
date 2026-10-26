package engine

import (
	"encoding/json"
	"io"
	"net/http"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
)

func expansionFactorFixture() accountState {
	xReward, yReward := int64(200), int64(150)
	now, ask := time.Now().UTC().Add(-time.Second), int64(100)
	return accountState{
		Clock: Clock{UnlockedPhase: 1},
		Catalog: Catalog{
			Ports: []Port{
				{ID: "yard-b", BankingRegion: "caribbean", SeaZoneID: "fixture_sea", HasShipyard: true, Phase: 1, Produces: []string{"x"}},
				{ID: "yard-a", BankingRegion: "caribbean", SeaZoneID: "fixture_sea", HasShipyard: true, Phase: 1, Produces: []string{"x"}, Manufactures: []string{"y"}},
				{ID: "demand", BankingRegion: "caribbean", SeaZoneID: "fixture_sea", Phase: 1},
				{ID: "cove", BankingRegion: "caribbean", SeaZoneID: "fixture_sea", HasShipyard: true, IsCove: true, Phase: 1, Produces: []string{"x", "y"}},
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

func expansionFactorInput(state accountState) PlanningInput {
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
		change func(*accountState)
		port   string
		score  int64
	}{
		{name: "positive observed spot spread", port: "yard-a", score: 50},
		{name: "observed supply need not be catalog production", change: func(state *accountState) {
			for i := range state.Catalog.Ports {
				state.Catalog.Ports[i].Produces, state.Catalog.Ports[i].Manufactures = nil, nil
			}
		}, port: "yard-a", score: 50},
		{name: "newer phase does not beat a richer older region", change: func(state *accountState) {
			ask, bid := int64(100), int64(140)
			state.Clock.UnlockedPhase = 2
			state.Catalog.Ports = append(state.Catalog.Ports,
				Port{ID: "new-yard", BankingRegion: "america", SeaZoneID: "fixture_sea", HasShipyard: true, Phase: 2},
				Port{ID: "new-demand", BankingRegion: "america", SeaZoneID: "fixture_sea", Phase: 2},
			)
			detail := state.Ports[recoveryHomePort]
			now := time.Now().UTC().Add(-time.Second)
			detail.CoffeeHouseGossip = append(detail.CoffeeHouseGossip,
				Gossip{SubjectPortID: "new-yard", SnapshotAt: now, Commodities: []GossipCommodity{{CommodityID: "x", SnapshotAt: now, BestAskPence: &ask}}},
				Gossip{SubjectPortID: "new-demand", SnapshotAt: now, Commodities: []GossipCommodity{{CommodityID: "x", SnapshotAt: now, BestBidPence: &bid}}},
			)
			state.Ports[recoveryHomePort] = detail
		}, port: "yard-a", score: 50},
		{name: "oceanic water cannot host a regional build", change: func(state *accountState) {
			state.Catalog.SeaZones[0].ZoneClass = "oceanic"
		}},
		{name: "demand at yard itself", change: func(state *accountState) {
			state.Catalog.Ports = state.Catalog.Ports[1:2]
		}},
		{name: "commodity missing from catalog", change: func(state *accountState) {
			state.Catalog.Commodities = []Commodity{{ID: "z"}}
		}},
		{name: "coves and locked phases", change: func(state *accountState) {
			state.Catalog.Ports = state.Catalog.Ports[2:]
		}},
		{name: "charter-only evidence", change: func(state *accountState) { state.Ports = nil }},
		{name: "no charters needed", change: func(state *accountState) { state.Contracts = nil }, port: "yard-a", score: 50},
		{name: "finite rewards cannot increase score", change: func(state *accountState) {
			reward := int64(1_000_000)
			for i := range state.Contracts {
				state.Contracts[i].RewardPerTonPence = &reward
			}
		}, port: "yard-a", score: 50},
		{name: "another region", change: func(state *accountState) {
			state.Catalog.Ports[2].BankingRegion = "america"
		}},
		{name: "no positive stressed spread", change: func(state *accountState) {
			ask := int64(1_000)
			for _, report := range state.Ports[recoveryHomePort].CoffeeHouseGossip {
				for i := range report.Commodities {
					report.Commodities[i].BestAskPence = &ask
				}
			}
		}},
		{name: "missing observed supply", change: func(state *accountState) {
			for _, report := range state.Ports[recoveryHomePort].CoffeeHouseGossip {
				for i := range report.Commodities {
					report.Commodities[i].BestAskPence = nil
				}
			}
		}},
		{name: "stale report", change: func(state *accountState) {
			detail := state.Ports[recoveryHomePort]
			for i := range detail.CoffeeHouseGossip {
				detail.CoffeeHouseGossip[i].SnapshotAt = time.Now().Add(-7 * time.Minute)
			}
			state.Ports[recoveryHomePort] = detail
		}},
		{name: "stale commodity", change: func(state *accountState) {
			for _, report := range state.Ports[recoveryHomePort].CoffeeHouseGossip {
				for i := range report.Commodities {
					report.Commodities[i].SnapshotAt = time.Now().Add(-7 * time.Minute)
				}
			}
		}},
		{name: "factor presence", change: func(state *accountState) { state.Factors = []string{"demand"} }},
		{name: "ship presence", change: func(state *accountState) {
			ship := ShipState{}
			ship.Ship.Status, ship.Ship.CurrentPortID = "sailing", "demand"
			state.Ships = []ShipState{ship}
		}},
		{name: "sunk ship is not presence", change: func(state *accountState) {
			ship := ShipState{}
			ship.Ship.Status, ship.Ship.CurrentPortID = "sunk", "demand"
			state.Ships = []ShipState{ship}
		}, port: "yard-a", score: 50},
		{name: "port ID breaks ties", port: "yard-a", score: 50},
	} {
		t.Run(test.name, func(t *testing.T) {
			state := expansionFactorFixture()
			if test.change != nil {
				test.change(&state)
			}
			port, score := expansionAgentYard(state, expansionFactorInput(state), nil, time.Now())
			if port.ID != test.port || score != test.score {
				t.Fatalf("got %s score %d, want %s score %d", port.ID, score, test.port, test.score)
			}
		})
	}
	now := time.Now()
	recent := []expansionAgent{{Region: "caribbean", HiredAt: now.Add(-time.Hour)}}
	state := expansionFactorFixture()
	if port, score := expansionAgentYard(state, expansionFactorInput(state), recent, now); port.ID != "" || score != 0 {
		t.Fatalf("recent region latch was ignored: %s score %d", port.ID, score)
	}
	expired := []expansionAgent{{Region: "caribbean", HiredAt: now.Add(-expansionRegionRetry - time.Minute)}}
	if port, _ := expansionAgentYard(state, expansionFactorInput(state), expired, now); port.ID != "yard-a" {
		t.Fatalf("expired region latch still blocked exploration: %q", port.ID)
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
			if port, score := expansionAgentYard(state, input, nil, input.Now); port.ID != "" || score != 0 {
				t.Fatalf("stale %s still selected %s score %d", dataset, port.ID, score)
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
	h.api.mu.Unlock()
	h.api.setVaultBalance("caribbean", 500_000)
	return proxy
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
		catalog["commodities"] = fixture.Catalog.Commodities
		for _, zone := range fixture.Catalog.SeaZones {
			catalog["sea_zones"] = append(catalog["sea_zones"].([]any), zone)
		}
		p.h.api.writeJSON(writer, http.StatusOK, catalog)
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
	if json.Unmarshal(body, &terms) != nil || terms.PortID != "yard-a" || request.Header.Get("Idempotency-Key") == "" {
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
}

func TestExpansionFactorRequiresNonCharterEvidenceBeforeHiring(t *testing.T) {
	t.Parallel()
	for _, charterOnly := range []bool{true, false} {
		t.Run(map[bool]string{true: "charter only", false: "positive repeatable spot evidence"}[charterOnly], func(t *testing.T) {
			h := newRecoveryHarness(t)
			newExpansionFactorAPI(h)
			runner := h.runner(primaryTestKey)
			refreshExpansionFactor(t, h, runner)
			if charterOnly {
				runner.stateCache.Ports = nil
			}
			changed, err := runner.considerExpansionAgent(h.ctx)
			if err != nil || changed == charterOnly {
				t.Fatalf("charterOnly=%t changed=%t err=%v", charterOnly, changed, err)
			}
			if charterOnly {
				h.missingMission("__treasury")
				if h.api.mutationCount("/v1/company/hire-factor") != 0 {
					t.Fatal("finite charter income caused a paid hire")
				}
			} else {
				mission, data := h.mission("__treasury")
				if mission.Destination != "yard-a" || data.Action == nil || data.Action.Stage != "hire-factor" {
					t.Fatalf("repeatable spot evidence did not select a hire: %+v %+v", mission, data.Action)
				}
			}
		})
	}
}

func TestExpansionFactorLifecycle(t *testing.T) {
	t.Parallel()
	h := newRecoveryHarness(t)
	newExpansionFactorAPI(h)
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
	if len(agents) != 1 || agents[0].Region != "caribbean" || agents[0].PortID != "yard-a" || agents[0].HiredAt.Before(before) || agents[0].HiredAt.Location() != time.UTC {
		t.Fatalf("missing durable UTC hire latch: %+v", agents)
	}
	if err := h.db.DeleteMission(h.ctx, h.account.ID, "__treasury"); err != nil {
		t.Fatal(err)
	}
	runner = h.runner(primaryTestKey)
	refreshExpansionFactor(t, h, runner)
	if changed, err := runner.manageExpansionAgents(h.ctx); err != nil || changed {
		t.Fatalf("fresh agent dismissed early: changed=%t err=%v", changed, err)
	}
	agents[0].HiredAt = time.Now().UTC().Add(-2*time.Hour - time.Second)
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
	runner = h.runner(primaryTestKey)
	refreshExpansionFactor(t, h, runner)
	if changed, err := runner.considerExpansionAgent(h.ctx); err != nil || changed {
		t.Fatalf("dismissed region rehired: changed=%t err=%v", changed, err)
	}
	if changed, err := runner.manageExpansionAgents(h.ctx); err != nil || changed {
		t.Fatalf("absent agent dismissed twice: changed=%t err=%v", changed, err)
	}
	if h.api.mutationCount("/v1/company/hire-factor") != 1 || len(decode[[]expansionAgent](runner.snapshot, "_expansion_agents")) != 1 {
		t.Fatal("dismissal erased permanent region latch")
	}
}

func TestExpansionFactorRefusalLatch(t *testing.T) {
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
			proxy := newExpansionFactorAPI(h)
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
			if test.status == http.StatusConflict && len(agents) != 0 {
				t.Fatalf("transient funding refusal permanently latched region: %+v", agents)
			}
			if test.status == http.StatusForbidden && len(agents) != 1 {
				t.Fatalf("definitive port refusal lost its region latch: %+v", agents)
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
			if test.status == http.StatusConflict {
				runner.snapshot["_expansion_agent_retry_at"] = encode(time.Now().Add(-time.Second))
				if changed, err := runner.considerExpansionAgent(h.ctx); err != nil || !changed {
					t.Fatalf("temporary refusal permanently excluded region: changed=%t err=%v", changed, err)
				}
			}
		})
	}
}

func TestExpansionFactorFundsRegionalFeeFirst(t *testing.T) {
	t.Parallel()
	h := newRecoveryHarness(t)
	newExpansionFactorAPI(h)
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
	if input.Balances["caribbean"] < 75_000+runner.opts.ReservePence {
		t.Fatalf("funding omitted regional fee or reserve: %d", input.Balances["caribbean"])
	}
	if changed, err := runner.considerExpansionAgent(h.ctx); err != nil || !changed {
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
			newExpansionFactorAPI(h)
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
	proxy := newExpansionFactorAPI(h)
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

func TestExpansionFactorDoesNotCascadeWhileAgentPresent(t *testing.T) {
	t.Parallel()
	h := newRecoveryHarness(t)
	newExpansionFactorAPI(h)
	runner := h.runner(primaryTestKey)
	refreshExpansionFactor(t, h, runner)
	runner.snapshot["_expansion_agents"] = encode([]expansionAgent{{PortID: recoveryHomePort, Region: "north", HiredAt: time.Now().UTC()}})
	runner.stateCache.Factors = append(runner.stateCache.Factors, recoveryHomePort)
	input, err := runner.committedPlanningInput(h.ctx, ShipState{})
	if err != nil {
		t.Fatal(err)
	}
	yard, score := expansionAgentYard(runner.stateCache, input, decode[[]expansionAgent](runner.snapshot, "_expansion_agents"), time.Now())
	if yard.ID == "" || score <= 0 {
		t.Fatal("fixture has no second region worth exploring")
	}
	if changed, err := runner.considerExpansionAgent(h.ctx); err != nil || changed {
		t.Fatalf("active exploration hired another agent: changed=%t err=%v", changed, err)
	}
	h.missingMission("__treasury")
}

func TestExpansionFactorReconcilesUninterpretableHireFromFactors(t *testing.T) {
	t.Parallel()
	h := newRecoveryHarness(t)
	proxy := newExpansionFactorAPI(h)
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
	if mission.Phase != "done" || data.Action != nil || len(agents) != 1 || agents[0].PortID != "yard-a" {
		t.Fatalf("observed factor did not release control and persist agent: %+v %+v %+v", mission, data, agents)
	}
	if calls := h.api.mutationCount("/v1/company/hire-factor"); calls != 1 {
		t.Fatalf("reconciliation replayed hire: %d calls", calls)
	}
}

func TestExpansionFactorCanEstimateFromStaleRegionalQuote(t *testing.T) {
	t.Parallel()
	h := newRecoveryHarness(t)
	newExpansionFactorAPI(h)
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
			newExpansionFactorAPI(h)
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
