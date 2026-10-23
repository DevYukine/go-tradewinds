package engine

// Behavioral regressions for the production Runner.Run loop (c9-orc-1).
// Unlike the recovery tests these start the real SSE stream and the real
// message loop: sustained ordinary notifications during every authoritative
// capture must never starve a staged settings PATCH, and a hard stream
// control must stop the runner before any mutation.
//
// The fake game API serializes every request behind one mutex, so the SSE
// endpoint is served by a wrapper installed around the handler: the stream
// connection never holds that mutex and each /v1/auth/whoami capture
// synchronously flushes one ordinary event frame mid-capture. Milestones are
// channel- and request-based; timeouts exist only as failure bounds.

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/DevYukine/go-tradewinds/internal/store"
)

const (
	progressOrdinaryEvent = "market_tick"
	progressFrameData     = `{"origin":"runner_progress_test"}`
	//_gateCapture indexes the authoritative capture whose entry the test
	// blocks: capture one must already have dispatched before capture two
	// is allowed to start.
	progressGateCapture = 2
	progressEventIDBase = "ev-"
)

type progressFrame struct {
	id        string
	eventType string
	data      json.RawMessage
}

func progressEventID(captureIndex int32) string {
	return fmt.Sprintf("%s%d", progressEventIDBase, captureIndex+1)
}

// progressStream coordinates the real SSE endpoint with the REST captures.
type progressStream struct {
	connectedOnce sync.Once
	connected     chan struct{}

	captures      atomic.Int32
	whoamiEntered chan int32
	captureReady  chan int32
	disconnected  chan struct{}
	closedOnce    sync.Once
	gateFor       func(index int32) <-chan struct{}

	releaseOnce sync.Once
	release     chan struct{}

	produce chan progressFrame
	emitted chan progressFrame

	// frameFor picks the frame each capture flushes mid-capture; nil means
	// one ordinary event per capture.
	frameFor func(index int32) progressFrame
}

// interceptEventStream wraps the fake API handler: /v1/event/sse is served
// outside the fake's whole-request mutex, and every /v1/auth/whoami capture
// flushes one ordinary event frame before its response so the notification
// lands during that authoritative capture. gateFromCapture gates every
// capture from that 1-based index on the release channel; 0 disables gating.
func interceptEventStream(t *testing.T, api *fakeGameAPI, initial []progressFrame, gateFromCapture int) *progressStream {
	t.Helper()
	stream := &progressStream{
		connected:     make(chan struct{}),
		whoamiEntered: make(chan int32, 64),
		captureReady:  make(chan int32, 64),
		disconnected:  make(chan struct{}),
		release:       make(chan struct{}),
		produce:       make(chan progressFrame),
		emitted:       make(chan progressFrame),
	}
	downstream := api.server.Config.Handler
	api.server.Config.Handler = http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		switch {
		case request.URL.Path == "/v1/event/sse":
			stream.serveStream(writer, request, initial)
		case request.Method == http.MethodGet && request.URL.Path == "/v1/auth/whoami":
			stream.serveWhoami(writer, request, downstream, gateFromCapture)
		default:
			downstream.ServeHTTP(writer, request)
		}
	})
	t.Cleanup(stream.releaseGates)
	return stream
}

func (s *progressStream) releaseGates() {
	s.releaseOnce.Do(func() { close(s.release) })
}

func (s *progressStream) writeFrame(writer http.ResponseWriter, flusher http.Flusher, frame progressFrame) bool {
	data := frame.data
	if len(data) == 0 {
		data = json.RawMessage(progressFrameData)
	}
	if _, err := fmt.Fprintf(writer, "id: %s\nevent: %s\ndata: %s\n\n", frame.id, frame.eventType, data); err != nil {
		return false
	}
	flusher.Flush()
	return true
}

func (s *progressStream) serveStream(writer http.ResponseWriter, request *http.Request, initial []progressFrame) {
	writer.Header().Set("Content-Type", "text/event-stream")
	writer.WriteHeader(http.StatusOK)
	flusher, ok := writer.(http.Flusher)
	if !ok {
		return
	}
	for _, frame := range initial {
		if !s.writeFrame(writer, flusher, frame) {
			return
		}
	}
	s.connectedOnce.Do(func() { close(s.connected) })
	for {
		select {
		case <-request.Context().Done():
			s.closedOnce.Do(func() { close(s.disconnected) })
			return
		case frame := <-s.produce:
			if !s.writeFrame(writer, flusher, frame) {
				return
			}
			select {
			case s.emitted <- frame:
			case <-request.Context().Done():
				return
			}
		}
	}
}

