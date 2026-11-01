package engine

// Required production surface:
//   func (s *store.Store) LatestMetric(ctx context.Context, accountID, name string) (*store.Metric, error)
//   const capacitySolvencyReason string
// Hazard calibration uses the surface listed in overseas_hazard_estimate_acceptance_test.go.
// No growthEvidence fields beyond the existing production fields are required.

import (
	"bytes"
	"database/sql"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/DevYukine/go-tradewinds/internal/gameapi"
	"github.com/DevYukine/go-tradewinds/internal/store"
)

func overseasCapacityFixture(t *testing.T) (*recoveryHarness, *Runner, Port, Hull, YardQuote) {
	t.Helper()
	h, runner := capacityExecutionFixture(t, false)
	yard, hull, quote := overseasAddCapacityWorld(t, h, runner)
	return h, runner, yard, hull, quote
}

func overseasAddCapacityWorld(t *testing.T, h *recoveryHarness, runner *Runner) (Port, Hull, YardQuote) {
	t.Helper()
	now := time.Now().UTC()
	runner.stateCache.Clock.UnlockedPhase = 2
	runner.stateCache.Clock.Calendar.TimeScale = 60
	yard := Port{
		ID: "boston", BankingRegion: "america", SeaZoneID: "america-water", Phase: 2, HasShipyard: true,
		Produces: []string{recoveryGrain}, Imports: []string{"provisions"},
	}
	partner := Port{
		ID: "new_york", BankingRegion: "america", SeaZoneID: "america-water", Phase: 2,
		Imports: []string{recoveryGrain, "provisions"},
	}
	runner.stateCache.Catalog.Ports = append(runner.stateCache.Catalog.Ports, yard, partner)
	runner.stateCache.Catalog.SeaZones = append(runner.stateCache.Catalog.SeaZones, Zone{ID: "america-water", ZoneClass: "coastal", BaseStormBPS: 100})
	runner.stateCache.Catalog.Commodities = append(runner.stateCache.Catalog.Commodities,
		Commodity{ID: recoveryGrain, BasePricePence: 100}, Commodity{ID: "provisions", BasePricePence: 50})
	zones := decode[[]ZoneObservation](runner.snapshot, "zones")
	zones = append(zones, ZoneObservation{SeaZoneID: "america-water"})
	runner.snapshot["zones"] = encode(zones)
	runner.stateCache.Vaults = append(runner.stateCache.Vaults, Vault{BankingRegion: "america", AccountType: "vault", BalancePence: 500_000})
	runner.stateCache.Rates = append(runner.stateCache.Rates, Rate{FromRegion: "north", ToRegion: "america", FeeBPS: 100})
	h.api.setVaultBalance("america", 500_000)
	h.api.mu.Lock()
	h.api.rates = append(h.api.rates, Rate{FromRegion: "north", ToRegion: "america", FeeBPS: 100})
	h.api.mu.Unlock()
	for _, pair := range [][2]string{{yard.ID, recoveryHomePort}, {partner.ID, recoveryFarPort}} {
		var market Market
		if err := json.Unmarshal(encode(runner.stateCache.Markets[pair[1]]), &market); err != nil {
			t.Fatal(err)
		}
		market.PortID = pair[0]
		if pair[0] == partner.ID {
			bookCommodity(market, recoveryGrain).Depth.Bids[0].PricePence = 5_000
		}
		runner.stateCache.Markets[pair[0]] = market
		detail := runner.stateCache.Ports[pair[1]]
		detail.ID = pair[0]
		runner.stateCache.Ports[pair[0]] = detail
		for _, prefix := range []string{"books/", "ports/"} {
			runner.meta[prefix+pair[0]] = runner.meta[prefix+pair[1]]
		}
	}
	hull, _ := findHull(runner.stateCache.Catalog, recoveryHull)
	for _, pair := range [][2]string{{yard.ID, partner.ID}, {partner.ID, yard.ID}} {
		key := routeKey(pair[0], pair[1], hull.ID)
		runner.stateCache.Routes[key] = Route{
			FromPort: pair[0], ToPort: pair[1], ShipClass: hull.ID, TotalDistanceNM: 24, SafeProvisionsTons: 1,
			Legs: []RouteLeg{{FromNode: pair[0], ToNode: "america-water", EdgeClass: "coastal", DistanceNM: 12},
				{FromNode: "america-water", ToNode: pair[1], EdgeClass: "coastal", DistanceNM: 12}},
		}
		runner.meta["routes/"+key] = runner.meta["routes/"+routeKey(recoveryHomePort, recoveryFarPort, recoveryHull)]
	}
	quote := runner.stateCache.Quotes[recoveryHomePort+"|"+hull.ID]
	quote.PortID = yard.ID
	runner.stateCache.Quotes[yard.ID+"|"+hull.ID] = quote
	runner.meta["yard_quotes/"+yard.ID+"|"+hull.ID] = runner.meta["yard_quotes/"+recoveryHomePort+"|"+hull.ID]
	for index := range 10 {
		portID := fmt.Sprintf("overseas-calibration-%02d", index)
		runner.stateCache.Catalog.Ports = append(runner.stateCache.Catalog.Ports, Port{ID: portID, SeaZoneID: "zone-north", Phase: 1})
		route := Route{
			FromPort: recoveryHomePort, ToPort: portID, ShipClass: hull.ID, TotalDistanceNM: 200,
			Legs: []RouteLeg{{FromNode: recoveryHomePort, ToNode: "zone-north", EdgeClass: "coastal", DistanceNM: 100},
				{FromNode: "zone-north", ToNode: portID, EdgeClass: "coastal", DistanceNM: 100}},
		}
		runner.stateCache.Routes[routeKey(route.FromPort, route.ToPort, hull.ID)] = route
		runner.stateCache.HazardQuotes[fmt.Sprintf("overseas-sample-%02d", index)] = HazardQuote{
			ShipID: runner.stateCache.Ships[0].Ship.ID, Origin: route.FromPort, Destination: portID, ShipClass: hull.ID,
			CannonsCount: 4, Month: runner.stateCache.Clock.Calendar.Month, Laden: true,
			ExpectedLossBPS: 2, ObservedAt: now, NetPiracyBPS: map[string]int64{"zone-north": 0},
		}
	}
	ask, bid := int64(100), int64(5_000)
	detail := runner.stateCache.Ports[recoveryHomePort]
	detail.CoffeeHouseGossip = append(detail.CoffeeHouseGossip,
		Gossip{SubjectPortID: yard.ID, SnapshotAt: now, Commodities: []GossipCommodity{{CommodityID: recoveryGrain, SnapshotAt: now, BestAskPence: &ask}}},
		Gossip{SubjectPortID: partner.ID, SnapshotAt: now, Commodities: []GossipCommodity{{CommodityID: recoveryGrain, SnapshotAt: now, BestBidPence: &bid}}})
	runner.stateCache.Ports[recoveryHomePort] = detail
	base := h.api.server.Config.Handler
	h.api.server.Config.Handler = http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		if req.Method == http.MethodGet && req.URL.Path == "/v1/shipyard/quote" && req.URL.Query().Get("port_id") == yard.ID {
			h.api.writeJSON(w, http.StatusOK, runner.stateCache.Quotes[yard.ID+"|"+hull.ID])
			return
		}
		base.ServeHTTP(w, req)
	})
	return yard, hull, quote
}

