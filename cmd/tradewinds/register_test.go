package main

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/DevYukine/go-tradewinds/internal/config"
	"github.com/DevYukine/go-tradewinds/internal/gameapi"
	"github.com/DevYukine/go-tradewinds/internal/names"
	"github.com/DevYukine/go-tradewinds/internal/store"
	"github.com/google/uuid"
	"go.uber.org/zap"
)

type bootstrapAttempt struct {
	ID         string
	Body       []byte
	ReceivedAt time.Time
}

func TestRegisterRepairsCompanyKeyWithoutRepeatingBootstrap(t *testing.T) {
	db, _, _ := registrationStore(t)
	companyID := uuid.NewString()
	oldScopes := []string{"read", "fleet_navigate", "fleet_cargo", "market_spot", "infra_manage", "treasury_remit", "account_manage"}
	credentials := store.Credentials{Handle: names.DefaultHandle, Passphrase: "persisted-test-passphrase", APIKey: "old-company-key"}
	var mu sync.Mutex
	var mintedScopes []string
	keyCreations := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		defer mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		scopes := oldScopes
		if r.Header.Get("Authorization") == "Bearer repaired-company-key" {
			scopes = mintedScopes
		}
		switch r.URL.Path {
		case "/v1/player/profile":
			_ = json.NewEncoder(w).Encode(map[string]any{"companies": []bootstrapCompany{{ID: companyID, Name: names.DefaultCompany, HomePortID: "le_havre"}}})
		case "/v1/auth/whoami":
			_ = json.NewEncoder(w).Encode(map[string]any{"company_id": companyID, "scopes": scopes})
		case "/v1/auth/create-key":
			var request struct {
				Handle     string   `json:"handle"`
				Passphrase string   `json:"passphrase"`
				CompanyID  string   `json:"company_id"`
				Scopes     []string `json:"scopes"`
			}
			if err := json.NewDecoder(r.Body).Decode(&request); err != nil {
				t.Error(err)
				w.WriteHeader(http.StatusBadRequest)
				return
			}
			if request.Handle != credentials.Handle || request.Passphrase != credentials.Passphrase || request.CompanyID != companyID {
				w.WriteHeader(http.StatusUnauthorized)
				return
			}
			keyCreations++
			mintedScopes = request.Scopes
			w.WriteHeader(http.StatusCreated)
			_, _ = w.Write([]byte(`{"api_key":"repaired-company-key"}`))
		case "/v1/contract/deliver-charter":
			if !slices.Contains(scopes, "market_derivatives") {
				w.WriteHeader(http.StatusForbidden)
				_, _ = w.Write([]byte(`{"code":"missing_scope","detail":"market_derivatives required"}`))
				return
			}
			_, _ = w.Write([]byte(`{"delivered_quantity":24,"reward_paid_pence":526992}`))
		default:
			t.Errorf("unexpected bootstrap action: %s %s", r.Method, r.URL.Path)
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	defer server.Close()

	accountID := uuid.NewSHA1(uuid.NameSpaceURL, []byte(server.URL+"/player/"+strings.ToLower(credentials.Handle))).String()
	account := store.Account{ID: accountID, Label: credentials.Handle, CompanyID: companyID, Enabled: true}
	ctx, release, err := db.AcquireAccountContext(context.Background(), accountID)
	if err != nil {
		t.Fatal(err)
	}
	if err := db.SaveAccount(ctx, account, credentials); err != nil {
		release()
		t.Fatal(err)
	}
	oldBody, err := json.Marshal(map[string]any{
		"handle": credentials.Handle, "passphrase": credentials.Passphrase,
		"label": "Tradewinds fleet automation", "company_id": companyID, "scopes": oldScopes,
	})
	if err != nil {
		release()
		t.Fatal(err)
	}
	oldAction, err := db.BeginAction(ctx, &store.Action{
		ID:        uuid.NewSHA1(uuid.NameSpaceURL, []byte(accountID+"/bootstrap/key/"+companyID)).String(),
		AccountID: accountID,
		Method:    http.MethodPost,
		Path:      "/v1/auth/create-key",
		Body:      oldBody,
	})
	if err == nil {
		err = db.CompleteAction(ctx, oldAction.ID, http.StatusCreated, json.RawMessage(`{"api_key":"old-company-key"}`), nil)
	}
	release()
	if err != nil {
		t.Fatal(err)
	}

	cfg := config.Config{APIURL: server.URL}
	if err := register(cfg, db, zap.NewNop(), nil); err != nil {
		t.Fatalf("repair existing registration: %v", err)
	}
	repaired, err := db.Credentials(context.Background(), accountID)
	if err != nil {
		t.Fatal(err)
	}
	client := gameapi.New(server.URL, repaired.APIKey, zap.NewNop())
	var result struct {
		Delivered int64 `json:"delivered_quantity"`
	}
	if err := client.Do(context.Background(), http.MethodPost, "/v1/contract/deliver-charter", json.RawMessage(`{"quantity":24}`), uuid.NewString(), &result); err != nil {
		t.Fatalf("stored company key still cannot complete the charter: %v", err)
	}
	if result.Delivered != 24 || repaired.Passphrase != credentials.Passphrase {
		t.Fatal("key repair changed player credentials or failed to deliver the existing cargo")
	}
	if err := register(cfg, db, zap.NewNop(), nil); err != nil {
		t.Fatalf("resume repaired registration: %v", err)
	}
	mu.Lock()
	defer mu.Unlock()
	if keyCreations != 1 {
		t.Fatalf("repeated registration minted %d keys; want exactly one permission repair", keyCreations)
	}
	accounts, err := db.Accounts(context.Background())
	if err != nil || len(accounts) != 1 || accounts[0].CompanyID != companyID {
		t.Fatalf("registration did not preserve the original account/company: %v", err)
	}
}

func TestRegisterResumesThrottledBootstrapAfterRestart(t *testing.T) {
	db, _, schema := registrationStore(t)
	var mu sync.Mutex
	var attempts []bootstrapAttempt
	playersCreated := 0
	companiesCreated := 0
	keysCreated := 0
	company := bootstrapCompany{
		ID:         uuid.NewString(),
		Name:       names.DefaultCompany,
		HomePortID: "le_havre",
	}
	var player struct {
		Handle     string `json:"handle"`
		Passphrase string `json:"passphrase"`
	}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		defer mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		payload, err := io.ReadAll(r.Body)
		if err != nil {
			t.Error(err)
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		status := http.StatusOK
		var response any
		switch r.URL.Path {
		case "/v1/player/register":
			attempts = append(attempts, bootstrapAttempt{
				ID:         r.Header.Get("Idempotency-Key"),
				Body:       payload,
				ReceivedAt: time.Now(),
			})
			if err := json.Unmarshal(payload, &player); err != nil {
				t.Error(err)
				w.WriteHeader(http.StatusBadRequest)
				return
			}
			if len(attempts) == 1 || attempts[len(attempts)-1].ID == attempts[0].ID {
				w.Header().Set("Retry-After", "1")
				status = http.StatusTooManyRequests
				response = map[string]string{"code": "rate_limit_exceeded", "detail": "retry after the auth cooldown"}
			} else {
				playersCreated++
				status = http.StatusCreated
				response = map[string]any{"player": map[string]string{"id": "registered-player"}}
			}
		case "/v1/auth/create-key":
			var request struct {
				Handle     string `json:"handle"`
				Passphrase string `json:"passphrase"`
				CompanyID  string `json:"company_id"`
			}
			if err := json.Unmarshal(payload, &request); err != nil {
				t.Error(err)
				w.WriteHeader(http.StatusBadRequest)
				return
			}
			if request.Handle != player.Handle || request.Passphrase != player.Passphrase {
				w.WriteHeader(http.StatusUnauthorized)
				return
			}
			key := "tw_test_bootstrap_player"
			if request.CompanyID != "" {
				if request.CompanyID != company.ID {
					w.WriteHeader(http.StatusForbidden)
					return
				}
				key = "tw_test_bootstrap_company"
			}
			keysCreated++
			status = http.StatusCreated
			response = map[string]string{"api_key": key}
		case "/v1/player/profile":
			companies := []bootstrapCompany{}
			if companiesCreated != 0 {
				companies = append(companies, company)
			}
			response = map[string]any{"companies": companies}
		case "/v1/company/create":
			var request struct {
				Name            string `json:"name"`
				HomePortID      string `json:"home_port_id"`
				StarterShipName string `json:"starter_ship_name"`
			}
			if err := json.Unmarshal(payload, &request); err != nil {
				t.Error(err)
				w.WriteHeader(http.StatusBadRequest)
				return
			}
			if request.Name != company.Name || request.HomePortID != company.HomePortID || request.StarterShipName != names.Next(nil) {
				w.WriteHeader(http.StatusBadRequest)
				return
			}
			companiesCreated++
			status = http.StatusCreated
			response = map[string]any{"company": company}
		case "/v1/auth/whoami":
			response = map[string]any{"company_id": company.ID, "scopes": automationScopes[:]}
		default:
			t.Errorf("unexpected bootstrap request: %s %s", r.Method, r.URL.Path)
			w.WriteHeader(http.StatusNotFound)
			return
		}
		w.WriteHeader(status)
		if err := json.NewEncoder(w).Encode(response); err != nil {
			t.Error(err)
		}
	}))
	defer server.Close()
	cfg := config.Config{APIURL: server.URL}
	var upstream *gameapi.APIError
	if err := register(cfg, db, zap.NewNop(), nil); !errors.As(err, &upstream) || upstream.Status != http.StatusTooManyRequests {
		t.Fatalf("first registration did not retain the definitive throttle: %v", err)
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}

	// Reopen the encrypted records, rather than relying on a live registration
	// object or a client's in-memory retry state to find the next attempt.
	dsn := os.Getenv("TEST_DATABASE_URL")
	parsed, err := url.Parse(dsn)
	if err == nil && (parsed.Scheme == "postgres" || parsed.Scheme == "postgresql") {
		query := parsed.Query()
		query.Set("search_path", schema)
		parsed.RawQuery = query.Encode()
		dsn = parsed.String()
	} else {
		dsn += " search_path=" + schema
	}
	reopened, err := store.Open(context.Background(), dsn, bytes.Repeat([]byte{0x42}, 32), zap.NewNop())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := reopened.Close(); err != nil {
			t.Error(err)
		}
	})
	if err := register(cfg, reopened, zap.NewNop(), nil); err != nil {
		t.Fatalf("resume after the persisted throttle: %v", err)
	}
	if err := register(cfg, reopened, zap.NewNop(), nil); err != nil {
		t.Fatalf("restart after successful registration: %v", err)
	}

	mu.Lock()
	defer mu.Unlock()
	if len(attempts) != 2 || attempts[0].ID == "" || attempts[0].ID == attempts[1].ID {
		t.Fatalf("throttled registration did not get a fresh action identity: %+v", attempts)
	}
	if !bytes.Equal(attempts[0].Body, attempts[1].Body) {
		t.Fatal("registration retry changed the persisted player credentials")
	}
	if attempts[1].ReceivedAt.Before(attempts[0].ReceivedAt.Add(time.Second)) {
		t.Fatal("replacement registration crossed Retry-After")
	}
	if playersCreated != 1 || companiesCreated != 1 || keysCreated != 2 {
		t.Fatalf("registration repeated committed bootstrap work: players=%d companies=%d keys=%d", playersCreated, companiesCreated, keysCreated)
	}
	accounts, err := reopened.Accounts(context.Background())
	if err != nil || len(accounts) != 1 || !accounts[0].Enabled || accounts[0].CompanyID != company.ID {
		t.Fatalf("registration did not finish with exactly one enabled company: %+v, %v", accounts, err)
	}
	credentials, err := reopened.Credentials(context.Background(), accounts[0].ID)
	if err != nil || credentials.Passphrase != player.Passphrase || credentials.APIKey != "tw_test_bootstrap_company" {
		t.Fatalf("restart did not preserve the credentials and final company key: %v", err)
	}
	original, err := reopened.BeginAction(context.Background(), &store.Action{
		ID:        attempts[0].ID,
		AccountID: accounts[0].ID,
		Method:    http.MethodPost,
		Path:      "/v1/player/register",
		Body:      attempts[0].Body,
	})
	if err != nil || original.Status != http.StatusTooManyRequests {
		t.Fatalf("original throttle journal outcome was lost: %+v, %v", original, err)
	}
	if pending, err := reopened.PendingActions(context.Background(), accounts[0].ID); err != nil || len(pending) != 0 {
		t.Fatalf("registration left unresolved journal entries: %+v, %v", pending, err)
	}
}

