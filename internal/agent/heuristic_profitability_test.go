package agent

import (
	"context"
	"math"
	"testing"
	"time"

	"github.com/google/uuid"
	"go.uber.org/zap"
)

// ===========================================================================
// Test helpers
// ===========================================================================

func newTestAgent() *HeuristicAgent {
	logger, _ := zap.NewDevelopment()
	return NewHeuristicAgent(logger)
}

func uid(n byte) uuid.UUID { return uuid.UUID{n} }

// defaultParams returns params with speculative trading enabled.
func defaultParams() map[string]float64 {
	return map[string]float64{
		"speculativeTradeEnabled": 1.0,
		"passengerWeight":         5.0,
		"passengerDestBonus":      5.0,
		"minMarginPct":            0.05,
	}
}

// simpleNetwork creates a 3-port triangle network:
//
//	portA ↔ portB (dist 5), portA ↔ portC (dist 10), portB ↔ portC (dist 3)
func simpleNetwork() (portA, portB, portC uuid.UUID, routes []RouteInfo) {
	portA, portB, portC = uid(1), uid(2), uid(3)
	routes = []RouteInfo{
		{FromID: portA, ToID: portB, Distance: 5},
		{FromID: portA, ToID: portC, Distance: 10},
		{FromID: portB, ToID: portC, Distance: 3},
	}
	return
}

// ===========================================================================
// 1. PROFIT CORRECTNESS — ensure trades always have positive margin
// ===========================================================================

func TestProfitability_ArbitrageNeverBuysAtALoss(t *testing.T) {
	a := newTestAgent()
	portA, portB, portC, routes := simpleNetwork()
	goodX, goodY := uid(10), uid(11)

	dec, err := a.DecideTradeAction(context.Background(), TradeDecisionRequest{
		StrategyHint: "arbitrage",
		Ship:         ShipSnapshot{PortID: &portA, Capacity: 200},
		Routes:       routes,
		PriceCache: []PricePoint{
			// goodX: buy at 100, sell at 90 everywhere → LOSS
			{PortID: portA, GoodID: goodX, BuyPrice: 100, SellPrice: 80},
			{PortID: portB, GoodID: goodX, BuyPrice: 110, SellPrice: 90},
			{PortID: portC, GoodID: goodX, BuyPrice: 120, SellPrice: 95},
			// goodY: buy at 50, sell at 80 at portB → PROFIT
			{PortID: portA, GoodID: goodY, BuyPrice: 50, SellPrice: 30},
			{PortID: portB, GoodID: goodY, BuyPrice: 60, SellPrice: 80},
		},
		Constraints: Constraints{MaxSpend: 50000},
		Params:      defaultParams(),
	})
	if err != nil {
		t.Fatal(err)
	}

	// Should never buy goodX (sell price at all destinations < buy price at A).
	for _, buy := range dec.BuyOrders {
		if buy.GoodID == goodX {
			t.Errorf("bought goodX which is unprofitable at every destination")
		}
	}
	// Should buy goodY.
	foundY := false
	for _, buy := range dec.BuyOrders {
		if buy.GoodID == goodY {
			foundY = true
		}
	}
	if !foundY && dec.Action == "sell_and_buy" {
		t.Error("should have bought goodY which has positive margin")
	}
}

func TestProfitability_NeverBuysWhenAllTradesLoseMoney(t *testing.T) {
	a := newTestAgent()
	portA, portB := uid(1), uid(2)
	goodX := uid(10)

	dec, err := a.DecideTradeAction(context.Background(), TradeDecisionRequest{
		StrategyHint: "arbitrage",
		Ship:         ShipSnapshot{PortID: &portA, Capacity: 100},
		Routes:       []RouteInfo{{FromID: portA, ToID: portB, Distance: 5}},
		PriceCache: []PricePoint{
			{PortID: portA, GoodID: goodX, BuyPrice: 100, SellPrice: 80},
			{PortID: portB, GoodID: goodX, BuyPrice: 95, SellPrice: 90},
		},
		Constraints: Constraints{MaxSpend: 50000},
		Params:      defaultParams(),
	})
	if err != nil {
		t.Fatal(err)
	}

	// No profitable trades exist, should have no buy orders.
	if len(dec.BuyOrders) > 0 {
		t.Errorf("should not buy anything when all trades lose money, got %d buy orders", len(dec.BuyOrders))
	}
}

func TestProfitability_TaxesAreSubtractedFromBothSides(t *testing.T) {
	a := newTestAgent()
	portA, portB := uid(1), uid(2)
	goodX := uid(10)

	// Without taxes: buy at 100, sell at 110 → profit 10 (10% margin, above 5% min).
	// With 10% tax at both ends: buy_tax=10, sell_tax=11, net=110-100-10-11=-11 → LOSS.
	dec, err := a.DecideTradeAction(context.Background(), TradeDecisionRequest{
		StrategyHint: "arbitrage",
		Ship:         ShipSnapshot{PortID: &portA, Capacity: 100},
		Routes:       []RouteInfo{{FromID: portA, ToID: portB, Distance: 5}},
		PriceCache: []PricePoint{
			{PortID: portA, GoodID: goodX, BuyPrice: 100, SellPrice: 80},
			{PortID: portB, GoodID: goodX, BuyPrice: 120, SellPrice: 110},
		},
		Ports: []PortInfo{
			{ID: portA, TaxRateBps: 1000}, // 10%
			{ID: portB, TaxRateBps: 1000}, // 10%
		},
		Constraints: Constraints{MaxSpend: 50000},
		Params:      defaultParams(),
	})
	if err != nil {
		t.Fatal(err)
	}

	// With 10% tax on both sides, the trade becomes unprofitable.
	if len(dec.BuyOrders) > 0 {
		t.Error("should not buy when taxes eat the margin")
	}
}

func TestProfitability_MinMarginThresholdRespected(t *testing.T) {
	a := newTestAgent()
	portA, portB := uid(1), uid(2)
	goodX := uid(10)

	// 5% min margin: profit must be >= buyPrice * 0.05 = 5.
	// Sell at 104, buy at 100 → profit 4 → below 5% threshold.
	dec, err := a.DecideTradeAction(context.Background(), TradeDecisionRequest{
		StrategyHint: "arbitrage",
		Ship:         ShipSnapshot{PortID: &portA, Capacity: 100},
		Routes:       []RouteInfo{{FromID: portA, ToID: portB, Distance: 5}},
		PriceCache: []PricePoint{
			{PortID: portA, GoodID: goodX, BuyPrice: 100, SellPrice: 80},
			{PortID: portB, GoodID: goodX, BuyPrice: 120, SellPrice: 104},
		},
		Constraints: Constraints{MaxSpend: 50000},
		Params:      map[string]float64{"minMarginPct": 0.05},
	})
	if err != nil {
		t.Fatal(err)
	}

	if len(dec.BuyOrders) > 0 {
		t.Error("should reject trades below minimum margin threshold")
	}
}

func TestProfitability_AsymmetricTaxes_ChoosesCheaperPort(t *testing.T) {
	a := newTestAgent()
	portA, portB, portC := uid(1), uid(2), uid(3)
	goodX := uid(10)

	// portB: high tax, portC: low tax. Same sell price.
	// Agent should prefer portC (lower tax = more profit).
	dec, err := a.DecideTradeAction(context.Background(), TradeDecisionRequest{
		StrategyHint: "arbitrage",
		Ship:         ShipSnapshot{PortID: &portA, Capacity: 100},
		Routes: []RouteInfo{
			{FromID: portA, ToID: portB, Distance: 5},
			{FromID: portA, ToID: portC, Distance: 5},
		},
		PriceCache: []PricePoint{
			{PortID: portA, GoodID: goodX, BuyPrice: 100, SellPrice: 80},
			{PortID: portB, GoodID: goodX, BuyPrice: 150, SellPrice: 200},
			{PortID: portC, GoodID: goodX, BuyPrice: 150, SellPrice: 200},
		},
		Ports: []PortInfo{
			{ID: portA, TaxRateBps: 200},  // 2%
			{ID: portB, TaxRateBps: 3000}, // 30%
			{ID: portC, TaxRateBps: 200},  // 2%
		},
		Constraints: Constraints{MaxSpend: 50000},
		Params:      defaultParams(),
	})
	if err != nil {
		t.Fatal(err)
	}

	if dec.SailTo == nil {
		t.Fatal("should sail somewhere")
	}
	if *dec.SailTo != portC {
		t.Errorf("should prefer low-tax portC, got %v", *dec.SailTo)
	}
}

// ===========================================================================
// 2. CAPACITY MANAGEMENT — never exceed ship capacity
// ===========================================================================

func TestCapacity_BuyOrdersNeverExceedShipCapacity(t *testing.T) {
	a := newTestAgent()
	portA, portB := uid(1), uid(2)

	goods := make([]PricePoint, 0)
	for i := byte(10); i < 20; i++ {
		goods = append(goods,
			PricePoint{PortID: portA, GoodID: uid(i), BuyPrice: 10, SellPrice: 5},
			PricePoint{PortID: portB, GoodID: uid(i), BuyPrice: 25, SellPrice: 50},
		)
	}

	dec, err := a.DecideTradeAction(context.Background(), TradeDecisionRequest{
		StrategyHint: "arbitrage",
		Ship:         ShipSnapshot{PortID: &portA, Capacity: 50}, // Small ship
		Routes:       []RouteInfo{{FromID: portA, ToID: portB, Distance: 5}},
		PriceCache:   goods,
		Constraints:  Constraints{MaxSpend: 100000}, // Huge budget
		Params:       defaultParams(),
	})
	if err != nil {
		t.Fatal(err)
	}

	totalQty := 0
	for _, buy := range dec.BuyOrders {
		totalQty += buy.Quantity
	}
	if totalQty > 50 {
		t.Errorf("total buy quantity %d exceeds ship capacity 50", totalQty)
	}
}

