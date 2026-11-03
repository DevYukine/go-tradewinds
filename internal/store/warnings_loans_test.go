package store

import (
	"context"
	"encoding/json"
	"math"
	"reflect"
	"slices"
	"testing"
	"time"

	"github.com/DevYukine/go-tradewinds/internal/gameapi"
)

func loanWarningInput() WarningInput {
	installment := int64(1_000)
	return WarningInput{
		CompanyID: "company-one",
		Metadata: map[string]WarningDataset{
			"loans":  {UpdatedAt: warningsAcceptanceAt, Complete: true},
			"vaults": {UpdatedAt: warningsAcceptanceAt, Complete: true},
			"clock":  {UpdatedAt: warningsAcceptanceAt, Complete: true},
		},
		Clock: gameapi.Clock{Calendar: gameapi.Calendar{TimeScale: 60}},
		Loans: []gameapi.Loan{{
			ID: "old-loan", BorrowerCompanyID: "company-one", Status: "active",
			Repayment: "amortised", CollateralKind: "unsecured",
			InstallmentPence: &installment, OwedPence: 20_000,
		}},
		Vaults: []gameapi.Vault{{AccountType: "vault", BalancePence: 5_999}},
	}
}

func TestAssessWarningsLoanPaymentsBoundaries(t *testing.T) {
	for _, test := range []struct {
		name     string
		cash     int64
		arrears  int64
		scale    int32
		severity string
	}{
		{name: "exact real day", cash: 6_000, scale: 60},
		{name: "one penny below real day", cash: 5_999, scale: 60, severity: "warning"},
		{name: "exactly two installments", cash: 2_000, scale: 60, severity: "warning"},
		{name: "below two installments", cash: 1_999, scale: 60, severity: "critical"},
		{name: "no cash", cash: 0, scale: 60, severity: "critical"},
		{name: "negative vault", cash: -1_000, scale: 60, severity: "critical"},
		{name: "arrears despite cash", cash: 10_000, arrears: 1, scale: 60, severity: "critical"},
		{name: "derived slower clock", cash: 3_000, scale: 30},
		{name: "derived faster clock", cash: 6_000, scale: 120, severity: "warning"},
	} {
		t.Run(test.name, func(t *testing.T) {
			input := loanWarningInput()
			input.Vaults[0].BalancePence = test.cash
			input.Loans[0].ArrearsPence = test.arrears
			input.Clock.Calendar.TimeScale = test.scale
			assessment := AssessWarnings(input, warningsAcceptanceAt)
			warning := warningsAcceptanceFind(assessment.Warnings, "loan_payments_at_risk")
			if slices.Contains(assessment.Suppressed, "loan_payments_at_risk") || (warning != nil) != (test.severity != "") {
				t.Fatalf("loan warning boundary: %+v", assessment)
			}
			if warning == nil {
				return
			}
			want := map[string]int64{
				"cash_pence": max(0, test.cash), "owed_pence": 20_000, "installment_pence": 1_000,
				"arrears_pence": test.arrears, "installments_covered": max(0, test.cash) / 1_000,
			}
			if warning.Severity != test.severity || !reflect.DeepEqual(warning.Facts, want) {
				t.Fatalf("warning = %+v, want severity %s facts %+v", warning, test.severity, want)
			}
		})
	}
}

func TestAssessWarningsLoanPaymentsNegativeVaultPreservesOtherCash(t *testing.T) {
	input := loanWarningInput()
	input.Vaults = append(input.Vaults, gameapi.Vault{AccountType: "vault", BalancePence: -10_000})
	assessment := AssessWarnings(input, warningsAcceptanceAt)
	warning := warningsAcceptanceFind(assessment.Warnings, "loan_payments_at_risk")
	if slices.Contains(assessment.Suppressed, "loan_payments_at_risk") || warning == nil ||
		warning.Severity != "warning" || warning.Facts["cash_pence"] != 5_999 {
		t.Fatalf("negative vault must not suppress risk or subtract other spendable cash: %+v", assessment)
	}
}

func TestAssessWarningsLoanPaymentsEligibility(t *testing.T) {
	for name, change := range map[string]func(*WarningInput){
		"no loans":         func(input *WarningInput) { input.Loans = nil },
		"another borrower": func(input *WarningInput) { input.Loans[0].BorrowerCompanyID = "other" },
		"mortgage":         func(input *WarningInput) { input.Loans[0].IsMortgage = true },
		"ship lien":        func(input *WarningInput) { input.Loans[0].CollateralKind = "ship_lien" },
		"repaid":           func(input *WarningInput) { input.Loans[0].Status = "repaid" },
		"bullet":           func(input *WarningInput) { input.Loans[0].Repayment = "bullet" },
	} {
		t.Run(name, func(t *testing.T) {
			input := loanWarningInput()
			change(&input)
			assessment := AssessWarnings(input, warningsAcceptanceAt)
			if warningsAcceptanceFind(assessment.Warnings, "loan_payments_at_risk") != nil || slices.Contains(assessment.Suppressed, "loan_payments_at_risk") {
				t.Fatalf("ineligible loan is an assessed clear: %+v", assessment)
			}
		})
	}
}

