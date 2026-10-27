package engine

import (
	"context"
	"encoding/json"
	"errors"
	"math"
	"sort"
	"time"

	"github.com/DevYukine/go-tradewinds/internal/gameapi"
	"github.com/DevYukine/go-tradewinds/internal/store"
	"github.com/google/uuid"
)

const (
	goldsmithsMinimumPence = int64(10_000)
	goldsmithsMaximumPence = int64(750_000)
)

type capacityFinancing struct {
	Quotes            []gameapi.LoanQuote `json:"quotes"`
	Loans             []gameapi.Loan      `json:"loans"`
	Decision          GrowthDecision      `json:"decision"`
	ProtectedByRegion map[string]int64    `json:"protected_by_region"`
	DonorByRegion     map[string]int64    `json:"donor_by_region"`
	BuildBy           time.Time           `json:"build_by,omitempty"`
	Cancel            bool                `json:"cancel,omitempty"`
}

type loanRepaymentIntent struct {
	Before      gameapi.Loan `json:"before"`
	AmountPence int64        `json:"amount_pence"` // Zero means repay the full current balance.
}

type capacityLoanDecision struct {
	Borrow                bool     `json:"borrow"`
	Reason                string   `json:"reason"`
	ShortfallPence        int64    `json:"shortfall_pence"`
	LienCapacityPence     *int64   `json:"lien_capacity_pence"`
	PrincipalPence        int64    `json:"principal_pence"`
	InstallmentPence      *int64   `json:"installment_pence"`
	DebtReservePence      int64    `json:"debt_reserve_pence"`
	Coverage              *float64 `json:"half_net_installment_coverage"`
	NetAfterInterestPence float64  `json:"net_after_daily_interest_pence"`
}

func (r *Runner) loanAuthority() bool {
	// Successful captures validate the liability fields before publishing metadata.
	raw := r.snapshot["loans"]
	return r.fresh("loans") && r.meta["loans"].Complete && len(raw) > 0 && raw[0] == '['
}

func loanReserve(loan gameapi.Loan) int64 {
	if loan.Status != "active" && loan.Status != "defaulted" {
		return 0
	}
	reserve := max(0, loan.OwedPence)
	if loan.Status == "active" && loan.Repayment == "amortised" && loan.InstallmentPence != nil {
		reserve = growthCashTotal(*loan.InstallmentPence, *loan.InstallmentPence)
	}
	if loan.NextPaymentPence != nil {
		reserve = max(reserve, *loan.NextPaymentPence)
	}
	return max(reserve, loan.ArrearsPence)
}

func loanDailyInterest(loan gameapi.Loan) float64 {
	if loan.RateBasis == "annual" && loan.AnnualRateBPS != nil {
		return float64(loan.OutstandingPrincipalPence) * float64(*loan.AnnualRateBPS) / 10_000 / 360
	}
	return float64(loan.OutstandingPrincipalPence) * float64(loan.DailyRateBPS) / 10_000
}

func loanFullPayment(loan gameapi.Loan) int64 {
	return growthCashTotal(loan.OwedPence, int64(math.Ceil(loanDailyInterest(loan))))
}

func evaluateCapacityLoan(decision GrowthDecision, shortfall, capacity, principal, installment int64, annualRate int32) capacityLoanDecision {
	result := capacityLoanDecision{
		ShortfallPence: shortfall, LienCapacityPence: &capacity, PrincipalPence: principal,
		InstallmentPence: &installment, DebtReservePence: growthCashTotal(installment, installment),
	}
	if !decision.EconomicallyViable || decision.Accepted || shortfall <= 0 {
		result.Reason = "borrow only for an economically admitted depth-backed capacity candidate blocked solely by a positive total cash shortfall"
		return result
	}
	if principal < goldsmithsMinimumPence || principal > capacity || installment <= 0 || result.DebtReservePence == math.MaxInt64 {
		result.Reason = "available liens cannot cover the full shortfall and two installments"
		return result
	}
	coverage := decision.IncrementalNetPencePerGameDay * 5 / float64(installment)
	result.Coverage = &coverage
	result.NetAfterInterestPence = decision.IncrementalNetPencePerGameDay - float64(principal)*float64(annualRate)/10_000/360
	if math.IsNaN(coverage) || math.IsInf(coverage, 0) || coverage+1e-9 < 1.25 || result.NetAfterInterestPence <= 0 {
		result.Reason = "half the repeatable marginal net must cover ten-day installments by at least 1.25x after interest"
		return result
	}
	result.Borrow = true
	result.Reason = "Goldsmiths proceeds fund the cash-only shortfall and two installments with at least 1.25x half-net coverage"
	return result
}

