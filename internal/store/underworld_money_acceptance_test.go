package store

import (
	"context"
	"encoding/json"
	"fmt"
	"reflect"
	"strings"
	"testing"
	"time"

	"go.uber.org/zap"
)

func TestUnderworldMoneyCredentialEncryptedRoundTripAndRedaction(t *testing.T) {
	db, dsn, key := integrationStore(t)
	ctx := context.Background()
	const account = "underworld-owner"
	const mainKey = "main-company-key-byte-identical"
	// A non-prefixed value proves value-based redaction was extended, rather
	// than accidentally passing because the generic tw_* regex recognized it.
	const observerKey = "separate-observer-opaque-value-92731"
	var credentials Credentials
	if err := json.Unmarshal([]byte(fmt.Sprintf(`{"handle":"Yukine","passphrase":"stored-passphrase","api_key":%q,"underworld_api_key":%q}`, mainKey, observerKey)), &credentials); err != nil {
		t.Fatal(err)
	}
	if err := db.SaveAccount(ctx, Account{ID: account, Label: "Yukine", CompanyID: "our-company", Enabled: true}, credentials); err != nil {
		t.Fatal(err)
	}
	reopened, err := Open(ctx, dsn, key, zap.NewNop())
	if err != nil {
		t.Fatal(err)
	}
	defer reopened.Close()
	loaded, err := reopened.Credentials(ctx, account)
	if err != nil {
		t.Fatal(err)
	}
	encoded, err := json.Marshal(loaded)
	if err != nil {
		t.Fatal(err)
	}
	var fields map[string]string
	if err := json.Unmarshal(encoded, &fields); err != nil {
		t.Fatal(err)
	}
	if fields["underworld_api_key"] != observerKey || loaded.APIKey != mainKey {
		t.Error("separate observer credential was lost or replaced the main API key")
	}
	if err := reopened.RecordRequest(ctx, account, "GET", "/v1/underworld/wants?port_id=amsterdam", 403, time.Millisecond,
		[]byte(fmt.Sprintf(`{"detail":%q}`, observerKey)), []byte(fmt.Sprintf(`{"detail":%q}`, observerKey)), "upstream repeated "+observerKey); err != nil {
		t.Fatal(err)
	}
	if err := reopened.RecordDecision(ctx, account, "", "underworld", "observer rejected "+observerKey, map[string]any{"detail": observerKey}); err != nil {
		t.Fatal(err)
	}
	if err := reopened.RecordEvent(ctx, account, "observer-secret", "notice", json.RawMessage(fmt.Sprintf(`{"detail":%q}`, observerKey))); err != nil {
		t.Fatal(err)
	}
	if err := reopened.SaveSnapshot(ctx, account, map[string]json.RawMessage{"underworld": json.RawMessage(fmt.Sprintf(`{"detail":%q}`, observerKey))}); err != nil {
		t.Fatal(err)
	}
	for _, table := range []string{"accounts", "requests", "decisions", "events", "snapshots"} {
		var rows []string
		if err := reopened.db.Raw("SELECT to_jsonb(t)::text FROM " + table + " t").Scan(&rows).Error; err != nil {
			t.Fatal(err)
		}
		for _, row := range rows {
			if strings.Contains(row, observerKey) {
				t.Errorf("observer key leaked into %s", table)
			}
		}
	}
	for _, kind := range []string{"requests", "decisions", "events"} {
		history, err := reopened.History(ctx, account, kind, 20)
		if err != nil {
			t.Fatal(err)
		}
		raw, err := json.Marshal(history)
		if err != nil {
			t.Fatal(err)
		}
		if strings.Contains(string(raw), observerKey) {
			t.Errorf("observer key leaked through %s read API", kind)
		}
	}
	clean := reopened.ScrubOutcomeTexts(ctx, account, []string{"rejected " + observerKey})
	if len(clean) != 1 || strings.Contains(clean[0], observerKey) {
		t.Fatal("observer key leaked through outcome log redaction")
	}
	// Non-JSON action responses are stored base64-wrapped; the excerpt must be
	// the decoded, scrubbed text. Malformed JSON could hide an escaped key, so
	// it is omitted.
	wrapped, err := json.Marshal(map[string][]byte{"raw_body": []byte("echo: " + observerKey)})
	if err != nil {
		t.Fatal(err)
	}
	escaped := `{"echo":"\u` + fmt.Sprintf("%04x", observerKey[0]) + observerKey[1:]
	malformed, err := json.Marshal(map[string][]byte{"raw_body": []byte(escaped)})
	if err != nil {
		t.Fatal(err)
	}
	for response, want := range map[string]string{
		string(wrapped):                         `"echo: [REDACTED]"`,
		string(malformed):                       `{"omitted":"invalid_json_cannot_be_safely_redacted","original_bytes":` + fmt.Sprint(len(escaped)) + `,"unparsed":true}`,
		fmt.Sprintf(`{"echo":%q}`, observerKey): `{"echo":"[REDACTED]"}`,
	} {
		if excerpt := reopened.ScrubOutcomeResponse(ctx, account, json.RawMessage(response)); excerpt != want {
			t.Fatalf("response excerpt %q, want %q", excerpt, want)
		}
	}
}

