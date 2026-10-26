package engine

import "testing"

func TestInvalidatedStandaloneMaintenanceReleasesShip(t *testing.T) {
	t.Parallel()
	for _, restart := range []bool{false, true} {
		name := "financial_event"
		if restart {
			name = "restart"
		}
		t.Run(name, func(t *testing.T) {
			h := newRecoveryHarness(t)
			runner := h.runner(primaryTestKey)
			ship := ShipState{}
			ship.Ship.ID = recoveryShipBlastoise
			body := map[string]any{"ship_id": ship.Ship.ID}
			if changed, err := runner.startMaintenance(h.ctx, ship, "offshore", "/v1/fleet/offshore", body); err != nil || !changed {
				t.Fatalf("stage offshore: changed %t error %v", changed, err)
			}
			mission, data := h.mission(ship.Ship.ID)
			oldID := data.Action.ID
			if restart {
				runner = h.runner(primaryTestKey)
			} else {
				runner.publishedFinancial.Add(1)
				runner.capturedFinancial = runner.publishedFinancial.Load()
			}
			if changed, err := runner.dispatch(h.ctx, mission, &data); err != nil || !changed {
				t.Fatalf("release stale standalone intent: changed %t error %v", changed, err)
			}
			h.missingMission(ship.Ship.ID)
			if len(h.pending()) != 0 || h.api.mutationCount("/v1/fleet/offshore") != 0 {
				t.Fatal("invalidated intent was journaled or sent")
			}
			if changed, err := runner.startMaintenance(h.ctx, ship, "offshore", "/v1/fleet/offshore", body); err != nil || !changed {
				t.Fatalf("fresh maintenance: changed %t error %v", changed, err)
			}
			_, fresh := h.mission(ship.Ship.ID)
			if fresh.Action == nil || fresh.Action.ID == oldID || !runner.stagedFundsValid(fresh.Action) {
				t.Fatalf("ship cannot receive a fresh valid intent: %+v", fresh.Action)
			}
		})
	}

}
