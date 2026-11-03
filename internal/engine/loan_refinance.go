package engine

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"math"
	"reflect"
	"slices"
	"time"

	"github.com/DevYukine/go-tradewinds/internal/gameapi"
	"github.com/DevYukine/go-tradewinds/internal/store"
	"github.com/google/uuid"
)

const loanRefinanceMission = "__loan_refinance"

var errLoanRefinanceReceipt = errors.New("refinance repayment waits for its named disbursed unsecured loan")

type loanRefinanceState struct {
	OldLoanID       string             `json:"old_loan_id"`
	RequestID       string             `json:"request_id"`
	NewLoanID       string             `json:"new_loan_id"`
	State           string             `json:"state"`
	RetryAt         time.Time          `json:"retry_at,omitempty"`
	PayoffAtPosting int64              `json:"payoff_at_posting,omitempty"`
	ApprovedQuote   *gameapi.LoanQuote `json:"approved_quote,omitempty"`
	RepayAttempts   int                `json:"repay_attempts,omitempty"`
	RequestDueKey   string             `json:"request_due_key,omitempty"`
}

func (r *Runner) saveLoanRefinance(ctx context.Context, state loanRefinanceState) error {
	r.snapshot["_loan_refinance"] = encode(state)
	return r.saveSnapshot(ctx)
}

func loanRefinanceEarmark(state loanRefinanceState, loan gameapi.Loan) int64 {
	if state.OldLoanID != loan.ID || state.NewLoanID == "" && state.State != "approve" {
		return 0
	}
	return max(0, loanFullPayment(loan)-loanReserve(loan))
}

func reserveLoanRefinance(required map[string]int64, state AccountState, snapshot map[string]json.RawMessage) {
	refinance := decode[loanRefinanceState](snapshot, "_loan_refinance")
	for _, loan := range state.Loans {
		if loan.ID == refinance.OldLoanID && loan.OwedPence > 0 {
			required[loan.BankingRegion] = growthCashTotal(required[loan.BankingRegion], loanRefinanceEarmark(refinance, loan))
			return
		}
	}
}

func (r *Runner) loanRefinanceNeeded(loan gameapi.Loan) (bool, string) {
	lower, _, ok := r.serverBounds()
	if !ok || !debtServiceHoldLoan(loan, r.account.CompanyID) || loan.Repayment != "amortised" ||
		loan.InstallmentPence == nil || *loan.InstallmentPence <= 0 || loan.RateBasis != "annual" ||
		loan.AnnualRateBPS == nil || *loan.AnnualRateBPS < 0 || loan.MaturesAt == nil || loan.NextPaymentDueAt == nil ||
		r.stateCache.Clock.Paused || r.stateCache.Clock.Calendar.TimeScale <= 0 {
		return false, "refinance requires an active amortised unsecured loan and a running authoritative clock"
	}
	unsecured := 0
	for _, debt := range r.stateCache.Loans {
		if debt.BorrowerCompanyID == r.account.CompanyID && (debt.Status == "active" || debt.Status == "defaulted") &&
			debt.CollateralKind == "unsecured" && !debt.IsMortgage {
			unsecured++
			if unsecured > 1 {
				return false, "refinance blocked by multiple own active or defaulted unsecured loans"
			}
		}
	}
	// Count the remaining ten-game-day payments from the authoritative clock.
	// The present-value annuity ratio is the payment reduction at an equal rate.
	remaining := math.Ceil(loan.MaturesAt.Sub(lower).Hours() * float64(r.stateCache.Clock.Calendar.TimeScale) / 240)
	if remaining < 1 || remaining > 36 {
		return false, "refinance no longer meets the remaining-payment schedule gate"
	}
	periodic := float64(*loan.AnnualRateBPS) / 10_000 / 36
	ratio := remaining / 36
	if periodic > 0 {
		ratio = math.Expm1(-remaining*math.Log1p(periodic)) / math.Expm1(-36*math.Log1p(periodic))
	}
	if ratio > 0.9 {
		return false, "refinance no longer meets the ten-percent annuity gate"
	}
	window := time.Duration(float64(72*time.Hour) / float64(r.stateCache.Clock.Calendar.TimeScale))
	if loan.NextPaymentDueAt.After(lower.Add(window)) {
		return false, "next installment is outside the three-game-day emergency window"
	}
	if r.loanRefinanceCash(loan) >= loanRefinancePayment(loan) {
		return false, "all-vault cash covers this installment after earlier or simultaneous loan payments; hold cash instead of refinancing"
	}
	return true, ""
}

