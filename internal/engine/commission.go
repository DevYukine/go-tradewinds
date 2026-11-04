package engine

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"slices"
	"time"

	"github.com/DevYukine/go-tradewinds/internal/gameapi"
	"github.com/DevYukine/go-tradewinds/internal/store"
	"github.com/google/uuid"
)

const commissionMission = "__commission"

// Delivery builds atomically at the yard. Six game hours allow two journalled
// requests and a refresh; they are an execution allowance, not shipbuilding time.
const commissionExecutionHours = 6

type commissionDecision struct {
	Accepted          bool   `json:"accepted"`
	Reason            string `json:"reason"`
	BuildPence        int64  `json:"build_pence"`
	CarryPence        int64  `json:"carry_pence"`
	DeadlineRiskPence int64  `json:"deadline_risk_pence"`
	MarginPence       int64  `json:"margin_pence"`
	FundingFeePence   int64  `json:"funding_fee_pence"`
	NetPence          int64  `json:"net_pence"`
}

type commissionIntent struct {
	Contract              gameapi.CommissionListing `json:"contract"`
	Decision              commissionDecision        `json:"decision"`
	BuildLimitPence       int64                     `json:"build_limit_pence"`
	Accepted              bool                      `json:"accepted"`
	FundingShortfallPence int64                     `json:"funding_shortfall_pence,omitempty"`
}

func commissionTerms(row gameapi.CommissionListing) (gameapi.CommissionMetadata, bool) {
	var metadata gameapi.CommissionMetadata
	valid := json.Unmarshal(row.Metadata, &metadata) == nil && metadata.Admiralty && row.ContractType == "commission" &&
		row.ID != "" && row.OriginPortID != "" && row.ShipClass != nil && *row.ShipClass != "" &&
		metadata.ShipClass == *row.ShipClass && row.RewardPence > 0 && row.RewardPence <= maximumPence &&
		row.CollateralPence >= 0 && row.CollateralPence <= row.RewardPence/4 && row.ExpiresAt != nil && !row.Stale
	return metadata, valid
}

func evaluateCommission(row gameapi.CommissionListing, port Port, quote YardQuote, phase int, now time.Time, scale int64, available, fundingFee int64) commissionDecision {
	decision := commissionDecision{}
	if _, valid := commissionTerms(row); !valid || row.Status != "open" || port.ID != row.OriginPortID ||
		!port.HasShipyard || port.Phase < 2 || port.Phase > phase || port.BankingRegion == "" {
		decision.Reason = "commission requires live Admiralty terms at an unlocked Phase 2 or later yard"
		return decision
	}
	if scale <= 0 || now.IsZero() || !row.ExpiresAt.After(now.Add(time.Duration(commissionExecutionHours)*time.Hour/time.Duration(scale))) {
		decision.Reason = "commission deadline cannot fit the execution allowance"
		return decision
	}
	if quote.PortID != port.ID || quote.ShipClass != *row.ShipClass || quote.BuildMode != "turnkey_cash" ||
		!quote.Available || quote.TotalCostPence == nil || *quote.TotalCostPence <= 0 || *quote.TotalCostPence > maximumPence {
		decision.Reason = "commission lacks its available matching yard quote"
		return decision
	}
	decision.BuildPence = *quote.TotalCostPence
	capital := growthCashTotal(decision.BuildPence, row.CollateralPence)
	if available < capital {
		decision.Reason = "commission build and deposit would spend protected cash"
		return decision
	}
	// Reserve a quarter-day at the documented maximum bank deposit yield.
	// Full deposit loss is the deadline exposure, discounted only by time left.
	decision.CarryPence = ceilRatio(capital, 50, 40_000)
	hoursLeft := int64(row.ExpiresAt.Sub(now).Seconds() * float64(scale) / 3600)
	decision.DeadlineRiskPence = ceilRatio(row.CollateralPence, commissionExecutionHours, max(commissionExecutionHours, hoursLeft))
	decision.MarginPence = ceilBPS(decision.BuildPence, 500)
	decision.FundingFeePence = fundingFee
	decision.NetPence = row.RewardPence - decision.BuildPence - decision.CarryPence - decision.DeadlineRiskPence - fundingFee
	decision.Accepted = decision.NetPence > decision.MarginPence
	decision.Reason = "commission does not clear build cost, deposit carry, deadline risk and five percent margin"
	if decision.Accepted {
		decision.Reason = "commission clears build cost, deposit carry, deadline risk and five percent margin"
	}
	return decision
}

