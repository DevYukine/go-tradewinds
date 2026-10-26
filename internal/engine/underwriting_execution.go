package engine

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"slices"
	"strings"
	"time"

	"github.com/DevYukine/go-tradewinds/internal/store"
	"github.com/google/uuid"
	"go.uber.org/zap"
)

type underwritingIntent struct {
	Policy   InsurancePolicy      `json:"policy"`
	Decision UnderwritingDecision `json:"decision"`
}

type underwritingPending struct {
	Regions         []string `json:"regions"`
	CollateralPence int64    `json:"collateral_pence"`
}

func insuranceTermsHash(policy InsurancePolicy) string {
	policy.Status = ""
	policy.CoveredBPS = 0
	policy.UpdatedAt = time.Time{}
	policy.TermsChanged = false
	hash := sha256.Sum256(encode(policy))
	return hex.EncodeToString(hash[:])
}

func (r *Runner) underwritingRegions(policy InsurancePolicy) ([]string, error) {
	home, homeOK := findPort(r.stateCache.Catalog, r.stateCache.HomePortID)
	origin, originOK := findPort(r.stateCache.Catalog, policy.OriginPortID)
	if !homeOK || !originOK || home.BankingRegion == "" || origin.BankingRegion == "" {
		return nil, errors.New("underwriting requires observed home and policy-origin banking regions")
	}
	regions := []string{home.BankingRegion}
	collateral := ceilBPS(policy.InsuredValuePence, 500)
	for _, vault := range r.stateCache.Vaults {
		if vault.AccountType == "vault" && vault.BalancePence >= collateral && !slices.Contains(regions, vault.BankingRegion) {
			regions = append(regions, vault.BankingRegion)
		}
	}
	if !slices.Contains(regions, origin.BankingRegion) {
		regions = append(regions, origin.BankingRegion)
	}
	return regions, nil
}

