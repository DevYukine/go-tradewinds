package engine

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"testing"

	"github.com/DevYukine/go-tradewinds/internal/gameapi"
	"go.uber.org/zap"
)

// Fixtures are response bodies, not live-probe's capture envelopes. Empty
// shapes and the credit/future/error samples come from 2026-10-28 live-probe;
// populated new fields follow the published 0.5.1 schemas.
func decode051Preserves(t *testing.T, raw string, target any) {
	t.Helper()
	if err := json.Unmarshal([]byte(raw), target); err != nil {
		t.Fatalf("0.5.1 response refused: %v", err)
	}
	encoded, err := json.Marshal(target)
	if err != nil {
		t.Fatal(err)
	}
	decode := func(raw []byte) any {
		decoder := json.NewDecoder(bytes.NewReader(raw))
		decoder.UseNumber()
		var value any
		if err := decoder.Decode(&value); err != nil {
			t.Fatal(err)
		}
		return value
	}
	// Allow a DTO to include other optional fields, but every supplied field
	// must survive with its exact type, signed money, nullness and nesting.
	var check func(any, any, string)
	check = func(want, got any, path string) {
		switch want := want.(type) {
		case map[string]any:
			actual, ok := got.(map[string]any)
			if !ok {
				t.Errorf("%s decoded as %T, want object", path, got)
				return
			}
			for key, value := range want {
				observed, exists := actual[key]
				if !exists {
					t.Errorf("0.5.1 field %s/%s was dropped", path, key)
					continue
				}
				check(value, observed, path+"/"+key)
			}
		case []any:
			actual, ok := got.([]any)
			if !ok || len(actual) != len(want) {
				t.Errorf("%s array changed: %v", path, got)
				return
			}
			for i := range want {
				check(want[i], actual[i], path+"[]")
			}
		default:
			if !reflect.DeepEqual(want, got) {
				t.Errorf("%s=%v (%T), want %v (%T)", path, got, got, want, want)
			}
		}
	}
	check(decode([]byte(raw)), decode(encoded), "")
}

func TestUpdateDecode051LoanDepositCollateralAndUnknownKinds(t *testing.T) {
	base := map[string]any{"id": "11111111-1111-4111-8111-111111111111", "borrower_company_id": recoveryCompanyID, "lender_company_id": "22222222-2222-4222-8222-222222222222",
		"banking_region": "north_sea", "status": "active", "owed_pence": 15000, "outstanding_principal_pence": 14000, "arrears_pence": 0, "rate_basis": "annual", "annual_rate_bps": 1200,
		"collateral_kind": "deposit", "collateral_status": "pledged", "is_mortgage": false, "collateral_deposit_id": "33333333-3333-4333-8333-333333333333", "deposit_lock_pence": 16000,
		"repayment": "balloon", "next_payment_pence": 16000}
	for _, tc := range []struct {
		name    string
		kind    string
		deposit any
		remove  bool
		good    bool
	}{
		{"deposit_with_identity", "deposit", "33333333-3333-4333-8333-333333333333", false, true},
		{"missing_deposit_identity", "deposit", nil, true, false}, {"null_deposit_identity", "deposit", nil, false, false}, {"blank_deposit_identity", "deposit", "", false, false},
		{"unknown_kind_stays_closed", "future_deposit", "33333333-3333-4333-8333-333333333333", false, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			row := make(map[string]any, len(base))
			for k, v := range base {
				row[k] = v
			}
			row["collateral_kind"], row["collateral_deposit_id"] = tc.kind, tc.deposit
			if tc.remove {
				delete(row, "collateral_deposit_id")
			}
			raw, err := json.Marshal([]any{row})
			if err != nil {
				t.Fatal(err)
			}
			err = validateBorrowerLoans(raw, recoveryCompanyID)
			if (err == nil) != tc.good {
				t.Fatalf("deposit collateral authority good=%t: %v", tc.good, err)
			}
			if tc.good {
				single, _ := json.Marshal(row)
				decode051Preserves(t, string(single), &gameapi.Loan{})
			}
		})
	}
}

