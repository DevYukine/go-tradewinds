package engine

import (
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
	bulletinInterval          = 3 * time.Hour
	bulletinRetryDelay        = 15 * time.Minute
	bulletinRequestBudget     = 2 * time.Second
	bulletinReplyHistoryLimit = 128
	bulletinPageLimit         = 100
	bulletinCursorLimit       = 4096
)

type bulletinState struct {
	LastPostID       string             `json:"last_post_id,omitempty"`
	LastPostAt       time.Time          `json:"last_post_at"`
	NextDue          time.Time          `json:"next_due"`
	CooldownUntil    time.Time          `json:"cooldown_until"`
	ThreadCursor     int                `json:"thread_cursor"`
	ReplyCursor      int                `json:"reply_cursor"`
	NextReply        bool               `json:"next_reply"`
	RepliedThreadIDs []string           `json:"replied_thread_ids,omitempty"`
	LastOutcomeID    string             `json:"last_outcome_id,omitempty"`
	ReplyScan        *bulletinReplyScan `json:"reply_scan,omitempty"`
}

type bulletinReplyScan struct {
	Since      time.Time `json:"since"`
	Cursor     string    `json:"cursor,omitempty"`
	Candidates []string  `json:"candidates,omitempty"`
	Complete   bool      `json:"complete"`
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
	TemplateIndex    int                    `json:"template_index"`
	ReplyTurn        bool                   `json:"reply_turn"`
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
	ID          string    `json:"id"`
	Title       string    `json:"title"`
	CompanyName string    `json:"company_name"`
	CreatedAt   time.Time `json:"created_at"`
	LastPostAt  time.Time `json:"last_post_at"`
	Archived    bool      `json:"archived"`
	Removed     bool      `json:"removed"`
}

type bulletinPost struct {
	ID          string    `json:"id"`
	ThreadID    string    `json:"thread_id"`
	CompanyName string    `json:"company_name"`
	Body        string    `json:"body"`
	Opening     bool      `json:"is_opening"`
	CreatedAt   time.Time `json:"created_at"`
	Removed     bool      `json:"removed"`
	Deleted     bool      `json:"deleted"`
}

type bulletinPage struct {
	Threads    []bulletinThread `json:"threads"`
	NextCursor string           `json:"next_cursor"`
}

type bulletinDetail struct {
	Thread bulletinThread `json:"thread"`
	Posts  []bulletinPost `json:"posts"`
}

type bulletinTemplate struct {
	Title string
	Body  string
}

