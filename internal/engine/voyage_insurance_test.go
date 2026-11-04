package engine

import (
	"encoding/json"
	"io"
	"math"
	"net/http"
	"reflect"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/DevYukine/go-tradewinds/internal/gameapi"
	"github.com/DevYukine/go-tradewinds/internal/store"
	"github.com/google/uuid"
)

func voyageCoverTestQuote() gameapi.LloydsQuoteResult {
	return gameapi.LloydsQuoteResult{ShipID: recoveryShipBlastoise, OriginPortID: recoveryHomePort, DestPortID: recoveryFarPort,
		InsuredValuePence: 1_000_000, HullValuePence: 900_000, MaxInsurablePence: 1_000_000, LloydsWrites: true,
		LloydsExpectedLossBPS: new(int64(50)), LloydsExpectedLossPence: new(int64(5_000)), LloydsPremiumBPS: new(int64(100)), LloydsPremiumPence: new(int64(10_000))}
}

func TestVoyageInsuranceOnlyOceanicAtTwiceExpectedLoss(t *testing.T) {
	hull := Hull{ID: "barque", HullCategory: "oceanic"}
	route := Route{FromPort: recoveryHomePort, ToPort: recoveryFarPort, Legs: []RouteLeg{{FromNode: "origin", ToNode: "destination", EdgeClass: "oceanic"}}}
	quote := voyageCoverTestQuote()
	if !worthwhileVoyageCover(hull, route, nil, quote, 0) {
		t.Fatal("oceanic offer exactly at twice EL was refused")
	}
	quote.LloydsPremiumPence = new(int64(10_001))
	if worthwhileVoyageCover(hull, route, nil, quote, 0) {
		t.Fatal("one penny above twice EL was purchased")
	}
	quote = voyageCoverTestQuote()
	route.Legs[0].EdgeClass = "coastal"
	if worthwhileVoyageCover(hull, route, nil, quote, 0) {
		t.Fatal("coastal hop bought voyage insurance")
	}
	zones := []Zone{{ID: "destination", ZoneClass: "oceanic"}}
	if !worthwhileVoyageCover(hull, route, zones, quote, 0) {
		t.Fatal("oceanic arrival water was mistaken for a coastal hop")
	}
	hull.HullCategory = "regional"
	if worthwhileVoyageCover(hull, route, zones, quote, 0) {
		t.Fatal("under-rated hull was admitted by buying cover")
	}
	hull.HullCategory = "oceanic"
	for _, mutate := range []func(*gameapi.LloydsQuoteResult){
		func(q *gameapi.LloydsQuoteResult) { q.LloydsExpectedLossPence = nil },
		func(q *gameapi.LloydsQuoteResult) { q.LloydsPremiumPence = nil },
		func(q *gameapi.LloydsQuoteResult) { q.LloydsWrites = false },
		func(q *gameapi.LloydsQuoteResult) { q.PricedConsortShipIDs = []string{"absent-consort"} },
		func(q *gameapi.LloydsQuoteResult) { q.InsuredValuePence-- },
	} {
		quote = voyageCoverTestQuote()
		mutate(&quote)
		if worthwhileVoyageCover(hull, route, zones, quote, 0) {
			t.Fatalf("incomplete or voidable declaration was purchased: %+v", quote)
		}
	}
	quote = voyageCoverTestQuote()
	if worthwhileVoyageCover(hull, route, zones, quote, quote.InsuredValuePence) {
		t.Fatal("fully self-insured voyage bought cover")
	}
	if !worthwhileVoyageCover(hull, route, zones, quote, quote.InsuredValuePence-1) {
		t.Fatal("one-penny unprotected exposure was treated as self-insured")
	}
}

func TestVoyageInsurancePlanningPricesFullPremiumInsteadOfCargoLoss(t *testing.T) {
	in, hull, origin, candidate := oceanicPremiumFixture(t, "oceanic", 1_000_000)
	candidate.Economics.CollateralPence = 100_000
	priced := pricePlan(in, hull, origin, candidate, 0, 0)
	insured := int64(2_000_000) + priced.Economics.PurchasePence + priced.Economics.CustomsPence
	wantPremium := ceilBPS(insured, 400)
	if priced.Economics.VoyagePremiumPence != wantPremium || priced.Economics.HazardPence != 4_000 {
		t.Fatalf("premium or separate collateral risk: %+v", priced.Economics)
	}
	wantNet := priced.Economics.RevenuePence - priced.Economics.PurchasePence - priced.Economics.CustomsPence -
		priced.Economics.ProvisionsPence - priced.Economics.BerthDuesPence - wantPremium - 4_000
	if priced.Accepted || priced.Economics.ExpectedNetPence != wantNet {
		t.Fatalf("covered cargo was double charged or admitted without its gross premium: %+v", priced)
	}
	required := cargoFundingRequired(in, priced)
	withoutCover := priced
	withoutCover.Economics.VoyagePremiumPence = 0
	baseline := cargoFundingRequired(in, withoutCover)
	if required[origin.BankingRegion]-baseline[origin.BankingRegion] != wantPremium {
		t.Fatalf("origin funding omitted premium: required=%v baseline=%v", required, baseline)
	}
	in.Balances[origin.BankingRegion] = 5_000_000
	in.pinnedHazard = &priced.Economics
	pinned := pricePlan(in, hull, origin, candidate, 0, 0)
	if pinned.Economics.VoyagePremiumPence != wantPremium || pinned.Economics.HazardPence != 4_000 {
		t.Fatalf("first-send repricing changed admitted cover: %+v", pinned.Economics)
	}
	in.pinnedHazard = &Economics{}
	optimistic := pricePlan(in, hull, origin, candidate, 0, 0)
	if optimistic.Economics.VoyagePremiumPence != 0 || optimistic.Economics.HazardPence != 0 {
		t.Fatalf("quote-refresh scheduling charged a premium: %+v", optimistic.Economics)
	}
}

func TestVoyageInsurancePlanningUsesCashAfterCargoAndStores(t *testing.T) {
	in, hull, origin, candidate := oceanicPremiumFixture(t, "oceanic", 1_000_000)
	in.Ship.Stores.ProvisionsTons = 0
	priced := pricePlan(in, hull, origin, candidate, 0, 0)
	if priced.Economics.StoreCashPence <= 0 {
		t.Fatal("fixture did not require a stores purchase")
	}
	insured := int64(2_000_000) + priced.Economics.PurchasePence + priced.Economics.CustomsPence
	lotCash := priced.Economics.PurchasePence + priced.Economics.CustomsPence + priced.Economics.StoreCashPence
	in.Balances[origin.BankingRegion] = in.ReserveByRegion[origin.BankingRegion] + lotCash + insured
	selfInsured := pricePlan(in, hull, origin, candidate, 0, 0)
	if selfInsured.Economics.VoyagePremiumPence != 0 {
		t.Fatalf("cash exactly covering post-load value bought insurance: %+v", selfInsured.Economics)
	}
	in.Balances[origin.BankingRegion]--
	covered := pricePlan(in, hull, origin, candidate, 0, 0)
	if covered.Economics.VoyagePremiumPence != ceilBPS(insured, 400) {
		t.Fatalf("one penny below post-cargo-and-stores cover omitted premium: %+v", covered.Economics)
	}
	for key, quote := range in.HazardQuotes {
		quote.LloydsWrites, quote.LloydsWritesKnown = false, true
		in.HazardQuotes[key] = quote
	}
	uninsurable := pricePlan(in, hull, origin, candidate, 0, 0)
	if !reflect.DeepEqual(uninsurable.Economics, selfInsured.Economics) {
		t.Fatalf("uninsurable voyage changed self-insured economics: uninsurable=%+v self-insured=%+v", uninsurable.Economics, selfInsured.Economics)
	}
}

