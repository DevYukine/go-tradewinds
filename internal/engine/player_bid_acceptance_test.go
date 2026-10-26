package engine

import "testing"

func TestPlayerBidCapacity(t *testing.T) {
	price, quantity := int64(50_000), int64(30)
	zero, negative := int64(0), int64(-1)
	for _, tc := range []struct {
		name      string
		price     *int64
		quantity  *int64
		reserved  int64
		wantTons  int64
		wantPrice int64
	}{
		{"unreserved", &price, &quantity, 0, 30, 50_000},
		{"partly reserved", &price, &quantity, 10, 20, 50_000},
		{"over reserved", &price, &quantity, 40, 0, 0},
		{"fully reserved", &price, &quantity, 30, 0, 0},
		{"missing price", nil, &quantity, 0, 0, 0},
		{"missing quantity", &price, nil, 0, 0, 0},
		{"zero quantity", &price, &zero, 0, 0, 0},
		{"negative quantity", &price, &negative, 0, 0, 0},
		{"zero price", &zero, &quantity, 0, 0, 0},
		{"negative price", &negative, &quantity, 0, 0, 0},
	} {
		t.Run(tc.name, func(t *testing.T) {
			report := GossipCommodity{
				CommodityID:           "coarse_cloth",
				BestPlayerBidPence:    tc.price,
				BestPlayerBidQuantity: tc.quantity,
			}
			gotPrice, gotTons := playerBidCapacity(report, tc.reserved)
			if gotTons != tc.wantTons {
				t.Fatalf("capacity = (%d pence, %d tons), want %d tons", gotPrice, gotTons, tc.wantTons)
			}
			// The price of an unusable quote is unspecified; only a positive
			// capacity may authorize revenue at the observed player price.
			if tc.wantTons > 0 && gotPrice != tc.wantPrice {
				t.Fatalf("price = %d, want %d", gotPrice, tc.wantPrice)
			}
		})
	}
}
