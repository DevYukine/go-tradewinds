package engine

import (
	"context"
	"errors"
	"math"
	"net/http"
	"testing"
	"time"

	"github.com/DevYukine/go-tradewinds/internal/gameapi"
	"github.com/DevYukine/go-tradewinds/internal/store"
	"github.com/google/uuid"
)

func TestPlayerLoanFallbackDoesNotSuppressAnotherCandidate(t *testing.T) {
	h, runner := playerLoanAcceptanceFixture(t)
	if err := runner.playerLoanBackoff(h.ctx, "another-yard", "barque", true, time.Now().UTC()); err != nil {
		t.Fatal(err)
	}
	playerLoanAcceptanceAdmit(t, h, runner)
	playerLoanAcceptanceDispatch(t, h, runner)
	_, data := h.mission("__growth")
	if data.PlayerLoan == nil || data.PlayerLoan.RequestID == "" || h.api.mutationCount("/v1/loan/request") != 1 || h.api.mutationCount("/v1/loan/quote") != 0 {
		t.Fatalf("another candidate's fallback suppressed a fresh request: %+v", data)
	}
}

func TestPlayerLoanNeverSentFundingFailureReleasesIntent(t *testing.T) {
	h, runner := playerLoanAcceptanceFixture(t)
	playerLoanAcceptanceAdmit(t, h, runner)
	mission, data := h.mission("__growth")
	h.api.setVaultBalance("north", 0)
	h.api.setVaultBalance("south", 0)
	loanCapacityRefresh(t, h, runner)
	if err := runner.newActionPreflight(h.ctx, mission, &data); err == nil {
		t.Fatal("funding collapse did not veto the never-sent request")
	}
	if _, err := runner.replanUnsentAction(h.ctx, mission, &data, neverSentPrecondition); err != nil {
		t.Fatal(err)
	}
	restarted := h.runner(primaryTestKey)
	loanCapacityRefresh(t, h, restarted)
	mission, data = h.mission("__growth")
	if changed, err := restarted.driveGrowth(h.ctx, mission, &data); err != nil || changed || !time.Now().Before(data.CooldownUntil) {
		t.Fatalf("funding retirement ignored its persisted retry: %t %v", changed, err)
	}
	h.expireCooldown("__growth")
	mission, data = h.mission("__growth")
	if changed, err := restarted.driveGrowth(h.ctx, mission, &data); err != nil || !changed {
		t.Fatalf("never-sent stale body was not released: %t, %v", changed, err)
	}
	h.missingMission("__growth")
	if len(h.pending()) != 0 || h.api.mutationCount("/v1/loan/request") != 0 {
		t.Fatal("funding failure left a pending action or sent the stale body")
	}
}

func TestPlayerLoanWaitProtectsOnlyOwnPackageCapitalAndTracksAccounting(t *testing.T) {
	h, runner := playerLoanAcceptanceFixture(t)
	playerLoanAcceptanceAdmit(t, h, runner)
	_, data := h.mission("__growth")
	intent := data.PlayerLoan
	var protected int64
	for _, amount := range intent.ProtectedByRegion {
		protected += amount
	}
	for _, amount := range intent.DonorByRegion {
		protected += amount
	}
	if protected != intent.OwnCashPence || protected >= growthCapital(intent.Decision)+intent.Decision.WorkingCapitalPence ||
		data.InitialLedgerIDs == nil || data.StartedGameHours != runner.stateCache.Clock.Calendar.TotalGameHours || !data.CostsKnown {
		t.Fatalf("request wait froze incumbent lots or lost accounting anchors: %+v", data)
	}
	before, err := h.db.SettlementHistory(h.ctx, h.account.ID, "", store.MaxHistoryLimit)
	if err != nil {
		t.Fatal(err)
	}
	h.api.loanRequestMode = "conflict"
	playerLoanAcceptanceDispatch(t, h, runner)
	h.missingMission("__growth")
	after, err := h.db.SettlementHistory(h.ctx, h.account.ID, "", store.MaxHistoryLimit)
	if err != nil || len(after) != len(before) {
		t.Fatalf("refused borrowing invented an aborted trading settlement: %d/%d, %v", len(before), len(after), err)
	}
}

