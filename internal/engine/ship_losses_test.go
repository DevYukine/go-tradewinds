package engine

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/DevYukine/go-tradewinds/internal/store"
	"github.com/google/uuid"
	"go.uber.org/zap"
	"go.uber.org/zap/zaptest/observer"
)

func ownedLossRow(row map[string]any) map[string]any {
	row["ship"].(map[string]any)["company_id"] = recoveryCompanyID
	return row
}

func freshLossRunner(t *testing.T, h *recoveryHarness) *Runner {
	t.Helper()
	h.api.setFleet([]map[string]any{})
	runner := h.runner(primaryTestKey)
	refreshLossFleet(t, h, runner)
	// Direct-capture cases also exercise bootstrap from a legacy snapshot.
	delete(runner.snapshot, "_ship_loss_verified_ships")
	return runner
}

func refreshLossFleet(t *testing.T, h *recoveryHarness, runner *Runner) {
	t.Helper()
	runner.generation = uuid.NewString()
	if err := runner.refresh(h.ctx); err != nil {
		t.Fatalf("refresh fleet: %v", err)
	}
}

func recordedShipLoss(t *testing.T, h *recoveryHarness, shipID string) store.ShipLoss {
	t.Helper()
	rows, err := h.db.ShipLosses(h.ctx, h.account.ID)
	if err != nil {
		t.Fatal(err)
	}
	for _, row := range rows {
		if row.ShipID == shipID {
			return row
		}
	}
	t.Fatalf("ship loss %s was not recorded: %+v", shipID, rows)
	return store.ShipLoss{}
}

func shipLossEventRecord(t *testing.T, h *recoveryHarness, id, kind string, at time.Time, payload any) {
	t.Helper()
	raw := encode(map[string]any{"company_id": h.account.CompanyID, "timestamp": at, "payload": payload})
	if err := h.db.RecordEvent(h.ctx, h.account.ID, id, kind, raw); err != nil {
		t.Fatal(err)
	}
}

func TestShipLossFirstObservationPreservesLastAfloatFactsAcrossRestart(t *testing.T) {
	h := newRecoveryHarness(t)
	afloat := ownedLossRow(sailingShipRow(recoveryShipBlastoise, "Blastoise", recoveryHomePort, recoveryFarPort))
	afloat["ship"].(map[string]any)["current_leg_idx"] = 1
	afloat["ship"].(map[string]any)["current_port_id"] = nil
	afloat["cargo_manifest"] = map[string]int64{"grain": 9, "provisions": 2}
	afloat["cargo_used_tons"] = 11
	afloat["stores"] = map[string]any{"provisions_tons": 1.75, "sea_days_remaining": 8.5}
	afloat["route_legs"] = []any{
		map[string]any{"from_node": recoveryHomePort, "to_node": "zone-north", "distance_nm": 12, "edge_class": "coastal"},
		map[string]any{"from_node": "zone-north", "to_node": recoveryFarPort, "distance_nm": 12, "edge_class": "coastal"},
	}
	h.api.setFleet([]map[string]any{afloat})
	runner := h.runner(primaryTestKey)
	refreshLossFleet(t, h, runner)
	lastObserved := runner.meta["ship_details/"+recoveryShipBlastoise].UpdatedAt

	lost := ownedLossRow(sunkShipRow(recoveryShipBlastoise, "Blastoise"))
	lost["ship"].(map[string]any)["fate"] = "lost_at_sea"
	h.api.setFleet([]map[string]any{lost})
	shipLossEventRecord(t, h, "terminal-storm", "voyage_hazard", time.Now().UTC(), map[string]any{
		"ship_id": recoveryShipBlastoise, "hazard": "storm", "sunk": true, "captured": false,
	})
	before := time.Now().UTC().Add(-time.Microsecond)
	refreshLossFleet(t, h, runner)
	first := recordedShipLoss(t, h, recoveryShipBlastoise)
	if first.ObservedAt.Before(before) || first.ObservedAt.After(time.Now().UTC()) || first.Fate != "lost_at_sea" || first.Status != "sunk" || first.ShipName != "Blastoise" || first.ShipClass != recoveryHull {
		t.Fatalf("first observed loss was not recorded on this pass: %+v", first)
	}
	// PostgreSQL timestamps retain microsecond, not capture-clock, precision.
	if first.LastObservedAt == nil || first.LastObservedAt.Sub(lastObserved).Abs() > time.Microsecond || first.LastPortID != nil || first.LastSeaZoneID == nil || *first.LastSeaZoneID != "zone-north" || first.VoyageOriginID == nil || *first.VoyageOriginID != recoveryHomePort || first.VoyageDestinationID == nil || *first.VoyageDestinationID != recoveryFarPort {
		t.Fatalf("last afloat time or location disappeared: want time=%s, loss=%s", lastObserved, encode(first))
	}
	var cargo map[string]int64
	var stores struct {
		ProvisionsTons   float64 `json:"provisions_tons"`
		SeaDaysRemaining float64 `json:"sea_days_remaining"`
	}
	if json.Unmarshal(first.CargoManifest, &cargo) != nil || !reflect.DeepEqual(cargo, map[string]int64{"grain": 9, "provisions": 2}) || json.Unmarshal(first.Stores, &stores) != nil || stores.ProvisionsTons != 1.75 || stores.SeaDaysRemaining != 8.5 {
		t.Fatalf("last observed cargo/stores replaced by the empty sunk hold: cargo=%s stores=%s", first.CargoManifest, first.Stores)
	}
	if first.HazardCause == nil || *first.HazardCause != "storm" || first.InsuranceClaimPaidPence != nil {
		t.Fatalf("loss cause or unknown claim changed: %+v", first)
	}

	refreshLossFleet(t, h, runner)
	runner = h.runner(primaryTestKey)
	refreshLossFleet(t, h, runner)
	again := recordedShipLoss(t, h, recoveryShipBlastoise)
	if !reflect.DeepEqual(first, again) {
		t.Fatalf("repeated sunk observations or restart overwrote loss evidence:\nfirst=%+v\nagain=%+v", first, again)
	}
	rows, err := h.db.ShipLosses(h.ctx, h.account.ID)
	if err != nil || len(rows) != 1 {
		t.Fatalf("duplicate loss records: %+v, %v", rows, err)
	}

	shipLossEventRecord(t, h, "claim-closed", "insurance_policy_closed", time.Now().UTC(), map[string]any{
		"ship_id": recoveryShipBlastoise, "insured_company_id": h.account.CompanyID, "status": "claimed_loss", "loss_bps": 10_000, "claim_paid_pence": 123_456,
	})
	h.api.setFleet([]map[string]any{})
	refreshLossFleet(t, h, runner)
	paid := recordedShipLoss(t, h, recoveryShipBlastoise)
	if paid.InsuranceClaimPaidPence == nil || *paid.InsuranceClaimPaidPence != 123_456 || !paid.ObservedAt.Equal(first.ObservedAt) || !reflect.DeepEqual(paid.CargoManifest, first.CargoManifest) {
		t.Fatalf("later terminal claim did not enrich the missing hull without replacing its evidence: %+v", paid)
	}
}

