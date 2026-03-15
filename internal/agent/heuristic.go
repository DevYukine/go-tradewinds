package agent

import (
	"context"
	"fmt"
	"math"
	"sort"
	"time"

	"github.com/google/uuid"
	"go.uber.org/zap"
)

// HeuristicAgent uses hand-coded rules for trading decisions.
// It adapts behavior based on the StrategyHint in each request:
//   - "arbitrage": fast trades, small ships, profit-per-distance scoring
//   - "bulk_hauler": large ships, high-value goods, total-profit scoring
//   - "market_maker": NPC arbitrage + P2P market order filling/posting
type HeuristicAgent struct {
	logger *zap.Logger
}

// NewHeuristicAgent creates a new heuristic-based agent.
func NewHeuristicAgent(logger *zap.Logger) *HeuristicAgent {
	return &HeuristicAgent{
		logger: logger.Named("heuristic_agent"),
	}
}

func (a *HeuristicAgent) Name() string { return "heuristic" }

// heldCargo represents cargo that should not be sold at the current port
// because a reachable destination offers a significantly better price.
type heldCargo struct {
	goodID     uuid.UUID
	quantity   int
	bestDestID uuid.UUID
	profitGain int // total extra profit from holding (per-unit gain * quantity)
}

// ---------------------------------------------------------------------------
// Trade Decisions
// ---------------------------------------------------------------------------

// DecideTradeAction evaluates the current port and price cache to decide
// what to sell, what to buy, and where to sail next. Behavior varies by strategy.
func (a *HeuristicAgent) DecideTradeAction(_ context.Context, req TradeDecisionRequest) (*TradeDecision, error) {
	ship := req.Ship
	if ship.PortID == nil {
		return &TradeDecision{Action: "wait", Reasoning: "ship is not docked"}, nil
	}
	currentPortID := *ship.PortID

	// Build indexes for fast lookup — needed by smart selling and trade decisions.
	priceIndex := a.buildPriceIndex(req.PriceCache)
	reachable := a.reachablePorts(req.Routes, currentPortID)
	taxIndex := a.buildPortTaxIndex(req.Ports)

	if len(reachable) == 0 {
		sells := a.buildSellOrders(ship)
		a.logger.Warn("no outgoing routes found for port, attempting fallback",
			zap.String("port_id", currentPortID.String()),
			zap.Int("total_routes", len(req.Routes)),
		)
		var fallbackDest *uuid.UUID
		for _, p := range req.Ports {
			if p.ID != currentPortID {
				id := p.ID
				fallbackDest = &id
				break
			}
		}
		return &TradeDecision{
			Action: "sell_and_buy", SellOrders: sells, SailTo: fallbackDest,
			Reasoning: "no cached routes for port, sailing to fallback destination", Confidence: 0.3,
		}, nil
	}

	// Step 1: Smart sell — hold cargo that's worth more at a reachable destination.
	sells, held := a.buildSmartSellOrders(ship, priceIndex, reachable, taxIndex, currentPortID)

	if len(held) > 0 {
		a.logger.Info("holding cargo for better destination",
			zap.Int("items_held", len(held)),
			zap.Int("items_sold", len(sells)),
		)
	}

	budget := req.Constraints.MaxSpend

	// Step 1.5: Fill profitable P2P orders at the current port.
	// This happens before NPC buying since fills can be more profitable.
	fills, fillCost := a.findProfitableOrderFills(req.PortOrders, req.OwnOrders, priceIndex, taxIndex, currentPortID, budget)
	budget -= fillCost

	if budget <= 0 {
		dest := a.closestPort(reachable)
		// Even when broke, board passengers — they're pure revenue.
		// Check if a passenger destination is better than closest port.
		bestPassDest, bestPassRev := a.bestPassengerDestination(req.AvailablePassengers, reachable, ship.Speed)
		if bestPassRev > 0 {
			dest = bestPassDest
		}
		passengers := a.selectPassengers(req.AvailablePassengers, req.BoardedPassengers, ship.PassengerCap, &dest, reachable, 8.0, ship.Speed)
		return &TradeDecision{
			Action: "sell_and_buy", SellOrders: sells, FillOrders: fills, SailTo: &dest,
			BoardPassengers: passengers,
			Reasoning:       "treasury at floor, selling cargo and filling orders before moving on", Confidence: 0.4,
		}, nil
	}

	// Step 2: Find best opportunity — scoring differs by strategy.
	// Read tunable params with defaults.
	passengerWeight := getParam(req.Params, "passengerWeight", 8.0)
	passengerDestBonus := getParam(req.Params, "passengerDestBonus", 8.0)
	minMarginPct := getParam(req.Params, "minMarginPct", 0.08)
	speculativeEnabled := getParam(req.Params, "speculativeTradeEnabled", 0.0) > 0.5

	// Build route history bonus index for destination scoring.
	routeBonus := a.buildRouteHistoryBonus(req.RouteHistory, currentPortID)
	// Exploration bonus scales with idle ticks: zero when actively trading,
	// grows when a ship is sitting idle. This ensures profitable routes are
	// never overridden, but idle ships are nudged toward unexplored ports.
	explorationBonus := a.buildExplorationBonus(req.RouteHistory, reachable, currentPortID)
	idleScale := math.Min(float64(ship.IdleTicks), 3.0) / 3.0 // 0.0 → 1.0 over 3 idle ticks
	for id := range explorationBonus {
		explorationBonus[id] *= idleScale
	}

	var decision *TradeDecision
	var err error

	switch req.StrategyHint {
	case "bulk_hauler":
		decision, err = a.decideBulkHaulerTrade(req, sells, priceIndex, reachable, taxIndex, currentPortID, budget, passengerWeight, minMarginPct, speculativeEnabled, held, routeBonus, explorationBonus)
	default:
		decision, err = a.decideArbitrageTrade(req, sells, priceIndex, reachable, taxIndex, currentPortID, budget, passengerWeight, minMarginPct, speculativeEnabled, held, routeBonus, explorationBonus)
	}

	if err != nil {
		return decision, err
	}

	// Attach P2P fills to the decision.
	decision.FillOrders = fills

	// Step 2.4: Warehouse operations.
	// Load profitable warehouse inventory onto the ship, and store cheap goods
	// when the ship has spare capacity and no better use for it.
	// All strategies benefit from warehouse ops — market_makers especially since
	// they're most likely to have warehouses.
	{
		loads, stores := a.warehouseOps(req, decision, priceIndex, reachable, taxIndex, currentPortID, budget, minMarginPct)
		decision.WarehouseLoads = loads
		decision.WarehouseStores = stores
	}

	// Step 2.4b: Warehouse pickup routing.
	// When the ship has no profitable trade (low confidence) and no warehouse
	// loads were generated (not at a warehouse port), check if any reachable
	// warehouse has goods worth picking up. Route the ship there instead of
	// sitting idle or relocating randomly.
	if decision.Confidence <= 0.5 && len(decision.WarehouseLoads) == 0 && len(req.Warehouses) > 0 {
		bestWhPort, bestWhProfit := a.findBestWarehousePickup(req, priceIndex, taxIndex, reachable, currentPortID)
		if bestWhProfit > 0 {
			decision.SailTo = &bestWhPort
			decision.Action = "sell_and_buy"
			decision.Reasoning += fmt.Sprintf(" + routing to warehouse port for pickup (%d est. profit)", bestWhProfit)
		}
	}

	// Step 2.5: Passenger-only destination override.
	// If passenger revenue for the best passenger destination exceeds the
	// expected trade PROFIT of the chosen destination, switch to the passenger destination.
	if decision.SailTo != nil {
		bestPassDest, bestPassRev := a.bestPassengerDestination(req.AvailablePassengers, reachable, ship.Speed)
		expectedProfit := int64(0)
		for _, b := range decision.BuyOrders {
			buyPP, buyOK := priceIndex[priceKey(currentPortID, b.GoodID)]
			sellPP, sellOK := priceIndex[priceKey(*decision.SailTo, b.GoodID)]
			if buyOK && sellOK && sellPP.SellPrice > buyPP.BuyPrice {
				buyTax := buyPP.BuyPrice * taxIndex[currentPortID] / 10000
				sellTax := sellPP.SellPrice * taxIndex[*decision.SailTo] / 10000
				profit := sellPP.SellPrice - buyPP.BuyPrice - buyTax - sellTax
				expectedProfit += int64(b.Quantity) * int64(profit)
			}
		}
		if bestPassRev > 0 && int64(bestPassRev) > expectedProfit && bestPassDest != *decision.SailTo {
			decision.SailTo = &bestPassDest
			decision.Reasoning += " (overridden: passenger revenue dominates)"
		}
	}

	// Step 3: Board passengers heading to our destination (or any reachable port).
	decision.BoardPassengers = a.selectPassengers(
		req.AvailablePassengers, req.BoardedPassengers,
		ship.PassengerCap, decision.SailTo, reachable, passengerDestBonus, ship.Speed,
	)

	return decision, nil
}

// selectPassengers picks profitable passengers to board, preferring those heading
// to the chosen destination. Filters out passengers that cannot be delivered
// before their deadline. Returns passenger IDs to board.
func (a *HeuristicAgent) selectPassengers(
	available []PassengerInfo,
	boarded []PassengerInfo,
	passengerCap int,
	destPortID *uuid.UUID,
	reachable map[uuid.UUID]float64,
	destBonus float64,
	shipSpeed int,
) []uuid.UUID {
	if passengerCap <= 0 || len(available) == 0 {
		return nil
	}

	// Remaining capacity = cap minus total individual passengers already boarded.
	boardedCount := 0
	for _, p := range boarded {
		boardedCount += p.Count
	}
	remaining := passengerCap - boardedCount
	if remaining <= 0 {
		return nil
	}

	now := time.Now()
	speed := math.Max(float64(shipSpeed), 1.0)

	// Score passengers: those heading to our destination get a bonus.
	type scored struct {
		id    uuid.UUID
		count int
		score float64
	}
	var candidates []scored
	for _, p := range available {
		// Only board passengers heading to a reachable port.
		dist, ok := reachable[p.DestinationPortID]
		if !ok {
			continue
		}

		// Skip passengers that have already expired or cannot be delivered
		// in time. Use a 20% safety margin on travel time to account for
		// docking delays and processing time.
		travelMins := dist / speed
		arrivalTime := now.Add(time.Duration(travelMins*1.2) * time.Minute)
		if !p.ExpiresAt.IsZero() && arrivalTime.After(p.ExpiresAt) {
			continue
		}

		// Skip groups that are too large to fit.
		if p.Count > remaining {
			continue
		}

		bidPerHead := float64(p.Bid) / float64(p.Count)
		score := bidPerHead / max(dist, 1.0)

		// Bonus if heading to the same destination we've already chosen.
		if destPortID != nil && p.DestinationPortID == *destPortID {
			score *= destBonus
		}

		candidates = append(candidates, scored{id: p.ID, count: p.Count, score: score})
	}

	sort.Slice(candidates, func(i, j int) bool {
		return candidates[i].score > candidates[j].score
	})

	var selected []uuid.UUID
	used := 0
	for _, c := range candidates {
		if used+c.count > remaining {
			continue // skip groups that don't fit
		}
		selected = append(selected, c.id)
		used += c.count
	}

	if len(selected) > 0 {
		a.logger.Info("selecting passengers to board",
			zap.Int("count", len(selected)),
			zap.Int("available", len(available)),
			zap.Int("capacity_remaining", remaining),
		)
	}

	return selected
}

