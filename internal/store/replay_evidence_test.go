package store

import (
	"bytes"
	"context"
	"crypto/cipher"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"reflect"
	"strings"
	"testing"
	"time"

	"go.uber.org/zap"
	"gorm.io/gorm"
)

func TestReplayEvidenceProjectionKeepsExactSimulatorFields(t *testing.T) {
	for _, test := range []struct {
		name  string
		shape replayEvidenceShape
		input string
		want  string
	}{
		{
			name:  "immutable request quantities and prices",
			shape: replayEvidenceRequest,
			input: `{"commodity_id":"cloth","quantity":9007199254740993,"ship_id":"ship","port_id":"port","side":"bid","price_pence":9223372036854775807,"contract_id":"charter","dest_port_id":"destination","diagnostic":{"private":"discard"},"into_stores":true}`,
			want:  `{"commodity_id":"cloth","quantity":9007199254740993,"ship_id":"ship","port_id":"port","side":"bid","price_pence":9223372036854775807,"contract_id":"charter","dest_port_id":"destination"}`,
		},
		{
			name:  "receipt money presence and nested departure ETA",
			shape: replayEvidenceResponse,
			input: `{"commodity_id":"cloth","filled_quantity":0,"total_gross_pence":9007199254740993,"customs_duty_pence":null,"reward_paid_pence":9223372036854775807,"fee_pence":0,"committed_uninterpreted":true,"contract_id":"freight","collateral_pence":17,"delivered_quantity":3,"estimated_arrival_at":"2026-10-20T01:02:03.123456Z","voyage":{"estimated_arrival_at":"2026-10-20T02:03:04.654321Z","legs":[{"diagnostic":"discard"}]},"detail":"discard"}`,
			want:  `{"commodity_id":"cloth","filled_quantity":0,"total_gross_pence":9007199254740993,"customs_duty_pence":null,"reward_paid_pence":9223372036854775807,"fee_pence":0,"committed_uninterpreted":true,"contract_id":"freight","collateral_pence":17,"delivered_quantity":3,"estimated_arrival_at":"2026-10-20T01:02:03.123456Z","voyage":{"estimated_arrival_at":"2026-10-20T02:03:04.654321Z"}}`,
		},
		{
			name:  "duplicate and invalid typed evidence stays uninterpretable",
			shape: replayEvidenceResponse,
			input: `{"filled_quantity":"invalid","filled_quantity":3,"TOTAL_GROSS_PENCE":5,"customs_duty_pence":0,"diagnostic":true}`,
			want:  `{"filled_quantity":"invalid","filled_quantity":3,"TOTAL_GROSS_PENCE":5,"customs_duty_pence":0}`,
		},
		{name: "no response", shape: replayEvidenceResponse},
		{name: "explicit null", shape: replayEvidenceResponse, input: "null", want: "null"},
		{name: "rejection details", shape: replayEvidenceResponse, input: `{"code":"forbidden","detail":"private diagnostics"}`, want: `{}`},
	} {
		t.Run(test.name, func(t *testing.T) {
			got := projectReplayEvidenceJSON([]byte(test.input), test.shape)
			if string(got) != test.want {
				t.Fatalf("projected evidence = %s; want %s", got, test.want)
			}
		})
	}
	for _, raw := range []string{`Too Many Requests: private-body`, `{"filled_quantity":1} true`, `{"detail":[}`, `[]`, `"not an object"`} {
		got := projectReplayEvidenceJSON([]byte(raw), replayEvidenceResponse)
		var receipt struct {
			Filled int64 `json:"filled_quantity"`
		}
		if string(got) != `"opaque"` || json.Unmarshal(got, &receipt) == nil {
			t.Fatalf("opaque body became usable evidence: %q", got)
		}
	}
}

func TestReplayEvidenceAggregateBudgetBoundaries(t *testing.T) {
	var budget replayEvidenceBudget
	if err := budget.addRows(MaxReplayEvidenceRows, "action history"); err != nil {
		t.Fatal(err)
	}
	if err := budget.addRows(1, "action history"); !errors.Is(err, ErrReplayEvidenceBudget) {
		t.Fatalf("metadata row budget overflow was not identifiable: %v", err)
	}
	if err := budget.retain(MaxReplayEvidenceRetainedBytes, "settlement history"); err != nil {
		t.Fatal(err)
	}
	if err := budget.retain(1, "settlement history"); !errors.Is(err, ErrReplayEvidenceBudget) {
		t.Fatalf("projected byte budget overflow was not identifiable: %v", err)
	}
}

type replayEvidenceOpenCounter struct {
	cipher.AEAD
	opens int
}

func (counter *replayEvidenceOpenCounter) Open(dst, nonce, ciphertext, additionalData []byte) ([]byte, error) {
	counter.opens++
	return counter.AEAD.Open(dst, nonce, ciphertext, additionalData)
}

type replayEvidenceMaterialization struct {
	maxCiphertext int
	maxPageRows   int
	pages         int
}

func observeReplayEvidence(t *testing.T, db *Store) *replayEvidenceMaterialization {
	t.Helper()
	observed := &replayEvidenceMaterialization{}
	const callback = "test:replay_evidence_materialization"
	if err := db.db.Callback().Query().After("gorm:query").Register(callback, func(tx *gorm.DB) {
		var size int
		switch destination := tx.Statement.Dest.(type) {
		case *[]replayEvidenceRowMeta:
			observed.maxPageRows = max(observed.maxPageRows, len(*destination))
			observed.pages++
		case *actionRow:
			size = len(destination.BodyCipher) + len(destination.ResponseCipher)
		case *[]actionRow:
			for _, row := range *destination {
				size += len(row.BodyCipher) + len(row.ResponseCipher)
			}
		case *settlementRow:
			size = len(destination.ContextCipher)
		case *[]settlementRow:
			for _, row := range *destination {
				size += len(row.ContextCipher)
			}
		}
		observed.maxCiphertext = max(observed.maxCiphertext, size)
	}); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := db.db.Callback().Query().Remove(callback); err != nil {
			t.Error(err)
		}
	})
	return observed
}

