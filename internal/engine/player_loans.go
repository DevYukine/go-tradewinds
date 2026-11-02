package engine

import (
	"context"
	"encoding/json"
	"math"
	"net/url"
	"slices"
	"sort"
	"time"

	"github.com/DevYukine/go-tradewinds/internal/gameapi"
	"github.com/DevYukine/go-tradewinds/internal/store"
	"github.com/google/uuid"
)

const playerLoanTermGameDays int32 = 360
const playerLoanBackoff = 6 * time.Hour
const playerLoanClockSkew = 5 * time.Second

type capacityPackage struct {
	MortgagePence  int64  `json:"mortgage_pence"`
	Source         string `json:"source"`
	PrincipalPence int64  `json:"principal_pence"`
	Installments   int64  `json:"installments"`
}

func capacityPackageFor(decision GrowthDecision, source string, principal, installment int64) capacityPackage {
	result := capacityPackage{Source: source, PrincipalPence: principal, Installments: installment}
	if decision.Mortgage != nil {
		result.MortgagePence = decision.Mortgage.PrincipalPence
		if decision.Mortgage.InstallmentPence != nil {
			result.Installments = growthCashTotal(result.Installments, *decision.Mortgage.InstallmentPence)
		}
	}
	return result
}

type playerLoanIntent struct {
	RequestID         string           `json:"request_id"`
	PortID            string           `json:"port_id"`
	HullID            string           `json:"hull_id"`
	BankingRegion     string           `json:"banking_region"`
	PrincipalPence    int64            `json:"principal_pence"`
	TermGameDays      int32            `json:"term_game_days"`
	Repayment         string           `json:"repayment"`
	CollateralKind    string           `json:"collateral_kind"`
	ShipID            string           `json:"ship_id"`
	OpenGameHours     int64            `json:"open_game_hours,omitempty"`
	PostedAt          time.Time        `json:"posted_at"`
	ExpiresAt         time.Time        `json:"expires_at"`
	Decision          GrowthDecision   `json:"decision"`
	HeadroomPence     int64            `json:"headroom_pence"`
	OwnCashPence      int64            `json:"own_cash_pence"`
	ProtectedByRegion map[string]int64 `json:"protected_by_region"`
	DonorByRegion     map[string]int64 `json:"donor_by_region"`
	RejectedQuotes    []string         `json:"rejected_quotes"`
}

func (intent *playerLoanIntent) body() gameapi.PostLoanRequest {
	hours := intent.OpenGameHours
	if hours == 0 {
		hours = 24
	}
	return gameapi.PostLoanRequest{
		BankingRegion: intent.BankingRegion, PrincipalPence: intent.PrincipalPence,
		TermGameDays: intent.TermGameDays, Repayment: intent.Repayment,
		CollateralKind: intent.CollateralKind, ShipID: intent.ShipID,
		Recourse: true, AutoQuote: true, OpenGameHours: hours,
	}
}

func (r *Runner) loanRequestAuthority() bool {
	raw := r.snapshot["loan_requests"]
	return r.fresh("loan_requests") && r.meta["loan_requests"].Complete && len(raw) > 0 && raw[0] == '['
}

func (r *Runner) openOwnLoanRequest() bool {
	for _, request := range r.stateCache.LoanRequests {
		if request.BorrowerCompanyID == r.account.CompanyID && request.Status == "open" {
			return true
		}
	}
	return false
}

func (r *Runner) freeLoanShips() []ShipState {
	var listings []struct {
		ShipID string `json:"ship_id"`
		Status string `json:"status"`
	}
	_ = json.Unmarshal(r.snapshot["ship_listings"], &listings)
	blocked := make(map[string]bool)
	for _, listing := range listings {
		if listing.Status == "open" {
			blocked[listing.ShipID] = true
		}
	}
	for _, loan := range r.stateCache.Loans {
		if loan.CollateralShipID != nil && (openMortgage(loan) || loan.CollateralStatus == "pledged" ||
			loan.CollateralStatus == "held" || loan.CollateralStatus == "seizure_pending") {
			blocked[*loan.CollateralShipID] = true
		}
	}
	for _, request := range r.stateCache.LoanRequests {
		if request.BorrowerCompanyID == r.account.CompanyID && request.Status == "open" && request.CollateralShipID != nil {
			blocked[*request.CollateralShipID] = true
		}
	}
	var ships []ShipState
	for _, ship := range r.stateCache.Ships {
		if ship.Ship.Status != "sunk" && !blocked[ship.Ship.ID] {
			ships = append(ships, ship)
		}
	}
	sort.Slice(ships, func(i, j int) bool { return ships[i].Ship.ID < ships[j].Ship.ID })
	return ships
}

