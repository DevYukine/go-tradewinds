package engine

import (
	"encoding/json"
	"math"
	"strconv"
	"testing"
	"time"

	"github.com/DevYukine/go-tradewinds/internal/gameapi"
)

func playerLoanAcceptanceQuote(t *testing.T, h *recoveryHarness, runner *Runner, rate int32) gameapi.LoanQuote {
	t.Helper()
	_, data := h.mission("__growth")
	intent := data.PlayerLoan
	if intent == nil || intent.RequestID == "" {
		t.Fatal("quote fixture requires a recorded own request")
	}
	factor := (float64(rate) / 10_000 / 36) / (1 - math.Pow(1+float64(rate)/10_000/36, -36))
	installment := int64(math.Ceil(float64(intent.PrincipalPence) * factor))
	scale := runner.stateCache.Clock.Calendar.TimeScale
	if scale <= 0 {
		t.Fatal("quote fixture requires a positive authoritative time scale")
	}
	gameHour := time.Hour / time.Duration(scale)
	return gameapi.LoanQuote{
		ID: "kraken-capacity-quote", RequestID: &intent.RequestID,
		LenderCompanyID: krakenAcceptanceCompany, LenderName: "The Kraken Cartel", BorrowerCompanyID: h.account.CompanyID,
		BankingRegion: intent.BankingRegion, PrincipalPence: intent.PrincipalPence,
		AnnualRateBPS: rate, TermGameDays: 360, Repayment: "amortised",
		InstallmentPence: &installment, TotalRepayablePence: installment * 36,
		Status: "open", CreatedAt: time.Now().UTC(), ExpiresAt: time.Now().UTC().Add(2 * gameHour),
	}
}

func playerLoanAcceptancePublishQuote(t *testing.T, h *recoveryHarness, runner *Runner, quote gameapi.LoanQuote) {
	t.Helper()
	h.api.mu.Lock()
	for index := range h.api.loanRequests {
		h.api.loanRequests[index].Quotes = []gameapi.LoanQuote{quote}
	}
	h.api.mu.Unlock()
	loanCapacityRefresh(t, h, runner)
}

func playerLoanAcceptanceStageApproval(t *testing.T, h *recoveryHarness, runner *Runner) {
	t.Helper()
	for range 4 {
		_, data := h.mission("__growth")
		if data.Action != nil {
			if data.Action.Path != "/v1/loan/approve" {
				t.Fatalf("player quote staged wrong endpoint: %+v", data.Action)
			}
			return
		}
		playerLoanAcceptanceDrive(t, h, runner)
	}
	t.Fatal("valid player quote never staged approval")
}

