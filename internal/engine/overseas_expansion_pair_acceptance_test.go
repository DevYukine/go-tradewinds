package engine

// Required production surface:
//   expansionAgent.Partner string `json:"partner_port_id"`
//   expansionAgent.CreatedAt time.Time `json:"created_at"`
//   type expansionPair struct { yard, partner Port; score int64; competition CompetitionEvidence }
//   func expansionAgentYards(state AccountState, input PlanningInput) []expansionPair
//   func (r *Runner) expansionRetryUntil(region string) time.Time
//   func (r *Runner) expansionBuildEstimate(ctx context.Context, yard, partner Port, capacity CapacityGrowthInput, asOf time.Time) (GrowthDecision, YardQuote, bool, error)
// Existing considerExpansionAgent, manageExpansionAgents, recordExpansionAgent
// and newActionPreflight signatures remain unchanged.

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/DevYukine/go-tradewinds/internal/store"
)

func overseasPairRankingFixture() (AccountState, PlanningInput) {
	now := time.Date(2026, 7, 15, 12, 0, 0, 0, time.UTC)
	at := now.Add(-2 * 24 * time.Hour / 60)
	state := expansionFactorFixture()
	state.Clock.UnlockedPhase = 2
	state.Catalog.Ports = []Port{
		{ID: "havana", BankingRegion: "caribbean", SeaZoneID: "fixture_sea", HasShipyard: true, Phase: 2},
		{ID: "port_royal", BankingRegion: "caribbean", SeaZoneID: "fixture_sea", HasShipyard: true, Phase: 2},
		{ID: "cove", BankingRegion: "caribbean", SeaZoneID: "fixture_sea", IsCove: true, Phase: 2},
		{ID: "other-region", BankingRegion: "africa", SeaZoneID: "fixture_sea", Phase: 2},
	}
	xAsk, xBid, yAsk, yBid := int64(100), int64(180), int64(40), int64(60)
	state.Ports = map[string]PortDetail{recoveryHomePort: {CoffeeHouseGossip: []Gossip{
		{SubjectPortID: "havana", SnapshotAt: at, Commodities: []GossipCommodity{
			{CommodityID: "x", SnapshotAt: at, BestAskPence: &xAsk},
			{CommodityID: "y", SnapshotAt: at, BestBidPence: &yBid},
		}},
		{SubjectPortID: "port_royal", SnapshotAt: at, Commodities: []GossipCommodity{
			{CommodityID: "x", SnapshotAt: at, BestBidPence: &xBid},
			{CommodityID: "y", SnapshotAt: at, BestAskPence: &yAsk},
		}},
	}}}
	input := expansionFactorInput(state)
	input.Now, input.Clock.RealUTC = now, now
	for key, meta := range input.Metadata {
		meta.UpdatedAt, meta.AttemptedAt = now, now
		input.Metadata[key] = meta
	}
	return state, input
}

func TestOverseasExpansionHistoricalGossipRanksBothDirections(t *testing.T) {
	for _, mode := range []string{"two game days old", "thirty game days old", "older report", "older commodity", "port royal only yard", "phase one yard", "phase one yards", "locked yards", "yard lacks observed outbound", "negative outbound stays negative", "partner cove", "partner other region", "partner oceanic"} {
		t.Run(mode, func(t *testing.T) {
			state, input := overseasPairRankingFixture()
			wantYard, wantPartner, wantScore := "havana", "port_royal", int64(100)
			wantPairs := 2
			detail := state.Ports[recoveryHomePort]
			if mode == "thirty game days old" || mode == "older report" || mode == "older commodity" {
				for i := range detail.CoffeeHouseGossip {
					report := &detail.CoffeeHouseGossip[i]
					report.SnapshotAt = input.Now.Add(-30 * 24 * time.Hour / 60)
					for j := range report.Commodities {
						report.Commodities[j].SnapshotAt = report.SnapshotAt
						if mode == "older commodity" {
							report.Commodities[j].SnapshotAt = report.Commodities[j].SnapshotAt.Add(-time.Nanosecond)
						}
					}
					if mode == "older report" {
						report.SnapshotAt = report.SnapshotAt.Add(-time.Nanosecond)
					}
				}
			}
			switch mode {
			case "port royal only yard":
				state.Catalog.Ports[0].HasShipyard = false
				wantYard, wantPartner = "port_royal", "havana"
				wantPairs = 1
			case "phase one yard":
				state.Catalog.Ports[0].Phase = 1
				wantYard, wantPartner = "port_royal", "havana"
				wantPairs = 1
			case "phase one yards":
				state.Catalog.Ports[0].Phase = 1
				state.Catalog.Ports[1].Phase = 1
			case "locked yards":
				state.Catalog.Ports[0].Phase = 3
				state.Catalog.Ports[1].Phase = 3
			case "yard lacks observed outbound":
				detail.CoffeeHouseGossip[0].Commodities[0].BestAskPence = nil
				wantYard, wantPartner, wantScore, wantPairs = "port_royal", "havana", 20, 1
			case "negative outbound stays negative":
				ask := int64(190)
				detail.CoffeeHouseGossip[0].Commodities[0].BestAskPence = &ask
				wantYard, wantPartner, wantScore = "port_royal", "havana", 20
			case "partner cove":
				state.Catalog.Ports[1].IsCove = true
			case "partner other region":
				state.Catalog.Ports[1].BankingRegion = "africa"
			case "partner oceanic":
				state.Catalog.Ports[1].SeaZoneID = "ocean"
				state.Catalog.SeaZones = append(state.Catalog.SeaZones, Zone{ID: "ocean", ZoneClass: "oceanic"})
			}
			if mode == "older report" || mode == "older commodity" || mode == "phase one yards" || mode == "locked yards" || mode == "partner cove" || mode == "partner other region" || mode == "partner oceanic" {
				wantYard, wantPartner, wantScore = "", "", 0
				wantPairs = 0
			}
			state.Ports[recoveryHomePort] = detail
			input.Catalog, input.Ports = state.Catalog, state.Ports
			pairs := expansionAgentYards(state, input)
			if len(pairs) != wantPairs {
				t.Fatalf("ranked pair count = %d; want %d: %+v", len(pairs), wantPairs, pairs)
			}
			if len(pairs) == 0 {
				return
			}
			yard, partner, score := pairs[0].yard, pairs[0].partner, pairs[0].score
			if yard.ID != wantYard || partner.ID != wantPartner || score != wantScore {
				t.Fatalf("historical pair = %s/%s %dp; want %s/%s %dp", yard.ID, partner.ID, score, wantYard, wantPartner, wantScore)
			}
		})
	}
}