func overseasMetric(t *testing.T, h *recoveryHarness, value int64, at time.Time) {
	t.Helper()
	connection, err := sql.Open("pgx", h.dsn)
	if err != nil {
		t.Fatal(err)
	}
	defer connection.Close()
	if _, err := connection.ExecContext(h.ctx, "DELETE FROM metrics WHERE account_id = $1 AND name = 'net_worth_pence'", h.account.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := connection.ExecContext(h.ctx, "INSERT INTO metrics (account_id, name, value, recorded_at) VALUES ($1, 'net_worth_pence', $2, $3)", h.account.ID, value, at); err != nil {
		t.Fatal(err)
	}
}

func overseasCapacityEvidence(t *testing.T, h *recoveryHarness, runner *Runner, at time.Time) (growthEvidence, CapacityGrowthInput) {
	t.Helper()
	required, err := runner.treasuryReserves(h.ctx)
	if err != nil {
		t.Fatal(err)
	}
	balances := runner.planningInput(ShipState{}).Balances
	home, _ := findPort(runner.stateCache.Catalog, runner.stateCache.HomePortID)
	evidence := growthEvidence{
		asOf: at, required: required, balances: balances, home: home,
		workingByRegion: make(map[string]int64), markets: runner.stateCache.Markets,
	}
	for region, balance := range balances {
		evidence.available += max(int64(0), balance-required[region])
	}
	input, reason, err := runner.capacityFleetInput(h.ctx, &evidence)
	if err != nil || reason != "" {
		t.Fatalf("overseas fleet evidence: %q %v", reason, err)
	}
	return evidence, input
}

func overseasCapacityDecision(t *testing.T, h *recoveryHarness, yard, hull string) store.Decision {
	t.Helper()
	history, err := h.db.History(h.ctx, h.account.ID, "decisions", store.MaxHistoryLimit)
	if err != nil {
		t.Fatal(err)
	}
	for _, row := range history.([]store.Decision) {
		if row.Kind == "capacity_candidate:"+yard+"|"+hull {
			return row
		}
	}
	t.Fatalf("no capacity_candidate verdict for %s|%s", yard, hull)
	return store.Decision{}
}

func overseasRequireEstimatePayload(t *testing.T, row store.Decision) {
	t.Helper()
	var payload struct {
		HazardSource   string          `json:"hazard_source"`
		HazardEstimate json.RawMessage `json:"hazard_estimate"`
	}
	if err := json.Unmarshal(row.Details, &payload); err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(payload.HazardSource, "zone estimate") || len(payload.HazardEstimate) == 0 ||
		bytes.Equal(payload.HazardEstimate, []byte("null")) || bytes.Equal(payload.HazardEstimate, []byte("false")) {
		t.Fatalf("estimated capacity verdict lost its labelled risk: %s", row.Details)
	}
}

func TestOverseasCapacityLatestMetricUsesPublicationTimeAndAccount(t *testing.T) {
	h := newRecoveryHarness(t)
	if metric, err := h.db.LatestMetric(h.ctx, h.account.ID, "net_worth_pence"); err != nil || metric != nil {
		t.Fatalf("missing metric is not unavailable: %+v %v", metric, err)
	}
	now := time.Now().UTC().Truncate(time.Microsecond)
	overseasMetric(t, h, 900_000, now)
	connection, err := sql.Open("pgx", h.dsn)
	if err != nil {
		t.Fatal(err)
	}
	defer connection.Close()
	if _, err := connection.ExecContext(h.ctx, "INSERT INTO metrics (account_id, name, value, recorded_at) VALUES ($1, 'net_worth_pence', 1, $2), ($1, 'other_metric', 999999999, $3)", h.account.ID, now.Add(-time.Hour), now.Add(time.Minute)); err != nil {
		t.Fatal(err)
	}
	metric, err := h.db.LatestMetric(h.ctx, h.account.ID, "net_worth_pence")
	if err != nil || metric == nil || metric.Value != 900_000 || !metric.RecordedAt.Equal(now) {
		t.Fatalf("latest metric followed insertion order or another series: %+v %v", metric, err)
	}
	if metric, err := h.db.LatestMetric(h.ctx, "another-account", "net_worth_pence"); err != nil || metric != nil {
		t.Fatalf("another account inherited our valuation: %+v %v", metric, err)
	}
}

func TestOverseasCapacityEstimateRequiresUnownedRegionAndFreshNetWorth(t *testing.T) {
	for _, tc := range []struct {
		name string
		age  time.Duration
		ship string
		want bool
	}{
		{name: "no ships in region", want: true},
		{name: "exactly six real hours", age: 6 * time.Hour, want: true},
		{name: "six hours plus one microsecond", age: 6*time.Hour + time.Microsecond},
		{name: "missing net worth", age: -1},
		{name: "live ship in region", ship: "sailing"},
		{name: "sunk ship is not live", ship: "sunk", want: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			h, runner, yard, hull, quote := overseasCapacityFixture(t)
			at := time.Now().UTC().Truncate(time.Microsecond)
			if tc.age >= 0 {
				overseasMetric(t, h, 10_000_000, at.Add(-tc.age))
			}
			if tc.ship != "" {
				ship := runner.stateCache.Ships[0]
				ship.Ship.ID, ship.Ship.Status, ship.Ship.CurrentPortID = "regional-presence", tc.ship, "new_york"
				lostAt := at.Add(-25 * time.Hour)
				ship.Ship.UpdatedAt = &lostAt
				runner.stateCache.Ships = append(runner.stateCache.Ships, ship)
			}
			evidence, input := overseasCapacityEvidence(t, h, runner, at)
			decision, _, err := runner.capacityAtYard(h.ctx, yard, hull, quote, input, &evidence)
			if err != nil || decision.Accepted != tc.want {
				t.Fatalf("estimate eligibility: %+v %v", decision, err)
			}
			row := overseasCapacityDecision(t, h, yard.ID, hull.ID)
			if tc.want {
				overseasRequireEstimatePayload(t, row)
				return
			}
			if tc.ship == "sailing" {
				var payload struct {
					Accepted       bool           `json:"accepted"`
					HazardSource   string         `json:"hazard_source"`
					PlanRejections map[string]int `json:"plan_rejections"`
				}
				if err := json.Unmarshal(row.Details, &payload); err != nil {
					t.Fatal(err)
				}
				if payload.Accepted || strings.HasPrefix(payload.HazardSource, "zone estimate") || decision.Reason == hazardUnavailable {
					t.Fatalf("owned region gained an estimate or lost its ordinary headline: %+v %s", decision, row.Details)
				}
				hazardRejected := false
				for reason, count := range payload.PlanRejections {
					if count > 0 && strings.Contains(reason, hazardUnavailable) {
						hazardRejected = true
					}
				}
				if !hazardRejected {
					t.Fatalf("unpriced owned-region route lost its hazard rejection: %s", row.Details)
				}
			}
			if decision.EconomicallyViable || decision.Reason == capacitySolvencyReason || !strings.Contains(string(row.Details), hazardUnavailable) {
				t.Fatalf("missing risk evidence was scored as a cap/economic loss: %+v %s", decision, row.Details)
			}
		})
	}
}

