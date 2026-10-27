package store

import (
	"bytes"
	"compress/gzip"
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"math/rand/v2"
	"sort"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgconn"
	"go.uber.org/zap"
	"go.uber.org/zap/zaptest/observer"
	"gorm.io/gorm"
)

func TestReplayManifestSurvivesFrameIDReassignment(t *testing.T) {
	db, _, _ := integrationStore(t)
	saveTestAccount(t, db, "reassigned")
	frame := replayAcceptanceFrame("reassigned", "portable-generation", time.Now().UTC(), []byte(`{"port_id":"private-port"}`))
	id := replayAcceptanceRecord(t, db, frame)
	if err := db.db.Exec("UPDATE replay_frames SET id = id + 1000 WHERE id = ?", id).Error; err != nil {
		t.Fatal(err)
	}
	frames := replayAcceptanceFrames(t, db, "reassigned")
	if len(frames) != 1 || frames[0].ID != id+1000 || !bytes.Equal(frames[0].Datasets["books/private-port"].Data, frame.Datasets["books/private-port"].Data) {
		t.Fatalf("frame ID entered manifest AAD: %+v", frames)
	}
}

func TestReplayPlanPrivateContextAndManifestGzip(t *testing.T) {
	db, _, _ := integrationStore(t)
	saveTestAccount(t, db, "private-context")
	ctx := context.Background()
	plan := ReplayPlan{AccountID: "private-context", MissionID: "mission", ShipID: "private-ship",
		Phase: "private-phase", Generation: "generation", ObservedAt: time.Now().UTC(),
		Plan: json.RawMessage(`{"commodity":"private-commodity"}`), Policy: json.RawMessage(`{"reserve":100}`), Revision: "revision"}
	if err := db.RecordReplayPlan(ctx, plan); err != nil {
		t.Fatal(err)
	}
	var row replayPlanRow
	if err := db.db.First(&row).Error; err != nil {
		t.Fatal(err)
	}
	for _, secret := range []string{plan.ShipID, plan.Phase, "private-commodity"} {
		if bytes.Contains(row.PlanCipher, []byte(secret)) {
			t.Fatalf("plan context contains plaintext %s", secret)
		}
	}
	var columns int64
	if err := db.db.Raw(`SELECT count(*) FROM information_schema.columns WHERE table_schema = current_schema()
		AND table_name = 'replay_plans' AND column_name IN ('ship_id', 'phase', 'plan', 'policy')`).Scan(&columns).Error; err != nil || columns != 0 {
		t.Fatalf("private plan context has plaintext columns: %d, %v", columns, err)
	}
	id := replayAcceptanceRecord(t, db, replayAcceptanceFrame("private-context", "compressed", time.Now().UTC(), []byte(`{"book":1}`)))
	var frame replayFrameRow
	if err := db.db.First(&frame, id).Error; err != nil {
		t.Fatal(err)
	}
	aad, err := replayManifestAAD(frame)
	if err != nil {
		t.Fatal(err)
	}
	compressed, err := db.decrypt(frame.ManifestCipher, aad)
	if err != nil {
		t.Fatal(err)
	}
	plain, err := replayDecompress(compressed)
	if err != nil || !json.Valid(plain) {
		t.Fatalf("manifest is not encrypted gzip JSON: %v", err)
	}
}

func TestReplayDiagnosticBatchesPreserveRequestStartOrder(t *testing.T) {
	db, _, _ := integrationStore(t)
	const accountID = "batched-starts"
	saveTestAccount(t, db, accountID)
	ctx := context.Background()
	at := time.Date(2026, 10, 26, 12, 0, 0, 0, time.UTC)
	if err := db.db.Exec(`INSERT INTO requests (account_id, method, path, status, duration_ms, request, response, created_at)
		SELECT ?, 'GET', '/v1/company/profile', 200, 10, '{}', '{"company_id":"company"}',
		CAST(? AS timestamptz) + interval '10 seconds' + n * interval '1 millisecond'
		FROM generate_series(1, 5000) AS n`, accountID, at).Error; err != nil {
		t.Fatal(err)
	}
	if err := db.db.Exec(`INSERT INTO requests (account_id, method, path, status, duration_ms, request, response, created_at)
		VALUES (?, 'GET', '/v1/world/clock', 200, 15000, '{}', '{"total_game_hours":100}', ?)`,
		accountID, at.Add(16*time.Second)).Error; err != nil {
		t.Fatal(err)
	}
	boundary, err := db.ReplayBackfillBoundary(ctx, accountID)
	if err != nil {
		t.Fatal(err)
	}
	if boundary.MaxDuration != 15*time.Second {
		t.Fatalf("frozen maximum duration = %s", boundary.MaxDuration)
	}
	var previous ReplayDiagnostic
	count := 0
	err = db.ScanReplayDiagnostics(ctx, accountID, boundary, func(row ReplayDiagnostic) error {
		if count == 0 && (row.Path != "/v1/world/clock" || !row.StartedAt.Equal(at.Add(time.Second))) {
			t.Fatalf("long request crossing the completion batch was emitted late: %+v", row)
		}
		if count > 0 && (row.StartedAt.Before(previous.StartedAt) ||
			row.StartedAt.Equal(previous.StartedAt) && row.ID <= previous.ID) {
			t.Fatalf("request start ordering regressed at row %d: %+v after %+v", count, row, previous)
		}
		previous = row
		count++
		return nil
	})
	if err != nil || count != 5001 {
		t.Fatalf("scan emitted %d requests: %v", count, err)
	}
}

func TestReplayRedactionRetainsExactSafeBytes(t *testing.T) {
	for _, raw := range [][]byte{
		[]byte("{\n \"count\":9007199254740993, \"duplicate\": 1, \"duplicate\": 2\n}"),
		[]byte("opaque public response\n"),
		[]byte(`["public", true, 1]`),
		[]byte(`"public"`),
		[]byte(`9007199254740993`),
		[]byte("public response after 42\n"),
	} {
		clean, err := replayRedactJSON(raw, []string{"known-canary"})
		if err != nil || !bytes.Equal(clean, raw) {
			t.Fatalf("safe bytes changed: %q, %v", clean, err)
		}
	}
	for _, raw := range [][]byte{
		[]byte(`{"api_key":"unknown-canary","count":9007199254740993}`),
		[]byte(`{"duplicate":"known-canary","duplicate":"public"}`),
		[]byte(`{"detail":"known\u002dcanary"}`),
		[]byte(`{"known\u002dcanary":"public"}`),
		[]byte(`{"password":"malformed-canary`),
		[]byte("Bearer opaque-canary"),
		[]byte(`{"public":true}{"detail":"known-canary"}`),
		[]byte(`["public", "known-canary"`),
		[]byte(`42 {"detail":"known-canary"}`),
	} {
		clean, err := replayRedactJSON(raw, []string{"known-canary"})
		if err != nil || bytes.Contains(clean, []byte("canary")) {
			t.Fatalf("redaction retained a credential: %q, %v", clean, err)
		}
	}
}

