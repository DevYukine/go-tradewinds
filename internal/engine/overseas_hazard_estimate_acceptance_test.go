package engine

// Required production surface:
//   type hazardCalibration (opaque; no fields are required by these tests)
//   PlanningInput.hazardEstimate *hazardCalibration
//   func calibrateZoneHazard(in PlanningInput, hull Hull, cannons int64) *hazardCalibration
// The existing planHazard entry point remains unchanged.

import (
	"fmt"
	"strings"
	"testing"
	"time"
)

func overseasHazardFixture(count int) (PlanningInput, Route, Hull) {
	in := hazardInputFixture()
	in.Now = time.Date(2026, 7, 15, 12, 0, 0, 0, time.UTC)
	in.Clock.RealUTC = in.Now
	in.Clock.Calendar.Month = 7
	for key, meta := range in.Metadata {
		meta.UpdatedAt, meta.AttemptedAt = in.Now, in.Now
		in.Metadata[key] = meta
	}
	in.Catalog.SeaZones = []Zone{
		{ID: "sample-water", ZoneClass: "coastal", BaseStormBPS: 100},
		{ID: "target-water", ZoneClass: "regional", BaseStormBPS: 101},
	}
	in.Zones = []ZoneObservation{
		{SeaZoneID: "sample-water", NetPiracyBPS: 100},
		{SeaZoneID: "target-water", NetPiracyBPS: 100},
	}
	for i := range in.Catalog.Ports {
		in.Catalog.Ports[i].SeaZoneID = "target-water"
	}
	source := in.Ship
	source.Ship.ID = "calibration-owner"
	in.Fleet = []ShipState{source}
	target := in.Catalog.ShipClasses[0]
	target.ID, target.HullCategory = "brigantine", "regional"
	in.Catalog.ShipClasses = append(in.Catalog.ShipClasses, target)
	in.Ship.Ship.ID, in.Ship.Ship.ShipClass = "growth-candidate", target.ID
	in.HazardQuotes = make(map[string]HazardQuote)
	in.Routes = make(map[string]Route)
	for i := range count {
		portID := fmt.Sprintf("sample-port-%02d", i)
		in.Catalog.Ports = append(in.Catalog.Ports, Port{ID: portID, SeaZoneID: "sample-water", Phase: 1})
		route := Route{
			FromPort: "origin", ToPort: portID, ShipClass: source.Ship.ShipClass, TotalDistanceNM: 200,
			Legs: []RouteLeg{
				{FromNode: "origin", ToNode: "sample-water", EdgeClass: "coastal", DistanceNM: 100},
				{FromNode: "sample-water", ToNode: portID, EdgeClass: "coastal", DistanceNM: 100},
			},
		}
		in.Routes[routeKey(route.FromPort, route.ToPort, route.ShipClass)] = route
		in.HazardQuotes[fmt.Sprintf("sample-%02d", i)] = HazardQuote{
			ShipID: source.Ship.ID, Origin: "origin", Destination: portID, ShipClass: source.Ship.ShipClass,
			CannonsCount: 4, Month: 7, Laden: true, ExpectedLossBPS: 10,
			ObservedAt: in.Now, NetPiracyBPS: map[string]int64{"sample-water": 100},
		}
	}
	route := Route{
		FromPort: "origin", ToPort: "destination", ShipClass: target.ID,
		TotalDistanceNM: 200, SafeProvisionsTons: 0.1,
		Legs: []RouteLeg{
			{FromNode: "origin", ToNode: "target-water", EdgeClass: "regional", DistanceNM: 100},
			{FromNode: "target-water", ToNode: "destination", EdgeClass: "regional", DistanceNM: 100},
		},
	}
	in.Routes[routeKey(route.FromPort, route.ToPort, route.ShipClass)] = route
	in.Metadata["routes/"+routeKey(route.FromPort, route.ToPort, route.ShipClass)] = in.Metadata["routes/origin|destination|hoy"]
	return in, route, target
}

