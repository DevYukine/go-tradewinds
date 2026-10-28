package store

import (
	"bytes"
	"context"
	"crypto/cipher"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"strings"
	"testing"
	"testing/fstest"
	"time"

	"github.com/pressly/goose/v3"
	"go.uber.org/zap"
)

func pendingPassengerReceiptFixture(accountID, id string, completed time.Time) PassengerReceipt {
	settlement := passageSettlementFixture(accountID, id, "", 0, completed)
	settlement.CostsKnown, settlement.ProceedsUnknown = false, true
	return PassengerReceipt{
		Settlement: settlement, CostsKnown: true, TimeScale: 60,
		Context: json.RawMessage(`{ "mission" : {"generation":"immutable-generation"}, "expense_ids":["bill-a"] }`),
	}
}

func receiptTerminalFixture(t *testing.T, database *Store, receipt PassengerReceipt, ids []string, kinds []string, proceeds []int64) Settlement {
	t.Helper()
	resolved := receipt.Settlement
	resolved.EventID, resolved.ProceedsUnknown, resolved.CostsKnown = ids[0], false, receipt.CostsKnown
	resolved.ProceedsPence = 0
	for index, id := range ids {
		payload, err := json.Marshal(map[string]any{
			"timestamp": receipt.Settlement.CompletedAt,
			"payload":   map[string]any{"ship_id": receipt.Settlement.ShipID, "fare_paid_pence": proceeds[index]},
		})
		if err != nil {
			t.Fatal(err)
		}
		retainedPassengerEvent(t, database, receipt.Settlement.AccountID, id, kinds[index], payload)
		if kinds[index] == "passengers_paid" {
			resolved.ProceedsPence += proceeds[index]
		}
	}
	resolved.GameSeconds = int64(resolved.CompletedAt.Sub(resolved.StartedAt).Seconds() * float64(receipt.TimeScale))
	return resolved
}

func TestPassengerPendingReceiptEncryptedImmutableRestart(t *testing.T) {
	database, dsn, key := integrationStore(t)
	ctx := context.Background()
	const accountID = "passenger-pending-private"
	credentials := saveTestAccount(t, database, accountID)
	saveTestAccount(t, database, "passenger-pending-foreign")
	completed := time.Now().UTC().Add(-8 * 24 * time.Hour).Truncate(time.Microsecond)
	receipt := pendingPassengerReceiptFixture(accountID, "pending-private", completed)
	receipt.Context = json.RawMessage(fmt.Sprintf(`{ "mission" : %q, "expense_ids":["immutable-bill"] }`, credentials.APIKey+credentials.Passphrase))
	receipt.Settlement.Lane = credentials.APIKey + "|" + credentials.Passphrase
	if err := database.PutPendingPassengerReceipt(ctx, &receipt); err != nil {
		t.Fatal(err)
	}
	var raw string
	if err := database.db.Raw("SELECT to_jsonb(archive)::text FROM passenger_receipts AS archive WHERE id = ?", receipt.Settlement.ID).Scan(&raw).Error; err != nil {
		t.Fatal(err)
	}
	for _, secret := range []string{credentials.APIKey, credentials.Passphrase, "immutable-bill", receipt.Settlement.Lane} {
		if strings.Contains(raw, secret) {
			t.Fatal("passenger recovery archive retained plaintext private context")
		}
	}
	for name, mutate := range map[string]func(*PassengerReceipt){
		"lane":           func(row *PassengerReceipt) { row.Settlement.Lane = "different|lane" },
		"cost":           func(row *PassengerReceipt) { row.Settlement.ExpensesPence++ },
		"cost knowledge": func(row *PassengerReceipt) { row.CostsKnown = false },
		"clock":          func(row *PassengerReceipt) { row.TimeScale++ },
		"context":        func(row *PassengerReceipt) { row.Context = json.RawMessage(`{"different":true}`) },
		"created":        func(row *PassengerReceipt) { row.CreatedAt = row.CreatedAt.Add(time.Second) },
	} {
		t.Run(name, func(t *testing.T) {
			changed := receipt
			mutate(&changed)
			if err := database.PutPendingPassengerReceipt(ctx, &changed); !errors.Is(err, ErrActionConflict) {
				t.Fatalf("immutable recovery archive changed: %v", err)
			}
		})
	}
	if err := database.Prune(ctx); err != nil {
		t.Fatal(err)
	}
	reopened, err := Open(ctx, dsn, key, zap.NewNop())
	if err != nil {
		t.Fatal(err)
	}
	defer reopened.Close()
	pending, err := reopened.PendingPassengerReceipts(ctx, accountID)
	if err != nil || len(pending) != 1 || !sameSettlement(pending[0].Settlement, receipt.Settlement) ||
		!bytes.Equal(pending[0].Context, receipt.Context) || !pending[0].CostsKnown || pending[0].TimeScale != 60 {
		t.Fatalf("private accounting basis did not survive restart: %+v %v", pending, err)
	}
	if foreign, err := reopened.PendingPassengerReceipts(ctx, "passenger-pending-foreign"); err != nil || len(foreign) != 0 {
		t.Fatalf("pending receipt crossed accounts: %+v %v", foreign, err)
	}
	if err := reopened.PutPendingPassengerReceipt(ctx, &pending[0]); err != nil {
		t.Fatalf("pending receipt replay: %v", err)
	}
	rows, err := reopened.SettlementHistory(ctx, accountID, receipt.Settlement.Lane, 10)
	if err != nil || len(rows) != 1 || !rows[0].ProceedsUnknown || rows[0].CostsKnown || rows[0].EventID != "" || rows[0].ActionID != "" {
		t.Fatalf("unknown receipt invented realization evidence: %+v %v", rows, err)
	}
	var actions, metrics int64
	if err := reopened.db.Model(&Action{}).Where("account_id = ?", accountID).Count(&actions).Error; err != nil {
		t.Fatal(err)
	}
	if err := reopened.db.Model(&Metric{}).Where("account_id = ? AND name = 'realized_profit_pence'", accountID).Count(&metrics).Error; err != nil || actions != 0 || metrics != 0 {
		t.Fatalf("unknown receipt fabricated an action or realized metric: actions=%d metrics=%d error=%v", actions, metrics, err)
	}
}

