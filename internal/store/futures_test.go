package store

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/DevYukine/go-tradewinds/internal/gameapi"
)

func futuresLedgerFixture(t *testing.T, db *Store, side string) FuturesIntent {
	t.Helper()
	now := time.Now().UTC().Truncate(time.Microsecond)
	row := FuturesIntent{AccountID: "pilot", IntentID: side, ActionID: "create-" + side, CohortID: "measured-pair",
		PortID: "amsterdam", CommodityID: "water_beer", Side: side, LotTons: 1, Status: "intent", CreatedAt: now, MaturesAt: now.Add(30 * time.Minute)}
	if side == "ask" {
		row.StrikePricePence, row.ReservedTons, row.MarginPence = 1700, 1, 425
		row.ReservedCashPence, row.ExposurePence = 425, 2525
	} else {
		row.StrikePricePence, row.MarginPence = 2300, 575
		row.ReservedCashPence, row.ExposurePence = 2937, 2937
	}
	if err := db.PutFuturesIntent(context.Background(), &row); err != nil {
		t.Fatal(err)
	}
	return row
}

func futuresContractFixture(row FuturesIntent, status string) gameapi.FuturesContract {
	company := "company-pilot"
	contract := gameapi.FuturesContract{ID: "future-" + row.Side, CreatorCompanyID: company, PortID: row.PortID, CommodityID: row.CommodityID,
		Side: row.Side, LotTons: 1, StrikePricePence: row.StrikePricePence, Status: status,
		CreatedAt: row.CreatedAt, UpdatedAt: row.CreatedAt, MaturesAt: row.MaturesAt}
	if row.Side == "ask" {
		contract.SellerCompanyID, contract.SellerMarginPence = &company, row.MarginPence
	} else {
		contract.BuyerCompanyID, contract.BuyerMarginPence = &company, row.MarginPence
	}
	return contract
}

func TestFuturesReservationsAndMeasuredPairLoss(t *testing.T) {
	db, _, _ := integrationStore(t)
	saveTestAccount(t, db, "pilot")
	ctx := context.Background()
	ask := futuresLedgerFixture(t, db, "ask")
	bid := futuresLedgerFixture(t, db, "bid")
	if reserve, err := db.FuturesCashReserve(ctx, "pilot"); err != nil || reserve != 3362 {
		t.Fatalf("pre-send coverage = %dp, %v", reserve, err)
	}
	for _, row := range []FuturesIntent{ask, bid} {
		if err := db.ObserveFuturesContract(ctx, "pilot", row.IntentID, futuresContractFixture(row, "active")); err != nil {
			t.Fatal(err)
		}
	}
	if reserve, err := db.FuturesCashReserve(ctx, "pilot"); err != nil || reserve != 2362 {
		t.Fatalf("escrow margin was reserved twice: %dp, %v", reserve, err)
	}
	for _, row := range []FuturesIntent{ask, bid} {
		receipt, _ := json.Marshal(gameapi.SettleFuturesResult{FuturesID: "future-" + row.Side, Status: "settled", SettledTons: 1})
		if err := db.RecordFuturesOutcome(ctx, "pilot", row.IntentID, "settled", receipt); err != nil {
			t.Fatal(err)
		}
	}
	if err := db.RecordFuturesCashFlow(ctx, "pilot", "ask", "margin-refund", 0, json.RawMessage(`{"internal_vault_escrow_transfer":true}`)); err != nil {
		t.Fatal(err)
	}
	if err := db.RecordFuturesPairPnL(ctx, "pilot", "measured-pair", FuturesCohortAccounting{}); err != nil {
		t.Fatal(err)
	}
	rows, err := db.FuturesIntents(ctx, "pilot")
	if err != nil {
		t.Fatal(err)
	}
	for _, row := range rows {
		if row.RealizedPnLPence != nil {
			t.Fatal("margin return or a status alone invented realized profit")
		}
	}
	for _, flow := range []struct {
		side  string
		delta int64
	}{{"ask", 1700}, {"bid", -2362}} {
		if err := db.RecordFuturesCashFlow(ctx, "pilot", flow.side, "principal-"+flow.side, flow.delta, json.RawMessage(`{"measured":true}`)); err != nil {
			t.Fatal(err)
		}
	}
	if err := db.RecordFuturesPairPnL(ctx, "pilot", "measured-pair", FuturesCohortAccounting{}); err != nil {
		t.Fatal(err)
	}
	rows, err = db.FuturesIntents(ctx, "pilot")
	if err != nil {
		t.Fatal(err)
	}
	var net int64
	for _, row := range rows {
		if row.RealizedPnLPence == nil || row.PnLBasis != "measured cohort cash after procurement and liquidation; rent unknown; unpaid default claims excluded" {
			t.Fatalf("physical measured pair lacks labelled realized accounting: %+v", row)
		}
		net += *row.RealizedPnLPence
	}
	if net != -662 {
		t.Fatalf("signed replacement-cost loss = %dp, want -662p including measured customs", net)
	}
}

