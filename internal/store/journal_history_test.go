package store

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"gorm.io/gorm"
)

func TestCompanyJournalFiltersBeforeLimitAndPreservesUnknownCosts(t *testing.T) {
	db, _, _ := integrationStore(t)
	for _, id := range []string{"journal-a", "journal-b"} {
		saveTestAccount(t, db, id)
	}
	ctx, release, err := db.AcquireAccountContext(context.Background(), "journal-a")
	if err != nil {
		t.Fatal(err)
	}
	defer release()
	payload := json.RawMessage(`{"company_id":"company-journal-a","payload":{"ship_id":"lapras","dest_port_id":"portsmouth","api_key":"tw_live_private_journal_secret"}}`)
	if err := db.RecordEvent(ctx, "journal-a", "own-departure", "ship_departed", payload); err != nil {
		t.Fatal(err)
	}
	// Public deliveries and other companies are not our activity, even when
	// they occupy all of the newest rows received on this account's stream.
	for i := range 6 {
		company := "null"
		if i%2 == 0 {
			company = `"company-journal-b"`
		}
		body := fmt.Sprintf(`{"company_id":%s,"payload":{"ship_id":"other-ship","delivered_quantity":24}}`, company)
		if err := db.RecordEvent(ctx, "journal-a", fmt.Sprintf("public-%d", i), "charter_delivered", json.RawMessage(body)); err != nil {
			t.Fatal(err)
		}
	}
	intent := &Action{ID: "journal-outcome", AccountID: "journal-a", Method: "POST", Path: "/v1/test", Body: json.RawMessage(`{}`)}
	if _, err := db.BeginAction(ctx, intent); err != nil {
		t.Fatal(err)
	}
	if err := db.ReconcileAction(ctx, intent.ID, json.RawMessage(`{"observed":true}`)); err != nil {
		t.Fatal(err)
	}
	settlement := &Settlement{
		ID: "unknown-costs", AccountID: "journal-a", ShipID: "lapras", MissionID: "unknown-costs", ActionID: intent.ID,
		Kind: "recovery", Lane: "le_havre|portsmouth|provisions|recovery", Origin: "le_havre", Destination: "portsmouth", Commodity: "provisions",
		QuantityTons: 24, CostsKnown: false, ProceedsPence: 50000, CompletedAt: time.Now().UTC(),
	}
	if err := db.PutSettlement(ctx, settlement); err != nil {
		t.Fatal(err)
	}
	result, err := db.History(ctx, "journal-a", "journal", 2)
	if err != nil {
		t.Fatal(err)
	}
	encoded, err := json.Marshal(result)
	if err != nil {
		t.Fatal(err)
	}
	var rows []struct {
		AccountID  string `json:"account_id"`
		Kind       string `json:"kind"`
		Amount     *int64 `json:"amount_pence"`
		CostsKnown *bool  `json:"costs_known"`
	}
	if err := json.Unmarshal(encoded, &rows); err != nil {
		t.Fatal(err)
	}
	if len(rows) != 2 || rows[0].Kind != "settlement" || rows[1].Kind != "ship_departed" {
		t.Fatalf("public activity displaced company journal rows: %s", encoded)
	}
	if rows[0].Amount != nil || rows[0].CostsKnown == nil || *rows[0].CostsKnown {
		t.Fatalf("unknown cost basis became numeric profit: %s", encoded)
	}
	for _, row := range rows {
		if row.AccountID != "journal-a" {
			t.Fatalf("journal crossed account boundary: %s", encoded)
		}
	}
	for _, forbidden := range []string{"tw_live_private", "api_key", "other-ship", "public-"} {
		if strings.Contains(string(encoded), forbidden) {
			t.Fatalf("journal exposed unrelated or private data %q: %s", forbidden, encoded)
		}
	}
	if _, err := db.History(ctx, "", "journal", 2); err == nil {
		t.Fatal("company journal accepted an unscoped account")
	}
}