func TestReplayRedactsDatasetsMetadataKeysAndPlanContext(t *testing.T) {
	db, _, _ := integrationStore(t)
	const account = "replay-redaction"
	credentials := saveTestAccount(t, db, account)
	frame := replayAcceptanceFrame(account, "redaction", time.Now().UTC(),
		[]byte(fmt.Sprintf(`{"api_key":"unknown-key","detail":%q,"public":9007199254740993}`, credentials.Passphrase)))
	frame.Datasets["books/private-port"] = ReplayDataset{
		Data: frame.Datasets["books/private-port"].Data,
		Meta: json.RawMessage(fmt.Sprintf(`{"complete":true,"error":%q,"headers":{"x-test":"unknown-header"}}`, credentials.APIKey)),
	}
	frame.Datasets["books/"+credentials.Passphrase] = ReplayDataset{
		Data: []byte(`{"public":true}`), Meta: json.RawMessage(`{"complete":true}`),
	}
	rawClock := json.RawMessage(fmt.Sprintf(`{
		"real_utc":"2026-10-27T01:02:03Z","unlocked_phase":3,"paused":false,
		"extension":{"ship_id":"private-clock-ship","echo":%q},
		"calendar":{"total_game_hours":9007199254740993,"month":10,"season":"private-season","extension":"private-calendar"}
	}`, credentials.Passphrase))
	frame.Clock = rawClock
	frame.Datasets["clock"] = ReplayDataset{Data: rawClock, Meta: json.RawMessage(`{"complete":true}`)}
	id := replayAcceptanceRecord(t, db, frame)
	var row replayFrameRow
	if err := db.db.First(&row, id).Error; err != nil {
		t.Fatal(err)
	}
	for _, hidden := range []string{credentials.Passphrase, "private-clock-ship", "private-season", "private-calendar", "extension"} {
		if bytes.Contains(row.Clock, []byte(hidden)) {
			t.Fatalf("clock extension reached plaintext header: %s", hidden)
		}
	}
	var clock replayPublicClock
	if err := json.Unmarshal(row.Clock, &clock); err != nil || clock.Calendar == nil ||
		clock.Calendar.TotalGameHours == nil || *clock.Calendar.TotalGameHours != 9007199254740993 {
		t.Fatalf("public clock scalars lost precision: %s, %v", row.Clock, err)
	}
	recorded := replayAcceptanceFrames(t, db, account)[0]
	encoded, err := json.Marshal(recorded.Datasets)
	if err != nil {
		t.Fatal(err)
	}
	for _, dataset := range recorded.Datasets {
		for _, secret := range []string{credentials.APIKey, credentials.Passphrase, "unknown-key", "unknown-header"} {
			if bytes.Contains(dataset.Data, []byte(secret)) || bytes.Contains(dataset.Meta, []byte(secret)) {
				t.Fatalf("dataset retained credential %q", secret)
			}
		}
	}
	if bytes.Contains(encoded, []byte(credentials.Passphrase)) ||
		!bytes.Contains(recorded.Datasets["clock"].Data, []byte("private-clock-ship")) {
		t.Fatal("dataset key redaction or encrypted clock extension retention failed")
	}
	plan := ReplayPlan{AccountID: account, MissionID: "mission", Generation: "generation",
		ShipID: credentials.Passphrase, Phase: credentials.APIKey, Revision: "Bearer revision-canary",
		ObservedAt: time.Now().UTC(),
		Plan:       json.RawMessage(fmt.Sprintf(`{"detail":%q,"password":"unknown-plan"}`, credentials.Passphrase)),
		Policy:     json.RawMessage(fmt.Sprintf(`{"api_key":%q}`, credentials.APIKey))}
	if err := db.RecordReplayPlan(context.Background(), plan); err != nil {
		t.Fatal(err)
	}
	plans, err := db.ReplayPlans(context.Background(), account)
	if err != nil || len(plans) != 1 {
		t.Fatalf("read redacted plan: %+v, %v", plans, err)
	}
	private, err := json.Marshal(replayPlanTuple(plans[0]))
	if err != nil {
		t.Fatal(err)
	}
	for _, secret := range []string{credentials.APIKey, credentials.Passphrase, "revision-canary"} {
		if bytes.Contains(private, []byte(secret)) {
			t.Fatalf("plan context retained credential %q", secret)
		}
	}
	for _, raw := range [][]byte{plans[0].Plan, plans[0].Policy} {
		if bytes.Contains(raw, []byte("unknown-plan")) || bytes.Contains(raw, []byte(credentials.Passphrase)) ||
			bytes.Contains(raw, []byte(credentials.APIKey)) {
			t.Fatalf("plan JSON retained credential: %s", raw)
		}
	}
}

func TestReplayRedactionPrecedesDedupeAndFailsClosed(t *testing.T) {
	db, _, _ := integrationStore(t)
	const account = "replay-fail-closed"
	saveTestAccount(t, db, account)
	frame := replayAcceptanceFrame(account, "first", time.Now().UTC(), []byte(`{"api_key":"one","public":1}`))
	if result, err := db.recordReplayFrame(context.Background(), frame); err != nil || result.PayloadsAdded != 1 {
		t.Fatalf("first redacted payload: result=%+v, err=%v", result, err)
	}
	frame.Generation = "second"
	frame.Datasets["books/private-port"] = ReplayDataset{
		Data: []byte(`{"api_key":"two","public":1}`), Meta: json.RawMessage(`{"complete":true}`),
	}
	if result, err := db.recordReplayFrame(context.Background(), frame); err != nil || result.PayloadsAdded != 0 {
		t.Fatalf("equivalent redacted payload was not deduplicated: result=%+v, err=%v", result, err)
	}
	if err := db.db.Model(&accountSecret{}).Where("account_id = ?", account).
		Update("ciphertext", []byte("corrupt")).Error; err != nil {
		t.Fatal(err)
	}
	if _, err := db.RecordReplayFrame(context.Background(), frame); err == nil {
		t.Fatal("frame persisted without reading credentials")
	}
	plan := ReplayPlan{AccountID: account, MissionID: "mission", Plan: json.RawMessage(`{}`), Policy: json.RawMessage(`{}`)}
	if err := db.RecordReplayPlan(context.Background(), plan); err == nil {
		t.Fatal("plan persisted without reading credentials")
	}
	if replayAcceptanceCount(t, db, "replay_frames", account) != 2 ||
		replayAcceptanceCount(t, db, "replay_plans", account) != 0 {
		t.Fatal("failed credential read wrote replay evidence")
	}
}

