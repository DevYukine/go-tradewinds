package engine

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/DevYukine/go-tradewinds/internal/gameapi"
	"github.com/DevYukine/go-tradewinds/internal/store"
)

type tradeResult struct {
	CommodityID      string `json:"commodity_id"`
	FilledQuantity   *int64 `json:"filled_quantity"`
	TotalGrossPence  *int64 `json:"total_gross_pence"`
	CustomsDutyPence *int64 `json:"customs_duty_pence"`
}

type charterTerms struct {
	Origin      string    `json:"origin"`
	Destination string    `json:"destination"`
	Commodity   string    `json:"commodity"`
	Status      string    `json:"status"`
	Required    int64     `json:"required"`
	Delivered   int64     `json:"delivered"`
	Remaining   int64     `json:"remaining"`
	Reward      int64     `json:"reward"`
	Expiry      time.Time `json:"expiry"`
}

func observedCharterTerms(contract Contract) charterTerms {
	terms := charterTerms{
		Origin: contract.OriginPortID, Destination: contract.DestPortID,
		Commodity: contract.CommodityID, Status: contract.Status,
		Required: contract.QuantityRequired, Delivered: contract.QuantityDelivered,
		Remaining: contract.RemainingQuantity, Reward: nominalCharterReward(contract),
	}
	if contract.ExpiresAt != nil {
		terms.Expiry = contract.ExpiresAt.UTC()
	}
	return terms
}

// recordTradeCosts consumes a validated response matched to its immutable request.
// Provision customs belong to the provision cost, not a second fee deduction.
func recordTradeCosts(data *missionData, stage string, result tradeResult) {
	switch stage {
	case "buy":
		data.AcquisitionPence += *result.TotalGrossPence
		data.FeesPence += *result.CustomsDutyPence
	case "sell":
		data.ProceedsPence += *result.TotalGrossPence
		data.FeesPence += *result.CustomsDutyPence
	case "provision":
		data.ProvisionsPence += *result.TotalGrossPence + *result.CustomsDutyPence
	}
}

func validTrade(result tradeResult) bool {
	return result.FilledQuantity != nil && result.TotalGrossPence != nil && result.CustomsDutyPence != nil && *result.FilledQuantity >= 0 && *result.TotalGrossPence >= 0 && *result.CustomsDutyPence >= 0
}

