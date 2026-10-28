package engine

import (
	"fmt"
	"testing"
	"time"

	"github.com/DevYukine/go-tradewinds/internal/store"
)

func TestSourcingRollingAllowanceBoundaries(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name                     string
		net, expense, free, want int64
		unknown                  bool
	}{
		{"last penny", 0, 19_999, 5_000_000, 1, false},
		{"exhausted", 0, 20_000, 5_000_000, 0, false},
		{"half converted net", 100_000, 30_000, 5_000_000, 40_000, false},
		{"liquidity cap", 100_000, 30_000, 500_000, 10_000, false},
		{"negative odd net floors", -1, 0, 5_000_000, 19_999, false},
		{"conversion losses", -40_001, 0, 5_000_000, 0, false},
		{"unknown probe", 100_000, 0, 5_000_000, 0, true},
		{"money audit 72 hours", 2_304_400, 401_000, 100_000_000, 771_200, false},
		{"money audit 24 hour estimate", 768_133, 133_666, 100_000_000, 270_400, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			history := sourcingProbeHistory{ConversionNetPence: tc.net, ProbeExpensePence: tc.expense, ProbeCostsUnknown: tc.unknown}
			budget := history.rollingAllowance()
			in := PlanningInput{Balances: map[string]int64{"north": tc.free}, SourcingAllowancePence: &budget, SourcingProbeCostsUnknown: tc.unknown}
			for _, idle := range []bool{false, true} {
				in.SourcingIdleEligible = idle
				if got := sourcingAllowance(in); got != tc.want {
					t.Fatalf("idle=%v budget=%dp, want %dp", idle, got, tc.want)
				}
			}
			if tc.name == "exhausted" && sourcingBudgetReason(in, 1, 0) != "rolling exploration budget exhausted" {
				t.Fatal("exhaustion did not report the rolling budget policy")
			}
		})
	}
}

func TestSourcingRollingBudgetWindowAndScoutExclusion(t *testing.T) {
	t.Parallel()
	now := time.Date(2026, 10, 28, 12, 0, 0, 0, time.UTC)
	window := 24 * time.Hour // 60 game days at the live 60x clock.
	for _, tc := range []struct {
		name    string
		age     time.Duration
		known   bool
		lane    string
		want    int64
		unknown bool
	}{
		{"at boundary", window, true, "source|demand|grain|charter", 19_000, false},
		{"one second outside", window + time.Second, true, "source|demand|grain|charter", 20_000, false},
		{"unknown in window", window, false, "source|demand|grain|charter", 0, true},
		{"unknown aged out", window + time.Second, false, "source|demand|grain|charter", 20_000, false},
		{"scout cost excluded", time.Hour, true, "source|demand|grain|scout", 20_000, false},
		{"unknown scout excluded", time.Hour, false, "source|demand|grain|scout", 20_000, false},
		{"future excluded", -time.Second, false, "source|demand|grain|charter", 20_000, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			history := probeHistoryFixture()
			row := store.Settlement{Kind: "relocation", Lane: tc.lane, CostsKnown: tc.known, ExpensesPence: 1_000, CompletedAt: now.Add(-tc.age)}
			if err := history.rollingBudget([]store.Settlement{row}, now, 60); err != nil {
				t.Fatal(err)
			}
			if history.rollingAllowance() != tc.want || history.ProbeCostsUnknown != tc.unknown {
				t.Fatalf("unexpected rolling history: %+v", history)
			}
		})
	}
}

func TestSourcingRollingBudgetConversionAttributionAndLedgerBound(t *testing.T) {
	t.Parallel()
	now := time.Date(2026, 10, 28, 12, 0, 0, 0, time.UTC)
	probeAt := now.Add(-8 * 24 * time.Hour)
	probe := store.Settlement{Kind: "relocation", Origin: "home", Destination: "source", Commodity: "grain", CompletedAt: probeAt, CostsKnown: true}
	start := probeAt.Add(sourcingConversionWindow)
	for _, tc := range []struct {
		name   string
		change func(*store.Settlement)
		want   int64
	}{
		{"seven day boundary", func(*store.Settlement) {}, 100_000},
		{"one second after seven days", func(r *store.Settlement) { r.StartedAt = start.Add(time.Second) }, 0},
		{"delivery started before probe", func(r *store.Settlement) { r.StartedAt = probeAt.Add(-time.Second) }, 0},
		{"wrong source", func(r *store.Settlement) { r.Origin = "other" }, 0},
		{"other delivered commodity", func(r *store.Settlement) { r.Commodity = "canvas" }, 100_000},
		{"unknown costs", func(r *store.Settlement) { r.CostsKnown = false }, 0},
		{"no delivered tons", func(r *store.Settlement) { r.QuantityTons = 0 }, 0},
		{"not delivery", func(r *store.Settlement) { r.Kind = "aborted" }, 0},
		{"aged delivery", func(r *store.Settlement) { r.CompletedAt = now.Add(-24*time.Hour - time.Second) }, 0},
		{"future delivery", func(r *store.Settlement) { r.CompletedAt = now.Add(time.Second) }, 0},
		{"known net loss", func(r *store.Settlement) { r.ProceedsPence = 0 }, -40_000},
	} {
		t.Run(tc.name, func(t *testing.T) {
			row := store.Settlement{ID: "conversion", Kind: "charter", Origin: "source", Commodity: "grain", QuantityTons: 10, CostsKnown: true,
				StartedAt: start, CompletedAt: now.Add(-time.Hour), ProceedsPence: 140_000, AcquisitionPence: 30_000, FeesPence: 2_000, ProvisionsPence: 3_000, ExpensesPence: 5_000}
			tc.change(&row)
			history := probeHistoryFixture()
			// Deliberately reverse accounting order and add a newer probe after
			// the delivery started. Neither can hide the eligible earlier probe.
			later := probe
			later.CompletedAt = row.StartedAt.Add(time.Second)
			sameTime := probe
			sameTime.Commodity = "canvas"
			if err := history.rollingBudget([]store.Settlement{row, later, probe, sameTime}, now, 60); err != nil {
				t.Fatal(err)
			}
			if history.ConversionNetPence != tc.want {
				t.Fatalf("conversion net=%dp, want %dp", history.ConversionNetPence, tc.want)
			}
		})
	}
	rows := []store.Settlement{probe}
	for i := range 205 {
		rows = append(rows, store.Settlement{ID: fmt.Sprintf("delivery-%03d", i), Kind: "spot", Origin: "source", Commodity: "grain", QuantityTons: 1,
			CostsKnown: true, StartedAt: start, CompletedAt: now.Add(-time.Hour).Add(time.Duration(i) * time.Second), ProceedsPence: 100})
	}
	history := probeHistoryFixture()
	if err := history.rollingBudget(rows, now, 60); err != nil {
		t.Fatal(err)
	}
	if len(history.Conversions) != 200 || history.ConversionNetPence != 20_500 || history.Conversions[0].SettlementID != "delivery-204" || history.Conversions[199].SettlementID != "delivery-005" {
		t.Fatalf("capping diagnostic rows changed money evidence or ordering: %+v", history)
	}
}
