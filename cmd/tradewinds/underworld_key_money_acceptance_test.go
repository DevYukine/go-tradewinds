package main

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"slices"
	"strings"
	"sync"
	"testing"

	"github.com/DevYukine/go-tradewinds/internal/store"
	"github.com/google/uuid"
)

func TestUnderworldMoneyKeyCommandJournaledIdempotentAndSeparate(t *testing.T) {
	db, _, schema := registrationStore(t)
	ctx := context.Background()
	const company = "11111111-1111-4111-8111-111111111111"
	const handle = "UnderworldOwner"
	const mainKey = "main-fleet-opaque-key-byte-for-byte"
	const observerKey = "read-only-observer-opaque-key-24681"
	credentials := store.Credentials{Handle: handle, Passphrase: "encrypted-existing-passphrase", APIKey: mainKey}
	originalScopes := append([]string(nil), automationScopes[:]...)
	var mu sync.Mutex
	creates := 0
	var accountID string
	var requestBody []byte
	var requestID string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		defer mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case "/v1/auth/whoami":
			scopes := originalScopes
			if r.Header.Get("Authorization") == "Bearer "+observerKey {
				scopes = []string{"read", "underworld"}
			}
			_ = json.NewEncoder(w).Encode(map[string]any{"company_id": company, "scopes": scopes})
		case "/v1/auth/create-key":
			creates++
			body, err := io.ReadAll(r.Body)
			if err != nil {
				t.Error(err)
				w.WriteHeader(400)
				return
			}
			requestBody = body
			var request struct {
				Handle     string   `json:"handle"`
				Passphrase string   `json:"passphrase"`
				CompanyID  string   `json:"company_id"`
				Label      string   `json:"label"`
				Scopes     []string `json:"scopes"`
			}
			if err := json.Unmarshal(body, &request); err != nil {
				t.Error(err)
				w.WriteHeader(400)
				return
			}
			if r.Method != http.MethodPost || request.Handle != handle || request.Passphrase != credentials.Passphrase || request.CompanyID != company || !slices.Equal(request.Scopes, []string{"read", "underworld"}) || strings.TrimSpace(request.Label) == "" || request.Label == "Tradewinds fleet automation" {
				t.Errorf("underworld key creation did not preserve exact owner/scopes/distinct label: %+v", request)
				w.WriteHeader(400)
				return
			}
			stored, err := db.Credentials(ctx, accountID)
			if err != nil || stored.UnderworldMintID == "" {
				t.Errorf("mint identity was not persisted before send: %v", err)
				w.WriteHeader(http.StatusInternalServerError)
				return
			}
			key := uuid.NewSHA1(uuid.NameSpaceURL, []byte(accountID+"/bootstrap/underworld-key/"+stored.UnderworldMintID)).String()
			requestID = r.Header.Get("Idempotency-Key")
			if requestID != key {
				t.Errorf("underworld key uses wrong persisted mint identity: %q want %q", requestID, key)
			}
			pending, err := db.PendingActions(ctx, accountID)
			if err != nil {
				t.Error(err)
			}
			found := false
			for _, action := range pending {
				found = found || action.ID == key && action.Method == http.MethodPost && action.Path == r.URL.Path && bytes.Equal(action.Body, body)
			}
			if !found {
				t.Error("create-key reached server before its exact body and scoped idempotency key were persisted")
			}
			w.WriteHeader(http.StatusCreated)
			_ = json.NewEncoder(w).Encode(map[string]string{"api_key": observerKey})
		default:
			t.Errorf("underworld-key attempted unrelated operation: %s %s", r.Method, r.URL.Path)
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	defer server.Close()
	accountID = uuid.NewSHA1(uuid.NameSpaceURL, []byte(server.URL+"/player/"+strings.ToLower(handle))).String()
	if err := db.SaveAccount(ctx, store.Account{ID: accountID, Label: handle, CompanyID: company, Enabled: true}, credentials); err != nil {
		t.Fatal(err)
	}
	parsed, err := url.Parse(os.Getenv("TEST_DATABASE_URL"))
	if err != nil {
		t.Fatal(err)
	}
	query := parsed.Query()
	query.Set("search_path", schema)
	parsed.RawQuery = query.Encode()
	t.Setenv("DATABASE_URL", parsed.String())
	t.Setenv("CREDENTIAL_KEY", base64.StdEncoding.EncodeToString(bytes.Repeat([]byte{0x42}, 32)))
	t.Setenv("TRADEWINDS_API_URL", server.URL)
	for range 2 {
		if err := command([]string{"underworld-key", "-handle", handle}); err != nil {
			t.Fatalf("underworld-key command: %v", err)
		}
		loaded, err := db.Credentials(ctx, accountID)
		if err != nil {
			t.Fatal(err)
		}
		var fields map[string]string
		raw, err := json.Marshal(loaded)
		if err != nil {
			t.Fatal(err)
		}
		if err := json.Unmarshal(raw, &fields); err != nil {
			t.Fatal(err)
		}
		if loaded.APIKey != mainKey || loaded.Handle != credentials.Handle || loaded.Passphrase != credentials.Passphrase || fields["underworld_api_key"] != observerKey {
			t.Fatal("command replaced main credentials or did not save the separate encrypted key")
		}
	}
	mu.Lock()
	count, body, actionID := creates, append([]byte(nil), requestBody...), requestID
	mu.Unlock()
	if count != 1 {
		t.Fatalf("rerun minted %d observer keys; want exactly one", count)
	}
	if !slices.Equal(automationScopes[:], []string{"read", "fleet_navigate", "fleet_cargo", "market_spot", "market_derivatives", "infra_manage", "treasury_remit", "account_manage"}) {
		t.Fatalf("main automation permissions changed: %v", automationScopes)
	}
	action, err := db.BeginAction(ctx, &store.Action{ID: actionID, AccountID: accountID, Method: http.MethodPost, Path: "/v1/auth/create-key", Body: body})
	if err != nil || action == nil || action.Status != http.StatusCreated {
		t.Fatalf("rerun lost completed journal step: %+v %v", action, err)
	}
	history, err := db.History(ctx, accountID, "requests", 20)
	if err != nil {
		t.Fatal(err)
	}
	raw, err := json.Marshal(history)
	if err != nil {
		t.Fatal(err)
	}
	for _, secret := range []string{mainKey, observerKey, credentials.Passphrase} {
		if bytes.Contains(raw, []byte(secret)) {
			t.Fatal("bootstrap diagnostics leaked a stored or newly minted credential")
		}
	}
}
