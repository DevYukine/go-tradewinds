package engine

import (
	"bytes"
	"context"
	"errors"
	"net/http"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/DevYukine/go-tradewinds/internal/store"
	"github.com/google/uuid"
)

// idleBulletinRunner pins the recovery fleet to one stationary roadstead hull
// with no shipyard port, so a full work pass reaches the public-posting tail
// without an unrelated trading mutation preempting it.
func idleBulletinRunner(t *testing.T, h *recoveryHarness, runner *Runner) {
	t.Helper()
	h.api.setFleet([]map[string]any{sourcingReviewShipRow()})
	refreshBulletinRunner(t, h, runner)
	runner.stateCache.Ships[0].Ship.Status = "anchored_roadstead"
	for i := range runner.stateCache.Catalog.Ports {
		runner.stateCache.Catalog.Ports[i].HasShipyard = false
	}
}

func bulletinDecisionReasons(t *testing.T, h *recoveryHarness) []string {
	t.Helper()
	rows, err := h.db.History(h.ctx, h.account.ID, "decisions", 500)
	if err != nil {
		t.Fatal(err)
	}
	decisions, ok := rows.([]store.Decision)
	if !ok {
		t.Fatalf("unexpected decision history type %T", rows)
	}
	reasons := make([]string, 0, len(decisions))
	for _, row := range decisions {
		if row.ShipID == "__bulletin" {
			reasons = append(reasons, row.Kind+" "+row.Reason)
		}
	}
	return reasons
}

func TestBulletinAuthorizationRefusalStopsExecutionAndRetainsOutcome(t *testing.T) {
	for _, test := range []struct {
		name   string
		status int
		code   string
	}{
		{"unauthorized", http.StatusUnauthorized, "invalid_api_key"},
		{"forbidden", http.StatusForbidden, "company_suspended"},
	} {
		t.Run(test.name, func(t *testing.T) {
			h, proxy, runner := newBulletinFixture(t)
			idleBulletinRunner(t, h, runner)
			_, data := stageBulletinFixture(t, h, runner)
			key := data.Action.ID
			proxy.postStatus, proxy.problemCode, proxy.problemDetail = test.status, test.code, "authorization refused this public write"
			changed, err := runner.work(h.ctx)
			if err == nil || !errors.Is(err, ErrExecutionStopped) {
				t.Fatalf("authorization refusal did not stop execution: changed=%t error=%v", changed, err)
			}
			if changed {
				t.Fatal("stopped pass reported progress")
			}
			if proxy.postCalls != 1 {
				t.Fatalf("authorization refusal did not send exactly one immutable post: %d", proxy.postCalls)
			}
			pending, err := h.db.PendingActions(h.ctx, h.account.ID)
			if err != nil || len(pending) != 0 {
				t.Fatalf("definitive refusal left an unresolved request: pending=%+v error=%v", pending, err)
			}
			actions, err := h.db.ActionsBetween(h.ctx, h.account.ID, time.Now().UTC().Add(-time.Hour), time.Now().UTC().Add(time.Minute))
			if err != nil {
				t.Fatal(err)
			}
			journaled := 0
			for _, action := range actions {
				if action.ID != key {
					continue
				}
				journaled++
				if action.State != store.ActionComplete || action.Status != test.status {
					t.Fatalf("refused post journal is not terminal with its refusal status: %+v", action)
				}
			}
			if journaled != 1 {
				t.Fatalf("refused post lost its durable journal row: %d", journaled)
			}
			retained, retainedData := h.mission("__bulletin")
			if retained.Phase == "done" || retainedData.Action != nil || retainedData.LastActionID != key || retainedData.Bulletin == nil {
				t.Fatalf("authorization refusal discarded the retained mission: phase=%s data=%+v", retained.Phase, retainedData)
			}
			rejected := false
			for _, reason := range bulletinDecisionReasons(t, h) {
				if strings.Contains(reason, "public posting deferred") {
					t.Fatalf("account stop was swallowed as an optional deferral: %q", reason)
				}
				rejected = rejected || strings.HasPrefix(reason, "action_rejected ")
			}
			if !rejected {
				t.Fatal("authorization refusal bypassed the durable rejection record")
			}
		})
	}
}

