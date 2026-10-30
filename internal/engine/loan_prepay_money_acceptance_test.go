package engine

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"math"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/DevYukine/go-tradewinds/internal/gameapi"
	"github.com/DevYukine/go-tradewinds/internal/store"
)

func TestMoneyCapacityOnlyCheapHullsCanBeCandidates(t *testing.T) {
	for _, class := range []string{"barque", "fluyt", "frigate"} {
		t.Run(class, func(t *testing.T) {
			in := capacityGrowthAcceptanceInput()
			in.Hull.ID = class
			got := EvaluateCapacityGrowth(in)
			if got.Accepted || got.EconomicallyViable || strings.TrimSpace(got.Reason) == "" {
				t.Fatalf("non-cheap %s entered capacity candidate admission: %+v", class, got)
			}
		})
	}
	for _, class := range []string{"hoy", "cutter", "brigantine", "collier"} {
		in := capacityGrowthAcceptanceInput()
		in.Hull.ID, in.MeasuredNetAvailable = class, false
		if got := EvaluateCapacityGrowth(in); !got.Accepted {
			t.Fatalf("cash-funded cheap %s was accidentally put behind an earnings wait: %+v", class, got)
		}
	}
}

func TestMoneyCapacityNonCheapSavingsTargetsAreRetired(t *testing.T) {
	for _, class := range []string{"barque", "fluyt", "frigate"} {
		t.Run(class, func(t *testing.T) {
			h := newRecoveryHarness(t)
			runner := h.runner(primaryTestKey)
			runner.stateCache.Catalog = Catalog{Ports: []Port{{ID: recoveryHomePort, HasShipyard: true, Phase: 1}}, ShipClasses: []Hull{{ID: class, HoldCapacityTons: 240, MaxCannons: 4, Phase: 1}}}
			runner.snapshot["_capacity_savings_target"] = encode(capacitySavingsTarget{
				PortID: recoveryHomePort, HullID: class, RequiredPence: 10_000_000, CapitalPence: 3_000_000,
				DepthNetPencePerGameDay: 100_000, ExpiresAt: time.Now().UTC().Add(time.Hour),
			})
			got, err := runner.liveCapacitySavingsTarget(h.ctx)
			if err != nil {
				t.Fatal(err)
			}
			if got.PortID != "" || decode[capacitySavingsTarget](runner.snapshot, "_capacity_savings_target").HullID != "" {
				t.Fatalf("non-cheap %s remained a live durable savings target: %+v", class, got)
			}
		})
	}
}

func TestMoneyCapacityUnmeasuredClassCannotFillCashShortfallWithMortgage(t *testing.T) {
	in := capacityGrowthAcceptanceInput()
	in.MeasuredNetAvailable, in.MeasuredRatePencePerGameDay, in.SampleHullGameDays = false, 0, 0
	cash := EvaluateCapacityGrowth(in)
	if !cash.Accepted {
		t.Fatalf("cash reinvestment control rejected: %+v", cash)
	}
	in.SurplusPence = growthCapital(cash) + cash.WorkingCapitalPence - 10_000
	if short := EvaluateCapacityGrowth(in); short.Accepted || short.CashShortfallPence != 10_000 {
		t.Fatalf("fixture is not exactly cash short before debt: %+v", short)
	}
	in.Mortgage = mortgageTestTerms()
	if got := EvaluateCapacityGrowth(in); got.Accepted || got.EconomicallyViable {
		t.Fatalf("unmeasured cheap class used optimistic voyage earnings to obtain a mortgage: %+v", got)
	}
}

func TestMoneyCapacityMortgageMeasuredServiceCoverageBoundary(t *testing.T) {
	for _, measured := range []float64{287.49, 287.5} {
		in := capacityGrowthAcceptanceInput()
		in.HullPricePence, in.InitialStoresPence = 60_000, 0
		in.Mortgage = mortgageTestTerms() // 2,300p every ten game days: 230p/gd service.
		in.MeasuredNetAvailable, in.MeasuredRatePencePerGameDay = true, measured
		in.CandidateRatePencePerGameDay, in.IncrementalDailyUpkeepPence = 100_000, 0
		in.MaxPaybackDays = 1_000_000
		got := EvaluateCapacityGrowth(in)
		if got.Accepted != (measured >= 1.25*230) {
			t.Errorf("measured class net %.2fp/gd must cover 1.25x 230p/gd service: %+v", measured, got)
		}
	}
}

type moneyLoanAPI struct {
	mu         sync.Mutex
	h          *recoveryHarness
	base       http.Handler
	loans      []gameapi.Loan
	repayments map[string]int
}

