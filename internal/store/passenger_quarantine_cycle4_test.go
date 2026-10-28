package store

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"testing"
	"time"

	"go.uber.org/zap"
	"go.uber.org/zap/zaptest/observer"
)

func TestPassengerQuarantineMigrationUnreadableMissionDoesNotBlockStartup(t *testing.T) {
	db, dsn, key := integrationStore(t)
	ctx := context.Background()
	saveTestAccount(t, db, "healthy-archive-account")
	saveTestAccount(t, db, "broken-archive-account")
	live := Mission{AccountID: "healthy-archive-account", ShipID: "live-hull", Generation: "live", Phase: "sailing", Data: json.RawMessage(`{}`)}
	if err := db.PutMission(ctx, &live); err != nil {
		t.Fatal(err)
	}
	healthy := passengerQuarantineFixture(live.AccountID, "released-hull", "healthy-legacy")
	if err := db.PutMission(ctx, &healthy); err != nil {
		t.Fatal(err)
	}
	broken := Mission{AccountID: "broken-archive-account", ShipID: "unreadable-hull", Generation: "broken", Phase: "sailing", Data: json.RawMessage(`{"private_recovery":"retain-me"}`)}
	if err := db.PutMission(ctx, &broken); err != nil {
		t.Fatal(err)
	}
	if err := db.db.Exec("UPDATE missions SET metadata_cipher = ? WHERE account_id = ? AND ship_lookup = ?", bytes.Repeat([]byte("!"), 64), broken.AccountID, db.privateLookup("mission-ship", broken.AccountID, broken.ShipID)).Error; err != nil {
		t.Fatal(err)
	}
	malformed := passengerQuarantineFixture(broken.AccountID, "bad-plan", "bad-legacy")
	malformed.Data = json.RawMessage(`{"plan":{"ship_id":3},"private_recovery":"also-retain"}`)
	if err := db.PutMission(ctx, &malformed); err != nil {
		t.Fatal(err)
	}
	// Recreate the actual upgrade boundary, with unreadable missions belonging to
	// an unrelated account. Migration must retain them, not block all startup.
	if err := db.db.Exec("DROP TABLE passenger_quarantine_archives").Error; err != nil {
		t.Fatal(err)
	}
	if err := db.db.Exec("DELETE FROM goose_db_version WHERE version_id = ?", passengerQuarantineMigrationVersion).Error; err != nil {
		t.Fatal(err)
	}
	core, logs := observer.New(zap.WarnLevel)
	reopened, err := Open(ctx, dsn, key, zap.New(core))
	if err != nil {
		t.Fatalf("one unreadable mission blocked all accounts at startup: %v", err)
	}
	defer reopened.Close()
	missions, err := reopened.Missions(ctx, live.AccountID)
	if err != nil || len(missions) != 1 || missions[0].ShipID != live.ShipID {
		t.Fatalf("healthy account failed archive migration: %+v %v", missions, err)
	}
	if archive, err := reopened.PassengerQuarantineArchive(ctx, live.AccountID, "healthy-legacy"); err != nil || archive == nil {
		t.Fatalf("healthy archive was not moved: %+v %v", archive, err)
	}
	var retained int64
	if err := db.db.Model(&missionRow{}).Where("account_id = ?", broken.AccountID).Count(&retained).Error; err != nil || retained != 2 {
		t.Fatalf("migration discarded unreadable recovery state: %d %v", retained, err)
	}
	warnings := logs.FilterMessage("passenger quarantine migration retained unreadable recovery rows").All()
	if len(warnings) != 1 || warnings[0].ContextMap()["skipped_rows"] != int64(2) {
		t.Fatalf("unreadable recovery rows not visibly summarized: %+v", warnings)
	}
}

func TestPassengerQuarantineMarkersRequireArchiveAndStayUnknownAfterRetention(t *testing.T) {
	db, _, _ := integrationStore(t)
	ctx := context.Background()
	const account = "quarantine-economic-marker"
	saveTestAccount(t, db, account)
	marker := Settlement{ID: "economic-marker", AccountID: account, ShipID: "ship", MissionID: "generation", Kind: "passage", Lane: "origin|destination", Origin: "origin", Destination: "destination", CostsKnown: false, ProceedsUnknown: true, RevenueBasis: new("quarantined_passage_accounting"), ProvisionsPence: 100, CompletedAt: time.Now().UTC()}
	if err := db.PutPassengerQuarantineSettlement(ctx, &marker); !errors.Is(err, ErrNotFound) {
		t.Fatalf("unanchored unknown marker accepted: %v", err)
	}
	archive := passengerQuarantineFixture(account, marker.ShipID, marker.MissionID)
	if err := db.PutPassengerQuarantineArchive(ctx, marker.ShipID, &archive); err != nil {
		t.Fatal(err)
	}
	wrong := marker
	wrong.ShipID = "foreign-hull"
	if err := db.PutPassengerQuarantineSettlement(ctx, &wrong); !errors.Is(err, ErrActionConflict) {
		t.Fatalf("marker crossed recovery ownership: %v", err)
	}
	if err := db.PutPassengerQuarantineSettlement(ctx, &marker); err != nil {
		t.Fatal(err)
	}
	if err := db.PutPassengerQuarantineSettlement(ctx, &marker); err != nil {
		t.Fatalf("exact marker replay failed: %v", err)
	}
	conflicting := marker
	conflicting.ProvisionsPence++
	if err := db.PutPassengerQuarantineSettlement(ctx, &conflicting); !errors.Is(err, ErrActionConflict) {
		t.Fatalf("unknown marker accounting changed: %v", err)
	}
	known := marker
	known.ID = "invented-income"
	known.CostsKnown = true
	if err := db.PutPassengerQuarantineSettlement(ctx, &known); !errors.Is(err, ErrInvalidInput) {
		t.Fatalf("quarantine marker became measured income: %v", err)
	}
	if err := db.ReleaseQuarantinedPassengerHull(ctx, account, marker.ShipID); err != nil {
		t.Fatal(err)
	}
	if err := db.db.Exec("UPDATE passenger_quarantine_archives SET closed_at = ? WHERE account_id = ?", time.Now().Add(-8*24*time.Hour), account).Error; err != nil {
		t.Fatal(err)
	}
	if err := db.Prune(ctx); err != nil {
		t.Fatal(err)
	}
	if archive, err := db.PassengerQuarantineArchive(ctx, account, marker.MissionID); err != nil || archive != nil {
		t.Fatalf("raw quarantine evidence exceeded retention: %+v %v", archive, err)
	}
	if err := db.PutPassengerQuarantineSettlement(ctx, &marker); err != nil {
		t.Fatalf("retention broke immutable marker replay: %v", err)
	}
	history, err := db.EconomicHistoryWindow(ctx, account, "", time.Now().UTC(), 24*time.Hour)
	if err != nil || !history.UnknownCosts || history.UnknownLossPence != -100 || history.PositiveCycles != 0 || history.GameSeconds != 0 {
		t.Fatalf("retention erased unknown-cost evidence or measured fictional fares: %+v %v", history, err)
	}
	rows, err := db.SettlementHistory(ctx, account, marker.Lane, 20)
	if err != nil || len(rows) != 1 || !rows[0].ProceedsUnknown || rows[0].CostsKnown {
		t.Fatalf("unknown-cost settlement marker lost: %+v %v", rows, err)
	}
}
