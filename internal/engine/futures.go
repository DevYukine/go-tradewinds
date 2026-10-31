package engine

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/DevYukine/go-tradewinds/internal/gameapi"
	"github.com/DevYukine/go-tradewinds/internal/store"
	"go.uber.org/zap"
)

const futuresPilotCohort = "amsterdam-water-beer-owner-pilot"

var errFuturesEvidence = errors.New("invalid futures evidence")
var errFuturesPrecondition = errors.New("transient futures precondition")
var errFuturesTerms = errors.New("invalid immutable futures terms or cap")

// RearmUnusedFuturesPilot is an explicit operator action, never a restart rule.
// The permanent cohort and sequence survive; any admitted future forbids re-arm.
func RearmUnusedFuturesPilot(ctx context.Context, db *store.Store, accountID string) error {
	ctx, release, err := db.AcquireAccountContext(ctx, accountID)
	if err != nil {
		return err
	}
	defer release()
	mission, err := db.GetMission(ctx, accountID, "__futures")
	if err != nil {
		return err
	}
	if mission == nil || mission.Generation != futuresPilotCohort || mission.Phase != "futures_done" {
		return fmt.Errorf("futures re-arm requires a completed pilot wind-down: %w", store.ErrActionConflict)
	}
	var data missionData
	if err := json.Unmarshal(mission.Data, &data); err != nil {
		return err
	}
	pilot := data.Futures
	if pilot == nil || data.Action != nil || data.Sequence <= 0 ||
		pilot.IntentID != "" || pilot.ProcurementPaid != 0 || pilot.ProcurementUnknown ||
		pilot.LiquidatedTons != 0 || pilot.LiquidationPaid != 0 || pilot.LiquidationUnknown {
		return fmt.Errorf("futures re-arm requires unused, known-zero pilot stock and no staged action: %w", store.ErrActionConflict)
	}
	rows, err := db.FuturesIntents(ctx, accountID)
	if err != nil {
		return err
	}
	if len(rows) != 0 {
		return fmt.Errorf("futures re-arm is forbidden after any futures admission: %w", store.ErrActionConflict)
	}
	pending, err := db.PendingActions(ctx, accountID)
	if err != nil {
		return err
	}
	if tradingActionsPending(pending) {
		return fmt.Errorf("futures re-arm requires pending trading actions to reconcile first: %w", store.ErrActionConflict)
	}
	data.Futures = &futuresPilotIntent{}
	data.CooldownUntil = time.Time{}
	mission.Phase = "futures_pilot"
	mission.Data, err = json.Marshal(data)
	if err != nil {
		return err
	}
	return db.PutMission(ctx, mission)
}

func (r *Runner) refreshFuturesCredit(ctx context.Context) error {
	lower, _, _ := serverBounds(time.Now(), decode[Clock](r.snapshot, "clock"), r.meta["clock"], r.generation)
	reserved, awaited, err := r.db.ReserveFuturesCreditRead(ctx, r.account.ID, lower)
	if err == nil && !awaited {
		r.futuresCreditUnavailable = false
	}
	if err == nil && reserved {
		var report json.RawMessage
		err = r.api.GetOnce(ctx, "/v1/loan/credit?company_id="+url.QueryEscape(r.account.CompanyID), &report)
		if err == nil {
			err = r.db.RecordFuturesCredit(ctx, r.account.ID, report)
			if err == nil {
				r.futuresCreditUnavailable = false
			}
		}
	}
	if err != nil {
		r.futuresCreditUnavailable = true
		if ctx.Err() != nil {
			return ctx.Err()
		}
		if entry := r.log.Check(zap.WarnLevel, "futures own-credit observation failed"); entry != nil {
			entry.Write(r.scrubOutcomeFields(ctx, nil, []outcomeText{{field: "cause", value: err.Error()}})...)
		}
	}
	return ctx.Err()
}

// A single permanent cohort is deliberate. Neither a profitable receipt nor
// disappearing list history authorizes another pair or a larger lot.
type futuresPilotIntent struct {
	IntentID           string            `json:"intent_id"`
	LeaseOwned         bool              `json:"lease_owned"`
	SetupAttempted     bool              `json:"setup_attempted"`
	ProcurementPaid    int64             `json:"procurement_paid_pence"`
	InitialLease       int64             `json:"initial_lease_tons"`
	LiquidatedTons     int64             `json:"liquidated_tons"`
	LiquidationPaid    int64             `json:"liquidation_paid_pence"`
	RejectedEvidence   map[string]string `json:"rejected_evidence,omitempty"`
	ProcurementUnknown bool              `json:"procurement_cash_unknown,omitempty"`
	LiquidationUnknown bool              `json:"liquidation_cash_unknown,omitempty"`
}

type futuresWarehouse struct {
	PortID          string `json:"port_id"`
	LeasedTonnage   int64  `json:"leased_tonnage"`
	ReservedTonnage int64  `json:"reserved_tonnage"`
	UsedTonnage     int64  `json:"used_tonnage"`
	FreeTonnage     int64  `json:"free_tonnage"`
	Inventory       []struct {
		CompanyID      string `json:"company_id"`
		PortID         string `json:"port_id"`
		CommodityID    string `json:"commodity_id"`
		Quantity       int64  `json:"quantity"`
		LockedQuantity int64  `json:"locked_quantity"`
	} `json:"inventory"`
}

func (r *Runner) futuresWarehouse() (futuresWarehouse, int64) {
	for _, warehouse := range decode[[]futuresWarehouse](r.snapshot, "warehouses") {
		if warehouse.PortID != "amsterdam" {
			continue
		}
		var unlocked int64
		for _, item := range warehouse.Inventory {
			if item.CompanyID == r.account.CompanyID && item.PortID == "amsterdam" && item.CommodityID == "water_beer" {
				unlocked += max(0, item.Quantity-item.LockedQuantity)
			}
		}
		return warehouse, unlocked
	}
	return futuresWarehouse{PortID: "amsterdam"}, 0
}

func futuresNPCPrices(market Market) (bid, ask int64) {
	for _, book := range market.Books {
		if book.CommodityID != "water_beer" || book.Depth == nil {
			continue
		}
		for _, level := range book.Depth.Bids {
			if level.IsNPC && level.RemainingQuantity > 0 && level.PricePence > bid {
				bid = level.PricePence
			}
		}
		for _, level := range book.Depth.Asks {
			if level.IsNPC && level.RemainingQuantity > 0 && level.PricePence > 0 && (ask == 0 || level.PricePence < ask) {
				ask = level.PricePence
			}
		}
	}
	return bid, ask
}

