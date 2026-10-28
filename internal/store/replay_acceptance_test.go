package store

// Assumed API: RecordReplayFrame(context.Context, ReplayFrame) (int64, error);
// ScanReplayFrames(context.Context, string, func(ReplayFrame) error) error;
// NewReplayFrameReader(context.Context, string, time.Time) *ReplayFrameReader with
// PeekTime() (time.Time, error)/Next() (ReplayFrame, error) returning io.EOF when drained;
// ReplayFrameBounds(context.Context, string, time.Time) (time.Time, time.Time, error);
// RecordReplayPlan(context.Context, ReplayPlan) error; ReplayPlans(context.Context, string) ([]ReplayPlan, error);
// LinkReplayAction(context.Context, ReplayActionLink) error; ReplayActionLinks(context.Context, string, time.Time, time.Time, []string) ([]ReplayActionLink, error);
// PruneReplay(context.Context, time.Duration, int64) (time.Time, error).
// ReplayDataset has Data []byte and Meta json.RawMessage. ReplayFrame has ID int64,
// AccountID/Generation/Source/CatalogDigest string, ObservedAt time.Time, Clock json.RawMessage,
// Phase int, Decision/AssumedCatalog bool and Datasets map[string]ReplayDataset.
// ReplayPlan has ID int64, AccountID/MissionID/ShipID/Phase/Generation/Revision string,
// Sequence int64, ObservedAt time.Time, Plan/Policy json.RawMessage. ReplayActionLink has
// AccountID/ActionID/MissionID/Stage/Generation string and Sequence int64.
// Storage columns used below: payload_cipher, manifest_cipher, digest, stored_bytes,
// last_seen_at, observed_at. Payload AAD is privateAAD("replay_payloads", "payload", accountID, digest).

import (
	"bytes"
	"compress/gzip"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"reflect"
	"testing"
	"time"
)

func replayAcceptanceFrame(account, generation string, at time.Time, data []byte) ReplayFrame {
	return ReplayFrame{
		AccountID: account, Generation: generation, Source: "live", ObservedAt: at,
		Clock: json.RawMessage(`{"total_game_hours":100,"phase":1}`), Phase: 1,
		Datasets: map[string]ReplayDataset{
			"books/private-port": {Data: data, Meta: json.RawMessage(`{"complete":true,"generation":"` + generation + `"}`)},
		},
	}
}

func replayAcceptanceRecord(t *testing.T, db *Store, frame ReplayFrame) int64 {
	t.Helper()
	id, err := db.RecordReplayFrame(context.Background(), frame)
	if err != nil {
		t.Fatal(err)
	}
	return id
}

