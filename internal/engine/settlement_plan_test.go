package engine

import (
	"encoding/json"
	"fmt"
	"math"
	"net/http"
	"testing"
	"time"
)

func TestSettlementPlanCopiesEconomicsAndCargoShortfallAcrossRestart(t *testing.T) {
	for _, sold := range []int64{22, 30} {
		t.Run(fmt.Sprintf("%d tons sold", sold), func(t *testing.T) {
			h := newRecoveryHarness(t)
			h.api.setFleet([]map[string]any{destinationShipRow(recoveryShipBlastoise, "Blastoise", recoveryFarPort, "anchored_roadstead", 3, nil, nil)})
			runner := refreshedRunner(t, h)
			ship := harnessShip(t, runner, recoveryShipBlastoise)
			data := missionData{
				Plan: Plan{Kind: "spot+freight", Origin: recoveryHomePort, Destination: recoveryFarPort, Commodity: recoveryGrain, Quantity: 30,
					Economics: Economics{Basis: "destination", RevenueBasis: "retained", RevenueAgeGameHours: new(24.5), RevenuePence: 24_000, ExpectedNetPence: 4_000}},
				BoughtQuantity: 30, BuyDone: true, AcquisitionPence: 18_000, CostsKnown: true,
				StartedAt: time.Now().UTC().Add(-time.Hour), StartedGameHours: runner.stateCache.Clock.Calendar.TotalGameHours - 1,
				InitialLedgerIDs: map[string]bool{}, FinishAtDestination: true,
				Freight: []freightObligation{{ContractID: "sealed-cargo", Commodity: recoveryGrain, Quantity: 5}},
			}
			data.ProvenLane = laneKey(data.Plan)
			data.Action = &durableAction{ID: "outbound", Stage: "depart", Path: "/v1/fleet/depart", Before: ship}
			data.Action.Before.CargoManifest = map[string]int64{recoveryGrain: 35}
			request := `{"ship_id":"` + ship.Ship.ID + `","dest_port_id":"` + recoveryFarPort + `"}`
			response := `{"ship_id":"` + ship.Ship.ID + `","dest_port_id":"` + recoveryFarPort + `","berth_dues_pence":0,"berth_dues_debt_pence":0}`
			data.Action.Body = json.RawMessage(request)
			destinationSeedAction(t, h, "outbound", "/v1/fleet/depart", request, response)
			h.putMission(ship.Ship.ID, "planned-settlement", "depart", data)
			mission, data := h.mission(ship.Ship.ID)
			if changed, err := runner.consumeResult(h.ctx, mission, &data, http.StatusOK, json.RawMessage(response)); err != nil || !changed {
				t.Fatalf("consume departure receipt: %t, %v", changed, err)
			}
			mission, data = h.mission(ship.Ship.ID)
			if data.DepartedCargoPence == nil || *data.DepartedCargoPence != 18_000 {
				t.Fatalf("outbound valuation included freight or lost purchased cargo: %+v", data.DepartedCargoPence)
			}
			data.DeliveredQuantity = sold + 5
			data.Freight[0].Settled = true
			data.ProceedsPence = sold * 800
			mission.Phase = "closing"
			if changed, err := runner.finishMission(h.ctx, ship, mission, &data); err != nil || !changed {
				t.Fatalf("persist immutable settlement intent: %t, %v", changed, err)
			}
			// Restart at the boundary between mission persistence and settlement insertion.
			mission, data = h.mission(ship.Ship.ID)
			frozenAt := data.Settlement.CompletedAt
			data.Plan.Economics.ExpectedNetPence = 999_999
			restarted := h.runner(primaryTestKey)
			if changed, err := restarted.finishMission(h.ctx, ship, mission, &data); err != nil || !changed {
				t.Fatalf("settlement restart: %t, %v", changed, err)
			}
			rows, err := h.db.SettlementHistory(h.ctx, h.account.ID, data.ProvenLane, 10)
			if err != nil || len(rows) != 1 {
				t.Fatalf("settlement history: %+v, %v", rows, err)
			}
			row := rows[0]
			if row.PlannedNetPence == nil || *row.PlannedNetPence != 4_000 || row.PlannedRevenuePence == nil || *row.PlannedRevenuePence != 24_000 || row.RevenueBasis == nil || *row.RevenueBasis != "retained" || row.RevenueAgeGameHours == nil || *row.RevenueAgeGameHours != 24.5 {
				t.Fatalf("planned economics changed across restart: %+v", row)
			}
			if row.HazardBPS != nil || row.HazardSource != nil {
				t.Fatalf("missing hazard authority became measured zero: %+v", row)
			}
			wantShortfall := (30 - sold) * 600
			if row.CargoShortfallPence == nil || *row.CargoShortfallPence != wantShortfall || row.DepartedCargoPence == nil || *row.DepartedCargoPence != 18_000 || !row.CompletedAt.Equal(frozenAt.Truncate(time.Microsecond)) {
				t.Fatalf("realized cargo shortfall or immutable timestamp changed: %+v", row)
			}
			if err := h.db.PutSettlement(h.ctx, data.Settlement); err != nil {
				t.Fatalf("replay after restart: %v", err)
			}
		})
	}
}