func TestBootstrapKeepsAmbiguousExpiredAndRejectedActionIdentity(t *testing.T) {
	cases := []struct {
		name      string
		expired   bool
		rejection int
	}{
		{name: "transient failure"},
		{name: "expired unresolved action", expired: true},
		{name: "completed authorization rejection", rejection: http.StatusForbidden},
	}
	for _, test := range cases {
		t.Run(test.name, func(t *testing.T) {
			db, admin, schema := registrationStore(t)
			ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
			defer cancel()
			accountID := uuid.NewString()
			credentials := store.Credentials{Handle: "bootstrap-guard", Passphrase: "synthetic-bootstrap-guard-passphrase"}
			if err := db.SaveAccount(ctx, store.Account{ID: accountID, Label: credentials.Handle}, credentials); err != nil {
				t.Fatal(err)
			}
			body := map[string]string{"handle": credentials.Handle, "passphrase": credentials.Passphrase}
			encoded, err := json.Marshal(body)
			if err != nil {
				t.Fatal(err)
			}
			id := uuid.NewSHA1(uuid.NameSpaceURL, []byte(accountID+"/bootstrap/register")).String()
			if _, err := db.BeginAction(ctx, &store.Action{
				ID:        id,
				AccountID: accountID,
				Method:    http.MethodPost,
				Path:      "/v1/player/register",
				Body:      encoded,
			}); err != nil {
				t.Fatal(err)
			}
			if test.expired {
				// Insert a historical fixture in another isolated schema. The
				// immutable-action trigger correctly forbids aging an existing
				// journal row with UPDATE, even in a test.
				historical, _, historicalSchema := registrationStore(t)
				if err := historical.SaveAccount(ctx, store.Account{
					ID:    accountID,
					Label: credentials.Handle,
				}, credentials); err != nil {
					t.Fatal(err)
				}
				createdAt := time.Now().UTC().Add(-store.ActionReplayWindow - time.Minute)
				query := `INSERT INTO ` + historicalSchema + `.actions (
					id, account_id, company_id, method, path, body_cipher,
					state, status, response_cipher, created_at, updated_at
				)
				SELECT id, account_id, company_id, method, path, body_cipher,
					state, status, response_cipher, $1, $1
				FROM ` + schema + `.actions
				WHERE id = $2`
				result, err := admin.ExecContext(ctx, query, createdAt, id)
				if err != nil {
					t.Fatal(err)
				}
				if count, err := result.RowsAffected(); err != nil || count != 1 {
					t.Fatalf("historical journal fixture was not inserted: rows=%d, %v", count, err)
				}
				db = historical
			}
			if test.rejection != 0 {
				if err := db.CompleteAction(ctx, id, test.rejection, json.RawMessage(`{"code":"auth_forbidden"}`), nil); err != nil {
					t.Fatal(err)
				}
			}
			var mu sync.Mutex
			var attempts []bootstrapAttempt
			recovered := false
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				mu.Lock()
				defer mu.Unlock()
				payload, err := io.ReadAll(r.Body)
				if err != nil {
					t.Error(err)
					w.WriteHeader(http.StatusBadRequest)
					return
				}
				attempts = append(attempts, bootstrapAttempt{
					ID:         r.Header.Get("Idempotency-Key"),
					Body:       payload,
					ReceivedAt: time.Now(),
				})
				w.Header().Set("Content-Type", "application/json")
				if !recovered {
					w.WriteHeader(http.StatusServiceUnavailable)
					_, _ = w.Write([]byte(`{"code":"temporarily_unavailable"}`))
					return
				}
				w.WriteHeader(http.StatusCreated)
				_, _ = w.Write([]byte(`{"registered":true}`))
			}))
			defer server.Close()
			client := gameapi.New(server.URL, "", zap.NewNop())
			err = bootstrapAction(ctx, db, client, accountID, "register", "/v1/player/register", body, nil)
			if test.expired {
				if !errors.Is(err, store.ErrActionExpired) {
					t.Fatalf("expired unresolved action was not quarantined: %v", err)
				}
				pending, err := db.PendingActions(ctx, accountID)
				if err != nil || len(pending) != 1 || pending[0].ID != id ||
					!bytes.Equal(pending[0].Body, encoded) ||
					pending[0].CreatedAt.Add(store.ActionReplayWindow).After(time.Now()) {
					t.Fatalf("expired request identity or safety window changed: %+v, %v", pending, err)
				}
			} else if test.rejection != 0 {
				var upstream *gameapi.APIError
				if !errors.As(err, &upstream) || upstream.Status != test.rejection {
					t.Fatalf("definitive authorization rejection was replaced: %v", err)
				}
			} else {
				var upstream *gameapi.APIError
				if !errors.As(err, &upstream) || upstream.Status != http.StatusServiceUnavailable {
					t.Fatalf("transient failure was misclassified: %v", err)
				}
				pending, err := db.PendingActions(ctx, accountID)
				if err != nil || len(pending) != 1 || pending[0].ID != id || !bytes.Equal(pending[0].Body, encoded) {
					t.Fatalf("ambiguous request was not preserved: %+v, %v", pending, err)
				}
				mu.Lock()
				recovered = true
				mu.Unlock()
				restarted := gameapi.New(server.URL, "", zap.NewNop())
				if err := bootstrapAction(ctx, db, restarted, accountID, "register", "/v1/player/register", body, nil); err != nil {
					t.Fatalf("resume unresolved bootstrap with its original identity: %v", err)
				}
				if pending, err := db.PendingActions(ctx, accountID); err != nil || len(pending) != 0 {
					t.Fatalf("successful replay did not settle the original request: %+v, %v", pending, err)
				}
			}
			mu.Lock()
			defer mu.Unlock()
			if test.expired || test.rejection != 0 {
				if len(attempts) != 0 {
					t.Fatal("expired or definitively rejected bootstrap action was resent")
				}
				return
			}
			if len(attempts) < 2 {
				t.Fatal("transient recovery never reached both attempts")
			}
			for _, attempt := range attempts {
				if attempt.ID != id || !bytes.Equal(attempt.Body, encoded) {
					t.Fatal("ambiguous bootstrap action was replaced by a new identity or payload")
				}
			}
		})
	}
}

