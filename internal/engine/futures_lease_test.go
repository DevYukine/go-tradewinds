package engine

import (
	"encoding/json"
	"testing"
	"time"

	"github.com/DevYukine/go-tradewinds/internal/gameapi"
	"github.com/DevYukine/go-tradewinds/internal/store"
)

func futuresLeaseCross(t *testing.T, baseline int64) (*recoveryHarness, *futuresMoneyAPI, *Runner, *store.Mission, missionData) {
	t.Helper()
	h, p := newFuturesMoneyAPI(t)
	p.quantity, p.leased = baseline, baseline
	runner := futuresMoneyRunner(t, h)
	refreshMarqueExecution(t, h, runner)
	futuresCounterpartyFixture(runner, "buyer")
	futuresCounterpartyFixture(runner, "seller")
	sale := futuresListingFixture(runner, "lease-sale", "buyer", "bid", 2000, 2)
	purchase := futuresListingFixture(runner, "lease-purchase", "seller", "ask", 500, 2)
	sale.MaturesAt = purchase.MaturesAt.Add(time.Minute)
	runner.snapshot["futures"] = encode([]gameapi.FuturesContract{sale, purchase})
	selected, err := runner.evaluateFuturesCross(h.ctx, sale, purchase, nil, 100_000)
	if err != nil || len(selected) != 2 {
		t.Fatalf("cross could not reserve receiving capacity: %+v %v", selected, err)
	}
	control := &store.Mission{AccountID: h.account.ID, ShipID: "__futures", Generation: "lease-controller", Phase: "evaluating"}
	data := missionData{Futures: &futuresState{LegacyClosed: true}}
	if changed, err := runner.stageFuturesAdmission(h.ctx, control, &data, selected); err != nil || !changed {
		t.Fatalf("cross did not stage exact capacity: %t %v", changed, err)
	}
	return h, p, runner, control, data
}

func TestFuturesLeaseIsJournaledBeforeCrossAndReturnsToUnrelatedBaseline(t *testing.T) {
	for _, baseline := range []int64{0, 2, 6} {
		t.Run(map[int64]string{0: "no opening stock or lease", 2: "occupied lease", 6: "preserves unrelated lease"}[baseline], func(t *testing.T) {
			h, p, runner, control, data := futuresLeaseCross(t, baseline)
			leaseKey := data.Action.ID
			if data.Action.Stage != "futures-resize-lease" || data.Futures.LeaseChange.TargetTons != baseline+2 ||
				data.Futures.LeaseChange.BaselineTons != baseline || p.leased != baseline || p.mutations != 0 {
				t.Fatalf("lease changed before journaling or sized speculative tons: %+v %+v", data.Action, data.Futures.LeaseChange)
			}
			rows, err := h.db.FuturesIntents(h.ctx, h.account.ID)
			if err != nil || len(rows) != 2 {
				t.Fatalf("lease lacked durable cross legs: %+v %v", rows, err)
			}
			for _, row := range rows {
				var admitted futuresAdmission
				if err := json.Unmarshal(row.Admission, &admitted); err != nil || admitted.Lease == nil {
					t.Fatalf("cross lost its exact receiving admission: %+v %v", admitted, err)
				}
				if row.Side == "bid" && (admitted.Lease.ReceivingTons != 2 || admitted.RentPence <= 0 || row.ReservedCashPence < row.MarginPence+admitted.RentPence) {
					t.Fatalf("purchase omitted receiving capacity or rent: %+v %+v", row, admitted)
				}
			}
			admissionKey := rows[0].ActionID
			if changed, err := runner.dispatch(h.ctx, control, &data); err != nil || !changed {
				t.Fatalf("journaled lease did not execute: %t %v", changed, err)
			}
			if p.leased != baseline+2 || data.Action != nil || data.Futures.LeaseChange != nil || data.Futures.Leases["amsterdam"].BaselineTons != baseline {
				t.Fatalf("confirmed lease lost its owned baseline: %+v", data.Futures)
			}
			refreshMarqueExecution(t, h, runner)
			if changed, err := runner.stageFuturesCapacity(h.ctx, control, &data, rows); err != nil || !changed {
				t.Fatalf("funded lease did not resume cross: %t %v", changed, err)
			}
			if data.Action.Stage != "futures-accept-batch" || data.Action.ID != admissionKey || data.Action.ID == leaseKey {
				t.Fatalf("lease replaced the immutable atomic admission: %+v", data.Action)
			}
			if _, err := runner.replanUnsentAction(h.ctx, control, &data, neverSentFuturesTerms); err != nil {
				t.Fatal(err)
			}
			rows, err = h.db.FuturesIntents(h.ctx, h.account.ID)
			if err != nil || rows[0].Status != "rejected" || rows[1].Status != "rejected" {
				t.Fatalf("no-send proof did not retire both legs: %+v %v", rows, err)
			}
			if changed, err := runner.releaseFuturesLease(h.ctx, control, &data, rows); err != nil || !changed || data.Futures.LeaseChange.TargetTons != baseline {
				t.Fatalf("unused receiving capacity kept charging rent: %t %v %+v", changed, err, data.Futures.LeaseChange)
			}
			data.CooldownUntil = time.Time{}
			if changed, err := runner.dispatch(h.ctx, control, &data); err != nil || !changed || p.leased != baseline || len(data.Futures.Leases) != 0 {
				t.Fatalf("journaled release lost unrelated stock or lease: %t %v %+v", changed, err, data.Futures)
			}
		})
	}
}

