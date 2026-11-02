package engine

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"slices"
	"strings"
	"time"

	"github.com/DevYukine/go-tradewinds/internal/gameapi"
	"github.com/DevYukine/go-tradewinds/internal/store"
	"github.com/google/uuid"
)

const americaFlagshipMission = "__america_flagship"
const americaFlagshipQuoteRetry = 30 * time.Minute

// A request is open only 72 game hours (72 real minutes at time scale 60), so
// a lapsed one is reposted quickly to keep it visible to lenders.
const americaFlagshipRequestRetry = 10 * time.Minute

type americaFlagshipState struct {
	Pending     bool      `json:"pending"`
	LoanID      string    `json:"loan_id,omitempty"`
	BuildName   string    `json:"build_name,omitempty"`
	BuiltShipID string    `json:"built_ship_id,omitempty"`
	RetryAt     time.Time `json:"retry_at,omitempty"`
}

func (r *Runner) saveAmericaFlagship(ctx context.Context, state americaFlagshipState) error {
	r.snapshot["_america_flagship"] = encode(state)
	return r.db.SaveSnapshot(ctx, r.account.ID, r.snapshot)
}

// The flagship spends its own reservation. Every other planner and treasury
// donor sees the America cash as committed until the hull has been built.
func reserveAmericaFlagship(required map[string]int64, state AccountState, snapshot map[string]json.RawMessage) {
	flagship := decode[americaFlagshipState](snapshot, "_america_flagship")
	if flagship.Pending && flagship.BuiltShipID == "" {
		for _, vault := range state.Vaults {
			if vault.BankingRegion == "america" && vault.AccountType == "vault" {
				required["america"] = max(required["america"], vault.BalancePence)
			}
		}
	}
}

func (r *Runner) americaFlagshipQuote(ctx context.Context, class string) (YardQuote, error) {
	var quote YardQuote
	err := r.api.Get(ctx, "/v1/shipyard/quote?port_id=boston&ship_class="+url.QueryEscape(class)+"&build_mode=turnkey_cash", &quote)
	if err == nil && (quote.PortID != "boston" || quote.ShipClass != class || quote.BuildMode != "turnkey_cash" ||
		quote.TotalCostPence == nil || *quote.TotalCostPence <= 0 || *quote.TotalCostPence > maximumPence/2) {
		err = errors.New("Boston turnkey quote has no usable price")
	}
	return quote, err
}

// lastAmericaFlagshipPrincipal returns the principal of the newest own
// unsecured amortised America request, or 0 when none was recorded.
func (r *Runner) lastAmericaFlagshipPrincipal() int64 {
	var newest *gameapi.LoanRequestView
	for index := range r.stateCache.LoanRequests {
		request := &r.stateCache.LoanRequests[index]
		if request.BorrowerCompanyID != r.account.CompanyID || request.BankingRegion != "america" ||
			request.CollateralKind != "unsecured" || request.Repayment != "amortised" || request.TermGameDays != 360 {
			continue
		}
		if newest == nil || request.CreatedAt.After(newest.CreatedAt) {
			newest = request
		}
	}
	if newest == nil {
		return 0
	}
	return newest.PrincipalPence
}