func TestBootstrapInterruptedThrottleWaitSurvivesRestart(t *testing.T) {
	source, admin, sourceSchema := registrationStore(t)
	db, _, schema := registrationStore(t)
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	account := store.Account{
		ID:    uuid.NewString(),
		Label: "bootstrap-cooldown-window",
	}
	credentials := store.Credentials{
		Handle:     account.Label,
		Passphrase: "synthetic-bootstrap-cooldown-passphrase",
	}
	for _, destination := range []*store.Store{source, db} {
		if err := destination.SaveAccount(ctx, account, credentials); err != nil {
			t.Fatal(err)
		}
	}
	body := map[string]string{
		"handle":     credentials.Handle,
		"passphrase": credentials.Passphrase,
	}
	encoded, err := json.Marshal(body)
	if err != nil {
		t.Fatal(err)
	}
	id := uuid.NewSHA1(uuid.NameSpaceURL, []byte(account.ID+"/bootstrap/register")).String()
	intent := &store.Action{
		ID:        id,
		AccountID: account.ID,
		Method:    http.MethodPost,
		Path:      "/v1/player/register",
		Body:      encoded,
	}
	if _, err := source.BeginAction(ctx, intent); err != nil {
		t.Fatal(err)
	}

	// Insert, rather than mutate, an immutable historical request whose replay
	// window ends during the server's longer Retry-After wait.
	createdAt := time.Now().Add(2 * time.Second).Add(-store.ActionReplayWindow)
	query := `INSERT INTO ` + schema + `.actions (
		id, account_id, company_id, method, path, body_cipher,
		state, status, response_cipher, created_at, updated_at
	)
	SELECT id, account_id, company_id, method, path, body_cipher,
		state, status, response_cipher, $1, $1
	FROM ` + sourceSchema + `.actions
	WHERE id = $2`
	inserted, err := admin.ExecContext(ctx, query, createdAt, id)
	if err != nil {
		t.Fatal(err)
	}
	if count, err := inserted.RowsAffected(); err != nil || count != 1 {
		t.Fatalf("near-deadline fixture was not inserted: rows=%d, %v", count, err)
	}

	const throttleBody = `{ "code":"rate_limit_exceeded", "detail":"wait for the auth quota" }`
	var mu sync.Mutex
	var attempts []bootstrapAttempt
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		payload, err := io.ReadAll(r.Body)
		if err != nil {
			t.Error(err)
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		requestID := r.Header.Get("Idempotency-Key")
		mu.Lock()
		attempts = append(attempts, bootstrapAttempt{
			ID:         requestID,
			Body:       payload,
			ReceivedAt: time.Now(),
		})
		mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		if requestID == id {
			w.Header().Set("Retry-After", "8")
			w.WriteHeader(http.StatusTooManyRequests)
			_, _ = w.Write([]byte(throttleBody))
			return
		}
		w.WriteHeader(http.StatusCreated)
		_, _ = w.Write([]byte(`{"registered":true}`))
	}))
	defer server.Close()

	err = bootstrapAction(ctx, db, gameapi.New(server.URL, "", zap.NewNop()),
		account.ID, "register", intent.Path, body, nil)
	var throttled *gameapi.APIError
	if !errors.As(err, &throttled) || throttled.Status != http.StatusTooManyRequests ||
		!errors.Is(err, context.DeadlineExceeded) || ctx.Err() != nil {
		t.Fatalf("expected an interrupted action wait with a still-live parent: %v", err)
	}
	if !throttled.RetryAfter.After(time.Now()) {
		t.Fatal("the replay deadline did not interrupt the longer server cooldown")
	}
	recorded, err := db.BeginAction(ctx, intent)
	if err != nil || recorded.Status != http.StatusTooManyRequests {
		t.Fatalf("interrupted cooldown lost the definitive rejection: %+v, %v", recorded, err)
	}
	if recorded.RetryAfter == nil || recorded.RetryAfter.Before(throttled.RetryAfter) ||
		recorded.RetryAfter.Sub(throttled.RetryAfter) >= time.Microsecond ||
		!bytes.Equal(recorded.Response, []byte(throttleBody)) {
		t.Fatal("journal lost the exact response or conservatively rounded deadline")
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}

	// Reopen encrypted state and create a new client. A short invocation must
	// report the stored cooldown without even staging a replacement action.
	dsn := os.Getenv("TEST_DATABASE_URL")
	parsed, err := url.Parse(dsn)
	if err == nil && (parsed.Scheme == "postgres" || parsed.Scheme == "postgresql") {
		parameters := parsed.Query()
		parameters.Set("search_path", schema)
		parsed.RawQuery = parameters.Encode()
		dsn = parsed.String()
	} else {
		dsn += " search_path=" + schema
	}
	reopened, err := store.Open(ctx, dsn, bytes.Repeat([]byte{0x42}, 32), zap.NewNop())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := reopened.Close(); err != nil {
			t.Error(err)
		}
	})
	beforeCooldown, stop := context.WithTimeout(ctx, 500*time.Millisecond)
	err = bootstrapAction(beforeCooldown, reopened, gameapi.New(server.URL, "", zap.NewNop()),
		account.ID, "register", intent.Path, body, nil)
	stop()
	var restored *gameapi.APIError
	if !errors.As(err, &restored) || restored.Status != http.StatusTooManyRequests ||
		!restored.RetryAfter.Equal(*recorded.RetryAfter) || !restored.RetryAfter.After(time.Now()) ||
		!bytes.Equal(restored.Body, []byte(throttleBody)) {
		t.Fatalf("restart did not honor the durable deadline and exact raw response: %v", err)
	}
	if pending, err := reopened.PendingActions(ctx, account.ID); err != nil || len(pending) != 0 {
		t.Fatalf("restart staged a new identity before the cooldown: %+v, %v", pending, err)
	}
	mu.Lock()
	requestsBeforeCooldown := len(attempts)
	mu.Unlock()
	if requestsBeforeCooldown != 1 {
		t.Fatalf("restart sent a request before the cooldown: requests=%d", requestsBeforeCooldown)
	}

	timer := time.NewTimer(time.Until(restored.RetryAfter) + 20*time.Millisecond)
	defer timer.Stop()
	select {
	case <-timer.C:
	case <-ctx.Done():
		t.Fatal(ctx.Err())
	}
	var response struct {
		Registered bool `json:"registered"`
	}
	if err := bootstrapAction(ctx, reopened, gameapi.New(server.URL, "", zap.NewNop()),
		account.ID, "register", intent.Path, body, &response); err != nil || !response.Registered {
		t.Fatalf("registration did not resume after the durable cooldown: %v", err)
	}
	recorded, err = reopened.BeginAction(ctx, intent)
	if err != nil || recorded.Status != http.StatusTooManyRequests {
		t.Fatalf("replacement erased the original rejected attempt: %+v, %v", recorded, err)
	}

	// Old-format outcomes cannot prove whether an interrupted wait finished.
	// They must remain visible and blocked, not receive a guessed deadline.
	legacyStep := "legacy-throttle"
	legacy := &store.Action{
		ID:        uuid.NewSHA1(uuid.NameSpaceURL, []byte(account.ID+"/bootstrap/"+legacyStep)).String(),
		AccountID: account.ID,
		Method:    intent.Method,
		Path:      intent.Path,
		Body:      encoded,
	}
	if _, err := reopened.BeginAction(ctx, legacy); err != nil {
		t.Fatal(err)
	}
	if err := reopened.CompleteAction(ctx, legacy.ID, http.StatusTooManyRequests, []byte(throttleBody), nil); err != nil {
		t.Fatal(err)
	}
	err = bootstrapAction(ctx, reopened, gameapi.New(server.URL, "", zap.NewNop()),
		account.ID, legacyStep, intent.Path, body, nil)
	if err == nil || !strings.Contains(err.Error(), "durable retry deadline") {
		t.Fatalf("old-format rejection silently assumed the cooldown elapsed: %v", err)
	}
	if pending, err := reopened.PendingActions(ctx, account.ID); err != nil || len(pending) != 0 {
		t.Fatalf("old-format rejection staged a guessed replacement: %+v, %v", pending, err)
	}
	mu.Lock()
	observed := append([]bootstrapAttempt(nil), attempts...)
	mu.Unlock()
	if len(observed) != 2 || observed[0].ID != id || observed[1].ID == id || observed[1].ID == "" {
		t.Fatalf("expected only the rejected request and one distinct replacement: %+v", observed)
	}
	if !bytes.Equal(observed[0].Body, encoded) || !bytes.Equal(observed[1].Body, encoded) ||
		observed[1].ReceivedAt.Before(throttled.RetryAfter) {
		t.Fatal("replacement changed the immutable request or crossed the persisted cooldown")
	}
}

