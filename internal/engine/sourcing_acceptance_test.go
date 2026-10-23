package engine

// Acceptance contracts for the bounded sourcing relocation probe (plan
// E4-E10, E14-E15): an idle, empty, unsealed ship may spend one capped, fully
// costed allowance to relocate toward a source with public supply evidence for
// exactly one live charter, then choose a whole-ton lot at the source from
// fresh executable books only. These tests are written before the feature and
// pin the coordinated contracts: planSourcingRelocation, sizeRelocationCargo
// and the gossip best-ask projection. They must fail until the implementation
// exists, then keep the probe inside its policy bounds forever.

import (
	"encoding/json"
	"fmt"
	"testing"
	"time"
)

func sourcingFixture[T any](t *testing.T, raw string) T {
	t.Helper()
	var value T
	if err := json.Unmarshal([]byte(raw), &value); err != nil {
		t.Fatal(err)
	}
	return value
}

func sourcingFreshMeta(now time.Time) DatasetMeta {
	return DatasetMeta{UpdatedAt: now, AttemptedAt: now, Generation: "sourcing-sync", Complete: true}
}

func sourcingRoute(from, to string, distanceNM int64) Route {
	return Route{
		FromPort:                 from,
		ToPort:                   to,
		ShipClass:                "hoy",
		TotalDistanceNM:          distanceNM,
		SafeProvisionsTons:       0.1,
		PortProvisionsTonsPerDay: 0.05,
		Legs: []RouteLeg{{
			FromNode:   from,
			ToNode:     to,
			DistanceNM: distanceNM,
			EdgeClass:  "coastal",
		}},
	}
}

