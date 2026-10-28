package engine

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	"github.com/DevYukine/go-tradewinds/internal/gameapi"
	"github.com/DevYukine/go-tradewinds/internal/store"
	"github.com/google/uuid"
)

func TestPassengerQuarantinedDepartRetiresBeforeWorkCreditOnlyGate(t *testing.T) {
	for _, alreadyQuarantined := range []bool{false, true} {
		t.Run(map[bool]string{false: "dispatch integrity failure", true: "restored quarantine"}[alreadyQuarantined], func(t *testing.T) {
			h, r, in := passengerRunnerFixture(t)
			mission, data := r.newVoyage(in.Ship, Plan{Kind: "passage", ShipID: in.Ship.Ship.ID, Origin: "origin", Destination: "destination"})
			generation := mission.Generation
			data.Passage = &passageIntent{}
			if alreadyQuarantined {
				data.Passage.QuarantineReason = "restored immutable proof conflict"
			}
			if err := r.stage(h.ctx, mission, &data, in.Ship, "depart", "/v1/fleet/depart", map[string]string{"ship_id": in.Ship.Ship.ID, "dest_port_id": "destination"}); err != nil {
				t.Fatal(err)
			}
			action := data.Action
			if _, err := r.db.BeginAction(h.ctx, &store.Action{ID: action.ID, AccountID: h.account.ID, Method: action.Method, Path: action.Path, Body: json.RawMessage(`{"conflicting":"immutable depart request"}`)}); err != nil {
				t.Fatal(err)
			}
			other := in.Ship
			other.Ship.ID = "normal-work-hull"
			otherMission, otherData := r.newVoyage(other, Plan{Kind: "passage", ShipID: other.Ship.ID, Origin: "origin", Destination: "destination"})
			otherData.Passage = &passageIntent{Abandon: true}
			if err := r.saveMission(h.ctx, otherMission, &otherData); err != nil {
				t.Fatal(err)
			}
			r.stateCache.Ships = append(r.stateCache.Ships, other)
			treasury := &store.Mission{AccountID: h.account.ID, ShipID: "__treasury", Phase: "settings", Generation: uuid.NewString()}
			treasuryData := missionData{CooldownUntil: time.Now().Add(time.Hour)}
			if err := r.saveMission(h.ctx, treasury, &treasuryData); err != nil {
				t.Fatal(err)
			}
			for _, key := range []string{"ships", "contracts", "vaults", "clock", "catalog", "factors", "warehouses", "orders", "fills", "ledger", "rates", "settings"} {
				r.meta[key] = r.meta["zones"]
			}
			if changed, err := r.work(h.ctx); err != nil || !changed {
				t.Fatalf("quarantined depart froze normal work: %t %v", changed, err)
			}
			h.missingMission(other.Ship.ID)
			h.missingMission(in.Ship.Ship.ID)
			if pending := h.pending(); len(pending) != 0 {
				t.Fatalf("retired quarantine left unresolved spending: %+v", pending)
			}
			archive, retained := loadPassengerQuarantineRecord(t, h, r, generation)
			if archive == nil || retained.Action == nil || retained.Action.ID != action.ID || retained.Passage.QuarantineReason == "" {
				t.Fatalf("normal work discarded quarantined request evidence: %+v", retained)
			}
			// Repeat work after a process restart: another ordinary mission is still
			// reached, rather than an old depart keeping the account credit-only.
			if err := r.saveMission(h.ctx, otherMission, &otherData); err != nil {
				t.Fatal(err)
			}
			restarted := h.runner(primaryTestKey)
			restarted.meta, restarted.generation, restarted.stateCache = r.meta, r.generation, r.stateCache
			restarted.stateCache.Ships = []ShipState{other, in.Ship}
			if changed, err := restarted.work(h.ctx); err != nil || !changed {
				t.Fatalf("restart restored the fleet deadlock: %t %v", changed, err)
			}
			h.missingMission(other.Ship.ID)
		})
	}
}

