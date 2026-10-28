package engine

// Acceptance tests for plan R1: the REST capture wave must overlap its
// independent dataset GETs behind a bounded worker pool while keeping the
// serial capture semantics the engine already guarantees — per-dataset failure
// isolation with last-good retention, owner cancellation that joins every
// worker and never publishes a canceled generation, and a server-clock
// envelope stamped inside the fetch task rather than at the wave join.
//
// The tests drive the existing capture seams against a disposable
// PostgreSQL store (recoveryHarness) and a dedicated concurrency-safe fake
// game API. Unlike fakeGameAPI, whose whole-request mutex serializes every
// handler invocation, this fake never holds a lock while a request is in
// flight, so real overlap is observable. Gating uses channels and atomics
// with bounded deadlines; no assertion depends on wall-clock speed.

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/DevYukine/go-tradewinds/internal/gameapi"
	"go.uber.org/zap"
)

// Selection filters distinguish datasets; pagination controls do not. Gates
// use only the path. Contract status reads remain one ordered serial task.
var snapshotBaseDatasetPaths = []string{
	"/v1/auth/whoami",
	"/v1/player/profile",
	"/v1/company/profile",
	"/v1/company/settings",
	"/v1/world/catalog",
	"/v1/world/clock",
	"/v1/world/zones",
	"/v1/world/expansion",
	"/v1/world/leaderboard",
	"/v1/world/events?include_expired=true",
	"/v1/treasury/vaults",
	"/v1/treasury/rates",
	"/v1/fleet/ships",
	"/v1/warehouse/list",
	"/v1/market/orders",
	"/v1/market/fills",
	"/v1/company/factors",
	"/v1/insurance/list",
	"/v1/insurance/list?underwriter_company_id=" + recoveryCompanyID,
	"/v1/future/list",
	"/v1/fleet/convoys",
	"/v1/fleet/marques",
	"/v1/fleet/patrols",
	"/v1/treasury/ledger",
	"/v1/rnd/status",
	"/v1/rnd/register",
}

// snapshotConcurrencyAPI serves the full base dataset surface plus the child
// endpoints a one-ship fleet derives. It counts arrivals, records per-path
// serve timestamps, tracks peak in-flight GETs, and can hold configured paths
// behind gates that the test releases by closing a channel. Map state is
// guarded by a mutex that is never held while a request blocks.
type snapshotConcurrencyAPI struct {
	t      *testing.T
	server *httptest.Server

	mu         sync.Mutex
	fleet      []map[string]any
	arrivals   map[string]int
	serveTimes map[string]time.Time
	failStatus map[string]int
	gates      map[string]chan struct{}

	inFlight atomic.Int64
	peak     atomic.Int64
}

func newSnapshotConcurrencyAPI(t *testing.T, fleet []map[string]any) *snapshotConcurrencyAPI {
	t.Helper()
	if fleet == nil {
		fleet = []map[string]any{}
	}
	api := &snapshotConcurrencyAPI{
		t:          t,
		fleet:      fleet,
		arrivals:   make(map[string]int),
		serveTimes: make(map[string]time.Time),
		failStatus: make(map[string]int),
		gates:      make(map[string]chan struct{}),
	}
	api.server = httptest.NewServer(http.HandlerFunc(api.handle))
	t.Cleanup(api.server.Close)
	return api
}

func (f *snapshotConcurrencyAPI) gatePaths(paths ...string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	for _, path := range paths {
		path, _, _ = strings.Cut(path, "?")
		if f.gates[path] == nil {
			f.gates[path] = make(chan struct{})
		}
	}
}

func (f *snapshotConcurrencyAPI) releaseGates(paths ...string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	for _, path := range paths {
		if gate, ok := f.gates[path]; ok {
			select {
			case <-gate:
			default:
				close(gate)
			}
		}
	}
}

func (f *snapshotConcurrencyAPI) releaseAllGates() {
	f.mu.Lock()
	paths := make([]string, 0, len(f.gates))
	for path := range f.gates {
		paths = append(paths, path)
	}
	f.mu.Unlock()
	f.releaseGates(paths...)
}

func (f *snapshotConcurrencyAPI) setFailureStatus(path string, status int) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.failStatus[path] = status
}

func (f *snapshotConcurrencyAPI) arrivedCount(path string) int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.arrivals[path]
}