func (r *Runner) consumeResult(ctx context.Context, mission *store.Mission, data *missionData, status int, response json.RawMessage) (bool, error) {
	if data.Action == nil {
		return false, fmt.Errorf("mission %s has no attached response stage", mission.ShipID)
	}
	if commissionStage(data.Action.Stage) {
		return r.consumeCommissionResult(ctx, mission, data, status, response)
	}
	if data.Action.Stage == "voyage-insure" {
		return r.consumeVoyageInsuranceResult(ctx, mission, data, status, response)
	}
	if bulletinStage(data.Action.Stage) {
		if gameapi.BulletinAuthorizationRefusal(status, response) {
			_, err := r.consumeRejection(ctx, mission, data, status, response)
			return false, bulletinAuthorizationError(ctx, status, err)
		}
		return r.consumeBulletinResult(ctx, mission, data, status, response)
	}
	if data.Futures != nil {
		return r.consumeFuturesResult(ctx, mission, data, status, response)
	}
	if passengerStage(data.Action.Stage) {
		return r.consumePassengerResult(ctx, mission, data, status, response)
	}
	var envelope struct {
		Uninterpreted bool `json:"committed_uninterpreted"`
	}
	_ = json.Unmarshal(response, &envelope)
	if status >= 200 && status < 300 && envelope.Uninterpreted {
		return r.reconcileCommitted(ctx, mission, data)
	}
	if status >= 400 && status < 500 {
		return r.consumeRejection(ctx, mission, data, status, response)
	}
	if status < 200 || status >= 300 {
		return false, r.hold(ctx, mission.ShipID, "non-definitive journal outcome retained without stage advancement")
	}
	stage := data.Action.Stage
	switch stage {
	case "rename":
		if status != http.StatusOK || !validShipRenameResult(data, response) {
			return r.reconcileCommitted(ctx, mission, data)
		}
		mission.Phase = "done"
	case "loan-request":
		if status != http.StatusCreated || !r.acceptPlayerLoanRequest(data, response) {
			return r.reconcileCommitted(ctx, mission, data)
		}
		if mission.ShipID == loanRefinanceMission {
			if retired, err := r.loanRefinanceResult(ctx, mission, data, true); retired || err != nil {
				return retired, err
			}
		} else {
			mission.Phase = "growth_loan_request"
		}
	case "loan-accept", "loan-repay":
		if status != http.StatusOK || !r.acceptLoanResult(data, response) {
			return r.reconcileCommitted(ctx, mission, data)
		}
		r.recordLoanReceipt(ctx, response, data.Action.ReceiptAt)
		if mission.ShipID == loanRefinanceMission {
			repaymentConfirmed := true
			if stage == "loan-repay" {
				var loan gameapi.Loan
				if json.Unmarshal(response, &loan) != nil || loan.OwedPence != 0 || loan.Status != "repaid" && loan.Status != "recovered" {
					repaymentConfirmed = false
					if err := r.decision(ctx, loanRefinanceMission, "loan_refinance_wait", "repayment receipt does not confirm the old loan repaid with zero owed; wait for fresh borrower history before a bounded retry", nil); err != nil {
						return false, err
					}
				}
			}
			if retired, err := r.loanRefinanceResult(ctx, mission, data, repaymentConfirmed); retired || err != nil {
				return retired, err
			}
		} else {
			if data.Capacity != nil && data.Capacity.Financing != nil && data.Capacity.Financing.Cancel {
				if err := r.recordCapacityFinancingRefusal(ctx, data); err != nil {
					return false, err
				}
			}
			if data.LoanBridge != nil {
				if err := r.recordLoanBridgeFunding(ctx, data.LoanBridge); err != nil {
					return false, err
				}
				if err := r.recordLoanBridgeDecision(ctx, data.LoanBridge, "Goldsmiths bridge accepted: installment shortfall funded by quote "+data.LoanBridge.Quote.Quote.ID); err != nil {
					return false, err
				}
			}
			loanResultPhase(mission, data)
		}
	case "marque":
		var marque letterOfMarque
		if status != http.StatusCreated || json.Unmarshal(response, &marque) != nil || !validMarque(marque) ||
			data.Marque == nil || marque.SeaZoneID != data.Marque.Quote.SeaZoneID || marque.AddedPiracyBPS != 500 ||
			marque.SponsorCompanyID != "" && marque.SponsorCompanyID != r.account.CompanyID {
			return r.reconcileCommitted(ctx, mission, data)
		}
		data.Marque.IssuedID = marque.ID
		data.Marque.ExpiresAt = marque.ExpiresAt
		mission.Phase = "done"
	case "underwrite":
		if status != http.StatusOK {
			return false, r.hold(ctx, mission.ShipID, "underwriting requires its documented HTTP 200 receipt")
		}
		if err := r.acceptUnderwriting(ctx, data, response); err != nil {
			return false, r.hold(ctx, mission.ShipID, err.Error())
		}
		mission.Phase = "done"
	case "buy", "sell", "provision":
		var result tradeResult
		if err := json.Unmarshal(response, &result); err != nil || !validTrade(result) {
			return false, r.hold(ctx, mission.ShipID, errUninterpretableResult.Error())
		}
		var request struct {
			Quantity    int64  `json:"quantity"`
			CommodityID string `json:"commodity_id"`
		}
		if err := json.Unmarshal(data.Action.Body, &request); err != nil || *result.FilledQuantity > request.Quantity || result.CommodityID != request.CommodityID {
			return false, r.hold(ctx, mission.ShipID, "committed trade response disagrees with immutable request")
		}
		filled := *result.FilledQuantity
		recordTradeCosts(data, stage, result)
		switch stage {
		case "buy":
			data.BoughtQuantity = filled
			data.BuyDone = true
			if data.GrowthLane != nil {
				data.GrowthLane.InitialProcurementComplete = true
			}
			data.Plan.Quantity = filled
			mission.Quantity = filled
			mission.Phase = "freight"
		case "sell":
			data.Eligible[result.CommodityID] = max(0, data.Eligible[result.CommodityID]-filled)
			if result.CommodityID == data.Plan.Commodity {
				data.DeliveredQuantity += filled
			}
		case "provision":
			recordRelocationStores(data, result)
			if data.Plan.Kind == "maintenance" {
				mission.Phase = "maintenance"
				data.Plan.StoresToBuy = max(0, data.Plan.StoresToBuy-filled)
				data.Plan.Economics.StoreCashPence = max(0, data.Plan.Economics.StoreCashPence-*result.TotalGrossPence-*result.CustomsDutyPence)
				if data.Plan.StoresToBuy == 0 {
					data.Plan.Economics.StoreCashPence = 0
				}
			}
		}
		if filled == 0 {
			data.CooldownUntil = r.gameDelay(1)
		}
	case "accept-freight":
		var result struct {
			ContractID  string     `json:"contract_id"`
			CommodityID string     `json:"commodity_id"`
			Destination string     `json:"dest_port_id"`
			Quantity    *int64     `json:"quantity"`
			Reward      *int64     `json:"reward_pence"`
			Collateral  *int64     `json:"collateral_pence"`
			Deadline    *time.Time `json:"delivery_deadline"`
			ShipID      string     `json:"carrier_ship_id"`
		}
		if err := json.Unmarshal(response, &result); err != nil ||
			result.ContractID == "" ||
			result.ShipID != mission.ShipID ||
			result.Quantity == nil ||
			*result.Quantity <= 0 ||
			result.Reward == nil ||
			result.Collateral == nil ||
			result.Deadline == nil ||
			result.Destination != data.Plan.Destination {
			return false, r.hold(ctx, mission.ShipID, errUninterpretableResult.Error())
		}
		var request struct {
			ContractID string `json:"contract_id"`
			ShipID     string `json:"ship_id"`
		}
		if err := json.Unmarshal(data.Action.Body, &request); err != nil || request.ContractID != result.ContractID || request.ShipID != mission.ShipID {
			return false, r.hold(ctx, mission.ShipID, "accepted freight response disagrees with immutable request")
		}
		data.Freight = append(data.Freight, freightObligation{
			ContractID:      result.ContractID,
			Commodity:       result.CommodityID,
			Quantity:        *result.Quantity,
			Destination:     result.Destination,
			Deadline:        result.Deadline,
			RewardPence:     *result.Reward,
			CollateralPence: *result.Collateral,
		})
	case "deliver-freight", "deliver-charter":
		var result struct {
			ContractID  string `json:"contract_id"`
			CommodityID string `json:"commodity_id"`
			Delivered   *int64 `json:"delivered_quantity"`
			Reward      *int64 `json:"reward_paid_pence"`
		}
		if err := json.Unmarshal(response, &result); err != nil || result.Delivered == nil || result.Reward == nil || result.ContractID == "" || *result.Delivered < 0 || *result.Reward < 0 {
			return false, r.hold(ctx, mission.ShipID, errUninterpretableResult.Error())
		}
		var request struct {
			ContractID string `json:"contract_id"`
			ShipID     string `json:"ship_id"`
			Quantity   int64  `json:"quantity"`
		}
		if err := json.Unmarshal(data.Action.Body, &request); err != nil ||
			request.ShipID != mission.ShipID || request.ContractID != result.ContractID {
			return false, r.hold(ctx, mission.ShipID, "committed delivery response disagrees with immutable request")
		}
		if stage == "deliver-charter" {
			if request.Quantity <= 0 || *result.Delivered > request.Quantity {
				return false, r.hold(ctx, mission.ShipID, "charter response exceeds immutable requested quantity")
			}
			data.Eligible[result.CommodityID] = max(0, data.Eligible[result.CommodityID]-*result.Delivered)
			data.DeliveredQuantity += *result.Delivered
		} else {
			found := false
			for i := range data.Freight {
				obligation := &data.Freight[i]
				if obligation.ContractID == request.ContractID && !obligation.Settled &&
					obligation.Commodity == result.CommodityID && obligation.Quantity == *result.Delivered {
					obligation.Settled = true
					data.DeliveredQuantity += *result.Delivered
					found = true
					break
				}
			}
			if !found {
				return false, r.hold(ctx, mission.ShipID, "freight payment does not match the immutable requested unsettled obligation")
			}
		}
		data.ProceedsPence += *result.Reward
		mission.Phase = "arrival"
	case "depart":
		var result struct {
			ShipID      string `json:"ship_id"`
			Destination string `json:"dest_port_id"`
			BerthDues   *int64 `json:"berth_dues_pence"`
			Debt        *int64 `json:"berth_dues_debt_pence"`
		}
		if err := json.Unmarshal(response, &result); err != nil || result.ShipID != mission.ShipID || result.Destination != mission.Destination || result.BerthDues == nil || result.Debt == nil {
			return false, r.hold(ctx, mission.ShipID, errUninterpretableResult.Error())
		}
		if *result.Debt > 0 {
			data.CostsKnown = false
		}
		data.DepartedCargo = data.Action.Before.CargoManifest
		recordDepartureCargoCost(data, data.DepartedCargo)
		mission.Phase = "sailing"
		if data.ReturnJourney {
			mission.Phase = "return_sailing"
		}
	case "dock":
		if data.Plan.Kind == "maintenance" {
			mission.Phase = "maintenance"
		}
	case "offshore":
		if mission.Phase == "closing" {
			data.ClosingObservedAt = time.Now()
		} else if data.Passage != nil {
			mission.Phase = "passenger_loading"
		} else {
			mission.Phase = "maintenance_done"
		}
	case "hire-factor", "dismiss-factor":
		var result struct {
			PortID    string `json:"port_id"`
			HasFactor *bool  `json:"has_factor"`
		}
		var request struct {
			PortID string `json:"port_id"`
		}
		if json.Unmarshal(response, &result) != nil || json.Unmarshal(data.Action.Body, &request) != nil ||
			request.PortID == "" || result.PortID != request.PortID || result.HasFactor == nil || *result.HasFactor != (stage == "hire-factor") {
			return r.reconcileCommitted(ctx, mission, data)
		}
		if stage == "hire-factor" {
			if err := r.recordExpansionAgent(ctx, data); err != nil {
				return false, err
			}
		}
		mission.Phase = "done"
	case "settings":
		var result struct {
			AutoRemitForBills *bool  `json:"auto_remit_for_bills"`
			MinReservePence   *int64 `json:"auto_remit_min_reserve_pence"`
		}
		if err := json.Unmarshal(response, &result); err != nil || result.AutoRemitForBills == nil || result.MinReservePence == nil {
			return false, r.hold(ctx, mission.ShipID, errUninterpretableResult.Error())
		}
		mission.Phase = "done"
	case "remit", "sourcing-remit":
		var result struct {
			Gross *int64 `json:"gross_amount_pence"`
			Fee   *int64 `json:"fee_pence"`
			Net   *int64 `json:"net_credited_pence"`
		}
		var request struct {
			Gross int64 `json:"gross_amount_pence"`
		}
		if err := json.Unmarshal(response, &result); err != nil || result.Gross == nil || result.Fee == nil || result.Net == nil ||
			*result.Gross <= 0 || *result.Fee < 0 || *result.Net < 0 || *result.Gross-*result.Fee != *result.Net ||
			json.Unmarshal(data.Action.Body, &request) != nil || request.Gross != *result.Gross {
			return false, r.hold(ctx, mission.ShipID, errUninterpretableResult.Error())
		}
		companyTransfer := stage == "remit" && (mission.ShipID == "__treasury" || mission.ShipID == "__loans")
		if companyTransfer {
			// No settlement carries this fee and the ledger copy is excluded, so a
			// failed write keeps the receipt for the next pass.
			if err := r.recordCompanyRemittanceFee(ctx, data.Action, *result.Fee); err != nil {
				return false, err
			}
		}
		data.FeesPence += *result.Fee
		if stage == "remit" && data.Commission != nil {
			data.Commission.FundingShortfallPence = max(int64(1), *result.Net)
		}
		if companyTransfer {
			mission.Phase = "done"
		}
	case "build":
		var result struct {
			ShipID   string          `json:"ship_id"`
			Name     string          `json:"name"`
			Class    string          `json:"ship_class"`
			Cost     *int64          `json:"total_cost_pence"`
			Mortgage json.RawMessage `json:"mortgage"`
		}
		if err := json.Unmarshal(response, &result); err != nil || result.ShipID == "" || result.Cost == nil || result.Name != data.BuildName || result.Class != data.BuildClass {
			return false, r.hold(ctx, mission.ShipID, errUninterpretableResult.Error())
		}
		if len(result.Mortgage) != 0 && string(result.Mortgage) != "null" {
			r.recordLoanReceipt(ctx, result.Mortgage, data.Action.ReceiptAt)
		}
		data.BuiltShipID = result.ShipID
		data.BuildCostPence = *result.Cost
		mission.Phase = "growth_refit"
		if err := r.clearCapacitySavingsAfterBuild(ctx); err != nil {
			return false, err
		}
		if err := r.clearExpansionLossAfterBuild(ctx, data.BuildPort); err != nil {
			return false, err
		}
		if data.Capacity != nil && *result.Cost > data.Capacity.QuoteLimitPence {
			reason := "capacity build receipt exceeded the admitted turnkey quote ceiling"
			if err := r.decision(ctx, result.ShipID, "capacity_build_price_overrun", reason, map[string]int64{
				"quote_limit_pence": data.Capacity.QuoteLimitPence,
				"actual_cost_pence": *result.Cost,
			}); err != nil {
				return false, err
			}
			until := time.Now().UTC().Add(capacityHistoryWindow)
			r.snapshot["_capacity_growth_paused_until"] = encode(maxTime(decode[time.Time](r.snapshot, "_capacity_growth_paused_until"), until))
			if err := r.recordGrowthBuildRefusal(ctx, data); err != nil {
				return false, err
			}
		}
	case "refit":
		if data.Capacity != nil {
			data.Capacity.RefitAttempts++
		}
		if data.Plan.Kind == "maintenance" {
			mission.Phase = "maintenance"
		} else if mission.ShipID == "__growth" {
			mission.Phase = "growth_provision"
		}
	default:
		return false, r.hold(ctx, mission.ShipID, "successful response belongs to an unknown durable stage")
	}
	data.LastActionID = data.Action.ID
	data.Sequence++
	data.Action = nil
	return true, r.saveMission(ctx, mission, data)
}