func overseasCapacityBoundary(t *testing.T, h *recoveryHarness, runner *Runner, yard Port, hull Hull, quote YardQuote) (YardQuote, int64, int64) {
	t.Helper()
	at := time.Now().UTC()
	overseasMetric(t, h, 10_000_000, at)
	evidence, input := overseasCapacityEvidence(t, h, runner, at)
	baseline, _, err := runner.capacityAtYard(h.ctx, yard, hull, quote, input, &evidence)
	if err != nil || !baseline.Accepted {
		t.Fatalf("exposure boundary control: %+v %v", baseline, err)
	}
	for price := *quote.TotalCostPence; price < *quote.TotalCostPence+70; price++ {
		exposure := price + (price+9)/10 + baseline.CannonsPence + baseline.InitialStoresPence
		if exposure%7 == 0 && (price+9)/10 == (price+10)/10 {
			quote.TotalCostPence = &price
			runner.stateCache.Quotes[yard.ID+"|"+hull.ID] = quote
			return quote, exposure * 20 / 7, exposure
		}
	}
	t.Fatal("could not place the integer-pence 35% boundary")
	return YardQuote{}, 0, 0
}

func TestOverseasCapacityEstimatedExposureBoundaryCountsGrossMortgage(t *testing.T) {
	for _, mortgage := range []bool{false, true} {
		for _, excess := range []int64{0, 1} {
			t.Run(fmt.Sprintf("mortgage=%t/excess=%dp", mortgage, excess), func(t *testing.T) {
				h, runner, yard, hull, quote := overseasCapacityFixture(t)
				quote, netWorth, gross := overseasCapacityBoundary(t, h, runner, yard, hull, quote)
				price := *quote.TotalCostPence + excess
				quote.TotalCostPence = &price
				runner.stateCache.Quotes[yard.ID+"|"+hull.ID] = quote
				at := time.Now().UTC()
				overseasMetric(t, h, netWorth, at)
				evidence, input := overseasCapacityEvidence(t, h, runner, at)
				if mortgage {
					input.Mortgage, input.FinancingFixed = mortgageTestTerms(), true
				}
				decision, _, err := runner.capacityAtYard(h.ctx, yard, hull, quote, input, &evidence)
				if err != nil || decision.Accepted != (excess == 0) {
					t.Fatalf("35%% gross exposure: %+v %v", decision, err)
				}
				actual := decision.QuotedHullPence + decision.SlippageReservePence + decision.CannonsPence + decision.InitialStoresPence
				if actual != gross+excess || mortgage && (decision.Mortgage == nil || growthCapital(decision) >= actual) {
					t.Fatalf("gross pence or financed capital changed: gross=%d want=%d decision=%+v", actual, gross+excess, decision)
				}
				if excess != 0 && (decision.EconomicallyViable || decision.Reason != capacitySolvencyReason) {
					t.Fatalf("one penny over cap did not give the typed non-viable reason: %+v", decision)
				}
				overseasRequireEstimatePayload(t, overseasCapacityDecision(t, h, yard.ID, hull.ID))
			})
		}
	}
}