// This local estimate never creates server quotes. Split evenly across the
// minimum number of liens; exact quoted installments must verify the package.
func estimatedCapacityPrincipal(shortfall, ships int64) (int64, int64) {
	fraction := (0.30 / 36) / (1 - math.Pow(1+0.30/36, -36))
	principal := shortfall
	for principal > 0 && principal <= ships*goldsmithsMaximumPence {
		count := (principal + goldsmithsMaximumPence - 1) / goldsmithsMaximumPence
		part, extra := principal/count, principal%count
		installment := (count-extra)*int64(math.Ceil(float64(part)*fraction)) + extra*int64(math.Ceil(float64(part+1)*fraction))
		next := growthCashTotal(shortfall, installment, installment)
		if next == principal {
			return principal, installment
		}
		principal = next
	}
	return principal, 0
}

func capacitySavingsFinancingWorthwhile(buy, save GrowthDecision, savingsRate float64, fleetSize int) bool {
	if savingsRate <= 0 || save.CashShortfallPence <= 0 || fleetSize <= 0 {
		return false
	}
	principal, installment := estimatedCapacityPrincipal(save.CashShortfallPence, int64(fleetSize))
	if installment <= 0 || principal < goldsmithsMinimumPence {
		return false
	}
	wait := float64(save.CashShortfallPence) / savingsRate
	interest := 36*float64(installment) - float64(principal)
	return (save.DepthNetPencePerGameDay+buy.IncrementalNetPencePerGameDay)*wait > interest
}

func (r *Runner) considerCapacitySavingsFinancing(ctx context.Context, buy GrowthDecision, save *capacityCandidate, savingsRate float64, fleetSize int, evidence *growthEvidence) (bool, error) {
	if !capacitySavingsFinancingWorthwhile(buy, save.decision, savingsRate, fleetSize) {
		return false, r.recordCapacityLoan(ctx, save.port, save.hull, capacityLoanDecision{
			ShortfallPence: save.decision.CashShortfallPence,
			Reason:         "no borrowing: savings financing requires available liens and avoided-wait earnings above estimated total interest",
		})
	}
	return r.considerCapacityFinancing(ctx, save.port, save.hull, save.decision, save.required, evidence)
}

func capacityLoanFunding(decision GrowthDecision, evidence *growthEvidence, region string, principal, reserve int64, rates []Rate) (map[string]int64, map[string]int64, map[string]int64, bool) {
	required := clonePence(evidence.required)
	capital := growthCashTotal(decision.QuotedHullPence, decision.SlippageReservePence, decision.CannonsPence, decision.InitialStoresPence)
	required[region] = growthCashTotal(required[region], capital, reserve)
	balances := clonePence(evidence.balances)
	balances[region] = growthCashTotal(balances[region], principal)
	projected, _, transfers, funded := measuredFunding(balances, required, rates)
	if !funded {
		return required, nil, nil, false
	}
	protected := make(map[string]int64, len(required))
	for key, amount := range required {
		protected[key] = max(0, amount-evidence.required[key])
	}
	regions := make([]string, 0, len(projected))
	for key := range projected {
		regions = append(regions, key)
	}
	sort.Strings(regions)
	buffer := decision.WorkingCapitalPence
	for _, key := range regions {
		cash := min(buffer, max(0, projected[key]-required[key]))
		protected[key] = growthCashTotal(protected[key], cash)
		buffer -= cash
	}
	donors := make(map[string]int64)
	for _, transfer := range transfers {
		donors[transfer.Rate.FromRegion] = growthCashTotal(donors[transfer.Rate.FromRegion], transfer.Gross)
	}
	return required, protected, donors, buffer == 0
}

