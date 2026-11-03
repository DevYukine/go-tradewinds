package engine

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"slices"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/DevYukine/go-tradewinds/internal/gameapi"
	"github.com/DevYukine/go-tradewinds/internal/names"
	"github.com/DevYukine/go-tradewinds/internal/store"
	"github.com/google/uuid"
)

const (
	bulletinInterval          = 24 * time.Hour
	bulletinRetryDelay        = 15 * time.Minute
	bulletinRequestBudget     = 2 * time.Second
	bulletinReplyHistoryLimit = 128
	bulletinDailyReplies      = 2
	bulletinPageLimit         = 100
	bulletinCursorLimit       = 4096
	bulletinDetailLimit       = 3
	bulletinHistoryLimit      = 500
)

type bulletinState struct {
	LastPostID        string                         `json:"last_post_id,omitempty"`
	LastPostAt        time.Time                      `json:"last_post_at"`
	NextDue           time.Time                      `json:"next_due"`
	CooldownUntil     time.Time                      `json:"cooldown_until"`
	DiscussedItems    map[string]time.Time           `json:"discussed_items,omitempty"`
	DiscussedSubjects map[string]time.Time           `json:"discussed_subjects,omitempty"`
	CheckedThreads    map[string]bulletinThreadCheck `json:"thread_checks,omitempty"`
	TotalReplies      uint64                         `json:"total_replies,omitempty"`
	LeaderBaseline    string                         `json:"leader_baseline,omitempty"`
	LeaderSeason      int32                          `json:"leader_season,omitempty"`
	LeaderChange      *bulletinItem                  `json:"leader_change,omitempty"`
	ReplyTimes        []time.Time                    `json:"reply_times,omitempty"`
	Replies           map[string]bulletinReply       `json:"replies,omitempty"`
	ReplyCheckAfter   time.Time                      `json:"reply_check_after"`
	OpeningCheckAfter time.Time                      `json:"opening_check_after"`
	LastOutcomeID     string                         `json:"last_outcome_id,omitempty"`
	ReplyScan         *bulletinReplyScan             `json:"reply_scan,omitempty"`
}

type bulletinThreadCheck struct {
	LastPostAt time.Time `json:"last_post_at"`
	ObservedAt time.Time `json:"observed_at"`
}

type bulletinReply struct {
	At         time.Time `json:"at"`
	ObservedAt time.Time `json:"observed_at"`
	Count      int       `json:"count"`
}

type bulletinReplyScan struct {
	Since      time.Time            `json:"since"`
	Cursor     string               `json:"cursor,omitempty"`
	Candidates []string             `json:"candidates,omitempty"`
	LastPostAt map[string]time.Time `json:"last_post_at,omitempty"`
	Complete   bool                 `json:"complete"`
}

type bulletinReconcileScan struct {
	Cursor     string        `json:"cursor,omitempty"`
	Candidates []string      `json:"candidates,omitempty"`
	Complete   bool          `json:"complete"`
	Match      *bulletinPost `json:"match,omitempty"`
}

type bulletinIntent struct {
	CompanyName      string                 `json:"company_name"`
	Title            string                 `json:"title,omitempty"`
	Body             string                 `json:"body"`
	ThreadID         string                 `json:"thread_id,omitempty"`
	ItemID           string                 `json:"item_id,omitempty"`
	Content          bulletinContent        `json:"content"`
	NotBefore        time.Time              `json:"not_before"`
	BeforeIDs        []string               `json:"before_ids,omitempty"`
	ReconcilePending bool                   `json:"reconcile_pending,omitempty"`
	ReconcileScan    *bulletinReconcileScan `json:"reconcile_scan,omitempty"`
}

type bulletinPayload struct {
	Title    string `json:"title,omitempty"`
	Body     string `json:"body"`
	ThreadID string `json:"thread_id,omitempty"`
}

type bulletinThread struct {
	ID          string          `json:"id"`
	Title       string          `json:"title"`
	CompanyName string          `json:"company_name"`
	Link        json.RawMessage `json:"link"`
	Replies     int             `json:"replies"`
	CreatedAt   time.Time       `json:"created_at"`
	LastPostAt  time.Time       `json:"last_post_at"`
	Archived    bool            `json:"archived"`
	Removed     bool            `json:"removed"`
}

type bulletinPost struct {
	ID            string     `json:"id"`
	ThreadID      string     `json:"thread_id"`
	CompanyName   string     `json:"company_name"`
	Body          string     `json:"body"`
	Opening       bool       `json:"is_opening"`
	CreatedAt     time.Time  `json:"created_at"`
	EditedAt      *time.Time `json:"edited_at"`
	EditableUntil *time.Time `json:"editable_until"`
	Removed       bool       `json:"removed"`
	Deleted       bool       `json:"deleted"`
}

type bulletinPage struct {
	Threads    []bulletinThread `json:"threads"`
	NextCursor string           `json:"next_cursor"`
}