func TestOverseasCapacityBuildReadyReappliesExposureAndValuation(t *testing.T) {
	for _, change := range []string{"exact cap", "one penny over cap", "stale valuation"} {
		t.Run(change, func(t *testing.T) {
			h, runner, yard, hull, quote := overseasCapacityFixture(t)
			runner.stateCache.Factors = append(runner.stateCache.Factors, yard.ID)
			quote, netWorth, _ := overseasCapacityBoundary(t, h, runner, yard, hull, quote)
			at := time.Now().UTC()
			if change == "one penny over cap" {
				price := *quote.TotalCostPence + 1
				quote.TotalCostPence = &price
				runner.stateCache.Quotes[yard.ID+"|"+hull.ID] = quote
			}
			if change == "stale valuation" {
				at = at.Add(-6*time.Hour - time.Second)
			}
			overseasMetric(t, h, netWorth, at)
			data := missionData{
				BuildPort: yard.ID, BuildClass: hull.ID, TargetCannons: 4,
				Capacity: &capacityIntent{BuildPort: yard.ID, BuildClass: hull.ID, QuoteLimitPence: 1_000_000, AdmittedAt: time.Now().UTC()},
			}
			err := runner.capacityBuildReady(h.ctx, &data)
			if (err == nil) != (change == "exact cap") {
				t.Fatalf("%s preflight: %v", change, err)
			}
			if change == "one penny over cap" && err.Error() != capacitySolvencyReason {
				t.Fatalf("preflight did not preserve the exposure verdict: %v", err)
			}
			if change == "stale valuation" {
				row := overseasCapacityDecision(t, h, yard.ID, hull.ID)
				if err.Error() == capacitySolvencyReason || !strings.Contains(string(row.Details), hazardUnavailable) {
					t.Fatalf("stale preflight valuation masqueraded as a cap rejection: %v %s", err, row.Details)
				}
			}
			if h.api.mutationCount("/v1/shipyard/build-ship") != 0 {
				t.Fatal("capacityBuildReady dispatched construction")
			}
		})
	}
}

func TestOverseasCapacityNetWorthIsStableWithinOneGrowthPass(t *testing.T) {
	h, runner, yard, hull, quote := overseasCapacityFixture(t)
	at := time.Now().UTC()
	overseasMetric(t, h, 10_000_000, at)
	evidence, input := overseasCapacityEvidence(t, h, runner, at)
	first, _, err := runner.capacityAtYard(h.ctx, yard, hull, quote, input, &evidence)
	if err != nil || !first.Accepted {
		t.Fatalf("first growth candidate: %+v %v", first, err)
	}
	overseasMetric(t, h, 1, at.Add(time.Microsecond))
	second, _, err := runner.capacityAtYard(h.ctx, yard, hull, quote, input, &evidence)
	if err != nil || !second.Accepted {
		t.Fatalf("one growth pass mixed valuation publications: %+v %v", second, err)
	}
	nextEvidence, nextInput := overseasCapacityEvidence(t, h, runner, at.Add(time.Second))
	next, _, err := runner.capacityAtYard(h.ctx, yard, hull, quote, nextInput, &nextEvidence)
	if err != nil || next.Accepted || next.Reason != capacitySolvencyReason {
		t.Fatalf("next growth pass did not reload the valuation: %+v %v", next, err)
	}
}

func TestOverseasCapacityEuropeanBuildExploresThenBetterCandidateWins(t *testing.T) {
	h, runner, yard, hull, quote := overseasCapacityFixture(t)
	overseasMetric(t, h, 10_000_000, time.Now().UTC())
	evidence, input := overseasCapacityEvidence(t, h, runner, time.Now().UTC())
	home, _ := findPort(runner.stateCache.Catalog, recoveryHomePort)
	localQuote := runner.stateCache.Quotes[home.ID+"|"+hull.ID]
	local, _, err := runner.capacityAtYard(h.ctx, home, hull, localQuote, input, &evidence)
	if err != nil || !local.Accepted {
		t.Fatalf("European control build is not admitted: %+v %v", local, err)
	}
	if changed, err := runner.considerCapacityGrowth(h.ctx, &evidence); err != nil || !changed {
		t.Fatalf("admitted European build did not stage exploration: %t %v", changed, err)
	}
	h.missingMission("__growth")
	mission, hire := h.mission("__treasury")
	if hire.Action == nil || hire.Action.Stage != "hire-factor" || mission.Destination != yard.ID {
		t.Fatalf("European spending preceded the admissible pair: %+v %+v", mission, hire.Action)
	}
	if err := h.db.DeleteMission(h.ctx, h.account.ID, "__treasury"); err != nil {
		t.Fatal(err)
	}
	runner.stateCache.Factors = append(runner.stateCache.Factors, yard.ID, "new_york")
	runner.snapshot["_expansion_agents"] = encode([]expansionAgent{
		{PortID: yard.ID, Partner: "new_york", Region: yard.BankingRegion, HiredAt: time.Now().UTC().Add(-3 * time.Hour)},
		{PortID: "new_york", Partner: yard.ID, Region: yard.BankingRegion, HiredAt: time.Now().UTC().Add(-3 * time.Hour)},
	})
	evidence, input = overseasCapacityEvidence(t, h, runner, time.Now().UTC())
	overseas, _, err := runner.capacityAtYard(h.ctx, yard, hull, quote, input, &evidence)
	if err != nil || !overseas.Accepted || overseas.IncrementalNetPencePerGameDay <= local.IncrementalNetPencePerGameDay {
		t.Fatalf("overseas fixture is not better after costs: local=%+v overseas=%+v err=%v", local, overseas, err)
	}
	if changed, err := runner.considerCapacityGrowth(h.ctx, &evidence); err != nil || !changed {
		t.Fatalf("evaluated overseas candidate did not compete: %t %v", changed, err)
	}
	_, build := h.mission("__growth")
	if build.BuildPort != yard.ID || build.Action == nil || build.Action.Stage != "build" {
		t.Fatalf("European candidate won over better overseas net: %+v", build)
	}
	overseasRequireEstimatePayload(t, overseasCapacityDecision(t, h, yard.ID, hull.ID))
}