func (r *Runner) commissionListings() ([]gameapi.CommissionListing, error) {
	var rows []gameapi.CommissionListing
	if err := json.Unmarshal(r.snapshot["contracts"], &rows); err != nil {
		return nil, err
	}
	return rows, nil
}

func (r *Runner) commissionAuthority() bool {
	for _, key := range []string{"clock", "catalog", "contracts", "vaults", "ships", "factors", "profile"} {
		if !r.fresh(key) {
			return false
		}
	}
	_, _, valid := r.serverBounds()
	return valid && !r.stateCache.Clock.Paused && r.stateCache.Clock.UnlockedPhase >= 2
}

func (r *Runner) commissionQuote(row gameapi.CommissionListing) (YardQuote, error) {
	port, portExists := findPort(r.stateCache.Catalog, row.OriginPortID)
	if !portExists || row.ShipClass == nil {
		return YardQuote{}, errors.New("commission yard or hull is absent from the catalog")
	}
	hull, hullExists := findHull(r.stateCache.Catalog, *row.ShipClass)
	if !hullExists {
		return YardQuote{}, errors.New("commission hull is absent from the catalog")
	}
	return r.presentYardQuote(port, hull)
}

func (r *Runner) commissionAdmission(row gameapi.CommissionListing, port Port, quote YardQuote, input PlanningInput, paidFee int64) commissionDecision {
	flagship := decode[americaFlagshipState](r.snapshot, "_america_flagship")
	if port.BankingRegion == "america" && flagship.BuiltShipID == "" && (flagship.Pending || flagship.LoanID != "" || !flagship.RetryAt.IsZero()) {
		return commissionDecision{Reason: "America commission waits for the pending flagship to be built"}
	}
	if !slices.Contains(r.stateCache.Factors, port.ID) {
		return commissionDecision{Reason: "commission requires resident factor presence without pinning a trading ship"}
	}
	reserve := input.ReserveByRegion[port.BankingRegion]
	for _, deferred := range input.DeferredLoanReserves {
		if deferred.Region == port.BankingRegion {
			reserve = growthCashTotal(reserve, deferred.FullReserve)
		}
	}
	available := input.Balances[port.BankingRegion] - reserve
	fee := paidFee
	if quote.TotalCostPence != nil {
		capital := growthCashTotal(*quote.TotalCostPence, row.CollateralPence)
		if available < capital && r.fresh("rates") {
			required := fullLoanReserves(input)
			required[port.BankingRegion] = growthCashTotal(required[port.BankingRegion], capital)
			_, gross, net, funded := bestRemittance(input.Balances, required, r.stateCache.Rates, port.BankingRegion)
			if funded && net >= capital-available {
				available += net
				fee += gross - net
			}
		}
	}
	_, upper, _ := r.serverBounds()
	return evaluateCommission(row, port, quote, r.stateCache.Clock.UnlockedPhase, upper,
		r.stateCache.Clock.Calendar.TimeScale, available, fee)
}

