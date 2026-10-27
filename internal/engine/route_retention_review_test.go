package engine

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"sort"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/DevYukine/go-tradewinds/internal/store"
	"go.uber.org/zap"
)

func TestRouteRetentionReviewBoundsMultiOriginHistory(t *testing.T) {
	for _, padding := range []int{0, 32 * 1024} {
		t.Run(fmt.Sprintf("padding-%d", padding), func(t *testing.T) {
			w := newIdleFallbackWorld(t)
			w.h.api.setFleet([]map[string]any{sailingShipRow(recoveryShipBlastoise, "Blastoise", recoveryHomePort, recoveryFarPort)})
			w.h.api.setFactorPorts()
			var sources, destinations []string
			for i := range 20 {
				sources = append(sources, fmt.Sprintf("visited-source-%02d", i))
			}
			var charters []Contract
			for i := range 16 {
				port := fmt.Sprintf("charter-port-%02d", i)
				destinations = append(destinations, port)
				charter := sourcingReviewCharter(port)
				charter.OriginPortID = port
				charters = append(charters, charter)
			}
			base := w.h.api.server.Config.Handler
			w.h.api.server.Config.Handler = http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
				switch request.URL.Path {
				case "/v1/world/catalog":
					catalog := w.h.api.catalogDocument()
					for _, port := range append(append([]string(nil), sources...), destinations...) {
						catalog["ports"] = append(catalog["ports"].([]any), map[string]any{
							"id": port, "banking_region": "north", "sea_zone_id": "zone-north", "phase": 1,
							"manufactures": []string{"sawn_planks"},
						})
					}
					w.h.api.writeJSON(writer, http.StatusOK, catalog)
					return
				case "/v1/contract/list":
					w.h.api.writeJSON(writer, http.StatusOK, charters)
					return
				}
				base.ServeHTTP(writer, request)
			})
			runner := w.h.runner(primaryTestKey)
			w.observe(t, runner, 100)
			routes := make(map[string]json.RawMessage)
			original := make(map[string]DatasetMeta)
			captured := DatasetMeta{UpdatedAt: time.Now().Add(-time.Hour), AttemptedAt: time.Now().Add(-time.Hour), Complete: true, Generation: "visited-origins"}
			for _, source := range sources {
				for _, target := range destinations {
					route := sourcingRoute(source, target, 24)
					route.Legs[0].EdgeClass = "oceanic"
					key := routeKey(source, target, recoveryHull)
					routes[key] = encode(struct {
						Route
						Padding string `json:"padding"`
					}{route, strings.Repeat("x", padding)})
					runner.meta["routes/"+key] = captured
					original[key] = captured
				}
			}
			// Ordinary visited ports without public supply are not source candidates.
			unrelated := sourcingRoute("abandoned-no-supply", destinations[0], 24)
			unrelated.Legs[0].EdgeClass = "oceanic"
			unrelatedKey := routeKey(unrelated.FromPort, unrelated.ToPort, recoveryHull)
			routes[unrelatedKey], runner.meta["routes/"+unrelatedKey] = encode(unrelated), captured
			runner.snapshot["routes"] = encode(routes)
			if padding > 0 && len(encode(runner.snapshot)) <= storeSnapshotBudgetBytes {
				t.Fatal("multi-origin route history must exceed the store budget before pruning")
			}
			w.observe(t, runner, 101)
			retained := decode[map[string]json.RawMessage](runner.snapshot, "routes")
			if len(retained) == 0 || len(retained) > sourcingRouteRetentionLimit {
				t.Fatalf("retained route count is not bounded: %d", len(retained))
			}
			if padding == 0 && len(retained) != sourcingRouteRetentionLimit {
				t.Fatalf("count cap retained %d routes, want %d", len(retained), sourcingRouteRetentionLimit)
			}
			if _, exists := retained[unrelatedKey]; exists {
				t.Fatal("retention kept ordinary geometry unrelated to a public source")
			}
			bytes := 0
			var keys []string
			for key, value := range retained {
				keys = append(keys, key)
				meta := runner.meta["routes/"+key]
				if meta != original[key] {
					t.Fatalf("retention restamped route %s: %+v", key, meta)
				}
				bytes += len(encode(value)) + len(encode(meta)) + len(encode(key)) + len(encode("routes/"+key)) + 4
			}
			if bytes > sourcingRouteRetentionBytes || len(encode(runner.snapshot)) >= storeSnapshotBudgetBytes {
				t.Fatalf("retained history exceeded its byte or snapshot budget: retention=%d snapshot=%d", bytes, len(encode(runner.snapshot)))
			}
			sort.Strings(keys)
			w.observe(t, runner, 102)
			var again []string
			for key := range runner.stateCache.Routes {
				again = append(again, key)
			}
			sort.Strings(again)
			if strings.Join(keys, "\n") != strings.Join(again, "\n") {
				t.Fatal("unchanged history did not retain a deterministic bounded set")
			}
		})
	}
}

