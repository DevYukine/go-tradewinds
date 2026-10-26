package engine

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"time"

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
		data.FeesPence += *result.Fee
		if stage == "remit" && mission.ShipID == "__treasury" {
			mission.Phase = "done"
		}
	case "build":
		var result struct {
			ShipID string `json:"ship_id"`
			Name   string `json:"name"`
			Class  string `json:"ship_class"`
			Cost   *int64 `json:"total_cost_pence"`
		}
		if err := json.Unmarshal(response, &result); err != nil || result.ShipID == "" || result.Cost == nil || result.Name != data.BuildName || result.Class != data.BuildClass {
			return false, r.hold(ctx, mission.ShipID, errUninterpretableResult.Error())
		}
		data.BuiltShipID = result.ShipID
		data.BuildCostPence = *result.Cost
		mission.Phase = "growth_refit"
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
	domainRefusal := status >= 400 && status < 500 && status != 401 && status != 403 && status != 429
	factorStage := stage == "hire-factor" || stage == "dismiss-factor"
	factorDomainRefusal := factorStage && (domainRefusal || status == 403 && (problem.Code == "no_factor_at_cove" || problem.Code == "no_port_presence"))
	if stage == "hire-factor" && factorDomainRefusal && (problem.Code == "no_factor_at_cove" || problem.Code == "no_port_presence") {
		if err := r.recordExpansionAgent(ctx, data); err != nil {
			return false, err
		}
	}
	if stage == "build" && domainRefusal {
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
	// A domain refusal already has a durable, offer-specific latch. It must
	// not delay selling other cargo or accepting a newly changed offer.
	if stage != "deliver-charter" || status == 401 || status == 403 || status == 429 {
		data.CooldownUntil = maxTime(data.CooldownUntil, r.gameDelay(1))
		if status == 409 {
			data.CooldownUntil = maxTime(data.CooldownUntil, time.Now().Add(5*time.Second))
		}
	}
	if stage == "hire-factor" && factorDomainRefusal && problem.Code != "no_factor_at_cove" && problem.Code != "no_port_presence" {
		r.snapshot["_expansion_agent_retry_at"] = encode(data.CooldownUntil)
		if err := r.db.SaveSnapshot(ctx, r.account.ID, r.snapshot); err != nil {
			return false, err
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
	if err := r.saveMission(ctx, mission, data); err != nil {
		return false, err
	}
	if status == 401 || status == 403 && !factorDomainRefusal || (stage == "settings" && status != 429) {
		return false, fmt.Errorf("execution stopped after definitive HTTP %d; journal and mission retained", status)
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
	// Operational intents must release their control row after a definite
	// refusal so fresh state can replan them once the cooldown has elapsed.
	// This is not evidence that cargo, a charter or a quote is unavailable.
	if stage == "build" {
		mission.Phase = "growth_rejected"
	} else if stage == "marque" || stage == "settings" || stage == "hire-factor" || stage == "dismiss-factor" || stage == "remit" && mission.ShipID == "__treasury" {
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
	if data.Action.Stage == "build" && r.fresh("ships") {
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
