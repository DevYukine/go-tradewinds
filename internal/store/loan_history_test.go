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

	"github.com/DevYukine/go-tradewinds/internal/gameapi"
	"go.uber.org/zap"
)

func loanHistoryJSON(t *testing.T, loan gameapi.Loan) json.RawMessage {
	t.Helper()
	raw, err := json.Marshal(loan)
	if err != nil {
		t.Fatal(err)
	}
	return raw
}

func TestLoanHistoryRoundTripRestartRetainsClosedAndRejectsOlderRows(t *testing.T) {
	db, dsn, key := integrationStore(t)
	ctx := context.Background()
	const accountID = "loan-history"
	credentials := saveTestAccount(t, db, accountID)
	saveTestAccount(t, db, "loan-other")
	at := time.Now().UTC().Add(-24 * time.Hour).Truncate(time.Microsecond)
	loan := loanHistoryFixture("same-loan-id", accountID, at)
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(loanHistoryJSON(t, loan), &fields); err != nil {
		t.Fatal(err)
	}
	fields["extension"] = json.RawMessage(`{"exact_pence":9007199254740993,"small_exponent":1e32,"nested":["preserved"]}`)
	fields["api_key"] = json.RawMessage(`"` + credentials.APIKey + `"`)
	raw, err := json.Marshal(fields)
	if err != nil {
		t.Fatal(err)
	}
	if err := db.SyncLoanHistory(ctx, accountID, []json.RawMessage{raw}, at.Add(time.Hour)); err != nil {
		t.Fatal(err)
	}
	var original loanHistoryRow
	if err := db.db.First(&original, "account_id = ? AND loan_id = ?", accountID, loan.ID).Error; err != nil {
		t.Fatal(err)
	}
	if bytes.Contains(original.Data, []byte(credentials.APIKey)) || !bytes.Contains(original.Data, []byte(`9007199254740993`)) || !bytes.Contains(original.Data, []byte(`"preserved"`)) {
		t.Fatalf("raw extensions lost precision or secret redaction: %s", original.Data)
	}
	other := loanHistoryFixture(loan.ID, "loan-other", at)
	if err := db.SyncLoanHistory(ctx, "loan-other", []json.RawMessage{loanHistoryJSON(t, other)}, at.Add(time.Hour)); err != nil {
		t.Fatal(err)
	}
	closed := loan
	closed.Status, closed.OwedPence, closed.OutstandingPrincipalPence = "repaid", 0, 0
	closed.PrincipalRepaidPence, closed.InterestPaidPence = 30_000, 300
	closed.UpdatedAt = at.Add(2 * time.Hour)
	closed.ClosedAt = &closed.UpdatedAt
	if err := db.SyncLoanHistory(ctx, accountID, []json.RawMessage{loanHistoryJSON(t, closed)}, at.Add(3*time.Hour)); err != nil {
		t.Fatal(err)
	}
	for _, stale := range []struct {
		loan     gameapi.Loan
		observed time.Time
	}{
		{loan, at.Add(5 * time.Hour)},   // Later receipt of older source state.
		{closed, at.Add(2 * time.Hour)}, // Same source state, older observation.
		{func() gameapi.Loan {
			row := loan
			row.UpdatedAt = time.Time{}
			return row
		}(), at.Add(6 * time.Hour)},
	} {
		stale.loan.PrincipalRepaidPence = 1
		if err := db.SyncLoanHistory(ctx, accountID, []json.RawMessage{loanHistoryJSON(t, stale.loan)}, stale.observed); err != nil {
			t.Fatal(err)
		}
	}
	active := loanHistoryFixture("still-active", accountID, at)
	if err := db.SyncLoanHistory(ctx, accountID, []json.RawMessage{loanHistoryJSON(t, active)}, at.Add(4*time.Hour)); err != nil {
		t.Fatal(err)
	}
	if err := db.SyncLoanHistory(ctx, accountID, nil, at.Add(5*time.Hour)); err != nil {
		t.Fatal(err)
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}
	restarted, err := Open(ctx, dsn, key, zap.NewNop())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = restarted.Close() })
	report, err := restarted.Loans(ctx, accountID)
	if err != nil || len(report.Accounts) != 1 {
		t.Fatalf("restart report: %+v, %v", report, err)
	}
	result := report.Accounts[0]
	if len(result.ActiveLoans) != 1 || len(result.ClosedLoans) != 1 || result.ClosedLoans[0].Status != "repaid" ||
		result.ClosedLoans[0].PrincipalRepaidPence != 30_000 || result.Totals.RepaidCount != 1 || result.HistoryComplete || !result.LoansStale {
		t.Fatalf("closed row regressed, vanished or claimed unobserved completeness: %+v", result)
	}
	var persisted loanHistoryRow
	if err := restarted.db.First(&persisted, "account_id = ? AND loan_id = ?", accountID, loan.ID).Error; err != nil {
		t.Fatal(err)
	}
	if persisted.ClosedAt == nil || !persisted.ClosedAt.Equal(*closed.ClosedAt) || persisted.SourceUpdatedAt == nil ||
		!persisted.SourceUpdatedAt.Equal(closed.UpdatedAt) || !persisted.ObservedAt.Equal(at.Add(3*time.Hour)) {
		t.Fatalf("stored source/observation/closure guard failed: %+v", persisted)
	}
	if err := restarted.db.Delete(&Account{}, "id = ?", accountID).Error; err != nil {
		t.Fatal(err)
	}
	var deleted, retained int64
	if err := restarted.db.Model(&loanHistoryRow{}).Where("account_id = ?", accountID).Count(&deleted).Error; err != nil {
		t.Fatal(err)
	}
	if err := restarted.db.Model(&loanHistoryRow{}).Where("account_id = ?", "loan-other").Count(&retained).Error; err != nil {
		t.Fatal(err)
	}
	if deleted != 0 || retained != 1 {
		t.Fatalf("account cascade crossed its boundary: deleted account rows=%d other=%d", deleted, retained)
	}
}