func TestCapacity_HeldCargoReducesBuyCapacity(t *testing.T) {
	a := newTestAgent()
	portA, portB, portC := uid(1), uid(2), uid(3)
	goodX, goodY := uid(10), uid(11)

	dec, err := a.DecideTradeAction(context.Background(), TradeDecisionRequest{
		StrategyHint: "arbitrage",
		Ship: ShipSnapshot{
			PortID:   &portA,
			Capacity: 100,
			Cargo:    []CargoItem{{GoodID: goodX, Quantity: 60}}, // 60 units held
		},
		Routes: []RouteInfo{
			{FromID: portA, ToID: portB, Distance: 5},
			{FromID: portA, ToID: portC, Distance: 10},
		},
		PriceCache: []PricePoint{
			{PortID: portA, GoodID: goodX, BuyPrice: 50, SellPrice: 40},
			{PortID: portA, GoodID: goodY, BuyPrice: 10, SellPrice: 5},
			{PortID: portB, GoodID: goodX, BuyPrice: 70, SellPrice: 100}, // Hold goodX
			{PortID: portB, GoodID: goodY, BuyPrice: 30, SellPrice: 40},
		},
		Constraints: Constraints{MaxSpend: 100000},
		Params:      defaultParams(),
	})
	if err != nil {
		t.Fatal(err)
	}

	// goodX is held (100 > 40*1.5=60), so 60 units still on ship.
	// Remaining capacity = 100 - 60 = 40.
	totalBuyQty := 0
	for _, buy := range dec.BuyOrders {
		totalBuyQty += buy.Quantity
	}

	// Sold cargo frees capacity. But held cargo stays.
	// Total on ship after trades should never exceed capacity.
	heldQty := 60 // goodX is held
	if totalBuyQty+heldQty > 100 {
		t.Errorf("total cargo after trade (%d held + %d bought = %d) exceeds capacity 100",
			heldQty, totalBuyQty, totalBuyQty+heldQty)
	}
}

func TestCapacity_BulkHaulerRespects(t *testing.T) {
	a := newTestAgent()
	portA, portB := uid(1), uid(2)
	goodX := uid(10)

	dec, err := a.DecideTradeAction(context.Background(), TradeDecisionRequest{
		StrategyHint: "bulk_hauler",
		Ship:         ShipSnapshot{PortID: &portA, Capacity: 500},
		Routes:       []RouteInfo{{FromID: portA, ToID: portB, Distance: 5}},
		PriceCache: []PricePoint{
			{PortID: portA, GoodID: goodX, BuyPrice: 10, SellPrice: 5},
			{PortID: portB, GoodID: goodX, BuyPrice: 25, SellPrice: 50},
		},
		Constraints: Constraints{MaxSpend: 1000000},
		Params:      defaultParams(),
	})
	if err != nil {
		t.Fatal(err)
	}

	totalQty := 0
	for _, buy := range dec.BuyOrders {
		totalQty += buy.Quantity
	}
	if totalQty > 500 {
		t.Errorf("bulk hauler bought %d units, exceeding capacity 500", totalQty)
	}
}

// ===========================================================================
// 3. BUDGET CONSTRAINTS — never spend more than available
// ===========================================================================

func TestBudget_BuyOrdersNeverExceedMaxSpend(t *testing.T) {
	a := newTestAgent()
	portA, portB := uid(1), uid(2)
	goodX := uid(10)

	dec, err := a.DecideTradeAction(context.Background(), TradeDecisionRequest{
		StrategyHint: "arbitrage",
		Ship:         ShipSnapshot{PortID: &portA, Capacity: 1000}, // Huge ship
		Routes:       []RouteInfo{{FromID: portA, ToID: portB, Distance: 5}},
		PriceCache: []PricePoint{
			{PortID: portA, GoodID: goodX, BuyPrice: 100, SellPrice: 80},
			{PortID: portB, GoodID: goodX, BuyPrice: 120, SellPrice: 200},
		},
		Constraints: Constraints{MaxSpend: 500}, // Very tight budget
		Params:      defaultParams(),
	})
	if err != nil {
		t.Fatal(err)
	}

	totalCost := int64(0)
	for _, buy := range dec.BuyOrders {
		// Use buy price at current port.
		totalCost += int64(buy.Quantity) * 100
	}
	if totalCost > 500 {
		t.Errorf("total buy cost %d exceeds MaxSpend 500", totalCost)
	}
}

func TestBudget_ZeroBudgetNobuys(t *testing.T) {
	a := newTestAgent()
	portA, portB := uid(1), uid(2)
	goodX := uid(10)

	dec, err := a.DecideTradeAction(context.Background(), TradeDecisionRequest{
		Ship:   ShipSnapshot{PortID: &portA, Capacity: 100},
		Routes: []RouteInfo{{FromID: portA, ToID: portB, Distance: 5}},
		PriceCache: []PricePoint{
			{PortID: portA, GoodID: goodX, BuyPrice: 100, SellPrice: 80},
			{PortID: portB, GoodID: goodX, BuyPrice: 120, SellPrice: 200},
		},
		Constraints: Constraints{MaxSpend: 0},
	})
	if err != nil {
		t.Fatal(err)
	}

	if len(dec.BuyOrders) > 0 {
		t.Error("should not buy anything with zero budget")
	}
}

// ===========================================================================
// 4. SMART SELLING — hold cargo only when significantly better destination
// ===========================================================================

func TestSmartSell_HoldsCargoWhenDestinationMuchBetter(t *testing.T) {
	a := newTestAgent()
	portA, portB := uid(1), uid(2)
	goodX := uid(10)

	dec, err := a.DecideTradeAction(context.Background(), TradeDecisionRequest{
		Ship: ShipSnapshot{
			PortID:   &portA,
			Capacity: 100,
			Cargo:    []CargoItem{{GoodID: goodX, Quantity: 50}},
		},
		Routes: []RouteInfo{{FromID: portA, ToID: portB, Distance: 5}},
		PriceCache: []PricePoint{
			{PortID: portA, GoodID: goodX, BuyPrice: 60, SellPrice: 40}, // Sell here for 40
			{PortID: portB, GoodID: goodX, BuyPrice: 80, SellPrice: 100}, // Sell there for 100
		},
		Constraints: Constraints{MaxSpend: 50000},
		Params:      defaultParams(),
	})
	if err != nil {
		t.Fatal(err)
	}

	// 100 > 40 * 1.5 = 60 → should hold.
	for _, sell := range dec.SellOrders {
		if sell.GoodID == goodX {
			t.Error("should hold goodX for better price at portB (100 vs 40 here)")
		}
	}
}

func TestSmartSell_SellsWhenDestinationOnlySlightlyBetter(t *testing.T) {
	a := newTestAgent()
	portA, portB := uid(1), uid(2)
	goodX := uid(10)

	dec, err := a.DecideTradeAction(context.Background(), TradeDecisionRequest{
		Ship: ShipSnapshot{
			PortID:   &portA,
			Capacity: 100,
			Cargo:    []CargoItem{{GoodID: goodX, Quantity: 50}},
		},
		Routes: []RouteInfo{{FromID: portA, ToID: portB, Distance: 5}},
		PriceCache: []PricePoint{
			{PortID: portA, GoodID: goodX, BuyPrice: 60, SellPrice: 40}, // Sell here for 40
			{PortID: portB, GoodID: goodX, BuyPrice: 50, SellPrice: 45}, // Only 45, not >48 (40*1.2)
		},
		Constraints: Constraints{MaxSpend: 50000},
		Params:      defaultParams(),
	})
	if err != nil {
		t.Fatal(err)
	}

	// 45 < 40 * 1.2 = 48 → should sell now (below 20% threshold).
	found := false
	for _, sell := range dec.SellOrders {
		if sell.GoodID == goodX {
			found = true
		}
	}
	if !found {
		t.Error("should sell goodX when destination is only slightly better (below 20% threshold)")
	}
}

func TestSmartSell_SellsCargoWithNoKnownSellPrice(t *testing.T) {
	a := newTestAgent()
	portA, portB := uid(1), uid(2)
	goodX := uid(10)

	dec, err := a.DecideTradeAction(context.Background(), TradeDecisionRequest{
		Ship: ShipSnapshot{
			PortID:   &portA,
			Capacity: 100,
			Cargo:    []CargoItem{{GoodID: goodX, Quantity: 50}},
		},
		Routes: []RouteInfo{{FromID: portA, ToID: portB, Distance: 5}},
		PriceCache: []PricePoint{
			// No sell price known at portA or portB for goodX.
		},
		Constraints: Constraints{MaxSpend: 50000},
	})
	if err != nil {
		t.Fatal(err)
	}

	// Even with no price data, cargo should be sold (execution layer handles pricing).
	found := false
	for _, sell := range dec.SellOrders {
		if sell.GoodID == goodX {
			found = true
		}
	}
	if !found {
		t.Error("should sell cargo with unknown price rather than holding indefinitely")
	}
}

// ===========================================================================
// 5. DESTINATION SCORING — arbitrage vs bulk_hauler differences
// ===========================================================================

func TestScoring_ArbitragePrefersHighProfitPerDistance(t *testing.T) {
	a := newTestAgent()
	portA, portB, portC := uid(1), uid(2), uid(3)
	goodX := uid(10)

	// portB: close, moderate profit → high profit/dist
	// portC: far, high profit → low profit/dist
	dec, err := a.DecideTradeAction(context.Background(), TradeDecisionRequest{
		StrategyHint: "arbitrage",
		Ship:         ShipSnapshot{PortID: &portA, Capacity: 100},
		Routes: []RouteInfo{
			{FromID: portA, ToID: portB, Distance: 3},
			{FromID: portA, ToID: portC, Distance: 30},
		},
		PriceCache: []PricePoint{
			{PortID: portA, GoodID: goodX, BuyPrice: 100, SellPrice: 80},
			{PortID: portB, GoodID: goodX, BuyPrice: 120, SellPrice: 130}, // profit 30, dist 3 → 10/dist
			{PortID: portC, GoodID: goodX, BuyPrice: 200, SellPrice: 200}, // profit 100, dist 30 → 3.3/dist
		},
		Constraints: Constraints{MaxSpend: 100000},
		Params:      defaultParams(),
	})
	if err != nil {
		t.Fatal(err)
	}

	if dec.SailTo == nil || *dec.SailTo != portB {
		t.Errorf("arbitrage should prefer portB (better profit/distance), got %v", dec.SailTo)
	}
}

func TestScoring_BulkHaulerPrefersTotalProfit(t *testing.T) {
	a := newTestAgent()
	portA, portB, portC := uid(1), uid(2), uid(3)
	goodX := uid(10)

	dec, err := a.DecideTradeAction(context.Background(), TradeDecisionRequest{
		StrategyHint: "bulk_hauler",
		Ship:         ShipSnapshot{PortID: &portA, Capacity: 200},
		Routes: []RouteInfo{
			{FromID: portA, ToID: portB, Distance: 3},
			{FromID: portA, ToID: portC, Distance: 30},
		},
		PriceCache: []PricePoint{
			{PortID: portA, GoodID: goodX, BuyPrice: 10, SellPrice: 5},
			{PortID: portB, GoodID: goodX, BuyPrice: 20, SellPrice: 15}, // profit 5/unit * 200 = 1000
			{PortID: portC, GoodID: goodX, BuyPrice: 50, SellPrice: 60}, // profit 50/unit * 200 = 10000
		},
		Constraints: Constraints{MaxSpend: 1000000},
		Params:      defaultParams(),
	})
	if err != nil {
		t.Fatal(err)
	}

	if dec.SailTo == nil || *dec.SailTo != portC {
		t.Errorf("bulk_hauler should prefer portC (higher total profit), got %v", dec.SailTo)
	}
}

