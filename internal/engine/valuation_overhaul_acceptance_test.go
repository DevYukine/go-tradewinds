package engine

// Acceptance contracts for the E2 valuation overhaul (planner.go/freight.go):
// destination-ending voyages are valued and admitted at their real delivery
// boundary. Ranking compares those admitted deliveries with a separately recorded
// continuation, never by hiding its duration or costs in the outward economics.
// A whole-ton store top-up stays a cash funding requirement while only the
// consumed share becomes voyage expense, and same-destination cargo and sealed
// freight are sized jointly before the shared-cost profitability gate.
//
// Every failing assertion names the structural bug it exposes, and each case
// carries its causal control: funded/underfunded pairs, standalone-loss
// evidence and absent-demand rejection. No scenario assumes that observed
// depth or prices always yield profit.

import (
	"encoding/json"
	"fmt"
	"math"
	"testing"
	"time"
)

func valuationFixture[T any](t *testing.T, raw string) T {
	t.Helper()
	var value T
	if err := json.Unmarshal([]byte(raw), &value); err != nil {
		t.Fatal(err)
	}
	return value
}

func valuationMeta(now time.Time) DatasetMeta {
	return DatasetMeta{UpdatedAt: now, AttemptedAt: now, Generation: "valuation-sync", Complete: true}
}

// valuationRoute models a coastal passage through the one observed zone; the
// return direction is a separate route whose distance the scenarios mutate.
func valuationRoute(from, to string, distanceNM int64) Route {
	half := distanceNM / 2
	return Route{
		FromPort: from, ToPort: to, ShipClass: "hoy",
		TotalDistanceNM:    distanceNM,
		SafeProvisionsTons: 0.1,
		Legs: []RouteLeg{
			{FromNode: from, ToNode: "english_channel", DistanceNM: half, EdgeClass: "coastal"},
			{FromNode: "english_channel", ToNode: to, DistanceNM: distanceNM - half, EdgeClass: "coastal"},
		},
	}
}

func valuationDestinationBook(t *testing.T, port string, bidPence, bidTons int64) Market {
	t.Helper()
	return valuationFixture[Market](t, fmt.Sprintf(`{
		"port_id":%q,"visibility":"quay","customs_bps":250,
		"books":[{"commodity_id":"grain","depth":{"asks":[],"bids":[{"order_id":"bid-%s","price_pence":%d,"remaining_quantity":%d}]}}]
	}`, port, port, bidPence, bidTons))
}

