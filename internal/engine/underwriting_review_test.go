package engine

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"testing"
	"time"

	"github.com/DevYukine/go-tradewinds/internal/store"
	"github.com/google/uuid"
	"go.uber.org/zap"
	"go.uber.org/zap/zapcore"
)

func underwritingReviewRemember(t *testing.T, h *recoveryHarness, policy InsurancePolicy) {
	t.Helper()
	if err := h.db.PutUnderwritingPolicies(h.ctx, h.account.ID, []store.UnderwritingPolicyRecord{{
		AccountID: h.account.ID, PolicyID: policy.ID, TermsHash: insuranceTermsHash(policy),
		Status: policy.Status, Payload: encode(policy), SourceUpdatedAt: policy.UpdatedAt,
		ObservedAt: time.Now().UTC(), TermsChanged: policy.TermsChanged,
	}}); err != nil {
		t.Fatalf("retain policy evidence: %v", err)
	}
}

func underwritingReviewRefresh(t *testing.T, h *recoveryHarness, runner *Runner) {
	t.Helper()
	runner.generation = uuid.NewString()
	if err := runner.refresh(h.ctx); err != nil {
		t.Fatalf("refresh underwriting evidence: %v", err)
	}
}

func TestUnderwritingReviewCandidateCannotEscapeItsRetainedEvidence(t *testing.T) {
	t.Parallel()
	candidate := uwAccCandidate()
	if decision := EvaluateUnderwriting(candidate, uwAccSafeHistory(8), uwAccBudget()); !decision.Accepted {
		t.Fatalf("positive control is not admissible: %+v", decision)
	}
	cases := []struct {
		name   string
		change func(*InsurancePolicy)
	}{
		{"permanent censorship", func(policy *InsurancePolicy) { policy.TermsChanged = true }},
		{"changed then reverted terms", func(policy *InsurancePolicy) {
			policy.HullValuePence++
		}},
		{"safe settlement reopened", func(policy *InsurancePolicy) { policy.Status = "settled_safe" }},
		{"loss settlement reopened", func(policy *InsurancePolicy) { policy.Status = "claimed_loss" }},
		{"cancelled policy reopened", func(policy *InsurancePolicy) { policy.Status = "cancelled" }},
	}
	for _, test := range cases {
		t.Run(test.name, func(t *testing.T) {
			retained := candidate
			test.change(&retained)
			for _, observations := range [][]InsurancePolicy{{retained, candidate}, {candidate, retained}} {
				history := append(uwAccSafeHistory(8), observations...)
				if decision := EvaluateUnderwriting(candidate, history, uwAccBudget()); decision.Accepted {
					t.Fatalf("candidate escaped contradictory same-ID history: %+v", decision)
				}
			}
		})
	}

}

