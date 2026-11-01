package engine

import (
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"testing"
	"time"

	"github.com/DevYukine/go-tradewinds/internal/gameapi"
	"github.com/DevYukine/go-tradewinds/internal/store"
)

func playerLoanApprovedFixture(t *testing.T) (*recoveryHarness, *Runner) {
	t.Helper()
	h, runner := playerLoanAcceptancePosted(t)
	quote := playerLoanAcceptanceQuote(t, h, runner, 3_000)
	playerLoanAcceptancePublishQuote(t, h, runner, quote)
	playerLoanAcceptanceStageApproval(t, h, runner)
	playerLoanAcceptanceDispatch(t, h, runner)
	loanCapacityRefresh(t, h, runner)
	return h, runner
}

func TestPlayerLoanCompletedApprovalReplaysReceiptThenBuildsOnce(t *testing.T) {
	h, runner := playerLoanAcceptancePosted(t)
	quote := playerLoanAcceptanceQuote(t, h, runner, 3_000)
	playerLoanAcceptancePublishQuote(t, h, runner, quote)
	playerLoanAcceptanceStageApproval(t, h, runner)
	beforeMission, beforeData := h.mission("__growth")
	playerLoanAcceptanceDispatch(t, h, runner)
	// Restore only the mission to the instant before receipt consumption. The
	// actual HTTP 200 and complete immutable journal row remain committed.
	if err := runner.saveMission(h.ctx, beforeMission, &beforeData); err != nil {
		t.Fatal(err)
	}
	restarted := h.runner(primaryTestKey)
	loanCapacityRefresh(t, h, restarted)
	playerLoanAcceptanceDispatch(t, h, restarted)
	_, data := h.mission("__growth")
	if data.Action != nil || len(data.Capacity.Financing.Loans) != 1 || len(restarted.stateCache.Loans) != 1 {
		t.Fatalf("completed approval journal did not restore one receipt: %+v", data)
	}
	playerLoanAcceptanceDrive(t, h, restarted)
	playerLoanAcceptanceDispatch(t, h, restarted)
	loanCapacityRefresh(t, h, restarted)
	_, data = h.mission("__growth")
	if data.BuiltShipID != recoveryBuiltShipID || h.api.mutationCount("/v1/loan/approve") != 1 || h.api.mutationCount("/v1/shipyard/build-ship") != 1 || len(h.pending()) != 0 {
		t.Fatalf("receipt recovery duplicated debt or construction: %+v", data)
	}
}

func TestPlayerLoanExpiredBuildDeadlineRepaysUnusedPrincipalAcrossRestart(t *testing.T) {
	h, runner := playerLoanApprovedFixture(t)
	mission, data := h.mission("__growth")
	data.Capacity.Financing.BuildBy = time.Now().UTC().Add(-time.Second)
	if err := runner.saveMission(h.ctx, mission, &data); err != nil {
		t.Fatal(err)
	}
	restarted := h.runner(primaryTestKey)
	loanCapacityRefresh(t, h, restarted)
	for range 6 {
		mission, data, err := restarted.loadMission(h.ctx, "__growth")
		if err != nil {
			t.Fatal(err)
		}
		if mission == nil {
			break
		}
		if data.Action != nil {
			if _, err := restarted.dispatch(h.ctx, mission, &data); err != nil {
				t.Fatal(err)
			}
		} else if _, err := restarted.driveGrowth(h.ctx, mission, &data); err != nil {
			t.Fatal(err)
		}
		loanCapacityRefresh(t, h, restarted)
	}
	h.missingMission("__growth")
	if h.api.mutationCount("/v1/loan/approve") != 1 || h.api.mutationCount("/v1/loan/repay") != 1 || h.api.mutationCount("/v1/shipyard/build-ship") != 0 || len(h.pending()) != 0 {
		t.Fatal("expired approved intent duplicated debt, built late or failed to repay unused principal")
	}
	var loans []gameapi.Loan
	if err := restarted.api.Get(h.ctx, "/v1/loan/list?role=borrower", &loans); err != nil {
		t.Fatal(err)
	}
	if len(loans) != 1 || loans[0].OwedPence != 0 || loans[0].Status != "repaid" {
		t.Fatalf("unused player debt remains outstanding: %+v", loans)
	}
	playerLoanAcceptanceDecision(t, h, "build_deadline")
}