func TestReplaySemanticHeadersAuthenticatedAndPortable(t *testing.T) {
	db, _, _ := integrationStore(t)
	const account = "authenticated-header"
	saveTestAccount(t, db, account)
	saveTestAccount(t, db, "other-header")
	at := time.Date(2026, 10, 27, 6, 7, 8, 123456789, time.FixedZone("offset", 2*60*60))
	frame := replayAcceptanceFrame(account, "same-generation", at, []byte(`{"book":1}`))
	frame.Clock = json.RawMessage(`{ "paused":false, "calendar":{"total_game_hours":9007199254740993,"month":10}, "unlocked_phase":3 }`)
	id := replayAcceptanceRecord(t, db, frame)
	got := replayAcceptanceFrames(t, db, account)
	if len(got) != 1 || !got[0].ObservedAt.Equal(at.UTC().Truncate(time.Microsecond)) {
		t.Fatalf("timestamp/JSONB normalization broke header authentication: %+v", got)
	}
	var original replayFrameRow
	if err := db.db.First(&original, id).Error; err != nil {
		t.Fatal(err)
	}
	for _, test := range []struct {
		column  string
		value   any
		restore any
		account string
	}{
		{"account_id", "other-header", account, "other-header"},
		{"capture_id", "tampered-capture", original.CaptureID, account},
		{"generation", "tampered-generation", original.Generation, account},
		{"source", "diagnostic_backfill", original.Source, account},
		{"observed_at", original.ObservedAt.Add(time.Second), original.ObservedAt, account},
		{"clock", `{"unlocked_phase":4}`, string(original.Clock), account},
		{"phase", original.Phase + 1, original.Phase, account},
		{"catalog_digest", "tampered-catalog", original.CatalogDigest, account},
		{"decision", !original.Decision, original.Decision, account},
		{"assumed_catalog", !original.AssumedCatalog, original.AssumedCatalog, account},
	} {
		t.Run(test.column, func(t *testing.T) {
			query := "UPDATE replay_frames SET " + test.column + " = ? WHERE id = ?"
			if err := db.db.Exec(query, test.value, id).Error; err != nil {
				t.Fatal(err)
			}
			defer func() {
				if err := db.db.Exec(query, test.restore, id).Error; err != nil {
					t.Error(err)
				}
			}()
			if err := db.ScanReplayFrames(context.Background(), test.account, func(ReplayFrame) error { return nil }); err == nil {
				t.Fatalf("header %s tamper was not authenticated", test.column)
			}
		})
	}
	other := replayAcceptanceRecord(t, db, frame)
	if err := db.db.Exec("UPDATE replay_frames SET manifest_cipher = ? WHERE id = ?", original.ManifestCipher, other).Error; err != nil {
		t.Fatal(err)
	}
	if err := db.ScanReplayFrames(context.Background(), account, func(ReplayFrame) error { return nil }); err == nil {
		t.Fatal("manifest transplanted between otherwise identical live captures")
	}
}

func TestReplayPlanHeadersAndExactBytes(t *testing.T) {
	db, _, _ := integrationStore(t)
	const account = "plan-authentication"
	saveTestAccount(t, db, account)
	plan := ReplayPlan{AccountID: account, MissionID: "mission", ShipID: "ship", Phase: "buy",
		Generation: "generation", Sequence: 7, Revision: "revision",
		ObservedAt: time.Date(2026, 10, 27, 3, 4, 5, 987654321, time.FixedZone("offset", -3*60*60)),
		Plan:       json.RawMessage("{\n \"net\": 9007199254740993\n}"), Policy: json.RawMessage("{ \"reserve\": 5 }")}
	if err := db.RecordReplayPlan(context.Background(), plan); err != nil {
		t.Fatal(err)
	}
	plans, err := db.ReplayPlans(context.Background(), account)
	if err != nil || len(plans) != 1 || !bytes.Equal(plans[0].Plan, plan.Plan) || !bytes.Equal(plans[0].Policy, plan.Policy) ||
		!plans[0].ObservedAt.Equal(plan.ObservedAt.UTC().Truncate(time.Microsecond)) {
		t.Fatalf("plan bytes or normalized time changed: %+v, %v", plans, err)
	}
	var row replayPlanRow
	if err := db.db.First(&row).Error; err != nil {
		t.Fatal(err)
	}
	if err := db.db.Exec("UPDATE replay_plans SET id = id + 1000 WHERE id = ?", row.ID).Error; err != nil {
		t.Fatal(err)
	}
	if _, err := db.ReplayPlans(context.Background(), account); err != nil {
		t.Fatalf("portable plan serial entered AAD: %v", err)
	}
	for _, test := range []struct {
		column  string
		value   any
		restore any
	}{
		{"generation", "tampered", row.Generation},
		{"sequence", row.Sequence + 1, row.Sequence},
		{"observed_at", row.ObservedAt.Add(time.Second), row.ObservedAt},
	} {
		t.Run(test.column, func(t *testing.T) {
			query := "UPDATE replay_plans SET " + test.column + " = ? WHERE id = ?"
			if err := db.db.Exec(query, test.value, row.ID+1000).Error; err != nil {
				t.Fatal(err)
			}
			defer func() {
				if err := db.db.Exec(query, test.restore, row.ID+1000).Error; err != nil {
					t.Error(err)
				}
			}()
			if _, err := db.ReplayPlans(context.Background(), account); err == nil {
				t.Fatalf("plan %s tamper was not authenticated", test.column)
			}
		})
	}
}

func TestReplayPlanFingerprintCoversPrivateTuple(t *testing.T) {
	plan := ReplayPlan{ShipID: "ship", Phase: "buy", Plan: json.RawMessage(`{"net":1}`),
		Policy: json.RawMessage(`{"reserve":2}`), Revision: "revision"}
	original := ReplayPlanFingerprint(plan)
	for _, change := range []func(*ReplayPlan){
		func(value *ReplayPlan) { value.ShipID = "other-ship" },
		func(value *ReplayPlan) { value.Phase = "sell" },
		func(value *ReplayPlan) { value.Plan = json.RawMessage(`{"net":2}`) },
		func(value *ReplayPlan) { value.Policy = json.RawMessage(`{"reserve":3}`) },
		func(value *ReplayPlan) { value.Revision = "other-revision" },
	} {
		changed := plan
		change(&changed)
		if ReplayPlanFingerprint(changed) == original {
			t.Fatalf("private tuple field omitted: %+v", changed)
		}
	}
	plan.AccountID, plan.MissionID, plan.Generation, plan.Sequence, plan.ObservedAt = "account", "mission", "generation", 9, time.Now()
	if ReplayPlanFingerprint(plan) != original {
		t.Fatal("public plan headers entered the private tuple fingerprint")
	}
}