func (r *Runner) driveCommissions(ctx context.Context) (bool, error) {
	if !r.opts.Execute || !r.commissionAuthority() {
		return false, nil
	}
	mission, data, err := r.loadMission(ctx, commissionMission)
	if err != nil {
		return false, err
	}
	if mission != nil {
		if data.Action != nil || time.Now().Before(data.CooldownUntil) {
			r.wakeBy(data.CooldownUntil)
			return false, nil
		}
		if mission.Phase == "commission_settle" && data.Settlement != nil {
			if err := r.db.PutSettlement(ctx, data.Settlement); err != nil {
				return false, err
			}
			mission.Phase = "done"
			return true, r.saveMission(ctx, mission, &data)
		}
		if mission.Phase == "done" {
			return true, r.db.DeleteMission(ctx, r.account.ID, commissionMission)
		}
		return r.continueCommission(ctx, mission, &data)
	}
	rows, err := r.commissionListings()
	if err != nil {
		return false, r.hold(ctx, commissionMission, "commission listings unavailable: "+err.Error())
	}
	input, err := r.committedPlanningInput(ctx, ShipState{})
	if err != nil {
		return false, err
	}
	_, upper, _ := r.serverBounds()
	refused := decode[map[string]time.Time](r.snapshot, "_commission_refusals")
	var best *commissionIntent
	for _, row := range rows {
		if row.ContractType != "commission" {
			continue
		}
		if row.Status == "active" && row.CarrierCompanyID != nil && *row.CarrierCompanyID == r.account.CompanyID {
			return false, r.hold(ctx, commissionMission, "existing Admiralty obligation needs its original durable commission intent")
		}
		if row.Status != "open" || upper.Before(refused[row.ID]) {
			continue
		}
		port, exists := findPort(r.stateCache.Catalog, row.OriginPortID)
		if !exists {
			continue
		}
		quote, err := r.commissionQuote(row)
		if err != nil {
			continue
		}
		decision := r.commissionAdmission(row, port, quote, input, 0)
		if err := r.decision(ctx, commissionMission, "commission:"+row.ID, decision.Reason, decision); err != nil {
			return false, err
		}
		if decision.Accepted && (best == nil || decision.NetPence > best.Decision.NetPence) {
			best = &commissionIntent{Contract: row, Decision: decision, BuildLimitPence: decision.BuildPence}
		}
	}
	if best == nil {
		return false, nil
	}
	mission = &store.Mission{AccountID: r.account.ID, ShipID: commissionMission, Generation: uuid.NewString(), Phase: "commission_accept", Destination: best.Contract.OriginPortID}
	data = missionData{Commission: best, BuildPort: best.Contract.OriginPortID, BuildClass: *best.Contract.ShipClass,
		StartedAt: time.Now().UTC(), StartedGameHours: r.stateCache.Clock.Calendar.TotalGameHours, CostsKnown: true}
	return r.continueCommission(ctx, mission, &data)
}

func (r *Runner) continueCommission(ctx context.Context, mission *store.Mission, data *missionData) (bool, error) {
	if data.Commission == nil {
		return false, errors.New("commission mission has no durable terms")
	}
	rows, err := r.commissionListings()
	if err != nil {
		return false, err
	}
	for _, row := range rows {
		if row.ID != data.Commission.Contract.ID {
			continue
		}
		if !data.Commission.Accepted {
			if row.Status != "open" {
				return r.retireCommission(ctx, mission, data)
			}
			return r.prepareCommissionBuild(ctx, mission, data, row)
		}
		if !r.ownedCommissionRow(row, data.Commission.Contract) {
			break
		}
		if row.Status == "defaulted" || row.Status == "completed" {
			return r.finishCommissionTerminal(ctx, mission, data, row, data.LastActionID)
		}
		if row.Status == "active" {
			return r.prepareCommissionBuild(ctx, mission, data, row)
		}
	}
	if !data.Commission.Accepted {
		return r.retireCommission(ctx, mission, data)
	}
	// A fresh listing capture can omit the builder or the order entirely.
	// Check the active yard once before closing a genuinely expired obligation.
	if current, found, err := r.liveCommissionBuilder(ctx, data.Commission.Contract); err != nil {
		return false, err
	} else if found {
		return r.prepareCommissionBuild(ctx, mission, data, current)
	}
	if r.commissionExpired(data.Commission.Contract) {
		return r.settleCommission(ctx, mission, data, data.LastActionID, nil, true, time.Now().UTC())
	}
	return false, r.hold(ctx, commissionMission, "accepted commission waits for its authoritative builder record")
}