var bulletinThreads = []bulletinTemplate{
	{"A Lapras-sized salute", "Ahoy from The Pokemon Pirates! Our ally The Kraken Cartel keeps the Admiralty expansion tide moving. We bring the sea legs and a suspiciously cheerful Lapras impression."},
	{"Teamwork used Surf", "The Kraken Cartel funds expansion, the harbour gets bigger, and our crew shouts 'super effective' at the paperwork. Admiralty cargo deserves a good escort and a better sea shanty."},
	{"The forward book and the Pokedex", "Our Pokedex has a new entry: Kraken Cartel forward book, known habitat a well-organised harbour. We would welcome a chat about freight terms. No Master Ball required."},
	{"Credit where credit is due", "Pikachu handles morale, not interest calculations. We are interested in sensible Kraken Cartel credit terms and freight work. An ally with a ledger is worth saluting."},
	{"A flag for shared horizons", "The Pokemon Pirates back The Kraken Cartel and the shared expansion effort. More harbours means more room for everyone to argue about whose Wingull stole the biscuits."},
	{"Admiralty cargo, meet the crew", "We are keen to carry Admiralty cargo alongside our Kraken Cartel allies when the terms fit. Machop has volunteered for loading. Psyduck has volunteered for supervision."},
	{"The leaderboard needs a crow's nest", "The leaderboard race is friendly, but our lookout still wants a taller crow's nest. A salute to The Kraken Cartel for turning expansion funding into shared horizons."},
	{"Freight talk over berry biscuits", "Kraken Cartel freight terms are welcome at our table. The biscuits are berries, the parrot is a Chatot, and the crew promises not to sing during negotiations."},
	{"Expansion used Helping Hand", "Helping Hand is our favourite move when The Kraken Cartel and the Admiralty open new water. Our crew cheers the joint work, even when Psyduck applauds with the manifest."},
	{"A forward-looking flock", "Our Wingull is learning about The Kraken Cartel's forward book. It currently understands only 'forward', but we welcome a conversation about useful freight and credit terms."},
	{"All aboard the cooperation ship", "The Pokemon Pirates sail under our own flag and cheer our Kraken Cartel allies. Shared expansion beats a harbour full of captains trying to evolve their egos."},
	{"The great biscuit regatta", "To our leaderboard neighbours: fair winds, good cargo, and may your biscuits stay dry. To The Kraken Cartel: cheers for backing Admiralty expansion. Our Chatot has written a chorus."},
	{"A Squirtle salute to the harbour", "Squirtle says a bigger harbour is just a better splash zone. We say cheers to The Kraken Cartel and everyone carrying the Admiralty expansion effort forward."},
	{"Terms welcome, sea shanties optional", "The Kraken Cartel brings freight service and a forward book; we bring willing sea legs. Credit and carriage terms are welcome. Sea shanties can be negotiated separately."},
	{"The alliance has good sea legs", "The Pokemon Pirates proudly back our Kraken Cartel allies. Expansion takes many holds and plenty of patience. Snorlax is demonstrating the patience part on a coil of rope."},
	{"No rare candy for the leaderboard", "Our crew checked: rare candy cannot raise a company rank. We will stick to good voyages, friendly rivals, and cheering The Kraken Cartel's expansion work."},
	{"A manifest with room for friends", "There is room in our manifest for useful work with The Kraken Cartel. Admiralty cargo, freight discussions and sensible credit terms all sound better than another Psyduck committee."},
	{"The harbour used Grow", "The harbour used Grow. It was a team effort! A loud ahoy to The Kraken Cartel for funding expansion and to the crews moving Admiralty cargo. Our Bulbasaur approves the growth theme."},
	{"A toast without Thunderbolt", "We raise a mug to The Kraken Cartel's expansion backing and freight service. Pikachu has been asked to keep Thunderbolt away from the rigging during the toast."},
	{"Chatot's very public minutes", "Meeting minutes: back our Kraken Cartel allies, welcome freight and credit terms, cheer shared Admiralty expansion, keep Chatot away from the speaking trumpet. Motion carried."},
	{"The friendly race continues", "Friendly leaderboard rivals, keep those sails bright. Our crew is chasing good voyages, not grudges. The Kraken Cartel gets a salute for helping the whole chart grow."},
	{"Machop asks about the freight desk", "Machop would like to know where The Kraken Cartel keeps the freight desk. We would like to discuss carriage terms. Psyduck would like to know whether the desk floats."},
	{"An Eevee for every horizon", "Eevee sees several ways to grow; our crew likes the one with allies and new harbours. We back The Kraken Cartel and welcome practical talk about credit, freight and Admiralty work."},
	{"Fair winds and shared charts", "Our Kraken Cartel allies help fund the next horizon, and the harbour crews do the carrying. The Pokemon Pirates cheer the joint work. Magikarp has contributed a ceremonial splash."},
}