func (r *Runner) validGoldsmithsQuote(quote gameapi.GoldsmithsQuote, shipID, region string, principal int64) bool {
	_, upper, clockOK := r.serverBounds()
	q := quote.Quote
	return clockOK && quote.Recourse && quote.LoanToValueBPS > 0 && quote.LoanToValueBPS <= 4_000 &&
		quote.MaxPrincipalPence >= principal && quote.RebuildValuePence >= principal &&
		q.ID != "" && q.LenderCompanyID != "" && q.BorrowerCompanyID == r.account.CompanyID &&
		q.BankingRegion == region && q.CollateralShipID != nil && *q.CollateralShipID == shipID &&
		q.PrincipalPence == principal && q.TermGameDays == 360 && q.Repayment == "amortised" && q.AnnualRateBPS == 3_000 &&
		q.Status == "open" && q.InstallmentPence != nil && *q.InstallmentPence > 0 &&
		q.TotalRepayablePence >= principal && upper.Before(q.ExpiresAt)
}

func (r *Runner) recordCapacityLoan(ctx context.Context, port Port, hull Hull, decision capacityLoanDecision) error {
	return r.decision(ctx, "", "capacity_financing:"+port.ID+"|"+hull.ID, decision.Reason, decision)
}

func (r *Runner) recordCapacityFinancingRefusal(ctx context.Context, data *missionData) error {
	return r.recordGrowthBackoff(ctx, "_capacity_financing_backoffs", data)
}

func (r *Runner) considerCapacityFinancing(ctx context.Context, port Port, hull Hull, decision GrowthDecision, required map[string]int64, evidence *growthEvidence) (bool, error) {
	result := capacityLoanDecision{ShortfallPence: decision.CashShortfallPence}
	quoted := false
	refuse := func(reason string) (bool, error) {
		result.Borrow = false
		result.Reason = reason
		if quoted {
			if err := r.recordCapacityFinancingRefusal(ctx, &missionData{BuildPort: port.ID, BuildClass: hull.ID}); err != nil {
				return false, err
			}
		}
		return false, r.recordCapacityLoan(ctx, port, hull, result)
	}
	if !decision.EconomicallyViable || decision.Accepted || required == nil || decision.CashShortfallPence <= 0 {
		return refuse("no borrowing: this is not a positive total cash-only capacity shortfall")
	}
	if until := decode[map[string]time.Time](r.snapshot, "_capacity_financing_backoffs")[port.ID+"|"+hull.ID]; time.Now().Before(until) {
		r.wakeBy(until)
		return false, nil
	}
	if !r.loanAuthority() || !r.opts.Execute {
		return refuse("borrowing requires execute mode and fresh complete borrower-loan authority")
	}
	capital := growthCashTotal(decision.QuotedHullPence, decision.SlippageReservePence, decision.CannonsPence, decision.InitialStoresPence)
	if decision.DepthNetPencePerGameDay <= 0 || float64(capital)/decision.DepthNetPencePerGameDay > r.opts.CapacityPaybackDays {
		return refuse("recourse debt requires depth-backed non-charter earnings inside the capacity payback horizon")
	}
	decision.IncrementalNetPencePerGameDay = decision.DepthNetPencePerGameDay
	decision.PaybackGameDays = float64(capital) / decision.DepthNetPencePerGameDay
	var ships []string
	for _, ship := range r.stateCache.Ships {
		if ship.Ship.Status == "sunk" {
			continue
		}
		pledged := false
		for _, loan := range r.stateCache.Loans {
			pledged = pledged || loan.CollateralShipID != nil && *loan.CollateralShipID == ship.Ship.ID &&
				(loan.CollateralStatus == "pledged" || loan.CollateralStatus == "held" || loan.CollateralStatus == "seizure_pending")
		}
		if !pledged {
			ships = append(ships, ship.Ship.ID)
		}
	}
	sort.Strings(ships)
	capacity := int64(len(ships)) * goldsmithsMaximumPence
	principal, estimate := estimatedCapacityPrincipal(decision.CashShortfallPence, int64(len(ships)))
	result = evaluateCapacityLoan(decision, decision.CashShortfallPence, capacity, principal, estimate, 3_000)
	if !result.Borrow {
		return refuse(result.Reason)
	}
	result.Borrow = false
	var quotes []gameapi.LoanQuote
	var installment int64
	for attempt := range 2 {
		if principal < goldsmithsMinimumPence || principal > int64(len(ships))*goldsmithsMaximumPence {
			quoted = false
			return refuse("rounded installments cannot fit a complete package within the available liens and loan minimum")
		}
		count := int((principal + goldsmithsMaximumPence - 1) / goldsmithsMaximumPence)
		quotes, installment = quotes[:0], 0
		quotedCapacity, houseFunds := int64(0), int64(math.MaxInt64)
		for index := range count {
			amount := principal / int64(count)
			if int64(index) < principal%int64(count) {
				amount++
			}
			quoted = true
			quote, err := r.api.GetGoldsmithsQuote(ctx, gameapi.GoldsmithsQuoteRequest{PrincipalPence: amount, TermGameDays: 360, Repayment: "amortised", ShipID: ships[index], BankingRegion: port.BankingRegion})
			if err != nil || !r.validGoldsmithsQuote(quote, ships[index], port.BankingRegion, amount) {
				return refuse("Goldsmiths refused or returned unavailable, expired or mismatched exact terms")
			}
			intrinsic := min(goldsmithsMaximumPence, quote.RebuildValuePence*quote.LoanToValueBPS/10_000)
			quotedCapacity = growthCashTotal(quotedCapacity, min(intrinsic, quote.MaxPrincipalPence))
			if quote.MaxPrincipalPence < intrinsic {
				// The lower quote limit reveals shared regional house funds.
				houseFunds = min(houseFunds, quote.MaxPrincipalPence)
			}
			if principal > houseFunds {
				capacity = houseFunds
				result.LienCapacityPence = &capacity
				return refuse("the house's shared funds cannot cover the whole package")
			}
			quotes = append(quotes, quote.Quote)
			installment = growthCashTotal(installment, *quote.Quote.InstallmentPence)
			capacity = min(quotedCapacity, houseFunds)
		}
		needed := growthCashTotal(decision.CashShortfallPence, installment, installment)
		if principal == needed || attempt == 1 && principal >= needed && principal-needed <= 2*int64(count) {
			break
		}
		if attempt == 1 {
			quoted = false
			return refuse("one corrected quote set still cannot cover the package within two pence per lien of excess")
		}
		principal = needed
	}
	result = evaluateCapacityLoan(decision, decision.CashShortfallPence, capacity, principal, installment, quotes[0].AnnualRateBPS)
	_, protected, donors, funded := capacityLoanFunding(decision, evidence, port.BankingRegion, principal, result.DebtReservePence, r.stateCache.Rates)
	if !result.Borrow || !funded {
		result.Borrow = false
		return refuse("exact financing cannot preserve the admitted capital, cargo buffer, debt reserve and coverage")
	}
	if err := r.recordCapacityLoan(ctx, port, hull, result); err != nil {
		return false, err
	}
	return r.startCapacityGrowth(ctx, port, hull, decision, &capacityFinancing{Quotes: quotes, Decision: decision, ProtectedByRegion: protected, DonorByRegion: donors})
}

