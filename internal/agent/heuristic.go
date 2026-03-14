package agent

import (
	"context"
	"fmt"
	"math"
	"sort"

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

	// Step 1: Sell all cargo at current port.
	sells := a.buildSellOrders(ship)

	// Build indexes for fast lookup.
	priceIndex := a.buildPriceIndex(req.PriceCache)
	reachable := a.reachablePorts(req.Routes, currentPortID)

	if len(reachable) == 0 {
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

	budget := req.Constraints.MaxSpend

	// Step 1.5: Fill profitable P2P orders at the current port.
	// This happens before NPC buying since fills can be more profitable.
	fills, fillCost := a.findProfitableOrderFills(req.PortOrders, req.OwnOrders, priceIndex, currentPortID, budget)
	budget -= fillCost

	if budget <= 0 {
		dest := a.closestPort(reachable)
		passengers := a.selectPassengers(req.AvailablePassengers, req.BoardedPassengers, ship.PassengerCap, &dest, reachable)
		return &TradeDecision{
			Action: "sell_and_buy", SellOrders: sells, FillOrders: fills, SailTo: &dest,
			BoardPassengers: passengers,
			Reasoning:       "treasury at floor, selling cargo and filling orders before moving on", Confidence: 0.4,
		}, nil
	}

	// Step 2: Find best opportunity — scoring differs by strategy.
	var decision *TradeDecision
	var err error

	switch req.StrategyHint {
	case "bulk_hauler":
		decision, err = a.decideBulkHaulerTrade(req, sells, priceIndex, reachable, currentPortID, budget)
	default:
		decision, err = a.decideArbitrageTrade(req, sells, priceIndex, reachable, currentPortID, budget)
	}

	if err != nil {
		return decision, err
	}

	// Attach P2P fills to the decision.
	decision.FillOrders = fills

	// Step 3: Board passengers heading to our destination (or any reachable port).
	decision.BoardPassengers = a.selectPassengers(
		req.AvailablePassengers, req.BoardedPassengers,
		ship.PassengerCap, decision.SailTo, reachable,
	)

	return decision, nil
}

// selectPassengers picks profitable passengers to board, preferring those heading
// to the chosen destination. Returns passenger IDs to board.
func (a *HeuristicAgent) selectPassengers(
	available []PassengerInfo,
	boarded []PassengerInfo,
	passengerCap int,
	destPortID *uuid.UUID,
	reachable map[uuid.UUID]float64,
) []uuid.UUID {
	if passengerCap <= 0 || len(available) == 0 {
		return nil
	}

	// Remaining capacity = cap minus already boarded groups.
	remaining := passengerCap - len(boarded)
	if remaining <= 0 {
		return nil
	}

	// Score passengers: those heading to our destination get a 2x bonus.
	type scored struct {
		id    uuid.UUID
		score float64
	}
	var candidates []scored
	for _, p := range available {
		// Only board passengers heading to a reachable port.
		if _, ok := reachable[p.DestinationPortID]; !ok {
			continue
		}

		bidPerHead := float64(p.Bid) / float64(p.Count)
		dist := reachable[p.DestinationPortID]
		score := bidPerHead / max(dist, 1.0)

		// Bonus if heading to the same destination we've already chosen.
		if destPortID != nil && p.DestinationPortID == *destPortID {
			score *= 2.0
		}

		candidates = append(candidates, scored{id: p.ID, score: score})
	}

	sort.Slice(candidates, func(i, j int) bool {
		return candidates[i].score > candidates[j].score
	})

	var selected []uuid.UUID
	for _, c := range candidates {
		if len(selected) >= remaining {
			break
		}
		selected = append(selected, c.id)
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

// decideArbitrageTrade scores opportunities by profit per unit of distance (fast turnover).
// It picks the best destination, then greedily fills ship capacity with profitable goods.
func (a *HeuristicAgent) decideArbitrageTrade(
	req TradeDecisionRequest,
	sells []SellOrder,
	priceIndex map[string]PricePoint,
	reachable map[uuid.UUID]float64,
	currentPortID uuid.UUID,
	budget int64,
) (*TradeDecision, error) {
	taxIndex := a.buildPortTaxIndex(req.Ports)
	buyTaxBps := taxIndex[currentPortID]

	// Build passenger revenue index for destination scoring.
	passengerRevByDest := make(map[uuid.UUID]int)
	for _, p := range req.AvailablePassengers {
		passengerRevByDest[p.DestinationPortID] += p.Bid
	}

	type opp struct {
		goodID   uuid.UUID
		destID   uuid.UUID
		buyPrice int
		sellAt   int
		profit   int
		distance float64
		score    float64
	}
	var opps []opp

	for _, pp := range req.PriceCache {
		if pp.PortID != currentPortID || pp.BuyPrice <= 0 {
			continue
		}
		taxCost := pp.BuyPrice * buyTaxBps / 10000
		for destID, dist := range reachable {
			dp, ok := priceIndex[priceKey(destID, pp.GoodID)]
			if !ok || dp.SellPrice <= 0 {
				continue
			}
			profit := dp.SellPrice - pp.BuyPrice - taxCost
			if profit <= 0 {
				continue
			}
			// Arbitrage: profit per distance favors fast turnover.
			score := float64(profit) / math.Max(dist, 1.0)
			// Add passenger revenue bonus for this destination.
			score += float64(passengerRevByDest[destID]) / math.Max(dist, 1.0) * 0.5
			opps = append(opps, opp{pp.GoodID, destID, pp.BuyPrice, dp.SellPrice, profit, dist, score})
		}
	}

	sort.Slice(opps, func(i, j int) bool { return opps[i].score > opps[j].score })

	if len(opps) > 0 {
		// Pick the best destination from the highest-scoring opportunity.
		bestDest := opps[0].destID

		// Collect all profitable goods going to that destination, sorted by profit/unit.
		type destGood struct {
			goodID   uuid.UUID
			buyPrice int
			profit   int
		}
		seen := make(map[uuid.UUID]bool)
		var goods []destGood
		for _, o := range opps {
			if o.destID != bestDest {
				continue
			}
			if seen[o.goodID] {
				continue
			}
			seen[o.goodID] = true
			goods = append(goods, destGood{o.goodID, o.buyPrice, o.profit})
		}
		sort.Slice(goods, func(i, j int) bool { return goods[i].profit > goods[j].profit })

		// Greedily fill ship capacity.
		remaining := req.Ship.Capacity
		remainBudget := budget
		var buys []BuyOrder
		for _, g := range goods {
			if remaining <= 0 || remainBudget <= 0 {
				break
			}
			qty := a.calcQuantity(remainBudget, g.buyPrice, remaining)
			if qty > 0 {
				buys = append(buys, BuyOrder{GoodID: g.goodID, Quantity: qty})
				remaining -= qty
				remainBudget -= int64(qty * g.buyPrice)
			}
		}

		a.logger.Info("arbitrage opportunity",
			zap.Int("goods", len(buys)), zap.Int("capacity_used", req.Ship.Capacity-remaining),
		)
		return &TradeDecision{
			Action:     "sell_and_buy",
			SellOrders: sells,
			BuyOrders:  buys,
			SailTo:     &bestDest,
			Reasoning:  "arbitrage: best profit/distance ratio, multi-good fill",
			Confidence: 0.8,
		}, nil
	}

	return a.speculativeTrade(req.PriceCache, sells, reachable, currentPortID, budget)
}

// decideBulkHaulerTrade scores by total profit (high-value goods, fill capacity).
// It picks the best destination, then greedily fills ship capacity with profitable goods.
func (a *HeuristicAgent) decideBulkHaulerTrade(
	req TradeDecisionRequest,
	sells []SellOrder,
	priceIndex map[string]PricePoint,
	reachable map[uuid.UUID]float64,
	currentPortID uuid.UUID,
	budget int64,
) (*TradeDecision, error) {
	taxIndex := a.buildPortTaxIndex(req.Ports)
	buyTaxBps := taxIndex[currentPortID]
	capacity := req.Ship.Capacity

	// Build passenger revenue index for destination scoring.
	passengerRevByDest := make(map[uuid.UUID]int)
	for _, p := range req.AvailablePassengers {
		passengerRevByDest[p.DestinationPortID] += p.Bid
	}

	type opp struct {
		goodID      uuid.UUID
		destID      uuid.UUID
		buyPrice    int
		sellAt      int
		profitUnit  int
		totalProfit int
		score       float64
	}
	var opps []opp

	for _, pp := range req.PriceCache {
		if pp.PortID != currentPortID || pp.BuyPrice <= 0 {
			continue
		}
		taxCost := pp.BuyPrice * buyTaxBps / 10000
		for destID, dist := range reachable {
			dp, ok := priceIndex[priceKey(destID, pp.GoodID)]
			if !ok || dp.SellPrice <= 0 {
				continue
			}
			profit := dp.SellPrice - pp.BuyPrice - taxCost
			if profit <= 0 {
				continue
			}
			// Bulk hauler: maximize total profit by filling capacity.
			qty := a.calcQuantity(budget, pp.BuyPrice, capacity)
			totalProfit := profit * qty
			score := float64(totalProfit)
			// Add passenger revenue bonus for this destination.
			score += float64(passengerRevByDest[destID]) / math.Max(dist, 1.0) * 0.5
			opps = append(opps, opp{pp.GoodID, destID, pp.BuyPrice, dp.SellPrice, profit, totalProfit, score})
		}
	}

	// Sort by score descending — bulk hauler cares about absolute profit + passenger bonus.
	sort.Slice(opps, func(i, j int) bool { return opps[i].score > opps[j].score })

	if len(opps) > 0 {
		// Pick the best destination from the highest-scoring opportunity.
		bestDest := opps[0].destID

		// Collect all profitable goods going to that destination, sorted by profit/unit.
		type destGood struct {
			goodID   uuid.UUID
			buyPrice int
			profit   int
		}
		seen := make(map[uuid.UUID]bool)
		var goods []destGood
		for _, o := range opps {
			if o.destID != bestDest {
				continue
			}
			if seen[o.goodID] {
				continue
			}
			seen[o.goodID] = true
			goods = append(goods, destGood{o.goodID, o.buyPrice, o.profitUnit})
		}
		sort.Slice(goods, func(i, j int) bool { return goods[i].profit > goods[j].profit })

		// Greedily fill ship capacity.
		remaining := capacity
		remainBudget := budget
		var buys []BuyOrder
		for _, g := range goods {
			if remaining <= 0 || remainBudget <= 0 {
				break
			}
			qty := a.calcQuantity(remainBudget, g.buyPrice, remaining)
			if qty > 0 {
				buys = append(buys, BuyOrder{GoodID: g.goodID, Quantity: qty})
				remaining -= qty
				remainBudget -= int64(qty * g.buyPrice)
			}
		}

		a.logger.Info("bulk hauler opportunity",
			zap.Int("goods", len(buys)), zap.Int("capacity_used", capacity-remaining),
		)
		return &TradeDecision{
			Action:     "sell_and_buy",
			SellOrders: sells,
			BuyOrders:  buys,
			SailTo:     &bestDest,
			Reasoning:  "bulk hauler: maximizing total cargo profit, multi-good fill",
			Confidence: 0.8,
		}, nil
	}

	return a.speculativeTrade(req.PriceCache, sells, reachable, currentPortID, budget)
}

// speculativeTrade scans the price cache for the best margin opportunity
// when no clear arbitrage exists. Falls back to closest port with no cargo.
func (a *HeuristicAgent) speculativeTrade(
	priceCache []PricePoint,
	sells []SellOrder,
	reachable map[uuid.UUID]float64,
	currentPortID uuid.UUID,
	budget int64,
) (*TradeDecision, error) {
	// For each buyable good at the current port, find the best sell price at any reachable port.
	type specOpp struct {
		goodID   uuid.UUID
		destID   uuid.UUID
		buyPrice int
		maxSell  int
		margin   int
	}
	var opps []specOpp

	for _, pp := range priceCache {
		if pp.PortID != currentPortID || pp.BuyPrice <= 0 {
			continue
		}
		var bestDest uuid.UUID
		bestSell := 0
		for _, other := range priceCache {
			if _, ok := reachable[other.PortID]; !ok {
				continue
			}
			if other.GoodID == pp.GoodID && other.SellPrice > bestSell {
				bestSell = other.SellPrice
				bestDest = other.PortID
			}
		}
		if bestSell > 0 {
			margin := bestSell - pp.BuyPrice
			opps = append(opps, specOpp{pp.GoodID, bestDest, pp.BuyPrice, bestSell, margin})
		}
	}

	sort.Slice(opps, func(i, j int) bool { return opps[i].margin > opps[j].margin })

	if len(opps) > 0 && opps[0].margin > 0 {
		best := opps[0]
		qty := a.calcQuantity(budget, best.buyPrice, 50)
		if qty > 0 {
			return &TradeDecision{
				Action:     "sell_and_buy",
				SellOrders: sells,
				BuyOrders:  []BuyOrder{{GoodID: best.goodID, Quantity: qty}},
				SailTo:     &best.destID,
				Reasoning:  "speculative: best margin from price cache scan",
				Confidence: 0.4,
			}, nil
		}
	}

	// Fallback: sail to closest port with no cargo.
	dest := a.closestPort(reachable)
	return &TradeDecision{
		Action: "sell_and_buy", SellOrders: sells, SailTo: &dest,
		Reasoning: "no clear arbitrage, exploring without cargo", Confidence: 0.4,
	}, nil
}

// ---------------------------------------------------------------------------
// Fleet Decisions
// ---------------------------------------------------------------------------

// reserveHours calculates how many hours of upkeep buffer the company should
// maintain after a ship purchase. Grows with fleet size so that companies
// become progressively more cautious before adding each additional ship.
//
//	fleet 1  → 3h   |  fleet 5  → 5h   |  fleet 10 → 7h
//	fleet 15 → 9h   |  fleet 20 → 11h  |  fleet 30 → 15h
//
// The strategy multiplier shifts the curve: bulk_hauler is more conservative
// (fewer but bigger ships), market_maker is more aggressive (cheap ships).
func reserveHours(numShips int, strategy string) int64 {
	base := int64(3)
	growth := int64(numShips) / 2 // +1 hour of reserve for every 2 ships

	switch strategy {
	case "bulk_hauler":
		// More conservative — big ships cost more upkeep, so demand a larger buffer.
		growth = int64(numShips) // +1 hour per ship
	case "market_maker":
		// More aggressive — cheap ships, fast expansion.
		growth = int64(numShips) / 3 // +1 hour per 3 ships
	}

	return base + growth
}

// DecideFleetAction decides whether to buy or sell ships, or buy warehouses.
// Fleet size is governed by economics rather than hard caps: a new ship is
// purchased only when the treasury can cover the price AND a scaling upkeep
// reserve that grows with fleet size. This means wealthy companies naturally
// grow larger fleets while cash-strapped ones hold steady.
//
// Ship type preferences by strategy:
//
//	BulkHauler  → largest capacity (galleons), conservative growth
//	Arbitrage   → fastest ships, moderate growth
//	MarketMaker → cheapest ships, aggressive growth
func (a *HeuristicAgent) DecideFleetAction(_ context.Context, req FleetDecisionRequest) (*FleetDecision, error) {
	treasury := req.Company.Treasury
	upkeep := req.Company.TotalUpkeep
	numShips := len(req.Ships)

	// --- WAREHOUSE PURCHASE ---
	// Only consider warehouses when fleet is already at a reasonable size (3+ ships)
	// and treasury is very healthy.
	if len(req.Warehouses) < 2 && treasury > upkeep*10 && numShips >= 3 {
		portActivity := make(map[uuid.UUID]int)
		for _, ship := range req.Ships {
			if ship.Status == "docked" && ship.PortID != nil {
				portActivity[*ship.PortID]++
			}
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
				zap.Int("docked_ships_at_port", bestCount),
				zap.Int("existing_warehouses", len(req.Warehouses)),
			)
			return &FleetDecision{
				BuyWarehouses: []uuid.UUID{bestPort},
				Reasoning:     "buying warehouse at high-activity port",
			}, nil
		}
	}

	// --- SHIP SALE ---
	// Sell if upkeep is unsustainable: treasury can't cover reserveHours of upkeep.
	reserve := reserveHours(numShips, req.StrategyHint)
	if numShips > 1 && upkeep > 0 && treasury > 0 && treasury < upkeep*reserve {
		sellShips := a.findShipsToSell(req.Ships, req.StrategyHint, req.ShipyardPorts)
		if len(sellShips) > 0 {
			a.logger.Info("recommending ship decommission — upkeep too high",
				zap.Int64("treasury", treasury),
				zap.Int64("upkeep", upkeep),
				zap.Int64("reserve_hours", reserve),
				zap.Int("ships_to_sell", len(sellShips)),
			)
			return &FleetDecision{
				SellShips: sellShips,
				Reasoning: fmt.Sprintf("decommissioning: treasury covers only %dh of upkeep (need %dh)", treasury/max(upkeep, 1), reserve),
			}, nil
		}
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
		// Prefer largest capacity ships (galleons).
		sort.Slice(shipTypes, func(i, j int) bool {
			return shipTypes[i].Capacity > shipTypes[j].Capacity
		})
		targetShipType = shipTypes[0]

	case "arbitrage":
		// Prefer fastest ships for quick turnover.
		sort.Slice(shipTypes, func(i, j int) bool {
			return shipTypes[i].Speed > shipTypes[j].Speed
		})
		targetShipType = shipTypes[0]

	default: // market_maker and others
		// Prefer cheapest ships to minimize upkeep (more budget for orders).
		sort.Slice(shipTypes, func(i, j int) bool {
			return shipTypes[i].BasePrice < shipTypes[j].BasePrice
		})
		targetShipType = shipTypes[0]
	}

	// --- AFFORDABILITY CHECK ---
	// The company must be able to afford the ship price AND still maintain
	// a reserve of (newTotalUpkeep * reserveHours) afterwards.
	// reserveHours grows with fleet size, naturally limiting expansion.
	newReserve := reserveHours(numShips+1, req.StrategyHint)
	canAfford := func(st ShipTypeInfo) bool {
		newUpkeep := upkeep + int64(st.Upkeep)
		required := int64(st.BasePrice) + newUpkeep*newReserve
		return treasury >= required
	}

	if !canAfford(targetShipType) {
		// Preferred type too expensive — try all types sorted cheapest first.
		sort.Slice(shipTypes, func(i, j int) bool {
			return shipTypes[i].BasePrice < shipTypes[j].BasePrice
		})
		found := false
		for _, st := range shipTypes {
			if canAfford(st) {
				targetShipType = st
				found = true
				break
			}
		}
		if !found {
			newUpkeep := upkeep + int64(shipTypes[0].Upkeep)
			required := int64(shipTypes[0].BasePrice) + newUpkeep*newReserve
			return &FleetDecision{
				Reasoning: fmt.Sprintf("treasury %d too low: cheapest ship needs %d (price %d + %dh reserve of %d/hr upkeep)",
					treasury, required, shipTypes[0].BasePrice, newReserve, newUpkeep),
			}, nil
		}
	}

	// --- PURCHASE PORT SELECTION ---
	purchasePortID := a.findPurchasePort(req.Ships, req.ShipyardPorts)
	if purchasePortID == uuid.Nil {
		return &FleetDecision{Reasoning: "no suitable port found for ship purchase"}, nil
	}

	newUpkeep := upkeep + int64(targetShipType.Upkeep)
	a.logger.Info("recommending ship purchase",
		zap.String("strategy", req.StrategyHint),
		zap.String("ship_type", targetShipType.Name),
		zap.Int("price", targetShipType.BasePrice),
		zap.Int("capacity", targetShipType.Capacity),
		zap.Int("speed", targetShipType.Speed),
		zap.Int("current_fleet", numShips),
		zap.Int64("treasury_after", treasury-int64(targetShipType.BasePrice)),
		zap.Int64("new_upkeep", newUpkeep),
		zap.Int64("reserve_hours", newReserve),
	)

	return &FleetDecision{
		BuyShips: []ShipPurchase{{
			ShipTypeID: targetShipType.ID,
			PortID:     purchasePortID,
		}},
		Reasoning: fmt.Sprintf("expanding fleet to %d ships (%s), treasury covers %dh of new upkeep",
			numShips+1, targetShipType.Name, treasury/max(newUpkeep, 1)),
	}, nil
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
						a.logger.Info("filling underpriced sell order",
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
				minProfit := npcPrice.BuyPrice / 10
				if profit >= minProfit {
					qty := order.Remaining
					cost := int64(qty * npcPrice.BuyPrice)
					if cost > available-totalFillCost {
						qty = int((available - totalFillCost) / int64(npcPrice.BuyPrice))
					}
					if qty > 0 {
						fills = append(fills, FillOrder{OrderID: order.ID, Quantity: qty})
						totalFillCost += int64(qty * npcPrice.BuyPrice)
						a.logger.Info("filling overpriced buy order",
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
				a.logger.Info("posting market buy order",
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

	// If worst is losing money and best is profitable, recommend switching.
	if worst.ProfitPerHour < 0 && best.ProfitPerHour > 0 && worst.StrategyName != best.StrategyName {
		switchTo := best.StrategyName
		return &StrategyEvaluation{
			SwitchTo:  &switchTo,
			Reasoning: worst.StrategyName + " is losing money, switching to " + best.StrategyName,
		}, nil
	}

	// If best is 2x better than worst, recommend switch.
	if best.ProfitPerHour > 0 && worst.ProfitPerHour > 0 &&
		best.ProfitPerHour > worst.ProfitPerHour*2 &&
		worst.StrategyName != best.StrategyName {
		switchTo := best.StrategyName
		return &StrategyEvaluation{
			SwitchTo:  &switchTo,
			Reasoning: best.StrategyName + " outperforms " + worst.StrategyName + " by 2x+",
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
			// Player selling cheap → we buy (profit = NPC sell price - order price).
			if npcPrice.SellPrice > 0 && order.Price < npcPrice.SellPrice {
				profit := npcPrice.SellPrice - order.Price
				minProfit := npcPrice.SellPrice / 10 // 10% min margin
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
			// Player buying expensive → we sell to them (profit = order price - NPC buy price).
			if npcPrice.BuyPrice > 0 && order.Price > npcPrice.BuyPrice {
				profit := order.Price - npcPrice.BuyPrice
				minProfit := npcPrice.BuyPrice / 10
				if profit >= minProfit {
					candidates = append(candidates, scoredFill{
						orderID: order.ID,
						side:    "buy",
						qty:     order.Remaining,
						cost:    int64(order.Remaining * npcPrice.BuyPrice),
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
			a.logger.Info("trade fill opportunity",
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

func (a *HeuristicAgent) buildSellOrders(ship ShipSnapshot) []SellOrder {
	var sells []SellOrder
	for _, cargo := range ship.Cargo {
		if cargo.Quantity > 0 {
			sells = append(sells, SellOrder{GoodID: cargo.GoodID, Quantity: cargo.Quantity})
		}
	}
	return sells
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

func (a *HeuristicAgent) calcQuantity(budget int64, unitPrice int, maxQty int) int {
	if unitPrice <= 0 {
		return 0
	}
	affordable := int(budget) / unitPrice
	if affordable < maxQty {
		maxQty = affordable
	}
	if maxQty < 1 {
		maxQty = 1
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
		default:
			// Market maker: keep cheap ships, sell expensive ones.
			// Use negative capacity as proxy for cost (bigger = more upkeep).
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
