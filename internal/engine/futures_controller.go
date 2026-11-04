package engine

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"net/http"
	"net/url"
	"slices"
	"time"

	"github.com/DevYukine/go-tradewinds/internal/gameapi"
	"github.com/DevYukine/go-tradewinds/internal/store"
	"github.com/google/uuid"
)

const futuresBackstop = time.Hour

// These are estimates, not a simulated futures return model. Demand a five
// percent edge after rent, one percent/day margin carry and credit loss.
func futuresEdge(alternative int64) int64 { return max(100, ceilBPS(alternative, 500)) }

type futuresAdmission struct {
	ModelSource       string                   `json:"model_source"`
	SpotAlternative   int64                    `json:"spot_alternative_pence"`
	RentPence         int64                    `json:"rent_pence"`
	FundingPence      int64                    `json:"funding_pence"`
	CreditLossPence   int64                    `json:"credit_loss_pence"`
	ExpectedNetPence  int64                    `json:"expected_net_pence"`
	CreditObservedAt  time.Time                `json:"credit_observed_at"`
	BookBudgetPence   int64                    `json:"book_budget_pence"`
	BindingConstraint string                   `json:"binding_constraint"`
	Listing           *gameapi.FuturesContract `json:"listing,omitempty"`
	Lease             *futuresLeaseReservation `json:"lease,omitempty"`
}

func futuresAdmissionStage(rows []store.FuturesIntent) (string, string, any) {
	if len(rows) > 1 {
		ids := make([]string, 0, len(rows))
		for _, row := range rows {
			ids = append(ids, row.ListingID)
		}
		return "futures-accept-batch", "/v1/future/accept-batch", map[string]any{"futures_ids": ids}
	}
	return "futures-accept", "/v1/future/accept", map[string]string{"futures_id": rows[0].ListingID}
}

func futuresAdmissionStageName(stage string) bool {
	return stage == "futures-create" || stage == "futures-accept" || stage == "futures-accept-batch"
}

func (r *Runner) futuresActionIntents(ctx context.Context, data *missionData) ([]store.FuturesIntent, error) {
	rows, err := r.db.FuturesIntents(ctx, r.account.ID)
	if err != nil {
		return nil, err
	}
	first, err := r.futuresIntent(ctx, data)
	if err != nil {
		return nil, err
	}
	selected := make([]store.FuturesIntent, 0, 2)
	for _, row := range rows {
		if row.ActionID == first.ActionID && row.CohortID == first.CohortID {
			selected = append(selected, row)
		}
	}
	// The sale is always first in both the durable request and its recovery.
	slices.SortStableFunc(selected, func(a, b store.FuturesIntent) int {
		if a.Side == b.Side {
			return 0
		}
		if a.Side == "ask" {
			return -1
		}
		return 1
	})
	return selected, nil
}

func (r *Runner) refreshFuturesCounterparties(ctx context.Context, contracts []gameapi.FuturesContract) error {
	now := time.Now().UTC()
	seen := make(map[string]bool)
	tasks := make([]fetchTask, 0)
	for _, contract := range contracts {
		id := contract.CreatorCompanyID
		if contract.Status != "open" || id == r.account.CompanyID || id == "" || seen[id] {
			continue
		}
		seen[id] = true
		key := "futures_credit/" + id
		meta := r.meta[key]
		if now.Sub(maxTime(meta.AttemptedAt, meta.UpdatedAt)) < 6*time.Hour {
			continue
		}
		tasks = append(tasks, fetchTask{key: key, path: "/v1/loan/credit?company_id=" + url.QueryEscape(id)})
		if len(tasks) == 25 {
			break
		}
	}
	return r.captureWave(ctx, tasks, nil)
}