// valuationInput builds one berthed hoy at le_havre with two unlocked channel
// destinations: portsmouth 30nm out with a 120nm open return, dunkirk 120nm
// out with a 30nm return. All datasets are fresh; no contracts exist.
func valuationInput(t *testing.T) PlanningInput {
	t.Helper()
	now := time.Date(2026, 10, 22, 17, 0, 0, 0, time.UTC)
	in := PlanningInput{
		Now:       now,
		CompanyID: "valuation-company",
		Catalog: valuationFixture[Catalog](t, `{
			"ports":[
				{"id":"le_havre","banking_region":"channel","sea_zone_id":"english_channel","phase":1,"has_shipyard":true,"manufactures":["provisions"]},
				{"id":"portsmouth","banking_region":"channel","sea_zone_id":"english_channel","phase":1,"imports":["grain","provisions"]},
				{"id":"dunkirk","banking_region":"channel","sea_zone_id":"english_channel","phase":1,"imports":["grain","provisions"]}
			],
			"ship_classes":[{"id":"hoy","hull_category":"coastal","phase":1,"base_speed_centiknots":600,"hold_capacity_tons":40,"max_cannons":4,"crew_required":10}],
			"sea_zones":[{"id":"english_channel","zone_class":"coastal","base_storm_bps":200,"base_piracy_bps":200}]
		}`),
		Ship: valuationFixture[ShipState](t, `{
			"ship":{"id":"ship-valuation","name":"Lapras","ship_class":"hoy","status":"berthed","current_port_id":"le_havre","cannons_count":4},
			"hold_capacity_tons":40,"effective_hold_capacity_tons":24,"cargo_used_tons":0,
			"stores":{"provisions_tons":8},"cargo_manifest":{},"route_legs":[]
		}`),
		Clock: valuationFixture[Clock](t, `{"unlocked_phase":1,"paused":false,"calendar":{"month":3,"day":5,"time_scale":60,"total_game_hours":118}}`),
		Zones: []ZoneObservation{{SeaZoneID: "english_channel", NetPiracyBPS: 200}},
		Markets: map[string]Market{
			"le_havre": valuationFixture[Market](t, `{
				"port_id":"le_havre","visibility":"quay","customs_bps":250,
				"books":[
					{"commodity_id":"provisions","depth":{"asks":[{"order_id":"npc-provisions","price_pence":1000,"remaining_quantity":100}],"bids":[]}},
					{"commodity_id":"grain","depth":{"asks":[{"order_id":"npc-grain","price_pence":1000,"remaining_quantity":100}],"bids":[]}}
				]
			}`),
			"portsmouth": valuationDestinationBook(t, "portsmouth", 4000, 100),
			"dunkirk":    valuationDestinationBook(t, "dunkirk", 4100, 100),
		},
		Ports: map[string]PortDetail{
			"le_havre":   {ID: "le_havre", BerthDuesPerDayPence: 1000, EffectiveBerths: 4},
			"portsmouth": {ID: "portsmouth", BerthDuesPerDayPence: 1000, EffectiveBerths: 4},
			"dunkirk":    {ID: "dunkirk", BerthDuesPerDayPence: 1000, EffectiveBerths: 4},
		},
		Routes: map[string]Route{
			"le_havre|portsmouth|hoy": valuationRoute("le_havre", "portsmouth", 30),
			"portsmouth|le_havre|hoy": valuationRoute("portsmouth", "le_havre", 120),
			"le_havre|dunkirk|hoy":    valuationRoute("le_havre", "dunkirk", 120),
			"dunkirk|le_havre|hoy":    valuationRoute("dunkirk", "le_havre", 30),
		},
		Balances:        map[string]int64{"channel": 500_000},
		ReserveByRegion: map[string]int64{"channel": 75_000},
		Generation:      "valuation-sync",
	}
	in.Clock.RealUTC = now
	in.Metadata = map[string]DatasetMeta{}
	for _, key := range []string{
		"catalog", "clock", "zones", "vaults", "contracts", "ships",
		"books/le_havre", "books/portsmouth", "books/dunkirk",
		"ports/le_havre", "ports/portsmouth", "ports/dunkirk",
		"routes/le_havre|portsmouth|hoy", "routes/portsmouth|le_havre|hoy",
		"routes/le_havre|dunkirk|hoy", "routes/dunkirk|le_havre|hoy",
	} {
		in.Metadata[key] = valuationMeta(now)
	}
	seedZeroHazardQuotes(&in)
	return in
}

// valuationScopePortsmouth removes dunkirk so a competing far destination
// cannot mask the scenario under test.
func valuationScopePortsmouth(in *PlanningInput) {
	in.Catalog.Ports = in.Catalog.Ports[:2]
}

func valuationPlanFor(t *testing.T, plans []Plan, destination string) Plan {
	t.Helper()
	for _, plan := range plans {
		if plan.Destination == destination {
			return plan
		}
	}
	t.Fatalf("no candidate plan for destination %s: %+v", destination, plans)
	return Plan{}
}