func (r *Runner) consumeRejection(ctx context.Context, mission *store.Mission, data *missionData, status int, response json.RawMessage) (bool, error) {
	var problem struct {
		Code   string `json:"code"`
		Detail string `json:"detail"`
	}
	_ = json.Unmarshal(response, &problem)
	stage := data.Action.Stage
	if err := r.decision(ctx, mission.ShipID, "action_rejected", fmt.Sprintf("%s received definitive HTTP %d %s: %s", stage, status, problem.Code, problem.Detail), map[string]any{"action_id": data.Action.ID, "stage": stage}); err != nil {
		return false, err
	}
	if mission.ShipID == americaFlagshipMission {
		return r.rejectAmericaFlagship(ctx, mission, data, status)
	}
	if mission.ShipID == loanRefinanceMission {
		return r.rejectLoanRefinance(ctx, mission, data, status)
	}
	domainRefusal := status >= 400 && status < 500 && status != 401 && status != 403 && status != 429
	if stage == "rename" && domainRefusal {
		if err := r.recordShipRenameRefusal(ctx, data); err != nil {
			return false, err
		}
		applyRejectedResult(mission, data, status, problem.Detail)
		return true, r.db.DeleteMission(ctx, r.account.ID, mission.ShipID)
	}
	factorStage := stage == "hire-factor" || stage == "dismiss-factor"
	factorDomainRefusal := factorStage && (domainRefusal || status == 403 && (problem.Code == "no_factor_at_cove" || problem.Code == "no_port_presence"))
	if stage == "hire-factor" && factorDomainRefusal {
		if err := r.clearAwaitingExpansion(ctx, data.Action.Body, true); err != nil {
			return false, err
		}
	}
	playerApprovalRefusal := stage == "loan-accept" && data.PlayerLoan != nil && data.Action.Path == "/v1/loan/approve" && domainRefusal
	bridgeRefusal := data.LoanBridge != nil && stage == "loan-accept" && data.Action.Path == "/v1/loan/accept-quote" &&
		(domainRefusal || status == http.StatusTooManyRequests || status == http.StatusForbidden && problem.Code == "goldsmiths_walk_away_ban")
	if bridgeRefusal {
		reason := fmt.Sprintf("Goldsmiths bridge refused: HTTP %d %s: %s", status, problem.Code, problem.Detail)
		if status == http.StatusForbidden && problem.Code == "goldsmiths_walk_away_ban" {
			refused := *data.LoanBridge
			refused.Quote = gameapi.GoldsmithsQuote{}
			if err := r.markLoanBridgeDue(ctx, &refused, reason); err != nil {
				return false, err
			}
		} else if err := r.deferLoanBridge(ctx, data.LoanBridge, reason, data.CooldownUntil); err != nil {
			return false, err
		}
		applyRejectedResult(mission, data, status, problem.Detail)
		return true, r.db.DeleteMission(ctx, r.account.ID, mission.ShipID)
	}
	if stage == "loan-request" && domainRefusal {
		if err := r.recordPlayerLoanDecision(ctx, data.PlayerLoan, "request_refused", "server refused the player request: "+problem.Detail); err != nil {
			return false, err
		}
		if err := r.playerLoanBackoff(ctx, data.PlayerLoan.PortID, data.PlayerLoan.HullID, true, time.Now().UTC()); err != nil {
			return false, err
		}
	}
	if playerApprovalRefusal {
		if err := r.recordPlayerLoanDecision(ctx, data.PlayerLoan, "quote_refused", "approval refused only this quote: "+problem.Detail); err != nil {
			return false, err
		}
	}
	if stage == "loan-accept" && data.Capacity != nil && domainRefusal && !playerApprovalRefusal {
		if err := r.recordCapacityFinancingRefusal(ctx, data); err != nil {
			return false, err
		}
	}
	mortgageRefusal := stage == "build" && data.Capacity != nil && data.Capacity.Mortgage != nil && status == http.StatusConflict &&
		(strings.Contains(strings.ToLower(problem.Code), "mortgage") || strings.Contains(strings.ToLower(problem.Detail), "mortgage still owed"))
	if stage == "build" && data.Capacity != nil && data.Capacity.Financing != nil && domainRefusal {
		verdict := "need_stale"
		if mortgageRefusal {
			verdict = "mortgage_unavailable"
		}
		if err := r.recordCapacityFinanceFailure(ctx, data, verdict, "financed build refused: "+problem.Detail); err != nil {
			return false, err
		}
	}
	if mortgageRefusal {
		r.snapshot["_mortgage_refused_at"] = encode(time.Now().UTC())
		r.snapshot["_mortgage_retry_at"] = encode(maxTime(r.gameDelay(1), time.Now().UTC().Add(30*time.Second)))
		if err := r.saveSnapshot(ctx); err != nil {
			return false, err
		}
	} else if stage == "build" && domainRefusal {
		if err := r.recordGrowthBuildRefusal(ctx, data); err != nil {
			return false, err
		}
	}
	if stage == "marque" && domainRefusal {
		if err := r.recordMarqueRefusal(ctx, data); err != nil {
			return false, err
		}
	}
	if stage == "refit" && data.Capacity != nil {
		data.Capacity.RefitAttempts++
		data.Capacity.RefitRefused = domainRefusal
	}
	if stage == "rename" {
		data.CooldownUntil = maxTime(data.CooldownUntil, time.Now().UTC().Add(shipRenameRetry))
	}
	// A domain refusal already has a durable, offer-specific latch. It must
	// not delay selling other cargo or accepting a newly changed offer.
	if !playerApprovalRefusal && (stage != "deliver-charter" || status == 401 || status == 403 || status == 429) {
		if domainRefusal && (stage == "voyage-insure" || stage == "remit" && data.Commission != nil) {
			data.CooldownUntil = maxTime(data.CooldownUntil, time.Now().UTC().Add(5*time.Second))
		} else {
			data.CooldownUntil = maxTime(data.CooldownUntil, r.gameDelay(1))
			if status == 409 {
				data.CooldownUntil = maxTime(data.CooldownUntil, time.Now().Add(5*time.Second))
			}
		}
	}
	if stage == "deliver-charter" && status != 401 && status != 403 && status != 429 {
		var request struct {
			ContractID string `json:"contract_id"`
		}
		if json.Unmarshal(data.Action.Body, &request) == nil && r.fresh("contracts") {
			if contract, exists := r.contract(request.ContractID); exists && !contract.Stale {
				if data.RejectedCharterTerms == nil {
					data.RejectedCharterTerms = make(map[string]charterTerms)
				}
				data.RejectedCharterTerms[request.ContractID] = observedCharterTerms(contract)
			}
		}
	}
	applyRejectedResult(mission, data, status, problem.Detail)
	if stage == "loan-request" && domainRefusal {
		// A refused borrower request acquired no hull or trading obligation.
		// Do not invent an aborted trade settlement from its journal receipt.
		return true, r.db.DeleteMission(ctx, r.account.ID, mission.ShipID)
	}
	if err := r.saveMission(ctx, mission, data); err != nil {
		return false, err
	}
	if status == 401 || status == 403 && !factorDomainRefusal || (stage == "settings" && status != 429) {
		return false, fmt.Errorf("%w after definitive HTTP %d; journal and mission retained", ErrExecutionStopped, status)
	}
	return true, nil
}

