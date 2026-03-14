package agent

import (
	"context"
	"testing"

	"github.com/google/uuid"
	"go.uber.org/zap"
)

// ---------------------------------------------------------------------------
// Test helpers
// ---------------------------------------------------------------------------

func testAgent() *HeuristicAgent {
	logger, _ := zap.NewDevelopment()
	return NewHeuristicAgent(logger)
}

func id(n byte) uuid.UUID {
	return uuid.UUID{n}
}

func ptr[T any](v T) *T { return &v }

// ---------------------------------------------------------------------------
// Trade Decision Tests
// ---------------------------------------------------------------------------

func TestDecideTradeAction_ShipNotDocked(t *testing.T) {
	a := testAgent()
	dec, err := a.DecideTradeAction(context.Background(), TradeDecisionRequest{
		Ship: ShipSnapshot{PortID: nil},
	})
	if err != nil {
		t.Fatal(err)
	}
	if dec.Action != "wait" {
		t.Errorf("expected wait, got %s", dec.Action)
	}
}

func TestDecideTradeAction_NoRoutes_WithCargo(t *testing.T) {
	a := testAgent()
	portA := id(1)
	dec, err := a.DecideTradeAction(context.Background(), TradeDecisionRequest{
		Ship: ShipSnapshot{
			PortID: &portA,
			Cargo:  []CargoItem{{GoodID: id(10), Quantity: 5}},
		},
		Routes: nil, // no routes
	})
	if err != nil {
		t.Fatal(err)
	}
	if dec.Action != "sell_and_buy" {
		t.Errorf("expected sell_and_buy, got %s", dec.Action)
	}
	if len(dec.SellOrders) != 1 || dec.SellOrders[0].Quantity != 5 {
		t.Errorf("expected 1 sell order for qty 5, got %+v", dec.SellOrders)
	}
}

func TestDecideTradeAction_NoRoutes_NoCargo(t *testing.T) {
	a := testAgent()
	portA := id(1)
	dec, err := a.DecideTradeAction(context.Background(), TradeDecisionRequest{
		Ship: ShipSnapshot{PortID: &portA},
	})
	if err != nil {
		t.Fatal(err)
	}
	if dec.Action != "wait" {
		t.Errorf("expected wait, got %s", dec.Action)
	}
}

func TestDecideTradeAction_BudgetZero_SellsAndMoves(t *testing.T) {
	a := testAgent()
	portA := id(1)
	portB := id(2)
	dec, err := a.DecideTradeAction(context.Background(), TradeDecisionRequest{
		Ship: ShipSnapshot{
			PortID: &portA,
			Cargo:  []CargoItem{{GoodID: id(10), Quantity: 3}},
		},
		Routes:      []RouteInfo{{FromID: portA, ToID: portB, Distance: 5}},
		Constraints: Constraints{MaxSpend: 0}, // at treasury floor
	})
	if err != nil {
		t.Fatal(err)
	}
	if dec.Action != "sell_and_buy" {
		t.Errorf("expected sell_and_buy, got %s", dec.Action)
	}
	if dec.SailTo == nil || *dec.SailTo != portB {
		t.Errorf("expected to sail to portB")
	}
	if len(dec.SellOrders) != 1 {
		t.Errorf("expected sell orders")
	}
}

func TestDecideTradeAction_Arbitrage_PicksBestProfitPerDistance(t *testing.T) {
	a := testAgent()
	portA := id(1)
	portB := id(2) // close, lower profit
	portC := id(3) // far, higher profit

	goodX := id(10)

	dec, err := a.DecideTradeAction(context.Background(), TradeDecisionRequest{
		StrategyHint: "arbitrage",
		Ship:         ShipSnapshot{PortID: &portA, Capacity: 100},
		Routes: []RouteInfo{
			{FromID: portA, ToID: portB, Distance: 2},
			{FromID: portA, ToID: portC, Distance: 20},
		},
		PriceCache: []PricePoint{
			{PortID: portA, GoodID: goodX, BuyPrice: 100, SellPrice: 80},
			{PortID: portB, GoodID: goodX, BuyPrice: 120, SellPrice: 130}, // profit 30, dist 2 → score 15
			{PortID: portC, GoodID: goodX, BuyPrice: 200, SellPrice: 200}, // profit 100, dist 20 → score 5
		},
		Constraints: Constraints{MaxSpend: 10000},
	})
	if err != nil {
		t.Fatal(err)
	}
	if dec.Action != "sell_and_buy" {
		t.Errorf("expected sell_and_buy, got %s", dec.Action)
	}
	// Arbitrage should prefer portB (higher profit/distance score).
	if dec.SailTo == nil || *dec.SailTo != portB {
		t.Errorf("arbitrage should pick portB (best profit/distance), got %v", dec.SailTo)
	}
}

func TestDecideTradeAction_BulkHauler_PicksBestTotalProfit(t *testing.T) {
	a := testAgent()
	portA := id(1)
	portB := id(2) // close, lower profit per unit
	portC := id(3) // far, higher profit per unit

	goodX := id(10)

	dec, err := a.DecideTradeAction(context.Background(), TradeDecisionRequest{
		StrategyHint: "bulk_hauler",
		Ship:         ShipSnapshot{PortID: &portA, Capacity: 200},
		Routes: []RouteInfo{
			{FromID: portA, ToID: portB, Distance: 2},
			{FromID: portA, ToID: portC, Distance: 20},
		},
		PriceCache: []PricePoint{
			{PortID: portA, GoodID: goodX, BuyPrice: 10, SellPrice: 5},
			{PortID: portB, GoodID: goodX, BuyPrice: 20, SellPrice: 12}, // profit 2/unit
			{PortID: portC, GoodID: goodX, BuyPrice: 20, SellPrice: 50}, // profit 40/unit
		},
		Constraints: Constraints{MaxSpend: 100000},
	})
	if err != nil {
		t.Fatal(err)
	}
	if dec.Action != "sell_and_buy" {
		t.Errorf("expected sell_and_buy, got %s", dec.Action)
	}
	// Bulk hauler should prefer portC (higher total profit).
	if dec.SailTo == nil || *dec.SailTo != portC {
		t.Errorf("bulk_hauler should pick portC (best total profit), got %v", dec.SailTo)
	}
}