func TestUpdateDecode051LeaderboardLiveEmptyAndPopulatedRising(t *testing.T) {
	const live = `{"entries":[{"company_name":"The Kraken Cartel","home_port_id":"london","net_worth_pence":445504624,"rank":1,"titles":[]}],"next_refresh_at":"2026-10-28T16:28:22.612048Z","ranked_players":9,"refreshed_at":"2026-10-28T15:28:22.98261Z","rising":{"entries":[],"ranked_players":0,"window_start_at":null,"you":null},"season_number":1,"you":{"company_name":"The Pokemon Pirates","is_calling_company":true,"net_worth_pence":10461138,"rank":5}}`
	decode051Preserves(t, live, &gameapi.Leaderboard{})
	const populated = `{"season_number":1,"ranked_players":9,"entries":[],"rising":{"entries":[{"rank":1,"company_name":"Growing Company","home_port_id":"amsterdam","growth_bps":12500,"gained_pence":125000000,"window_start_at":"2026-10-21T15:28:22Z"},{"rank":2,"company_name":"Losing Company","home_port_id":"london","growth_bps":-2500,"gained_pence":-25000000,"window_start_at":"2026-10-22T15:28:22Z"}],"ranked_players":2,"window_start_at":"2026-10-21T15:28:22Z","you":{"rank":2,"company_name":"Losing Company","growth_bps":-2500,"gained_pence":-25000000,"window_start_at":"2026-10-22T15:28:22Z","is_calling_company":false}}}`
	decode051Preserves(t, populated, &gameapi.Leaderboard{})
}

func TestUpdateDecode051BankExchangeAndPledgedDeposit(t *testing.T) {
	const bank = `{"chartered_at":"2026-10-28T14:57:24.008675Z","company_id":"01a16469-7564-711d-b8da-e69d7d73e623","demand_rate_bps":1,"deposits_pence":0,"exchange_fee_bps":{},"failed_at":null,"home_region":"channel","insured":true,"lending_headroom_pence":51208422,"loans_out_pence":36073411,"name":"The Kraken Cartel","notice_game_hours":240,"notice_rate_bps":5,"overdue_withdrawals_pence":0,"required_reserve_pence":0,"reserves_pence":51208422,"status":"open","underwriting_locked_pence":0}`
	var banks []gameapi.BankResult
	decode051Preserves(t, "["+bank+"]", &banks)
	decode051Preserves(t, strings.Replace(bank, `"exchange_fee_bps":{}`, `"exchange_fee_bps":{"north_sea":1200,"america":0}`, 1), &gameapi.BankResult{})
	const deposit = `{"id":"11111111-1111-4111-8111-111111111111","bank_company_id":"22222222-2222-4222-8222-222222222222","bank_name":"Bank","depositor_company_id":"33333333-3333-4333-8333-333333333333","banking_region":"channel","balance_pence":100000,"deposited_pence":110000,"interest_credited_pence":100,"withdrawn_pence":10000,"insured_paid_pence":0,"written_off_pence":0,"daily_rate_bps":1,"notice_game_hours":240,"pending_withdrawal_pence":5000,"pledged_pence":70000,"set_off_pence":100,"exchange_fees_pence":25,"status":"open","created_at":"2026-10-28T14:00:00Z"}`
	decode051Preserves(t, deposit, &gameapi.DepositResult{})
	decode051Preserves(t, `{"deposit_id":"11111111-1111-4111-8111-111111111111","amount_pence":25000,"to_region":"north_sea"}`, &gameapi.WithdrawRequest{})
}

