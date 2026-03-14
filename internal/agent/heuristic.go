package agent

import (
	"context"
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
		if len(sells) > 0 {
			return &TradeDecision{
				Action: "sell_and_buy", SellOrders: sells,
				Reasoning: "selling cargo, no outgoing routes", Confidence: 0.5,
			}, nil
		}
		return &TradeDecision{Action: "wait", Reasoning: "no outgoing routes and no cargo"}, nil
	}

	budget := req.Constraints.MaxSpend
	if budget <= 0 {
		dest := a.closestPort(reachable)
		return &TradeDecision{
			Action: "sell_and_buy", SellOrders: sells, SailTo: &dest,
			Reasoning: "treasury at floor, selling and moving on", Confidence: 0.4,
		}, nil
	}

	// Step 2: Find best opportunity — scoring differs by strategy.
	switch req.StrategyHint {
	case "bulk_hauler":
		return a.decideBulkHaulerTrade(req, sells, priceIndex, reachable, currentPortID, budget)
	default:
		// Arbitrage and market_maker both use profit/distance scoring for NPC trades.
		return a.decideArbitrageTrade(req, sells, priceIndex, reachable, currentPortID, budget)
	}
}

// decideArbitrageTrade scores opportunities by profit per unit of distance (fast turnover).
func (a *HeuristicAgent) decideArbitrageTrade(
	req TradeDecisionRequest,
	sells []SellOrder,
	priceIndex map[string]PricePoint,
	reachable map[uuid.UUID]float64,
	currentPortID uuid.UUID,
	budget int64,
) (*TradeDecision, error) {
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
		for destID, dist := range reachable {
			dp, ok := priceIndex[priceKey(destID, pp.GoodID)]
			if !ok || dp.SellPrice <= 0 {
				continue
			}
			profit := dp.SellPrice - pp.BuyPrice
			if profit <= 0 {
				continue
			}
			// Arbitrage: profit per distance favors fast turnover.
			score := float64(profit) / math.Max(dist, 1.0)
			opps = append(opps, opp{pp.GoodID, destID, pp.BuyPrice, dp.SellPrice, profit, dist, score})
		}
	}

	sort.Slice(opps, func(i, j int) bool { return opps[i].score > opps[j].score })

	if len(opps) > 0 {
		best := opps[0]
		qty := a.calcQuantity(budget, best.buyPrice, 100)
		a.logger.Info("arbitrage opportunity",
			zap.Int("buy", best.buyPrice), zap.Int("sell", best.sellAt),
			zap.Int("profit/unit", best.profit), zap.Int("qty", qty),
		)
		return &TradeDecision{
			Action:     "sell_and_buy",
			SellOrders: sells,
			BuyOrders:  []BuyOrder{{GoodID: best.goodID, Quantity: qty}},
			SailTo:     &best.destID,
			Reasoning:  "arbitrage: best profit/distance ratio",
			Confidence: 0.8,
		}, nil
	}

	return a.speculativeTrade(req.PriceCache, sells, reachable, currentPortID, budget)
}

