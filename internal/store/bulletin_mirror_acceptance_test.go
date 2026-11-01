package store

// Acceptance contracts for the bulletin mirror tables (plan G1/G2) against a
// disposable real PostgreSQL schema: listing upserts keep one row per id and
// fence out older rows, detail upserts keep posts oldest first and update in
// place, event stubs never overwrite synced facts and force exactly one
// refetch for a new post, the stale-thread selector follows the documented
// staleness rules and attempt order, the board and thread readers order,
// filter, cap and truncate as the dashboard needs, oversized or control-laden
// text is cleaned on rune boundaries, and the seven-day prune leaves bulletin
// rows alone. The tests skip when TEST_DATABASE_URL is absent.

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
)

func TestBulletinListingUpsertKeepsOneRowPerThread(t *testing.T) {
	db, _, _ := integrationStore(t)
	ctx := context.Background()
	saveTestAccount(t, db, "bulletin-list-owner")
	base := time.Now().UTC().Truncate(time.Microsecond)
	first := uuid.NewString()
	second := uuid.NewString()
	if err := db.SaveBulletinThreads(ctx, "bulletin-list-owner", []BulletinThread{
		{ID: first, Title: "Grain prices", CompanyName: "The Kraken Cartel", CreatedAt: base.Add(-2 * time.Hour), LastPostAt: base.Add(-time.Hour), Replies: 2},
		{ID: second, Title: "Charter offers", CompanyName: "Salt Oval", CreatedAt: base.Add(-time.Hour), LastPostAt: base.Add(-30 * time.Minute), Replies: 1},
	}); err != nil {
		t.Fatal(err)
	}
	// A detail sync must survive later listing saves untouched.
	if err := db.SaveBulletinThreadDetail(ctx, "bulletin-list-owner", BulletinThread{
		ID: first, Title: "Grain prices", CompanyName: "The Kraken Cartel",
		CreatedAt: base.Add(-2 * time.Hour), LastPostAt: base.Add(-time.Hour), Replies: 2,
	}, []BulletinPost{
		{ID: uuid.NewString(), ThreadID: first, CompanyName: "The Kraken Cartel", Body: "Rye is scarce.", IsOpening: true, CreatedAt: base.Add(-2 * time.Hour)},
	}); err != nil {
		t.Fatal(err)
	}
	if err := db.SaveBulletinThreads(ctx, "bulletin-list-owner", []BulletinThread{
		{ID: first, Title: "Grain prices", CompanyName: "The Kraken Cartel", CreatedAt: base.Add(-2 * time.Hour), LastPostAt: base.Add(-time.Hour), Replies: 2},
		{ID: second, Title: "Charter offers", CompanyName: "Salt Oval", CreatedAt: base.Add(-time.Hour), LastPostAt: base.Add(-30 * time.Minute), Replies: 1},
	}); err != nil {
		t.Fatal(err)
	}
	board, err := db.BulletinBoard(ctx, BulletinQuery{})
	if err != nil {
		t.Fatal(err)
	}
	if board.Total != 2 || len(board.Threads) != 2 {
		t.Fatalf("repeated listing upserts did not keep one row per id: total %d threads %d", board.Total, len(board.Threads))
	}
	byID := make(map[string]BulletinThreadSummary, len(board.Threads))
	for _, thread := range board.Threads {
		byID[thread.ID] = thread
	}
	if byID[first].Replies != 2 || byID[first].Archived || byID[first].Removed {
		t.Fatalf("a repeated listing save changed stored flags: %+v", byID[first])
	}
	if byID[second].Replies != 1 || byID[second].Archived || byID[second].Removed {
		t.Fatalf("a repeated listing save changed stored flags: %+v", byID[second])
	}
	if byID[first].PostsRecorded != 1 || byID[first].DetailSyncedAt == nil {
		t.Fatalf("a listing save clobbered the recorded detail: %+v", byID[first])
	}
	if !byID[first].DetailCurrent {
		t.Fatalf("a matching listing and detail reported a stale thread: %+v", byID[first])
	}
	if board.SyncedAt != nil {
		t.Fatalf("a direct listing save reported a board list sync time: %v", board.SyncedAt)
	}
	// A same-age listing can mark the thread removed without inserting a new
	// row. Keep this separate from the matching-detail freshness assertion.
	if err := db.SaveBulletinThreads(ctx, "bulletin-list-owner", []BulletinThread{
		{ID: first, Title: "Grain prices", CompanyName: "The Kraken Cartel", CreatedAt: base.Add(-2 * time.Hour), LastPostAt: base.Add(-time.Hour), Replies: 2, Removed: true},
	}); err != nil {
		t.Fatal(err)
	}
	board, err = db.BulletinBoard(ctx, BulletinQuery{})
	if err != nil {
		t.Fatal(err)
	}
	if board.Total != 2 || len(board.Threads) != 2 {
		t.Fatalf("a removal listing changed the row count: total %d threads %d", board.Total, len(board.Threads))
	}
	removed := slices.IndexFunc(board.Threads, func(thread BulletinThreadSummary) bool { return thread.ID == first })
	if removed < 0 || !board.Threads[removed].Removed {
		t.Fatalf("a same-age listing did not apply removal in place: %+v", board.Threads)
	}
}