func (f *snapshotConcurrencyAPI) serveTimeFor(path string) (time.Time, bool) {
	f.mu.Lock()
	defer f.mu.Unlock()
	served, ok := f.serveTimes[path]
	return served, ok
}

func (f *snapshotConcurrencyAPI) inFlightNow() int64 { return f.inFlight.Load() }

func (f *snapshotConcurrencyAPI) handle(writer http.ResponseWriter, request *http.Request) {
	if request.Method != http.MethodGet {
		f.writeProblem(writer, http.StatusMethodNotAllowed, "method_not_allowed", "the capture wave only issues GET requests")
		return
	}
	inFlight := f.inFlight.Add(1)
	for {
		observed := f.peak.Load()
		if inFlight <= observed || f.peak.CompareAndSwap(observed, inFlight) {
			break
		}
	}
	datasetKey := request.URL.Path
	query := request.URL.Query()
	query.Del("limit")
	query.Del("cursor")
	if encoded := query.Encode(); encoded != "" {
		datasetKey += "?" + encoded
	}
	f.mu.Lock()
	f.arrivals[datasetKey]++
	gate := f.gates[request.URL.Path]
	f.mu.Unlock()
	if gate != nil {
		select {
		case <-gate:
		case <-request.Context().Done():
			f.inFlight.Add(-1)
			return
		}
	}
	status, body := f.response(request)
	f.mu.Lock()
	f.serveTimes[request.URL.Path] = time.Now()
	f.mu.Unlock()
	writer.Header().Set("Content-Type", "application/json")
	writer.WriteHeader(status)
	_, _ = writer.Write(body)
	f.inFlight.Add(-1)
}

func (f *snapshotConcurrencyAPI) response(request *http.Request) (int, []byte) {
	f.mu.Lock()
	failure := f.failStatus[request.URL.Path]
	fleet := f.fleet
	f.mu.Unlock()
	if failure != 0 {
		return failure, f.mustJSON(map[string]any{
			"type":   "about:blank",
			"title":  http.StatusText(failure),
			"status": failure,
			"code":   "dataset_unavailable",
			"detail": "this dataset refused the refresh for the acceptance scenario",
		})
	}
	switch request.URL.Path {
	case "/v1/auth/whoami":
		return http.StatusOK, f.mustJSON(map[string]any{
			"api_key_id": "cccccccc-cccc-4ccc-8ccc-cccccccccccc",
			"company_id": recoveryCompanyID,
			"player_id":  recoveryPlayerID,
			"scopes":     []any{"company:trade"},
		})
	case "/v1/player/profile":
		return http.StatusOK, f.mustJSON(map[string]any{
			"player":    map[string]any{"id": recoveryPlayerID},
			"companies": []any{},
		})
	case "/v1/company/profile":
		return http.StatusOK, f.mustJSON(map[string]any{
			"company": map[string]any{
				"id":           recoveryCompanyID,
				"player_id":    recoveryPlayerID,
				"name":         "The Pokemon Pirates",
				"home_port_id": recoveryHomePort,
			},
			"handle":       "Yukine",
			"fleet_count":  len(fleet),
			"factor_ports": []any{},
			"vaults":       []any{},
		})
	case "/v1/company/settings":
		return http.StatusOK, f.mustJSON(map[string]any{
			"auto_remit_for_bills":         true,
			"auto_remit_min_reserve_pence": 200_000,
		})
	case "/v1/rnd/status":
		return http.StatusOK, f.mustJSON(rndStatusFixture())
	case "/v1/rnd/register":
		return http.StatusOK, f.mustJSON(rndRegisterFixture())
	case "/v1/world/catalog":
		return http.StatusOK, f.mustJSON(map[string]any{
			"ports": []any{
				map[string]any{
					"id":             recoveryHomePort,
					"banking_region": "north",
					"sea_zone_id":    "zone-north",
					"phase":          0,
					"is_hub":         true,
					"has_shipyard":   true,
					"produces":       []any{},
					"manufactures":   []any{},
					"imports":        []any{},
				},
				map[string]any{
					"id":             recoveryFarPort,
					"banking_region": "south",
					"sea_zone_id":    "zone-south",
					"phase":          0,
					"is_hub":         false,
					"has_shipyard":   false,
					"produces":       []any{},
					"manufactures":   []any{},
					"imports":        []any{},
				},
			},
			"ship_classes": []any{
				map[string]any{
					"id":                    recoveryHull,
					"hull_category":         "coaster",
					"phase":                 0,
					"base_speed_centiknots": 100,
					"hold_capacity_tons":    60,
					"max_cannons":           4,
					"crew_required":         3,
				},
			},
			"sea_zones": []any{},
		})
	case "/v1/world/clock":
		return http.StatusOK, f.mustJSON(map[string]any{
			"real_utc":       time.Now().UTC().Format(time.RFC3339Nano),
			"unlocked_phase": 0,
			"paused":         false,
			"calendar": map[string]any{
				"month":            1,
				"day":              1,
				"time_scale":       60,
				"total_game_hours": 24,
			},
		})
	case "/v1/world/zones":
		return http.StatusOK, f.mustJSON([]any{
			map[string]any{"sea_zone_id": "zone-north", "net_piracy_bps": 0},
			map[string]any{"sea_zone_id": "zone-south", "net_piracy_bps": 0},
		})
	case "/v1/world/expansion", "/v1/world/leaderboard":
		return http.StatusOK, f.mustJSON(map[string]any{})
	case "/v1/treasury/vaults":
		return http.StatusOK, f.mustJSON(map[string]any{
			"company_id": recoveryCompanyID,
			"vaults": []Vault{
				{AccountID: "vault-north", BankingRegion: "north", AccountType: "vault", BalancePence: 5_000_000},
			},
		})
	case "/v1/treasury/rates":
		return http.StatusOK, f.mustJSON([]Rate{})
	case "/v1/fleet/ships":
		return http.StatusOK, f.mustJSON(fleet)
	case "/v1/fleet/ship":
		shipID := request.URL.Query().Get("ship_id")
		for _, row := range fleet {
			ship := row["ship"].(map[string]any)
			if ship["id"] == shipID {
				return http.StatusOK, f.mustJSON(row)
			}
		}
		return http.StatusNotFound, f.mustJSON(map[string]any{
			"type": "about:blank", "title": "Not Found", "status": 404,
			"code": "ship_not_found", "detail": "no such ship: " + shipID,
		})
	case "/v1/insurance/quote":
		return http.StatusOK, f.mustJSON(map[string]any{
			"lloyds_expected_loss_bps": 0, "max_insurable_pence": 2_000_000,
			"priced_consort_ship_ids": []any{},
		})
	case "/v1/world/port", "/v1/market/book", "/v1/shipyard/quote", "/v1/world/route":
		return http.StatusOK, f.mustJSON(map[string]any{})
	default:
		// Paginated lists, the contract status lists and factor lists are all
		// empty for these scenarios.
		return http.StatusOK, f.mustJSON([]any{})
	}
}

