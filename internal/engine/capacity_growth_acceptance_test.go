package engine

import (
	"math"
	"strings"
	"testing"
)

func capacityGrowthAcceptanceInput() CapacityGrowthInput {
	return CapacityGrowthInput{
		Hull:                         Hull{ID: "hoy", HoldCapacityTons: 40, MaxCannons: 4},
		HullPricePence:               2_486_427,
		InitialStoresPence:           50_000,
		SurplusPence:                 11_300_000,
		CandidateRatePencePerGameDay: 1_733_800.0 / 60,
		FleetSize:                    5,
		LotCashPence:                 480_000,
		IncrementalDailyUpkeepPence:  1_192,
		ReservePence:                 100_000,
		MaxPaybackDays:               420,
		HasSpareWork:                 true,
	}
}

func TestEvaluateCapacityGrowthLiveShaped(t *testing.T) {
	t.Parallel()
	in := capacityGrowthAcceptanceInput()
	got := EvaluateCapacityGrowth(in)
	if !got.Accepted {
		t.Fatalf("live-shaped affordable hoy rejected: %+v", got)
	}
	wantNet := in.CandidateRatePencePerGameDay - float64(in.IncrementalDailyUpkeepPence)
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
		{"upkeep consumes candidate rate", 3, func(in *CapacityGrowthInput) { in.IncrementalDailyUpkeepPence = 28_897 }},
		{"nonfinite candidate rate", 3, func(in *CapacityGrowthInput) { in.CandidateRatePencePerGameDay = math.Inf(1) }},
		{"disabled payback limit", 4, func(in *CapacityGrowthInput) { in.MaxPaybackDays = 0 }},
		{"payback exceeds limit", 4, func(in *CapacityGrowthInput) { in.MaxPaybackDays = 100 }},
		{"insufficient surplus", 5, func(in *CapacityGrowthInput) { in.SurplusPence = 3_000_000 }},
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

func TestEvaluateCapacityGrowthOneShipWithoutHistory(t *testing.T) {
	t.Parallel()
	in := capacityGrowthAcceptanceInput()
	in.FleetSize = 1
	in.SurplusPence = 11_000_000
	in.CandidateRatePencePerGameDay = 10_000
	got := EvaluateCapacityGrowth(in)
	if !got.Accepted || got.PaybackGameDays > 420 {
		t.Fatalf("one-ship fleet without earnings history rejected: %+v", got)
	}
	capital := growthCashTotal(got.QuotedHullPence, got.SlippageReservePence, got.CannonsPence, got.InitialStoresPence)
	in.CandidateRatePencePerGameDay = float64(capital)/421 + float64(in.IncrementalDailyUpkeepPence)
	if slow := EvaluateCapacityGrowth(in); slow.Accepted {
		t.Fatalf("payback beyond 420 game days admitted: %+v", slow)
	}
	in.CandidateRatePencePerGameDay = 10_000
	in.LotCashPence = 3_000_000
	buffer := max(in.ReservePence, ceilBPS(in.HullPricePence, 1_500)) + int64(in.FleetSize+1)*in.LotCashPence
	in.SurplusPence = capital + buffer - 1
	if short := EvaluateCapacityGrowth(in); short.Accepted || !strings.Contains(short.Reason, "cash lot") {
		t.Fatalf("build spent protected working cash: %+v", short)
	}
	in.SurplusPence++
	if exact := EvaluateCapacityGrowth(in); !exact.Accepted || exact.WorkingCapitalPence != buffer {
		t.Fatalf("exactly funded build rejected: %+v", exact)
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
