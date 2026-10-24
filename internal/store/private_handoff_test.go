package store

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"sync"
	"testing"
)

func privateHandoffMissions(accountID string) (Mission, Mission) {
	control := Mission{
		AccountID: accountID, ShipID: "__growth", Generation: "growth-admission", Phase: "commissioning",
		Data: json.RawMessage(`{"built_ship_id":"built-ship","growth_lane":{"admission_id":"growth-admission","quantity_limit":7}}`),
	}
	next := Mission{
		AccountID: accountID, ShipID: "built-ship", Generation: "distinct-first-voyage", Phase: "procure",
		Destination: "lisbon", Commodity: "grain", Quantity: 7,
		Data: json.RawMessage(`{"growth_lane":{"admission_id":"growth-admission","quantity_limit":7},"initial_procurement_limit_pence":4200}`),
	}
	return control, next
}

func TestGrowthHandoffRetainsAnAdvancingMatchingMissionOnRetry(t *testing.T) {
	database, _, _ := integrationStore(t)
	ctx := context.Background()
	const accountID = "private-handoff"
	saveTestAccount(t, database, accountID)
	control, next := privateHandoffMissions(accountID)
	if err := database.PutMission(ctx, &control); err != nil {
		t.Fatal(err)
	}
	first, err := database.HandoffGrowthMission(ctx, accountID, control.Generation, &next)
	if err != nil || first == nil || !bytes.Equal(first.Data, next.Data) {
		t.Fatalf("commissioning did not become its bounded first voyage: %+v, %v", first, err)
	}
	if remaining, err := database.GetMission(ctx, accountID, "__growth"); err != nil || remaining != nil {
		t.Fatalf("handoff retained commissioning: %+v, %v", remaining, err)
	}
	advanced := *first
	advanced.Phase = "sailing"
	advanced.Destination = "porto"
	advanced.Data = json.RawMessage(`{"growth_lane":{"admission_id":"growth-admission","quantity_limit":7},"action":{"id":"unchanged-attempt","body":"eyJxdWFudGl0eSI6M30="},"spent_pence":4000}`)
	if err := database.PutMission(ctx, &advanced); err != nil {
		t.Fatal(err)
	}
	var before missionRow
	lookup := database.privateLookup("mission-ship", accountID, next.ShipID)
	if err := database.db.Where("account_id = ? AND ship_lookup = ?", accountID, lookup).Take(&before).Error; err != nil {
		t.Fatal(err)
	}
	retried, err := database.HandoffGrowthMission(ctx, accountID, control.Generation, &next)
	if err != nil || retried == nil || retried.Phase != advanced.Phase || retried.Destination != advanced.Destination ||
		!retried.UpdatedAt.Equal(advanced.UpdatedAt) || !bytes.Equal(retried.Data, advanced.Data) {
		t.Fatalf("handoff replay overwrote an advancing operating mission: %+v, %v", retried, err)
	}
	var after missionRow
	if err := database.db.Where("account_id = ? AND ship_lookup = ?", accountID, lookup).Take(&after).Error; err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(before.MetadataCipher, after.MetadataCipher) || !bytes.Equal(before.DataCipher, after.DataCipher) {
		t.Fatal("handoff replay regenerated persisted ciphertext")
	}
	if _, err := database.HandoffGrowthMission(ctx, accountID, "another-admission", &next); !errors.Is(err, ErrActionConflict) {
		t.Fatalf("unrelated admission adopted an existing voyage: %v", err)
	}
}

func TestGrowthHandoffRejectsOccupiedSlotAndUnexpectedControlProof(t *testing.T) {
	for _, mode := range []string{"occupied", "wrong-generation", "unexpected-action", "wrong-built-ship", "wrong-phase"} {
		t.Run(mode, func(t *testing.T) {
			database, _, _ := integrationStore(t)
			ctx := context.Background()
			const accountID = "private-handoff-conflict"
			saveTestAccount(t, database, accountID)
			control, next := privateHandoffMissions(accountID)
			switch mode {
			case "occupied":
				occupied := next
				occupied.Generation = "unrelated-voyage"
				if err := database.PutMission(ctx, &occupied); err != nil {
					t.Fatal(err)
				}
			case "wrong-generation":
				control.Generation = "another-control"
			case "unexpected-action":
				control.Data = json.RawMessage(`{"built_ship_id":"built-ship","growth_lane":{"admission_id":"growth-admission"},"action":{"id":"pending-build"}}`)
			case "wrong-built-ship":
				control.Data = json.RawMessage(`{"built_ship_id":"different-ship","growth_lane":{"admission_id":"growth-admission"}}`)
			case "wrong-phase":
				control.Phase = "growth_build"
			}
			if err := database.PutMission(ctx, &control); err != nil {
				t.Fatal(err)
			}
			if _, err := database.HandoffGrowthMission(ctx, accountID, "growth-admission", &next); !errors.Is(err, ErrActionConflict) {
				t.Fatalf("unproven commissioning replaced an operating slot: %v", err)
			}
			remaining, err := database.GetMission(ctx, accountID, "__growth")
			if err != nil || remaining == nil || remaining.Generation != control.Generation || !bytes.Equal(remaining.Data, control.Data) {
				t.Fatalf("rejected handoff lost commissioning proof/reserves: %+v, %v", remaining, err)
			}
		})
	}
}

func TestGrowthHandoffRollsBackBothSlotsAndSerializesDuplicateAdmission(t *testing.T) {
	database, _, _ := integrationStore(t)
	ctx := context.Background()
	const accountID = "private-handoff-atomic"
	saveTestAccount(t, database, accountID)
	control, next := privateHandoffMissions(accountID)
	if err := database.PutMission(ctx, &control); err != nil {
		t.Fatal(err)
	}
	if err := database.db.Exec(`CREATE FUNCTION fail_growth_handoff() RETURNS trigger LANGUAGE plpgsql AS $$
		BEGIN RAISE EXCEPTION 'forced handoff failure'; END; $$;
		CREATE TRIGGER fail_growth_handoff BEFORE DELETE ON missions FOR EACH ROW EXECUTE FUNCTION fail_growth_handoff()`).Error; err != nil {
		t.Fatal(err)
	}
	if _, err := database.HandoffGrowthMission(ctx, accountID, control.Generation, &next); err == nil {
		t.Fatal("forced handoff transaction committed")
	}
	if remaining, err := database.GetMission(ctx, accountID, "__growth"); err != nil || remaining == nil {
		t.Fatalf("failed handoff deleted control: %+v, %v", remaining, err)
	}
	if occupied, err := database.GetMission(ctx, accountID, next.ShipID); err != nil || occupied != nil {
		t.Fatalf("failed handoff retained a half-committed voyage: %+v, %v", occupied, err)
	}
	if err := database.db.Exec("DROP TRIGGER fail_growth_handoff ON missions; DROP FUNCTION fail_growth_handoff()").Error; err != nil {
		t.Fatal(err)
	}
	var workers sync.WaitGroup
	errorsByWorker := make(chan error, 2)
	for range 2 {
		workers.Go(func() {
			_, err := database.HandoffGrowthMission(ctx, accountID, control.Generation, &next)
			errorsByWorker <- err
		})
	}
	workers.Wait()
	close(errorsByWorker)
	for err := range errorsByWorker {
		if err != nil {
			t.Fatalf("duplicate same-admission handoff was not idempotent: %v", err)
		}
	}
	var count int64
	if err := database.db.Model(&missionRow{}).Where("account_id = ?", accountID).Count(&count).Error; err != nil || count != 1 {
		t.Fatalf("atomic handoff left duplicate/control missions: %d, %v", count, err)
	}
}