func TestUnderwritingReviewPermanentCensorshipSurvivesRestart(t *testing.T) {
	t.Parallel()
	for _, priorStatus := range []string{"terms_changed", "settled_safe", "claimed_loss", "cancelled"} {
		t.Run(priorStatus, func(t *testing.T) {
			h := newRecoveryHarness(t)
			base := time.Now().UTC()
			policy := uwExecPolicy(base)
			underwritingReviewRemember(t, h, policy)
			contradiction := policy
			contradiction.UpdatedAt = base.Add(-20 * time.Minute)
			if priorStatus == "terms_changed" {
				contradiction.HullValuePence++
			} else {
				contradiction.Status = priorStatus
			}
			underwritingReviewRemember(t, h, contradiction)
			policy.UpdatedAt = base.Add(-10 * time.Minute)
			underwritingReviewRemember(t, h, policy)
			proxy := installUWExecProxy(t, h)
			proxy.insuranceRows = append([]map[string]any{uwExecPolicyRow(policy)}, uwExecCohort(base)...)
			proxy.membership = map[string]bool{}
			runner := h.runner(primaryTestKey)
			underwritingReviewRefresh(t, h, runner)
			if changed, err := runner.considerUnderwriting(h.ctx); err != nil || changed {
				t.Fatalf("permanently censored policy was selected after restart: changed %v, error %v", changed, err)
			}
			risk, err := h.db.UnderwritingRisk(h.ctx, h.account.ID)
			if err != nil || risk.InitialPence != 0 {
				t.Fatalf("ineligible candidate seeded a new pilot: %+v, error %v", risk, err)
			}
			uwExecPutMission(t, h, base, nil)
			restarted := h.runner(primaryTestKey)
			uwExecTicks(t, h, restarted, 3)
			if attempts := uwExecAttempts(t, h); len(attempts) != 0 {
				t.Fatalf("restored censored intent reached POST: %+v", attempts)
			}
			uwExecRequireReleasedMission(t, h, "permanently censored intent")
			uwExecRequireNoPositions(t, h, "permanently censored intent")
			records, err := h.db.UnderwritingPolicies(h.ctx, h.account.ID)
			if err != nil {
				t.Fatal(err)
			}
			censored := false
			for _, record := range records {
				if record.PolicyID == policy.ID {
					censored = record.TermsChanged
				}
			}
			if !censored {
				t.Fatal("reverted board observation erased permanent censorship")
			}
		})
	}

}

func TestUnderwritingReviewPostWaitPreflightRejectsNewPermanentContradiction(t *testing.T) {
	t.Parallel()
	h := newRecoveryHarness(t)
	base := time.Now().UTC()
	proxy := installUWExecProxy(t, h)
	proxy.insuranceRows = uwExecBoardRows(base)
	proxy.membership = map[string]bool{}
	uwExecPutMission(t, h, base, nil)
	runner := h.runner(primaryTestKey)
	underwritingReviewRefresh(t, h, runner)
	mission, data := h.mission(uwExecMissionID)
	if changed, err := runner.driveUnderwriting(h.ctx, mission, &data); err != nil || !changed || data.Action == nil {
		t.Fatalf("stage positive-control underwriting: changed %v, error %v, action %+v", changed, err, data.Action)
	}
	original := *data.Action
	injected := false
	core := zapcore.NewCore(zapcore.NewJSONEncoder(zap.NewProductionEncoderConfig()), zapcore.AddSync(io.Discard), zapcore.DebugLevel)
	runner.log = zap.New(core, zap.Hooks(func(entry zapcore.Entry) error {
		if entry.Message != "dispatching durable action" || injected {
			return nil
		}
		injected = true
		changed := uwExecPolicy(base)
		changed.HullValuePence++
		changed.UpdatedAt = base.Add(-20 * time.Minute)
		underwritingReviewRemember(t, h, changed)
		reverted := uwExecPolicy(base)
		reverted.UpdatedAt = base.Add(-10 * time.Minute)
		underwritingReviewRemember(t, h, reverted)
		return nil
	}))
	if changed, err := runner.dispatch(h.ctx, mission, &data); err != nil || !changed {
		t.Fatalf("post-wait permanent-policy veto failed: changed %v, error %v", changed, err)
	}
	if !injected || len(uwExecAttempts(t, h)) != 0 {
		t.Fatal("contradiction after the initial preflight crossed the transport boundary")
	}
	stored, err := h.db.BeginAction(h.ctx, &store.Action{
		ID: original.ID, AccountID: h.account.ID, Method: original.Method, Path: original.Path, Body: original.Body,
	})
	if err != nil || stored.State != store.ActionReconciled || !bytes.Equal(stored.Body, original.Body) {
		t.Fatalf("never-sent action lost its exact journal identity: %+v, error %v", stored, err)
	}
	var evidence struct {
		Kind string `json:"kind"`
	}
	if err := json.Unmarshal(stored.Response, &evidence); err != nil || evidence.Kind != neverSentPrecondition {
		t.Fatalf("policy veto has no durable never-sent proof: %s, error %v", stored.Response, err)
	}
	if data.Action != nil || data.LastActionID != "" {
		t.Fatalf("never-sent policy veto was treated as a receipt: %+v", data)
	}

}

