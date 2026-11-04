package engine

import (
	"context"
	"errors"
	"math"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/DevYukine/go-tradewinds/internal/gameapi"
	"github.com/google/uuid"
)

// Unavailable collateral and transient Goldsmiths quote faults must record a
// durable decision without latching the due date, so the same installment can
// stage once collateral frees or the service recovers.
func TestLoanBridgeZeroFreeCollateralWaitsThenStagesWhenItFrees(t *testing.T) {
	for _, test := range []struct {
		name  string
		block func(t *testing.T, world *loanBridgeWorld, runner *Runner)
		free  func(t *testing.T, world *loanBridgeWorld, runner *Runner)
	}{
		{name: "open sale listings", block: func(t *testing.T, world *loanBridgeWorld, runner *Runner) {
			world.h.api.mu.Lock()
			world.h.api.shipListings = []map[string]any{
				{"id": "bridge-listing-blastoise", "ship_id": recoveryShipBlastoise, "status": "open"},
				{"id": "bridge-listing-charizard", "ship_id": recoveryShipCharizard, "status": "open"},
			}
			world.h.api.mu.Unlock()
		}, free: func(t *testing.T, world *loanBridgeWorld, runner *Runner) {
			world.h.api.mu.Lock()
			world.h.api.shipListings = []map[string]any{
				{"id": "bridge-listing-blastoise", "ship_id": recoveryShipBlastoise, "status": "cancelled"},
				{"id": "bridge-listing-charizard", "ship_id": recoveryShipCharizard, "status": "cancelled"},
			}
			world.h.api.mu.Unlock()
		}},
		{name: "active ship liens", block: func(t *testing.T, world *loanBridgeWorld, runner *Runner) {
			world.setLoans(runner.stateCache.Loans[0],
				loanBridgeLien(t, world, recoveryShipBlastoise),
				loanBridgeLien(t, world, recoveryShipCharizard))
		}, free: func(t *testing.T, world *loanBridgeWorld, runner *Runner) {
			world.setLoans(runner.stateCache.Loans[0])
		}},
	} {
		t.Run(test.name, func(t *testing.T) {
			world, runner := loanBridgeFixture(t, 100_000)
			loan := runner.stateCache.Loans[0]
			test.block(t, world, runner)
			loanCapacityRefresh(t, world.h, runner)
			world.cash(t, runner, nil)
			if changed, err := runner.serviceLoans(world.h.ctx); changed || err != nil {
				t.Fatalf("zero free collateral staged or failed loan service: changed=%v err=%v", changed, err)
			}
			world.h.missingMission("__loans")
			if len(world.requests()) != 0 || world.h.api.mutationCount("/v1/loan/accept-quote") != 0 || len(world.h.pending()) != 0 {
				t.Fatal("unavailable collateral still reached Goldsmiths")
			}
			loanBridgeNoMarker(t, world, runner, loan)
			loanBridgeDecision(t, world, "no free collateral; no quote requested")
			test.free(t, world, runner)
			loanCapacityRefresh(t, world.h, runner)
			world.cash(t, runner, nil)
			loanBridgeStage(t, world, runner, loan.ID, 100_000, recoveryShipCharizard)
		})
	}
}

// loanBridgeLien builds an active third-party lien whose installment falls far
// outside the bridge service window, so it blocks collateral without creating
// its own shortfall.
func loanBridgeLien(t *testing.T, world *loanBridgeWorld, ship string) gameapi.Loan {
	t.Helper()
	lien := snapshotTestLoan()
	lien.ID = "bridge-lien-" + ship
	lien.CollateralShipID = &ship
	due := world.now.Add(serverClockStaleness + gameDuration(3, 60) + 24*time.Hour)
	lien.NextPaymentDueAt = &due
	return lien
}

