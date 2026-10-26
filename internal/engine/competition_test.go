package engine

import (
	"context"
	"encoding/json"
	"reflect"
	"testing"
	"time"

	"github.com/DevYukine/go-tradewinds/internal/store"
	"go.uber.org/zap"
	"go.uber.org/zap/zaptest/observer"
)

func competitionFixture() *store.CompetitionSummary {
	low, high, companies := 2, 20, 1
	return &store.CompetitionSummary{Regions: []store.CompetitionRegion{
		{ID: "low", CompetitionActivity: store.CompetitionActivity{Coverage: "partial", ForeignShips: &low, ForeignCompanies: &companies}},
		{ID: "high", CompetitionActivity: store.CompetitionActivity{Coverage: "partial", ForeignShips: &high, ForeignCompanies: &companies}},
		{ID: "unknown", CompetitionActivity: store.CompetitionActivity{Coverage: "unobserved"}},
	}}
}

func competitionPlanning() PlanningInput {
	return PlanningInput{Competition: competitionFixture(), Catalog: Catalog{Ports: []Port{
		{ID: "low", BankingRegion: "low"}, {ID: "high", BankingRegion: "high"}, {ID: "unknown", BankingRegion: "unknown"},
	}}}
}

func competitionPlans(lowRate, highRate float64) []Plan {
	return []Plan{
		{Destination: "high", Accepted: true, Economics: Economics{GameDays: 1, NetPencePerGameDay: highRate}},
		{Destination: "low", Accepted: true, Economics: Economics{GameDays: 1, NetPencePerGameDay: lowRate}},
		{Destination: "unknown", Accepted: true, Economics: Economics{GameDays: 1, NetPencePerGameDay: highRate}},
	}
}

func TestCompetitionVoyageRanking(t *testing.T) {
	for _, test := range []struct {
		name    string
		lowRate float64
		want    []string
	}{
		{"equal earnings prefer observed low then unknown then high", 100, []string{"low", "unknown", "high"}},
		{"five percent band", 95, []string{"low", "unknown", "high"}},
		{"clearly better economics win", 94, []string{"unknown", "high", "low"}},
	} {
		t.Run(test.name, func(t *testing.T) {
			plans := competitionPlans(test.lowRate, 100)
			rankPlans(plans, competitionPlanning())
			for i, want := range test.want {
				if plans[i].Destination != want || !plans[i].Accepted {
					t.Fatalf("rank %d = %+v, want %s", i, plans[i], want)
				}
			}
			for _, plan := range plans {
				if plan.Destination == "unknown" && (plan.Competition.ForeignShips != nil || plan.Competition.PreferenceShips != 11) {
					t.Fatalf("unknown coverage became zero competition: %+v", plan.Competition)
				}
			}
		})
	}
	plans := competitionPlans(96, 100)
	plans[1].Accepted = false
	rankPlans(plans, competitionPlanning())
	if plans[len(plans)-1].Accepted {
		t.Fatal("competition promoted a rejected voyage")
	}
}

func TestCompetitionCapacityRanking(t *testing.T) {
	for _, financing := range []bool{false, true} {
		for _, lowRate := range []float64{95, 94} {
			var candidates []capacityCandidate
			for _, region := range []string{"high", "unknown", "low"} {
				rate := 100.0
				if region == "low" {
					rate = lowRate
				}
				candidates = append(candidates, capacityCandidate{decision: GrowthDecision{
					Accepted: !financing, EconomicallyViable: true, QuotedHullPence: 1000,
					IncrementalNetPencePerGameDay: rate, DepthNetPencePerGameDay: rate,
					Competition: competitionAt(competitionFixture(), region), DepthCompetition: competitionAt(competitionFixture(), region),
				}})
			}
			markCapacityCompetition(candidates)
			better := betterCapacityCandidate
			if financing {
				better = betterCapacityFinancingCandidate
			}
			best := candidates[0].decision
			for _, candidate := range candidates[1:] {
				if better(candidate.decision, best) {
					best = candidate.decision
				}
			}
			want := "low"
			if lowRate < 95 {
				want = "unknown"
			}
			if best.Competition.Region != want {
				t.Fatalf("financing %t low rate %.0f selected %s, want %s", financing, lowRate, best.Competition.Region, want)
			}
		}
	}
}

