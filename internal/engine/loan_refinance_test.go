package engine

import (
	"encoding/json"
	"io"
	"math"
	"net/http"
	"net/http/httptest"
	"reflect"
	"slices"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/DevYukine/go-tradewinds/internal/gameapi"
	"github.com/DevYukine/go-tradewinds/internal/store"
)

func loanRefinanceFixture(t *testing.T) (*recoveryHarness, *Runner) {
	t.Helper()
	h := newRecoveryHarness(t)
	now := time.Now().UTC()
	old := snapshotTestLoan()
	old.ID, old.CollateralKind, old.CollateralStatus = "refinance-old", "unsecured", "none"
	old.CollateralShipID = nil
	old.PrincipalPence, old.OutstandingPrincipalPence, old.OwedPence = 1_000_000, 1_000_000, 1_000_000
	old.InstallmentPence, old.NextPaymentPence = new(int64(100_000)), new(int64(100_000))
	old.MaturesAt, old.NextPaymentDueAt = new(now.Add(96*time.Hour)), new(now.Add(time.Hour))
	old.CreatedAt, old.UpdatedAt = now.Add(-48*time.Hour), now
	h.api.playerLoans = []gameapi.Loan{old}
	base := h.api.server.Config.Handler
	h.api.server.Config.Handler = http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		if req.Method == http.MethodGet && req.URL.Path == "/v1/world/clock" {
			clock := h.api.clockDocument()
			clock["calendar"].(map[string]any)["time_scale"] = 60
			h.api.writeJSON(w, http.StatusOK, clock)
			return
		}
		recorder := httptest.NewRecorder()
		base.ServeHTTP(recorder, req)
		body := recorder.Body.Bytes()
		if req.Method == http.MethodPost && req.URL.Path == "/v1/loan/approve" && recorder.Code == http.StatusOK {
			var loan gameapi.Loan
			if json.Unmarshal(body, &loan) != nil {
				t.Error("approval returned invalid loan JSON")
			}
			loan.MaturesAt, loan.NextPaymentDueAt = new(time.Now().UTC().Add(144*time.Hour)), new(time.Now().UTC().Add(4*time.Hour))
			h.api.mu.Lock()
			for index := range h.api.playerLoans {
				if h.api.playerLoans[index].ID == loan.ID {
					h.api.playerLoans[index] = loan
				}
			}
			h.api.mu.Unlock()
			body = encode(loan)
		}
		for key, values := range recorder.Header() {
			w.Header()[key] = values
		}
		w.WriteHeader(recorder.Code)
		_, _ = io.WriteString(w, strings.ReplaceAll(strings.ReplaceAll(string(body), `"north"`, `"north_sea"`), `"south"`, `"channel"`))
	})
	runner := h.runner(primaryTestKey)
	loanCapacityRefresh(t, h, runner)
	h.api.setVaultBalance("north", 99_999)
	h.api.setVaultBalance("south", 0)
	loanCapacityRefresh(t, h, runner)
	return h, runner
}

func loanRefinanceDrive(t *testing.T, h *recoveryHarness, runner *Runner) bool {
	t.Helper()
	mission, data, err := runner.loadMission(h.ctx, loanRefinanceMission)
	if err != nil {
		t.Fatal(err)
	}
	changed, err := runner.driveLoanRefinance(h.ctx, mission, &data)
	if err != nil {
		t.Fatal(err)
	}
	return changed
}

func loanRefinanceDispatch(t *testing.T, h *recoveryHarness, runner *Runner) {
	t.Helper()
	mission, data := h.mission(loanRefinanceMission)
	if data.Action == nil {
		t.Fatalf("refinance phase %s has no staged action", mission.Phase)
	}
	if _, err := runner.dispatch(h.ctx, mission, &data); err != nil {
		t.Fatal(err)
	}
}

func loanRefinancePosted(t *testing.T) (*recoveryHarness, *Runner) {
	t.Helper()
	h, runner := loanRefinanceFixture(t)
	if !loanRefinanceDrive(t, h, runner) {
		t.Fatal("cash-short mature loan did not stage a refinance")
	}
	loanRefinanceDispatch(t, h, runner)
	loanCapacityRefresh(t, h, runner)
	return h, runner
}

func loanRefinanceQuote(request gameapi.LoanRequestView, id string, rate int32, installment int64) gameapi.LoanQuote {
	quote := americaFlagshipTestQuote(request, "refinance-lender", "Refinance lender", id, rate)
	quote.BankingRegion = request.BankingRegion
	quote.InstallmentPence = &installment
	quote.TotalRepayablePence = installment * 36
	return quote
}

func loanRefinanceApprove(t *testing.T, h *recoveryHarness, runner *Runner) {
	t.Helper()
	h.api.mu.Lock()
	request := &h.api.loanRequests[0]
	request.Quotes = []gameapi.LoanQuote{loanRefinanceQuote(*request, "refinance-quote", 3_000, 70_000)}
	h.api.mu.Unlock()
	loanCapacityRefresh(t, h, runner)
	if !loanRefinanceDrive(t, h, runner) {
		t.Fatal("acceptable quote did not stage approval")
	}
	loanRefinanceDispatch(t, h, runner)
	loanCapacityRefresh(t, h, runner)
}

func TestLoanRefinanceExactRequestAndBorrowerSlot(t *testing.T) {
	h, runner := loanRefinancePosted(t)
	rows := h.api.mutationsFor("/v1/loan/request")
	want := gameapi.PostLoanRequest{BankingRegion: "north_sea", PrincipalPence: 1_100_000, TermGameDays: 360,
		Repayment: "amortised", CollateralKind: "unsecured", Recourse: true, AutoQuote: true, OpenGameHours: 72}
	var body gameapi.PostLoanRequest
	if len(rows) != 1 || json.Unmarshal(rows[0].Body, &body) != nil || body != want {
		t.Fatalf("refinance request = %+v, want %+v", rows, want)
	}
	state := decode[loanRefinanceState](runner.snapshot, "_loan_refinance")
	if state.OldLoanID != "refinance-old" || state.RequestID == "" || state.NewLoanID != "" || state.State != "wait" {
		t.Fatalf("request lost its durable identity: %+v", state)
	}
	runner = h.runner(primaryTestKey)
	loanCapacityRefresh(t, h, runner)
	loanRefinanceDrive(t, h, runner)
	if h.api.mutationCount("/v1/loan/request") != 1 {
		t.Fatal("restart duplicated the borrower request")
	}
	if changed, err := runner.serviceLoans(h.ctx); changed || err != nil {
		t.Fatalf("normal loan service deleted or blocked the waiting refinance: %v, %v", changed, err)
	}
	h.mission(loanRefinanceMission)
	if changed, err := runner.prepayLoans(h.ctx); changed || err != nil {
		t.Fatalf("surplus prepayment did not yield to the durable refinance: %v, %v", changed, err)
	}

	// Recover a missing idle control row by adopting its matching open request.
	if err := h.db.DeleteMission(h.ctx, h.account.ID, loanRefinanceMission); err != nil {
		t.Fatal(err)
	}
	runner = h.runner(primaryTestKey)
	loanCapacityRefresh(t, h, runner)
	loanRefinanceDrive(t, h, runner)
	_, data := h.mission(loanRefinanceMission)
	if data.PlayerLoan == nil || data.PlayerLoan.RequestID != state.RequestID || data.Action != nil || h.api.mutationCount("/v1/loan/request") != 1 {
		t.Fatalf("open request was not adopted without reposting: %+v", data)
	}
}

func TestLoanRefinanceRequestCashAndAnnuityBoundaries(t *testing.T) {
	for _, test := range []struct {
		name      string
		free      int64
		remaining int
		want      bool
	}{
		{name: "one penny below next installment", free: 99_999, remaining: 24, want: true},
		{name: "exact next installment", free: 100_000, remaining: 24},
		{name: "ordinary dip below three installments", free: 299_999, remaining: 24},
		{name: "fully affordable payoff", free: 2_000_000, remaining: 24},
		{name: "annuity above ninety percent", free: 0, remaining: 32},
		{name: "fresh thirty-six-payment loan", free: 0, remaining: 36},
		{name: "thirty-one payments qualifies", free: 0, remaining: 31, want: true},
	} {
		t.Run(test.name, func(t *testing.T) {
			h, runner := loanRefinanceFixture(t)
			h.api.setVaultBalance("north", test.free)
			h.api.mu.Lock()
			h.api.playerLoans[0].MaturesAt = new(time.Now().UTC().Add(time.Duration(test.remaining)*4*time.Hour - time.Minute))
			h.api.mu.Unlock()
			loanCapacityRefresh(t, h, runner)
			if changed := loanRefinanceDrive(t, h, runner); changed != test.want {
				t.Fatalf("request staged = %v, want %v", changed, test.want)
			}
			if !test.want {
				h.missingMission(loanRefinanceMission)
			}
		})
	}
}