func TestOverseasExpansionRetryUsesLatestHireAndAwaitingCreatedAt(t *testing.T) {
	for _, tc := range []struct {
		name   string
		yard   time.Duration
		mate   time.Duration
		await  bool
		admits bool
	}{
		{name: "one nanosecond before twenty four hours", yard: 25 * time.Hour, mate: 24*time.Hour - time.Nanosecond},
		{name: "exactly twenty four hours", yard: 25 * time.Hour, mate: 24 * time.Hour, admits: true},
		{name: "later partner hire controls retry", yard: 25 * time.Hour, mate: 23 * time.Hour},
		{name: "awaiting hire counts as present", mate: time.Hour, await: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, input := overseasPairRankingFixture()
			agents := []expansionAgent{
				{PortID: "havana", Partner: "port_royal", Region: "caribbean", HiredAt: input.Now.Add(-tc.yard)},
				{PortID: "port_royal", Partner: "havana", Region: "caribbean", HiredAt: input.Now.Add(-tc.mate)},
			}
			if tc.await {
				for i := range agents {
					agents[i].HiredAt = time.Time{}
					agents[i].CreatedAt = input.Now.Add(-tc.mate)
				}
			}
			runner := Runner{snapshot: map[string]json.RawMessage{"_expansion_agents": encode(agents)}}
			retryAt := runner.expansionRetryUntil("caribbean")
			wantRetryAt := input.Now.Add(24*time.Hour - tc.mate)
			if !retryAt.Equal(wantRetryAt) || !input.Now.Before(retryAt) != tc.admits {
				t.Fatalf("regional retry boundary: until=%s want=%s admits=%t want_admits=%t", retryAt, wantRetryAt, !input.Now.Before(retryAt), tc.admits)
			}
		})
	}
}

func TestOverseasExpansionBuildEstimateRespectsYardRatingAndPairFees(t *testing.T) {
	h, runner, yard, _, _ := overseasCapacityFixture(t)
	partner, _ := findPort(runner.stateCache.Catalog, "new_york")
	asOf := time.Now().UTC()
	_, input := overseasCapacityEvidence(t, h, runner, asOf)
	input.MeasuredNetAvailable, input.MeasuredRatePencePerGameDay = false, 0
	control, quote, _, err := runner.expansionBuildEstimate(h.ctx, yard, partner, input, asOf)
	if err != nil || !control.Accepted || quote.ShipClass != "hoy" {
		t.Fatalf("coastal Boston did not admit a gossip-priced hoy without European earnings: %+v %+v %v", control, quote, err)
	}
	required := growthCapital(control) + control.WorkingCapitalPence + 2*expansionAgentHireFeePence
	for _, extra := range []int64{0, -1} {
		input.SurplusPence = required + extra
		decision, _, _, err := runner.expansionBuildEstimate(h.ctx, yard, partner, input, asOf)
		if err != nil || decision.Accepted != (extra == 0) {
			t.Fatalf("two 750 Cr hire fees boundary, delta=%dp: %+v %v", extra, decision, err)
		}
	}
	yard.SeaZoneID = "regional-yard-water"
	runner.stateCache.Catalog.SeaZones = append(runner.stateCache.Catalog.SeaZones, Zone{ID: yard.SeaZoneID, ZoneClass: "regional"})
	input.SurplusPence = 50_000_000
	if decision, _, _, err := runner.expansionBuildEstimate(h.ctx, yard, partner, input, asOf); err != nil || decision.Accepted {
		t.Fatalf("a coastal hoy admitted a regional-water yard: %+v %v", decision, err)
	}
}

func TestOverseasExpansionCashShortBrigantineNeedsValidMortgage(t *testing.T) {
	h, runner, yard, hull, observed := overseasCapacityFixture(t)
	hull.ID, hull.HullCategory = "brigantine", "regional"
	runner.stateCache.Catalog.ShipClasses = append(runner.stateCache.Catalog.ShipClasses, hull)
	yard.ID, yard.BankingRegion, yard.SeaZoneID = "havana", "caribbean", "regional-yard-water"
	partner := Port{
		ID: "port_royal", BankingRegion: yard.BankingRegion, SeaZoneID: yard.SeaZoneID, Phase: 2,
		Imports: []string{recoveryGrain, "provisions"},
	}
	runner.stateCache.Catalog.Ports = append(runner.stateCache.Catalog.Ports, yard, partner)
	runner.stateCache.Catalog.SeaZones = append(runner.stateCache.Catalog.SeaZones, Zone{ID: yard.SeaZoneID, ZoneClass: "regional", BaseStormBPS: 100})
	observed.PortID, observed.ShipClass, observed.Mortgage = recoveryHomePort, hull.ID, nil
	runner.stateCache.Quotes = map[string]YardQuote{observed.PortID + "|" + hull.ID: observed}
	runner.meta["yard_quotes/"+observed.PortID+"|"+hull.ID] = runner.meta["yard_quotes/"+recoveryHomePort+"|"+recoveryHull]
	asOf := time.Now().UTC()
	zones := decode[[]ZoneObservation](runner.snapshot, "zones")
	runner.snapshot["zones"] = encode(append(zones, ZoneObservation{SeaZoneID: yard.SeaZoneID}))
	for _, pair := range [][2]string{{yard.ID, recoveryHomePort}, {partner.ID, recoveryFarPort}} {
		detail := runner.stateCache.Ports[pair[1]]
		detail.ID = pair[0]
		runner.stateCache.Ports[pair[0]] = detail
		runner.meta["ports/"+pair[0]] = runner.meta["ports/"+pair[1]]
	}
	for _, pair := range [][2]string{{yard.ID, partner.ID}, {partner.ID, yard.ID}} {
		key := routeKey(pair[0], pair[1], hull.ID)
		runner.stateCache.Routes[key] = Route{
			FromPort: pair[0], ToPort: pair[1], ShipClass: hull.ID, TotalDistanceNM: 24, SafeProvisionsTons: 1,
			Legs: []RouteLeg{{FromNode: pair[0], ToNode: yard.SeaZoneID, EdgeClass: "regional", DistanceNM: 12},
				{FromNode: yard.SeaZoneID, ToNode: pair[1], EdgeClass: "regional", DistanceNM: 12}},
		}
		runner.meta["routes/"+key] = runner.meta["routes/"+routeKey(recoveryHomePort, recoveryFarPort, recoveryHull)]
	}
	ask, bid := int64(100), int64(5_000)
	detail := runner.stateCache.Ports[recoveryHomePort]
	detail.CoffeeHouseGossip = append(detail.CoffeeHouseGossip,
		Gossip{SubjectPortID: yard.ID, SnapshotAt: asOf, Commodities: []GossipCommodity{{CommodityID: recoveryGrain, SnapshotAt: asOf, BestAskPence: &ask}}},
		Gossip{SubjectPortID: partner.ID, SnapshotAt: asOf, Commodities: []GossipCommodity{{CommodityID: recoveryGrain, SnapshotAt: asOf, BestBidPence: &bid}}})
	runner.stateCache.Ports[recoveryHomePort] = detail
	runner.snapshot["loans"] = json.RawMessage("[]")
	runner.meta["loans"] = runner.meta["ships"]
	_, input := overseasCapacityEvidence(t, h, runner, asOf)
	cash, _, _, err := runner.expansionBuildEstimate(h.ctx, yard, partner, input, asOf)
	if err != nil || !cash.Accepted {
		t.Fatalf("cash brigantine control: %+v %v", cash, err)
	}
	input.SurplusPence = growthCapital(cash) + cash.WorkingCapitalPence + 2*expansionAgentHireFeePence - 20_000
	if decision, _, _, err := runner.expansionBuildEstimate(h.ctx, yard, partner, input, asOf); err != nil || decision.Accepted {
		t.Fatalf("cash-short brigantine ignored two hires and working cash: %+v %v", decision, err)
	}
	observed.Mortgage = mortgageTestTerms()
	runner.stateCache.Quotes[observed.PortID+"|"+hull.ID] = observed
	if decision, _, _, err := runner.expansionBuildEstimate(h.ctx, yard, partner, input, asOf); err != nil || decision.Accepted {
		t.Fatalf("fleet-wide hoy income financed a brigantine without per-class evidence: %+v %v", decision, err)
	}
	runner.stateCache.Ships[0].Ship.ShipClass = hull.ID
	for index := range 2 {
		persistGrowthWindowSettlement(t, h, store.Settlement{
			ID: fmt.Sprintf("expansion-brigantine-measured-%d", index), Kind: "spot", CostsKnown: true,
			QuantityTons: 48, GameSeconds: 43_200, AcquisitionPence: 4_800, ProceedsPence: 900_000,
			CompletedAt: asOf.Add(-time.Duration(index+1) * 30 * time.Minute),
		}, growthSettlementAction{Path: "/v1/market/place-order", ShipID: runner.stateCache.Ships[0].Ship.ID})
	}
	financed, _, _, err := runner.expansionBuildEstimate(h.ctx, yard, partner, input, asOf)
	if err != nil || !financed.Accepted || financed.Mortgage == nil {
		t.Fatalf("quoted mortgage did not admit the cash-short measured brigantine: %+v %v", financed, err)
	}
	runner.snapshot["_mortgage_retry_at"] = encode(asOf.Add(time.Hour))
	if decision, _, _, err := runner.expansionBuildEstimate(h.ctx, yard, partner, input, asOf); err != nil || decision.Accepted {
		t.Fatalf("mortgage cooldown financed the exploration: %+v %v", decision, err)
	}
	delete(runner.snapshot, "_mortgage_retry_at")
	observed.Mortgage.Available = false
	if decision, _, _, err := runner.expansionBuildEstimate(h.ctx, yard, partner, input, asOf); err != nil || decision.Accepted {
		t.Fatalf("unavailable mortgage financed the exploration: %+v %v", decision, err)
	}
}