type bulletinDetail struct {
	Thread bulletinThread `json:"thread"`
	Posts  []bulletinPost `json:"posts"`
}

func bulletinStage(stage string) bool {
	return stage == "bulletin-thread" || stage == "bulletin-reply"
}

func bulletinPath(path string) bool {
	return path == "/v1/bulletin/thread" || path == "/v1/bulletin/reply"
}

func optionalControlPath(path string) bool {
	return bulletinPath(path) || path == "/v1/fleet/rename-ship"
}

func bulletinAuthorizationError(ctx context.Context, status int, err error) error {
	if canceled := ctx.Err(); canceled != nil {
		return errors.Join(canceled, err)
	}
	if errors.Is(err, ErrExecutionStopped) {
		return err
	}
	return errors.Join(fmt.Errorf("%w after definitive bulletin HTTP %d", ErrExecutionStopped, status), err)
}

func tradingActionsPending(actions []store.Action) bool {
	for _, action := range actions {
		if !optionalControlPath(action.Path) {
			return true
		}
	}
	return false
}

func (state bulletinState) due() time.Time {
	return maxTime(maxTime(state.NextDue, state.CooldownUntil), maxTime(state.LastPostAt.Add(bulletinInterval), state.OpeningCheckAfter))
}

func (state *bulletinState) rememberReply(threadID string, at time.Time) {
	if state.Replies == nil {
		state.Replies = make(map[string]bulletinReply)
	}
	reply := state.Replies[threadID]
	reply.At = at
	reply.ObservedAt = time.Now().UTC()
	reply.Count++
	state.Replies[threadID] = reply
	limitBulletinHistory(state.Replies, func(reply bulletinReply) time.Time { return reply.ObservedAt })
	state.TotalReplies++
}

func (state *bulletinState) rememberThreadCheck(id string, lastPostAt time.Time) {
	if state.CheckedThreads == nil {
		state.CheckedThreads = make(map[string]bulletinThreadCheck)
	}
	state.CheckedThreads[id] = bulletinThreadCheck{LastPostAt: lastPostAt, ObservedAt: time.Now().UTC()}
	limitBulletinHistory(state.CheckedThreads, func(check bulletinThreadCheck) time.Time { return check.ObservedAt })
}

func (state *bulletinState) prune(now time.Time) {
	cutoff := now.Add(-14 * 24 * time.Hour)
	for id, at := range state.DiscussedItems {
		if at.After(now) {
			state.DiscussedItems[id] = now
		}
	}
	for id, at := range state.DiscussedSubjects {
		if at.Before(cutoff) {
			delete(state.DiscussedSubjects, id)
		} else if at.After(now) {
			state.DiscussedSubjects[id] = now
		}
	}
	for id, check := range state.CheckedThreads {
		if check.ObservedAt.Before(cutoff) || check.ObservedAt.After(now) {
			delete(state.CheckedThreads, id)
		}
	}
	for id, reply := range state.Replies {
		// Older reply records retain their limits, but remote post times are
		// never used to start or extend local retention.
		if reply.ObservedAt.IsZero() || reply.ObservedAt.After(now) {
			reply.ObservedAt = now
			state.Replies[id] = reply
		}
		if reply.ObservedAt.Before(cutoff) {
			delete(state.Replies, id)
		}
	}
	limitBulletinHistory(state.DiscussedItems, func(at time.Time) time.Time { return at })
	limitBulletinHistory(state.DiscussedSubjects, func(at time.Time) time.Time { return at })
	limitBulletinHistory(state.CheckedThreads, func(check bulletinThreadCheck) time.Time { return check.ObservedAt })
	limitBulletinHistory(state.Replies, func(reply bulletinReply) time.Time { return reply.ObservedAt })
}

func limitBulletinHistory[T any](history map[string]T, observedAt func(T) time.Time) {
	if len(history) <= bulletinHistoryLimit {
		return
	}
	ids := make([]string, 0, len(history))
	for id := range history {
		ids = append(ids, id)
	}
	slices.SortFunc(ids, func(left, right string) int {
		if order := observedAt(history[left]).Compare(observedAt(history[right])); order != 0 {
			return order
		}
		return strings.Compare(left, right)
	})
	for _, id := range ids[:len(ids)-bulletinHistoryLimit] {
		delete(history, id)
	}
}

func (state *bulletinState) repliesAvailable(now time.Time) bool {
	state.ReplyTimes = slices.DeleteFunc(state.ReplyTimes, func(at time.Time) bool {
		return !at.After(now.Add(-24 * time.Hour))
	})
	return len(state.ReplyTimes) < bulletinDailyReplies
}

