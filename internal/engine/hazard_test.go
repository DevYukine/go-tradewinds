package engine

import (
	"context"
	"encoding/json"
	"fmt"
	"math"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
	"unicode/utf8"

	"github.com/DevYukine/go-tradewinds/internal/gameapi"
	"github.com/DevYukine/go-tradewinds/internal/store"
	"go.uber.org/zap"
	"go.uber.org/zap/zaptest/observer"
)

// Fixtures opt into observed zero-loss quotes rather than giving production
// planning a zero-hazard fallback when evidence is absent.
func seedZeroHazardQuotes(in *PlanningInput) {
	if in.HazardQuotes == nil {
		in.HazardQuotes = make(map[string]HazardQuote)
	}
	ships := append([]ShipState{in.Ship}, in.Fleet...)
	for _, ship := range ships {
		local := *in
		local.Ship = ship
		for _, route := range in.Routes {
			if route.FromPort != ship.Ship.CurrentPortID || route.ShipClass != ship.Ship.ShipClass {
				continue
			}
			piracy, complete := hazardRoutePiracy(local, route)
			if !complete {
				continue
			}
			in.HazardQuotes[hazardHullKey(ship.Ship.ID, route.FromPort, route.ToPort)] = HazardQuote{
				ShipID: ship.Ship.ID, Origin: route.FromPort, Destination: route.ToPort, ShipClass: ship.Ship.ShipClass,
				CannonsCount: ship.Ship.CannonsCount, Month: in.Clock.Calendar.Month,
				ObservedAt: in.Now, AttemptedAt: in.Now, MaxInsurablePence: 2_000_000, NetPiracyBPS: piracy,
			}
		}
	}
}

func hazardInputFixture() PlanningInput {
	now := time.Now().UTC()
	in := PlanningInput{Now: now, Generation: "hazard-test", CompanyID: "ours",
		Metadata: make(map[string]DatasetMeta), Balances: map[string]int64{"channel": 10_000_000},
		Routes: make(map[string]Route), Markets: make(map[string]Market), Ports: make(map[string]PortDetail),
	}
	in.Clock.RealUTC = now
	in.Clock.Calendar.Month, in.Clock.Calendar.Day, in.Clock.Calendar.TimeScale = 3, 5, 60
	in.Clock.UnlockedPhase = 1
	in.Catalog = Catalog{
		Ports:       []Port{{ID: "origin", BankingRegion: "channel", Phase: 1}, {ID: "destination", BankingRegion: "channel", Phase: 1}},
		ShipClasses: []Hull{{ID: "hoy", HullCategory: "coastal", BaseSpeedCentiknots: 600, HoldCapacityTons: 40, MaxCannons: 4, CrewRequired: 10}},
		SeaZones:    []Zone{{ID: "channel", ZoneClass: "coastal", BaseStormBPS: 9_999}},
	}
	in.Ship.Ship.ID, in.Ship.Ship.ShipClass, in.Ship.Ship.Status, in.Ship.Ship.CurrentPortID = "ship-a", "hoy", "berthed", "origin"
	in.Ship.Ship.CannonsCount, in.Ship.EffectiveHoldCapacityTons, in.Ship.HoldCapacityTons = 4, 24, 40
	in.Ship.Stores.ProvisionsTons = 8
	in.Zones = []ZoneObservation{{SeaZoneID: "channel", NetPiracyBPS: 200}}
	originBook := Market{PortID: "origin", Visibility: "quay"}
	_ = json.Unmarshal([]byte(`{"books":[{"commodity_id":"provisions","depth":{"asks":[{"price_pence":1000,"remaining_quantity":100}]}}]}`), &originBook)
	in.Markets["origin"] = originBook
	for _, port := range in.Catalog.Ports {
		in.Ports[port.ID] = PortDetail{ID: port.ID, EffectiveBerths: 4}
	}
	for _, pair := range [][2]string{{"origin", "destination"}, {"destination", "origin"}} {
		route := Route{FromPort: pair[0], ToPort: pair[1], ShipClass: "hoy", TotalDistanceNM: 30, SafeProvisionsTons: 0.1,
			Legs: []RouteLeg{{FromNode: pair[0], ToNode: "channel", EdgeClass: "coastal", DistanceNM: 15}, {FromNode: "channel", ToNode: pair[1], EdgeClass: "coastal", DistanceNM: 15}}}
		in.Routes[routeKey(pair[0], pair[1], "hoy")] = route
	}
	for _, key := range []string{"catalog", "clock", "zones", "vaults", "contracts", "ships", "books/origin", "ports/origin", "ports/destination", "routes/origin|destination|hoy", "routes/destination|origin|hoy"} {
		in.Metadata[key] = DatasetMeta{Complete: true, UpdatedAt: now, AttemptedAt: now, Generation: in.Generation}
	}
	reward := int64(3_000)
	in.Contracts = []Contract{{ID: "charter", ContractType: "charter", OriginPortID: "destination", CommodityID: "provisions", RemainingQuantity: 24, RewardPerTonPence: &reward, Status: "open"}}
	seedZeroHazardQuotes(&in)
	return in
}

