package engine

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"slices"
	"strings"

	"github.com/DevYukine/go-tradewinds/internal/store"
)

type voyageFundingQuote struct {
	plan      Plan
	transfers []observedRemittance
}

func evaluatePlan(in PlanningInput, hull Hull, origin Port, plan Plan, revenue, extraTons int64) Plan {
	return quoteVoyage(in, hull, origin, plan, revenue, extraTons).plan
}

func quoteVoyage(in PlanningInput, hull Hull, origin Port, plan Plan, revenue, extraTons int64) voyageFundingQuote {
	return fundVoyage(in, pricePlan(in, hull, origin, plan, revenue, extraTons))
}

func fundVoyage(in PlanningInput, plan Plan) voyageFundingQuote {
	quote := voyageFundingQuote{plan: plan}
	if !quote.plan.Accepted {
		return quote
	}
	required := cargoFundingRequired(in, quote.plan)
	locallyFunded := true
	for region, amount := range required {
		if amount < 0 || amount == math.MaxInt64 {
			quote.plan = reject(quote.plan, "shipment cash requirements exceed checked integer-pence bounds")
			return quote
		}
		locallyFunded = locallyFunded && in.Balances[region] >= amount
	}
	if locallyFunded {
		return quote
	}
	_, fees, transfers, funded := measuredFunding(in.Balances, required, in.Rates)
	if !funded || len(transfers) > 0 && !in.fresh("rates") {
		quote.plan = reject(quote.plan, "shipment cannot fund trade, safe stores, fleet bills and destination reserves with fresh observed fees")
		return quote
	}
	quote.transfers = transfers
	quote.plan = addFundingFees(quote.plan, fees)
	return quote
}

// Funding principal stays cash. Actual paid fees remain in the economics, but
// only prospective fees can decide whether a funded commitment should continue.
func addFundingFees(plan Plan, fees int64) Plan {
	if fees < 0 || fees == math.MaxInt64 || plan.Economics.FundingFeesPence > math.MaxInt64-fees ||
		plan.Economics.StressNetPence < math.MinInt64+fees ||
		plan.Economics.BallastRepeatGameDays > 0 && plan.Economics.BallastRepeatNetPence < math.MinInt64+fees {
		return reject(plan, "funding fees exceed checked integer-pence bounds")
	}
	plan.Economics.FundingFeesPence += fees
	plan.Economics.ExpectedNetPence -= fees
	plan.Economics.StressNetPence -= fees
	if plan.Economics.BallastRepeatGameDays > 0 {
		plan.Economics.BallastRepeatNetPence -= fees
	}
	plan.Economics.NetPencePerGameDay = float64(plan.Economics.ExpectedNetPence) / plan.Economics.GameDays
	remainingNet := plan.Economics.ExpectedNetPence + plan.sunkFundingFeesPence
	if remainingNet < 0 || remainingNet == 0 && plan.sunkFundingFeesPence == 0 {
		return reject(plan, "non-positive destination contribution after measured funding fees")
	}
	if plan.playerBidStress && remainingNet-plan.Economics.PriceUncertaintyStressPence <= 0 {
		return reject(plan, "player bid lot is not net-positive at the stressed gossip NPC bid after funding fees")
	}
	if plan.Economics.RevenueBasis == "retained" && remainingNet-plan.Economics.PriceUncertaintyStressPence <= 0 {
		return reject(plan, "retained quay book lot is not net-positive after price uncertainty stress and funding fees")
	}
	if reason := minimumMarginRejection(plan); reason != "" {
		return reject(plan, reason)
	}
	return plan
}

func checkedPenceProduct(quantity, price int64) int64 {
	if quantity < 0 || price < 0 || quantity > 0 && price > math.MaxInt64/quantity {
		return math.MaxInt64
	}
	return quantity * price
}

func iocCash(quantity, limit, customsBPS int64) int64 {
	if quantity < 0 || customsBPS < 0 || quantity > 0 && limit <= 0 {
		return math.MaxInt64
	}
	if quantity == 0 {
		return 0
	}
	purchase := checkedPenceProduct(quantity, limit)
	customs := growthCashTotal(
		checkedPenceProduct(purchase/10_000, customsBPS),
		checkedPenceProduct(purchase%10_000, customsBPS/10_000),
		ceilBPS(purchase%10_000, customsBPS%10_000),
	)
	// Each whole-ton fill may round its buyer customs separately.
	return growthCashTotal(purchase, customs, quantity)
}

