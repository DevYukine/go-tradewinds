package store

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"testing"
	"time"

	"go.uber.org/zap"
)

// A completed HTTP 429 carries its durable retry deadline as dedicated
// nullable metadata, never inferred from the stored response bytes. The
// deadline must survive restarts without losing precision, repeated identical
// completions must stay idempotent after canonicalization, and no caller or
// direct SQL write may add, change, or remove the deadline of a terminal
// action. Outcomes recorded without metadata stay NULL and must not gain a
// guessed deadline later.
func TestPostgresRetryDeadlineSurvivesReplayAndResistsMutation(t *testing.T) {
	db, dsn, key := integrationStore(t)
	ctx := context.Background()
	saveTestAccount(t, db, "one")

	// The stored response keeps exact raw bytes, including non-JSON content
	// and surrounding whitespace, so a restart replays the upstream outcome
	// verbatim instead of a re-encoded copy.
	raw := []byte(" \t rate limited: slow down \n")
	body := json.RawMessage(`{"step":"register"}`)
	intent := &Action{
		ID:        "retry-deadline",
		AccountID: "one",
		Method:    http.MethodPost,
		Path:      "/v1/player/register",
		Body:      body,
	}
	if _, err := db.BeginAction(ctx, intent); err != nil {
		t.Fatal(err)
	}
	// A sub-microsecond tail must round upward: PostgreSQL timestamptz keeps
	// microseconds, and shortening the deadline would release a replacement
	// request before the upstream cooldown ends.
	input := time.Now().UTC().Add(30 * time.Second).Add(7 * time.Nanosecond)
	if err := db.CompleteAction(ctx, intent.ID, http.StatusTooManyRequests, raw, &input); err != nil {
		t.Fatal(err)
	}

	replayed, err := db.BeginAction(ctx, intent)
	if err != nil {
		t.Fatal(err)
	}
	if replayed.State != ActionComplete || replayed.Status != http.StatusTooManyRequests || !bytes.Equal(replayed.Response, raw) {
		t.Fatalf("replay lost the exact raw outcome: %+v", replayed)
	}
	if replayed.RetryAfter == nil {
		t.Fatal("completed 429 replayed without its durable retry deadline")
	}
	stored := *replayed.RetryAfter
	if stored.Before(input) || stored.Sub(input) >= time.Microsecond {
		t.Fatalf("stored deadline %v is not the conservative microsecond ceiling of %v", stored, input)
	}
	if stored.Nanosecond()%1000 != 0 {
		t.Fatalf("stored deadline %v is not normalized to the microsecond grid", stored)
	}

	// The identical completion, replayed from the same unnormalized input, is
	// idempotent once both sides are canonicalized.
	if err := db.CompleteAction(ctx, intent.ID, http.StatusTooManyRequests, raw, &input); err != nil {
		t.Fatalf("identical completion was not idempotent: %v", err)
	}
	later := input.Add(time.Second)
	if err := db.CompleteAction(ctx, intent.ID, http.StatusTooManyRequests, raw, &later); !errors.Is(err, ErrActionConflict) {
		t.Fatalf("terminal deadline was changed through the API: %v", err)
	}
	if err := db.CompleteAction(ctx, intent.ID, http.StatusTooManyRequests, raw, nil); !errors.Is(err, ErrActionConflict) {
		t.Fatalf("terminal deadline was removed through the API: %v", err)
	}
	if err := db.db.Exec("UPDATE actions SET retry_after = $1 WHERE id = $2", later, intent.ID).Error; err == nil {
		t.Fatal("direct SQL mutated the terminal retry deadline")
	}

	// A legacy completed 429 has no stored deadline: it replays NULL, stays
	// idempotently NULL, and must not gain a backfilled deadline.
	legacy := &Action{
		ID:        "legacy-null-deadline",
		AccountID: "one",
		Method:    http.MethodPost,
		Path:      "/v1/player/register",
		Body:      body,
	}
	if _, err := db.BeginAction(ctx, legacy); err != nil {
		t.Fatal(err)
	}
	if err := db.CompleteAction(ctx, legacy.ID, http.StatusTooManyRequests, raw, nil); err != nil {
		t.Fatal(err)
	}
	legacyReplay, err := db.BeginAction(ctx, legacy)
	if err != nil {
		t.Fatal(err)
	}
	if legacyReplay.RetryAfter != nil || !bytes.Equal(legacyReplay.Response, raw) {
		t.Fatalf("legacy outcome gained metadata or lost bytes: %+v", legacyReplay)
	}
	if err := db.CompleteAction(ctx, legacy.ID, http.StatusTooManyRequests, raw, nil); err != nil {
		t.Fatalf("identical NULL completion was not idempotent: %v", err)
	}
	if err := db.CompleteAction(ctx, legacy.ID, http.StatusTooManyRequests, raw, &later); !errors.Is(err, ErrActionConflict) {
		t.Fatalf("a guessed deadline was backfilled onto a legacy outcome: %v", err)
	}

	if err := db.Close(); err != nil {
		t.Fatal(err)
	}
	reopened, err := Open(ctx, dsn, key, zap.NewNop())
	if err != nil {
		t.Fatalf("restart with existing schema: %v", err)
	}
	t.Cleanup(func() {
		if err := reopened.Close(); err != nil {
			t.Error(err)
		}
	})
	restarted, err := reopened.BeginAction(ctx, intent)
	if err != nil {
		t.Fatal(err)
	}
	if restarted.Status != http.StatusTooManyRequests || !bytes.Equal(restarted.Response, raw) {
		t.Fatalf("restart lost the exact raw outcome: %+v", restarted)
	}
	if restarted.RetryAfter == nil || !restarted.RetryAfter.Equal(stored) || restarted.RetryAfter.Nanosecond()%1000 != 0 {
		t.Fatalf("restart changed the durable deadline: %+v", restarted.RetryAfter)
	}
	restartedLegacy, err := reopened.BeginAction(ctx, legacy)
	if err != nil {
		t.Fatal(err)
	}
	if restartedLegacy.RetryAfter != nil {
		t.Fatalf("restart invented a deadline for a legacy outcome: %+v", restartedLegacy.RetryAfter)
	}
}
