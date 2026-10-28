package engine

import (
	"bytes"
	"context"
	"net/http"
	"testing"
	"time"

	"github.com/DevYukine/go-tradewinds/internal/store"
)

func capacitySavingsSpendMarker(runner *Runner) {
	runner.snapshot["_capacity_savings_target"] = encode(capacitySavingsTarget{
		PortID: recoveryHomePort, HullID: "savings-large-hull", RequiredPence: 1_000_000,
		CapitalPence: 500_000, DepthNetPencePerGameDay: 100_000,
		ExpiresAt: time.Now().UTC().Add(time.Hour),
	})
}

func TestCapacitySavingsOptionalSpendingWaits(t *testing.T) {
	t.Parallel()
	for _, entry := range []struct {
		name      string
		missionID string
		stage     string
		path      string
		setup     func(*testing.T, *recoveryHarness)
		consider  func(*Runner, context.Context) (bool, error)
		drive     func(*Runner, context.Context, *store.Mission, *missionData) (bool, error)
	}{
		{
			name: "expansion agent", missionID: "__treasury", stage: "hire-factor", path: "/v1/company/hire-factor",
			setup: func(t *testing.T, h *recoveryHarness) {
				newMeasuredExpansionFactorAPI(h)
			},
			consider: (*Runner).considerExpansionAgent,
		},
		{
			name: "underwriting", missionID: "__underwriting", stage: "underwrite", path: uwExecPath,
			setup: func(t *testing.T, h *recoveryHarness) {
				base := time.Now().UTC()
				underwritingReviewRemember(t, h, uwExecPolicy(base))
				proxy := installUWExecProxy(t, h)
				proxy.insuranceRows = uwExecBoardRows(base)
				proxy.membership = map[string]bool{}
				proxy.underwrite = func(writer http.ResponseWriter, request *http.Request, body []byte) {
					proxy.recordMutation(request, body, http.StatusOK)
					row := uwExecRow(recoveryCompanyID, uwExecSliceBPS, uwExecCollateralCeiling, uwExecPremiumFloor+1)
					uwExecWriteJSON(writer, http.StatusOK, uwExecEnvelope(uwExecPolicy(base), []map[string]any{row}))
				}
			},
			consider: (*Runner).considerUnderwriting, drive: (*Runner).driveUnderwriting,
		},
		{
			name: "marque", missionID: "__marque", stage: "marque", path: "/v1/fleet/issue-marque",
			setup: func(t *testing.T, h *recoveryHarness) {
				newMarqueExecutionAPI(h)
			},
			consider: (*Runner).considerMarque, drive: (*Runner).driveMarque,
		},
	} {
		t.Run(entry.name, func(t *testing.T) {
			for _, boundary := range []string{"admission", "first send"} {
				t.Run(boundary, func(t *testing.T) {
					for _, saving := range []bool{false, true} {
						t.Run(map[bool]string{false: "positive control", true: "live savings target"}[saving], func(t *testing.T) {
							h := newRecoveryHarness(t)
							entry.setup(t, h)
							runner := h.runner(primaryTestKey)
							underwritingReviewRefresh(t, h, runner)
							if saving && boundary == "admission" {
								capacitySavingsSpendMarker(runner)
							}
							changed, err := entry.consider(runner, h.ctx)
							blockedAdmission := saving && boundary == "admission"
							if err != nil || changed == blockedAdmission {
								t.Fatalf("consider optional spending: changed=%t blocked=%t err=%v", changed, blockedAdmission, err)
							}
							if blockedAdmission {
								h.missingMission(entry.missionID)
							} else {
								mission, data := h.mission(entry.missionID)
								if entry.drive != nil {
									if changed, err := entry.drive(runner, h.ctx, mission, &data); err != nil || !changed {
										t.Fatalf("stage admitted optional spending: changed=%t err=%v", changed, err)
									}
									mission, data = h.mission(entry.missionID)
								}
								if data.Action == nil || data.Action.ID == "" || data.Action.Stage != entry.stage || data.Action.Path != entry.path {
									t.Fatalf("admissible fixture did not stage a genuine optional action: %+v", data.Action)
								}
								if boundary == "first send" {
									if saving {
										capacitySavingsSpendMarker(runner)
									}
									if changed, err := runner.dispatch(h.ctx, mission, &data); err != nil || !changed {
										t.Fatalf("dispatch optional spending: changed=%t err=%v", changed, err)
									}
									_, restored := h.mission(entry.missionID)
									if restored.Action != nil || len(h.pending()) != 0 {
										t.Fatalf("first-send boundary retained an unresolved optional action: %+v", restored.Action)
									}
								}
							}
							wantCalls := 0
							if boundary == "first send" && !saving {
								wantCalls = 1
							}
							if calls := h.api.mutationCount(entry.path); calls != wantCalls {
								t.Fatalf("optional spending crossed transport %d times, want %d", calls, wantCalls)
							}
							if saving {
								found := false
								for _, decision := range uwExecDecisions(t, h) {
									found = found || decision.Kind == "hold" && decision.Reason == "capacity growth saves for a better hull"
								}
								if !found {
									t.Fatal("optional-spend guard did not record the exact capacity savings hold reason")
								}
							}
						})
					}
				})
			}
		})
	}
}

func TestCapacitySavingsAttemptedHireRetainsImmutableReplay(t *testing.T) {
	t.Parallel()
	h := newRecoveryHarness(t)
	proxy := newMeasuredExpansionFactorAPI(h)
	runner := h.runner(primaryTestKey)
	refreshExpansionFactor(t, h, runner)
	if changed, err := runner.considerExpansionAgent(h.ctx); err != nil || !changed {
		t.Fatalf("stage hire positive control: changed=%t err=%v", changed, err)
	}
	mission, data := h.mission("__treasury")
	if data.Action == nil || data.Action.Stage != "hire-factor" {
		t.Fatalf("fixture did not stage a genuine hire: %+v", data.Action)
	}
	key, body := data.Action.ID, bytes.Clone(data.Action.Body)
	proxy.status, proxy.code = http.StatusConflict, "idempotency_request_in_flight"
	if changed, err := runner.dispatch(h.ctx, mission, &data); err != nil || changed {
		t.Fatalf("attempt unresolved hire: changed=%t err=%v", changed, err)
	}
	calls := h.api.mutationsFor("/v1/company/hire-factor")
	if len(calls) != 1 || calls[0].IdempotencyKey != key || !bytes.Equal(calls[0].Body, body) {
		t.Fatalf("fixture did not attempt the original hire request: %+v", calls)
	}
	mission, data = h.mission("__treasury")
	if data.Action == nil || data.Action.ID != key || !bytes.Equal(data.Action.Body, body) {
		t.Fatalf("unresolved attempt lost its immutable action: %+v", data.Action)
	}
	data.CooldownUntil = time.Time{}
	if err := runner.saveMission(h.ctx, mission, &data); err != nil {
		t.Fatal(err)
	}
	capacitySavingsSpendMarker(runner)
	proxy.status, proxy.code = 0, ""
	if changed, err := runner.dispatch(h.ctx, mission, &data); err != nil || !changed {
		t.Fatalf("replay attempted hire despite new marker: changed=%t err=%v", changed, err)
	}
	calls = h.api.mutationsFor("/v1/company/hire-factor")
	if len(calls) != 2 || calls[1].IdempotencyKey != key || !bytes.Equal(calls[1].Body, body) {
		t.Fatalf("savings marker stopped or replaced an immutable attempted hire: %+v", calls)
	}
}
