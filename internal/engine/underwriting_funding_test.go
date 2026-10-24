package engine

import (
	"bytes"
	"context"
	"encoding/json"
	"testing"
	"time"

	"github.com/DevYukine/go-tradewinds/internal/store"
	"go.uber.org/zap"
)

func TestUnderwritingFundingRequiresUniqueOwnedDebitIdentity(t *testing.T) {
	now := time.Date(2026, 10, 1, 12, 0, 0, 0, time.UTC)
	position := store.UnderwritingPosition{ID: "owned-slice", PolicyID: "policy", CollateralPence: 50000, AcceptedAt: now}
	vaults := []Vault{{AccountID: "own-vault", AccountType: "vault", BankingRegion: "north"}}
	entry := map[string]any{"id": "debit", "debit_account_id": "own-vault", "amount_pence": int64(50000), "created_at": now, "metadata": map[string]any{"underwriting_id": "owned-slice"}}
	if proof := underwritingFundingEvidence(position, []json.RawMessage{encode(entry)}, vaults); len(proof) == 0 {
		t.Fatal("unique identity-linked own-vault collateral debit was not recognized")
	}
	for _, test := range []struct {
		name   string
		change func(map[string]any)
	}{
		{"equal amount without identity is not proof", func(row map[string]any) { row["metadata"] = map[string]any{"note": "unrelated trade"} }},
		{"another company's debit is not ours", func(row map[string]any) { row["debit_account_id"] = "foreign-vault" }},
		{"premium is not collateral", func(row map[string]any) { row["amount_pence"] = int64(15000) }},
		{"old unrelated policy debit is not this slice", func(row map[string]any) { row["created_at"] = now.Add(-time.Hour) }},
	} {
		t.Run(test.name, func(t *testing.T) {
			row := make(map[string]any, len(entry))
			for key, value := range entry {
				row[key] = value
			}
			test.change(row)
			if proof := underwritingFundingEvidence(position, []json.RawMessage{encode(row)}, vaults); proof != nil {
				t.Fatal("unrelated ledger entry established debit provenance")
			}
		})
	}
	duplicate := make(map[string]any, len(entry))
	for key, value := range entry {
		duplicate[key] = value
	}
	duplicate["id"] = "second-debit"
	if proof := underwritingFundingEvidence(position, []json.RawMessage{encode(entry), encode(duplicate)}, vaults); proof != nil {
		t.Fatal("ambiguous matching debits were treated as unique funding evidence")
	}
}

func TestInsuranceCaptureRejectsMissingCoverageWithoutReplacingLastGood(t *testing.T) {
	policy := uwExecPolicy(time.Date(2026, 10, 1, 12, 0, 0, 0, time.UTC))
	policy.CoveredBPS = 0
	complete := encode([]InsurancePolicy{policy})
	for _, absent := range []bool{false, true} {
		var row map[string]any
		if err := json.Unmarshal(encode(policy), &row); err != nil {
			t.Fatal(err)
		}
		if absent {
			delete(row, "covered_bps")
		} else {
			row["covered_bps"] = nil
		}
		runner := &Runner{log: zap.NewNop(), snapshot: map[string]json.RawMessage{"insurance": complete}, meta: make(map[string]DatasetMeta), generation: "current"}
		if runner.applyCapture(context.Background(), "insurance", fetchResult{data: encode([]any{row}), attemptedAt: time.Now(), completedAt: time.Now()}) {
			t.Fatal("missing/null coverage became available underwriting capacity")
		}
		if !runner.meta["insurance"].Stale || !bytes.Equal(runner.snapshot["insurance"], complete) {
			t.Fatal("malformed policy replaced the last complete financial observation")
		}
		if !runner.applyCapture(context.Background(), "insurance", fetchResult{data: complete, attemptedAt: time.Now(), completedAt: time.Now()}) {
			t.Fatal("explicit zero coverage was rejected as absent")
		}
	}
}

func TestUnderwritingProtectsCapableFallbackVaults(t *testing.T) {
	runner := &Runner{}
	runner.stateCache.HomePortID = "home"
	runner.stateCache.Catalog.Ports = []Port{
		{ID: "home", BankingRegion: "north"},
		{ID: "origin", BankingRegion: "south"},
	}
	runner.stateCache.Vaults = []Vault{
		{AccountType: "vault", BankingRegion: "north", BalancePence: 100_000},
		{AccountType: "vault", BankingRegion: "west", BalancePence: 50_000},
		{AccountType: "vault", BankingRegion: "east", BalancePence: 49_999},
		{AccountType: "escrow", BankingRegion: "escrow-only", BalancePence: 100_000},
	}
	policy := uwExecPolicy(time.Date(2026, 10, 1, 12, 0, 0, 0, time.UTC))
	policy.OriginPortID = "origin"
	regions, err := runner.underwritingRegions(policy)
	if err != nil {
		t.Fatal(err)
	}
	protected := make(map[string]bool)
	for _, region := range regions {
		protected[region] = true
	}
	for _, region := range []string{"north", "south", "west"} {
		if !protected[region] {
			t.Errorf("possible collateral debit in %s would escape protected reserves", region)
		}
	}
	for _, region := range []string{"east", "escrow-only"} {
		if protected[region] {
			t.Errorf("ineligible funding account %s unnecessarily blocks a pilot", region)
		}
	}
}

func TestUnderwritingAdmissionUsesObservedServerTime(t *testing.T) {
	h := newRecoveryHarness(t)
	base := time.Now().UTC()
	proxy := installUWExecProxy(t, h)
	proxy.insuranceRows = uwExecBoardRows(base)
	runner := h.runner(primaryTestKey)
	runner.generation = "server-clock-regression"
	if err := runner.refresh(h.ctx); err != nil {
		t.Fatal(err)
	}
	runner.stateCache.Clock.RealUTC = base.Add(2 * time.Hour)
	runner.snapshot["clock"] = encode(runner.stateCache.Clock)
	policy := uwExecPolicy(base)
	policy.ExpiresAt = base.Add(time.Hour)
	budget, err := runner.underwritingBudget(h.ctx, policy, false)
	if err != nil {
		t.Fatal(err)
	}
	var history []InsurancePolicy
	if err := json.Unmarshal(encode(uwExecCohort(base)), &history); err != nil {
		t.Fatal(err)
	}
	if decision := EvaluateUnderwriting(policy, history, budget); decision.Accepted {
		t.Fatal("host clock authorized underwriting after the observed server expiry")
	}
	policy.ExpiresAt = base.Add(3 * time.Hour)
	if decision := EvaluateUnderwriting(policy, history, budget); !decision.Accepted {
		t.Fatalf("server-valid funded policy was rejected: %s", decision.Reason)
	}
}
