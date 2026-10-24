package store

import (
	"bytes"
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"testing"
	"time"

	"go.uber.org/zap"
)

type privateLegacyEvidence struct {
	account     string
	credentials Credentials
	mission     Mission
	settlement  Settlement
	policy      UnderwritingPolicyRecord
	position    UnderwritingPosition
	closed      UnderwritingClosure
	pending     Action
	pendingRow  actionRow
}

func seedPrivateLegacyEvidence(t *testing.T, database *Store) privateLegacyEvidence {
	t.Helper()
	ctx := context.Background()
	value := privateLegacyEvidence{account: "private-migration"}
	value.credentials = saveTestAccount(t, database, value.account)
	shipID := "ship-" + value.credentials.APIKey
	body := json.RawMessage(fmt.Sprintf("{ \"ship_id\" : %q, \"quantity\" : 4, \"future\" : 9007199254740993 }", shipID))
	value.pending = Action{
		ID: "migration-pending", AccountID: value.account, Method: "POST", Path: "/v1/market/place-order", Body: body,
	}
	if _, err := database.BeginAction(ctx, &value.pending); err != nil {
		t.Fatal(err)
	}
	if err := database.db.Where("id = ?", value.pending.ID).Take(&value.pendingRow).Error; err != nil {
		t.Fatal(err)
	}
	value.mission = Mission{
		AccountID: value.account, ShipID: shipID, Phase: "buy", Generation: "unchanged-legacy-generation",
		Destination: value.credentials.Passphrase, Commodity: value.credentials.APIKey,
		ContractID: "original-contract", Quantity: 4, PricePence: 9_007_199_254_740_993,
		Data:      json.RawMessage(fmt.Sprintf(`{"unknown_recovery":{"integer":9007199254740993},"action":{"id":"migration-pending","method":"POST","path":"/v1/market/place-order","body":%s,"before":{"ship":{"id":%q}},"unknown_attempt":"retained"},"built_ship_id":%q}`, body, shipID, shipID)),
		UpdatedAt: time.Now().UTC().Add(-30 * 24 * time.Hour).Truncate(time.Microsecond),
	}
	if err := database.db.Create(&value.mission).Error; err != nil {
		t.Fatal(err)
	}
	value.mission.Data = legacyMissionJSON(t, database, value.account, shipID)
	policy := privatePolicy(t, "private-policy", value.credentials.APIKey, value.credentials.Passphrase)
	value.position = privatePosition(t, database, value.account, "receipt-"+value.credentials.APIKey, policy)
	if err := database.db.Create(&value.position).Error; err != nil {
		t.Fatal(err)
	}
	value.policy = UnderwritingPolicyRecord{
		AccountID: value.account, PolicyID: "private-policy", TermsHash: value.credentials.Passphrase,
		Status: "active", Payload: policy, SourceUpdatedAt: value.position.AcceptedAt,
		ObservedAt: value.position.AcceptedAt, TermsChanged: true,
	}
	if err := database.db.Create(&value.policy).Error; err != nil {
		t.Fatal(err)
	}
	var policyText string
	if err := database.db.Raw("SELECT payload::text FROM underwriting_policies WHERE account_id = ? AND policy_id = ?", value.account, value.policy.PolicyID).Row().Scan(&policyText); err != nil {
		t.Fatal(err)
	}
	value.policy.Payload = json.RawMessage(policyText)
	var frozenText string
	if err := database.db.Raw("SELECT frozen_policy::text FROM underwriting_positions WHERE account_id = ? AND id = ?", value.account, value.position.ID).Row().Scan(&frozenText); err != nil {
		t.Fatal(err)
	}
	value.position.FrozenPolicy = json.RawMessage(frozenText)
	closedPosition := privatePosition(t, database, value.account, "closed-"+value.credentials.Passphrase, policy)
	closedAt := closedPosition.AcceptedAt.Add(time.Minute)
	earned, claim, returned, fees, net := int64(9000), int64(30000), int64(0), int64(1000), int64(-22000)
	closedPosition.Status = "claimed_loss"
	closedPosition.EarnedPremiumPence = &earned
	closedPosition.ClaimsPence = &claim
	closedPosition.ReturnedCollateralPence = &returned
	closedPosition.FeesPence = &fees
	closedPosition.NetPence = &net
	closedPosition.ClosedAt = &closedAt
	closedPosition.FundingEvidence = json.RawMessage(`{"banking_region":"north","entry":{"proof":"[REDACTED]"}}`)
	value.closed = UnderwritingClosure{
		AccountID: value.account, ID: closedPosition.ID, Status: closedPosition.Status,
		EarnedPremiumPence: earned, ClaimsPence: claim, ReturnedCollateralPence: returned,
		FeesPence: fees, ClosedAt: closedAt, Evidence: json.RawMessage(`{"status":"claimed_loss","proof":"[REDACTED]"}`),
	}
	closedRow := struct {
		UnderwritingPosition `gorm:"embedded"`
		ClosureEvidence      json.RawMessage `gorm:"type:jsonb"`
	}{UnderwritingPosition: closedPosition, ClosureEvidence: value.closed.Evidence}
	if err := database.db.Table("underwriting_positions").Create(&closedRow).Error; err != nil {
		t.Fatal(err)
	}
	if err := database.db.Exec("INSERT INTO underwriting_budgets (account_id, initial_pence) VALUES (?, ?)", value.account, 100000).Error; err != nil {
		t.Fatal(err)
	}
	value.settlement = Settlement{
		ID: "migration-settlement", AccountID: value.account, ShipID: shipID, MissionID: value.mission.Generation,
		ActionID: value.position.ActionID, Lane: value.credentials.APIKey + "|" + value.credentials.Passphrase,
		Kind: "spot", Origin: value.credentials.APIKey, Destination: value.credentials.Passphrase, Commodity: value.credentials.APIKey,
		QuantityTons: 4, GameSeconds: 3600, CostsKnown: true,
		ProceedsPence: 9_007_199_254_740_993, AcquisitionPence: 100, FeesPence: 25,
		ProvisionsPence: 20, ExpensesPence: 5,
		StartedAt: value.position.AcceptedAt, CompletedAt: value.position.AcceptedAt.Add(time.Hour),
		CreatedAt: value.position.AcceptedAt.Add(time.Hour),
	}
	if err := database.db.Create(&value.settlement).Error; err != nil {
		t.Fatal(err)
	}
	secrets := credentialSecrets(value.credentials)
	details, err := redactJSON(json.RawMessage(fmt.Sprintf(`{"identity":%q}`, shipID)), secrets, maxDiagnosticBytes)
	if err != nil {
		t.Fatal(err)
	}
	decision := Decision{
		AccountID: value.account, ShipID: shipID, Kind: scrubText("hold-"+value.credentials.APIKey, secrets),
		Reason: scrubText(value.credentials.Passphrase, secrets), Details: details,
		CreatedAt: time.Now().UTC(),
	}
	if err := database.db.Create(&decision).Error; err != nil {
		t.Fatal(err)
	}
	return value
}