// The public board is not an owned-slice ledger. Permanent receipts remain the
// source of our exposure even if a policy disappears from the current listing.
func (r *Runner) recordUnderwriting(ctx context.Context) error {
	if r.fresh("insurance") {
		policies := decode[[]InsurancePolicy](r.snapshot, "insurance")
		records := make([]store.UnderwritingPolicyRecord, 0, len(policies))
		for _, policy := range policies {
			if policy.ID == "" || policy.CreatedAt.IsZero() || policy.UpdatedAt.Before(policy.CreatedAt) {
				continue
			}
			records = append(records, store.UnderwritingPolicyRecord{
				AccountID: r.account.ID, PolicyID: policy.ID, TermsHash: insuranceTermsHash(policy),
				Status: policy.Status, Payload: encode(policy), SourceUpdatedAt: policy.UpdatedAt,
				ObservedAt: r.meta["insurance"].UpdatedAt, TermsChanged: policy.TermsChanged,
			})
		}
		if err := r.db.PutUnderwritingPolicies(ctx, r.account.ID, records); err != nil {
			return err
		}
	}
	records, err := r.db.UnderwritingPolicies(ctx, r.account.ID)
	if err != nil {
		return err
	}
	known := make(map[string]store.UnderwritingPolicyRecord, len(records))
	for _, record := range records {
		known[record.PolicyID] = record
	}
	positions, err := r.db.UnderwritingPositions(ctx, r.account.ID)
	if err != nil {
		return err
	}
	positionsChanged := false
	ledger := decode[[]json.RawMessage](r.snapshot, "ledger")
	for _, position := range positions {
		if len(position.FundingEvidence) == 0 && r.fresh("ledger") && r.fresh("vaults") {
			if proof := underwritingFundingEvidence(position, ledger, r.stateCache.Vaults); proof != nil {
				if err := r.db.RecordUnderwritingFunding(ctx, r.account.ID, position.ID, proof); err != nil {
					return err
				}
				positionsChanged = true
			}
		}
		record, exists := known[position.PolicyID]
		if position.ClosedAt != nil || !exists || record.TermsChanged ||
			(record.Status != "settled_safe" && record.Status != "claimed_loss") {
			continue
		}
		var frozen, policy InsurancePolicy
		if json.Unmarshal(position.FrozenPolicy, &frozen) != nil || json.Unmarshal(record.Payload, &policy) != nil ||
			!validHullPolicy(frozen) || !sameInsuranceTerms(frozen, policy) {
			continue
		}
		claim, returned := int64(0), position.CollateralPence
		if record.Status == "claimed_loss" {
			claim, returned = position.CollateralPence, 0
		}
		closure := store.UnderwritingClosure{
			AccountID: r.account.ID, ID: position.ID, Status: record.Status,
			EarnedPremiumPence: position.PremiumPence, ClaimsPence: claim,
			ReturnedCollateralPence: returned, ClosedAt: policy.UpdatedAt, Evidence: record.Payload,
		}
		if err := r.db.CloseUnderwritingPosition(ctx, closure); err != nil {
			return err
		}
		positionsChanged = true
		if entry := r.log.Check(zap.InfoLevel, "underwriting settled"); entry != nil {
			entry.Write(r.scrubOutcomeFields(ctx, []zap.Field{
				zap.Int64("earned_premium_pence", position.PremiumPence), zap.Int64("claims_pence", claim),
				zap.Int64("returned_collateral_pence", returned), zap.Int64("net_pence", position.PremiumPence-claim),
			}, []outcomeText{{field: "policy_id", value: policy.ID}, {field: "underwriting_id", value: position.ID}, {field: "status", value: policy.Status}})...)
		}
	}
	if positionsChanged {
		positions, err = r.db.UnderwritingPositions(ctx, r.account.ID)
		if err != nil {
			return err
		}
	}
	risk, err := r.db.UnderwritingRisk(ctx, r.account.ID)
	if err != nil {
		return err
	}
	r.snapshot["underwriting_positions"] = encode(positions)
	r.snapshot["underwriting_risk"] = encode(risk)
	for _, key := range []string{"underwriting_positions", "underwriting_risk"} {
		r.meta[key] = DatasetMeta{UpdatedAt: time.Now().UTC(), Generation: r.generation, Complete: true}
	}
	delete(r.snapshot, "_underwriting_pending")
	mission, data, err := r.loadMission(ctx, "__underwriting")
	if err != nil {
		return err
	}
	if mission != nil && data.Underwriting != nil && (data.Action != nil || data.LastActionID == "") {
		regions, err := r.underwritingRegions(data.Underwriting.Policy)
		if err != nil {
			return err
		}
		if collateral := data.Underwriting.Decision.CollateralPence; collateral > 0 && collateral <= maximumPence {
			r.snapshot["_underwriting_pending"] = encode(underwritingPending{Regions: regions, CollateralPence: collateral})
		}
	}
	return nil
}

func (r *Runner) underwritingEvidence(ctx context.Context) ([]InsurancePolicy, map[string]bool, error) {
	for _, key := range []string{"insurance", "insurance_owned", "player", "profile", "catalog", "vaults", "rates", "clock"} {
		if !r.fresh(key) {
			return nil, nil, fmt.Errorf("underwriting requires complete current %s", key)
		}
	}
	records, err := r.db.UnderwritingPolicies(ctx, r.account.ID)
	if err != nil {
		return nil, nil, err
	}
	history := make([]InsurancePolicy, 0, len(records))
	known := make(map[string]InsurancePolicy, len(records))
	for _, record := range records {
		var policy InsurancePolicy
		if json.Unmarshal(record.Payload, &policy) != nil {
			return nil, nil, errors.New("permanent underwriting policy evidence is invalid; no additional risk")
		}
		policy.TermsChanged = record.TermsChanged || record.TermsHash != insuranceTermsHash(policy) ||
			record.PolicyID != policy.ID || record.Status != policy.Status
		history = append(history, policy)
		known[policy.ID] = policy
	}
	owned := make(map[string]bool)
	positions, err := r.db.UnderwritingPositions(ctx, r.account.ID)
	if err != nil {
		return nil, nil, err
	}
	for _, position := range positions {
		owned[strings.ToLower(position.PolicyID)] = true
		policy, exists := known[strings.ToLower(position.PolicyID)]
		var frozen InsurancePolicy
		if !exists || policy.TermsChanged || json.Unmarshal(position.FrozenPolicy, &frozen) != nil ||
			!validHullPolicy(frozen) || !sameInsuranceTerms(frozen, policy) ||
			position.ClosedAt != nil && position.Status != policy.Status {
			return nil, nil, errors.New("owned underwriting has contradictory permanent terms or settlement evidence; no additional risk")
		}
		if len(position.FundingEvidence) == 0 {
			return nil, nil, errors.New("accepted underwriting awaits exact ledger-to-vault debit verification; no additional risk")
		}
	}
	for _, policy := range decode[[]InsurancePolicy](r.snapshot, "insurance_owned") {
		if !owned[policy.ID] {
			return nil, nil, errors.New("underwriting membership lacks an exact durable receipt; no additional risk until reconciled")
		}
	}
	return history, owned, nil
}