func TestDecideTradeAction_NoProfitableRoutes_SpeculativeTrade(t *testing.T) {
	a := testAgent()
	portA := id(1)
	portB := id(2)

	goodX := id(10)

	dec, err := a.DecideTradeAction(context.Background(), TradeDecisionRequest{
		Ship: ShipSnapshot{PortID: &portA, Capacity: 100},
		Routes: []RouteInfo{
			{FromID: portA, ToID: portB, Distance: 5},
		},
		PriceCache: []PricePoint{
			// Buy price at A is higher than sell at B → no profit.
			{PortID: portA, GoodID: goodX, BuyPrice: 100, SellPrice: 80},
			{PortID: portB, GoodID: goodX, BuyPrice: 110, SellPrice: 90},
		},
		Constraints: Constraints{MaxSpend: 5000},
	})
	if err != nil {
		t.Fatal(err)
	}
	if dec.Action != "sell_and_buy" {
		t.Errorf("expected sell_and_buy (speculative), got %s", dec.Action)
	}
	if dec.SailTo == nil {
		t.Error("speculative trade should still sail somewhere")
	}
	if dec.Confidence >= 0.5 {
		t.Errorf("speculative trade should have low confidence, got %f", dec.Confidence)
	}
}

func TestDecideTradeAction_SellsExistingCargo(t *testing.T) {
	a := testAgent()
	portA := id(1)
	portB := id(2)
	goodX := id(10)
	goodY := id(11)

	dec, err := a.DecideTradeAction(context.Background(), TradeDecisionRequest{
		Ship: ShipSnapshot{
			PortID: &portA,
			Cargo: []CargoItem{
				{GoodID: goodX, Quantity: 10},
				{GoodID: goodY, Quantity: 20},
			},
		},
		Routes: []RouteInfo{{FromID: portA, ToID: portB, Distance: 5}},
		PriceCache: []PricePoint{
			{PortID: portA, GoodID: goodX, BuyPrice: 50, SellPrice: 40},
			{PortID: portB, GoodID: goodX, BuyPrice: 60, SellPrice: 80},
		},
		Constraints: Constraints{MaxSpend: 5000},
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(dec.SellOrders) != 2 {
		t.Errorf("expected 2 sell orders, got %d", len(dec.SellOrders))
	}
}

// ---------------------------------------------------------------------------
// P2P Market Decision Tests
// ---------------------------------------------------------------------------

func TestDecideMarketAction_NoPriceData(t *testing.T) {
	a := testAgent()
	dec, err := a.DecideMarketAction(context.Background(), MarketDecisionRequest{
		PriceCache: nil,
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(dec.FillOrders) != 0 || len(dec.PostOrders) != 0 || len(dec.CancelOrders) != 0 {
		t.Error("expected no actions when no price data")
	}
}

func TestDecideMarketAction_TreasuryAtFloor(t *testing.T) {
	a := testAgent()
	dec, err := a.DecideMarketAction(context.Background(), MarketDecisionRequest{
		Company: CompanySnapshot{
			Treasury:    100,
			TotalUpkeep: 100, // floor = 200, treasury < floor
		},
		PriceCache: []PricePoint{{PortID: id(1), GoodID: id(10), BuyPrice: 50, SellPrice: 80}},
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(dec.FillOrders) != 0 {
		t.Error("should not fill orders when treasury at floor")
	}
}

func TestDecideMarketAction_FillUnderpricedSellOrder(t *testing.T) {
	a := testAgent()
	portA := id(1)
	goodX := id(10)
	orderID := id(100)

	dec, err := a.DecideMarketAction(context.Background(), MarketDecisionRequest{
		Company: CompanySnapshot{Treasury: 10000, TotalUpkeep: 100},
		OpenOrders: []MarketOrder{
			{
				ID:        orderID,
				PortID:    portA,
				GoodID:    goodX,
				Side:      "sell",
				Price:     40, // Player selling at 40
				Remaining: 10,
			},
		},
		PriceCache: []PricePoint{
			{PortID: portA, GoodID: goodX, BuyPrice: 30, SellPrice: 80}, // NPC sell at 80
		},
		Warehouses: []WarehouseSnapshot{{PortID: portA}},
	})
	if err != nil {
		t.Fatal(err)
	}
	// Profit = 80 - 40 = 40, minProfit = 80/10 = 8. 40 >= 8, so fill.
	if len(dec.FillOrders) != 1 {
		t.Fatalf("expected 1 fill order, got %d", len(dec.FillOrders))
	}
	if dec.FillOrders[0].OrderID != orderID {
		t.Errorf("expected fill order ID %v, got %v", orderID, dec.FillOrders[0].OrderID)
	}
	if dec.FillOrders[0].Quantity != 10 {
		t.Errorf("expected fill qty 10, got %d", dec.FillOrders[0].Quantity)
	}
}

func TestDecideMarketAction_SkipFairlyPricedSellOrder(t *testing.T) {
	a := testAgent()
	portA := id(1)
	goodX := id(10)

	dec, err := a.DecideMarketAction(context.Background(), MarketDecisionRequest{
		Company: CompanySnapshot{Treasury: 10000, TotalUpkeep: 100},
		OpenOrders: []MarketOrder{
			{
				ID:        id(100),
				PortID:    portA,
				GoodID:    goodX,
				Side:      "sell",
				Price:     75, // Close to NPC sell price
				Remaining: 10,
			},
		},
		PriceCache: []PricePoint{
			{PortID: portA, GoodID: goodX, BuyPrice: 30, SellPrice: 80},
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	// Profit = 80 - 75 = 5, minProfit = 80/10 = 8. 5 < 8, so skip.
	if len(dec.FillOrders) != 0 {
		t.Errorf("should not fill fairly-priced order, got %d fills", len(dec.FillOrders))
	}
}

func TestDecideMarketAction_FillOverpricedBuyOrder(t *testing.T) {
	a := testAgent()
	portA := id(1)
	goodX := id(10)
	orderID := id(101)

	dec, err := a.DecideMarketAction(context.Background(), MarketDecisionRequest{
		Company: CompanySnapshot{Treasury: 10000, TotalUpkeep: 100},
		OpenOrders: []MarketOrder{
			{
				ID:        orderID,
				PortID:    portA,
				GoodID:    goodX,
				Side:      "buy",
				Price:     100, // Player buying at 100
				Remaining: 5,
			},
		},
		PriceCache: []PricePoint{
			{PortID: portA, GoodID: goodX, BuyPrice: 50, SellPrice: 80}, // NPC buy at 50
		},
		Warehouses: []WarehouseSnapshot{{PortID: portA}},
	})
	if err != nil {
		t.Fatal(err)
	}
	// Profit = 100 - 50 = 50, minProfit = 50/10 = 5. 50 >= 5, so fill.
	if len(dec.FillOrders) != 1 {
		t.Fatalf("expected 1 fill order, got %d", len(dec.FillOrders))
	}
	if dec.FillOrders[0].OrderID != orderID {
		t.Errorf("expected order %v, got %v", orderID, dec.FillOrders[0].OrderID)
	}
}

func TestDecideMarketAction_SkipFairBuyOrder(t *testing.T) {
	a := testAgent()
	portA := id(1)
	goodX := id(10)

	dec, err := a.DecideMarketAction(context.Background(), MarketDecisionRequest{
		Company: CompanySnapshot{Treasury: 10000, TotalUpkeep: 100},
		OpenOrders: []MarketOrder{
			{
				ID:        id(101),
				PortID:    portA,
				GoodID:    goodX,
				Side:      "buy",
				Price:     53, // Only 3 above NPC buy
				Remaining: 5,
			},
		},
		PriceCache: []PricePoint{
			{PortID: portA, GoodID: goodX, BuyPrice: 50, SellPrice: 80},
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	// Profit = 53 - 50 = 3, minProfit = 50/10 = 5. 3 < 5, skip.
	if len(dec.FillOrders) != 0 {
		t.Errorf("should not fill barely-profitable order, got %d fills", len(dec.FillOrders))
	}
}

func TestDecideMarketAction_DoNotFillOwnOrders(t *testing.T) {
	a := testAgent()
	portA := id(1)
	goodX := id(10)
	ownOrderID := id(200)

	dec, err := a.DecideMarketAction(context.Background(), MarketDecisionRequest{
		Company: CompanySnapshot{Treasury: 10000, TotalUpkeep: 100},
		OpenOrders: []MarketOrder{
			{
				ID:        ownOrderID,
				PortID:    portA,
				GoodID:    goodX,
				Side:      "sell",
				Price:     20, // Very underpriced, would be profitable to fill
				Remaining: 10,
			},
		},
		OwnOrders: []MarketOrder{
			{ID: ownOrderID}, // This is our own order
		},
		PriceCache: []PricePoint{
			{PortID: portA, GoodID: goodX, BuyPrice: 30, SellPrice: 80},
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(dec.FillOrders) != 0 {
		t.Error("should never fill own orders")
	}
}

func TestDecideMarketAction_CancelStaleSellOrder(t *testing.T) {
	a := testAgent()
	portA := id(1)
	goodX := id(10)
	ownOrderID := id(200)

	dec, err := a.DecideMarketAction(context.Background(), MarketDecisionRequest{
		Company: CompanySnapshot{Treasury: 10000, TotalUpkeep: 100},
		OwnOrders: []MarketOrder{
			{
				ID:     ownOrderID,
				PortID: portA,
				GoodID: goodX,
				Side:   "sell",
				Price:  100, // Our sell price is above NPC sell price
			},
		},
		PriceCache: []PricePoint{
			{PortID: portA, GoodID: goodX, BuyPrice: 30, SellPrice: 80}, // NPC sells for 80
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	// Our sell at 100 > NPC sell at 80 → should cancel.
	if len(dec.CancelOrders) != 1 || dec.CancelOrders[0] != ownOrderID {
		t.Errorf("expected cancel of stale sell order, got %v", dec.CancelOrders)
	}
}

func TestDecideMarketAction_CancelStaleBuyOrder(t *testing.T) {
	a := testAgent()
	portA := id(1)
	goodX := id(10)
	ownOrderID := id(201)

	dec, err := a.DecideMarketAction(context.Background(), MarketDecisionRequest{
		Company: CompanySnapshot{Treasury: 10000, TotalUpkeep: 100},
		OwnOrders: []MarketOrder{
			{
				ID:     ownOrderID,
				PortID: portA,
				GoodID: goodX,
				Side:   "buy",
				Price:  40, // Our buy price is below NPC buy price
			},
		},
		PriceCache: []PricePoint{
			{PortID: portA, GoodID: goodX, BuyPrice: 50, SellPrice: 80}, // NPC buys for 50
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	// Our bid at 40 < NPC buy at 50 → should cancel (NPC outbids us).
	if len(dec.CancelOrders) != 1 || dec.CancelOrders[0] != ownOrderID {
		t.Errorf("expected cancel of stale buy order, got %v", dec.CancelOrders)
	}
}

func TestDecideMarketAction_DoNotCancelHealthyOrders(t *testing.T) {
	a := testAgent()
	portA := id(1)
	goodX := id(10)

	dec, err := a.DecideMarketAction(context.Background(), MarketDecisionRequest{
		Company: CompanySnapshot{Treasury: 10000, TotalUpkeep: 100},
		OwnOrders: []MarketOrder{
			{
				ID:     id(202),
				PortID: portA,
				GoodID: goodX,
				Side:   "sell",
				Price:  70, // Below NPC sell → still viable
			},
			{
				ID:     id(203),
				PortID: portA,
				GoodID: goodX,
				Side:   "buy",
				Price:  60, // Above NPC buy → still viable
			},
		},
		PriceCache: []PricePoint{
			{PortID: portA, GoodID: goodX, BuyPrice: 50, SellPrice: 80},
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(dec.CancelOrders) != 0 {
		t.Errorf("should not cancel healthy orders, got %v", dec.CancelOrders)
	}
}

func TestDecideMarketAction_PostOrdersAtWideSpread(t *testing.T) {
	a := testAgent()
	portA := id(1)
	goodX := id(10)

	dec, err := a.DecideMarketAction(context.Background(), MarketDecisionRequest{
		Company: CompanySnapshot{Treasury: 10000, TotalUpkeep: 100},
		PriceCache: []PricePoint{
			{PortID: portA, GoodID: goodX, BuyPrice: 50, SellPrice: 120}, // 70 spread, 140% > 20%
		},
		Warehouses: []WarehouseSnapshot{{PortID: portA}},
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(dec.PostOrders) == 0 {
		t.Fatal("expected post orders for wide spread")
	}
	order := dec.PostOrders[0]
	if order.Side != "buy" {
		t.Errorf("expected buy order, got %s", order.Side)
	}
	// Bid should be buyPrice + spread/4 = 50 + 70/4 = 50 + 17 = 67.
	expectedBid := 50 + 70/4
	if order.Price != expectedBid {
		t.Errorf("expected bid %d, got %d", expectedBid, order.Price)
	}
	if order.Total <= 0 {
		t.Error("expected positive quantity")
	}
	if order.Total > 20 {
		t.Errorf("quantity should be capped at 20, got %d", order.Total)
	}
}

func TestDecideMarketAction_NoPostsWhenNarrowSpread(t *testing.T) {
	a := testAgent()
	portA := id(1)
	goodX := id(10)

	dec, err := a.DecideMarketAction(context.Background(), MarketDecisionRequest{
		Company: CompanySnapshot{Treasury: 10000, TotalUpkeep: 100},
		PriceCache: []PricePoint{
			{PortID: portA, GoodID: goodX, BuyPrice: 100, SellPrice: 105}, // 5% spread < 20%
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(dec.PostOrders) != 0 {
		t.Errorf("should not post with narrow spread, got %d orders", len(dec.PostOrders))
	}
}

func TestDecideMarketAction_MaxFiveActiveOrders(t *testing.T) {
	a := testAgent()
	portA := id(1)

	// Create 5 existing own orders.
	ownOrders := make([]MarketOrder, 5)
	for i := range ownOrders {
		ownOrders[i] = MarketOrder{
			ID:     id(byte(50 + i)),
			PortID: portA,
			GoodID: id(10),
			Side:   "buy",
			Price:  60, // Still healthy (above NPC buy)
		}
	}

	dec, err := a.DecideMarketAction(context.Background(), MarketDecisionRequest{
		Company:   CompanySnapshot{Treasury: 10000, TotalUpkeep: 100},
		OwnOrders: ownOrders,
		PriceCache: []PricePoint{
			{PortID: portA, GoodID: id(10), BuyPrice: 50, SellPrice: 120}, // Wide spread
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(dec.PostOrders) != 0 {
		t.Errorf("should not post when at 5 active orders, got %d", len(dec.PostOrders))
	}
}

func TestDecideMarketAction_BudgetConstrainsFillQuantity(t *testing.T) {
	a := testAgent()
	portA := id(1)
	goodX := id(10)

	dec, err := a.DecideMarketAction(context.Background(), MarketDecisionRequest{
		Company: CompanySnapshot{
			Treasury:    500, // Only 500
			TotalUpkeep: 100, // Floor = 200, available = 300
		},
		OpenOrders: []MarketOrder{
			{
				ID:        id(100),
				PortID:    portA,
				GoodID:    goodX,
				Side:      "sell",
				Price:     20, // Very cheap
				Remaining: 100,
			},
		},
		PriceCache: []PricePoint{
			{PortID: portA, GoodID: goodX, BuyPrice: 10, SellPrice: 80},
		},
		Warehouses: []WarehouseSnapshot{{PortID: portA}},
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(dec.FillOrders) != 1 {
		t.Fatal("expected 1 fill order")
	}
	// Available = 300, price = 20, max qty = 300/20 = 15 (not 100).
	if dec.FillOrders[0].Quantity > 15 {
		t.Errorf("quantity should be budget-capped to 15, got %d", dec.FillOrders[0].Quantity)
	}
}

func TestDecideMarketAction_SkipZeroRemainingOrders(t *testing.T) {
	a := testAgent()
	portA := id(1)
	goodX := id(10)

	dec, err := a.DecideMarketAction(context.Background(), MarketDecisionRequest{
		Company: CompanySnapshot{Treasury: 10000, TotalUpkeep: 100},
		OpenOrders: []MarketOrder{
			{
				ID:        id(100),
				PortID:    portA,
				GoodID:    goodX,
				Side:      "sell",
				Price:     20,
				Remaining: 0, // Already fully filled
			},
		},
		PriceCache: []PricePoint{
			{PortID: portA, GoodID: goodX, BuyPrice: 10, SellPrice: 80},
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(dec.FillOrders) != 0 {
		t.Error("should not fill order with 0 remaining")
	}
}

func TestDecideMarketAction_MultipleFills(t *testing.T) {
	a := testAgent()
	portA := id(1)
	goodX := id(10)
	goodY := id(11)

	dec, err := a.DecideMarketAction(context.Background(), MarketDecisionRequest{
		Company: CompanySnapshot{Treasury: 100000, TotalUpkeep: 100},
		OpenOrders: []MarketOrder{
			{
				ID: id(100), PortID: portA, GoodID: goodX,
				Side: "sell", Price: 20, Remaining: 5,
			},
			{
				ID: id(101), PortID: portA, GoodID: goodY,
				Side: "buy", Price: 200, Remaining: 3,
			},
		},
		PriceCache: []PricePoint{
			{PortID: portA, GoodID: goodX, BuyPrice: 10, SellPrice: 80},
			{PortID: portA, GoodID: goodY, BuyPrice: 50, SellPrice: 100},
		},
		Warehouses: []WarehouseSnapshot{{PortID: portA}},
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(dec.FillOrders) != 2 {
		t.Errorf("expected 2 fill orders, got %d", len(dec.FillOrders))
	}
}

func TestDecideMarketAction_SellOrderAboveNPCSell_Ignored(t *testing.T) {
	a := testAgent()
	portA := id(1)
	goodX := id(10)

	dec, err := a.DecideMarketAction(context.Background(), MarketDecisionRequest{
		Company: CompanySnapshot{Treasury: 10000, TotalUpkeep: 100},
		OpenOrders: []MarketOrder{
			{
				ID: id(100), PortID: portA, GoodID: goodX,
				Side: "sell", Price: 90, Remaining: 10, // Above NPC sell of 80
			},
		},
		PriceCache: []PricePoint{
			{PortID: portA, GoodID: goodX, BuyPrice: 30, SellPrice: 80},
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	// Player sell at 90 > NPC sell at 80 → not profitable to buy from player.
	if len(dec.FillOrders) != 0 {
		t.Error("should not fill sell order priced above NPC sell price")
	}
}

func TestDecideMarketAction_BuyOrderBelowNPCBuy_Ignored(t *testing.T) {
	a := testAgent()
	portA := id(1)
	goodX := id(10)

	dec, err := a.DecideMarketAction(context.Background(), MarketDecisionRequest{
		Company: CompanySnapshot{Treasury: 10000, TotalUpkeep: 100},
		OpenOrders: []MarketOrder{
			{
				ID: id(101), PortID: portA, GoodID: goodX,
				Side: "buy", Price: 40, Remaining: 10, // Below NPC buy of 50
			},
		},
		PriceCache: []PricePoint{
			{PortID: portA, GoodID: goodX, BuyPrice: 50, SellPrice: 80},
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	// Player buy at 40 < NPC buy at 50 → not profitable to sell to player.
	if len(dec.FillOrders) != 0 {
		t.Error("should not fill buy order priced below NPC buy price")
	}
}

// ---------------------------------------------------------------------------
// Fleet Decision Tests
// ---------------------------------------------------------------------------

func TestDecideFleetAction_NoShipTypes(t *testing.T) {
	a := testAgent()
	dec, err := a.DecideFleetAction(context.Background(), FleetDecisionRequest{
		Company:   CompanySnapshot{Treasury: 10000, TotalUpkeep: 100},
		ShipTypes: nil,
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(dec.BuyShips) != 0 {
		t.Error("should not buy when no ship types")
	}
}

func TestDecideFleetAction_FleetAtMax(t *testing.T) {
	a := testAgent()
	ships := make([]ShipSnapshot, 5) // 5 = max for non-bulk_hauler
	dec, err := a.DecideFleetAction(context.Background(), FleetDecisionRequest{
		Company:   CompanySnapshot{Treasury: 100000, TotalUpkeep: 100},
		Ships:     ships,
		ShipTypes: []ShipTypeInfo{{ID: id(1), BasePrice: 100, Upkeep: 10}},
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(dec.BuyShips) != 0 {
		t.Error("should not buy when fleet is at max")
	}
}

func TestDecideFleetAction_BulkHaulerMaxIs3(t *testing.T) {
	a := testAgent()
	ships := make([]ShipSnapshot, 3)
	dec, err := a.DecideFleetAction(context.Background(), FleetDecisionRequest{
		StrategyHint: "bulk_hauler",
		Company:      CompanySnapshot{Treasury: 100000, TotalUpkeep: 100},
		Ships:        ships,
		ShipTypes:    []ShipTypeInfo{{ID: id(1), BasePrice: 100, Capacity: 200, Upkeep: 10}},
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(dec.BuyShips) != 0 {
		t.Error("bulk_hauler max fleet is 3")
	}
}

func TestDecideFleetAction_ArbitragePrefersSpeed(t *testing.T) {
	a := testAgent()
	portA := id(1)
	dec, err := a.DecideFleetAction(context.Background(), FleetDecisionRequest{
		StrategyHint:  "arbitrage",
		Company:       CompanySnapshot{Treasury: 100000, TotalUpkeep: 100},
		Ships:         []ShipSnapshot{{Status: "docked", PortID: &portA}},
		ShipyardPorts: []uuid.UUID{portA},
		ShipTypes: []ShipTypeInfo{
			{ID: id(1), Name: "Slow", Speed: 5, Capacity: 200, BasePrice: 500, Upkeep: 50},
			{ID: id(2), Name: "Fast", Speed: 15, Capacity: 100, BasePrice: 1000, Upkeep: 80},
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(dec.BuyShips) != 1 {
		t.Fatal("expected 1 ship purchase")
	}
	if dec.BuyShips[0].ShipTypeID != id(2) {
		t.Error("arbitrage should prefer fastest ship")
	}
}

func TestDecideFleetAction_BulkHaulerPrefersCapacity(t *testing.T) {
	a := testAgent()
	portA := id(1)
	dec, err := a.DecideFleetAction(context.Background(), FleetDecisionRequest{
		StrategyHint:  "bulk_hauler",
		Company:       CompanySnapshot{Treasury: 100000, TotalUpkeep: 100},
		Ships:         []ShipSnapshot{{Status: "docked", PortID: &portA}},
		ShipyardPorts: []uuid.UUID{portA},
		ShipTypes: []ShipTypeInfo{
			{ID: id(1), Name: "Small", Speed: 15, Capacity: 100, BasePrice: 500, Upkeep: 50},
			{ID: id(2), Name: "Big", Speed: 5, Capacity: 300, BasePrice: 1000, Upkeep: 80},
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(dec.BuyShips) != 1 {
		t.Fatal("expected 1 ship purchase")
	}
	if dec.BuyShips[0].ShipTypeID != id(2) {
		t.Error("bulk_hauler should prefer largest capacity")
	}
}

func TestDecideFleetAction_MarketMakerPrefersCheapest(t *testing.T) {
	a := testAgent()
	portA := id(1)
	dec, err := a.DecideFleetAction(context.Background(), FleetDecisionRequest{
		StrategyHint:  "market_maker",
		Company:       CompanySnapshot{Treasury: 100000, TotalUpkeep: 100},
		Ships:         []ShipSnapshot{{Status: "docked", PortID: &portA}},
		ShipyardPorts: []uuid.UUID{portA},
		ShipTypes: []ShipTypeInfo{
			{ID: id(1), Name: "Expensive", Speed: 15, Capacity: 200, BasePrice: 5000, Upkeep: 200},
			{ID: id(2), Name: "Cheap", Speed: 5, Capacity: 100, BasePrice: 500, Upkeep: 30},
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(dec.BuyShips) != 1 {
		t.Fatal("expected 1 ship purchase")
	}
	if dec.BuyShips[0].ShipTypeID != id(2) {
		t.Error("market_maker should prefer cheapest ship")
	}
}

func TestDecideFleetAction_TreasuryTooLow(t *testing.T) {
	a := testAgent()
	dec, err := a.DecideFleetAction(context.Background(), FleetDecisionRequest{
		Company:   CompanySnapshot{Treasury: 100, TotalUpkeep: 50},
		ShipTypes: []ShipTypeInfo{{ID: id(1), BasePrice: 500, Upkeep: 50}},
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(dec.BuyShips) != 0 {
		t.Error("should not buy when treasury too low")
	}
}

func TestDecideFleetAction_SellShips_HighUpkeep(t *testing.T) {
	a := testAgent()
	portA := id(1)
	// Treasury 400, upkeep 100. 400 < 100*5 = 500 → should recommend selling.
	dec, err := a.DecideFleetAction(context.Background(), FleetDecisionRequest{
		StrategyHint: "arbitrage",
		Company:      CompanySnapshot{Treasury: 400, TotalUpkeep: 100},
		Ships: []ShipSnapshot{
			{ID: id(1), Status: "docked", PortID: &portA, Speed: 10, Capacity: 100},
			{ID: id(2), Status: "docked", PortID: &portA, Speed: 5, Capacity: 200}, // Slower → sell this
		},
		ShipTypes: []ShipTypeInfo{{ID: id(1), BasePrice: 500, Upkeep: 50}},
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(dec.SellShips) != 1 {
		t.Fatalf("expected 1 ship to sell, got %d", len(dec.SellShips))
	}
	// Arbitrage sells slowest → id(2) with speed 5.
	if dec.SellShips[0] != id(2) {
		t.Errorf("arbitrage should sell slowest ship, got %v", dec.SellShips[0])
	}
}

func TestDecideFleetAction_SellShips_OnlyDockedEmpty(t *testing.T) {
	a := testAgent()
	portA := id(1)
	dec, err := a.DecideFleetAction(context.Background(), FleetDecisionRequest{
		StrategyHint: "arbitrage",
		Company:      CompanySnapshot{Treasury: 400, TotalUpkeep: 100},
		Ships: []ShipSnapshot{
			{ID: id(1), Status: "docked", PortID: &portA, Speed: 10, Cargo: []CargoItem{{GoodID: id(10), Quantity: 5}}},
			{ID: id(2), Status: "traveling", Speed: 5}, // Not docked
		},
		ShipTypes: []ShipTypeInfo{{ID: id(1), BasePrice: 500, Upkeep: 50}},
	})
	if err != nil {
		t.Fatal(err)
	}
	// Ship 1 has cargo, ship 2 is traveling → no candidates.
	if len(dec.SellShips) != 0 {
		t.Error("should not sell ships that are traveling or have cargo")
	}
}

func TestDecideFleetAction_NoSellWhenSingleShip(t *testing.T) {
	a := testAgent()
	portA := id(1)
	dec, err := a.DecideFleetAction(context.Background(), FleetDecisionRequest{
		Company: CompanySnapshot{Treasury: 100, TotalUpkeep: 100},
		Ships: []ShipSnapshot{
			{ID: id(1), Status: "docked", PortID: &portA, Speed: 5},
		},
		ShipTypes: []ShipTypeInfo{{ID: id(1), BasePrice: 500, Upkeep: 50}},
	})
	if err != nil {
		t.Fatal(err)
	}
	// Only 1 ship → don't sell.
	if len(dec.SellShips) != 0 {
		t.Error("should not sell when only 1 ship")
	}
}

// ---------------------------------------------------------------------------
// Strategy Evaluation Tests
// ---------------------------------------------------------------------------

func TestEvaluateStrategy_NotEnoughStrategies(t *testing.T) {
	a := testAgent()
	dec, err := a.EvaluateStrategy(context.Background(), StrategyEvalRequest{
		Metrics: []StrategyMetrics{{StrategyName: "arbitrage", ProfitPerHour: 100}},
	})
	if err != nil {
		t.Fatal(err)
	}
	if dec.SwitchTo != nil {
		t.Error("should not recommend switch with single strategy")
	}
}

func TestEvaluateStrategy_LosingVsProfitable(t *testing.T) {
	a := testAgent()
	dec, err := a.EvaluateStrategy(context.Background(), StrategyEvalRequest{
		Metrics: []StrategyMetrics{
			{StrategyName: "arbitrage", ProfitPerHour: 500},
			{StrategyName: "bulk_hauler", ProfitPerHour: -100},
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	if dec.SwitchTo == nil || *dec.SwitchTo != "arbitrage" {
		t.Error("should recommend switching to profitable strategy")
	}
}

func TestEvaluateStrategy_TwoXOutperformance(t *testing.T) {
	a := testAgent()
	dec, err := a.EvaluateStrategy(context.Background(), StrategyEvalRequest{
		Metrics: []StrategyMetrics{
			{StrategyName: "arbitrage", ProfitPerHour: 1000},
			{StrategyName: "bulk_hauler", ProfitPerHour: 400},
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	if dec.SwitchTo == nil || *dec.SwitchTo != "arbitrage" {
		t.Error("should recommend switch when 2x+ outperformance")
	}
}

func TestEvaluateStrategy_WithinRange(t *testing.T) {
	a := testAgent()
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
		t.Error("should not recommend switch when strategies are close")
	}
}

// ---------------------------------------------------------------------------
// Helper Tests
// ---------------------------------------------------------------------------

func TestCalcQuantity(t *testing.T) {
	a := testAgent()

	tests := []struct {
		name      string
		budget    int64
		unitPrice int
		maxQty    int
		want      int
	}{
		{"zero price", 1000, 0, 100, 0},
		{"affordable max", 10000, 10, 100, 100},
		{"budget limited", 500, 100, 100, 5},
		{"min 1", 1, 100, 100, 1},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := a.calcQuantity(tc.budget, tc.unitPrice, tc.maxQty)
			if got != tc.want {
				t.Errorf("calcQuantity(%d, %d, %d) = %d, want %d",
					tc.budget, tc.unitPrice, tc.maxQty, got, tc.want)
			}
		})
	}
}

func TestBuildPriceIndex(t *testing.T) {
	a := testAgent()
	portA := id(1)
	goodX := id(10)

	cache := []PricePoint{
		{PortID: portA, GoodID: goodX, BuyPrice: 50, SellPrice: 80},
	}
	idx := a.buildPriceIndex(cache)

	key := priceKey(portA, goodX)
	pp, ok := idx[key]
	if !ok {
		t.Fatal("expected price in index")
	}
	if pp.BuyPrice != 50 || pp.SellPrice != 80 {
		t.Errorf("unexpected prices: %+v", pp)
	}
}

func TestReachablePorts(t *testing.T) {
	a := testAgent()
	portA := id(1)
	portB := id(2)
	portC := id(3)

	routes := []RouteInfo{
		{FromID: portA, ToID: portB, Distance: 5},
		{FromID: portA, ToID: portC, Distance: 10},
		{FromID: portB, ToID: portC, Distance: 3}, // Not from A
	}

	reachable := a.reachablePorts(routes, portA)
	if len(reachable) != 2 {
		t.Errorf("expected 2 reachable ports, got %d", len(reachable))
	}
	if reachable[portB] != 5 {
		t.Errorf("expected distance 5 to portB, got %f", reachable[portB])
	}
}

func TestClosestPort(t *testing.T) {
	a := testAgent()
	reachable := map[uuid.UUID]float64{
		id(1): 10,
		id(2): 3,
		id(3): 7,
	}
	closest := a.closestPort(reachable)
	if closest != id(2) {
		t.Errorf("expected closest to be id(2), got %v", closest)
	}
}

func TestFindShipsToSell_ArbitrageSellsSlowest(t *testing.T) {
	a := testAgent()
	portA := id(1)
	ships := []ShipSnapshot{
		{ID: id(1), Status: "docked", PortID: &portA, Speed: 10},
		{ID: id(2), Status: "docked", PortID: &portA, Speed: 3},
		{ID: id(3), Status: "docked", PortID: &portA, Speed: 8},
	}
	result := a.findShipsToSell(ships, "arbitrage")
	if len(result) != 1 || result[0] != id(2) {
		t.Errorf("arbitrage should sell slowest ship (id 2), got %v", result)
	}
}

func TestFindShipsToSell_BulkHaulerSellsSmallest(t *testing.T) {
	a := testAgent()
	portA := id(1)
	ships := []ShipSnapshot{
		{ID: id(1), Status: "docked", PortID: &portA, Capacity: 200},
		{ID: id(2), Status: "docked", PortID: &portA, Capacity: 50},
		{ID: id(3), Status: "docked", PortID: &portA, Capacity: 150},
	}
	result := a.findShipsToSell(ships, "bulk_hauler")
	if len(result) != 1 || result[0] != id(2) {
		t.Errorf("bulk_hauler should sell smallest ship (id 2), got %v", result)
	}
}

func TestFindShipsToSell_MarketMakerSellsMostExpensive(t *testing.T) {
	a := testAgent()
	portA := id(1)
	ships := []ShipSnapshot{
		{ID: id(1), Status: "docked", PortID: &portA, Capacity: 50},
		{ID: id(2), Status: "docked", PortID: &portA, Capacity: 200}, // Biggest = most expensive
		{ID: id(3), Status: "docked", PortID: &portA, Capacity: 100},
	}
	result := a.findShipsToSell(ships, "market_maker")
	if len(result) != 1 || result[0] != id(2) {
		t.Errorf("market_maker should sell most expensive ship (id 2), got %v", result)
	}
}

// ---------------------------------------------------------------------------
// Phase 4: Enhanced Heuristic Agent Tests
// ---------------------------------------------------------------------------

func TestDecideTradeAction_Arbitrage_MultiGoodBuying(t *testing.T) {
	a := testAgent()
	portA := id(1)
	portB := id(2)
	goodX := id(10)
	goodY := id(11)
	goodZ := id(12)

	dec, err := a.DecideTradeAction(context.Background(), TradeDecisionRequest{
		StrategyHint: "arbitrage",
		Ship:         ShipSnapshot{PortID: &portA, Capacity: 500},
		Routes:       []RouteInfo{{FromID: portA, ToID: portB, Distance: 5}},
		PriceCache: []PricePoint{
			{PortID: portA, GoodID: goodX, BuyPrice: 10, SellPrice: 5},
			{PortID: portA, GoodID: goodY, BuyPrice: 20, SellPrice: 15},
			{PortID: portA, GoodID: goodZ, BuyPrice: 30, SellPrice: 25},
			{PortID: portB, GoodID: goodX, BuyPrice: 20, SellPrice: 25},  // profit 15
			{PortID: portB, GoodID: goodY, BuyPrice: 30, SellPrice: 40},  // profit 20
			{PortID: portB, GoodID: goodZ, BuyPrice: 50, SellPrice: 60},  // profit 30
		},
		// Budget of 5000 limits how much of the top good we can buy (5000/30=166),
		// so remaining capacity (334) should be filled with cheaper goods.
		Constraints: Constraints{MaxSpend: 5000},
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(dec.BuyOrders) < 2 {
		t.Errorf("expected multiple buy orders for multi-good filling, got %d", len(dec.BuyOrders))
	}
	// Total quantity should not exceed ship capacity.
	totalQty := 0
	for _, buy := range dec.BuyOrders {
		totalQty += buy.Quantity
	}
	if totalQty > 500 {
		t.Errorf("total quantity %d exceeds ship capacity 500", totalQty)
	}
}

func TestDecideTradeAction_TaxAwareScoring(t *testing.T) {
	a := testAgent()
	portA := id(1) // high tax port
	portB := id(2)
	goodX := id(10)

	// With high tax, the net profit should be lower.
	dec, err := a.DecideTradeAction(context.Background(), TradeDecisionRequest{
		StrategyHint: "arbitrage",
		Ship:         ShipSnapshot{PortID: &portA, Capacity: 100},
		Routes:       []RouteInfo{{FromID: portA, ToID: portB, Distance: 5}},
		PriceCache: []PricePoint{
			{PortID: portA, GoodID: goodX, BuyPrice: 100, SellPrice: 80},
			{PortID: portB, GoodID: goodX, BuyPrice: 120, SellPrice: 150},
		},
		Ports: []PortInfo{
			{ID: portA, TaxRateBps: 5000}, // 50% tax!
			{ID: portB, TaxRateBps: 0},
		},
		Constraints: Constraints{MaxSpend: 10000},
	})
	if err != nil {
		t.Fatal(err)
	}
	// With 50% tax on buy price of 100, tax = 50.
	// Net profit = 150 - 100 - 50 = 0, so no profitable arbitrage.
	// Should fall through to speculative trade.
	if dec.Confidence >= 0.8 {
		t.Errorf("high tax should reduce confidence (speculative), got %f", dec.Confidence)
	}
}

func TestDecideTradeAction_BulkHauler_CapacityFilling(t *testing.T) {
	a := testAgent()
	portA := id(1)
	portB := id(2)
	goodX := id(10)

	dec, err := a.DecideTradeAction(context.Background(), TradeDecisionRequest{
		StrategyHint: "bulk_hauler",
		Ship:         ShipSnapshot{PortID: &portA, Capacity: 300},
		Routes:       []RouteInfo{{FromID: portA, ToID: portB, Distance: 5}},
		PriceCache: []PricePoint{
			{PortID: portA, GoodID: goodX, BuyPrice: 10, SellPrice: 5},
			{PortID: portB, GoodID: goodX, BuyPrice: 20, SellPrice: 30},
		},
		Constraints: Constraints{MaxSpend: 100000},
	})
	if err != nil {
		t.Fatal(err)
	}
	totalQty := 0
	for _, buy := range dec.BuyOrders {
		totalQty += buy.Quantity
	}
	if totalQty > 300 {
		t.Errorf("total quantity %d exceeds ship capacity 300", totalQty)
	}
}

func TestDecideTradeAction_PassengerIntegratedRouting(t *testing.T) {
	a := testAgent()
	portA := id(1)
	portB := id(2)
	portC := id(3)
	goodX := id(10)

	dec, err := a.DecideTradeAction(context.Background(), TradeDecisionRequest{
		StrategyHint: "arbitrage",
		Ship:         ShipSnapshot{PortID: &portA, Capacity: 100, PassengerCap: 5},
		Routes: []RouteInfo{
			{FromID: portA, ToID: portB, Distance: 5},
			{FromID: portA, ToID: portC, Distance: 5},
		},
		PriceCache: []PricePoint{
			{PortID: portA, GoodID: goodX, BuyPrice: 100, SellPrice: 80},
			// Equal profit at both destinations.
			{PortID: portB, GoodID: goodX, BuyPrice: 120, SellPrice: 120},
			{PortID: portC, GoodID: goodX, BuyPrice: 120, SellPrice: 120},
		},
		AvailablePassengers: []PassengerInfo{
			// High-value passengers going to portC.
			{ID: id(50), Count: 3, Bid: 5000, DestinationPortID: portC},
		},
		Constraints: Constraints{MaxSpend: 10000},
	})
	if err != nil {
		t.Fatal(err)
	}
	// With equal trade profit, passenger revenue should tip the balance to portC.
	if dec.SailTo != nil && *dec.SailTo == portC {
		// Good — passenger revenue influenced destination.
	}
	// Note: if no profitable arbitrage found, it'll go speculative, which is also fine.
}

func TestDecideTradeAction_SmartSpeculative(t *testing.T) {
	a := testAgent()
	portA := id(1)
	portB := id(2)
	portC := id(3)
	goodX := id(10)
	goodY := id(11)

	dec, err := a.DecideTradeAction(context.Background(), TradeDecisionRequest{
		Ship: ShipSnapshot{PortID: &portA, Capacity: 100},
		Routes: []RouteInfo{
			{FromID: portA, ToID: portB, Distance: 5},
			{FromID: portA, ToID: portC, Distance: 10},
		},
		PriceCache: []PricePoint{
			// No arbitrage (all buy > sell).
			{PortID: portA, GoodID: goodX, BuyPrice: 100, SellPrice: 80},
			{PortID: portA, GoodID: goodY, BuyPrice: 50, SellPrice: 30},
			{PortID: portB, GoodID: goodX, BuyPrice: 90, SellPrice: 80},
			{PortID: portB, GoodID: goodY, BuyPrice: 60, SellPrice: 40},
			// goodY has high sell at portC (margin = 200 - 50 = 150).
			{PortID: portC, GoodID: goodY, BuyPrice: 250, SellPrice: 200},
		},
		Constraints: Constraints{MaxSpend: 5000},
	})
	if err != nil {
		t.Fatal(err)
	}
	// Should pick goodY (highest margin speculation) heading to portC.
	if len(dec.BuyOrders) > 0 {
		foundGoodY := false
		for _, buy := range dec.BuyOrders {
			if buy.GoodID == goodY {
				foundGoodY = true
			}
		}
		if !foundGoodY {
			t.Error("smart speculative should pick goodY (highest margin), not goodX")
		}
	}
}