// decideArbitrageTrade scores destinations by total achievable profit per unit
// of distance (fast turnover). It evaluates all reachable destinations, computing
// the full ship-fill profit for each, then picks the best.
func (a *HeuristicAgent) decideArbitrageTrade(
	req TradeDecisionRequest,
	sells []SellOrder,
	priceIndex map[string]PricePoint,
	reachable map[uuid.UUID]float64,
	taxIndex map[uuid.UUID]int,
	currentPortID uuid.UUID,
	budget int64,
	passengerWeight float64,
	minMarginPct float64,
	speculativeEnabled bool,
	held []heldCargo,
	routeBonus map[uuid.UUID]float64,
	explorationBonus map[uuid.UUID]float64,
) (*TradeDecision, error) {
	buyTaxBps := taxIndex[currentPortID]
	now := time.Now()
	speed := math.Max(float64(req.Ship.Speed), 1.0)

	// Build claimed route set for anti-self-competition filtering.
	claimedSet := make(map[string]bool, len(req.ClaimedRoutes))
	for _, key := range req.ClaimedRoutes {
		claimedSet[key] = true
	}

	// Build passenger revenue index for destination scoring.
	// Include both available passengers (can board) and boarded passengers
	// (already on ship, must be delivered). Filter out passengers that
	// cannot be delivered before their deadline.
	passengerRevByDest := make(map[uuid.UUID]int)
	for _, p := range req.AvailablePassengers {
		dist, ok := reachable[p.DestinationPortID]
		if !ok {
			continue
		}
		if !p.ExpiresAt.IsZero() {
			travelMins := dist / speed
			if now.Add(time.Duration(travelMins*1.2) * time.Minute).After(p.ExpiresAt) {
				continue
			}
		}
		passengerRevByDest[p.DestinationPortID] += p.Bid
	}
	for _, p := range req.BoardedPassengers {
		// Boarded passengers are committed revenue — weight them heavily.
		// Add urgency multiplier for passengers near their deadline to
		// prevent the ship from detouring and missing the delivery window.
		weight := 2
		if !p.ExpiresAt.IsZero() {
			dist, ok := reachable[p.DestinationPortID]
			if !ok {
				continue
			}
			travelMins := dist / speed
			arrivalTime := now.Add(time.Duration(travelMins*1.2) * time.Minute)
			if arrivalTime.After(p.ExpiresAt) {
				// Already impossible — still count to avoid total loss.
				weight = 1
			} else {
				remaining := p.ExpiresAt.Sub(now).Minutes()
				// Urgency: if less than 2x travel time remains, boost heavily.
				if remaining < travelMins*2 {
					weight = 5
				}
			}
		}
		passengerRevByDest[p.DestinationPortID] += p.Bid * weight
	}

	// Build held cargo bonus by destination.
	heldProfitByDest := make(map[uuid.UUID]int)
	for _, h := range held {
		heldProfitByDest[h.bestDestID] += h.profitGain
	}

	// Collect all profitable (good, destination) pairs.
	type goodOpp struct {
		goodID   uuid.UUID
		buyPrice int
		profit   int
		roi      float64 // profit / effective buy cost (for sorting)
	}
	destGoods := make(map[uuid.UUID][]goodOpp)

	for _, pp := range req.PriceCache {
		if pp.PortID != currentPortID || pp.BuyPrice <= 0 {
			continue
		}
		buyTaxCost := pp.BuyPrice * buyTaxBps / 10000
		effectiveBuyCost := pp.BuyPrice + buyTaxCost
		for destID := range reachable {
			dp, ok := priceIndex[priceKey(destID, pp.GoodID)]
			if !ok || dp.SellPrice <= 0 {
				continue
			}
			// Skip routes claimed by other ships (anti-self-competition).
			rk := currentPortID.String() + ":" + destID.String() + ":" + pp.GoodID.String()
			if claimedSet[rk] {
				continue
			}
			sellTaxCost := dp.SellPrice * taxIndex[destID] / 10000
			profit := dp.SellPrice - pp.BuyPrice - buyTaxCost - sellTaxCost
			if profit < int(float64(pp.BuyPrice)*minMarginPct) {
				continue
			}
			roi := float64(profit) / float64(max(effectiveBuyCost, 1))
			destGoods[destID] = append(destGoods[destID], goodOpp{pp.GoodID, pp.BuyPrice, profit, roi})
		}
	}

	// Capacity available for new buys (subtract held cargo).
	heldCapacity := 0
	for _, h := range held {
		heldCapacity += h.quantity
	}
	buyCapacity := req.Ship.Capacity - heldCapacity

	// Upkeep cost per minute for this ship (upkeep is per 5-hour cycle = 300 minutes).
	upkeepPerMin := float64(req.Ship.Upkeep) / 300.0

	// Score each destination by simulating a full ship fill.
	type destCandidate struct {
		destID uuid.UUID
		score  float64
		goods  []goodOpp
	}
	var candidates []destCandidate

	for destID, goods := range destGoods {
		// Deduplicate and sort goods by ROI descending (profit per gold invested).
		seen := make(map[uuid.UUID]bool)
		var unique []goodOpp
		for _, g := range goods {
			if !seen[g.goodID] {
				seen[g.goodID] = true
				unique = append(unique, g)
			}
		}
		sort.Slice(unique, func(i, j int) bool { return unique[i].roi > unique[j].roi })

		// Simulate greedy fill to compute total achievable profit.
		remaining := buyCapacity
		remainBudget := budget
		totalProfit := 0
		for _, g := range unique {
			if remaining <= 0 || remainBudget <= 0 {
				break
			}
			qty := a.calcQuantity(remainBudget, g.buyPrice, buyTaxBps, remaining)
			if qty > 0 {
				totalProfit += g.profit * qty
				remaining -= qty
				effectiveCost := g.buyPrice + g.buyPrice*buyTaxBps/10000
				remainBudget -= int64(qty * effectiveCost)
			}
		}

		dist := math.Max(reachable[destID], 1.0)
		// Score by profit-per-minute instead of profit-per-distance.
		// totalTripMinutes = travel time + 2 min overhead for trade execution.
		travelMins := dist / speed
		totalTripMins := travelMins + 2.0
		travelUpkeep := upkeepPerMin * totalTripMins
		adjustedProfit := float64(totalProfit) - travelUpkeep

		score := adjustedProfit / totalTripMins
		score += float64(passengerRevByDest[destID]) / totalTripMins * passengerWeight
		score += float64(heldProfitByDest[destID]) / totalTripMins
		// Use sqrt(trip time) for route and exploration bonuses so that
		// far-away ports aren't as harshly penalized for these components.
		sqrtTrip := math.Sqrt(totalTripMins)
		score += routeBonus[destID] / sqrtTrip
		score += explorationBonus[destID] / sqrtTrip

		candidates = append(candidates, destCandidate{destID, score, unique})
	}

	// Add passenger-only destinations that have no cargo profit but do have
	// passenger revenue. These would otherwise be invisible to scoring.
	candidateSet := make(map[uuid.UUID]bool, len(candidates))
	for _, c := range candidates {
		candidateSet[c.destID] = true
	}
	for destID, passRev := range passengerRevByDest {
		if candidateSet[destID] || passRev <= 0 {
			continue
		}
		if _, ok := reachable[destID]; !ok {
			continue
		}
		dist := math.Max(reachable[destID], 1.0)
		totalTripMins := dist/speed + 2.0
		sqrtTrip := math.Sqrt(totalTripMins)
		score := float64(passRev) / totalTripMins * passengerWeight
		score += float64(heldProfitByDest[destID]) / totalTripMins
		score += routeBonus[destID] / sqrtTrip
		score += explorationBonus[destID] / sqrtTrip
		candidates = append(candidates, destCandidate{destID, score, nil})
	}

	// Add opportunity sell-port bonus from ProfitAnalyzer.
	oppSellBonus := a.buildOpportunitySellBonus(req.TopOpportunities, reachable)
	for i := range candidates {
		if bonus, ok := oppSellBonus[candidates[i].destID]; ok {
			candidates[i].score += bonus
		}
	}

	sort.Slice(candidates, func(i, j int) bool { return candidates[i].score > candidates[j].score })

	if len(candidates) > 0 {
		best := candidates[0]

		// Build buy orders for the winning destination.
		// Only buy goods with positive profit — skip loss-making cargo even if
		// the destination was chosen for passengers or other bonuses.
		remaining := buyCapacity
		remainBudget := budget
		var buys []BuyOrder
		for _, g := range best.goods {
			if remaining <= 0 || remainBudget <= 0 {
				break
			}
			if g.profit <= 0 {
				continue // Don't buy cargo that loses money.
			}
			qty := a.calcQuantity(remainBudget, g.buyPrice, buyTaxBps, remaining)
			if qty > 0 {
				buys = append(buys, BuyOrder{GoodID: g.goodID, Quantity: qty})
				remaining -= qty
				effectiveCost := g.buyPrice + g.buyPrice*buyTaxBps/10000
				remainBudget -= int64(qty * effectiveCost)
			}
		}

		destID := best.destID
		a.logger.Info("arbitrage opportunity",
			zap.Int("goods", len(buys)),
			zap.Int("capacity_used", req.Ship.Capacity-remaining-heldCapacity),
			zap.Int("destinations_evaluated", len(candidates)),
		)
		return &TradeDecision{
			Action:     "sell_and_buy",
			SellOrders: sells,
			BuyOrders:  buys,
			SailTo:     &destID,
			Reasoning:  fmt.Sprintf("arbitrage: best of %d destinations, profit/distance scoring", len(candidates)),
			Confidence: 0.8,
		}, nil
	}

	return a.speculativeTrade(req, sells, priceIndex, reachable, taxIndex, currentPortID, budget, speculativeEnabled, passengerWeight)
}

