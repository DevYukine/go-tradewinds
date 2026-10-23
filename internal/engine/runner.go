package engine

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"math/rand/v2"
	"sync/atomic"
	"time"

	"github.com/DevYukine/go-tradewinds/internal/gameapi"
	"github.com/DevYukine/go-tradewinds/internal/store"
	"github.com/google/uuid"
	"go.uber.org/zap"
)

type Options struct {
	Execute        bool
	ReservePence   int64
	MaxPaybackDays float64
}

type decisionFingerprint struct {
	hash      [sha256.Size]byte
	writtenAt time.Time
}

type Runner struct {
	api                *gameapi.Client
	db                 *store.Store
	account            store.Account
	log                *zap.Logger
	opts               Options
	snapshot           map[string]json.RawMessage
	meta               map[string]DatasetMeta
	stateCache         accountState
	generation         string
	holding            []string
	decisionHashes     map[string]decisionFingerprint
	lastMutationStatus atomic.Int64
	expenseOwners      map[string]string
	nextDeadline       time.Time
	publishedFinancial atomic.Uint64
	capturedFinancial  uint64
	financialSession   string
}

func New(api *gameapi.Client, db *store.Store, account store.Account, log *zap.Logger, opts Options) *Runner {
	if log == nil {
		log = zap.NewNop()
	}
	if opts.ReservePence <= 0 {
		opts.ReservePence = 75_000
	}
	if opts.MaxPaybackDays <= 0 {
		opts.MaxPaybackDays = 12
	}
	return &Runner{
		api:              api,
		db:               db,
		account:          account,
		log:              log.With(zap.String("account_id", account.ID)),
		opts:             opts,
		snapshot:         make(map[string]json.RawMessage),
		meta:             make(map[string]DatasetMeta),
		decisionHashes:   make(map[string]decisionFingerprint),
		holding:          make([]string, 0),
		financialSession: uuid.NewString(),
	}
}

type streamMessage struct {
	event        *gameapi.Event
	err          error
	closed       bool
	controlEpoch uint64
	// financialEpoch marks a company cash movement published after the
	// enqueue decision, without stopping the stream.
	financialEpoch uint64
}

// financialEvents are the official Money events (company scope): each one
// moves vault cash, so a REST capture can read balances that predate it.
var financialEvents = map[string]bool{
	"company_upkeep_billed":           true,
	"auto_remittance":                 true,
	"tax_and_fee_charged":             true,
	"crown_bailiff_warehouse_seizure": true,
	"crown_bailiff_upkeep_seizure":    true,
	"company_bankruptcy_reset":        true,
}

