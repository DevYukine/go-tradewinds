package store

import (
	"bytes"
	"context"
	"encoding/json"
	"reflect"
	"testing"
	"time"

	"gorm.io/gorm"
)

func dashboardLossJSON(t *testing.T, value any) json.RawMessage {
	t.Helper()
	encoded, err := json.Marshal(value)
	if err != nil {
		t.Fatal(err)
	}
	return encoded
}

func saveDashboardLossAccount(t *testing.T, database *Store, accountID string) Credentials {
	t.Helper()
	credentials := saveTestAccount(t, database, accountID)
	ctx := context.Background()
	if err := database.SaveSnapshot(ctx, accountID, map[string]json.RawMessage{
		"ships": dashboardLossJSON(t, []map[string]string{{"id": "working-" + accountID}}),
	}); err != nil {
		t.Fatal(err)
	}
	mission := Mission{
		AccountID: accountID, ShipID: "working-" + accountID, Phase: "sailing",
		Data: json.RawMessage(`{"plan":{"kind":"spot","origin":"london","destination":"boston","commodity":"grain","quantity":7}}`),
	}
	if err := database.PutMission(ctx, &mission); err != nil {
		t.Fatal(err)
	}
	if err := database.SaveShipLosses(ctx, accountID, []ShipLoss{{
		ShipID: "lost-" + accountID, ShipName: "Lapras", ShipClass: "hoy",
		Fate: "lost_at_sea", Status: "sunk", ObservedAt: time.Now().UTC(),
	}}); err != nil {
		t.Fatal(err)
	}
	return credentials
}

func dashboardLossHydration(t *testing.T, database *Store) map[string]int {
	t.Helper()
	hydrated := make(map[string]int)
	const callback = "test:dashboard-loss-hydration"
	if err := database.db.Callback().Query().After("gorm:query").Register(callback, func(tx *gorm.DB) {
		if rows, ok := tx.Statement.Dest.(*[]ShipLoss); ok {
			for _, row := range *rows {
				hydrated[row.AccountID]++
			}
		}
	}); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := database.db.Callback().Query().Remove(callback); err != nil {
			t.Error(err)
		}
	})
	return hydrated
}

func assertDashboardLossUnavailable(t *testing.T, account DashboardAccount) {
	t.Helper()
	if !account.ShipLossesUnavailable || account.ShipLosses == nil || len(account.ShipLosses) != 0 {
		t.Fatalf("account %q did not explicitly withhold its loss history: %+v", account.ID, account.ShipLosses)
	}
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(dashboardLossJSON(t, account), &fields); err != nil {
		t.Fatal(err)
	}
	if string(fields["ship_losses"]) != "[]" || string(fields["ship_losses_unavailable"]) != "true" {
		t.Fatal("unavailable history did not serialize as ship_losses:[] and ship_losses_unavailable:true")
	}
}