func TestUnderwritingReviewDisputedOwnedClosureVetoesFurtherRisk(t *testing.T) {
	t.Parallel()
	for _, contradiction := range []string{"reopened", "conflicting terminal", "changed terms"} {
		t.Run(contradiction, func(t *testing.T) {
			h := newRecoveryHarness(t)
			base := time.Now().UTC()
			proxy := installUWExecProxy(t, h)
			proxy.insuranceRows = uwExecBoardRows(base)
			proxy.membership = map[string]bool{uwExecPolicyID: true}
			id, _ := uwExecSeedPendingUnderwrite(t, h, base)
			row := uwExecRow(recoveryCompanyID, uwExecSliceBPS, uwExecCollateralCeiling, uwExecPremiumFloor+1)
			receipt := uwExecEnvelope(uwExecPolicy(base), []map[string]any{row})
			if err := h.db.CompleteAction(h.ctx, id, http.StatusOK, receipt, nil); err != nil {
				t.Fatal(err)
			}
			if _, err := h.db.EnsureUnderwritingBudget(h.ctx, h.account.ID, uwExecCollateralCeiling); err != nil {
				t.Fatal(err)
			}
			runner := h.runner(primaryTestKey)
			uwExecTicks(t, h, runner, 2)
			position := uwExecRequirePosition(t, h, id, uwExecCollateralCeiling, uwExecPremiumFloor+1, "owned receipt")
			closed := uwExecPolicy(base)
			closed.Status = "settled_safe"
			closed.UpdatedAt = time.Now().UTC()
			proxy.ledgerRows = []map[string]any{{
				"id": "owned-underwriting-debit", "debit_account_id": "vault-north",
				"amount_pence": position.CollateralPence, "metadata": map[string]any{"underwriting_id": position.ID},
				"created_at": position.AcceptedAt.Format(time.RFC3339Nano),
			}}
			proxy.insuranceRows = append([]map[string]any{uwExecPolicyRow(closed), uwExecPolicyRow(uwExecSecondPolicy(base))}, uwExecCohort(base)...)
			underwritingReviewRefresh(t, h, runner)
			positions, err := h.db.UnderwritingPositions(h.ctx, h.account.ID)
			if err != nil || len(positions) != 1 || positions[0].ClosedAt == nil || positions[0].NetPence == nil || *positions[0].NetPence != uwExecPremiumFloor+1 || len(positions[0].FundingEvidence) == 0 {
				t.Fatalf("positive-control closure lacks exact funded accounting: %+v, error %v", positions, err)
			}
			original := encode(positions[0])
			riskBefore, err := h.db.UnderwritingRisk(h.ctx, h.account.ID)
			if err != nil || riskBefore.RemainingPence <= uwExecCollateralCeiling {
				t.Fatalf("safe closure did not provide recycled pilot credit: %+v, error %v", riskBefore, err)
			}
			if changed, err := runner.considerUnderwriting(h.ctx); err != nil || !changed {
				t.Fatalf("undisputed funded closure did not allow the second policy: changed %v, error %v", changed, err)
			}
			mission, data := h.mission(uwExecMissionID)
			if data.Underwriting.Policy.ID != uwExecSecondPolicyID {
				t.Fatalf("positive control selected the wrong policy: %+v", data.Underwriting)
			}
			if changed, err := runner.driveUnderwriting(h.ctx, mission, &data); err != nil || !changed || data.Action == nil {
				t.Fatalf("stage next policy before dispute: changed %v, error %v", changed, err)
			}
			disputed := closed
			disputed.UpdatedAt = time.Now().UTC()
			switch contradiction {
			case "reopened":
				disputed.Status = "active"
			case "conflicting terminal":
				disputed.Status = "claimed_loss"
			case "changed terms":
				disputed.HullValuePence++
			}
			proxy.insuranceRows = append([]map[string]any{uwExecPolicyRow(disputed), uwExecPolicyRow(uwExecSecondPolicy(base))}, uwExecCohort(base)...)
			underwritingReviewRefresh(t, h, runner)
			if changed, err := runner.dispatch(h.ctx, mission, &data); err != nil || !changed {
				t.Fatalf("disputed owned settlement failed to veto the next first send: changed %v, error %v", changed, err)
			}
			if len(uwExecAttempts(t, h)) != 0 || data.Action != nil {
				t.Fatalf("recycled disputed pilot credit reached transport: %+v", data.Action)
			}
			restarted := h.runner(primaryTestKey)
			underwritingReviewRefresh(t, h, restarted)
			if _, _, err := restarted.underwritingEvidence(h.ctx); err == nil {
				t.Fatal("owned policy dispute vanished across restart")
			}
			if changed, err := restarted.considerUnderwriting(h.ctx); err != nil || changed {
				t.Fatalf("owned dispute selected more risk: changed %v, error %v", changed, err)
			}
			positions, err = h.db.UnderwritingPositions(h.ctx, h.account.ID)
			if err != nil || len(positions) != 1 || !bytes.Equal(encode(positions[0]), original) {
				t.Fatalf("dispute rewrote the immutable receipt or completed accounting: %+v, error %v", positions, err)
			}
			riskAfter, err := h.db.UnderwritingRisk(h.ctx, h.account.ID)
			if err != nil || riskAfter != riskBefore {
				t.Fatalf("dispute changed the permanent seed or accounting identities: before %+v, after %+v, error %v", riskBefore, riskAfter, err)
			}
		})
	}

}

