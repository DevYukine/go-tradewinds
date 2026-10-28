package engine_test

import (
	"encoding/json"
	"math"
	"strings"
	"testing"
	"time"

	"github.com/DevYukine/go-tradewinds/internal/engine"
)

func fixture[T any](t *testing.T, raw string) T {
	t.Helper()
	var value T
	if err := json.Unmarshal([]byte(raw), &value); err != nil {
		t.Fatal(err)
	}
	return value
}

func inputFixture(t *testing.T) engine.PlanningInput {
	t.Helper()
	now := time.Date(2026, 10, 22, 17, 0, 0, 0, time.UTC)
	catalog := fixture[engine.Catalog](t, `{
		"ports":[
			{"id":"le_havre","banking_region":"channel","sea_zone_id":"english_channel","phase":1,"has_shipyard":true,"manufactures":["provisions"]},
			{"id":"portsmouth","banking_region":"channel","sea_zone_id":"english_channel","phase":1,"imports":["provisions"]}
		],
		"ship_classes":[{"id":"hoy","hull_category":"coastal","phase":1,"base_speed_centiknots":600,"hold_capacity_tons":40,"max_cannons":4,"crew_required":10}],
		"sea_zones":[{"id":"english_channel","zone_class":"coastal","base_storm_bps":200,"base_piracy_bps":200}]
	}`)
	ship := fixture[engine.ShipState](t, `{
		"ship":{"id":"ship-a","name":"Lapras","ship_class":"hoy","status":"berthed","current_port_id":"le_havre","cannons_count":4},
		"hold_capacity_tons":40,"effective_hold_capacity_tons":24,"cargo_used_tons":0,
		"stores":{"provisions_tons":8},"cargo_manifest":{},"route_legs":[]
	}`)
	book := fixture[engine.Market](t, `{
		"port_id":"le_havre","visibility":"quay","customs_bps":250,
		"books":[{"commodity_id":"provisions","depth":{"asks":[{"order_id":"npc-1","price_pence":1000,"remaining_quantity":100}],"bids":[]}}]
	}`)
	outward := fixture[engine.Route](t, `{
		"from_port":"le_havre","to_port":"portsmouth","ship_class":"hoy","total_distance_nm":30,"safe_provisions_tons":0.1,"port_provisions_tons_per_day":0.05,
		"legs":[{"from_node":"le_havre","to_node":"english_channel","distance_nm":15,"edge_class":"coastal","is_sound_toll":false},{"from_node":"english_channel","to_node":"portsmouth","distance_nm":15,"edge_class":"coastal","is_sound_toll":false}]
	}`)
	back := outward
	back.FromPort = "portsmouth"
	back.ToPort = "le_havre"
	back.Legs = []engine.RouteLeg{
		{FromNode: "portsmouth", ToNode: "english_channel", DistanceNM: 15, EdgeClass: "coastal"},
		{FromNode: "english_channel", ToNode: "le_havre", DistanceNM: 15, EdgeClass: "coastal"},
	}
	contract := fixture[engine.Contract](t, `{"id":"charter-a","contract_type":"charter","origin_port_id":"portsmouth","commodity_id":"provisions","quantity_required":1000,"remaining_quantity":1000,"reward_per_ton_pence":3000,"status":"open","is_public":true,"stale":false}`)
	clock := fixture[engine.Clock](t, `{"unlocked_phase":1,"paused":false,"calendar":{"month":3,"day":5,"time_scale":60,"total_game_hours":118}}`)
	clock.RealUTC = now
	meta := make(map[string]engine.DatasetMeta)
	for _, key := range []string{"catalog", "clock", "zones", "vaults", "contracts", "ships", "books/le_havre", "ports/le_havre", "ports/portsmouth", "routes/le_havre|portsmouth|hoy", "routes/portsmouth|le_havre|hoy"} {
		meta[key] = engine.DatasetMeta{UpdatedAt: now, AttemptedAt: now, Generation: "sync-a", Complete: true}
	}
	return engine.PlanningInput{
		Now:       now,
		CompanyID: "company-a",
		Ship:      ship,
		Catalog:   catalog,
		Clock:     clock,
		Zones:     []engine.ZoneObservation{{SeaZoneID: "english_channel", NetPiracyBPS: 200}},
		HazardQuotes: map[string]engine.HazardQuote{
			"hull|ship-a|le_havre|portsmouth": {
				ShipID: "ship-a", Origin: "le_havre", Destination: "portsmouth", ShipClass: "hoy",
				CannonsCount: 4, Month: 3, ObservedAt: now, AttemptedAt: now,
				NetPiracyBPS: map[string]int64{"english_channel": 200},
			},
		},
		Markets: map[string]engine.Market{"le_havre": book},
		Ports: map[string]engine.PortDetail{
			"le_havre":   {ID: "le_havre", BerthDuesPerDayPence: 1000, EffectiveBerths: 4},
			"portsmouth": {ID: "portsmouth", BerthDuesPerDayPence: 1000, EffectiveBerths: 4},
		},
		Routes:          map[string]engine.Route{"le_havre|portsmouth|hoy": outward, "portsmouth|le_havre|hoy": back},
		Contracts:       []engine.Contract{contract},
		Balances:        map[string]int64{"channel": 1_000_000},
		ReserveByRegion: map[string]int64{"channel": 75_000},
		Metadata:        meta,
		Generation:      "sync-a",
	}
}

func TestDepthWalkUsesEveryFillAndPreservesInput(t *testing.T) {
	t.Parallel()
	levels := []engine.Level{{PricePence: 200, RemainingQuantity: 3}, {PricePence: 100, RemainingQuantity: 2}}
	fill, err := engine.WalkDepth(levels, 4, true)
	if err != nil || fill.GrossPence != 600 || fill.LimitPence != 200 || fill.Quantity != 4 {
		t.Fatalf("unexpected buy depth: %+v, %v", fill, err)
	}
	if levels[0].PricePence != 200 {
		t.Fatal("planner mutated consumer-owned depth")
	}
	fill, err = engine.WalkDepth(levels, 4, false)
	if err != nil || fill.GrossPence != 700 || fill.LimitPence != 100 {
		t.Fatalf("unexpected sell depth: %+v, %v", fill, err)
	}
	if _, err := engine.WalkDepth(levels, 6, true); err == nil {
		t.Fatal("missing depth was extrapolated")
	}
	if _, err := engine.WalkDepth([]engine.Level{{PricePence: 1_000_000_000_000, RemainingQuantity: math.MaxInt64}}, math.MaxInt64, true); err == nil {
		t.Fatal("overflowing monetary value was accepted")
	}
}

