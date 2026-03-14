package api

import (
	"context"
	"net/http"

	"github.com/google/uuid"
)

// ListTraders returns all NPC traders (one per port).
func (c *Client) ListTraders(ctx context.Context) ([]Trader, error) {
	return Paginate[Trader](ctx, c, "/trade/traders", PriorityLow)
}

// ListTraderPositions returns all stock positions for a given trader.
// Handles pagination automatically (~200 total positions across all traders).
func (c *Client) ListTraderPositions(ctx context.Context, traderID uuid.UUID) ([]TraderPosition, error) {
	query := BuildQueryString(map[string]string{
		"trader_id": traderID.String(),
	})
	return Paginate[TraderPosition](ctx, c, "/trade/trader-positions"+query, PriorityLow)
}

// ListAllTraderPositions returns all trader positions across all traders.
func (c *Client) ListAllTraderPositions(ctx context.Context) ([]TraderPosition, error) {
	return Paginate[TraderPosition](ctx, c, "/trade/trader-positions", PriorityLow)
}

// GetQuote requests a price quote for buying or selling a good at a port.
// The returned quote contains a signed token that must be executed within 120 seconds.
func (c *Client) GetQuote(ctx context.Context, req QuoteRequest) (*QuoteResponse, error) {
	var resp QuoteResponse
	if err := c.do(ctx, http.MethodPost, "/trade/quote", req, &resp, PriorityHigh); err != nil {
		return nil, err
	}
	return &resp, nil
}

// ExecuteQuote executes a previously obtained quote using its signed token.
// Destinations specify where bought goods go or where sold goods come from.
func (c *Client) ExecuteQuote(ctx context.Context, req ExecuteQuoteRequest) (*TradeExecution, error) {
	var execution TradeExecution
	if err := c.do(ctx, http.MethodPost, "/trade/quotes/execute", req, &execution, PriorityHigh); err != nil {
		return nil, err
	}
	return &execution, nil
}

// DirectTrade executes a trade without a prior quote. The price may differ
// from what a quote would have returned. Convenient but less predictable.
func (c *Client) DirectTrade(ctx context.Context, req ExecuteTradeRequest) (*TradeExecution, error) {
	var execution TradeExecution
	if err := c.do(ctx, http.MethodPost, "/trade/execute", req, &execution, PriorityHigh); err != nil {
		return nil, err
	}
	return &execution, nil
}

// BatchQuotes requests multiple quotes in a single API call.
// Each result contains either a successful quote or an error message.
func (c *Client) BatchQuotes(ctx context.Context, requests []QuoteRequest) ([]BatchQuoteResult, error) {
	req := BatchQuoteRequest{Requests: requests}
	var results []BatchQuoteResult
	if err := c.do(ctx, http.MethodPost, "/trade/quotes/batch", req, &results, PriorityHigh); err != nil {
		return nil, err
	}
	return results, nil
}

// BatchExecuteQuotes executes multiple quotes in a single API call.
func (c *Client) BatchExecuteQuotes(ctx context.Context, requests []ExecuteQuoteRequest) ([]BatchExecuteResult, error) {
	req := BatchExecuteQuoteRequest{Requests: requests}
	var results []BatchExecuteResult
	if err := c.do(ctx, http.MethodPost, "/trade/quotes/execute/batch", req, &results, PriorityHigh); err != nil {
		return nil, err
	}
	return results, nil
}
