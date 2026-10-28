package engine

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"reflect"
	"sync"
	"testing"
	"time"

	"github.com/DevYukine/go-tradewinds/internal/gameapi"
	"github.com/DevYukine/go-tradewinds/internal/store"
	"go.uber.org/zap"
)

func snapshotTestLoan() gameapi.Loan {
	now := time.Date(2026, 10, 26, 12, 0, 0, 0, time.UTC)
	due := now.Add(4 * time.Hour)
	maturity := now.Add(6 * 24 * time.Hour)
	annual := int32(3000)
	installment := int64(24_200)
	total := int64(871_200)
	shipID := "collateral-ship"
	return gameapi.Loan{
		ID: "loan-one", LenderCompanyID: "goldsmiths", BorrowerCompanyID: recoveryCompanyID,
		BankingRegion: "north_sea", PrincipalPence: 750_000, RateBasis: "annual",
		AnnualRateBPS: &annual, TermGameDays: 360, Repayment: "amortised",
		TotalRepayablePence: &total, InstallmentPence: &installment,
		NextPaymentDueAt: &due, NextPaymentPence: &installment,
		Recourse: true, CollateralKind: "ship_lien", CollateralShipID: &shipID,
		CollateralValuePence: 2_000_000, CollateralStatus: "pledged", Status: "active",
		OutstandingPrincipalPence: 750_000, OwedPence: 750_000,
		OfferExpiresAt: now, DisbursedAt: &now, MaturesAt: &maturity, CreatedAt: now, UpdatedAt: now,
	}
}

func TestLoanCaptureRejectsUnknownAuthorityAndRetainsLastGood(t *testing.T) {
	t.Parallel()
	good := encode([]gameapi.Loan{snapshotTestLoan()})
	cases := map[string]json.RawMessage{
		"null list":      json.RawMessage("null"),
		"malformed list": json.RawMessage(`{"loans":[]}`),
		"null row":       json.RawMessage(`[null]`),
		"malformed row":  json.RawMessage(`[42]`),
		"partial row":    json.RawMessage(`[{"id":"loan-one"}]`),
	}
	for _, field := range []string{
		"id", "borrower_company_id", "banking_region", "status", "outstanding_principal_pence",
		"owed_pence", "arrears_pence", "rate_basis", "annual_rate_bps", "repayment",
		"installment_pence", "next_payment_pence", "collateral_kind", "collateral_status", "collateral_ship_id", "is_mortgage",
	} {
		for _, missing := range []bool{false, true} {
			var rows []map[string]any
			if err := json.Unmarshal(good, &rows); err != nil {
				t.Fatal(err)
			}
			if missing {
				delete(rows[0], field)
			} else {
				rows[0][field] = nil
			}
			cases[fmt.Sprintf("%s absent=%t", field, missing)] = encode(rows)
		}
	}
	for name, raw := range cases {
		t.Run(name, func(t *testing.T) {
			runner := &Runner{
				account: store.Account{CompanyID: recoveryCompanyID}, log: zap.NewNop(),
				snapshot: map[string]json.RawMessage{"loans": good}, meta: make(map[string]DatasetMeta), generation: "capture",
			}
			now := time.Now()
			if runner.applyCapture(context.Background(), "loans", fetchResult{data: raw, attemptedAt: now, completedAt: now}) {
				t.Fatal("unknown loan authority was accepted")
			}
			meta := runner.meta["loans"]
			if !meta.Stale || meta.Complete || meta.Error == "" || !bytes.Equal(runner.snapshot["loans"], good) {
				t.Fatalf("failed capture replaced last-good debt or hid its failure: %+v", meta)
			}
			if state := runner.state(); len(state.Loans) != 1 || state.Loans[0].OwedPence != 750_000 {
				t.Fatalf("failed capture discarded last-good debt: %+v", state.Loans)
			}
			if !runner.applyCapture(context.Background(), "loans", fetchResult{data: json.RawMessage("[]"), attemptedAt: now, completedAt: now}) {
				t.Fatal("complete empty loan book failed to recover authority")
			}
			if meta := runner.meta["loans"]; meta.Stale || !meta.Complete || meta.Error != "" || len(runner.state().Loans) != 0 {
				t.Fatalf("successful empty capture retained debt or failure: %+v", meta)
			}
		})
	}
}