func TestPassengerPendingReceiptNullProceedsExcludedFromMeasuredRates(t *testing.T) {
	database, _, _ := integrationStore(t)
	ctx := context.Background()
	const accountID = "passenger-unknown-economics"
	saveTestAccount(t, database, accountID)
	now := time.Now().UTC().Truncate(time.Microsecond)
	receipt := pendingPassengerReceiptFixture(accountID, "unknown-economics", now)
	if err := database.PutPendingPassengerReceipt(ctx, &receipt); err != nil {
		t.Fatal(err)
	}
	encoded, err := json.Marshal(receipt.Settlement)
	if err != nil || !bytes.Contains(encoded, []byte(`"proceeds_pence":null`)) || !bytes.Contains(encoded, []byte(`"proceeds_unknown":true`)) {
		t.Fatalf("unknown proceeds serialized as a fake numeric receipt: %s %v", encoded, err)
	}
	var restored Settlement
	if err := json.Unmarshal(encoded, &restored); err != nil || !sameSettlement(restored, receipt.Settlement) {
		t.Fatalf("nullable receipt lost private recovery identity: %+v %v", restored, err)
	}
	history, err := database.EconomicHistoryWindow(ctx, accountID, receipt.Settlement.Lane, now.Add(time.Second), time.Hour)
	if err != nil || !history.UnknownCosts || history.NetPence != 0 || history.CommercialNetPence != 0 ||
		history.PositiveCycles != 0 || history.PositiveNetPence != 0 || history.PositiveGameSeconds != 0 || history.GameSeconds != 0 {
		t.Fatalf("unknown passenger proceeds contributed measured realization: %+v %v", history, err)
	}
	money, err := database.MoneyHistory(ctx, accountID, now.Add(-time.Minute), now.Add(time.Second))
	if err != nil || len(money.Settlements) != 1 || money.Settlements[0].UnknownProceedsCount != 1 ||
		money.Settlements[0].KnownCount != 0 || money.Settlements[0].NetPence != nil || money.Settlements[0].CostsPence != nil {
		t.Fatalf("money history misclassified unknown receipt: %+v %v", money, err)
	}
	encoded, err = json.Marshal(money.Settlements[0])
	if err != nil || !bytes.Contains(encoded, []byte(`"gross_pence":null`)) {
		t.Fatalf("money history displayed fake zero proceeds: %s %v", encoded, err)
	}
	journal, err := journalSettlementRow(receipt.Settlement)
	if err != nil || journal.AmountPence != nil || journal.AmountKind != "" || !strings.Contains(journal.Summary, "proceeds unknown") || journal.CostsKnown == nil || *journal.CostsKnown {
		t.Fatalf("journal displayed an invented realized loss: %+v %v", journal, err)
	}
}