func (f *moneyLoanAPI) ServeHTTP(writer http.ResponseWriter, request *http.Request) {
	if request.Method == http.MethodGet && request.URL.Path == "/v1/loan/list" {
		f.mu.Lock()
		defer f.mu.Unlock()
		f.h.api.writeJSON(writer, http.StatusOK, f.loans)
		return
	}
	if request.Method == http.MethodPost && request.URL.Path == "/v1/loan/repay" {
		body, err := io.ReadAll(request.Body)
		if err != nil {
			f.h.t.Error(err)
			writer.WriteHeader(http.StatusBadRequest)
			return
		}
		var payload gameapi.RepayLoanRequest
		if err := json.Unmarshal(body, &payload); err != nil {
			f.h.t.Error(err)
			writer.WriteHeader(http.StatusBadRequest)
			return
		}
		pending, err := f.h.db.PendingActions(request.Context(), f.h.account.ID)
		journaled := false
		for _, action := range pending {
			journaled = journaled || action.ID == request.Header.Get("Idempotency-Key") && action.Method == request.Method && action.Path == request.URL.Path && bytes.Equal(action.Body, body)
		}
		if err != nil || !journaled || request.Header.Get("Idempotency-Key") == "" {
			f.h.t.Error("prepayment reached the API without its immutable pending journal request")
			writer.WriteHeader(http.StatusConflict)
			return
		}
		f.mu.Lock()
		defer f.mu.Unlock()
		for i := range f.loans {
			loan := &f.loans[i]
			if loan.ID != payload.LoanID {
				continue
			}
			amount := loan.OwedPence
			if payload.AmountPence != nil {
				amount = *payload.AmountPence
			}
			if loan.IsMortgage || loan.Status != "active" || amount != loan.OwedPence || amount <= 0 {
				f.h.t.Errorf("prepayment must be the exact observed owed amount of active non-mortgage debt: request=%s loan=%+v", body, loan)
				writer.WriteHeader(http.StatusBadRequest)
				return
			}
			f.repayments[loan.ID]++
			if f.repayments[loan.ID] != 1 {
				f.h.t.Error("fully paid loan was sent again")
			}
			f.h.api.mu.Lock()
			f.h.api.vaultBalance["north"] -= amount
			f.h.api.record(request, body, http.StatusOK)
			f.h.api.mu.Unlock()
			loan.PrincipalRepaidPence += loan.OutstandingPrincipalPence
			loan.OwedPence, loan.OutstandingPrincipalPence, loan.AccruedInterestPence = 0, 0, 0
			loan.Status, loan.CollateralStatus = "repaid", "released"
			loan.NextPaymentPence = new(int64(0))
			loan.UpdatedAt = time.Now().UTC()
			f.h.api.writeJSON(writer, http.StatusOK, loan)
			return
		}
		writer.WriteHeader(http.StatusNotFound)
		return
	}
	// Reuse the recovery server unchanged, with the canonical banking regions
	// required by authoritative loan validation (as loanCapacityFixture does).
	if request.Method == http.MethodGet {
		recorder := httptest.NewRecorder()
		f.base.ServeHTTP(recorder, request)
		for key, values := range recorder.Header() {
			writer.Header()[key] = values
		}
		writer.WriteHeader(recorder.Code)
		body := strings.ReplaceAll(recorder.Body.String(), `"north"`, `"north_sea"`)
		body = strings.ReplaceAll(body, `"south"`, `"channel"`)
		_, _ = io.WriteString(writer, body)
		return
	}
	f.base.ServeHTTP(writer, request)
}