func loanRefinancePrincipal(loan gameapi.Loan) int64 {
	payoff := loanFullPayment(loan)
	principal := payoff / 100_000 * 100_000
	if payoff%100_000 != 0 {
		principal = growthCashTotal(principal, 100_000)
	}
	return principal
}

func (r *Runner) finishLoanRefinance(ctx context.Context, state loanRefinanceState, kind, reason string) (bool, error) {
	if state.RequestID != "" {
		r.snapshot["_loan_refinance_retry_at"] = encode(time.Now().UTC().Add(24 * time.Hour))
	}
	if kind == "loan_refinance_quarantined" || kind == "loan_refinance_repay_exhausted" {
		backoffs := decode[map[string]time.Time](r.snapshot, "_loan_refinance_backoffs")
		if backoffs == nil {
			backoffs = make(map[string]time.Time)
		}
		now := time.Now()
		maps.DeleteFunc(backoffs, func(_ string, until time.Time) bool { return !now.Before(until) })
		backoffs[state.OldLoanID] = r.gameDelay(10 * 24)
		r.snapshot["_loan_refinance_backoffs"] = encode(backoffs)
	}
	if err := r.decision(ctx, loanRefinanceMission, kind, reason, state); err != nil {
		return false, err
	}
	if err := r.db.DeleteMission(ctx, r.account.ID, loanRefinanceMission); err != nil {
		return false, err
	}
	delete(r.snapshot, "_loan_refinance")
	return true, r.saveSnapshot(ctx)
}

func (r *Runner) refreshLoanRefinanceLoan(ctx context.Context, id string) (gameapi.Loan, bool, error) {
	tasks := []fetchTask{
		{key: "loans", path: "/v1/loan/list?role=borrower", kind: fetchPages},
		{key: "vaults", path: "/v1/treasury/vaults"},
	}
	results, err := fetchWave(ctx, r.api, tasks, time.Time{})
	if err != nil {
		return gameapi.Loan{}, false, err
	}
	if results[0].err == nil {
		results[0].err = validateBorrowerLoans(results[0].data, r.account.CompanyID)
	}
	var history []gameapi.Loan
	if results[0].err == nil {
		results[0].err = json.Unmarshal(results[0].data, &history)
	}
	var found gameapi.Loan
	exists := false
	active := make([]gameapi.Loan, 0, len(history))
	for _, loan := range history {
		if loan.ID == id {
			found, exists = loan, true
		}
		if loan.Status == "active" || loan.Status == "defaulted" {
			active = append(active, loan)
		}
	}
	// Full history proves retirement without publishing closed loans as debt
	// commitments to the ordinary planner.
	if results[0].err == nil {
		results[0].data = encode(active)
	}
	for index, task := range tasks {
		r.applyCapture(ctx, task.key, results[index])
	}
	r.stateCache.Loans = decode[[]gameapi.Loan](r.snapshot, "loans")
	r.stateCache.Vaults = r.state().Vaults
	r.snapshot["_meta"] = encode(r.meta)
	if err := r.saveSnapshot(ctx); err != nil {
		return gameapi.Loan{}, false, err
	}
	return found, exists, nil
}

func (r *Runner) loanRefinanceDisbursement(state loanRefinanceState, old, loan gameapi.Loan) bool {
	quote := state.ApprovedQuote
	return quote != nil && quote.RequestID != nil && *quote.RequestID == state.RequestID &&
		debtServiceHoldLoan(loan, r.account.CompanyID) && loan.ID == state.NewLoanID && loan.ID != old.ID &&
		loan.BankingRegion == old.BankingRegion && loan.CollateralShipID == nil &&
		quote.TermGameDays == 360 && quote.Repayment == "amortised" && quote.CollateralShipID == nil &&
		loanQuoteIdentity(loan, *quote) && matchesLoanQuote(loan, *quote, r.account.CompanyID)
}

func (r *Runner) loanRefinanceHoldsPrepay() bool {
	state := decode[loanRefinanceState](r.snapshot, "_loan_refinance")
	if state.OldLoanID == "" {
		return false
	}
	if state.NewLoanID != "" || state.State == "approve" || state.State == "repay" || state.State == "repay_wait" {
		return true
	}
	_, upper, ok := r.serverBounds()
	if !ok {
		return false
	}
	for _, request := range r.stateCache.LoanRequests {
		if request.ID == state.RequestID && request.BorrowerCompanyID == r.account.CompanyID &&
			request.Status == "open" && upper.Before(request.ExpiresAt) {
			return true
		}
	}
	return false
}