func TestLoanRefinanceApprovalUsesIdenticalInstallmentAndRateGates(t *testing.T) {
	for _, test := range []struct {
		name        string
		rate        int32
		installment *int64
		want        bool
	}{
		{name: "exact installment and rate limits", rate: 6_000, installment: new(int64(90_000)), want: true},
		{name: "installment one penny too high", rate: 3_000, installment: new(int64(90_001))},
		{name: "rate one basis point too high", rate: 6_001, installment: new(int64(70_000))},
		{name: "unknown installment", rate: 3_000},
	} {
		t.Run(test.name, func(t *testing.T) {
			h, runner := loanRefinancePosted(t)
			h.api.mu.Lock()
			request := &h.api.loanRequests[0]
			quote := loanRefinanceQuote(*request, "gate-quote", test.rate, 70_000)
			quote.InstallmentPence = test.installment
			request.Quotes = []gameapi.LoanQuote{quote}
			h.api.mu.Unlock()
			loanCapacityRefresh(t, h, runner)
			changed := loanRefinanceDrive(t, h, runner)
			if changed != test.want {
				t.Fatalf("quote accepted = %v, want %v", changed, test.want)
			}
			_, data := h.mission(loanRefinanceMission)
			if !test.want && data.Action != nil {
				t.Fatal("rejected quote escaped the selection gate")
			}
			if test.want {
				mission, data := h.mission(loanRefinanceMission)
				if err := runner.newActionPreflight(h.ctx, mission, &data); err != nil {
					t.Fatalf("accepted quote was vetoed at first send: %v", err)
				}
				runner.stateCache.LoanRequests[0].Quotes[0].AnnualRateBPS++
				if err := runner.newActionPreflight(h.ctx, mission, &data); err == nil {
					t.Fatal("first-send approval ignored the changed rate cap")
				}
			}
		})
	}
}

func TestLoanRefinanceSelectsLowestAcceptableRate(t *testing.T) {
	h, runner := loanRefinancePosted(t)
	h.api.mu.Lock()
	request := &h.api.loanRequests[0]
	request.Quotes = []gameapi.LoanQuote{
		loanRefinanceQuote(*request, "expensive", 6_000, 70_000),
		loanRefinanceQuote(*request, "bad-installment", 1_000, 90_001),
		loanRefinanceQuote(*request, "cheapest", 2_000, 80_000),
	}
	h.api.mu.Unlock()
	loanCapacityRefresh(t, h, runner)
	loanRefinanceDrive(t, h, runner)
	_, data := h.mission(loanRefinanceMission)
	if data.Action == nil || string(data.Action.Body) != `{"quote_id":"cheapest"}` {
		t.Fatalf("did not choose the lowest acceptable rate: %+v", data.Action)
	}
}

func TestLoanRefinanceEarmarkFullPayoffExactlyOnceAcrossRestart(t *testing.T) {
	for _, loseMission := range []bool{false, true} {
		t.Run(map[bool]string{false: "durable mission", true: "restored from snapshot"}[loseMission], func(t *testing.T) {
			h, runner := loanRefinancePosted(t)
			loanRefinanceApprove(t, h, runner)
			state := decode[loanRefinanceState](runner.snapshot, "_loan_refinance")
			old, _ := runner.loanByID(state.OldLoanID)
			newLoan, exists := runner.loanByID(state.NewLoanID)
			if !exists || state.State != "repay" || newLoan.ID == old.ID {
				t.Fatalf("approval did not durably name both loans: %+v", state)
			}
			with := obligationReserves(runner.stateCache, runner.snapshot, runner.protectedReservePence())
			withoutSnapshot := make(map[string]json.RawMessage)
			for key, raw := range runner.snapshot {
				if key != "_loan_refinance" {
					withoutSnapshot[key] = raw
				}
			}
			without := obligationReserves(runner.stateCache, withoutSnapshot, runner.protectedReservePence())
			if with[old.BankingRegion]-without[old.BankingRegion] != loanFullPayment(old)-loanReserve(old) {
				t.Fatal("refinance proceeds are not earmarked above the old installment reservation")
			}
			// Cash before borrowing covered only the old two-installment reserve;
			// the new reserve and earmark must not veto paying the old loan off.
			input, err := runner.loanRefinanceRepaymentInput(h.ctx, state, old)
			if err != nil {
				t.Fatal(err)
			}
			h.api.setVaultBalance("north", input.ReserveByRegion[old.BankingRegion]+loanFullPayment(old))
			if loseMission {
				if err := h.db.DeleteMission(h.ctx, h.account.ID, loanRefinanceMission); err != nil {
					t.Fatal(err)
				}
			}
			runner = h.runner(primaryTestKey)
			loanCapacityRefresh(t, h, runner)
			if !loanRefinanceDrive(t, h, runner) {
				t.Fatal("restart did not stage payoff from the existing refinance")
			}
			mission, data := h.mission(loanRefinanceMission)
			if data.Action == nil || data.Action.Stage != "loan-repay" {
				t.Fatalf("restart did not prioritize the old loan payoff: %+v", data.Action)
			}
			var body map[string]json.RawMessage
			if json.Unmarshal(data.Action.Body, &body) != nil || len(body) != 1 || string(body["loan_id"]) != `"refinance-old"` {
				t.Fatalf("old loan was not repaid in full without a frozen partial amount: %s", data.Action.Body)
			}
			if err := runner.newActionPreflight(h.ctx, mission, &data); err != nil {
				t.Fatalf("payoff with only the preserved operating floor was vetoed: %v", err)
			}
			loanRefinanceDispatch(t, h, runner)
			if state := decode[loanRefinanceState](runner.snapshot, "_loan_refinance"); state.State != "repay_wait" || state.OldLoanID == "" {
				t.Fatalf("repayment receipt prematurely cleared the durable earmark: %+v", state)
			}
			loanRefinanceExpireRetry(t, h, runner)
			if !loanRefinanceDrive(t, h, runner) {
				t.Fatal("fresh full borrower history did not confirm repayment")
			}
			h.missingMission(loanRefinanceMission)
			if _, exists := runner.snapshot["_loan_refinance"]; exists {
				t.Fatal("fresh old-loan repayment did not release the earmark")
			}
			for range 3 {
				runner = h.runner(primaryTestKey)
				loanCapacityRefresh(t, h, runner)
				loanRefinanceDrive(t, h, runner)
			}
			if h.api.mutationCount("/v1/loan/request") != 1 || h.api.mutationCount("/v1/loan/approve") != 1 || h.api.mutationCount("/v1/loan/repay") != 1 || len(h.pending()) != 0 {
				t.Fatal("restart duplicated borrowing or full payoff, or re-triggered on the fresh loan")
			}
			rows, err := h.db.History(h.ctx, h.account.ID, "decisions", store.MaxHistoryLimit)
			if err != nil {
				t.Fatal(err)
			}
			kinds := map[string]bool{}
			for _, row := range rows.([]store.Decision) {
				kinds[row.Kind] = true
			}
			for _, kind := range []string{"loan_refinance_request", "loan_refinance_approve", "loan_refinance_repay", "loan_refinance_done"} {
				if !kinds[kind] {
					t.Fatalf("missing refinance decision %s", kind)
				}
			}
		})
	}
}

func TestLoanRefinanceLapseAndRefusalCooldownSurvivesRestart(t *testing.T) {
	for _, kind := range []string{"lapse", "request refusal"} {
		t.Run(kind, func(t *testing.T) {
			h, runner := loanRefinanceFixture(t)
			loanRefinanceDrive(t, h, runner)
			if kind == "request refusal" {
				h.api.loanRequestMode = "conflict"
			}
			loanRefinanceDispatch(t, h, runner)
			loanCapacityRefresh(t, h, runner)
			before := time.Now().UTC()
			switch kind {
			case "lapse":
				h.api.mu.Lock()
				h.api.loanRequests[0].Status = "lapsed"
				h.api.mu.Unlock()
				loanCapacityRefresh(t, h, runner)
				loanRefinanceDrive(t, h, runner)
			}
			state := decode[loanRefinanceState](runner.snapshot, "_loan_refinance")
			if state.State != "backoff" || state.RetryAt.Before(before.Add(24*time.Hour-time.Second)) || state.RetryAt.After(time.Now().UTC().Add(24*time.Hour)) {
				t.Fatalf("%s retry is not 24 real hours: %+v", kind, state)
			}
			loanRefinanceNextDue(t, h, runner)
			runner = h.runner(primaryTestKey)
			loanCapacityRefresh(t, h, runner)
			if loanRefinanceDrive(t, h, runner) {
				t.Fatal("restart skipped the persisted refinance backoff")
			}
			if h.api.mutationCount("/v1/loan/request") != 1 || h.api.mutationCount("/v1/loan/approve") > 1 {
				t.Fatal("backoff duplicated a refused or lapsed request")
			}
			until := decode[time.Time](runner.snapshot, "_loan_refinance_retry_at")
			if !until.Equal(state.RetryAt) {
				t.Fatalf("restart lost the account refinance cooldown: %v, want %v", until, state.RetryAt)
			}
			runner.snapshot["_loan_refinance_retry_at"] = encode(time.Now().UTC().Add(-time.Second))
			if err := runner.saveSnapshot(h.ctx); err != nil {
				t.Fatal(err)
			}
			h.api.mu.Lock()
			h.api.loanRequestMode = "ok"
			h.api.mu.Unlock()
			if !loanRefinanceDrive(t, h, runner) {
				t.Fatal("elapsed cooldown and a new installment due date did not reopen refinancing")
			}
			loanRefinanceDispatch(t, h, runner)
			if h.api.mutationCount("/v1/loan/request") != 2 {
				t.Fatal("elapsed cooldown did not post exactly one replacement request")
			}
		})
	}
}