func TestPostgresDashboardStripsShipLossRecorderStateWithoutChangingSnapshot(t *testing.T) {
	database, _, _ := integrationStore(t)
	ctx := context.Background()
	const accountID = "recorder-display"
	saveTestAccount(t, database, accountID)
	const private = "retained-recorder-state-must-not-be-displayed"
	internalKeys := []string{
		"_pending_ship_losses", "_ship_loss_verified_ships", "_ship_loss_legacy_ships",
		"_ship_loss_event_cursor", "_ship_loss_events_backfill", "_ship_loss_future_state",
	}
	publicKeys := []string{"_pending_ship_losses_extra", "_ship_losses", "ship_loss_summary"}
	metadata := map[string]json.RawMessage{
		"ships":       json.RawMessage(`{"complete":true,"stale":false}`),
		"ship_losses": json.RawMessage(`{"complete":true,"stale":false}`),
		"_engine":     json.RawMessage(`{"complete":true,"stale":false}`),
	}
	snapshot := map[string]json.RawMessage{
		"ships":   json.RawMessage(`[{"id":"healthy"}]`),
		"_engine": json.RawMessage(`{"mode":"observe"}`),
	}
	for _, key := range internalKeys {
		snapshot[key] = dashboardLossJSON(t, map[string]string{"private": private})
		metadata[key] = dashboardLossJSON(t, map[string]string{"error": private})
	}
	for _, key := range publicKeys {
		snapshot[key] = json.RawMessage(`{"public":true}`)
		metadata[key] = json.RawMessage(`{"complete":true}`)
	}
	snapshot["_meta"] = dashboardLossJSON(t, metadata)
	if err := database.SaveSnapshot(ctx, accountID, snapshot); err != nil {
		t.Fatal(err)
	}
	before, err := database.Snapshot(ctx, accountID)
	if err != nil {
		t.Fatal(err)
	}
	dashboard, err := database.Dashboard(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(dashboard.Accounts) != 1 {
		t.Fatalf("dashboard lost the recorder's account: %d", len(dashboard.Accounts))
	}
	account := dashboard.Accounts[0]
	var displayedMetadata map[string]json.RawMessage
	if err := json.Unmarshal(account.Snapshot["_meta"], &displayedMetadata); err != nil {
		t.Fatal(err)
	}
	for _, key := range internalKeys {
		if _, exists := account.Snapshot[key]; exists {
			t.Errorf("dashboard exposed internal recorder dataset %q", key)
		}
		if _, exists := displayedMetadata[key]; exists {
			t.Errorf("dashboard exposed internal recorder metadata %q", key)
		}
		if len(before[key]) == 0 {
			t.Errorf("fixture did not persist internal recorder dataset %q", key)
		}
	}
	for _, key := range publicKeys {
		if len(account.Snapshot[key]) == 0 || len(displayedMetadata[key]) == 0 {
			t.Errorf("reserved-namespace filtering removed unrelated key %q", key)
		}
	}
	if len(account.Snapshot["ships"]) == 0 || len(account.Snapshot["_engine"]) == 0 ||
		len(displayedMetadata["ships"]) == 0 || len(displayedMetadata["ship_losses"]) == 0 || len(displayedMetadata["_engine"]) == 0 {
		t.Fatal("recorder filtering removed unrelated display data or metadata")
	}
	if bytes.Contains(dashboardLossJSON(t, dashboard), []byte(private)) {
		t.Fatal("recorder-only evidence was visible in the serialized dashboard")
	}
	if account.ShipLossesUnavailable || account.ShipLosses == nil || len(account.ShipLosses) != 0 {
		t.Fatal("a genuinely empty loss history was reported as unavailable or null")
	}
	after, err := database.Snapshot(ctx, accountID)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(before, after) {
		t.Fatal("dashboard filtering changed the engine's durable snapshot")
	}
	var retainedMetadata map[string]json.RawMessage
	if err := json.Unmarshal(after["_meta"], &retainedMetadata); err != nil {
		t.Fatal(err)
	}
	for _, key := range internalKeys {
		if len(after[key]) == 0 || len(retainedMetadata[key]) == 0 {
			t.Errorf("dashboard filtering destroyed retained recorder state %q", key)
		}
	}
}

func TestPostgresDashboardShipLossBudgetsWithholdOnlyAffectedAccountBeforeHydration(t *testing.T) {
	for _, mode := range []string{"per_account", "shared_snapshot_budget"} {
		t.Run(mode, func(t *testing.T) {
			database, _, _ := integrationStore(t)
			ctx := context.Background()
			const affectedID = "loss-budget-affected"
			const healthyID = "loss-budget-healthy"
			var secrets []string
			for _, accountID := range []string{affectedID, healthyID} {
				secrets = append(secrets, credentialSecrets(saveDashboardLossAccount(t, database, accountID))...)
			}
			padding := MaxSnapshotBytes + 1
			if mode == "shared_snapshot_budget" {
				// Leave a small shared budget even though both snapshot rows fit
				// their individual cap. Only the small peer history can fit.
				if err := database.db.Exec(`UPDATE snapshots SET data = data ||
					jsonb_build_object('legacy_padding', repeat('x', ?))`, MaxSnapshotBytes-2048).Error; err != nil {
					t.Fatal(err)
				}
				padding = 8192
			}
			// Model legacy evidence without allocating or hydrating its payload
			// in Go. Current writers independently bound individual evidence.
			if err := database.db.Exec(`UPDATE ship_losses SET cargo_manifest =
				jsonb_build_object('legacy', repeat('x', ?)) WHERE account_id = ?`, padding, affectedID).Error; err != nil {
				t.Fatal(err)
			}
			var snapshotBytes, affectedBytes, healthyBytes int64
			if err := database.db.Table("snapshots").Select("SUM(octet_length(data::text)::bigint)").Scan(&snapshotBytes).Error; err != nil {
				t.Fatal(err)
			}
			for accountID, size := range map[string]*int64{affectedID: &affectedBytes, healthyID: &healthyBytes} {
				if err := database.db.Table("ship_losses").Select("SUM(octet_length(row_to_json(ship_losses)::text)::bigint)").
					Where("account_id = ?", accountID).Scan(size).Error; err != nil {
					t.Fatal(err)
				}
			}
			if snapshotBytes >= maxDashboardSnapshotBytes || healthyBytes > maxDashboardSnapshotBytes-snapshotBytes {
				t.Fatal("fixture did not leave room for the healthy snapshot and loss history")
			}
			if mode == "per_account" {
				if affectedBytes <= MaxSnapshotBytes || affectedBytes+healthyBytes+snapshotBytes > maxDashboardSnapshotBytes {
					t.Fatal("fixture did not independently exceed only the per-account history cap")
				}
			} else if affectedBytes > MaxSnapshotBytes || affectedBytes+healthyBytes+snapshotBytes <= maxDashboardSnapshotBytes {
				t.Fatal("fixture did not exceed the shared snapshot budget while fitting the per-account history cap")
			}
			hydrated := dashboardLossHydration(t, database)
			dashboard, err := database.Dashboard(ctx)
			if err != nil {
				t.Fatalf("one oversized loss history failed the whole dashboard: %v", err)
			}
			if len(dashboard.Accounts) != 2 {
				t.Fatalf("one oversized loss history lost peer accounts: %d", len(dashboard.Accounts))
			}
			for _, account := range dashboard.Accounts {
				var ships []struct {
					ID string `json:"id"`
				}
				if err := json.Unmarshal(account.Snapshot["ships"], &ships); err != nil || len(ships) != 1 ||
					ships[0].ID != "working-"+account.ID || account.UpdatedAt == nil {
					t.Fatalf("loss history overflow changed account %q snapshot: %v", account.ID, err)
				}
				if account.MissionsUnavailable || len(account.Missions) != 1 || account.Missions[0].ShipID != "working-"+account.ID {
					t.Fatalf("loss history overflow changed account %q missions", account.ID)
				}
				if account.ID == affectedID {
					assertDashboardLossUnavailable(t, account)
				} else if account.ShipLossesUnavailable || len(account.ShipLosses) != 1 || account.ShipLosses[0].ShipID != "lost-"+healthyID {
					t.Fatal("oversized sibling withheld or changed the healthy account's loss history")
				}
			}
			if hydrated[affectedID] != 0 || hydrated[healthyID] != 1 {
				t.Fatalf("loss preflight hydrated rejected history or skipped its healthy peer: %+v", hydrated)
			}
			var retainedBytes int64
			if err := database.db.Table("ship_losses").Select("SUM(octet_length(row_to_json(ship_losses)::text)::bigint)").
				Where("account_id = ?", affectedID).Scan(&retainedBytes).Error; err != nil {
				t.Fatal(err)
			}
			if retainedBytes != affectedBytes {
				t.Fatal("display withholding changed the durable loss history")
			}
			encoded := dashboardLossJSON(t, dashboard)
			for _, secret := range secrets {
				if bytes.Contains(encoded, []byte(secret)) {
					t.Fatal("bounded dashboard exposed an account credential")
				}
			}
		})
	}
}

func TestPostgresDashboardShipLossUnavailableCredentialsFlag(t *testing.T) {
	for _, mode := range []string{"corrupt", "missing", "missing_empty", "missing_no_snapshot"} {
		t.Run(mode, func(t *testing.T) {
			database, _, _ := integrationStore(t)
			ctx := context.Background()
			const affectedID = "loss-credentials-unavailable"
			const healthyID = "loss-credentials-healthy"
			credentials := saveDashboardLossAccount(t, database, affectedID)
			saveDashboardLossAccount(t, database, healthyID)
			const private = "unsafe-retained-loss-canary"
			if err := database.db.Model(&ShipLoss{}).Where("account_id = ?", affectedID).
				Update("ship_name", private+credentials.Passphrase).Error; err != nil {
				t.Fatal(err)
			}
			if mode == "missing_empty" {
				if err := database.db.Where("account_id = ?", affectedID).Delete(&ShipLoss{}).Error; err != nil {
					t.Fatal(err)
				}
			}
			if mode == "missing_no_snapshot" {
				if err := database.db.Where("account_id = ?", affectedID).Delete(&snapshotRow{}).Error; err != nil {
					t.Fatal(err)
				}
			}
			if mode == "corrupt" {
				if err := database.db.Model(&accountSecret{}).Where("account_id = ?", affectedID).
					Update("ciphertext", []byte("corrupt")).Error; err != nil {
					t.Fatal(err)
				}
			} else if err := database.db.Where("account_id = ?", affectedID).Delete(&accountSecret{}).Error; err != nil {
				t.Fatal(err)
			}
			hydrated := dashboardLossHydration(t, database)
			dashboard, err := database.Dashboard(ctx)
			if err != nil {
				t.Fatalf("unavailable credentials prevented the healthy dashboard read: %v", err)
			}
			if len(dashboard.Accounts) != 2 {
				t.Fatal("credential failure omitted an account")
			}
			for _, account := range dashboard.Accounts {
				if account.ID == affectedID {
					assertDashboardLossUnavailable(t, account)
					var metadata map[string]struct {
						Complete bool   `json:"complete"`
						Stale    bool   `json:"stale"`
						Error    string `json:"error"`
					}
					if err := json.Unmarshal(account.Snapshot["_meta"], &metadata); err != nil {
						t.Fatal(err)
					}
					entry, exists := metadata["ship_losses"]
					if !exists || entry.Complete || !entry.Stale ||
						entry.Error != "Account credentials unavailable; retained ship losses cannot be safely displayed." {
						t.Fatal("credential failure lost the safe account-local loss metadata error")
					}
				} else if account.ShipLossesUnavailable || len(account.ShipLosses) != 1 ||
					account.ShipLosses[0].ShipID != "lost-"+healthyID || len(account.Snapshot["ships"]) == 0 ||
					account.MissionsUnavailable || len(account.Missions) != 1 {
					t.Fatal("credential failure withheld the healthy account's recorded data")
				}
			}
			if hydrated[affectedID] != 0 || hydrated[healthyID] != 1 {
				t.Fatalf("credential failure hydrated unsafe loss history or skipped its peer: %+v", hydrated)
			}
			encoded := dashboardLossJSON(t, dashboard)
			for _, secret := range append(credentialSecrets(credentials), private) {
				if bytes.Contains(encoded, []byte(secret)) {
					t.Fatal("credential failure exposed retained loss evidence or a credential")
				}
			}
		})
	}
}