func legacyMissionJSON(t *testing.T, database *Store, accountID, shipID string) json.RawMessage {
	t.Helper()
	var raw string
	if err := database.db.Raw("SELECT data::text FROM missions WHERE account_id = ? AND ship_id = ?", accountID, shipID).Row().Scan(&raw); err != nil {
		t.Fatal(err)
	}
	return json.RawMessage(raw)
}

func assertLegacyRecoveryIntact(t *testing.T, database *Store, evidence privateLegacyEvidence) {
	t.Helper()
	var version int64
	if err := database.db.Raw("SELECT MAX(version_id) FROM goose_db_version WHERE is_applied").Scan(&version).Error; err != nil || version != 4 {
		t.Fatalf("failed cutover advanced its version: %d, %v", version, err)
	}
	var mission Mission
	if err := database.db.Table("missions").Select("account_id, ship_id, phase, destination, commodity, contract_id, quantity, price_pence, generation, updated_at").Where("account_id = ? AND ship_id = ?", evidence.account, evidence.mission.ShipID).Take(&mission).Error; err != nil {
		t.Fatal(err)
	}
	mission.Data = legacyMissionJSON(t, database, evidence.account, evidence.mission.ShipID)
	if !bytes.Equal(mission.Data, evidence.mission.Data) || mission.Generation != evidence.mission.Generation || mission.PricePence != evidence.mission.PricePence {
		t.Fatal("failed cutover changed original recovery")
	}
	var action actionRow
	if err := database.db.Where("id = ?", evidence.pending.ID).Take(&action).Error; err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(action.BodyCipher, evidence.pendingRow.BodyCipher) || !action.CreatedAt.Equal(evidence.pendingRow.CreatedAt) {
		t.Fatal("failed cutover changed original immutable action")
	}
	var triggers int64
	if err := database.db.Raw(`SELECT COUNT(*) FROM pg_trigger WHERE tgrelid IN ('underwriting_policies'::regclass, 'underwriting_positions'::regclass) AND NOT tgisinternal`).Scan(&triggers).Error; err != nil || triggers != 2 {
		t.Fatalf("failed cutover did not restore immutability triggers: %d, %v", triggers, err)
	}
	var columns int64
	if err := database.db.Raw(`SELECT COUNT(*) FROM information_schema.columns WHERE table_schema = current_schema() AND column_name IN ('ship_lookup', 'policy_lookup', 'position_lookup', 'context_cipher')`).Scan(&columns).Error; err != nil || columns != 0 {
		t.Fatalf("failed cutover left a partial private layout: %d, %v", columns, err)
	}
}