func TestLoanHistoryRejectsInvalidRawRowsBeforeDatabaseAccess(t *testing.T) {
	at := time.Date(2026, 11, 3, 12, 0, 0, 0, time.UTC)
	valid := loanHistoryJSON(t, loanHistoryFixture("valid", "one", at))
	padded := append(bytes.Clone(valid), bytes.Repeat([]byte(" "), MaxSnapshotBytes/2)...)
	for _, test := range []struct {
		name     string
		account  string
		rows     []json.RawMessage
		observed time.Time
		want     error
	}{
		{"missing account", "", nil, at, ErrInvalidInput},
		{"blank account", "  ", nil, at, ErrInvalidInput},
		{"oversized account", strings.Repeat("a", 256), nil, at, ErrInvalidInput},
		{"missing observation", "one", nil, time.Time{}, ErrInvalidInput},
		{"not JSON", "one", []json.RawMessage{json.RawMessage(`{"id":`)}, at, ErrInvalidInput},
		{"null row", "one", []json.RawMessage{json.RawMessage(`null`)}, at, ErrInvalidInput},
		{"array row", "one", []json.RawMessage{json.RawMessage(`[]`)}, at, ErrInvalidInput},
		{"duplicate identity", "one", []json.RawMessage{valid, valid}, at, ErrInvalidInput},
		{"blank borrower identity", "one", []json.RawMessage{json.RawMessage(`{"id":"valid","borrower_company_id":" ","status":"active"}`)}, at, ErrInvalidInput},
		{"malformed borrower type", "one", []json.RawMessage{json.RawMessage(`{"id":"valid","borrower_company_id":42,"status":"active"}`)}, at, ErrInvalidInput},
		{"negative principal repayment", "one", []json.RawMessage{json.RawMessage(`{"id":"valid","borrower_company_id":"company-one","status":"repaid","principal_repaid_pence":-1}`)}, at, ErrInvalidInput},
		{"negative interest payment", "one", []json.RawMessage{json.RawMessage(`{"id":"valid","borrower_company_id":"company-one","status":"repaid","interest_paid_pence":-1}`)}, at, ErrInvalidInput},
		{"aggregate input bound", "one", []json.RawMessage{padded, padded}, at, ErrResponseTooLarge},
		{"unknown numeric extension expansion", "one", []json.RawMessage{json.RawMessage(`{"id":"valid","borrower_company_id":"company-one","status":"active","extension":1e130000}`)}, at, ErrResponseTooLarge},
	} {
		t.Run(test.name, func(t *testing.T) {
			err := (*Store)(nil).SyncLoanHistory(context.Background(), test.account, test.rows, test.observed)
			if !errors.Is(err, test.want) {
				t.Fatalf("invalid observation accepted: %v; want %v", err, test.want)
			}
		})
	}
}