func matchesLoanQuote(loan gameapi.Loan, quote gameapi.LoanQuote, companyID string) bool {
	return loan.BorrowerCompanyID == companyID && loan.LenderCompanyID == quote.LenderCompanyID && loan.BankingRegion == quote.BankingRegion &&
		loan.PrincipalPence == quote.PrincipalPence && loan.RateBasis == "annual" && loan.AnnualRateBPS != nil && *loan.AnnualRateBPS == quote.AnnualRateBPS &&
		loan.TermGameDays == quote.TermGameDays && loan.Repayment == quote.Repayment && loan.Recourse &&
		loan.InstallmentPence != nil && quote.InstallmentPence != nil && *loan.InstallmentPence == *quote.InstallmentPence &&
		loan.TotalRepayablePence != nil && *loan.TotalRepayablePence == quote.TotalRepayablePence
}

func loanQuoteIdentity(loan gameapi.Loan, quote gameapi.LoanQuote) bool {
	return loan.ID != "" && loan.QuoteID != nil && *loan.QuoteID == quote.ID && loan.CollateralShipID != nil && quote.CollateralShipID != nil && *loan.CollateralShipID == *quote.CollateralShipID
}

func (r *Runner) loanByID(id string) (gameapi.Loan, bool) {
	for _, loan := range r.stateCache.Loans {
		if loan.ID == id {
			return loan, true
		}
	}
	return gameapi.Loan{}, false
}

