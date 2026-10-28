package engine

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/DevYukine/go-tradewinds/internal/gameapi"
)

// RG-4: durable missions written before gameapi.Loan carried is_mortgage must
// still load, while an explicitly present classification stays exact.
func TestLegacyMissionLoanWithoutMortgageFieldLoads(t *testing.T) {
	t.Parallel()
	const legacy = `{
		"plan":{"kind":"spot","origin":"amsterdam","destination":"rotterdam","commodity":"grain","quantity":10},
		"loan_repayment":{"before":{
			"id":"loan-old","lender_company_id":"goldsmiths","borrower_company_id":"company-1",
			"banking_region":"north_sea","principal_pence":750000,"rate_basis":"annual","daily_rate_bps":0,
			"term_game_days":360,"repayment":"amortised","arrears_pence":0,
			"collateral_kind":"ship_lien","collateral_value_pence":2000000,"collateral_status":"pledged",
			"status":"active","outstanding_principal_pence":700000,"owed_pence":700000,
			"offer_expires_at":"2026-10-26T18:06:00Z","created_at":"2026-10-26T18:00:00Z","updated_at":"2026-10-26T18:02:00Z"
		},"amount_pence":700000}
	}`
	var data missionData
	if err := json.Unmarshal([]byte(legacy), &data); err != nil {
		t.Fatalf("old durable mission must load: %v", err)
	}
	before := data.LoanRepayment.Before
	if data.LoanRepayment.AmountPence != 700000 || before.ID != "loan-old" ||
		before.OwedPence != 700000 || before.CollateralStatus != "pledged" || before.IsMortgage {
		t.Fatalf("legacy loan lost fields or invented a mortgage: %+v", before)
	}
	for _, test := range []struct {
		name    string
		value   string
		want    bool
		wantErr bool
	}{
		{"explicit false", `"is_mortgage":false`, false, false},
		{"explicit true", `"is_mortgage":true`, true, false},
		{"explicit null", `"is_mortgage":null`, false, true},
	} {
		t.Run(test.name, func(t *testing.T) {
			var classified missionData
			err := json.Unmarshal([]byte(strings.Replace(legacy, `"collateral_status":"pledged"`,
				`"collateral_status":"pledged", `+test.value, 1)), &classified)
			if test.wantErr != (err != nil) {
				t.Fatalf("durable is_mortgage %s error = %v, wantErr %t", test.value, err, test.wantErr)
			}
			if err == nil && classified.LoanRepayment.Before.IsMortgage != test.want {
				t.Fatalf("durable is_mortgage %s decoded %t, want %t",
					test.value, classified.LoanRepayment.Before.IsMortgage, test.want)
			}
		})
	}
}

// Live loan capture keeps failing closed: validateBorrowerLoans requires the
// is_mortgage classification on every published row even though durable
// mission JSON without the field now decodes.
func TestLiveCaptureStillRejectsMissingMortgageClassification(t *testing.T) {
	t.Parallel()
	loan := snapshotTestLoan()
	companyID := loan.BorrowerCompanyID
	for _, test := range []struct {
		name    string
		value   any
		present bool
	}{
		{"absent", nil, false},
		{"null", nil, true},
		{"string", "yes", true},
		{"number", 1, true},
	} {
		t.Run(test.name, func(t *testing.T) {
			var rows []map[string]any
			if err := json.Unmarshal(encode([]gameapi.Loan{loan}), &rows); err != nil {
				t.Fatal(err)
			}
			if test.present {
				rows[0]["is_mortgage"] = test.value
			} else {
				delete(rows[0], "is_mortgage")
			}
			if err := validateBorrowerLoans(encode(rows), companyID); err == nil {
				t.Fatalf("live capture accepted is_mortgage %s", test.name)
			}
		})
	}
	if err := validateBorrowerLoans(encode([]gameapi.Loan{loan}), companyID); err != nil {
		t.Fatalf("boolean is_mortgage still validates: %v", err)
	}
}