func TestShipLossMissingHullRequiresTwoNonemptyFleetCapturesFiveRealMinutesApart(t *testing.T) {
	h := newRecoveryHarness(t)
	runner := freshLossRunner(t, h)
	start := time.Now().UTC().Truncate(time.Microsecond).Add(-time.Hour)
	afloat := ownedLossRow(sailingShipRow(recoveryShipBlastoise, "Blastoise", recoveryHomePort, recoveryFarPort))
	runner.observeShipLosses(h.ctx, "ships", encode([]any{afloat}), start)
	now := start.Add(time.Minute)
	if runner.applyCapture(h.ctx, "ships", fetchResult{err: errors.New("fleet unavailable"), attemptedAt: now, completedAt: now}) {
		t.Fatal("failed fleet response was accepted")
	}
	peer := ownedLossRow(sailingShipRow(recoveryShipLapras, "Lapras", recoveryHomePort, recoveryFarPort))
	runner.observeShipLosses(h.ctx, "ships", encode([]any{peer}), now)
	runner.observeShipLosses(h.ctx, "ships", encode([]any{peer}), now.Add(4*time.Minute))
	rows, err := h.db.ShipLosses(h.ctx, h.account.ID)
	if err != nil || len(rows) != 0 {
		t.Fatalf("unconfirmed absence invented a missing hull: %+v, %v", rows, err)
	}
	runner.observeShipLosses(h.ctx, "ships", json.RawMessage(`[]`), now.Add(5*time.Minute))
	runner.observeShipLosses(h.ctx, "ships", encode([]any{peer}), now.Add(6*time.Minute))
	rows, err = h.db.ShipLosses(h.ctx, h.account.ID)
	if err != nil || len(rows) != 0 {
		t.Fatalf("empty fleet counted as absence confirmation: %+v, %v", rows, err)
	}
	confirmedAt := now.Add(11 * time.Minute)
	runner.observeShipLosses(h.ctx, "ships", encode([]any{peer}), confirmedAt)
	loss := recordedShipLoss(t, h, recoveryShipBlastoise)
	if loss.Fate != "missing" || loss.ShipName != "Blastoise" || loss.LastObservedAt == nil || !loss.LastObservedAt.Equal(start) ||
		loss.CargoManifest == nil || loss.InsuranceClaimPaidPence != nil || !loss.ObservedAt.Equal(confirmedAt.Truncate(time.Microsecond)) {
		t.Fatalf("confirmed vanished hull lost its known identity or invented a claim: %+v", loss)
	}
	unknown := ownedLossRow(sunkShipRow("never-observed-afloat", "MissingNo"))
	unknown["ship"].(map[string]any)["fate"] = "captured"
	runner.observeShipLosses(h.ctx, "ships", encode([]any{peer, unknown}), confirmedAt.Add(time.Second))
	captured := recordedShipLoss(t, h, "never-observed-afloat")
	if captured.Fate != "captured" || captured.LastObservedAt != nil || captured.CargoManifest != nil || captured.Stores != nil || captured.LastPortID != nil || captured.LastSeaZoneID != nil {
		t.Fatalf("first observed already-lost hull fabricated afloat facts: %+v", captured)
	}
}

func TestShipLossDetailCaptureUsesNewerAfloatObservation(t *testing.T) {
	h := newRecoveryHarness(t)
	runner := freshLossRunner(t, h)
	at := time.Now().UTC().Add(-time.Minute)
	list := ownedLossRow(sailingShipRow(recoveryShipBlastoise, "Blastoise", recoveryHomePort, recoveryFarPort))
	list["cargo_manifest"] = map[string]int64{"grain": 1}
	child := ownedLossRow(sailingShipRow(recoveryShipBlastoise, "Blastoise", recoveryHomePort, recoveryFarPort))
	child["cargo_manifest"] = map[string]int64{"grain": 7}
	runner.snapshot["ships"] = encode([]any{list})
	runner.snapshot["ship_details"] = encode(map[string]any{recoveryShipBlastoise: child})
	runner.meta["ships"] = DatasetMeta{UpdatedAt: at}
	runner.meta["ship_details/"+recoveryShipBlastoise] = DatasetMeta{UpdatedAt: at.Add(time.Second)}
	lost := ownedLossRow(sunkShipRow(recoveryShipBlastoise, "Blastoise"))
	lost["ship"].(map[string]any)["fate"] = "lost_at_sea"
	if !runner.applyCapture(h.ctx, "ship_details/"+recoveryShipBlastoise, fetchResult{data: encode(lost), attemptedAt: at.Add(time.Minute), completedAt: at.Add(time.Minute)}) {
		t.Fatal("lost child observation was not captured")
	}
	loss := recordedShipLoss(t, h, recoveryShipBlastoise)
	var cargo map[string]int64
	if json.Unmarshal(loss.CargoManifest, &cargo) != nil || cargo["grain"] != 7 {
		t.Fatalf("older list replaced newer last afloat cargo: %s", loss.CargoManifest)
	}
}

func TestShipLossRecordingFailureWarnsWithoutBlockingAndRetriesAfterRestart(t *testing.T) {
	h := newRecoveryHarness(t)
	h.api.setFleet([]map[string]any{ownedLossRow(sailingShipRow(recoveryShipBlastoise, "Blastoise", recoveryHomePort, recoveryFarPort))})
	runner := h.runner(primaryTestKey)
	refreshLossFleet(t, h, runner)
	admin, err := sql.Open("pgx", h.dsn)
	if err != nil {
		t.Fatal(err)
	}
	defer admin.Close()
	if _, err := admin.ExecContext(h.ctx, "ALTER TABLE ship_losses RENAME TO ship_losses_unavailable"); err != nil {
		t.Fatal(err)
	}
	core, logs := observer.New(zap.WarnLevel)
	runner.log = zap.New(core)
	lost := ownedLossRow(sunkShipRow(recoveryShipBlastoise, "Blastoise"))
	h.api.setFleet([]map[string]any{lost})
	refreshLossFleet(t, h, runner)
	if logs.FilterMessage("ship loss history not recorded").Len() < 1 || len(runner.stateCache.Ships) != 1 || runner.stateCache.Ships[0].Ship.Status != "sunk" {
		t.Fatalf("recording failure blocked the fresh fleet or lacked a warning: %v", logs.All())
	}
	persisted, err := h.db.Snapshot(h.ctx, h.account.ID)
	if err != nil {
		t.Fatal(err)
	}
	queued := decode[map[string]store.ShipLoss](persisted, "_pending_ship_losses")[recoveryShipBlastoise]
	if queued.ShipID == "" || queued.LastObservedAt == nil || queued.Stores == nil {
		t.Fatalf("recording failure discarded first observation evidence: %+v", queued)
	}
	if _, err := admin.ExecContext(h.ctx, "ALTER TABLE ship_losses_unavailable RENAME TO ship_losses"); err != nil {
		t.Fatal(err)
	}
	runner = h.runner(primaryTestKey)
	refreshLossFleet(t, h, runner)
	runner.observeShipLosses(h.ctx, "ships", encode([]any{lost}), queued.ObservedAt.Add(15*time.Minute))
	loss := recordedShipLoss(t, h, recoveryShipBlastoise)
	if loss.ObservedAt.Sub(queued.ObservedAt).Abs() > time.Microsecond || loss.LastObservedAt == nil || loss.LastObservedAt.Sub(*queued.LastObservedAt).Abs() > time.Microsecond || !reflect.DeepEqual(loss.Stores, queued.Stores) {
		t.Fatalf("retry after restart lost original observation: queued=%+v, saved=%+v", queued, loss)
	}
	if _, exists := runner.snapshot["_pending_ship_losses"]; exists {
		t.Fatal("successful retry retained a pending loss")
	}
}