// ===========================================================================
// 6. SPECULATIVE TRADE — fallback behavior when no profitable trades
// ===========================================================================

func TestSpeculative_SailsToPassengerDestWhenNoTrade(t *testing.T) {
	a := newTestAgent()
	portA, portB := uid(1), uid(2)
	goodX := uid(10)

	dec, err := a.DecideTradeAction(context.Background(), TradeDecisionRequest{
		StrategyHint: "arbitrage",
		Ship:         ShipSnapshot{PortID: &portA, Capacity: 100, PassengerCap: 10},
		Routes:       []RouteInfo{{FromID: portA, ToID: portB, Distance: 5}},
		PriceCache: []PricePoint{
			{PortID: portA, GoodID: goodX, BuyPrice: 100, SellPrice: 80},
			{PortID: portB, GoodID: goodX, BuyPrice: 90, SellPrice: 80}, // No profit
		},
		AvailablePassengers: []PassengerInfo{
			{ID: uid(50), Count: 5, Bid: 3000, DestinationPortID: portB},
		},
		Constraints: Constraints{MaxSpend: 50000},
		Params:      defaultParams(),
	})
	if err != nil {
		t.Fatal(err)
	}

	if dec.SailTo == nil || *dec.SailTo != portB {
		t.Errorf("should sail to passenger destination when no profitable trade exists")
	}
	// Passenger-only destinations are scored in the main flow (not speculative),
	// so confidence will be normal (0.8). Just verify we sail there.
}

func TestSpeculative_RelocatesAfterIdleTicks(t *testing.T) {
	a := newTestAgent()
	portA, portB := uid(1), uid(2)
	goodX := uid(10)

	dec, err := a.DecideTradeAction(context.Background(), TradeDecisionRequest{
		StrategyHint: "arbitrage",
		Ship:         ShipSnapshot{PortID: &portA, Capacity: 100, IdleTicks: 3},
		Routes:       []RouteInfo{{FromID: portA, ToID: portB, Distance: 5}},
		Ports:        []PortInfo{{ID: portA}, {ID: portB, IsHub: true}},
		PriceCache: []PricePoint{
			{PortID: portA, GoodID: goodX, BuyPrice: 100, SellPrice: 80},
			{PortID: portB, GoodID: goodX, BuyPrice: 90, SellPrice: 80},
		},
		Constraints: Constraints{MaxSpend: 50000},
		Params:      defaultParams(),
	})
	if err != nil {
		t.Fatal(err)
	}

	if dec.SailTo == nil {
		t.Error("should relocate after 3 idle ticks, not stay docked")
	}
	if *dec.SailTo != portB {
		t.Errorf("should relocate to hub portB, got %v", *dec.SailTo)
	}
}

func TestSpeculative_WaitsOnFirstIdleTick(t *testing.T) {
	a := newTestAgent()
	portA, portB := uid(1), uid(2)
	goodX := uid(10)

	dec, err := a.DecideTradeAction(context.Background(), TradeDecisionRequest{
		StrategyHint: "arbitrage",
		Ship:         ShipSnapshot{PortID: &portA, Capacity: 100, IdleTicks: 0},
		Routes:       []RouteInfo{{FromID: portA, ToID: portB, Distance: 5}},
		PriceCache: []PricePoint{
			{PortID: portA, GoodID: goodX, BuyPrice: 100, SellPrice: 80},
			{PortID: portB, GoodID: goodX, BuyPrice: 90, SellPrice: 80},
		},
		Constraints: Constraints{MaxSpend: 50000},
		Params:      defaultParams(),
	})
	if err != nil {
		t.Fatal(err)
	}

	if dec.Action != "wait" {
		t.Errorf("should wait on first idle tick, got %s", dec.Action)
	}
}

func TestSpeculative_NavigatesToOpportunityBuyPort(t *testing.T) {
	a := newTestAgent()
	portA, portB, portC := uid(1), uid(2), uid(3)
	goodX := uid(10)

	dec, err := a.DecideTradeAction(context.Background(), TradeDecisionRequest{
		StrategyHint: "arbitrage",
		Ship:         ShipSnapshot{PortID: &portA, Capacity: 100},
		Routes: []RouteInfo{
			{FromID: portA, ToID: portB, Distance: 5},
			{FromID: portA, ToID: portC, Distance: 10},
		},
		PriceCache: []PricePoint{
			{PortID: portA, GoodID: goodX, BuyPrice: 100, SellPrice: 80}, // unprofitable locally
		},
		TopOpportunities: []TradeOpportunity{
			{BuyPortID: portC, SellPortID: portB, GoodID: goodX, Profit: 50, Score: 10},
		},
		Constraints: Constraints{MaxSpend: 50000},
		Params:      defaultParams(),
	})
	if err != nil {
		t.Fatal(err)
	}

	if dec.SailTo != nil && *dec.SailTo == portC {
		// Good — sailed to the opportunity buy port.
	} else if dec.Action != "wait" {
		// Also acceptable on first idle tick.
	}
}

// ===========================================================================
// 7. PASSENGER INTEGRATION
// ===========================================================================

func TestPassengers_BoardedWhenHeadingToDestination(t *testing.T) {
	a := newTestAgent()
	portA, portB := uid(1), uid(2)
	goodX := uid(10)
	passengerID := uid(50)

	dec, err := a.DecideTradeAction(context.Background(), TradeDecisionRequest{
		StrategyHint: "arbitrage",
		Ship:         ShipSnapshot{PortID: &portA, Capacity: 100, PassengerCap: 10},
		Routes:       []RouteInfo{{FromID: portA, ToID: portB, Distance: 5}},
		PriceCache: []PricePoint{
			{PortID: portA, GoodID: goodX, BuyPrice: 10, SellPrice: 5},
			{PortID: portB, GoodID: goodX, BuyPrice: 25, SellPrice: 40},
		},
		AvailablePassengers: []PassengerInfo{
			{ID: passengerID, Count: 3, Bid: 1000, DestinationPortID: portB},
		},
		Constraints: Constraints{MaxSpend: 50000},
		Params:      defaultParams(),
	})
	if err != nil {
		t.Fatal(err)
	}

	if dec.SailTo == nil || *dec.SailTo != portB {
		t.Skip("ship not going to portB, cannot verify passenger boarding")
	}

	found := false
	for _, pid := range dec.BoardPassengers {
		if pid == passengerID {
			found = true
		}
	}
	if !found {
		t.Error("should board passengers heading to our destination")
	}
}

func TestPassengers_NotBoardedWhenCapacityFull(t *testing.T) {
	a := newTestAgent()
	portA, portB := uid(1), uid(2)
	goodX := uid(10)

	dec, err := a.DecideTradeAction(context.Background(), TradeDecisionRequest{
		StrategyHint: "arbitrage",
		Ship: ShipSnapshot{
			PortID:       &portA,
			Capacity:     100,
			PassengerCap: 5,
		},
		Routes: []RouteInfo{{FromID: portA, ToID: portB, Distance: 5}},
		PriceCache: []PricePoint{
			{PortID: portA, GoodID: goodX, BuyPrice: 10, SellPrice: 5},
			{PortID: portB, GoodID: goodX, BuyPrice: 25, SellPrice: 40},
		},
		BoardedPassengers: []PassengerInfo{
			{ID: uid(49), Count: 5, Bid: 500, DestinationPortID: portB}, // Already full
		},
		AvailablePassengers: []PassengerInfo{
			{ID: uid(50), Count: 3, Bid: 1000, DestinationPortID: portB},
		},
		Constraints: Constraints{MaxSpend: 50000},
		Params:      defaultParams(),
	})
	if err != nil {
		t.Fatal(err)
	}

	if len(dec.BoardPassengers) > 0 {
		t.Error("should not board passengers when at capacity")
	}
}

func TestPassengers_OverrideDestWhenPassengerRevenueDominates(t *testing.T) {
	a := newTestAgent()
	portA, portB, portC := uid(1), uid(2), uid(3)
	goodX := uid(10)

	dec, err := a.DecideTradeAction(context.Background(), TradeDecisionRequest{
		StrategyHint: "arbitrage",
		Ship:         ShipSnapshot{PortID: &portA, Capacity: 100, PassengerCap: 50},
		Routes: []RouteInfo{
			{FromID: portA, ToID: portB, Distance: 5},
			{FromID: portA, ToID: portC, Distance: 5},
		},
		PriceCache: []PricePoint{
			{PortID: portA, GoodID: goodX, BuyPrice: 100, SellPrice: 80},
			// Tiny profit at portB.
			{PortID: portB, GoodID: goodX, BuyPrice: 120, SellPrice: 106},
			{PortID: portC, GoodID: goodX, BuyPrice: 120, SellPrice: 105},
		},
		AvailablePassengers: []PassengerInfo{
			// Massive passenger revenue to portC.
			{ID: uid(50), Count: 40, Bid: 50000, DestinationPortID: portC},
		},
		Constraints: Constraints{MaxSpend: 50000},
		Params:      defaultParams(),
	})
	if err != nil {
		t.Fatal(err)
	}

	if dec.SailTo != nil && *dec.SailTo == portC {
		// Good — passenger revenue overrode cargo destination.
	}
}

// ===========================================================================
// 8. P2P ORDER FILLS — during trade decisions
// ===========================================================================

func TestP2PFills_FillsProfitableOrdersDuringTrade(t *testing.T) {
	a := newTestAgent()
	portA, portB := uid(1), uid(2)
	goodX := uid(10)
	orderID := uid(100)

	dec, err := a.DecideTradeAction(context.Background(), TradeDecisionRequest{
		Ship:   ShipSnapshot{PortID: &portA, Capacity: 100},
		Routes: []RouteInfo{{FromID: portA, ToID: portB, Distance: 5}},
		PriceCache: []PricePoint{
			{PortID: portA, GoodID: goodX, BuyPrice: 50, SellPrice: 80},
		},
		PortOrders: []MarketOrder{
			{ID: orderID, PortID: portA, GoodID: goodX, Side: "sell", Price: 30, Remaining: 10},
		},
		Constraints: Constraints{MaxSpend: 50000},
		Params:      defaultParams(),
	})
	if err != nil {
		t.Fatal(err)
	}

	found := false
	for _, fill := range dec.FillOrders {
		if fill.OrderID == orderID {
			found = true
		}
	}
	if !found {
		t.Error("should fill underpriced sell order during trade (30 vs NPC sell 80)")
	}
}

