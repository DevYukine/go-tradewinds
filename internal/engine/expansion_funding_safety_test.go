package engine

import (
	"io"
	"net/http"
	"strings"
	"testing"
	"time"
)

func TestExpansionFundedPairHirePreservesAdmissionAfterRemittance(t *testing.T) {
	t.Parallel()
	h := newRecoveryHarness(t)
	newMeasuredExpansionFactorAPI(h)
	h.api.setVaultBalance("caribbean", 0)
	h.api.mu.Lock()
	h.api.rates = append(h.api.rates, Rate{FromRegion: "north", ToRegion: "caribbean", FeeBPS: 100})
	h.api.mu.Unlock()
	runner := h.runner(primaryTestKey)
	refreshExpansionFactor(t, h, runner)
	control, reason, err := runner.expansionAdmission(h.ctx)
	if err != nil || reason != "" {
		t.Fatalf("funded control: %s %v", reason, err)
	}
	// Leave precisely the admitted capital and lot after every protected
	// region, both hire fees and the measured remittance fee are funded.
	_, fees, _, funded := measuredFunding(control.input.Balances, control.required, control.input.Rates)
	if !funded || fees <= 0 {
		t.Fatalf("fixture lacks priced regional funding: %d %t", fees, funded)
	}
	var protected int64
	for _, reserve := range control.required {
		protected += reserve
	}
	h.api.setVaultBalance("south", control.required["south"])
	control.input.Balances["south"] = control.required["south"]
	other := int64(0)
	for region, balance := range control.input.Balances {
		if region != "north" {
			other += balance
		}
	}
	cash := protected + growthCapital(control.build) + control.build.WorkingCapitalPence + fees - other
	for _, delta := range []int64{-1, 0, 1} {
		h.api.setVaultBalance("north", cash+delta)
		refreshExpansionFactor(t, h, runner)
		_, reason, err := runner.expansionAdmission(h.ctx)
		if err != nil || (reason == "") != (delta >= 0) {
			t.Fatalf("admission cash boundary %+dp: reason=%s err=%v", delta, reason, err)
		}
	}
	h.api.setVaultBalance("north", cash)
	refreshExpansionFactor(t, h, runner)
	if changed, err := runner.considerExpansionAgent(h.ctx); err != nil || !changed {
		t.Fatalf("admit exact funded pair: %t %v", changed, err)
	}
	mission, data := h.mission("__treasury")
	if data.Action == nil || data.Action.Stage != "remit" {
		t.Fatalf("pair did not fund regional base and both fees first: %+v", data.Action)
	}
	if _, err := runner.dispatch(h.ctx, mission, &data); err != nil {
		t.Fatal(err)
	}
	if err := h.db.DeleteMission(h.ctx, h.account.ID, "__treasury"); err != nil {
		t.Fatal(err)
	}
	runner = h.runner(primaryTestKey)
	refreshExpansionFactor(t, h, runner)
	if changed, err := runner.manageExpansionAgents(h.ctx); err != nil || !changed {
		t.Fatalf("restart after pair funding: %t %v", changed, err)
	}
	mission, data = h.mission("__treasury")
	if data.Action == nil || data.Action.Stage != "hire-factor" {
		t.Fatalf("funded pair did not stage its hire: %+v", data.Action)
	}
	if err := runner.newActionPreflight(h.ctx, mission, &data); err != nil {
		t.Fatalf("unchanged funded pair lost admission after fees became pending: %v", err)
	}
}

