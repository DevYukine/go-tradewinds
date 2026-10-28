package engine

import (
	"context"
	"encoding/json"
	"net/http"
	"testing"
	"time"

	"github.com/DevYukine/go-tradewinds/internal/gameapi"
	"github.com/DevYukine/go-tradewinds/internal/store"
	"github.com/google/uuid"
)

func passengerWorkReady(t *testing.T, h *recoveryHarness, r *Runner) {
	t.Helper()
	treasury := &store.Mission{AccountID: h.account.ID, ShipID: "__treasury", Phase: "settings", Generation: uuid.NewString()}
	data := missionData{CooldownUntil: time.Now().Add(time.Hour)}
	if err := r.saveMission(h.ctx, treasury, &data); err != nil {
		t.Fatal(err)
	}
	for _, key := range []string{"ships", "contracts", "vaults", "clock", "catalog", "factors", "warehouses", "orders", "fills", "ledger", "rates", "settings"} {
		r.meta[key] = r.meta["zones"]
	}
	r.snapshot["loans"] = encode([]gameapi.Loan{})
}

func TestPassengerWorkQuarantineHintSurvivesArchiveFailureAndRepairsStaleCapture(t *testing.T) {
	h, r, in := passengerRunnerFixture(t)
	passengerWorkReady(t, h, r)
	mission, data := r.newVoyage(in.Ship, Plan{Kind: "passage", ShipID: in.Ship.Ship.ID, Origin: "origin", Destination: "destination"})
	data.Passage = &passageIntent{Groups: []gameapi.PassengerAboardGroup{{ID: "missing-boarding-time"}}}
	if err := r.saveMission(h.ctx, mission, &data); err != nil {
		t.Fatal(err)
	}
	db := passengerFaultDatabase(t, h)
	if _, err := db.ExecContext(h.ctx, "ALTER TABLE passenger_quarantine_archives RENAME TO passenger_fault_quarantines"); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_, _ = db.ExecContext(context.Background(), "ALTER TABLE IF EXISTS passenger_fault_quarantines RENAME TO passenger_quarantine_archives")
	})
	if _, err := r.work(h.ctx); err != nil {
		t.Fatal(err)
	}
	snapshot, err := h.db.Snapshot(h.ctx, h.account.ID)
	if err != nil || !decode[map[string]bool](snapshot, "_passenger_boarding_ships")[in.Ship.Ship.ID] {
		t.Fatalf("archive failure lost durable boarding hint: %v %s", err, snapshot["_passenger_boarding_ships"])
	}
	_, retained := h.mission(in.Ship.Ship.ID)
	if retained.Passage.QuarantineReason == "" {
		t.Fatal("integrity failure was not quarantined")
	}
	// Simulate a restored quarantine predating the hint and an expired capture.
	delete(r.snapshot, "_passenger_boarding_ships")
	delete(r.meta, "passenger_aboard/ship-a")
	if err := h.db.SaveSnapshot(h.ctx, h.account.ID, r.snapshot); err != nil {
		t.Fatal(err)
	}
	if _, err := r.work(h.ctx); err != nil {
		t.Fatal(err)
	}
	snapshot, err = h.db.Snapshot(h.ctx, h.account.ID)
	if err != nil || !decode[map[string]bool](snapshot, "_passenger_boarding_ships")[in.Ship.Ship.ID] {
		t.Fatalf("stale quarantine failed to repair hint: %v", err)
	}
	aboardReads := 0
	h.api.server.Config.Handler = http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		if req.Method != http.MethodGet {
			t.Errorf("unexpected mutation: %s %s", req.Method, req.URL)
			w.WriteHeader(500)
			return
		}
		if req.URL.Path == "/v1/passengers/aboard" {
			aboardReads++
		}
		_ = json.NewEncoder(w).Encode([]any{})
	})
	r.snapshot["ships"] = encode(r.stateCache.Ships)
	r.snapshot["clock"] = encode(r.stateCache.Clock)
	if err := r.refreshPassengers(h.ctx); err != nil {
		t.Fatal(err)
	}
	if aboardReads != 1 || !r.fresh("passenger_aboard/ship-a") {
		t.Fatalf("hint did not restore authoritative empty capture: reads=%d meta=%+v", aboardReads, r.meta["passenger_aboard/ship-a"])
	}
	if _, err := db.ExecContext(h.ctx, "ALTER TABLE passenger_fault_quarantines RENAME TO passenger_quarantine_archives"); err != nil {
		t.Fatal(err)
	}
	if _, err := r.work(h.ctx); err != nil {
		t.Fatal(err)
	}
	h.missingMission(in.Ship.Ship.ID)
}