func TestPlayerLoanHeadroomAbsorbsOrdinaryCashDrift(t *testing.T) {
	h, runner := playerLoanAcceptancePosted(t)
	h.api.mu.Lock()
	h.api.vaultBalance["north"] -= 1_000
	h.api.mu.Unlock()
	loanCapacityRefresh(t, h, runner)
	quote := playerLoanAcceptanceQuote(t, h, runner, 3_000)
	playerLoanAcceptancePublishQuote(t, h, runner, quote)
	playerLoanAcceptanceStageApproval(t, h, runner)
	playerLoanAcceptanceDispatch(t, h, runner)
	_, data := h.mission("__growth")
	if h.api.mutationCount("/v1/loan/approve") != 1 || len(data.Capacity.Financing.Loans) != 1 {
		t.Fatalf("ordinary upkeep drift abandoned a funded request: %+v", data)
	}
}

func TestPlayerLoanApprovedTransientInputsHoldWithoutRepayment(t *testing.T) {
	for _, key := range []string{"zones", "profile", "warehouses", "paused clock", "catalog hull", "unmeasured", "net worth"} {
		t.Run(key, func(t *testing.T) {
			h, runner := playerLoanApprovedFixture(t)
			mission, data := h.mission("__growth")
			switch key {
			case "paused clock":
				runner.stateCache.Clock.Paused = true
			case "catalog hull":
				runner.stateCache.Catalog.ShipClasses = nil
			case "unmeasured":
				for index := range runner.stateCache.Ships {
					runner.stateCache.Ships[index].Ship.CreatedAt = nil
				}
			case "net worth":
				overseasMetric(t, h, 10_000_000, time.Now().UTC().Add(time.Hour))
			default:
				meta := runner.meta[key]
				meta.Stale = true
				runner.meta[key] = meta
			}
			if changed, err := runner.driveGrowth(h.ctx, mission, &data); err != nil || changed || data.Capacity.Financing.Cancel || data.Action != nil {
				t.Fatalf("transient %s canceled approved debt instead of holding: %t, %v, %+v", key, changed, err, data)
			}
			if h.api.mutationCount("/v1/loan/repay") != 0 || h.api.mutationCount("/v1/shipyard/build-ship") != 0 {
				t.Fatalf("transient %s spent or repaid approved debt", key)
			}
		})
	}
}

