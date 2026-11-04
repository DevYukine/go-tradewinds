package engine

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"math"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/DevYukine/go-tradewinds/internal/gameapi"
	"github.com/DevYukine/go-tradewinds/internal/store"
)

type loanBridgeQuoteProblem struct {
	Status int
	Code   string
	Detail string
}

type loanBridgeTestDue struct {
	NeedPence    int64 `json:"need_pence"`
	BridgedPence int64 `json:"bridged_pence"`
	Pending      bool  `json:"pending"`
}

// Keep the existing recovery world's real captures, reserves, missions and
// journal. Only the clock and Goldsmiths debt/quote surface differ here.
type loanBridgeWorld struct {
	h             *recoveryHarness
	mu            sync.Mutex
	now           time.Time
	loans         []gameapi.Loan
	quotes        map[string]gameapi.GoldsmithsQuote
	receipts      map[string]json.RawMessage
	rebuild       map[string]int64
	caps          map[string]int64
	queries       []gameapi.GoldsmithsQuoteRequest
	quoteStatus   int
	quoteCode     string
	quoteProblems map[string]loanBridgeQuoteProblem
	acceptStatus  int
	acceptCode    string
	dropReceipt   bool
}

func loanBridgeFixture(t *testing.T, payment int64) (*loanBridgeWorld, *Runner) {
	t.Helper()
	h, runner := loanCapacityFixture(t, 750_000, false)
	now := time.Now().UTC()
	loan := snapshotTestLoan()
	loan.ID, loan.CollateralKind, loan.CollateralStatus = "bridge-target", "unsecured", "none"
	loan.CollateralShipID = nil
	loan.PrincipalPence, loan.OutstandingPrincipalPence, loan.OwedPence = 30*payment, 30*payment, 30*payment
	loan.InstallmentPence, loan.NextPaymentPence, loan.TotalRepayablePence = new(payment), new(payment), new(36*payment)
	loan.CreatedAt, loan.UpdatedAt = now.Add(-2*time.Hour), now
	loan.DisbursedAt, loan.MaturesAt, loan.NextPaymentDueAt = new(loan.CreatedAt), new(now.Add(6*24*time.Hour)), new(now.Add(time.Hour))
	world := &loanBridgeWorld{
		h: h, now: now, loans: []gameapi.Loan{loan},
		quotes: make(map[string]gameapi.GoldsmithsQuote), receipts: make(map[string]json.RawMessage), quoteProblems: make(map[string]loanBridgeQuoteProblem),
		rebuild: map[string]int64{recoveryShipBlastoise: 4_000_000, recoveryShipCharizard: 2_000_000},
		caps:    make(map[string]int64),
	}
	h.api.setFleet([]map[string]any{
		sailingShipRow(recoveryShipBlastoise, "Blastoise", recoveryHomePort, recoveryFarPort),
		sailingShipRow(recoveryShipCharizard, "Charizard", recoveryFarPort, recoveryHomePort),
	})
	h.api.setRates(Rate{FromRegion: "north", ToRegion: "south"}, Rate{FromRegion: "south", ToRegion: "north"})
	base := h.api.server.Config.Handler
	h.api.server.Config.Handler = http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		switch {
		case request.Method == http.MethodGet && request.URL.Path == "/v1/world/clock":
			clock := h.api.clockDocument()
			world.mu.Lock()
			clock["real_utc"] = world.now.Format(time.RFC3339Nano)
			world.mu.Unlock()
			clock["calendar"].(map[string]any)["time_scale"] = 60
			h.api.writeJSON(writer, http.StatusOK, clock)
		case request.Method == http.MethodGet && request.URL.Path == "/v1/loan/list":
			if request.URL.Query().Get("role") != "borrower" {
				t.Error("bridge capture did not request borrower-only loan authority")
			}
			world.mu.Lock()
			loans := make([]gameapi.Loan, 0, len(world.loans))
			for _, row := range world.loans {
				if status := request.URL.Query().Get("status"); status == "" || row.Status == status {
					loans = append(loans, row)
				}
			}
			world.mu.Unlock()
			h.api.writeJSON(writer, http.StatusOK, loans)
		case request.Method == http.MethodGet && request.URL.Path == "/v1/loan/quote":
			world.quote(writer, request)
		case request.Method == http.MethodPost && request.URL.Path == "/v1/loan/accept-quote":
			world.accept(writer, request)
		default:
			base.ServeHTTP(writer, request)
		}
	})
	loanCapacityRefresh(t, h, runner)
	world.cash(t, runner, nil)
	return world, runner
}