func TestRouteRetentionReviewFullShortlistReallySkipsLaterHull(t *testing.T) {
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
			if from != recoveryHomePort && to != recoveryHomePort {
				mu.Lock()
				gets[class]++
				mu.Unlock()
			}
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
	first := destinationShipRow(recoveryShipLapras, "Lapras", recoveryHomePort, "berthed", 5, nil, nil)
	first["ship"].(map[string]any)["ship_class"] = "cutter"
	w.h.api.setFleet([]map[string]any{first, sourcingReviewShipRow()})
	mu.Lock()
	clear(gets)
	mu.Unlock()
	w.observe(t, runner, 101)
	in := w.input(t, runner, recoveryShipLapras)
	if reason := sourcingAdmissionRejection(in); reason != "" {
		t.Fatalf("Lapras did not reach the shortlist: %s", reason)
	}
	if accepted := acceptedRelocations(planSourcingRelocation(in)); len(accepted) != sourcingPairLimit {
		t.Fatalf("Lapras did not fill the three-pair shortlist: %+v", accepted)
	}
	mu.Lock()
	cutterGETs, hoyGETs := gets["cutter"], gets[recoveryHull]
	mu.Unlock()
	if cutterGETs != 6 || hoyGETs != 0 {
		t.Fatalf("full shortlist did not skip Blastoise: Lapras GETs=%d Blastoise GETs=%d", cutterGETs, hoyGETs)
	}
	if runner.meta["routes/"+key] != captured {
		t.Fatal("a genuinely full earlier shortlist discarded the later hull's rejection")
	}
}

func TestRouteRetentionReviewExpiredIllegalPairsShareGETBudget(t *testing.T) {
	w := newIdleFallbackWorld(t)
	var mu sync.Mutex
	gets := 0
	base := w.h.api.server.Config.Handler
	w.h.api.server.Config.Handler = http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		if request.URL.Path == "/v1/world/route" && request.URL.Query().Get("from") != recoveryHomePort && request.URL.Query().Get("to") != recoveryHomePort {
			mu.Lock()
			gets++
			mu.Unlock()
			route := w.h.api.routeDocument(request.URL.Query().Get("from"), request.URL.Query().Get("to"))
			route["ship_class"] = request.URL.Query().Get("ship_class")
			route["legs"].([]any)[0].(map[string]any)["edge_class"] = "oceanic"
			w.h.api.writeJSON(writer, http.StatusOK, route)
			return
		}
		base.ServeHTTP(writer, request)
	})
	runner := w.h.runner(primaryTestKey)
	w.observe(t, runner, 100)
	routes := decode[map[string]json.RawMessage](runner.snapshot, "routes")
	for _, source := range []string{"amsterdam", "rouen", "calais", "ostend"} {
		for _, pair := range [][2]string{{source, recoveryFarPort}, {recoveryFarPort, source}} {
			key := routeKey(pair[0], pair[1], recoveryHull)
			route := sourcingRoute(pair[0], pair[1], 24)
			route.Legs[0].EdgeClass = "oceanic"
			routes[key] = encode(route)
			runner.meta["routes/"+key] = DatasetMeta{UpdatedAt: time.Now().Add(-routeGeometryTTL - time.Minute), Complete: true}
		}
	}
	runner.snapshot["routes"] = encode(routes)
	mu.Lock()
	gets = 0
	mu.Unlock()
	w.observe(t, runner, 101)
	mu.Lock()
	first := gets
	gets = 0
	mu.Unlock()
	w.observe(t, runner, 102)
	mu.Lock()
	second := gets
	mu.Unlock()
	if first != 6 || second != 2 {
		t.Fatalf("expired illegal pairs escaped or stalled the shared budget: first=%d second=%d", first, second)
	}
}

