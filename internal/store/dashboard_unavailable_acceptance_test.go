package store

import (
	"bytes"
	"context"
	"encoding/json"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/DevYukine/go-tradewinds/internal/gameapi"
)

func TestDashboardUnavailableCredentialsOmitDatasetsAndKeepHealthyAccountTyped(t *testing.T) {
	for _, mode := range []string{"corrupt", "missing"} {
		t.Run(mode, func(t *testing.T) {
			database, _, _ := integrationStore(t)
			ctx := context.Background()
			const healthyID = "dashboard-healthy"
			const unavailableID = "dashboard-unavailable"
			healthyCredentials := saveTestAccount(t, database, healthyID)
			unavailableCredentials := saveTestAccount(t, database, unavailableID)
			observedAt := time.Now().UTC().Truncate(time.Microsecond)
			const unsafeMetadata = "retained-private-metadata-canary"
			marshal := func(value any) json.RawMessage {
				t.Helper()
				encoded, err := json.Marshal(value)
				if err != nil {
					t.Fatal(err)
				}
				return encoded
			}
			ships := []gameapi.ShipDetail{{
				Ship:             gameapi.Ship{ID: "healthy-ship", CompanyID: "company-" + healthyID, Name: "Lapras", Status: "berthed"},
				HoldCapacityTons: 40,
				CargoManifest:    map[string]int32{"grain": 7},
			}}
			profile := gameapi.Profile{
				Company:    gameapi.Company{ID: "company-" + healthyID, Name: "Healthy company"},
				Handle:     "healthy-player",
				FleetCount: 1,
			}
			catalog := gameapi.Catalog{
				Ports:       []gameapi.Port{{ID: "healthy-port", Name: "Healthy port"}},
				Commodities: []gameapi.Commodity{{ID: "grain", Name: "Grain", BasePricePence: 125}},
			}
			datasets := []string{"ships", "profile", "catalog", "_engine"}
			healthyMeta := make(map[string]any)
			unavailableMeta := make(map[string]any)
			for _, key := range datasets {
				healthyMeta[key] = map[string]any{
					"updated_at": observedAt, "attempted_at": observedAt, "generation": "healthy-generation",
					"complete": true, "stale": false, "error": "",
				}
				unavailableMeta[key] = map[string]any{
					"updated_at": observedAt, "attempted_at": observedAt,
					"generation": unsafeMetadata + unavailableCredentials.Passphrase,
					"complete":   true, "stale": false,
					"error": unsafeMetadata + unavailableCredentials.APIKey,
				}
			}
			healthySnapshot := map[string]json.RawMessage{
				"ships":   marshal(ships),
				"profile": marshal(profile),
				"catalog": marshal(catalog),
				"_engine": marshal(map[string]any{
					"mode": "observe", "holds": []any{}, "assessed_ship_ids": []string{"healthy-ship"},
					"generation": "healthy-generation", "observed_at": observedAt, "next_check_at": observedAt.Add(time.Minute),
				}),
				"_meta": marshal(healthyMeta),
			}
			if err := database.SaveSnapshot(ctx, healthyID, healthySnapshot); err != nil {
				t.Fatal(err)
			}
			// Retained rows can predate write-time scrubbing; display must remain safe
			// when their account credentials can no longer be decrypted or found.
			unavailableSnapshot := map[string]json.RawMessage{
				"ships": marshal([]gameapi.ShipDetail{{Ship: gameapi.Ship{
					ID: "unavailable-ship", Name: unavailableCredentials.Passphrase,
				}}}),
				"profile": marshal(gameapi.Profile{Company: gameapi.Company{Name: unavailableCredentials.APIKey}}),
				"catalog": marshal(gameapi.Catalog{Ports: []gameapi.Port{{Name: unavailableCredentials.Passphrase}}}),
				"_engine": marshal(map[string]any{
					"mode": "execute", "holds": []string{unavailableCredentials.APIKey},
					"generation": unsafeMetadata, "observed_at": observedAt,
				}),
				"_meta": marshal(unavailableMeta),
			}
			retained := snapshotRow{AccountID: unavailableID, Data: marshal(unavailableSnapshot), UpdatedAt: observedAt}
			if err := database.db.Create(&retained).Error; err != nil {
				t.Fatal(err)
			}
			var before snapshotRow
			if err := database.db.Where("account_id = ?", unavailableID).Take(&before).Error; err != nil {
				t.Fatal(err)
			}
			if mode == "corrupt" {
				if err := database.db.Model(&accountSecret{}).Where("account_id = ?", unavailableID).
					Update("ciphertext", []byte("corrupt")).Error; err != nil {
					t.Fatal(err)
				}
			} else if err := database.db.Where("account_id = ?", unavailableID).Delete(&accountSecret{}).Error; err != nil {
				t.Fatal(err)
			}
			if _, err := database.Credentials(ctx, unavailableID); err == nil {
				t.Fatal("fixture account credentials remain available")
			}

			dashboard, err := database.Dashboard(ctx)
			if err != nil {
				t.Fatalf("unavailable account prevented fleet dashboard read: %v", err)
			}
			if len(dashboard.Accounts) != 2 {
				t.Fatalf("dashboard lost an account: got %d, want 2", len(dashboard.Accounts))
			}
			accounts := make(map[string]DashboardAccount)
			for _, account := range dashboard.Accounts {
				accounts[account.ID] = account
			}
			healthy, ok := accounts[healthyID]
			if !ok {
				t.Fatal("dashboard omitted healthy account")
			}
			var displayedShips []gameapi.ShipDetail
			if err := json.Unmarshal(healthy.Snapshot["ships"], &displayedShips); err != nil || !reflect.DeepEqual(displayedShips, ships) {
				t.Fatalf("healthy fleet shape or content changed: %v", err)
			}
			var displayedProfile gameapi.Profile
			if err := json.Unmarshal(healthy.Snapshot["profile"], &displayedProfile); err != nil || !reflect.DeepEqual(displayedProfile, profile) {
				t.Fatalf("healthy profile shape or content changed: %v", err)
			}
			var displayedCatalog gameapi.Catalog
			if err := json.Unmarshal(healthy.Snapshot["catalog"], &displayedCatalog); err != nil || !reflect.DeepEqual(displayedCatalog, catalog) {
				t.Fatalf("healthy catalog shape or content changed: %v", err)
			}
			for _, key := range []string{"_engine", "_meta"} {
				var want, got map[string]any
				if err := json.Unmarshal(healthySnapshot[key], &want); err != nil {
					t.Fatal(err)
				}
				if err := json.Unmarshal(healthy.Snapshot[key], &got); err != nil || !reflect.DeepEqual(got, want) {
					t.Errorf("unavailable sibling changed healthy %s object or content: %v", key, err)
				}
			}

			unavailable, ok := accounts[unavailableID]
			if !ok {
				t.Fatal("dashboard omitted unavailable account instead of reporting its data gap")
			}
			for _, key := range datasets {
				if _, exists := unavailable.Snapshot[key]; exists {
					t.Errorf("unavailable %s dataset was returned instead of omitted", key)
				}
			}
			var meta map[string]struct {
				Complete *bool  `json:"complete"`
				Stale    *bool  `json:"stale"`
				Error    string `json:"error"`
			}
			if err := json.Unmarshal(unavailable.Snapshot["_meta"], &meta); err != nil {
				t.Errorf("unavailable account metadata is not a DatasetMeta object: %v", err)
			} else {
				for _, key := range datasets {
					entry, exists := meta[key]
					if !exists || entry.Complete == nil || *entry.Complete || entry.Stale == nil || !*entry.Stale || strings.TrimSpace(entry.Error) == "" {
						t.Errorf("unavailable %s lacks an explicit incomplete, stale, account-local error", key)
					}
				}
			}
			encoded := marshal(dashboard)
			for _, canary := range []string{
				unsafeMetadata, healthyCredentials.APIKey, healthyCredentials.Passphrase,
				unavailableCredentials.APIKey, unavailableCredentials.Passphrase,
			} {
				if bytes.Contains(encoded, []byte(canary)) {
					t.Error("dashboard exposed an original unsafe metadata or credential canary")
				}
			}
			var after snapshotRow
			if err := database.db.Where("account_id = ?", unavailableID).Take(&after).Error; err != nil {
				t.Fatal(err)
			}
			if !bytes.Equal(before.Data, after.Data) || !before.UpdatedAt.Equal(after.UpdatedAt) {
				t.Error("display projection changed retained account recordings")
			}
		})
	}
}