func TestCompanyJournalProjectsRecordedCashAndPartialFills(t *testing.T) {
	db, _, _ := integrationStore(t)
	saveTestAccount(t, db, "journal-cash")
	ctx := context.Background()
	cases := []struct {
		name       string
		path       string
		body       string
		response   string
		amount     int64
		amountKind string
		quantity   int64
		hasAmount  bool
		hasFill    bool
	}{
		{
			name: "partial purchase includes customs", path: "/v1/market/place-order",
			body:     `{"ship_id":"lapras","port_id":"origin","commodity_id":"grain","side":"bid","quantity":19}`,
			response: `{"commodity_id":"grain","filled_quantity":7,"total_gross_pence":12347,"customs_duty_pence":251}`,
			amount:   12598, amountKind: "cash_out", quantity: 7, hasAmount: true, hasFill: true,
		},
		{
			name: "partial sale deducts customs", path: "/v1/market/place-order",
			body:     `{"ship_id":"lapras","port_id":"destination","commodity_id":"grain","side":"ask","quantity":19}`,
			response: `{"commodity_id":"grain","filled_quantity":5,"total_gross_pence":7654,"customs_duty_pence":163}`,
			amount:   7491, amountKind: "cash_in", quantity: 5, hasAmount: true, hasFill: true,
		},
		{
			name: "unfilled order is not requested quantity", path: "/v1/market/place-order",
			body:       `{"ship_id":"lapras","port_id":"origin","commodity_id":"grain","side":"bid","quantity":19}`,
			response:   `{"commodity_id":"grain","filled_quantity":0,"total_gross_pence":0,"customs_duty_pence":0}`,
			amountKind: "cash_out", hasAmount: true, hasFill: true,
		},
		{
			name: "partial charter reward", path: "/v1/contract/deliver-charter",
			body:     `{"ship_id":"lapras","contract_id":"charter","quantity":20}`,
			response: `{"contract_id":"charter","commodity_id":"grain","port_id":"destination","delivered_quantity":9,"reward_paid_pence":19003}`,
			amount:   19003, amountKind: "cash_in", quantity: 9, hasAmount: true, hasFill: true,
		},
		{
			name: "freight reward", path: "/v1/contract/deliver-freight",
			body:     `{"ship_id":"lapras","contract_id":"freight"}`,
			response: `{"contract_id":"freight","commodity_id":"grain","dest_port_id":"destination","delivered_quantity":11,"reward_paid_pence":22007}`,
			amount:   22007, amountKind: "cash_in", quantity: 11, hasAmount: true, hasFill: true,
		},
		{
			name: "remittance charges only fee not transferred principal", path: "/v1/treasury/remit",
			body:     `{"from_region":"north","to_region":"south","gross_amount_pence":10003}`,
			response: `{"from_region":"north","to_region":"south","gross_amount_pence":10003,"fee_pence":103,"net_credited_pence":9900}`,
			amount:   103, amountKind: "charge", hasAmount: true,
		},
		{
			name: "ship capital cash out", path: "/v1/shipyard/build-ship",
			body:     `{"port_id":"origin","name":"Lapras","ship_class":"hoy"}`,
			response: `{"ship_id":"new-lapras","name":"Lapras","ship_class":"hoy","total_cost_pence":63007}`,
			amount:   63007, amountKind: "cash_out", hasAmount: true,
		},
		{
			name: "malformed response has no invented cash", path: "/v1/market/place-order",
			body: `{"commodity_id":"grain","side":"bid","quantity":19}`, response: `not JSON`,
		},
		{
			name: "missing customs is unknown not zero", path: "/v1/market/place-order",
			body:     `{"commodity_id":"grain","side":"bid","quantity":19}`,
			response: `{"commodity_id":"grain","filled_quantity":7,"total_gross_pence":12347}`,
		},
		{
			name: "wrong commodity cannot describe fill", path: "/v1/market/place-order",
			body:     `{"commodity_id":"grain","side":"bid","quantity":19}`,
			response: `{"commodity_id":"iron","filled_quantity":7,"total_gross_pence":12347,"customs_duty_pence":251}`,
		},
		{
			name: "overfill cannot describe cash", path: "/v1/market/place-order",
			body:     `{"commodity_id":"grain","side":"bid","quantity":3}`,
			response: `{"commodity_id":"grain","filled_quantity":7,"total_gross_pence":12347,"customs_duty_pence":251}`,
		},
		{
			name: "inconsistent transfer has no known fee", path: "/v1/treasury/remit",
			body:     `{"from_region":"north","to_region":"south","gross_amount_pence":10003}`,
			response: `{"from_region":"north","to_region":"south","gross_amount_pence":10003,"fee_pence":103,"net_credited_pence":10003}`,
		},
		{
			name: "uninterpretable commitment remains unknown", path: "/v1/market/place-order",
			body:     `{"commodity_id":"grain","side":"bid","quantity":19}`,
			response: `{"committed_uninterpreted":true}`,
		},
	}
	for i, test := range cases {
		t.Run(test.name, func(t *testing.T) {
			action := &Action{
				ID: fmt.Sprintf("cash-%02d", i), AccountID: "journal-cash", Method: "POST",
				Path: test.path, Body: json.RawMessage(test.body),
			}
			if _, err := db.BeginAction(ctx, action); err != nil {
				t.Fatal(err)
			}
			if err := db.CompleteAction(ctx, action.ID, 200, []byte(test.response), nil); err != nil {
				t.Fatal(err)
			}
			rows := companyJournalRows(t, db, action.AccountID, 100)
			var row *JournalRow
			for j := range rows {
				if rows[j].ID == "action:"+action.ID {
					row = &rows[j]
				}
			}
			if row == nil {
				t.Fatalf("completed action absent from journal: %+v", rows)
			}
			if (row.AmountPence != nil) != test.hasAmount || row.AmountKind != test.amountKind {
				t.Fatalf("cash certainty/kind differs from recorded result: %+v", row)
			}
			if test.hasAmount && *row.AmountPence != test.amount {
				t.Fatalf("cash amount = %d, want %d: %+v", *row.AmountPence, test.amount, row)
			}
			if (row.QuantityTons != nil) != test.hasFill || test.hasFill && *row.QuantityTons != test.quantity {
				t.Fatalf("journal used requested rather than validated filled quantity: %+v", row)
			}
			if !test.hasAmount && !strings.Contains(row.Summary, "outcome") {
				t.Fatalf("unknown outcome presented as a successful economic fact: %+v", row)
			}
		})
	}
}