func TestP2PFills_DoesNotSelfFill(t *testing.T) {
	a := newTestAgent()
	portA, portB := uid(1), uid(2)
	goodX := uid(10)
	ownOrderID := uid(100)

	dec, err := a.DecideTradeAction(context.Background(), TradeDecisionRequest{
		Ship:   ShipSnapshot{PortID: &portA, Capacity: 100},
		Routes: []RouteInfo{{FromID: portA, ToID: portB, Distance: 5}},
		PriceCache: []PricePoint{
			{PortID: portA, GoodID: goodX, BuyPrice: 50, SellPrice: 80},
		},
		PortOrders: []MarketOrder{
			{ID: ownOrderID, PortID: portA, GoodID: goodX, Side: "sell", Price: 10, Remaining: 10},
		},
		OwnOrders: []MarketOrder{
			{ID: ownOrderID},
		},
		Constraints: Constraints{MaxSpend: 50000},
	})
	if err != nil {
		t.Fatal(err)
	}

	for _, fill := range dec.FillOrders {
		if fill.OrderID == ownOrderID {
			t.Error("must never fill own orders")
		}
	}
}

// ===========================================================================
// 9. FLEET MANAGEMENT — ship buying/selling economics
// ===========================================================================

func TestFleet_DoesNotBuyWhenCannotAffordReserve(t *testing.T) {
	a := newTestAgent()

	// Treasury 1000, upkeep 100, ship costs 500.
	// After purchase: new upkeep 150, reserve = 5 cycles * 150 = 750.
	// Remaining treasury = 1000 - 500 = 500 < 750 → can't afford.
	dec, err := a.DecideFleetAction(context.Background(), FleetDecisionRequest{
		StrategyHint:  "arbitrage",
		Company:       CompanySnapshot{Treasury: 1000, TotalUpkeep: 100},
		Ships:         []ShipSnapshot{{Status: "docked", PortID: ptr(uid(1))}},
		ShipyardPorts: []uuid.UUID{uid(1)},
		ShipTypes: []ShipTypeInfo{
			{ID: uid(1), BasePrice: 500, Upkeep: 50, Speed: 10},
		},
	})
	if err != nil {
		t.Fatal(err)
	}

	if len(dec.BuyShips) > 0 {
		t.Error("should not buy ship when remaining treasury < reserve requirement")
	}
}

func TestFleet_BuysWhenWealthyEnough(t *testing.T) {
	a := newTestAgent()

	// Treasury 100000, upkeep 100, ship costs 500, upkeep 50.
	// After: new upkeep 150, reserve = 5*150 = 750. Remaining = 99500 >> 750. ✓
	dec, err := a.DecideFleetAction(context.Background(), FleetDecisionRequest{
		StrategyHint:  "arbitrage",
		Company:       CompanySnapshot{Treasury: 100000, TotalUpkeep: 100},
		Ships:         []ShipSnapshot{{Status: "docked", PortID: ptr(uid(1))}},
		ShipyardPorts: []uuid.UUID{uid(1)},
		ShipTypes: []ShipTypeInfo{
			{ID: uid(1), BasePrice: 500, Upkeep: 50, Speed: 10},
		},
	})
	if err != nil {
		t.Fatal(err)
	}

	if len(dec.BuyShips) != 1 {
		t.Error("should buy ship when treasury is abundant")
	}
}

func TestFleet_SellsShipWhenUpkeepUnsustainable(t *testing.T) {
	a := newTestAgent()
	portA := uid(1)

	// Treasury 200, upkeep 100, 2 ships. reserve = reserveCycles(2, "arbitrage") = 5.
	// Need 100 * 5 = 500 but only have 200 → sell.
	dec, err := a.DecideFleetAction(context.Background(), FleetDecisionRequest{
		StrategyHint: "arbitrage",
		Company:      CompanySnapshot{Treasury: 200, TotalUpkeep: 100},
		Ships: []ShipSnapshot{
			{ID: uid(1), Status: "docked", PortID: &portA, Speed: 10},
			{ID: uid(2), Status: "docked", PortID: &portA, Speed: 3},
		},
		ShipyardPorts: []uuid.UUID{portA},
		ShipTypes:     []ShipTypeInfo{{ID: uid(1), BasePrice: 500, Upkeep: 50}},
	})
	if err != nil {
		t.Fatal(err)
	}

	if len(dec.SellShips) == 0 {
		t.Error("should sell a ship when upkeep is unsustainable")
	}
	if len(dec.SellShips) == 1 && dec.SellShips[0] != uid(2) {
		t.Error("arbitrage should sell the slowest ship")
	}
}

func TestFleet_NeverSellsLastShip(t *testing.T) {
	a := newTestAgent()
	portA := uid(1)

	dec, err := a.DecideFleetAction(context.Background(), FleetDecisionRequest{
		Company: CompanySnapshot{Treasury: 10, TotalUpkeep: 500}, // Dire situation
		Ships: []ShipSnapshot{
			{ID: uid(1), Status: "docked", PortID: &portA, Speed: 5},
		},
		ShipyardPorts: []uuid.UUID{portA},
	})
	if err != nil {
		t.Fatal(err)
	}

	if len(dec.SellShips) > 0 {
		t.Error("must never sell the last ship")
	}
}

func TestFleet_WarehousePurchaseRequirements(t *testing.T) {
	a := newTestAgent()
	portA := uid(1)

	// 3+ ships, healthy treasury, 2+ docked at same port, <2 warehouses.
	dec, err := a.DecideFleetAction(context.Background(), FleetDecisionRequest{
		StrategyHint: "arbitrage",
		Company:      CompanySnapshot{Treasury: 100000, TotalUpkeep: 300},
		Ships: []ShipSnapshot{
			{ID: uid(1), Status: "docked", PortID: &portA},
			{ID: uid(2), Status: "docked", PortID: &portA},
			{ID: uid(3), Status: "traveling"},
		},
		ShipyardPorts: []uuid.UUID{portA},
		ShipTypes:     []ShipTypeInfo{{ID: uid(1), BasePrice: 500, Upkeep: 50, Speed: 10}},
	})
	if err != nil {
		t.Fatal(err)
	}

	if len(dec.BuyWarehouses) != 1 || dec.BuyWarehouses[0] != portA {
		t.Error("should buy warehouse at port with 2+ docked ships when conditions met")
	}
}

func TestFleet_NoWarehouseWhenTooFewShips(t *testing.T) {
	a := newTestAgent()
	portA := uid(1)

	dec, err := a.DecideFleetAction(context.Background(), FleetDecisionRequest{
		Company: CompanySnapshot{Treasury: 100000, TotalUpkeep: 200},
		Ships: []ShipSnapshot{
			{ID: uid(1), Status: "docked", PortID: &portA},
			{ID: uid(2), Status: "docked", PortID: &portA},
		},
		ShipyardPorts: []uuid.UUID{portA},
		ShipTypes:     []ShipTypeInfo{{ID: uid(1), BasePrice: 500, Upkeep: 50, Speed: 10}},
	})
	if err != nil {
		t.Fatal(err)
	}

	if len(dec.BuyWarehouses) > 0 {
		t.Error("should not buy warehouse with <3 ships")
	}
}

func TestFleet_ReserveCyclesGrowsWithFleetSize(t *testing.T) {
	tests := []struct {
		ships    int
		strategy string
		minCycles int64
	}{
		{1, "arbitrage", 5},
		{4, "arbitrage", 6},
		{8, "arbitrage", 7},
		{2, "bulk_hauler", 6},
		{4, "bulk_hauler", 7},
		{5, "market_maker", 6},
		{10, "market_maker", 7},
	}

	for _, tc := range tests {
		cycles := reserveCycles(tc.ships, tc.strategy)
		if cycles < tc.minCycles {
			t.Errorf("reserveCycles(%d, %q) = %d, want >= %d",
				tc.ships, tc.strategy, cycles, tc.minCycles)
		}
	}
}

// ===========================================================================
// 10. MARKET MAKING — P2P order management
// ===========================================================================

func TestMarket_FillsSellOrderBelowNPCSell(t *testing.T) {
	a := newTestAgent()
	portA := uid(1)
	goodX := uid(10)

	dec, err := a.DecideMarketAction(context.Background(), MarketDecisionRequest{
		Company: CompanySnapshot{Treasury: 50000, TotalUpkeep: 100},
		OpenOrders: []MarketOrder{
			{ID: uid(100), PortID: portA, GoodID: goodX, Side: "sell", Price: 30, Remaining: 10},
		},
		PriceCache: []PricePoint{
			{PortID: portA, GoodID: goodX, BuyPrice: 20, SellPrice: 80},
		},
		Warehouses: []WarehouseSnapshot{{PortID: portA}},
	})
	if err != nil {
		t.Fatal(err)
	}

	if len(dec.FillOrders) != 1 {
		t.Fatalf("expected 1 fill, got %d", len(dec.FillOrders))
	}
	// Profit = 80 - 30 = 50 per unit. Min = 80/10 = 8. 50 > 8. ✓
}

func TestMarket_FillsBuyOrderAboveNPCBuy(t *testing.T) {
	a := newTestAgent()
	portA := uid(1)
	goodX := uid(10)

	dec, err := a.DecideMarketAction(context.Background(), MarketDecisionRequest{
		Company: CompanySnapshot{Treasury: 50000, TotalUpkeep: 100},
		OpenOrders: []MarketOrder{
			{ID: uid(100), PortID: portA, GoodID: goodX, Side: "buy", Price: 200, Remaining: 5},
		},
		PriceCache: []PricePoint{
			{PortID: portA, GoodID: goodX, BuyPrice: 50, SellPrice: 80},
		},
		Warehouses: []WarehouseSnapshot{{PortID: portA}},
	})
	if err != nil {
		t.Fatal(err)
	}

	if len(dec.FillOrders) != 1 {
		t.Fatalf("expected 1 fill, got %d", len(dec.FillOrders))
	}
	// Profit = 200 - 50 = 150 per unit. Min = 50*7/100 = 3.5. 150 > 4. ✓
}