func TestPublicCharterUsesMeasuredLocalDepthWithoutRemoteBook(t *testing.T) {
	t.Parallel()
	in := inputFixture(t)
	plans := engine.PlanVoyages(in)
	if len(plans) == 0 || !plans[0].Accepted || plans[0].Kind != "charter" {
		t.Fatalf("observable public feeder rejected: %+v", plans)
	}
	plan := plans[0]
	if plan.Origin != "le_havre" || plan.Destination != "portsmouth" || plan.Quantity != 24 || plan.Economics.ExpectedNetPence <= 0 {
		t.Fatalf("incorrect measured feeder economics: %+v", plan)
	}
	if plan.Economics.CustomsPence <= 0 || plan.Economics.ProvisionsPence <= 0 || plan.Economics.GameDays <= 30.0/6/24 {
		t.Fatalf("customs, consumed stores or delivery duration omitted: %+v", plan.Economics)
	}
	if plan.Economics.ProbabilityEstimate != nil || plan.Economics.StressHaircutPence <= 0 {
		t.Fatal("diagnostic stress was missing or presented as measured hazard probability")
	}
}

func TestPlannerUsesRetainedQuayDepthWithoutRepeatFunding(t *testing.T) {
	t.Parallel()
	in := inputFixture(t)
	in.Contracts = nil
	in.QuayBookMemory = map[string]engine.RetainedQuayBook{
		"portsmouth": {
			ObservedAt: in.Now.Add(-144 * time.Minute),
			Bids: map[string][]engine.RetainedBid{
				"provisions": {{PricePence: 5_000, RemainingQuantity: 20}},
			},
		},
	}
	plan := engine.PlanVoyages(in)[0]
	if !plan.Accepted || plan.Kind != "spot" || plan.Quantity != 20 ||
		plan.Economics.RevenueBasis != "retained" || plan.Economics.RevenuePence != 94_000 ||
		plan.Economics.PriceUncertaintyStressPence != 9_000 || plan.SellLimitPence != 4_250 {
		t.Fatalf("remembered quay depth did not fund the complete delivery lot: %+v", plan)
	}
	if plan.PriceSource != "our quay book 6.0 game days old; 600 bps age haircut" ||
		plan.Economics.BallastRepeatGameDays != 0 || plan.Economics.BallastRepeatNetPence != 0 {
		t.Fatalf("retained estimates lost their source or financed a repeat: %+v", plan)
	}
}

func narrowMarginFixture(t *testing.T) engine.PlanningInput {
	t.Helper()
	in := inputFixture(t)
	in.Catalog.ShipClasses[0].BaseSpeedCentiknots = 850
	in.Catalog.SeaZones[0].BaseStormBPS = 500
	in.Catalog.SeaZones[0].BasePiracyBPS = 400
	in.Zones[0].NetPiracyBPS = 400
	quote := in.HazardQuotes["hull|ship-a|le_havre|portsmouth"]
	quote.NetPiracyBPS["english_channel"] = 400
	in.HazardQuotes["hull|ship-a|le_havre|portsmouth"] = quote
	in.Ship.Stores.ProvisionsTons = 7.9
	berthedAt := in.Now.Add(-48*time.Minute - 30*time.Second)
	in.Ship.Ship.BerthedAt = &berthedAt
	in.Ports["le_havre"] = engine.PortDetail{
		ID: "le_havre", BerthDuesPerDayPence: 1024, EffectiveBerths: 8,
	}
	in.Ports["portsmouth"] = engine.PortDetail{
		ID: "portsmouth", BerthDuesPerDayPence: 1000, EffectiveBerths: 12,
	}
	market := in.Markets["le_havre"]
	market.CustomsBPS = 290
	market.Books[0].Depth.Asks = []engine.Level{
		{PricePence: 19_716, RemainingQuantity: 40},
	}
	in.Markets["le_havre"] = market
	reward := int64(21_958)
	in.Contracts[0].RewardPerTonPence = &reward
	return in
}

func TestMeasuredPositiveCashMarginIsNotRejectedByNegativeStress(t *testing.T) {
	t.Parallel()
	in := narrowMarginFixture(t)
	plan := engine.PlanVoyages(in)[0]
	if !plan.Accepted || plan.Kind != "charter" || plan.Quantity != 24 {
		t.Fatalf("profitable measured public feeder rejected: %+v", plan)
	}
	economics := plan.Economics
	if economics.PurchasePence != 473_184 || economics.CustomsPence != 13_747 ||
		economics.RevenuePence != 526_992 || economics.ProvisionsPence <= 0 ||
		economics.BerthDuesPence <= 0 || economics.ExpectedNetPence <= 0 {
		t.Fatalf("observable cash costs were changed into hypothetical loss costs: %+v", economics)
	}
	if economics.StressNetPence >= 0 || economics.ProbabilityEstimate != nil {
		t.Fatalf("uncalibrated stress must stay separately disclosed: %+v", economics)
	}
	if math.Abs(economics.NetPencePerGameDay-float64(economics.ExpectedNetPence)/economics.GameDays) > 1e-8 {
		t.Fatalf("ranking still deducts a hypothetical loss: %+v", economics)
	}
	if plan.StoresToBuy != 0 || plan.TargetStoresTons > in.Ship.Stores.ProvisionsTons {
		t.Fatalf("existing safe stores were ignored: %+v", plan)
	}
}

