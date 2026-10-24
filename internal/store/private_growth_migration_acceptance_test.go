package store

import (
	"bytes"
	"context"
	"encoding/json"
	"reflect"
	"testing"
	"time"

	"go.uber.org/zap"
)

func TestPrivateGrowthMigrationExplicitlyClosesLegacyAllocationWithoutChangingRecovery(t *testing.T) {
	for _, phase := range []string{"growth_build", "growth_refit"} {
		for _, lane := range []string{"absent", "null", "existing"} {
			t.Run(phase+"/"+lane, func(t *testing.T) {
				legacy, dsn, key := privateLegacyStore(t)
				ctx := context.Background()
				const accountID = "legacy-growth-upgrade"
				const generation = "original-growth-admission"
				saveTestAccount(t, legacy, accountID)
				marshal := func(value any) json.RawMessage {
					t.Helper()
					encoded, err := json.Marshal(value)
					if err != nil {
						t.Fatal(err)
					}
					return encoded
				}
				path := "/v1/shipyard/build-ship"
				body := json.RawMessage(`{ "port_id" : "home", "name" : "Lapras", "ship_class" : "sloop", "build_mode" : "turnkey_cash", "future" : 9007199254740993 }`)
				stage, builtShipID, buildCost := "build", "", int64(0)
				if phase == "growth_refit" {
					path = "/v1/shipyard/modify-ship"
					body = json.RawMessage(`{ "ship_id" : "paid-hull", "target_cannons_count" : 2, "future" : 9007199254740993 }`)
					stage, builtShipID, buildCost = "refit", "paid-hull", 35000
				}
				intent := Action{ID: "original-growth-attempt", AccountID: accountID, Method: "POST", Path: path, Body: body}
				if _, err := legacy.BeginAction(ctx, &intent); err != nil {
					t.Fatal(err)
				}
				var originalAction actionRow
				if err := legacy.db.Where("id = ?", intent.ID).Take(&originalAction).Error; err != nil {
					t.Fatal(err)
				}
				members := map[string]json.RawMessage{
					"plan": json.RawMessage(`{"origin":"home","destination":"buyer","commodity":"grain","quantity":7,"buy_limit_pence":125,"economics":{"purchase_pence":875,"net_pence":450},"future_plan":{"integer":9007199254740993}}`),
					"action": marshal(map[string]any{
						"id": intent.ID, "method": intent.Method, "path": intent.Path, "stage": stage,
						"body": body, "before": map[string]any{"built_ship_id": builtShipID},
						"unknown_attempt": json.RawMessage(`{"integer":9007199254740993}`),
					}),
					"built_ship_id":                marshal(builtShipID),
					"build_cost_pence":             marshal(buildCost),
					"acquisition_pence":            json.RawMessage(`875`),
					"provisions_pence":             json.RawMessage(`221`),
					"fees_pence":                   json.RawMessage(`37`),
					"expenses_pence":               json.RawMessage(`19`),
					"costs_known":                  json.RawMessage(`true`),
					"last_action_id":               json.RawMessage(`"paid-commissioning-remittance"`),
					"growth_working_capital_pence": json.RawMessage(`7000`),
					"unknown_accounting":           json.RawMessage(`{"paid_pence":9007199254740993,"items":[{"fee_pence":13}]}`),
					"future_recovery":              json.RawMessage(`{"token":"retain-exactly","integer":9007199254740993}`),
				}
				var expectedLane json.RawMessage
				switch lane {
				case "null":
					members["growth_lane"] = json.RawMessage(`null`)
				case "existing":
					expectedLane = json.RawMessage(`{"admission_id":"already-allocated","quantity_limit":7,"initial_procurement_limit_pence":1200,"initial_procurement_complete":false,"next_plan":{"origin":"buyer","destination":"home","quantity":3},"future_allocation":{"integer":9007199254740993}}`)
					members["growth_lane"] = expectedLane
				}
				if lane != "existing" {
					expectedLane = marshal(map[string]any{
						"admission_id": generation, "quantity_limit": 0,
						"initial_procurement_limit_pence": 0, "initial_procurement_complete": true,
					})
				}
				original := Mission{
					AccountID: accountID, ShipID: "__growth", Phase: phase, Generation: generation,
					Destination: "home", Commodity: "grain", Quantity: 7, PricePence: 125,
					Data: marshal(members), UpdatedAt: time.Now().UTC().Add(-time.Hour).Truncate(time.Microsecond),
				}
				if err := legacy.db.Create(&original).Error; err != nil {
					t.Fatal(err)
				}
				// Compare the persisted v4 metadata, not the pre-insert time.Location
				// representation, against the decoded v5 recovery.
				var persisted Mission
				if err := legacy.db.Table("missions").
					Select("account_id, ship_id, phase, destination, commodity, contract_id, quantity, price_pence, generation, updated_at").
					Where("account_id = ? AND ship_id = ?", accountID, original.ShipID).Take(&persisted).Error; err != nil {
					t.Fatal(err)
				}
				original = persisted
				original.Data = legacyMissionJSON(t, legacy, accountID, original.ShipID)
				database, err := Open(ctx, dsn, key, zap.NewNop())
				if err != nil {
					t.Fatalf("upgrade legacy growth recovery: %v", err)
				}
				defer database.Close()
				var version int64
				if err := database.db.Raw("SELECT MAX(version_id) FROM goose_db_version WHERE is_applied").Scan(&version).Error; err != nil || version != 5 {
					t.Fatalf("growth upgrade did not complete v5 cutover: %d, %v", version, err)
				}
				loaded, err := database.GetMission(ctx, accountID, original.ShipID)
				if err != nil || loaded == nil {
					t.Fatalf("load migrated growth recovery: %v", err)
				}
				originalMetadata, loadedMetadata := original, *loaded
				originalMetadata.Data, loadedMetadata.Data = nil, nil
				if !originalMetadata.UpdatedAt.Equal(loadedMetadata.UpdatedAt) {
					t.Error("upgrade changed the persisted growth observation timestamp")
				}
				originalMetadata.UpdatedAt, loadedMetadata.UpdatedAt = time.Time{}, time.Time{}
				if !reflect.DeepEqual(originalMetadata, loadedMetadata) {
					t.Error("upgrade changed growth identity, phase, generation, money or replay metadata")
				}
				var migratedMembers map[string]json.RawMessage
				if err := json.Unmarshal(loaded.Data, &migratedMembers); err != nil {
					t.Fatal(err)
				}
				var sameLane bool
				actualLane := migratedMembers["growth_lane"]
				if len(actualLane) == 0 {
					t.Error("v5 growth recovery lacks an explicit allocation contract")
				} else if err := database.db.Raw("SELECT ?::jsonb = ?::jsonb", string(actualLane), string(expectedLane)).Scan(&sameLane).Error; err != nil || !sameLane {
					t.Errorf("v5 allocation is not the explicit zero-procurement closure or preserved existing allocation: %v", err)
				}
				// The only permitted changes are the allocation marker and the existing
				// v5 conversion from nested JSON to exact opaque journal request bytes.
				var originalMembers map[string]json.RawMessage
				if err := json.Unmarshal(original.Data, &originalMembers); err != nil {
					t.Fatal(err)
				}
				delete(originalMembers, "growth_lane")
				delete(migratedMembers, "growth_lane")
				assertMigratedMissionBody(t, database, marshal(originalMembers), marshal(migratedMembers), body)
				var migratedAction actionRow
				if err := database.db.Where("id = ?", intent.ID).Take(&migratedAction).Error; err != nil {
					t.Fatal(err)
				}
				if !reflect.DeepEqual(originalAction, migratedAction) {
					t.Error("growth upgrade changed original attempted journal key, ciphertext, state or replay window")
				}
				replay, err := database.BeginAction(ctx, &intent)
				if err != nil || replay == nil || replay.ID != intent.ID || replay.State != ActionPending || !bytes.Equal(replay.Body, body) || !replay.CreatedAt.Equal(originalAction.CreatedAt) {
					t.Fatalf("migrated growth attempt cannot replay its original key and exact payload: %v", err)
				}
				if err := database.PutMission(ctx, loaded); err != nil {
					t.Fatal(err)
				}
				restarted, err := Open(ctx, dsn, key, zap.NewNop())
				if err != nil {
					t.Fatal(err)
				}
				defer restarted.Close()
				roundTrip, err := restarted.GetMission(ctx, accountID, original.ShipID)
				if err != nil || roundTrip == nil || !bytes.Equal(roundTrip.Data, loaded.Data) || roundTrip.Generation != generation || roundTrip.Phase != phase {
					t.Fatalf("growth allocation or paid recovery changed across save/restart: %v", err)
				}
				var recovery struct {
					Action struct {
						ID   string `json:"id"`
						Body []byte `json:"body"`
					} `json:"action"`
				}
				if err := json.Unmarshal(roundTrip.Data, &recovery); err != nil {
					t.Fatal(err)
				}
				if recovery.Action.ID != intent.ID || !bytes.Equal(recovery.Action.Body, body) {
					t.Errorf("%s growth recovery lost exact attempted request after restart", phase)
				}
			})
		}
	}
}
