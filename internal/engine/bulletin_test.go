package engine

import (
	"bytes"
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"slices"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"
	"unicode"

	"github.com/DevYukine/go-tradewinds/internal/gameapi"
	"github.com/DevYukine/go-tradewinds/internal/store"
	"github.com/google/uuid"
)

type bulletinTestAPI struct {
	mu            sync.Mutex
	h             *recoveryHarness
	downstream    http.Handler
	threads       []bulletinThread
	details       map[string]bulletinDetail
	posted        map[string]bulletinPost
	bodies        map[string][]byte
	boardReads    int
	boardSince    time.Time
	boardCursors  []string
	postCalls     int
	readStatus    int
	postStatus    int
	problemCode   string
	problemDetail string
	retryAfter    string
	nextCursor    string
	badReceipt    bool
	dropReceipt   bool
	hideOwn       bool
}

func newBulletinFixture(t *testing.T) (*recoveryHarness, *bulletinTestAPI, *Runner) {
	t.Helper()
	h := newRecoveryHarness(t)
	proxy := &bulletinTestAPI{h: h, downstream: h.api.server.Config.Handler, threads: []bulletinThread{}, details: make(map[string]bulletinDetail), posted: make(map[string]bulletinPost), bodies: make(map[string][]byte)}
	h.api.server.Config.Handler = proxy
	runner := h.runner(primaryTestKey)
	runner.opts.BulletinEnabled = true
	refreshBulletinRunner(t, h, runner)
	return h, proxy, runner
}

func refreshBulletinRunner(t *testing.T, h *recoveryHarness, runner *Runner) {
	t.Helper()
	runner.generation = uuid.NewString()
	if err := runner.refresh(h.ctx); err != nil {
		t.Fatal(err)
	}
}

