package engine

import (
	"context"
	"errors"
	"fmt"
	"math"
	"strings"
	"time"

	"github.com/DevYukine/go-tradewinds/internal/store"
)

// Procurement can finish later than admission. Recompute the upper-bound arrival
// from current route and queue evidence rather than reusing the admitted ETA.
func procurementArrival(in PlanningInput, plan Plan, quantity int64) time.Time {
	hull, exists := findHull(in.Catalog, in.Ship.Ship.ShipClass)
	destination := in.Ports[plan.Destination]
	_, upper, validClock := in.serverBounds()
	if !exists || hull.BaseSpeedCentiknots <= 0 || !validClock || in.Clock.Calendar.TimeScale <= 0 ||
		!in.fresh("ports/"+plan.Destination) || destination.EffectiveBerths <= 0 {
		return time.Time{}
	}
	route, err := observedPlanningRoute(in, plan.Origin, plan.Destination, hull)
	if err != nil {
		return time.Time{}
	}
	tons := quantity
	for _, id := range plan.FreightIDs {
		for _, contract := range in.Contracts {
			if contract.ID == id && contract.ContractType == "freight" {
				tons = growthCashTotal(tons, max(0, contract.QuantityRequired))
				break
			}
		}
	}
	tons = max(tons, in.Ship.CargoUsedTons)
	sailingDays, portDays := deliveryGameDays(route, hull, destination, tons)
	return upper.Add(gameDuration(max(sailingDays+portDays, plan.Economics.GameDays), in.Clock.Calendar.TimeScale))
}

// acquiredRevenue uses the actual partial IOC quantity. The successful purchase
// stage is never reopened when a charter closes or destination evidence ages.
func acquiredRevenue(in PlanningInput, plan Plan, bought int64, obligations []freightObligation) (int64, string) {
	var revenue int64
	for _, obligation := range obligations {
		if !obligation.Settled {
			revenue += obligation.RewardPence
		}
	}
	if strings.HasPrefix(plan.Kind, "charter") && bought > 0 {
		var charter *Contract
		for i := range in.Contracts {
			if in.Contracts[i].ID == plan.ContractID {
				charter = &in.Contracts[i]
				break
			}
		}
		if charter == nil || charter.Status != "open" || charter.Stale || charter.RewardPerTonPence == nil || charter.RemainingQuantity <= 0 {
			if revenue > 0 {
				return revenue, "charter closed; only accepted freight reward remains counted"
			}
			return 0, "charter is no longer live after procurement"
		}
		hull, exists := findHull(in.Catalog, in.Ship.Ship.ShipClass)
		if !exists || hull.BaseSpeedCentiknots <= 0 || in.Clock.Calendar.TimeScale <= 0 {
			return 0, "cannot establish a fresh laden arrival before charter expiry"
		}
		route := in.Routes[routeKey(plan.Origin, plan.Destination, hull.ID)]
		if route.TotalDistanceNM <= 0 {
			return 0, "current charter route is missing after procurement"
		}
		days := float64(route.TotalDistanceNM)/(float64(hull.BaseSpeedCentiknots)/100*0.8)/24 + 1
		_, upper, ok := in.serverBounds()
		if !ok {
			return revenue, "current server clock authority is unavailable after procurement"
		}
		latestArrival := upper.Add(gameDuration(days, in.Clock.Calendar.TimeScale))
		if charter.ExpiresAt != nil && !latestArrival.Before(*charter.ExpiresAt) {
			return revenue, "charter expiry no longer has a game-day delivery buffer"
		}
		revenue += min(bought, charter.RemainingQuantity) * *charter.RewardPerTonPence
	}
	if strings.HasPrefix(plan.Kind, "spot") && bought > 0 {
		gross, _, _, _, reason, _ := destinationRevenue(in, plan.Destination, plan.Commodity, bought, procurementArrival(in, plan, bought))
		if reason != "" {
			return revenue, reason
		}
		revenue += gross
	}
	return revenue, ""
}

func (r *Runner) committedProcurementDepartureDecision(ctx context.Context, ship ShipState, destination string, data *missionData) (bool, error) {
	input, err := r.committedPlanningInput(ctx, ship)
	if err != nil {
		return false, err
	}
	return r.procurementDepartureDecision(input, ship, destination, data)
}

