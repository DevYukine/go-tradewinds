package engine

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"math"

	"github.com/DevYukine/go-tradewinds/internal/store"
	"github.com/google/uuid"
)

// The admission belongs to the new hull, not to the incumbent-inclusive cash
// protected while it was built. NextPlan reserves only a selected future lot.
type growthLaneIntent struct {
	AdmissionID                  string `json:"admission_id"`
	QuantityLimit                int64  `json:"quantity_limit"`
	InitialProcurementLimitPence int64  `json:"initial_procurement_limit_pence"`
	InitialProcurementComplete   bool   `json:"initial_procurement_complete"`
	NextPlan                     *Plan  `json:"next_plan,omitempty"`
}

func (lane *growthLaneIntent) withdrawn() bool {
	return lane != nil && lane.QuantityLimit == 0 && lane.InitialProcurementLimitPence == 0 &&
		lane.InitialProcurementComplete && lane.NextPlan == nil
}

func growthHandoffGeneration(generation, shipID string) string {
	identity := fmt.Sprintf("growth-handoff:%d:%s:%d:%s", len(generation), generation, len(shipID), shipID)
	return uuid.NewSHA1(uuid.NameSpaceOID, []byte(identity)).String()
}

func growthCargoCash(plan Plan, customsBPS int64) int64 {
	return iocCash(plan.Quantity, plan.BuyLimitPence, customsBPS)
}

// reserveGrowthBook is shared by capital forecasts and owned-hull quotes. All
// depletion is private: a quote cannot spend the authoritative book observation.
func reserveGrowthBook(book Market, commodity string, asks, bids, stores int64) Market {
	if asks == 0 && bids == 0 && stores == 0 {
		return book
	}
	book.Books = append([]CommodityBook(nil), book.Books...)
	for i, entry := range book.Books {
		if entry.Depth == nil {
			continue
		}
		var reservedAsks, reservedBids int64
		if entry.CommodityID == commodity {
			reservedAsks = asks
			reservedBids = bids
		}
		if entry.CommodityID == "provisions" {
			reservedAsks = growthCashTotal(reservedAsks, stores)
		}
		if reservedAsks == 0 && reservedBids == 0 {
			continue
		}
		depth := *entry.Depth
		depth.Asks = depleteDepth(depth.Asks, reservedAsks, true)
		depth.Bids = depleteDepth(depth.Bids, reservedBids, false)
		book.Books[i].Depth = &depth
	}
	return book
}

func (r *Runner) growthSourceAuthority(input PlanningInput, portID string) bool {
	if !input.fresh("books/"+portID) || input.Markets[portID].Visibility != "quay" {
		return false
	}
	if input.fresh("ships") {
		for _, ship := range r.stateCache.Ships {
			if ship.Ship.CurrentPortID == portID && ship.Voyage == nil &&
				(ship.Ship.Status == "berthed" || ship.Ship.Status == "anchored_roadstead") {
				return true
			}
		}
	}
	if input.fresh("factors") {
		for _, factorPort := range r.stateCache.Factors {
			if factorPort == portID {
				return true
			}
		}
	}
	return false
}