// decideBulkHaulerTrade scores destinations by total achievable profit
// (high-value goods, fill capacity). It evaluates all reachable destinations,
// computing the full ship-fill profit for each, then picks the best.
func (a *HeuristicAgent) decideBulkHaulerTrade(
	req TradeDecisionRequest,
	sells []SellOrder,
	priceIndex map[string]PricePoint,
	reachable map[uuid.UUID]float64,
	taxIndex map[uuid.UUID]int,
	currentPortID uuid.UUID,
	budget int64,
	passengerWeight float64,
	minMarginPct float64,
	speculativeEnabled bool,
	held []heldCargo,
	routeBonus map[uuid.UUID]float64,
	explorationBonus map[uuid.UUID]float64,
) (*TradeDecision, error) {
	buyTaxBps := taxIndex[currentPortID]
	capacity := req.Ship.Capacity
	now := time.Now()
	speed := math.Max(float64(req.Ship.Speed), 1.0)

	// Build claimed route set for anti-self-competition filtering.
	claimedSet := make(map[string]bool, len(req.ClaimedRoutes))
	for _, key := range req.ClaimedRoutes {
		claimedSet[key] = true
	}

	// Build passenger revenue index for destination scoring.
	// Include both available passengers (can board) and boarded passengers
	// (already on ship, must be delivered). Filter out passengers that
	// cannot be delivered before their deadline.
	passengerRevByDest := make(map[uuid.UUID]int)
	for _, p := range req.AvailablePassengers {
		dist, ok := reachable[p.DestinationPortID]
		if !ok {
			continue
		}
		if !p.ExpiresAt.IsZero() {
			travelMins := dist / speed
			if now.Add(time.Duration(travelMins*1.2) * time.Minute).After(p.ExpiresAt) {
				continue
			}
		}
		passengerRevByDest[p.DestinationPortID] += p.Bid
	}
	for _, p := range req.BoardedPassengers {
		// Boarded passengers are committed revenue — weight them heavily.
		// Add urgency multiplier for passengers near their deadline.
		weight := 2
		if !p.ExpiresAt.IsZero() {
			dist, ok := reachable[p.DestinationPortID]
			if !ok {
				continue
			}
			travelMins := dist / speed
			arrivalTime := now.Add(time.Duration(travelMins*1.2) * time.Minute)
			if arrivalTime.After(p.ExpiresAt) {
				weight = 1
			} else {
				remaining := p.ExpiresAt.Sub(now).Minutes()
				if remaining < travelMins*2 {
					weight = 5
				}
			}
		}
		passengerRevByDest[p.DestinationPortID] += p.Bid * weight
	}

	// Build held cargo bonus by destination.
	heldProfitByDest := make(map[uuid.UUID]int)
	for _, h := range held {
		heldProfitByDest[h.bestDestID] += h.profitGain
	}

	// Collect all profitable (good, destination) pairs.
	type goodOpp struct {
		goodID   uuid.UUID
		buyPrice int
		profit   int
		roi      float64
	}
	destGoods := make(map[uuid.UUID][]goodOpp)

	for _, pp := range req.PriceCache {
		if pp.PortID != currentPortID || pp.BuyPrice <= 0 {
			continue
		}
		buyTaxCost := pp.BuyPrice * buyTaxBps / 10000
		effectiveBuyCost := pp.BuyPrice + buyTaxCost
		for destID := range reachable {
			dp, ok := priceIndex[priceKey(destID, pp.GoodID)]
			if !ok || dp.SellPrice <= 0 {
				continue
			}
			// Skip routes claimed by other ships (anti-self-competition).
			rk := currentPortID.String() + ":" + destID.String() + ":" + pp.GoodID.String()
			if claimedSet[rk] {
				continue
			}
			sellTaxCost := dp.SellPrice * taxIndex[destID] / 10000
			profit := dp.SellPrice - pp.BuyPrice - buyTaxCost - sellTaxCost
			if profit < int(float64(pp.BuyPrice)*minMarginPct) {
				continue
			}
			roi := float64(profit) / float64(max(effectiveBuyCost, 1))
			destGoods[destID] = append(destGoods[destID], goodOpp{pp.GoodID, pp.BuyPrice, profit, roi})
		}
	}

	// Capacity available for new buys (subtract held cargo).
	heldCapacity := 0
	for _, h := range held {
		heldCapacity += h.quantity
	}
	buyCapacity := capacity - heldCapacity

	// Upkeep cost per minute for this ship (upkeep is per 5-hour cycle = 300 minutes).
	upkeepPerMin := float64(req.Ship.Upkeep) / 300.0

	// Score each destination by simulating a full ship fill.
	type destCandidate struct {
		destID uuid.UUID
		score  float64
		goods  []goodOpp
	}
	var candidates []destCandidate

	for destID, goods := range destGoods {
		// Deduplicate and sort goods by ROI descending (profit per gold invested).
		seen := make(map[uuid.UUID]bool)
		var unique []goodOpp
		for _, g := range goods {
			if !seen[g.goodID] {
				seen[g.goodID] = true
				unique = append(unique, g)
			}
		}
		sort.Slice(unique, func(i, j int) bool { return unique[i].roi > unique[j].roi })

		// Simulate greedy fill to compute total achievable profit.
		remaining := buyCapacity
		remainBudget := budget
		totalProfit := 0
		for _, g := range unique {
			if remaining <= 0 || remainBudget <= 0 {
				break
			}
			qty := a.calcQuantity(remainBudget, g.buyPrice, buyTaxBps, remaining)
			if qty > 0 {
				totalProfit += g.profit * qty
				remaining -= qty
				effectiveCost := g.buyPrice + g.buyPrice*buyTaxBps/10000
				remainBudget -= int64(qty * effectiveCost)
			}
		}

		dist := math.Max(reachable[destID], 1.0)
		// Score by profit-per-minute. Bulk hauler still favors absolute profit
		// but normalizes by trip time to penalize slow routes.
		travelMins := dist / speed
		totalTripMins := travelMins + 2.0
		travelUpkeep := upkeepPerMin * totalTripMins
		adjustedProfit := float64(totalProfit) - travelUpkeep

		// Bulk hauler: profit per minute (not per distance) to favor fast high-value routes.
		score := adjustedProfit / totalTripMins
		score += float64(passengerRevByDest[destID]) / totalTripMins * passengerWeight
		score += float64(heldProfitByDest[destID]) / totalTripMins
		score += routeBonus[destID] / math.Sqrt(totalTripMins)
		score += explorationBonus[destID] / math.Sqrt(totalTripMins)

		candidates = append(candidates, destCandidate{destID, score, unique})
	}

	// Add passenger-only destinations that have no cargo profit but do have
	// passenger revenue. These would otherwise be invisible to scoring.
	candidateSet := make(map[uuid.UUID]bool, len(candidates))
	for _, c := range candidates {
		candidateSet[c.destID] = true
	}
	for destID, passRev := range passengerRevByDest {
		if candidateSet[destID] || passRev <= 0 {
			continue
		}
		if _, ok := reachable[destID]; !ok {
			continue
		}
		dist := math.Max(reachable[destID], 1.0)
		totalTripMins := dist/speed + 2.0
		sqrtTrip := math.Sqrt(totalTripMins)
		score := float64(passRev) / totalTripMins * passengerWeight
		score += float64(heldProfitByDest[destID]) / totalTripMins
		score += routeBonus[destID] / sqrtTrip
		score += explorationBonus[destID] / sqrtTrip
		candidates = append(candidates, destCandidate{destID, score, nil})
	}

	// Add opportunity sell-port bonus from ProfitAnalyzer.
	oppSellBonus := a.buildOpportunitySellBonus(req.TopOpportunities, reachable)
	for i := range candidates {
		if bonus, ok := oppSellBonus[candidates[i].destID]; ok {
			candidates[i].score += bonus
		}
	}

	sort.Slice(candidates, func(i, j int) bool { return candidates[i].score > candidates[j].score })

	if len(candidates) > 0 {
		best := candidates[0]

		// Build buy orders for the winning destination.
		// Only buy goods with positive profit — skip loss-making cargo.
		remaining := buyCapacity
		remainBudget := budget
		var buys []BuyOrder
		for _, g := range best.goods {
			if remaining <= 0 || remainBudget <= 0 {
				break
			}
			if g.profit <= 0 {
				continue
			}
			qty := a.calcQuantity(remainBudget, g.buyPrice, buyTaxBps, remaining)
			if qty > 0 {
				buys = append(buys, BuyOrder{GoodID: g.goodID, Quantity: qty})
				remaining -= qty
				effectiveCost := g.buyPrice + g.buyPrice*buyTaxBps/10000
				remainBudget -= int64(qty * effectiveCost)
			}
		}

		destID := best.destID
		a.logger.Info("bulk hauler opportunity",
			zap.Int("goods", len(buys)),
			zap.Int("capacity_used", capacity-remaining-heldCapacity),
			zap.Int("destinations_evaluated", len(candidates)),
		)
		return &TradeDecision{
			Action:     "sell_and_buy",
			SellOrders: sells,
			BuyOrders:  buys,
			SailTo:     &destID,
			Reasoning:  fmt.Sprintf("bulk hauler: best of %d destinations, total profit scoring", len(candidates)),
			Confidence: 0.8,
		}, nil
	}

	return a.speculativeTrade(req, sells, priceIndex, reachable, taxIndex, currentPortID, budget, speculativeEnabled, passengerWeight)
}

// speculativeTrade handles the fallback when no clear arbitrage exists.
// Ships with passengers sail to the best passenger destination. Otherwise,
// ships check the ProfitAnalyzer for reachable buy ports with known profitable
// routes. After 2+ idle ticks, ships forcibly relocate to a hub port rather
// than sitting idle indefinitely.
//
// When sailing to any destination, the ship tries to buy goods at the current
// port that can be sold profitably at the destination — avoiding empty-sailing.
func (a *HeuristicAgent) speculativeTrade(
	req TradeDecisionRequest,
	sells []SellOrder,
	priceIndex map[string]PricePoint,
	reachable map[uuid.UUID]float64,
	taxIndex map[uuid.UUID]int,
	currentPortID uuid.UUID,
	budget int64,
	speculativeEnabled bool,
	passengerWeight float64,
) (*TradeDecision, error) {
	buyTaxBps := taxIndex[currentPortID]

	// Try to sail toward the best passenger revenue destination.
	bestPassDest, bestPassRev := a.bestPassengerDestination(req.AvailablePassengers, reachable, req.Ship.Speed)
	if bestPassRev > 0 {
		buys := a.speculativeBuys(req.Ship, priceIndex, taxIndex, currentPortID, bestPassDest, budget, buyTaxBps)
		a.logger.Info("no profitable trade, sailing toward passenger revenue",
			zap.Int("passenger_revenue", bestPassRev),
			zap.Int("speculative_buys", len(buys)),
		)
		return &TradeDecision{
			Action: "sell_and_buy", SellOrders: sells, BuyOrders: buys, SailTo: &bestPassDest,
			Reasoning: "no profitable trade, sailing to best passenger destination", Confidence: 0.5,
		}, nil
	}

	// Check ProfitAnalyzer: is there a reachable buy port with a profitable route?
	if dest, ok := a.findOpportunityBuyPort(req.TopOpportunities, reachable, currentPortID); ok {
		buys := a.speculativeBuys(req.Ship, priceIndex, taxIndex, currentPortID, dest, budget, buyTaxBps)
		a.logger.Info("no local trade, sailing to opportunity buy port",
			zap.String("dest_port", dest.String()),
			zap.Int("speculative_buys", len(buys)),
		)
		return &TradeDecision{
			Action: "sell_and_buy", SellOrders: sells, BuyOrders: buys, SailTo: &dest,
			Reasoning: "sailing to opportunity buy port from profit analyzer", Confidence: 0.5,
		}, nil
	}

	// Passenger ships (small cargo, has passenger slots) should spread across
	// ports for maximum passenger sniping coverage. They relocate after just
	// 1 idle tick and prefer ports where no other company ships are docked.
	if a.isPassengerShip(req.Ship) && req.Ship.IdleTicks >= 1 {
		if dest, ok := a.findPassengerRelocationPort(req.AllShips, req.Ports, reachable, currentPortID); ok {
			buys := a.speculativeBuys(req.Ship, priceIndex, taxIndex, currentPortID, dest, budget, buyTaxBps)
			a.logger.Info("passenger ship spreading to uncovered port",
				zap.String("ship", req.Ship.Name),
				zap.Int("idle_ticks", req.Ship.IdleTicks),
				zap.String("dest_port", dest.String()),
				zap.Int("speculative_buys", len(buys)),
			)
			return &TradeDecision{
				Action: "sell_and_buy", SellOrders: sells, BuyOrders: buys, SailTo: &dest,
				Reasoning: "passenger ship relocating to uncovered port for passenger sniping coverage",
				Confidence: 0.5,
			}, nil
		}
	}

	// After 2+ idle ticks, force relocation instead of sitting at a dead port.
	// Prefer hub ports (more trade variety) > opportunity sell ports > closest port.
	if req.Ship.IdleTicks >= 2 {
		if dest, ok := a.findRelocationPort(req.Ports, req.TopOpportunities, reachable, currentPortID); ok {
			buys := a.speculativeBuys(req.Ship, priceIndex, taxIndex, currentPortID, dest, budget, buyTaxBps)
			a.logger.Info("ship idle, relocating to better port",
				zap.Int("idle_ticks", req.Ship.IdleTicks),
				zap.String("dest_port", dest.String()),
				zap.Int("speculative_buys", len(buys)),
			)
			return &TradeDecision{
				Action: "sell_and_buy", SellOrders: sells, BuyOrders: buys, SailTo: &dest,
				Reasoning: fmt.Sprintf("idle %d ticks, relocating to port with more opportunities", req.Ship.IdleTicks),
				Confidence: 0.4,
			}, nil
		}
	}

	// First tick — wait to see if prices change on the next scan cycle.
	a.logger.Debug("no profitable opportunity, waiting at port (will relocate if idle persists)")
	return &TradeDecision{
		Action: "wait", SellOrders: sells,
		Reasoning: "no profitable trade — waiting briefly before relocating", Confidence: 0.3,
	}, nil
}

