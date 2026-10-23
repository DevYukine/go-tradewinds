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
		Markets:   map[string]engine.Market{"le_havre": book},
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
		t.Fatalf("customs, stores or repeatable return cycle omitted: %+v", plan.Economics)
	}
	if plan.Economics.ProbabilityEstimate != nil || plan.Economics.StressHaircutPence <= 0 {
		t.Fatal("diagnostic stress was missing or presented as measured hazard probability")
	}
}

func narrowMarginFixture(t *testing.T) engine.PlanningInput {
	t.Helper()
	in := inputFixture(t)
	in.Catalog.ShipClasses[0].BaseSpeedCentiknots = 850
	in.Catalog.SeaZones[0].BaseStormBPS = 500
	in.Catalog.SeaZones[0].BasePiracyBPS = 400
	in.Zones[0].NetPiracyBPS = 400
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
				cash.ProvisionsPence - cash.BerthDuesPence - cash.UpkeepPence - cash.TollPence
			if cash.ExpectedNetPence != wantCash || cash.UpkeepPence <= 0 ||
				cash.ProvisionsPence <= 0 || cash.BerthDuesPence <= 0 {
				t.Fatalf("cash net is not revenue less observable nonrefundable costs: %+v; identity %dp", cash, wantCash)
			}
			if kind == "freight" && cash.CollateralPence != in.Contracts[0].CollateralPence {
				t.Fatalf("refundable collateral disappeared instead of remaining separately funded: %+v", cash)
			}
			in.Catalog.SeaZones[0].BaseStormBPS = 8_000
			in.Zones[0].NetPiracyBPS = 8_000
			stressed := engine.PlanVoyages(in)[0]
			diagnostic := stressed.Economics
			if !stressed.Accepted || stressed.Kind != calm.Kind || stressed.Quantity != calm.Quantity ||
				diagnostic.ExpectedNetPence != cash.ExpectedNetPence ||
				diagnostic.NetPencePerGameDay != cash.NetPencePerGameDay {
				t.Fatalf("uncalibrated hazards changed cash acceptance, lot size or ranking: calm %+v stressed %+v", calm, stressed)
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

func TestRoundTripTurnaroundMatchesBerthLocks(t *testing.T) {
	in := narrowMarginFixture(t)
	plan := engine.PlanVoyages(in)[0]
	if !plan.Accepted {
		t.Fatal("turnaround comparison requires an accepted charter")
	}
	hull := in.Catalog.ShipClasses[0]
	outward := in.Routes["le_havre|portsmouth|hoy"]
	back := in.Routes["portsmouth|le_havre|hoy"]
	destination := in.Ports["portsmouth"]
	sailingDays := float64(outward.TotalDistanceNM+back.TotalDistanceNM) / (float64(hull.BaseSpeedCentiknots) / 100 * 0.8) / 24
	berthLockDays := 60.0 / 1440
	handlingDays := float64(plan.Quantity*4) / 1440
	queueDays := float64(destination.QueuedShips+1) / float64(destination.EffectiveBerths) * 0.25
	want := sailingDays + berthLockDays + handlingDays + queueDays
	if math.Abs(plan.Economics.GameDays-want) > 1e-9 {
		t.Fatalf("round-trip duration %.9f days, want %.9f (sailing %.9f + two 30min berth locks %.9f + handling %.9f + queue %.9f)", plan.Economics.GameDays, want, sailingDays, berthLockDays, handlingDays, queueDays)
	}
}

func TestCashLossPrecedesUnpricedPurchasesInEmptyHoldExplanation(t *testing.T) {
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
		best.Economics.ExpectedNetPence >= 0 || !strings.Contains(best.Reason, "non-positive expected cash net") {
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
		if plan.ContractID == unpriced.ID &&
			(!strings.Contains(plan.Reason, "purchase of") ||
				!strings.Contains(plan.Reason, "silver_bullion at le_havre") ||
				strings.Contains(plan.Reason, "cargo has")) {
			t.Fatalf("empty hold was blamed for an unavailable future purchase: %+v", plan)
		}
	}
	if !foundUnpriced {
		t.Fatal("unpriced public charter was omitted from the hold explanation")
	}
}

func TestPlannerChoosesProfitableDepthBoundaryNotFullHold(t *testing.T) {
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

func TestOwnRestingOrdersCannotCreateProfit(t *testing.T) {
	in := inputFixture(t)
	in.OwnedOrderIDs = map[string]bool{"npc-1": true}
	for _, plan := range engine.PlanVoyages(in) {
		if plan.Accepted {
			t.Fatalf("own-player liquidity was counted as executable: %+v", plan)
		}
	}
}

func TestRemittanceGrossCoversActualFeeAndRejectsInvalidRates(t *testing.T) {
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

func TestUpkeepChargesOnlyMarginalFleetAndSeparateOceanicTons(t *testing.T) {
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
	if got := engine.DailyUpkeep([]engine.ShipState{in.Ship, ship}, in.Catalog, 0); got != 22_000 {
		t.Fatalf("oceanic tons incorrectly entered sprawl: %d", got)
	}
}

func TestGrowthRequiresActualSettlementsSpareDepthAndFundedPayback(t *testing.T) {
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
