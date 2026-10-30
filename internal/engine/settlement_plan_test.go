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
					Economics: Economics{Basis: "destination", RevenueBasis: "retained", RevenueAgeGameHours: new(24.5), RevenuePence: 24_000,
						UncalibratedRevenuePence: new(int64(24_000)), RevenueQuantityTons: 30, ExpectedNetPence: 4_000}},
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

func TestMoneyCalibrationSettledCalibratedQuotesKeepTheirRawRevenue(t *testing.T) {
	h := newRecoveryHarness(t)
	h.api.setFleet([]map[string]any{destinationShipRow(recoveryShipBlastoise, "Blastoise", recoveryFarPort, "anchored_roadstead", 3, nil, nil)})
	runner := refreshedRunner(t, h)
	runner.snapshot["ledger"] = json.RawMessage(`[]`)
	ship := harnessShip(t, runner, recoveryShipBlastoise)
	in := moneyInput(t, time.Now().UTC(), "canvas_sails", 6_000, 10_000, 1, 30)
	in.RevenueCalibration = map[string]int64{"depth": 9500}
	plan := moneySpot(t, in, "canvas_sails")
	if !plan.Accepted || plan.Economics.RevenuePence != 9500 {
		t.Fatalf("fixture needs an already calibrated quote: %+v", plan)
	}
	since := time.Now().UTC().Add(-time.Hour)
	for i := range 30 {
		id := fmt.Sprintf("calibrated-settlement-%d", i)
		destinationSeedAction(t, h, id, "/v1/market/place-order", `{"quantity":1}`, `{"filled_quantity":1}`)
		data := missionData{
			Plan: plan, BoughtQuantity: 1, DeliveredQuantity: 1, BuyDone: true, CostsKnown: true,
			AcquisitionPence: 6000, ProceedsPence: 9500, LastActionID: id, FinishAtDestination: true,
			StartedAt: since.Add(time.Minute), ProvenLane: laneKey(plan), InitialLedgerIDs: map[string]bool{},
		}
		h.putMission(ship.Ship.ID, id, "closing", data)
		mission, data := h.mission(ship.Ship.ID)
		if _, err := runner.finishMission(h.ctx, ship, mission, &data); err != nil {
			t.Fatal(err)
		}
		// Reload the frozen intent before inserting, as on a process restart.
		mission, data = h.mission(ship.Ship.ID)
		if _, err := runner.finishMission(h.ctx, ship, mission, &data); err != nil {
			t.Fatal(err)
		}
		if data.Settlement.PlannedRevenuePence == nil || *data.Settlement.PlannedRevenuePence != 10_000 ||
			data.Settlement.PlannedNetPence == nil || *data.Settlement.PlannedNetPence != plan.Economics.ExpectedNetPence {
			t.Fatalf("settlement mixed calibrated net with the wrong revenue baseline: %+v", data.Settlement)
		}
		if i == 9 || i == 29 {
			factors, err := h.db.RevenueCalibration(h.ctx, h.account.ID, since, time.Now().UTC().Add(time.Second))
			if err != nil || factors["depth"] != 9500 {
				t.Fatalf("calibration cancelled itself after %d receipts: %v, %v", i+1, factors, err)
			}
		}
	}
}

func TestMoneyCalibrationPartialIOCBuyDoesNotBecomeAFullLotPriceMiss(t *testing.T) {
	for _, filled := range []int64{5, 10} {
		t.Run(fmt.Sprint(filled), func(t *testing.T) {
			h := newRecoveryHarness(t)
			h.api.setFleet([]map[string]any{destinationShipRow(recoveryShipBlastoise, "Blastoise", recoveryFarPort, "anchored_roadstead", 3, nil, nil)})
			runner := refreshedRunner(t, h)
			runner.snapshot["ledger"] = json.RawMessage(`[]`)
			ship := harnessShip(t, runner, recoveryShipBlastoise)
			in := moneyInput(t, time.Now().UTC(), "canvas_sails", 100, 200, 10, 30)
			plan := moneySpot(t, in, "canvas_sails")
			body := `{"commodity_id":"canvas_sails","quantity":10}`
			response := fmt.Sprintf(`{"commodity_id":"canvas_sails","filled_quantity":%d,"total_gross_pence":%d,"customs_duty_pence":0}`, filled, filled*100)
			destinationSeedAction(t, h, "partial-calibration-buy", "/v1/market/place-order", body, response)
			data := missionData{
				Plan: plan, CostsKnown: true, FinishAtDestination: true, StartedAt: time.Now().UTC().Add(-time.Minute),
				ProvenLane: laneKey(plan), InitialLedgerIDs: map[string]bool{},
				Action: &durableAction{ID: "partial-calibration-buy", Stage: "buy", Body: json.RawMessage(body)},
			}
			h.putMission(ship.Ship.ID, "partial-calibration", "buy", data)
			mission, data := h.mission(ship.Ship.ID)
			if _, err := runner.consumeResult(h.ctx, mission, &data, http.StatusOK, json.RawMessage(response)); err != nil {
				t.Fatal(err)
			}
			if data.Plan.Quantity != filled || data.Plan.Economics.RevenueQuantityTons != 10 {
				t.Fatalf("IOC receipt erased the full-lot quote quantity: %+v", data.Plan)
			}
			data.DeliveredQuantity, data.ProceedsPence = filled, filled*200
			mission.Phase = "closing"
			for range 2 {
				if _, err := runner.finishMission(h.ctx, ship, mission, &data); err != nil {
					t.Fatal(err)
				}
			}
			row := data.Settlement
			if row.CargoShortfallPence == nil || *row.CargoShortfallPence != 0 {
				t.Fatalf("partial purchase was incorrectly treated as lost cargo: %+v", row)
			}
			if filled == 5 && row.PlannedRevenuePence != nil ||
				filled == 10 && (row.PlannedRevenuePence == nil || *row.PlannedRevenuePence != 2000) {
				t.Fatalf("forecast denominator did not distinguish partial from full IOC fill: %+v", row)
			}
		})
	}
}

func TestMoneyCalibrationLegacyRevenueRemainsUnmeasured(t *testing.T) {
	row := procurementReviewSettlement(t, missionData{
		Plan: Plan{Kind: "spot", Origin: recoveryHomePort, Destination: recoveryFarPort, Commodity: recoveryGrain, Quantity: 1,
			Economics: Economics{RevenuePence: 9500, RevenueCalibrationBPS: 9500, RevenueBasis: "depth"}},
		BoughtQuantity: 1, DeliveredQuantity: 1, AcquisitionPence: 6000, ProceedsPence: 9500, CostsKnown: true,
	}, nil)
	if row.PlannedRevenuePence != nil {
		t.Fatalf("legacy mission without a raw quote fabricated a calibration denominator: %+v", row)
	}
}