func TestLoanValidationAcceptsClosedAndLegacyDebtButRejectsUnknownTerms(t *testing.T) {
	t.Parallel()
	for _, status := range []string{"offered", "active", "repaid", "defaulted", "recovered", "written_off", "withdrawn", "declined", "lapsed"} {
		loan := snapshotTestLoan()
		loan.Status = status
		if status != "active" {
			loan.NextPaymentDueAt = nil
			loan.NextPaymentPence = nil
		}
		if err := validateBorrowerLoans(encode([]gameapi.Loan{loan}), recoveryCompanyID); err != nil {
			t.Fatalf("documented status %s was discarded: %v", status, err)
		}
	}
	legacy := snapshotTestLoan()
	legacy.RateBasis = "daily"
	legacy.DailyRateBPS = 40
	legacy.AnnualRateBPS = nil
	legacy.Repayment = "balloon"
	legacy.InstallmentPence = nil
	legacy.TotalRepayablePence = nil
	if err := validateBorrowerLoans(encode([]gameapi.Loan{legacy}), recoveryCompanyID); err != nil {
		t.Fatalf("documented legacy daily loan was discarded: %v", err)
	}
	for name, change := range map[string]func(*gameapi.Loan){
		"wrong borrower":        func(loan *gameapi.Loan) { loan.BorrowerCompanyID = "other-company" },
		"unknown status":        func(loan *gameapi.Loan) { loan.Status = "future-status" },
		"unknown region":        func(loan *gameapi.Loan) { loan.BankingRegion = "unknown-region" },
		"unknown rate":          func(loan *gameapi.Loan) { loan.RateBasis = "monthly" },
		"unknown repayment":     func(loan *gameapi.Loan) { loan.Repayment = "unknown" },
		"negative debt":         func(loan *gameapi.Loan) { loan.OwedPence = -1 },
		"negative arrears":      func(loan *gameapi.Loan) { loan.ArrearsPence = -1 },
		"negative principal":    func(loan *gameapi.Loan) { loan.OutstandingPrincipalPence = -1 },
		"negative interest":     func(loan *gameapi.Loan) { *loan.AnnualRateBPS = -1 },
		"negative installment":  func(loan *gameapi.Loan) { *loan.InstallmentPence = -1 },
		"negative next payment": func(loan *gameapi.Loan) { *loan.NextPaymentPence = -1 },
	} {
		t.Run(name, func(t *testing.T) {
			loan := snapshotTestLoan()
			change(&loan)
			if err := validateBorrowerLoans(encode([]gameapi.Loan{loan}), recoveryCompanyID); err == nil {
				t.Fatal("unknown or invalid essential loan terms were accepted")
			}
		})
	}
}

func TestLoanValidationDoesNotRequireUnusedAccountingOrLifecycleMetadata(t *testing.T) {
	t.Parallel()
	for _, missing := range []bool{false, true} {
		var rows []map[string]any
		if err := json.Unmarshal(encode([]gameapi.Loan{snapshotTestLoan()}), &rows); err != nil {
			t.Fatal(err)
		}
		for _, field := range []string{
			"lender_company_id", "principal_pence", "daily_rate_bps", "term_game_days", "total_repayable_pence",
			"recourse", "collateral_value_pence", "accrued_interest_pence", "principal_repaid_pence",
			"interest_paid_pence", "collateral_credit_pence", "extinguished_pence", "written_off_pence",
			"foreclosure_surplus_paid_pence", "foreclosure_surplus_owed_pence", "offer_expires_at",
			"disbursed_at", "next_payment_due_at", "arrears_due_by", "matures_at", "defaulted_at",
			"closed_at", "interest_accrued_through", "created_at", "updated_at",
		} {
			if missing {
				delete(rows[0], field)
			} else {
				rows[0][field] = nil
			}
		}
		rows[0]["arrears_pence"] = 100
		if err := validateBorrowerLoans(encode(rows), recoveryCompanyID); err != nil {
			t.Fatalf("unused metadata absent=%t removed debt authority: %v", missing, err)
		}
	}
	closed := json.RawMessage(`[{
		"id":"closed-loan","borrower_company_id":"` + recoveryCompanyID + `","banking_region":"north_sea",
		"status":"repaid","owed_pence":0,"arrears_pence":0,"is_mortgage":false,
		"collateral_kind":"ship_lien","collateral_status":"released"
	}]`)
	if err := validateBorrowerLoans(closed, recoveryCompanyID); err != nil {
		t.Fatalf("closed history required unused payment or interest terms: %v", err)
	}
}

