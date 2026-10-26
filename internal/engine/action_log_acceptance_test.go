package engine

// Failure-first acceptance contract for L2: confirmed dock and offshore
// outcomes announce once at INFO with safe actor and port context, completed
// action replay stays silent, a supported authoritative reconciliation is
// labelled as a recovery that omits unknown money, and the recovery's actor
// context redacts credentials and fails closed when credentials become
// unreadable exactly at the logging boundary.

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/google/uuid"
	"go.uber.org/zap"
	"go.uber.org/zap/zapcore"
	"go.uber.org/zap/zaptest/observer"
)

// actionLogIntercept routes every non-GET mutation through the given handler
// while GETs still hit the deterministic fixture.
func actionLogIntercept(t *testing.T, h *recoveryHarness, mutation http.HandlerFunc) {
	t.Helper()
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		if request.Method == http.MethodGet {
			h.api.handle(writer, request)
			return
		}
		mutation(writer, request)
	}))
	t.Cleanup(server.Close)
	h.api.server = server
}

func actionLogRefresh(t *testing.T, h *recoveryHarness, runner *Runner) {
	t.Helper()
	runner.generation = uuid.NewString()
	if err := runner.refresh(h.ctx); err != nil {
		t.Fatalf("capture fresh authoritative state: %v", err)
	}
}

// Check is the boundary immediately before recovery actor strings are
// scrubbed. The observer still records every entry; only database
// availability changes.
type recoveryCredentialBarrierCore struct {
	zapcore.Core
	beforeCommit func()
}

func (core recoveryCredentialBarrierCore) Check(entry zapcore.Entry, checked *zapcore.CheckedEntry) *zapcore.CheckedEntry {
	if entry.Message == "action recovered" {
		core.beforeCommit()
	}
	return core.Core.Check(entry, checked)
}

func TestActionLogDockAndOffshoreOutcomesAreInfoOnceWithSilentReplay(t *testing.T) {
	t.Parallel()
	for _, stage := range []string{"dock", "offshore"} {
		t.Run(stage, func(t *testing.T) {
			h := newRecoveryHarness(t)
			path := "/v1/fleet/" + stage
			var sends atomic.Int64
			actionLogIntercept(t, h, func(writer http.ResponseWriter, request *http.Request) {
				if request.URL.Path != path {
					t.Errorf("unexpected mutation %s %s", request.Method, request.URL.Path)
					h.api.writeProblem(writer, http.StatusConflict, "unexpected_mutation", request.URL.Path)
					return
				}
				if sends.Add(1) > 1 {
					t.Errorf("completed or reconciled %s action was resent", stage)
					return
				}
				writer.Header().Set("Content-Type", "application/json")
				writer.WriteHeader(http.StatusOK)
				_, _ = writer.Write([]byte(`{}`))
			})
			runner := h.runner(primaryTestKey)
			core, logs := observer.New(zap.DebugLevel)
			runner.log = zap.New(core)
			h.api.setFleet([]map[string]any{berthedLaprasRow()})
			actionLogRefresh(t, h, runner)
			ship, exists := runner.ship(recoveryShipLapras)
			if !exists {
				t.Fatal("fresh fleet lost the berthed maintenance ship")
			}
			if changed, err := runner.startMaintenance(h.ctx, ship, stage, path, map[string]any{"ship_id": ship.Ship.ID}); err != nil || !changed {
				t.Fatalf("stage %s: changed %t error %v", stage, changed, err)
			}
			mission, data := h.mission(recoveryShipLapras)
			replayMission, replayData := h.mission(recoveryShipLapras)
			actionID := data.Action.ID

			if changed, err := runner.dispatch(h.ctx, mission, &data); err != nil || !changed {
				t.Fatalf("fresh %s dispatch: changed %t error %v", stage, changed, err)
			}
			commits := logs.FilterMessage("action committed").All()
			if len(commits) != 1 || commits[0].Level != zap.InfoLevel {
				t.Fatalf("confirmed %s outcome must announce exactly once at INFO: %+v", stage, commits)
			}
			fields := acceptanceFields(t, commits[0], "action_id", "stage", "ship_id", "port_id")
			if fields["action_id"] != actionID || fields["stage"] != stage || fields["ship_id"] != recoveryShipLapras {
				t.Fatalf("commit lost its action identity: %+v", fields)
			}
			if fields["port_id"] != recoveryHomePort {
				t.Fatalf("%s commit must carry the safe port context: %+v", stage, fields)
			}
			_, persistedData := h.mission(recoveryShipLapras)
			if persistedData.Action != nil || persistedData.LastActionID != actionID {
				t.Fatalf("commit log must describe the durable action consumption: %+v", persistedData)
			}
			if sends.Load() != 1 {
				t.Fatalf("fresh logical action sent %d mutations, want one", sends.Load())
			}

			// Reattach the pre-transition mission, as after a crash between the
			// action journal commit and mission consumption: replay consumes the
			// completed action durably and stays silent.
			if changed, err := runner.dispatch(h.ctx, replayMission, &replayData); err != nil || !changed {
				t.Fatalf("replay completed %s action: changed %t error %v", stage, changed, err)
			}
			if commits := logs.FilterMessage("action committed").All(); len(commits) != 1 {
				t.Fatalf("completed-action replay duplicated or retracted its announcement: %+v", commits)
			}
			if sends.Load() != 1 {
				t.Fatalf("replay resent the immutable %s request", stage)
			}
		})
	}

}