// sourcingInputFixture builds an idle berthed ship at le_havre, one open
// sawn_planks charter delivering at portsmouth, and public supply evidence for
// the amsterdam source: its catalog manufacture list plus fresh coffee-house
// ask gossip. Amsterdam's executable book models what the ship may only read
// after actually arriving there.
func sourcingInputFixture(t *testing.T) PlanningInput {
	t.Helper()
	now := time.Date(2026, 10, 23, 8, 0, 0, 0, time.UTC)
	fresh := now.Add(-30 * time.Second).Format(time.RFC3339Nano)
	in := PlanningInput{
		Now:       now,
		CompanyID: "company-a",
		Catalog: sourcingFixture[Catalog](t, `{
			"ports":[
				{"id":"le_havre","banking_region":"channel","sea_zone_id":"english_channel","phase":1,"has_shipyard":true,"manufactures":["provisions"]},
				{"id":"amsterdam","banking_region":"channel","sea_zone_id":"english_channel","phase":1,"manufactures":["sawn_planks"]},
				{"id":"portsmouth","banking_region":"channel","sea_zone_id":"english_channel","phase":1,"imports":["provisions"]}
			],
			"ship_classes":[{"id":"hoy","hull_category":"coastal","phase":1,"base_speed_centiknots":600,"hold_capacity_tons":40,"max_cannons":4,"crew_required":10}],
			"sea_zones":[{"id":"english_channel","zone_class":"coastal","base_storm_bps":200,"base_piracy_bps":200}]
		}`),
		Ship: sourcingFixture[ShipState](t, `{
			"ship":{"id":"ship-a","name":"Lapras","ship_class":"hoy","status":"berthed","current_port_id":"le_havre","cannons_count":4},
			"hold_capacity_tons":40,"effective_hold_capacity_tons":24,"cargo_used_tons":0,
			"stores":{"provisions_tons":8},"cargo_manifest":{},"route_legs":[]
		}`),
		Clock: sourcingFixture[Clock](t, fmt.Sprintf(`{"real_utc":%q,"unlocked_phase":1,"paused":false,"calendar":{"month":3,"day":5,"time_scale":60,"total_game_hours":118}}`, now.Format(time.RFC3339Nano))),
		Zones: []ZoneObservation{{SeaZoneID: "english_channel", NetPiracyBPS: 200}},
		Markets: map[string]Market{
			"le_havre": sourcingFixture[Market](t, `{"port_id":"le_havre","visibility":"quay","customs_bps":250,"books":[{"commodity_id":"provisions","depth":{"asks":[{"order_id":"lh-p","price_pence":100,"remaining_quantity":500}],"bids":[]}}]}`),
			"amsterdam": sourcingFixture[Market](t, `{"port_id":"amsterdam","visibility":"quay","customs_bps":250,"books":[
				{"commodity_id":"sawn_planks","depth":{"asks":[
					{"order_id":"ams-1","price_pence":500,"remaining_quantity":10},
					{"order_id":"ams-2","price_pence":2000,"remaining_quantity":4},
					{"order_id":"ams-3","price_pence":2950,"remaining_quantity":100}],"bids":[]}},
				{"commodity_id":"provisions","depth":{"asks":[{"order_id":"ams-p","price_pence":100,"remaining_quantity":500}],"bids":[]}}]}`),
			"portsmouth": sourcingFixture[Market](t, `{"port_id":"portsmouth","visibility":"quay","customs_bps":250,"books":[{"commodity_id":"provisions","depth":{"asks":[{"order_id":"pm-p","price_pence":100,"remaining_quantity":500}],"bids":[]}}]}`),
		},
		Contracts:             []Contract{sourcingFixture[Contract](t, `{"id":"charter-planks","contract_type":"charter","origin_port_id":"portsmouth","commodity_id":"sawn_planks","quantity_required":1000,"remaining_quantity":1000,"reward_per_ton_pence":3000,"status":"open","is_public":true,"stale":false}`)},
		Balances:              map[string]int64{"channel": 1_000_000},
		ReserveByRegion:       map[string]int64{"channel": 75_000},
		Generation:            "sourcing-sync",
		FleetDailyUpkeepPence: 1_200,
	}
	in.Ports = map[string]PortDetail{
		"le_havre":   {ID: "le_havre", BerthDuesPerDayPence: 1000, EffectiveBerths: 4},
		"amsterdam":  {ID: "amsterdam", BerthDuesPerDayPence: 1000, EffectiveBerths: 4},
		"portsmouth": {ID: "portsmouth", BerthDuesPerDayPence: 1000, EffectiveBerths: 4},
	}
	gossip := sourcingFixture[PortDetail](t, fmt.Sprintf(`{"coffee_house_gossip":[{"subject_port_id":"amsterdam","snapshot_at":%q,"commodities":[{"commodity_id":"sawn_planks","snapshot_at":%q,"best_ask_pence":900}]}]}`, fresh, fresh))
	home := in.Ports["le_havre"]
	home.CoffeeHouseGossip = gossip.CoffeeHouseGossip
	in.Ports["le_havre"] = home
	in.Routes = make(map[string]Route)
	in.Metadata = make(map[string]DatasetMeta)
	for _, key := range []string{"catalog", "clock", "zones", "vaults", "contracts", "ships", "books/le_havre", "books/amsterdam", "books/portsmouth", "ports/le_havre", "ports/amsterdam", "ports/portsmouth"} {
		in.Metadata[key] = sourcingFreshMeta(now)
	}
	for _, from := range []string{"le_havre", "amsterdam", "portsmouth"} {
		for _, to := range []string{"le_havre", "amsterdam", "portsmouth"} {
			if from == to {
				continue
			}
			key := routeKey(from, to, "hoy")
			in.Routes[key] = sourcingRoute(from, to, 30)
			in.Metadata["routes/"+key] = sourcingFreshMeta(now)
		}
	}
	return in
}

// addSourcingSource introduces one more eligible source port that manufactures
// the charter commodity, with its own book, gossip report and route legs.
func addSourcingSource(t *testing.T, in *PlanningInput, id string, distanceNM, bestAskPence int64) {
	t.Helper()
	now := in.Now
	fresh := now.Add(-30 * time.Second).Format(time.RFC3339Nano)
	in.Catalog.Ports = append(in.Catalog.Ports, Port{ID: id, BankingRegion: "channel", SeaZoneID: "english_channel", Phase: 1, Manufactures: []string{"sawn_planks"}})
	for _, peer := range []string{"le_havre", "portsmouth"} {
		in.Routes[routeKey(peer, id, "hoy")] = sourcingRoute(peer, id, distanceNM)
		in.Routes[routeKey(id, peer, "hoy")] = sourcingRoute(id, peer, distanceNM)
	}
	in.Markets[id] = sourcingFixture[Market](t, fmt.Sprintf(`{"port_id":%q,"visibility":"quay","customs_bps":250,"books":[
		{"commodity_id":"sawn_planks","depth":{"asks":[{"order_id":%q,"price_pence":%d,"remaining_quantity":100}],"bids":[]}},
		{"commodity_id":"provisions","depth":{"asks":[{"order_id":%q,"price_pence":100,"remaining_quantity":500}],"bids":[]}}]}`,
		id, id+"-ask", bestAskPence, id+"-p"))
	in.Ports[id] = PortDetail{ID: id, BerthDuesPerDayPence: 1000, EffectiveBerths: 4}
	gossip := sourcingFixture[PortDetail](t, fmt.Sprintf(`{"coffee_house_gossip":[{"subject_port_id":%q,"snapshot_at":%q,"commodities":[{"commodity_id":"sawn_planks","snapshot_at":%q,"best_ask_pence":%d}]}]}`, id, fresh, fresh, bestAskPence))
	home := in.Ports["le_havre"]
	home.CoffeeHouseGossip = append(home.CoffeeHouseGossip, gossip.CoffeeHouseGossip...)
	in.Ports["le_havre"] = home
	for _, key := range []string{
		"books/" + id,
		"ports/" + id,
		"routes/" + routeKey("le_havre", id, "hoy"),
		"routes/" + routeKey(id, "le_havre", "hoy"),
		"routes/" + routeKey("portsmouth", id, "hoy"),
		"routes/" + routeKey(id, "portsmouth", "hoy"),
	} {
		in.Metadata[key] = sourcingFreshMeta(now)
	}
}

