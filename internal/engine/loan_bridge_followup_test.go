package engine

import (
	"bytes"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/DevYukine/go-tradewinds/internal/gameapi"
	"github.com/DevYukine/go-tradewinds/internal/store"
)

func loanBridgeTestNeed(t *testing.T, world *loanBridgeWorld, runner *Runner, loan gameapi.Loan) int64 {
	t.Helper()
	need, err := runner.loanBridgeNeed(world.h.ctx, loan)
	if err != nil {
		t.Fatalf("calculate due-bound hard need: %v", err)
	}
	return need
}

func loanBridgeTestProgress(t *testing.T, world *loanBridgeWorld, runner *Runner, loan gameapi.Loan, need, bridged int64, pending bool) {
	t.Helper()
	key := loanRefinanceDueKey(loan)
	snapshot, err := world.h.db.Snapshot(world.h.ctx, world.h.account.ID)
	if err != nil {
		t.Fatal(err)
	}
	want := loanBridgeTestDue{NeedPence: need, BridgedPence: bridged, Pending: pending}
	for name, frame := range map[string]map[string]json.RawMessage{"memory": runner.snapshot, "durable": snapshot} {
		if got := decode[map[string]loanBridgeTestDue](frame, "_loan_bridge_due")[key]; got != want {
			t.Fatalf("%s bridge progress for %s = %+v, want %+v", name, key, got, want)
		}
	}
}

func loanBridgeRetryAt(t *testing.T, world *loanBridgeWorld, runner *Runner, loan gameapi.Loan) time.Time {
	t.Helper()
	key := loanRefinanceDueKey(loan)
	snapshot, err := world.h.db.Snapshot(world.h.ctx, world.h.account.ID)
	if err != nil {
		t.Fatal(err)
	}
	deadline := decode[map[string]time.Time](snapshot, "_loan_bridge_retry_at")[key]
	if deadline.IsZero() || !deadline.Equal(decode[map[string]time.Time](runner.snapshot, "_loan_bridge_retry_at")[key]) {
		t.Fatalf("bridge retry for %s is missing or differs from its durable deadline: %s", key, deadline)
	}
	return deadline
}

// Change only the persisted deadline, so tests cover restart expiry without
// sleeping through a live game-clock service window.
func loanBridgeExpireRetry(t *testing.T, world *loanBridgeWorld, runner *Runner, loan gameapi.Loan) {
	t.Helper()
	loanBridgeRetryAt(t, world, runner, loan)
	retries := decode[map[string]time.Time](runner.snapshot, "_loan_bridge_retry_at")
	retries[loanRefinanceDueKey(loan)] = time.Now().UTC().Add(-time.Second)
	runner.snapshot["_loan_bridge_retry_at"] = encode(retries)
	if err := world.h.db.SaveSnapshot(world.h.ctx, world.h.account.ID, runner.snapshot); err != nil {
		t.Fatal(err)
	}
}

func TestLoanBridgeQuoteRejectsPrincipalAboveCandidateCap(t *testing.T) {
	world, runner := loanBridgeFixture(t, 100_000)
	world.mu.Lock()
	world.caps[recoveryShipCharizard] = 139_999
	world.mu.Unlock()
	_, err := runner.api.GetGoldsmithsQuote(world.h.ctx, gameapi.GoldsmithsQuoteRequest{
		PrincipalPence: 140_000, TermGameDays: 360, Repayment: "amortised",
		ShipID: recoveryShipCharizard, BankingRegion: "north_sea",
	})
	var problem *gameapi.APIError
	if !errors.As(err, &problem) || problem.Status != http.StatusBadRequest || problem.Code != "bad_request" ||
		!strings.Contains(problem.Detail, "at most 139999p") {
		t.Fatalf("candidate cap must refuse the request, not issue an unusable 200 quote: %v", err)
	}
	world.mu.Lock()
	issued := len(world.quotes)
	world.mu.Unlock()
	if issued != 0 || len(world.requests()) != 1 {
		t.Fatal("over-cap refusal issued a quote or did not record its exact request")
	}
}

func TestLoanBridgeOperatingFloorsDoNotInflatePrincipal(t *testing.T) {
	world, runner := loanBridgeFixture(t, 139_001)
	loan := runner.stateCache.Loans[0]
	for id, port := range runner.stateCache.Ports {
		port.BerthDuesPerDayPence = 100_000_000
		runner.stateCache.Ports[id] = port
	}
	margin, err := runner.loanBridgeShortfall(world.h.ctx, loan)
	if err != nil || margin <= 750_000 {
		t.Fatalf("fixture did not retain huge regional trigger floors: margin=%d err=%v", margin, err)
	}
	if need := loanBridgeTestNeed(t, world, runner, loan); need != 139_001 {
		t.Fatalf("sailing ships' hypothetical berth floors became scheduled bills: need=%d", need)
	}
	mission, data := loanBridgeStage(t, world, runner, loan.ID, 140_000, recoveryShipCharizard)
	id := data.Action.ID
	loanBridgeTestProgress(t, world, runner, loan, 139_001, 0, true)
	if err := runner.newActionPreflight(world.h.ctx, mission, &data); err != nil {
		t.Fatalf("huge trigger margin vetoed hard-need-sized principal: %v", err)
	}
	if changed, err := runner.dispatch(world.h.ctx, mission, &data); err != nil || !changed {
		t.Fatalf("hard-need bridge did not dispatch: changed=%v err=%v", changed, err)
	}
	if row := loanBridgeJournal(t, world, id); row.State != store.ActionComplete || row.Status != http.StatusOK {
		t.Fatalf("hard-need-sized acceptance did not settle its exact journal: %+v", row)
	}
	loanBridgeTestProgress(t, world, runner, loan, 139_001, 140_000, false)
}