// procurementDepartureDecision has no persistence or mutation effects. False
// without an error selects executable local liquidation; an error preserves the
// mission on hold. Navigation, stores and freight deadlines remain safety gates
// in both departure preparation and first-send preflight.
func (r *Runner) procurementDepartureDecision(input PlanningInput, ship ShipState, destination string, data *missionData) (bool, error) {
	if data.Relocation != nil || data.ReturnJourney || data.Plan.Kind == "recovery" {
		return true, nil
	}
	if destination == "" || destination != data.Plan.Destination {
		return false, errors.New("procurement recovery requires the unchanged mission destination")
	}
	if !r.fresh("contracts") {
		return false, errors.New("procurement recovery requires fresh accepted-freight authority")
	}
	sealed := false
	for _, obligation := range data.Freight {
		if obligation.Settled {
			continue
		}
		contract, exists := r.contract(obligation.ContractID)
		if !exists || contract.Stale || contract.ContractType != "freight" || contract.Status != "active" ||
			contract.CarrierCompanyID != r.account.CompanyID || contract.CarrierShipID != ship.Ship.ID ||
			contract.CommodityID != obligation.Commodity || contract.DestPortID != obligation.Destination ||
			obligation.Quantity <= 0 || contract.RemainingQuantity != obligation.Quantity {
			return false, errors.New("procurement recovery must retain every matching fresh active freight obligation")
		}
		if reason := freightShortfall(ship, data.Freight, obligation.Commodity); reason != "" {
			return false, errors.New(reason)
		}
		sealed = true
	}
	for _, contract := range r.stateCache.Contracts {
		if contract.ContractType != "freight" || contract.Status != "active" ||
			contract.CarrierCompanyID != r.account.CompanyID || contract.CarrierShipID != ship.Ship.ID {
			continue
		}
		found := false
		for _, obligation := range data.Freight {
			if obligation.ContractID == contract.ID && !obligation.Settled {
				found = true
				break
			}
		}
		if !found {
			return false, errors.New("actual active freight is absent from the durable recovery itinerary")
		}
	}
	if (sealed || data.BoughtQuantity > 0) &&
		(!r.fresh("ports/"+ship.Ship.CurrentPortID) || !r.fresh("ports/"+destination)) {
		return false, errors.New("procurement recovery needs fresh local and destination harbour costs")
	}
	if sealed {
		// There is no legal local-liquidation alternative to an accepted seal.
		// Its safe, deadline-checked itinerary is not optional sunk-cost profit.
		return true, nil
	}
	if data.BoughtQuantity <= 0 {
		return true, nil
	}
	if ship.Ship.CurrentPortID != data.Plan.Origin {
		return false, errors.New("paid cargo and its authoritative procurement origin disagree")
	}
	if ship.CargoManifest[data.Plan.Commodity] < data.BoughtQuantity {
		return false, errors.New("paid cargo quantity is not fully evidenced by the actual manifest")
	}
	local, liquidatable, err := procurementBidRevenue(input, ship.Ship.CurrentPortID, data.Plan.Commodity, data.BoughtQuantity)
	if err != nil {
		return false, fmt.Errorf("local procurement recovery: %w", err)
	}
	eventPaidSpot := data.Plan.Accepted && data.Plan.EventPriceStress && strings.HasPrefix(data.Plan.Kind, "spot")
	var revenue int64
	var destinationSource string
	if eventPaidSpot {
		var reason string
		revenue, _, _, destinationSource, reason, _ = destinationRevenue(input, data.Plan.Destination, data.Plan.Commodity, data.BoughtQuantity, procurementArrival(input, data.Plan, data.BoughtQuantity))
		if reason != "" {
			err = errors.New("destination procurement recovery lacks current bounded receipt evidence")
		}
	} else {
		revenue, err = procurementDestinationRevenue(input, data.Plan, data.BoughtQuantity)
	}
	unpricedEventCargo := false
	if err != nil {
		accepted, ok := paidSpotRevenue(input, data.Plan, data.BoughtQuantity)
		if ok {
			revenue = accepted
		} else if eventPaidSpot {
			unpricedEventCargo = true
		} else {
			return false, err
		}
	}
	// Both exits settle the current origin stay. Paid invoices, acquisition,
	// existing stores and fixed fleet upkeep are not another continuation bill.
	// Only local sale handling can extend that common stay; the destination
	// incurs a new berth stay. Sound tolls remain rejected by the route gate.
	hull, exists := findHull(input.Catalog, ship.Ship.ShipClass)
	origin, originExists := findPort(input.Catalog, ship.Ship.CurrentPortID)
	arrivalPort, destinationExists := findPort(input.Catalog, destination)
	if !exists || !originExists || !destinationExists {
		return false, errors.New("current recovery hull or harbour classification is unavailable")
	}
	route, err := observedPlanningRoute(input, origin.ID, destination, hull)
	if err != nil {
		return false, errors.New("current recovery route or its costs are unavailable")
	}
	_, upper, clockOK := input.serverBounds()
	if !clockOK || input.Clock.Calendar.TimeScale <= 0 {
		return false, errors.New("current recovery clock authority is unavailable")
	}
	originDetail, originKnown := input.Ports[origin.ID]
	destinationDetail, destinationKnown := input.Ports[destination]
	if !originKnown || !destinationKnown || originDetail.ID != origin.ID || destinationDetail.ID != destination ||
		originDetail.BerthDuesPerDayPence < 0 || destinationDetail.BerthDuesPerDayPence < 0 {
		return false, errors.New("current recovery harbour dues are unavailable or invalid")
	}
	originDues := originDetail.BerthDuesPerDayPence
	destinationDues := destinationDetail.BerthDuesPerDayPence
	localHandlingDays := float64(data.BoughtQuantity) * 2 / 1_440
	if origin.IsHub {
		localHandlingDays /= 2
	}
	destinationHandlingDays := float64(data.BoughtQuantity) * 2 / 1_440
	if arrivalPort.IsHub {
		destinationHandlingDays /= 2
	}
	destinationBills := math.Ceil(float64(destinationDues) * max(0.25, 30.0/1_440+destinationHandlingDays))
	var localBills, maximumLocalBills float64
	if liquidatable && originDues > 0 {
		switch ship.Ship.Status {
		case "berthed":
			if ship.Ship.BerthedAt == nil {
				// Unknown elapsed time cannot cost more than the extra handling.
				// Decide only if every bill inside this bound has the same winner.
				maximumLocalBills = math.Ceil(float64(originDues) * localHandlingDays)
			} else {
				if ship.Ship.BerthedAt.After(upper) {
					return false, errors.New("current origin berth stay is invalid for recovery comparison")
				}
				accruedDays := upper.Sub(*ship.Ship.BerthedAt).Seconds() * float64(input.Clock.Calendar.TimeScale) / 86_400
				localBills = math.Ceil(float64(originDues)*max(0.25, accruedDays+localHandlingDays)) -
					math.Ceil(float64(originDues)*max(0.25, accruedDays))
				maximumLocalBills = localBills
			}
		case "anchored_roadstead":
			localBills = math.Ceil(float64(originDues) * max(0.25, 30.0/1_440+localHandlingDays))
			maximumLocalBills = localBills
		default:
			return false, errors.New("current stationary recovery harbour state is unavailable")
		}
	}
	if math.IsNaN(destinationBills) || math.IsInf(destinationBills, 0) || destinationBills >= float64(math.MaxInt64) ||
		math.IsNaN(maximumLocalBills) || math.IsInf(maximumLocalBills, 0) || localBills < 0 ||
		maximumLocalBills >= float64(math.MaxInt64) {
		return false, errors.New("remaining procurement recovery bills exceed integer-pence bounds")
	}
	if unpricedEventCargo || eventPaidSpot && strings.HasPrefix(destinationSource, retainedBookSource) {
		// A lost shock quote is not a permanent receipt floor. Paid cost is
		// sunk when an estimated destination recovery can price the choice.
		if !liquidatable {
			return true, nil
		}
		recoveryFloor := data.AcquisitionPence
		if strings.HasPrefix(destinationSource, retainedBookSource) {
			// This is a labelled retained/pre-event estimate, not executable
			// receipts. Deduct the new destination stay and consumed stores.
			if hull.BaseSpeedCentiknots <= 0 || destinationDetail.EffectiveBerths <= 0 {
				return true, nil
			}
			sailingDays, portDays := deliveryGameDays(route, hull, destinationDetail, max(data.BoughtQuantity, ship.CargoUsedTons))
			consumed := float64(hull.CrewRequired)/100*sailingDays + float64(hull.CrewRequired)/200*portDays
			provisions, err := provisionsExpense(input.Markets[origin.ID], consumed)
			if err != nil {
				return true, nil
			}
			recoveryFloor = revenue - int64(destinationBills)
			if recoveryFloor < math.MinInt64+provisions {
				recoveryFloor = math.MinInt64
			} else {
				recoveryFloor -= provisions
			}
		} else if !data.CostsKnown || data.AcquisitionPence <= 0 {
			// With no destination estimate, only actual known acquisition
			// supplies the conservative fallback floor.
			return true, nil
		}
		// Use the highest possible extra local bill so the floor holds even
		// when the elapsed origin berth stay is not authoritative.
		return local-int64(maximumLocalBills) < recoveryFloor, nil
	}
	destinationRecovery := revenue - int64(destinationBills)
	if !liquidatable {
		if destinationRecovery > 0 {
			return true, nil
		}
		return false, errors.New("destination recovery does not cover future bills and no full local liquidation is executable")
	}
	if destinationRecovery > local-int64(localBills) {
		return true, nil
	}
	if destinationRecovery <= local-int64(maximumLocalBills) {
		return false, nil
	}
	return false, errors.New("current origin berth stay cannot distinguish the better recovery")
}

