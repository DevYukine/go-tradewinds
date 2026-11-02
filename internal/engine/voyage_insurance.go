package engine

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"strings"
	"time"

	"github.com/DevYukine/go-tradewinds/internal/gameapi"
	"github.com/DevYukine/go-tradewinds/internal/store"
)

type voyageInsuranceIntent struct {
	Request         gameapi.CreateInsurancePolicyRequest `json:"request"`
	Quote           gameapi.LloydsQuoteResult            `json:"quote"`
	ReturnJourney   bool                                 `json:"return_journey"`
	PolicyID        string                               `json:"policy_id,omitempty"`
	OfferedAt       time.Time                            `json:"offered_at"`
	ServerNotBefore time.Time                            `json:"server_not_before"`
	Declined        bool                                 `json:"declined,omitempty"`
}

func oceanicVoyage(route Route, zones []Zone) bool {
	for _, leg := range route.Legs {
		if leg.EdgeClass == "oceanic" {
			return true
		}
		for _, zone := range zones {
			if zone.ZoneClass == "oceanic" && (leg.FromNode == zone.ID || leg.ToNode == zone.ID) {
				return true
			}
		}
	}
	return false
}

func worthwhileVoyageCover(hull Hull, route Route, zones []Zone, quote gameapi.LloydsQuoteResult) bool {
	if hull.HullCategory != "oceanic" || !oceanicVoyage(route, zones) || !quote.LloydsWrites ||
		quote.OriginPortID != route.FromPort || quote.DestPortID != route.ToPort || quote.ShipID == "" ||
		quote.HullValuePence <= 0 || quote.MaxInsurablePence < quote.HullValuePence || quote.MaxInsurablePence > maximumPence ||
		quote.InsuredValuePence != quote.MaxInsurablePence || len(quote.PricedConsortShipIDs) != 0 ||
		quote.LloydsExpectedLossBPS == nil || *quote.LloydsExpectedLossBPS < 0 || *quote.LloydsExpectedLossBPS > 10_000 ||
		quote.LloydsExpectedLossPence == nil || *quote.LloydsExpectedLossPence <= 0 || *quote.LloydsExpectedLossPence > maximumPence ||
		quote.LloydsPremiumBPS == nil || *quote.LloydsPremiumBPS < 10 || *quote.LloydsPremiumBPS > 3_000 ||
		quote.LloydsPremiumPence == nil || *quote.LloydsPremiumPence <= 0 || *quote.LloydsPremiumPence > maximumPence {
		return false
	}
	return *quote.LloydsPremiumPence-*quote.LloydsExpectedLossPence <= *quote.LloydsExpectedLossPence
}

func voyageInsuranceForLeg(data *missionData, origin, destination string) *voyageInsuranceIntent {
	for index := len(data.VoyageInsurance) - 1; index >= 0; index-- {
		intent := &data.VoyageInsurance[index]
		if intent.ReturnJourney == data.ReturnJourney && intent.Quote.OriginPortID == origin && intent.Request.DestPortID == destination {
			return intent
		}
	}
	return nil
}

func voyageLegInsured(data *missionData, origin, destination string) bool {
	intent := voyageInsuranceForLeg(data, origin, destination)
	return intent != nil && intent.PolicyID != ""
}

func hasIssuedVoyageInsurance(data *missionData) bool {
	for _, intent := range data.VoyageInsurance {
		if intent.PolicyID != "" {
			return true
		}
	}
	return false
}

func (r *Runner) voyageInsuranceWaitSafe(ship ShipState, hull Hull, route Route, data *missionData) bool {
	_, upper, valid := r.serverBounds()
	if !valid || r.stateCache.Clock.Calendar.TimeScale <= 0 {
		return false
	}
	wait := 6 * time.Hour / time.Duration(r.stateCache.Clock.Calendar.TimeScale)
	if stops := freightStops(data.Freight); len(stops) > 0 && r.freightDepartureAt(ship, route.ToPort, hull, stops, upper.Add(wait)) != nil {
		return false
	}
	if data.Plan.ContractID != "" && !data.ReturnJourney {
		contract, exists := r.contract(data.Plan.ContractID)
		if !exists || contract.ExpiresAt == nil {
			return false
		}
		arrival := procurementArrival(r.planningInput(ship), data.Plan, max(data.Plan.Quantity, data.BoughtQuantity))
		slack := 24 * time.Hour / time.Duration(r.stateCache.Clock.Calendar.TimeScale)
		if arrival.IsZero() || !arrival.Add(wait+slack).Before(*contract.ExpiresAt) {
			return false
		}
	}
	return true
}

