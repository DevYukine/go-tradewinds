package engine

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/DevYukine/go-tradewinds/internal/store"
	"go.uber.org/zap"
	"go.uber.org/zap/zapcore"
	"go.uber.org/zap/zaptest/observer"
)

func TestDispatchLogsFreshCommitOnceButNotCompletedActionReplay(t *testing.T) {
	h := newRecoveryHarness(t)
	runner := h.runner(primaryTestKey)
	core, logs := observer.New(zap.DebugLevel)
	runner.log = zap.New(core)
	mission, data := stageLoggingSettings(t, h, runner, "fresh-settings")
	stagedMission, stagedData := h.mission(mission.ShipID)
	actionID := data.Action.ID

	changed, err := runner.dispatch(h.ctx, mission, &data)
	if err != nil || !changed {
		t.Fatalf("fresh settings dispatch: changed %v, error %v", changed, err)
	}
	persisted, persistedData := h.mission(mission.ShipID)
	if persisted.Phase != "done" || persistedData.Action != nil || persistedData.LastActionID != actionID {
		t.Fatalf("success log must describe a durable transition: phase %s, data %+v", persisted.Phase, persistedData)
	}
	commits := logs.FilterMessage("action committed").All()
	if len(commits) != 1 || commits[0].Level != zap.InfoLevel {
		t.Fatalf("fresh success should emit exactly one INFO commitment: %+v", commits)
	}
	fields := commits[0].ContextMap()
	if fields["action_id"] != actionID || fields["stage"] != "settings" || fields["reserve_pence"] != int64(234567) {
		t.Fatalf("commit did not identify the recorded action outcome: %+v", fields)
	}
	calls := len(h.api.mutationsFor("/v1/company/settings"))
	if calls != 1 {
		t.Fatalf("fresh logical action sent %d mutations, want one", calls)
	}

	// Reattach the pre-transition mission, as after a crash between the action
	// journal commit and mission consumption. The durable response is replayed.
	changed, err = runner.dispatch(h.ctx, stagedMission, &stagedData)
	if err != nil || !changed {
		t.Fatalf("consume completed journal action: changed %v, error %v", changed, err)
	}
	if len(h.api.mutationsFor("/v1/company/settings")) != calls || logs.FilterMessage("action committed").Len() != 1 {
		t.Fatal("completed-action recovery resent a mutation or duplicated its success announcement")
	}
}

func TestDispatchUninterpretableSuccessWarnsWithoutClaimingSuccess(t *testing.T) {
	h := newRecoveryHarness(t)
	interceptLoggingMutation(t, h, func(writer http.ResponseWriter, request *http.Request) {
		// The real fixture applies the mutation; only the returned outcome is
		// unusable. HTTP success alone cannot justify a success announcement.
		result := httptest.NewRecorder()
		h.api.handle(result, request)
		if result.Code != http.StatusOK {
			t.Errorf("fixture mutation failed: %d %s", result.Code, result.Body.String())
		}
		writer.Header().Set("Content-Type", "application/json")
		writer.WriteHeader(http.StatusOK)
		_, _ = writer.Write([]byte(`{}`))
	})
	runner := h.runner(primaryTestKey)
	core, logs := observer.New(zap.DebugLevel)
	runner.log = zap.New(core)
	mission, data := stageLoggingSettings(t, h, runner, "unknown-settings")
	actionID := data.Action.ID
	changed, err := runner.dispatch(h.ctx, mission, &data)
	if err != nil || changed {
		t.Fatalf("uninterpretable success advanced the mission: changed %v, error %v", changed, err)
	}
	_, persisted := h.mission(mission.ShipID)
	if persisted.Action == nil || persisted.Action.ID != actionID {
		t.Fatalf("uninterpretable commitment lost its reconciliation identity: %+v", persisted.Action)
	}
	if logs.FilterMessage("action committed").Len() != 0 {
		t.Fatal("uninterpretable HTTP success was announced as an interpreted commitment")
	}
	warnings := logs.FilterMessage("committed action requires authoritative reconciliation").All()
	if len(warnings) != 1 || warnings[0].Level != zap.WarnLevel || warnings[0].ContextMap()["action_id"] != actionID {
		t.Fatalf("missing action-specific reconciliation warning: %+v", warnings)
	}
}