func TestPlannerCashNetIdentityDoesNotChargeStressOrRefundableCollateral(t *testing.T) {
	t.Parallel()
	for _, kind := range []string{"charter", "spot", "freight"} {
		t.Run(kind, func(t *testing.T) {
			in := inputFixture(t)
			in.FleetDailyUpkeepPence = 1_200
			switch kind {
			case "spot":
				in.Contracts = nil
				in.Markets["portsmouth"] = fixture[engine.Market](t, `{
					"port_id":"portsmouth","visibility":"quay",
					"books":[{"commodity_id":"provisions","depth":{"asks":[],"bids":[{"price_pence":5000,"remaining_quantity":100}]}}]
				}`)
				in.Metadata["books/portsmouth"] = in.Metadata["books/le_havre"]
			case "freight":
				in.Contracts = []engine.Contract{{
					ID: "sealed-freight", ContractType: "freight", Status: "open",
					OriginPortID: "le_havre", DestPortID: "portsmouth",
					CommodityID: "grain", QuantityRequired: 12, RewardPence: 100_000,
					CollateralPence: 40_000, IssuerCompanyID: "other-company",
				}}
			}
			calm := engine.PlanVoyages(in)[0]
			if !calm.Accepted || calm.Kind != kind {
				t.Fatalf("funded %s control was not executable: %+v", kind, calm)
			}
			cash := calm.Economics
			wantCash := cash.RevenuePence - cash.PurchasePence - cash.CustomsPence -
				cash.ProvisionsPence - cash.BerthDuesPence - cash.UpkeepPence - cash.TollPence - cash.HazardPence
			if cash.ExpectedNetPence != wantCash || cash.UpkeepPence != 0 ||
				cash.ProvisionsPence <= 0 || cash.BerthDuesPence <= 0 {
				t.Fatalf("cash net is not revenue less observable nonrefundable costs: %+v; identity %dp", cash, wantCash)
			}
			if kind == "freight" && cash.CollateralPence != in.Contracts[0].CollateralPence {
				t.Fatalf("refundable collateral disappeared instead of remaining separately funded: %+v", cash)
			}
			in.Catalog.SeaZones[0].BaseStormBPS = 8_000
			in.Zones[0].NetPiracyBPS = 8_000
			quote := in.HazardQuotes["hull|ship-a|le_havre|portsmouth"]
			quote.NetPiracyBPS["english_channel"] = 8_000
			in.HazardQuotes["hull|ship-a|le_havre|portsmouth"] = quote
			stressed := engine.PlanVoyages(in)[0]
			diagnostic := stressed.Economics
			if !stressed.Accepted || stressed.Kind != calm.Kind || stressed.Quantity != calm.Quantity ||
				diagnostic.ExpectedNetPence != cash.ExpectedNetPence ||
				diagnostic.NetPencePerGameDay != cash.NetPencePerGameDay {
				t.Fatalf("zone-rating stress changed cash acceptance, lot size or ranking despite unchanged published loss: calm %+v stressed %+v", calm, stressed)
			}
			if diagnostic.StressHaircutPence <= cash.StressHaircutPence ||
				diagnostic.StressNetPence != diagnostic.ExpectedNetPence-diagnostic.StressHaircutPence-diagnostic.PriceUncertaintyStressPence ||
				diagnostic.ProbabilityEstimate != nil {
				t.Fatalf("stress did not remain a separate uncalibrated diagnostic: calm %+v stressed %+v", cash, diagnostic)
			}
			if math.Abs(diagnostic.NetPencePerGameDay-float64(wantCash)/diagnostic.GameDays) > 1e-8 {
				t.Fatalf("cash ranking deducted stress or collateral: %+v", diagnostic)
			}
		})
	}
}

func TestDeliveryTurnaroundMatchesBerthLocks(t *testing.T) {
	t.Parallel()
	in := narrowMarginFixture(t)
	plan := engine.PlanVoyages(in)[0]
	if !plan.Accepted {
		t.Fatal("turnaround comparison requires an accepted charter")
	}
	hull := in.Catalog.ShipClasses[0]
	outward := in.Routes["le_havre|portsmouth|hoy"]
	destination := in.Ports["portsmouth"]
	sailingDays := float64(outward.TotalDistanceNM) / (float64(hull.BaseSpeedCentiknots) / 100 * 0.8) / 24
	berthLockDays := 60.0 / 1440
	handlingDays := float64(plan.Quantity*4) / 1440
	queueDays := float64(destination.QueuedShips+1) / float64(destination.EffectiveBerths) * 0.25
	want := sailingDays + berthLockDays + handlingDays + queueDays
	if math.Abs(plan.Economics.GameDays-want) > 1e-9 {
		t.Fatalf("delivery duration %.9f days, want %.9f (sailing %.9f + two 30min berth locks %.9f + handling %.9f + queue %.9f)", plan.Economics.GameDays, want, sailingDays, berthLockDays, handlingDays, queueDays)
	}
}

func TestCandidateDestinationPreservesReserveWithoutRecordedObligation(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name     string
		reserve  int64
		balance  int64
		accepted bool
	}{
		{name: "unoccupied destination below floor", balance: 75_999},
		{name: "unoccupied destination funded", balance: 76_000, accepted: true},
		{name: "existing obligations exceed floor", reserve: 80_000, balance: 80_999},
		{name: "existing obligations funded", reserve: 80_000, balance: 81_000, accepted: true},
	}
	for _, test := range cases {
		t.Run(test.name, func(t *testing.T) {
			in := inputFixture(t)
			in.ReservePence = 75_000
			in.Catalog.Ports[1].BankingRegion = "north_sea"
			in.Balances["north_sea"] = test.balance
			if test.reserve > 0 {
				in.ReserveByRegion["north_sea"] = test.reserve
			}
			plan := engine.PlanVoyages(in)[0]
			if plan.Accepted != test.accepted {
				t.Fatalf("candidate destination ignored its reserve floor or existing obligations: %+v", plan)
			}
		})
	}
}

