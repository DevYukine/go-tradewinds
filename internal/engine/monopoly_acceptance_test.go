package engine

import (
	"encoding/json"
	"reflect"
	"testing"
	"time"
)

func TestWithoutMonopolizedNPCAsks(t *testing.T) {
	now := time.Date(2026, 10, 25, 12, 0, 0, 0, time.UTC)
	ports := []Port{
		{ID: "plymouth", BankingRegion: "channel"},
		{ID: "london", BankingRegion: "channel"},
		{ID: "lisbon", BankingRegion: "iberia"},
	}
	active := Monopoly{
		ID:              "cloth-monopoly",
		BankingRegion:   "channel",
		CommodityID:     "coarse_cloth",
		HolderCompanyID: "kraken",
		ExpiresAt:       now.Add(time.Hour),
		InForce:         true,
	}
	for _, tc := range []struct {
		name     string
		change   func(*Monopoly)
		own      map[string]bool
		filtered bool
	}{
		{name: "foreign active monopoly", filtered: true},
		{name: "expired", change: func(m *Monopoly) { m.ExpiresAt = now.Add(-time.Second) }},
		{name: "expires exactly now", change: func(m *Monopoly) { m.ExpiresAt = now }},
		{name: "not in force", change: func(m *Monopoly) { m.InForce = false }},
		{name: "other region", change: func(m *Monopoly) { m.BankingRegion = "baltic" }},
		{name: "other commodity", change: func(m *Monopoly) { m.CommodityID = "silk" }},
		{name: "own holder", own: map[string]bool{"kraken": true}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			markets := make(map[string]Market)
			for _, port := range ports {
				var market Market
				if err := json.Unmarshal([]byte(`{
					"visibility":"quay","customs_bps":250,
					"books":[{
						"commodity_id":"coarse_cloth",
						"depth":{
							"bids":[
								{"order_id":"npc-bid","price_pence":40000,"remaining_quantity":15,"is_npc":true},
								{"order_id":"player-bid","price_pence":45000,"remaining_quantity":30,"is_npc":false}
							],
							"asks":[
								{"order_id":"npc-ask-1","price_pence":47000,"remaining_quantity":20,"is_npc":true},
								{"order_id":"player-ask","price_pence":48000,"remaining_quantity":9,"is_npc":false},
								{"order_id":"npc-ask-2","price_pence":49000,"remaining_quantity":10,"is_npc":true}
							]
						}
					},{
						"commodity_id":"grain",
						"depth":{
							"bids":[{"order_id":"grain-bid","price_pence":200,"remaining_quantity":12,"is_npc":true}],
							"asks":[{"order_id":"grain-ask","price_pence":300,"remaining_quantity":18,"is_npc":true}]
						}
					}]
				}`), &market); err != nil {
					t.Fatal(err)
				}
				market.PortID = port.ID
				markets[port.ID] = market
			}
			before, err := json.Marshal(markets)
			if err != nil {
				t.Fatal(err)
			}
			var original map[string]Market
			if err := json.Unmarshal(before, &original); err != nil {
				t.Fatal(err)
			}
			monopoly := active
			if tc.change != nil {
				tc.change(&monopoly)
			}
			got := withoutMonopolizedNPCAsks(markets, ports, []Monopoly{monopoly}, tc.own, now)
			if len(got) != len(markets) {
				t.Fatalf("market count = %d, want %d", len(got), len(markets))
			}
			for _, port := range ports {
				want := original[port.ID]
				if tc.filtered && port.BankingRegion == "channel" {
					want.Books[0].Depth.Asks = []Level{want.Books[0].Depth.Asks[1]}
				}
				if !reflect.DeepEqual(got[port.ID], want) {
					t.Errorf("%s market = %+v, want only monopolized NPC cloth asks removed", port.ID, got[port.ID])
				}
			}
			// Decode again because the expected filtered view above owns its
			// slices; this comparison is against the entire original input.
			if err := json.Unmarshal(before, &original); err != nil {
				t.Fatal(err)
			}
			if !reflect.DeepEqual(markets, original) {
				t.Fatal("filter mutated the input market map or its books/depth slices")
			}
			// A returned copy must not expose the original's nested slices,
			// including unchanged player asks, bids and other commodities.
			for id, market := range got {
				market.Books[0].Depth.Bids[0].PricePence++
				market.Books[0].Depth.Asks[0].RemainingQuantity++
				market.Books[1].Depth.Asks[0].PricePence++
				market.PortID = "changed"
				got[id] = market
			}
			delete(got, "lisbon")
			if !reflect.DeepEqual(markets, original) {
				t.Fatal("returned market copy aliases the input map or nested slices")
			}
		})
	}
}