func (r *Runner) underwritingBudget(ctx context.Context, policy InsurancePolicy, self bool) (UnderwritingBudget, error) {
	input, err := r.committedPlanningInput(ctx, ShipState{})
	if err != nil {
		return UnderwritingBudget{}, err
	}
	if self {
		pending := decode[underwritingPending](r.snapshot, "_underwriting_pending")
		for _, region := range pending.Regions {
			input.ReserveByRegion[region] -= pending.CollateralPence
		}
	}
	_, serverUpper, validClock := input.serverBounds()
	if !validClock {
		return UnderwritingBudget{}, errors.New("underwriting requires fresh authoritative server time")
	}
	budget := UnderwritingBudget{Now: serverUpper, TimeScale: float64(input.Clock.Calendar.TimeScale)}
	var player struct {
		Companies []struct {
			ID string `json:"id"`
		} `json:"companies"`
	}
	if json.Unmarshal(r.snapshot["player"], &player) != nil || len(player.Companies) == 0 {
		return budget, errors.New("underwriting requires the complete observed player company list")
	}
	for _, company := range player.Companies {
		if company.ID == "" {
			return budget, errors.New("underwriting player company identity is incomplete")
		}
		budget.OwnCompanyIDs = append(budget.OwnCompanyIDs, company.ID)
	}
	for _, balance := range input.Balances {
		if balance < 0 || balance > maximumPence {
			return budget, errors.New("underwriting cannot commit while any regional vault is negative or unbounded")
		}
	}
	for _, reserve := range input.ReserveByRegion {
		if reserve < 0 || reserve > maximumPence {
			return budget, errors.New("underwriting protected regional commitments exceed supported money range")
		}
	}
	protected, _, _, funded := measuredFunding(input.Balances, input.ReserveByRegion, input.Rates)
	if !funded {
		return budget, errors.New("underwriting cannot fund every protected regional commitment with observed remittance fees")
	}
	for region, balance := range protected {
		free := balance - input.ReserveByRegion[region]
		if budget.DiscretionaryPence > maximumPence-free {
			return budget, errors.New("underwriting discretionary balance exceeds supported money range")
		}
		budget.DiscretionaryPence += free
	}
	regions, err := r.underwritingRegions(policy)
	if err != nil {
		return budget, err
	}
	budget.AvailableLocalPence = maximumPence
	for _, region := range regions {
		budget.AvailableLocalPence = min(budget.AvailableLocalPence,
			max(int64(0), min(input.Balances[region], protected[region])-input.ReserveByRegion[region]))
	}
	risk, err := r.db.UnderwritingRisk(ctx, r.account.ID)
	if err != nil {
		return budget, err
	}
	budget.RemainingPilotPence = risk.RemainingPence
	if risk.InitialPence == 0 && budget.DiscretionaryPence >= 10 {
		risk, err = r.db.EnsureUnderwritingBudget(ctx, r.account.ID, budget.DiscretionaryPence/10)
		if err != nil {
			return budget, err
		}
		budget.RemainingPilotPence = risk.RemainingPence
	}
	return budget, nil
}