func TestLoanBridgeNeedCountsOnlyCollectionsAndScheduledBillsByDue(t *testing.T) {
	world, runner := loanBridgeFixture(t, 200_000)
	clock := &runner.stateCache.Clock
	clock.Calendar.TotalGameHours, clock.Calendar.Minute = 23, 30
	loan := runner.stateCache.Loans[0]
	loan.NextPaymentDueAt = new(clock.RealUTC.Add(31 * time.Second))
	earlier := loan
	earlier.ID, earlier.NextPaymentPence, earlier.NextPaymentDueAt = "earlier-collection", new(int64(30_000)), new(clock.RealUTC.Add(15*time.Second))
	later := loan
	later.ID, later.NextPaymentPence, later.NextPaymentDueAt = "later-collection", new(int64(40_000)), new(clock.RealUTC.Add(32*time.Second))
	arrears := loan
	arrears.ID, arrears.Status, arrears.ArrearsPence = "earlier-arrears", "defaulted", 20_000
	arrears.NextPaymentPence, arrears.NextPaymentDueAt = new(int64(50_000)), new(clock.RealUTC.Add(time.Hour))
	runner.stateCache.Loans = []gameapi.Loan{loan, earlier, later, arrears}
	account := "bridge-need-north-vault"
	runner.stateCache.Vaults = []Vault{
		{AccountID: account, AccountType: "vault", BankingRegion: "north_sea", BalancePence: 10_000},
		{AccountID: "bridge-need-channel-vault", AccountType: "vault", BankingRegion: "channel"},
		{AccountID: "not-available-escrow", AccountType: "escrow", BankingRegion: "north_sea", BalancePence: 10_000_000},
	}
	runner.snapshot["ledger"] = encode([]ledgerBill{
		{ID: "old-rent", Debit: account, Credit: "warehouse", Amount: 900, Reason: "daily_warehouse_rent", CreatedAt: clock.RealUTC.Add(-time.Hour)},
		{ID: "latest-rent", Debit: account, Credit: "warehouse", Amount: 500, Reason: "daily_warehouse_rent", CreatedAt: clock.RealUTC.Add(-time.Minute)},
		{ID: "research", Debit: account, Credit: "research", Amount: 300, Reason: "research_upkeep", CreatedAt: clock.RealUTC.Add(-time.Minute)},
		{ID: "not-ours", Debit: "another-company", Credit: "warehouse", Amount: 1_000_000, Reason: "daily_warehouse_rent", CreatedAt: clock.RealUTC.Add(-time.Minute)},
		{ID: "future-rent", Debit: account, Credit: "warehouse", Amount: 2_000_000, Reason: "daily_warehouse_rent", CreatedAt: clock.RealUTC.Add(time.Minute)},
	})
	upkeep := DailyUpkeep(runner.stateCache.Ships, runner.stateCache.Catalog, len(runner.stateCache.Factors))
	// One midnight occurs in 31 seconds at 60x, whereas the target's earlier
	// 29-second deadline would precede every scheduled daily collection.
	if got, want := loanBridgeTestNeed(t, world, runner, loan), int64(240_000)+upkeep+500+300; got != want {
		t.Fatalf("wrong due-bound hard need: got=%d want=%d", got, want)
	}
	loan.NextPaymentDueAt = new(clock.RealUTC.Add(29 * time.Second))
	if got := loanBridgeTestNeed(t, world, runner, loan); got != 240_000 {
		t.Fatalf("later daily collections entered the earlier deadline: got=%d want=240000", got)
	}
	loan.ArrearsPence = 300_000
	if got := loanBridgeTestNeed(t, world, runner, loan); got != 340_000 {
		t.Fatalf("arrears ignored collections before the grace deadline or charged future bills: got=%d want=340000", got)
	}
}

func TestLoanBridgeLaterCandidateRefusalPreservesBestQuote(t *testing.T) {
	for _, status := range []int{http.StatusBadRequest, http.StatusConflict} {
		t.Run(fmt.Sprint(status), func(t *testing.T) {
			world, runner := loanBridgeFixture(t, 100_000)
			world.h.api.mu.Lock()
			world.h.api.fleet[0]["ship"].(map[string]any)["cannons_count"] = 0
			world.h.api.fleet[1]["ship"].(map[string]any)["cannons_count"] = 8
			world.h.api.mu.Unlock()
			world.mu.Lock()
			world.quoteProblems["north_sea|"+recoveryShipCharizard] = loanBridgeQuoteProblem{Status: status, Code: "bad_request", Detail: "the later candidate is unavailable"}
			world.mu.Unlock()
			loanCapacityRefresh(t, world.h, runner)
			world.cash(t, runner, nil)
			mission, data := loanBridgeStage(t, world, runner, "bridge-target", 100_000, recoveryShipBlastoise)
			requests := world.requests()
			if len(requests) != 2 || requests[0].ShipID != recoveryShipBlastoise || requests[1].ShipID != recoveryShipCharizard {
				t.Fatalf("later refusal discarded or replaced an earlier valid quote: %+v", requests)
			}
			id := data.Action.ID
			if changed, err := runner.dispatch(world.h.ctx, mission, &data); err != nil || !changed {
				t.Fatalf("retained best quote did not dispatch: changed=%v err=%v", changed, err)
			}
			if row := loanBridgeJournal(t, world, id); row.State != store.ActionComplete || row.Status != http.StatusOK ||
				world.h.api.mutationCount("/v1/loan/accept-quote") != 1 || len(world.h.pending()) != 0 {
				t.Fatalf("retained best quote did not settle exactly once: %+v", row)
			}
		})
	}
}

