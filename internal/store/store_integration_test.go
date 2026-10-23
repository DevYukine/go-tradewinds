package store

import (
	"bytes"
	"context"
	"crypto/rand"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"go.uber.org/zap"
)

func integrationStore(t *testing.T) (*Store, string, []byte) {
	t.Helper()
	dsn := os.Getenv("TEST_DATABASE_URL")
	if dsn == "" {
		t.Skip("set TEST_DATABASE_URL to run isolated real PostgreSQL integration tests")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	admin, err := sql.Open("pgx", dsn)
	if err != nil {
		t.Fatal(err)
	}
	var random [8]byte
	if _, err := rand.Read(random[:]); err != nil {
		_ = admin.Close()
		t.Fatal(err)
	}
	schema := "tradewinds_test_" + hex.EncodeToString(random[:])
	if _, err := admin.ExecContext(ctx, "CREATE SCHEMA "+schema); err != nil {
		_ = admin.Close()
		t.Fatalf("create isolated test schema: %v", err)
	}
	var db *Store
	t.Cleanup(func() {
		if db != nil {
			if err := db.Close(); err != nil {
				t.Errorf("close test store: %v", err)
			}
		}
		cleanupCtx, cleanupCancel := context.WithTimeout(context.Background(), 15*time.Second)
		defer cleanupCancel()
		if _, err := admin.ExecContext(cleanupCtx, "DROP SCHEMA "+schema+" CASCADE"); err != nil {
			t.Errorf("drop isolated test schema: %v", err)
		}
		if err := admin.Close(); err != nil {
			t.Errorf("close test administration connection: %v", err)
		}
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
	key := bytes.Repeat([]byte{0x42}, 32)
	db, err = Open(ctx, dsn, key, zap.NewNop())
	if err != nil {
		t.Fatalf("open migrated test store: %v", err)
	}
	return db, dsn, key
}

func saveTestAccount(t *testing.T, db *Store, id string) Credentials {
	t.Helper()
	credentials := Credentials{
		Handle:     "player-" + id,
		Passphrase: "never-persist-plaintext-passphrase-" + id,
		APIKey:     "tw_live_never_persist_plaintext_" + id,
	}
	err := db.SaveAccount(context.Background(), Account{
		ID:        id,
		Label:     "Account " + id,
		CompanyID: "company-" + id,
		Enabled:   true,
	}, credentials)
	if err != nil {
		t.Fatal(err)
	}
	return credentials
}

func insertOldPending(t *testing.T, db *Store, accountID, id string, age time.Duration) *Action {
	t.Helper()
	body := json.RawMessage(`{"quantity":4}`)
	ciphertext, err := db.encrypt(body, actionAAD(accountID, id, "body"))
	if err != nil {
		t.Fatal(err)
	}
	created := time.Now().UTC().Add(-age)
	row := actionRow{
		ID:         id,
		AccountID:  accountID,
		CompanyID:  "company-" + accountID,
		Method:     "POST",
		Path:       "/v1/fleet/depart",
		BodyCipher: ciphertext,
		State:      ActionPending,
		CreatedAt:  created,
		UpdatedAt:  created,
	}
	if err := db.db.Create(&row).Error; err != nil {
		t.Fatal(err)
	}
	return &Action{ID: id, AccountID: accountID, Method: row.Method, Path: row.Path, Body: body}
}

func TestPostgresCredentialIsolationAndCompanyUniqueness(t *testing.T) {
	db, _, _ := integrationStore(t)
	ctx := context.Background()
	one := saveTestAccount(t, db, "one")
	two := saveTestAccount(t, db, "two")
	for id, expected := range map[string]Credentials{"one": one, "two": two} {
		actual, err := db.Credentials(ctx, id)
		if err != nil || actual != expected {
			t.Fatalf("isolated credentials for %s: %+v, %v", id, actual, err)
		}
	}
	var first, second accountSecret
	if err := db.db.First(&first, "account_id = ?", "one").Error; err != nil {
		t.Fatal(err)
	}
	if err := db.db.First(&second, "account_id = ?", "two").Error; err != nil {
		t.Fatal(err)
	}
	for _, secret := range []string{one.Handle, one.Passphrase, one.APIKey} {
		if bytes.Contains(first.Ciphertext, []byte(secret)) {
			t.Fatal("credentials were stored as plaintext")
		}
	}
	if err := db.db.Model(&accountSecret{}).Where("account_id = ?", "two").Update("ciphertext", first.Ciphertext).Error; err != nil {
		t.Fatal(err)
	}
	if _, err := db.Credentials(ctx, "two"); err == nil {
		t.Fatal("ciphertext copied to another account authenticated without its account ID")
	}
	if err := db.db.Model(&accountSecret{}).Where("account_id = ?", "two").Update("ciphertext", second.Ciphertext).Error; err != nil {
		t.Fatal(err)
	}
	if err := db.SaveAccount(ctx, Account{ID: "duplicate", Label: "duplicate", CompanyID: "company-one"}, one); err == nil {
		t.Fatal("a second local record imported the same company")
	}
	accounts, err := db.Accounts(ctx)
	if err != nil || len(accounts) != 2 {
		t.Fatalf("failed duplicate import was not rolled back: %d, %v", len(accounts), err)
	}
}

func TestPostgresActionImmutableReplayAndEncryptedRawResponse(t *testing.T) {
	db, dsn, key := integrationStore(t)
	ctx := context.Background()
	credentials := saveTestAccount(t, db, "one")
	body := json.RawMessage(fmt.Sprintf(`{"passphrase":%q,"api_key":%q,"quantity":7}`, credentials.Passphrase, credentials.APIKey))
	intent := &Action{ID: "immutable", AccountID: "one", Method: "POST", Path: "/v1/auth/create-key", Body: body}
	first, err := db.BeginAction(ctx, intent)
	if err != nil || first.State != ActionPending || !bytes.Equal(first.Body, body) {
		t.Fatalf("begin immutable request: %+v, %v", first, err)
	}
	for _, changed := range []Action{
		{ID: intent.ID, AccountID: "one", Method: "PATCH", Path: intent.Path, Body: body},
		{ID: intent.ID, AccountID: "one", Method: intent.Method, Path: "/different", Body: body},
		{ID: intent.ID, AccountID: "one", Method: intent.Method, Path: intent.Path, Body: json.RawMessage(`{"quantity":8}`)},
	} {
		if _, err := db.BeginAction(ctx, &changed); !errors.Is(err, ErrActionConflict) {
			t.Fatalf("changed request reused a key: %v", err)
		}
	}
	if err := db.CompleteAction(ctx, intent.ID, 503, json.RawMessage(`{"code":"unavailable"}`), nil); err == nil {
		t.Fatal("transient 5xx settled an ambiguous action")
	}
	if err := db.CompleteAction(ctx, intent.ID, 409, json.RawMessage(`{"code":"idempotency_request_in_flight"}`), nil); err == nil {
		t.Fatal("an in-flight action was settled")
	}
	raw := append([]byte("not JSON: "), []byte(credentials.APIKey)...)
	raw = append(raw, 0xff, '\n')
	if err := db.CompleteAction(ctx, intent.ID, 201, raw, nil); err != nil {
		t.Fatal(err)
	}
	if err := db.CompleteAction(ctx, intent.ID, 201, raw, nil); err != nil {
		t.Fatalf("identical completion was not idempotent: %v", err)
	}
	if err := db.CompleteAction(ctx, intent.ID, 400, []byte("changed"), nil); !errors.Is(err, ErrActionConflict) {
		t.Fatalf("terminal outcome changed: %v", err)
	}
	var stored actionRow
	if err := db.db.First(&stored, "id = ?", intent.ID).Error; err != nil {
		t.Fatal(err)
	}
	for _, ciphertext := range [][]byte{stored.BodyCipher, stored.ResponseCipher} {
		if bytes.Contains(ciphertext, []byte(credentials.APIKey)) || bytes.Contains(ciphertext, []byte(credentials.Passphrase)) {
			t.Fatal("journal payload leaked plaintext credentials")
		}
	}
	if err := db.db.Model(&actionRow{}).Where("id = ?", intent.ID).Update("method", "PATCH").Error; err == nil {
		t.Fatal("database accepted an immutable request mutation")
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}
	reopened, err := Open(ctx, dsn, key, zap.NewNop())
	if err != nil {
		t.Fatalf("restart with existing Goose schema: %v", err)
	}
	t.Cleanup(func() {
		if err := reopened.Close(); err != nil {
			t.Error(err)
		}
	})
	replayed, err := reopened.BeginAction(ctx, intent)
	if err != nil || replayed.State != ActionComplete || replayed.Status != 201 || !bytes.Equal(replayed.Body, body) || !bytes.Equal(replayed.Response, raw) {
		t.Fatalf("restart did not return exact immutable request and raw outcome: %+v, %v", replayed, err)
	}
	pending, err := reopened.PendingActions(ctx, "one")
	if err != nil || len(pending) != 0 {
		t.Fatalf("completed action remained pending: %d, %v", len(pending), err)
	}
}

func TestPostgresConcurrentActionConflictAndExpiredReconciliation(t *testing.T) {
	db, _, _ := integrationStore(t)
	ctx := context.Background()
	saveTestAccount(t, db, "one")
	var group sync.WaitGroup
	results := make(chan error, 2)
	for _, quantity := range []int{1, 2} {
		group.Add(1)
		go func(quantity int) {
			defer group.Done()
			_, err := db.BeginAction(ctx, &Action{
				ID: "concurrent", AccountID: "one", Method: "POST", Path: "/v1/market/buy",
				Body: json.RawMessage(fmt.Sprintf(`{"quantity":%d}`, quantity)),
			})
			results <- err
		}(quantity)
	}
	group.Wait()
	close(results)
	var successes, conflicts int
	for err := range results {
		switch {
		case err == nil:
			successes++
		case errors.Is(err, ErrActionConflict):
			conflicts++
		default:
			t.Fatal(err)
		}
	}
	if successes != 1 || conflicts != 1 {
		t.Fatalf("concurrent insert did not pick one immutable winner: success=%d conflict=%d", successes, conflicts)
	}
	aged := insertOldPending(t, db, "one", "expired", ActionReplayWindow+time.Minute)
	persisted, err := db.BeginAction(ctx, aged)
	if !errors.Is(err, ErrActionExpired) || persisted == nil || !bytes.Equal(persisted.Body, aged.Body) {
		t.Fatalf("unsafe replay was not blocked with retained evidence: %+v, %v", persisted, err)
	}
	evidence := json.RawMessage(`{"ship_status":"sailing","source":"authoritative_rest"}`)
	if err := db.ReconcileAction(ctx, aged.ID, evidence); err != nil {
		t.Fatal(err)
	}
	reconciled, err := db.BeginAction(ctx, aged)
	if err != nil || reconciled.State != ActionReconciled || reconciled.Status != 0 || !bytes.Equal(reconciled.Response, evidence) {
		t.Fatalf("reconciliation fabricated an HTTP outcome: %+v, %v", reconciled, err)
	}
	if err := db.ReconcileAction(ctx, aged.ID, json.RawMessage(`{"changed":true}`)); !errors.Is(err, ErrActionConflict) {
		t.Fatalf("reconciliation was mutable: %v", err)
	}
}

func TestPostgresEventDeduplicationAndCursorTransaction(t *testing.T) {
	db, _, _ := integrationStore(t)
	ctx := context.Background()
	saveTestAccount(t, db, "one")
	saveTestAccount(t, db, "two")
	for _, id := range []string{"opaque:first", "opaque:second", "opaque:first"} {
		if err := db.RecordEvent(ctx, "one", id, "ship_arrived", json.RawMessage(`{"quantity":4}`)); err != nil {
			t.Fatal(err)
		}
	}
	if err := db.RecordEvent(ctx, "two", "opaque:first", "ship_arrived", json.RawMessage(`{}`)); err != nil {
		t.Fatal(err)
	}
	cursor, err := db.Cursor(ctx, "one")
	if err != nil || cursor != "opaque:second" {
		t.Fatalf("duplicate event regressed the cursor: %q, %v", cursor, err)
	}
	var count int64
	if err := db.db.Model(&Event{}).Where("account_id = ?", "one").Count(&count).Error; err != nil || count != 2 {
		t.Fatalf("events not deduplicated per account: %d, %v", count, err)
	}
	trigger := `CREATE FUNCTION reject_test_cursor() RETURNS trigger LANGUAGE plpgsql AS $$
	BEGIN
		IF NEW.event_id = 'will-fail' THEN
			RAISE EXCEPTION 'injected cursor failure';
		END IF;
		RETURN NEW;
	END $$;
	CREATE TRIGGER reject_test_cursor BEFORE INSERT OR UPDATE ON event_cursors
	FOR EACH ROW EXECUTE FUNCTION reject_test_cursor();`
	if err := db.db.Exec(trigger).Error; err != nil {
		t.Fatal(err)
	}
	if err := db.RecordEvent(ctx, "one", "will-fail", "ship_arrived", json.RawMessage(`{}`)); err == nil {
		t.Fatal("cursor failure was silently accepted")
	}
	if err := db.db.Model(&Event{}).Where("account_id = ? AND event_id = ?", "one", "will-fail").Count(&count).Error; err != nil || count != 0 {
		t.Fatalf("event committed without its cursor: %d, %v", count, err)
	}
	cursor, err = db.Cursor(ctx, "one")
	if err != nil || cursor != "opaque:second" {
		t.Fatalf("failed transaction changed cursor: %q, %v", cursor, err)
	}
	if err := db.ResetCursor(ctx, "one"); err != nil {
		t.Fatal(err)
	}
	cursor, err = db.Cursor(ctx, "one")
	if err != nil || cursor != "0" {
		t.Fatalf("gap did not replace rejected cursor: %q, %v", cursor, err)
	}
	for _, id := range []string{"opaque:first", "opaque:second", "opaque:first"} {
		if err := db.RecordEvent(ctx, "one", id, "ship_arrived", json.RawMessage(`{"quantity":999}`)); err != nil {
			t.Fatal(err)
		}
	}
	cursor, err = db.Cursor(ctx, "one")
	if err != nil || cursor != "opaque:second" {
		t.Fatalf("retained replay after reset did not advance monotonically: %q, %v", cursor, err)
	}
	var first Event
	if err := db.db.Where("account_id = ? AND event_id = ?", "one", "opaque:first").Take(&first).Error; err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(first.Payload), "999") {
		t.Fatal("duplicate event replaced immutable retained payload")
	}
}

func TestPostgresRetentionPreservesOperationalStateAndIntegerAccounting(t *testing.T) {
	db, _, _ := integrationStore(t)
	ctx := context.Background()
	saveTestAccount(t, db, "one")
	old := time.Now().UTC().Add(-8 * 24 * time.Hour)
	if err := db.RecordRequest(ctx, "one", "GET", "/old", 200, time.Millisecond, nil, []byte(`{}`), ""); err != nil {
		t.Fatal(err)
	}
	if err := db.RecordEvent(ctx, "one", "old", "ship_arrived", json.RawMessage(`{}`)); err != nil {
		t.Fatal(err)
	}
	if err := db.RecordDecision(ctx, "one", "ship", "old", "old decision", nil); err != nil {
		t.Fatal(err)
	}
	for _, table := range []string{"requests", "events", "decisions"} {
		if err := db.db.Table(table).Where("account_id = ?", "one").Update("created_at", old).Error; err != nil {
			t.Fatal(err)
		}
	}
	if err := db.RecordRequest(ctx, "one", "GET", "/fresh", 200, time.Millisecond, nil, nil, ""); err != nil {
		t.Fatal(err)
	}
	if err := db.RecordEvent(ctx, "one", "fresh", "ship_arrived", json.RawMessage(`{}`)); err != nil {
		t.Fatal(err)
	}
	if err := db.RecordDecision(ctx, "one", "ship", "fresh", "fresh decision", nil); err != nil {
		t.Fatal(err)
	}
	pending := insertOldPending(t, db, "one", "keep-pending", 8*24*time.Hour)
	completed := insertOldPending(t, db, "one", "keep-completed", 8*24*time.Hour)
	if err := db.CompleteAction(ctx, completed.ID, 200, []byte(`{"filled_quantity":3}`), nil); err != nil {
		t.Fatal(err)
	}
	mission := &Mission{AccountID: "one", ShipID: "ship", Phase: "sailing", Generation: "mission-1", Data: json.RawMessage(`{"action_id":"keep-pending"}`)}
	if err := db.PutMission(ctx, mission); err != nil {
		t.Fatal(err)
	}
	if err := db.SaveSnapshot(ctx, "one", map[string]json.RawMessage{"ships": json.RawMessage(`[{"id":"ship"}]`)}); err != nil {
		t.Fatal(err)
	}
	if err := db.RecordMetric(ctx, "one", "net_worth_pence", 12345); err != nil {
		t.Fatal(err)
	}
	if err := db.db.Table("metrics").Where("account_id = ?", "one").Update("recorded_at", old).Error; err != nil {
		t.Fatal(err)
	}
	settlement := &Settlement{
		ID: "mission-1", AccountID: "one", ShipID: "ship", MissionID: "mission-1", Lane: "a:b:tea",
		ActionID: completed.ID, Kind: "spot", Origin: "a", Destination: "b", Commodity: "tea",
		QuantityTons: 3, GameSeconds: 86400, CostsKnown: true,
		AcquisitionPence: 100, ProceedsPence: 9_007_199_254_740_993,
		FeesPence: 2, ProvisionsPence: 3, ExpensesPence: 4,
		StartedAt: old.Add(-time.Hour), CompletedAt: old, CreatedAt: old,
	}
	if err := db.PutSettlement(ctx, settlement); err != nil {
		t.Fatal(err)
	}
	if err := db.PutSettlement(ctx, settlement); err != nil {
		t.Fatalf("settlement replay was not idempotent: %v", err)
	}
	changed := *settlement
	changed.ProceedsPence++
	if err := db.PutSettlement(ctx, &changed); !errors.Is(err, ErrActionConflict) {
		t.Fatalf("settled integer accounting changed: %v", err)
	}
	if err := db.Prune(ctx); err != nil {
		t.Fatal(err)
	}
	for _, table := range []string{"requests", "events", "decisions"} {
		var count int64
		if err := db.db.Table(table).Where("account_id = ?", "one").Count(&count).Error; err != nil || count != 1 {
			t.Fatalf("seven-day retention for %s: count=%d, err=%v", table, count, err)
		}
	}
	actions, err := db.PendingActions(ctx, "one")
	if err != nil || len(actions) != 1 || actions[0].ID != pending.ID {
		t.Fatalf("pending action was pruned: %+v, %v", actions, err)
	}
	if _, err := db.BeginAction(ctx, completed); err != nil {
		t.Fatalf("terminal action tombstone was lost: %v", err)
	}
	storedMission, err := db.GetMission(ctx, "one", "ship")
	if err != nil || storedMission == nil || storedMission.Generation != mission.Generation {
		t.Fatalf("mission was pruned: %+v, %v", storedMission, err)
	}
	snapshot, err := db.Snapshot(ctx, "one")
	if err != nil || len(snapshot["ships"]) == 0 {
		t.Fatalf("current snapshot was pruned: %v", err)
	}
	metrics, err := db.History(ctx, "one", "metrics", 10)
	if err != nil || len(metrics.([]Metric)) != 2 {
		t.Fatalf("durable metrics were pruned: %v", err)
	}
	for _, metric := range metrics.([]Metric) {
		if metric.ID <= 0 {
			t.Fatal("persisted chart metric has no stable identity")
		}
	}
	var profitMetrics int64
	if err := db.db.Table("metrics").Where("account_id = ? AND name = ?", "one", "realized_profit_pence").Count(&profitMetrics).Error; err != nil || profitMetrics != 1 {
		t.Fatalf("settlement replay did not post exactly one durable chart metric: %d, %v", profitMetrics, err)
	}
	settlements, err := db.SettlementHistory(ctx, "one", "a:b:tea", 10)
	if err != nil || len(settlements) != 1 || settlements[0].ProceedsPence != settlement.ProceedsPence {
		t.Fatalf("integer settlement history was lost or rounded: %+v, %v", settlements, err)
	}
	cursor, err := db.Cursor(ctx, "one")
	if err != nil || cursor != "fresh" {
		t.Fatalf("operational cursor was pruned: %q, %v", cursor, err)
	}
	stats, err := db.ServiceStats(ctx)
	if err != nil {
		t.Fatal(err)
	}
	foundRequestCounter := false
	for _, counter := range stats.Counters {
		if counter.Name == "http_requests_total" {
			foundRequestCounter = true
			if counter.Value != 2 {
				t.Fatalf("request aggregate changed with raw retention: %v", counter.Value)
			}
		}
	}
	if !foundRequestCounter {
		t.Fatal("durable request aggregate was lost")
	}
}

func TestPostgresSecretRedactionAcrossStoredReadSurfaces(t *testing.T) {
	db, _, _ := integrationStore(t)
	ctx := context.Background()
	credentials := saveTestAccount(t, db, "one")
	memberNames, err := json.Marshal(map[string]any{
		"phrase " + credentials.Passphrase: "known passphrase member",
		"api " + credentials.APIKey:        "known API key member",
		"tw_test_unlisted_member":          "unlisted API key member",
		"Bearer member-token":              "unlisted bearer member",
		"password=assigned-member-secret":  "assignment member",
	})
	if err != nil {
		t.Fatal(err)
	}
	request := fmt.Sprintf(`{"passphrase":%q,"deep":[{"API-Key":"unknown-other-key","plain":%q}],"headers":{"X-Trace":"header-never-stored"},"members":%s}`, credentials.Passphrase, credentials.APIKey, memberNames)
	response := fmt.Sprintf(`{"api_key":%q,"nested":{"new_passphrase":"another-secret","detail":%q},"members":%s}`, credentials.APIKey, credentials.Passphrase, memberNames)
	path := "/v1/auth/login?api_key=" + url.QueryEscape(credentials.APIKey) + "&password=query-secret"
	if err := db.RecordRequest(ctx, "one", "POST", path, 0, 42*time.Millisecond, []byte(request), []byte(response), "transport failed with "+credentials.Passphrase); err != nil {
		t.Fatal(err)
	}
	if err := db.RecordEvent(ctx, "one", "secret-event", "notice", json.RawMessage(response)); err != nil {
		t.Fatal(err)
	}
	if err := db.RecordDecision(ctx, "one", "ship", "hold", "reason "+credentials.APIKey, map[string]any{"authorization": "Bearer arbitrary-secret", "detail": credentials.Passphrase}); err != nil {
		t.Fatal(err)
	}
	if err := db.SaveSnapshot(ctx, "one", map[string]json.RawMessage{
		"profile":          json.RawMessage(response),
		credentials.APIKey: json.RawMessage(`{"public_count":2}`),
	}); err != nil {
		t.Fatal(err)
	}
	var surfaces []string
	for _, table := range []string{"requests", "events", "decisions", "snapshots"} {
		var rows []string
		if err := db.db.Raw("SELECT to_jsonb(t)::text FROM " + table + " AS t").Scan(&rows).Error; err != nil {
			t.Fatal(err)
		}
		surfaces = append(surfaces, rows...)
	}
	dashboard, err := db.Dashboard(ctx)
	if err != nil {
		t.Fatal(err)
	}
	encoded, err := json.Marshal(dashboard)
	if err != nil {
		t.Fatal(err)
	}
	surfaces = append(surfaces, string(encoded))
	for _, kind := range []string{"requests", "events", "decisions"} {
		history, err := db.History(ctx, "one", kind, 10)
		if err != nil {
			t.Fatal(err)
		}
		encoded, err := json.Marshal(history)
		if err != nil {
			t.Fatal(err)
		}
		surfaces = append(surfaces, string(encoded))
	}
	for _, surface := range surfaces {
		if apiKeyPattern.MatchString(surface) || bearerPattern.MatchString(surface) {
			t.Fatal("a credential pattern survived in a returned JSON name or value")
		}
		for _, secret := range []string{credentials.Passphrase, credentials.APIKey, "unknown-other-key", "header-never-stored", "another-secret", "query-secret", "arbitrary-secret", "tw_test_unlisted_member", "member-token", "assigned-member-secret"} {
			if strings.Contains(surface, secret) {
				t.Fatalf("secret leaked into a persisted/read surface: %q", secret)
			}
		}
	}
	requests, err := db.History(ctx, "one", "requests", 10)
	if err != nil {
		t.Fatal(err)
	}
	rows := requests.([]Request)
	if len(rows) != 1 || rows[0].Status != 0 || rows[0].Error == "" || rows[0].DurationMS != 42 {
		t.Fatalf("transport metadata was misrepresented: %+v", rows)
	}
}

func TestPostgresOwnershipLossCancelsRunnerAndFencesWrites(t *testing.T) {
	db, _, _ := integrationStore(t)
	ctx := context.Background()
	saveTestAccount(t, db, "one")
	saveTestAccount(t, db, "two")
	ownerCtx, release, err := db.AcquireAccountContext(ctx, "one")
	if err != nil {
		t.Fatal(err)
	}
	defer release()
	if _, duplicateRelease, err := db.AcquireAccountContext(ctx, "one"); !errors.Is(err, ErrAccountLocked) {
		if duplicateRelease != nil {
			duplicateRelease()
		}
		t.Fatalf("duplicate account owner accepted: %v", err)
	}
	otherCtx, otherRelease, err := db.AcquireAccountContext(ctx, "two")
	if err != nil {
		t.Fatalf("different account could not run concurrently: %v", err)
	}
	defer otherRelease()
	if err := db.PutMission(otherCtx, &Mission{AccountID: "two", ShipID: "ship-two", Phase: "idle"}); err != nil {
		t.Fatal(err)
	}
	lease := ownerCtx.Value(leaseContextKey{}).(*accountLease)
	var pid int
	lease.connMu.Lock()
	err = lease.conn.QueryRowContext(ctx, "SELECT pg_backend_pid()").Scan(&pid)
	lease.connMu.Unlock()
	if err != nil {
		t.Fatal(err)
	}
	var terminated bool
	if err := db.db.Raw("SELECT pg_terminate_backend(?)", pid).Scan(&terminated).Error; err != nil || !terminated {
		t.Fatalf("terminate owned lock session: %v, %v", terminated, err)
	}
	select {
	case <-ownerCtx.Done():
	case <-time.After(6 * time.Second):
		t.Fatal("lock loss did not cancel the runner")
	}
	if err := db.PutMission(ownerCtx, &Mission{AccountID: "one", ShipID: "stale", Phase: "idle"}); err == nil {
		t.Fatal("a stale owner wrote a new mission")
	}
	release()
	newCtx, newRelease, err := db.AcquireAccountContext(ctx, "one")
	if err != nil {
		t.Fatalf("ownership was not released after lock loss: %v", err)
	}
	defer newRelease()
	if err := db.PutMission(newCtx, &Mission{AccountID: "one", ShipID: "new", Phase: "idle"}); err != nil {
		t.Fatal(err)
	}
}

func TestPostgresBootstrapLockAndGenerationFence(t *testing.T) {
	db, _, _ := integrationStore(t)
	ctx := context.Background()
	ownerCtx, release, err := db.AcquireAccountContext(ctx, "bootstrap")
	if err != nil {
		t.Fatal(err)
	}
	defer release()
	if err := db.SaveAccount(ownerCtx, Account{ID: "bootstrap", Label: "Yukine", Enabled: true}, Credentials{Handle: "Yukine", Passphrase: "protected"}); err != nil {
		t.Fatalf("bootstrap ownership could not precede account persistence: %v", err)
	}
	if err := db.db.Exec("UPDATE accounts SET owner_generation = owner_generation + 1 WHERE id = ?", "bootstrap").Error; err != nil {
		t.Fatal(err)
	}
	if err := db.PutMission(ownerCtx, &Mission{AccountID: "bootstrap", ShipID: "ship", Phase: "idle"}); !errors.Is(err, ErrOwnershipLost) {
		t.Fatalf("old generation was not fenced: %v", err)
	}
}

func TestPostgresBootstrapBindingPreservesRecoveryAndExclusiveOwnership(t *testing.T) {
	db, _, _ := integrationStore(t)
	ctx := context.Background()
	ownerCtx, release, err := db.AcquireAccountContext(ctx, "bootstrap")
	if err != nil {
		t.Fatal(err)
	}
	defer release()
	account := Account{ID: "bootstrap", Label: "Yukine", Enabled: true}
	credentials := Credentials{Handle: "Yukine", Passphrase: "protected bootstrap passphrase"}
	if err := db.SaveAccount(ownerCtx, account, credentials); err != nil {
		t.Fatal(err)
	}
	intent := &Action{
		ID: "bootstrap-company", AccountID: account.ID, Method: "POST",
		Path: "/v1/company/create", Body: json.RawMessage(`{"name":"The Pokemon Pirates"}`),
	}
	if _, err := db.BeginAction(ownerCtx, intent); err != nil {
		t.Fatal(err)
	}
	account.CompanyID = "new-company"
	if err := db.SaveAccount(ownerCtx, account, credentials); !errors.Is(err, ErrActionConflict) {
		t.Fatalf("pending intent was rebound to a different company: %v", err)
	}
	response := json.RawMessage(`{"company_id":"new-company"}`)
	if err := db.CompleteAction(ownerCtx, intent.ID, 201, response, nil); err != nil {
		t.Fatal(err)
	}
	if err := db.SaveAccount(ownerCtx, account, credentials); err != nil {
		t.Fatal(err)
	}
	recovered, err := db.BeginAction(ownerCtx, intent)
	if err != nil || recovered.State != ActionComplete || !bytes.Equal(recovered.Response, response) {
		t.Fatalf("bound company could not recover completed bootstrap result: %+v, %v", recovered, err)
	}
	if _, duplicateRelease, err := db.AcquireAccountContext(ctx, account.ID); !errors.Is(err, ErrAccountLocked) {
		if duplicateRelease != nil {
			duplicateRelease()
		}
		t.Fatalf("company binding bypassed the active bootstrap account lock: %v", err)
	}
}

func TestPostgresDashboardEmptyArraysAndHistoryBounds(t *testing.T) {
	db, _, _ := integrationStore(t)
	ctx := context.Background()
	dashboard, err := db.Dashboard(ctx)
	if err != nil {
		t.Fatal(err)
	}
	encoded, err := json.Marshal(dashboard)
	if err != nil {
		t.Fatal(err)
	}
	for _, field := range []string{"accounts", "metrics", "events", "decisions", "requests", "settlements"} {
		if !strings.Contains(string(encoded), `"`+field+`":[]`) {
			t.Fatalf("empty %s is not an array: %s", field, encoded)
		}
	}
	for _, limit := range []int{-1, 501} {
		if _, err := db.History(ctx, "", "events", limit); !errors.Is(err, ErrInvalidInput) {
			t.Fatalf("unbounded history accepted limit %d: %v", limit, err)
		}
	}
	if _, err := db.History(ctx, "", "actions", 10); !errors.Is(err, ErrInvalidInput) {
		t.Fatalf("private action journal exposed through history: %v", err)
	}
}