func acceptedRelocations(plans []Plan) []Plan {
	accepted := make([]Plan, 0, len(plans))
	for _, plan := range plans {
		if plan.Accepted {
			accepted = append(accepted, plan)
		}
	}
	return accepted
}

func sourcingExposure(plan Plan) int64 {
	return -plan.Economics.ExpectedNetPence
}

func TestSourcingProbeEligibilityRejectsUnsafeOrUnprovenSources(t *testing.T) {
	cases := []struct {
		name   string
		change func(t *testing.T, in *PlanningInput)
	}{
		{"sailing ship", func(t *testing.T, in *PlanningInput) {
			in.Ship.Ship.Status = "sailing"
		}},
		{"convoy bound", func(t *testing.T, in *PlanningInput) {
			in.Ship.Ship.ConvoyID = "convoy-1"
		}},
		{"residual cargo", func(t *testing.T, in *PlanningInput) {
			in.Ship.CargoUsedTons = 10
			in.Ship.CargoManifest = map[string]int64{"grain": 10}
		}},
		{"active sealed freight", func(t *testing.T, in *PlanningInput) {
			in.Contracts = append(in.Contracts, Contract{
				ID: "seal-1", ContractType: "freight", Status: "active",
				CarrierCompanyID: "company-a", CarrierShipID: "ship-a",
				OriginPortID: "le_havre", DestPortID: "portsmouth",
				CommodityID: "grain", QuantityRequired: 10,
			})
		}},
		{"no live charter", func(t *testing.T, in *PlanningInput) {
			in.Contracts = nil
		}},
		{"stale charter", func(t *testing.T, in *PlanningInput) {
			in.Contracts[0].Stale = true
		}},
		{"charter without typed reward", func(t *testing.T, in *PlanningInput) {
			in.Contracts[0].RewardPerTonPence = nil
		}},
		{"charter already filled", func(t *testing.T, in *PlanningInput) {
			in.Contracts[0].Status = "filled"
		}},
		{"accepted ordinary opportunity displaces exploration", func(t *testing.T, in *PlanningInput) {
			// An executable destination bid gives the ship a profitable spot
			// voyage from its current port; catalog manufacture alone never
			// blocks exploration because it is not executable stock or price.
			market := in.Markets["amsterdam"]
			market.Books[1].Depth.Bids = []Level{{OrderID: "ams-prov-bid", PricePence: 5000, RemainingQuantity: 100}}
			in.Markets["amsterdam"] = market
		}},
		{"imports alone are not supply evidence", func(t *testing.T, in *PlanningInput) {
			in.Catalog.Ports[1].Manufactures = nil
			in.Catalog.Ports[1].Imports = []string{"sawn_planks"}
			detail := in.Ports["le_havre"]
			detail.CoffeeHouseGossip = nil
			in.Ports["le_havre"] = detail
			delete(in.Markets, "amsterdam")
			delete(in.Metadata, "books/amsterdam")
		}},
		{"stale gossip report", func(t *testing.T, in *PlanningInput) {
			sourcingStripProduction(in)
			sourcingRestampGossip(t, in, in.Now.Add(-7*time.Minute), in.Now.Add(-30*time.Second))
		}},
		{"stale gossip commodity price", func(t *testing.T, in *PlanningInput) {
			sourcingStripProduction(in)
			sourcingRestampGossip(t, in, in.Now.Add(-30*time.Second), in.Now.Add(-7*time.Minute))
		}},
		{"ask already at charter reward", func(t *testing.T, in *PlanningInput) {
			sourcingStripProduction(in)
			sourcingRestampGossipAsk(t, in, 3000)
		}},
		{"source port phase locked", func(t *testing.T, in *PlanningInput) {
			in.Catalog.Ports[1].Phase = 2
		}},
		{"missing laden route", func(t *testing.T, in *PlanningInput) {
			key := routeKey("amsterdam", "portsmouth", "hoy")
			delete(in.Routes, key)
			delete(in.Metadata, "routes/"+key)
		}},
		{"laden route above hull rating", func(t *testing.T, in *PlanningInput) {
			key := routeKey("amsterdam", "portsmouth", "hoy")
			route := in.Routes[key]
			route.Legs[0].EdgeClass = "regional"
			in.Routes[key] = route
		}},
		{"unpriced Sound toll on the laden route", func(t *testing.T, in *PlanningInput) {
			key := routeKey("amsterdam", "portsmouth", "hoy")
			route := in.Routes[key]
			route.Legs[0].IsSoundToll = true
			in.Routes[key] = route
		}},
		{"impossible charter deadline", func(t *testing.T, in *PlanningInput) {
			expiry := in.Now.Add(time.Second)
			in.Contracts[0].ExpiresAt = &expiry
		}},
	}
	for _, test := range cases {
		t.Run(test.name, func(t *testing.T) {
			in := sourcingInputFixture(t)
			test.change(t, &in)
			if accepted := acceptedRelocations(planSourcingRelocation(in)); len(accepted) != 0 {
				t.Fatalf("ineligible sourcing candidate was accepted: %+v", accepted)
			}
		})
	}
}

