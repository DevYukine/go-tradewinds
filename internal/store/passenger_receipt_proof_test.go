package store

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"testing"
	"time"
)

func TestPassengerReceiptUpgradeRequiresOwnedRetainedTerminalProof(t *testing.T) {
	database, _, _ := integrationStore(t)
	ctx := context.Background()
	const accountID = "passenger-proof-auth"
	saveTestAccount(t, database, accountID)
	saveTestAccount(t, database, "passenger-proof-foreign")
	now := time.Now().UTC().Truncate(time.Microsecond)
	receipt := pendingPassengerReceiptFixture(accountID, "proof-auth-unknown", now)
	if err := database.PutPendingPassengerReceipt(ctx, &receipt); err != nil {
		t.Fatal(err)
	}
	for index, fixture := range []struct {
		account string
		kind    string
		payload json.RawMessage
		missing bool
	}{
		{accountID, "passengers_paid", nil, true},
		{"passenger-proof-foreign", "passengers_paid", json.RawMessage(`{"ship_id":"passenger-ship","fare_paid_pence":1000}`), false},
		{accountID, "passengers_boarded", json.RawMessage(`{"ship_id":"passenger-ship","fare_paid_pence":1000}`), false},
		{accountID, "passengers_paid", json.RawMessage(`{"ship_id":"other-ship","fare_paid_pence":1000}`), false},
		{accountID, "passengers_paid", json.RawMessage(`{"ship_id":"passenger-ship"}`), false},
		{accountID, "passengers_paid", json.RawMessage(`{"ship_id":"passenger-ship","fare_paid_pence":-1}`), false},
		{accountID, "passengers_paid", json.RawMessage(`{"ship_id":"passenger-ship","fare_paid_pence":1000.5}`), false},
		{accountID, "passengers_lost", json.RawMessage(`{"ship_id":"passenger-ship"}`), false},
	} {
		eventID := fmt.Sprintf("rejected-proof-%d", index)
		if !fixture.missing {
			retainedPassengerEvent(t, database, fixture.account, eventID, fixture.kind, fixture.payload)
		}
		candidate := receipt.Settlement
		candidate.EventID, candidate.ProceedsPence, candidate.ProceedsUnknown, candidate.CostsKnown = eventID, 1000, false, true
		candidate.GameSeconds = int64(candidate.CompletedAt.Sub(candidate.StartedAt).Seconds() * float64(receipt.TimeScale))
		err := database.ResolvePassengerReceipt(ctx, &candidate, []string{eventID})
		if !errors.Is(err, ErrNotFound) && !errors.Is(err, ErrActionConflict) {
			t.Fatalf("unauthenticated proof %d was accepted: %v", index, err)
		}
	}
	oldPayload, err := json.Marshal(map[string]any{
		"ship_id": receipt.Settlement.ShipID, "fare_paid_pence": 1000,
		"timestamp": receipt.Settlement.StartedAt.Add(-time.Second),
	})
	if err != nil {
		t.Fatal(err)
	}
	retainedPassengerEvent(t, database, accountID, "before-boarding-proof", "passengers_paid", oldPayload)
	candidate := receipt.Settlement
	candidate.EventID, candidate.ProceedsPence, candidate.ProceedsUnknown, candidate.CostsKnown = "before-boarding-proof", 1000, false, true
	if err := database.ResolvePassengerReceipt(ctx, &candidate, []string{candidate.EventID}); !errors.Is(err, ErrActionConflict) {
		t.Fatalf("pre-boarding payment funded archived passage: %v", err)
	}
	rows, err := database.SettlementHistory(ctx, accountID, "", 10)
	if err != nil || len(rows) != 1 || !rows[0].ProceedsUnknown {
		t.Fatalf("rejected terminal proofs changed immutable unknown accounting: %+v %v", rows, err)
	}
	resolved := receiptTerminalFixture(t, database, receipt, []string{"valid-owned-proof"}, []string{"passengers_paid"}, []int64{1000})
	if err := database.ResolvePassengerReceipt(ctx, &resolved, []string{resolved.EventID}); err != nil {
		t.Fatalf("valid proof failed after rejected attempts: %v", err)
	}
}