func TestPlayerLoanApprovalKrakenRateBoundaryBuildsExactlyOnce(t *testing.T) {
	for _, rate := range []int32{2_999, 3_000} {
		t.Run(strconv.Itoa(int(rate)), func(t *testing.T) {
			h, runner := playerLoanAcceptancePosted(t)
			quote := playerLoanAcceptanceQuote(t, h, runner, rate)
			playerLoanAcceptancePublishQuote(t, h, runner, quote)
			playerLoanAcceptanceStageApproval(t, h, runner)
			beforeApproval := time.Now().UTC()
			playerLoanAcceptanceDispatch(t, h, runner)
			afterApproval := time.Now().UTC()
			loanCapacityRefresh(t, h, runner)
			_, data := h.mission("__growth")
			if data.Capacity == nil || data.Capacity.Financing == nil || len(data.Capacity.Financing.Loans) != 1 {
				t.Fatalf("approved loan receipt was not retained: %+v", data)
			}
			loan := data.Capacity.Financing.Loans[0]
			if loan.QuoteID == nil || *loan.QuoteID != quote.ID || loan.RequestID == nil || *loan.RequestID != *quote.RequestID || loan.LenderCompanyID != krakenAcceptanceCompany {
				t.Fatalf("approval recorded the wrong debt: %+v", loan)
			}
			if loan.CollateralKind != "ship_lien" || loan.CollateralShipID == nil || *loan.CollateralShipID != recoveryShipBlastoise {
				t.Fatalf("null quote collateral lost the recorded request's ship lien: %+v", loan)
			}
			gameDay := 24 * time.Hour / time.Duration(runner.stateCache.Clock.Calendar.TimeScale)
			if data.Capacity.Financing.BuildBy.Before(beforeApproval.Add(gameDay)) ||
				data.Capacity.Financing.BuildBy.After(afterApproval.Add(gameDay)) {
				t.Fatalf("approval did not enforce one-game-day build deadline: %+v", data.Capacity.Financing)
			}
			restarted := h.runner(primaryTestKey)
			loanCapacityRefresh(t, h, restarted)
			playerLoanAcceptanceDrive(t, h, restarted)
			_, data = h.mission("__growth")
			if data.Action == nil || data.Action.Stage != "build" {
				t.Fatalf("approved regional cash did not stage construction: %+v", data)
			}
			playerLoanAcceptanceDispatch(t, h, restarted)
			loanCapacityRefresh(t, h, restarted)
			_, data = h.mission("__growth")
			if data.BuiltShipID != recoveryBuiltShipID || len(restarted.stateCache.Loans) != 1 {
				t.Fatalf("restart lost the financed hull or duplicated debt: %+v", data)
			}
			rows := h.api.mutationsFor("/v1/loan/approve")
			var body map[string]any
			if len(rows) != 1 || json.Unmarshal(rows[0].Body, &body) != nil || len(body) != 1 || body["quote_id"] != quote.ID {
				t.Fatalf("approval was not exactly one quote_id mutation: %+v", rows)
			}
			if h.api.mutationCount("/v1/loan/accept-quote") != 0 || h.api.mutationCount("/v1/shipyard/build-ship") != 1 || len(h.pending()) != 0 {
				t.Fatal("player financing used Goldsmiths acceptance, duplicated construction or orphaned an action")
			}
		})
	}
}

func TestPlayerLoanApprovalRejectsRateIdentityRegionAndExpiryGates(t *testing.T) {
	for _, test := range []struct {
		name   string
		gate   string
		change func(*gameapi.LoanQuote, time.Duration)
	}{
		{"one basis point above Goldsmiths", "quote_rate", func(q *gameapi.LoanQuote, _ time.Duration) {
			q.AnnualRateBPS = 3_001
		}},
		{"wrong request", "request_mismatch", func(q *gameapi.LoanQuote, _ time.Duration) {
			id := "another-own-request"
			q.RequestID = &id
		}},
		{"wrong banking region", "quote_terms", func(q *gameapi.LoanQuote, _ time.Duration) {
			q.BankingRegion = "channel"
		}},
		{"own company lender", "quote_lender", func(q *gameapi.LoanQuote, _ time.Duration) {
			q.LenderCompanyID = recoveryCompanyID
		}},
		{"wrong principal", "quote_terms", func(q *gameapi.LoanQuote, _ time.Duration) {
			q.PrincipalPence++
		}},
		{"wrong term", "quote_terms", func(q *gameapi.LoanQuote, _ time.Duration) {
			q.TermGameDays--
		}},
		{"wrong repayment", "quote_terms", func(q *gameapi.LoanQuote, _ time.Duration) {
			q.Repayment = "bullet"
		}},
		{"expires at one game hour", "quote_expiry", func(q *gameapi.LoanQuote, gameHour time.Duration) {
			q.ExpiresAt = time.Now().UTC().Add(gameHour)
		}},
		{"expires inside one game hour", "quote_expiry", func(q *gameapi.LoanQuote, gameHour time.Duration) {
			q.ExpiresAt = time.Now().UTC().Add(gameHour - time.Second)
		}},
	} {
		t.Run(test.name, func(t *testing.T) {
			h, runner := playerLoanAcceptancePosted(t)
			quote := playerLoanAcceptanceQuote(t, h, runner, 3_000)
			test.change(&quote, time.Hour/time.Duration(runner.stateCache.Clock.Calendar.TimeScale))
			playerLoanAcceptancePublishQuote(t, h, runner, quote)
			for range 3 {
				playerLoanAcceptanceDrive(t, h, runner)
			}
			_, data := h.mission("__growth")
			if h.api.mutationCount("/v1/loan/approve") != 0 || data.Action != nil || len(h.pending()) != 0 {
				t.Fatalf("rejected quote escaped the %s gate: %+v", test.gate, data)
			}
			playerLoanAcceptanceDecision(t, h, test.gate)
		})
	}
}