func (s *progressStream) serveWhoami(writer http.ResponseWriter, request *http.Request, downstream http.Handler, gateFromCapture int) {
	index := s.captures.Add(1)
	select {
	case s.whoamiEntered <- index:
	case <-request.Context().Done():
		return
	}
	// Run subscribes before its first capture; wait for the live stream so
	// the frame is guaranteed to be flushed during this capture.
	select {
	case <-s.connected:
	case <-request.Context().Done():
		return
	}
	frame := progressFrame{id: progressEventID(index), eventType: progressOrdinaryEvent}
	if s.frameFor != nil {
		frame = s.frameFor(index)
	}
	if frame.eventType != "" && !s.emit(request.Context(), frame) {
		return
	}
	select {
	case s.captureReady <- index:
	case <-request.Context().Done():
		return
	}
	if s.gateFor != nil {
		select {
		case <-s.gateFor(index):
		case <-request.Context().Done():
			return
		}
	}
	if gateFromCapture > 0 && int(index) >= gateFromCapture {
		select {
		case <-s.release:
		case <-request.Context().Done():
			return
		}
	}
	downstream.ServeHTTP(writer, request)
}

func (s *progressStream) emit(ctx context.Context, frame progressFrame) bool {
	select {
	case s.produce <- frame:
		select {
		case <-s.emitted:
			return true
		case <-ctx.Done():
			return false
		}
	case <-ctx.Done():
		return false
	}
}

func (s *progressStream) waitReady(t *testing.T, index int32) {
	t.Helper()
	deadline := time.After(30 * time.Second)
	for {
		select {
		case ready := <-s.captureReady:
			if ready >= index {
				return
			}
		case <-deadline:
			t.Fatalf("capture %d never reached its REST gate", index)
		}
	}
}

func (s *progressStream) waitCapture(t *testing.T, index int32) {
	t.Helper()
	deadline := time.After(30 * time.Second)
	for {
		select {
		case entered := <-s.whoamiEntered:
			if entered >= index {
				return
			}
			// An earlier capture's entry; keep waiting for the gated one.
		case <-deadline:
			t.Fatalf("runner never entered capture %d of /v1/auth/whoami", index)
		}
	}
}

// waitEventCursor bounds a durable-state milestone: the runner must have
// consumed and recorded the delivered events in frame order.
func waitEventCursor(t *testing.T, db *store.Store, ctx context.Context, want string) {
	t.Helper()
	deadline := time.After(30 * time.Second)
	for {
		cursor, err := db.Cursor(ctx, recoveryAccountID)
		if err != nil {
			t.Fatalf("load event cursor: %v", err)
		}
		if cursor == want {
			return
		}
		select {
		case <-deadline:
			t.Fatalf("event cursor stuck at %q, want recorded event %q", cursor, want)
		case <-time.After(25 * time.Millisecond):
		}
	}
}

// runningRunner joins runner.Run exactly once, either from the test body or
// from cleanup, so a failing test can never leak the runner goroutine.
type runningRunner struct {
	done     chan error
	cancel   context.CancelFunc
	joinOnce sync.Once
}

func startRunner(t *testing.T, runner *Runner, parent context.Context) *runningRunner {
	t.Helper()
	runCtx, cancel := context.WithCancel(parent)
	running := &runningRunner{done: make(chan error, 1), cancel: cancel}
	go func() {
		running.done <- runner.Run(runCtx)
	}()
	t.Cleanup(func() {
		running.cancel()
		running.joinOnce.Do(func() {
			select {
			case <-running.done:
			case <-time.After(15 * time.Second):
				t.Error("runner.Run did not return after cancellation")
			}
		})
	})
	return running
}

func (r *runningRunner) wait(t *testing.T, timeout time.Duration) error {
	t.Helper()
	var err error
	r.joinOnce.Do(func() {
		select {
		case err = <-r.done:
		case <-time.After(timeout):
			t.Fatalf("runner.Run did not return within %s", timeout)
		}
	})
	return err
}