func TestFuturesImmutableCoverageCannotBeRewritten(t *testing.T) {
	db, _, _ := integrationStore(t)
	saveTestAccount(t, db, "pilot")
	row := futuresLedgerFixture(t, db, "ask")
	for _, change := range []func(*FuturesIntent){
		func(row *FuturesIntent) { row.LotTons = 2 },
		func(row *FuturesIntent) { row.ReservedTons = 0 },
		func(row *FuturesIntent) { row.ReservedCashPence-- },
		func(row *FuturesIntent) { row.MarginPence++ },
		func(row *FuturesIntent) { row.MaturesAt = row.MaturesAt.Add(time.Second) },
	} {
		candidate := row
		change(&candidate)
		if err := db.PutFuturesIntent(context.Background(), &candidate); !errors.Is(err, ErrActionConflict) {
			t.Fatalf("changed immutable coverage did not conflict: %v", err)
		}
	}
}

func TestFuturesDefaultKeepsBuyerCashProtected(t *testing.T) {
	db, _, _ := integrationStore(t)
	saveTestAccount(t, db, "pilot")
	row := futuresLedgerFixture(t, db, "bid")
	if err := db.ObserveFuturesContract(context.Background(), "pilot", row.IntentID, futuresContractFixture(row, "defaulted")); err != nil {
		t.Fatal(err)
	}
	if reserve, err := db.FuturesCashReserve(context.Background(), "pilot"); err != nil || reserve != 2362 {
		t.Fatalf("unattributed default released potential claim coverage: %dp, %v", reserve, err)
	}
}

func TestFuturesPermanentEvidenceRedactsCredentials(t *testing.T) {
	db, _, _ := integrationStore(t)
	credentials := saveTestAccount(t, db, "pilot")
	ctx := context.Background()
	credentials.UnderworldAPIKey = "observer-key-exact-secret"
	if err := db.SaveAccount(ctx, Account{ID: "pilot", Label: "pilot", CompanyID: "company-pilot", Enabled: true}, credentials); err != nil {
		t.Fatal(err)
	}
	row := futuresLedgerFixture(t, db, "ask")
	contract := futuresContractFixture(row, "open")
	counterparty := credentials.Passphrase + " " + credentials.APIKey + " " + credentials.UnderworldAPIKey
	contract.BuyerCompanyID = &counterparty
	if err := db.ObserveFuturesContract(ctx, "pilot", row.IntentID, contract); err != nil {
		t.Fatal(err)
	}
	evidence, err := json.Marshal(map[string]any{"note": counterparty, "api_key": credentials.APIKey})
	if err != nil {
		t.Fatal(err)
	}
	if err := db.RecordFuturesOutcome(ctx, "pilot", row.IntentID, "cancelled", evidence); err != nil {
		t.Fatal(err)
	}
	if err := db.RecordFuturesCashFlow(ctx, "pilot", row.IntentID, "redaction-evidence", 0, evidence); err != nil {
		t.Fatal(err)
	}
	rows, err := db.FuturesIntents(ctx, "pilot")
	if err != nil {
		t.Fatal(err)
	}
	query, cancel := db.query(ctx)
	defer cancel()
	var flow futuresCashFlow
	if err := query.Where("account_id = ? AND ledger_id = ?", "pilot", "redaction-evidence").Take(&flow).Error; err != nil {
		t.Fatal(err)
	}
	for _, body := range []json.RawMessage{rows[0].Contract, rows[0].Outcome, flow.Evidence} {
		for _, secret := range credentialSecrets(credentials) {
			if bytes.Contains(body, []byte(secret)) || !bytes.Contains(body, []byte(redacted)) {
				t.Fatal("permanent futures evidence retained a credential")
			}
		}
	}
}