func (f *snapshotConcurrencyAPI) mustJSON(document any) []byte {
	f.t.Helper()
	payload, err := json.Marshal(document)
	if err != nil {
		f.t.Fatalf("marshal fake document: %v", err)
	}
	return payload
}

func (f *snapshotConcurrencyAPI) writeProblem(writer http.ResponseWriter, status int, code, detail string) {
	payload := f.mustJSON(map[string]any{
		"type": "about:blank", "title": http.StatusText(status),
		"status": status, "code": code, "detail": detail,
	})
	writer.Header().Set("Content-Type", "application/json")
	writer.WriteHeader(status)
	_, _ = writer.Write(payload)
}

// boundedWaitFor polls until the condition holds or the deadline passes. It
// exists to make liveness waits bounded, never to assert speed.
func boundedWaitFor(timeout time.Duration, condition func() bool) bool {
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if condition() {
			return true
		}
		time.Sleep(time.Millisecond)
	}
	return condition()
}

func concurrencyRefreshAsync(runner *Runner, ctx context.Context) <-chan error {
	done := make(chan error, 1)
	go func() {
		done <- runner.refresh(ctx)
	}()
	return done
}

// concurrencyRunner mirrors recoveryHarness.runner but points the API client
// at the gated snapshotConcurrencyAPI server instead of the harness's default
// fake, so refresh traffic actually reaches the gates and counters.
func concurrencyRunner(h *recoveryHarness, fake *snapshotConcurrencyAPI) *Runner {
	h.t.Helper()
	client := gameapi.New(fake.server.URL, primaryTestKey, zap.NewNop())
	instance := New(client, h.db, h.account, zap.NewNop(), Options{Execute: true})
	snapshot, err := h.db.Snapshot(h.ctx, h.account.ID)
	if err != nil {
		h.t.Fatalf("load persisted snapshot: %v", err)
	}
	if snapshot != nil {
		instance.snapshot = snapshot
		instance.meta = decode[map[string]DatasetMeta](snapshot, "_meta")
		if instance.meta == nil {
			instance.meta = make(map[string]DatasetMeta)
		}
	}
	instance.expenseOwners = decode[map[string]string](instance.snapshot, "_expense_allocations")
	if instance.expenseOwners == nil {
		instance.expenseOwners = make(map[string]string)
	}
	client.SetObserver(func(method, path string, status int, duration time.Duration, request, response []byte, errText string) {
		if method != http.MethodGet {
			instance.lastMutationStatus.Store(int64(status))
		}
		if err := h.db.RecordRequest(h.ctx, h.account.ID, method, path, status, duration, request, response, errText); err != nil && h.ctx.Err() == nil {
			h.t.Logf("record request %s %s: %v", method, path, err)
		}
	})
	return instance
}