// Fleet results do not publish an appraisal. Rank by the cheapest fresh catalog
// class price plus guns; the server rechecks the actual pledge at approval.
func (r *Runner) playerLoanCollateral() (string, int64) {
	var best string
	var bestValue int64
	for _, ship := range r.freeLoanShips() {
		var classPrice int64
		for key, quote := range r.stateCache.Quotes {
			if quote.ShipClass != ship.Ship.ShipClass || !r.fresh("yard_quotes/"+key) ||
				quote.TotalCostPence == nil || *quote.TotalCostPence <= 0 {
				continue
			}
			if classPrice == 0 || *quote.TotalCostPence < classPrice {
				classPrice = *quote.TotalCostPence
			}
		}
		if classPrice == 0 {
			continue
		}
		value := growthCashTotal(classPrice, ship.Ship.CannonsCount*15_000)
		if value > bestValue || value == bestValue && (best == "" || ship.Ship.ID < best) {
			best, bestValue = ship.Ship.ID, value
		}
	}
	return best, bestValue
}

func capacityAnnuityFactor(annualRate int32) float64 {
	if annualRate == 0 {
		return 1.0 / 36
	}
	periodic := float64(annualRate) / 10_000 / 36
	return periodic / (1 - math.Pow(1+periodic, -36))
}

// All loan admission paths share the same half-net, ten-game-day debt bound.
func capacityHalfNetCoverage(net float64, installment int64) bool {
	return installment > 0 && net > 0 && !math.IsNaN(net) && !math.IsInf(net, 0) && net*4 >= float64(installment)
}

func capacityPackageCoverage(decision GrowthDecision, loanInstallment int64) bool {
	if loanInstallment > 0 && !capacityHalfNetCoverage(decision.IncrementalNetPencePerGameDay, loanInstallment) {
		return false
	}
	if decision.Mortgage == nil {
		return loanInstallment > 0
	}
	if decision.Mortgage.InstallmentPence == nil {
		return false
	}
	beforeMortgageInterest := decision.IncrementalNetPencePerGameDay + mortgageInterestPerGameDay(decision.Mortgage)
	return beforeMortgageInterest*10 >= 1.25*float64(growthCashTotal(loanInstallment, *decision.Mortgage.InstallmentPence))
}

func estimatedPlayerPrincipal(shortfall int64) (int64, int64) {
	factor := capacityAnnuityFactor(3_000)
	if shortfall <= 0 || float64(shortfall)/(1-2*factor) >= float64(math.MaxInt64) {
		return 0, 0
	}
	principal := int64(math.Ceil(float64(shortfall) / (1 - 2*factor)))
	for {
		installment := int64(math.Ceil(float64(principal) * factor))
		needed := growthCashTotal(shortfall, installment, installment)
		if needed == math.MaxInt64 {
			return 0, 0
		}
		if principal >= needed {
			return principal, installment
		}
		principal = needed
	}
}

func capacityDebtCashReady(decision GrowthDecision, surplus int64) bool {
	return surplus >= max(0, decision.WorkingCapitalPence-decision.CandidateLotCashPence)
}

func capacityDebtExposure(decision GrowthDecision, principal, netWorth int64, loans []gameapi.Loan, excludedQuotes map[string]bool) bool {
	if decision.Mortgage != nil {
		principal = growthCashTotal(principal, decision.Mortgage.PrincipalPence)
	}
	for _, loan := range loans {
		if (loan.Status == "active" || loan.Status == "defaulted") &&
			(loan.QuoteID == nil || !excludedQuotes[*loan.QuoteID]) {
			principal = growthCashTotal(principal, max(0, loan.OutstandingPrincipalPence))
		}
	}
	if netWorth <= 0 || principal <= 0 || principal == math.MaxInt64 {
		return false
	}
	return principal <= netWorth/20*7+netWorth%20*7/20
}

func (r *Runner) recordPlayerLoanDecision(ctx context.Context, intent *playerLoanIntent, verdict, reason string) error {
	port, _ := findPort(r.stateCache.Catalog, intent.PortID)
	hull, _ := findHull(r.stateCache.Catalog, intent.HullID)
	return r.recordCapacityLoan(ctx, port, hull, capacityLoanDecision{
		Source: "player_request", PrincipalPence: intent.PrincipalPence,
		TermGameDays: intent.TermGameDays, CollateralKind: intent.CollateralKind,
		ShipID: intent.ShipID, Verdict: verdict, Reason: reason,
		EvidenceBasis: intent.Decision.EvidenceBasis + "; estimated rebuild (catalog class price + guns)",
		Package:       capacityPackageFor(intent.Decision, "player_request", intent.PrincipalPence, 0),
	})
}

