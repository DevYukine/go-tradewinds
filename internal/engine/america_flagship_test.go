package engine

import (
	"encoding/json"
	"io"
	"net/http"
	"reflect"
	"slices"
	"testing"
	"time"

	"github.com/DevYukine/go-tradewinds/internal/gameapi"
	"github.com/DevYukine/go-tradewinds/internal/store"
)

func americaFlagshipFixture(t *testing.T) (*recoveryHarness, *Runner, *bool) {
	t.Helper()
	h := newRecoveryHarness(t)
	h.api.setVaultBalance("america", 100_500)
	h.api.setFactorPorts("boston")
	available := true
	base := h.api.server.Config.Handler
	h.api.server.Config.Handler = http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		if req.Method == http.MethodGet && req.URL.Path == "/v1/world/clock" {
			h.api.mu.Lock()
			defer h.api.mu.Unlock()
			clock := h.api.clockDocument()
			clock["unlocked_phase"] = 2
			h.api.writeJSON(w, http.StatusOK, clock)
			return
		}
		if req.Method == http.MethodGet && req.URL.Path == "/v1/world/catalog" {
			catalog := h.api.catalogDocument()
			catalog["ports"] = append(catalog["ports"].([]any), map[string]any{
				"id": "boston", "name": "Boston", "banking_region": "america", "sea_zone_id": "zone-north", "phase": 2, "has_shipyard": true,
			})
			h.api.writeJSON(w, http.StatusOK, catalog)
			return
		}
		if req.Method == http.MethodPost && req.URL.Path == "/v1/company/hire-factor" {
			body, err := io.ReadAll(req.Body)
			if err != nil || string(body) != `{"port_id":"boston"}` || req.Header.Get("Idempotency-Key") == "" {
				t.Errorf("Boston factor lost its exact durable request: %s %v", body, err)
				w.WriteHeader(http.StatusBadRequest)
				return
			}
			h.api.mu.Lock()
			defer h.api.mu.Unlock()
			if !slices.Contains(h.api.factorPorts, "boston") {
				h.api.factorPorts = append(h.api.factorPorts, "boston")
				h.api.vaultBalance["america"] -= expansionAgentHireFeePence
			}
			h.api.record(req, body, http.StatusOK)
			h.api.writeJSON(w, http.StatusOK, map[string]any{"port_id": "boston", "has_factor": true})
			return
		}
		if req.Method == http.MethodGet && req.URL.Path == "/v1/shipyard/quote" && req.URL.Query().Get("port_id") == "boston" {
			h.api.mu.Lock()
			defer h.api.mu.Unlock()
			if !slices.Contains(h.api.factorPorts, "boston") {
				h.api.writeProblem(w, http.StatusForbidden, "no_port_presence", "Boston requires ship or factor presence")
				return
			}
			if req.URL.Query().Get("build_mode") != "turnkey_cash" {
				t.Error("flagship quote omitted turnkey_cash")
			}
			h.api.writeJSON(w, http.StatusOK, YardQuote{PortID: "boston", ShipClass: req.URL.Query().Get("ship_class"),
				BuildMode: "turnkey_cash", Available: available, TotalCostPence: new(int64(20_570_000))})
			return
		}
		base.ServeHTTP(w, req)
	})
	runner := h.runner(primaryTestKey)
	return h, runner, &available
}

func americaFlagshipTick(t *testing.T, h *recoveryHarness, runner *Runner) {
	t.Helper()
	loanCapacityRefresh(t, h, runner)
	// Unrelated trading evidence must not gate the owner's explicit loan/build.
	delete(runner.meta, "contracts")
	if _, err := runner.work(h.ctx); err != nil {
		t.Fatal(err)
	}
}