func (r *Runner) bulletinState() (bulletinState, error) {
	var state bulletinState
	if raw := r.snapshot["_bulletin"]; len(raw) > 0 {
		if err := json.Unmarshal(raw, &state); err != nil {
			return state, errors.New("invalid durable bulletin schedule")
		}
	}
	// Keep the former three-reply allowance readable across deployment.
	if len(state.ReplyTimes) > 3 {
		return state, errors.New("invalid durable bulletin reply history")
	}
	if state.ReplyScan != nil && (state.ReplyScan.Since.IsZero() || !validBulletinScan(state.ReplyScan.Cursor, state.ReplyScan.Candidates) || len(state.ReplyScan.LastPostAt) > bulletinPageLimit) {
		return state, errors.New("invalid durable bulletin reply scan")
	}
	if state.ReplyScan != nil {
		for _, id := range state.ReplyScan.Candidates {
			if state.ReplyScan.LastPostAt[id].IsZero() {
				state.ReplyScan = nil
				break
			}
		}
	}
	state.prune(time.Now().UTC())
	return state, nil
}

func (r *Runner) saveBulletinState(ctx context.Context, state bulletinState) error {
	state.prune(time.Now().UTC())
	raw := encode(state)
	previous := r.snapshot["_bulletin"]
	if bytes.Equal(raw, previous) {
		return nil
	}
	r.snapshot["_bulletin"] = raw
	if err := r.saveSnapshot(ctx); err != nil {
		if previous == nil {
			delete(r.snapshot, "_bulletin")
		} else {
			r.snapshot["_bulletin"] = previous
		}
		return err
	}
	return nil
}

