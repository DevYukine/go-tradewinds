package engine

import (
	"encoding/json"
	"net/http"
	"testing"
	"time"

	"github.com/DevYukine/go-tradewinds/internal/gameapi"
	"github.com/DevYukine/go-tradewinds/internal/store"
	"github.com/google/uuid"
)

func TestPassengerProvisionsBeforeOffshoreAndWaitsForUnlock(t *testing.T) {
	for _, locked := range []bool{false, true} {
		t.Run(map[bool]string{false: "short stores", true: "turnaround locked"}[locked], func(t *testing.T) {
			h, r, in := passengerRunnerFixture(t)
			in.Ship.Ship.Status = "berthed"
			in.Ship.Stores.ProvisionsTons = 0
			if locked {
				in.Ship.Ship.TurnaroundUnlocksAt = new(in.Now.Add(time.Hour))
			}
			r.stateCache.Ships[0] = in.Ship
			in.Ship = r.stateCache.Ships[0]
			if changed, err := r.considerPassage(h.ctx, in.Ship, in, nil); err != nil || !changed {
				t.Fatalf("admit: %t %v", changed, err)
			}
			mission, data := h.mission(in.Ship.Ship.ID)
			if changed, err := r.driveMission(h.ctx, in.Ship, mission, &data); err != nil {
				t.Fatal(err)
			} else if locked {
				if changed || data.Action != nil {
					t.Fatalf("locked hull staged navigation: %+v", data.Action)
				}
			} else {
				if !changed || data.Action == nil || data.Action.Stage != "provision" {
					t.Fatalf("short stores moved offshore before provisions: %+v", data.Action)
				}
				// Once the purchase is observed, one offshore transition leads directly
				// to offering; no return to the berth to provision again.
				data.Action = nil
				in.Ship.Stores.ProvisionsTons = data.Plan.TargetStoresTons
				r.stateCache.Ships[0] = in.Ship
				if _, err := r.driveMission(h.ctx, in.Ship, mission, &data); err != nil || data.Action == nil || data.Action.Stage != "offshore" {
					t.Fatalf("stocked hull did not move offshore: %+v %v", data.Action, err)
				}
				data.Action = nil
				in.Ship.Ship.Status = "anchored_roadstead"
				r.stateCache.Ships[0] = in.Ship
				if _, err := r.driveMission(h.ctx, in.Ship, mission, &data); err != nil || data.Action == nil || data.Action.Stage != "passenger-offer" {
					t.Fatalf("stocked roadstead hull redocked: %+v %v", data.Action, err)
				}
			}
		})
	}
}

func TestPassengerOddNewestOfferFenceHoldsWithoutAccountError(t *testing.T) {
	for _, body := range []string{"null", `[{"id":"x","ship_id":"other"}]`, `[{"id":"x","ship_id":"ship-a"},{"id":"y","ship_id":"ship-a"}]`} {
		t.Run(body, func(t *testing.T) {
			h, r, in := passengerRunnerFixture(t)
			if _, err := r.considerPassage(h.ctx, in.Ship, in, nil); err != nil {
				t.Fatal(err)
			}
			mission, data := h.mission(in.Ship.Ship.ID)
			h.api.server.Config.Handler = http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
				if req.Method != http.MethodGet {
					t.Errorf("odd fence authorized POST")
				}
				_, _ = w.Write([]byte(body))
			})
			if changed, err := r.driveMission(h.ctx, in.Ship, mission, &data); err != nil || changed || data.Action != nil || data.Passage.QuarantineReason != "" {
				t.Fatalf("odd fence aborted/released passage: %t %v %+v", changed, err, data)
			}
		})
	}
}