func (r *Runner) playerLoanBackoff(ctx context.Context, portID, hullID string, fallback bool, at time.Time) error {
	key := portID + "|" + hullID
	backoffs := decode[map[string]time.Time](r.snapshot, "_player_loan_backoffs")
	if backoffs == nil {
		backoffs = make(map[string]time.Time)
	}
	until := at.UTC().Add(playerLoanBackoff)
	backoffs[key] = until
	r.snapshot["_player_loan_backoffs"] = encode(backoffs)
	fallbacks := decode[map[string]time.Time](r.snapshot, "_player_loan_fallback")
	if fallbacks == nil {
		fallbacks = make(map[string]time.Time)
	}
	if fallback {
		fallbacks[key] = until
	} else {
		delete(fallbacks, key)
	}
	r.snapshot["_player_loan_fallback"] = encode(fallbacks)
	return r.db.SaveSnapshot(ctx, r.account.ID, r.snapshot)
}

func (r *Runner) stagePlayerLoan(ctx context.Context, port Port, hull Hull, decision GrowthDecision, principal, installment int64, evidence *growthEvidence) (bool, error) {
	if r.phaseOneYard(port) {
		return false, r.holdExpected(ctx, "", r.overseasGrowthHoldReason(""))
	}
	if !r.loanRequestAuthority() || !r.fresh("ship_listings") {
		return false, r.recordCapacityLoan(ctx, port, hull, capacityLoanDecision{
			Source: "player_request", Verdict: "debt_capacity", Reason: "player request waits for fresh own request and listing authority",
		})
	}
	if r.openOwnLoanRequest() {
		return false, r.recordCapacityLoan(ctx, port, hull, capacityLoanDecision{
			Source: "player_request", Verdict: "request_open", Reason: "an open own request already occupies the single borrower request slot",
		})
	}
	shipID, rebuild := r.playerLoanCollateral()
	if shipID == "" || principal > rebuild {
		if err := r.playerLoanBackoff(ctx, port.ID, hull.ID, true, time.Now().UTC()); err != nil {
			return false, err
		}
		return false, r.recordCapacityLoan(ctx, port, hull, capacityLoanDecision{
			Source: "player_request", PrincipalPence: principal, ShipID: shipID, Verdict: "debt_capacity",
			Reason:        "player request needs a free priced ship whose estimated rebuild (catalog class price + guns) covers principal; Goldsmiths fallback is eligible",
			EvidenceBasis: decision.EvidenceBasis + "; estimated rebuild (catalog class price + guns)",
			Package:       capacityPackageFor(decision, "player_request", principal, 0),
		})
	}
	_, _, _, funded := capacityLoanFunding(decision, evidence, port.BankingRegion, principal, growthCashTotal(installment, installment), r.stateCache.Rates)
	if !funded {
		return false, r.recordCapacityLoan(ctx, port, hull, capacityLoanDecision{
			Source: "player_request", Verdict: "own_cash_buffer", Reason: "player loan cannot fund the yard and preserve the own-cash buffer",
		})
	}
	lower, _, clockOK := r.serverBounds()
	if !clockOK {
		return false, nil
	}
	// Freeze only the package's own capital and debt reserve. The fleet's
	// incumbent working lots must remain available to fund ordinary trading.
	ownRequired := clonePence(evidence.required)
	ownRequired[port.BankingRegion] = growthCashTotal(ownRequired[port.BankingRegion], growthCapital(decision), installment, installment)
	fundedBalances := clonePence(evidence.balances)
	fundedBalances[port.BankingRegion] = growthCashTotal(fundedBalances[port.BankingRegion], principal)
	_, _, transfers, _ := measuredFunding(fundedBalances, ownRequired, r.stateCache.Rates)
	own := max(0, growthCashTotal(growthCapital(decision), installment, installment)-principal)
	protected, donors := map[string]int64{port.BankingRegion: own}, make(map[string]int64)
	for _, transfer := range transfers {
		donors[transfer.Rate.FromRegion] = growthCashTotal(donors[transfer.Rate.FromRegion], transfer.Gross)
		protected[port.BankingRegion] = max(0, protected[port.BankingRegion]-transfer.Net)
	}
	intent := &playerLoanIntent{
		PortID: port.ID, HullID: hull.ID, BankingRegion: port.BankingRegion,
		PrincipalPence: principal, TermGameDays: playerLoanTermGameDays,
		Repayment: "amortised", CollateralKind: "ship_lien", ShipID: shipID,
		PostedAt: lower.Add(-playerLoanClockSkew), Decision: decision,
		HeadroomPence: r.capacityLoanHeadroom(decision), OwnCashPence: own,
		ProtectedByRegion: protected, DonorByRegion: donors,
	}
	mission := &store.Mission{
		AccountID: r.account.ID, ShipID: "__growth", Phase: "growth_loan_request",
		Generation: uuid.NewString(), Destination: port.ID,
	}
	data := missionData{
		PlayerLoan: intent, BuildPort: port.ID, BuildClass: hull.ID,
		StartedAt: time.Now().UTC(), CostsKnown: true, InitialLedgerIDs: r.ledgerIDs(),
		StartedGameHours: r.stateCache.Clock.Calendar.TotalGameHours,
	}
	reason := "posting one recourse amortised 360-game-day ship-lien request; principal does not exceed estimated rebuild (catalog class price + guns)"
	if err := r.recordPlayerLoanDecision(ctx, intent, "request_posted", reason); err != nil {
		return false, err
	}
	err := r.stage(ctx, mission, &data, ShipState{}, "loan-request", "/v1/loan/request", intent.body())
	return data.Action != nil, err
}