func (r *Runner) driveBulletin(ctx context.Context, mission *store.Mission, data *missionData) (changed bool, err error) {
	if !r.opts.Execute {
		return false, nil
	}
	state, err := r.bulletinState()
	if err != nil {
		return false, err
	}
	defer func() {
		if err == nil || ctx.Err() != nil || errors.Is(err, ErrExecutionStopped) {
			return
		}
		var upstream *gameapi.APIError
		if errors.As(err, &upstream) && gameapi.BulletinAuthorizationRefusal(upstream.Status, upstream.Body) {
			err = bulletinAuthorizationError(ctx, upstream.Status, err)
			return
		}
		// Optional public reads and writes must never consume the trading retry loop.
		if current, stateErr := r.bulletinState(); stateErr == nil {
			state = current
		}
		state.CooldownUntil = maxTime(state.CooldownUntil, time.Now().UTC().Add(bulletinRetryDelay))
		if upstream != nil {
			state.CooldownUntil = maxTime(state.CooldownUntil, upstream.RetryAfter)
		}
		if data.Action != nil {
			data.CooldownUntil = maxTime(data.CooldownUntil, state.CooldownUntil)
			err = errors.Join(err, r.saveMission(ctx, mission, data))
		}
		err = errors.Join(err, r.saveBulletinState(ctx, state))
		_ = r.decision(ctx, "__bulletin", "bulletin", "public posting skipped after an unavailable observation or unresolved request; retry cooldown recorded", nil)
	}()
	if mission != nil && mission.Phase == "done" && data.Action == nil {
		return true, r.db.DeleteMission(ctx, r.account.ID, mission.ShipID)
	}
	if mission != nil && time.Now().Before(data.CooldownUntil) {
		r.wakeBy(data.CooldownUntil)
		return false, r.holdExpected(ctx, mission.ShipID, "public posting waits for its scoped persisted retry")
	}
	if mission != nil && data.Action != nil {
		until := maxTime(state.CooldownUntil, data.CooldownUntil)
		if time.Now().Before(until) {
			r.wakeBy(until)
			return false, r.decision(ctx, "__bulletin", "bulletin", "public posting waits for its persisted cooldown", map[string]any{"until": until})
		}
		requestCtx, cancel := context.WithTimeout(ctx, bulletinRequestBudget)
		changed, err = r.dispatchWithOutcomeContext(requestCtx, ctx, mission, data)
		cancel()
		if err == nil && !changed && data.Action != nil {
			data.CooldownUntil = maxTime(data.CooldownUntil, time.Now().UTC().Add(bulletinRetryDelay))
			state.CooldownUntil = maxTime(state.CooldownUntil, data.CooldownUntil)
			err = r.saveBulletinState(ctx, state)
			if err == nil {
				err = r.saveMission(ctx, mission, data)
			}
			if err == nil {
				err = r.decision(ctx, "__bulletin", "bulletin", "ambiguous public post retained with its original key; authoritative company posts are checked before any resend", nil)
			}
		}
		return changed, err
	}
	if mission != nil {
		if data.Bulletin == nil {
			return false, errors.New("bulletin mission has no immutable post intent")
		}
		if !r.bulletinGeneratedIntent(data.Bulletin) {
			return true, r.db.DeleteMission(ctx, r.account.ID, mission.ShipID)
		}
		return true, r.stageBulletin(ctx, mission, data)
	}
	if time.Now().Before(state.CooldownUntil) {
		r.wakeBy(state.CooldownUntil)
		return false, r.decision(ctx, "__bulletin", "bulletin", "public posting waits for its posting-only cooldown", map[string]any{"until": state.CooldownUntil})
	}
	var profile struct {
		Company struct {
			ID   string `json:"id"`
			Name string `json:"name"`
		} `json:"company"`
	}
	if json.Unmarshal(r.snapshot["profile"], &profile) != nil || profile.Company.ID != r.account.CompanyID || profile.Company.Name != names.DefaultCompany {
		return false, r.decision(ctx, "__bulletin", "bulletin", "public posting skipped: the recorded company is not The Pokemon Pirates", nil)
	}
	intent := &bulletinIntent{CompanyName: profile.Company.Name}
	intent.NotBefore, _, _ = r.serverBounds()
	if intent.NotBefore.IsZero() {
		return false, errors.New("bulletin staging requires an authoritative server clock")
	}
	replyAvailable := state.repliesAvailable(intent.NotBefore)
	if time.Now().Before(state.due()) && (!replyAvailable || time.Now().Before(state.ReplyCheckAfter)) {
		r.wakeBy(state.due())
		return false, r.saveBulletinState(ctx, state)
	}
	pending, err := r.db.PendingActions(ctx, r.account.ID)
	if err != nil {
		return false, err
	}
	for _, action := range pending {
		if bulletinPath(action.Path) {
			return false, r.decision(ctx, "__bulletin", "bulletin", "public posting skipped: an orphaned bulletin request requires its original mission", nil)
		}
	}
	items := r.bulletinItems(&state, intent.NotBefore)
	openingDue := !time.Now().Before(state.due())
	if openingDue {
		if item, ok := selectBulletinItem(items, state, intent.NotBefore); ok {
			intent.ItemID, intent.Content = item.ID, item.Content
		}
	}
	requestCtx, cancel := context.WithTimeout(ctx, bulletinRequestBudget)
	defer cancel()
	if intent.ItemID == "" && (replyAvailable || openingDue) && !time.Now().Before(state.ReplyCheckAfter) {
		cursor, candidates := "", 0
		if state.ReplyScan != nil {
			cursor, candidates = state.ReplyScan.Cursor, len(state.ReplyScan.Candidates)
		}
		debate, readErr := r.selectBulletinReply(requestCtx, &state, intent, items, openingDue, replyAvailable)
		err = readErr
		var upstream *gameapi.APIError
		if errors.Is(err, context.DeadlineExceeded) && ctx.Err() == nil &&
			(!errors.As(err, &upstream) || upstream.Status < http.StatusBadRequest) {
			err = nil
			if state.ReplyScan != nil && state.ReplyScan.Cursor == cursor && len(state.ReplyScan.Candidates) == candidates {
				state.ReplyCheckAfter = time.Now().UTC().Add(bulletinRetryDelay)
			}
		}
		if err != nil {
			return false, errors.Join(err, r.saveBulletinState(ctx, state))
		}
		if openingDue && debate != nil {
			if item, ok := selectBulletinItem([]bulletinItem{*debate}, state, intent.NotBefore); ok {
				intent.ThreadID, intent.BeforeIDs = "", nil
				intent.ItemID, intent.Content = item.ID, item.Content
			}
		}
	}
	if intent.ThreadID == "" && intent.ItemID == "" {
		if err := r.saveBulletinState(ctx, state); err != nil {
			return false, err
		}
		if !openingDue {
			r.wakeBy(state.due())
			return false, r.decision(ctx, "__bulletin", "bulletin", "new public threads wait at least 24 real hours and then need a new notable item; relevant replies use a separate daily allowance", map[string]any{"until": state.due()})
		}
		return false, r.decision(ctx, "__bulletin", "bulletin", "no new notable current world item or relevant board debate; no filler post", nil)
	}
	if intent.ThreadID == "" {
		// Page one is immutable pre-send evidence even when no reply scan ran.
		var page bulletinPage
		if err := r.api.GetOnce(requestCtx, "/v1/bulletin?limit=100", &page); err != nil {
			if errors.Is(err, context.DeadlineExceeded) && ctx.Err() == nil {
				state.ReplyCheckAfter = time.Now().UTC().Add(bulletinRetryDelay)
				state.OpeningCheckAfter = state.ReplyCheckAfter
				return false, r.saveBulletinState(ctx, state)
			}
			return false, errors.Join(err, r.saveBulletinState(ctx, state))
		}
		if page.Threads == nil || len(page.Threads) > bulletinPageLimit {
			return false, errors.New("bulletin opening page omitted its thread list")
		}
		intent.BeforeIDs = nil
		for _, thread := range page.Threads {
			if uuid.Validate(thread.ID) == nil {
				intent.BeforeIDs = append(intent.BeforeIDs, thread.ID)
			}
		}
	}
	intent.Title, intent.Body, err = r.renderBulletin(intent.Content)
	if err != nil {
		return false, err
	}
	if err := r.saveBulletinState(ctx, state); err != nil {
		return false, err
	}
	mission = &store.Mission{AccountID: r.account.ID, ShipID: "__bulletin", Phase: "bulletin-thread", Generation: uuid.NewString()}
	*data = missionData{Plan: Plan{Kind: "bulletin"}, Bulletin: intent, CostsKnown: true}
	return true, r.stageBulletin(ctx, mission, data)
}