func (r *Runner) stageLoanRepayment(ctx context.Context, mission *store.Mission, data *missionData, loan gameapi.Loan, amount int64) (bool, error) {
	data.LoanRepayment = &loanRepaymentIntent{Before: loan, AmountPence: amount}
	var requested *int64
	if amount > 0 {
		requested = &amount
	}
	return true, r.stage(ctx, mission, data, ShipState{}, "loan-repay", "/v1/loan/repay", gameapi.RepayLoanRequest{LoanID: loan.ID, AmountPence: requested})
}

func (r *Runner) serviceLoans(ctx context.Context) (bool, error) {
	if !r.loanAuthority() || !r.fresh("vaults") || !r.fresh("rates") {
		return false, nil
	}
	mission, data, err := r.loadMission(ctx, "__loans")
	if err != nil {
		return false, err
	}
	if mission != nil {
		if data.Action != nil || time.Now().Before(data.CooldownUntil) {
			return false, nil
		}
		return true, r.db.DeleteMission(ctx, r.account.ID, mission.ShipID)
	}
	for _, loan := range r.stateCache.Loans {
		if loan.Status != "active" || loan.ArrearsPence <= 0 {
			continue
		}
		input := r.planningInput(ShipState{})
		required, err := r.treasuryReserves(ctx)
		if err != nil {
			return false, err
		}
		required[loan.BankingRegion] = growthCashTotal(max(0, required[loan.BankingRegion]-loanReserve(loan)), loan.ArrearsPence)
		intent := &loanRepaymentIntent{Before: loan, AmountPence: loan.ArrearsPence}
		if changed, err := r.fundTreasuryMission(ctx, input.Balances, required, "__loans", intent); changed || err != nil {
			return changed, err
		}
		if input.Balances[loan.BankingRegion] < required[loan.BankingRegion] {
			return false, r.hold(ctx, "", "active loan arrears await hard-obligation-safe regional funding")
		}
		mission := &store.Mission{AccountID: r.account.ID, ShipID: "__loans", Phase: "loan_repay", Generation: uuid.NewString()}
		data := missionData{Plan: Plan{Kind: "treasury"}, CostsKnown: true}
		if _, err := r.stageLoanRepayment(ctx, mission, &data, loan, loan.ArrearsPence); err != nil {
			return false, err
		}
		return r.dispatch(ctx, mission, &data)
	}
	return false, nil
}

func (r *Runner) capacityFinanceReady(ctx context.Context, data *missionData) (GrowthDecision, map[string]int64, error) {
	if !r.loanAuthority() || !r.fresh("ships") || !r.fresh("vaults") {
		return GrowthDecision{}, nil, errors.New("capacity financing waits for fresh loan, fleet and cash authority")
	}
	financing := data.Capacity.Financing
	if !financing.BuildBy.IsZero() && !time.Now().Before(financing.BuildBy) {
		financing.Cancel = true
		return financing.Decision, nil, errors.New("capacity financing reached its one-game-day construction deadline")
	}
	_, upper, clockOK := r.serverBounds()
	if !clockOK {
		return financing.Decision, nil, errors.New("capacity financing waits for current server time")
	}
	pending, err := r.db.PendingActions(ctx, r.account.ID)
	if err != nil {
		return financing.Decision, nil, err
	}
	for _, action := range pending {
		if data.Action == nil || action.ID != data.Action.ID {
			return financing.Decision, nil, errors.New("capacity financing waits for another unresolved action")
		}
	}
	for _, id := range []string{"__treasury", "__loans", "__underwriting", "__marque"} {
		mission, current, err := r.loadMission(ctx, id)
		if err != nil {
			return financing.Decision, nil, err
		}
		if mission != nil && current.Action != nil {
			return financing.Decision, nil, errors.New("capacity financing waits for an unresolved control intent")
		}
	}
	for _, accepted := range financing.Loans {
		if _, exists := r.loanByID(accepted.ID); !exists {
			return financing.Decision, nil, errors.New("capacity financing awaits its named authoritative loan")
		}
	}
	var installment int64
	netAfterInterest := financing.Decision.IncrementalNetPencePerGameDay
	for index, quote := range financing.Quotes {
		if index >= len(financing.Loans) && !upper.Before(quote.ExpiresAt) {
			financing.Cancel = true
			return financing.Decision, nil, errors.New("unaccepted Goldsmiths quote expired")
		}
		if quote.InstallmentPence == nil {
			financing.Cancel = true
			return financing.Decision, nil, errors.New("financing lacks exact quoted payment terms")
		}
		installment = growthCashTotal(installment, *quote.InstallmentPence)
		netAfterInterest -= float64(quote.PrincipalPence) * float64(quote.AnnualRateBPS) / 10_000 / 360
	}
	if installment <= 0 || financing.Decision.IncrementalNetPencePerGameDay*5+1e-9 < 1.25*float64(installment) || netAfterInterest <= 0 {
		financing.Cancel = true
		return financing.Decision, nil, errors.New("admitted depth earnings no longer cover the quoted debt")
	}
	input, err := r.committedPlanningInputExcept(ctx, ShipState{}, "__growth")
	if err != nil {
		return financing.Decision, nil, err
	}
	required := clonePence(input.ReserveByRegion)
	port, exists := findPort(r.stateCache.Catalog, data.BuildPort)
	if !exists {
		return financing.Decision, nil, errors.New("capacity financing lacks its yard banking region")
	}
	capital := growthCashTotal(financing.Decision.QuotedHullPence, financing.Decision.SlippageReservePence, financing.Decision.CannonsPence, financing.Decision.InitialStoresPence)
	var protectedDebt int64
	for _, accepted := range financing.Loans {
		loan, _ := r.loanByID(accepted.ID)
		protectedDebt = growthCashTotal(protectedDebt, loanReserve(loan))
	}
	required[port.BankingRegion] = growthCashTotal(required[port.BankingRegion], capital, max(0, growthCashTotal(installment, installment)-protectedDebt))
	if len(financing.Loans) == len(financing.Quotes) {
		projected, _, _, funded := measuredFunding(input.Balances, required, input.Rates)
		var available int64
		for region, balance := range projected {
			available = growthCashTotal(available, max(0, balance-required[region]))
		}
		if !funded || available+2*int64(len(financing.Quotes)) < financing.Decision.WorkingCapitalPence {
			return financing.Decision, nil, errors.New("financed capacity waits for its fleet-wide working buffer and protected capital")
		}
	}
	return financing.Decision, required, nil
}