func TestBulletinListingGuardKeepsNewerRowsAgainstOlderSaves(t *testing.T) {
	db, _, _ := integrationStore(t)
	ctx := context.Background()
	saveTestAccount(t, db, "bulletin-guard-owner")
	base := time.Now().UTC().Truncate(time.Microsecond)
	thread := uuid.NewString()
	if err := db.SaveBulletinThreads(ctx, "bulletin-guard-owner", []BulletinThread{
		{ID: thread, Title: "Rye market", CompanyName: "Salt Oval", CreatedAt: base.Add(-time.Hour), LastPostAt: base.Add(-30 * time.Minute), Replies: 2},
	}); err != nil {
		t.Fatal(err)
	}
	if err := db.SaveBulletinThreads(ctx, "bulletin-guard-owner", []BulletinThread{
		{ID: thread, Title: "Rye market", CompanyName: "Salt Oval", CreatedAt: base.Add(-time.Hour), LastPostAt: base.Add(-20 * time.Minute), Replies: 5, Archived: true},
	}); err != nil {
		t.Fatal(err)
	}
	// An older board page must not roll back newer replies, archive or removal
	// facts observed from the newer page.
	if err := db.SaveBulletinThreads(ctx, "bulletin-guard-owner", []BulletinThread{
		{ID: thread, Title: "Rye market", CompanyName: "Salt Oval", CreatedAt: base.Add(-time.Hour), LastPostAt: base.Add(-25 * time.Minute), Replies: 1, Removed: true},
	}); err != nil {
		t.Fatal(err)
	}
	board, err := db.BulletinBoard(ctx, BulletinQuery{})
	if err != nil {
		t.Fatal(err)
	}
	if board.Total != 1 || len(board.Threads) != 1 {
		t.Fatalf("the guarded listing kept the wrong row count: total %d threads %d", board.Total, len(board.Threads))
	}
	got := board.Threads[0]
	if got.Replies != 5 || !got.Archived || got.Removed || !got.LastPostAt.Equal(base.Add(-20*time.Minute)) {
		t.Fatalf("an older listing rolled back newer facts: %+v", got)
	}
}

func TestBulletinThreadDetailUpsertsPostsOldestFirstInPlace(t *testing.T) {
	db, _, _ := integrationStore(t)
	ctx := context.Background()
	saveTestAccount(t, db, "bulletin-detail-owner")
	base := time.Now().UTC().Truncate(time.Microsecond)
	thread := uuid.NewString()
	opening := uuid.NewString()
	reply := uuid.NewString()
	tied := uuid.NewString()
	// The slice is deliberately newest first; the reader must reorder it.
	posts := []BulletinPost{
		{ID: reply, ThreadID: thread, CompanyName: "Replier Co", Body: "Agreed on rye.", IsOpening: false, CreatedAt: base.Add(-30 * time.Minute)},
		{ID: tied, ThreadID: thread, CompanyName: "Salt Oval", Body: "Also watching rye.", IsOpening: false, CreatedAt: base.Add(-30 * time.Minute)},
		{ID: opening, ThreadID: thread, CompanyName: "Salt Oval", Body: "Selling rye at the quay.", IsOpening: true, CreatedAt: base.Add(-2 * time.Hour)},
	}
	if err := db.SaveBulletinThreadDetail(ctx, "bulletin-detail-owner", BulletinThread{
		ID: thread, Title: "Rye market", CompanyName: "Salt Oval",
		CreatedAt: base.Add(-2 * time.Hour), LastPostAt: base.Add(-30 * time.Minute), Replies: 2,
	}, posts); err != nil {
		t.Fatal(err)
	}
	expectedTail := []string{tied, reply}
	slices.Sort(expectedTail)
	view, err := db.BulletinThread(ctx, thread)
	if err != nil {
		t.Fatal(err)
	}
	if len(view.Posts) != 3 || view.Posts[0].ID != opening || view.Posts[1].ID != expectedTail[0] || view.Posts[2].ID != expectedTail[1] {
		t.Fatalf("thread posts were not stored oldest first with id ties: %+v", view.Posts)
	}
	if view.PostsTruncated || view.PostsTotal != 3 {
		t.Fatalf("a small thread reported a truncated post window: truncated %t total %d", view.PostsTruncated, view.PostsTotal)
	}
	edited := base.Add(-29 * time.Minute)
	if err := db.SaveBulletinThreadDetail(ctx, "bulletin-detail-owner", BulletinThread{
		ID: thread, Title: "Rye market", CompanyName: "Salt Oval",
		CreatedAt: base.Add(-2 * time.Hour), LastPostAt: base.Add(-30 * time.Minute), Replies: 2,
	}, []BulletinPost{
		{ID: opening, ThreadID: thread, CompanyName: "Salt Oval", Body: "Selling wheat instead.", IsOpening: true, CreatedAt: base.Add(-2 * time.Hour), EditedAt: &edited},
		{ID: reply, ThreadID: thread, CompanyName: "Replier Co", Body: "Agreed on rye.", IsOpening: false, CreatedAt: base.Add(-30 * time.Minute), Removed: true, Deleted: true},
	}); err != nil {
		t.Fatal(err)
	}
	view, err = db.BulletinThread(ctx, thread)
	if err != nil {
		t.Fatal(err)
	}
	if len(view.Posts) != 3 {
		t.Fatalf("a repeated detail added post rows instead of updating in place: %+v", view.Posts)
	}
	if view.Thread.PostsRecorded != 3 || !view.Thread.DetailCurrent {
		t.Fatalf("the refetched thread lost its summary state: %+v", view.Thread)
	}
	byID := make(map[string]BulletinPostView, len(view.Posts))
	for _, post := range view.Posts {
		byID[post.ID] = post
	}
	if byID[opening].Body != "Selling wheat instead." || byID[opening].EditedAt == nil || !byID[opening].EditedAt.Equal(edited) {
		t.Fatalf("an edited post body or edit time was lost: %+v", byID[opening])
	}
	if !byID[reply].Removed || !byID[reply].Deleted {
		t.Fatalf("removed and deleted flags did not update in place: %+v", byID[reply])
	}
	if byID[tied].Body != "Also watching rye." || byID[tied].Removed || byID[tied].Deleted {
		t.Fatalf("an untouched post changed during the refetch: %+v", byID[tied])
	}
}