// paidSpotRevenue values paid spot cargo when the destination cannot price the
// whole lot. A destination quay book goes stale as soon as no ship is there to
// refresh it, usually right after buying for it, and holding paid cargo in port
// only accrues dues. Without a fresh book, value the tons aboard at the per-ton
// sell floor accepted at purchase (worst depth fill, stressed NPC bid or
// one-ton probe floor; never freight income), lowered by any fresh gossip
// floor. A fresh thin book counts only receipts that fill now. It is used only
// for paid cargo and never authorizes a purchase.
func paidSpotRevenue(in PlanningInput, plan Plan, quantity int64) (int64, bool) {
	if !strings.HasPrefix(plan.Kind, "spot") || !plan.Accepted || !in.fresh("clock") || quantity <= 0 {
		return 0, false
	}
	arrival := procurementArrival(in, plan, quantity)
	if in.fresh("books/" + plan.Destination) {
		from, to, bounded := in.marketObservationBounds("books/" + plan.Destination)
		if !bounded {
			return 0, false
		}
		if _, eventReason := in.marketObservationPolicy(plan.Destination, plan.Commodity, from, to, arrival); eventReason != "" {
			revenue, _, _, _, reason, _ := destinationRevenue(in, plan.Destination, plan.Commodity, quantity, arrival)
			return revenue, reason == ""
		}
		receipts, _, err := procurementBidRevenue(in, plan.Destination, plan.Commodity, quantity)
		if err != nil {
			return 0, false
		}
		return receipts, true
	}
	if plan.EventPriceStress {
		// An admitted shock bid is not a permanent liquidation floor after
		// presence is lost. Only a current bounded whole-lot quote can replace it.
		revenue, _, _, _, reason, _ := destinationRevenue(in, plan.Destination, plan.Commodity, quantity, arrival)
		return revenue, reason == ""
	}
	perTon := plan.SellLimitPence
	if _, gossipFloor, _, _, reason, _ := destinationRevenue(in, plan.Destination, plan.Commodity, 1, arrival); reason == "" {
		perTon = min(perTon, gossipFloor)
	}
	if perTon <= 0 {
		return 0, false
	}
	revenue := checkedPenceProduct(quantity, perTon)
	if revenue == math.MaxInt64 {
		return 0, false
	}
	return revenue, true
}