func TestPlayerLoanMortgageApprovalReevaluatesLiveTerms(t *testing.T) {
	for _, affordable := range []bool{true, false} {
		t.Run(map[bool]string{true: "affordable drift", false: "unaffordable drift"}[affordable], func(t *testing.T) {
			h, runner := playerLoanAcceptanceFixture(t)
			key := recoveryHomePort + "|" + recoveryHull
			quote := runner.stateCache.Quotes[key]
			quote.Mortgage = mortgageTestTerms()
			base := h.api.server.Config.Handler
			h.api.server.Config.Handler = http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
				if req.Method == http.MethodGet && req.URL.Path == "/v1/shipyard/quote" && req.URL.Query().Get("port_id") == recoveryHomePort && req.URL.Query().Get("ship_class") == recoveryHull {
					h.api.writeJSON(w, http.StatusOK, quote)
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
			*quote.Mortgage.InstallmentPence++
			quote.Mortgage.TotalRepayablePence = *quote.Mortgage.InstallmentPence * int64(quote.Mortgage.Installments)
			if !affordable {
				*quote.Mortgage.InstallmentPence = 10_000_000
				quote.Mortgage.TotalRepayablePence = 180_000_000
			}
			loanCapacityRefresh(t, h, runner)
			mission, data := h.mission("__growth")
			changed, err := runner.driveGrowth(h.ctx, mission, &data)
			if err != nil {
				t.Fatal(err)
			}
			if affordable {
				if !changed || data.Action == nil || data.Action.Path != "/v1/loan/approve" || !sameMortgageTerms(data.Capacity.Mortgage, quote.Mortgage) {
					t.Fatalf("affordable live terms did not reach shared approval gates: %+v", data)
				}
				playerLoanAcceptanceDispatch(t, h, runner)
				if h.api.mutationCount("/v1/loan/approve") != 1 {
					t.Fatal("affordable mortgage drift entered a stage/veto loop")
				}
			} else {
				if changed || data.Action != nil || data.PlayerLoan == nil || h.api.mutationCount("/v1/loan/approve") != 0 {
					t.Fatalf("unaffordable new terms did not hold the posted request: %+v", data)
				}
				_, _ = h.mission("__growth")
			}
		})
	}
}

func TestCapacityGrossDebtCapIncludesExistingPrincipalAtPennyBoundary(t *testing.T) {
	quoteID := "this-package"
	loans := []gameapi.Loan{
		{Status: "active", OutstandingPrincipalPence: 100_000},
		{Status: "defaulted", IsMortgage: true, OutstandingPrincipalPence: 50_000},
		{Status: "repaid", OutstandingPrincipalPence: 900_000},
		{Status: "active", QuoteID: &quoteID, OutstandingPrincipalPence: 20_000},
	}
	decision := GrowthDecision{Mortgage: &gameapi.MortgageTerms{PrincipalPence: 100_000}}
	for _, delta := range []int64{-1, 0, 1} {
		if got := capacityDebtExposure(decision, 100_000+delta, 1_000_000, loans, map[string]bool{quoteID: true}); got != (delta <= 0) {
			t.Fatalf("gross existing loan and mortgage threshold %+dp: %t", delta, got)
		}
	}
	if capacityDebtExposure(decision, 100_000, 1_000_000, loans, nil) {
		t.Fatal("unrelated existing principal escaped the gross cap")
	}
}

func TestPlayerLoanBuildDeadlineUsesReceiptNotOfferCreation(t *testing.T) {
	now := time.Now().UTC()
	runner := &Runner{stateCache: AccountState{Clock: Clock{}}}
	runner.stateCache.Clock.Calendar.TimeScale = 60
	quote := gameapi.LoanQuote{ID: "quote", BorrowerCompanyID: recoveryCompanyID}
	loan := gameapi.Loan{ID: "loan", QuoteID: &quote.ID, BorrowerCompanyID: recoveryCompanyID, CreatedAt: now.Add(-7 * 24 * time.Hour)}
	for _, receipt := range []time.Time{now, {}} {
		data := missionData{Action: &durableAction{CreatedAt: now.Add(-time.Minute), ReceiptAt: receipt}, Capacity: &capacityIntent{Financing: &capacityFinancing{}}}
		runner.recordAcceptedCapacityLoan(&data, loan, quote)
		anchor := receipt
		if anchor.IsZero() {
			anchor = data.Action.CreatedAt
		}
		if !data.Capacity.Financing.BuildBy.Equal(anchor.Add(24 * time.Minute)) {
			t.Fatalf("offer age shortened approval's one-game-day build window: %+v", data.Capacity.Financing)
		}
	}
}

func TestPlayerLoanSourceCannotBeSpoofedByLenderName(t *testing.T) {
	quote := gameapi.LoanQuote{LenderCompanyID: krakenAcceptanceCompany, LenderName: "Goldsmiths instant lending"}
	if playerQuoteSource(quote) != "player_request" {
		t.Fatal("player-controlled name impersonated an automatic Goldsmiths quote")
	}
	quote.Auto = true
	if playerQuoteSource(quote) != "goldsmiths" {
		t.Fatal("automatic Goldsmiths quote lost its source")
	}
	quote.Auto, quote.LendingLineID = false, new(string("line"))
	if playerQuoteSource(quote) != "goldsmiths" {
		t.Fatal("server lending-line quote lost its source")
	}
}

func TestCapacityLoanEnforcesPaybackAfterInterest(t *testing.T) {
	decision := GrowthDecision{EconomicallyViable: true, QuotedHullPence: 200_000, IncrementalNetPencePerGameDay: 10_000}
	if result := evaluateCapacityLoan(decision, 100_000, 110_000, 110_000, 3_500, 3_000); !result.Borrow {
		t.Fatalf("control debt package did not pass coverage: %+v", result)
	}
	decision.QuotedHullPence = 5_000_000
	if result := evaluateCapacityLoan(decision, 100_000, 110_000, 110_000, 3_500, 3_000); result.Borrow || result.Verdict != "payback" {
		t.Fatalf("selection admitted capital above the fixed after-interest payback horizon: %+v", result)
	}
}

func TestPlayerLoanRequestCaptureFailureBlocksBorrowingButNotPaidCargoSale(t *testing.T) {
	for _, posted := range []bool{false, true} {
		for _, missing := range []bool{false, true} {
			name := map[bool]string{false: "posting", true: "approval"}[posted] + "/" + map[bool]string{false: "invalid", true: "missing"}[missing]
			t.Run(name, func(t *testing.T) {
				var h *recoveryHarness
				var runner *Runner
				if posted {
					h, runner = playerLoanAcceptancePosted(t)
					quote := playerLoanAcceptanceQuote(t, h, runner, 3_000)
					playerLoanAcceptancePublishQuote(t, h, runner, quote)
				} else {
					h, runner = playerLoanAcceptanceFixture(t)
				}
				base := h.api.server.Config.Handler
				h.api.server.Config.Handler = http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
					if req.Method == http.MethodGet && req.URL.Path == "/v1/loan/requests" {
						if missing {
							h.api.writeProblem(w, http.StatusNotFound, "missing", "own requests unavailable")
						} else {
							h.api.writeJSON(w, http.StatusOK, []map[string]any{{"id": "foreign", "borrower_company_id": krakenAcceptanceCompany}})
						}
						return
					}
					if req.Method == http.MethodPost && req.URL.Path == "/v1/market/place-order" {
						h.api.mu.Lock()
						defer h.api.mu.Unlock()
						h.api.fleet[0]["cargo_manifest"] = map[string]any{}
						h.api.fleet[0]["cargo_used_tons"] = 0
						h.api.vaultBalance["north"] += 600
						h.api.record(req, nil, http.StatusOK)
						h.api.writeJSON(w, http.StatusOK, map[string]any{"commodity_id": recoveryGrain, "filled_quantity": 6, "total_gross_pence": 600, "customs_duty_pence": 0})
						return
					}
					base.ServeHTTP(w, req)
				})
				loanCapacityRefresh(t, h, runner)
				if runner.loanRequestAuthority() {
					t.Fatal("invalid or missing capture retained request mutation authority")
				}
				if posted {
					playerLoanAcceptanceDrive(t, h, runner)
				} else {
					port, hull, decision, required, evidence := loanCapacityDecision(t, h, runner)
					if changed, err := runner.considerCapacityFinancing(h.ctx, port, hull, decision, required, evidence); err != nil || changed {
						t.Fatalf("request capture failure admitted borrowing: %t, %v", changed, err)
					}
				}
				if h.api.mutationCount("/v1/loan/request") != map[bool]int{false: 0, true: 1}[posted] || h.api.mutationCount("/v1/loan/approve") != 0 {
					t.Fatal("request capture failure permitted a new borrowing mutation")
				}
				h.api.setFleet([]map[string]any{destinationShipRow(recoveryShipBlastoise, "Blastoise", recoveryHomePort, "berthed", 3, nil, map[string]int64{recoveryGrain: 6})})
				loanCapacityRefresh(t, h, runner)
				ship := runner.stateCache.Ships[0]
				mission := &store.Mission{AccountID: h.account.ID, ShipID: ship.Ship.ID, Generation: uuid.NewString(), Phase: "arrival"}
				data := missionData{CostsKnown: true, Eligible: map[string]int64{recoveryGrain: 6}, Plan: Plan{Kind: "spot", Commodity: recoveryGrain, Quantity: 6}}
				if err := runner.stage(h.ctx, mission, &data, ship, "sell", "/v1/market/place-order", orderBody(ship, recoveryGrain, "ask", 6, 100, false)); err != nil {
					t.Fatal(err)
				}
				if changed, err := runner.dispatch(h.ctx, mission, &data); err != nil || !changed || data.ProceedsPence != 600 || data.Action != nil || h.api.mutationCount("/v1/market/place-order") != 1 {
					t.Fatalf("loan-request outage blocked ordinary sale of owned cargo: %t, %v, %+v", changed, err, data)
				}
			})
		}
	}
}

