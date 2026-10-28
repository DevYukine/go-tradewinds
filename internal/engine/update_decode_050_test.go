package engine

import (
	"encoding/json"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/DevYukine/go-tradewinds/internal/gameapi"
	"github.com/DevYukine/go-tradewinds/internal/store"
	"github.com/google/uuid"
)

func TestDecode050Fields(t *testing.T) {
	t.Parallel()
	var catalog Catalog
	raw := []byte(`{"ports":[{"id":"lisbon","yard_discount":{"labour_discount_bps":3000,"turnkey_markup_waived":true}}],"ship_classes":[{"id":"packet","passengers_per_ton":2}]}`)
	if err := json.Unmarshal(raw, &catalog); err != nil {
		t.Fatal(err)
	}
	if catalog.Ports[0].YardDiscount == nil || catalog.Ports[0].YardDiscount.LabourDiscountBPS != 3000 || !catalog.Ports[0].YardDiscount.TurnkeyMarkupWaived || catalog.ShipClasses[0].PassengersPerTon != 2 {
		t.Fatalf("catalog lost 0.5.0 terms: %+v", catalog)
	}
	var apiCatalog gameapi.Catalog
	if err := json.Unmarshal(raw, &apiCatalog); err != nil || apiCatalog.Ports[0].YardDiscount == nil || apiCatalog.ShipClasses[0].PassengersPerTon != 2 {
		t.Fatalf("API catalog lost 0.5.0 terms: %+v, %v", apiCatalog, err)
	}
	var ship ShipState
	if err := json.Unmarshal([]byte(`{"ship":{"id":"ship-one","passenger_hold_tons":1},"cargo_used_tons":0,"cargo_manifest":{}}`), &ship); err != nil {
		t.Fatal(err)
	}
	if ship.PassengerHoldTons != 1 || emptyCargo(ship) {
		t.Fatal("occupied passenger hold was treated as empty")
	}
	var restored ShipState
	if err := json.Unmarshal(encode(ship), &restored); err != nil || restored.PassengerHoldTons != 1 {
		t.Fatalf("persisted ship lost passenger safety: %+v, %v", restored, err)
	}
	ship.PassengerHoldTons = 0
	if !emptyCargo(ship) {
		t.Fatal("zero passengers occupied an otherwise empty hold")
	}
	var quote YardQuote
	if err := json.Unmarshal([]byte(`{"mortgage":{"lender_company_id":"goldsmiths","lender_name":"Goldsmiths","principal_pence":60000,"annual_rate_bps":2500,"term_game_days":180,"repayment":"amortised","installments":18,"installment_pence":4000,"total_repayable_pence":72000,"available":true},"yard_discount":{"labour_discount_bps":3000,"turnkey_markup_waived":true}}`), &quote); err != nil {
		t.Fatal(err)
	}
	if quote.Mortgage == nil || quote.Mortgage.PrincipalPence != 60000 || quote.Mortgage.InstallmentPence == nil || *quote.Mortgage.InstallmentPence != 4000 || quote.Mortgage.Installments != 18 || quote.Mortgage.TermGameDays != 180 || !quote.Mortgage.Available || quote.YardDiscount == nil {
		t.Fatalf("yard quote lost mortgage terms: %+v", quote)
	}
	if err := json.Unmarshal([]byte(`{"mortgage":null,"yard_discount":null}`), &quote); err != nil || quote.Mortgage != nil || quote.YardDiscount != nil {
		t.Fatalf("nullable yard terms failed: %+v, %v", quote, err)
	}
	for _, value := range []string{"false", "true", "null", "", `"false"`} {
		body := `{"id":"loan","borrower_company_id":"borrower","banking_region":"channel","status":"offered","owed_pence":0,"arrears_pence":0,"collateral_kind":"unsecured","collateral_status":"none"`
		if value != "" {
			body += `,"is_mortgage":` + value
		}
		body += `}`
		// Durable mission blobs may predate mortgage classification; live
		// captures must explicitly supply the boolean before type decoding.
		valid := value == "false" || value == "true"
		err := validateBorrowerLoans(json.RawMessage("["+body+"]"), "borrower")
		if (err == nil) != valid {
			t.Fatalf("live mortgage flag %q validation error %v", value, err)
		}
		var loan gameapi.Loan
		err = json.Unmarshal([]byte(body), &loan)
		if (err == nil) != (valid || value == "") || err == nil && loan.IsMortgage != (value == "true") {
			t.Fatalf("durable mortgage flag %q decoded to %+v, error %v", value, loan, err)
		}
	}
}

