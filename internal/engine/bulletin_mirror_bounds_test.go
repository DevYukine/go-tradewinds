package engine

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"testing"
	"time"

	"github.com/DevYukine/go-tradewinds/internal/store"
	"github.com/google/uuid"
)

func TestBulletinMirrorResetsOnlyRejectedCursorStatuses(t *testing.T) {
	for _, status := range []int{400, 404, 410, 422, 408, 429, 500} {
		t.Run(fmt.Sprint(status), func(t *testing.T) {
			h, _, runner := newBulletinMirrorFixture(t)
			if _, err := h.db.ClaimBulletinList(h.ctx, h.account.ID); err != nil {
				t.Fatal(err)
			}
			if err := h.db.SaveBulletinHeadProgress(h.ctx, h.account.ID, "saved-head"); err != nil {
				t.Fatal(err)
			}
			bulletinMirrorAgeAttempt(t, h, `UPDATE bulletin_sync SET listed_at = now() - interval '11 minutes' WHERE singleton`)
			downstream := h.api.server.Config.Handler
			h.api.server.Config.Handler = http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.URL.Path == "/v1/bulletin" {
					h.api.writeProblem(w, status, "unavailable", "synthetic cursor response")
					return
				}
				downstream.ServeHTTP(w, r)
			})
			if err := runner.syncBulletinMirror(h.ctx); err != nil {
				t.Fatal(err)
			}
			bulletinMirrorAgeAttempt(t, h, `UPDATE bulletin_sync SET listed_at = now() - interval '11 minutes' WHERE singleton`)
			claim, err := h.db.ClaimBulletinList(h.ctx, h.account.ID)
			if err != nil {
				t.Fatal(err)
			}
			want := "saved-head"
			if status == 400 || status == 404 || status == 410 || status == 422 {
				want = ""
			}
			if !claim.Claimed || claim.HeadCursor != want {
				t.Fatalf("status %d left cursor %q, want %q", status, claim.HeadCursor, want)
			}
		})
	}
}

func TestBulletinMirrorReleasesUnrequestedDetailClaims(t *testing.T) {
	h, _, runner := newBulletinMirrorFixture(t)
	base := time.Now().UTC()
	ids := []string{uuid.NewString(), uuid.NewString()}
	threads := []store.BulletinThread{
		{ID: ids[0], Title: "First", CreatedAt: base, LastPostAt: base},
		{ID: ids[1], Title: "Second", CreatedAt: base, LastPostAt: base},
	}
	if err := h.db.SaveBulletinThreads(h.ctx, h.account.ID, threads); err != nil {
		t.Fatal(err)
	}
	if _, err := h.db.ClaimBulletinList(h.ctx, h.account.ID); err != nil {
		t.Fatal(err)
	}
	requested := make(chan string, 2)
	downstream := h.api.server.Config.Handler
	h.api.server.Config.Handler = http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/v1/bulletin/thread" {
			requested <- r.URL.Query().Get("id")
			<-r.Context().Done()
			return
		}
		downstream.ServeHTTP(w, r)
	})
	if err := runner.syncBulletinMirror(h.ctx); err != nil {
		t.Fatal(err)
	}
	if len(requested) != 1 {
		t.Fatalf("expected exactly one request before timeout, got %d", len(requested))
	}
	first := <-requested
	ready, err := h.db.BulletinThreadsNeedingDetail(h.ctx, 2)
	if err != nil || len(ready) != 1 || ready[0] == first {
		t.Fatalf("unrequested claim was backed off: requested=%s ready=%v err=%v", first, ready, err)
	}
}

func TestBulletinPoisonedSSEEventDoesNotWedgeRunner(t *testing.T) {
	h, _, runner := newBulletinMirrorFixture(t)
	thread, post := uuid.NewString(), uuid.NewString()
	payload := json.RawMessage(fmt.Sprintf(`{"payload":{"thread_id":%q,"post_id":%q,"title":"Quay news","company_name":"Salt Oval","body":"ordinary\u0000 text \ud800","extra\u0000key":"kept","is_reply":false},"timestamp":%q}`, thread, post, time.Now().UTC().Format(time.RFC3339Nano)))
	stream := interceptEventStream(t, h.api, []progressFrame{
		{id: "poisoned-bulletin", eventType: "bulletin_posted", data: payload},
		{id: "after-poison", eventType: progressOrdinaryEvent},
		{eventType: "auth_revoked"},
	}, 0)
	stream.frameFor = func(int32) progressFrame { return progressFrame{} }
	running := startRunner(t, runner, h.ctx)
	if err := running.wait(t, 15*time.Second); !errors.Is(err, ErrAuthorizationEnded) {
		t.Fatalf("runner stopped on poisoned data rather than the final control frame: %v", err)
	}
	if cursor, err := h.db.Cursor(h.ctx, h.account.ID); err != nil || cursor != "after-poison" {
		t.Fatalf("poisoned SSE event wedged cursor: %q, %v", cursor, err)
	}
	view, err := h.db.BulletinThread(h.ctx, thread)
	if err != nil || len(view.Posts) != 1 || view.Posts[0].Body != "ordinary text �" {
		t.Fatalf("safe bulletin content was not mirrored: %+v, %v", view, err)
	}
}