func TestLoanRefinanceWaitingDoesNotBlockPriorityArrearsCure(t *testing.T) {
	h, runner := loanRefinancePosted(t)
	before, data := h.mission(loanRefinanceMission)
	waiting := encode(data)
	h.api.mu.Lock()
	h.api.playerLoans[0].ArrearsPence = 20_000
	h.api.playerLoans[0].NextPaymentPence = new(int64(20_000))
	h.api.mu.Unlock()
	loanCapacityRefresh(t, h, runner)
	h.api.setVaultBalance("north", 19_999)
	loanCapacityRefresh(t, h, runner)
	if needed, reason := runner.loanRefinanceNeeded(runner.stateCache.Loans[0]); !needed {
		t.Fatalf("waiting refinance with uncured arrears is not needed: %s", reason)
	}
	if changed, err := runner.serviceLoans(h.ctx); changed || err != nil {
		t.Fatalf("unfunded arrears cure should wait without advancing refinance: %v, %v", changed, err)
	}
	required, err := runner.loanServiceReserves(h.ctx, "refinance-old")
	if err != nil {
		t.Fatal(err)
	}
	h.api.setVaultBalance("north", required["north_sea"]+20_000)
	loanCapacityRefresh(t, h, runner)
	if changed, err := runner.work(h.ctx); !changed || err != nil {
		t.Fatalf("arrears cure did not run while the refinance waited: %v, %v", changed, err)
	}
	rows := h.api.mutationsFor("/v1/loan/repay")
	var body gameapi.RepayLoanRequest
	if len(rows) != 1 || json.Unmarshal(rows[0].Body, &body) != nil || body.LoanID != "refinance-old" || body.AmountPence == nil || *body.AmountPence != 20_000 {
		t.Fatalf("priority arrears cure was not sent: %+v", rows)
	}
	after, data := h.mission(loanRefinanceMission)
	if after.Generation != before.Generation || !reflect.DeepEqual(waiting, encode(data)) || h.api.mutationCount("/v1/loan/approve") != 0 {
		t.Fatal("loan service destroyed or advanced a waiting refinance before curing arrears")
	}
}

func TestLoanRefinanceFundingShortfallPreservesDonorAndOperatingCash(t *testing.T) {
	h, runner := loanRefinancePosted(t)
	loanRefinanceApprove(t, h, runner)
	state := decode[loanRefinanceState](runner.snapshot, "_loan_refinance")
	old, _ := runner.loanByID(state.OldLoanID)
	input, err := runner.loanRefinanceRepaymentInput(h.ctx, state, old)
	if err != nil {
		t.Fatal(err)
	}
	h.api.setVaultBalance("north", input.ReserveByRegion["north_sea"]+loanFullPayment(old)-50_000)
	h.api.setVaultBalance("south", input.ReserveByRegion["channel"]+100_000)
	loanCapacityRefresh(t, h, runner)
	if !loanRefinanceDrive(t, h, runner) {
		t.Fatal("regional payoff shortfall did not stage donor funding")
	}
	mission, data := h.mission("__treasury")
	var body struct {
		From  string `json:"from_region"`
		To    string `json:"to_region"`
		Gross int64  `json:"gross_amount_pence"`
	}
	if data.Action == nil || data.Action.Stage != "remit" || data.FundingMission != loanRefinanceMission ||
		json.Unmarshal(data.Action.Body, &body) != nil || body.From != "channel" || body.To != "north_sea" || body.Gross < 50_000 || body.Gross > 100_000 {
		t.Fatalf("payoff shortfall did not use the recorded refinance owner and reserve-safe donor: %+v", data)
	}
	if err := runner.newActionPreflight(h.ctx, mission, &data); err != nil {
		t.Fatalf("refinance donor funding was vetoed as non-arrears loan service: %v", err)
	}
	if h.api.mutationCount("/v1/loan/repay") != 0 {
		t.Fatal("old loan was paid before its region was safely funded")
	}
}

func TestLoanRefinanceNeverOpensSecondBorrowerRequest(t *testing.T) {
	h, runner := loanRefinanceFixture(t)
	h.api.mu.Lock()
	h.api.loanRequests = []gameapi.LoanRequestView{{ID: "unrelated-open-request", BorrowerCompanyID: recoveryCompanyID,
		Status: "open", PrincipalPence: 5_000_000, BankingRegion: "north_sea", Repayment: "amortised", TermGameDays: 360,
		CollateralKind: "unsecured", Recourse: true, AutoQuote: true, CreatedAt: time.Now().UTC(), ExpiresAt: time.Now().UTC().Add(time.Hour)}}
	h.api.mu.Unlock()
	loanCapacityRefresh(t, h, runner)
	if loanRefinanceDrive(t, h, runner) {
		t.Fatal("an unrelated open request did not retain the single borrower slot")
	}
	h.missingMission(loanRefinanceMission)
	if h.api.mutationCount("/v1/loan/request") != 0 {
		t.Fatal("refinance posted alongside an open own request")
	}
}

func TestLoanRefinanceSmallFinalPaymentCashTrigger(t *testing.T) {
	for _, short := range []bool{false, true} {
		t.Run(map[bool]string{false: "payoff available", true: "one penny below payoff"}[short], func(t *testing.T) {
			h, runner := loanRefinanceFixture(t)
			h.api.mu.Lock()
			h.api.playerLoans[0].OwedPence, h.api.playerLoans[0].OutstandingPrincipalPence = 50_000, 50_000
			h.api.playerLoans[0].NextPaymentPence = new(int64(50_000))
			h.api.mu.Unlock()
			loanCapacityRefresh(t, h, runner)
			old := runner.stateCache.Loans[0]
			balance := *old.NextPaymentPence
			if short {
				balance--
			}
			h.api.setVaultBalance("north", balance)
			loanCapacityRefresh(t, h, runner)
			if changed := loanRefinanceDrive(t, h, runner); changed != short {
				t.Fatalf("refinance staged=%v, want %v at the actual next payment boundary", changed, short)
			}
		})
	}
}

func loanRefinanceExpireRetry(t *testing.T, h *recoveryHarness, runner *Runner) {
	t.Helper()
	state := decode[loanRefinanceState](runner.snapshot, "_loan_refinance")
	state.RetryAt = time.Now().UTC().Add(-time.Second)
	if err := runner.saveLoanRefinance(h.ctx, state); err != nil {
		t.Fatal(err)
	}
}

func loanRefinancePublishQuote(t *testing.T, h *recoveryHarness, runner *Runner) {
	t.Helper()
	h.api.mu.Lock()
	request := &h.api.loanRequests[0]
	request.Quotes = []gameapi.LoanQuote{loanRefinanceQuote(*request, "refinance-quote", 3_000, 70_000)}
	h.api.mu.Unlock()
	loanCapacityRefresh(t, h, runner)
}

func TestLoanRefinanceRecoveredRunwayClearsUnfundedIntent(t *testing.T) {
	for _, backoff := range []bool{false, true} {
		t.Run(map[bool]string{false: "request not sent", true: "elapsed backoff"}[backoff], func(t *testing.T) {
			h, runner := loanRefinanceFixture(t)
			loanRefinanceDrive(t, h, runner)
			mission, data := h.mission(loanRefinanceMission)
			if _, err := runner.replanUnsentAction(h.ctx, mission, &data, neverSentFinancialEvent); err != nil {
				t.Fatal(err)
			}
			if backoff {
				state := decode[loanRefinanceState](runner.snapshot, "_loan_refinance")
				state.State, state.RetryAt = "backoff", time.Now().UTC().Add(-time.Second)
				if err := runner.saveLoanRefinance(h.ctx, state); err != nil {
					t.Fatal(err)
				}
			}
			h.api.setVaultBalance("north", 10_000_000)
			loanCapacityRefresh(t, h, runner)
			if runner.loanRefinanceHoldsPrepay() {
				t.Fatal("unfunded intent blocked ordinary surplus prepayment")
			}
			if !loanRefinanceDrive(t, h, runner) {
				t.Fatal("recovered cash did not retire the unfunded intent")
			}
			h.missingMission(loanRefinanceMission)
			if _, exists := runner.snapshot["_loan_refinance"]; exists {
				t.Fatal("retired unfunded refinance retained its snapshot record")
			}
		})
	}
}

