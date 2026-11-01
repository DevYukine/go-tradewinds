package sim

import (
	"encoding/json"
	"testing"
)

func TestPlayerLoanRequestDisbursementAndRepaymentAreNotTradingProfit(t *testing.T) {
	before := Money{AcquisitionPence: 100, ProceedsPence: 300, FeesPence: 10, ProvisionsPence: 20, ExpensesPence: 30}
	money := before
	for _, receipt := range []struct {
		stage string
		raw   string
	}{
		{"loan-request", `{"id":"request-1","principal_pence":1000000}`},
		{"loan-accept", `{"id":"loan-1","request_id":"request-1","principal_pence":1000000}`},
		{"loan-repay", `{"id":"loan-1","owed_pence":0,"principal_repaid_pence":1000000,"interest_paid_pence":10000}`},
	} {
		if err := ApplyReceipt(&money, receipt.stage, json.RawMessage(receipt.raw)); err != nil {
			t.Fatal(err)
		}
		if money != before || money.Net() != before.Net() {
			t.Fatalf("%s became trading income or expenses: before %+v after %+v", receipt.stage, before, money)
		}
	}
}