// A fresh empty or partially filled bid ladder is known limited liquidity, not
// missing evidence. Preserve its measured receipts without authorizing full-lot
// liquidation; absent, stale or malformed depth still holds the mission.
func procurementBidRevenue(in PlanningInput, port, commodity string, quantity int64) (int64, bool, error) {
	if quantity <= 0 {
		return 0, false, errors.New("current recovery quantity must be positive")
	}
	if !in.fresh("books/" + port) {
		return 0, false, errors.New("current executable bid depth is missing or stale")
	}
	book := in.Markets[port]
	entry := bookCommodity(book, commodity)
	if book.Visibility != "quay" || entry == nil || entry.Depth == nil {
		return 0, false, errors.New("current full bid depth is unavailable")
	}
	if len(entry.Depth.Bids) == 0 {
		return 0, false, nil
	}
	levels := entry.Depth.Bids
	if reserved := in.ReservedRetainedBidTons[port][commodity]; reserved > 0 {
		levels = append([]Level(nil), levels...)
		reserveBidLevels(levels, reserved)
		remaining := false
		for _, level := range levels {
			if level.PricePence > 0 && level.PricePence <= maximumPence && level.RemainingQuantity > 0 {
				remaining = true
				break
			}
		}
		if !remaining {
			for _, level := range entry.Depth.Bids {
				if level.PricePence > 0 && level.PricePence <= maximumPence && level.RemainingQuantity > 0 {
					return 0, false, nil
				}
			}
		}
	}
	fill, err := WalkDepth(levels, quantity, false)
	if err != nil {
		if fill.Quantity > 0 && fill.Quantity < quantity {
			return fill.GrossPence, false, nil
		}
		return 0, false, err
	}
	return fill.GrossPence, true, nil
}

