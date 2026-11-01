package httpapi

// Acceptance contracts for the dashboard bulletin endpoints (plan G2) against
// the real store in a disposable PostgreSQL schema: the board lists threads
// newest activity first with working search and company filters, the thread
// endpoint returns every recorded post oldest first, and bad or unknown ids
// map to the shared error codes. No test talks to the game. They skip when
// TEST_DATABASE_URL is absent.

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/DevYukine/go-tradewinds/internal/store"
	"github.com/google/uuid"
	"go.uber.org/zap"
)

func bulletinGet(t *testing.T, handler http.Handler, path string) *httptest.ResponseRecorder {
	t.Helper()
	request := httptest.NewRequest("GET", path, nil)
	request.Host = "127.0.0.1:8585"
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, request)
	return response
}

func TestBulletinBoardEndpointListsThreadsNewestFirstAndFilters(t *testing.T) {
	database, _ := warningsAcceptanceHTTPStore(t)
	ctx := context.Background()
	base := time.Now().UTC().Truncate(time.Microsecond)
	fresh := uuid.NewString()
	grain := uuid.NewString()
	ancient := uuid.NewString()
	if err := database.SaveBulletinThreadDetail(ctx, "one", store.BulletinThread{
		ID: fresh, Title: "Fresh arrivals", CompanyName: "Harbour Union",
		CreatedAt: base.Add(-time.Hour), LastPostAt: base.Add(-30 * time.Minute), Replies: 1,
	}, []store.BulletinPost{
		{ID: uuid.NewString(), ThreadID: fresh, CompanyName: "Harbour Union", Body: "Fresh arrivals at dawn", IsOpening: true, CreatedAt: base.Add(-time.Hour)},
		{ID: uuid.NewString(), ThreadID: fresh, CompanyName: "Replier Co", Body: "Seeing the same prices here", IsOpening: false, CreatedAt: base.Add(-30 * time.Minute)},
	}); err != nil {
		t.Fatal(err)
	}
	if err := database.SaveBulletinThreads(ctx, "one", []store.BulletinThread{
		{ID: grain, Title: "Grain talk", CompanyName: "The Kraken Cartel", CreatedAt: base.Add(-3 * time.Hour), LastPostAt: base.Add(-2 * time.Hour), Replies: 2},
		// Quiet for more than three days while stored unarchived: the reader
		// must report it archived.
		{ID: ancient, Title: "Old colours", CompanyName: "Salt Oval", CreatedAt: base.Add(-80 * time.Hour), LastPostAt: base.Add(-73 * time.Hour)},
	}); err != nil {
		t.Fatal(err)
	}
	handler := NewHandler(database, zap.NewNop(), "127.0.0.1:8585")
	response := bulletinGet(t, handler, "/api/bulletin")
	if response.Code != 200 {
		t.Fatalf("board route status %d body %s", response.Code, response.Body.String())
	}
	if response.Header().Get("Cache-Control") != "no-store" {
		t.Fatal("board responses permit caching")
	}
	var board struct {
		Threads []struct {
			ID             string     `json:"id"`
			Title          string     `json:"title"`
			CompanyName    string     `json:"company_name"`
			LastPostAt     time.Time  `json:"last_post_at"`
			Replies        int        `json:"replies"`
			PostsRecorded  int        `json:"posts_recorded"`
			Archived       bool       `json:"archived"`
			DetailCurrent  bool       `json:"detail_current"`
			DetailSyncedAt *time.Time `json:"detail_synced_at"`
		} `json:"threads"`
		Total              int        `json:"total"`
		Companies          []string   `json:"companies"`
		CompaniesTruncated bool       `json:"companies_truncated"`
		SyncedAt           *time.Time `json:"synced_at"`
	}
	if err := json.Unmarshal(response.Body.Bytes(), &board); err != nil {
		t.Fatalf("board wire contract: %v", err)
	}
	if board.CompaniesTruncated {
		t.Fatal("a small company list reported truncation")
	}
	if len(board.Threads) != 3 || board.Total != 3 {
		t.Fatalf("board lost threads: total %d threads %d", board.Total, len(board.Threads))
	}
	if board.Threads[0].ID != fresh || board.Threads[1].ID != grain || board.Threads[2].ID != ancient {
		t.Fatalf("board threads were not newest activity first: %+v", board.Threads)
	}
	if board.Threads[0].PostsRecorded != 2 || !board.Threads[0].DetailCurrent || board.Threads[0].DetailSyncedAt == nil {
		t.Fatalf("the detailed thread lost its summary facts: %+v", board.Threads[0])
	}
	if board.Threads[1].DetailCurrent {
		t.Fatalf("a never detailed thread reported a current detail: %+v", board.Threads[1])
	}
	if !board.Threads[2].Archived {
		t.Fatalf("a thread quiet for three days was not reported archived: %+v", board.Threads[2])
	}
	wantCompanies := []string{"Harbour Union", "Replier Co", "Salt Oval", "The Kraken Cartel"}
	if len(board.Companies) != len(wantCompanies) {
		t.Fatalf("board companies got %v, want %v", board.Companies, wantCompanies)
	}
	for index, name := range wantCompanies {
		if board.Companies[index] != name {
			t.Fatalf("board companies got %v, want sorted unique %v", board.Companies, wantCompanies)
		}
	}
	response = bulletinGet(t, handler, "/api/bulletin?q=seeing%20the%20SAME%20prices")
	var filtered struct {
		Threads []struct {
			ID string `json:"id"`
		} `json:"threads"`
		Total int `json:"total"`
	}
	if err := json.Unmarshal(response.Body.Bytes(), &filtered); err != nil {
		t.Fatalf("search wire contract: %v", err)
	}
	if filtered.Total != 1 || len(filtered.Threads) != 1 || filtered.Threads[0].ID != fresh {
		t.Fatalf("case insensitive body search matched %d threads: %s", filtered.Total, response.Body.String())
	}
	response = bulletinGet(t, handler, "/api/bulletin?company=Replier%20Co")
	if err := json.Unmarshal(response.Body.Bytes(), &filtered); err != nil {
		t.Fatalf("company filter wire contract: %v", err)
	}
	if filtered.Total != 1 || len(filtered.Threads) != 1 || filtered.Threads[0].ID != fresh {
		t.Fatalf("a replier company filter matched %d threads: %s", filtered.Total, response.Body.String())
	}
	response = bulletinGet(t, handler, "/api/bulletin?limit=1")
	if err := json.Unmarshal(response.Body.Bytes(), &filtered); err != nil {
		t.Fatalf("limited wire contract: %v", err)
	}
	if filtered.Total != 3 || len(filtered.Threads) != 1 {
		t.Fatalf("the limit did not keep the total count before it: %s", response.Body.String())
	}
}