func TestLoanRefinanceCashRecoveryVetoesApprovalAndLetsRequestLapse(t *testing.T) {
	for _, staged := range []bool{false, true} {
		t.Run(map[bool]string{false: "selection", true: "first send"}[staged], func(t *testing.T) {
			h, runner := loanRefinancePosted(t)
			loanRefinancePublishQuote(t, h, runner)
			if staged {
				loanRefinanceDrive(t, h, runner)
			}
			h.api.setVaultBalance("north", 10_000_000)
			loanCapacityRefresh(t, h, runner)
			if staged {
				mission, data := h.mission(loanRefinanceMission)
				if err := runner.newActionPreflight(h.ctx, mission, &data); err == nil {
					t.Fatal("approval spent borrowed money despite recovered runway")
				}
				loanRefinanceDispatch(t, h, runner)
				_, data = h.mission(loanRefinanceMission)
				data.CooldownUntil = time.Time{}
				if err := runner.saveMission(h.ctx, mission, &data); err != nil {
					t.Fatal(err)
				}
			}
			loanRefinanceDrive(t, h, runner)
			_, data := h.mission(loanRefinanceMission)
			if data.Action != nil || h.api.mutationCount("/v1/loan/approve") != 0 {
				t.Fatal("cash recovery still approved an unnecessary replacement loan")
			}
			h.api.mu.Lock()
			h.api.loanRequests[0].Status = "lapsed"
			h.api.mu.Unlock()
			loanCapacityRefresh(t, h, runner)
			if !loanRefinanceDrive(t, h, runner) {
				t.Fatal("unfunded recovered request did not retire after lapse")
			}
			h.missingMission(loanRefinanceMission)
		})
	}
}

func TestLoanRefinanceUnsentAndRateLimitedApprovalsReleaseEarmark(t *testing.T) {
	for _, kind := range []string{"never sent", "rate limited", "staging held"} {
		t.Run(kind, func(t *testing.T) {
			h, runner := loanRefinancePosted(t)
			loanRefinancePublishQuote(t, h, runner)
			if kind == "staging held" {
				runner.publishedFinancial.Add(1)
			}
			loanRefinanceDrive(t, h, runner)
			mission, data := h.mission(loanRefinanceMission)
			if kind == "never sent" {
				if _, err := runner.replanUnsentAction(h.ctx, mission, &data, neverSentFinancialEvent); err != nil {
					t.Fatal(err)
				}
			} else if kind == "rate limited" {
				h.api.loanApproveMode = "rate_limited"
				loanRefinanceDispatch(t, h, runner)
			}
			state := decode[loanRefinanceState](runner.snapshot, "_loan_refinance")
			old, _ := runner.loanByID(state.OldLoanID)
			if state.State != "wait" || loanRefinanceEarmark(state, old) != 0 {
				t.Fatalf("unissued approval left a phantom earmark: %+v", state)
			}
		})
	}
}

func TestLoanRefinanceQuoteRefusalTriesAnotherOpenQuote(t *testing.T) {
	h, runner := loanRefinancePosted(t)
	loanRefinancePublishQuote(t, h, runner)
	loanRefinanceDrive(t, h, runner)
	h.api.loanApproveMode = "conflict"
	loanRefinanceDispatch(t, h, runner)
	state := decode[loanRefinanceState](runner.snapshot, "_loan_refinance")
	if state.State != "wait" || !state.RetryAt.IsZero() || state.RequestID == "" {
		t.Fatalf("one refused quote backed off the whole open request: %+v", state)
	}
	h.api.mu.Lock()
	h.api.loanApproveMode = "ok"
	request := &h.api.loanRequests[0]
	request.Quotes = append(request.Quotes, loanRefinanceQuote(*request, "replacement-quote", 4_000, 75_000))
	h.api.mu.Unlock()
	loanCapacityRefresh(t, h, runner)
	if !loanRefinanceDrive(t, h, runner) {
		t.Fatal("replacement quote was not considered on the open request")
	}
	_, data := h.mission(loanRefinanceMission)
	if data.Action == nil || string(data.Action.Body) != `{"quote_id":"replacement-quote"}` || h.api.mutationCount("/v1/loan/request") != 1 {
		t.Fatal("refused quote was retried or the borrower request was duplicated")
	}
}

func TestLoanRefinanceApprovalAllowsAccruedPayoffButRequiresExpiryMargin(t *testing.T) {
	h, runner := loanRefinancePosted(t)
	loanRefinancePublishQuote(t, h, runner)
	h.api.mu.Lock()
	h.api.playerLoans[0].OwedPence = h.api.loanRequests[0].PrincipalPence + 100
	h.api.mu.Unlock()
	loanCapacityRefresh(t, h, runner)
	if !loanRefinanceDrive(t, h, runner) {
		t.Fatal("interest accrued after posting wrongly invalidated the frozen principal")
	}
	mission, data := h.mission(loanRefinanceMission)
	if err := runner.newActionPreflight(h.ctx, mission, &data); err != nil {
		t.Fatal(err)
	}
	runner.stateCache.LoanRequests[0].Quotes[0].ExpiresAt = time.Now().UTC().Add(30 * time.Second)
	if err := runner.newActionPreflight(h.ctx, mission, &data); err == nil {
		t.Fatal("quote with less than one game hour to expiry was approved")
	}
}

func TestLoanRefinancePriorityArrearsCureIgnoresProceedsEarmark(t *testing.T) {
	for _, phase := range []string{"approve", "repay"} {
		t.Run(phase, func(t *testing.T) {
			h, runner := loanRefinancePosted(t)
			loanRefinancePublishQuote(t, h, runner)
			if phase == "repay" {
				loanRefinanceApprove(t, h, runner)
			} else {
				loanRefinanceDrive(t, h, runner)
			}
			h.api.mu.Lock()
			h.api.playerLoans[0].ArrearsPence = 20_000
			h.api.mu.Unlock()
			loanCapacityRefresh(t, h, runner)
			required, err := runner.loanServiceReserves(h.ctx, "refinance-old")
			if err != nil {
				t.Fatal(err)
			}
			h.api.setVaultBalance("north", required["north_sea"]+20_000)
			h.api.setVaultBalance("south", required["channel"])
			loanCapacityRefresh(t, h, runner)
			if changed, err := runner.work(h.ctx); !changed || err != nil {
				t.Fatalf("priority arrears remained blocked by refinance %s: %v, %v", phase, changed, err)
			}
			rows := h.api.mutationsFor("/v1/loan/repay")
			var body gameapi.RepayLoanRequest
			if len(rows) != 1 || json.Unmarshal(rows[0].Body, &body) != nil || body.AmountPence == nil || *body.AmountPence != 20_000 {
				t.Fatalf("old loan was not cured before full payoff: %+v", rows)
			}
			h.mission(loanRefinanceMission)
		})
	}
}

func TestLoanRefinanceLapseBackoffRunsDespiteArrears(t *testing.T) {
	h, runner := loanRefinancePosted(t)
	h.api.mu.Lock()
	h.api.loanRequests[0].Status = "lapsed"
	h.api.playerLoans[0].ArrearsPence = 20_000
	h.api.mu.Unlock()
	loanCapacityRefresh(t, h, runner)
	if !loanRefinanceDrive(t, h, runner) {
		t.Fatal("arrears prevented lapsed refinance bookkeeping")
	}
	if state := decode[loanRefinanceState](runner.snapshot, "_loan_refinance"); state.State != "backoff" || state.RequestID != "" {
		t.Fatalf("lapsed request retained a permanent prepayment hold: %+v", state)
	}
}