func TestHazardSelectsPublishedHullLadenMaximum(t *testing.T) {
	for _, tc := range []struct {
		name        string
		hull, laden int64
		age         float64
		want        int64
		source      string
	}{
		{"hull bound", 351, 200, 1, 351, "hull-only"},
		{"laden bound", 351, 490, 1, 490, "laden"},
		{"laden thirty-day boundary", 351, 490, 720, 490, "laden"},
		{"expired laden", 351, 490, 720.01, 351, "hull-only"},
		{"zero is observed", 0, 0, 1, 0, "hull-only"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			in := hazardInputFixture()
			key := hazardHullKey("ship-a", "origin", "destination")
			hull := in.HazardQuotes[key]
			hull.ExpectedLossBPS = tc.hull
			in.HazardQuotes[key] = hull
			laden := hull
			laden.Laden, laden.ExpectedLossBPS = true, tc.laden
			laden.ObservedAt = in.Now.Add(-time.Duration(tc.age * float64(time.Hour) / 60))
			in.HazardQuotes[hazardLadenKey("origin", "destination", "hoy", 4, 3)] = laden
			bps, source, ok := planHazard(in, in.Routes[routeKey("origin", "destination", "hoy")])
			if !ok || bps != tc.want || !strings.Contains(source, tc.source) {
				t.Fatalf("bound = %d, %q, %t; want %d %s", bps, source, ok, tc.want, tc.source)
			}
		})
	}
}

func TestHazardQuoteValidityBoundaries(t *testing.T) {
	for _, tc := range []struct {
		name   string
		change func(*PlanningInput, *HazardQuote)
		want   bool
	}{
		{"six game hours", func(in *PlanningInput, q *HazardQuote) { q.ObservedAt = in.Now.Add(-6 * time.Minute) }, true},
		{"six point zero one game hours", func(in *PlanningInput, q *HazardQuote) {
			q.ObservedAt = in.Now.Add(-6*time.Minute - 600*time.Millisecond)
		}, false},
		{"month changes", func(in *PlanningInput, q *HazardQuote) { in.Clock.Calendar.Month = 4 }, false},
		{"guns change", func(in *PlanningInput, q *HazardQuote) { in.Ship.Ship.CannonsCount-- }, false},
		{"route piracy changes", func(in *PlanningInput, q *HazardQuote) { in.Zones[0].NetPiracyBPS++ }, false},
		{"unrelated piracy changes", func(in *PlanningInput, q *HazardQuote) {
			in.Zones = append(in.Zones, ZoneObservation{SeaZoneID: "elsewhere", NetPiracyBPS: 9_000})
		}, true},
		{"failed refresh preserves a published bound", func(in *PlanningInput, q *HazardQuote) { q.Error = "HTTP 503" }, true},
		{"missing quote", func(in *PlanningInput, q *HazardQuote) { q.ObservedAt = time.Time{} }, false},
		{"no open route", func(in *PlanningInput, q *HazardQuote) { q.Illegal = true }, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			in := hazardInputFixture()
			key := hazardHullKey("ship-a", "origin", "destination")
			quote := in.HazardQuotes[key]
			tc.change(&in, &quote)
			in.HazardQuotes[key] = quote
			_, _, ok := planHazard(in, in.Routes[routeKey("origin", "destination", "hoy")])
			if ok != tc.want {
				t.Fatalf("available = %t; want %t", ok, tc.want)
			}
		})
	}
}

func TestHazardSyntheticCandidateRequiresOwnedOriginClass(t *testing.T) {
	for _, tc := range []struct {
		name   string
		change func(*PlanningInput)
		want   bool
	}{
		{"matching owner", func(*PlanningInput) {}, true},
		{"no owner", func(in *PlanningInput) { in.Fleet = nil }, false},
		{"wrong class", func(in *PlanningInput) { in.Fleet[0].Ship.ShipClass = "brig" }, false},
		{"owner has moved", func(in *PlanningInput) { in.Fleet[0].Ship.CurrentPortID = "elsewhere" }, true},
		{"armed owner cannot price an unarmed target", func(in *PlanningInput) { in.Ship.Ship.CannonsCount = 0 }, false},
		{"owner sailing", func(in *PlanningInput) { in.Fleet[0].Ship.Status = "sailing" }, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			in := hazardInputFixture()
			in.Fleet = []ShipState{in.Ship}
			in.Ship.Ship.ID = "growth-candidate"
			tc.change(&in)
			_, _, ok := planHazard(in, in.Routes[routeKey("origin", "destination", "hoy")])
			if ok != tc.want {
				t.Fatalf("available = %t; want %t", ok, tc.want)
			}
		})
	}
}