func TestFuturesCohortPnLIncludesProcurementAndLiquidationForSingleLegs(t *testing.T) {
	for _, test := range []struct {
		name       string
		sides      []string
		accounting FuturesCohortAccounting
		want       int64
	}{
		{"ask only", []string{"ask"}, FuturesCohortAccounting{ProcurementPence: 2157, SetupOwned: true, WindDownComplete: true}, -457},
		{"bid only", []string{"bid"}, FuturesCohortAccounting{LiquidationPence: 1900, LiquidatedTons: 1, WindDownComplete: true}, -462},
		{"owned pair", []string{"ask", "bid"}, FuturesCohortAccounting{ProcurementPence: 2157, LiquidationPence: 1900, LiquidatedTons: 1, SetupOwned: true, WindDownComplete: true}, -919},
		{"inventory-only reconciliation", []string{"ask", "bid"}, FuturesCohortAccounting{LiquidatedTons: 1, SetupOwned: true, WindDownComplete: true, CashUnknown: true}, 0},
	} {
		t.Run(test.name, func(t *testing.T) {
			db, _, _ := integrationStore(t)
			saveTestAccount(t, db, "pilot")
			ctx := context.Background()
			for _, side := range test.sides {
				row := futuresLedgerFixture(t, db, side)
				if err := db.ObserveFuturesContract(ctx, "pilot", side, futuresContractFixture(row, "active")); err != nil {
					t.Fatal(err)
				}
				receipt, _ := json.Marshal(gameapi.SettleFuturesResult{FuturesID: "future-" + side, Status: "settled", SettledTons: 1})
				if err := db.RecordFuturesOutcome(ctx, "pilot", side, "settled", receipt); err != nil {
					t.Fatal(err)
				}
				delta := int64(1700)
				if side == "bid" {
					delta = -2362
				}
				if err := db.RecordFuturesCashFlow(ctx, "pilot", side, "principal-"+side, delta, json.RawMessage(`{"measured":true}`)); err != nil {
					t.Fatal(err)
				}
			}
			if err := db.RecordFuturesPairPnL(ctx, "pilot", "measured-pair", test.accounting); err != nil {
				t.Fatal(err)
			}
			rows, err := db.FuturesIntents(ctx, "pilot")
			if err != nil {
				t.Fatal(err)
			}
			var total int64
			for _, row := range rows {
				if test.accounting.CashUnknown {
					if row.RealizedPnLPence != nil || row.PnLBasis != "procurement or liquidation cash unknown; rent unknown" {
						t.Fatalf("inventory-only reconciliation invented realized cash: %+v", row)
					}
					continue
				}
				if row.RealizedPnLPence == nil || !strings.Contains(row.PnLBasis, "rent unknown") {
					t.Fatalf("missing measured cost accounting or honest rent basis: %+v", row)
				}
				total += *row.RealizedPnLPence
			}
			if total != test.want {
				t.Fatalf("cohort P&L = %d, want %d", total, test.want)
			}
		})
	}
}