func playerRequestMatches(request gameapi.LoanRequestView, intent *playerLoanIntent, companyID string) bool {
	return intent != nil && request.ID != "" && request.BorrowerCompanyID == companyID &&
		request.BankingRegion == intent.BankingRegion && request.PrincipalPence == intent.PrincipalPence &&
		request.TermGameDays == intent.TermGameDays && request.Repayment == intent.Repayment &&
		request.CollateralKind == intent.CollateralKind && request.Recourse && request.AutoQuote &&
		(intent.CollateralKind == "ship_lien" && intent.ShipID != "" && request.CollateralShipID != nil && *request.CollateralShipID == intent.ShipID ||
			intent.CollateralKind == "unsecured" && intent.ShipID == "" && request.CollateralShipID == nil) &&
		!request.CreatedAt.Before(intent.PostedAt)
}

func (r *Runner) acceptPlayerLoanRequest(data *missionData, raw json.RawMessage) bool {
	var request gameapi.LoanRequestView
	if json.Unmarshal(raw, &request) != nil || !playerRequestMatches(request, data.PlayerLoan, r.account.CompanyID) || request.ExpiresAt.IsZero() {
		return false
	}
	data.PlayerLoan.RequestID, data.PlayerLoan.ExpiresAt = request.ID, request.ExpiresAt
	return true
}

func (r *Runner) reconcilePlayerLoanRequest(ctx context.Context, mission *store.Mission, data *missionData, action *store.Action) (bool, error) {
	if !r.loanRequestAuthority() || data.PlayerLoan == nil {
		return false, nil
	}
	var found *gameapi.LoanRequestView
	for _, request := range r.stateCache.LoanRequests {
		if !playerRequestMatches(request, data.PlayerLoan, r.account.CompanyID) {
			continue
		}
		if found != nil {
			return false, r.hold(ctx, "", "multiple authoritative requests match the immutable borrower intent; request remains quarantined")
		}
		copy := request
		found = &copy
	}
	if found == nil {
		return false, nil
	}
	if action.State != store.ActionComplete {
		evidence := encode(map[string]any{"kind": "authoritative_loan_request", "request": found, "observed_at": r.meta["loan_requests"].UpdatedAt})
		if err := r.db.ReconcileAction(ctx, action.ID, evidence); err != nil {
			return false, err
		}
	}
	if !r.acceptPlayerLoanRequest(data, encode(found)) {
		return false, nil
	}
	return r.applyReconciliation(ctx, mission, data)
}