func TestPassengerReceiptPaidLostUpgradeIdempotentRestart(t *testing.T) {
	for _, kind := range []string{"passengers_paid", "passengers_lost"} {
		t.Run(kind, func(t *testing.T) {
			database, dsn, key := integrationStore(t)
			ctx := context.Background()
			const accountID = "passenger-upgrade"
			saveTestAccount(t, database, accountID)
			now := time.Now().UTC().Truncate(time.Microsecond)
			receipt := pendingPassengerReceiptFixture(accountID, "unknown-upgrade", now)
			if err := database.PutPendingPassengerReceipt(ctx, &receipt); err != nil {
				t.Fatal(err)
			}
			paid := int64(9_007_199_254_740_993)
			if kind == "passengers_lost" {
				paid = 0
			}
			resolved := receiptTerminalFixture(t, database, receipt, []string{"late-terminal"}, []string{kind}, []int64{paid})
			if err := database.PutSettlement(ctx, &resolved); !errors.Is(err, ErrActionConflict) {
				t.Fatalf("ordinary immutable insert upgraded unknown receipt: %v", err)
			}
			if err := database.ResolvePassengerReceipt(ctx, &resolved, []string{resolved.EventID}); err != nil {
				t.Fatal(err)
			}
			reopened, err := Open(ctx, dsn, key, zap.NewNop())
			if err != nil {
				t.Fatal(err)
			}
			defer reopened.Close()
			for range 2 {
				if err := reopened.ResolvePassengerReceipt(ctx, &resolved, []string{resolved.EventID}); err != nil {
					t.Fatalf("real terminal receipt replay changed: %v", err)
				}
				if err := reopened.PutPendingPassengerReceipt(ctx, &receipt); err != nil {
					t.Fatalf("archived fallback replay reverted known receipt: %v", err)
				}
			}
			rows, err := reopened.SettlementHistory(ctx, accountID, receipt.Settlement.Lane, 10)
			if err != nil || len(rows) != 1 || !sameSettlement(rows[0], resolved) || rows[0].ID != receipt.Settlement.ID ||
				rows[0].ProceedsUnknown || !rows[0].CostsKnown || rows[0].ProceedsPence != paid {
				t.Fatalf("late proof did not upgrade the same immutable accounting identity: %+v %v", rows, err)
			}
			if pending, err := reopened.PendingPassengerReceipts(ctx, accountID); err != nil || len(pending) != 0 {
				t.Fatalf("resolved receipt remained pending: %+v %v", pending, err)
			}
			var metrics int64
			if err := reopened.db.Model(&Metric{}).Where("account_id = ? AND name = 'realized_profit_pence'", accountID).Count(&metrics).Error; err != nil || metrics != 1 {
				t.Fatalf("late receipt posted realized profit more than once: %d %v", metrics, err)
			}
			history, err := reopened.EconomicHistoryWindow(ctx, accountID, receipt.Settlement.Lane, now.Add(time.Second), time.Hour)
			if err != nil || history.UnknownCosts || history.NetPence != paid-30 || history.GameSeconds != resolved.GameSeconds {
				t.Fatalf("late proof failed to replace unknown economics: %+v %v", history, err)
			}
		})
	}
}

func TestPassengerReceiptAggregateProofImmutableAndCannotBeReused(t *testing.T) {
	database, _, _ := integrationStore(t)
	ctx := context.Background()
	const accountID = "passenger-aggregate"
	saveTestAccount(t, database, accountID)
	receipt := pendingPassengerReceiptFixture(accountID, "aggregate-unknown", time.Now().UTC().Truncate(time.Microsecond))
	if err := database.PutPendingPassengerReceipt(ctx, &receipt); err != nil {
		t.Fatal(err)
	}
	ids := []string{"batch-paid-a", "batch-paid-b"}
	resolved := receiptTerminalFixture(t, database, receipt, ids, []string{"passengers_paid", "passengers_paid"}, []int64{1700, 2300})
	for name, mutate := range map[string]func(*Settlement){
		"proceeds":              func(row *Settlement) { row.ProceedsPence++ },
		"costs":                 func(row *Settlement) { row.ExpensesPence++ },
		"quantity":              func(row *Settlement) { row.QuantityTons++ },
		"lane":                  func(row *Settlement) { row.Lane = "other|lane" },
		"duration":              func(row *Settlement) { row.GameSeconds++ },
		"completion":            func(row *Settlement) { row.CompletedAt = row.CompletedAt.Add(time.Second) },
		"restore unknown costs": func(row *Settlement) { row.CostsKnown = false },
	} {
		t.Run(name, func(t *testing.T) {
			changed := resolved
			mutate(&changed)
			if err := database.ResolvePassengerReceipt(ctx, &changed, ids); !errors.Is(err, ErrActionConflict) {
				t.Fatalf("unverified accounting changed during upgrade: %v", err)
			}
		})
	}
	if err := database.ResolvePassengerReceipt(ctx, &resolved, []string{ids[0]}); !errors.Is(err, ErrActionConflict) {
		t.Fatalf("partial proof authenticated a whole batch: %v", err)
	}
	if err := database.ResolvePassengerReceipt(ctx, &resolved, ids); err != nil {
		t.Fatal(err)
	}
	if err := database.ResolvePassengerReceipt(ctx, &resolved, ids); err != nil {
		t.Fatalf("aggregate proof was not idempotent: %v", err)
	}
	for index, id := range ids {
		ordinary := passageSettlementFixture(accountID, fmt.Sprintf("proof-reuse-%d", index), id, []int64{1700, 2300}[index], receipt.Settlement.CompletedAt)
		if err := database.PutSettlement(ctx, &ordinary); !errors.Is(err, ErrActionConflict) {
			t.Fatalf("linked terminal event funded another settlement: %v", err)
		}
	}
	var links int64
	if err := database.db.Model(&passengerReceiptEventRow{}).Where("settlement_id = ?", resolved.ID).Count(&links).Error; err != nil || links != 2 {
		t.Fatalf("aggregate did not retain every exact event anchor: %d %v", links, err)
	}
	var archive passengerReceiptRow
	if err := database.db.Where("id = ?", receipt.Settlement.ID).Take(&archive).Error; err != nil {
		t.Fatal(err)
	}
	if err := database.db.Model(&passengerReceiptRow{}).Where("id = ?", archive.ID).Update("resolved", false).Error; err == nil {
		t.Fatal("resolved receipt reverted to unknown")
	}
	if err := database.db.Model(&passengerReceiptRow{}).Where("id = ?", archive.ID).Update("context_cipher", archive.ContextCipher[:len(archive.ContextCipher)-1]).Error; err == nil {
		t.Fatal("database allowed archived accounting context mutation")
	}
}