func TestRouteRetentionReviewRejectsResponseIdentityMismatch(t *testing.T) {
	for _, field := range []string{"from_port", "to_port", "ship_class"} {
		t.Run(field, func(t *testing.T) {
			runner := New(nil, nil, store.Account{}, zap.NewNop(), Options{})
			runner.generation = "identity-review"
			now := time.Now()
			route := map[string]any{"from_port": recoveryHomePort, "to_port": recoveryFarPort, "ship_class": recoveryHull}
			route[field] = "different-identity"
			key := "routes/" + routeKey(recoveryHomePort, recoveryFarPort, recoveryHull)
			if runner.applyCapture(context.Background(), key, fetchResult{data: encode(route), attemptedAt: now, completedAt: now}) {
				t.Fatal("mismatched route response became authoritative")
			}
			if _, exists := runner.snapshot[key]; exists || !runner.meta[key].Stale || runner.meta[key].Error == "" {
				t.Fatalf("identity mismatch lost visible failure state: %+v", runner.meta[key])
			}
		})
	}
}

func TestRouteRetentionReviewEvictsAgainstCurrentSnapshotHeadroom(t *testing.T) {
	for _, whitespace := range []bool{false, true} {
		t.Run(fmt.Sprintf("raw-whitespace-%t", whitespace), func(t *testing.T) {
			w := newIdleFallbackWorld(t)
			w.h.api.setFleet([]map[string]any{sailingShipRow(recoveryShipBlastoise, "Blastoise", recoveryHomePort, recoveryFarPort)})
			w.h.api.setFactorPorts()
			runner := w.h.runner(primaryTestKey)
			w.observe(t, runner, 100)
			const currentKey = "_near_limit_current"
			target := store.MaxSnapshotBytes - snapshotSafetyMargin - 96*1024
			padding := target - len(encode(runner.snapshot)) - len(currentKey) - 6
			if whitespace {
				runner.snapshot[currentKey] = json.RawMessage(strings.Repeat(" ", padding) + `"x"`)
			} else {
				runner.snapshot[currentKey] = encode(strings.Repeat("x", padding))
			}
			routes := decode[map[string]json.RawMessage](runner.snapshot, "routes")
			var ordered []string
			original := make(map[string]DatasetMeta)
			for _, source := range []string{"amsterdam", "calais", "ostend", "rouen"} {
				for _, pair := range [][2]string{{source, recoveryFarPort}, {recoveryFarPort, source}} {
					key := routeKey(pair[0], pair[1], recoveryHull)
					route := sourcingRoute(pair[0], pair[1], 24)
					route.Legs[0].EdgeClass = "oceanic"
					routes[key] = encode(struct {
						Route
						Padding string `json:"padding"`
					}{route, strings.Repeat("x", 48*1024)})
					at := time.Now().Add(-time.Hour + time.Duration(len(ordered))*time.Minute)
					meta := DatasetMeta{UpdatedAt: at, AttemptedAt: at, Complete: true, Generation: "near-limit-history"}
					runner.meta["routes/"+key], original[key] = meta, meta
					ordered = append(ordered, key)
				}
			}
			runner.snapshot["routes"] = encode(routes)
			runner.snapshot["_meta"] = encode(runner.meta)
			rawBytes := 0
			for key, value := range runner.snapshot {
				rawBytes += len(key) + len(value)
			}
			if max(len(encode(runner.snapshot)), rawBytes) <= store.MaxSnapshotBytes {
				t.Fatal("optional history must overflow the near-limit current snapshot")
			}
			w.observe(t, runner, 101)
			retained := decode[map[string]json.RawMessage](runner.snapshot, "routes")
			if len(retained) == 0 || len(retained) >= len(ordered) {
				t.Fatalf("headroom eviction should retain only the newest fitting history: %d of %d", len(retained), len(ordered))
			}
			if _, exists := retained[ordered[0]]; exists {
				t.Fatal("headroom eviction kept the oldest optional route")
			}
			if _, exists := retained[ordered[len(ordered)-1]]; !exists {
				t.Fatal("headroom eviction discarded the newest fitting route")
			}
			for key := range retained {
				if runner.meta["routes/"+key] != original[key] {
					t.Fatalf("headroom eviction restamped retained metadata for %s", key)
				}
			}
			rawBytes = 0
			for key, value := range runner.snapshot {
				rawBytes += len(key) + len(value)
			}
			if got := max(len(encode(runner.snapshot)), rawBytes); got > store.MaxSnapshotBytes-snapshotSafetyMargin {
				t.Fatalf("optional history ignored actual snapshot headroom: %d", got)
			}
			persisted, err := w.h.db.Snapshot(w.h.ctx, w.h.account.ID)
			if err != nil || len(encode(persisted)) > store.MaxSnapshotBytes {
				t.Fatalf("near-limit refresh did not persist within the store budget: %v", err)
			}
		})
	}
}