func TestLoanRefinanceMismatchedDisbursementIsQuarantined(t *testing.T) {
	for _, path := range []string{"receipt", "restart adoption", "repayment preflight"} {
		t.Run(path, func(t *testing.T) {
			h, runner := loanRefinancePosted(t)
			if path == "receipt" {
				base := h.api.server.Config.Handler
				h.api.server.Config.Handler = http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
					recorder := httptest.NewRecorder()
					base.ServeHTTP(recorder, req)
					body := recorder.Body.Bytes()
					if req.Method == http.MethodPost && req.URL.Path == "/v1/loan/approve" && recorder.Code == http.StatusOK {
						var loan gameapi.Loan
						_ = json.Unmarshal(body, &loan)
						loan.PrincipalPence++
						body = encode(loan)
						h.api.mu.Lock()
						h.api.playerLoans[len(h.api.playerLoans)-1].PrincipalPence++
						h.api.mu.Unlock()
					}
					w.WriteHeader(recorder.Code)
					_, _ = w.Write(body)
				})
			}
			if path == "receipt" {
				loanRefinancePublishQuote(t, h, runner)
				loanRefinanceDrive(t, h, runner)
				mission, data := h.mission(loanRefinanceMission)
				actionID := data.Action.ID
				if changed, err := runner.dispatch(h.ctx, mission, &data); !changed || err != nil {
					t.Fatalf("mismatched receipt did not retire the controller: %v, %v", changed, err)
				}
				if data.Action != nil || data.LastActionID != actionID {
					t.Fatal("settled mismatch receipt retained an action requiring spurious reconciliation")
				}
				loanCapacityRefresh(t, h, runner)
			} else {
				loanRefinanceApprove(t, h, runner)
			}
			if path == "repayment preflight" {
				loanRefinanceDrive(t, h, runner)
			}
			if path != "receipt" {
				h.api.mu.Lock()
				*h.api.playerLoans[len(h.api.playerLoans)-1].InstallmentPence++
				h.api.mu.Unlock()
				loanCapacityRefresh(t, h, runner)
			}
			if path == "repayment preflight" {
				mission, data := h.mission(loanRefinanceMission)
				if err := runner.newActionPreflight(h.ctx, mission, &data); err == nil {
					t.Fatal("repayment preflight trusted financially mismatched proceeds")
				}
				loanRefinanceDispatch(t, h, runner)
				_, data = h.mission(loanRefinanceMission)
				data.CooldownUntil = time.Time{}
				if err := runner.saveMission(h.ctx, mission, &data); err != nil {
					t.Fatal(err)
				}
			}
			if path == "restart adoption" {
				if err := h.db.DeleteMission(h.ctx, h.account.ID, loanRefinanceMission); err != nil {
					t.Fatal(err)
				}
				runner = h.runner(primaryTestKey)
				loanCapacityRefresh(t, h, runner)
			}
			loanRefinanceDrive(t, h, runner)
			loanRefinanceRequireRetired(t, h, runner)
			if h.api.mutationCount("/v1/loan/repay") != 0 {
				t.Fatal("mismatched refinancing retired old debt")
			}
		})
	}
}

func TestLoanRefinanceRepaymentInputPreservesMaxFloorsAndRejectsOverflow(t *testing.T) {
	h, runner := loanRefinancePosted(t)
	loanRefinanceApprove(t, h, runner)
	state := decode[loanRefinanceState](runner.snapshot, "_loan_refinance")
	old, _ := runner.loanByID(state.OldLoanID)
	port := runner.stateCache.Catalog.Ports[0]
	port.ID, port.BankingRegion = "refinance-expensive-destination", old.BankingRegion
	runner.stateCache.Catalog.Ports = append(runner.stateCache.Catalog.Ports, port)
	details := runner.stateCache.Ports[runner.stateCache.Ships[0].Ship.CurrentPortID]
	details.BerthDuesPerDayPence = 200_000
	runner.stateCache.Ports[port.ID] = details
	shipID := runner.stateCache.Ships[0].Ship.ID
	mission := &store.Mission{AccountID: h.account.ID, ShipID: shipID, Phase: "travel", Destination: port.ID, Generation: "expensive-destination"}
	data := missionData{Plan: Plan{Kind: "treasury", ShipID: shipID}, CostsKnown: true}
	if err := runner.saveMission(h.ctx, mission, &data); err != nil {
		t.Fatal(err)
	}
	input, err := runner.loanRefinanceRepaymentInput(h.ctx, state, old)
	if err != nil || input.ReserveByRegion[old.BankingRegion] < 700_000 {
		t.Fatalf("releasing old debt subtracted from a max-derived operating floor: %v, %v", input.ReserveByRegion, err)
	}
	old.NextPaymentPence = new(int64(math.MaxInt64))
	if _, err := runner.loanRefinanceRepaymentInput(h.ctx, state, old); err == nil {
		t.Fatal("unrepresentable installment reservations were treated as releasable cash")
	}
}

func TestLoanRefinanceRepaymentWaitsForFreshProofAndStopsBoundedRetries(t *testing.T) {
	h, runner := loanRefinancePosted(t)
	loanRefinanceApprove(t, h, runner)
	base := h.api.server.Config.Handler
	h.api.server.Config.Handler = http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		if req.Method == http.MethodPost && req.URL.Path == "/v1/loan/repay" {
			body, _ := io.ReadAll(req.Body)
			h.api.mu.Lock()
			h.api.record(req, body, http.StatusOK)
			loan := h.api.playerLoans[0]
			h.api.mu.Unlock()
			h.api.writeJSON(w, http.StatusOK, loan)
			return
		}
		base.ServeHTTP(w, req)
	})
	for attempt := range 3 {
		if !loanRefinanceDrive(t, h, runner) {
			t.Fatalf("full repayment attempt %d was not staged", attempt+1)
		}
		loanRefinanceDispatch(t, h, runner)
		state := decode[loanRefinanceState](runner.snapshot, "_loan_refinance")
		if state.State != "repay_wait" || state.RepayAttempts != attempt+1 || !state.RetryAt.After(time.Now()) {
			t.Fatalf("nonterminal receipt was accepted as retirement or retried without a clock delay: %+v", state)
		}
		loanRefinanceExpireRetry(t, h, runner)
		loanRefinanceDrive(t, h, runner)
		if attempt < 2 {
			loanRefinanceExpireRetry(t, h, runner)
		}
	}
	loanRefinanceRequireRetired(t, h, runner)
	if h.api.mutationCount("/v1/loan/repay") != 3 {
		t.Fatal("unconfirmed full repayments were retried without a finite bound")
	}
	rows, err := h.db.History(h.ctx, h.account.ID, "decisions", store.MaxHistoryLimit)
	if err != nil {
		t.Fatal(err)
	}
	exhausted := false
	for _, row := range rows.([]store.Decision) {
		if row.Kind == "loan_refinance_repay_exhausted" {
			exhausted = true
		}
		if row.Kind == "loan_refinance_quarantined" {
			t.Fatal("repayment exhaustion was mislabeled as a disbursement mismatch")
		}
	}
	if !exhausted {
		t.Fatal("repayment exhaustion did not record its own decision")
	}
}

func TestLoanRefinanceTerminalOldLoanRetiresControlWithoutRequestAuthority(t *testing.T) {
	for _, status := range []string{"defaulted", "written_off", "missing"} {
		t.Run(status, func(t *testing.T) {
			h, runner := loanRefinancePosted(t)
			loanRefinanceApprove(t, h, runner)
			h.api.mu.Lock()
			if status == "missing" {
				h.api.playerLoans = h.api.playerLoans[1:]
			} else {
				h.api.playerLoans[0].Status = status
			}
			h.api.mu.Unlock()
			loanCapacityRefresh(t, h, runner)
			meta := runner.meta["loan_requests"]
			meta.Stale = true
			runner.meta["loan_requests"] = meta
			if !loanRefinanceDrive(t, h, runner) {
				t.Fatal("terminal old loan left its refinance controller permanently live")
			}
			h.missingMission(loanRefinanceMission)
			if _, exists := runner.snapshot["_loan_refinance"]; exists {
				t.Fatal("terminal old loan retained the old-loan payoff earmark")
			}
			if h.api.mutationCount("/v1/loan/repay") != 0 {
				t.Fatal("terminal old loan caused another full repayment")
			}
		})
	}
}

func TestLoanRefinanceRepaysWithStaleRequestsAndDoesNotPublishClosedHistory(t *testing.T) {
	h, runner := loanRefinancePosted(t)
	loanRefinanceApprove(t, h, runner)
	meta := runner.meta["loan_requests"]
	meta.Stale = true
	runner.meta["loan_requests"] = meta
	if !loanRefinanceDrive(t, h, runner) {
		t.Fatal("stale request authority blocked repayment from an already verified disbursement")
	}
	mission, data := h.mission(loanRefinanceMission)
	if err := runner.newActionPreflight(h.ctx, mission, &data); err != nil {
		t.Fatal(err)
	}
	loanRefinanceDispatch(t, h, runner)
	loanRefinanceExpireRetry(t, h, runner)
	if !loanRefinanceDrive(t, h, runner) {
		t.Fatal("fresh old-loan retirement still depended on stale request authority")
	}
	for _, loan := range decode[[]gameapi.Loan](runner.snapshot, "loans") {
		if loan.Status != "active" && loan.Status != "defaulted" {
			t.Fatalf("refinance history refresh published closed loan debt: %+v", loan)
		}
	}
}

