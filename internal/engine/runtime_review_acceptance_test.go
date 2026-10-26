package engine

// Fix-cycle1 acceptance regressions (security-0.json sec-1/sec-5,
// architecture-0.json A3/A6). Consumer behavior only: every scenario drives
// the real Runner edges (refresh, work, driveMission, dispatch) against the
// fakeGameAPI harness and asserts wire traffic, durable mission state and
// journal identity — never helper echoes. The review proxy below is
// deliberately separate from reconciliationProxy because these scenarios need
// interception arms (quote failure/price, vault failure, first order loss)
// that must not extend the earlier frozen files.

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"go.uber.org/zap"
	"go.uber.org/zap/zapcore"

	"github.com/DevYukine/go-tradewinds/internal/store"
)

type reviewProxy struct {
	mu             sync.Mutex
	downstream     http.Handler
	recordMutation func(request *http.Request, body []byte, status int)
	contractRows   []map[string]any
	quoteFailed    bool
	quoteCostPence int64
	vaultsFailed   bool
	orderLost      bool
	deliverMode    string
	deliverLossKey string
}

func installReviewProxy(t *testing.T, h *recoveryHarness) *reviewProxy {
	t.Helper()
	proxy := &reviewProxy{downstream: h.api.server.Config.Handler}
	proxy.recordMutation = func(request *http.Request, body []byte, status int) {
		h.api.mu.Lock()
		h.api.record(request, body, status)
		h.api.mu.Unlock()
	}
	h.api.server.Config.Handler = proxy
	return proxy
}

func (p *reviewProxy) ServeHTTP(writer http.ResponseWriter, request *http.Request) {
	if request.Method == http.MethodGet {
		if p.serveGet(writer, request) {
			return
		}
	}
	if request.Method == http.MethodPost && request.URL.Path == "/v1/contract/deliver-freight" {
		if p.loseFirstKeyDelivery(writer, request) {
			return
		}
	}
	if request.Method == http.MethodPost && request.URL.Path == "/v1/market/place-order" {
		if p.loseFirstOrder(writer, request) {
			return
		}
	}
	p.downstream.ServeHTTP(writer, request)
}

// loseFirstKeyDelivery mirrors reconciliationProxy's lose_first_key mode: the
// first tender's idempotency key is captured and every wire attempt carrying
// that key is a transport loss; all other keys pass through and succeed.
func (p *reviewProxy) loseFirstKeyDelivery(writer http.ResponseWriter, request *http.Request) bool {
	body, err := io.ReadAll(request.Body)
	if err != nil {
		p.writeProblem(writer, http.StatusBadRequest, "unreadable_request", err.Error())
		return true
	}
	p.mu.Lock()
	if p.deliverMode == "lose_first_key" && p.deliverLossKey == "" {
		p.deliverLossKey = request.Header.Get("Idempotency-Key")
	}
	hijack := p.deliverMode == "lose_first_key" &&
		request.Header.Get("Idempotency-Key") == p.deliverLossKey
	p.mu.Unlock()
	if !hijack {
		// The stock fake leaves deliver-freight unsupported, so successful
		// tenders are served on the real HTTP path, mirroring
		// reconciliationProxy's synthesized success.
		var requested struct {
			ContractID string `json:"contract_id"`
		}
		_ = json.Unmarshal(body, &requested)
		p.recordMutation(request, body, http.StatusOK)
		p.writeJSON(writer, http.StatusOK, deliverFreightSuccess(requested.ContractID))
		return true
	}
	p.recordMutation(request, body, 0)
	connection, _, err := writer.(http.Hijacker).Hijack()
	if err != nil {
		p.writeProblem(writer, http.StatusInternalServerError, "hijack_failed", err.Error())
		return true
	}
	_ = connection.Close()
	return true
}

func (p *reviewProxy) setDeliverMode(mode string) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.deliverMode = mode
}