// sourcingStripProduction removes amsterdam's catalog manufacture evidence and
// its pre-arrival book so the gossip report is the only supply evidence. The
// stale-gossip and ask-cap rules are gossip-specific: production evidence on
// its own still qualifies the source, and stale gossip must be ignored rather
// than disqualifying.
func sourcingStripProduction(in *PlanningInput) {
	in.Catalog.Ports[1].Manufactures = nil
	delete(in.Markets, "amsterdam")
	delete(in.Metadata, "books/amsterdam")
}

func sourcingRestampGossip(t *testing.T, in *PlanningInput, reportAt, priceAt time.Time) {
	t.Helper()
	sourcingReplaceGossip(t, in, fmt.Sprintf(`{"coffee_house_gossip":[{"subject_port_id":"amsterdam","snapshot_at":%q,"commodities":[{"commodity_id":"sawn_planks","snapshot_at":%q,"best_ask_pence":900}]}]}`,
		reportAt.Format(time.RFC3339Nano), priceAt.Format(time.RFC3339Nano)))
}

func sourcingRestampGossipAsk(t *testing.T, in *PlanningInput, bestAskPence int64) {
	t.Helper()
	fresh := in.Now.Add(-30 * time.Second).Format(time.RFC3339Nano)
	sourcingReplaceGossip(t, in, fmt.Sprintf(`{"coffee_house_gossip":[{"subject_port_id":"amsterdam","snapshot_at":%q,"commodities":[{"commodity_id":"sawn_planks","snapshot_at":%q,"best_ask_pence":%d}]}]}`,
		fresh, fresh, bestAskPence))
}

func sourcingReplaceGossip(t *testing.T, in *PlanningInput, raw string) {
	t.Helper()
	gossip := sourcingFixture[PortDetail](t, raw)
	detail := in.Ports["le_havre"]
	detail.CoffeeHouseGossip = gossip.CoffeeHouseGossip
	in.Ports["le_havre"] = detail
}

// TestUnstockedLocalManufactureDoesNotSuppressRemoteSourcing pins the E4/E5
// consumer rule from the other side: catalog manufacture at the current port
// is not executable inventory or price, so it must never revoke eligibility
// for relocating to a source that actually holds public supply evidence.
func TestUnstockedLocalManufactureDoesNotSuppressRemoteSourcing(t *testing.T) {
	in := sourcingInputFixture(t)
	in.Catalog.Ports[0].Manufactures = append(in.Catalog.Ports[0].Manufactures, "sawn_planks")
	accepted := acceptedRelocations(planSourcingRelocation(in))
	if len(accepted) == 0 {
		t.Fatalf("unstocked local manufacture suppressed eligible remote sourcing: %+v", planSourcingRelocation(in))
	}
	if accepted[0].Destination != "amsterdam" || accepted[0].Economics.RevenuePence != 0 {
		t.Fatalf("unexpected probe under unstocked local manufacture: %+v", accepted[0])
	}
}