func validBulletinScan(cursor string, candidates []string) bool {
	if len(cursor) > bulletinCursorLimit || len(candidates) > bulletinPageLimit {
		return false
	}
	for _, id := range candidates {
		if uuid.Validate(id) != nil {
			return false
		}
	}
	return true
}

func (r *Runner) selectBulletinReply(ctx context.Context, state *bulletinState, intent *bulletinIntent, items []bulletinItem, openingDue, replyAvailable bool) (*bulletinItem, error) {
	if state.ReplyScan == nil {
		state.ReplyScan = &bulletinReplyScan{Since: intent.NotBefore.Add(-72 * time.Hour), LastPostAt: make(map[string]time.Time)}
	}
	scan := state.ReplyScan
	if scan.LastPostAt == nil {
		scan.LastPostAt = make(map[string]time.Time)
	}
	catalog := decode[gameapi.Catalog](r.snapshot, "catalog")
	if len(scan.Candidates) == 0 && !scan.Complete {
		path := "/v1/bulletin?limit=100&since=" + url.QueryEscape(scan.Since.Format(time.RFC3339Nano))
		if scan.Cursor != "" {
			path += "&cursor=" + url.QueryEscape(scan.Cursor)
		}
		var page bulletinPage
		if err := r.api.GetOnce(ctx, path, &page); err != nil {
			return nil, err
		}
		if page.Threads == nil || len(page.Threads) > bulletinPageLimit || len(page.NextCursor) > bulletinCursorLimit || page.NextCursor != "" && page.NextCursor == scan.Cursor {
			return nil, errors.New("bulletin reply page is incomplete or has an invalid cursor")
		}
		scan.Complete, scan.Cursor = page.NextCursor == "", page.NextCursor
		slices.SortFunc(page.Threads, func(left, right bulletinThread) int {
			if order := right.LastPostAt.Compare(left.LastPostAt); order != 0 {
				return order
			}
			return strings.Compare(left.ID, right.ID)
		})
		for _, thread := range page.Threads {
			if bulletinThreadActive(thread, intent.NotBefore) && uuid.Validate(thread.ID) == nil &&
				(thread.CompanyName != intent.CompanyName || thread.Replies > 0) &&
				!thread.LastPostAt.Equal(state.CheckedThreads[thread.ID].LastPostAt) && !slices.Contains(scan.Candidates, thread.ID) {
				scan.Candidates = append(scan.Candidates, thread.ID)
				scan.LastPostAt[thread.ID] = thread.LastPostAt
			}
		}
		if len(page.Threads) > 0 && page.Threads[len(page.Threads)-1].Archived {
			scan.Complete = true
		}
	}
	var debate *bulletinItem
	for reads := 0; len(scan.Candidates) > 0 && reads < bulletinDetailLimit; reads++ {
		if err := ctx.Err(); err != nil {
			return debate, err
		}
		id := scan.Candidates[0]
		lastPostAt := scan.LastPostAt[id]
		var detail bulletinDetail
		err := r.api.GetOnce(ctx, "/v1/bulletin/thread?id="+url.QueryEscape(id), &detail)
		if err != nil {
			var upstream *gameapi.APIError
			if errors.As(err, &upstream) && gameapi.BulletinAuthorizationRefusal(upstream.Status, upstream.Body) {
				return debate, err
			}
		}
		scan.Candidates = scan.Candidates[1:]
		delete(scan.LastPostAt, id)
		if err != nil || detail.Thread.ID != id || detail.Posts == nil {
			state.rememberThreadCheck(id, lastPostAt)
			continue
		}
		if bulletinThreadActive(detail.Thread, intent.NotBefore) {
			content, relevant, discussion := bulletinRelevance(detail, *state, items, catalog, intent.NotBefore)
			discussionAvailable := false
			if discussion != nil {
				_, discussionAvailable = selectBulletinItem([]bulletinItem{*discussion}, *state, intent.NotBefore)
				if discussionAvailable {
					debate = discussion
					if openingDue {
						if scan.Complete && len(scan.Candidates) == 0 {
							state.ReplyScan = nil
						}
						return debate, nil
					}
				}
			}
			if relevant && replyAvailable {
				state.rememberThreadCheck(id, detail.Thread.LastPostAt)
				intent.ThreadID, intent.Content, intent.BeforeIDs = id, content, nil
				for _, post := range detail.Posts {
					if post.CompanyName == intent.CompanyName && uuid.Validate(post.ID) == nil && len(intent.BeforeIDs) < bulletinReplyHistoryLimit {
						intent.BeforeIDs = append(intent.BeforeIDs, post.ID)
					}
				}
				if scan.Complete && len(scan.Candidates) == 0 {
					state.ReplyScan = nil
				}
				return debate, nil
			}
			if content.Kind != "" || discussionAvailable {
				// A time or allowance gate is temporary, not evidence that the
				// unchanged conversation has nothing left to discuss.
				continue
			}
		}
		state.rememberThreadCheck(id, detail.Thread.LastPostAt)
	}
	if scan.Complete && len(scan.Candidates) == 0 {
		state.ReplyScan = nil
		state.ReplyCheckAfter = time.Now().UTC().Add(bulletinRetryDelay)
	}
	return debate, nil
}