var bulletinReplies = []string{
	"Ahoy, Kraken Cartel allies! The Pokemon Pirates cheer this expansion work. Lapras supplies the applause and most of the splashing.",
	"A Squirtle salute from our crew. We welcome freight terms that help turn shared Admiralty work into full holds and wider horizons.",
	"Your forward book has our attention. Psyduck is reading it backwards, but the rest of us would welcome useful carriage terms.",
	"Credit and freight discussions are welcome from our Kraken Cartel allies. Pikachu promises not to sign anything with Thunderbolt.",
	"Helping Hand is the right move for expansion. Cheers to the Cartel and the crews carrying Admiralty cargo. Machop is already applauding.",
	"The Pokemon Pirates back our Kraken Cartel allies. Our Chatot has prepared a supportive sea shanty and has mercifully kept it to one verse.",
	"A bigger chart is good news for every lookout. Cheers for the expansion funding. Our Wingull has requested a larger map to perch on.",
	"We would welcome a chat about freight or credit terms. Snorlax offers to chair the meeting, provided the chair is a hammock.",
	"Fair winds, allies. Admiralty cargo alongside the Cartel sounds like useful teamwork. Squirtle has stamped the idea 'splash approved'.",
	"The leaderboard can race; the harbour can still cooperate. Our crew salutes the Cartel's expansion work and offers a friendly tip of the tricorn.",
	"Our Pokedex lists good allies as a rare encounter. Glad to have The Kraken Cartel on the chart. Sensible freight terms are always worth discussing.",
	"Cheers from The Pokemon Pirates. Expansion used Surf, the crews used Helping Hand, and Psyduck used the manifest as a hat.",
	"The Cartel's freight service gets a hearty ahoy. We welcome practical terms, with sea shanties available only by mutual consent.",
	"We support this shared expansion effort. Bulbasaur says growth is easier when everyone remembers to water the harbourmaster's plants.",
	"Our crew is interested in the Cartel's forward book and credit terms. Meowth is allowed to bring a pencil, not make promises.",
	"A good alliance beats a deck full of captains arguing over the compass. Cheers to The Kraken Cartel and everyone hauling for the Admiralty.",
	"The Pokemon Pirates are keen to discuss useful carriage alongside our Cartel allies. Machop brings enthusiasm; Psyduck brings questions.",
	"A salute for funding the next horizon. Our Magikarp contributed a splash, which the quartermaster has entered as moral support.",
	"Friendly leaderboard racing, shared harbour building: our crew likes both. Cheers to the Cartel for helping expansion keep its sea legs.",
	"Freight and credit terms are welcome. Eevee suggests several possibilities, and our crew would rather discuss them than guess.",
	"We cheer the Admiralty work and our Kraken Cartel allies. Chatot has been told that 'all hands' does not mean borrowing Machop's.",
	"The forward book looks more useful than Psyduck's treasure map, which is mostly a drawing of lunch. Happy to talk practical freight terms.",
	"Ahoy from our crew. The Cartel's expansion backing deserves a toast, and Pikachu has agreed to keep the toast free of sparks.",
	"Fair winds to our Kraken Cartel allies. The Pokemon Pirates welcome sensible credit and freight talk, and cheer every shared step toward new harbours.",
}

func bulletinStage(stage string) bool {
	return stage == "bulletin-thread" || stage == "bulletin-reply"
}

func bulletinPath(path string) bool {
	return path == "/v1/bulletin/thread" || path == "/v1/bulletin/reply"
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
		if !bulletinPath(action.Path) {
			return true
		}
	}
	return false
}

func (state bulletinState) due() time.Time {
	return maxTime(maxTime(state.NextDue, state.CooldownUntil), state.LastPostAt.Add(bulletinInterval))
}

func (state *bulletinState) rememberReply(id string) {
	if slices.Contains(state.RepliedThreadIDs, id) {
		return
	}
	state.RepliedThreadIDs = append(state.RepliedThreadIDs, id)
	if len(state.RepliedThreadIDs) > bulletinReplyHistoryLimit {
		state.RepliedThreadIDs = state.RepliedThreadIDs[len(state.RepliedThreadIDs)-bulletinReplyHistoryLimit:]
	}
}

func (r *Runner) bulletinState() (bulletinState, error) {
	var state bulletinState
	if raw := r.snapshot["_bulletin"]; len(raw) > 0 {
		if err := json.Unmarshal(raw, &state); err != nil {
			return state, errors.New("invalid durable bulletin schedule")
		}
	}
	if state.ThreadCursor < 0 || state.ReplyCursor < 0 || len(state.RepliedThreadIDs) > bulletinReplyHistoryLimit {
		return state, errors.New("invalid durable bulletin pool cursor or reply history")
	}
	if state.ReplyScan != nil && (state.ReplyScan.Since.IsZero() || !validBulletinScan(state.ReplyScan.Cursor, state.ReplyScan.Candidates)) {
		return state, errors.New("invalid durable bulletin reply scan")
	}
	return state, nil
}

