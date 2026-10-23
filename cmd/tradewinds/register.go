package main

import (
	"context"
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"net/http"
	"slices"
	"strings"
	"time"

	"github.com/DevYukine/go-tradewinds/internal/config"
	"github.com/DevYukine/go-tradewinds/internal/gameapi"
	"github.com/DevYukine/go-tradewinds/internal/names"
	"github.com/DevYukine/go-tradewinds/internal/store"
	"github.com/google/uuid"
	"go.uber.org/zap"
)

var automationScopes = [...]string{
	"read",
	"fleet_navigate",
	"fleet_cargo",
	"market_spot",
	"market_derivatives",
	"infra_manage",
	"treasury_remit",
	"account_manage",
}

type bootstrapCompany struct {
	ID         string `json:"id"`
	Name       string `json:"name"`
	HomePortID string `json:"home_port_id"`
}

func register(cfg config.Config, db *store.Store, log *zap.Logger, args []string) error {
	flags := flag.NewFlagSet("register", flag.ContinueOnError)
	handle := flags.String("handle", names.DefaultHandle, "Exact player handle")
	companyName := flags.String("company", names.DefaultCompany, "Exact company name")
	home := flags.String("home", "le_havre", "Canonical Phase 1 home port ID")
	if err := flags.Parse(args); err != nil {
		return err
	}
	if flags.NArg() != 0 || strings.TrimSpace(*handle) == "" || strings.TrimSpace(*companyName) == "" {
		return errors.New("register requires nonempty names and no positional arguments")
	}

	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
	defer cancel()
	accountID := uuid.NewSHA1(uuid.NameSpaceURL, []byte(cfg.APIURL+"/player/"+strings.ToLower(*handle))).String()
	ctx, release, err := db.AcquireAccountContext(ctx, accountID)
	if err != nil {
		return err
	}
	defer release()

	account, credentials, err := prepareAccount(ctx, db, accountID, *handle)
	if err != nil {
		return err
	}
	client := gameapi.New(cfg.APIURL, credentials.APIKey, log)
	observeBootstrap(client, db, account.ID, log)
	if credentials.APIKey == "" {
		request := map[string]string{"handle": credentials.Handle, "passphrase": credentials.Passphrase}
		err := bootstrapAction(ctx, db, client, account.ID, "register", "/v1/player/register", request, nil)
		var apiErr *gameapi.APIError
		if err != nil && (!errors.As(err, &apiErr) || apiErr.Status != http.StatusConflict) {
			return fmt.Errorf("register player: %w", err)
		}
		// A handle conflict is recoverable only with our previously persisted password.
		key, err := createBootstrapKey(ctx, db, client, account.ID, credentials, "")
		if err != nil {
			return fmt.Errorf("authenticate exact handle %q; no alternate account was created: %w", *handle, err)
		}
		credentials.APIKey = key
		if err := db.SaveAccount(ctx, account, credentials); err != nil {
			return err
		}
		client = gameapi.New(cfg.APIURL, key, log)
		observeBootstrap(client, db, account.ID, log)
	}

	var profile struct {
		Companies []bootstrapCompany `json:"companies"`
	}
	if err := client.Get(ctx, "/v1/player/profile", &profile); err != nil {
		return fmt.Errorf("load registered player: %w", err)
	}
	var company bootstrapCompany
	for _, owned := range profile.Companies {
		if owned.Name == *companyName {
			company = owned
			break
		}
	}
	if company.ID == "" {
		if len(profile.Companies) != 0 {
			return errors.New("player already owns a differently named company; refusing to create an additional company")
		}
		var result struct {
			Company bootstrapCompany `json:"company"`
		}
		request := map[string]string{
			"name":              *companyName,
			"home_port_id":      *home,
			"starter_ship_name": names.Next(nil),
		}
		if err := bootstrapAction(ctx, db, client, account.ID, "company", "/v1/company/create", request, &result); err != nil {
			return fmt.Errorf("create main company: %w", err)
		}
		company = result.Company
	}
	if company.ID == "" || company.HomePortID != *home {
		return errors.New("existing company does not match the requested home port; no replacement was created")
	}
	if account.CompanyID != "" && account.CompanyID != company.ID {
		return errors.New("stored account is bound to a different company")
	}

	needsCompanyKey := account.CompanyID == ""
	if !needsCompanyKey {
		var identity struct {
			CompanyID string   `json:"company_id"`
			Scopes    []string `json:"scopes"`
		}
		if err := client.Get(ctx, "/v1/auth/whoami", &identity); err != nil {
			return fmt.Errorf("inspect company key permissions: %w", err)
		}
		needsCompanyKey = identity.CompanyID != company.ID
		for _, required := range automationScopes {
			if !slices.Contains(identity.Scopes, required) {
				needsCompanyKey = true
			}
		}
	}
	if needsCompanyKey {
		key, err := createBootstrapKey(ctx, db, client, account.ID, credentials, company.ID)
		if err != nil {
			return fmt.Errorf("create company-scoped key: %w", err)
		}
		credentials.APIKey = key
		account.CompanyID = company.ID
		account.Enabled = true
		if err := db.SaveAccount(ctx, account, credentials); err != nil {
			return err
		}
	}
	log.Info("account ready", zap.String("handle", account.Label), zap.String("company", company.Name), zap.String("company_id", company.ID))
	return nil
}