func TestDestinationContributionProtectsFleetBillsWithoutChargingThem(t *testing.T) {
	t.Parallel()
	in := inputFixture(t)
	baseline := engine.PlanVoyages(in)[0]
	in.FleetDailyUpkeepPence = 100_000
	funded := engine.PlanVoyages(in)[0]
	if !baseline.Accepted || !funded.Accepted ||
		funded.Economics.Basis != "destination" ||
		funded.Quantity != baseline.Quantity ||
		funded.Economics.ExpectedNetPence != baseline.Economics.ExpectedNetPence ||
		funded.Economics.UpkeepPence != 0 {
		t.Fatalf("fixed fleet bills changed the voyage's operating contribution: baseline %+v funded %+v", baseline, funded)
	}
	in.Balances["channel"] = in.ReserveByRegion["channel"] + 5_000
	for _, plan := range engine.PlanVoyages(in) {
		if plan.Accepted {
			t.Fatalf("fixed fleet bills were omitted from protected cash: %+v", plan)
		}
	}
}

func TestBallastRepeatIsSeparateAndRequiresFreshSafeReturn(t *testing.T) {
	t.Parallel()
	in := inputFixture(t)
	control := engine.PlanVoyages(in)[0]
	if !control.Accepted || control.Economics.BallastRepeatGameDays <= control.Economics.GameDays ||
		control.Economics.BallastRepeatNetPence >= control.Economics.ExpectedNetPence {
		t.Fatalf("legal empty return lost its separate time and expense: %+v", control)
	}
	cases := []struct {
		name   string
		change func(*engine.PlanningInput)
	}{
		{"missing return", func(in *engine.PlanningInput) {
			delete(in.Routes, "portsmouth|le_havre|hoy")
		}},
		{"stale return", func(in *engine.PlanningInput) {
			meta := in.Metadata["routes/portsmouth|le_havre|hoy"]
			meta.Stale = true
			in.Metadata["routes/portsmouth|le_havre|hoy"] = meta
		}},
		{"unsafe return", func(in *engine.PlanningInput) {
			route := in.Routes["portsmouth|le_havre|hoy"]
			route.Legs[0].EdgeClass = "oceanic"
			in.Routes["portsmouth|le_havre|hoy"] = route
		}},
		{"unfunded return stores", func(in *engine.PlanningInput) {
			route := in.Routes["portsmouth|le_havre|hoy"]
			route.SafeProvisionsTons = 20
			in.Routes["portsmouth|le_havre|hoy"] = route
		}},
	}
	for _, test := range cases {
		t.Run(test.name, func(t *testing.T) {
			in := inputFixture(t)
			test.change(&in)
			plan := engine.PlanVoyages(in)[0]
			if !plan.Accepted || plan.Quantity != control.Quantity ||
				plan.Economics.ExpectedNetPence != control.Economics.ExpectedNetPence ||
				plan.Economics.GameDays != control.Economics.GameDays {
				t.Fatalf("unneeded return changed the delivery: %+v", plan)
			}
			if plan.Economics.BallastRepeatGameDays != 0 || plan.Economics.BallastRepeatNetPence != 0 {
				t.Fatalf("unsafe or unavailable return could underwrite repeat growth: %+v", plan.Economics)
			}
		})
	}
}

func TestBallastRepeatCrossRegionRestoresRecurringOriginCash(t *testing.T) {
	t.Parallel()
	freshInput := func() engine.PlanningInput {
		in := inputFixture(t)
		in.Ship.Stores.ProvisionsTons = 0
		in.Catalog.Ports[1].BankingRegion = "north_sea"
		in.Balances["north_sea"] = 1_000_000
		in.ReserveByRegion["north_sea"] = 75_000
		in.Metadata["rates"] = in.Metadata["vaults"]
		in.Rates = []engine.Rate{{FromRegion: "north_sea", ToRegion: "channel", FeeBPS: 1000}}
		return in
	}
	free := freshInput()
	free.Rates[0].FeeBPS = 0
	control := engine.PlanVoyages(free)[0]
	paid := engine.PlanVoyages(freshInput())[0]
	if !control.Accepted || !paid.Accepted || paid.Quantity != 22 ||
		paid.Economics.BallastRepeatGameDays <= 0 ||
		paid.Economics.ExpectedNetPence != control.Economics.ExpectedNetPence ||
		paid.Economics.BallastRepeatGameDays != control.Economics.BallastRepeatGameDays {
		t.Fatalf("recurring remittance changed the delivery or lost the funded repeat: free %+v paid %+v", control, paid)
	}
	// The loop consumes 67p of stores. Origin invoices total 500p; with the
	// 22t purchase and 572p customs, 23,139p net needs 25,710p gross at 10%.
	// The 2,052p initial whole-ton stores are inventory, not a recurring bill.
	if paid.Economics.StoreCashPence != 2052 ||
		control.Economics.BallastRepeatNetPence-paid.Economics.BallastRepeatNetPence != 2571 {
		t.Fatalf("repeat remittance did not charge only its observed replenishment fee: free %+v paid %+v", control.Economics, paid.Economics)
	}
	cases := []struct {
		name   string
		change func(*engine.PlanningInput)
	}{
		{"missing rates", func(in *engine.PlanningInput) {
			delete(in.Metadata, "rates")
		}},
		{"stale rates", func(in *engine.PlanningInput) {
			meta := in.Metadata["rates"]
			meta.Stale = true
			in.Metadata["rates"] = meta
		}},
		{"wrong direction", func(in *engine.PlanningInput) {
			in.Rates[0].FromRegion, in.Rates[0].ToRegion = "channel", "north_sea"
		}},
		{"unusable fee", func(in *engine.PlanningInput) {
			in.Rates[0].FeeBPS = 10_000
		}},
		{"delivery cannot replenish origin after fees", func(in *engine.PlanningInput) {
			reward := int64(1071)
			in.Contracts[0].RewardPerTonPence = &reward
		}},
	}
	for _, test := range cases {
		t.Run(test.name, func(t *testing.T) {
			in := freshInput()
			test.change(&in)
			plan := engine.PlanVoyages(in)[0]
			if !plan.Accepted {
				t.Fatalf("missing repeat funding incorrectly rejected a funded destination delivery: %+v", plan)
			}
			if plan.Economics.BallastRepeatGameDays != 0 || plan.Economics.BallastRepeatNetPence != 0 {
				t.Fatalf("repeat could drain the origin or invent a remittance fee: %+v", plan.Economics)
			}
		})
	}
}