func TestDispatchUnresolvedWarningDeduplicatesByActionIdentity(t *testing.T) {
	h := newRecoveryHarness(t)
	interceptLoggingMutation(t, h, func(writer http.ResponseWriter, request *http.Request) {
		// A definitive in-flight response is unresolved without transport retry
		// sleeps or a cancelled parent context that would skip durable handling.
		h.api.writeProblem(writer, http.StatusConflict, "idempotency_request_in_flight", "still processing this action")
	})
	runner := h.runner(primaryTestKey)
	core, logs := observer.New(zap.DebugLevel)
	runner.log = zap.New(core)
	mission, data := stageLoggingSettings(t, h, runner, "unresolved-first")
	firstID := data.Action.ID
	for range 2 {
		if changed, err := runner.dispatch(h.ctx, mission, &data); err != nil || changed {
			t.Fatalf("unresolved dispatch: changed %v, error %v", changed, err)
		}
		mission, data = h.mission(mission.ShipID)
		if data.Action == nil || data.Action.ID != firstID {
			t.Fatalf("unresolved replay changed the immutable identity: %+v", data.Action)
		}
	}
	warnings := logs.FilterMessage("action unresolved; retained immutable request for recovery").All()
	if len(warnings) != 1 || warnings[0].Level != zap.WarnLevel || warnings[0].ContextMap()["action_id"] != firstID {
		t.Fatalf("same unresolved action must warn once: %+v", warnings)
	}
	// Authoritative reconciliation closes the old intent before the same ship
	// obtains a genuinely new action. Identical hold prose must not hide it.
	if err := h.db.ReconcileAction(h.ctx, firstID, json.RawMessage(`{"observed":true}`)); err != nil {
		t.Fatal(err)
	}
	mission, data = stageLoggingSettings(t, h, runner, "unresolved-second")
	secondID := data.Action.ID
	if secondID == firstID {
		t.Fatal("new logical action reused the old identity")
	}
	if changed, err := runner.dispatch(h.ctx, mission, &data); err != nil || changed {
		t.Fatalf("new unresolved dispatch: changed %v, error %v", changed, err)
	}
	warnings = logs.FilterMessage("action unresolved; retained immutable request for recovery").All()
	if len(warnings) != 2 || warnings[1].ContextMap()["action_id"] != secondID || warnings[1].Level != zap.WarnLevel {
		t.Fatalf("new action identity did not receive its own warning: %+v", warnings)
	}
	if logs.FilterMessage("action committed").Len() != 0 {
		t.Fatal("unresolved actions emitted a success announcement")
	}
}

func TestDispatchCommittedInfoRedactsCredentialEchoFromSuccessfulResponse(t *testing.T) {
	h := newRecoveryHarness(t)
	const accountSecret = "synthetic passphrase, never used against the real game"
	const patternSecret = "tw_live_synthetic_response_credential"
	interceptLoggingMutation(t, h, func(writer http.ResponseWriter, request *http.Request) {
		result := httptest.NewRecorder()
		h.api.handle(result, request)
		var outcome map[string]any
		if result.Code != http.StatusOK || json.Unmarshal(result.Body.Bytes(), &outcome) != nil {
			t.Errorf("fixture build did not succeed: %d %s", result.Code, result.Body.String())
		}
		// The upstream operation is valid, but a returned identifier includes
		// both an exact account secret and a token not in stored credentials.
		outcome["ship_id"] = "built-" + accountSecret + "-" + patternSecret
		writer.Header().Set("Content-Type", "application/json")
		writer.WriteHeader(http.StatusOK)
		_ = json.NewEncoder(writer).Encode(outcome)
	})
	runner := h.runner(primaryTestKey)
	core, logs := observer.New(zap.DebugLevel)
	runner.log = zap.New(core)
	h.putMission("__growth", "credential-echo", "growth_build", missionData{BuildName: "Lapras", BuildClass: recoveryHull})
	mission, data := h.mission("__growth")
	if err := runner.stage(h.ctx, mission, &data, ShipState{}, "build", "/v1/shipyard/build-ship", map[string]any{
		"port_id": recoveryHomePort, "name": "Lapras", "ship_class": recoveryHull, "build_mode": "standard",
	}); err != nil {
		t.Fatal(err)
	}
	if changed, err := runner.dispatch(h.ctx, mission, &data); err != nil || !changed {
		t.Fatalf("valid credential-echo outcome did not commit: changed %v, error %v", changed, err)
	}
	commits := logs.FilterMessage("action committed").All()
	if len(commits) != 1 || commits[0].Level != zap.InfoLevel {
		t.Fatalf("expected a redacted success announcement, not suppression: %+v", commits)
	}
	if commits[0].ContextMap()["cost_pence"] != int64(60000) {
		t.Fatal("redaction removed the legitimate economic outcome")
	}
	for _, entry := range logs.All() {
		encoded, err := json.Marshal(entry.ContextMap())
		if err != nil {
			t.Fatal(err)
		}
		for _, secret := range []string{accountSecret, patternSecret, primaryTestKey} {
			if strings.Contains(entry.Message, secret) || strings.Contains(string(encoded), secret) {
				t.Fatalf("successful upstream response leaked a credential into %s output", entry.Level)
			}
		}
	}
}