func TestReplayEventsUseUncompressedSeparateEncoding(t *testing.T) {
	db, _, _ := integrationStore(t)
	const account = "events-encoding"
	saveTestAccount(t, db, account)
	data := []byte(`[{ "event":"sighting", "ship_id":"private-ship", "detail":"chosen event text" }]`)
	frame := replayAcceptanceFrame(account, "events", time.Now().UTC(), data)
	frame.Datasets["events"] = ReplayDataset{Data: data, Meta: json.RawMessage(`{"complete":true}`)}
	result, err := db.recordReplayFrame(context.Background(), frame)
	if err != nil || result.PayloadsAdded != 2 {
		t.Fatalf("encoding domains collided: result=%+v, err=%v", result, err)
	}
	var row replayFrameRow
	if err := db.db.First(&row, result.ID).Error; err != nil {
		t.Fatal(err)
	}
	aad, err := replayManifestAAD(row)
	if err != nil {
		t.Fatal(err)
	}
	compressed, err := db.decrypt(row.ManifestCipher, aad)
	if err != nil {
		t.Fatal(err)
	}
	plain, err := replayDecompress(compressed)
	if err != nil {
		t.Fatal(err)
	}
	var manifest map[string]replayManifestEntry
	if err := json.Unmarshal(plain, &manifest); err != nil {
		t.Fatal(err)
	}
	events, book := manifest["events"], manifest["books/private-port"]
	if events.Encoding != replayEncodingEvents || book.Encoding != replayEncodingGzip || events.Digest == book.Digest {
		t.Fatalf("missing authenticated encoding/domain marker: %+v", manifest)
	}
	for _, entry := range []replayManifestEntry{events, book} {
		var payload replayPayloadRow
		if err := db.db.First(&payload, "account_id = ? AND digest = ?", account, entry.Digest).Error; err != nil {
			t.Fatal(err)
		}
		encoded, err := db.decrypt(payload.PayloadCipher, privateAAD("replay_payloads", "payload", account, entry.Digest))
		if err != nil {
			t.Fatal(err)
		}
		if entry.Encoding == replayEncodingEvents {
			if !bytes.Equal(encoded, data) {
				t.Fatal("events were compressed or their exact clean bytes changed")
			}
		} else {
			decoded, err := replayDecompress(encoded)
			if err != nil || !bytes.Equal(decoded, data) {
				t.Fatalf("non-events payload lost gzip encoding: %q, %v", decoded, err)
			}
		}
	}
	recorded := replayAcceptanceFrames(t, db, account)
	if len(recorded) != 1 || !bytes.Equal(recorded[0].Datasets["events"].Data, data) {
		t.Fatalf("events did not round trip: %+v", recorded)
	}
	if result, err := db.recordReplayFrame(context.Background(), frame); err != nil || result.PayloadsAdded != 0 {
		t.Fatalf("events/raw and gzip duplicates were recompressed or reinserted: %+v, %v", result, err)
	}
}

func TestReplayRawBoundKeepsSmallEvidenceWithoutAllocations(t *testing.T) {
	frame := replayAcceptanceFrame("bound", "generation", time.Now().UTC(), []byte(`{"ok":true}`))
	if allocations := testing.AllocsPerRun(10, func() {
		bounded, gaps := boundReplayFrame(frame)
		if gaps != 0 || &bounded.Datasets["books/private-port"].Data[0] != &frame.Datasets["books/private-port"].Data[0] {
			t.Fatal("bounded evidence was changed")
		}
	}); allocations != 0 {
		t.Fatalf("within-budget frame allocated: %v", allocations)
	}
	bounded, _ := boundReplayFrame(frame)
	bounded.Datasets["same-map"] = ReplayDataset{}
	if _, exists := frame.Datasets["same-map"]; !exists {
		t.Fatal("within-budget dataset map was copied")
	}
}

func TestReplayRawBoundDemotesLargestThenKeyWithoutMutation(t *testing.T) {
	meta := json.RawMessage(`{"complete":true,"generation":"original","attempted_at":"2026-10-27T00:00:00Z"}`)
	payload := []byte(`"` + strings.Repeat("x", 6<<20) + `"`)
	for _, order := range [][]string{{"z", "a", "m"}, {"m", "z", "a"}} {
		frame := replayAcceptanceFrame("bound", "generation", time.Now().UTC(), nil)
		frame.Datasets = make(map[string]ReplayDataset)
		for _, key := range order {
			frame.Datasets[key] = ReplayDataset{Data: payload, Meta: meta}
		}
		bounded, gaps := boundReplayFrame(frame)
		if gaps != 1 || len(bounded.Datasets["a"].Data) != 0 ||
			len(bounded.Datasets["m"].Data) != len(payload) || len(bounded.Datasets["z"].Data) != len(payload) {
			t.Fatalf("equal-sized demotion was not deterministic: %d gaps", gaps)
		}
		var gap struct {
			Complete      bool   `json:"complete"`
			Stale         bool   `json:"stale"`
			Error         string `json:"error"`
			OriginalBytes int    `json:"original_bytes"`
			Generation    string `json:"generation"`
			AttemptedAt   string `json:"attempted_at"`
		}
		if err := json.Unmarshal(bounded.Datasets["a"].Meta, &gap); err != nil || gap.Complete || !gap.Stale ||
			gap.Error == "" || gap.OriginalBytes != len(payload) || gap.Generation != "original" || gap.AttemptedAt == "" {
			t.Fatalf("demotion lost useful metadata or disclosure: %+v, %v", gap, err)
		}
		for _, dataset := range frame.Datasets {
			if len(dataset.Data) != len(payload) || !bytes.Equal(dataset.Meta, meta) {
				t.Fatal("normalization mutated caller evidence")
			}
		}
		if _, newGaps := boundReplayFrame(bounded); newGaps != 0 {
			t.Fatalf("pre-normalized frame counted gaps twice: %d", newGaps)
		}
		// A larger later key must beat the alphabetical tie breaker.
		frame.Datasets["z"] = ReplayDataset{Data: append(bytes.Clone(payload), ' '), Meta: meta}
		bounded, gaps = boundReplayFrame(frame)
		if gaps != 1 || len(bounded.Datasets["z"].Data) != 0 || len(bounded.Datasets["a"].Data) == 0 {
			t.Fatal("demotion did not prefer the largest data")
		}
	}
}

func TestReplayPersistsAggregateGapsForLiveAndBackfill(t *testing.T) {
	db, _, _ := integrationStore(t)
	payload := []byte(`"` + strings.Repeat("x", 6<<20) + `"`)
	for _, source := range []string{"live", "diagnostic_backfill"} {
		t.Run(source, func(t *testing.T) {
			account := "aggregate-" + source
			saveTestAccount(t, db, account)
			frame := replayAcceptanceFrame(account, "generation", time.Now().UTC(), nil)
			frame.Source = source
			frame.Datasets = map[string]ReplayDataset{
				"a": {Data: payload, Meta: json.RawMessage(`{"complete":true}`)},
				"b": {Data: payload, Meta: json.RawMessage(`{"complete":true}`)},
				"c": {Data: payload, Meta: json.RawMessage(`{"complete":true}`)},
			}
			for opportunity := range 2 {
				frame.Generation = fmt.Sprintf("generation-%d", opportunity)
				if source == "diagnostic_backfill" {
					if result, err := db.RecordReplayBackfillFrame(context.Background(), frame); err != nil || result.DemotedDatasets != 1 {
						t.Fatalf("backfill aggregate gap: result=%+v, err=%v", result, err)
					}
				} else {
					replayAcceptanceRecord(t, db, frame)
				}
			}
			frames := replayAcceptanceFrames(t, db, account)
			if len(frames) != 2 {
				t.Fatalf("repeated oversized opportunities did not persist: %d", len(frames))
			}
			for _, recorded := range frames {
				if len(recorded.Datasets["a"].Data) != 0 || !bytes.Contains(recorded.Datasets["a"].Meta, []byte(`"original_bytes":`)) ||
					!bytes.Equal(recorded.Datasets["b"].Data, payload) || !bytes.Equal(recorded.Datasets["c"].Data, payload) {
					t.Fatal("aggregate gap did not round trip with surviving evidence")
				}
			}
		})
	}
}