func TestCompetitionExpansionRanking(t *testing.T) {
	for _, bid := range []int64{295, 180} {
		state := expansionFactorFixture()
		// Existing region has a stressed spread of 50; 295 yields 48 after
		// gossip stress against a 173 ask, within five percent of that best.
		ask := int64(173)
		state.Catalog.Ports = append(state.Catalog.Ports,
			Port{ID: "new-yard", BankingRegion: "new", SeaZoneID: "fixture_sea", HasShipyard: true, Phase: 1},
			Port{ID: "new-demand", BankingRegion: "new", SeaZoneID: "fixture_sea", Phase: 1})
		detail := state.Ports[recoveryHomePort]
		now := time.Now().Add(-time.Second)
		detail.CoffeeHouseGossip = append(detail.CoffeeHouseGossip,
			Gossip{SubjectPortID: "new-yard", SnapshotAt: now, Commodities: []GossipCommodity{{CommodityID: "x", SnapshotAt: now, BestAskPence: &ask}}},
			Gossip{SubjectPortID: "new-demand", SnapshotAt: now, Commodities: []GossipCommodity{{CommodityID: "x", SnapshotAt: now, BestBidPence: &bid}}})
		state.Ports[recoveryHomePort] = detail
		input := expansionFactorInput(state)
		input.Competition = competitionFixture()
		input.Competition.Regions[0].ID = "new"
		input.Competition.Regions[1].ID = "caribbean"
		yard, _ := expansionAgentYard(state, input, nil, input.Now)
		want := "new-yard"
		if bid == 180 {
			want = "yard-a"
		}
		if yard.ID != want {
			t.Fatalf("bid %d selected %s, want %s", bid, yard.ID, want)
		}
	}
}

func TestCompetitionLoadFailureLeavesEconomicSelection(t *testing.T) {
	h := newRecoveryHarness(t)
	runner := h.runner(primaryTestKey)
	actionLogRefresh(t, h, runner)
	if runner.competition == nil {
		t.Fatal("successful refresh did not load competition")
	}
	first := runner.competition
	if runner.planningInput(ShipState{}).Competition != first || runner.planningInput(ShipState{}).Competition != first {
		t.Fatal("planning did not share the refresh competition frame")
	}
	core, logs := observer.New(zap.WarnLevel)
	runner.log = zap.New(core)
	ctx, cancel := context.WithCancel(h.ctx)
	cancel()
	runner.refreshCompetition(ctx)
	if runner.competition != first {
		t.Fatal("cache hit performed a failing read")
	}
	runner.competitionNextLoad = time.Time{}
	runner.refreshCompetition(ctx)
	if runner.competition != nil {
		t.Fatal("failed load retained old competition")
	}
	runner.refreshCompetition(ctx)
	if logs.Len() != 1 || competitionAt(runner.competition, "low").Coverage != "unavailable" {
		t.Fatal("failure was not explicit and rate limited")
	}
	input := competitionPlanning()
	input.Competition = runner.planningInput(ShipState{}).Competition
	got, want := competitionPlans(96, 100), competitionPlans(96, 100)
	rankPlans(got, input)
	input.Competition = nil
	rankPlans(want, input)
	if !reflect.DeepEqual(got, want) || got[0].Destination != "high" {
		t.Fatalf("load failure changed economic ranking: %+v", got)
	}
}

func TestCompetitionShortCoverageIsNeutral(t *testing.T) {
	summary := competitionFixture()
	zero := 0
	summary.Regions = append(summary.Regions, store.CompetitionRegion{
		ID: "brief", CompetitionActivity: store.CompetitionActivity{Coverage: "short_coverage", ForeignShips: &zero},
	})
	evidence := competitionAt(summary, "brief")
	if evidence.PreferenceShips != 11 || evidence.Coverage != "short_coverage" {
		t.Fatalf("brief zero observation was preferred as empty: %+v", evidence)
	}
	if unknown := competitionAt(summary, "unknown"); unknown.PreferenceShips != 11 {
		t.Fatalf("brief coverage biased the neutral midpoint: %+v", unknown)
	}
}