func TestShipLossEvidenceRejectsPartialForeignEarlierVoyageAndInvalidClaims(t *testing.T) {
	runner := New(nil, nil, store.Account{CompanyID: recoveryCompanyID}, zap.NewNop(), Options{})
	lastObserved := time.Now().UTC().Add(-time.Minute)
	observedAt := lastObserved.Add(30 * time.Second)
	origin, destination := recoveryHomePort, recoveryFarPort
	losses := map[string]store.ShipLoss{"lost": {
		ShipID: "lost", Fate: "lost_at_sea", ObservedAt: observedAt, LastObservedAt: &lastObserved,
		VoyageOriginID: &origin, VoyageDestinationID: &destination,
	}}
	changes := make(map[string]store.ShipLoss)
	event := func(kind string, at time.Time, payload any) store.Event {
		return store.Event{EventType: kind, CreatedAt: at, Payload: encode(map[string]any{"payload": payload})}
	}
	for _, payload := range []map[string]any{
		{"ship_id": "lost", "insured_company_id": recoveryCompanyID, "status": "active", "loss_bps": 10_000, "claim_paid_pence": 100},
		{"ship_id": "lost", "insured_company_id": recoveryCompanyID, "status": "claimed_loss", "loss_bps": 387, "claim_paid_pence": 100},
		{"ship_id": "lost", "insured_company_id": "another-company", "status": "claimed_loss", "loss_bps": 10_000, "claim_paid_pence": 200},
		{"ship_id": "other-hull", "insured_company_id": recoveryCompanyID, "status": "claimed_loss", "loss_bps": 10_000, "claim_paid_pence": 300},
		{"ship_id": "lost", "insured_company_id": recoveryCompanyID, "status": "settled_safe", "loss_bps": 10_000, "claim_paid_pence": 400},
		{"ship_id": "lost", "insured_company_id": recoveryCompanyID, "status": "claimed_loss", "loss_bps": 10_000, "claim_paid_pence": -1},
		{"ship_id": "lost", "insured_company_id": recoveryCompanyID, "status": "claimed_loss", "loss_bps": 10_000, "claim_paid_pence": 500, "origin_port_id": "old-origin", "dest_port_id": "old-destination"},
	} {
		runner.enrichShipLosses(context.Background(), losses, changes, []store.Event{event("insurance_policy_closed", observedAt.Add(time.Second), payload)})
	}
	fullClaim := map[string]any{
		"ship_id": "lost", "insured_company_id": recoveryCompanyID, "status": "claimed_loss", "loss_bps": 10_000,
		"claim_paid_pence": 600, "origin_port_id": origin, "dest_port_id": destination,
	}
	runner.enrichShipLosses(context.Background(), losses, changes, []store.Event{
		event("insurance_policy_closed", lastObserved.Add(-time.Hour), fullClaim),
		event("voyage_hazard", observedAt.Add(time.Second), map[string]any{"ship_id": "lost", "hazard": "storm", "sunk": false}),
		event("voyage_hazard", observedAt.Add(time.Second), map[string]any{"ship_id": "lost", "hazard": "pirates", "captured": true}),
	})
	if len(changes) != 0 || losses["lost"].InsuranceClaimPaidPence != nil || losses["lost"].HazardCause != nil {
		t.Fatalf("unrelated, old or partial evidence fabricated a loss payment: %+v", losses["lost"])
	}
	runner.enrichShipLosses(context.Background(), losses, changes, []store.Event{
		event("insurance_policy_closed", observedAt.Add(time.Second), fullClaim),
		event("voyage_hazard", observedAt.Add(time.Second), map[string]any{"ship_id": "lost", "hazard": "storm", "sunk": true}),
	})
	loss := losses["lost"]
	if len(changes) != 1 || loss.InsuranceClaimPaidPence == nil || *loss.InsuranceClaimPaidPence != 600 || loss.HazardCause == nil || *loss.HazardCause != "storm" {
		t.Fatalf("matching total loss evidence was not applied: %+v", loss)
	}
}

func TestShipLossClaimReceivedBeforeTerminalCaptureRequiresCurrentVoyageEvidence(t *testing.T) {
	for _, matchingVoyage := range []bool{false, true} {
		t.Run(fmt.Sprintf("matching_voyage=%v", matchingVoyage), func(t *testing.T) {
			runner := New(nil, nil, store.Account{CompanyID: recoveryCompanyID}, zap.NewNop(), Options{})
			lastObserved := time.Now().UTC().Add(-time.Minute)
			origin, destination := recoveryHomePort, recoveryFarPort
			losses := map[string]store.ShipLoss{"lost": {
				ShipID: "lost", Fate: "lost_at_sea", LastObservedAt: &lastObserved,
				ObservedAt: lastObserved.Add(30 * time.Second), VoyageOriginID: &origin, VoyageDestinationID: &destination,
			}}
			payload := map[string]any{
				"ship_id": "lost", "insured_company_id": recoveryCompanyID, "status": "claimed_loss",
				"loss_bps": 10_000, "claim_paid_pence": 700,
			}
			if matchingVoyage {
				payload["origin_port_id"], payload["dest_port_id"] = origin, destination
			}
			changes := make(map[string]store.ShipLoss)
			runner.enrichShipLosses(context.Background(), losses, changes, []store.Event{{
				EventType: "insurance_policy_closed", CreatedAt: lastObserved.Add(time.Second),
				Payload: encode(map[string]any{"payload": payload}),
			}})
			if (losses["lost"].InsuranceClaimPaidPence != nil) != matchingVoyage {
				t.Fatalf("claim without current voyage authority was attributed to the hull: %s", encode(losses["lost"]))
			}
		})
	}
}

func TestShipLossRecordsFleetLargerThanOneWriteBatch(t *testing.T) {
	h := newRecoveryHarness(t)
	runner := freshLossRunner(t, h)
	ships := make([]map[string]any, 1001)
	for i := range ships {
		ships[i] = ownedLossRow(sunkShipRow(fmt.Sprintf("lost-hull-%d", i), fmt.Sprintf("Hull %d", i)))
		ships[i]["ship"].(map[string]any)["fate"] = "lost_at_sea"
	}
	runner.observeShipLosses(h.ctx, "ships", encode(ships), time.Now().UTC())
	rows, err := h.db.ShipLosses(h.ctx, h.account.ID)
	if err != nil || len(rows) != len(ships) {
		t.Fatalf("large fleet loss was only partially recorded: %d of %d rows, %v", len(rows), len(ships), err)
	}
}

