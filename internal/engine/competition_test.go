package engine

import (
	"context"
	"encoding/json"
	"fmt"
	"math"
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

func TestRankContinuationCachesQuayForecastAndLeavesInputUntouched(t *testing.T) {
	in := moneyRankInput(t, time.Now().UTC())
	presence, dark := moneyRankAlternatives(t, in)
	second := presence
	second.Commodity = "other-cargo"
	before := encode(in)
	plans := []Plan{presence, second, dark}
	rankPlans(plans, in)
	if plans[0].Continuation == nil || plans[0].Continuation != plans[1].Continuation {
		t.Fatalf("same-quay alternatives did not share the planning-pass forecast: %+v", plans)
	}
	if !reflect.DeepEqual(before, encode(in)) {
		t.Fatal("onward planning mutated observed ship, market or reservation state")
	}
	// A later pass must not reuse a continuation whose fixed freight vanished.
	in.Contracts = nil
	rankPlans(plans, in)
	for _, plan := range plans {
		if plan.Destination == "hamburg" && (plan.Continuation == nil || plan.Continuation.NetPence >= 0) {
			t.Fatalf("a prior pass's freight survived its removal: %+v", plan)
		}
	}
}

func TestRankContinuationSearchStopsAfterThreeFirstLegs(t *testing.T) {
	in := moneyRankInput(t, time.Now().UTC())
	presence, dark := moneyRankAlternatives(t, in)
	plans := []Plan{dark, dark, dark, presence}
	rankPlans(plans, in)
	for _, plan := range plans {
		if plan.Destination == "hamburg" && (plan.Continuation == nil || plan.Continuation.NetPence >= 0) {
			t.Fatalf("fourth outward candidate triggered an unbounded onward search: %+v", plan)
		}
	}
}

func TestRankInnerSearchReturnsAtMostThreeAdmittedLanes(t *testing.T) {
	in := moneyRankInput(t, time.Now().UTC())
	freight := in.Contracts[0]
	in.Contracts = nil
	for i := range 6 {
		candidate := freight
		candidate.ID = fmt.Sprintf("onward-freight-%d", i)
		in.Contracts = append(in.Contracts, candidate)
	}
	in.Ship.Ship.CurrentPortID = "hamburg"
	in.singleLegOnly = true
	plans := PlanVoyages(in)
	if len(plans) != 3 {
		t.Fatalf("bounded inner search returned %d lanes, want three", len(plans))
	}
	for _, plan := range plans {
		if !plan.Accepted || plan.Continuation != nil || plan.Economics.ExpectedNetPence != 500 {
			t.Fatalf("inner search skipped admission or recursively ranked another continuation: %+v", plan)
		}
	}
}

func TestRankInnerSizingCanRejectOneTonAndAdmitProfitableLargerLot(t *testing.T) {
	in := moneyInput(t, time.Now().UTC(), "canvas_sails", 100, 200, 10, 30)
	origin := in.Ports["amsterdam"]
	origin.BerthDuesPerDayPence = 800
	in.Ports["amsterdam"] = origin
	in.singleLegOnly = true
	plans := PlanVoyages(in)
	if len(plans) == 0 || !plans[0].Accepted || plans[0].Quantity != 10 || plans[0].Economics.ExpectedNetPence != 790 {
		t.Fatalf("one-ton shortlist quote replaced proper whole-ton admission: %+v", plans)
	}
}

func TestRankOnwardDeadlineIncludesFirstLegDuration(t *testing.T) {
	in := moneyRankInput(t, time.Now().UTC())
	deadline := in.Now.Add(gameDuration(1.25, in.Clock.Calendar.TimeScale) - time.Second)
	in.Contracts[0].ExpiresAt = &deadline
	presence, _ := moneyRankAlternatives(t, in)
	if presence.Continuation == nil || presence.Continuation.NetPence >= 0 {
		t.Fatalf("freight expiring before outward arrival financed a continuation: %+v", presence)
	}
}

func TestCalibrationRegimeBoundarySurvivesRunnerRestart(t *testing.T) {
	h := newRecoveryHarness(t)
	first := h.runner(primaryTestKey)
	if err := first.refreshRevenueCalibration(h.ctx); err != nil {
		t.Fatal(err)
	}
	since := decode[time.Time](first.snapshot, "_revenue_calibration_since")
	if since.IsZero() {
		t.Fatal("calibration did not establish its deployment boundary")
	}
	second := h.runner(primaryTestKey)
	snapshot, err := h.db.Snapshot(h.ctx, h.account.ID)
	if err != nil {
		t.Fatal(err)
	}
	second.snapshot = snapshot
	if err := second.refreshRevenueCalibration(h.ctx); err != nil {
		t.Fatal(err)
	}
	if got := decode[time.Time](second.snapshot, "_revenue_calibration_since"); !got.Equal(since) {
		t.Fatalf("restart reset calibration boundary from %v to %v", since, got)
	}
	if len(decode[map[string]int64](second.snapshot, "_revenue_calibration")) != 0 {
		t.Fatal("new pricing regime inherited pre-deployment calibration")
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
		stateCache: AccountState{
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
	observations := runner.planningInput(ShipState{})
	seedZeroHazardQuotes(&observations)
	runner.stateCache.HazardQuotes = observations.HazardQuotes
	runner.snapshot[hazardQuoteSnapshotKey] = encode(observations.HazardQuotes)
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

func TestMoneyCalibrationFixedFreightRewardIsNotCargoRevenue(t *testing.T) {
	for _, kind := range []string{"spot+freight", "charter+freight", "freight"} {
		t.Run(kind, func(t *testing.T) {
			in := moneyInput(t, time.Now().UTC(), "canvas_sails", 100, 1000, 2, 30)
			in.RevenueCalibration = map[string]int64{"depth": 9000, "contract": 9000}
			in.marketEventsInvalid = true
			in.Contracts = []Contract{
				{ID: "fixed-reward", ContractType: "freight", Status: "open", IssuerCompanyID: "other",
					OriginPortID: "amsterdam", DestPortID: "hamburg", CommodityID: "grain", QuantityRequired: 1, RewardPence: 1000},
				{ID: "calibrated-charter", ContractType: "charter", Status: "open", OriginPortID: "hamburg",
					CommodityID: "canvas_sails", RemainingQuantity: 1, RewardPerTonPence: new(int64(1000))},
			}
			for _, plan := range PlanVoyages(in) {
				if plan.Kind != kind || kind != "freight" && plan.Commodity != "canvas_sails" {
					continue
				}
				wantRevenue, wantRaw, wantStress := int64(1900), int64(2000), int64(0)
				if kind == "spot+freight" {
					wantStress = 225
				}
				if kind == "freight" {
					wantRevenue, wantRaw = 1000, 1000
				}
				if !plan.Accepted || plan.Economics.RevenuePence != wantRevenue ||
					plan.Economics.UncalibratedRevenuePence == nil || *plan.Economics.UncalibratedRevenuePence != wantRaw ||
					plan.Economics.PriceUncertaintyStressPence != wantStress {
					t.Fatalf("cargo calibration/stress changed the fixed freight reward: %+v", plan)
				}
				return
			}
			t.Fatalf("missing %s fixture quote", kind)
		})
	}
}

func TestMoneyRankRejectedStressLanesCannotCrowdOutOnwardFreight(t *testing.T) {
	in := moneyInput(t, time.Now().UTC(), "canvas_sails", 800, 1000, 10, 30)
	for i := range 2 {
		commodity := fmt.Sprintf("stressed-cargo-%d", i)
		in.Catalog.Ports[1].Imports = append(in.Catalog.Ports[1].Imports, commodity)
		for _, port := range []string{"amsterdam", "hamburg"} {
			extra := moneyMarket(t, port, commodity, 800, 1000, 1000)
			book := in.Markets[port]
			book.Books = append(book.Books, extra.Books[0])
			in.Markets[port] = book
		}
	}
	in.Contracts = []Contract{{
		ID: "admissible-onward-freight", ContractType: "freight", Status: "open", IssuerCompanyID: "other",
		OriginPortID: "amsterdam", DestPortID: "hamburg", CommodityID: "grain", QuantityRequired: 10, RewardPence: 100,
	}}
	in.singleLegOnly, in.marketEventsInvalid = true, true
	plans := PlanVoyages(in)
	if len(plans) == 0 || !plans[0].Accepted || plans[0].ContractID != "admissible-onward-freight" {
		t.Fatalf("rejected event-stressed gross margins displaced admissible onward work: %+v", plans)
	}
}

func TestMoneyRankBallastAndRepeatUseIdenticalReturnCosts(t *testing.T) {
	in := moneyInput(t, time.Now().UTC(), "canvas_sails", 100, 1000, 1, 30)
	in.Catalog.ShipClasses[0].CrewRequired = 8
	in.Ship.Stores.ProvisionsTons = 20
	origin := in.Ports["amsterdam"]
	origin.QueuedShips, origin.BerthDuesPerDayPence = 3, 79
	in.Ports["amsterdam"] = origin
	plan := moneySpot(t, in, "canvas_sails")
	hull, _ := findHull(in.Catalog, in.Ship.Ship.ShipClass)
	next := ballastContinuation(in, plan, hull)
	if !plan.Accepted || next == nil || plan.Economics.BallastRepeatGameDays <= 0 ||
		math.Abs(plan.Economics.BallastRepeatGameDays-plan.Economics.GameDays-next.GameDays) > 1e-12 ||
		plan.Economics.BallastRepeatNetPence != plan.Economics.ExpectedNetPence+next.NetPence {
		t.Fatalf("rank ballast and repeat forecast disagree on return time or expense: %+v, %+v", plan, next)
	}
}

func TestMoneyRankCompetitionStillAppliesToNegativeCombinedScores(t *testing.T) {
	in := moneyInput(t, time.Now().UTC(), "canvas_sails", 100, 1000, 1, 30)
	high := in.Catalog.Ports[1]
	high.BankingRegion = "high"
	in.Catalog.Ports[1] = high
	low := high
	low.ID, low.BankingRegion = "low-port", "low"
	in.Catalog.Ports = append(in.Catalog.Ports, low)
	in.Ports[low.ID] = in.Ports["hamburg"]
	back := in.Routes[routeKey("hamburg", "amsterdam", "hoy")]
	back.FromPort = low.ID
	key := routeKey(low.ID, "amsterdam", "hoy")
	in.Routes[key], in.Metadata["routes/"+key] = back, sourcingFreshMeta(in.Now)
	origin := in.Ports["amsterdam"]
	origin.BerthDuesPerDayPence = 4000
	in.Ports["amsterdam"] = origin
	in.Competition = competitionFixture()
	plans := []Plan{
		{Kind: "spot", Accepted: true, Origin: "amsterdam", Destination: "hamburg", Economics: Economics{ExpectedNetPence: 100, GameDays: 1, NetPencePerGameDay: 100}},
		{Kind: "spot", Accepted: true, Origin: "amsterdam", Destination: low.ID, Economics: Economics{ExpectedNetPence: 99, GameDays: 1, NetPencePerGameDay: 99}},
	}
	delete(in.Metadata, "books/hamburg")
	rankPlans(plans, in)
	if plans[0].Destination != low.ID || !plans[0].competitionComparable || planRankScore(plans[0].Economics) >= 0 {
		t.Fatalf("nonpositive two-leg scores disabled the competition tolerance: %+v", plans)
	}
}

func TestMoneyRankFundingKeepsLiveAndOfflineScoresInSync(t *testing.T) {
	for _, continued := range []bool{false, true} {
		t.Run(fmt.Sprint(continued), func(t *testing.T) {
			plan := Plan{Kind: "spot", Accepted: true, Economics: Economics{
				PurchasePence: 100, RevenuePence: 300, ExpectedNetPence: 200, StressNetPence: 200,
				GameDays: 1, NetPencePerGameDay: 200, RankScoreSet: continued, RankScorePencePerGameDay: 12345,
				BallastRepeatGameDays: 2, BallastRepeatNetPence: 100,
			}}
			if continued {
				plan.Continuation = &Continuation{Destination: "return", NetPence: 100, GameDays: 1}
			}
			got := addFundingFees(plan, 10)
			want := 190.0
			if continued {
				want = 145
			}
			if !got.Accepted || planRankScore(got.Economics) != want || got.Economics.RankScoreSet != continued {
				t.Fatalf("live funding left a stale continuation or single-leg score: %+v", got)
			}
			if !continued && (got.Economics.RankScorePencePerGameDay != 0 || capacityVoyageRate(got.Economics) != 45) {
				t.Fatalf("funding retained continuation-rank metadata or lost the costed return: %+v", got)
			}
			in := moneyInput(t, time.Now().UTC(), "canvas_sails", 100, 200, 1, 30)
			got.Origin, got.Destination, got.Commodity, got.Quantity, got.BuyLimitPence = "amsterdam", "hamburg", "canvas_sails", 1, 100
			offline, _ := PlanFunding(in, got)
			want = 200
			if continued {
				want = 150
			}
			if !offline.Accepted || planRankScore(offline.Economics) != want || offline.Economics.RankScoreSet != continued {
				t.Fatalf("offline re-quote failed to refund fees and refresh the score: %+v", offline)
			}
			if !continued && (offline.Economics.RankScorePencePerGameDay != 0 || capacityVoyageRate(offline.Economics) != 50) {
				t.Fatalf("offline funding retained continuation-rank metadata or lost the costed return: %+v", offline)
			}
		})
	}
}

func TestMoneyRankScoreSurvivesPlanSerialization(t *testing.T) {
	for _, score := range []float64{-100, 0, 300} {
		before := Plan{Accepted: true, Economics: Economics{
			GameDays: 1, ExpectedNetPence: 1000, NetPencePerGameDay: 1000, RankScorePencePerGameDay: score, RankScoreSet: true,
		}}
		var after Plan
		if err := json.Unmarshal(encode(before), &after); err != nil {
			t.Fatal(err)
		}
		if got := planRankScore(after.Economics); got != score || capacityVoyageRate(after.Economics) != score {
			t.Fatalf("restart replaced computed %.0f score with single-leg economics: %+v", score, after)
		}
	}
}

func TestMoneyRankAdmissionOnlyDoesNotSearchContinuations(t *testing.T) {
	in := moneyRankInput(t, time.Now().UTC())
	if !hasAcceptedVoyage(in) {
		t.Fatal("admission-only lookup lost an executable outward leg")
	}
	for _, plan := range planVoyages(in) {
		if plan.Continuation != nil || plan.Economics.RankScoreSet {
			t.Fatalf("admission-only planning computed an unnecessary onward forecast: %+v", plan)
		}
	}
}

func TestMoneyCalibrationGrowthCharterRevenueIsScaledOnce(t *testing.T) {
	h := newRecoveryHarness(t)
	runner := h.runner(primaryTestKey)
	in := moneyInput(t, time.Now().UTC(), "canvas_sails", 100, 1000, 1, 30)
	in.RevenueCalibration = map[string]int64{"contract": 9500}
	in.Contracts = []Contract{{
		ID: "growth-calibration-charter", ContractType: "charter", Status: "open", OriginPortID: "hamburg",
		CommodityID: "canvas_sails", RemainingQuantity: 1000, RewardPerTonPence: new(int64(1000)),
	}}
	hull, _ := findHull(in.Catalog, in.Ship.Ship.ShipClass)
	origin, _ := findPort(in.Catalog, "amsterdam")
	plan := evaluatePlan(in, hull, origin, Plan{
		Kind: "charter", Origin: "amsterdam", Destination: "hamburg", Commodity: "canvas_sails", ContractID: in.Contracts[0].ID, Quantity: 1,
	}, 1000, 0)
	if !plan.Accepted || plan.Economics.RevenuePence != 950 {
		t.Fatalf("fixture lacks a once-calibrated charter: %+v", plan)
	}
	evidence := &growthEvidence{asOf: in.Now, markets: in.Markets, lanes: map[string]store.GrowthHistory{}}
	if _, _, err := runner.evaluateGrowthCandidate(h.ctx, origin, hull, &plan, in, YardQuote{}, 0, evidence); err != nil {
		t.Fatal(err)
	}
	if !plan.Accepted || plan.Economics.RevenuePence != 950 {
		t.Fatalf("marginal growth reprice calibrated the charter twice: %+v", plan)
	}
}

func TestMoneyRankOnwardShortlistUsesNetCostsForCargoAndFreight(t *testing.T) {
	in := moneyInput(t, time.Now().UTC(), "canvas_sails", 1000, 1200, 10, 1000)
	origin := in.Ports["amsterdam"]
	origin.BerthDuesPerDayPence = 2000
	in.Ports["amsterdam"] = origin
	for i := range 2 {
		commodity := fmt.Sprintf("costed-cargo-%d", i)
		in.Catalog.Ports[1].Imports = append(in.Catalog.Ports[1].Imports, commodity)
		for _, port := range []string{"amsterdam", "hamburg"} {
			extra := moneyMarket(t, port, commodity, 1000, 1200, 1000)
			book := in.Markets[port]
			book.Books = append(book.Books, extra.Books[0])
			in.Markets[port] = book
		}
	}
	in.Contracts = []Contract{{
		ID: "higher-net-freight", ContractType: "freight", Status: "open", IssuerCompanyID: "other",
		OriginPortID: "amsterdam", DestPortID: "hamburg", CommodityID: "grain", QuantityRequired: 10, RewardPence: 2200,
	}}
	in.singleLegOnly = true
	plans := PlanVoyages(in)
	if len(plans) == 0 || !plans[0].Accepted || plans[0].ContractID != "higher-net-freight" {
		t.Fatalf("optimistic cargo gross margins crowded out higher-net freight: %+v", plans)
	}
}

func TestMoneyCalibrationAllFreightRewardsSurviveIncomeRepricing(t *testing.T) {
	for _, kind := range []string{"spot+freight", "charter+freight"} {
		t.Run(kind, func(t *testing.T) {
			in := moneyInput(t, time.Now().UTC(), "canvas_sails", 100, 1000, 3, 30)
			in.RevenueCalibration = map[string]int64{"depth": 9000, "contract": 9000}
			in.marketEventsInvalid = true
			in.Contracts = []Contract{
				{ID: "first-freight", ContractType: "freight", Status: "open", IssuerCompanyID: "other",
					OriginPortID: "amsterdam", DestPortID: "hamburg", CommodityID: "grain", QuantityRequired: 1, RewardPence: 1000},
				{ID: "second-freight", ContractType: "freight", Status: "open", IssuerCompanyID: "other",
					OriginPortID: "amsterdam", DestPortID: "hamburg", CommodityID: "grain", QuantityRequired: 1, RewardPence: 2000},
				{ID: "cargo-charter", ContractType: "charter", Status: "open", OriginPortID: "hamburg",
					CommodityID: "canvas_sails", RemainingQuantity: 1, RewardPerTonPence: new(int64(1000))},
			}
			plan := priceIncomePlan(in, Plan{
				Kind: kind, ShipID: in.Ship.Ship.ID, Origin: "amsterdam", Destination: "hamburg",
				Commodity: "canvas_sails", Quantity: 1, ContractID: "cargo-charter",
				FreightIDs: []string{"first-freight", "second-freight"},
			}, 0)
			wantStress := int64(0)
			if kind == "spot+freight" {
				wantStress = 225
			}
			if !plan.Accepted || plan.Economics.RevenuePence != 3900 || plan.Economics.RevenueCalibrationBPS != 9000 ||
				plan.Economics.UncalibratedRevenuePence == nil || *plan.Economics.UncalibratedRevenuePence != 4000 ||
				plan.Economics.RevenueCalibrationReason != "" || plan.Economics.PriceUncertaintyStressPence != wantStress {
				t.Fatalf("income reprice calibrated a fixed reward or stressed the freight receipts: %+v", plan)
			}
		})
	}
}

func TestMoneyCalibrationMissingFreightTermsPreserveRevenueAndReason(t *testing.T) {
	for _, kind := range []string{"spot+freight", "charter+freight"} {
		t.Run(kind, func(t *testing.T) {
			in := moneyInput(t, time.Now().UTC(), "canvas_sails", 100, 1000, 3, 30)
			in.RevenueCalibration = map[string]int64{"depth": 9000, "contract": 9000}
			in.marketEventsInvalid = true
			in.Contracts = []Contract{{
				ID: "known-freight", ContractType: "freight", Status: "open", IssuerCompanyID: "other",
				OriginPortID: "amsterdam", DestPortID: "hamburg", CommodityID: "grain", QuantityRequired: 1, RewardPence: 1000,
			}}
			hull, _ := findHull(in.Catalog, in.Ship.Ship.ShipClass)
			origin, _ := findPort(in.Catalog, "amsterdam")
			revenue, wantStress := int64(4000), int64(0)
			if kind == "spot+freight" {
				revenue, wantStress = 3000, 250
			}
			plan := pricePlan(in, hull, origin, Plan{
				Kind: kind, ShipID: in.Ship.Ship.ID, Origin: "amsterdam", Destination: "hamburg",
				Commodity: "canvas_sails", Quantity: 1, FreightIDs: []string{"known-freight", "missing-freight"},
			}, revenue, 2)
			if !plan.Accepted || plan.Economics.RevenuePence != 4000 || plan.Economics.RevenueCalibrationBPS != 10000 ||
				plan.Economics.UncalibratedRevenuePence == nil || *plan.Economics.UncalibratedRevenuePence != 4000 ||
				plan.Economics.RevenueCalibrationReason == "" || plan.Economics.PriceUncertaintyStressPence != wantStress {
				t.Fatalf("missing fixed terms silently calibrated an unknown receipt split: %+v", plan)
			}
			var restored Plan
			if err := json.Unmarshal(encode(plan), &restored); err != nil {
				t.Fatal(err)
			}
			if restored.Economics.RevenueCalibrationReason != plan.Economics.RevenueCalibrationReason ||
				restored.Economics.RevenueCalibrationBPS != 10000 || restored.Economics.RevenuePence != 4000 {
				t.Fatalf("persisted plan lost its explicit calibration hold: %+v", restored)
			}
		})
	}
}