func (world *loanBridgeWorld) quote(writer http.ResponseWriter, request *http.Request) {
	query := request.URL.Query()
	principal, principalErr := strconv.ParseInt(query.Get("principal_pence"), 10, 64)
	term, termErr := strconv.ParseInt(query.Get("term_game_days"), 10, 32)
	if principalErr != nil || termErr != nil || principal <= 0 {
		world.h.t.Error("bridge quote request has invalid integer terms")
		world.h.api.writeProblem(writer, http.StatusBadRequest, "invalid_terms", "invalid principal or term")
		return
	}
	terms := gameapi.GoldsmithsQuoteRequest{
		PrincipalPence: principal, TermGameDays: int32(term), Repayment: query.Get("repayment"),
		ShipID: query.Get("ship_id"), BankingRegion: query.Get("banking_region"),
	}
	world.mu.Lock()
	defer world.mu.Unlock()
	world.queries = append(world.queries, terms)
	if world.quoteStatus != 0 {
		// In particular, a walk-away refusal does not supply a retry date.
		world.h.api.writeProblem(writer, world.quoteStatus, world.quoteCode, "Goldsmiths will not quote this borrower")
		return
	}
	if problem, exists := world.quoteProblems[terms.BankingRegion+"|"+terms.ShipID]; exists {
		world.h.api.writeProblem(writer, problem.Status, problem.Code, problem.Detail)
		return
	}
	cap, exists := world.caps[terms.ShipID]
	if !exists {
		cap = 750_000
	}
	if principal > cap {
		world.h.api.writeProblem(writer, http.StatusBadRequest, "bad_request",
			fmt.Sprintf("Goldsmiths lends at most %dp against this ship now, limited by what the house can spare", cap))
		return
	}
	installment := int64(math.Ceil(float64(principal) * (0.30 / 36) / (1 - math.Pow(1+0.30/36, -36))))
	shipID := terms.ShipID
	quote := gameapi.GoldsmithsQuote{
		Quote: gameapi.LoanQuote{
			ID:              fmt.Sprintf("bridge-quote-%s-%d-%d", shipID, principal, len(world.queries)),
			LenderCompanyID: "goldsmiths", BorrowerCompanyID: world.h.account.CompanyID,
			BankingRegion: terms.BankingRegion, PrincipalPence: principal, AnnualRateBPS: 3_000,
			TermGameDays: terms.TermGameDays, Repayment: terms.Repayment,
			InstallmentPence: &installment, TotalRepayablePence: installment * 36, CollateralShipID: &shipID,
			Status: "open", CreatedAt: world.now, ExpiresAt: world.now.Add(10 * time.Minute),
		},
		RebuildValuePence: world.rebuild[shipID], LoanToValueBPS: 4_000, MaxPrincipalPence: cap, Recourse: true,
	}
	world.quotes[quote.Quote.ID] = quote
	world.h.api.writeJSON(writer, http.StatusOK, quote)
}

func (world *loanBridgeWorld) accept(writer http.ResponseWriter, request *http.Request) {
	body, err := io.ReadAll(request.Body)
	if err != nil {
		world.h.t.Error(err)
		writer.WriteHeader(http.StatusBadRequest)
		return
	}
	mission, err := world.h.db.GetMission(request.Context(), world.h.account.ID, "__loans")
	var data missionData
	if err != nil || mission == nil || mission.Phase != "loan_bridge" || json.Unmarshal(mission.Data, &data) != nil ||
		data.LoanBridge == nil || data.Capacity != nil || data.Action == nil ||
		data.Action.ID != request.Header.Get("Idempotency-Key") || !bytes.Equal(data.Action.Body, body) {
		world.h.t.Error("bridge accept reached the server without its exact durable loan-service intent")
		writer.WriteHeader(http.StatusConflict)
		return
	}
	journaled, err := world.h.db.HasAction(request.Context(), world.h.account.ID, data.Action.ID)
	if err != nil || !journaled {
		world.h.t.Error("bridge accept reached the server before journal insertion")
		writer.WriteHeader(http.StatusConflict)
		return
	}
	snapshot, err := world.h.db.Snapshot(request.Context(), world.h.account.ID)
	if err != nil || decode[map[string]loanBridgeTestDue](snapshot, "_loan_bridge_due")[loanRefinanceDueKey(data.LoanBridge.Before)].NeedPence <= 0 {
		world.h.t.Error("bridge accept reached the server before its due-date marker was persisted")
		writer.WriteHeader(http.StatusConflict)
		return
	}
	var payload gameapi.QuoteActionRequest
	if json.Unmarshal(body, &payload) != nil || payload.QuoteID != data.LoanBridge.Quote.Quote.ID {
		world.h.t.Error("bridge accept did not identify the exact admitted quote")
		writer.WriteHeader(http.StatusConflict)
		return
	}
	world.mu.Lock()
	defer world.mu.Unlock()
	if world.acceptStatus != 0 {
		world.h.api.mu.Lock()
		world.h.api.record(request, body, world.acceptStatus)
		world.h.api.mu.Unlock()
		world.h.api.writeProblem(writer, world.acceptStatus, world.acceptCode, "Goldsmiths refused this bridge")
		return
	}
	key := request.Header.Get("Idempotency-Key")
	response := world.receipts[key]
	if response == nil {
		quote, exists := world.quotes[payload.QuoteID]
		if !exists {
			world.h.t.Error("bridge accept used a quote the local server never issued")
			writer.WriteHeader(http.StatusNotFound)
			return
		}
		loan := snapshotTestLoan()
		loan.ID, loan.QuoteID, loan.BankingRegion = fmt.Sprintf("bridge-disbursed-%d", len(world.receipts)+1), &quote.Quote.ID, quote.Quote.BankingRegion
		loan.PrincipalPence, loan.OutstandingPrincipalPence, loan.OwedPence = quote.Quote.PrincipalPence, quote.Quote.PrincipalPence, quote.Quote.PrincipalPence
		loan.InstallmentPence, loan.NextPaymentPence, loan.TotalRepayablePence = quote.Quote.InstallmentPence, quote.Quote.InstallmentPence, &quote.Quote.TotalRepayablePence
		loan.CollateralShipID, loan.CollateralValuePence = quote.Quote.CollateralShipID, quote.RebuildValuePence
		loan.CreatedAt, loan.UpdatedAt = world.now, world.now
		loan.DisbursedAt, loan.MaturesAt = new(world.now), new(world.now.Add(gameDuration(360, 60)))
		loan.NextPaymentDueAt = new(world.now.Add(gameDuration(10, 60)))
		world.loans = append(world.loans, loan)
		response = encode(loan)
		world.receipts[key] = response
		region := "north"
		if loan.BankingRegion == "channel" {
			region = "south"
		}
		world.h.api.mu.Lock()
		world.h.api.vaultBalance[region] += loan.PrincipalPence
		world.h.api.mu.Unlock()
	}
	status := http.StatusOK
	if world.dropReceipt {
		status = 0
	}
	world.h.api.mu.Lock()
	world.h.api.record(request, body, status)
	world.h.api.mu.Unlock()
	if world.dropReceipt {
		connection, _, err := writer.(http.Hijacker).Hijack()
		if err != nil {
			world.h.t.Error(err)
			return
		}
		_ = connection.Close()
		return
	}
	world.h.api.writeJSON(writer, http.StatusOK, response)
}