func voyageFleetBills(in PlanningInput, plan Plan) int64 {
	bills := math.Ceil(float64(in.FleetDailyUpkeepPence) * plan.Economics.GameDays)
	if in.FleetDailyUpkeepPence < 0 || plan.Economics.GameDays <= 0 ||
		math.IsNaN(bills) || math.IsInf(bills, 0) || bills < 0 || bills >= float64(math.MaxInt64) {
		return math.MaxInt64
	}
	return int64(bills)
}

// Quotation, staged funding and first-send procurement use the same immutable
// IOC bounds. MaxInt64 is the existing fail-closed cash-overflow sentinel.
func cargoFundingRequired(in PlanningInput, plan Plan) map[string]int64 {
	required := clonePence(in.ReserveByRegion)
	origin, originOK := findPort(in.Catalog, plan.Origin)
	destination, destinationOK := findPort(in.Catalog, plan.Destination)
	if !originOK || !destinationOK || origin.BankingRegion == "" || destination.BankingRegion == "" {
		required[""] = math.MaxInt64
		return required
	}
	var purchase int64
	if plan.Kind != "freight" {
		purchase = iocCash(plan.Quantity, plan.BuyLimitPence, in.Markets[plan.Origin].CustomsBPS)
	}
	stores := iocCash(plan.StoresToBuy, plan.StoresLimitPence, in.Markets[plan.Origin].CustomsBPS)
	required[origin.BankingRegion] = growthCashTotal(required[origin.BankingRegion], purchase, stores,
		plan.Economics.BerthDuesPence, plan.Economics.UpkeepPence, plan.Economics.CollateralPence,
		plan.Economics.TollPence, voyageFleetBills(in, plan))
	if destination.BankingRegion != origin.BankingRegion {
		required[destination.BankingRegion] = growthCashTotal(
			max(in.ReservePence, required[destination.BankingRegion]),
			in.Ports[plan.Destination].BerthDuesPerDayPence,
		)
	}
	return required
}

// Only unstarted ordinary income is conditional. Acquired cargo, accepted seals
// and attempted requests keep their existing forward-recovery/replay semantics.
func ordinaryIncomePending(data *missionData) bool {
	if data.Relocation != nil || data.SourcingHandoff != nil || data.GrowthLane != nil ||
		data.ReturnJourney || data.BuyDone || data.BoughtQuantity != 0 || data.AcquisitionPence != 0 ||
		len(data.Freight) != 0 || data.Settlement != nil {
		return false
	}
	switch data.Plan.Kind {
	case "charter", "spot", "freight", "charter+freight", "spot+freight":
		return true
	default:
		return false
	}
}

func quoteIncomePlan(in PlanningInput, pinned Plan, paidFees int64) voyageFundingQuote {
	return fundVoyage(in, priceIncomePlan(in, pinned, paidFees))
}