func TestDispatchCommittedInfoFailsClosedWhenCredentialsBecomeUnreadable(t *testing.T) {
	h := newRecoveryHarness(t)
	runner := h.runner(primaryTestKey)
	const accountSecret = "synthetic passphrase, never used against the real game"
	const patternSecret = "tw_live_synthetic_unreadable_credentials"
	name := "Lapras " + accountSecret + " " + patternSecret
	h.putMission("__growth", "credentials-unreadable", "growth_build", missionData{BuildName: name, BuildClass: recoveryHull})
	mission, data := h.mission("__growth")
	if err := runner.stage(h.ctx, mission, &data, ShipState{}, "build", "/v1/shipyard/build-ship", map[string]any{
		"port_id": recoveryHomePort, "name": name, "ship_class": recoveryHull, "build_mode": "standard",
	}); err != nil {
		t.Fatal(err)
	}
	actionID := data.Action.ID
	core, logs := observer.New(zap.DebugLevel)
	credentialsUnavailable := false
	runner.log = zap.New(commitCredentialBarrierCore{
		Core: core,
		beforeCommit: func() {
			// The real upstream mutation, action completion and mission write
			// must all succeed before loss of credential access is introduced.
			persisted, outcome := h.mission("__growth")
			if persisted.Phase != "growth_refit" || outcome.Action != nil || outcome.LastActionID != actionID ||
				outcome.BuiltShipID != recoveryBuiltShipID || outcome.BuildCostPence != 60000 {
				t.Fatalf("credential failure was injected before durable success: phase %s, data %+v", persisted.Phase, outcome)
			}
			if err := h.db.Close(); err != nil {
				t.Fatal(err)
			}
			if _, err := h.db.Credentials(h.ctx, h.account.ID); err == nil {
				t.Fatal("closed store unexpectedly allowed credential access")
			}
			credentialsUnavailable = true
		},
	})
	if changed, err := runner.dispatch(h.ctx, mission, &data); err != nil || !changed {
		t.Fatalf("credential lookup failure turned a committed action into failure: changed %v, error %v", changed, err)
	}
	if !credentialsUnavailable || len(h.api.mutationsFor("/v1/shipyard/build-ship")) != 1 {
		t.Fatal("test did not reach the post-commit credential failure boundary")
	}
	commits := logs.FilterMessage("action committed").All()
	if len(commits) != 1 || commits[0].Level != zap.InfoLevel {
		t.Fatalf("credential failure suppressed or misclassified genuine success: %+v", commits)
	}
	fields := commits[0].ContextMap()
	if fields["action_id"] != actionID || fields["stage"] != "build" || fields["cost_pence"] != int64(60000) {
		t.Fatalf("fail-closed logging lost action identity or economic outcome: %+v", fields)
	}
	for _, field := range []string{"ship_id", "built_ship_id", "ship_name", "ship_class"} {
		if fields[field] != "[REDACTED]" {
			t.Fatalf("unreadable credentials did not redact upstream field %s: %+v", field, fields)
		}
	}
	if logs.FilterLevelExact(zap.WarnLevel).Len() != 0 || logs.FilterLevelExact(zap.ErrorLevel).Len() != 0 {
		t.Fatal("credential lookup failure produced a false action warning or failure")
	}
	for _, entry := range logs.All() {
		encoded, err := json.Marshal(entry.ContextMap())
		if err != nil {
			t.Fatal(err)
		}
		for _, secret := range []string{accountSecret, patternSecret, primaryTestKey} {
			if strings.Contains(entry.Message, secret) || strings.Contains(string(encoded), secret) {
				t.Fatalf("unavailable credentials exposed a raw secret in %s output", entry.Level)
			}
		}
	}
}

// Check is the boundary immediately before outcome strings are scrubbed.
// Observer still records the real entry; only database availability changes.
type commitCredentialBarrierCore struct {
	zapcore.Core
	beforeCommit func()
}

func (core commitCredentialBarrierCore) Check(entry zapcore.Entry, checked *zapcore.CheckedEntry) *zapcore.CheckedEntry {
	if entry.Message == "action committed" {
		core.beforeCommit()
	}
	return core.Core.Check(entry, checked)
}

func stageLoggingSettings(t *testing.T, h *recoveryHarness, runner *Runner, generation string) (*store.Mission, missionData) {
	t.Helper()
	h.putMission("__treasury", generation, "settings", missionData{})
	mission, data := h.mission("__treasury")
	if err := runner.stage(h.ctx, mission, &data, ShipState{}, "settings", "/v1/company/settings", map[string]any{
		"auto_remit_for_bills": true, "auto_remit_min_reserve_pence": 234567,
	}); err != nil {
		t.Fatal(err)
	}
	// Dispatch the durable mission representation on the first attempt too:
	// PostgreSQL JSONB normalizes embedded RawMessage whitespace when saved.
	// Every subsequent replay must use those exact same request bytes.
	return h.mission("__treasury")
}

func interceptLoggingMutation(t *testing.T, h *recoveryHarness, mutation http.HandlerFunc) {
	t.Helper()
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		if request.Method == http.MethodGet {
			h.api.handle(writer, request)
			return
		}
		mutation(writer, request)
	}))
	t.Cleanup(server.Close)
	// runner() still installs the fixture's real request observer and durable
	// store. Only the upstream success body or in-flight outcome is varied.
	h.api.server = server
}