func (r *Runner) futuresCreditLoss(companyID string, notional int64) (int64, time.Time, bool) {
	key := "futures_credit/" + companyID
	meta := r.meta[key]
	var report struct {
		CompanyID     string `json:"company_id"`
		Claims        *int64 `json:"futures_claims_open_pence"`
		Defaults      *int64 `json:"loans_defaulted"`
		FuturesClaims *int64 `json:"futures_claims_total"`
	}
	if !meta.Complete || meta.Stale || meta.Error != "" || meta.UpdatedAt.IsZero() ||
		time.Since(meta.UpdatedAt) > 6*time.Hour || json.Unmarshal(r.snapshot[key], &report) != nil ||
		report.CompanyID != companyID || report.Claims == nil || *report.Claims != 0 ||
		report.Defaults == nil || *report.Defaults < 0 || report.FuturesClaims == nil || *report.FuturesClaims < 0 {
		return 0, time.Time{}, false
	}
	bps := int64(500)
	if *report.Defaults > 0 || *report.FuturesClaims > 0 {
		bps = 2_500
	}
	return ceilBPS(notional, bps), meta.UpdatedAt, true
}

func futuresCrossOrdered(sale, purchase gameapi.FuturesContract, timeScale int64) bool {
	return timeScale > 0 && sale.Side == "bid" && purchase.Side == "ask" && sale.PortID == purchase.PortID &&
		sale.CommodityID == purchase.CommodityID && sale.CreatorCompanyID != purchase.CreatorCompanyID &&
		sale.LotTons == purchase.LotTons && sale.LotTons > 0 &&
		!purchase.MaturesAt.Add(time.Hour/time.Duration(timeScale)).After(sale.MaturesAt)
}

func futuresCarry(notional, tons int64, days float64) (rent, funding int64) {
	rent = int64(math.Ceil(float64(tons) * 50 * days))
	funding = int64(math.Ceil(float64(notional/4) * 0.01 * days))
	return rent, funding
}

func (r *Runner) futuresBookBudgets(ctx context.Context, rows []store.FuturesIntent) (map[string]int64, map[string]int64, error) {
	input, err := r.committedPlanningInput(ctx, ShipState{})
	if err != nil {
		return nil, nil, err
	}
	required := r.growthLoanReserves(input.ReserveByRegion)
	free := make(map[string]int64, len(input.Balances))
	budgets := make(map[string]int64, len(input.Balances))
	for region, balance := range input.Balances {
		free[region] = balance - required[region]
	}
	totalFree := growthFreeCash(input.Balances, required)
	hold := r.debtServiceHoldPence()
	if totalFree < hold {
		if err := r.recordDebtServiceHold(ctx, totalFree); err != nil {
			return nil, nil, err
		}
		return nil, nil, fmt.Errorf("%s: %w", debtServiceHoldPrefix, errFuturesPrecondition)
	}
	for region := range free {
		free[region] = min(free[region], totalFree-hold)
		budgets[region] = free[region]
	}
	exposure := make(map[string]int64)
	for _, row := range rows {
		port, exists := findPort(input.Catalog, row.PortID)
		if !exists || store.FuturesTerminal(row.Status) {
			continue
		}
		budgets[port.BankingRegion] += row.ReservedCashPence
		if row.FuturesID != "" {
			budgets[port.BankingRegion] -= row.MarginPence
		}
		exposure[port.BankingRegion] += row.ExposurePence
	}
	for region, capital := range budgets {
		budgets[region] = max(0, capital/10-exposure[region])
	}
	return budgets, free, nil
}

func futuresReceivingReserved(rows []store.FuturesIntent, port string) int64 {
	var reserved int64
	for _, row := range rows {
		if row.PortID != port || row.Side != "bid" || row.Status == "cancelled" || row.Status == "rejected" {
			continue
		}
		_, known := store.FuturesSettlement(row)
		if !store.FuturesTerminal(row.Status) || !known {
			reserved += row.LotTons
		}
	}
	return reserved
}

func (r *Runner) futuresReceivingSpace(rows []store.FuturesIntent, port string) int64 {
	warehouse, _ := r.futuresWarehouse(port, "")
	return warehouse.FreeTonnage - futuresReceivingReserved(rows, port)
}

func futuresRemainingStock(rows []store.FuturesIntent, cohort string, state *futuresState) (int64, bool) {
	var remaining int64
	if state != nil {
		if cohort == legacyFuturesCohort {
			remaining -= state.LiquidatedTons
			if state.ProcurementPaid > 0 || state.ProcurementUnknown {
				remaining++
			}
		} else {
			remaining -= state.StockExits[cohort].Tons
		}
	}
	for _, row := range rows {
		if row.CohortID != cohort || row.Status == "cancelled" || row.Status == "rejected" {
			continue
		}
		if !store.FuturesTerminal(row.Status) {
			return 0, false
		}
		receipt, known := store.FuturesSettlement(row)
		if !known {
			return 0, false
		}
		if row.Side == "bid" {
			remaining += int64(receipt.SettledTons)
		} else {
			remaining -= int64(receipt.SettledTons)
		}
	}
	return max(0, remaining), true
}