func TestDestinationStoreCashDoesNotExpenseUnconsumedInventory(t *testing.T) {
	t.Parallel()
	in := inputFixture(t)
	in.Ship.Stores.ProvisionsTons = 0
	market := in.Markets["le_havre"]
	market.Books[0].Depth.Asks = []engine.Level{
		{PricePence: 1000, RemainingQuantity: 1},
		{PricePence: 2000, RemainingQuantity: 100},
	}
	in.Markets["le_havre"] = market
	plan := engine.PlanVoyages(in)[0]
	if !plan.Accepted || plan.StoresToBuy <= 0 {
		t.Fatalf("funded store purchase control was not executable: %+v", plan)
	}
	wantCash := plan.StoresToBuy*plan.StoresLimitPence +
		(plan.StoresToBuy*plan.StoresLimitPence*250+9_999)/10_000 + plan.StoresToBuy
	if plan.Economics.StoreCashPence != wantCash ||
		plan.Economics.ProvisionsPence <= 0 || plan.Economics.ProvisionsPence >= wantCash {
		t.Fatalf("cash for whole tons was confused with consumed provisions: %+v", plan)
	}
}

func TestCashLossPrecedesUnpricedPurchasesInEmptyHoldExplanation(t *testing.T) {
	t.Parallel()
	in := narrowMarginFixture(t)
	lossReward := int64(20_000)
	in.Contracts[0].RewardPerTonPence = &lossReward
	unpriced := in.Contracts[0]
	unpriced.ID = "unpriced-charter"
	unpriced.CommodityID = "silver_bullion"
	in.Contracts = append(in.Contracts, unpriced)

	plans := engine.PlanVoyages(in)
	best := plans[0]
	if best.Accepted || best.ContractID != "charter-a" || best.Economics.GameDays <= 0 ||
		best.Economics.ExpectedNetPence >= 0 {
		t.Fatalf("hold did not explain the best costed cash-loss alternative: %+v", best)
	}
	foundUnpriced := false
	for _, plan := range plans {
		if plan.Accepted {
			t.Fatalf("actual-cost loss was admitted: %+v", plan)
		}
		if plan.ContractID == unpriced.ID {
			foundUnpriced = true
		}
	}
	if !foundUnpriced {
		t.Fatal("unpriced public charter was omitted from the hold explanation")
	}
}

func TestPlannerEqualContributionTiesAreDeterministic(t *testing.T) {
	t.Parallel()
	in := inputFixture(t)
	second := in.Contracts[0]
	second.ID = "charter-b"
	first := in.Contracts[0]
	in.Contracts = []engine.Contract{second, first}
	reversed := engine.PlanVoyages(in)[0]
	in.Contracts = []engine.Contract{first, second}
	ordered := engine.PlanVoyages(in)[0]
	if !ordered.Accepted || !reversed.Accepted ||
		ordered.ContractID != first.ID || reversed.ContractID != ordered.ContractID ||
		ordered.Quantity != reversed.Quantity {
		t.Fatalf("equal delivery contributions depend on listing order: reversed %+v ordered %+v", reversed, ordered)
	}
}

func TestPlannerChoosesProfitableDepthBoundaryNotFullHold(t *testing.T) {
	t.Parallel()
	in := inputFixture(t)
	market := in.Markets["le_havre"]
	market.Books[0].Depth.Asks = []engine.Level{{PricePence: 1000, RemainingQuantity: 10}, {PricePence: 5000, RemainingQuantity: 100}}
	in.Markets["le_havre"] = market
	plans := engine.PlanVoyages(in)
	if !plans[0].Accepted || plans[0].Quantity != 10 || plans[0].BuyLimitPence != 1000 {
		t.Fatalf("profitable ladder boundary missed: %+v", plans[0])
	}
}

func TestPlannerRejectsUnsafeOrUnfundedVoyages(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name   string
		change func(*engine.PlanningInput)
		want   string
	}{
		{"under-rated edge", func(in *engine.PlanningInput) {
			route := in.Routes["le_havre|portsmouth|hoy"]
			route.Legs[0].EdgeClass = "regional"
			in.Routes["le_havre|portsmouth|hoy"] = route
		}, "below route edge"},
		{"under-rated zone", func(in *engine.PlanningInput) {
			in.Catalog.SeaZones[0].ZoneClass = "oceanic"
		}, "below zone"},
		{"unknown Sound toll", func(in *engine.PlanningInput) {
			route := in.Routes["le_havre|portsmouth|hoy"]
			route.Legs[0].IsSoundToll = true
			in.Routes["le_havre|portsmouth|hoy"] = route
		}, "Sound toll"},
		{"protected local reserve", func(in *engine.PlanningInput) {
			in.Balances["channel"] = 75_000
		}, "reserve"},
		{"failed local book", func(in *engine.PlanningInput) {
			meta := in.Metadata["books/le_havre"]
			meta.Error = "upstream unavailable"
			meta.Stale = true
			in.Metadata["books/le_havre"] = meta
		}, "not fresh"},
		{"incomplete pagination", func(in *engine.PlanningInput) {
			meta := in.Metadata["contracts"]
			meta.Complete = false
			in.Metadata["contracts"] = meta
		}, "contracts"},
		{"prior sync generation", func(in *engine.PlanningInput) {
			meta := in.Metadata["vaults"]
			meta.Generation = "old-sync"
			in.Metadata["vaults"] = meta
		}, "vaults"},
		{"impossible deadline", func(in *engine.PlanningInput) {
			deadline := in.Now.Add(time.Second)
			in.Contracts[0].ExpiresAt = &deadline
		}, "deadline"},
	}
	for _, test := range cases {
		t.Run(test.name, func(t *testing.T) {
			in := inputFixture(t)
			test.change(&in)
			plans := engine.PlanVoyages(in)
			found := false
			for _, plan := range plans {
				if plan.Accepted {
					t.Fatalf("unsafe candidate accepted: %+v", plan)
				}
				found = found || strings.Contains(plan.Reason, test.want)
			}
			if !found {
				t.Fatalf("missing actionable rejection %q: %+v", test.want, plans)
			}
		})
	}
}