func bulletinThreadActive(thread bulletinThread, now time.Time) bool {
	return !thread.Archived && !thread.Removed && !thread.LastPostAt.IsZero() && thread.LastPostAt.After(now.Add(-72*time.Hour))
}

func (r *Runner) stageBulletin(ctx context.Context, mission *store.Mission, data *missionData) error {
	intent := data.Bulletin
	stage, path := "bulletin-thread", "/v1/bulletin/thread"
	if intent.ThreadID != "" {
		stage, path = "bulletin-reply", "/v1/bulletin/reply"
	}
	mission.Phase = stage
	if err := r.decision(ctx, "__bulletin", "bulletin", "staging an immutable public company post after trading was idle", intent); err != nil {
		return err
	}
	return r.stage(ctx, mission, data, ShipState{}, stage, path, bulletinPayload{Title: intent.Title, Body: intent.Body, ThreadID: intent.ThreadID})
}

func validBulletinText(title, body string) error {
	if utf8.RuneCountInString(title) > 120 || strings.TrimSpace(body) == "" || utf8.RuneCountInString(body) > 1000 || strings.ContainsAny(title+body, "\u2013\u2014") {
		return errors.New("bulletin content exceeds public text limits or contains a forbidden dash")
	}
	return nil
}

func (r *Runner) bulletinPreflight(data *missionData) error {
	intent, action := data.Bulletin, data.Action
	if intent == nil || action == nil || intent.CompanyName != names.DefaultCompany || intent.NotBefore.IsZero() {
		return errors.New("bulletin request lacks its immutable company intent")
	}
	var request bulletinPayload
	if json.Unmarshal(action.Body, &request) != nil || request.Title != intent.Title || request.Body != intent.Body || request.ThreadID != intent.ThreadID || action.Method != http.MethodPost {
		return errors.New("bulletin request differs from its immutable post intent")
	}
	if intent.ThreadID == "" && (intent.Title == "" || action.Stage != "bulletin-thread" || action.Path != "/v1/bulletin/thread") || intent.ThreadID != "" && (intent.Title != "" || action.Stage != "bulletin-reply" || action.Path != "/v1/bulletin/reply") {
		return errors.New("bulletin request stage differs from its thread or reply payload")
	}
	if !r.bulletinGeneratedIntent(intent) {
		return errors.New("public post content differs from its world-aware company template")
	}
	return validBulletinText(intent.Title, intent.Body)
}

func (r *Runner) bulletinGeneratedIntent(intent *bulletinIntent) bool {
	if intent == nil || (intent.ThreadID != "") != strings.HasPrefix(intent.Content.Kind, "reply-") {
		return false
	}
	title, body, err := r.renderBulletin(intent.Content)
	return err == nil && title == intent.Title && body == intent.Body && (intent.ThreadID != "" || intent.ItemID != "")
}

func bulletinCooldown(status int, code, detail string, retryAfter, now time.Time) time.Time {
	until := now.Add(6 * time.Hour)
	if status == http.StatusTooManyRequests {
		until = maxTime(retryAfter, gameapi.BulletinDetailDeadline(detail, now))
		if !until.After(now) {
			until = now.Add(time.Hour)
		}
	} else if status == http.StatusForbidden && code == "posting_muted" {
		until = maxTime(retryAfter, gameapi.BulletinDetailDeadline(detail, now))
		if !until.After(now) {
			until = now.Add(24 * time.Hour)
		}
	}
	return maxTime(until, retryAfter)
}

func bulletinThreadRefusal(status int, code string) bool {
	if status == http.StatusNotFound || status == http.StatusConflict {
		return true
	}
	if status != http.StatusBadRequest && status != http.StatusUnprocessableEntity {
		return false
	}
	switch code {
	case "not_found", "archived", "removed", "thread_archived", "thread_removed", "thread_locked":
		return true
	}
	return false
}