func moneyPrepayFixture(t *testing.T, owed []int64, surplus int64, mortgage bool) (*recoveryHarness, *Runner, *moneyLoanAPI, int64) {
	t.Helper()
	h := newRecoveryHarness(t)
	var fleet []map[string]any
	for i := range max(1, len(owed)) {
		id := recoveryShipBlastoise
		if i > 0 {
			id = recoveryShipCharizard
		}
		row := sailingShipRow(id, fmt.Sprintf("money-hoy-%d", i), recoveryHomePort, recoveryFarPort)
		row["ship"].(map[string]any)["created_at"] = time.Now().UTC().Add(-30 * 24 * time.Hour)
		fleet = append(fleet, row)
	}
	h.api.setFleet(fleet)
	h.api.setFactorPorts(recoveryHomePort, recoveryFarPort)
	h.api.mu.Lock()
	h.api.autoRemit, h.api.minReserve = true, 10_000_000
	h.api.mu.Unlock()
	api := &moneyLoanAPI{h: h, base: h.api.server.Config.Handler, repayments: make(map[string]int), loans: make([]gameapi.Loan, 0, len(owed))}
	for i, amount := range owed {
		loan := snapshotTestLoan()
		loan.ID = fmt.Sprintf("money-goldsmiths-%d", i)
		loan.IsMortgage = mortgage
		loan.PrincipalPence = 746_952
		loan.OwedPence = amount
		loan.OutstandingPrincipalPence = 561_367 + int64(i)
		loan.AccruedInterestPence = amount - loan.OutstandingPrincipalPence
		loan.PrincipalRepaidPence = loan.PrincipalPence - loan.OutstandingPrincipalPence
		loan.InstallmentPence, loan.NextPaymentPence = new(int64(24_103)), new(int64(24_103))
		loan.NextPaymentDueAt = new(time.Now().UTC().Add(24 * time.Hour))
		loan.MaturesAt = new(time.Now().UTC().Add(72 * time.Hour))
		loan.CollateralShipID = new(fleet[i]["ship"].(map[string]any)["id"].(string))
		loan.CreatedAt, loan.UpdatedAt = time.Now().UTC().Add(-48*time.Hour), time.Now().UTC()
		api.loans = append(api.loans, loan)
	}
	h.api.server.Config.Handler = api
	for i := range 10 {
		persistGrowthWindowSettlement(t, h, store.Settlement{
			ID: fmt.Sprintf("money-lot-%02d", i), Kind: "spot", CostsKnown: true,
			QuantityTons: 10, AcquisitionPence: 10_000, ProceedsPence: 0, GameSeconds: 86_400,
			CompletedAt: time.Now().UTC().Add(-time.Duration(20-i) * time.Minute),
		})
	}
	persistGrowthWindowSettlement(t, h, store.Settlement{
		ID: "money-measured-closeout", Kind: "aborted", CostsKnown: true,
		ProceedsPence: 10_000_000, GameSeconds: 3_600,
		CompletedAt: time.Now().UTC().Add(-time.Hour),
	})
	runner := h.runner(primaryTestKey)
	loanCapacityRefresh(t, h, runner)
	runner.stateCache.Clock.Calendar.TimeScale = 60
	runner.opts.CapacityPaybackDays = 0 // No accepted capacity use of this test's cash.
	protected, err := runner.treasuryReserves(h.ctx)
	if err != nil {
		t.Fatal(err)
	}
	lot, err := runner.capacityLotCash(h.ctx, time.Now().UTC())
	if err != nil || lot != 10_000 {
		t.Fatalf("working-cash fixture lot: %d, %v", lot, err)
	}
	buffer := int64(len(fleet)+1) * lot
	for i := range runner.stateCache.Vaults {
		vault := &runner.stateCache.Vaults[i]
		vault.BalancePence = protected[vault.BankingRegion]
		if vault.BankingRegion == "north_sea" {
			vault.BalancePence += buffer + surplus
		}
		region := "south"
		if vault.BankingRegion == "north_sea" {
			region = "north"
		}
		h.api.setVaultBalance(region, vault.BalancePence)
	}
	return h, runner, api, protected["north_sea"] + buffer
}

func moneyLoanWork(t *testing.T, h *recoveryHarness, runner *Runner, passes int) {
	t.Helper()
	for range passes {
		if _, err := runner.work(h.ctx); err != nil {
			t.Fatalf("prepayment runner pass: %v", err)
		}
	}
}

func TestMoneyLoanPrepayExactSurplusAndOnePennyShort(t *testing.T) {
	for _, owed := range []int64{563_785, 563_789} {
		for _, extra := range []int64{-1, 0, 1} {
			t.Run(fmt.Sprintf("owed_%d_surplus_%+d", owed, extra), func(t *testing.T) {
				h, runner, api, protected := moneyPrepayFixture(t, []int64{owed}, 0, false)
				// 561,367p × 3000/10,000/360 rounds up to 468p interest; payoff releases 2 × 24,103p of this loan's reserves.
				surplus := owed + 468 - 48_206 + extra
				for i := range runner.stateCache.Vaults {
					vault := &runner.stateCache.Vaults[i]
					if vault.BankingRegion == "north_sea" {
						vault.BalancePence += surplus
						h.api.setVaultBalance("north", vault.BalancePence)
					}
				}
				protected += 468 - 48_206
				moneyLoanWork(t, h, runner, 8)
				want := 1
				if extra < 0 {
					want = 0
				}
				if got := h.api.mutationCount("/v1/loan/repay"); got != want {
					t.Fatalf("surplus above reserves and (fleet+1) lots must repay the exact 5,637.xx Cr balance once, or send nothing when 1p short: got %d want %d, holds=%v", got, want, runner.holding)
				}
				if want == 1 {
					api.mu.Lock()
					count, balance := api.repayments["money-goldsmiths-0"], api.loans[0].OwedPence
					api.mu.Unlock()
					if count != 1 || balance != 0 || len(h.pending()) != 0 {
						t.Fatal("prepayment did not close exactly one journaled loan")
					}
					h.api.mu.Lock()
					cash := h.api.vaultBalance["north"]
					h.api.mu.Unlock()
					if cash != protected+extra {
						t.Fatalf("prepay spent protected reserves or lots: cash=%d, want %d", cash, protected+extra)
					}
				}
			})
		}
	}
}