func TestUnderworldReaderNeverRetriesTransientResponse(t *testing.T) {
	var mu sync.Mutex
	requests := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		requests++
		mu.Unlock()
		if r.Method != http.MethodGet {
			t.Errorf("observer sent %s", r.Method)
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusServiceUnavailable)
		_, _ = w.Write([]byte(`{"code":"unavailable","detail":"try a later visit"}`))
	}))
	defer server.Close()
	reader := underworldReader{client: gameapi.New(server.URL, "observer-only-key", zap.NewNop())}
	var response json.RawMessage
	if err := reader.Get(context.Background(), "/v1/underworld/wants?port_id=flushing", &response); err == nil {
		t.Fatal("transient upstream failure was hidden")
	}
	mu.Lock()
	defer mu.Unlock()
	if requests != 1 {
		t.Fatalf("one arrival attempted %d HTTP reads", requests)
	}
}

func TestUnderworldKeyDoesNotCreateMissingAccount(t *testing.T) {
	db, _, _ := registrationStore(t)
	err := underworldKey(config.Config{APIURL: "https://tradewinds.invalid"}, db, zap.NewNop(), []string{"-handle", "Unregistered"})
	if err == nil || !strings.Contains(err.Error(), "existing company-bound account") {
		t.Fatalf("missing registration was not rejected: %v", err)
	}
	accounts, err := db.Accounts(context.Background())
	if err != nil || len(accounts) != 0 {
		t.Fatalf("observer command created an account: %+v %v", accounts, err)
	}
}