func (r *Runner) pricedUnderwriting(policy InsurancePolicy, history []InsurancePolicy, budget UnderwritingBudget) UnderwritingDecision {
	decision := EvaluateUnderwriting(policy, history, budget)
	var listings []json.RawMessage
	if err := json.Unmarshal(r.snapshot["insurance"], &listings); err != nil {
		decision.Accepted = false
		decision.Reason = fmt.Sprintf("insurance estimates could not be decoded: %v", err)
		return decision
	}
	estimates := make(map[string]LloydsEstimate, len(listings))
	for _, listing := range listings {
		var identity struct {
			ID string `json:"id"`
		}
		if err := json.Unmarshal(listing, &identity); err != nil {
			decision.Accepted = false
			decision.Reason = fmt.Sprintf("insurance estimate identity could not be decoded: %v", err)
			return decision
		}
		var estimate LloydsEstimate
		if err := json.Unmarshal(listing, &estimate); err != nil {
			if strings.EqualFold(identity.ID, policy.ID) {
				decision.Accepted = false
				decision.Reason = fmt.Sprintf("Lloyd's estimate could not be decoded: %v", err)
				return decision
			}
			continue
		}
		if estimate.ExpectedLossBPS != nil {
			estimates[strings.ToLower(estimate.PolicyID)] = estimate
		}
	}
	estimate, hasEstimate := estimates[strings.ToLower(policy.ID)]
	var lloydsNet int64
	if hasEstimate {
		if admission := underwritingAdmission(policy, history, budget); !admission.Accepted {
			return admission
		}
		var ok bool
		lloydsNet, ok = lloydsSliceNet(policy, estimate, decision.SliceBPS)
		if !ok {
			decision.Accepted = false
			decision.Reason = "Lloyd's estimate lacks a positive hull-only slice net or at least 50 bps premium margin"
			return decision
		}
		decision.ExpectedClaimsPence = decision.PremiumPence - lloydsNet
		decision.ExpectedNetPence = lloydsNet - budget.OpportunityCostPence
		decision.EstimatedLossBPS = *estimate.ExpectedLossBPS
		decision.ConservativeLossBPS = *estimate.ExpectedLossBPS
		decision.Accepted = decision.ExpectedNetPence > 0
		decision.Reason = "minimum hull-only pilot has positive net using Lloyd's expected-claim upper bound after shipping opportunity cost; full loss remains affordable"
		if !decision.Accepted {
			decision.Reason = "Lloyd's underwriting net is not positive after shipping opportunity cost"
		}
		if decision.EstimatedLockGameDays <= 0 {
			// The public policy omits hull class; use the slowest observed
			// matching route rather than depending on map iteration order.
			for _, route := range r.stateCache.Routes {
				hull, exists := findHull(r.stateCache.Catalog, route.ShipClass)
				if route.FromPort != policy.OriginPortID || route.ToPort != policy.DestPortID ||
					!exists || hull.BaseSpeedCentiknots <= 0 || route.TotalDistanceNM <= 0 {
					continue
				}
				sailingDays := float64(route.TotalDistanceNM) / (float64(hull.BaseSpeedCentiknots) / 100 * 0.8) / 24
				decision.EstimatedLockGameDays = max(decision.EstimatedLockGameDays, sailingDays)
			}
			if decision.EstimatedLockGameDays <= 0 {
				decision.EstimatedLockGameDays = 15
			}
		}
	}
	if !decision.Accepted {
		return decision
	}
	// This is a modeled opportunity cost, not a cash charge. Shipping and
	// committed fleet capital are given priority before this control mission.
	bestYield := float64(0)
	for _, ship := range r.stateCache.Ships {
		for _, plan := range PlanVoyages(r.planningInput(ship)) {
			capital := growthWorkingCapital(plan)
			if plan.Accepted && capital > 0 && plan.Economics.GameDays > 0 {
				bestYield = max(bestYield, plan.Economics.NetPencePerGameDay/float64(capital))
			}
		}
	}
	opportunity := math.Ceil(float64(decision.CollateralPence) * bestYield * decision.EstimatedLockGameDays)
	if math.IsNaN(opportunity) || math.IsInf(opportunity, 0) || opportunity > float64(maximumPence) {
		decision.Accepted = false
		decision.Reason = "underwriting opportunity cost is not bounded"
		return decision
	}
	budget.OpportunityCostPence = int64(opportunity)
	if hasEstimate {
		decision.ExpectedNetPence = lloydsNet - budget.OpportunityCostPence
		decision.Accepted = decision.ExpectedNetPence > 0
		if !decision.Accepted {
			decision.Reason = "Lloyd's underwriting net is not positive after shipping opportunity cost"
		}
		return decision
	}
	return EvaluateUnderwriting(policy, history, budget)
}