func (r *Runner) liveCommissionBuilder(ctx context.Context, original gameapi.CommissionListing) (gameapi.CommissionListing, bool, error) {
	var active []gameapi.CommissionListing
	path := "/v1/contract/list?contract_type=commission&status=active&limit=200&port_id=" + url.QueryEscape(original.OriginPortID)
	if err := r.api.Get(ctx, path, &active); err != nil {
		return gameapi.CommissionListing{}, false, err
	}
	for _, current := range active {
		if current.Status == "active" && r.ownedCommissionRow(current, original) {
			return current, true, nil
		}
	}
	return gameapi.CommissionListing{}, false, nil
}

func (r *Runner) commissionExpired(row gameapi.CommissionListing) bool {
	lower, _, valid := r.serverBounds()
	margin := 15 * time.Minute / time.Duration(max(int64(1), r.stateCache.Clock.Calendar.TimeScale))
	return valid && row.ExpiresAt != nil && lower.After(row.ExpiresAt.Add(margin))
}

func (r *Runner) prepareCommissionBuild(ctx context.Context, mission *store.Mission, data *missionData, row gameapi.CommissionListing) (bool, error) {
	quote, err := r.commissionQuote(row)
	if err != nil {
		return false, r.hold(ctx, commissionMission, err.Error())
	}
	port, _ := findPort(r.stateCache.Catalog, row.OriginPortID)
	input, err := r.committedPlanningInputExcept(ctx, ShipState{}, commissionMission)
	if err != nil {
		return false, err
	}
	target := *quote.TotalCostPence
	required := fullLoanReserves(input)
	if data.Commission.Accepted {
		if target > row.RewardPence+row.CollateralPence {
			return false, r.hold(ctx, commissionMission, "commission build quote exceeds price and recoverable deposit")
		}
		// Delivery restores the build cash and collateral atomically.
		// Other reservations must not cause an avoidable deposit forfeiture.
		required[port.BankingRegion] = target
	} else {
		decision := r.commissionAdmission(row, port, quote, input, data.FeesPence)
		if !decision.Accepted || target > data.Commission.BuildLimitPence {
			return r.retireCommission(ctx, mission, data)
		}
		required[port.BankingRegion] = growthCashTotal(required[port.BankingRegion], target, row.CollateralPence)
	}
	shortfall := required[port.BankingRegion] - input.Balances[port.BankingRegion]
	if shortfall > 0 {
		if data.Commission.FundingShortfallPence > 0 {
			return false, r.hold(ctx, commissionMission, "commission funding remit was already used; no second fee is authorized")
		}
		if !r.fresh("rates") {
			return false, r.hold(ctx, commissionMission, "commission funding needs fresh observed remit fees")
		}
		rate, gross, net, funded := bestRemittance(input.Balances, required, r.stateCache.Rates, port.BankingRegion)
		if !funded || net < shortfall || data.Commission.Accepted && target+gross-net > row.RewardPence+row.CollateralPence {
			return false, r.hold(ctx, commissionMission, "commission has no single reserve-safe, loss-mitigating cash remittance")
		}
		err := r.stage(ctx, mission, data, ShipState{}, "remit", "/v1/treasury/remit", map[string]any{
			"from_region": rate.FromRegion, "to_region": port.BankingRegion, "gross_amount_pence": gross})
		return data.Action != nil, err
	}
	data.Commission.BuildLimitPence = target
	if !data.Commission.Accepted {
		err := r.stage(ctx, mission, data, ShipState{}, "commission-accept", "/v1/contract/accept-commission", gameapi.CommissionActionRequest{ContractID: row.ID})
		return data.Action != nil, err
	}
	if data.BuildName == "" {
		name, ready, err := r.reserveShipName(ctx, commissionMission)
		if err != nil || !ready {
			return false, err
		}
		data.BuildName = name
	}
	err = r.stage(ctx, mission, data, ShipState{}, "commission-deliver", "/v1/contract/deliver-commission",
		gameapi.DeliverCommissionRequest{ContractID: row.ID, Name: data.BuildName, BuildMode: "turnkey_cash"})
	return data.Action != nil, err
}