type overseasPairAPI struct {
	h      *recoveryHarness
	runner *Runner
	base   http.Handler
	status int
}

func (api *overseasPairAPI) ServeHTTP(w http.ResponseWriter, req *http.Request) {
	if req.Method != http.MethodPost || req.URL.Path != "/v1/company/hire-factor" && req.URL.Path != "/v1/company/dismiss-factor" {
		api.base.ServeHTTP(w, req)
		return
	}
	body, err := io.ReadAll(req.Body)
	if err != nil {
		api.h.t.Error(err)
		w.WriteHeader(http.StatusBadRequest)
		return
	}
	var terms struct {
		PortID string `json:"port_id"`
	}
	if json.Unmarshal(body, &terms) != nil || req.Header.Get("Idempotency-Key") == "" {
		api.h.t.Errorf("factor request lost immutable identity: %s", body)
		w.WriteHeader(http.StatusBadRequest)
		return
	}
	port, exists := findPort(api.runner.stateCache.Catalog, terms.PortID)
	if !exists || terms.PortID != "boston" && terms.PortID != "new_york" {
		api.h.t.Errorf("unexpected paired port: %s", terms.PortID)
		w.WriteHeader(http.StatusBadRequest)
		return
	}
	api.h.api.mu.Lock()
	defer api.h.api.mu.Unlock()
	if api.status != 0 {
		api.h.api.record(req, body, api.status)
		api.h.api.writeProblem(w, api.status, "no_port_presence", "yard hire refused")
		return
	}
	hire := req.URL.Path == "/v1/company/hire-factor"
	if hire && !slices.Contains(api.h.api.factorPorts, terms.PortID) {
		api.h.api.factorPorts = append(api.h.api.factorPorts, terms.PortID)
		api.h.api.vaultBalance[port.BankingRegion] -= expansionAgentHireFeePence
	}
	if !hire {
		api.h.api.factorPorts = slices.DeleteFunc(api.h.api.factorPorts, func(id string) bool { return id == terms.PortID })
	}
	api.h.api.record(req, body, http.StatusOK)
	api.h.api.writeJSON(w, http.StatusOK, map[string]any{"port_id": terms.PortID, "has_factor": hire})
}

func overseasPairFixture(t *testing.T) (*recoveryHarness, *Runner, *overseasPairAPI) {
	t.Helper()
	h, runner, _, _, _ := overseasCapacityFixture(t)
	overseasMetric(t, h, 10_000_000, time.Now().UTC())
	api := &overseasPairAPI{h: h, runner: runner, base: h.api.server.Config.Handler}
	h.api.server.Config.Handler = api
	return h, runner, api
}

func overseasPairSyncFactors(h *recoveryHarness, runner *Runner) {
	h.api.mu.Lock()
	defer h.api.mu.Unlock()
	runner.stateCache.Factors = append([]string(nil), h.api.factorPorts...)
	for i := range runner.stateCache.Vaults {
		vault := &runner.stateCache.Vaults[i]
		vault.BalancePence = h.api.vaultBalance[vault.BankingRegion]
	}
}

func overseasPairSave(t *testing.T, h *recoveryHarness, runner *Runner, agents []expansionAgent) {
	t.Helper()
	runner.snapshot["_expansion_agents"] = encode(agents)
	if err := h.db.SaveSnapshot(h.ctx, h.account.ID, runner.snapshot); err != nil {
		t.Fatal(err)
	}
}

func TestOverseasExpansionPairAdmissionGuards(t *testing.T) {
	for _, guard := range []string{"nine calibration quotes", "stale net worth", "eleven factors"} {
		t.Run(guard, func(t *testing.T) {
			h, runner, _ := overseasPairFixture(t)
			switch guard {
			case "nine calibration quotes":
				delete(runner.stateCache.HazardQuotes, "overseas-sample-09")
				// Other fixture quotes are hull-only, not calibration observations.
			case "stale net worth":
				overseasMetric(t, h, 10_000_000, time.Now().UTC().Add(-6*time.Hour-time.Second))
			case "eleven factors":
				for len(runner.stateCache.Factors) < 11 {
					runner.stateCache.Factors = append(runner.stateCache.Factors, fmt.Sprintf("occupied-%d", len(runner.stateCache.Factors)))
				}
			}
			if changed, err := runner.considerExpansionAgent(h.ctx); err != nil || changed {
				t.Fatalf("%s admitted a paid pair: %t %v", guard, changed, err)
			}
			h.missingMission("__treasury")
			if agents := decode[[]expansionAgent](runner.snapshot, "_expansion_agents"); len(agents) != 0 {
				t.Fatalf("rejected exploration left awaiting hires: %+v", agents)
			}
		})
	}
}