func (r *Runner) loanServiceReserves(ctx context.Context, excludedLoan string) (map[string]int64, error) {
	snapshot := maps.Clone(r.snapshot)
	delete(snapshot, "_loan_refinance")
	return r.treasuryReservesFor(ctx, snapshot, excludedLoan)
}

func (r *Runner) resetUnsentLoanRefinanceApproval(ctx context.Context, data *missionData) error {
	state := decode[loanRefinanceState](r.snapshot, "_loan_refinance")
	if state.NewLoanID == "" && data.Action != nil && data.Action.Stage == "loan-accept" {
		state.State, state.ApprovedQuote = "wait", nil
		data.Capacity = nil
		return r.saveLoanRefinance(ctx, state)
	}
	return nil
}

func (r *Runner) driveLoanRefinance(ctx context.Context, mission *store.Mission, data *missionData) (bool, error) {
	if !r.opts.Execute || !r.loanAuthority() || !r.fresh("vaults") || !r.fresh("clock") ||
		r.stateCache.Clock.Paused || r.stateCache.Clock.Calendar.TimeScale <= 0 {
		return false, nil
	}
	state := decode[loanRefinanceState](r.snapshot, "_loan_refinance")
	if data.Action != nil || time.Now().Before(data.CooldownUntil) {
		r.wakeBy(data.CooldownUntil)
		return false, nil
	}
	if state.OldLoanID == "" {
		if mission != nil {
			return true, r.db.DeleteMission(ctx, r.account.ID, mission.ShipID)
		}
		if !r.loanRequestAuthority() {
			return false, nil
		}
		backoffs := decode[map[string]time.Time](r.snapshot, "_loan_refinance_backoffs")
		now := time.Now()
		for _, loan := range r.stateCache.Loans {
			if !debtServiceHoldLoan(loan, r.account.CompanyID) {
				continue
			}
			if until := backoffs[loan.ID]; now.Before(until) {
				r.wakeBy(until)
				continue
			}
			needed, _ := r.loanRefinanceNeeded(loan)
			allowed, _ := r.loanRefinanceMayRequest(loan)
			if needed && allowed {
				state.OldLoanID, state.State = loan.ID, "request"
				break
			}
		}
		if state.OldLoanID == "" {
			return false, nil
		}
	}
	if state.NewLoanID != "" && time.Now().Before(state.RetryAt) {
		r.wakeBy(state.RetryAt)
		return false, nil
	}
	old, exists := r.loanByID(state.OldLoanID)
	if !exists || state.State == "repay_wait" {
		var err error
		old, exists, err = r.refreshLoanRefinanceLoan(ctx, state.OldLoanID)
		if err != nil {
			return false, err
		}
		if !r.loanAuthority() || !r.fresh("vaults") {
			return false, r.hold(ctx, loanRefinanceMission, "refinance awaits fresh borrower history and cash authority")
		}
	}
	if !exists {
		return r.finishLoanRefinance(ctx, state, "loan_refinance_stopped", "fresh borrower history no longer contains the old loan; any replacement loan remains ordinary debt")
	}
	if old.OwedPence == 0 && (old.Status == "repaid" || old.Status == "recovered") {
		return r.finishLoanRefinance(ctx, state, "loan_refinance_done", "fresh borrower loans confirm the old loan is repaid")
	}
	if !debtServiceHoldLoan(old, r.account.CompanyID) {
		return r.finishLoanRefinance(ctx, state, "loan_refinance_stopped", "the old loan is no longer active unsecured debt; any replacement loan remains ordinary debt")
	}
	if mission == nil {
		mission = &store.Mission{AccountID: r.account.ID, ShipID: loanRefinanceMission, Phase: "loan_refinance_request", Generation: uuid.NewString()}
		*data = missionData{Plan: Plan{Kind: "treasury"}, CostsKnown: true, StartedAt: time.Now().UTC()}
	}
	if state.ApprovedQuote == nil && data.Capacity != nil && data.Capacity.Financing != nil && len(data.Capacity.Financing.Quotes) == 1 {
		state.ApprovedQuote = &data.Capacity.Financing.Quotes[0]
	}
	if state.NewLoanID == "" && state.RequestID != "" {
		for _, loan := range r.stateCache.Loans {
			if loan.RequestID == nil || *loan.RequestID != state.RequestID || loan.ID == old.ID {
				continue
			}
			if state.NewLoanID != "" {
				return false, errors.New("multiple loans match the recorded refinance request")
			}
			state.NewLoanID, state.State = loan.ID, "repay"
		}
	}
	if state.NewLoanID != "" {
		newLoan, found := r.loanByID(state.NewLoanID)
		if !found {
			var err error
			newLoan, found, err = r.refreshLoanRefinanceLoan(ctx, state.NewLoanID)
			if err != nil {
				return false, err
			}
			if !r.loanAuthority() || !r.fresh("vaults") {
				return false, r.hold(ctx, loanRefinanceMission, "refinance awaits fresh borrower history and cash authority")
			}
			if !found {
				return false, r.decision(ctx, loanRefinanceMission, "loan_refinance_wait", errLoanRefinanceReceipt.Error(), state)
			}
		}
		if newLoan.Status != "active" || newLoan.OwedPence <= 0 {
			return r.finishLoanRefinance(ctx, state, "loan_refinance_stopped", "fresh borrower history confirms the replacement loan is no longer active debt; retire the controller and leave the old loan as ordinary debt")
		}
		if !r.loanRefinanceDisbursement(state, old, newLoan) ||
			data.Capacity != nil && data.Capacity.Financing != nil && data.Capacity.Financing.Cancel {
			return r.finishLoanRefinance(ctx, state, "loan_refinance_quarantined", "refinance disbursement differs from the approved quote; retire the controller and leave both loans as ordinary debt")
		}
		if state.State == "repay_wait" {
			if state.RepayAttempts >= 3 {
				return r.finishLoanRefinance(ctx, state, "loan_refinance_repay_exhausted", "old-loan payoff remains unconfirmed after three failed attempts; retire the controller and leave both loans as ordinary debt")
			}
			state.State, state.RetryAt = "repay", r.gameDelay(1)
			if err := r.decision(ctx, loanRefinanceMission, "loan_refinance_repay_retry", "fresh borrower history still shows old debt; retry the full payoff after one game hour", state); err != nil {
				return false, err
			}
			return true, r.saveLoanRefinance(ctx, state)
		}
		for _, loan := range r.stateCache.Loans {
			if loan.ID != old.ID && loan.Status == "active" && loan.ArrearsPence > 0 {
				return false, nil
			}
		}
		if err := r.saveLoanRefinance(ctx, state); err != nil {
			return false, err
		}
		if err := r.saveMission(ctx, mission, data); err != nil {
			return false, err
		}
		return r.repayLoanRefinance(ctx, mission, data, state, old)
	}
	if state.State == "backoff" {
		if allowed, _ := r.loanRefinanceMayRequest(old); !allowed {
			return false, nil
		}
	}
	if state.State == "backoff" {
		state.State, state.RequestID, state.RequestDueKey, state.RetryAt = "request", "", "", time.Time{}
		data.PlayerLoan, data.Capacity = nil, nil
	}
	needed, reason := r.loanRefinanceNeeded(old)
	if state.RequestID == "" && !needed {
		return r.finishLoanRefinance(ctx, state, "loan_refinance_stopped", reason+"; no refinance request or replacement was acquired, so surplus prepayment may resume")
	}
	if !r.loanRequestAuthority() {
		return false, nil
	}
	if data.PlayerLoan == nil {
		lower, _, ok := r.serverBounds()
		if !ok {
			return false, nil
		}
		data.PlayerLoan = &playerLoanIntent{RequestID: state.RequestID, BankingRegion: old.BankingRegion,
			PrincipalPence: loanRefinancePrincipal(old), TermGameDays: 360, Repayment: "amortised",
			CollateralKind: "unsecured", OpenGameHours: 72, PostedAt: lower.Add(-playerLoanClockSkew)}
		if state.RequestID != "" {
			if request := r.recordedPlayerRequest(data.PlayerLoan); request != nil {
				data.PlayerLoan.PrincipalPence, data.PlayerLoan.PostedAt = request.PrincipalPence, request.CreatedAt
				data.PlayerLoan.ExpiresAt = request.ExpiresAt
			}
		}
	}
	intent := data.PlayerLoan
	if intent.RequestID == "" {
		for _, request := range r.stateCache.LoanRequests {
			candidate := *intent
			candidate.PostedAt = request.CreatedAt
			if request.Status == "open" && playerRequestMatches(request, &candidate, r.account.CompanyID) {
				intent.PostedAt, intent.RequestID, intent.ExpiresAt = request.CreatedAt, request.ID, request.ExpiresAt
				state.RequestID, state.State = request.ID, "wait"
				r.recordLoanRefinanceDue(old, state.RequestDueKey)
				break
			}
		}
		if intent.RequestID == "" {
			if allowed, _ := r.loanRefinanceMayRequest(old); !allowed {
				return false, nil
			}
			if r.openOwnLoanRequest() {
				return false, nil
			}
			for _, loan := range r.stateCache.Loans {
				if loan.Status == "active" && loan.ArrearsPence > 0 {
					return false, nil
				}
			}
			intent.PrincipalPence = loanRefinancePrincipal(old)
			if intent.PrincipalPence <= 0 || intent.PrincipalPence > maximumPence {
				return false, errors.New("refinance payoff exceeds the supported principal")
			}
			state.RequestDueKey = loanRefinanceDueKey(old)
			state.State, state.PayoffAtPosting = "request", loanFullPayment(old)
			if err := r.saveLoanRefinance(ctx, state); err != nil {
				return false, err
			}
			mission.Phase = "loan_refinance_request"
			if err := r.decision(ctx, loanRefinanceMission, "loan_refinance_request", fmt.Sprintf("request %d Cr over 360 game days to reduce installments on loan %s", intent.PrincipalPence/100, old.ID), intent.body()); err != nil {
				return false, err
			}
			return true, r.stage(ctx, mission, data, ShipState{}, "loan-request", "/v1/loan/request", intent.body())
		}
	}
	request := r.recordedPlayerRequest(intent)
	if request == nil || !playerRequestMatches(*request, intent, r.account.CompanyID) {
		return false, r.decision(ctx, loanRefinanceMission, "loan_refinance_wait", "refinance waits for its recorded borrower request", state)
	}
	_, upper, ok := r.serverBounds()
	if !ok {
		return false, nil
	}
	if request.Status == "funded" {
		return false, r.decision(ctx, loanRefinanceMission, "loan_refinance_wait", "refinance waits for its quote-specific disbursed loan", state)
	}
	if request.Status != "open" || !upper.Before(request.ExpiresAt) {
		if !needed {
			return r.finishLoanRefinance(ctx, state, "loan_refinance_stopped", "unfunded refinance request lapsed; "+reason+"; surplus prepayment may resume")
		}
		state.RequestID = ""
		data.PlayerLoan = nil
		return r.deferLoanRefinance(ctx, mission, data, state, "loan_refinance_wait", "refinance request lapsed without funding; wait at least 24 real hours and never repost for this installment due date")
	}
	state.State = "wait"
	if err := r.saveLoanRefinance(ctx, state); err != nil {
		return false, err
	}
	if err := r.saveMission(ctx, mission, data); err != nil {
		return false, err
	}
	if !needed {
		r.wakeAtServer(request.ExpiresAt, true)
		return false, r.decision(ctx, loanRefinanceMission, "loan_refinance_wait", reason+"; let the unfunded refinance request lapse without approving more debt", state)
	}
	for _, loan := range r.stateCache.Loans {
		if loan.ID != old.ID && loan.Status == "active" && loan.ArrearsPence > 0 {
			return false, nil
		}
	}
	quote := r.loanRefinanceLender(old, *request, intent)
	if quote == nil {
		r.wakeAtServer(request.ExpiresAt, true)
		kind, reason := "loan_refinance_wait", "refinance awaits lender quotes while ordinary trading continues"
		if len(request.Quotes) != 0 {
			kind, reason = "loan_refinance_no_quote", "no refinance quote reduces installments by ten percent within twice the Goldsmiths rate"
		}
		return false, r.decision(ctx, loanRefinanceMission, kind, reason, state)
	}
	data.Capacity = &capacityIntent{Financing: &capacityFinancing{Quotes: []gameapi.LoanQuote{*quote}}}
	state.ApprovedQuote = quote
	if err := r.saveLoanRefinance(ctx, state); err != nil {
		return false, err
	}
	if err := r.decision(ctx, loanRefinanceMission, "loan_refinance_approve", "approve the lowest-rate refinance quote that reduces installments by at least ten percent", quote); err != nil {
		return false, err
	}
	mission.Phase = "loan_refinance_approve"
	if err := r.stage(ctx, mission, data, ShipState{}, "loan-accept", "/v1/loan/approve", gameapi.QuoteActionRequest{QuoteID: quote.ID}); err != nil {
		return false, err
	}
	if data.Action != nil {
		state.State = "approve"
	}
	return data.Action != nil, r.saveLoanRefinance(ctx, state)
}