func TestReplayDemotionCountersAndWarningsAreProcessLocalAndCauseScoped(t *testing.T) {
	core, logs := observer.New(zap.WarnLevel)
	db := Store{log: zap.New(core)}
	db.CountReplayRecordError("one")
	db.observeReplayDemotions("one", replayFrameBudgetCause, 2)
	db.observeReplayDemotions("one", replayFrameBudgetCause, 3)
	db.observeReplayDemotions("one", replayCompressedCause, 1)
	db.observeReplayDemotions("one", replayCompressedCause, 2)
	db.observeReplayDemotions("two", replayFrameBudgetCause, 4)
	db.observeReplayDemotions("two", replayFrameBudgetCause, 5)
	db.observeReplayDemotions("unused", replayFrameBudgetCause, 0)
	for account, want := range map[string]int64{"one": 9, "two": 9} {
		value, exists := db.replayErrors.Load(account)
		if !exists || value.(*atomic.Int64).Load() != want {
			t.Fatalf("%s replay errors = %v, want %d", account, value, want)
		}
	}
	if _, exists := db.replayErrors.Load("unused"); exists {
		t.Fatal("zero demotions created a counter")
	}
	if logs.Len() != 3 {
		t.Fatalf("warnings were not once per account/cause: %d", logs.Len())
	}
	seen := make(map[string]bool)
	for _, entry := range logs.All() {
		fields := entry.ContextMap()
		cause, _ := fields["cause"].(string)
		account, _ := fields["account_id"].(string)
		key := account + "/" + cause
		if entry.Message != "replay recording stored dataset gaps" || len(fields) != 3 ||
			(cause != replayFrameBudgetCause && cause != replayCompressedCause) || seen[key] {
			t.Fatalf("unsafe or repeated replay warning: %+v", entry)
		}
		seen[key] = true
	}
}

func TestReplayPersistsSanitizedBudgetGapsForLiveAndBackfill(t *testing.T) {
	db, _, _ := integrationStore(t)
	for _, source := range []string{"live", "diagnostic_backfill"} {
		for _, kind := range []string{"passphrase", "html", "aggregate", "metadata", "key", "catalog", "mixed", "compressed"} {
			t.Run(source+"/"+kind, func(t *testing.T) {
				account := "expanded-" + source + "-" + kind
				credentials := saveTestAccount(t, db, account)
				credentials.Passphrase = "q"
				if err := db.SaveAccount(context.Background(), Account{
					ID: account, Label: account, CompanyID: "company-" + account, Enabled: true,
				}, credentials); err != nil {
					t.Fatal(err)
				}
				core, logs := observer.New(zap.WarnLevel)
				db.log = zap.New(core)
				safe := []byte("{\n \"count\":9007199254740993, \"duplicate\":1, \"duplicate\":2\n}")
				meta := json.RawMessage(`{"complete":true,"count":9007199254740993,"detail":"q"}`)
				frame := replayAcceptanceFrame(account, "generation", time.Now().UTC(), nil)
				frame.Source = source
				frame.Datasets["healthy"] = ReplayDataset{Data: safe, Meta: json.RawMessage(`{"complete":true}`)}
				gapKeys := []string{"books/private-port"}
				wantDemotions := int64(1)
				compact := false
				switch kind {
				case "passphrase":
					frame.Datasets["books/private-port"] = ReplayDataset{
						Data: []byte(`{"detail":"` + strings.Repeat("q", 1<<20) + strings.Repeat("x", 13<<20) + `"}`),
						Meta: meta,
					}
				case "html":
					frame.Datasets["books/private-port"] = ReplayDataset{
						Data: []byte(`{"detail":"q","html":"` + strings.Repeat("<", 3<<20) + strings.Repeat("x", 12<<20) + `"}`),
						Meta: meta,
					}
				case "aggregate":
					delete(frame.Datasets, "books/private-port")
					data := []byte(`{"detail":"` + strings.Repeat("q", 512<<10) + strings.Repeat("x", 4<<20) + `"}`)
					for _, key := range []string{"a", "b", "c"} {
						frame.Datasets[key] = ReplayDataset{Data: data, Meta: meta}
					}
					gapKeys, wantDemotions = []string{"a", "b"}, 2
				case "metadata":
					frame.Datasets["books/private-port"] = ReplayDataset{
						Data: []byte(`{"ok":true}`), Meta: json.RawMessage(`{"detail":"` + strings.Repeat("q", 2<<20) + `"}`),
					}
				case "key":
					delete(frame.Datasets, "books/private-port")
					frame.Datasets[strings.Repeat("q", 400<<10)] = ReplayDataset{Data: []byte(`{"ok":true}`), Meta: meta}
					gapKeys, wantDemotions, compact = []string{"_replay_gap"}, 2, true
				case "catalog":
					frame.CatalogDigest = strings.Repeat("q", 2<<20)
					gapKeys, wantDemotions, compact = []string{"_replay_gap"}, 2, true
				case "mixed":
					delete(frame.Datasets, "books/private-port")
					frame.Datasets["a"] = ReplayDataset{Data: []byte(`"` + strings.Repeat("x", 9<<20) + `"`), Meta: meta}
					frame.Datasets["b"] = ReplayDataset{
						Data: []byte(`{"detail":"` + strings.Repeat("q", 2<<20) + strings.Repeat("x", 4<<20) + `"}`), Meta: meta,
					}
					frame.Datasets["c"] = ReplayDataset{Data: []byte(`"` + strings.Repeat("x", 3<<20) + `"`), Meta: meta}
					gapKeys, wantDemotions = []string{"a", "b"}, 2
				case "compressed":
					noise := make([]byte, 5<<20)
					if _, err := rand.NewChaCha8([32]byte{1}).Read(noise); err != nil {
						t.Fatal(err)
					}
					data := []byte(`"` + base64.StdEncoding.EncodeToString(noise) + `"`)
					frame.Datasets["books/private-port"] = ReplayDataset{Data: data, Meta: meta}
					frame.Datasets["alias"] = ReplayDataset{Data: data, Meta: meta}
					gapKeys, wantDemotions = []string{"alias", "books/private-port"}, 2
				}
				if _, gaps := boundReplayFrame(frame); kind != "mixed" && gaps != 0 {
					t.Fatalf("fixture exceeded the raw budget before redaction: %d", gaps)
				}
				fingerprints := make(map[string][2][32]byte, len(frame.Datasets))
				for key, dataset := range frame.Datasets {
					fingerprints[key] = [2][32]byte{sha256.Sum256(dataset.Data), sha256.Sum256(dataset.Meta)}
				}
				for opportunity := range 2 {
					frame.Generation = fmt.Sprintf("generation-%d", opportunity)
					var result ReplayFrameWrite
					var err error
					if source == "diagnostic_backfill" {
						result, err = db.RecordReplayBackfillFrame(context.Background(), frame)
					} else {
						result, err = db.recordReplayFrame(context.Background(), frame)
					}
					if err != nil || result.ID == 0 || result.DemotedDatasets != wantDemotions {
						t.Fatalf("sanitized gap did not persist: result=%+v, want demotions=%d, err=%v", result, wantDemotions, err)
					}
				}
				if source == "diagnostic_backfill" {
					result, err := db.RecordReplayBackfillFrame(context.Background(), frame)
					if err != nil || result != (ReplayFrameWrite{}) {
						t.Fatalf("idempotent rerun reported unstored losses: %+v, %v", result, err)
					}
				}
				frames := replayAcceptanceFrames(t, db, account)
				if len(frames) != 2 {
					t.Fatalf("repeated overflow did not persist both opportunities: %d", len(frames))
				}
				for _, recorded := range frames {
					if recorded.Source != source {
						t.Fatal("gap changed the authenticated source")
					}
					for _, key := range gapKeys {
						dataset, exists := recorded.Datasets[key]
						var gap struct {
							Complete      bool   `json:"complete"`
							Stale         bool   `json:"stale"`
							Error         string `json:"error"`
							OriginalBytes int64  `json:"original_bytes"`
						}
						if err := json.Unmarshal(dataset.Meta, &gap); err != nil || !exists || gap.Complete ||
							!gap.Stale || gap.Error == "" || len(dataset.Data) != 0 {
							t.Fatalf("expanded %s omitted a reasoned gap: %+v, %v", kind, gap, err)
						}
					}
					if compact {
						if len(recorded.Datasets) != 1 || recorded.CatalogDigest != "" {
							t.Fatal("expanded names/context did not become one compact gap")
						}
					} else if !bytes.Equal(recorded.Datasets["healthy"].Data, safe) {
						t.Fatal("unaffected exact evidence changed")
					}
					if kind == "aggregate" && !bytes.Contains(recorded.Datasets["c"].Data, []byte(redacted)) {
						t.Fatal("aggregate bounding dropped or failed to redact surviving evidence")
					}
					if kind == "metadata" && !bytes.Contains(recorded.Datasets["books/private-port"].Meta, []byte(`"original_meta_bytes":`)) {
						t.Fatal("redaction-sized metadata gap omitted its original size")
					}
					if kind == "compressed" && !bytes.Contains(recorded.Datasets["alias"].Meta, []byte(`9007199254740993`)) {
						t.Fatal("compressed demotion lost metadata integer precision")
					}
				}
				if len(frame.Datasets) != len(fingerprints) {
					t.Fatal("preparation mutated the caller's dataset map")
				}
				for key, dataset := range frame.Datasets {
					if fingerprints[key] != ([2][32]byte{sha256.Sum256(dataset.Data), sha256.Sum256(dataset.Meta)}) {
						t.Fatal("preparation mutated caller evidence")
					}
				}
				stats, err := db.ServiceStats(context.Background())
				if err != nil {
					t.Fatal(err)
				}
				errors := float64(0)
				for _, counter := range stats.Counters {
					if counter.AccountID == account && counter.Name == "replay_record_errors_total" {
						errors += counter.Value
					}
				}
				if source == "live" {
					causes := map[string]bool{replayFrameBudgetCause: true}
					switch kind {
					case "passphrase", "html", "metadata":
						causes = map[string]bool{replayRedactionSizeCause: true}
					case "mixed":
						causes[replayRedactionSizeCause] = true
					case "compressed":
						causes = map[string]bool{replayCompressedCause: true}
					}
					if errors != float64(2*wantDemotions) || logs.Len() != len(causes) {
						t.Fatalf("successful live gaps were not counted/warned once: errors=%g, logs=%+v", errors, logs.All())
					}
					for _, entry := range logs.All() {
						cause, _ := entry.ContextMap()["cause"].(string)
						if !causes[cause] {
							t.Fatalf("live demotion warned with an incorrect/repeated cause: %+v", entry)
						}
						delete(causes, cause)
					}
				} else if errors != 0 || logs.Len() != 0 {
					t.Fatal("diagnostic backfill gaps entered live recorder counters or warnings")
				}
			})
		}
	}
}