func TestBulletinOptionalRefusalsStayRecoverable(t *testing.T) {
	for _, test := range []struct {
		name   string
		status int
		code   string
	}{
		{"posting muted", http.StatusForbidden, "posting_muted"},
		{"bad request", http.StatusBadRequest, "invalid_payload"},
	} {
		t.Run(test.name, func(t *testing.T) {
			h, proxy, runner := newBulletinFixture(t)
			idleBulletinRunner(t, h, runner)
			_, data := stageBulletinFixture(t, h, runner)
			key := data.Action.ID
			proxy.postStatus, proxy.problemCode, proxy.problemDetail = test.status, test.code, "company posting muted for 2 hours"
			changed, err := runner.work(h.ctx)
			if err != nil || !changed {
				t.Fatalf("optional refusal escalated beyond the posting cooldown: changed=%t error=%v", changed, err)
			}
			if proxy.postCalls != 1 {
				t.Fatalf("optional refusal retried the immutable post: %d", proxy.postCalls)
			}
			state, err := runner.bulletinState()
			if err != nil || !state.CooldownUntil.After(time.Now()) || state.LastPostID != "" {
				t.Fatalf("optional refusal did not record a posting cooldown without a confirmed post: %+v error=%v", state, err)
			}
			pending, err := h.db.PendingActions(h.ctx, h.account.ID)
			if err != nil || len(pending) != 0 {
				t.Fatalf("optional refusal left an unresolved request: pending=%+v error=%v", pending, err)
			}
			retired, retiredData := h.mission("__bulletin")
			if retired.Phase != "done" || retiredData.Action != nil || retiredData.LastActionID != key {
				t.Fatalf("optional refusal did not retire its mission durably: phase=%s data=%+v", retired.Phase, retiredData)
			}
			for _, reason := range bulletinDecisionReasons(t, h) {
				if strings.HasPrefix(reason, "action_rejected ") {
					t.Fatalf("optional refusal escalated to an authorization stop: %q", reason)
				}
			}
		})
	}
}

// stalledBulletinAPI wraps the shared bulletin fixture so one bulletin POST
// response flushes its status headers and then stalls until the HTTP request
// context is canceled by the engine. The regressions below therefore exercise
// real delayed HTTP responses instead of fabricated APIError values.
type stalledBulletinAPI struct {
	next           http.Handler
	deliverBody    bool
	headersFlushed chan struct{}
}

func (api *stalledBulletinAPI) ServeHTTP(writer http.ResponseWriter, request *http.Request) {
	if request.Method != http.MethodPost || !bulletinPath(request.URL.Path) {
		api.next.ServeHTTP(writer, request)
		return
	}
	stalled := &stalledBulletinWriter{
		ResponseWriter: writer,
		done:           request.Context().Done(),
		deliverBody:    api.deliverBody,
		headersFlushed: api.headersFlushed,
	}
	api.next.ServeHTTP(stalled, request)
}

// stalledBulletinWriter flushes the response header as soon as the fixture
// writes it. With deliverBody the complete JSON problem body is still
// delivered before the stall, so the client observes a valid value followed by
// a missing EOF instead of a truncated one.
type stalledBulletinWriter struct {
	http.ResponseWriter
	done           <-chan struct{}
	deliverBody    bool
	headersFlushed chan struct{}
	headerSent     bool
}

func (writer *stalledBulletinWriter) WriteHeader(status int) {
	if writer.headerSent {
		return
	}
	writer.headerSent = true
	writer.ResponseWriter.WriteHeader(status)
	if flusher, ok := writer.ResponseWriter.(http.Flusher); ok {
		flusher.Flush()
	}
	if writer.headersFlushed != nil {
		select {
		case writer.headersFlushed <- struct{}{}:
		default:
		}
	}
}

func (writer *stalledBulletinWriter) Write(payload []byte) (int, error) {
	if !writer.headerSent {
		writer.WriteHeader(http.StatusOK)
	}
	if writer.deliverBody {
		if _, err := writer.ResponseWriter.Write(payload); err != nil {
			return 0, err
		}
		if flusher, ok := writer.ResponseWriter.(http.Flusher); ok {
			flusher.Flush()
		}
	}
	<-writer.done
	return len(payload), nil
}

func stallBulletinResponses(h *recoveryHarness, deliverBody bool) chan struct{} {
	flushed := make(chan struct{}, 1)
	h.api.server.Config.Handler = &stalledBulletinAPI{next: h.api.server.Config.Handler, deliverBody: deliverBody, headersFlushed: flushed}
	return flushed
}