func TestFuturesDefaultHTTPReceiptDoesNotIdentifyOurOpenClaim(t *testing.T) {
	db, _, _ := integrationStore(t)
	saveTestAccount(t, db, "pilot")
	ctx := context.Background()
	row := futuresLedgerFixture(t, db, "bid")
	if err := db.ObserveFuturesContract(ctx, "pilot", row.IntentID, futuresContractFixture(row, "defaulted")); err != nil {
		t.Fatal(err)
	}
	receipt, _ := json.Marshal(gameapi.SettleFuturesResult{FuturesID: "future-bid", Status: "defaulted", PenaltyPaidPence: 575, ClaimPence: 125})
	if err := db.RecordFuturesOutcome(ctx, "pilot", row.IntentID, "defaulted", receipt); err != nil {
		t.Fatal(err)
	}
	if reserve, err := db.FuturesCashReserve(ctx, "pilot"); err != nil || reserve != 2362 {
		t.Fatalf("unattributed receipt released possible outstanding debt: %d, %v", reserve, err)
	}
}

func TestFuturesAccountDeletionCascadesCashFlows(t *testing.T) {
	db, _, _ := integrationStore(t)
	saveTestAccount(t, db, "pilot")
	row := futuresLedgerFixture(t, db, "ask")
	if err := db.RecordFuturesCashFlow(context.Background(), "pilot", row.IntentID, "cash", 0, json.RawMessage(`{"measured":true}`)); err != nil {
		t.Fatal(err)
	}
	if err := db.db.Delete(&Account{}, "id = ?", "pilot").Error; err != nil {
		t.Fatalf("cash flow foreign key blocked account deletion: %v", err)
	}
	var count int64
	if err := db.db.Model(&futuresCashFlow{}).Count(&count).Error; err != nil || count != 0 {
		t.Fatalf("account deletion retained cash flows: %d, %v", count, err)
	}
}

func TestFuturesDocumentedStatusesAndUnchangedEvidence(t *testing.T) {
	db, _, _ := integrationStore(t)
	saveTestAccount(t, db, "pilot")
	ctx := context.Background()
	row := futuresLedgerFixture(t, db, "ask")
	contract := futuresContractFixture(row, "open")
	if err := db.ObserveFuturesContract(ctx, "pilot", row.IntentID, contract); err != nil {
		t.Fatal(err)
	}
	before, _ := db.FuturesIntents(ctx, "pilot")
	if err := db.ObserveFuturesContract(ctx, "pilot", row.IntentID, contract); err != nil {
		t.Fatal(err)
	}
	after, _ := db.FuturesIntents(ctx, "pilot")
	if !before[0].UpdatedAt.Equal(after[0].UpdatedAt) {
		t.Fatal("unchanged public evidence rewrote permanent state")
	}
	for _, status := range []string{"open", "active", "settled", "defaulted", "cancelled"} {
		contract.Status = status
		if err := db.ObserveFuturesContract(ctx, "pilot", row.IntentID, contract); err != nil {
			t.Fatalf("documented status %q rejected: %v", status, err)
		}
	}
	contract.Status = "expired"
	if err := db.ObserveFuturesContract(ctx, "pilot", row.IntentID, contract); !errors.Is(err, ErrInvalidInput) {
		t.Fatalf("undocumented lifecycle status was accepted: %v", err)
	}
}