func (r *Runner) commissionPreflight(ctx context.Context, data *missionData) error {
	intent := data.Commission
	if intent == nil || !r.commissionAuthority() || data.Action == nil {
		return errors.New("commission requires fresh authority and its immutable terms")
	}
	row := intent.Contract
	var request gameapi.DeliverCommissionRequest
	if json.Unmarshal(data.Action.Body, &request) != nil || request.ContractID != row.ID {
		return errors.New("commission action disagrees with the admitted contract")
	}
	rows, err := r.commissionListings()
	if err != nil {
		return err
	}
	var actual *gameapi.CommissionListing
	for index := range rows {
		if rows[index].ID == row.ID {
			actual = &rows[index]
			break
		}
	}
	if actual == nil || actual.OriginPortID != row.OriginPortID || actual.RewardPence != row.RewardPence ||
		actual.CollateralPence != row.CollateralPence || actual.ExpiresAt == nil || row.ExpiresAt == nil ||
		!actual.ExpiresAt.Equal(*row.ExpiresAt) || actual.ShipClass == nil || row.ShipClass == nil || *actual.ShipClass != *row.ShipClass {
		return errors.New("commission live terms no longer match the admitted order")
	}
	if _, valid := commissionTerms(*actual); !valid {
		return errors.New("commission live Admiralty classification is unavailable")
	}
	var quote YardQuote
	path := "/v1/shipyard/quote?port_id=" + url.QueryEscape(row.OriginPortID) + "&ship_class=" + url.QueryEscape(*row.ShipClass) + "&build_mode=turnkey_cash"
	err = r.api.Get(ctx, path, &quote)
	if err != nil {
		return err
	}
	if quote.PortID != row.OriginPortID || quote.ShipClass != *row.ShipClass || quote.BuildMode != "turnkey_cash" ||
		!quote.Available || quote.TotalCostPence == nil || *quote.TotalCostPence <= 0 || *quote.TotalCostPence > maximumPence {
		return errors.New("commission live turnkey quote is unavailable or mismatched")
	}
	port, _ := findPort(r.stateCache.Catalog, row.OriginPortID)
	input, err := r.committedPlanningInputExcept(ctx, ShipState{}, commissionMission)
	if err != nil {
		return err
	}
	_, upper, _ := r.serverBounds()
	if data.Action.Stage == "commission-accept" {
		if intent.Accepted || actual.Status != "open" {
			return errors.New("commission acceptance requires the unaccepted open order")
		}
		decision := r.commissionAdmission(*actual, port, quote, input, data.FeesPence)
		if !decision.Accepted || *quote.TotalCostPence > intent.BuildLimitPence {
			return errors.New("commission no longer clears its admitted cash and margin: " + decision.Reason)
		}
		reserve := input.ReserveByRegion[port.BankingRegion]
		for _, deferred := range input.DeferredLoanReserves {
			if deferred.Region == port.BankingRegion {
				reserve = growthCashTotal(reserve, deferred.FullReserve)
			}
		}
		if input.Balances[port.BankingRegion]-reserve < growthCashTotal(*quote.TotalCostPence, row.CollateralPence) {
			return errors.New("commission acceptance waits for build and deposit funding in the yard region")
		}
		return nil
	}
	if data.Action.Stage != "commission-deliver" || !intent.Accepted || actual.Status != "active" || actual.CarrierCompanyID == nil ||
		*actual.CarrierCompanyID != r.account.CompanyID || request.Name != data.BuildName || request.BuildMode != "turnkey_cash" ||
		!upper.Before(*row.ExpiresAt) || input.Balances[port.BankingRegion] < *quote.TotalCostPence || *quote.TotalCostPence > row.RewardPence+row.CollateralPence {
		return errors.New("commission delivery lacks its active builder, deadline, cash or loss-mitigating build quote")
	}
	return nil
}

