package engine

import (
	"context"
	"database/sql"
	"encoding/json"
	"math"
	"strings"
	"testing"
	"time"

	"github.com/DevYukine/go-tradewinds/internal/gameapi"
	"github.com/DevYukine/go-tradewinds/internal/store"
	"github.com/google/uuid"
)

func passengerFaultDatabase(t *testing.T, h *recoveryHarness) *sql.DB {
	t.Helper()
	db, err := sql.Open("pgx", h.dsn)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	return db
}

func passengerDecisionRows(t *testing.T, h *recoveryHarness) []store.Decision {
	t.Helper()
	history, err := h.db.History(h.ctx, h.account.ID, "decisions", 100)
	if err != nil {
		t.Fatal(err)
	}
	raw, err := json.Marshal(history)
	if err != nil {
		t.Fatal(err)
	}
	var rows []store.Decision
	if err := json.Unmarshal(raw, &rows); err != nil {
		t.Fatal(err)
	}
	return rows
}

func TestPassengerTransientEventReadHoldsAndRetriesWithoutQuarantine(t *testing.T) {
	h, r, in := passengerRunnerFixture(t)
	if _, err := r.considerPassage(h.ctx, in.Ship, in, nil); err != nil {
		t.Fatal(err)
	}
	mission, data := h.mission(in.Ship.Ship.ID)
	original := mission.Generation
	db := passengerFaultDatabase(t, h)
	if _, err := db.ExecContext(h.ctx, "ALTER TABLE events RENAME TO passenger_fault_events"); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_, _ = db.ExecContext(context.Background(), "ALTER TABLE IF EXISTS passenger_fault_events RENAME TO events")
	})
	if changed, err := r.driveMission(h.ctx, in.Ship, mission, &data); err != nil || changed {
		t.Fatalf("transient read did not hold: %t %v", changed, err)
	}
	_, restored := h.mission(in.Ship.Ship.ID)
	if data.Passage.QuarantineReason != "" || restored.Passage.QuarantineReason != "" || mission.Generation != original {
		t.Fatalf("transient database read quarantined immutable passage: %+v", restored)
	}
	held := false
	for _, row := range passengerDecisionRows(t, h) {
		if row.Kind == "passage_quarantined" {
			t.Fatal("transient read recorded a quarantine")
		}
		held = held || row.Kind == "hold" && strings.Contains(row.Reason, "deferred")
	}
	if !held {
		t.Fatal("transient read has no visible hold reason")
	}
	if _, err := db.ExecContext(h.ctx, "ALTER TABLE passenger_fault_events RENAME TO events"); err != nil {
		t.Fatal(err)
	}
	if changed, err := r.driveMission(h.ctx, in.Ship, mission, &data); err != nil || !changed || data.Action == nil || data.Action.Stage != "passenger-offer" {
		t.Fatalf("restored database did not retry unchanged passage: %t %+v %v", changed, data, err)
	}
	cancelled, cancel := context.WithCancel(h.ctx)
	cancel()
	if _, err := r.dispatch(cancelled, mission, &data); err == nil {
		t.Fatal("cancelled dispatch unexpectedly succeeded")
	}
	if data.Passage.QuarantineReason != "" {
		t.Fatal("cancelled dispatch permanently quarantined passage")
	}
}

