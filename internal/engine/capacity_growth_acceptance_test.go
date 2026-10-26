package engine

import (
	"math"
	"strings"
	"testing"
)

func capacityGrowthAcceptanceInput() CapacityGrowthInput {
	return CapacityGrowthInput{
		Hull:                        Hull{ID: "hoy", HoldCapacityTons: 40, MaxCannons: 4},
		HullPricePence:              2_486_427,
		InitialStoresPence:          50_000,
		SurplusPence:                11_300_000,
		AllTimeNetPence:             2_917_222,
		WindowNetPence:              1_733_800,
		ActiveShipGameDays:          60,
		FleetSize:                   5,
		SmallFleetTarget:            5,
		LotCashPence:                480_000,
		IncrementalDailyUpkeepPence: 1_192,
		ReservePence:                100_000,
		MaxPaybackDays:              180,
		HasSpareWork:                true,
		NewestShipProven:            true,
	}
}

func TestEvaluateCapacityGrowthLiveShaped(t *testing.T) {
	t.Parallel()
	in := capacityGrowthAcceptanceInput()
	got := EvaluateCapacityGrowth(in)
	if !got.Accepted {
		t.Fatalf("live-shaped affordable hoy rejected: %+v", got)
	}
	wantNet := float64(in.WindowNetPence)/in.ActiveShipGameDays - float64(in.IncrementalDailyUpkeepPence)
	if math.IsNaN(got.IncrementalNetPencePerGameDay) || math.Abs(got.IncrementalNetPencePerGameDay-wantNet) > 1 {
		t.Fatalf("net/day = %v, want within 1 penny of %v", got.IncrementalNetPencePerGameDay, wantNet)
	}
	if !(got.PaybackGameDays > 100 && got.PaybackGameDays < 105) {
		t.Fatalf("payback = %v game days, want (100,105)", got.PaybackGameDays)
	}
	wantSlippage := (in.HullPricePence*1_000 + 9_999) / 10_000
	wantCannons := min(int64(4), in.Hull.MaxCannons) * 15_000
	wantCapital := in.HullPricePence + wantSlippage + wantCannons + in.InitialStoresPence
	wantBuffer := max(in.ReservePence, (in.HullPricePence*1_500+9_999)/10_000) + int64(in.FleetSize+1)*in.LotCashPence
	if got.QuotedHullPence != in.HullPricePence || got.SlippageReservePence != wantSlippage || got.CannonsPence != wantCannons || got.InitialStoresPence != in.InitialStoresPence || got.WorkingCapitalPence != wantBuffer || got.IncrementalUpkeepPence != in.IncrementalDailyUpkeepPence {
		t.Fatalf("growth capital/working cash breakdown = %+v", got)
	}
	if math.Abs(got.PaybackGameDays-float64(wantCapital)/wantNet) > 0.01 {
		t.Fatalf("payback %v does not use hull, slippage, cannons and stores capital %d", got.PaybackGameDays, wantCapital)
	}
}

