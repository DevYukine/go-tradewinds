package engine

import (
	"encoding/json"
	"strings"
	"testing"
)

func TestEvaluateMarqueAcceptance(t *testing.T) {
	t.Parallel()
	base := MarqueInput{
		Quote: MarqueQuote{
			CostPence:             500_000,
			ExpectedNetPrizePence: 830_000,
			LadenPassagesPerDay:   3,
			MarqueBPSInForce:      0,
			AddedPiracyBPS:        500,
		},
		Exposed:          false,
		InForceCostPence: 0,
		LiquidPence:      11_394_526,
	}
	reasons := make(map[string]string)
	for _, tc := range []struct {
		name         string
		change       func(*MarqueInput)
		wantAccepted bool
	}{
		{name: "profitable unexposed zone", wantAccepted: true},
		{name: "fleet or underwriting exposure", change: func(in *MarqueInput) {
			in.Exposed = true
		}},
		{name: "floor-priced quote", change: func(in *MarqueInput) {
			in.Quote.FloorApplied = true
		}},
		{name: "insufficient laden passages", change: func(in *MarqueInput) {
			in.Quote.LadenPassagesPerDay = 0.5
		}},
		{name: "zone marque bps exceeded", change: func(in *MarqueInput) {
			in.Quote.MarqueBPSInForce = 4_600
		}},
		{name: "expected prize below one point four times cost", change: func(in *MarqueInput) {
			in.Quote.ExpectedNetPrizePence = 690_000
		}},
		{name: "single marque exceeds five percent of liquidity", change: func(in *MarqueInput) {
			in.LiquidPence = 9_000_000
		}},
		{name: "in-force cost exceeds ten percent of liquidity", change: func(in *MarqueInput) {
			in.InForceCostPence = 700_000
		}},
		{name: "three expired marques below realized return floor", change: func(in *MarqueInput) {
			in.ExpiredCount = 3
			in.ExpiredPrizePence = 200_000
			in.ExpiredCostPence = 300_000
		}},
		{name: "expected prize exactly one point four times cost", wantAccepted: true, change: func(in *MarqueInput) {
			in.Quote.ExpectedNetPrizePence = 700_000
		}},
		{name: "three expired marques exactly at realized return floor", wantAccepted: true, change: func(in *MarqueInput) {
			in.ExpiredCount = 3
			in.ExpiredPrizePence = 240_000
			in.ExpiredCostPence = 300_000
		}},
		{name: "two expired marques with zero prize", wantAccepted: true, change: func(in *MarqueInput) {
			in.ExpiredCount = 2
			in.ExpiredPrizePence = 0
			in.ExpiredCostPence = 300_000
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			in := base
			if tc.change != nil {
				tc.change(&in)
			}
			decision := EvaluateMarque(in)
			if decision.Accepted != tc.wantAccepted {
				t.Fatalf("EvaluateMarque = %+v, want accepted=%t", decision, tc.wantAccepted)
			}
			if tc.wantAccepted {
				return
			}
			reason := strings.TrimSpace(decision.Reason)
			if reason == "" {
				t.Fatal("rejected marque has no reason")
			}
			if previous, exists := reasons[reason]; exists {
				t.Errorf("rejection reason %q duplicates rule %q", reason, previous)
			}
			reasons[reason] = tc.name
		})
	}
}

func TestMarqueQuoteDecodeOpenAPIFields(t *testing.T) {
	t.Parallel()
	var quote MarqueQuote
	err := json.Unmarshal([]byte(`{
		"sea_zone_id":"western_approaches",
		"added_piracy_bps":500,
		"duration_game_hours":60,
		"cost_pence":500000,
		"expected_net_prize_pence":830000,
		"laden_passages_per_day":3.5,
		"marque_bps_in_force":1200,
		"floor_applied":true
	}`), &quote)
	if err != nil {
		t.Fatal(err)
	}
	want := MarqueQuote{
		SeaZoneID:             "western_approaches",
		AddedPiracyBPS:        500,
		DurationGameHours:     60,
		CostPence:             500_000,
		ExpectedNetPrizePence: 830_000,
		LadenPassagesPerDay:   3.5,
		MarqueBPSInForce:      1_200,
		FloorApplied:          true,
	}
	if quote != want {
		t.Fatalf("decoded marque quote = %+v, want %+v", quote, want)
	}
}
