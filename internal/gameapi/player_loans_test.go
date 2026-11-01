package gameapi_test

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/DevYukine/go-tradewinds/internal/gameapi"
)

func TestPlayerLoanRequestExactWireBody(t *testing.T) {
	for _, ship := range []string{"", "ship-1"} {
		body := gameapi.PostLoanRequest{
			BankingRegion: "north_sea", PrincipalPence: 106_900, TermGameDays: 360,
			Repayment: "amortised", CollateralKind: "ship_lien", ShipID: ship,
			Recourse: true, AutoQuote: true, OpenGameHours: 24,
		}
		if ship == "" {
			body.CollateralKind = "unsecured"
		}
		raw, err := json.Marshal(body)
		if err != nil {
			t.Fatal(err)
		}
		want := `{"banking_region":"north_sea","principal_pence":106900,"term_game_days":360,"repayment":"amortised","collateral_kind":"ship_lien","recourse":true,"auto_quote":true,"open_game_hours":24}`
		if ship != "" {
			want = strings.Replace(want, `"recourse"`, `"ship_id":"ship-1","recourse"`, 1)
		} else {
			want = strings.Replace(want, `"ship_lien"`, `"unsecured"`, 1)
		}
		assertLoanJSON(t, want, body)
		if ship == "" && strings.Contains(string(raw), "ship_id") {
			t.Fatal("absent ship id must not be transmitted")
		}
	}
	raw, err := json.Marshal(gameapi.PostLoanRequest{})
	if err != nil {
		t.Fatal(err)
	}
	for _, field := range []string{`"recourse":false`, `"auto_quote":false`, `"open_game_hours":0`} {
		if !strings.Contains(string(raw), field) {
			t.Fatalf("explicit request zero value %s disappeared: %s", field, raw)
		}
	}
}

const playerRequestJSON = `{
	"id":"request-1","borrower_company_id":"borrower-1","borrower_name":"Borrower",
	"borrower_record":{"company_id":"borrower-1","loans_repaid":2},
	"banking_region":"north_sea","principal_pence":106900,"term_game_days":360,
	"repayment":"amortised","recourse":true,"collateral_kind":"ship_lien",
	"collateral_ship_id":"ship-1","collateral_port_id":null,"collateral_commodity_id":null,
	"collateral_tons":null,"collateral_contract_id":null,"collateral_deposit_id":null,
	"collateral_deposit_bank_id":null,"goldsmiths_rate_bps":3000,"rate_floor_bps":300,
	"rate_ceiling_bps":9000,"auto_quote":true,"status":"open","loan_id":null,
	"created_at":"2026-10-31T12:00:00Z","expires_at":"2026-10-31T12:24:00Z",
	"quotes":[{
		"id":"quote-1","request_id":"request-1","auto":true,"lending_line_id":"line-1",
		"lender_company_id":"01a16469-7564-711d-b8da-e69d7d73e623","lender_name":"The Kraken Cartel",
		"borrower_company_id":"borrower-1","banking_region":"north_sea","principal_pence":106900,
		"annual_rate_bps":2000,"term_game_days":360,"repayment":"amortised",
		"total_repayable_pence":118800,"installment_pence":3300,"collateral_ship_id":null,
		"status":"open","expires_at":"2026-10-31T12:10:00Z","loan_id":null,
		"created_at":"2026-10-31T12:01:00Z"
	}]
}`

func TestPlayerLoanOwnRequestsCaptureExactScopeAndFields(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet || r.URL.RequestURI() != "/v1/loan/requests?mine=true&limit=50" {
			t.Errorf("own request capture scope changed: %s %s", r.Method, r.URL.RequestURI())
		}
		if r.Header.Get("Idempotency-Key") != "" {
			t.Error("GET request capture received a mutation key")
		}
		_, _ = io.WriteString(w, "["+playerRequestJSON+"]")
	}))
	defer server.Close()
	requests, err := newTestClient(t, server).ListOwnLoanRequests(context.Background())
	if err != nil || len(requests) != 1 {
		t.Fatalf("capture own requests: %+v, %v", requests, err)
	}
	assertLoanJSON(t, playerRequestJSON, requests[0])
	quote := requests[0].Quotes[0]
	if !quote.Auto || quote.LendingLineID == nil || *quote.LendingLineID != "line-1" || quote.CollateralShipID != nil {
		t.Fatalf("player quote auto line or null collateral was lost: %+v", quote)
	}
}

func TestPlayerLoanLendingLinePersistsInLoan(t *testing.T) {
	const raw = `{"id":"loan-1","request_id":"request-1","quote_id":"quote-1","lending_line_id":"line-1","is_mortgage":false}`
	var loan gameapi.Loan
	if err := json.Unmarshal([]byte(raw), &loan); err != nil {
		t.Fatal(err)
	}
	encoded, err := json.Marshal(loan)
	if err != nil {
		t.Fatal(err)
	}
	var restored gameapi.Loan
	if err := json.Unmarshal(encoded, &restored); err != nil {
		t.Fatal(err)
	}
	if restored.LendingLineID == nil || *restored.LendingLineID != "line-1" || restored.RequestID == nil || *restored.RequestID != "request-1" {
		t.Fatalf("loan lost its player request/line identity: %s", encoded)
	}
}

func TestPlayerLoanMutationsLeaveAmbiguousRetryToTheJournal(t *testing.T) {
	for _, path := range []string{"/v1/loan/request", "/v1/loan/approve"} {
		t.Run(path, func(t *testing.T) {
			var calls atomic.Int32
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				calls.Add(1)
				w.WriteHeader(http.StatusInternalServerError)
				_, _ = io.WriteString(w, `{"code":"unknown_outcome"}`)
			}))
			defer server.Close()
			err := newTestClient(t, server).Do(context.Background(), http.MethodPost, path, map[string]any{}, "immutable-loan-action", nil)
			if err == nil || calls.Load() != 1 {
				t.Fatalf("ambiguous loan action retried before REST reconciliation: calls=%d err=%v", calls.Load(), err)
			}
		})
	}
}
