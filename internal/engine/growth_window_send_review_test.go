package engine

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"testing"
	"time"

	"github.com/DevYukine/go-tradewinds/internal/store"
	"go.uber.org/zap"
	"go.uber.org/zap/zapcore"
)

func growthWindowSendReviewAdmit(t *testing.T, h *recoveryHarness, runner *Runner) (*store.Mission, missionData) {
	t.Helper()
	if changed, err := runner.considerGrowth(h.ctx); err != nil || !changed {
		t.Fatalf("admit funded build: changed %t error %v holds %v", changed, err, runner.holding)
	}
	mission, data := growthMissionWithHoldDiagnostics(t, h, runner)
	if mission.Phase != "growth_build" || data.Action == nil || data.Action.Stage != "build" {
		t.Fatalf("fixture did not stage a build: phase %s data %+v", mission.Phase, data)
	}
	if exists, err := h.db.HasAction(h.ctx, h.account.ID, data.Action.ID); err != nil || exists {
		t.Fatalf("staged build already has attempted journal evidence: %t %v", exists, err)
	}
	if err := runner.newActionPreflight(h.ctx, mission, &data); err != nil {
		t.Fatalf("unchanged staged build failed its initial first-send guard: %v", err)
	}
	return mission, data
}

func growthWindowSendReviewFixture(t *testing.T) (*recoveryHarness, *Runner, *store.Mission, missionData) {
	t.Helper()
	h := newRecoveryHarness(t)
	h.seedLaneProofs()
	runner := growthWindowRunner(t, h)
	mission, data := growthWindowSendReviewAdmit(t, h, runner)
	return h, runner, mission, data
}

func growthWindowSendReviewBlock(t *testing.T, h *recoveryHarness, kind string) {
	t.Helper()
	row := store.Settlement{
		ID: "send-review-" + kind, Kind: "aborted", CostsKnown: true,
		GameSeconds: 3_600, CompletedAt: time.Now().UTC().Add(-time.Minute),
	}
	switch kind {
	case "loss":
		row.AcquisitionPence = 100_000_000
	case "unknown":
		row.CostsKnown = false
	case "future":
		row.CompletedAt = time.Now().UTC().Add(time.Hour)
	default:
		t.Fatalf("unsupported blocking evidence %q", kind)
	}
	persistGrowthWindowSettlement(t, h, row)
}

// Match the existing insurance post-wait regression: the debug boundary is
// after initial preflight and journal creation, not keyed to log wording.
// A real response publishes a write-bucket pause; production's second guard
// must run after that pause, even though the earlier guard admitted the build.
func growthWindowSendReviewPause(t *testing.T, h *recoveryHarness, runner *Runner, until time.Time, afterInitialGuard func()) *bool {
	t.Helper()
	downstream := h.api.server.Config.Handler
	h.api.server.Config.Handler = http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		if request.URL.Path == "/test/growth-window-write-pause" {
			writer.Header().Set("X-Ratelimit-Bucket", "write")
			writer.Header().Set("Retry-After", until.UTC().Format(http.TimeFormat))
			h.api.writeJSON(writer, http.StatusOK, map[string]any{})
			return
		}
		downstream.ServeHTTP(writer, request)
	})
	injected := false
	core := zapcore.NewCore(zapcore.NewJSONEncoder(zap.NewProductionEncoderConfig()), zapcore.AddSync(io.Discard), zap.DebugLevel)
	runner.log = zap.New(core, zap.Hooks(func(entry zapcore.Entry) error {
		if injected || entry.Level != zap.DebugLevel {
			return nil
		}
		injected = true
		if afterInitialGuard != nil {
			afterInitialGuard()
		}
		if !time.Now().Before(until.Truncate(time.Second)) {
			t.Fatal("fixture missed its bounded real limiter interval before transport")
		}
		var response json.RawMessage
		if err := runner.api.Get(h.ctx, "/test/growth-window-write-pause", &response); err != nil {
			t.Fatalf("publish real write-bucket pause: %v", err)
		}
		return nil
	}))
	return &injected
}

func growthWindowSendReviewAssertVeto(t *testing.T, h *recoveryHarness, mission *store.Mission, original missionData, postWait bool) {
	t.Helper()
	if calls := h.api.mutationsFor("/v1/shipyard/build-ship"); len(calls) != 0 {
		t.Fatalf("obsolete build crossed HTTP transport: %+v", calls)
	}
	if pending := h.pending(); len(pending) != 0 {
		t.Fatalf("known-unsent build remained ambiguously pending: %+v", pending)
	}
	retained, data := h.mission("__growth")
	if retained.Generation != mission.Generation || data.Action != nil || data.Sequence != original.Sequence+1 ||
		!bytes.Equal(encode(data.Plan), encode(original.Plan)) || data.BuiltShipID != "" {
		t.Fatalf("no-send transition changed the admitted lot or retained its action: %+v", data)
	}
	action := original.Action
	exists, err := h.db.HasAction(h.ctx, h.account.ID, action.ID)
	if err != nil || exists != postWait {
		t.Fatalf("first-send journal boundary changed: exists %t post-wait %t error %v", exists, postWait, err)
	}
	if postWait {
		// Exact existing identity: this reads the terminal no-send row rather
		// than fabricating or aging an attempted action.
		journaled, err := h.db.BeginAction(h.ctx, &store.Action{
			ID: action.ID, AccountID: h.account.ID, Method: action.Method, Path: action.Path, Body: action.Body,
		})
		if err != nil {
			t.Fatalf("read proven-unsent action: %v", err)
		}
		var evidence struct {
			Kind string `json:"kind"`
		}
		if err := json.Unmarshal(journaled.Response, &evidence); err != nil ||
			journaled.State != store.ActionReconciled || evidence.Kind != neverSentPrecondition ||
			!bytes.Equal(journaled.Body, action.Body) {
			t.Fatalf("post-wait veto lacks immutable durable no-send proof: %+v error %v", journaled, err)
		}
	}
}