func TestSourcingProbePlansBoundedCostOnlyRelocation(t *testing.T) {
	in := sourcingInputFixture(t)
	plans := planSourcingRelocation(in)
	accepted := acceptedRelocations(plans)
	if len(accepted) == 0 {
		t.Fatalf("eligible source with fresh public supply evidence was not explored: %+v", plans)
	}
	probe := accepted[0]
	if probe.Kind != "relocation" || probe.ShipID != "ship-a" || probe.Origin != "le_havre" ||
		probe.Destination != "amsterdam" || probe.Commodity != "sawn_planks" || probe.ContractID != "charter-planks" {
		t.Fatalf("relocation probe does not target the evidenced source and live charter: %+v", probe)
	}
	if probe.Quantity != 0 || probe.Economics.RevenuePence != 0 || probe.Economics.ExpectedNetPence >= 0 {
		t.Fatalf("relocation probe must be a zero-revenue, negatively costed move, not a trade: %+v", probe)
	}
	if probe.Economics.ProbabilityEstimate != nil {
		t.Fatal("relocation exposure was presented as a measured loss probability")
	}
	// Both-way safe stores for the relocation leg and the intended laden leg.
	wantStores := in.Routes["le_havre|amsterdam|hoy"].SafeProvisionsTons + in.Routes["amsterdam|portsmouth|hoy"].SafeProvisionsTons
	if probe.TargetStoresTons < wantStores {
		t.Fatalf("probe stores %.2ft omit the safe relocation and laden provisions (%.2ft)", probe.TargetStoresTons, wantStores)
	}
	if exposure := sourcingExposure(probe); exposure > 20_000 || exposure > (1_000_000-75_000)/50 {
		t.Fatalf("modeled probe exposure %dp exceeds the policy allowance", exposure)
	}
	// A ship waiting at the roadstead is equally stationary and eligible.
	roadstead := in
	roadstead.Ship.Ship.Status = "anchored_roadstead"
	if accepted := acceptedRelocations(planSourcingRelocation(roadstead)); len(accepted) == 0 {
		t.Fatalf("stationary roadstead ship was denied the bounded probe: %+v", plans)
	}
}

func TestSourcingProbeUsesPublicEvidenceWithoutPrivateBooksOrRemoteRevenue(t *testing.T) {
	// Each variant starts from its own fixture: Catalog.Ports, Markets and
	// Metadata alias shared backing storage, so shallow copies would leak
	// mutations between the evidence scenarios.
	gossipOnly := sourcingInputFixture(t)
	gossipOnly.Catalog.Ports[1].Manufactures = nil
	delete(gossipOnly.Markets, "amsterdam")
	delete(gossipOnly.Metadata, "books/amsterdam")
	accepted := acceptedRelocations(planSourcingRelocation(gossipOnly))
	if len(accepted) == 0 {
		t.Fatalf("fresh public ask gossip did not qualify the source: %+v", planSourcingRelocation(gossipOnly))
	}
	if accepted[0].Economics.RevenuePence != 0 {
		t.Fatal("gossip ask hints were converted into invented executable revenue")
	}
	bookOnly := sourcingInputFixture(t)
	bookOnly.Catalog.Ports[1].Manufactures = nil
	detail := bookOnly.Ports["le_havre"]
	detail.CoffeeHouseGossip = nil
	bookOnly.Ports["le_havre"] = detail
	plans := planSourcingRelocation(bookOnly)
	if accepted := acceptedRelocations(plans); len(accepted) != 0 {
		t.Fatalf("a remote private book unauthorized production evidence: %+v", accepted)
	}
	// E4's evidence rule is a disjunction: catalog production alone qualifies
	// the source, and stale gossip is ignored rather than disqualifying.
	productionOnly := sourcingInputFixture(t)
	homeDetail := productionOnly.Ports["le_havre"]
	homeDetail.CoffeeHouseGossip = nil
	productionOnly.Ports["le_havre"] = homeDetail
	if accepted := acceptedRelocations(planSourcingRelocation(productionOnly)); len(accepted) == 0 {
		t.Fatalf("catalog production alone did not qualify the source: %+v", planSourcingRelocation(productionOnly))
	}
	staleGossipWithProduction := sourcingInputFixture(t)
	sourcingRestampGossip(t, &staleGossipWithProduction, staleGossipWithProduction.Now.Add(-7*time.Minute), staleGossipWithProduction.Now.Add(-7*time.Minute))
	if accepted := acceptedRelocations(planSourcingRelocation(staleGossipWithProduction)); len(accepted) == 0 {
		t.Fatalf("stale gossip wrongly revoked manufacture-based eligibility: %+v", planSourcingRelocation(staleGossipWithProduction))
	}
}