func (r *Runner) playerCapacityNeed(ctx context.Context, intent *playerLoanIntent) (Port, Hull, GrowthDecision, *growthEvidence, error) {
	port, portOK := findPort(r.stateCache.Catalog, intent.PortID)
	hull, hullOK := findHull(r.stateCache.Catalog, intent.HullID)
	if err := r.capacityInputsReady(); err != nil {
		return port, hull, GrowthDecision{}, nil, err
	}
	if !portOK || !hullOK || !capacityHull(hull) || port.BankingRegion != intent.BankingRegion {
		return port, hull, GrowthDecision{}, nil, capacityGate("hull", true, "capacity waits for its recorded catalog yard and hull")
	}
	if r.phaseOneYard(port) {
		return port, hull, GrowthDecision{}, nil, capacityGate("overseas_first", true, r.overseasGrowthHoldReason(""))
	}
	planning, err := r.committedPlanningInputExcept(ctx, ShipState{}, "__growth")
	if err != nil {
		return port, hull, GrowthDecision{}, nil, err
	}
	evidence := &growthEvidence{
		asOf: time.Now().UTC(), balances: planning.Balances, required: planning.ReserveByRegion,
		workingByRegion: make(map[string]int64),
	}
	for region, balance := range planning.Balances {
		evidence.available = growthCashTotal(evidence.available, max(0, balance-evidence.required[region]))
	}
	input, reason, err := r.capacityFleetInput(ctx, evidence)
	if err != nil {
		return port, hull, GrowthDecision{}, evidence, err
	}
	if reason != "" {
		return port, hull, GrowthDecision{}, evidence, capacityGate("authority", true, reason)
	}
	quote, err := r.growthQuote(port, hull)
	if err != nil {
		return port, hull, GrowthDecision{}, evidence, capacityGate("presence", true, err.Error())
	}
	input.FinancingFixed, input.Mortgage = true, intent.Decision.Mortgage
	mortgageChanged := false
	if input.Mortgage != nil {
		if !r.mortgageAvailable() {
			return port, hull, GrowthDecision{}, evidence, capacityGate("mortgage_unavailable", true, "capacity waits for the mortgage slot")
		}
		// Mortgage terms follow the live price. Re-evaluate the whole package,
		// rather than repeatedly staging an approval with an obsolete down payment.
		if err := r.api.Get(ctx, "/v1/shipyard/quote?port_id="+url.QueryEscape(port.ID)+"&ship_class="+url.QueryEscape(hull.ID)+"&build_mode=turnkey_cash", &quote); err != nil {
			return port, hull, GrowthDecision{}, evidence, capacityGate("mortgage_unavailable", true, err.Error())
		}
		if quote.PortID != port.ID || quote.ShipClass != hull.ID || !quote.Available ||
			quote.TotalCostPence == nil || !validMortgageTerms(quote.Mortgage, *quote.TotalCostPence) {
			return port, hull, GrowthDecision{}, evidence, capacityGate("mortgage_unavailable", true, "capacity waits for available live mortgage terms")
		}
		input.Mortgage = quote.Mortgage
		mortgageChanged = !sameMortgageTerms(intent.Decision.Mortgage, quote.Mortgage)
	}
	grown := append(append([]ShipState(nil), r.stateCache.Ships...), growthCandidate(port, hull))
	input.IncrementalDailyUpkeepPence = DailyUpkeep(grown, r.stateCache.Catalog, len(r.stateCache.Factors)) - DailyUpkeep(r.stateCache.Ships, r.stateCache.Catalog, len(r.stateCache.Factors))
	decision, _, err := r.capacityAtYard(ctx, port, hull, quote, input, evidence)
	if err != nil {
		return port, hull, decision, evidence, err
	}
	if !decision.EconomicallyViable {
		gate := decision.Verdict
		if gate == "" {
			gate = "need_stale"
		}
		hold := mortgageChanged || gate == "measured_net" || gate == "mortgage_unavailable" || decision.Reason == hazardUnavailable
		if gate == "debt_capacity" && evidence.hazard.netWorth <= 0 {
			hold = true
		}
		return port, hull, decision, evidence, capacityGate(gate, hold, decision.Reason)
	}
	if decision.Accepted {
		return port, hull, decision, evidence, capacityGate("need_stale", false, "capacity no longer needs debt")
	}
	if !capacityDebtCashReady(decision, evidence.available) {
		return port, hull, decision, evidence, capacityGate("own_cash_buffer", true, "capacity waits for its own working-cash buffer")
	}
	decision, gate, err := r.capacityDebtDecision(ctx, hull, decision, evidence.asOf, nil)
	if err != nil {
		return port, hull, decision, evidence, err
	}
	if gate != "" {
		return port, hull, decision, evidence, capacityGate(gate, mortgageChanged || gate == "measured_net", "capacity need fails "+gate)
	}
	if _, err := r.growthNetWorth(ctx, evidence); err != nil {
		return port, hull, decision, evidence, err
	}
	if evidence.hazard.netWorth <= 0 {
		return port, hull, decision, evidence, capacityGate("debt_capacity", true, "capacity waits for fresh positive net worth")
	}
	if !capacityDebtExposure(decision, intent.PrincipalPence, evidence.hazard.netWorth, r.stateCache.Loans, nil) {
		return port, hull, decision, evidence, capacityGate("debt_capacity", mortgageChanged, "gross outstanding debt exceeds 35% of fresh net worth")
	}
	return port, hull, decision, evidence, nil
}

func (r *Runner) playerQuoteGate(request gameapi.LoanRequestView, intent *playerLoanIntent, quote gameapi.LoanQuote) string {
	if quote.RequestID == nil || *quote.RequestID != intent.RequestID || request.ID != intent.RequestID ||
		quote.ID == "" || quote.Status != "open" || slices.Contains(intent.RejectedQuotes, quote.ID) {
		return "request_mismatch"
	}
	if quote.BankingRegion != request.BankingRegion || quote.BorrowerCompanyID != r.account.CompanyID ||
		quote.PrincipalPence != request.PrincipalPence || quote.TermGameDays != request.TermGameDays ||
		quote.Repayment != request.Repayment || quote.InstallmentPence == nil || *quote.InstallmentPence <= 0 ||
		quote.TotalRepayablePence < quote.PrincipalPence {
		return "quote_terms"
	}
	if quote.LenderCompanyID == "" || quote.LenderCompanyID == r.account.CompanyID {
		return "quote_lender"
	}
	if quote.AnnualRateBPS < 0 || quote.AnnualRateBPS > request.GoldsmithsRateBPS {
		return "quote_rate"
	}
	_, upper, ok := r.serverBounds()
	gameHour := time.Hour / time.Duration(max(int64(1), r.stateCache.Clock.Calendar.TimeScale))
	if !ok || !upper.Add(gameHour).Before(quote.ExpiresAt) {
		return "quote_expiry"
	}
	return ""
}

