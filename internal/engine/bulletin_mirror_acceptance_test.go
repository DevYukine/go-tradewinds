package engine

// Acceptance contracts for the bulletin mirror (plan G1) against the fake
// game API and a disposable real PostgreSQL schema: bulletin_posted events
// persist posts without any game bulletin request, the idle mirror lists the
// board once per persisted interval, fetches at most two thread details per
// work call, resumes the backfill cursor across restarts before switching to
// head scans, resets a stored cursor the game refuses, keeps a failing thread
// from blocking other threads, marks a 404 thread as synced without removing
// it, refetches a synced thread exactly once for a new post stub, records a
// mirror decision on ordinary failures while cancellation, ownership loss and
// authorization refusals propagate, and an unavailable board never holds or
// stops ship trading, even when public posting is disabled. work() calls
// syncBulletinMirror in its idle position; these tests drive that helper
// directly and the full runner seams through h.tick. They skip when
// TEST_DATABASE_URL is absent.

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"slices"
	"testing"
	"time"

	"github.com/DevYukine/go-tradewinds/internal/store"
	"github.com/google/uuid"
)

type bulletinMirrorAPI struct {
	*bulletinTestAPI
	detailReads    int            // guarded by bulletinTestAPI.mu
	headReads      int            // guarded by bulletinTestAPI.mu
	backfillReads  int            // guarded by bulletinTestAPI.mu
	seenCursors    []string       // guarded by bulletinTestAPI.mu
	failDetail     map[string]int // thread id to forced problem status
	failListCursor string         // backfill cursor that must answer 400
}

func (p *bulletinMirrorAPI) ServeHTTP(writer http.ResponseWriter, request *http.Request) {
	if request.Method == http.MethodGet && request.URL.Path == "/v1/bulletin" {
		cursor := request.URL.Query().Get("cursor")
		p.mu.Lock()
		if request.URL.Query().Get("since") != "" {
			p.headReads++
		} else {
			p.backfillReads++
		}
		p.seenCursors = append(p.seenCursors, cursor)
		refusedCursor := p.failListCursor
		p.mu.Unlock()
		if refusedCursor != "" && cursor == refusedCursor {
			p.h.api.writeProblem(writer, http.StatusBadRequest, "invalid_cursor", "synthetic stored cursor refusal")
			return
		}
	} else if request.Method == http.MethodGet && bulletinPath(request.URL.Path) {
		p.mu.Lock()
		p.detailReads++
		status := p.failDetail[request.URL.Query().Get("id")]
		p.mu.Unlock()
		if status != 0 {
			p.h.api.writeProblem(writer, status, "unavailable", "synthetic thread detail outage")
			return
		}
	}
	p.bulletinTestAPI.ServeHTTP(writer, request)
}

func newBulletinMirrorFixture(t *testing.T) (*recoveryHarness, *bulletinMirrorAPI, *Runner) {
	t.Helper()
	h := newRecoveryHarness(t)
	proxy := &bulletinMirrorAPI{
		bulletinTestAPI: &bulletinTestAPI{
			h: h, downstream: h.api.server.Config.Handler,
			threads: []bulletinThread{}, details: make(map[string]bulletinDetail),
			posted: make(map[string]bulletinPost), bodies: make(map[string][]byte),
		},
		failDetail: make(map[string]int),
	}
	h.api.server.Config.Handler = proxy
	runner := h.runner(primaryTestKey)
	refreshBulletinRunner(t, h, runner)
	return h, proxy, runner
}

func bulletinMirrorCounts(t *testing.T, proxy *bulletinMirrorAPI) (int, int, int) {
	t.Helper()
	proxy.mu.Lock()
	defer proxy.mu.Unlock()
	return proxy.headReads, proxy.backfillReads, proxy.detailReads
}

func bulletinMirrorSyncedAt(t *testing.T, h *recoveryHarness, id string) *time.Time {
	t.Helper()
	view, err := h.db.BulletinThread(h.ctx, id)
	if err != nil {
		t.Fatal(err)
	}
	return view.Thread.DetailSyncedAt
}

func bulletinMirrorKnownPost(t *testing.T, proxy *bulletinMirrorAPI, thread string) string {
	t.Helper()
	proxy.mu.Lock()
	defer proxy.mu.Unlock()
	detail := proxy.details[thread]
	if len(detail.Posts) == 0 {
		t.Fatalf("fixture thread %s has no known post", thread)
	}
	return detail.Posts[0].ID
}