// Run owns the account's one mutation loop. SSE only invalidates REST state;
// historical events never themselves trigger a historical trade.
func (r *Runner) Run(ctx context.Context) error {
	if r.api == nil || r.db == nil || r.account.ID == "" || r.account.CompanyID == "" {
		return errors.New("engine requires an API client, store and company-bound account")
	}
	owner, release, err := r.db.AcquireAccountContext(ctx, r.account.ID)
	if err != nil {
		return err
	}
	defer release()
	ctx = owner
	r.api.SetObserver(func(method, path string, status int, duration time.Duration, request, response []byte, errText string) {
		if method != "GET" {
			r.lastMutationStatus.Store(int64(status))
		}
		if err := r.db.RecordRequest(ctx, r.account.ID, method, path, status, duration, request, response, errText); err != nil && ctx.Err() == nil {
			r.log.Warn("record request", zap.Error(err))
		}
	})
	snapshot, err := r.db.Snapshot(ctx, r.account.ID)
	if err != nil {
		return err
	}
	if snapshot != nil {
		r.snapshot = snapshot
		r.meta = decode[map[string]DatasetMeta](snapshot, "_meta")
		if r.meta == nil {
			r.meta = make(map[string]DatasetMeta)
		}
	}
	r.expenseOwners = decode[map[string]string](r.snapshot, "_expense_allocations")
	if r.expenseOwners == nil {
		r.expenseOwners = make(map[string]string)
	}
	messages := make(chan streamMessage, 256)
	var publishedControl atomic.Uint64
	var handledControl uint64
	// Financial events publish before enqueue like controls, so a barrier
	// behind a full ordinary queue still vetoes work against an earlier
	// REST capture; unlike controls they leave the stream open.
	publishedFinancial := &r.publishedFinancial
	var handledFinancial uint64
	var stopStream context.CancelFunc
	startStream := func() error {
		cursor, err := r.db.Cursor(ctx, r.account.ID)
		if err != nil {
			return err
		}
		reconnecting := stopStream != nil
		streamContext, cancel := context.WithCancel(ctx)
		stopStream = cancel
		go func() {
			defer cancel()
			err := r.api.Stream(streamContext, cursor, func(event gameapi.Event) error {
				message := streamMessage{event: &event}
				if event.Type == "gap" || event.Type == "auth_revoked" || event.Type == "auth_expired" {
					// Publish before enqueue so a control behind a full ordinary
					// queue still prevents work against an earlier REST capture.
					message.controlEpoch = publishedControl.Add(1)
				} else if financialEvents[event.Type] {
					// Publish before enqueue so spending cannot start from vault
					// balances captured before this cash movement; unlike a
					// control the stream stays open and ordinary events keep
					// flowing.
					message.financialEpoch = publishedFinancial.Add(1)
				} else if reconnecting {
					r.log.Info("event stream reconnected")
					reconnecting = false
				}
				select {
				case messages <- message:
				case <-streamContext.Done():
					return streamContext.Err()
				}
				if message.controlEpoch != 0 {
					return errors.New("stream control: " + event.Type)
				}
				return nil
			})
			message := streamMessage{closed: true, err: err, controlEpoch: publishedControl.Add(1)}
			select {
			case messages <- message:
			case <-streamContext.Done():
			}
		}()
		return nil
	}
	defer func() {
		if stopStream != nil {
			stopStream()
		}
	}()
	// Subscribe before the initial authoritative capture so invalidations that
	// happen during a multi-page read remain queued rather than being lost.
	if err := startStream(); err != nil {
		return err
	}
	timer := time.NewTimer(0)
	defer timer.Stop()
	backoff := time.Second
	var reconnectAt time.Time
	handleMessage := func(message streamMessage) error {
		if message.controlEpoch != 0 {
			handledControl = message.controlEpoch
		}
		if message.financialEpoch != 0 {
			handledFinancial = message.financialEpoch
		}
		if message.closed {
			var upstream *gameapi.APIError
			if errors.As(message.err, &upstream) && (upstream.Status == 401 || upstream.Status == 403) {
				return message.err
			}
			jitter := time.Duration(rand.Int64N(int64(backoff/2 + 1)))
			reconnectAt = time.Now().Add(backoff + jitter)
			backoff = min(30*time.Second, backoff*2)
			if message.err != nil {
				r.log.Warn("event stream disconnected; REST resync required", zap.Error(message.err))
			}
			return nil
		}
		if message.event == nil {
			return nil
		}
		event := message.event
		if event.Type == "auth_revoked" || event.Type == "auth_expired" {
			return fmt.Errorf("account stream authorization ended: %s", event.Type)
		}
		if event.Type == "gap" {
			if err := r.db.ResetCursor(ctx, r.account.ID); err != nil {
				return err
			}
			r.log.Warn("event stream cursor gap; cursor reset and full REST resync required")
			return r.decision(ctx, "", "stream_gap", "rejected cursor reset to 0; full REST resynchronization precedes any mutation", event.Data)
		}
		if event.ID != "" {
			if err := r.db.RecordEvent(ctx, r.account.ID, event.ID, event.Type, event.Data); err != nil {
				return err
			}
			backoff = time.Second
		}
		return nil
	}
	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case message := <-messages:
			if err := handleMessage(message); err != nil {
				return err
			}
		case <-timer.C:
		}
		// Coalesce only the current queue; a busy stream must not starve REST.
		// Each event/cursor is still durably recorded in frame order.
		for pending := len(messages); pending > 0; pending-- {
			if err := handleMessage(<-messages); err != nil {
				return err
			}
		}
		if ctx.Err() != nil {
			return ctx.Err()
		}
		if !reconnectAt.IsZero() && !time.Now().Before(reconnectAt) {
			if err := startStream(); err != nil {
				return err
			}
			reconnectAt = time.Time{}
		}
		r.generation = uuid.NewString()
		captureControl := handledControl
		captureFinancial := handledFinancial
		r.capturedFinancial = captureFinancial
		if err := r.refresh(ctx); err != nil {
			return err
		}
		pending := len(messages)
		refreshPending := pending > 0
		for ; pending > 0; pending-- {
			if err := handleMessage(<-messages); err != nil {
				return err
			}
		}
		// Ordinary events request another capture without vetoing safe work.
		// Controls remain barriers even when still queued behind ordinary events.
		if captureControl != handledControl || handledControl != publishedControl.Load() {
			timer.Reset(0)
			continue
		}
		// A financial event published or handled during this capture means the
		// captured vault balances predate the cash movement: spending waits
		// for the next authoritative refresh while observation, the stream and
		// ordinary events continue.
		financialStale := captureFinancial != handledFinancial || handledFinancial != publishedFinancial.Load()
		r.holding = r.holding[:0]
		changed := false
		identityReady := r.fresh("whoami") && r.fresh("profile") && r.fresh("player")
		if financialStale {
			if err := r.hold(ctx, "", "financial event invalidated the captured vault balances; authoritative resynchronization precedes new spending"); err != nil {
				return err
			}
		} else if !identityReady {
			if err := r.hold(ctx, "", "identity datasets are stale or failed; observation continues and all mutations are disabled"); err != nil {
				return err
			}
		} else {
			if err := r.verifyIdentity(); err != nil {
				return err
			}
			var err error
			changed, err = r.work(ctx)
			if err != nil {
				return err
			}
		}
		mode := "observe"
		if r.opts.Execute {
			mode = "execute"
		}
		r.snapshot["_engine"] = encode(map[string]any{"mode": mode, "holding_reasons": r.holding, "generation": r.generation})
		if err := r.db.SaveSnapshot(ctx, r.account.ID, r.snapshot); err != nil {
			return err
		}
		financialStale = financialStale || captureFinancial != publishedFinancial.Load()
		delay := r.nextWake()
		if changed {
			delay = 100 * time.Millisecond
		}
		if refreshPending || financialStale {
			delay = 0
		}
		if !reconnectAt.IsZero() {
			delay = min(delay, max(0, time.Until(reconnectAt)))
		}
		if !timer.Stop() {
			select {
			case <-timer.C:
			default:
			}
		}
		timer.Reset(delay)
	}
}

