package store

import (
	"context"
	"encoding/json"
	"errors"
	"reflect"
	"testing"
	"time"

	"go.uber.org/zap"
)

// Reflection only keeps tests-first additions out of the package's compile
// dependency graph. The store contract is PutFuturesIntent(ctx, *record) error;
// record's JSON fields below are the durable reservation/identity contract.
func futuresMoneyPut(t *testing.T, db *Store, raw json.RawMessage) error {
	t.Helper()
	method := reflect.ValueOf(db).MethodByName("PutFuturesIntent")
	if !method.IsValid() {
		t.Fatal("missing durable PutFuturesIntent store admission")
	}
	typ := method.Type()
	if typ.NumIn() != 2 || typ.In(1).Kind() != reflect.Pointer || typ.NumOut() != 1 || !typ.Out(0).Implements(reflect.TypeFor[error]()) {
		t.Fatal("PutFuturesIntent must accept (context.Context, *record) and return error")
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

func futuresMoneyIntent(account, id, action, side string, exposure int64) json.RawMessage {
	now := time.Now().UTC()
	tons, cash := int64(1), int64(384)
	if side == "bid" {
		tons, cash = 0, 1536+(1536*268+9999)/10000+384
	}
	raw, _ := json.Marshal(map[string]any{
		"account_id": account, "intent_id": id, "action_id": action, "cohort_id": "owner-pilot",
		"port_id": "amsterdam", "commodity_id": "water_beer", "side": side, "lot_tons": 1,
		"strike_price_pence": 1536, "reserved_tons": tons, "reserved_cash_pence": cash,
		"margin_pence": 384, "exposure_pence": exposure, "status": "intent",
		"matures_at": now.Add(30 * time.Minute), "created_at": now,
	})
	return raw
}

func TestFuturesMoneyLedgerExposureCapIncludesOutstandingIntents(t *testing.T) {
	for _, extra := range []int64{20_000, 20_001} {
		t.Run(map[int64]string{20_000: "30000_pence_allowed", 20_001: "30001_pence_refused"}[extra], func(t *testing.T) {
			db, _, _ := integrationStore(t)
			saveTestAccount(t, db, "pilot")
			saveTestAccount(t, db, "other")
			if err := futuresMoneyPut(t, db, futuresMoneyIntent("pilot", "first", "first-action", "ask", 10_000)); err != nil {
				t.Fatal(err)
			}
			if err := futuresMoneyPut(t, db, futuresMoneyIntent("other", "other", "other-action", "ask", 30_000)); err != nil {
				t.Fatal(err)
			}
			err := futuresMoneyPut(t, db, futuresMoneyIntent("pilot", "second", "second-action", "bid", extra))
			if extra == 20_000 && err != nil {
				t.Fatalf("30,000p inclusive cap refused: %v", err)
			}
			if extra == 20_001 && err == nil {
				t.Fatal("30,001p combined exposure admitted; pending intents must count before send")
			}
			var count int64
			if err := db.db.Table("futures_intents").Where("account_id = ?", "pilot").Count(&count).Error; err != nil {
				t.Fatal(err)
			}
			want := int64(1)
			if extra == 20_000 {
				want = 2
			}
			if count != want {
				t.Fatalf("failed admission left a partial intent: count=%d want=%d", count, want)
			}
		})
	}
}

func TestFuturesMoneyLedgerImmutableReplayAndPermanentAccounting(t *testing.T) {
	db, dsn, key := integrationStore(t)
	ctx := context.Background()
	saveTestAccount(t, db, "pilot")
	intent := futuresMoneyIntent("pilot", "ask-one", "create-ask-one", "ask", 2200)
	if err := futuresMoneyPut(t, db, intent); err != nil {
		t.Fatal(err)
	}
	if err := futuresMoneyPut(t, db, intent); err != nil {
		t.Fatalf("same intent/key replay failed: %v", err)
	}
	var changed map[string]any
	if err := json.Unmarshal(intent, &changed); err != nil {
		t.Fatal(err)
	}
	changed["strike_price_pence"] = 1537
	changedRaw, _ := json.Marshal(changed)
	if err := futuresMoneyPut(t, db, changedRaw); !errors.Is(err, ErrActionConflict) {
		t.Fatalf("changed strike reused a reserved immutable intent: %v", err)
	}
	// A realized loss must retain its sign. It is not margin returned, a gain,
	// or a replacement listing when diagnostic history expires.
	if err := db.db.Exec("UPDATE futures_intents SET status = 'settled', futures_id = ?, realized_pnl_pence = ?, created_at = ? WHERE account_id = ? AND intent_id = ?",
		"11111111-1111-4111-8111-111111111111", -257, time.Now().Add(-30*24*time.Hour), "pilot", "ask-one").Error; err != nil {
		t.Fatal(err)
	}
	if err := db.Prune(ctx); err != nil {
		t.Fatal(err)
	}
	reopened, err := Open(ctx, dsn, key, zap.NewNop())
	if err != nil {
		t.Fatal(err)
	}
	defer reopened.Close()
	var row struct {
		ActionID         string
		Status           string
		RealizedPnLPence int64 `gorm:"column:realized_pnl_pence"`
		ExposurePence    int64
	}
	if err := reopened.db.Table("futures_intents").Where("account_id = ? AND intent_id = ?", "pilot", "ask-one").Take(&row).Error; err != nil {
		t.Fatalf("permanent pilot ledger lost on prune/restart: %v", err)
	}
	if row.ActionID != "create-ask-one" || row.Status != "settled" || row.RealizedPnLPence != -257 || row.ExposurePence != 2200 {
		t.Fatalf("durable pilot identity/cost/P&L changed: %+v", row)
	}
}
