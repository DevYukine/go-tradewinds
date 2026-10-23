package main

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/DevYukine/go-tradewinds/internal/config"
	"github.com/DevYukine/go-tradewinds/internal/names"
	"github.com/DevYukine/go-tradewinds/internal/store"
	"github.com/google/uuid"
	"go.uber.org/zap"
)

// A completed bootstrap 429 whose stored response is the raw upstream body is
// legacy data, even when that body carries a "not_before" member that merely
// resembles the locally encoded throttle envelope. Only a locally persisted
// envelope proves the durable deadline; a legacy body must fail closed and
// demand reconciliation instead of unlocking a replacement registration while
// the original cooldown may still be outstanding.
func TestRegisterRejectsLegacyThrottleResponseMimickingEnvelope(t *testing.T) {
	db, _, _ := registrationStore(t)
	var mu sync.Mutex
	var attempts []bootstrapAttempt
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		payload, err := io.ReadAll(r.Body)
		if err != nil {
			t.Error(err)
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		mu.Lock()
		attempts = append(attempts, bootstrapAttempt{
			ID:         r.Header.Get("Idempotency-Key"),
			Body:       payload,
			ReceivedAt: time.Now(),
		})
		mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusCreated)
		_, _ = w.Write([]byte(`{}`))
	}))
	defer server.Close()

	cfg := config.Config{APIURL: server.URL}
	accountID := uuid.NewSHA1(uuid.NameSpaceURL, []byte(server.URL+"/player/"+strings.ToLower(names.DefaultHandle))).String()
	credentials := store.Credentials{Handle: names.DefaultHandle, Passphrase: "synthetic-legacy-throttle-passphrase"}
	ctx, release, err := db.AcquireAccountContext(context.Background(), accountID)
	if err != nil {
		t.Fatal(err)
	}
	if err := db.SaveAccount(ctx, store.Account{ID: accountID, Label: credentials.Handle}, credentials); err != nil {
		release()
		t.Fatal(err)
	}

	// Seed the journal exactly as an older build left it: a completed 429
	// whose response is the raw upstream body, not the trusted local envelope.
	legacy := []byte(`{"code":"rate_limit_exceeded","not_before":"2000-01-01T00:00:00Z"}`)
	id := uuid.NewSHA1(uuid.NameSpaceURL, []byte(accountID+"/bootstrap/register")).String()
	request := map[string]string{"handle": credentials.Handle, "passphrase": credentials.Passphrase}
	encoded, err := json.Marshal(request)
	if err != nil {
		release()
		t.Fatal(err)
	}
	if _, err := db.BeginAction(ctx, &store.Action{
		ID:        id,
		AccountID: accountID,
		Method:    http.MethodPost,
		Path:      "/v1/player/register",
		Body:      encoded,
	}); err != nil {
		release()
		t.Fatal(err)
	}
	if err := db.CompleteAction(ctx, id, http.StatusTooManyRequests, legacy, nil); err != nil {
		release()
		t.Fatal(err)
	}
	release()

	err = register(cfg, db, zap.NewNop(), nil)
	if err == nil || !strings.Contains(err.Error(), "durable retry deadline") {
		t.Fatalf("legacy throttle response was trusted as a durable deadline: %v", err)
	}

	mu.Lock()
	observed := len(attempts)
	mu.Unlock()
	if observed != 0 {
		t.Fatalf("legacy completed throttle received %d replacement registration requests", observed)
	}
	replayed, err := db.BeginAction(context.Background(), &store.Action{
		ID:        id,
		AccountID: accountID,
		Method:    http.MethodPost,
		Path:      "/v1/player/register",
		Body:      encoded,
	})
	if err != nil || replayed.Status != http.StatusTooManyRequests || !bytes.Equal(replayed.Response, legacy) {
		t.Fatalf("rejected legacy outcome was not preserved intact: %+v, %v", replayed, err)
	}
	if pending, err := db.PendingActions(context.Background(), accountID); err != nil || len(pending) != 0 {
		t.Fatalf("legacy rejection staged a replacement identity: %+v, %v", pending, err)
	}
	unchanged, err := db.Credentials(context.Background(), accountID)
	if err != nil || unchanged != credentials {
		t.Fatalf("legacy rejection changed the stored account credentials: %+v, %v", unchanged, err)
	}
	accounts, err := db.Accounts(context.Background())
	if err != nil || len(accounts) != 1 || accounts[0].Enabled || accounts[0].CompanyID != "" {
		t.Fatalf("legacy rejection enabled an account or bound a company: %+v, %v", accounts, err)
	}
}