func TestCompetitionCoverageRestartsAfterGap(t *testing.T) {
	now := time.Now()
	prior := map[string]competitionCoverage{"region": {Since: now.Add(-store.CompetitionWindow), Through: now.Add(-15 * time.Second)}}
	runner := &Runner{
		snapshot: map[string]json.RawMessage{"_competition_coverage": encode(prior)},
		meta:     map[string]DatasetMeta{"ports/quay": {UpdatedAt: now, Complete: true}},
		stateCache: accountState{
			Catalog: Catalog{Ports: []Port{{ID: "quay", BankingRegion: "region"}}},
			Ports:   map[string]PortDetail{"quay": {Visibility: "quay"}},
		},
	}
	runner.observeCompetitionCoverage()
	if got := runner.competitionCoverage["region"]; got.Through.Sub(got.Since) != store.CompetitionWindow {
		t.Fatalf("persisted continuous coverage was lost: %+v", got)
	}
	runner.competitionCoverage["region"] = competitionCoverage{Since: now.Add(-store.CompetitionWindow), Through: now.Add(-3 * time.Minute)}
	runner.observeCompetitionCoverage()
	if got := runner.competitionCoverage["region"]; !got.Since.Equal(now) {
		t.Fatalf("polling gap did not restart coverage: %+v", got)
	}
	runner.stateCache.Ports["quay"] = PortDetail{Visibility: "public"}
	runner.observeCompetitionCoverage()
	if len(runner.competitionCoverage) != 0 {
		t.Fatal("lost quay visibility retained continuous coverage")
	}
}

func TestCompetitionDoesNotChangeDecisionFingerprints(t *testing.T) {
	low := competitionAt(competitionFixture(), "low")
	high := competitionAt(competitionFixture(), "high")
	for _, pair := range [][2]any{
		{Plan{Competition: low}, Plan{Competition: high}},
		{GrowthDecision{Competition: low, DepthCompetition: low}, GrowthDecision{Competition: high, DepthCompetition: high}},
		{map[string]any{"accepted": true, "competition": low}, map[string]any{"accepted": true, "competition": high}},
	} {
		before := encode(pair[0])
		if !reflect.DeepEqual(encode(competitionFingerprintDetails(pair[0])), encode(competitionFingerprintDetails(pair[1]))) {
			t.Fatalf("traffic changed decision fingerprint for %T", pair[0])
		}
		if !reflect.DeepEqual(before, encode(pair[0])) {
			t.Fatal("fingerprinting removed stored evidence")
		}
	}
}

func TestCompetitionCapacityEarningTiePreservesStoresAndAdmission(t *testing.T) {
	expensive := Plan{Destination: "low", Economics: Economics{
		GameDays: 1, ExpectedNetPence: 20000, StoreCashPence: 10000,
	}, Competition: competitionAt(competitionFixture(), "low")}
	cheap := expensive
	cheap.Destination, cheap.Competition = "high", competitionAt(competitionFixture(), "high")
	cheap.Economics.StoreCashPence = 100
	if !betterCapacityEarningPlan(cheap, expensive) || betterCapacityEarningPlan(expensive, cheap) {
		t.Fatal("competition overrode lower store capital at an equal earning rate")
	}
	input := capacityGrowthAcceptanceInput()
	input.CandidateRatePencePerGameDay = capacityVoyageRate(cheap.Economics)
	input.InitialStoresPence = cheap.Economics.StoreCashPence
	input.MaxPaybackDays = 1000
	base := EvaluateCapacityGrowth(input)
	capital := growthCashTotal(base.QuotedHullPence, base.SlippageReservePence, base.CannonsPence, base.InitialStoresPence)
	input.SurplusPence = capital + base.WorkingCapitalPence
	want := EvaluateCapacityGrowth(input)
	for _, lowerCompetition := range []bool{false, true} {
		candidate := expensive
		if !lowerCompetition {
			candidate.Competition = cheap.Competition
		}
		selected := cheap
		if betterCapacityEarningPlan(candidate, selected) {
			selected = candidate
		}
		input.InitialStoresPence = max(selected.Economics.StoreCashPence, selected.Economics.ProvisionsPence)
		got := EvaluateCapacityGrowth(input)
		if !got.Accepted || !reflect.DeepEqual(got, want) {
			t.Fatalf("competition changed exact-capital admission: %+v, want %+v", got, want)
		}
	}
	expensive.Economics.StoreCashPence = cheap.Economics.StoreCashPence
	if !betterCapacityEarningPlan(expensive, cheap) {
		t.Fatal("equal stores and rate did not use competition")
	}
}