func TestBulletinThreadReaderReturnsNewestThousandPosts(t *testing.T) {
	db, _, _ := integrationStore(t)
	ctx := context.Background()
	saveTestAccount(t, db, "bulletin-window-owner")
	base := time.Now().UTC().Truncate(time.Microsecond)
	thread := uuid.NewString()
	posts := make([]BulletinPost, 0, 1001)
	for index := range 1001 {
		posts = append(posts, BulletinPost{
			ID: uuid.NewString(), ThreadID: thread, CompanyName: "Salt Oval",
			Body: "Post number for the window check.", IsOpening: index == 0,
			CreatedAt: base.Add(-time.Hour + time.Duration(index)*time.Second),
		})
	}
	if err := db.SaveBulletinThreadDetail(ctx, "bulletin-window-owner", BulletinThread{
		ID: thread, Title: "Long harbour thread", CompanyName: "Salt Oval",
		CreatedAt: base.Add(-time.Hour), LastPostAt: base, Replies: 1000,
	}, posts); err != nil {
		t.Fatal(err)
	}
	view, err := db.BulletinThread(ctx, thread)
	if err != nil {
		t.Fatal(err)
	}
	if len(view.Posts) != 1000 || !view.PostsTruncated || view.PostsTotal != 1001 || view.Thread.PostsRecorded != 1001 {
		t.Fatalf("the post window cap lost its bounds: posts %d truncated %t total %d recorded %d",
			len(view.Posts), view.PostsTruncated, view.PostsTotal, view.Thread.PostsRecorded)
	}
	// The window keeps the newest thousand posts, still oldest first inside it.
	if view.Posts[0].ID != posts[1].ID || view.Posts[len(view.Posts)-1].ID != posts[1000].ID {
		t.Fatalf("the newest thousand posts were not kept oldest first: first %s last %s",
			view.Posts[0].ID, view.Posts[len(view.Posts)-1].ID)
	}
}

func TestBulletinThreadDetailBatchesThousandsOfPosts(t *testing.T) {
	db, _, _ := integrationStore(t)
	ctx := context.Background()
	owner := "bulletin-batch-owner"
	saveTestAccount(t, db, owner)
	base := time.Now().UTC().Truncate(time.Microsecond)
	thread := uuid.NewString()
	posts := make([]BulletinPost, 5500)
	for index := range posts {
		posts[index] = BulletinPost{
			ID: uuid.NewString(), ThreadID: thread, CompanyName: "Salt Oval",
			Body: "A recorded reply.", IsOpening: index == 0,
			CreatedAt: base.Add(-2*time.Hour + time.Duration(index)*time.Second),
		}
	}
	// One unbatched insert would exceed PostgreSQL's 65,535 bind parameters.
	if err := db.SaveBulletinThreadDetail(ctx, owner, BulletinThread{
		ID: thread, Title: "Long harbour conversation", CompanyName: "Salt Oval",
		CreatedAt: posts[0].CreatedAt, LastPostAt: posts[len(posts)-1].CreatedAt, Replies: len(posts) - 1,
	}, posts); err != nil {
		t.Fatal(err)
	}
	var stored int64
	if err := db.db.WithContext(ctx).Table("bulletin_posts").Where("thread_id = ?", thread).Count(&stored).Error; err != nil {
		t.Fatal(err)
	}
	if stored != int64(len(posts)) {
		t.Fatalf("a batched detail save stored %d posts, want %d", stored, len(posts))
	}
}