func TestGrowthWindowSendReviewUnchangedProofSendsOriginalBuild(t *testing.T) {
	h, runner, mission, data := growthWindowSendReviewFixture(t)
	original := *data.Action
	if _, err := runner.dispatch(h.ctx, mission, &data); err != nil {
		t.Fatalf("send unchanged justified build: %v", err)
	}
	calls := h.api.mutationsFor("/v1/shipyard/build-ship")
	if len(calls) != 1 || calls[0].IdempotencyKey != original.ID || !bytes.Equal(calls[0].Body, original.Body) {
		t.Fatalf("unchanged proof lost its exact first send: %+v", calls)
	}
	_, built := h.mission("__growth")
	if built.BuiltShipID != recoveryBuiltShipID || built.Action != nil {
		t.Fatalf("real build result did not advance commissioning: %+v", built)
	}
}

func TestGrowthWindowSendReviewNewEvidenceBeforeDispatch(t *testing.T) {
	for _, kind := range []string{"loss", "unknown", "future"} {
		t.Run(kind, func(t *testing.T) {
			h, runner, mission, data := growthWindowSendReviewFixture(t)
			original := data
			growthWindowSendReviewBlock(t, h, kind)
			if _, err := runner.dispatch(h.ctx, mission, &data); err != nil {
				t.Fatalf("veto staged build after new %s evidence: %v", kind, err)
			}
			growthWindowSendReviewAssertVeto(t, h, mission, original, false)
		})
	}
}

func TestGrowthWindowSendReviewNewEvidenceAcrossLimiterWait(t *testing.T) {
	for _, kind := range []string{"loss", "unknown", "future"} {
		t.Run(kind, func(t *testing.T) {
			h, runner, mission, data := growthWindowSendReviewFixture(t)
			original := data
			until := time.Now().UTC().Add(5 * time.Second)
			injected := growthWindowSendReviewPause(t, h, runner, until, func() {
				growthWindowSendReviewBlock(t, h, kind)
			})
			if _, err := runner.dispatch(h.ctx, mission, &data); err != nil {
				t.Fatalf("post-wait veto after new %s evidence: %v", kind, err)
			}
			if !*injected || time.Now().Before(until.Truncate(time.Second)) {
				t.Fatal("dispatch did not cross the real write-bucket pause after initial admission")
			}
			growthWindowSendReviewAssertVeto(t, h, mission, original, true)
		})
	}
}

func TestGrowthWindowSendReviewProofExpiresAcrossLimiterWait(t *testing.T) {
	h := newRecoveryHarness(t)
	runner := growthWindowRunner(t, h)
	// Establish the clock only after schema, account, HTTP and observation
	// setup finishes. No stored action or settlement timestamp is changed.
	completed := time.Now().UTC().Add(-12*time.Hour + 30*time.Second).Truncate(time.Microsecond)
	expires := completed.Add(12 * time.Hour)
	for i := range 2 {
		persistGrowthWindowSettlement(t, h, store.Settlement{
			ID: fmt.Sprintf("send-review-expiring-%d", i), Kind: "spot", CostsKnown: true,
			QuantityTons: 48, GameSeconds: 43_200, AcquisitionPence: 4_800, ProceedsPence: 30_000,
			ProvisionsPence: 208, ExpensesPence: 350, CompletedAt: completed,
		})
	}
	mission, data := growthWindowSendReviewAdmit(t, h, runner)
	original := data
	until := expires.Add(2 * time.Second)
	injected := growthWindowSendReviewPause(t, h, runner, until, func() {
		asOf := time.Now().UTC()
		history, err := h.db.GrowthHistory(h.ctx, h.account.ID, "", asOf)
		if err != nil || history.PositiveCycles != 2 || history.UnknownCosts || history.FutureCompletion || !asOf.Before(expires) {
			t.Fatalf("proof was not valid immediately before the limiter wait: %+v error %v", history, err)
		}
	})
	if _, err := runner.dispatch(h.ctx, mission, &data); err != nil {
		t.Fatalf("dispatch after rolling proof expiry: %v", err)
	}
	after, err := h.db.GrowthHistory(h.ctx, h.account.ID, "", time.Now().UTC())
	if err != nil || after.PositiveCycles != 0 || !*injected || time.Now().Before(until.Truncate(time.Second)) {
		t.Fatalf("proof did not genuinely expire during the bounded limiter wait: %+v error %v", after, err)
	}
	growthWindowSendReviewAssertVeto(t, h, mission, original, true)
}