func TestExpansionHireCashVetoDefersOnlyTreasuryWhileFleetTrades(t *testing.T) {
	t.Parallel()
	h := newRecoveryHarness(t)
	newMeasuredExpansionFactorAPI(h)
	runner := h.runner(primaryTestKey)
	refreshExpansionFactor(t, h, runner)
	if changed, err := runner.considerExpansionAgent(h.ctx); err != nil || !changed {
		t.Fatalf("admit paid pair: %t %v", changed, err)
	}
	_, before := h.mission("__treasury")
	if before.Action == nil || before.Action.Stage != "hire-factor" {
		t.Fatalf("fixture did not stage first hire: %+v", before.Action)
	}
	// An independent cash change invalidates only the never-sent control.
	required, err := runner.treasuryReserves(h.ctx)
	if err != nil {
		t.Fatal(err)
	}
	h.api.setVaultBalance("caribbean", required["caribbean"])
	h.api.setVaultBalance("south", required["south"])
	h.api.setVaultBalance("north", required["north"]+50_000)
	berthed := sailingShipRow(recoveryShipBlastoise, "Blastoise", recoveryHomePort, recoveryFarPort)
	berthed["ship"].(map[string]any)["status"] = "anchored_roadstead"
	berthed["ship"].(map[string]any)["cannons_count"] = 4
	berthed["ship"].(map[string]any)["created_at"] = time.Now().UTC().Add(-30 * 24 * time.Hour)
	berthed["voyage"], berthed["route_legs"] = nil, []any{}
	berthed["effective_hold_capacity_tons"] = 52
	berthed["stores"] = map[string]any{"provisions_tons": 4, "sea_days_remaining": 5}
	h.api.setFleet([]map[string]any{berthed})
	h.api.setFactorPorts(recoveryHomePort, recoveryFarPort)
	base := h.api.server.Config.Handler
	h.api.server.Config.Handler = http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		if request.Method != http.MethodPost || request.URL.Path != "/v1/fleet/dock" {
			base.ServeHTTP(writer, request)
			return
		}
		body, err := io.ReadAll(request.Body)
		if err != nil {
			t.Error(err)
			writer.WriteHeader(http.StatusBadRequest)
			return
		}
		h.api.mu.Lock()
		defer h.api.mu.Unlock()
		h.api.fleet[0]["ship"].(map[string]any)["status"] = "berthed"
		h.api.record(request, body, http.StatusOK)
		h.api.writeJSON(writer, http.StatusOK, map[string]any{"ship_id": recoveryShipBlastoise})
	})
	started := time.Now().UTC()
	var until time.Time
	for tick := range 20 {
		refreshExpansionFactor(t, h, runner)
		// The expansion fake has separate commodity history. Retain the
		// incumbent fixture's ordinary grain market as well.
		runner.stateCache.Catalog.Commodities = append(runner.stateCache.Catalog.Commodities, Commodity{ID: recoveryGrain, BasePricePence: 100})
		if tick == 0 {
			mission, data := h.mission("__treasury")
			if err := runner.newActionPreflight(h.ctx, mission, &data); err == nil || !strings.Contains(err.Error(), "cash") {
				t.Fatalf("fixture did not produce a transient hire cash veto: %v", err)
			}
		}
		if _, err := runner.work(h.ctx); err != nil {
			t.Fatalf("fleet pass %d: %v", tick, err)
		}
		if tick == 0 {
			if mission, err := h.db.GetMission(h.ctx, h.account.ID, recoveryShipBlastoise); err != nil || mission == nil {
				t.Fatalf("treasury cash veto did not yield its own pass to the ship: mission=%+v err=%v", mission, err)
			}
		}
		_, data := h.mission("__treasury")
		if data.Action != nil || data.CooldownUntil.IsZero() {
			t.Fatalf("cash-vetoed hire was restaged or lacked a scoped cooldown: %+v", data.Action)
		}
		if tick == 0 {
			until = data.CooldownUntil
			if exists, err := h.db.HasAction(h.ctx, h.account.ID, before.Action.ID); err != nil || exists {
				t.Fatalf("never-sent veto left a pending journal request: exists=%t err=%v", exists, err)
			}
		}
		if data.CooldownUntil != until {
			t.Fatal("repeated passes extended the pair's retry deadline")
		}
		if h.api.mutationCount("/v1/fleet/depart") > 0 {
			break
		}
	}
	if until.Before(started.Add(15*time.Minute/time.Duration(runner.stateCache.Clock.Calendar.TimeScale))) || until.After(time.Now().Add(16*time.Minute/time.Duration(runner.stateCache.Clock.Calendar.TimeScale))) {
		t.Fatalf("retry is not 15 game minutes: %s", until)
	}
	agents := decode[[]expansionAgent](runner.snapshot, "_expansion_agents")
	if len(agents) != 2 || !agents[0].RetryAt.Equal(until) || !agents[1].RetryAt.Equal(until) {
		t.Fatalf("pair retry did not persist for both members: %+v", agents)
	}
	for _, path := range []string{"/v1/fleet/dock", "/v1/market/place-order", "/v1/fleet/depart"} {
		if h.api.mutationCount(path) == 0 {
			t.Fatalf("never-sent treasury veto starved fleet %s; holds=%v", path, runner.holding)
		}
	}
	if h.api.mutationCount("/v1/company/hire-factor") != 0 || h.api.mutationCount("/v1/treasury/remit") > 1 {
		t.Fatalf("transient veto sent a hire or remittance storm: hires=%d remits=%+v", h.api.mutationCount("/v1/company/hire-factor"), h.api.mutationsFor("/v1/treasury/remit"))
	}
	restarted := h.runner(primaryTestKey)
	refreshExpansionFactor(t, h, restarted)
	if changed, err := restarted.manageExpansionAgents(h.ctx); err != nil || changed {
		t.Fatalf("restart ignored the durable pair retry: %t %v", changed, err)
	}
	for _, hold := range runner.holding {
		if hold.ShipID == "" && strings.Contains(hold.Reason, "authoritative resynchronization") {
			t.Fatalf("scoped veto emitted a fleet-wide hold: %+v", hold)
		}
	}
	// The retry never grants another two hours to an already paid attempt.
	for i := range agents {
		agents[i].CreatedAt = time.Now().UTC().Add(-2*time.Hour - time.Second)
	}
	restarted.snapshot["_expansion_agents"] = encode(agents)
	if err := h.db.DeleteMission(h.ctx, h.account.ID, "__treasury"); err != nil {
		t.Fatal(err)
	}
	if err := restarted.expireAwaitingExpansion(h.ctx, time.Now().UTC()); err != nil {
		t.Fatal(err)
	}
	if remaining := decode[[]expansionAgent](restarted.snapshot, "_expansion_agents"); len(remaining) != 0 {
		t.Fatalf("transient retry extended the original two-hour lifetime: %+v", remaining)
	}
}