func TestUnderwritingReviewReservesEveryRegionalDeficitAndMeasuredFee(t *testing.T) {
	t.Parallel()
	for _, test := range []struct {
		name          string
		feeBPS        int64
		afterDeficit  int64
		wantAdmission bool
	}{
		{"exact no-fee pilot", 0, 500_000, true},
		{"fee consumes pilot boundary", 1000, 500_000, false},
		{"fundable deficit permits risk", 1000, 600_000, true},
	} {
		t.Run(test.name, func(t *testing.T) {
			h := newRecoveryHarness(t)
			base := time.Now().UTC()
			proxy := installUWExecProxy(t, h)
			proxy.insuranceRows = uwExecBoardRows(base)
			proxy.membership = map[string]bool{}
			proxy.underwrite = func(writer http.ResponseWriter, request *http.Request, body []byte) {
				proxy.recordMutation(request, body, http.StatusOK)
				row := uwExecRow(recoveryCompanyID, uwExecSliceBPS, uwExecCollateralCeiling, uwExecPremiumFloor+1)
				uwExecWriteJSON(writer, http.StatusOK, uwExecEnvelope(uwExecPolicy(base), []map[string]any{row}))
			}
			runner := h.runner(primaryTestKey)
			underwritingReviewRefresh(t, h, runner)
			input, err := runner.committedPlanningInput(h.ctx, ShipState{})
			if err != nil {
				t.Fatal(err)
			}
			const smallRegionBalance int64 = 10_000
			shortfall := input.ReserveByRegion["south"] - smallRegionBalance
			if shortfall <= 0 {
				t.Fatal("fixture does not protect the sailing fleet's south-region arrival")
			}
			surplus := shortfall + test.afterDeficit
			h.api.setVaultBalance("north", input.ReserveByRegion["north"]+surplus)
			h.api.setVaultBalance("south", smallRegionBalance)
			h.api.setRates(Rate{FromRegion: "north", ToRegion: "south", FeeBPS: test.feeBPS})
			underwritingReviewRefresh(t, h, runner)
			regions, err := runner.underwritingRegions(uwExecPolicy(base))
			if err != nil || len(regions) != 1 || regions[0] != "north" {
				t.Fatalf("small protected region must be outside the possible debit set: %v, error %v", regions, err)
			}
			budget, err := runner.underwritingBudget(h.ctx, uwExecPolicy(base), false)
			if err != nil {
				t.Fatal(err)
			}
			gross := (shortfall*10_000 + (10_000 - test.feeBPS) - 1) / (10_000 - test.feeBPS)
			fee := gross * test.feeBPS / 10_000
			wantDiscretionary := test.afterDeficit - fee
			if budget.DiscretionaryPence != wantDiscretionary || budget.AvailableLocalPence != surplus-gross || budget.RemainingPilotPence != wantDiscretionary/10 {
				t.Fatalf("regional shortfall/fee disappeared before seeding: %+v, want discretionary %d, local %d, pilot %d", budget, wantDiscretionary, surplus-gross, wantDiscretionary/10)
			}
			if changed, err := runner.considerUnderwriting(h.ctx); err != nil || changed != test.wantAdmission {
				t.Fatalf("protected funding admission = %v, want %v, error %v", changed, test.wantAdmission, err)
			}
			if !test.wantAdmission {
				uwExecRequireReleasedMission(t, h, "protected regional fee consumes pilot capacity")
				return
			}
			mission, data := h.mission(uwExecMissionID)
			if changed, err := runner.driveUnderwriting(h.ctx, mission, &data); err != nil || !changed || data.Action == nil {
				t.Fatalf("stage fully funded policy: changed %v, error %v", changed, err)
			}
			if changed, err := runner.dispatch(h.ctx, mission, &data); err != nil || !changed {
				t.Fatalf("execute fully funded policy: changed %v, error %v", changed, err)
			}
			if len(uwExecAttempts(t, h)) != 1 {
				t.Fatal("one valid funded policy did not execute exactly once")
			}
			uwExecRequirePosition(t, h, data.LastActionID, uwExecCollateralCeiling, uwExecPremiumFloor+1, "fully funded policy")
			restarted := h.runner(primaryTestKey)
			uwExecTicks(t, h, restarted, 3)
			if len(uwExecAttempts(t, h)) != 1 {
				t.Fatal("restart sent another funded underwriting request")
			}
		})
	}

}