func TestPrivateMigrationFailureAndCancellationRollbackOriginalRecovery(t *testing.T) {
	for _, mode := range []string{"wrong-key", "active-account", "active-company", "forced-failure", "cancellation"} {
		t.Run(mode, func(t *testing.T) {
			database, dsn, key := privateLegacyStore(t)
			evidence := seedPrivateLegacyEvidence(t, database)
			ctx := context.Background()
			migrationKey := key
			var lock *sql.Conn
			if mode == "wrong-key" {
				migrationKey = bytes.Repeat([]byte{0x43}, 32)
			}
			if mode == "active-account" || mode == "active-company" {
				var err error
				lock, err = database.sqlDB.Conn(ctx)
				if err != nil {
					t.Fatal(err)
				}
				defer lock.Close()
				identity := "tradewinds:runner:account:" + evidence.account
				if mode == "active-company" {
					identity = "tradewinds:runner:company:company-" + evidence.account
				}
				digest := sha256.Sum256([]byte(identity))
				if _, err := lock.ExecContext(ctx, "SELECT pg_advisory_lock($1)", int64(binary.BigEndian.Uint64(digest[:8]))); err != nil {
					t.Fatal(err)
				}
			}
			if mode == "forced-failure" || mode == "cancellation" {
				body := "RAISE EXCEPTION 'forced migration failure';"
				if mode == "cancellation" {
					body = "PERFORM set_config('application_name', 'tradewinds-private-cutover-cancellation', true); PERFORM pg_sleep(10); RETURN NEW;"
				}
				if err := database.db.Exec("CREATE FUNCTION force_private_failure() RETURNS trigger LANGUAGE plpgsql AS $$ BEGIN " + body + " END; $$; CREATE TRIGGER force_private_failure BEFORE UPDATE ON missions FOR EACH ROW EXECUTE FUNCTION force_private_failure()").Error; err != nil {
					t.Fatal(err)
				}
			}
			var opened *Store
			var err error
			if mode == "cancellation" {
				opened, err = cancelPrivateMigrationAtPause(t, database, dsn, migrationKey)
			} else {
				opened, err = Open(ctx, dsn, migrationKey, zap.NewNop())
			}
			if err == nil {
				_ = opened.Close()
				t.Fatal("unsafe/failed migration committed")
			}
			assertLegacyRecoveryIntact(t, database, evidence)
			if lock != nil {
				if _, err := lock.ExecContext(ctx, "SELECT pg_advisory_unlock_all()"); err != nil {
					t.Fatal(err)
				}
			}
			if mode == "forced-failure" || mode == "cancellation" {
				if err := database.db.Exec("DROP TRIGGER force_private_failure ON missions; DROP FUNCTION force_private_failure()").Error; err != nil {
					t.Fatal(err)
				}
			}
			recovered, err := Open(ctx, dsn, key, zap.NewNop())
			if err != nil {
				t.Fatalf("restored prerequisite did not permit normal cutover: %v", err)
			}
			defer recovered.Close()
			mission, err := recovered.GetMission(ctx, evidence.account, evidence.mission.ShipID)
			if err != nil || mission == nil {
				t.Fatalf("retry lost original recovery: %+v, %v", mission, err)
			}
			assertMigratedMissionBody(t, recovered, evidence.mission.Data, mission.Data, evidence.pending.Body)
		})
	}
}