// decideBulkHaulerTrade scores by total profit (high-value goods, fill capacity).
func (a *HeuristicAgent) decideBulkHaulerTrade(
	req TradeDecisionRequest,
	sells []SellOrder,
	priceIndex map[string]PricePoint,
	reachable map[uuid.UUID]float64,
	currentPortID uuid.UUID,
	budget int64,
) (*TradeDecision, error) {
	type opp struct {
		goodID      uuid.UUID
		destID      uuid.UUID
		buyPrice    int
		sellAt      int
		profitUnit  int
		maxQty      int
		totalProfit int
	}
	var opps []opp

	for _, pp := range req.PriceCache {
		if pp.PortID != currentPortID || pp.BuyPrice <= 0 {
			continue
		}
		for destID := range reachable {
			dp, ok := priceIndex[priceKey(destID, pp.GoodID)]
			if !ok || dp.SellPrice <= 0 {
				continue
			}
			profit := dp.SellPrice - pp.BuyPrice
			if profit <= 0 {
				continue
			}
			// Bulk hauler: maximize total profit by filling capacity.
			qty := a.calcQuantity(budget, pp.BuyPrice, 200)
			opps = append(opps, opp{pp.GoodID, destID, pp.BuyPrice, dp.SellPrice, profit, qty, profit * qty})
		}
	}

	// Sort by total profit descending — bulk hauler cares about absolute profit.
	sort.Slice(opps, func(i, j int) bool { return opps[i].totalProfit > opps[j].totalProfit })

	if len(opps) > 0 {
		best := opps[0]
		a.logger.Info("bulk hauler opportunity",
			zap.Int("buy", best.buyPrice), zap.Int("sell", best.sellAt),
			zap.Int("profit/unit", best.profitUnit), zap.Int("qty", best.maxQty),
			zap.Int("total_profit", best.totalProfit),
		)
		return &TradeDecision{
			Action:     "sell_and_buy",
			SellOrders: sells,
			BuyOrders:  []BuyOrder{{GoodID: best.goodID, Quantity: best.maxQty}},
			SailTo:     &best.destID,
			Reasoning:  "bulk hauler: maximizing total cargo profit",
			Confidence: 0.8,
		}, nil
	}

	return a.speculativeTrade(req.PriceCache, sells, reachable, currentPortID, budget)
}

// speculativeTrade buys the cheapest good and sails to explore when no clear arbitrage exists.
func (a *HeuristicAgent) speculativeTrade(
	priceCache []PricePoint,
	sells []SellOrder,
	reachable map[uuid.UUID]float64,
	currentPortID uuid.UUID,
	budget int64,
) (*TradeDecision, error) {
	dest := a.closestPort(reachable)
	var buys []BuyOrder

	var cheapest *PricePoint
	for _, pp := range priceCache {
		if pp.PortID == currentPortID && pp.BuyPrice > 0 {
			if cheapest == nil || pp.BuyPrice < cheapest.BuyPrice {
				cp := pp
				cheapest = &cp
			}
		}
	}
	if cheapest != nil {
		qty := a.calcQuantity(budget, cheapest.BuyPrice, 50)
		if qty > 0 {
			buys = append(buys, BuyOrder{GoodID: cheapest.GoodID, Quantity: qty})
		}
	}

	return &TradeDecision{
		Action: "sell_and_buy", SellOrders: sells, BuyOrders: buys, SailTo: &dest,
		Reasoning: "no clear arbitrage, exploring with speculative cargo", Confidence: 0.4,
	}, nil
}

// ---------------------------------------------------------------------------
// Fleet Decisions
// ---------------------------------------------------------------------------

