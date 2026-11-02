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
	world, err := newVariantWorld(tape, RunOptions{From: from, To: to, CompanyID: "ours"}, Variant{Name: "baseline"}, view)
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

func TestPlanningInputRecomputesHomeUpkeepWhenFactorsLeave(t *testing.T) {
	tape, from, to := simAcceptanceRunTape(t)
	view, err := NewTapeReader(tape.Frames).Advance(from)
	if err != nil {
		t.Fatal(err)
	}
	view.State.Factors = []string{"one", "two", "three", "four"}
	world, err := newVariantWorld(tape, RunOptions{From: from, To: to, CompanyID: "ours"}, Variant{Name: "baseline"}, view)
	if err != nil {
		t.Fatal(err)
	}
	world.market.Observe(view)
	ships := world.fleet.Ships()
	before, err := world.planningInput(view, ships[0])
	if err != nil {
		t.Fatal(err)
	}
	var homeRegion string
	for _, port := range view.State.Catalog.Ports {
		if port.ID == view.State.HomePortID {
			homeRegion = port.BankingRegion
		}
	}
	if homeRegion == "" {
		t.Fatal("fixture lacks a home billing region")
	}
	beforeUpkeep := engine.DailyUpkeep(ships, view.State.Catalog, len(view.State.Factors))
	view.State.Factors = nil
	after, err := world.planningInput(view, ships[0])
	if err != nil {
		t.Fatal(err)
	}
	wantReleased := 3 * (beforeUpkeep - engine.DailyUpkeep(ships, view.State.Catalog, 0))
	if released := before.ReserveByRegion[homeRegion] - after.ReserveByRegion[homeRegion]; released != wantReleased || released <= 0 {
		t.Fatalf("simulation retained obsolete home upkeep: released %d, want %d", released, wantReleased)
	}
}