func TestDepartRefusedWithUnknownPassengers(t *testing.T) {
	h, runner := loanCapacityFixture(t, 750_000, false)
	ship := runner.stateCache.Ships[0]
	ship.PassengerHoldTons = 1
	runner.stateCache.Ships[0] = ship
	mission := &store.Mission{AccountID: h.account.ID, ShipID: ship.Ship.ID, Generation: uuid.NewString()}
	data := missionData{Action: &durableAction{Stage: "depart", Path: "/v1/fleet/depart", Body: encode(map[string]any{"ship_id": ship.Ship.ID, "dest_port_id": recoveryHomePort})}}
	if err := runner.newActionPreflight(h.ctx, mission, &data); err == nil || !strings.Contains(err.Error(), "passengers aboard") {
		t.Fatalf("unknown passengers reached departure planning: %v", err)
	}
	ship.PassengerHoldTons = 0
	runner.stateCache.Ships[0] = ship
	if err := runner.newActionPreflight(h.ctx, mission, &data); err != nil && strings.Contains(err.Error(), "passengers aboard") {
		t.Fatalf("empty hold retained passenger block: %v", err)
	}
}

func TestJournalPassengerRows(t *testing.T) {
	h := newRecoveryHarness(t)
	for kind, payload := range map[string]any{
		"passengers_boarded": map[string]any{"ship_id": "passenger-ship", "heads": 7, "fare_per_head_pence": 300, "full_fare_pence": 2100, "dest_port_id": "lisbon"},
		"passengers_paid":    map[string]any{"ship_id": "passenger-ship", "fare_paid_pence": 1800, "game_days_late": 1.5},
		"passengers_lost":    map[string]any{"ship_id": "passenger-ship", "cause": "overdue"},
		"voyage_hazard":      map[string]any{"ship_id": "passenger-ship", "cargo_loss_pct": 25, "stores_loss_pct": 0},
	} {
		if err := h.db.RecordEvent(h.ctx, h.account.ID, kind, kind, encode(map[string]any{"company_id": h.account.CompanyID, "payload": payload})); err != nil {
			t.Fatal(err)
		}
	}
	history, err := h.db.History(h.ctx, h.account.ID, "journal", 10)
	if err != nil {
		t.Fatal(err)
	}
	var rows []store.JournalRow
	if err := json.Unmarshal(encode(history), &rows); err != nil {
		t.Fatal(err)
	}
	if len(rows) != 4 {
		t.Fatalf("passenger activity absent from journal: %+v", rows)
	}
	for _, row := range rows {
		if row.ShipID != "passenger-ship" {
			t.Fatalf("passenger event lost ship ownership: %+v", row)
		}
		switch row.Kind {
		case "passengers_boarded":
			if row.Heads == nil || *row.Heads != 7 || row.FarePerHeadPence == nil || *row.FarePerHeadPence != 300 || row.Destination != "lisbon" || !strings.Contains(row.Summary, "7 heads") {
				t.Fatalf("boarding details lost: %+v", row)
			}
		case "passengers_paid":
			if row.AmountPence == nil || *row.AmountPence != 1800 || row.AmountKind != "cash_in" || row.FarePaidPence == nil || *row.FarePaidPence != 1800 || row.GameDaysLate == nil || *row.GameDaysLate != 1.5 {
				t.Fatalf("fare credit misrepresented: %+v", row)
			}
		case "passengers_lost":
			if row.Cause != "overdue" || !strings.Contains(strings.ToLower(row.Summary), "overdue") {
				t.Fatalf("loss cause hidden: %+v", row)
			}
		case "voyage_hazard":
			if row.CargoLossPct == nil || *row.CargoLossPct != 25 || row.StoresLossPct == nil || *row.StoresLossPct != 0 || !strings.Contains(row.Summary, "stores loss 0%") {
				t.Fatalf("hazard losses hidden or invented: %+v", row)
			}
		}
	}
}