func (r *Runner) futuresAuthority() error {
	if _, err := r.futuresContracts(); err != nil {
		return err
	}
	for _, key := range []string{"catalog", "clock", "vaults", "warehouses", "futures", "ledger"} {
		if !r.fresh(key) {
			return fmt.Errorf("futures pilot requires current successful %s authority", key)
		}
	}
	if _, _, ok := r.serverBounds(); !ok || r.stateCache.Clock.Paused || r.stateCache.Clock.Calendar.TimeScale <= 0 {
		return errors.New("futures pilot requires unpaused server clock authority")
	}
	port, exists := findPort(r.stateCache.Catalog, "amsterdam")
	if !exists || !port.IsHub || port.BankingRegion != "north_sea" {
		return errors.New("futures pilot requires the Amsterdam North Sea hub")
	}
	return nil
}

func (r *Runner) futuresFreeCash(ctx context.Context, own *store.FuturesIntent) (int64, error) {
	input, err := r.committedPlanningInput(ctx, ShipState{})
	if err != nil {
		return 0, err
	}
	cash := input.Balances["north_sea"] - input.ReserveByRegion["north_sea"]
	if own != nil && !store.FuturesTerminal(own.Status) {
		cash += own.ReservedCashPence
		if own.FuturesID != "" {
			cash -= own.MarginPence
		}
	}
	return cash, nil
}

func futuresCreateBody(intent store.FuturesIntent) any {
	return struct {
		PortID           string `json:"port_id"`
		CommodityID      string `json:"commodity_id"`
		Side             string `json:"side"`
		LotTons          int64  `json:"lot_tons"`
		StrikePricePence int64  `json:"strike_price_pence"`
		MaturityHours    int64  `json:"matures_in_game_hours"`
	}{intent.PortID, intent.CommodityID, intent.Side, intent.LotTons, intent.StrikePricePence, 30}
}

func (r *Runner) futuresErrorHold(ctx context.Context, changed *bool, err *error) {
	if *err == nil || ctx.Err() != nil || !errors.Is(*err, errFuturesEvidence) {
		return
	}
	reason := "futures pilot evidence held: " + (*err).Error()
	if entry := r.log.Check(zap.WarnLevel, "optional futures pilot held"); entry != nil {
		entry.Write(r.scrubOutcomeFields(ctx, nil, []outcomeText{{field: "cause", value: reason}})...)
	}
	if holdErr := r.hold(ctx, "__futures", reason); holdErr != nil {
		*err = holdErr
		return
	}
	*changed, *err = false, nil
}

// futuresAccounted reports whether an intent's cash accounting is final. A
// terminal status alone is not: cash flows are recorded after it.
func futuresAccounted(row store.FuturesIntent) bool {
	return store.FuturesTerminal(row.Status) && (row.RealizedPnLPence != nil ||
		row.PnLBasis == "opening or remaining stock cost unknown; rent unknown" ||
		row.PnLBasis == "procurement or liquidation cash unknown; rent unknown")
}