func bulletinMirrorDecisionRecorded(t *testing.T, h *recoveryHarness) {
	t.Helper()
	rows, err := h.db.History(h.ctx, h.account.ID, "decisions", 50)
	if err != nil {
		t.Fatal(err)
	}
	decisions, ok := rows.([]store.Decision)
	if !ok {
		t.Fatalf("decision history returned %T", rows)
	}
	if !slices.ContainsFunc(decisions, func(row store.Decision) bool {
		return row.ShipID == "__bulletin_mirror" && row.Kind == "bulletin_mirror"
	}) {
		t.Fatalf("an ordinary mirror failure recorded no diagnostic decision: %+v", decisions)
	}
}

func bulletinMirrorAgeAttempt(t *testing.T, h *recoveryHarness, statement string, args ...any) {
	t.Helper()
	admin, err := sql.Open("pgx", h.dsn)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = admin.Close() }()
	if _, err := admin.ExecContext(h.ctx, statement, args...); err != nil {
		t.Fatal(err)
	}
}

func TestBulletinEventRecordedWithoutGameRequests(t *testing.T) {
	h, proxy, runner := newBulletinMirrorFixture(t)
	opened := time.Now().UTC().Add(-time.Hour).Truncate(time.Microsecond)
	threadID, postID := uuid.NewString(), uuid.NewString()
	event := fmt.Sprintf(
		`{"thread_id":%q,"post_id":%q,"title":"Grain prices at the quay","company_name":"The Kraken Cartel","body":"Rye is scarce this season.","is_reply":false,"timestamp":%q}`,
		threadID, postID, opened.Format(time.RFC3339Nano))
	runner.recordBulletinEvent(h.ctx, json.RawMessage(event))
	if head, backfill, details := bulletinMirrorCounts(t, proxy); head+backfill+details != 0 {
		t.Fatalf("recording an event made %d game bulletin requests", head+backfill+details)
	}
	view, err := h.db.BulletinThread(h.ctx, threadID)
	if err != nil {
		t.Fatal(err)
	}
	if view.Thread.Title != "Grain prices at the quay" || view.Thread.CompanyName != "The Kraken Cartel" {
		t.Fatalf("an opening event did not create its thread: %+v", view.Thread)
	}
	if len(view.Posts) != 1 || !view.Posts[0].IsOpening || view.Posts[0].Body != "Rye is scarce this season." || !view.Posts[0].CreatedAt.Equal(opened) {
		t.Fatalf("an opening event did not store its post: %+v", view.Posts)
	}
	// The wrapped SSE envelope decodes the same way, without an inner timestamp.
	replied := time.Now().UTC().Add(-30 * time.Minute).Truncate(time.Microsecond)
	replyThread, replyPost := uuid.NewString(), uuid.NewString()
	wrapped := fmt.Sprintf(
		`{"payload":{"thread_id":%q,"post_id":%q,"title":"Harbour gossip","company_name":"Salt Oval","body":"Charter seen at the quay.","is_reply":true},"timestamp":%q}`,
		replyThread, replyPost, replied.Format(time.RFC3339Nano))
	runner.recordBulletinEvent(h.ctx, json.RawMessage(wrapped))
	view, err = h.db.BulletinThread(h.ctx, replyThread)
	if err != nil {
		t.Fatal(err)
	}
	if view.Thread.CompanyName != "" {
		t.Fatalf("a reply stub invented a thread author: %+v", view.Thread)
	}
	if len(view.Posts) != 1 || view.Posts[0].IsOpening || !view.Posts[0].CreatedAt.Equal(replied) || view.Posts[0].CompanyName != "Salt Oval" {
		t.Fatalf("a wrapped reply event did not store its post: %+v", view.Posts)
	}
	board, err := h.db.BulletinBoard(h.ctx, store.BulletinQuery{})
	if err != nil {
		t.Fatal(err)
	}
	if board.Total != 2 || slices.Contains(board.Companies, "") {
		t.Fatalf("event stubs produced unexpected board state: total %d companies %v", board.Total, board.Companies)
	}
	// A replayed event keeps exactly one post row.
	runner.recordBulletinEvent(h.ctx, json.RawMessage(event))
	board, err = h.db.BulletinBoard(h.ctx, store.BulletinQuery{})
	if err != nil {
		t.Fatal(err)
	}
	for _, thread := range board.Threads {
		if thread.ID == threadID && thread.PostsRecorded != 1 {
			t.Fatalf("a replayed event duplicated its post: %+v", thread)
		}
	}
	// An undecodable payload is ignored without store changes.
	runner.recordBulletinEvent(h.ctx, json.RawMessage(`{"thread_id":"not-a-uuid"}`))
	board, err = h.db.BulletinBoard(h.ctx, store.BulletinQuery{})
	if err != nil {
		t.Fatal(err)
	}
	if board.Total != 2 {
		t.Fatalf("a malformed event created a thread: %d", board.Total)
	}
	if head, backfill, details := bulletinMirrorCounts(t, proxy); head+backfill+details != 0 {
		t.Fatalf("event recording made %d game bulletin requests", head+backfill+details)
	}
}

