package store

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/DevYukine/go-tradewinds/internal/gameapi"
	"go.uber.org/zap"
)

// The HTTP observer must hand RecordRequest payloads whose distinct members
// are still distinct. A response can publicly contain a member literally named
// "[REDACTED]" next to members whose names merely look like credentials. If
// the observer renames every credential-shaped name onto the same "[REDACTED]"
// key, the public member's value is overwritten and colliding members collapse
// into one before the store's collision-safe sanitizer ever sees them. With
// credentials absent, no value-based scrubbing may hide or repair this loss.
func TestObserverRecordRequestPreservesCollidingRedactedMembers(t *testing.T) {
	db, _, _ := integrationStore(t)
	ctx := context.Background()

	// Credentials absent: the account stores a handle only, so redaction of
	// this payload can rely on key names alone.
	accountID := "observer-collision"
	if err := db.SaveAccount(ctx, Account{ID: accountID, Label: "Observer collision"}, Credentials{Handle: "observer-collision"}); err != nil {
		t.Fatal(err)
	}

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{
			"[REDACTED]": "public diagnostic value",
			"tw_live_collision_one": "first public value",
			"tw_live_collision_two": "second public value",
			"nested": {
				"[REDACTED]": "nested public value",
				"tw_live_collision_three": "third public value"
			}
		}`))
	}))
	defer server.Close()

	client := gameapi.New(server.URL, "", zap.NewNop())
	var recordErr error
	client.SetObserver(func(method, path string, status int, duration time.Duration, request, response []byte, errText string) {
		recordErr = db.RecordRequest(ctx, accountID, method, path, status, duration, request, response, errText)
	})
	path := "/v1/player/profile?tw_live_collision_query_one=first-query-value&tw_live_collision_query_two=second-query-value&detail=plain"
	var out json.RawMessage
	if err := client.Get(ctx, path, &out); err != nil {
		t.Fatalf("observed profile request failed: %v", err)
	}
	if recordErr != nil {
		t.Fatalf("observer could not record the request: %v", recordErr)
	}

	history, err := db.History(ctx, accountID, "requests", 10)
	if err != nil {
		t.Fatal(err)
	}
	rows := history.([]Request)
	if len(rows) != 1 || rows[0].Status != http.StatusOK {
		t.Fatalf("observed request was not recorded once: %+v", rows)
	}

	storedResponse := string(rows[0].Response)
	for _, kept := range []string{
		"public diagnostic value",
		"first public value",
		"second public value",
		"nested public value",
		"third public value",
	} {
		if !strings.Contains(storedResponse, kept) {
			t.Fatalf("stored response diagnostic lost a distinct public member value %q: %s", kept, storedResponse)
		}
	}
	if apiKeyPattern.MatchString(storedResponse) {
		t.Fatalf("a credential-shaped member name survived verbatim in the stored response: %s", storedResponse)
	}

	storedPath := rows[0].Path
	for _, kept := range []string{"first-query-value", "second-query-value", "plain"} {
		if !strings.Contains(storedPath, kept) {
			t.Fatalf("stored path lost a distinct public query value %q: %s", kept, storedPath)
		}
	}
	if apiKeyPattern.MatchString(storedPath) {
		t.Fatalf("a credential-shaped query name survived verbatim in the stored path: %s", storedPath)
	}
}