// Only unpaid cash is committed. Existing cargo, hardware and completed store
// orders do not reserve their original purchase price a second time.
func (r *Runner) reserveMissionCash(required map[string]int64, mission *store.Mission, data *missionData, ship ShipState) {
	if data.Capacity != nil && data.Capacity.Financing != nil && data.BuiltShipID == "" {
		financing := data.Capacity.Financing
		if mission.Phase == "growth_finance_repay" || mission.Phase == "growth_rejected" {
			for _, accepted := range financing.Loans {
				if loan, exists := r.loanByID(accepted.ID); exists {
					required[loan.BankingRegion] = growthCashTotal(required[loan.BankingRegion], max(0, loanFullPayment(loan)-loanReserve(loan)))
				}
			}
		} else {
			for region, amount := range financing.ProtectedByRegion {
				for _, accepted := range financing.Loans {
					if loan, exists := r.loanByID(accepted.ID); exists && loan.BankingRegion == region {
						amount = max(0, amount-loanReserve(loan))
					}
				}
				required[region] = growthCashTotal(required[region], amount, financing.DonorByRegion[region])
			}
		}
		return
	}
	if data.Plan.Kind == "recovery" || mission.Phase == "maintenance_done" || mission.Phase == "done" ||
		data.Plan.Kind == "maintenance" && data.Plan.StoresToBuy == 0 {
		return
	}
	if r.loadingReservationSuspended(mission, data, ship) {
		return
	}
	if data.Relocation != nil {
		if data.Settlement == nil {
			if origin, exists := findPort(r.stateCache.Catalog, data.Plan.Origin); exists {
				required[origin.BankingRegion] = growthCashTotal(required[origin.BankingRegion], growthWorkingCapital(data.Plan))
			}
		}
		return
	}
	reserve := func(plan Plan, unbought bool, stores int64) {
		origin, exists := findPort(r.stateCache.Catalog, plan.Origin)
		if !exists {
			return
		}
		var cash int64
		if unbought && plan.Kind != "freight" && plan.Quantity > 0 {
			cash = growthCargoCash(plan, r.stateCache.Markets[plan.Origin].CustomsBPS)
			if data.GrowthLane != nil && !data.GrowthLane.InitialProcurementComplete {
				cash = max(cash, data.GrowthLane.InitialProcurementLimitPence)
			}
		}
		if stores > 0 {
			limit := plan.StoresLimitPence
			if limit <= 0 {
				entry := bookCommodity(r.stateCache.Markets[plan.Origin], "provisions")
				if entry == nil || entry.Depth == nil {
					limit = math.MaxInt64
				} else if fill, err := WalkDepth(entry.Depth.Asks, stores, true); err != nil {
					limit = math.MaxInt64
				} else {
					limit = fill.LimitPence
				}
			}
			storePlan := Plan{Quantity: stores, BuyLimitPence: limit}
			cash = growthCashTotal(cash, growthCargoCash(storePlan, r.stateCache.Markets[plan.Origin].CustomsBPS))
		}
		collateral := plan.Economics.CollateralPence
		for _, obligation := range data.Freight {
			collateral = max(0, collateral-obligation.CollateralPence)
		}
		cash = growthCashTotal(cash, collateral)
		if unbought {
			cash = growthCashTotal(cash, plan.Economics.BerthDuesPence, plan.Economics.TollPence)
			if ordinaryIncomePending(data) {
				input := PlanningInput{FleetDailyUpkeepPence: DailyUpkeep(r.stateCache.Ships, r.stateCache.Catalog, len(r.stateCache.Factors))}
				cash = growthCashTotal(cash, voyageFleetBills(input, plan))
				destination, known := findPort(r.stateCache.Catalog, plan.Destination)
				if known && destination.BankingRegion != origin.BankingRegion {
					required[destination.BankingRegion] = growthCashTotal(required[destination.BankingRegion],
						r.stateCache.Ports[destination.ID].BerthDuesPerDayPence)
				}
			}
		}
		required[origin.BankingRegion] = growthCashTotal(required[origin.BankingRegion], cash)
	}
	if data.Settlement == nil {
		stores := int64(0)
		if ship.Voyage == nil && (ship.Ship.CurrentPortID == data.Plan.Origin || mission.ShipID == "__growth") {
			stores = max(0, int64(math.Ceil(data.Plan.TargetStoresTons-ship.Stores.ProvisionsTons)))
			if data.Plan.Kind == "maintenance" {
				stores = min(stores, data.Plan.StoresToBuy)
				if data.Action != nil && data.Action.Stage == "provision" {
					stores = data.Plan.StoresToBuy
				}
			}
		}
		unbought := !data.BuyDone && (data.Plan.Kind != "freight" || mission.Phase == "provision" ||
			mission.Phase == "buy" || mission.Phase == "freight")
		plan := data.Plan
		if plan.Kind != "freight" {
			plan.Quantity = max(0, plan.Quantity-data.BoughtQuantity)
			unbought = unbought && plan.Quantity > 0
		}
		reserve(plan, unbought, stores)
	}
	if mission.ShipID == "__growth" {
		if port, exists := findPort(r.stateCache.Catalog, data.BuildPort); exists {
			guns := max(0, data.TargetCannons-ship.Ship.CannonsCount)
			required[port.BankingRegion] = growthCashTotal(required[port.BankingRegion], guns*15_000)
		}
	}
	if data.GrowthLane != nil && data.GrowthLane.NextPlan != nil {
		plan := *data.GrowthLane.NextPlan
		stores := plan.StoresToBuy
		if ship.Ship.CurrentPortID == plan.Origin && ship.Voyage == nil {
			stores = max(0, int64(math.Ceil(plan.TargetStoresTons-ship.Stores.ProvisionsTons)))
		}
		reserve(plan, true, stores)
		// Roadstead closing does not open a source berth. NextPlan already
		// funds its load berth; reserve only return departure and stores here.
		input := r.planningInput(ship)
		if quote, err := growthReturnBasis(input, data.Plan.Origin); err == nil {
			if port, exists := findPort(input.Catalog, ship.Ship.CurrentPortID); exists {
				required[port.BankingRegion] = growthCashTotal(required[port.BankingRegion], quote.storeCash, quote.departureBill)
			}
		}
	}
}