func TestPassengerWorkRetirementFailureAllowsDebtServiceAndCreditOnlyDelivery(t *testing.T) {
	for _, serviceLoan := range []bool{true, false} {
		t.Run(map[bool]string{true: "debt service", false: "credit delivery"}[serviceLoan], func(t *testing.T) {
			h, r, in := passengerRunnerFixture(t)
			passengerWorkReady(t, h, r)
			mission, data := r.newVoyage(in.Ship, Plan{Kind: "passage", ShipID: in.Ship.Ship.ID, Origin: "origin", Destination: "destination"})
			data.Passage = &passageIntent{QuarantineReason: "restored integrity conflict"}
			if err := r.stage(h.ctx, mission, &data, in.Ship, "passenger-offer", "/v1/passengers/offer", map[string]string{"ship_id": in.Ship.Ship.ID}); err != nil {
				t.Fatal(err)
			}
			action := data.Action
			if _, err := h.db.BeginAction(h.ctx, &store.Action{ID: action.ID, AccountID: h.account.ID, Method: action.Method, Path: action.Path, Body: action.Body}); err != nil {
				t.Fatal(err)
			}
			db := passengerFaultDatabase(t, h)
			if _, err := db.ExecContext(h.ctx, "ALTER TABLE passenger_quarantine_archives RENAME TO passenger_fault_quarantines"); err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() {
				_, _ = db.ExecContext(context.Background(), "ALTER TABLE IF EXISTS passenger_fault_quarantines RENAME TO passenger_quarantine_archives")
			})
			ordinary := in.Ship
			ordinary.Ship.ID = "non-credit-hull"
			ordinaryMission, ordinaryData := r.newVoyage(ordinary, Plan{Kind: "passage", ShipID: ordinary.Ship.ID, Origin: "origin", Destination: "destination"})
			ordinaryData.Passage = &passageIntent{Abandon: true}
			if err := r.saveMission(h.ctx, ordinaryMission, &ordinaryData); err != nil {
				t.Fatal(err)
			}
			r.stateCache.Ships = append(r.stateCache.Ships, ordinary)
			other := in.Ship
			other.Ship.ID = "credit-hull"
			other.CargoManifest = map[string]int64{"provisions": 1}
			other.CargoUsedTons = 1
			r.stateCache.Ships = append(r.stateCache.Ships, other)
			otherMission, otherData := r.newVoyage(other, Plan{ShipID: other.Ship.ID, Origin: "destination", Destination: "origin"})
			otherMission.Phase = "arrival"
			otherData.Eligible = map[string]int64{"provisions": 1}
			if err := r.saveMission(h.ctx, otherMission, &otherData); err != nil {
				t.Fatal(err)
			}
			r.stateCache.Contracts = []Contract{{ID: "credit-charter", ContractType: "charter", Status: "open", OriginPortID: "origin", CommodityID: "provisions", RemainingQuantity: 1, RewardPerTonPence: new(int64(1000))}}
			if serviceLoan {
				control := &store.Mission{AccountID: h.account.ID, ShipID: "__loans", Phase: "loan", Generation: uuid.NewString()}
				if err := r.saveMission(h.ctx, control, &missionData{}); err != nil {
					t.Fatal(err)
				}
			}
			if changed, err := r.work(h.ctx); err != nil || !changed {
				t.Fatalf("retirement failure froze other work: %t %v", changed, err)
			}
			if serviceLoan {
				h.missingMission("__loans")
			} else {
				_, credit := h.mission(other.Ship.ID)
				if credit.Action == nil || credit.Action.Stage != "deliver-charter" {
					t.Fatalf("credit-only delivery not reached: %+v", credit.Action)
				}
			}
			h.mission(ordinary.Ship.ID)
			_, retained := h.mission(in.Ship.Ship.ID)
			if retained.Action == nil || retained.Action.ID != action.ID || len(h.pending()) != 1 {
				t.Fatal("failed retirement discarded unresolved request")
			}
			found := false
			for _, decision := range passengerDecisionRows(t, h) {
				if decision.ShipID == in.Ship.Ship.ID && decision.Kind == "hold" {
					found = true
				}
			}
			if !found {
				t.Fatal("retirement failure was not visibly held")
			}
		})
	}
}