func TestBulletinAuthorizationStalledBodyStillStopsExecution(t *testing.T) {
	for _, test := range []struct {
		name   string
		status int
		code   string
	}{
		{"unauthorized stalled body", http.StatusUnauthorized, "invalid_api_key"},
		{"forbidden stalled body", http.StatusForbidden, "company_suspended"},
	} {
		t.Run(test.name, func(t *testing.T) {
			h, proxy, runner := newBulletinFixture(t)
			idleBulletinRunner(t, h, runner)
			_, data := stageBulletinFixture(t, h, runner)
			key := data.Action.ID
			immutable := slices.Clone(data.Action.Body)
			proxy.postStatus, proxy.problemCode, proxy.problemDetail = test.status, test.code, "authorization refused this public write"
			stallBulletinResponses(h, false)
			started := time.Now()
			changed, err := runner.work(h.ctx)
			if elapsed := time.Since(started); elapsed+250*time.Millisecond < bulletinRequestBudget {
				t.Fatalf("refusal response did not stall until the request budget expired: %v", elapsed)
			}
			if err == nil || !errors.Is(err, ErrExecutionStopped) {
				t.Fatalf("stalled authorization refusal did not stop execution: changed=%t error=%v", changed, err)
			}
			if changed {
				t.Fatal("stopped pass reported progress")
			}
			if proxy.postCalls != 1 {
				t.Fatalf("stalled refusal did not send exactly one immutable post: %d", proxy.postCalls)
			}
			pending, err := h.db.PendingActions(h.ctx, h.account.ID)
			if err != nil || len(pending) != 0 {
				t.Fatalf("definitive refusal left an unresolved request after the network budget expired: pending=%+v error=%v", pending, err)
			}
			action := bulletinAuthorizationJournal(t, h, *data.Action)
			if action.State != store.ActionComplete || action.Status != test.status {
				t.Fatalf("stalled refusal journal is not terminal with its refusal status: %+v", action)
			}
			if !bytes.Equal(action.Body, immutable) || !bytes.Equal(proxy.bodies[key], immutable) {
				t.Fatalf("stalled refusal changed its authenticated request or transport bytes: journal=%s sent=%s", action.Body, proxy.bodies[key])
			}
			retained, retainedData := h.mission("__bulletin")
			if retained.Phase == "done" || retainedData.Action != nil || retainedData.LastActionID != key || retainedData.Bulletin == nil {
				t.Fatalf("stalled refusal discarded the retained mission: phase=%s data=%+v", retained.Phase, retainedData)
			}
			state, err := runner.bulletinState()
			if err != nil || state.CooldownUntil.After(time.Now()) || state.LastOutcomeID != "" {
				t.Fatalf("stalled refusal recorded an optional posting cooldown: %+v error=%v", state, err)
			}
			rejected := false
			for _, reason := range bulletinDecisionReasons(t, h) {
				if strings.Contains(reason, "public posting deferred") || strings.Contains(reason, "public posting skipped") ||
					strings.Contains(reason, "public post refused") || strings.Contains(reason, "remains unresolved") {
					t.Fatalf("account stop was swallowed as an optional outcome: %q", reason)
				}
				rejected = rejected || strings.HasPrefix(reason, "action_rejected ")
			}
			if !rejected {
				t.Fatal("stalled authorization refusal bypassed the durable rejection record")
			}
		})
	}
}

func TestBulletinPostingMutedStalledEOFRemainsOptional(t *testing.T) {
	h, proxy, runner := newBulletinFixture(t)
	idleBulletinRunner(t, h, runner)
	_, data := stageBulletinFixture(t, h, runner)
	key := data.Action.ID
	proxy.postStatus, proxy.problemCode, proxy.problemDetail = http.StatusForbidden, "posting_muted", "company posting is muted"
	proxy.retryAfter = "7200"
	stallBulletinResponses(h, true)
	started := time.Now()
	changed, err := runner.work(h.ctx)
	if elapsed := time.Since(started); elapsed+250*time.Millisecond < bulletinRequestBudget {
		t.Fatalf("muted response did not stall at EOF after its complete body: %v", elapsed)
	}
	if err != nil || changed {
		t.Fatalf("complete posting_muted body with a stalled EOF did not stay recoverable: changed=%t error=%v", changed, err)
	}
	if proxy.postCalls != 1 {
		t.Fatalf("muted stalled outcome retried the immutable post: %d", proxy.postCalls)
	}
	pending, err := h.db.PendingActions(h.ctx, h.account.ID)
	if err != nil || len(pending) != 1 || pending[0].ID != key {
		t.Fatalf("muted stalled outcome did not retain its original unresolved request: pending=%+v error=%v", pending, err)
	}
	retained, retainedData := h.mission("__bulletin")
	if retained.Phase == "done" || retainedData.Action == nil || retainedData.Action.ID != key {
		t.Fatalf("muted stalled outcome did not retain its mission for a keyed replay: phase=%s data=%+v", retained.Phase, retainedData)
	}
	state, err := runner.bulletinState()
	minimum := started.Add(2*time.Hour - time.Second)
	if err != nil || state.CooldownUntil.Before(minimum) || retainedData.CooldownUntil.Before(minimum) {
		t.Fatalf("muted stalled outcome lost its received Retry-After cooldown: state=%+v mission=%v error=%v", state, retainedData.CooldownUntil, err)
	}
	for _, reason := range bulletinDecisionReasons(t, h) {
		if strings.HasPrefix(reason, "action_rejected ") {
			t.Fatalf("posting_muted with a stalled EOF escalated to an authorization stop: %q", reason)
		}
	}
	// A company-only mute classification must never poison the shared buckets:
	// ordinary writes and reads stay available right after the stalled outcome.
	probeCtx, cancelProbe := context.WithTimeout(h.ctx, 6*time.Second)
	defer cancelProbe()
	if err := runner.api.DoWithPreflight(probeCtx, http.MethodPost, "/v1/ordinary-write-probe", map[string]any{"probe": true}, uuid.NewString(), nil, nil); errors.Is(err, context.DeadlineExceeded) || errors.Is(err, context.Canceled) {
		t.Fatalf("shared write bucket was poisoned by a company-only muted classification: %v", err)
	}
	readCtx, cancelRead := context.WithTimeout(h.ctx, 6*time.Second)
	defer cancelRead()
	var page bulletinPage
	if err := runner.api.GetOnce(readCtx, "/v1/bulletin?limit=100", &page); err != nil {
		t.Fatalf("ordinary board reads were unavailable after a company-only mute: %v", err)
	}
}

