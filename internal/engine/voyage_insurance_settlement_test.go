package engine

import (
	"encoding/json"
	"testing"
	"time"
)

func TestVoyageInsuranceSettlementUsesOnlyOwnPolicyCashCredits(t *testing.T) {
	now := time.Now().UTC()
	policyID := "aaaaaaaa-aaaa-4aaa-8aaa-aaaaaaaaaaaa"
	runner := &Runner{generation: "sourcing-sync", meta: map[string]DatasetMeta{
		"insurance": sourcingFreshMeta(now), "ledger": sourcingFreshMeta(now),
	}, snapshot: make(map[string]json.RawMessage), stateCache: AccountState{
		Vaults:            []Vault{{AccountID: "own-cash", AccountType: "vault"}, {AccountID: "own-escrow", AccountType: "escrow"}},
		InsurancePolicies: []InsurancePolicy{{ID: policyID, Status: "settled_safe"}},
	}}
	runner.snapshot["ledger"] = json.RawMessage(`[
		{"id":"refund","debit_account_id":"own-escrow","credit_account_id":"own-cash","amount_pence":5000,"reason":"insurance_premium_refund","metadata":{"policy_id":"aaaaaaaa-aaaa-4aaa-8aaa-aaaaaaaaaaaa"}},
		{"id":"claim","debit_account_id":"writer","credit_account_id":"own-cash","amount_pence":20000,"reason":"insurance_claim_payout","metadata":{"policy_id":"aaaaaaaa-aaaa-4aaa-8aaa-aaaaaaaaaaaa"}},
		{"id":"other-policy","debit_account_id":"writer","credit_account_id":"own-cash","amount_pence":90000,"reason":"insurance_claim_payout","metadata":{"policy_id":"bbbbbbbb-bbbb-4bbb-8bbb-bbbbbbbbbbbb"}},
		{"id":"internal","debit_account_id":"own-escrow","credit_account_id":"own-cash","amount_pence":1000000,"reason":"insurance_collateral_return","metadata":{"policy_id":"aaaaaaaa-aaaa-4aaa-8aaa-aaaaaaaaaaaa"}},
		{"id":"lender","debit_account_id":"writer","credit_account_id":"lender-vault","amount_pence":80000,"reason":"loan_insurance_loss_paid","metadata":{"policy_id":"aaaaaaaa-aaaa-4aaa-8aaa-aaaaaaaaaaaa"}}
	]`)
	data := missionData{FeesPence: 10_000, ProceedsPence: 50_000, VoyageInsurance: []voyageInsuranceIntent{{PolicyID: policyID, Quote: voyageCoverTestQuote()}}}
	data.VoyageInsuranceSettledAt = now.Add(time.Second)
	if err := runner.settleVoyageInsurance(&data); err == nil || data.FeesPence != 10_000 || data.ProceedsPence != 50_000 {
		t.Fatal("ledger fetched before terminal policy observation fixed an incomplete settlement")
	}
	data.VoyageInsuranceSettledAt = now.Add(-time.Second)
	if err := runner.settleVoyageInsurance(&data); err != nil {
		t.Fatal(err)
	}
	if data.FeesPence != 5_000 || data.ProceedsPence != 70_000 {
		t.Fatalf("insurance principal, another policy or lender cash became mission income: fees=%d proceeds=%d", data.FeesPence, data.ProceedsPence)
	}
	runner.stateCache.InsurancePolicies[0].Status = "active"
	before := data.ProceedsPence
	if err := runner.settleVoyageInsurance(&data); err == nil || data.ProceedsPence != before {
		t.Fatal("unsettled cover booked speculative refunds/claims")
	}
}