func TestPassengerQuarantineRecoveryErrorHasRateLimitedVisibleHold(t *testing.T) {
	h, r, in := passengerRunnerFixture(t)
	mission, data := r.newVoyage(in.Ship, Plan{Kind: "passage", ShipID: in.Ship.Ship.ID, Origin: "origin", Destination: "destination"})
	data.Passage = &passageIntent{QuarantineReason: "conflicting receipt"}
	if err := r.saveMission(h.ctx, mission, &data); err != nil {
		t.Fatal(err)
	}
	db := passengerFaultDatabase(t, h)
	if _, err := db.ExecContext(h.ctx, "ALTER TABLE passenger_quarantine_archives RENAME TO passenger_fault_archives"); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_, _ = db.ExecContext(context.Background(), "ALTER TABLE IF EXISTS passenger_fault_archives RENAME TO passenger_quarantine_archives")
	})
	for range 3 {
		if changed, err := r.driveMission(h.ctx, in.Ship, mission, &data); err != nil || changed {
			t.Fatalf("archive read fault did not hold recovery: %t %v", changed, err)
		}
	}
	holds := 0
	for _, row := range passengerDecisionRows(t, h) {
		if row.Kind == "hold" && strings.Contains(row.Reason, "quarantined passenger recovery deferred") {
			holds++
		}
	}
	if holds != 1 {
		t.Fatalf("recovery hold missing or not rate limited: %d", holds)
	}
	if _, err := db.ExecContext(h.ctx, "ALTER TABLE passenger_fault_archives RENAME TO passenger_quarantine_archives"); err != nil {
		t.Fatal(err)
	}
	if _, err := r.driveMission(h.ctx, in.Ship, mission, &data); err != nil {
		t.Fatal(err)
	}
	h.missingMission(in.Ship.Ship.ID)
}

func TestPassengerFirstOfferUsesOutboundStoresAndReofferNeverRedocks(t *testing.T) {
	for _, mode := range []string{"first offer stocked", "first offer short", "reoffer consumed stores"} {
		t.Run(mode, func(t *testing.T) {
			h, r, in := passengerRunnerFixture(t)
			if _, err := r.considerPassage(h.ctx, in.Ship, in, nil); err != nil {
				t.Fatal(err)
			}
			mission, data := h.mission(in.Ship.Ship.ID)
			hull, _ := findHull(r.stateCache.Catalog, in.Ship.Ship.ShipClass)
			route := r.stateCache.Routes[routeKey("origin", "destination", hull.ID)]
			data.Plan.TargetStoresTons = route.SafeProvisionsTons + 10
			ladderConsumption := float64(hull.CrewRequired) / 200 * (3.0 / 24)
			in.Ship.Stores.ProvisionsTons = route.SafeProvisionsTons + 1 + ladderConsumption
			if mode == "first offer short" {
				in.Ship.Ship.Status = "berthed"
				in.Ship.Stores.ProvisionsTons = 0
			}
			if mode == "reoffer consumed stores" {
				data.Passage.Reoffer = true
				data.Passage.StepOpenedAt = in.Now.Add(-time.Minute)
				in.Ship.Stores.ProvisionsTons = route.SafeProvisionsTons + 0.99
			}
			r.stateCache.Ships[0] = in.Ship
			if err := r.saveMission(h.ctx, mission, &data); err != nil {
				t.Fatal(err)
			}
			if changed, err := r.driveMission(h.ctx, in.Ship, mission, &data); err != nil || !changed || data.Action == nil {
				t.Fatalf("offer progression stalled: %t %+v %v", changed, data.Action, err)
			}
			if mode == "first offer short" {
				if data.Action.Stage != "provision" {
					t.Fatalf("first offer skipped safe outbound stores: %+v", data.Action)
				}
				var body struct {
					Quantity int64 `json:"quantity"`
				}
				if err := json.Unmarshal(data.Action.Body, &body); err != nil || body.Quantity != int64(math.Ceil(route.SafeProvisionsTons+1+ladderConsumption)) {
					t.Fatalf("first offer omitted ladder consumption or bought return stores: %s %v", data.Action.Body, err)
				}
			} else if data.Action.Stage != "passenger-offer" {
				t.Fatalf("offshore ladder redocked for target-store top-up: %+v", data.Action)
			}
		})
	}
}

func TestPassengerCashActionsRetainAccountCreditOnlyGate(t *testing.T) {
	for _, stage := range []string{"provision", "dock", "offshore", "depart"} {
		t.Run(stage, func(t *testing.T) {
			h, r, in := passengerRunnerFixture(t)
			mission, data := r.newVoyage(in.Ship, Plan{Kind: "passage", ShipID: in.Ship.Ship.ID, Origin: "origin", Destination: "destination"})
			data.Passage = &passageIntent{}
			path := "/v1/fleet/" + stage
			var body any = map[string]string{"ship_id": in.Ship.Ship.ID, "dest_port_id": "destination"}
			if stage == "provision" {
				path = "/v1/market/place-order"
				body = orderBody(in.Ship, "provisions", "bid", 1, 1000, true)
			}
			if err := r.stage(h.ctx, mission, &data, in.Ship, stage, path, body); err != nil {
				t.Fatal(err)
			}
			data.CooldownUntil = time.Now().Add(time.Hour)
			if err := r.saveMission(h.ctx, mission, &data); err != nil {
				t.Fatal(err)
			}
			other := in.Ship
			other.Ship.ID = "unrelated"
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
			if changed, err := r.work(h.ctx); err != nil || changed {
				t.Fatalf("unresolved passenger cash action bypassed credit-only account gate: %t %v", changed, err)
			}
			h.mission(other.Ship.ID)
		})
	}
}