func (r *Runner) driveAmericaFlagship(ctx context.Context, mission *store.Mission, data *missionData) (bool, error) {
	if !r.opts.Execute || r.stateCache.Clock.UnlockedPhase < 2 || r.stateCache.Clock.Paused || !r.fresh("clock") {
		return false, nil
	}
	state := decode[americaFlagshipState](r.snapshot, "_america_flagship")
	now := time.Now().UTC()
	if mission == nil {
		if state.BuiltShipID != "" || state.LoanID != "" {
			return false, nil
		}
		if now.Before(state.RetryAt) {
			r.wakeBy(state.RetryAt)
			return false, nil
		}
		if !r.loanAuthority() || !r.loanRequestAuthority() || !r.fresh("vaults") || !r.fresh("ships") || !r.fresh("factors") || r.openOwnLoanRequest() {
			return false, nil
		}
		if !r.portPresence("boston") {
			state.Pending = true
			if err := r.saveAmericaFlagship(ctx, state); err != nil {
				return false, err
			}
			mission = &store.Mission{AccountID: r.account.ID, ShipID: americaFlagshipMission, Phase: "flagship_presence", Generation: uuid.NewString(), Destination: "boston"}
			*data = missionData{BuildPort: "boston", CostsKnown: true, StartedAt: now}
			return true, r.stage(ctx, mission, data, ShipState{}, "hire-factor", "/v1/company/hire-factor", map[string]any{"port_id": "boston"})
		}
		class := "barque"
		quote, err := r.americaFlagshipQuote(ctx, class)
		if err != nil {
			class = "frigate"
			quote, err = r.americaFlagshipQuote(ctx, class)
		}
		var principal int64
		if err == nil {
			shortfall := max(int64(0), growthCashTotal(*quote.TotalCostPence, ceilBPS(*quote.TotalCostPence, 1_000))-r.planningInput(ShipState{}).Balances["america"])
			principal = shortfall / 100_000 * 100_000
			if shortfall%100_000 != 0 {
				principal += 100_000
			}
			principal = max(int64(10_000), principal)
		} else {
			// Boston turnkey quotes lose their total while a material such as
			// canvas is out of stock. Lenders still need a visible request, so
			// repost the last flagship principal; the build itself waits for a
			// priced quote before spending.
			class, principal = "barque", r.lastAmericaFlagshipPrincipal()
			if principal == 0 {
				return false, r.holdExpected(ctx, americaFlagshipMission, "America flagship waits for a priced Boston barque or frigate")
			}
		}
		lower, _, ok := r.serverBounds()
		if !ok || r.stateCache.Clock.Paused {
			return false, nil
		}
		intent := &playerLoanIntent{
			PortID: "boston", HullID: class, BankingRegion: "america", PrincipalPence: principal,
			TermGameDays: 360, Repayment: "amortised", CollateralKind: "unsecured", OpenGameHours: 72,
			PostedAt: lower.Add(-playerLoanClockSkew),
		}
		name := state.BuildName
		if name == "" {
			reserved, ready, err := r.reserveShipName(ctx, americaFlagshipMission)
			if err != nil || !ready {
				return false, err
			}
			name, state.BuildName = reserved, reserved
		}
		state.Pending, state.RetryAt = true, time.Time{}
		if err := r.saveAmericaFlagship(ctx, state); err != nil {
			return false, err
		}
		mission = &store.Mission{AccountID: r.account.ID, ShipID: americaFlagshipMission, Phase: "growth_loan_request", Generation: uuid.NewString(), Destination: "boston"}
		*data = missionData{PlayerLoan: intent, BuildPort: "boston", BuildClass: class, BuildName: name, CostsKnown: true,
			StartedAt: now, StartedGameHours: r.stateCache.Clock.Calendar.TotalGameHours, InitialLedgerIDs: r.ledgerIDs()}
		if err := r.decision(ctx, americaFlagshipMission, "america_flagship_request", fmt.Sprintf("America flagship: requested %d Cr amortised from player lenders", principal/100), intent.body()); err != nil {
			return false, err
		}
		return true, r.stage(ctx, mission, data, ShipState{}, "loan-request", "/v1/loan/request", intent.body())
	}
	if data.Action != nil || now.Before(data.CooldownUntil) {
		r.wakeBy(data.CooldownUntil)
		return false, nil
	}
	if data.BuiltShipID != "" {
		state.Pending, state.BuiltShipID = false, data.BuiltShipID
		if err := r.saveAmericaFlagship(ctx, state); err != nil {
			return false, err
		}
		// Normal per-ship maintenance buys guns and stores before voyage planning.
		return true, r.db.DeleteMission(ctx, r.account.ID, mission.ShipID)
	}
	if !r.fresh("factors") {
		return false, nil
	}
	if !r.portPresence("boston") {
		return true, r.stage(ctx, mission, data, ShipState{}, "hire-factor", "/v1/company/hire-factor", map[string]any{"port_id": "boston"})
	}
	if data.PlayerLoan == nil {
		return true, r.db.DeleteMission(ctx, r.account.ID, mission.ShipID)
	}
	if data.Capacity != nil && data.Capacity.Financing != nil && len(data.Capacity.Financing.Loans) != 0 {
		if state.LoanID == "" {
			state.Pending, state.LoanID = true, data.Capacity.Financing.Loans[0].ID
			if err := r.saveAmericaFlagship(ctx, state); err != nil {
				return false, err
			}
		}
		quote, err := r.americaFlagshipBuildQuote(ctx, data)
		if err != nil {
			if saveErr := r.saveMission(ctx, mission, data); saveErr != nil {
				return false, saveErr
			}
			return false, r.holdExpected(ctx, americaFlagshipMission, err.Error())
		}
		data.Capacity.QuoteLimitPence = *quote.TotalCostPence
		mission.Phase = "growth_build"
		return true, r.stage(ctx, mission, data, ShipState{}, "build", "/v1/shipyard/build-ship", map[string]any{
			"port_id": "boston", "ship_class": data.BuildClass, "name": data.BuildName, "build_mode": "turnkey_cash", "mortgage": false,
		})
	}
	if !r.loanAuthority() || !r.loanRequestAuthority() || data.PlayerLoan == nil {
		return false, nil
	}
	if data.PlayerLoan.RequestID == "" {
		if r.openOwnLoanRequest() {
			return false, nil
		}
		return true, r.stage(ctx, mission, data, ShipState{}, "loan-request", "/v1/loan/request", data.PlayerLoan.body())
	}
	request := r.recordedPlayerRequest(data.PlayerLoan)
	if request == nil || !playerRequestMatches(*request, data.PlayerLoan, r.account.CompanyID) {
		return false, r.holdExpected(ctx, americaFlagshipMission, "America flagship waits for its recorded loan request")
	}
	_, upper, ok := r.serverBounds()
	if !ok {
		return false, nil
	}
	if request.Status == "funded" {
		return false, r.holdExpected(ctx, americaFlagshipMission, "America flagship awaits its disbursed loan receipt")
	}
	if request.Status != "open" || !upper.Before(request.ExpiresAt) {
		state.Pending, state.RetryAt = false, now.Add(americaFlagshipRequestRetry)
		if err := r.saveAmericaFlagship(ctx, state); err != nil {
			return false, err
		}
		return true, r.db.DeleteMission(ctx, r.account.ID, mission.ShipID)
	}
	quote := r.americaFlagshipLender(*request, data.PlayerLoan)
	if quote == nil {
		r.wakeBy(request.ExpiresAt)
		return false, nil
	}
	data.Capacity = &capacityIntent{BuildPort: "boston", BuildClass: data.BuildClass,
		Financing: &capacityFinancing{Quotes: []gameapi.LoanQuote{*quote}}}
	return true, r.stage(ctx, mission, data, ShipState{}, "loan-accept", "/v1/loan/approve", gameapi.QuoteActionRequest{QuoteID: quote.ID})
}