func (r *Runner) loanRefinanceLender(old gameapi.Loan, request gameapi.LoanRequestView, intent *playerLoanIntent) *gameapi.LoanQuote {
	_, upper, ok := r.serverBounds()
	if !ok || r.stateCache.Clock.Calendar.TimeScale <= 0 || !debtServiceHoldLoan(old, r.account.CompanyID) ||
		old.InstallmentPence == nil || *old.InstallmentPence <= 0 || request.Status != "open" ||
		!upper.Add(time.Duration(float64(time.Hour)/float64(r.stateCache.Clock.Calendar.TimeScale))).Before(request.ExpiresAt) ||
		!playerRequestMatches(request, intent, r.account.CompanyID) {
		return nil
	}
	limit := *old.InstallmentPence/10*9 + *old.InstallmentPence%10*9/10
	margin := upper.Add(time.Duration(float64(time.Hour) / float64(r.stateCache.Clock.Calendar.TimeScale)))
	postingPayoff := decode[loanRefinanceState](r.snapshot, "_loan_refinance").PayoffAtPosting
	if postingPayoff == 0 {
		postingPayoff = intent.PrincipalPence
	}
	var best *gameapi.LoanQuote
	for index := range request.Quotes {
		quote := &request.Quotes[index]
		if quote.ID == "" || quote.Status != "open" || slices.Contains(intent.RejectedQuotes, quote.ID) || !margin.Before(quote.ExpiresAt) ||
			quote.RequestID == nil || *quote.RequestID != request.ID || quote.BorrowerCompanyID != r.account.CompanyID ||
			quote.LenderCompanyID == "" || quote.LenderCompanyID == r.account.CompanyID || quote.BankingRegion != old.BankingRegion ||
			quote.PrincipalPence != intent.PrincipalPence || quote.PrincipalPence < postingPayoff ||
			quote.TermGameDays != 360 || quote.Repayment != "amortised" || quote.CollateralShipID != nil ||
			quote.InstallmentPence == nil || *quote.InstallmentPence <= 0 || *quote.InstallmentPence > limit ||
			quote.AnnualRateBPS < 0 || int64(quote.AnnualRateBPS) > 2*int64(request.GoldsmithsRateBPS) {
			continue
		}
		if best == nil || quote.AnnualRateBPS < best.AnnualRateBPS || quote.AnnualRateBPS == best.AnnualRateBPS && quote.ID < best.ID {
			best = quote
		}
	}
	return best
}

