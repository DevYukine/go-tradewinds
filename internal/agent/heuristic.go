package agent

import (
	"context"
	"math"
	"sort"

	"github.com/google/uuid"
	"go.uber.org/zap"
)

// HeuristicAgent uses hand-coded rules for trading decisions.
// It implements simple arbitrage: buy cheap goods at one port and sell
// them at another where prices are higher.
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

// DecideTradeAction evaluates the current port and price cache to decide
// what to sell, what to buy, and where to sail next.
func (a *HeuristicAgent) DecideTradeAction(_ context.Context, req TradeDecisionRequest) (*TradeDecision, error) {
	ship := req.Ship
	portID := ship.PortID
	if portID == nil {
		return &TradeDecision{
			Action:    "wait",
			Reasoning: "ship is not docked",
		}, nil
	}

	currentPortID := *portID

	// Step 1: Sell all cargo at current port.
	var sells []SellOrder
	for _, cargo := range ship.Cargo {
		if cargo.Quantity > 0 {
			sells = append(sells, SellOrder{
				GoodID:   cargo.GoodID,
				Quantity: cargo.Quantity,
			})
		}
	}

	// Step 2: Find the best arbitrage opportunity using price cache.
	// Build a lookup: portID:goodID → PricePoint
	priceIndex := make(map[string]PricePoint, len(req.PriceCache))
	for _, pp := range req.PriceCache {
		key := pp.PortID.String() + ":" + pp.GoodID.String()
		priceIndex[key] = pp
	}

	// Find reachable ports from current port.
	reachablePorts := make(map[uuid.UUID]float64) // portID → distance
	for _, route := range req.Routes {
		if route.FromID == currentPortID {
			reachablePorts[route.ToID] = route.Distance
		}
	}

	if len(reachablePorts) == 0 {
		// No routes from this port — just sell and wait.
		if len(sells) > 0 {
			return &TradeDecision{
				Action:     "sell_and_buy",
				SellOrders: sells,
				Reasoning:  "selling cargo, no outgoing routes from this port",
				Confidence: 0.5,
			}, nil
		}
		return &TradeDecision{
			Action:    "wait",
			Reasoning: "no outgoing routes and no cargo to sell",
		}, nil
	}

	// Calculate available spending budget.
	budget := req.Constraints.MaxSpend
	if budget <= 0 {
		// Treasury is at or below floor, just sell and move.
		if len(sells) > 0 {
			destID := a.pickRandomDestination(reachablePorts)
			return &TradeDecision{
				Action:     "sell_and_buy",
				SellOrders: sells,
				SailTo:     &destID,
				Reasoning:  "treasury too low to buy, selling cargo and moving on",
				Confidence: 0.4,
			}, nil
		}
		destID := a.pickRandomDestination(reachablePorts)
		return &TradeDecision{
			Action:     "buy_and_sail",
			SailTo:     &destID,
			Reasoning:  "treasury at floor, sailing to find opportunities",
			Confidence: 0.3,
		}, nil
	}

	// Score each (good, destination) pair by expected profit per distance.
	type opportunity struct {
		goodID   uuid.UUID
		destID   uuid.UUID
		buyPrice int
		sellAt   int
		profit   int
		distance float64
		score    float64
	}

	var opportunities []opportunity

	// Collect all goods we know buy prices for at the current port.
	for _, pp := range req.PriceCache {
		if pp.PortID != currentPortID || pp.BuyPrice <= 0 {
			continue
		}

		// For each reachable destination, check sell price.
		for destID, dist := range reachablePorts {
			destKey := destID.String() + ":" + pp.GoodID.String()
			destPrice, ok := priceIndex[destKey]
			if !ok || destPrice.SellPrice <= 0 {
				continue
			}

			profit := destPrice.SellPrice - pp.BuyPrice
			if profit <= 0 {
				continue
			}

			// Score: profit per unit, adjusted by distance (shorter is better).
			score := float64(profit) / math.Max(dist, 1.0)
			opportunities = append(opportunities, opportunity{
				goodID:   pp.GoodID,
				destID:   destID,
				buyPrice: pp.BuyPrice,
				sellAt:   destPrice.SellPrice,
				profit:   profit,
				distance: dist,
				score:    score,
			})
		}
	}

	// Sort by score descending.
	sort.Slice(opportunities, func(i, j int) bool {
		return opportunities[i].score > opportunities[j].score
	})

	if len(opportunities) > 0 {
		best := opportunities[0]

		// Calculate quantity we can afford and carry.
		// Estimate capacity: since we don't have exact capacity in the snapshot,
		// use a conservative default. The ship will have sold cargo by now.
		maxQuantity := 100 // Conservative default.
		affordable := int(budget) / best.buyPrice
		if affordable < maxQuantity {
			maxQuantity = affordable
		}
		if maxQuantity < 1 {
			maxQuantity = 1
		}

		buys := []BuyOrder{{
			GoodID:   best.goodID,
			Quantity: maxQuantity,
		}}

		a.logger.Info("arbitrage opportunity found",
			zap.Int("buy_price", best.buyPrice),
			zap.Int("sell_at", best.sellAt),
			zap.Int("profit_per_unit", best.profit),
			zap.Float64("distance", best.distance),
			zap.Int("quantity", maxQuantity),
		)

		return &TradeDecision{
			Action:     "sell_and_buy",
			SellOrders: sells,
			BuyOrders:  buys,
			SailTo:     &best.destID,
			Reasoning:  "arbitrage: buying cheap and selling at higher price at destination",
			Confidence: 0.8,
		}, nil
	}

	// No good opportunities found — sell cargo and explore a new port.
	destID := a.pickRandomDestination(reachablePorts)

	// If we have no price data at destination, buy some goods speculatively.
	var buys []BuyOrder
	if len(req.PriceCache) > 0 {
		// Buy the cheapest available good at current port for speculative trade.
		var cheapest *PricePoint
		for _, pp := range req.PriceCache {
			if pp.PortID == currentPortID && pp.BuyPrice > 0 {
				if cheapest == nil || pp.BuyPrice < cheapest.BuyPrice {
					cp := pp
					cheapest = &cp
				}
			}
		}

		if cheapest != nil {
			qty := int(budget) / cheapest.BuyPrice
			if qty > 50 {
				qty = 50
			}
			if qty > 0 {
				buys = append(buys, BuyOrder{
					GoodID:   cheapest.GoodID,
					Quantity: qty,
				})
			}
		}
	}

	return &TradeDecision{
		Action:     "sell_and_buy",
		SellOrders: sells,
		BuyOrders:  buys,
		SailTo:     &destID,
		Reasoning:  "no clear arbitrage, exploring new ports with speculative cargo",
		Confidence: 0.4,
	}, nil
}