func TestCompanyJournalKnownNetSubtractsEveryCostAndUnknownNetStaysAbsent(t *testing.T) {
	db, _, _ := integrationStore(t)
	saveTestAccount(t, db, "journal-net")
	ctx := context.Background()
	for i, known := range []bool{true, false} {
		action := &Action{ID: fmt.Sprintf("net-outcome-%d", i), AccountID: "journal-net", Method: "POST", Path: "/v1/test", Body: json.RawMessage(`{}`)}
		if _, err := db.BeginAction(ctx, action); err != nil {
			t.Fatal(err)
		}
		if err := db.ReconcileAction(ctx, action.ID, json.RawMessage(`{"observed":true}`)); err != nil {
			t.Fatal(err)
		}
		settlement := &Settlement{
			ID: fmt.Sprintf("net-%d", i), AccountID: action.AccountID, ShipID: "lapras",
			MissionID: fmt.Sprintf("net-%d", i), ActionID: action.ID, Kind: "trade",
			Origin: "origin", Destination: "destination", Commodity: "grain", QuantityTons: 7,
			CostsKnown: known, AcquisitionPence: 12347, ProceedsPence: 19003,
			FeesPence: 251, ProvisionsPence: 1301, ExpensesPence: 611,
			CompletedAt: time.Date(2026, 10, 23, 1, i, 0, 0, time.UTC),
		}
		if err := db.PutSettlement(ctx, settlement); err != nil {
			t.Fatal(err)
		}
	}
	rows := companyJournalRows(t, db, "journal-net", 10)
	if len(rows) != 2 || rows[0].ID != "settlement:net-1" || rows[1].ID != "settlement:net-0" {
		t.Fatalf("settlements missing or not newest first: %+v", rows)
	}
	if rows[0].CostsKnown == nil || *rows[0].CostsKnown || rows[0].AmountPence != nil || rows[0].AmountKind != "" {
		t.Fatalf("unknown basis became zero cost or gross profit: %+v", rows[0])
	}
	// 19,003 receipts less 12,347 cargo, 251 customs, 1,301 stores and
	// 611 voyage expenses is 4,493 pence, not cash-in or gross margin.
	if rows[1].CostsKnown == nil || !*rows[1].CostsKnown || rows[1].AmountPence == nil ||
		*rows[1].AmountPence != 4493 || rows[1].AmountKind != "realized_net" {
		t.Fatalf("known settlement omitted or double-counted costs: %+v", rows[1])
	}
}