func (r *Runner) loanRefinanceRepaymentInput(ctx context.Context, state loanRefinanceState, old gameapi.Loan) (PlanningInput, error) {
	newLoan, exists := r.loanByID(state.NewLoanID)
	if !exists || !r.loanRefinanceDisbursement(state, old, newLoan) {
		return PlanningInput{}, errLoanRefinanceReceipt
	}
	input, err := r.committedPlanningInputExceptLoan(ctx, ShipState{}, loanRefinanceMission, old.ID, newLoan.ID)
	if err != nil {
		return input, err
	}
	if loanReserve(old) == math.MaxInt64 || loanReserve(newLoan) == math.MaxInt64 ||
		loanFullPayment(old) <= 0 || loanFullPayment(old) > maximumPence ||
		growthCashTotal(input.ReserveByRegion[old.BankingRegion], loanFullPayment(old)) == math.MaxInt64 {
		return input, errors.New("refinance repayment has unrepresentable protected obligations or payoff")
	}
	for _, reserve := range input.ReserveByRegion {
		if reserve == math.MaxInt64 {
			return input, errors.New("refinance repayment has unrepresentable protected obligations")
		}
	}
	return input, nil
}

func (r *Runner) repayLoanRefinance(ctx context.Context, mission *store.Mission, data *missionData, state loanRefinanceState, old gameapi.Loan) (bool, error) {
	input, err := r.loanRefinanceRepaymentInput(ctx, state, old)
	if err != nil {
		if errors.Is(err, errLoanRefinanceReceipt) {
			return false, r.decision(ctx, loanRefinanceMission, "loan_refinance_wait", err.Error(), state)
		}
		return false, err
	}
	required := clonePence(input.ReserveByRegion)
	required[old.BankingRegion] = growthCashTotal(required[old.BankingRegion], loanFullPayment(old))
	if input.Balances[old.BankingRegion] < required[old.BankingRegion] {
		if changed, err := r.fundTreasuryMission(ctx, input.Balances, required, "__treasury", &loanRepaymentIntent{Before: old}, mission); changed || err != nil {
			return changed, err
		}
		return false, r.decision(ctx, loanRefinanceMission, "loan_refinance_wait", "refinance payoff awaits regional funding above the operating floor and trading commitments", state)
	}
	if err := r.decision(ctx, loanRefinanceMission, "loan_refinance_repay", "repay the old loan in full from its earmarked refinance proceeds", state); err != nil {
		return false, err
	}
	mission.Phase = "loan_refinance_repay"
	return r.stageLoanRepayment(ctx, mission, data, old, 0)
}