// speculativeBuys finds goods at the current port that sell profitably at
// the given destination. This prevents ships from sailing empty when
// relocating or chasing passengers/opportunities.
func (a *HeuristicAgent) speculativeBuys(
	ship ShipSnapshot,
	priceIndex map[string]PricePoint,
	taxIndex map[uuid.UUID]int,
	currentPortID, destID uuid.UUID,
	budget int64,
	buyTaxBps int,
) []BuyOrder {
	// Calculate remaining capacity.
	cargoUsed := 0
	for _, c := range ship.Cargo {
		cargoUsed += c.Quantity
	}
	remaining := ship.Capacity - cargoUsed
	if remaining <= 0 || budget <= 0 {
		return nil
	}

	destTaxBps := taxIndex[destID]

	// Find all goods available here that sell at a profit at the destination.
	type candidate struct {
		goodID uuid.UUID
		profit int // net profit per unit
		cost   int // effective buy cost per unit (with tax)
	}
	var candidates []candidate

	for _, pp := range priceIndex {
		if pp.PortID != currentPortID || pp.BuyPrice <= 0 {
			continue
		}
		dp, ok := priceIndex[priceKey(destID, pp.GoodID)]
		if !ok || dp.SellPrice <= 0 {
			continue
		}
		buyCost := pp.BuyPrice + pp.BuyPrice*buyTaxBps/10000
		sellNet := dp.SellPrice - dp.SellPrice*destTaxBps/10000
		profit := sellNet - buyCost
		if profit > 0 {
			candidates = append(candidates, candidate{
				goodID: pp.GoodID,
				profit: profit,
				cost:   buyCost,
			})
		}
	}

	if len(candidates) == 0 {
		return nil
	}

	// Sort by profit descending.
	sort.Slice(candidates, func(i, j int) bool {
		return candidates[i].profit > candidates[j].profit
	})

	var buys []BuyOrder
	for _, c := range candidates {
		if remaining <= 0 || budget <= 0 {
			break
		}
		qty := a.calcQuantity(budget, c.cost, 0, remaining) // tax already in cost
		if qty <= 0 {
			continue
		}
		buys = append(buys, BuyOrder{GoodID: c.goodID, Quantity: qty})
		remaining -= qty
		budget -= int64(qty) * int64(c.cost)
	}
	return buys
}

// ---------------------------------------------------------------------------
// Fleet Decisions
// ---------------------------------------------------------------------------

// upkeepCycleHours is the interval at which the game charges upkeep.
// The API returns per-cycle amounts, so multiply by this to convert to hourly.
const upkeepCycleHours int64 = 5

// reserveCycles returns a flat 3-cycle (15h) reserve for all strategies.
// V2 scales aggressively — revenue tracking handles safety instead of
// conservative reserves that cap fleet growth.
func reserveCycles(_ int, _ string) int64 {
	return 3
}

// maxFleetSize returns the hard fleet cap per strategy.
func maxFleetSize(strategy string) int {
	switch strategy {
	case "bulk_hauler":
		return 10
	case "passenger_sniper":
		return 12
	default: // arbitrage
		return 15
	}
}

// DecideFleetAction decides whether to buy or sell ships, or buy warehouses.
// V2: aggressive scaling with flat 3-cycle reserve, multi-ship purchases,
// strategy-specific fleet caps, and warehouse scaling (grow/shrink/demolish).
//
// Ship type preferences by strategy:
//
//	Arbitrage        → fastest ships with passenger slots, max 15
//	BulkHauler       → largest capacity, max 10
//	PassengerSniper  → cheapest with passenger slots, max 12
func (a *HeuristicAgent) DecideFleetAction(_ context.Context, req FleetDecisionRequest) (*FleetDecision, error) {
	treasury := req.Company.Treasury
	upkeep := req.Company.TotalUpkeep
	numShips := len(req.Ships)
	fleetCap := maxFleetSize(req.StrategyHint)

	// --- WAREHOUSE SCALING (grow/shrink/demolish) ---
	// Only for non-passenger strategies with 3+ ships and profitable history.
	if req.StrategyHint != "passenger_sniper" && numShips >= 3 && len(req.Warehouses) > 0 {
		actions := a.evaluateWarehouseScaling(req)
		if len(actions) > 0 {
			return &FleetDecision{
				WarehouseActions: actions,
				Reasoning:        "warehouse scaling based on utilization and trade activity",
			}, nil
		}
	}

	// --- WAREHOUSE PURCHASE ---
	// Buy at high-traffic ports. Max 3 warehouses per company.
	// Never buy for passenger_sniper.
	if req.StrategyHint != "passenger_sniper" && len(req.Warehouses) < 3 && numShips >= 3 {
		// Check that total warehouse upkeep < 15% of trailing revenue
		// (approximated by treasury health).
		warehouseAffordable := treasury > upkeep*5
		if warehouseAffordable {
			portActivity := make(map[uuid.UUID]int)
			for _, ship := range req.Ships {
				if ship.Status == "docked" && ship.PortID != nil {
					portActivity[*ship.PortID]++
				}
			}
			for _, entry := range req.RouteHistory {
				portActivity[entry.FromPortID]++
				portActivity[entry.ToPortID]++
			}

			warehousePorts := make(map[uuid.UUID]bool)
			for _, w := range req.Warehouses {
				warehousePorts[w.PortID] = true
			}

			var bestPort uuid.UUID
			bestCount := 0
			for portID, count := range portActivity {
				if !warehousePorts[portID] && count > bestCount {
					bestCount = count
					bestPort = portID
				}
			}

			if bestPort != uuid.Nil && bestCount >= 2 {
				a.logger.Info("recommending warehouse purchase",
					zap.Int("port_activity_score", bestCount),
					zap.Int("existing_warehouses", len(req.Warehouses)),
				)
				return &FleetDecision{
					BuyWarehouses: []uuid.UUID{bestPort},
					Reasoning:     "buying warehouse at high-activity port",
				}, nil
			}
		}
	}

	// --- SHIP SALE ---
	// Emergency downsize: treasury < 2 cycles of upkeep.
	reserve := reserveCycles(numShips, req.StrategyHint)
	if numShips > 1 && upkeep > 0 && treasury > 0 && treasury < upkeep*reserve {
		sellShips := a.findShipsToSell(req.Ships, req.StrategyHint, req.ShipyardPorts)
		if len(sellShips) > 0 {
			cyclesCovered := treasury / max(upkeep, 1)
			a.logger.Info("recommending ship decommission — upkeep too high",
				zap.Int64("treasury", treasury),
				zap.Int64("upkeep_per_cycle", upkeep),
				zap.Int64("reserve_cycles", reserve),
				zap.Int64("hours_covered", cyclesCovered*upkeepCycleHours),
				zap.Int("ships_to_sell", len(sellShips)),
			)
			return &FleetDecision{
				SellShips: sellShips,
				Reasoning: fmt.Sprintf("decommissioning: treasury covers only %dh of upkeep (need %dh)", cyclesCovered*upkeepCycleHours, reserve*upkeepCycleHours),
			}, nil
		}
	}

	// --- FLEET CAP CHECK ---
	if numShips >= fleetCap {
		return &FleetDecision{
			Reasoning: fmt.Sprintf("fleet at cap (%d/%d ships)", numShips, fleetCap),
		}, nil
	}

	if len(req.ShipTypes) == 0 {
		return &FleetDecision{Reasoning: "no ship types available"}, nil
	}

	// --- SHIP TYPE SELECTION ---
	shipTypes := make([]ShipTypeInfo, len(req.ShipTypes))
	copy(shipTypes, req.ShipTypes)

	var targetShipType ShipTypeInfo
	switch req.StrategyHint {
	case "bulk_hauler":
		sort.Slice(shipTypes, func(i, j int) bool {
			if shipTypes[i].Capacity != shipTypes[j].Capacity {
				return shipTypes[i].Capacity > shipTypes[j].Capacity
			}
			return shipTypes[i].PassengerCap > shipTypes[j].PassengerCap
		})
		targetShipType = shipTypes[0]

	case "passenger_sniper":
		// Cheapest ships with passenger slots — minimize upkeep.
		sort.Slice(shipTypes, func(i, j int) bool {
			iPax := shipTypes[i].PassengerCap > 0
			jPax := shipTypes[j].PassengerCap > 0
			if iPax != jPax {
				return iPax // passenger-capable first
			}
			return shipTypes[i].Upkeep < shipTypes[j].Upkeep
		})
		targetShipType = shipTypes[0]

	case "arbitrage":
		sort.Slice(shipTypes, func(i, j int) bool {
			si := float64(shipTypes[i].Speed) + float64(shipTypes[i].PassengerCap)/5.0
			sj := float64(shipTypes[j].Speed) + float64(shipTypes[j].PassengerCap)/5.0
			return si > sj
		})
		targetShipType = shipTypes[0]

	default:
		sort.Slice(shipTypes, func(i, j int) bool {
			return shipTypes[i].BasePrice < shipTypes[j].BasePrice
		})
		targetShipType = shipTypes[0]
	}

	// --- AFFORDABILITY CHECK ---
	newReserve := reserveCycles(numShips+1, req.StrategyHint)

	portTaxIndex := make(map[uuid.UUID]int, len(req.Ports))
	for _, p := range req.Ports {
		portTaxIndex[p.ID] = p.TaxRateBps
	}
	purchasePortID := a.findPurchasePort(req.Ships, req.ShipyardPorts)
	purchaseTaxBps := portTaxIndex[purchasePortID]

	canAfford := func(st ShipTypeInfo, nShips int) bool {
		totalNewUpkeep := upkeep
		for k := 0; k < nShips; k++ {
			totalNewUpkeep += int64(st.Upkeep)
		}
		totalShipCost := int64(nShips) * (int64(st.BasePrice) + int64(st.BasePrice)*int64(purchaseTaxBps)/10000)
		required := totalShipCost + totalNewUpkeep*newReserve
		return treasury >= required
	}

	if !canAfford(targetShipType, 1) {
		sort.Slice(shipTypes, func(i, j int) bool {
			return shipTypes[i].BasePrice < shipTypes[j].BasePrice
		})
		found := false
		for _, st := range shipTypes {
			if canAfford(st, 1) {
				targetShipType = st
				found = true
				break
			}
		}
		if !found {
			newUpkeep := upkeep + int64(shipTypes[0].Upkeep)
			required := int64(shipTypes[0].BasePrice) + newUpkeep*newReserve
			return &FleetDecision{
				Reasoning: fmt.Sprintf("treasury %d too low: cheapest ship needs %d (price %d + %d-cycle reserve of %d/cycle upkeep)",
					treasury, required, shipTypes[0].BasePrice, newReserve, newUpkeep),
			}, nil
		}
	}

	if purchasePortID == uuid.Nil {
		return &FleetDecision{Reasoning: "no suitable port found for ship purchase"}, nil
	}

	// --- MULTI-SHIP PURCHASE ---
	// Buy up to 3 ships per eval (startup) or 2 (growth), respecting fleet cap.
	maxBuy := 2
	if numShips < 5 {
		maxBuy = 3 // Rapid scaling during startup
	}
	remaining := fleetCap - numShips
	if maxBuy > remaining {
		maxBuy = remaining
	}

	var purchases []ShipPurchase
	for i := 0; i < maxBuy; i++ {
		if !canAfford(targetShipType, i+1) {
			break
		}
		purchases = append(purchases, ShipPurchase{
			ShipTypeID: targetShipType.ID,
			PortID:     purchasePortID,
		})
	}

	if len(purchases) == 0 {
		return &FleetDecision{Reasoning: "cannot afford any ships after reserve"}, nil
	}

	newUpkeep := upkeep + int64(len(purchases))*int64(targetShipType.Upkeep)
	totalCost := int64(len(purchases)) * (int64(targetShipType.BasePrice) + int64(targetShipType.BasePrice)*int64(purchaseTaxBps)/10000)
	cyclesCovered := (treasury - totalCost) / max(newUpkeep, 1)

	a.logger.Info("recommending ship purchase",
		zap.String("strategy", req.StrategyHint),
		zap.String("ship_type", targetShipType.Name),
		zap.Int("quantity", len(purchases)),
		zap.Int("price_each", targetShipType.BasePrice),
		zap.Int("current_fleet", numShips),
		zap.Int("fleet_cap", fleetCap),
		zap.Int64("treasury_after", treasury-totalCost),
		zap.Int64("new_upkeep_per_cycle", newUpkeep),
	)

	return &FleetDecision{
		BuyShips:  purchases,
		Reasoning: fmt.Sprintf("expanding fleet %d→%d ships (%dx %s), treasury covers %dh of new upkeep",
			numShips, numShips+len(purchases), len(purchases), targetShipType.Name, cyclesCovered*upkeepCycleHours),
	}, nil
}