func TestBulletinEventStubCreatesMissingThreadsAndKeepsSyncedFacts(t *testing.T) {
	db, _, _ := integrationStore(t)
	ctx := context.Background()
	saveTestAccount(t, db, "bulletin-stub-owner")
	base := time.Now().UTC().Truncate(time.Microsecond)
	openingThread := uuid.NewString()
	if err := db.SaveBulletinEventPost(ctx, "bulletin-stub-owner", "Grain prices", BulletinPost{
		ID: uuid.NewString(), ThreadID: openingThread, CompanyName: "The Kraken Cartel",
		Body: "Rye is scarce this season.", IsOpening: true, CreatedAt: base.Add(-time.Hour),
	}); err != nil {
		t.Fatal(err)
	}
	view, err := db.BulletinThread(ctx, openingThread)
	if err != nil {
		t.Fatal(err)
	}
	if view.Thread.Title != "Grain prices" || view.Thread.CompanyName != "The Kraken Cartel" || view.Thread.Replies != 0 {
		t.Fatalf("an opening stub did not create the thread with the poster company: %+v", view.Thread)
	}
	if len(view.Posts) != 1 || !view.Posts[0].IsOpening || view.Posts[0].CompanyName != "The Kraken Cartel" {
		t.Fatalf("an opening stub did not create its post: %+v", view.Posts)
	}
	replyThread := uuid.NewString()
	if err := db.SaveBulletinEventPost(ctx, "bulletin-stub-owner", "Harbour gossip", BulletinPost{
		ID: uuid.NewString(), ThreadID: replyThread, CompanyName: "Salt Oval",
		Body: "Charter seen at the quay.", IsOpening: false, CreatedAt: base.Add(-30 * time.Minute),
	}); err != nil {
		t.Fatal(err)
	}
	view, err = db.BulletinThread(ctx, replyThread)
	if err != nil {
		t.Fatal(err)
	}
	if view.Thread.CompanyName != "" {
		t.Fatalf("a reply stub invented a thread author: %+v", view.Thread)
	}
	if len(view.Posts) != 1 || view.Posts[0].IsOpening || view.Posts[0].CompanyName != "Salt Oval" {
		t.Fatalf("a reply stub did not create its post with the poster company: %+v", view.Posts)
	}
	board, err := db.BulletinBoard(ctx, BulletinQuery{})
	if err != nil {
		t.Fatal(err)
	}
	if slices.Contains(board.Companies, "") {
		t.Fatalf("board companies listed an empty name: %v", board.Companies)
	}
	// A replayed stub for a post that a detail already synced keeps the synced
	// facts and does not schedule a refetch.
	syncedThread := uuid.NewString()
	syncedPost := uuid.NewString()
	if err := db.SaveBulletinThreadDetail(ctx, "bulletin-stub-owner", BulletinThread{
		ID: syncedThread, Title: "Synced title", CompanyName: "The Kraken Cartel",
		CreatedAt: base.Add(-2 * time.Hour), LastPostAt: base, Replies: 3,
	}, []BulletinPost{
		{ID: syncedPost, ThreadID: syncedThread, CompanyName: "The Kraken Cartel", Body: "synced body", IsOpening: true, CreatedAt: base, EditedAt: &base, Removed: true},
	}); err != nil {
		t.Fatal(err)
	}
	if err := db.SaveBulletinEventPost(ctx, "bulletin-stub-owner", "Stub title", BulletinPost{
		ID: syncedPost, ThreadID: syncedThread, CompanyName: "Other Co",
		Body: "stub body", IsOpening: false, CreatedAt: base.Add(-2 * time.Hour),
	}); err != nil {
		t.Fatal(err)
	}
	view, err = db.BulletinThread(ctx, syncedThread)
	if err != nil {
		t.Fatal(err)
	}
	if view.Thread.Title != "Synced title" || view.Thread.Replies != 3 || !view.Thread.LastPostAt.Equal(base) {
		t.Fatalf("a stub overwrote synced thread fields or moved last_post_at back: %+v", view.Thread)
	}
	if len(view.Posts) != 1 || view.Posts[0].Body != "synced body" || !view.Posts[0].Removed || view.Posts[0].EditedAt == nil || view.Posts[0].CompanyName != "The Kraken Cartel" {
		t.Fatalf("a stub overwrote a detail synced post: %+v", view.Posts)
	}
	need, err := db.BulletinThreadsNeedingDetail(ctx, 10)
	if err != nil {
		t.Fatal(err)
	}
	if slices.Contains(need, syncedThread) {
		t.Fatalf("a replayed stub for a known post scheduled a refetch: %v", need)
	}
	// A stub carrying a new post schedules exactly one refetch.
	if err := db.SaveBulletinEventPost(ctx, "bulletin-stub-owner", "Stub title", BulletinPost{
		ID: uuid.NewString(), ThreadID: syncedThread, CompanyName: "Other Co",
		Body: "a fresh reply from the stream", IsOpening: false, CreatedAt: base.Add(-90 * time.Minute),
	}); err != nil {
		t.Fatal(err)
	}
	need, err = db.BulletinThreadsNeedingDetail(ctx, 10)
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Contains(need, syncedThread) {
		t.Fatalf("a stub with a new post did not schedule one refetch: %v", need)
	}
	if err := db.SaveBulletinThreadDetail(ctx, "bulletin-stub-owner", BulletinThread{
		ID: syncedThread, Title: "Synced title", CompanyName: "The Kraken Cartel",
		CreatedAt: base.Add(-2 * time.Hour), LastPostAt: base, Replies: 3,
	}, []BulletinPost{
		{ID: syncedPost, ThreadID: syncedThread, CompanyName: "The Kraken Cartel", Body: "synced body", IsOpening: true, CreatedAt: base, EditedAt: &base, Removed: true},
	}); err != nil {
		t.Fatal(err)
	}
	need, err = db.BulletinThreadsNeedingDetail(ctx, 10)
	if err != nil {
		t.Fatal(err)
	}
	if slices.Contains(need, syncedThread) {
		t.Fatalf("the refetch after a stub did not settle the thread: %v", need)
	}
}