func TestOverseasHazardEstimateSampleCountArithmeticAndLabel(t *testing.T) {
	for _, count := range []int{9, 10} {
		t.Run(fmt.Sprintf("n=%d", count), func(t *testing.T) {
			in, route, hull := overseasHazardFixture(count)
			in.hazardEstimate = calibrateZoneHazard(in, hull, 4)
			bps, label, ok := planHazard(in, route)
			if count == 9 {
				if ok {
					t.Fatalf("nine quotes supplied an estimate: %d %q", bps, label)
				}
				return
			}
			// Each sampled route has Z=400, so k=10/400. Both target
			// arrivals cost 201 bps, including the arrival at a port.
			if !ok || bps != 11 {
				t.Fatalf("ceil(0.025*402) = %d, %q, %t; want 11 bps", bps, label, ok)
			}
			for _, fragment := range []string{"k=0.025", "n=10", "Z=402"} {
				if !strings.Contains(label, fragment) {
					t.Errorf("estimate label %q lacks %q", label, fragment)
				}
			}
			if !strings.HasPrefix(label, "zone estimate") {
				t.Fatalf("estimate mislabelled as a Lloyd's quote: %q", label)
			}
		})
	}
}

func TestOverseasHazardEstimateUsesP90NotMeanOrMaximum(t *testing.T) {
	in, route, hull := overseasHazardFixture(20)
	outlier := in.HazardQuotes["sample-19"]
	outlier.ExpectedLossBPS = 1_000
	in.HazardQuotes["sample-19"] = outlier
	in.hazardEstimate = calibrateZoneHazard(in, hull, 4)
	if bps, label, ok := planHazard(in, route); !ok || bps != 11 {
		t.Fatalf("p90 was replaced by the mean or maximum: %d %q %t", bps, label, ok)
	}
}

func TestOverseasHazardEstimateFiltersCalibrationEvidence(t *testing.T) {
	for _, tc := range []struct {
		name   string
		change func(*PlanningInput, *HazardQuote)
		want   bool
	}{
		{"ninety game days", func(in *PlanningInput, q *HazardQuote) {
			q.ObservedAt = in.Now.Add(-90 * 24 * time.Hour / 60)
		}, true},
		{"older than ninety game days", func(in *PlanningInput, q *HazardQuote) {
			q.ObservedAt = in.Now.Add(-90*24*time.Hour/60 - time.Nanosecond)
		}, false},
		{"future quote", func(in *PlanningInput, q *HazardQuote) { q.ObservedAt = in.Now.Add(time.Second) }, false},
		{"same non-winter season", func(_ *PlanningInput, q *HazardQuote) { q.Month = 3 }, true},
		{"other storm season", func(_ *PlanningInput, q *HazardQuote) { q.Month = 11 }, false},
		{"same winter season", func(in *PlanningInput, q *HazardQuote) {
			in.Clock.Calendar.Month = 2
			for key, quote := range in.HazardQuotes {
				quote.Month = 12
				in.HazardQuotes[key] = quote
			}
			q.Month = 11
		}, true},
		{"higher rated source", func(in *PlanningInput, q *HazardQuote) {
			in.Catalog.ShipClasses[0].HullCategory = "oceanic"
		}, false},
		{"equal rated source", func(in *PlanningInput, q *HazardQuote) {
			in.Catalog.ShipClasses[0].HullCategory = "regional"
		}, true},
		{"more cannons than target", func(_ *PlanningInput, q *HazardQuote) { q.CannonsCount = 5 }, false},
		{"fewer cannons than target", func(_ *PlanningInput, q *HazardQuote) { q.CannonsCount = 3 }, true},
		{"hull-only quote", func(_ *PlanningInput, q *HazardQuote) { q.Laden = false }, false},
		{"failed quote", func(_ *PlanningInput, q *HazardQuote) { q.Error = "HTTP 503" }, false},
		{"illegal quote", func(_ *PlanningInput, q *HazardQuote) { q.Illegal = true }, false},
		{"unowned quote", func(_ *PlanningInput, q *HazardQuote) { q.ShipID = "someone-else" }, false},
		{"sunk owner", func(in *PlanningInput, _ *HazardQuote) { in.Fleet[0].Ship.Status = "sunk" }, false},
		{"unknown route", func(in *PlanningInput, q *HazardQuote) {
			delete(in.Routes, routeKey(q.Origin, q.Destination, q.ShipClass))
		}, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			in, route, hull := overseasHazardFixture(10)
			quote := in.HazardQuotes["sample-00"]
			tc.change(&in, &quote)
			in.HazardQuotes["sample-00"] = quote
			in.hazardEstimate = calibrateZoneHazard(in, hull, 4)
			bps, label, ok := planHazard(in, route)
			if ok != tc.want || ok && bps != 11 {
				t.Fatalf("availability after %s = %d %q %t; want %t", tc.name, bps, label, ok, tc.want)
			}
		})
	}
}