func TestOverseasCapacityMissingGossipAndValuationHoldEuropeanBuild(t *testing.T) {
	h, runner, _, _, _ := overseasCapacityFixture(t)
	detail := runner.stateCache.Ports[recoveryHomePort]
	detail.CoffeeHouseGossip = nil
	runner.stateCache.Ports[recoveryHomePort] = detail
	evidence, _ := overseasCapacityEvidence(t, h, runner, time.Now().UTC())
	if changed, err := runner.considerCapacityGrowth(h.ctx, &evidence); err != nil || changed {
		t.Fatalf("unexplored region without probe valuation released Europe: %t %v", changed, err)
	}
	h.missingMission("__growth")
	requireReviewHold(t, runner, "Phase 1 growth waits: new hulls go overseas", true)
	h.missingMission("__treasury")
	history, err := h.db.History(h.ctx, h.account.ID, "decisions", store.MaxHistoryLimit)
	if err != nil {
		t.Fatal(err)
	}
	recorded := false
	for _, row := range history.([]store.Decision) {
		if !strings.HasPrefix(row.Kind, "expansion") || row.Reason == "" {
			continue
		}
		var payload map[string]json.RawMessage
		if err := json.Unmarshal(row.Details, &payload); err != nil {
			t.Fatal(err)
		}
		recorded = len(payload["region"]) > 0 && len(payload["reason"]) > 0
		if recorded {
			break
		}
	}
	if !recorded {
		t.Fatal("quiet exploration skip did not record its region and reason")
	}
}

func TestOverseasCapacityAwaitingPairBlocksOnlyUntilEvaluationOrFloor(t *testing.T) {
	for _, mode := range []string{"awaiting evaluation", "partner book stale", "evaluated within floor", "floor expired"} {
		t.Run(mode, func(t *testing.T) {
			h, runner, yard, hull, _ := overseasCapacityFixture(t)
			hiredAt := time.Now().UTC().Add(-time.Hour)
			if mode == "floor expired" {
				hiredAt = time.Now().UTC().Add(-2*time.Hour - time.Second)
			}
			runner.snapshot["_expansion_agents"] = encode([]expansionAgent{
				{PortID: yard.ID, Partner: "new_york", Region: yard.BankingRegion, HiredAt: hiredAt},
				{PortID: "new_york", Partner: yard.ID, Region: yard.BankingRegion, HiredAt: hiredAt},
			})
			runner.stateCache.Factors = append(runner.stateCache.Factors, yard.ID, "new_york")
			if mode == "evaluated within floor" || mode == "partner book stale" {
				bookCommodity(runner.stateCache.Markets[yard.ID], recoveryGrain).Depth.Asks = nil
			}
			stalePort := yard.ID
			if mode == "partner book stale" {
				stalePort = "new_york"
			}
			if mode != "evaluated within floor" {
				meta := runner.meta["books/"+stalePort]
				meta.Stale = true
				runner.meta["books/"+stalePort] = meta
			}
			if mode == "evaluated within floor" || mode == "floor expired" {
				// Releasing the pair-evaluation hold does not bypass the
				// overseas-first rule; Europe also needs an exhausted region.
				runner.snapshot["_expansion_agent_outcomes"] = encode(map[string]expansionOutcome{
					yard.BankingRegion: {Losses: 1, RetryAt: time.Now().UTC().Add(24 * time.Hour)},
				})
			}
			evidence, _ := overseasCapacityEvidence(t, h, runner, time.Now().UTC())
			changed, err := runner.considerCapacityGrowth(h.ctx, &evidence)
			blocked := mode == "awaiting evaluation" || mode == "partner book stale"
			if err != nil || changed == blocked {
				t.Fatalf("pair wait %s: changed=%t err=%v holds=%v", mode, changed, err, runner.holding)
			}
			if blocked {
				h.missingMission("__growth")
				h.missingMission("__treasury")
				if len(runner.holding) == 0 {
					t.Fatal("unexplained exploration hold")
				}
				return
			}
			_, build := h.mission("__growth")
			if build.BuildPort != recoveryHomePort || build.Action == nil || build.Action.Stage != "build" {
				t.Fatalf("completed/expired exploration retained the European build: %+v", build)
			}
			if mode == "evaluated within floor" {
				row := overseasCapacityDecision(t, h, yard.ID, hull.ID)
				if row.Reason == "" {
					t.Fatal("overseas evaluation lost its rejection reason")
				}
			}
		})
	}
}

func TestOverseasCapacityFundsYardRegionBeforeStagingBuild(t *testing.T) {
	h, runner, yard, hull, _ := overseasCapacityFixture(t)
	overseasMetric(t, h, 10_000_000, time.Now().UTC())
	runner.stateCache.Factors = append(runner.stateCache.Factors, yard.ID, "new_york")
	for index := range runner.stateCache.Vaults {
		if runner.stateCache.Vaults[index].BankingRegion == yard.BankingRegion {
			runner.stateCache.Vaults[index].BalancePence = 0
		}
	}
	h.api.setVaultBalance(yard.BankingRegion, 0)
	evidence, _ := overseasCapacityEvidence(t, h, runner, time.Now().UTC())
	if changed, err := runner.considerCapacityGrowth(h.ctx, &evidence); err != nil || !changed {
		t.Fatalf("overseas funding admission: %t %v", changed, err)
	}
	h.missingMission("__growth")
	mission, remit := h.mission("__treasury")
	var terms struct {
		From string `json:"from_region"`
		To   string `json:"to_region"`
	}
	if remit.Action == nil || remit.Action.Stage != "remit" || json.Unmarshal(remit.Action.Body, &terms) != nil || terms.From != "north" || terms.To != yard.BankingRegion {
		t.Fatalf("overseas yard did not get its regional remit first: %+v %+v", remit.Action, terms)
	}
	if changed, err := runner.dispatch(h.ctx, mission, &remit); err != nil || !changed {
		t.Fatalf("dispatch overseas remit: %t %v", changed, err)
	}
	if err := h.db.DeleteMission(h.ctx, h.account.ID, "__treasury"); err != nil {
		t.Fatal(err)
	}
	h.api.mu.Lock()
	for index := range runner.stateCache.Vaults {
		vault := &runner.stateCache.Vaults[index]
		vault.BalancePence = h.api.vaultBalance[vault.BankingRegion]
	}
	h.api.mu.Unlock()
	evidence, _ = overseasCapacityEvidence(t, h, runner, time.Now().UTC())
	if changed, err := runner.considerCapacityGrowth(h.ctx, &evidence); err != nil || !changed {
		t.Fatalf("funded overseas hull not staged: %t %v", changed, err)
	}
	_, build := h.mission("__growth")
	if build.BuildPort != yard.ID || build.BuildClass != hull.ID || build.Action == nil || build.Action.Stage != "build" {
		t.Fatalf("regional remit did not release the overseas build: %+v", build)
	}
	if h.api.mutationCount("/v1/treasury/remit") != 1 || h.api.mutationCount("/v1/shipyard/build-ship") != 0 {
		t.Fatal("regional funding repeated a remit or sent construction instead of staging it")
	}
}

