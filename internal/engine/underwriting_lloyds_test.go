package engine

import (
	"encoding/json"
	"fmt"
	"reflect"
	"strings"
	"testing"
	"time"
)

func TestPricedLloydsUnderwriting(t *testing.T) {
	for _, tc := range []struct {
		name     string
		change   func(*Runner, *InsurancePolicy, *[]InsurancePolicy, *UnderwritingBudget)
		rejected bool
		oldModel bool
		lockDays float64
	}{
		{name: "no history uses policy lapse", lockDays: 15},
		{name: "route sailing days", lockDays: 2, change: func(r *Runner, _ *InsurancePolicy, _ *[]InsurancePolicy, _ *UnderwritingBudget) {
			r.stateCache.Catalog.ShipClasses = []Hull{{ID: "hoy", BaseSpeedCentiknots: 1000}}
			r.stateCache.Routes = map[string]Route{
				"outward": {FromPort: uwAccLisbonPortID, ToPort: uwAccCadizPortID, ShipClass: "hoy", TotalDistanceNM: 384},
				"reverse": {FromPort: uwAccCadizPortID, ToPort: uwAccLisbonPortID, ShipClass: "hoy", TotalDistanceNM: 1920},
			}
		}},
		{name: "closed history retains lock duration", lockDays: 5, change: func(_ *Runner, _ *InsurancePolicy, history *[]InsurancePolicy, budget *UnderwritingBudget) {
			*history = uwAccSafeHistory(8)
			budget.TimeScale = 24
		}},
		{name: "Lloyds replaces unprofitable historical model", lockDays: 15, change: func(_ *Runner, _ *InsurancePolicy, history *[]InsurancePolicy, _ *UnderwritingBudget) {
			*history = uwAccSafeHistory(8)
			for i := range *history {
				(*history)[i].Status = "claimed_loss"
			}
		}},
		{name: "absent estimate preserves old model", oldModel: true, change: func(r *Runner, policy *InsurancePolicy, history *[]InsurancePolicy, _ *UnderwritingBudget) {
			r.snapshot["insurance"] = encode([]InsurancePolicy{*policy})
			*history = uwAccSafeHistory(8)
		}},
		{name: "null estimate preserves old model", oldModel: true, change: func(r *Runner, policy *InsurancePolicy, history *[]InsurancePolicy, _ *UnderwritingBudget) {
			r.snapshot["insurance"] = encode([]LloydsEstimate{{PolicyID: policy.ID}})
			*history = uwAccSafeHistory(8)
		}},
		{name: "minimum premium alone preserves old model", oldModel: true, change: func(r *Runner, policy *InsurancePolicy, history *[]InsurancePolicy, _ *UnderwritingBudget) {
			minimum := int64(275)
			r.snapshot["insurance"] = encode([]LloydsEstimate{{PolicyID: policy.ID, MinPremiumBPS: &minimum}})
			*history = uwAccSafeHistory(8)
		}},
		{name: "low margin rejects", rejected: true, change: func(r *Runner, policy *InsurancePolicy, history *[]InsurancePolicy, _ *UnderwritingBudget) {
			loss, margin := int64(100), int64(49)
			r.snapshot["insurance"] = encode([]LloydsEstimate{{PolicyID: policy.ID, ExpectedLossBPS: &loss, PremiumMarginBPS: &margin}})
			*history = uwAccSafeHistory(8)
		}},
		{name: "own company excluded", rejected: true, change: func(_ *Runner, policy *InsurancePolicy, _ *[]InsurancePolicy, _ *UnderwritingBudget) {
			policy.InsuredCompanyID = uwAccOwnCompanyID
		}},
		{name: "discretionary cash protected", rejected: true, change: func(_ *Runner, _ *InsurancePolicy, _ *[]InsurancePolicy, budget *UnderwritingBudget) {
			budget.DiscretionaryPence = 1
		}},
		{name: "local cash protected", rejected: true, change: func(_ *Runner, _ *InsurancePolicy, _ *[]InsurancePolicy, budget *UnderwritingBudget) {
			budget.AvailableLocalPence = 1
		}},
		{name: "pilot allowance protected", rejected: true, change: func(_ *Runner, _ *InsurancePolicy, _ *[]InsurancePolicy, budget *UnderwritingBudget) {
			budget.RemainingPilotPence = 1
		}},
		{name: "minimum slice capacity protected", rejected: true, change: func(_ *Runner, policy *InsurancePolicy, _ *[]InsurancePolicy, _ *UnderwritingBudget) {
			policy.CoveredBPS = 9600
		}},
		{name: "expired policy excluded", rejected: true, change: func(_ *Runner, _ *InsurancePolicy, _ *[]InsurancePolicy, budget *UnderwritingBudget) {
			budget.Now = budget.Now.Add(7 * 24 * time.Hour)
		}},
		{name: "changed permanent terms excluded", rejected: true, change: func(_ *Runner, policy *InsurancePolicy, history *[]InsurancePolicy, _ *UnderwritingBudget) {
			retained := *policy
			retained.TermsChanged = true
			*history = []InsurancePolicy{retained}
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			policy, budget := uwAccCandidate(), uwAccBudget()
			var listing map[string]any
			if err := json.Unmarshal(encode(policy), &listing); err != nil {
				t.Fatal(err)
			}
			listing["lloyds_expected_loss_bps"] = 100
			listing["lloyds_min_premium_bps"] = 275
			listing["premium_margin_bps"] = 200
			runner := Runner{snapshot: map[string]json.RawMessage{"insurance": encode([]map[string]any{listing})}}
			var history []InsurancePolicy
			if tc.change != nil {
				tc.change(&runner, &policy, &history, &budget)
			}
			decision := runner.pricedUnderwriting(policy, history, budget)
			if tc.oldModel {
				if want := EvaluateUnderwriting(policy, history, budget); !reflect.DeepEqual(decision, want) {
					t.Fatalf("policy without estimate changed: got %+v, want %+v", decision, want)
				}
				return
			}
			if decision.Accepted == tc.rejected {
				t.Fatalf("acceptance = %t, want %t: %+v", decision.Accepted, !tc.rejected, decision)
			}
			if tc.rejected {
				return
			}
			if decision.SliceBPS != 500 || decision.CollateralPence != 128950 || decision.PremiumPence != 38685 ||
				decision.ExpectedClaimsPence != 1290 || decision.ExpectedNetPence != 37395 || decision.EstimatedLockGameDays != tc.lockDays {
				t.Fatalf("incorrect Lloyds slice or lock duration: %+v", decision)
			}
		})
	}
}