func (p *reviewProxy) serveGet(writer http.ResponseWriter, request *http.Request) bool {
	p.mu.Lock()
	quoteFailed := p.quoteFailed
	quoteCostPence := p.quoteCostPence
	vaultsFailed := p.vaultsFailed
	contractRows := append([]map[string]any(nil), p.contractRows...)
	p.mu.Unlock()

	switch request.URL.Path {
	case "/v1/contract/list":
		status := request.URL.Query().Get("status")
		rows := make([]any, 0)
		for _, row := range contractRows {
			if row["status"] == status {
				rows = append(rows, row)
			}
		}
		p.writeJSON(writer, http.StatusOK, rows)
		return true
	case "/v1/shipyard/quote":
		if quoteFailed {
			p.writeProblem(writer, http.StatusInternalServerError, "yard_unavailable", "the yard quote refused this refresh")
			return true
		}
		if quoteCostPence > 0 {
			p.writeJSON(writer, http.StatusOK, map[string]any{
				"port_id":          request.URL.Query().Get("port_id"),
				"ship_class":       request.URL.Query().Get("ship_class"),
				"build_mode":       "turnkey_cash",
				"available":        true,
				"total_cost_pence": quoteCostPence,
			})
			return true
		}
	case "/v1/treasury/vaults":
		if vaultsFailed {
			p.writeProblem(writer, http.StatusInternalServerError, "vaults_unavailable", "the vault list refused this refresh")
			return true
		}
	}
	return false
}

func (p *reviewProxy) loseFirstOrder(writer http.ResponseWriter, request *http.Request) bool {
	body, err := io.ReadAll(request.Body)
	if err != nil {
		p.writeProblem(writer, http.StatusBadRequest, "unreadable_request", err.Error())
		return true
	}
	p.mu.Lock()
	lose := p.orderLost
	p.mu.Unlock()
	if !lose {
		replayed := request.Clone(request.Context())
		replayed.Body = io.NopCloser(bytes.NewReader(body))
		p.downstream.ServeHTTP(writer, replayed)
		return true
	}
	p.recordMutation(request, body, 0)
	connection, _, err := writer.(http.Hijacker).Hijack()
	if err != nil {
		p.writeProblem(writer, http.StatusInternalServerError, "hijack_failed", err.Error())
		return true
	}
	_ = connection.Close()
	return true
}

func (p *reviewProxy) setContractRows(rows []map[string]any) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.contractRows = rows
}

func (p *reviewProxy) setQuoteRefreshFailed(failed bool) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.quoteFailed = failed
}

func (p *reviewProxy) setQuoteCostPence(cost int64) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.quoteCostPence = cost
}

func (p *reviewProxy) setVaultsRefreshFailed(failed bool) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.vaultsFailed = failed
}

func (p *reviewProxy) setOrderLost(lost bool) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.orderLost = lost
}

func (p *reviewProxy) writeJSON(writer http.ResponseWriter, status int, document any) {
	payload, err := json.Marshal(document)
	if err != nil {
		http.Error(writer, err.Error(), http.StatusInternalServerError)
		return
	}
	writer.Header().Set("Content-Type", "application/json")
	writer.WriteHeader(status)
	_, _ = writer.Write(payload)
}

func (p *reviewProxy) writeProblem(writer http.ResponseWriter, status int, code, detail string) {
	p.writeJSON(writer, status, map[string]any{
		"type":   "about:blank",
		"title":  http.StatusText(status),
		"status": status,
		"code":   code,
		"detail": detail,
	})
}

func berthedLaprasRow() map[string]any {
	row := sailingShipRow(recoveryShipLapras, "Lapras", recoveryHomePort, recoveryFarPort)
	ship := row["ship"].(map[string]any)
	ship["status"] = "berthed"
	ship["cannons_count"] = 4
	row["effective_hold_capacity_tons"] = 52
	row["stores"] = map[string]any{"provisions_tons": 4, "sea_days_remaining": 5}
	row["route_legs"] = []any{}
	row["voyage"] = nil
	return row
}

// stageFundedGrowthBuild leaves a valid funded never-journaled build intent on
// __growth. Shared by the first-send revalidation tests and the escalated
// preflight-invalidation regression.
func stageFundedGrowthBuild(t *testing.T, h *recoveryHarness) *Runner {
	t.Helper()
	h.api.setVaultBalance("north", 500_000)
	h.api.setVaultBalance("south", 100_000)
	h.seedLaneProofs()
	runner := h.runner(primaryTestKey)
	if _, err := h.tick(runner); err != nil {
		t.Fatalf("stage growth build: %v", err)
	}
	_, staged := h.mission("__growth")
	if staged.Action == nil || staged.Action.Stage != "build" {
		t.Fatalf("expected a staged never-journaled build: %+v", staged.Action)
	}
	if h.api.mutationCount("/v1/shipyard/build-ship") != 0 {
		t.Fatal("staging must not send the build request")
	}
	return runner
}