func TestLoanBridgeRefusedRegionFallsBackToAnotherVaultRegion(t *testing.T) {
	world, runner := loanBridgeFixture(t, 100_000)
	world.h.api.setRates(
		Rate{FromRegion: "north", ToRegion: "south", FeeBPS: 335},
		Rate{FromRegion: "south", ToRegion: "north", FeeBPS: 335},
	)
	loanCapacityRefresh(t, world.h, runner)
	loan := runner.stateCache.Loans[0]
	world.mu.Lock()
	for _, ship := range []string{recoveryShipBlastoise, recoveryShipCharizard} {
		world.quoteProblems["north_sea|"+ship] = loanBridgeQuoteProblem{Status: http.StatusConflict, Code: "house_capacity", Detail: "this region cannot fund another lien now"}
	}
	world.mu.Unlock()
	mission, data := loanBridgeStageInRegion(t, world, runner, loan.ID, 110_000, recoveryShipCharizard, "channel")
	if data.LoanBridge.FundingPence != 106_315 {
		t.Fatalf("regional bridge did not protect the 335-bps remittance fee: net funding=%d", data.LoanBridge.FundingPence)
	}
	if data.LoanBridge.Before.BankingRegion != "north_sea" {
		t.Fatal("regional fallback changed the original installment identity")
	}
	counts := make(map[string]int)
	for _, request := range world.requests() {
		counts[request.BankingRegion]++
	}
	if counts["north_sea"] != 2 || counts["channel"] != 2 || len(counts) != 2 {
		t.Fatalf("bridge did not exhaust the refused region then compare the alternative's appraisals: %+v", world.requests())
	}
	id := data.Action.ID
	if err := runner.newActionPreflight(world.h.ctx, mission, &data); err != nil {
		t.Fatalf("first-send preflight forced fallback funds back into the refused region: %v", err)
	}
	if changed, err := runner.dispatch(world.h.ctx, mission, &data); err != nil || !changed {
		t.Fatalf("alternative-region bridge did not dispatch: changed=%v err=%v", changed, err)
	}
	if row := loanBridgeJournal(t, world, id); row.State != store.ActionComplete || row.Status != http.StatusOK || len(world.h.pending()) != 0 {
		t.Fatalf("alternative-region bridge did not settle its exact journal: %+v", row)
	}
	world.mu.Lock()
	funded := world.loans[len(world.loans)-1]
	world.mu.Unlock()
	if funded.BankingRegion != "channel" || funded.PrincipalPence != 110_000 {
		t.Fatalf("alternative quote did not fund its own real vault: %+v", funded)
	}
	loanBridgeTestProgress(t, world, runner, loan, 100_000, 106_315, false)
}

func TestLoanBridgeParsedCapRetriesExactPrincipalAboveMinimum(t *testing.T) {
	for _, status := range []int{http.StatusBadRequest, http.StatusConflict} {
		t.Run(fmt.Sprint(status), func(t *testing.T) {
			world, runner := loanBridgeFixture(t, 139_001)
			world.mu.Lock()
			world.caps[recoveryShipBlastoise], world.caps[recoveryShipCharizard] = 139_999, 139_999
			world.mu.Unlock()
			if status == http.StatusConflict {
				base := world.h.api.server.Config.Handler
				world.h.api.server.Config.Handler = http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
					if request.Method == http.MethodGet && request.URL.Path == "/v1/loan/quote" && request.URL.Query().Get("principal_pence") == "140000" {
						// Record the exact real quote attempt, but model a
						// capacity race reported as 409 rather than 400.
						base.ServeHTTP(httptest.NewRecorder(), request)
						world.h.api.writeProblem(writer, status, "bad_request", "Goldsmiths lends at most 139999p against this ship now")
						return
					}
					base.ServeHTTP(writer, request)
				})
			}
			mission, data := loanBridgeStage(t, world, runner, "bridge-target", 139_999, recoveryShipCharizard)
			byRegion := map[string]map[string][]int64{"north_sea": {}, "channel": {}}
			for _, request := range world.requests() {
				byRegion[request.BankingRegion][request.ShipID] = append(byRegion[request.BankingRegion][request.ShipID], request.PrincipalPence)
			}
			for _, ship := range []string{recoveryShipBlastoise, recoveryShipCharizard} {
				// Another house is tried at the full principal before any
				// parsed capacity reduces the request for the original region.
				north, channel := byRegion["north_sea"][ship], byRegion["channel"][ship]
				if len(north) != 2 || north[0] != 140_000 || north[1] != 139_999 || len(channel) != 1 || channel[0] != 140_000 {
					t.Fatalf("parsed capacity did not retry the same candidate at its exact viable limit: ship=%s requests=%+v", ship, world.requests())
				}
			}
			if len(world.requests()) != 6 {
				t.Fatalf("parsed-cap bridge made unexpected extra quote requests: %+v", world.requests())
			}
			id, loan := data.Action.ID, data.LoanBridge.Before
			if err := runner.newActionPreflight(world.h.ctx, mission, &data); err != nil {
				t.Fatalf("exact parsed-cap principal was vetoed for not being a 100-Cr multiple: %v", err)
			}
			if changed, err := runner.dispatch(world.h.ctx, mission, &data); err != nil || !changed {
				t.Fatalf("parsed-cap bridge did not dispatch: changed=%v err=%v", changed, err)
			}
			if row := loanBridgeJournal(t, world, id); row.State != store.ActionComplete || row.Status != http.StatusOK || len(world.h.pending()) != 0 {
				t.Fatalf("parsed-cap acceptance did not settle its immutable journal: %+v", row)
			}
			loanBridgeTestProgress(t, world, runner, loan, 139_001, 139_999, false)
		})
	}
}