func TestHazardChargeAndPlannerFailClosed(t *testing.T) {
	for _, tc := range []struct {
		risk, bps, want int64
		ok              bool
	}{
		{100_000, 351, 3_510, true}, {1, 1, 1, true}, {100_000, 20_000, 200_000, true},
		{math.MaxInt64, math.MaxInt64, 0, false},
	} {
		got, ok := hazardCharge(tc.risk, tc.bps)
		if ok != tc.ok || ok && got != tc.want {
			t.Fatalf("hazardCharge(%d,%d) = %d,%t", tc.risk, tc.bps, got, ok)
		}
	}
	in := hazardInputFixture()
	in.Markets["origin"].Books[0].Depth.Asks[0].PricePence = 99_999
	plan := Plan{Kind: "charter", ShipID: "ship-a", Origin: "origin", Destination: "destination", Commodity: "provisions", Quantity: 1}
	base := pricePlan(in, in.Catalog.ShipClasses[0], in.Catalog.Ports[0], plan, 1_000_000, 0)
	if !base.Accepted {
		t.Fatalf("unpriced baseline: %+v", base)
	}
	key := hazardHullKey("ship-a", "origin", "destination")
	quote := in.HazardQuotes[key]
	quote.ExpectedLossBPS = 351
	in.HazardQuotes[key] = quote
	beforeHazard := base.Economics.RevenuePence - base.Economics.ExpectedNetPence + 3_510
	priced := pricePlan(in, in.Catalog.ShipClasses[0], in.Catalog.Ports[0], plan, beforeHazard, 0)
	if priced.Accepted || priced.Economics.HazardPence != 3_510 || priced.Economics.ExpectedNetPence != 0 || priced.Economics.ProbabilityEstimate != nil {
		t.Fatalf("3,510p before hazard did not reject at zero: %+v", priced)
	}
	delete(in.HazardQuotes, key)
	priced = pricePlan(in, in.Catalog.ShipClasses[0], in.Catalog.Ports[0], plan, 1_000_000, 0)
	if priced.Accepted || priced.Reason != hazardUnavailable {
		t.Fatalf("missing quote assumed zero: %+v", priced)
	}
}

func TestHazardRefreshCapsRequestsAndOrdersOldestFirst(t *testing.T) {
	in := hazardInputFixture()
	for index := range 34 {
		id := fmt.Sprintf("port-%02d", index)
		in.Catalog.Ports = append(in.Catalog.Ports, Port{ID: id, Phase: 1})
		route := in.Routes[routeKey("origin", "destination", "hoy")]
		route.ToPort = id
		route.Legs = append([]RouteLeg(nil), route.Legs...)
		route.Legs[len(route.Legs)-1].ToNode = id
		in.Routes[routeKey("origin", id, "hoy")] = route
		in.Metadata["routes/"+routeKey("origin", id, "hoy")] = in.Metadata["routes/origin|destination|hoy"]
	}
	seedZeroHazardQuotes(&in)
	for key, quote := range in.HazardQuotes {
		quote.ObservedAt = in.Now.Add(-7 * time.Minute)
		quote.AttemptedAt = in.Now.Add(-time.Hour)
		if quote.Destination == "port-33" {
			quote.AttemptedAt = in.Now.Add(-2 * time.Hour)
		}
		in.HazardQuotes[key] = quote
	}
	var destinations []string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		if req.Method != http.MethodGet || req.URL.Path != "/v1/insurance/quote" || req.URL.Query().Get("insured_value_pence") != "10000" {
			t.Errorf("unexpected quote request %s", req.URL)
		}
		destinations = append(destinations, req.URL.Query().Get("dest_port_id"))
		_ = json.NewEncoder(w).Encode(map[string]any{"lloyds_expected_loss_bps": 351, "max_insurable_pence": 2_000_000})
	}))
	defer server.Close()
	runner := New(gameapi.New(server.URL, "synthetic-key", nil), nil, store.Account{}, zap.NewNop(), Options{})
	runner.meta, runner.generation = in.Metadata, in.Generation
	runner.snapshot[hazardQuoteSnapshotKey] = encode(in.HazardQuotes)
	state := AccountState{Catalog: in.Catalog, Clock: in.Clock, Ships: []ShipState{in.Ship}, Routes: in.Routes}
	runner.snapshot["zones"] = encode(in.Zones)
	if err := runner.refreshHazardQuotes(context.Background(), state); err != nil {
		t.Fatal(err)
	}
	if len(destinations) != 30 || destinations[0] != "port-33" {
		t.Fatalf("GET order/count: %v", destinations)
	}
}
func hazardManyCandidateFixture(count int) PlanningInput {
	in := hazardInputFixture()
	in.HazardQuotes = nil
	for index := 1; index < count; index++ {
		id := fmt.Sprintf("port-%02d", index)
		port := in.Catalog.Ports[1]
		port.ID = id
		in.Catalog.Ports = append(in.Catalog.Ports, port)
		in.Ports[id] = PortDetail{ID: id, EffectiveBerths: 4}
		in.Metadata["ports/"+id] = in.Metadata["ports/destination"]
		for _, pair := range [][2]string{{"origin", "destination"}, {"destination", "origin"}} {
			route := in.Routes[routeKey(pair[0], pair[1], "hoy")]
			route.Legs = append([]RouteLeg(nil), route.Legs...)
			if route.FromPort == "destination" {
				route.FromPort, route.Legs[0].FromNode = id, id
			} else {
				route.ToPort, route.Legs[len(route.Legs)-1].ToNode = id, id
			}
			key := routeKey(route.FromPort, route.ToPort, "hoy")
			in.Routes[key] = route
			in.Metadata["routes/"+key] = in.Metadata["routes/"+routeKey(pair[0], pair[1], "hoy")]
		}
		reward := int64(3_000 + index*100)
		contract := in.Contracts[0]
		contract.ID, contract.OriginPortID, contract.RewardPerTonPence = "charter-"+id, id, &reward
		in.Contracts = append(in.Contracts, contract)
	}
	return in
}