func priceIncomePlan(in PlanningInput, pinned Plan, paidFees int64) Plan {
	rejected := func(reason string) Plan {
		return reject(pinned, reason)
	}
	for _, key := range []string{"catalog", "clock", "zones", "vaults", "contracts", "ships"} {
		if !in.fresh(key) {
			return rejected("income opportunity requires current successful " + key + " authority")
		}
	}
	if in.Clock.Paused || in.Clock.Calendar.TimeScale <= 0 || in.Clock.Calendar.Month < 1 || in.Clock.Calendar.Month > 12 ||
		in.Clock.Calendar.Day < 1 || in.Clock.Calendar.Day > 30 || in.Ship.Voyage != nil || in.Ship.Ship.ConvoyID != "" ||
		in.Ship.Ship.Status != "berthed" && in.Ship.Ship.Status != "anchored_roadstead" || !emptyCargo(in.Ship) ||
		pinned.ShipID != in.Ship.Ship.ID || pinned.Origin != in.Ship.Ship.CurrentPortID || pinned.Quantity <= 0 {
		return rejected("income opportunity no longer matches its fresh empty independent ship, source or clock")
	}
	hull, hullOK := findHull(in.Catalog, in.Ship.Ship.ShipClass)
	origin, originOK := findPort(in.Catalog, pinned.Origin)
	if !hullOK || !originOK {
		return rejected("income opportunity lacks its current hull or source harbour")
	}
	in.Markets = withoutOwnOrders(in.Markets, in.OwnedOrderIDs)
	plan := Plan{Kind: pinned.Kind, ShipID: pinned.ShipID, Origin: pinned.Origin, Destination: pinned.Destination,
		Commodity: pinned.Commodity, ContractID: pinned.ContractID, FreightIDs: slices.Clone(pinned.FreightIDs),
		Quantity: pinned.Quantity, TargetStoresTons: pinned.TargetStoresTons, sunkFundingFeesPence: paidFees}
	var revenue, extraTons int64
	if strings.HasPrefix(plan.Kind, "charter") {
		found := false
		for _, contract := range in.Contracts {
			if contract.ID == plan.ContractID && contract.ContractType == "charter" && contract.Status == "open" && !contract.Stale &&
				contract.OriginPortID == plan.Destination && contract.CommodityID == plan.Commodity && contract.RemainingQuantity >= plan.Quantity &&
				contract.RewardPerTonPence != nil && *contract.RewardPerTonPence > 0 {
				revenue = checkedPenceProduct(plan.Quantity, *contract.RewardPerTonPence)
				plan.Deadline = contract.ExpiresAt
				found = true
				break
			}
		}
		if !found {
			return rejected("pinned charter no longer has fresh matching uncommitted demand")
		}
	} else if strings.HasPrefix(plan.Kind, "spot") {
		var reason string
		revenue, plan.SellLimitPence, plan.Economics.PriceUncertaintyStressPence, plan.PriceSource, reason = destinationRevenue(in, plan.Destination, plan.Commodity, plan.Quantity)
		if reason != "" {
			return rejected(reason)
		}
	} else if plan.Kind != "freight" {
		return rejected("pinned income kind is not an ordinary shipment")
	}
	for i, id := range plan.FreightIDs {
		if slices.Contains(plan.FreightIDs[:i], id) {
			return rejected("pinned freight manifest repeats an indivisible contract")
		}
		found := false
		for _, contract := range in.Contracts {
			if contract.ID != id || contract.ContractType != "freight" || contract.Status != "open" || contract.Stale ||
				contract.IssuerCompanyID == in.CompanyID || contract.OriginPortID != plan.Origin || contract.DestPortID != plan.Destination ||
				contract.QuantityRequired <= 0 || contract.CollateralPence < 0 || contract.RewardPence < 0 ||
				!in.deadlineOpen(contract.ExpiresAt) {
				continue
			}
			if plan.Kind == "freight" && (plan.ContractID != id || plan.Quantity != contract.QuantityRequired || plan.Commodity != contract.CommodityID) {
				return rejected("pinned indivisible freight terms changed")
			}
			revenue = growthCashTotal(revenue, contract.RewardPence)
			plan.Economics.CollateralPence = growthCashTotal(plan.Economics.CollateralPence, contract.CollateralPence)
			if plan.Kind != "freight" {
				extraTons = growthCashTotal(extraTons, contract.QuantityRequired)
			}
			found = true
			break
		}
		if !found {
			return rejected("pinned freight no longer has fresh unreserved matching terms")
		}
	}
	if plan.Kind == "freight" && len(plan.FreightIDs) != 1 || revenue == math.MaxInt64 || extraTons == math.MaxInt64 {
		return rejected("pinned income manifest exceeds checked terms or integer-pence bounds")
	}
	// Procurement is an existing commitment, not a fresh hazard-priced voyage.
	// Keep its admission charge, including zero for plans made before HZ.
	in.pinnedHazard = &pinned.Economics
	plan = pricePlan(in, hull, origin, plan, revenue, extraTons)
	if plan.Accepted {
		plan = addFundingFees(plan, paidFees)
	}
	if pinned.PriceSource != "" {
		// Current receipts can improve without resizing the original probe or
		// relabeling the evidence under which its fixed quantity was admitted.
		plan.PriceSource = pinned.PriceSource
		if pinned.PriceSource != "live contract terms" && pinned.PriceSource != "observed destination full depth" {
			plan.Economics.BallastRepeatGameDays = 0
			plan.Economics.BallastRepeatNetPence = 0
		}
	}
	return plan
}