func (r *Runner) recordedPlayerRequest(intent *playerLoanIntent) *gameapi.LoanRequestView {
	for index := range r.stateCache.LoanRequests {
		if r.stateCache.LoanRequests[index].ID == intent.RequestID {
			return &r.stateCache.LoanRequests[index]
		}
	}
	return nil
}

func playerQuoteSource(quote gameapi.LoanQuote) string {
	if quote.Auto || quote.LendingLineID != nil {
		return "goldsmiths"
	}
	return "player_request"
}

func (r *Runner) drivePlayerLoan(ctx context.Context, mission *store.Mission, data *missionData) (bool, error) {
	if handled, changed, err := r.retirePhaseOneFinancing(ctx, mission, data); handled || err != nil {
		return changed, err
	}
	intent := data.PlayerLoan
	if intent.RequestID != "" && (!r.loanAuthority() || !r.loanRequestAuthority()) {
		if err := r.recordPlayerLoanDecision(ctx, intent, "authority", "player request waits for current borrower loans and own requests"); err != nil {
			return false, err
		}
		return false, r.hold(ctx, "", "player request waits for current borrower loans and own requests")
	}
	var request *gameapi.LoanRequestView
	if intent.RequestID != "" {
		request = r.recordedPlayerRequest(intent)
		if request == nil {
			return false, r.hold(ctx, "", "recorded player request is missing from authoritative own requests")
		}
		if !playerRequestMatches(*request, intent, r.account.CompanyID) {
			return false, r.recordPlayerLoanDecision(ctx, intent, "request_mismatch", "recorded request terms changed")
		}
		_, upper, ok := r.serverBounds()
		if !ok {
			return false, nil
		}
		if request.Status != "open" || !upper.Before(request.ExpiresAt) {
			if request.Status == "funded" {
				return false, r.hold(ctx, "", "funded request awaits its quote-specific durable loan reconciliation")
			}
			if err := r.recordPlayerLoanDecision(ctx, intent, "request_expired", "unfunded request lapsed; this candidate may use Goldsmiths for six real hours"); err != nil {
				return false, err
			}
			if err := r.playerLoanBackoff(ctx, intent.PortID, intent.HullID, true, time.Now().UTC()); err != nil {
				return false, err
			}
			return true, r.db.DeleteMission(ctx, r.account.ID, mission.ShipID)
		}
	}
	if request == nil {
		if err := r.playerRequestReady(ctx, data); err != nil {
			verdict, _ := capacityGateDetails(err)
			if recordErr := r.recordPlayerLoanDecision(ctx, intent, verdict, err.Error()); recordErr != nil {
				return false, recordErr
			}
			// No request was sent. Release the obsolete body and all its cash
			// reservations so ordinary admission can resize and choose again.
			return true, r.db.DeleteMission(ctx, r.account.ID, mission.ShipID)
		}
		return true, r.stage(ctx, mission, data, ShipState{}, "loan-request", "/v1/loan/request", intent.body())
	}
	_, _, decision, evidence, err := r.playerCapacityNeed(ctx, intent)
	if err != nil {
		verdict, hold := capacityGateDetails(err)
		if hold {
			if recordErr := r.recordPlayerLoanDecision(ctx, intent, verdict, err.Error()); recordErr != nil {
				return false, recordErr
			}
			return false, r.hold(ctx, "", err.Error())
		}
		if recordErr := r.recordPlayerLoanDecision(ctx, intent, verdict, err.Error()); recordErr != nil {
			return false, recordErr
		}
		if err := r.playerLoanBackoff(ctx, intent.PortID, intent.HullID, false, time.Now().UTC()); err != nil {
			return false, err
		}
		return true, r.db.DeleteMission(ctx, r.account.ID, mission.ShipID)
	}
	installment := int64(math.Ceil(float64(intent.PrincipalPence) * capacityAnnuityFactor(request.GoldsmithsRateBPS)))
	if _, _, _, funded := capacityLoanFunding(decision, evidence, intent.BankingRegion, intent.PrincipalPence, growthCashTotal(installment, installment), r.stateCache.Rates); !funded {
		return false, r.hold(ctx, "", "published request rate waits for enough own cash to fund the package")
	}
	quotes := append([]gameapi.LoanQuote(nil), request.Quotes...)
	sort.Slice(quotes, func(i, j int) bool {
		if quotes[i].AnnualRateBPS != quotes[j].AnnualRateBPS {
			return quotes[i].AnnualRateBPS < quotes[j].AnnualRateBPS
		}
		return quotes[i].ID < quotes[j].ID
	})
	for _, quote := range quotes {
		if gate := r.playerQuoteGate(*request, intent, quote); gate != "" {
			if err := r.recordPlayerLoanDecision(ctx, intent, gate, "quote "+quote.ID+" fails "+gate); err != nil {
				return false, err
			}
			continue
		}
		result, err := r.playerApprovalReady(ctx, data, quote)
		if err != nil {
			gate, hold := capacityGateDetails(err)
			if hold {
				if recordErr := r.recordPlayerLoanDecision(ctx, intent, gate, err.Error()); recordErr != nil {
					return false, recordErr
				}
				return false, r.hold(ctx, "", err.Error())
			}
			if err := r.recordPlayerLoanDecision(ctx, intent, gate, err.Error()); err != nil {
				return false, err
			}
			continue
		}
		port, _ := findPort(r.stateCache.Catalog, intent.PortID)
		hull, _ := findHull(r.stateCache.Catalog, intent.HullID)
		if err := r.recordCapacityLoan(ctx, port, hull, result); err != nil {
			return false, err
		}
		if data.BuildName == "" {
			name, ready, err := r.reserveShipName(ctx, mission.ShipID)
			if err != nil || !ready {
				return false, err
			}
			data.BuildName = name
		}
		data.TargetCannons = min(4, hull.MaxCannons)
		mission.Phase = "growth_finance"
		return true, r.stage(ctx, mission, data, ShipState{}, "loan-accept", "/v1/loan/approve", gameapi.QuoteActionRequest{QuoteID: quote.ID})
	}
	r.wakeBy(request.ExpiresAt)
	return false, nil
}