func replayEvidenceAction(t *testing.T, db *Store, account, id string, at time.Time, body, response []byte) actionRow {
	t.Helper()
	bodyCipher, err := db.encrypt(body, actionAAD(account, id, "body"))
	if err != nil {
		t.Fatal(err)
	}
	row := actionRow{
		ID: id, AccountID: account, CompanyID: "company-" + account,
		Method: "POST", Path: "/v1/market/place-order", BodyCipher: bodyCipher,
		State: ActionPending, CreatedAt: at, UpdatedAt: at,
	}
	if response != nil {
		row.ResponseCipher, err = db.encrypt(response, actionAAD(account, id, "response"))
		if err != nil {
			t.Fatal(err)
		}
		row.State, row.Status = ActionComplete, 200
	}
	return row
}

func replayEvidenceReadOnly(t *testing.T, dsn string, key []byte) (*Store, *replayEvidenceOpenCounter) {
	t.Helper()
	db, err := OpenReadOnly(context.Background(), dsn, key, zap.NewNop())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := db.Close(); err != nil {
			t.Error(err)
		}
	})
	counter := &replayEvidenceOpenCounter{AEAD: db.aead}
	db.aead = counter
	return db, counter
}

func TestReplayEvidenceRejectsOversizedCiphertextBeforeFetchOrAuthentication(t *testing.T) {
	db, dsn, key := integrationStore(t)
	ctx := context.Background()
	from := time.Date(2026, 10, 20, 0, 0, 0, 0, time.UTC)
	for _, field := range []string{"request", "response", "settlement"} {
		t.Run(field, func(t *testing.T) {
			account := "oversized-" + field
			saveTestAccount(t, db, account)
			body, response := []byte(`{}`), []byte(`{}`)
			large := []byte(fmt.Sprintf(`{"diagnostic":%q}`, strings.Repeat("sensitive-evidence-", MaxReplayEvidenceCiphertextBytes/8)))
			if field == "request" {
				body = large
			} else if field == "response" {
				response = large
			}
			action := replayEvidenceAction(t, db, account, account+"-action", from, body, response)
			if err := db.db.Create(&action).Error; err != nil {
				t.Fatal(err)
			}
			var private settlementRow
			if field == "settlement" {
				var err error
				private, err = db.encodeSettlement(Settlement{
					ID: account, AccountID: account, MissionID: account, ActionID: action.ID,
					ShipID: "ship", Lane: "lane", Kind: "spot", CostsKnown: true,
					StartedAt: from, CompletedAt: from, CreatedAt: from,
				})
				if err != nil {
					t.Fatal(err)
				}
				private.ContextCipher, err = db.encryptPrivate(json.RawMessage(large), "settlements", "context", account, private.ID)
				if err != nil {
					t.Fatal(err)
				}
				if err := db.db.Create(&private).Error; err != nil {
					t.Fatal(err)
				}
			}
			readOnly, authentication := replayEvidenceReadOnly(t, dsn, key)
			observed := observeReplayEvidence(t, readOnly)
			var err error
			if field == "settlement" {
				var rows []Settlement
				rows, err = readOnly.SettlementsBetween(ctx, account, from, from.Add(time.Hour))
				if rows != nil {
					t.Fatal("oversized settlement returned partial or truncated success")
				}
			} else {
				var rows []Action
				rows, err = readOnly.ActionsBetween(ctx, account, from, from.Add(time.Hour))
				if rows != nil {
					t.Fatal("oversized action returned partial or truncated success")
				}
			}
			if !errors.Is(err, ErrReplayEvidenceBudget) || strings.Contains(err.Error(), "sensitive-evidence") {
				t.Fatalf("oversized evidence did not fail with a safe budget error: %v", err)
			}
			if authentication.opens != 0 || observed.maxCiphertext != 0 {
				t.Fatalf("oversized ciphertext was fetched/authenticated: opens=%d materialized=%d", authentication.opens, observed.maxCiphertext)
			}
			var unchanged actionRow
			if err := db.db.Take(&unchanged, "id = ?", action.ID).Error; err != nil {
				t.Fatal(err)
			}
			if !bytes.Equal(action.BodyCipher, unchanged.BodyCipher) || !bytes.Equal(action.ResponseCipher, unchanged.ResponseCipher) {
				t.Fatal("read-only rejection modified durable action ciphertext")
			}
			if field == "settlement" {
				var unchanged settlementRow
				if err := db.db.Take(&unchanged, "id = ?", private.ID).Error; err != nil {
					t.Fatal(err)
				}
				if !bytes.Equal(private.ContextCipher, unchanged.ContextCipher) {
					t.Fatal("read-only rejection modified durable settlement ciphertext")
				}
			}
		})
	}
}

func TestReplayEvidenceAuthenticatesBoundedRowsWithoutPartialResults(t *testing.T) {
	db, dsn, key := integrationStore(t)
	const account = "evidence-authentication"
	saveTestAccount(t, db, account)
	from := time.Date(2026, 10, 20, 0, 0, 0, 0, time.UTC)
	for _, id := range []string{"a-valid", "b-corrupt"} {
		action := replayEvidenceAction(t, db, account, id, from, []byte(`{"quantity":1}`), []byte(`{"filled_quantity":1}`))
		if id == "b-corrupt" {
			action.ResponseCipher[len(action.ResponseCipher)-1] ^= 1
		}
		if err := db.db.Create(&action).Error; err != nil {
			t.Fatal(err)
		}
		private, err := db.encodeSettlement(Settlement{
			ID: id, AccountID: account, MissionID: id, ActionID: id,
			Lane: "lane", Kind: "spot", CostsKnown: true,
			StartedAt: from, CompletedAt: from, CreatedAt: from,
		})
		if err != nil {
			t.Fatal(err)
		}
		if id == "b-corrupt" {
			private.ContextCipher[len(private.ContextCipher)-1] ^= 1
		}
		if err := db.db.Create(&private).Error; err != nil {
			t.Fatal(err)
		}
	}
	readOnly, authentication := replayEvidenceReadOnly(t, dsn, key)
	if rows, err := readOnly.ActionsBetween(context.Background(), account, from, from.Add(time.Hour)); err == nil || errors.Is(err, ErrReplayEvidenceBudget) || rows != nil {
		t.Fatalf("corrupt bounded action did not fail authentication without partial evidence: rows=%d err=%v", len(rows), err)
	}
	if rows, err := readOnly.SettlementsBetween(context.Background(), account, from, from.Add(time.Hour)); err == nil || errors.Is(err, ErrReplayEvidenceBudget) || rows != nil {
		t.Fatalf("corrupt bounded settlement did not fail authentication without partial evidence: rows=%d err=%v", len(rows), err)
	}
	if authentication.opens != 6 {
		t.Fatalf("bounded rows bypassed authentication: %d opens, want 6", authentication.opens)
	}
}