func TestLoanRefinanceRepaymentRechecksEveryApprovedFinancialTerm(t *testing.T) {
	h, runner := loanRefinancePosted(t)
	loanRefinanceApprove(t, h, runner)
	loanRefinanceDrive(t, h, runner)
	mission, data := h.mission(loanRefinanceMission)
	state := decode[loanRefinanceState](runner.snapshot, "_loan_refinance")
	index := slices.IndexFunc(runner.stateCache.Loans, func(loan gameapi.Loan) bool { return loan.ID == state.NewLoanID })
	original := runner.stateCache.Loans[index]
	for _, mutate := range []func(*gameapi.Loan){
		func(loan *gameapi.Loan) { loan.PrincipalPence++ },
		func(loan *gameapi.Loan) { loan.AnnualRateBPS = new(*loan.AnnualRateBPS + 1) },
		func(loan *gameapi.Loan) { loan.InstallmentPence = new(*loan.InstallmentPence + 1) },
		func(loan *gameapi.Loan) { loan.BankingRegion = "channel" },
		func(loan *gameapi.Loan) { loan.RequestID = new("another-request") },
		func(loan *gameapi.Loan) { loan.TermGameDays-- },
	} {
		runner.stateCache.Loans[index] = original
		mutate(&runner.stateCache.Loans[index])
		if err := runner.newActionPreflight(h.ctx, mission, &data); err == nil {
			t.Fatal("repayment trusted a loan whose terms differ from its durable approved quote")
		}
	}
	runner.stateCache.Loans[index] = original
	if err := runner.newActionPreflight(h.ctx, mission, &data); err != nil {
		t.Fatalf("unchanged approved financial terms were vetoed: %v", err)
	}
}

func loanRefinanceRequireRetired(t *testing.T, h *recoveryHarness, runner *Runner) {
	t.Helper()
	h.missingMission(loanRefinanceMission)
	if _, exists := runner.snapshot["_loan_refinance"]; exists {
		t.Fatal("retired refinance retained its earmark record")
	}
	if runner.loanRefinanceHoldsPrepay() {
		t.Fatal("retired refinance still blocked ordinary prepayment")
	}
	if runner.debtServiceHoldPence() <= 0 {
		t.Fatal("retired refinance did not leave remaining unsecured loans as ordinary debt")
	}
}

func TestLoanRefinanceSuccessfulReceiptDoesNotConsumeRetryAllowance(t *testing.T) {
	h, runner := loanRefinancePosted(t)
	loanRefinanceApprove(t, h, runner)
	loanRefinanceDrive(t, h, runner)
	state := decode[loanRefinanceState](runner.snapshot, "_loan_refinance")
	state.RepayAttempts = 2
	if err := runner.saveLoanRefinance(h.ctx, state); err != nil {
		t.Fatal(err)
	}
	loanRefinanceDispatch(t, h, runner)
	state = decode[loanRefinanceState](runner.snapshot, "_loan_refinance")
	if state.RepayAttempts != 2 || state.State != "repay_wait" {
		t.Fatalf("successful repayment consumed the failed-attempt allowance: %+v", state)
	}
	loanRefinanceExpireRetry(t, h, runner)
	before := time.Now().UTC()
	if !loanRefinanceDrive(t, h, runner) {
		t.Fatal("successful repayment did not retire after fresh proof")
	}
	h.missingMission(loanRefinanceMission)
	runner = h.runner(primaryTestKey)
	until := decode[time.Time](runner.snapshot, "_loan_refinance_retry_at")
	if until.Before(before.Add(24*time.Hour)) || until.After(time.Now().UTC().Add(24*time.Hour)) {
		t.Fatalf("successful refinance lost its durable 24-real-hour account cooldown: %v", until)
	}
}

func TestLoanRefinanceCaptureFailureWaitsWithoutRestartingWork(t *testing.T) {
	for _, dataset := range []string{"loans", "vaults"} {
		t.Run(dataset, func(t *testing.T) {
			h, runner := loanRefinancePosted(t)
			loanRefinanceApprove(t, h, runner)
			loanRefinanceDrive(t, h, runner)
			loanRefinanceDispatch(t, h, runner)
			loanRefinanceExpireRetry(t, h, runner)
			failed := true
			base := h.api.server.Config.Handler
			h.api.server.Config.Handler = http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
				if failed && req.Method == http.MethodGet &&
					(dataset == "loans" && req.URL.Path == "/v1/loan/list" ||
						dataset == "vaults" && req.URL.Path == "/v1/treasury/vaults") {
					h.api.writeProblem(w, http.StatusBadRequest, "temporary_capture_failure", "dataset temporarily unavailable")
					return
				}
				base.ServeHTTP(w, req)
			})
			mission, data := h.mission(loanRefinanceMission)
			if _, err := runner.driveLoanRefinance(h.ctx, mission, &data); err != nil {
				t.Fatalf("transient %s capture failure restarted the account: %v", dataset, err)
			}
			h.mission(loanRefinanceMission)
			if state := decode[loanRefinanceState](runner.snapshot, "_loan_refinance"); state.State != "repay_wait" {
				t.Fatalf("failed capture discarded the waiting refinance: %+v", state)
			}
			if runner.fresh(dataset) {
				t.Fatalf("failed %s capture was published as fresh", dataset)
			}
			waiting := false
			for _, hold := range runner.holding {
				waiting = waiting || strings.Contains(hold.Reason, "refinance awaits fresh borrower history and cash authority")
			}
			if !waiting {
				t.Fatal("failed refinance capture did not publish an authority wait")
			}
			failed = false
			loanCapacityRefresh(t, h, runner)
			if !loanRefinanceDrive(t, h, runner) {
				t.Fatal("recovered capture did not finish the existing refinance")
			}
			h.missingMission(loanRefinanceMission)
			if h.api.mutationCount("/v1/loan/repay") != 1 {
				t.Fatal("capture recovery duplicated the full payoff")
			}
		})
	}
}

func TestLoanRefinanceMultipleOwnUnsecuredLoansPreventRequest(t *testing.T) {
	for _, status := range []string{"active", "defaulted"} {
		t.Run(status, func(t *testing.T) {
			h, runner := loanRefinanceFixture(t)
			old := runner.stateCache.Loans[0]
			if needed, reason := runner.loanRefinanceNeeded(old); !needed {
				t.Fatalf("single old loan does not otherwise need refinancing: %v, %s", needed, reason)
			}
			second := old
			second.ID, second.Status = "second-unsecured-loan", status
			h.api.mu.Lock()
			h.api.playerLoans = append(h.api.playerLoans, second)
			h.api.mu.Unlock()
			loanCapacityRefresh(t, h, runner)
			if needed, reason := runner.loanRefinanceNeeded(old); needed {
				t.Fatalf("multiple own unsecured loans remain eligible: %v, %s", needed, reason)
			}
			if loanRefinanceDrive(t, h, runner) {
				t.Fatal("multiple own unsecured loans started another refinance")
			}
			h.missingMission(loanRefinanceMission)
			if h.api.mutationCount("/v1/loan/request") != 0 {
				t.Fatal("multiple own unsecured loans posted another borrower request")
			}
		})
	}
}

func TestLoanRefinanceTerminalReplacementLoanRetiresController(t *testing.T) {
	for _, status := range []string{"repaid", "defaulted", "written_off", "recovered"} {
		t.Run(status, func(t *testing.T) {
			h, runner := loanRefinancePosted(t)
			loanRefinanceApprove(t, h, runner)
			state := decode[loanRefinanceState](runner.snapshot, "_loan_refinance")
			h.api.mu.Lock()
			for index := range h.api.playerLoans {
				loan := &h.api.playerLoans[index]
				if loan.ID == state.NewLoanID {
					loan.Status = status
					if status == "repaid" {
						loan.OwedPence, loan.OutstandingPrincipalPence = 0, 0
					}
				}
			}
			h.api.mu.Unlock()
			loanCapacityRefresh(t, h, runner)
			if !loanRefinanceDrive(t, h, runner) {
				t.Fatal("terminal replacement loan left its refinance controller waiting forever")
			}
			loanRefinanceRequireRetired(t, h, runner)
			if h.api.mutationCount("/v1/loan/repay") != 0 {
				t.Fatal("terminal replacement debt triggered repayment of the old loan")
			}
			h.api.setVaultBalance("north", 0)
			h.api.setVaultBalance("south", 0)
			runner = h.runner(primaryTestKey)
			loanCapacityRefresh(t, h, runner)
			old, exists := runner.loanByID(state.OldLoanID)
			if !exists || old.Status != "active" {
				t.Fatal("terminal replacement changed the unpaid old loan")
			}
			wantNeeded := status != "defaulted"
			if needed, reason := runner.loanRefinanceNeeded(old); needed != wantNeeded {
				t.Fatalf("replacement status does not follow the active/defaulted count: %v, %s", needed, reason)
			}
			if loanRefinanceDrive(t, h, runner) {
				t.Fatal("terminal replacement bypassed the posted-request cooldown")
			}
			runner.snapshot["_loan_refinance_retry_at"] = encode(time.Now().UTC().Add(-time.Second))
			if err := runner.saveSnapshot(h.ctx); err != nil {
				t.Fatal(err)
			}
			loanRefinanceNextDue(t, h, runner)
			if changed := loanRefinanceDrive(t, h, runner); changed != wantNeeded {
				t.Fatalf("replacement status handled refinancing incorrectly after restart: changed=%v", changed)
			}
			wantRequests := 1
			if wantNeeded {
				loanRefinanceDispatch(t, h, runner)
				wantRequests++
			} else {
				loanRefinanceRequireRetired(t, h, runner)
			}
			if h.api.mutationCount("/v1/loan/request") != wantRequests {
				t.Fatal("replacement status posted an incorrect number of borrower requests")
			}
		})
	}
}