func TestPassengerReceiptUnknownOriginalCostsRemainUnknown(t *testing.T) {
	database, _, _ := integrationStore(t)
	ctx := context.Background()
	const accountID = "passenger-unknown-original-costs"
	saveTestAccount(t, database, accountID)
	receipt := pendingPassengerReceiptFixture(accountID, "unknown-basis", time.Now().UTC().Truncate(time.Microsecond))
	receipt.CostsKnown = false
	if err := database.PutPendingPassengerReceipt(ctx, &receipt); err != nil {
		t.Fatal(err)
	}
	resolved := receiptTerminalFixture(t, database, receipt, []string{"known-fare-unknown-cost"}, []string{"passengers_paid"}, []int64{2000})
	invented := resolved
	invented.CostsKnown = true
	if err := database.ResolvePassengerReceipt(ctx, &invented, []string{resolved.EventID}); !errors.Is(err, ErrActionConflict) {
		t.Fatalf("real fare invented an absent original cost basis: %v", err)
	}
	if err := database.ResolvePassengerReceipt(ctx, &resolved, []string{resolved.EventID}); err != nil {
		t.Fatal(err)
	}
	rows, err := database.SettlementHistory(ctx, accountID, "", 10)
	if err != nil || len(rows) != 1 || rows[0].ProceedsUnknown || rows[0].CostsKnown || rows[0].ProceedsPence != 2000 {
		t.Fatalf("upgrade lost measured fare versus unknown costs distinction: %+v %v", rows, err)
	}
	var metrics int64
	if err := database.db.Model(&Metric{}).Where("account_id = ?", accountID).Count(&metrics).Error; err != nil || metrics != 0 {
		t.Fatalf("unknown cost basis posted measured realization: %d %v", metrics, err)
	}
}

func TestPassengerReceiptValidationOwnedWritesAndCrashRecovery(t *testing.T) {
	database, _, _ := integrationStore(t)
	ctx := context.Background()
	const accountID = "passenger-receipt-owner"
	saveTestAccount(t, database, accountID)
	saveTestAccount(t, database, "passenger-receipt-foreign")
	base := pendingPassengerReceiptFixture(accountID, "pending-owner", time.Now().UTC().Truncate(time.Microsecond))
	for name, mutate := range map[string]func(*PassengerReceipt){
		"negative cost": func(row *PassengerReceipt) { row.Settlement.ExpensesPence = -1 },
		"overflow cost": func(row *PassengerReceipt) {
			row.Settlement.AcquisitionPence = math.MaxInt64
			row.Settlement.ExpensesPence = math.MaxInt64
		},
		"fake action":   func(row *PassengerReceipt) { row.Settlement.ActionID = "invented-action" },
		"fake event":    func(row *PassengerReceipt) { row.Settlement.EventID = "invented-event" },
		"fake proceeds": func(row *PassengerReceipt) { row.Settlement.ProceedsPence = 100 },
		"known costs":   func(row *PassengerReceipt) { row.Settlement.CostsKnown = true },
		"invalid json":  func(row *PassengerReceipt) { row.Context = json.RawMessage(`{`) },
		"invalid clock": func(row *PassengerReceipt) { row.TimeScale = 0 },
		"missing start": func(row *PassengerReceipt) { row.Settlement.StartedAt = time.Time{} },
	} {
		t.Run(name, func(t *testing.T) {
			invalid := base
			mutate(&invalid)
			if err := database.PutPendingPassengerReceipt(ctx, &invalid); !errors.Is(err, ErrInvalidInput) {
				t.Fatalf("invalid immutable archive was accepted: %v", err)
			}
		})
	}
	if pending, err := database.PendingPassengerReceipts(ctx, accountID); err != nil || len(pending) != 0 {
		t.Fatalf("invalid fields left a durable poisoned archive: %+v %v", pending, err)
	}
	if err := database.PutSettlement(ctx, &base.Settlement); !errors.Is(err, ErrNotFound) {
		t.Fatalf("unknown settlement was inserted without durable recovery context: %v", err)
	}
	owner, release, err := database.AcquireAccountContext(ctx, accountID)
	if err != nil {
		t.Fatal(err)
	}
	defer release()
	foreign := base
	foreign.Settlement.AccountID = "passenger-receipt-foreign"
	if err := database.PutPendingPassengerReceipt(owner, &foreign); !errors.Is(err, ErrOwnershipLost) {
		t.Fatalf("owner archived another account's passage: %v", err)
	}
	if err := database.DeferPassengerReceipt(owner, foreign.Settlement.AccountID, foreign.Settlement.ID, time.Now()); !errors.Is(err, ErrOwnershipLost) {
		t.Fatalf("owner deferred another account's passage: %v", err)
	}
	if err := database.ClosePassengerReceipt(owner, foreign.Settlement.AccountID, foreign.Settlement.ID, "quarantined", time.Now()); !errors.Is(err, ErrOwnershipLost) {
		t.Fatalf("owner closed another account's passage: %v", err)
	}
	if err := database.PutPendingPassengerReceipt(owner, &base); err != nil {
		t.Fatal(err)
	}
	// Recreate the persisted state of a crash after the archive commit but before
	// the idempotent unknown settlement insert. No action or payment is invented.
	if err := database.db.Where("id = ?", base.Settlement.ID).Delete(&settlementRow{}).Error; err != nil {
		t.Fatal(err)
	}
	pending, err := database.PendingPassengerReceipts(ctx, accountID)
	if err != nil || len(pending) != 1 || !pending[0].SettlementMissing {
		t.Fatalf("archive-only crash lost completion: %+v %v", pending, err)
	}
	if err := database.PutSettlement(owner, &pending[0].Settlement); err != nil {
		t.Fatalf("archive-only crash was not replayable: %v", err)
	}
	resolved := receiptTerminalFixture(t, database, base, []string{"owned-terminal"}, []string{"passengers_paid"}, []int64{3000})
	wrongAccount := resolved
	wrongAccount.AccountID = foreign.Settlement.AccountID
	if err := database.ResolvePassengerReceipt(owner, &wrongAccount, []string{resolved.EventID}); !errors.Is(err, ErrOwnershipLost) {
		t.Fatalf("owner resolved another account's passage: %v", err)
	}
	if err := database.ResolvePassengerReceipt(owner, &resolved, []string{"missing-event"}); !errors.Is(err, ErrInvalidInput) {
		t.Fatalf("primary proof mismatch accepted: %v", err)
	}
	if err := database.ResolvePassengerReceipt(owner, &resolved, []string{resolved.EventID}); err != nil {
		t.Fatal(err)
	}
}