func TestFuturesDefaultReserveRequiresOwnCreditOrRetainsUnpaidClaim(t *testing.T) {
	for _, kind := range []string{
		"own zero", "own claim", "large claim", "old zero", "missing credit", "missing claim field",
		"negative claim", "overflow claim", "foreign zero", "net zero cash", "margin debit",
		"penalty credit", "wrong futures penalty", "penalty debit",
	} {
		t.Run(kind, func(t *testing.T) {
			db, _, _ := integrationStore(t)
			saveTestAccount(t, db, "pilot")
			ctx := context.Background()
			row := futuresLedgerFixture(t, db, "bid")
			contract := futuresContractFixture(row, "defaulted")
			if err := db.ObserveFuturesContract(ctx, "pilot", row.IntentID, contract); err != nil {
				t.Fatal(err)
			}
			receipt := gameapi.SettleFuturesResult{FuturesID: contract.ID, Status: "defaulted", PenaltyPaidPence: 575, ClaimPence: 125}
			event, _ := json.Marshal(map[string]any{"timestamp": contract.UpdatedAt, "payload": receipt})
			if err := db.RecordFuturesOutcome(ctx, "pilot", row.IntentID, "defaulted", event); err != nil {
				t.Fatal(err)
			}
			want := int64(2362)
			switch kind {
			case "net zero cash", "margin debit", "penalty credit", "wrong futures penalty", "penalty debit":
				delta, reason, id := int64(-575), "futures_default_penalty", contract.ID
				switch kind {
				case "net zero cash":
					delta = 0
				case "margin debit":
					reason = "futures_margin"
				case "penalty credit":
					delta = 575
				case "wrong futures penalty":
					id = "other-futures"
				case "penalty debit":
					want = 125
				}
				evidence, _ := json.Marshal(map[string]any{"reason": reason, "metadata": map[string]string{"futures_id": id}})
				if err := db.RecordFuturesCashFlow(ctx, "pilot", row.IntentID, "default-cash", delta, evidence); err != nil {
					t.Fatal(err)
				}
			case "missing credit":
			default:
				if reserved, _, err := db.ReserveFuturesCreditRead(ctx, "pilot", time.Now().UTC()); err != nil || !reserved {
					t.Fatalf("default did not reserve an own-credit read: %t %v", reserved, err)
				}
				claims, company := int64(0), "company-pilot"
				switch kind {
				case "own zero":
					want = 0
				case "own claim":
					claims, want = 125, 125
				case "large claim":
					claims, want = 5000, 5000
				case "negative claim":
					claims = -1
				case "overflow claim":
					claims = 1_000_000_000_001
				case "foreign zero":
					company = "other-company"
				}
				report := map[string]any{"company_id": company, "futures_claims_open_pence": claims}
				if kind == "missing claim field" {
					delete(report, "futures_claims_open_pence")
				}
				raw, _ := json.Marshal(report)
				err := db.RecordFuturesCredit(ctx, "pilot", raw)
				invalid := kind == "missing claim field" || kind == "negative claim" || kind == "overflow claim" || kind == "foreign zero"
				if invalid && !errors.Is(err, ErrInvalidInput) || !invalid && err != nil {
					t.Fatalf("own-credit validation: %v", err)
				}
				if kind == "old zero" {
					if err := db.db.Model(&futuresCreditEvidence{}).Where("account_id = ?", "pilot").Update("observed_at", contract.UpdatedAt.Add(-time.Second)).Error; err != nil {
						t.Fatal(err)
					}
				}
			}
			if reserve, err := db.FuturesCashReserve(ctx, "pilot"); err != nil || reserve != want {
				t.Fatalf("default claim reserve: got=%d want=%d err=%v", reserve, want, err)
			}
		})
	}
}