func TestUnderworldKeyReplacesOnlyPermanentlyRejectedStoredKeys(t *testing.T) {
	for _, test := range []struct {
		status  int
		code    string
		replace bool
	}{
		{401, "revoked_api_key", true},
		{401, "expired_api_key", true},
		{401, "invalid_api_key", true},
		{401, "other_auth_error", false},
		{403, "missing_scope", false},
		{503, "unavailable", false},
		{200, "", false},
	} {
		t.Run(test.code, func(t *testing.T) {
			db, _, _ := registrationStore(t)
			credentials := store.Credentials{Handle: names.DefaultHandle, Passphrase: "observer-mint-passphrase",
				APIKey: "main-automation-key", UnderworldAPIKey: "old-observer-key"}
			companyID := uuid.NewString()
			var ids []string
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Content-Type", "application/json")
				switch r.URL.Path {
				case "/v1/auth/whoami":
					if r.Header.Get("Authorization") == "Bearer old-observer-key" {
						w.WriteHeader(test.status)
						_ = json.NewEncoder(w).Encode(map[string]string{"code": test.code})
					} else {
						_, _ = w.Write([]byte(`{"scopes":["read","underworld"]}`))
					}
				case "/v1/auth/create-key":
					ids = append(ids, r.Header.Get("Idempotency-Key"))
					w.WriteHeader(http.StatusCreated)
					_, _ = w.Write([]byte(`{"api_key":"new-observer-key"}`))
				default:
					t.Errorf("unexpected observer bootstrap request: %s", r.URL.Path)
					w.WriteHeader(http.StatusNotFound)
				}
			}))
			defer server.Close()
			accountID := uuid.NewSHA1(uuid.NameSpaceURL, []byte(server.URL+"/player/"+strings.ToLower(credentials.Handle))).String()
			account := store.Account{ID: accountID, Label: credentials.Handle, CompanyID: companyID, Enabled: true}
			ctx := context.Background()
			if err := db.SaveAccount(ctx, account, credentials); err != nil {
				t.Fatal(err)
			}
			legacyID := uuid.NewSHA1(uuid.NameSpaceURL, []byte(accountID+"/bootstrap/key/"+companyID+"/read,underworld")).String()
			if _, err := db.BeginAction(ctx, &store.Action{ID: legacyID, AccountID: accountID,
				Method: http.MethodPost, Path: "/v1/auth/create-key", Body: json.RawMessage(`{"legacy":true}`)}); err != nil {
				t.Fatal(err)
			}
			if err := db.CompleteAction(ctx, legacyID, http.StatusCreated, json.RawMessage(`{"api_key":"old-observer-key"}`), nil); err != nil {
				t.Fatal(err)
			}
			for range 2 {
				if err := underworldKey(config.Config{APIURL: server.URL}, db, zap.NewNop(), nil); err != nil {
					t.Fatal(err)
				}
			}
			stored, err := db.Credentials(ctx, accountID)
			if err != nil {
				t.Fatal(err)
			}
			wantKey := credentials.UnderworldAPIKey
			wantMints := 0
			if test.replace {
				wantKey = "new-observer-key"
				wantMints = 1
			}
			if stored.UnderworldAPIKey != wantKey || stored.APIKey != credentials.APIKey ||
				stored.Passphrase != credentials.Passphrase || stored.UnderworldMintID != "" || len(ids) != wantMints {
				t.Fatalf("replacement policy changed credentials or mint count: observer=%q pending=%q mints=%d",
					stored.UnderworldAPIKey, stored.UnderworldMintID, len(ids))
			}
			if len(ids) > 0 && (ids[0] == "" || ids[0] == legacyID) {
				t.Fatal("replacement reused the deterministic legacy mint identity")
			}
		})
	}
}