func TestReplayEvidenceAggregateOverflowReturnsNoTruncatedHistory(t *testing.T) {
	db, dsn, key := integrationStore(t)
	from := time.Date(2026, 10, 20, 0, 0, 0, 0, time.UTC)
	const fieldBytes = 64 << 10
	count := MaxReplayEvidenceRetainedBytes/fieldBytes + 2
	for _, kind := range []string{"actions", "settlements"} {
		t.Run(kind, func(t *testing.T) {
			account := "aggregate-" + kind
			saveTestAccount(t, db, account)
			identity := strings.Repeat("retained-", fieldBytes/len("retained-"))
			actions := make([]actionRow, 0, count)
			settlements := make([]settlementRow, 0, count)
			for i := range count {
				id := fmt.Sprintf("%s-%04d", kind, i)
				body := []byte(`{}`)
				if kind == "actions" {
					body = []byte(fmt.Sprintf(`{"ship_id":%q,"quantity":9007199254740993}`, identity))
				}
				action := replayEvidenceAction(t, db, account, id, from, body, []byte(`{}`))
				actions = append(actions, action)
				if kind == "settlements" {
					private, err := db.encodeSettlement(Settlement{
						ID: id, AccountID: account, MissionID: id, ActionID: id,
						Lane: identity, Kind: "spot", CostsKnown: true,
						StartedAt: from, CompletedAt: from, CreatedAt: from,
					})
					if err != nil {
						t.Fatal(err)
					}
					settlements = append(settlements, private)
				}
			}
			if err := db.db.CreateInBatches(&actions, 100).Error; err != nil {
				t.Fatal(err)
			}
			if len(settlements) != 0 {
				if err := db.db.CreateInBatches(&settlements, 100).Error; err != nil {
					t.Fatal(err)
				}
			}
			readOnly, _ := replayEvidenceReadOnly(t, dsn, key)
			observed := observeReplayEvidence(t, readOnly)
			if kind == "actions" {
				if rows, err := readOnly.ActionsBetween(context.Background(), account, from, from.Add(time.Hour)); !errors.Is(err, ErrReplayEvidenceBudget) || rows != nil {
					t.Fatalf("aggregate action overflow returned truncated evidence: rows=%d err=%v", len(rows), err)
				}
			} else if rows, err := readOnly.SettlementsBetween(context.Background(), account, from, from.Add(time.Hour)); !errors.Is(err, ErrReplayEvidenceBudget) || rows != nil {
				t.Fatalf("aggregate settlement overflow returned truncated evidence: rows=%d err=%v", len(rows), err)
			}
			if observed.maxCiphertext > MaxReplayEvidenceCiphertextBytes || observed.maxPageRows > replayEvidencePageSize || observed.pages < 2 {
				t.Fatalf("evidence was not read as bounded rows/pages: %+v", observed)
			}
			var stored actionRow
			if err := db.db.Take(&stored, "id = ?", actions[0].ID).Error; err != nil {
				t.Fatal(err)
			}
			if !bytes.Equal(stored.BodyCipher, actions[0].BodyCipher) || !bytes.Equal(stored.ResponseCipher, actions[0].ResponseCipher) {
				t.Fatal("aggregate failure changed durable action ciphertext")
			}
			if len(settlements) != 0 {
				var stored settlementRow
				if err := db.db.Take(&stored, "id = ?", settlements[0].ID).Error; err != nil {
					t.Fatal(err)
				}
				if !bytes.Equal(stored.ContextCipher, settlements[0].ContextCipher) {
					t.Fatal("aggregate failure changed durable settlement ciphertext")
				}
			}
		})
	}
}