func TestPrivateMigrationPreservesRawEvidenceMoneyAndImmutableReplay(t *testing.T) {
	legacy, dsn, key := privateLegacyStore(t)
	evidence := seedPrivateLegacyEvidence(t, legacy)
	ctx := context.Background()
	database, err := Open(ctx, dsn, key, zap.NewNop())
	if err != nil {
		t.Fatal(err)
	}
	defer database.Close()
	assertPrivateAtRest(t, database, evidence.credentials.APIKey, evidence.credentials.Passphrase)
	assertPrivateDisplays(t, database, evidence.account, evidence.credentials.APIKey, evidence.credentials.Passphrase)
	var plaintextColumns int64
	if err := database.db.Raw(`WITH removed(table_name, column_name) AS (VALUES
		('missions', 'ship_id'), ('missions', 'phase'), ('missions', 'destination'),
		('missions', 'commodity'), ('missions', 'contract_id'), ('missions', 'quantity'),
		('missions', 'price_pence'), ('missions', 'generation'), ('missions', 'data'),
		('settlements', 'ship_id'), ('settlements', 'lane'), ('settlements', 'origin'),
		('settlements', 'destination'), ('settlements', 'commodity'),
		('underwriting_policies', 'policy_id'), ('underwriting_policies', 'terms_hash'), ('underwriting_policies', 'payload'),
		('underwriting_positions', 'id'), ('underwriting_positions', 'policy_id'), ('underwriting_positions', 'company_id'), ('underwriting_positions', 'frozen_policy')
	)
	SELECT COUNT(*) FROM information_schema.columns JOIN removed USING (table_name, column_name)
	WHERE table_schema = current_schema()`).Scan(&plaintextColumns).Error; err != nil || plaintextColumns != 0 {
		t.Fatalf("cutover retained logical plaintext source columns: %d, %v", plaintextColumns, err)
	}
	mission, err := database.GetMission(ctx, evidence.account, evidence.mission.ShipID)
	if err != nil || mission == nil || mission.Generation != evidence.mission.Generation || mission.PricePence != evidence.mission.PricePence || !mission.UpdatedAt.Equal(evidence.mission.UpdatedAt) {
		t.Fatalf("legacy raw mission recovery changed: %+v, %v", mission, err)
	}
	assertMigratedMissionBody(t, database, evidence.mission.Data, mission.Data, evidence.pending.Body)
	exerciseMigratedCooldownSave(t, database, dsn, key, evidence, mission)
	pending, err := database.BeginAction(ctx, &evidence.pending)
	if err != nil || !bytes.Equal(pending.Body, evidence.pending.Body) || !pending.CreatedAt.Equal(evidence.pendingRow.CreatedAt) {
		t.Fatalf("legacy replay identity/window changed: %+v, %v", pending, err)
	}
	var action actionRow
	if err := database.db.Where("id = ?", evidence.pending.ID).Take(&action).Error; err != nil || !bytes.Equal(action.BodyCipher, evidence.pendingRow.BodyCipher) {
		t.Fatal("cutover regenerated immutable action ciphertext")
	}
	settlements, err := database.SettlementHistory(ctx, "", evidence.settlement.Lane, 100)
	if err != nil || len(settlements) != 1 || !sameSettlement(settlements[0], evidence.settlement) || settlements[0].ExplorationExpensePence != nil {
		t.Fatalf("legacy accounting identity/nullable expense changed: %+v, %v", settlements, err)
	}
	history, err := database.EconomicHistory(ctx, evidence.account, evidence.settlement.Lane)
	if err != nil || history.NetPence != 9_007_199_254_740_843 || history.PositiveCycles != 1 || history.PositiveQuantityTons != 4 {
		t.Fatalf("cutover rounded/reclassified exact permanent accounting: %+v, %v", history, err)
	}
	policies, err := database.UnderwritingPolicies(ctx, evidence.account)
	if err != nil || len(policies) != 1 || policies[0].TermsHash != evidence.policy.TermsHash || !policies[0].TermsChanged || !bytes.Equal(policies[0].Payload, evidence.policy.Payload) {
		t.Fatalf("legacy original policy fingerprint/payload changed: %+v, %v", policies, err)
	}
	positions, err := database.UnderwritingPositions(ctx, evidence.account)
	if err != nil || len(positions) != 2 {
		t.Fatalf("legacy invalid receipt identities were lost: %+v, %v", positions, err)
	}
	var active UnderwritingPosition
	for _, position := range positions {
		if position.ID == evidence.position.ID {
			active = position
		}
	}
	if active.ID == "" || !bytes.Equal(active.FrozenPolicy, evidence.position.FrozenPolicy) || active.EarnedPremiumPence != nil || active.ClosedAt != nil {
		t.Fatal("legacy immutable frozen evidence or nullable money changed")
	}
	var before underwritingPositionRow
	lookup := database.privateLookup("underwriting-position", evidence.account, active.ID)
	if err := database.db.Where("account_id = ? AND position_lookup = ?", evidence.account, lookup).Take(&before).Error; err != nil {
		t.Fatal(err)
	}
	var policyBefore underwritingPolicyRow
	policyLookup := database.privateLookup("underwriting-policy", evidence.account, evidence.policy.PolicyID)
	if err := database.db.Where("account_id = ? AND policy_lookup = ?", evidence.account, policyLookup).Take(&policyBefore).Error; err != nil {
		t.Fatal(err)
	}
	if err := database.PutUnderwritingPolicies(ctx, evidence.account, []UnderwritingPolicyRecord{evidence.policy}); err != nil {
		t.Fatal(err)
	}
	var policyAfter underwritingPolicyRow
	if err := database.db.Where("account_id = ? AND policy_lookup = ?", evidence.account, policyLookup).Take(&policyAfter).Error; err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(policyBefore.IdentityCipher, policyAfter.IdentityCipher) {
		t.Fatal("policy observation replay regenerated the first immutable identity/fingerprint")
	}
	var closedBefore underwritingPositionRow
	closedLookup := database.privateLookup("underwriting-position", evidence.account, evidence.closed.ID)
	if err := database.db.Where("account_id = ? AND position_lookup = ?", evidence.account, closedLookup).Take(&closedBefore).Error; err != nil {
		t.Fatal(err)
	}
	if err := database.PutUnderwritingPosition(ctx, &evidence.position); err != nil {
		t.Fatalf("identical legacy invalid-ID replay did not retain its encrypted acceptance: %v", err)
	}
	if err := database.CloseUnderwritingPosition(ctx, evidence.closed); err != nil {
		t.Fatalf("legacy closure replay changed JSONB evidence semantics: %v", err)
	}
	var closedAfter underwritingPositionRow
	if err := database.db.Where("account_id = ? AND position_lookup = ?", evidence.account, closedLookup).Take(&closedAfter).Error; err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(closedBefore.IdentityCipher, closedAfter.IdentityCipher) || !bytes.Equal(closedBefore.FrozenCipher, closedAfter.FrozenCipher) ||
		!bytes.Equal(closedBefore.FundingEvidence, closedAfter.FundingEvidence) || !bytes.Equal(closedBefore.ClosureEvidence, closedAfter.ClosureEvidence) {
		t.Fatal("closure replay changed immutable acceptance/funding/closure evidence")
	}
	var after underwritingPositionRow
	if err := database.db.Where("account_id = ? AND position_lookup = ?", evidence.account, lookup).Take(&after).Error; err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(before.IdentityCipher, after.IdentityCipher) || !bytes.Equal(before.FrozenCipher, after.FrozenCipher) {
		t.Fatal("identical acceptance replay regenerated random ciphertext")
	}
	for _, field := range []string{"identity_cipher", "frozen_cipher"} {
		ciphertext := before.IdentityCipher
		if field == "frozen_cipher" {
			ciphertext = before.FrozenCipher
		}
		changed := append([]byte(nil), ciphertext...)
		changed[len(changed)-1] ^= 1
		if err := database.db.Model(&underwritingPositionRow{}).Where("account_id = ? AND position_lookup = ?", evidence.account, lookup).Update(field, changed).Error; err == nil {
			t.Fatalf("cutover lost %s immutability", field)
		}
	}
	for _, field := range []string{"funding_evidence", "closure_evidence"} {
		if err := database.db.Model(&underwritingPositionRow{}).Where("account_id = ? AND position_lookup = ?", evidence.account, closedLookup).Update(field, json.RawMessage(`{"changed":true}`)).Error; err == nil {
			t.Fatalf("cutover lost immutable %s", field)
		}
	}
	if err := database.db.Exec("UPDATE underwriting_budgets SET initial_pence = initial_pence + 1 WHERE account_id = ?", evidence.account).Error; err == nil {
		t.Fatal("cutover lost the immutable pilot seed")
	}
	risk, err := database.UnderwritingRisk(ctx, evidence.account)
	if err != nil || risk.InitialPence != 100000 || risk.OutstandingPence != 30000 || risk.RealizedNetPence != -22000 || risk.RemainingPence != 48000 {
		t.Fatalf("cutover reset budget or changed exact collateral/closure totals: %+v, %v", risk, err)
	}
	if err := database.Prune(ctx); err != nil {
		t.Fatal(err)
	}
	if err := database.PutSettlement(ctx, &evidence.settlement); err != nil {
		t.Fatalf("legacy immutable settlement replay changed: %v", err)
	}
	changed := evidence.settlement
	changed.Lane += "different"
	if err := database.PutSettlement(ctx, &changed); !errors.Is(err, ErrActionConflict) {
		t.Fatalf("original encrypted lane equality was replaced by a tag-only merge: %v", err)
	}
}