func TestUnderworldKeyMintResumesInFlightAndRotatesIdentityForNextMint(t *testing.T) {
	db, _, _ := registrationStore(t)
	credentials := store.Credentials{Handle: names.DefaultHandle, Passphrase: "observer-retry-passphrase", APIKey: "main-key"}
	var attempts []bootstrapAttempt
	revoke := false
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case "/v1/auth/whoami":
			if revoke {
				w.WriteHeader(http.StatusUnauthorized)
				_, _ = w.Write([]byte(`{"code":"revoked_api_key"}`))
			} else {
				_, _ = w.Write([]byte(`{"scopes":["read","underworld"]}`))
			}
		case "/v1/auth/create-key":
			payload, err := io.ReadAll(r.Body)
			if err != nil {
				t.Error(err)
			}
			attempts = append(attempts, bootstrapAttempt{ID: r.Header.Get("Idempotency-Key"), Body: payload})
			if len(attempts) == 1 {
				w.WriteHeader(http.StatusConflict)
				_, _ = w.Write([]byte(`{"code":"idempotency_request_in_flight"}`))
				return
			}
			w.WriteHeader(http.StatusCreated)
			if revoke {
				_, _ = w.Write([]byte(`{"api_key":"replacement-observer-key"}`))
			} else {
				_, _ = w.Write([]byte(`{"api_key":"first-observer-key"}`))
			}
		default:
			t.Errorf("unexpected request: %s", r.URL.Path)
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	defer server.Close()
	accountID := uuid.NewSHA1(uuid.NameSpaceURL, []byte(server.URL+"/player/"+strings.ToLower(credentials.Handle))).String()
	account := store.Account{ID: accountID, Label: credentials.Handle, CompanyID: uuid.NewString(), Enabled: true}
	ctx := context.Background()
	if err := db.SaveAccount(ctx, account, credentials); err != nil {
		t.Fatal(err)
	}
	cfg := config.Config{APIURL: server.URL}
	var apiErr *gameapi.APIError
	if err := underworldKey(cfg, db, zap.NewNop(), nil); !errors.As(err, &apiErr) || apiErr.Status != 409 {
		t.Fatalf("expected unresolved mint, got %v", err)
	}
	pending, err := db.PendingActions(ctx, accountID)
	if err != nil || len(pending) != 1 || pending[0].ID != attempts[0].ID {
		t.Fatalf("mint identity was not durably journaled: %+v %v", pending, err)
	}
	stored, err := db.Credentials(ctx, accountID)
	if err != nil || stored.UnderworldMintID == "" || stored.UnderworldAPIKey != "" {
		t.Fatalf("pending mint recovery identity missing: %v", err)
	}
	if err := underworldKey(cfg, db, zap.NewNop(), nil); err != nil {
		t.Fatal(err)
	}
	if len(attempts) != 2 || attempts[0].ID != attempts[1].ID || !bytes.Equal(attempts[0].Body, attempts[1].Body) {
		t.Fatal("in-flight mint retry changed its durable identity or payload")
	}
	revoke = true
	if err := underworldKey(cfg, db, zap.NewNop(), nil); err != nil {
		t.Fatal(err)
	}
	if len(attempts) != 3 || attempts[2].ID == attempts[1].ID || attempts[2].ID == "" {
		t.Fatal("a new mint after revocation reused the preceding mint identity")
	}
	stored, err = db.Credentials(ctx, accountID)
	if err != nil || stored.UnderworldAPIKey != "replacement-observer-key" || stored.UnderworldMintID != "" ||
		stored.APIKey != credentials.APIKey {
		t.Fatalf("replacement was not committed without altering the main key: %v", err)
	}
}