func americaFlagshipTestQuote(request gameapi.LoanRequestView, lender, name, id string, rate int32) gameapi.LoanQuote {
	installment := request.PrincipalPence / 30
	return gameapi.LoanQuote{
		ID: id, RequestID: &request.ID, LenderCompanyID: lender, LenderName: name,
		BorrowerCompanyID: recoveryCompanyID, BankingRegion: "america", PrincipalPence: request.PrincipalPence,
		AnnualRateBPS: rate, TermGameDays: 360, Repayment: "amortised", InstallmentPence: &installment,
		TotalRepayablePence: installment * 36, Status: "open", CreatedAt: time.Now().UTC(), ExpiresAt: time.Now().UTC().Add(time.Hour),
	}
}
func TestAmericaFlagshipHiresAndKeepsBostonFactorAcrossRestart(t *testing.T) {
	h, runner, _ := americaFlagshipFixture(t)
	h.api.setFactorPorts()
	if _, err := runner.americaFlagshipQuote(h.ctx, "barque"); err == nil {
		t.Fatal("fixture accepted a Boston quote without presence")
	}
	americaFlagshipTick(t, h, runner)
	_, data := h.mission(americaFlagshipMission)
	if data.Action == nil || data.Action.Stage != "hire-factor" || string(data.Action.Body) != `{"port_id":"boston"}` {
		t.Fatalf("missing presence did not stage a durable Boston factor: %+v", data.Action)
	}
	runner = h.runner(primaryTestKey)
	for range 4 {
		americaFlagshipTick(t, h, runner)
		if h.api.mutationCount("/v1/company/hire-factor") != 0 {
			break
		}
	}
	if h.api.mutationCount("/v1/company/hire-factor") != 1 || h.api.mutationCount("/v1/loan/request") != 0 {
		t.Fatal("restart lost the single Boston hire or requested before its receipt")
	}
	runner = h.runner(primaryTestKey)
	for range 6 {
		americaFlagshipTick(t, h, runner)
		if h.api.mutationCount("/v1/loan/request") != 0 {
			break
		}
	}
	if h.api.mutationCount("/v1/company/hire-factor") != 1 || h.api.mutationCount("/v1/loan/request") != 1 {
		t.Fatal("Boston hire did not unblock one priced flagship loan request")
	}

	// Retention must also survive a control-mission gap and the six-hour retry.
	if err := h.db.DeleteMission(h.ctx, h.account.ID, americaFlagshipMission); err != nil {
		t.Fatal(err)
	}
	aged := time.Now().UTC().Add(-3 * time.Hour)
	runner.snapshot["_expansion_agents"] = encode([]expansionAgent{{PortID: "boston", Region: "america", CreatedAt: aged, HiredAt: aged}})
	for _, state := range []americaFlagshipState{{Pending: true}, {RetryAt: time.Now().UTC().Add(6 * time.Hour)}} {
		if err := runner.saveAmericaFlagship(h.ctx, state); err != nil {
			t.Fatal(err)
		}
		if changed, err := runner.manageExpansionAgents(h.ctx); changed || err != nil {
			t.Fatalf("pending flagship dismissed Boston: changed=%v err=%v", changed, err)
		}
		h.missingMission("__treasury")
	}
	if err := runner.saveAmericaFlagship(h.ctx, americaFlagshipState{BuiltShipID: recoveryBuiltShipID}); err != nil {
		t.Fatal(err)
	}
	if changed, err := runner.manageExpansionAgents(h.ctx); !changed || err != nil {
		t.Fatalf("finished flagship did not release normal Boston cleanup: changed=%v err=%v", changed, err)
	}
	_, dismissal := h.mission("__treasury")
	if dismissal.Action == nil || dismissal.Action.Stage != "dismiss-factor" || string(dismissal.Action.Body) != `{"port_id":"boston"}` {
		t.Fatalf("finished flagship did not use normal factor dismissal: %+v", dismissal.Action)
	}
}