func (r *Runner) driveFutures(ctx context.Context, mission *store.Mission, data *missionData) (changed bool, err error) {
	defer r.futuresErrorHold(ctx, &changed, &err)
	rows, err := r.db.FuturesIntents(ctx, r.account.ID)
	if err != nil {
		return false, err
	}
	accounted := true
	for _, row := range rows {
		accounted = accounted && futuresAccounted(row)
	}
	if mission != nil && mission.Phase == "futures_done" && accounted {
		return false, nil
	}
	if err := r.observeFutures(ctx, rows); err != nil {
		return false, err
	}
	rows, err = r.db.FuturesIntents(ctx, r.account.ID)
	if err != nil {
		return false, err
	}
	if data.Action != nil {
		return false, nil
	}
	if err := r.futuresAuthority(); err != nil {
		return false, r.hold(ctx, "__futures", err.Error())
	}
	warehouse, unlocked := r.futuresWarehouse()
	if warehouse.LeasedTonnage > 2 {
		return false, r.hold(ctx, "__futures", "futures pilot requires operator reconciliation: Amsterdam lease exceeds its 2t limit")
	}
	if mission == nil {
		if !r.opts.FuturesPilotEnabled {
			return false, nil
		}
		mission = &store.Mission{AccountID: r.account.ID, ShipID: "__futures", Generation: futuresPilotCohort, Phase: "futures_pilot"}
		data = &missionData{Futures: &futuresPilotIntent{}, StartedAt: time.Now().UTC()}
	}
	if data.Futures == nil {
		return false, errors.New("futures mission is missing its permanent pilot identity")
	}
	if err := r.recordFuturesPnL(ctx, mission, data); err != nil {
		return false, err
	}
	if mission.Phase == "futures_done" {
		return false, nil
	}
	if time.Now().Before(data.CooldownUntil) {
		return false, r.hold(ctx, "__futures", "futures pilot waits for its persisted retry cooldown")
	}
	lower, _, _ := r.serverBounds()
	// Physical obligations outrank every discretionary admission, including
	// an admission restored after a crash before its mission was saved.
	for _, row := range rows {
		if row.CohortID != futuresPilotCohort || row.Status != "active" || row.ObservedMaturesAt == nil {
			continue
		}
		r.wakeAtServer(*row.ObservedMaturesAt, true)
		if lower.Before(*row.ObservedMaturesAt) {
			continue
		}
		if _, exists := r.listedFuture(row.FuturesID); !exists {
			continue
		}
		data.Futures.IntentID = row.IntentID
		refused, err := r.futuresRefused(ctx, data, "futures-settle")
		if err != nil {
			return false, err
		}
		if refused {
			continue
		}
		if err := r.stage(ctx, mission, data, ShipState{}, "futures-settle", "/v1/future/settle", map[string]string{"futures_id": row.FuturesID}); err != nil {
			return false, err
		}
		return data.Action != nil, nil
	}
	for i := range rows {
		expired := !lower.Before(rows[i].MaturesAt)
		if mission.Phase != "futures_stopped" && r.opts.FuturesPilotEnabled && !expired {
			continue
		}
		if rows[i].Status != "intent" || rows[i].FuturesID != "" {
			continue
		}
		rejected, err := r.db.RejectUnsentFuturesIntent(ctx, r.account.ID, rows[i].IntentID)
		if err != nil {
			return false, err
		}
		if rejected {
			rows[i].Status = "rejected"
		}
	}
	terminal := len(rows) > 0 || mission.Phase == "futures_stopped" || !r.opts.FuturesPilotEnabled
	for _, row := range rows {
		terminal = terminal && store.FuturesTerminal(row.Status)
	}
	if terminal {
		return r.closeFuturesWarehouse(ctx, mission, data, rows)
	}
	if mission.Phase == "futures_stopped" || !r.opts.FuturesPilotEnabled {
		return false, r.hold(ctx, "__futures", "futures admissions stopped; existing obligations still reconcile, settle and wind down")
	}
	for _, row := range rows {
		if row.CohortID != futuresPilotCohort || row.Status != "intent" {
			continue
		}
		data.Futures.IntentID = row.IntentID
		exists, err := r.db.HasAction(ctx, r.account.ID, row.ActionID)
		if err != nil {
			return false, err
		}
		if !exists {
			body := encode(futuresCreateBody(row))
			probe := *data
			probe.Action = &durableAction{ID: row.ActionID, Stage: "futures-create", Method: http.MethodPost, Path: "/v1/future/create", Body: body}
			if err := r.futuresPreflight(ctx, &probe); err != nil {
				if errors.Is(err, errFuturesTerms) {
					if err := r.db.RecordFuturesOutcome(ctx, r.account.ID, row.IntentID, "rejected", encode(map[string]string{"kind": "never_sent_futures_terms"})); err != nil {
						return false, err
					}
					mission.Phase = "futures_stopped"
					return true, r.saveMission(ctx, mission, data)
				}
				if errors.Is(err, errFuturesPrecondition) {
					return false, r.hold(ctx, "__futures", err.Error())
				}
				return false, err
			}
		}
		if err := r.stage(ctx, mission, data, ShipState{}, "futures-create", "/v1/future/create", futuresCreateBody(row)); err != nil {
			return false, err
		}
		return data.Action != nil, nil
	}
	if !r.fresh("books/amsterdam") || !r.fresh("ports/amsterdam") {
		return false, r.hold(ctx, "__futures", "new futures admissions wait for a current Amsterdam NPC book; existing contracts still reconcile")
	}
	if warehouse.LeasedTonnage < 2 {
		if len(rows) != 0 || data.Futures.SetupAttempted {
			return false, r.hold(ctx, "__futures", "futures pilot requires two covered warehouse tons; no repeated setup")
		}
		// Do not create a factor or redirect a ship for an experiment. The
		// first billing cycle measures rent; no published numeric tariff exists.
		presence := false
		for _, port := range r.stateCache.Factors {
			presence = presence || port == "amsterdam"
		}
		for _, ship := range r.stateCache.Ships {
			presence = presence || ship.Ship.CurrentPortID == "amsterdam" && (ship.Ship.Status == "berthed" || ship.Ship.Status == "anchored_roadstead")
		}
		cash, err := r.futuresFreeCash(ctx, nil)
		if err != nil {
			return false, err
		}
		if !presence || cash < store.FuturesExposureLimitPence {
			return false, r.hold(ctx, "__futures", "futures setup waits for ordinary Amsterdam presence and a spare 300 Cr allowance")
		}
		data.Futures.SetupAttempted = true
		data.Futures.InitialLease = warehouse.LeasedTonnage
		err = r.stage(ctx, mission, data, ShipState{}, "futures-lease", "/v1/warehouse/set-lease", map[string]any{"port_id": "amsterdam", "target_leased_tonnage": 2})
		if data.Action == nil {
			data.Futures.SetupAttempted = false
		}
		return data.Action != nil, err
	}
	if data.Futures.LeaseOwned && data.Futures.ProcurementPaid == 0 && unlocked == 0 && len(rows) == 0 {
		_, ask := futuresNPCPrices(r.stateCache.Markets["amsterdam"])
		market := r.stateCache.Markets["amsterdam"]
		if ask <= 0 || ask > store.FuturesExposureLimitPence || market.CustomsBPS < 0 || market.CustomsBPS > 10000 {
			return false, r.hold(ctx, "__futures", "futures procurement requires bounded NPC prices and customs")
		}
		cost := ask + ceilBPS(ask, market.CustomsBPS)
		cash, err := r.futuresFreeCash(ctx, nil)
		if err != nil {
			return false, err
		}
		if cost > store.FuturesExposureLimitPence || cash < store.FuturesExposureLimitPence || warehouse.FreeTonnage < 2 {
			return false, r.hold(ctx, "__futures", "futures procurement lacks bounded cash or two free warehouse tons")
		}
		err = r.stage(ctx, mission, data, ShipState{}, "futures-stock", "/v1/market/place-order", map[string]any{
			"port_id": "amsterdam", "commodity_id": "water_beer", "side": "bid", "time_in_force": "ioc", "quantity": 1, "price_pence": ask,
		})
		return data.Action != nil, err
	}
	bid, ask := futuresNPCPrices(r.stateCache.Markets["amsterdam"])
	if bid <= 0 || ask < bid || ask > store.FuturesExposureLimitPence {
		return false, r.hold(ctx, "__futures", "futures pilot has no coherent NPC midpoint proxy")
	}
	mid := (bid + ask) / 2
	market := r.stateCache.Markets["amsterdam"]
	if market.CustomsBPS < 0 || market.CustomsBPS > 10000 {
		return false, r.hold(ctx, "__futures", "futures pilot customs sensitivity is unavailable")
	}
	seen := make(map[string]bool)
	var reservedTons, reservedSpace int64
	for _, row := range rows {
		seen[row.Side] = true // Even terminal or never-listed admissions prohibit replacement.
		if !store.FuturesTerminal(row.Status) {
			reservedTons += row.ReservedTons
			if row.Side == "bid" {
				reservedSpace++
			}
		}
	}
	for _, side := range []string{"ask", "bid"} {
		if seen[side] || side == "ask" && unlocked-reservedTons < 1 || side == "bid" && warehouse.FreeTonnage-reservedSpace < 1 {
			continue
		}
		strike := mid * 85 / 100
		if side == "bid" {
			strike = mid * 115 / 100
		}
		margin := strike / 4
		cash := margin
		tons := int64(1)
		if side == "bid" {
			tons = 0
			cash += strike + ceilBPS(strike, market.CustomsBPS)
		}
		free, err := r.futuresFreeCash(ctx, nil)
		if err != nil {
			return false, err
		}
		if cash > free || cash > store.FuturesExposureLimitPence || strike <= 0 {
			continue
		}
		exposure := cash
		if side == "ask" {
			exposure += ask + ceilBPS(ask, market.CustomsBPS)
		}
		if exposure > store.FuturesExposureLimitPence {
			continue
		}
		intent := store.FuturesIntent{AccountID: r.account.ID, IntentID: futuresPilotCohort + "/" + side,
			ActionID: ActionID(r.account.ID, mission.Generation, "futures-create", data.Sequence), CohortID: futuresPilotCohort,
			PortID: "amsterdam", CommodityID: "water_beer", Side: side, LotTons: 1, StrikePricePence: strike,
			ReservedTons: tons, ReservedCashPence: cash, MarginPence: margin, ExposurePence: exposure, Status: "intent",
			CreatedAt: lower, MaturesAt: lower.Add(time.Duration(float64(30*time.Hour) / float64(r.stateCache.Clock.Calendar.TimeScale)))}
		if err := r.db.PutFuturesIntent(ctx, &intent); err != nil {
			if errors.Is(err, store.ErrInvalidInput) {
				return false, r.hold(ctx, "__futures", "futures pilot refused an admission above its inclusive 300 Cr exposure limit")
			}
			return false, err
		}
		data.Futures.IntentID = intent.IntentID
		err = r.stage(ctx, mission, data, ShipState{}, "futures-create", "/v1/future/create", futuresCreateBody(intent))
		return data.Action != nil, err
	}
	return false, r.hold(ctx, "__futures", "futures pilot only: one covered 1t pair; no accepts, replacement cohorts or scale-up")
}