func (r *Runner) futuresCover(rows []store.FuturesIntent, port, commodity string, state *futuresState) int64 {
	_, held := r.futuresWarehouse(port, commodity)
	seen := make(map[string]bool)
	for _, row := range rows {
		if row.PortID != port || row.CommodityID != commodity {
			continue
		}
		if row.Side == "ask" && !store.FuturesTerminal(row.Status) {
			held -= row.ReservedTons
		}
		if seen[row.CohortID] {
			continue
		}
		seen[row.CohortID] = true
		remaining, known := futuresRemainingStock(rows, row.CohortID, state)
		if known {
			held -= remaining
		} else if row.CohortID == legacyFuturesCohort && state != nil && !state.LegacyClosed {
			return 0
		} else {
			for _, purchase := range rows {
				if purchase.CohortID == row.CohortID && purchase.Side == "bid" && store.FuturesTerminal(purchase.Status) &&
					purchase.Status != "cancelled" && purchase.Status != "rejected" {
					held -= purchase.LotTons
				}
			}
		}
	}
	if state != nil && !state.LegacyClosed && port == "amsterdam" && commodity == "water_beer" && !seen[legacyFuturesCohort] {
		remaining, _ := futuresRemainingStock(rows, legacyFuturesCohort, state)
		held -= remaining
	}
	return max(0, held)
}

func (r *Runner) futuresForeignListing(listing gameapi.FuturesContract) bool {
	port, exists := findPort(r.stateCache.Catalog, listing.PortID)
	if !exists || !port.IsHub || listing.ID == "" || listing.CreatorCompanyID == "" ||
		listing.CreatorCompanyID == r.account.CompanyID || listing.Status != "open" ||
		(listing.Side != "bid" && listing.Side != "ask") || !r.fresh("player") {
		return false
	}
	var profile struct {
		Companies []struct {
			ID string `json:"id"`
		} `json:"companies"`
	}
	if json.Unmarshal(r.snapshot["player"], &profile) != nil || profile.Companies == nil {
		return false
	}
	for _, company := range profile.Companies {
		if company.ID == listing.CreatorCompanyID {
			return false
		}
	}
	return true
}

