package main

import (
	"context"
	"encoding/json"
	"net"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/DevYukine/go-tradewinds/internal/config"
	"github.com/DevYukine/go-tradewinds/internal/store"
	"github.com/google/uuid"
	"go.uber.org/fx"
	"go.uber.org/zap"
	"go.uber.org/zap/zaptest/observer"
)

func TestRuntimeStartsHealthyAccountAndDashboardWithUnreadableCredentials(t *testing.T) {
	db, admin, schema := registrationStore(t)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	broken := store.Account{ID: "broken-account", Label: "Broken account", CompanyID: uuid.NewString(), Enabled: true}
	healthy := store.Account{ID: "healthy-account", Label: "Healthy account", CompanyID: uuid.NewString(), Enabled: true}
	const healthyKey = "synthetic-healthy-startup-key"
	for _, account := range []store.Account{broken, healthy} {
		if err := db.SaveAccount(ctx, account, store.Credentials{APIKey: healthyKey}); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := admin.ExecContext(ctx, "UPDATE "+schema+".account_secrets SET ciphertext = $1 WHERE account_id = $2", []byte("unreadable"), broken.ID); err != nil {
		t.Fatal(err)
	}

	playerID := uuid.NewString()
	game := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/update" {
			if r.Method != http.MethodGet || r.Header.Get("Authorization") != "" {
				t.Error("release watcher must use an unauthenticated GET")
				w.WriteHeader(http.StatusUnauthorized)
				return
			}
			w.Header().Set("Content-Type", "application/json")
			if err := json.NewEncoder(w).Encode([]map[string]string{{
				"version": "0.4.0", "released_on": "2026-10-26", "markdown": "# R&D",
			}}); err != nil {
				t.Error(err)
			}
			return
		}
		if r.Method != http.MethodGet || r.Header.Get("Authorization") != "Bearer "+healthyKey {
			t.Error("runtime sent an unexpected mutation or credential")
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		if r.URL.Path == "/v1/event/sse" {
			w.Header().Set("Content-Type", "text/event-stream")
			w.WriteHeader(http.StatusOK)
			w.(http.Flusher).Flush()
			<-r.Context().Done()
			return
		}
		w.Header().Set("Content-Type", "application/json")
		var document any = []any{}
		switch r.URL.Path {
		case "/v1/auth/whoami":
			document = map[string]any{"company_id": healthy.CompanyID, "player_id": playerID}
		case "/v1/player/profile":
			document = map[string]any{"player": map[string]any{"id": playerID}, "companies": []any{}}
		case "/v1/company/profile":
			document = map[string]any{"company": map[string]any{"id": healthy.CompanyID, "player_id": playerID}, "fleet_count": 0}
		case "/v1/company/settings":
			document = map[string]any{"auto_remit_for_bills": true, "auto_remit_min_reserve_pence": 200_000}
		case "/v1/world/catalog":
			document = map[string]any{"ports": []any{}, "ship_classes": []any{}, "sea_zones": []any{}, "commodities": []any{}}
		case "/v1/world/clock":
			document = map[string]any{"paused": false, "calendar": map[string]any{"year": 1700, "month": 5, "day": 10, "time_scale": 1}}
		case "/v1/world/expansion", "/v1/world/leaderboard":
			document = map[string]any{}
		case "/v1/treasury/vaults":
			document = map[string]any{"company_id": healthy.CompanyID, "vaults": []any{}}
		case "/v1/rnd/status":
			document = map[string]any{
				"can_start": true, "player_net_worth_pence": 100_000_000,
				"entry_net_worth_pence": 50_000_000, "start_fee_pence": 1_500_000,
				"upkeep_per_game_day_pence": 30_000,
			}
		}
		if err := json.NewEncoder(w).Encode(document); err != nil {
			t.Error(err)
		}
	}))
	t.Cleanup(game.Close)

	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	address := listener.Addr().String()
	if err := listener.Close(); err != nil {
		t.Fatal(err)
	}
	core, logs := observer.New(zap.DebugLevel)
	log := zap.New(core)
	cfg := config.Config{APIURL: game.URL, HTTPAddr: address}
	app := fx.New(
		fx.NopLogger,
		fx.Invoke(func(lifecycle fx.Lifecycle, shutdown fx.Shutdowner) {
			startRuntime(lifecycle, shutdown, cfg, db, log, false)
		}),
	)
	if err := app.Start(ctx); err != nil {
		t.Fatalf("degraded runtime startup failed: %v", err)
	}
	t.Cleanup(func() {
		stopCtx, stopCancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer stopCancel()
		if err := app.Stop(stopCtx); err != nil {
			t.Errorf("stop runtime: %v", err)
		}
	})

	client := &http.Client{Timeout: 5 * time.Second}
	for {
		request, err := http.NewRequestWithContext(ctx, http.MethodGet, "http://"+address+"/api/dashboard", nil)
		if err != nil {
			t.Fatal(err)
		}
		response, err := client.Do(request)
		if err != nil {
			t.Fatalf("dashboard unavailable after degraded startup: %v", err)
		}
		var dashboard store.Dashboard
		err = json.NewDecoder(response.Body).Decode(&dashboard)
		_ = response.Body.Close()
		if err != nil || response.StatusCode != http.StatusOK {
			t.Fatalf("dashboard response status %d, decode error %v", response.StatusCode, err)
		}
		if len(dashboard.Accounts) != 2 {
			t.Fatalf("dashboard accounts = %d, want both configured accounts", len(dashboard.Accounts))
		}
		healthyStarted := false
		for _, account := range dashboard.Accounts {
			if account.ID == broken.ID && len(account.Snapshot) != 0 {
				t.Fatal("unreadable account unexpectedly ran and persisted a snapshot")
			}
			if account.ID == healthy.ID {
				var state struct {
					Mode       string `json:"mode"`
					Generation string `json:"generation"`
				}
				if raw := account.Snapshot["_engine"]; len(raw) != 0 {
					if err := json.Unmarshal(raw, &state); err != nil {
						t.Fatal(err)
					}
					healthyStarted = state.Mode == "observe" && state.Generation != ""
				}
			}
		}
		if healthyStarted {
			break
		}
		select {
		case <-ctx.Done():
			t.Fatal("healthy runner never completed its initial observation")
		case <-time.After(10 * time.Millisecond):
		}
	}
	skipped := logs.FilterMessage("skipping account with unreadable credentials").All()
	if len(skipped) != 1 || skipped[0].Level != zap.ErrorLevel ||
		skipped[0].ContextMap()["account_id"] != broken.ID || skipped[0].ContextMap()["error"] == nil {
		t.Fatalf("missing error log identifying the skipped account: %+v", skipped)
	}
	if stopped := logs.FilterMessage("account runner stopped").Len(); stopped != 0 {
		t.Fatalf("healthy runner stopped unexpectedly: %d errors", stopped)
	}
}

