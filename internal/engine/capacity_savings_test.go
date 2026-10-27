package engine

import (
	"math"
	"testing"
)

func TestCapacitySavingsMargin(t *testing.T) {
	t.Parallel()
	buy := GrowthDecision{
		Accepted: true, EconomicallyViable: true,
		QuotedHullPence: 100, SlippageReservePence: 10, CannonsPence: 5, InitialStoresPence: 5,
		IncrementalNetPencePerGameDay: 10,
	}
	save := GrowthDecision{
		EconomicallyViable: true, QuotedHullPence: 200,
		CashShortfallPence: 10, DepthNetPencePerGameDay: 40,
		// Savings must rank B by its depth rate, not its ordinary earning rate.
		IncrementalNetPencePerGameDay: 1,
	}
	for _, tc := range []struct {
		name   string
		change func(*GrowthDecision, *GrowthDecision, *float64)
		lot    int64
		want   bool
	}{
		{name: "near better hull", want: true},
		{name: "includes another cargo lot", lot: 30, want: true},
		{name: "huge shortfall", change: func(_ *GrowthDecision, b *GrowthDecision, _ *float64) { b.CashShortfallPence = 100_000 }},
		{name: "A pays back before saving finishes", change: func(_ *GrowthDecision, b *GrowthDecision, _ *float64) { b.CashShortfallPence = 1_000 }},
		{name: "zero savings rate", change: func(_ *GrowthDecision, _ *GrowthDecision, r *float64) { *r = 0 }},
		{name: "negative savings rate", change: func(_ *GrowthDecision, _ *GrowthDecision, r *float64) { *r = -1 }},
		{name: "zero depth net", change: func(_ *GrowthDecision, b *GrowthDecision, _ *float64) { b.DepthNetPencePerGameDay = 0 }},
		{name: "negative depth net", change: func(_ *GrowthDecision, b *GrowthDecision, _ *float64) { b.DepthNetPencePerGameDay = -1 }},
		{name: "already affordable", change: func(_ *GrowthDecision, b *GrowthDecision, _ *float64) { b.CashShortfallPence = 0 }},
		{name: "negative shortfall", change: func(_ *GrowthDecision, b *GrowthDecision, _ *float64) { b.CashShortfallPence = -1 }},
		{name: "higher absolute net but lower return on capital", change: func(_ *GrowthDecision, b *GrowthDecision, _ *float64) { b.QuotedHullPence = 1_000 }},
		{name: "NaN savings rate", change: func(_ *GrowthDecision, _ *GrowthDecision, r *float64) { *r = math.NaN() }},
		{name: "infinite savings rate", change: func(_ *GrowthDecision, _ *GrowthDecision, r *float64) { *r = math.Inf(1) }},
		{name: "negative infinite savings rate", change: func(_ *GrowthDecision, _ *GrowthDecision, r *float64) { *r = math.Inf(-1) }},
		{name: "NaN buy net", change: func(a *GrowthDecision, _ *GrowthDecision, _ *float64) { a.IncrementalNetPencePerGameDay = math.NaN() }},
		{name: "infinite buy net", change: func(a *GrowthDecision, _ *GrowthDecision, _ *float64) { a.IncrementalNetPencePerGameDay = math.Inf(1) }},
		{name: "NaN depth net", change: func(_ *GrowthDecision, b *GrowthDecision, _ *float64) { b.DepthNetPencePerGameDay = math.NaN() }},
		{name: "infinite depth net", change: func(_ *GrowthDecision, b *GrowthDecision, _ *float64) { b.DepthNetPencePerGameDay = math.Inf(1) }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			a, b, rate := buy, save, float64(20)
			if tc.change != nil {
				tc.change(&a, &b, &rate)
			}
			wait, margin, ok := capacitySavingsMargin(a, b, rate, tc.lot)
			if ok != tc.want {
				t.Fatalf("save = %t, want %t (wait %v, margin %v)", ok, tc.want, wait, margin)
			}
			if tc.name != "higher absolute net but lower return on capital" && rate > 0 && !math.IsInf(rate, 0) && b.CashShortfallPence > 0 &&
				!math.IsNaN(a.IncrementalNetPencePerGameDay) && !math.IsInf(a.IncrementalNetPencePerGameDay, 0) &&
				b.DepthNetPencePerGameDay > 0 && !math.IsInf(b.DepthNetPencePerGameDay, 0) {
				wantWait := float64(b.CashShortfallPence) / rate
				capital := float64(120 + tc.lot)
				wantMargin := b.DepthNetPencePerGameDay*((float64(b.CashShortfallPence)+capital)/(rate+a.IncrementalNetPencePerGameDay)-wantWait) -
					a.IncrementalNetPencePerGameDay*(wantWait+capital/(rate+b.DepthNetPencePerGameDay))
				if math.Abs(wait-wantWait) > 1e-12 || math.Abs(margin-wantMargin) > 1e-9 {
					t.Fatalf("wait/margin = %v/%v, want %v/%v", wait, margin, wantWait, wantMargin)
				}
			}
		})
	}
}

func TestCapacitySavingsMarginStrictBoundary(t *testing.T) {
	t.Parallel()
	buy := GrowthDecision{Accepted: true, QuotedHullPence: 120, IncrementalNetPencePerGameDay: 10}
	save := GrowthDecision{EconomicallyViable: true, QuotedHullPence: 200, DepthNetPencePerGameDay: 20}
	for _, tc := range []struct {
		shortfall int64
		margin    float64
		want      bool
	}{
		{39, 2, true},
		{40, 0, false},
		{41, -2, false},
	} {
		save.CashShortfallPence = tc.shortfall
		wait, margin, ok := capacitySavingsMargin(buy, save, 10, 0)
		if math.Abs(margin-tc.margin) > 1e-12 || ok != tc.want || wait != float64(tc.shortfall)/10 {
			t.Fatalf("shortfall %d: wait/margin/save = %v/%v/%t, want %v/%v/%t", tc.shortfall, wait, margin, ok, float64(tc.shortfall)/10, tc.margin, tc.want)
		}
		if tc.shortfall == 40 && margin != 0 {
			t.Fatalf("exact equality fixture returned margin %v, want exactly zero", margin)
		}
	}
}

func TestCapacitySavingsMarginDecreasesWithShortfall(t *testing.T) {
	t.Parallel()
	buy := GrowthDecision{Accepted: true, QuotedHullPence: 120, IncrementalNetPencePerGameDay: 10}
	save := GrowthDecision{EconomicallyViable: true, QuotedHullPence: 200, DepthNetPencePerGameDay: 40}
	previous := math.Inf(1)
	for _, shortfall := range []int64{1, 10, 100, 1_000, 100_000} {
		save.CashShortfallPence = shortfall
		wait, margin, _ := capacitySavingsMargin(buy, save, 20, 30)
		if !(margin < previous) || wait != float64(shortfall)/20 {
			t.Fatalf("shortfall %d: margin %v must be below %v and wait %v must equal Sb/R", shortfall, margin, previous, wait)
		}
		previous = margin
	}
}