func TestReplayPersistsOversizedPayloadMetadataAndDuplicateClockAsGaps(t *testing.T) {
	db, _, _ := integrationStore(t)
	saveTestAccount(t, db, "large-gaps")
	large := []byte(`"` + strings.Repeat("x", MaxReplayPlaintextBytes) + `"`)
	frame := replayAcceptanceFrame("large-gaps", "generation", time.Now().UTC(), large)
	frame.Clock = large
	frame.Datasets["clock"] = ReplayDataset{Data: large, Meta: json.RawMessage(`{"complete":true}`)}
	frame.Datasets["huge-meta"] = ReplayDataset{Meta: large}
	bounded, gaps := boundReplayFrame(frame)
	if gaps != 3 || string(bounded.Clock) != "null" {
		t.Fatalf("huge raw evidence was not bounded before parsing: %d, %s", gaps, bounded.Clock)
	}
	replayAcceptanceRecord(t, db, frame)
	recorded := replayAcceptanceFrames(t, db, "large-gaps")[0]
	if string(recorded.Clock) != "null" || len(frame.Clock) != len(large) {
		t.Fatal("clock projection was not dropped safely or caller header changed")
	}
	for key, dataset := range recorded.Datasets {
		var meta map[string]json.RawMessage
		if err := json.Unmarshal(dataset.Meta, &meta); err != nil || len(dataset.Data) != 0 ||
			string(meta["complete"]) != "false" || string(meta["stale"]) != "true" || len(meta["error"]) == 0 {
			t.Fatalf("oversized %s did not persist a reasoned gap: %s, %v", key, dataset.Meta, err)
		}
	}
	if !bytes.Contains(recorded.Datasets["huge-meta"].Meta, []byte(`"original_meta_bytes":`)) {
		t.Fatal("compact metadata gap omitted original metadata size")
	}
}

func TestReplayPersistsCompactGapWhenNamesOrContextCannotFit(t *testing.T) {
	db, _, _ := integrationStore(t)
	for _, kind := range []string{"key", "catalog", "metadata"} {
		account := "compact-gap-" + kind
		saveTestAccount(t, db, account)
		frame := replayAcceptanceFrame(account, "generation", time.Now().UTC(), nil)
		switch kind {
		case "key":
			frame.Datasets = map[string]ReplayDataset{
				strings.Repeat("x", MaxReplayPlaintextBytes+1): {Meta: json.RawMessage(`{"complete":true}`)},
			}
		case "catalog":
			frame.CatalogDigest = strings.Repeat("x", MaxReplayPlaintextBytes+1)
		case "metadata":
			meta := json.RawMessage(`{"detail":"` + strings.Repeat("x", 60<<10) + `"}`)
			frame.Datasets = make(map[string]ReplayDataset)
			for i := range 300 {
				frame.Datasets[strconv.Itoa(i)] = ReplayDataset{Meta: meta}
			}
		}
		bounded, gaps := boundReplayFrame(frame)
		if gaps != int64(len(frame.Datasets)) || len(bounded.Datasets) != 1 ||
			len(bounded.Datasets["_replay_gap"].Meta) == 0 || bounded.CatalogDigest != "" {
			t.Fatalf("%s context did not become one compact frame gap: %d", kind, gaps)
		}
		replayAcceptanceRecord(t, db, frame)
		recorded := replayAcceptanceFrames(t, db, account)[0]
		var meta struct {
			Complete         bool   `json:"complete"`
			Stale            bool   `json:"stale"`
			Error            string `json:"error"`
			OriginalDatasets int    `json:"original_datasets"`
		}
		if err := json.Unmarshal(recorded.Datasets["_replay_gap"].Meta, &meta); err != nil || meta.Complete ||
			!meta.Stale || meta.Error == "" || meta.OriginalDatasets != len(frame.Datasets) {
			t.Fatalf("%s summary did not round trip: %+v, %v", kind, meta, err)
		}
	}
}