func TestShipLossRecoveredMissingHullStartsFreshEpisode(t *testing.T) {
	for _, terminal := range []bool{false, true} {
		t.Run(fmt.Sprintf("terminal=%v", terminal), func(t *testing.T) {
			h := newRecoveryHarness(t)
			runner := freshLossRunner(t, h)
			start := time.Now().UTC().Truncate(time.Microsecond).Add(-time.Hour)
			afloat := ownedLossRow(sailingShipRow(recoveryShipBlastoise, "Blastoise", recoveryHomePort, recoveryFarPort))
			afloat["cargo_manifest"] = map[string]int64{"grain": 2}
			peer := ownedLossRow(sailingShipRow(recoveryShipLapras, "Lapras", recoveryHomePort, recoveryFarPort))
			runner.observeShipLosses(h.ctx, "ships", encode([]any{afloat, peer}), start)
			runner.observeShipLosses(h.ctx, "ships", encode([]any{peer}), start.Add(time.Minute))
			runner.observeShipLosses(h.ctx, "ships", encode([]any{peer}), start.Add(6*time.Minute))
			first := recordedShipLoss(t, h, recoveryShipBlastoise)
			afloat["cargo_manifest"] = map[string]int64{"grain": 7}
			runner.observeShipLosses(h.ctx, "ships", encode([]any{afloat, peer}), start.Add(7*time.Minute))
			recovered := recordedShipLoss(t, h, recoveryShipBlastoise)
			if recovered.RecoveredAt == nil || !recovered.ObservedAt.Equal(first.ObservedAt) || !reflect.DeepEqual(recovered.CargoManifest, first.CargoManifest) {
				t.Fatalf("afloat recovery discarded the absence evidence: first=%s recovered=%s", encode(first), encode(recovered))
			}
			if err := runner.saveSnapshot(h.ctx); err != nil {
				t.Fatal(err)
			}
			h.api.setFleet([]map[string]any{afloat, peer})
			runner = h.runner(primaryTestKey)
			refreshLossFleet(t, h, runner)
			lastAfloat := runner.meta["ship_details/"+recoveryShipBlastoise].UpdatedAt
			shipLossEventRecord(t, h, "actual-terminal-storm", "voyage_hazard", time.Now().UTC(), map[string]any{
				"ship_id": recoveryShipBlastoise, "hazard": "storm", "sunk": true,
			})
			if terminal {
				lost := ownedLossRow(sunkShipRow(recoveryShipBlastoise, ""))
				lost["ship"].(map[string]any)["ship_class"] = ""
				lost["ship"].(map[string]any)["fate"] = "lost_at_sea"
				h.api.setFleet([]map[string]any{lost, peer})
				refreshLossFleet(t, h, runner)
			} else {
				now := time.Now().UTC()
				runner.observeShipLosses(h.ctx, "ships", encode([]any{peer}), now)
				runner.observeShipLosses(h.ctx, "ships", encode([]any{peer}), now.Add(5*time.Minute))
			}
			loss := recordedShipLoss(t, h, recoveryShipBlastoise)
			var cargo map[string]int64
			if loss.RecoveredAt != nil || !loss.ObservedAt.After(*recovered.RecoveredAt) || loss.Fate != "lost_at_sea" ||
				loss.ShipName != "Blastoise" || loss.ShipClass != recoveryHull || loss.LastObservedAt == nil ||
				loss.LastObservedAt.Sub(lastAfloat).Abs() > time.Microsecond ||
				json.Unmarshal(loss.CargoManifest, &cargo) != nil || cargo["grain"] != 7 ||
				loss.HazardCause == nil || *loss.HazardCause != "storm" {
				t.Fatalf("new loss reused the resolved absence episode: %s", encode(loss))
			}
		})
	}
}

func TestShipLossPendingMissingRecoverySurvivesRestart(t *testing.T) {
	h := newRecoveryHarness(t)
	runner := freshLossRunner(t, h)
	start := time.Now().UTC().Truncate(time.Microsecond).Add(-time.Hour)
	afloat := ownedLossRow(sailingShipRow(recoveryShipBlastoise, "Blastoise", recoveryHomePort, recoveryFarPort))
	peer := ownedLossRow(sailingShipRow(recoveryShipLapras, "Lapras", recoveryHomePort, recoveryFarPort))
	runner.observeShipLosses(h.ctx, "ships", encode([]any{afloat, peer}), start)
	admin, err := sql.Open("pgx", h.dsn)
	if err != nil {
		t.Fatal(err)
	}
	defer admin.Close()
	if _, err := admin.ExecContext(h.ctx, "ALTER TABLE ship_losses RENAME TO ship_losses_unavailable"); err != nil {
		t.Fatal(err)
	}
	runner.observeShipLosses(h.ctx, "ships", encode([]any{peer}), start.Add(time.Minute))
	runner.observeShipLosses(h.ctx, "ships", encode([]any{peer}), start.Add(6*time.Minute))
	runner.observeShipLosses(h.ctx, "ships", encode([]any{afloat, peer}), start.Add(7*time.Minute))
	pending := decode[map[string]store.ShipLoss](runner.snapshot, "_pending_ship_losses")[recoveryShipBlastoise]
	if pending.RecoveredAt == nil {
		t.Fatal("afloat observation did not resolve the pending absence")
	}
	if err := runner.saveSnapshot(h.ctx); err != nil {
		t.Fatal(err)
	}
	if _, err := admin.ExecContext(h.ctx, "ALTER TABLE ship_losses_unavailable RENAME TO ship_losses"); err != nil {
		t.Fatal(err)
	}
	h.api.setFleet([]map[string]any{afloat, peer})
	runner = h.runner(primaryTestKey)
	refreshLossFleet(t, h, runner)
	loss := recordedShipLoss(t, h, recoveryShipBlastoise)
	if loss.Fate != "missing" || loss.RecoveredAt == nil || loss.RecoveredAt.Sub(*pending.RecoveredAt).Abs() > time.Microsecond {
		t.Fatalf("pending retry resurrected a recovered absence: %s", encode(loss))
	}
	if _, exists := runner.snapshot["_pending_ship_losses"]; exists {
		t.Fatal("saved recovery retained a pending loss")
	}
}

func TestShipLossIdentityFenceRejectsForeignOrUnverifiedFleet(t *testing.T) {
	for _, invalid := range []string{"profiles", "mismatched_profiles", "current_ship", "previous_ship"} {
		t.Run(invalid, func(t *testing.T) {
			h := newRecoveryHarness(t)
			runner := freshLossRunner(t, h)
			afloat := ownedLossRow(sailingShipRow(recoveryShipBlastoise, "Blastoise", recoveryHomePort, recoveryFarPort))
			runner.snapshot["ships"] = encode([]any{afloat})
			runner.meta["ships"] = DatasetMeta{Complete: true, UpdatedAt: time.Now().Add(-time.Minute)}
			current := []any{}
			switch invalid {
			case "profiles":
				delete(runner.meta, "whoami")
				runner.shipLosses = nil
			case "mismatched_profiles":
				runner.snapshot["whoami"] = encode(map[string]any{"company_id": "foreign-company", "player_id": "foreign-player"})
				runner.shipLosses = nil
			case "current_ship":
				foreign := ownedLossRow(sunkShipRow(recoveryShipLapras, "Lapras"))
				foreign["ship"].(map[string]any)["company_id"] = "foreign-company"
				current = append(current, foreign)
			case "previous_ship":
				afloat["ship"].(map[string]any)["company_id"] = "old-company"
				runner.snapshot["ships"] = encode([]any{afloat})
			}
			runner.observeShipLosses(h.ctx, "ships", encode(current), time.Now().UTC())
			rows, err := h.db.ShipLosses(h.ctx, h.account.ID)
			if err != nil || len(rows) != 0 {
				t.Fatalf("invalid identity wrote loss history: %s, %v", encode(rows), err)
			}
		})
	}
}

func TestShipLossNewHullBackfillsEventsBeforeTheSavedCursor(t *testing.T) {
	h := newRecoveryHarness(t)
	runner := freshLossRunner(t, h)
	shipLossEventRecord(t, h, "older-second-hull-hazard", "voyage_hazard", time.Now().Add(-time.Hour), map[string]any{
		"ship_id": recoveryShipLapras, "hazard": "piracy", "captured": true,
	})
	first := ownedLossRow(sunkShipRow(recoveryShipBlastoise, "Blastoise"))
	first["ship"].(map[string]any)["fate"] = "lost_at_sea"
	h.api.setFleet([]map[string]any{first})
	refreshLossFleet(t, h, runner)
	if decode[int64](runner.snapshot, "_ship_loss_event_cursor") == 0 {
		t.Fatal("first loss observation did not retain its event observation cursor")
	}
	second := ownedLossRow(sunkShipRow(recoveryShipLapras, "Lapras"))
	second["ship"].(map[string]any)["fate"] = "captured"
	h.api.setFleet([]map[string]any{first, second})
	refreshLossFleet(t, h, runner)
	loss := recordedShipLoss(t, h, recoveryShipLapras)
	if loss.HazardCause == nil || *loss.HazardCause != "piracy" {
		t.Fatalf("new loss skipped its retained pre-cursor event: %s", encode(loss))
	}
}

