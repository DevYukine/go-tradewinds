package engine

import (
	"encoding/json"
	"math"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/DevYukine/go-tradewinds/internal/store"
)

const krakenAcceptanceCompany = "01a16469-7564-711d-b8da-e69d7d73e623"

func playerLoanAcceptanceFixture(t *testing.T) (*recoveryHarness, *Runner) {
	t.Helper()
	h, runner := loanCapacityFixture(t, 750_000, false)
	delete(runner.snapshot, "_player_loan_fallback")
	overseasMetric(t, h, 10_000_000, time.Now().UTC())
	base := h.api.server.Config.Handler
	h.api.server.Config.Handler = http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		if req.URL.Path == "/v1/loan/list" || req.URL.Path == "/v1/loan/repay" {
			recorder := httptest.NewRecorder()
			h.api.handle(recorder, req)
			for key, values := range recorder.Header() {
				w.Header()[key] = values
			}
			w.WriteHeader(recorder.Code)
			_, _ = w.Write([]byte(strings.ReplaceAll(strings.ReplaceAll(recorder.Body.String(), `"north"`, `"north_sea"`), `"south"`, `"channel"`)))
			return
		}
		base.ServeHTTP(w, req)
	})
	loanCapacityRefresh(t, h, runner)
	loanCapacityCashShortfall(t, h, runner, 5_000)
	return h, runner
}

func playerLoanAcceptanceAdmit(t *testing.T, h *recoveryHarness, runner *Runner) {
	t.Helper()
	port, hull, decision, required, evidence := loanCapacityDecision(t, h, runner)
	if decision.Accepted || !decision.EconomicallyViable {
		t.Fatalf("financing fixture is not a viable cash-short candidate: %+v", decision)
	}
	if changed, err := runner.considerCapacityFinancing(h.ctx, port, hull, decision, required, evidence); err != nil || !changed {
		t.Fatalf("admit player request: changed %t, error %v", changed, err)
	}
}

func playerLoanAcceptanceDrive(t *testing.T, h *recoveryHarness, runner *Runner) {
	t.Helper()
	mission, data := h.mission("__growth")
	if _, err := runner.driveGrowth(h.ctx, mission, &data); err != nil {
		t.Fatalf("drive growth: %v", err)
	}
}

func playerLoanAcceptanceDispatch(t *testing.T, h *recoveryHarness, runner *Runner) {
	t.Helper()
	mission, data := h.mission("__growth")
	if data.Action == nil {
		playerLoanAcceptanceDrive(t, h, runner)
		mission, data = h.mission("__growth")
	}
	if data.Action == nil {
		t.Fatalf("expected a staged financing mutation: phase %s, %+v", mission.Phase, data)
	}
	stage := data.Action.Stage
	if _, err := runner.dispatch(h.ctx, mission, &data); err != nil {
		t.Fatalf("dispatch %s: %v", stage, err)
	}
}

func playerLoanAcceptancePosted(t *testing.T) (*recoveryHarness, *Runner) {
	t.Helper()
	h, runner := playerLoanAcceptanceFixture(t)
	playerLoanAcceptanceAdmit(t, h, runner)
	playerLoanAcceptanceDispatch(t, h, runner)
	loanCapacityRefresh(t, h, runner)
	_, data := h.mission("__growth")
	if data.PlayerLoan == nil || data.PlayerLoan.RequestID == "" {
		t.Fatalf("posted request was not recorded: %+v", data)
	}
	return h, runner
}

func playerLoanAcceptanceDecision(t *testing.T, h *recoveryHarness, verdict string) map[string]any {
	t.Helper()
	rows, err := h.db.History(h.ctx, h.account.ID, "decisions", store.MaxHistoryLimit)
	if err != nil {
		t.Fatal(err)
	}
	for _, row := range rows.([]store.Decision) {
		if !strings.HasPrefix(row.Kind, "capacity_financing:") {
			continue
		}
		var details map[string]any
		if err := json.Unmarshal(row.Details, &details); err != nil {
			t.Fatal(err)
		}
		if details["verdict"] == verdict {
			for _, key := range []string{"source", "principal_pence", "term_game_days", "collateral_kind", "ship_id"} {
				if _, exists := details[key]; !exists {
					t.Fatalf("%s decision omits %s: %s", verdict, key, row.Details)
				}
			}
			return details
		}
	}
	t.Fatalf("no capacity financing decision names gate %q: %+v", verdict, rows)
	return nil
}