func TestPlayerLoanQuoteEventApprovesOnNextAuthoritativePass(t *testing.T) {
	h, runner := playerLoanAcceptancePosted(t)
	quote := playerLoanAcceptanceQuote(t, h, runner, 3_000)
	playerLoanAcceptancePublishQuote(t, h, runner, quote)
	stream := interceptEventStream(t, h.api, nil, 4)
	stream.frameFor = func(index int32) progressFrame {
		if index == 1 {
			return progressFrame{id: "player-quote-arrived", eventType: "loan_quote_received"}
		}
		return progressFrame{}
	}
	running := startRunner(t, runner, h.ctx)
	stream.waitReady(t, 4)
	running.cancel()
	if err := running.wait(t, 15*time.Second); err != nil && !errors.Is(err, context.Canceled) {
		t.Fatal(err)
	}
	_, data := h.mission("__growth")
	if runner.publishedFinancial.Load() != 1 || h.api.mutationCount("/v1/loan/approve") != 1 || data.Capacity == nil || len(data.Capacity.Financing.Loans) != 1 {
		t.Fatalf("quote event did not refresh then approve exactly once: %+v, holds %v", data, runner.holding)
	}
}

func TestPlayerLoanWaitProtectsDonorWithoutARegionalCapitalEntry(t *testing.T) {
	runner := &Runner{}
	required := map[string]int64{"north_sea": 11, "channel": 13}
	data := missionData{PlayerLoan: &playerLoanIntent{
		ProtectedByRegion: map[string]int64{"north_sea": 25},
		DonorByRegion:     map[string]int64{"channel": 10},
	}}
	runner.reserveMissionCash(required, &store.Mission{}, &data, ShipState{})
	if required["north_sea"] != 36 || required["channel"] != 23 {
		t.Fatalf("request wait failed to protect its own donor share: %v", required)
	}
}