func TestPassengerBoardsFullHoldThenDepartsOnSafeStoresAfterLadderConsumption(t *testing.T) {
	for _, recovery := range []bool{false, true} {
		t.Run(map[bool]string{false: "ordinary passage", true: "quarantined recovery"}[recovery], func(t *testing.T) {
			h, r, in := passengerRunnerFixture(t)
			ship := in.Ship
			hull, _ := findHull(r.stateCache.Catalog, ship.Ship.ShipClass)
			route := r.stateCache.Routes[routeKey("origin", "destination", hull.ID)]
			// Less than Safe+1 after hotel consumption, but still exactly safe to sail.
			ship.Stores.ProvisionsTons = route.SafeProvisionsTons
			ship.PassengerHoldTons = ship.EffectiveHoldCapacityTons
			ship.CargoUsedTons = ship.EffectiveHoldCapacityTons
			r.stateCache.Ships[0] = ship
			boarded, promised := in.Now.Add(-time.Minute), in.Now.Add(time.Hour)
			group := gameapi.PassengerAboardGroup{ID: "full-hold", Kind: "travellers", Size: ship.PassengerHoldTons, HoldTons: ship.PassengerHoldTons, OriginPortID: "origin", DestPortID: "destination", BoardedAt: &boarded, PromisedArrivalAt: &promised}
			r.snapshot["passenger_aboard"] = encode(map[string]any{ship.Ship.ID: []gameapi.PassengerAboardGroup{group}})
			mission, data := r.newVoyage(ship, Plan{Kind: "passage", ShipID: ship.Ship.ID, Origin: "origin", Destination: "destination"})
			data.Passage = &passageIntent{Groups: []gameapi.PassengerAboardGroup{group}, RecoveryOnly: recovery}
			if recovery {
				data.Passage.RecoveryReason = "damaged receipt accounting"
			}
			if err := r.saveMission(h.ctx, mission, &data); err != nil {
				t.Fatal(err)
			}
			for range 3 {
				if _, err := r.driveMission(h.ctx, ship, mission, &data); err != nil {
					t.Fatal(err)
				}
				if data.Action != nil {
					break
				}
			}
			if data.Action == nil || data.Action.Stage != "depart" {
				t.Fatalf("safe full passenger hold re-docked for consumed margin: %+v", data.Action)
			}
			if err := r.newActionPreflight(h.ctx, mission, &data); err != nil {
				t.Fatalf("safe departure cannot cross first-send safety gate: %v", err)
			}
		})
	}
}

func TestPassengerMissingHullClassifiesIntegrityAndTransientEvidenceErrors(t *testing.T) {
	h, r, in := passengerRunnerFixture(t)
	mission, data := r.newVoyage(in.Ship, Plan{Kind: "passage", ShipID: in.Ship.Ship.ID, Origin: "origin", Destination: "destination"})
	data.Passage = &passageIntent{Measurements: map[string]store.PassengerEvidence{"bad": {ID: "bad-evidence", AccountID: h.account.ID, Lane: "origin|destination", Kind: "invalid-kind", Heads: 10, FarePerHeadPence: 100, Outcome: "boarded"}}}
	if err := r.saveMission(h.ctx, mission, &data); err != nil {
		t.Fatal(err)
	}
	r.stateCache.Ships = nil
	db := passengerFaultDatabase(t, h)
	if _, err := db.ExecContext(h.ctx, "ALTER TABLE events RENAME TO passenger_fault_events"); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_, _ = db.ExecContext(context.Background(), "ALTER TABLE IF EXISTS passenger_fault_events RENAME TO events")
	})
	if _, err := r.reconcileMissingPassages(h.ctx); err != nil {
		t.Fatal(err)
	}
	_, retained := h.mission(in.Ship.Ship.ID)
	if retained.Passage.QuarantineReason != "" {
		t.Fatal("transient missing-hull read quarantined evidence")
	}
	if _, err := db.ExecContext(h.ctx, "ALTER TABLE passenger_fault_events RENAME TO events"); err != nil {
		t.Fatal(err)
	}
	if _, err := r.reconcileMissingPassages(h.ctx); err != nil {
		t.Fatal(err)
	}
	_, retained = h.mission(in.Ship.Ship.ID)
	if retained.Passage.QuarantineReason == "" {
		t.Fatal("invalid sunk-passage evidence was retried instead of quarantined")
	}
	before := len(passengerDecisionRows(t, h))
	if _, err := r.reconcileMissingPassages(h.ctx); err != nil {
		t.Fatal(err)
	}
	if after := len(passengerDecisionRows(t, h)); after != before {
		t.Fatalf("quarantined missing hull kept replaying malformed evidence: %d -> %d", before, after)
	}
}