func TestLoanBridgeQuoteShortRetrySurvivesRestartAndExpires(t *testing.T) {
	for _, test := range []struct {
		status int
		code   string
	}{
		{http.StatusBadRequest, "bad_request"},
		{http.StatusConflict, "house_capacity"},
		{http.StatusForbidden, "house_unavailable"},
		{http.StatusTooManyRequests, "rate_limited"},
		{http.StatusServiceUnavailable, "internal_error"},
	} {
		t.Run(test.code, func(t *testing.T) {
			world, runner := loanBridgeFixture(t, 100_000)
			loan := runner.stateCache.Loans[0]
			world.mu.Lock()
			world.quoteStatus, world.quoteCode = test.status, test.code
			world.mu.Unlock()
			started := time.Now().UTC()
			if changed, err := runner.serviceLoans(world.h.ctx); err != nil || changed {
				t.Fatalf("retryable quote fault advanced or failed service: changed=%v err=%v", changed, err)
			}
			ended := time.Now().UTC()
			world.h.missingMission("__loans")
			loanBridgeNoMarker(t, world, runner, loan)
			deadline := loanBridgeRetryAt(t, world, runner, loan)
			if deadline.Before(started.Add(15*time.Second)) || deadline.After(ended.Add(15*time.Second)) {
				t.Fatalf("retryable quote fault did not persist a 15-second deadline: start=%s end=%s retry=%s", started, ended, deadline)
			}
			attempts := len(world.requests())
			if attempts == 0 {
				t.Fatal("quote fault was not exercised")
			}
			world.mu.Lock()
			world.quoteStatus, world.quoteCode = 0, ""
			world.mu.Unlock()
			runner = world.h.runner(primaryTestKey)
			loanCapacityRefresh(t, world.h, runner)
			if changed, err := runner.serviceLoans(world.h.ctx); err != nil || changed || len(world.requests()) != attempts {
				t.Fatalf("restart ignored the short retry deadline: changed=%v err=%v requests=%+v", changed, err, world.requests())
			}
			if got := loanBridgeRetryAt(t, world, runner, loan); !got.Equal(deadline) {
				t.Fatal("restart extended the persisted retry instead of honoring its expiry")
			}
			loanBridgeExpireRetry(t, world, runner, loan)
			runner = world.h.runner(primaryTestKey)
			loanCapacityRefresh(t, world.h, runner)
			loanBridgeStage(t, world, runner, loan.ID, 100_000, recoveryShipCharizard)
			if len(world.requests()) != attempts+2 || world.h.api.mutationCount("/v1/loan/accept-quote") != 0 || len(world.h.pending()) != 0 {
				t.Fatal("expired retry did not stage fresh quotes without sending or orphaning an action")
			}
		})
	}
}

func TestLoanBridgeAcceptRefusalRetriesFreshQuoteAfterShortDeadline(t *testing.T) {
	for _, test := range []struct {
		status int
		code   string
	}{
		{http.StatusBadRequest, "bad_request"},
		{http.StatusConflict, "quote_expired"},
		{http.StatusTooManyRequests, "rate_limited"},
	} {
		t.Run(test.code, func(t *testing.T) {
			world, runner := loanBridgeFixture(t, 100_000)
			mission, data := loanBridgeStage(t, world, runner, "bridge-target", 100_000, recoveryShipCharizard)
			oldID, oldBody, loan := data.Action.ID, bytes.Clone(data.Action.Body), data.LoanBridge.Before
			world.mu.Lock()
			world.acceptStatus, world.acceptCode = test.status, test.code
			world.mu.Unlock()
			started := time.Now().UTC()
			if _, err := runner.dispatch(world.h.ctx, mission, &data); err != nil {
				t.Fatalf("retryable bridge refusal escaped dispatch: %v", err)
			}
			ended := time.Now().UTC()
			world.h.missingMission("__loans")
			loanBridgeNoMarker(t, world, runner, loan)
			row := loanBridgeJournal(t, world, oldID)
			exact, err := world.h.db.BeginAction(world.h.ctx, &store.Action{ID: oldID, AccountID: world.h.account.ID, Method: http.MethodPost, Path: "/v1/loan/accept-quote", Body: oldBody})
			if err != nil || row.State != store.ActionComplete || row.Status != test.status || !bytes.Equal(exact.Body, oldBody) || len(world.h.pending()) != 0 {
				t.Fatalf("known refusal failed to settle its exact original journal: row=%+v err=%v", row, err)
			}
			deadline := loanBridgeRetryAt(t, world, runner, loan)
			if deadline.Before(started.Add(15*time.Second)) || deadline.After(ended.Add(15*time.Second)) {
				t.Fatalf("accept refusal did not schedule a 15-second fresh-quote retry: %s", deadline)
			}
			world.mu.Lock()
			world.acceptStatus, world.acceptCode = 0, ""
			world.mu.Unlock()
			runner = world.h.runner(primaryTestKey)
			loanCapacityRefresh(t, world.h, runner)
			if changed, err := runner.serviceLoans(world.h.ctx); err != nil || changed || len(world.requests()) != 2 {
				t.Fatalf("accept refusal lost its persisted short retry across restart: changed=%v err=%v", changed, err)
			}
			loanBridgeExpireRetry(t, world, runner, loan)
			runner = world.h.runner(primaryTestKey)
			loanCapacityRefresh(t, world.h, runner)
			mission, fresh := loanBridgeStage(t, world, runner, loan.ID, 100_000, recoveryShipCharizard)
			if fresh.Action.ID == oldID || bytes.Equal(fresh.Action.Body, oldBody) || len(world.requests()) != 4 {
				t.Fatal("definitive refusal reused an expired quote or its settled journal identity")
			}
			newID := fresh.Action.ID
			if changed, err := runner.dispatch(world.h.ctx, mission, &fresh); err != nil || !changed {
				t.Fatalf("fresh bridge after refusal did not dispatch: changed=%v err=%v", changed, err)
			}
			if row := loanBridgeJournal(t, world, newID); row.State != store.ActionComplete || row.Status != http.StatusOK ||
				world.h.api.mutationCount("/v1/loan/accept-quote") != 2 || len(world.h.pending()) != 0 {
				t.Fatalf("fresh retry did not settle exactly one new acceptance: %+v", row)
			}
			loanBridgeTestProgress(t, world, runner, loan, 100_000, 100_000, false)
		})
	}
}

func TestLoanBridgeFourActiveInstantLiensBlockFurtherBorrowing(t *testing.T) {
	for _, count := range []int{4, 5} {
		t.Run(fmt.Sprint(count), func(t *testing.T) {
			world, runner := loanBridgeFixture(t, 100_000)
			loan := runner.stateCache.Loans[0]
			loans := []gameapi.Loan{loan}
			for i := range count {
				loans = append(loans, loanBridgeLien(t, world, fmt.Sprintf("outside-fleet-%d", i)))
			}
			world.setLoans(loans...)
			loanCapacityRefresh(t, world.h, runner)
			world.cash(t, runner, nil)
			if changed, err := runner.serviceLoans(world.h.ctx); err != nil || changed {
				t.Fatalf("active instant-loan cap staged or failed loan service: changed=%v err=%v", changed, err)
			}
			world.h.missingMission("__loans")
			loanBridgeNoMarker(t, world, runner, loan)
			loanBridgeDecision(t, world, "four active")
			if len(world.requests()) != 0 || world.h.api.mutationCount("/v1/loan/accept-quote") != 0 || len(world.h.pending()) != 0 {
				t.Fatal("four active instant liens did not block bridge admission before quoting")
			}
		})
	}
}