func TestVoyageInsurancePremiumMaximumIsIndependentOfLossMaximum(t *testing.T) {
	in, _, _, _ := oceanicPremiumFixture(t, "oceanic", 1_000_000)
	hullKey := hazardHullKey(in.Ship.Ship.ID, "le_havre", "portsmouth")
	ladenKey := hazardLadenKey("le_havre", "portsmouth", "hoy", in.Ship.Ship.CannonsCount, in.Clock.Calendar.Month)
	hullQuote, laden := in.HazardQuotes[hullKey], in.HazardQuotes[ladenKey]
	hullQuote.ExpectedLossBPS, hullQuote.LloydsPremiumBPS = 500, new(int64(600))
	laden.ExpectedLossBPS, laden.LloydsPremiumBPS = 400, new(int64(700))
	in.HazardQuotes[hullKey], in.HazardQuotes[ladenKey] = hullQuote, laden
	route := in.Routes[routeKey("le_havre", "portsmouth", "hoy")]
	quote, source, ok := quotedLaneHazard(in, route)
	if !ok || quote.ExpectedLossBPS != 500 || *quote.LloydsPremiumBPS != 700 || source != "hull-only" {
		t.Fatalf("premium maximum followed the wrong loss quote: source=%s quote=%+v", source, quote)
	}
	hullQuote.ExpectedLossBPS, hullQuote.LloydsPremiumBPS = 350, new(int64(800))
	in.HazardQuotes[hullKey] = hullQuote
	quote, source, ok = quotedLaneHazard(in, route)
	if !ok || quote.ExpectedLossBPS != 400 || *quote.LloydsPremiumBPS != 800 || source != "laden" {
		t.Fatalf("laden loss maximum dropped hull premium maximum: source=%s quote=%+v", source, quote)
	}
}

func TestVoyageInsuranceGrowthPremiumUsesLaneProxyOrTwiceLoss(t *testing.T) {
	for _, proxyAvailable := range []bool{true, false} {
		in, hull, origin, candidate := oceanicPremiumFixture(t, "oceanic", 1_000_000)
		in.Fleet = []ShipState{in.Ship}
		in.Ship = growthCandidate(origin, hull)
		in.Ship.Stores.ProvisionsTons = 8
		in.growthHullValuePence = 3_000_000
		candidate.ShipID = in.Ship.Ship.ID
		if !proxyAvailable {
			for key, quote := range in.HazardQuotes {
				quote.LloydsPremiumBPS = nil
				in.HazardQuotes[key] = quote
			}
		}
		priced := pricePlan(in, hull, origin, candidate, 0, 0)
		bps := int64(400)
		if !proxyAvailable {
			bps = 800
		}
		insured := in.growthHullValuePence + priced.Economics.PurchasePence + priced.Economics.CustomsPence
		if priced.Economics.VoyagePremiumPence != ceilBPS(insured, bps) || priced.Economics.HazardPence != 0 {
			t.Fatalf("growth proxy available=%v left a premium-free or double-charged oceanic plan: %+v", proxyAvailable, priced)
		}
	}
}

func TestVoyageInsuranceIncomeRepricingPinsPremiumAcrossCashChanges(t *testing.T) {
	for _, initialCash := range []int64{1_000_000, 5_000_000} {
		in, hull, origin, candidate := oceanicPremiumFixture(t, "oceanic", initialCash)
		candidate.Kind, candidate.ContractID, candidate.PriceSource = "charter", "cover-charter", "live contract terms"
		reward := int64(300_000)
		in.Contracts = []Contract{{ID: candidate.ContractID, ContractType: "charter", Status: "open",
			OriginPortID: candidate.Destination, CommodityID: candidate.Commodity, RemainingQuantity: 100,
			RewardPerTonPence: &reward}}
		priced := pricePlan(in, hull, origin, candidate, candidate.Quantity*reward, 0)
		if !priced.Accepted {
			t.Fatalf("profitable insured fixture was rejected: %+v", priced)
		}
		in.Balances[origin.BankingRegion] = 6_000_000 - initialCash
		repriced := priceIncomePlan(in, priced, 0)
		if !repriced.Accepted || repriced.Economics.VoyagePremiumPence != priced.Economics.VoyagePremiumPence ||
			repriced.Economics.HazardPence != priced.Economics.HazardPence {
			t.Fatalf("cash change flipped fixed first-send insurance: admitted=%+v repriced=%+v", priced, repriced)
		}
	}
}

func TestVoyageInsuranceOnwardProjectionPricesCargoAndHullCoverSwitch(t *testing.T) {
	for _, initiallyCovered := range []bool{true, false} {
		in, hull, origin, candidate := oceanicPremiumFixture(t, "oceanic", 1_000_000)
		if !initiallyCovered {
			one := pricePlan(in, hull, origin, Plan{Kind: "charter", ShipID: candidate.ShipID,
				Origin: candidate.Origin, Destination: candidate.Destination, Commodity: candidate.Commodity, Quantity: 1}, 300_000, 0)
			cargo := one.Economics.PurchasePence + one.Economics.CustomsPence
			in.Balances[origin.BankingRegion] = in.ReserveByRegion[origin.BankingRegion] + 2_000_000 + 2*cargo + one.Economics.StoreCashPence
		}
		in.singleLegOnly = true
		candidate.Kind, candidate.PriceSource = "charter", "live contract terms"
		priced := sizePlan(in, hull, origin, candidate, 4, 300_000, 0, 0)
		projectedCargo := 4 * (priced.Economics.PurchasePence + priced.Economics.CustomsPence)
		projectedPremium := ceilBPS(2_000_000+projectedCargo, 400)
		want := float64(priced.Economics.ExpectedNetPence+
			3*(priced.Economics.RevenuePence-priced.Economics.PurchasePence-priced.Economics.CustomsPence)+
			priced.Economics.HazardPence+priced.Economics.VoyagePremiumPence-projectedPremium) / priced.Economics.GameDays
		if !priced.Accepted || (priced.Economics.VoyagePremiumPence > 0) != initiallyCovered || priced.onwardPriority != want {
			t.Fatalf("onward cover initially=%v: want=%g plan=%+v priority=%g", initiallyCovered, want, priced, priced.onwardPriority)
		}
	}
}

func TestVoyageInsuranceReservesPlannedOrActualPremiumOnce(t *testing.T) {
	in, hull, origin, candidate := oceanicPremiumFixture(t, "oceanic", 1_000_000)
	plan := pricePlan(in, hull, origin, candidate, 0, 0)
	runner := &Runner{stateCache: AccountState{Catalog: in.Catalog, Markets: in.Markets, Ports: in.Ports}}
	mission := &store.Mission{ShipID: in.Ship.Ship.ID, Phase: "buy", Destination: plan.Destination}
	data := missionData{Plan: plan}
	data.Plan.Economics.VoyagePremiumPence = 0
	baseline := make(map[string]int64)
	runner.reserveMissionCash(baseline, mission, &data, in.Ship)
	data.Plan.Economics.VoyagePremiumPence = 10_000
	planned := make(map[string]int64)
	runner.reserveMissionCash(planned, mission, &data, in.Ship)
	if planned[origin.BankingRegion]-baseline[origin.BankingRegion] != 10_000 {
		t.Fatalf("pending cargo did not protect its planned premium: planned=%v baseline=%v", planned, baseline)
	}
	input := PlanningInput{ReserveByRegion: make(map[string]int64)}
	runner.voyagePremiumReserves(&input, mission, &data, in.Ship)
	if !reflect.DeepEqual(input.ReserveByRegion, baseline) {
		t.Fatalf("premium eligibility reserved its own premium twice: actual=%v want=%v", input.ReserveByRegion, baseline)
	}
	data.VoyageInsurance = []voyageInsuranceIntent{{
		Request: gameapi.CreateInsurancePolicyRequest{DestPortID: plan.Destination},
		Quote:   gameapi.LloydsQuoteResult{OriginPortID: plan.Origin, LloydsPremiumPence: new(int64(10_100))},
	}}
	offered := make(map[string]int64)
	runner.reserveMissionCash(offered, mission, &data, in.Ship)
	if offered[origin.BankingRegion]-baseline[origin.BankingRegion] != 10_100 {
		t.Fatalf("actual offer did not replace the planned reservation: offered=%v baseline=%v", offered, baseline)
	}
	data.VoyageInsurance[0].PolicyID = "paid-policy"
	paid := make(map[string]int64)
	runner.reserveMissionCash(paid, mission, &data, in.Ship)
	if !reflect.DeepEqual(paid, baseline) {
		t.Fatalf("paid policy retained an unpaid premium reserve: paid=%v baseline=%v", paid, baseline)
	}
}