func TestReplayEvidencePagesKeepPendingRejectedLinkageAndExactSettlementContext(t *testing.T) {
	db, dsn, key := integrationStore(t)
	const account = "paged-evidence"
	saveTestAccount(t, db, account)
	from := time.Date(2026, 10, 20, 0, 0, 0, 0, time.UTC)
	count := replayEvidencePageSize + 3
	for i := count - 1; i >= 0; i-- {
		id := fmt.Sprintf("evidence-%03d", i)
		response := []byte(`{"commodity_id":"cloth","filled_quantity":0,"total_gross_pence":9007199254740993,"customs_duty_pence":0,"diagnostic":"discard"}`)
		if i == 0 {
			response = nil
		}
		action := replayEvidenceAction(t, db, account, id, from, []byte(`{"commodity_id":"cloth","quantity":7,"diagnostic":"discard"}`), response)
		if i == 1 {
			action.Status = 429
			retry := from.Add(time.Minute)
			action.RetryAfter = &retry
		}
		if i == 2 {
			action.State, action.Status = ActionReconciled, 0
		}
		if err := db.db.Create(&action).Error; err != nil {
			t.Fatal(err)
		}
		if i == 0 {
			continue
		}
		exploration := int64(123)
		value := Settlement{
			ID: id, AccountID: account, MissionID: "mission-" + id, ActionID: id, ShipID: "ship",
			Lane: "origin|destination|cloth", Kind: "spot", Origin: "origin", Destination: "destination",
			Commodity: "cloth", QuantityTons: 7, GameSeconds: 3600, CostsKnown: true,
			AcquisitionPence: 9007199254740993, ProceedsPence: 9007199254741007,
			FeesPence: 1, ProvisionsPence: 2, ExpensesPence: 3, ExplorationExpensePence: &exploration,
			StartedAt: from.Add(-time.Hour), CompletedAt: from, CreatedAt: from,
		}
		private, err := db.encodeSettlement(value)
		if err != nil {
			t.Fatal(err)
		}
		private.ContextCipher, err = db.encryptPrivate(map[string]any{
			"ShipID": value.ShipID, "Lane": value.Lane, "Kind": value.Kind,
			"Origin": value.Origin, "Destination": value.Destination, "Commodity": value.Commodity,
			"Diagnostic": strings.Repeat("private-context-", 1000),
		}, "settlements", "context", account, id)
		if err != nil {
			t.Fatal(err)
		}
		if err := db.db.Create(&private).Error; err != nil {
			t.Fatal(err)
		}
	}
	readOnly, _ := replayEvidenceReadOnly(t, dsn, key)
	observed := observeReplayEvidence(t, readOnly)
	actions, err := readOnly.ActionsBetween(context.Background(), account, from, from.Add(time.Hour))
	if err != nil || len(actions) != count {
		t.Fatalf("paged actions lost evidence: count=%d err=%v", len(actions), err)
	}
	settlements, err := readOnly.SettlementsBetween(context.Background(), account, from, from.Add(time.Hour))
	if err != nil || len(settlements) != count-1 {
		t.Fatalf("paged settlements lost evidence: count=%d err=%v", len(settlements), err)
	}
	if actions[0].State != ActionPending || actions[0].Status != 0 || actions[0].Response != nil {
		t.Fatal("projected pending action metadata changed")
	}
	if actions[1].State != ActionComplete || actions[1].Status != 429 ||
		actions[1].RetryAfter == nil || !actions[1].RetryAfter.Equal(from.Add(time.Minute)) {
		t.Fatal("projected rejected action metadata changed")
	}
	if actions[2].State != ActionReconciled || actions[2].Status != 0 {
		t.Fatal("projected reconciled action metadata changed")
	}
	for i, action := range actions {
		if action.ID != fmt.Sprintf("evidence-%03d", i) || action.AccountID != account ||
			!action.CreatedAt.Equal(from) || !action.UpdatedAt.Equal(from) ||
			strings.Contains(string(action.Body)+string(action.Response), "diagnostic") {
			t.Fatalf("action ordering, linkage, timestamps or projection changed at %d: %+v", i, action)
		}
	}
	for i, row := range settlements {
		id := fmt.Sprintf("evidence-%03d", i+1)
		exploration := int64(123)
		want := Settlement{
			ID: id, AccountID: account, MissionID: "mission-" + id, ActionID: id, ShipID: "ship",
			Lane: "origin|destination|cloth", Kind: "spot", Origin: "origin", Destination: "destination",
			Commodity: "cloth", QuantityTons: 7, GameSeconds: 3600, CostsKnown: true,
			AcquisitionPence: 9007199254740993, ProceedsPence: 9007199254741007,
			FeesPence: 1, ProvisionsPence: 2, ExpensesPence: 3, ExplorationExpensePence: &exploration,
			StartedAt: from.Add(-time.Hour), CompletedAt: from, CreatedAt: from,
		}
		row.StartedAt = row.StartedAt.UTC()
		row.CompletedAt = row.CompletedAt.UTC()
		row.CreatedAt = row.CreatedAt.UTC()
		if !reflect.DeepEqual(row, want) {
			t.Fatalf("exact settlement context/accounting/linkage changed: got %+v; want %+v", row, want)
		}
	}
	if observed.maxPageRows != replayEvidencePageSize || observed.pages < 4 || observed.maxCiphertext > MaxReplayEvidenceCiphertextBytes {
		t.Fatalf("readers did not paginate and fetch bounded single rows: %+v", observed)
	}
	if rows, err := readOnly.ActionsBetween(context.Background(), account, from.Add(time.Hour), from.Add(2*time.Hour)); err != nil || !reflect.DeepEqual(rows, []Action{}) {
		t.Fatalf("empty bounded window: rows=%v err=%v", rows, err)
	}
}

func TestReplayEvidenceRejectsOversizedMetadataBeforePayloadFetch(t *testing.T) {
	db, dsn, key := integrationStore(t)
	from := time.Date(2026, 10, 20, 0, 0, 0, 0, time.UTC)
	for _, kind := range []string{"actions", "settlements"} {
		t.Run(kind, func(t *testing.T) {
			account := "metadata-" + kind
			saveTestAccount(t, db, account)
			action := replayEvidenceAction(t, db, account, account+"-action", from, []byte(`{}`), []byte(`{}`))
			if kind == "actions" {
				action.Path = "/" + strings.Repeat("private-metadata-", MaxReplayEvidenceMetadataBytes)
			}
			if err := db.db.Create(&action).Error; err != nil {
				t.Fatal(err)
			}
			if kind == "settlements" {
				value := Settlement{
					ID: account + "-settlement", AccountID: account, ActionID: action.ID,
					MissionID: strings.Repeat("private-metadata-", MaxReplayEvidenceMetadataBytes),
					Lane:      "lane", Kind: "spot", StartedAt: from, CompletedAt: from, CreatedAt: from,
				}
				private, err := db.encodeSettlement(value)
				if err != nil {
					t.Fatal(err)
				}
				if err := db.db.Create(&private).Error; err != nil {
					t.Fatal(err)
				}
			}
			readOnly, authentication := replayEvidenceReadOnly(t, dsn, key)
			observed := observeReplayEvidence(t, readOnly)
			var err error
			if kind == "actions" {
				var rows []Action
				rows, err = readOnly.ActionsBetween(context.Background(), account, from, from.Add(time.Hour))
				if rows != nil {
					t.Fatal("oversized metadata returned partial action evidence")
				}
			} else {
				var rows []Settlement
				rows, err = readOnly.SettlementsBetween(context.Background(), account, from, from.Add(time.Hour))
				if rows != nil {
					t.Fatal("oversized metadata returned partial settlement evidence")
				}
			}
			if !errors.Is(err, ErrReplayEvidenceBudget) || strings.Contains(err.Error(), "private-metadata") {
				t.Fatalf("oversized metadata did not fail with a safe budget error: %v", err)
			}
			if authentication.opens != 0 || observed.maxCiphertext != 0 {
				t.Fatalf("metadata overflow fetched/authenticated a payload: opens=%d materialized=%d", authentication.opens, observed.maxCiphertext)
			}
		})
	}
}