func cancelPrivateMigrationAtPause(t *testing.T, legacy *Store, dsn string, key []byte) (*Store, error) {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	type result struct {
		database *Store
		err      error
	}
	completed := make(chan result, 1)
	go func() {
		database, err := Open(ctx, dsn, key, zap.NewNop())
		completed <- result{database: database, err: err}
	}()
	ticker := time.NewTicker(10 * time.Millisecond)
	defer ticker.Stop()
	deadline := time.NewTimer(5 * time.Second)
	defer deadline.Stop()
	for {
		select {
		case outcome := <-completed:
			if outcome.database != nil {
				_ = outcome.database.Close()
			}
			t.Fatalf("migration ended before the post-DDL cancellation boundary: %v", outcome.err)
		case <-deadline.C:
			t.Fatal("migration never reached its post-DDL row-update pause")
		case <-ticker.C:
			var paused bool
			if err := legacy.db.Raw(`SELECT EXISTS (
				SELECT 1 FROM pg_stat_activity
				WHERE datname = current_database()
				  AND application_name = 'tradewinds-private-cutover-cancellation'
				  AND wait_event = 'PgSleep'
			)`).Scan(&paused).Error; err != nil {
				t.Fatal(err)
			}
			if paused {
				cancel()
				select {
				case outcome := <-completed:
					return outcome.database, outcome.err
				case <-time.After(5 * time.Second):
					t.Fatal("paused migration did not honor cancellation")
				}
			}
		}
	}
}

