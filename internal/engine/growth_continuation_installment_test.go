package engine

import (
	"testing"
	"time"
)

// continuationInstallmentFixture models the continuation repricing world: a
// hoy berthed at portsmouth after its first settlement, a profitable open
// charter delivering back at portsmouth, and executable sawn_planks asks at
// amsterdam, so the next lot is the round trip portsmouth->amsterdam->
// portsmouth. The lane prices exactly one funded lot: delivery arrivals grow
// with tonnage, and the charter deadline boundary below treats the control
// arrival as the earliest admissible one.
func continuationInstallmentFixture(t *testing.T) (PlanningInput, Plan, *growthLaneIntent, growthReturnQuote) {
	t.Helper()
	input := sourcingInputFixture(t)
	// The settlement was delivered at portsmouth: the empty repositioning leg
	// to the amsterdam source departs there, and the source inspection waits
	// in that same roadstead.
	input.Ship.Ship.CurrentPortID = "portsmouth"
	seedZeroHazardQuotes(&input)
	// quoteGrowthDelivery prices the amsterdam->portsmouth delivery lane with
	// observed owned-hull evidence; the settlement round trip already quoted it.
	forward := input.HazardQuotes[hazardHullKey(input.Ship.Ship.ID, "portsmouth", "amsterdam")]
	forward.Origin, forward.Destination = "amsterdam", "portsmouth"
	input.HazardQuotes[hazardHullKey(input.Ship.Ship.ID, "amsterdam", "portsmouth")] = forward
	input.Metadata["rates"] = sourcingFreshMeta(input.Now)
	input.Balances = map[string]int64{"channel": 50_000_000}
	input.ReserveByRegion = map[string]int64{"channel": 10_000}
	base := Plan{Kind: "charter", ShipID: input.Ship.Ship.ID, Origin: "amsterdam", Destination: "portsmouth",
		Commodity: "sawn_planks", ContractID: "charter-planks"}
	lane := &growthLaneIntent{AdmissionID: "continuation-admission", QuantityLimit: 1}
	back, err := growthReturnBasis(input, base.Origin)
	if err != nil {
		t.Fatalf("growth return basis unavailable: %v", err)
	}
	return input, base, lane, back
}

func TestGrowthContinuationRoundTripArrivalGatesDeferredInstallment(t *testing.T) {
	t.Parallel()
	input, base, lane, back := continuationInstallmentFixture(t)
	control := quoteGrowthDelivery(input, base, lane, false, &back)
	if !control.plan.Accepted {
		t.Fatalf("continuation control was not admitted: %s", control.plan.Reason)
	}
	reserveFrom := control.plan.ExpectedArrival
	fullReserve := int64(48_402)
	input.DeferredLoanReserves = []loanReserveDeferral{{Region: "channel", FullReserve: fullReserve, ReserveFrom: reserveFrom}}

	// The forward leg alone returns home inside the reserve window; the
	// deployed round trip arrives exactly at ReserveFrom and must not.
	forward := control.plan
	forward.ExpectedArrival = reserveFrom.Add(-gameDuration(back.days, input.Clock.Calendar.TimeScale))
	if cargoFundingRequired(input, forward)["channel"] != cargoFundingRequired(input, control.plan)["channel"]-fullReserve {
		t.Fatalf("fixture missed the reserve boundary: forward-only %d, round trip %d",
			cargoFundingRequired(input, forward)["channel"], cargoFundingRequired(input, control.plan)["channel"])
	}
	quoted := quoteGrowthDelivery(input, base, lane, false, &back)
	if !quoted.plan.Accepted {
		t.Fatalf("continuation admission refused with restored installments: %s", quoted.plan.Reason)
	}
	if !quoted.plan.ExpectedArrival.Equal(reserveFrom) {
		t.Fatalf("quoted cash-return ETA did not carry the return leg exactly once: %s vs %s",
			quoted.plan.ExpectedArrival, reserveFrom)
	}
	if quoted.required["channel"] != control.required["channel"]+fullReserve {
		t.Fatalf("admission waived a deferred installment with a forward-only ETA: required=%d want=%d",
			quoted.required["channel"], control.required["channel"]+fullReserve)
	}
}

func TestGrowthContinuationCharterDeadlineCountsReturnExactlyOnce(t *testing.T) {
	t.Parallel()
	input, base, lane, back := continuationInstallmentFixture(t)
	control := quoteGrowthDelivery(input, base, lane, false, &back)
	if !control.plan.Accepted {
		t.Fatalf("continuation control was not admitted: %s", control.plan.Reason)
	}
	day := gameDuration(1, input.Clock.Calendar.TimeScale)
	arrival := control.plan.ExpectedArrival

	// Exactly one game day of slack after the complete round trip: the strict
	// boundary must refuse, one second more must admit.
	deadline := arrival.Add(day)
	input.Contracts[0].ExpiresAt = &deadline
	if quote := quoteGrowthDelivery(input, base, lane, false, &back); quote.plan.Accepted {
		t.Fatalf("round trip without one game day of deadline slack was admitted: %+v", quote.plan)
	}
	later := deadline.Add(time.Second)
	input.Contracts[0].ExpiresAt = &later
	quote := quoteGrowthDelivery(input, base, lane, false, &back)
	if !quote.plan.Accepted || !quote.plan.ExpectedArrival.Add(day).Before(*quote.plan.Deadline) {
		t.Fatalf("round trip with deadline slack was refused or mis-gated: %s arrival %s deadline %s",
			quote.plan.Reason, quote.plan.ExpectedArrival, quote.plan.Deadline)
	}
	if !quote.plan.ExpectedArrival.Equal(arrival) {
		t.Fatalf("deadline pricing moved the cash-return ETA: %s vs %s", quote.plan.ExpectedArrival, arrival)
	}
}

func TestGrowthSourceInspectionProtectsFullDeferrals(t *testing.T) {
	t.Parallel()
	input, _, _, _ := continuationInstallmentFixture(t)
	inspection := Plan{Kind: "spot", Origin: "portsmouth", Destination: "amsterdam"}
	input.Ship.Ship.Status = "anchored_roadstead"
	input.Metadata["factors"] = sourcingFreshMeta(input.Now)
	input.Metadata["profile"] = sourcingFreshMeta(input.Now)
	input.ReserveByRegion = map[string]int64{"channel": 0}
	input.DeferredLoanReserves = []loanReserveDeferral{{
		Region: "channel", FullReserve: 48_402, ReserveFrom: input.Now.Add(time.Hour),
	}}
	bill := input.Ports[inspection.Origin].BerthDuesPerDayPence / 4
	input.Balances = map[string]int64{"channel": bill + 48_402 - 1}
	if err := growthSourceInspectionReady(input, inspection); err == nil {
		t.Fatal("harbour inspection spent a fully deferred installment reserve")
	}
	input.Balances["channel"] = bill + 48_402
	if err := growthSourceInspectionReady(input, inspection); err != nil {
		t.Fatalf("fully funded harbour inspection was refused: %v", err)
	}
}