func TestShipLossUnusedSchemaFieldsAndEmptyPortDoNotDiscardEvidence(t *testing.T) {
	h := newRecoveryHarness(t)
	runner := freshLossRunner(t, h)
	afloat := ownedLossRow(sailingShipRow(recoveryShipBlastoise, "Blastoise", "", recoveryFarPort))
	afloat["ship"].(map[string]any)["status"] = "berthed"
	afloat["ship"].(map[string]any)["cannons_count"] = "unused-schema-extension"
	afloat["route_legs"].([]any)[0].(map[string]any)["distance_nm"] = "unused-schema-extension"
	runner.snapshot["ships"] = encode([]any{afloat})
	runner.meta["ships"] = DatasetMeta{UpdatedAt: time.Now().Add(-time.Minute)}
	lost := ownedLossRow(sunkShipRow(recoveryShipBlastoise, ""))
	lost["ship"].(map[string]any)["ship_class"] = ""
	lost["ship"].(map[string]any)["updated_at"] = "unused-schema-extension"
	runner.observeShipLosses(h.ctx, "ships", encode([]any{lost}), time.Now().UTC())
	loss := recordedShipLoss(t, h, recoveryShipBlastoise)
	if loss.ShipName != "Blastoise" || loss.ShipClass != recoveryHull || loss.LastPortID != nil || loss.Stores == nil {
		t.Fatalf("unused fields or an empty port discarded known evidence: %s", encode(loss))
	}
}

func TestShipLossForeignAfloatDoesNotRecoverMissingHull(t *testing.T) {
	h := newRecoveryHarness(t)
	runner := freshLossRunner(t, h)
	start := time.Now().UTC().Truncate(time.Microsecond).Add(-time.Hour)
	afloat := ownedLossRow(sailingShipRow(recoveryShipBlastoise, "Blastoise", recoveryHomePort, recoveryFarPort))
	peer := ownedLossRow(sailingShipRow(recoveryShipLapras, "Lapras", recoveryHomePort, recoveryFarPort))
	runner.observeShipLosses(h.ctx, "ships", encode([]any{afloat, peer}), start)
	runner.observeShipLosses(h.ctx, "ships", encode([]any{peer}), start.Add(time.Minute))
	runner.observeShipLosses(h.ctx, "ships", encode([]any{peer}), start.Add(6*time.Minute))
	afloat["ship"].(map[string]any)["company_id"] = "foreign-company"
	runner.observeShipLosses(h.ctx, "ships", encode([]any{afloat, peer}), start.Add(7*time.Minute))
	loss := recordedShipLoss(t, h, recoveryShipBlastoise)
	if loss.RecoveredAt != nil {
		t.Fatalf("foreign company evidence recovered our missing hull: %s", encode(loss))
	}
}

func TestShipLossInvalidObservationDoesNotStarveOtherLosses(t *testing.T) {
	h := newRecoveryHarness(t)
	runner := freshLossRunner(t, h)
	core, logs := observer.New(zap.WarnLevel)
	runner.log = zap.New(core)
	invalid := ownedLossRow(sunkShipRow(recoveryShipBlastoise, fmt.Sprintf("%0256d", 1)))
	valid := ownedLossRow(sunkShipRow(recoveryShipLapras, "Lapras"))
	now := time.Now().UTC()
	if !runner.applyCapture(h.ctx, "ships", fetchResult{data: encode([]any{invalid, valid}), attemptedAt: now, completedAt: now}) {
		t.Fatal("one rejected history row blocked the fresh fleet")
	}
	rows, err := h.db.ShipLosses(h.ctx, h.account.ID)
	if err != nil || len(rows) != 1 || rows[0].ShipID != recoveryShipLapras {
		t.Fatalf("one rejected observation starved a valid loss: %s, %v", encode(rows), err)
	}
	pending := decode[map[string]store.ShipLoss](runner.snapshot, "_pending_ship_losses")
	if len(pending) != 1 || pending[recoveryShipBlastoise].ShipID == "" || logs.FilterMessage("ship loss history not recorded").Len() != 1 {
		t.Fatalf("rejected observation was neither retained nor warned: %s, %v", encode(pending), logs.All())
	}
}

func TestShipLossIdentityFailureRetainsAfloatEvidenceAcrossRestart(t *testing.T) {
	for _, legacy := range []bool{false, true} {
		t.Run(fmt.Sprintf("legacy_snapshot=%v", legacy), func(t *testing.T) {
			h := newRecoveryHarness(t)
			afloat := ownedLossRow(sailingShipRow(recoveryShipBlastoise, "Blastoise", recoveryHomePort, recoveryFarPort))
			afloat["cargo_manifest"] = map[string]int64{"grain": 4}
			h.api.setFleet([]map[string]any{afloat})
			runner := h.runner(primaryTestKey)
			refreshLossFleet(t, h, runner)
			lastAfloat := runner.meta["ship_details/"+recoveryShipBlastoise].UpdatedAt
			if legacy {
				delete(runner.snapshot, "_ship_loss_verified_ships")
			}
			runner.generation = uuid.NewString()
			delete(runner.meta, "whoami")
			now := time.Now().UTC()
			if !runner.applyCapture(h.ctx, "ships", fetchResult{data: encode([]any{}), attemptedAt: now, completedAt: now}) {
				t.Fatal("identity failure blocked observation")
			}
			rows, err := h.db.ShipLosses(h.ctx, h.account.ID)
			if err != nil || len(rows) != 0 {
				t.Fatalf("unverified identity recorded a disappearance: %s, %v", encode(rows), err)
			}
			runner.snapshot["ship_details"] = encode(map[string]any{})
			runner.snapshot["_meta"] = encode(runner.meta)
			if err := runner.saveSnapshot(h.ctx); err != nil {
				t.Fatal(err)
			}
			peer := ownedLossRow(sailingShipRow(recoveryShipLapras, "Lapras", recoveryHomePort, recoveryFarPort))
			h.api.setFleet([]map[string]any{peer})
			runner = h.runner(primaryTestKey)
			refreshLossFleet(t, h, runner)
			runner.observeShipLosses(h.ctx, "ships", encode([]any{peer}), time.Now().UTC().Add(5*time.Minute))
			loss := recordedShipLoss(t, h, recoveryShipBlastoise)
			var cargo map[string]int64
			if loss.Fate != "missing" || loss.LastObservedAt == nil || loss.LastObservedAt.Sub(lastAfloat).Abs() > time.Microsecond ||
				json.Unmarshal(loss.CargoManifest, &cargo) != nil || cargo["grain"] != 4 {
				t.Fatalf("identity outage discarded the prior hull observation: %s", encode(loss))
			}
			if _, exists := runner.snapshot["_ship_loss_legacy_ships"]; exists {
				t.Fatal("verified capture retained its temporary legacy bootstrap")
			}
		})
	}
}