func TestFuturesOwnCreditAllowanceAndPositiveClaimsSurviveFailureAndAge(t *testing.T) {
	db, _, _ := integrationStore(t)
	saveTestAccount(t, db, "pilot")
	ctx := context.Background()
	if reserved, awaited, err := db.ReserveFuturesCreditRead(ctx, "pilot", time.Now().UTC()); err != nil || reserved || awaited {
		t.Fatalf("ordinary fleet reserved an unnecessary credit read: %t %v", reserved, err)
	}
	row := futuresLedgerFixture(t, db, "bid")
	if err := db.ObserveFuturesContract(ctx, "pilot", row.IntentID, futuresContractFixture(row, "defaulted")); err != nil {
		t.Fatal(err)
	}
	if reserved, _, err := db.ReserveFuturesCreditRead(ctx, "pilot", time.Now().UTC()); err != nil || !reserved {
		t.Fatalf("default credit allowance: %t %v", reserved, err)
	}
	if err := db.RecordFuturesCredit(ctx, "pilot", json.RawMessage(`{"company_id":"company-pilot","futures_claims_open_pence":5000}`)); err != nil {
		t.Fatal(err)
	}
	if reserved, awaited, err := db.ReserveFuturesCreditRead(ctx, "pilot", time.Now().UTC()); err != nil || reserved || !awaited {
		t.Fatalf("five-real-minute allowance was not persisted: %t %v", reserved, err)
	}
	// Positive evidence remains authoritative after it ages; absence of a new
	// successful report cannot turn an outstanding claim into spendable cash.
	old := time.Now().UTC().Add(-24 * time.Hour)
	contract := futuresContractFixture(row, "defaulted")
	contract.UpdatedAt = old.Add(-time.Hour)
	raw, _ := json.Marshal(contract)
	if err := db.db.Model(&FuturesIntent{}).Where("account_id = ? AND intent_id = ?", "pilot", row.IntentID).Update("contract", json.RawMessage(raw)).Error; err != nil {
		t.Fatal(err)
	}
	if err := db.db.Model(&futuresCreditEvidence{}).Where("account_id = ?", "pilot").
		Updates(map[string]any{"observed_at": old, "last_attempt_at": old}).Error; err != nil {
		t.Fatal(err)
	}
	if reserve, err := db.FuturesCashReserve(ctx, "pilot"); err != nil || reserve != 5000 {
		t.Fatalf("aged positive claim became available cash: %d %v", reserve, err)
	}
	if reserved, _, err := db.ReserveFuturesCreditRead(ctx, "pilot", time.Now().UTC()); err != nil || !reserved {
		t.Fatalf("aged positive claim did not permit a bounded refresh: %t %v", reserved, err)
	}
	if err := db.RecordFuturesCredit(ctx, "pilot", json.RawMessage(`{"company_id":"other","futures_claims_open_pence":0}`)); !errors.Is(err, ErrInvalidInput) {
		t.Fatalf("failed refresh accepted a foreign zero: %v", err)
	}
	if reserve, err := db.FuturesCashReserve(ctx, "pilot"); err != nil || reserve != 5000 {
		t.Fatalf("failed refresh forgot the last positive claim: %d %v", reserve, err)
	}
	if reserved, awaited, err := db.ReserveFuturesCreditRead(ctx, "pilot", time.Now().UTC()); err != nil || reserved || !awaited {
		t.Fatalf("failed request refunded the durable allowance: %t %v", reserved, err)
	}
	if err := db.RecordFuturesCredit(ctx, "pilot", json.RawMessage(`{"company_id":"company-pilot","futures_claims_open_pence":125}`)); err != nil {
		t.Fatal(err)
	}
	if reserve, err := db.FuturesCashReserve(ctx, "pilot"); err != nil || reserve != 125 {
		t.Fatalf("later validated reduction was not applied: %d %v", reserve, err)
	}
	if err := db.RecordFuturesCredit(ctx, "pilot", json.RawMessage(`{"company_id":"company-pilot","futures_claims_open_pence":0}`)); err != nil {
		t.Fatal(err)
	}
	if reserve, err := db.FuturesCashReserve(ctx, "pilot"); err != nil || reserve != 0 {
		t.Fatalf("post-default validated zero did not clear liability: %d %v", reserve, err)
	}
	if err := db.db.Model(&futuresCreditEvidence{}).Where("account_id = ?", "pilot").Update("last_attempt_at", old).Error; err != nil {
		t.Fatal(err)
	}
	if reserved, awaited, err := db.ReserveFuturesCreditRead(ctx, "pilot", time.Now().UTC()); err != nil || reserved || awaited {
		t.Fatalf("resolved default continued polling own credit: %t %v", reserved, err)
	}
}