// DecideFleetAction decides whether to buy or sell ships, or buy warehouses.
// BulkHauler prefers the largest (most cargo capacity) ships.
// Arbitrage prefers the fastest (best speed) ships.
// MarketMaker prefers cheap ships to minimize upkeep.
func (a *HeuristicAgent) DecideFleetAction(_ context.Context, req FleetDecisionRequest) (*FleetDecision, error) {
	treasury := req.Company.Treasury
	upkeep := req.Company.TotalUpkeep
	numShips := len(req.Ships)

	// Check if we should sell ships — upkeep is destroying our treasury.
	// If upkeep exceeds 60% of treasury and we have more than 1 ship, sell the worst.
	if numShips > 1 && upkeep > 0 && treasury > 0 && treasury < upkeep*5 {
		sellShips := a.findShipsToSell(req.Ships, req.StrategyHint)
		if len(sellShips) > 0 {
			a.logger.Info("recommending ship decommission — upkeep too high",
				zap.Int64("treasury", treasury),
				zap.Int64("upkeep", upkeep),
				zap.Int("ships_to_sell", len(sellShips)),
			)
			return &FleetDecision{
				SellShips: sellShips,
				Reasoning: "decommissioning ships: upkeep is unsustainable relative to treasury",
			}, nil
		}
	}

	if len(req.ShipTypes) == 0 {
		return &FleetDecision{Reasoning: "no ship types available"}, nil
	}

	// Max fleet size depends on strategy.
	maxShips := 5
	if req.StrategyHint == "bulk_hauler" {
		maxShips = 3 // Fewer but bigger ships.
	}
	if numShips >= maxShips {
		return &FleetDecision{Reasoning: "fleet is at maximum size"}, nil
	}

	// Choose ship type based on strategy.
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
		// Fall back to cheaper if can't afford the biggest.
		newUpkeep := upkeep + int64(targetShipType.Upkeep)
		if treasury < int64(targetShipType.BasePrice)+newUpkeep*3 {
			// Try second largest.
			if len(shipTypes) > 1 {
				targetShipType = shipTypes[1]
			}
		}

	case "arbitrage":
		// Prefer fastest ships for quick turnover.
		sort.Slice(shipTypes, func(i, j int) bool {
			return shipTypes[i].Speed > shipTypes[j].Speed
		})
		targetShipType = shipTypes[0]
		newUpkeep := upkeep + int64(targetShipType.Upkeep)
		if treasury < int64(targetShipType.BasePrice)+newUpkeep*3 {
			// Fall back to cheapest.
			sort.Slice(shipTypes, func(i, j int) bool {
				return shipTypes[i].BasePrice < shipTypes[j].BasePrice
			})
			targetShipType = shipTypes[0]
		}

	default: // market_maker and others
		// Prefer cheapest ships to minimize upkeep (more budget for orders).
		sort.Slice(shipTypes, func(i, j int) bool {
			return shipTypes[i].BasePrice < shipTypes[j].BasePrice
		})
		targetShipType = shipTypes[0]
	}

	// Safety check: can we afford it?
	newUpkeep := upkeep + int64(targetShipType.Upkeep)
	safetyMargin := newUpkeep * 3
	if treasury < int64(targetShipType.BasePrice)+safetyMargin {
		// Last resort: try absolute cheapest ship.
		sort.Slice(shipTypes, func(i, j int) bool {
			return shipTypes[i].BasePrice < shipTypes[j].BasePrice
		})
		targetShipType = shipTypes[0]
		newUpkeep = upkeep + int64(targetShipType.Upkeep)
		safetyMargin = newUpkeep * 3
		if treasury < int64(targetShipType.BasePrice)+safetyMargin {
			return &FleetDecision{Reasoning: "treasury too low to safely purchase a ship"}, nil
		}
	}

	// Pick a port to buy at.
	purchasePortID := a.findPurchasePort(req.Ships, req.PriceCache)
	if purchasePortID == uuid.Nil {
		return &FleetDecision{Reasoning: "no suitable port found for ship purchase"}, nil
	}

	a.logger.Info("recommending ship purchase",
		zap.String("strategy", req.StrategyHint),
		zap.String("ship_type", targetShipType.Name),
		zap.Int("price", targetShipType.BasePrice),
		zap.Int("capacity", targetShipType.Capacity),
		zap.Int("speed", targetShipType.Speed),
		zap.Int("current_fleet", numShips),
	)

	return &FleetDecision{
		BuyShips: []ShipPurchase{{
			ShipTypeID: targetShipType.ID,
			PortID:     purchasePortID,
		}},
		Reasoning: "expanding fleet: " + req.StrategyHint + " strategy",
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

func (a *HeuristicAgent) findPurchasePort(ships []ShipSnapshot, priceCache []PricePoint) uuid.UUID {
	// Prefer port where we already have a docked ship.
	for _, ship := range ships {
		if ship.Status == "docked" && ship.PortID != nil {
			return *ship.PortID
		}
	}
	// Fall back to any port from price cache.
	if len(priceCache) > 0 {
		return priceCache[0].PortID
	}
	return uuid.Nil
}

// findShipsToSell identifies the least valuable ship to decommission.
// For arbitrage: sell the slowest ship. For bulk_hauler: sell the smallest.
// For market_maker: sell the most expensive (highest upkeep).
// Only considers docked ships with no cargo.
func (a *HeuristicAgent) findShipsToSell(ships []ShipSnapshot, strategy string) []uuid.UUID {
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

func priceKey(portID, goodID uuid.UUID) string {
	return portID.String() + ":" + goodID.String()
}