func TestRouteRetentionReviewRejectedLegalExplorationSharesHistoryCap(t *testing.T) {
	w := newIdleFallbackWorld(t)
	var mu sync.Mutex
	gets := 0
	base := w.h.api.server.Config.Handler
	w.h.api.server.Config.Handler = http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		if request.URL.Path == "/v1/world/route" && request.URL.Query().Get("from") != recoveryHomePort && request.URL.Query().Get("to") != recoveryHomePort {
			mu.Lock()
			gets++
			mu.Unlock()
			route := w.h.api.routeDocument(request.URL.Query().Get("from"), request.URL.Query().Get("to"))
			route["ship_class"] = request.URL.Query().Get("ship_class")
			route["safe_provisions_tons"] = 10_000
			route["padding"] = strings.Repeat("x", 512*1024)
			w.h.api.writeJSON(writer, http.StatusOK, route)
			return
		}
		base.ServeHTTP(writer, request)
	})
	runner := w.h.runner(primaryTestKey)
	w.observe(t, runner, 100)
	mu.Lock()
	captured := gets
	mu.Unlock()
	if captured != 6 {
		t.Fatalf("fixture did not inspect three rejected legal pairs: %d GETs", captured)
	}
	if accepted := acceptedRelocations(planSourcingRelocation(w.input(t, runner, recoveryShipBlastoise))); len(accepted) != 0 {
		t.Fatalf("oversized continuation stores must reject the explored pairs: %+v", accepted)
	}
	bytes, optional := 0, 0
	for key, raw := range decode[map[string]json.RawMessage](runner.snapshot, "routes") {
		route := runner.stateCache.Routes[key]
		if route.FromPort == recoveryHomePort || route.ToPort == recoveryHomePort {
			continue
		}
		optional++
		bytes += len(encode(raw)) + len(encode(runner.meta["routes/"+key])) +
			len(encode(key)) + len(encode("routes/"+key)) + 4
	}
	if optional == 0 || bytes > sourcingRouteRetentionBytes {
		t.Fatalf("seen-but-rejected legal pairs bypassed optional history accounting: entries=%d bytes=%d", optional, bytes)
	}
}

func TestRouteRetentionReviewCommittedPairsPrecedeExplorationBudget(t *testing.T) {
	w := newIdleFallbackWorld(t)
	runner := w.h.runner(primaryTestKey)
	w.observe(t, runner, 100)
	candidates := sourcingCandidates(w.input(t, runner, recoveryShipBlastoise))
	if len(candidates) != 4 {
		t.Fatalf("fixture needs four distinct source candidates, got %d", len(candidates))
	}
	plan := candidates[3]
	plan.ShipID = recoveryShipLapras
	committed := missionData{
		Plan: plan, CostsKnown: true,
		Relocation: &relocationIntent{
			TargetContractID: plan.ContractID, TargetCommodity: plan.Commodity,
			TargetDestination: recoveryFarPort, ExpenseCapPence: 20_000,
		},
	}
	w.h.putMission(recoveryShipLapras, "committed-before-exploration", "sourcing_depart", committed)
	w.h.api.setFleet([]map[string]any{
		sourcingReviewShipRow(),
		destinationShipRow(recoveryShipLapras, "Lapras", recoveryHomePort, "berthed", 5, nil, nil),
	})
	for _, candidate := range candidates {
		for _, pair := range [][2]string{{candidate.Destination, recoveryFarPort}, {recoveryFarPort, candidate.Destination}} {
			key := "routes/" + routeKey(pair[0], pair[1], recoveryHull)
			meta := runner.meta[key]
			meta.UpdatedAt = time.Now().Add(-routeGeometryTTL - time.Minute)
			runner.meta[key] = meta
		}
	}
	var mu sync.Mutex
	gets := 0
	base := w.h.api.server.Config.Handler
	w.h.api.server.Config.Handler = http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		if request.URL.Path == "/v1/world/route" && request.URL.Query().Get("from") != recoveryHomePort && request.URL.Query().Get("to") != recoveryHomePort {
			mu.Lock()
			gets++
			mu.Unlock()
		}
		base.ServeHTTP(writer, request)
	})
	w.observe(t, runner, 101)
	mu.Lock()
	captured := gets
	mu.Unlock()
	if captured != 8 {
		t.Fatalf("committed two-leg capture did not precede six exploratory GETs: %d", captured)
	}
	for _, pair := range [][2]string{
		{plan.Origin, plan.Destination}, {plan.Destination, plan.Origin},
		{plan.Destination, recoveryFarPort}, {recoveryFarPort, plan.Destination},
	} {
		key := routeKey(pair[0], pair[1], recoveryHull)
		if _, exists := runner.stateCache.Routes[key]; !exists || !runner.fresh("routes/"+key) {
			t.Fatalf("exploration pruned a committed relocation leg: %s", key)
		}
	}
	if checked := costRelocation(w.input(t, runner, recoveryShipLapras), plan); !checked.Accepted {
		t.Fatalf("committed relocation lost its usable public continuation: %+v", checked)
	}
}