// sec-1: a never-journaled ship-less build must be revalidated at first send.
// A yard quote that went stale or lost access, or an upstream repricing that
// makes the full growth commitment (hull, guns, stores, cycles, upkeep and
// regional reserves) unfunded, must reject the dispatch before transport.
// An already-attempted (journaled) build keeps its immutable replay.
func TestBuildFirstSendRevalidatesQuoteAndFunding(t *testing.T) {
	t.Parallel()
	t.Run("stale yard quote rejects first send", func(t *testing.T) {
		h := newRecoveryHarness(t)
		proxy := installReviewProxy(t, h)
		runner := stageFundedGrowthBuild(t, h)

		proxy.setQuoteRefreshFailed(true)
		if _, err := h.tick(runner); err != nil {
			t.Fatalf("dispatch tick with stale quote: %v", err)
		}
		if h.api.mutationCount("/v1/shipyard/build-ship") != 0 {
			t.Fatal("a stale yard quote still sent the build request")
		}
		_, data := h.mission("__growth")
		if data.LastActionID != "" {
			t.Fatalf("stale-quote build was consumed as a success: %+v", data)
		}
	})

	t.Run("repriced unfunded full growth rejects first send", func(t *testing.T) {
		h := newRecoveryHarness(t)
		proxy := installReviewProxy(t, h)
		runner := stageFundedGrowthBuild(t, h)

		proxy.setQuoteCostPence(5_000_000)
		h.api.setVaultBalance("north", 300_000)
		if _, err := h.tick(runner); err != nil {
			t.Fatalf("dispatch tick with repriced yard: %v", err)
		}
		if h.api.mutationCount("/v1/shipyard/build-ship") != 0 {
			t.Fatal("a repriced unfunded build still reached transport")
		}
		_, data := h.mission("__growth")
		if data.LastActionID != "" {
			t.Fatalf("unfunded build was consumed as a success: %+v", data)
		}
	})

	t.Run("attempted build replays its immutable request", func(t *testing.T) {
		h := newRecoveryHarness(t)
		proxy := installReviewProxy(t, h)
		runner := stageFundedGrowthBuild(t, h)

		_, staged := h.mission("__growth")
		attemptedID := staged.Action.ID
		attemptedBody := staged.Action.Body
		if _, err := h.db.BeginAction(h.ctx, &store.Action{
			ID:        attemptedID,
			AccountID: h.account.ID,
			Method:    http.MethodPost,
			Path:      "/v1/shipyard/build-ship",
			Body:      attemptedBody,
		}); err != nil {
			t.Fatalf("seed attempted build journal row: %v", err)
		}

		// A published financial event must not block an already-attempted
		// immutable retry; only first sends revalidate the capture.
		h.api.setVaultBalance("north", 75_000)
		runner.publishedFinancial.Add(1)
		proxy.setQuoteRefreshFailed(true)
		if _, err := h.tick(runner); err != nil {
			t.Fatalf("replay attempted build: %v", err)
		}
		calls := h.api.mutationsFor("/v1/shipyard/build-ship")
		if len(calls) != 1 || calls[0].IdempotencyKey != attemptedID || !bytes.Equal(calls[0].Body, attemptedBody) {
			t.Fatalf("attempted build lost its immutable replay: %+v", calls)
		}
	})

}