func TestUnderwritingReviewUnfundableCommitmentDoesNotBlockSafeShipping(t *testing.T) {
	t.Parallel()
	h := newRecoveryHarness(t)
	base := time.Now().UTC()
	proxy := installUWExecProxy(t, h)
	proxy.insuranceRows = uwExecBoardRows(base)
	proxy.contractRows = []map[string]any{freightContractRow("freight-a", "active", false)}
	h.api.setVaultBalance("south", 10_000)
	h.api.setRates()
	h.api.setFleet([]map[string]any{stationaryShipRow(recoveryShipBlastoise, "Blastoise", recoveryFarPort, "berthed", 24, 3)})
	deadline := base.Add(6 * time.Hour)
	h.putMission(recoveryShipBlastoise, "funding-review-freight", "arrival", missionData{
		Plan: Plan{Kind: "recovery", ShipID: recoveryShipBlastoise, Destination: recoveryFarPort},
		Freight: []freightObligation{{ContractID: "freight-a", Commodity: recoveryGrain, Quantity: 24,
			Destination: recoveryFarPort, Deadline: &deadline, RewardPence: 9_000, CollateralPence: 5_000}},
		Eligible: map[string]int64{},
	})
	runner := h.runner(primaryTestKey)
	underwritingReviewRefresh(t, h, runner)
	if changed, err := runner.considerUnderwriting(h.ctx); err != nil || changed {
		t.Fatalf("unfundable protected region admitted discretionary risk: changed %v, error %v", changed, err)
	}
	risk, err := h.db.UnderwritingRisk(h.ctx, h.account.ID)
	if err != nil || risk.InitialPence != 0 {
		t.Fatalf("unfundable commitment seeded the permanent pilot: %+v, error %v", risk, err)
	}
	uwExecTicks(t, h, runner, 2)
	if len(uwExecAttempts(t, h)) != 0 || h.api.mutationCount("/v1/contract/deliver-freight") != 1 {
		t.Fatalf("underwriting funding hold suppressed safe credit-only shipping: %+v", h.api.mutationsFor("/v1/contract/deliver-freight"))
	}
	_, data := h.mission(recoveryShipBlastoise)
	if len(data.Freight) != 1 || !data.Freight[0].Settled || data.ProceedsPence != 9_000 {
		t.Fatalf("safe shipping did not retain its authoritative delivery: %+v", data)
	}

}