func (world *loanBridgeWorld) setLoans(loans ...gameapi.Loan) {
	world.mu.Lock()
	world.loans = append([]gameapi.Loan{}, loans...)
	world.mu.Unlock()
}

// Fund only upkeep collections scheduled before the fixture's installment,
// then leave the requested hard-cash remainder in the two real vaults.
func (world *loanBridgeWorld) cash(t *testing.T, runner *Runner, free map[string]int64) {
	t.Helper()
	clock := runner.stateCache.Clock
	collectionBy := clock.RealUTC
	loan := runner.stateCache.Loans[0]
	if loan.ArrearsPence <= 0 && loan.NextPaymentDueAt != nil && loan.NextPaymentDueAt.After(collectionBy) {
		collectionBy = *loan.NextPaymentDueAt
	}
	day := 24 * time.Hour / time.Duration(clock.Calendar.TimeScale)
	elapsed := time.Duration(clock.Calendar.TotalGameHours%24)*time.Hour/time.Duration(clock.Calendar.TimeScale) +
		time.Duration(clock.Calendar.Minute)*time.Minute/time.Duration(clock.Calendar.TimeScale)
	days := int64((elapsed + collectionBy.Sub(clock.RealUTC)) / day)
	home, exists := findPort(runner.stateCache.Catalog, runner.stateCache.HomePortID)
	if !exists {
		t.Fatal("bridge fixture has no home billing region")
	}
	required := map[string]int64{home.BankingRegion: days * DailyUpkeep(runner.stateCache.Ships, runner.stateCache.Catalog, len(runner.stateCache.Factors))}
	world.h.api.setVaultBalance("north", required["north_sea"]+free["north_sea"])
	world.h.api.setVaultBalance("south", required["channel"]+free["channel"])
	loanCapacityRefresh(t, world.h, runner)
	if !runner.loanAuthority() || !runner.loanRequestAuthority() {
		t.Fatal("bridge fixture lacks complete borrower and request authority")
	}
}

func (world *loanBridgeWorld) requests() []gameapi.GoldsmithsQuoteRequest {
	world.mu.Lock()
	defer world.mu.Unlock()
	return append([]gameapi.GoldsmithsQuoteRequest{}, world.queries...)
}

func loanBridgeMarker(t *testing.T, world *loanBridgeWorld, loan gameapi.Loan) {
	t.Helper()
	snapshot, err := world.h.db.Snapshot(world.h.ctx, world.h.account.ID)
	if err != nil {
		t.Fatal(err)
	}
	if decode[map[string]loanBridgeTestDue](snapshot, "_loan_bridge_due")[loanRefinanceDueKey(loan)].NeedPence <= 0 {
		t.Fatalf("bridge due-date marker was not persisted for %s", loanRefinanceDueKey(loan))
	}
}

func loanBridgeStage(t *testing.T, world *loanBridgeWorld, runner *Runner, loanID string, principal int64, shipID string) (*store.Mission, missionData) {
	t.Helper()
	loan, exists := runner.loanByID(loanID)
	if !exists {
		t.Fatalf("bridge target %s is absent from authoritative loans", loanID)
	}
	return loanBridgeStageInRegion(t, world, runner, loanID, principal, shipID, loan.BankingRegion)
}