func (r *Runner) futuresContracts() ([]gameapi.FuturesContract, error) {
	if !r.fresh("futures") {
		return nil, nil
	}
	var records []json.RawMessage
	if err := json.Unmarshal(r.snapshot["futures"], &records); err != nil {
		return nil, fmt.Errorf("%w: invalid futures list: %v", errFuturesEvidence, err)
	}
	if records == nil {
		return nil, fmt.Errorf("%w: futures list is null", errFuturesEvidence)
	}
	contracts := make([]gameapi.FuturesContract, 0, len(records))
	skipped := 0
	for _, raw := range records {
		var identity struct {
			Creator string  `json:"creator_company_id"`
			Seller  *string `json:"seller_company_id"`
			Buyer   *string `json:"buyer_company_id"`
		}
		identityErr := json.Unmarshal(raw, &identity)
		owned := identity.Creator == r.account.CompanyID || identity.Seller != nil && *identity.Seller == r.account.CompanyID ||
			identity.Buyer != nil && *identity.Buyer == r.account.CompanyID
		var contract gameapi.FuturesContract
		if err := json.Unmarshal(raw, &contract); identityErr != nil || err != nil {
			if owned {
				return nil, fmt.Errorf("%w: invalid owned futures contract", errFuturesEvidence)
			}
			skipped++
			continue
		}
		contracts = append(contracts, contract)
	}
	if skipped > 0 && !time.Now().Before(r.futuresWarningAfter) {
		r.futuresWarningAfter = time.Now().Add(time.Minute)
		r.log.Warn("skipping malformed third-party futures records", zap.Int("skipped_records", skipped))
	}
	return contracts, nil
}

func (r *Runner) futuresRejectionEvidence(ctx context.Context, data *missionData, stage string) (string, error) {
	if stage == "futures-settle" {
		row, err := r.futuresIntent(ctx, data)
		if err != nil {
			return "", err
		}
		if data.Action != nil {
			var request struct {
				ID string `json:"futures_id"`
			}
			if json.Unmarshal(data.Action.Body, &request) != nil || request.ID != row.FuturesID {
				return "", errors.New("settlement request disagrees with its permanent futures identity")
			}
		}
		contracts, err := r.futuresContracts()
		if err != nil {
			return "", err
		}
		for _, contract := range contracts {
			if contract.ID == row.FuturesID {
				return string(encode(contract)), nil
			}
		}
		return "", nil
	}
	warehouse, _ := r.futuresWarehouse()
	if stage == "futures-close-lease" {
		return string(encode(warehouse)), nil
	}
	bid, _ := futuresNPCPrices(r.stateCache.Markets["amsterdam"])
	return string(encode(struct {
		Warehouse futuresWarehouse
		Bid       int64
	}{warehouse, bid})), nil
}

func (r *Runner) futuresRefused(ctx context.Context, data *missionData, stage string) (bool, error) {
	key := stage
	if stage == "futures-settle" {
		key += "/" + data.Futures.IntentID
	}
	previous, exists := data.Futures.RejectedEvidence[key]
	if !exists {
		return false, nil
	}
	current, err := r.futuresRejectionEvidence(ctx, data, stage)
	return previous == current, err
}

func (r *Runner) recordFuturesPnL(ctx context.Context, mission *store.Mission, data *missionData) error {
	if data.Futures == nil {
		return nil
	}
	return r.db.RecordFuturesPairPnL(ctx, r.account.ID, futuresPilotCohort, store.FuturesCohortAccounting{
		ProcurementPence: data.Futures.ProcurementPaid, LiquidationPence: data.Futures.LiquidationPaid,
		LiquidatedTons: data.Futures.LiquidatedTons, SetupOwned: data.Futures.LeaseOwned,
		WindDownComplete: mission.Phase == "futures_done",
		CashUnknown:      data.Futures.ProcurementUnknown || data.Futures.LiquidationUnknown,
	})
}

func (r *Runner) listedFuture(id string) (gameapi.FuturesContract, bool) {
	if !r.fresh("futures") {
		return gameapi.FuturesContract{}, false
	}
	contracts, err := r.futuresContracts()
	if err != nil {
		return gameapi.FuturesContract{}, false
	}
	for _, row := range contracts {
		if row.ID == id {
			return row, true
		}
	}
	return gameapi.FuturesContract{}, false
}

func (r *Runner) futuresIntent(ctx context.Context, data *missionData) (*store.FuturesIntent, error) {
	if data.Futures == nil || data.Futures.IntentID == "" {
		return nil, errors.New("futures action has no durable reservation identity")
	}
	rows, err := r.db.FuturesIntents(ctx, r.account.ID)
	if err != nil {
		return nil, err
	}
	for i := range rows {
		if rows[i].IntentID == data.Futures.IntentID {
			return &rows[i], nil
		}
	}
	return nil, errors.New("futures action has no durable reservation")
}