func TestPlayerLoanMortgageBuildConflictRepaysPrincipalWithoutRetryingBuild(t *testing.T) {
	h, runner := playerLoanAcceptanceFixture(t)
	key := recoveryHomePort + "|" + recoveryHull
	quote := runner.stateCache.Quotes[key]
	quote.Mortgage = mortgageTestTerms()
	runner.stateCache.Quotes[key] = quote
	base := h.api.server.Config.Handler
	h.api.server.Config.Handler = http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		if req.Method == http.MethodGet && req.URL.Path == "/v1/shipyard/quote" && req.URL.Query().Get("port_id") == recoveryHomePort && req.URL.Query().Get("ship_class") == recoveryHull {
			h.api.writeJSON(w, http.StatusOK, quote)
			return
		}
		if req.Method == http.MethodPost && req.URL.Path == "/v1/shipyard/build-ship" {
			body, err := io.ReadAll(req.Body)
			if err != nil {
				t.Error(err)
			}
			var payload struct {
				Mortgage bool `json:"mortgage"`
			}
			if json.Unmarshal(body, &payload) != nil || !payload.Mortgage {
				t.Errorf("stacked financing dropped mortgage:true: %s", body)
			}
			pending, err := h.db.PendingActions(req.Context(), h.account.ID)
			if err != nil || len(pending) != 1 || pending[0].ID != req.Header.Get("Idempotency-Key") {
				t.Error("mortgage build was not durably journaled")
			}
			h.api.mu.Lock()
			h.api.record(req, body, http.StatusConflict)
			h.api.mu.Unlock()
			h.api.writeProblem(w, http.StatusConflict, "mortgage_still_owed", "mortgage still owed")
			return
		}
		base.ServeHTTP(w, req)
	})
	h.api.setVaultBalance("north", 50_000_000)
	h.api.setVaultBalance("south", 500_000)
	loanCapacityRefresh(t, h, runner)
	loanCapacityCashShortfall(t, h, runner, 50_000)
	playerLoanAcceptanceAdmit(t, h, runner)
	playerLoanAcceptanceDispatch(t, h, runner)
	loanCapacityRefresh(t, h, runner)
	playerQuote := playerLoanAcceptanceQuote(t, h, runner, 3_000)
	playerLoanAcceptancePublishQuote(t, h, runner, playerQuote)
	playerLoanAcceptanceStageApproval(t, h, runner)
	playerLoanAcceptanceDispatch(t, h, runner)
	loanCapacityRefresh(t, h, runner)
	restarted := h.runner(primaryTestKey)
	loanCapacityRefresh(t, h, restarted)
	playerLoanAcceptanceDrive(t, h, restarted)
	playerLoanAcceptanceDispatch(t, h, restarted)
	loanCapacityRefresh(t, h, restarted)
	for range 6 {
		mission, data, err := restarted.loadMission(h.ctx, "__growth")
		if err != nil {
			t.Fatal(err)
		}
		if mission == nil {
			break
		}
		data.CooldownUntil = time.Time{}
		if err := restarted.saveMission(h.ctx, mission, &data); err != nil {
			t.Fatal(err)
		}
		if data.Action != nil {
			if _, err := restarted.dispatch(h.ctx, mission, &data); err != nil {
				t.Fatal(err)
			}
		} else if _, err := restarted.driveGrowth(h.ctx, mission, &data); err != nil {
			t.Fatal(err)
		}
		loanCapacityRefresh(t, h, restarted)
	}
	h.missingMission("__growth")
	if h.api.mutationCount("/v1/loan/approve") != 1 || h.api.mutationCount("/v1/shipyard/build-ship") != 1 || h.api.mutationCount("/v1/loan/repay") != 1 || len(h.pending()) != 0 {
		t.Fatal("mortgage conflict orphaned or duplicated a player loan, repayment or build")
	}
	playerLoanAcceptanceDecision(t, h, "mortgage_unavailable")
}

func TestPlayerLoanRequestStagedBeforeCrashSendsExactlyOnce(t *testing.T) {
	h, runner := playerLoanAcceptanceFixture(t)
	playerLoanAcceptanceAdmit(t, h, runner)
	_, staged := h.mission("__growth")
	if staged.Action == nil || len(h.pending()) != 0 {
		t.Fatal("fixture must crash after mission staging but before the first journal/send")
	}
	restarted := h.runner(primaryTestKey)
	loanCapacityRefresh(t, h, restarted)
	for range 3 {
		playerLoanAcceptanceDispatch(t, h, restarted)
		loanCapacityRefresh(t, h, restarted)
		_, current := h.mission("__growth")
		if current.PlayerLoan != nil && current.PlayerLoan.RequestID != "" {
			break
		}
		if time.Now().Before(current.CooldownUntil) {
			t.Fatal("routine restart invalidation introduced a request retry delay")
		}
	}
	_, data := h.mission("__growth")
	if data.PlayerLoan == nil || data.PlayerLoan.RequestID == "" || h.api.mutationCount("/v1/loan/request") != 1 || len(h.pending()) != 0 {
		t.Fatalf("staged restart did not finish exactly one request: %+v", data)
	}
}