func TestPlayerLoanEconomicAbandonmentBacksOffWithoutFallback(t *testing.T) {
	h, runner := playerLoanAcceptancePosted(t)
	h.api.setVaultBalance("north", 50_000_000)
	loanCapacityRefresh(t, h, runner)
	mission, data := h.mission("__growth")
	if changed, err := runner.driveGrowth(h.ctx, mission, &data); err != nil || !changed {
		t.Fatalf("fully cash-funded need did not end the wait: %t, %v", changed, err)
	}
	h.missingMission("__growth")
	key := recoveryHomePort + "|" + recoveryHull
	if until := decode[map[string]time.Time](runner.snapshot, "_player_loan_backoffs")[key]; !until.After(time.Now().Add(5*time.Hour)) ||
		runner.playerFallback(recoveryHomePort, recoveryHull) || h.api.mutationCount("/v1/loan/approve") != 0 {
		t.Fatalf("economic abandonment lost its candidate backoff or enabled fallback: %s", until)
	}
}

func TestPlayerLoanFallbackTicketClearsWhenGoldsmithsPackageStarts(t *testing.T) {
	h, runner := loanCapacityFixture(t, 750_000, false)
	loanCapacityCashShortfall(t, h, runner, 100_000)
	port, hull, decision, required, evidence := loanCapacityDecision(t, h, runner)
	if changed, err := runner.considerCapacityFinancing(h.ctx, port, hull, decision, required, evidence); err != nil || !changed {
		t.Fatalf("scoped Goldsmiths package did not start: %t, %v", changed, err)
	}
	if runner.playerFallback(port.ID, hull.ID) {
		t.Fatal("started Goldsmiths package retained its reusable fallback ticket")
	}
	restarted := h.runner(primaryTestKey)
	if restarted.playerFallback(port.ID, hull.ID) {
		t.Fatal("restart resurrected a consumed fallback ticket")
	}
}