func TestBulletinParentCancellationAfterHeadersIsNeverDetached(t *testing.T) {
	h, proxy, runner := newBulletinFixture(t)
	idleBulletinRunner(t, h, runner)
	mission, data := stageBulletinFixture(t, h, runner)
	key := data.Action.ID
	proxy.postStatus, proxy.problemCode, proxy.problemDetail = http.StatusUnauthorized, "invalid_api_key", "authorization refused this public write"
	flushed := stallBulletinResponses(h, false)
	parent, cancel := context.WithCancel(context.Background())
	defer cancel()
	type bulletinOutcome struct {
		changed bool
		err     error
	}
	results := make(chan bulletinOutcome, 1)
	go func() {
		changed, err := runner.driveBulletin(parent, mission, &data)
		results <- bulletinOutcome{changed: changed, err: err}
	}()
	select {
	case <-flushed:
	case <-time.After(10 * time.Second):
		t.Fatal("stalled response never flushed its status headers")
	}
	cancel()
	select {
	case result := <-results:
		if result.err == nil || !errors.Is(result.err, context.Canceled) {
			t.Fatalf("driveBulletin did not respect parent cancellation: changed=%t error=%v", result.changed, result.err)
		}
		if errors.Is(result.err, ErrExecutionStopped) {
			t.Fatalf("parent cancellation detached into an authorization stop: %v", result.err)
		}
		if result.changed {
			t.Fatal("canceled pass reported progress")
		}
	case <-time.After(10 * time.Second):
		t.Fatal("driveBulletin kept working after its parent context was canceled")
	}
	pending, err := h.db.PendingActions(h.ctx, h.account.ID)
	if err != nil || len(pending) != 1 || pending[0].ID != key || pending[0].State != store.ActionPending {
		t.Fatalf("canceled pass did not retain its unresolved request: pending=%+v error=%v", pending, err)
	}
	actions, err := h.db.ActionsBetween(h.ctx, h.account.ID, time.Now().UTC().Add(-time.Hour), time.Now().UTC().Add(time.Minute))
	if err != nil {
		t.Fatal(err)
	}
	for _, action := range actions {
		if action.ID == key && action.State != store.ActionPending {
			t.Fatalf("canceled pass detached a completion for its stalled request: %+v", action)
		}
	}
	retained, retainedData := h.mission("__bulletin")
	if retained.Phase == "done" || retainedData.Action == nil || retainedData.Action.ID != key {
		t.Fatalf("canceled pass mutated the retained mission: phase=%s data=%+v", retained.Phase, retainedData)
	}
	state, err := runner.bulletinState()
	if err != nil || state.CooldownUntil.After(time.Now()) {
		t.Fatalf("canceled pass recorded an optional cooldown: %+v error=%v", state, err)
	}
	for _, reason := range bulletinDecisionReasons(t, h) {
		if strings.HasPrefix(reason, "action_rejected ") || strings.Contains(reason, "public posting deferred") || strings.Contains(reason, "public posting skipped") {
			t.Fatalf("canceled pass detached persistence work: %q", reason)
		}
	}
}