// evaluateWarehouseScaling checks existing warehouses for grow/shrink/demolish actions.
func (a *HeuristicAgent) evaluateWarehouseScaling(req FleetDecisionRequest) []WarehouseAction {
	if len(req.Warehouses) == 0 {
		return nil
	}

	// Build port trade frequency from route history.
	portTradeCount := make(map[uuid.UUID]int)
	for _, entry := range req.RouteHistory {
		portTradeCount[entry.FromPortID]++
		portTradeCount[entry.ToPortID]++
	}

	var actions []WarehouseAction
	for _, wh := range req.Warehouses {
		trades := portTradeCount[wh.PortID]

		// Demolish: no trades at this port in route history and warehouse is empty.
		totalItems := 0
		for _, item := range wh.Items {
			totalItems += item.Quantity
		}
		if trades == 0 && totalItems == 0 {
			actions = append(actions, WarehouseAction{
				WarehouseID: wh.ID,
				Action:      "demolish",
			})
			continue
		}

		// Grow: high utilization + active port.
		utilization := float64(totalItems) / math.Max(float64(wh.Capacity), 1.0)
		if utilization > 0.7 && trades >= 5 {
			actions = append(actions, WarehouseAction{
				WarehouseID: wh.ID,
				Action:      "grow",
			})
		}

		// Shrink: very low utilization at inactive port.
		if utilization < 0.2 && trades <= 2 && wh.Level > 1 {
			actions = append(actions, WarehouseAction{
				WarehouseID: wh.ID,
				Action:      "shrink",
			})
		}
	}

	return actions
}

// ---------------------------------------------------------------------------
// Market Decisions (P2P)
// ---------------------------------------------------------------------------

// DecideMarketAction evaluates the P2P market for profitable opportunities.
// It looks for mispriced orders relative to NPC prices and fills them,
// and posts orders at favorable spreads.
func (a *HeuristicAgent) DecideMarketAction(_ context.Context, req MarketDecisionRequest) (*MarketDecision, error) {
	if len(req.PriceCache) == 0 {
		return &MarketDecision{Reasoning: "no price data yet, waiting for scanner"}, nil
	}

	treasury := req.Company.Treasury
	upkeep := req.Company.TotalUpkeep
	floor := upkeep * 2
	available := treasury - floor
	if available <= 0 {
		return &MarketDecision{Reasoning: "treasury at floor, skipping market activity"}, nil
	}

	priceIndex := a.buildPriceIndex(req.PriceCache)

	// Build warehouse port index so we only fill orders at ports where we have a warehouse.
	warehousePorts := make(map[uuid.UUID]bool, len(req.Warehouses))
	for _, w := range req.Warehouses {
		warehousePorts[w.PortID] = true
	}

	// Track own order IDs to avoid filling our own orders.
	ownOrderIDs := make(map[uuid.UUID]bool, len(req.OwnOrders))
	for _, o := range req.OwnOrders {
		ownOrderIDs[o.ID] = true
	}

	// Step 1: Find other players' mispriced orders to fill.
	var fills []FillOrder
	var totalFillCost int64

	for _, order := range req.OpenOrders {
		if ownOrderIDs[order.ID] {
			continue // Don't fill our own orders.
		}
		if order.Remaining <= 0 {
			continue
		}
		if !warehousePorts[order.PortID] {
			continue // Need a warehouse at the order's port to fill it.
		}

		npcPrice, ok := priceIndex[priceKey(order.PortID, order.GoodID)]
		if !ok {
			continue
		}

		switch order.Side {
		case "sell":
			// Player selling — we buy if their price is below NPC sell price (we can resell to NPC).
			if npcPrice.SellPrice > 0 && order.Price < npcPrice.SellPrice {
				profit := npcPrice.SellPrice - order.Price
				minProfit := npcPrice.SellPrice / 10 // At least 10% margin.
				if profit >= minProfit {
					qty := order.Remaining
					cost := int64(qty * order.Price)
					if cost > available-totalFillCost {
						qty = int((available - totalFillCost) / int64(order.Price))
					}
					if qty > 0 {
						fills = append(fills, FillOrder{OrderID: order.ID, Quantity: qty})
						totalFillCost += int64(qty * order.Price)
						a.logger.Debug("filling underpriced sell order",
							zap.Int("order_price", order.Price),
							zap.Int("npc_sell", npcPrice.SellPrice),
							zap.Int("profit/unit", profit),
							zap.Int("qty", qty),
						)
					}
				}
			}

		case "buy":
			// Player buying — we sell if their price is above NPC buy price (we buy from NPC and sell to them).
			if npcPrice.BuyPrice > 0 && order.Price > npcPrice.BuyPrice {
				profit := order.Price - npcPrice.BuyPrice
				minProfit := npcPrice.BuyPrice * 7 / 100 // 7% min margin
				if profit >= minProfit {
					qty := order.Remaining
					cost := int64(qty * npcPrice.BuyPrice)
					if cost > available-totalFillCost {
						qty = int((available - totalFillCost) / int64(npcPrice.BuyPrice))
					}
					if qty > 0 {
						fills = append(fills, FillOrder{OrderID: order.ID, Quantity: qty})
						totalFillCost += int64(qty * npcPrice.BuyPrice)
						a.logger.Debug("filling overpriced buy order",
							zap.Int("order_price", order.Price),
							zap.Int("npc_buy", npcPrice.BuyPrice),
							zap.Int("profit/unit", profit),
							zap.Int("qty", qty),
						)
					}
				}
			}
		}
	}

	// Step 2: Cancel own orders that are no longer profitable.
	var cancels []uuid.UUID
	for _, order := range req.OwnOrders {
		npcPrice, ok := priceIndex[priceKey(order.PortID, order.GoodID)]
		if !ok {
			continue
		}
		switch order.Side {
		case "sell":
			// Cancel if NPC sell price dropped below our ask.
			if npcPrice.SellPrice > 0 && order.Price > npcPrice.SellPrice {
				cancels = append(cancels, order.ID)
			}
		case "buy":
			// Cancel if NPC buy price rose above our bid.
			if npcPrice.BuyPrice > 0 && order.Price < npcPrice.BuyPrice {
				cancels = append(cancels, order.ID)
			}
		}
	}

	// Step 3: Post new orders at favorable spreads (only if we have few active orders).
	var posts []NewMarketOrder
	if len(req.OwnOrders) < 5 && available-totalFillCost > 0 {
		// Find goods with wide NPC bid-ask spreads and post in the middle.
		type spreadOpp struct {
			portID   uuid.UUID
			goodID   uuid.UUID
			buyPrice int
			sellPrice int
			spread   int
		}
		var spreads []spreadOpp

		for _, pp := range req.PriceCache {
			if !warehousePorts[pp.PortID] {
				continue // Need a warehouse at the port to post orders.
			}
			if pp.BuyPrice > 0 && pp.SellPrice > 0 && pp.SellPrice > pp.BuyPrice {
				spread := pp.SellPrice - pp.BuyPrice
				if spread > pp.BuyPrice/5 { // At least 20% spread.
					spreads = append(spreads, spreadOpp{pp.PortID, pp.GoodID, pp.BuyPrice, pp.SellPrice, spread})
				}
			}
		}

		sort.Slice(spreads, func(i, j int) bool { return spreads[i].spread > spreads[j].spread })

		// Post up to 2 orders.
		for i, sp := range spreads {
			if i >= 2 {
				break
			}
			// Post a buy order slightly above NPC buy price.
			bidPrice := sp.buyPrice + sp.spread/4
			qty := int((available - totalFillCost) / int64(bidPrice))
			if qty > 20 {
				qty = 20
			}
			if qty > 0 {
				posts = append(posts, NewMarketOrder{
					PortID: sp.portID,
					GoodID: sp.goodID,
					Side:   "buy",
					Price:  bidPrice,
					Total:  qty,
				})
				a.logger.Debug("posting market buy order",
					zap.Int("bid", bidPrice),
					zap.Int("npc_buy", sp.buyPrice),
					zap.Int("npc_sell", sp.sellPrice),
					zap.Int("qty", qty),
				)
			}
		}
	}

	if len(fills) == 0 && len(posts) == 0 && len(cancels) == 0 {
		return &MarketDecision{Reasoning: "no profitable P2P opportunities found"}, nil
	}

	return &MarketDecision{
		FillOrders:   fills,
		PostOrders:   posts,
		CancelOrders: cancels,
		Reasoning:    "market making: filling mispriced orders and posting at favorable spreads",
	}, nil
}

// ---------------------------------------------------------------------------
// Strategy Evaluation
// ---------------------------------------------------------------------------