func TestOverseasCapacityEveryPresentYardRecordsVerdictAndReason(t *testing.T) {
	for _, unavailable := range []bool{false, true} {
		t.Run(fmt.Sprintf("unavailable_quote=%t", unavailable), func(t *testing.T) {
			h, runner, yard, hull, quote := overseasCapacityFixture(t)
			runner.stateCache.Factors = append(runner.stateCache.Factors, yard.ID, "new_york")
			if unavailable {
				quote.Available = false
				runner.stateCache.Quotes[yard.ID+"|"+hull.ID] = quote
			}
			evidence, _ := overseasCapacityEvidence(t, h, runner, time.Now().UTC())
			if _, err := runner.considerCapacityGrowth(h.ctx, &evidence); err != nil {
				t.Fatal(err)
			}
			row := overseasCapacityDecision(t, h, yard.ID, hull.ID)
			var payload struct {
				Accepted bool `json:"accepted"`
			}
			if err := json.Unmarshal(row.Details, &payload); err != nil || row.Reason == "" || payload.Accepted {
				t.Fatalf("missing risk/quote authority did not get a rejected verdict and reason: %+v %v", row, err)
			}
		})
	}
}

func TestOverseasCapacityMortgageAndFinancedPreflightsReapplyRiskCap(t *testing.T) {
	for _, mode := range []string{"mortgage", "financed"} {
		for _, change := range []string{"exact cap", "one penny over cap", "stale valuation"} {
			t.Run(mode+"/"+change, func(t *testing.T) {
				h, runner, yard, hull, quote := overseasCapacityFixture(t)
				runner.stateCache.Factors = append(runner.stateCache.Factors, yard.ID)
				quote, netWorth, _ := overseasCapacityBoundary(t, h, runner, yard, hull, quote)
				at := time.Now().UTC()
				overseasMetric(t, h, netWorth, at)
				evidence, input := overseasCapacityEvidence(t, h, runner, at)
				admitted, _, err := runner.capacityAtYard(h.ctx, yard, hull, quote, input, &evidence)
				if err != nil || !admitted.Accepted {
					t.Fatalf("admitted financing control: %+v %v", admitted, err)
				}
				data := missionData{
					BuildPort: yard.ID, BuildClass: hull.ID, TargetCannons: 4,
					Capacity: &capacityIntent{BuildPort: yard.ID, BuildClass: hull.ID, QuoteLimitPence: 1_000_000, AdmittedAt: at},
				}
				if mode == "mortgage" {
					quote.Mortgage = mortgageTestTerms()
					data.Capacity.Mortgage = quote.Mortgage
				} else {
					loan := snapshotTestLoan()
					loan.ID, loan.BorrowerCompanyID, loan.BankingRegion = "overseas-financing", runner.account.CompanyID, "north"
					loan.IsMortgage, loan.Status = false, "active"
					loan.PrincipalPence, loan.OutstandingPrincipalPence, loan.OwedPence = 10_000, 10_000, 10_000
					payment := int64(100)
					loan.InstallmentPence, loan.NextPaymentPence = &payment, &payment
					runner.stateCache.Loans = append(runner.stateCache.Loans, loan)
					runner.snapshot["loans"] = encode(runner.stateCache.Loans)
					runner.meta["loans"] = runner.meta["ships"]
					data.Capacity.Financing = &capacityFinancing{
						Decision: admitted, Loans: []gameapi.Loan{loan},
						Quotes: []gameapi.LoanQuote{{PrincipalPence: loan.PrincipalPence, AnnualRateBPS: 300, InstallmentPence: &payment, ExpiresAt: at.Add(time.Hour)}},
					}
				}
				if change == "one penny over cap" {
					price := *quote.TotalCostPence + 1
					quote.TotalCostPence = &price
				}
				runner.stateCache.Quotes[yard.ID+"|"+hull.ID] = quote
				if change == "stale valuation" {
					overseasMetric(t, h, netWorth, at.Add(-6*time.Hour-time.Second))
				}
				err = runner.capacityBuildReady(h.ctx, &data)
				if (err == nil) != (change == "exact cap") {
					t.Fatalf("%s %s preflight: %v", mode, change, err)
				}
				if change == "one penny over cap" && err.Error() != capacitySolvencyReason {
					t.Fatalf("%s preflight bypassed the typed exposure verdict: %v", mode, err)
				}
				if change == "stale valuation" {
					row := overseasCapacityDecision(t, h, yard.ID, hull.ID)
					if err.Error() == capacitySolvencyReason || !strings.Contains(string(row.Details), hazardUnavailable) {
						t.Fatalf("%s stale valuation became an economic/cap rejection: %v %s", mode, err, row.Details)
					}
				}
				if h.api.mutationCount("/v1/shipyard/build-ship") != 0 {
					t.Fatal("a preflight sent construction")
				}
			})
		}
	}
}