func TestCompetitionGrowthPackageEconomicTies(t *testing.T) {
	high := growthPackage{port: Port{ID: "a"}, committedPence: 10000, decision: GrowthDecision{
		Accepted: true, InitialStoresPence: 100, IncrementalNetPencePerGameDay: 1000,
		Competition: competitionAt(competitionFixture(), "high"),
	}}
	low := high
	low.port.ID, low.decision.Competition = "z", competitionAt(competitionFixture(), "low")
	if !betterGrowthPackage(low, high) || betterGrowthPackage(high, low) {
		t.Fatal("equal economic packages did not prefer lower competition")
	}
	low.decision.InitialStoresPence += 100
	low.committedPence += 100
	if betterGrowthPackage(low, high) {
		t.Fatal("lower competition overrode extra store capital")
	}
	low.committedPence = high.committedPence / 2
	low.decision.IncrementalNetPencePerGameDay /= 2
	if betterGrowthPackage(low, high) {
		t.Fatal("competition overrode daily net at an equal capital return")
	}
}

func TestCompetitionCapacityAtYardAdmissionUnchanged(t *testing.T) {
	h, runner := capacityExecutionFixture(t, false)
	port, _ := findPort(runner.stateCache.Catalog, recoveryHomePort)
	hull, _ := findHull(runner.stateCache.Catalog, recoveryHull)
	destination, _ := findPort(runner.stateCache.Catalog, recoveryFarPort)
	// Duplicate a real executable earning lane to exercise equal-rate plans
	// through capacityAtYard, not just the pure earning-plan comparator.
	copyID := "competition-copy"
	copyPort := destination
	copyPort.ID = copyID
	runner.stateCache.Catalog.Ports = append(runner.stateCache.Catalog.Ports, copyPort)
	runner.stateCache.Ports[copyID] = runner.stateCache.Ports[destination.ID]
	market := runner.stateCache.Markets[destination.ID]
	market.PortID = copyID
	runner.stateCache.Markets[copyID] = market
	runner.meta["ports/"+copyID] = runner.meta["ports/"+destination.ID]
	runner.meta["books/"+copyID] = runner.meta["books/"+destination.ID]
	for _, pair := range [][2]string{{port.ID, destination.ID}, {destination.ID, port.ID}} {
		key := routeKey(pair[0], pair[1], hull.ID)
		route := runner.stateCache.Routes[key]
		if route.FromPort == destination.ID {
			route.FromPort = copyID
		}
		if route.ToPort == destination.ID {
			route.ToPort = copyID
		}
		newKey := routeKey(route.FromPort, route.ToPort, hull.ID)
		runner.stateCache.Routes[newKey] = route
		runner.meta["routes/"+newKey] = runner.meta["routes/"+key]
	}
	evidence := &growthEvidence{workingByRegion: make(map[string]int64)}
	if reason, err := runner.growthCashFunding(h.ctx, evidence); err != nil || reason != "" {
		t.Fatalf("capacity funding: %s, %v", reason, err)
	}
	input := capacityGrowthAcceptanceInput()
	input.LotCashPence, input.CandidateRatePencePerGameDay = 0, 0
	input.SurplusPence, input.IncrementalDailyUpkeepPence = evidence.available, 0
	quote, err := runner.growthQuote(port, hull)
	if err != nil {
		t.Fatal(err)
	}
	runner.competition = nil
	planning, err := runner.committedPlanningInputExcept(h.ctx, growthCandidate(port, hull), "__growth")
	if err != nil {
		t.Fatal(err)
	}
	planning.FleetDailyUpkeepPence = 0
	rates := make(map[string]float64)
	for _, plan := range PlanVoyages(planning) {
		if plan.Accepted && plan.ContractID == "" && len(plan.FreightIDs) == 0 {
			rates[plan.Destination] = max(rates[plan.Destination], capacityVoyageRate(plan.Economics))
		}
	}
	if rates[destination.ID] <= 0 || rates[destination.ID] != rates[copyID] {
		t.Fatalf("fixture lacks equal-rate earning lanes: %v", rates)
	}
	want, required, err := runner.capacityAtYard(h.ctx, port, hull, quote, input, evidence)
	if err != nil || !want.Accepted {
		t.Fatalf("baseline admission: %+v, %v", want, err)
	}
	summary := competitionFixture()
	summary.Regions[0].ID = destination.BankingRegion
	runner.competition = summary
	got, gotRequired, err := runner.capacityAtYard(h.ctx, port, hull, quote, input, evidence)
	got.Competition, got.DepthCompetition = want.Competition, want.DepthCompetition
	if err != nil || !reflect.DeepEqual(got, want) || !reflect.DeepEqual(required, gotRequired) {
		t.Fatalf("competition changed yard admission, stores or funding: %+v vs %+v, %v", got, want, err)
	}
}