func (r *Runner) consumeBulletinResult(ctx context.Context, mission *store.Mission, data *missionData, status int, response json.RawMessage) (bool, error) {
	if data.Bulletin == nil {
		return false, errors.New("bulletin result has no durable company intent")
	}
	if status >= 400 && status < 500 {
		state, err := r.bulletinState()
		if err != nil {
			return false, err
		}
		var problem struct {
			Code   string `json:"code"`
			Detail string `json:"detail"`
		}
		_ = json.Unmarshal(response, &problem)
		threadRefusal := data.Bulletin.ThreadID != "" && bulletinThreadRefusal(status, problem.Code)
		if state.LastOutcomeID != data.Action.ID {
			if !threadRefusal {
				state.CooldownUntil = bulletinCooldown(status, problem.Code, problem.Detail, data.CooldownUntil, time.Now().UTC())
			}
			state.LastOutcomeID = data.Action.ID
			if err := r.saveBulletinState(ctx, state); err != nil {
				return false, err
			}
		}
		reason := "public post refused; company posting cooldown recorded without delaying trade"
		if threadRefusal {
			reason = "public reply refused; checked thread skipped without a company posting cooldown"
			data.CooldownUntil = time.Time{}
		} else {
			data.CooldownUntil = state.CooldownUntil
		}
		if err := r.decision(ctx, "__bulletin", "bulletin", reason, map[string]any{"status": status, "thread_id": data.Bulletin.ThreadID, "until": state.CooldownUntil}); err != nil {
			return false, err
		}
		return r.finishBulletinMission(ctx, mission, data)
	}
	if status < 200 || status >= 300 {
		return false, nil
	}
	if data.Action.Stage == "bulletin-reply" {
		var post bulletinPost
		if json.Unmarshal(response, &post) == nil && matchesBulletinPost(data.Bulletin, post) {
			return r.completeBulletinPost(ctx, mission, data, post)
		}
	} else {
		var detail bulletinDetail
		if json.Unmarshal(response, &detail) == nil && detail.Thread.Title == data.Bulletin.Title && detail.Thread.CompanyName == data.Bulletin.CompanyName {
			for _, post := range detail.Posts {
				if post.ThreadID == detail.Thread.ID && matchesBulletinPost(data.Bulletin, post) {
					return r.completeBulletinPost(ctx, mission, data, post)
				}
			}
		}
	}
	if !data.Bulletin.ReconcilePending {
		data.Bulletin.ReconcilePending = true
		return false, r.saveMission(ctx, mission, data)
	}
	return r.reconcileBulletin(ctx, mission, data, &store.Action{ID: data.Action.ID, State: store.ActionComplete})
}

func matchesBulletinPost(intent *bulletinIntent, post bulletinPost) bool {
	return intent != nil && uuid.Validate(post.ID) == nil && uuid.Validate(post.ThreadID) == nil && post.CompanyName == intent.CompanyName && post.Body == intent.Body && !post.Removed && !post.Deleted &&
		post.Opening == (intent.ThreadID == "") && (intent.ThreadID == "" || intent.ThreadID == post.ThreadID) && !post.CreatedAt.IsZero() &&
		!post.CreatedAt.Before(intent.NotBefore) && !slices.Contains(intent.BeforeIDs, post.ID)
}

func (r *Runner) completeBulletinPost(ctx context.Context, mission *store.Mission, data *missionData, post bulletinPost) (bool, error) {
	state, err := r.bulletinState()
	if err != nil {
		return false, err
	}
	if state.LastOutcomeID != data.Action.ID {
		intent := data.Bulletin
		state.LastOutcomeID, state.LastPostID = data.Action.ID, post.ID
		state.CooldownUntil = time.Time{}
		observedAt := time.Now().UTC()
		if intent.ThreadID == "" {
			// LastPostAt is the last opening post; reactive replies never move it.
			state.LastPostAt = post.CreatedAt
			// The next thread waits at least a real day, then follows the next
			// notable world item rather than a fixed clock.
			state.NextDue = maxTime(time.Now().UTC(), post.CreatedAt).Add(bulletinInterval)
			state.OpeningCheckAfter = time.Time{}
			if state.DiscussedItems == nil {
				state.DiscussedItems = make(map[string]time.Time)
			}
			if intent.ItemID != "" {
				state.DiscussedItems[intent.ItemID] = observedAt
			}
			if state.DiscussedSubjects == nil {
				state.DiscussedSubjects = make(map[string]time.Time)
			}
			state.DiscussedSubjects[intent.Content.subjectKey()] = observedAt
		} else {
			state.repliesAvailable(post.CreatedAt)
			state.ReplyTimes = append(state.ReplyTimes, post.CreatedAt)
			state.rememberReply(intent.ThreadID, post.CreatedAt)
			state.rememberThreadCheck(intent.ThreadID, post.CreatedAt)
			state.ReplyCheckAfter = time.Time{}
		}
		if err := r.saveBulletinState(ctx, state); err != nil {
			return false, err
		}
	}
	if err := r.decision(ctx, "__bulletin", "bulletin", "public company post confirmed; thread cadence and reactive reply allowance recorded", map[string]any{"post_id": post.ID, "thread_id": post.ThreadID, "posted_at": post.CreatedAt, "next_thread_due": state.NextDue}); err != nil {
		return false, err
	}
	return r.finishBulletinMission(ctx, mission, data)
}

func (r *Runner) finishBulletinMission(ctx context.Context, mission *store.Mission, data *missionData) (bool, error) {
	data.LastActionID = data.Action.ID
	data.Action = nil
	data.Sequence++
	mission.Phase = "done"
	return true, r.saveMission(ctx, mission, data)
}