func TestPlayerLoanRequestPostsExactYardTermsOnceAcrossRestart(t *testing.T) {
	h, runner := playerLoanAcceptanceFixture(t)
	_, _, candidate, _, _ := loanCapacityDecision(t, h, runner)
	headroom := candidate.CandidateLotCashPence + DailyUpkeep(runner.stateCache.Ships, runner.stateCache.Catalog, len(runner.stateCache.Factors))
	factor := (0.30 / 36) / (1 - math.Pow(1+0.30/36, -36))
	principal := int64(5_000) + headroom
	for principal-2*int64(math.Ceil(float64(principal)*factor)) < 5_000+headroom {
		principal++
	}
	playerLoanAcceptanceAdmit(t, h, runner)
	playerLoanAcceptanceDispatch(t, h, runner)
	loanCapacityRefresh(t, h, runner)
	rows := h.api.mutationsFor("/v1/loan/request")
	if len(rows) != 1 {
		t.Fatalf("request posts = %d, want exactly one", len(rows))
	}
	var body map[string]any
	if err := json.Unmarshal(rows[0].Body, &body); err != nil {
		t.Fatal(err)
	}
	_, data := h.mission("__growth")
	// Independent penny-by-penny closure includes a candidate cargo lot,
	// one fleet game-day upkeep and two rounded-up loan installments.
	want := map[string]any{
		"banking_region": "north_sea", "principal_pence": float64(principal),
		"term_game_days": float64(360), "repayment": "amortised", "collateral_kind": "ship_lien",
		"ship_id": recoveryShipBlastoise, "recourse": true, "auto_quote": true, "open_game_hours": float64(24),
	}
	if len(body) != len(want) {
		t.Fatalf("request contains extra or missing fields: %s", rows[0].Body)
	}
	for key, value := range want {
		if body[key] != value {
			t.Errorf("request %s = %v; want %v", key, body[key], value)
		}
	}
	if data.BuildPort != recoveryHomePort || data.BuildClass != recoveryHull || data.PlayerLoan.RequestID == "" {
		t.Fatalf("request lost the yard, hull or server receipt: %+v", data)
	}
	decision := playerLoanAcceptanceDecision(t, h, "request_posted")
	if decision["source"] != "player_request" {
		t.Fatalf("request decision has wrong source: %+v", decision)
	}
	restarted := h.runner(primaryTestKey)
	loanCapacityRefresh(t, h, restarted)
	for range 3 {
		playerLoanAcceptanceDrive(t, h, restarted)
	}
	if h.api.mutationCount("/v1/loan/request") != 1 || h.api.mutationCount("/v1/loan/accept-quote") != 0 || len(h.pending()) != 0 {
		t.Fatal("restart duplicated the open request, borrowed elsewhere or orphaned its journal action")
	}
}

func TestPlayerLoanRequestOpenOwnRequestPreventsSecondPost(t *testing.T) {
	h, runner := playerLoanAcceptancePosted(t)
	if err := h.db.DeleteMission(h.ctx, h.account.ID, "__growth"); err != nil {
		t.Fatal(err)
	}
	runner = h.runner(primaryTestKey)
	loanCapacityRefresh(t, h, runner)
	port, hull, decision, required, evidence := loanCapacityDecision(t, h, runner)
	if _, err := runner.considerCapacityFinancing(h.ctx, port, hull, decision, required, evidence); err != nil {
		t.Fatal(err)
	}
	if h.api.mutationCount("/v1/loan/request") != 1 || len(h.pending()) != 0 {
		t.Fatal("an authoritative open own request allowed a second mutation")
	}
	playerLoanAcceptanceDecision(t, h, "request_open")
}