// EvaluateStrategy analyzes performance metrics and recommends strategy changes.
func (a *HeuristicAgent) EvaluateStrategy(_ context.Context, req StrategyEvalRequest) (*StrategyEvaluation, error) {
	if len(req.Metrics) < 2 {
		return &StrategyEvaluation{Reasoning: "not enough strategies to compare"}, nil
	}

	// Find best and worst by profit per hour.
	best := req.Metrics[0]
	worst := req.Metrics[0]
	for _, m := range req.Metrics[1:] {
		if m.ProfitPerHour > best.ProfitPerHour {
			best = m
		}
		if m.ProfitPerHour < worst.ProfitPerHour {
			worst = m
		}
	}

	// Don't recommend switches when both strategies are barely active.
	// With a 30-minute lookback, <3 trades means the data is too noisy.
	if best.TradesExecuted < 3 && worst.TradesExecuted < 3 {
		return &StrategyEvaluation{Reasoning: "insufficient trade data to evaluate strategies"}, nil
	}

	// Only recommend switching away from a losing strategy if:
	// 1. It's meaningfully negative (not just -1 gold/hour noise)
	// 2. Best is actually positive (switching to another loser doesn't help)
	// 3. The difference is significant (best is at least 2x better by absolute gap)
	if worst.ProfitPerHour < -100 && best.ProfitPerHour > 0 &&
		worst.StrategyName != best.StrategyName {
		switchTo := best.StrategyName
		return &StrategyEvaluation{
			SwitchTo:  &switchTo,
			Reasoning: fmt.Sprintf("%s is significantly losing money (%.0f/hr), switching to %s (%.0f/hr)",
				worst.StrategyName, worst.ProfitPerHour, best.StrategyName, best.ProfitPerHour),
		}, nil
	}

	// If best is 2x better than worst and both are positive, recommend switch.
	if best.ProfitPerHour > 0 && worst.ProfitPerHour > 0 &&
		best.ProfitPerHour > worst.ProfitPerHour*2.0 &&
		worst.StrategyName != best.StrategyName {
		switchTo := best.StrategyName
		return &StrategyEvaluation{
			SwitchTo:  &switchTo,
			Reasoning: fmt.Sprintf("%s outperforms %s by 2x+ (%.0f vs %.0f/hr)",
				best.StrategyName, worst.StrategyName, best.ProfitPerHour, worst.ProfitPerHour),
		}, nil
	}

	return &StrategyEvaluation{Reasoning: "all strategies performing within acceptable range"}, nil
}

// ---------------------------------------------------------------------------
// P2P Order Fill Scanning (used by trade decisions)
// ---------------------------------------------------------------------------

// findProfitableOrderFills scans P2P orders at the current port for fills
// that are more profitable than NPC prices. Returns fill instructions and
// the total cost committed to fills (deducted from the trade budget).
func (a *HeuristicAgent) findProfitableOrderFills(
	portOrders []MarketOrder,
	ownOrders []MarketOrder,
	priceIndex map[string]PricePoint,
	taxIndex map[uuid.UUID]int,
	currentPortID uuid.UUID,
	budget int64,
) ([]FillOrder, int64) {
	if len(portOrders) == 0 || budget <= 0 {
		return nil, 0
	}

	ownOrderIDs := make(map[uuid.UUID]bool, len(ownOrders))
	for _, o := range ownOrders {
		ownOrderIDs[o.ID] = true
	}

	// Score each order by profit margin and sort best-first.
	type scoredFill struct {
		orderID uuid.UUID
		side    string
		qty     int
		cost    int64 // how much budget this fill consumes
		profit  int   // profit per unit
	}
	var candidates []scoredFill

	portTaxBps := taxIndex[currentPortID]

	for _, order := range portOrders {
		if ownOrderIDs[order.ID] || order.Remaining <= 0 {
			continue
		}
		if order.PortID != currentPortID {
			continue
		}

		npcPrice, ok := priceIndex[priceKey(order.PortID, order.GoodID)]
		if !ok {
			continue
		}

		switch order.Side {
		case "sell":
			// Player selling cheap → we buy from them, then sell to NPC.
			// Profit = NPC sell price - order price - sell tax (we pay tax when selling to NPC).
			if npcPrice.SellPrice > 0 && order.Price < npcPrice.SellPrice {
				sellTax := npcPrice.SellPrice * portTaxBps / 10000
				profit := npcPrice.SellPrice - order.Price - sellTax
				minProfit := npcPrice.SellPrice * 5 / 100 // 5% min margin
				if profit >= minProfit {
					candidates = append(candidates, scoredFill{
						orderID: order.ID,
						side:    "sell",
						qty:     order.Remaining,
						cost:    int64(order.Remaining * order.Price),
						profit:  profit,
					})
				}
			}
		case "buy":
			// Player buying expensive → we buy from NPC, sell to them.
			// Profit = order price - NPC buy price - buy tax (we pay tax when buying from NPC).
			if npcPrice.BuyPrice > 0 && order.Price > npcPrice.BuyPrice {
				buyTax := npcPrice.BuyPrice * portTaxBps / 10000
				profit := order.Price - npcPrice.BuyPrice - buyTax
				minProfit := npcPrice.BuyPrice * 5 / 100 // 5% min margin
				if profit >= minProfit {
					candidates = append(candidates, scoredFill{
						orderID: order.ID,
						side:    "buy",
						qty:     order.Remaining,
						cost:    int64(order.Remaining * (npcPrice.BuyPrice + buyTax)),
						profit:  profit,
					})
				}
			}
		}
	}

	// Sort by profit per unit descending.
	sort.Slice(candidates, func(i, j int) bool {
		return candidates[i].profit > candidates[j].profit
	})

	var fills []FillOrder
	var totalCost int64

	for _, c := range candidates {
		remaining := budget - totalCost
		if remaining <= 0 {
			break
		}
		qty := c.qty
		unitCost := c.cost / int64(c.qty)
		if unitCost > 0 && int64(qty)*unitCost > remaining {
			qty = int(remaining / unitCost)
		}
		if qty > 0 {
			fills = append(fills, FillOrder{OrderID: c.orderID, Quantity: qty})
			totalCost += int64(qty) * unitCost
			a.logger.Debug("trade fill opportunity",
				zap.String("side", c.side),
				zap.Int("profit/unit", c.profit),
				zap.Int("qty", qty),
			)
		}
	}

	return fills, totalCost
}

// ---------------------------------------------------------------------------
// Helpers
// ---------------------------------------------------------------------------

// buildSmartSellOrders decides which cargo to sell at the current port vs hold
// for a better destination. Cargo is held only when a reachable destination
// offers >20% better net sell price after taxes.
func (a *HeuristicAgent) buildSmartSellOrders(
	ship ShipSnapshot,
	priceIndex map[string]PricePoint,
	reachable map[uuid.UUID]float64,
	taxIndex map[uuid.UUID]int,
	currentPortID uuid.UUID,
) ([]SellOrder, []heldCargo) {
	if len(ship.Cargo) == 0 {
		return nil, nil
	}

	var sells []SellOrder
	var held []heldCargo
	currentTaxBps := taxIndex[currentPortID]

	for _, cargo := range ship.Cargo {
		if cargo.Quantity <= 0 {
			continue
		}

		// Net sell price at current port.
		currentNet := 0
		if pp, ok := priceIndex[priceKey(currentPortID, cargo.GoodID)]; ok && pp.SellPrice > 0 {
			tax := pp.SellPrice * currentTaxBps / 10000
			currentNet = pp.SellPrice - tax
		}

		// Find best reachable destination for this good.
		bestDestID := uuid.Nil
		bestDestNet := 0
		for destID := range reachable {
			dp, ok := priceIndex[priceKey(destID, cargo.GoodID)]
			if !ok || dp.SellPrice <= 0 {
				continue
			}
			destTax := dp.SellPrice * taxIndex[destID] / 10000
			net := dp.SellPrice - destTax
			if net > bestDestNet {
				bestDestNet = net
				bestDestID = destID
			}
		}

		// Hold if a destination offers >20% better net price AFTER subtracting
		// travel upkeep cost. Without this, ships hold cargo for far-away ports
		// where the upkeep to get there eats all the price improvement.
		if currentNet > 0 && bestDestNet > currentNet && bestDestID != uuid.Nil {
			gainPerUnit := bestDestNet - currentNet
			// Subtract travel upkeep from the total hold gain estimate.
			travelUpkeep := 0
			if dist, ok := reachable[bestDestID]; ok && ship.Speed > 0 && ship.Upkeep > 0 {
				travelMins := dist / float64(ship.Speed)
				travelUpkeep = int(float64(ship.Upkeep) / 300.0 * travelMins)
			}
			totalGain := gainPerUnit*cargo.Quantity - travelUpkeep
			// Only hold if the net gain (after upkeep) exceeds 20% of selling now.
			if totalGain > currentNet*cargo.Quantity*20/100 {
				held = append(held, heldCargo{
					goodID:     cargo.GoodID,
					quantity:   cargo.Quantity,
					bestDestID: bestDestID,
					profitGain: totalGain,
				})
				continue
			}
		}

		// Sell at current port (includes cargo with no sell price — execution layer handles it).
		sells = append(sells, SellOrder{GoodID: cargo.GoodID, Quantity: cargo.Quantity})
	}

	return sells, held
}

func (a *HeuristicAgent) buildSellOrders(ship ShipSnapshot) []SellOrder {
	var sells []SellOrder
	for _, cargo := range ship.Cargo {
		if cargo.Quantity > 0 {
			sells = append(sells, SellOrder{GoodID: cargo.GoodID, Quantity: cargo.Quantity})
		}
	}
	return sells
}

// buildRouteHistoryBonus computes a scoring bonus for each destination based
// on historical route performance. Routes with positive average profit get a
// bonus, routes with losses get a penalty. The bonus is capped to prevent
// established routes from permanently dominating over unexplored ones.
func (a *HeuristicAgent) buildRouteHistoryBonus(history []RoutePerformanceEntry, fromPortID uuid.UUID) map[uuid.UUID]float64 {
	bonus := make(map[uuid.UUID]float64)
	if len(history) == 0 {
		return bonus
	}

	type stats struct {
		totalProfit int
		count       int
	}
	byDest := make(map[uuid.UUID]*stats)

	for _, rp := range history {
		if rp.FromPortID != fromPortID {
			continue
		}
		s, ok := byDest[rp.ToPortID]
		if !ok {
			s = &stats{}
			byDest[rp.ToPortID] = s
		}
		s.totalProfit += rp.Profit
		s.count++
	}

	for destID, s := range byDest {
		if s.count > 0 {
			avg := float64(s.totalProfit) / float64(s.count)
			// Cap the bonus so established routes can't overwhelm new ones.
			// Allow negative penalties to be uncapped (avoid loss routes).
			const maxRouteBonus = 500.0
			if avg > maxRouteBonus {
				avg = maxRouteBonus
			}
			bonus[destID] = avg
		}
	}

	return bonus
}