func (p *bulletinTestAPI) ServeHTTP(writer http.ResponseWriter, request *http.Request) {
	if request.URL.Path != "/v1/bulletin" && !bulletinPath(request.URL.Path) {
		p.downstream.ServeHTTP(writer, request)
		return
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	if request.Method == http.MethodGet {
		if p.readStatus != 0 {
			p.h.api.writeProblem(writer, p.readStatus, "unavailable", "public board unavailable")
			return
		}
		if request.URL.Path == "/v1/bulletin" {
			p.boardReads++
			threads := make([]bulletinThread, 0, len(p.threads))
			p.boardSince = time.Time{}
			if since := request.URL.Query().Get("since"); since != "" {
				var err error
				p.boardSince, err = time.Parse(time.RFC3339Nano, since)
				if err != nil {
					p.h.t.Errorf("invalid bulletin since filter: %v", err)
				}
			}
			for _, thread := range p.threads {
				if (!p.hideOwn || thread.CompanyName != "The Pokemon Pirates") && !thread.LastPostAt.Before(p.boardSince) {
					threads = append(threads, thread)
				}
			}
			slices.SortFunc(threads, func(left, right bulletinThread) int { return right.LastPostAt.Compare(left.LastPostAt) })
			offset := 0
			if cursor := request.URL.Query().Get("cursor"); cursor != "" {
				var err error
				offset, err = strconv.Atoi(cursor)
				if err != nil || offset < 0 || offset > len(threads) {
					p.h.t.Errorf("invalid bulletin cursor %q", cursor)
					p.h.api.writeProblem(writer, http.StatusBadRequest, "invalid_cursor", "invalid fixture cursor")
					return
				}
			}
			p.boardCursors = append(p.boardCursors, request.URL.Query().Get("cursor"))
			threads = threads[offset:]
			cursor := p.nextCursor
			if len(threads) > bulletinPageLimit {
				threads, cursor = threads[:bulletinPageLimit], strconv.Itoa(offset+bulletinPageLimit)
			}
			p.h.api.writeJSON(writer, http.StatusOK, bulletinPage{Threads: threads, NextCursor: cursor})
			return
		}
		detail, exists := p.details[request.URL.Query().Get("id")]
		if !exists {
			p.h.api.writeProblem(writer, http.StatusNotFound, "not_found", "no thread")
			return
		}
		if p.hideOwn {
			detail.Posts = slices.DeleteFunc(slices.Clone(detail.Posts), func(post bulletinPost) bool { return post.CompanyName == "The Pokemon Pirates" })
		}
		p.h.api.writeJSON(writer, http.StatusOK, detail)
		return
	}
	body, err := io.ReadAll(request.Body)
	if err != nil {
		p.h.t.Errorf("read bulletin mutation: %v", err)
		writer.WriteHeader(http.StatusBadRequest)
		return
	}
	p.postCalls++
	key := request.Header.Get("Idempotency-Key")
	pending, err := p.h.db.PendingActions(request.Context(), p.h.account.ID)
	if err != nil {
		p.h.t.Error(err)
	}
	journaled := false
	for _, action := range pending {
		journaled = journaled || action.ID == key && action.Path == request.URL.Path && bytes.Equal(action.Body, body)
	}
	if key == "" || !journaled {
		p.h.t.Error("bulletin mutation reached the server before its immutable request and key were journaled")
	}
	if prior, exists := p.bodies[key]; exists && !bytes.Equal(prior, body) {
		p.h.t.Error("bulletin retry changed its immutable payload")
	}
	p.bodies[key] = slices.Clone(body)
	if p.postStatus != 0 {
		if p.retryAfter != "" {
			writer.Header().Set("Retry-After", p.retryAfter)
		}
		p.h.api.writeProblem(writer, p.postStatus, p.problemCode, p.problemDetail)
		return
	}
	var payload bulletinPayload
	if err := json.Unmarshal(body, &payload); err != nil {
		p.h.t.Error(err)
		writer.WriteHeader(http.StatusBadRequest)
		return
	}
	post, exists := p.posted[key]
	if !exists {
		now := time.Now().UTC()
		threadID := payload.ThreadID
		opening := request.URL.Path == "/v1/bulletin/thread"
		if opening {
			threadID = uuid.NewString()
			thread := bulletinThread{ID: threadID, Title: payload.Title, CompanyName: "The Pokemon Pirates", CreatedAt: now, LastPostAt: now}
			p.threads = append(p.threads, thread)
			p.details[threadID] = bulletinDetail{Thread: thread, Posts: []bulletinPost{}}
		}
		post = bulletinPost{ID: uuid.NewString(), ThreadID: threadID, CompanyName: "The Pokemon Pirates", Body: payload.Body, Opening: opening, CreatedAt: now}
		p.posted[key] = post
		detail := p.details[threadID]
		detail.Posts = append(detail.Posts, post)
		p.details[threadID] = detail
	}
	if p.dropReceipt {
		connection, _, err := writer.(http.Hijacker).Hijack()
		if err != nil {
			p.h.t.Error(err)
		} else {
			_ = connection.Close()
		}
		return
	}
	if p.badReceipt {
		writer.WriteHeader(http.StatusCreated)
		_, _ = writer.Write([]byte("unreadable committed bulletin receipt"))
		return
	}
	if post.Opening {
		p.h.api.writeJSON(writer, http.StatusCreated, p.details[post.ThreadID])
	} else {
		p.h.api.writeJSON(writer, http.StatusCreated, post)
	}
}

func (p *bulletinTestAPI) addThread(company string, age time.Duration, archived, removed bool) string {
	p.mu.Lock()
	defer p.mu.Unlock()
	id := uuid.NewString()
	at := time.Now().UTC().Add(-age)
	thread := bulletinThread{ID: id, Title: "A public harbour conversation", CompanyName: company, CreatedAt: at, LastPostAt: at, Archived: archived, Removed: removed}
	p.threads = append(p.threads, thread)
	p.details[id] = bulletinDetail{Thread: thread, Posts: []bulletinPost{{ID: uuid.NewString(), ThreadID: id, CompanyName: company, Body: "Fair winds", Opening: true, CreatedAt: at}}}
	return id
}

func stageBulletinFixture(t *testing.T, h *recoveryHarness, runner *Runner) (*store.Mission, missionData) {
	t.Helper()
	data := missionData{}
	if changed, err := runner.driveBulletin(h.ctx, nil, &data); err != nil || !changed {
		t.Fatalf("stage bulletin: changed=%t error=%v", changed, err)
	}
	mission, restored := h.mission("__bulletin")
	if restored.Action == nil || !bulletinStage(restored.Action.Stage) || restored.Bulletin == nil {
		t.Fatalf("public post was not durably staged: %+v", restored)
	}
	return mission, restored
}

func sendBulletinFixture(t *testing.T, h *recoveryHarness, runner *Runner, mission *store.Mission, data *missionData) {
	t.Helper()
	if changed, err := runner.driveBulletin(h.ctx, mission, data); err != nil || !changed || data.Action != nil {
		t.Fatalf("consume bulletin: changed=%t error=%v data=%+v", changed, err, data)
	}
	if changed, err := runner.driveBulletin(h.ctx, mission, data); err != nil || !changed {
		t.Fatalf("retire bulletin mission: changed=%t error=%v", changed, err)
	}
}

func makeBulletinDue(t *testing.T, h *recoveryHarness, runner *Runner) bulletinState {
	t.Helper()
	state, err := runner.bulletinState()
	if err != nil {
		t.Fatal(err)
	}
	state.LastPostAt = time.Now().UTC().Add(-4 * time.Hour)
	state.NextDue = time.Now().UTC().Add(-time.Hour)
	state.CooldownUntil = time.Time{}
	if err := runner.saveBulletinState(h.ctx, state); err != nil {
		t.Fatal(err)
	}
	return state
}

func TestBulletinScheduleAndAlternation(t *testing.T) {
	h, proxy, runner := newBulletinFixture(t)
	old := proxy.addThread("The Kraken Cartel", 2*time.Hour, false, false)
	latest := proxy.addThread("The Kraken Cartel", time.Hour, false, false)
	proxy.addThread("The Kraken Cartel", time.Minute, true, false)
	proxy.addThread("The Kraken Cartel", time.Second, false, true)
	proxy.addThread("Friendly rivals", 0, false, false)
	mission, data := stageBulletinFixture(t, h, runner)
	if data.Action.Stage != "bulletin-thread" || proxy.boardReads != 1 {
		t.Fatalf("first slot was not a thread with one board read: %s, %d", data.Action.Stage, proxy.boardReads)
	}
	sendBulletinFixture(t, h, runner, mission, &data)
	state, err := runner.bulletinState()
	if err != nil || state.LastPostID == "" || state.LastPostAt.IsZero() || !state.NextReply || state.ThreadCursor != 1 || state.NextDue.Before(state.LastPostAt.Add(3*time.Hour)) {
		t.Fatalf("successful thread lost its schedule: %+v, %v", state, err)
	}
	reads := proxy.boardReads
	for _, until := range []time.Time{time.Now().Add(time.Hour), time.Now().Add(3 * time.Hour)} {
		state.NextDue = until
		if err := runner.saveBulletinState(h.ctx, state); err != nil {
			t.Fatal(err)
		}
		if changed, err := runner.driveBulletin(h.ctx, nil, &missionData{}); err != nil || changed || proxy.boardReads != reads {
			t.Fatalf("not-due schedule read or posted: %t %v reads=%d", changed, err, proxy.boardReads)
		}
	}
	makeBulletinDue(t, h, runner)
	mission, data = stageBulletinFixture(t, h, runner)
	if data.Action.Stage != "bulletin-reply" || data.Bulletin.ThreadID != latest || proxy.boardReads != reads+1 {
		t.Fatalf("reply did not select latest eligible ally thread: %+v", data.Bulletin)
	}
	sendBulletinFixture(t, h, runner, mission, &data)
	state, _ = runner.bulletinState()
	if state.NextReply || state.ReplyCursor != 1 || !slices.Contains(state.RepliedThreadIDs, latest) {
		t.Fatalf("reply did not advance the separate pool and thread history: %+v", state)
	}
	makeBulletinDue(t, h, runner)
	mission, data = stageBulletinFixture(t, h, runner)
	if data.Action.Stage != "bulletin-thread" || data.Bulletin.TemplateIndex != 1 {
		t.Fatalf("third slot did not return to a new thread template: %+v", data)
	}
	sendBulletinFixture(t, h, runner, mission, &data)
	makeBulletinDue(t, h, runner)
	mission, data = stageBulletinFixture(t, h, runner)
	if data.Bulletin.ThreadID != old {
		t.Fatalf("already-replied ally thread repeated: got %s want %s", data.Bulletin.ThreadID, old)
	}
}

func TestBulletinReplyFallbackAndPriorCompanyReply(t *testing.T) {
	for _, priorReply := range []bool{false, true} {
		t.Run(fmt.Sprint(priorReply), func(t *testing.T) {
			h, proxy, runner := newBulletinFixture(t)
			state := bulletinState{NextReply: true}
			if priorReply {
				id := proxy.addThread("The Kraken Cartel", time.Hour, false, false)
				proxy.mu.Lock()
				detail := proxy.details[id]
				detail.Posts = append(detail.Posts, bulletinPost{ID: uuid.NewString(), ThreadID: id, CompanyName: "The Pokemon Pirates", Body: "Earlier company reply", CreatedAt: time.Now().Add(-time.Minute)})
				proxy.details[id] = detail
				proxy.mu.Unlock()
			}
			if err := runner.saveBulletinState(h.ctx, state); err != nil {
				t.Fatal(err)
			}
			mission, data := stageBulletinFixture(t, h, runner)
			if data.Action.Stage != "bulletin-thread" || !data.Bulletin.ReplyTurn || proxy.boardReads != 1 {
				t.Fatalf("no unreplied ally did not fall back to a thread: %+v", data)
			}
			sendBulletinFixture(t, h, runner, mission, &data)
			state, _ = runner.bulletinState()
			if state.NextReply || state.ThreadCursor != 1 || state.ReplyCursor != 0 {
				t.Fatalf("fallback did not advance the reply slot using only the thread pool: %+v", state)
			}
		})
	}
}

func TestBulletinCooldownResultsSurviveRestart(t *testing.T) {
	mutedUntil := time.Now().UTC().Add(7 * time.Hour).Truncate(time.Second)
	for _, test := range []struct {
		name   string
		status int
		code   string
		detail string
		header string
		wait   time.Duration
		until  time.Time
	}{
		{"hourly limit", 429, "bulletin_limit", "Please wait 7200 seconds before posting", "3600", 2 * time.Hour, time.Time{}},
		{"hourly fallback", 429, "bulletin_limit", "hourly limit", "", time.Hour, time.Time{}},
		{"hourly detail without header", 429, "bulletin_limit", "Please wait 7200 seconds", "", 2 * time.Hour, time.Time{}},
		{"short detail without header", 429, "bulletin_limit", "Please wait 10 minutes", "", 10 * time.Minute, time.Time{}},
		{"later header wins", 429, "bulletin_limit", "Please wait 10 minutes", "1200", 20 * time.Minute, time.Time{}},
		{"muted until", 403, "posting_muted", "muted until " + mutedUntil.Format(time.RFC3339), "", 0, mutedUntil},
		{"mute fallback", 403, "posting_muted", "company muted", "", 24 * time.Hour, time.Time{}},
		{"mute header without detail time", 403, "posting_muted", "company muted", "1200", 20 * time.Minute, time.Time{}},
		{"mute later header wins", 403, "posting_muted", "muted until " + mutedUntil.Format(time.RFC3339), "28800", 8 * time.Hour, time.Time{}},
		{"other refusal", 409, "thread_archived", "closed", "", 6 * time.Hour, time.Time{}},
	} {
		t.Run(test.name, func(t *testing.T) {
			h, proxy, runner := newBulletinFixture(t)
			mission, data := stageBulletinFixture(t, h, runner)
			proxy.postStatus, proxy.problemCode, proxy.problemDetail, proxy.retryAfter = test.status, test.code, test.detail, test.header
			before := time.Now().UTC()
			sendBulletinFixture(t, h, runner, mission, &data)
			state, err := runner.bulletinState()
			minimum := before.Add(test.wait - time.Second)
			maximum := time.Now().UTC().Add(test.wait + time.Second)
			if !test.until.IsZero() {
				minimum, maximum = test.until, test.until
			}
			if err != nil || state.CooldownUntil.Before(minimum) || state.CooldownUntil.After(maximum) || state.ThreadCursor != 0 || !state.LastPostAt.IsZero() {
				t.Fatalf("refusal cooldown or unposted pool changed: %+v error=%v", state, err)
			}
			restarted := h.runner(primaryTestKey)
			restarted.opts.BulletinEnabled = true
			refreshBulletinRunner(t, h, restarted)
			reads := proxy.boardReads
			if changed, err := restarted.driveBulletin(h.ctx, nil, &missionData{}); err != nil || changed || proxy.postCalls != 1 || proxy.boardReads != reads {
				t.Fatalf("restart ignored posting cooldown: changed=%t error=%v posts=%d", changed, err, proxy.postCalls)
			}
		})
	}
}

func TestBulletinCooldownParsing(t *testing.T) {
	now := time.Date(2026, 10, 31, 12, 0, 0, 0, time.UTC)
	for _, test := range []struct {
		detail string
		wait   time.Duration
	}{
		{"retry in 1h30m", 90 * time.Minute},
		{"retry in 1 hour 30 minutes", 90 * time.Minute},
		{"wait 45 seconds", 45 * time.Second},
		{"muted until 2026-10-31T14:00:00Z", 2 * time.Hour},
		{"muted until 2026-10-31 14:00:00 UTC", 2 * time.Hour},
	} {
		if got := gameapi.BulletinDetailDeadline(test.detail, now); !got.Equal(now.Add(test.wait)) {
			t.Errorf("%q: got %v want %v", test.detail, got, now.Add(test.wait))
		}
	}
	if got := bulletinCooldown(429, "bulletin_limit", "unreadable duration", time.Time{}, now); !got.Equal(now.Add(time.Hour)) {
		t.Fatalf("429 fallback=%v", got)
	}
}

func TestBulletinRestartPreservesStagedRequest(t *testing.T) {
	h, proxy, runner := newBulletinFixture(t)
	_, data := stageBulletinFixture(t, h, runner)
	key, payload := data.Action.ID, slices.Clone(data.Action.Body)
	restarted := h.runner(primaryTestKey)
	restarted.opts.BulletinEnabled = true
	refreshBulletinRunner(t, h, restarted)
	mission, data := h.mission("__bulletin")
	if changed, err := restarted.driveBulletin(h.ctx, mission, &data); err != nil || !changed {
		t.Fatalf("refresh never-sent action authority: %t %v", changed, err)
	}
	mission, data = h.mission("__bulletin")
	if data.Action == nil || data.Action.ID != key || !bytes.Equal(data.Action.Body, payload) || proxy.postCalls != 0 || proxy.boardReads != 1 {
		t.Fatal("restart regenerated a public post, key, payload or board read before first send")
	}
	sendBulletinFixture(t, h, restarted, mission, &data)
	if proxy.postCalls != 1 || !bytes.Equal(proxy.bodies[key], payload) {
		t.Fatal("restaged request did not send the original immutable bytes exactly once")
	}
}

func TestBulletinAmbiguousCommitReconcilesBeforeNewPost(t *testing.T) {
	h, proxy, runner := newBulletinFixture(t)
	mission, data := stageBulletinFixture(t, h, runner)
	key := data.Action.ID
	proxy.badReceipt, proxy.hideOwn = true, true
	if changed, err := runner.driveBulletin(h.ctx, mission, &data); err != nil || changed || data.Action == nil {
		t.Fatalf("uninterpretable committed post was not retained: changed=%t error=%v", changed, err)
	}
	if proxy.postCalls != 1 || proxy.boardReads != 1 {
		t.Fatalf("ambiguous attempt made extra writes or board reads: posts=%d reads=%d", proxy.postCalls, proxy.boardReads)
	}
	restarted := h.runner(primaryTestKey)
	restarted.opts.BulletinEnabled = true
	refreshBulletinRunner(t, h, restarted)
	mission, data = h.mission("__bulletin")
	if data.Action.ID != key {
		t.Fatal("restart lost the unresolved post identity")
	}
	data.CooldownUntil = time.Now().Add(-time.Second)
	if err := restarted.saveMission(h.ctx, mission, &data); err != nil {
		t.Fatal(err)
	}
	cooldown, err := restarted.bulletinState()
	if err != nil || !cooldown.CooldownUntil.After(time.Now()) {
		t.Fatalf("ambiguous posting cooldown did not persist in its snapshot: %+v %v", cooldown, err)
	}
	cooldown.CooldownUntil = time.Time{}
	if err := restarted.saveBulletinState(h.ctx, cooldown); err != nil {
		t.Fatal(err)
	}
	proxy.hideOwn = false
	sendBulletinFixture(t, h, restarted, mission, &data)
	state, _ := restarted.bulletinState()
	if proxy.postCalls != 1 || state.LastPostID != proxy.posted[key].ID || data.LastActionID != key || state.ThreadCursor != 1 {
		t.Fatalf("reconciliation duplicated the post or lost its receipt: posts=%d state=%+v", proxy.postCalls, state)
	}
}

func TestBulletinPendingTransportCommitReconcilesWithoutResend(t *testing.T) {
	h, proxy, runner := newBulletinFixture(t)
	proxy.dropReceipt = true
	mission, data := stageBulletinFixture(t, h, runner)
	key, payload := data.Action.ID, slices.Clone(data.Action.Body)
	// A fresh connection excludes net/http's stale-connection retries from this lost-receipt scenario.
	h.api.server.CloseClientConnections()
	if changed, err := runner.driveBulletin(h.ctx, mission, &data); err != nil || changed || data.Action == nil {
		t.Fatalf("lost receipt was not held as pending: changed=%t error=%v", changed, err)
	}
	pending, err := h.db.PendingActions(h.ctx, h.account.ID)
	if err != nil || len(pending) != 1 || pending[0].ID != key || proxy.postCalls != 1 || proxy.boardReads != 1 {
		t.Fatalf("transport ambiguity lost its pending request or repeated I/O: pending=%+v posts=%d reads=%d error=%v", pending, proxy.postCalls, proxy.boardReads, err)
	}
	proxy.dropReceipt = false
	restarted := h.runner(primaryTestKey)
	restarted.opts.BulletinEnabled = true
	refreshBulletinRunner(t, h, restarted)
	mission, data = h.mission("__bulletin")
	if data.Action == nil || data.Action.ID != key || !bytes.Equal(data.Action.Body, payload) {
		t.Fatal("restart lost the pending request's immutable key or payload")
	}
	data.CooldownUntil = time.Now().Add(-time.Second)
	if err := restarted.saveMission(h.ctx, mission, &data); err != nil {
		t.Fatal(err)
	}
	state, err := restarted.bulletinState()
	if err != nil {
		t.Fatal(err)
	}
	state.CooldownUntil = time.Time{}
	if err := restarted.saveBulletinState(h.ctx, state); err != nil {
		t.Fatal(err)
	}
	sendBulletinFixture(t, h, restarted, mission, &data)
	pending, err = h.db.PendingActions(h.ctx, h.account.ID)
	state, stateErr := restarted.bulletinState()
	if err != nil || stateErr != nil || len(pending) != 0 || proxy.postCalls != 1 || proxy.boardReads != 2 || state.LastPostID != proxy.posted[key].ID || state.ThreadCursor != 1 || data.LastActionID != key {
		t.Fatalf("GET reconciliation resent or failed to settle the pending post: pending=%+v posts=%d reads=%d state=%+v errors=%v,%v", pending, proxy.postCalls, proxy.boardReads, state, err, stateErr)
	}
}

func TestBulletinReconciledJournalSurvivesMissionWriteRestart(t *testing.T) {
	h, proxy, runner := newBulletinFixture(t)
	mission, data := stageBulletinFixture(t, h, runner)
	original := data
	if _, err := h.db.BeginAction(h.ctx, &store.Action{ID: data.Action.ID, AccountID: h.account.ID, Method: data.Action.Method, Path: data.Action.Path, Body: data.Action.Body}); err != nil {
		t.Fatal(err)
	}
	threadID := uuid.NewString()
	post := bulletinPost{ID: uuid.NewString(), ThreadID: threadID, CompanyName: data.Bulletin.CompanyName, Body: data.Bulletin.Body, Opening: true, CreatedAt: time.Now().UTC()}
	if err := h.db.ReconcileAction(h.ctx, data.Action.ID, encode(map[string]any{"kind": "authoritative_bulletin_post", "post": post})); err != nil {
		t.Fatal(err)
	}
	if err := runner.saveMission(h.ctx, mission, &original); err != nil {
		t.Fatal(err)
	}
	restarted := h.runner(primaryTestKey)
	restarted.opts.BulletinEnabled = true
	refreshBulletinRunner(t, h, restarted)
	mission, data = h.mission("__bulletin")
	sendBulletinFixture(t, h, restarted, mission, &data)
	state, _ := restarted.bulletinState()
	if state.LastPostID != post.ID || proxy.postCalls != 0 || proxy.boardReads != 1 {
		t.Fatal("reconciled journal recovery sent or observed an unnecessary replacement post")
	}
}

func TestBulletinPendingAndOrphanRequestsNeverBlockTrading(t *testing.T) {
	for _, orphan := range []bool{false, true} {
		t.Run(fmt.Sprint(orphan), func(t *testing.T) {
			h, proxy, runner := newBulletinFixture(t)
			row := sailingShipRow(recoveryShipBlastoise, "Blastoise", recoveryHomePort, recoveryFarPort)
			row["ship"].(map[string]any)["status"] = "berthed"
			row["voyage"], row["route_legs"] = nil, []any{}
			h.api.setFleet([]map[string]any{row})
			refreshBulletinRunner(t, h, runner)
			mission, data := stageBulletinFixture(t, h, runner)
			key := data.Action.ID
			if _, err := h.db.BeginAction(h.ctx, &store.Action{ID: key, AccountID: h.account.ID, Method: data.Action.Method, Path: data.Action.Path, Body: data.Action.Body}); err != nil {
				t.Fatal(err)
			}
			data.CooldownUntil = time.Now().Add(time.Hour)
			if err := runner.saveMission(h.ctx, mission, &data); err != nil {
				t.Fatal(err)
			}
			if orphan {
				if err := h.db.DeleteMission(h.ctx, h.account.ID, "__bulletin"); err != nil {
					t.Fatal(err)
				}
			}
			reads := proxy.boardReads
			proxy.readStatus = http.StatusInternalServerError
			if changed, err := runner.work(h.ctx); err != nil || !changed {
				t.Fatalf("pending bulletin blocked unrelated maintenance: changed=%t error=%v", changed, err)
			}
			_, shipData := h.mission(recoveryShipBlastoise)
			if shipData.Action == nil || shipData.Action.Stage != "refit" || proxy.boardReads != reads || proxy.postCalls != 0 {
				t.Fatalf("public posting preempted maintenance: %+v reads=%d", shipData, proxy.boardReads)
			}
			if changed, err := runner.work(h.ctx); err != nil || !changed || h.api.mutationCount("/v1/shipyard/modify-ship") != 1 {
				t.Fatalf("pending bulletin blocked maintenance dispatch: changed=%t error=%v", changed, err)
			}
			if !orphan {
				_, restored := h.mission("__bulletin")
				if restored.Action == nil || restored.Action.ID != key {
					t.Fatal("unrelated trading replaced the unresolved post")
				}
			}
		})
	}
}

func TestBulletinTradingPendingFilterKeepsMoneyActions(t *testing.T) {
	posts := []store.Action{{Path: "/v1/bulletin/thread"}, {Path: "/v1/bulletin/reply"}}
	if tradingActionsPending(posts) {
		t.Fatal("zero-cost public posts block growth or prepayment")
	}
	posts = append(posts, store.Action{Path: "/v1/treasury/remit"})
	if !tradingActionsPending(posts) {
		t.Fatal("public-post exemption discarded a genuine unresolved money action")
	}
}

func TestBulletinDisabledDoesNotReadOrSend(t *testing.T) {
	h, proxy, runner := newBulletinFixture(t)
	runner.opts.BulletinEnabled = false
	if changed, err := runner.driveBulletin(h.ctx, nil, &missionData{}); err != nil || changed || proxy.boardReads != 0 || proxy.postCalls != 0 {
		t.Fatalf("disabled activity read or posted: %t %v", changed, err)
	}
	runner.opts.BulletinEnabled = true
	mission, data := stageBulletinFixture(t, h, runner)
	runner.opts.BulletinEnabled = false
	if _, err := runner.driveBulletin(h.ctx, mission, &data); err != nil || proxy.postCalls != 0 {
		t.Fatalf("disabled staged activity posted: %v", err)
	}
}

func TestBulletinContentPoolsAndBounds(t *testing.T) {
	if len(bulletinThreads) < 24 || len(bulletinReplies) < 24 {
		t.Fatalf("too few curated templates: threads=%d replies=%d", len(bulletinThreads), len(bulletinReplies))
	}
	seen := make(map[string]bool)
	for _, template := range bulletinThreads {
		body := template.Body
		if err := validBulletinText(template.Title, body); err != nil || template.Title == "" || seen[template.Body] || strings.Contains(body, "{log}") {
			t.Fatalf("invalid or repeated thread template %q: %v", template.Title, err)
		}
		seen[template.Body] = true
		for _, character := range template.Title + body {
			if unicode.IsSymbol(character) || character == '\u2013' || character == '\u2014' {
				t.Fatalf("non-plain public thread contains %q", character)
			}
		}
	}
	clear(seen)
	for _, template := range bulletinReplies {
		body := template
		if err := validBulletinText("", body); err != nil || seen[template] {
			t.Fatalf("invalid or repeated reply template: %v", err)
		}
		seen[template] = true
		for _, character := range body {
			if unicode.IsSymbol(character) || character == '\u2013' || character == '\u2014' {
				t.Fatalf("non-plain public reply contains %q", character)
			}
		}
	}
	for _, test := range []struct{ title, body string }{{strings.Repeat("t", 121), "body"}, {"title", strings.Repeat("b", 1001)}, {"title", "bad\u2014dash"}, {"title", "bad\u2013dash"}, {"title", ""}} {
		if validBulletinText(test.title, test.body) == nil {
			t.Fatal("public content validation accepted a forbidden payload")
		}
	}
}

func TestBulletinPostsNeverCopyPrivateFleetEvidence(t *testing.T) {
	for _, reply := range []bool{false, true} {
		t.Run(fmt.Sprint(reply), func(t *testing.T) {
			h, proxy, runner := newBulletinFixture(t)
			privateName := "private-census-" + uuid.NewString() + "-Nidoran\u2640"
			runner.stateCache.Ships[0].Ship.Name = privateName
			runner.snapshot["expansion"] = encode(map[string]any{"current_tier": map[string]any{"target_phase": 3, "progress_bps": 4321}})
			runner.meta["expansion"] = DatasetMeta{Complete: true, Generation: runner.generation, UpdatedAt: time.Now(), AttemptedAt: time.Now()}
			if reply {
				proxy.addThread("The Kraken Cartel", time.Hour, false, false)
			}
			if err := runner.saveBulletinState(h.ctx, bulletinState{NextReply: reply}); err != nil {
				t.Fatal(err)
			}
			_, data := stageBulletinFixture(t, h, runner)
			expected := bulletinThreads[0].Body
			if reply {
				expected = bulletinReplies[0]
			}
			if data.Bulletin.Body != expected || strings.Contains(data.Bulletin.Body, privateName) ||
				strings.Contains(data.Bulletin.Body, "Fleet roll:") || strings.Contains(data.Bulletin.Body, "43.21%") ||
				strings.Contains(data.Bulletin.Body, primaryTestKey) || strings.Contains(data.Bulletin.Body, "[REDACTED]") {
				t.Fatalf("private fleet or account evidence reached a public post: %s", data.Bulletin.Body)
			}
		})
	}
}

func TestBulletinReplyHistoryIsBounded(t *testing.T) {
	var state bulletinState
	for index := range 2 * bulletinReplyHistoryLimit {
		state.rememberReply(fmt.Sprint(index))
	}
	state.rememberReply(fmt.Sprint(2*bulletinReplyHistoryLimit - 1))
	if len(state.RepliedThreadIDs) != bulletinReplyHistoryLimit || state.RepliedThreadIDs[0] != fmt.Sprint(bulletinReplyHistoryLimit) {
		t.Fatalf("reply history grew or duplicate retention changed: %+v", state.RepliedThreadIDs)
	}
}

func TestBulletinPoolsExhaustBeforeRepeating(t *testing.T) {
	h, proxy, runner := newBulletinFixture(t)
	threadBodies := make(map[string]bool)
	replyBodies := make(map[string]bool)
	for index := range 2 * len(bulletinThreads) {
		if index > 0 {
			makeBulletinDue(t, h, runner)
		}
		if index%2 == 1 {
			proxy.addThread("The Kraken Cartel", time.Duration(index)*time.Second, false, false)
		}
		mission, data := stageBulletinFixture(t, h, runner)
		seen := threadBodies
		if data.Action.Stage == "bulletin-reply" {
			seen = replyBodies
		}
		if seen[data.Bulletin.Body] {
			t.Fatalf("template repeated before its pool was exhausted at slot %d", index)
		}
		seen[data.Bulletin.Body] = true
		sendBulletinFixture(t, h, runner, mission, &data)
	}
	if len(threadBodies) != len(bulletinThreads) || len(replyBodies) != len(bulletinReplies) {
		t.Fatalf("separate pools did not exhaust: threads=%d replies=%d", len(threadBodies), len(replyBodies))
	}
	makeBulletinDue(t, h, runner)
	_, data := stageBulletinFixture(t, h, runner)
	if data.Bulletin.TemplateIndex != 0 || !threadBodies[data.Bulletin.Body] {
		t.Fatal("thread pool did not wrap only after every template was posted")
	}
}

func TestBulletinReplySearchUsesArchiveBoundaryAndDefersOnlyActivePages(t *testing.T) {
	for _, mode := range []string{"old archived pages", "archived page boundary", "inconclusive active page"} {
		t.Run(mode, func(t *testing.T) {
			h, proxy, runner := newBulletinFixture(t)
			switch mode {
			case "old archived pages":
				for range 150 {
					proxy.addThread("The Kraken Cartel", 96*time.Hour, true, false)
				}
				proxy.addThread("Friendly rivals", time.Hour, false, false)
			case "archived page boundary":
				proxy.addThread("Friendly rivals", time.Hour, false, false)
				proxy.addThread("The Kraken Cartel", 72*time.Hour-time.Minute, true, false)
				proxy.nextCursor = "older-archived-threads"
			case "inconclusive active page":
				for range 101 {
					proxy.addThread("Friendly rivals", time.Hour, false, false)
				}
				proxy.addThread("The Kraken Cartel", 2*time.Hour, false, false)
			}
			if err := runner.saveBulletinState(h.ctx, bulletinState{NextReply: true}); err != nil {
				t.Fatal(err)
			}
			data := missionData{}
			changed, err := runner.driveBulletin(h.ctx, nil, &data)
			if err != nil || proxy.postCalls != 0 || proxy.boardReads != 1 || proxy.boardSince.IsZero() {
				t.Fatalf("reply search did not use one bounded board read: changed=%t error=%v", changed, err)
			}
			if mode == "inconclusive active page" {
				state, _ := runner.bulletinState()
				if changed || data.Action != nil || !state.CooldownUntil.After(time.Now()) {
					t.Fatal("incomplete active page did not skip with a durable cooldown")
				}
				return
			}
			if !changed || data.Action == nil || data.Action.Stage != "bulletin-thread" || !proxy.boardSince.Equal(data.Bulletin.NotBefore.Add(-72*time.Hour)) {
				t.Fatalf("archived history prevented the fallback thread: changed=%t data=%+v", changed, data)
			}
		})
	}
}

func resumeBulletinFixture(t *testing.T, h *recoveryHarness) (*Runner, *store.Mission, missionData) {
	t.Helper()
	runner := h.runner(primaryTestKey)
	runner.opts.BulletinEnabled = true
	refreshBulletinRunner(t, h, runner)
	state, err := runner.bulletinState()
	if err != nil {
		t.Fatal(err)
	}
	state.CooldownUntil = time.Time{}
	if err := runner.saveBulletinState(h.ctx, state); err != nil {
		t.Fatal(err)
	}
	mission, data, err := runner.loadMission(h.ctx, "__bulletin")
	if err != nil {
		t.Fatal(err)
	}
	if mission != nil {
		data.CooldownUntil = time.Time{}
		if err := runner.saveMission(h.ctx, mission, &data); err != nil {
			t.Fatal(err)
		}
	}
	return runner, mission, data
}

func seedAgedBulletinFixture(t *testing.T, h *recoveryHarness, runner *Runner, mission *store.Mission, data *missionData, age time.Duration) {
	t.Helper()
	// Historical requests must be inserted with their original timestamp:
	// the journal correctly forbids changing any request identity afterward.
	block, err := aes.NewCipher(bytes.Repeat([]byte{0x5A}, 32))
	if err != nil {
		t.Fatal(err)
	}
	aead, err := cipher.NewGCM(block)
	if err != nil {
		t.Fatal(err)
	}
	nonce := make([]byte, aead.NonceSize(), aead.NonceSize()+len(data.Action.Body)+aead.Overhead())
	if _, err := rand.Read(nonce); err != nil {
		t.Fatal(err)
	}
	aad := "action\x00" + h.account.ID + "\x00" + data.Action.ID + "\x00body"
	ciphertext := aead.Seal(nonce, nonce, data.Action.Body, []byte(aad))
	connection, err := sql.Open("pgx", h.dsn)
	if err != nil {
		t.Fatal(err)
	}
	defer connection.Close()
	createdAt := time.Now().UTC().Add(-age).Truncate(time.Microsecond)
	result, err := connection.ExecContext(h.ctx, `INSERT INTO actions
		(id, account_id, company_id, method, path, body_cipher, state, status, created_at, updated_at)
		VALUES ($1, $2, $3, $4, $5, $6, $7, 0, $8, $8)`,
		data.Action.ID, h.account.ID, h.account.CompanyID, data.Action.Method, data.Action.Path, ciphertext, store.ActionPending, createdAt)
	if err != nil {
		t.Fatal(err)
	}
	if affected, err := result.RowsAffected(); err != nil || affected != 1 {
		t.Fatalf("historical journal insertion affected %d rows: %v", affected, err)
	}
	data.Action.CreatedAt = createdAt
	if err := runner.saveMission(h.ctx, mission, data); err != nil {
		t.Fatal(err)
	}
}

func TestBulletinReplyPaginationSurvivesRestart(t *testing.T) {
	for _, hasAlly := range []bool{false, true} {
		t.Run(fmt.Sprint(hasAlly), func(t *testing.T) {
			h, proxy, runner := newBulletinFixture(t)
			for range 205 {
				proxy.addThread("Friendly rivals", time.Hour, false, false)
			}
			ally := ""
			if hasAlly {
				ally = proxy.addThread("The Kraken Cartel", 2*time.Hour, false, false)
			}
			if err := runner.saveBulletinState(h.ctx, bulletinState{NextReply: true}); err != nil {
				t.Fatal(err)
			}
			var since time.Time
			for index, cursor := range []string{"100", "200"} {
				if index > 0 {
					runner, _, _ = resumeBulletinFixture(t, h)
				}
				if changed, err := runner.driveBulletin(h.ctx, nil, &missionData{}); err != nil || changed {
					t.Fatalf("reply page %d did not defer its saved continuation: changed=%t error=%v", index, changed, err)
				}
				state, err := runner.bulletinState()
				if err != nil || state.ReplyScan == nil || state.ReplyScan.Cursor != cursor || !state.CooldownUntil.After(time.Now()) || proxy.boardReads != index+1 {
					t.Fatalf("reply continuation was lost: state=%+v reads=%d error=%v", state, proxy.boardReads, err)
				}
				if index == 0 {
					since = state.ReplyScan.Since
				} else if !state.ReplyScan.Since.Equal(since) {
					t.Fatal("reply restart changed the scan's archive boundary")
				}
			}
			runner, _, _ = resumeBulletinFixture(t, h)
			_, data := stageBulletinFixture(t, h, runner)
			if data.Bulletin.ThreadID != ally || proxy.boardReads != 3 || proxy.postCalls != 0 || !proxy.boardSince.Equal(since) ||
				!slices.Equal(proxy.boardCursors, []string{"", "100", "200"}) {
				t.Fatalf("reply search reread page one or missed the older ally/fallback: intent=%+v cursors=%v", data.Bulletin, proxy.boardCursors)
			}
		})
	}
}

func TestBulletinReconciliationPaginationSurvivesRestartAndAge(t *testing.T) {
	for _, test := range []struct {
		name    string
		pending bool
		age     time.Duration
	}{
		{"pending replayable", true, 2 * time.Hour},
		{"pending expired", true, 2 * store.ActionReplayWindow},
		{"completed expired", false, 2 * store.ActionReplayWindow},
	} {
		t.Run(test.name, func(t *testing.T) {
			h, proxy, runner := newBulletinFixture(t)
			mission, data := stageBulletinFixture(t, h, runner)
			key, payload := data.Action.ID, slices.Clone(data.Action.Body)
			seedAgedBulletinFixture(t, h, runner, mission, &data, test.age)
			proxy.badReceipt, proxy.dropReceipt = !test.pending, test.pending
			h.api.server.CloseClientConnections()
			// Reconstruct the historical exchange directly; production dispatch
			// must not be asked to send a request whose replay window has expired.
			var response json.RawMessage
			err := runner.api.Do(h.ctx, data.Action.Method, data.Action.Path, json.RawMessage(data.Action.Body), key, &response)
			var upstream *gameapi.APIError
			if test.pending {
				if err == nil || errors.As(err, &upstream) {
					t.Fatalf("lost receipt did not leave a transport-ambiguous attempt: %v", err)
				}
			} else {
				if !errors.As(err, &upstream) || upstream.Status != http.StatusCreated || upstream.Code != "invalid_response" {
					t.Fatalf("historical committed receipt was not unreadable: %v", err)
				}
				response = encode(map[string]any{"committed_uninterpreted": true, "raw_body": []byte(upstream.Body), "status": upstream.Status})
				if err := h.db.CompleteAction(h.ctx, key, upstream.Status, response, nil); err != nil {
					t.Fatal(err)
				}
				data.Bulletin.ReconcilePending = true
				if err := runner.saveMission(h.ctx, mission, &data); err != nil {
					t.Fatal(err)
				}
			}
			post := proxy.posted[key]
			if proxy.postCalls != 1 || post.ID == "" {
				t.Fatalf("fixture did not create one ambiguous post: posts=%d post=%+v", proxy.postCalls, post)
			}
			proxy.badReceipt, proxy.dropReceipt = false, false
			for range 205 {
				proxy.addThread("Friendly rivals", 0, false, false)
			}
			for index, cursor := range []string{"100", "200"} {
				runner, mission, data = resumeBulletinFixture(t, h)
				if changed, err := runner.driveBulletin(h.ctx, mission, &data); err != nil || !changed || data.Action == nil {
					t.Fatalf("reconciliation page %d did not persist progress: changed=%t error=%v", index, changed, err)
				}
				_, saved := h.mission("__bulletin")
				if saved.Action.ID != key || !bytes.Equal(saved.Action.Body, payload) || saved.Bulletin.ReconcileScan == nil ||
					saved.Bulletin.ReconcileScan.Cursor != cursor || proxy.postCalls != 1 || proxy.boardReads != index+2 {
					t.Fatalf("restart lost scan progress or replayed its POST: data=%+v reads=%d posts=%d", saved, proxy.boardReads, proxy.postCalls)
				}
			}
			runner, mission, data = resumeBulletinFixture(t, h)
			sendBulletinFixture(t, h, runner, mission, &data)
			state, err := runner.bulletinState()
			if err != nil || state.LastPostID != post.ID || state.ThreadCursor != 1 || data.LastActionID != key ||
				len(h.pending()) != 0 || proxy.postCalls != 1 || proxy.boardReads != 4 ||
				!slices.Equal(proxy.boardCursors, []string{"", "", "100", "200"}) {
				t.Fatalf("page-three evidence failed to reconcile without a resend: state=%+v cursors=%v posts=%d error=%v", state, proxy.boardCursors, proxy.postCalls, err)
			}
		})
	}
}

func TestBulletinExpiredRequestsNeverReplayAndNeverBlockTrading(t *testing.T) {
	for _, age := range []time.Duration{
		store.ActionReplayWindow - 30*time.Second, store.ActionReplayWindow, store.ActionReplayWindow + time.Second,
		24*time.Hour - 30*time.Second, 24 * time.Hour, 24*time.Hour + time.Second,
	} {
		t.Run(age.String(), func(t *testing.T) {
			h, proxy, runner := newBulletinFixture(t)
			mission, data := stageBulletinFixture(t, h, runner)
			key, payload := data.Action.ID, slices.Clone(data.Action.Body)
			seedAgedBulletinFixture(t, h, runner, mission, &data, age)
			for range 2 {
				runner, mission, data = resumeBulletinFixture(t, h)
				reads := proxy.boardReads
				if changed, err := runner.driveBulletin(h.ctx, mission, &data); err != nil || changed || data.Action == nil ||
					data.Action.ID != key || !bytes.Equal(data.Action.Body, payload) || proxy.postCalls != 0 || proxy.boardReads != reads+1 {
					t.Fatalf("replay safety boundary resent, discarded or scanned twice: changed=%t error=%v posts=%d reads=%d want=%d", changed, err, proxy.postCalls, proxy.boardReads, reads+1)
				}
			}
			row := sailingShipRow(recoveryShipBlastoise, "Blastoise", recoveryHomePort, recoveryFarPort)
			row["ship"].(map[string]any)["status"] = "berthed"
			row["voyage"], row["route_legs"] = nil, []any{}
			h.api.setFleet([]map[string]any{row})
			refreshBulletinRunner(t, h, runner)
			reads := proxy.boardReads
			proxy.readStatus = http.StatusServiceUnavailable
			if changed, err := runner.work(h.ctx); err != nil || !changed {
				t.Fatalf("expired optional request blocked maintenance planning: changed=%t error=%v", changed, err)
			}
			if changed, err := runner.work(h.ctx); err != nil || !changed || h.api.mutationCount("/v1/shipyard/modify-ship") != 1 || proxy.boardReads != reads || proxy.postCalls != 0 {
				t.Fatalf("expired optional request blocked maintenance dispatch: changed=%t error=%v", changed, err)
			}
		})
	}
}

func TestBulletinReplayCutoffDuringObservationDoesNotScanTwice(t *testing.T) {
	h, proxy, runner := newBulletinFixture(t)
	mission, data := stageBulletinFixture(t, h, runner)
	key, payload := data.Action.ID, slices.Clone(data.Action.Body)
	seedAgedBulletinFixture(t, h, runner, mission, &data, store.ActionReplayWindow-time.Minute-4*time.Second)
	cutoff := data.Action.CreatedAt.Add(store.ActionReplayWindow - time.Minute)
	entered := make(chan time.Time, 2)
	h.api.server.Config.Handler = http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		if request.Method == http.MethodGet && request.URL.Path == "/v1/bulletin" {
			entered <- time.Now()
			timer := time.NewTimer(time.Until(cutoff) + 20*time.Millisecond)
			defer timer.Stop()
			select {
			case <-timer.C:
			case <-request.Context().Done():
				return
			}
		}
		proxy.ServeHTTP(writer, request)
	})
	reads := proxy.boardReads
	// The journal receives a longer observation context here so the first GET
	// can cross the cutoff without merely exercising the optional child timeout.
	if changed, err := runner.dispatch(h.ctx, mission, &data); err != nil || changed || data.Action == nil ||
		data.Action.ID != key || !bytes.Equal(data.Action.Body, payload) || proxy.postCalls != 0 || proxy.boardReads != reads+1 {
		t.Fatalf("cutoff observation replayed or scanned twice: changed=%t error=%v posts=%d reads=%d want=%d", changed, err, proxy.postCalls, proxy.boardReads, reads+1)
	}
	select {
	case started := <-entered:
		if !started.Before(cutoff) {
			t.Fatalf("fixture did not enter its first observation before the cutoff: start=%v cutoff=%v", started, cutoff)
		}
	default:
		t.Fatal("fixture never observed the board")
	}
	if pending := h.pending(); len(pending) != 1 || pending[0].ID != key || !bytes.Equal(pending[0].Body, payload) {
		t.Fatalf("cutoff observation replaced its unresolved immutable request: %+v", pending)
	}
}