// DecideFleetAction decides whether to buy ships or warehouses.
func (a *HeuristicAgent) DecideFleetAction(_ context.Context, req FleetDecisionRequest) (*FleetDecision, error) {
	treasury := req.Company.Treasury
	upkeep := req.Company.TotalUpkeep
	numShips := len(req.Ships)

	if len(req.ShipTypes) == 0 {
		return &FleetDecision{
			Reasoning: "no ship types available",
		}, nil
	}

	// Sort ship types by base price ascending (cheapest first).
	shipTypes := make([]ShipTypeInfo, len(req.ShipTypes))
	copy(shipTypes, req.ShipTypes)
	sort.Slice(shipTypes, func(i, j int) bool {
		return shipTypes[i].BasePrice < shipTypes[j].BasePrice
	})

	// Decide how many ships to target.
	maxShips := 5
	if numShips >= maxShips {
		return &FleetDecision{
			Reasoning: "fleet is at maximum size",
		}, nil
	}

	// Choose ship type based on current fleet size.
	// Start with cheapest ships, graduate to better ones as we grow.
	var targetShipType ShipTypeInfo
	if numShips == 0 {
		// First ship: buy the cheapest.
		targetShipType = shipTypes[0]
	} else if numShips < 3 && len(shipTypes) > 1 {
		// Growing fleet: pick a mid-tier ship if affordable.
		mid := len(shipTypes) / 2
		targetShipType = shipTypes[mid]
		// Fall back to cheapest if too expensive.
		newUpkeep := upkeep + int64(targetShipType.Upkeep)
		if treasury < int64(targetShipType.BasePrice)+newUpkeep*3 {
			targetShipType = shipTypes[0]
		}
	} else {
		// Expansion: keep buying cheapest to maximize fleet.
		targetShipType = shipTypes[0]
	}

	// Safety check: can we afford it with margin?
	newUpkeep := upkeep + int64(targetShipType.Upkeep)
	safetyMargin := newUpkeep * 3 // Keep 3x upkeep after purchase.
	if treasury < int64(targetShipType.BasePrice)+safetyMargin {
		a.logger.Debug("cannot afford ship",
			zap.Int64("treasury", treasury),
			zap.Int("ship_price", targetShipType.BasePrice),
			zap.Int64("safety_margin", safetyMargin),
		)
		return &FleetDecision{
			Reasoning: "treasury too low to safely purchase a ship",
		}, nil
	}

	// Pick a port to buy at. Use the first port we find that has routes.
	// Prefer ports where we already have docked ships, or just use any port.
	var purchasePortID uuid.UUID
	for _, ship := range req.Ships {
		if ship.Status == "docked" && ship.PortID != nil {
			purchasePortID = *ship.PortID
			break
		}
	}

	// If no docked ships, use a port from price cache (we know it exists).
	if purchasePortID == uuid.Nil && len(req.PriceCache) > 0 {
		purchasePortID = req.PriceCache[0].PortID
	}

	if purchasePortID == uuid.Nil {
		return &FleetDecision{
			Reasoning: "no suitable port found for ship purchase",
		}, nil
	}

	a.logger.Info("recommending ship purchase",
		zap.String("ship_type", targetShipType.Name),
		zap.Int("price", targetShipType.BasePrice),
		zap.String("port", purchasePortID.String()),
		zap.Int("current_fleet", numShips),
	)

	return &FleetDecision{
		BuyShips: []ShipPurchase{{
			ShipTypeID: targetShipType.ID,
			PortID:     purchasePortID,
		}},
		Reasoning: "expanding fleet to increase trading capacity",
	}, nil
}

