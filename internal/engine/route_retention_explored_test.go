package engine

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"
)

// rev-F42: the expansion marking pass records fresh yard/partner pair routes
// for cheap unlocked candidate hulls that are not in the fleet, but retention
// indexed hulls by fleet classes only and pruned those marks. Passes after an
// explored candidate pair must still see the observed geometry: while the
// pair is selected, while it stays persisted inside its hiring floor, and
// while a hired pair remains present. The existing scan/count/byte caps hold
// and no extra route fetch may appear.
func TestRouteRetentionKeepsExploredCandidateHullPairRoutes(t *testing.T) {
	w := newIdleFallbackWorld(t)
	var mu sync.Mutex
	gets := make(map[string]int)
	base := w.h.api.server.Config.Handler
	w.h.api.server.Config.Handler = http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		switch request.URL.Path {
		case "/v1/world/clock":
			recorder := httptest.NewRecorder()
			base.ServeHTTP(recorder, request)
			var clock map[string]any
			if err := json.Unmarshal(recorder.Body.Bytes(), &clock); err != nil {
				t.Fatalf("decode fixture clock: %v", err)
			}
			// The explored candidate pair is a paid phase-2 overseas yard, so
			// the world clock must report the matching unlocked phase.
			clock["unlocked_phase"] = 2
			w.h.api.writeJSON(writer, http.StatusOK, clock)
			return
		case "/v1/world/catalog":
			recorder := httptest.NewRecorder()
			base.ServeHTTP(recorder, request)
			var catalog map[string]any
			if err := json.Unmarshal(recorder.Body.Bytes(), &catalog); err != nil {
				t.Fatalf("decode fixture catalog: %v", err)
			}
			catalog["ports"] = append(catalog["ports"].([]any),
				map[string]any{"id": "yard-a", "name": "Yard A", "banking_region": "caribbean", "sea_zone_id": "fixture_sea", "phase": 2, "has_shipyard": true, "produces": []any{"x"}},
				map[string]any{"id": "demand", "name": "Demand", "banking_region": "caribbean", "sea_zone_id": "fixture_sea", "phase": 2},
				map[string]any{"id": "history-a", "name": "History A", "banking_region": "quiet", "sea_zone_id": "fixture_sea", "phase": 1},
				map[string]any{"id": "history-b", "name": "History B", "banking_region": "quiet", "sea_zone_id": "fixture_sea", "phase": 1},
			)
			catalog["sea_zones"] = append(catalog["sea_zones"].([]any), map[string]any{"id": "fixture_sea", "zone_class": "regional", "base_storm_bps": 100, "base_piracy_bps": 0})
			catalog["commodities"] = append(catalog["commodities"].([]any),
				map[string]any{"id": "x", "base_price_pence": 100},
				map[string]any{"id": "y", "base_price_pence": 100})
			classes := catalog["ship_classes"].([]any)
			candidate := map[string]any{}
			for key, value := range classes[0].(map[string]any) {
				candidate[key] = value
			}
			candidate["id"], candidate["hull_category"] = "cutter", "regional"
			catalog["ship_classes"] = append(classes, candidate)
			w.h.api.writeJSON(writer, http.StatusOK, catalog)
			return
		case "/v1/world/port":
			if request.URL.Query().Get("port_id") == recoveryHomePort {
				recorder := httptest.NewRecorder()
				base.ServeHTTP(recorder, request)
				var detail map[string]any
				if err := json.Unmarshal(recorder.Body.Bytes(), &detail); err != nil {
					t.Fatalf("decode fixture port detail: %v", err)
				}
				at := time.Now().UTC().Add(-time.Second).Format(time.RFC3339Nano)
				ask, bid := int64(100), int64(200)
				detail["coffee_house_gossip"] = []map[string]any{
					{"subject_port_id": "yard-a", "snapshot_at": at, "commodities": []map[string]any{
						{"commodity_id": "x", "snapshot_at": at, "best_ask_pence": ask},
						{"commodity_id": "y", "snapshot_at": at, "best_ask_pence": ask},
					}},
					{"subject_port_id": "demand", "snapshot_at": at, "commodities": []map[string]any{
						{"commodity_id": "x", "snapshot_at": at, "best_bid_pence": bid},
						{"commodity_id": "y", "snapshot_at": at, "best_bid_pence": 150},
					}},
				}
				w.h.api.writeJSON(writer, http.StatusOK, detail)
				return
			}
		case "/v1/world/route":
			mu.Lock()
			gets[routeKey(request.URL.Query().Get("from"), request.URL.Query().Get("to"), request.URL.Query().Get("ship_class"))]++
			mu.Unlock()
		}
		base.ServeHTTP(writer, request)
	})
	runner := w.h.runner(primaryTestKey)
	w.observe(t, runner, 100)
	routes := decode[map[string]json.RawMessage](runner.snapshot, "routes")
	captured := DatasetMeta{UpdatedAt: time.Now().Add(-time.Hour), AttemptedAt: time.Now().Add(-time.Hour), Complete: true}
	pairKeys := []string{routeKey("yard-a", "demand", "cutter"), routeKey("demand", "yard-a", "cutter")}
	want := make(map[string]Route, len(pairKeys))
	for _, pair := range [][2]string{{"yard-a", "demand"}, {"demand", "yard-a"}} {
		key := routeKey(pair[0], pair[1], "cutter")
		route := sourcingRoute(pair[0], pair[1], 24)
		route.ShipClass = "cutter"
		want[key] = route
		routes[key], runner.meta["routes/"+key] = encode(route), captured
	}
	// Neither quiet port has presence or a yard, so these candidate-hull
	// routes cannot become required routes or a ranked exploration pair.
	offline := sourcingRoute("history-a", "history-b", 24)
	offline.ShipClass = "cutter"
	offlineKey := routeKey("history-a", "history-b", "cutter")
	routes[offlineKey], runner.meta["routes/"+offlineKey] = encode(offline), captured
	runner.snapshot["routes"] = encode(routes)

	assertRetained := func(pass int) {
		retained := decode[map[string]json.RawMessage](runner.snapshot, "routes")
		if len(retained) > SourcingRouteRetentionLimit {
			t.Fatalf("pass %d retained %d routes over the count cap", pass, len(retained))
		}
		retainedBytes := 0
		for key, value := range retained {
			retainedBytes += len(encode(value)) + len(encode(runner.meta["routes/"+key])) +
				len(encode(key)) + len(encode("routes/"+key)) + 4
		}
		if retainedBytes > SourcingRouteRetentionBytes {
			t.Fatalf("pass %d retained %d bytes over the byte cap", pass, retainedBytes)
		}
		for _, key := range pairKeys {
			route, exists := runner.stateCache.Routes[key]
			if !exists {
				t.Fatalf("pass %d pruned the explored candidate-hull pair route %s", pass, key)
			}
			if route.FromPort != want[key].FromPort || route.ToPort != want[key].ToPort || route.ShipClass != want[key].ShipClass {
				t.Fatalf("pass %d altered the explored pair route %s: %+v", pass, key, route)
			}
			if runner.meta["routes/"+key] != captured {
				t.Fatalf("pass %d restamped explored pair metadata %s: %+v", pass, key, runner.meta["routes/"+key])
			}
			if !runner.fresh("routes/" + key) {
				t.Fatalf("pass %d left the retained pair route %s unusable", pass, key)
			}
		}
		if _, exists := runner.stateCache.Routes[offlineKey]; exists {
			t.Fatalf("pass %d promoted unexplored non-fleet geometry %s", pass, offlineKey)
		}
		if _, exists := runner.meta["routes/"+offlineKey]; exists {
			t.Fatalf("pass %d kept pruned non-fleet metadata %s", pass, offlineKey)
		}
	}
	for pass, hour := range []int64{101, 102} {
		w.observe(t, runner, hour)
		assertRetained(pass)
	}

	// While the pair stays persisted, regional backoff and factor presence
	// hide it from expansionAgentYards. A pending pair inside its hiring
	// floor and a hired pair whose factors are still present must both keep
	// the fresh explored geometry for the candidate hull.
	pending := time.Now().UTC().Add(-30 * time.Minute)
	runner.snapshot["_expansion_agents"] = encode([]expansionAgent{
		{PortID: "yard-a", Partner: "demand", Region: "caribbean", CreatedAt: pending},
		{PortID: "demand", Partner: "yard-a", Region: "caribbean", CreatedAt: pending},
	})
	runner.snapshot["_expansion_agent_retries"] = encode(map[string]time.Time{"caribbean": time.Now().Add(24 * time.Hour)})
	w.observe(t, runner, 103)
	assertRetained(103)
	hired := time.Now().UTC().Add(-3 * time.Hour)
	runner.snapshot["_expansion_agents"] = encode([]expansionAgent{
		{PortID: "yard-a", Partner: "demand", Region: "caribbean", CreatedAt: hired.Add(-time.Hour), HiredAt: hired},
		{PortID: "demand", Partner: "yard-a", Region: "caribbean", CreatedAt: hired.Add(-time.Hour), HiredAt: hired},
	})
	w.h.api.setFactorPorts("yard-a", "demand")
	w.observe(t, runner, 104)
	assertRetained(104)

	mu.Lock()
	defer mu.Unlock()
	for _, key := range append(pairKeys, offlineKey) {
		if gets[key] != 0 {
			t.Fatalf("retention forced %d route fetches for %s; want none", gets[key], key)
		}
	}
}