func TestFreightCompetesAndCompatibleMixedHoldKeepsSealSeparate(t *testing.T) {
	t.Parallel()
	in := inputFixture(t)
	in.Contracts[0].RemainingQuantity = 10
	freight := fixture[engine.Contract](t, `{"id":"freight-a","contract_type":"freight","origin_port_id":"le_havre","dest_port_id":"portsmouth","commodity_id":"sawn_planks","quantity_required":4,"remaining_quantity":4,"reward_pence":20000,"collateral_pence":5000,"status":"open","stale":false,"issuer_company_id":"other-company"}`)
	in.Contracts = append(in.Contracts, freight)
	plans := engine.PlanVoyages(in)
	if !plans[0].Accepted || plans[0].Kind != "charter+freight" || plans[0].Quantity != 10 || len(plans[0].FreightIDs) != 1 || plans[0].Economics.CollateralPence != 5000 {
		t.Fatalf("compatible mixed opportunity was not compared: %+v", plans)
	}
	if plans[0].Economics.RevenuePence != 50_000 {
		t.Fatal("collateral principal was counted as income")
	}
}

func TestMixedManifestSizesDemandHoldDepthAndCashIndependently(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name      string
		demand    int64
		hold      int64
		balance   int64
		asks      []engine.Level
		wantCargo int64
	}{
		{
			name: "charter demand leaves spare hold", demand: 10, hold: 24, balance: 1_000_000,
			asks: []engine.Level{{PricePence: 1000, RemainingQuantity: 100}}, wantCargo: 10,
		},
		{
			name: "freight consumes actual free hold", demand: 10, hold: 12, balance: 1_000_000,
			asks: []engine.Level{{PricePence: 1000, RemainingQuantity: 100}}, wantCargo: 8,
		},
		{
			name: "interior depth optimum", demand: 14, hold: 24, balance: 1_000_000,
			asks:      []engine.Level{{PricePence: 1000, RemainingQuantity: 10}, {PricePence: 5000, RemainingQuantity: 100}},
			wantCargo: 10,
		},
		{
			name: "cash preserves collateral and reserve", demand: 20, hold: 24, balance: 90_000,
			asks: []engine.Level{{PricePence: 1000, RemainingQuantity: 100}}, wantCargo: 9,
		},
	}
	for _, test := range cases {
		t.Run(test.name, func(t *testing.T) {
			in := inputFixture(t)
			in.Contracts[0].RemainingQuantity = test.demand
			in.Ship.EffectiveHoldCapacityTons = test.hold
			in.Balances["channel"] = test.balance
			market := in.Markets["le_havre"]
			market.Books[0].Depth.Asks = test.asks
			in.Markets["le_havre"] = market
			freight := engine.Contract{
				ID: "fixed-freight", ContractType: "freight", OriginPortID: "le_havre", DestPortID: "portsmouth",
				CommodityID: "sawn_planks", QuantityRequired: 4, RemainingQuantity: 4,
				RewardPence: 20_000, CollateralPence: 5_000, Status: "open", IssuerCompanyID: "other-company",
			}
			in.Contracts = append(in.Contracts, freight)
			plan := engine.PlanVoyages(in)[0]
			if !plan.Accepted || plan.Kind != "charter+freight" || plan.Quantity != test.wantCargo ||
				len(plan.FreightIDs) != 1 || plan.FreightIDs[0] != freight.ID ||
				plan.Economics.CollateralPence != freight.CollateralPence {
				t.Fatalf("joint quantity optimization confused independent limits: %+v", plan)
			}
			if plan.Quantity > test.demand ||
				plan.Quantity+freight.QuantityRequired > in.Ship.EffectiveHoldCapacityTons-in.Ship.CargoUsedTons ||
				plan.Economics.RevenuePence != plan.Quantity**in.Contracts[0].RewardPerTonPence+freight.RewardPence {
				t.Fatalf("manifest changed demand, sealed tons or contract reward: %+v", plan)
			}
		})
	}
}

func TestOwnRestingOrdersCannotCreateProfit(t *testing.T) {
	t.Parallel()
	in := inputFixture(t)
	in.OwnedOrderIDs = map[string]bool{"npc-1": true}
	for _, plan := range engine.PlanVoyages(in) {
		if plan.Accepted {
			t.Fatalf("own-player liquidity was counted as executable: %+v", plan)
		}
	}
}

func TestRemittanceGrossCoversActualFeeAndRejectsInvalidRates(t *testing.T) {
	t.Parallel()
	for _, fee := range []int64{0, 50, 150, 600, 9999} {
		gross, err := engine.GrossRemittance(12_345, fee)
		if err != nil {
			t.Fatal(err)
		}
		conservativeFee := (gross*fee + 9999) / 10_000
		if gross-conservativeFee < 12_345 {
			t.Fatalf("destination underfunded for %d bps: %d", fee, gross)
		}
	}
	if _, err := engine.GrossRemittance(100, 10_000); err == nil {
		t.Fatal("non-remittable fee accepted")
	}
}

func TestUpkeepChargesMarginalFleetAndHalfOceanicTons(t *testing.T) {
	t.Parallel()
	in := inputFixture(t)
	if got := engine.DailyUpkeep([]engine.ShipState{in.Ship}, in.Catalog, 0); got != 0 {
		t.Fatalf("first 40 coastal tons should be free: %d", got)
	}
	if got := engine.DailyUpkeep([]engine.ShipState{in.Ship, in.Ship}, in.Catalog, 0); got <= 0 {
		t.Fatal("second hull's nonlinear sprawl omitted")
	}
	ocean := engine.Hull{ID: "barque", HullCategory: "oceanic", HoldCapacityTons: 220}
	in.Catalog.ShipClasses = append(in.Catalog.ShipClasses, ocean)
	ship := in.Ship
	ship.Ship.ShipClass = "barque"
	// 40 coastal tons plus half of 220 oceanic tons leave 110 sprawl tons.
	if got := engine.DailyUpkeep([]engine.ShipState{in.Ship, ship}, in.Catalog, 0); got != 22_000+5_167 {
		t.Fatalf("half the oceanic tonnage did not enter sprawl: %d", got)
	}
}