func TestLoanValidationRequiresOnlyApplicableInterestAndPledgedShipIdentity(t *testing.T) {
	t.Parallel()
	legacy := snapshotTestLoan()
	legacy.RateBasis = "daily"
	legacy.DailyRateBPS = 40
	legacy.AnnualRateBPS = nil
	for _, missing := range []bool{false, true} {
		var rows []map[string]any
		if err := json.Unmarshal(encode([]gameapi.Loan{legacy}), &rows); err != nil {
			t.Fatal(err)
		}
		if missing {
			delete(rows[0], "daily_rate_bps")
		} else {
			rows[0]["daily_rate_bps"] = nil
		}
		if err := validateBorrowerLoans(encode(rows), recoveryCompanyID); err == nil {
			t.Fatalf("unknown legacy daily interest absent=%t was accepted", missing)
		}
	}
	for _, status := range []string{"pledged", "held", "seizure_pending", "released", "seized", "lost"} {
		loan := snapshotTestLoan()
		loan.Status = "defaulted"
		loan.CollateralStatus = status
		loan.CollateralShipID = nil
		err := validateBorrowerLoans(encode([]gameapi.Loan{loan}), recoveryCompanyID)
		needsShip := status == "pledged" || status == "held" || status == "seizure_pending"
		if (err != nil) != needsShip {
			t.Fatalf("collateral status %s: err %v, needs ship identity %t", status, err, needsShip)
		}
	}
}

func snapshotLoanPage(status string) []gameapi.Loan {
	loans := make([]gameapi.Loan, 200)
	for index := range loans {
		loans[index] = snapshotTestLoan()
		loans[index].ID = fmt.Sprintf("%s-%03d", status, index)
		loans[index].Status = status
	}
	return loans
}

func TestLoanFilteredCaptureKeepsOrderedPagesStatusesAndNewestDuplicate(t *testing.T) {
	t.Parallel()
	var mu sync.Mutex
	var queries []string
	runner := newRetentionRunner(t, http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		query := request.URL.Query()
		if request.URL.Path != "/v1/loan/list" || query.Get("role") != "borrower" || query.Get("limit") != "200" {
			t.Errorf("incorrect borrower pagination query: %s", request.URL)
		}
		status, cursor := query.Get("status"), query.Get("cursor")
		mu.Lock()
		queries = append(queries, status+":"+cursor)
		mu.Unlock()
		loans := []gameapi.Loan{}
		switch {
		case (status == "active" || status == "defaulted") && cursor == "":
			loans = snapshotLoanPage(status)
			if status == "defaulted" {
				loans[0].ID = "active-000"
			}
		case status == "active" && cursor == "active-199", status == "defaulted" && cursor == "defaulted-199":
			loan := snapshotTestLoan()
			loan.ID, loan.Status = status+"-tail", status
			loans = append(loans, loan)
		default:
			t.Errorf("unexpected borrower pagination query: %s", request.URL)
		}
		_ = json.NewEncoder(writer).Encode(loans)
	}))
	runner.account.CompanyID = recoveryCompanyID
	if err := runner.captureWave(context.Background(), []fetchTask{{key: "loans", kind: fetchBorrowerLoans}}, nil); err != nil {
		t.Fatal(err)
	}
	mu.Lock()
	gotQueries := append([]string(nil), queries...)
	mu.Unlock()
	wantQueries := []string{"active:", "active:active-199", "defaulted:", "defaulted:defaulted-199"}
	if !reflect.DeepEqual(gotQueries, wantQueries) {
		t.Fatalf("borrower status/page order = %v, want %v", gotQueries, wantQueries)
	}
	loans := runner.state().Loans
	if len(loans) != 401 || loans[0].ID != "active-000" || loans[0].Status != "defaulted" ||
		loans[200].ID != "active-tail" || loans[400].ID != "defaulted-tail" || !runner.fresh("loans") {
		t.Fatalf("filtered borrower capture lost debt, duplicated a changing status or lost authority: %d %+v", len(loans), runner.meta["loans"])
	}
}