func TestPassengerReceiptMigrationRoundTripPreservesLegacyAndRefusesArchives(t *testing.T) {
	database, _, _ := integrationStore(t)
	ctx := context.Background()
	const accountID = "passenger-receipt-migration"
	saveTestAccount(t, database, accountID)
	legacy := putEconomicEvidence(t, database, accountID, "receipt-legacy", "spot", "legacy-lane", 4, 100, 20, true, nil)
	raw, err := migrations.ReadFile("migrations/00014_passenger_receipts.sql")
	if err != nil {
		t.Fatal(err)
	}
	lifecycle, err := migrations.ReadFile("migrations/00015_passenger_receipt_lifecycle.sql")
	if err != nil {
		t.Fatal(err)
	}
	provider, err := goose.NewProvider(goose.DialectPostgres, database.sqlDB,
		fstest.MapFS{"00014_passenger_receipts.sql": {Data: raw}},
		goose.WithDisableGlobalRegistry(true), goose.WithDisableVersioning(true))
	if err != nil {
		t.Fatal(err)
	}
	lifecycleProvider, err := goose.NewProvider(goose.DialectPostgres, database.sqlDB,
		fstest.MapFS{"00015_passenger_receipt_lifecycle.sql": {Data: lifecycle}},
		goose.WithDisableGlobalRegistry(true), goose.WithDisableVersioning(true))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := lifecycleProvider.Down(ctx); err != nil {
		t.Fatalf("receipt lifecycle migration down: %v", err)
	}
	if _, err := provider.Down(ctx); err != nil {
		t.Fatalf("receipt migration down: %v", err)
	}
	if _, err := provider.UpTo(ctx, 14); err != nil {
		t.Fatalf("receipt migration up: %v", err)
	}
	if _, err := lifecycleProvider.UpTo(ctx, 15); err != nil {
		t.Fatalf("receipt lifecycle migration up: %v", err)
	}
	rows, err := database.SettlementHistory(ctx, accountID, legacy.Lane, 10)
	if err != nil || len(rows) != 1 || !sameSettlement(rows[0], legacy) || rows[0].ProceedsUnknown {
		t.Fatalf("receipt migration changed existing realized accounting: %+v %v", rows, err)
	}
	if err := database.PutSettlement(ctx, &legacy); err != nil {
		t.Fatal(err)
	}
	receipt := pendingPassengerReceiptFixture(accountID, "migration-archive", time.Now().UTC())
	if err := database.PutPendingPassengerReceipt(ctx, &receipt); err != nil {
		t.Fatal(err)
	}
	if _, err := provider.Down(ctx); err == nil {
		t.Fatal("migration rollback discarded unresolved accounting recovery")
	}
	if _, err := lifecycleProvider.Down(ctx); err == nil {
		t.Fatal("lifecycle rollback discarded unresolved accounting recovery")
	}
	if pending, err := database.PendingPassengerReceipts(ctx, accountID); err != nil || len(pending) != 1 {
		t.Fatalf("failed migration rollback changed immutable recovery: %+v %v", pending, err)
	}
}