func TestVoyageInsuranceLadenWithoutPremiumPreservesHullOffer(t *testing.T) {
	in, hull, origin, candidate := oceanicPremiumFixture(t, "oceanic", 1_000_000)
	ladenKey := hazardLadenKey(candidate.Origin, candidate.Destination, hull.ID, in.Ship.Ship.CannonsCount, in.Clock.Calendar.Month)
	laden := in.HazardQuotes[ladenKey]
	laden.LloydsPremiumBPS, laden.HullValuePence, laden.LloydsWrites = nil, 0, false
	in.HazardQuotes[ladenKey] = laden
	priced := pricePlan(in, hull, origin, candidate, 0, 0)
	insured := int64(2_000_000) + priced.Economics.PurchasePence + priced.Economics.CustomsPence
	if priced.Economics.HazardBPS != 400 || priced.Economics.VoyagePremiumPence != ceilBPS(insured, 350) {
		t.Fatalf("optional laden terms erased the hull offer: %+v", priced)
	}
}

func TestVoyageInsuranceUnknownTermsUseConservativePremiumOrReject(t *testing.T) {
	for _, terms := range []string{"legacy", "missing premium", "missing value", "no value evidence", "authoritative refusal"} {
		t.Run(terms, func(t *testing.T) {
			in, hull, origin, candidate := oceanicPremiumFixture(t, "oceanic", 1_000_000)
			for key, quote := range in.HazardQuotes {
				switch terms {
				case "legacy":
					quote.LloydsPremiumBPS, quote.HullValuePence, quote.LloydsWrites = nil, 0, false
				case "missing premium":
					quote.LloydsPremiumBPS = nil
				case "missing value":
					quote.HullValuePence = 0
				case "no value evidence":
					quote.HullValuePence, quote.MaxInsurablePence = 0, 0
				case "authoritative refusal":
					quote.LloydsPremiumBPS, quote.HullValuePence = nil, 0
					quote.LloydsWrites, quote.LloydsWritesKnown = false, true
				}
				in.HazardQuotes[key] = quote
			}
			priced := pricePlan(in, hull, origin, candidate, 0, 0)
			if terms == "no value evidence" {
				if priced.Accepted || !strings.Contains(priced.Reason, "no checked hull value") {
					t.Fatalf("unknown hull value admitted a free oceanic passage: %+v", priced)
				}
				return
			}
			if terms == "authoritative refusal" {
				if !priced.Accepted || priced.Economics.VoyagePremiumPence != 0 {
					t.Fatalf("explicit no-write did not retain uninsured economics: %+v", priced)
				}
				return
			}
			bps := int64(400)
			if terms == "legacy" || terms == "missing premium" {
				bps = 800
			}
			insured := int64(2_000_000) + priced.Economics.PurchasePence + priced.Economics.CustomsPence
			if priced.Accepted || priced.Economics.VoyagePremiumPence != ceilBPS(insured, bps) {
				t.Fatalf("unknown insurance terms escaped admission cost: %+v", priced)
			}
		})
	}
}

func TestVoyageInsuranceOmittedAndNullWritesWithPremiumAreNotRefusals(t *testing.T) {
	for _, writes := range []string{"", `,"lloyds_writes":null`, `,"lloyds_writes":false`} {
		var response gameapi.LloydsQuoteResult
		if err := json.Unmarshal([]byte(`{"hull_value_pence":2000000,"lloyds_premium_bps":400`+writes+`}`), &response); err != nil {
			t.Fatal(err)
		}
		in, hull, origin, candidate := oceanicPremiumFixture(t, "oceanic", 1_000_000)
		for key, quote := range in.HazardQuotes {
			quote.LloydsWrites, quote.LloydsWritesKnown = response.LloydsWrites, response.LloydsWritesKnown
			quote.HullValuePence, quote.LloydsPremiumBPS = response.HullValuePence, response.LloydsPremiumBPS
			in.HazardQuotes[key] = quote
		}
		priced := pricePlan(in, hull, origin, candidate, 0, 0)
		if response.LloydsWritesKnown {
			if !priced.Accepted || priced.Economics.VoyagePremiumPence != 0 {
				t.Fatalf("authoritative refusal lost uninsured economics: %+v", priced)
			}
			continue
		}
		insured := int64(2_000_000) + priced.Economics.PurchasePence + priced.Economics.CustomsPence
		if priced.Accepted || priced.Economics.VoyagePremiumPence != ceilBPS(insured, 400) {
			t.Fatalf("unknown writes with complete price fields escaped insurance admission: writes=%q plan=%+v", writes, priced)
		}
		raw := in.HazardQuotes[hazardHullKey(in.Ship.Ship.ID, candidate.Origin, candidate.Destination)]
		if _, valid := plannedVoyagePremium(in, hull, in.Routes[routeKey(candidate.Origin, candidate.Destination, hull.ID)], raw, 0, 0); valid {
			t.Fatal("un-normalized unknown writes was treated as an authoritative no-write")
		}
	}
}

func TestVoyageInsuranceIntentRoundTripPreservesUnknownWrites(t *testing.T) {
	for _, writes := range []string{"", `,"lloyds_writes":null`, `,"lloyds_writes":false`, `,"lloyds_writes":true`} {
		var quote gameapi.LloydsQuoteResult
		if err := json.Unmarshal([]byte(`{"hull_value_pence":2000000,"lloyds_premium_bps":400`+writes+`}`), &quote); err != nil {
			t.Fatal(err)
		}
		data := missionData{VoyageInsurance: []voyageInsuranceIntent{{Quote: quote}}}
		var restored missionData
		if err := json.Unmarshal(encode(data), &restored); err != nil {
			t.Fatal(err)
		}
		got := restored.VoyageInsurance[0].Quote
		if got.LloydsWritesKnown != quote.LloydsWritesKnown || got.LloydsWrites != quote.LloydsWrites ||
			got.HullValuePence != quote.HullValuePence || got.LloydsPremiumBPS == nil || *got.LloydsPremiumBPS != *quote.LloydsPremiumBPS {
			t.Fatalf("stored intent promoted unknown writes or lost known terms: writes=%q before=%+v after=%+v", writes, quote, got)
		}
	}
}

func TestVoyageInsuranceRealHullUsesSelectedLaneAndStaleProxyTerms(t *testing.T) {
	for _, source := range []string{"owned-hull", "stale published"} {
		in, hull, origin, candidate := oceanicPremiumFixture(t, "oceanic", 1_000_000)
		if source == "owned-hull" {
			in.Fleet = []ShipState{in.Ship}
			in.Ship.Ship.ID, candidate.ShipID = "target-hull", "target-hull"
		} else {
			for key, quote := range in.HazardQuotes {
				quote.Error = "refresh timeout"
				quote.ObservedAt = in.Now.Add(-gameDuration(2, in.Clock.Calendar.TimeScale))
				in.HazardQuotes[key] = quote
			}
		}
		priced := pricePlan(in, hull, origin, candidate, 0, 0)
		insured := int64(2_000_000) + priced.Economics.PurchasePence + priced.Economics.CustomsPence
		if !strings.Contains(priced.Economics.HazardSource, "lane proxy") ||
			priced.Economics.VoyagePremiumPence != ceilBPS(insured, 400) || priced.Accepted {
			t.Fatalf("%s proxy omitted its insurance terms: %+v", source, priced)
		}
	}
}

func TestVoyageInsuranceGrowthCashExcludesHullBuildBeforeSelfInsurance(t *testing.T) {
	in, hull, origin, candidate := oceanicPremiumFixture(t, "oceanic", 5_000_000)
	in.Fleet = []ShipState{in.Ship}
	in.Ship = growthCandidate(origin, hull)
	in.Ship.Stores.ProvisionsTons = 8
	in.growthHullValuePence = 3_000_000
	candidate.ShipID = in.Ship.Ship.ID
	priced := pricePlan(in, hull, origin, candidate, 0, 0)
	cargo := priced.Economics.PurchasePence + priced.Economics.CustomsPence
	insured := in.growthHullValuePence + cargo
	if priced.Economics.VoyagePremiumPence != ceilBPS(insured, 400) {
		t.Fatalf("pre-build cash was mistaken for post-build self-insurance: %+v", priced)
	}
	in.Balances[origin.BankingRegion] = in.ReserveByRegion[origin.BankingRegion] + in.growthHullValuePence +
		cargo + priced.Economics.StoreCashPence + insured
	atBoundary := pricePlan(in, hull, origin, candidate, 0, 0)
	in.Balances[origin.BankingRegion]--
	belowBoundary := pricePlan(in, hull, origin, candidate, 0, 0)
	if atBoundary.Economics.VoyagePremiumPence != 0 || belowBoundary.Economics.VoyagePremiumPence != ceilBPS(insured, 400) {
		t.Fatalf("post-build cover boundary: at=%+v below=%+v", atBoundary.Economics, belowBoundary.Economics)
	}
}