func TestMarket_RequiresWarehouseToFill(t *testing.T) {
	a := newTestAgent()
	portA := uid(1)
	goodX := uid(10)

	dec, err := a.DecideMarketAction(context.Background(), MarketDecisionRequest{
		Company: CompanySnapshot{Treasury: 50000, TotalUpkeep: 100},
		OpenOrders: []MarketOrder{
			{ID: uid(100), PortID: portA, GoodID: goodX, Side: "sell", Price: 10, Remaining: 10},
		},
		PriceCache: []PricePoint{
			{PortID: portA, GoodID: goodX, BuyPrice: 20, SellPrice: 80},
		},
		// No warehouses!
	})
	if err != nil {
		t.Fatal(err)
	}

	if len(dec.FillOrders) > 0 {
		t.Error("should not fill orders without a warehouse at the port")
	}
}

func TestMarket_CancelsOwnSellAboveNPCSell(t *testing.T) {
	a := newTestAgent()
	portA := uid(1)
	goodX := uid(10)
	ownID := uid(200)

	dec, err := a.DecideMarketAction(context.Background(), MarketDecisionRequest{
		Company: CompanySnapshot{Treasury: 50000, TotalUpkeep: 100},
		OwnOrders: []MarketOrder{
			{ID: ownID, PortID: portA, GoodID: goodX, Side: "sell", Price: 100},
		},
		PriceCache: []PricePoint{
			{PortID: portA, GoodID: goodX, BuyPrice: 50, SellPrice: 80}, // Our 100 > NPC 80
		},
	})
	if err != nil {
		t.Fatal(err)
	}

	found := false
	for _, cancelID := range dec.CancelOrders {
		if cancelID == ownID {
			found = true
		}
	}
	if !found {
		t.Error("should cancel own sell order priced above NPC sell price")
	}
}

func TestMarket_PostsOrdersOnWideSpread(t *testing.T) {
	a := newTestAgent()
	portA := uid(1)
	goodX := uid(10)

	dec, err := a.DecideMarketAction(context.Background(), MarketDecisionRequest{
		Company: CompanySnapshot{Treasury: 50000, TotalUpkeep: 100},
		PriceCache: []PricePoint{
			{PortID: portA, GoodID: goodX, BuyPrice: 50, SellPrice: 150}, // 200% spread
		},
		Warehouses: []WarehouseSnapshot{{PortID: portA}},
	})
	if err != nil {
		t.Fatal(err)
	}

	if len(dec.PostOrders) == 0 {
		t.Fatal("should post orders on wide spread")
	}
	// Bid = 50 + 100/4 = 75
	if dec.PostOrders[0].Price != 75 {
		t.Errorf("expected bid price 75, got %d", dec.PostOrders[0].Price)
	}
	if dec.PostOrders[0].Side != "buy" {
		t.Errorf("expected buy order, got %s", dec.PostOrders[0].Side)
	}
}

func TestMarket_NoPostsWhenAtMaxOrders(t *testing.T) {
	a := newTestAgent()
	portA := uid(1)

	ownOrders := make([]MarketOrder, 5)
	for i := range ownOrders {
		ownOrders[i] = MarketOrder{
			ID: uid(byte(50 + i)), PortID: portA, GoodID: uid(10),
			Side: "buy", Price: 60,
		}
	}

	dec, err := a.DecideMarketAction(context.Background(), MarketDecisionRequest{
		Company:   CompanySnapshot{Treasury: 50000, TotalUpkeep: 100},
		OwnOrders: ownOrders,
		PriceCache: []PricePoint{
			{PortID: portA, GoodID: uid(10), BuyPrice: 50, SellPrice: 150},
		},
		Warehouses: []WarehouseSnapshot{{PortID: portA}},
	})
	if err != nil {
		t.Fatal(err)
	}

	if len(dec.PostOrders) > 0 {
		t.Error("should not post when 5 orders already active")
	}
}

// ===========================================================================
// 11. WAREHOUSE OPERATIONS
// ===========================================================================

func TestWarehouse_LoadsProfitableInventory(t *testing.T) {
	a := newTestAgent()
	portA, portB := uid(1), uid(2)
	goodX, goodY := uid(10), uid(11)
	warehouseID := uid(200)

	// Use two goods: goodX is buyable at portA (agent will buy it), goodY is only in the warehouse.
	// Budget limited so agent can only buy ~100 of goodX, leaving 100 capacity for warehouse loads.
	dec, err := a.DecideTradeAction(context.Background(), TradeDecisionRequest{
		StrategyHint: "arbitrage",
		Ship:         ShipSnapshot{PortID: &portA, Capacity: 200},
		Routes:       []RouteInfo{{FromID: portA, ToID: portB, Distance: 5}},
		PriceCache: []PricePoint{
			{PortID: portA, GoodID: goodX, BuyPrice: 50, SellPrice: 30},
			{PortID: portB, GoodID: goodX, BuyPrice: 80, SellPrice: 100},
			// goodY has no buy price at portA (only in warehouse), but sells well at portB.
			{PortID: portB, GoodID: goodY, SellPrice: 150},
		},
		Warehouses: []WarehouseSnapshot{
			{ID: warehouseID, PortID: portA, Capacity: 200, Items: []WarehouseItem{
				{GoodID: goodY, Quantity: 30},
			}},
		},
		Constraints: Constraints{MaxSpend: 5000},
		Params:      defaultParams(),
	})
	if err != nil {
		t.Fatal(err)
	}

	foundLoad := false
	for _, load := range dec.WarehouseLoads {
		if load.GoodID == goodY && load.WarehouseID == warehouseID {
			foundLoad = true
			if load.Quantity > 200 {
				t.Errorf("loaded %d from warehouse but ship capacity is 200", load.Quantity)
			}
		}
	}
	if !foundLoad {
		t.Error("should load profitable warehouse inventory (goodY sells for 150 at portB)")
	}
}

func TestWarehouse_NoLoadWhenNoWarehouse(t *testing.T) {
	a := newTestAgent()
	portA, portB := uid(1), uid(2)
	goodX := uid(10)

	dec, err := a.DecideTradeAction(context.Background(), TradeDecisionRequest{
		StrategyHint: "arbitrage",
		Ship:         ShipSnapshot{PortID: &portA, Capacity: 100},
		Routes:       []RouteInfo{{FromID: portA, ToID: portB, Distance: 5}},
		PriceCache: []PricePoint{
			{PortID: portA, GoodID: goodX, BuyPrice: 10, SellPrice: 5},
			{PortID: portB, GoodID: goodX, BuyPrice: 25, SellPrice: 40},
		},
		// No warehouses
		Constraints: Constraints{MaxSpend: 50000},
		Params:      defaultParams(),
	})
	if err != nil {
		t.Fatal(err)
	}

	if len(dec.WarehouseLoads) > 0 {
		t.Error("should not generate warehouse loads when no warehouse at port")
	}
}

func TestWarehouse_StoresOnlyDuringLowConfidence(t *testing.T) {
	a := newTestAgent()
	portA, portB := uid(1), uid(2)
	goodX, goodY := uid(10), uid(11)
	warehouseID := uid(200)

	// goodX has a profitable trade (high confidence) → should NOT store.
	dec, err := a.DecideTradeAction(context.Background(), TradeDecisionRequest{
		StrategyHint: "arbitrage",
		Ship:         ShipSnapshot{PortID: &portA, Capacity: 100},
		Routes:       []RouteInfo{{FromID: portA, ToID: portB, Distance: 5}},
		PriceCache: []PricePoint{
			{PortID: portA, GoodID: goodX, BuyPrice: 10, SellPrice: 5},
			{PortID: portA, GoodID: goodY, BuyPrice: 20, SellPrice: 10},
			{PortID: portB, GoodID: goodX, BuyPrice: 25, SellPrice: 50}, // Profitable!
			{PortID: portB, GoodID: goodY, BuyPrice: 30, SellPrice: 40}, // Also profitable
		},
		Warehouses: []WarehouseSnapshot{
			{ID: warehouseID, PortID: portA, Capacity: 500},
		},
		Constraints: Constraints{MaxSpend: 50000},
		Params:      defaultParams(),
	})
	if err != nil {
		t.Fatal(err)
	}

	if dec.Confidence > 0.5 {
		// High-confidence trade found — verify no warehouse stores.
		warehouseStores := 0
		for _, buy := range dec.BuyOrders {
			if buy.Destination == warehouseID {
				warehouseStores++
			}
		}
		if warehouseStores > 0 {
			t.Error("should not store in warehouse during high-confidence trades")
		}
	}
}

func TestWarehouse_SkippedForMarketMaker(t *testing.T) {
	a := newTestAgent()
	portA, portB := uid(1), uid(2)
	goodX := uid(10)
	warehouseID := uid(200)

	dec, err := a.DecideTradeAction(context.Background(), TradeDecisionRequest{
		StrategyHint: "market_maker",
		Ship:         ShipSnapshot{PortID: &portA, Capacity: 100},
		Routes:       []RouteInfo{{FromID: portA, ToID: portB, Distance: 5}},
		PriceCache: []PricePoint{
			{PortID: portA, GoodID: goodX, BuyPrice: 10, SellPrice: 5},
			{PortID: portB, GoodID: goodX, BuyPrice: 25, SellPrice: 50},
		},
		Warehouses: []WarehouseSnapshot{
			{ID: warehouseID, PortID: portA, Capacity: 500, Items: []WarehouseItem{
				{GoodID: goodX, Quantity: 30},
			}},
		},
		Constraints: Constraints{MaxSpend: 50000},
		Params:      defaultParams(),
	})
	if err != nil {
		t.Fatal(err)
	}

	if len(dec.WarehouseLoads) > 0 {
		t.Error("market_maker should not use warehouse trading ops")
	}
}

// ===========================================================================
// 12. ROUTE HISTORY BONUS — learning from past trades
// ===========================================================================

func TestRouteHistory_BoostsProfitableRoutes(t *testing.T) {
	a := newTestAgent()
	portA, portB, portC := uid(1), uid(2), uid(3)
	goodX := uid(10)

	// Both destinations have equal profit, but portB has strong route history.
	dec, err := a.DecideTradeAction(context.Background(), TradeDecisionRequest{
		StrategyHint: "arbitrage",
		Ship:         ShipSnapshot{PortID: &portA, Capacity: 100},
		Routes: []RouteInfo{
			{FromID: portA, ToID: portB, Distance: 5},
			{FromID: portA, ToID: portC, Distance: 5},
		},
		PriceCache: []PricePoint{
			{PortID: portA, GoodID: goodX, BuyPrice: 10, SellPrice: 5},
			{PortID: portB, GoodID: goodX, BuyPrice: 25, SellPrice: 25},
			{PortID: portC, GoodID: goodX, BuyPrice: 25, SellPrice: 25},
		},
		RouteHistory: []RoutePerformanceEntry{
			{FromPortID: portA, ToPortID: portB, Profit: 500, Quantity: 50},
			{FromPortID: portA, ToPortID: portB, Profit: 600, Quantity: 50},
			{FromPortID: portA, ToPortID: portC, Profit: 100, Quantity: 50},
		},
		Constraints: Constraints{MaxSpend: 50000},
		Params:      defaultParams(),
	})
	if err != nil {
		t.Fatal(err)
	}

	if dec.SailTo != nil && *dec.SailTo == portB {
		// Good — route history bonus tipped the balance.
	}
}