func TestBulletinThreadEndpointReturnsPostsOldestFirst(t *testing.T) {
	database, _ := warningsAcceptanceHTTPStore(t)
	ctx := context.Background()
	base := time.Now().UTC().Truncate(time.Microsecond)
	thread := uuid.NewString()
	opening := uuid.NewString()
	removed := uuid.NewString()
	reply := uuid.NewString()
	edited := base.Add(-2 * time.Hour)
	if err := database.SaveBulletinThreadDetail(ctx, "one", store.BulletinThread{
		ID: thread, Title: "Charter offers", CompanyName: "Salt Oval",
		CreatedAt: base.Add(-2 * time.Hour), LastPostAt: base.Add(-30 * time.Minute), Replies: 2,
	}, []store.BulletinPost{
		// Stored newest first on purpose; the endpoint must reorder.
		{ID: reply, ThreadID: thread, CompanyName: "Replier Co", Body: "Taking the barley slot.", IsOpening: false, CreatedAt: base.Add(-30 * time.Minute)},
		{ID: removed, ThreadID: thread, CompanyName: "Replier Co", Body: "Withdrawn offer.", IsOpening: false, CreatedAt: base.Add(-time.Hour), Removed: true, Deleted: true},
		{ID: opening, ThreadID: thread, CompanyName: "Salt Oval", Body: "Offering charter space.", IsOpening: true, CreatedAt: base.Add(-2 * time.Hour), EditedAt: &edited},
	}); err != nil {
		t.Fatal(err)
	}
	handler := NewHandler(database, zap.NewNop(), "127.0.0.1:8585")
	response := bulletinGet(t, handler, "/api/bulletin/thread?id="+thread)
	if response.Code != 200 {
		t.Fatalf("thread route status %d body %s", response.Code, response.Body.String())
	}
	var view struct {
		Thread struct {
			ID    string `json:"id"`
			Title string `json:"title"`
		} `json:"thread"`
		Posts []struct {
			ID        string     `json:"id"`
			Body      string     `json:"body"`
			IsOpening bool       `json:"is_opening"`
			EditedAt  *time.Time `json:"edited_at"`
			Removed   bool       `json:"removed"`
			Deleted   bool       `json:"deleted"`
		} `json:"posts"`
		PostsTruncated bool `json:"posts_truncated"`
		PostsTotal     int  `json:"posts_total"`
	}
	if err := json.Unmarshal(response.Body.Bytes(), &view); err != nil {
		t.Fatalf("thread wire contract: %v", err)
	}
	if view.PostsTruncated || view.PostsTotal != 3 {
		t.Fatalf("a small thread reported a truncated post window: truncated %t total %d", view.PostsTruncated, view.PostsTotal)
	}
	if view.Thread.ID != thread || view.Thread.Title != "Charter offers" {
		t.Fatalf("thread summary lost its identity: %+v", view.Thread)
	}
	if len(view.Posts) != 3 || view.Posts[0].ID != opening || view.Posts[1].ID != removed || view.Posts[2].ID != reply {
		t.Fatalf("thread posts were not oldest first: %+v", view.Posts)
	}
	if !view.Posts[0].IsOpening || view.Posts[0].EditedAt == nil || !view.Posts[0].EditedAt.Equal(edited) {
		t.Fatalf("the opening post lost its edit state: %+v", view.Posts[0])
	}
	if !view.Posts[1].Removed || !view.Posts[1].Deleted {
		t.Fatalf("the withdrawn post lost its states: %+v", view.Posts[1])
	}
}

func TestBulletinThreadEndpointRejectsInvalidAndUnknownIDs(t *testing.T) {
	database, _ := warningsAcceptanceHTTPStore(t)
	handler := NewHandler(database, zap.NewNop(), "127.0.0.1:8585")
	for _, test := range []struct {
		name   string
		path   string
		status int
		code   string
	}{
		{"malformed id", "/api/bulletin/thread?id=not-a-uuid", 400, "invalid_query"},
		{"missing id", "/api/bulletin/thread", 400, "invalid_query"},
		{"unknown thread", "/api/bulletin/thread?id=" + uuid.NewString(), 404, "not_found"},
	} {
		t.Run(test.name, func(t *testing.T) {
			response := bulletinGet(t, handler, test.path)
			var body struct {
				Error string `json:"error"`
			}
			if err := json.Unmarshal(response.Body.Bytes(), &body); err != nil {
				t.Fatalf("non JSON response: %v", err)
			}
			if response.Code != test.status || body.Error != test.code {
				t.Fatalf("status %d code %s, want %d %s", response.Code, body.Error, test.status, test.code)
			}
		})
	}
}