// TestCaptureWaveOverlapsIndependentDatasetsWithinWorkerCap gates every
// independent base dataset behind an unreleased channel. Once more than six
// gated GETs exist, only an overlapping worker pool (bounded at six) can make
// progress, so the peak in-flight count must land strictly between one and
// six: the serial implementation pins the peak at one and an unbounded
// implementation overshoots six. The single berthed ship and two-port catalog
// keep the child groups (ship details, ports, books, yard quotes, routes) in
// the same measurement so no wave may exceed the cap either.
func TestCaptureWaveOverlapsIndependentDatasetsWithinWorkerCap(t *testing.T) {
	t.Parallel()
	h := newRecoveryHarness(t)
	fake := newSnapshotConcurrencyAPI(t, []map[string]any{
		stationaryShipRow(recoveryShipBlastoise, "Blastoise", recoveryHomePort, "berthed", 0, 3),
	})
	fake.gatePaths(snapshotBaseDatasetPaths...)

	runner := concurrencyRunner(h, fake)
	runner.generation = "gen-overlap"
	done := concurrencyRefreshAsync(runner, h.ctx)

	if !boundedWaitFor(2*time.Second, func() bool {
		return fake.peak.Load() >= 6
	}) {
		select {
		case err := <-done:
			t.Fatalf("refresh finished while every dataset gate was still closed: %v", err)
		default:
		}
	}
	// Observation grace: an unbounded implementation keeps admitting gated
	// requests past the six-worker line and must be visible in the peak.
	time.Sleep(250 * time.Millisecond)
	peak := fake.peak.Load()
	fake.releaseAllGates()

	if err := <-done; err != nil {
		t.Fatalf("refresh with all gates released: %v", err)
	}
	if peak < 2 {
		t.Fatalf("peak in-flight dataset GETs = %d; the capture wave never overlapped independent fetches (worker pool missing or serialized)", peak)
	}
	if peak > 6 {
		t.Fatalf("peak in-flight dataset GETs = %d; the capture wave exceeded the six-worker capture bound", peak)
	}
	for _, path := range snapshotBaseDatasetPaths {
		if count := fake.arrivedCount(path); count != 1 {
			t.Fatalf("dataset %s arrived %d times in one generation, want exactly one fetch", path, count)
		}
	}
	for _, status := range []string{"open", "active", "completed", "cancelled", "defaulted"} {
		if count := fake.arrivedCount("/v1/contract/list?status=" + status); count != 1 {
			t.Fatalf("contract status %s arrived %d times, want exactly one fetch", status, count)
		}
	}
	for _, dataset := range []string{"catalog", "clock", "zones", "insurance", "insurance_owned"} {
		if !runner.fresh(dataset) {
			t.Fatalf("overlapped wave left dataset %s unfresh: %+v", dataset, runner.meta[dataset])
		}
	}
}

