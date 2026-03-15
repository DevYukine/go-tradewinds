package bot

import (
	"math"
	"sort"
	"sync"

	"github.com/google/uuid"

	"github.com/DevYukine/go-tradewinds/internal/agent"
)

// TradeOpportunity represents a cross-port trade opportunity discovered by
// the ProfitAnalyzer. Used to guide idle ships toward profitable routes.
type TradeOpportunity struct {
	BuyPortID  uuid.UUID `json:"buy_port_id"`
	SellPortID uuid.UUID `json:"sell_port_id"`
	GoodID     uuid.UUID `json:"good_id"`
	BuyPrice   int       `json:"buy_price"`
	SellPrice  int       `json:"sell_price"`
	Profit     int       `json:"profit"`  // per unit, after both buy and sell taxes
	Distance   float64   `json:"distance"`
	Score      float64   `json:"score"` // profit / distance
}

// ProfitAnalyzer continuously evaluates cross-port trade opportunities using
// cached price data. It maintains a ranked list of the top opportunities,
// enabling idle ships to navigate toward known profitable trades instead of
// wandering aimlessly.
type ProfitAnalyzer struct {
	priceCache *PriceCache
	world      *WorldCache
	mu         sync.RWMutex
	top        []TradeOpportunity // ranked by score, max 50
}

// NewProfitAnalyzer creates a new analyzer using the shared price cache and world data.
func NewProfitAnalyzer(priceCache *PriceCache, world *WorldCache) *ProfitAnalyzer {
	return &ProfitAnalyzer{
		priceCache: priceCache,
		world:      world,
	}
}

// Recompute rebuilds the top opportunities list from current price data.
// Called after each full scanner cycle completes. Considers both direct
// routes and 2-hop routes (buy at A, sail through B, sell at C).
func (pa *ProfitAnalyzer) Recompute() {
	prices := pa.priceCache.All()

	// Build indexes: buyable goods by port, sellable goods by port.
	type portGood struct {
		portID uuid.UUID
		goodID uuid.UUID
		price  int
	}

	buyByGood := make(map[uuid.UUID][]portGood)  // goodID -> ports where you can buy
	sellByGood := make(map[uuid.UUID][]portGood) // goodID -> ports where you can sell

	// Build port tax index.
	taxIndex := make(map[uuid.UUID]int, len(pa.world.Ports))
	for _, p := range pa.world.Ports {
		taxIndex[p.ID] = p.TaxRateBps
	}

	for _, pp := range prices {
		if pp.BuyPrice > 0 {
			buyByGood[pp.GoodID] = append(buyByGood[pp.GoodID], portGood{pp.PortID, pp.GoodID, pp.BuyPrice})
		}
		if pp.SellPrice > 0 {
			sellByGood[pp.GoodID] = append(sellByGood[pp.GoodID], portGood{pp.PortID, pp.GoodID, pp.SellPrice})
		}
	}

	// Build adjacency map for 2-hop distance lookups.
	// neighbors[portA] = map[portB] = distance
	portIDs := pa.world.PortIDs()
	neighbors := make(map[uuid.UUID]map[uuid.UUID]float64, len(portIDs))
	for _, pid := range portIDs {
		routes := pa.world.RoutesFrom(pid)
		m := make(map[uuid.UUID]float64, len(routes))
		for _, r := range routes {
			peer := r.ToID
			if peer == pid {
				peer = r.FromID
			}
			m[peer] = r.Distance
		}
		neighbors[pid] = m
	}

	// lookupDist checks direct route, then 2-hop via intermediate port.
	lookupDist := func(from, to uuid.UUID) float64 {
		// Direct route.
		if d, ok := neighbors[from][to]; ok {
			return d
		}
		if d, ok := neighbors[to][from]; ok {
			return d
		}
		// 2-hop: find shortest from->mid->to path.
		bestDist := 0.0
		fromNeighbors := neighbors[from]
		for mid, d1 := range fromNeighbors {
			if mid == to {
				continue
			}
			d2, ok := neighbors[mid][to]
			if !ok {
				d2, ok = neighbors[to][mid]
			}
			if !ok {
				continue
			}
			total := d1 + d2
			if bestDist == 0 || total < bestDist {
				bestDist = total
			}
		}
		return bestDist
	}

	var opportunities []TradeOpportunity

	for goodID, buys := range buyByGood {
		sells, ok := sellByGood[goodID]
		if !ok {
			continue
		}

		for _, buy := range buys {
			buyTax := buy.price * taxIndex[buy.portID] / 10000

			for _, sell := range sells {
				if buy.portID == sell.portID {
					continue // Same port, not a trade route.
				}

				sellTax := sell.price * taxIndex[sell.portID] / 10000
				profit := sell.price - buy.price - buyTax - sellTax
				if profit <= 0 {
					continue
				}

				// Look up distance (direct or 2-hop).
				dist := lookupDist(buy.portID, sell.portID)
				if dist <= 0 {
					continue // No route between these ports (even via 2 hops).
				}

				score := float64(profit) / math.Max(dist, 1.0)

				opportunities = append(opportunities, TradeOpportunity{
					BuyPortID:  buy.portID,
					SellPortID: sell.portID,
					GoodID:     goodID,
					BuyPrice:   buy.price,
					SellPrice:  sell.price,
					Profit:     profit,
					Distance:   dist,
					Score:      score,
				})
			}
		}
	}

	// Sort by score descending and keep top 100 (increased from 50 to capture
	// more opportunities across different ship locations).
	sort.Slice(opportunities, func(i, j int) bool {
		return opportunities[i].Score > opportunities[j].Score
	})
	if len(opportunities) > 100 {
		opportunities = opportunities[:100]
	}

	pa.mu.Lock()
	pa.top = opportunities
	pa.mu.Unlock()
}

// Top returns a snapshot of the current top opportunities.
func (pa *ProfitAnalyzer) Top() []TradeOpportunity {
	pa.mu.RLock()
	defer pa.mu.RUnlock()

	out := make([]TradeOpportunity, len(pa.top))
	copy(out, pa.top)
	return out
}

// ToAgentOpportunities converts the top opportunities to agent-compatible format.
func (pa *ProfitAnalyzer) ToAgentOpportunities() []agent.TradeOpportunity {
	pa.mu.RLock()
	defer pa.mu.RUnlock()

	out := make([]agent.TradeOpportunity, len(pa.top))
	for i, opp := range pa.top {
		out[i] = agent.TradeOpportunity{
			BuyPortID:  opp.BuyPortID,
			SellPortID: opp.SellPortID,
			GoodID:     opp.GoodID,
			BuyPrice:   opp.BuyPrice,
			SellPrice:  opp.SellPrice,
			Profit:     opp.Profit,
			Distance:   opp.Distance,
			Score:      opp.Score,
		}
	}
	return out
}

// lookupDistance finds the distance between two ports using the world cache.
// Checks both directions since routes are bidirectional.
func (pa *ProfitAnalyzer) lookupDistance(fromID, toID uuid.UUID) float64 {
	if r := pa.world.FindRoute(fromID, toID); r != nil {
		return r.Distance
	}
	if r := pa.world.FindRoute(toID, fromID); r != nil {
		return r.Distance
	}
	return 0
}