type growthOperatingQuote struct {
	plan         Plan
	required     map[string]int64
	transfers    []observedRemittance
	returnTarget float64
	returnStores int64
	returnLimit  int64
	rate         float64
}

type growthReturnQuote struct {
	days          float64
	consumed      float64
	target        float64
	stores        int64
	storeLimit    int64
	storeCash     int64
	departureBill int64
	expense       int64
}

// This is future B->A work only. The earned first sale and all paid hardware,
// stores and earlier navigation are absent from the continuation comparison.
func growthReturnBasis(input PlanningInput, source string) (growthReturnQuote, error) {
	var quote growthReturnQuote
	ship := input.Ship
	hull, exists := findHull(input.Catalog, ship.Ship.ShipClass)
	if !exists || !emptyCargo(ship) || ship.Voyage != nil || source == ship.Ship.CurrentPortID ||
		(ship.Ship.Status != "berthed" && ship.Ship.Status != "anchored_roadstead") {
		return quote, errors.New("growth return needs an empty, stationary independently navigable hull")
	}
	back, err := observedPlanningRoute(input, ship.Ship.CurrentPortID, source, hull)
	if err != nil {
		return quote, err
	}
	current := ship.Ship.CurrentPortID
	if !input.fresh("ports/"+current) || !input.fresh("ports/"+source) || !input.fresh("books/"+current) ||
		input.Ports[source].EffectiveBerths <= 0 || input.Markets[current].Visibility != "quay" {
		return quote, errors.New("growth return requires fresh current provisions and both harbour costs")
	}
	_, upper, ok := input.serverBounds()
	if !ok {
		return quote, errors.New("growth return requires current server time")
	}
	sailingDays := float64(back.TotalDistanceNM) / (float64(hull.BaseSpeedCentiknots) / 100 * 0.8) / 24
	portDays := 1.0/24 + float64(input.Ports[source].QueuedShips+1)/float64(input.Ports[source].EffectiveBerths)*0.25
	quote.days = sailingDays + portDays
	quote.consumed = float64(hull.CrewRequired)/100*sailingDays + float64(hull.CrewRequired)/200*portDays
	quote.target = max(back.SafeProvisionsTons+float64(hull.CrewRequired)/200*portDays+1, 1+float64(hull.CrewRequired)/200*2)
	quote.stores = max(0, int64(math.Ceil(quote.target-ship.Stores.ProvisionsTons)))
	if quote.stores > ship.EffectiveHoldCapacityTons-ship.CargoUsedTons {
		return quote, errors.New("growth return safe stores exceed actual hold")
	}
	if quote.stores > 0 {
		provisions := bookCommodity(input.Markets[current], "provisions")
		if provisions == nil || provisions.Depth == nil {
			return quote, errors.New("growth return has no current executable provisions")
		}
		fill, err := WalkDepth(provisions.Depth.Asks, quote.stores, true)
		if err != nil {
			return quote, err
		}
		quote.storeLimit = fill.LimitPence
		quote.storeCash = growthCargoCash(Plan{Quantity: quote.stores, BuyLimitPence: fill.LimitPence}, input.Markets[current].CustomsBPS)
	}
	currentDays := 0.0
	if ship.Ship.Status == "berthed" {
		currentDays = max(0.25, portDays/2)
		if ship.Ship.BerthedAt != nil {
			if ship.Ship.BerthedAt.After(upper) {
				return quote, errors.New("growth return has an invalid current berth timestamp")
			}
			currentDays = max(currentDays, upper.Sub(*ship.Ship.BerthedAt).Seconds()*float64(input.Clock.Calendar.TimeScale)/86_400+portDays/2)
		}
	}
	quote.departureBill = int64(math.Ceil(float64(input.Ports[current].BerthDuesPerDayPence) * currentDays))
	provisionExpense, err := provisionsExpense(input.Markets[current], quote.consumed)
	if err != nil {
		return quote, err
	}
	quote.expense = growthCashTotal(provisionExpense, quote.departureBill)
	return quote, nil
}