func TestOverseasGrowthLaneBuildWaitsThenCompetesWithExploration(t *testing.T) {
	h := newRecoveryHarness(t)
	row := sailingShipRow(recoveryShipBlastoise, "Blastoise", recoveryHomePort, recoveryFarPort)
	row["ship"].(map[string]any)["created_at"] = time.Now().UTC().Add(-30 * 24 * time.Hour)
	h.api.setFleet([]map[string]any{row})
	h.seedLaneProofs()
	persistGrowthWindowSettlement(t, h, store.Settlement{
		ID: "overseas-lane-measured-income", Kind: "spot", CostsKnown: true, QuantityTons: 48,
		AcquisitionPence: 4_800, ProceedsPence: 2_000_000, GameSeconds: 43_200,
		CompletedAt: time.Now().UTC().Add(-time.Minute),
	})
	runner := growthWindowRunner(t, h)
	yard, hull, quote := overseasAddCapacityWorld(t, h, runner)
	overseasMetric(t, h, 10_000_000, time.Now().UTC())
	evidence := capacityMeasuredEvidence(t, h, runner, time.Now().UTC())
	if reason, err := runner.growthLaneProof(h.ctx, &evidence); err != nil || reason != "" {
		t.Fatalf("lane growth control lacks durable lane evidence: %q %v", reason, err)
	}
	home, _ := findPort(runner.stateCache.Catalog, recoveryHomePort)
	local, feasible, err := runner.growthAtYard(h.ctx, home, hull, runner.stateCache.Quotes[home.ID+"|"+hull.ID], 0, &evidence, nil)
	if err != nil || !feasible || !local.decision.Accepted {
		t.Fatalf("European lane control is not admitted: %+v %t %v", local.decision, feasible, err)
	}
	if changed, err := runner.considerGrowth(h.ctx); err != nil || !changed {
		t.Fatalf("lane growth bypassed the admissible exploration: %t %v", changed, err)
	}
	h.missingMission("__growth")
	mission, hire := h.mission("__treasury")
	if mission.Destination != yard.ID || hire.Action == nil || hire.Action.Stage != "hire-factor" {
		t.Fatalf("admitted European lane spent cash before exploration: %+v %+v", mission, hire.Action)
	}
	if err := h.db.DeleteMission(h.ctx, h.account.ID, "__treasury"); err != nil {
		t.Fatal(err)
	}
	runner.stateCache.Factors = append(runner.stateCache.Factors, yard.ID, "new_york")
	hired := time.Now().UTC().Add(-time.Hour)
	runner.snapshot["_expansion_agents"] = encode([]expansionAgent{
		{PortID: yard.ID, Partner: "new_york", Region: yard.BankingRegion, HiredAt: hired},
		{PortID: "new_york", Partner: yard.ID, Region: yard.BankingRegion, HiredAt: hired},
	})
	evidence, input := overseasCapacityEvidence(t, h, runner, time.Now().UTC())
	overseas, _, err := runner.capacityAtYard(h.ctx, yard, hull, quote, input, &evidence)
	if err != nil || !overseas.Accepted || overseas.IncrementalNetPencePerGameDay <= local.decision.IncrementalNetPencePerGameDay {
		t.Fatalf("explored candidate is not the better net earner: local=%+v overseas=%+v err=%v", local.decision, overseas, err)
	}
	if changed, err := runner.considerGrowth(h.ctx); err != nil || !changed {
		t.Fatalf("lane path did not release its better explored candidate: %t %v", changed, err)
	}
	_, build := h.mission("__growth")
	if build.BuildPort != yard.ID || build.Action == nil || build.Action.Stage != "build" {
		t.Fatalf("lane path still preferred Europe after overseas evaluation: %+v", build)
	}
}

func TestOverseasExpansionInFlightDefersVoluntaryLoanPrepayment(t *testing.T) {
	for _, state := range []string{"awaiting hire", "awaiting evaluation", "floor expired"} {
		t.Run(state, func(t *testing.T) {
			h, runner, _, _ := moneyPrepayFixture(t, []int64{563_785}, 1_000_000, false)
			now := time.Now().UTC()
			agent := expansionAgent{PortID: "boston", Partner: "new_york", Region: "america", CreatedAt: now}
			mate := expansionAgent{PortID: "new_york", Partner: "boston", Region: "america", CreatedAt: now}
			if state != "awaiting hire" {
				agent.HiredAt, mate.HiredAt = now.Add(-time.Hour), now.Add(-time.Hour)
			}
			if state == "floor expired" {
				agent.HiredAt, mate.HiredAt = now.Add(-3*time.Hour), now.Add(-3*time.Hour)
			}
			runner.stateCache.Catalog.Ports = append(runner.stateCache.Catalog.Ports,
				Port{ID: "boston", BankingRegion: "america", HasShipyard: true, SeaZoneID: "zone-north", Phase: 2},
				Port{ID: "new_york", BankingRegion: "america", SeaZoneID: "zone-north", Phase: 2})
			runner.snapshot["_expansion_agents"] = encode([]expansionAgent{agent, mate})
			changed, err := runner.prepayLoans(h.ctx)
			want := state == "floor expired"
			if err != nil || changed != want || h.api.mutationCount("/v1/loan/repay") != map[bool]int{false: 0, true: 1}[want] {
				t.Fatalf("pair %s voluntary prepayment: changed=%t want=%t err=%v", state, changed, want, err)
			}
			if !want {
				h.missingMission("__loans")
			}
		})
	}
}