func TestRuntimeWatchesReleasesWithoutAccounts(t *testing.T) {
	db, _, _ := registrationStore(t)
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	requested := make(chan struct{}, 1)
	game := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet || r.URL.Path != "/update" || r.Header.Get("Authorization") != "" {
			t.Errorf("unexpected public watcher request: %s %s", r.Method, r.URL.Path)
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		if err := json.NewEncoder(w).Encode([]map[string]string{{
			"version": "0.4.0", "released_on": "2026-10-26", "markdown": "# R&D",
		}}); err != nil {
			t.Error(err)
		}
		requested <- struct{}{}
	}))
	defer game.Close()
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	address := listener.Addr().String()
	if err := listener.Close(); err != nil {
		t.Fatal(err)
	}
	core, logs := observer.New(zap.InfoLevel)
	app := fx.New(
		fx.NopLogger,
		fx.Invoke(func(lifecycle fx.Lifecycle, shutdown fx.Shutdowner) {
			startRuntime(lifecycle, shutdown, config.Config{APIURL: game.URL, HTTPAddr: address}, db, zap.New(core), false)
		}),
	)
	if err := app.Start(ctx); err != nil {
		t.Fatal(err)
	}
	defer func() {
		stopCtx, stopCancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer stopCancel()
		if err := app.Stop(stopCtx); err != nil {
			t.Errorf("stop account-free runtime: %v", err)
		}
	}()
	select {
	case <-requested:
	case <-ctx.Done():
		t.Fatal("public releases were not fetched without accounts")
	}
	for logs.FilterMessage("game release notes").Len() == 0 {
		select {
		case <-ctx.Done():
			t.Fatal("account-free watcher did not log the startup version")
		case <-time.After(time.Millisecond):
		}
	}
	startup := logs.FilterMessage("game release notes").All()
	if len(startup) != 1 || startup[0].Level != zap.InfoLevel || startup[0].ContextMap()["version"] != "0.4.0" {
		t.Fatalf("account-free startup release log: %+v", startup)
	}
}