func (r *Runner) loanRefinancePreflight(ctx context.Context, data *missionData) error {
	state := decode[loanRefinanceState](r.snapshot, "_loan_refinance")
	old, exists := r.loanByID(state.OldLoanID)
	if !r.loanAuthority() || !r.fresh("vaults") || !exists || !debtServiceHoldLoan(old, r.account.CompanyID) {
		return errors.New("refinance requires fresh loans, cash and its owned active unsecured old loan")
	}
	for _, loan := range r.stateCache.Loans {
		if loan.Status == "active" && loan.ArrearsPence > 0 && (data.Action.Stage == "loan-request" || loan.ID != old.ID) {
			return errors.New("refinance waits for priority loan arrears cure")
		}
	}
	if data.Action.Stage == "loan-repay" {
		var body gameapi.RepayLoanRequest
		if state.State != "repay" || json.Unmarshal(data.Action.Body, &body) != nil || body.LoanID != old.ID || body.AmountPence != nil ||
			data.LoanRepayment == nil || data.LoanRepayment.Before.ID != old.ID || data.LoanRepayment.AmountPence != 0 {
			return errors.New("refinance repayment requires its immutable full old-loan payoff")
		}
		input, err := r.loanRefinanceRepaymentInput(ctx, state, old)
		if err != nil {
			return err
		}
		if input.Balances[old.BankingRegion]-loanFullPayment(old) < input.ReserveByRegion[old.BankingRegion] {
			return errors.New("refinance repayment would spend the operating floor or another trading commitment")
		}
		return nil
	}
	if !r.loanRequestAuthority() {
		return errors.New("refinance request and approval require fresh borrower request authority")
	}
	intent := data.PlayerLoan
	if intent == nil || intent.BankingRegion != old.BankingRegion || intent.TermGameDays != 360 || intent.Repayment != "amortised" ||
		intent.CollateralKind != "unsecured" || intent.ShipID != "" || intent.OpenGameHours != 72 {
		return errors.New("refinance requires its recorded unsecured 360-game-day request")
	}
	switch data.Action.Stage {
	case "loan-request":
		var body gameapi.PostLoanRequest
		if state.NewLoanID != "" || intent.RequestID != "" || json.Unmarshal(data.Action.Body, &body) != nil || body != intent.body() ||
			intent.PrincipalPence != loanRefinancePrincipal(old) || r.openOwnLoanRequest() {
			return errors.New("refinance request waits for its exact payoff and the free borrower request slot")
		}
		needed, reason := r.loanRefinanceNeeded(old)
		if allowed, why := r.loanRefinanceMayRequest(old); !allowed {
			return errors.New("refinance request: " + why)
		}
		if !needed {
			return errors.New("refinance request: " + reason)
		}
		return nil
	case "loan-accept":
		needed, reason := r.loanRefinanceNeeded(old)
		if !needed {
			return errors.New("refinance approval: " + reason)
		}
		var body gameapi.QuoteActionRequest
		request := r.recordedPlayerRequest(intent)
		if state.NewLoanID != "" || state.RequestID != intent.RequestID || request == nil ||
			json.Unmarshal(data.Action.Body, &body) != nil || data.Capacity == nil || data.Capacity.Financing == nil ||
			len(data.Capacity.Financing.Quotes) != 1 || len(data.Capacity.Financing.Loans) != 0 {
			return errors.New("refinance approval waits for its recorded open borrower request and quote")
		}
		quote := r.loanRefinanceLender(old, *request, intent)
		if quote == nil || quote.ID != body.QuoteID || body.QuoteID != data.Capacity.Financing.Quotes[0].ID ||
			state.ApprovedQuote == nil || !reflect.DeepEqual(*state.ApprovedQuote, *quote) || data.Capacity.Financing.Cancel {
			return errors.New("refinance approval requires the cheapest quote within the installment and Goldsmiths rate limits")
		}
		return nil
	}
	return errors.New("unknown refinance action")
}