func loanBridgeStageInRegion(t *testing.T, world *loanBridgeWorld, runner *Runner, loanID string, principal int64, shipID, region string) (*store.Mission, missionData) {
	t.Helper()
	if changed, err := runner.serviceLoans(world.h.ctx); err != nil || !changed {
		t.Fatalf("short installment did not stage bridge: changed=%v err=%v", changed, err)
	}
	mission, data := world.h.mission("__loans")
	if mission.Phase != "loan_bridge" || data.LoanBridge == nil || data.LoanBridge.Before.ID != loanID ||
		data.Action == nil || data.Action.Stage != "loan-accept" || data.Action.Path != "/v1/loan/accept-quote" ||
		data.Action.Method != http.MethodPost || data.Plan.Kind != "treasury" || data.Capacity != nil {
		t.Fatalf("bridge did not persist its loan-service action: phase=%s data=%+v", mission.Phase, data)
	}
	quote := data.LoanBridge.Quote.Quote
	if quote.PrincipalPence != principal || quote.BankingRegion != region ||
		quote.TermGameDays != 360 || quote.Repayment != "amortised" || quote.CollateralShipID == nil || *quote.CollateralShipID != shipID ||
		data.LoanBridge.ShortfallPence <= 0 || !runner.validGoldsmithsQuote(data.LoanBridge.Quote, shipID, quote.BankingRegion, principal) {
		t.Fatalf("wrong exact bridge principal, region or collateral: %+v", data.LoanBridge)
	}
	if !bytes.Equal(data.Action.Body, encode(gameapi.QuoteActionRequest{QuoteID: quote.ID})) {
		t.Fatalf("accept body changed the admitted exact quote: %s", data.Action.Body)
	}
	if exists, err := world.h.db.HasAction(world.h.ctx, world.h.account.ID, data.Action.ID); err != nil || exists {
		t.Fatalf("staging sent or journaled the bridge before dispatch: exists=%v err=%v", exists, err)
	}
	loanBridgeMarker(t, world, data.LoanBridge.Before)
	return mission, data
}

func loanBridgeJournal(t *testing.T, world *loanBridgeWorld, id string) store.Action {
	t.Helper()
	rows, err := world.h.db.ReplayActionsByID(world.h.ctx, world.h.account.ID, []string{id}, time.Now().UTC().Add(time.Second))
	if err != nil || len(rows) != 1 {
		t.Fatalf("missing exact bridge journal row: rows=%+v err=%v", rows, err)
	}
	return rows[0]
}

func loanBridgeDecision(t *testing.T, world *loanBridgeWorld, reason string) {
	t.Helper()
	rows, err := world.h.db.History(world.h.ctx, world.h.account.ID, "decisions", store.MaxHistoryLimit)
	if err != nil {
		t.Fatal(err)
	}
	for _, row := range rows.([]store.Decision) {
		if row.Kind == "loan_bridge" && row.ShipID == "__loans" && strings.Contains(row.Reason, reason) {
			return
		}
	}
	t.Fatalf("bridge did not record a durable loan_bridge decision containing %q", reason)
}

func TestLoanBridgeStagesOneAcceptAndReplaysPersistedReceipt(t *testing.T) {
	world, runner := loanBridgeFixture(t, 100_000)
	world.cash(t, runner, map[string]int64{"north_sea": 25_000})
	mission, staged := loanBridgeStage(t, world, runner, "bridge-target", 100_000, recoveryShipCharizard)
	id, generation := staged.Action.ID, mission.Generation
	if len(world.requests()) != 2 || world.h.api.mutationCount("/v1/loan/accept-quote") != 0 {
		t.Fatal("bridge failed to inspect both actual appraisals or dispatched during staging")
	}
	if changed, err := runner.serviceLoans(world.h.ctx); err != nil || changed {
		t.Fatalf("pending bridge was replaced: changed=%v err=%v", changed, err)
	}
	_, pending := world.h.mission("__loans")
	if pending.Action.ID != id || len(world.requests()) != 2 {
		t.Fatal("second service pass changed the immutable pending bridge")
	}
	mission, data := world.h.mission("__loans")
	if err := runner.newActionPreflight(world.h.ctx, mission, &data); err != nil {
		t.Fatalf("emergency bridge was vetoed by a capacity funding gate: %v", err)
	}
	if changed, err := runner.dispatch(world.h.ctx, mission, &data); err != nil || !changed {
		t.Fatalf("bridge acceptance failed: changed=%v err=%v", changed, err)
	}
	mission, data = world.h.mission("__loans")
	if mission.Phase != "done" || data.Action != nil || data.LastActionID != id || data.Capacity != nil || data.ProceedsPence != 0 || data.ExpensesPence != 0 {
		t.Fatalf("bridge receipt entered growth or trading P&L instead of completing debt service: %+v %+v", mission, data)
	}
	journal := loanBridgeJournal(t, world, id)
	if journal.State != store.ActionComplete || journal.Status != http.StatusOK || len(world.h.pending()) != 0 {
		t.Fatalf("bridge receipt did not settle its real journal: %+v", journal)
	}
	// Crash between completing the journal and saving the consumed mission.
	world.h.putMission("__loans", generation, "loan_bridge", staged)
	runner = world.h.runner(primaryTestKey)
	loanCapacityRefresh(t, world.h, runner)
	mission, data = world.h.mission("__loans")
	if changed, err := runner.dispatch(world.h.ctx, mission, &data); err != nil || !changed {
		t.Fatalf("restart did not consume the existing bridge receipt: changed=%v err=%v", changed, err)
	}
	mission, data = world.h.mission("__loans")
	if mission.Phase != "done" || data.Action != nil || data.LastActionID != id || world.h.api.mutationCount("/v1/loan/accept-quote") != 1 {
		t.Fatal("receipt replay did not finish the same action without a second accept")
	}
	// Even if proceeds have gone, the original loan+due may never borrow again.
	world.cash(t, runner, nil)
	for range 3 {
		runner = world.h.runner(primaryTestKey)
		loanCapacityRefresh(t, world.h, runner)
		if _, err := runner.serviceLoans(world.h.ctx); err != nil {
			t.Fatal(err)
		}
	}
	world.h.missingMission("__loans")
	if len(world.requests()) != 2 || world.h.api.mutationCount("/v1/loan/accept-quote") != 1 {
		t.Fatal("restart duplicated the bridge for a marked due date")
	}
	loanBridgeMarker(t, world, staged.LoanBridge.Before)
	loanBridgeDecision(t, world, "accepted")
}