func TestFuturesOwnCreditSkewCannotReleaseLaterDefault(t *testing.T) {
	db, _, _ := integrationStore(t)
	saveTestAccount(t, db, "pilot")
	ctx := context.Background()
	row := futuresLedgerFixture(t, db, "bid")
	serverLower := time.Now().UTC().Add(-2 * time.Hour).Truncate(time.Microsecond)
	contract := futuresContractFixture(row, "defaulted")
	contract.UpdatedAt = serverLower.Add(-time.Minute)
	if err := db.ObserveFuturesContract(ctx, "pilot", row.IntentID, contract); err != nil {
		t.Fatal(err)
	}
	if reserved, _, err := db.ReserveFuturesCreditRead(ctx, "pilot", serverLower); err != nil || !reserved {
		t.Fatalf("skewed server capture was not reserved: %t %v", reserved, err)
	}
	if err := db.RecordFuturesCredit(ctx, "pilot", json.RawMessage(`{"company_id":"company-pilot","futures_claims_open_pence":0}`)); err != nil {
		t.Fatal(err)
	}
	var credit futuresCreditEvidence
	if err := db.db.Where("account_id = ?", "pilot").Take(&credit).Error; err != nil || credit.ObservedAt == nil || !credit.ObservedAt.Equal(serverLower) {
		t.Fatalf("capture timestamp used local time instead of the server lower bound: %+v %v", credit, err)
	}
	if reserve, err := db.FuturesCashReserve(ctx, "pilot"); err != nil || reserve != 0 {
		t.Fatalf("validated zero after the first default did not clear debt: %d %v", reserve, err)
	}
	contract.UpdatedAt = serverLower.Add(time.Minute)
	if err := db.ObserveFuturesContract(ctx, "pilot", row.IntentID, contract); err != nil {
		t.Fatal(err)
	}
	if reserve, err := db.FuturesCashReserve(ctx, "pilot"); err != nil || reserve != 2362 {
		t.Fatalf("host clock ahead let an earlier zero release a later server default: %d %v", reserve, err)
	}
}

func TestFuturesOwnCreditWithoutUnresolvedDefaultsDoesNotLockAccount(t *testing.T) {
	for _, resolved := range []bool{false, true} {
		t.Run(map[bool]string{false: "no defaults", true: "resolved default"}[resolved], func(t *testing.T) {
			db, _, _ := integrationStore(t)
			saveTestAccount(t, db, "pilot")
			ctx := context.Background()
			if resolved {
				row := futuresLedgerFixture(t, db, "bid")
				if err := db.ObserveFuturesContract(ctx, "pilot", row.IntentID, futuresContractFixture(row, "defaulted")); err != nil {
					t.Fatal(err)
				}
				if reserved, _, err := db.ReserveFuturesCreditRead(ctx, "pilot", time.Now().UTC()); err != nil || !reserved {
					t.Fatalf("default observation was not reserved: %t %v", reserved, err)
				}
				if err := db.RecordFuturesCredit(ctx, "pilot", json.RawMessage(`{"company_id":"company-pilot","futures_claims_open_pence":0}`)); err != nil {
					t.Fatal(err)
				}
			}
			tx := db.db.Begin()
			if tx.Error != nil {
				t.Fatal(tx.Error)
			}
			defer tx.Rollback()
			var id string
			if err := tx.Raw("SELECT id FROM accounts WHERE id = ? FOR UPDATE", "pilot").Scan(&id).Error; err != nil {
				t.Fatal(err)
			}
			bounded, cancel := context.WithTimeout(ctx, time.Second)
			defer cancel()
			if reserved, awaited, err := db.ReserveFuturesCreditRead(bounded, "pilot", time.Time{}); err != nil || reserved || awaited {
				t.Fatalf("optional no-op waited for the account write lock: %t %v", reserved, err)
			}
			var count int64
			want := int64(0)
			if resolved {
				want = 1
			}
			if err := db.db.Model(&futuresCreditEvidence{}).Count(&count).Error; err != nil || count != want {
				t.Fatalf("optional no-op created a reservation row: %d %v", count, err)
			}
		})
	}
}