func TestAssessWarningsLoanPaymentsAggregate(t *testing.T) {
	input := loanWarningInput()
	input.Vaults = []gameapi.Vault{
		{AccountType: "vault", BankingRegion: "europe", BalancePence: 4_000},
		{AccountType: "vault", BankingRegion: "america", BalancePence: 3_000},
		{AccountType: "futures_escrow", BalancePence: 1_000_000},
	}
	second := input.Loans[0]
	second.ID, second.ArrearsPence = "new-loan", 50
	input.Loans = append(input.Loans, second)
	assessment := AssessWarnings(input, warningsAcceptanceAt)
	var found []Warning
	for _, warning := range assessment.Warnings {
		if warning.Code == "loan_payments_at_risk" {
			found = append(found, warning)
		}
	}
	want := map[string]int64{
		"cash_pence": 7_000, "owed_pence": 40_000, "installment_pence": 2_000,
		"arrears_pence": 50, "installments_covered": 3,
	}
	if len(found) != 1 || found[0].Severity != "critical" || !reflect.DeepEqual(found[0].Facts, want) {
		t.Fatalf("loans must aggregate once and escrow cannot fund payments: %+v", found)
	}
}

func TestAssessWarningsLoanPaymentsSuppression(t *testing.T) {
	for _, dataset := range []string{"loans", "vaults", "clock"} {
		for name, meta := range map[string]WarningDataset{
			"stale flag": {UpdatedAt: warningsAcceptanceAt, Complete: true, Stale: true},
			"expired":    {UpdatedAt: warningsAcceptanceAt.Add(-WarningFreshFor), Complete: true},
			"future":     {UpdatedAt: warningsAcceptanceAt.Add(time.Second), Complete: true},
			"incomplete": {UpdatedAt: warningsAcceptanceAt},
			"missing":    {},
			"error":      {UpdatedAt: warningsAcceptanceAt, Complete: true, Error: "failed"},
		} {
			t.Run(dataset+"/"+name, func(t *testing.T) {
				input := loanWarningInput()
				input.Metadata[dataset] = meta
				assessment := AssessWarnings(input, warningsAcceptanceAt)
				if !slices.Contains(assessment.Suppressed, "loan_payments_at_risk") || warningsAcceptanceFind(assessment.Warnings, "loan_payments_at_risk") != nil {
					t.Fatalf("missing current observations must suppress assessment: %+v", assessment)
				}
			})
		}
	}
	for name, change := range map[string]func(*WarningInput){
		"zero time scale":     func(input *WarningInput) { input.Clock.Calendar.TimeScale = 0 },
		"negative time scale": func(input *WarningInput) { input.Clock.Calendar.TimeScale = -1 },
		"paused":              func(input *WarningInput) { input.Clock.Paused = true },
		"missing installment": func(input *WarningInput) { input.Loans[0].InstallmentPence = nil },
		"aggregate overflow": func(input *WarningInput) {
			input.Loans[0].OwedPence = math.MaxInt64
			input.Loans = append(input.Loans, input.Loans[0])
		},
	} {
		t.Run(name, func(t *testing.T) {
			input := loanWarningInput()
			change(&input)
			assessment := AssessWarnings(input, warningsAcceptanceAt)
			if !slices.Contains(assessment.Suppressed, "loan_payments_at_risk") || warningsAcceptanceFind(assessment.Warnings, "loan_payments_at_risk") != nil {
				t.Fatalf("unassessable payments must be suppressed: %+v", assessment)
			}
		})
	}
}

func TestAssessWarningsLoanPaymentsLargePence(t *testing.T) {
	input := loanWarningInput()
	installment := int64(math.MaxInt64 / 6)
	input.Loans[0].InstallmentPence = &installment
	input.Vaults[0].BalancePence = installment * 6
	assessment := AssessWarnings(input, warningsAcceptanceAt)
	if warningsAcceptanceFind(assessment.Warnings, "loan_payments_at_risk") != nil || slices.Contains(assessment.Suppressed, "loan_payments_at_risk") {
		t.Fatalf("large exact real-day boundary overflowed: %+v", assessment)
	}
	input.Vaults[0].BalancePence--
	warning := warningsAcceptanceRequire(t, input, "loan_payments_at_risk", true)
	if warning.Severity != "warning" || warning.Facts["installments_covered"] != 5 {
		t.Fatalf("large one-penny deficit was rounded or overflowed: %+v", warning)
	}
}

func TestWarningsLoanPaymentsSnapshot(t *testing.T) {
	database, _, _ := integrationStore(t)
	ctx := context.Background()
	saveTestAccount(t, database, "one")
	input := loanWarningInput()
	foreign := input.Loans[0]
	foreign.BorrowerCompanyID = "another-company"
	if err := database.SaveSnapshot(ctx, "one", map[string]json.RawMessage{
		"_meta": rivalTestJSON(t, input.Metadata),
		"clock": rivalTestJSON(t, input.Clock),
		"loans": rivalTestJSON(t, append(input.Loans, foreign)),
		"vaults": rivalTestJSON(t, map[string]any{"vaults": []gameapi.Vault{
			{AccountType: "vault", BalancePence: 5_999},
			{AccountType: "escrow", BalancePence: 50_000},
		}}),
	}); err != nil {
		t.Fatal(err)
	}
	loaded, code, err := warningInput(database.db, "one", warningsAcceptanceAt)
	if err != nil || code != "" || len(loaded.Loans) != 2 || len(loaded.Vaults) != 2 {
		t.Fatalf("snapshot payment inputs: loans=%+v vaults=%+v code=%q err=%v", loaded.Loans, loaded.Vaults, code, err)
	}
	accounts, err := database.Warnings(ctx, warningsAcceptanceAt)
	if err != nil || len(accounts) != 1 || !accounts[0].Available {
		t.Fatalf("warning accounts=%+v err=%v", accounts, err)
	}
	warning := warningsAcceptanceFind(accounts[0].Warnings, "loan_payments_at_risk")
	if warning == nil || warning.Severity != "warning" || warning.Facts["cash_pence"] != 5_999 || warning.Facts["installment_pence"] != 1_000 {
		t.Fatalf("snapshot warning must use own debt and only spendable vaults: %+v", accounts[0])
	}
}