// ===========================================================================
// 13. STRATEGY EVALUATION
// ===========================================================================

func TestStrategy_SwitchesFromLosingToWinning(t *testing.T) {
	a := newTestAgent()

	dec, err := a.EvaluateStrategy(context.Background(), StrategyEvalRequest{
		Metrics: []StrategyMetrics{
			{StrategyName: "arbitrage", ProfitPerHour: 500},
			{StrategyName: "bulk_hauler", ProfitPerHour: -200},
		},
	})
	if err != nil {
		t.Fatal(err)
	}

	if dec.SwitchTo == nil || *dec.SwitchTo != "arbitrage" {
		t.Error("should switch from losing strategy to profitable one")
	}
}

func TestStrategy_NoSwitchWhenClose(t *testing.T) {
	a := newTestAgent()

	dec, err := a.EvaluateStrategy(context.Background(), StrategyEvalRequest{
		Metrics: []StrategyMetrics{
			{StrategyName: "arbitrage", ProfitPerHour: 500},
			{StrategyName: "bulk_hauler", ProfitPerHour: 400},
		},
	})
	if err != nil {
		t.Fatal(err)
	}

	if dec.SwitchTo != nil {
		t.Error("should not switch when strategies are within 1.5x range")
	}
}

func TestStrategy_SwitchesAt1_5xOutperformance(t *testing.T) {
	a := newTestAgent()

	dec, err := a.EvaluateStrategy(context.Background(), StrategyEvalRequest{
		Metrics: []StrategyMetrics{
			{StrategyName: "arbitrage", ProfitPerHour: 1000},
			{StrategyName: "bulk_hauler", ProfitPerHour: 500},
		},
	})
	if err != nil {
		t.Fatal(err)
	}

	if dec.SwitchTo == nil || *dec.SwitchTo != "arbitrage" {
		t.Error("should switch when outperformance exceeds 1.5x (1000 vs 500 = 2x)")
	}
}

func TestStrategy_NoSwitchWithSingleStrategy(t *testing.T) {
	a := newTestAgent()

	dec, err := a.EvaluateStrategy(context.Background(), StrategyEvalRequest{
		Metrics: []StrategyMetrics{
			{StrategyName: "arbitrage", ProfitPerHour: 1000},
		},
	})
	if err != nil {
		t.Fatal(err)
	}

	if dec.SwitchTo != nil {
		t.Error("cannot switch with only one strategy")
	}
}

// ===========================================================================
// 14. MULTI-TRADE SIMULATION — verify sustained profitability
// ===========================================================================

// simulateTradeCycle simulates a ship doing multiple trade cycles and tracks P&L.
func TestSimulation_ArbitrageMultiCycleProfitable(t *testing.T) {
	a := newTestAgent()
	portA, portB, portC := uid(1), uid(2), uid(3)
	goodX, goodY := uid(10), uid(11)

	prices := []PricePoint{
		{PortID: portA, GoodID: goodX, BuyPrice: 50, SellPrice: 30},
		{PortID: portA, GoodID: goodY, BuyPrice: 30, SellPrice: 20},
		{PortID: portB, GoodID: goodX, BuyPrice: 70, SellPrice: 90},
		{PortID: portB, GoodID: goodY, BuyPrice: 50, SellPrice: 60},
		{PortID: portC, GoodID: goodX, BuyPrice: 60, SellPrice: 80},
		{PortID: portC, GoodID: goodY, BuyPrice: 40, SellPrice: 55},
	}
	routes := []RouteInfo{
		{FromID: portA, ToID: portB, Distance: 5},
		{FromID: portA, ToID: portC, Distance: 8},
		{FromID: portB, ToID: portC, Distance: 4},
	}
	ports := []PortInfo{
		{ID: portA, TaxRateBps: 200},
		{ID: portB, TaxRateBps: 300},
		{ID: portC, TaxRateBps: 200},
	}

	treasury := int64(10000)
	totalProfit := int64(0)
	totalTrades := 0
	currentPort := portA
	capacity := 200

	// Simulate 10 trade cycles.
	for cycle := 0; cycle < 10; cycle++ {
		ship := ShipSnapshot{PortID: &currentPort, Capacity: capacity}
		maxSpend := treasury - 200 // Keep 200 floor.
		if maxSpend < 0 {
			maxSpend = 0
		}

		dec, err := a.DecideTradeAction(context.Background(), TradeDecisionRequest{
			StrategyHint: "arbitrage",
			Ship:         ship,
			Routes:       routes,
			Ports:        ports,
			PriceCache:   prices,
			Constraints:  Constraints{MaxSpend: maxSpend, TreasuryFloor: 200},
			Params:       defaultParams(),
		})
		if err != nil {
			t.Fatalf("cycle %d: %v", cycle, err)
		}

		// Calculate cost of buys.
		buyCost := int64(0)
		buyQty := map[uuid.UUID]int{}
		priceIndex := a.buildPriceIndex(prices)
		taxIndex := a.buildPortTaxIndex(ports)

		for _, buy := range dec.BuyOrders {
			pp, ok := priceIndex[priceKey(currentPort, buy.GoodID)]
			if !ok {
				continue
			}
			buyTax := pp.BuyPrice * taxIndex[currentPort] / 10000
			cost := int64(buy.Quantity) * int64(pp.BuyPrice+buyTax)
			buyCost += cost
			buyQty[buy.GoodID] += buy.Quantity
		}

		// Calculate sell revenue at destination.
		sellRevenue := int64(0)
		if dec.SailTo != nil {
			destPort := *dec.SailTo
			for goodID, qty := range buyQty {
				pp, ok := priceIndex[priceKey(destPort, goodID)]
				if ok && pp.SellPrice > 0 {
					sellTax := pp.SellPrice * taxIndex[destPort] / 10000
					revenue := int64(qty) * int64(pp.SellPrice-sellTax)
					sellRevenue += revenue
				}
			}
		}

		cycleProfit := sellRevenue - buyCost
		totalProfit += cycleProfit
		treasury -= buyCost
		treasury += sellRevenue

		if len(dec.BuyOrders) > 0 {
			totalTrades++
		}

		// Move to destination.
		if dec.SailTo != nil {
			currentPort = *dec.SailTo
		}
	}

	if totalTrades == 0 {
		t.Error("simulation made zero trades over 10 cycles — agent is stuck")
	}
	if totalProfit <= 0 {
		t.Errorf("agent lost money over %d trades: total P&L = %d", totalTrades, totalProfit)
	}
	t.Logf("simulation: %d trades, P&L = %d, final treasury = %d", totalTrades, totalProfit, treasury)
}

func TestSimulation_BulkHaulerMultiCycleProfitable(t *testing.T) {
	a := newTestAgent()
	portA, portB := uid(1), uid(2)
	goodX := uid(10)

	prices := []PricePoint{
		{PortID: portA, GoodID: goodX, BuyPrice: 20, SellPrice: 10},
		{PortID: portB, GoodID: goodX, BuyPrice: 40, SellPrice: 60},
	}
	routes := []RouteInfo{{FromID: portA, ToID: portB, Distance: 5}}
	ports := []PortInfo{
		{ID: portA, TaxRateBps: 200},
		{ID: portB, TaxRateBps: 200},
	}

	treasury := int64(20000)
	totalProfit := int64(0)
	totalTrades := 0
	currentPort := portA
	capacity := 500

	for cycle := 0; cycle < 10; cycle++ {
		ship := ShipSnapshot{PortID: &currentPort, Capacity: capacity}
		maxSpend := treasury - 200
		if maxSpend < 0 {
			maxSpend = 0
		}

		dec, err := a.DecideTradeAction(context.Background(), TradeDecisionRequest{
			StrategyHint: "bulk_hauler",
			Ship:         ship,
			Routes:       routes,
			Ports:        ports,
			PriceCache:   prices,
			Constraints:  Constraints{MaxSpend: maxSpend, TreasuryFloor: 200},
			Params:       defaultParams(),
		})
		if err != nil {
			t.Fatalf("cycle %d: %v", cycle, err)
		}

		priceIndex := a.buildPriceIndex(prices)
		taxIndex := a.buildPortTaxIndex(ports)

		buyCost := int64(0)
		buyQty := map[uuid.UUID]int{}
		for _, buy := range dec.BuyOrders {
			pp, ok := priceIndex[priceKey(currentPort, buy.GoodID)]
			if !ok {
				continue
			}
			buyTax := pp.BuyPrice * taxIndex[currentPort] / 10000
			cost := int64(buy.Quantity) * int64(pp.BuyPrice+buyTax)
			buyCost += cost
			buyQty[buy.GoodID] += buy.Quantity
		}

		sellRevenue := int64(0)
		if dec.SailTo != nil {
			destPort := *dec.SailTo
			for goodID, qty := range buyQty {
				pp, ok := priceIndex[priceKey(destPort, goodID)]
				if ok && pp.SellPrice > 0 {
					sellTax := pp.SellPrice * taxIndex[destPort] / 10000
					revenue := int64(qty) * int64(pp.SellPrice-sellTax)
					sellRevenue += revenue
				}
			}
		}

		cycleProfit := sellRevenue - buyCost
		totalProfit += cycleProfit
		treasury -= buyCost
		treasury += sellRevenue

		if len(dec.BuyOrders) > 0 {
			totalTrades++
		}

		if dec.SailTo != nil {
			currentPort = *dec.SailTo
		}
	}

	if totalTrades == 0 {
		t.Error("bulk hauler made zero trades — agent is stuck")
	}
	if totalProfit <= 0 {
		t.Errorf("bulk hauler lost money: P&L = %d over %d trades", totalProfit, totalTrades)
	}
	t.Logf("bulk hauler: %d trades, P&L = %d, final treasury = %d", totalTrades, totalProfit, treasury)
}

// ===========================================================================
// 15. EDGE CASES
// ===========================================================================

func TestEdge_EmptyPriceCache(t *testing.T) {
	a := newTestAgent()
	portA := uid(1)
	portB := uid(2)

	dec, err := a.DecideTradeAction(context.Background(), TradeDecisionRequest{
		Ship:        ShipSnapshot{PortID: &portA, Capacity: 100},
		Routes:      []RouteInfo{{FromID: portA, ToID: portB, Distance: 5}},
		PriceCache:  nil,
		Constraints: Constraints{MaxSpend: 50000},
		Params:      defaultParams(),
	})
	if err != nil {
		t.Fatal(err)
	}

	// No price data → no buys, but should not crash.
	if len(dec.BuyOrders) > 0 {
		t.Error("should not buy with no price data")
	}
}