func TestShipLossFailedBackfillIsRetriedAfterRestartBelowTheCursor(t *testing.T) {
	for _, snapshotSaved := range []bool{false, true} {
		t.Run(fmt.Sprintf("retry_snapshot_saved=%v", snapshotSaved), func(t *testing.T) {
			h := newRecoveryHarness(t)
			runner := freshLossRunner(t, h)
			shipLossEventRecord(t, h, "pre-cursor-hazard", "voyage_hazard", time.Now().Add(-time.Hour), map[string]any{
				"ship_id": recoveryShipLapras, "hazard": "piracy", "captured": true,
			})
			first := ownedLossRow(sunkShipRow(recoveryShipBlastoise, "Blastoise"))
			first["ship"].(map[string]any)["fate"] = "lost_at_sea"
			now := time.Now().UTC().Truncate(time.Microsecond).Add(999 * time.Nanosecond)
			runner.applyCapture(h.ctx, "ships", fetchResult{data: encode([]any{first}), attemptedAt: now, completedAt: now})
			if decode[int64](runner.snapshot, "_ship_loss_event_cursor") == 0 {
				t.Fatal("initial evidence cursor was not recorded")
			}
			runner.snapshot["_meta"] = encode(runner.meta)
			if err := runner.saveSnapshot(h.ctx); err != nil {
				t.Fatal(err)
			}
			admin, err := sql.Open("pgx", h.dsn)
			if err != nil {
				t.Fatal(err)
			}
			defer admin.Close()
			if _, err := admin.ExecContext(h.ctx, "ALTER TABLE events RENAME TO events_unavailable"); err != nil {
				t.Fatal(err)
			}
			second := ownedLossRow(sunkShipRow(recoveryShipLapras, "Lapras"))
			second["ship"].(map[string]any)["fate"] = "captured"
			now = time.Now().UTC().Truncate(time.Microsecond).Add(999 * time.Nanosecond)
			runner.applyCapture(h.ctx, "ships", fetchResult{data: encode([]any{first, second}), attemptedAt: now, completedAt: now})
			primary := recordedShipLoss(t, h, recoveryShipLapras)
			if primary.HazardCause != nil || !decode[bool](runner.snapshot, "_ship_loss_events_backfill") {
				t.Fatal("failed event read did not retain its new-loss backfill obligation")
			}
			if snapshotSaved {
				runner.snapshot["_meta"] = encode(runner.meta)
				if err := runner.saveSnapshot(h.ctx); err != nil {
					t.Fatal(err)
				}
			}
			if _, err := admin.ExecContext(h.ctx, "ALTER TABLE events_unavailable RENAME TO events"); err != nil {
				t.Fatal(err)
			}
			h.api.setFleet([]map[string]any{first, second})
			runner = h.runner(primaryTestKey)
			refreshLossFleet(t, h, runner)
			retryAt := primary.ObservedAt.Add(15 * time.Minute)
			if snapshotSaved {
				runner.observeShipLosses(h.ctx, "ships", encode([]any{first, second}), retryAt.Add(-time.Microsecond))
				if early := recordedShipLoss(t, h, recoveryShipLapras); early.HazardCause != nil {
					t.Fatalf("event backfill retried before its persisted deadline: %s", encode(early))
				}
			}
			runner.observeShipLosses(h.ctx, "ships", encode([]any{first, second}), retryAt)
			loss := recordedShipLoss(t, h, recoveryShipLapras)
			if loss.HazardCause == nil || *loss.HazardCause != "piracy" || !loss.ObservedAt.Equal(primary.ObservedAt) {
				t.Fatalf("successful retry skipped retained evidence below the cursor: %s", encode(loss))
			}
			if decode[bool](runner.snapshot, "_ship_loss_events_backfill") {
				t.Fatal("successful backfill retained its retry marker")
			}
		})
	}
}

func TestShipLossStalePendingEpisodeCannotOverwriteDurableRecoveryOrNewLoss(t *testing.T) {
	for _, episode := range []string{"recovered", "terminal", "missing_again"} {
		t.Run(episode, func(t *testing.T) {
			h := newRecoveryHarness(t)
			runner := freshLossRunner(t, h)
			at := time.Now().UTC().Add(-3 * time.Hour).Truncate(time.Microsecond)
			lastAfloat := at.Add(-time.Minute)
			pending := store.ShipLoss{
				ShipID: recoveryShipBlastoise, ShipName: "Blastoise", ShipClass: recoveryHull,
				Fate: "missing", Status: "sunk", ObservedAt: at, LastObservedAt: &lastAfloat,
				CargoManifest: json.RawMessage(`{"grain":2}`),
			}
			if err := h.db.SaveShipLosses(h.ctx, h.account.ID, []store.ShipLoss{pending}); err != nil {
				t.Fatal(err)
			}
			recoveredAt := at.Add(time.Hour)
			recovered := pending
			recovered.RecoveredAt = &recoveredAt
			if err := h.db.SaveShipLosses(h.ctx, h.account.ID, []store.ShipLoss{recovered}); err != nil {
				t.Fatal(err)
			}
			switch episode {
			case "terminal", "missing_again":
				freshAfloat := at.Add(90 * time.Minute)
				fresh := pending
				fresh.ObservedAt = at.Add(2 * time.Hour)
				fresh.LastObservedAt = &freshAfloat
				fresh.CargoManifest = json.RawMessage(`{"grain":7}`)
				if episode == "terminal" {
					fresh.Fate = "lost_at_sea"
				}
				if err := h.db.SaveShipLosses(h.ctx, h.account.ID, []store.ShipLoss{fresh}); err != nil {
					t.Fatal(err)
				}
			}
			durable := recordedShipLoss(t, h, recoveryShipBlastoise)
			runner.snapshot["_pending_ship_losses"] = encode(map[string]store.ShipLoss{recoveryShipBlastoise: pending})
			if err := runner.saveSnapshot(h.ctx); err != nil {
				t.Fatal(err)
			}
			if episode == "terminal" {
				lost := ownedLossRow(sunkShipRow(recoveryShipBlastoise, "Blastoise"))
				lost["ship"].(map[string]any)["fate"] = "lost_at_sea"
				h.api.setFleet([]map[string]any{lost})
			} else if episode == "recovered" {
				h.api.setFleet([]map[string]any{ownedLossRow(sailingShipRow(recoveryShipBlastoise, "Blastoise", recoveryHomePort, recoveryFarPort))})
			}
			runner = h.runner(primaryTestKey)
			refreshLossFleet(t, h, runner)
			after := recordedShipLoss(t, h, recoveryShipBlastoise)
			if !reflect.DeepEqual(durable, after) {
				t.Fatalf("stale snapshot retry overwrote durable episode: before=%s after=%s", encode(durable), encode(after))
			}
			if _, exists := runner.snapshot["_pending_ship_losses"]; exists {
				t.Fatal("superseded retry remained queued")
			}
		})
	}
}