func TestLoanFilteredCaptureFailureRetainsWholeLastGoodAuthority(t *testing.T) {
	t.Parallel()
	for _, failure := range []string{"active unavailable", "defaulted unavailable", "defaulted tail unavailable", "defaulted null", "defaulted missing debt"} {
		t.Run(failure, func(t *testing.T) {
			runner := newRetentionRunner(t, http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
				if request.URL.Path == "/v1/world/zones" {
					_, _ = writer.Write([]byte(`[{"id":"new-zone"}]`))
					return
				}
				status, cursor := request.URL.Query().Get("status"), request.URL.Query().Get("cursor")
				switch {
				case failure == status+" unavailable", failure == "defaulted tail unavailable" && status == "defaulted" && cursor != "":
					writer.WriteHeader(http.StatusNotFound)
					_, _ = writer.Write([]byte(`{"code":"page_unavailable"}`))
				case failure == "defaulted tail unavailable" && status == "defaulted":
					_ = json.NewEncoder(writer).Encode(snapshotLoanPage("defaulted"))
				case failure == "defaulted null" && status == "defaulted":
					_, _ = writer.Write([]byte("null"))
				case failure == "defaulted missing debt" && status == "defaulted":
					_, _ = writer.Write([]byte(`[{"id":"partial-defaulted"}]`))
				default:
					loan := snapshotTestLoan()
					loan.ID, loan.Status = status+"-only", status
					_ = json.NewEncoder(writer).Encode([]gameapi.Loan{loan})
				}
			}))
			runner.account.CompanyID = recoveryCompanyID
			lastGood := encode([]gameapi.Loan{snapshotTestLoan()})
			observed := time.Now().Add(-time.Minute)
			prior := DatasetMeta{UpdatedAt: observed, AttemptedAt: observed, Generation: "before", Complete: true}
			runner.snapshot["loans"], runner.meta["loans"] = lastGood, prior
			if err := runner.captureWave(context.Background(), []fetchTask{
				{key: "loans", kind: fetchBorrowerLoans},
				{key: "zones", path: "/v1/world/zones"},
			}, nil); err != nil {
				t.Fatal(err)
			}
			failed := runner.meta["loans"]
			if !bytes.Equal(runner.snapshot["loans"], lastGood) || !failed.UpdatedAt.Equal(prior.UpdatedAt) || failed.Generation != prior.Generation ||
				!failed.AttemptedAt.After(prior.UpdatedAt) || !failed.Stale || failed.Complete || failed.Error == "" {
				t.Fatalf("partial borrower capture replaced last-good debt authority: %+v", failed)
			}
			if !runner.fresh("zones") {
				t.Fatal("failed borrower capture hid the independent successful dataset")
			}
		})
	}
}

func TestLoanHistoryCapturePaginatesClosedReconciliationRows(t *testing.T) {
	t.Parallel()
	runner := newRetentionRunner(t, http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		query := request.URL.Query()
		if request.URL.Path != "/v1/loan/list" || query.Get("role") != "borrower" || query.Get("limit") != "200" || query.Get("status") != "" {
			t.Errorf("incorrect borrower history pagination query: %s", request.URL)
		}
		if query.Get("cursor") == "" {
			_ = json.NewEncoder(writer).Encode(snapshotLoanPage("active"))
			return
		}
		if query.Get("cursor") != "active-199" {
			t.Errorf("wrong history-page cursor: %s", request.URL)
		}
		loan := snapshotTestLoan()
		loan.ID, loan.Status = "closed-tail", "repaid"
		_ = json.NewEncoder(writer).Encode([]gameapi.Loan{loan})
	}))
	runner.account.CompanyID = recoveryCompanyID
	if err := runner.captureWave(context.Background(), []fetchTask{{key: "loans", path: "/v1/loan/list?role=borrower", kind: fetchPages}}, nil); err != nil {
		t.Fatal(err)
	}
	if loans := runner.state().Loans; len(loans) != 201 || loans[200].Status != "repaid" || !runner.fresh("loans") {
		t.Fatalf("full borrower history discarded closed reconciliation rows: %d %+v", len(loans), runner.meta["loans"])
	}
}