func TestVoyageInsuranceSourcingPinnedRepricingKeepsAdmittedLot(t *testing.T) {
	in, hull, origin, candidate := oceanicPremiumFixture(t, "oceanic", 1_000_000)
	reward := int64(300_000)
	contract := Contract{ID: "cover-handoff", ContractType: "charter", Status: "open", OriginPortID: candidate.Destination,
		CommodityID: candidate.Commodity, RemainingQuantity: 24, RewardPerTonPence: &reward}
	in.Contracts = []Contract{contract}
	candidate.Kind, candidate.ContractID = "charter", contract.ID
	admitted := evaluatePlan(in, hull, origin, candidate, candidate.Quantity*reward, 0)
	if !admitted.Accepted || admitted.Economics.VoyagePremiumPence <= 0 {
		t.Fatalf("handoff fixture lacks an admitted insured lot: %+v", admitted)
	}
	in.Balances[origin.BankingRegion] = 5_000_000
	in.pinnedHazard = &admitted.Economics
	repriced := sizeRelocationCargo(in, admitted, contract, 0)
	if !repriced.Accepted || repriced.Quantity != admitted.Quantity ||
		repriced.Economics.VoyagePremiumPence != admitted.Economics.VoyagePremiumPence {
		t.Fatalf("first send resized the admitted lot or changed its fixed premium: admitted=%+v repriced=%+v", admitted, repriced)
	}
}

func TestVoyageInsuranceSourcingStageAndFirstSendPinSameLot(t *testing.T) {
	h, runner, ship, plan := incomeReviewFixture(t, "charter", true, 4)
	input, err := runner.committedPlanningInput(h.ctx, ship)
	if err != nil {
		t.Fatal(err)
	}
	plan.Quantity = 4
	plan = priceIncomePlan(input, plan, 0)
	if !plan.Accepted {
		t.Fatalf("small handoff fixture was not admitted: %+v", plan)
	}
	plan.Economics.VoyagePremiumPence = 101
	plan.Economics.ExpectedNetPence -= 101
	plan.Economics.StressNetPence -= 101
	plan.Economics.NetPencePerGameDay = float64(plan.Economics.ExpectedNetPence) / plan.Economics.GameDays
	for index := range runner.stateCache.Contracts {
		if runner.stateCache.Contracts[index].ID == plan.ContractID {
			runner.stateCache.Contracts[index].RemainingQuantity = 2
		}
	}
	mission, data := runner.newVoyage(ship, plan)
	mission.Phase = "buy"
	data.SourcingHandoff = &relocationIntent{TargetContractID: plan.ContractID}
	if err := runner.saveMission(h.ctx, mission, &data); err != nil {
		t.Fatal(err)
	}
	if _, err := runner.driveMission(h.ctx, ship, mission, &data); err != nil {
		t.Fatal(err)
	}
	mission, data = h.mission(ship.Ship.ID)
	if data.Action == nil || data.Action.Stage != "buy" || data.Plan.Quantity != 2 || data.Plan.Economics.VoyagePremiumPence != 0 {
		t.Fatalf("fresh smaller profitable handoff did not replace the old admission before staging: %+v", data)
	}
	for index := range runner.stateCache.Contracts {
		if runner.stateCache.Contracts[index].ID == plan.ContractID {
			runner.stateCache.Contracts[index].RemainingQuantity = 1_000
		}
	}
	if err := runner.newActionPreflight(h.ctx, mission, &data); err != nil {
		t.Fatalf("unchanged handoff first-send disagreed with stage-time admission: %v", err)
	}
}

func TestVoyageInsuranceBuyPreflightProtectsPremiumPennyBoundary(t *testing.T) {
	h, runner, ship, plan := incomeReviewFixture(t, "charter", true, 4)
	mission, data := incomeReviewStage(t, h, runner, ship, plan, "buy")
	data.Plan.Economics.VoyagePremiumPence = 12_345
	// Earlier paid cargo makes this an already-committed voyage, so this
	// isolates the buy's bills check from discretionary income admission.
	data.AcquisitionPence = 1
	input, err := runner.committedPlanningInput(h.ctx, ship)
	if err != nil {
		t.Fatal(err)
	}
	port, _ := findPort(input.Catalog, ship.Ship.CurrentPortID)
	purchase := data.Plan.Quantity * data.Plan.BuyLimitPence
	customs := ceilBPS(purchase, input.Markets[port.ID].CustomsBPS) + data.Plan.Quantity
	needed := growthCashTotal(input.ReserveByRegion[port.BankingRegion], purchase, customs,
		data.Plan.Economics.BerthDuesPence, data.Plan.Economics.UpkeepPence, data.Plan.Economics.CollateralPence,
		data.Plan.Economics.TollPence, data.Plan.Economics.VoyagePremiumPence)
	for _, delta := range []int64{-1, 0} {
		for index := range runner.stateCache.Vaults {
			if runner.stateCache.Vaults[index].BankingRegion == port.BankingRegion {
				runner.stateCache.Vaults[index].BalancePence = needed + delta
			}
		}
		err := runner.newActionPreflight(h.ctx, mission, &data)
		if (err == nil) != (delta == 0) || err != nil && !strings.Contains(err.Error(), "protected regional bills reserves") {
			t.Fatalf("premium first-send cash boundary %+dp: %v", delta, err)
		}
	}
}

func TestVoyageInsuranceBallastRepeatPricesEmptyReturnAndReplacementCash(t *testing.T) {
	in, hull, origin, candidate := oceanicPremiumFixture(t, "oceanic", 1_000_000)
	reward := int64(300_000)
	candidate.Kind, candidate.PriceSource = "charter", "live contract terms"
	key := routeKey(candidate.Destination, candidate.Origin, hull.ID)
	back := in.Routes[key]
	back.Legs[0].EdgeClass = "oceanic"
	in.Routes[key] = back
	reverse := in.HazardQuotes[hazardHullKey(in.Ship.Ship.ID, candidate.Origin, candidate.Destination)]
	reverse.Origin, reverse.Destination, reverse.ExpectedLossBPS = candidate.Destination, candidate.Origin, 400
	reverse.LloydsPremiumBPS = new(int64(400))
	in.HazardQuotes[hazardHullKey(in.Ship.Ship.ID, reverse.Origin, reverse.Destination)] = reverse
	in.Catalog.Ports[2].BankingRegion = "other"
	in.Balances["other"] = 0
	in.Metadata["rates"] = in.Metadata["vaults"]
	in.Rates = []Rate{{FromRegion: "other", ToRegion: origin.BankingRegion, FeeBPS: 0}}
	free := pricePlan(in, hull, origin, candidate, candidate.Quantity*reward, 0)
	_, portDays := deliveryGameDays(in.Routes[routeKey(candidate.Origin, candidate.Destination, hull.ID)],
		hull, in.Ports[candidate.Destination], candidate.Quantity)
	returnDays, returnProvisions, returnBerth, ok := ballastLegEconomics(in, hull, origin.ID, back)
	if !ok || !free.Accepted || free.Economics.BallastRepeatGameDays != free.Economics.GameDays+returnDays {
		t.Fatalf("insured ballast repeat fixture lacks a safe funded return: %+v", free)
	}
	returnPremium := ceilBPS(2_000_000, 400)
	wantNet := free.Economics.ExpectedNetPence - returnProvisions - returnBerth - returnPremium
	if free.Economics.BallastRepeatNetPence != wantNet {
		t.Fatalf("empty return lost its hull-only premium: want=%d economics=%+v", wantNet, free.Economics)
	}
	in.Rates[0].FeeBPS = 1_000
	paid := pricePlan(in, hull, origin, candidate, candidate.Quantity*reward, 0)
	destinationBerth := int64(math.Ceil(float64(in.Ports[candidate.Destination].BerthDuesPerDayPence) * max(0.25, portDays/2)))
	replacement := free.Economics.PurchasePence + free.Economics.CustomsPence + free.Economics.ProvisionsPence +
		returnProvisions + free.Economics.BerthDuesPence - destinationBerth + returnBerth + free.Economics.VoyagePremiumPence
	gross, err := GrossRemittance(replacement, 1_000)
	if err != nil || paid.Economics.BallastRepeatGameDays <= 0 ||
		free.Economics.BallastRepeatNetPence-paid.Economics.BallastRepeatNetPence != gross-replacement {
		t.Fatalf("recurring origin funding did not replace the outbound premium: free=%+v paid=%+v gross=%d err=%v",
			free.Economics, paid.Economics, gross, err)
	}
}

