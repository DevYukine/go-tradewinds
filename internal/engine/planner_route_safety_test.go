package engine

import (
	"math"
	"reflect"
	"testing"
)

func TestPlanningRouteRejectsUnsafeReplayEvidence(t *testing.T) {
	for _, test := range []struct {
		name   string
		mutate func(*PlanningInput, string)
	}{
		{name: "valid"},
		{name: "missing", mutate: func(in *PlanningInput, key string) { delete(in.Routes, key) }},
		{name: "stale", mutate: func(in *PlanningInput, key string) {
			meta := in.Metadata["routes/"+key]
			meta.Stale = true
			in.Metadata["routes/"+key] = meta
		}},
		{name: "under-rated edge", mutate: func(in *PlanningInput, key string) {
			route := in.Routes[key]
			route.Legs[0].EdgeClass = "ocean"
			in.Routes[key] = route
		}},
		{name: "marque", mutate: func(in *PlanningInput, _ string) {
			in.MarqueZones = map[string]bool{"english_channel": true}
		}},
		{name: "invalid provisions", mutate: func(in *PlanningInput, key string) {
			route := in.Routes[key]
			route.SafeProvisionsTons = math.NaN()
			in.Routes[key] = route
		}},
		{name: "missing piracy", mutate: func(in *PlanningInput, _ string) { in.Zones = nil }},
	} {
		t.Run(test.name, func(t *testing.T) {
			in := sourcingInputFixture(t)
			hull := in.Catalog.ShipClasses[0]
			key := routeKey("le_havre", "amsterdam", hull.ID)
			route := in.Routes[key]
			route.Legs = []RouteLeg{
				{FromNode: "le_havre", ToNode: "english_channel", DistanceNM: 15, EdgeClass: "coastal"},
				{FromNode: "english_channel", ToNode: "amsterdam", DistanceNM: 15, EdgeClass: "coastal"},
			}
			in.Routes[key] = route
			if test.mutate != nil {
				test.mutate(&in, key)
			}
			got, err := observedPlanningRoute(in, "le_havre", "amsterdam", hull)
			if (err == nil) != (test.name == "valid") {
				t.Fatalf("route validation: got %+v, %v", got, err)
			}
			if err == nil && !reflect.DeepEqual(got, in.Routes[key]) {
				t.Fatalf("validation changed the recorded route: %+v", got)
			}
		})
	}
}

func TestPlanVoyagesRechecksRouteSafetyAcrossCalls(t *testing.T) {
	in := sourcingInputFixture(t)
	in.Ship.Ship.CurrentPortID = "amsterdam"
	seedZeroHazardQuotes(&in)
	plans := PlanVoyages(in)
	if len(plans) == 0 || !plans[0].Accepted {
		t.Fatalf("fixture lacks a profitable charter: %+v", plans)
	}
	key := "routes/" + routeKey("amsterdam", "portsmouth", "hoy")
	meta := in.Metadata[key]
	meta.Stale = true
	in.Metadata[key] = meta
	for _, plan := range PlanVoyages(in) {
		if plan.Accepted {
			t.Fatalf("next planning call reused stale route authority: %+v", plan)
		}
	}
	meta.Stale = false
	in.Metadata[key] = meta
	plans = PlanVoyages(in)
	if len(plans) == 0 || !plans[0].Accepted {
		t.Fatalf("later fresh route remained blocked by a cached failure: %+v", plans)
	}
}