func TestSourcingProbeShortlistsAtMostThreePairsCheapestExposureFirst(t *testing.T) {
	in := sourcingInputFixture(t)
	addSourcingSource(t, &in, "rouen", 300, 920)
	accepted := acceptedRelocations(planSourcingRelocation(in))
	if len(accepted) < 2 {
		t.Fatalf("both evidenced sources were not compared: %+v", accepted)
	}
	if len(accepted) > 3 {
		t.Fatalf("more than three source/charter pairs were shortlisted: %+v", accepted)
	}
	if accepted[0].Destination != "amsterdam" || accepted[1].Destination != "rouen" {
		t.Fatalf("candidates are not ordered by lowest relocation exposure first: %+v", accepted)
	}
	if sourcingExposure(accepted[0]) >= sourcingExposure(accepted[1]) {
		t.Fatalf("the cheaper near source was not ranked first: %dp then %dp", sourcingExposure(accepted[0]), sourcingExposure(accepted[1]))
	}
	four := sourcingInputFixture(t)
	addSourcingSource(t, &four, "rouen", 300, 920)
	addSourcingSource(t, &four, "bristol", 300, 920)
	addSourcingSource(t, &four, "gent", 300, 920)
	if accepted := acceptedRelocations(planSourcingRelocation(four)); len(accepted) > 3 {
		t.Fatalf("candidate shortlist grew past three pairs: %+v", accepted)
	}
}

func TestSourcingProbeAllowanceIsMinOfTwoHundredCrownsAndTwoPercent(t *testing.T) {
	// Base fixture: floor((1_000_000-75_000)/50) = 18_500p. Every variant
	// rebuilds the fixture because Balances and ReserveByRegion are shared
	// maps behind a struct copy.
	in := sourcingInputFixture(t)
	accepted := acceptedRelocations(planSourcingRelocation(in))
	if len(accepted) == 0 {
		t.Fatalf("affordable probe was rejected: %+v", planSourcingRelocation(in))
	}
	if exposure := sourcingExposure(accepted[0]); exposure > 18_500 {
		t.Fatalf("probe exposure %dp exceeds floor(uncommitted cash/50) = 18500p", exposure)
	}
	rich := sourcingInputFixture(t)
	rich.Balances["channel"] = 50_000_000
	accepted = acceptedRelocations(planSourcingRelocation(rich))
	if len(accepted) == 0 {
		t.Fatalf("probe disappeared for a wealthy account: %+v", planSourcingRelocation(rich))
	}
	if exposure := sourcingExposure(accepted[0]); exposure > 20_000 {
		t.Fatalf("probe exposure %dp exceeds the absolute 200 Crown ceiling", exposure)
	}
	starved := sourcingInputFixture(t)
	starved.Balances["channel"] = 200_000
	starved.ReserveByRegion["channel"] = 150_000
	if accepted := acceptedRelocations(planSourcingRelocation(starved)); len(accepted) != 0 {
		t.Fatalf("probe accepted beyond floor(uncommitted cash/50) = 1000p: %+v", accepted)
	}
	expensive := sourcingInputFixture(t)
	expensive.FleetDailyUpkeepPence = 500_000
	if accepted := acceptedRelocations(planSourcingRelocation(expensive)); len(accepted) != 0 {
		t.Fatalf("probe accepted beyond the absolute 200 Crown allowance: %+v", accepted)
	}
}

func sourcingArrivalFixture(t *testing.T, asks []Level, rewardPerTon, spentPence int64) (PlanningInput, Plan, Contract) {
	t.Helper()
	in := sourcingInputFixture(t)
	market := in.Markets["amsterdam"]
	market.Books[0].Depth.Asks = asks
	in.Markets["amsterdam"] = market
	in.Ship = sourcingFixture[ShipState](t, `{
		"ship":{"id":"ship-a","name":"Lapras","ship_class":"hoy","status":"berthed","current_port_id":"amsterdam","cannons_count":4},
		"hold_capacity_tons":40,"effective_hold_capacity_tons":24,"cargo_used_tons":0,
		"stores":{"provisions_tons":8},"cargo_manifest":{},"route_legs":[]
	}`)
	charter := in.Contracts[0]
	reward := rewardPerTon
	charter.RewardPerTonPence = &reward
	probe := Plan{Kind: "relocation", ShipID: "ship-a", Origin: "amsterdam", Destination: "portsmouth", Commodity: "sawn_planks"}
	return in, probe, charter
}