func TestPassengerSettlementEventIDDisplayScrubPreservesDurableAnchor(t *testing.T) {
	database, dsn, key := integrationStore(t)
	ctx := context.Background()
	const accountID = "passenger-display-anchor"
	credentials := saveTestAccount(t, database, accountID)
	eventID := "paid-" + credentials.APIKey + "-" + credentials.Passphrase
	retainedPassengerEvent(t, database, accountID, eventID, "passengers_paid", json.RawMessage(`{"fare_paid_pence":1000}`))
	settlement := passageSettlementFixture(accountID, "display-anchor", eventID, 1000, time.Now().UTC())
	if err := database.PutSettlement(ctx, &settlement); err != nil {
		t.Fatal(err)
	}
	assertPrivateDisplays(t, database, accountID, credentials.APIKey, credentials.Passphrase)
	reopened, err := Open(ctx, dsn, key, zap.NewNop())
	if err != nil {
		t.Fatal(err)
	}
	defer reopened.Close()
	durable, err := reopened.SettlementHistory(ctx, accountID, "", 10)
	if err != nil || len(durable) != 1 || durable[0].EventID != eventID {
		t.Fatalf("display projection mutated exact retained evidence anchor: %+v %v", durable, err)
	}
	if err := reopened.PutSettlement(ctx, &durable[0]); err != nil {
		t.Fatalf("display scrubbing broke authenticated settlement replay: %v", err)
	}
	projected, err := reopened.History(ctx, accountID, "settlements", 10)
	if err != nil {
		t.Fatal(err)
	}
	rows, ok := projected.([]Settlement)
	if !ok || len(rows) != 1 || rows[0].EventID == eventID || !strings.Contains(rows[0].EventID, redacted) {
		t.Fatalf("display event ID remained untrusted plaintext: %+v", projected)
	}
	if err := reopened.db.Model(&accountSecret{}).Where("account_id = ?", accountID).Update("ciphertext", []byte("corrupt")).Error; err != nil {
		t.Fatal(err)
	}
	projected, err = reopened.History(ctx, accountID, "settlements", 10)
	if err != nil {
		t.Fatal(err)
	}
	rows = projected.([]Settlement)
	if len(rows) != 1 || rows[0].EventID != redacted {
		t.Fatalf("credential failure did not censor event anchor display: %+v", rows)
	}
}

type passengerReceiptCountingAEAD struct {
	cipher.AEAD
	opens int
	seals int
}

func (a *passengerReceiptCountingAEAD) Open(dst, nonce, ciphertext, additionalData []byte) ([]byte, error) {
	a.opens++
	return a.AEAD.Open(dst, nonce, ciphertext, additionalData)
}

func (a *passengerReceiptCountingAEAD) Seal(dst, nonce, plaintext, additionalData []byte) []byte {
	a.seals++
	return a.AEAD.Seal(dst, nonce, plaintext, additionalData)
}

func TestPassengerReceiptDueBatchBoundsDecryptionAndIsolatesUnreadableArchive(t *testing.T) {
	database, _, _ := integrationStore(t)
	ctx := context.Background()
	const accountID = "passenger-receipt-due-batch"
	saveTestAccount(t, database, accountID)
	now := time.Now().UTC().Truncate(time.Microsecond)
	created := now.Add(-time.Hour)
	for index := range PassengerReceiptBatchLimit + 3 {
		receipt := pendingPassengerReceiptFixture(accountID, fmt.Sprintf("due-%02d", index), now)
		receipt.CreatedAt = created
		if err := database.PutPendingPassengerReceipt(ctx, &receipt); err != nil {
			t.Fatal(err)
		}
	}
	for _, row := range []passengerReceiptRow{
		{ID: "a-unreadable", AccountID: accountID, ContextCipher: make([]byte, 28), CreatedAt: created, NextScanAt: created, ExpiresAt: created.Add(PassengerReceiptLifetime)},
		{ID: "not-due-unreadable", AccountID: accountID, ContextCipher: make([]byte, 28), CreatedAt: created, NextScanAt: now.Add(PassengerReceiptRetryInterval), ExpiresAt: created.Add(PassengerReceiptLifetime)},
	} {
		if err := database.db.Create(&row).Error; err != nil {
			t.Fatal(err)
		}
	}
	counter := &passengerReceiptCountingAEAD{AEAD: database.aead}
	database.aead = counter
	for _, readDue := range []func() ([]PassengerReceipt, error){
		func() ([]PassengerReceipt, error) {
			return database.DuePassengerReceipts(ctx, accountID, now, PassengerReceiptBatchLimit)
		},
		func() ([]PassengerReceipt, error) { return database.PendingPassengerReceipts(ctx, accountID) },
	} {
		counter.opens, counter.seals = 0, 0
		rows, err := readDue()
		if err != nil || len(rows) != PassengerReceiptBatchLimit || counter.opens != PassengerReceiptBatchLimit || counter.seals != 0 {
			t.Fatalf("due query decrypted an unbounded or not-due archive: count=%d opens=%d seals=%d error=%v", len(rows), counter.opens, counter.seals, err)
		}
		if rows[0].Settlement.ID != "a-unreadable" || rows[0].ReadError == nil {
			t.Fatalf("unreadable receipt lost its trusted quarantine identity: %+v", rows[0])
		}
		for _, row := range rows[1:] {
			if row.ReadError != nil || len(row.Context) == 0 || row.SettlementMissing || row.Settlement.ID == "not-due-unreadable" {
				t.Fatalf("bad first archive hid healthy later receipts or decrypted deferred history: %+v", row)
			}
		}
	}
	for _, limit := range []int{0, MaxHistoryLimit + 1} {
		if _, err := database.DuePassengerReceipts(ctx, accountID, now, limit); !errors.Is(err, ErrInvalidInput) {
			t.Fatalf("unbounded due batch accepted: %d %v", limit, err)
		}
	}
}