func TestAmericaFlagshipRequestKrakenApprovalBostonBuildOnceAcrossRestart(t *testing.T) {
	h, runner, available := americaFlagshipFixture(t)
	americaFlagshipTick(t, h, runner)
	americaFlagshipTick(t, h, runner)
	rows := h.api.mutationsFor("/v1/loan/request")
	var body map[string]any
	want := map[string]any{
		"banking_region": "america", "principal_pence": float64(22_600_000), "repayment": "amortised", "term_game_days": float64(360),
		"collateral_kind": "unsecured", "recourse": true, "open_game_hours": float64(72), "auto_quote": true,
	}
	if len(rows) != 1 || json.Unmarshal(rows[0].Body, &body) != nil || !reflect.DeepEqual(body, want) {
		t.Fatalf("flagship request body: %+v; want %+v", rows, want)
	}
	mission, data := h.mission(americaFlagshipMission)
	if data.PlayerLoan.RequestID == "" || data.BuildClass != "barque" || mission.Destination != "boston" {
		t.Fatalf("request lost its immutable Boston barque: %+v", data)
	}
	decisions, err := h.db.History(h.ctx, h.account.ID, "decisions", store.MaxHistoryLimit)
	if err != nil {
		t.Fatal(err)
	}
	found := false
	for _, row := range decisions.([]store.Decision) {
		found = found || row.Kind == "america_flagship_request" && row.Reason == "America flagship: requested 226000 Cr amortised from player lenders"
	}
	if !found {
		t.Fatal("flagship request lacks its plain decision reason")
	}
	runner = h.runner(primaryTestKey)
	americaFlagshipTick(t, h, runner)
	if h.api.mutationCount("/v1/loan/request") != 1 {
		t.Fatal("restart duplicated the open borrower request")
	}
	h.api.mu.Lock()
	request := h.api.loanRequests[0]
	kraken := americaFlagshipTestQuote(request, krakenAcceptanceCompany, "The Kraken Cartel", "z-kraken", 6_000)
	other := americaFlagshipTestQuote(request, "another-lender", "Another lender", "a-other", 6_000)
	self := americaFlagshipTestQuote(request, recoveryCompanyID, "Our company", "own-company", 1)
	expensive := americaFlagshipTestQuote(request, "expensive", "Expensive lender", "expensive", 6_001)
	h.api.loanRequests[0].Quotes = []gameapi.LoanQuote{other, self, expensive, kraken}
	h.api.mu.Unlock()
	americaFlagshipTick(t, h, runner)
	americaFlagshipTick(t, h, runner)
	approvals := h.api.mutationsFor("/v1/loan/approve")
	if len(approvals) != 1 || string(approvals[0].Body) != `{"quote_id":"z-kraken"}` {
		t.Fatalf("Kraken equal-rate preference or 2x rate boundary failed: %+v", approvals)
	}
	_, data = h.mission(americaFlagshipMission)
	if data.Capacity == nil || len(data.Capacity.Financing.Loans) != 1 || data.Capacity.Financing.Loans[0].CollateralKind != "unsecured" {
		t.Fatalf("approval did not retain its exact unsecured disbursement: %+v", data)
	}

	*available = false
	runner = h.runner(primaryTestKey)
	before := time.Now().UTC()
	americaFlagshipTick(t, h, runner)
	_, data = h.mission(americaFlagshipMission)
	if data.CooldownUntil.Before(before.Add(30*time.Minute)) || h.api.mutationCount("/v1/shipyard/build-ship") != 0 {
		t.Fatalf("unavailable barque did not wait 30 real minutes: %+v", data)
	}
	if data.BuildClass != "barque" {
		t.Fatal("temporary material shortage switched the priced barque to a frigate")
	}
	cash := runner.planningInput(ShipState{}).Balances["america"]
	reserves, err := runner.treasuryReserves(h.ctx)
	if err != nil || reserves["america"] != cash || cash != 22_700_500 {
		t.Fatalf("flagship cash is spendable by growth or treasury: cash=%d reserves=%v err=%v", cash, reserves, err)
	}
	americaFlagshipTick(t, h, runner)
	if h.api.mutationCount("/v1/loan/approve") != 1 {
		t.Fatal("unavailable yard duplicated the accepted loan")
	}

	*available = true
	h.expireCooldown(americaFlagshipMission)
	americaFlagshipTick(t, h, runner)
	// Restart between staging and dispatch; only an unsent action may replan.
	runner = h.runner(primaryTestKey)
	for range 4 {
		americaFlagshipTick(t, h, runner)
		if h.api.mutationCount("/v1/shipyard/build-ship") == 1 {
			break
		}
	}
	builds := h.api.mutationsFor("/v1/shipyard/build-ship")
	body = nil
	if len(builds) != 1 || json.Unmarshal(builds[0].Body, &body) != nil || body["port_id"] != "boston" || body["ship_class"] != "barque" ||
		body["build_mode"] != "turnkey_cash" || body["mortgage"] != false || body["name"] == "" {
		t.Fatalf("flagship build body: %+v", builds)
	}
	runner = h.runner(primaryTestKey)
	americaFlagshipTick(t, h, runner)
	h.missingMission(americaFlagshipMission)
	state := decode[americaFlagshipState](runner.snapshot, "_america_flagship")
	if state.Pending || state.BuiltShipID != recoveryBuiltShipID || state.LoanID == "" {
		t.Fatalf("finished flagship was not persisted across restart: %+v", state)
	}
	for range 2 {
		runner = h.runner(primaryTestKey)
		americaFlagshipTick(t, h, runner)
	}
	if h.api.mutationCount("/v1/loan/request") != 1 || h.api.mutationCount("/v1/loan/approve") != 1 || h.api.mutationCount("/v1/shipyard/build-ship") != 1 || len(h.pending()) != 0 {
		t.Fatal("restart duplicated a request, approval or build, or orphaned an action")
	}
}