func (r *Runner) restoreBulletinReconciliation(ctx context.Context, mission *store.Mission, data *missionData, response json.RawMessage) (bool, error) {
	var evidence struct {
		Kind string       `json:"kind"`
		Post bulletinPost `json:"post"`
	}
	if json.Unmarshal(response, &evidence) != nil || evidence.Kind != "authoritative_bulletin_post" || !matchesBulletinPost(data.Bulletin, evidence.Post) {
		return false, errors.New("reconciled bulletin request lacks its exact company post")
	}
	return r.completeBulletinPost(ctx, mission, data, evidence.Post)
}

func (r *Runner) reconcileBulletin(ctx context.Context, mission *store.Mission, data *missionData, action *store.Action) (bool, error) {
	intent := data.Bulletin
	if intent == nil {
		return false, errors.New("unresolved bulletin request lacks its immutable company intent")
	}
	if time.Now().Before(data.CooldownUntil) {
		return false, nil
	}
	if intent.ReconcileScan == nil {
		intent.ReconcileScan = &bulletinReconcileScan{}
		if intent.ThreadID != "" {
			intent.ReconcileScan.Candidates = []string{intent.ThreadID}
			intent.ReconcileScan.Complete = true
		}
	}
	scan := intent.ReconcileScan
	if !validBulletinScan(scan.Cursor, scan.Candidates) || scan.Match != nil && !matchesBulletinPost(intent, *scan.Match) {
		return false, errors.New("unresolved bulletin request has an invalid reconciliation scan")
	}
	if len(scan.Candidates) == 0 && !scan.Complete {
		path := "/v1/bulletin?limit=100&since=" + url.QueryEscape(intent.NotBefore.Format(time.RFC3339Nano))
		if scan.Cursor != "" {
			path += "&cursor=" + url.QueryEscape(scan.Cursor)
		}
		var page bulletinPage
		if err := r.api.GetOnce(ctx, path, &page); err != nil {
			return false, err
		}
		if page.Threads == nil || len(page.Threads) > bulletinPageLimit || len(page.NextCursor) > bulletinCursorLimit || page.NextCursor != "" && page.NextCursor == scan.Cursor {
			return false, errors.New("bulletin reconciliation page is incomplete or has an invalid cursor")
		}
		scan.Cursor, scan.Complete = page.NextCursor, page.NextCursor == ""
		for _, thread := range page.Threads {
			if thread.CompanyName == intent.CompanyName && thread.Title == intent.Title && !thread.CreatedAt.Before(intent.NotBefore) &&
				uuid.Validate(thread.ID) == nil && !slices.Contains(intent.BeforeIDs, thread.ID) && !slices.Contains(scan.Candidates, thread.ID) {
				scan.Candidates = append(scan.Candidates, thread.ID)
			}
		}
		if err := r.saveMission(ctx, mission, data); err != nil {
			return false, err
		}
	}
	for len(scan.Candidates) > 0 {
		id := scan.Candidates[0]
		var detail bulletinDetail
		if err := r.api.GetOnce(ctx, "/v1/bulletin/thread?id="+url.QueryEscape(id), &detail); err != nil {
			return false, err
		}
		if detail.Thread.ID != id || detail.Posts == nil {
			return false, errors.New("bulletin reconciliation omitted the requested thread or posts")
		}
		for _, post := range detail.Posts {
			if post.ThreadID != id || !matchesBulletinPost(intent, post) {
				continue
			}
			if scan.Match != nil && scan.Match.ID != post.ID {
				return false, errors.New("multiple company bulletin posts match the unresolved request")
			}
			copy := post
			scan.Match = &copy
		}
		scan.Candidates = scan.Candidates[1:]
		if err := r.saveMission(ctx, mission, data); err != nil {
			return false, err
		}
	}
	if !scan.Complete {
		state, err := r.bulletinState()
		if err != nil {
			return false, err
		}
		data.CooldownUntil = time.Now().UTC().Add(bulletinRetryDelay)
		state.CooldownUntil = maxTime(state.CooldownUntil, data.CooldownUntil)
		if err := r.saveMission(ctx, mission, data); err != nil {
			return false, err
		}
		if err := r.saveBulletinState(ctx, state); err != nil {
			return false, err
		}
		return true, r.decision(ctx, "__bulletin", "bulletin", "public post reconciliation saved its next page without resending the immutable request", nil)
	}
	if scan.Match == nil {
		intent.ReconcileScan = nil
		if !r.bulletinGeneratedIntent(intent) {
			return false, errors.New("legacy public post remains reconciliation-only; private facts are never resent")
		}
		return false, r.saveMission(ctx, mission, data)
	}
	if action.State != store.ActionComplete {
		evidence := encode(map[string]any{"kind": "authoritative_bulletin_post", "post": scan.Match})
		if err := r.db.ReconcileAction(ctx, action.ID, evidence); err != nil {
			return false, err
		}
	}
	return r.completeBulletinPost(ctx, mission, data, *scan.Match)
}