func (r *Runner) americaFlagshipLender(request gameapi.LoanRequestView, intent *playerLoanIntent) *gameapi.LoanQuote {
	_, upper, ok := r.serverBounds()
	if !ok {
		return nil
	}
	var best *gameapi.LoanQuote
	kraken := func(q *gameapi.LoanQuote) bool {
		return q.LenderCompanyID == "01a16469-7564-711d-b8da-e69d7d73e623" || strings.EqualFold(q.LenderName, "The Kraken Cartel") || strings.EqualFold(q.LenderName, "Kraken Cartel")
	}
	for index := range request.Quotes {
		quote := &request.Quotes[index]
		if quote.ID == "" || quote.Status != "open" || slices.Contains(intent.RejectedQuotes, quote.ID) || !upper.Before(quote.ExpiresAt) ||
			quote.RequestID == nil || *quote.RequestID != request.ID || quote.BorrowerCompanyID != r.account.CompanyID || quote.BankingRegion != "america" ||
			quote.LenderCompanyID == "" || quote.LenderCompanyID == r.account.CompanyID || quote.PrincipalPence != intent.PrincipalPence ||
			quote.TermGameDays != 360 || quote.Repayment != "amortised" || quote.AnnualRateBPS < 0 || int64(quote.AnnualRateBPS) > 2*int64(request.GoldsmithsRateBPS) {
			continue
		}
		if best == nil || quote.AnnualRateBPS < best.AnnualRateBPS || quote.AnnualRateBPS == best.AnnualRateBPS &&
			(kraken(quote) && !kraken(best) || kraken(quote) == kraken(best) && quote.ID < best.ID) {
			best = quote
		}
	}
	return best
}

func (r *Runner) americaFlagshipBuildQuote(ctx context.Context, data *missionData) (YardQuote, error) {
	quote, err := r.americaFlagshipQuote(ctx, data.BuildClass)
	if err != nil || !quote.Available {
		data.CooldownUntil = time.Now().UTC().Add(americaFlagshipQuoteRetry)
		r.wakeBy(data.CooldownUntil)
		return quote, errors.New("America flagship waits 30 real minutes for Boston turnkey materials")
	}
	if !r.fresh("vaults") || r.planningInput(ShipState{}).Balances["america"] < *quote.TotalCostPence {
		return quote, errors.New("America flagship waits for America cash to cover the live Boston quote")
	}
	return quote, nil
}