func TestLoanBridgeInstantLienCapCountsOnlyEligibleOwnDebt(t *testing.T) {
	world, runner := loanBridgeFixture(t, 100_000)
	target := runner.stateCache.Loans[0]
	base := []gameapi.Loan{target}
	for i := range 3 {
		base = append(base, loanBridgeLien(t, world, fmt.Sprintf("counted-%d", i)))
	}
	for _, test := range []struct {
		name  string
		alter func(*gameapi.Loan)
		block bool
	}{
		{name: "fourth instant lien", alter: func(*gameapi.Loan) {}, block: true},
		{name: "another house lender", alter: func(loan *gameapi.Loan) { loan.LenderCompanyID = "other-house" }, block: true},
		{name: "player-request lien", alter: func(loan *gameapi.Loan) { loan.RequestID = new("player-lien-request") }, block: true},
		{name: "another borrower", alter: func(loan *gameapi.Loan) { loan.BorrowerCompanyID = "other-company" }},
		{name: "defaulted lien", alter: func(loan *gameapi.Loan) { loan.Status = "defaulted" }},
		{name: "repaid lien", alter: func(loan *gameapi.Loan) { loan.Status = "repaid" }},
		{name: "mortgage", alter: func(loan *gameapi.Loan) { loan.IsMortgage = true }},
		{name: "unsecured", alter: func(loan *gameapi.Loan) { loan.CollateralKind, loan.CollateralShipID = "unsecured", nil }},
	} {
		t.Run(test.name, func(t *testing.T) {
			fourth := loanBridgeLien(t, world, "fourth-eligibility")
			test.alter(&fourth)
			runner.stateCache.Loans = append(append([]gameapi.Loan{}, base...), fourth)
			if reason := runner.loanBridgeTargetReason(target); (reason != "") != test.block {
				t.Fatalf("wrong lien-cap classification for %s: reason=%q block=%v", test.name, reason, test.block)
			}
		})
	}
}

func TestLoanBridgeTargetsOnlyUnsecuredAmortisedDebt(t *testing.T) {
	for _, test := range []struct {
		name  string
		alter func(*gameapi.Loan)
	}{
		{name: "ship lien bridge", alter: func(loan *gameapi.Loan) {
			loan.CollateralKind, loan.CollateralStatus, loan.CollateralShipID = "ship_lien", "pledged", new(recoveryShipBlastoise)
			loan.QuoteID = new("earlier-bridge-quote")
		}},
		{name: "mortgage", alter: func(loan *gameapi.Loan) {
			loan.IsMortgage, loan.CollateralKind, loan.CollateralStatus, loan.CollateralShipID = true, "ship_lien", "pledged", new(recoveryShipBlastoise)
		}},
		{name: "balloon", alter: func(loan *gameapi.Loan) { loan.Repayment = "balloon" }},
	} {
		t.Run(test.name, func(t *testing.T) {
			world, runner := loanBridgeFixture(t, 100_000)
			loan := runner.stateCache.Loans[0]
			test.alter(&loan)
			world.setLoans(loan)
			loanCapacityRefresh(t, world.h, runner)
			world.cash(t, runner, nil)
			if changed, err := runner.serviceInstallmentBridge(world.h.ctx); err != nil || changed {
				t.Fatalf("ineligible debt entered installment bridging: changed=%v err=%v", changed, err)
			}
			world.h.missingMission("__loans")
			loanBridgeNoMarker(t, world, runner, loan)
			if len(world.requests()) != 0 || world.h.api.mutationCount("/v1/loan/accept-quote") != 0 || len(world.h.pending()) != 0 {
				t.Fatal("bridge of a lien, mortgage or balloon reached Goldsmiths")
			}
		})
	}
}

func TestLoanBridgePartialFundingBorrowsOnlyOriginalNeedRemainder(t *testing.T) {
	world, runner := loanBridgeFixture(t, 950_001)
	loan := runner.stateCache.Loans[0]
	world.mu.Lock()
	world.caps[recoveryShipCharizard], world.caps[recoveryShipBlastoise] = 750_000, 300_000
	world.mu.Unlock()
	mission, first := loanBridgeStage(t, world, runner, loan.ID, 750_000, recoveryShipCharizard)
	firstID := first.Action.ID
	loanBridgeTestProgress(t, world, runner, loan, 950_001, 0, true)
	if changed, err := runner.dispatch(world.h.ctx, mission, &first); err != nil || !changed {
		t.Fatalf("partial first bridge did not dispatch: changed=%v err=%v", changed, err)
	}
	loanBridgeTestProgress(t, world, runner, loan, 950_001, 750_000, false)
	runner = world.h.runner(primaryTestKey)
	loanCapacityRefresh(t, world.h, runner)
	if changed, err := runner.serviceLoans(world.h.ctx); err != nil || !changed {
		t.Fatalf("completed partial bridge did not retire before planning the remainder: changed=%v err=%v", changed, err)
	}
	world.h.missingMission("__loans")
	if need := loanBridgeTestNeed(t, world, runner, loan); need != 200_001 {
		t.Fatalf("partial proceeds were not applied once to the original hard need: %d", need)
	}
	mission, second := loanBridgeStage(t, world, runner, loan.ID, 210_000, recoveryShipBlastoise)
	secondID := second.Action.ID
	if firstID == secondID {
		t.Fatal("remaining need reused the first bridge's completed journal identity")
	}
	loanBridgeTestProgress(t, world, runner, loan, 950_001, 750_000, true)
	if changed, err := runner.dispatch(world.h.ctx, mission, &second); err != nil || !changed {
		t.Fatalf("remaining original need did not dispatch: changed=%v err=%v", changed, err)
	}
	loanBridgeTestProgress(t, world, runner, loan, 950_001, 960_000, false)
	for _, id := range []string{firstID, secondID} {
		if row := loanBridgeJournal(t, world, id); row.State != store.ActionComplete || row.Status != http.StatusOK {
			t.Fatalf("partial-bridge journal did not settle exactly: %+v", row)
		}
	}
	// Both lien installments are now inside the window, but neither may
	// become a bridge target. Draining proceeds must not reset original need.
	world.mu.Lock()
	for i := 1; i < len(world.loans); i++ {
		world.loans[i].NextPaymentDueAt = new(world.now.Add(30 * time.Minute))
	}
	world.mu.Unlock()
	loanCapacityRefresh(t, world.h, runner)
	world.cash(t, runner, nil)
	quotes := len(world.requests())
	for range 3 {
		runner = world.h.runner(primaryTestKey)
		loanCapacityRefresh(t, world.h, runner)
		if _, err := runner.serviceLoans(world.h.ctx); err != nil {
			t.Fatal(err)
		}
	}
	world.h.missingMission("__loans")
	if len(world.requests()) != quotes || world.h.api.mutationCount("/v1/loan/accept-quote") != 2 || len(world.h.pending()) != 0 {
		t.Fatal("covered original need or a bridge lien borrowed again after restart")
	}
	loanBridgeTestProgress(t, world, runner, loan, 950_001, 960_000, false)
}