func (r *Runner) voyagePremiumReserves(input *PlanningInput, mission *store.Mission, data *missionData, ship ShipState) {
	protected := *data
	protected.VoyageInsurance = nil
	r.reserveMissionCash(input.ReserveByRegion, mission, &protected, ship)
}

// Loading and the final liquidation comparison must finish before this call:
// later purchases void the policy's declared-cargo cover.
func (r *Runner) prepareVoyageInsurance(ctx context.Context, ship ShipState, hull Hull, route Route, mission *store.Mission, data *missionData) (bool, bool, error) {
	if hull.HullCategory != "oceanic" || !oceanicVoyage(route, r.stateCache.Catalog.SeaZones) {
		return false, false, nil
	}
	intent := voyageInsuranceForLeg(data, route.FromPort, route.ToPort)
	if intent != nil && intent.Declined {
		return false, false, nil
	}
	if !r.fresh("insurance") {
		if intent != nil && intent.PolicyID != "" {
			return true, false, r.hold(ctx, ship.Ship.ID, "durable voyage policy needs the fresh insurance board")
		}
		return false, false, nil
	}
	for _, policy := range r.stateCache.InsurancePolicies {
		if policy.ShipID != ship.Ship.ID || policy.InsuredCompanyID != r.account.CompanyID ||
			(intent != nil && intent.PolicyID != "" && policy.ID != intent.PolicyID) {
			continue
		}
		if (intent == nil || intent.PolicyID == "") && policy.Status != "open" && policy.Status != "active" {
			continue
		}
		if policy.OriginPortID != route.FromPort || policy.DestPortID != route.ToPort {
			return true, false, r.hold(ctx, ship.Ship.ID, "observed voyage cover does not match the departure")
		}
		if intent == nil {
			data.CostsKnown = false
		}
		// Lloyd's first looks after an hour and repeats every couple of hours.
		// Its capacity is not guaranteed: do not burn fifteen days waiting for it.
		until := policy.CreatedAt.Add(6 * time.Hour / time.Duration(max(int64(1), r.stateCache.Clock.Calendar.TimeScale)))
		_, upper, clockOK := r.serverBounds()
		if policy.CoveredBPS < 10_000 && (policy.Status == "open" || policy.Status == "active") &&
			(!clockOK || upper.Before(until)) && r.voyageInsuranceWaitSafe(ship, hull, route, data) {
			r.wakeBy(until)
			return true, false, r.holdExpected(ctx, ship.Ship.ID, "waiting up to six game hours for Lloyd's voyage cover")
		}
		return false, false, nil
	}
	if intent != nil && intent.PolicyID != "" {
		return true, false, r.hold(ctx, ship.Ship.ID, "durable voyage policy is missing from the authoritative board")
	}
	if !r.voyageInsuranceWaitSafe(ship, hull, route, data) {
		return false, false, nil
	}
	quote, err := r.lloydsVoyageQuote(ctx, ship.Ship.ID, route.ToPort, 10_000)
	if err != nil {
		return false, false, nil
	}
	if quote.MaxInsurablePence > 0 && quote.MaxInsurablePence <= maximumPence && quote.MaxInsurablePence != quote.InsuredValuePence {
		quote, err = r.lloydsVoyageQuote(ctx, ship.Ship.ID, route.ToPort, quote.MaxInsurablePence)
		if err != nil {
			return false, false, nil
		}
	}
	if quote.ShipID != ship.Ship.ID || !worthwhileVoyageCover(hull, route, r.stateCache.Catalog.SeaZones, quote) {
		return false, false, nil
	}
	port, exists := findPort(r.stateCache.Catalog, route.FromPort)
	if !exists {
		return true, false, errors.New("insured departure port is absent from catalog")
	}
	input, err := r.committedPlanningInputExcept(ctx, ship, mission.ShipID)
	if err != nil {
		return true, false, err
	}
	r.voyagePremiumReserves(&input, mission, data, ship)
	if quote.InsuredValuePence <= sourcingFreeCash(input) ||
		input.Balances[port.BankingRegion]-input.ReserveByRegion[port.BankingRegion] < *quote.LloydsPremiumPence ||
		data.Relocation != nil && !r.relocationBudget(data, *quote.LloydsPremiumPence) {
		return false, false, nil
	}
	if data.Relocation != nil {
		probe := *data
		probe.FeesPence = growthCashTotal(probe.FeesPence, *quote.LloydsPremiumPence)
		if err := r.relocationDepartureReady(ctx, ship, &probe); err != nil {
			return false, false, nil
		}
	}
	lower, _, clockOK := r.serverBounds()
	if !clockOK {
		return false, false, nil
	}
	admitted := voyageInsuranceIntent{Request: gameapi.CreateInsurancePolicyRequest{ShipID: ship.Ship.ID,
		DestPortID: route.ToPort, InsuredValuePence: quote.InsuredValuePence, PremiumBPS: *quote.LloydsPremiumBPS},
		Quote: quote, ReturnJourney: data.ReturnJourney, ServerNotBefore: lower}
	if intent == nil {
		data.VoyageInsurance = append(data.VoyageInsurance, admitted)
	} else {
		*intent = admitted
	}
	data.VoyageInsuranceSettledAt = time.Time{}
	return true, true, r.stage(ctx, mission, data, ship, "voyage-insure", "/v1/insurance/create", admitted.Request)
}