func TestLoanRefinanceRetirementBackoffSurvivesRestart(t *testing.T) {
	for _, cause := range []string{"mismatched terms", "cancelled financing", "exhausted payoff"} {
		t.Run(cause, func(t *testing.T) {
			h, runner := loanRefinancePosted(t)
			loanRefinanceApprove(t, h, runner)
			state := decode[loanRefinanceState](runner.snapshot, "_loan_refinance")
			mission, data := h.mission(loanRefinanceMission)
			switch cause {
			case "mismatched terms":
				for index := range runner.stateCache.Loans {
					if runner.stateCache.Loans[index].ID == state.NewLoanID {
						runner.stateCache.Loans[index].PrincipalPence++
					}
				}
			case "cancelled financing":
				data.Capacity.Financing.Cancel = true
				if err := runner.saveMission(h.ctx, mission, &data); err != nil {
					t.Fatal(err)
				}
			case "exhausted payoff":
				state.State, state.RepayAttempts = "repay_wait", 3
				if err := runner.saveLoanRefinance(h.ctx, state); err != nil {
					t.Fatal(err)
				}
			}
			before := time.Now().UTC()
			if !loanRefinanceDrive(t, h, runner) {
				t.Fatal("unsafe refinancing did not retire its controller")
			}
			loanRefinanceRequireRetired(t, h, runner)
			backoffs := decode[map[string]time.Time](runner.snapshot, "_loan_refinance_backoffs")
			until := backoffs[state.OldLoanID]
			if until.Before(before.Add(4*time.Hour-time.Second)) || until.After(time.Now().UTC().Add(4*time.Hour)) {
				t.Fatalf("retirement backoff is not ten game days at the observed scale: %v", until)
			}
			h.api.setVaultBalance("north", 0)
			h.api.setVaultBalance("south", 0)
			for range 3 {
				runner = h.runner(primaryTestKey)
				loanCapacityRefresh(t, h, runner)
				old, _ := runner.loanByID(state.OldLoanID)
				if needed, reason := runner.loanRefinanceNeeded(old); needed {
					t.Fatalf("multiple own unsecured loans remain eligible after retirement: %v, %s", needed, reason)
				}
				if loanRefinanceDrive(t, h, runner) {
					t.Fatal("restart re-entered the retired old loan during its backoff")
				}
				loanRefinanceRequireRetired(t, h, runner)
			}
			if h.api.mutationCount("/v1/loan/request") != 1 || h.api.mutationCount("/v1/loan/approve") != 1 {
				t.Fatal("retirement backoff stacked another borrower request or replacement loan")
			}
			backoffs[state.OldLoanID] = time.Now().UTC().Add(-time.Second)
			runner.snapshot["_loan_refinance_backoffs"] = encode(backoffs)
			if err := runner.saveSnapshot(h.ctx); err != nil {
				t.Fatal(err)
			}
			runner = h.runner(primaryTestKey)
			loanCapacityRefresh(t, h, runner)
			if loanRefinanceDrive(t, h, runner) {
				t.Fatal("elapsed retirement backoff bypassed the multiple-loan invariant")
			}
			loanRefinanceRequireRetired(t, h, runner)
			if h.api.mutationCount("/v1/loan/request") != 1 || h.api.mutationCount("/v1/loan/approve") != 1 {
				t.Fatal("elapsed backoff stacked another borrower request or replacement loan")
			}
			h.api.mu.Lock()
			for index := range h.api.playerLoans {
				loan := &h.api.playerLoans[index]
				if loan.ID == state.NewLoanID {
					loan.Status, loan.OwedPence, loan.OutstandingPrincipalPence = "repaid", 0, 0
				}
			}
			h.api.mu.Unlock()
			loanCapacityRefresh(t, h, runner)
			backoffs[state.OldLoanID] = until
			runner.snapshot["_loan_refinance_backoffs"] = encode(backoffs)
			if err := runner.saveSnapshot(h.ctx); err != nil {
				t.Fatal(err)
			}
			runner = h.runner(primaryTestKey)
			loanCapacityRefresh(t, h, runner)
			old, _ := runner.loanByID(state.OldLoanID)
			if needed, reason := runner.loanRefinanceNeeded(old); !needed {
				t.Fatalf("repaid replacement still blocks the old loan: %v, %s", needed, reason)
			}
			runner = h.runner(primaryTestKey)
			loanCapacityRefresh(t, h, runner)
			if loanRefinanceDrive(t, h, runner) {
				t.Fatal("repaid replacement bypassed the persisted retirement backoff")
			}
			backoffs[state.OldLoanID] = time.Now().UTC().Add(-time.Second)
			runner.snapshot["_loan_refinance_backoffs"] = encode(backoffs)
			if err := runner.saveSnapshot(h.ctx); err != nil {
				t.Fatal(err)
			}
			runner = h.runner(primaryTestKey)
			loanCapacityRefresh(t, h, runner)
			if loanRefinanceDrive(t, h, runner) {
				t.Fatal("elapsed quarantine backoff bypassed the 24-real-hour posted-request cooldown")
			}
			runner.snapshot["_loan_refinance_retry_at"] = encode(time.Now().UTC().Add(-time.Second))
			if err := runner.saveSnapshot(h.ctx); err != nil {
				t.Fatal(err)
			}
			loanRefinanceNextDue(t, h, runner)
			if !loanRefinanceDrive(t, h, runner) {
				t.Fatal("repaid replacement and elapsed backoff did not reopen refinancing")
			}
			loanRefinanceDispatch(t, h, runner)
			if h.api.mutationCount("/v1/loan/request") != 2 || h.api.mutationCount("/v1/loan/approve") != 1 {
				t.Fatal("repaid replacement did not allow exactly one new borrower request")
			}
		})
	}
}

func TestLoanRefinanceEligibilityVetoRecordsItsActualReason(t *testing.T) {
	for _, phase := range []string{"before request", "open request", "lapsed request"} {
		for _, status := range []string{"active", "defaulted"} {
			t.Run(phase+"/"+status, func(t *testing.T) {
				h, runner := loanRefinanceFixture(t)
				old := runner.stateCache.Loans[0]
				if phase == "before request" {
					if err := runner.saveLoanRefinance(h.ctx, loanRefinanceState{OldLoanID: old.ID, State: "request"}); err != nil {
						t.Fatal(err)
					}
				} else {
					loanRefinanceDrive(t, h, runner)
					loanRefinanceDispatch(t, h, runner)
					loanCapacityRefresh(t, h, runner)
				}
				debt := old
				debt.ID, debt.Status = "eligibility-other-loan", status
				wantReason := "multiple own active or defaulted unsecured loans"
				h.api.mu.Lock()
				h.api.playerLoans = append(h.api.playerLoans, debt)
				if phase == "lapsed request" {
					h.api.loanRequests[0].Status = "lapsed"
				}
				h.api.mu.Unlock()
				loanCapacityRefresh(t, h, runner)
				changed := loanRefinanceDrive(t, h, runner)
				if changed != (phase != "open request") {
					t.Fatalf("eligibility veto handled the %s lifecycle incorrectly: changed=%v", phase, changed)
				}
				kind := "loan_refinance_stopped"
				if phase == "open request" {
					kind = "loan_refinance_wait"
				}
				rows, err := h.db.History(h.ctx, h.account.ID, "decisions", store.MaxHistoryLimit)
				if err != nil {
					t.Fatal(err)
				}
				recorded := false
				for _, row := range rows.([]store.Decision) {
					if row.Kind != kind {
						continue
					}
					if strings.Contains(row.Reason, "cash runway recovered") || !strings.Contains(row.Reason, wantReason) {
						t.Fatalf("eligibility veto reported an inaccurate decision: %s", row.Reason)
					}
					recorded = true
				}
				if !recorded {
					t.Fatalf("eligibility veto did not record its %s reason", kind)
				}
				if h.api.mutationCount("/v1/loan/approve") != 0 {
					t.Fatal("eligibility veto approved another loan")
				}
			})
		}
	}
}