func TestLoanMortgageNotCapacityFinancing(t *testing.T) {
	h, runner := loanCapacityFixture(t, 750_000, false)
	capacity := snapshotTestLoan()
	quoteID := "capacity-quote"
	capacity.QuoteID = &quoteID
	quote := gameapi.LoanQuote{ID: quoteID, LenderCompanyID: capacity.LenderCompanyID, BankingRegion: capacity.BankingRegion, PrincipalPence: capacity.PrincipalPence, AnnualRateBPS: *capacity.AnnualRateBPS, TermGameDays: capacity.TermGameDays, Repayment: capacity.Repayment, InstallmentPence: capacity.InstallmentPence, TotalRepayablePence: *capacity.TotalRepayablePence, CollateralShipID: capacity.CollateralShipID}
	mortgage := capacity
	mortgage.ID, mortgage.IsMortgage = "mortgage", true
	if !matchesLoanQuote(capacity, quote, h.account.CompanyID) || !loanQuoteIdentity(capacity, quote) || matchesLoanQuote(mortgage, quote, h.account.CompanyID) || loanQuoteIdentity(mortgage, quote) {
		t.Fatal("mortgage matched capacity quote identity or payment terms")
	}
	mortgageShipID := runner.stateCache.Ships[0].Ship.ID
	mortgage.CollateralShipID = &mortgageShipID
	loanJournalCapture(t, runner, []gameapi.Loan{capacity, mortgage})
	if loanReserve(mortgage) != 48_400 {
		t.Fatalf("mortgage omitted from hard reserves: %d", loanReserve(mortgage))
	}
	withMortgage, err := runner.capacitySavingsRate(h.ctx, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	runner.stateCache.Loans = []gameapi.Loan{capacity}
	withoutMortgage, err := runner.capacitySavingsRate(h.ctx, time.Now())
	if err != nil || withoutMortgage-withMortgage != 2420 {
		t.Fatalf("mortgage installment omitted from savings rate: %g vs %g, %v", withoutMortgage, withMortgage, err)
	}
	loanJournalCapture(t, runner, []gameapi.Loan{capacity, mortgage})
	mission := &store.Mission{AccountID: h.account.ID, ShipID: "__growth", Generation: uuid.NewString(), Phase: "growth_finance_repay"}
	data := missionData{Capacity: &capacityIntent{Financing: &capacityFinancing{Loans: []gameapi.Loan{mortgage}}}}
	if changed, err := runner.stageLoanRepayment(h.ctx, mission, &data, mortgage, 0); err == nil || changed || data.Action != nil {
		t.Fatalf("capacity repayment staged mortgage: %t, %v", changed, err)
	}
	if changed, err := runner.repayCapacityFinancing(h.ctx, mission, &data); err != nil || !changed || data.Action != nil {
		t.Fatalf("capacity cancellation touched mortgage: %t, %v, %+v", changed, err, data.Action)
	}
	// Arrears service remains allowed for mortgages, independently of growth.
	mortgage.ArrearsPence = 1000
	loanJournalCapture(t, runner, []gameapi.Loan{capacity, mortgage})
	for key, meta := range runner.meta {
		meta.Generation = runner.generation
		runner.meta[key] = meta
	}
	for index := range runner.stateCache.Vaults {
		runner.stateCache.Vaults[index].BalancePence = 100_000_000
	}
	var repaidID string
	h.api.server.Config.Handler = http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		if request.Method != http.MethodPost || request.URL.Path != "/v1/loan/repay" {
			t.Errorf("unexpected debt-service request: %s %s", request.Method, request.URL.Path)
			writer.WriteHeader(http.StatusBadRequest)
			return
		}
		var body gameapi.RepayLoanRequest
		if err := json.NewDecoder(request.Body).Decode(&body); err != nil {
			t.Error(err)
		}
		repaidID = body.LoanID
		result := mortgage
		result.ArrearsPence = 0
		result.InterestPaidPence = 1000
		_ = json.NewEncoder(writer).Encode(result)
	})
	if changed, err := runner.serviceLoans(h.ctx); err != nil || !changed || repaidID != mortgage.ID {
		t.Fatalf("mortgage arrears not serviced: changed %t repaid %q error %v", changed, repaidID, err)
	}
	t.Run("mortgage collateral is unavailable for liens", func(t *testing.T) {
		h, runner := loanCapacityFixture(t, 750_000, false)
		loanCapacityCashShortfall(t, h, runner, 100_000)
		port, hull, decision, required, evidence := loanCapacityDecision(t, h, runner)
		capacity, mortgage := snapshotTestLoan(), snapshotTestLoan()
		capacityShipID := "another-pledged-ship"
		mortgageShipID := runner.stateCache.Ships[0].Ship.ID
		capacity.CollateralShipID = &capacityShipID
		mortgage.ID, mortgage.IsMortgage, mortgage.CollateralShipID = "mortgage", true, &mortgageShipID
		loanJournalCapture(t, runner, []gameapi.Loan{capacity, mortgage})
		if changed, err := runner.considerCapacityFinancing(h.ctx, port, hull, decision, required, evidence); err != nil || changed {
			t.Fatalf("pledged fleet admitted another capacity loan: %t, %v", changed, err)
		}
		if h.api.mutationCount("/v1/loan/quote") != 0 {
			t.Fatal("mortgage collateral was quoted for another lien")
		}
		h.missingMission("__growth")
	})
}