func TestLoanBridgeUsesAllVaultCashAndConservativeWindow(t *testing.T) {
	for _, test := range []struct {
		name        string
		freeNorth   int64
		freeChannel int64
		dueAfter    time.Duration
		arrears     int64
		want        bool
	}{
		{name: "short inside window", freeNorth: 99_999, dueAfter: time.Hour, want: true},
		{name: "exactly funded", freeNorth: 100_000, dueAfter: time.Hour},
		{name: "funded across vaults", freeNorth: 40_000, freeChannel: 60_000, dueAfter: time.Hour},
		{name: "outside window", dueAfter: serverClockStaleness + gameDuration(3, 60) + time.Second},
		{name: "conservative boundary", dueAfter: serverClockStaleness + gameDuration(3, 60), want: true},
		{name: "arrears override future due", freeNorth: 24_999, dueAfter: 2 * time.Hour, arrears: 25_000, want: true},
	} {
		t.Run(test.name, func(t *testing.T) {
			world, runner := loanBridgeFixture(t, 100_000)
			loan := runner.stateCache.Loans[0]
			loan.NextPaymentDueAt, loan.ArrearsPence = new(world.now.Add(test.dueAfter)), test.arrears
			if test.arrears > 0 {
				loan.NextPaymentPence = new(test.arrears)
			}
			world.setLoans(loan)
			loanCapacityRefresh(t, world.h, runner)
			world.cash(t, runner, map[string]int64{"north_sea": test.freeNorth, "channel": test.freeChannel})
			if test.want {
				loanBridgeStage(t, world, runner, loan.ID, 100_000, recoveryShipCharizard)
				return
			}
			if changed, err := runner.serviceLoans(world.h.ctx); err != nil || changed {
				t.Fatalf("unneeded bridge advanced loan service: changed=%v err=%v", changed, err)
			}
			world.h.missingMission("__loans")
			if len(world.requests()) != 0 || len(world.h.pending()) != 0 || world.h.api.mutationCount("/v1/loan/accept-quote") != 0 {
				t.Fatal("funded or not-yet-due installment reached Goldsmiths")
			}
		})
	}
}

func TestLoanBridgeBoundsPrincipalWithoutCapacityReserveOrCoverage(t *testing.T) {
	for _, test := range []struct {
		shortfall int64
		principal int64
	}{
		{1, 100_000}, {100_000, 100_000}, {100_001, 110_000},
		{139_001, 140_000}, {750_000, 750_000}, {950_001, 750_000},
	} {
		t.Run(fmt.Sprintf("short_%dp", test.shortfall), func(t *testing.T) {
			world, runner := loanBridgeFixture(t, 1_000_000)
			world.cash(t, runner, map[string]int64{"north_sea": 1_000_000 - test.shortfall})
			runner.stateCache.Markets = nil
			runner.stateCache.Quotes = nil
			_, data := loanBridgeStage(t, world, runner, "bridge-target", test.principal, recoveryShipCharizard)
			if got := loanBridgeTestNeed(t, world, runner, data.LoanBridge.Before); got != test.shortfall {
				t.Fatalf("bridge counted loan payoff/installment reserves or operating floors as hard bills: got %d want %d", got, test.shortfall)
			}
			for _, request := range world.requests() {
				if request.PrincipalPence != test.principal || request.TermGameDays != 360 || request.Repayment != "amortised" || request.BankingRegion != "north_sea" {
					t.Fatalf("wrong exact requested bridge terms: %+v", request)
				}
			}
		})
	}
}

