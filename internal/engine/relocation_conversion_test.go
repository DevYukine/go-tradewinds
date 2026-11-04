package engine

import (
	"testing"
	"time"

	"github.com/DevYukine/go-tradewinds/internal/store"
)

// planner.md D6: a commercial voyage credits the exploration budget only when
// the probing ship itself made the voyage, only on its first voyage from the
// probed port after that probe, and only when it hands off the probed charter.
func TestProbeConversionCreditsOnlyTheProbingShip(t *testing.T) {
	t.Parallel()
	now := time.Date(2026, 10, 23, 8, 0, 0, 0, time.UTC)
	probe := store.Settlement{ID: "probe-move", Kind: "relocation", ShipID: "ship-a", Origin: "le_havre",
		Destination: "amsterdam", Commodity: "sawn_planks", CostsKnown: true, ExpensesPence: 1_000,
		CompletedAt:  now.Add(-2 * time.Hour),
		ProbeOutcome: store.ProbeOutcomeFailed, Lane: "amsterdam|portsmouth|sawn_planks|probe:0:3000"}
	handoff := store.Settlement{ID: "probe-handoff", Kind: "charter", ShipID: "ship-a", Origin: "amsterdam",
		Destination: "portsmouth", Commodity: "sawn_planks", CostsKnown: true, QuantityTons: 10, GameSeconds: 3_600,
		ProceedsPence: 30_000, AcquisitionPence: 20_000, FeesPence: 2_000, ProvisionsPence: 1_000, ExpensesPence: 1_000,
		StartedAt: now.Add(-time.Hour), CompletedAt: now.Add(-45 * time.Minute)}
	second := store.Settlement{ID: "probe-second", Kind: "spot", ShipID: "ship-a", Origin: "amsterdam",
		Destination: "portsmouth", Commodity: "sawn_planks", CostsKnown: true, QuantityTons: 10, GameSeconds: 1_800,
		ProceedsPence: 10_000, AcquisitionPence: 5_000,
		StartedAt: now.Add(-40 * time.Minute), CompletedAt: now.Add(-30 * time.Minute)}
	other := store.Settlement{ID: "probe-other", Kind: "spot", ShipID: "ship-b", Origin: "amsterdam",
		Destination: "portsmouth", Commodity: "sawn_planks", CostsKnown: true, QuantityTons: 10, GameSeconds: 1_800,
		ProceedsPence: 45_000, AcquisitionPence: 5_000,
		StartedAt: now.Add(-35 * time.Minute), CompletedAt: now.Add(-20 * time.Minute)}
	var history sourcingProbeHistory
	if err := history.rollingBudget([]store.Settlement{probe, handoff, second, other}, now, 60); err != nil {
		t.Fatal(err)
	}
	if len(history.Conversions) != 1 || history.Conversions[0].SettlementID != handoff.ID {
		t.Fatalf("only the probing ship's first charter handoff after its probe may credit the budget: %+v", history.Conversions)
	}
	if history.ConversionNetPence != 6_000 || history.rollingAllowance() != 22_000 {
		t.Fatalf("unrelated commerce replenished the exploration budget: net=%dp allowance=%dp want 6,000p/22,000p",
			history.ConversionNetPence, history.rollingAllowance())
	}
	if history.ScoutConversionNetPence != 0 {
		t.Fatalf("non-scout probes leaked into scout conversion accounting: %d", history.ScoutConversionNetPence)
	}
}

// planner.md D7: a realized lane finances an empty relocation only once it has
// at least two recent loss-inclusive settlements; one lucky receipt does not.
func TestRealizedRelocationNeedsTwoRecentSettlements(t *testing.T) {
	t.Parallel()
	in, _ := realizedRelocationInput(t)
	profit := store.Settlement{Kind: "spot", CostsKnown: true, Origin: "amsterdam", Destination: "portsmouth",
		Commodity: "sawn_planks", QuantityTons: 12, GameSeconds: 10_800, ProceedsPence: 26_000, AcquisitionPence: 4_000,
		StartedAt: in.Now.Add(-2 * time.Hour), CompletedAt: in.Now.Add(-time.Hour)}
	loss := profit
	loss.ProceedsPence = 2_000
	loss.StartedAt, loss.CompletedAt = in.Now.Add(-90*time.Minute), in.Now.Add(-30*time.Minute)
	for _, tc := range []struct {
		name         string
		rows         []store.Settlement
		wantLanes    int
		wantAccepted int
	}{
		{"one settled receipt does not fund a relocation", []store.Settlement{profit}, 0, 0},
		{"two loss-inclusive settlements fund one relocation", []store.Settlement{profit, loss}, 1, 1},
	} {
		t.Run(tc.name, func(t *testing.T) {
			lanes, err := recentRealizedSourcingLanes(tc.rows, in.Now)
			if err != nil {
				t.Fatal(err)
			}
			if len(lanes) != tc.wantLanes {
				t.Fatalf("realized lanes from %d settlement(s) = %+v, want %d", len(tc.rows), lanes, tc.wantLanes)
			}
			in.SourcingProbeHistory.RealizedLanes = lanes
			accepted := acceptedRelocations(realizedRelocationPlans(in, nil))
			if len(accepted) != tc.wantAccepted {
				t.Fatalf("realized-lane relocations admitted from %d settlement(s): %+v, want %d",
					len(tc.rows), accepted, tc.wantAccepted)
			}
		})
	}
}