func commissionStage(stage string) bool {
	return stage == "commission-accept" || stage == "commission-deliver"
}

func (r *Runner) consumeCommissionResult(ctx context.Context, mission *store.Mission, data *missionData, status int, response json.RawMessage) (bool, error) {
	if data.Commission == nil {
		return false, errors.New("commission receipt has no admitted terms")
	}
	if status >= 400 && status < 500 {
		var problem struct {
			Code   string `json:"code"`
			Detail string `json:"detail"`
		}
		_ = json.Unmarshal(response, &problem)
		presenceRefusal := status == http.StatusForbidden && problem.Code == "no_port_presence"
		if status == http.StatusUnauthorized || status == http.StatusForbidden && !presenceRefusal || status == http.StatusTooManyRequests {
			return r.consumeRejection(ctx, mission, data, status, response)
		}
		stage := data.Action.Stage
		if err := r.decision(ctx, commissionMission, "commission_refused", fmt.Sprintf("%s received definitive HTTP %d: %s", stage, status, problem.Detail), nil); err != nil {
			return false, err
		}
		applyRejectedResult(mission, data, http.StatusConflict, problem.Detail)
		if stage == "commission-accept" {
			refused := decode[map[string]time.Time](r.snapshot, "_commission_refusals")
			if refused == nil {
				refused = make(map[string]time.Time)
			}
			_, upper, clockOK := r.serverBounds()
			for id, until := range refused {
				if clockOK && !upper.Before(until) {
					delete(refused, id)
				}
			}
			refused[data.Commission.Contract.ID] = *data.Commission.Contract.ExpiresAt
			if presenceRefusal {
				refused[data.Commission.Contract.ID] = upper.Add(15 * time.Minute / time.Duration(max(int64(1), r.stateCache.Clock.Calendar.TimeScale)))
			}
			r.snapshot["_commission_refusals"] = encode(refused)
			if err := r.saveSnapshot(ctx); err != nil {
				return false, err
			}
			return r.retireCommission(ctx, mission, data)
		}
		data.CooldownUntil = maxTime(data.CooldownUntil, r.gameDelay(0.25))
		return true, r.saveMission(ctx, mission, data)
	}
	row := data.Commission.Contract
	if status != http.StatusOK {
		return r.reconcileCommitted(ctx, mission, data)
	}
	if data.Action.Stage == "commission-accept" {
		var result gameapi.CommissionResult
		if json.Unmarshal(response, &result) != nil || result.ContractID != row.ID || result.Status != "active" ||
			result.PortID != row.OriginPortID || row.ShipClass == nil || result.ShipClass != *row.ShipClass ||
			!result.Admiralty || result.BuyerCompanyID != nil || result.BuilderCompanyID == nil || *result.BuilderCompanyID != r.account.CompanyID ||
			result.PricePence == nil || *result.PricePence != row.RewardPence || result.CollateralPence == nil || *result.CollateralPence != row.CollateralPence ||
			result.ExpiresAt == nil || row.ExpiresAt == nil || !result.ExpiresAt.Equal(*row.ExpiresAt) {
			return r.reconcileCommitted(ctx, mission, data)
		}
		return r.applyCommissionAcceptance(ctx, mission, data)
	} else {
		var result gameapi.DeliverCommissionResult
		if json.Unmarshal(response, &result) != nil || result.ContractID != row.ID || result.ShipID == "" ||
			result.ShipName != data.BuildName || result.PortID != row.OriginPortID || row.ShipClass == nil || result.ShipClass != *row.ShipClass ||
			result.BuildMode != "turnkey_cash" || result.BuyerCompanyID != nil || result.BuildCostPence == nil || *result.BuildCostPence <= 0 ||
			result.PricePaidPence == nil || *result.PricePaidPence != row.RewardPence || result.CollateralReturnedPence == nil || *result.CollateralReturnedPence != row.CollateralPence {
			return r.reconcileCommitted(ctx, mission, data)
		}
		// Fresh dispatch timestamps its in-memory receipt after the database
		// commit. Use the durable timestamp so crash replay inserts identical P&L.
		action, err := r.db.BeginAction(ctx, &store.Action{ID: data.Action.ID, AccountID: r.account.ID,
			Method: data.Action.Method, Path: data.Action.Path, Body: data.Action.Body})
		if err != nil {
			return false, err
		}
		if action.State != store.ActionComplete || action.UpdatedAt.IsZero() {
			return false, errors.New("commission delivery lacks its durable completed receipt")
		}
		return r.settleCommission(ctx, mission, data, data.Action.ID, &result, false, action.UpdatedAt)
	}
}