func (r *Runner) incomeProcurementQuote(ctx context.Context, ship ShipState, data *missionData) (voyageFundingQuote, error) {
	if err := r.insuranceActionReady(ship, "buy", data.Plan.Destination); err != nil {
		return voyageFundingQuote{plan: reject(data.Plan, err.Error())}, nil
	}
	input, err := r.committedPlanningInput(ctx, ship)
	if err != nil {
		return voyageFundingQuote{}, err
	}
	// FeesPence contains actual funding receipts, not transferred principal
	// or a modeled estimate. Repricing records them without charging them again.
	quote := quoteIncomePlan(input, data.Plan, data.FeesPence)
	return quote, nil
}

func (r *Runner) refreshIncomeProcurement(ctx context.Context, ship ShipState, mission *store.Mission, data *missionData) (bool, bool, error) {
	if data.FeesPence == 0 {
		if err := r.insuranceActionReady(ship, "buy", data.Plan.Destination); err != nil {
			if changed, maintenanceErr := r.maintain(ctx, ship, mission, data); changed || maintenanceErr != nil {
				return false, changed, maintenanceErr
			}
			return false, false, r.hold(ctx, ship.Ship.ID, err.Error())
		}
	}
	quote, err := r.incomeProcurementQuote(ctx, ship, data)
	if err != nil {
		return false, false, err
	}
	if !quote.plan.Accepted {
		changed, err := r.unwindProcurement(ctx, ship, mission, data, quote.plan.Reason)
		return false, changed, err
	}
	data.Plan = quote.plan
	mission.Quantity = quote.plan.Quantity
	mission.PricePence = quote.plan.BuyLimitPence
	if len(quote.transfers) > 0 {
		transfer := quote.transfers[0]
		return false, true, r.stage(ctx, mission, data, ship, "remit", "/v1/treasury/remit", map[string]any{
			"from_region": transfer.Rate.FromRegion, "to_region": transfer.Rate.ToRegion, "gross_amount_pence": transfer.Gross,
		})
	}
	return true, false, nil
}