func TestBulletinMirrorIdleSyncFetchesTwoDetailsPerListInterval(t *testing.T) {
	h, proxy, runner := newBulletinMirrorFixture(t)
	proxy.addThread("The Kraken Cartel", time.Hour, false, false)
	proxy.addThread("The Kraken Cartel", 2*time.Hour, false, false)
	third := proxy.addThread("The Kraken Cartel", 3*time.Hour, false, false)
	fourth := proxy.addThread("The Kraken Cartel", 4*time.Hour, false, false)
	fifth := proxy.addThread("The Kraken Cartel", 5*time.Hour, false, false)
	oldest := proxy.addThread("The Kraken Cartel", 6*time.Hour, false, false)
	if err := runner.syncBulletinMirror(h.ctx); err != nil {
		t.Fatalf("the first idle mirror sync failed: %v", err)
	}
	head, backfill, details := bulletinMirrorCounts(t, proxy)
	if head != 1 || backfill != 1 || details != 2 {
		t.Fatalf("the first idle sync read %d head and %d backfill pages and %d details, want 1, 1 and 2", head, backfill, details)
	}
	need, err := h.db.BulletinThreadsNeedingDetail(h.ctx, 10)
	if err != nil {
		t.Fatal(err)
	}
	if want := []string{third, fourth, fifth, oldest}; !slices.Equal(need, want) {
		t.Fatalf("needing detail after one sync got %v, want %v", need, want)
	}
	// A fresh runner shares the persisted list interval across restarts: no
	// board reads at all, but the remaining details still land.
	restarted := h.runner(primaryTestKey)
	refreshBulletinRunner(t, h, restarted)
	if err := restarted.syncBulletinMirror(h.ctx); err != nil {
		t.Fatalf("a mirror sync after restart failed: %v", err)
	}
	head, backfill, details = bulletinMirrorCounts(t, proxy)
	if head != 1 || backfill != 1 || details != 4 {
		t.Fatalf("the second sync read %d head and %d backfill pages and %d details, want 1, 1 and 4", head, backfill, details)
	}
	need, err = h.db.BulletinThreadsNeedingDetail(h.ctx, 10)
	if err != nil {
		t.Fatal(err)
	}
	if want := []string{fifth, oldest}; !slices.Equal(need, want) {
		t.Fatalf("needing detail after two syncs got %v, want %v", need, want)
	}
	if err := restarted.syncBulletinMirror(h.ctx); err != nil {
		t.Fatalf("the third mirror sync failed: %v", err)
	}
	head, backfill, details = bulletinMirrorCounts(t, proxy)
	if head != 1 || backfill != 1 || details != 6 {
		t.Fatalf("the third sync read %d head and %d backfill pages and %d details, want 1, 1 and 6", head, backfill, details)
	}
	need, err = h.db.BulletinThreadsNeedingDetail(h.ctx, 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(need) != 0 {
		t.Fatalf("every thread settled after three syncs: %v", need)
	}
	board, err := h.db.BulletinBoard(h.ctx, store.BulletinQuery{})
	if err != nil {
		t.Fatal(err)
	}
	for _, thread := range board.Threads {
		if thread.DetailSyncedAt == nil || !thread.DetailCurrent {
			t.Fatalf("a synced thread lost its current detail: %+v", thread)
		}
	}
}

func TestBulletinMirrorBackfillResumesAcrossRestartsThenHeadScans(t *testing.T) {
	h, proxy, runner := newBulletinMirrorFixture(t)
	for index := range 550 {
		proxy.addThread("The Kraken Cartel", 3*time.Minute+time.Duration(index)*time.Minute, false, false)
	}
	if err := runner.syncBulletinMirror(h.ctx); err != nil {
		t.Fatalf("the backfill sync failed: %v", err)
	}
	head, backfill, details := bulletinMirrorCounts(t, proxy)
	if head != 1 || backfill != 4 || details != 2 {
		t.Fatalf("the backfill spent %d head and %d backfill pages and %d details, want 1, 4 and 2", head, backfill, details)
	}
	board, err := h.db.BulletinBoard(h.ctx, store.BulletinQuery{})
	if err != nil {
		t.Fatal(err)
	}
	if board.Total != 400 {
		t.Fatalf("the first backfill pass listed %d threads, want 400", board.Total)
	}
	if board.SyncedAt == nil {
		t.Fatal("a completed head scan did not stamp the board sync time")
	}
	firstSync := *board.SyncedAt
	// The claim cadence is persisted, so every later claim must age it first.
	bulletinMirrorAgeAttempt(t, h, "UPDATE bulletin_sync SET listed_at = now() - interval '11 minutes'")
	// A fresh runner resumes from the persisted cursor instead of restarting.
	restarted := h.runner(primaryTestKey)
	refreshBulletinRunner(t, h, restarted)
	if err := restarted.syncBulletinMirror(h.ctx); err != nil {
		t.Fatalf("the resume sync after restart failed: %v", err)
	}
	head, backfill, details = bulletinMirrorCounts(t, proxy)
	if head != 2 || backfill != 6 || details != 4 {
		t.Fatalf("the resume read %d head and %d backfill pages and %d details, want 2, 6 and 4", head, backfill, details)
	}
	board, err = h.db.BulletinBoard(h.ctx, store.BulletinQuery{})
	if err != nil {
		t.Fatal(err)
	}
	if board.Total != 550 || board.SyncedAt == nil || !board.SyncedAt.After(firstSync) {
		t.Fatalf("the exhausted backfill left %d threads with sync time %v, want a stamp after %v", board.Total, board.SyncedAt, firstSync)
	}
	// Later claims scan the head only and keep catching up on details.
	bulletinMirrorAgeAttempt(t, h, "UPDATE bulletin_sync SET listed_at = now() - interval '11 minutes'")
	if err := restarted.syncBulletinMirror(h.ctx); err != nil {
		t.Fatal(err)
	}
	head, backfill, details = bulletinMirrorCounts(t, proxy)
	if head != 3 || backfill != 6 || details != 6 {
		t.Fatalf("the head scan read %d head and %d backfill pages and %d details, want 3, 6 and 6", head, backfill, details)
	}
}

func TestBulletinMirrorCursorResetAfterBadStoredCursor(t *testing.T) {
	h, proxy, runner := newBulletinMirrorFixture(t)
	for range 5 {
		proxy.addThread("The Kraken Cartel", time.Hour, false, false)
	}
	if err := h.db.SaveBulletinListProgress(h.ctx, h.account.ID, "400", false); err != nil {
		t.Fatal(err)
	}
	proxy.failListCursor = "400"
	if err := runner.syncBulletinMirror(h.ctx); err != nil {
		t.Fatalf("a refused stored cursor failed the mirror: %v", err)
	}
	head, backfill, _ := bulletinMirrorCounts(t, proxy)
	if head != 1 || backfill != 1 {
		t.Fatalf("the refused cursor spent %d head and %d backfill reads, want 1 and 1", head, backfill)
	}
	if !slices.Equal(proxy.seenCursors, []string{"", "400"}) {
		t.Fatalf("the refused cursor was not requested as stored: %v", proxy.seenCursors)
	}
	proxy.failListCursor = ""
	bulletinMirrorAgeAttempt(t, h, "UPDATE bulletin_sync SET listed_at = now() - interval '11 minutes'")
	restarted := h.runner(primaryTestKey)
	refreshBulletinRunner(t, h, restarted)
	if err := restarted.syncBulletinMirror(h.ctx); err != nil {
		t.Fatalf("the sync after a cursor reset failed: %v", err)
	}
	head, backfill, _ = bulletinMirrorCounts(t, proxy)
	if head != 2 || backfill != 2 {
		t.Fatalf("the sync after a reset read %d head and %d backfill pages, want 2 and 2", head, backfill)
	}
	if !slices.Equal(proxy.seenCursors, []string{"", "400", "", ""}) {
		t.Fatalf("a refused cursor did not reset the backfill to the head: %v", proxy.seenCursors)
	}
	board, err := h.db.BulletinBoard(h.ctx, store.BulletinQuery{})
	if err != nil {
		t.Fatal(err)
	}
	if board.Total != 5 {
		t.Fatalf("the reset backfill listed %d threads, want 5", board.Total)
	}
}

func TestBulletinMirrorInvalidPageClearsSavedCursor(t *testing.T) {
	for _, scan := range []string{"head", "backfill"} {
		for _, invalid := range []string{"nil threads", "repeated cursor"} {
			t.Run(scan+"/"+invalid, func(t *testing.T) {
				h, proxy, runner := newBulletinMirrorFixture(t)
				var err error
				if scan == "head" {
					err = h.db.SaveBulletinHeadProgress(h.ctx, h.account.ID, "1")
				} else {
					err = h.db.SaveBulletinListProgress(h.ctx, h.account.ID, "1", false)
				}
				if err != nil {
					t.Fatal(err)
				}
				downstream := h.api.server.Config.Handler
				h.api.server.Config.Handler = http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
					if request.URL.Path == "/v1/bulletin" && request.URL.Query().Get("cursor") == "1" {
						proxy.mu.Lock()
						proxy.seenCursors = append(proxy.seenCursors, "1")
						proxy.mu.Unlock()
						page := bulletinPage{}
						if invalid == "repeated cursor" {
							page.Threads = []bulletinThread{}
							page.NextCursor = "1"
						}
						h.api.writeJSON(writer, http.StatusOK, page)
						return
					}
					downstream.ServeHTTP(writer, request)
				})
				if err := runner.syncBulletinMirror(h.ctx); err != nil {
					t.Fatalf("an invalid saved-cursor page failed the mirror: %v", err)
				}
				bulletinMirrorAgeAttempt(t, h, "UPDATE bulletin_sync SET listed_at = now() - interval '11 minutes'")
				claim, err := h.db.ClaimBulletinList(h.ctx, h.account.ID)
				if err != nil {
					t.Fatal(err)
				}
				if !claim.Claimed || claim.HeadCursor != "" || claim.ListCursor != "" {
					t.Fatalf("the claim after an invalid %s page retained a saved cursor: %+v", scan, claim)
				}
				bulletinMirrorAgeAttempt(t, h, "UPDATE bulletin_sync SET listed_at = now() - interval '11 minutes'")
				proxy.mu.Lock()
				proxy.seenCursors = nil
				proxy.mu.Unlock()
				if err := runner.syncBulletinMirror(h.ctx); err != nil {
					t.Fatalf("the first-page retry failed: %v", err)
				}
				proxy.mu.Lock()
				cursors := slices.Clone(proxy.seenCursors)
				proxy.mu.Unlock()
				if !slices.Equal(cursors, []string{"", ""}) {
					t.Fatalf("the retry did not restart head and backfill from page one: %v", cursors)
				}
			})
		}
	}
}