func TestPlayerCollateralRefusalAllowsCompleteMultiLienGoldsmithPackage(t *testing.T) {
	t.Parallel()
	h, runner := loanCapacityFixture(t, goldsmithsMaximumPence, false)
	delete(runner.snapshot, "_player_loan_fallback")
	ship := runner.stateCache.Ships[0]
	ship.Ship.ID = recoveryShipCharizard
	runner.stateCache.Ships = append(runner.stateCache.Ships, ship)
	port, _ := findPort(runner.stateCache.Catalog, recoveryHomePort)
	hull, _ := findHull(runner.stateCache.Catalog, recoveryHull)
	decision := GrowthDecision{
		EconomicallyViable: true, CashShortfallPence: 800_000, QuotedHullPence: 2_000_000,
		IncrementalNetPencePerGameDay: 100_000, DepthNetPencePerGameDay: 100_000,
		WorkingCapitalPence: 50_000, CandidateLotCashPence: 25_000,
	}
	evidence := &growthEvidence{
		available: 1_250_000, balances: map[string]int64{port.BankingRegion: 1_250_000},
		required: map[string]int64{},
	}
	if changed, err := runner.considerCapacityFinancing(h.ctx, port, hull, decision, map[string]int64{}, evidence); err != nil || changed {
		t.Fatalf("oversized single player pledge staged: %t %v", changed, err)
	}
	if !runner.playerFallback(port.ID, hull.ID) {
		t.Fatal("single-player-pledge refusal stranded an otherwise feasible debt package")
	}
	if changed, err := runner.considerCapacityFinancing(h.ctx, port, hull, decision, map[string]int64{}, evidence); err != nil || !changed {
		t.Fatalf("complete multi-lien fallback was refused: %t %v", changed, err)
	}
	_, data := h.mission("__growth")
	if data.Capacity == nil || data.Capacity.Financing == nil || len(data.Capacity.Financing.Quotes) != 2 {
		t.Fatalf("fallback did not freeze its complete two-lien package: %+v", data.Capacity)
	}
	var principal int64
	ships := make(map[string]bool)
	for _, quote := range data.Capacity.Financing.Quotes {
		principal += quote.PrincipalPence
		if quote.CollateralShipID == nil || ships[*quote.CollateralShipID] {
			t.Fatal("multi-lien package reused collateral")
		}
		ships[*quote.CollateralShipID] = true
	}
	if principal <= goldsmithsMaximumPence || principal < decision.CashShortfallPence || runner.playerFallback(port.ID, hull.ID) {
		t.Fatalf("fallback ticket or multi-lien principal was wrong: %d", principal)
	}
	if h.api.mutationCount("/v1/loan/request") != 0 || h.api.mutationCount("/v1/loan/approve") != 0 {
		t.Fatal("collateral refusal posted an invalid player request")
	}
}