// TestDefinitiveDatasetFailureRetainsLastGoodWhileSiblingsUpdate fails the
// zones dataset with a definitive 4xx in the second generation. The failed
// dataset must keep its last-good bytes, UpdatedAt and generation while its
// AttemptedAt advances and the visible stale/error flags appear, the upstream
// must see exactly one attempt (4xx is definitive, never retried), the
// refresh itself must still succeed, and successful siblings must move to the
// new generation.
func TestDefinitiveDatasetFailureRetainsLastGoodWhileSiblingsUpdate(t *testing.T) {
	t.Parallel()
	h := newRecoveryHarness(t)
	fake := newSnapshotConcurrencyAPI(t, nil)

	runner := concurrencyRunner(h, fake)
	runner.generation = "gen-failure-good"
	if err := runner.refresh(h.ctx); err != nil {
		t.Fatalf("baseline refresh: %v", err)
	}
	lastGood := runner.meta["zones"]
	lastGoodBytes := string(runner.snapshot["zones"])
	if !lastGood.Complete || lastGood.Error != "" || lastGood.Stale || lastGoodBytes == "" {
		t.Fatalf("baseline zones capture is not a usable last-good state: meta=%+v bytes=%q", lastGood, lastGoodBytes)
	}

	fake.setFailureStatus("/v1/world/zones", http.StatusNotFound)
	beforeSecondWave := fake.arrivedCount("/v1/world/zones")
	runner.generation = "gen-failure-bad"
	if err := runner.refresh(h.ctx); err != nil {
		t.Fatalf("refresh with one definitively failed dataset must still succeed: %v", err)
	}

	if attempts := fake.arrivedCount("/v1/world/zones") - beforeSecondWave; attempts != 1 {
		t.Fatalf("definitive 4xx dataset attempted %d times in one wave, want exactly one non-retried attempt", attempts)
	}
	failed := runner.meta["zones"]
	if failed.Error == "" || !failed.Stale || failed.Complete {
		t.Fatalf("failed dataset lost its visible failure metadata: %+v", failed)
	}
	if !failed.UpdatedAt.Equal(lastGood.UpdatedAt) {
		t.Fatalf("failed dataset UpdatedAt moved from %v to %v; last-good observation time must be preserved", lastGood.UpdatedAt, failed.UpdatedAt)
	}
	if failed.Generation != lastGood.Generation {
		t.Fatalf("failed dataset generation moved from %s to %s; last-good authority must be preserved", lastGood.Generation, failed.Generation)
	}
	if !failed.AttemptedAt.After(lastGood.UpdatedAt) {
		t.Fatalf("failed dataset was not re-attempted in the failing generation: AttemptedAt=%v", failed.AttemptedAt)
	}
	if string(runner.snapshot["zones"]) != lastGoodBytes {
		t.Fatalf("failed dataset damaged last-good bytes: %q", string(runner.snapshot["zones"]))
	}
	sibling := runner.meta["catalog"]
	if sibling.Generation != runner.generation || sibling.Error != "" || sibling.Stale || !sibling.Complete {
		t.Fatalf("successful sibling did not advance to the new generation: %+v", sibling)
	}
	if !sibling.UpdatedAt.After(lastGood.UpdatedAt) {
		t.Fatalf("sibling UpdatedAt %v did not advance past the failed dataset's last-good %v", sibling.UpdatedAt, lastGood.UpdatedAt)
	}
	if runner.fresh("zones") {
		t.Fatal("a definitively failed dataset must not read as fresh")
	}
	if !runner.fresh("catalog") {
		t.Fatal("a successful sibling must stay fresh in the new generation")
	}
}