func TestUpdateDecode051CreditReportAllTwentyOneFields(t *testing.T) {
	const live = `{"active_loans":0,"active_owed_pence":0,"bankrupt_at":null,"company_id":"01a16469-7564-711d-b8da-e69d7d73e623","company_name":"The Kraken Cartel","defaulted_open_loans":0,"defaulted_owed_pence":0,"futures_claims_open_pence":0,"futures_claims_total":0,"futures_claims_written_off":0,"last_conviction_at":"2026-10-28T12:16:03.920853Z","last_default_at":null,"lent_outstanding_pence":35323079,"loan_walked_away_pence":0,"loan_written_off_pence":0,"loans_defaulted":0,"loans_repaid":0,"loans_walked_away":0,"loans_written_off":0,"open_mortgages":0,"smuggling_convictions":2}`
	decode051Preserves(t, live, &gameapi.CreditReport{})
	const populated = `{"company_id":"01a16469-7564-711d-b8da-e69d7d73e623","company_name":"Credit Company","active_loans":2,"active_owed_pence":2147483649,"bankrupt_at":"2026-10-21T12:00:00Z","defaulted_open_loans":3,"defaulted_owed_pence":40000,"futures_claims_open_pence":50000,"futures_claims_total":6,"futures_claims_written_off":7,"last_conviction_at":"2026-10-22T12:00:00Z","last_default_at":"2026-10-23T12:00:00Z","lent_outstanding_pence":80000,"loan_walked_away_pence":90000,"loan_written_off_pence":100000,"loans_defaulted":11,"loans_repaid":12,"loans_walked_away":13,"loans_written_off":14,"open_mortgages":1,"smuggling_convictions":16}`
	decode051Preserves(t, populated, &gameapi.CreditReport{})
}

func TestUpdateDecode051FuturesListAndSettlement(t *testing.T) {
	const listed = `[{"buyer_company_id":null,"buyer_margin_pence":0,"commodity_id":"grain","created_at":"2026-10-28T14:01:29.581271Z","creator_company_id":"01a172d0-23fa-7280-9bc0-5048df3a8773","id":"01a182d0-e0ee-70ab-821a-9d571636ea13","lot_tons":10,"matures_at":"2026-10-28T16:01:29.585632Z","port_id":"london","seller_company_id":"01a172d0-23fa-7280-9bc0-5048df3a8773","seller_margin_pence":22382,"side":"ask","status":"open","strike_price_pence":8953,"updated_at":"2026-10-28T14:01:29.581271Z"}]`
	var contracts []gameapi.FuturesContract
	decode051Preserves(t, listed, &contracts)
	decode051Preserves(t, `{"futures_id":"01a182d0-e0ee-70ab-821a-9d571636ea13","status":"settled","settled_tons":1,"penalty_paid_pence":0,"claim_pence":0}`, &gameapi.SettleFuturesResult{})
	decode051Preserves(t, `{"futures_id":"01a182d0-e0ee-70ab-821a-9d571636ea13","status":"defaulted","settled_tons":0,"penalty_paid_pence":22382,"claim_pence":123456}`, &gameapi.SettleFuturesResult{})
}

func TestUpdateDecode051UnderworldWantsBookAnd403Problem(t *testing.T) {
	decode051Preserves(t, `{"port_id":"amsterdam","is_cove":false,"wants":[{"commodity_id":"untaxed_tea","multiple_bps":22500,"remaining_tons":7}],"board":[]}`, &gameapi.WantsResult{})
	decode051Preserves(t, `{"port_id":"flushing","is_cove":true,"wants":[],"board":[{"port_id":"amsterdam","commodity_id":"untaxed_tea","multiple_bps":22500,"observed_at":"2026-10-28T15:00:00Z","age_game_hours":1}]}`, &gameapi.WantsResult{})
	decode051Preserves(t, `{"commodity_id":"untaxed_tea","house_ask":{"price_pence":31000,"quantity":3},"bids":[{"price_pence":29000,"quantity":1}],"asks":[]}`, &gameapi.CoveBook{})
	decode051Preserves(t, `{"commodity_id":"grog","house_ask":null,"bids":[],"asks":[]}`, &gameapi.CoveBook{})
	const body = `{"code":"missing_scope","detail":"API key lacks required scope: 'underworld'. Check GET /v1/auth/whoami or issue a key with this scope via POST /v1/auth/create-key","status":403,"title":"Forbidden","type":"urn:tradewinds:error:missing_scope"}`
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/problem+json")
		w.WriteHeader(403)
		_, _ = w.Write([]byte(body))
	}))
	defer server.Close()
	client := gameapi.New(server.URL, "test-main-key", zap.NewNop())
	var wants gameapi.WantsResult
	err := client.Get(context.Background(), "/v1/underworld/wants?port_id=amsterdam", &wants)
	var problem *gameapi.APIError
	if !errors.As(err, &problem) || problem.Status != 403 || problem.Code != "missing_scope" || !strings.Contains(problem.Detail, "underworld") {
		t.Fatalf("403 problem was lost or decoded as a successful empty want: %+v", err)
	}
}