func TestHazardFortyCandidatesPricesBestOnFirstPass(t *testing.T) {
	in := hazardManyCandidateFixture(40)
	var destinations []string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		destinations = append(destinations, req.URL.Query().Get("dest_port_id"))
		_ = json.NewEncoder(w).Encode(map[string]any{"lloyds_expected_loss_bps": 351, "max_insurable_pence": 2_000_000})
	}))
	defer server.Close()
	runner := New(gameapi.New(server.URL, "synthetic-key", nil), nil, store.Account{}, zap.NewNop(), Options{})
	runner.meta, runner.generation = in.Metadata, in.Generation
	runner.snapshot["zones"] = encode(in.Zones)
	state := AccountState{Catalog: in.Catalog, Clock: in.Clock, Ships: []ShipState{in.Ship}, Routes: in.Routes,
		Markets: in.Markets, Ports: in.Ports, Contracts: in.Contracts,
		Vaults: []Vault{{AccountType: "vault", BankingRegion: "channel", BalancePence: 10_000_000}}}
	if err := runner.refreshHazardQuotes(context.Background(), state); err != nil {
		t.Fatal(err)
	}
	if len(destinations) != 30 || destinations[0] != "port-39" {
		t.Fatalf("best of forty candidates was not quoted first within cap: %v", destinations)
	}
	state.HazardQuotes = decode[map[string]HazardQuote](runner.snapshot, hazardQuoteSnapshotKey)
	plans := PlanVoyages(runner.planningInputFrom(in.Ship, state))
	if len(plans) == 0 || !plans[0].Accepted || plans[0].Destination != "port-39" || plans[0].Economics.HazardBPS != 351 {
		t.Fatalf("best candidate was not priced and accepted in its first pass: %+v", plans)
	}
}

func TestHazardCappedCandidatesWaitAtMostOnePassDespiteFailures(t *testing.T) {
	in := hazardManyCandidateFixture(40)
	var destinations []string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		destinations = append(destinations, req.URL.Query().Get("dest_port_id"))
		w.WriteHeader(http.StatusServiceUnavailable)
	}))
	defer server.Close()
	runner := New(gameapi.New(server.URL, "synthetic-key", nil), nil, store.Account{}, zap.NewNop(), Options{})
	runner.meta, runner.generation = in.Metadata, in.Generation
	runner.snapshot["zones"] = encode(in.Zones)
	state := AccountState{Catalog: in.Catalog, Clock: in.Clock, Ships: []ShipState{in.Ship}, Routes: in.Routes,
		Markets: in.Markets, Ports: in.Ports, Contracts: in.Contracts,
		Vaults: []Vault{{AccountType: "vault", BankingRegion: "channel", BalancePence: 10_000_000}}}
	for range 2 {
		before := len(destinations)
		if err := runner.refreshHazardQuotes(context.Background(), state); err != nil {
			t.Fatal(err)
		}
		if len(destinations)-before != 30 {
			t.Fatalf("refresh exceeded or underused its thirty requests: %v", destinations)
		}
	}
	seen := make(map[string]bool)
	for _, destination := range destinations[:40] {
		seen[destination] = true
	}
	if len(seen) != 40 {
		t.Fatalf("retries starved capped candidates past the next pass: %v", destinations)
	}
}

func TestHazardLadenRefinementsDoNotStarvePrimaryBounds(t *testing.T) {
	in := hazardManyCandidateFixture(40)
	in.Ship.CargoUsedTons, in.Ship.CargoManifest = 2, map[string]int64{"provisions": 2}
	var primary []string
	requests := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		requests++
		if req.URL.Query().Get("insured_value_pence") == "10000" {
			primary = append(primary, req.URL.Query().Get("dest_port_id"))
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"lloyds_expected_loss_bps": 351, "max_insurable_pence": 2_000_000})
	}))
	defer server.Close()
	runner := New(gameapi.New(server.URL, "synthetic-key", nil), nil, store.Account{}, zap.NewNop(), Options{})
	runner.meta, runner.generation = in.Metadata, in.Generation
	runner.snapshot["zones"] = encode(in.Zones)
	state := AccountState{Catalog: in.Catalog, Clock: in.Clock, Ships: []ShipState{in.Ship}, Routes: in.Routes,
		Markets: in.Markets, Ports: in.Ports, Contracts: in.Contracts,
		Vaults: []Vault{{AccountType: "vault", BankingRegion: "channel", BalancePence: 10_000_000}}}
	for pass := range 2 {
		if err := runner.refreshHazardQuotes(context.Background(), state); err != nil {
			t.Fatal(err)
		}
		if requests != (pass+1)*30 {
			t.Fatalf("request cap changed with laden refinements: pass %d requests %d", pass, requests)
		}
	}
	if len(primary) != 40 || primary[0] != "port-39" {
		t.Fatalf("laden refinements delayed a primary bound beyond the next pass: %v", primary)
	}
}