func TestVoyageInsuranceBallastWithoutReverseQuoteUsesOutwardTermsOrTwiceLoss(t *testing.T) {
	for _, hasPremium := range []bool{true, false} {
		in, hull, origin, candidate := oceanicPremiumFixture(t, "oceanic", 1_000_000)
		reverseKey := routeKey(candidate.Destination, candidate.Origin, hull.ID)
		back := in.Routes[reverseKey]
		back.Legs[0].EdgeClass = "oceanic"
		in.Routes[reverseKey] = back
		if !hasPremium {
			for key, quote := range in.HazardQuotes {
				quote.LloydsPremiumBPS = nil
				in.HazardQuotes[key] = quote
			}
		}
		candidate.Kind, candidate.PriceSource = "charter", "live contract terms"
		priced := pricePlan(in, hull, origin, candidate, candidate.Quantity*300_000, 0)
		backDays, backProvisions, backBerth, ok := ballastLegEconomics(in, hull, origin.ID, back)
		bps := int64(400)
		if !hasPremium {
			bps = 800
		}
		want := priced.Economics.ExpectedNetPence - backProvisions - backBerth - ceilBPS(2_000_000, bps)
		if !ok || !priced.Accepted || priced.Economics.BallastRepeatGameDays != priced.Economics.GameDays+backDays ||
			priced.Economics.BallastRepeatNetPence != want {
			t.Fatalf("missing reverse quote disabled or underpriced the repeat: hasPremium=%v plan=%+v want=%d", hasPremium, priced, want)
		}
	}
}

func TestVoyageInsuranceGrowthReturnPricesAndFundsHullOnlyCover(t *testing.T) {
	in, hull, _, candidate := oceanicPremiumFixture(t, "oceanic", 1_000_000)
	in.Ship.Ship.CurrentPortID = candidate.Destination
	key := routeKey(candidate.Destination, candidate.Origin, hull.ID)
	back := in.Routes[key]
	back.Legs[0].EdgeClass = "oceanic"
	in.Routes[key] = back
	book := in.Markets[candidate.Origin]
	book.PortID = candidate.Destination
	in.Markets[candidate.Destination] = book
	in.Metadata["books/"+candidate.Destination] = in.Metadata["books/"+candidate.Origin]
	in.Metadata["rates"] = in.Metadata["vaults"]
	reverse := in.HazardQuotes[hazardHullKey(in.Ship.Ship.ID, candidate.Origin, candidate.Destination)]
	reverse.Origin, reverse.Destination, reverse.ExpectedLossBPS = candidate.Destination, candidate.Origin, 400
	reverse.LloydsPremiumBPS = new(int64(400))
	reverseKey := hazardHullKey(in.Ship.Ship.ID, reverse.Origin, reverse.Destination)
	in.HazardQuotes[reverseKey] = reverse
	covered, err := growthReturnBasis(in, candidate.Origin)
	if err != nil || covered.premiumPence != 80_000 {
		t.Fatalf("growth empty return omitted its hull-only premium: quote=%+v err=%v", covered, err)
	}
	reward := int64(300_000)
	contract := Contract{ID: "growth-cover", ContractType: "charter", Status: "open", OriginPortID: candidate.Destination,
		CommodityID: candidate.Commodity, RemainingQuantity: 24, RewardPerTonPence: &reward}
	in.Contracts = []Contract{contract}
	candidate.Kind, candidate.ContractID = "charter", contract.ID
	lane := &growthLaneIntent{AdmissionID: "insured-return", QuantityLimit: 4}
	withCover := quoteGrowthDelivery(in, candidate, lane, false, &covered)
	reverse.LloydsWrites, reverse.LloydsWritesKnown = false, true
	in.HazardQuotes[reverseKey] = reverse
	uncovered, err := growthReturnBasis(in, candidate.Origin)
	withoutCover := quoteGrowthDelivery(in, candidate, lane, false, &uncovered)
	if err != nil || !withCover.plan.Accepted || !withoutCover.plan.Accepted ||
		withCover.required["channel"]-withoutCover.required["channel"] != covered.premiumPence ||
		covered.expense-uncovered.expense != covered.premiumPence ||
		withCover.rate >= withoutCover.rate {
		t.Fatalf("growth return did not fund and expense its premium: with=%+v without=%+v returns=%+v/%+v err=%v",
			withCover, withoutCover, covered, uncovered, err)
	}
}

func TestVoyageInsuranceGrowthReturnReservesCurrentPremiumOnlyOnce(t *testing.T) {
	_, runner, ship, _, route := voyageInsuranceCandidate(t)
	input := runner.planningInput(ship)
	piracy, complete := hazardRoutePiracy(input, route)
	if !complete {
		t.Fatal("return fixture lacks piracy observations")
	}
	quote := HazardQuote{ShipID: ship.Ship.ID, Origin: route.FromPort, Destination: route.ToPort,
		ShipClass: ship.Ship.ShipClass, CannonsCount: ship.Ship.CannonsCount, Month: input.Clock.Calendar.Month,
		ExpectedLossBPS: 50, MaxInsurablePence: 900_000, HullValuePence: 900_000,
		LloydsWrites: true, LloydsWritesKnown: true, LloydsPremiumBPS: new(int64(100)),
		ObservedAt: input.Now, NetPiracyBPS: piracy}
	runner.stateCache.HazardQuotes = map[string]HazardQuote{
		hazardHullKey(ship.Ship.ID, route.FromPort, route.ToPort): quote,
	}
	runner.snapshot[hazardQuoteSnapshotKey] = encode(runner.stateCache.HazardQuotes)
	mission := &store.Mission{ShipID: ship.Ship.ID, Phase: "return", Destination: route.ToPort}
	data := missionData{Plan: Plan{Kind: "spot", Origin: route.ToPort, Destination: route.FromPort},
		ReturnJourney: true, BuyDone: true, Settlement: &store.Settlement{},
		GrowthLane: &growthLaneIntent{NextPlan: &Plan{Kind: "spot", Origin: route.ToPort, Destination: route.FromPort,
			Quantity: 4, BuyLimitPence: 100}}}
	planned := make(map[string]int64)
	if premium := runner.reserveMissionCash(planned, mission, &data, ship); premium != 9_000 {
		t.Fatalf("return reserve omitted or duplicated its planned premium: premium=%d reserves=%v", premium, planned)
	}
	input.ReserveByRegion = make(map[string]int64)
	runner.voyagePremiumReserves(&input, mission, &data, ship)
	port, _ := findPort(input.Catalog, ship.Ship.CurrentPortID)
	if planned[port.BankingRegion]-input.ReserveByRegion[port.BankingRegion] != 9_000 {
		t.Fatalf("cover check could not exclude its own return premium: planned=%v own=%v", planned, input.ReserveByRegion)
	}
	data.VoyageInsurance = []voyageInsuranceIntent{{ReturnJourney: true,
		Request: gameapi.CreateInsurancePolicyRequest{DestPortID: route.ToPort},
		Quote:   gameapi.LloydsQuoteResult{OriginPortID: route.FromPort, LloydsPremiumPence: new(int64(9_001))}}}
	offered := make(map[string]int64)
	runner.reserveMissionCash(offered, mission, &data, ship)
	if offered[port.BankingRegion]-planned[port.BankingRegion] != 1 {
		t.Fatalf("actual return offer did not replace its prediction: planned=%v offered=%v", planned, offered)
	}
	data.VoyageInsurance[0].PolicyID = "paid-return-policy"
	paid := make(map[string]int64)
	runner.reserveMissionCash(paid, mission, &data, ship)
	if planned[port.BankingRegion]-paid[port.BankingRegion] != 9_000 {
		t.Fatalf("paid return premium remained reserved: planned=%v paid=%v", planned, paid)
	}
}