// Authorization and throttling reject a request, not its cargo, charter or quote.
// Consume their definite journal outcome without inventing a domain refusal.
func applyRejectedResult(mission *store.Mission, data *missionData, status int, detail string) {
	action := data.Action
	stage := action.Stage
	data.LastActionID = action.ID
	data.Sequence++
	data.Action = nil
	if (status == 401 || status == 403 || status == 429) &&
		(stage == "loan-request" || stage == "loan-accept" || stage == "loan-repay" || stage == "build" && data.Capacity != nil && data.Capacity.Financing != nil) {
		return
	}
	if stage == "loan-request" {
		data.PlayerLoan = nil
		mission.Phase = "growth_rejected"
		return
	}
	if stage == "loan-accept" && action.Path == "/v1/loan/approve" && data.PlayerLoan != nil {
		var request gameapi.QuoteActionRequest
		_ = json.Unmarshal(action.Body, &request)
		data.PlayerLoan.RejectedQuotes = append(data.PlayerLoan.RejectedQuotes, request.QuoteID)
		data.Capacity = nil
		mission.Phase = "growth_loan_request"
		return
	}
	// Operational intents must release their control row after a definite
	// refusal so fresh state can replan them once the cooldown has elapsed.
	// This is not evidence that cargo, a charter or a quote is unavailable.
	if stage == "build" {
		mission.Phase = "growth_rejected"
		if data.Capacity != nil && data.Capacity.Financing != nil {
			mission.Phase = "growth_finance_repay"
		}
	} else if stage == "loan-accept" || stage == "loan-repay" && mission.ShipID == "__growth" {
		mission.Phase = "growth_finance_repay"
	} else if stage == "loan-repay" || stage == "marque" || stage == "settings" || stage == "hire-factor" || stage == "dismiss-factor" || stage == "remit" && (mission.ShipID == "__treasury" || mission.ShipID == "__loans") {
		mission.Phase = "done"
	} else if data.Plan.Kind == "maintenance" {
		mission.Phase = "maintenance"
		if stage == "offshore" {
			mission.Phase = "maintenance_done"
		}
	}
	if status == 401 || status == 403 || status == 429 {
		return
	}
	if stage == "deliver-freight" && status == 409 {
		var request struct {
			ContractID string `json:"contract_id"`
		}
		_ = json.Unmarshal(action.Body, &request)
		for i := range data.Freight {
			if data.Freight[i].ContractID == request.ContractID {
				data.Freight[i].TenderRefused = true
				data.Freight[i].TenderRetryAt = data.CooldownUntil
			}
		}
		mission.Phase = "tender_wait"
	} else if stage == "buy" {
		data.BuyDone = true
		if data.GrowthLane != nil {
			data.GrowthLane.InitialProcurementComplete = true
		}
		mission.Phase = "freight"
	} else if stage == "accept-freight" {
		var request struct {
			ContractID string `json:"contract_id"`
		}
		_ = json.Unmarshal(action.Body, &request)
		removeFreightCandidate(&data.Plan, request.ContractID)
	} else if stage == "deliver-charter" {
		// A definite refusal applies to this charter, not every carried-in
		// lot or every other charter at the port.
		var request struct {
			ContractID string `json:"contract_id"`
		}
		_ = json.Unmarshal(action.Body, &request)
		if data.RejectedCharterIDs == nil {
			data.RejectedCharterIDs = make(map[string]bool)
		}
		data.RejectedCharterIDs[request.ContractID] = true
	}
}