func TestGrowthWindowSendReviewAttemptedBuildReplaysDespiteNewUncertainty(t *testing.T) {
	h, runner, mission, data := growthWindowSendReviewFixture(t)
	original := *data.Action
	downstream := h.api.server.Config.Handler
	h.api.server.Config.Handler = http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		if request.Method == http.MethodPost && request.URL.Path == "/v1/shipyard/build-ship" {
			body, err := io.ReadAll(request.Body)
			if err != nil {
				t.Error(err)
				writer.WriteHeader(http.StatusBadRequest)
				return
			}
			h.api.mu.Lock()
			h.api.record(request, body, http.StatusServiceUnavailable)
			h.api.mu.Unlock()
			h.api.writeProblem(writer, http.StatusServiceUnavailable, "internal_error", "synthetic ambiguous shipyard outage")
			return
		}
		downstream.ServeHTTP(writer, request)
	})
	if _, err := runner.dispatch(h.ctx, mission, &data); err != nil {
		t.Fatalf("retain genuinely attempted build: %v", err)
	}
	before := h.api.mutationCount("/v1/shipyard/build-ship")
	pending := h.pending()
	if before == 0 || len(pending) != 1 || pending[0].ID != original.ID || !bytes.Equal(pending[0].Body, original.Body) {
		t.Fatalf("fixture did not retain an actual immutable transport attempt: calls %d pending %+v", before, pending)
	}
	growthWindowSendReviewBlock(t, h, "unknown")
	h.expireCooldown("__growth")
	h.api.server.Config.Handler = downstream
	restarted := growthWindowRunner(t, h)
	evidence := growthEvidence{workingByRegion: make(map[string]int64)}
	if reason, err := restarted.growthFunding(h.ctx, &evidence); err != nil || reason == "" {
		t.Fatalf("new uncertainty did not veto fresh build proof: reason %q error %v", reason, err)
	}
	retained, replay := h.mission("__growth")
	if _, err := restarted.dispatch(h.ctx, retained, &replay); err != nil {
		t.Fatalf("replay attempted original build after restart: %v", err)
	}
	calls := h.api.mutationsFor("/v1/shipyard/build-ship")
	if len(calls) != before+1 || calls[len(calls)-1].Status != http.StatusOK {
		t.Fatalf("new accounting uncertainty abandoned the attempted build: %+v", calls)
	}
	for _, call := range calls {
		if call.IdempotencyKey != original.ID || !bytes.Equal(call.Body, original.Body) {
			t.Fatalf("attempted build replay replaced its immutable identity or payload: %+v", call)
		}
	}
	_, built := h.mission("__growth")
	if len(h.pending()) != 0 || built.BuiltShipID != recoveryBuiltShipID || built.Action != nil {
		t.Fatalf("attempted build did not settle into owned-hull commissioning: %+v", built)
	}
}

func TestGrowthWindowSendReviewOwnedHullStillCommissions(t *testing.T) {
	w, runner := newGrowthExecutionWorld(t)
	built := false
	for range 20 {
		w.tick(runner)
		mission, data, err := runner.loadMission(w.h.ctx, "__growth")
		if err != nil {
			t.Fatalf("read build progress: %v", err)
		}
		if mission != nil && data.BuiltShipID == recoveryBuiltShipID {
			built = true
			break
		}
	}
	if !built || w.h.api.mutationCount("/v1/shipyard/build-ship") != 1 {
		t.Fatal("fixture did not buy exactly one authoritative hull")
	}
	growthWindowSendReviewBlock(t, w.h, "unknown")
	evidence := growthEvidence{workingByRegion: make(map[string]int64)}
	if reason, err := runner.growthFunding(w.h.ctx, &evidence); err != nil || reason == "" {
		t.Fatalf("new uncertainty did not block a second hull: reason %q error %v", reason, err)
	}
	// The real controller must still refit, provision and load the paid hull,
	// handing off to its ordinary delivery mission without renewed build proof.
	_, delivery := w.until(runner, func(mission *store.Mission, data missionData) bool {
		return mission != nil && data.Action != nil && data.Action.Stage == "depart"
	})
	original := *delivery.Action
	before := w.h.api.mutationCount("/v1/fleet/depart")
	for range 10 {
		w.tick(runner)
		if w.h.api.mutationCount("/v1/fleet/depart") > before {
			break
		}
	}
	calls := w.h.api.mutationsFor("/v1/fleet/depart")
	if len(calls) != before+1 || calls[len(calls)-1].IdempotencyKey != original.ID || !bytes.Equal(calls[len(calls)-1].Body, original.Body) {
		t.Fatalf("new accounting uncertainty stranded the paid hull before its admitted departure: %+v", calls)
	}
	if w.h.api.mutationCount("/v1/shipyard/build-ship") != 1 {
		t.Fatal("commissioning bought another hull after build proof became blocked")
	}
}