func TestVoyageInsuranceGrowthReturnUnavailablePremiumRetainsStoresAndBills(t *testing.T) {
	for _, knownHull := range []bool{true, false} {
		h, runner, ship, _, route := voyageInsuranceCandidate(t)
		h.api.setFactorPorts(route.ToPort)
		ship, _, route = voyageInsuranceRefresh(t, h, runner)
		ship.Stores.ProvisionsTons = 0
		runner.stateCache.HazardQuotes = nil
		premium := int64(0)
		if knownHull {
			runner.stateCache.HazardQuotes = map[string]HazardQuote{"known-hull": {
				ShipID: ship.Ship.ID, ShipClass: ship.Ship.ShipClass, HullValuePence: 900_000,
			}}
			premium = ceilBPS(900_000, 3_000)
		}
		input := runner.planningInput(ship)
		quote, err := growthReturnBasis(input, route.ToPort)
		if err != nil || quote.storeCash <= 0 || quote.departureBill <= 0 || quote.premiumPence != premium || quote.premiumKnown != knownHull {
			t.Fatalf("premium outage lost otherwise measured return expenses: knownHull=%v quote=%+v err=%v", knownHull, quote, err)
		}
		mission := &store.Mission{ShipID: ship.Ship.ID, Phase: "return", Destination: route.ToPort}
		data := missionData{Plan: Plan{Kind: "spot", Origin: route.ToPort, Destination: route.FromPort},
			ReturnJourney: true, BuyDone: true, Settlement: &store.Settlement{},
			GrowthLane: &growthLaneIntent{AdmissionID: "unpriced-return", QuantityLimit: 4,
				NextPlan: &Plan{Kind: "spot", Origin: route.ToPort, Destination: route.FromPort, Quantity: 4, BuyLimitPence: 100}}}
		required := make(map[string]int64)
		runner.reserveMissionCash(required, mission, &data, ship)
		port, _ := findPort(input.Catalog, ship.Ship.CurrentPortID)
		want := growthCashTotal(quote.storeCash, quote.departureBill, premium)
		if required[port.BankingRegion] != want {
			t.Fatalf("premium outage released return stores or departure cash: knownHull=%v want=%d reserves=%v", knownHull, want, required)
		}
		if !knownHull {
			delivery := quoteGrowthDelivery(input, *data.GrowthLane.NextPlan, data.GrowthLane, false, &quote)
			if delivery.plan.Accepted || delivery.plan.Reason != "growth return has no checked ocean voyage premium" {
				t.Fatalf("growth delivery admitted an unpriceable return: %+v", delivery)
			}
			continuation, err := runner.quoteGrowthContinuation(h.ctx, ship, &data)
			if err != nil || continuation.plan.Accepted || continuation.plan.Reason != "growth return has no checked ocean voyage premium" {
				t.Fatalf("growth continuation admitted an unpriceable return: quote=%+v err=%v", continuation, err)
			}
		}
	}
}

func voyageInsuranceRefresh(t *testing.T, h *recoveryHarness, runner *Runner) (ShipState, Hull, Route) {
	t.Helper()
	loanCapacityRefresh(t, h, runner)
	runner.stateCache.Catalog.ShipClasses[0].HullCategory = "oceanic"
	key := routeKey(recoveryHomePort, recoveryFarPort, recoveryHull)
	route := runner.stateCache.Routes[key]
	for index := range route.Legs {
		route.Legs[index].EdgeClass = "oceanic"
	}
	runner.stateCache.Routes[key] = route
	ship, exists := runner.ship(recoveryShipBlastoise)
	if !exists {
		t.Fatal("fixture ship missing")
	}
	return ship, runner.stateCache.Catalog.ShipClasses[0], route
}

func TestVoyageInsuranceJournalSurvivesRestartAndWaitsForCover(t *testing.T) {
	h := newRecoveryHarness(t)
	h.api.setVaultBalance("north", 500_000)
	h.api.setVaultBalance("south", 150_000)
	berthed := sailingShipRow(recoveryShipBlastoise, "Blastoise", recoveryHomePort, recoveryFarPort)
	berthed["ship"].(map[string]any)["status"] = "berthed"
	berthed["voyage"], berthed["route_legs"] = nil, []any{}
	h.api.setFleet([]map[string]any{berthed})
	var offered *InsurancePolicy
	base := h.api.server.Config.Handler
	h.api.server.Config.Handler = http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		if req.Method == http.MethodGet && req.URL.Path == "/v1/insurance/quote" {
			insured, _ := strconv.ParseInt(req.URL.Query().Get("insured_value_pence"), 10, 64)
			quote := voyageCoverTestQuote()
			quote.InsuredValuePence = insured
			quote.LloydsExpectedLossPence = new(ceilBPS(insured, 50))
			quote.LloydsPremiumPence = new(max(int64(100), ceilBPS(insured, 100)))
			h.api.writeJSON(w, http.StatusOK, quote)
			return
		}
		if req.Method == http.MethodGet && req.URL.Path == "/v1/insurance/list" {
			h.api.mu.Lock()
			defer h.api.mu.Unlock()
			policies := []InsurancePolicy{}
			if offered != nil && req.URL.Query().Get("underwriter_company_id") == "" {
				policies = append(policies, *offered)
			}
			h.api.writeJSON(w, http.StatusOK, policies)
			return
		}
		if req.Method == http.MethodPost && req.URL.Path == "/v1/insurance/create" {
			body, err := io.ReadAll(req.Body)
			if err != nil {
				t.Error(err)
				w.WriteHeader(http.StatusBadRequest)
				return
			}
			var request gameapi.CreateInsurancePolicyRequest
			if json.Unmarshal(body, &request) != nil || request.ShipID != recoveryShipBlastoise || request.DestPortID != recoveryFarPort ||
				request.InsuredValuePence != 1_000_000 || request.PremiumBPS != 100 || req.Header.Get("Idempotency-Key") == "" {
				t.Errorf("insurance offer did not declare the actual complete quote: %s", body)
				w.WriteHeader(http.StatusBadRequest)
				return
			}
			h.api.mu.Lock()
			defer h.api.mu.Unlock()
			if offered != nil {
				t.Error("duplicate voyage policy creation")
			}
			now := time.Now().UTC()
			offered = &InsurancePolicy{ID: "aaaaaaaa-aaaa-4aaa-8aaa-aaaaaaaaaaaa", InsuredCompanyID: recoveryCompanyID, ShipID: request.ShipID,
				OriginPortID: recoveryHomePort, DestPortID: request.DestPortID, InsuredValuePence: request.InsuredValuePence, HullValuePence: 900_000,
				PremiumBPS: request.PremiumBPS, TotalPremiumPence: 10_000, Status: "open", CreatedAt: now, UpdatedAt: now, ExpiresAt: now.Add(24 * time.Hour)}
			h.api.vaultBalance["north"] -= offered.TotalPremiumPence
			h.api.record(req, body, http.StatusCreated)
			h.api.writeJSON(w, http.StatusCreated, struct {
				InsurancePolicy
				LloydsPremiumBPS int64 `json:"lloyds_premium_bps"`
				AboveLloydsPrice bool  `json:"above_lloyds_price"`
			}{InsurancePolicy: *offered, LloydsPremiumBPS: 100})
			return
		}
		base.ServeHTTP(w, req)
	})
	runner := h.runner(primaryTestKey)
	ship, hull, route := voyageInsuranceRefresh(t, h, runner)
	mission := &store.Mission{AccountID: h.account.ID, ShipID: ship.Ship.ID, Generation: uuid.NewString(), Phase: "depart", Destination: recoveryFarPort}
	data := missionData{Plan: Plan{Kind: "charter", Origin: recoveryHomePort, Destination: recoveryFarPort}, StartedAt: time.Now().UTC(), CostsKnown: true}
	if handled, changed, err := runner.prepareVoyageInsurance(h.ctx, ship, hull, route, mission, &data); !handled || !changed || err != nil {
		t.Fatalf("offer staging: %v %v %v", handled, changed, err)
	}
	beforeOffer := copyCommissionMissionData(t, data)
	if _, err := runner.dispatch(h.ctx, mission, &data); err != nil {
		t.Fatal(err)
	}
	if len(data.VoyageInsurance) != 1 || data.VoyageInsurance[0].PolicyID == "" || data.FeesPence != 10_000 {
		t.Fatalf("offer was not booked once: %+v", data)
	}
	if err := runner.saveMission(h.ctx, mission, &beforeOffer); err != nil {
		t.Fatal(err)
	}
	runner = h.runner(primaryTestKey)
	ship, hull, route = voyageInsuranceRefresh(t, h, runner)
	mission, data = h.mission(ship.Ship.ID)
	if _, err := runner.dispatch(h.ctx, mission, &data); err != nil {
		t.Fatal(err)
	}
	if h.api.mutationCount("/v1/insurance/create") != 1 || data.FeesPence != 10_000 {
		t.Fatal("restart repeated the policy or premium")
	}
	if handled, changed, err := runner.prepareVoyageInsurance(h.ctx, ship, hull, route, mission, &data); !handled || changed || err != nil {
		t.Fatalf("departed before Lloyd's first look: %v %v %v", handled, changed, err)
	}
	h.api.mu.Lock()
	offered.Status, offered.CoveredBPS = "active", 5_000
	h.api.mu.Unlock()
	ship, hull, route = voyageInsuranceRefresh(t, h, runner)
	if handled, _, err := runner.prepareVoyageInsurance(h.ctx, ship, hull, route, mission, &data); !handled || err != nil {
		t.Fatalf("partial syndicate slice bypassed Lloyd's first-look window: %v %v", handled, err)
	}
	h.api.mu.Lock()
	offered.CoveredBPS = 10_000
	h.api.mu.Unlock()
	ship, hull, route = voyageInsuranceRefresh(t, h, runner)
	if handled, _, err := runner.prepareVoyageInsurance(h.ctx, ship, hull, route, mission, &data); handled || err != nil {
		t.Fatalf("full cover did not release departure immediately: %v %v", handled, err)
	}
	h.api.mu.Lock()
	offered.CoveredBPS = 5_000
	offered.CreatedAt = time.Now().UTC().Add(-7 * time.Hour / time.Duration(runner.stateCache.Clock.Calendar.TimeScale))
	h.api.mu.Unlock()
	ship, hull, route = voyageInsuranceRefresh(t, h, runner)
	if handled, _, err := runner.prepareVoyageInsurance(h.ctx, ship, hull, route, mission, &data); handled || err != nil {
		t.Fatalf("bounded partial-cover wait did not release departure: %v %v", handled, err)
	}
}