func TestMoneyLoanPrepayBothGoldsmithsBalancesOnceAcrossRestart(t *testing.T) {
	h, runner, api, _ := moneyPrepayFixture(t, []int64{563_785, 563_789}, 563_785+563_789, false)
	moneyLoanWork(t, h, runner, 12)
	if got := h.api.mutationCount("/v1/loan/repay"); got != 2 {
		t.Fatalf("two funded Goldsmiths loans need exactly two repayments, got %d", got)
	}
	restarted := h.runner(primaryTestKey)
	loanCapacityRefresh(t, h, restarted)
	restarted.stateCache.Clock.Calendar.TimeScale = 60
	restarted.opts.CapacityPaybackDays = 0
	moneyLoanWork(t, h, restarted, 4)
	api.mu.Lock()
	defer api.mu.Unlock()
	if api.repayments["money-goldsmiths-0"] != 1 || api.repayments["money-goldsmiths-1"] != 1 || h.api.mutationCount("/v1/loan/repay") != 2 || len(h.pending()) != 0 {
		t.Fatalf("restart duplicated or left unresolved a fully paid Goldsmiths loan: %+v", api.repayments)
	}
	actions, err := h.db.ActionsBetween(h.ctx, h.account.ID, time.Now().UTC().Add(-time.Hour), time.Now().UTC().Add(time.Minute))
	if err != nil {
		t.Fatal(err)
	}
	count := 0
	for _, action := range actions {
		if action.Path == "/v1/loan/repay" {
			count++
			if action.State != store.ActionComplete || action.Status != http.StatusOK {
				t.Fatalf("repayment journal is not terminal: %+v", action)
			}
		}
	}
	if count != 2 {
		t.Fatalf("repayment requests did not leave exactly two terminal journal rows: %d", count)
	}
}

func TestMoneyLoanPrepayNeverPrepaysMortgage(t *testing.T) {
	h, runner, _, _ := moneyPrepayFixture(t, []int64{563_785}, 2_000_000, true)
	moneyLoanWork(t, h, runner, 8)
	if h.api.mutationCount("/v1/loan/repay") != 0 || len(h.pending()) != 0 {
		t.Fatal("optional surplus prepayment touched a mortgage instead of leaving its scheduled service alone")
	}
}

func TestMoneyLoanPrepayYieldsToAcceptedCapacityCandidate(t *testing.T) {
	h, runner, _, _ := moneyPrepayFixture(t, []int64{563_785}, 2_000_000, false)
	runner.opts.CapacityPaybackDays = 420
	if changed, err := runner.considerGrowth(h.ctx); err != nil || !changed {
		t.Fatalf("accepted capacity candidate control: changed=%t err=%v holds=%v", changed, err, runner.holding)
	}
	_, data := h.mission("__growth")
	if data.Capacity == nil || data.BuildClass != "hoy" {
		t.Fatalf("control did not admit the cheap capacity candidate: %+v", data)
	}
	moneyLoanWork(t, h, runner, 2)
	if h.api.mutationCount("/v1/loan/repay") != 0 {
		t.Fatal("surplus repayment stole cash already held by an accepted capacity candidate")
	}
}

func TestMoneyLoanPrepayYieldsToLiveCheapHullSavingsTarget(t *testing.T) {
	h, runner, _, _ := moneyPrepayFixture(t, []int64{563_785}, 563_785, false)
	runner.opts.CapacityPaybackDays = 420
	key := recoveryHomePort + "|hoy"
	quote := runner.stateCache.Quotes[key]
	quote.TotalCostPence = new(int64(2_000_000))
	runner.stateCache.Quotes[key] = quote
	runner.snapshot["_capacity_savings_target"] = encode(capacitySavingsTarget{
		PortID: recoveryHomePort, HullID: "hoy", RequiredPence: 2_500_000, CapitalPence: 2_200_000,
		DepthNetPencePerGameDay: 100_000, ExpiresAt: time.Now().UTC().Add(time.Hour),
	})
	if changed, err := runner.considerGrowth(h.ctx); err != nil || changed {
		t.Fatalf("live cash-short savings target control: changed=%t err=%v holds=%v", changed, err, runner.holding)
	}
	target := decode[capacitySavingsTarget](runner.snapshot, "_capacity_savings_target")
	if target.HullID != "hoy" || target.RequiredPence <= 563_785 || math.IsNaN(target.DepthNetPencePerGameDay) {
		t.Fatalf("fixture lost its live cheap-hull savings hold: %+v", target)
	}
	moneyLoanWork(t, h, runner, 2)
	if h.api.mutationCount("/v1/loan/repay") != 0 {
		t.Fatal("surplus repayment consumed cash earmarked by a live savings target")
	}
}