func (r *Runner) futuresPreflight(ctx context.Context, data *missionData) error {
	if err := r.futuresAuthority(); err != nil {
		return fmt.Errorf("%w: %w", errFuturesPrecondition, err)
	}
	if data.Action == nil || data.Futures == nil {
		return errors.New("futures action has no durable pilot state")
	}
	stage := data.Action.Stage
	if !r.opts.FuturesPilotEnabled && (stage == "futures-create" || stage == "futures-stock" || stage == "futures-lease") {
		return fmt.Errorf("%w: pilot admissions disabled; only obligations and wind-down may dispatch", errFuturesPrecondition)
	}
	if stage == "futures-stock" || stage == "futures-close-stock" {
		if err := r.futuresOrderPreflight(data.Action); err != nil {
			return err
		}
	}
	warehouse, unlocked := r.futuresWarehouse()
	if warehouse.LeasedTonnage > 2 {
		return fmt.Errorf("%w: warehouse lease exceeds 2t", errFuturesTerms)
	}
	if data.Action.Stage == "futures-close-stock" || data.Action.Stage == "futures-close-lease" {
		rows, err := r.db.FuturesIntents(ctx, r.account.ID)
		if err != nil {
			return err
		}
		for _, row := range rows {
			_, defaultKnown := store.FuturesSettlement(row)
			if !store.FuturesTerminal(row.Status) || row.Status == "defaulted" && !defaultKnown {
				return fmt.Errorf("%w: wind-down waits for physical and default claim/penalty evidence", errFuturesPrecondition)
			}
		}
		if data.Action.Stage == "futures-close-stock" && (unlocked < 1 || !r.fresh("books/amsterdam")) {
			return fmt.Errorf("%w: wind-down requires an unlocked warehouse ton and a current exit quote", errFuturesPrecondition)
		}
		if data.Action.Stage == "futures-close-lease" && warehouse.UsedTonnage+warehouse.ReservedTonnage > data.Futures.InitialLease {
			return fmt.Errorf("%w: wind-down cannot shrink below occupied or locked warehouse space", errFuturesPrecondition)
		}
		return nil
	}
	if data.Action.Stage == "futures-lease" || data.Action.Stage == "futures-stock" {
		if stage == "futures-stock" && (warehouse.FreeTonnage < 2 || !r.fresh("books/amsterdam") || !r.fresh("ports/amsterdam")) {
			return fmt.Errorf("%w: procurement requires two free warehouse tons and current Amsterdam authority", errFuturesPrecondition)
		}
		cash, err := r.futuresFreeCash(ctx, nil)
		if err != nil {
			return err
		}
		if cash < store.FuturesExposureLimitPence {
			return fmt.Errorf("%w: setup requires its spare 300 Cr allowance", errFuturesPrecondition)
		}
		return nil
	}
	row, err := r.futuresIntent(ctx, data)
	if err != nil {
		return err
	}
	if data.Action.Stage == "futures-settle" {
		lower, _, ok := r.serverBounds()
		if !ok || row.ObservedMaturesAt == nil || lower.Before(*row.ObservedMaturesAt) {
			return fmt.Errorf("%w: settlement waits for the server-time lower bound to reach maturity", errFuturesPrecondition)
		}
		if _, exists := r.listedFuture(row.FuturesID); !exists {
			return fmt.Errorf("%w: vanished listing awaits private outcome evidence, never a replacement", errFuturesPrecondition)
		}
		return nil
	}
	rows, err := r.db.FuturesIntents(ctx, r.account.ID)
	if err != nil {
		return err
	}
	for _, candidate := range rows {
		if candidate.IntentID != row.IntentID && candidate.Status == "active" && candidate.ObservedMaturesAt != nil {
			lower, _, ok := r.serverBounds()
			if ok && !lower.Before(*candidate.ObservedMaturesAt) {
				return fmt.Errorf("%w: mature settlement takes priority over an unsent create", errFuturesPrecondition)
			}
		}
	}
	if row.StrikePricePence <= 0 || row.StrikePricePence > store.FuturesExposureLimitPence ||
		row.LotTons != 1 || row.MarginPence != row.StrikePricePence/4 ||
		row.ExposurePence <= 0 || row.ExposurePence > store.FuturesExposureLimitPence ||
		data.Action.Method != http.MethodPost || data.Action.Path != "/v1/future/create" {
		return fmt.Errorf("%w: create exceeds its immutable one-ton cash cap", errFuturesTerms)
	}
	if data.Action.Stage != "futures-create" || row.ActionID != data.Action.ID || row.Status != "intent" {
		return fmt.Errorf("%w: create disagrees with its immutable covered pilot admission", errFuturesTerms)
	}
	if lower, _, _ := r.serverBounds(); !lower.Before(row.MaturesAt) {
		return fmt.Errorf("%w: unsent create passed its admitted maturity", errFuturesTerms)
	}
	if warehouse.LeasedTonnage != 2 || !r.fresh("books/amsterdam") || !r.fresh("ports/amsterdam") {
		return fmt.Errorf("%w: create requires two leased tons and current Amsterdam authority", errFuturesPrecondition)
	}
	want := encode(futuresCreateBody(*row))
	if string(want) != string(data.Action.Body) {
		return fmt.Errorf("%w: create request differs from its permanent terms", errFuturesTerms)
	}
	free, err := r.futuresFreeCash(ctx, row)
	if err != nil {
		return err
	}
	customs := r.stateCache.Markets["amsterdam"].CustomsBPS
	if customs < 0 || customs > 10000 {
		return fmt.Errorf("%w: create requires current bounded customs", errFuturesPrecondition)
	}
	if row.Side == "bid" && row.ReservedCashPence < row.StrikePricePence+ceilBPS(row.StrikePricePence, customs)+row.MarginPence {
		return fmt.Errorf("%w: immutable bid reserve no longer covers current customs and margin", errFuturesTerms)
	}
	if free < row.ReservedCashPence || row.Side == "ask" && unlocked < row.ReservedTons || row.Side == "bid" && warehouse.FreeTonnage < 1 {
		return fmt.Errorf("%w: pilot lost its warehouse tons, peak capacity or full cash coverage", errFuturesPrecondition)
	}
	return nil
}

func (r *Runner) futuresOrderPreflight(action *durableAction) error {
	var request struct {
		PortID      string `json:"port_id"`
		CommodityID string `json:"commodity_id"`
		Side        string `json:"side"`
		TimeInForce string `json:"time_in_force"`
		Quantity    int64  `json:"quantity"`
		Price       int64  `json:"price_pence"`
	}
	decoder := json.NewDecoder(bytes.NewReader(action.Body))
	decoder.DisallowUnknownFields()
	if decoder.Decode(&request) != nil || action.Method != http.MethodPost || action.Path != "/v1/market/place-order" ||
		request.PortID != "amsterdam" || request.CommodityID != "water_beer" || request.TimeInForce != "ioc" ||
		request.Quantity != 1 || request.Price <= 0 || request.Price > maximumPence {
		return fmt.Errorf("%w: warehouse order differs from its bounded one-ton IOC", errFuturesTerms)
	}
	if action.Stage == "futures-close-stock" {
		if request.Side != "ask" {
			return fmt.Errorf("%w: wind-down must sell, never buy stock", errFuturesTerms)
		}
		return nil
	}
	customs := r.stateCache.Markets["amsterdam"].CustomsBPS
	if customs < 0 || customs > 10000 {
		return fmt.Errorf("%w: procurement requires bounded current customs", errFuturesPrecondition)
	}
	if request.Side != "bid" || request.Price > store.FuturesExposureLimitPence ||
		request.Price+ceilBPS(request.Price, customs) > store.FuturesExposureLimitPence {
		return fmt.Errorf("%w: procurement exceeds its inclusive 300 Cr cash limit", errFuturesTerms)
	}
	return nil
}

func futuresObservationError(err error) error {
	if errors.Is(err, store.ErrInvalidInput) || errors.Is(err, store.ErrActionConflict) {
		return fmt.Errorf("%w: %v", errFuturesEvidence, err)
	}
	return err
}