func TestBulletinThreadsNeedingDetailSelectsStaleThreadsInActivityOrder(t *testing.T) {
	db, _, _ := integrationStore(t)
	ctx := context.Background()
	owner := "bulletin-selector-owner"
	saveTestAccount(t, db, owner)
	base := time.Now().UTC().Truncate(time.Microsecond)
	newest := uuid.NewString()
	advanced := uuid.NewString()
	mismatch := uuid.NewString()
	editWindow := uuid.NewString()
	staleOld := uuid.NewString()
	archivedOld := uuid.NewString()
	removedOld := uuid.NewString()
	current := uuid.NewString()
	saveDetail := func(id string, lastPost time.Time, replies int, archived, removed bool, posts []BulletinPost) {
		t.Helper()
		thread := BulletinThread{
			ID: id, Title: "Thread " + id[:8], CompanyName: "Salt Oval",
			CreatedAt: lastPost.Add(-time.Hour), LastPostAt: lastPost, Replies: replies,
			Archived: archived, Removed: removed,
		}
		if err := db.SaveBulletinThreadDetail(ctx, owner, thread, posts); err != nil {
			t.Fatal(err)
		}
	}
	// Never detailed, newest activity.
	if err := db.SaveBulletinThreads(ctx, owner, []BulletinThread{
		{ID: newest, Title: "Fresh thread", CompanyName: "Salt Oval", CreatedAt: base.Add(-2 * time.Hour), LastPostAt: base.Add(-time.Minute)},
	}); err != nil {
		t.Fatal(err)
	}
	// Detailed two hours ago, then the listing advanced its activity.
	saveDetail(advanced, base.Add(-2*time.Hour), 1, false, false, []BulletinPost{
		{ID: uuid.NewString(), ThreadID: advanced, CompanyName: "Salt Oval", Body: "Opening words.", IsOpening: true, CreatedAt: base.Add(-2 * time.Hour)},
	})
	if err := db.SaveBulletinThreads(ctx, owner, []BulletinThread{
		{ID: advanced, Title: "Thread " + advanced[:8], CompanyName: "Salt Oval", CreatedAt: base.Add(-3 * time.Hour), LastPostAt: base.Add(-2 * time.Minute), Replies: 1},
	}); err != nil {
		t.Fatal(err)
	}
	// Reply count changed without any last_post_at change.
	saveDetail(mismatch, base.Add(-3*time.Minute), 1, false, false, []BulletinPost{
		{ID: uuid.NewString(), ThreadID: mismatch, CompanyName: "Salt Oval", Body: "One reply so far.", IsOpening: true, CreatedAt: base.Add(-3 * time.Minute)},
	})
	if err := db.SaveBulletinThreads(ctx, owner, []BulletinThread{
		{ID: mismatch, Title: "Thread " + mismatch[:8], CompanyName: "Salt Oval", CreatedAt: base.Add(-4 * time.Hour), LastPostAt: base.Add(-3 * time.Minute), Replies: 4},
	}); err != nil {
		t.Fatal(err)
	}
	// A post edit window that closes a moment into the run.
	editableUntil := base.Add(2 * time.Second)
	saveDetail(editWindow, base.Add(-4*time.Minute), 0, false, false, []BulletinPost{
		{ID: uuid.NewString(), ThreadID: editWindow, CompanyName: "Salt Oval", Body: "Will edit these prices.", IsOpening: true, CreatedAt: base.Add(-4 * time.Minute), EditableUntil: &editableUntil},
	})
	// Active thread with an aged detail, and its archived and removed
	// neighbours that must not refresh.
	oldPost := func(id string) []BulletinPost {
		return []BulletinPost{
			{ID: uuid.NewString(), ThreadID: id, CompanyName: "Salt Oval", Body: "Quiet thread.", IsOpening: true, CreatedAt: base.Add(-8 * time.Hour)},
		}
	}
	saveDetail(staleOld, base.Add(-5*time.Minute), 0, false, false, oldPost(staleOld))
	saveDetail(archivedOld, base.Add(-6*time.Minute), 0, true, false, oldPost(archivedOld))
	saveDetail(removedOld, base.Add(-7*time.Minute), 0, false, true, oldPost(removedOld))
	saveDetail(current, base.Add(-8*time.Minute), 0, false, false, oldPost(current))
	if err := db.db.WithContext(ctx).Exec(
		"UPDATE bulletin_threads SET detail_synced_at = now() - interval '8 hours', invalidated_at = now() - interval '8 hours' WHERE id IN (?, ?, ?)",
		staleOld, archivedOld, removedOld).Error; err != nil {
		t.Fatal(err)
	}
	need, err := db.BulletinThreadsNeedingDetail(ctx, 10)
	if err != nil {
		t.Fatal(err)
	}
	// Listing-derived staleness sorts first, then never-attempted threads,
	// each group newest activity first. The edit window is still open here.
	want := []string{advanced, mismatch, newest, staleOld}
	if !slices.Equal(need, want) {
		t.Fatalf("stale thread selection got %v, want %v", need, want)
	}
	time.Sleep(2500 * time.Millisecond)
	need, err = db.BulletinThreadsNeedingDetail(ctx, 10)
	if err != nil {
		t.Fatal(err)
	}
	want = []string{advanced, mismatch, newest, editWindow, staleOld}
	if !slices.Equal(need, want) {
		t.Fatalf("a closed edit window did not schedule one refetch: got %v, want %v", need, want)
	}
	bounded, err := db.BulletinThreadsNeedingDetail(ctx, 2)
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(bounded, want[:2]) {
		t.Fatalf("the limit did not bound the selection: %v", bounded)
	}
	// One refetch settles the closed edit window without further staleness.
	saveDetail(editWindow, base.Add(-4*time.Minute), 0, false, false, []BulletinPost{
		{ID: uuid.NewString(), ThreadID: editWindow, CompanyName: "Salt Oval", Body: "Prices edited.", IsOpening: true, CreatedAt: base.Add(-4 * time.Minute)},
	})
	need, err = db.BulletinThreadsNeedingDetail(ctx, 10)
	if err != nil {
		t.Fatal(err)
	}
	if slices.Contains(need, editWindow) {
		t.Fatalf("a settled edit window scheduled another refetch: %v", need)
	}
}