func TestOverseasExpansionPairHireOrderRestartAndPartnerPreflight(t *testing.T) {
	h, runner, _ := overseasPairFixture(t)
	if changed, err := runner.considerExpansionAgent(h.ctx); err != nil || !changed {
		t.Fatalf("yard hire admission: %t %v holds=%v", changed, err, runner.holding)
	}
	mission, data := h.mission("__treasury")
	if mission.Destination != "boston" || data.Action == nil || data.Action.Stage != "hire-factor" {
		t.Fatalf("yard was not first: %+v %+v", mission, data.Action)
	}
	snapshot, err := h.db.Snapshot(h.ctx, h.account.ID)
	if err != nil {
		t.Fatal(err)
	}
	agents := decode[[]expansionAgent](snapshot, "_expansion_agents")
	if len(agents) != 2 {
		t.Fatalf("pair was not durable before the yard hire: %+v", agents)
	}
	for _, agent := range agents {
		if agent.Partner == "" || agent.CreatedAt.IsZero() || !agent.HiredAt.IsZero() {
			t.Fatalf("awaiting pair lost its partner, creation time or hire state: %+v", agent)
		}
	}
	var rows []map[string]json.RawMessage
	if err := json.Unmarshal(snapshot["_expansion_agents"], &rows); err != nil || len(rows[0]["partner_port_id"]) == 0 {
		t.Fatalf("partner_port_id was not persisted: %s %v", snapshot["_expansion_agents"], err)
	}
	yardConfirmedBefore := time.Now().UTC()
	if changed, err := runner.dispatch(h.ctx, mission, &data); err != nil || !changed {
		t.Fatalf("yard hire dispatch: %t %v", changed, err)
	}
	yardConfirmedAfter := time.Now().UTC()
	if err := h.db.DeleteMission(h.ctx, h.account.ID, "__treasury"); err != nil {
		t.Fatal(err)
	}
	overseasPairSyncFactors(h, runner)
	restarted := h.runner(primaryTestKey)
	restarted.stateCache, restarted.meta, restarted.generation = runner.stateCache, runner.meta, runner.generation
	if changed, err := restarted.manageExpansionAgents(h.ctx); err != nil || !changed {
		t.Fatalf("restart did not resume the partner hire: %t %v", changed, err)
	}
	mission, data = h.mission("__treasury")
	if mission.Destination != "new_york" || data.Action == nil || data.Action.Stage != "hire-factor" {
		t.Fatalf("restart repeated the yard instead of its non-yard partner: %+v %+v", mission, data.Action)
	}
	restarted.snapshot["_capacity_savings_target"] = encode(capacitySavingsTarget{
		PortID: "boston", HullID: recoveryHull, RequiredPence: 1_000_000, CapitalPence: 126_000,
		DepthNetPencePerGameDay: 1_000, ExpiresAt: time.Now().UTC().Add(time.Hour),
	})
	if err := restarted.newActionPreflight(h.ctx, mission, &data); err != nil {
		t.Fatalf("recorded non-yard partner failed preflight with a live savings target: %v", err)
	}
	partnerConfirmedBefore := time.Now().UTC()
	if changed, err := restarted.dispatch(h.ctx, mission, &data); err != nil || !changed {
		t.Fatalf("partner hire dispatch: %t %v", changed, err)
	}
	partnerConfirmedAfter := time.Now().UTC()
	agents = decode[[]expansionAgent](restarted.snapshot, "_expansion_agents")
	for _, agent := range agents {
		before, after := yardConfirmedBefore, yardConfirmedAfter
		if agent.PortID == "new_york" {
			before, after = partnerConfirmedBefore, partnerConfirmedAfter
		}
		if agent.HiredAt.Before(before) || agent.HiredAt.After(after) {
			t.Fatalf("hire time is not its confirmed result time: %+v confirmation=[%s,%s]", agent, before, after)
		}
	}
	calls := h.api.mutationsFor("/v1/company/hire-factor")
	if len(calls) != 2 || !stringsContainPort(calls[0].Body, "boston") || !stringsContainPort(calls[1].Body, "new_york") {
		t.Fatalf("pair hire order or count changed: %+v", calls)
	}
}

func stringsContainPort(body []byte, port string) bool {
	var terms struct {
		PortID string `json:"port_id"`
	}
	return json.Unmarshal(body, &terms) == nil && terms.PortID == port
}

func TestOverseasExpansionPairFundsBothHireFeesTogether(t *testing.T) {
	h, runner, _ := overseasPairFixture(t)
	for i := range runner.stateCache.Vaults {
		if runner.stateCache.Vaults[i].BankingRegion == "america" {
			runner.stateCache.Vaults[i].BalancePence = 0
		}
	}
	h.api.setVaultBalance("america", 0)
	input, err := runner.committedPlanningInput(h.ctx, ShipState{})
	if err != nil {
		t.Fatal(err)
	}
	if changed, err := runner.considerExpansionAgent(h.ctx); err != nil || !changed {
		t.Fatalf("fund both hire fees: %t %v", changed, err)
	}
	mission, data := h.mission("__treasury")
	var remit struct {
		To     string `json:"to_region"`
		Amount int64  `json:"gross_amount_pence"`
	}
	if data.Action == nil || data.Action.Stage != "remit" || json.Unmarshal(data.Action.Body, &remit) != nil || remit.To != "america" ||
		remit.Amount < max(input.ReserveByRegion["america"], runner.protectedReservePence())+2*expansionAgentHireFeePence {
		t.Fatalf("pair hire fees were split or omitted: %+v %+v", mission, remit)
	}
	if h.api.mutationCount("/v1/company/hire-factor") != 0 {
		t.Fatal("hire preceded the shared regional fee funding")
	}
}

