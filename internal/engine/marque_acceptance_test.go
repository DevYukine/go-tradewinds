package engine

import (
	"encoding/json"
	"fmt"
	"strings"
	"testing"
	"time"
)

func TestEvaluateMarqueAcceptance(t *testing.T) {
	t.Parallel()
	base := MarqueInput{
		Quote: MarqueQuote{
			CostPence:             50_000,
			ExpectedNetPrizePence: 83_000,
			LadenPassagesPerDay:   3,
			MarqueBPSInForce:      0,
			AddedPiracyBPS:        500,
		},
		Exposed:          false,
		InForceCostPence: 0,
		LiquidPence:      1_139_452,
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
		{name: "expected prize below one point five times cost", change: func(in *MarqueInput) {
			in.Quote.ExpectedNetPrizePence = 74_999
		}},
		{name: "single marque exceeds five percent of liquidity", change: func(in *MarqueInput) {
			in.LiquidPence = 900_000
		}},
		{name: "one active marque blocks another", change: func(in *MarqueInput) {
			in.InForceCostPence = 1
		}},
		{name: "expected prize exactly one point five times cost", wantAccepted: true, change: func(in *MarqueInput) {
			in.Quote.ExpectedNetPrizePence = 75_000
		}},
		{name: "lifetime prizes offset losses", wantAccepted: true, change: func(in *MarqueInput) {
			in.ExpiredPrizePence = 500_000
			in.ExpiredCostPence = 600_000
		}},
		{name: "lifetime unrecovered spending exactly at cap", wantAccepted: true, change: func(in *MarqueInput) {
			in.ExpiredCostPence = 250_000
		}},
		{name: "lifetime unrecovered spending exceeds cap", change: func(in *MarqueInput) {
			in.ExpiredCostPence = 250_001
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

func TestEvaluateMarqueRollingZoneEV(t *testing.T) {
	t.Parallel()
	now := time.Date(2026, 11, 2, 12, 0, 0, 0, time.UTC)
	for _, tc := range []struct {
		name   string
		prizes []int64
		want   bool
	}{
		{"two expiries are not enough", []int64{0, 0}, true},
		{"three expiries one penny below cost", []int64{10_000, 10_000, 9_999}, false},
		{"three expiries exactly at cost", []int64{10_000, 10_000, 10_000}, true},
		{"large winner offsets three losers", []int64{50_000, 0, 0, 0}, true},
		{"one large loser outweighs two winners", []int64{0, 10_100, 10_100}, false},
		{"only the latest six zone expiries count", []int64{0, 10_000, 10_000, 10_000, 10_000, 10_000, 10_000}, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			outcomes := make(map[string]marqueOutcome)
			for i, prize := range tc.prizes {
				id := fmt.Sprint(i)
				outcomes[id] = marqueOutcome{ID: id, SeaZoneID: "zone", ExpiredAt: now.Add(time.Duration(i-len(tc.prizes)) * time.Minute),
					CostPence: 10_000, PrizePence: prize}
			}
			zones, global := rollingMarqueReturns(outcomes, now, 60)
			input := MarqueInput{
				Quote: MarqueQuote{SeaZoneID: "zone", CostPence: 10_000, ExpectedNetPrizePence: 20_000,
					LadenPassagesPerDay: 2, AddedPiracyBPS: 500},
				Now: now, LiquidPence: 1_000_000, ZoneReturns: zones, GlobalReturns: global,
			}
			globalPaused := global.Count > 0 && global.PrizePence < global.CostPence
			want := tc.want && !globalPaused
			if got := EvaluateMarque(input); got.Accepted != want {
				t.Fatalf("zone EV admission = %+v, want %t with %+v", got, want, zones["zone"])
			}
			input.Quote.SeaZoneID = "another-zone"
			if got := EvaluateMarque(input); got.Accepted == globalPaused {
				t.Fatalf("another zone admission = %+v, want global pause %t", got, globalPaused)
			}
		})
	}
}

func TestMarqueRollingReturnsUseServerGameDays(t *testing.T) {
	t.Parallel()
	now := time.Date(2026, 11, 2, 12, 0, 0, 0, time.UTC)
	for _, scale := range []int64{60, 120} {
		t.Run(fmt.Sprint(scale), func(t *testing.T) {
			for _, tc := range []struct {
				name        string
				expiredAt   time.Time
				zoneCount   int
				globalCount int
			}{
				{"thirty game days inclusive", now.Add(-gameDuration(30, scale)), 3, 3},
				{"older than thirty game days", now.Add(-gameDuration(30, scale) - time.Nanosecond), 0, 3},
				{"sixty game days inclusive", now.Add(-gameDuration(60, scale)), 0, 3},
				{"older than sixty game days", now.Add(-gameDuration(60, scale) - time.Nanosecond), 0, 0},
				{"legacy expiry unknown", time.Time{}, 0, 0},
				{"not yet expired", now.Add(time.Nanosecond), 0, 0},
			} {
				t.Run(tc.name, func(t *testing.T) {
					outcomes := make(map[string]marqueOutcome)
					for i := range 3 {
						id := fmt.Sprint(i)
						outcomes[id] = marqueOutcome{ID: id, SeaZoneID: "zone", ExpiredAt: tc.expiredAt, CostPence: 10_000}
					}
					zones, global := rollingMarqueReturns(outcomes, now, scale)
					if zones["zone"].Count != tc.zoneCount || global.Count != tc.globalCount {
						t.Fatalf("rolling counts = %+v / %+v, want %d / %d", zones["zone"], global, tc.zoneCount, tc.globalCount)
					}
				})
			}
		})
	}
}

func TestEvaluateMarqueGlobalEVAndReentryBoundaries(t *testing.T) {
	t.Parallel()
	now := time.Date(2026, 11, 2, 12, 0, 0, 0, time.UTC)
	for _, tc := range []struct {
		name     string
		prize    int64
		expected int64
		want     bool
	}{
		{"exactly one point zero five", 10_500, 15_000, true},
		{"one penny below global return", 10_499, 15_000, false},
		{"one penny below reentry quote", 0, 19_999, false},
		{"exactly twice cost reenters", 0, 20_000, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			input := MarqueInput{
				Quote: MarqueQuote{SeaZoneID: "new-zone", CostPence: 10_000, ExpectedNetPrizePence: tc.expected,
					LadenPassagesPerDay: 2, AddedPiracyBPS: 500},
				LiquidPence: 1_000_000, Now: now,
				GlobalReturns: marqueReturns{Count: 1, CostPence: 10_000, PrizePence: tc.prize},
			}
			if got := EvaluateMarque(input); got.Accepted != tc.want {
				t.Fatalf("global EV admission = %+v, want %t", got, tc.want)
			}
		})
	}
	outcomes := make(map[string]marqueOutcome)
	for i := range 11 {
		id := fmt.Sprint(i)
		prize := int64(0)
		if i == 0 {
			prize = 1_000_000
		}
		outcomes[id] = marqueOutcome{ID: id, SeaZoneID: id, ExpiredAt: now.Add(time.Duration(i-11) * time.Minute),
			CostPence: 10_000, PrizePence: prize}
	}
	_, global := rollingMarqueReturns(outcomes, now, 60)
	if global.Count != 10 || global.CostPence != 100_000 || global.PrizePence != 0 {
		t.Fatalf("global EV retained an eleventh older expiry: %+v", global)
	}
}

func TestEvaluateMarqueBackoffAllowsOneProbeAtExactDeadline(t *testing.T) {
	t.Parallel()
	until := time.Date(2026, 11, 2, 12, 0, 0, 0, time.UTC)
	input := MarqueInput{
		Quote: MarqueQuote{SeaZoneID: "zone", CostPence: 10_000, ExpectedNetPrizePence: 20_000,
			LadenPassagesPerDay: 2, AddedPiracyBPS: 500},
		LiquidPence: 1_000_000, Now: until.Add(-time.Nanosecond),
		Backoffs:      map[string]time.Time{"zone": until},
		BackoffBasis:  map[string]marqueBackoffBasis{"zone": {Returns: marqueReturns{Count: 3, CostPence: 30_000, PrizePence: 0}, Until: until}},
		ZoneReturns:   map[string]marqueReturns{"zone": {Count: 3, CostPence: 30_000, PrizePence: 0}},
		GlobalReturns: marqueReturns{Count: 3, CostPence: 30_000, PrizePence: 0},
	}
	if got := EvaluateMarque(input); got.Accepted {
		t.Fatal("a probe entered before the backoff expired")
	}
	input.Now = until
	if got := EvaluateMarque(input); !got.Accepted {
		t.Fatalf("a qualifying single probe did not enter at the exact deadline: %+v", got)
	}
	input.InForceCount = 1
	if got := EvaluateMarque(input); got.Accepted {
		t.Fatal("an active probe admitted a second issue")
	}
	input.InForceCount, input.Stopped = 0, true
	if got := EvaluateMarque(input); got.Accepted {
		t.Fatal("a probe bypassed the manual owner stop")
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