func TestPassengerReceiptCannotReuseOrdinaryEventInEitherWriteOrder(t *testing.T) {
	for _, archivedFirst := range []bool{false, true} {
		t.Run(map[bool]string{false: "ordinary first", true: "archive first"}[archivedFirst], func(t *testing.T) {
			database, _, _ := integrationStore(t)
			ctx := context.Background()
			const accountID = "passenger-ordinary-proof"
			saveTestAccount(t, database, accountID)
			receipt := pendingPassengerReceiptFixture(accountID, "ordinary-proof-unknown", time.Now().UTC().Truncate(time.Microsecond))
			if err := database.PutPendingPassengerReceipt(ctx, &receipt); err != nil {
				t.Fatal(err)
			}
			resolved := receiptTerminalFixture(t, database, receipt, []string{"already-settled-event"}, []string{"passengers_paid"}, []int64{1000})
			ordinary := resolved
			ordinary.ID = "ordinary-original-settlement"
			wantLinks := int64(0)
			if archivedFirst {
				if err := database.ResolvePassengerReceipt(ctx, &resolved, []string{resolved.EventID}); err != nil {
					t.Fatal(err)
				}
				if err := database.PutSettlement(ctx, &ordinary); !errors.Is(err, ErrActionConflict) {
					t.Fatalf("archived fare receipt was counted again in ordinary completion: %v", err)
				}
				wantLinks = 1
			} else {
				if err := database.PutSettlement(ctx, &ordinary); err != nil {
					t.Fatal(err)
				}
				if err := database.ResolvePassengerReceipt(ctx, &resolved, []string{resolved.EventID}); !errors.Is(err, ErrActionConflict) {
					t.Fatalf("ordinary fare receipt was counted again in archived completion: %v", err)
				}
			}
			var links int64
			if err := database.db.Model(&passengerReceiptEventRow{}).Count(&links).Error; err != nil || links != wantLinks {
				t.Fatalf("rejected duplicate attribution changed proof links: %d want=%d %v", links, wantLinks, err)
			}
			history, err := database.SettlementHistory(ctx, accountID, "", 10)
			if err != nil {
				t.Fatal(err)
			}
			realized := 0
			for _, row := range history {
				if row.EventID == resolved.EventID {
					realized++
				}
			}
			if realized != 1 {
				t.Fatalf("terminal proof was not assigned exactly once: %+v", history)
			}
		})
	}
}

func TestPassengerReceiptUntaggedProofRequiresOriginalSourceWindow(t *testing.T) {
	database, _, _ := integrationStore(t)
	ctx := context.Background()
	const accountID = "passenger-proof-source-window"
	saveTestAccount(t, database, accountID)
	now := time.Now().UTC().Truncate(time.Microsecond)
	for index, name := range []string{"missing source", "boarding instant", "after completion", "within window"} {
		t.Run(name, func(t *testing.T) {
			receipt := pendingPassengerReceiptFixture(accountID, fmt.Sprintf("source-window-%d", index), now)
			if err := database.PutPendingPassengerReceipt(ctx, &receipt); err != nil {
				t.Fatal(err)
			}
			at := receipt.Settlement.CompletedAt
			switch name {
			case "boarding instant":
				at = receipt.Settlement.StartedAt
			case "after completion":
				at = receipt.Settlement.CompletedAt.Add(time.Second)
			}
			payload := map[string]any{"ship_id": receipt.Settlement.ShipID, "fare_paid_pence": int64(1000)}
			if name != "missing source" {
				payload["timestamp"] = at
			}
			eventID := fmt.Sprintf("source-window-proof-%d", index)
			raw, err := json.Marshal(payload)
			if err != nil {
				t.Fatal(err)
			}
			retainedPassengerEvent(t, database, accountID, eventID, "passengers_paid", raw)
			resolved := receipt.Settlement
			resolved.EventID, resolved.ProceedsUnknown, resolved.CostsKnown = eventID, false, receipt.CostsKnown
			resolved.ProceedsPence, resolved.CompletedAt = 1000, at
			resolved.GameSeconds = max(int64(0), int64(at.Sub(resolved.StartedAt).Seconds()*float64(receipt.TimeScale)))
			err = database.ResolvePassengerReceipt(ctx, &resolved, []string{eventID})
			if name == "within window" {
				if err != nil {
					t.Fatalf("source-proven terminal receipt was rejected: %v", err)
				}
			} else if !errors.Is(err, ErrActionConflict) {
				t.Fatalf("ship-only proof without a unique original source window was accepted: %v", err)
			}
		})
	}
}