func TestRouteRetentionReviewIndexedGossipUsesLatestAskAndBestCharter(t *testing.T) {
	for _, higherReward := range []bool{false, true} {
		t.Run(fmt.Sprintf("higher-charter-%t", higherReward), func(t *testing.T) {
			w := newIdleFallbackWorld(t)
			w.h.api.setFleet([]map[string]any{sailingShipRow(recoveryShipBlastoise, "Blastoise", recoveryHomePort, recoveryFarPort)})
			w.h.api.setFactorPorts()
			base := w.h.api.server.Config.Handler
			w.h.api.server.Config.Handler = http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
				switch request.URL.Path {
				case "/v1/contract/list":
					if request.URL.Query().Get("status") != "open" {
						w.h.api.writeJSON(writer, http.StatusOK, []Contract{})
						return
					}
					contracts := make([]Contract, 500)
					for i := range contracts {
						contracts[i] = sourcingReviewCharter(fmt.Sprintf("indexed-charter-%d", i))
					}
					if higherReward {
						reward := int64(4_000)
						contracts[len(contracts)-1].RewardPerTonPence = &reward
					}
					start := 0
					if cursor := request.URL.Query().Get("cursor"); cursor != "" {
						found := false
						for i, contract := range contracts {
							if contract.ID == cursor {
								start, found = i+1, true
								break
							}
						}
						if !found {
							w.h.api.writeProblem(writer, http.StatusBadRequest, "invalid_cursor", "unknown fixture contract cursor")
							return
						}
					}
					w.h.api.writeJSON(writer, http.StatusOK, contracts[start:min(start+200, len(contracts))])
					return
				case "/v1/world/port":
					if request.URL.Query().Get("port_id") == recoveryHomePort {
						detail := w.h.api.portDocument(recoveryHomePort)
						old, latest := time.Now().Add(-time.Minute), time.Now().Add(-30*time.Second)
						cheap, dear := int64(1_000), int64(3_500)
						detail["coffee_house_gossip"] = []Gossip{
							{SubjectPortID: "amsterdam", SnapshotAt: old, Commodities: []GossipCommodity{{CommodityID: "sawn_planks", SnapshotAt: old, BestAskPence: &cheap}}},
							{SubjectPortID: "amsterdam", SnapshotAt: latest, Commodities: []GossipCommodity{{CommodityID: "sawn_planks", SnapshotAt: latest, BestAskPence: &dear}}},
						}
						w.h.api.writeJSON(writer, http.StatusOK, detail)
						return
					}
				}
				base.ServeHTTP(writer, request)
			})
			runner := w.h.runner(primaryTestKey)
			w.observe(t, runner, 100)
			if len(runner.stateCache.Contracts) != 500 || !runner.fresh("contracts") {
				t.Fatalf("fixture did not publish 500 current paginated charters: %d, %+v", len(runner.stateCache.Contracts), runner.meta["contracts"])
			}
			key := routeKey("amsterdam", recoveryFarPort, recoveryHull)
			route := sourcingRoute("amsterdam", recoveryFarPort, 24)
			route.Legs[0].EdgeClass = "oceanic"
			runner.snapshot["routes"] = encode(map[string]Route{key: route})
			at := time.Now().Add(-time.Hour)
			runner.meta["routes/"+key] = DatasetMeta{UpdatedAt: at, AttemptedAt: at, Complete: true}
			w.observe(t, runner, 101)
			_, retained := runner.stateCache.Routes[key]
			if retained != higherReward {
				t.Fatalf("indexed supply ignored latest ask or highest available reward: retained=%t want=%t", retained, higherReward)
			}
		})
	}
}

