package store

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"go.uber.org/zap"
	"go.uber.org/zap/zaptest/observer"
	"gorm.io/gorm"
)

func TestPassengerPruneRetainsOnlyPendingRecoveryEventsAndDurableSettlements(t *testing.T) {
	database, dsn, key := integrationStore(t)
	ctx := context.Background()
	const accountID = "passenger-retention"
	const foreignID = "passenger-retention-foreign"
	saveTestAccount(t, database, accountID)
	saveTestAccount(t, database, foreignID)
	old := time.Now().UTC().Add(-9 * 24 * time.Hour).Truncate(time.Microsecond)
	for _, event := range []Event{
		{AccountID: accountID, EventID: "ordinary-paid", EventType: "passengers_paid", Payload: json.RawMessage(`{"fare_paid_pence":1000}`), CreatedAt: old.Add(-2 * time.Hour)},
		{AccountID: accountID, EventID: "unlinked-boarded", EventType: "passengers_boarded", Payload: json.RawMessage(`{}`), CreatedAt: old.Add(-2 * time.Hour)},
		{AccountID: accountID, EventID: "unlinked-lost", EventType: "passengers_lost", Payload: json.RawMessage(`{}`), CreatedAt: old.Add(-2 * time.Hour)},
		{AccountID: accountID, EventID: "pending-proof", EventType: "passengers_paid", Payload: json.RawMessage(`{"fare_paid_pence":1000}`), CreatedAt: old},
		{AccountID: accountID, EventID: "pending-boundary", EventType: "passengers_boarded", Payload: json.RawMessage(`{}`), CreatedAt: old.Add(-time.Hour)},
		{AccountID: accountID, EventID: "old-noise", EventType: "ship_arrived", Payload: json.RawMessage(`{}`), CreatedAt: old},
		{AccountID: foreignID, EventID: "pending-proof", EventType: "passengers_paid", Payload: json.RawMessage(`{"fare_paid_pence":1000}`), CreatedAt: old},
		{AccountID: accountID, EventID: "recent-boarded", EventType: "passengers_boarded", Payload: json.RawMessage(`{}`), CreatedAt: old.Add(3 * 24 * time.Hour)},
	} {
		if err := database.db.Create(&event).Error; err != nil {
			t.Fatal(err)
		}
	}
	ordinary := passageSettlementFixture(accountID, "ordinary-passage", "ordinary-paid", 1000, old.Add(-2*time.Hour))
	if err := database.PutSettlement(ctx, &ordinary); err != nil {
		t.Fatal(err)
	}
	pending := pendingPassengerReceiptFixture(accountID, "pending-passage", old)
	if err := database.PutPendingPassengerReceipt(ctx, &pending); err != nil {
		t.Fatal(err)
	}
	receipt := pendingPassengerReceiptFixture(accountID, "resolved-passage", old.Add(-2*time.Hour))
	if err := database.PutPendingPassengerReceipt(ctx, &receipt); err != nil {
		t.Fatal(err)
	}
	proofIDs := []string{"resolved-paid", "resolved-extra"}
	resolved := receiptTerminalFixture(t, database, receipt, proofIDs, []string{"passengers_paid", "passengers_paid"}, []int64{1000, 500})
	if err := database.ResolvePassengerReceipt(ctx, &resolved, proofIDs); err != nil {
		t.Fatal(err)
	}
	if err := database.db.Model(&Event{}).Where("account_id = ? AND event_id IN ?", accountID, proofIDs).Update("created_at", old.Add(-2*time.Hour)).Error; err != nil {
		t.Fatal(err)
	}
	if err := database.Prune(ctx); err != nil {
		t.Fatal(err)
	}
	reopened, err := Open(ctx, dsn, key, zap.NewNop())
	if err != nil {
		t.Fatal(err)
	}
	defer reopened.Close()
	var retained []Event
	if err := reopened.db.Order("event_id").Find(&retained).Error; err != nil {
		t.Fatal(err)
	}
	if len(retained) != 3 || retained[0].AccountID != accountID || retained[0].EventID != "pending-boundary" || retained[1].EventID != "pending-proof" || retained[2].EventID != "recent-boarded" {
		t.Fatalf("raw passenger retention leaked or discarded recovery proof: %+v", retained)
	}
	recovery, err := reopened.PassengerEventsSince(ctx, accountID, pending.Settlement.StartedAt)
	if err != nil || len(recovery) != 3 || recovery[0].EventID != "pending-boundary" || recovery[1].EventID != "pending-proof" {
		t.Fatalf("open receipt could not read its protected older terminal evidence: %+v %v", recovery, err)
	}
	rows, err := reopened.SettlementsByID(ctx, accountID, []string{ordinary.ID, resolved.ID})
	if err != nil || len(rows) != 2 {
		t.Fatalf("event pruning deleted durable settlements: %+v %v", rows, err)
	}
	for _, settlement := range []*Settlement{&ordinary, &resolved} {
		if err := reopened.PutSettlement(ctx, settlement); err != nil {
			t.Fatalf("pruned raw proof blocked immutable replay: %v", err)
		}
		changed := *settlement
		changed.ProceedsPence++
		if err := reopened.PutSettlement(ctx, &changed); !errors.Is(err, ErrActionConflict) {
			t.Fatalf("pruning allowed changed terminal accounting: %v", err)
		}
	}
	if err := reopened.ResolvePassengerReceipt(ctx, &resolved, proofIDs); err != nil {
		t.Fatalf("resolved receipt replay depended on pruned raw proof: %v", err)
	}
	if err := reopened.ResolvePassengerReceipt(ctx, &resolved, []string{resolved.EventID, "invented-proof"}); !errors.Is(err, ErrActionConflict) {
		t.Fatalf("same-count replacement changed the durable proof set: %v", err)
	}
	var links int64
	if err := reopened.db.Model(&passengerReceiptEventRow{}).Where("settlement_id = ?", resolved.ID).Count(&links).Error; err != nil || links != 2 {
		t.Fatalf("pruning erased event ownership: %d %v", links, err)
	}
	// The protected raw event leaves retention once recovery is closed.
	if err := reopened.ClosePassengerReceipt(ctx, accountID, pending.Settlement.ID, "quarantined", time.Now().UTC()); err != nil {
		t.Fatal(err)
	}
	if err := reopened.Prune(ctx); err != nil {
		t.Fatal(err)
	}
	var remaining int64
	if err := reopened.db.Model(&Event{}).Where("event_id = ?", "pending-proof").Count(&remaining).Error; err != nil || remaining != 0 {
		t.Fatalf("closed recovery kept raw proof forever: %d %v", remaining, err)
	}
}