// Escalated c0-sec-sec-5: the dispatch path validated the financial epoch only
// before newActionPreflight, so a financial event published while
// growthBuildReady's SettlementHistory funding work was in flight reached
// first transport. An ACCESS EXCLUSIVE lock on the harness schema's
// settlements table parks that exact query strictly after the early check;
// the capture check must be repeated before the POST, durably reconcile the
// never-sent request with kind never_sent_financial_event and release the
// intent with its build obligations intact.
func TestGrowthPreflightInvalidatesEpochPublishedDuringWork(t *testing.T) {
	t.Parallel()
	h := newRecoveryHarness(t)
	runner := stageFundedGrowthBuild(t, h)
	_, staged := h.mission("__growth")
	originalID := staged.Action.ID
	originalBody := append(json.RawMessage(nil), staged.Action.Body...)
	buildClass := staged.BuildClass
	buildName := staged.BuildName
	buildPort := staged.BuildPort

	// Gate the existing "dispatching durable action" marker (journal.go
	// dispatch): the worker parks inside the log hook after the initial
	// preflight and before the early financial guard, so the settlements
	// lock can be taken at the architect's exact hook.
	gateOpened := make(chan struct{})
	var markerOnce, openGateOnce sync.Once
	gate := make(chan struct{})
	openGate := func() {
		openGateOnce.Do(func() { close(gate) })
	}
	core := zapcore.NewCore(zapcore.NewJSONEncoder(zap.NewProductionEncoderConfig()), zapcore.AddSync(io.Discard), zapcore.DebugLevel)
	runner.log = zap.New(core, zap.Hooks(func(entry zapcore.Entry) error {
		if entry.Message != "dispatching durable action" {
			return nil
		}
		markerOnce.Do(func() { close(gateOpened) })
		<-gate
		return nil
	}))

	lock, waitBlocked, releaseBarrier := reviewSettlementsBarrier(t, h)

	mission, pendingData := h.mission("__growth")
	type dispatchOutcome struct {
		changed bool
		err     error
	}
	done := make(chan dispatchOutcome, 1)
	dispatchCtx, cancelDispatch := context.WithCancel(h.ctx)
	go func(mission *store.Mission, data missionData) {
		changed, err := runner.dispatch(dispatchCtx, mission, &data)
		done <- dispatchOutcome{changed: changed, err: err}
	}(mission, pendingData)

	// Every failure path after launch must open the gate, release the
	// barrier and join the worker before the harness store tears down.
	var joined atomic.Bool
	t.Cleanup(func() {
		openGate()
		releaseBarrier()
		cancelDispatch()
		if !joined.Load() {
			select {
			case <-done:
			case <-time.After(5 * time.Second):
				t.Error("dispatch did not stop after cancellation")
			}
		}
	})

	select {
	case <-gateOpened:
	case <-time.After(10 * time.Second):
		t.Fatal("dispatch never reached the durable-action marker")
	}

	// The lock is taken only at the marker, after the initial preflight and
	// before the early financial guard, so the guard passes and the next
	// preflight's SettlementHistory work parks on it.
	lockPID := lock()
	openGate()
	if !waitBlocked(lockPID) {
		t.Fatal("growth preflight never reached the settlements barrier")
	}

	// The production publication point: company cash moved and the event
	// stream's epoch counter advanced while preflight work was in flight.
	h.api.setVaultBalance("north", 75_000)
	runner.publishedFinancial.Add(1)
	releaseBarrier()

	var outcome dispatchOutcome
	select {
	case outcome = <-done:
	case <-time.After(30 * time.Second):
		t.Fatal("dispatch never returned after the preflight barrier released")
	}
	joined.Store(true)
	if outcome.err != nil {
		t.Fatalf("released dispatch errored: %v", outcome.err)
	}
	if !outcome.changed {
		t.Fatal("released dispatch did not advance the invalidated mission")
	}
	if h.api.mutationCount("/v1/shipyard/build-ship") != 0 {
		t.Fatal("a financial event published during preflight let the build cross the first-send boundary")
	}
	_, data := h.mission("__growth")
	if data.Action != nil || data.Sequence != 1 {
		t.Fatalf("invalidated intent was not released: action %+v sequence %d", data.Action, data.Sequence)
	}
	if data.BuildClass != buildClass || data.BuildName != buildName || data.BuildPort != buildPort {
		t.Fatalf("the released replan lost durable build obligations: class %q name %q port %q", data.BuildClass, data.BuildName, data.BuildPort)
	}
	persisted, err := h.db.BeginAction(h.ctx, &store.Action{
		ID:        originalID,
		AccountID: h.account.ID,
		Method:    http.MethodPost,
		Path:      "/v1/shipyard/build-ship",
		Body:      originalBody,
	})
	if err != nil {
		t.Fatal(err)
	}
	if persisted.State != store.ActionReconciled {
		t.Fatalf("invalidated request was not durably reconciled: state %s", persisted.State)
	}
	var evidence struct {
		Kind string `json:"kind"`
	}
	if err := json.Unmarshal(persisted.Response, &evidence); err != nil {
		t.Fatalf("reconciliation evidence is not valid JSON: %v", err)
	}
	if evidence.Kind != neverSentFinancialEvent {
		t.Fatalf("reconciliation kind = %q, want %q", evidence.Kind, neverSentFinancialEvent)
	}

}