func (r *Runner) americaFlagshipPreflight(ctx context.Context, data *missionData) error {
	if data.Action.Stage == "hire-factor" {
		var body struct {
			PortID string `json:"port_id"`
		}
		if r.stateCache.Clock.UnlockedPhase < 2 || data.BuildPort != "boston" || !r.fresh("factors") ||
			json.Unmarshal(data.Action.Body, &body) != nil || body.PortID != "boston" || r.portPresence("boston") {
			return errors.New("America flagship factor requires missing Boston presence")
		}
		return nil
	}
	if r.stateCache.Clock.UnlockedPhase < 2 || data.PlayerLoan == nil || data.BuildPort != "boston" || data.BuildClass != data.PlayerLoan.HullID {
		return errors.New("America flagship requires its recorded Boston loan intent")
	}
	switch data.Action.Stage {
	case "loan-request":
		var body gameapi.PostLoanRequest
		if json.Unmarshal(data.Action.Body, &body) != nil || body != data.PlayerLoan.body() || !r.loanAuthority() || !r.loanRequestAuthority() || r.openOwnLoanRequest() {
			return errors.New("America flagship request waits for the free borrower request slot")
		}
		return nil
	case "loan-accept":
		var body gameapi.QuoteActionRequest
		request := r.recordedPlayerRequest(data.PlayerLoan)
		if !r.loanRequestAuthority() || request == nil || request.Status != "open" || !playerRequestMatches(*request, data.PlayerLoan, r.account.CompanyID) ||
			json.Unmarshal(data.Action.Body, &body) != nil {
			return errors.New("America flagship approval waits for its open loan request")
		}
		quote := r.americaFlagshipLender(*request, data.PlayerLoan)
		_, upper, ok := r.serverBounds()
		if quote == nil || quote.ID != body.QuoteID || !ok || !upper.Before(request.ExpiresAt) {
			return errors.New("America flagship approval waits for the cheapest acceptable quote")
		}
		return nil
	case "build":
		var body struct {
			PortID    string `json:"port_id"`
			ShipClass string `json:"ship_class"`
			Name      string `json:"name"`
			BuildMode string `json:"build_mode"`
			Mortgage  bool   `json:"mortgage"`
		}
		if json.Unmarshal(data.Action.Body, &body) != nil || body.PortID != "boston" || body.ShipClass != data.BuildClass || body.Name == "" ||
			body.Name != data.BuildName || body.BuildMode != "turnkey_cash" || body.Mortgage || data.BuiltShipID != "" ||
			data.Capacity == nil || data.Capacity.Financing == nil || len(data.Capacity.Financing.Loans) != 1 {
			return errors.New("America flagship build requires its disbursed loan and immutable hull")
		}
		quote, err := r.americaFlagshipBuildQuote(ctx, data)
		if err == nil {
			data.Capacity.QuoteLimitPence = *quote.TotalCostPence
		}
		return err
	}
	return errors.New("unknown America flagship action")
}

func (r *Runner) rejectAmericaFlagship(ctx context.Context, mission *store.Mission, data *missionData, status int) (bool, error) {
	stage := data.Action.Stage
	if stage == "loan-accept" && status != 401 && status != 403 && status != 429 {
		var body gameapi.QuoteActionRequest
		_ = json.Unmarshal(data.Action.Body, &body)
		data.PlayerLoan.RejectedQuotes = append(data.PlayerLoan.RejectedQuotes, body.QuoteID)
		data.Capacity = nil
	}
	data.LastActionID = data.Action.ID
	data.Sequence++
	data.Action = nil
	data.CooldownUntil = maxTime(data.CooldownUntil, r.gameDelay(1))
	if status != 401 && status != 403 && status != 429 {
		if stage == "loan-request" {
			state := decode[americaFlagshipState](r.snapshot, "_america_flagship")
			state.Pending, state.RetryAt = false, time.Now().UTC().Add(playerLoanBackoff)
			if err := r.saveAmericaFlagship(ctx, state); err != nil {
				return false, err
			}
			return true, r.db.DeleteMission(ctx, r.account.ID, mission.ShipID)
		}
		if stage == "build" {
			data.CooldownUntil = time.Now().UTC().Add(americaFlagshipQuoteRetry)
		}
	}
	if err := r.saveMission(ctx, mission, data); err != nil {
		return false, err
	}
	if status == 401 || status == 403 {
		return false, fmt.Errorf("%w after America flagship HTTP %d", ErrExecutionStopped, status)
	}
	return true, nil
}
