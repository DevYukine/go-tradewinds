package engine

import (
	"encoding/json"
	"net/http"
	"sync/atomic"
	"testing"
	"time"

	"github.com/google/uuid"
)

func TestRouteCacheRefreshFetchesOnlyMissingExpiredOrFailedPairs(t *testing.T) {
	t.Parallel()
	h := newRecoveryHarness(t)
	stationary := sailingShipRow(recoveryShipLapras, "Lapras", recoveryHomePort, recoveryFarPort)
	stationary["ship"].(map[string]any)["status"] = "berthed"
	stationary["voyage"] = nil
	stationary["route_legs"] = []any{}
	h.api.fleet = append(h.api.fleet, stationary)

	var routeGETs, bookGETs atomic.Int64
	var phase atomic.Int64
	phase.Store(1)
	var failOutward atomic.Bool
	base := h.api.server.Config.Handler
	h.api.server.Config.Handler = http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		switch request.URL.Path {
		case "/v1/world/clock":
			clock := h.api.clockDocument()
			clock["unlocked_phase"] = phase.Load()
			h.api.writeJSON(writer, http.StatusOK, clock)
			return
		case "/v1/world/route":
			routeGETs.Add(1)
			if failOutward.Load() && request.URL.Query().Get("from") == recoveryHomePort {
				h.api.writeProblem(writer, http.StatusNotFound, "route_unavailable", "route geometry is temporarily unavailable")
				return
			}
		case "/v1/market/book":
			bookGETs.Add(1)
		}
		base.ServeHTTP(writer, request)
	})

	runner := h.runner(primaryTestKey)
	refresh := func(wantRouteGETs int64) {
		t.Helper()
		beforeRoutes, beforeBooks := routeGETs.Load(), bookGETs.Load()
		runner.generation = uuid.NewString()
		if err := runner.refresh(h.ctx); err != nil {
			t.Fatalf("refresh route geometry: %v", err)
		}
		if got := routeGETs.Load() - beforeRoutes; got != wantRouteGETs {
			t.Fatalf("route GETs this refresh = %d, want %d", got, wantRouteGETs)
		}
		if got := bookGETs.Load() - beforeBooks; got != 1 {
			t.Fatalf("private book GETs this refresh = %d, want 1", got)
		}
		for _, dataset := range []string{"books/" + recoveryHomePort, "vaults", "ships", "contracts", "yard_quotes/" + recoveryHomePort + "|" + recoveryHull} {
			if runner.meta[dataset].Generation != runner.generation || !runner.fresh(dataset) {
				t.Fatalf("route reuse weakened executable freshness for %s: %+v", dataset, runner.meta[dataset])
			}
		}
	}

	// One present origin, one other port and one hull require two directed
	// routes on the cold refresh and none on an unchanged warm refresh.
	refresh(2)
	key := routeKey(recoveryHomePort, recoveryFarPort, recoveryHull)
	captured := runner.meta["routes/"+key]
	refresh(0)
	if runner.meta["routes/"+key] != captured || !runner.fresh("routes/"+key) || !runner.fresh("routes") {
		t.Fatalf("cached route lost its original observation or became unusable: %+v", runner.meta["routes/"+key])
	}
	t.Log("route GETs per unchanged two-port refresh: cold 2, warm 0; private book GETs remain 1")

	// The retained snapshot is also a cache across process restarts.
	runner = h.runner(primaryTestKey)
	refresh(0)

	values := decode[map[string]json.RawMessage](runner.snapshot, "routes")
	delete(values, key)
	runner.snapshot["routes"] = encode(values)
	refresh(1)

	expired := runner.meta["routes/"+key]
	expired.UpdatedAt = time.Now().Add(-routeGeometryTTL - time.Minute)
	runner.meta["routes/"+key] = expired
	refresh(1)

	stale := runner.meta["routes/"+key]
	stale.Stale = true
	runner.meta["routes/"+key] = stale
	failOutward.Store(true)
	refresh(1)
	if runner.fresh("routes/"+key) || !runner.meta["routes/"+key].Stale || runner.meta["routes/"+key].Error == "" {
		t.Fatal("a failed route capture was reused as successful geometry")
	}
	failOutward.Store(false)
	refresh(1)

	// max_phase defaults to the world's unlock, so a newly available phase
	// must invalidate both previously successful directions.
	phase.Store(2)
	refresh(2)
}

func TestRouteCacheFreshnessDoesNotRestampPrivateEvidence(t *testing.T) {
	t.Parallel()
	in := sourcingInputFixture(t)
	key := "routes/" + routeKey("le_havre", "amsterdam", "hoy")
	old := DatasetMeta{UpdatedAt: in.Now.Add(-time.Hour), Generation: "previous-capture", Complete: true}
	in.Metadata[key] = old
	if !in.fresh(key) {
		t.Fatal("successful static geometry from an earlier generation was treated as a live quote")
	}
	for _, dataset := range []string{"books/le_havre", "vaults", "ships", "contracts", "yard_quotes/le_havre|hoy", "zones"} {
		in.Metadata[dataset] = old
		if in.fresh(dataset) {
			t.Fatalf("static geometry freshness leaked into executable dataset %s", dataset)
		}
	}
	for _, invalid := range []DatasetMeta{
		{UpdatedAt: in.Now.Add(-routeGeometryTTL - time.Second), Complete: true},
		{UpdatedAt: in.Now.Add(time.Minute), Complete: true},
		{UpdatedAt: in.Now, Complete: true, Stale: true},
		{UpdatedAt: in.Now, Complete: true, Error: "route unavailable"},
		{UpdatedAt: in.Now},
	} {
		in.Metadata[key] = invalid
		if in.fresh(key) {
			t.Fatalf("unusable route geometry was accepted from cache: %+v", invalid)
		}
	}
}