func (r *Runner) voyageInsurancePreflight(ctx context.Context, mission *store.Mission, data *missionData) error {
	var request gameapi.CreateInsurancePolicyRequest
	if json.Unmarshal(data.Action.Body, &request) != nil {
		return errors.New("voyage insurance lacks its immutable request")
	}
	intent := voyageInsuranceForLeg(data, data.Action.Before.Ship.CurrentPortID, request.DestPortID)
	if intent == nil || intent.PolicyID != "" || intent.ServerNotBefore.IsZero() ||
		!r.fresh("insurance") || !r.fresh("ships") || !r.fresh("vaults") || !r.fresh("zones") {
		return errors.New("voyage insurance requires its unspent intent and fresh authoritative observations")
	}
	if request != intent.Request {
		return errors.New("voyage insurance immutable request differs from admitted cover")
	}
	ship, exists := r.ship(request.ShipID)
	if !exists || ship.Voyage != nil || ship.Ship.ID != mission.ShipID ||
		ship.Ship.Status != "berthed" && ship.Ship.Status != "anchored_roadstead" || ship.Ship.CurrentPortID != intent.Quote.OriginPortID ||
		mission.Destination != request.DestPortID {
		return errors.New("voyage insurance requires the same loaded ship and destination in port")
	}
	for _, policy := range r.stateCache.InsurancePolicies {
		if policy.ShipID == ship.Ship.ID && policy.InsuredCompanyID == r.account.CompanyID && (policy.Status == "open" || policy.Status == "active") {
			return errors.New("ship already has an open voyage policy")
		}
	}
	hull, found := findHull(r.stateCache.Catalog, ship.Ship.ShipClass)
	key := routeKey(ship.Ship.CurrentPortID, request.DestPortID, hull.ID)
	route, routeExists := r.stateCache.Routes[key]
	if !found || !routeExists || !r.fresh("routes/"+key) || ValidateRoute(route, hull, r.stateCache.Catalog.SeaZones) != nil {
		return errors.New("voyage insurance lacks its safe observed ocean route")
	}
	quote, err := r.lloydsVoyageQuote(ctx, ship.Ship.ID, request.DestPortID, request.InsuredValuePence)
	if err != nil {
		return err
	}
	if quote.ShipID != ship.Ship.ID || !worthwhileVoyageCover(hull, route, r.stateCache.Catalog.SeaZones, quote) ||
		*quote.LloydsPremiumBPS != request.PremiumBPS || *quote.LloydsPremiumPence > *intent.Quote.LloydsPremiumPence {
		return errors.New("voyage cover no longer clears its immutable price and twice-expected-loss limit")
	}
	input, err := r.committedPlanningInputExcept(ctx, ship, mission.ShipID)
	if err != nil {
		return err
	}
	port, _ := findPort(r.stateCache.Catalog, ship.Ship.CurrentPortID)
	r.voyagePremiumReserves(&input, mission, data, ship)
	if quote.InsuredValuePence <= sourcingFreeCash(input) {
		return errors.New("voyage value can be self-insured from free liquid cash")
	}
	if input.Balances[port.BankingRegion]-input.ReserveByRegion[port.BankingRegion] < *quote.LloydsPremiumPence {
		return errors.New("voyage premium would spend protected operating cash")
	}
	if !r.voyageInsuranceWaitSafe(ship, hull, route, data) {
		return errors.New("voyage insurance wait would consume its delivery deadline")
	}
	if data.Relocation != nil {
		probe := *data
		probe.FeesPence = growthCashTotal(probe.FeesPence, *quote.LloydsPremiumPence)
		if err := r.relocationDepartureReady(ctx, ship, &probe); err != nil {
			return err
		}
	}
	return nil
}

