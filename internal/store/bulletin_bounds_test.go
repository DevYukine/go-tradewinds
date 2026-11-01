package store

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
)

func TestBulletinThreadBoundsEncodedBytesWithEscaping(t *testing.T) {
	db, _, _ := integrationStore(t)
	ctx := context.Background()
	const owner = "bulletin-byte-budget"
	saveTestAccount(t, db, owner)
	thread := uuid.NewString()
	base := time.Now().UTC().Truncate(time.Microsecond)
	posts := make([]BulletinPost, 300)
	for i := range posts {
		posts[i] = BulletinPost{
			ID: uuid.NewString(), ThreadID: thread, CompanyName: "Harbour & Co",
			Body: strings.Repeat("<>&\u2028\u2029\t\"\\", 1000), CreatedAt: base.Add(time.Duration(i) * time.Second),
		}
	}
	if err := db.SaveBulletinThreadDetail(ctx, owner, BulletinThread{
		ID: thread, Title: "Escaped replies", CreatedAt: base, LastPostAt: posts[len(posts)-1].CreatedAt, Replies: len(posts) - 1,
	}, posts); err != nil {
		t.Fatal(err)
	}
	view, err := db.BulletinThread(ctx, thread)
	if err != nil {
		t.Fatal(err)
	}
	encoded, err := json.Marshal(view)
	if err != nil {
		t.Fatal(err)
	}
	if len(encoded) > MaxBulletinThreadBytes || !view.PostsTruncated || view.PostsTotal != len(posts) || len(view.Posts) == 0 || len(view.Posts) >= len(posts) {
		t.Fatalf("encoded response bound/count mismatch: bytes=%d returned=%d total=%d truncated=%t", len(encoded), len(view.Posts), view.PostsTotal, view.PostsTruncated)
	}
	start := len(posts) - len(view.Posts)
	for i, post := range view.Posts {
		if post.ID != posts[start+i].ID {
			t.Fatalf("newest window lost chronological order at %d", i)
		}
	}
	older := posts[start-1]
	view.Posts = append([]BulletinPostView{{ID: older.ID, CompanyName: older.CompanyName, Body: older.Body, CreatedAt: older.CreatedAt}}, view.Posts...)
	encoded, err = json.Marshal(view)
	if err != nil {
		t.Fatal(err)
	}
	if len(encoded) <= MaxBulletinThreadBytes {
		t.Fatal("reader omitted a post that fits the encoded byte budget")
	}
}

func TestBulletinEventPersistenceSanitizesDecodedStringsAndKeys(t *testing.T) {
	db, _, _ := integrationStore(t)
	ctx := context.Background()
	const account = "bulletin-safe-event"
	saveTestAccount(t, db, account)
	payload := json.RawMessage(`{"payload":{"body":"ordinary\u0000 text \ud800","bad\u0000key":"retained","bad�key":"also retained","nested":[{"\udfff":"safe"}]},"count":9007199254740993}`)
	if err := db.RecordEvent(ctx, account, "poisoned", "bulletin_posted", payload); err != nil {
		t.Fatal(err)
	}
	var event Event
	if err := db.db.Where("account_id = ? AND event_id = ?", account, "poisoned").Take(&event).Error; err != nil {
		t.Fatal(err)
	}
	var data struct {
		Payload map[string]json.RawMessage `json:"payload"`
		Count   json.Number                `json:"count"`
	}
	if err := json.Unmarshal(event.Payload, &data); err != nil {
		t.Fatal(err)
	}
	var body string
	if err := json.Unmarshal(data.Payload["body"], &body); err != nil || body != "ordinary� text �" || data.Count != "9007199254740993" {
		t.Fatalf("sanitization lost ordinary content or numeric precision: %s, %v", event.Payload, err)
	}
	if len(data.Payload) != 4 || string(data.Payload["bad�key"]) != `"also retained"` || string(data.Payload["bad�key_2"]) != `"retained"` {
		t.Fatalf("sanitized keys collided: %s", event.Payload)
	}
	if err := db.RecordEvent(ctx, account, "after-poison", "notice", json.RawMessage(`{"body":"still processing"}`)); err != nil {
		t.Fatal(err)
	}
	if cursor, err := db.Cursor(ctx, account); err != nil || cursor != "after-poison" {
		t.Fatalf("poisoned event wedged the cursor: %q, %v", cursor, err)
	}
}