func TestPlayerLoanApprovalRefusesEconomicallyStaleNeed(t *testing.T) {
	h, runner := playerLoanAcceptancePosted(t)
	quote := playerLoanAcceptanceQuote(t, h, runner, 3_000)
	h.api.setVaultBalance("north", 50_000_000)
	h.api.setVaultBalance("south", 500_000)
	playerLoanAcceptancePublishQuote(t, h, runner, quote)
	playerLoanAcceptanceDrive(t, h, runner)
	if h.api.mutationCount("/v1/loan/approve") != 0 || len(h.pending()) != 0 {
		t.Fatal("fully funded capacity need still borrowed player principal")
	}
	playerLoanAcceptanceDecision(t, h, "need_stale")
	h.api.mu.Lock()
	defer h.api.mu.Unlock()
	if len(h.api.loanRequests) != 1 || h.api.loanRequests[0].ID != *quote.RequestID || h.api.loanRequests[0].Status != "open" {
		t.Fatal("economic refusal mutated the server request instead of leaving it to lapse")
	}
}

func TestPlayerLoanApprovalHoldsTransientAuthorityAndPresenceLoss(t *testing.T) {
	for _, test := range []struct {
		name   string
		gate   string
		change func(*Runner)
	}{
		{"missing own request authority", "authority", func(runner *Runner) {
			delete(runner.meta, "loan_requests")
		}},
		{"stale vault authority", "authority", func(runner *Runner) {
			meta := runner.meta["vaults"]
			meta.Stale = true
			runner.meta["vaults"] = meta
		}},
		{"absent yard presence", "presence", func(runner *Runner) {
			factors := make([]string, 0, len(runner.stateCache.Factors))
			for _, portID := range runner.stateCache.Factors {
				if portID != recoveryHomePort {
					factors = append(factors, portID)
				}
			}
			runner.stateCache.Factors = factors
			for index := range runner.stateCache.Ships {
				runner.stateCache.Ships[index].Ship.CurrentPortID = recoveryFarPort
			}
		}},
	} {
		t.Run(test.name, func(t *testing.T) {
			h, runner := playerLoanAcceptancePosted(t)
			quote := playerLoanAcceptanceQuote(t, h, runner, 3_000)
			playerLoanAcceptancePublishQuote(t, h, runner, quote)
			test.change(runner)
			playerLoanAcceptanceDrive(t, h, runner)
			mission, data := h.mission("__growth")
			if mission.Phase != "growth_loan_request" || data.PlayerLoan == nil || data.PlayerLoan.RequestID != *quote.RequestID ||
				data.Action != nil || len(h.pending()) != 0 || h.api.mutationCount("/v1/loan/approve") != 0 {
				t.Fatalf("transient loss canceled the request or approved without authority: phase %s, %+v", mission.Phase, data)
			}
			if test.gate != "" {
				playerLoanAcceptanceDecision(t, h, test.gate)
			}
			key := recoveryHomePort + "|" + recoveryHull
			if time.Now().Before(decode[map[string]time.Time](runner.snapshot, "_player_loan_backoffs")[key]) {
				t.Fatal("transient hold permanently abandoned a viable request")
			}
			restarted := h.runner(primaryTestKey)
			loanCapacityRefresh(t, h, restarted)
			playerLoanAcceptanceStageApproval(t, h, restarted)
			playerLoanAcceptanceDispatch(t, h, restarted)
			if h.api.mutationCount("/v1/loan/approve") != 1 || len(h.pending()) != 0 {
				t.Fatal("restored authority did not resume exactly one approval after restart")
			}
		})
	}
}