func matchingVoyagePolicy(policy InsurancePolicy, intent *voyageInsuranceIntent, company string) bool {
	return intent != nil && completeInsurancePolicy(policy) && policy.InsuredCompanyID == company &&
		policy.ShipID == intent.Request.ShipID && policy.OriginPortID == intent.Quote.OriginPortID && policy.DestPortID == intent.Request.DestPortID &&
		policy.InsuredValuePence == intent.Request.InsuredValuePence && policy.HullValuePence == intent.Quote.HullValuePence &&
		policy.PremiumBPS == intent.Request.PremiumBPS && intent.Quote.LloydsPremiumPence != nil && policy.TotalPremiumPence == *intent.Quote.LloydsPremiumPence
}

func (r *Runner) applyVoyageInsurance(ctx context.Context, mission *store.Mission, data *missionData, policy InsurancePolicy) (bool, error) {
	intent := voyageInsuranceForLeg(data, policy.OriginPortID, policy.DestPortID)
	if !matchingVoyagePolicy(policy, intent, r.account.CompanyID) || intent.PolicyID != "" {
		return false, r.hold(ctx, mission.ShipID, "voyage insurance receipt does not match its single durable offer")
	}
	intent.PolicyID, intent.OfferedAt = policy.ID, policy.CreatedAt
	data.FeesPence = growthCashTotal(data.FeesPence, policy.TotalPremiumPence)
	data.LastActionID, data.Action = data.Action.ID, nil
	data.Sequence++
	return true, r.saveMission(ctx, mission, data)
}

func (r *Runner) consumeVoyageInsuranceResult(ctx context.Context, mission *store.Mission, data *missionData, status int, response json.RawMessage) (bool, error) {
	if status >= 400 && status < 500 {
		if status != http.StatusUnauthorized && status != http.StatusForbidden && status != http.StatusTooManyRequests {
			var request gameapi.CreateInsurancePolicyRequest
			if json.Unmarshal(data.Action.Body, &request) == nil {
				if intent := voyageInsuranceForLeg(data, data.Action.Before.Ship.CurrentPortID, request.DestPortID); intent != nil {
					intent.Declined = true
				}
			}
		}
		return r.consumeRejection(ctx, mission, data, status, response)
	}
	var policy InsurancePolicy
	var result gameapi.CreateInsurancePolicyResult
	if status != http.StatusCreated || json.Unmarshal(response, &policy) != nil || json.Unmarshal(response, &result) != nil ||
		result.AboveLloydsPrice || result.LloydsPremiumBPS == nil || *result.LloydsPremiumBPS != policy.PremiumBPS ||
		!matchingVoyagePolicy(policy, voyageInsuranceForLeg(data, policy.OriginPortID, policy.DestPortID), r.account.CompanyID) {
		return r.reconcileCommitted(ctx, mission, data)
	}
	return r.applyVoyageInsurance(ctx, mission, data, policy)
}