func TestPassengerReceiptBackoffIsMetadataOnlyAndDurableAcrossRestart(t *testing.T) {
	database, dsn, key := integrationStore(t)
	ctx := context.Background()
	const accountID = "passenger-receipt-backoff"
	saveTestAccount(t, database, accountID)
	now := time.Now().UTC().Truncate(time.Microsecond)
	receipt := pendingPassengerReceiptFixture(accountID, "metadata-backoff", now)
	receipt.CreatedAt = now
	if err := database.PutPendingPassengerReceipt(ctx, &receipt); err != nil {
		t.Fatal(err)
	}
	var original passengerReceiptRow
	if err := database.db.Where("id = ?", receipt.Settlement.ID).Take(&original).Error; err != nil {
		t.Fatal(err)
	}
	counter := &passengerReceiptCountingAEAD{AEAD: database.aead}
	database.aead = counter
	if err := database.DeferPassengerReceipt(ctx, accountID, receipt.Settlement.ID, now); err != nil {
		t.Fatal(err)
	}
	if counter.opens != 0 || counter.seals != 0 {
		t.Fatalf("backoff decrypted or reencrypted immutable context: %+v", counter)
	}
	var before string
	if err := database.db.Raw("SELECT xmin::text FROM passenger_receipts WHERE id = ?", receipt.Settlement.ID).Scan(&before).Error; err != nil {
		t.Fatal(err)
	}
	for range 3 {
		rows, err := database.DuePassengerReceipts(ctx, accountID, now.Add(PassengerReceiptRetryInterval-time.Microsecond), PassengerReceiptBatchLimit)
		if err != nil || len(rows) != 0 || counter.opens != 0 || counter.seals != 0 {
			t.Fatalf("not-due receipt repeated reads or writes: %+v opens=%d seals=%d error=%v", rows, counter.opens, counter.seals, err)
		}
	}
	var after string
	if err := database.db.Raw("SELECT xmin::text FROM passenger_receipts WHERE id = ?", receipt.Settlement.ID).Scan(&after).Error; err != nil || before != after {
		t.Fatalf("not-due scans rewrote archive metadata: %s -> %s %v", before, after, err)
	}
	reopened, err := Open(ctx, dsn, key, zap.NewNop())
	if err != nil {
		t.Fatal(err)
	}
	defer reopened.Close()
	rows, err := reopened.DuePassengerReceipts(ctx, accountID, now.Add(PassengerReceiptRetryInterval), PassengerReceiptBatchLimit)
	if err != nil || len(rows) != 1 || !rows[0].NextScanAt.Equal(now.Add(PassengerReceiptRetryInterval)) {
		t.Fatalf("restart lost exact due boundary: %+v %v", rows, err)
	}
	var stored passengerReceiptRow
	if err := reopened.db.Where("id = ?", receipt.Settlement.ID).Take(&stored).Error; err != nil ||
		!bytes.Equal(original.ContextCipher, stored.ContextCipher) || !original.CreatedAt.Equal(stored.CreatedAt) || !original.ExpiresAt.Equal(stored.ExpiresAt) {
		t.Fatalf("backoff changed private accounting identity: %+v %v", stored, err)
	}
}