// reviewSettlementsBarrier takes an ACCESS EXCLUSIVE lock on the harness
// schema's settlements table on demand. lock acquires it and returns the
// locking connection's backend PID; waitBlocked reports once a settlements
// query is observed blocked specifically by that PID, proving the dispatch
// preflight's SettlementHistory work is parked mid-flight. release drops the
// lock, is idempotent and runs on cleanup for every exit path.
func reviewSettlementsBarrier(t *testing.T, h *recoveryHarness) (lock func() int, waitBlocked func(lockPID int) bool, release func()) {
	t.Helper()
	db, err := sql.Open("pgx", h.dsn)
	if err != nil {
		t.Fatal("open settlements barrier database")
	}
	barrierCtx, cancelBarrier := context.WithTimeout(h.ctx, 30*time.Second)
	var (
		conn     *sql.Conn
		tx       *sql.Tx
		lockPID  int
		acquired bool
	)
	var releaseOnce sync.Once
	release = func() {
		releaseOnce.Do(func() {
			defer cancelBarrier()
			if tx != nil {
				if err := tx.Rollback(); err != nil && !errors.Is(err, sql.ErrTxDone) && barrierCtx.Err() == nil {
					t.Errorf("release settlements barrier: %v", err)
				}
			}
			if conn != nil {
				_ = conn.Close()
			}
			_ = db.Close()
		})
	}
	t.Cleanup(release)

	lock = func() int {
		t.Helper()
		if acquired {
			return lockPID
		}
		conn, err = db.Conn(barrierCtx)
		if err != nil {
			t.Fatal(err)
		}
		tx, err = conn.BeginTx(barrierCtx, nil)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := tx.ExecContext(barrierCtx, "LOCK TABLE "+h.schema+".settlements IN ACCESS EXCLUSIVE MODE"); err != nil {
			t.Fatal(err)
		}
		if err := conn.QueryRowContext(barrierCtx, "SELECT pg_backend_pid()").Scan(&lockPID); err != nil {
			t.Fatal(err)
		}
		acquired = true
		return lockPID
	}
	waitBlocked = func(pid int) bool {
		pollCtx, cancelPoll := context.WithTimeout(barrierCtx, 10*time.Second)
		defer cancelPoll()
		ticker := time.NewTicker(5 * time.Millisecond)
		defer ticker.Stop()
		for {
			var waiting int
			if err := db.QueryRowContext(pollCtx,
				"SELECT count(*) FROM pg_stat_activity WHERE wait_event_type = 'Lock' AND query ILIKE '%settlements%' AND $1 = ANY(pg_blocking_pids(pid))", pid).Scan(&waiting); err != nil {
				t.Logf("settlements barrier poll: %v", err)
				return false
			} else if waiting > 0 {
				return true
			}
			select {
			case <-pollCtx.Done():
				return false
			case <-ticker.C:
			}
		}
	}
	return lock, waitBlocked, release
}