func TestLoanHistoryRejectsWrongBorrowerWithoutPartialWrite(t *testing.T) {
	db, _, _ := integrationStore(t)
	ctx := context.Background()
	saveTestAccount(t, db, "borrower")
	at := time.Now().UTC()
	valid := loanHistoryFixture("valid", "borrower", at)
	wrong := loanHistoryFixture("wrong", "other", at)
	if err := db.SyncLoanHistory(ctx, "borrower", []json.RawMessage{loanHistoryJSON(t, valid), loanHistoryJSON(t, wrong)}, at); !errors.Is(err, ErrInvalidInput) {
		t.Fatalf("cross-account loan accepted: %v", err)
	}
	var count int64
	if err := db.db.Model(&loanHistoryRow{}).Count(&count).Error; err != nil || count != 0 {
		t.Fatalf("invalid batch partially committed: %d, %v", count, err)
	}
}

func TestLoanHistoryRejectsPrivateIdentityWithoutWriting(t *testing.T) {
	db, _, _ := integrationStore(t)
	credentials := saveTestAccount(t, db, "private")
	at := time.Now().UTC()
	loan := loanHistoryFixture(credentials.APIKey, "private", at)
	if err := db.SyncLoanHistory(context.Background(), "private", []json.RawMessage{loanHistoryJSON(t, loan)}, at); !errors.Is(err, ErrInvalidInput) {
		t.Fatalf("credential-bearing identity accepted: %v", err)
	}
	var count int64
	if err := db.db.Model(&loanHistoryRow{}).Count(&count).Error; err != nil || count != 0 {
		t.Fatalf("private identity persisted: %d, %v", count, err)
	}
}

func loanHistoryExtensionJSON(t *testing.T, loan gameapi.Loan, extension string) json.RawMessage {
	t.Helper()
	raw := loanHistoryJSON(t, loan)
	return json.RawMessage(string(raw[:len(raw)-1]) + `,"extension":` + extension + `}`)
}

func loanHistoryCanonicalJSON(t *testing.T, raw json.RawMessage) json.RawMessage {
	t.Helper()
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.UseNumber()
	var value any
	if err := decoder.Decode(&value); err != nil {
		t.Fatal(err)
	}
	canonical, err := json.Marshal(value)
	if err != nil {
		t.Fatal(err)
	}
	return canonical
}

func TestLoanHistoryRetainsMinimalRowsWithoutBorrower(t *testing.T) {
	db, _, _ := integrationStore(t)
	ctx := context.Background()
	const accountID = "loan-minimal"
	saveTestAccount(t, db, accountID)
	at := time.Now().UTC().Truncate(time.Microsecond)
	raw := json.RawMessage(`{"id":"legacy-minimal","status":"active","extension":{"preserved":true}}`)
	if err := db.SyncLoanHistory(ctx, accountID, []json.RawMessage{raw}, at); err != nil {
		t.Fatalf("report-only legacy row was rejected: %v", err)
	}
	var stored loanHistoryRow
	if err := db.db.First(&stored, "account_id = ? AND loan_id = ?", accountID, "legacy-minimal").Error; err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(loanHistoryCanonicalJSON(t, stored.Data), loanHistoryCanonicalJSON(t, raw)) {
		t.Fatalf("minimal row extensions or omitted borrower changed: %s", stored.Data)
	}
	report, err := db.Loans(ctx, accountID)
	if err != nil || len(report.Accounts) != 1 || len(report.Accounts[0].ActiveLoans) != 1 ||
		report.Accounts[0].ActiveLoans[0].ID != "legacy-minimal" {
		t.Fatalf("retained minimal row was not readable in its account: %+v, %v", report, err)
	}
}