func TestPassengerPruneProtectsInFlightTerminalBetweenReadAndSettlement(t *testing.T) {
	database, _, _ := integrationStore(t)
	ctx := context.Background()
	const accountID = "passenger-prune-race"
	saveTestAccount(t, database, accountID)
	now := time.Now().UTC().Truncate(time.Microsecond)
	started := now.Add(-8 * 24 * time.Hour)
	mission := Mission{AccountID: accountID, ShipID: "passenger-ship", Generation: "in-flight-passage", Phase: "sailing", Data: json.RawMessage(`{"passage":{},"started_at":"` + started.Format(time.RFC3339Nano) + `"}`)}
	if err := database.PutMission(ctx, &mission); err != nil {
		t.Fatal(err)
	}
	retainedPassengerEvent(t, database, accountID, "boundary-paid", "passengers_paid", json.RawMessage(`{"fare_paid_pence":1000}`))
	if err := database.db.Model(&Event{}).Where("account_id = ?", accountID).Update("created_at", now.Add(-PassengerReceiptLifetime+time.Hour)).Error; err != nil {
		t.Fatal(err)
	}
	read, err := database.PassengerEventsSince(ctx, accountID, started)
	if err != nil || len(read) != 1 {
		t.Fatalf("passage could not read its terminal event: %+v %v", read, err)
	}
	// Advance retention age after the bounded read, before the settlement's
	// event lock. The still-operating passage must protect this raw proof.
	if err := database.db.Model(&Event{}).Where("account_id = ?", accountID).Update("created_at", now.Add(-PassengerReceiptLifetime-time.Hour)).Error; err != nil {
		t.Fatal(err)
	}
	if err := database.Prune(ctx); err != nil {
		t.Fatal(err)
	}
	restartedRead, err := database.PassengerEventsSince(ctx, accountID, started)
	if err != nil || len(restartedRead) != 1 || restartedRead[0].EventID != read[0].EventID {
		t.Fatalf("active passage could not reread protected terminal after restart: %+v %v", restartedRead, err)
	}
	settlement := passageSettlementFixture(accountID, "boundary-settlement", read[0].EventID, 1000, read[0].CreatedAt)
	if err := database.PutSettlement(ctx, &settlement); err != nil {
		t.Fatalf("prune stole already-observed paid terminal proof: %v", err)
	}
	if err := database.DeleteMission(ctx, accountID, mission.ShipID); err != nil {
		t.Fatal(err)
	}
	control := Mission{AccountID: accountID, ShipID: "__treasury", Generation: "ordinary-control", Phase: "settings", Data: json.RawMessage(`{}`)}
	if err := database.PutMission(ctx, &control); err != nil {
		t.Fatal(err)
	}
	if err := database.Prune(ctx); err != nil {
		t.Fatal(err)
	}
	var raw int64
	if err := database.db.Model(&Event{}).Where("account_id = ?", accountID).Count(&raw).Error; err != nil || raw != 0 {
		t.Fatalf("completed passage or unrelated control kept raw proof: %d %v", raw, err)
	}
	rows, err := database.SettlementsByID(ctx, accountID, []string{settlement.ID})
	if err != nil || len(rows) != 1 || rows[0].ProceedsUnknown || rows[0].ProceedsPence != 1000 {
		t.Fatalf("observed fare became unknown at retention boundary: %+v %v", rows, err)
	}
}

