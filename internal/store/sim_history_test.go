package store

import (
	"context"
	"encoding/json"
	"errors"
	"testing"
	"time"

	"go.uber.org/zap"
)

func TestSimulatorHistoryReadersDecryptScopeAndHalfOpenWindows(t *testing.T) {
	db, dsn, key := integrationStore(t)
	ctx := context.Background()
	saveTestAccount(t, db, "history-one")
	saveTestAccount(t, db, "history-two")
	from := time.Date(2026, 10, 20, 0, 0, 0, 0, time.UTC)
	body := json.RawMessage(`{"commodity_id":"private-cloth","quantity":7}`)
	response := json.RawMessage(`{"commodity_id":"private-cloth","filled_quantity":7,"total_gross_pence":630,"customs_duty_pence":16}`)
	for _, fixture := range []struct {
		id      string
		account string
		at      time.Time
	}{
		{"before", "history-one", from.Add(-time.Microsecond)},
		{"at-start-z", "history-one", from},
		{"at-start-a", "history-one", from},
		{"inside", "history-one", from.Add(time.Minute)},
		{"at-end", "history-one", from.Add(2 * time.Minute)},
		{"other", "history-two", from},
	} {
		bodyCipher, err := db.encrypt(body, actionAAD(fixture.account, fixture.id, "body"))
		if err != nil {
			t.Fatal(err)
		}
		responseCipher, err := db.encrypt(response, actionAAD(fixture.account, fixture.id, "response"))
		if err != nil {
			t.Fatal(err)
		}
		action := actionRow{ID: fixture.id, AccountID: fixture.account, CompanyID: "company-" + fixture.account, Method: "POST", Path: "/v1/market/place-order", BodyCipher: bodyCipher, ResponseCipher: responseCipher, State: ActionComplete, Status: 200, CreatedAt: fixture.at, UpdatedAt: fixture.at}
		if err := db.db.Create(&action).Error; err != nil {
			t.Fatal(err)
		}
		row := Settlement{ID: fixture.id, AccountID: fixture.account, MissionID: fixture.id, ActionID: fixture.id, ShipID: "private-ship", Kind: "spot", Lane: "private-lane", Commodity: "private-cloth", CostsKnown: true, QuantityTons: 7, AcquisitionPence: 630, ProceedsPence: 900, FeesPence: 16, StartedAt: from.Add(-time.Hour), CompletedAt: fixture.at}
		if err := db.PutSettlement(ctx, &row); err != nil {
			t.Fatal(err)
		}
	}
	to := from.Add(2 * time.Minute)
	actions, err := db.ActionsBetween(ctx, "history-one", from, to)
	if err != nil {
		t.Fatal(err)
	}
	settlements, err := db.SettlementsBetween(ctx, "history-one", from, to)
	if err != nil {
		t.Fatal(err)
	}
	want := []string{"at-start-a", "at-start-z", "inside"}
	if len(actions) != len(want) || len(settlements) != len(want) {
		t.Fatalf("window/account boundary mismatch: actions=%d settlements=%d", len(actions), len(settlements))
	}
	for i, id := range want {
		if actions[i].ID != id || string(actions[i].Body) != string(body) || string(actions[i].Response) != string(response) {
			t.Fatalf("decrypted immutable action %d does not match", i)
		}
		if settlements[i].ID != id || settlements[i].ShipID != "private-ship" || settlements[i].Lane != "private-lane" || settlements[i].AcquisitionPence != 630 || settlements[i].FeesPence != 16 {
			t.Fatalf("decrypted exact settlement %d does not match: %+v", i, settlements[i])
		}
	}
	if _, err := db.ActionsBetween(ctx, "history-one", to, from); !errors.Is(err, ErrInvalidInput) {
		t.Fatalf("invalid action window: %v", err)
	}
	if _, err := db.SettlementsBetween(ctx, "history-one", to, from); !errors.Is(err, ErrInvalidInput) {
		t.Fatalf("invalid settlement window: %v", err)
	}
	readOnly, err := OpenReadOnly(ctx, dsn, key, zap.NewNop())
	if err != nil {
		t.Fatal(err)
	}
	defer readOnly.Close()
	if rows, err := readOnly.ActionsBetween(ctx, "history-one", from, to); err != nil || len(rows) != 3 {
		t.Fatalf("read-only history query: rows=%d err=%v", len(rows), err)
	}
	if err := readOnly.SaveAccount(ctx, Account{ID: "forbidden"}, Credentials{Handle: "forbidden"}); err == nil {
		t.Fatal("read-only PostgreSQL connection permitted mutation")
	}
}