func TestPlayerLoanApprovalConflictDropsOnlyQuoteAndKeepsRequest(t *testing.T) {
	h, runner := playerLoanAcceptancePosted(t)
	quote := playerLoanAcceptanceQuote(t, h, runner, 3_000)
	playerLoanAcceptancePublishQuote(t, h, runner, quote)
	playerLoanAcceptanceStageApproval(t, h, runner)
	h.api.loanApproveMode = "conflict"
	playerLoanAcceptanceDispatch(t, h, runner)
	loanCapacityRefresh(t, h, runner)
	playerLoanAcceptanceDrive(t, h, runner)
	mission, data := h.mission("__growth")
	if mission.Phase != "growth_loan_request" || data.PlayerLoan == nil || data.PlayerLoan.RequestID != *quote.RequestID || data.Action != nil || len(h.pending()) != 0 {
		t.Fatalf("409 discarded the request or stranded approval: phase %s, %+v", mission.Phase, data)
	}
	key := recoveryHomePort + "|" + recoveryHull
	for _, snapshotKey := range []string{"_capacity_financing_backoffs", "_growth_build_backoffs"} {
		if time.Now().Before(decode[map[string]time.Time](runner.snapshot, snapshotKey)[key]) {
			t.Fatalf("quote-only refusal poisoned the yard/hull backoff %s", snapshotKey)
		}
	}
	playerLoanAcceptanceDecision(t, h, "quote_refused")
	for range 2 {
		playerLoanAcceptanceDrive(t, h, runner)
	}
	if h.api.mutationCount("/v1/loan/approve") != 1 {
		t.Fatal("refused quote was approved again")
	}
	h.api.loanApproveMode = "ok"
	quote.ID = "replacement-kraken-quote"
	playerLoanAcceptancePublishQuote(t, h, runner, quote)
	playerLoanAcceptanceStageApproval(t, h, runner)
	playerLoanAcceptanceDispatch(t, h, runner)
	if h.api.mutationCount("/v1/loan/approve") != 2 {
		t.Fatal("409 blocked a different valid quote on the same request")
	}
}

func TestPlayerLoanApprovalLostResponseReconcilesOneLoan(t *testing.T) {
	h, runner := playerLoanAcceptancePosted(t)
	quote := playerLoanAcceptanceQuote(t, h, runner, 3_000)
	playerLoanAcceptancePublishQuote(t, h, runner, quote)
	playerLoanAcceptanceStageApproval(t, h, runner)
	h.api.loanApproveMode = "drop"
	playerLoanAcceptanceDispatch(t, h, runner)
	h.api.loanApproveMode = "ok"
	restarted := h.runner(primaryTestKey)
	for range 3 {
		if _, err := h.tick(restarted); err != nil {
			t.Fatal(err)
		}
	}
	_, data := h.mission("__growth")
	if data.Capacity == nil || data.Capacity.Financing == nil || len(data.Capacity.Financing.Loans) != 1 || len(restarted.stateCache.Loans) != 1 || h.api.mutationCount("/v1/loan/approve") != 1 {
		t.Fatalf("ambiguous approval did not reconcile exactly one loan: %+v", data)
	}
	loan := data.Capacity.Financing.Loans[0]
	if loan.QuoteID == nil || *loan.QuoteID != quote.ID || loan.RequestID == nil || *loan.RequestID != *quote.RequestID {
		t.Fatalf("reconciled debt does not belong to the recorded quote and request: %+v", loan)
	}
	pending := h.pending()
	if len(pending) > 1 {
		t.Fatalf("approval recovery left %d pending actions; want at most one attached refit", len(pending))
	}
	if len(pending) == 1 {
		action := pending[0]
		var body struct {
			ShipID string `json:"ship_id"`
		}
		if data.BuiltShipID == "" || data.Action == nil || data.Action.Stage != "refit" ||
			data.Action.Path != "/v1/shipyard/modify-ship" || action.ID != data.Action.ID ||
			action.Path != "/v1/shipyard/modify-ship" || json.Unmarshal(action.Body, &body) != nil ||
			body.ShipID != data.BuiltShipID {
			t.Fatalf("approval recovery left an orphaned action instead of an attached post-build refit: %+v", action)
		}
	}
}