func TestReplayEvidenceSQLGuardRejectsGrowthAfterMetadataEnumeration(t *testing.T) {
	db, dsn, key := integrationStore(t)
	from := time.Date(2026, 10, 20, 0, 0, 0, 0, time.UTC)
	for _, kind := range []string{"actions", "settlements"} {
		t.Run(kind, func(t *testing.T) {
			account := "guarded-growth-" + kind
			saveTestAccount(t, db, account)
			action := replayEvidenceAction(t, db, account, account+"-action", from, []byte(`{}`), nil)
			if err := db.db.Create(&action).Error; err != nil {
				t.Fatal(err)
			}
			var private settlementRow
			if kind == "settlements" {
				var err error
				private, err = db.encodeSettlement(Settlement{
					ID: account, AccountID: account, ActionID: action.ID, MissionID: "mission",
					Lane: "lane", Kind: "spot", StartedAt: from, CompletedAt: from, CreatedAt: from,
				})
				if err != nil {
					t.Fatal(err)
				}
				if err := db.db.Create(&private).Error; err != nil {
					t.Fatal(err)
				}
			}
			large := []byte(fmt.Sprintf(`{"diagnostic":%q}`, strings.Repeat("growth-", MaxReplayEvidenceCiphertextBytes/len("growth-")+1)))
			var grown []byte
			var err error
			if kind == "actions" {
				grown, err = db.encrypt(large, actionAAD(account, action.ID, "response"))
			} else {
				grown, err = db.encryptPrivate(json.RawMessage(large), "settlements", "context", account, private.ID)
			}
			if err != nil {
				t.Fatal(err)
			}
			readOnly, authentication := replayEvidenceReadOnly(t, dsn, key)
			observed := observeReplayEvidence(t, readOnly)
			growthApplied := false
			const callback = "test:grow_replay_evidence_after_metadata"
			if err := readOnly.db.Callback().Query().After("gorm:query").Register(callback, func(tx *gorm.DB) {
				if _, ok := tx.Statement.Dest.(*[]replayEvidenceRowMeta); !ok || growthApplied {
					return
				}
				growthApplied = true
				if kind == "actions" {
					err = db.db.Model(&actionRow{}).Where("id = ?", action.ID).
						Updates(map[string]any{"state": ActionComplete, "status": 200, "response_cipher": grown}).Error
				} else {
					err = db.db.Model(&settlementRow{}).Where("id = ?", private.ID).Update("context_cipher", grown).Error
				}
				if err != nil {
					tx.AddError(err)
				}
			}); err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() {
				if err := readOnly.db.Callback().Query().Remove(callback); err != nil {
					t.Error(err)
				}
			})
			if kind == "actions" {
				if rows, err := readOnly.ActionsBetween(context.Background(), account, from, from.Add(time.Hour)); !errors.Is(err, ErrReplayEvidenceBudget) || rows != nil {
					t.Fatalf("grown action bypassed its SQL guard: rows=%d err=%v", len(rows), err)
				}
			} else if rows, err := readOnly.SettlementsBetween(context.Background(), account, from, from.Add(time.Hour)); !errors.Is(err, ErrReplayEvidenceBudget) || rows != nil {
				t.Fatalf("grown settlement bypassed its SQL guard: rows=%d err=%v", len(rows), err)
			}
			if !growthApplied || authentication.opens != 0 || observed.maxCiphertext != 0 {
				t.Fatalf("guard did not reject before fetch/authentication: grew=%v opens=%d materialized=%d", growthApplied, authentication.opens, observed.maxCiphertext)
			}
		})
	}
}