func (r *Runner) consumeFuturesResult(ctx context.Context, mission *store.Mission, data *missionData, status int, response json.RawMessage) (bool, error) {
	stage := data.Action.Stage
	var envelope struct {
		Uninterpreted bool `json:"committed_uninterpreted"`
	}
	_ = json.Unmarshal(response, &envelope)
	if status >= 200 && status < 300 && envelope.Uninterpreted {
		return r.reconcileCommitted(ctx, mission, data)
	}
	var problem struct {
		Code   string `json:"code"`
		Detail string `json:"detail"`
	}
	_ = json.Unmarshal(response, &problem)
	detail := strings.ToLower(problem.Detail)
	nonParty := stage == "futures-settle" && status == http.StatusForbidden && problem.Code == "forbidden" &&
		(strings.Contains(detail, "not a party") || strings.Contains(detail, "non-party") ||
			strings.Contains(detail, "neither") && strings.Contains(detail, "buyer") && strings.Contains(detail, "seller"))
	leaseDomainRefusal := (stage == "futures-lease" || stage == "futures-close-lease") &&
		status == http.StatusForbidden && problem.Code == "no_port_presence"
	if stage == "futures-lease" && leaseDomainRefusal {
		data.Futures.SetupAttempted = false
		data.CooldownUntil = maxTime(data.CooldownUntil, r.gameDelay(1))
		if err := r.hold(ctx, "__futures", "pilot lease waits for Amsterdam port presence; fleet work continues"); err != nil {
			return false, err
		}
		data.LastActionID = data.Action.ID
		data.Action = nil
		data.Sequence++
		return true, r.saveMission(ctx, mission, data)
	}
	if stage == "futures-create" && status >= 400 && status < 500 {
		row, err := r.futuresIntent(ctx, data)
		if err != nil {
			return false, err
		}
		if row.FuturesID != "" {
			if status == http.StatusUnauthorized || status == http.StatusForbidden || status == http.StatusTooManyRequests {
				return r.consumeRejection(ctx, mission, data, status, response)
			}
			return r.reconcileCommitted(ctx, mission, data)
		}
	}
	if status == http.StatusUnauthorized || status == http.StatusForbidden && !nonParty && !leaseDomainRefusal || status == http.StatusTooManyRequests {
		nextSequence := data.Sequence + 1
		if stage == "futures-create" {
			nextID := ActionID(r.account.ID, mission.Generation, stage, nextSequence)
			if err := r.db.ReadmitFuturesIntent(ctx, r.account.ID, data.Futures.IntentID, nextID); err != nil {
				return false, err
			}
		}
		if stage == "futures-lease" {
			data.Futures.SetupAttempted = false
		}
		return r.consumeRejection(ctx, mission, data, status, response)
	}
	if status >= 400 && status < 500 {
		if stage == "futures-create" {
			if err := r.db.RecordFuturesOutcome(ctx, r.account.ID, data.Futures.IntentID, "rejected", response); err != nil {
				return false, err
			}
		}
		mission.Phase = "futures_stopped"
		if stage == "futures-settle" || stage == "futures-close-stock" || stage == "futures-close-lease" {
			if data.Futures.RejectedEvidence == nil {
				data.Futures.RejectedEvidence = make(map[string]string)
			}
			key := stage
			if stage == "futures-settle" {
				key += "/" + data.Futures.IntentID
			}
			evidence, err := r.futuresRejectionEvidence(ctx, data, stage)
			if err != nil {
				return false, err
			}
			data.Futures.RejectedEvidence[key] = evidence
		}
		if err := r.hold(ctx, "__futures", "futures pilot stopped after a definitive rejected request; no replacement or scale-up"); err != nil {
			return false, err
		}
	} else if status >= 200 && status < 300 {
		switch stage {
		case "futures-create":
			var contract gameapi.FuturesContract
			if json.Unmarshal(response, &contract) != nil || contract.ID == "" {
				return r.reconcileCommitted(ctx, mission, data)
			}
			if err := r.db.ObserveFuturesContract(ctx, r.account.ID, data.Futures.IntentID, contract); err != nil {
				if !errors.Is(err, store.ErrInvalidInput) && !errors.Is(err, store.ErrActionConflict) {
					return false, err
				}
				return r.reconcileCommitted(ctx, mission, data)
			}
		case "futures-settle":
			row, err := r.futuresIntent(ctx, data)
			if err != nil {
				return false, err
			}
			var result gameapi.SettleFuturesResult
			if json.Unmarshal(response, &result) != nil || result.FuturesID != row.FuturesID ||
				(result.Status != "settled" && result.Status != "defaulted") || result.SettledTons < 0 || result.SettledTons > 1 ||
				result.PenaltyPaidPence < 0 || result.PenaltyPaidPence > maximumPence || result.ClaimPence < 0 || result.ClaimPence > maximumPence {
				return r.reconcileCommitted(ctx, mission, data)
			}
			if err := r.db.RecordFuturesOutcome(ctx, r.account.ID, row.IntentID, result.Status, response); err != nil {
				return false, futuresObservationError(err)
			}
		case "futures-lease":
			var warehouse futuresWarehouse
			if json.Unmarshal(response, &warehouse) != nil || warehouse.PortID != "amsterdam" || warehouse.LeasedTonnage != 2 {
				return r.reconcileCommitted(ctx, mission, data)
			}
			data.Futures.LeaseOwned = true
		case "futures-stock", "futures-close-stock":
			var result struct {
				tradeResult
				PortID string `json:"port_id"`
				Side   string `json:"side"`
			}
			var request struct {
				Side string `json:"side"`
			}
			_ = json.Unmarshal(data.Action.Body, &request)
			if json.Unmarshal(response, &result) != nil || !validTrade(result.tradeResult) || result.CommodityID != "water_beer" ||
				result.PortID != "amsterdam" || result.Side != request.Side || *result.FilledQuantity > 1 ||
				*result.TotalGrossPence > maximumPence || *result.CustomsDutyPence > *result.TotalGrossPence ||
				stage == "futures-stock" && (*result.TotalGrossPence > store.FuturesExposureLimitPence ||
					*result.TotalGrossPence+*result.CustomsDutyPence > store.FuturesExposureLimitPence) ||
				*result.FilledQuantity == 0 && (*result.TotalGrossPence != 0 || *result.CustomsDutyPence != 0) {
				return r.reconcileCommitted(ctx, mission, data)
			}
			if stage == "futures-stock" {
				if *result.FilledQuantity == 0 {
					mission.Phase = "futures_stopped"
				} else {
					data.Futures.ProcurementPaid = *result.TotalGrossPence + *result.CustomsDutyPence
				}
			} else {
				data.Futures.LiquidatedTons += *result.FilledQuantity
				data.Futures.LiquidationPaid += *result.TotalGrossPence - *result.CustomsDutyPence
				if *result.FilledQuantity == 0 {
					data.CooldownUntil = maxTime(data.CooldownUntil, r.gameDelay(1))
				}
			}
		case "futures-close-lease":
			var warehouse futuresWarehouse
			if json.Unmarshal(response, &warehouse) != nil || warehouse.PortID != "amsterdam" || warehouse.LeasedTonnage != data.Futures.InitialLease {
				return r.reconcileCommitted(ctx, mission, data)
			}
			mission.Phase = "futures_done"
		}
	} else {
		return false, r.hold(ctx, "__futures", "non-definitive futures outcome retains its original key and reservations")
	}
	data.LastActionID = data.Action.ID
	data.Action = nil
	data.Sequence++
	return true, r.saveMission(ctx, mission, data)
}