func TestDailyUpkeepFactorExponent(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		factors int
		want    int64
	}{
		{0, 0},
		{1, 12_000},
		{2, 28_541},
		{4, 67_883},
		{8, 161_453},
	} {
		if got := engine.DailyUpkeep(nil, engine.Catalog{}, tc.factors); got != tc.want {
			t.Errorf("DailyUpkeep with %d factors = %dp, want %dp", tc.factors, got, tc.want)
		}
	}
}

func TestGrowthRequiresActualSettlementsSpareDepthAndFundedPayback(t *testing.T) {
	t.Parallel()
	price := int64(100_000)
	base := engine.GrowthInput{
		Plan:         engine.Plan{Accepted: true, Quantity: 20, Economics: engine.Economics{PurchasePence: 10_000, GameDays: 1, NetPencePerGameDay: 100_000}},
		Quote:        engine.YardQuote{Available: true, BuildMode: "turnkey_cash", TotalCostPence: &price},
		Hull:         engine.Hull{HoldCapacityTons: 40, MaxCannons: 4},
		Proofs:       []engine.LaneProof{{QuantityTons: 20, GameSeconds: 86_400, NetPence: 100_000, CostsKnown: true}, {QuantityTons: 20, GameSeconds: 86_400, NetPence: 100_000, CostsKnown: true}},
		BalancePence: 1_000_000, ReservePence: 75_000,
		ExistingWorkingCapitalPence: 20_000, ExistingCycleTons: 20,
		ObservedAskCapacityTons: 200, ContractRemainingTons: 1000,
		IncrementalDailyUpkeepPence: 1000, InitialStoresPence: 5000, MaxPaybackDays: 12,
	}
	if decision := engine.EvaluateGrowth(base); !decision.Accepted || decision.PaybackGameDays <= 0 || decision.SlippageReservePence != 10_000 {
		t.Fatalf("funded proven lane rejected: %+v", decision)
	}
	unknown := base
	unknown.Proofs = []engine.LaneProof{{QuantityTons: 20, GameSeconds: 86_400, NetPence: 100_000, CostsKnown: false}}
	if engine.EvaluateGrowth(unknown).Accepted {
		t.Fatal("unknown cost basis funded fleet growth")
	}
	crowded := base
	crowded.ObservedAskCapacityTons = 60
	if engine.EvaluateGrowth(crowded).Accepted {
		t.Fatal("existing fleet's measured liquidity was allocated twice")
	}
	underfunded := base
	underfunded.BalancePence = 200_000
	if engine.EvaluateGrowth(underfunded).Accepted {
		t.Fatal("growth spent protected working capital")
	}
	expensive := base
	expensive.IncrementalDailyUpkeepPence = 100_000
	if engine.EvaluateGrowth(expensive).Accepted {
		t.Fatal("upkeep-negative growth accepted")
	}
	ending := base
	daysRemaining := 0.5
	ending.ContractGameDaysRemaining = &daysRemaining
	if engine.EvaluateGrowth(ending).Accepted {
		t.Fatal("growth relied on a charter ending before payback")
	}
	spot := base
	spot.Plan.Kind = "spot"
	spot.ContractRemainingTons = -1
	if engine.EvaluateGrowth(spot).Accepted {
		t.Fatal("spot growth inferred destination capacity from an unmeasured book")
	}
	spot.ObservedBidCapacityTons = 200
	if !engine.EvaluateGrowth(spot).Accepted {
		t.Fatal("fully measured profitable spot capacity was rejected")
	}
	losses := base
	losses.Proofs = append(append([]engine.LaneProof(nil), base.Proofs...), engine.LaneProof{
		GameSeconds: 86_400,
		NetPence:    -500_000,
		CostsKnown:  true,
	})
	if engine.EvaluateGrowth(losses).Accepted {
		t.Fatal("growth selected profitable winners and ignored actual lane losses")
	}
	unknownLoss := base
	unknownLoss.Proofs = append(append([]engine.LaneProof(nil), base.Proofs...), engine.LaneProof{
		GameSeconds: 86_400,
		CostsKnown:  false,
	})
	if engine.EvaluateGrowth(unknownLoss).Accepted {
		t.Fatal("growth treated an unresolved capital loss as zero cost")
	}
}

func minimumMarginInput(t *testing.T, kind string, net int64) engine.PlanningInput {
	t.Helper()
	in := inputFixture(t)
	in.Catalog.ShipClasses[0].CrewRequired = 0
	in.Ship.EffectiveHoldCapacityTons = 2
	book := in.Markets["le_havre"]
	book.CustomsBPS = 0
	book.Books[0].Depth.Asks[0].PricePence = 99_999
	in.Markets["le_havre"] = book
	for id, detail := range in.Ports {
		detail.BerthDuesPerDayPence = 0
		in.Ports[id] = detail
	}
	revenue := int64(100_000) + net
	if strings.Contains(kind, "+freight") {
		revenue -= 1_000
	}
	in.Contracts[0].RewardPerTonPence = &revenue
	in.Contracts[0].RemainingQuantity = 1
	if strings.HasPrefix(kind, "spot") {
		in.Contracts = nil
		destination := fixture[engine.Market](t, `{"port_id":"portsmouth","visibility":"quay","books":[{"commodity_id":"provisions","depth":{"bids":[{"price_pence":1,"remaining_quantity":1}]}}]}`)
		destination.Books[0].Depth.Bids[0].PricePence = revenue
		in.Markets["portsmouth"] = destination
		in.Metadata["books/portsmouth"] = in.Metadata["books/le_havre"]
	}
	if strings.Contains(kind, "freight") {
		freight := engine.Contract{
			ID: "margin-freight", ContractType: "freight", OriginPortID: "le_havre", DestPortID: "portsmouth",
			CommodityID: "grain", QuantityRequired: 1, RewardPence: 1_000, Status: "open", IssuerCompanyID: "other",
		}
		if kind == "freight" {
			freight.RewardPence = net
			in.Contracts = nil
		}
		in.Contracts = append(in.Contracts, freight)
	}
	return in
}