func (r *Runner) repayCapacityFinancing(ctx context.Context, mission *store.Mission, data *missionData) (bool, error) {
	financing := data.Capacity.Financing
	if mission.Phase != "growth_finance_repay" {
		financing.Cancel, mission.Phase = true, "growth_finance_repay"
		if err := r.saveMission(ctx, mission, data); err != nil {
			return false, err
		}
	}
	if !r.loanAuthority() {
		return false, r.hold(ctx, "", "unused capacity debt awaits fresh borrower-loan authority")
	}
	for _, accepted := range financing.Loans {
		loan, exists := r.loanByID(accepted.ID)
		if !exists {
			return false, r.hold(ctx, "", "unused capacity debt awaits its named authoritative loan")
		}
		if loan.OwedPence <= 0 {
			continue
		}
		input, err := r.committedPlanningInputExcept(ctx, ShipState{}, "__growth")
		if err != nil {
			return false, err
		}
		required := clonePence(input.ReserveByRegion)
		required[loan.BankingRegion] = growthCashTotal(max(0, required[loan.BankingRegion]-loanReserve(loan)), loanFullPayment(loan))
		if input.Balances[loan.BankingRegion] < required[loan.BankingRegion] {
			funding, current, err := r.loadMission(ctx, "__treasury")
			if err != nil {
				return false, err
			}
			if funding != nil && current.Action != nil {
				return false, r.hold(ctx, "", "unused capacity debt awaits the pending treasury funding action")
			}
			if changed, err := r.fundTreasuryMission(ctx, input.Balances, required, "__treasury", &loanRepaymentIntent{Before: loan}); changed || err != nil {
				return changed, err
			}
			return false, r.hold(ctx, "", "unused capacity principal and one day of interest remain protected pending repayment funding")
		}
		return r.stageLoanRepayment(ctx, mission, data, loan, 0)
	}
	return true, r.db.DeleteMission(ctx, r.account.ID, mission.ShipID)
}