func quoteGrowthDelivery(input PlanningInput, base Plan, lane *growthLaneIntent, initial bool, back *growthReturnQuote) growthOperatingQuote {
	best := growthOperatingQuote{plan: reject(base, "no positive funded residual operating lot remains")}
	if lane == nil || lane.AdmissionID == "" || lane.QuantityLimit <= 0 || len(base.FreightIDs) != 0 {
		best.plan.Reason = "growth lane lacks its durable allocation"
		return best
	}
	for _, key := range []string{"catalog", "clock", "zones", "vaults", "contracts", "ships", "rates"} {
		if !input.fresh(key) {
			best.plan.Reason = "growth operating quote requires fresh " + key
			return best
		}
	}
	origin, originOK := findPort(input.Catalog, base.Origin)
	hull, hullOK := findHull(input.Catalog, input.Ship.Ship.ShipClass)
	if !originOK || !hullOK || input.Clock.Paused || input.Clock.Calendar.TimeScale <= 0 ||
		!input.fresh("books/"+origin.ID) || input.Markets[origin.ID].Visibility != "quay" {
		best.plan.Reason = "growth operating source or clock authority is unavailable"
		return best
	}
	quoted := input
	quoted.Ship.Ship.CurrentPortID = origin.ID
	quoted.Ship.Ship.Status = "berthed"
	quoted.Ship.Ship.DockQueuedAt = nil
	quoted.Ship.Voyage = nil
	quoted.FleetDailyUpkeepPence = 0
	if initial {
		quoted.pinnedHazard = &base.Economics
	}
	returnCash := int64(0)
	returnRegion := ""
	if back != nil {
		quoted.Ship.Ship.BerthedAt = nil
		quoted.Ship.Stores.ProvisionsTons = max(0, input.Ship.Stores.ProvisionsTons+float64(back.stores)-back.consumed)
		quoted.Ship.EffectiveHoldCapacityTons += int64(math.Ceil(input.Ship.Stores.ProvisionsTons)) -
			int64(math.Ceil(quoted.Ship.Stores.ProvisionsTons))
		current, exists := findPort(input.Catalog, input.Ship.Ship.CurrentPortID)
		if !exists {
			return best
		}
		returnRegion = current.BankingRegion
		returnCash = growthCashTotal(back.storeCash, back.departureBill)
	}
	maximum := min(lane.QuantityLimit, quoted.Ship.EffectiveHoldCapacityTons-quoted.Ship.CargoUsedTons)
	var reward int64
	if base.Kind == "charter" {
		found := false
		for _, contract := range input.Contracts {
			if contract.ID == base.ContractID && contract.ContractType == "charter" && contract.Status == "open" && !contract.Stale &&
				contract.OriginPortID == base.Destination && contract.CommodityID == base.Commodity &&
				contract.RewardPerTonPence != nil && *contract.RewardPerTonPence > 0 {
				maximum = min(maximum, contract.RemainingQuantity)
				reward = *contract.RewardPerTonPence
				base.Deadline = contract.ExpiresAt
				found = true
				break
			}
		}
		if !found {
			best.plan.Reason = "growth charter no longer has fresh uncommitted demand"
			return best
		}
	} else if base.Kind != "spot" || !input.fresh("books/"+base.Destination) || input.Markets[base.Destination].Visibility != "quay" {
		best.plan.Reason = "growth spot delivery requires fresh executable destination demand"
		return best
	}
	// Price once; the complete funded itinerary below owns its transfer fees.
	for quantity := int64(1); quantity <= maximum; quantity++ {
		if reward > 0 && quantity > math.MaxInt64/reward {
			continue
		}
		plan := Plan{Kind: base.Kind, ShipID: input.Ship.Ship.ID, Origin: base.Origin, Destination: base.Destination,
			Commodity: base.Commodity, ContractID: base.ContractID, Deadline: base.Deadline, Quantity: quantity}
		if initial {
			plan.TargetStoresTons = base.TargetStoresTons
		}
		plan = pricePlan(quoted, hull, origin, plan, quantity*reward, 0)
		if !plan.Accepted || initial && growthCargoCash(plan, input.Markets[origin.ID].CustomsBPS) > lane.InitialProcurementLimitPence {
			continue
		}
		if back != nil && plan.Deadline != nil && !plan.ExpectedArrival.Add(gameDuration(back.days+1, input.Clock.Calendar.TimeScale)).Before(*plan.Deadline) {
			continue
		}
		required := cargoFundingRequired(quoted, plan)
		if back != nil {
			required[returnRegion] = growthCashTotal(required[returnRegion], returnCash)
		}
		_, fees, transfers, funded := measuredFunding(input.Balances, required, input.Rates)
		if !funded {
			continue
		}
		plan = addFundingFees(plan, fees)
		if !plan.Accepted {
			continue
		}
		net := plan.Economics.ExpectedNetPence
		days := plan.Economics.GameDays
		if back != nil {
			net -= back.expense
			days += back.days
		}
		if net <= 0 || days <= 0 {
			continue
		}
		rate := float64(net) / days
		if !best.plan.Accepted || rate > best.rate || rate == best.rate && betterPlan(plan, best.plan) {
			best = growthOperatingQuote{plan: plan, required: required, transfers: transfers, rate: rate}
			if back != nil {
				best.returnTarget = back.target
				best.returnStores = back.stores
				best.returnLimit = back.storeLimit
			}
		}
	}
	return best
}