func TestReplayClockProjectionOmitsWrongTypesAndRetainsPrivateRaw(t *testing.T) {
	db, _, _ := integrationStore(t)
	saveTestAccount(t, db, "typed-clock")
	rawClocks := []json.RawMessage{
		json.RawMessage(`{"phase":"unexpected","real_utc":123,"calendar":false,"unlocked_phase":3,"paused":false,"total_game_hours":9007199254740993,"private_label":"retained"}`),
		json.RawMessage(`{"phase":4,"real_utc":"2026-10-27T03:02:03+02:00","paused":"no","unlocked_phase":{},"calendar":{"year":"wrong","month":10,"day":false,"hour":[],"minute":2,"time_scale":"fast","total_game_hours":9007199254740993},"private_label":"retained"}`),
	}
	for index, raw := range rawClocks {
		frame := replayAcceptanceFrame("typed-clock", fmt.Sprintf("generation-%d", index), time.Now().UTC().Add(time.Duration(index)*time.Second), nil)
		frame.Clock = raw
		frame.Datasets["clock"] = ReplayDataset{Data: raw, Meta: json.RawMessage(`{"complete":true}`)}
		id := replayAcceptanceRecord(t, db, frame)
		var row replayFrameRow
		if err := db.db.First(&row, id).Error; err != nil {
			t.Fatal(err)
		}
		var clock replayPublicClock
		if err := json.Unmarshal(row.Clock, &clock); err != nil || bytes.Contains(row.Clock, []byte("private_label")) {
			t.Fatalf("invalid public projection: %s, %v", row.Clock, err)
		}
		if index == 0 {
			if clock.Phase != nil || clock.RealUTC != nil || clock.Calendar != nil || clock.UnlockedPhase == nil ||
				*clock.UnlockedPhase != 3 || clock.Paused == nil || *clock.Paused || clock.TotalGameHours == nil || *clock.TotalGameHours != 9007199254740993 {
				t.Fatalf("wrong scalar hid valid siblings: %s", row.Clock)
			}
		} else if clock.Phase == nil || *clock.Phase != 4 || clock.Paused != nil || clock.UnlockedPhase != nil ||
			clock.RealUTC == nil || clock.RealUTC.Format(time.RFC3339) != "2026-10-27T01:02:03Z" || clock.Calendar == nil ||
			clock.Calendar.Year != nil || clock.Calendar.Day != nil || clock.Calendar.Hour != nil || clock.Calendar.TimeScale != nil ||
			clock.Calendar.Month == nil || *clock.Calendar.Month != 10 || clock.Calendar.Minute == nil || *clock.Calendar.Minute != 2 ||
			clock.Calendar.TotalGameHours == nil || *clock.Calendar.TotalGameHours != 9007199254740993 {
			t.Fatalf("calendar scalar filtering lost UTC or precision: %s", row.Clock)
		}
	}
	frames := replayAcceptanceFrames(t, db, "typed-clock")
	if len(frames) != 2 {
		t.Fatalf("wrong scalar frame was rejected: %d", len(frames))
	}
	for index, frame := range frames {
		if !bytes.Equal(frame.Datasets["clock"].Data, rawClocks[index]) {
			t.Fatal("valid encrypted raw clock bytes were not retained")
		}
	}
}

func TestReplayRejectsOversizedPlanAndCompressionInput(t *testing.T) {
	var db Store
	large := bytes.Repeat([]byte("x"), MaxReplayPlaintextBytes+1)
	plan := ReplayPlan{AccountID: "bound", MissionID: "mission", Plan: json.RawMessage(`{}`), Policy: json.RawMessage(`{}`),
		Revision: string(large)}
	if err := db.RecordReplayPlan(context.Background(), plan); !errors.Is(err, ErrResponseTooLarge) {
		t.Fatalf("oversized plan context reached database work: %v", err)
	}
	if _, err := replayCompress(large); !errors.Is(err, ErrResponseTooLarge) {
		t.Fatalf("oversized plaintext was compressed: %v", err)
	}
}

func replayTestGzipBomb(t *testing.T) []byte {
	t.Helper()
	var buffer bytes.Buffer
	writer := gzip.NewWriter(&buffer)
	if _, err := writer.Write(bytes.Repeat([]byte(" "), MaxReplayPlaintextBytes+1)); err != nil {
		t.Fatal(err)
	}
	if err := writer.Close(); err != nil {
		t.Fatal(err)
	}
	return buffer.Bytes()
}

func TestReplayDecompressRejectsGzipBomb(t *testing.T) {
	bomb := replayTestGzipBomb(t)
	if len(bomb) >= MaxReplayCompressedBytes {
		t.Fatal("gzip bomb fixture does not exercise the expansion bound")
	}
	if _, err := replayDecompress(bomb); !errors.Is(err, ErrResponseTooLarge) {
		t.Fatalf("unbounded gzip expansion: %v", err)
	}
}

func TestReplayScanBoundsAuthenticatedManifestAndPayloadExpansion(t *testing.T) {
	db, _, _ := integrationStore(t)
	bomb := replayTestGzipBomb(t)
	for _, target := range []string{"manifest", "payload"} {
		t.Run(target, func(t *testing.T) {
			account := "bomb-" + target
			saveTestAccount(t, db, account)
			frame := replayAcceptanceFrame(account, "generation", time.Now().UTC(), []byte(`{"book":1}`))
			id := replayAcceptanceRecord(t, db, frame)
			var ciphertext []byte
			var err error
			if target == "manifest" {
				var row replayFrameRow
				if err := db.db.First(&row, id).Error; err != nil {
					t.Fatal(err)
				}
				aad, err := replayManifestAAD(row)
				if err != nil {
					t.Fatal(err)
				}
				ciphertext, err = db.encrypt(bomb, aad)
				if err != nil {
					t.Fatal(err)
				}
				err = db.db.Model(&replayFrameRow{}).Where("id = ?", id).Update("manifest_cipher", ciphertext).Error
			} else {
				var payload replayPayloadRow
				if err := db.db.First(&payload, "account_id = ?", account).Error; err != nil {
					t.Fatal(err)
				}
				ciphertext, err = db.encrypt(bomb, privateAAD("replay_payloads", "payload", account, payload.Digest))
				if err != nil {
					t.Fatal(err)
				}
				err = db.db.Model(&replayPayloadRow{}).Where("account_id = ? AND digest = ?", account, payload.Digest).
					Update("payload_cipher", ciphertext).Error
			}
			if err != nil {
				t.Fatal(err)
			}
			if err := db.ScanReplayFrames(context.Background(), account, func(ReplayFrame) error { return nil }); !errors.Is(err, ErrResponseTooLarge) {
				t.Fatalf("authenticated %s bomb was expanded: %v", target, err)
			}
		})
	}
}

