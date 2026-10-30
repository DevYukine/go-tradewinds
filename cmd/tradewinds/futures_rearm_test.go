package main

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"testing"

	"github.com/DevYukine/go-tradewinds/internal/store"
	"go.uber.org/zap"
)

func TestFuturesRearmCommandIsExplicitLocalAndPreservesJournalSequence(t *testing.T) {
	db, _, schema := registrationStore(t)
	ctx := context.Background()
	account := store.Account{ID: "futures-rearm-owner", CompanyID: "futures-rearm-company", Label: "Operator", Enabled: true}
	if err := db.SaveAccount(ctx, account, store.Credentials{Handle: "Operator"}); err != nil {
		t.Fatal(err)
	}
	mission := &store.Mission{AccountID: account.ID, ShipID: "__futures", Generation: "amsterdam-water-beer-owner-pilot", Phase: "futures_done",
		Data: json.RawMessage(`{"futures":{"setup_attempted":true,"lease_owned":true},"sequence":3,"last_action_id":"closed-lease"}`)}
	if err := db.PutMission(ctx, mission); err != nil {
		t.Fatal(err)
	}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, request *http.Request) {
		t.Errorf("local futures re-arm contacted the game: %s %s", request.Method, request.URL.Path)
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer server.Close()
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
	for _, args := range [][]string{nil, {"unexpected"}, {"--account", account.ID, "unexpected"}} {
		if err := futuresRearm(db, zap.NewNop(), args); err == nil {
			t.Fatalf("operator command accepted ambiguous arguments: %v", args)
		}
	}
	if err := command([]string{"futures-rearm", "--account", account.ID}); err != nil {
		t.Fatalf("documented operator command failed: %v", err)
	}
	saved, err := db.GetMission(ctx, account.ID, "__futures")
	if err != nil {
		t.Fatal(err)
	}
	var data struct {
		Sequence     int64  `json:"sequence"`
		LastActionID string `json:"last_action_id"`
		Futures      struct {
			SetupAttempted bool `json:"setup_attempted"`
			LeaseOwned     bool `json:"lease_owned"`
		} `json:"futures"`
	}
	if err := json.Unmarshal(saved.Data, &data); err != nil || saved.Phase != "futures_pilot" || saved.Generation != mission.Generation ||
		data.Sequence != 3 || data.LastActionID != "closed-lease" || data.Futures.SetupAttempted || data.Futures.LeaseOwned {
		t.Fatalf("operator command reset identity or retained completed setup: %+v %+v %v", saved, data, err)
	}
	if err := command([]string{"futures-rearm", "--account", account.ID}); !errors.Is(err, store.ErrActionConflict) {
		t.Fatalf("re-running the command automatically re-armed an active pilot: %v", err)
	}
}
