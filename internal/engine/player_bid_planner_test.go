package engine

import (
	"encoding/json"
	"testing"
	"time"
)

func TestPlayerBidGossipPricesWholeLotWithNPCStress(t *testing.T) {
	for _, tc := range []struct {
		name           string
		ask            int64
		reserved       int64
		accepted       bool
		probe          bool
		liveDepth      bool
		equalBid       bool
		lowerNPC       bool
		filledTons     int64
		oldFill        bool
		olderCommodity bool
	}{
		{name: "twenty tons fit fresh player bid", ask: 20_000, accepted: true},
		{name: "remaining twenty tons fit", ask: 20_000, reserved: 10, accepted: true},
		{name: "other commitments exhaust quantity", ask: 20_000, reserved: 11},
		{name: "NPC stress cannot cover purchase", ask: 46_000},
		{name: "one-ton fallback remains available below NPC stress floor", ask: 46_000, accepted: true, probe: true},
		{name: "fresh quay depth cannot be overridden by gossip", ask: 20_000, liveDepth: true},
		{name: "player best bid cannot stand in for NPC floor", ask: 20_000, equalBid: true},
		{name: "equal best bid still permits one-ton rule", ask: 20_000, equalBid: true, probe: true, accepted: true},
		{name: "minimum fresh NPC floor applies across reports", ask: 43_000, lowerNPC: true},
		{name: "our newer sell fills consume player quantity", ask: 20_000, filledTons: 11},
		{name: "fills already reflected in report are not deducted twice", ask: 20_000, filledTons: 11, oldFill: true, accepted: true},
		{name: "fills after commodity snapshot but before report consume quantity", ask: 20_000, filledTons: 11, oldFill: true, olderCommodity: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			in := sourcingInputFixture(t)
			delete(in.Markets, "portsmouth")
			in.Markets["le_havre"] = sourcingFixture[Market](t, `{"port_id":"le_havre","visibility":"quay","customs_bps":250,"books":[{"commodity_id":"provisions","depth":{"asks":[{"price_pence":100,"remaining_quantity":500}]}},{"commodity_id":"coarse_cloth","depth":{"asks":[{"price_pence":20000,"remaining_quantity":100}]}}]}`)
			in.Markets["le_havre"].Books[1].Depth.Asks[0].PricePence = tc.ask
			in.ReservedPlayerBidTons = map[string]map[string]int64{"portsmouth": {"coarse_cloth": tc.reserved}}
			npcBid, playerBid, playerTons := int64(60_000), int64(50_000), int64(30)
			if tc.equalBid {
				npcBid = playerBid
			}
			reportAt := in.Now.Add(-30 * time.Second)
			commodityAt := reportAt
			if tc.olderCommodity {
				commodityAt = reportAt.Add(-30 * time.Second)
			}
			detail := in.Ports["le_havre"]
			detail.CoffeeHouseGossip = []Gossip{{
				SubjectPortID: "portsmouth", SnapshotAt: reportAt,
				Commodities: []GossipCommodity{{
					CommodityID: "coarse_cloth", SnapshotAt: commodityAt,
					BestBidPence: &npcBid, BestPlayerBidPence: &playerBid, BestPlayerBidQuantity: &playerTons,
				}},
			}}
			if tc.lowerNPC {
				lowBid, noCapacity := int64(55_000), int64(0)
				other := detail.CoffeeHouseGossip[0]
				other.Commodities = []GossipCommodity{{
					CommodityID: "coarse_cloth", SnapshotAt: reportAt,
					BestBidPence: &lowBid, BestPlayerBidPence: &playerBid, BestPlayerBidQuantity: &noCapacity,
				}}
				detail.CoffeeHouseGossip = append(detail.CoffeeHouseGossip, other)
			}
			if tc.filledTons > 0 {
				fillAt := in.Now.Add(-time.Second)
				if tc.oldFill {
					fillAt = reportAt.Add(-time.Second)
				}
				in.PlayerBidSellFills = []PlayerBidSellFill{{PortID: "portsmouth", CommodityID: "coarse_cloth", Side: "ask", Quantity: tc.filledTons, CreatedAt: fillAt}}
			}
			if tc.liveDepth {
				in.Markets["portsmouth"] = sourcingFixture[Market](t, `{"port_id":"portsmouth","visibility":"quay","books":[{"commodity_id":"coarse_cloth","depth":{"bids":[{"price_pence":60000,"remaining_quantity":10}]}}]}`)
			}
			in.Ports["le_havre"] = detail
			quantity := int64(20)
			if tc.probe {
				quantity = 1
			}
			plan := evaluatePlan(in, in.Catalog.ShipClasses[0], in.Catalog.Ports[0], Plan{
				Kind: "spot", ShipID: in.Ship.Ship.ID, Origin: "le_havre", Destination: "portsmouth", Commodity: "coarse_cloth", Quantity: quantity,
			}, 0, 0)
			if plan.Accepted != tc.accepted {
				t.Fatalf("accepted = %t, want %t: %+v", plan.Accepted, tc.accepted, plan)
			}
			if tc.probe {
				if plan.Economics.RevenuePence != npcBid || plan.PriceSource != "recent observed gossip; one-ton probe; unverified bid size" || plan.playerBidStress || plan.Economics.RevenueBasis != "gossip" {
					t.Fatalf("failed player stress did not retain the one-ton probe: %+v", plan)
				}
				return
			}
			if tc.accepted {
				if plan.Economics.RevenuePence != 1_000_000 || plan.SellLimitPence != 45_000 || plan.PriceSource != playerBidGossipSource || plan.Economics.RevenueBasis != "gossip" {
					t.Fatalf("player revenue or NPC floor lost: %+v", plan)
				}
				if plan.Economics.ExpectedNetPence-plan.Economics.PriceUncertaintyStressPence <= 0 {
					t.Fatalf("accepted below NPC stressed net floor: %+v", plan.Economics)
				}
				fees := plan.Economics.ExpectedNetPence - plan.Economics.PriceUncertaintyStressPence
				if revised := addFundingFees(plan, fees); revised.Accepted {
					t.Fatalf("funding fees consumed the NPC floor without rejecting: %+v", revised)
				}
			}
		})
	}
}

func TestRestingPlayerBidReservationsUseUnfilledBidTons(t *testing.T) {
	runner := Runner{snapshot: map[string]json.RawMessage{"orders": json.RawMessage(`[
		{"port_id":"portsmouth","commodity_id":"coarse_cloth","side":"bid","quantity":20,"filled_quantity":5},
		{"port_id":"portsmouth","commodity_id":"coarse_cloth","side":"bid","quantity":10,"filled_quantity":2},
		{"port_id":"portsmouth","commodity_id":"coarse_cloth","side":"ask","quantity":100,"filled_quantity":0},
		{"port_id":"portsmouth","commodity_id":"coarse_cloth","side":"bid","quantity":10,"filled_quantity":10},
		{"port_id":"amsterdam","commodity_id":"coarse_cloth","side":"bid","quantity":7,"filled_quantity":0}
	]`)}}
	reserved := runner.restingBidReservations()
	if reserved["portsmouth"]["coarse_cloth"] != 23 || reserved["amsterdam"]["coarse_cloth"] != 7 {
		t.Fatalf("wrong resting bid reservations: %+v", reserved)
	}
}