func (r *Runner) reconcileCommission(ctx context.Context, mission *store.Mission, data *missionData, action *store.Action) (bool, error) {
	if data.Commission == nil || data.Action == nil || !commissionStage(data.Action.Stage) {
		return false, nil
	}
	if action.State == store.ActionReconciled {
		var evidence struct {
			Kind       string                    `json:"kind"`
			ContractID string                    `json:"contract_id"`
			Contract   gameapi.CommissionListing `json:"contract"`
			ObservedAt time.Time                 `json:"observed_at"`
		}
		if json.Unmarshal(action.Response, &evidence) != nil {
			return false, errors.New("commission reconciliation lacks durable evidence")
		}
		if evidence.Kind == "authoritative_commission_terminal" {
			return r.finishCommissionTerminal(ctx, mission, data, evidence.Contract, action.ID)
		}
		if evidence.Kind == "expired_commission_without_builder" && data.Commission.Accepted &&
			data.Action.Stage == "commission-deliver" && evidence.ContractID == data.Commission.Contract.ID && !evidence.ObservedAt.IsZero() {
			return r.settleCommission(ctx, mission, data, action.ID, nil, true, evidence.ObservedAt)
		}
		if evidence.Kind != "authoritative_commission_accept" || data.Action.Stage != "commission-accept" ||
			evidence.ContractID != data.Commission.Contract.ID {
			return false, errors.New("commission reconciliation lacks its exact durable acceptance evidence")
		}
		return r.applyCommissionAcceptance(ctx, mission, data)
	}
	if !r.fresh("contracts") {
		return false, nil
	}
	rows, err := r.commissionListings()
	if err != nil {
		return false, err
	}
	for _, row := range rows {
		if !r.ownedCommissionRow(row, data.Commission.Contract) {
			continue
		}
		kind := "authoritative_commission_accept"
		if row.Status == "completed" || row.Status == "defaulted" {
			if time.Now().Before(action.CreatedAt.Add(store.ActionReplayWindow)) {
				continue
			}
			kind = "authoritative_commission_terminal"
		} else if row.Status != "active" || data.Action.Stage != "commission-accept" {
			continue
		}
		if action.State == store.ActionPending {
			if err := r.db.ReconcileAction(ctx, action.ID, encode(map[string]any{"kind": kind, "contract_id": row.ID, "contract": row})); err != nil {
				return false, err
			}
		}
		if kind == "authoritative_commission_terminal" {
			return r.finishCommissionTerminal(ctx, mission, data, row, action.ID)
		}
		return r.applyCommissionAcceptance(ctx, mission, data)
	}
	if time.Now().Before(action.CreatedAt.Add(store.ActionReplayWindow)) {
		return false, nil
	}
	if data.Commission.Accepted && data.Action.Stage == "commission-deliver" {
		if _, found, err := r.liveCommissionBuilder(ctx, data.Commission.Contract); err != nil {
			return false, err
		} else if !found && r.commissionExpired(data.Commission.Contract) {
			observed := time.Now().UTC()
			if err := r.db.ReconcileAction(ctx, action.ID, encode(map[string]any{
				"kind": "expired_commission_without_builder", "contract_id": data.Commission.Contract.ID, "observed_at": observed,
			})); err != nil {
				return false, err
			}
			return r.settleCommission(ctx, mission, data, action.ID, nil, true, observed)
		}
	}
	return false, r.hold(ctx, commissionMission, fmt.Sprintf("commission %s retains its unresolved action until exact builder evidence is available", data.Commission.Contract.ID))
}