func TestAmericaFlagshipClosedRequestWaitsSixRealHours(t *testing.T) {
	h, runner, _ := americaFlagshipFixture(t)
	americaFlagshipTick(t, h, runner)
	americaFlagshipTick(t, h, runner)
	h.api.mu.Lock()
	h.api.loanRequests[0].Status = "lapsed"
	h.api.mu.Unlock()
	before := time.Now().UTC()
	americaFlagshipTick(t, h, runner)
	h.missingMission(americaFlagshipMission)
	state := decode[americaFlagshipState](runner.snapshot, "_america_flagship")
	if state.Pending || state.RetryAt.Before(before.Add(6*time.Hour)) {
		t.Fatalf("unfunded request did not get six real hours of backoff: %+v", state)
	}
	runner = h.runner(primaryTestKey)
	americaFlagshipTick(t, h, runner)
	h.missingMission(americaFlagshipMission)
	state.RetryAt = time.Now().Add(-time.Second)
	if err := runner.saveAmericaFlagship(h.ctx, state); err != nil {
		t.Fatal(err)
	}
	americaFlagshipTick(t, h, runner)
	americaFlagshipTick(t, h, runner)
	if h.api.mutationCount("/v1/loan/request") != 2 {
		t.Fatal("expired real-time backoff did not post a replacement request")
	}
}

func TestAmericaFlagshipLostRequestAndApprovalReceiptsReconcile(t *testing.T) {
	h, runner, _ := americaFlagshipFixture(t)
	h.api.loanRequestMode = "drop"
	americaFlagshipTick(t, h, runner)
	americaFlagshipTick(t, h, runner)
	h.api.loanRequestMode = "ok"
	runner = h.runner(primaryTestKey)
	americaFlagshipTick(t, h, runner)
	_, data := h.mission(americaFlagshipMission)
	if data.Action != nil || data.PlayerLoan.RequestID == "" || h.api.mutationCount("/v1/loan/request") != 1 {
		t.Fatalf("lost request did not reconcile without reposting: %+v", data)
	}
	h.expireCooldown(americaFlagshipMission)
	h.api.mu.Lock()
	h.api.loanRequests[0].Quotes = []gameapi.LoanQuote{americaFlagshipTestQuote(h.api.loanRequests[0], krakenAcceptanceCompany, "The Kraken Cartel", "kraken-lost", 3_000)}
	h.api.loanApproveMode = "drop"
	h.api.mu.Unlock()
	americaFlagshipTick(t, h, runner)
	americaFlagshipTick(t, h, runner)
	h.api.loanApproveMode = "ok"
	runner = h.runner(primaryTestKey)
	americaFlagshipTick(t, h, runner)
	_, data = h.mission(americaFlagshipMission)
	if data.Action != nil || data.Capacity == nil || len(data.Capacity.Financing.Loans) != 1 || h.api.mutationCount("/v1/loan/approve") != 1 || len(h.pending()) != 0 {
		t.Fatalf("lost approval did not reconcile without borrowing twice: %+v", data)
	}
}