func TestPassengerMalformedPassageQuarantinesOnlyItsShip(t *testing.T) {
	for _, mode := range []string{"null boarding", "duplicate group", "duplicate terminal", "null aboard"} {
		t.Run(mode, func(t *testing.T) {
			h, r, in := passengerRunnerFixture(t)
			boarded, promised := in.Now.Add(-time.Minute), in.Now.Add(time.Hour)
			group := gameapi.PassengerAboardGroup{ID: "group", Kind: "travellers", Size: 10, HoldTons: 10, OriginPortID: "origin", DestPortID: "destination", FarePerHeadPence: 100, FullFarePence: 1000, BoardedAt: &boarded, PromisedArrivalAt: &promised}
			mission, data := r.newVoyage(in.Ship, Plan{Kind: "passage", ShipID: in.Ship.Ship.ID, Origin: "origin", Destination: "destination"})
			data.StartedAt = boarded
			data.Passage = &passageIntent{Groups: []gameapi.PassengerAboardGroup{group}, FirstBoardedAt: boarded}
			if mode == "null boarding" {
				data.Passage.Groups[0].BoardedAt = nil
			}
			if mode == "duplicate group" {
				data.Passage.Groups = append(data.Passage.Groups, group)
			}
			if mode == "null aboard" {
				r.snapshot["passenger_aboard"] = encode(map[string]any{in.Ship.Ship.ID: nil})
			}
			if mode == "duplicate terminal" {
				for range 2 {
					if err := r.db.RecordEvent(h.ctx, h.account.ID, uuid.NewString(), "passengers_paid", encode(map[string]any{"timestamp": in.Now, "ship_id": in.Ship.Ship.ID, "group_id": group.ID, "fare_paid_pence": 1000})); err != nil {
						t.Fatal(err)
					}
				}
			}
			if err := r.saveMission(h.ctx, mission, &data); err != nil {
				t.Fatal(err)
			}
			for range 3 {
				if _, err := r.driveMission(h.ctx, in.Ship, mission, &data); err != nil {
					t.Fatalf("passenger corruption stopped runner: %v", err)
				}
				if data.Passage.QuarantineReason != "" {
					break
				}
			}
			_, restored := h.mission(in.Ship.Ship.ID)
			if restored.Passage.QuarantineReason == "" || restored.Passage.ReceiptsPrepared {
				t.Fatalf("bad batch not durably quarantined: %+v", restored.Passage)
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
			treasuryData := missionData{CooldownUntil: time.Now().Add(time.Minute)}
			if err := r.saveMission(h.ctx, treasury, &treasuryData); err != nil {
				t.Fatal(err)
			}
			for _, key := range []string{"ships", "contracts", "vaults", "clock", "catalog", "factors", "warehouses", "orders", "fills", "ledger", "rates", "settings"} {
				r.meta[key] = r.meta["zones"]
			}
			if changed, err := r.work(h.ctx); err != nil || !changed {
				t.Fatalf("quarantined passage blocked account work: %t %v", changed, err)
			}
			h.missingMission(other.Ship.ID)
			history, err := r.db.History(h.ctx, h.account.ID, "decisions", 20)
			if err != nil {
				t.Fatal(err)
			}
			raw, _ := json.Marshal(history)
			var rows []store.Decision
			if json.Unmarshal(raw, &rows) != nil {
				t.Fatalf("invalid decision history: %s", raw)
			}
			found := false
			for _, row := range rows {
				found = found || row.Kind == "passage_quarantined" && row.ShipID == in.Ship.Ship.ID
			}
			if !found {
				t.Fatalf("no durable quarantine decision: %s", raw)
			}
		})
	}
}

func TestPassengerDelayedUntaggedPaymentCannotPayNewVoyage(t *testing.T) {
	h, r, in := passengerRunnerFixture(t)
	boarded, promised := in.Now.Add(-time.Minute), in.Now.Add(time.Hour)
	group := gameapi.PassengerAboardGroup{ID: "new-group", Kind: "travellers", Size: 10, HoldTons: 10, OriginPortID: "origin", DestPortID: "destination", FarePerHeadPence: 100, FullFarePence: 1000, BoardedAt: &boarded, PromisedArrivalAt: &promised}
	mission, data := r.newVoyage(in.Ship, Plan{Kind: "passage", ShipID: in.Ship.Ship.ID, Origin: "origin", Destination: "destination"})
	data.StartedAt = boarded
	data.Passage = &passageIntent{Groups: []gameapi.PassengerAboardGroup{group}, FirstBoardedAt: boarded}
	for _, payload := range []map[string]any{
		{"timestamp": boarded.Add(-time.Minute), "ship_id": in.Ship.Ship.ID, "fare_paid_pence": 9000},
		{"ship_id": in.Ship.Ship.ID, "fare_paid_pence": 8000},
	} {
		if err := r.db.RecordEvent(h.ctx, h.account.ID, uuid.NewString(), "passengers_paid", encode(payload)); err != nil {
			t.Fatal(err)
		}
	}
	for range 2 {
		if _, err := r.syncPassage(h.ctx, in.Ship, mission, &data); err != nil {
			t.Fatal(err)
		}
	}
	if data.Passage.ReceiptsPrepared || len(data.Passage.Receipts) != 0 {
		t.Fatalf("old/untimed untagged payment funded new voyage: %+v", data.Passage.Receipts)
	}
	rows, err := r.db.SettlementHistory(h.ctx, h.account.ID, "origin|destination", 20)
	if err != nil || len(rows) != 0 {
		t.Fatalf("delayed payment was booked: %+v %v", rows, err)
	}
}

func TestPassengerPreparedSettlementConflictIsQuarantined(t *testing.T) {
	h, r, in := passengerRunnerFixture(t)
	mission, data := r.newVoyage(in.Ship, Plan{Kind: "passage", ShipID: in.Ship.Ship.ID, Origin: "origin", Destination: "destination"})
	boarded := in.Now.Add(-time.Minute)
	group := gameapi.PassengerAboardGroup{ID: "group", Size: 10, HoldTons: 10, BoardedAt: &boarded, DestPortID: "destination"}
	// An invalid prepared receipt models a journal/store proof conflict. The
	// original private accounting remains retained for repair, not suppressed.
	data.Passage = &passageIntent{Groups: []gameapi.PassengerAboardGroup{group}, ReceiptsPrepared: true, Receipts: []*store.Settlement{{ID: uuid.NewString(), AccountID: h.account.ID, ShipID: in.Ship.Ship.ID, MissionID: mission.Generation, Kind: "passage", EventID: "missing-proof", Lane: "origin|destination", Origin: "origin", Destination: "destination", StartedAt: boarded, CompletedAt: in.Now}}}
	if err := r.saveMission(h.ctx, mission, &data); err != nil {
		t.Fatal(err)
	}
	if _, err := r.driveMission(h.ctx, in.Ship, mission, &data); err != nil {
		t.Fatalf("receipt write failure stopped account: %v", err)
	}
	_, restored := h.mission(in.Ship.Ship.ID)
	if restored.Passage.QuarantineReason == "" || len(restored.Passage.Receipts) != 1 {
		t.Fatalf("conflicting receipt lost or not quarantined: %+v", restored.Passage)
	}
}

func TestPassengerJournalConflictQuarantinesWithoutReplaying(t *testing.T) {
	h, r, in := passengerRunnerFixture(t)
	if _, err := r.considerPassage(h.ctx, in.Ship, in, nil); err != nil {
		t.Fatal(err)
	}
	passengerTick(t, h, r)
	mission, data := h.mission(in.Ship.Ship.ID)
	action := data.Action
	if _, err := r.db.BeginAction(h.ctx, &store.Action{ID: action.ID, AccountID: h.account.ID, Method: action.Method, Path: action.Path, Body: json.RawMessage(`{"different":"immutable body"}`)}); err != nil {
		t.Fatal(err)
	}
	h.api.server.Config.Handler = http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		t.Errorf("conflicting journal authorized transport: %s %s", req.Method, req.URL)
		w.WriteHeader(http.StatusServiceUnavailable)
	})
	if changed, err := r.dispatch(h.ctx, mission, &data); err != nil || changed {
		t.Fatalf("journal conflict stopped account: %t %v", changed, err)
	}
	_, restored := h.mission(in.Ship.Ship.ID)
	if restored.Passage.QuarantineReason == "" || restored.Action == nil || restored.Action.ID != action.ID || len(h.pending()) != 1 {
		t.Fatalf("immutable conflict not quarantined with evidence retained: %+v", restored)
	}
}