func TestOverseasHazardEstimateLegLimitsAndPortResolution(t *testing.T) {
	for _, tc := range []struct {
		name   string
		change func(*PlanningInput, *Route)
		want   int64
		ok     bool
	}{
		{"twice sampled leg", func(_ *PlanningInput, route *Route) { route.Legs[0].DistanceNM = 200 }, 11, true},
		{"one nautical mile beyond", func(_ *PlanningInput, route *Route) { route.Legs[0].DistanceNM = 201 }, 0, false},
		{"one and a half sampled risk", func(in *PlanningInput, _ *Route) { in.Catalog.SeaZones[1].BaseStormBPS = 200 }, 15, true},
		{"one basis point beyond risk", func(in *PlanningInput, _ *Route) { in.Catalog.SeaZones[1].BaseStormBPS = 201 }, 0, false},
		{"repeated zone arrivals count", func(_ *PlanningInput, route *Route) {
			route.Legs = append(route.Legs, RouteLeg{FromNode: "destination", ToNode: "target-water", EdgeClass: "regional", DistanceNM: 50})
		}, 16, true},
		{"port to port resolves arrival zone", func(_ *PlanningInput, route *Route) {
			route.Legs = []RouteLeg{{FromNode: "origin", ToNode: "destination", EdgeClass: "regional", DistanceNM: 100}}
		}, 6, true},
		{"unknown arrival is unavailable", func(_ *PlanningInput, route *Route) { route.Legs[1].ToNode = "unknown-port" }, 0, false},
		{"quote fingerprint not current sample piracy", func(in *PlanningInput, _ *Route) { in.Zones[0].NetPiracyBPS = 900 }, 11, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			in, route, hull := overseasHazardFixture(10)
			tc.change(&in, &route)
			in.hazardEstimate = calibrateZoneHazard(in, hull, 4)
			bps, label, ok := planHazard(in, route)
			if ok != tc.ok || ok && bps != tc.want {
				t.Fatalf("leg bound = %d %q %t; want %d %t", bps, label, ok, tc.want, tc.ok)
			}
		})
	}
	in, route, hull := overseasHazardFixture(10)
	for key, quote := range in.HazardQuotes {
		quote.ExpectedLossBPS = 10_000
		in.HazardQuotes[key] = quote
	}
	in.Catalog.SeaZones[1].BaseStormBPS = 200
	in.hazardEstimate = calibrateZoneHazard(in, hull, 4)
	if bps, _, ok := planHazard(in, route); !ok || bps != 10_000 {
		t.Fatalf("estimate did not cap at 10,000 bps: %d %t", bps, ok)
	}
}