func (r *Runner) considerUnderwriting(ctx context.Context) (bool, error) {
	history, owned, err := r.underwritingEvidence(ctx)
	if err != nil {
		return false, r.hold(ctx, "__underwriting", err.Error())
	}
	var best underwritingIntent
	for _, policy := range decode[[]InsurancePolicy](r.snapshot, "insurance") {
		if owned[policy.ID] || (policy.Status != "open" && policy.Status != "active") ||
			!validHullPolicy(policy) || !retainedInsuranceTermsAgree(policy, history) {
			continue
		}
		budget, err := r.underwritingBudget(ctx, policy, false)
		if err != nil {
			return false, r.hold(ctx, "__underwriting", err.Error())
		}
		decision := r.pricedUnderwriting(policy, history, budget)
		if decision.Accepted && (!best.Decision.Accepted || decision.ExpectedNetPence > best.Decision.ExpectedNetPence ||
			decision.ExpectedNetPence == best.Decision.ExpectedNetPence && policy.ID < best.Policy.ID) {
			best = underwritingIntent{Policy: policy, Decision: decision}
		}
	}
	if !best.Decision.Accepted {
		return false, r.decision(ctx, "__underwriting", "underwriting_wait", "no available hull-only policy has positive modeled net after full-loss funding, directional history and shipping opportunity cost", nil)
	}
	if err := r.decision(ctx, "__underwriting", "underwriting_selected", best.Decision.Reason, best); err != nil {
		return false, err
	}
	mission := &store.Mission{AccountID: r.account.ID, ShipID: "__underwriting", Phase: "underwrite", Generation: uuid.NewString()}
	data := missionData{Plan: Plan{Kind: "underwriting"}, Underwriting: &best, CostsKnown: true}
	return true, r.saveMission(ctx, mission, &data)
}

func (r *Runner) driveUnderwriting(ctx context.Context, mission *store.Mission, data *missionData) (bool, error) {
	if data.Action != nil {
		return false, nil
	}
	if time.Now().Before(data.CooldownUntil) {
		r.wakeBy(data.CooldownUntil)
		return false, nil
	}
	if mission.Phase == "done" || data.Underwriting == nil || data.LastActionID != "" {
		return true, r.db.DeleteMission(ctx, r.account.ID, mission.ShipID)
	}
	if err := r.underwritingPreflight(ctx, data, false); err != nil {
		if holdErr := r.hold(ctx, mission.ShipID, err.Error()); holdErr != nil {
			return false, holdErr
		}
		return true, r.db.DeleteMission(ctx, r.account.ID, mission.ShipID)
	}
	return true, r.stage(ctx, mission, data, ShipState{}, "underwrite", "/v1/insurance/underwrite", map[string]any{"policy_id": data.Underwriting.Policy.ID, "slice_bps": int64(500)})
}