func TestLoanBridgeMissionPersistsBeforeMarkerAndRestartPreflightHealsIt(t *testing.T) {
	world, runner := loanBridgeFixture(t, 100_000)
	loan := runner.stateCache.Loans[0]
	admin, err := sql.Open("pgx", world.h.dsn)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = admin.Close() })
	var schema string
	if err := admin.QueryRowContext(world.h.ctx, "SELECT current_schema()").Scan(&schema); err != nil || schema != world.h.schema {
		t.Fatalf("bridge fault escaped its disposable schema: schema=%q err=%v", schema, err)
	}
	_, err = admin.ExecContext(world.h.ctx, `
CREATE FUNCTION reject_bridge_marker_save() RETURNS trigger LANGUAGE plpgsql AS $$
BEGIN
    RAISE EXCEPTION 'injected bridge marker persistence failure';
END;
$$;
CREATE TRIGGER reject_bridge_marker_save BEFORE INSERT OR UPDATE ON snapshots
    FOR EACH ROW EXECUTE FUNCTION reject_bridge_marker_save();`)
	if err != nil {
		t.Fatal(err)
	}
	if changed, err := runner.serviceLoans(world.h.ctx); err == nil || !strings.Contains(err.Error(), "injected bridge marker persistence failure") {
		t.Fatalf("marker storage fault was not reported after staging: changed=%v err=%v", changed, err)
	}
	mission, staged := world.h.mission("__loans")
	if mission.Phase != "loan_bridge" || staged.LoanBridge == nil || staged.Action == nil || staged.Action.Stage != "loan-accept" {
		t.Fatalf("marker persistence preceded the durable sendable mission: mission=%+v data=%+v", mission, staged)
	}
	action, generation, intent := *staged.Action, mission.Generation, bytes.Clone(encode(staged.LoanBridge))
	snapshot, err := world.h.db.Snapshot(world.h.ctx, world.h.account.ID)
	if err != nil {
		t.Fatal(err)
	}
	if _, exists := decode[map[string]json.RawMessage](snapshot, "_loan_bridge_due")[loanRefinanceDueKey(loan)]; exists {
		t.Fatal("injected failed marker write nevertheless became durable")
	}
	if exists, err := world.h.db.HasAction(world.h.ctx, world.h.account.ID, action.ID); err != nil || exists || len(world.h.pending()) != 0 || world.h.api.mutationCount("/v1/loan/accept-quote") != 0 {
		t.Fatalf("marker persistence fault journaled or sent an unprotected bridge: exists=%v err=%v", exists, err)
	}
	if _, err := admin.ExecContext(world.h.ctx, "DROP TRIGGER reject_bridge_marker_save ON snapshots"); err != nil {
		t.Fatal(err)
	}
	runner = world.h.runner(primaryTestKey)
	loanCapacityRefresh(t, world.h, runner)
	mission, data := world.h.mission("__loans")
	quotes := len(world.requests())
	// The process fence is repaired in its own dispatch pass, without
	// inserting a journal row or replacing any immutable request field.
	if changed, err := runner.dispatch(world.h.ctx, mission, &data); err != nil || !changed {
		t.Fatalf("restart did not restamp the durable never-sent mission: changed=%v err=%v", changed, err)
	}
	mission, data = world.h.mission("__loans")
	if err := runner.newActionPreflight(world.h.ctx, mission, &data); err != nil {
		t.Fatalf("first-send preflight did not heal the absent due marker: %v", err)
	}
	loanBridgeTestProgress(t, world, runner, loan, 100_000, 0, true)
	restamped := *data.Action
	restamped.FinancialSession, restamped.FinancialEpoch, restamped.ControlEpoch = action.FinancialSession, action.FinancialEpoch, action.ControlEpoch
	if mission.Generation != generation || !bytes.Equal(encode(data.LoanBridge), intent) || !bytes.Equal(encode(restamped), encode(action)) || len(world.requests()) != quotes {
		t.Fatal("marker healing replaced the exact staged mission, quote, request or original creation time")
	}
	if exists, err := world.h.db.HasAction(world.h.ctx, world.h.account.ID, action.ID); err != nil || exists {
		t.Fatalf("marker healing inserted the journal before dispatch: exists=%v err=%v", exists, err)
	}
	if changed, err := runner.dispatch(world.h.ctx, mission, &data); err != nil || !changed {
		t.Fatalf("healed durable bridge did not dispatch: changed=%v err=%v", changed, err)
	}
	exact, err := world.h.db.BeginAction(world.h.ctx, &store.Action{ID: action.ID, AccountID: world.h.account.ID, Method: action.Method, Path: action.Path, Body: action.Body})
	row := loanBridgeJournal(t, world, action.ID)
	if err != nil || row.State != store.ActionComplete || row.Status != http.StatusOK || !bytes.Equal(exact.Body, action.Body) ||
		world.h.api.mutationCount("/v1/loan/accept-quote") != 1 || len(world.h.pending()) != 0 {
		t.Fatalf("marker-healed bridge did not settle its exact original journal once: row=%+v err=%v", row, err)
	}
	loanBridgeTestProgress(t, world, runner, loan, 100_000, 100_000, false)
}