func TestBulletinMirrorDetailFailureDoesNotBlockOtherThreads(t *testing.T) {
	h, proxy, runner := newBulletinMirrorFixture(t)
	failing := proxy.addThread("The Kraken Cartel", time.Hour, false, false)
	other := proxy.addThread("Salt Oval", 5*time.Hour, false, false)
	proxy.failDetail[failing] = http.StatusInternalServerError
	if err := runner.syncBulletinMirror(h.ctx); err != nil {
		t.Fatalf("a failing thread detail failed the whole mirror: %v", err)
	}
	if syncedAt := bulletinMirrorSyncedAt(t, h, failing); syncedAt != nil {
		t.Fatalf("a 500 detail marked its thread synced: %v", syncedAt)
	}
	if syncedAt := bulletinMirrorSyncedAt(t, h, other); syncedAt == nil {
		t.Fatal("a failing sibling thread blocked another thread from syncing")
	}
	bulletinMirrorDecisionRecorded(t, h)
	need, err := h.db.BulletinThreadsNeedingDetail(h.ctx, 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(need) != 0 {
		t.Fatalf("a just failed thread stayed eligible before its backoff: %v", need)
	}
	bulletinMirrorAgeAttempt(t, h, "UPDATE bulletin_threads SET detail_attempted_at = now() - interval '11 minutes' WHERE id = $1", failing)
	need, err = h.db.BulletinThreadsNeedingDetail(h.ctx, 10)
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(need, []string{failing}) {
		t.Fatalf("the failed thread did not become eligible after its backoff: %v", need)
	}
	proxy.failDetail[failing] = 0
	restarted := h.runner(primaryTestKey)
	refreshBulletinRunner(t, h, restarted)
	if err := restarted.syncBulletinMirror(h.ctx); err != nil {
		t.Fatalf("the retry after a cleared failure failed: %v", err)
	}
	if syncedAt := bulletinMirrorSyncedAt(t, h, failing); syncedAt == nil {
		t.Fatal("the previously failing thread did not sync once its detail worked")
	}
	// A thread whose detail vanished from the game marks as synced instead of
	// retrying forever, without losing its board row. Its listing row already
	// sits in the mirror, so the persisted list cadence does not re-list.
	missing := uuid.NewString()
	at := time.Now().UTC().Add(-3 * time.Hour)
	if err := h.db.SaveBulletinThreads(h.ctx, h.account.ID, []store.BulletinThread{
		{ID: missing, Title: "Gone from the quay", CompanyName: "Harbour Union", CreatedAt: at, LastPostAt: at},
	}); err != nil {
		t.Fatal(err)
	}
	need, err = h.db.BulletinThreadsNeedingDetail(h.ctx, 10)
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(need, []string{missing}) {
		t.Fatalf("the vanished thread did not become eligible for detail: %v", need)
	}
	if err := restarted.syncBulletinMirror(h.ctx); err != nil {
		t.Fatalf("a 404 detail failed the mirror: %v", err)
	}
	need, err = h.db.BulletinThreadsNeedingDetail(h.ctx, 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(need) != 0 {
		t.Fatalf("a vanished thread stayed pending detail forever: %v", need)
	}
	board, err := h.db.BulletinBoard(h.ctx, store.BulletinQuery{})
	if err != nil {
		t.Fatal(err)
	}
	if board.Total != 3 || !slices.ContainsFunc(board.Threads, func(row store.BulletinThreadSummary) bool {
		return row.ID == missing && !row.Removed
	}) {
		t.Fatalf("the 404 removal lost or altered the board row: %+v", board.Threads)
	}
	if _, _, details := bulletinMirrorCounts(t, proxy); details != 4 {
		t.Fatalf("the failure flows made %d detail reads, want 4", details)
	}
	if head, backfill, _ := bulletinMirrorCounts(t, proxy); head != 1 || backfill != 1 {
		t.Fatalf("the retried syncs re-read the board: %d head and %d backfill reads", head, backfill)
	}
}

func TestBulletinEventStubForcesExactlyOneDetailRefetch(t *testing.T) {
	h, proxy, runner := newBulletinMirrorFixture(t)
	thread := proxy.addThread("The Kraken Cartel", time.Hour, false, false)
	if err := runner.syncBulletinMirror(h.ctx); err != nil {
		t.Fatalf("the initial mirror sync failed: %v", err)
	}
	if head, backfill, details := bulletinMirrorCounts(t, proxy); head != 1 || backfill != 1 || details != 1 {
		t.Fatalf("the initial sync read %d head and %d backfill pages and %d details, want 1, 1 and 1", head, backfill, details)
	}
	first := bulletinMirrorSyncedAt(t, h, thread)
	if first == nil {
		t.Fatal("the synced thread lost its detail sync time")
	}
	known := bulletinMirrorKnownPost(t, proxy, thread)
	stubbed := time.Now().UTC().Add(-90 * time.Minute).Truncate(time.Microsecond)
	knownEvent := fmt.Sprintf(
		`{"thread_id":%q,"post_id":%q,"title":"A public harbour conversation","company_name":"Salt Oval","body":"A replayed known reply.","is_reply":true,"timestamp":%q}`,
		thread, known, stubbed.Format(time.RFC3339Nano))
	runner.recordBulletinEvent(h.ctx, json.RawMessage(knownEvent))
	if err := runner.syncBulletinMirror(h.ctx); err != nil {
		t.Fatalf("the sync after a replayed stub failed: %v", err)
	}
	if _, _, details := bulletinMirrorCounts(t, proxy); details != 1 {
		t.Fatalf("a replayed stub for a known post forced a refetch: %d details", details)
	}
	// An older post time so only the new-post stub rule, not a newer
	// last_post_at, can schedule the refetch.
	newEvent := fmt.Sprintf(
		`{"thread_id":%q,"post_id":%q,"title":"A public harbour conversation","company_name":"Salt Oval","body":"An old reply replayed by the stream.","is_reply":true,"timestamp":%q}`,
		thread, uuid.NewString(), stubbed.Format(time.RFC3339Nano))
	runner.recordBulletinEvent(h.ctx, json.RawMessage(newEvent))
	if err := runner.syncBulletinMirror(h.ctx); err != nil {
		t.Fatalf("the sync after an event stub failed: %v", err)
	}
	if _, _, details := bulletinMirrorCounts(t, proxy); details != 2 {
		t.Fatalf("an event stub did not force exactly one refetch: %d details", details)
	}
	second := bulletinMirrorSyncedAt(t, h, thread)
	if second == nil || !second.After(*first) {
		t.Fatalf("the refetch did not advance the detail sync time: %v then %v", first, second)
	}
	// Follow-up passes must not refetch the settled thread again.
	if err := runner.syncBulletinMirror(h.ctx); err != nil {
		t.Fatal(err)
	}
	if _, _, details := bulletinMirrorCounts(t, proxy); details != 2 {
		t.Fatalf("the stub caused a refetch loop: %d details", details)
	}
	settled := bulletinMirrorSyncedAt(t, h, thread)
	if settled == nil || !settled.Equal(*second) {
		t.Fatalf("a settled thread changed its detail sync time: %v then %v", second, settled)
	}
}

func TestBulletinMirrorAuthFailureStopsExecution(t *testing.T) {
	for _, test := range []struct {
		name   string
		status int
	}{
		{"unauthorized", http.StatusUnauthorized},
		{"forbidden", http.StatusForbidden},
	} {
		t.Run(test.name, func(t *testing.T) {
			h, proxy, runner := newBulletinMirrorFixture(t)
			proxy.readStatus = test.status
			err := runner.syncBulletinMirror(h.ctx)
			if !errors.Is(err, ErrExecutionStopped) {
				t.Fatalf("an authorization refusal did not stop execution: %v", err)
			}
		})
	}
}

func TestBulletinMirrorCancellationAndOwnershipPropagate(t *testing.T) {
	h, _, runner := newBulletinMirrorFixture(t)
	canceled, cancel := context.WithCancel(h.ctx)
	cancel()
	if err := runner.syncBulletinMirror(canceled); !errors.Is(err, context.Canceled) {
		t.Fatalf("a canceled parent did not propagate: %v", err)
	}
	owned, release, err := h.db.AcquireAccountContext(h.ctx, h.account.ID)
	if err != nil {
		t.Fatal(err)
	}
	defer release()
	admin, err := sql.Open("pgx", h.dsn)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = admin.Close() }()
	// A successor owner fences this lease by bumping the table generation.
	if _, err := admin.ExecContext(h.ctx, "UPDATE accounts SET owner_generation = owner_generation + 1 WHERE id = $1", h.account.ID); err != nil {
		t.Fatal(err)
	}
	if err := runner.syncBulletinMirror(owned); !errors.Is(err, store.ErrOwnershipLost) {
		t.Fatalf("lost ownership did not propagate: %v", err)
	}
}

func TestBulletinMirrorSkipsImminentShipWakeAndDeadline(t *testing.T) {
	for _, reason := range []string{"ship unlock", "next deadline", "elapsed deadline"} {
		t.Run(reason, func(t *testing.T) {
			h, proxy, runner := newBulletinMirrorFixture(t)
			switch reason {
			case "ship unlock":
				if len(runner.stateCache.Ships) == 0 {
					t.Fatal("the imminent-unlock fixture has no ship")
				}
				lower, _, ok := runner.serverBounds()
				if !ok {
					t.Fatal("the imminent-unlock fixture has no valid server clock")
				}
				unlock := lower.Add(5 * time.Second)
				runner.stateCache.Ships[0].Ship.TurnaroundUnlocksAt = &unlock
				if wake := runner.nextWake(); wake > 5*time.Second {
					t.Fatalf("a ship unlocking in five seconds did not schedule an imminent wake: %v", wake)
				}
			case "next deadline":
				runner.nextDeadline = time.Now().Add(5 * time.Second)
			case "elapsed deadline":
				runner.nextDeadline = time.Now().Add(-time.Second)
			}
			if err := runner.syncBulletinMirror(h.ctx); err != nil {
				t.Fatalf("skipping the mirror for %s failed: %v", reason, err)
			}
			if head, backfill, details := bulletinMirrorCounts(t, proxy); head+backfill+details != 0 {
				t.Fatalf("%s allowed bulletin reads: head %d backfill %d details %d", reason, head, backfill, details)
			}
		})
	}
}

func TestBulletinBoardFailureLeavesTradingRunning(t *testing.T) {
	h := newRecoveryHarness(t)
	proxy := &bulletinMirrorAPI{
		bulletinTestAPI: &bulletinTestAPI{
			h: h, downstream: h.api.server.Config.Handler, readStatus: http.StatusInternalServerError,
			threads: []bulletinThread{}, details: make(map[string]bulletinDetail),
			posted: make(map[string]bulletinPost), bodies: make(map[string][]byte),
		},
		failDetail: make(map[string]int),
	}
	h.api.server.Config.Handler = proxy
	h.api.setFactorPorts(recoveryFarPort)
	runner := h.runner(primaryTestKey)
	runner.opts.BulletinEnabled = false
	refreshBulletinRunner(t, h, runner)
	// The mirror proves its outage behavior on the same runner before any
	// trading starts: the board read fails and only a decision is recorded.
	if err := runner.syncBulletinMirror(h.ctx); err != nil {
		t.Fatalf("the pre-trade mirror sync failed: %v", err)
	}
	headBeforeTrade, backfillBeforeTrade, _ := bulletinMirrorCounts(t, proxy)
	if headBeforeTrade+backfillBeforeTrade == 0 {
		t.Fatal("the mirror never attempted the board before trading")
	}
	bulletinMirrorDecisionRecorded(t, h)
	// The same runner then berths and trades while the board stays refused.
	berthed := sailingShipRow(recoveryShipLapras, "Lapras", recoveryHomePort, recoveryFarPort)
	berthed["ship"].(map[string]any)["status"] = "berthed"
	berthed["ship"].(map[string]any)["cannons_count"] = 4
	berthed["effective_hold_capacity_tons"] = 52
	berthed["stores"] = map[string]any{"provisions_tons": 4, "sea_days_remaining": 5}
	berthed["route_legs"] = []any{}
	berthed["voyage"] = nil
	h.api.setFleet([]map[string]any{
		berthed,
		sailingShipRow(recoveryShipCharizard, "Charizard", recoveryFarPort, recoveryHomePort),
	})
	runner.meta["ships"] = DatasetMeta{}
	// Make another list attempt due during the trading ticks rather than
	// letting the failed pre-trade attempt's persisted cadence skip the mirror.
	bulletinMirrorAgeAttempt(t, h, "UPDATE bulletin_sync SET listed_at = now() - interval '11 minutes'")
	for tick := range 16 {
		if _, err := h.tick(runner); err != nil {
			t.Fatalf("an unavailable board stopped trading at tick %d: %v", tick, err)
		}
		head, backfill, _ := bulletinMirrorCounts(t, proxy)
		if h.api.mutationCount("/v1/fleet/depart") > 0 && h.api.mutationCount("/v1/market/place-order") > 0 &&
			head+backfill > headBeforeTrade+backfillBeforeTrade {
			break
		}
	}
	if h.api.mutationCount("/v1/market/place-order") == 0 || h.api.mutationCount("/v1/fleet/depart") == 0 {
		t.Fatalf("trading did not buy and depart while the board failed: purchases %+v departures %+v holds %+v",
			h.api.mutationsFor("/v1/market/place-order"), h.api.mutationsFor("/v1/fleet/depart"), runner.holding)
	}
	if head, backfill, _ := bulletinMirrorCounts(t, proxy); head+backfill <= headBeforeTrade+backfillBeforeTrade {
		t.Fatalf("trading ticks never retried the refused board: before %d after %d",
			headBeforeTrade+backfillBeforeTrade, head+backfill)
	}
	for _, hold := range runner.holding {
		if hold.ShipID == "__bulletin_mirror" {
			t.Fatalf("a bulletin outage added a mirror hold: %+v", runner.holding)
		}
	}
	// A separate empty-fleet pass also lists with Execute off and posting
	// disabled, without turning the refused board into a mirror hold.
	idleH, idleProxy, idleRunner := newBulletinMirrorFixture(t)
	idleH.api.setFleet([]map[string]any{})
	idleProxy.readStatus = http.StatusInternalServerError
	idleRunner.opts.Execute = false
	idleRunner.opts.BulletinEnabled = false
	if _, err := idleH.tick(idleRunner); err != nil {
		t.Fatalf("an idle pass with a refused board failed: %v", err)
	}
	if head, backfill, _ := bulletinMirrorCounts(t, idleProxy); head+backfill == 0 {
		t.Fatal("the idle mirror never attempted the refused board list")
	}
	for _, hold := range idleRunner.holding {
		if hold.ShipID == "__bulletin_mirror" {
			t.Fatalf("an idle pass with a refused board added a mirror hold: %+v", idleRunner.holding)
		}
	}
}
