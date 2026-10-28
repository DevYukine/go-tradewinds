package engine

import (
	"encoding/json"
	"net/http"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"unicode/utf8"

	"github.com/google/uuid"
	"go.uber.org/zap"
	"go.uber.org/zap/zaptest/observer"
)

func TestRouteCacheDroppedOnGameVersionChange(t *testing.T) {
	t.Parallel()
	h := newRecoveryHarness(t)
	for _, ship := range h.api.fleet {
		ship["ship"].(map[string]any)["status"] = "berthed"
		ship["voyage"] = nil
		ship["route_legs"] = []any{}
	}
	var safeProvisions atomic.Int64
	safeProvisions.Store(1)
	var mu sync.Mutex
	fetches := make(map[string]int)
	downstream := h.api.server.Config.Handler
	h.api.server.Config.Handler = http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		if request.Method == http.MethodGet && request.URL.Path == "/v1/world/route" {
			query := request.URL.Query()
			key := routeKey(query.Get("from"), query.Get("to"), query.Get("ship_class"))
			mu.Lock()
			fetches[key]++
			mu.Unlock()
			route := h.api.routeDocument(query.Get("from"), query.Get("to"))
			route["ship_class"] = query.Get("ship_class")
			route["safe_provisions_tons"] = safeProvisions.Load()
			h.api.writeJSON(writer, http.StatusOK, route)
			return
		}
		downstream.ServeHTTP(writer, request)
	})
	runner := h.runner(primaryTestKey)
	version := ""
	runner.opts.GameVersion = func() string { return version }
	refresh := func(wantFetches int) {
		t.Helper()
		mu.Lock()
		clear(fetches)
		mu.Unlock()
		runner.generation = uuid.NewString()
		if err := runner.refresh(h.ctx); err != nil {
			t.Fatalf("refresh versioned routes: %v", err)
		}
		mu.Lock()
		defer mu.Unlock()
		if len(fetches) != wantFetches {
			t.Fatalf("version %q route fetches = %v; want %d distinct pairs", version, fetches, wantFetches)
		}
		for key, count := range fetches {
			if count != 1 {
				t.Fatalf("version %q fetched %s %d times instead of once", version, key, count)
			}
		}
	}
	key := routeKey(recoveryHomePort, recoveryFarPort, recoveryHull)
	refresh(2)
	refresh(0)
	if _, exists := runner.snapshot["_game_version"]; exists {
		t.Fatal("unknown watcher version was persisted as a known version")
	}
	version = "0.4.0"
	refresh(2)
	safeProvisions.Store(8)
	refresh(0)
	if runner.stateCache.Routes[key].SafeProvisionsTons != 1 {
		t.Fatal("unchanged version unexpectedly replaced cached geometry")
	}
	// Version invalidation also removes child-shaped leftovers and their
	// authority, not merely the aggregate routes map.
	runner.snapshot["routes/obsolete-pair"] = encode(Route{SafeProvisionsTons: 99})
	runner.meta["routes/obsolete-pair"] = runner.meta["routes/"+key]
	version = "0.5.0"
	refresh(2)
	if got := decode[string](runner.snapshot, "_game_version"); got != version {
		t.Fatalf("persisted game version = %q, want %q", got, version)
	}
	if runner.stateCache.Routes[key].SafeProvisionsTons != 8 {
		t.Fatalf("new version reused old safe provisions: %+v", runner.stateCache.Routes[key])
	}
	if _, exists := runner.snapshot["routes/obsolete-pair"]; exists {
		t.Fatal("version change retained an obsolete route child")
	}
	if _, exists := runner.meta["routes/obsolete-pair"]; exists {
		t.Fatal("version change retained obsolete route authority")
	}
	ship, _ := runner.ship(recoveryShipBlastoise)
	plans := PlanVoyages(runner.planningInput(ship))
	accepted := false
	for _, plan := range plans {
		if plan.Accepted {
			accepted = true
			if plan.TargetStoresTons < 9 {
				t.Fatalf("post-release plan used pre-release stores geometry: %+v", plan)
			}
		}
	}
	if !accepted {
		t.Fatalf("fixture has no accepted post-release plan: %+v", plans)
	}
	refresh(0)
	version = ""
	safeProvisions.Store(12)
	refresh(0)
	if decode[string](runner.snapshot, "_game_version") != "0.5.0" || runner.stateCache.Routes[key].SafeProvisionsTons != 8 {
		t.Fatal("unknown watcher version discarded the last-known version or routes")
	}
	version = "0.5.0"
	runner = h.runner(primaryTestKey)
	runner.opts.GameVersion = func() string { return version }
	refresh(0)
}

func TestRouteCacheVersionCallbackIsNotReplayPolicy(t *testing.T) {
	t.Parallel()
	if _, err := json.Marshal(Options{GameVersion: func() string { return "0.5.0" }}); err != nil {
		t.Fatalf("runtime version callback broke replay policy serialization: %v", err)
	}
}

func TestRouteVersionLogsRedactBeforeUnicodeLengthBound(t *testing.T) {
	t.Parallel()
	h := newRecoveryHarness(t)
	runner := h.runner(primaryTestKey)
	core, logs := observer.New(zap.InfoLevel)
	runner.log = zap.New(core)
	previous := "0.4.0 tw_live_previous_version_secret"
	current := strings.Repeat("界", 60) + "tw_live_current_version_secret" + strings.Repeat("尾", 100)
	runner.snapshot["_game_version"] = encode(previous)
	runner.opts.GameVersion = func() string { return current }
	runner.generation = uuid.NewString()
	if err := runner.refresh(h.ctx); err != nil {
		t.Fatal(err)
	}
	entries := logs.FilterMessage("game version changed; refetching cached routes").All()
	if len(entries) != 1 {
		t.Fatalf("version change did not log exactly once: %+v", entries)
	}
	fields := entries[0].ContextMap()
	if fields["previous_version"] != "0.4.0 [REDACTED]" ||
		fields["game_version"] != strings.Repeat("界", 60)+"[RED" {
		t.Fatalf("version text was not fully redacted before its 64-character bound: %+v", fields)
	}
	for _, key := range []string{"previous_version", "game_version"} {
		value := fields[key].(string)
		if !utf8.ValidString(value) || utf8.RuneCountInString(value) > 64 {
			t.Fatalf("version field has an invalid Unicode or length bound: %s=%q", key, value)
		}
	}
	if decode[string](runner.snapshot, "_game_version") != current {
		t.Fatal("console bounding corrupted the full version used for route invalidation")
	}
}