func TestOverseasExpansionPairFloorUsesLaterHireThenDismissesBoth(t *testing.T) {
	h, runner, _ := overseasPairFixture(t)
	h.api.setFactorPorts(recoveryHomePort, recoveryFarPort, "boston", "new_york")
	overseasPairSyncFactors(h, runner)
	now := time.Now().UTC()
	agents := []expansionAgent{
		{PortID: "boston", Partner: "new_york", Region: "america", HiredAt: now.Add(-3 * time.Hour)},
		{PortID: "new_york", Partner: "boston", Region: "america", HiredAt: now.Add(-time.Hour)},
	}
	overseasPairSave(t, h, runner, agents)
	if changed, err := runner.manageExpansionAgents(h.ctx); err != nil || changed {
		t.Fatalf("yard floor expired before the later partner's two hours: %t %v", changed, err)
	}
	// Equality is expiry: the later hire is exactly two real hours old.
	agents[1].HiredAt = time.Now().UTC().Add(-2 * time.Hour)
	overseasPairSave(t, h, runner, agents)
	for range 2 {
		if changed, err := runner.manageExpansionAgents(h.ctx); err != nil || !changed {
			t.Fatalf("expired unreferenced pair was not dismissed: %t %v", changed, err)
		}
		mission, data := h.mission("__treasury")
		if data.Action == nil || data.Action.Stage != "dismiss-factor" {
			t.Fatalf("pair staged a non-dismiss action after the floor: %+v", data.Action)
		}
		if changed, err := runner.dispatch(h.ctx, mission, &data); err != nil || !changed {
			t.Fatalf("dismiss pair member: %t %v", changed, err)
		}
		if err := h.db.DeleteMission(h.ctx, h.account.ID, "__treasury"); err != nil {
			t.Fatal(err)
		}
		overseasPairSyncFactors(h, runner)
	}
	calls := h.api.mutationsFor("/v1/company/dismiss-factor")
	if len(calls) != 2 || stringsContainPort(calls[0].Body, "boston") == stringsContainPort(calls[1].Body, "boston") {
		t.Fatalf("did not dismiss both distinct members: %+v", calls)
	}
	if changed, err := runner.manageExpansionAgents(h.ctx); err != nil || changed {
		t.Fatalf("pair dismissal repeated after both factors were absent: %t %v", changed, err)
	}
}

func TestOverseasExpansionPairRetainsBothForEitherPortReference(t *testing.T) {
	for _, port := range []string{"boston", "new_york"} {
		for _, reference := range []string{"growth port", "savings target", "mission origin", "mission destination", "mission build", "owned ship last plan", "owned ship next plan"} {
			t.Run(port+"/"+reference, func(t *testing.T) {
				h, runner, _ := overseasPairFixture(t)
				h.api.setFactorPorts(recoveryHomePort, recoveryFarPort, "boston", "new_york")
				overseasPairSyncFactors(h, runner)
				at := time.Now().UTC().Add(-3 * time.Hour)
				overseasPairSave(t, h, runner, []expansionAgent{
					{PortID: "boston", Partner: "new_york", Region: "america", HiredAt: at},
					{PortID: "new_york", Partner: "boston", Region: "america", HiredAt: at},
				})
				intent := missionData{Plan: Plan{Kind: "spot"}}
				switch reference {
				case "growth port":
					runner.snapshot["_capacity_growth_port"] = encode(port)
				case "savings target":
					runner.snapshot["_capacity_savings_target"] = encode(capacitySavingsTarget{PortID: port, HullID: recoveryHull,
						RequiredPence: 1_000_000, CapitalPence: 126_000, DepthNetPencePerGameDay: 1_000, ExpiresAt: time.Now().UTC().Add(time.Hour)})
				case "mission origin":
					intent.Plan.Origin = port
				case "mission destination":
					intent.Plan.Destination = port
				case "mission build":
					intent.Capacity = &capacityIntent{BuildPort: port}
				case "owned ship last plan", "owned ship next plan":
					ship := &runner.stateCache.Ships[0]
					ship.Ship.CurrentPortID, ship.Ship.Status, ship.Voyage = "boston", "berthed", nil
					intent.Plan = Plan{Origin: port, Destination: "boston"}
					if reference == "owned ship next plan" {
						intent.Plan = Plan{Origin: recoveryHomePort, Destination: recoveryFarPort}
						intent.GrowthLane = &growthLaneIntent{NextPlan: &Plan{Origin: port, Destination: "boston"}}
					}
				}
				if reference != "growth port" && reference != "savings target" {
					h.putMission(recoveryShipBlastoise, "paired-reference", "buy", intent)
				}
				if changed, err := runner.manageExpansionAgents(h.ctx); err != nil || changed {
					t.Fatalf("%s reference at %s did not retain its mate: %t %v", reference, port, changed, err)
				}
				h.missingMission("__treasury")
			})
		}
	}
}

func TestOverseasExpansionRefusedYardFreesAwaitingRegion(t *testing.T) {
	h, runner, api := overseasPairFixture(t)
	api.status = http.StatusForbidden
	if changed, err := runner.considerExpansionAgent(h.ctx); err != nil || !changed {
		t.Fatalf("stage refused yard: %t %v", changed, err)
	}
	mission, data := h.mission("__treasury")
	if changed, err := runner.dispatch(h.ctx, mission, &data); err != nil || !changed {
		t.Fatalf("consume yard refusal: %t %v", changed, err)
	}
	snapshot, err := h.db.Snapshot(h.ctx, h.account.ID)
	if err != nil {
		t.Fatal(err)
	}
	if agents := decode[[]expansionAgent](snapshot, "_expansion_agents"); len(agents) != 0 {
		t.Fatalf("refused hire permanently occupied the region: %+v", agents)
	}
	if slices.Contains(runner.stateCache.Factors, "boston") || h.api.mutationCount("/v1/company/hire-factor") != 1 {
		t.Fatal("refused yard became a factor or was retried during receipt handling")
	}
}

func TestOverseasExpansionReexplorationRefreshesConfirmationTimeOncePerPort(t *testing.T) {
	h, runner, _ := overseasPairFixture(t)
	old := time.Now().UTC().Add(-25 * time.Hour)
	overseasPairSave(t, h, runner, []expansionAgent{
		{PortID: "boston", Partner: "new_york", Region: "america", HiredAt: old, CreatedAt: old},
		{PortID: "new_york", Partner: "boston", Region: "america", HiredAt: old, CreatedAt: old},
	})
	if changed, err := runner.considerExpansionAgent(h.ctx); err != nil || !changed {
		t.Fatalf("24-hour retry was permanently latched: %t %v", changed, err)
	}
	_, data := h.mission("__treasury")
	before := time.Now().UTC()
	if err := runner.recordExpansionAgent(h.ctx, &data); err != nil {
		t.Fatal(err)
	}
	after := time.Now().UTC()
	var confirmedAt time.Time
	for _, agent := range decode[[]expansionAgent](runner.snapshot, "_expansion_agents") {
		if agent.PortID == "boston" {
			confirmedAt = agent.HiredAt
		}
	}
	if confirmedAt.Before(before) || confirmedAt.After(after) {
		t.Fatalf("re-exploration reused a prior hire or staged action time: %s confirmation=[%s,%s]", confirmedAt, before, after)
	}
	data.Action.CreatedAt = after.Add(time.Hour)
	if err := runner.recordExpansionAgent(h.ctx, &data); err != nil {
		t.Fatal(err)
	}
	agents := decode[[]expansionAgent](runner.snapshot, "_expansion_agents")
	if len(agents) != 2 {
		t.Fatalf("region deduplication erased a member or appended stale records: %+v", agents)
	}
	for _, agent := range agents {
		if agent.PortID == "boston" && !agent.HiredAt.Equal(confirmedAt) || agent.PortID == "new_york" && !agent.HiredAt.IsZero() {
			t.Fatalf("confirmed hire time was not recorded exactly once by PortID: %+v", agents)
		}
	}
}