func (r *Runner) evaluateFuturesListing(ctx context.Context, listing gameapi.FuturesContract, rows []store.FuturesIntent, budget, held int64) (*store.FuturesIntent, error) {
	port, exists := findPort(r.stateCache.Catalog, listing.PortID)
	lower, upper, clockOK := r.serverBounds()
	q := int64(listing.LotTons)
	if !exists || !port.IsHub || !clockOK || !r.futuresForeignListing(listing) || listing.Side != "bid" ||
		q <= 0 || listing.StrikePricePence <= 0 || listing.StrikePricePence > store.FuturesExposureLimitPence/q ||
		!listing.MaturesAt.After(upper.Add(time.Hour/time.Duration(r.stateCache.Clock.Calendar.TimeScale))) ||
		listing.MaturesAt.Sub(upper).Hours()*float64(r.stateCache.Clock.Calendar.TimeScale) > 1440 {
		return nil, nil
	}
	for _, row := range rows {
		if row.ListingID == listing.ID {
			return nil, nil
		}
	}
	if !r.fresh("books/"+port.ID) || !r.fresh("ports/"+port.ID) {
		return nil, nil
	}
	market := r.stateCache.Markets[port.ID]
	entry := bookCommodity(market, listing.CommodityID)
	if market.Visibility != "quay" || entry == nil || entry.Depth == nil || market.CustomsBPS < 0 || market.CustomsBPS > 10000 {
		return nil, nil
	}
	notional := q * listing.StrikePricePence
	loss, creditAt, creditOK := r.futuresCreditLoss(listing.CreatorCompanyID, notional)
	if !creditOK {
		return nil, nil
	}
	days := listing.MaturesAt.Sub(upper).Hours() * float64(r.stateCache.Clock.Calendar.TimeScale) / 24
	rent, carry := futuresCarry(notional, q, days)
	admission := futuresAdmission{ModelSource: "observed depth with conservative carry and credit allowance; no futures simulator", RentPence: rent,
		FundingPence: carry, CreditLossPence: loss, CreditObservedAt: creditAt, BookBudgetPence: budget, BindingConstraint: "regional reserves, ten percent book and twenty-five percent counterparty", Listing: &listing}
	intent := store.FuturesIntent{AccountID: r.account.ID, IntentID: uuid.NewString(), CohortID: uuid.NewString(), ListingID: listing.ID,
		CounterpartyID: listing.CreatorCompanyID, PortID: port.ID, CommodityID: listing.CommodityID, LotTons: q,
		StrikePricePence: listing.StrikePricePence, MarginPence: notional / 4, Status: "intent", CreatedAt: lower, MaturesAt: listing.MaturesAt, Strategy: "covered-sale"}
	intent.ReservedCashPence = intent.MarginPence
	if held < q {
		return nil, nil
	}
	intent.Side, intent.ReservedTons = "ask", q
	input := r.planningInput(ShipState{})
	arrivalValue, _, _, source, reason, _ := destinationRevenue(input, port.ID, listing.CommodityID, q, listing.MaturesAt)
	spot, err := WalkDepth(entry.Depth.Bids, q, false)
	if err != nil || reason != "" {
		return nil, nil
	}
	admission.ModelSource = source + "; conservative carry and credit allowance"
	admission.SpotAlternative = max(spot.GrossPence, arrivalValue)
	admission.ExpectedNetPence = notional - admission.SpotAlternative - rent - carry - loss
	intent.ExposurePence = intent.ReservedCashPence + admission.SpotAlternative
	if !r.reserveFuturesLease(&intent, &admission, rows, 0) {
		return nil, nil
	}
	if admission.ExpectedNetPence < futuresEdge(admission.SpotAlternative) || intent.ExposurePence > budget || intent.ExposurePence > store.FuturesExposureLimitPence {
		return nil, nil
	}
	if !futuresCounterpartyFits(rows, intent.CounterpartyID, budget, intent.ExposurePence) {
		return nil, nil
	}
	intent.Admission = encode(admission)
	return &intent, nil
}