func TestSettlementPlanPreservesFirstDepartureThroughReturnAndReconciliation(t *testing.T) {
	for _, reconciled := range []bool{false, true} {
		t.Run(map[bool]string{false: "receipt", true: "reconciled"}[reconciled], func(t *testing.T) {
			h := newRecoveryHarness(t)
			runner := refreshedRunner(t, h)
			ship := harnessShip(t, runner, recoveryShipBlastoise)
			data := missionData{Plan: Plan{Kind: "spot", Origin: recoveryHomePort, Destination: recoveryFarPort, Commodity: recoveryGrain}, BoughtQuantity: 30, AcquisitionPence: 18_000, CostsKnown: true}
			data.Action = &durableAction{ID: "outbound", Stage: "depart", Before: ship}
			data.Action.Before.CargoManifest = map[string]int64{recoveryGrain: 30}
			h.putMission(ship.Ship.ID, "first-departure", "depart", data)
			mission, data := h.mission(ship.Ship.ID)
			if reconciled {
				if changed, err := runner.applyReconciliation(h.ctx, mission, &data); err != nil || !changed {
					t.Fatalf("reconcile first departure: %t, %v", changed, err)
				}
			} else {
				response := encode(map[string]any{"ship_id": ship.Ship.ID, "dest_port_id": mission.Destination, "berth_dues_pence": 0, "berth_dues_debt_pence": 0})
				if changed, err := runner.consumeResult(h.ctx, mission, &data, http.StatusOK, response); err != nil || !changed {
					t.Fatalf("first departure receipt: %t, %v", changed, err)
				}
			}
			mission, data = h.mission(ship.Ship.ID)
			data.ReturnJourney = true
			data.Action = &durableAction{ID: "return", Stage: "depart", Before: ship}
			data.Action.Before.CargoManifest = map[string]int64{}
			if changed, err := runner.applyReconciliation(h.ctx, mission, &data); err != nil || !changed {
				t.Fatalf("return departure reconciliation: %t, %v", changed, err)
			}
			_, restored := h.mission(ship.Ship.ID)
			if restored.DepartedCargoPence == nil || *restored.DepartedCargoPence != 18_000 {
				t.Fatalf("empty return overwrote outbound risk exposure: %+v", restored.DepartedCargoPence)
			}
		})
	}
}

func TestSettlementPlanCargoShortfallIntegerBoundaries(t *testing.T) {
	for _, test := range []struct {
		name                                 string
		bought, delivered, acquisition, want int64
	}{
		{"partial loss", 30, 22, 18_000, 4_800},
		{"no loss", 30, 30, 18_000, 0},
		{"fractional penny rounds up", 3, 2, 1, 1},
		{"maximum value", math.MaxInt64, 0, math.MaxInt64, math.MaxInt64},
		{"overdelivery cannot create negative loss", 30, 31, 18_000, 0},
	} {
		t.Run(test.name, func(t *testing.T) {
			data := missionData{BoughtQuantity: test.bought, DeliveredQuantity: test.delivered, AcquisitionPence: test.acquisition}
			got := cargoShortfallPence(&data)
			if got == nil || *got != test.want {
				t.Fatalf("integer cargo loss: got %v, want %d", got, test.want)
			}
		})
	}
	if got := cargoShortfallPence(&missionData{}); got != nil {
		t.Fatal("unobserved legacy cargo basis became measured zero")
	}
}

func TestSettlementPlanRecordsPopulatedHazard(t *testing.T) {
	data := missionData{
		Plan: Plan{Kind: "spot", Origin: recoveryHomePort, Destination: recoveryFarPort, Commodity: recoveryGrain,
			Economics: Economics{Basis: "destination", RevenueBasis: "depth", ExpectedNetPence: 0, RevenuePence: 0, HazardBPS: 0, HazardSource: "lloyds_bound"}},
		CostsKnown: true,
	}
	row := procurementReviewSettlement(t, data, nil)
	if row.HazardBPS == nil || *row.HazardBPS != 0 || row.HazardSource == nil || *row.HazardSource != "lloyds_bound" {
		t.Fatalf("populated zero hazard lost its authority: %+v", row)
	}
}