func TestMinimumMarginBoundaryTable(t *testing.T) {
	for _, kind := range []string{"charter", "spot", "charter+freight", "spot+freight", "freight"} {
		for _, net := range []int64{1_999, 2_000} {
			in := minimumMarginInput(t, kind, net)
			var found bool
			for _, plan := range engine.PlanVoyages(in) {
				if plan.Kind != kind {
					continue
				}
				found = true
				accepted := net >= 2_000 || kind == "freight"
				if plan.Accepted != accepted || plan.Economics.ExpectedNetPence != net {
					t.Fatalf("%s net %dp: %+v", kind, net, plan)
				}
				if kind != "freight" && plan.Economics.PurchasePence+plan.Economics.CustomsPence != 100_000 {
					t.Fatalf("%s did not use the purchase-plus-customs denominator: %+v", kind, plan)
				}
				if !accepted && plan.Reason != "below 2% minimum margin on purchase plus customs" {
					t.Fatalf("%s rejection = %q", kind, plan.Reason)
				}
			}
			if !found {
				t.Fatalf("missing %s margin candidate", kind)
			}
		}
	}
	in := minimumMarginInput(t, "freight", 1)
	if plan := engine.PlanVoyages(in)[0]; !plan.Accepted || plan.Economics.ExpectedNetPence != 1 {
		t.Fatalf("freight-only one-penny contribution rejected: %+v", plan)
	}
}

func TestMinimumMarginAppliesToGrowthAndOneTonProbes(t *testing.T) {
	for _, kind := range []string{"growth", "probe"} {
		for _, net := range []int64{1_999, 2_000} {
			in := minimumMarginInput(t, "spot", net)
			if kind == "growth" {
				in.Fleet = []engine.ShipState{in.Ship}
				in.Ship.Ship.ID = "growth-candidate"
			} else {
				delete(in.Markets, "portsmouth")
				delete(in.Metadata, "books/portsmouth")
				detail := in.Ports["le_havre"]
				bid := int64(100_000) + net
				detail.CoffeeHouseGossip = []engine.Gossip{{
					SubjectPortID: "portsmouth", SnapshotAt: in.Now,
					Commodities: []engine.GossipCommodity{{CommodityID: "provisions", SnapshotAt: in.Now, BestBidPence: &bid}},
				}}
				in.Ports["le_havre"] = detail
			}
			plan := engine.PlanVoyages(in)[0]
			if plan.Accepted != (net >= 2_000) || plan.Economics.ExpectedNetPence != net || plan.Quantity != 1 {
				t.Fatalf("%s net %dp bypassed minimum margin: %+v", kind, net, plan)
			}
		}
	}
}

func TestMinimumMarginRetainedPlansAlsoRequireStressFloor(t *testing.T) {
	for _, net := range []int64{2_000, 4_000} {
		in := minimumMarginInput(t, "spot", net)
		delete(in.Markets, "portsmouth")
		delete(in.Metadata, "books/portsmouth")
		in.QuayBookMemory = map[string]engine.RetainedQuayBook{"portsmouth": {
			ObservedAt: in.Now,
			Bids:       map[string][]engine.RetainedBid{"provisions": {{PricePence: 100_000 + net, RemainingQuantity: 1}}},
		}}
		plan := engine.PlanVoyages(in)[0]
		if plan.Accepted != (net == 4_000) || plan.Economics.ExpectedNetPence != net {
			t.Fatalf("retained net %dp did not enforce both independent gates: %+v", net, plan)
		}
	}
}

func TestMinimumMarginUsesNetAfterFundingFees(t *testing.T) {
	for _, net := range []int64{1_999, 2_000} {
		in := minimumMarginInput(t, "charter", 10_000)
		in.Balances["channel"] = 75_000
		in.Balances["donor"] = 1_000_000
		in.Rates = []engine.Rate{{FromRegion: "donor", ToRegion: "channel", FeeBPS: 100}}
		in.Metadata["rates"] = in.Metadata["vaults"]
		baseline := engine.PlanVoyages(in)[0]
		if !baseline.Accepted || baseline.Economics.FundingFeesPence <= 0 {
			t.Fatalf("fixture lacks a measured donor fee: %+v", baseline)
		}
		reward := int64(100_000) + baseline.Economics.FundingFeesPence + net
		in.Contracts[0].RewardPerTonPence = &reward
		plan := engine.PlanVoyages(in)[0]
		if plan.Accepted != (net >= 2_000) || plan.Economics.ExpectedNetPence != net {
			t.Fatalf("post-funding net %dp bypassed minimum margin: %+v", net, plan)
		}
	}
}

func TestMinimumMarginUsesHazardAdjustedNet(t *testing.T) {
	for _, kind := range []string{"charter", "spot", "charter+freight", "spot+freight"} {
		for _, net := range []int64{1_999, 2_000} {
			in := minimumMarginInput(t, kind, net+3_510)
			quote := in.HazardQuotes["hull|ship-a|le_havre|portsmouth"]
			quote.ExpectedLossBPS = 351
			in.HazardQuotes["hull|ship-a|le_havre|portsmouth"] = quote
			found := false
			for _, plan := range engine.PlanVoyages(in) {
				if plan.Kind != kind {
					continue
				}
				found = true
				if plan.Accepted != (net >= 2_000) || plan.Economics.ExpectedNetPence != net || plan.Economics.HazardPence != 3_510 {
					t.Fatalf("%s post-hazard net %dp bypassed minimum margin: %+v", kind, net, plan)
				}
			}
			if !found {
				t.Fatalf("missing %s hazard-priced margin candidate", kind)
			}
		}
	}
}