func (r *Runner) evaluateFuturesCross(ctx context.Context, sale, purchase gameapi.FuturesContract, rows []store.FuturesIntent, budget int64) ([]store.FuturesIntent, error) {
	if !futuresCrossOrdered(sale, purchase, r.stateCache.Clock.Calendar.TimeScale) || sale.StrikePricePence <= purchase.StrikePricePence ||
		!r.futuresForeignListing(sale) || !r.futuresForeignListing(purchase) {
		return nil, nil
	}
	_, upper, ok := r.serverBounds()
	q := int64(sale.LotTons)
	if !ok || q <= 0 || sale.ID == "" || purchase.ID == "" || sale.CreatorCompanyID == "" || purchase.CreatorCompanyID == "" ||
		sale.StrikePricePence <= 0 || purchase.StrikePricePence <= 0 || sale.StrikePricePence > store.FuturesExposureLimitPence/q ||
		!sale.MaturesAt.After(upper.Add(time.Hour/time.Duration(r.stateCache.Clock.Calendar.TimeScale))) ||
		!purchase.MaturesAt.After(upper.Add(time.Hour/time.Duration(r.stateCache.Clock.Calendar.TimeScale))) ||
		maxTime(sale.MaturesAt, purchase.MaturesAt).Sub(upper).Hours()*float64(r.stateCache.Clock.Calendar.TimeScale) > 1440 {
		return nil, nil
	}
	market := r.stateCache.Markets[sale.PortID]
	port, exists := findPort(r.stateCache.Catalog, sale.PortID)
	entry := bookCommodity(market, sale.CommodityID)
	if !exists || !port.IsHub || !r.fresh("books/"+sale.PortID) || !r.fresh("ports/"+sale.PortID) ||
		market.Visibility != "quay" || market.CustomsBPS < 0 || market.CustomsBPS > 10000 || entry == nil || entry.Depth == nil {
		return nil, nil
	}
	if _, err := WalkDepth(entry.Depth.Bids, q, false); err != nil {
		return nil, nil
	}
	if _, err := WalkDepth(entry.Depth.Asks, q, true); err != nil {
		return nil, nil
	}
	for _, row := range rows {
		if row.ListingID == sale.ID || row.ListingID == purchase.ID {
			return nil, nil
		}
	}
	sellValue, buyValue := q*sale.StrikePricePence, q*purchase.StrikePricePence
	sellLoss, sellAt, sellOK := r.futuresCreditLoss(sale.CreatorCompanyID, sellValue)
	buyLoss, buyAt, buyOK := r.futuresCreditLoss(purchase.CreatorCompanyID, buyValue)
	if !sellOK || !buyOK {
		return nil, nil
	}
	days := maxTime(sale.MaturesAt, purchase.MaturesAt).Sub(upper).Hours() * float64(r.stateCache.Clock.Calendar.TimeScale) / 24
	rent, carry := futuresCarry(sellValue+buyValue, q, days)
	profit := sellValue - buyValue - ceilBPS(buyValue, market.CustomsBPS) - rent - carry - sellLoss - buyLoss
	if profit < futuresEdge(buyValue) {
		return nil, nil
	}
	cohort := uuid.NewString()
	result := make([]store.FuturesIntent, 0, 2)
	for i, listing := range []gameapi.FuturesContract{sale, purchase} {
		notional := q * listing.StrikePricePence
		intent := store.FuturesIntent{AccountID: r.account.ID, IntentID: uuid.NewString(), CohortID: cohort, PortID: listing.PortID, CommodityID: listing.CommodityID,
			LotTons: q, StrikePricePence: listing.StrikePricePence, MarginPence: notional / 4, ReservedCashPence: notional / 4, Status: "intent", CreatedAt: upper, MaturesAt: listing.MaturesAt,
			ListingID: listing.ID, CounterpartyID: listing.CreatorCompanyID, Strategy: "cross-pair"}
		admission := futuresAdmission{ModelSource: "atomic purchase-before-sale spread; conservative carry and credit allowance", SpotAlternative: buyValue, RentPence: rent,
			FundingPence: carry, CreditLossPence: sellLoss + buyLoss, ExpectedNetPence: profit, BookBudgetPence: budget, BindingConstraint: "leased receiving capacity and counterparty concentration", Listing: &listing}
		if i == 0 {
			intent.Side, intent.ReservedTons = "ask", q
			admission.CreditObservedAt = sellAt
			admission.RentPence = 0
		} else {
			intent.Side = "bid"
			intent.ReservedCashPence += buyValue + ceilBPS(buyValue, market.CustomsBPS)
			admission.CreditObservedAt = buyAt
		}
		intent.ExposurePence = intent.ReservedCashPence
		receiving := int64(0)
		if intent.Side == "bid" {
			receiving = q
		}
		if !r.reserveFuturesLease(&intent, &admission, rows, receiving) {
			return nil, nil
		}
		if !futuresCounterpartyFits(rows, intent.CounterpartyID, budget, intent.ExposurePence) {
			return nil, nil
		}
		intent.Admission = encode(admission)
		result = append(result, intent)
	}
	if result[0].ExposurePence+result[1].ExposurePence > budget {
		return nil, nil
	}
	return result, nil
}

func futuresCounterpartyFits(rows []store.FuturesIntent, company string, budget, additional int64) bool {
	byCompany := make(map[string]int64)
	var unknown int64
	for _, row := range rows {
		if store.FuturesTerminal(row.Status) {
			continue
		}
		if row.CounterpartyID == "" {
			unknown += row.ExposurePence
		} else {
			byCompany[row.CounterpartyID] += row.ExposurePence
		}
	}
	if company != "" {
		return unknown+byCompany[company]+additional <= budget/4
	}
	largest := int64(0)
	for _, exposure := range byCompany {
		largest = max(largest, exposure)
	}
	return unknown+largest+additional <= budget/4
}