func TestPassengerReceiptExpiryClosesScanWithoutDecryptingOrRealizingZero(t *testing.T) {
	database, dsn, key := integrationStore(t)
	ctx := context.Background()
	const accountID = "passenger-receipt-expiry"
	saveTestAccount(t, database, accountID)
	now := time.Now().UTC().Truncate(time.Microsecond)
	receipt := pendingPassengerReceiptFixture(accountID, "expiry-unknown", now)
	receipt.CreatedAt = now
	if err := database.PutPendingPassengerReceipt(ctx, &receipt); err != nil {
		t.Fatal(err)
	}
	if err := database.ClosePassengerReceipt(ctx, accountID, receipt.Settlement.ID, "expired", receipt.ExpiresAt.Add(-time.Microsecond)); !errors.Is(err, ErrActionConflict) {
		t.Fatalf("receipt expired before seven real days: %v", err)
	}
	counter := &passengerReceiptCountingAEAD{AEAD: database.aead}
	database.aead = counter
	rows, err := database.DuePassengerReceipts(ctx, accountID, receipt.ExpiresAt, PassengerReceiptBatchLimit)
	if err != nil || len(rows) != 1 || rows[0].Settlement.ID != receipt.Settlement.ID || len(rows[0].Context) != 0 || counter.opens != 0 || counter.seals != 0 {
		t.Fatalf("expiry decrypted accounting or skipped its exact boundary: %+v opens=%d seals=%d %v", rows, counter.opens, counter.seals, err)
	}
	for range 2 {
		if err := database.ClosePassengerReceipt(ctx, accountID, receipt.Settlement.ID, "expired", receipt.ExpiresAt); err != nil {
			t.Fatal(err)
		}
	}
	reopened, err := Open(ctx, dsn, key, zap.NewNop())
	if err != nil {
		t.Fatal(err)
	}
	defer reopened.Close()
	if due, err := reopened.DuePassengerReceipts(ctx, accountID, receipt.ExpiresAt.Add(time.Hour), PassengerReceiptBatchLimit); err != nil || len(due) != 0 {
		t.Fatalf("expired receipt reentered scans after restart: %+v %v", due, err)
	}
	resolved := receiptTerminalFixture(t, reopened, receipt, []string{"too-late-proof"}, []string{"passengers_paid"}, []int64{1000})
	if err := reopened.ResolvePassengerReceipt(ctx, &resolved, []string{resolved.EventID}); !errors.Is(err, ErrActionConflict) {
		t.Fatalf("late proof reopened expired accounting: %v", err)
	}
	history, err := reopened.SettlementHistory(ctx, accountID, "", 10)
	if err != nil || len(history) != 1 || !history[0].ProceedsUnknown || history[0].CostsKnown || history[0].EventID != "" {
		t.Fatalf("expiry fabricated realization: %+v %v", history, err)
	}
	encoded, err := json.Marshal(history[0])
	if err != nil || !bytes.Contains(encoded, []byte(`"proceeds_pence":null`)) {
		t.Fatalf("expired unknown became numeric zero: %s %v", encoded, err)
	}
	if err := reopened.db.Model(&passengerReceiptRow{}).Where("id = ?", receipt.Settlement.ID).
		Updates(map[string]any{"closed_at": nil, "close_reason": ""}).Error; err == nil {
		t.Fatal("database reopened a permanently closed receipt")
	}
}

func TestPassengerReceiptLifecycleMigrationSchedulesExistingPrivateArchives(t *testing.T) {
	database, _, _ := integrationStore(t)
	ctx := context.Background()
	const accountID = "passenger-receipt-lifecycle-backfill"
	saveTestAccount(t, database, accountID)
	raw, err := migrations.ReadFile("migrations/00015_passenger_receipt_lifecycle.sql")
	if err != nil {
		t.Fatal(err)
	}
	provider, err := goose.NewProvider(goose.DialectPostgres, database.sqlDB,
		fstest.MapFS{"00015_passenger_receipt_lifecycle.sql": {Data: raw}},
		goose.WithDisableGlobalRegistry(true), goose.WithDisableVersioning(true))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := provider.Down(ctx); err != nil {
		t.Fatal(err)
	}
	completed := time.Now().UTC().Add(-time.Hour).Truncate(time.Microsecond)
	receipt := pendingPassengerReceiptFixture(accountID, "pre-lifecycle-archive", completed)
	receipt.CreatedAt = completed
	payload := passengerReceiptPayload{Settlement: receipt.Settlement, CostsKnown: receipt.CostsKnown,
		TimeScale: receipt.TimeScale, Context: receipt.Context}
	ciphertext, err := database.encryptPrivate(payload, "passenger_receipts", "context", accountID, receipt.Settlement.ID)
	if err != nil {
		t.Fatal(err)
	}
	if err := database.db.Exec(`INSERT INTO passenger_receipts (id, account_id, context_cipher, created_at)
		VALUES (?, ?, ?, ?)`, receipt.Settlement.ID, accountID, ciphertext, receipt.CreatedAt).Error; err != nil {
		t.Fatal(err)
	}
	if err := database.PutSettlement(ctx, &receipt.Settlement); err != nil {
		t.Fatal(err)
	}
	if _, err := provider.UpTo(ctx, 15); err != nil {
		t.Fatal(err)
	}
	pending, err := database.PendingPassengerReceipts(ctx, accountID)
	if err != nil || len(pending) != 1 || !sameSettlement(pending[0].Settlement, receipt.Settlement) ||
		!pending[0].NextScanAt.Equal(receipt.CreatedAt) || !pending[0].ExpiresAt.Equal(receipt.CreatedAt.Add(PassengerReceiptLifetime)) {
		t.Fatalf("lifecycle migration changed accounting or failed to bound an existing archive: %+v %v", pending, err)
	}
	var stored passengerReceiptRow
	if err := database.db.Where("id = ?", receipt.Settlement.ID).Take(&stored).Error; err != nil || !bytes.Equal(stored.ContextCipher, ciphertext) {
		t.Fatalf("migration reencrypted immutable private context: %+v %v", stored, err)
	}
}