func TestPassengerRequarantineRetainsOneGenerationAndArchive(t *testing.T) {
	h, r, in := passengerRunnerFixture(t)
	ship := in.Ship
	ship.PassengerHoldTons = 10
	r.stateCache.Ships[0] = ship
	group := gameapi.PassengerAboardGroup{ID: "aboard", Size: 10, HoldTons: 10, OriginPortID: "origin", DestPortID: "destination"}
	r.snapshot["passenger_aboard"] = encode(map[string]any{ship.Ship.ID: []gameapi.PassengerAboardGroup{group}})
	mission, data := r.newVoyage(ship, Plan{Kind: "passage", ShipID: ship.Ship.ID, Origin: "origin", Destination: "destination"})
	generation := mission.Generation
	data.Passage = &passageIntent{QuarantineReason: "original damaged receipt", Groups: []gameapi.PassengerAboardGroup{group}}
	if err := r.saveMission(h.ctx, mission, &data); err != nil {
		t.Fatal(err)
	}
	for range 3 {
		if changed, err := r.driveMission(h.ctx, ship, mission, &data); err != nil || !changed || data.Action == nil {
			t.Fatalf("safe recovery did not stage: %t %+v %v", changed, data, err)
		}
		if mission.Generation != generation {
			t.Fatal("recovery allocated another passage generation")
		}
		action := data.Action
		if _, err := r.db.BeginAction(h.ctx, &store.Action{ID: action.ID, AccountID: h.account.ID, Method: action.Method, Path: action.Path, Body: json.RawMessage(`{"conflicting":"immutable request"}`)}); err != nil {
			t.Fatal(err)
		}
		if _, err := r.dispatch(h.ctx, mission, &data); err != nil {
			t.Fatal(err)
		}
		if data.Passage.QuarantineReason == "" {
			t.Fatal("actual immutable journal conflict was not quarantined")
		}
	}
	db := passengerFaultDatabase(t, h)
	var count int
	if err := db.QueryRowContext(h.ctx, "SELECT count(*) FROM passenger_quarantine_archives WHERE account_id = $1", h.account.ID).Scan(&count); err != nil {
		t.Fatal(err)
	}
	if count != 1 {
		t.Fatalf("re-quarantine grew archive generations: %d", count)
	}
	_, retained := loadPassengerQuarantineRecord(t, h, r, generation)
	if retained.Passage.QuarantineReason != "original damaged receipt" {
		t.Fatal("re-quarantine overwrote original accounting evidence")
	}
}

func TestPassengerReceiptReadFailureIsDeferredNotQuarantined(t *testing.T) {
	h, r, _ := passengerRunnerFixture(t)
	db := passengerFaultDatabase(t, h)
	if _, err := db.ExecContext(h.ctx, "ALTER TABLE passenger_receipts RENAME TO passenger_fault_receipts"); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_, _ = db.ExecContext(context.Background(), "ALTER TABLE IF EXISTS passenger_fault_receipts RENAME TO passenger_receipts")
	})
	delete(r.meta, "contracts")
	if _, err := r.work(h.ctx); err != nil {
		t.Fatal(err)
	}
	deferred := false
	for _, row := range passengerDecisionRows(t, h) {
		if row.Kind == "passenger_receipt_quarantined" {
			t.Fatal("failed database read claimed a real receipt quarantine")
		}
		deferred = deferred || row.Kind == "passenger_receipt_deferred"
	}
	if !deferred {
		t.Fatal("receipt read failure was not visibly deferred")
	}
}