func TestAmericaFlagshipPriceFallbackAndPrincipalMinimum(t *testing.T) {
	for _, test := range []struct {
		name        string
		noPrice     bool
		unavailable bool
		cash        int64
		class       string
		principal   int64
	}{
		{"unpriced barque uses frigate", true, false, 100_500, "frigate", 22_600_000},
		{"unavailable priced barque stays barque", false, true, 100_500, "barque", 22_600_000},
		{"sufficient cash still requests minimum", false, false, 50_000_000, "barque", 10_000},
	} {
		t.Run(test.name, func(t *testing.T) {
			h, runner, available := americaFlagshipFixture(t)
			*available = !test.unavailable
			h.api.setVaultBalance("america", test.cash)
			base := h.api.server.Config.Handler
			h.api.server.Config.Handler = http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
				if test.noPrice && req.URL.Path == "/v1/shipyard/quote" && req.URL.Query().Get("port_id") == "boston" && req.URL.Query().Get("ship_class") == "barque" {
					h.api.writeJSON(w, http.StatusOK, YardQuote{PortID: "boston", ShipClass: "barque", BuildMode: "turnkey_cash"})
					return
				}
				base.ServeHTTP(w, req)
			})
			americaFlagshipTick(t, h, runner)
			americaFlagshipTick(t, h, runner)
			_, data := h.mission(americaFlagshipMission)
			if data.BuildClass != test.class || data.PlayerLoan.PrincipalPence != test.principal || h.api.mutationCount("/v1/loan/request") != 1 {
				t.Fatalf("wrong priced hull or rounded loan: %+v", data)
			}
		})
	}
}

func TestAmericaFlagshipChoosesCheapestAcceptableQuote(t *testing.T) {
	h, runner, _ := americaFlagshipFixture(t)
	loanCapacityRefresh(t, h, runner)
	request := gameapi.LoanRequestView{ID: "flagship-request", GoldsmithsRateBPS: 3_000, PrincipalPence: 1_000_000}
	intent := &playerLoanIntent{RequestID: request.ID, PrincipalPence: request.PrincipalPence}
	kraken := americaFlagshipTestQuote(request, krakenAcceptanceCompany, "The Kraken Cartel", "kraken", 3_000)
	cheaper := americaFlagshipTestQuote(request, "cheaper-lender", "Cheaper lender", "cheaper", 2_999)
	request.Quotes = []gameapi.LoanQuote{kraken, cheaper}
	if chosen := runner.americaFlagshipLender(request, intent); chosen == nil || chosen.ID != cheaper.ID {
		t.Fatalf("Kraken displaced a cheaper eligible lender: %+v", chosen)
	}
	for _, test := range []struct {
		name   string
		change func(*gameapi.LoanQuote)
	}{
		{"above double Goldsmiths", func(q *gameapi.LoanQuote) { q.AnnualRateBPS = 6_001 }},
		{"self lender", func(q *gameapi.LoanQuote) { q.LenderCompanyID = recoveryCompanyID }},
		{"wrong principal", func(q *gameapi.LoanQuote) { q.PrincipalPence++ }},
		{"wrong term", func(q *gameapi.LoanQuote) { q.TermGameDays = 180 }},
		{"balloon repayment", func(q *gameapi.LoanQuote) { q.Repayment = "balloon" }},
	} {
		t.Run(test.name, func(t *testing.T) {
			quote := kraken
			test.change(&quote)
			request.Quotes = []gameapi.LoanQuote{quote}
			if chosen := runner.americaFlagshipLender(request, intent); chosen != nil {
				t.Fatalf("unacceptable quote selected: %+v", chosen)
			}
		})
	}
}