func growthOperatingAuthority(input PlanningInput, plan Plan) error {
	for _, key := range []string{"catalog", "clock", "zones", "vaults", "contracts", "ships", "rates",
		"books/" + plan.Origin, "ports/" + plan.Origin, "ports/" + plan.Destination,
		"routes/" + routeKey(plan.Origin, plan.Destination, input.Ship.Ship.ShipClass)} {
		if !input.fresh(key) {
			return fmt.Errorf("growth procurement waits for current successful %s authority", key)
		}
	}
	if plan.Kind == "spot" && !input.fresh("books/"+plan.Destination) {
		return errors.New("growth procurement waits for current executable destination demand")
	}
	return nil
}

func reserveGrowthCommissioning(input *PlanningInput, data *missionData) {
	if data.BuiltShipID == "" || data.BuiltShipID != input.Ship.Ship.ID {
		return
	}
	if port, exists := findPort(input.Catalog, data.BuildPort); exists {
		guns := max(0, data.TargetCannons-input.Ship.Ship.CannonsCount)
		input.Ship.EffectiveHoldCapacityTons -= 2 * guns
		input.ReserveByRegion[port.BankingRegion] = growthCashTotal(input.ReserveByRegion[port.BankingRegion], guns*15_000)
		// Only commissioning quotations may use hardware whose cash and hold
		// are reserved above. Actual departure safety still uses actual guns.
		if data.Action == nil || data.Action.Stage == "provision" || data.Action.Stage == "remit" {
			input.Ship.Ship.CannonsCount = max(input.Ship.Ship.CannonsCount, data.TargetCannons)
		}
	}
}

func (r *Runner) refreshGrowthProcurement(ctx context.Context, ship ShipState, mission *store.Mission, data *missionData) (bool, bool, error) {
	if err := r.insuranceActionReady(ship, "buy", data.Plan.Destination); err != nil {
		if changed, maintenanceErr := r.maintain(ctx, ship, mission, data); changed || maintenanceErr != nil {
			return false, changed, maintenanceErr
		}
		return false, false, r.hold(ctx, ship.Ship.ID, err.Error())
	}
	input, err := r.committedPlanningInput(ctx, ship)
	if err != nil {
		return false, false, err
	}
	if !r.growthSourceAuthority(input, data.Plan.Origin) {
		if ship.Ship.Status != "anchored_roadstead" || ship.Ship.CurrentPortID != data.Plan.Origin {
			return false, false, r.hold(ctx, ship.Ship.ID, "growth procurement waits for current lawful local quay prices")
		}
		if err := growthSourceInspectionReady(input, data.Plan); err != nil {
			return false, false, r.hold(ctx, ship.Ship.ID, err.Error())
		}
		changed, err := r.dockForMission(ctx, ship, mission, data, false)
		return false, changed, err
	}
	if err := growthOperatingAuthority(input, data.Plan); err != nil {
		return false, false, r.hold(ctx, ship.Ship.ID, err.Error())
	}
	input.pinnedHazard = &data.Plan.Economics
	quote := quoteGrowthDelivery(input, data.Plan, data.GrowthLane, !data.GrowthLane.InitialProcurementComplete, nil)
	if !quote.plan.Accepted {
		data.GrowthLane = nil
		changed, err := r.unwindProcurement(ctx, ship, mission, data, quote.plan.Reason)
		return false, changed, err
	}
	changed := growthPlanChanged(data.Plan, quote.plan)
	data.Plan = quote.plan
	mission.Quantity = quote.plan.Quantity
	mission.PricePence = quote.plan.BuyLimitPence
	if changed {
		if err := r.saveMission(ctx, mission, data); err != nil {
			return false, false, err
		}
	}
	if len(quote.transfers) > 0 {
		return false, true, r.stageGrowthFunding(ctx, ship, mission, data, quote.transfers[0])
	}
	return true, changed, nil
}