func (r *Runner) observeFutures(ctx context.Context, rows []store.FuturesIntent) error {
	contracts, err := r.futuresContracts()
	if err != nil {
		return err
	}
	events, err := r.db.FuturesEvents(ctx, r.account.ID)
	if err != nil {
		return err
	}
	vaults := r.stateCache.Vaults
	for _, row := range rows {
		if store.FuturesTerminal(row.Status) && row.RealizedPnLPence != nil {
			continue
		}
		if row.FuturesID == "" && r.fresh("futures") {
			var matched *gameapi.FuturesContract
			for _, contract := range contracts {
				if contract.CreatorCompanyID != r.account.CompanyID || contract.PortID != row.PortID || contract.CommodityID != row.CommodityID ||
					contract.Side != row.Side || int64(contract.LotTons) != row.LotTons || contract.StrikePricePence != row.StrikePricePence ||
					contract.CreatedAt.Before(row.CreatedAt.Add(-time.Second)) {
					continue
				}
				if matched != nil {
					return fmt.Errorf("%w: ambiguous create has multiple matching owned listings; original request retained", errFuturesEvidence)
				}
				value := contract
				matched = &value
			}
			if matched != nil {
				if err := r.db.ObserveFuturesContract(ctx, r.account.ID, row.IntentID, *matched); err != nil {
					return futuresObservationError(err)
				}
				row.FuturesID = matched.ID
			}
		}
		if row.FuturesID == "" {
			continue
		}
		if contract, exists := r.listedFuture(row.FuturesID); exists {
			if err := r.db.ObserveFuturesContract(ctx, r.account.ID, row.IntentID, contract); err != nil {
				return futuresObservationError(err)
			}
		}
		for _, event := range events {
			var envelope struct {
				CompanyID string                      `json:"company_id"`
				PortID    string                      `json:"port_id"`
				Timestamp time.Time                   `json:"timestamp"`
				Payload   gameapi.SettleFuturesResult `json:"payload"`
			}
			if json.Unmarshal(event.Payload, &envelope) != nil || envelope.CompanyID != r.account.CompanyID || envelope.PortID != row.PortID ||
				envelope.Timestamp.IsZero() || envelope.Payload.FuturesID != row.FuturesID {
				continue
			}
			status := envelope.Payload.Status
			if event.EventType == "futures_expired" {
				status = "cancelled"
			} else if event.EventType != "futures_settled" && event.EventType != "futures_defaulted" {
				continue
			}
			if !store.FuturesTerminal(status) || envelope.Payload.SettledTons < 0 || envelope.Payload.SettledTons > 1 ||
				envelope.Payload.PenaltyPaidPence < 0 || envelope.Payload.ClaimPence < 0 {
				continue
			}
			// A private terminal receipt may survive public list retention. Its
			// full source envelope is retained permanently with the owned ID.
			if err := r.db.RecordFuturesOutcome(ctx, r.account.ID, row.IntentID, status, event.Payload); err != nil {
				return futuresObservationError(err)
			}
		}
		for _, raw := range decode[[]json.RawMessage](r.snapshot, "ledger") {
			var entry struct {
				ID              string `json:"id"`
				DebitAccountID  string `json:"debit_account_id"`
				CreditAccountID string `json:"credit_account_id"`
				AmountPence     int64  `json:"amount_pence"`
				Metadata        any    `json:"metadata"`
			}
			if json.Unmarshal(raw, &entry) != nil || entry.ID == "" || entry.AmountPence <= 0 || entry.AmountPence > maximumPence ||
				!insuranceReference(entry.Metadata, row.FuturesID, row.FuturesID) {
				continue
			}
			var delta int64
			for _, vault := range vaults {
				if vault.AccountType != "vault" && vault.AccountType != "escrow" {
					continue
				}
				if entry.CreditAccountID == vault.AccountID {
					delta += entry.AmountPence
				}
				if entry.DebitAccountID == vault.AccountID {
					delta -= entry.AmountPence
				}
			}
			if err := r.db.RecordFuturesCashFlow(ctx, r.account.ID, row.IntentID, entry.ID, delta, raw); err != nil {
				return futuresObservationError(err)
			}
		}
	}
	return nil
}