func TestLoanBridgeTransientQuoteFaultsStayUnlatched(t *testing.T) {
	for _, test := range []struct {
		name  string
		block func(t *testing.T, world *loanBridgeWorld) (restore func())
	}{
		{name: "malformed 200 body", block: malformedQuoteBody},
		{name: "server 5xx", block: func(t *testing.T, world *loanBridgeWorld) (restore func()) {
			world.mu.Lock()
			world.quoteStatus, world.quoteCode = http.StatusServiceUnavailable, "internal_error"
			world.mu.Unlock()
			return func() {
				world.mu.Lock()
				world.quoteStatus, world.quoteCode = 0, ""
				world.mu.Unlock()
			}
		}},
		{name: "dropped connection", block: droppedQuoteConnection},
	} {
		t.Run(test.name, func(t *testing.T) {
			world, runner := loanBridgeFixture(t, 100_000)
			loan := runner.stateCache.Loans[0]
			restore := test.block(t, world)
			changed, err := runner.serviceLoans(world.h.ctx)
			restore()
			if changed || err != nil {
				t.Fatalf("transient quote fault stopped or failed loan service: changed=%v err=%v", changed, err)
			}
			world.h.missingMission("__loans")
			loanBridgeNoMarker(t, world, runner, loan)
			loanBridgeDecision(t, world, "temporarily unavailable")
			if world.h.api.mutationCount("/v1/loan/accept-quote") != 0 || len(world.h.pending()) != 0 {
				t.Fatal("transient quote fault dispatched or orphaned a bridge action")
			}
			if len(world.requests()) == 0 {
				t.Fatal("fault hook never observed the attempted quote")
			}
			// The fleet keeps trading: the same due stages once the fault clears.
			loanCapacityRefresh(t, world.h, runner)
			loanBridgeStage(t, world, runner, loan.ID, 100_000, recoveryShipCharizard)
		})
	}
}

// malformedQuoteBody answers quote requests with a truncated 200 JSON body so
// the decoding failure surfaces immediately, without transport retry sleeps.
func malformedQuoteBody(t *testing.T, world *loanBridgeWorld) (restore func()) {
	t.Helper()
	base := world.h.api.server.Config.Handler
	world.h.api.server.Config.Handler = http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		if request.Method == http.MethodGet && request.URL.Path == "/v1/loan/quote" {
			// Record the real request terms, then discard the valid body.
			world.quote(httptest.NewRecorder(), request)
			writer.Header().Set("Content-Type", "application/json")
			writer.WriteHeader(http.StatusOK)
			_, _ = writer.Write([]byte(`{"quote":{"id":`))
			return
		}
		base.ServeHTTP(writer, request)
	})
	return func() { world.h.api.server.Config.Handler = base }
}

// droppedQuoteConnection lets the ordinary handler record the attempt, then
// severs the connection before any buffered byte is flushed.
func droppedQuoteConnection(t *testing.T, world *loanBridgeWorld) (restore func()) {
	t.Helper()
	base := world.h.api.server.Config.Handler
	world.h.api.server.Config.Handler = http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		if request.Method == http.MethodGet && request.URL.Path == "/v1/loan/quote" {
			world.quote(writer, request)
			if connection, _, err := writer.(http.Hijacker).Hijack(); err == nil {
				_ = connection.Close()
			}
			return
		}
		base.ServeHTTP(writer, request)
	})
	return func() { world.h.api.server.Config.Handler = base }
}

func TestLoanBridgeCanceledContextStillPropagates(t *testing.T) {
	world, runner := loanBridgeFixture(t, 100_000)
	loan := runner.stateCache.Loans[0]
	base := world.h.api.server.Config.Handler
	ctx, cancel := context.WithCancel(world.h.ctx)
	defer cancel()
	world.h.api.server.Config.Handler = http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		if request.Method == http.MethodGet && request.URL.Path == "/v1/loan/quote" {
			// Cancel while the quote attempt is in flight; the client's own
			// context, not a Goldsmiths verdict, must decide the outcome.
			cancel()
			select {
			case <-request.Context().Done():
			case <-time.After(5 * time.Second):
			}
			return
		}
		base.ServeHTTP(writer, request)
	})
	defer func() { world.h.api.server.Config.Handler = base }()
	if changed, err := runner.serviceLoans(ctx); changed || !errors.Is(err, context.Canceled) {
		t.Fatalf("canceled quote attempt was swallowed: changed=%v err=%v", changed, err)
	}
	world.h.missingMission("__loans")
	loanBridgeNoMarker(t, world, runner, loan)
	if world.h.api.mutationCount("/v1/loan/accept-quote") != 0 || len(world.h.pending()) != 0 {
		t.Fatal("canceled quote attempt still produced a sendable or sent bridge")
	}
}

