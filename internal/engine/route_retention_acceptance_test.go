package engine

import (
	"encoding/json"
	"net/http"
	"strings"
	"sync"
	"testing"
	"time"
)

func TestRouteRetentionIllegalSourcingLegSurvivesSkippedPasses(t *testing.T) {
	for _, skip := range []string{"maintenance mission", "offshore voyage", "failed admission", "full shortlist"} {
		t.Run(skip, func(t *testing.T) {
			w := newIdleFallbackWorld(t)
			var mu sync.Mutex
			gets := make(map[string]int)
			base := w.h.api.server.Config.Handler
			w.h.api.server.Config.Handler = http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
				switch request.URL.Path {
				case "/v1/world/catalog":
					catalog := w.h.api.catalogDocument()
					for _, source := range []string{"amsterdam", "rouen", "calais", "ostend"} {
						catalog["ports"] = append(catalog["ports"].([]any), map[string]any{"id": source, "banking_region": "north", "sea_zone_id": "zone-north", "phase": 1, "manufactures": []string{"sawn_planks"}})
					}
					// A yard candidate is not a fleet hull until the full-shortlist pass.
					wide := map[string]any{}
					for key, value := range catalog["ship_classes"].([]any)[0].(map[string]any) {
						wide[key] = value
					}
					wide["id"], wide["hull_category"] = "cutter", "oceanic"
					catalog["ship_classes"] = append(catalog["ship_classes"].([]any), wide)
					w.h.api.writeJSON(writer, http.StatusOK, catalog)
					return
				case "/v1/world/route":
					from, to, class := request.URL.Query().Get("from"), request.URL.Query().Get("to"), request.URL.Query().Get("ship_class")
					mu.Lock()
					gets[routeKey(from, to, class)]++
					mu.Unlock()
					route := w.h.api.routeDocument(from, to)
					route["ship_class"] = class
					if from == "amsterdam" && to == recoveryFarPort || to == "amsterdam" && from == recoveryFarPort {
						route["legs"].([]any)[0].(map[string]any)["edge_class"] = "oceanic"
					}
					w.h.api.writeJSON(writer, http.StatusOK, route)
					return
				}
				base.ServeHTTP(writer, request)
			})
			runner := w.h.runner(primaryTestKey)
			w.observe(t, runner, 100)
			key := routeKey("amsterdam", recoveryFarPort, recoveryHull)
			captured := runner.meta["routes/"+key]
			if _, exists := runner.stateCache.Routes[key]; !exists || !runner.fresh("routes/"+key) {
				t.Fatal("cold refresh did not capture the illegal source/charter leg")
			}
			mu.Lock()
			coldGETs := gets[key]
			mu.Unlock()
			if coldGETs != 1 {
				t.Fatalf("cold illegal leg GETs = %d, want 1", coldGETs)
			}
			switch skip {
			case "maintenance mission":
				w.h.putMission(recoveryShipBlastoise, "retention-maintenance", "maintenance", missionData{Plan: Plan{Kind: "maintenance", ShipID: recoveryShipBlastoise, Origin: recoveryHomePort}})
			case "offshore voyage":
				w.h.api.setFleet([]map[string]any{sailingShipRow(recoveryShipBlastoise, "Blastoise", recoveryHomePort, recoveryFarPort)})
			case "failed admission":
				w.mu.Lock()
				w.paused = true
				w.mu.Unlock()
			case "full shortlist":
				first := destinationShipRow(recoveryShipLapras, "Lapras", recoveryHomePort, "berthed", 5, nil, nil)
				first["ship"].(map[string]any)["ship_class"] = "cutter"
				w.h.api.setFleet([]map[string]any{first, sourcingReviewShipRow()})
			}
			for range 2 {
				w.observe(t, runner, 101)
				if _, exists := runner.stateCache.Routes[key]; !exists || runner.meta["routes/"+key] != captured {
					t.Errorf("%s discarded or restamped the cached illegal leg: %+v", skip, runner.meta["routes/"+key])
				}
			}
			if err := w.h.db.DeleteMission(w.h.ctx, w.h.account.ID, recoveryShipBlastoise); err != nil {
				t.Fatal(err)
			}
			w.h.api.setFleet([]map[string]any{sourcingReviewShipRow()})
			w.mu.Lock()
			w.paused = false
			w.mu.Unlock()
			w.observe(t, runner, 102)
			plans := planSourcingRelocation(w.input(t, runner, recoveryShipBlastoise))
			found := false
			for _, plan := range plans {
				if plan.Destination == "amsterdam" {
					found = true
					if plan.Accepted || !strings.HasPrefix(plan.Reason, "hull coastal is below route edge") {
						t.Errorf("cached illegal candidate no longer rejects for route legality: %+v", plan)
					}
				}
			}
			if !found {
				t.Fatal("source candidate disappeared")
			}
			mu.Lock()
			got := gets[key]
			mu.Unlock()
			if got != 1 {
				t.Errorf("illegal leg fetched %d times, want the initial GET and 0 extra GETs", got)
			}
		})
	}
}