// Changing the empty return leg affects only the continuation and combined
// rank score, never the outbound delivery's duration, expense or arrival.
func TestValuationDeliveryBoundaryIgnoresUnneededReturnLeg(t *testing.T) {
	t.Parallel()
	in := valuationInput(t)
	basePlans := PlanVoyages(in)
	before := valuationPlanFor(t, basePlans, "dunkirk")
	if !before.Accepted || before.Economics.ExpectedNetPence <= 0 {
		t.Fatalf("profitable delivery control was not accepted: %+v", before)
	}
	route := in.Routes["dunkirk|le_havre|hoy"]
	route.TotalDistanceNM = 300
	for i := range route.Legs {
		route.Legs[i].DistanceNM = 150
	}
	in.Routes["dunkirk|le_havre|hoy"] = route
	stretched := PlanVoyages(in)
	after := valuationPlanFor(t, stretched, "dunkirk")
	if !after.Accepted || after.Economics.ExpectedNetPence <= 0 {
		t.Fatalf("legal outbound delivery was gated on its empty return leg: %+v", after)
	}
	if after.Quantity != before.Quantity {
		t.Fatalf("return distance changed the delivery lot: before %dt after %dt", before.Quantity, after.Quantity)
	}
	if math.Abs(after.Economics.GameDays-before.Economics.GameDays) > 1e-9 {
		t.Fatalf("delivery duration %.9f charges %.9f phantom return days (delivery-boundary is %.9f)",
			after.Economics.GameDays, after.Economics.GameDays-before.Economics.GameDays, before.Economics.GameDays)
	}
	if after.Economics.ExpectedNetPence != before.Economics.ExpectedNetPence ||
		after.Economics.NetPencePerGameDay != before.Economics.NetPencePerGameDay {
		t.Fatalf("return distance changed delivery economics: before %+v after %+v",
			before.Economics, after.Economics)
	}
	if !after.ExpectedArrival.Equal(before.ExpectedArrival) {
		t.Fatalf("arrival at the delivery port moved with the return leg: before %s after %s",
			before.ExpectedArrival, after.ExpectedArrival)
	}
	if before.Continuation == nil || after.Continuation == nil {
		t.Fatalf("both deliveries must record their separate continuation: before %+v after %+v", before, after)
	}
	if after.Continuation.GameDays <= before.Continuation.GameDays ||
		after.Economics.RankScorePencePerGameDay >= before.Economics.RankScorePencePerGameDay {
		t.Fatalf("longer empty return did not reduce the combined rank: before %+v after %+v", before, after)
	}
}

// The nearer delivery retains its better one-way rate, but the farther
// delivery wins the two-leg rank because its return is much shorter.
func TestValuationRanksContinuationWithoutChangingDeliveryEconomics(t *testing.T) {
	t.Parallel()
	in := valuationInput(t)
	plans := PlanVoyages(in)
	near := valuationPlanFor(t, plans, "portsmouth")
	far := valuationPlanFor(t, plans, "dunkirk")
	if !near.Accepted || !far.Accepted {
		t.Fatalf("both funded deliveries must stay costed controls: near %+v far %+v", near, far)
	}
	if plans[0].Destination != "dunkirk" {
		t.Fatalf("continuation rank ignored the shorter return: first %+v", plans[0])
	}
	if near.Economics.GameDays >= far.Economics.GameDays {
		t.Fatalf("delivery durations ignore the one-way distance ratio: near %.9f far %.9f",
			near.Economics.GameDays, far.Economics.GameDays)
	}
	if near.Economics.NetPencePerGameDay <= far.Economics.NetPencePerGameDay {
		t.Fatalf("per-day contribution contradicts one-way delivery economics: near %+v far %+v",
			near.Economics, far.Economics)
	}
	for _, plan := range []Plan{near, far} {
		if plan.Continuation == nil {
			t.Fatalf("delivery lacks a continuation forecast: %+v", plan)
		}
		want := float64(plan.Economics.ExpectedNetPence+plan.Continuation.NetPence) /
			(plan.Economics.GameDays + plan.Continuation.GameDays)
		if math.Abs(plan.Economics.RankScorePencePerGameDay-want) > 1e-9 {
			t.Fatalf("combined rank %.9f, want %.9f for %+v", plan.Economics.RankScorePencePerGameDay, want, plan)
		}
	}
	if far.Economics.RankScorePencePerGameDay <= near.Economics.RankScorePencePerGameDay {
		t.Fatalf("shorter return did not improve the farther delivery's combined rank: near %+v far %+v", near, far)
	}
}

// A profitable legal outbound must sail without any return route evidence:
// a missing dataset or an unusable empty leg removes repeat underwriting,
// not the delivery itself.
func TestValuationLegalOutboundDoesNotRequireReturnRoute(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name   string
		change func(in *PlanningInput)
	}{
		{"missing return route dataset", func(in *PlanningInput) {
			delete(in.Routes, "portsmouth|le_havre|hoy")
			delete(in.Metadata, "routes/portsmouth|le_havre|hoy")
		}},
		{"unusable empty return leg", func(in *PlanningInput) {
			route := in.Routes["portsmouth|le_havre|hoy"]
			route.Legs[0].IsSoundToll = true
			in.Routes["portsmouth|le_havre|hoy"] = route
		}},
	}
	for _, test := range cases {
		t.Run(test.name, func(t *testing.T) {
			in := valuationInput(t)
			valuationScopePortsmouth(&in)
			in.Markets["portsmouth"] = valuationDestinationBook(t, "portsmouth", 5000, 100)
			test.change(&in)
			plans := PlanVoyages(in)
			accepted := 0
			for _, plan := range plans {
				if !plan.Accepted {
					continue
				}
				accepted++
				if plan.Destination != "portsmouth" {
					t.Fatalf("accepted delivery left the scoped destination: %+v", plan)
				}
				if plan.Economics.ExpectedNetPence <= 0 {
					t.Fatalf("accepted delivery lost its measured profit: %+v", plan)
				}
			}
			if accepted == 0 {
				t.Fatalf("profitable legal outbound was rejected for want of an unneeded return route: %+v", plans)
			}
		})
	}
}