func TestReplayAccountingSnapshotProjectsScopedExactEvidenceAndRejectsOversizedSource(t *testing.T) {
	db, dsn, key := integrationStore(t)
	ctx := context.Background()
	const account = "accounting-evidence"
	saveTestAccount(t, db, account)
	saveTestAccount(t, db, "accounting-other")
	saveTestAccount(t, db, "accounting-missing")
	want := map[string]json.RawMessage{
		"_expense_allocations": json.RawMessage(`{"bill-1":"mission-1"}`),
		"ledger":               json.RawMessage(`[{"id":"bill-1","amount_pence":9007199254740993,"created_at":"2026-10-20T01:02:03.123456Z"}]`),
		"vaults":               json.RawMessage(`{"vaults":[{"account_id":"owned-vault","balance_pence":9007199254740993}]}`),
		"contracts":            json.RawMessage(`[{"id":"freight-1","status":"defaulted","collateral_pence":9223372036854775807}]`),
	}
	source := make(map[string]json.RawMessage, len(want)+2)
	for key, value := range want {
		source[key] = value
	}
	source["profile"] = json.RawMessage(`{"company":{"id":"must-not-be-loaded"}}`)
	source["diagnostic"] = json.RawMessage(fmt.Sprintf("%q", strings.Repeat("private-diagnostic-", 8192)))
	raw, err := json.Marshal(source)
	if err != nil {
		t.Fatal(err)
	}
	if err := db.db.Create(&snapshotRow{AccountID: account, Data: raw, UpdatedAt: time.Now().UTC()}).Error; err != nil {
		t.Fatal(err)
	}
	if err := db.db.Create(&snapshotRow{
		AccountID: "accounting-other", Data: json.RawMessage(`{"ledger":[{"id":"other-account-bill"}]}`),
		UpdatedAt: time.Now().UTC(),
	}).Error; err != nil {
		t.Fatal(err)
	}
	readOnly, _ := replayEvidenceReadOnly(t, dsn, key)
	maxMaterialized := 0
	const callback = "test:replay_accounting_snapshot_projection"
	if err := readOnly.db.Callback().Query().After("gorm:query").Register(callback, func(tx *gorm.DB) {
		switch row := tx.Statement.Dest.(type) {
		case *replayAccountingSnapshotRow:
			maxMaterialized = max(maxMaterialized, len(row.Data))
		case *snapshotRow:
			maxMaterialized = max(maxMaterialized, len(row.Data))
		}
	}); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := readOnly.db.Callback().Query().Remove(callback); err != nil {
			t.Error(err)
		}
	})
	if rows, err := readOnly.ReplayAccountingSnapshot(ctx, "accounting-missing"); err != nil || rows == nil || len(rows) != 0 {
		t.Fatalf("missing accounting snapshot did not preserve the empty-map contract: rows=%v err=%v", rows, err)
	}
	if rows, err := readOnly.ReplayAccountingSnapshot(ctx, " "); !errors.Is(err, ErrInvalidInput) || rows != nil {
		t.Fatalf("invalid accounting account reached storage: rows=%v err=%v", rows, err)
	}
	got, err := readOnly.ReplayAccountingSnapshot(ctx, account)
	if err != nil || len(got) != len(want) {
		t.Fatalf("accounting snapshot projection lost evidence or retained unrelated data: keys=%d err=%v", len(got), err)
	}
	decodeExact := func(raw json.RawMessage) any {
		t.Helper()
		decoder := json.NewDecoder(bytes.NewReader(raw))
		decoder.UseNumber()
		var value any
		if err := decoder.Decode(&value); err != nil {
			t.Fatal(err)
		}
		return value
	}
	for key, value := range want {
		if !reflect.DeepEqual(decodeExact(got[key]), decodeExact(value)) {
			t.Fatalf("accounting projection changed exact %s evidence: got %s; want %s", key, got[key], value)
		}
	}
	if maxMaterialized == 0 || maxMaterialized >= 1024 {
		t.Fatalf("unrelated snapshot payload was materialized: %d bytes", maxMaterialized)
	}
	if err := db.db.Exec(`UPDATE snapshots SET data = jsonb_build_object(
		'ledger', data -> 'ledger',
		'diagnostic', repeat('sensitive-accounting-', ?)
	) WHERE account_id = ?`, MaxSnapshotBytes/len("sensitive-accounting-")+1, account).Error; err != nil {
		t.Fatal(err)
	}
	var before string
	if err := db.db.Table("snapshots").Select("md5(data::text)").Where("account_id = ?", account).Scan(&before).Error; err != nil {
		t.Fatal(err)
	}
	maxMaterialized = 0
	if rows, err := readOnly.ReplayAccountingSnapshot(ctx, account); !errors.Is(err, ErrReplayEvidenceBudget) || rows != nil {
		t.Fatalf("oversized source returned truncated accounting evidence: rows=%v err=%v", rows, err)
	} else if strings.Contains(err.Error(), "sensitive-accounting") {
		t.Fatal("accounting budget error exposed source data")
	}
	if maxMaterialized != 0 {
		t.Fatalf("oversized accounting source returned payload bytes: %d", maxMaterialized)
	}
	var after string
	if err := db.db.Table("snapshots").Select("md5(data::text)").Where("account_id = ?", account).Scan(&after).Error; err != nil {
		t.Fatal(err)
	}
	if before != after {
		t.Fatal("read-only accounting guard changed the durable snapshot")
	}
}

