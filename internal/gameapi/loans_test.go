package gameapi_test

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/DevYukine/go-tradewinds/internal/gameapi"
)

const loanQuoteJSON = `{
	"quote": {
		"id":"quote-1", "request_id":null, "lender_company_id":"lender-1",
		"lender_name":"Goldsmiths", "borrower_company_id":"borrower-1",
		"banking_region":"north_sea", "principal_pence":750000,
		"annual_rate_bps":3000, "term_game_days":360, "repayment":"amortised",
		"total_repayable_pence":871214, "installment_pence":24200,
		"collateral_ship_id":"ship-1", "status":"open",
		"expires_at":"2026-10-26T18:06:00Z", "loan_id":null,
		"created_at":"2026-10-26T18:00:00Z"
	},
	"rebuild_value_pence":2500000, "loan_to_value_bps":4000,
	"max_principal_pence":750000, "recourse":true
}`

func TestLoanGoldsmithsQuoteEncodedQuery(t *testing.T) {
	for _, region := range []string{"", "north_sea & other=1"} {
		t.Run("region="+region, func(t *testing.T) {
			capture := &capturingServer{handler: func(w http.ResponseWriter, r *http.Request, _ int) {
				if r.Method != http.MethodGet || r.URL.Path != "/v1/loan/quote" {
					t.Errorf("request = %s %s, want GET /v1/loan/quote", r.Method, r.URL.Path)
				}
				want := map[string][]string{
					"principal_pence": {"9007199254740993"},
					"term_game_days":  {"360"},
					"repayment":       {"amortised & x=1"},
					"ship_id":         {"ship/one?x=1&other=2 +"},
				}
				if region != "" {
					want["banking_region"] = []string{region}
				}
				if got := map[string][]string(r.URL.Query()); !reflect.DeepEqual(got, want) {
					t.Errorf("query = %#v, want %#v", got, want)
				}
				body, _ := io.ReadAll(r.Body)
				if len(body) != 0 || r.Header.Get("Idempotency-Key") != "" {
					t.Error("quote GET must not send a mutation body or idempotency key")
				}
				_, _ = io.WriteString(w, loanQuoteJSON)
			}}
			server := httptest.NewServer(capture)
			defer server.Close()
			quote, err := newTestClient(t, server).GetGoldsmithsQuote(context.Background(), gameapi.GoldsmithsQuoteRequest{
				PrincipalPence: 9007199254740993,
				TermGameDays:   360,
				Repayment:      "amortised & x=1",
				ShipID:         "ship/one?x=1&other=2 +",
				BankingRegion:  region,
			})
			if err != nil {
				t.Fatal(err)
			}
			if capture.count() != 1 || quote.Quote.ID != "quote-1" || quote.Quote.AnnualRateBPS != 3000 || quote.LoanToValueBPS != 4000 {
				t.Fatalf("unexpected quote or request count: %+v, %d", quote, capture.count())
			}
			assertLoanJSON(t, loanQuoteJSON, quote)
		})
	}
}

func TestLoanQuoteNullableFields(t *testing.T) {
	for _, fixture := range []string{
		loanQuoteJSON,
		strings.NewReplacer(
			`"request_id":null`, `"request_id":"request-1"`,
			`"installment_pence":24200`, `"installment_pence":null`,
			`"collateral_ship_id":"ship-1"`, `"collateral_ship_id":null`,
			`"loan_id":null`, `"loan_id":"loan-1"`,
		).Replace(loanQuoteJSON),
	} {
		var quote gameapi.GoldsmithsQuote
		if err := json.Unmarshal([]byte(fixture), &quote); err != nil {
			t.Fatal(err)
		}
		assertLoanJSON(t, fixture, quote)
	}
}

