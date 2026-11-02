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

	"github.com/DevYukine/go-tradewinds/internal/gameapi"
	"github.com/DevYukine/go-tradewinds/internal/store"
	"github.com/google/uuid"
	"go.uber.org/zap"
)

const legacyFuturesCohort = "amsterdam-water-beer-owner-pilot"

var errFuturesEvidence = errors.New("invalid futures evidence")
var errFuturesPrecondition = errors.New("transient futures precondition")
var errFuturesTerms = errors.New("invalid immutable futures terms or cap")

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

// Lease and stock fields also retain the outstanding pre-controller wind-down.
type futuresState struct {
	IntentID           string                         `json:"intent_id"`
	PortID             string                         `json:"port_id,omitempty"`
	CommodityID        string                         `json:"commodity_id,omitempty"`
	LeaseOwned         bool                           `json:"lease_owned"`
	LegacyClosed       bool                           `json:"legacy_closed,omitempty"`
	ProcurementPaid    int64                          `json:"procurement_paid_pence"`
	InitialLease       int64                          `json:"initial_lease_tons"`
	LiquidatedTons     int64                          `json:"liquidated_tons"`
	LiquidationPaid    int64                          `json:"liquidation_paid_pence"`
	RejectedEvidence   map[string]string              `json:"rejected_evidence,omitempty"`
	ProcurementUnknown bool                           `json:"procurement_cash_unknown,omitempty"`
	LiquidationUnknown bool                           `json:"liquidation_cash_unknown,omitempty"`
	StockExits         map[string]futuresStockExit    `json:"stock_exits,omitempty"`
	StockExitCohort    string                         `json:"stock_exit_cohort,omitempty"`
	Leases             map[string]futuresLeaseHolding `json:"leases,omitempty"`
	LeaseChange        *futuresLeaseChange            `json:"lease_change,omitempty"`
}

type futuresStockExit struct {
	Tons        int64 `json:"tons"`
	Proceeds    int64 `json:"proceeds_pence"`
	CashUnknown bool  `json:"cash_unknown,omitempty"`
}

func recordFuturesStockExit(state *futuresState, tons, proceeds int64, unknown bool) {
	if state.StockExitCohort == "" || state.StockExitCohort == legacyFuturesCohort {
		state.LiquidatedTons += tons
		state.LiquidationPaid += proceeds
		state.LiquidationUnknown = state.LiquidationUnknown || unknown
		return
	}
	if state.StockExits == nil {
		state.StockExits = make(map[string]futuresStockExit)
	}
	exit := state.StockExits[state.StockExitCohort]
	exit.Tons += tons
	exit.Proceeds += proceeds
	exit.CashUnknown = exit.CashUnknown || unknown
	state.StockExits[state.StockExitCohort] = exit
}

func (state *futuresState) port() string {
	if state.PortID == "" {
		return "amsterdam" // Outstanding lease predating the controller.
	}
	return state.PortID
}

