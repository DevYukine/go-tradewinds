package engine

import (
	"context"
	"database/sql"
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
			if len(retained) == 0 || len(retained) > SourcingRouteRetentionLimit {
				t.Fatalf("retained route count is not bounded: %d", len(retained))
			}
			if padding == 0 && len(retained) != SourcingRouteRetentionLimit {
				t.Fatalf("count cap retained %d routes, want %d", len(retained), SourcingRouteRetentionLimit)
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
			if bytes > SourcingRouteRetentionBytes || len(encode(runner.snapshot)) >= storeSnapshotBudgetBytes {
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
			runner.meta["routes/"+key] = DatasetMeta{UpdatedAt: time.Now().Add(-RouteGeometryTTL - time.Minute), Complete: true}
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

func TestRouteRetentionReviewRejectsUnsafeRequiredRouteNumbers(t *testing.T) {
	for _, test := range []struct {
		name  string
		token string
	}{
		{"huge exponent", "1e130000"},
		{"zero extreme scale", "0e-16000"},
		{"65 digit integer", strings.Repeat("8", 65)},
	} {
		t.Run(test.name, func(t *testing.T) {
			w := newIdleFallbackWorld(t)
			runner := w.h.runner(primaryTestKey)
			w.observe(t, runner, 100)
			key := routeKey(recoveryHomePort, recoveryFarPort, recoveryHull)
			metaKey := "routes/" + key
			previous := decode[map[string]json.RawMessage](runner.snapshot, "routes")[key]
			if len(previous) == 0 {
				t.Fatal("fixture did not observe the required route")
			}
			captured := runner.meta[metaKey]
			retry := captured
			retry.Stale = true
			runner.meta[metaKey] = retry
			base := w.h.api.server.Config.Handler
			w.h.api.server.Config.Handler = http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
				query := request.URL.Query()
				if request.URL.Path == "/v1/world/route" && query.Get("from") == recoveryHomePort &&
					query.Get("to") == recoveryFarPort && query.Get("ship_class") == recoveryHull {
					route := w.h.api.routeDocument(recoveryHomePort, recoveryFarPort)
					route["total_distance_nm"] = 999
					route["future_extension"] = json.RawMessage(`{"n":` + test.token + `}`)
					w.h.api.writeJSON(writer, http.StatusOK, route)
					return
				}
				base.ServeHTTP(writer, request)
			})
			w.observe(t, runner, 101)
			meta := runner.meta[metaKey]
			if !meta.Stale || meta.Complete || !strings.Contains(meta.Error, "storage bounds") {
				t.Fatalf("unsafe route capture lost visible failure metadata: %+v", meta)
			}
			if !meta.UpdatedAt.Equal(captured.UpdatedAt) || meta.Generation != captured.Generation ||
				!meta.AttemptedAt.After(captured.AttemptedAt) {
				t.Fatalf("failed capture restamped the last-good route observation: %+v", meta)
			}
			if current := decode[map[string]json.RawMessage](runner.snapshot, "routes")[key]; string(current) != string(previous) {
				t.Fatal("unsafe numeric extension replaced the last-good required route")
			}
			if !runner.fresh("ships") {
				t.Fatal("a failed route capture stopped unrelated dataset refreshes")
			}
			admin, err := sql.Open("pgx", w.h.dsn)
			if err != nil {
				t.Fatal(err)
			}
			defer admin.Close()
			var same bool
			if err := admin.QueryRowContext(w.h.ctx, `SELECT data = $3::jsonb FROM snapshot_routes
				WHERE account_id = $1 AND route_key = $2`, w.h.account.ID, key, string(previous)).Scan(&same); err != nil {
				t.Fatal(err)
			}
			if !same {
				t.Fatal("failed numeric capture changed the stored last-good route")
			}
			persisted, err := w.h.db.Snapshot(w.h.ctx, w.h.account.ID)
			if err != nil {
				t.Fatalf("failed route capture prevented snapshot persistence: %v", err)
			}
			storedMeta := decode[map[string]DatasetMeta](persisted, "_meta")[metaKey]
			if !storedMeta.Stale || storedMeta.Complete || storedMeta.Error != meta.Error ||
				!storedMeta.UpdatedAt.Equal(captured.UpdatedAt) || storedMeta.Generation != captured.Generation {
				t.Fatalf("route payload and failure metadata were not persisted together: %+v", storedMeta)
			}
			w.h.api.server.Config.Handler = base
			w.observe(t, runner, 102)
			if !runner.fresh(metaKey) || runner.meta[metaKey].Error != "" {
				t.Fatalf("a healthy route capture did not recover: %+v", runner.meta[metaKey])
			}
		})
	}
}

func TestRouteRetentionReviewPreservesHistoryOutsideNearLimitSnapshot(t *testing.T) {
	w := newIdleFallbackWorld(t)
	w.h.api.setFleet([]map[string]any{sailingShipRow(recoveryShipBlastoise, "Blastoise", recoveryHomePort, recoveryFarPort)})
	w.h.api.setFactorPorts()
	runner := w.h.runner(primaryTestKey)
	w.observe(t, runner, 100)
	const currentKey = "_near_limit_current"
	target := store.MaxSnapshotBytes - 96*1024
	padding := target - len(encode(runner.snapshot)) - len(currentKey) - 6
	runner.snapshot[currentKey] = encode(strings.Repeat("x", padding))
	routes := decode[map[string]json.RawMessage](runner.snapshot, "routes")
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
			at := time.Now().UTC().Add(-time.Hour + time.Duration(len(original))*time.Minute)
			meta := DatasetMeta{UpdatedAt: at, AttemptedAt: at, Complete: true, Generation: "near-limit-history"}
			runner.meta["routes/"+key], original[key] = meta, meta
		}
	}
	runner.snapshot["routes"] = encode(routes)
	runner.snapshot["_meta"] = encode(runner.meta)
	if len(encode(runner.snapshot)) <= store.MaxSnapshotBytes {
		t.Fatal("route history must exceed the near-limit snapshot row budget")
	}
	w.observe(t, runner, 101)
	retained := decode[map[string]json.RawMessage](runner.snapshot, "routes")
	if len(retained) != len(original) {
		t.Fatalf("snapshot row pressure evicted route history: got %d want %d", len(retained), len(original))
	}
	retainedBytes := 0
	for key, raw := range retained {
		meta := runner.meta["routes/"+key]
		if meta != original[key] {
			t.Fatalf("retention restamped metadata for %s", key)
		}
		retainedBytes += len(encode(raw)) + len(encode(meta)) +
			len(encode(key)) + len(encode("routes/"+key)) + 4
	}
	if retainedBytes > SourcingRouteRetentionBytes || len(retained) > SourcingRouteRetentionLimit {
		t.Fatalf("retained history exceeded its independent limits: count=%d bytes=%d", len(retained), retainedBytes)
	}
	admin, err := sql.Open("pgx", w.h.dsn)
	if err != nil {
		t.Fatal(err)
	}
	defer admin.Close()
	var stored []byte
	var routeCount int
	if err := admin.QueryRowContext(w.h.ctx, `SELECT data,
		(SELECT count(*) FROM snapshot_routes WHERE account_id = $1)
		FROM snapshots WHERE account_id = $1`, w.h.account.ID).Scan(&stored, &routeCount); err != nil {
		t.Fatal(err)
	}
	var storedSnapshot map[string]json.RawMessage
	if err := json.Unmarshal(stored, &storedSnapshot); err != nil {
		t.Fatal(err)
	}
	if _, exists := storedSnapshot["routes"]; exists {
		t.Fatal("route payloads still consume snapshot row storage")
	}
	if size := len(encode(storedSnapshot)); size > store.MaxSnapshotBytes || size < store.MaxSnapshotBytes-128*1024 {
		t.Fatalf("fixture did not persist a near-limit snapshot row: %d", size)
	}
	if routeCount != len(retained) {
		t.Fatalf("route table lost retained history: got %d want %d", routeCount, len(retained))
	}
	persistedMeta := decode[map[string]DatasetMeta](storedSnapshot, "_meta")
	restored := w.h.runner(primaryTestKey)
	restoredState := restored.state()
	if len(restoredState.Routes) != len(retained) {
		t.Fatalf("runner restore did not merge route history: got %d want %d", len(restoredState.Routes), len(retained))
	}
	for key, meta := range original {
		if _, exists := restoredState.Routes[key]; !exists {
			t.Fatalf("runner restore lost route %s", key)
		}
		if persistedMeta["routes/"+key] != meta || restored.meta["routes/"+key] != meta {
			t.Fatalf("snapshot row and restored route metadata changed for %s", key)
		}
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
	if optional == 0 || bytes > SourcingRouteRetentionBytes {
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
			meta.UpdatedAt = time.Now().Add(-RouteGeometryTTL - time.Minute)
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
