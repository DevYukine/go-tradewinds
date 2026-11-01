package engine

import (
	"encoding/json"
	"net/http"
	"time"

	"github.com/DevYukine/go-tradewinds/internal/gameapi"
	"github.com/google/uuid"
)

func (f *fakeGameAPI) handleLoanGet(w http.ResponseWriter, req *http.Request) bool {
	if req.Method != http.MethodGet {
		return false
	}
	switch req.URL.Path {
	case "/v1/loan/requests":
		if req.URL.Query().Get("mine") != "true" || req.URL.Query().Get("limit") != "50" {
			f.t.Error("own loan capture omitted mine=true&limit=50")
		}
		f.writeJSON(w, http.StatusOK, append([]gameapi.LoanRequestView{}, f.loanRequests...))
	case "/v1/loan/list":
		rows := make([]gameapi.Loan, 0)
		for _, loan := range f.playerLoans {
			if status := req.URL.Query().Get("status"); status == "" || status == loan.Status {
				rows = append(rows, loan)
			}
		}
		f.writeJSON(w, http.StatusOK, rows)
	case "/v1/fleet/ship-listings":
		f.writeJSON(w, http.StatusOK, append([]map[string]any{}, f.shipListings...))
	default:
		return false
	}
	return true
}

func fakeLoanRegion(region string) string {
	switch region {
	case "north_sea":
		return "north"
	case "channel":
		return "south"
	default:
		return region
	}
}

func (f *fakeGameAPI) handlePlayerLoan(w http.ResponseWriter, req *http.Request, body []byte) {
	mode := f.loanApproveMode
	if req.URL.Path == "/v1/loan/request" {
		mode = f.loanRequestMode
	}
	status := http.StatusOK
	switch mode {
	case "conflict":
		status = http.StatusConflict
	case "unauthorized":
		status = http.StatusUnauthorized
	case "forbidden":
		status = http.StatusForbidden
	case "rate_limited":
		status = http.StatusTooManyRequests
	}
	if status != http.StatusOK {
		f.record(req, body, status)
		if status == http.StatusTooManyRequests {
			w.Header().Set("Retry-After", "1")
		}
		f.writeProblem(w, status, "loan_refused", "loan endpoint refused")
		return
	}
	key := req.Header.Get("Idempotency-Key")
	if response := f.loanReceipts[key]; key != "" && response != nil {
		if req.URL.Path == "/v1/loan/request" {
			status = http.StatusCreated
		}
		f.record(req, body, status)
		f.writeJSON(w, status, response)
		return
	}
	var response any
	switch req.URL.Path {
	case "/v1/loan/request":
		var payload gameapi.PostLoanRequest
		if json.Unmarshal(body, &payload) != nil {
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		for _, request := range f.loanRequests {
			if request.Status == "open" {
				f.record(req, body, http.StatusConflict)
				f.writeProblem(w, http.StatusConflict, "open_request", "already open")
				return
			}
		}
		now := time.Now().UTC()
		request := gameapi.LoanRequestView{
			ID: uuid.NewString(), BorrowerCompanyID: recoveryCompanyID,
			BorrowerName: "The Pokemon Pirates", BorrowerRecord: json.RawMessage(`{}`),
			BankingRegion: payload.BankingRegion, PrincipalPence: payload.PrincipalPence,
			TermGameDays: payload.TermGameDays, Repayment: payload.Repayment,
			CollateralKind: payload.CollateralKind, Recourse: payload.Recourse,
			AutoQuote: payload.AutoQuote, GoldsmithsRateBPS: 3_000,
			RateFloorBPS: 300, RateCeilingBPS: 9_000, Status: "open",
			CreatedAt: now, ExpiresAt: now.Add(time.Duration(payload.OpenGameHours) * time.Hour),
			Quotes: []gameapi.LoanQuote{},
		}
		if payload.ShipID != "" {
			request.CollateralShipID = &payload.ShipID
		}
		f.loanRequests = append(f.loanRequests, request)
		response, status = request, http.StatusCreated
	case "/v1/loan/approve":
		var payload gameapi.QuoteActionRequest
		_ = json.Unmarshal(body, &payload)
		response = f.approvePlayerQuote(payload.QuoteID)
	case "/v1/loan/repay":
		var payload gameapi.RepayLoanRequest
		_ = json.Unmarshal(body, &payload)
		for index := range f.playerLoans {
			loan := &f.playerLoans[index]
			if loan.ID != payload.LoanID {
				continue
			}
			amount := loan.OwedPence
			if payload.AmountPence != nil {
				amount = min(amount, *payload.AmountPence)
			}
			f.vaultBalance[fakeLoanRegion(loan.BankingRegion)] -= amount
			principal := min(amount, loan.OutstandingPrincipalPence)
			loan.PrincipalRepaidPence += principal
			loan.OutstandingPrincipalPence -= principal
			loan.OwedPence -= amount
			if loan.OwedPence == 0 {
				loan.Status, loan.CollateralStatus = "repaid", "released"
			}
			response = *loan
		}
	}
	if response == nil {
		f.record(req, body, http.StatusConflict)
		f.writeProblem(w, http.StatusConflict, "quote_unavailable", "no open quote or loan")
		return
	}
	raw, _ := json.Marshal(response)
	if f.loanReceipts == nil {
		f.loanReceipts = make(map[string]json.RawMessage)
	}
	f.loanReceipts[key] = raw
	f.record(req, body, status)
	if mode == "drop" {
		if hijacker, ok := w.(http.Hijacker); ok {
			connection, _, err := hijacker.Hijack()
			if err == nil {
				_ = connection.Close()
				return
			}
		}
	}
	f.writeJSON(w, status, response)
}

func (f *fakeGameAPI) approvePlayerQuote(quoteID string) any {
	for index := range f.loanRequests {
		request := &f.loanRequests[index]
		for quoteIndex := range request.Quotes {
			quote := &request.Quotes[quoteIndex]
			if quote.ID != quoteID || quote.Status != "open" || request.Status != "open" {
				continue
			}
			rate := quote.AnnualRateBPS
			loan := gameapi.Loan{
				ID: uuid.NewString(), LenderCompanyID: quote.LenderCompanyID,
				BorrowerCompanyID: request.BorrowerCompanyID, BankingRegion: request.BankingRegion,
				PrincipalPence: quote.PrincipalPence, RateBasis: "annual", AnnualRateBPS: &rate,
				TermGameDays: quote.TermGameDays, Repayment: quote.Repayment,
				TotalRepayablePence: &quote.TotalRepayablePence, InstallmentPence: quote.InstallmentPence,
				NextPaymentPence: quote.InstallmentPence, Recourse: request.Recourse,
				CollateralKind: request.CollateralKind, CollateralShipID: request.CollateralShipID,
				CollateralStatus: "pledged", Status: "active",
				OutstandingPrincipalPence: quote.PrincipalPence, OwedPence: quote.PrincipalPence,
				RequestID: &request.ID, QuoteID: &quote.ID, LendingLineID: quote.LendingLineID,
				CreatedAt: time.Now().UTC(),
			}
			if loan.CollateralKind == "unsecured" {
				loan.CollateralStatus = "none"
			}
			request.Status, request.LoanID = "funded", &loan.ID
			quote.Status, quote.LoanID = "approved", &loan.ID
			f.playerLoans = append(f.playerLoans, loan)
			f.vaultBalance[fakeLoanRegion(loan.BankingRegion)] += loan.PrincipalPence
			return loan
		}
	}
	return nil
}