func TestBulletinUnexpiredPendingRequestReplaysOnlyItsOriginalKey(t *testing.T) {
	h, proxy, runner := newBulletinFixture(t)
	mission, data := stageBulletinFixture(t, h, runner)
	key, payload := data.Action.ID, slices.Clone(data.Action.Body)
	seedAgedBulletinFixture(t, h, runner, mission, &data, store.ActionReplayWindow-2*time.Minute)
	runner, mission, data = resumeBulletinFixture(t, h)
	sendBulletinFixture(t, h, runner, mission, &data)
	if proxy.postCalls != 1 || !bytes.Equal(proxy.bodies[key], payload) || len(h.pending()) != 0 || data.LastActionID != key {
		t.Fatal("unexpired pending request was replaced instead of replaying its immutable identity")
	}
}

func TestBulletinLegacyPrivatePayloadIsNeverSentOrResent(t *testing.T) {
	for _, attempted := range []bool{false, true} {
		t.Run(fmt.Sprint(attempted), func(t *testing.T) {
			h, proxy, runner := newBulletinFixture(t)
			mission, data := stageBulletinFixture(t, h, runner)
			data.Bulletin.Body += " Fleet roll: 37 ships. Private vessel sends a salute."
			data.Action.Body = encode(bulletinPayload{Title: data.Bulletin.Title, Body: data.Bulletin.Body})
			key, payload := data.Action.ID, slices.Clone(data.Action.Body)
			if err := runner.saveMission(h.ctx, mission, &data); err != nil {
				t.Fatal(err)
			}
			if attempted {
				if _, err := h.db.BeginAction(h.ctx, &store.Action{ID: key, AccountID: h.account.ID, Method: data.Action.Method, Path: data.Action.Path, Body: payload}); err != nil {
					t.Fatal(err)
				}
			}
			_, err := runner.driveBulletin(h.ctx, mission, &data)
			if proxy.postCalls != 0 || attempted && (err == nil || data.Action == nil || data.Action.ID != key || !bytes.Equal(data.Action.Body, payload)) {
				t.Fatalf("legacy private content was sent, replaced or released: attempted=%t error=%v posts=%d", attempted, err, proxy.postCalls)
			}
			if !attempted {
				if err != nil || data.Action != nil {
					t.Fatalf("proven never-sent legacy post was not retired: %v", err)
				}
				if changed, err := runner.driveBulletin(h.ctx, mission, &data); err != nil || !changed || proxy.postCalls != 0 {
					t.Fatalf("retiring the unused legacy mission sent a post: changed=%t error=%v", changed, err)
				}
				h.missingMission("__bulletin")
			}
		})
	}
}