func TestFuturesLostCapacityRejectsUnsentCrossInsteadOfWedgingAdmissions(t *testing.T) {
	h, _, runner, control, data := futuresLeaseCross(t, 2)
	// Retire the capacity action before any HTTP request. Only the admission
	// remains, so a vanished port slot must release it rather than wait forever.
	data.Action, data.Futures.LeaseChange = nil, nil
	ports := decode[map[string]json.RawMessage](runner.snapshot, "ports")
	var detail map[string]any
	if err := json.Unmarshal(ports["amsterdam"], &detail); err != nil {
		t.Fatal(err)
	}
	detail["total_leased_warehouse"] = detail["total_warehouse_cap"]
	ports["amsterdam"] = encode(detail)
	runner.snapshot["ports"] = encode(ports)
	rows, err := h.db.FuturesIntents(h.ctx, h.account.ID)
	if err != nil {
		t.Fatal(err)
	}
	if changed, err := runner.stageFuturesCapacity(h.ctx, control, &data, rows); err != nil || !changed || data.Action != nil {
		t.Fatalf("vanished capacity retained a blocking intent: %t %v %+v", changed, err, data.Action)
	}
	rows, err = h.db.FuturesIntents(h.ctx, h.account.ID)
	if err != nil || rows[0].Status != "rejected" || rows[1].Status != "rejected" {
		t.Fatalf("capacity failure retained reservations: %+v %v", rows, err)
	}
}

func TestFuturesLeaseFloorPreservesLegacyStockAndEveryReceivingCommodity(t *testing.T) {
	h, p := newFuturesMoneyAPI(t)
	p.quantity, p.leased = 2, 6
	runner := futuresMoneyRunner(t, h)
	refreshMarqueExecution(t, h, runner)
	data := missionData{Futures: &futuresState{InitialLease: 1, Leases: map[string]futuresLeaseHolding{
		"amsterdam": {BaselineTons: 1, TargetTons: 6},
	}}}
	rows := []store.FuturesIntent{{PortID: "amsterdam", CommodityID: "grain", Side: "bid", Status: "intent", LotTons: 3}}
	if floor := runner.futuresLeaseFloor(&data, rows, "amsterdam"); floor != 5 {
		t.Fatalf("cleanup could shrink stock plus other receiving reservations: %d", floor)
	}
	rows[0].Status = "cancelled"
	if floor := runner.futuresLeaseFloor(&data, rows, "amsterdam"); floor != 2 {
		t.Fatalf("released receiving capacity remained rented: %d", floor)
	}
	p.quantity, p.leased = 0, 8
	refreshMarqueExecution(t, h, runner)
	if floor := runner.futuresLeaseFloor(&data, rows, "amsterdam"); floor != 3 {
		t.Fatalf("controller reclaimed another lease increase: %d", floor)
	}
}