func TestPlayerLoanRequestLostResponseReconcilesWithoutResend(t *testing.T) {
	h, runner := playerLoanAcceptanceFixture(t)
	h.api.loanRequestMode = "drop"
	playerLoanAcceptanceAdmit(t, h, runner)
	playerLoanAcceptanceDispatch(t, h, runner)
	if h.api.mutationCount("/v1/loan/request") != 1 {
		t.Fatal("lost-response fixture did not send its request")
	}
	h.api.loanRequestMode = "ok"
	restarted := h.runner(primaryTestKey)
	for range 3 {
		if _, err := h.tick(restarted); err != nil {
			t.Fatal(err)
		}
	}
	_, data := h.mission("__growth")
	if data.PlayerLoan == nil || data.PlayerLoan.RequestID == "" || data.Action != nil || len(h.pending()) != 0 || h.api.mutationCount("/v1/loan/request") != 1 {
		t.Fatalf("restart did not reconcile one accepted server request: %+v", data)
	}
}

func TestPlayerLoanRequestUnquotedLapseAllowsScopedGoldsmithsFallback(t *testing.T) {
	h, runner := playerLoanAcceptancePosted(t)
	createdAt := time.Now().UTC().Add(-49 * time.Hour)
	expiresAt := createdAt.Add(24 * time.Hour)
	mission, data := h.mission("__growth")
	data.PlayerLoan.PostedAt = createdAt.Add(-time.Second)
	data.PlayerLoan.ExpiresAt = expiresAt
	if err := runner.saveMission(h.ctx, mission, &data); err != nil {
		t.Fatal(err)
	}
	h.api.mu.Lock()
	for index := range h.api.loanRequests {
		h.api.loanRequests[index].Status = "lapsed"
		h.api.loanRequests[index].CreatedAt = createdAt
		h.api.loanRequests[index].ExpiresAt = expiresAt
	}
	h.api.mu.Unlock()
	loanCapacityRefresh(t, h, runner)
	beforeDetection := time.Now().UTC()
	playerLoanAcceptanceDrive(t, h, runner)
	afterDetection := time.Now().UTC()
	key := recoveryHomePort + "|" + recoveryHull
	until := decode[map[string]time.Time](runner.snapshot, "_player_loan_backoffs")[key]
	if until.Before(beforeDetection.Add(6*time.Hour)) || until.After(afterDetection.Add(6*time.Hour)) {
		t.Fatalf("lapse backoff = %s, want detection plus six real hours even after a late observation", until)
	}
	fallbackUntil := decode[map[string]time.Time](runner.snapshot, "_player_loan_fallback")[key]
	if !fallbackUntil.Equal(until) {
		t.Fatalf("Goldsmiths fallback ticket = %s, want scoped expiry %s", fallbackUntil, until)
	}
	playerLoanAcceptanceDecision(t, h, "request_expired")
	restarted := h.runner(primaryTestKey)
	loanCapacityRefresh(t, h, restarted)
	port, hull, decision, required, evidence := loanCapacityDecision(t, h, restarted)
	if changed, err := restarted.considerCapacityFinancing(h.ctx, port, hull, decision, required, evidence); err != nil || !changed {
		t.Fatalf("unquoted lapse did not immediately admit Goldsmiths fallback: %t, %v", changed, err)
	}
	_, data = h.mission("__growth")
	if data.Capacity == nil || data.Capacity.Financing == nil || len(data.Capacity.Financing.Quotes) == 0 || data.Capacity.Financing.Quotes[0].RequestID != nil {
		t.Fatalf("lapsed request did not become a Goldsmiths package: %+v", data)
	}
	if h.api.mutationCount("/v1/loan/request") != 1 {
		t.Fatal("Goldsmiths fallback posted another player request")
	}
}