// sec-5: staged-but-never-journaled spending requires current financial
// authority. A failed vault refresh must not let the intent reach transport,
// fresh funds below the regional reserve must reject it, and an already
// attempted request keeps its original key and body replay even after the
// freshness loss.
func TestSpendingFirstSendRequiresFreshVaultAuthority(t *testing.T) {
	t.Parallel()
	stageBuy := func(t *testing.T, h *recoveryHarness) (*Runner, string) {
		t.Helper()
		// First-send validation is the target, not the planner: put the ship
		// in the buy phase at fresh captured state and let the production
		// r.stage call produce the real immutable intent.
		h.api.setFleet([]map[string]any{berthedLaprasRow()})
		h.api.setFactorPorts(recoveryFarPort)
		runner := refreshedRunner(t, h)
		h.putMission(recoveryShipLapras, "review-spending-generation", "buy", missionData{
			Plan: Plan{
				Kind:          "spot",
				ShipID:        recoveryShipLapras,
				Origin:        recoveryHomePort,
				Destination:   recoveryFarPort,
				Commodity:     recoveryGrain,
				Quantity:      24,
				BuyLimitPence: 100,
			},
			Eligible: map[string]int64{},
		})
		if _, err := h.tick(runner); err != nil {
			t.Fatalf("stage immutable buy: %v", err)
		}
		_, staged := h.mission(recoveryShipLapras)
		if staged.Action == nil || staged.Action.Stage != "buy" {
			t.Fatalf("runner.stage did not produce a buy intent: %+v", staged.Action)
		}
		if h.api.mutationCount("/v1/market/place-order") != 0 {
			t.Fatal("staging must not send the buy request")
		}
		return runner, staged.Action.ID
	}

	t.Run("failed vault refresh blocks first send", func(t *testing.T) {
		h := newRecoveryHarness(t)
		proxy := installReviewProxy(t, h)
		runner, _ := stageBuy(t, h)

		proxy.setVaultsRefreshFailed(true)
		if _, err := h.tick(runner); err != nil {
			t.Fatalf("dispatch tick with failed vault refresh: %v", err)
		}
		if h.api.mutationCount("/v1/market/place-order") != 0 {
			t.Fatal("stale vault evidence still sent the buy request")
		}
	})

	t.Run("fresh funds below reserve reject first send", func(t *testing.T) {
		h := newRecoveryHarness(t)
		installReviewProxy(t, h)
		runner, _ := stageBuy(t, h)

		h.api.setVaultBalance("north", 1_000)
		if _, err := h.tick(runner); err != nil {
			t.Fatalf("dispatch tick with drained funds: %v", err)
		}
		if h.api.mutationCount("/v1/market/place-order") != 0 {
			t.Fatal("spending below the regional reserve still reached transport")
		}
	})

	t.Run("attempted buy replays after freshness loss", func(t *testing.T) {
		h := newRecoveryHarness(t)
		proxy := installReviewProxy(t, h)
		runner, stagedID := stageBuy(t, h)

		// The native client retries transport losses inside one dispatch, so
		// the window must stay armed for every attempt of this first send;
		// only the post-loss replay may reach the wire.
		proxy.setOrderLost(true)
		mission, data := h.mission(recoveryShipLapras)
		if _, err := runner.dispatch(h.ctx, mission, &data); err != nil {
			t.Fatalf("transport-lost buy dispatch: %v", err)
		}
		pending := h.pending()
		if len(pending) != 1 || pending[0].ID != stagedID {
			t.Fatalf("attempted buy was not durably retained: %+v", pending)
		}
		attemptedBody := pending[0].Body

		h.expireCooldown(recoveryShipLapras)
		proxy.setOrderLost(false)
		proxy.setVaultsRefreshFailed(true)
		if _, err := h.tick(runner); err != nil {
			t.Fatalf("replay tick after freshness loss: %v", err)
		}
		calls := h.api.mutationsFor("/v1/market/place-order")
		if len(calls) < 2 {
			t.Fatalf("expected the lost attempt and its replay on the wire: %+v", calls)
		}
		for _, call := range calls[:len(calls)-1] {
			if call.Status != 0 || call.IdempotencyKey != stagedID {
				t.Fatalf("lost attempts were not identical transport losses: %+v", calls)
			}
		}
		replayed := calls[len(calls)-1]
		if replayed.Status != http.StatusOK || replayed.IdempotencyKey != stagedID || !bytes.Equal(replayed.Body, attemptedBody) {
			t.Fatalf("replay changed the immutable request: %+v", replayed)
		}
	})

}