func (r *Runner) verifyIdentity() error {
	if !r.fresh("whoami") || !r.fresh("profile") || !r.fresh("player") {
		return errors.New("cannot verify company-bound credentials from fresh authoritative profiles")
	}
	var who struct {
		CompanyID string `json:"company_id"`
		PlayerID  string `json:"player_id"`
	}
	var profile struct {
		Company struct {
			ID       string `json:"id"`
			PlayerID string `json:"player_id"`
		} `json:"company"`
	}
	var player struct {
		Player struct {
			ID string `json:"id"`
		} `json:"player"`
	}
	if err := json.Unmarshal(r.snapshot["whoami"], &who); err != nil {
		return err
	}
	if err := json.Unmarshal(r.snapshot["profile"], &profile); err != nil {
		return err
	}
	if err := json.Unmarshal(r.snapshot["player"], &player); err != nil {
		return err
	}
	if who.CompanyID != r.account.CompanyID || profile.Company.ID != r.account.CompanyID ||
		who.PlayerID == "" || who.PlayerID != player.Player.ID || who.PlayerID != profile.Company.PlayerID {
		return errors.New("stored account company or player does not match authenticated API identity")
	}
	return nil
}

func (r *Runner) fresh(dataset string) bool {
	return (PlanningInput{Now: time.Now(), Clock: r.stateCache.Clock, Metadata: r.meta, Generation: r.generation}).fresh(dataset)
}

func (r *Runner) nextWake() time.Duration {
	now := time.Now()
	delay := 15 * time.Second
	lower, upper, ok := serverBounds(now, r.stateCache.Clock, r.meta["clock"], r.generation)
	if ok {
		for _, ship := range r.stateCache.Ships {
			if ship.Ship.TurnaroundUnlocksAt != nil && ship.Ship.TurnaroundUnlocksAt.After(lower) {
				delay = min(delay, ship.Ship.TurnaroundUnlocksAt.Sub(lower))
			}
			if ship.Voyage != nil && ship.Voyage.EstimatedArrivalAt.After(upper) {
				delay = min(delay, ship.Voyage.EstimatedArrivalAt.Sub(upper))
			}
		}
	}
	if r.nextDeadline.After(now) {
		delay = min(delay, r.nextDeadline.Sub(now))
	}
	return max(100*time.Millisecond, delay)
}