// Called for proven first sends only, both before journaling and after the
// limiter wait. A changed quotation can release an unsent request, never a replay.
func (r *Runner) incomeActionReady(ctx context.Context, ship ShipState, data *missionData, action *durableAction) error {
	if !ordinaryIncomePending(data) || action == nil ||
		action.Stage != "remit" && action.Stage != "buy" && action.Stage != "accept-freight" &&
			action.Stage != "provision" && action.Stage != "dock" {
		return nil
	}
	var request struct {
		ShipID     string `json:"ship_id"`
		PortID     string `json:"port_id"`
		Commodity  string `json:"commodity_id"`
		Quantity   int64  `json:"quantity"`
		Price      int64  `json:"price_pence"`
		IntoStores bool   `json:"into_stores"`
		From       string `json:"from_region"`
		To         string `json:"to_region"`
		Gross      int64  `json:"gross_amount_pence"`
		ContractID string `json:"contract_id"`
	}
	if err := json.Unmarshal(action.Body, &request); err != nil {
		return errors.New("new income action has an invalid immutable request")
	}
	if action.Stage == "dock" && (action.MaintenanceDock || !r.loanAuthority()) {
		// Debt reads cannot disable a safety berth or collection of owned cargo.
		return nil
	}
	if action.Stage == "provision" || action.Stage == "dock" {
		// An attached future plan cannot disable independent safety stores or
		// the berth needed for them, nor recovery of already-owned obligations.
		if !emptyCargo(ship) || !emptyCargo(action.Before) {
			return nil
		}
		for _, contract := range r.stateCache.Contracts {
			if contract.ContractType == "freight" && contract.Status == "active" &&
				contract.CarrierCompanyID == r.account.CompanyID && contract.CarrierShipID == ship.Ship.ID {
				return nil
			}
		}
		if action.Stage == "provision" {
			hull, exists := findHull(r.stateCache.Catalog, ship.Ship.ShipClass)
			if !exists {
				return errors.New("new voyage preparation lacks its observed hull safety requirements")
			}
			baseline := 1 + float64(hull.CrewRequired)/200*2
			baselineQuantity := max(0, int64(math.Ceil(baseline-action.Before.Stores.ProvisionsTons)))
			voyageQuantity := max(0, int64(math.Ceil(data.Plan.TargetStoresTons-action.Before.Stores.ProvisionsTons)))
			if !request.IntoStores || request.Quantity != voyageQuantity || request.Quantity <= baselineQuantity {
				return nil
			}
		}
	}
	if err := r.insuranceActionReady(ship, "buy", data.Plan.Destination); err != nil {
		return err
	}
	input, err := r.committedPlanningInput(ctx, ship)
	if err != nil {
		return err
	}
	plan := priceIncomePlan(input, data.Plan, data.FeesPence)
	if !plan.Accepted {
		return fmt.Errorf("new income action lost its pinned opportunity: %s", plan.Reason)
	}
	if action.Stage == "remit" {
		if !input.fresh("rates") || request.Gross <= 0 || request.Gross > maximumPence ||
			request.From == "" || request.To == "" || request.From == request.To {
			return errors.New("new income remittance lacks a valid immutable transfer and fresh observed fee")
		}
		var rate Rate
		found := false
		for _, candidate := range input.Rates {
			if candidate.FromRegion == request.From && candidate.ToRegion == request.To && candidate.FeeBPS >= 0 && candidate.FeeBPS < 10_000 {
				rate, found = candidate, true
				break
			}
		}
		if !found {
			return errors.New("new income remittance lost its observed donor route")
		}
		required := cargoFundingRequired(input, plan)
		for _, amount := range required {
			if amount < 0 || amount == math.MaxInt64 {
				return errors.New("new income remittance exceeds checked shipment cash requirements")
			}
		}
		donor, recipient := input.Balances[request.From], input.Balances[request.To]
		if donor < request.Gross || donor-request.Gross < required[request.From] ||
			required[request.To] <= recipient || recipient < 0 && required[request.To] > math.MaxInt64+recipient {
			return errors.New("new income remittance is no longer needed or would spend protected donor cash")
		}
		needed, err := GrossRemittance(required[request.To]-recipient, rate.FeeBPS)
		if err != nil || request.Gross > needed {
			return errors.New("new income remittance exceeds the current opportunity's funding deficit")
		}
		fee := request.Gross/10_000*rate.FeeBPS + request.Gross%10_000*rate.FeeBPS/10_000
		net := request.Gross - fee
		if recipient > math.MaxInt64-net {
			return errors.New("new income remittance exceeds checked recipient cash bounds")
		}
		// A staged request may remain a useful partial transfer as berth dues
		// accrue. Price its actual fee first, then fund the remaining deficits;
		// a differently split transfer must not inherit the old fee estimate.
		projected := clonePence(input.Balances)
		projected[request.From] -= request.Gross
		projected[request.To] += net
		_, remainingFees, _, funded := measuredFunding(projected, required, input.Rates)
		if !funded {
			return errors.New("new income remittance no longer leaves a fully fundable reserve-safe shipment")
		}
		plan = addFundingFees(plan, growthCashTotal(fee, remainingFees))
		if !plan.Accepted {
			return fmt.Errorf("new income remittance lost positive contribution after actual and remaining funding fees: %s", plan.Reason)
		}
		return nil
	}
	quote := fundVoyage(input, plan)
	if !quote.plan.Accepted {
		return fmt.Errorf("new income action lost its funded opportunity: %s", quote.plan.Reason)
	}
	if len(quote.transfers) != 0 || request.ShipID != ship.Ship.ID {
		return errors.New("new income procurement must await its actual ship-owned regional funding")
	}
	switch action.Stage {
	case "buy":
		if request.PortID != quote.plan.Origin || request.Commodity != quote.plan.Commodity ||
			request.Quantity != quote.plan.Quantity || request.Price != quote.plan.BuyLimitPence {
			return errors.New("new income cargo request no longer matches its fresh pinned funded quote")
		}
	case "provision":
		if request.PortID != quote.plan.Origin || request.Commodity != "provisions" ||
			request.Quantity != quote.plan.StoresToBuy || request.Price != quote.plan.StoresLimitPence {
			return errors.New("new voyage stores no longer match the fresh pinned funded quote")
		}
	case "accept-freight":
		if !slices.Contains(quote.plan.FreightIDs, request.ContractID) {
			return errors.New("new income freight acceptance lost its reserved indivisible lot")
		}
	}
	return nil
}