func (r *Runner) reconcileCommitted(ctx context.Context, mission *store.Mission, data *missionData) (bool, error) {
	known := &store.Action{ID: data.Action.ID, Body: data.Action.Body, State: store.ActionComplete}
	if changed, err := r.reconcileObservedAction(ctx, mission, data, known); changed || err != nil {
		return changed, err
	}
	if data.Action.Stage == "hire-factor" && r.fresh("factors") {
		var request struct {
			PortID string `json:"port_id"`
		}
		if json.Unmarshal(data.Action.Body, &request) == nil && request.PortID != "" {
			for _, portID := range r.stateCache.Factors {
				if portID != request.PortID {
					continue
				}
				if err := r.recordExpansionAgent(ctx, data); err != nil {
					return false, err
				}
				data.CostsKnown = false
				data.LastActionID = data.Action.ID
				data.Sequence++
				data.Action = nil
				mission.Phase = "done"
				return true, r.saveMission(ctx, mission, data)
			}
		}
	}
	if data.Action.Stage == "build" && r.fresh("ships") && (data.Capacity == nil || data.Capacity.Mortgage == nil) {
		for _, candidate := range r.stateCache.Ships {
			if candidate.Ship.Name == data.BuildName && candidate.Ship.ShipClass == data.BuildClass {
				data.BuiltShipID = candidate.Ship.ID
				data.CostsKnown = false
				return r.applyReconciliation(ctx, mission, data)
			}
		}
	}
	return false, r.hold(ctx, mission.ShipID, "HTTP success committed but uninterpreted; persisted outcome blocks a fresh logical request until authoritative reconciliation")
}

func (r *Runner) gameDelay(hours float64) time.Time {
	scale := max(int64(1), r.stateCache.Clock.Calendar.TimeScale)
	return time.Now().UTC().Add(time.Duration(hours*3_600/float64(scale)) * time.Second)
}

func maxTime(a, b time.Time) time.Time {
	if a.After(b) {
		return a
	}
	return b
}