func TestReplaySnapshotIsRepeatableReadAndReadOnly(t *testing.T) {
	db, _, _ := integrationStore(t)
	const account = "repeatable-replay"
	saveTestAccount(t, db, account)
	at := time.Now().UTC().Truncate(time.Microsecond)
	first := replayAcceptanceRecord(t, db, replayAcceptanceFrame(account, "first", at, []byte(`{}`)))
	last := replayAcceptanceRecord(t, db, replayAcceptanceFrame(account, "last", at.Add(time.Minute), []byte(`{}`)))
	err := db.WithReplaySnapshot(context.Background(), func(view *Store) error {
		var settings struct {
			Isolation string
			ReadOnly  string
		}
		if err := view.db.Raw("SELECT current_setting('transaction_isolation') AS isolation, current_setting('transaction_read_only') AS read_only").
			Scan(&settings).Error; err != nil {
			return err
		}
		if settings.Isolation != "repeatable read" || settings.ReadOnly != "on" {
			t.Fatalf("replay transaction is not repeatable/read-only: %+v", settings)
		}
		scan := func() ([]int64, error) {
			var ids []int64
			err := view.ScanReplayFrames(context.Background(), account, func(frame ReplayFrame) error {
				ids = append(ids, frame.ID)
				return nil
			})
			return ids, err
		}
		before, err := scan()
		if err != nil {
			return err
		}
		replayAcceptanceRecord(t, db, replayAcceptanceFrame(account, "concurrent-backfill", at.Add(30*time.Second), []byte(`{"new":true}`)))
		after, err := scan()
		if err != nil {
			return err
		}
		if !reflect.DeepEqual(before, []int64{first, last}) || !reflect.DeepEqual(before, after) {
			t.Fatalf("replay passes changed after concurrent backfill: before=%v after=%v", before, after)
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if frames := replayAcceptanceFrames(t, db, account); len(frames) != 3 {
		t.Fatalf("fresh snapshot did not see the concurrent backfill: %d", len(frames))
	}
}

func TestReplaySettlementReadersPreservePassengerEvidence(t *testing.T) {
	db, dsn, key := integrationStore(t)
	const account = "replay-passenger-evidence"
	saveTestAccount(t, db, account)
	from := time.Date(2026, 10, 20, 0, 0, 0, 0, time.UTC)
	paid := passageSettlementFixture(account, "replay-paid", "paid-event", 1_000, from.Add(time.Hour))
	unknown := passageSettlementFixture(account, "replay-unknown", "", 0, from.Add(2*time.Hour))
	unknown.CostsKnown, unknown.ProceedsUnknown = false, true
	for _, value := range []Settlement{paid, unknown} {
		row, err := db.encodeSettlement(value)
		if err != nil {
			t.Fatal(err)
		}
		if err := db.db.Create(&row).Error; err != nil {
			t.Fatal(err)
		}
	}
	readOnly, authentication := replayEvidenceReadOnly(t, dsn, key)
	for _, kind := range []string{"window", "rolling prefix"} {
		var rows []Settlement
		var err error
		if kind == "window" {
			rows, err = readOnly.SettlementsBetween(context.Background(), account, from, from.Add(3*time.Hour))
		} else {
			rows, err = readOnly.ReplaySourcingBudgetBefore(context.Background(), account, from, from.Add(3*time.Hour), from.Add(3*time.Hour))
		}
		if err != nil || len(rows) != 2 {
			t.Fatalf("%s reader rejected nullable action evidence: rows=%+v err=%v", kind, rows, err)
		}
		if rows[0].ActionID != "" || rows[0].EventID != paid.EventID || rows[0].ProceedsUnknown ||
			rows[0].ProceedsPence != paid.ProceedsPence || !rows[0].CostsKnown {
			t.Fatalf("%s reader lost resolved fare proof: %+v", kind, rows[0])
		}
		if rows[1].ActionID != "" || rows[1].EventID != "" || !rows[1].ProceedsUnknown ||
			rows[1].ProceedsPence != 0 || rows[1].CostsKnown {
			t.Fatalf("%s reader invented a known zero fare: %+v", kind, rows[1])
		}
	}
	if authentication.opens != 4 {
		t.Fatalf("passenger evidence bypassed authentication: %d opens", authentication.opens)
	}
}

func TestReplaySourcingPrefixMatchesLiveWindowAndGuardsRequiredBodies(t *testing.T) {
	db, dsn, key := integrationStore(t)
	const account = "rolling-replay-prefix"
	saveTestAccount(t, db, account)
	from := time.Date(2026, 10, 20, 0, 0, 0, 0, time.UTC)
	before := from.Add(24 * time.Hour)
	values := []Settlement{
		{ID: "irrelevant-old-probe", Kind: "relocation", CompletedAt: from.Add(-11 * 24 * time.Hour)},
		{ID: "attribution-probe", Kind: "relocation", CompletedAt: from.Add(-9 * 24 * time.Hour)},
		{ID: "old-profit", Kind: "spot", CompletedAt: from.Add(-time.Hour)},
		{ID: "window-delivery", Kind: "charter", StartedAt: from.Add(-3 * 24 * time.Hour), CompletedAt: from.Add(6 * time.Hour)},
		{ID: "cutover-delivery", Kind: "charter", CompletedAt: before},
		{ID: "future-probe", Kind: "relocation", CompletedAt: before.Add(time.Hour)},
	}
	for i := range values {
		value := &values[i]
		value.AccountID, value.ActionID, value.MissionID = account, value.ID+"-action", value.ID+"-mission"
		value.ShipID, value.Lane, value.Commodity = "ship", "source|destination|cloth|probe", "cloth"
		value.GameSeconds, value.QuantityTons, value.CostsKnown = 3600, 1, true
		if value.StartedAt.IsZero() {
			value.StartedAt = value.CompletedAt.Add(-time.Minute)
		}
		value.CreatedAt = value.CompletedAt
		action := replayEvidenceAction(t, db, account, value.ActionID, value.CompletedAt, []byte(`{}`), []byte(`{}`))
		if err := db.db.Create(&action).Error; err != nil {
			t.Fatal(err)
		}
		row, err := db.encodeSettlement(*value)
		if err != nil {
			t.Fatal(err)
		}
		if err := db.db.Create(&row).Error; err != nil {
			t.Fatal(err)
		}
	}
	live, err := db.SourcingBudgetSettlements(context.Background(), account, from, before.Add(-time.Microsecond))
	if err != nil {
		t.Fatal(err)
	}
	// The live attribution query includes the older source probe, not old
	// unrelated profit. Neither irrelevant nor future ciphertext is needed.
	if err := db.db.Model(&settlementRow{}).
		Where("id IN ?", []string{"irrelevant-old-probe", "old-profit", "cutover-delivery", "future-probe"}).
		Update("context_cipher", bytes.Repeat([]byte("x"), MaxReplayEvidenceCiphertextBytes+1)).Error; err != nil {
		t.Fatal(err)
	}
	readOnly, authentication := replayEvidenceReadOnly(t, dsn, key)
	rows, err := readOnly.ReplaySourcingBudgetBefore(context.Background(), account, from, before, before)
	if err != nil || len(rows) != 2 || len(rows) != len(live) || authentication.opens != 2 {
		t.Fatalf("rolling prefix lost live evidence or read irrelevant bodies: rows=%+v live=%+v opens=%d err=%v", rows, live, authentication.opens, err)
	}
	for i, row := range rows {
		want := live[i]
		if row.ID != want.ID || row.Kind != want.Kind || row.Lane != want.Lane || row.CostsKnown != want.CostsKnown ||
			row.QuantityTons != want.QuantityTons || !row.StartedAt.Equal(want.StartedAt) || !row.CompletedAt.Equal(want.CompletedAt) {
			t.Fatalf("rolling prefix differs from live row: got=%+v want=%+v", row, want)
		}
	}
	// A holdout may start after the tape's accounting boundary, even after
	// its budget start. Its in-window delivery still needs this older probe.
	rows, err = readOnly.ReplaySourcingBudgetBefore(context.Background(), account, from.Add(3*time.Hour), from, before)
	if err != nil || len(rows) != 1 || rows[0].ID != "attribution-probe" {
		t.Fatalf("holdout delivery lost its pre-tape attribution probe: rows=%+v err=%v", rows, err)
	}
	if err := db.db.Model(&settlementRow{}).Where("id = ?", "attribution-probe").
		Update("context_cipher", bytes.Repeat([]byte("x"), MaxReplayEvidenceCiphertextBytes+1)).Error; err != nil {
		t.Fatal(err)
	}
	if rows, err := readOnly.ReplaySourcingBudgetBefore(context.Background(), account, from, before, before); !errors.Is(err, ErrReplayEvidenceBudget) || rows != nil {
		t.Fatalf("required oversized prefix returned partial evidence: rows=%+v err=%v", rows, err)
	}
}

func TestReplayHistoryPrefixesKeepCharterWindowAndLatestNeededProbe(t *testing.T) {
	db, dsn, key := integrationStore(t)
	const account = "replay-prefixes"
	saveTestAccount(t, db, account)
	at := time.Now().UTC().Truncate(time.Microsecond)
	cutover := at.Add(time.Hour)
	lane := "source|destination|cloth|probe"
	values := []Settlement{
		{ID: "old-charter", Kind: "charter", ProceedsPence: 200, CompletedAt: at.Add(-time.Hour)},
		{ID: "window-charter", Kind: "charter", ProceedsPence: 300, CompletedAt: at},
		{ID: "cutover-charter", Kind: "charter", ProceedsPence: 500, CompletedAt: cutover},
		{ID: "future-charter", Kind: "charter", ProceedsPence: 700, CompletedAt: cutover.Add(time.Second)},
		{ID: "old-probe", Kind: "relocation", Lane: lane, ExpensesPence: 5, CompletedAt: at},
		{ID: "latest-probe", Kind: "relocation", Lane: lane, ExpensesPence: 7, CompletedAt: at.Add(time.Minute)},
		{ID: "future-probe", Kind: "relocation", Lane: lane, ExpensesPence: 11, CompletedAt: cutover.Add(time.Second)},
		{ID: "unrelated-probe", Kind: "relocation", Lane: "unrelated", ExpensesPence: 13, CompletedAt: at},
	}
	for i := range values {
		value := &values[i]
		value.AccountID, value.ActionID, value.MissionID = account, value.ID+"-action", value.ID+"-mission"
		value.GameSeconds, value.QuantityTons, value.CostsKnown = 3600, 1, true
		value.StartedAt, value.CreatedAt = value.CompletedAt.Add(-time.Minute), value.CompletedAt
		if value.Lane == "" {
			value.Lane = "charter-lane"
		}
		action := replayEvidenceAction(t, db, account, value.ActionID, value.CompletedAt, []byte(`{}`), []byte(`{}`))
		if err := db.db.Create(&action).Error; err != nil {
			t.Fatal(err)
		}
		row, err := db.encodeSettlement(*value)
		if err != nil {
			t.Fatal(err)
		}
		if err := db.db.Create(&row).Error; err != nil {
			t.Fatal(err)
		}
	}
	// An irrelevant or superseded private body must not make the exact
	// latest-lane query load that ciphertext.
	if err := db.db.Model(&settlementRow{}).Where("id IN ?", []string{"old-probe", "unrelated-probe"}).
		Update("context_cipher", bytes.Repeat([]byte("x"), MaxReplayEvidenceCiphertextBytes+1)).Error; err != nil {
		t.Fatal(err)
	}
	readOnly, authentication := replayEvidenceReadOnly(t, dsn, key)
	proceeds, err := readOnly.ReplayCharterProceedsBefore(context.Background(), account, at, cutover)
	if err != nil || proceeds != 800 || authentication.opens != 0 {
		t.Fatalf("charter prefix included pre-window/future rows: %d opens=%d err=%v", proceeds, authentication.opens, err)
	}
	completions, err := readOnly.ReplayProbeCompletionsBefore(context.Background(), account, []string{lane, "missing"}, cutover)
	if err != nil || len(completions) != 1 || !completions[lane].Equal(at.Add(time.Minute)) || authentication.opens != 1 {
		t.Fatalf("latest necessary probe query lost its time bound: completions=%v opens=%d err=%v", completions, authentication.opens, err)
	}
	if err := db.db.Model(&settlementRow{}).Where("id = ?", "latest-probe").
		Update("context_cipher", bytes.Repeat([]byte("x"), MaxReplayEvidenceCiphertextBytes+1)).Error; err != nil {
		t.Fatal(err)
	}
	if rows, err := readOnly.ReplayProbeCompletionsBefore(context.Background(), account, []string{lane}, cutover); !errors.Is(err, ErrReplayEvidenceBudget) || rows != nil {
		t.Fatalf("necessary over-budget prefix returned partial evidence: rows=%v err=%v", rows, err)
	}
}

type replaySnapshotFailurePool struct {
	gorm.ConnPool
	beginErr  error
	commitErr error
}

func (pool replaySnapshotFailurePool) BeginTx(ctx context.Context, options *sql.TxOptions) (gorm.ConnPool, error) {
	if pool.beginErr != nil {
		return nil, pool.beginErr
	}
	beginner := pool.ConnPool.(interface {
		BeginTx(context.Context, *sql.TxOptions) (*sql.Tx, error)
	})
	tx, err := beginner.BeginTx(ctx, options)
	if err != nil {
		return nil, err
	}
	return &replaySnapshotFailureTx{Tx: tx, err: pool.commitErr}, nil
}

type replaySnapshotFailureTx struct {
	*sql.Tx
	err error
}

func (tx replaySnapshotFailureTx) Commit() error {
	if tx.err != nil {
		return tx.err
	}
	return tx.Tx.Commit()
}

func TestReplaySnapshotRedactsLifecycleErrorsAndPreservesCallbackErrors(t *testing.T) {
	db, _, _ := integrationStore(t)
	const secret = "postgres://private:snapshot-driver-secret@host/database"
	for _, stage := range []string{"begin", "commit", "callback"} {
		t.Run(stage, func(t *testing.T) {
			failure := errors.New(secret)
			wrapped := &Store{db: db.db.Session(&gorm.Session{NewDB: true, Context: context.Background()})}
			pool := replaySnapshotFailurePool{ConnPool: wrapped.db.Statement.ConnPool}
			switch stage {
			case "begin":
				pool.beginErr = failure
			case "commit":
				pool.commitErr = failure
			}
			wrapped.db.Statement.ConnPool = pool
			called := false
			err := wrapped.WithReplaySnapshot(context.Background(), func(*Store) error {
				called = true
				if stage == "callback" {
					return failure
				}
				return nil
			})
			if !errors.Is(err, failure) || called != (stage != "begin") {
				t.Fatalf("snapshot lost its cause or ran an invalid callback: called=%t err=%v", called, err)
			}
			if stage == "callback" {
				if err != failure {
					t.Fatalf("callback error was replaced: %v", err)
				}
				return
			}
			var masked databaseError
			if !errors.As(err, &masked) || strings.Contains(err.Error(), secret) ||
				!strings.Contains(err.Error(), stage+" replay snapshot") {
				t.Fatalf("transaction lifecycle error was not safely classified: %v", err)
			}
		})
	}
}