func (r *Runner) applyCommissionAcceptance(ctx context.Context, mission *store.Mission, data *missionData) (bool, error) {
	data.Commission.Accepted = true
	data.Commission.FundingShortfallPence = 0
	mission.Phase = "commission_build"
	data.LastActionID, data.Action = data.Action.ID, nil
	data.Sequence++
	return true, r.saveMission(ctx, mission, data)
}

func (r *Runner) ownedCommissionRow(row, original gameapi.CommissionListing) bool {
	_, valid := commissionTerms(row)
	return valid && row.ID == original.ID && row.CarrierCompanyID != nil && *row.CarrierCompanyID == r.account.CompanyID &&
		row.OriginPortID == original.OriginPortID && row.RewardPence == original.RewardPence && row.CollateralPence == original.CollateralPence &&
		row.ShipClass != nil && original.ShipClass != nil && *row.ShipClass == *original.ShipClass &&
		row.ExpiresAt != nil && original.ExpiresAt != nil && row.ExpiresAt.Equal(*original.ExpiresAt)
}

func (r *Runner) finishCommissionTerminal(ctx context.Context, mission *store.Mission, data *missionData, row gameapi.CommissionListing, actionID string) (bool, error) {
	if !r.ownedCommissionRow(row, data.Commission.Contract) || actionID == "" ||
		row.Status != "completed" && row.Status != "defaulted" {
		return false, r.hold(ctx, commissionMission, "terminal commission lacks its exact durable economic boundary")
	}
	data.Commission.Accepted = true
	return r.settleCommission(ctx, mission, data, actionID, nil, row.Status == "defaulted", time.Now().UTC())
}

func (r *Runner) settleCommission(ctx context.Context, mission *store.Mission, data *missionData, actionID string, result *gameapi.DeliverCommissionResult, defaulted bool, completed time.Time) (bool, error) {
	if actionID == "" {
		return false, errors.New("commission settlement requires its durable action boundary")
	}
	row := data.Commission.Contract
	settlement := &store.Settlement{ID: mission.Generation, AccountID: r.account.ID, ShipID: commissionMission,
		MissionID: mission.Generation, ActionID: actionID, Kind: "commission",
		Lane: "commission|" + row.OriginPortID + "|" + data.BuildClass, Origin: row.OriginPortID, Destination: row.OriginPortID,
		StartedAt: data.StartedAt, CompletedAt: maxTime(completed, data.StartedAt), FeesPence: data.FeesPence,
		GameSeconds:     max(3_600, int64(completed.Sub(data.StartedAt).Seconds()*float64(r.stateCache.Clock.Calendar.TimeScale))),
		PlannedNetPence: &data.Commission.Decision.NetPence}
	if result != nil {
		settlement.CostsKnown = true
		settlement.AcquisitionPence, settlement.ProceedsPence = *result.BuildCostPence, *result.PricePaidPence
	} else if defaulted {
		settlement.CostsKnown = true
		if data.Commission.Accepted {
			settlement.ExpensesPence = row.CollateralPence
		}
	} else {
		// A terminal listing proves payment, not the actual build debit.
		settlement.ProceedsPence = row.RewardPence
	}
	if data.Action != nil {
		data.Sequence++
	}
	data.LastActionID, data.Action = actionID, nil
	data.Commission.Accepted = true
	data.Settlement, mission.Phase = settlement, "commission_settle"
	return true, r.saveMission(ctx, mission, data)
}

func (r *Runner) retireCommission(ctx context.Context, mission *store.Mission, data *missionData) (bool, error) {
	if data.FeesPence > 0 {
		return r.settleCommission(ctx, mission, data, data.LastActionID, nil, true, time.Now().UTC())
	}
	return true, r.db.DeleteMission(ctx, r.account.ID, commissionMission)
}