func TestPassengerWorkQuarantineSubtractsAlreadySavedReceiptCosts(t *testing.T) {
	for _, unknown := range []bool{false, true} {
		t.Run(map[bool]string{false: "partial prepared receipts", true: "unknown REST receipt"}[unknown], func(t *testing.T) {
			h, r, in := passengerRunnerFixture(t)
			passengerWorkReady(t, h, r)
			mission, data := r.newVoyage(in.Ship, Plan{Kind: "passage", ShipID: in.Ship.Ship.ID, Origin: "origin", Destination: "destination"})
			data.AcquisitionPence, data.FeesPence, data.ProvisionsPence, data.ExpensesPence = 10, 20, 100, 30
			receipt := &store.Settlement{ID: uuid.NewString(), AccountID: h.account.ID, ShipID: in.Ship.Ship.ID, MissionID: mission.Generation, Kind: "passage", Lane: "origin|destination", Origin: "origin", Destination: "destination", StartedAt: data.StartedAt, CompletedAt: time.Now().UTC(), AcquisitionPence: 5, FeesPence: 10, ProvisionsPence: 50, ExpensesPence: 15, EventID: "partial-paid", CostsKnown: true, ProceedsPence: 200}
			recordPassengerCompletionEvent(t, h, receipt.EventID, "passengers_paid", "partial-group", receipt.CompletedAt, receipt.ProceedsPence)
			if unknown {
				// Use the real pending-receipt path: no invented zero-fare event.
				h2, r2, ship2, m2, d2 := passengerCompletionFixture(t)
				saved := completePassengerRESTFallback(t, h2, r2, ship2, m2, &d2)
				h, r, in.Ship, mission, data = h2, r2, ship2, m2, d2
				if err := h.db.DeferPassengerReceipt(h.ctx, h.account.ID, saved.ID, time.Now().UTC()); err != nil {
					t.Fatal(err)
				}
				passengerWorkReady(t, h, r)
				r.stateCache.Clock.Paused = false
				receipt = &saved
				data.Passage.QuarantineReason = "unknown receipt subsequently quarantined"
			} else {
				bad := *receipt
				bad.ID, bad.EventID = uuid.NewString(), "missing-terminal-proof"
				data.Passage = &passageIntent{ReceiptsPrepared: true, Receipts: []*store.Settlement{receipt, &bad}}
			}
			if err := r.saveMission(h.ctx, mission, &data); err != nil {
				t.Fatal(err)
			}
			if _, err := r.work(h.ctx); err != nil {
				t.Fatal(err)
			}
			markerID := ActionID(h.account.ID, mission.Generation, "passenger-quarantine-accounting", 0)
			markers, err := h.db.SettlementsByID(h.ctx, h.account.ID, []string{markerID})
			if err != nil {
				t.Fatal(err)
			}
			marker := markers[0]
			if marker.AcquisitionPence != max(0, data.AcquisitionPence-receipt.AcquisitionPence) || marker.FeesPence != max(0, data.FeesPence-receipt.FeesPence) || marker.ProvisionsPence != max(0, data.ProvisionsPence-receipt.ProvisionsPence) || marker.ExpensesPence != max(0, data.ExpensesPence-receipt.ExpensesPence) {
				t.Fatalf("quarantine duplicated carried costs: %+v", marker)
			}
			measured, err := h.db.EconomicHistoryWindow(h.ctx, h.account.ID, "", time.Now().UTC(), 24*time.Hour)
			expectedLoss := int64(-80)
			if unknown {
				expectedLoss = -150
			}
			if err != nil || !measured.UnknownCosts || measured.UnknownLossPence != expectedLoss {
				t.Fatalf("unknown loss double counted receipts: %+v %v", measured, err)
			}
			// Repeat via normal work; the frozen marker must remain idempotent.
			if _, err := r.work(h.ctx); err != nil {
				t.Fatal(err)
			}
			replay, err := h.db.SettlementsByID(h.ctx, h.account.ID, []string{markerID})
			if err != nil || replay[0].AcquisitionPence != marker.AcquisitionPence || replay[0].FeesPence != marker.FeesPence || replay[0].ProvisionsPence != marker.ProvisionsPence || replay[0].ExpensesPence != marker.ExpensesPence {
				t.Fatalf("replay changed residual costs: %+v %v", replay, err)
			}
		})
	}
}