func TestLoanBridgeSkipsEncumberedCollateral(t *testing.T) {
	for _, reason := range []string{"active lien", "defaulted lien", "mortgage", "open listing"} {
		t.Run(reason, func(t *testing.T) {
			world, runner := loanBridgeFixture(t, 100_000)
			loans := append([]gameapi.Loan{}, runner.stateCache.Loans...)
			if reason == "open listing" {
				world.h.api.mu.Lock()
				world.h.api.shipListings = []map[string]any{{"id": "bridge-listing", "ship_id": recoveryShipCharizard, "status": "open"}}
				world.h.api.mu.Unlock()
			} else {
				blocked := snapshotTestLoan()
				blocked.ID, blocked.CollateralShipID = "bridge-encumbrance", new(recoveryShipCharizard)
				// Active/defaulted liens remain encumbrances even when their
				// collateral-status flag alone would not block freeLoanShips.
				blocked.CollateralStatus = "none"
				blocked.NextPaymentDueAt = new(world.now.Add(12 * time.Hour))
				if reason == "defaulted lien" {
					blocked.Status = "defaulted"
				}
				blocked.IsMortgage = reason == "mortgage"
				loans = append(loans, blocked)
			}
			world.setLoans(loans...)
			loanCapacityRefresh(t, world.h, runner)
			world.cash(t, runner, nil)
			loanBridgeStage(t, world, runner, "bridge-target", 100_000, recoveryShipBlastoise)
			requests := world.requests()
			if len(requests) != 1 || requests[0].ShipID != recoveryShipBlastoise {
				t.Fatalf("bridge requested a quote against %s collateral: %+v", reason, requests)
			}
		})
	}
}

func TestLoanBridgeChoosesActualCheapestRebuildWithEnoughQuoteCapacity(t *testing.T) {
	for _, lowCap := range []bool{false, true} {
		t.Run(fmt.Sprintf("cheaper_ship_cap_insufficient_%v", lowCap), func(t *testing.T) {
			world, runner := loanBridgeFixture(t, 100_000)
			// Catalog prices are identical; the first ship has fewer guns, but
			// only actual validated Goldsmiths appraisals decide the lien.
			world.h.api.mu.Lock()
			world.h.api.fleet[0]["ship"].(map[string]any)["cannons_count"] = 0
			world.h.api.fleet[1]["ship"].(map[string]any)["cannons_count"] = 8
			world.h.api.mu.Unlock()
			world.mu.Lock()
			if lowCap {
				world.caps[recoveryShipCharizard] = 99_999
			}
			world.mu.Unlock()
			loanCapacityRefresh(t, world.h, runner)
			world.cash(t, runner, nil)
			want := recoveryShipCharizard
			if lowCap {
				want = recoveryShipBlastoise
			}
			loanBridgeStage(t, world, runner, "bridge-target", 100_000, want)
			if requests := world.requests(); len(requests) != 2 || requests[0].ShipID != recoveryShipBlastoise || requests[1].ShipID != recoveryShipCharizard {
				t.Fatalf("bridge did not compare both fresh actual quotes: %+v", requests)
			}
		})
	}
}

func TestLoanBridgeMarkerSeparatesIndependentLoansAndNextDueDate(t *testing.T) {
	world, runner := loanBridgeFixture(t, 100_000)
	first := runner.stateCache.Loans[0]
	_, data := loanBridgeStage(t, world, runner, first.ID, 100_000, recoveryShipCharizard)
	// A crash may retain the snapshot marker but lose an unsent mission.
	if err := world.h.db.DeleteMission(world.h.ctx, world.h.account.ID, "__loans"); err != nil {
		t.Fatal(err)
	}
	runner = world.h.runner(primaryTestKey)
	loanCapacityRefresh(t, world.h, runner)
	if changed, err := runner.serviceLoans(world.h.ctx); changed || err != nil || len(world.requests()) != 2 {
		t.Fatalf("marker did not stop a lost-mission duplicate: changed=%v err=%v", changed, err)
	}
	second := first
	second.ID, second.BankingRegion = "bridge-other-target", "channel"
	second.NextPaymentDueAt = new(first.NextPaymentDueAt.Add(time.Minute))
	world.setLoans(first, second)
	loanCapacityRefresh(t, world.h, runner)
	_, secondData := loanBridgeStage(t, world, runner, second.ID, 200_000, recoveryShipCharizard)
	if loanBridgeTestNeed(t, world, runner, secondData.LoanBridge.Before) != 200_000 || secondData.LoanBridge.Quote.Quote.BankingRegion != "channel" {
		t.Fatal("independent due loan reused earlier collections' cash or borrowed in the wrong region")
	}
	if err := world.h.db.DeleteMission(world.h.ctx, world.h.account.ID, "__loans"); err != nil {
		t.Fatal(err)
	}
	// The next automatic installment is ten game days later, not a ban on
	// the loan forever. Keep the other loan outside this next cash window.
	world.mu.Lock()
	world.now = world.now.Add(gameDuration(10, 60))
	world.mu.Unlock()
	first.NextPaymentDueAt = new(first.NextPaymentDueAt.Add(gameDuration(10, 60)))
	second.NextPaymentDueAt = new(world.now.Add(12 * time.Hour))
	world.setLoans(first, second)
	runner = world.h.runner(primaryTestKey)
	loanCapacityRefresh(t, world.h, runner)
	loanBridgeStage(t, world, runner, first.ID, 100_000, recoveryShipCharizard)
	loanBridgeMarker(t, world, data.LoanBridge.Before)
	loanBridgeMarker(t, world, secondData.LoanBridge.Before)
	loanBridgeMarker(t, world, first)
	if len(world.requests()) != 6 || world.h.api.mutationCount("/v1/loan/accept-quote") != 0 || len(world.h.pending()) != 0 {
		t.Fatal("due-date markers conflated independent loans/dates or sent an unsent action")
	}
}