func TestOverseasExpansionLegacySoloDismissesAfterFloor(t *testing.T) {
	h, runner, _ := overseasPairFixture(t)
	h.api.setFactorPorts(recoveryHomePort, recoveryFarPort, "boston")
	overseasPairSyncFactors(h, runner)
	at := time.Now().UTC().Add(-2*time.Hour - time.Second)
	// Decode the old persisted shape rather than constructing new pair fields.
	runner.snapshot["_expansion_agents"] = encode([]map[string]any{{"port_id": "boston", "region": "america", "hired_at": at}})
	if changed, err := runner.manageExpansionAgents(h.ctx); err != nil || !changed {
		t.Fatalf("legacy solo was retained forever: %t %v", changed, err)
	}
	_, data := h.mission("__treasury")
	if data.Action == nil || data.Action.Stage != "dismiss-factor" || !stringsContainPort(data.Action.Body, "boston") {
		t.Fatalf("legacy solo did not stage its own dismissal: %+v", data.Action)
	}
}

func TestOverseasExpansionPartnerHireRestoresRegionalReserveFirst(t *testing.T) {
	h, runner, _ := overseasPairFixture(t)
	h.api.setFactorPorts(recoveryHomePort, recoveryFarPort, "boston")
	h.api.setVaultBalance("america", 0)
	overseasPairSyncFactors(h, runner)
	at := time.Now().UTC().Add(-time.Hour)
	overseasPairSave(t, h, runner, []expansionAgent{
		{PortID: "boston", Partner: "new_york", Region: "america", CreatedAt: at, HiredAt: at},
		{PortID: "new_york", Partner: "boston", Region: "america", CreatedAt: at},
	})
	input, err := runner.committedPlanningInput(h.ctx, ShipState{})
	if err != nil {
		t.Fatal(err)
	}
	if input.ReserveByRegion["america"] < expansionAgentHireFeePence {
		t.Fatalf("pending partner fee is not earmarked in regional reserves: %+v", input.ReserveByRegion)
	}
	normalReserve := max(input.ReserveByRegion["america"]-expansionAgentHireFeePence, runner.protectedReservePence())
	required := normalReserve + expansionAgentHireFeePence
	if changed, err := runner.manageExpansionAgents(h.ctx); err != nil || !changed {
		t.Fatalf("partner funding was not staged inside its mate's floor: %t %v", changed, err)
	}
	mission, data := h.mission("__treasury")
	var remit struct {
		From  string `json:"from_region"`
		To    string `json:"to_region"`
		Gross int64  `json:"gross_amount_pence"`
	}
	if data.Action == nil || data.Action.Stage != "remit" || json.Unmarshal(data.Action.Body, &remit) != nil ||
		remit.To != "america" {
		t.Fatalf("partner hire did not fund its region first: %+v %+v", data.Action, remit)
	}
	feeBPS := int64(-1)
	for _, rate := range input.Rates {
		if rate.FromRegion == remit.From && rate.ToRegion == remit.To {
			feeBPS = rate.FeeBPS
		}
	}
	if feeBPS < 0 {
		t.Fatalf("partner funding has no observed remittance fee: %+v", remit)
	}
	wantGross, err := GrossRemittance(required, feeBPS)
	if err != nil || remit.Gross != wantGross {
		t.Fatalf("partner funding must preserve normal reserves plus exactly one remaining fee: got=%d want=%d required=%d err=%v", remit.Gross, wantGross, required, err)
	}
	if changed, err := runner.dispatch(h.ctx, mission, &data); err != nil || !changed {
		t.Fatalf("dispatch partner funding: %t %v", changed, err)
	}
	if err := h.db.DeleteMission(h.ctx, h.account.ID, "__treasury"); err != nil {
		t.Fatal(err)
	}
	overseasPairSyncFactors(h, runner)
	balances := runner.planningInput(ShipState{}).Balances
	if balances["america"] < required {
		t.Fatalf("remittance did not deliver the normal regional reserve plus partner fee: balance=%d required=%d", balances["america"], required)
	}
	for region, protected := range input.ReserveByRegion {
		if region != "america" && balances[region] < protected {
			t.Fatalf("partner funding consumed protected %s reserves: balance=%d reserve=%d", region, balances[region], protected)
		}
	}
	if changed, err := runner.manageExpansionAgents(h.ctx); err != nil || !changed {
		t.Fatalf("funded partner hire was not resumed: %t %v", changed, err)
	}
	_, data = h.mission("__treasury")
	if data.Action == nil || data.Action.Stage != "hire-factor" || !stringsContainPort(data.Action.Body, "new_york") {
		t.Fatalf("partner funding did not release the recorded partner hire: %+v", data.Action)
	}
	if h.api.mutationCount("/v1/company/hire-factor") != 0 {
		t.Fatal("partner was hired without its staged action being dispatched")
	}
}

func TestOverseasExpansionPreflightRejectedYardFreesAwaitingRegion(t *testing.T) {
	h, runner, _ := overseasPairFixture(t)
	if changed, err := runner.considerExpansionAgent(h.ctx); err != nil || !changed {
		t.Fatalf("stage yard before authority change: %t %v", changed, err)
	}
	for i := range runner.stateCache.Catalog.Ports {
		if runner.stateCache.Catalog.Ports[i].ID == "boston" {
			runner.stateCache.Catalog.Ports[i].HasShipyard = false
		}
	}
	mission, data := h.mission("__treasury")
	if changed, err := runner.dispatch(h.ctx, mission, &data); err != nil || !changed {
		t.Fatalf("consume yard preflight rejection: %t %v", changed, err)
	}
	snapshot, err := h.db.Snapshot(h.ctx, h.account.ID)
	if err != nil {
		t.Fatal(err)
	}
	if agents := decode[[]expansionAgent](snapshot, "_expansion_agents"); len(agents) != 0 {
		t.Fatalf("preflight-failed hire occupied the region: %+v", agents)
	}
	if h.api.mutationCount("/v1/company/hire-factor") != 0 {
		t.Fatal("rejected yard preflight reached the factor API")
	}
}

