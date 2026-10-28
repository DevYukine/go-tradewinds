package sim

import (
	"testing"

	"github.com/DevYukine/go-tradewinds/internal/engine"
)

func TestPlanningInputPreservesObservedQuayOpportunity(t *testing.T) {
	tape, from, to := simAcceptanceRunTape(t)
	view, err := NewTapeReader(tape.Frames).Advance(from)
	if err != nil {
		t.Fatal(err)
	}
	world, err := newVariantWorld(tape, RunOptions{From: from, To: to, ReservePence: 75_000, CompanyID: "ours"}, Variant{Name: "baseline"}, view)
	if err != nil {
		t.Fatal(err)
	}
	world.market.Observe(view)
	input, err := world.planningInput(view, world.fleet.Ships()[0])
	if err != nil {
		t.Fatal(err)
	}
	if plan, ok := world.bestOrdinary(input); !ok || plan.Kind != "spot" || plan.Quantity != 30 {
		t.Fatalf("variant input lost the observed quay opportunity: %+v", engine.PlanVoyages(input))
	}
}