func TestPassengerPruneIsolatesUnreadableMissionProtection(t *testing.T) {
	database, _, _ := integrationStore(t)
	ctx := context.Background()
	const damaged = "passenger-prune-damaged"
	const healthy = "passenger-prune-healthy"
	old := time.Now().UTC().Add(-9 * 24 * time.Hour)
	for _, accountID := range []string{damaged, healthy} {
		saveTestAccount(t, database, accountID)
		if err := database.RecordRequest(ctx, accountID, "GET", "/old", 200, time.Millisecond, nil, nil, ""); err != nil {
			t.Fatal(err)
		}
		if err := database.RecordDecision(ctx, accountID, "ship", "old", "old decision", nil); err != nil {
			t.Fatal(err)
		}
		for _, kind := range []string{"passengers_paid", "ship_arrived"} {
			if err := database.RecordEvent(ctx, accountID, kind, kind, json.RawMessage(`{}`)); err != nil {
				t.Fatal(err)
			}
		}
		for _, table := range []string{"requests", "events", "decisions"} {
			if err := database.db.Table(table).Where("account_id = ?", accountID).Update("created_at", old).Error; err != nil {
				t.Fatal(err)
			}
		}
	}
	for _, shipID := range []string{"corrupt-cipher", "invalid-shape"} {
		mission := Mission{AccountID: damaged, ShipID: shipID, Generation: shipID, Phase: "sailing", Data: json.RawMessage(`[]`)}
		if err := database.PutMission(ctx, &mission); err != nil {
			t.Fatal(err)
		}
	}
	if err := database.db.Model(&missionRow{}).Where("account_id = ? AND ship_lookup = ?", damaged, database.privateLookup("mission-ship", damaged, "corrupt-cipher")).Update("data_cipher", make([]byte, 28)).Error; err != nil {
		t.Fatal(err)
	}
	core, logs := observer.New(zap.WarnLevel)
	database.log = zap.New(core)
	if err := database.Prune(ctx); err != nil {
		t.Fatalf("one account's unreadable mission stopped retention: %v", err)
	}
	for _, table := range []string{"requests", "decisions"} {
		var count int64
		if err := database.db.Table(table).Count(&count).Error; err != nil || count != 0 {
			t.Fatalf("unreadable mission stopped %s pruning: %d %v", table, count, err)
		}
	}
	var retained []Event
	if err := database.db.Find(&retained).Error; err != nil || len(retained) != 1 || retained[0].AccountID != damaged || retained[0].EventType != "passengers_paid" {
		t.Fatalf("unreadable protection crossed account/type boundaries: %+v %v", retained, err)
	}
	warnings := logs.FilterMessage("passenger retention protected unreadable recovery rows").All()
	if len(warnings) != 1 || warnings[0].ContextMap()["unreadable_rows"] != int64(2) {
		t.Fatalf("unreadable rows were not aggregated into one warning: %+v", warnings)
	}
}