func voyageInsuranceCandidate(t *testing.T) (*recoveryHarness, *Runner, ShipState, Hull, Route) {
	t.Helper()
	h := newRecoveryHarness(t)
	h.api.setVaultBalance("north", 500_000)
	h.api.setVaultBalance("south", 150_000)
	berthed := sailingShipRow(recoveryShipBlastoise, "Blastoise", recoveryHomePort, recoveryFarPort)
	berthed["ship"].(map[string]any)["status"] = "berthed"
	berthed["voyage"], berthed["route_legs"] = nil, []any{}
	h.api.setFleet([]map[string]any{berthed})
	base := h.api.server.Config.Handler
	h.api.server.Config.Handler = http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		if req.Method == http.MethodGet && req.URL.Path == "/v1/insurance/quote" {
			quote := voyageCoverTestQuote()
			quote.InsuredValuePence, _ = strconv.ParseInt(req.URL.Query().Get("insured_value_pence"), 10, 64)
			h.api.writeJSON(w, http.StatusOK, quote)
			return
		}
		base.ServeHTTP(w, req)
	})
	runner := h.runner(primaryTestKey)
	ship, hull, route := voyageInsuranceRefresh(t, h, runner)
	return h, runner, ship, hull, route
}

func TestVoyageInsuranceOptionalOfferDoesNotTrapUnfundedOrBoundedLeg(t *testing.T) {
	for _, reason := range []string{"stale board", "protected cash", "exploration cap"} {
		t.Run(reason, func(t *testing.T) {
			h, runner, ship, hull, route := voyageInsuranceCandidate(t)
			mission := &store.Mission{AccountID: h.account.ID, ShipID: ship.Ship.ID, Generation: uuid.NewString(), Phase: "depart", Destination: recoveryFarPort}
			data := missionData{Plan: Plan{ShipID: ship.Ship.ID, Kind: "charter", Origin: recoveryHomePort, Destination: recoveryFarPort}, CostsKnown: true}
			switch reason {
			case "stale board":
				delete(runner.meta, "insurance")
			case "protected cash":
				for index := range runner.stateCache.Vaults {
					runner.stateCache.Vaults[index].BalancePence = 0
				}
			case "exploration cap":
				data.Relocation = &relocationIntent{ExpenseCapPence: 9_999}
			}
			if handled, changed, err := runner.prepareVoyageInsurance(h.ctx, ship, hull, route, mission, &data); handled || changed || err != nil {
				t.Fatalf("optional cover trapped the working leg: %v %v %v", handled, changed, err)
			}
			if data.Action != nil || len(data.VoyageInsurance) != 0 || h.api.mutationCount("/v1/company/remit") != 0 {
				t.Fatal("optional cover spent or reserved unavailable cash")
			}
		})
	}
}

func TestVoyageInsuranceBindsEachRecoveryLegAndCommitsLoadedDeparture(t *testing.T) {
	h, runner, ship, hull, route := voyageInsuranceCandidate(t)
	firstQuote := voyageCoverTestQuote()
	firstQuote.OriginPortID, firstQuote.DestPortID = "previous", recoveryHomePort
	data := missionData{Plan: Plan{Kind: "recovery", Origin: recoveryHomePort, Destination: recoveryFarPort}, CostsKnown: true,
		VoyageInsurance: []voyageInsuranceIntent{{Quote: firstQuote, Request: gameapi.CreateInsurancePolicyRequest{DestPortID: recoveryHomePort},
			PolicyID: "bbbbbbbb-bbbb-4bbb-8bbb-bbbbbbbbbbbb"}}}
	mission := &store.Mission{AccountID: h.account.ID, ShipID: ship.Ship.ID, Generation: uuid.NewString(), Phase: "depart", Destination: recoveryFarPort}
	if handled, changed, err := runner.prepareVoyageInsurance(h.ctx, ship, hull, route, mission, &data); !handled || !changed || err != nil {
		t.Fatalf("previous outbound recovery leg prevented the next leg's policy: %v %v %v", handled, changed, err)
	}
	if len(data.VoyageInsurance) != 2 || data.VoyageInsurance[0].PolicyID == "" || data.VoyageInsurance[1].Request.DestPortID != recoveryFarPort {
		t.Fatalf("new leg overwrote the old policy: %+v", data.VoyageInsurance)
	}
	data.Plan.Kind, data.BoughtQuantity = "spot", 10
	data.VoyageInsurance[1].PolicyID = "aaaaaaaa-aaaa-4aaa-8aaa-aaaaaaaaaaaa"
	// No executable destination book is supplied: the accepted policy retains
	// the already-compared leg instead of stranding insured cargo in port.
	if proceed, err := runner.procurementDepartureDecision(PlanningInput{}, ship, recoveryFarPort, &data); !proceed || err != nil {
		t.Fatalf("post-policy repricing unwound the insured leg: %v %v", proceed, err)
	}
	required := make(map[string]int64)
	data.VoyageInsurance[1].PolicyID = ""
	runner.reserveMissionCash(required, mission, &data, ship)
	if required["north"] < 10_000 {
		t.Fatal("unspent premium was not protected before its offer")
	}
	ship.Ship.Status = "sailing"
	atSea := make(map[string]int64)
	runner.reserveMissionCash(atSea, mission, &data, ship)
	if required["north"]-atSea["north"] != 10_000 {
		t.Fatalf("declined premium remained reserved after departure: in port=%d at sea=%d", required["north"], atSea["north"])
	}
}