func TestLoanBridgeQuoteRefusalWithoutRetryDateSurvivesRestart(t *testing.T) {
	for _, test := range []struct {
		status int
		code   string
	}{
		{http.StatusForbidden, "goldsmiths_walk_away_ban"},
	} {
		t.Run(test.code, func(t *testing.T) {
			world, runner := loanBridgeFixture(t, 100_000)
			loan := runner.stateCache.Loans[0]
			world.mu.Lock()
			world.quoteStatus, world.quoteCode = test.status, test.code
			world.mu.Unlock()
			if _, err := runner.serviceLoans(world.h.ctx); err != nil {
				t.Fatalf("domain quote refusal escaped loan service: %v", err)
			}
			world.h.missingMission("__loans")
			loanBridgeMarker(t, world, loan)
			loanBridgeDecision(t, world, test.code)
			for range 3 {
				runner = world.h.runner(primaryTestKey)
				loanCapacityRefresh(t, world.h, runner)
				if _, err := runner.serviceLoans(world.h.ctx); err != nil {
					t.Fatal(err)
				}
			}
			if len(world.requests()) != 1 || world.h.api.mutationCount("/v1/loan/accept-quote") != 0 || len(world.h.pending()) != 0 {
				t.Fatal("quote refusal without Retry-After retried collateral or borrowed after restart")
			}
		})
	}
}

func TestLoanBridgeAcceptWalkAwayRefusalSettlesWithoutAuthStop(t *testing.T) {
	world, runner := loanBridgeFixture(t, 100_000)
	mission, data := loanBridgeStage(t, world, runner, "bridge-target", 100_000, recoveryShipCharizard)
	id, before := data.Action.ID, data.LoanBridge.Before
	world.mu.Lock()
	world.acceptStatus, world.acceptCode = http.StatusForbidden, "goldsmiths_walk_away_ban"
	world.mu.Unlock()
	if err := runner.newActionPreflight(world.h.ctx, mission, &data); err != nil {
		t.Fatal(err)
	}
	if _, err := runner.dispatch(world.h.ctx, mission, &data); err != nil {
		t.Fatalf("walk-away domain refusal became an account authorization stop: %v", err)
	}
	world.h.missingMission("__loans")
	journal := loanBridgeJournal(t, world, id)
	if journal.State != store.ActionComplete || journal.Status != http.StatusForbidden || len(world.h.pending()) != 0 {
		t.Fatalf("refused bridge did not settle its exact journal request: %+v", journal)
	}
	if len(decode[map[string]time.Time](runner.snapshot, "_capacity_financing_backoffs")) != 0 {
		t.Fatal("bridge refusal contaminated capacity financing backoffs")
	}
	for range 3 {
		runner = world.h.runner(primaryTestKey)
		loanCapacityRefresh(t, world.h, runner)
		if _, err := runner.serviceLoans(world.h.ctx); err != nil {
			t.Fatal(err)
		}
	}
	world.h.missingMission("__loans")
	if len(world.requests()) != 2 || world.h.api.mutationCount("/v1/loan/accept-quote") != 1 {
		t.Fatal("refused bridge was requoted or accepted again after restart")
	}
	loanBridgeMarker(t, world, before)
	loanBridgeDecision(t, world, "goldsmiths_walk_away_ban")
}