func TestPrivateMigrationPreservesLegacyUntrustedClassificationWithoutCredit(t *testing.T) {
	legacy, dsn, key := privateLegacyStore(t)
	evidence := seedPrivateLegacyEvidence(t, legacy)
	kind := "kind-" + evidence.credentials.APIKey
	policyStatus := "policy-" + evidence.credentials.Passphrase
	positionStatus := "position-" + evidence.credentials.APIKey
	if err := legacy.db.Exec("UPDATE settlements SET kind = ? WHERE id = ?", kind, evidence.settlement.ID).Error; err != nil {
		t.Fatal(err)
	}
	if err := legacy.db.Exec("UPDATE underwriting_policies SET status = ? WHERE account_id = ? AND policy_id = ?", policyStatus, evidence.account, evidence.policy.PolicyID).Error; err != nil {
		t.Fatal(err)
	}
	if err := legacy.db.Exec("UPDATE underwriting_positions SET status = ? WHERE account_id = ? AND id = ?", positionStatus, evidence.account, evidence.position.ID).Error; err != nil {
		t.Fatal(err)
	}
	database, err := Open(context.Background(), dsn, key, zap.NewNop())
	if err != nil {
		t.Fatal(err)
	}
	defer database.Close()
	assertPrivateAtRest(t, database, evidence.credentials.APIKey, evidence.credentials.Passphrase)
	assertPrivateDisplays(t, database, evidence.account, evidence.credentials.APIKey, evidence.credentials.Passphrase)
	settlements, err := database.SettlementHistory(context.Background(), evidence.account, evidence.settlement.Lane, 100)
	if err != nil || len(settlements) != 1 || settlements[0].Kind != kind {
		t.Fatalf("legacy original classification was discarded: %+v, %v", settlements, err)
	}
	policies, err := database.UnderwritingPolicies(context.Background(), evidence.account)
	if err != nil || len(policies) != 1 || policies[0].Status != policyStatus {
		t.Fatalf("legacy original policy status was discarded: %+v, %v", policies, err)
	}
	positions, err := database.UnderwritingPositions(context.Background(), evidence.account)
	if err != nil {
		t.Fatal(err)
	}
	found := false
	for _, position := range positions {
		if position.ID == evidence.position.ID {
			found = position.Status == positionStatus
		}
	}
	if !found {
		t.Fatal("legacy original receipt status was discarded")
	}
	history, err := database.EconomicHistory(context.Background(), evidence.account, evidence.settlement.Lane)
	if err != nil || !history.UnknownCosts || history.PositiveCycles != 0 || history.PositiveQuantityTons != 0 || history.PositiveNetPence != 0 {
		t.Fatalf("untrusted classification became positive delivery proof: %+v, %v", history, err)
	}
}