func (r *Runner) playerLoanActionReady(ctx context.Context, data *missionData) error {
	if data.Action.Stage == "loan-request" {
		var body gameapi.PostLoanRequest
		if data.PlayerLoan == nil || json.Unmarshal(data.Action.Body, &body) != nil || body != data.PlayerLoan.body() {
			return capacityGate("request_mismatch", false, "player loan request differs from its immutable intent")
		}
		return r.playerRequestReady(ctx, data)
	}
	var body gameapi.QuoteActionRequest
	if json.Unmarshal(data.Action.Body, &body) != nil || data.PlayerLoan == nil {
		return capacityGate("request_mismatch", true, "player approval has no recorded quote")
	}
	request := r.recordedPlayerRequest(data.PlayerLoan)
	if request != nil {
		for _, quote := range request.Quotes {
			if quote.ID == body.QuoteID {
				_, err := r.playerApprovalReady(ctx, data, quote)
				return err
			}
		}
	}
	return capacityGate("request_mismatch", true, "player approval waits for its recorded open quote")
}

func (r *Runner) playerRequestReady(ctx context.Context, data *missionData) error {
	if data.PlayerLoan == nil || !r.loanAuthority() || !r.loanRequestAuthority() || !r.fresh("ship_listings") {
		return capacityGate("authority", true, "player request needs fresh loan, request and listing authority")
	}
	intent := data.PlayerLoan
	if r.openOwnLoanRequest() {
		return capacityGate("request_open", true, "an own request already occupies the borrower slot")
	}
	_, _, decision, evidence, err := r.playerCapacityNeed(ctx, intent)
	if err != nil {
		return err
	}
	shipID, rebuild := r.playerLoanCollateral()
	if intent.CollateralKind != "ship_lien" || shipID == "" || intent.PrincipalPence > rebuild || shipID != intent.ShipID {
		return capacityGate("debt_capacity", false, "player request needs the highest estimated-rebuild free priced ship to cover principal")
	}
	installment := int64(math.Ceil(float64(intent.PrincipalPence) * capacityAnnuityFactor(3_000)))
	result := evaluateCapacityLoan(decision, decision.CashShortfallPence, intent.PrincipalPence, intent.PrincipalPence, installment, 3_000, r.capacityPaybackLimit())
	_, _, _, funded := capacityLoanFunding(decision, evidence, intent.BankingRegion, intent.PrincipalPence, result.DebtReservePence, r.stateCache.Rates)
	if !result.Borrow {
		return capacityGate(result.Verdict, false, result.Reason)
	}
	if !funded {
		return capacityGate("own_cash_buffer", true, "player request waits for its coverage and own-cash funding")
	}
	return nil
}