func TestBulletinBoardReaderOrdersFiltersAndSummarizes(t *testing.T) {
	db, _, _ := integrationStore(t)
	ctx := context.Background()
	owner := "bulletin-board-owner"
	saveTestAccount(t, db, owner)
	base := time.Now().UTC().Truncate(time.Microsecond)
	empty, err := db.BulletinBoard(ctx, BulletinQuery{})
	if err != nil {
		t.Fatal(err)
	}
	if empty.Total != 0 || len(empty.Threads) != 0 || len(empty.Companies) != 0 || empty.SyncedAt != nil {
		t.Fatalf("an empty mirror reported content or freshness: %+v", empty)
	}
	fresh := uuid.NewString()
	grain := uuid.NewString()
	ancient := uuid.NewString()
	if err := db.SaveBulletinThreadDetail(ctx, owner, BulletinThread{
		ID: fresh, Title: "Fresh arrivals", CompanyName: "Harbour Union",
		CreatedAt: base.Add(-time.Hour), LastPostAt: base.Add(-30 * time.Minute), Replies: 1,
	}, []BulletinPost{
		{ID: uuid.NewString(), ThreadID: fresh, CompanyName: "Harbour Union", Body: "Fresh arrivals at dawn", IsOpening: true, CreatedAt: base.Add(-time.Hour)},
		{ID: uuid.NewString(), ThreadID: fresh, CompanyName: "Replier Co", Body: "Seeing the same prices here", IsOpening: false, CreatedAt: base.Add(-30 * time.Minute)},
	}); err != nil {
		t.Fatal(err)
	}
	if err := db.SaveBulletinThreads(ctx, owner, []BulletinThread{
		{ID: grain, Title: "Grain talk", CompanyName: "The Kraken Cartel", CreatedAt: base.Add(-3 * time.Hour), LastPostAt: base.Add(-2 * time.Hour), Replies: 2},
		// Stored unarchived, but quiet for more than three days: readers must
		// report it archived.
		{ID: ancient, Title: "Old colours", CompanyName: "Salt Oval", CreatedAt: base.Add(-80 * time.Hour), LastPostAt: base.Add(-73 * time.Hour)},
	}); err != nil {
		t.Fatal(err)
	}
	board, err := db.BulletinBoard(ctx, BulletinQuery{})
	if err != nil {
		t.Fatal(err)
	}
	if board.Total != 3 || len(board.Threads) != 3 {
		t.Fatalf("board totals lost threads: total %d threads %d", board.Total, len(board.Threads))
	}
	if board.Threads[0].ID != fresh || board.Threads[1].ID != grain || board.Threads[2].ID != ancient {
		t.Fatalf("board threads were not newest activity first: %+v", board.Threads)
	}
	wantCompanies := []string{"Harbour Union", "Replier Co", "Salt Oval", "The Kraken Cartel"}
	if !slices.Equal(board.Companies, wantCompanies) {
		t.Fatalf("board companies got %v, want sorted unique %v", board.Companies, wantCompanies)
	}
	if board.CompaniesTruncated {
		t.Fatal("a small company list reported truncation")
	}
	if board.Threads[0].PostsRecorded != 2 || !board.Threads[0].DetailCurrent || board.Threads[0].DetailSyncedAt == nil {
		t.Fatalf("the detailed thread lost its summary state: %+v", board.Threads[0])
	}
	if board.Threads[1].DetailCurrent {
		t.Fatalf("a never detailed thread reported a current detail: %+v", board.Threads[1])
	}
	if !board.Threads[2].Archived || board.Threads[2].Removed {
		t.Fatalf("a thread quiet for three days was not reported archived: %+v", board.Threads[2])
	}
	board, err = db.BulletinBoard(ctx, BulletinQuery{Search: "seeing the SAME prices"})
	if err != nil {
		t.Fatal(err)
	}
	if board.Total != 1 || len(board.Threads) != 1 || board.Threads[0].ID != fresh {
		t.Fatalf("case insensitive body search matched %v: %+v", board.Threads, board)
	}
	board, err = db.BulletinBoard(ctx, BulletinQuery{Company: "Replier Co"})
	if err != nil {
		t.Fatal(err)
	}
	if board.Total != 1 || len(board.Threads) != 1 || board.Threads[0].ID != fresh {
		t.Fatalf("a replier company filter matched %v: %+v", board.Threads, board)
	}
	board, err = db.BulletinBoard(ctx, BulletinQuery{Limit: 1})
	if err != nil {
		t.Fatal(err)
	}
	if board.Total != 3 || len(board.Threads) != 1 {
		t.Fatalf("the limit did not keep the total count before it: total %d threads %d", board.Total, len(board.Threads))
	}
	if board.SyncedAt != nil {
		t.Fatalf("direct store writes reported a board list sync time: %v", board.SyncedAt)
	}
}