func TestOverseasExpansionDelayedHireSuccessStartsFloorAtConfirmation(t *testing.T) {
	h, runner, _ := overseasPairFixture(t)
	h.api.setFactorPorts(recoveryHomePort, recoveryFarPort, "boston", "new_york")
	overseasPairSyncFactors(h, runner)
	stagedAt := time.Now().UTC().Add(-3 * time.Hour)
	overseasPairSave(t, h, runner, []expansionAgent{
		{PortID: "boston", Partner: "new_york", Region: "america", CreatedAt: stagedAt},
		{PortID: "new_york", Partner: "boston", Region: "america", CreatedAt: stagedAt, HiredAt: stagedAt.Add(-time.Hour)},
	})
	data := missionData{Action: &durableAction{
		ID: "overseas-delayed-hire", Stage: "hire-factor", Method: http.MethodPost, Path: "/v1/company/hire-factor",
		Body: encode(map[string]string{"port_id": "boston"}), CreatedAt: stagedAt,
	}}
	before := time.Now().UTC()
	if err := runner.recordExpansionAgent(h.ctx, &data); err != nil {
		t.Fatal(err)
	}
	after := time.Now().UTC()
	var confirmedAt time.Time
	for _, agent := range decode[[]expansionAgent](runner.snapshot, "_expansion_agents") {
		if agent.PortID == "boston" {
			confirmedAt = agent.HiredAt
		}
	}
	if confirmedAt.Before(before) || confirmedAt.After(after) || confirmedAt.Sub(stagedAt) < 3*time.Hour {
		t.Fatalf("delayed success counted the staged action as the hire: staged=%s confirmed=%s confirmation=[%s,%s]", stagedAt, confirmedAt, before, after)
	}
	if err := runner.recordExpansionAgent(h.ctx, &data); err != nil {
		t.Fatal(err)
	}
	for _, agent := range decode[[]expansionAgent](runner.snapshot, "_expansion_agents") {
		if agent.PortID == "boston" && !agent.HiredAt.Equal(confirmedAt) {
			t.Fatalf("replayed success moved the confirmed hire time: first=%s replay=%s", confirmedAt, agent.HiredAt)
		}
	}
	if changed, err := runner.manageExpansionAgents(h.ctx); err != nil || changed {
		t.Fatalf("three-hour delayed receipt exhausted the pair's two-hour floor: changed=%t err=%v", changed, err)
	}
	h.missingMission("__treasury")
	if h.api.mutationCount("/v1/company/dismiss-factor") != 0 {
		t.Fatal("factor was dismissed before two hours of confirmed presence")
	}
}

func TestOverseasExpansionRetryCombinesAttemptMinimumAndLossBackoff(t *testing.T) {
	now := time.Date(2026, 7, 15, 12, 0, 0, 0, time.UTC)
	runner := Runner{snapshot: map[string]json.RawMessage{
		"_expansion_agent_retries": encode(map[string]time.Time{
			"caribbean": now.Add(-2 * time.Hour), "america": now.Add(24 * time.Hour),
		}),
		"_expansion_agents": encode([]expansionAgent{
			{PortID: "port_royal", Region: "caribbean", HiredAt: now.Add(-time.Hour)},
		}),
		"_expansion_agent_outcomes": encode(map[string]expansionOutcome{
			"caribbean": {RetryAt: now.Add(48 * time.Hour), Losses: 2},
			"america":   {RetryAt: now.Add(96 * time.Hour), Losses: 3},
		}),
	}}
	if got := runner.expansionRetryUntil("caribbean"); !got.Equal(now.Add(48 * time.Hour)) {
		t.Fatalf("loss backoff was not the latest regional deadline: %s", got)
	}
	delete(runner.snapshot, "_expansion_agent_outcomes")
	if got := runner.expansionRetryUntil("caribbean"); !got.Equal(now.Add(23 * time.Hour)) {
		t.Fatalf("later confirmed hire did not protect its 24-hour retry minimum: %s", got)
	}
	delete(runner.snapshot, "_expansion_agents")
	if got := runner.expansionRetryUntil("caribbean"); !got.Equal(now.Add(22 * time.Hour)) {
		t.Fatalf("persisted attempt lost its 24-hour minimum after records were removed: %s", got)
	}
	if got := runner.expansionRetryUntil("africa"); !got.IsZero() {
		t.Fatalf("another region inherited retry history: %s", got)
	}
}