func (r *Runner) underwritingPreflight(ctx context.Context, data *missionData, requestExists bool) error {
	if data.Underwriting == nil || !data.Underwriting.Decision.Accepted {
		return errors.New("underwriting has no frozen admitted policy")
	}
	history, owned, err := r.underwritingEvidence(ctx)
	if err != nil {
		return err
	}
	intent := data.Underwriting
	if owned[intent.Policy.ID] {
		return errors.New("policy already has an owned underwriting receipt")
	}
	if requestExists {
		var request struct {
			PolicyID string `json:"policy_id"`
			SliceBPS int64  `json:"slice_bps"`
		}
		if json.Unmarshal(data.Action.Body, &request) != nil || request.PolicyID != intent.Policy.ID || request.SliceBPS != 500 {
			return errors.New("underwriting request differs from the frozen minimum slice")
		}
	}
	for _, policy := range decode[[]InsurancePolicy](r.snapshot, "insurance") {
		if policy.ID != intent.Policy.ID {
			continue
		}
		if !sameInsuranceTerms(policy, intent.Policy) {
			return errors.New("underwriting policy terms changed before first send")
		}
		if !validHullPolicy(policy) || !retainedInsuranceTermsAgree(policy, history) {
			return errors.New("underwriting policy contradicts permanent terms or terminal evidence")
		}
		budget, err := r.underwritingBudget(ctx, policy, true)
		if err != nil {
			return err
		}
		current := r.pricedUnderwriting(policy, history, budget)
		if !current.Accepted || current.CollateralPence > intent.Decision.CollateralPence || current.PremiumPence < intent.Decision.PremiumPence {
			return fmt.Errorf("underwriting admission changed: %s", current.Reason)
		}
		return nil
	}
	return errors.New("underwriting policy is absent from the complete current board")
}

func (r *Runner) acceptUnderwriting(ctx context.Context, data *missionData, response json.RawMessage) error {
	if data.Underwriting == nil {
		return errors.New("underwriting response has no frozen policy")
	}
	var result struct {
		Policy        InsurancePolicy `json:"policy"`
		Underwritings []struct {
			ID         string    `json:"id"`
			PolicyID   string    `json:"policy_id"`
			CompanyID  string    `json:"underwriter_company_id"`
			SliceBPS   *int64    `json:"slice_bps"`
			Collateral *int64    `json:"collateral_locked_pence"`
			Premium    *int64    `json:"premium_share_pence"`
			CreatedAt  time.Time `json:"created_at"`
			UpdatedAt  time.Time `json:"updated_at"`
		} `json:"underwritings"`
	}
	intent := data.Underwriting
	if json.Unmarshal(response, &result) != nil || !sameInsuranceTerms(result.Policy, intent.Policy) {
		return errors.New("underwriting response lacks matching frozen policy terms")
	}
	var position *store.UnderwritingPosition
	for _, row := range result.Underwritings {
		if row.CompanyID != r.account.CompanyID {
			continue
		}
		if position != nil || !insuranceUUID(row.ID) || row.PolicyID != intent.Policy.ID || row.SliceBPS == nil || *row.SliceBPS != 500 ||
			row.Collateral == nil || *row.Collateral <= 0 || *row.Collateral > intent.Decision.CollateralPence ||
			row.Premium == nil || *row.Premium < intent.Decision.PremiumPence || *row.Premium > maximumPence ||
			row.CreatedAt.IsZero() || row.UpdatedAt.Before(row.CreatedAt) {
			return errors.New("underwriting response has ambiguous ownership, amounts or slice identity")
		}
		position = &store.UnderwritingPosition{
			ID: row.ID, AccountID: r.account.ID, ActionID: data.Action.ID, PolicyID: row.PolicyID, CompanyID: row.CompanyID,
			SliceBPS: *row.SliceBPS, CollateralPence: *row.Collateral, PremiumPence: *row.Premium,
			FrozenPolicy: encode(intent.Policy), AcceptedAt: row.CreatedAt, Status: "active",
		}
	}
	if position == nil {
		return errors.New("underwriting response has no owned slice receipt")
	}
	if r.db.ScrubOutcomeTexts(ctx, r.account.ID, []string{position.ID})[0] != position.ID {
		return errors.New("underwriting receipt identity cannot be retained as public evidence")
	}
	return r.db.PutUnderwritingPosition(ctx, position)
}
