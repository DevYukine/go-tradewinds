package engine

import (
	"encoding/json"
	"testing"

	"github.com/DevYukine/go-tradewinds/internal/gameapi"
	"github.com/DevYukine/go-tradewinds/internal/store"
	"github.com/google/uuid"
)

func TestPassengerQuarantineReleasesEmptyHullAndRetainsPrivateRecord(t *testing.T) {
	h, r, in := passengerRunnerFixture(t)
	mission, data := r.newVoyage(in.Ship, Plan{Kind: "passage", ShipID: in.Ship.Ship.ID, Origin: "origin", Destination: "destination"})
	generation := mission.Generation
	receipt := &store.Settlement{ID: uuid.NewString(), AccountID: h.account.ID, ShipID: in.Ship.Ship.ID, MissionID: generation, Kind: "passage", EventID: "conflicting-proof", ProceedsUnknown: true}
	data.Passage = &passageIntent{QuarantineReason: "conflicting terminal proof", ReceiptsPrepared: true, Receipts: []*store.Settlement{receipt}}
	data.CostsKnown = false
	if err := r.saveMission(h.ctx, mission, &data); err != nil {
		t.Fatal(err)
	}
	if err := r.stage(h.ctx, mission, &data, in.Ship, "passenger-offer", "/v1/passengers/offer", map[string]string{"ship_id": in.Ship.Ship.ID}); err != nil {
		t.Fatal(err)
	}
	action := data.Action
	if _, err := r.db.BeginAction(h.ctx, &store.Action{ID: action.ID, AccountID: h.account.ID, Method: action.Method, Path: action.Path, Body: action.Body}); err != nil {
		t.Fatal(err)
	}
	if _, err := r.driveMission(h.ctx, in.Ship, mission, &data); err != nil {
		t.Fatal(err)
	}
	h.missingMission(in.Ship.Ship.ID)
	if pending := h.pending(); len(pending) != 0 {
		t.Fatalf("released hull left orphan pending intent: %+v", pending)
	}
	// Reload through a new runner: the unknown receipt and immutable request are
	// retained privately, but neither occupies the operating ship slot.
	restarted := h.runner(primaryTestKey)
	archived, retained := loadPassengerQuarantineRecord(t, h, restarted, generation)
	if archived == nil {
		t.Fatal("quarantine record lost")
	}
	if archived.Phase != "passage_quarantined" || retained.Passage.QuarantineReason == "" || len(retained.Passage.Receipts) != 1 || !retained.Passage.Receipts[0].ProceedsUnknown || retained.Passage.Receipts[0].EventID != receipt.EventID || retained.Action == nil || retained.Action.ID != action.ID {
		t.Fatalf("quarantine evidence changed: %+v", retained)
	}
	rows, err := r.db.SettlementHistory(h.ctx, h.account.ID, "origin|destination", 20)
	if err != nil || len(rows) != 1 || rows[0].CostsKnown || !rows[0].ProceedsUnknown {
		t.Fatalf("quarantine invented settled income: %+v %v", rows, err)
	}
	// The normal idle-ship admission path is usable again, not a special
	// quarantine-only mission loop. The record must not count as a live hull.
	if changed, err := r.considerPassage(h.ctx, in.Ship, in, nil); err != nil || !changed {
		t.Fatalf("released hull cannot return to normal planning: %t %v", changed, err)
	}
	next, _ := h.mission(in.Ship.Ship.ID)
	if next.Generation == generation {
		t.Fatal("normal work reused quarantined generation")
	}
}

func TestPassengerQuarantineRequiresAuthoritativeEmptyAboard(t *testing.T) {
	for _, mode := range []string{"stale", "null", "occupied hold"} {
		t.Run(mode, func(t *testing.T) {
			h, r, in := passengerRunnerFixture(t)
			mission, data := r.newVoyage(in.Ship, Plan{Kind: "passage", ShipID: in.Ship.Ship.ID, Origin: "origin", Destination: "destination"})
			data.Passage = &passageIntent{QuarantineReason: "bad receipt"}
			if err := r.saveMission(h.ctx, mission, &data); err != nil {
				t.Fatal(err)
			}
			switch mode {
			case "stale":
				delete(r.meta, "passenger_aboard/"+in.Ship.Ship.ID)
			case "null":
				r.snapshot["passenger_aboard"] = encode(map[string]any{in.Ship.Ship.ID: nil})
			case "occupied hold":
				in.Ship.PassengerHoldTons = 10
			}
			if changed, err := r.driveMission(h.ctx, in.Ship, mission, &data); err != nil || changed {
				t.Fatalf("uncertain aboard state released hull: %t %v", changed, err)
			}
			retained, _ := h.mission(in.Ship.Ship.ID)
			if retained.Generation != mission.Generation {
				t.Fatal("uncertain aboard state replaced mission")
			}
		})
	}
}