func TestPassengerPruneProtectsReceiptMissionHandoff(t *testing.T) {
	database, _, _ := integrationStore(t)
	ctx := context.Background()
	const accountID = "passenger-prune-handoff"
	saveTestAccount(t, database, accountID)
	old := time.Now().UTC().Add(-9 * 24 * time.Hour)
	mission := Mission{AccountID: accountID, ShipID: "passenger-ship", Generation: "handoff-passage", Phase: "sailing", Data: json.RawMessage(`{"passage":{},"started_at":"` + old.Add(-time.Hour).Format(time.RFC3339Nano) + `"}`)}
	if err := database.PutMission(ctx, &mission); err != nil {
		t.Fatal(err)
	}
	event := Event{AccountID: accountID, EventID: "handoff-paid", EventType: "passengers_paid", Payload: json.RawMessage(`{"fare_paid_pence":1000}`), CreatedAt: old}
	if err := database.db.Create(&event).Error; err != nil {
		t.Fatal(err)
	}
	receipt := pendingPassengerReceiptFixture(accountID, "handoff-receipt", old)
	handedOff := false
	callback := database.db.Callback().Row()
	if err := callback.After("gorm:row").Register("test:passenger-retention-handoff", func(tx *gorm.DB) {
		if handedOff || !strings.Contains(tx.Statement.SQL.String(), "passenger_receipts AS receipt") {
			return
		}
		// The receipt SELECT already holds its result snapshot. Commit the
		// production handoff before the next protection query can observe it.
		handedOff = true
		if err := database.PutPendingPassengerReceipt(ctx, &receipt); err != nil {
			tx.AddError(err)
			return
		}
		tx.AddError(database.DeleteMission(ctx, accountID, mission.ShipID))
	}); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = callback.Remove("test:passenger-retention-handoff") })
	if err := database.Prune(ctx); err != nil {
		t.Fatal(err)
	}
	if !handedOff {
		t.Fatal("receipt handoff was not exercised between protection reads")
	}
	var retained int64
	if err := database.db.Model(&Event{}).Where("account_id = ?", accountID).Count(&retained).Error; err != nil || retained != 1 {
		t.Fatalf("receipt-before-mission-release handoff lost terminal proof: %d %v", retained, err)
	}
	if mission, err := database.GetMission(ctx, accountID, mission.ShipID); err != nil || mission != nil {
		t.Fatalf("handoff did not release the operating mission: %+v %v", mission, err)
	}
	events, err := database.PassengerEventsSince(ctx, accountID, receipt.Settlement.StartedAt)
	if err != nil || len(events) != 1 || events[0].EventID != event.EventID {
		t.Fatalf("new receipt could not recover protected handoff proof: %+v %v", events, err)
	}
}

func TestPassengerRecoveryWindowsScanBeyondFirstMissionBatch(t *testing.T) {
	database, _, _ := integrationStore(t)
	ctx := context.Background()
	const ordinary = "a-passenger-batch-controls"
	const passage = "z-passenger-batch-recovery"
	saveTestAccount(t, database, ordinary)
	saveTestAccount(t, database, passage)
	for index := range 65 {
		mission := Mission{AccountID: ordinary, ShipID: fmt.Sprintf("control-%d", index), Generation: "ordinary-generation", Phase: "settings", Data: json.RawMessage(`{}`)}
		if err := database.PutMission(ctx, &mission); err != nil {
			t.Fatal(err)
		}
	}
	started := time.Now().UTC().Add(-9 * 24 * time.Hour)
	mission := Mission{AccountID: passage, ShipID: "passenger-ship", Generation: "late-batch-passage", Phase: "sailing", Data: json.RawMessage(`{"passage":{},"started_at":"` + started.Format(time.RFC3339Nano) + `"}`)}
	if err := database.PutMission(ctx, &mission); err != nil {
		t.Fatal(err)
	}
	for _, scope := range []string{"", passage, ordinary} {
		windows, err := database.passengerRecoveryWindows(ctx, scope)
		if err != nil {
			t.Fatal(err)
		}
		if scope == ordinary {
			if len(windows) != 0 {
				t.Fatalf("ordinary missions acquired passenger protection: %+v", windows)
			}
		} else if len(windows) != 1 || !windows[passage].Equal(started) {
			t.Fatalf("composite-key batching lost later recovery account: %+v", windows)
		}
	}
}