func TestOverseasHazardQuotesAndProxiesPrecedeEstimate(t *testing.T) {
	for _, mode := range []string{"owned quote", "owned lane proxy", "stale published proxy", "failed refresh without usable proxy", "no quote"} {
		t.Run(mode, func(t *testing.T) {
			in, route, hull := overseasHazardFixture(10)
			in.hazardEstimate = calibrateZoneHazard(in, hull, 4)
			quote := HazardQuote{
				ShipID: in.Fleet[0].Ship.ID, Origin: route.FromPort, Destination: route.ToPort,
				ShipClass: "hoy", CannonsCount: 4, Month: in.Clock.Calendar.Month,
				ExpectedLossBPS: 777, ObservedAt: in.Now,
				NetPiracyBPS: map[string]int64{"target-water": 100},
			}
			proxyRoute := route
			proxyRoute.ShipClass = quote.ShipClass
			in.Routes[routeKey(route.FromPort, route.ToPort, quote.ShipClass)] = proxyRoute
			if mode == "owned quote" {
				in.Ship = in.Fleet[0]
				in.Catalog.ShipClasses[0].HullCategory = "regional"
			}
			if mode == "stale published proxy" || mode == "failed refresh without usable proxy" {
				quote.ObservedAt, quote.Error = in.Now.Add(-7*time.Minute), "HTTP 503"
			}
			if mode == "failed refresh without usable proxy" {
				quote.CannonsCount = 5
			}
			if mode != "no quote" {
				in.HazardQuotes[hazardHullKey(quote.ShipID, quote.Origin, quote.Destination)] = quote
			}
			bps, label, ok := planHazard(in, route)
			estimate := mode == "no quote" || mode == "failed refresh without usable proxy"
			want := int64(777)
			if estimate {
				want = 11
			}
			if !ok || bps != want || strings.HasPrefix(label, "zone estimate") != estimate {
				t.Fatalf("%s priority: %d %q %t; want %d estimate=%t", mode, bps, label, ok, want, estimate)
			}
		})
	}
}

func TestOverseasHazardEstimateRemainsGrowthOnlyAndNilFailsClosed(t *testing.T) {
	for _, ordinaryShip := range []bool{false, true} {
		t.Run(fmt.Sprintf("ordinary_ship=%t", ordinaryShip), func(t *testing.T) {
			in, route, hull := overseasHazardFixture(10)
			if ordinaryShip {
				in.Ship.Ship.ID = "ordinary-unquoted-ship"
				in.hazardEstimate = calibrateZoneHazard(in, hull, 4)
			}
			if bps, label, ok := planHazard(in, route); ok {
				t.Fatalf("unscoped estimate priced an unquoted ship: %d %q", bps, label)
			}
			plan := pricePlan(in, hull, in.Catalog.Ports[0], Plan{
				Kind: "charter", ShipID: in.Ship.Ship.ID, Origin: route.FromPort,
				Destination: route.ToPort, Commodity: "provisions", Quantity: 1,
			}, 1_000_000, 0)
			if plan.Accepted || plan.Reason != hazardUnavailable {
				t.Fatalf("nil/unscoped estimate did not preserve hazardUnavailable: %+v", plan)
			}
		})
	}
}

func TestOverseasHazardSampleAndTargetUseSameArrivalZoneSet(t *testing.T) {
	in, route, hull := overseasHazardFixture(10)
	in.Catalog.SeaZones = append(in.Catalog.SeaZones, Zone{ID: "arrival-water", ZoneClass: "regional", BaseStormBPS: 300})
	in.Zones = append(in.Zones, ZoneObservation{SeaZoneID: "arrival-water"})
	for i := range in.Catalog.Ports {
		if strings.HasPrefix(in.Catalog.Ports[i].ID, "sample-port-") || in.Catalog.Ports[i].ID == "destination" {
			in.Catalog.Ports[i].SeaZoneID = "arrival-water"
		}
	}
	for key, quote := range in.HazardQuotes {
		quote.NetPiracyBPS["arrival-water"] = 0
		in.HazardQuotes[key] = quote
	}
	route.Legs[0].ToNode, route.Legs[1].FromNode = "sample-water", "sample-water"
	in.hazardEstimate = calibrateZoneHazard(in, hull, 4)
	// Both quote and target have one 200-bps zone arrival and one
	// 300-bps port-zone arrival: Z_q=Z=500, so the estimate stays 10 bps.
	if bps, label, ok := planHazard(in, route); !ok || bps != 10 || !strings.Contains(label, "Z=500") {
		t.Fatalf("sample and target used different arrival-zone sets: %d %q %t", bps, label, ok)
	}
}