func TestOverseasExpansionRegionalRealizedEarningsOverrideGossipAndFleetRate(t *testing.T) {
	for _, evidence := range []string{"known", "unknown costs", "unknown proceeds"} {
		t.Run(evidence, func(t *testing.T) {
			h, runner, yard, _, _ := overseasCapacityFixture(t)
			partner, _ := findPort(runner.stateCache.Catalog, "new_york")
			asOf := time.Now().UTC()
			_, input := overseasCapacityEvidence(t, h, runner, asOf)
			input.MeasuredNetAvailable, input.MeasuredRatePencePerGameDay = false, 0
			control, _, _, err := runner.expansionBuildEstimate(h.ctx, yard, partner, input, asOf)
			if err != nil || !control.Accepted {
				t.Fatalf("positive gossip without regional receipts did not admit cash exploration: %+v %v", control, err)
			}
			ship := &runner.stateCache.Ships[0]
			ship.Ship.CurrentPortID, ship.Ship.Status, ship.Voyage, ship.RouteLegs = yard.ID, "berthed", nil, nil
			row := store.Settlement{
				ID: "expansion-regional-realized", AccountID: h.account.ID, ShipID: ship.Ship.ID,
				MissionID: "expansion-regional-realized-mission", ActionID: "expansion-regional-realized-action",
				Kind: "spot", Origin: yard.ID, Destination: partner.ID, Commodity: recoveryGrain,
				Lane:         laneKey(Plan{Kind: "spot", Origin: yard.ID, Destination: partner.ID, Commodity: recoveryGrain}),
				QuantityTons: 1, AcquisitionPence: 100, ProceedsPence: 101, GameSeconds: 86_400, CostsKnown: true,
				StartedAt: asOf.Add(-time.Minute - 24*time.Hour/60), CompletedAt: asOf.Add(-time.Minute),
			}
			switch evidence {
			case "unknown costs":
				row.CostsKnown = false
			case "unknown proceeds":
				row.Kind, row.ActionID = "passage", ""
				row.CostsKnown, row.ProceedsUnknown, row.ProceedsPence = false, true, 0
			}
			if row.ProceedsUnknown {
				receipt := store.PassengerReceipt{Settlement: row, CostsKnown: true, TimeScale: 60, Context: json.RawMessage(`{}`)}
				if err := h.db.PutPendingPassengerReceipt(h.ctx, &receipt); err != nil {
					t.Fatal(err)
				}
			} else {
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
			input.SurplusPence = 50_000_000
			input.MeasuredNetAvailable, input.MeasuredRatePencePerGameDay = true, 1_000_000_000
			decision, _, _, err := runner.expansionBuildEstimate(h.ctx, yard, partner, input, asOf)
			if err != nil {
				t.Fatal(err)
			}
			if evidence == "known" {
				if decision.Accepted || decision.EconomicallyViable || decision.CashShortfallPence != 0 {
					t.Fatalf("gossip or European fleet rate overrode one penny across regional hull-days: %+v", decision)
				}
			} else if !decision.Accepted {
				t.Fatalf("%s was priced as realized regional earnings instead of falling back to gossip: %+v", evidence, decision)
			}
		})
	}
}

func TestOverseasExpansionAwaitingPairInsideFloorRecordsWaitDecision(t *testing.T) {
	h, runner, _ := overseasPairFixture(t)
	createdAt := time.Now().UTC().Add(-time.Hour)
	overseasPairSave(t, h, runner, []expansionAgent{
		{PortID: "boston", Partner: "new_york", Region: "america", CreatedAt: createdAt},
		{PortID: "new_york", Partner: "boston", Region: "america", CreatedAt: createdAt},
	})
	if changed, err := runner.considerExpansionAgent(h.ctx); err != nil || changed {
		t.Fatalf("in-window awaiting pair did not block another exploration: changed=%t err=%v", changed, err)
	}
	h.missingMission("__treasury")
	history, err := h.db.History(h.ctx, h.account.ID, "decisions", 100)
	if err != nil {
		t.Fatal(err)
	}
	waitRecorded := false
	for _, row := range history.([]store.Decision) {
		if strings.HasPrefix(row.Kind, "expansion") && strings.Contains(row.Reason, "waits for the existing exploration") {
			waitRecorded = true
		}
	}
	if !waitRecorded {
		t.Fatal("in-window awaiting pair did not record its existing-exploration wait decision")
	}
	if agents := decode[[]expansionAgent](runner.snapshot, "_expansion_agents"); len(agents) != 2 {
		t.Fatalf("in-window awaiting records were expired early: %+v", agents)
	}
	if !decode[time.Time](runner.snapshot, "_expansion_last_started_at").IsZero() || h.api.mutationCount("/v1/company/hire-factor") != 0 {
		t.Fatal("fee-free awaiting records consumed a confirmed paid start")
	}
}

func TestOverseasExpansionAbandonedAwaitingRecordsExpireThroughAdmission(t *testing.T) {
	h, runner, _ := overseasPairFixture(t)
	now := time.Now().UTC()
	createdAt := now.Add(-2*time.Hour - time.Second)
	overseasPairSave(t, h, runner, []expansionAgent{
		{PortID: "boston", Partner: "new_york", Region: "america", CreatedAt: createdAt},
		{PortID: "new_york", Partner: "boston", Region: "america", CreatedAt: createdAt},
	})
	yard := Port{
		ID: "luanda", BankingRegion: "africa", SeaZoneID: "america-water", Phase: 2, HasShipyard: true,
		Produces: []string{recoveryGrain}, Imports: []string{"provisions"},
	}
	partner := Port{
		ID: "benguela", BankingRegion: "africa", SeaZoneID: yard.SeaZoneID, Phase: 2,
		Imports: []string{recoveryGrain, "provisions"},
	}
	runner.stateCache.Catalog.Ports = append(runner.stateCache.Catalog.Ports, yard, partner)
	for _, pair := range [][2]string{{yard.ID, recoveryHomePort}, {partner.ID, recoveryFarPort}} {
		detail := runner.stateCache.Ports[pair[1]]
		detail.ID = pair[0]
		runner.stateCache.Ports[pair[0]] = detail
		runner.meta["ports/"+pair[0]] = runner.meta["ports/"+pair[1]]
	}
	for _, pair := range [][2]string{{yard.ID, partner.ID}, {partner.ID, yard.ID}} {
		key := routeKey(pair[0], pair[1], recoveryHull)
		sourceKey := routeKey("boston", "new_york", recoveryHull)
		route := runner.stateCache.Routes[sourceKey]
		route.FromPort, route.ToPort = pair[0], pair[1]
		route.Legs = []RouteLeg{
			{FromNode: pair[0], ToNode: yard.SeaZoneID, EdgeClass: "coastal", DistanceNM: 12},
			{FromNode: yard.SeaZoneID, ToNode: pair[1], EdgeClass: "coastal", DistanceNM: 12},
		}
		runner.stateCache.Routes[key] = route
		runner.meta["routes/"+key] = runner.meta["routes/"+sourceKey]
	}
	ask, bid := int64(100), int64(4_500)
	detail := runner.stateCache.Ports[recoveryHomePort]
	detail.CoffeeHouseGossip = append(detail.CoffeeHouseGossip,
		Gossip{SubjectPortID: yard.ID, SnapshotAt: now, Commodities: []GossipCommodity{{CommodityID: recoveryGrain, SnapshotAt: now, BestAskPence: &ask}}},
		Gossip{SubjectPortID: partner.ID, SnapshotAt: now, Commodities: []GossipCommodity{{CommodityID: recoveryGrain, SnapshotAt: now, BestBidPence: &bid}}})
	runner.stateCache.Ports[recoveryHomePort] = detail
	runner.stateCache.Vaults = append(runner.stateCache.Vaults, Vault{BankingRegion: yard.BankingRegion, AccountType: "vault", BalancePence: 500_000})
	runner.stateCache.Rates = append(runner.stateCache.Rates, Rate{FromRegion: "north", ToRegion: yard.BankingRegion, FeeBPS: 100})
	h.api.setVaultBalance(yard.BankingRegion, 500_000)
	if changed, err := runner.considerExpansionAgent(h.ctx); err != nil || !changed {
		t.Fatalf("fee-free abandoned records blocked another region: changed=%t err=%v holds=%v", changed, err, runner.holding)
	}
	mission, data := h.mission("__treasury")
	if mission.Destination != yard.ID || data.Action == nil || data.Action.Stage != "hire-factor" || !stringsContainPort(data.Action.Body, yard.ID) {
		t.Fatalf("expired America pair did not release the African yard hire: %+v %+v", mission, data.Action)
	}
	snapshot, err := h.db.Snapshot(h.ctx, h.account.ID)
	if err != nil {
		t.Fatal(err)
	}
	agents := decode[[]expansionAgent](snapshot, "_expansion_agents")
	if len(agents) != 2 {
		t.Fatalf("abandoned records survived admission alongside the new pair: %+v", agents)
	}
	for _, agent := range agents {
		if agent.Region != yard.BankingRegion || !agent.HiredAt.IsZero() {
			t.Fatalf("abandoned or invented paid presence survived admission: %+v", agent)
		}
	}
	if got := runner.expansionRetryUntil("america"); !got.Equal(createdAt.Add(24 * time.Hour)) {
		t.Fatalf("fee-free expiry extended or erased the original regional retry: got=%s want=%s", got, createdAt.Add(24*time.Hour))
	}
	if outcome := decode[map[string]expansionOutcome](snapshot, "_expansion_agent_outcomes")["america"]; outcome.Losses != 0 {
		t.Fatalf("fee-free abandonment was recorded as a paid loss: %+v", outcome)
	}
	if !decode[time.Time](snapshot, "_expansion_last_started_at").IsZero() || h.api.mutationCount("/v1/company/hire-factor") != 0 {
		t.Fatal("staging or fee-free abandonment consumed the confirmed paid-pair throttle")
	}
}