// TestOwnerCancellationJoinsFetchWorkersWithoutPublishingGeneration gates the
// clock dataset mid-wave and cancels the owner context while its GET is
// blocked. The canceled refresh must return a cancellation error, no request
// may still be held after it returns (workers joined, not abandoned to a
// detached context), and the store must not gain a snapshot whose metadata
// claims the canceled generation is complete.
func TestOwnerCancellationJoinsFetchWorkersWithoutPublishingGeneration(t *testing.T) {
	t.Parallel()
	h := newRecoveryHarness(t)
	fake := newSnapshotConcurrencyAPI(t, nil)
	fake.gatePaths("/v1/world/clock")

	runner := concurrencyRunner(h, fake)
	canceledGeneration := "gen-canceled"
	runner.generation = canceledGeneration

	owner, cancelOwner := context.WithCancel(h.ctx)
	defer cancelOwner()
	done := concurrencyRefreshAsync(runner, owner)

	if !boundedWaitFor(10*time.Second, func() bool {
		return fake.arrivedCount("/v1/world/clock") == 1
	}) {
		t.Fatalf("clock GET never arrived; arrivals=%d", fake.arrivedCount("/v1/world/clock"))
	}
	cancelOwner()

	select {
	case err := <-done:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("canceled refresh returned %v, want a context.Canceled chain", err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("canceled refresh did not return within the bounded deadline; the owner did not join its fetch workers")
	}

	if !boundedWaitFor(10*time.Second, func() bool {
		return fake.inFlightNow() == 0
	}) {
		t.Fatalf("%d request(s) still held after the canceled refresh returned; fetches outlived the owner context", fake.inFlightNow())
	}
	if arrivals := fake.arrivedCount("/v1/world/clock"); arrivals != 1 {
		t.Fatalf("canceled wave admitted %d clock GETs, want the single canceled attempt", arrivals)
	}

	snapshot, err := h.db.Snapshot(h.ctx, h.account.ID)
	if err != nil {
		t.Fatalf("load persisted snapshot after cancellation: %v", err)
	}
	if snapshot != nil {
		meta := decode[map[string]DatasetMeta](snapshot, "_meta")
		for key, entry := range meta {
			if entry.Generation == canceledGeneration && entry.Complete {
				t.Fatalf("canceled generation was published: dataset %s persisted as complete", key)
			}
		}
	}
}

// Five blocked siblings leave only the clock worker able to accept the seventh
// task. Its arrival proves the clock's client-side fetch and timestamp are
// complete, unlike merely observing the server write its clock response.
func TestClockEnvelopeSurvivesWaveJoinWithoutRestamping(t *testing.T) {
	t.Parallel()
	h := newRecoveryHarness(t)
	fake := newSnapshotConcurrencyAPI(t, nil)
	fake.gatePaths(
		"/v1/world/zones", "/v1/company/profile", "/v1/company/settings",
		"/v1/world/catalog", "/v1/player/profile", "/v1/world/expansion",
	)

	runner := concurrencyRunner(h, fake)
	runner.generation = "gen-clock-envelope"
	done := make(chan error, 1)
	go func() {
		done <- runner.captureWave(h.ctx, []fetchTask{
			{"clock", "/v1/world/clock", fetchJSON},
			{"zones", "/v1/world/zones", fetchJSON},
			{"profile", "/v1/company/profile", fetchJSON},
			{"settings", "/v1/company/settings", fetchJSON},
			{"catalog", "/v1/world/catalog", fetchJSON},
			{"player", "/v1/player/profile", fetchJSON},
			{"expansion", "/v1/world/expansion", fetchJSON},
		}, nil)
	}()

	if !boundedWaitFor(10*time.Second, func() bool {
		return fake.arrivedCount("/v1/world/expansion") == 1
	}) {
		fake.releaseAllGates()
		t.Fatal("clock worker did not admit the gated seventh task")
	}
	fake.releaseAllGates()

	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("refresh with sequenced gates: %v", err)
		}
	case <-time.After(30 * time.Second):
		t.Fatal("refresh did not finish within the bounded deadline")
	}

	zonesServed, _ := fake.serveTimeFor("/v1/world/zones")
	expansionServed, _ := fake.serveTimeFor("/v1/world/expansion")
	clockMeta := runner.meta["clock"]
	if clockMeta.Error != "" || clockMeta.Stale || !clockMeta.Complete || clockMeta.Generation != runner.generation {
		t.Fatalf("clock dataset lost its successful capture state: %+v", clockMeta)
	}
	if clockMeta.AttemptedAt.After(zonesServed) {
		t.Fatalf("clock AttemptedAt %v is after the zones serve at %v; the clock envelope was restamped at the wave join instead of inside its fetch task",
			clockMeta.AttemptedAt, zonesServed)
	}
	if clockMeta.UpdatedAt.After(expansionServed) {
		t.Fatalf("clock UpdatedAt %v is after the expansion serve at %v; the clock completion time was restamped after later siblings completed",
			clockMeta.UpdatedAt, expansionServed)
	}
	if clockMeta.AttemptedAt.After(clockMeta.UpdatedAt) {
		t.Fatalf("clock envelope inverted: AttemptedAt %v after UpdatedAt %v", clockMeta.AttemptedAt, clockMeta.UpdatedAt)
	}
	input := PlanningInput{Now: time.Now(), Clock: decode[Clock](runner.snapshot, "clock"), Metadata: runner.meta, Generation: runner.generation}
	if _, _, ok := input.serverBounds(); !ok {
		t.Fatal("the preserved early clock envelope does not satisfy the planner's server-clock bounds")
	}
	if !input.fresh("clock") {
		t.Fatal("the early clock dataset must stay fresh after the wave join")
	}
	if count := fake.arrivedCount("/v1/world/clock"); count != 1 {
		t.Fatalf("clock dataset fetched %d times, want a single early fetch that the join must not repeat", count)
	}
}