func TestEvaluateCapacityGrowthRejectRules(t *testing.T) {
	t.Parallel()
	// Rule 6 is the scaling calculation, not a rejection gate. Each other
	// rule has an isolated failure; alternatives within one gate may share
	// its reason, but different gates must explain different failures.
	cases := []struct {
		name   string
		rule   int
		change func(*CapacityGrowthInput)
	}{
		{"dispatch hull", 1, func(in *CapacityGrowthInput) { in.Hull.CanSealDispatches = true }},
		{"zero hull price", 1, func(in *CapacityGrowthInput) { in.HullPricePence = 0 }},
		{"negative hull price", 1, func(in *CapacityGrowthInput) { in.HullPricePence = -1 }},
		{"no cannon capacity", 1, func(in *CapacityGrowthInput) { in.Hull.MaxCannons = 0 }},
		{"hold consumed by guns", 1, func(in *CapacityGrowthInput) { in.Hull.HoldCapacityTons = 8 }},
		{"no spare work", 2, func(in *CapacityGrowthInput) { in.HasSpareWork = false }},
		{"newest ship unproven", 3, func(in *CapacityGrowthInput) { in.NewestShipProven = false }},
		{"no all-time realized profit", 4, func(in *CapacityGrowthInput) { in.AllTimeNetPence = 0 }},
		{"insufficient active game days", 5, func(in *CapacityGrowthInput) { in.ActiveShipGameDays = 0.99 }},
		{"nonpositive window net", 5, func(in *CapacityGrowthInput) { in.WindowNetPence = 0 }},
		{"upkeep consumes observed rate", 7, func(in *CapacityGrowthInput) { in.IncrementalDailyUpkeepPence = 28_897 }},
		{"disabled payback limit", 8, func(in *CapacityGrowthInput) { in.MaxPaybackDays = 0 }},
		{"payback exceeds limit", 8, func(in *CapacityGrowthInput) { in.MaxPaybackDays = 100 }},
		{"insufficient surplus", 9, func(in *CapacityGrowthInput) { in.SurplusPence = 3_000_000 }},
	}
	reasonRules := make(map[string]int)
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			in := capacityGrowthAcceptanceInput()
			tc.change(&in)
			got := EvaluateCapacityGrowth(in)
			if got.Accepted || strings.TrimSpace(got.Reason) == "" {
				t.Fatalf("rule %d must reject with a reason: %+v", tc.rule, got)
			}
			if rule, exists := reasonRules[got.Reason]; exists && rule != tc.rule {
				t.Errorf("rules %d and %d share the same non-specific reason %q", rule, tc.rule, got.Reason)
			}
			reasonRules[got.Reason] = tc.rule
		})
	}
}

func TestEvaluateCapacityGrowthCapacityScaling(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name        string
		fleetSize   int
		fillRatio   float64
		averageHold float64
		hullTons    int64
		multiplier  float64
	}{
		{"full established fleet caps at double rate", 5, 0.9, 40, 160, 2},
		{"underfilled fleet is unscaled", 5, 0.79, 40, 160, 1},
		{"fill threshold permits scaling", 5, 0.8, 40, 160, 2},
		{"missing average hold is unscaled", 5, 0.9, 0, 160, 1},
		{"smaller hull scaling floors at half rate", 5, 0.9, 40, 10, 0.5},
	} {
		t.Run(tc.name, func(t *testing.T) {
			in := capacityGrowthAcceptanceInput()
			in.FleetSize = tc.fleetSize
			in.FleetFillRatio = tc.fillRatio
			in.FleetAverageHoldTons = tc.averageHold
			in.Hull.HoldCapacityTons = tc.hullTons
			// Keep the lower-clamp case affordable under the same payback gate.
			in.MaxPaybackDays = 250
			got := EvaluateCapacityGrowth(in)
			if !got.Accepted {
				t.Fatalf("scaling fixture rejected: %+v", got)
			}
			wantNet := float64(in.WindowNetPence)/in.ActiveShipGameDays*tc.multiplier - float64(in.IncrementalDailyUpkeepPence)
			if math.IsNaN(got.IncrementalNetPencePerGameDay) || math.Abs(got.IncrementalNetPencePerGameDay-wantNet) > 1 {
				t.Fatalf("scaled net/day = %v, want %v (rate multiplier %v)", got.IncrementalNetPencePerGameDay, wantNet, tc.multiplier)
			}
		})
	}
}