func (r *Runner) stageFuturesAdmission(ctx context.Context, mission *store.Mission, data *missionData, rows []store.FuturesIntent) (bool, error) {
	stage, _, _ := futuresAdmissionStage(rows)
	actionID := ActionID(r.account.ID, mission.Generation, stage, data.Sequence)
	intents := make([]*store.FuturesIntent, 0, len(rows))
	for i := range rows {
		rows[i].ActionID = actionID
		intents = append(intents, &rows[i])
	}
	if err := r.db.PutFuturesIntents(ctx, intents); err != nil {
		return false, err
	}
	data.Futures.IntentID = rows[0].IntentID
	data.Futures.PortID, data.Futures.CommodityID = rows[0].PortID, rows[0].CommodityID
	mission.Phase = "committed"
	return r.stageFuturesCapacity(ctx, mission, data, rows)
}

func (r *Runner) driveFuturesController(ctx context.Context, mission *store.Mission, data *missionData, rows []store.FuturesIntent) (bool, error) {
	if changed, err := r.releaseFuturesLease(ctx, mission, data, rows); changed || err != nil {
		return changed, err
	}
	contracts, err := r.futuresContracts()
	if err != nil {
		return false, err
	}
	for _, row := range rows {
		if row.Status != "intent" {
			continue
		}
		if _, _, err := r.futuresBookBudgets(ctx, rows); err != nil {
			if errors.Is(err, errFuturesPrecondition) {
				return false, r.holdExpected(ctx, "__futures", err.Error())
			}
			return false, err
		}
		data.Futures.IntentID = row.IntentID
		group, err := r.futuresActionIntents(ctx, data)
		if err != nil {
			return false, err
		}
		data.Futures.PortID, data.Futures.CommodityID = row.PortID, row.CommodityID
		return r.stageFuturesCapacity(ctx, mission, data, group)
	}
	candidates := make([]gameapi.FuturesContract, 0, len(contracts))
	for _, listing := range contracts {
		if !r.futuresForeignListing(listing) {
			continue
		}
		used := false
		for _, row := range rows {
			used = used || row.ListingID == listing.ID
		}
		if !used {
			candidates = append(candidates, listing)
		}
	}
	if len(candidates) == 0 {
		return false, nil
	}
	if err := r.refreshFuturesCounterparties(ctx, candidates); err != nil {
		return false, err
	}
	budgets, _, err := r.futuresBookBudgets(ctx, rows)
	if err != nil {
		if errors.Is(err, errFuturesPrecondition) {
			return false, r.holdExpected(ctx, "__futures", err.Error())
		}
		return false, err
	}
	cover := make(map[string]int64)
	for _, sale := range candidates {
		if sale.Side != "bid" {
			continue
		}
		port, _ := findPort(r.stateCache.Catalog, sale.PortID)
		budget := budgets[port.BankingRegion]
		key := sale.PortID + "/" + sale.CommodityID
		held, exists := cover[key]
		if !exists {
			held = r.futuresCover(rows, sale.PortID, sale.CommodityID, data.Futures)
			cover[key] = held
		}
		for _, purchase := range candidates {
			if !futuresCrossOrdered(sale, purchase, r.stateCache.Clock.Calendar.TimeScale) {
				continue
			}
			pair, err := r.evaluateFuturesCross(ctx, sale, purchase, rows, budget)
			if err != nil {
				return false, err
			}
			if len(pair) > 0 {
				return r.stageFuturesAdmission(ctx, mission, data, pair)
			}
		}
		intent, err := r.evaluateFuturesListing(ctx, sale, rows, budget, held)
		if err != nil {
			return false, err
		}
		if intent != nil {
			return r.stageFuturesAdmission(ctx, mission, data, []store.FuturesIntent{*intent})
		}
	}
	return false, nil
}