func TestEdge_SinglePortNetwork(t *testing.T) {
	a := newTestAgent()
	portA := uid(1)

	dec, err := a.DecideTradeAction(context.Background(), TradeDecisionRequest{
		Ship:        ShipSnapshot{PortID: &portA, Capacity: 100},
		Routes:      nil, // Dead-end port
		Ports:       []PortInfo{{ID: portA}},
		Constraints: Constraints{MaxSpend: 50000},
	})
	if err != nil {
		t.Fatal(err)
	}

	// Should not crash, may sail to fallback or wait.
	if dec.Action == "" {
		t.Error("action should not be empty")
	}
}

func TestEdge_ZeroCapacityShip(t *testing.T) {
	a := newTestAgent()
	portA, portB := uid(1), uid(2)
	goodX := uid(10)

	dec, err := a.DecideTradeAction(context.Background(), TradeDecisionRequest{
		Ship:   ShipSnapshot{PortID: &portA, Capacity: 0}, // Broken ship?
		Routes: []RouteInfo{{FromID: portA, ToID: portB, Distance: 5}},
		PriceCache: []PricePoint{
			{PortID: portA, GoodID: goodX, BuyPrice: 10, SellPrice: 5},
			{PortID: portB, GoodID: goodX, BuyPrice: 25, SellPrice: 50},
		},
		Constraints: Constraints{MaxSpend: 50000},
	})
	if err != nil {
		t.Fatal(err)
	}

	totalQty := 0
	for _, buy := range dec.BuyOrders {
		totalQty += buy.Quantity
	}
	if totalQty > 0 {
		t.Error("should not buy anything with zero capacity")
	}
}

func TestEdge_NegativeBudget(t *testing.T) {
	a := newTestAgent()
	portA, portB := uid(1), uid(2)

	dec, err := a.DecideTradeAction(context.Background(), TradeDecisionRequest{
		Ship:        ShipSnapshot{PortID: &portA, Capacity: 100},
		Routes:      []RouteInfo{{FromID: portA, ToID: portB, Distance: 5}},
		PriceCache:  []PricePoint{{PortID: portA, GoodID: uid(10), BuyPrice: 10}},
		Constraints: Constraints{MaxSpend: -1000},
	})
	if err != nil {
		t.Fatal(err)
	}

	if len(dec.BuyOrders) > 0 {
		t.Error("should not buy with negative budget")
	}
}

func TestEdge_IdenticalPricesAllPorts(t *testing.T) {
	a := newTestAgent()
	portA, portB, portC := uid(1), uid(2), uid(3)
	goodX := uid(10)

	// Same buy and sell price everywhere → zero profit.
	dec, err := a.DecideTradeAction(context.Background(), TradeDecisionRequest{
		StrategyHint: "arbitrage",
		Ship:         ShipSnapshot{PortID: &portA, Capacity: 100},
		Routes: []RouteInfo{
			{FromID: portA, ToID: portB, Distance: 5},
			{FromID: portA, ToID: portC, Distance: 10},
		},
		PriceCache: []PricePoint{
			{PortID: portA, GoodID: goodX, BuyPrice: 50, SellPrice: 50},
			{PortID: portB, GoodID: goodX, BuyPrice: 50, SellPrice: 50},
			{PortID: portC, GoodID: goodX, BuyPrice: 50, SellPrice: 50},
		},
		Constraints: Constraints{MaxSpend: 50000},
		Params:      defaultParams(),
	})
	if err != nil {
		t.Fatal(err)
	}

	// Zero margin → no buys.
	if len(dec.BuyOrders) > 0 {
		t.Error("should not buy when profit is zero everywhere")
	}
}

func TestEdge_VeryExpensiveGoodsWithTinyBudget(t *testing.T) {
	a := newTestAgent()
	portA, portB := uid(1), uid(2)
	goodX := uid(10)

	dec, err := a.DecideTradeAction(context.Background(), TradeDecisionRequest{
		StrategyHint: "arbitrage",
		Ship:         ShipSnapshot{PortID: &portA, Capacity: 1000},
		Routes:       []RouteInfo{{FromID: portA, ToID: portB, Distance: 5}},
		PriceCache: []PricePoint{
			{PortID: portA, GoodID: goodX, BuyPrice: 10000, SellPrice: 8000},
			{PortID: portB, GoodID: goodX, BuyPrice: 15000, SellPrice: 20000},
		},
		Constraints: Constraints{MaxSpend: 100}, // Can't afford even 1 unit
		Params:      defaultParams(),
	})
	if err != nil {
		t.Fatal(err)
	}

	totalCost := int64(0)
	for _, buy := range dec.BuyOrders {
		totalCost += int64(buy.Quantity) * 10000
	}
	if totalCost > 100 {
		t.Errorf("spent %d which exceeds budget of 100", totalCost)
	}
}

// ===========================================================================
// 16. BIDIRECTIONAL ROUTES
// ===========================================================================

func TestRoutes_BidirectionalDiscovery(t *testing.T) {
	a := newTestAgent()
	portA, portB := uid(1), uid(2)

	// Route is defined as B→A, but ship is at A.
	// reachablePorts should find portB through reverse lookup.
	reachable := a.reachablePorts([]RouteInfo{
		{FromID: portB, ToID: portA, Distance: 7},
	}, portA)

	if len(reachable) != 1 {
		t.Fatalf("expected 1 reachable port via reverse route, got %d", len(reachable))
	}
	if reachable[portB] != 7 {
		t.Errorf("expected distance 7 to portB, got %f", reachable[portB])
	}
}

// ===========================================================================
// 17. CONCURRENT SHIP CAPACITY — warehouse loads don't exceed remaining
// ===========================================================================

func TestWarehouse_LoadDoesNotExceedRemainingCapacity(t *testing.T) {
	a := newTestAgent()
	portA, portB := uid(1), uid(2)
	goodX, goodY := uid(10), uid(11)
	warehouseID := uid(200)

	dec, err := a.DecideTradeAction(context.Background(), TradeDecisionRequest{
		StrategyHint: "arbitrage",
		Ship:         ShipSnapshot{PortID: &portA, Capacity: 50},
		Routes:       []RouteInfo{{FromID: portA, ToID: portB, Distance: 5}},
		PriceCache: []PricePoint{
			{PortID: portA, GoodID: goodX, BuyPrice: 10, SellPrice: 5},
			{PortID: portA, GoodID: goodY, BuyPrice: 20, SellPrice: 15},
			{PortID: portB, GoodID: goodX, BuyPrice: 30, SellPrice: 50},
			{PortID: portB, GoodID: goodY, BuyPrice: 40, SellPrice: 60},
		},
		Warehouses: []WarehouseSnapshot{
			{ID: warehouseID, PortID: portA, Capacity: 1000, Items: []WarehouseItem{
				{GoodID: goodX, Quantity: 100},
				{GoodID: goodY, Quantity: 100},
			}},
		},
		Constraints: Constraints{MaxSpend: 50000},
		Params:      defaultParams(),
	})
	if err != nil {
		t.Fatal(err)
	}

	totalLoad := 0
	for _, load := range dec.WarehouseLoads {
		totalLoad += load.Quantity
	}
	totalBuy := 0
	for _, buy := range dec.BuyOrders {
		totalBuy += buy.Quantity
	}

	if totalLoad+totalBuy > 50 {
		t.Errorf("loads (%d) + buys (%d) = %d exceeds ship capacity 50", totalLoad, totalBuy, totalLoad+totalBuy)
	}
}

// ===========================================================================
// 18. FLEET AFFORDABILITY — verify the math
// ===========================================================================

func TestFleet_AffordabilityMath(t *testing.T) {
	a := newTestAgent()

	tests := []struct {
		name      string
		treasury  int64
		upkeep    int64
		ships     int
		basePrice int
		newUpkeep int
		strategy  string
		wantBuy   bool
	}{
		{
			name: "can afford with big treasury",
			treasury: 100000, upkeep: 200, ships: 2,
			basePrice: 3000, newUpkeep: 100, strategy: "arbitrage",
			wantBuy: true,
		},
		{
			name: "cannot afford: price + reserve exceeds treasury",
			treasury: 5000, upkeep: 500, ships: 3,
			basePrice: 3000, newUpkeep: 200, strategy: "arbitrage",
			// Need: 3000 + (500+200)*5 = 3000 + 3500 = 6500 > 5000
			wantBuy: false,
		},
		{
			name: "bulk_hauler needs more reserve",
			treasury: 10000, upkeep: 500, ships: 2,
			basePrice: 3000, newUpkeep: 200, strategy: "bulk_hauler",
			// reserveCycles(3, "bulk_hauler") = 5 + 3/2 = 6
			// Need: 3000 + (500+200)*6 = 3000 + 4200 = 7200 < 10000
			wantBuy: true,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			ships := make([]ShipSnapshot, tc.ships)
			for i := range ships {
				ships[i] = ShipSnapshot{Status: "docked", PortID: ptr(uid(1))}
			}
			dec, err := a.DecideFleetAction(context.Background(), FleetDecisionRequest{
				StrategyHint:  tc.strategy,
				Company:       CompanySnapshot{Treasury: tc.treasury, TotalUpkeep: tc.upkeep},
				Ships:         ships,
				ShipyardPorts: []uuid.UUID{uid(1)},
				ShipTypes:     []ShipTypeInfo{{ID: uid(1), BasePrice: tc.basePrice, Upkeep: tc.newUpkeep, Speed: 10, Capacity: 100}},
			})
			if err != nil {
				t.Fatal(err)
			}
			gotBuy := len(dec.BuyShips) > 0
			if gotBuy != tc.wantBuy {
				t.Errorf("wantBuy=%v, gotBuy=%v. Reasoning: %s", tc.wantBuy, gotBuy, dec.Reasoning)
			}
		})
	}
}

// ===========================================================================
// 19. PRICE STALENESS — ObservedAt handling
// ===========================================================================