func TestPrivateMigrationSkipsSafeDiagnosticHistoryAndBatchesChangedIdentity(t *testing.T) {
	legacy, dsn, key := privateLegacyStore(t)
	evidence := seedPrivateLegacyEvidence(t, legacy)
	if err := legacy.db.Exec(`INSERT INTO decisions (account_id, ship_id, kind, reason, details)
		SELECT ?, 'safe-ship', 'hold', 'safe reason', '{"stage":"waiting"}'::jsonb
		FROM generate_series(1, 2000)`, evidence.account).Error; err != nil {
		t.Fatal(err)
	}
	if err := legacy.db.Exec(`CREATE TABLE diagnostic_migration_updates (ship_id text NOT NULL);
		CREATE FUNCTION guard_safe_diagnostic_history() RETURNS trigger LANGUAGE plpgsql AS $$
		BEGIN
			IF OLD.ship_id = 'safe-ship' THEN
				RAISE EXCEPTION 'safe historical diagnostics must not be rewritten';
			END IF;
			INSERT INTO diagnostic_migration_updates (ship_id) VALUES (OLD.ship_id);
			RETURN NEW;
		END;
		$$;
		CREATE TRIGGER guard_safe_diagnostic_history BEFORE UPDATE ON decisions
			FOR EACH ROW EXECUTE FUNCTION guard_safe_diagnostic_history()`).Error; err != nil {
		t.Fatal(err)
	}
	database, err := Open(context.Background(), dsn, key, zap.NewNop())
	if err != nil {
		t.Fatalf("safe diagnostic history was needlessly rewritten: %v", err)
	}
	defer database.Close()
	var changed, retained int64
	if err := database.db.Table("diagnostic_migration_updates").Count(&changed).Error; err != nil || changed != 1 {
		t.Fatalf("changed diagnostic identity was not sanitized exactly once: %d, %v", changed, err)
	}
	if err := database.db.Table("decisions").Where("ship_id = ?", "safe-ship").Count(&retained).Error; err != nil || retained != 2000 {
		t.Fatalf("safe diagnostic history changed: %d, %v", retained, err)
	}
	assertPrivateAtRest(t, database, evidence.credentials.APIKey, evidence.credentials.Passphrase)
}