var sourcingArrivalLadder = []Level{
	{PricePence: 500, RemainingQuantity: 10},
	{PricePence: 2000, RemainingQuantity: 4},
	{PricePence: 2950, RemainingQuantity: 100},
}

func TestRelocationArrivalSelectsWholeTonsOnAllCosts(t *testing.T) {
	t.Run("exhaustive winner and all-cost pence identity", func(t *testing.T) {
		in, probe, charter := sourcingArrivalFixture(t, sourcingArrivalLadder, 3000, 18_500)
		plan := sizeRelocationCargo(in, probe, charter, 18_500)
		if !plan.Accepted || plan.Quantity != 14 {
			t.Fatalf("whole-ton winner deviates from the exhaustive all-cost optimum (the 15th ton at 2950p loses against the 3000p reward): %+v", plan)
		}
		if plan.Kind != "charter" || plan.Origin != "amsterdam" || plan.Destination != "portsmouth" ||
			plan.Commodity != "sawn_planks" || plan.ContractID != "charter-planks" {
			t.Fatalf("arrival lot does not serve the targeted charter lane: %+v", plan)
		}
		e := plan.Economics
		if e.RevenuePence != 42_000 || e.PurchasePence != 13_000 || e.CustomsPence != 339 {
			t.Fatalf("executable depth terms were not measured exactly: %+v", e)
		}
		want := e.RevenuePence - e.PurchasePence - e.CustomsPence - e.ProvisionsPence -
			e.BerthDuesPence - e.UpkeepPence - e.TollPence - 18_500
		if e.ExpectedNetPence != want {
			t.Fatalf("all-cost identity broken: net %dp, want %dp including the sunk probe exposure: %+v", e.ExpectedNetPence, want, e)
		}
	})
	t.Run("supported 800 to 1000 Crown opportunity is selected", func(t *testing.T) {
		// Reward 6500 keeps every marginal ton positive (cheapest marginal
		// cost 3025p at the 2950p step), so the exhaustive all-cost optimum
		// is the capacity-bound 24t lot: 156000 revenue − 42500 purchase −
		// 1087 customs − sunk 18500 − operating costs lands in the band.
		in, probe, charter := sourcingArrivalFixture(t, sourcingArrivalLadder, 6500, 18_500)
		plan := sizeRelocationCargo(in, probe, charter, 18_500)
		if !plan.Accepted || plan.Quantity != 24 {
			t.Fatalf("exhaustive all-cost optimum was not the capacity-bound whole-ton lot: %+v", plan)
		}
		if plan.Economics.RevenuePence != 156_000 || plan.Economics.PurchasePence != 42_500 || plan.Economics.CustomsPence != 1_087 {
			t.Fatalf("band fixture depth terms were not measured exactly: %+v", plan.Economics)
		}
		if net := plan.Economics.ExpectedNetPence; net < 80_000 || net > 100_000 {
			t.Fatalf("synthetic 800-1000 Crown candidate was not selected when supported: %dp net (%+v)", net, plan)
		}
	})
	t.Run("positive opportunities below 800 Crowns stay accepted", func(t *testing.T) {
		thin := []Level{{PricePence: 500, RemainingQuantity: 10}}
		in, probe, charter := sourcingArrivalFixture(t, thin, 1200, 500)
		plan := sizeRelocationCargo(in, probe, charter, 500)
		if !plan.Accepted || plan.Quantity != 10 {
			t.Fatalf("measured sub-800 Crown opportunity was rejected by an invented profit floor: %+v", plan)
		}
		if net := plan.Economics.ExpectedNetPence; net <= 0 || net >= 80_000 {
			t.Fatalf("sub-800 Crown net left the expected band: %dp", net)
		}
	})
	t.Run("thin depth bounds the whole-ton lot", func(t *testing.T) {
		thin := []Level{{PricePence: 500, RemainingQuantity: 3}}
		in, probe, charter := sourcingArrivalFixture(t, thin, 3000, 500)
		plan := sizeRelocationCargo(in, probe, charter, 500)
		if !plan.Accepted || plan.Quantity != 3 {
			t.Fatalf("lot was not bounded by observed ask depth: %+v", plan)
		}
	})
}