func TestShipLossUnboundedCargoDoesNotPoisonOtherLossesOrSnapshotRetries(t *testing.T) {
	h := newRecoveryHarness(t)
	runner := freshLossRunner(t, h)
	core, logs := observer.New(zap.WarnLevel)
	runner.log = zap.New(core)
	bad := ownedLossRow(sailingShipRow(recoveryShipBlastoise, "Blastoise", recoveryHomePort, recoveryFarPort))
	bad["cargo_manifest"] = json.RawMessage(`{"grain":1e130000}`)
	good := ownedLossRow(sailingShipRow(recoveryShipLapras, "Lapras", recoveryHomePort, recoveryFarPort))
	good["cargo_manifest"] = map[string]int64{"grain": 4}
	runner.snapshot["ships"] = encode([]any{bad, good})
	runner.meta["ships"] = DatasetMeta{UpdatedAt: time.Now().Add(-time.Minute)}
	first := ownedLossRow(sunkShipRow(recoveryShipBlastoise, "Blastoise"))
	second := ownedLossRow(sunkShipRow(recoveryShipLapras, "Lapras"))
	now := time.Now().UTC()
	if !runner.applyCapture(h.ctx, "ships", fetchResult{data: encode([]any{first, second}), attemptedAt: now, completedAt: now}) {
		t.Fatal("unpersistable cargo prevented fleet observation")
	}
	rows, err := h.db.ShipLosses(h.ctx, h.account.ID)
	if err != nil || len(rows) != 2 {
		t.Fatalf("unpersistable cargo starved hull history: %s, %v", encode(rows), err)
	}
	loss := recordedShipLoss(t, h, recoveryShipBlastoise)
	if loss.CargoManifest != nil || loss.CargoUnavailableReason == "" || loss.Stores == nil || logs.FilterMessage("ship loss history not recorded").Len() != 1 {
		t.Fatalf("unpersistable cargo was fabricated or its valid context discarded: %s, %v", encode(loss), logs.All())
	}
	other := recordedShipLoss(t, h, recoveryShipLapras)
	var cargo map[string]int64
	if json.Unmarshal(other.CargoManifest, &cargo) != nil || cargo["grain"] != 4 {
		t.Fatalf("valid hull cargo disappeared: %s", encode(other))
	}
	if err := runner.saveSnapshot(h.ctx); err != nil {
		t.Fatalf("invalid numeric evidence poisoned the protected retry snapshot: %v", err)
	}
}

func TestShipLossSkipsOnlyUnownedRowsAndWarnsForIdentityFailure(t *testing.T) {
	h := newRecoveryHarness(t)
	runner := freshLossRunner(t, h)
	core, logs := observer.New(zap.WarnLevel)
	runner.log = zap.New(core)
	start := time.Now().UTC().Truncate(time.Microsecond).Add(-time.Hour)
	afloat := ownedLossRow(sailingShipRow(recoveryShipBlastoise, "Blastoise", recoveryHomePort, recoveryFarPort))
	runner.observeShipLosses(h.ctx, "ships", encode([]any{afloat}), start)
	foreign := ownedLossRow(sunkShipRow(recoveryShipBlastoise, "Blastoise"))
	foreign["ship"].(map[string]any)["company_id"] = "foreign-company"
	blankCompany := ownedLossRow(sunkShipRow("no-company", "Unverified"))
	blankCompany["ship"].(map[string]any)["company_id"] = ""
	blankID := ownedLossRow(sunkShipRow("", "No identity"))
	valid := ownedLossRow(sunkShipRow(recoveryShipLapras, "Lapras"))
	current := encode([]any{foreign, blankCompany, blankID, valid})
	runner.observeShipLosses(h.ctx, "ships", current, start.Add(time.Minute))
	runner.observeShipLosses(h.ctx, "ships", current, start.Add(6*time.Minute))
	rows, err := h.db.ShipLosses(h.ctx, h.account.ID)
	if err != nil || len(rows) != 1 || rows[0].ShipID != recoveryShipLapras {
		t.Fatalf("one foreign row blocked an owned loss or invented an absence: %s, %v", encode(rows), err)
	}
	if logs.FilterField(zap.String("operation", "row_identity")).Len() != 1 {
		t.Fatalf("row failures were not rate limited by cause: %v", logs.All())
	}
	runner.generation = uuid.NewString()
	delete(runner.meta, "whoami")
	runner.observeShipLosses(h.ctx, "ships", current, start.Add(7*time.Minute))
	if logs.FilterField(zap.String("operation", "identity")).Len() != 1 {
		t.Fatalf("a previous row warning hid an identity failure: %v", logs.All())
	}
}

func TestShipLossMissingHullReappearsWithTerminalFate(t *testing.T) {
	for _, fate := range []string{"unknown", "lost_at_sea", "captured", "royal_navy", "broken_up"} {
		t.Run(fate, func(t *testing.T) {
			h := newRecoveryHarness(t)
			runner := freshLossRunner(t, h)
			start := time.Now().UTC().Truncate(time.Microsecond).Add(-time.Hour)
			afloat := ownedLossRow(sailingShipRow(recoveryShipBlastoise, "Blastoise", recoveryHomePort, recoveryFarPort))
			peer := ownedLossRow(sailingShipRow(recoveryShipLapras, "Lapras", recoveryHomePort, recoveryFarPort))
			runner.observeShipLosses(h.ctx, "ships", encode([]any{afloat, peer}), start)
			runner.observeShipLosses(h.ctx, "ships", encode([]any{peer}), start.Add(time.Minute))
			runner.observeShipLosses(h.ctx, "ships", encode([]any{peer}), start.Add(6*time.Minute))
			missing := recordedShipLoss(t, h, recoveryShipBlastoise)
			terminal := ownedLossRow(sunkShipRow(recoveryShipBlastoise, "Blastoise"))
			if fate != "unknown" {
				terminal["ship"].(map[string]any)["fate"] = fate
			}
			runner.observeShipLosses(h.ctx, "ships", encode([]any{terminal, peer}), start.Add(7*time.Minute))
			confirmed := recordedShipLoss(t, h, recoveryShipBlastoise)
			if confirmed.Fate != fate || !confirmed.ObservedAt.Equal(missing.ObservedAt) || confirmed.RecoveredAt != nil ||
				!reflect.DeepEqual(confirmed.CargoManifest, missing.CargoManifest) {
				t.Fatalf("terminal observation failed to correct missing fate: %s", encode(confirmed))
			}
		})
	}
}

func TestShipLossHazardBoundsDoNotDiscardHullAndLaterValidEvidenceWins(t *testing.T) {
	h := newRecoveryHarness(t)
	runner := freshLossRunner(t, h)
	core, logs := observer.New(zap.WarnLevel)
	runner.log = zap.New(core)
	afloat := ownedLossRow(sailingShipRow(recoveryShipBlastoise, "Blastoise", recoveryHomePort, recoveryFarPort))
	runner.observeShipLosses(h.ctx, "ships", encode([]any{afloat}), time.Now().UTC().Add(-time.Minute))
	shipLossEventRecord(t, h, "oversized-hazard", "voyage_hazard", time.Now().UTC(), map[string]any{
		"ship_id": recoveryShipBlastoise, "sunk": true, "hazard": strings.Repeat("x", 4097),
	})
	lost := ownedLossRow(sunkShipRow(recoveryShipBlastoise, "Blastoise"))
	lost["ship"].(map[string]any)["fate"] = "lost_at_sea"
	now := time.Now().UTC()
	runner.observeShipLosses(h.ctx, "ships", encode([]any{lost}), now)
	first := recordedShipLoss(t, h, recoveryShipBlastoise)
	if first.HazardCause != nil || first.HazardUnavailableReason == "" || first.Stores == nil || first.CargoManifest == nil ||
		logs.FilterField(zap.String("operation", "hazard")).Len() != 1 {
		t.Fatalf("bad optional hazard poisoned the hull record: %s, %v", encode(first), logs.All())
	}
	shipLossEventRecord(t, h, "bounded-hazard", "voyage_hazard", time.Now().UTC(), map[string]any{
		"ship_id": recoveryShipBlastoise, "sunk": true, "hazard": "storm",
	})
	runner.observeShipLosses(h.ctx, "ships", encode([]any{lost}), time.Now().UTC())
	updated := recordedShipLoss(t, h, recoveryShipBlastoise)
	if updated.HazardCause == nil || *updated.HazardCause != "storm" || updated.HazardUnavailableReason != "" ||
		!updated.ObservedAt.Equal(first.ObservedAt) || !reflect.DeepEqual(updated.CargoManifest, first.CargoManifest) {
		t.Fatalf("valid later hazard failed to enrich the bounded record: %s", encode(updated))
	}
}