func TestLoanBridgeAffordableArrearsCureIgnoresRegionalFloors(t *testing.T) {
	world, runner := loanBridgeFixture(t, 100_000)
	loan := runner.stateCache.Loans[0]
	loan.ArrearsPence, loan.NextPaymentPence = 25_000, new(int64(25_000))
	loan.NextPaymentDueAt = new(world.now.Add(10 * time.Second))
	world.setLoans(loan)
	loanCapacityRefresh(t, world.h, runner)
	world.cash(t, runner, map[string]int64{"north_sea": 25_000})
	required, err := runner.loanServiceReserves(world.h.ctx, loan.ID)
	if err != nil || required[loan.BankingRegion]+loan.ArrearsPence <= 25_000 {
		t.Fatalf("fixture did not leave the old floor-inclusive cure target unaffordable: required=%+v err=%v", required, err)
	}
	base := world.h.api.server.Config.Handler
	world.h.api.server.Config.Handler = http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		if request.Method != http.MethodPost || request.URL.Path != "/v1/loan/repay" {
			base.ServeHTTP(writer, request)
			return
		}
		body, err := io.ReadAll(request.Body)
		if err != nil {
			t.Error(err)
			writer.WriteHeader(http.StatusBadRequest)
			return
		}
		mission, err := world.h.db.GetMission(request.Context(), world.h.account.ID, "__loans")
		var data missionData
		var payment gameapi.RepayLoanRequest
		if err != nil || mission == nil || mission.Phase != "loan_repay" || json.Unmarshal(mission.Data, &data) != nil ||
			data.Action == nil || data.LoanRepayment == nil || data.LoanBridge != nil ||
			data.Action.ID != request.Header.Get("Idempotency-Key") || !bytes.Equal(data.Action.Body, body) ||
			json.Unmarshal(body, &payment) != nil || payment.LoanID != loan.ID || payment.AmountPence == nil || *payment.AmountPence != 25_000 {
			t.Error("affordable cure reached the wire without its exact durable arrears intent")
			writer.WriteHeader(http.StatusConflict)
			return
		}
		if exists, err := world.h.db.HasAction(request.Context(), world.h.account.ID, data.Action.ID); err != nil || !exists {
			t.Error("affordable cure reached the wire before journal insertion")
			writer.WriteHeader(http.StatusConflict)
			return
		}
		paid := loan
		paid.ArrearsPence = 0
		paid.OutstandingPrincipalPence -= 25_000
		paid.OwedPence -= 25_000
		paid.PrincipalRepaidPence += 25_000
		world.setLoans(paid)
		world.h.api.mu.Lock()
		world.h.api.vaultBalance["north"] -= 25_000
		world.h.api.record(request, body, http.StatusOK)
		world.h.api.mu.Unlock()
		world.h.api.writeJSON(writer, http.StatusOK, paid)
	})
	if changed, err := runner.serviceLoans(world.h.ctx); err != nil || !changed {
		t.Fatalf("fundable arrears froze below regional floors: changed=%v err=%v", changed, err)
	}
	mission, data := world.h.mission("__loans")
	calls := world.h.api.mutationsFor("/v1/loan/repay")
	exactBody := encode(gameapi.RepayLoanRequest{LoanID: loan.ID, AmountPence: new(int64(25_000))})
	if len(calls) != 1 || calls[0].Status != http.StatusOK || !bytes.Equal(calls[0].Body, exactBody) {
		t.Fatalf("affordable arrears cure changed its exact wire request: %+v", calls)
	}
	actionID := calls[0].IdempotencyKey
	row := loanBridgeJournal(t, world, actionID)
	exact, err := world.h.db.BeginAction(world.h.ctx, &store.Action{ID: actionID, AccountID: world.h.account.ID, Method: http.MethodPost, Path: "/v1/loan/repay", Body: exactBody})
	if err != nil || row.State != store.ActionComplete || row.Status != http.StatusOK || !bytes.Equal(exact.Body, exactBody) ||
		mission.Phase != "done" || data.Action != nil || data.LastActionID != actionID || len(world.h.pending()) != 0 {
		t.Fatalf("affordable arrears cure did not settle its exact durable repayment: row=%+v mission=%+v err=%v", row, mission, err)
	}
	if len(world.requests()) != 0 || world.h.api.mutationCount("/v1/loan/accept-quote") != 0 ||
		world.h.api.mutationCount("/v1/loan/repay") != 1 || world.h.api.mutationCount("/v1/treasury/remit") != 0 {
		t.Fatal("affordable local arrears cure borrowed, remitted, or repeated repayment")
	}
	loanBridgeNoMarker(t, world, runner, loan)
	history, err := world.h.db.History(world.h.ctx, world.h.account.ID, "decisions", store.MaxHistoryLimit)
	if err != nil {
		t.Fatal(err)
	}
	for _, decision := range history.([]store.Decision) {
		if decision.ShipID == "__loans" && (decision.Kind == "hold" || decision.Kind == "loan_bridge") {
			t.Fatalf("fundable arrears cure left an idle hold or bridge decision: %+v", decision)
		}
	}
}