// A3: the quarantine credit-only pass must never tender a recovered
// depart-phase mission at its current port (its earlier freight stop is
// elsewhere), which would overwrite the phase and permanently hold the ship.
// An eligible arrival-phase mission with a matching destination still earns
// its credit during the same quarantine.
func TestQuarantineCreditSkipsDepartPhaseMission(t *testing.T) {
	t.Parallel()
	h := newRecoveryHarness(t)
	proxy := installReviewProxy(t, h)
	proxy.setContractRows([]map[string]any{
		freightContractRow("freight-a", "active", false),
		freightContractRowFor("freight-b1", "active", false, recoveryShipCharizard),
		freightContractRowFor("freight-b2", "active", false, recoveryShipCharizard),
		freightContractRowFor("freight-c", "active", false, recoveryShipLapras),
	})
	proxy.setDeliverMode("lose_first_key")
	h.api.setFleet([]map[string]any{
		stationaryShipRow(recoveryShipBlastoise, "Blastoise", recoveryFarPort, "berthed", 24, 3),
		stationaryShipRow(recoveryShipCharizard, "Charizard", recoveryHomePort, "berthed", 48, 3),
		stationaryShipRow(recoveryShipLapras, "Lapras", recoveryFarPort, "berthed", 24, 3),
	})
	runner := refreshedRunner(t, h)

	obligation := func(id string, destination string, in time.Duration) freightObligation {
		deadline := time.Now().UTC().Add(in)
		return freightObligation{
			ContractID:      id,
			Commodity:       recoveryGrain,
			Quantity:        24,
			Destination:     destination,
			Deadline:        &deadline,
			RewardPence:     9_000,
			CollateralPence: 5_000,
		}
	}
	// Ship A: the quarantined ambiguous tender.
	h.putMission(recoveryShipBlastoise, "review-quarantine-a", "arrival", missionData{
		Plan:     Plan{Kind: "recovery", ShipID: recoveryShipBlastoise, Destination: recoveryFarPort},
		Freight:  []freightObligation{obligation("freight-a", recoveryFarPort, 6*time.Hour)},
		Eligible: map[string]int64{},
	})
	// Ship B: recovered depart-phase mission; its earlier stop is portb while
	// a later obligation sits at the current port.
	h.putMission(recoveryShipCharizard, "review-quarantine-b", "depart", missionData{
		Plan:     Plan{Kind: "recovery", ShipID: recoveryShipCharizard, Origin: recoveryHomePort, Destination: recoveryFarPort},
		Freight:  []freightObligation{obligation("freight-b1", recoveryFarPort, 48*time.Hour), obligation("freight-b2", recoveryHomePort, 96*time.Hour)},
		Eligible: map[string]int64{},
	})
	// Ship C: eligible arrival-phase credit at its matching destination.
	h.putMission(recoveryShipLapras, "review-quarantine-c", "arrival", missionData{
		Plan:     Plan{Kind: "recovery", ShipID: recoveryShipLapras, Destination: recoveryFarPort},
		Freight:  []freightObligation{obligation("freight-c", recoveryFarPort, 6*time.Hour)},
		Eligible: map[string]int64{},
	})

	missionA, dataA := h.mission(recoveryShipBlastoise)
	shipA := harnessShip(t, runner, recoveryShipBlastoise)
	if _, err := runner.driveMission(h.ctx, shipA, missionA, &dataA); err != nil {
		t.Fatalf("stage quarantined tender: %v", err)
	}
	missionA, dataA = h.mission(recoveryShipBlastoise)
	ambiguousID := dataA.Action.ID
	if _, err := runner.dispatch(h.ctx, missionA, &dataA); err != nil {
		t.Fatalf("dispatch transport-lost tender: %v", err)
	}
	dataA.CooldownUntil = time.Now().UTC().Add(time.Hour)
	if err := runner.saveMission(h.ctx, missionA, &dataA); err != nil {
		t.Fatalf("pin quarantine window: %v", err)
	}

	if _, err := h.tick(runner); err != nil {
		t.Fatalf("quarantine tick: %v", err)
	}
	if _, err := h.tick(runner); err != nil {
		t.Fatalf("quarantine dispatch tick: %v", err)
	}

	_, dataC := h.mission(recoveryShipLapras)
	if !dataC.Freight[0].Settled || dataC.ProceedsPence != 9_000 {
		t.Fatalf("eligible arrival credit did not work during quarantine: %+v", dataC)
	}

	missionB, dataB := h.mission(recoveryShipCharizard)
	if missionB.Phase != "depart" {
		t.Fatalf("credit-only pass overwrote the depart phase: %s", missionB.Phase)
	}
	if dataB.Action != nil {
		t.Fatalf("depart-phase mission staged a credit tender: %+v", dataB.Action)
	}
	for _, obligation := range dataB.Freight {
		if obligation.Settled {
			t.Fatalf("depart-phase mission lost a seal during quarantine: %+v", obligation)
		}
	}
	calls := h.api.mutationsFor("/v1/contract/deliver-freight")
	for _, call := range calls {
		var request struct {
			ContractID string `json:"contract_id"`
		}
		if err := json.Unmarshal(call.Body, &request); err != nil {
			t.Fatal(err)
		}
		if request.ContractID == "freight-b1" || request.ContractID == "freight-b2" {
			t.Fatalf("depart-phase mission was tendered during quarantine: %+v", call)
		}
	}
	pending := h.pending()
	if len(pending) != 1 || pending[0].ID != ambiguousID {
		t.Fatalf("quarantined request was not durably retained: %+v", pending)
	}

}