// DecideMarketAction is a no-op for the heuristic agent.
// P2P market making requires more sophisticated logic.
func (a *HeuristicAgent) DecideMarketAction(_ context.Context, _ MarketDecisionRequest) (*MarketDecision, error) {
	return &MarketDecision{
		Reasoning: "heuristic agent does not participate in P2P market",
	}, nil
}

// EvaluateStrategy returns no changes for the heuristic agent.
func (a *HeuristicAgent) EvaluateStrategy(_ context.Context, _ StrategyEvalRequest) (*StrategyEvaluation, error) {
	return &StrategyEvaluation{
		Reasoning: "heuristic agent does not change strategy parameters",
	}, nil
}

// pickRandomDestination returns a port ID from the reachable set,
// preferring shorter distances.
func (a *HeuristicAgent) pickRandomDestination(reachable map[uuid.UUID]float64) uuid.UUID {
	// Sort by distance, pick the closest.
	type portDist struct {
		id   uuid.UUID
		dist float64
	}
	var ports []portDist
	for id, dist := range reachable {
		ports = append(ports, portDist{id, dist})
	}
	sort.Slice(ports, func(i, j int) bool {
		return ports[i].dist < ports[j].dist
	})

	if len(ports) > 0 {
		return ports[0].id
	}
	// Shouldn't happen since caller checks len > 0.
	for id := range reachable {
		return id
	}
	return uuid.Nil
}
