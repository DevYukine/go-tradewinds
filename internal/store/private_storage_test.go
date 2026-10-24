package store

import (
	"bytes"
	"context"
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"net/http"
	"net/url"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/pressly/goose/v3"
	"go.uber.org/zap"
	"gorm.io/driver/postgres"
	"gorm.io/gorm"
	gormlog "gorm.io/gorm/logger"
)

func privateLegacyStore(t *testing.T) (*Store, string, []byte) {
	t.Helper()
	dsn := os.Getenv("TEST_DATABASE_URL")
	if dsn == "" {
		t.Skip("set TEST_DATABASE_URL to run isolated PostgreSQL migration regressions")
	}
	admin, err := sql.Open("pgx", dsn)
	if err != nil {
		t.Fatal(err)
	}
	var random [8]byte
	if _, err := rand.Read(random[:]); err != nil {
		t.Fatal(err)
	}
	schema := "tradewinds_private_" + hex.EncodeToString(random[:])
	if _, err := admin.ExecContext(context.Background(), "CREATE SCHEMA "+schema); err != nil {
		t.Fatal(err)
	}
	var database *Store
	t.Cleanup(func() {
		if database != nil {
			_ = database.Close()
		}
		ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
		defer cancel()
		if _, err := admin.ExecContext(ctx, "DROP SCHEMA "+schema+" CASCADE"); err != nil {
			t.Error(err)
		}
		_ = admin.Close()
	})
	parsed, err := url.Parse(dsn)
	if err == nil && (parsed.Scheme == "postgres" || parsed.Scheme == "postgresql") {
		query := parsed.Query()
		query.Set("search_path", schema)
		parsed.RawQuery = query.Encode()
		dsn = parsed.String()
	} else {
		dsn += " search_path=" + schema
	}
	sqlDB, err := sql.Open("pgx", dsn)
	if err != nil {
		t.Fatal(err)
	}
	migrationFS, err := fs.Sub(migrations, "migrations")
	if err != nil {
		t.Fatal(err)
	}
	provider, err := goose.NewProvider(goose.DialectPostgres, sqlDB, migrationFS, goose.WithDisableGlobalRegistry(true), goose.WithLogger(migrationLogger{log: zap.NewNop()}))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := provider.UpTo(context.Background(), 4); err != nil {
		t.Fatal(err)
	}
	key := bytes.Repeat([]byte{0x42}, 32)
	block, err := aes.NewCipher(key)
	if err != nil {
		t.Fatal(err)
	}
	aead, err := cipher.NewGCM(block)
	if err != nil {
		t.Fatal(err)
	}
	models, err := gorm.Open(postgres.New(postgres.Config{Conn: sqlDB}), &gorm.Config{Logger: gormlog.Default.LogMode(gormlog.Silent)})
	if err != nil {
		t.Fatal(err)
	}
	database = &Store{
		db: models, sqlDB: sqlDB, dsn: dsn, aead: aead, lookupKey: deriveLookupKey(key),
		log: zap.NewNop(), leases: make(map[*accountLease]struct{}),
	}
	return database, dsn, key
}

func privatePolicy(t *testing.T, id, origin, destination string) json.RawMessage {
	t.Helper()
	payload, err := json.Marshal(map[string]any{
		"id": id, "insured_company_id": "foreign-company", "ship_id": "foreign-ship",
		"origin_port_id": origin, "dest_port_id": destination, "insured_value_pence": 600000,
		"hull_value_pence": 600000, "premium_bps": 3000, "total_premium_pence": 180000,
		"covered_bps": 500, "status": "active",
	})
	if err != nil {
		t.Fatal(err)
	}
	policy, err := decodeUnderwritingPolicy(payload)
	if err != nil {
		t.Fatal(err)
	}
	canonical, err := json.Marshal(policy)
	if err != nil {
		t.Fatal(err)
	}
	return canonical
}