func TestLoanHistoryRejectsExpandedRowsAndBatchesWithoutOverwrite(t *testing.T) {
	db, _, _ := integrationStore(t)
	ctx := context.Background()
	expandedRow := `[` + strings.Repeat("1e32,", MaxSnapshotBytes/35) + `1e32]`
	expandedHalf := `[` + strings.Repeat("1e32,", MaxSnapshotBytes/(2*35)) + `1e32]`
	escapedRow := `{"values":[` + strings.Repeat("1e32,", MaxSnapshotBytes/(2*34)) +
		`1e32],"literal":"` + strings.Repeat("<", MaxSnapshotBytes/12+1) + `"}`
	for _, fixture := range []struct {
		name       string
		extensions []string
	}{
		{"single-expanded-row", []string{expandedRow}},
		{"expanded-batch", []string{expandedHalf, expandedHalf}},
		{"canonical-escaping", []string{escapedRow}},
	} {
		t.Run(fixture.name, func(t *testing.T) {
			accountID := "loan-bound-" + fixture.name
			saveTestAccount(t, db, accountID)
			at := time.Now().UTC().Truncate(time.Microsecond)
			loan := loanHistoryFixture("retained", accountID, at)
			if err := db.SyncLoanHistory(ctx, accountID, []json.RawMessage{loanHistoryJSON(t, loan)}, at); err != nil {
				t.Fatal(err)
			}
			var original loanHistoryRow
			if err := db.db.First(&original, "account_id = ? AND loan_id = ?", accountID, loan.ID).Error; err != nil {
				t.Fatal(err)
			}
			loan.PrincipalRepaidPence = 1
			loan.UpdatedAt = at.Add(time.Hour)
			rows := []json.RawMessage{loanHistoryJSON(t, loan)}
			inputBytes := len(rows[0])
			for index, extension := range fixture.extensions {
				row := loanHistoryExtensionJSON(t, loanHistoryFixture(fmt.Sprintf("overflow-%d", index), accountID, at), extension)
				if err := ValidateSnapshotRouteNumbers(row); err != nil {
					t.Fatalf("fixture must fit the route numeric policy: %v", err)
				}
				inputBytes += len(row)
				rows = append(rows, row)
			}
			if inputBytes > MaxSnapshotBytes {
				t.Fatalf("compact fixture already exceeded the input cap: %d", inputBytes)
			}
			if err := db.SyncLoanHistory(ctx, accountID, rows, at.Add(time.Hour)); !errors.Is(err, ErrResponseTooLarge) {
				t.Fatalf("expanded observation was not rejected: %v", err)
			}
			var stored []loanHistoryRow
			if err := db.db.Where("account_id = ?", accountID).Find(&stored).Error; err != nil {
				t.Fatal(err)
			}
			if len(stored) != 1 || stored[0].LoanID != original.LoanID || !bytes.Equal(stored[0].Data, original.Data) ||
				!stored[0].ObservedAt.Equal(original.ObservedAt) {
				t.Fatal("rejected expansion committed a new row or overwrote last-good history")
			}
		})
	}
}