func TestHazardOutageUsesHighestStalePublishedLaneBoundForOneSeason(t *testing.T) {
	for _, tc := range []struct {
		name string
		age  float64
		want bool
	}{
		{"one season boundary", hazardPublishedGameHours, true},
		{"older than one season", hazardPublishedGameHours + 0.01, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			in := hazardInputFixture()
			in.Fleet = []ShipState{in.Ship, in.Ship}
			in.Fleet[1].Ship.ID = "other-owned-hull"
			key := hazardHullKey("ship-a", "origin", "destination")
			quote := in.HazardQuotes[key]
			quote.ObservedAt = in.Now.Add(-time.Duration(tc.age * float64(time.Hour) / 60))
			quote.ExpectedLossBPS, quote.Error, quote.Month = 351, "HTTP 503", 3
			in.HazardQuotes[key] = quote
			quote.ShipID, quote.ExpectedLossBPS, quote.Error = "other-owned-hull", 490, ""
			in.HazardQuotes[hazardHullKey(quote.ShipID, quote.Origin, quote.Destination)] = quote
			bps, source, ok := planHazard(in, in.Routes[routeKey("origin", "destination", "hoy")])
			if ok != tc.want || ok && (bps != 490 || !strings.Contains(source, "stale published quote")) {
				t.Fatalf("stale lane proxy = %d, %q, %t; want available %t", bps, source, ok, tc.want)
			}
		})
	}
}

func TestHazardFailedRefreshRetainsPublishedBoundAndAllowsVoyage(t *testing.T) {
	in := hazardInputFixture()
	key := hazardHullKey("ship-a", "origin", "destination")
	previous := in.HazardQuotes[key]
	previous.ExpectedLossBPS = 351
	previous.ObservedAt = in.Now.Add(-7 * time.Minute)
	in.HazardQuotes[key] = previous
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusServiceUnavailable)
	}))
	defer server.Close()
	runner := New(gameapi.New(server.URL, "synthetic-key", nil), nil, store.Account{}, zap.NewNop(), Options{})
	runner.meta, runner.generation = in.Metadata, in.Generation
	runner.snapshot["zones"] = encode(in.Zones)
	runner.snapshot[hazardQuoteSnapshotKey] = encode(in.HazardQuotes)
	state := AccountState{Catalog: in.Catalog, Clock: in.Clock, Ships: []ShipState{in.Ship}, Routes: in.Routes,
		Markets: in.Markets, Ports: in.Ports, Contracts: in.Contracts,
		Vaults: []Vault{{AccountType: "vault", BankingRegion: "channel", BalancePence: 10_000_000}}}
	if err := runner.refreshHazardQuotes(context.Background(), state); err != nil {
		t.Fatal(err)
	}
	state.HazardQuotes = decode[map[string]HazardQuote](runner.snapshot, hazardQuoteSnapshotKey)
	current := state.HazardQuotes[key]
	if current.ExpectedLossBPS != 351 || !current.ObservedAt.Equal(previous.ObservedAt) || current.Error == "" {
		t.Fatalf("refresh failure erased published evidence: %+v", current)
	}
	plans := PlanVoyages(runner.planningInputFrom(in.Ship, state))
	if len(plans) == 0 || !plans[0].Accepted || plans[0].Economics.HazardBPS != 351 ||
		!strings.Contains(plans[0].Economics.HazardSource, "stale published quote") {
		t.Fatalf("quote outage unnecessarily stopped the previously priced lane: %+v", plans)
	}
}

func TestHazardActualVoyageUsesFreshOwnedHullLaneProxy(t *testing.T) {
	in := hazardInputFixture()
	in.Fleet = []ShipState{in.Ship}
	in.Fleet[0].Ship.ID = "another-owned-hoy"
	key := hazardHullKey("ship-a", "origin", "destination")
	quote := in.HazardQuotes[key]
	quote.ShipID, quote.ExpectedLossBPS = in.Fleet[0].Ship.ID, 351
	delete(in.HazardQuotes, key)
	in.HazardQuotes[hazardHullKey(quote.ShipID, quote.Origin, quote.Destination)] = quote
	plans := PlanVoyages(in)
	if len(plans) == 0 || !plans[0].Accepted || plans[0].Economics.HazardBPS != 351 ||
		!strings.Contains(plans[0].Economics.HazardSource, "lane proxy") {
		t.Fatalf("missing hull quote ignored a fresh owned-hull lane proxy: %+v", plans)
	}
}