func TestLoanAnnualAndLegacyDailyDecoding(t *testing.T) {
	const annual = `{
		"id":"loan-1", "lender_company_id":"lender-1", "borrower_company_id":"borrower-1",
		"banking_region":"north_sea", "principal_pence":9007199254740993,
		"rate_basis":"annual", "daily_rate_bps":0, "annual_rate_bps":3000,
		"term_game_days":360, "repayment":"amortised", "total_repayable_pence":975000,
		"installment_pence":24200, "next_payment_due_at":"2026-10-26T22:00:00Z",
		"next_payment_pence":24201, "arrears_pence":100, "arrears_due_by":"2026-10-26T18:24:00Z",
		"is_mortgage":false, "recourse":true, "collateral_kind":"ship_lien", "collateral_ship_id":"ship-1",
		"collateral_port_id":"amsterdam", "collateral_commodity_id":"grain", "collateral_tons":25,
		"collateral_contract_id":"contract-1", "collateral_value_pence":2500000,
		"collateral_status":"pledged", "status":"active", "outstanding_principal_pence":700000,
		"accrued_interest_pence":101, "owed_pence":700101, "principal_repaid_pence":50000,
		"interest_paid_pence":102, "collateral_credit_pence":103, "extinguished_pence":104,
		"written_off_pence":105, "foreclosure_surplus_paid_pence":106, "foreclosure_surplus_owed_pence":107,
		"offer_expires_at":"2026-10-26T18:06:00Z", "disbursed_at":"2026-10-26T18:01:00Z",
		"matures_at":"2026-11-01T18:01:00Z", "defaulted_at":"2026-10-27T18:00:00Z",
		"closed_at":"2026-10-28T18:00:00Z", "interest_accrued_through":"2026-10-26T18:00:00Z",
		"request_id":"request-1", "quote_id":"quote-1",
		"created_at":"2026-10-26T18:00:00Z", "updated_at":"2026-10-26T18:02:00Z"
	}`
	const legacy = `{
		"id":"loan-2", "lender_company_id":"lender-1", "borrower_company_id":"borrower-1",
		"banking_region":"channel", "principal_pence":750000,
		"rate_basis":"daily", "daily_rate_bps":45, "annual_rate_bps":null,
		"term_game_days":20, "repayment":"balloon", "total_repayable_pence":null,
		"installment_pence":null, "next_payment_due_at":null, "next_payment_pence":null,
		"arrears_pence":0, "arrears_due_by":null, "is_mortgage":false, "recourse":false, "collateral_kind":"unsecured",
		"collateral_ship_id":null, "collateral_port_id":null, "collateral_commodity_id":null,
		"collateral_tons":null, "collateral_contract_id":null, "collateral_value_pence":0,
		"collateral_status":"none", "status":"repaid", "outstanding_principal_pence":0,
		"accrued_interest_pence":0, "owed_pence":0, "principal_repaid_pence":750000,
		"interest_paid_pence":67500, "collateral_credit_pence":0, "extinguished_pence":0,
		"written_off_pence":0, "foreclosure_surplus_paid_pence":0, "foreclosure_surplus_owed_pence":0,
		"offer_expires_at":"2026-10-26T18:06:00Z", "disbursed_at":null, "matures_at":null,
		"defaulted_at":null, "closed_at":null, "interest_accrued_through":null,
		"request_id":null, "quote_id":null,
		"created_at":"2026-10-26T18:00:00Z", "updated_at":"2026-10-26T18:02:00Z"
	}`
	for _, fixture := range []string{annual, legacy} {
		var loan gameapi.Loan
		if err := json.Unmarshal([]byte(fixture), &loan); err != nil {
			t.Fatal(err)
		}
		t.Run(loan.RateBasis, func(t *testing.T) {
			if loan.RateBasis == "annual" {
				if loan.AnnualRateBPS == nil || *loan.AnnualRateBPS != 3000 || loan.DailyRateBPS != 0 || loan.PrincipalPence != 9007199254740993 {
					t.Fatalf("annual terms lost or reinterpreted: %+v", loan)
				}
				wantDue := time.Date(2026, 10, 26, 22, 0, 0, 0, time.UTC)
				if loan.NextPaymentDueAt == nil || !loan.NextPaymentDueAt.Equal(wantDue) {
					t.Fatalf("real-UTC next payment = %v, want %v", loan.NextPaymentDueAt, wantDue)
				}
			} else if loan.AnnualRateBPS != nil || loan.DailyRateBPS != 45 || loan.InstallmentPence != nil || loan.NextPaymentDueAt != nil || loan.QuoteID != nil {
				t.Fatalf("legacy nullable fields or daily rate changed: %+v", loan)
			}
			assertLoanJSON(t, fixture, loan)
		})
	}
}