func TestPlayerLoanRequestMultipleMatchesAndMissingAuthorityNeverReplay(t *testing.T) {
	for _, missing := range []bool{false, true} {
		t.Run(map[bool]string{false: "duplicate matching requests", true: "missing request authority"}[missing], func(t *testing.T) {
			h, runner := playerLoanAcceptanceFixture(t)
			playerLoanAcceptanceAdmit(t, h, runner)
			mission, data := h.mission("__growth")
			action := data.Action
			if _, err := h.db.BeginAction(h.ctx, &store.Action{ID: action.ID, AccountID: h.account.ID, Method: action.Method, Path: action.Path, Body: action.Body}); err != nil {
				t.Fatal(err)
			}
			if missing {
				delete(runner.meta, "loan_requests")
			} else {
				request := data.PlayerLoan.body()
				now := time.Now().UTC()
				row := map[string]any{
					"id": "request-one", "borrower_company_id": h.account.CompanyID, "banking_region": request.BankingRegion,
					"principal_pence": request.PrincipalPence, "term_game_days": request.TermGameDays, "repayment": request.Repayment,
					"collateral_kind": request.CollateralKind, "collateral_ship_id": request.ShipID,
					"recourse": true, "auto_quote": true, "status": "open", "created_at": now, "expires_at": now.Add(time.Hour),
				}
				var matched gameapi.LoanRequestView
				if err := json.Unmarshal(encode(row), &matched); err != nil {
					t.Fatal(err)
				}
				runner.stateCache.LoanRequests = append(runner.stateCache.LoanRequests, matched)
				matched.ID = "request-two"
				runner.stateCache.LoanRequests = append(runner.stateCache.LoanRequests, matched)
			}
			if _, err := runner.dispatch(h.ctx, mission, &data); err != nil {
				t.Fatal(err)
			}
			if h.api.mutationCount("/v1/loan/request") != 0 || len(h.pending()) != 1 || data.Action == nil {
				t.Fatal("ambiguous or unauthoritative request replay sent another mutation")
			}
		})
	}
}

func TestPlayerLoanPledgeRefusalPersistsBackoffWithoutResending(t *testing.T) {
	h, runner := playerLoanAcceptanceFixture(t)
	h.api.loanRequestMode = "conflict"
	playerLoanAcceptanceAdmit(t, h, runner)
	playerLoanAcceptanceDispatch(t, h, runner)
	h.missingMission("__growth")
	if len(h.pending()) != 0 {
		t.Fatal("definitive pledge refusal retained a resendable request")
	}
	if until := decode[map[string]time.Time](runner.snapshot, "_player_loan_backoffs")[recoveryHomePort+"|"+recoveryHull]; !until.After(time.Now().Add(5 * time.Hour)) {
		t.Fatalf("pledge refusal did not persist six-real-hour backoff: %s", until)
	}
	playerLoanAcceptanceDecision(t, h, "request_refused")
	restarted := h.runner(primaryTestKey)
	loanCapacityRefresh(t, h, restarted)
	if _, err := restarted.considerGrowth(h.ctx); err != nil {
		t.Fatal(err)
	}
	if h.api.mutationCount("/v1/loan/request") != 1 {
		t.Fatal("pledge refusal was reposted after restart")
	}
}

func TestPlayerLoanAuthorizationAndThrottleRetainPendingNeed(t *testing.T) {
	for _, mode := range []string{"unauthorized", "forbidden", "rate_limited"} {
		t.Run(mode, func(t *testing.T) {
			h, runner := playerLoanAcceptanceFixture(t)
			h.api.loanRequestMode = mode
			playerLoanAcceptanceAdmit(t, h, runner)
			mission, data := h.mission("__growth")
			principal := data.PlayerLoan.PrincipalPence
			_, err := runner.dispatch(h.ctx, mission, &data)
			if err != nil && !errors.Is(err, ErrExecutionStopped) {
				t.Fatal(err)
			}
			_, kept := h.mission("__growth")
			if kept.PlayerLoan == nil || kept.PlayerLoan.PrincipalPence != principal || kept.PlayerLoan.RequestID != "" {
				t.Fatalf("temporary request rejection discarded the immutable need: %+v", kept.PlayerLoan)
			}
			if len(decode[map[string]time.Time](runner.snapshot, "_player_loan_backoffs")) != 0 {
				t.Fatal("temporary authorization or throttle response created a domain backoff")
			}
			if h.api.mutationCount("/v1/loan/request") != 1 {
				t.Fatal("temporary response duplicated request mutation")
			}
		})
	}
}

func TestPlayerLoanCompletedRequestRestoresReceiptWithoutReposting(t *testing.T) {
	h, runner := playerLoanAcceptanceFixture(t)
	playerLoanAcceptanceAdmit(t, h, runner)
	mission, before := h.mission("__growth")
	playerLoanAcceptanceDispatch(t, h, runner)
	if err := runner.saveMission(h.ctx, mission, &before); err != nil {
		t.Fatal(err)
	}
	restarted := h.runner(primaryTestKey)
	loanCapacityRefresh(t, h, restarted)
	mission, restored := h.mission("__growth")
	if changed, err := restarted.dispatch(h.ctx, mission, &restored); err != nil || !changed {
		t.Fatalf("completed request receipt was not consumed after restart: %t, %v", changed, err)
	}
	_, restored = h.mission("__growth")
	if restored.PlayerLoan == nil || restored.PlayerLoan.RequestID == "" || restored.Action != nil || len(h.pending()) != 0 {
		t.Fatalf("request receipt recovery orphaned its action or request: %+v", restored)
	}
	if h.api.mutationCount("/v1/loan/request") != 1 || len(h.api.loanRequests) != 1 {
		t.Fatal("completed request was posted twice")
	}
}