func TestRouteRetentionReviewAccountsForRedactionExpansion(t *testing.T) {
	w := newIdleFallbackWorld(t)
	w.h.api.setFleet([]map[string]any{sailingShipRow(recoveryShipBlastoise, "Blastoise", recoveryHomePort, recoveryFarPort)})
	w.h.api.setFactorPorts()
	runner := w.h.runner(primaryTestKey)
	w.observe(t, runner, 100)
	const currentKey = "_near_limit_current"
	target := store.MaxSnapshotBytes - snapshotSafetyMargin - 700*1024
	padding := target - len(encode(runner.snapshot)) - len(currentKey) - 6
	runner.snapshot[currentKey] = encode(strings.Repeat("x", padding))
	extensions := make([]struct {
		Token int `json:"token"`
	}, 5_000)
	routes := decode[map[string]json.RawMessage](runner.snapshot, "routes")
	var ordered []string
	original := make(map[string]DatasetMeta)
	for _, source := range []string{"amsterdam", "calais", "ostend", "rouen"} {
		for _, pair := range [][2]string{{source, recoveryFarPort}, {recoveryFarPort, source}} {
			key := routeKey(pair[0], pair[1], recoveryHull)
			route := sourcingRoute(pair[0], pair[1], 24)
			route.Legs[0].EdgeClass = "oceanic"
			routes[key] = encode(struct {
				Route
				Extensions any `json:"extensions"`
			}{route, extensions})
			at := time.Now().Add(-time.Hour + time.Duration(len(ordered))*time.Minute)
			meta := DatasetMeta{UpdatedAt: at, AttemptedAt: at, Complete: true, Generation: "redaction-history"}
			runner.meta["routes/"+key], original[key] = meta, meta
			ordered = append(ordered, key)
		}
	}
	runner.snapshot["routes"] = encode(routes)
	runner.snapshot["_meta"] = encode(runner.meta)
	if raw := len(encode(runner.snapshot)); raw > store.MaxSnapshotBytes-snapshotSafetyMargin {
		t.Fatalf("fixture must fit the old unredacted headroom check: %d", raw)
	}
	before, err := w.h.db.RedactedSnapshotSize(w.h.ctx, w.h.account.ID, runner.snapshot)
	if err != nil || before <= store.MaxSnapshotBytes {
		t.Fatalf("short sensitive values must expand beyond the store limit: size=%d error=%v", before, err)
	}
	w.observe(t, runner, 101)
	retained := decode[map[string]json.RawMessage](runner.snapshot, "routes")
	if len(retained) == 0 || len(retained) >= len(ordered) {
		t.Fatalf("redaction-aware eviction should retain only fitting history: %d of %d", len(retained), len(ordered))
	}
	if _, exists := retained[ordered[0]]; exists {
		t.Fatal("redaction-aware eviction kept the oldest optional route")
	}
	if _, exists := retained[ordered[len(ordered)-1]]; !exists {
		t.Fatal("redaction-aware eviction discarded the newest fitting route")
	}
	for key := range retained {
		if runner.meta["routes/"+key] != original[key] {
			t.Fatalf("redaction-aware eviction restamped metadata for %s", key)
		}
	}
	after, err := w.h.db.RedactedSnapshotSize(w.h.ctx, w.h.account.ID, runner.snapshot)
	if err != nil || after > store.MaxSnapshotBytes-snapshotSafetyMargin {
		t.Fatalf("eviction did not converge against the persisted representation: size=%d error=%v", after, err)
	}
	persisted, err := w.h.db.Snapshot(w.h.ctx, w.h.account.ID)
	if err != nil || len(encode(persisted)) != after {
		t.Fatalf("size helper disagrees with the persisted snapshot: measured=%d persisted=%d error=%v", after, len(encode(persisted)), err)
	}
	var persistedRoutes map[string]struct {
		Extensions []struct {
			Token string `json:"token"`
		} `json:"extensions"`
	}
	if err := json.Unmarshal(persisted["routes"], &persistedRoutes); err != nil {
		t.Fatal(err)
	}
	for key, route := range persistedRoutes {
		if len(route.Extensions) != len(extensions) {
			t.Fatalf("route extension count changed during persistence for %s", key)
		}
		for _, extension := range route.Extensions {
			if extension.Token != "[REDACTED]" {
				t.Fatal("persisted route extension retained an unredacted sensitive value")
			}
		}
	}
}