func replayAcceptanceFrames(t *testing.T, db *Store, account string) []ReplayFrame {
	t.Helper()
	var frames []ReplayFrame
	if err := db.ScanReplayFrames(context.Background(), account, func(frame ReplayFrame) error {
		frames = append(frames, frame)
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	return frames
}

func replayAcceptanceCount(t *testing.T, db *Store, table, account string) int64 {
	t.Helper()
	var count int64
	if err := db.db.Table(table).Where("account_id = ?", account).Count(&count).Error; err != nil {
		t.Fatal(err)
	}
	return count
}

func TestReplayAcceptancePayloadDedupeEncryptionAndAAD(t *testing.T) {
	db, _, _ := integrationStore(t)
	saveTestAccount(t, db, "replay-one")
	saveTestAccount(t, db, "replay-two")
	at := time.Now().UTC().Truncate(time.Microsecond)
	book := []byte(`{ "port_id": "private-port", "ship_id":"private-ship", "commodity_id":"private-commodity", "orders": [] }`)
	for i := range 100 {
		replayAcceptanceRecord(t, db, replayAcceptanceFrame("replay-one", "same", at.Add(time.Duration(i)*time.Second), book))
	}
	if count := replayAcceptanceCount(t, db, "replay_frames", "replay-one"); count != 100 {
		t.Fatalf("recorded %d frames, want 100 distinct observations", count)
	}
	if count := replayAcceptanceCount(t, db, "replay_payloads", "replay-one"); count != 1 {
		t.Fatalf("100 identical books stored %d payloads, want 1", count)
	}
	replayAcceptanceRecord(t, db, replayAcceptanceFrame("replay-one", "changed", at.Add(101*time.Second), append(bytes.Clone(book[:len(book)-1]), []byte(`,"changed":true}`)...)))
	if count := replayAcceptanceCount(t, db, "replay_payloads", "replay-one"); count != 2 {
		t.Fatalf("changed book stored %d payloads, want 2", count)
	}
	replayAcceptanceRecord(t, db, replayAcceptanceFrame("replay-two", "same", at, book))
	var payloads []struct {
		AccountID     string
		Digest        string
		PayloadCipher []byte
	}
	if err := db.db.Table("replay_payloads").Order("account_id, last_seen_at").Find(&payloads).Error; err != nil {
		t.Fatal(err)
	}
	var manifests []struct{ ManifestCipher []byte }
	if err := db.db.Table("replay_frames").Find(&manifests).Error; err != nil {
		t.Fatal(err)
	}
	for _, payload := range payloads {
		for _, secret := range []string{"private-port", "private-ship", "private-commodity"} {
			if bytes.Contains(payload.PayloadCipher, []byte(secret)) {
				t.Fatalf("payload contains plaintext %s", secret)
			}
		}
	}
	for _, manifest := range manifests {
		for _, secret := range []string{"private-port", "private-ship", "private-commodity"} {
			if bytes.Contains(manifest.ManifestCipher, []byte(secret)) {
				t.Fatalf("manifest contains plaintext %s", secret)
			}
		}
	}
	one := payloads[0]
	plainHash := sha256.Sum256(book)
	if one.Digest == hex.EncodeToString(plainHash[:]) || one.Digest == payloads[2].Digest {
		t.Fatal("digest is unkeyed or shared across accounts")
	}
	compressed, err := db.decrypt(one.PayloadCipher, privateAAD("replay_payloads", "payload", one.AccountID, one.Digest))
	if err != nil {
		t.Fatal(err)
	}
	reader, err := gzip.NewReader(bytes.NewReader(compressed))
	if err != nil {
		t.Fatalf("payload is not encrypted gzip: %v", err)
	}
	defer reader.Close()
	plain, err := io.ReadAll(reader)
	if err != nil || !bytes.Equal(plain, book) {
		t.Fatalf("payload did not preserve exact bytes: %q, %v", plain, err)
	}
	for _, aad := range []string{
		privateAAD("replay_payloads", "payload", "replay-two", one.Digest),
		privateAAD("replay_payloads", "payload", one.AccountID, "wrong-digest"),
	} {
		if _, err := db.decrypt(one.PayloadCipher, aad); err == nil {
			t.Fatal("payload decrypted under another account or digest AAD")
		}
	}
}

func TestReplayAcceptanceScanOrderBytesMetaAndSingleResolution(t *testing.T) {
	db, _, _ := integrationStore(t)
	saveTestAccount(t, db, "scan")
	saveTestAccount(t, db, "other")
	at := time.Now().UTC().Truncate(time.Microsecond)
	data := []byte("{\n \"books\": [ ]\n}")
	late := replayAcceptanceRecord(t, db, replayAcceptanceFrame("scan", "late", at.Add(time.Minute), data))
	firstFrame := replayAcceptanceFrame("scan", "early", at, data)
	gapMeta := json.RawMessage(`{"complete":false,"error":"403 no_port_presence","attempted_at":"2026-10-26T00:00:00Z"}`)
	firstFrame.Datasets["books/absent-port"] = ReplayDataset{Meta: gapMeta}
	first := replayAcceptanceRecord(t, db, firstFrame)
	second := replayAcceptanceRecord(t, db, replayAcceptanceFrame("scan", "tie", at, data))
	replayAcceptanceRecord(t, db, replayAcceptanceFrame("other", "hidden", at, data))
	var ids []int64
	err := db.ScanReplayFrames(context.Background(), "scan", func(frame ReplayFrame) error {
		ids = append(ids, frame.ID)
		if frame.AccountID != "scan" || !bytes.Equal(frame.Datasets["books/private-port"].Data, data) {
			t.Fatalf("scan leaked an account or changed bytes: %+v", frame)
		}
		if len(ids) == 1 {
			if !reflect.DeepEqual(frame.Datasets["books/absent-port"], ReplayDataset{Meta: gapMeta}) {
				t.Fatalf("failed capture was not meta-only: %+v", frame.Datasets)
			}
			if !bytes.Equal(frame.Datasets["books/private-port"].Meta, firstFrame.Datasets["books/private-port"].Meta) {
				t.Fatal("scan changed dataset meta")
			}
			// Later references must use this scan's resolved digest, not decrypt again.
			if err := db.db.Exec("UPDATE replay_payloads SET payload_cipher = ? WHERE account_id = ?", []byte("corrupt-after-first-yield"), "scan").Error; err != nil {
				t.Fatal(err)
			}
		}
		return nil
	})
	if err != nil || !reflect.DeepEqual(ids, []int64{first, second, late}) {
		t.Fatalf("ordered streaming scan: ids=%v, err=%v", ids, err)
	}
	stop := errors.New("consumer stopped")
	// Use the untouched other account to test callback cancellation independently.
	if err := db.ScanReplayFrames(context.Background(), "other", func(ReplayFrame) error { return stop }); !errors.Is(err, stop) {
		t.Fatalf("scan swallowed consumer error: %v", err)
	}
}

func TestReplayAcceptancePruneAgeGreatestAndDiagnosticsIsolation(t *testing.T) {
	db, _, _ := integrationStore(t)
	saveTestAccount(t, db, "prune")
	now := time.Now().UTC().Truncate(time.Microsecond)
	shared := []byte(`{"shared":true}`)
	replayAcceptanceRecord(t, db, replayAcceptanceFrame("prune", "recent", now.Add(-time.Hour), shared))
	// An out-of-order backfill must not regress last_seen_at.
	replayAcceptanceRecord(t, db, replayAcceptanceFrame("prune", "old-shared", now.Add(-100*24*time.Hour), shared))
	replayAcceptanceRecord(t, db, replayAcceptanceFrame("prune", "old-orphan", now.Add(-100*24*time.Hour), []byte(`{"orphan":true}`)))
	if err := db.Prune(context.Background()); err != nil {
		t.Fatal(err)
	}
	if replayAcceptanceCount(t, db, "replay_frames", "prune") != 3 || replayAcceptanceCount(t, db, "replay_payloads", "prune") != 2 {
		t.Fatal("seven-day diagnostics prune touched replay")
	}
	var lastSeen time.Time
	if err := db.db.Raw("SELECT max(last_seen_at) FROM replay_payloads WHERE account_id = ?", "prune").Scan(&lastSeen).Error; err != nil || !lastSeen.Equal(now.Add(-time.Hour)) {
		t.Fatalf("last_seen_at regressed: %v, %v", lastSeen, err)
	}
	cutoff, err := db.PruneReplay(context.Background(), 90*24*time.Hour, 1<<30)
	if err != nil || cutoff.Before(now.Add(-90*24*time.Hour-time.Second)) || cutoff.After(time.Now().Add(-90*24*time.Hour+time.Second)) {
		t.Fatalf("retention cutoff=%v, err=%v", cutoff, err)
	}
	frames := replayAcceptanceFrames(t, db, "prune")
	if len(frames) != 1 || !bytes.Equal(frames[0].Datasets["books/private-port"].Data, shared) || replayAcceptanceCount(t, db, "replay_payloads", "prune") != 1 {
		t.Fatal("age pruning removed a recent reference or retained an expired payload")
	}
}

func TestReplayAcceptancePruneByteCapOldestWholeFrames(t *testing.T) {
	db, _, _ := integrationStore(t)
	saveTestAccount(t, db, "cap")
	now := time.Now().UTC().Truncate(time.Microsecond)
	var ids []int64
	books := []string{`{"book":1}`, `{"book":2}`, `{"book":3}`}
	for i, data := range books {
		ids = append(ids, replayAcceptanceRecord(t, db, replayAcceptanceFrame("cap", "cap", now.Add(time.Duration(i-3)*time.Hour), []byte(data))))
	}
	var total, oldest int64
	if err := db.db.Raw("SELECT (SELECT COALESCE(sum(stored_bytes),0) FROM replay_payloads) + (SELECT COALESCE(sum(stored_bytes),0) FROM replay_frames)").Scan(&total).Error; err != nil {
		t.Fatal(err)
	}
	if err := db.db.Raw("SELECT (SELECT stored_bytes FROM replay_frames WHERE id = ?) + (SELECT stored_bytes FROM replay_payloads ORDER BY last_seen_at LIMIT 1)", ids[0]).Scan(&oldest).Error; err != nil {
		t.Fatal(err)
	}
	cutoff, err := db.PruneReplay(context.Background(), 90*24*time.Hour, total-oldest)
	if err != nil {
		t.Fatal(err)
	}
	frames := replayAcceptanceFrames(t, db, "cap")
	if len(frames) == 0 || len(frames) >= len(ids) {
		t.Fatalf("cap must remove oldest frames while preserving the newest: frames=%+v", frames)
	}
	firstSurvivor := len(ids) - len(frames)
	for i, frame := range frames {
		expected := firstSurvivor + i
		if frame.ID != ids[expected] {
			t.Fatalf("survivors are not a contiguous newest suffix: frames=%+v ids=%v", frames, ids)
		}
		if len(frame.Datasets) != 1 || !bytes.Equal(frame.Datasets["books/private-port"].Data, []byte(books[expected])) {
			t.Fatalf("surviving frame %d lost its resolvable payload: %+v", frame.ID, frame.Datasets)
		}
	}
	newestRemovedAt := now.Add(time.Duration(firstSurvivor-4) * time.Hour)
	if cutoff.After(frames[0].ObservedAt) || !cutoff.After(newestRemovedAt) {
		t.Fatalf("cutoff %v must follow newest removed frame %v and not exceed oldest survivor %v", cutoff, newestRemovedAt, frames[0].ObservedAt)
	}
	var remaining int64
	if err := db.db.Raw("SELECT (SELECT COALESCE(sum(stored_bytes),0) FROM replay_payloads) + (SELECT COALESCE(sum(stored_bytes),0) FROM replay_frames)").Scan(&remaining).Error; err != nil || remaining > total-oldest {
		t.Fatalf("stored bytes=%d exceed cap=%d: %v", remaining, total-oldest, err)
	}
}

func TestReplayAcceptancePlansLinksAppendOnlyAndAccountCascade(t *testing.T) {
	db, _, _ := integrationStore(t)
	saveTestAccount(t, db, "journal")
	saveTestAccount(t, db, "unrelated")
	ctx := context.Background()
	plan := ReplayPlan{AccountID: "journal", MissionID: "mission", ShipID: "private-ship", Phase: "buy", Generation: "generation", Sequence: 2, ObservedAt: time.Now().Add(-100 * 24 * time.Hour), Plan: json.RawMessage(`{"economics":{"expected_net_pence":900}}`), Policy: json.RawMessage(`{"reserve_pence":75000}`), Revision: "synthetic-revision"}
	if err := db.RecordReplayPlan(ctx, plan); err != nil {
		t.Fatal(err)
	}
	link := ReplayActionLink{AccountID: "journal", ActionID: "action", MissionID: "mission", Stage: "buy", Generation: "generation", Sequence: 2}
	if err := db.LinkReplayAction(ctx, link); err != nil {
		t.Fatal(err)
	}
	changed := plan
	changed.Plan = json.RawMessage(`{"economics":{"expected_net_pence":1}}`)
	_ = db.RecordReplayPlan(ctx, changed)
	changedLink := link
	changedLink.Stage = "sell"
	_ = db.LinkReplayAction(ctx, changedLink)
	for _, prune := range []func() error{
		func() error { return db.Prune(ctx) },
		func() error { _, err := db.PruneReplay(ctx, time.Hour, 1); return err },
	} {
		if err := prune(); err != nil {
			t.Fatal(err)
		}
	}
	plans, err := db.ReplayPlans(ctx, "journal")
	if err != nil || len(plans) == 0 || !bytes.Equal(plans[0].Plan, plan.Plan) || plans[0].Revision != plan.Revision || !bytes.Equal(plans[0].Policy, plan.Policy) {
		t.Fatalf("original plan was overwritten or pruned: %+v, %v", plans, err)
	}
	links, err := db.ReplayActionLinks(ctx, "journal", plan.ObservedAt, time.Now().Add(time.Hour), []string{"mission"})
	if err != nil || len(links) == 0 || links[0].Stage != "buy" {
		t.Fatalf("original action link was overwritten or pruned: %+v, %v", links, err)
	}
	if other, err := db.ReplayPlans(ctx, "unrelated"); err != nil || len(other) != 0 {
		t.Fatalf("plan account isolation: %+v, %v", other, err)
	}
	if other, err := db.ReplayActionLinks(ctx, "unrelated", plan.ObservedAt, time.Now().Add(time.Hour), []string{"mission"}); err != nil || len(other) != 0 {
		t.Fatalf("link account isolation: %+v, %v", other, err)
	}
	replayAcceptanceRecord(t, db, replayAcceptanceFrame("journal", "cascade", time.Now(), []byte(`{"cascade":true}`)))
	if err := db.db.Exec("DELETE FROM accounts WHERE id = ?", "journal").Error; err != nil {
		t.Fatal(err)
	}
	for _, table := range []string{"replay_frames", "replay_payloads", "replay_plans", "replay_action_links"} {
		if count := replayAcceptanceCount(t, db, table, "journal"); count != 0 {
			t.Fatalf("account deletion left %d rows in %s", count, table)
		}
	}
}

func TestReplayAcceptanceDiagnosticGenerationIdempotence(t *testing.T) {
	db, _, _ := integrationStore(t)
	saveTestAccount(t, db, "diagnostic")
	frame := replayAcceptanceFrame("diagnostic", "diagnostic:42", time.Now().UTC(), []byte(`{"book":1}`))
	frame.Source = "diagnostic_backfill"
	replayAcceptanceRecord(t, db, frame)
	// A conflicting rerun cannot introduce an unused payload or replace the frame.
	frame.Datasets["books/private-port"] = ReplayDataset{Data: []byte(`{"book":2}`), Meta: json.RawMessage(`{"complete":true}`)}
	replayAcceptanceRecord(t, db, frame)
	if replayAcceptanceCount(t, db, "replay_frames", "diagnostic") != 1 || replayAcceptanceCount(t, db, "replay_payloads", "diagnostic") != 1 {
		t.Fatal("diagnostic generation rerun inserted a frame or payload")
	}
	frames := replayAcceptanceFrames(t, db, "diagnostic")
	if !bytes.Equal(frames[0].Datasets["books/private-port"].Data, []byte(`{"book":1}`)) {
		t.Fatal("diagnostic rerun replaced original evidence")
	}
}

func TestReplayAcceptanceWriteTimeoutPreservesOwnerContext(t *testing.T) {
	db, _, _ := integrationStore(t)
	saveTestAccount(t, db, "owner")
	owner, release, err := db.AcquireAccountContext(context.Background(), "owner")
	if err != nil {
		t.Fatal(err)
	}
	defer release()
	for _, statement := range []string{
		"CREATE FUNCTION delay_replay_frame_acceptance() RETURNS trigger LANGUAGE plpgsql AS $$ BEGIN PERFORM pg_sleep(1); RETURN NEW; END $$",
		"CREATE TRIGGER delay_replay_frame_acceptance BEFORE INSERT ON replay_frames FOR EACH ROW EXECUTE FUNCTION delay_replay_frame_acceptance()",
	} {
		if err := db.db.Exec(statement).Error; err != nil {
			t.Fatal(err)
		}
	}
	writeCtx, cancel := context.WithTimeout(owner, 100*time.Millisecond)
	defer cancel()
	if _, err := db.RecordReplayFrame(writeCtx, replayAcceptanceFrame("owner", "timeout", time.Now().UTC(), []byte(`{"book":1}`))); err == nil {
		t.Fatal("delayed replay write ignored its deadline")
	}
	if owner.Err() != nil {
		t.Fatalf("best-effort replay timeout canceled account ownership: %v", owner.Err())
	}
	if err := db.PutMission(owner, &Mission{AccountID: "owner", ShipID: "ship", Generation: "mission", Phase: "buy", Data: json.RawMessage(`{}`)}); err != nil {
		t.Fatalf("authoritative mission write failed after replay timeout: %v", err)
	}
	if replayAcceptanceCount(t, db, "replay_frames", "owner") != 0 || replayAcceptanceCount(t, db, "replay_payloads", "owner") != 0 {
		t.Fatal("timed-out frame transaction retained partial replay evidence")
	}
}