func (state *futuresState) commodity() string {
	if state.CommodityID == "" {
		return "water_beer"
	}
	return state.CommodityID
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

func (r *Runner) futuresWarehouse(port, commodity string) (futuresWarehouse, int64) {
	for _, warehouse := range decode[[]futuresWarehouse](r.snapshot, "warehouses") {
		if warehouse.PortID != port {
			continue
		}
		var unlocked int64
		for _, item := range warehouse.Inventory {
			if item.CompanyID == r.account.CompanyID && item.PortID == port && item.CommodityID == commodity {
				unlocked += max(0, item.Quantity-item.LockedQuantity)
			}
		}
		return warehouse, unlocked
	}
	return futuresWarehouse{PortID: port}, 0
}

func futuresNPCPrices(market Market, commodity string) (bid, ask int64) {
	for _, book := range market.Books {
		if book.CommodityID != commodity || book.Depth == nil {
			continue
		}
		for _, level := range book.Depth.Bids {
			if level.IsNPC && level.RemainingQuantity > 0 && level.PricePence > bid && level.PricePence <= maximumPence {
				bid = level.PricePence
			}
		}
		for _, level := range book.Depth.Asks {
			if level.IsNPC && level.RemainingQuantity > 0 && level.PricePence > 0 && level.PricePence <= maximumPence && (ask == 0 || level.PricePence < ask) {
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
			return fmt.Errorf("futures requires current successful %s authority", key)
		}
	}
	if _, _, ok := r.serverBounds(); !ok || r.stateCache.Clock.Paused || r.stateCache.Clock.Calendar.TimeScale <= 0 {
		return errors.New("futures requires unpaused server clock authority")
	}
	return nil
}

func (r *Runner) futuresErrorHold(ctx context.Context, changed *bool, err *error) {
	if *err == nil || ctx.Err() != nil || !errors.Is(*err, errFuturesEvidence) {
		return
	}
	reason := "futures evidence held: " + (*err).Error()
	if entry := r.log.Check(zap.WarnLevel, "optional futures controller held"); entry != nil {
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
	if mission == nil {
		mission = &store.Mission{AccountID: r.account.ID, ShipID: "__futures", Generation: uuid.NewString(), Phase: "evaluating"}
		data = &missionData{Futures: &futuresState{}, StartedAt: time.Now().UTC()}
	}
	if data.Futures == nil {
		return false, errors.New("futures mission is missing durable state")
	}
	if mission.Generation == legacyFuturesCohort {
		data.Futures.PortID, data.Futures.CommodityID = "amsterdam", "water_beer"
	}
	if err := r.recordFuturesPnL(ctx, mission, data); err != nil {
		return false, err
	}
	// A resumed pass may find both legs mature. Deliver purchases before sales.
	slices.SortStableFunc(rows, func(a, b store.FuturesIntent) int {
		return a.MaturesAt.Compare(b.MaturesAt)
	})
	lower, _, _ := r.serverBounds()
	for _, row := range rows {
		needsReceipt := futuresNeedsReceipt(row)
		if row.Status != "active" && !needsReceipt || row.ObservedMaturesAt == nil || row.FuturesID == "" {
			continue
		}
		r.wakeAtServer(*row.ObservedMaturesAt, true)
		if lower.Before(*row.ObservedMaturesAt) {
			continue
		}
		if !needsReceipt {
			if _, exists := r.listedFuture(row.FuturesID); !exists {
				continue
			}
		}
		data.Futures.IntentID = row.IntentID
		data.Futures.PortID, data.Futures.CommodityID = row.PortID, row.CommodityID
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
		if rows[i].Status != "intent" || rows[i].FuturesID != "" ||
			(mission.Generation != legacyFuturesCohort && lower.Before(rows[i].MaturesAt)) {
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
	if time.Now().Before(data.CooldownUntil) {
		r.wakeBy(data.CooldownUntil)
		return false, nil
	}
	if changed, err := r.closeFuturesSurplus(ctx, mission, data, rows); changed || err != nil {
		return changed, err
	}
	if mission.Generation == legacyFuturesCohort {
		terminal := true
		for _, row := range rows {
			if row.CohortID == legacyFuturesCohort && !store.FuturesTerminal(row.Status) {
				terminal = false
			}
		}
		if !terminal {
			return r.driveFuturesController(ctx, mission, data, rows)
		}
		if mission.Phase != "futures_done" || !r.futuresLeaseClosed(data) {
			mission.Phase = "wind-down"
			changed, err := r.closeFuturesWarehouse(ctx, mission, data, rows)
			if changed || err != nil {
				return changed, err
			}
			return r.driveFuturesController(ctx, mission, data, rows)
		}
		mission.Generation = uuid.NewString()
		mission.Phase = "evaluating"
		data.Futures.IntentID, data.Futures.PortID, data.Futures.CommodityID = "", "", ""
		data.Futures.LegacyClosed = true
		data.Futures.RejectedEvidence = nil
		return true, r.saveMission(ctx, mission, data)
	}
	return r.driveFuturesController(ctx, mission, data, rows)
}

func futuresNeedsReceipt(row store.FuturesIntent) bool {
	_, known := store.FuturesSettlement(row)
	return (row.Status == "settled" || row.Status == "defaulted") && !known
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
	warehouse, _ := r.futuresWarehouse(data.Futures.port(), data.Futures.commodity())
	if stage == "futures-close-lease" {
		return string(encode(warehouse)), nil
	}
	bid, _ := futuresNPCPrices(r.stateCache.Markets[data.Futures.port()], data.Futures.commodity())
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
	rows, err := r.db.FuturesIntents(ctx, r.account.ID)
	if err != nil {
		return err
	}
	seen := make(map[string]bool)
	for _, row := range rows {
		if seen[row.CohortID] {
			continue
		}
		seen[row.CohortID] = true
		accounting := store.FuturesCohortAccounting{}
		if exit, exists := data.Futures.StockExits[row.CohortID]; exists {
			accounting.LiquidatedTons = exit.Tons
			accounting.LiquidationPence = exit.Proceeds
			accounting.CashUnknown = exit.CashUnknown
		}
		if row.CohortID == legacyFuturesCohort {
			if mission.Generation != legacyFuturesCohort && !data.Futures.LegacyClosed {
				continue
			}
			accounting = store.FuturesCohortAccounting{
				ProcurementPence: data.Futures.ProcurementPaid, LiquidationPence: data.Futures.LiquidationPaid,
				LiquidatedTons: data.Futures.LiquidatedTons, SetupOwned: data.Futures.LeaseOwned,
				WindDownComplete: data.Futures.LegacyClosed || mission.Phase == "futures_done" && r.futuresLeaseClosed(data),
				CashUnknown:      data.Futures.ProcurementUnknown || data.Futures.LiquidationUnknown,
			}
		}
		if err := r.db.RecordFuturesPairPnL(ctx, r.account.ID, row.CohortID, accounting); err != nil {
			return err
		}
	}
	return nil
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
		return errors.New("futures action has no durable state")
	}
	stage := data.Action.Stage
	if futuresLeaseStage(stage) {
		return r.futuresLeasePreflight(ctx, data)
	}
	warehouse, unlocked := r.futuresWarehouse(data.Futures.port(), data.Futures.commodity())
	if stage == "futures-close-stock" || stage == "futures-close-lease" {
		rows, err := r.db.FuturesIntents(ctx, r.account.ID)
		if err != nil {
			return err
		}
		cohort := legacyFuturesCohort
		if stage == "futures-close-stock" && data.Futures.StockExitCohort != "" {
			cohort = data.Futures.StockExitCohort
		}
		for _, row := range rows {
			if row.CohortID != cohort {
				continue
			}
			_, receiptKnown := store.FuturesSettlement(row)
			if !store.FuturesTerminal(row.Status) ||
				(row.Status == "settled" || row.Status == "defaulted") && !receiptKnown {
				return fmt.Errorf("%w: wind-down waits for physical and default claim/penalty evidence", errFuturesPrecondition)
			}
		}
		if stage == "futures-close-stock" {
			if err := r.futuresOrderPreflight(data.Action); err != nil {
				return err
			}
			var request struct {
				PortID      string `json:"port_id"`
				CommodityID string `json:"commodity_id"`
				Quantity    int64  `json:"quantity"`
			}
			if json.Unmarshal(data.Action.Body, &request) != nil || request.PortID != data.Futures.port() || request.CommodityID != data.Futures.commodity() {
				return fmt.Errorf("%w: liquidation changed its stock identity", errFuturesTerms)
			}
			remaining, known := futuresRemainingStock(rows, cohort, data.Futures)
			if unlocked < request.Quantity || !known || remaining < request.Quantity || !r.fresh("books/"+request.PortID) {
				return fmt.Errorf("%w: wind-down requires owned surplus stock and a current exit quote", errFuturesPrecondition)
			}
		} else if warehouse.UsedTonnage+warehouse.ReservedTonnage > data.Futures.InitialLease {
			return fmt.Errorf("%w: wind-down cannot shrink below occupied or locked warehouse space", errFuturesPrecondition)
		}
		return nil
	}
	if stage == "futures-settle" {
		row, err := r.futuresIntent(ctx, data)
		if err != nil {
			return err
		}
		lower, _, ok := r.serverBounds()
		if !ok || row.ObservedMaturesAt == nil || lower.Before(*row.ObservedMaturesAt) {
			return fmt.Errorf("%w: settlement waits for authoritative maturity", errFuturesPrecondition)
		}
		if !futuresNeedsReceipt(*row) {
			if _, exists := r.listedFuture(row.FuturesID); !exists {
				return fmt.Errorf("%w: vanished listing awaits private outcome evidence", errFuturesPrecondition)
			}
		}
		var request struct {
			ID string `json:"futures_id"`
		}
		if json.Unmarshal(data.Action.Body, &request) != nil || request.ID != row.FuturesID {
			return fmt.Errorf("%w: settlement changed its contract identity", errFuturesTerms)
		}
		return nil
	}
	return r.futuresAdmissionPreflight(ctx, data, false)
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
		request.PortID == "" || request.CommodityID == "" || request.TimeInForce != "ioc" ||
		request.Quantity <= 0 || request.Price <= 0 || request.Price > maximumPence/request.Quantity {
		return fmt.Errorf("%w: warehouse order differs from its bounded IOC", errFuturesTerms)
	}
	if action.Stage != "futures-close-stock" || request.Side != "ask" {
		return fmt.Errorf("%w: wind-down must sell, never buy stock", errFuturesTerms)
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
	if futuresLeaseStage(stage) {
		return r.consumeFuturesLease(ctx, mission, data, status, response)
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
	leaseDomainRefusal := stage == "futures-close-lease" && status == http.StatusForbidden && problem.Code == "no_port_presence"
	if futuresAdmissionStageName(stage) && status >= 400 && status < 500 {
		row, err := r.futuresIntent(ctx, data)
		if err != nil {
			return false, err
		}
		if row.FuturesID != "" {
			if status == http.StatusUnauthorized || status == http.StatusTooManyRequests {
				return r.consumeRejection(ctx, mission, data, status, response)
			}
			return r.reconcileCommitted(ctx, mission, data)
		}
	}
	if status == http.StatusUnauthorized || status == http.StatusForbidden && !futuresAdmissionStageName(stage) && !nonParty && !leaseDomainRefusal || status == http.StatusTooManyRequests {
		nextSequence := data.Sequence + 1
		if futuresAdmissionStageName(stage) {
			nextID := ActionID(r.account.ID, mission.Generation, stage, nextSequence)
			if err := r.db.ReadmitFuturesIntent(ctx, r.account.ID, data.Futures.IntentID, nextID); err != nil {
				return false, err
			}
		}
		return r.consumeRejection(ctx, mission, data, status, response)
	}
	if status >= 400 && status < 500 {
		if futuresAdmissionStageName(stage) {
			rows, err := r.futuresActionIntents(ctx, data)
			if err != nil {
				return false, err
			}
			for _, row := range rows {
				if err := r.db.RecordFuturesOutcome(ctx, r.account.ID, row.IntentID, "rejected", response); err != nil {
					return false, err
				}
			}
		}
		mission.Phase = "evaluating"
		data.CooldownUntil = maxTime(data.CooldownUntil, r.gameDelay(1))
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
		if err := r.hold(ctx, "__futures", "definitive futures refusal retained; obligations wait for changed evidence and new admissions continue"); err != nil {
			return false, err
		}
	} else if status >= 200 && status < 300 {
		switch stage {
		case "futures-create", "futures-accept", "futures-accept-batch":
			rows, err := r.futuresActionIntents(ctx, data)
			if err != nil {
				return false, err
			}
			var contracts []gameapi.FuturesContract
			if stage == "futures-accept-batch" {
				if json.Unmarshal(response, &contracts) != nil || len(contracts) != len(rows) {
					return r.reconcileCommitted(ctx, mission, data)
				}
			} else {
				var contract gameapi.FuturesContract
				if json.Unmarshal(response, &contract) != nil || contract.ID == "" {
					return r.reconcileCommitted(ctx, mission, data)
				}
				contracts = []gameapi.FuturesContract{contract}
			}
			for _, row := range rows {
				matched := false
				for _, contract := range contracts {
					if row.ListingID != "" && contract.ID != row.ListingID {
						continue
					}
					if err := r.db.ObserveFuturesContract(ctx, r.account.ID, row.IntentID, contract); err != nil {
						if !errors.Is(err, store.ErrInvalidInput) && !errors.Is(err, store.ErrActionConflict) {
							return false, err
						}
						return r.reconcileCommitted(ctx, mission, data)
					}
					matched = true
					break
				}
				if !matched {
					return r.reconcileCommitted(ctx, mission, data)
				}
			}
		case "futures-settle":
			row, err := r.futuresIntent(ctx, data)
			if err != nil {
				return false, err
			}
			var result gameapi.SettleFuturesResult
			if json.Unmarshal(response, &result) != nil || result.FuturesID != row.FuturesID ||
				(result.Status != "settled" && result.Status != "defaulted") || result.SettledTons < 0 || int64(result.SettledTons) > row.LotTons ||
				result.PenaltyPaidPence < 0 || result.PenaltyPaidPence > maximumPence || result.ClaimPence < 0 || result.ClaimPence > maximumPence {
				return r.reconcileCommitted(ctx, mission, data)
			}
			if err := r.db.RecordFuturesOutcome(ctx, r.account.ID, row.IntentID, result.Status, response); err != nil {
				return false, futuresObservationError(err)
			}
		case "futures-close-stock":
			var result struct {
				tradeResult
				PortID string `json:"port_id"`
				Side   string `json:"side"`
			}
			var request struct {
				PortID      string `json:"port_id"`
				CommodityID string `json:"commodity_id"`
				Side        string `json:"side"`
				Quantity    int64  `json:"quantity"`
			}
			_ = json.Unmarshal(data.Action.Body, &request)
			if json.Unmarshal(response, &result) != nil || !validTrade(result.tradeResult) || result.CommodityID != request.CommodityID ||
				result.PortID != request.PortID || result.Side != "ask" || request.Side != "ask" || *result.FilledQuantity > request.Quantity ||
				*result.TotalGrossPence > maximumPence || *result.CustomsDutyPence > *result.TotalGrossPence ||
				*result.FilledQuantity == 0 && (*result.TotalGrossPence != 0 || *result.CustomsDutyPence != 0) {
				return r.reconcileCommitted(ctx, mission, data)
			}
			recordFuturesStockExit(data.Futures, *result.FilledQuantity, *result.TotalGrossPence-*result.CustomsDutyPence, false)
			if *result.FilledQuantity == 0 {
				data.CooldownUntil = maxTime(data.CooldownUntil, r.gameDelay(1))
			}
		case "futures-close-lease":
			var warehouse futuresWarehouse
			if json.Unmarshal(response, &warehouse) != nil || warehouse.PortID != data.Futures.port() || warehouse.LeasedTonnage != data.Futures.InitialLease {
				return r.reconcileCommitted(ctx, mission, data)
			}
			// The next authoritative warehouse capture proves the lease invariant.
			mission.Phase = "wind-down"
		}
	} else {
		return false, r.hold(ctx, "__futures", "non-definitive futures outcome retains its original key and reservations")
	}
	r.invalidateFutures()
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
	for _, row := range rows {
		if row.FuturesID == "" && r.fresh("futures") {
			var matched *gameapi.FuturesContract
			for _, contract := range contracts {
				ours := contract.SellerCompanyID
				if row.Side == "bid" {
					ours = contract.BuyerCompanyID
				}
				if ours == nil || *ours != r.account.CompanyID || contract.PortID != row.PortID ||
					contract.CommodityID != row.CommodityID || int64(contract.LotTons) != row.LotTons ||
					contract.StrikePricePence != row.StrikePricePence {
					continue
				}
				if row.ListingID != "" {
					if contract.ID != row.ListingID {
						continue
					}
				} else if contract.CreatorCompanyID != r.account.CompanyID || contract.Side != row.Side ||
					contract.CreatedAt.Before(row.CreatedAt.Add(-time.Second)) {
					continue
				}
				if matched != nil {
					return fmt.Errorf("%w: ambiguous listing identity; original request retained", errFuturesEvidence)
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
		if err := r.observeFuturesCash(ctx, row); err != nil {
			return err
		}
	}
	// Ledger attribution precedes terminal receipt validation, including after
	// restart when the event was captured before the corresponding ledger page.
	current, err := r.db.FuturesIntents(ctx, r.account.ID)
	if err != nil {
		return err
	}
	for _, row := range current {
		if row.FuturesID == "" {
			continue
		}
		for _, event := range events {
			var envelope struct {
				CompanyID string                         `json:"company_id"`
				PortID    string                         `json:"port_id"`
				Timestamp time.Time                      `json:"timestamp"`
				Payload   gameapi.FuturesSettlementEvent `json:"payload"`
			}
			if json.Unmarshal(event.Payload, &envelope) != nil || envelope.CompanyID != r.account.CompanyID ||
				envelope.PortID != row.PortID || envelope.Timestamp.IsZero() ||
				envelope.Payload.FuturesID != row.FuturesID {
				continue
			}
			status := envelope.Payload.Status
			if event.EventType == "futures_expired" {
				status = "cancelled"
			} else {
				if event.EventType != "futures_"+status || status != "settled" && status != "defaulted" {
					continue
				}
				candidate := row
				candidate.Status, candidate.Outcome = status, event.Payload
				receipt, valid := store.FuturesSettlement(candidate)
				if !valid || !r.futuresEventParties(row, envelope.Payload) {
					continue
				}
				if status == "settled" {
					notional := int64(receipt.SettledTons) * row.StrikePricePence
					if row.CashDeltaPence == nil ||
						row.Side == "ask" && *row.CashDeltaPence != notional ||
						row.Side == "bid" && (*row.CashDeltaPence > -notional || *row.CashDeltaPence < -2*notional) {
						continue
					}
				}
			}
			if err := r.db.RecordFuturesOutcome(ctx, r.account.ID, row.IntentID, status, event.Payload); err != nil {
				return futuresObservationError(err)
			}
		}
	}
	return nil
}

func (r *Runner) futuresEventParties(row store.FuturesIntent, event gameapi.FuturesSettlementEvent) bool {
	ours, other := event.SellerCompanyID, event.BuyerCompanyID
	if row.Side == "bid" {
		ours, other = event.BuyerCompanyID, event.SellerCompanyID
	}
	return (ours == nil || *ours == r.account.CompanyID) &&
		(row.CounterpartyID == "" || other == nil || *other == row.CounterpartyID)
}

func (r *Runner) observeFuturesCash(ctx context.Context, row store.FuturesIntent) error {
	for _, raw := range decode[[]json.RawMessage](r.snapshot, "ledger") {
		var entry struct {
			ID              string `json:"id"`
			DebitAccountID  string `json:"debit_account_id"`
			CreditAccountID string `json:"credit_account_id"`
			AmountPence     int64  `json:"amount_pence"`
			Metadata        any    `json:"metadata"`
		}
		if json.Unmarshal(raw, &entry) != nil || entry.ID == "" || entry.AmountPence <= 0 ||
			entry.AmountPence > maximumPence || !insuranceReference(entry.Metadata, row.FuturesID, row.FuturesID) {
			continue
		}
		var delta int64
		for _, vault := range r.stateCache.Vaults {
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
	return nil
}

func (r *Runner) reconcileFuturesAction(ctx context.Context, mission *store.Mission, data *missionData, action *store.Action) (bool, error) {
	if futuresLeaseStage(data.Action.Stage) {
		return r.reconcileFuturesLease(ctx, mission, data, action)
	}
	if data.Action.Stage == "futures-close-stock" {
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
			PortID      string `json:"port_id"`
			CommodityID string `json:"commodity_id"`
			Side        string `json:"side"`
			Quantity    int64  `json:"quantity"`
			Price       int64  `json:"price_pence"`
		}
		if err := json.Unmarshal(data.Action.Body, &request); err != nil {
			return false, err
		}
		_, unlocked := r.futuresWarehouse(data.Futures.port(), data.Futures.commodity())
		delta := unlocked - *data.Action.FuturesStockBefore
		expected := -request.Quantity
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
				if fill.PortID != request.PortID || fill.CommodityID != request.CommodityID || fill.Side != "ask" ||
					fill.IsMaker == nil || *fill.IsMaker || fill.Quantity <= 0 || fill.Quantity > request.Quantity || fill.Price <= 0 || fill.Price > maximumPence/fill.Quantity ||
					fill.Customs == nil || *fill.Customs < 0 || *fill.Customs > fill.Price*fill.Quantity ||
					fill.CreatedAt.Before(data.Action.CreatedAt) || fill.CreatedAt.After(r.meta["fills"].UpdatedAt) ||
					fill.Price < request.Price {
					continue
				}
				candidates++
				gross, customs = fill.Price*fill.Quantity, *fill.Customs
			}
		}
		noFill := delta == 0 && candidates == 0 && freshFills && action != nil && action.State == store.ActionComplete
		if delta != expected && !noFill {
			return false, nil
		}
		knownCash := candidates == 1
		if noFill {
			data.CooldownUntil = maxTime(data.CooldownUntil, r.gameDelay(1))
		} else {
			recordFuturesStockExit(data.Futures, -delta, gross-customs, !knownCash)
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
	if data.Action.Stage == "futures-close-lease" {
		warehouse, _ := r.futuresWarehouse(data.Futures.port(), data.Futures.commodity())
		if !r.fresh("warehouses") || warehouse.LeasedTonnage != data.Futures.InitialLease {
			return false, nil
		}
		mission.Phase = "futures_done"
		if action != nil && action.State == store.ActionPending {
			if err := r.db.ReconcileAction(ctx, action.ID, encode(map[string]any{"kind": "authoritative_futures_lease", "warehouse": warehouse})); err != nil {
				return false, err
			}
		}
		data.LastActionID, data.Action = data.Action.ID, nil
		data.Sequence++
		return true, r.saveMission(ctx, mission, data)
	}
	if !futuresAdmissionStageName(data.Action.Stage) && data.Action.Stage != "futures-settle" {
		return false, nil
	}
	selected, err := r.futuresActionIntents(ctx, data)
	if err != nil {
		return false, err
	}
	if data.Action.Stage == "futures-settle" {
		row, err := r.futuresIntent(ctx, data)
		if err != nil {
			return false, err
		}
		selected = []store.FuturesIntent{*row}
	}
	var priorProof struct {
		Kind string `json:"kind"`
	}
	var notAccepted json.RawMessage
	if action != nil && action.State == store.ActionReconciled &&
		json.Unmarshal(action.Response, &priorProof) == nil && priorProof.Kind == "authoritative_futures_not_accepted" {
		notAccepted = action.Response
	}
	for _, row := range selected {
		if notAccepted == nil && row.Status == "rejected" && row.FuturesID == "" &&
			json.Unmarshal(row.Outcome, &priorProof) == nil && priorProof.Kind == "authoritative_futures_not_accepted" {
			notAccepted = row.Outcome
		}
	}
	if notAccepted == nil {
		if err := r.observeFutures(ctx, selected); err != nil {
			return false, err
		}
	}
	if notAccepted != nil && r.fresh("futures") {
		// A later committed listing outranks an earlier negative observation.
		// Missing list authority must still permit replay of durable proof.
		if _, err := r.futuresContracts(); err == nil {
			if err := r.observeFutures(ctx, selected); err != nil {
				return false, err
			}
		}
	}
	rows, err := r.db.FuturesIntents(ctx, r.account.ID)
	if err != nil {
		return false, err
	}
	unobserved := true
	for _, expected := range selected {
		for _, observed := range rows {
			if observed.IntentID == expected.IntentID && observed.FuturesID != "" {
				unobserved = false
			}
		}
	}
	if !unobserved {
		notAccepted = nil
	}
	if notAccepted == nil && unobserved && (data.Action.Stage == "futures-accept" || data.Action.Stage == "futures-accept-batch") {
		if data.Action.FuturesObserveAfter.IsZero() {
			now := time.Now().UTC()
			lastAttempt := data.Action.FuturesAttemptedAt
			if lastAttempt.IsZero() {
				// A crash or a legacy mission may lack the final send timestamp.
				lastAttempt = now
			}
			data.Action.FuturesObserveAfter = maxTime(now, lastAttempt.Add(gameapi.RequestTimeout+5*time.Second))
			r.invalidateFutures()
			return true, r.saveMission(ctx, mission, data)
		}
		if time.Now().Before(data.Action.FuturesObserveAfter) {
			r.wakeBy(data.Action.FuturesObserveAfter)
			return false, nil
		}
		if r.fresh("futures") && r.meta["futures"].AttemptedAt.After(data.Action.FuturesObserveAfter) {
			rejected := false
			proof := make([]gameapi.FuturesContract, 0, len(selected))
			for _, expected := range selected {
				listing, exists := r.listedFuture(expected.ListingID)
				if !exists || listing.CreatorCompanyID != expected.CounterpartyID ||
					listing.PortID != expected.PortID || listing.CommodityID != expected.CommodityID || listing.Side == expected.Side ||
					int64(listing.LotTons) != expected.LotTons || listing.StrikePricePence != expected.StrikePricePence ||
					!listing.MaturesAt.Equal(expected.MaturesAt) {
					continue
				}
				party := listing.SellerCompanyID
				if expected.Side == "bid" {
					party = listing.BuyerCompanyID
				}
				if listing.Status == "open" && party == nil ||
					listing.Status == "active" && party != nil && *party != "" && *party != r.account.CompanyID {
					rejected = true
					proof = append(proof, listing)
				}
			}
			if rejected {
				// Atomic acceptance cannot commit if even one leg did not.
				notAccepted = encode(map[string]any{"kind": "authoritative_futures_not_accepted", "contracts": proof})
			}
		}
	}
	if notAccepted != nil {
		for _, row := range selected {
			if err := r.db.RecordFuturesOutcome(ctx, r.account.ID, row.IntentID, "rejected", notAccepted); err != nil {
				return false, err
			}
		}
		if action != nil && action.State == store.ActionPending {
			if err := r.db.ReconcileAction(ctx, action.ID, notAccepted); err != nil {
				return false, err
			}
		}
		data.LastActionID, data.Action = data.Action.ID, nil
		data.Sequence++
		mission.Phase = "evaluating"
		return true, r.saveMission(ctx, mission, data)
	}
	evidence := make([]map[string]string, 0, len(selected))
	for _, expected := range selected {
		found := false
		for _, observed := range rows {
			if observed.IntentID != expected.IntentID || observed.FuturesID == "" ||
				data.Action.Stage == "futures-settle" && (!store.FuturesTerminal(observed.Status) || futuresNeedsReceipt(observed)) {
				continue
			}
			evidence = append(evidence, map[string]string{"intent_id": observed.IntentID,
				"futures_id": observed.FuturesID, "status": observed.Status})
			found = true
			break
		}
		if !found {
			return false, nil
		}
	}
	if action != nil && action.State == store.ActionPending {
		if err := r.db.ReconcileAction(ctx, action.ID, encode(map[string]any{"kind": "authoritative_futures", "contracts": evidence})); err != nil {
			return false, err
		}
	}
	r.invalidateFutures()
	data.LastActionID, data.Action = data.Action.ID, nil
	data.Sequence++
	return true, r.saveMission(ctx, mission, data)
}

func (r *Runner) closeFuturesWarehouse(ctx context.Context, mission *store.Mission, data *missionData, rows []store.FuturesIntent) (bool, error) {
	data.Futures.PortID, data.Futures.CommodityID = "amsterdam", "water_beer"
	remaining, known := futuresRemainingStock(rows, legacyFuturesCohort, data.Futures)
	if !known {
		return false, r.hold(ctx, "__futures", "futures wind-down waits for exact physical and default claim/penalty receipts")
	}
	warehouse, unlocked := r.futuresWarehouse(data.Futures.port(), data.Futures.commodity())
	if remaining > 0 {
		refused, err := r.futuresRefused(ctx, data, "futures-close-stock")
		if err != nil {
			return false, err
		}
		if refused {
			return false, r.hold(ctx, "__futures", "futures liquidation refusal waits for changed stock or exit quote evidence")
		}
		bid, _ := futuresNPCPrices(r.stateCache.Markets[data.Futures.port()], data.Futures.commodity())
		if unlocked < remaining || !r.fresh("books/amsterdam") || bid <= 0 {
			return false, r.hold(ctx, "__futures", "futures wind-down waits for owned unlocked stock and a normal Amsterdam exit quote")
		}
		data.Futures.StockExitCohort = legacyFuturesCohort
		err = r.stage(ctx, mission, data, ShipState{}, "futures-close-stock", "/v1/market/place-order", map[string]any{
			"port_id": "amsterdam", "commodity_id": "water_beer", "side": "ask", "time_in_force": "ioc", "quantity": 1, "price_pence": bid,
		})
		return data.Action != nil, err
	}
	floor := r.futuresLeaseFloor(data, rows, "amsterdam")
	if floor > data.Futures.InitialLease {
		return false, r.hold(ctx, "__futures", "legacy lease close preserves controller receiving reservations and unrelated stock")
	}
	if r.futuresLeaseClosed(data) {
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

func (r *Runner) closeFuturesSurplus(ctx context.Context, mission *store.Mission, data *missionData, rows []store.FuturesIntent) (bool, error) {
	seen := make(map[string]bool)
	for _, row := range rows {
		if row.CohortID == legacyFuturesCohort || seen[row.CohortID] {
			continue
		}
		seen[row.CohortID] = true
		remaining, known := futuresRemainingStock(rows, row.CohortID, data.Futures)
		if !known || remaining == 0 {
			continue
		}
		_, unlocked := r.futuresWarehouse(row.PortID, row.CommodityID)
		bid, _ := futuresNPCPrices(r.stateCache.Markets[row.PortID], row.CommodityID)
		if unlocked < remaining || bid <= 0 || !r.fresh("books/"+row.PortID) {
			continue
		}
		data.Futures.PortID, data.Futures.CommodityID = row.PortID, row.CommodityID
		data.Futures.StockExitCohort = row.CohortID
		if refused, err := r.futuresRefused(ctx, data, "futures-close-stock"); refused || err != nil {
			if err != nil {
				return false, err
			}
			continue
		}
		err := r.stage(ctx, mission, data, ShipState{}, "futures-close-stock", "/v1/market/place-order", map[string]any{
			"port_id": row.PortID, "commodity_id": row.CommodityID, "side": "ask", "time_in_force": "ioc", "quantity": 1, "price_pence": bid,
		})
		return data.Action != nil, err
	}
	return false, nil
}

func (r *Runner) futuresLeaseClosed(data *missionData) bool {
	if data.Futures == nil || !r.fresh("warehouses") {
		return false
	}
	warehouse, _ := r.futuresWarehouse(data.Futures.port(), data.Futures.commodity())
	return warehouse.LeasedTonnage <= data.Futures.InitialLease
}