func (r *Runner) wakeBy(deadline time.Time) {
	if deadline.After(time.Now()) && (r.nextDeadline.IsZero() || deadline.Before(r.nextDeadline)) {
		r.nextDeadline = deadline
	}
}

func (r *Runner) decision(ctx context.Context, shipID, kind, reason string, details any) error {
	const retention = 7 * 24 * time.Hour
	key := shipID + "|" + kind
	fingerprintDetails := details
	if plan, ok := details.(Plan); ok && plan.Kind == "relocation" {
		// A rolling ETA alone is not a new economic decision. Keep it in the
		// stored plan, but do not let it defeat diagnostic deduplication.
		plan.ExpectedArrival = time.Time{}
		fingerprintDetails = plan
	}
	digest := sha256.New()
	digest.Write([]byte(reason))
	digest.Write([]byte{0})
	digest.Write(encode(fingerprintDetails))
	var fingerprint [sha256.Size]byte
	digest.Sum(fingerprint[:0])
	now := time.Now()
	previous, exists := r.decisionHashes[key]
	if exists && previous.hash == fingerprint && now.Sub(previous.writtenAt) < retention {
		return nil
	}
	if err := r.db.RecordDecision(ctx, r.account.ID, shipID, kind, reason, details); err != nil {
		return err
	}
	r.rememberDecision(key, fingerprint, now)
	return nil
}

func (r *Runner) rememberDecision(key string, fingerprint [sha256.Size]byte, now time.Time) {
	const capacity = 2048
	const retention = 7 * 24 * time.Hour
	_, exists := r.decisionHashes[key]
	if !exists && len(r.decisionHashes) >= capacity {
		var oldestKey string
		var oldestTime time.Time
		for cachedKey, cached := range r.decisionHashes {
			if now.Sub(cached.writtenAt) >= retention {
				delete(r.decisionHashes, cachedKey)
			} else if oldestKey == "" || cached.writtenAt.Before(oldestTime) {
				oldestKey, oldestTime = cachedKey, cached.writtenAt
			}
		}
		if len(r.decisionHashes) >= capacity {
			delete(r.decisionHashes, oldestKey)
		}
	}
	r.decisionHashes[key] = decisionFingerprint{hash: fingerprint, writtenAt: now}
}

func (r *Runner) hold(ctx context.Context, shipID, reason string) error {
	r.holding = append(r.holding, reason)
	return r.decision(ctx, shipID, "hold", reason, nil)
}

func (r *Runner) planningInput(ship ShipState) PlanningInput {
	return r.planningInputFrom(ship, r.stateCache)
}

func (r *Runner) planningInputFrom(ship ShipState, state accountState) PlanningInput {
	balances := make(map[string]int64)
	reserves := make(map[string]int64)
	for _, vault := range state.Vaults {
		if vault.AccountType == "vault" {
			balances[vault.BankingRegion] += vault.BalancePence
			reserves[vault.BankingRegion] = r.opts.ReservePence
		}
	}
	upkeep := DailyUpkeep(state.Ships, state.Catalog, len(state.Factors))
	if home, exists := findPort(state.Catalog, state.HomePortID); exists {
		reserves[home.BankingRegion] = r.opts.ReservePence + 3*upkeep
	}
	return PlanningInput{
		Now:                   time.Now(),
		CompanyID:             r.account.CompanyID,
		Ship:                  ship,
		Catalog:               state.Catalog,
		Clock:                 state.Clock,
		Zones:                 decode[[]ZoneObservation](r.snapshot, "zones"),
		Markets:               state.Markets,
		Ports:                 state.Ports,
		Routes:                state.Routes,
		Contracts:             state.Contracts,
		Balances:              balances,
		ReserveByRegion:       reserves,
		ReservePence:          r.opts.ReservePence,
		Metadata:              r.meta,
		Generation:            r.generation,
		FleetDailyUpkeepPence: upkeep,
		Rates:                 state.Rates,
	}
}