func (r *Runner) saveBulletinState(ctx context.Context, state bulletinState) error {
	r.snapshot["_bulletin"] = encode(state)
	return r.db.SaveSnapshot(ctx, r.account.ID, r.snapshot)
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
	if !r.opts.BulletinEnabled {
		if mission != nil {
			return true, r.db.DeleteMission(ctx, r.account.ID, mission.ShipID)
		}
		return false, r.decision(ctx, "__bulletin", "bulletin", "public posting disabled by BULLETIN_ENABLED=false", nil)
	}
	if time.Now().Before(state.due()) {
		r.wakeBy(state.due())
		return false, r.decision(ctx, "__bulletin", "bulletin", "public posting waits for the three-real-hour schedule or cooldown", map[string]any{"until": state.due()})
	}
	if mission != nil {
		if data.Bulletin == nil {
			return false, errors.New("bulletin mission has no immutable post intent")
		}
		if !bulletinCuratedIntent(data.Bulletin) {
			return true, r.db.DeleteMission(ctx, r.account.ID, mission.ShipID)
		}
		return true, r.stageBulletin(ctx, mission, data)
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
	var profile struct {
		Company struct {
			ID   string `json:"id"`
			Name string `json:"name"`
		} `json:"company"`
	}
	if json.Unmarshal(r.snapshot["profile"], &profile) != nil || profile.Company.ID != r.account.CompanyID || profile.Company.Name != names.DefaultCompany {
		return false, r.decision(ctx, "__bulletin", "bulletin", "public posting skipped: the recorded company is not The Pokemon Pirates", nil)
	}
	intent := &bulletinIntent{CompanyName: profile.Company.Name, ReplyTurn: state.NextReply}
	intent.NotBefore, _, _ = r.serverBounds()
	if intent.NotBefore.IsZero() {
		return false, errors.New("bulletin staging requires an authoritative server clock")
	}
	requestCtx, cancel := context.WithTimeout(ctx, bulletinRequestBudget)
	defer cancel()
	if state.NextReply {
		ready, err := r.selectBulletinReply(requestCtx, &state, intent)
		if err != nil || !ready {
			return false, err
		}
	} else {
		var page bulletinPage
		if err := r.api.GetOnce(requestCtx, "/v1/bulletin?limit=100", &page); err != nil {
			return false, err
		}
		if page.Threads == nil {
			return false, errors.New("bulletin list omitted its threads")
		}
		for _, thread := range page.Threads {
			if uuid.Validate(thread.ID) == nil && len(intent.BeforeIDs) < bulletinReplyHistoryLimit {
				intent.BeforeIDs = append(intent.BeforeIDs, thread.ID)
			}
		}
	}
	state.ReplyScan = nil
	if intent.ThreadID != "" {
		intent.TemplateIndex = state.ReplyCursor % len(bulletinReplies)
		intent.Body = bulletinReplies[intent.TemplateIndex]
	} else {
		intent.TemplateIndex = state.ThreadCursor % len(bulletinThreads)
		template := bulletinThreads[intent.TemplateIndex]
		intent.Title, intent.Body = template.Title, template.Body
	}
	if err := validBulletinText(intent.Title, intent.Body); err != nil {
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

func (r *Runner) selectBulletinReply(ctx context.Context, state *bulletinState, intent *bulletinIntent) (bool, error) {
	if state.ReplyScan == nil {
		state.ReplyScan = &bulletinReplyScan{Since: intent.NotBefore.Add(-72 * time.Hour)}
	}
	scan := state.ReplyScan
	if len(scan.Candidates) == 0 && !scan.Complete {
		path := "/v1/bulletin?limit=100&since=" + url.QueryEscape(scan.Since.Format(time.RFC3339Nano))
		if scan.Cursor != "" {
			path += "&cursor=" + url.QueryEscape(scan.Cursor)
		}
		var page bulletinPage
		if err := r.api.GetOnce(ctx, path, &page); err != nil {
			return false, err
		}
		if page.Threads == nil || len(page.Threads) > bulletinPageLimit || len(page.NextCursor) > bulletinCursorLimit || page.NextCursor != "" && page.NextCursor == scan.Cursor {
			return false, errors.New("bulletin reply page is incomplete or has an invalid cursor")
		}
		scan.Complete = page.NextCursor == "" || len(page.Threads) > 0 && page.Threads[len(page.Threads)-1].Archived
		scan.Cursor = page.NextCursor
		slices.SortFunc(page.Threads, func(left, right bulletinThread) int {
			if order := right.LastPostAt.Compare(left.LastPostAt); order != 0 {
				return order
			}
			return strings.Compare(left.ID, right.ID)
		})
		for _, thread := range page.Threads {
			if thread.CompanyName == "The Kraken Cartel" && !thread.Archived && !thread.Removed && uuid.Validate(thread.ID) == nil && !slices.Contains(state.RepliedThreadIDs, thread.ID) {
				scan.Candidates = append(scan.Candidates, thread.ID)
			}
			if uuid.Validate(thread.ID) == nil && len(intent.BeforeIDs) < bulletinReplyHistoryLimit {
				intent.BeforeIDs = append(intent.BeforeIDs, thread.ID)
			}
		}
		if err := r.saveBulletinState(ctx, *state); err != nil {
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
			return false, errors.New("bulletin reply observation omitted the selected thread or posts")
		}
		alreadyReplied := false
		for _, post := range detail.Posts {
			alreadyReplied = alreadyReplied || post.CompanyName == intent.CompanyName && !post.Opening
		}
		if !alreadyReplied && !detail.Thread.Archived && !detail.Thread.Removed && detail.Thread.CompanyName == "The Kraken Cartel" {
			intent.ThreadID, intent.BeforeIDs = id, nil
			for _, post := range detail.Posts {
				if post.CompanyName == intent.CompanyName && !post.CreatedAt.Before(intent.NotBefore) && uuid.Validate(post.ID) == nil && len(intent.BeforeIDs) < bulletinReplyHistoryLimit {
					intent.BeforeIDs = append(intent.BeforeIDs, post.ID)
				}
			}
			return true, nil
		}
		scan.Candidates = scan.Candidates[1:]
		if alreadyReplied {
			state.rememberReply(id)
		}
		if err := r.saveBulletinState(ctx, *state); err != nil {
			return false, err
		}
	}
	if scan.Complete {
		return true, nil
	}
	state.CooldownUntil = time.Now().UTC().Add(bulletinRetryDelay)
	if err := r.saveBulletinState(ctx, *state); err != nil {
		return false, err
	}
	return false, r.decision(ctx, "__bulletin", "bulletin", "public reply search saved its next page for a later idle pass", nil)
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
	if !r.opts.BulletinEnabled {
		return errors.New("public posting is disabled (BULLETIN_ENABLED=false)")
	}
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
	if !bulletinCuratedIntent(intent) {
		return errors.New("public post content must come only from the curated company voice")
	}
	return validBulletinText(intent.Title, intent.Body)
}

func bulletinCuratedIntent(intent *bulletinIntent) bool {
	if intent == nil || intent.TemplateIndex < 0 {
		return false
	}
	if intent.ThreadID != "" {
		return intent.TemplateIndex < len(bulletinReplies) && intent.Title == "" && intent.Body == bulletinReplies[intent.TemplateIndex]
	}
	return intent.TemplateIndex < len(bulletinThreads) && intent.Title == bulletinThreads[intent.TemplateIndex].Title && intent.Body == bulletinThreads[intent.TemplateIndex].Body
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

func (r *Runner) consumeBulletinResult(ctx context.Context, mission *store.Mission, data *missionData, status int, response json.RawMessage) (bool, error) {
	if data.Bulletin == nil {
		return false, errors.New("bulletin result has no durable company intent")
	}
	if status >= 400 && status < 500 {
		state, err := r.bulletinState()
		if err != nil {
			return false, err
		}
		if state.LastOutcomeID != data.Action.ID {
			var problem struct {
				Code   string `json:"code"`
				Detail string `json:"detail"`
			}
			_ = json.Unmarshal(response, &problem)
			state.CooldownUntil = bulletinCooldown(status, problem.Code, problem.Detail, data.CooldownUntil, time.Now().UTC())
			state.LastOutcomeID = data.Action.ID
			if err := r.saveBulletinState(ctx, state); err != nil {
				return false, err
			}
		}
		if err := r.decision(ctx, "__bulletin", "bulletin", "public post refused; company posting cooldown recorded without delaying trade", map[string]any{"status": status, "until": state.CooldownUntil}); err != nil {
			return false, err
		}
		data.CooldownUntil = state.CooldownUntil
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
		state.LastOutcomeID, state.LastPostID, state.LastPostAt = data.Action.ID, post.ID, post.CreatedAt
		state.NextDue = maxTime(time.Now().UTC(), post.CreatedAt).Add(bulletinInterval)
		state.CooldownUntil = time.Time{}
		state.ReplyScan = nil
		state.NextReply = !intent.ReplyTurn
		if intent.ThreadID == "" {
			state.ThreadCursor = (intent.TemplateIndex + 1) % len(bulletinThreads)
		} else {
			state.ReplyCursor = (intent.TemplateIndex + 1) % len(bulletinReplies)
			state.rememberReply(intent.ThreadID)
		}
		if err := r.saveBulletinState(ctx, state); err != nil {
			return false, err
		}
	}
	if err := r.decision(ctx, "__bulletin", "bulletin", "public company post confirmed; next three-real-hour slot recorded", map[string]any{"post_id": post.ID, "thread_id": post.ThreadID, "posted_at": post.CreatedAt, "next_due": state.NextDue}); err != nil {
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
		if !bulletinCuratedIntent(intent) {
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