// The c9-orc-1 regression: every authoritative capture ends with an ordinary
// notification queued mid-capture, so the loop must still grant capture one
// its work opportunity. Before the bounded-drain fix, capture one reaches
// capture two with zero settings PATCHes because len(messages)>0 vetoes every
// capture.
func TestRunDispatchesStagedSettingsDespiteSustainedOrdinaryEvents(t *testing.T) {
	h := newRecoveryHarness(t)
	runner := h.runner(primaryTestKey)
	h.api.setAutoRemit(false, 0)
	if _, err := h.tick(runner); err != nil {
		t.Fatalf("stage treasury settings intent: %v", err)
	}
	mission, data := h.mission("__treasury")
	if mission.Phase != "settings" || data.Action == nil || data.Action.Stage != "settings" {
		t.Fatalf("unexpected staged intent: phase %s action %+v", mission.Phase, data.Action)
	}
	staged := *data.Action
	if h.api.mutationCount("/v1/company/settings") != 0 {
		t.Fatal("staging must not send the settings request")
	}

	stream := interceptEventStream(t, h.api, []progressFrame{
		{id: progressEventID(0), eventType: progressOrdinaryEvent},
	}, progressGateCapture)
	running := startRunner(t, runner, h.ctx)

	stream.waitCapture(t, progressGateCapture)

	calls := h.api.mutationsFor("/v1/company/settings")
	if len(calls) != 1 {
		t.Fatalf("capture one dispatched %d settings PATCHes under sustained ordinary events, want exactly 1", len(calls))
	}
	dispatched := calls[0]
	if dispatched.Status != http.StatusOK || dispatched.IdempotencyKey != staged.ID ||
		dispatched.Method != http.MethodPatch || dispatched.Authorization != "Bearer "+primaryTestKey {
		t.Fatalf("capture one replayed the wrong journal request: %+v want id %s", dispatched, staged.ID)
	}
	var sent, want struct {
		AutoRemit  bool  `json:"auto_remit_for_bills"`
		MinReserve int64 `json:"auto_remit_min_reserve_pence"`
	}
	if err := json.Unmarshal(dispatched.Body, &sent); err != nil {
		t.Fatalf("decode dispatched settings body: %v", err)
	}
	if err := json.Unmarshal(staged.Body, &want); err != nil {
		t.Fatalf("decode staged settings body: %v", err)
	}
	if sent.AutoRemit != want.AutoRemit || sent.MinReserve != want.MinReserve {
		t.Fatalf("dispatched settings body %+v diverged from the immutable journal request %+v", sent, want)
	}

	// The consumed notifications must be durably recorded in frame order.
	stream.releaseGates()
	waitEventCursor(t, h.db, h.ctx, progressEventID(progressGateCapture))

	running.cancel()
	if err := running.wait(t, 15*time.Second); err != nil && !errors.Is(err, context.Canceled) {
		t.Fatalf("runner stopped with an unexpected error: %v", err)
	}
	if h.api.mutationCount("/v1/company/settings") != 1 {
		t.Fatal("extra settings attempts appeared after the dispatched capture")
	}
}

// Hard-control safety: an auth_revoked frame delivered during the first
// authoritative capture (behind the connect-time ordinary event) must stop
// Run terminally before any mutation, and the staged intent stays durable
// for repair. The mid-capture arrival matters: a work opportunity granted
// after that capture must be vetoed by the pending hard control, not by
// scheduler luck.
func TestRunStopsTerminalOnStreamAuthRevokedWithoutMutations(t *testing.T) {
	h := newRecoveryHarness(t)
	runner := h.runner(primaryTestKey)
	h.api.setAutoRemit(false, 0)
	if _, err := h.tick(runner); err != nil {
		t.Fatalf("stage treasury settings intent: %v", err)
	}
	if _, data := h.mission("__treasury"); data.Action == nil {
		t.Fatal("staging lost the treasury settings intent")
	}

	stream := interceptEventStream(t, h.api, []progressFrame{
		{id: progressEventID(0), eventType: progressOrdinaryEvent},
	}, 0)
	stream.frameFor = func(index int32) progressFrame {
		if index == 1 {
			return progressFrame{id: progressEventID(index), eventType: "auth_revoked"}
		}
		return progressFrame{id: progressEventID(index), eventType: progressOrdinaryEvent}
	}
	running := startRunner(t, runner, h.ctx)

	if err := running.wait(t, 30*time.Second); err == nil {
		t.Fatal("stream auth_revoked must stop Run with a terminal error, got nil")
	}

	for _, path := range []string{
		"/v1/company/settings", "/v1/treasury/remit", "/v1/shipyard/build-ship",
		"/v1/market/place-order", "/v1/fleet/depart",
	} {
		if h.api.mutationCount(path) != 0 {
			t.Fatalf("mutation %s crossed a revoked credential", path)
		}
	}
	mission, data := h.mission("__treasury")
	if mission.Phase != "settings" || data.Action == nil {
		t.Fatalf("auth stop did not retain the staged intent: phase %s action %+v", mission.Phase, data.Action)
	}
}