func (r *Runner) futuresAdmissionPreflight(ctx context.Context, data *missionData, leasing bool) error {
	if !futuresAdmissionStageName(data.Action.Stage) {
		return fmt.Errorf("%w: unknown futures action", errFuturesTerms)
	}
	rows, err := r.futuresActionIntents(ctx, data)
	if err != nil {
		return err
	}
	if len(rows) == 0 {
		return fmt.Errorf("%w: admission has no immutable intents", errFuturesTerms)
	}
	stage, path, body := futuresAdmissionStage(rows)
	if data.Action.Stage != stage || data.Action.Path != path || data.Action.Method != http.MethodPost ||
		!jsonEqual(encode(body), data.Action.Body) {
		return fmt.Errorf("%w: request changed immutable admission", errFuturesTerms)
	}
	if !leasing {
		if err := r.futuresCapacityReady(ctx, rows); err != nil {
			return err
		}
	}
	all, err := r.db.FuturesIntents(ctx, r.account.ID)
	if err != nil {
		return err
	}
	lower, upper, ok := r.serverBounds()
	if !ok {
		return fmt.Errorf("%w: no clock authority", errFuturesPrecondition)
	}
	excluded := make([]store.FuturesIntent, 0, len(all))
	for _, candidate := range all {
		if candidate.ActionID != data.Action.ID {
			excluded = append(excluded, candidate)
		}
		if candidate.Status == "active" && candidate.ObservedMaturesAt != nil && !upper.Before(*candidate.ObservedMaturesAt) {
			return fmt.Errorf("%w: mature physical obligations take priority", errFuturesPrecondition)
		}
	}
	budgets, free, err := r.futuresBookBudgets(ctx, all)
	if err != nil {
		return err
	}
	port, _ := findPort(r.stateCache.Catalog, rows[0].PortID)
	budget := budgets[port.BankingRegion]
	held := r.futuresCover(excluded, rows[0].PortID, rows[0].CommodityID, data.Futures)
	listings := make([]gameapi.FuturesContract, 0, len(rows))
	for _, row := range rows {
		budget += row.ExposurePence
		if row.ActionID != data.Action.ID || row.Status != "intent" || !lower.Before(row.MaturesAt) {
			return fmt.Errorf("%w: admission expired or changed", errFuturesTerms)
		}
		if !r.fresh("books/"+row.PortID) || !r.fresh("ports/"+row.PortID) {
			return fmt.Errorf("%w: admission lost local price authority", errFuturesPrecondition)
		}
		if row.CounterpartyID != "" {
			meta := r.meta["futures_credit/"+row.CounterpartyID]
			if !meta.Complete || meta.Stale || meta.Error != "" || time.Since(meta.UpdatedAt) > 6*time.Hour {
				return fmt.Errorf("%w: admission awaits current counterparty credit", errFuturesPrecondition)
			}
		}
		if row.ListingID == "" {
			return fmt.Errorf("%w: owned quotes are retired", errFuturesTerms)
		}
		listing, exists := r.listedFuture(row.ListingID)
		if !exists || listing.Status != "open" || listing.PortID != row.PortID || listing.CommodityID != row.CommodityID ||
			listing.StrikePricePence != row.StrikePricePence || int64(listing.LotTons) != row.LotTons ||
			listing.CreatorCompanyID != row.CounterpartyID || !listing.MaturesAt.Equal(row.MaturesAt) {
			return fmt.Errorf("%w: listing no longer matches admitted terms", errFuturesTerms)
		}
		listings = append(listings, listing)
	}
	var evaluated []store.FuturesIntent
	if len(rows) == 2 && len(listings) == 2 {
		evaluated, err = r.evaluateFuturesCross(ctx, listings[0], listings[1], excluded, budget)
	} else if len(rows) == 1 && len(listings) == 1 {
		var proposed *store.FuturesIntent
		proposed, err = r.evaluateFuturesListing(ctx, listings[0], excluded, budget, held)
		if proposed != nil {
			evaluated = []store.FuturesIntent{*proposed}
		}
	}
	if err != nil {
		return err
	}
	if len(evaluated) != len(rows) {
		return fmt.Errorf("%w: fresh warehouse cover, credit or EV admission no longer passes", errFuturesTerms)
	}
	for i, fresh := range evaluated {
		if fresh.ReservedCashPence > rows[i].ReservedCashPence || fresh.ExposurePence > rows[i].ExposurePence {
			return fmt.Errorf("%w: fresh costs changed immutable reservations", errFuturesTerms)
		}
	}
	if free[port.BankingRegion] < 0 {
		return fmt.Errorf("%w: regional reserves no longer fit", errFuturesPrecondition)
	}
	return nil
}

func jsonEqual(a, b []byte) bool {
	var left, right any
	if json.Unmarshal(a, &left) != nil || json.Unmarshal(b, &right) != nil {
		return false
	}
	leftRaw, _ := json.Marshal(left)
	rightRaw, _ := json.Marshal(right)
	return string(leftRaw) == string(rightRaw)
}