func TestHazardRefreshLadenQuoteIllegalPairAndRateLimit(t *testing.T) {
	for _, mode := range []string{"laden", "illegal", "throttle", "missing bound", "failed"} {
		t.Run(mode, func(t *testing.T) {
			in := hazardInputFixture()
			in.Ship.CargoUsedTons = 2
			in.Ship.CargoManifest = map[string]int64{"provisions": 2}
			var values []string
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
				values = append(values, req.URL.Query().Get("insured_value_pence"))
				switch mode {
				case "illegal":
					w.WriteHeader(400)
					_, _ = w.Write([]byte(`{"code":"bad_request","detail":"no open route"}`))
				case "throttle":
					w.Header().Set("Retry-After", "1")
					w.WriteHeader(429)
					_, _ = w.Write([]byte(`{"code":"rate_limit_exceeded"}`))
				case "failed":
					w.WriteHeader(503)
				case "missing bound":
					_, _ = w.Write([]byte(`{"max_insurable_pence":2000000}`))
				default:
					bps := 351
					if len(values) > 1 {
						bps = 490
					}
					_ = json.NewEncoder(w).Encode(map[string]any{"lloyds_expected_loss_bps": bps, "max_insurable_pence": 2_000_000})
				}
			}))
			defer server.Close()
			runner := New(gameapi.New(server.URL, "synthetic-key", nil), nil, store.Account{}, zap.NewNop(), Options{})
			runner.meta, runner.generation = in.Metadata, in.Generation
			runner.snapshot["zones"] = encode(in.Zones)
			state := AccountState{Catalog: in.Catalog, Clock: in.Clock, Ships: []ShipState{in.Ship}, Routes: in.Routes}
			started := time.Now()
			if err := runner.refreshHazardQuotes(context.Background(), state); err != nil {
				t.Fatal(err)
			}
			quotes := decode[map[string]HazardQuote](runner.snapshot, hazardQuoteSnapshotKey)
			hull := quotes[hazardHullKey("ship-a", "origin", "destination")]
			if mode == "laden" {
				laden := quotes[hazardLadenKey("origin", "destination", "hoy", 4, 3)]
				if len(values) != 2 || values[0] != "10000" || values[1] != "2000000" || hull.ExpectedLossBPS != 351 || laden.ExpectedLossBPS != 490 {
					t.Fatalf("laden quote values=%v hull=%+v laden=%+v", values, hull, laden)
				}
			} else {
				if len(values) != 1 || hull.Error == "" {
					t.Fatalf("failed quote retried or assumed zero: %v %+v", values, hull)
				}
				if mode == "illegal" {
					if !hull.Illegal {
						t.Fatal("no-open-route pair not retained as illegal")
					}
					if err := runner.refreshHazardQuotes(context.Background(), state); err != nil {
						t.Fatal(err)
					}
					if len(values) != 1 {
						t.Fatal("known illegal pair was requested again")
					}
				}
				if mode == "throttle" && time.Since(started) < time.Second {
					t.Fatal("Retry-After was not honoured")
				}
			}
		})
	}
}

func TestHazardAdjustedNetReordersPlannerRanking(t *testing.T) {
	in := hazardInputFixture()
	in.Catalog.Ports = append(in.Catalog.Ports, Port{ID: "risky", BankingRegion: "channel", Phase: 1})
	in.Ports["risky"] = PortDetail{ID: "risky", EffectiveBerths: 4}
	in.Metadata["ports/risky"] = in.Metadata["ports/destination"]
	route := in.Routes[routeKey("origin", "destination", "hoy")]
	route.ToPort = "risky"
	route.Legs = append([]RouteLeg(nil), route.Legs...)
	route.Legs[1].ToNode = "risky"
	in.Routes[routeKey("origin", "risky", "hoy")] = route
	in.Metadata["routes/"+routeKey("origin", "risky", "hoy")] = in.Metadata["routes/origin|destination|hoy"]
	reward := int64(3_100)
	contract := in.Contracts[0]
	contract.ID, contract.OriginPortID, contract.RewardPerTonPence = "risky-charter", "risky", &reward
	in.Contracts = append(in.Contracts, contract)
	seedZeroHazardQuotes(&in)
	before := PlanVoyages(in)[0]
	if !before.Accepted || before.Destination != "risky" {
		t.Fatalf("baseline did not prefer the higher revenue: %+v", before)
	}
	key := hazardHullKey("ship-a", "origin", "risky")
	quote := in.HazardQuotes[key]
	quote.ExpectedLossBPS = 2_000
	in.HazardQuotes[key] = quote
	after := PlanVoyages(in)[0]
	if !after.Accepted || after.Destination != "destination" || after.Economics.HazardSource == "" {
		t.Fatalf("hazard did not change ranking: %+v", after)
	}
}

func TestHazardFirstCutterUsesOwnedHoyLaneProxy(t *testing.T) {
	in := hazardInputFixture()
	in.Ship.Ship.CannonsCount = 0
	for key, quote := range in.HazardQuotes {
		quote.CannonsCount = 0
		in.HazardQuotes[key] = quote
	}
	in.Fleet = []ShipState{in.Ship}
	in.Fleet[0].Ship.Status = "sailing"
	cutter := in.Catalog.ShipClasses[0]
	cutter.ID = "cutter"
	cutter.HullCategory = "regional"
	in.Catalog.ShipClasses = append(in.Catalog.ShipClasses, cutter)
	for _, pair := range [][2]string{{"origin", "destination"}, {"destination", "origin"}} {
		key := routeKey(pair[0], pair[1], "hoy")
		route := in.Routes[key]
		route.ShipClass = "cutter"
		in.Routes[routeKey(pair[0], pair[1], "cutter")] = route
		in.Metadata["routes/"+routeKey(pair[0], pair[1], "cutter")] = in.Metadata["routes/"+key]
	}
	in.Ship = growthCandidate(in.Catalog.Ports[0], cutter)
	plans := PlanVoyages(in)
	if len(plans) == 0 || !plans[0].Accepted || !strings.Contains(plans[0].Economics.HazardSource, "lane proxy") {
		t.Fatalf("first cutter was blocked for lacking a cutter twin: %+v", plans)
	}
}