func TestVoyageInsuranceReconcilesAgainstAdmissionServerTimeNotHostTime(t *testing.T) {
	h, runner, ship, hull, route := voyageInsuranceCandidate(t)
	mission := &store.Mission{AccountID: h.account.ID, ShipID: ship.Ship.ID, Generation: uuid.NewString(), Phase: "depart", Destination: recoveryFarPort}
	data := missionData{Plan: Plan{Kind: "charter", Origin: recoveryHomePort, Destination: recoveryFarPort}, CostsKnown: true}
	if _, _, err := runner.prepareVoyageInsurance(h.ctx, ship, hull, route, mission, &data); err != nil || data.Action == nil {
		t.Fatalf("offer staging: %v", err)
	}
	intent := &data.VoyageInsurance[0]
	intent.ServerNotBefore = time.Now().UTC().Add(-time.Minute)
	created := intent.ServerNotBefore.Add(time.Second)
	policy := InsurancePolicy{ID: "aaaaaaaa-aaaa-4aaa-8aaa-aaaaaaaaaaaa", InsuredCompanyID: recoveryCompanyID,
		ShipID: ship.Ship.ID, OriginPortID: recoveryHomePort, DestPortID: recoveryFarPort, InsuredValuePence: 1_000_000,
		HullValuePence: 900_000, PremiumBPS: 100, TotalPremiumPence: 10_000, Status: "open", CreatedAt: created, UpdatedAt: created, ExpiresAt: created.Add(time.Hour)}
	runner.stateCache.InsurancePolicies = []InsurancePolicy{policy}
	action := data.Action
	persisted, err := h.db.BeginAction(h.ctx, &store.Action{ID: action.ID, AccountID: h.account.ID, Method: action.Method, Path: action.Path, Body: action.Body})
	if err != nil {
		t.Fatal(err)
	}
	if changed, err := runner.reconcileVoyageInsurance(h.ctx, mission, &data, persisted); !changed || err != nil {
		t.Fatalf("server-issued matching policy was lost to local clock skew: %v %v", changed, err)
	}
	if data.Action != nil || data.VoyageInsurance[0].PolicyID != policy.ID || data.FeesPence != 10_000 {
		t.Fatalf("authoritative policy did not settle the single offer: %+v", data)
	}
}

func TestVoyageInsuranceDefinitiveRefusalSkipsRepeatedOfferAfterRestart(t *testing.T) {
	h, runner, ship, hull, route := voyageInsuranceCandidate(t)
	mission := &store.Mission{AccountID: h.account.ID, ShipID: ship.Ship.ID, Generation: uuid.NewString(), Phase: "depart", Destination: recoveryFarPort}
	data := missionData{Plan: Plan{Kind: "recovery", Origin: recoveryHomePort, Destination: recoveryFarPort}, CostsKnown: true}
	if _, _, err := runner.prepareVoyageInsurance(h.ctx, ship, hull, route, mission, &data); err != nil {
		t.Fatal(err)
	}
	if _, err := runner.consumeVoyageInsuranceResult(h.ctx, mission, &data, http.StatusConflict, encode(map[string]string{"code": "policy_refused", "detail": "cannot write this offer"})); err != nil {
		t.Fatal(err)
	}
	if data.CooldownUntil.After(time.Now().UTC().Add(6 * time.Second)) {
		t.Fatal("optional policy refusal imposed a game-day freight delay")
	}
	runner = h.runner(primaryTestKey)
	ship, hull, route = voyageInsuranceRefresh(t, h, runner)
	mission, data = h.mission(ship.Ship.ID)
	if handled, changed, err := runner.prepareVoyageInsurance(h.ctx, ship, hull, route, mission, &data); handled || changed || err != nil {
		t.Fatalf("definitively refused optional cover trapped the same leg: %v %v %v", handled, changed, err)
	}
}

func TestVoyageInsuranceCommittedReturnRetainsOwnWorkingCashAndNavigation(t *testing.T) {
	h, runner, ship, _, _ := voyageInsuranceCandidate(t)
	quote := voyageCoverTestQuote()
	data := missionData{Plan: Plan{Origin: recoveryFarPort, Destination: recoveryHomePort}, ReturnJourney: true,
		GrowthLane: &growthLaneIntent{NextPlan: &Plan{Kind: "spot", Origin: recoveryFarPort, Destination: recoveryHomePort, Quantity: 100, BuyLimitPence: 5_000}},
		VoyageInsurance: []voyageInsuranceIntent{{ReturnJourney: true, Quote: quote, PolicyID: "aaaaaaaa-aaaa-4aaa-8aaa-aaaaaaaaaaaa",
			Request: gameapi.CreateInsurancePolicyRequest{DestPortID: recoveryFarPort}}}}
	action := &durableAction{Stage: "depart", Body: encode(map[string]string{"dest_port_id": recoveryFarPort})}
	if err := runner.growthAllocationReady(h.ctx, ship, &data, action); err != nil {
		t.Fatalf("insured return re-entered discretionary next-lot repricing: %v", err)
	}
	mission := &store.Mission{ShipID: ship.Ship.ID, Destination: recoveryFarPort}
	input := runner.planningInput(ship)
	input.ReserveByRegion = make(map[string]int64)
	runner.voyagePremiumReserves(&input, mission, &data, ship)
	port, _ := findPort(runner.stateCache.Catalog, recoveryFarPort)
	if input.ReserveByRegion[port.BankingRegion] < 500_000 {
		t.Fatalf("premium could spend the mission's next cargo allocation: %+v", input.ReserveByRegion)
	}
}

func TestVoyageInsuranceWaitCannotConsumeFreightDeadline(t *testing.T) {
	h, runner, ship, hull, route := voyageInsuranceCandidate(t)
	_, upper, _ := runner.serverBounds()
	scale := float64(runner.stateCache.Clock.Calendar.TimeScale)
	days := float64(route.TotalDistanceNM)/(float64(hull.BaseSpeedCentiknots)/100*0.8)/24 + 1
	deadline := upper.Add(time.Duration((days*24+3)*3600/scale) * time.Second)
	data := missionData{Plan: Plan{Kind: "recovery", Origin: recoveryHomePort, Destination: recoveryFarPort},
		Freight: []freightObligation{{ContractID: "sealed", Destination: recoveryFarPort, Deadline: &deadline}}}
	if err := runner.validateFreightDeparture(ship, recoveryFarPort, &data, hull); err != nil {
		t.Fatalf("fixture already misses its immediate departure window: %v", err)
	}
	mission := &store.Mission{AccountID: h.account.ID, ShipID: ship.Ship.ID, Generation: uuid.NewString(), Phase: "depart", Destination: recoveryFarPort}
	if handled, changed, err := runner.prepareVoyageInsurance(h.ctx, ship, hull, route, mission, &data); handled || changed || err != nil {
		t.Fatalf("optional cover spent the final freight slack: %v %v %v", handled, changed, err)
	}
}

func TestVoyageInsuranceSelfInsuresSurvivableLoss(t *testing.T) {
	h, runner, ship, hull, route := voyageInsuranceCandidate(t)
	h.api.setVaultBalance("north", 50_000_000)
	ship, hull, route = voyageInsuranceRefresh(t, h, runner)
	mission := &store.Mission{AccountID: h.account.ID, ShipID: ship.Ship.ID, Generation: uuid.NewString(), Phase: "depart", Destination: recoveryFarPort}
	data := missionData{Plan: Plan{Kind: "recovery", Origin: recoveryHomePort, Destination: recoveryFarPort}, CostsKnown: true}
	if handled, changed, err := runner.prepareVoyageInsurance(h.ctx, ship, hull, route, mission, &data); handled || changed || err != nil {
		t.Fatalf("survivable loss bought negative-EV cover: %v %v %v", handled, changed, err)
	}
	if data.Action != nil || len(data.VoyageInsurance) != 0 {
		t.Fatal("self-insured leg reserved a premium")
	}
}