func TestLoanBridgeLegacyBooleanRecomputesCurrentCash(t *testing.T) {
	for _, funded := range []bool{false, true} {
		t.Run(fmt.Sprintf("funded=%t", funded), func(t *testing.T) {
			world, runner := loanBridgeFixture(t, 100_000)
			loan := runner.stateCache.Loans[0]
			runner.snapshot["_loan_bridge_due"] = encode(map[string]bool{loanRefinanceDueKey(loan): true})
			if err := world.h.db.SaveSnapshot(world.h.ctx, world.h.account.ID, runner.snapshot); err != nil {
				t.Fatal(err)
			}
			if funded {
				world.cash(t, runner, map[string]int64{loan.BankingRegion: *loan.NextPaymentPence})
			}
			runner = world.h.runner(primaryTestKey)
			loanCapacityRefresh(t, world.h, runner)
			if len(runner.loanBridgeDates()) != 0 {
				t.Fatal("legacy boolean became a consumed or pending bridge budget")
			}
			if !funded {
				loanBridgeStage(t, world, runner, loan.ID, 100_000, recoveryShipCharizard)
				return
			}
			if changed, err := runner.serviceLoans(world.h.ctx); err != nil || changed {
				t.Fatalf("current cash already covers the installment: changed=%v err=%v", changed, err)
			}
			world.h.missingMission("__loans")
			if len(world.requests()) != 0 || world.h.api.mutationCount("/v1/loan/accept-quote") != 0 {
				t.Fatal("legacy marker migration borrowed despite landed cash")
			}
		})
	}
}

func TestLoanBridgeNeedNetsForeignVaultAtCheapestDirectedFee(t *testing.T) {
	world, runner := loanBridgeFixture(t, 200_000)
	loan := runner.stateCache.Loans[0]
	loan.NextPaymentDueAt = new(world.now.Add(10 * time.Second))
	runner.stateCache.Loans = []gameapi.Loan{loan}
	runner.stateCache.Vaults = []Vault{
		{AccountID: "fee-local", AccountType: "vault", BankingRegion: "north_sea", BalancePence: 10_000},
		{AccountID: "fee-foreign", AccountType: "vault", BankingRegion: "channel", BalancePence: 100_000},
	}
	runner.stateCache.Rates = []Rate{
		{FromRegion: "channel", ToRegion: "north_sea", FeeBPS: 500},
		{FromRegion: "channel", ToRegion: "north_sea", FeeBPS: 335},
		{FromRegion: "north_sea", ToRegion: "channel", FeeBPS: 900},
	}
	if got := loanBridgeTestNeed(t, world, runner, loan); got != 93_350 {
		t.Fatalf("foreign vault did not retain its cheapest directed remittance fee: need=%d want=93350", got)
	}
	runner.stateCache.Rates = runner.stateCache.Rates[2:]
	if got := loanBridgeTestNeed(t, world, runner, loan); got != 190_000 {
		t.Fatalf("unroutable foreign cash was counted as spendable local funding: need=%d", got)
	}
}

func TestLoanBridgeActionlessAndAuthorizationRejectedMissionsReleasePendingDue(t *testing.T) {
	for _, status := range []int{0, http.StatusUnauthorized, http.StatusForbidden} {
		t.Run(fmt.Sprint(status), func(t *testing.T) {
			world, runner := loanBridgeFixture(t, 100_000)
			mission, data := loanBridgeStage(t, world, runner, "bridge-target", 100_000, recoveryShipCharizard)
			oldID, loan := data.Action.ID, data.LoanBridge.Before
			if status == 0 {
				data.Action = nil
				world.h.putMission("__loans", mission.Generation, mission.Phase, data)
			} else {
				world.mu.Lock()
				world.acceptStatus, world.acceptCode = status, "authorization_required"
				world.mu.Unlock()
				if _, err := runner.dispatch(world.h.ctx, mission, &data); !errors.Is(err, ErrExecutionStopped) {
					t.Fatalf("authorization refusal did not stop after its definite journal outcome: %v", err)
				}
				row := loanBridgeJournal(t, world, oldID)
				if row.State != store.ActionComplete || row.Status != status || len(world.h.pending()) != 0 {
					t.Fatalf("authorization refusal lost its definite journal result: %+v", row)
				}
			}
			world.mu.Lock()
			world.acceptStatus, world.acceptCode = 0, ""
			world.mu.Unlock()
			runner = world.h.runner(primaryTestKey)
			loanCapacityRefresh(t, world.h, runner)
			retired, err := world.h.db.GetMission(world.h.ctx, world.h.account.ID, "__loans")
			if err != nil {
				t.Fatal(err)
			}
			if retired != nil {
				if changed, err := runner.serviceLoans(world.h.ctx); err != nil || !changed {
					t.Fatalf("restart did not retire actionless bridge control: changed=%v err=%v", changed, err)
				}
				world.h.missingMission("__loans")
			}
			snapshot, err := world.h.db.Snapshot(world.h.ctx, world.h.account.ID)
			if err != nil {
				t.Fatal(err)
			}
			if decode[map[string]loanBridgeTestDue](snapshot, "_loan_bridge_due")[loanRefinanceDueKey(loan)].Pending {
				t.Fatal("retiring actionless bridge control left the installment permanently pending")
			}
			if !decode[map[string]time.Time](runner.snapshot, "_loan_bridge_retry_at")[loanRefinanceDueKey(loan)].IsZero() {
				loanBridgeExpireRetry(t, world, runner, loan)
			}
			if status == 0 {
				if exists, err := world.h.db.HasAction(world.h.ctx, world.h.account.ID, oldID); err != nil || exists {
					t.Fatalf("retiring a proven unsent action invented a journal: exists=%v err=%v", exists, err)
				}
			}
			mission, fresh := loanBridgeStage(t, world, runner, loan.ID, 100_000, recoveryShipCharizard)
			if fresh.Action.ID == oldID {
				t.Fatal("fresh bridge reused the retired or authorization-rejected action identity")
			}
			freshID := fresh.Action.ID
			if changed, err := runner.dispatch(world.h.ctx, mission, &fresh); err != nil || !changed {
				t.Fatalf("cleared pending installment did not accept a fresh quote: changed=%v err=%v", changed, err)
			}
			if row := loanBridgeJournal(t, world, freshID); row.State != store.ActionComplete || row.Status != http.StatusOK || len(world.h.pending()) != 0 {
				t.Fatalf("fresh bridge did not settle after retiring actionless control: %+v", row)
			}
			loanBridgeTestProgress(t, world, runner, loan, 100_000, 100_000, false)
		})
	}
}