func TestPlayerLoanEstimatedRebuildPrincipalPennyBoundary(t *testing.T) {
	for _, delta := range []int64{-1, 0, 1} {
		t.Run(map[int64]string{-1: "below", 0: "equal", 1: "above"}[delta], func(t *testing.T) {
			h, runner := playerLoanAcceptanceFixture(t)
			port, hull, decision, _, evidence := loanCapacityDecision(t, h, runner)
			shipID, rebuild := runner.playerLoanCollateral()
			if shipID == "" || rebuild <= 0 {
				t.Fatal("fixture lacks a free priced collateral ship")
			}
			principal := rebuild + delta
			installment := int64(math.Ceil(float64(principal) * capacityAnnuityFactor(3_000)))
			changed, err := runner.stagePlayerLoan(h.ctx, port, hull, decision, principal, installment, evidence)
			if err != nil || changed != (delta <= 0) {
				t.Fatalf("estimated rebuild %+dp: changed %t, %v", delta, changed, err)
			}
			if delta > 0 {
				h.missingMission("__growth")
				playerLoanAcceptanceDecision(t, h, "debt_capacity")
				if h.api.mutationCount("/v1/loan/request") != 0 || len(h.pending()) != 0 {
					t.Fatal("excess principal left a request or orphaned action")
				}
				return
			}
			_, data := h.mission("__growth")
			if data.PlayerLoan.CollateralKind != "ship_lien" || data.PlayerLoan.ShipID != shipID {
				t.Fatalf("eligible principal lost its highest estimated-rebuild lien: %+v", data.PlayerLoan)
			}
			playerLoanAcceptanceDispatch(t, h, runner)
			if h.api.mutationCount("/v1/loan/request") != 1 {
				t.Fatal("principal at or below estimated rebuild failed to post")
			}
		})
	}
}

func TestLoanCapacityExpiredUnacceptedQuoteReleasesBeforeTransientHolds(t *testing.T) {
	for _, gap := range []string{"zones", "profile", "measured net", "paused clock"} {
		t.Run(gap, func(t *testing.T) {
			h, runner := loanCapacityFixture(t, 750_000, false)
			loanCapacityCashShortfall(t, h, runner, 100_000)
			port, hull, decision, required, evidence := loanCapacityDecision(t, h, runner)
			if changed, err := runner.considerCapacityFinancing(h.ctx, port, hull, decision, required, evidence); err != nil || !changed {
				t.Fatalf("stage Goldsmiths package: %t, %v", changed, err)
			}
			mission, data := h.mission("__growth")
			data.Capacity.Financing.Quotes[0].ExpiresAt = time.Now().UTC().Add(-time.Second)
			if err := runner.saveMission(h.ctx, mission, &data); err != nil {
				t.Fatal(err)
			}
			switch gap {
			case "measured net":
				for index := range runner.stateCache.Ships {
					runner.stateCache.Ships[index].Ship.CreatedAt = nil
				}
			case "paused clock":
				runner.stateCache.Clock.Paused = true
			default:
				meta := runner.meta[gap]
				meta.Stale = true
				runner.meta[gap] = meta
			}
			if changed, err := runner.driveGrowth(h.ctx, mission, &data); err != nil || !changed {
				t.Fatalf("expired quote held for %s: %t, %v", gap, changed, err)
			}
			h.missingMission("__growth")
			playerLoanAcceptanceDecision(t, h, "quote_expiry")
			if h.api.mutationCount("/v1/loan/accept-quote") != 0 || h.api.mutationCount("/v1/loan/repay") != 0 || len(h.pending()) != 0 {
				t.Fatal("expired unaccepted debt sent a mutation or left an orphaned action")
			}
		})
	}
}