func TestRelocationArrivalRefusesUnpricedLateOrStaleEvidence(t *testing.T) {
	cases := []struct {
		name   string
		change func(t *testing.T, in *PlanningInput, charter *Contract)
	}{
		{"negative combined outcome", func(t *testing.T, in *PlanningInput, charter *Contract) {
			reward := int64(520)
			charter.RewardPerTonPence = &reward
		}},
		{"no local ask depth", func(t *testing.T, in *PlanningInput, charter *Contract) {
			market := in.Markets["amsterdam"]
			market.Books[0].Depth.Asks = nil
			in.Markets["amsterdam"] = market
		}},
		{"stale source depth", func(t *testing.T, in *PlanningInput, charter *Contract) {
			meta := in.Metadata["books/amsterdam"]
			meta.Stale = true
			in.Metadata["books/amsterdam"] = meta
		}},
		{"source depth lacks quay visibility", func(t *testing.T, in *PlanningInput, charter *Contract) {
			market := in.Markets["amsterdam"]
			market.Visibility = "roster"
			in.Markets["amsterdam"] = market
		}},
		{"charter deadline cannot be met", func(t *testing.T, in *PlanningInput, charter *Contract) {
			expiry := in.Now.Add(time.Second)
			charter.ExpiresAt = &expiry
		}},
		{"charter no longer open", func(t *testing.T, in *PlanningInput, charter *Contract) {
			charter.Status = "filled"
		}},
	}
	for _, test := range cases {
		t.Run(test.name, func(t *testing.T) {
			in, probe, charter := sourcingArrivalFixture(t, sourcingArrivalLadder, 3000, 18_500)
			test.change(t, &in, &charter)
			plan := sizeRelocationCargo(in, probe, charter, 18_500)
			if plan.Accepted {
				t.Fatalf("arrival purchase accepted on unusable evidence: %+v", plan)
			}
			if plan.Quantity != 0 || plan.Economics.RevenuePence != 0 {
				t.Fatalf("rejected arrival invented a lot or revenue: %+v", plan)
			}
		})
	}
}

// TestGrowthEvidenceCountsRelocationLossesButNeverZeroTonProbes pins plan E14:
// the two-positive-known-cycle and loss-inclusive growth gates survive the new
// probe settlements unchanged.
func TestGrowthEvidenceCountsRelocationLossesButNeverZeroTonProbes(t *testing.T) {
	control := fundedGrowthFixture()
	if decision := EvaluateGrowth(control); !decision.Accepted {
		t.Fatalf("funded proven lane control rejected: %s", decision.Reason)
	}
	paybackFlip := fundedGrowthFixture()
	paybackFlip.Proofs = append(paybackFlip.Proofs, LaneProof{QuantityTons: 0, GameSeconds: 86_400, NetPence: -160_000, CostsKnown: true})
	if decision := EvaluateGrowth(paybackFlip); decision.Accepted {
		t.Fatalf("relocation probe loss did not reduce the lane's realized payback: %+v", decision)
	}
	wiped := fundedGrowthFixture()
	wiped.Proofs = append(wiped.Proofs, LaneProof{QuantityTons: 0, GameSeconds: 86_400, NetPence: -400_000, CostsKnown: true})
	if decision := EvaluateGrowth(wiped); decision.Accepted {
		t.Fatal("loss-inclusive lane net still admitted construction after a wiped-out probe")
	}
	zeroTons := fundedGrowthFixture()
	zeroTons.Proofs = []LaneProof{
		{QuantityTons: 0, GameSeconds: 86_400, NetPence: 100_000, CostsKnown: true},
		{QuantityTons: 0, GameSeconds: 86_400, NetPence: 100_000, CostsKnown: true},
	}
	if decision := EvaluateGrowth(zeroTons); decision.Accepted {
		t.Fatalf("zero-ton probes were counted as positive throughput proof: %+v", decision)
	}
	unknown := fundedGrowthFixture()
	unknown.Proofs = append(unknown.Proofs, LaneProof{QuantityTons: 0, GameSeconds: 86_400, CostsKnown: false})
	if decision := EvaluateGrowth(unknown); decision.Accepted {
		t.Fatal("unknown probe cost basis funded fleet growth")
	}
}