func prepareAccount(ctx context.Context, db *store.Store, id, handle string) (store.Account, store.Credentials, error) {
	accounts, err := db.Accounts(ctx)
	if err != nil {
		return store.Account{}, store.Credentials{}, err
	}
	for _, account := range accounts {
		if account.ID == id {
			credentials, err := db.Credentials(ctx, id)
			return account, credentials, err
		}
	}
	secret := make([]byte, 32)
	if _, err := rand.Read(secret); err != nil {
		return store.Account{}, store.Credentials{}, err
	}
	account := store.Account{ID: id, Label: handle, Enabled: false}
	credentials := store.Credentials{Handle: handle, Passphrase: base64.RawURLEncoding.EncodeToString(secret)}
	if err := db.SaveAccount(ctx, account, credentials); err != nil {
		return account, credentials, err
	}
	return account, credentials, nil
}

func createBootstrapKey(ctx context.Context, db *store.Store, client *gameapi.Client, accountID string, credentials store.Credentials, companyID string) (string, error) {
	request := map[string]any{
		"handle":     credentials.Handle,
		"passphrase": credentials.Passphrase,
		"label":      "Tradewinds fleet automation",
		"scopes":     automationScopes[:],
	}
	if companyID != "" {
		request["company_id"] = companyID
	}
	var result struct {
		APIKey string `json:"api_key"`
	}
	// Changed permissions are a new logical request, not a replay of the old key.
	step := "key/" + companyID + "/" + strings.Join(automationScopes[:], ",")
	err := bootstrapAction(ctx, db, client, accountID, step, "/v1/auth/create-key", request, &result)
	if err != nil {
		return "", err
	}
	if result.APIKey == "" {
		return "", errors.New("key creation returned no credential")
	}
	return result.APIKey, nil
}

func observeBootstrap(client *gameapi.Client, db *store.Store, accountID string, log *zap.Logger) {
	client.SetObserver(func(method, path string, status int, duration time.Duration, request, response []byte, errText string) {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		if err := db.RecordRequest(ctx, accountID, method, path, status, duration, request, response, errText); err != nil {
			log.Warn("persist bootstrap diagnostic", zap.String("account_id", accountID), zap.Error(err))
		}
	})
}

func bootstrapAction(ctx context.Context, db *store.Store, client *gameapi.Client, accountID, step, path string, body any, out any) error {
	encoded, err := json.Marshal(body)
	if err != nil {
		return err
	}
	id := uuid.NewSHA1(uuid.NameSpaceURL, []byte(accountID+"/bootstrap/"+step)).String()
	var action *store.Action
	for {
		action, err = db.BeginAction(ctx, &store.Action{
			ID:        id,
			AccountID: accountID,
			Method:    http.MethodPost,
			Path:      path,
			Body:      encoded,
		})
		if err != nil {
			return err
		}
		if action.Status != http.StatusTooManyRequests {
			break
		}
		if action.RetryAfter == nil || action.RetryAfter.IsZero() {
			return errors.New("stored bootstrap HTTP 429 lacks a durable retry deadline; reconcile before retrying")
		}
		if time.Now().Before(*action.RetryAfter) {
			return &gameapi.APIError{
				Status:     http.StatusTooManyRequests,
				Detail:     "bootstrap retry cooldown has not elapsed",
				Body:       action.Response,
				RetryAfter: *action.RetryAfter,
			}
		}
		// The rejected request needs a new identity only after its persisted
		// cooldown. Its original journal and immutable payload remain intact.
		id = uuid.NewSHA1(uuid.NameSpaceURL, []byte(action.ID+"/retry")).String()
	}
	if action.Status >= 200 && action.Status < 300 {
		if out == nil {
			return nil
		}
		return json.Unmarshal(action.Response, out)
	}
	if action.Status >= 400 {
		return &gameapi.APIError{Status: action.Status, Detail: "previous bootstrap attempt was rejected", Body: action.Response}
	}
	if time.Since(action.CreatedAt) >= store.ActionReplayWindow {
		return errors.New("unresolved bootstrap action is near the server's 24-hour idempotency expiry; reconcile it before retrying")
	}
	attemptCtx, cancel := context.WithDeadline(ctx, action.CreatedAt.Add(store.ActionReplayWindow))
	defer cancel()
	var response json.RawMessage
	if err := client.Do(attemptCtx, action.Method, action.Path, action.Body, action.ID, &response); err != nil {
		var apiErr *gameapi.APIError
		if errors.As(err, &apiErr) {
			committed := apiErr.Status >= 200 && apiErr.Status < 300
			rejected := apiErr.Status >= 400 && apiErr.Status < 500 &&
				apiErr.Status != 408 && apiErr.Status != 409 && apiErr.Status != 425
			if committed || rejected {
				var retryAfter *time.Time
				if apiErr.Status == http.StatusTooManyRequests {
					retryAfter = &apiErr.RetryAfter
				}
				if saveErr := db.CompleteAction(ctx, action.ID, apiErr.Status, apiErr.Body, retryAfter); saveErr != nil {
					return errors.Join(err, saveErr)
				}
			}
		}
		return err
	}
	if err := db.CompleteAction(ctx, action.ID, http.StatusCreated, response, nil); err != nil {
		return err
	}
	if out == nil {
		return nil
	}
	return json.Unmarshal(response, out)
}