func TestRouteRetentionPrunesUnrequestedLegalAndNonFleetRoutesAndPhaseChanges(t *testing.T) {
	w := newIdleFallbackWorld(t)
	base := w.h.api.server.Config.Handler
	w.h.api.server.Config.Handler = http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		if request.URL.Path == "/v1/world/catalog" {
			catalog := w.h.api.catalogDocument()
			for _, source := range []string{"amsterdam", "rouen", "calais", "ostend"} {
				catalog["ports"] = append(catalog["ports"].([]any), map[string]any{"id": source, "banking_region": "north", "sea_zone_id": "zone-north", "phase": 1, "manufactures": []string{"sawn_planks"}})
			}
			yardHull := map[string]any{}
			for key, value := range catalog["ship_classes"].([]any)[0].(map[string]any) {
				yardHull[key] = value
			}
			yardHull["id"] = "cutter"
			catalog["ship_classes"] = append(catalog["ship_classes"].([]any), yardHull)
			w.h.api.writeJSON(writer, http.StatusOK, catalog)
			return
		}
		base.ServeHTTP(writer, request)
	})
	runner := w.h.runner(primaryTestKey)
	w.observe(t, runner, 100)
	routes := decode[map[string]json.RawMessage](runner.snapshot, "routes")
	fresh := runner.meta["routes/"+routeKey(recoveryHomePort, recoveryFarPort, recoveryHull)]
	keys := []string{
		routeKey("amsterdam", recoveryFarPort, recoveryHull),
		routeKey("rouen", recoveryFarPort, "cutter"),
		routeKey("calais", recoveryFarPort, recoveryHull),
	}
	for i, key := range keys {
		route := sourcingRoute([]string{"amsterdam", "rouen", "calais"}[i], recoveryFarPort, 24)
		if i > 0 {
			route.Legs[0].EdgeClass = "oceanic"
		}
		if i == 1 {
			route.ShipClass = "cutter"
		}
		routes[key], runner.meta["routes/"+key] = encode(route), fresh
	}
	runner.snapshot["routes"] = encode(routes)
	w.h.putMission(recoveryShipBlastoise, "prune-maintenance", "maintenance", missionData{Plan: Plan{Kind: "maintenance", ShipID: recoveryShipBlastoise, Origin: recoveryHomePort}})
	w.observe(t, runner, 101)
	for _, key := range keys[:2] {
		if _, exists := runner.stateCache.Routes[key]; exists {
			t.Errorf("unrequested legal or non-fleet geometry retained: %s", key)
		}
		if _, exists := runner.meta["routes/"+key]; exists {
			t.Errorf("pruned route metadata retained: %s", key)
		}
	}
	if _, exists := runner.stateCache.Routes[keys[2]]; !exists {
		t.Error("illegal current-fleet route touching an open charter was pruned")
	}
	// Simulate a phase boundary; no sourcing mission can request these pairs.
	runner.snapshot["_route_phase"] = encode(0)
	w.observe(t, runner, 102)
	if _, exists := runner.stateCache.Routes[keys[2]]; exists {
		t.Fatal("phase boundary retained previously illegal route geometry")
	}
}

func TestRouteRetentionInspectionBudgetCountsOnlyPairsNeedingGET(t *testing.T) {
	w := newIdleFallbackWorld(t)
	var sourceGETs int
	var mu sync.Mutex
	base := w.h.api.server.Config.Handler
	w.h.api.server.Config.Handler = http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		if request.URL.Path == "/v1/world/route" && request.URL.Query().Get("from") != recoveryHomePort && request.URL.Query().Get("to") != recoveryHomePort {
			mu.Lock()
			sourceGETs++
			mu.Unlock()
		}
		base.ServeHTTP(writer, request)
	})
	runner := w.h.runner(primaryTestKey)
	w.observe(t, runner, 100)
	in := w.input(t, runner, recoveryShipBlastoise)
	candidates := sourcingCandidates(in)
	if len(candidates) != 4 {
		t.Fatalf("fixture candidates = %d, want four: %+v", len(candidates), candidates)
	}
	routes := decode[map[string]json.RawMessage](runner.snapshot, "routes")
	for _, candidate := range candidates[:3] {
		if !candidate.Accepted {
			t.Fatalf("higher-ranked candidate must pass initial admission: %+v", candidate)
		}
		for _, pair := range [][2]string{{candidate.Destination, recoveryFarPort}, {recoveryFarPort, candidate.Destination}} {
			key := routeKey(pair[0], pair[1], recoveryHull)
			route, exists := in.Routes[key]
			if !exists {
				t.Fatalf("higher-ranked source did not retain its cold route: %s", key)
			}
			// Legal geometry, but continuation stores cannot fit this hull.
			route.SafeProvisionsTons = 10_000
			in.Routes[key], routes[key] = route, encode(route)
		}
		if checked := validateRelocationTarget(in, candidate); checked.Accepted {
			t.Fatalf("higher-ranked target must fail its continuation gate: %+v", checked)
		}
	}
	last := candidates[3]
	if !last.Accepted {
		t.Fatalf("lower-ranked source must be funded and legal: %+v", last)
	}
	lastKey := routeKey(last.Destination, recoveryFarPort, recoveryHull)
	if _, exists := routes[lastKey]; exists {
		t.Fatalf("fourth source must still need a GET: %s", lastKey)
	}
	runner.snapshot["routes"] = encode(routes)
	mu.Lock()
	sourceGETs = 0
	mu.Unlock()
	w.observe(t, runner, 101)
	if _, exists := runner.stateCache.Routes[lastKey]; !exists || !runner.fresh("routes/"+lastKey) {
		t.Error("three retained pairs consumed the new-pair inspection budget; fourth source not inspected")
	}
	mu.Lock()
	got := sourceGETs
	mu.Unlock()
	if got != 2 || got > 6 {
		t.Errorf("source route GETs = %d, want only the two unknown directions and never more than 6", got)
	}
	// Cache observations must keep their original time rather than renewing TTL.
	if runner.meta["routes/"+lastKey].UpdatedAt.Before(time.Now().Add(-time.Minute)) && got == 2 {
		t.Fatal("new pair did not receive a current successful observation")
	}
}