func TestLoanRefinancePreflightUsesCurrentCaptureWithoutNetworkIO(t *testing.T) {
	for _, test := range []struct {
		name   string
		stage  string
		change string
		reason string
	}{
		{"request arrears", "loan-request", "arrears", "arrears"},
		{"approval other arrears", "loan-accept", "other arrears", "arrears"},
		{"request due date", "loan-request", "due", "three-game-day"},
		{"request payoff", "loan-request", "payoff", "exact payoff"},
		{"approval covered payment", "loan-accept", "next", "cash covers"},
		{"approval installment", "loan-accept", "installment", "cheapest quote"},
		{"repayment other arrears", "loan-repay", "other arrears", "arrears"},
	} {
		t.Run(test.name, func(t *testing.T) {
			h, runner := loanRefinanceFixture(t)
			loanRefinanceDrive(t, h, runner)
			if test.stage != "loan-request" {
				loanRefinanceDispatch(t, h, runner)
				loanCapacityRefresh(t, h, runner)
				if test.stage == "loan-accept" {
					loanRefinancePublishQuote(t, h, runner)
				} else {
					loanRefinanceApprove(t, h, runner)
				}
				loanRefinanceDrive(t, h, runner)
			}
			mission, data := h.mission(loanRefinanceMission)
			if data.Action == nil || data.Action.Stage != test.stage {
				t.Fatalf("test did not stage its refinance action: %+v", data.Action)
			}
			var reads atomic.Int32
			base := h.api.server.Config.Handler
			noReads := http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
				if req.Method == http.MethodGet {
					reads.Add(1)
					h.api.writeJSON(w, http.StatusServiceUnavailable, map[string]string{"error": "preflight must not fetch"})
					return
				}
				base.ServeHTTP(w, req)
			})
			h.api.server.Config.Handler = noReads
			if err := runner.newActionPreflight(h.ctx, mission, &data); err != nil {
				t.Fatalf("valid captured refinance required network I/O: %v", err)
			}
			h.api.server.Config.Handler = base
			h.api.mu.Lock()
			for index := range h.api.playerLoans {
				loan := &h.api.playerLoans[index]
				if loan.ID != "refinance-old" {
					continue
				}
				switch test.change {
				case "arrears":
					loan.ArrearsPence = 1
				case "other arrears":
					other := *loan
					other.ID, other.IsMortgage, other.CollateralKind = "other-arrears", true, "ship_lien"
					other.CollateralShipID, other.ArrearsPence = new(recoveryShipLapras), 1
					h.api.playerLoans = append(h.api.playerLoans, other)
				case "due":
					loan.NextPaymentDueAt = new(time.Now().UTC().Add(4 * time.Hour))
				case "next":
					loan.NextPaymentPence = new(int64(1))
				case "payoff":
					loan.OwedPence += 100_000
				case "installment":
					loan.InstallmentPence = new(int64(60_000))
				}
			}
			h.api.mu.Unlock()
			loanCapacityRefresh(t, h, runner)
			h.api.server.Config.Handler = noReads
			if err := runner.newActionPreflight(h.ctx, mission, &data); err == nil || !strings.Contains(err.Error(), test.reason) {
				t.Fatalf("changed old debt bypassed its final captured-state guard: %v", err)
			}
			requests := h.api.mutationCount("/v1/loan/request")
			approvals := h.api.mutationCount("/v1/loan/approve")
			repayments := h.api.mutationCount("/v1/loan/repay")
			loanRefinanceDispatch(t, h, runner)
			if reads.Load() != 0 {
				t.Fatal("refinance first-send preflight made a network read")
			}
			if h.api.mutationCount("/v1/loan/request") != requests || h.api.mutationCount("/v1/loan/approve") != approvals ||
				h.api.mutationCount("/v1/loan/repay") != repayments {
				t.Fatal("changed old debt reached a refinance mutation after its final preflight")
			}
		})
	}
}

func TestLoanRefinanceInstallmentEmergencyWindow(t *testing.T) {
	for _, test := range []struct {
		name    string
		due     time.Duration
		cash    int64
		next    int64
		arrears int64
		want    bool
	}{
		{name: "inside window one penny short", due: 72*time.Minute - time.Minute, cash: 99_999, next: 100_000, want: true},
		{name: "outside window", due: 72*time.Minute + time.Minute, cash: 0, next: 100_000},
		{name: "ordinary dip after collection", due: 4*time.Hour - time.Minute, cash: 0, next: 100_000},
		{name: "exact installment cash", due: time.Hour, cash: 100_000, next: 100_000},
		{name: "actual next payment below installment", due: time.Hour, cash: 80_000, next: 80_000},
		{name: "exact arrears cash", due: 4 * time.Hour, cash: 20_000, next: 20_000, arrears: 20_000},
		{name: "arrears outside the window", due: 4 * time.Hour, cash: 19_999, next: 20_000, arrears: 20_000},
	} {
		t.Run(test.name, func(t *testing.T) {
			h, runner := loanRefinanceFixture(t)
			h.api.setVaultBalance("north", test.cash)
			h.api.mu.Lock()
			h.api.playerLoans[0].NextPaymentDueAt = new(time.Now().UTC().Add(test.due))
			h.api.playerLoans[0].NextPaymentPence = new(test.next)
			h.api.playerLoans[0].ArrearsPence = test.arrears
			h.api.mu.Unlock()
			loanCapacityRefresh(t, h, runner)
			needed, reason := runner.loanRefinanceNeeded(runner.stateCache.Loans[0])
			if needed != test.want {
				t.Fatalf("emergency = %v, want %v, reason=%s", needed, test.want, reason)
			}
		})
	}
}

func TestLoanRefinanceStoppedCooldownPersistsWithoutMission(t *testing.T) {
	h, runner := loanRefinancePosted(t)
	h.api.setVaultBalance("north", 100_000)
	h.api.mu.Lock()
	h.api.loanRequests[0].Status = "withdrawn"
	h.api.mu.Unlock()
	loanCapacityRefresh(t, h, runner)
	before := time.Now().UTC()
	if !loanRefinanceDrive(t, h, runner) {
		t.Fatal("withdrawn recovered request did not stop")
	}
	h.missingMission(loanRefinanceMission)
	runner = h.runner(primaryTestKey)
	loanCapacityRefresh(t, h, runner)
	until := decode[time.Time](runner.snapshot, "_loan_refinance_retry_at")
	if until.Before(before.Add(24*time.Hour)) || until.After(time.Now().UTC().Add(24*time.Hour)) {
		t.Fatalf("stopped-request cooldown was not durably kept for 24 real hours: %v", until)
	}
	if loanRefinanceDrive(t, h, runner) {
		t.Fatal("cash-covered installment reopened a stopped request")
	}
	h.api.setVaultBalance("north", 99_999)
	loanCapacityRefresh(t, h, runner)
	loanRefinanceNextDue(t, h, runner)
	if loanRefinanceDrive(t, h, runner) {
		t.Fatal("ordinary installment shortfall bypassed the stopped-request cooldown")
	}
	runner.snapshot["_loan_refinance_retry_at"] = encode(time.Now().UTC().Add(-time.Second))
	if err := runner.saveSnapshot(h.ctx); err != nil {
		t.Fatal(err)
	}
	if !loanRefinanceDrive(t, h, runner) {
		t.Fatal("new due date and expired cooldown did not allow a new request")
	}
	loanRefinanceDispatch(t, h, runner)
	if h.api.mutationCount("/v1/loan/request") != 2 {
		t.Fatal("new due date did not post exactly one new request")
	}
}

func loanRefinanceNextDue(t *testing.T, h *recoveryHarness, runner *Runner) {
	t.Helper()
	h.api.mu.Lock()
	h.api.playerLoans[0].NextPaymentDueAt = new(time.Now().UTC().Add(30 * time.Minute))
	h.api.mu.Unlock()
	loanCapacityRefresh(t, h, runner)
}

func TestLoanRefinanceInFlightIgnoresOldLoanArrears(t *testing.T) {
	for _, stage := range []string{"loan-accept", "loan-repay"} {
		t.Run(stage, func(t *testing.T) {
			h, runner := loanRefinancePosted(t)
			if stage == "loan-accept" {
				loanRefinancePublishQuote(t, h, runner)
			} else {
				loanRefinanceApprove(t, h, runner)
			}
			h.api.mu.Lock()
			h.api.playerLoans[0].ArrearsPence = 1
			h.api.playerLoans[0].NextPaymentPence = new(int64(1))
			h.api.playerLoans[0].NextPaymentDueAt = new(time.Now().UTC().Add(20 * time.Minute))
			h.api.mu.Unlock()
			if stage == "loan-accept" {
				h.api.setVaultBalance("north", 0)
			}
			loanCapacityRefresh(t, h, runner)
			if !loanRefinanceDrive(t, h, runner) {
				t.Fatal("old loan arrears blocked its already-public refinance")
			}
			mission, data := h.mission(loanRefinanceMission)
			if data.Action == nil || data.Action.Stage != stage {
				t.Fatalf("in-flight refinance did not stage %s: %+v", stage, data.Action)
			}
			if err := runner.newActionPreflight(h.ctx, mission, &data); err != nil {
				t.Fatalf("old loan arrears blocked first send of %s: %v", stage, err)
			}
		})
	}
}
