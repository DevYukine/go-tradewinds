package gameapi

import (
	"context"
	"net/url"
	"strconv"
)

// GoldsmithsQuoteRequest asks for a stored ship-lien quote. PrincipalPence is
// integer pence, TermGameDays is 1–360, and Repayment is balloon or amortised.
// An empty BankingRegion uses the company's home region.
type GoldsmithsQuoteRequest struct {
	PrincipalPence int64  `json:"principal_pence"`
	TermGameDays   int32  `json:"term_game_days"`
	Repayment      string `json:"repayment"`
	ShipID         string `json:"ship_id"`
	BankingRegion  string `json:"banking_region,omitempty"`
}

// QuoteActionRequest identifies a stored quote for a journaled loan action.
type QuoteActionRequest struct {
	QuoteID string `json:"quote_id"`
}

// RepayLoanRequest pays interest first, then principal, from the loan's region.
// Omitting AmountPence repays everything currently owed.
type RepayLoanRequest struct {
	LoanID      string `json:"loan_id"`
	AmountPence *int64 `json:"amount_pence,omitempty"`
}

// GetGoldsmithsQuote creates a stored quote despite using GET. It does not
// accept the quote or repay debt; those POSTs must use the durable action journal.
func (c *Client) GetGoldsmithsQuote(ctx context.Context, request GoldsmithsQuoteRequest) (GoldsmithsQuote, error) {
	query := url.Values{
		"principal_pence": {strconv.FormatInt(request.PrincipalPence, 10)},
		"term_game_days":  {strconv.FormatInt(int64(request.TermGameDays), 10)},
		"repayment":       {request.Repayment},
		"ship_id":         {request.ShipID},
	}
	if request.BankingRegion != "" {
		query.Set("banking_region", request.BankingRegion)
	}
	var quote GoldsmithsQuote
	err := c.Get(ctx, "/v1/loan/quote?"+query.Encode(), &quote)
	return quote, err
}