func TestEvaluateCapacityGrowthFillsFleetBelowTarget(t *testing.T) {
	t.Parallel()
	in := capacityGrowthAcceptanceInput()
	in.FleetSize = 1
	// Below the target no earnings, proof or payback gate applies.
	in.NewestShipProven = false
	in.AllTimeNetPence = -1
	in.WindowNetPence = -1
	in.ActiveShipGameDays = 0
	in.MaxPaybackDays = 0
	got := EvaluateCapacityGrowth(in)
	if !got.Accepted {
		t.Fatalf("fleet below target was not filled: %+v", got)
	}
	idle := in
	idle.HasSpareWork = false
	if got := EvaluateCapacityGrowth(idle); got.Accepted {
		t.Fatalf("fleet fill bought a hull with no profitable first voyage: %+v", got)
	}
	wantBuffer := max(in.ReservePence, (in.HullPricePence*1_500+9_999)/10_000) + int64(in.FleetSize+1)*in.LotCashPence
	if got.WorkingCapitalPence != wantBuffer {
		t.Fatalf("fill mode working cash = %d, want %d", got.WorkingCapitalPence, wantBuffer)
	}
	capital := got.QuotedHullPence + got.SlippageReservePence + got.CannonsPence + got.InitialStoresPence
	in.SurplusPence = 3*capital - 1
	if over := EvaluateCapacityGrowth(in); over.Accepted {
		t.Fatalf("fill mode spent more than a third of surplus on one hull: %+v", over)
	}
	in.SurplusPence = 3 * capital
	if third := EvaluateCapacityGrowth(in); !third.Accepted {
		t.Fatalf("fill mode rejected a hull costing exactly a third of surplus: %+v", third)
	}
	// Large working-cash lots make the trading floor, not the spend cap, bind.
	in.LotCashPence = 3_000_000
	wantBuffer = max(in.ReservePence, (in.HullPricePence*1_500+9_999)/10_000) + int64(in.FleetSize+1)*in.LotCashPence
	in.SurplusPence = capital + wantBuffer - 1
	if short := EvaluateCapacityGrowth(in); short.Accepted {
		t.Fatalf("fill mode spent reserves or working cash: %+v", short)
	}
	in.SurplusPence = capital + wantBuffer
	if exact := EvaluateCapacityGrowth(in); !exact.Accepted {
		t.Fatalf("fill mode rejected exactly funded hull: %+v", exact)
	}
	cheaper := in
	cheaper.HullPricePence = in.HullPricePence / 2
	if !betterCapacityCandidate(EvaluateCapacityGrowth(cheaper), EvaluateCapacityGrowth(in)) {
		t.Fatal("fill mode did not prefer the cheaper hull")
	}
	in.FleetSize = in.SmallFleetTarget
	if full := EvaluateCapacityGrowth(in); full.Accepted {
		t.Fatalf("earnings gates did not return at the fleet target: %+v", full)
	}
}

func TestBetterCapacityCandidateOrdering(t *testing.T) {
	t.Parallel()
	other := GrowthDecision{
		Accepted:                      true,
		QuotedHullPence:               100_000,
		SlippageReservePence:          10_000,
		CannonsPence:                  30_000,
		InitialStoresPence:            10_000,
		IncrementalNetPencePerGameDay: 1_500,
	}
	for _, tc := range []struct {
		name   string
		change func(*GrowthDecision)
		better bool
	}{
		{"higher return on capital", func(d *GrowthDecision) { d.IncrementalNetPencePerGameDay = 1_600 }, true},
		{"higher daily net but worse return", func(d *GrowthDecision) {
			d.QuotedHullPence = 200_000
			d.IncrementalNetPencePerGameDay = 2_000
		}, false},
		{"equal return prefers lower capital", func(d *GrowthDecision) {
			d.QuotedHullPence = 50_000
			d.IncrementalNetPencePerGameDay = 1_000
		}, true},
		{"stores are capital", func(d *GrowthDecision) { d.InitialStoresPence += 100_000 }, false},
		{"slippage is capital", func(d *GrowthDecision) { d.SlippageReservePence += 100_000 }, false},
		{"cannons are capital", func(d *GrowthDecision) { d.CannonsPence += 100_000 }, false},
		{"exact tie is not better", func(d *GrowthDecision) {}, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			candidate := other
			tc.change(&candidate)
			if got := betterCapacityCandidate(candidate, other); got != tc.better {
				t.Fatalf("betterCapacityCandidate = %t, want %t", got, tc.better)
			}
			if tc.name != "exact tie is not better" && betterCapacityCandidate(other, candidate) == tc.better {
				t.Fatal("candidate ordering is not asymmetric")
			}
			if tc.name == "exact tie is not better" && betterCapacityCandidate(other, candidate) {
				t.Fatal("exact tie won in reverse order")
			}
		})
	}
}