func TestLoanMortgageClassificationDecoding(t *testing.T) {
	const legacy = `{
		"id":"loan-old", "lender_company_id":"lender-1", "borrower_company_id":"borrower-1",
		"banking_region":"north_sea", "principal_pence":750000,
		"rate_basis":"annual", "daily_rate_bps":0, "annual_rate_bps":3000,
		"term_game_days":360, "repayment":"amortised", "status":"active",
		"recourse":true, "collateral_kind":"ship_lien", "collateral_status":"pledged",
		"owed_pence":700000, "offer_expires_at":"2026-10-26T18:06:00Z",
		"created_at":"2026-10-26T18:00:00Z", "updated_at":"2026-10-26T18:02:00Z"
	}`
	var loan gameapi.Loan
	if err := json.Unmarshal([]byte(legacy), &loan); err != nil {
		t.Fatalf("durable loan written before is_mortgage must decode: %v", err)
	}
	if loan.IsMortgage || loan.ID != "loan-old" || loan.PrincipalPence != 750000 || !loan.Recourse {
		t.Fatalf("absent is_mortgage misdecoded: %+v", loan)
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
		{"string value", `"is_mortgage":"yes"`, false, true},
		{"number value", `"is_mortgage":1`, false, true},
	} {
		t.Run(test.name, func(t *testing.T) {
			var classified gameapi.Loan
			err := json.Unmarshal([]byte(strings.Replace(legacy, `"recourse":true`,
				`"recourse":true, `+test.value, 1)), &classified)
			if test.wantErr != (err != nil) {
				t.Fatalf("is_mortgage %s error = %v, wantErr %t", test.value, err, test.wantErr)
			}
			if err == nil && classified.IsMortgage != test.want {
				t.Fatalf("is_mortgage %s decoded %t, want %t", test.value, classified.IsMortgage, test.want)
			}
		})
	}
}

func TestLoanGoldsmithsQuoteErrorsRetained(t *testing.T) {
	for _, test := range []struct {
		name   string
		status int
		body   string
		code   string
	}{
		{"bad principal", http.StatusBadRequest, `{"code":"invalid_principal","detail":"above house cap"}`, "invalid_principal"},
		{"missing ship", http.StatusNotFound, `{"code":"not_found","detail":"ship not found"}`, "not_found"},
		{"lien conflict", http.StatusConflict, `{"code":"ship_liened","detail":"already pledged"}`, "ship_liened"},
		{"invalid response", http.StatusOK, `{"quote":{"annual_rate_bps":"wrong"}}`, "invalid_response"},
	} {
		t.Run(test.name, func(t *testing.T) {
			capture := &capturingServer{handler: func(w http.ResponseWriter, _ *http.Request, _ int) {
				w.WriteHeader(test.status)
				_, _ = io.WriteString(w, test.body)
			}}
			server := httptest.NewServer(capture)
			defer server.Close()
			_, err := newTestClient(t, server).GetGoldsmithsQuote(context.Background(), gameapi.GoldsmithsQuoteRequest{
				PrincipalPence: 750000, TermGameDays: 360, Repayment: "amortised", ShipID: "ship-1",
			})
			var apiErr *gameapi.APIError
			if !errors.As(err, &apiErr) {
				t.Fatalf("error = %v, want APIError", err)
			}
			if apiErr.Status != test.status || apiErr.Code != test.code || string(apiErr.Body) != test.body || apiErr.Detail == "" {
				t.Fatalf("lost HTTP outcome: %+v", apiErr)
			}
			if capture.count() != 1 {
				t.Fatalf("requests = %d, want no retry for definitive outcome", capture.count())
			}
			if test.status == http.StatusOK && errors.Unwrap(apiErr) == nil {
				t.Error("response decoding cause was lost")
			}
		})
	}
}

func TestLoanActionRequestEncoding(t *testing.T) {
	amount := int64(9007199254740993)
	for _, test := range []struct {
		request any
		want    string
	}{
		{gameapi.QuoteActionRequest{QuoteID: "quote-1"}, `{"quote_id":"quote-1"}`},
		{gameapi.RepayLoanRequest{LoanID: "loan-1"}, `{"loan_id":"loan-1"}`},
		{gameapi.RepayLoanRequest{LoanID: "loan-1", AmountPence: &amount}, `{"loan_id":"loan-1","amount_pence":9007199254740993}`},
	} {
		got, err := json.Marshal(test.request)
		if err != nil {
			t.Fatal(err)
		}
		if string(got) != test.want {
			t.Errorf("request = %s, want %s", got, test.want)
		}
	}
}

// Comparing decoded JSON with UseNumber detects missing fields and null/zero
// conflation without rounding integer pence through float64.
func assertLoanJSON(t *testing.T, fixture string, value any) {
	t.Helper()
	encoded, err := json.Marshal(value)
	if err != nil {
		t.Fatal(err)
	}
	var want, got any
	decoder := json.NewDecoder(strings.NewReader(fixture))
	decoder.UseNumber()
	if err := decoder.Decode(&want); err != nil {
		t.Fatal(err)
	}
	decoder = json.NewDecoder(strings.NewReader(string(encoded)))
	decoder.UseNumber()
	if err := decoder.Decode(&got); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("loan JSON round trip changed fields: got %s, want %s", encoded, fixture)
	}
}