// A6: a sunk ship must reconcile authoritative terminal freight before the
// sunk shortcut: defaulted settles the collateral exactly once and the
// mission finishes across restarts, while active or missing contracts remain
// unresolved instead of fabricating a settlement.
func TestSunkShipReconcilesTerminalFreightBeforeFinish(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name       string
		rowStatus  string
		includeRow bool
	}{
		{name: "fresh defaulted reconciles collateral once and finishes", rowStatus: "defaulted", includeRow: true},
		{name: "fresh active stays unresolved", rowStatus: "active", includeRow: true},
		{name: "missing record stays unresolved", includeRow: false},
	}
	for _, test := range cases {
		t.Run(test.name, func(t *testing.T) {
			h := newRecoveryHarness(t)
			proxy := installReviewProxy(t, h)
			rows := []map[string]any{}
			if test.includeRow {
				rows = append(rows, freightContractRowFor("freight-x", test.rowStatus, false, recoveryShipLapras))
			}
			proxy.setContractRows(rows)
			h.api.setFleet([]map[string]any{
				sunkShipRow(recoveryShipLapras, "Lapras"),
			})

			// The settlement insert requires a durably completed journal
			// action owned by this account; seed the historical accept
			// instead of referencing an ID that never existed.
			destinationSeedAction(t, h, "review-sunk-accept", "/v1/contract/accept-freight",
				`{"contract_id":"freight-x","ship_id":"`+recoveryShipLapras+`","quantity":24}`,
				`{"ok":true}`)
			sunkDeadline := time.Now().UTC().Add(6 * time.Hour)
			h.putMission(recoveryShipLapras, "review-sunk-generation", "arrival", missionData{
				Plan:         Plan{Kind: "freight", ShipID: recoveryShipLapras, Origin: recoveryHomePort, Destination: recoveryFarPort, Commodity: recoveryGrain},
				Freight:      []freightObligation{{ContractID: "freight-x", Commodity: recoveryGrain, Quantity: 24, Destination: recoveryFarPort, Deadline: &sunkDeadline, RewardPence: 9_000, CollateralPence: 5_000}},
				Eligible:     map[string]int64{},
				LastActionID: "review-sunk-accept",
				CostsKnown:   true,
			})
			runner := refreshedRunner(t, h)
			ship := harnessShip(t, runner, recoveryShipLapras)
			mission, data := h.mission(recoveryShipLapras)
			if _, err := runner.driveMission(h.ctx, ship, mission, &data); err != nil {
				t.Fatalf("drive sunk mission: %v", err)
			}

			mission, data = h.mission(recoveryShipLapras)
			switch test.rowStatus {
			case "defaulted":
				if !data.Freight[0].Settled || data.ExpensesPence != 5_000 || data.CostsKnown || data.Settlement != nil {
					t.Fatalf("terminal default was not reconciled before the sunk shortcut: %+v", data)
				}
			default:
				if data.Freight[0].Settled {
					t.Fatalf("unresolved contract was settled: %+v", data.Freight[0])
				}
				if test.includeRow == false && data.Settlement != nil {
					t.Fatalf("missing contract fabricated a settlement: %+v", data.Settlement)
				}
			}

			if test.rowStatus != "defaulted" {
				if h.api.mutationCount("/v1/contract/deliver-freight") != 0 {
					t.Fatal("sunk reconciliation produced tender traffic")
				}
				return
			}
			// Durable settlement stages across restarts, one wake each:
			// seal, persist the immutable settlement, insert it, finish.
			driveSunk := func(stage string) {
				t.Helper()
				restarted := refreshedRunner(t, h)
				mission, data := h.mission(recoveryShipLapras)
				ship := harnessShip(t, restarted, recoveryShipLapras)
				if _, err := restarted.driveMission(h.ctx, ship, mission, &data); err != nil {
					t.Fatalf("%s: %v", stage, err)
				}
			}
			driveSunk("persist terminal settlement")
			mission, data = h.mission(recoveryShipLapras)
			if mission.Phase != "settling" || data.Settlement == nil || data.ExpensesPence != 5_000 || !data.Freight[0].Settled {
				t.Fatalf("terminal settlement was not persisted before insert: phase %s data %+v", mission.Phase, data)
			}
			driveSunk("insert terminal settlement")
			mission, data = h.mission(recoveryShipLapras)
			if mission.Phase != "settled" || data.ExpensesPence != 5_000 {
				t.Fatalf("settled sunk mission did not keep its once-only loss: phase %s expenses %d", mission.Phase, data.ExpensesPence)
			}
			driveSunk("finish sunk mission")
			row, err := h.db.GetMission(h.ctx, h.account.ID, recoveryShipLapras)
			if err != nil {
				t.Fatal(err)
			}
			if row != nil {
				t.Fatalf("settled sunk mission was not finished: phase %s", row.Phase)
			}
			if h.api.mutationCount("/v1/contract/deliver-freight") != 0 {
				t.Fatal("sunk reconciliation produced tender traffic")
			}
		})
	}

}