func TestPassengerQuarantineSailsOccupiedHullThenReleasesAfterArrival(t *testing.T) {
	h, r, in := passengerRunnerFixture(t)
	ship := in.Ship
	ship.PassengerHoldTons = 10
	r.stateCache.Ships[0] = ship
	group := gameapi.PassengerAboardGroup{ID: "aboard", Size: 10, HoldTons: 10, OriginPortID: "origin", DestPortID: "destination"}
	// Optional historical timestamps may be null; current destination/occupancy
	// still supplies the navigation obligation, never revenue evidence.
	r.snapshot["passenger_aboard"] = encode(map[string]any{ship.Ship.ID: []gameapi.PassengerAboardGroup{group}})
	mission, data := r.newVoyage(ship, Plan{Kind: "passage", ShipID: ship.Ship.ID, Origin: "origin", Destination: "wrong-old-destination"})
	oldGeneration := mission.Generation
	data.Passage = &passageIntent{QuarantineReason: "null historical boarding timestamp", Groups: []gameapi.PassengerAboardGroup{group}}
	if err := r.saveMission(h.ctx, mission, &data); err != nil {
		t.Fatal(err)
	}
	if changed, err := r.driveMission(h.ctx, ship, mission, &data); err != nil || !changed {
		t.Fatalf("occupied quarantined hull did not sail: %t %v", changed, err)
	}
	if !data.Passage.RecoveryOnly || data.Passage.QuarantineReason != "" || data.Action == nil || data.Action.Stage != "depart" || mission.Generation != oldGeneration {
		t.Fatalf("unsafe recovery intent: %+v", data)
	}
	var departure struct {
		Destination string `json:"dest_port_id"`
	}
	if err := json.Unmarshal(data.Action.Body, &departure); err != nil || departure.Destination != "destination" {
		t.Fatalf("recovery used historical destination: %s %v", data.Action.Body, err)
	}
	if err := r.newActionPreflight(h.ctx, mission, &data); err != nil {
		t.Fatalf("safe recovery departure cannot pass first-send safety gates: %v", err)
	}
	actionID := data.Action.ID
	restarted := h.runner(primaryTestKey)
	durable, restored, err := restarted.loadMission(h.ctx, ship.Ship.ID)
	if err != nil || durable == nil || !restored.Passage.RecoveryOnly || restored.Action == nil || restored.Action.ID != actionID {
		t.Fatalf("restart lost recovery obligation: %+v %v", restored, err)
	}
	// Model the authoritative arrival after the departure journal has completed.
	data.Action = nil
	ship.Ship.CurrentPortID = "destination"
	ship.Ship.Status = "berthed"
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
	archived, retained := loadPassengerQuarantineRecord(t, h, r, oldGeneration)
	if archived == nil || retained.Passage.QuarantineReason == "" {
		t.Fatalf("original quarantined accounting not retained: %+v %+v", archived, retained)
	}
}

func TestPassengerQuarantineCancelsOpenOfferBeforeReleasingEmptyHull(t *testing.T) {
	h, r, in := passengerRunnerFixture(t)
	mission, data := r.newVoyage(in.Ship, Plan{Kind: "passage", ShipID: in.Ship.Ship.ID, Origin: "origin", Destination: "destination"})
	data.Passage = &passageIntent{QuarantineReason: "bad receipt"}
	r.snapshot["passenger_offers"] = encode([]gameapi.PassengerOffer{{ID: "still-open", ShipID: in.Ship.Ship.ID, Status: "open"}})
	if err := r.saveMission(h.ctx, mission, &data); err != nil {
		t.Fatal(err)
	}
	if changed, err := r.driveMission(h.ctx, in.Ship, mission, &data); err != nil || !changed {
		t.Fatalf("open offer not cancelled: %t %v", changed, err)
	}
	_, retained := h.mission(in.Ship.Ship.ID)
	if retained.Action == nil || retained.Action.Stage != "passenger-cancel" || !retained.Passage.RecoveryOnly {
		t.Fatalf("empty hull released while it can still board passengers: %+v", retained)
	}
}

func loadPassengerQuarantineRecord(t *testing.T, h *recoveryHarness, r *Runner, generation string) (*store.Mission, missionData) {
	t.Helper()
	mission, err := r.db.PassengerQuarantineArchive(h.ctx, h.account.ID, generation)
	if err != nil {
		t.Fatal(err)
	}
	var data missionData
	if mission != nil {
		if err := json.Unmarshal(mission.Data, &data); err != nil {
			t.Fatal(err)
		}
	}
	return mission, data
}