func TestHazardPricesFreightCollateralWithoutExpensingItsPrincipal(t *testing.T) {
	for _, kind := range []string{"freight", "charter+freight"} {
		in := hazardInputFixture()
		in.Markets["origin"].Books[0].Depth.Asks[0].PricePence = 99_999
		plan := Plan{
			Kind: kind, ShipID: "ship-a", Origin: "origin", Destination: "destination", Commodity: "provisions", Quantity: 1,
			Economics: Economics{CollateralPence: 100_000},
		}
		base := pricePlan(in, in.Catalog.ShipClasses[0], in.Catalog.Ports[0], plan, 1_000_000, 0)
		if !base.Accepted {
			t.Fatalf("unpriced collateral fixture: %+v", base)
		}
		risk := base.Economics.PurchasePence + base.Economics.CustomsPence + 100_000
		charge := ceilBPS(risk, 351)
		wantNet := int64(2_000)
		if kind == "freight" {
			wantNet = 1
		}
		quote := in.HazardQuotes[hazardHullKey("ship-a", "origin", "destination")]
		quote.ExpectedLossBPS = 351
		in.HazardQuotes[hazardHullKey("ship-a", "origin", "destination")] = quote
		revenue := base.Economics.RevenuePence - base.Economics.ExpectedNetPence + charge + wantNet
		priced := pricePlan(in, in.Catalog.ShipClasses[0], in.Catalog.Ports[0], plan, revenue, 0)
		if !priced.Accepted || priced.Economics.HazardPence != charge ||
			priced.Economics.ExpectedNetPence != wantNet || priced.Economics.CollateralPence != 100_000 {
			t.Fatalf("%s collateral not priced solely as capital at risk: %+v", kind, priced)
		}
	}
}

func TestHazardStaleProxyRespectsSeasonAndCurrentPiracy(t *testing.T) {
	for _, tc := range []struct {
		name         string
		quotedMonth  int
		passageMonth int
		quotedPiracy int64
		want         bool
	}{
		{"autumn quote cannot price winter", 10, 11, 200, false},
		{"winter months share a storm class", 11, 12, 200, true},
		{"winter crosses the year", 12, 1, 200, true},
		{"summer months share a storm class", 3, 10, 200, true},
		{"winter quote is not a summer-class quote", 2, 3, 200, false},
		{"unknown quoted month", 0, 3, 200, false},
		{"piracy increased after quote", 3, 3, 199, false},
		{"same piracy fingerprint", 3, 3, 200, true},
		{"higher quoted piracy is conservative", 3, 3, 201, true},
		{"missing target zone fingerprint", 3, 3, -1, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			in := hazardInputFixture()
			in.Clock.Calendar.Month = tc.passageMonth
			key := hazardHullKey(in.Ship.Ship.ID, "origin", "destination")
			quote := in.HazardQuotes[key]
			quote.Month, quote.Error = tc.quotedMonth, "HTTP 503"
			quote.ExpectedLossBPS, quote.ObservedAt = 351, in.Now.Add(-7*time.Minute)
			quote.NetPiracyBPS = map[string]int64{"channel": tc.quotedPiracy}
			if tc.quotedPiracy < 0 {
				quote.NetPiracyBPS = nil
			}
			in.HazardQuotes[key] = quote
			bps, source, ok := planHazard(in, in.Routes[routeKey("origin", "destination", "hoy")])
			if ok != tc.want || ok && (bps != 351 || !strings.Contains(source, "stale published quote")) {
				t.Fatalf("stale quote = %d, %q, %t; want available %t", bps, source, ok, tc.want)
			}
			if !tc.want {
				plan := pricePlan(in, in.Catalog.ShipClasses[0], in.Catalog.Ports[0], Plan{
					Kind: "charter", ShipID: in.Ship.Ship.ID, Origin: "origin", Destination: "destination",
					Commodity: "provisions", Quantity: 1,
				}, 1_000_000, 0)
				if plan.Accepted || plan.Reason != hazardUnavailable {
					t.Fatalf("unpriced passage did not fail closed with an explicit hazard reason: %+v", plan)
				}
			}
		})
	}
}

func TestHazardProxyRefusesBetterProtectedSources(t *testing.T) {
	for _, stale := range []bool{false, true} {
		for _, synthetic := range []bool{false, true} {
			for _, protection := range []string{"more cannons", "higher hull category"} {
				t.Run(fmt.Sprintf("stale=%t/growth=%t/%s", stale, synthetic, protection), func(t *testing.T) {
					in := hazardInputFixture()
					in.Fleet = []ShipState{in.Ship}
					in.Ship.Ship.ID = "unquoted-target"
					if synthetic {
						in.Ship.Ship.ID = "growth-candidate"
					}
					if protection == "more cannons" {
						in.Ship.Ship.CannonsCount = 0
					} else {
						larger := in.Catalog.ShipClasses[0]
						larger.ID, larger.HullCategory = "barque", "oceanic"
						in.Catalog.ShipClasses = append(in.Catalog.ShipClasses, larger)
						in.Fleet[0].Ship.ShipClass = larger.ID
						sourceRoute := in.Routes[routeKey("origin", "destination", "hoy")]
						sourceRoute.ShipClass = larger.ID
						in.Routes[routeKey("origin", "destination", larger.ID)] = sourceRoute
						key := hazardHullKey(in.Fleet[0].Ship.ID, "origin", "destination")
						quote := in.HazardQuotes[key]
						quote.ShipClass = larger.ID
						in.HazardQuotes[key] = quote
					}
					if stale {
						for key, quote := range in.HazardQuotes {
							quote.Error, quote.ObservedAt = "HTTP 503", in.Now.Add(-7*time.Minute)
							in.HazardQuotes[key] = quote
						}
					}
					if bps, source, ok := planHazard(in, in.Routes[routeKey("origin", "destination", "hoy")]); ok {
						t.Fatalf("better-protected source underpriced target: %d %q", bps, source)
					}
				})
			}
		}
	}
}