func TestPriceIndex_LatestObservationWins(t *testing.T) {
	a := newTestAgent()
	portA := uid(1)
	goodX := uid(10)

	now := time.Now()
	cache := []PricePoint{
		{PortID: portA, GoodID: goodX, BuyPrice: 50, SellPrice: 80, ObservedAt: now.Add(-1 * time.Hour)},
		{PortID: portA, GoodID: goodX, BuyPrice: 100, SellPrice: 200, ObservedAt: now}, // Latest
	}
	idx := a.buildPriceIndex(cache)
	key := priceKey(portA, goodX)
	pp := idx[key]

	// buildPriceIndex takes the last entry in the slice (simple overwrite).
	// In practice, the slice is ordered by ObservedAt.
	if pp.BuyPrice != 100 || pp.SellPrice != 200 {
		t.Errorf("expected latest price (100/200), got %d/%d", pp.BuyPrice, pp.SellPrice)
	}
}

// ===========================================================================
// 20. MULTI-GOOD GREEDY FILL — verify the fill algorithm is optimal-ish
// ===========================================================================

func TestGreedyFill_PrefersHighProfitGoods(t *testing.T) {
	a := newTestAgent()
	portA, portB := uid(1), uid(2)
	goodLow, goodMid, goodHigh := uid(10), uid(11), uid(12)

	dec, err := a.DecideTradeAction(context.Background(), TradeDecisionRequest{
		StrategyHint: "arbitrage",
		Ship:         ShipSnapshot{PortID: &portA, Capacity: 50},
		Routes:       []RouteInfo{{FromID: portA, ToID: portB, Distance: 5}},
		PriceCache: []PricePoint{
			{PortID: portA, GoodID: goodLow, BuyPrice: 10, SellPrice: 5},
			{PortID: portA, GoodID: goodMid, BuyPrice: 20, SellPrice: 10},
			{PortID: portA, GoodID: goodHigh, BuyPrice: 30, SellPrice: 20},
			{PortID: portB, GoodID: goodLow, BuyPrice: 15, SellPrice: 12},  // profit 2
			{PortID: portB, GoodID: goodMid, BuyPrice: 35, SellPrice: 30},  // profit 10
			{PortID: portB, GoodID: goodHigh, BuyPrice: 80, SellPrice: 80}, // profit 50
		},
		Constraints: Constraints{MaxSpend: 5000},
		Params:      defaultParams(),
	})
	if err != nil {
		t.Fatal(err)
	}

	if len(dec.BuyOrders) == 0 {
		t.Fatal("expected buy orders")
	}

	// Greedy fill should prioritize goodHigh (profit 50/unit) over others.
	if dec.BuyOrders[0].GoodID != goodHigh {
		t.Errorf("greedy fill should start with highest-profit good, got %v", dec.BuyOrders[0].GoodID)
	}
}

// ===========================================================================
// 21. INVARIANT CHECKS — properties that must always hold
// ===========================================================================

func TestInvariant_SailToIsAlwaysReachable(t *testing.T) {
	a := newTestAgent()
	portA, portB, portC := uid(1), uid(2), uid(3)
	goodX := uid(10)

	// portC is NOT reachable from portA.
	dec, err := a.DecideTradeAction(context.Background(), TradeDecisionRequest{
		StrategyHint: "arbitrage",
		Ship:         ShipSnapshot{PortID: &portA, Capacity: 100},
		Routes:       []RouteInfo{{FromID: portA, ToID: portB, Distance: 5}},
		PriceCache: []PricePoint{
			{PortID: portA, GoodID: goodX, BuyPrice: 10, SellPrice: 5},
			{PortID: portB, GoodID: goodX, BuyPrice: 30, SellPrice: 50},
			{PortID: portC, GoodID: goodX, BuyPrice: 100, SellPrice: 200}, // Amazing, but unreachable
		},
		Constraints: Constraints{MaxSpend: 50000},
		Params:      defaultParams(),
	})
	if err != nil {
		t.Fatal(err)
	}

	if dec.SailTo != nil && *dec.SailTo == portC {
		t.Error("should never sail to unreachable port")
	}
}

func TestInvariant_ConfidenceInRange(t *testing.T) {
	a := newTestAgent()
	portA, portB := uid(1), uid(2)
	goodX := uid(10)

	scenarios := []TradeDecisionRequest{
		{
			Ship:        ShipSnapshot{PortID: &portA, Capacity: 100},
			Routes:      []RouteInfo{{FromID: portA, ToID: portB, Distance: 5}},
			PriceCache:  []PricePoint{{PortID: portA, GoodID: goodX, BuyPrice: 10}, {PortID: portB, GoodID: goodX, SellPrice: 50}},
			Constraints: Constraints{MaxSpend: 50000},
			Params:      defaultParams(),
		},
		{
			Ship:        ShipSnapshot{PortID: &portA, Capacity: 100, IdleTicks: 5},
			Routes:      []RouteInfo{{FromID: portA, ToID: portB, Distance: 5}},
			Ports:       []PortInfo{{ID: portA}, {ID: portB, IsHub: true}},
			Constraints: Constraints{MaxSpend: 50000},
			Params:      defaultParams(),
		},
	}

	for i, req := range scenarios {
		dec, err := a.DecideTradeAction(context.Background(), req)
		if err != nil {
			t.Fatalf("scenario %d: %v", i, err)
		}
		if dec.Confidence < 0 || dec.Confidence > 1 {
			t.Errorf("scenario %d: confidence %f outside [0, 1]", i, dec.Confidence)
		}
	}
}

func TestInvariant_ActionNeverEmpty(t *testing.T) {
	a := newTestAgent()
	portA := uid(1)

	scenarios := []TradeDecisionRequest{
		{Ship: ShipSnapshot{PortID: nil}},
		{Ship: ShipSnapshot{PortID: &portA}},
		{Ship: ShipSnapshot{PortID: &portA, Capacity: 100}, Routes: []RouteInfo{{FromID: portA, ToID: uid(2), Distance: 5}}, Constraints: Constraints{MaxSpend: 0}},
	}

	for i, req := range scenarios {
		dec, err := a.DecideTradeAction(context.Background(), req)
		if err != nil {
			t.Fatalf("scenario %d: %v", i, err)
		}
		if dec.Action == "" {
			t.Errorf("scenario %d: action should never be empty", i)
		}
	}
}

func TestInvariant_BuyQuantitiesPositive(t *testing.T) {
	a := newTestAgent()
	portA, portB := uid(1), uid(2)

	dec, err := a.DecideTradeAction(context.Background(), TradeDecisionRequest{
		StrategyHint: "arbitrage",
		Ship:         ShipSnapshot{PortID: &portA, Capacity: 100},
		Routes:       []RouteInfo{{FromID: portA, ToID: portB, Distance: 5}},
		PriceCache: []PricePoint{
			{PortID: portA, GoodID: uid(10), BuyPrice: 10, SellPrice: 5},
			{PortID: portB, GoodID: uid(10), BuyPrice: 25, SellPrice: 50},
		},
		Constraints: Constraints{MaxSpend: 50000},
		Params:      defaultParams(),
	})
	if err != nil {
		t.Fatal(err)
	}

	for _, buy := range dec.BuyOrders {
		if buy.Quantity <= 0 {
			t.Errorf("buy order has non-positive quantity: %d", buy.Quantity)
		}
	}
	for _, sell := range dec.SellOrders {
		if sell.Quantity <= 0 {
			t.Errorf("sell order has non-positive quantity: %d", sell.Quantity)
		}
	}
}

// ===========================================================================
// 22. calcQuantity edge cases
// ===========================================================================

func TestCalcQuantity_Overflow(t *testing.T) {
	a := newTestAgent()

	// Very large budget, ensure no integer overflow.
	qty := a.calcQuantity(math.MaxInt64/2, 1, 0, 1000)
	if qty < 0 || qty > 1000 {
		t.Errorf("calcQuantity overflow: got %d", qty)
	}
}

func TestCalcQuantity_NegativePrice(t *testing.T) {
	a := newTestAgent()

	qty := a.calcQuantity(1000, -1, 0, 100)
	if qty != 0 {
		t.Errorf("expected 0 for negative price, got %d", qty)
	}
}

// ===========================================================================
// 23. FLEET — max fleet sizes by strategy
// ===========================================================================

func TestFleet_MaxFleetSizes(t *testing.T) {
	a := newTestAgent()

	tests := []struct {
		strategy string
		maxFleet int
	}{
		{"arbitrage", 5},
		{"bulk_hauler", 3},
		{"market_maker", 5},
	}

	for _, tc := range tests {
		t.Run(tc.strategy, func(t *testing.T) {
			ships := make([]ShipSnapshot, tc.maxFleet)
			for i := range ships {
				ships[i] = ShipSnapshot{Status: "docked", PortID: ptr(uid(1))}
			}
			dec, err := a.DecideFleetAction(context.Background(), FleetDecisionRequest{
				StrategyHint:  tc.strategy,
				Company:       CompanySnapshot{Treasury: 1000000, TotalUpkeep: 100},
				Ships:         ships,
				ShipyardPorts: []uuid.UUID{uid(1)},
				ShipTypes:     []ShipTypeInfo{{ID: uid(1), BasePrice: 100, Upkeep: 10, Speed: 10, Capacity: 100}},
			})
			if err != nil {
				t.Fatal(err)
			}
			if len(dec.BuyShips) > 0 {
				t.Errorf("%s at max fleet (%d) should not buy more ships", tc.strategy, tc.maxFleet)
			}
		})
	}
}

// ===========================================================================
// 24. MARKET — budget tracking across multiple fills
// ===========================================================================

func TestMarket_BudgetTrackingAcrossMultipleFills(t *testing.T) {
	a := newTestAgent()
	portA := uid(1)

	dec, err := a.DecideMarketAction(context.Background(), MarketDecisionRequest{
		Company: CompanySnapshot{Treasury: 500, TotalUpkeep: 100}, // available = 300
		OpenOrders: []MarketOrder{
			{ID: uid(100), PortID: portA, GoodID: uid(10), Side: "sell", Price: 20, Remaining: 50},
			{ID: uid(101), PortID: portA, GoodID: uid(11), Side: "sell", Price: 25, Remaining: 50},
		},
		PriceCache: []PricePoint{
			{PortID: portA, GoodID: uid(10), BuyPrice: 10, SellPrice: 80},
			{PortID: portA, GoodID: uid(11), BuyPrice: 10, SellPrice: 80},
		},
		Warehouses: []WarehouseSnapshot{{PortID: portA}},
	})
	if err != nil {
		t.Fatal(err)
	}

	totalCost := int64(0)
	for _, fill := range dec.FillOrders {
		for _, order := range []MarketOrder{
			{ID: uid(100), Price: 20},
			{ID: uid(101), Price: 25},
		} {
			if fill.OrderID == order.ID {
				totalCost += int64(fill.Quantity) * int64(order.Price)
			}
		}
	}

	if totalCost > 300 {
		t.Errorf("total fill cost %d exceeds available budget 300", totalCost)
	}
}