func TestLloydsSliceNetRoundingAndBounds(t *testing.T) {
	policy := uwAccCandidate()
	policy.InsuredValuePence = 1_234_567
	policy.PremiumBPS = 300
	policy.TotalPremiumPence = ceilBPS(policy.InsuredValuePence, policy.PremiumBPS)
	loss, margin := int64(100), int64(200)
	estimate := LloydsEstimate{PolicyID: policy.ID, ExpectedLossBPS: &loss, PremiumMarginBPS: &margin}
	if net, ok := lloydsSliceNet(policy, estimate, 500); !ok || net != 1233 {
		t.Fatalf("rounded slice = (%d, %t), want (1233, true)", net, ok)
	}
	for _, slice := range []int64{-1, 0, 10001} {
		if net, ok := lloydsSliceNet(policy, estimate, slice); ok {
			t.Fatalf("invalid slice %d accepted with net %d", slice, net)
		}
	}
	for _, invalidLoss := range []int64{-1, 10001} {
		estimate.ExpectedLossBPS = &invalidLoss
		if net, ok := lloydsSliceNet(policy, estimate, 500); ok {
			t.Fatalf("invalid expected loss %d accepted with net %d", invalidLoss, net)
		}
	}
}

func TestPricedLloydsRejectsMalformedEstimates(t *testing.T) {
	for _, field := range []string{"lloyds_expected_loss_bps", "lloyds_min_premium_bps", "premium_margin_bps"} {
		for _, value := range []any{"100", 1.5, int64(-1), int64(10001)} {
			t.Run(fmt.Sprintf("%s/%v", field, value), func(t *testing.T) {
				policy, budget := uwAccCandidate(), uwAccBudget()
				var listing map[string]any
				if err := json.Unmarshal(encode(policy), &listing); err != nil {
					t.Fatal(err)
				}
				listing["lloyds_expected_loss_bps"] = 100
				listing["lloyds_min_premium_bps"] = 275
				listing["premium_margin_bps"] = 200
				listing[field] = value
				var estimate LloydsEstimate
				if err := json.Unmarshal(encode(listing), &estimate); err == nil {
					t.Fatal("malformed estimate decoded without an error")
				}
				runner := Runner{snapshot: map[string]json.RawMessage{"insurance": encode([]map[string]any{listing})}}
				history := uwAccSafeHistory(8)
				decision := runner.pricedUnderwriting(policy, history, budget)
				if decision.Accepted || !strings.Contains(decision.Reason, "could not be decoded") {
					t.Fatalf("malformed estimate must reject rather than use history or partial values: %+v", decision)
				}
				listing["id"] = uwAccHistoryID(99)
				loss, margin := int64(100), int64(200)
				runner.snapshot["insurance"] = encode([]any{
					LloydsEstimate{PolicyID: policy.ID, ExpectedLossBPS: &loss, PremiumMarginBPS: &margin},
					listing,
				})
				if decision := runner.pricedUnderwriting(policy, history, budget); !decision.Accepted || decision.ExpectedNetPence != 37395 {
					t.Fatalf("another policy's malformed estimate must not block this valid estimate: %+v", decision)
				}
			})
		}
	}
}