func TestUnderworldKeyRefusesPendingLegacyMint(t *testing.T) {
	db, _, _ := registrationStore(t)
	requests := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests++
		t.Errorf("legacy recovery must not send another request: %s", r.URL.Path)
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer server.Close()
	ctx := context.Background()
	accountID := uuid.NewSHA1(uuid.NameSpaceURL, []byte(server.URL+"/player/"+strings.ToLower(names.DefaultHandle))).String()
	account := store.Account{ID: accountID, Label: names.DefaultHandle, CompanyID: uuid.NewString(), Enabled: true}
	credentials := store.Credentials{Handle: names.DefaultHandle, Passphrase: "legacy-observer-passphrase", APIKey: "main-key"}
	if err := db.SaveAccount(ctx, account, credentials); err != nil {
		t.Fatal(err)
	}
	id := uuid.NewSHA1(uuid.NameSpaceURL, []byte(accountID+"/bootstrap/key/"+account.CompanyID+"/read,underworld")).String()
	body, err := json.Marshal(map[string]any{
		"handle": credentials.Handle, "passphrase": credentials.Passphrase, "company_id": account.CompanyID,
		"label": "Tradewinds underworld observer", "scopes": []string{"read", "underworld"},
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.BeginAction(ctx, &store.Action{ID: id, AccountID: accountID,
		Method: http.MethodPost, Path: "/v1/auth/create-key", Body: body}); err != nil {
		t.Fatal(err)
	}
	if err := underworldKey(config.Config{APIURL: server.URL}, db, zap.NewNop(), nil); err == nil ||
		!strings.Contains(err.Error(), "reconcile the pending legacy underworld create-key") {
		t.Fatalf("legacy unresolved mint was not held for reconciliation: %v", err)
	}
	stored, err := db.Credentials(ctx, accountID)
	if err != nil || stored != credentials || requests != 0 {
		t.Fatalf("legacy hold changed credentials or sent a request: %v requests=%d", err, requests)
	}
	pending, err := db.PendingActions(ctx, accountID)
	if err != nil || len(pending) != 1 || pending[0].ID != id || !bytes.Equal(pending[0].Body, body) {
		t.Fatalf("legacy hold changed the unresolved journal: %+v %v", pending, err)
	}
}

func TestUnderworldKeyMintRetiresOnlyDefinitiveJournaledRejection(t *testing.T) {
	for _, test := range []struct {
		name      string
		status    int
		code      string
		journaled bool
		retire    bool
	}{
		{"new forbidden", 403, "forbidden", false, true},
		{"stored forbidden", 403, "forbidden", true, true},
		{"rate limited", 429, "rate_limit_exceeded", false, false},
		{"ambiguous conflict", 409, "idempotency_request_in_flight", false, false},
		{"unsettleable reused key", 422, "idempotency_key_reused", false, false},
	} {
		t.Run(test.name, func(t *testing.T) {
			db, _, _ := registrationStore(t)
			var ids []string
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.URL.Path != "/v1/auth/create-key" {
					t.Errorf("unexpected request: %s", r.URL.Path)
					w.WriteHeader(http.StatusNotFound)
					return
				}
				ids = append(ids, r.Header.Get("Idempotency-Key"))
				w.Header().Set("Content-Type", "application/json")
				if !test.journaled && len(ids) == 1 {
					if test.status == 429 {
						w.Header().Set("Retry-After", "1")
					}
					w.WriteHeader(test.status)
					_ = json.NewEncoder(w).Encode(map[string]string{"code": test.code})
					return
				}
				w.WriteHeader(http.StatusCreated)
				_, _ = w.Write([]byte(`{"api_key":"observer-after-rejection"}`))
			}))
			defer server.Close()
			ctx := context.Background()
			accountID := uuid.NewSHA1(uuid.NameSpaceURL, []byte(server.URL+"/player/"+strings.ToLower(names.DefaultHandle))).String()
			account := store.Account{ID: accountID, Label: names.DefaultHandle, CompanyID: uuid.NewString(), Enabled: true}
			credentials := store.Credentials{Handle: names.DefaultHandle, Passphrase: "rejected-mint-passphrase",
				APIKey: "main-key", UnderworldMintID: uuid.NewString()}
			if err := db.SaveAccount(ctx, account, credentials); err != nil {
				t.Fatal(err)
			}
			oldID := uuid.NewSHA1(uuid.NameSpaceURL, []byte(accountID+"/bootstrap/underworld-key/"+credentials.UnderworldMintID)).String()
			if test.journaled {
				body, err := json.Marshal(map[string]any{
					"handle": credentials.Handle, "passphrase": credentials.Passphrase, "company_id": account.CompanyID,
					"label": "Tradewinds underworld observer", "scopes": []string{"read", "underworld"},
				})
				if err != nil {
					t.Fatal(err)
				}
				if _, err := db.BeginAction(ctx, &store.Action{ID: oldID, AccountID: accountID,
					Method: http.MethodPost, Path: "/v1/auth/create-key", Body: body}); err != nil {
					t.Fatal(err)
				}
				if err := db.CompleteAction(ctx, oldID, test.status, json.RawMessage(`{"code":"forbidden"}`), nil); err != nil {
					t.Fatal(err)
				}
			}
			cfg := config.Config{APIURL: server.URL}
			var apiErr *gameapi.APIError
			if err := underworldKey(cfg, db, zap.NewNop(), nil); !errors.As(err, &apiErr) || apiErr.Status != test.status {
				t.Fatalf("unexpected mint rejection: %v", err)
			}
			stored, err := db.Credentials(ctx, accountID)
			if err != nil {
				t.Fatal(err)
			}
			wantMintID := credentials.UnderworldMintID
			if test.retire {
				wantMintID = ""
			}
			if stored.UnderworldMintID != wantMintID || stored.APIKey != credentials.APIKey || stored.UnderworldAPIKey != "" {
				t.Fatal("mint identity was incorrectly retired or retained")
			}
			if test.retire {
				if err := underworldKey(cfg, db, zap.NewNop(), nil); err != nil {
					t.Fatalf("definitely rejected mint could not be replaced: %v", err)
				}
				if len(ids) == 0 || ids[len(ids)-1] == oldID || ids[len(ids)-1] == "" {
					t.Fatal("replacement reused the definitely rejected identity")
				}
			}
		})
	}
}