func TestOverseasCapacityCashShortPairedWinnerBecomesSavingsTarget(t *testing.T) {
	h, runner, _, _ := capacitySavingsRunnerFixture(t, true)
	yard, small, _ := overseasAddCapacityWorld(t, h, runner)
	overseasMetric(t, h, 10_000_000, time.Now().UTC())
	runner.stateCache.Factors = append(runner.stateCache.Factors, yard.ID, "new_york")
	for i := range runner.stateCache.Vaults {
		if runner.stateCache.Vaults[i].BankingRegion == yard.BankingRegion {
			runner.stateCache.Vaults[i].BalancePence = 0
		}
	}
	hiredAt := time.Now().UTC().Add(-3 * time.Hour)
	runner.snapshot["_expansion_agents"] = encode([]expansionAgent{
		{PortID: yard.ID, Partner: "new_york", Region: yard.BankingRegion, HiredAt: hiredAt},
		{PortID: "new_york", Partner: yard.ID, Region: yard.BankingRegion, HiredAt: hiredAt},
	})
	large, _ := findHull(runner.stateCache.Catalog, "brigantine")
	quote := runner.stateCache.Quotes[recoveryHomePort+"|"+large.ID]
	delete(runner.stateCache.Quotes, recoveryHomePort+"|"+large.ID)
	quote.PortID = yard.ID
	runner.stateCache.Quotes[yard.ID+"|"+large.ID] = quote
	runner.meta["yard_quotes/"+yard.ID+"|"+large.ID] = runner.meta["yard_quotes/"+recoveryHomePort+"|"+large.ID]
	for _, pair := range [][2]string{{yard.ID, "new_york"}, {"new_york", yard.ID}} {
		key := routeKey(pair[0], pair[1], large.ID)
		sourceKey := routeKey(pair[0], pair[1], small.ID)
		route := runner.stateCache.Routes[sourceKey]
		route.ShipClass = large.ID
		runner.stateCache.Routes[key] = route
		runner.meta["routes/"+key] = runner.meta["routes/"+sourceKey]
	}
	bookCommodity(runner.stateCache.Markets["new_york"], recoveryGrain).Depth.Bids[0].PricePence = 50_000
	// This savings-only fixture has already exhausted player credit here.
	runner.snapshot["_player_loan_fallback"] = encode(map[string]time.Time{
		yard.ID + "|" + large.ID: time.Now().UTC().Add(6 * time.Hour),
	})
	evidence, input := overseasCapacityEvidence(t, h, runner, time.Now().UTC())
	candidate, _, err := runner.capacityAtYard(h.ctx, yard, large, quote, input, &evidence)
	if err != nil || candidate.Accepted || !candidate.EconomicallyViable || candidate.CashShortfallPence <= 0 || candidate.DepthNetPencePerGameDay <= 0 {
		t.Fatalf("paired overseas winner must be cash-short, not uneconomic: %+v %v", candidate, err)
	}
	if _, err := runner.considerCapacityGrowth(h.ctx, &evidence); err != nil {
		t.Fatal(err)
	}
	target := decode[capacitySavingsTarget](runner.snapshot, "_capacity_savings_target")
	if target.PortID != yard.ID || target.HullID != large.ID || target.RequiredPence <= evidence.available || target.DepthNetPencePerGameDay <= 0 {
		t.Fatalf("agent-only overseas winner was excluded from savings: %+v", target)
	}
	h.missingMission("__growth")
	h.missingMission("__treasury")
}

func TestOverseasCapacityCalibrationIsStableForOneHullWithinGrowthPass(t *testing.T) {
	h, runner, yard, hull, quote := overseasCapacityFixture(t)
	at := time.Now().UTC()
	overseasMetric(t, h, 10_000_000, at)
	evidence, input := overseasCapacityEvidence(t, h, runner, at)
	first, _, err := runner.capacityAtYard(h.ctx, yard, hull, quote, input, &evidence)
	if err != nil || !first.Accepted {
		t.Fatalf("calibrated growth-pass control: %+v %v", first, err)
	}
	delete(runner.stateCache.HazardQuotes, "overseas-sample-09")
	second, _, err := runner.capacityAtYard(h.ctx, yard, hull, quote, input, &evidence)
	if err != nil || !second.Accepted {
		t.Fatalf("same hull mixed hazard samples within one growth pass: %+v %v", second, err)
	}
	nextEvidence, nextInput := overseasCapacityEvidence(t, h, runner, time.Now().UTC())
	next, _, err := runner.capacityAtYard(h.ctx, yard, hull, quote, nextInput, &nextEvidence)
	if err != nil || next.Accepted || next.Reason == capacitySolvencyReason {
		t.Fatalf("next growth pass did not use the nine-sample publication: %+v %v", next, err)
	}
	if row := overseasCapacityDecision(t, h, yard.ID, hull.ID); !strings.Contains(string(row.Details), hazardUnavailable) {
		t.Fatalf("unavailable next-pass calibration lost its hazard verdict: %s", row.Details)
	}
}

func TestOverseasExpansionQuietSkipDecisionIsDeduplicated(t *testing.T) {
	h, runner, _ := overseasPairFixture(t)
	delete(runner.stateCache.HazardQuotes, "overseas-sample-09")
	for range 2 {
		if changed, err := runner.considerExpansionAgent(h.ctx); err != nil || changed {
			t.Fatalf("nine-sample quiet admission mutated state: %t %v", changed, err)
		}
	}
	history, err := h.db.History(h.ctx, h.account.ID, "decisions", store.MaxHistoryLimit)
	if err != nil {
		t.Fatal(err)
	}
	count := 0
	for _, row := range history.([]store.Decision) {
		if !strings.HasPrefix(row.Kind, "expansion") {
			continue
		}
		var payload struct {
			Region string `json:"region"`
			Reason string `json:"reason"`
		}
		if err := json.Unmarshal(row.Details, &payload); err != nil {
			t.Fatal(err)
		}
		if row.Reason == "" || payload.Region != "america" || payload.Reason == "" {
			t.Fatalf("quiet skip lost its region or reason: %+v %s", row, row.Details)
		}
		count++
	}
	if count != 1 {
		t.Fatalf("unchanged quiet admission wrote %d expansion decisions; want one deduped verdict", count)
	}
	h.missingMission("__treasury")
}