func TestLoanHistoryExpandedAndCanonicalBoundariesRoundTrip(t *testing.T) {
	db, _, _ := integrationStore(t)
	ctx := context.Background()
	for _, fixture := range []struct {
		name    string
		literal string
	}{
		{"postgres", ""},
		{"canonical", strings.Repeat("<>&\u2028\u2029", 20)},
	} {
		t.Run(fixture.name, func(t *testing.T) {
			accountID := "loan-roundtrip-" + fixture.name
			saveTestAccount(t, db, accountID)
			at := time.Now().UTC().Truncate(time.Microsecond)
			loan := loanHistoryFixture("boundary", accountID, at)
			makeRow := func(padding string) json.RawMessage {
				return loanHistoryExtensionJSON(t, loan, `{"amount":1e32,"literal":"`+fixture.literal+`","text":"`+padding+`"}`)
			}
			var base struct{ Data json.RawMessage }
			if err := db.db.Raw("SELECT ?::jsonb AS data", string(makeRow(""))).Scan(&base).Error; err != nil {
				t.Fatal(err)
			}
			canonicalBase := loanHistoryCanonicalJSON(t, base.Data)
			if fixture.name == "postgres" && len(base.Data) <= len(canonicalBase) ||
				fixture.name == "canonical" && len(canonicalBase) <= len(base.Data) {
				t.Fatal("fixture did not distinguish PostgreSQL and canonical escaping")
			}
			raw := makeRow(strings.Repeat("x", MaxSnapshotBytes-max(len(base.Data), len(canonicalBase))))
			if len(raw) >= MaxSnapshotBytes {
				t.Fatalf("compact exponent fixture must fit below the write cap: %d", len(raw))
			}
			if err := db.SyncLoanHistory(ctx, accountID, []json.RawMessage{raw}, at); err != nil {
				t.Fatalf("boundary row was rejected: %v", err)
			}
			var firstCanonical json.RawMessage
			for round := range 2 {
				var stored loanHistoryRow
				if err := db.db.First(&stored, "account_id = ? AND loan_id = ?", accountID, loan.ID).Error; err != nil {
					t.Fatal(err)
				}
				canonical := loanHistoryCanonicalJSON(t, stored.Data)
				if len(stored.Data) > MaxSnapshotBytes || len(canonical) > MaxSnapshotBytes ||
					max(len(stored.Data), len(canonical)) != MaxSnapshotBytes {
					t.Fatalf("round trip missed a storage boundary: postgres=%d canonical=%d", len(stored.Data), len(canonical))
				}
				if err := ValidateSnapshotRouteNumbers(stored.Data); err != nil {
					t.Fatalf("expanded numeric width was not round-trip stable: %v", err)
				}
				var extensions struct {
					Extension struct {
						Amount  json.Number
						Literal string
					}
				}
				if err := json.Unmarshal(stored.Data, &extensions); err != nil {
					t.Fatal(err)
				}
				if extensions.Extension.Amount.String() != "1"+strings.Repeat("0", 32) || extensions.Extension.Literal != fixture.literal {
					t.Fatal("numeric precision or HTML/Unicode extension values changed")
				}
				if firstCanonical == nil {
					firstCanonical = canonical
				} else if !bytes.Equal(firstCanonical, canonical) {
					t.Fatal("canonical history changed after re-saving PostgreSQL-expanded numbers")
				}
				report, err := db.Loans(ctx, accountID)
				if err != nil || len(report.Accounts) != 1 || len(report.Accounts[0].ActiveLoans) != 1 ||
					report.Accounts[0].ActiveLoans[0].ID != loan.ID {
					t.Fatalf("accepted boundary row was not readable: %+v, %v", report, err)
				}
				if round == 0 {
					if err := db.SyncLoanHistory(ctx, accountID, []json.RawMessage{canonical}, at.Add(time.Hour)); err != nil {
						t.Fatalf("canonical readback poisoned the next observation: %v", err)
					}
				}
			}
		})
	}
}

func TestLoanHistoryRejectsRetainedReportOverflowWithoutPartialWrite(t *testing.T) {
	db, _, _ := integrationStore(t)
	ctx := context.Background()
	const accountID = "loan-retained-bound"
	saveTestAccount(t, db, accountID)
	at := time.Now().UTC().Truncate(time.Microsecond)
	for index := range 2 {
		loan := loanHistoryFixture(fmt.Sprintf("retained-%d", index), accountID, at)
		raw := loanHistoryExtensionJSON(t, loan, `{"text":"`+strings.Repeat("x", MaxSnapshotBytes-8192)+`"}`)
		if err := db.SyncLoanHistory(ctx, accountID, []json.RawMessage{raw}, at); err != nil {
			t.Fatalf("individually bounded history batch was rejected: %v", err)
		}
	}
	loan := loanHistoryFixture("overflow", accountID, at)
	raw := loanHistoryExtensionJSON(t, loan, `{"text":"`+strings.Repeat("x", 32*1024)+`"}`)
	if err := db.SyncLoanHistory(ctx, accountID, []json.RawMessage{raw}, at.Add(time.Hour)); !errors.Is(err, ErrResponseTooLarge) {
		t.Fatalf("disjoint history batches poisoned the account reader: %v", err)
	}
	var count int64
	if err := db.db.Model(&loanHistoryRow{}).Where("account_id = ?", accountID).Count(&count).Error; err != nil || count != 2 {
		t.Fatalf("report overflow partially committed: rows=%d err=%v", count, err)
	}
	report, err := db.Loans(ctx, accountID)
	if err != nil || len(report.Accounts) != 1 || len(report.Accounts[0].ActiveLoans) != 2 {
		t.Fatalf("report overflow damaged last-good readable history: %+v, %v", report, err)
	}
}