func TestUnderwritingReviewInvalidReceiptIdentityRetainsCommittedAmbiguity(t *testing.T) {
	t.Parallel()
	for _, test := range []struct {
		name       string
		receiptID  string
		passphrase string
	}{
		{"non UUID receipt", "invalid-receipt-id", ""},
		{"credential-pattern receipt", "tw_live_receipt_identity_canary", ""},
		{"UUID-shaped account secret", uwExecOwnedRowID, uwExecOwnedRowID},
	} {
		t.Run(test.name, func(t *testing.T) {
			h := newRecoveryHarness(t)
			if test.passphrase != "" {
				if err := h.db.SaveAccount(h.ctx, h.account, store.Credentials{Handle: "Yukine", APIKey: primaryTestKey, Passphrase: test.passphrase}); err != nil {
					t.Fatal(err)
				}
			}
			base := time.Now().UTC()
			proxy := installUWExecProxy(t, h)
			proxy.insuranceRows = uwExecBoardRows(base)
			row := uwExecRow(recoveryCompanyID, uwExecSliceBPS, uwExecCollateralCeiling, uwExecPremiumFloor+1)
			row["id"] = test.receiptID
			receipt := uwExecEnvelope(uwExecPolicy(base), []map[string]any{row})
			proxy.underwrite = func(writer http.ResponseWriter, request *http.Request, body []byte) {
				proxy.recordMutation(request, body, http.StatusOK)
				writer.Header().Set("Content-Type", "application/json")
				writer.WriteHeader(http.StatusOK)
				_, _ = writer.Write(receipt)
			}
			id, body := uwExecSeedPendingUnderwrite(t, h, base)
			uwExecTicks(t, h, h.runner(primaryTestKey), 3)
			uwExecTicks(t, h, h.runner(primaryTestKey), 2)
			if len(uwExecAttempts(t, h)) != 1 {
				t.Fatalf("malformed committed receipt caused a new request: %+v", uwExecAttempts(t, h))
			}
			uwExecRequireSingleKey(t, h, id, body, test.name)
			uwExecRequireRetainedAction(t, h, id, body, test.name)
			uwExecRequireNoPositions(t, h, test.name)
			uwExecRequireHold(t, h, test.name)
			stored, err := h.db.BeginAction(h.ctx, &store.Action{ID: id, AccountID: h.account.ID, Method: http.MethodPost, Path: uwExecPath, Body: body})
			if err != nil || stored.State != store.ActionComplete || stored.Status != http.StatusOK || !bytes.Equal(stored.Response, receipt) {
				t.Fatalf("invalid receipt lost its exact encrypted committed evidence: %+v, error %v", stored, err)
			}
			snapshot, err := h.db.Snapshot(h.ctx, h.account.ID)
			if err != nil {
				t.Fatal(err)
			}
			pending := decode[underwritingPending](snapshot, "_underwriting_pending")
			if pending.CollateralPence != uwExecCollateralCeiling || len(pending.Regions) == 0 {
				t.Fatalf("invalid committed receipt released unresolved collateral: %+v", pending)
			}
		})
	}

}