func TestPassengerQuarantineAndRecoveryMarkUnknownMeasuredCostsWithoutDoubleCounting(t *testing.T) {
	h, r, in := passengerRunnerFixture(t)
	ship := in.Ship
	ship.PassengerHoldTons = 10
	r.stateCache.Ships[0] = ship
	group := gameapi.PassengerAboardGroup{ID: "aboard", Size: 10, HoldTons: 10, OriginPortID: "origin", DestPortID: "destination"}
	r.snapshot["passenger_aboard"] = encode(map[string]any{ship.Ship.ID: []gameapi.PassengerAboardGroup{group}})
	mission, data := r.newVoyage(ship, Plan{Kind: "passage", ShipID: ship.Ship.ID, Origin: "origin", Destination: "destination"})
	generation := mission.Generation
	data.Passage = &passageIntent{QuarantineReason: "damaged fare proof", Groups: []gameapi.PassengerAboardGroup{group}}
	data.ProvisionsPence, data.FeesPence, data.ExpensesPence = 100, 20, 30
	if err := r.saveMission(h.ctx, mission, &data); err != nil {
		t.Fatal(err)
	}
	if changed, err := r.driveMission(h.ctx, ship, mission, &data); err != nil || !changed {
		t.Fatalf("recovery did not start: %t %v", changed, err)
	}
	first, err := r.db.EconomicHistoryWindow(h.ctx, h.account.ID, "", time.Now().UTC(), 24*time.Hour)
	if err != nil || !first.UnknownCosts || first.PositiveCycles != 0 {
		t.Fatalf("quarantine disappeared from measured-R window: %+v %v", first, err)
	}
	// Only the additional recovery debits belong to the second marker.
	data.Action = nil
	data.ProvisionsPence += 40
	data.FeesPence += 5
	data.ExpensesPence += 7
	ship.Ship.CurrentPortID, ship.Ship.Status = "destination", "berthed"
	ship.PassengerHoldTons = 0
	r.stateCache.Ships[0] = ship
	r.snapshot["passenger_aboard"] = encode(map[string]any{ship.Ship.ID: []gameapi.PassengerAboardGroup{}})
	if err := r.saveMission(h.ctx, mission, &data); err != nil {
		t.Fatal(err)
	}
	if _, err := r.driveMission(h.ctx, ship, mission, &data); err != nil {
		t.Fatal(err)
	}
	h.missingMission(ship.Ship.ID)
	rows, err := r.db.SettlementHistory(h.ctx, h.account.ID, "origin|destination", 20)
	if err != nil || len(rows) != 2 {
		t.Fatalf("quarantine/recovery markers missing: %+v %v", rows, err)
	}
	var costs int64
	for _, row := range rows {
		if row.CostsKnown || !row.ProceedsUnknown || row.MissionID != generation || row.ActionID != "" || row.EventID != "" {
			t.Fatalf("quarantine became measured/fictional income: %+v", row)
		}
		costs += row.ProvisionsPence + row.FeesPence + row.ExpensesPence
		raw, err := json.Marshal(row)
		var displayed map[string]any
		if err != nil || json.Unmarshal(raw, &displayed) != nil || displayed["proceeds_pence"] != nil {
			t.Fatalf("unknown fare displayed as zero: %s %v", raw, err)
		}
	}
	if costs != 202 {
		t.Fatalf("quarantine and recovery costs counted twice or lost: %d", costs)
	}
	measured, err := r.db.EconomicHistoryWindow(h.ctx, h.account.ID, "", time.Now().UTC(), 24*time.Hour)
	if err != nil || !measured.UnknownCosts || measured.PositiveCycles != 0 || measured.GameSeconds != 0 || measured.UnknownLossPence != -202 {
		t.Fatalf("unknown accounting biased measured R: %+v %v", measured, err)
	}
}