func TestBulletinBoardCapsCompaniesAtThousand(t *testing.T) {
	db, _, _ := integrationStore(t)
	ctx := context.Background()
	saveTestAccount(t, db, "bulletin-company-cap-owner")
	threads := make([]BulletinThread, 0, 1001)
	for index := range 1001 {
		threads = append(threads, BulletinThread{
			ID: uuid.NewString(), Title: "House thread", CompanyName: fmt.Sprintf("House %04d", index),
			CreatedAt: time.Now().UTC().Add(-time.Hour), LastPostAt: time.Now().UTC().Add(-time.Hour),
		})
	}
	if err := db.SaveBulletinThreads(ctx, "bulletin-company-cap-owner", threads); err != nil {
		t.Fatal(err)
	}
	board, err := db.BulletinBoard(ctx, BulletinQuery{})
	if err != nil {
		t.Fatal(err)
	}
	if len(board.Companies) != 1000 || !board.CompaniesTruncated {
		t.Fatalf("the company list did not cap at one thousand sorted names: %d truncated %t",
			len(board.Companies), board.CompaniesTruncated)
	}
	if board.Total != 1001 {
		t.Fatalf("the company cap changed thread totals: %d", board.Total)
	}
}

func TestBulletinSearchEscapesLikeWildcards(t *testing.T) {
	db, _, _ := integrationStore(t)
	ctx := context.Background()
	owner := "bulletin-escape-owner"
	saveTestAccount(t, db, owner)
	base := time.Now().UTC().Truncate(time.Microsecond)
	seed := func(body string) string {
		t.Helper()
		id := uuid.NewString()
		if err := db.SaveBulletinThreadDetail(ctx, owner, BulletinThread{
			ID: id, Title: "Wildcards", CompanyName: "Salt Oval",
			CreatedAt: base.Add(-time.Hour), LastPostAt: base.Add(-30 * time.Minute),
		}, []BulletinPost{
			{ID: uuid.NewString(), ThreadID: id, CompanyName: "Salt Oval", Body: body, IsOpening: true, CreatedAt: base.Add(-30 * time.Minute)},
		}); err != nil {
			t.Fatal(err)
		}
		return id
	}
	percent := seed("Rye prices at 100% sure")
	plainPercent := seed("Rye prices at 100 quite sure")
	underscore := seed("a_b slot")
	plainUnderscore := seed("axb slot")
	backslash := seed(`cost\cut`)
	plainBackslash := seed("costcut")
	for _, test := range []struct {
		query string
		want  string
	}{
		{"% sure", percent},
		{"a_b", underscore},
		{`cost\cut`, backslash},
	} {
		board, err := db.BulletinBoard(ctx, BulletinQuery{Search: test.query})
		if err != nil {
			t.Fatal(err)
		}
		if board.Total != 1 || len(board.Threads) != 1 || board.Threads[0].ID != test.want {
			t.Fatalf("escaped search %q matched %v, want only %s", test.query, board.Threads, test.want)
		}
	}
	// The plain neighbours exist so the escaped queries above really had to
	// distinguish literal wildcards from pattern wildcards.
	if plainPercent == "" || plainUnderscore == "" || plainBackslash == "" {
		t.Fatal("escaped search fixtures missing")
	}
}

func TestBulletinTextCleansControlAndTruncatesOnRuneBoundary(t *testing.T) {
	db, _, _ := integrationStore(t)
	ctx := context.Background()
	saveTestAccount(t, db, "bulletin-text-owner")
	base := time.Now().UTC().Truncate(time.Microsecond)
	thread := uuid.NewString()
	want := strings.Repeat("é", 4000) + strings.Repeat("b", 4000)
	sanitized := "startmidend\ttab\nline" + want + "€€"
	wantBody := string([]rune(sanitized)[:8000])
	if err := db.SaveBulletinThreadDetail(ctx, "bulletin-text-owner", BulletinThread{
		ID: thread, Title: strings.Repeat("t", 302), CompanyName: strings.Repeat("c", 130),
		CreatedAt: base.Add(-time.Hour), LastPostAt: base.Add(-time.Hour),
	}, []BulletinPost{
		{ID: uuid.NewString(), ThreadID: thread, CompanyName: "Salt Oval", Body: "start\x00mid\x01end\ttab\nline" + want + "€€", IsOpening: true, CreatedAt: base.Add(-time.Hour)},
	}); err != nil {
		t.Fatal(err)
	}
	view, err := db.BulletinThread(ctx, thread)
	if err != nil {
		t.Fatal(err)
	}
	if len(view.Thread.Title) != 300 || len(view.Thread.CompanyName) != 120 {
		t.Fatalf("title or company text ignored its bounds: %d %d", len(view.Thread.Title), len(view.Thread.CompanyName))
	}
	if view.Posts[0].Body != wantBody || len([]rune(view.Posts[0].Body)) != 8000 {
		t.Fatalf("control characters survived or the whole body missed the 8000 rune bound: %d runes", len([]rune(view.Posts[0].Body)))
	}
}

