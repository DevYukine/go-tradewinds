package store

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"testing"
	"time"

	"go.uber.org/zap"
)

func passengerQuarantineFixture(account, ship, generation string) Mission {
	return Mission{AccountID: account, ShipID: PassengerQuarantinePrefix + generation, Generation: generation, Phase: "passage_quarantined", Data: json.RawMessage(fmt.Sprintf(`{"plan":{"ship_id":%q},"passage":{"quarantine_reason":"private-damaged-proof"},"private_cost":12345}`, ship))}
}

func TestPassengerQuarantineArchivesExcludedFromActiveMissionsAndBoundedReads(t *testing.T) {
	db, dsn, key := integrationStore(t)
	ctx := context.Background()
	const account = "passenger-quarantine-private"
	saveTestAccount(t, db, account)
	operating := Mission{AccountID: account, ShipID: "recovering-hull", Generation: "live", Phase: "passenger_recovery", Data: json.RawMessage(`{"passage":{"recovery_only":true}}`)}
	if err := db.PutMission(ctx, &operating); err != nil {
		t.Fatal(err)
	}
	for i := range 105 {
		archive := passengerQuarantineFixture(account, "recovering-hull", fmt.Sprintf("archive-%03d", i))
		if err := db.PutPassengerQuarantineArchive(ctx, "recovering-hull", &archive); err != nil {
			t.Fatal(err)
		}
	}
	var raw string
	if err := db.db.Raw("SELECT to_jsonb(archive)::text FROM passenger_quarantine_archives AS archive LIMIT 1").Scan(&raw).Error; err != nil {
		t.Fatal(err)
	}
	for _, secret := range []string{"recovering-hull", "private-damaged-proof", "private_cost", "archive-000"} {
		if strings.Contains(raw, secret) {
			t.Fatalf("private quarantine metadata leaked %q", secret)
		}
	}
	reopened, err := Open(ctx, dsn, key, zap.NewNop())
	if err != nil {
		t.Fatal(err)
	}
	defer reopened.Close()
	archived, err := reopened.PassengerQuarantineArchive(ctx, account, "archive-104")
	if err != nil || archived == nil || !bytes.Contains(archived.Data, []byte("private-damaged-proof")) {
		t.Fatalf("exact-key restart read failed: %+v %v", archived, err)
	}
	if foreign, err := reopened.PassengerQuarantineArchive(ctx, "foreign-account", "archive-104"); err != nil || foreign != nil {
		t.Fatalf("archive crossed account: %+v %v", foreign, err)
	}
	// An unreadable unrelated archive must not be decrypted by fleet enumeration
	// or by an exact-key read of a healthy archive.
	if err := db.db.Exec("UPDATE passenger_quarantine_archives SET data_cipher = ? WHERE account_id = ? AND ship_lookup = ?", []byte("broken archive"), account, db.privateLookup("mission-ship", account, PassengerQuarantinePrefix+"archive-000")).Error; err != nil {
		t.Fatal(err)
	}
	missions, err := reopened.Missions(ctx, account)
	if err != nil || len(missions) != 1 || missions[0].ShipID != operating.ShipID {
		t.Fatalf("closed archives entered fleet work enumeration: %+v %v", missions, err)
	}
	if archive, err := reopened.PassengerQuarantineArchive(ctx, account, "archive-104"); err != nil || archive == nil {
		t.Fatalf("exact-key read scanned another archive: %+v %v", archive, err)
	}
	if _, err := reopened.PassengerQuarantineArchive(ctx, account, "archive-000"); err == nil {
		t.Fatal("corrupt archive unexpectedly readable")
	}
	if _, err := reopened.PassengerQuarantineArchive(ctx, account, strings.Repeat("x", 1025)); err == nil {
		t.Fatal("unbounded archive lookup accepted")
	}
}