// Selection and the actual approve first-send use the same gates. No quote can
// enter a stage/veto loop merely because the selection omitted a precondition.
func (r *Runner) playerApprovalReady(ctx context.Context, data *missionData, quote gameapi.LoanQuote) (capacityLoanDecision, error) {
	var result capacityLoanDecision
	intent := data.PlayerLoan
	if intent == nil || !r.loanAuthority() || !r.loanRequestAuthority() {
		return result, capacityGate("authority", true, "player approval needs fresh borrower loans and own requests")
	}
	request := r.recordedPlayerRequest(intent)
	if request == nil || request.Status != "open" || !playerRequestMatches(*request, intent, r.account.CompanyID) {
		return result, capacityGate("request_mismatch", true, "player approval waits for its immutable open request")
	}
	if gate := r.playerQuoteGate(*request, intent, quote); gate != "" {
		return result, capacityGate(gate, false, "player approval fails "+gate)
	}
	port, _, decision, evidence, err := r.playerCapacityNeed(ctx, intent)
	if err != nil {
		return result, err
	}
	reserve := growthCashTotal(*quote.InstallmentPence, *quote.InstallmentPence)
	if max(0, growthCashTotal(growthCapital(decision), reserve)-quote.PrincipalPence) > intent.OwnCashPence {
		return result, capacityGate("own_cash_buffer", true, "player approval exceeds the posting-time own-capital share")
	}
	_, protected, donors, funded := capacityLoanFunding(decision, evidence, port.BankingRegion, quote.PrincipalPence, reserve, r.stateCache.Rates)
	if !funded {
		return result, capacityGate("own_cash_buffer", true, "player approval waits for the current own-cash share and buffer")
	}
	// Funding includes all current own surplus: posting-time headroom need not
	// be borrowed again when ordinary cash can absorb a small deficit.
	shortfall := min(decision.CashShortfallPence, max(1, quote.PrincipalPence-reserve))
	result = evaluateCapacityLoan(decision, shortfall, quote.PrincipalPence, quote.PrincipalPence, *quote.InstallmentPence, quote.AnnualRateBPS, r.capacityPaybackLimit())
	if !result.Borrow {
		return result, capacityGate(result.Verdict, false, result.Reason)
	}
	quote.CollateralShipID = request.CollateralShipID
	candidate := *data
	candidate.Capacity = &capacityIntent{
		BuildPort: port.ID, BuildClass: intent.HullID,
		QuoteLimitPence: growthCashTotal(decision.QuotedHullPence, decision.SlippageReservePence),
		AdmittedAt:      time.Now().UTC(), Mortgage: decision.Mortgage,
		Financing: &capacityFinancing{
			Quotes: []gameapi.LoanQuote{quote}, Decision: decision,
			ProtectedByRegion: protected, DonorByRegion: donors,
		},
	}
	if err := r.capacityZoneEstimateReady(ctx, &candidate, decision); err != nil {
		return result, err
	}
	if _, _, err := r.capacityFinanceReady(ctx, &candidate); err != nil {
		return result, err
	}
	data.Capacity = candidate.Capacity
	source := playerQuoteSource(quote)
	result.Source, result.TermGameDays = source, playerLoanTermGameDays
	result.CollateralKind, result.ShipID = intent.CollateralKind, intent.ShipID
	result.Package = capacityPackageFor(decision, source, quote.PrincipalPence, *quote.InstallmentPence)
	result.EvidenceBasis = decision.EvidenceBasis
	return result, nil
}

// An unresolved mutation is replayed only after current REST authority has
// ruled out a committed result; a duplicate match must stay quarantined.
func (r *Runner) playerLoanReplayHold(data *missionData) string {
	if data.PlayerLoan == nil || !r.loanAuthority() || !r.loanRequestAuthority() {
		return "player loan replay waits for fresh borrower loans and own requests"
	}
	if data.Action.Stage == "loan-request" {
		for _, request := range r.stateCache.LoanRequests {
			if request.Status == "open" || playerRequestMatches(request, data.PlayerLoan, r.account.CompanyID) {
				return "player request replay cannot exclude an existing own request"
			}
		}
		return ""
	}
	if data.Capacity == nil || data.Capacity.Financing == nil {
		return "player approval replay lacks its frozen financing intent"
	}
	financing := data.Capacity.Financing
	if len(financing.Loans) >= len(financing.Quotes) {
		return "player approval replay already has its named loan"
	}
	quote := financing.Quotes[len(financing.Loans)]
	for _, loan := range r.stateCache.Loans {
		if loanQuoteIdentity(loan, quote) {
			return "player approval replay found unresolved quote-specific debt"
		}
	}
	return ""
}