func registrationStore(t *testing.T) (*store.Store, *sql.DB, string) {
	t.Helper()
	dsn := os.Getenv("TEST_DATABASE_URL")
	if dsn == "" {
		t.Skip("set TEST_DATABASE_URL to run isolated PostgreSQL registration coverage")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	admin, err := sql.Open("pgx", dsn)
	if err != nil {
		t.Fatal(err)
	}
	schema := "registration_test_" + strings.ReplaceAll(uuid.NewString(), "-", "")
	if _, err := admin.ExecContext(ctx, "CREATE SCHEMA "+schema); err != nil {
		_ = admin.Close()
		t.Fatal(err)
	}
	var db *store.Store
	t.Cleanup(func() {
		if db != nil {
			if err := db.Close(); err != nil {
				t.Error(err)
			}
		}
		cleanupCtx, cleanupCancel := context.WithTimeout(context.Background(), 15*time.Second)
		defer cleanupCancel()
		if _, err := admin.ExecContext(cleanupCtx, "DROP SCHEMA "+schema+" CASCADE"); err != nil {
			t.Error(err)
		}
		if err := admin.Close(); err != nil {
			t.Error(err)
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
	db, err = store.Open(ctx, dsn, bytes.Repeat([]byte{0x42}, 32), zap.NewNop())
	if err != nil {
		t.Fatal(err)
	}
	return db, admin, schema
}