func TestPassengerQuarantineRetentionKeepsUnresolvedRecovery(t *testing.T) {
	db, _, _ := integrationStore(t)
	ctx := context.Background()
	const account = "passenger-quarantine-retention"
	saveTestAccount(t, db, account)
	for _, entry := range []struct {
		ship, generation string
		closedAge        time.Duration
	}{
		{"unresolved-hull", "open", 0}, {"closed-hull", "expired", 8 * 24 * time.Hour}, {"recent-hull", "recent", 6 * 24 * time.Hour},
	} {
		archive := passengerQuarantineFixture(account, entry.ship, entry.generation)
		if err := db.PutPassengerQuarantineArchive(ctx, entry.ship, &archive); err != nil {
			t.Fatal(err)
		}
		if err := db.db.Exec("UPDATE passenger_quarantine_archives SET updated_at = ? WHERE account_id = ? AND ship_lookup = ?", time.Now().Add(-30*24*time.Hour), account, db.privateLookup("mission-ship", account, archive.ShipID)).Error; err != nil {
			t.Fatal(err)
		}
		if entry.closedAge > 0 {
			mission := Mission{AccountID: account, ShipID: entry.ship, Generation: "recovery-" + entry.generation, Phase: "passenger_recovery", Data: json.RawMessage(`{"passage":{"recovery_only":true}}`)}
			if err := db.PutMission(ctx, &mission); err != nil {
				t.Fatal(err)
			}
			if err := db.ReleaseQuarantinedPassengerHull(ctx, account, entry.ship); err != nil {
				t.Fatal(err)
			}
			if current, err := db.GetMission(ctx, account, entry.ship); err != nil || current != nil {
				t.Fatalf("closure did not release operating hull: %+v %v", current, err)
			}
			if err := db.db.Exec("UPDATE passenger_quarantine_archives SET closed_at = ? WHERE account_id = ? AND ship_lookup = ?", time.Now().Add(-entry.closedAge), account, db.privateLookup("mission-ship", account, archive.ShipID)).Error; err != nil {
				t.Fatal(err)
			}
		}
	}
	if err := db.Prune(ctx); err != nil {
		t.Fatal(err)
	}
	for _, generation := range []string{"open", "recent"} {
		if archive, err := db.PassengerQuarantineArchive(ctx, account, generation); err != nil || archive == nil {
			t.Fatalf("protected/recent archive removed: %s %+v %v", generation, archive, err)
		}
	}
	if archive, err := db.PassengerQuarantineArchive(ctx, account, "expired"); err != nil || archive != nil {
		t.Fatalf("closed raw accounting exceeded seven-day retention: %+v %v", archive, err)
	}
}

func TestPassengerQuarantineMigrationMovesLegacySlotsAndPreservesOpenRecovery(t *testing.T) {
	db, _, _ := integrationStore(t)
	ctx := context.Background()
	const account = "passenger-quarantine-cutover"
	saveTestAccount(t, db, account)
	if err := db.db.Exec("DROP TABLE passenger_quarantine_archives").Error; err != nil {
		t.Fatal(err)
	}
	operating := Mission{AccountID: account, ShipID: "unresolved-hull", Generation: "recovery", Phase: "passenger_recovery", Data: json.RawMessage(`{"passage":{"recovery_only":true}}`)}
	if err := db.PutMission(ctx, &operating); err != nil {
		t.Fatal(err)
	}
	for i := range 103 {
		ship := "closed-hull"
		if i == 102 {
			ship = "unresolved-hull"
		}
		archive := passengerQuarantineFixture(account, ship, fmt.Sprintf("legacy-%03d", i))
		if err := db.PutMission(ctx, &archive); err != nil {
			t.Fatal(err)
		}
		if err := db.db.Exec("UPDATE missions SET updated_at = ? WHERE account_id = ? AND ship_lookup = ?", time.Now().Add(-8*24*time.Hour), account, db.privateLookup("mission-ship", account, archive.ShipID)).Error; err != nil {
			t.Fatal(err)
		}
	}
	tx, err := db.sqlDB.BeginTx(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback()
	if err := db.migratePassengerQuarantineArchives(ctx, tx); err != nil {
		t.Fatal(err)
	}
	if err := tx.Commit(); err != nil {
		t.Fatal(err)
	}
	missions, err := db.Missions(ctx, account)
	if err != nil || len(missions) != 1 || missions[0].ShipID != "unresolved-hull" {
		t.Fatalf("migration retained legacy archive slots: %+v %v", missions, err)
	}
	if err := db.Prune(ctx); err != nil {
		t.Fatal(err)
	}
	if old, err := db.PassengerQuarantineArchive(ctx, account, "legacy-000"); err != nil || old == nil {
		t.Fatalf("migration guessed an earlier closure date: %+v %v", old, err)
	}
	result := db.db.Exec("UPDATE passenger_quarantine_archives SET closed_at = ? WHERE account_id = ? AND closed_at IS NOT NULL", time.Now().Add(-8*24*time.Hour), account)
	if result.Error != nil || result.RowsAffected != 102 {
		t.Fatalf("migration did not distinguish closed archives from unresolved recovery: %d %v", result.RowsAffected, result.Error)
	}
	if err := db.Prune(ctx); err != nil {
		t.Fatal(err)
	}
	if old, err := db.PassengerQuarantineArchive(ctx, account, "legacy-000"); err != nil || old != nil {
		t.Fatalf("legacy closed archive not subject to retention: %+v %v", old, err)
	}
	if open, err := db.PassengerQuarantineArchive(ctx, account, "legacy-102"); err != nil || open == nil {
		t.Fatalf("migration pruned unresolved recovery state: %+v %v", open, err)
	}
}
