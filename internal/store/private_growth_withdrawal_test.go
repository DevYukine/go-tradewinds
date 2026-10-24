package store

import (
	"bytes"
	"context"
	"encoding/json"
	"testing"
	"time"

	"go.uber.org/zap"
)

func TestPrivateGrowthMigrationWithdrawsActionFreePaidControlOnly(t *testing.T) {
	for _, actionState := range []string{"absent", "null"} {
		t.Run(actionState, func(t *testing.T) {
			legacy, dsn, key := privateLegacyStore(t)
			ctx := context.Background()
			const accountID = "action-free-growth-upgrade"
			saveTestAccount(t, legacy, accountID)
			data := json.RawMessage(`{"plan":{"kind":"spot","origin":"home","destination":"buyer","quantity":7},"built_ship_id":"paid-hull","provisions_pence":221,"fees_pence":37,"costs_known":true,"unknown_accounting":{"paid_pence":9007199254740993}}`)
			if actionState == "null" {
				data = json.RawMessage(`{"plan":{"kind":"spot","origin":"home","destination":"buyer","quantity":7},"built_ship_id":"paid-hull","provisions_pence":221,"fees_pence":37,"costs_known":true,"unknown_accounting":{"paid_pence":9007199254740993},"action":null,"growth_lane":null}`)
			}
			control := Mission{
				AccountID: accountID, ShipID: "__growth", Phase: "growth_provision",
				Generation: "original-paid-admission", Data: data,
				UpdatedAt: time.Now().UTC().Add(-time.Hour).Truncate(time.Microsecond),
			}
			ordinary := control
			ordinary.ShipID = "ordinary-ship"
			for _, mission := range []*Mission{&control, &ordinary} {
				if err := legacy.db.Create(mission).Error; err != nil {
					t.Fatal(err)
				}
				mission.Data = legacyMissionJSON(t, legacy, accountID, mission.ShipID)
			}
			database, err := Open(ctx, dsn, key, zap.NewNop())
			if err != nil {
				t.Fatal(err)
			}
			defer database.Close()
			loaded, err := database.GetMission(ctx, accountID, control.ShipID)
			if err != nil || loaded == nil || loaded.Generation != control.Generation || loaded.Phase != control.Phase || !loaded.UpdatedAt.Equal(control.UpdatedAt) {
				t.Fatalf("paid action-free control changed during migration: %+v, %v", loaded, err)
			}
			var before, after map[string]json.RawMessage
			if err := json.Unmarshal(control.Data, &before); err != nil {
				t.Fatal(err)
			}
			if err := json.Unmarshal(loaded.Data, &after); err != nil {
				t.Fatal(err)
			}
			var sameLane bool
			if err := database.db.Raw("SELECT ?::jsonb = ?::jsonb", string(after["growth_lane"]),
				`{"admission_id":"original-paid-admission","quantity_limit":0,"initial_procurement_limit_pence":0,"initial_procurement_complete":true}`).Scan(&sameLane).Error; err != nil || !sameLane {
				t.Fatalf("action-free paid control lacks explicit zero-procurement withdrawal: %v", err)
			}
			delete(before, "growth_lane")
			delete(after, "growth_lane")
			beforeJSON, err := json.Marshal(before)
			if err != nil {
				t.Fatal(err)
			}
			afterJSON, err := json.Marshal(after)
			if err != nil {
				t.Fatal(err)
			}
			var sameRecovery bool
			if err := database.db.Raw("SELECT ?::jsonb = ?::jsonb", string(beforeJSON), string(afterJSON)).Scan(&sameRecovery).Error; err != nil || !sameRecovery {
				t.Fatalf("withdrawal changed paid costs, hull, plan or unknown accounting: %v", err)
			}
			loadedOrdinary, err := database.GetMission(ctx, accountID, ordinary.ShipID)
			if err != nil || loadedOrdinary == nil || !bytes.Equal(loadedOrdinary.Data, ordinary.Data) {
				t.Fatalf("ordinary mission was treated as a legacy growth admission: %+v, %v", loadedOrdinary, err)
			}
		})
	}
}