func TestReplayPayloadBatchingDeterministicOrderAndActualAdditions(t *testing.T) {
	db, _, _ := integrationStore(t)
	const account = "payload-batching"
	saveTestAccount(t, db, account)
	for _, statement := range []string{
		"CREATE TABLE replay_payload_insert_order (sequence bigserial PRIMARY KEY, digest text NOT NULL)",
		`CREATE FUNCTION observe_replay_payload_insert() RETURNS trigger LANGUAGE plpgsql AS $$
			BEGIN INSERT INTO replay_payload_insert_order (digest) VALUES (NEW.digest); RETURN NEW; END $$`,
		"CREATE TRIGGER observe_replay_payload_insert BEFORE INSERT ON replay_payloads FOR EACH ROW EXECUTE FUNCTION observe_replay_payload_insert()",
	} {
		if err := db.db.Exec(statement).Error; err != nil {
			t.Fatal(err)
		}
	}
	var inserts []string
	if err := db.db.Callback().Create().After("gorm:create").Register("test:replay-payload-batches", func(tx *gorm.DB) {
		if tx.Statement.Table == "replay_payloads" {
			inserts = append(inserts, tx.Statement.SQL.String())
		}
	}); err != nil {
		t.Fatal(err)
	}
	frame := replayAcceptanceFrame(account, "batch", time.Now().UTC(), nil)
	frame.Datasets = make(map[string]ReplayDataset)
	for i := range 300 {
		frame.Datasets[fmt.Sprintf("books/port-%d", i)] = ReplayDataset{
			Data: []byte(fmt.Sprintf(`{"book":%d}`, i)), Meta: json.RawMessage(`{"complete":true}`),
		}
	}
	if result, err := db.recordReplayFrame(context.Background(), frame); err != nil || result.PayloadsAdded != 300 {
		t.Fatalf("batched payload additions: %+v, %v", result, err)
	}
	if len(inserts) != 3 {
		t.Fatalf("300 payloads used %d INSERTs instead of bounded batches", len(inserts))
	}
	for _, query := range inserts {
		_, returning, ok := strings.Cut(query, "RETURNING ")
		if !ok || strings.TrimSpace(returning) != `"digest"` {
			t.Fatalf("payload insertion returned ciphertext instead of digest only: %s", query)
		}
	}
	var digests []string
	if err := db.db.Table("replay_payload_insert_order").Order("sequence").Pluck("digest", &digests).Error; err != nil {
		t.Fatal(err)
	}
	if len(digests) != 300 || !sort.StringsAreSorted(digests) {
		t.Fatalf("payload inserts did not acquire unique identities in digest order: %v", digests)
	}
	inserts = nil
	frame.Generation = "mostly-duplicates"
	frame.Datasets["books/port-0"] = ReplayDataset{Data: []byte(`{"book":1000}`), Meta: json.RawMessage(`{"complete":true}`)}
	if result, err := db.recordReplayFrame(context.Background(), frame); err != nil || result.PayloadsAdded != 1 || len(inserts) != 1 {
		t.Fatalf("duplicates reached payload inserts: result=%+v, inserts=%d, err=%v", result, len(inserts), err)
	}
}

func TestReplayConflictInsertGetsPruneProtection(t *testing.T) {
	db, _, _ := integrationStore(t)
	const account = "conflict-locking"
	saveTestAccount(t, db, account)
	at := time.Now().UTC().Truncate(time.Microsecond)
	data := []byte(`{"book":1}`)
	digest := db.replayDigest(account, data)
	compressed, err := replayCompress(data)
	if err != nil {
		t.Fatal(err)
	}
	ciphertext, err := db.encrypt(compressed, privateAAD("replay_payloads", "payload", account, digest))
	if err != nil {
		t.Fatal(err)
	}
	competitor := db.db.Begin()
	if competitor.Error != nil {
		t.Fatal(competitor.Error)
	}
	defer competitor.Rollback()
	if err := competitor.Create(&replayPayloadRow{AccountID: account, Digest: digest, PayloadCipher: ciphertext,
		StoredBytes: int64(len(ciphertext)), LastSeenAt: at}).Error; err != nil {
		t.Fatal(err)
	}
	queries := 0
	protected := false
	var callbackError error
	if err := db.db.Callback().Query().After("gorm:query").Register("test:replay-conflict-lock", func(tx *gorm.DB) {
		if tx.Statement.Table != "replay_payloads" {
			return
		}
		queries++
		if queries == 1 {
			// The initial KEY SHARE read cannot see this uncommitted payload.
			// Commit after that read to force the INSERT conflict path.
			callbackError = competitor.Commit().Error
			return
		}
		var lockedDigest string
		err := db.db.Raw("SELECT digest FROM replay_payloads WHERE account_id = ? AND digest = ? FOR UPDATE NOWAIT",
			account, digest).Scan(&lockedDigest).Error
		var postgresError *pgconn.PgError
		protected = errors.As(err, &postgresError) && postgresError.Code == "55P03"
	}); err != nil {
		t.Fatal(err)
	}
	frame := replayAcceptanceFrame(account, "conflict", at, data)
	if result, err := db.recordReplayFrame(context.Background(), frame); err != nil || result.PayloadsAdded != 0 ||
		callbackError != nil || queries != 2 || !protected {
		t.Fatalf("conflict payload lacked KEY SHARE protection: result=%+v, queries=%d, protected=%t, err=%v, callback=%v",
			result, queries, protected, err, callbackError)
	}
}

func TestReplayLastSeenCoalescingRetainsSlackReferences(t *testing.T) {
	db, _, _ := integrationStore(t)
	const account = "last-seen-slack"
	saveTestAccount(t, db, account)
	now := time.Now().UTC().Truncate(time.Microsecond)
	first := now.Add(-2 * time.Hour)
	data := []byte(`{"book":1}`)
	frame := replayAcceptanceFrame(account, "initial", first, data)
	replayAcceptanceRecord(t, db, frame)
	for i, observed := range []time.Time{first.Add(30 * time.Minute), first.Add(time.Hour), first.Add(-time.Hour)} {
		frame.Generation, frame.ObservedAt = fmt.Sprintf("coalesced-%d", i), observed
		replayAcceptanceRecord(t, db, frame)
	}
	var payload replayPayloadRow
	if err := db.db.First(&payload, "account_id = ?", account).Error; err != nil || !payload.LastSeenAt.Equal(first) {
		t.Fatalf("within-hour or out-of-order capture rewrote last_seen_at: %v, %v", payload.LastSeenAt, err)
	}
	if _, err := db.PruneReplay(context.Background(), 90*time.Minute, 0); err != nil {
		t.Fatal(err)
	}
	retained := replayAcceptanceFrames(t, db, account)
	if len(retained) != 1 || !bytes.Equal(retained[0].Datasets["books/private-port"].Data, data) {
		t.Fatalf("hour slack removed a retained reference: %+v", retained)
	}
	frame.Generation, frame.ObservedAt = "advanced", first.Add(time.Hour+time.Microsecond)
	replayAcceptanceRecord(t, db, frame)
	if err := db.db.First(&payload, "account_id = ?", account).Error; err != nil || !payload.LastSeenAt.Equal(frame.ObservedAt) {
		t.Fatalf("last_seen_at did not advance beyond one hour: %v, %v", payload.LastSeenAt, err)
	}
	if _, err := db.PruneReplay(context.Background(), 90*24*time.Hour, 1); err != nil {
		t.Fatal(err)
	}
	if replayAcceptanceCount(t, db, "replay_frames", account) != 0 || replayAcceptanceCount(t, db, "replay_payloads", account) != 0 {
		t.Fatal("conservative payload slack prevented enforcement of a sub-record byte cap")
	}
}