func growthPlanChanged(prior, next Plan) bool {
	return prior.Quantity != next.Quantity || prior.BuyLimitPence != next.BuyLimitPence ||
		prior.StoresToBuy != next.StoresToBuy || prior.StoresLimitPence != next.StoresLimitPence ||
		prior.TargetStoresTons != next.TargetStoresTons || prior.Economics != next.Economics
}

func (r *Runner) stageGrowthFunding(ctx context.Context, ship ShipState, mission *store.Mission, data *missionData, transfer observedRemittance) error {
	return r.stage(ctx, mission, data, ship, "remit", "/v1/treasury/remit", map[string]any{
		"from_region":        transfer.Rate.FromRegion,
		"to_region":          transfer.Rate.ToRegion,
		"gross_amount_pence": transfer.Gross,
	})
}

func (r *Runner) quoteGrowthContinuation(ctx context.Context, ship ShipState, data *missionData) (growthOperatingQuote, error) {
	input, err := r.committedPlanningInput(ctx, ship)
	if err != nil {
		return growthOperatingQuote{}, err
	}
	base := data.Plan
	if data.GrowthLane.NextPlan != nil {
		base = *data.GrowthLane.NextPlan
	}
	if !r.growthSourceAuthority(input, base.Origin) {
		return growthOperatingQuote{plan: reject(base, "optional growth return lost current lawful source access")}, nil
	}
	back, err := growthReturnBasis(input, base.Origin)
	if err != nil {
		return growthOperatingQuote{plan: reject(base, err.Error())}, nil
	}
	quote := quoteGrowthDelivery(input, base, data.GrowthLane, false, &back)
	if quote.plan.Accepted {
		// Ties release the hull locally. A past admission is not a standing
		// obligation to return, and cannot displace better destination work.
		for _, local := range PlanVoyages(input) {
			if local.Accepted && local.Economics.NetPencePerGameDay >= quote.rate {
				quote.plan = reject(quote.plan, "current destination-origin work matches or beats the optional return")
				break
			}
		}
	}
	return quote, nil
}

func (r *Runner) selectGrowthContinuation(ctx context.Context, ship ShipState, mission *store.Mission, data *missionData) (bool, error) {
	if data.Action != nil || !emptyCargo(ship) || len(freightStops(data.Freight)) > 0 || len(r.activeFreight(ship.Ship.ID)) > 0 {
		return false, r.hold(ctx, ship.Ship.ID, "growth continuation waits for all actual cargo, seals and actions to resolve")
	}
	quote, err := r.quoteGrowthContinuation(ctx, ship, data)
	if err != nil {
		return false, err
	}
	if !quote.plan.Accepted {
		return r.releaseGrowthContinuation(ctx, ship, mission, data)
	}
	data.GrowthLane.NextPlan = &quote.plan
	data.ReturnJourney = true
	data.FinishAtDestination = false
	mission.Destination = data.Plan.Origin
	mission.Phase = "return"
	return true, r.saveMission(ctx, mission, data)
}

func (r *Runner) releaseGrowthContinuation(ctx context.Context, ship ShipState, mission *store.Mission, data *missionData) (bool, error) {
	data.GrowthLane.NextPlan = nil
	data.ReturnJourney = false
	data.FinishAtDestination = true
	mission.Destination = data.Plan.Destination
	mission.Phase = "closing"
	return true, r.saveMission(ctx, mission, data)
}

