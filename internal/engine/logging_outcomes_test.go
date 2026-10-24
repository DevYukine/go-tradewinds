package engine

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/DevYukine/go-tradewinds/internal/store"
	"github.com/google/uuid"
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
	mission, data := stageLoggingGrowth(t, h, runner)
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
	const accountSecret = "synthetic passphrase, never used against the real game"
	const patternSecret = "tw_live_synthetic_unreadable_credentials"
	const returnedShipID = "built-" + accountSecret + "-" + patternSecret
	interceptLoggingMutation(t, h, func(writer http.ResponseWriter, request *http.Request) {
		result := httptest.NewRecorder()
		h.api.handle(result, request)
		var outcome map[string]any
		if result.Code != http.StatusOK || json.Unmarshal(result.Body.Bytes(), &outcome) != nil {
			t.Errorf("fixture build did not succeed: %d %s", result.Code, result.Body.String())
		}
		outcome["ship_id"] = returnedShipID
		writer.Header().Set("Content-Type", "application/json")
		writer.WriteHeader(http.StatusOK)
		_ = json.NewEncoder(writer).Encode(outcome)
	})
	runner := h.runner(primaryTestKey)
	mission, data := stageLoggingGrowth(t, h, runner)
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
				outcome.BuiltShipID != returnedShipID || outcome.BuildCostPence != 60000 {
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

func refreshLoggingState(t *testing.T, h *recoveryHarness, runner *Runner) {
	t.Helper()
	runner.generation = uuid.NewString()
	if err := runner.refresh(h.ctx); err != nil {
		t.Fatalf("capture current first-send clock authority: %v", err)
	}
}

func stageLoggingSettings(t *testing.T, h *recoveryHarness, runner *Runner, generation string) (*store.Mission, missionData) {
	t.Helper()
	refreshLoggingState(t, h, runner)
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

func stageLoggingGrowth(t *testing.T, h *recoveryHarness, runner *Runner) (*store.Mission, missionData) {
	t.Helper()
	h.api.setVaultBalance("north", 500_000)
	h.api.setVaultBalance("south", 100_000)
	h.seedLaneProofs()
	if _, err := h.tick(runner); err != nil {
		t.Fatalf("stage funded growth with current quote and lane proof: %v", err)
	}
	mission, data := h.mission("__growth")
	if data.Action == nil || data.Action.Stage != "build" {
		t.Fatalf("expected a staged build, got %+v", data.Action)
	}
	return mission, data
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

func TestDispatchLogsTypedMutationFamiliesWithActualOutcomesAndSilentReplay(t *testing.T) {
	cases := []struct {
		stage    string
		path     string
		body     string
		response string
		expected map[string]any
	}{
		{
			stage: "buy", path: "/v1/market/place-order",
			body:     `{"side":"bid","quantity":4,"price_pence":110,"commodity_id":"grain"}`,
			response: `{"commodity_id":"grain","filled_quantity":3,"total_gross_pence":300,"customs_duty_pence":6}`,
			expected: map[string]any{"quantity_tons": int64(3), "gross_pence": int64(300), "customs_pence": int64(6), "side": "bid"},
		},
		{
			stage: "sell", path: "/v1/market/place-order",
			body:     `{"side":"ask","quantity":4,"price_pence":110,"commodity_id":"grain"}`,
			response: `{"commodity_id":"grain","filled_quantity":3,"total_gross_pence":345,"customs_duty_pence":0}`,
			expected: map[string]any{"quantity_tons": int64(3), "gross_pence": int64(345), "customs_pence": int64(0), "side": "ask"},
		},
		{
			stage: "provision", path: "/v1/market/place-order",
			body:     `{"side":"bid","quantity":4,"price_pence":110,"commodity_id":"provisions","into_stores":true}`,
			response: `{"commodity_id":"provisions","filled_quantity":0,"total_gross_pence":0,"customs_duty_pence":0}`,
			expected: map[string]any{"quantity_tons": int64(0), "gross_pence": int64(0), "customs_pence": int64(0)},
		},
		{
			stage: "accept-freight", path: "/v1/contract/accept-freight",
			body:     `{"contract_id":"freight-contract"}`,
			response: `{"contract_id":"freight-contract","commodity_id":"grain","quantity":3,"reward_pence":1290,"collateral_pence":200,"delivery_deadline":"2030-01-01T00:00:00Z"}`,
			expected: map[string]any{"contract_id": "freight-contract", "quantity_tons": int64(3), "reward_pence": int64(1290), "collateral_pence": int64(200)},
		},
		{
			stage: "deliver-freight", path: "/v1/contract/deliver-freight",
			body:     `{"contract_id":"freight-contract"}`,
			response: `{"contract_id":"freight-contract","commodity_id":"grain","delivered_quantity":3,"reward_paid_pence":1290}`,
			expected: map[string]any{"contract_id": "freight-contract", "quantity_tons": int64(3), "reward_pence": int64(1290)},
		},
		{
			stage: "deliver-charter", path: "/v1/contract/deliver-charter",
			body:     `{"contract_id":"charter-contract","quantity":4}`,
			response: `{"contract_id":"charter-contract","commodity_id":"grain","delivered_quantity":3,"reward_paid_pence":1380}`,
			expected: map[string]any{"contract_id": "charter-contract", "quantity_tons": int64(3), "reward_pence": int64(1380)},
		},
		{
			stage: "depart", path: "/v1/fleet/depart", body: `{}`,
			response: `{"berth_dues_pence":120,"berth_dues_debt_pence":0}`,
			expected: map[string]any{"berth_dues_pence": int64(120), "berth_dues_debt_pence": int64(0), "cargo_tons": int64(7)},
		},
		{
			stage: "dock", path: "/v1/fleet/dock", body: `{}`, response: `{}`,
			expected: map[string]any{},
		},
		{
			stage: "offshore", path: "/v1/fleet/offshore", body: `{}`,
			response: `{"berth_dues_pence":125,"berth_dues_debt_pence":0}`,
			expected: map[string]any{"berth_dues_pence": int64(125), "berth_dues_debt_pence": int64(0)},
		},
		{
			stage: "build", path: "/v1/shipyard/build-ship",
			body:     `{"name":"Pikachu","ship_class":"hoy","build_mode":"turnkey_cash"}`,
			response: `{"name":"Pikachu","ship_class":"hoy","total_cost_pence":60000}`,
			expected: map[string]any{"built_ship_id": recoveryBuiltShipID, "ship_name": "Pikachu", "ship_class": "hoy", "cost_pence": int64(60000)},
		},
		{
			stage: "refit", path: "/v1/shipyard/modify-ship",
			body: `{"target_cannons_count":4}`, response: `{}`,
			expected: map[string]any{"target_cannons_count": int64(4), "cost_known": false},
		},
		{
			stage: "settings", path: "/v1/company/settings",
			body:     `{"auto_remit_for_bills":true,"auto_remit_min_reserve_pence":234567}`,
			response: `{"auto_remit_for_bills":true,"auto_remit_min_reserve_pence":234567}`,
			expected: map[string]any{"auto_remit_for_bills": true, "reserve_pence": int64(234567)},
		},
		{
			stage: "remit", path: "/v1/treasury/remit",
			body:     `{"from_region":"north","to_region":"south","gross_amount_pence":1000}`,
			response: `{"gross_amount_pence":1000,"fee_pence":10,"net_credited_pence":990}`,
			expected: map[string]any{"from_region": "north", "to_region": "south", "gross_pence": int64(1000), "fee_pence": int64(10), "net_credited_pence": int64(990)},
		},
		{
			stage: "sourcing-remit", path: "/v1/treasury/remit",
			body:     `{"from_region":"north","to_region":"south","gross_amount_pence":1000}`,
			response: `{"gross_amount_pence":1000,"fee_pence":10,"net_credited_pence":990}`,
			expected: map[string]any{"from_region": "north", "to_region": "south", "gross_pence": int64(1000), "fee_pence": int64(10), "net_credited_pence": int64(990)},
		},
	}
	for _, test := range cases {
		t.Run(test.stage, func(t *testing.T) {
			h := newRecoveryHarness(t)
			var sends atomic.Int64
			interceptLoggingMutation(t, h, func(writer http.ResponseWriter, request *http.Request) {
				sends.Add(1)
				if request.URL.Path != test.path {
					t.Errorf("unexpected mutation %s", request.URL.Path)
				}
				var response map[string]any
				if err := json.Unmarshal([]byte(test.response), &response); err != nil {
					t.Errorf("invalid fixture outcome: %v", err)
					http.Error(writer, "invalid fixture outcome", http.StatusInternalServerError)
					return
				}
				switch test.stage {
				case "build":
					response["ship_id"] = recoveryBuiltShipID
				case "accept-freight":
					response["carrier_ship_id"] = recoveryShipLapras
					response["dest_port_id"] = recoveryFarPort
				case "depart":
					response["ship_id"] = recoveryShipLapras
					response["dest_port_id"] = recoveryFarPort
				}
				h.api.writeJSON(writer, http.StatusOK, response)
			})
			runner := h.runner(primaryTestKey)
			core, logs := observer.New(zap.DebugLevel)
			runner.log = zap.New(core)
			h.api.setFleet([]map[string]any{berthedLaprasRow()})
			refreshLoggingState(t, h, runner)
			ship, _ := runner.ship(recoveryShipLapras)
			ship.CargoManifest = map[string]int64{recoveryGrain: 7}
			data := missionData{
				Plan:      Plan{Kind: "spot", ShipID: recoveryShipLapras, Destination: recoveryFarPort, Commodity: recoveryGrain},
				Eligible:  map[string]int64{recoveryGrain: 7},
				BuildName: "Pikachu", BuildClass: "hoy", BuildPort: recoveryHomePort,
				CostsKnown: true,
			}
			if test.stage == "deliver-freight" {
				data.Freight = []freightObligation{{ContractID: "freight-contract", Commodity: recoveryGrain, Quantity: 3}}
			}
			shipID := recoveryShipLapras
			if test.stage == "build" {
				shipID, ship = "__growth", ShipState{}
			} else if test.stage == "settings" || test.stage == "remit" {
				shipID, ship = "__treasury", ShipState{}
			}
			h.putMission(shipID, uuid.NewString(), test.stage, data)
			mission, data := h.mission(shipID)
			var body map[string]any
			if err := json.Unmarshal([]byte(test.body), &body); err != nil {
				t.Fatal(err)
			}
			if ship.Ship.ID != "" {
				body["ship_id"] = ship.Ship.ID
			}
			if test.stage == "buy" || test.stage == "sell" || test.stage == "provision" || test.stage == "build" {
				body["port_id"] = recoveryHomePort
			}
			if test.stage == "depart" {
				body["dest_port_id"] = recoveryFarPort
			}
			if err := runner.stage(h.ctx, mission, &data, ship, test.stage, test.path, body); err != nil {
				t.Fatal(err)
			}
			mission, data = h.mission(shipID)
			replayMission, replayData := h.mission(shipID)
			action := data.Action
			// An existing pending journal is a supported send path. Economic
			// first-send admission is covered separately from outcome logging.
			if _, err := h.db.BeginAction(h.ctx, &store.Action{ID: action.ID, AccountID: h.account.ID, Method: action.Method, Path: action.Path, Body: action.Body}); err != nil {
				t.Fatal(err)
			}
			if changed, err := runner.dispatch(h.ctx, mission, &data); err != nil || !changed {
				t.Fatalf("typed outcome was not consumed: changed %t error %v", changed, err)
			}
			commits := logs.FilterMessage("action committed").All()
			if len(commits) != 1 || commits[0].Level != zap.InfoLevel {
				t.Fatalf("typed outcome did not announce once at INFO: %+v", commits)
			}
			fields := commits[0].ContextMap()
			if fields["action_id"] != action.ID || fields["stage"] != test.stage {
				t.Fatalf("outcome lost immutable action identity: %+v", fields)
			}
			if ship.Ship.ID != "" && (fields["ship_id"] != ship.Ship.ID || fields["ship"] != ship.Ship.Name || fields["port_id"] != recoveryHomePort) {
				t.Fatalf("outcome lost its authoritative actor and port: %+v", fields)
			}
			for key, want := range test.expected {
				if fields[key] != want {
					t.Errorf("%s = %v, want %v", key, fields[key], want)
				}
			}
			_, persisted := h.mission(shipID)
			if persisted.Action != nil || persisted.LastActionID != action.ID {
				t.Fatalf("announcement preceded durable consumption: %+v", persisted)
			}
			if changed, err := runner.dispatch(h.ctx, replayMission, &replayData); err != nil || !changed {
				t.Fatalf("completed replay: changed %t error %v", changed, err)
			}
			if sends.Load() != 1 || logs.FilterMessage("action committed").Len() != 1 {
				t.Fatal("completed replay repeated a mutation or outcome")
			}
		})
	}
}

func TestNamedBuildRecoveryLogsEverySupportedJournalPathWithoutInventingCost(t *testing.T) {
	for _, state := range []string{"pending_expiry", "committed_uninterpreted", "persisted_reconciliation"} {
		t.Run(state, func(t *testing.T) {
			h := newRecoveryHarness(t)
			runner := h.runner(primaryTestKey)
			mission, data := stageLoggingGrowth(t, h, runner)
			core, logs := observer.New(zap.DebugLevel)
			runner.log = zap.New(core)
			action := data.Action
			persisted, err := h.db.BeginAction(h.ctx, &store.Action{ID: action.ID, AccountID: h.account.ID, Method: action.Method, Path: action.Path, Body: action.Body})
			if err != nil {
				t.Fatal(err)
			}
			built := berthedLaprasRow()
			actor := built["ship"].(map[string]any)
			actor["id"], actor["name"], actor["ship_class"] = recoveryBuiltShipID, data.BuildName, data.BuildClass
			actor["current_port_id"] = data.BuildPort
			// startGrowth names the hull with the first free catalog name,
			// which is "Lapras" for this fixture fleet, so the untouched decoy
			// row must not keep that name: exactly one hull may carry the
			// durable build name for the authoritative name+class scan.
			decoy := berthedLaprasRow()
			decoy["ship"].(map[string]any)["name"] = "Charizard"
			h.api.setFleet([]map[string]any{decoy, built})
			refreshLoggingState(t, h, runner)
			observed, _ := runner.ship(recoveryBuiltShipID)
			switch state {
			case "committed_uninterpreted":
				err = h.db.CompleteAction(h.ctx, action.ID, http.StatusOK, json.RawMessage(`{"committed_uninterpreted":true}`), nil)
			case "persisted_reconciliation":
				err = h.db.ReconcileAction(h.ctx, action.ID, encode(map[string]any{"kind": "authoritative_named_build", "ship": observed}))
			}
			if err != nil {
				t.Fatal(err)
			}
			var changed bool
			if state == "pending_expiry" {
				changed, err = runner.reconcileExpired(h.ctx, mission, &data, persisted)
			} else {
				changed, err = runner.dispatch(h.ctx, mission, &data)
			}
			if err != nil || !changed {
				t.Fatalf("recover named build: changed %t error %v", changed, err)
			}
			recoveries := logs.FilterMessage("action recovered").All()
			if len(recoveries) != 1 || recoveries[0].Level != zap.InfoLevel {
				t.Fatalf("named build recovery missing: %+v", recoveries)
			}
			fields := recoveries[0].ContextMap()
			if fields["action_id"] != action.ID || fields["stage"] != "build" || fields["built_ship_id"] != recoveryBuiltShipID || fields["ship"] != data.BuildName || fields["port_id"] != data.BuildPort {
				t.Fatalf("recovery lost its observed identity: %+v", fields)
			}
			for key := range fields {
				if strings.HasSuffix(key, "_pence") {
					t.Fatalf("recovery invented a receipt field: %s", key)
				}
			}
			_, saved := h.mission("__growth")
			if saved.Action != nil || saved.LastActionID != action.ID || saved.BuiltShipID != recoveryBuiltShipID {
				t.Fatalf("recovery announcement preceded its durable transition: %+v", saved)
			}
			if len(h.api.mutationsFor("/v1/shipyard/build-ship")) != 0 || logs.FilterMessage("action committed").Len() != 0 {
				t.Fatal("recovery resent a build or claimed a fresh receipt")
			}
		})
	}
}

func TestRejectedActionPolicyRedactsBeforeBoundingAndKeepsReplaySilent(t *testing.T) {
	const accountSecret = "synthetic passphrase, never used against the real game"
	for _, test := range []struct {
		code   string
		status int
		level  zapcore.Level
	}{
		{code: "ship_not_berthed", status: http.StatusConflict, level: zap.DebugLevel},
		{code: "conflict", status: http.StatusConflict, level: zap.WarnLevel},
		{code: "unexpected_conflict", status: http.StatusConflict, level: zap.WarnLevel},
		{code: "rate_limit_exceeded", status: http.StatusTooManyRequests, level: zap.DebugLevel},
	} {
		t.Run(test.code, func(t *testing.T) {
			h := newRecoveryHarness(t)
			var sends atomic.Int64
			interceptLoggingMutation(t, h, func(writer http.ResponseWriter, request *http.Request) {
				sends.Add(1)
				writer.Header().Set("Retry-After", "0")
				h.api.writeProblem(writer, test.status, test.code, strings.Repeat("界", 500)+accountSecret+" tw_live_rejected_reason_secret")
			})
			runner := h.runner(primaryTestKey)
			core, logs := observer.New(zap.DebugLevel)
			runner.log = zap.New(core)
			h.api.setVaultBalance("north", 500_000)
			fleet := berthedLaprasRow()
			fleet["ship"].(map[string]any)["name"] = accountSecret + "-tw_live_rejected_actor_secret"
			fleet["stores"] = map[string]any{"provisions_tons": 1.05, "sea_days_remaining": 10.5}
			h.api.setFleet([]map[string]any{fleet})
			refreshLoggingState(t, h, runner)
			ship, _ := runner.ship(recoveryShipLapras)
			if changed, err := runner.maintain(h.ctx, ship, nil, nil); err != nil || !changed {
				t.Fatalf("admit real idle replenishment: changed %t error %v", changed, err)
			}
			mission, data := h.mission(recoveryShipLapras)
			for range 3 {
				if data.Action != nil {
					break
				}
				if changed, err := runner.driveMission(h.ctx, ship, mission, &data); err != nil || !changed {
					t.Fatalf("stage quoted idle replenishment: changed %t error %v", changed, err)
				}
			}
			if data.Action == nil || data.Action.Stage != "provision" {
				t.Fatalf("real store deficit did not stage a valid standalone provision order: %+v", data)
			}
			replayMission, replayData := h.mission(recoveryShipLapras)
			actionID := data.Action.ID
			if changed, err := runner.dispatch(h.ctx, mission, &data); err != nil || !changed {
				t.Fatalf("consume rejection: changed %t error %v", changed, err)
			}
			rejections := logs.FilterMessage("action rejected").All()
			if len(rejections) != 1 || rejections[0].Level != test.level {
				t.Fatalf("wrong audited rejection severity: %+v", rejections)
			}
			fields := rejections[0].ContextMap()
			reason, _ := fields["reason"].(string)
			if fields["action_id"] != actionID || fields["port_id"] != recoveryHomePort || !strings.Contains(reason, "[REDACTED]") || !strings.HasSuffix(reason, "...") {
				t.Fatalf("rejection lost safe context or bounded-before-redacting: %+v", fields)
			}
			if _, ok := fields["retry_at"].(time.Time); !ok {
				t.Fatalf("handled wait lost its recorded cooldown: %+v", fields)
			}
			for _, entry := range logs.All() {
				encoded, err := json.Marshal(entry.ContextMap())
				if err != nil {
					t.Fatal(err)
				}
				for _, secret := range []string{accountSecret, "tw_live_rejected_actor_secret", "tw_live_rejected_reason_secret", primaryTestKey} {
					if strings.Contains(string(encoded), secret) {
						t.Fatalf("rejection context exposed credential %q", secret)
					}
				}
			}
			if changed, err := runner.dispatch(h.ctx, replayMission, &replayData); err != nil || !changed {
				t.Fatalf("replay rejection: changed %t error %v", changed, err)
			}
			if sends.Load() != 1 || logs.FilterMessage("action rejected").Len() != 1 {
				t.Fatal("completed rejection replay repeated a POST or announcement")
			}
		})
	}
}