func TestHazardProxyTakesHighestEligibleQuoteWithoutExactPriority(t *testing.T) {
	for _, stale := range []bool{false, true} {
		t.Run(fmt.Sprintf("stale=%t", stale), func(t *testing.T) {
			in := hazardInputFixture()
			in.Ship.Ship.ID, in.Ship.Ship.CannonsCount = "growth-candidate", 0
			target := in.Catalog.ShipClasses[0]
			target.ID, target.HullCategory = "cutter", "regional"
			in.Catalog.ShipClasses = append(in.Catalog.ShipClasses, target)
			targetRoute := in.Routes[routeKey("origin", "destination", "hoy")]
			targetRoute.ShipClass = target.ID
			in.Routes[routeKey("origin", "destination", target.ID)] = targetRoute
			in.Ship.Ship.ShipClass = target.ID
			in.HazardQuotes = make(map[string]HazardQuote)
			for _, source := range []struct {
				id, class string
				cannons   int64
				bps       int64
			}{
				{"exact-cutter", "cutter", 0, 100},
				{"unarmed-hoy", "hoy", 0, 500},
				{"armed-hoy", "hoy", 4, 900},
			} {
				ship := in.Ship
				ship.Ship.ID, ship.Ship.ShipClass, ship.Ship.CannonsCount = source.id, source.class, source.cannons
				in.Fleet = append(in.Fleet, ship)
				quote := HazardQuote{
					ShipID: source.id, Origin: "origin", Destination: "destination", ShipClass: source.class,
					CannonsCount: source.cannons, Month: 3, ExpectedLossBPS: source.bps, ObservedAt: in.Now,
					NetPiracyBPS: map[string]int64{"channel": 200},
				}
				if stale {
					quote.Error, quote.ObservedAt = "HTTP 503", in.Now.Add(-7*time.Minute)
				}
				in.HazardQuotes[hazardHullKey(source.id, "origin", "destination")] = quote
			}
			bps, source, ok := planHazard(in, targetRoute)
			if !ok || bps != 500 || !strings.Contains(source, "lane proxy") {
				t.Fatalf("proxy ignored highest eligible source or selected armed source: %d %q %t", bps, source, ok)
			}
		})
	}
}

func TestHazardBoundLogsScrubAndBoundGameDerivedIDs(t *testing.T) {
	t.Parallel()
	h := newRecoveryHarness(t)
	in := hazardInputFixture()
	upstream := primaryTestKey + " tw_live_hazard_echo " + strings.Repeat("界", 600)
	origin, destination := "origin-"+upstream, "destination-"+upstream
	in.Ship.Ship.ID, in.Ship.Ship.CurrentPortID = "ship-"+upstream, origin
	in.Ship.CargoUsedTons, in.Ship.CargoManifest = 1, map[string]int64{"provisions": 1}
	in.Catalog.Ports[0].ID, in.Catalog.Ports[1].ID = origin, destination
	route := in.Routes[routeKey("origin", "destination", "hoy")]
	route.FromPort, route.ToPort = origin, destination
	in.Routes = map[string]Route{routeKey(origin, destination, "hoy"): route}
	in.Metadata["routes/"+routeKey(origin, destination, "hoy")] = in.Metadata["routes/"+routeKey("origin", "destination", "hoy")]
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]any{"lloyds_expected_loss_bps": 351, "max_insurable_pence": 2_000_000})
	}))
	defer server.Close()
	core, logs := observer.New(zap.InfoLevel)
	runner := New(gameapi.New(server.URL, "synthetic-key", nil), h.db, h.account, zap.New(core), Options{})
	runner.meta, runner.generation = in.Metadata, in.Generation
	runner.snapshot["zones"] = encode(in.Zones)
	state := AccountState{Catalog: in.Catalog, Clock: in.Clock, Ships: []ShipState{in.Ship}, Routes: in.Routes}
	if err := runner.refreshHazardQuotes(h.ctx, state); err != nil {
		t.Fatal(err)
	}
	entries := logs.FilterMessage("Lloyd's voyage hazard bounds").All()
	if len(entries) != 1 {
		t.Fatalf("laden bound did not log exactly once: %+v", entries)
	}
	fields := entries[0].ContextMap()
	for _, key := range []string{"ship_id", "origin", "destination"} {
		value, ok := fields[key].(string)
		if !ok || strings.Contains(value, primaryTestKey) || strings.Contains(value, "tw_live_hazard_echo") ||
			!strings.Contains(value, "[REDACTED]") || !utf8.ValidString(value) || utf8.RuneCountInString(value) > 515 {
			t.Fatalf("unbounded or unscrubbed upstream identifier: %s=%q", key, value)
		}
	}
	if fields["hull_only_bps"] != int64(351) || fields["laden_bps"] != int64(351) {
		t.Fatalf("text scrubbing lost numeric hazard evidence: %+v", fields)
	}
}