func (r *Runner) driveGrowthReturn(ctx context.Context, ship ShipState, mission *store.Mission, data *missionData) (bool, error) {
	quote, err := r.quoteGrowthContinuation(ctx, ship, data)
	if err != nil {
		return false, err
	}
	if !quote.plan.Accepted {
		return r.releaseGrowthContinuation(ctx, ship, mission, data)
	}
	if growthPlanChanged(*data.GrowthLane.NextPlan, quote.plan) {
		data.GrowthLane.NextPlan = &quote.plan
		if err := r.saveMission(ctx, mission, data); err != nil {
			return false, err
		}
	}
	if len(quote.transfers) > 0 {
		return true, r.stageGrowthFunding(ctx, ship, mission, data, quote.transfers[0])
	}
	if ship.Stores.ProvisionsTons+1e-9 < quote.returnTarget {
		return r.provisionMission(ctx, ship, mission, data, quote.returnTarget)
	}
	return r.prepareDeparture(ctx, ship, mission, data)
}

func growthSourceInspectionReady(input PlanningInput, plan Plan) error {
	ship := input.Ship
	if ship.Voyage != nil || ship.Ship.Status != "anchored_roadstead" || ship.Ship.DockQueuedAt != nil ||
		ship.Ship.CurrentPortID != plan.Origin || !emptyCargo(ship) {
		return errors.New("growth source inspection waits for an empty unqueued source roadstead")
	}
	for _, key := range []string{"catalog", "clock", "ships", "vaults", "factors", "profile", "ports/" + plan.Origin} {
		if !input.fresh(key) {
			return fmt.Errorf("growth source inspection requires current successful %s authority", key)
		}
	}
	port, portOK := findPort(input.Catalog, plan.Origin)
	hull, hullOK := findHull(input.Catalog, ship.Ship.ShipClass)
	harbour := input.Ports[plan.Origin]
	if !portOK || port.BankingRegion == "" || !hullOK || hull.CrewRequired < 0 ||
		harbour.EffectiveBerths <= 0 || harbour.QueuedShips < 0 || harbour.BerthDuesPerDayPence < 0 {
		return errors.New("growth source inspection lacks known legal berth, crew or banking terms")
	}
	queueDays := 30.0/1_440 + float64(harbour.QueuedShips+1)/float64(harbour.EffectiveBerths)*0.25
	stores := 1 + float64(hull.CrewRequired)/200*queueDays
	if ship.Stores.ProvisionsTons+1e-9 < stores {
		return errors.New("growth source inspection cannot safely wait for its observed berth queue")
	}
	bill := harbour.BerthDuesPerDayPence / 4
	if harbour.BerthDuesPerDayPence%4 != 0 {
		bill++
	}
	if input.Balances[port.BankingRegion]-bill < input.ReserveByRegion[port.BankingRegion] {
		return errors.New("growth source inspection cannot fund its harbour bill without protected reserves or forecast receipts")
	}
	return nil
}

func (r *Runner) continueGrowth(ctx context.Context, ship ShipState, mission *store.Mission, data *missionData) (bool, error) {
	if !r.fresh("ships") || ship.Voyage != nil || ship.Ship.CurrentPortID != data.Plan.Origin || !emptyCargo(ship) {
		return false, r.hold(ctx, ship.Ship.ID, "growth continuation requires actual empty source arrival after settlement")
	}
	input, err := r.committedPlanningInput(ctx, ship)
	if err != nil {
		return false, err
	}
	plan := *data.GrowthLane.NextPlan
	if !r.growthSourceAuthority(input, ship.Ship.CurrentPortID) {
		if err := growthSourceInspectionReady(input, plan); err != nil {
			return false, r.hold(ctx, ship.Ship.ID, err.Error())
		}
		// Open the next accounting boundary before inspecting the source.
		// Its old conditional quote permits docking, never a cargo purchase.
		plan.Accepted = false
		plan.Reason = "paid return reached source; safely funded berth inspection must refresh executable prices before procurement"
	} else {
		if err := growthOperatingAuthority(input, plan); err != nil {
			return false, r.hold(ctx, ship.Ship.ID, err.Error())
		}
		quote := quoteGrowthDelivery(input, plan, data.GrowthLane, false, nil)
		if !quote.plan.Accepted {
			// The immutable first settlement retains the actual return. A new
			// purchase never needs to earn back that sunk cost or the hull.
			return true, r.db.DeleteMission(ctx, r.account.ID, ship.Ship.ID)
		}
		plan = quote.plan
	}
	next, nextData := r.newVoyage(ship, plan)
	next.Generation = growthHandoffGeneration(mission.Generation, ship.Ship.ID)
	lane := *data.GrowthLane
	lane.NextPlan = nil
	lane.InitialProcurementComplete = true
	nextData.GrowthLane = &lane
	return true, r.saveMission(ctx, next, &nextData)
}