func TestShipLossPendingSaveAndBackfillRetryAtMostEveryFifteenRealMinutes(t *testing.T) {
	h := newRecoveryHarness(t)
	runner := freshLossRunner(t, h)
	afloat := ownedLossRow(sailingShipRow(recoveryShipBlastoise, "Blastoise", recoveryHomePort, recoveryFarPort))
	runner.observeShipLosses(h.ctx, "ships", encode([]any{afloat}), time.Now().UTC().Add(-time.Minute))
	admin, err := sql.Open("pgx", h.dsn)
	if err != nil {
		t.Fatal(err)
	}
	defer admin.Close()
	_, err = admin.ExecContext(h.ctx, `
		CREATE SEQUENCE ship_loss_save_attempts;
		CREATE FUNCTION reject_ship_loss_save() RETURNS trigger LANGUAGE plpgsql AS $$
		BEGIN PERFORM nextval('ship_loss_save_attempts'); RAISE EXCEPTION 'synthetic loss-history outage'; END $$;
		CREATE TRIGGER reject_ship_loss_save BEFORE INSERT ON ship_losses FOR EACH ROW EXECUTE FUNCTION reject_ship_loss_save()`)
	if err != nil {
		t.Fatal(err)
	}
	shipLossEventRecord(t, h, "partial-storm", "voyage_hazard", time.Now().UTC(), map[string]any{
		"ship_id": recoveryShipBlastoise, "hazard": "storm", "sunk": false,
	})
	lost := ownedLossRow(sunkShipRow(recoveryShipBlastoise, "Blastoise"))
	lost["ship"].(map[string]any)["fate"] = "lost_at_sea"
	now := time.Now().UTC().Truncate(time.Microsecond)
	runner.observeShipLosses(h.ctx, "ships", encode([]any{lost}), now)
	firstCursor := decode[int64](runner.snapshot, "_ship_loss_event_cursor")
	if firstCursor == 0 || decode[bool](runner.snapshot, "_ship_loss_events_backfill") {
		t.Fatal("successful event scan remained tied to failed row persistence")
	}
	shipLossEventRecord(t, h, "terminal-storm", "voyage_hazard", time.Now().UTC(), map[string]any{
		"ship_id": recoveryShipBlastoise, "hazard": "storm", "sunk": true,
	})
	for _, after := range []time.Duration{time.Second, time.Minute, 8 * time.Minute, 15*time.Minute - time.Microsecond} {
		runner.observeShipLosses(h.ctx, "ship_details/"+recoveryShipBlastoise, encode(lost), now.Add(after))
	}
	var attempts int64
	if err := admin.QueryRowContext(h.ctx, "SELECT last_value FROM ship_loss_save_attempts").Scan(&attempts); err != nil {
		t.Fatal(err)
	}
	pending := decode[map[string]store.ShipLoss](runner.snapshot, "_pending_ship_losses")[recoveryShipBlastoise]
	if attempts != 1 || pending.HazardCause != nil || decode[int64](runner.snapshot, "_ship_loss_event_cursor") != firstCursor {
		t.Fatalf("child captures repeated saves or event reads during backoff: attempts=%d pending=%s", attempts, encode(pending))
	}
	runner.observeShipLosses(h.ctx, "ship_details/"+recoveryShipBlastoise, encode(lost), now.Add(15*time.Minute))
	if err := admin.QueryRowContext(h.ctx, "SELECT last_value FROM ship_loss_save_attempts").Scan(&attempts); err != nil {
		t.Fatal(err)
	}
	pending = decode[map[string]store.ShipLoss](runner.snapshot, "_pending_ship_losses")[recoveryShipBlastoise]
	if attempts != 2 || pending.HazardCause == nil || *pending.HazardCause != "storm" ||
		decode[int64](runner.snapshot, "_ship_loss_event_cursor") <= firstCursor || decode[bool](runner.snapshot, "_ship_loss_events_backfill") {
		t.Fatalf("due retry failed to advance evidence independently of the failed save: attempts=%d pending=%s", attempts, encode(pending))
	}
	if _, err := admin.ExecContext(h.ctx, "DROP TRIGGER reject_ship_loss_save ON ship_losses"); err != nil {
		t.Fatal(err)
	}
	runner.observeShipLosses(h.ctx, "ships", encode([]any{lost}), now.Add(29*time.Minute))
	rows, err := h.db.ShipLosses(h.ctx, h.account.ID)
	if err != nil || len(rows) != 0 {
		t.Fatalf("retry ran before its next fifteen-minute deadline: %s, %v", encode(rows), err)
	}
	runner.observeShipLosses(h.ctx, "ships", encode([]any{lost}), now.Add(30*time.Minute))
	saved := recordedShipLoss(t, h, recoveryShipBlastoise)
	if !saved.ObservedAt.Equal(now) || saved.HazardCause == nil || *saved.HazardCause != "storm" {
		t.Fatalf("bounded retry lost first-observed evidence: %s", encode(saved))
	}
}

func TestShipLossUnsafeEvidenceIsBoundedBeforeVerifiedAndLegacyRetention(t *testing.T) {
	for _, retained := range []string{"verified", "legacy"} {
		t.Run(retained, func(t *testing.T) {
			h := newRecoveryHarness(t)
			runner := freshLossRunner(t, h)
			afloat := ownedLossRow(sailingShipRow(recoveryShipBlastoise, "Blastoise", recoveryHomePort, recoveryFarPort))
			numbers := make(map[string]json.RawMessage, 1000)
			for i := range 1000 {
				numbers[fmt.Sprintf("commodity-%d", i)] = json.RawMessage(`1e60`)
			}
			afloat["cargo_manifest"] = numbers
			afloat["stores"] = json.RawMessage(`{"provisions_tons":1e130000}`)
			if retained == "legacy" {
				runner.snapshot["ships"] = encode([]any{afloat})
				delete(runner.meta, "whoami")
				runner.shipLosses = nil
			}
			now := time.Now().UTC()
			runner.observeShipLosses(h.ctx, "ships", encode([]any{afloat}), now)
			var observation shipLossObservation
			if retained == "legacy" {
				observation = decode[retainedShipLossObservations](runner.snapshot, "_ship_loss_legacy_ships").Afloat[recoveryShipBlastoise]
			} else {
				observation = decode[map[string]shipLossObservation](runner.snapshot, "_ship_loss_verified_ships")[recoveryShipBlastoise]
			}
			if observation.Ship.ID == "" || string(observation.CargoManifest) != "null" || string(observation.Stores) != "null" ||
				observation.CargoUnavailableReason == "" || observation.StoresUnavailableReason == "" {
				t.Fatalf("unsafe optional evidence entered protected %s state: %s", retained, encode(observation))
			}
			// The ordinary display dataset has its own storage policy. This
			// assertion isolates the protected recorder evidence that survives it.
			runner.snapshot["ships"] = json.RawMessage(`[]`)
			if err := runner.saveSnapshot(h.ctx); err != nil {
				t.Fatalf("unsafe %s evidence poisoned snapshot persistence: %v", retained, err)
			}
		})
	}
}