func (r *Runner) reconcileVoyageInsurance(ctx context.Context, mission *store.Mission, data *missionData, action *store.Action) (bool, error) {
	if action.State == store.ActionReconciled {
		var evidence struct {
			Policy InsurancePolicy `json:"policy"`
		}
		if json.Unmarshal(action.Response, &evidence) != nil {
			return false, errors.New("invalid durable voyage policy evidence")
		}
		return r.applyVoyageInsurance(ctx, mission, data, evidence.Policy)
	}
	if !r.fresh("insurance") {
		return false, nil
	}
	var request gameapi.CreateInsurancePolicyRequest
	if json.Unmarshal(data.Action.Body, &request) != nil {
		return false, errors.New("durable voyage insurance lacks its immutable request")
	}
	intent := voyageInsuranceForLeg(data, data.Action.Before.Ship.CurrentPortID, request.DestPortID)
	if intent == nil || intent.ServerNotBefore.IsZero() {
		return false, errors.New("durable voyage insurance lacks its server-clock admission bound")
	}
	var observed *InsurancePolicy
	for _, policy := range r.stateCache.InsurancePolicies {
		if !matchingVoyagePolicy(policy, intent, r.account.CompanyID) || policy.CreatedAt.Before(intent.ServerNotBefore) {
			continue
		}
		if observed != nil {
			return false, r.hold(ctx, mission.ShipID, "multiple matching voyage policies leave the offer ambiguous")
		}
		copy := policy
		observed = &copy
	}
	if observed == nil {
		return false, nil
	}
	if action.State == store.ActionPending {
		if err := r.db.ReconcileAction(ctx, action.ID, encode(map[string]any{"kind": "authoritative_voyage_insurance", "policy": observed})); err != nil {
			return false, err
		}
	}
	return r.applyVoyageInsurance(ctx, mission, data, *observed)
}

func (r *Runner) voyagePoliciesTerminal(data *missionData) bool {
	if !r.fresh("insurance") {
		return false
	}
	for _, intent := range data.VoyageInsurance {
		if intent.PolicyID == "" {
			continue
		}
		terminal := false
		for _, policy := range r.stateCache.InsurancePolicies {
			if policy.ID == intent.PolicyID {
				terminal = policy.Status == "settled_safe" || policy.Status == "claimed_loss" || policy.Status == "cancelled"
			}
		}
		if !terminal {
			return false
		}
	}
	return true
}

func (r *Runner) settleVoyageInsurance(data *missionData) error {
	if !hasIssuedVoyageInsurance(data) {
		return nil
	}
	if !r.voyagePoliciesTerminal(data) || !r.fresh("ledger") || data.VoyageInsuranceSettledAt.IsZero() ||
		r.meta["ledger"].AttemptedAt.Before(data.VoyageInsuranceSettledAt) {
		return errors.New("voyage insurance settlement needs a complete ledger fetched after terminal policy observation")
	}
	ownedCash := make(map[string]bool, len(r.stateCache.Vaults))
	ownedEscrow := make(map[string]bool, len(r.stateCache.Vaults))
	for _, vault := range r.stateCache.Vaults {
		ownedCash[vault.AccountID] = vault.AccountType == "vault"
		ownedEscrow[vault.AccountID] = vault.AccountType == "escrow"
	}
	refunds := make(map[string]int64)
	for _, row := range decode[[]struct {
		ID       string `json:"id"`
		Debit    string `json:"debit_account_id"`
		Credit   string `json:"credit_account_id"`
		Amount   int64  `json:"amount_pence"`
		Reason   string `json:"reason"`
		Metadata any    `json:"metadata"`
	}](r.snapshot, "ledger") {
		if data.InitialLedgerIDs[row.ID] || row.Amount <= 0 || !ownedCash[row.Credit] || ownedCash[row.Debit] ||
			!strings.Contains(row.Reason, "insurance") || strings.Contains(row.Reason, "collateral") {
			continue
		}
		for _, intent := range data.VoyageInsurance {
			if intent.PolicyID != "" && insuranceReference(row.Metadata, intent.PolicyID, intent.PolicyID) {
				if ownedEscrow[row.Debit] {
					refunds[intent.PolicyID] = growthCashTotal(refunds[intent.PolicyID], row.Amount)
				} else {
					data.ProceedsPence = growthCashTotal(data.ProceedsPence, row.Amount)
				}
				break
			}
		}
	}
	for _, intent := range data.VoyageInsurance {
		if intent.PolicyID != "" && intent.Quote.LloydsPremiumPence != nil {
			refund := min(refunds[intent.PolicyID], *intent.Quote.LloydsPremiumPence)
			data.FeesPence = max(int64(0), data.FeesPence-refund)
		}
	}
	return nil
}