func TestLoanBridgeLostReceiptReconcilesExactLoanAfterRestart(t *testing.T) {
	world, runner := loanBridgeFixture(t, 100_000)
	mission, data := loanBridgeStage(t, world, runner, "bridge-target", 100_000, recoveryShipCharizard)
	id, body := data.Action.ID, bytes.Clone(data.Action.Body)
	world.mu.Lock()
	world.dropReceipt = true
	world.mu.Unlock()
	if _, err := runner.dispatch(world.h.ctx, mission, &data); err != nil {
		t.Fatal(err)
	}
	attempts := world.h.api.mutationsFor("/v1/loan/accept-quote")
	if len(attempts) == 0 {
		t.Fatal("lost-receipt fixture did not send the bridge")
	}
	for _, attempt := range attempts {
		if attempt.IdempotencyKey != id || !bytes.Equal(attempt.Body, body) {
			t.Fatal("transport retry changed the exact bridge request or idempotency key")
		}
	}
	if journal := loanBridgeJournal(t, world, id); journal.State != store.ActionPending || len(world.h.pending()) != 1 {
		t.Fatalf("lost bridge receipt was treated as definitive: %+v", journal)
	}
	world.mu.Lock()
	world.dropReceipt = false
	if len(world.receipts) != 1 || len(world.loans) != 2 {
		t.Error("transport retries disbursed more than one quote-specific bridge")
	}
	world.mu.Unlock()
	runner = world.h.runner(primaryTestKey)
	loanCapacityRefresh(t, world.h, runner)
	mission, data = world.h.mission("__loans")
	if data.Action.ID != id || !bytes.Equal(data.Action.Body, body) {
		t.Fatal("restart replaced the unresolved immutable bridge intent")
	}
	if changed, err := runner.dispatch(world.h.ctx, mission, &data); err != nil || !changed {
		t.Fatalf("fresh quote-specific loan did not reconcile the lost receipt: changed=%v err=%v", changed, err)
	}
	mission, data = world.h.mission("__loans")
	if mission.Phase != "done" || data.Action != nil || data.Capacity != nil || len(world.h.pending()) != 0 ||
		world.h.api.mutationCount("/v1/loan/accept-quote") != len(attempts) {
		t.Fatal("lost-receipt restart resent the bridge or routed it into growth")
	}
	if journal := loanBridgeJournal(t, world, id); journal.State != store.ActionReconciled {
		t.Fatalf("authoritative bridge loan did not reconcile the journal: %+v", journal)
	}
	loanBridgeDecision(t, world, "reconciled")
}

func TestLoanBridgeFirstSendRechecksCashAndCollateral(t *testing.T) {
	for _, test := range []struct {
		reason  string
		payment int64
		free    int64
		listed  bool
	}{
		{reason: "cash recovered", payment: 100_000, free: 100_000},
		{reason: "partial cash recovery reduces bounded principal", payment: 300_000, free: 200_001},
		{reason: "collateral listed", payment: 100_000, listed: true},
	} {
		t.Run(test.reason, func(t *testing.T) {
			world, runner := loanBridgeFixture(t, test.payment)
			_, staged := loanBridgeStage(t, world, runner, "bridge-target", test.payment, recoveryShipCharizard)
			if test.listed {
				world.h.api.mu.Lock()
				world.h.api.shipListings = []map[string]any{{"id": "late-bridge-listing", "ship_id": recoveryShipCharizard, "status": "open"}}
				world.h.api.mu.Unlock()
				loanCapacityRefresh(t, world.h, runner)
			} else {
				world.cash(t, runner, map[string]int64{"north_sea": test.free})
			}
			mission, data := world.h.mission("__loans")
			if err := runner.newActionPreflight(world.h.ctx, mission, &data); err == nil {
				t.Fatalf("first-send preflight ignored %s", test.reason)
			}
			if _, err := runner.dispatch(world.h.ctx, mission, &data); err != nil {
				t.Fatal(err)
			}
			if world.h.api.mutationCount("/v1/loan/accept-quote") != 0 || len(world.h.pending()) != 0 {
				t.Fatalf("first-send veto still sent or orphaned the bridge after %s", test.reason)
			}
			_, retired := world.h.mission("__loans")
			if retired.Action != nil {
				t.Fatal("first-send veto retained a dispatchable bridge action")
			}
			if exists, err := world.h.db.HasAction(world.h.ctx, world.h.account.ID, staged.Action.ID); err != nil || exists {
				t.Fatalf("first-send veto journaled an unsent bridge: exists=%v err=%v", exists, err)
			}
			loanBridgeNoMarker(t, world, runner, staged.LoanBridge.Before)
		})
	}
}

func TestLoanBridgeUnknownClockNeverBorrows(t *testing.T) {
	for _, reason := range []string{"failed capture", "unknown UTC", "paused", "unknown scale"} {
		t.Run(reason, func(t *testing.T) {
			world, runner := loanBridgeFixture(t, 100_000)
			switch reason {
			case "failed capture":
				meta := runner.meta["clock"]
				meta.Stale, meta.Complete, meta.Error = true, false, "clock unavailable"
				runner.meta["clock"] = meta
			case "unknown UTC":
				runner.stateCache.Clock.RealUTC = time.Time{}
			case "paused":
				runner.stateCache.Clock.Paused = true
			case "unknown scale":
				runner.stateCache.Clock.Calendar.TimeScale = 0
			}
			if changed, err := runner.serviceLoans(world.h.ctx); err != nil || changed {
				t.Fatalf("unknown-clock fallback initiated debt service borrowing: changed=%v err=%v", changed, err)
			}
			world.h.missingMission("__loans")
			if len(world.requests()) != 0 || world.h.api.mutationCount("/v1/loan/accept-quote") != 0 || len(world.h.pending()) != 0 {
				t.Fatal("unknown-clock fallback reached Goldsmiths")
			}
		})
	}
}
