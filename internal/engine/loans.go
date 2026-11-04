package engine

import (
	"context"
	"encoding/json"
	"errors"
	"maps"
	"math"
	"sort"
	"strings"
	"time"

	"github.com/DevYukine/go-tradewinds/internal/gameapi"
	"github.com/DevYukine/go-tradewinds/internal/store"
	"github.com/google/uuid"
)

const (
	goldsmithsMinimumPence  = int64(10_000)
	goldsmithsMaximumPence  = int64(750_000)
	loanServiceLeadGameDays = 3
	debtServiceHoldPrefix   = "growth deferred: free cash must keep the remaining unsecured loan payoff"
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
type loanBridgeIntent struct {
	Before         gameapi.Loan            `json:"before"`
	Quote          gameapi.GoldsmithsQuote `json:"quote"`
	ShortfallPence int64                   `json:"shortfall_pence"`
}

func validMortgageTerms(terms *gameapi.MortgageTerms, total int64) bool {
	return terms != nil && terms.Available && terms.PrincipalPence > 0 && terms.PrincipalPence <= total &&
		terms.TermGameDays == 180 && terms.Repayment == "amortised" && terms.Installments == 18 &&
		terms.InstallmentPence != nil && *terms.InstallmentPence > 0 &&
		terms.TotalRepayablePence >= terms.PrincipalPence && terms.AnnualRateBPS >= 0 && terms.LenderCompanyID != ""
}

func mortgageInterestPerGameDay(terms *gameapi.MortgageTerms) float64 {
	if terms == nil {
		return 0
	}
	return float64(terms.TotalRepayablePence-terms.PrincipalPence) / float64(terms.TermGameDays)
}

func mortgageReserve(terms *gameapi.MortgageTerms) int64 {
	if terms == nil || terms.InstallmentPence == nil {
		return 0
	}
	return growthCashTotal(*terms.InstallmentPence, *terms.InstallmentPence)
}

func sameMortgageTerms(a, b *gameapi.MortgageTerms) bool {
	return a != nil && b != nil && a.InstallmentPence != nil && b.InstallmentPence != nil &&
		a.LenderCompanyID == b.LenderCompanyID && a.PrincipalPence == b.PrincipalPence &&
		a.AnnualRateBPS == b.AnnualRateBPS && a.TermGameDays == b.TermGameDays && a.Repayment == b.Repayment &&
		a.Installments == b.Installments && *a.InstallmentPence == *b.InstallmentPence && a.TotalRepayablePence == b.TotalRepayablePence
}

func openMortgage(loan gameapi.Loan) bool {
	return loan.IsMortgage && (loan.Status == "active" || loan.Status == "defaulted") && loan.OwedPence > 0
}

func (r *Runner) mortgagedShip(shipID string) bool {
	for _, loan := range r.stateCache.Loans {
		if openMortgage(loan) && loan.CollateralShipID != nil && *loan.CollateralShipID == shipID {
			return true
		}
	}
	return false
}

func (r *Runner) mortgageAvailable() bool {
	if until := decode[time.Time](r.snapshot, "_mortgage_retry_at"); time.Now().Before(until) {
		r.wakeBy(until)
		return false
	}
	if !r.loanAuthority() || !r.meta["loans"].UpdatedAt.After(decode[time.Time](r.snapshot, "_mortgage_refused_at")) {
		return false
	}
	for _, loan := range r.stateCache.Loans {
		if openMortgage(loan) {
			return false
		}
	}
	return true
}

func (r *Runner) mortgageBuildTermsReady(data *missionData, quote YardQuote) error {
	if data.Capacity == nil || data.Capacity.Mortgage == nil {
		return nil
	}
	if !r.mortgageAvailable() {
		return capacityGate("mortgage_unavailable", true, "capacity mortgage waits until authoritative loans show no open mortgage")
	}
	if quote.TotalCostPence == nil || !validMortgageTerms(quote.Mortgage, *quote.TotalCostPence) ||
		!sameMortgageTerms(data.Capacity.Mortgage, quote.Mortgage) {
		return capacityGate("mortgage_unavailable", true, "capacity mortgage fresh terms differ from the admitted immutable financing")
	}
	return nil
}

func (r *Runner) reconcileMortgageBuild(ctx context.Context, mission *store.Mission, data *missionData, action *store.Action) (bool, error) {
	if !r.fresh("ships") || !r.loanAuthority() {
		return false, nil
	}
	for _, ship := range r.stateCache.Ships {
		if ship.Ship.Name != data.BuildName || ship.Ship.ShipClass != data.BuildClass || ship.Ship.CurrentPortID != data.BuildPort {
			continue
		}
		for _, loan := range r.stateCache.Loans {
			if !loan.IsMortgage || loan.BorrowerCompanyID != r.account.CompanyID || loan.CollateralShipID == nil ||
				*loan.CollateralShipID != ship.Ship.ID {
				continue
			}
			if action.State != store.ActionComplete {
				evidence := encode(map[string]any{"kind": "authoritative_named_mortgage_build", "ship": ship, "loan": loan,
					"fleet_observed_at": r.meta["ships"].UpdatedAt, "loans_observed_at": r.meta["loans"].UpdatedAt})
				if err := r.db.ReconcileAction(ctx, action.ID, evidence); err != nil {
					return false, err
				}
			}
			data.BuiltShipID, data.CostsKnown = ship.Ship.ID, false
			return r.applyReconciliation(ctx, mission, data)
		}
	}
	return false, nil
}

type loanRepaymentIntent struct {
	Before      gameapi.Loan `json:"before"`
	AmountPence int64        `json:"amount_pence"` // Zero means repay the full current balance.
}

type capacityLoanDecision struct {
	Borrow                bool            `json:"borrow"`
	Source                string          `json:"source"`
	TermGameDays          int32           `json:"term_game_days"`
	CollateralKind        string          `json:"collateral_kind"`
	ShipID                string          `json:"ship_id"`
	Verdict               string          `json:"verdict"`
	HullCategory          string          `json:"hull_category"`
	EvidenceBasis         string          `json:"evidence_basis"`
	Package               capacityPackage `json:"package"`
	Reason                string          `json:"reason"`
	ShortfallPence        int64           `json:"shortfall_pence"`
	LienCapacityPence     *int64          `json:"lien_capacity_pence"`
	PrincipalPence        int64           `json:"principal_pence"`
	InstallmentPence      *int64          `json:"installment_pence"`
	DebtReservePence      int64           `json:"debt_reserve_pence"`
	Coverage              *float64        `json:"half_net_installment_coverage"`
	NetAfterInterestPence float64         `json:"net_after_daily_interest_pence"`
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

type loanReserveDeferral struct {
	Region      string
	FullReserve int64
	ReserveFrom time.Time
}

// Only short-turnover operating capital may use an installment before the
// three-game-day cash window shared with emergency refinancing.
func (state AccountState) loanServiceReserve(loan gameapi.Loan, at time.Time) int64 {
	if state.CompanyID == "" || loan.BorrowerCompanyID != state.CompanyID || loan.Status != "active" ||
		loan.Repayment != "amortised" || loan.ArrearsPence != 0 || loan.NextPaymentDueAt == nil ||
		loan.NextPaymentDueAt.IsZero() || loan.NextPaymentPence == nil || *loan.NextPaymentPence <= 0 ||
		at.IsZero() || state.Clock.RealUTC.IsZero() || state.Clock.Paused || state.Clock.Calendar.TimeScale <= 0 || state.loanClockUnknown {
		return loanReserve(loan)
	}
	if at.Add(gameDuration(loanServiceLeadGameDays, state.Clock.Calendar.TimeScale)).Before(*loan.NextPaymentDueAt) {
		return 0
	}
	return max(*loan.NextPaymentPence, loan.ArrearsPence)
}

func (r *Runner) loanServiceReserve(loan gameapi.Loan) int64 {
	state := r.stateCache
	_, _, valid := r.serverBounds()
	state.loanClockUnknown = !valid
	return state.loanServiceReserve(loan, state.Clock.RealUTC.Add(serverClockStaleness))
}

func fullLoanReserves(in PlanningInput) map[string]int64 {
	required := clonePence(in.ReserveByRegion)
	for _, deferred := range in.DeferredLoanReserves {
		required[deferred.Region] = growthCashTotal(required[deferred.Region], deferred.FullReserve)
	}
	return required
}

func loanDailyInterest(loan gameapi.Loan) float64 {
	if loan.RateBasis == "annual" && loan.AnnualRateBPS != nil {
		return float64(loan.OutstandingPrincipalPence) * float64(*loan.AnnualRateBPS) / 10_000 / 360
	}
	return float64(loan.OutstandingPrincipalPence) * float64(loan.DailyRateBPS) / 10_000
}

func loanAnnualRate(loan gameapi.Loan) (int64, bool) {
	var rate int64
	switch loan.RateBasis {
	case "annual":
		if loan.AnnualRateBPS == nil {
			return 0, false
		}
		rate = int64(*loan.AnnualRateBPS)
	case "daily":
		rate = int64(loan.DailyRateBPS) * 360
	default:
		return 0, false
	}
	return rate, rate >= 0 && rate <= 1_000_000
}

func loanFullPayment(loan gameapi.Loan) int64 {
	if loan.OwedPence == 0 && loan.OutstandingPrincipalPence == 0 {
		return 0
	}
	if _, valid := loanAnnualRate(loan); !valid || loan.OwedPence < 0 || loan.OwedPence > maximumPence ||
		loan.OutstandingPrincipalPence < 0 || loan.OutstandingPrincipalPence > maximumPence {
		return math.MaxInt64
	}
	return growthCashTotal(loan.OwedPence, int64(math.Ceil(loanDailyInterest(loan))))
}

func debtServiceHoldLoan(loan gameapi.Loan, companyID string) bool {
	return loan.BorrowerCompanyID == companyID && loan.Status == "active" &&
		loan.CollateralKind == "unsecured" && !loan.IsMortgage && loan.OwedPence > 0
}

func (r *Runner) debtServiceHoldPence() int64 {
	var hold int64
	for _, loan := range r.stateCache.Loans {
		if debtServiceHoldLoan(loan, r.account.CompanyID) {
			// Match the captured input's phase even if its metadata becomes stale
			// while a controller is running; never release a zero-reserve payoff.
			reserve := r.stateCache.loanServiceReserve(loan, r.stateCache.Clock.RealUTC.Add(serverClockStaleness))
			hold = growthCashTotal(hold, max(0, loanFullPayment(loan)-reserve))
		}
	}
	return hold
}

func (r *Runner) recordDebtServiceHold(ctx context.Context, free int64) error {
	return r.decision(ctx, "", "capacity_financing:debt_service_hold", debtServiceHoldPrefix, map[string]any{
		"verdict":          "debt_service_hold",
		"hold_crowns":      float64(r.debtServiceHoldPence()) / 100,
		"free_cash_crowns": float64(free) / 100,
	})
}

func evaluateCapacityLoan(decision GrowthDecision, shortfall, capacity, principal, installment int64, annualRate int32) capacityLoanDecision {
	result := capacityLoanDecision{
		ShortfallPence: shortfall, LienCapacityPence: &capacity, PrincipalPence: principal, Verdict: "debt_capacity",
		InstallmentPence: &installment, DebtReservePence: growthCashTotal(installment, installment),
	}
	if !decision.EconomicallyViable || decision.Accepted || shortfall <= 0 {
		result.Reason = "borrow only for an economically admitted depth-backed capacity candidate blocked solely by a positive total cash shortfall"
		return result
	}
	if principal < goldsmithsMinimumPence || principal > capacity || installment <= 0 ||
		result.DebtReservePence == math.MaxInt64 || principal < growthCashTotal(shortfall, result.DebtReservePence) {
		result.Reason = "available liens cannot cover the full shortfall and two installments"
		return result
	}
	coverage := decision.IncrementalNetPencePerGameDay * 5 / float64(installment)
	result.Coverage = &coverage
	result.NetAfterInterestPence = decision.IncrementalNetPencePerGameDay - float64(principal)*float64(annualRate)/10_000/360
	if !capacityPackageCoverage(decision, installment) || result.NetAfterInterestPence <= 0 {
		result.Verdict = "dscr"
		result.Reason = "half the repeatable marginal net must cover ten-day installments by at least 1.25x after interest"
		return result
	}
	if float64(growthCapital(decision))/result.NetAfterInterestPence > capacityPaybackGameDays {
		result.Verdict = "payback"
		result.Reason = "capacity capital exceeds the maximum game-day payback after loan interest"
		return result
	}
	result.Borrow = true
	result.Verdict = "financed"
	result.Reason = "loan proceeds fund only capacity capital, the candidate lot and two installments with at least 1.25x half-net coverage"
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
	capital := growthCapital(decision)
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
	if decision.Source == "" {
		decision.Source = "goldsmiths"
	}
	if decision.TermGameDays == 0 {
		decision.TermGameDays = 360
	}
	if decision.CollateralKind == "" {
		decision.CollateralKind = "ship_lien"
	}
	decision.HullCategory = hull.HullCategory
	if decision.Verdict == "" {
		decision.Verdict = "debt_capacity"
	}
	return r.decision(ctx, "", "capacity_financing:"+port.ID+"|"+hull.ID, decision.Reason, decision)
}

func (r *Runner) recordCapacityFinancingRefusal(ctx context.Context, data *missionData) error {
	return r.recordGrowthBackoff(ctx, "_capacity_financing_backoffs", data)
}

func (r *Runner) considerCapacityFinancing(ctx context.Context, port Port, hull Hull, decision GrowthDecision, required map[string]int64, evidence *growthEvidence) (bool, error) {
	if r.phaseOneYard(port) {
		return false, r.holdExpected(ctx, "", r.overseasGrowthHoldReason(""))
	}
	if evidence.asOf.IsZero() {
		evidence.asOf = time.Now().UTC()
	}
	result := capacityLoanDecision{
		ShortfallPence: decision.CashShortfallPence, EvidenceBasis: decision.EvidenceBasis,
		Source:  "player_request",
		Package: capacityPackageFor(decision, "player_request", 0, 0),
	}
	if r.playerFallback(port.ID, hull.ID) {
		result.Source = "goldsmiths"
		result.Package.Source = "goldsmiths"
	}
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
	free := growthFreeCash(evidence.balances, evidence.required)
	hold := r.debtServiceHoldPence()
	evidence.available = free - hold
	if !capacityDebtCashReady(decision, evidence.available) {
		if hold > 0 && (free < hold || capacityDebtCashReady(decision, free)) {
			if err := r.recordDebtServiceHold(ctx, free); err != nil {
				return false, err
			}
			result.Verdict = "debt_service_hold"
			return refuse(debtServiceHoldPrefix)
		}
		result.Verdict = "own_cash_buffer"
		return refuse("own cash must cover the incumbent buffer before borrowing for capital and the candidate lot")
	}
	if until := decode[map[string]time.Time](r.snapshot, "_capacity_financing_backoffs")[port.ID+"|"+hull.ID]; time.Now().Before(until) {
		r.wakeBy(until)
		return false, nil
	}
	if !r.loanAuthority() || !r.opts.Execute {
		return refuse("borrowing requires execute mode and fresh complete borrower-loan authority")
	}
	var gate string
	var err error
	decision, gate, err = r.capacityDebtDecision(ctx, hull, decision, evidence.asOf, nil)
	if err != nil {
		return false, err
	}
	result.EvidenceBasis = decision.EvidenceBasis
	if gate != "" {
		result.Verdict = gate
		return refuse("debt requires fresh realized settlement net: " + gate)
	}
	capital := growthCapital(decision)
	if decision.DepthNetPencePerGameDay <= 0 {
		result.Verdict = "no_depth"
		return refuse("recourse debt requires repeatable depth-backed non-charter earnings")
	}
	if float64(capital)/decision.DepthNetPencePerGameDay > capacityPaybackGameDays {
		result.Verdict = "payback"
		return refuse("debt-funded capacity exceeds the maximum payback")
	}
	decision.IncrementalNetPencePerGameDay = decision.DepthNetPencePerGameDay
	decision.PaybackGameDays = float64(capital) / decision.DepthNetPencePerGameDay
	if decision.DepthHazardDestination != "" {
		decision.HazardSource = decision.DepthHazardSource
		decision.HazardDestination = decision.DepthHazardDestination
	}
	if strings.HasPrefix(decision.HazardSource, "zone estimate") {
		if capped := capacityEstimatedExposure(decision, evidence.hazard.netWorth); !capped.EconomicallyViable {
			result.Verdict = "zone_exposure"
			return refuse(capped.Reason)
		}
	}
	freeShips := r.freeLoanShips()
	ships := make([]string, 0, len(freeShips))
	for _, ship := range freeShips {
		ships = append(ships, ship.Ship.ID)
	}
	// A request precedes all instant Goldsmiths quote creation.
	if until := decode[map[string]time.Time](r.snapshot, "_player_loan_backoffs")[port.ID+"|"+hull.ID]; !r.playerFallback(port.ID, hull.ID) && time.Now().Before(until) {
		r.wakeBy(until)
		return false, nil
	}
	if !r.loanRequestAuthority() || !r.fresh("ship_listings") || r.openOwnLoanRequest() {
		result.Source, result.Verdict = "player_request", "request_open"
		return refuse("fresh own requests must show no open request before borrowing")
	}
	if _, err := r.growthNetWorth(ctx, evidence); err != nil {
		return false, err
	}
	financingShortfall := growthCashTotal(decision.CashShortfallPence, r.capacityLoanHeadroom(decision))
	playerPrincipal, playerInstallment := estimatedPlayerPrincipal(financingShortfall)
	if !capacityDebtExposure(decision, playerPrincipal, evidence.hazard.netWorth, r.stateCache.Loans, nil) {
		result.Verdict = "debt_capacity"
		return refuse("gross package debt exceeds 35% of fresh net worth, or net worth is stale")
	}
	if !r.playerFallback(port.ID, hull.ID) {
		result = evaluateCapacityLoan(decision, decision.CashShortfallPence, math.MaxInt64, playerPrincipal, playerInstallment, 3_000)
		result.Source = "player_request"
		result.EvidenceBasis = decision.EvidenceBasis
		result.Package = capacityPackageFor(decision, "player_request", playerPrincipal, playerInstallment)
		if !result.Borrow {
			return refuse(result.Reason)
		}
		return r.stagePlayerLoan(ctx, port, hull, decision, playerPrincipal, playerInstallment, evidence)
	}
	capacity := int64(len(ships)) * goldsmithsMaximumPence
	principal, estimate := estimatedCapacityPrincipal(financingShortfall, int64(len(ships)))
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
		needed := growthCashTotal(financingShortfall, installment, installment)
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
	result.Source, result.TermGameDays, result.CollateralKind = "goldsmiths", 360, "ship_lien"
	result.Package, result.EvidenceBasis = capacityPackageFor(decision, "goldsmiths", principal, installment), decision.EvidenceBasis
	if len(quotes) > 0 && quotes[0].CollateralShipID != nil {
		result.ShipID = *quotes[0].CollateralShipID
	}
	if !capacityDebtExposure(decision, principal, evidence.hazard.netWorth, r.stateCache.Loans, nil) {
		result.Verdict = "debt_capacity"
		return refuse("exact gross package debt exceeds 35% of fresh net worth")
	}
	_, protected, donors, funded := capacityLoanFunding(decision, evidence, port.BankingRegion, principal, result.DebtReservePence, r.stateCache.Rates)
	if !result.Borrow || !funded {
		result.Borrow = false
		return refuse("exact financing cannot preserve the admitted capital, cargo buffer, debt reserve and coverage")
	}
	if err := r.recordCapacityLoan(ctx, port, hull, result); err != nil {
		return false, err
	}
	r.clearPlayerFallback(port.ID, hull.ID)
	if err := r.saveSnapshot(ctx); err != nil {
		return false, err
	}
	return r.startCapacityGrowth(ctx, port, hull, decision, &capacityFinancing{Quotes: quotes, Decision: decision, ProtectedByRegion: protected, DonorByRegion: donors})
}

func matchesLoanQuote(loan gameapi.Loan, quote gameapi.LoanQuote, companyID string) bool {
	return !loan.IsMortgage && loan.BorrowerCompanyID == companyID && loan.LenderCompanyID == quote.LenderCompanyID && loan.BankingRegion == quote.BankingRegion &&
		loan.PrincipalPence == quote.PrincipalPence && loan.RateBasis == "annual" && loan.AnnualRateBPS != nil && *loan.AnnualRateBPS == quote.AnnualRateBPS &&
		loan.TermGameDays == quote.TermGameDays && loan.Repayment == quote.Repayment && loan.Recourse &&
		loan.InstallmentPence != nil && quote.InstallmentPence != nil && *loan.InstallmentPence == *quote.InstallmentPence &&
		loan.TotalRepayablePence != nil && *loan.TotalRepayablePence == quote.TotalRepayablePence
}

func loanQuoteIdentity(loan gameapi.Loan, quote gameapi.LoanQuote) bool {
	if loan.IsMortgage || loan.ID == "" || loan.QuoteID == nil || *loan.QuoteID != quote.ID {
		return false
	}
	if quote.RequestID != nil {
		return loan.RequestID != nil && *loan.RequestID == *quote.RequestID &&
			sameOptionalString(loan.CollateralShipID, quote.CollateralShipID)
	}
	return loan.CollateralShipID != nil && quote.CollateralShipID != nil && *loan.CollateralShipID == *quote.CollateralShipID
}

func sameOptionalString(a, b *string) bool {
	return a == nil && b == nil || a != nil && b != nil && *a == *b
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
	if loan.IsMortgage && mission.ShipID != "__loans" {
		return false, errors.New("mortgage repayment requires its debt-service intent, never capacity financing")
	}
	if loan.IsMortgage && mission.Phase == "loan_prepay" {
		return false, errors.New("surplus prepayment never repays a mortgage")
	}
	data.LoanRepayment = &loanRepaymentIntent{Before: loan, AmountPence: amount}
	var requested *int64
	if amount > 0 {
		requested = &amount
	}
	return true, r.stage(ctx, mission, data, ShipState{}, "loan-repay", "/v1/loan/repay", gameapi.RepayLoanRequest{LoanID: loan.ID, AmountPence: requested})
}

// Installment collections draw from all vaults. Protect non-loan bills and
// earlier collections once, rather than counting the same cash for every loan.
func (r *Runner) loanBridgeShortfall(ctx context.Context, loan gameapi.Loan) (int64, error) {
	if !r.opts.Execute || loan.BorrowerCompanyID != r.account.CompanyID || loan.Status != "active" ||
		loan.Repayment != "amortised" || loan.OwedPence <= 0 ||
		r.stateCache.Clock.Paused || r.stateCache.Clock.Calendar.TimeScale <= 0 {
		return 0, nil
	}
	if _, _, ok := r.serverBounds(); !ok {
		return 0, nil
	}
	at := r.stateCache.Clock.RealUTC.Add(serverClockStaleness)
	if loan.ArrearsPence <= 0 && (loan.NextPaymentDueAt == nil || loan.NextPaymentDueAt.IsZero() ||
		loan.NextPaymentPence == nil || *loan.NextPaymentPence <= 0 ||
		at.Add(gameDuration(loanServiceLeadGameDays, r.stateCache.Clock.Calendar.TimeScale)).Before(*loan.NextPaymentDueAt)) {
		return 0, nil
	}
	collectionBy := at
	if loan.NextPaymentDueAt != nil {
		collectionBy = *loan.NextPaymentDueAt
	}
	excluded := make([]string, 0, len(r.stateCache.Loans))
	for _, other := range r.stateCache.Loans {
		excluded = append(excluded, other.ID)
	}
	snapshot := maps.Clone(r.snapshot)
	delete(snapshot, "_loan_refinance")
	delete(snapshot, "_america_flagship")
	bills, err := r.treasuryReservesFor(ctx, snapshot, excluded...)
	if err != nil {
		return 0, err
	}
	var cash int64
	for _, vault := range r.stateCache.Vaults {
		if vault.AccountType == "vault" {
			cash += vault.BalancePence
		}
	}
	for _, bill := range bills {
		cash -= bill
	}
	for _, other := range r.stateCache.Loans {
		if other.ID == loan.ID || other.BorrowerCompanyID != r.account.CompanyID ||
			other.Status != "active" && other.Status != "defaulted" {
			continue
		}
		var collection int64
		if other.ArrearsPence > 0 {
			collection = other.ArrearsPence
		}
		if other.NextPaymentDueAt != nil && !other.NextPaymentDueAt.After(collectionBy) && other.NextPaymentPence != nil {
			collection = max(collection, *other.NextPaymentPence)
		}
		cash -= collection
	}
	payment := loanRefinancePayment(loan)
	return max(0, payment-cash), nil
}

func (r *Runner) loanBridgeShips(ctx context.Context) ([]ShipState, error) {
	mission, data, err := r.loadMission(ctx, "__growth")
	if err != nil {
		return nil, err
	}
	var pending []gameapi.LoanQuote
	if mission != nil && data.Capacity != nil && data.Capacity.Financing != nil {
		financing := data.Capacity.Financing
		if !financing.Cancel && len(financing.Loans) < len(financing.Quotes) {
			pending = financing.Quotes[len(financing.Loans):]
		}
	}
	ships := r.freeLoanShips()
	free := ships[:0]
	for _, ship := range ships {
		pledged := false
		for _, loan := range r.stateCache.Loans {
			if loan.CollateralShipID != nil && *loan.CollateralShipID == ship.Ship.ID &&
				(loan.Status == "active" || loan.Status == "defaulted") {
				pledged = true
				break
			}
		}
		for _, quote := range pending {
			if quote.CollateralShipID != nil && *quote.CollateralShipID == ship.Ship.ID {
				pledged = true
				break
			}
		}
		if !pledged {
			free = append(free, ship)
		}
	}
	return free, nil
}

func (r *Runner) recordLoanBridgeDecision(ctx context.Context, intent *loanBridgeIntent, reason string) error {
	return r.decision(ctx, "__loans", "loan_bridge", reason, map[string]any{
		"loan_id": intent.Before.ID, "due_at": intent.Before.NextPaymentDueAt,
		"shortfall_pence": intent.ShortfallPence, "principal_pence": intent.Quote.Quote.PrincipalPence,
		"banking_region": intent.Before.BankingRegion, "ship_id": intent.Quote.Quote.CollateralShipID,
	})
}

func (r *Runner) markLoanBridgeDue(ctx context.Context, intent *loanBridgeIntent, reason string) error {
	if err := r.recordLoanBridgeDecision(ctx, intent, reason); err != nil {
		return err
	}
	dates := decode[map[string]bool](r.snapshot, "_loan_bridge_due")
	if dates == nil {
		dates = make(map[string]bool)
	}
	for key := range dates {
		id, _, _ := strings.Cut(key, "|")
		if loan, exists := r.loanByID(id); !exists || loan.Status != "active" && loan.Status != "defaulted" {
			delete(dates, key)
		}
	}
	dates[loanRefinanceDueKey(intent.Before)] = true
	r.snapshot["_loan_bridge_due"] = encode(dates)
	return r.saveSnapshot(ctx)
}

func (r *Runner) clearLoanBridgeDue(ctx context.Context, intent *loanBridgeIntent) error {
	dates := decode[map[string]bool](r.snapshot, "_loan_bridge_due")
	delete(dates, loanRefinanceDueKey(intent.Before))
	r.snapshot["_loan_bridge_due"] = encode(dates)
	return r.saveSnapshot(ctx)
}

func (r *Runner) serviceInstallmentBridge(ctx context.Context) (bool, error) {
	if !r.opts.Execute || !r.loanRequestAuthority() {
		return false, nil
	}
	for _, dataset := range []string{"clock", "ships", "ship_listings", "catalog", "profile", "settings", "factors", "warehouses", "contracts"} {
		if !r.fresh(dataset) {
			return false, nil
		}
	}
	dates := decode[map[string]bool](r.snapshot, "_loan_bridge_due")
	for _, loan := range r.stateCache.Loans {
		if dates[loanRefinanceDueKey(loan)] {
			continue
		}
		shortfall, err := r.loanBridgeShortfall(ctx, loan)
		if err != nil {
			return false, err
		}
		if shortfall == 0 {
			continue
		}
		intent := &loanBridgeIntent{Before: loan, ShortfallPence: shortfall}
		principal := max(int64(100_000), min(goldsmithsMaximumPence, shortfall))
		principal = min(goldsmithsMaximumPence, (principal+9_999)/10_000*10_000)
		ships, err := r.loanBridgeShips(ctx)
		if err != nil {
			return false, err
		}
		if len(ships) == 0 {
			return false, r.recordLoanBridgeDecision(ctx, intent, "Goldsmiths bridge unavailable: no free collateral; no quote requested")
		}
		// Fresh turnkey class prices rank the requests cheaply; the returned
		// appraisal below decides which exact eligible lien is least valuable.
		values := make(map[string]int64, len(ships))
		for _, ship := range ships {
			value := int64(math.MaxInt64)
			for key, quote := range r.stateCache.Quotes {
				if quote.ShipClass == ship.Ship.ShipClass && r.fresh("yard_quotes/"+key) &&
					quote.TotalCostPence != nil && *quote.TotalCostPence > 0 {
					value = min(value, growthCashTotal(*quote.TotalCostPence, ship.Ship.CannonsCount*15_000))
				}
			}
			values[ship.Ship.ID] = value
		}
		sort.SliceStable(ships, func(i, j int) bool { return values[ships[i].Ship.ID] < values[ships[j].Ship.ID] })
		var best gameapi.GoldsmithsQuote
		for _, ship := range ships {
			quote, err := r.api.GetGoldsmithsQuote(ctx, gameapi.GoldsmithsQuoteRequest{
				PrincipalPence: principal, TermGameDays: 360, Repayment: "amortised",
				ShipID: ship.Ship.ID, BankingRegion: loan.BankingRegion,
			})
			if err != nil {
				var apiErr *gameapi.APIError
				if errors.As(err, &apiErr) && apiErr.Status >= 400 && apiErr.Status < 500 {
					return false, r.markLoanBridgeDue(ctx, intent, "Goldsmiths bridge quote refused: "+apiErr.Code+" "+apiErr.Detail)
				}
				if ctx.Err() != nil {
					return false, ctx.Err()
				}
				return false, r.recordLoanBridgeDecision(ctx, intent, "Goldsmiths bridge quote temporarily unavailable: "+err.Error())
			}
			if quote.MaxPrincipalPence < principal {
				continue
			}
			if !r.validGoldsmithsQuote(quote, ship.Ship.ID, loan.BankingRegion, principal) {
				return false, r.markLoanBridgeDue(ctx, intent, "Goldsmiths bridge quote refused: unavailable, expired or mismatched exact terms")
			}
			if best.Quote.ID == "" || quote.RebuildValuePence < best.RebuildValuePence {
				best = quote
			}
		}
		if best.Quote.ID == "" {
			return false, r.markLoanBridgeDue(ctx, intent, "Goldsmiths bridge unavailable: no free lien can cover the bounded installment shortfall")
		}
		intent.Quote = best
		if err := r.markLoanBridgeDue(ctx, intent, "emergency Goldsmiths bridge admitted for installment cash shortfall"); err != nil {
			return false, err
		}
		mission := &store.Mission{AccountID: r.account.ID, ShipID: "__loans", Phase: "loan_bridge", Generation: uuid.NewString()}
		data := missionData{Plan: Plan{Kind: "treasury"}, CostsKnown: true, LoanBridge: intent}
		err = r.stage(ctx, mission, &data, ShipState{}, "loan-accept", "/v1/loan/accept-quote", gameapi.QuoteActionRequest{QuoteID: best.Quote.ID})
		if err != nil || data.Action == nil {
			return false, errors.Join(err, r.clearLoanBridgeDue(ctx, intent))
		}
		return true, nil
	}
	return false, nil
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
		if mission.Phase == "loan_prepay_done" {
			// Keep the paid loan's durable intent until fresh debt and cash
			// captures prevent another payment from the pre-mutation snapshot.
			tasks := []fetchTask{
				{key: "loans", path: "/v1/loan/list?role=borrower", kind: fetchPages},
				{key: "loan_requests", path: "/v1/loan/requests?mine=true&limit=50", kind: fetchJSON},
				{key: "vaults", path: "/v1/treasury/vaults", kind: fetchJSON},
			}
			if err := r.captureWave(ctx, tasks, nil); err != nil {
				return false, err
			}
			r.stateCache.Loans = decode[[]gameapi.Loan](r.snapshot, "loans")
			r.stateCache.LoanRequests = decode[[]gameapi.LoanRequestView](r.snapshot, "loan_requests")
			r.stateCache.Vaults = decode[struct {
				Vaults []Vault `json:"vaults"`
			}](r.snapshot, "vaults").Vaults
			if !r.loanAuthority() || !r.fresh("vaults") || data.LoanRepayment == nil {
				return false, r.hold(ctx, "", "completed surplus repayment awaits fresh debt and cash authority")
			}
			loan, exists := r.loanByID(data.LoanRepayment.Before.ID)
			if !exists || !loanRepaymentObserved(loan, data.LoanRepayment) {
				return false, r.hold(ctx, "", "completed surplus repayment awaits authoritative repayment evidence")
			}
		}
		return true, r.db.DeleteMission(ctx, r.account.ID, mission.ShipID)
	}
	if changed, err := r.serviceInstallmentBridge(ctx); changed || err != nil {
		return changed, err
	}
	for _, loan := range r.stateCache.Loans {
		if !loan.IsMortgage || loan.Status != "active" || loan.OwedPence <= 0 || loan.NextPaymentDueAt == nil {
			continue
		}
		required, err := r.treasuryReserves(ctx)
		if err != nil {
			return false, err
		}
		input := r.planningInput(ShipState{})
		if input.Balances[loan.BankingRegion] < required[loan.BankingRegion] {
			if changed, err := r.fundTreasuryMission(ctx, input.Balances, required, "__loans", &loanRepaymentIntent{Before: loan}, nil, false); changed || err != nil {
				return changed, err
			}
		}
	}
	for _, loan := range r.stateCache.Loans {
		if loan.Status != "active" || loan.ArrearsPence <= 0 {
			continue
		}
		input := r.planningInput(ShipState{})
		required, err := r.loanServiceReserves(ctx, loan.ID)
		if err != nil {
			return false, err
		}
		required[loan.BankingRegion] = growthCashTotal(required[loan.BankingRegion], loan.ArrearsPence)
		intent := &loanRepaymentIntent{Before: loan, AmountPence: loan.ArrearsPence}
		if changed, err := r.fundTreasuryMission(ctx, input.Balances, required, "__loans", intent, nil, false); changed || err != nil {
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

// Growth calls this only after no capacity candidate or savings hold wins.
// Scheduled debt service stays ahead of growth in serviceLoans.
func (r *Runner) prepayLoans(ctx context.Context) (bool, error) {
	if !r.opts.Execute {
		return false, nil
	}
	if r.loanRefinanceHoldsPrepay() {
		return false, nil
	}
	if r.expansionAwaitingEvaluation() {
		return false, nil
	}
	pending, err := r.db.PendingActions(ctx, r.account.ID)
	if err != nil || tradingActionsPending(pending) {
		return false, err
	}
	if mission, err := r.db.GetMission(ctx, r.account.ID, "__loans"); err != nil || mission != nil {
		return false, err
	}
	type ratedLoan struct {
		loan *gameapi.Loan
		rate int64
	}
	candidates := make([]ratedLoan, 0, len(r.stateCache.Loans))
	for i := range r.stateCache.Loans {
		loan := &r.stateCache.Loans[i]
		rate, valid := loanAnnualRate(*loan)
		if !valid || loan.IsMortgage || loan.Status != "active" || loan.BorrowerCompanyID != r.account.CompanyID ||
			loan.OwedPence <= 0 || loan.OwedPence > maximumPence {
			continue
		}
		candidates = append(candidates, ratedLoan{loan: loan, rate: rate})
	}
	sort.SliceStable(candidates, func(i, j int) bool {
		return candidates[i].rate > candidates[j].rate
	})
	for _, candidate := range candidates {
		loan := *candidate.loan
		input, ready, err := r.loanPrepaymentInput(ctx, loan)
		if err != nil {
			return false, err
		}
		if !ready {
			continue
		}
		var amount int64
		if input.Balances[loan.BankingRegion]-input.ReserveByRegion[loan.BankingRegion] < loanFullPayment(loan) {
			amount = min(loanPrepaymentSurplus(input, loan), loan.OwedPence, maximumPence) / 100 * 100
			if amount < 100_000 {
				continue
			}
		}
		mission := &store.Mission{AccountID: r.account.ID, ShipID: "__loans", Phase: "loan_prepay", Generation: uuid.NewString()}
		data := missionData{Plan: Plan{Kind: "treasury"}, CostsKnown: true}
		if _, err := r.stageLoanRepayment(ctx, mission, &data, loan, amount); err != nil {
			return false, err
		}
		return r.dispatch(ctx, mission, &data)
	}
	return false, nil
}

func loanPrepaymentSurplus(input PlanningInput, loan gameapi.Loan) int64 {
	free := max(0, input.Balances[loan.BankingRegion]-input.ReserveByRegion[loan.BankingRegion])
	return max(0, free-loanReserve(loan))
}

func (r *Runner) loanPrepaymentInput(ctx context.Context, loan gameapi.Loan) (PlanningInput, bool, error) {
	if !r.loanAuthority() {
		return PlanningInput{}, false, nil
	}
	for _, dataset := range []string{"ships", "vaults", "catalog", "clock", "rates", "profile", "factors", "warehouses", "contracts", "orders", "fills"} {
		if !r.fresh(dataset) {
			return PlanningInput{}, false, nil
		}
	}
	if r.stateCache.Clock.Paused {
		return PlanningInput{}, false, nil
	}
	if mission, err := r.db.GetMission(ctx, r.account.ID, "__growth"); err != nil || mission != nil {
		return PlanningInput{}, false, err
	}
	target, err := r.liveCapacitySavingsTarget(ctx)
	if err != nil || target.PortID != "" {
		return PlanningInput{}, false, err
	}
	input, err := r.committedPlanningInputExceptLoan(ctx, ShipState{}, "__loans", loan.ID)
	if err != nil {
		return PlanningInput{}, false, err
	}
	input.ReserveByRegion = fullLoanReserves(input)
	input.DeferredLoanReserves = nil
	region := loan.BankingRegion
	for bankRegion, required := range input.ReserveByRegion {
		if input.Balances[bankRegion] < required {
			return input, false, nil
		}
	}
	lot, err := r.capacityLotCash(ctx, time.Now().UTC())
	if err != nil {
		return PlanningInput{}, false, err
	}
	fleetSize := int64(0)
	for _, ship := range r.stateCache.Ships {
		if ship.Ship.Status != "sunk" {
			fleetSize++
		}
	}
	if lot < 0 || lot > 0 && fleetSize >= math.MaxInt64/lot {
		return input, false, nil
	}
	input.ReserveByRegion[region] = growthCashTotal(input.ReserveByRegion[region], (fleetSize+1)*lot)
	return input, input.ReserveByRegion[region] != math.MaxInt64, nil
}

func (r *Runner) capacityFinanceReady(ctx context.Context, data *missionData) (GrowthDecision, map[string]int64, error) {
	financing := data.Capacity.Financing
	if !financing.BuildBy.IsZero() && !time.Now().Before(financing.BuildBy) {
		financing.Cancel = true
		return financing.Decision, nil, capacityGate("build_deadline", false, "capacity financing reached its one-game-day construction deadline")
	}
	_, upper, clockOK := r.serverBounds()
	if clockOK {
		for index, quote := range financing.Quotes {
			if index >= len(financing.Loans) && !upper.Before(quote.ExpiresAt) {
				financing.Cancel = true
				return financing.Decision, nil, capacityGate("quote_expiry", false, "unaccepted loan quote expired")
			}
		}
	}
	if err := r.capacityInputsReady(); err != nil {
		return financing.Decision, nil, err
	}
	if !r.loanAuthority() {
		return financing.Decision, nil, capacityGate("authority", true, "capacity financing waits for fresh loan authority")
	}
	if port, _ := findPort(r.stateCache.Catalog, data.Capacity.BuildPort); r.phaseOneYard(port) && len(financing.Loans) == 0 {
		return financing.Decision, nil, capacityGate("overseas_first", true, r.overseasGrowthHoldReason(""))
	}
	hull, hullExists := findHull(r.stateCache.Catalog, data.Capacity.BuildClass)
	if !hullExists || !capacityHull(hull) {
		return financing.Decision, nil, capacityGate("hull", true, "capacity financing waits for its admitted trading hull")
	}
	if !clockOK {
		return financing.Decision, nil, capacityGate("authority", true, "capacity financing waits for current server time")
	}
	pending, err := r.db.PendingActions(ctx, r.account.ID)
	if err != nil {
		return financing.Decision, nil, err
	}
	for _, action := range pending {
		if optionalControlPath(action.Path) {
			continue
		}
		if data.Action == nil || action.ID != data.Action.ID {
			return financing.Decision, nil, capacityGate("pending_action", true, "capacity financing waits for another unresolved action")
		}
	}
	for _, id := range []string{"__treasury", "__loans", "__underwriting", "__marque"} {
		mission, current, err := r.loadMission(ctx, id)
		if err != nil {
			return financing.Decision, nil, err
		}
		if mission != nil && current.Action != nil {
			return financing.Decision, nil, capacityGate("pending_action", true, "capacity financing waits for an unresolved control intent")
		}
	}
	for _, accepted := range financing.Loans {
		if accepted.IsMortgage {
			return financing.Decision, nil, errors.New("mortgage is not capacity financing")
		}
		loan, exists := r.loanByID(accepted.ID)
		if !exists || loan.IsMortgage {
			return financing.Decision, nil, errors.New("capacity financing awaits its named authoritative loan")
		}
	}
	excludedQuotes := make(map[string]bool, len(financing.Quotes))
	for _, quote := range financing.Quotes {
		excludedQuotes[quote.ID] = true
	}
	coverageDecision, gate, err := r.capacityDebtDecision(ctx, hull, financing.Decision, time.Now().UTC(), excludedQuotes)
	if err != nil {
		return financing.Decision, nil, err
	}
	if gate != "" {
		financing.Cancel = gate != "measured_net"
		return financing.Decision, nil, capacityGate(gate, gate == "measured_net", "capacity financing fails "+gate)
	}
	coverageDecision.IncrementalNetPencePerGameDay = min(coverageDecision.IncrementalNetPencePerGameDay, financing.Decision.IncrementalNetPencePerGameDay)
	var installment int64
	netAfterInterest := coverageDecision.IncrementalNetPencePerGameDay
	for _, quote := range financing.Quotes {
		if quote.InstallmentPence == nil {
			financing.Cancel = true
			return financing.Decision, nil, capacityGate("quote_terms", false, "financing lacks exact quoted payment terms")
		}
		installment = growthCashTotal(installment, *quote.InstallmentPence)
		netAfterInterest -= float64(quote.PrincipalPence) * float64(quote.AnnualRateBPS) / 10_000 / 360
	}
	var principal int64
	for _, quote := range financing.Quotes {
		principal = growthCashTotal(principal, quote.PrincipalPence)
	}
	evidence := growthEvidence{asOf: time.Now().UTC()}
	if _, err := r.growthNetWorth(ctx, &evidence); err != nil {
		return financing.Decision, nil, err
	}
	if evidence.hazard.netWorth <= 0 {
		return financing.Decision, nil, capacityGate("debt_capacity", true, "capacity financing waits for fresh positive net worth")
	}
	if !capacityDebtExposure(financing.Decision, principal, evidence.hazard.netWorth, r.stateCache.Loans, excludedQuotes) {
		financing.Cancel = true
		return financing.Decision, nil, capacityGate("debt_capacity", false, "capacity financing debt_capacity: gross outstanding debt exceeds 35% of fresh net worth")
	}
	if !capacityPackageCoverage(coverageDecision, installment) || netAfterInterest <= 0 {
		financing.Cancel = true
		return financing.Decision, nil, capacityGate("dscr", false, "capacity financing dscr: realized depth earnings no longer cover the quoted debt")
	}
	if float64(growthCapital(financing.Decision))/netAfterInterest > capacityPaybackGameDays {
		financing.Cancel = true
		return financing.Decision, nil, capacityGate("payback", false, "capacity financing payback: realized after-interest net exceeds maximum payback")
	}
	input, err := r.committedPlanningInputExcept(ctx, ShipState{}, "__growth")
	if err != nil {
		return financing.Decision, nil, err
	}
	required := fullLoanReserves(input)
	port, exists := findPort(r.stateCache.Catalog, data.BuildPort)
	if !exists {
		return financing.Decision, nil, errors.New("capacity financing lacks its yard banking region")
	}
	capital := growthCapital(financing.Decision)
	var protectedDebt int64
	for _, accepted := range financing.Loans {
		loan, _ := r.loanByID(accepted.ID)
		reserve := r.loanServiceReserve(loan)
		if reserve == 0 {
			reserve = loanReserve(loan)
		}
		protectedDebt = growthCashTotal(protectedDebt, reserve)
	}
	required[port.BankingRegion] = growthCashTotal(required[port.BankingRegion], capital, max(0, growthCashTotal(installment, installment)-protectedDebt))
	if len(financing.Loans) == len(financing.Quotes) {
		projected, _, _, funded := measuredFunding(input.Balances, required, input.Rates)
		var available int64
		for region, balance := range projected {
			available = growthCashTotal(available, max(0, balance-required[region]))
		}
		if !funded || available+2*int64(len(financing.Quotes)) < financing.Decision.WorkingCapitalPence {
			return financing.Decision, nil, capacityGate("own_cash_buffer", true, "financed capacity waits for its fleet-wide working buffer and protected capital")
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
		if accepted.IsMortgage {
			continue
		}
		loan, exists := r.loanByID(accepted.ID)
		if !exists {
			return false, r.hold(ctx, "", "unused capacity debt awaits its named authoritative loan")
		}
		if loan.IsMortgage {
			continue
		}
		if loan.OwedPence <= 0 {
			continue
		}
		input, err := r.committedPlanningInputExceptLoan(ctx, ShipState{}, "__growth", loan.ID)
		if err != nil {
			return false, err
		}
		required := fullLoanReserves(input)
		required[loan.BankingRegion] = growthCashTotal(required[loan.BankingRegion], loanFullPayment(loan))
		if input.Balances[loan.BankingRegion] < required[loan.BankingRegion] {
			if changed, err := r.fundTreasuryMission(ctx, input.Balances, required, "__treasury", &loanRepaymentIntent{Before: loan}, mission, false); changed || err != nil {
				return changed, err
			}
			return false, r.holdExpected(ctx, mission.ShipID, "unused capacity principal and one day of interest remain protected pending repayment funding")
		}
		return r.stageLoanRepayment(ctx, mission, data, loan, 0)
	}
	r.clearPlayerFallback(data.BuildPort, data.BuildClass)
	if err := r.saveSnapshot(ctx); err != nil {
		return false, err
	}
	return true, r.db.DeleteMission(ctx, r.account.ID, mission.ShipID)
}

func (r *Runner) driveCapacityFinancing(ctx context.Context, mission *store.Mission, data *missionData) (bool, error) {
	if handled, changed, err := r.retirePhaseOneFinancing(ctx, mission, data); handled || err != nil {
		return changed, err
	}
	financing := data.Capacity.Financing
	if !financing.BuildBy.IsZero() && !time.Now().Before(financing.BuildBy) {
		if !financing.Cancel {
			if err := r.recordCapacityFinanceFailure(ctx, data, "build_deadline", "approved capacity reached its one-game-day construction deadline; repay unused principal"); err != nil {
				return false, err
			}
			if err := r.playerLoanBackoff(ctx, data.BuildPort, data.BuildClass, false, time.Now().UTC()); err != nil {
				return false, err
			}
			if err := r.recordCapacityFinancingRefusal(ctx, data); err != nil {
				return false, err
			}
		}
		financing.Cancel = true
	}
	if financing.Cancel || mission.Phase == "growth_finance_repay" || mission.Phase == "growth_rejected" {
		return r.repayCapacityFinancing(ctx, mission, data)
	}
	_, required, err := r.capacityFinanceReady(ctx, data)
	if err == nil {
		err = r.capacityZoneEstimateReady(ctx, data, financing.Decision)
	}
	if err != nil {
		verdict, hold := capacityGateDetails(err)
		if !hold {
			financing.Cancel = true
		}
		if recordErr := r.recordCapacityFinanceFailure(ctx, data, verdict, err.Error()); recordErr != nil {
			return false, recordErr
		}
		if financing.Cancel {
			if saveErr := r.recordCapacityFinancingRefusal(ctx, data); saveErr != nil {
				return false, saveErr
			}
			if saveErr := r.playerLoanBackoff(ctx, data.BuildPort, data.BuildClass, false, time.Now().UTC()); saveErr != nil {
				return false, saveErr
			}
			return r.repayCapacityFinancing(ctx, mission, data)
		}
		return false, r.holdExpected(ctx, mission.ShipID, err.Error())
	}
	if len(financing.Loans) < len(financing.Quotes) {
		quote := financing.Quotes[len(financing.Loans)]
		path := "/v1/loan/accept-quote"
		if quote.RequestID != nil {
			path = "/v1/loan/approve"
		}
		return true, r.stage(ctx, mission, data, ShipState{}, "loan-accept", path, gameapi.QuoteActionRequest{QuoteID: quote.ID})
	}
	input := r.planningInput(ShipState{})
	if changed, err := r.fundTreasuryMission(ctx, input.Balances, required, "__treasury", nil, mission, false); changed || err != nil {
		return changed, err
	}
	for region, amount := range required {
		if input.Balances[region] < amount {
			return false, r.holdExpected(ctx, mission.ShipID, "financed capacity waits for protected package funding; admission is retained")
		}
	}
	mission.Phase = "growth_build"
	return true, r.stage(ctx, mission, data, ShipState{}, "build", "/v1/shipyard/build-ship", map[string]any{"port_id": data.BuildPort, "name": data.BuildName, "ship_class": data.BuildClass, "build_mode": "turnkey_cash", "mortgage": data.Capacity.Mortgage != nil})
}

func (r *Runner) loanActionReady(ctx context.Context, mission *store.Mission, data *missionData) error {
	if data.Action.Stage == "loan-request" || data.Action.Path == "/v1/loan/approve" {
		return r.playerLoanActionReady(ctx, data)
	}
	if !r.loanAuthority() || !r.fresh("ships") || !r.fresh("vaults") {
		return errors.New("new loan action requires fresh debt, fleet and cash authority")
	}
	if data.Action.Stage == "loan-accept" {
		if data.LoanBridge != nil {
			intent := data.LoanBridge
			var request gameapi.QuoteActionRequest
			loan, exists := r.loanByID(intent.Before.ID)
			if mission.ShipID != "__loans" || mission.Phase != "loan_bridge" || !exists ||
				loanRefinanceDueKey(loan) != loanRefinanceDueKey(intent.Before) ||
				!decode[map[string]bool](r.snapshot, "_loan_bridge_due")[loanRefinanceDueKey(loan)] ||
				json.Unmarshal(data.Action.Body, &request) != nil || request.QuoteID != intent.Quote.Quote.ID ||
				intent.Quote.Quote.CollateralShipID == nil {
				return errors.New("emergency bridge requires its immutable installment, quote and durable due-date marker")
			}
			for _, dataset := range []string{"ship_listings", "loan_requests", "clock"} {
				if !r.fresh(dataset) {
					return errors.New("emergency bridge awaits fresh collateral and installment authority")
				}
			}
			shipID := *intent.Quote.Quote.CollateralShipID
			available := false
			ships, err := r.loanBridgeShips(ctx)
			if err != nil {
				return err
			}
			for _, ship := range ships {
				available = available || ship.Ship.ID == shipID
			}
			if !available || !r.validGoldsmithsQuote(intent.Quote, shipID, loan.BankingRegion, intent.Quote.Quote.PrincipalPence) {
				return errors.New("emergency bridge quote expired or collateral is no longer free")
			}
			shortfall, err := r.loanBridgeShortfall(ctx, loan)
			if err != nil {
				return err
			}
			if shortfall == 0 {
				return errors.New("emergency bridge no longer has an installment cash shortfall")
			}
			principal := max(int64(100_000), min(goldsmithsMaximumPence, shortfall))
			principal = min(goldsmithsMaximumPence, (principal+9_999)/10_000*10_000)
			if principal < intent.Quote.Quote.PrincipalPence {
				return errors.New("emergency bridge cash shortfall shrank below its admitted principal")
			}
			return nil
		}
		if mission.ShipID != "__growth" || data.Capacity == nil || data.Capacity.Financing == nil {
			return errors.New("new borrowing requires its durable capacity financing intent")
		}
		financing := data.Capacity.Financing
		var request gameapi.QuoteActionRequest
		if json.Unmarshal(data.Action.Body, &request) != nil || len(financing.Loans) >= len(financing.Quotes) || request.QuoteID != financing.Quotes[len(financing.Loans)].ID || financing.Cancel {
			return errors.New("new borrowing disagrees with its admitted quote")
		}
		if err := r.capacityZoneEstimateReady(ctx, data, financing.Decision); err != nil {
			return err
		}
		_, _, err := r.capacityFinanceReady(ctx, data)
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
	if loan.IsMortgage && mission.ShipID != "__loans" {
		return errors.New("mortgage cannot be repaid as capacity financing")
	}
	if mission.ShipID == "__loans" && mission.Phase == "loan_prepay" {
		_, validRate := loanAnnualRate(loan)
		if !validRate || loan.IsMortgage || loan.Status != "active" || loan.OwedPence > maximumPence ||
			intent.AmountPence < 0 || intent.AmountPence > loan.OwedPence || intent.AmountPence > maximumPence {
			return errors.New("surplus prepayment requires bounded active non-mortgage debt and supported interest terms")
		}
		input, ready, err := r.loanPrepaymentInput(ctx, loan)
		if err != nil {
			return err
		}
		covered := loanPrepaymentSurplus(input, loan) >= intent.AmountPence
		if intent.AmountPence == 0 {
			covered = input.Balances[loan.BankingRegion]-input.ReserveByRegion[loan.BankingRegion] >= loanFullPayment(loan)
		}
		if !ready || !covered {
			return errors.New("surplus prepayment must preserve growth commitments, reserves and a cash lot for every ship plus one")
		}
		return nil
	}
	input := r.planningInput(ShipState{})
	var protected map[string]int64
	var err error
	if mission.ShipID == "__loans" {
		protected, err = r.loanServiceReserves(ctx, loan.ID)
	} else {
		input, err = r.committedPlanningInputExceptLoan(ctx, ShipState{}, mission.ShipID, loan.ID)
		protected = fullLoanReserves(input)
	}
	if err != nil {
		return err
	}
	amount := intent.AmountPence
	if amount == 0 {
		amount = loanFullPayment(loan)
	}
	if input.Balances[loan.BankingRegion]-amount < protected[loan.BankingRegion] {
		return errors.New("loan repayment would spend another hard cash obligation")
	}
	return nil
}

func (r *Runner) recordAcceptedCapacityLoan(data *missionData, loan gameapi.Loan, quote gameapi.LoanQuote) {
	if loan.IsMortgage {
		return
	}
	financing := data.Capacity.Financing
	if len(financing.Loans) == 0 {
		// The local receipt (or original action for a lost response) fixes the
		// deadline. A lender's older offer creation time is not disbursement.
		acceptedAt := data.Action.ReceiptAt
		if acceptedAt.IsZero() {
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
		if data.LoanBridge != nil {
			var request gameapi.QuoteActionRequest
			quote := data.LoanBridge.Quote.Quote
			return json.Unmarshal(data.Action.Body, &request) == nil && request.QuoteID == quote.ID && loanQuoteIdentity(loan, quote)
		}
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

func loanRepaymentReconciled(mission *store.Mission, loan gameapi.Loan, intent *loanRepaymentIntent) bool {
	if !loanRepaymentObserved(loan, intent) {
		return false
	}
	if loan.OwedPence == 0 && (loan.Status == "repaid" || loan.Status == "recovered") {
		return true
	}
	// Scheduled installments share the counters, so an open loan cannot
	// prove a discretionary prepayment without the original request's receipt.
	if mission.Phase == "loan_prepay" {
		return false
	}
	return mission.ShipID != "__loans" || mission.Phase != "loan_repay" || loan.ArrearsPence == 0
}

func (r *Runner) reconcileLoanAction(ctx context.Context, mission *store.Mission, data *missionData, action *store.Action) (bool, error) {
	if !r.loanAuthority() {
		return false, nil
	}
	var found *gameapi.Loan
	if data.Action.Stage == "loan-accept" {
		var quote gameapi.LoanQuote
		if data.LoanBridge != nil {
			quote = data.LoanBridge.Quote.Quote
		} else {
			if data.Capacity == nil || data.Capacity.Financing == nil {
				return false, nil
			}
			financing := data.Capacity.Financing
			if len(financing.Loans) >= len(financing.Quotes) {
				return false, nil
			}
			quote = financing.Quotes[len(financing.Loans)]
		}
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
		if loan, exists := r.loanByID(intent.Before.ID); exists && loanRepaymentReconciled(mission, loan, intent) {
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
	if data.Action.Stage == "loan-accept" && data.LoanBridge != nil {
		if err := r.recordLoanBridgeDecision(ctx, data.LoanBridge, "emergency Goldsmiths bridge reconciled to its authoritative loan"); err != nil {
			return false, err
		}
	} else if data.Action.Stage == "loan-accept" {
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
		if data.LoanBridge != nil {
			mission.Phase = "done"
			return
		}
		mission.Phase = "growth_finance"
		if data.Capacity.Financing.Cancel {
			mission.Phase = "growth_finance_repay"
		}
	} else if mission.ShipID == "__growth" {
		mission.Phase = "growth_finance_repay"
	} else if mission.Phase == "loan_prepay" {
		mission.Phase = "loan_prepay_done"
		return
	} else {
		mission.Phase = "done"
	}
	data.LoanRepayment = nil
}

func (r *Runner) recordCapacityFinanceFailure(ctx context.Context, data *missionData, verdict, reason string) error {
	financing := data.Capacity.Financing
	port, _ := findPort(r.stateCache.Catalog, data.BuildPort)
	hull, _ := findHull(r.stateCache.Catalog, data.BuildClass)
	var principal, installment int64
	source, shipID := "goldsmiths", ""
	for _, quote := range financing.Quotes {
		principal = growthCashTotal(principal, quote.PrincipalPence)
		if quote.InstallmentPence != nil {
			installment = growthCashTotal(installment, *quote.InstallmentPence)
		}
		if quote.RequestID != nil {
			source = playerQuoteSource(quote)
		}
		if shipID == "" && quote.CollateralShipID != nil {
			shipID = *quote.CollateralShipID
		}
	}
	collateral := "ship_lien"
	if data.PlayerLoan != nil {
		collateral = data.PlayerLoan.CollateralKind
	}
	return r.recordCapacityLoan(ctx, port, hull, capacityLoanDecision{
		Source: source, PrincipalPence: principal, TermGameDays: playerLoanTermGameDays,
		CollateralKind: collateral, ShipID: shipID, Verdict: verdict, Reason: reason,
		EvidenceBasis: financing.Decision.EvidenceBasis,
		Package:       capacityPackageFor(financing.Decision, source, principal, installment),
	})
}