func TestLoanCaptureTaskFetchesHistoryOnlyForOpenLoanWork(t *testing.T) {
	for _, test := range []struct {
		name        string
		shipID      string
		data        missionData
		pendingPath string
		history     bool
	}{
		{name: "ordinary capture"},
		{name: "ordinary growth", shipID: "__growth", data: missionData{Capacity: &capacityIntent{}}},
		{name: "ordinary treasury", shipID: "__treasury", data: missionData{Plan: Plan{Kind: "treasury"}}},
		{name: "unrelated pending action", pendingPath: "/v1/treasury/remit"},
		{name: "capacity financing", shipID: "__growth", data: missionData{Capacity: &capacityIntent{Financing: &capacityFinancing{}}}, history: true},
		{name: "treasury repayment", shipID: "__treasury", data: missionData{LoanRepayment: &loanRepaymentIntent{}}, history: true},
		{name: "priority arrears repayment", shipID: "__loans", data: missionData{LoanRepayment: &loanRepaymentIntent{}}, history: true},
		{name: "staged loan action", shipID: "__growth", data: missionData{Action: &durableAction{Path: "/v1/loan/accept-quote"}}, history: true},
		{name: "orphan pending loan action", pendingPath: "/v1/loan/repay", history: true},
	} {
		t.Run(test.name, func(t *testing.T) {
			h := newRecoveryHarness(t)
			runner := h.runner(primaryTestKey)
			if test.shipID != "" {
				if err := h.db.PutMission(h.ctx, &store.Mission{
					AccountID: h.account.ID, ShipID: test.shipID, Phase: "capture-test", Generation: "loan-capture", Data: encode(test.data),
				}); err != nil {
					t.Fatal(err)
				}
			}
			const actionID = "loan-history-capture-action"
			if test.pendingPath != "" {
				if _, err := h.db.BeginAction(h.ctx, &store.Action{
					ID: actionID, AccountID: h.account.ID, Method: http.MethodPost, Path: test.pendingPath, Body: []byte(`{}`),
				}); err != nil {
					t.Fatal(err)
				}
			}
			task, err := runner.loanCaptureTask(h.ctx)
			if err != nil {
				t.Fatal(err)
			}
			wantKind := fetchBorrowerLoans
			if test.history {
				wantKind = fetchPages
			}
			if task.key != "loans" || task.kind != wantKind || test.history && task.path != "/v1/loan/list?role=borrower" {
				t.Fatalf("wrong borrower capture scope: %+v, want history %t", task, test.history)
			}
			if test.shipID != "" {
				if err := h.db.DeleteMission(h.ctx, h.account.ID, test.shipID); err != nil {
					t.Fatal(err)
				}
			}
			if test.pendingPath != "" {
				if err := h.db.CompleteAction(h.ctx, actionID, http.StatusOK, json.RawMessage(`{}`), nil); err != nil {
					t.Fatal(err)
				}
			}
			if task, err := runner.loanCaptureTask(h.ctx); err != nil || task.kind != fetchBorrowerLoans {
				t.Fatalf("closed loan work retained unbounded history capture: task %+v error %v", task, err)
			}
		})
	}
}

func TestLoanFinancialEventsFencePreEventFirstSend(t *testing.T) {
	for _, eventType := range []string{
		"loan_disbursed", "loan_repayment", "loan_defaulted", "loan_collateral_seized",
		"loan_written_off", "loan_foreclosure_surplus_paid", "loan_insurance_loss_paid",
	} {
		t.Run(eventType, func(t *testing.T) {
			h := newRecoveryHarness(t)
			h.api.setVaultBalance("north", 500_000)
			h.api.setVaultBalance("south", 100_000)
			h.seedLaneProofs()
			runner := h.runner(primaryTestKey)
			stream := interceptEventStream(t, h.api, nil, 2)
			stream.frameFor = func(index int32) progressFrame {
				if index == 1 {
					return progressFrame{id: "loan-financial-event", eventType: eventType}
				}
				return progressFrame{}
			}
			running := startRunner(t, runner, h.ctx)
			stream.waitReady(t, 2)
			running.cancel()
			if err := running.wait(t, 15*time.Second); err != nil && !errors.Is(err, context.Canceled) {
				t.Fatal(err)
			}
			if runner.publishedFinancial.Load() != 1 {
				t.Fatalf("%s did not publish the financial first-send barrier", eventType)
			}
			if runner.stagedFundsValid(&durableAction{FinancialSession: runner.financialSession, FinancialEpoch: 0}) {
				t.Fatal("pre-event action retained first-send authority")
			}
			h.missingMission("__growth")
			if h.api.mutationCount("/v1/shipyard/build-ship") != 0 {
				t.Fatal("loan event allowed a pre-event build to cross first-send")
			}
			for _, dataset := range []string{"loans", "vaults", "ships"} {
				if meta := runner.meta[dataset]; meta.UpdatedAt.IsZero() || !meta.Complete || meta.Stale {
					t.Fatalf("loan event did not retain authoritative %s capture: %+v", dataset, meta)
				}
			}
		})
	}
}