func privatePosition(t *testing.T, database *Store, accountID, id string, policy json.RawMessage) UnderwritingPosition {
	t.Helper()
	position := UnderwritingPosition{
		ID: id, AccountID: accountID, ActionID: uuid.NewString(), PolicyID: "private-policy",
		CompanyID: "company-" + accountID, SliceBPS: 500, CollateralPence: 30000, PremiumPence: 9000,
		FrozenPolicy: policy, AcceptedAt: time.Now().UTC().Add(-time.Hour).Truncate(time.Microsecond), Status: "active",
	}
	ctx := context.Background()
	if _, err := database.BeginAction(ctx, &Action{
		ID: position.ActionID, AccountID: accountID, Method: http.MethodPost,
		Path: "/v1/insurance/underwrite", Body: json.RawMessage(`{"policy_id":"private-policy","slice_bps":500}`),
	}); err != nil {
		t.Fatal(err)
	}
	response, err := json.Marshal(map[string]any{
		"policy": policy,
		"underwritings": []any{map[string]any{
			"id": id, "policy_id": position.PolicyID, "underwriter_company_id": position.CompanyID,
			"slice_bps": position.SliceBPS, "collateral_locked_pence": position.CollateralPence,
			"premium_share_pence": position.PremiumPence,
		}},
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := database.CompleteAction(ctx, position.ActionID, http.StatusOK, response, nil); err != nil {
		t.Fatal(err)
	}
	return position
}

func assertPrivateAtRest(t *testing.T, database *Store, canaries ...string) {
	t.Helper()
	for _, table := range []string{"missions", "settlements", "underwriting_policies", "underwriting_positions", "decisions"} {
		var records []string
		if err := database.db.Raw("SELECT to_jsonb(evidence)::text FROM " + table + " AS evidence").Scan(&records).Error; err != nil {
			t.Fatal(err)
		}
		for _, record := range records {
			for _, canary := range canaries {
				if strings.Contains(record, canary) {
					t.Fatalf("%s retained a plaintext canary", table)
				}
			}
		}
	}
}

func assertPrivateDisplays(t *testing.T, database *Store, accountID string, canaries ...string) {
	t.Helper()
	ctx := context.Background()
	dashboard, err := database.Dashboard(ctx)
	if err != nil {
		t.Fatal(err)
	}
	values := []any{dashboard}
	for _, kind := range []string{"settlements", "decisions", "journal", "events", "requests"} {
		value, err := database.History(ctx, accountID, kind, 100)
		if err != nil {
			if kind == "journal" {
				if _, credentialErr := database.Credentials(ctx, accountID); credentialErr != nil {
					rows, ok := value.([]JournalRow)
					if value == nil || ok && len(rows) == 0 {
						continue
					}
					t.Fatal("credential failure returned journal outcome rows")
				}
			}
			t.Fatalf("read %s: %v", kind, err)
		}
		values = append(values, value)
	}
	for _, value := range values {
		encoded, err := json.Marshal(value)
		if err != nil {
			t.Fatal(err)
		}
		for _, canary := range canaries {
			if bytes.Contains(encoded, []byte(canary)) {
				t.Fatal("a display exposed private operational evidence")
			}
		}
	}
}

func TestPrivateEvidenceWritesRemainExactAndDisplayOnlyCopiesAreScrubbed(t *testing.T) {
	database, dsn, key := integrationStore(t)
	ctx := context.Background()
	const account = "private-writes"
	credentials := saveTestAccount(t, database, account)
	shipID := "ship-" + credentials.APIKey
	body := json.RawMessage(fmt.Sprintf("{ \"ship_id\" : %q, \"quantity\" : 7 }", shipID))
	action := &Action{ID: "private-request", AccountID: account, Method: http.MethodPost, Path: "/v1/market/place-order", Body: body}
	if _, err := database.BeginAction(ctx, action); err != nil {
		t.Fatal(err)
	}
	encodedBody, err := json.Marshal([]byte(body))
	if err != nil {
		t.Fatal(err)
	}
	data := json.RawMessage(fmt.Sprintf(`{"unknown_recovery":{"future":9007199254740993},"action":{"id":"private-request","body":%s,"before":{"ship":{"id":%q}}},"built_ship_id":%q}`, encodedBody, shipID, shipID))
	mission := Mission{
		AccountID: account, ShipID: shipID, Phase: "buy", Generation: "unchanged-generation",
		Destination: credentials.Passphrase, Commodity: credentials.APIKey, Data: data,
	}
	if err := database.PutMission(ctx, &mission); err != nil {
		t.Fatal(err)
	}
	if err := database.CompleteAction(ctx, action.ID, http.StatusOK, json.RawMessage(`{"filled":7}`), nil); err != nil {
		t.Fatal(err)
	}
	settlement := Settlement{
		ID: "private-settlement", AccountID: account, ShipID: shipID, MissionID: mission.Generation, ActionID: action.ID,
		Lane: credentials.APIKey + "|" + credentials.Passphrase, Kind: "kind-" + credentials.APIKey,
		Origin: credentials.Passphrase, Destination: credentials.APIKey, Commodity: credentials.Passphrase,
		QuantityTons: 7, CostsKnown: true, ProceedsPence: 9_007_199_254_740_993,
		StartedAt: time.Now().UTC().Add(-time.Hour), CompletedAt: time.Now().UTC(),
	}
	if err := database.PutSettlement(ctx, &settlement); err != nil {
		t.Fatal(err)
	}
	if err := database.PutSettlement(ctx, &settlement); err != nil {
		t.Fatal(err)
	}
	policy := privatePolicy(t, "private-policy", credentials.APIKey, credentials.Passphrase)
	observation := UnderwritingPolicyRecord{
		AccountID: account, PolicyID: "private-policy", TermsHash: credentials.APIKey, Status: "active",
		Payload: policy, ObservedAt: time.Now().UTC(), SourceUpdatedAt: time.Now().UTC(),
	}
	if err := database.PutUnderwritingPolicies(ctx, account, []UnderwritingPolicyRecord{observation}); err != nil {
		t.Fatal(err)
	}
	position := privatePosition(t, database, account, uuid.NewString(), policy)
	if err := database.PutUnderwritingPosition(ctx, &position); err != nil {
		t.Fatal(err)
	}
	if err := database.RecordDecision(ctx, account, shipID, "hold", "safe", map[string]any{"ship_id": shipID}); err != nil {
		t.Fatal(err)
	}
	assertPrivateAtRest(t, database, credentials.APIKey, credentials.Passphrase)
	assertPrivateDisplays(t, database, account, credentials.APIKey, credentials.Passphrase)
	reopened, err := Open(ctx, dsn, key, zap.NewNop())
	if err != nil {
		t.Fatal(err)
	}
	defer reopened.Close()
	if err := reopened.Prune(ctx); err != nil {
		t.Fatal(err)
	}
	storedMission, err := reopened.GetMission(ctx, account, shipID)
	if err != nil || storedMission == nil || storedMission.Generation != mission.Generation || storedMission.ShipID != shipID || !bytes.Equal(storedMission.Data, data) {
		t.Fatalf("raw recovery changed across restart/retention: %+v, %v", storedMission, err)
	}
	var recovered struct {
		Action struct {
			Body []byte `json:"body"`
		} `json:"action"`
	}
	if err := json.Unmarshal(storedMission.Data, &recovered); err != nil {
		t.Fatal(err)
	}
	action.Body = recovered.Action.Body
	if persisted, err := reopened.BeginAction(ctx, action); err != nil || !bytes.Equal(persisted.Body, body) {
		t.Fatalf("recovery changed immutable attempted bytes: %+v, %v", persisted, err)
	}
	settlements, err := reopened.SettlementHistory(ctx, "", settlement.Lane, 100)
	if err != nil || len(settlements) != 1 || !sameSettlement(settlements[0], settlement) {
		t.Fatalf("cross-account exact lane/financial identity changed: %+v, %v", settlements, err)
	}
	policies, err := reopened.UnderwritingPolicies(ctx, account)
	if err != nil || len(policies) != 1 || policies[0].TermsHash != observation.TermsHash || !bytes.Equal(policies[0].Payload, policy) {
		t.Fatalf("policy evidence was scrubbed operationally: %+v, %v", policies, err)
	}
	positions, err := reopened.UnderwritingPositions(ctx, account)
	if err != nil || len(positions) != 1 || positions[0].ID != position.ID || !bytes.Equal(positions[0].FrozenPolicy, policy) {
		t.Fatalf("frozen evidence was scrubbed operationally: %+v, %v", positions, err)
	}
	var original accountSecret
	if err := database.db.Where("account_id = ?", account).Take(&original).Error; err != nil {
		t.Fatal(err)
	}
	for _, mode := range []string{"corrupt", "missing"} {
		t.Run(mode, func(t *testing.T) {
			if mode == "corrupt" {
				if err := database.db.Model(&accountSecret{}).Where("account_id = ?", account).Update("ciphertext", []byte("corrupt")).Error; err != nil {
					t.Fatal(err)
				}
			} else if err := database.db.Where("account_id = ?", account).Delete(&accountSecret{}).Error; err != nil {
				t.Fatal(err)
			}
			assertPrivateDisplays(t, reopened, account, credentials.APIKey, credentials.Passphrase)
		})
	}
}

func TestPrivateReceiptBoundaryRejectsMalformedAndCredentialBearingOwnedIDs(t *testing.T) {
	for _, test := range []struct{ name, id, passphrase string }{
		{name: "malformed", id: "not-a-uuid", passphrase: "receipt-passphrase"},
		{name: "credential", id: "tw_live_receipt_canary", passphrase: "receipt-passphrase"},
		{name: "valid-uuid-containing-secret", id: "00420042-0000-4000-8000-000000000001", passphrase: "00420042"},
	} {
		t.Run(test.name, func(t *testing.T) {
			database, _, _ := integrationStore(t)
			ctx := context.Background()
			const account = "private-receipt"
			credentials := Credentials{APIKey: "tw_live_receipt_canary", Passphrase: test.passphrase}
			if err := database.SaveAccount(ctx, Account{ID: account, Label: account, CompanyID: "company-" + account}, credentials); err != nil {
				t.Fatal(err)
			}
			position := privatePosition(t, database, account, test.id, privatePolicy(t, "private-policy", "cadiz", "lisbon"))
			recovery := Mission{
				AccountID: account, ShipID: "__underwriting", Phase: "underwriting", Generation: "original-admission",
				Data: json.RawMessage(fmt.Sprintf(`{"action":{"id":%q},"underwriting":{"collateral_pence":30000}}`, position.ActionID)),
			}
			if err := database.PutMission(ctx, &recovery); err != nil {
				t.Fatal(err)
			}
			if err := database.PutUnderwritingPosition(ctx, &position); !errors.Is(err, ErrActionConflict) {
				t.Fatalf("unsafe committed receipt was adopted: %v", err)
			}
			positions, err := database.UnderwritingPositions(ctx, account)
			if err != nil || len(positions) != 0 {
				t.Fatalf("unsafe receipt entered permanent positions: %+v, %v", positions, err)
			}
			persisted, err := database.GetMission(ctx, account, recovery.ShipID)
			if err != nil || persisted == nil || !bytes.Equal(persisted.Data, recovery.Data) {
				t.Fatalf("receipt failure erased unresolved collateral reservation: %+v, %v", persisted, err)
			}
			var count int64
			if err := database.db.Model(&actionRow{}).Where("account_id = ?", account).Count(&count).Error; err != nil || count != 1 {
				t.Fatalf("receipt failure replaced or duplicated the action: %d, %v", count, err)
			}
			var row actionRow
			if err := database.db.Where("id = ?", position.ActionID).Take(&row).Error; err != nil {
				t.Fatal(err)
			}
			decoded, err := database.decodeAction(row)
			if err != nil || !bytes.Contains(decoded.Response, []byte(test.id)) {
				t.Fatalf("original committed receipt evidence was lost: %+v, %v", decoded, err)
			}
			assertPrivateAtRest(t, database, credentials.APIKey, credentials.Passphrase, test.id)
		})
	}
}

func TestPrivateLookupWrongKeyReopenFailsBeforeMissingMissionOrNewState(t *testing.T) {
	database, dsn, _ := integrationStore(t)
	ctx := context.Background()
	const account = "private-wrong-key"
	saveTestAccount(t, database, account)
	mission := Mission{
		AccountID: account, ShipID: "original-ship", Phase: "ambiguous",
		Generation: "original-generation", Data: json.RawMessage(`{"attempt":"original"}`),
	}
	if err := database.PutMission(ctx, &mission); err != nil {
		t.Fatal(err)
	}
	opened, err := Open(ctx, dsn, bytes.Repeat([]byte{0x99}, 32), zap.NewNop())
	if err == nil {
		_ = opened.Close()
		t.Fatal("wrong lookup key opened an existing encrypted store")
	}
	loaded, err := database.GetMission(ctx, account, mission.ShipID)
	if err != nil || loaded == nil || loaded.Generation != mission.Generation || !bytes.Equal(loaded.Data, mission.Data) {
		t.Fatalf("wrong-key reopen changed original recovery: %+v, %v", loaded, err)
	}
	var count int64
	if err := database.db.Model(&missionRow{}).Where("account_id = ?", account).Count(&count).Error; err != nil || count != 1 {
		t.Fatalf("wrong-key reopen forked operational state: %d, %v", count, err)
	}
}

func TestPrivateKeyVerifierRejectsWrongKeyWithNoAccounts(t *testing.T) {
	database, dsn, key := integrationStore(t)
	ctx := context.Background()
	var accounts int64
	if err := database.db.Model(&Account{}).Count(&accounts).Error; err != nil || accounts != 0 {
		t.Fatalf("empty initialized-store fixture is not empty: %d, %v", accounts, err)
	}
	wrong, err := Open(ctx, dsn, bytes.Repeat([]byte{0x99}, 32), zap.NewNop())
	if err == nil {
		_ = wrong.Close()
		t.Fatal("empty initialized store accepted a different encryption/lookup key")
	}
	correct, err := Open(ctx, dsn, key, zap.NewNop())
	if err != nil {
		t.Fatalf("wrong-key attempt replaced the original empty-store verifier: %v", err)
	}
	defer correct.Close()
}

func TestPrivateKeyVerifierKeepsCorruptCredentialsAccountLocalAfterReopen(t *testing.T) {
	database, dsn, key := integrationStore(t)
	ctx := context.Background()
	const healthyID = "private-healthy"
	const corruptID = "private-corrupt"
	healthy := saveTestAccount(t, database, healthyID)
	corrupt := saveTestAccount(t, database, corruptID)
	if err := database.RecordDecision(ctx, healthyID, "healthy-ship", "hold", "healthy account remains readable", nil); err != nil {
		t.Fatal(err)
	}
	if err := database.RecordDecision(ctx, corruptID, "ship-"+corrupt.Passphrase, "hold", "account credentials unreadable", nil); err != nil {
		t.Fatal(err)
	}
	if err := database.db.Model(&accountSecret{}).Where("account_id = ?", corruptID).Update("ciphertext", []byte("corrupt")).Error; err != nil {
		t.Fatal(err)
	}
	reopened, err := Open(ctx, dsn, key, zap.NewNop())
	if err != nil {
		t.Fatalf("one corrupt credential row took down the healthy fleet/API: %v", err)
	}
	defer reopened.Close()
	credentials, err := reopened.Credentials(ctx, healthyID)
	if err != nil || credentials != healthy {
		t.Fatalf("healthy account credentials became unavailable: %+v, %v", credentials, err)
	}
	if _, err := reopened.Credentials(ctx, corruptID); err == nil {
		t.Fatal("corrupt account credentials were silently accepted")
	}
	history, err := reopened.History(ctx, healthyID, "decisions", 100)
	if err != nil {
		t.Fatal(err)
	}
	rows, ok := history.([]Decision)
	if !ok || len(rows) != 1 || rows[0].ShipID != "healthy-ship" || rows[0].Reason != "healthy account remains readable" {
		t.Fatalf("unrelated credential corruption censored healthy account evidence: %+v", history)
	}
	assertPrivateDisplays(t, reopened, corruptID, corrupt.APIKey, corrupt.Passphrase, healthy.APIKey, healthy.Passphrase)
}

func TestPrivateKeyVerifierMissingOrCorruptFailsClosedWithoutRepair(t *testing.T) {
	for _, mode := range []string{"missing", "corrupt"} {
		t.Run(mode, func(t *testing.T) {
			database, dsn, key := integrationStore(t)
			if mode == "missing" {
				if err := database.db.Exec("DELETE FROM private_evidence_key").Error; err != nil {
					t.Fatal(err)
				}
			} else if err := database.db.Exec("UPDATE private_evidence_key SET verifier = ?", bytes.Repeat([]byte{0x99}, 32)).Error; err != nil {
				t.Fatal(err)
			}
			opened, err := Open(context.Background(), dsn, key, zap.NewNop())
			if err == nil {
				_ = opened.Close()
				t.Fatal("unverifiable store silently opened/recreated its key verifier")
			}
			var row struct {
				Verifier []byte
			}
			result := database.db.Table("private_evidence_key").Select("verifier").Find(&row)
			if result.Error != nil {
				t.Fatal(result.Error)
			}
			if mode == "missing" && result.RowsAffected != 0 || mode == "corrupt" && !bytes.Equal(row.Verifier, bytes.Repeat([]byte{0x99}, 32)) {
				t.Fatal("failed startup repaired/replaced the durable key verifier")
			}
		})
	}
}