// buildExplorationBonus computes a bonus for ports that the company has not
// visited recently. This encourages ships to explore new/distant ports instead
// of always clustering at familiar nearby ones. Ports with zero route history
// entries get the full exploration bonus; ports visited fewer times get a
// partial bonus that decays with visit count.
func (a *HeuristicAgent) buildExplorationBonus(
	history []RoutePerformanceEntry,
	reachable map[uuid.UUID]float64,
	fromPortID uuid.UUID,
) map[uuid.UUID]float64 {
	// Count visits to each destination from the current port.
	visitCount := make(map[uuid.UUID]int)
	for _, rp := range history {
		if rp.FromPortID == fromPortID {
			visitCount[rp.ToPortID]++
		}
	}

	bonus := make(map[uuid.UUID]float64, len(reachable))
	const explorationBonus = 200.0 // Base bonus for completely unvisited ports.

	for destID := range reachable {
		visits := visitCount[destID]
		if visits == 0 {
			// Never visited: full exploration bonus.
			bonus[destID] = explorationBonus
		} else if visits < 5 {
			// Rarely visited: decaying partial bonus.
			bonus[destID] = explorationBonus / float64(visits+1)
		}
		// 5+ visits: no exploration bonus (well-explored).
	}

	return bonus
}

func (a *HeuristicAgent) buildPriceIndex(cache []PricePoint) map[string]PricePoint {
	idx := make(map[string]PricePoint, len(cache))
	for _, pp := range cache {
		idx[priceKey(pp.PortID, pp.GoodID)] = pp
	}
	return idx
}

func (a *HeuristicAgent) reachablePorts(routes []RouteInfo, from uuid.UUID) map[uuid.UUID]float64 {
	r := make(map[uuid.UUID]float64)
	for _, route := range routes {
		if route.FromID == from {
			r[route.ToID] = route.Distance
		} else if route.ToID == from {
			// Routes are bidirectional with equal distance; use reverse
			// direction as fallback when the forward entry is missing
			// from the cached route set.
			if _, exists := r[route.FromID]; !exists {
				r[route.FromID] = route.Distance
			}
		}
	}
	return r
}

func (a *HeuristicAgent) closestPort(reachable map[uuid.UUID]float64) uuid.UUID {
	var bestID uuid.UUID
	bestDist := math.MaxFloat64
	for id, dist := range reachable {
		if dist < bestDist {
			bestDist = dist
			bestID = id
		}
	}
	return bestID
}

// calcQuantity calculates how many units can be bought given budget, unit price,
// buy tax rate (in bps), and maximum capacity. Tax is factored into the cost.
func (a *HeuristicAgent) calcQuantity(budget int64, unitPrice int, taxBps int, maxQty int) int {
	if unitPrice <= 0 {
		return 0
	}
	effectiveCost := unitPrice + unitPrice*taxBps/10000
	if effectiveCost <= 0 {
		return 0
	}
	affordable := int(budget / int64(effectiveCost))
	if affordable < maxQty {
		maxQty = affordable
	}
	if maxQty < 1 {
		return 0
	}
	return maxQty
}

func (a *HeuristicAgent) findPurchasePort(ships []ShipSnapshot, shipyardPorts []uuid.UUID) uuid.UUID {
	if len(shipyardPorts) == 0 {
		return uuid.Nil
	}

	// Build shipyard set for fast lookup.
	shipyardSet := make(map[uuid.UUID]bool, len(shipyardPorts))
	for _, id := range shipyardPorts {
		shipyardSet[id] = true
	}

	// Prefer a shipyard port where we already have a docked ship.
	for _, ship := range ships {
		if ship.Status == "docked" && ship.PortID != nil && shipyardSet[*ship.PortID] {
			return *ship.PortID
		}
	}

	// Fall back to the first known shipyard port.
	return shipyardPorts[0]
}

// findShipsToSell identifies the least valuable ship to decommission.
// For arbitrage: sell the slowest ship. For bulk_hauler: sell the smallest.
// For market_maker: sell the most expensive (highest upkeep).
// Only considers docked ships with no cargo that are at a port with a shipyard.
func (a *HeuristicAgent) findShipsToSell(ships []ShipSnapshot, strategy string, shipyardPorts []uuid.UUID) []uuid.UUID {
	shipyardSet := make(map[uuid.UUID]bool, len(shipyardPorts))
	for _, pid := range shipyardPorts {
		shipyardSet[pid] = true
	}

	type candidate struct {
		id    uuid.UUID
		score float64 // lower = more likely to sell
	}
	var candidates []candidate

	for _, ship := range ships {
		if ship.Status != "docked" {
			continue
		}
		// Don't sell ships with cargo — they're actively trading.
		if len(ship.Cargo) > 0 {
			continue
		}
		// Only consider ships at ports with a shipyard.
		if ship.PortID == nil || !shipyardSet[*ship.PortID] {
			continue
		}

		var score float64
		switch strategy {
		case "arbitrage":
			score = float64(ship.Speed) // Keep fast ships, sell slow ones.
		case "bulk_hauler":
			score = float64(ship.Capacity) // Keep big ships, sell small ones.
		case "passenger_sniper":
			// Keep ships with lowest upkeep, sell expensive ones.
			score = -float64(ship.Upkeep)
		default:
			// Market maker: keep cheap ships, sell expensive ones.
			score = -float64(ship.Capacity)
		}
		candidates = append(candidates, candidate{id: ship.ID, score: score})
	}

	if len(candidates) == 0 {
		return nil
	}

	// Sort ascending by score — first candidate is the worst ship.
	sort.Slice(candidates, func(i, j int) bool {
		return candidates[i].score < candidates[j].score
	})

	// Sell only 1 ship per evaluation to avoid over-downsizing.
	return []uuid.UUID{candidates[0].id}
}

// ---------------------------------------------------------------------------
// Warehouse Operations
// ---------------------------------------------------------------------------

// warehouseOps generates warehouse load/store orders for a ship at a port.
//
// LOAD: If the warehouse has goods that are profitable to sell at the ship's
// destination (or any reachable port), load them to fill remaining capacity.
// This turns dead inventory into active trade profit.
//
// STORE: Only during idle/speculative decisions (low confidence), buy cheap
// goods and store them in the warehouse for future retrieval. This is a
// low-priority investment — never competes with active profitable trades.
func (a *HeuristicAgent) warehouseOps(
	req TradeDecisionRequest,
	decision *TradeDecision,
	priceIndex map[string]PricePoint,
	reachable map[uuid.UUID]float64,
	taxIndex map[uuid.UUID]int,
	currentPortID uuid.UUID,
	budget int64,
	minMarginPct float64,
) (loads []WarehouseTransfer, stores []WarehouseTransfer) {
	// Find warehouse at this port.
	var wh *WarehouseSnapshot
	for i := range req.Warehouses {
		if req.Warehouses[i].PortID == currentPortID {
			wh = &req.Warehouses[i]
			break
		}
	}
	if wh == nil {
		return nil, nil
	}

	buyTaxBps := taxIndex[currentPortID]

	// --- LOAD: retrieve profitable goods from warehouse ---

	// Calculate remaining ship capacity after buys.
	cargoUsed := 0
	for _, c := range req.Ship.Cargo {
		cargoUsed += c.Quantity
	}
	for _, b := range decision.BuyOrders {
		cargoUsed += b.Quantity
	}
	remaining := req.Ship.Capacity - cargoUsed

	if remaining > 0 && len(wh.Items) > 0 {
		// Determine the destination — either the ship's chosen destination
		// or find the best destination for warehouse goods.
		destID := uuid.Nil
		if decision.SailTo != nil {
			destID = *decision.SailTo
		}

		// Score each warehouse item by profitability at the destination (or best dest).
		type loadCandidate struct {
			goodID uuid.UUID
			qty    int
			profit int // profit per unit
			destID uuid.UUID
		}
		var candidates []loadCandidate

		// Use the current NPC buy price at this port as a cost baseline for
		// warehouse goods (original purchase price is not tracked).
		for _, item := range wh.Items {
			if item.Quantity <= 0 {
				continue
			}
			// Estimate cost basis: what we'd pay to buy this good at this port now.
			costBasis := 0
			if pp, ok := priceIndex[priceKey(currentPortID, item.GoodID)]; ok && pp.BuyPrice > 0 {
				costBasis = pp.BuyPrice + pp.BuyPrice*buyTaxBps/10000
			}

			// Find the best reachable destination by net profit over cost basis.
			bestProfit := 0
			bestDest := uuid.Nil

			checkDest := func(dID uuid.UUID) {
				dp, ok := priceIndex[priceKey(dID, item.GoodID)]
				if !ok || dp.SellPrice <= 0 {
					return
				}
				sellTax := dp.SellPrice * taxIndex[dID] / 10000
				netSell := dp.SellPrice - sellTax
				// Profit over cost basis. If no cost basis (good not sold here),
				// use net sell revenue directly (still profitable to move).
				profit := netSell - costBasis
				if profit > bestProfit {
					bestProfit = profit
					bestDest = dID
				}
			}

			if destID != uuid.Nil {
				checkDest(destID)
			}
			// Check all known ports, not just reachable — warehouse goods may
			// need to travel multiple hops to reach the best market.
			for _, pp := range req.PriceCache {
				if pp.PortID != currentPortID && pp.GoodID == item.GoodID {
					checkDest(pp.PortID)
				}
			}

			if bestProfit > 0 && bestDest != uuid.Nil {
				candidates = append(candidates, loadCandidate{
					goodID: item.GoodID,
					qty:    item.Quantity,
					profit: bestProfit,
					destID: bestDest,
				})
			}
		}

		// Sort by profit descending and fill remaining capacity.
		sort.Slice(candidates, func(i, j int) bool {
			return candidates[i].profit > candidates[j].profit
		})

		for _, c := range candidates {
			if remaining <= 0 {
				break
			}
			qty := c.qty
			if qty > remaining {
				qty = remaining
			}
			loads = append(loads, WarehouseTransfer{
				WarehouseID: wh.ID,
				GoodID:      c.goodID,
				Quantity:    qty,
			})
			remaining -= qty

			// If no destination chosen yet, use the best dest for warehouse goods.
			if decision.SailTo == nil {
				decision.SailTo = &c.destID
				decision.Action = "sell_and_buy"
				decision.Reasoning += " + loaded warehouse goods for sale"
			}
		}
	}

	// --- STORE: buy cheap goods into warehouse ---

	// Only store goods when idle/speculative (low confidence). High-confidence
	// trades mean we have profitable work to do — don't waste budget on
	// warehouse speculation.
	if decision.Confidence > 0.5 {
		return loads, nil
	}

	// Calculate warehouse free space.
	warehouseUsed := 0
	for _, item := range wh.Items {
		warehouseUsed += item.Quantity
	}
	warehouseFree := wh.Capacity - warehouseUsed
	if warehouseFree <= 0 {
		return loads, nil
	}

	// Don't spend more than 25% of available budget on warehouse speculation.
	storeBudget := budget / 4
	if storeBudget <= 0 {
		return loads, nil
	}

	// Find goods at this port that are cheap relative to their sell price elsewhere.
	type storeCandidate struct {
		goodID   uuid.UUID
		buyPrice int
		margin   float64 // best sell margin across all reachable ports
	}
	var storeCandidates []storeCandidate

	for _, pp := range req.PriceCache {
		if pp.PortID != currentPortID || pp.BuyPrice <= 0 {
			continue
		}
		buyTaxCost := pp.BuyPrice * buyTaxBps / 10000
		totalBuyCost := pp.BuyPrice + buyTaxCost

		// Find the best sell price across all known ports (not just reachable).
		bestMargin := 0.0
		for _, sp := range req.PriceCache {
			if sp.PortID == currentPortID || sp.SellPrice <= 0 || sp.GoodID != pp.GoodID {
				continue
			}
			sellTax := sp.SellPrice * taxIndex[sp.PortID] / 10000
			netSell := sp.SellPrice - sellTax
			margin := float64(netSell-totalBuyCost) / float64(totalBuyCost)
			if margin > bestMargin {
				bestMargin = margin
			}
		}

		// Only store if at least 15% margin — this is speculative so be pickier.
		if bestMargin >= 0.15 {
			storeCandidates = append(storeCandidates, storeCandidate{
				goodID:   pp.GoodID,
				buyPrice: pp.BuyPrice,
				margin:   bestMargin,
			})
		}
	}

	// Sort by margin descending.
	sort.Slice(storeCandidates, func(i, j int) bool {
		return storeCandidates[i].margin > storeCandidates[j].margin
	})

	// Buy goods directly into the warehouse.
	for _, sc := range storeCandidates {
		if warehouseFree <= 0 || storeBudget <= 0 {
			break
		}
		qty := int(storeBudget / int64(sc.buyPrice))
		if qty <= 0 {
			continue
		}
		if qty > warehouseFree {
			qty = warehouseFree
		}

		// Instead of executing a buy here (agent doesn't do API calls),
		// add a BuyOrder with the warehouse as destination.
		decision.BuyOrders = append(decision.BuyOrders, BuyOrder{
			GoodID:      sc.goodID,
			Quantity:    qty,
			Destination: wh.ID,
		})
		warehouseFree -= qty
		storeBudget -= int64(qty * sc.buyPrice)

		if decision.Action == "wait" {
			decision.Action = "sell_and_buy"
		}
		decision.Reasoning += fmt.Sprintf(" + storing %d units in warehouse (%.0f%% margin)", qty, sc.margin*100)
	}

	return loads, nil
}