func (r *Runner) reconcileFuturesAction(ctx context.Context, mission *store.Mission, data *missionData, action *store.Action) (bool, error) {
	if data.Action.Stage == "futures-stock" || data.Action.Stage == "futures-close-stock" {
		if data.Action.FuturesStockBefore == nil {
			return false, r.hold(ctx, "__futures", "warehouse order reconciliation requires its original inventory baseline")
		}
		if data.Action.FuturesObserveAfter.IsZero() {
			data.Action.FuturesObserveAfter = time.Now().UTC()
			return true, r.saveMission(ctx, mission, data)
		}
		if !r.fresh("warehouses") || !r.meta["warehouses"].UpdatedAt.After(data.Action.FuturesObserveAfter) {
			return false, nil
		}
		var request struct {
			Side  string `json:"side"`
			Price int64  `json:"price_pence"`
		}
		if err := json.Unmarshal(data.Action.Body, &request); err != nil {
			return false, err
		}
		_, unlocked := r.futuresWarehouse()
		delta := unlocked - *data.Action.FuturesStockBefore
		expected := int64(1)
		if data.Action.Stage == "futures-close-stock" {
			expected = -1
		}
		candidates, gross, customs := 0, int64(0), int64(0)
		freshFills := r.fresh("fills") && r.meta["fills"].UpdatedAt.After(data.Action.FuturesObserveAfter)
		if freshFills {
			var fills []struct {
				PortID      string    `json:"port_id"`
				CommodityID string    `json:"commodity_id"`
				Side        string    `json:"side"`
				IsMaker     *bool     `json:"is_maker"`
				Price       int64     `json:"price_pence"`
				Quantity    int64     `json:"quantity"`
				Customs     *int64    `json:"customs_duty_pence"`
				CreatedAt   time.Time `json:"created_at"`
			}
			if err := json.Unmarshal(r.snapshot["fills"], &fills); err != nil {
				return false, fmt.Errorf("%w: invalid private fills", errFuturesEvidence)
			}
			for _, fill := range fills {
				if fill.PortID != "amsterdam" || fill.CommodityID != "water_beer" || fill.Side != request.Side ||
					fill.IsMaker == nil || *fill.IsMaker || fill.Quantity != 1 || fill.Price <= 0 || fill.Price > maximumPence ||
					fill.Customs == nil || *fill.Customs < 0 || *fill.Customs > fill.Price ||
					fill.CreatedAt.Before(data.Action.CreatedAt) || fill.CreatedAt.After(r.meta["fills"].UpdatedAt) ||
					request.Side == "bid" && (fill.Price > request.Price || fill.Price+*fill.Customs > store.FuturesExposureLimitPence) ||
					request.Side == "ask" && fill.Price < request.Price {
					continue
				}
				candidates++
				gross, customs = fill.Price, *fill.Customs
			}
		}
		noFill := delta == 0 && candidates == 0 && freshFills && action != nil && action.State == store.ActionComplete
		if delta != expected && !noFill {
			return false, nil
		}
		knownCash := candidates == 1
		if data.Action.Stage == "futures-stock" {
			if noFill {
				mission.Phase = "futures_stopped"
			} else if knownCash {
				data.Futures.ProcurementPaid = gross + customs
			} else {
				data.Futures.ProcurementUnknown = true
				mission.Phase = "futures_stopped"
			}
		} else if noFill {
			data.CooldownUntil = maxTime(data.CooldownUntil, r.gameDelay(1))
		} else {
			data.Futures.LiquidatedTons++
			if knownCash {
				data.Futures.LiquidationPaid += gross - customs
			} else {
				data.Futures.LiquidationUnknown = true
			}
		}
		if action != nil && action.State == store.ActionPending {
			evidence := encode(map[string]any{"kind": "authoritative_futures_stock", "stock_before": *data.Action.FuturesStockBefore,
				"stock_after": unlocked, "cash_known": knownCash, "gross_pence": gross, "customs_pence": customs})
			if err := r.db.ReconcileAction(ctx, action.ID, evidence); err != nil {
				return false, err
			}
		}
		data.LastActionID, data.Action = data.Action.ID, nil
		data.Sequence++
		return true, r.saveMission(ctx, mission, data)
	}
	if data.Action.Stage == "futures-lease" || data.Action.Stage == "futures-close-lease" {
		warehouse, _ := r.futuresWarehouse()
		target := int64(2)
		if data.Action.Stage == "futures-close-lease" {
			target = data.Futures.InitialLease
		}
		if !r.fresh("warehouses") || warehouse.LeasedTonnage != target {
			return false, nil
		}
		if data.Action.Stage == "futures-lease" {
			data.Futures.LeaseOwned = true
		} else {
			mission.Phase = "futures_done"
		}
		data.LastActionID, data.Action = data.Action.ID, nil
		data.Sequence++
		return true, r.saveMission(ctx, mission, data)
	}
	if data.Action.Stage != "futures-create" && data.Action.Stage != "futures-settle" {
		return false, nil
	}
	row, err := r.futuresIntent(ctx, data)
	if err != nil {
		return false, err
	}
	if err := r.observeFutures(ctx, []store.FuturesIntent{*row}); err != nil {
		return false, err
	}
	rows, err := r.db.FuturesIntents(ctx, r.account.ID)
	if err != nil {
		return false, err
	}
	for _, observed := range rows {
		if observed.IntentID != row.IntentID || observed.FuturesID == "" ||
			data.Action.Stage == "futures-settle" && !store.FuturesTerminal(observed.Status) {
			continue
		}
		evidence := encode(map[string]any{"kind": "authoritative_futures", "intent_id": observed.IntentID, "futures_id": observed.FuturesID, "status": observed.Status})
		if action != nil && action.State == store.ActionPending {
			if err := r.db.ReconcileAction(ctx, action.ID, evidence); err != nil {
				return false, err
			}
		}
		data.LastActionID = data.Action.ID
		data.Action = nil
		data.Sequence++
		return true, r.saveMission(ctx, mission, data)
	}
	return false, nil
}

func (r *Runner) closeFuturesWarehouse(ctx context.Context, mission *store.Mission, data *missionData, rows []store.FuturesIntent) (bool, error) {
	remaining := -data.Futures.LiquidatedTons
	if data.Futures.ProcurementPaid > 0 || data.Futures.ProcurementUnknown {
		remaining++
	}
	for _, row := range rows {
		if row.Status != "settled" && row.Status != "defaulted" {
			continue
		}
		receipt, known := store.FuturesSettlement(row)
		if !known {
			return false, r.hold(ctx, "__futures", "futures wind-down waits for exact physical and default claim/penalty receipts")
		}
		if row.Side == "bid" {
			remaining += int64(receipt.SettledTons)
		} else {
			remaining -= int64(receipt.SettledTons)
		}
	}
	warehouse, unlocked := r.futuresWarehouse()
	if remaining > 0 {
		refused, err := r.futuresRefused(ctx, data, "futures-close-stock")
		if err != nil {
			return false, err
		}
		if refused {
			return false, r.hold(ctx, "__futures", "futures liquidation refusal waits for changed stock or exit quote evidence")
		}
		bid, _ := futuresNPCPrices(r.stateCache.Markets["amsterdam"])
		if unlocked < remaining || !r.fresh("books/amsterdam") || bid <= 0 {
			return false, r.hold(ctx, "__futures", "futures wind-down waits for owned unlocked stock and a normal Amsterdam exit quote")
		}
		err = r.stage(ctx, mission, data, ShipState{}, "futures-close-stock", "/v1/market/place-order", map[string]any{
			"port_id": "amsterdam", "commodity_id": "water_beer", "side": "ask", "time_in_force": "ioc", "quantity": 1, "price_pence": bid,
		})
		return data.Action != nil, err
	}
	if !data.Futures.LeaseOwned {
		mission.Phase = "futures_done"
		return true, r.saveMission(ctx, mission, data)
	}
	refused, err := r.futuresRefused(ctx, data, "futures-close-lease")
	if err != nil {
		return false, err
	}
	if refused {
		return false, r.hold(ctx, "__futures", "futures lease-close refusal waits for changed warehouse evidence")
	}
	if warehouse.UsedTonnage+warehouse.ReservedTonnage > data.Futures.InitialLease {
		return false, r.hold(ctx, "__futures", "futures wind-down preserves unrelated warehouse inventory and locks")
	}
	err = r.stage(ctx, mission, data, ShipState{}, "futures-close-lease", "/v1/warehouse/set-lease", map[string]any{
		"port_id": "amsterdam", "target_leased_tonnage": data.Futures.InitialLease,
	})
	return data.Action != nil, err
}