// A pending unaccepted growth financing package holds its quoted collateral,
// so the emergency bridge must quote the next cheapest free lien instead and
// must never double-admit that collateral. Cancelling the package releases it.
func TestLoanBridgeAvoidsPendingGrowthFinancingCollateral(t *testing.T) {
	world, runner := loanBridgeFixture(t, 100_000)
	loan := runner.stateCache.Loans[0]
	generation := uuid.NewString()
	world.h.putMission("__growth", generation, "growth_finance",
		missionData{Capacity: loanBridgeGrowthFinancing(t, world, recoveryShipCharizard, false)})
	loanCapacityRefresh(t, world.h, runner)
	loanBridgeStage(t, world, runner, loan.ID, 100_000, recoveryShipBlastoise)
	before := world.requests()
	if len(before) != 1 || before[0].ShipID != recoveryShipBlastoise {
		t.Fatalf("bridge quoted the pending growth lien's collateral: %+v", before)
	}
	_, growth := world.h.mission("__growth")
	financing := growth.Capacity.Financing
	if financing.Cancel || len(financing.Loans) != 0 || len(financing.Quotes) != 1 ||
		*financing.Quotes[0].CollateralShipID != recoveryShipCharizard {
		t.Fatalf("bridge mutated the pending growth package: %+v", financing)
	}
	if world.h.api.mutationCount("/v1/loan/accept-quote") != 0 || len(world.h.pending()) != 0 {
		t.Fatal("bridge admission accepted or journaled growth debt")
	}
	// Cancel the package, retire the never-sent staged bridge without its
	// latch, and prove the same due may now pledge the released collateral.
	if err := world.h.db.DeleteMission(world.h.ctx, world.h.account.ID, "__loans"); err != nil {
		t.Fatal(err)
	}
	dates := decode[map[string]bool](runner.snapshot, "_loan_bridge_due")
	delete(dates, loanRefinanceDueKey(loan))
	runner.snapshot["_loan_bridge_due"] = encode(dates)
	if err := world.h.db.SaveSnapshot(world.h.ctx, world.h.account.ID, runner.snapshot); err != nil {
		t.Fatal(err)
	}
	world.h.putMission("__growth", generation, "growth_finance",
		missionData{Capacity: loanBridgeGrowthFinancing(t, world, recoveryShipCharizard, true)})
	runner = world.h.runner(primaryTestKey)
	loanCapacityRefresh(t, world.h, runner)
	loanBridgeStage(t, world, runner, loan.ID, 100_000, recoveryShipCharizard)
	sawReleased := false
	for _, request := range world.requests()[len(before):] {
		sawReleased = sawReleased || request.ShipID == recoveryShipCharizard
	}
	if !sawReleased {
		t.Fatalf("cancelled package did not release its collateral for requoting: %+v", world.requests()[len(before):])
	}
}

// loanBridgeGrowthFinancing stages a one-quote growth package whose only lien
// is the named ship, mirroring a real admitted-but-unaccepted financing intent.
func loanBridgeGrowthFinancing(t *testing.T, world *loanBridgeWorld, ship string, cancel bool) *capacityIntent {
	t.Helper()
	installment := int64(math.Ceil(float64(200_000) * (0.30 / 36) / (1 - math.Pow(1+0.30/36, -36))))
	quote := gameapi.LoanQuote{
		ID: "growth-financing-" + ship, LenderCompanyID: "goldsmiths", BorrowerCompanyID: world.h.account.CompanyID,
		BankingRegion: "north_sea", PrincipalPence: 200_000, AnnualRateBPS: 3_000,
		TermGameDays: 360, Repayment: "amortised", InstallmentPence: &installment,
		TotalRepayablePence: installment * 36, CollateralShipID: &ship, Status: "open",
		CreatedAt: world.now, ExpiresAt: world.now.Add(time.Hour),
	}
	return &capacityIntent{BuildPort: recoveryHomePort, BuildClass: recoveryHull, AdmittedAt: world.now,
		Financing: &capacityFinancing{Quotes: []gameapi.LoanQuote{quote}, Cancel: cancel}}
}