// Called only by proven-first-send preflight, after the limiter wait. It must
// never rewrite a durable request or veto an attempted/ambiguous navigation.
func (r *Runner) growthAllocationReady(ctx context.Context, ship ShipState, data *missionData, action *durableAction) error {
	if data.GrowthLane == nil || action == nil {
		return nil
	}
	if action.Stage == "dock" {
		if !r.loanAuthority() || data.BuyDone || ship.Ship.CurrentPortID != data.Plan.Origin {
			return nil
		}
		input, err := r.committedPlanningInput(ctx, ship)
		if err != nil {
			return err
		}
		return growthSourceInspectionReady(input, data.Plan)
	}
	selectedReturn := data.ReturnJourney && data.GrowthLane.NextPlan != nil
	if action.Stage != "buy" && action.Stage != "provision" && action.Stage != "remit" && !(action.Stage == "depart" && selectedReturn) {
		return nil
	}
	if !selectedReturn && data.BuyDone {
		return nil
	}
	var quote growthOperatingQuote
	var err error
	if selectedReturn {
		quote, err = r.quoteGrowthContinuation(ctx, ship, data)
	} else {
		input, inputErr := r.committedPlanningInput(ctx, ship)
		if inputErr != nil {
			return inputErr
		}
		if !r.growthSourceAuthority(input, data.Plan.Origin) {
			return errors.New("growth first send lost lawful source access")
		}
		if err := growthOperatingAuthority(input, data.Plan); err != nil {
			return err
		}
		reserveGrowthCommissioning(&input, data)
		input.pinnedHazard = &data.Plan.Economics
		quote = quoteGrowthDelivery(input, data.Plan, data.GrowthLane, !data.GrowthLane.InitialProcurementComplete, nil)
	}
	if err != nil {
		return err
	}
	if !quote.plan.Accepted {
		return fmt.Errorf("growth first send lost its forward allocation: %s", quote.plan.Reason)
	}
	var request struct {
		Quantity    int64  `json:"quantity"`
		Price       int64  `json:"price_pence"`
		Destination string `json:"dest_port_id"`
		From        string `json:"from_region"`
		To          string `json:"to_region"`
		Gross       int64  `json:"gross_amount_pence"`
	}
	if json.Unmarshal(action.Body, &request) != nil {
		return errors.New("growth first send has an invalid immutable request")
	}
	if action.Stage == "remit" {
		for _, transfer := range quote.transfers {
			if request.From == transfer.Rate.FromRegion && request.To == transfer.Rate.ToRegion && request.Gross == transfer.Gross {
				return nil
			}
		}
		return errors.New("growth funding request no longer matches the measured funded itinerary")
	}
	if len(quote.transfers) > 0 {
		return errors.New("growth first send must await actual regional funding")
	}
	switch action.Stage {
	case "buy":
		if data.BuyDone || request.Quantity != quote.plan.Quantity || request.Price != quote.plan.BuyLimitPence || request.Quantity > data.GrowthLane.QuantityLimit {
			return errors.New("growth cargo request no longer fits its fresh bounded operating allocation")
		}
	case "provision":
		quantity, limit := quote.plan.StoresToBuy, quote.plan.StoresLimitPence
		if selectedReturn {
			quantity, limit = quote.returnStores, quote.returnLimit
		}
		if request.Quantity <= 0 || request.Quantity != quantity || request.Price != limit {
			return errors.New("growth store request no longer fits its funded cargo and navigation allocation")
		}
	case "depart":
		if request.Destination != data.Plan.Origin || ship.Stores.ProvisionsTons+1e-9 < quote.returnTarget {
			return errors.New("growth return lost its legal funded safe-store allocation")
		}
	}
	return nil
}