// findBestWarehousePickup checks if any reachable warehouse has goods that
// can be profitably sold elsewhere. Returns the warehouse port ID and estimated
// total profit. This lets idle ships actively seek out warehouse inventory
// instead of leaving goods to rot.
func (a *HeuristicAgent) findBestWarehousePickup(
	req TradeDecisionRequest,
	priceIndex map[string]PricePoint,
	taxIndex map[uuid.UUID]int,
	reachable map[uuid.UUID]float64,
	currentPortID uuid.UUID,
) (uuid.UUID, int) {
	bestPort := uuid.Nil
	bestProfit := 0

	for _, wh := range req.Warehouses {
		// Skip warehouses at the current port (already handled by warehouseOps).
		if wh.PortID == currentPortID {
			continue
		}
		// Only consider directly reachable warehouse ports.
		if _, ok := reachable[wh.PortID]; !ok {
			continue
		}

		// Estimate total profit from selling all warehouse goods.
		totalProfit := 0
		for _, item := range wh.Items {
			if item.Quantity <= 0 {
				continue
			}
			// Cost basis: current buy price at the warehouse port.
			costBasis := 0
			if pp, ok := priceIndex[priceKey(wh.PortID, item.GoodID)]; ok && pp.BuyPrice > 0 {
				whTax := pp.BuyPrice * taxIndex[wh.PortID] / 10000
				costBasis = pp.BuyPrice + whTax
			}

			// Find the best sell price across all known ports.
			bestSellProfit := 0
			for _, pp := range req.PriceCache {
				if pp.PortID == wh.PortID || pp.GoodID != item.GoodID || pp.SellPrice <= 0 {
					continue
				}
				sellTax := pp.SellPrice * taxIndex[pp.PortID] / 10000
				netSell := pp.SellPrice - sellTax
				profit := netSell - costBasis
				if profit > bestSellProfit {
					bestSellProfit = profit
				}
			}
			totalProfit += bestSellProfit * item.Quantity
		}

		if totalProfit > bestProfit {
			bestProfit = totalProfit
			bestPort = wh.PortID
		}
	}

	return bestPort, bestProfit
}

func (a *HeuristicAgent) buildPortTaxIndex(ports []PortInfo) map[uuid.UUID]int {
	idx := make(map[uuid.UUID]int, len(ports))
	for _, p := range ports {
		idx[p.ID] = p.TaxRateBps
	}
	return idx
}

func priceKey(portID, goodID uuid.UUID) string {
	return portID.String() + ":" + goodID.String()
}

// getParam returns a tunable parameter from the Params map, or fallback if not set.
func getParam(params map[string]float64, key string, fallback float64) float64 {
	if params == nil {
		return fallback
	}
	if v, ok := params[key]; ok {
		return v
	}
	return fallback
}

// bestPassengerDestination finds the reachable destination with the highest
// total passenger revenue (sum of bids heading there). Only considers
// passengers that can be delivered before their deadline.
func (a *HeuristicAgent) bestPassengerDestination(
	available []PassengerInfo,
	reachable map[uuid.UUID]float64,
	shipSpeed int,
) (uuid.UUID, int) {
	now := time.Now()
	speed := math.Max(float64(shipSpeed), 1.0)

	revByDest := make(map[uuid.UUID]int)
	for _, p := range available {
		dist, ok := reachable[p.DestinationPortID]
		if !ok {
			continue
		}
		// Skip passengers that can't be delivered in time.
		if !p.ExpiresAt.IsZero() {
			travelMins := dist / speed
			arrivalTime := now.Add(time.Duration(travelMins*1.2) * time.Minute)
			if arrivalTime.After(p.ExpiresAt) {
				continue
			}
		}
		revByDest[p.DestinationPortID] += p.Bid
	}
	var bestDest uuid.UUID
	bestRev := 0
	for dest, rev := range revByDest {
		if rev > bestRev {
			bestRev = rev
			bestDest = dest
		}
	}
	return bestDest, bestRev
}

// findOpportunityBuyPort finds the best reachable buy port from the
// ProfitAnalyzer's top opportunities. Used when no local trade is profitable.
func (a *HeuristicAgent) findOpportunityBuyPort(
	opportunities []TradeOpportunity,
	reachable map[uuid.UUID]float64,
	currentPortID uuid.UUID,
) (uuid.UUID, bool) {
	if len(opportunities) == 0 {
		return uuid.Nil, false
	}

	// Opportunities are already sorted by score descending.
	for _, opp := range opportunities {
		// Skip if we're already at the buy port.
		if opp.BuyPortID == currentPortID {
			continue
		}
		// Check if the buy port is reachable from here.
		if _, ok := reachable[opp.BuyPortID]; ok {
			return opp.BuyPortID, true
		}
	}
	return uuid.Nil, false
}

// findRelocationPort picks the best port to relocate an idle ship to.
// Priority: (1) reachable hub port with most routes, (2) reachable sell port
// from top opportunities, (3) closest reachable port (just move somewhere).
func (a *HeuristicAgent) findRelocationPort(
	ports []PortInfo,
	opportunities []TradeOpportunity,
	reachable map[uuid.UUID]float64,
	currentPortID uuid.UUID,
) (uuid.UUID, bool) {
	if len(reachable) == 0 {
		return uuid.Nil, false
	}

	// Prefer hub ports — they tend to have more goods and routes.
	// Prioritize the farthest reachable hub to encourage geographic spread
	// and exploration of new areas rather than clustering at the nearest hub.
	var bestHub uuid.UUID
	bestHubDist := 0.0
	for _, p := range ports {
		if !p.IsHub || p.ID == currentPortID {
			continue
		}
		if dist, ok := reachable[p.ID]; ok && dist > bestHubDist {
			bestHubDist = dist
			bestHub = p.ID
		}
	}
	if bestHub != uuid.Nil {
		return bestHub, true
	}

	// Fall back to the sell port of a top opportunity.
	for _, opp := range opportunities {
		if opp.SellPortID == currentPortID {
			continue
		}
		if _, ok := reachable[opp.SellPortID]; ok {
			return opp.SellPortID, true
		}
	}

	// Last resort: sail to the closest port — moving is better than sitting.
	closest := a.closestPort(reachable)
	if closest != uuid.Nil {
		return closest, true
	}

	return uuid.Nil, false
}

// isPassengerShip returns true if the ship is a small/fast vessel suited for
// dedicated passenger runs (low cargo capacity, has passenger slots).
func (a *HeuristicAgent) isPassengerShip(ship ShipSnapshot) bool {
	return ship.Capacity <= 60 && ship.PassengerCap > 0
}

// findPassengerRelocationPort picks the best port for a passenger ship to
// idle at for sniping. Prefers reachable ports where no other company ships
// are currently docked, maximizing geographic coverage. Among uncovered ports,
// prefers hubs (higher passenger traffic) and farther distances (more spread).
func (a *HeuristicAgent) findPassengerRelocationPort(
	allShips []ShipSnapshot,
	ports []PortInfo,
	reachable map[uuid.UUID]float64,
	currentPortID uuid.UUID,
) (uuid.UUID, bool) {
	if len(reachable) == 0 {
		return uuid.Nil, false
	}

	// Build a set of ports where company ships are currently docked or heading.
	coveredPorts := make(map[uuid.UUID]bool)
	for _, s := range allShips {
		if s.ID == uuid.Nil {
			continue
		}
		if s.PortID != nil {
			coveredPorts[*s.PortID] = true
		}
	}

	// Score reachable ports: prefer uncovered, then hubs, then farther distance.
	type candidate struct {
		id   uuid.UUID
		dist float64
		hub  bool
	}
	var uncoveredHubs, uncoveredNonHubs, coveredHubs []candidate
	portIsHub := make(map[uuid.UUID]bool)
	for _, p := range ports {
		portIsHub[p.ID] = p.IsHub
	}

	for portID, dist := range reachable {
		if portID == currentPortID {
			continue
		}
		c := candidate{id: portID, dist: dist, hub: portIsHub[portID]}
		if !coveredPorts[portID] {
			if c.hub {
				uncoveredHubs = append(uncoveredHubs, c)
			} else {
				uncoveredNonHubs = append(uncoveredNonHubs, c)
			}
		} else if c.hub {
			coveredHubs = append(coveredHubs, c)
		}
	}

	// Pick from uncovered hubs first, then uncovered non-hubs, then covered hubs.
	// Within each tier, pick the farthest port for maximum geographic spread.
	pickFarthest := func(candidates []candidate) (uuid.UUID, bool) {
		if len(candidates) == 0 {
			return uuid.Nil, false
		}
		best := candidates[0]
		for _, c := range candidates[1:] {
			if c.dist > best.dist {
				best = c
			}
		}
		return best.id, true
	}

	if id, ok := pickFarthest(uncoveredHubs); ok {
		return id, true
	}
	if id, ok := pickFarthest(uncoveredNonHubs); ok {
		return id, true
	}
	if id, ok := pickFarthest(coveredHubs); ok {
		return id, true
	}

	return uuid.Nil, false
}

// buildOpportunitySellBonus computes a small scoring bonus for destinations
// that are the sell port of a top trade opportunity. This biases ships toward
// ports where they can sell goods from a known profitable route.
func (a *HeuristicAgent) buildOpportunitySellBonus(
	opportunities []TradeOpportunity,
	reachable map[uuid.UUID]float64,
) map[uuid.UUID]float64 {
	bonus := make(map[uuid.UUID]float64)
	if len(opportunities) == 0 {
		return bonus
	}

	for _, opp := range opportunities {
		if _, ok := reachable[opp.SellPortID]; !ok {
			continue
		}
		// Use opportunity score as bonus, capped at top 10 to avoid noise.
		if existing, ok := bonus[opp.SellPortID]; !ok || opp.Score > existing {
			bonus[opp.SellPortID] = opp.Score
		}
	}
	return bonus
}