func (r *Runner) loanRefinanceResult(ctx context.Context, mission *store.Mission, data *missionData, repaymentConfirmed bool) (bool, error) {
	state := decode[loanRefinanceState](r.snapshot, "_loan_refinance")
	switch data.Action.Stage {
	case "loan-request":
		state.RequestID, state.State = data.PlayerLoan.RequestID, "wait"
		if old, exists := r.loanByID(state.OldLoanID); exists {
			r.recordLoanRefinanceDue(old, state.RequestDueKey)
		}
		mission.Phase = "loan_refinance_request"
	case "loan-accept":
		loan := data.Capacity.Financing.Loans[0]
		state.NewLoanID, state.State = loan.ID, "repay"
		if state.ApprovedQuote == nil && len(data.Capacity.Financing.Quotes) == 1 {
			state.ApprovedQuote = &data.Capacity.Financing.Quotes[0]
		}
		old, exists := r.loanByID(state.OldLoanID)
		if !exists || data.Capacity.Financing.Cancel || !r.loanRefinanceDisbursement(state, old, loan) {
			data.LastActionID = data.Action.ID
			data.Sequence++
			data.Action = nil
			return r.finishLoanRefinance(ctx, state, "loan_refinance_quarantined", "refinance disbursement differs from the approved quote; retire the controller and leave both loans as ordinary debt")
		}
		mission.Phase = "loan_refinance_repay"
	case "loan-repay":
		state.State, state.RetryAt = "repay_wait", r.gameDelay(1)
		if !repaymentConfirmed {
			state.RepayAttempts++
		}
		mission.Phase = "loan_refinance_repay_wait"
	}
	return false, r.saveLoanRefinance(ctx, state)
}