// A whole-ton store top-up is cash that must be funded, but only the share
// the voyage actually consumes is expense. Charging the full expensive
// purchase as consumption expense rejects or mislabels profitable voyages;
// dropping the funding requirement would launch an unfunded purchase.
func TestValuationFundsWholeTonStoresWithoutChargingUnusedStoresAsExpense(t *testing.T) {
	t.Parallel()
	in := valuationInput(t)
	valuationScopePortsmouth(&in)
	in.Ship.Stores.ProvisionsTons = 0
	in.Markets["le_havre"] = valuationFixture[Market](t, `{
		"port_id":"le_havre","visibility":"quay","customs_bps":250,
		"books":[
			{"commodity_id":"provisions","depth":{"asks":[{"order_id":"npc-provisions","price_pence":40000,"remaining_quantity":100}],"bids":[]}},
			{"commodity_id":"grain","depth":{"asks":[{"order_id":"npc-grain","price_pence":500,"remaining_quantity":100}],"bids":[]}}
		]
	}`)
	in.Markets["portsmouth"] = valuationDestinationBook(t, "portsmouth", 5000, 100)
	plan := valuationPlanFor(t, PlanVoyages(in), "portsmouth")
	if !plan.Accepted || plan.Economics.ExpectedNetPence <= 0 {
		t.Fatalf("funded profitable voyage was not costed: %+v", plan)
	}
	if plan.StoresToBuy < 1 || plan.StoresLimitPence != 40_000 {
		t.Fatalf("whole-ton store top-up was not priced from the observed ladder: %+v", plan)
	}
	if plan.Economics.ProvisionsPence >= plan.StoresToBuy*plan.StoresLimitPence {
		t.Fatalf("the whole %dp store purchase became voyage consumption expense instead of the consumed share (bought %dt): %+v",
			plan.StoresToBuy*plan.StoresLimitPence, plan.StoresToBuy, plan.Economics)
	}
	if plan.Economics.ProvisionsPence <= 0 {
		t.Fatalf("consumed stores lost their replacement cost: %+v", plan.Economics)
	}
	// Causal funding control: cash short of the whole-ton store purchase plus
	// the protected reserve must still hold the ship, whatever its size.
	in.Balances["channel"] = 110_000
	for _, candidate := range PlanVoyages(in) {
		if candidate.Accepted {
			t.Fatalf("voyage was accepted without cash for the whole-ton store top-up: %+v", candidate)
		}
	}
}