func TestBulletinThreadReaderRejectsUnknownAndMalformedIDs(t *testing.T) {
	db, _, _ := integrationStore(t)
	ctx := context.Background()
	if _, err := db.BulletinThread(ctx, uuid.NewString()); !errors.Is(err, ErrNotFound) {
		t.Fatalf("an unknown thread uuid did not return ErrNotFound: %v", err)
	}
	if _, err := db.BulletinThread(ctx, "not-a-uuid"); !errors.Is(err, ErrInvalidInput) {
		t.Fatalf("a non uuid thread id did not return ErrInvalidInput: %v", err)
	}
}

func TestBulletinLinkSanitizesInvalidJSONBeforeStorage(t *testing.T) {
	db, _, _ := integrationStore(t)
	ctx := context.Background()
	saveTestAccount(t, db, "bulletin-link-owner")
	base := time.Now().UTC().Truncate(time.Microsecond)
	for _, test := range []struct {
		name    string
		link    json.RawMessage
		wantNil bool
	}{
		{"decoded nul", json.RawMessage("{\"name\":\"bad\u0000name\"}"), true},
		{"nul escape", json.RawMessage(`{"name":"bad` + `\` + `u0000name"}`), true},
		{"lone high escape", json.RawMessage("{\"name\":\"\\ud800\"}"), true},
		{"lone low escape", json.RawMessage("{\"name\":\"\\udc00\"}"), true},
		{"invalid utf8 byte", json.RawMessage("{\"name\":\"" + "\xff" + "\"}"), true},
		{"valid surrogate pair", json.RawMessage("{\"name\":\"\\ud83d\\ude80\"}"), false},
		{"literal escaped backslash", json.RawMessage(`{"name":"\\ud800"}`), false},
	} {
		t.Run(test.name, func(t *testing.T) {
			thread := uuid.NewString()
			if err := db.SaveBulletinThreadDetail(ctx, "bulletin-link-owner", BulletinThread{
				ID: thread, Title: "Linked thread", CompanyName: "Salt Oval", Link: test.link,
				CreatedAt: base.Add(-time.Hour), LastPostAt: base.Add(-time.Hour),
			}, []BulletinPost{
				{ID: uuid.NewString(), ThreadID: thread, CompanyName: "Salt Oval", Body: "Prices attached.", IsOpening: true, CreatedAt: base.Add(-time.Hour)},
			}); err != nil {
				t.Fatal(err)
			}
			view, err := db.BulletinThread(ctx, thread)
			if err != nil {
				t.Fatal(err)
			}
			if test.wantNil && view.Thread.Link != nil {
				t.Fatalf("invalid link content survived storage: %s", string(view.Thread.Link))
			}
			if !test.wantNil && (view.Thread.Link == nil || !json.Valid(view.Thread.Link)) {
				t.Fatalf("a valid link was dropped: %s", string(view.Thread.Link))
			}
		})
	}
}

func TestPruneKeepsBulletinRowsAndClearsOldDiagnostics(t *testing.T) {
	db, _, _ := integrationStore(t)
	ctx := context.Background()
	saveTestAccount(t, db, "bulletin-prune-owner")
	base := time.Now().UTC().Truncate(time.Microsecond)
	thread := uuid.NewString()
	if err := db.SaveBulletinThreadDetail(ctx, "bulletin-prune-owner", BulletinThread{
		ID: thread, Title: "Still here", CompanyName: "Salt Oval",
		CreatedAt: base.Add(-2 * time.Hour), LastPostAt: base.Add(-time.Hour), Replies: 1,
	}, []BulletinPost{
		{ID: uuid.NewString(), ThreadID: thread, CompanyName: "Salt Oval", Body: "Still buying rye.", IsOpening: true, CreatedAt: base.Add(-time.Hour)},
	}); err != nil {
		t.Fatal(err)
	}
	for _, statement := range []string{
		"INSERT INTO requests (account_id, method, path, status, duration_ms, request, response, error, created_at) " +
			"VALUES ('bulletin-prune-owner', 'GET', '/v1/bulletin', 200, 1, '{}', '{}', '', now() - interval '8 days')",
		"INSERT INTO events (account_id, event_id, event_type, payload, created_at) " +
			"VALUES ('bulletin-prune-owner', 'bulletin-prune-old-event', 'bulletin_posted', '{}', now() - interval '8 days')",
		"INSERT INTO decisions (account_id, ship_id, kind, reason, details, created_at) " +
			"VALUES ('bulletin-prune-owner', '__bulletin_mirror', 'bulletin_mirror', 'old outage', '{}', now() - interval '8 days')",
	} {
		if err := db.db.WithContext(ctx).Exec(statement).Error; err != nil {
			t.Fatal(err)
		}
	}
	if err := db.Prune(ctx); err != nil {
		t.Fatal(err)
	}
	for _, table := range []string{"requests", "events", "decisions"} {
		var remaining int64
		if err := db.db.WithContext(ctx).Raw("SELECT count(*) FROM " + table).Scan(&remaining).Error; err != nil {
			t.Fatal(err)
		}
		if remaining != 0 {
			t.Fatalf("the prune left %d rows in %s", remaining, table)
		}
	}
	view, err := db.BulletinThread(ctx, thread)
	if err != nil {
		t.Fatal(err)
	}
	if view.Thread.Title != "Still here" || len(view.Posts) != 1 || view.Posts[0].Body != "Still buying rye." {
		t.Fatalf("the prune deleted permanent bulletin rows: %+v", view)
	}
}