func TestCompanyJournalBoundsMaterializedCiphertextAndPreservesNewestOutcomes(t *testing.T) {
	db, _, _ := integrationStore(t)
	saveTestAccount(t, db, "journal-bounded")
	ctx := context.Background()
	const actionCount = 12
	const payloadBytes = 64 << 10
	payload := strings.Repeat("x", payloadBytes)
	var newest *Action
	var newestResponse []byte
	for i := range actionCount {
		action := &Action{
			ID: fmt.Sprintf("bounded-%02d", i), AccountID: "journal-bounded", Method: "POST",
			Path: "/v1/market/place-order",
			Body: json.RawMessage(`{"ship_id":"lapras","port_id":"origin","commodity_id":"grain","side":"bid","quantity":19}`),
		}
		response := []byte(fmt.Sprintf(`{"commodity_id":"grain","filled_quantity":%d,"total_gross_pence":%d,"customs_duty_pence":3,"diagnostic":%q}`, i+1, 100*(i+1), payload))
		if _, err := db.BeginAction(ctx, action); err != nil {
			t.Fatal(err)
		}
		if err := db.CompleteAction(ctx, action.ID, 200, response, nil); err != nil {
			t.Fatal(err)
		}
		newest, newestResponse = action, response
	}
	// Observe fetched values, not SQL spelling. Small payloads make the old
	// bulk-retention behavior visible without allocating 500 near-limit blobs.
	var maxMaterialized int
	const callback = "test:journal_ciphertext_bound"
	if err := db.db.Callback().Query().After("gorm:query").Register(callback, func(tx *gorm.DB) {
		var materialized int
		switch destination := tx.Statement.Dest.(type) {
		case *[]actionRow:
			for _, action := range *destination {
				materialized += len(action.BodyCipher) + len(action.ResponseCipher)
			}
		case *actionRow:
			materialized = len(destination.BodyCipher) + len(destination.ResponseCipher)
		}
		maxMaterialized = max(maxMaterialized, materialized)
	}); err != nil {
		t.Fatal(err)
	}
	rows := companyJournalRows(t, db, "journal-bounded", actionCount)
	if err := db.db.Callback().Query().Remove(callback); err != nil {
		t.Fatal(err)
	}
	if maxMaterialized > 2*payloadBytes {
		t.Fatalf("journal retained bulk encrypted payloads in one query: %d bytes for %d bounded outcomes", maxMaterialized, actionCount)
	}
	if len(rows) != actionCount {
		t.Fatalf("bounded retrieval lost outcomes: got %d, want %d", len(rows), actionCount)
	}
	for i, row := range rows {
		n := actionCount - i
		if row.ID != fmt.Sprintf("action:bounded-%02d", n-1) || row.QuantityTons == nil ||
			*row.QuantityTons != int64(n) || row.AmountPence == nil || *row.AmountPence != int64(100*n+3) {
			t.Fatalf("incremental projection lost newest ordering or recorded result at %d: %+v", i, row)
		}
		if len(row.Summary) > 512 || strings.Contains(row.Summary, payload[:1024]) {
			t.Fatalf("journal projected unbounded diagnostic body: summary length %d", len(row.Summary))
		}
	}
	replayed, err := db.BeginAction(ctx, newest)
	if err != nil || !bytes.Equal(replayed.Response, newestResponse) {
		t.Fatalf("bounded journal projection changed the durable raw response: %v", err)
	}
}