func (r *Runner) driveCapacityFinancing(ctx context.Context, mission *store.Mission, data *missionData) (bool, error) {
	financing := data.Capacity.Financing
	if !financing.BuildBy.IsZero() && !time.Now().Before(financing.BuildBy) {
		financing.Cancel = true
	}
	if financing.Cancel || mission.Phase == "growth_finance_repay" || mission.Phase == "growth_rejected" {
		return r.repayCapacityFinancing(ctx, mission, data)
	}
	_, required, err := r.capacityFinanceReady(ctx, data)
	if err == nil {
		_, err = r.capacityYardReady(ctx, data)
	}
	if err != nil {
		if financing.Cancel {
			if saveErr := r.recordCapacityFinancingRefusal(ctx, data); saveErr != nil {
				return false, saveErr
			}
			return r.repayCapacityFinancing(ctx, mission, data)
		}
		return false, r.hold(ctx, "", err.Error())
	}
	if len(financing.Loans) < len(financing.Quotes) {
		quote := financing.Quotes[len(financing.Loans)]
		return true, r.stage(ctx, mission, data, ShipState{}, "loan-accept", "/v1/loan/accept-quote", gameapi.QuoteActionRequest{QuoteID: quote.ID})
	}
	input := r.planningInput(ShipState{})
	if changed, err := r.fundTreasury(ctx, input.Balances, required); changed || err != nil {
		return changed, err
	}
	for region, amount := range required {
		if input.Balances[region] < amount {
			return false, r.hold(ctx, "", "financed capacity waits for protected package funding; admission is retained")
		}
	}
	mission.Phase = "growth_build"
	return true, r.stage(ctx, mission, data, ShipState{}, "build", "/v1/shipyard/build-ship", map[string]any{"port_id": data.BuildPort, "name": data.BuildName, "ship_class": data.BuildClass, "build_mode": "turnkey_cash"})
}

func (r *Runner) loanActionReady(ctx context.Context, mission *store.Mission, data *missionData) error {
	if !r.loanAuthority() || !r.fresh("ships") || !r.fresh("vaults") {
		return errors.New("new loan action requires fresh debt, fleet and cash authority")
	}
	if data.Action.Stage == "loan-accept" {
		if mission.ShipID != "__growth" || data.Capacity == nil || data.Capacity.Financing == nil {
			return errors.New("new borrowing requires its durable capacity financing intent")
		}
		financing := data.Capacity.Financing
		var request gameapi.QuoteActionRequest
		if json.Unmarshal(data.Action.Body, &request) != nil || len(financing.Loans) >= len(financing.Quotes) || request.QuoteID != financing.Quotes[len(financing.Loans)].ID || financing.Cancel {
			return errors.New("new borrowing disagrees with its admitted quote")
		}
		if _, _, err := r.capacityFinanceReady(ctx, data); err != nil {
			return err
		}
		_, err := r.capacityYardReady(ctx, data)
		return err
	}
	intent := data.LoanRepayment
	var request gameapi.RepayLoanRequest
	if intent == nil || json.Unmarshal(data.Action.Body, &request) != nil || request.LoanID != intent.Before.ID ||
		intent.AmountPence == 0 && request.AmountPence != nil || intent.AmountPence > 0 && (request.AmountPence == nil || *request.AmountPence != intent.AmountPence) {
		return errors.New("new loan repayment disagrees with its immutable loan and amount")
	}
	loan, exists := r.loanByID(request.LoanID)
	if !exists || loan.BorrowerCompanyID != r.account.CompanyID || loan.OwedPence <= 0 {
		return errors.New("new repayment requires its current owned outstanding debt")
	}
	input := r.planningInput(ShipState{})
	var protected map[string]int64
	var err error
	if mission.ShipID == "__loans" {
		protected, err = r.treasuryReserves(ctx)
	} else {
		input, err = r.committedPlanningInputExcept(ctx, ShipState{}, mission.ShipID)
		protected = input.ReserveByRegion
	}
	if err != nil {
		return err
	}
	amount := intent.AmountPence
	if amount == 0 {
		amount = loanFullPayment(loan)
	}
	if input.Balances[loan.BankingRegion]-amount < max(0, protected[loan.BankingRegion]-loanReserve(loan)) {
		return errors.New("loan repayment would spend another hard cash obligation")
	}
	return nil
}

func (r *Runner) recordAcceptedCapacityLoan(data *missionData, loan gameapi.Loan, quote gameapi.LoanQuote) {
	financing := data.Capacity.Financing
	if len(financing.Loans) == 0 {
		// The receipt time prevents reconciliation from extending the idle-debt window.
		acceptedAt := loan.CreatedAt
		if acceptedAt.IsZero() || acceptedAt.After(time.Now()) {
			acceptedAt = data.Action.CreatedAt
		}
		financing.BuildBy = acceptedAt.Add(24 * time.Hour / time.Duration(max(int64(1), r.stateCache.Clock.Calendar.TimeScale)))
	}
	financing.Loans = append(financing.Loans, loan)
	financing.Cancel = financing.Cancel || !matchesLoanQuote(loan, quote, r.account.CompanyID)
}