func (r *Runner) deferLoanRefinance(ctx context.Context, mission *store.Mission, data *missionData, state loanRefinanceState, kind, reason string) (bool, error) {
	state.State, state.RetryAt = "backoff", time.Now().UTC().Add(24*time.Hour)
	r.snapshot["_loan_refinance_retry_at"] = encode(state.RetryAt)
	data.Capacity = nil
	mission.Phase = "loan_refinance_request"
	if err := r.decision(ctx, loanRefinanceMission, kind, reason, state); err != nil {
		return false, err
	}
	if err := r.saveLoanRefinance(ctx, state); err != nil {
		return false, err
	}
	r.wakeBy(state.RetryAt)
	return true, r.saveMission(ctx, mission, data)
}

func (r *Runner) rejectLoanRefinance(ctx context.Context, mission *store.Mission, data *missionData, status int) (bool, error) {
	stage, body := data.Action.Stage, data.Action.Body
	if stage == "loan-accept" {
		if err := r.resetUnsentLoanRefinanceApproval(ctx, data); err != nil {
			return false, err
		}
	}
	data.LastActionID = data.Action.ID
	data.Sequence++
	data.Action = nil
	if status == 401 || status == 403 || status == 429 {
		if err := r.saveMission(ctx, mission, data); err != nil {
			return false, err
		}
		if status != 429 {
			return false, fmt.Errorf("%w after definitive refinance HTTP %d; journal and mission retained", ErrExecutionStopped, status)
		}
		return true, nil
	}
	state := decode[loanRefinanceState](r.snapshot, "_loan_refinance")
	if stage == "loan-repay" {
		state.State, state.RetryAt = "repay_wait", r.gameDelay(1)
		state.RepayAttempts++
		mission.Phase = "loan_refinance_repay_wait"
		if err := r.saveLoanRefinance(ctx, state); err != nil {
			return false, err
		}
		return true, r.saveMission(ctx, mission, data)
	}
	if stage == "loan-accept" {
		if data.PlayerLoan != nil {
			var quote gameapi.QuoteActionRequest
			if json.Unmarshal(body, &quote) == nil {
				data.PlayerLoan.RejectedQuotes = append(data.PlayerLoan.RejectedQuotes, quote.QuoteID)
			}
		}
		mission.Phase = "loan_refinance_request"
		if err := r.decision(ctx, loanRefinanceMission, "loan_refinance_no_quote", "the selected quote was refused; reject that quote and wait for another on the same open request", state); err != nil {
			return false, err
		}
		return true, r.saveMission(ctx, mission, data)
	}
	if old, exists := r.loanByID(state.OldLoanID); exists {
		r.recordLoanRefinanceDue(old, state.RequestDueKey)
	}
	state.RequestID = ""
	data.PlayerLoan = nil
	return r.deferLoanRefinance(ctx, mission, data, state, "loan_refinance_wait", "borrower request was refused; never resend for this installment due date and wait at least 24 real hours")
}