func TestCompanyJournalUsesTransactionForCredentialRedaction(t *testing.T) {
	db, _, _ := integrationStore(t)
	const accountID = "journal-single-connection"
	credentials := Credentials{
		Handle:     "journal-player",
		Passphrase: "opaque-journal-passphrase",
		APIKey:     "opaque-journal-api-value",
	}
	ctx := context.Background()
	if err := db.SaveAccount(ctx, Account{
		ID: accountID, Label: "Journal", CompanyID: "journal-company", Enabled: true,
	}, credentials); err != nil {
		t.Fatal(err)
	}
	body, err := json.Marshal(map[string]any{
		"ship_id": credentials.Passphrase, "port_id": credentials.APIKey,
		"commodity_id": credentials.Passphrase, "side": "bid", "quantity": 1,
	})
	if err != nil {
		t.Fatal(err)
	}
	action := &Action{
		ID: "single-connection-outcome", AccountID: accountID, Method: "POST",
		Path: "/v1/market/place-order", Body: body,
	}
	response, err := json.Marshal(map[string]any{
		"commodity_id": credentials.Passphrase, "filled_quantity": 1,
		"total_gross_pence": 100, "customs_duty_pence": 3,
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.BeginAction(ctx, action); err != nil {
		t.Fatal(err)
	}
	if err := db.CompleteAction(ctx, action.ID, 200, response, nil); err != nil {
		t.Fatal(err)
	}

	// A transaction can hold the only connection, but must not need another
	// connection to load the credentials used to redact its included outcome.
	db.sqlDB.SetMaxOpenConns(1)
	waitCount := db.sqlDB.Stats().WaitCount
	readCtx, cancel := context.WithTimeout(ctx, 3*time.Second)
	defer cancel()
	rows, err := db.journalHistory(readCtx, accountID, 1)
	if err != nil {
		t.Fatalf("journal with one database connection: %v", err)
	}
	if db.sqlDB.Stats().WaitCount != waitCount {
		t.Fatal("journal waited for a second pooled database connection")
	}
	if len(rows) != 1 || rows[0].ID != "action:"+action.ID ||
		rows[0].AmountPence == nil || *rows[0].AmountPence != 103 ||
		rows[0].QuantityTons == nil || *rows[0].QuantityTons != 1 {
		t.Fatalf("bounded outcome was not projected: %+v", rows)
	}
	if rows[0].ShipID != redacted || rows[0].Origin != redacted ||
		rows[0].Commodity != redacted || !strings.Contains(rows[0].Summary, redacted) {
		t.Fatalf("journal did not redact current account credentials: %+v", rows[0])
	}
	encoded, err := json.Marshal(rows)
	if err != nil {
		t.Fatal(err)
	}
	for _, secret := range []string{credentials.Passphrase, credentials.APIKey} {
		if bytes.Contains(encoded, []byte(secret)) {
			t.Fatal("journal exposed an account credential")
		}
	}
	actual, err := db.Credentials(ctx, accountID)
	if err != nil || actual != credentials {
		t.Fatalf("credential retrieval changed: %+v, %v", actual, err)
	}
	replayed, err := db.BeginAction(ctx, action)
	if err != nil || !bytes.Equal(replayed.Response, response) {
		t.Fatalf("journal changed the durable outcome: %v", err)
	}
}

func TestCompanyJournalCredentialLoadingRemainsLazyAndFailClosed(t *testing.T) {
	for _, test := range []struct {
		name       string
		oversized  bool
		newerEvent bool
		corrupt    bool
		wantError  bool
	}{
		{name: "missing credentials fail closed", wantError: true},
		{name: "corrupt credentials fail closed", corrupt: true, wantError: true},
		{name: "oversized outcome needs no credentials", oversized: true},
		{name: "excluded outcome needs no credentials", newerEvent: true},
	} {
		t.Run(test.name, func(t *testing.T) {
			db, _, _ := integrationStore(t)
			const accountID = "journal-lazy-credentials"
			saveTestAccount(t, db, accountID)
			ctx := context.Background()
			action := &Action{
				ID: "lazy-outcome", AccountID: accountID, Method: "POST",
				Path: "/v1/market/place-order",
				Body: json.RawMessage(`{"ship_id":"lapras","port_id":"origin","commodity_id":"grain","side":"bid","quantity":1}`),
			}
			response := []byte(`{"commodity_id":"grain","filled_quantity":1,"total_gross_pence":100,"customs_duty_pence":3}`)
			if test.oversized {
				response = []byte(fmt.Sprintf(`{"diagnostic":%q}`, strings.Repeat("x", journalOutcomeCiphertextLimit)))
			}
			if _, err := db.BeginAction(ctx, action); err != nil {
				t.Fatal(err)
			}
			if err := db.CompleteAction(ctx, action.ID, 200, response, nil); err != nil {
				t.Fatal(err)
			}
			if test.newerEvent {
				if err := db.RecordEvent(ctx, accountID, "newer-event", "ship_departed",
					json.RawMessage(`{"company_id":"company-journal-lazy-credentials","payload":{"ship_id":"lapras"}}`)); err != nil {
					t.Fatal(err)
				}
				if err := db.db.Model(&Event{}).Where("account_id = ?", accountID).
					Update("created_at", time.Now().UTC().Add(time.Minute)).Error; err != nil {
					t.Fatal(err)
				}
			}
			if test.corrupt {
				if err := db.db.Model(&accountSecret{}).Where("account_id = ?", accountID).
					Update("ciphertext", []byte("truncated")).Error; err != nil {
					t.Fatal(err)
				}
			} else if err := db.db.Where("account_id = ?", accountID).Delete(&accountSecret{}).Error; err != nil {
				t.Fatal(err)
			}
			db.sqlDB.SetMaxOpenConns(1)
			readCtx, cancel := context.WithTimeout(ctx, 3*time.Second)
			defer cancel()
			rows, err := db.journalHistory(readCtx, accountID, 1)
			if test.wantError {
				if err == nil || len(rows) != 0 {
					t.Fatalf("credential failure exposed an outcome: %+v, %v", rows, err)
				}
				if test.corrupt {
					if !strings.Contains(err.Error(), "encrypted value is truncated") {
						t.Fatalf("credential decryption error was not preserved: %v", err)
					}
				} else if !errors.Is(err, ErrNotFound) {
					t.Fatalf("missing credential error was not preserved: %v", err)
				}
				return
			}
			if err != nil || len(rows) != 1 {
				t.Fatalf("journal unnecessarily required credentials: %+v, %v", rows, err)
			}
			if test.newerEvent {
				if rows[0].ID != "event:newer-event" {
					t.Fatalf("journal did not exclude the older outcome: %+v", rows)
				}
			} else if rows[0].ID != "action:"+action.ID || rows[0].AmountPence != nil ||
				!strings.Contains(rows[0].Summary, "outcome details unavailable") {
				t.Fatalf("oversized outcome did not retain honest metadata: %+v", rows)
			}
		})
	}
}

func companyJournalRows(t *testing.T, db *Store, accountID string, limit int) []JournalRow {
	t.Helper()
	result, err := db.History(context.Background(), accountID, "journal", limit)
	if err != nil {
		t.Fatal(err)
	}
	encoded, err := json.Marshal(result)
	if err != nil {
		t.Fatal(err)
	}
	var rows []JournalRow
	if err := json.Unmarshal(encoded, &rows); err != nil {
		t.Fatal(err)
	}
	return rows
}