func procurementDestinationRevenue(in PlanningInput, plan Plan, quantity int64) (int64, error) {
	if quantity <= 0 {
		return 0, errors.New("destination procurement recovery requires a positive cargo quantity")
	}
	if !in.fresh("clock") || in.Clock.Calendar.TimeScale <= 0 {
		return 0, errors.New("destination procurement recovery requires current clock authority")
	}
	if strings.HasPrefix(plan.Kind, "spot") {
		// Use the planner's exact evidence policy: full depth, or a fresh
		// one-ton gossip estimate. The persisted PriceSource keeps that probe
		// explicitly estimated; it is not executable destination quantity.
		revenue, reason := acquiredRevenue(in, plan, quantity, nil)
		if reason != "" {
			return 0, errors.New("destination procurement recovery lacks current bounded receipt evidence")
		}
		return revenue, nil
	}
	best, executable, bidErr := procurementBidRevenue(in, plan.Destination, plan.Commodity, quantity)
	if strings.HasPrefix(plan.Kind, "charter") && in.fresh("contracts") {
		for _, contract := range in.Contracts {
			if contract.ID != plan.ContractID || contract.ContractType != "charter" || contract.Status != "open" ||
				contract.Stale || contract.OriginPortID != plan.Destination || contract.CommodityID != plan.Commodity ||
				contract.RemainingQuantity < quantity || contract.RewardPerTonPence == nil || *contract.RewardPerTonPence <= 0 {
				continue
			}
			if quantity > math.MaxInt64 / *contract.RewardPerTonPence {
				return 0, errors.New("current charter recovery exceeds integer-pence bounds")
			}
			revenue, reason := acquiredRevenue(in, plan, quantity, nil)
			if reason != "" {
				continue
			}
			if !executable || revenue > best {
				best = revenue
				executable = true
			}
		}
	}
	if !executable {
		if bidErr != nil {
			return 0, fmt.Errorf("destination procurement recovery: %w", bidErr)
		}
		return 0, errors.New("destination procurement recovery has no executable full-lot receipts")
	}
	return best, nil
}

func (r *Runner) unwindProcurement(ctx context.Context, ship ShipState, mission *store.Mission, data *missionData, reason string) (bool, error) {
	if !r.fresh("contracts") || len(freightStops(data.Freight)) > 0 || len(r.activeFreight(ship.Ship.ID)) > 0 {
		return false, r.hold(ctx, ship.Ship.ID, "procurement unwind requires fresh proof that no sealed obligation prohibits diversion: "+reason)
	}
	if err := r.decision(ctx, ship.Ship.ID, "procurement_unwind", "release the completed procurement plan for executable local recovery without repeating its purchase: "+reason, data.Plan); err != nil {
		return false, err
	}
	if data.BoughtQuantity == 0 && data.AcquisitionPence == 0 && len(data.Freight) == 0 && emptyCargo(ship) {
		// Release only hypothetical inventory/demand. Actual paid fees remain
		// attached to this mission and close through its normal settlement.
		data.Plan.Quantity = 0
		data.Plan.BuyLimitPence = 0
		data.Plan.StoresToBuy = 0
		data.Plan.TargetStoresTons = 0
		data.Plan.ContractID = ""
		data.Plan.FreightIDs = nil
		mission.Quantity = 0
		mission.ContractID = ""
		mission.PricePence = 0
	}
	data.Plan.Kind = "aborted"
	data.Plan.Reason = reason
	data.Plan.Origin = ""
	data.Plan.Destination = ship.Ship.CurrentPortID
	data.Eligible = make(map[string]int64)
	mission.Destination = ship.Ship.CurrentPortID
	mission.Phase = "arrival"
	return true, r.saveMission(ctx, mission, data)
}