// Cargo and sealed freight to one shared destination are sized jointly before
// the profitability gate: each candidate loses alone on its own copy of the
// shared berth and store costs, yet the single shared trip clears them. The
// manifest must also leave the sealed freight its hold tons.
func TestValuationSizesSharedDestinationCargoAndFreightJointly(t *testing.T) {
	t.Parallel()
	in := valuationInput(t)
	valuationScopePortsmouth(&in)
	in.Markets["portsmouth"] = valuationDestinationBook(t, "portsmouth", 1050, 100)
	in.Ports["le_havre"] = PortDetail{ID: "le_havre", BerthDuesPerDayPence: 2500, EffectiveBerths: 4}
	in.Ports["portsmouth"] = PortDetail{ID: "portsmouth", BerthDuesPerDayPence: 2500, EffectiveBerths: 4}
	freight := valuationFixture[Contract](t, `{
		"id":"freight-joint","contract_type":"freight","origin_port_id":"le_havre","dest_port_id":"portsmouth",
		"commodity_id":"sawn_planks","quantity_required":6,"remaining_quantity":6,
		"reward_pence":1225,"collateral_pence":5000,"status":"open","stale":false,
		"issuer_company_id":"valuation-other"
	}`)
	in.Contracts = []Contract{freight}
	plans := PlanVoyages(in)
	manifest := -1
	for i := range plans {
		plan := &plans[i]
		if !plan.Accepted {
			continue
		}
		if len(plan.FreightIDs) != 1 || plan.FreightIDs[0] != "freight-joint" || plan.Destination != "portsmouth" {
			t.Fatalf("accepted a plan that ignores the joint shared-cost constraint: %+v", plans)
		}
		if plan.Quantity+freight.QuantityRequired > in.Ship.EffectiveHoldCapacityTons-in.Ship.CargoUsedTons {
			t.Fatalf("manifest stole hold tons from the sealed freight: %dt cargo + %dt freight exceeds %dt hold",
				plan.Quantity, freight.QuantityRequired, in.Ship.EffectiveHoldCapacityTons)
		}
		manifest = i
	}
	if manifest < 0 || plans[manifest].Economics.ExpectedNetPence <= 0 {
		t.Fatalf("no profitable joint cargo+freight manifest although the shared trip clears its costs: %+v", plans)
	}
	var standaloneCargo, standaloneFreight bool
	for _, plan := range plans {
		if plan.Accepted {
			continue
		}
		if plan.Kind == "spot" && plan.Destination == "portsmouth" && len(plan.FreightIDs) == 0 &&
			plan.Economics.ExpectedNetPence <= 0 {
			standaloneCargo = true
		}
		if plan.Kind == "freight" && plan.ContractID == "freight-joint" && plan.Economics.ExpectedNetPence <= 0 {
			standaloneFreight = true
		}
	}
	if !standaloneCargo || !standaloneFreight {
		t.Fatalf("standalone shared-cost losers vanished as evidence: cargo %t freight %t", standaloneCargo, standaloneFreight)
	}
}

// Partial measured depth and the shared hold stay hard bounds while sizing:
// cargo stops at executable bid depth, a sealed freight keeps its tons, and
// an absent bid ladder never becomes revenue.
func TestValuationKeepsPartialDepthAndSharedHoldSafe(t *testing.T) {
	t.Parallel()
	t.Run("cargo stops at measured bid depth", func(t *testing.T) {
		in := valuationInput(t)
		valuationScopePortsmouth(&in)
		in.Markets["portsmouth"] = valuationDestinationBook(t, "portsmouth", 5000, 10)
		plan := valuationPlanFor(t, PlanVoyages(in), "portsmouth")
		if !plan.Accepted || plan.Quantity != 10 || plan.SellLimitPence != 5000 {
			t.Fatalf("partial destination depth was extrapolated or ignored: %+v", plan)
		}
	})
	t.Run("sealed freight keeps its hold tons", func(t *testing.T) {
		in := valuationInput(t)
		valuationScopePortsmouth(&in)
		freight := valuationFixture[Contract](t, `{
			"id":"freight-partial","contract_type":"freight","origin_port_id":"le_havre","dest_port_id":"portsmouth",
			"commodity_id":"sawn_planks","quantity_required":6,"remaining_quantity":6,
			"reward_pence":200000,"collateral_pence":5000,"status":"open","stale":false,
			"issuer_company_id":"valuation-other"
		}`)
		in.Contracts = []Contract{freight}
		plans := PlanVoyages(in)
		sealed := false
		for _, plan := range plans {
			if !plan.Accepted || len(plan.FreightIDs) == 0 {
				continue
			}
			sealed = true
			if plan.Quantity+freight.QuantityRequired > in.Ship.EffectiveHoldCapacityTons-in.Ship.CargoUsedTons {
				t.Fatalf("accepted plan overfilled the shared hold: %dt cargo + %dt freight exceeds %dt hold",
					plan.Quantity, freight.QuantityRequired, in.Ship.EffectiveHoldCapacityTons)
			}
		}
		if !sealed {
			t.Fatalf("compatible profitable freight lost its shared-trip slot: %+v", plans)
		}
	})
	t.Run("absent destination demand is not revenue", func(t *testing.T) {
		in := valuationInput(t)
		valuationScopePortsmouth(&in)
		in.Markets["portsmouth"] = valuationDestinationBook(t, "portsmouth", 5000, 0)
		for _, plan := range PlanVoyages(in) {
			if plan.Accepted {
				t.Fatalf("an empty bid ladder produced revenue: %+v", plan)
			}
		}
	})
}