func (r *Runner) acceptLoanResult(data *missionData, response json.RawMessage) bool {
	var loan gameapi.Loan
	if json.Unmarshal(response, &loan) != nil || loan.ID == "" {
		return false
	}
	if data.Action.Stage == "loan-accept" {
		if data.Capacity == nil || data.Capacity.Financing == nil {
			return false
		}
		financing := data.Capacity.Financing
		if len(financing.Loans) >= len(financing.Quotes) {
			return false
		}
		quote := financing.Quotes[len(financing.Loans)]
		var request gameapi.QuoteActionRequest
		if json.Unmarshal(data.Action.Body, &request) != nil || request.QuoteID != quote.ID || !loanQuoteIdentity(loan, quote) {
			return false
		}
		r.recordAcceptedCapacityLoan(data, loan, quote)
		return true
	}
	return data.LoanRepayment != nil && loan.ID == data.LoanRepayment.Before.ID
}

func loanRepaymentObserved(loan gameapi.Loan, intent *loanRepaymentIntent) bool {
	if intent == nil || loan.ID != intent.Before.ID {
		return false
	}
	if loan.OwedPence == 0 && (loan.Status == "repaid" || loan.Status == "recovered") {
		return true
	}
	paid := loan.PrincipalRepaidPence + loan.InterestPaidPence - intent.Before.PrincipalRepaidPence - intent.Before.InterestPaidPence
	return intent.AmountPence > 0 && paid >= intent.AmountPence
}

func (r *Runner) reconcileLoanAction(ctx context.Context, mission *store.Mission, data *missionData, action *store.Action) (bool, error) {
	if !r.loanAuthority() {
		return false, nil
	}
	var found *gameapi.Loan
	if data.Action.Stage == "loan-accept" {
		if data.Capacity == nil || data.Capacity.Financing == nil {
			return false, nil
		}
		financing := data.Capacity.Financing
		if len(financing.Loans) >= len(financing.Quotes) {
			return false, nil
		}
		quote := financing.Quotes[len(financing.Loans)]
		var request gameapi.QuoteActionRequest
		if json.Unmarshal(action.Body, &request) != nil || request.QuoteID != quote.ID {
			return false, nil
		}
		for _, loan := range r.stateCache.Loans {
			if !loanQuoteIdentity(loan, quote) {
				continue
			}
			if found != nil {
				return false, r.hold(ctx, mission.ShipID, "multiple authoritative loans match one quote; borrowing remains quarantined")
			}
			copy := loan
			found = &copy
		}
	} else if intent := data.LoanRepayment; intent != nil {
		if loan, exists := r.loanByID(intent.Before.ID); exists && loanRepaymentObserved(loan, intent) {
			found = &loan
		}
	}
	if found == nil {
		return false, nil
	}
	if action.State != store.ActionComplete {
		evidence := encode(map[string]any{"kind": "authoritative_loan", "loan": found, "observed_at": r.meta["loans"].UpdatedAt})
		if err := r.db.ReconcileAction(ctx, action.ID, evidence); err != nil {
			return false, err
		}
	}
	if data.Action.Stage == "loan-accept" {
		quote := data.Capacity.Financing.Quotes[len(data.Capacity.Financing.Loans)]
		r.recordAcceptedCapacityLoan(data, *found, quote)
		if data.Capacity.Financing.Cancel {
			if err := r.recordCapacityFinancingRefusal(ctx, data); err != nil {
				return false, err
			}
		}
	}
	return r.applyReconciliation(ctx, mission, data)
}

func loanResultPhase(mission *store.Mission, data *missionData) {
	if data.Action.Stage == "loan-accept" {
		mission.Phase = "growth_finance"
		if data.Capacity.Financing.Cancel {
			mission.Phase = "growth_finance_repay"
		}
	} else if mission.ShipID == "__growth" {
		mission.Phase = "growth_finance_repay"
	} else {
		mission.Phase = "done"
	}
	data.LoanRepayment = nil
}