func TestPassengerReplaySourceTimeSeparatesArchivedAndCurrentVoyages(t *testing.T) {
	for _, archivedFirst := range []bool{false, true} {
		t.Run(map[bool]string{false: "current first", true: "archive first"}[archivedFirst], func(t *testing.T) {
			h, r, ship, oldMission, oldData := passengerCompletionFixture(t)
			unknown := completePassengerRESTFallback(t, h, r, ship, oldMission, &oldData)
			boarded, promised := unknown.CompletedAt.Add(time.Nanosecond), unknown.CompletedAt.Add(time.Hour)
			group := gameapi.PassengerAboardGroup{ID: "replacement-group", Kind: "travellers", Size: 10, HoldTons: 10,
				OriginPortID: "origin", DestPortID: "destination", FarePerHeadPence: 200, FullFarePence: 2000,
				BoardedAt: &boarded, PromisedArrivalAt: &promised}
			mission, data := r.newVoyage(ship, Plan{Kind: "passage", ShipID: ship.Ship.ID, Origin: "origin", Destination: "destination"})
			data.StartedAt = boarded
			data.Passage = &passageIntent{Groups: []gameapi.PassengerAboardGroup{group}, FirstBoardedAt: boarded}
			if err := r.saveMission(h.ctx, mission, &data); err != nil {
				t.Fatal(err)
			}
			recordPassengerCompletionEvent(t, h, "replayed-old-payment", "passengers_paid", "", unknown.CompletedAt, 1800)
			current := func() {
				for range 2 {
					if _, err := r.syncPassage(h.ctx, ship, mission, &data); err != nil {
						t.Fatal(err)
					}
				}
				if data.Passage.ReceiptsPrepared || data.Passage.QuarantineReason != "" {
					t.Fatalf("old event attributed to newer boarding: %+v", data.Passage)
				}
			}
			archive := func() {
				if changed, err := r.reconcilePendingPassengerReceipts(h.ctx); err != nil || !changed {
					t.Fatalf("old payment failed to upgrade only its archive: %t %v", changed, err)
				}
			}
			if archivedFirst {
				archive()
				current()
			} else {
				current()
				archive()
			}
			rows, err := h.db.SettlementHistory(h.ctx, h.account.ID, "origin|destination", 10)
			if err != nil || len(rows) != 1 || rows[0].ID != unknown.ID || rows[0].ProceedsUnknown || rows[0].ProceedsPence != 1800 {
				t.Fatalf("one source event paid two voyages: %+v %v", rows, err)
			}
		})
	}
}