func TestRunQueuedGapAndClosureRequireAuthoritativeResyncBeforeDispatch(t *testing.T) {
	h := newRecoveryHarness(t)
	h.api.setAutoRemit(false, 0)
	runner := h.runner(primaryTestKey)
	if _, err := h.tick(runner); err != nil {
		t.Fatalf("stage settings before stream gap: %v", err)
	}
	_, staged := h.mission("__treasury")
	if staged.Action == nil {
		t.Fatal("settings control has no durable action")
	}
	stagedID := staged.Action.ID
	first, second, third := make(chan struct{}), make(chan struct{}), make(chan struct{})
	stream := interceptEventStream(t, h.api, nil, 0)
	stream.frameFor = func(index int32) progressFrame {
		if index == 1 {
			return progressFrame{id: progressEventID(index), eventType: progressOrdinaryEvent}
		}
		return progressFrame{}
	}
	stream.gateFor = func(index int32) <-chan struct{} {
		switch index {
		case 1:
			return first
		case 2:
			return second
		default:
			return third
		}
	}
	running := startRunner(t, runner, h.ctx)
	stream.waitReady(t, 1)
	emissionContext, cancelEmission := context.WithTimeout(h.ctx, 30*time.Second)
	defer cancelEmission()
	// One ordinary frame is already queued by capture one. These 254 plus
	// gap fill the 256-message queue; the resulting stream closure must
	// publish its barrier even before there is room to enqueue the closure.
	for index := range 254 {
		if !stream.emit(emissionContext, progressFrame{
			id: fmt.Sprintf("queued-before-gap-%d", index), eventType: progressOrdinaryEvent,
		}) {
			t.Fatal("ordinary event burst did not reach the blocked capture")
		}
	}
	if !stream.emit(emissionContext, progressFrame{
		id: "queued-gap", eventType: "gap",
		data: json.RawMessage(`{"requested_after_id":"expired-cursor"}`),
	}) {
		t.Fatal("gap did not follow the queued ordinary events")
	}
	select {
	case <-stream.disconnected:
		// Client cancellation proves the gap callback ran while REST was
		// still gated, rather than relying on server flush timing.
	case <-time.After(30 * time.Second):
		t.Fatal("queued gap was not consumed before releasing the stale capture")
	}
	close(first)
	stream.waitReady(t, 2)
	if h.api.mutationCount("/v1/company/settings") != 0 {
		t.Fatal("settings dispatch crossed queued gap/closure before a new authoritative capture")
	}
	cursor, err := h.db.Cursor(h.ctx, h.account.ID)
	if err != nil || cursor != "0" {
		t.Fatalf("gap did not durably reset the rejected cursor: cursor %q error %v", cursor, err)
	}
	_, retained := h.mission("__treasury")
	if retained.Action == nil || retained.Action.ID != stagedID {
		t.Fatalf("barrier lost or replaced the staged immutable request: %+v", retained.Action)
	}

	close(second)
	stream.waitReady(t, 3)
	calls := h.api.mutationsFor("/v1/company/settings")
	if len(calls) != 1 || calls[0].Status != http.StatusOK || calls[0].IdempotencyKey != stagedID {
		t.Fatalf("authoritative resync did not release exactly the retained settings request: %+v", calls)
	}
	h.api.mu.Lock()
	enabled, reserve := h.api.autoRemit, h.api.minReserve
	h.api.mu.Unlock()
	if !enabled || reserve < runner.opts.ReservePence {
		t.Fatalf("resynchronized dispatch did not update actual company bill settings: enabled %t reserve %d", enabled, reserve)
	}
	running.cancel()
	if err := running.wait(t, 15*time.Second); err != nil && !errors.Is(err, context.Canceled) {
		t.Fatalf("stop resynchronized runner: %v", err)
	}
}