func TestActionLogAuthoritativeRecoveryIsLabelledOnceWithoutInventingMoney(t *testing.T) {
	t.Parallel()
	h := newRecoveryHarness(t)
	var attempts atomic.Int64
	actionLogIntercept(t, h, func(writer http.ResponseWriter, request *http.Request) {
		if request.URL.Path != "/v1/fleet/offshore" {
			t.Errorf("unexpected mutation %s %s", request.Method, request.URL.Path)
			h.api.writeProblem(writer, http.StatusConflict, "unexpected_mutation", request.URL.Path)
			return
		}
		if attempts.Add(1) > 1 {
			t.Errorf("reconciled offshore action was resent")
			return
		}
		// The first attempt is lost in flight: no interpreted outcome exists.
		h.api.writeProblem(writer, http.StatusConflict, "idempotency_request_in_flight", "still processing this action")
	})
	runner := h.runner(primaryTestKey)
	core, logs := observer.New(zap.DebugLevel)
	runner.log = zap.New(core)

	h.api.setFleet([]map[string]any{berthedLaprasRow()})
	actionLogRefresh(t, h, runner)
	ship, exists := runner.ship(recoveryShipLapras)
	if !exists {
		t.Fatal("fresh fleet lost the berthed maintenance ship")
	}
	if changed, err := runner.startMaintenance(h.ctx, ship, "offshore", "/v1/fleet/offshore", map[string]any{"ship_id": ship.Ship.ID}); err != nil || !changed {
		t.Fatalf("stage offshore: changed %t error %v", changed, err)
	}
	mission, data := h.mission(recoveryShipLapras)
	actionID := data.Action.ID
	if changed, err := runner.dispatch(h.ctx, mission, &data); err != nil || changed {
		t.Fatalf("lost offshore outcome must stay unresolved: changed %t error %v", changed, err)
	}

	// Fresh authoritative state later confirms the offshore move even though
	// the request's own outcome was never interpreted, without any resend.
	roadstead := berthedLaprasRow()
	roadstead["ship"].(map[string]any)["status"] = "anchored_roadstead"
	h.api.setFleet([]map[string]any{roadstead})
	actionLogRefresh(t, h, runner)
	mission, data = h.mission(recoveryShipLapras)
	if changed, err := runner.dispatch(h.ctx, mission, &data); err != nil || !changed {
		t.Fatalf("authoritative offshore reconciliation: changed %t error %v", changed, err)
	}

	recoveries := logs.FilterMessage("action recovered").All()
	if len(recoveries) != 1 || recoveries[0].Level != zap.InfoLevel {
		t.Fatalf("a supported authoritative reconciliation must be labelled exactly once at INFO: %+v", recoveries)
	}
	fields := acceptanceFields(t, recoveries[0], "action_id", "stage", "ship_id", "ship")
	if fields["action_id"] != actionID || fields["stage"] != "offshore" || fields["ship_id"] != recoveryShipLapras {
		t.Fatalf("recovery lost its action identity: %+v", fields)
	}
	for key := range fields {
		if strings.HasSuffix(key, "_pence") {
			t.Fatalf("reconciliation must omit unknown monetary fields, found %s", key)
		}
	}
	persisted, persistedData := h.mission(recoveryShipLapras)
	if persisted.Phase != "maintenance_done" || persistedData.Action != nil || persistedData.LastActionID != actionID {
		t.Fatalf("recovery must describe the durable transition: phase %s data %+v", persisted.Phase, persistedData)
	}
	if logs.FilterMessage("action committed").Len() != 0 {
		t.Fatal("a reconciled recovery must not masquerade as a fresh commit")
	}

}