func TestPlayerLoanRequestPledgesHighestRebuildFreeShip(t *testing.T) {
	h, runner := playerLoanAcceptanceFixture(t)
	h.api.mu.Lock()
	second := sailingShipRow(recoveryShipCharizard, "Charizard", recoveryFarPort, recoveryHomePort)
	second["ship"].(map[string]any)["created_at"] = time.Now().UTC().Add(-30 * 24 * time.Hour)
	second["ship"].(map[string]any)["cannons_count"] = int64(4)
	h.api.fleet = append(h.api.fleet, second)
	h.api.mu.Unlock()
	h.api.setVaultBalance("north", 50_000_000)
	h.api.setVaultBalance("south", 500_000)
	loanCapacityRefresh(t, h, runner)
	loanCapacityCashShortfall(t, h, runner, 5_000)
	_, _, candidate, _, _ := loanCapacityDecision(t, h, runner)
	headroom := candidate.CandidateLotCashPence + DailyUpkeep(runner.stateCache.Ships, runner.stateCache.Catalog, len(runner.stateCache.Factors))
	factor := (0.30 / 36) / (1 - math.Pow(1+0.30/36, -36))
	principal := int64(5_000) + headroom
	for principal-2*int64(math.Ceil(float64(principal)*factor)) < 5_000+headroom {
		principal++
	}
	playerLoanAcceptanceAdmit(t, h, runner)
	playerLoanAcceptanceDispatch(t, h, runner)
	rows := h.api.mutationsFor("/v1/loan/request")
	var body map[string]any
	if len(rows) != 1 || json.Unmarshal(rows[0].Body, &body) != nil {
		t.Fatalf("request did not post once: %+v", rows)
	}
	want := map[string]any{
		"banking_region": "north_sea", "principal_pence": float64(principal),
		"term_game_days": float64(360), "repayment": "amortised", "collateral_kind": "ship_lien",
		"ship_id": recoveryShipCharizard, "recourse": true, "auto_quote": true, "open_game_hours": float64(24),
	}
	if len(body) != len(want) {
		t.Fatalf("secured request contains extra or missing fields: %s", rows[0].Body)
	}
	for key, value := range want {
		if body[key] != value {
			t.Errorf("secured request %s = %v; want %v", key, body[key], value)
		}
	}
}

func TestPlayerLoanRequestRequiresPricedFreeCollateralWithinRebuildValue(t *testing.T) {
	for _, test := range []struct {
		name      string
		shortfall int64
		listed    bool
	}{
		{"no priced free ship", 5_000, true},
		{"principal exceeds rebuild value", 100_000, false},
	} {
		t.Run(test.name, func(t *testing.T) {
			h, runner := playerLoanAcceptanceFixture(t)
			if test.listed {
				h.api.mu.Lock()
				h.api.shipListings = []map[string]any{{"id": "listed-collateral-ship", "ship_id": recoveryShipBlastoise, "status": "open"}}
				h.api.mu.Unlock()
			}
			h.api.setVaultBalance("north", 50_000_000)
			h.api.setVaultBalance("south", 500_000)
			loanCapacityRefresh(t, h, runner)
			loanCapacityCashShortfall(t, h, runner, test.shortfall)
			port, hull, decision, required, evidence := loanCapacityDecision(t, h, runner)
			if decision.Accepted || !decision.EconomicallyViable {
				t.Fatalf("collateral refusal fixture lost its viable debt need: %+v", decision)
			}
			if changed, err := runner.considerCapacityFinancing(h.ctx, port, hull, decision, required, evidence); err != nil || changed {
				t.Fatalf("invalid collateral admitted a player request: changed %t, error %v", changed, err)
			}
			h.missingMission("__growth")
			if h.api.mutationCount("/v1/loan/request") != 0 || h.api.mutationCount("/v1/loan/approve") != 0 || len(h.pending()) != 0 {
				t.Fatal("missing or insufficient collateral borrowed unsecured debt or orphaned an action")
			}
			refusal := playerLoanAcceptanceDecision(t, h, "debt_capacity")
			if refusal["source"] != "player_request" {
				t.Fatalf("collateral gate names wrong financing source: %+v", refusal)
			}
		})
	}
}