func TestPlayerLoanPrincipalIsNotProfitOrSavingsIncome(t *testing.T) {
	h, runner := playerLoanAcceptancePosted(t)
	asOf := time.Now().UTC()
	before, err := runner.capacitySavingsRate(h.ctx, asOf)
	if err != nil {
		t.Fatal(err)
	}
	beforeHistory, err := h.db.EconomicHistory(h.ctx, h.account.ID, "")
	if err != nil {
		t.Fatal(err)
	}
	quote := playerLoanAcceptanceQuote(t, h, runner, 3_000)
	playerLoanAcceptancePublishQuote(t, h, runner, quote)
	playerLoanAcceptanceStageApproval(t, h, runner)
	playerLoanAcceptanceDispatch(t, h, runner)
	loanCapacityRefresh(t, h, runner)
	after, err := runner.capacitySavingsRate(h.ctx, asOf)
	if err != nil {
		t.Fatal(err)
	}
	want := before - float64(*quote.InstallmentPence)/10
	if math.Abs(after-want) > 1e-8 {
		t.Fatalf("borrowing changed savings income: before %f, after %f, want %f with service deduction only", before, after, want)
	}
	afterHistory, err := h.db.EconomicHistory(h.ctx, h.account.ID, "")
	if err != nil {
		t.Fatal(err)
	}
	if string(encode(beforeHistory)) != string(encode(afterHistory)) {
		t.Fatalf("loan receipt became a trade settlement: before %+v, after %+v", beforeHistory, afterHistory)
	}
	_, data := h.mission("__growth")
	if data.ProceedsPence != 0 || data.ExpensesPence != 0 {
		t.Fatalf("loan principal became mission profit or expense: %+v", data)
	}
	if len(runner.stateCache.Loans) != 1 || loanReserve(runner.stateCache.Loans[0]) != 2**quote.InstallmentPence {
		t.Fatal("player debt lacks the same two-installment reserve as Goldsmiths debt")
	}
}

func TestPlayerLoanSurplusPrepaymentServesLienAndUnsecuredDebt(t *testing.T) {
	for _, collateral := range []string{"ship_lien", "unsecured"} {
		t.Run(collateral, func(t *testing.T) {
			h, runner, api, _ := moneyPrepayFixture(t, []int64{563_785}, 1_000_000, false)
			api.mu.Lock()
			requestID := "kraken-servicing-request"
			api.loans[0].LenderCompanyID = krakenAcceptanceCompany
			api.loans[0].RequestID = &requestID
			api.loans[0].CollateralKind = collateral
			if collateral == "unsecured" {
				api.loans[0].CollateralShipID = nil
				api.loans[0].CollateralStatus = "none"
			}
			api.mu.Unlock()
			loanCapacityRefresh(t, h, runner)
			moneyLoanWork(t, h, runner, 8)
			if h.api.mutationCount("/v1/loan/repay") != 1 || len(h.pending()) != 0 {
				t.Fatal("player loan surplus prepayment was skipped, repeated or left pending")
			}
			api.mu.Lock()
			loan := api.loans[0]
			api.mu.Unlock()
			if loan.Status != "repaid" || loan.OwedPence != 0 {
				t.Fatalf("player debt was not fully serviced: %+v", loan)
			}
			restarted := h.runner(primaryTestKey)
			restarted.snapshot["_capacity_growth_paused_until"] = encode(time.Now().Add(time.Hour))
			loanCapacityRefresh(t, h, restarted)
			moneyLoanWork(t, h, restarted, 3)
			if h.api.mutationCount("/v1/loan/repay") != 1 {
				t.Fatal("restart repaid an already closed player loan")
			}
		})
	}
}