func underworldMoneyRecord(t *testing.T, db *Store, raw json.RawMessage) error {
	t.Helper()
	method := reflect.ValueOf(db).MethodByName("RecordUnderworldObservation")
	if !method.IsValid() {
		t.Fatal("missing permanent RecordUnderworldObservation store API")
	}
	typ := method.Type()
	if typ.NumIn() != 2 || typ.In(1).Kind() != reflect.Pointer || typ.NumOut() != 1 || !typ.Out(0).Implements(reflect.TypeFor[error]()) {
		t.Fatal("RecordUnderworldObservation must accept (context.Context, *record) and return error")
	}
	value := reflect.New(typ.In(1).Elem())
	if err := json.Unmarshal(raw, value.Interface()); err != nil {
		t.Fatal(err)
	}
	result := method.Call([]reflect.Value{reflect.ValueOf(context.Background()), value})[0]
	if result.IsNil() {
		return nil
	}
	return result.Interface().(error)
}

func TestUnderworldMoneyObservationsPermanentAndAccountIsolated(t *testing.T) {
	db, dsn, key := integrationStore(t)
	ctx := context.Background()
	for _, account := range []string{"first", "second"} {
		saveTestAccount(t, db, account)
		for _, kind := range []string{"wants", "book"} {
			payload := json.RawMessage(`{"commodity_id":"untaxed_tea","house_ask":{"price_pence":31000,"quantity":2},"bids":[],"asks":[]}`)
			if kind == "wants" {
				payload = json.RawMessage(`{"port_id":"flushing","is_cove":true,"wants":[],"board":[{"port_id":"amsterdam","commodity_id":"untaxed_tea","multiple_bps":22500,"observed_at":"2026-10-28T15:00:00Z","age_game_hours":1}]}`)
			}
			raw, _ := json.Marshal(map[string]any{"account_id": account, "ship_id": "same-ship", "visit_id": "same-arrival", "port_id": "flushing",
				"kind": kind, "commodity_id": map[string]string{"wants": "", "book": "untaxed_tea"}[kind], "observed_at": time.Now().Add(-30 * 24 * time.Hour),
				"payload": payload})
			if err := underworldMoneyRecord(t, db, raw); err != nil {
				t.Fatal(err)
			}
			if err := underworldMoneyRecord(t, db, raw); err != nil {
				t.Fatalf("same arrival observation replay failed: %v", err)
			}
		}
	}
	if err := db.Prune(ctx); err != nil {
		t.Fatal(err)
	}
	reopened, err := Open(ctx, dsn, key, zap.NewNop())
	if err != nil {
		t.Fatal(err)
	}
	defer reopened.Close()
	for _, account := range []string{"first", "second"} {
		var rows []struct {
			Kind        string
			CommodityID string
			Payload     json.RawMessage
		}
		if err := reopened.db.Table("underworld_observations").Where("account_id = ?", account).Order("kind").Find(&rows).Error; err != nil {
			t.Fatal(err)
		}
		if len(rows) != 2 {
			t.Fatalf("permanent observations lost, duplicated or crossed account: %s %+v", account, rows)
		}
		if rows[0].Kind != "book" || rows[0].CommodityID != "untaxed_tea" || rows[1].Kind != "wants" || !strings.Contains(string(rows[0].Payload), "31000") {
			t.Fatalf("observation content changed on prune/restart: %+v", rows)
		}
	}
}