func TestActionLogRecoveryFailsClosedAtActorContextBoundary(t *testing.T) {
	t.Parallel()
	const accountSecret = "synthetic passphrase, never used against the real game"
	const patternSecret = "tw_live_synthetic_recovery_actor_credential"
	h := newRecoveryHarness(t)
	var attempts atomic.Int64
	actionLogIntercept(t, h, func(writer http.ResponseWriter, request *http.Request) {
		if attempts.Add(1) > 1 {
			t.Errorf("reconciled offshore action was resent")
			return
		}
		h.api.writeProblem(writer, http.StatusConflict, "idempotency_request_in_flight", "still processing this action")
	})
	runner := h.runner(primaryTestKey)
	core, logs := observer.New(zap.DebugLevel)

	secretFleet := berthedLaprasRow()
	secretFleet["ship"].(map[string]any)["name"] = accountSecret + "-" + patternSecret
	h.api.setFleet([]map[string]any{secretFleet})
	actionLogRefresh(t, h, runner)
	ship, exists := runner.ship(recoveryShipLapras)
	if !exists {
		t.Fatal("fresh fleet lost the berthed maintenance ship")
	}
	if changed, err := runner.startMaintenance(h.ctx, ship, "offshore", "/v1/fleet/offshore", map[string]any{"ship_id": ship.Ship.ID}); err != nil || !changed {
		t.Fatalf("stage offshore: changed %t error %v", changed, err)
	}
	mission, data := h.mission(recoveryShipLapras)
	actionID := data.Action.ID
	if changed, err := runner.dispatch(h.ctx, mission, &data); err != nil || changed {
		t.Fatalf("lost offshore outcome must stay unresolved: changed %t error %v", changed, err)
	}

	roadstead := berthedLaprasRow()
	roadsteadShip := roadstead["ship"].(map[string]any)
	roadsteadShip["name"] = accountSecret + "-" + patternSecret
	roadsteadShip["status"] = "anchored_roadstead"
	h.api.setFleet([]map[string]any{roadstead})
	actionLogRefresh(t, h, runner)
	mission, data = h.mission(recoveryShipLapras)

	credentialsUnavailable := false
	runner.log = zap.New(recoveryCredentialBarrierCore{
		Core: core,
		beforeCommit: func() {
			// The authoritative reconciliation and its durable mission write
			// must both complete before loss of credential access is introduced.
			persisted, outcome := h.mission(recoveryShipLapras)
			if persisted.Phase != "maintenance_done" || outcome.Action != nil || outcome.LastActionID != actionID {
				t.Errorf("credential failure was injected before durable recovery: phase %s, data %+v", persisted.Phase, outcome)
			}
			if err := h.db.Close(); err != nil {
				t.Errorf("close store at the recovery boundary: %v", err)
			}
			if _, err := h.db.Credentials(h.ctx, h.account.ID); err == nil {
				t.Errorf("closed store unexpectedly allowed credential access")
			}
			credentialsUnavailable = true
		},
	})
	changed, err := runner.dispatch(h.ctx, mission, &data)
	if err != nil || !changed {
		t.Fatalf("credential lookup failure turned a recovered action into failure: changed %t error %v", changed, err)
	}
	if !credentialsUnavailable || attempts.Load() != 1 {
		t.Fatalf("test did not reach the post-recovery credential failure boundary: reached %t, attempts %d", credentialsUnavailable, attempts.Load())
	}
	recoveries := logs.FilterMessage("action recovered").All()
	if len(recoveries) != 1 || recoveries[0].Level != zap.InfoLevel {
		t.Fatalf("credential failure suppressed or misclassified the recovery: %+v", recoveries)
	}
	fields := recoveries[0].ContextMap()
	if fields["action_id"] != actionID || fields["stage"] != "offshore" {
		t.Fatalf("fail-closed logging lost the action identity: %+v", fields)
	}
	if fields["ship"] != "[REDACTED]" {
		t.Fatalf("unreadable credentials did not redact the recovery actor name: %+v", fields)
	}
	for _, entry := range logs.All() {
		encoded, err := json.Marshal(entry.ContextMap())
		if err != nil {
			t.Fatal(err)
		}
		for _, secret := range []string{accountSecret, patternSecret, primaryTestKey} {
			if strings.Contains(entry.Message, secret) || strings.Contains(string(encoded), secret) {
				t.Fatalf("recovery actor context leaked a credential into %s output", entry.Level)
			}
		}
	}

}