func TestBulletinPublicReadAuthorizationStopsExecution(t *testing.T) {
	for _, status := range []int{http.StatusUnauthorized, http.StatusForbidden} {
		t.Run(fmt.Sprint(status), func(t *testing.T) {
			h, proxy, runner := newBulletinFixture(t)
			proxy.readStatus = status
			if changed, err := runner.driveBulletin(h.ctx, nil, &missionData{}); changed || !errors.Is(err, ErrExecutionStopped) || proxy.postCalls != 0 {
				t.Fatalf("public-read authorization failure was swallowed: changed=%t error=%v", changed, err)
			}
			state, err := runner.bulletinState()
			if err != nil || !state.CooldownUntil.IsZero() {
				t.Fatalf("authorization failure became an optional cooldown: %+v %v", state, err)
			}
		})
	}
}

func TestBulletinPublicScanStateIsBounded(t *testing.T) {
	ids := make([]string, bulletinPageLimit)
	for index := range ids {
		ids[index] = uuid.NewString()
	}
	if !validBulletinScan(strings.Repeat("c", bulletinCursorLimit), ids) {
		t.Fatal("the bounded public page was rejected")
	}
	if validBulletinScan(strings.Repeat("c", bulletinCursorLimit+1), ids) ||
		validBulletinScan("", append(slices.Clone(ids), uuid.NewString())) ||
		validBulletinScan("", []string{"not-a-thread-id"}) {
		t.Fatal("public scan accepted an oversized cursor, candidate queue or invalid thread ID")
	}
}
