package api

import (
	"context"
	"fmt"
	"net/http"

	"github.com/google/uuid"
)

// ListOrders returns open player-to-player market orders.
// Supports filtering by port, good, and side. No auth required.
func (c *Client) ListOrders(ctx context.Context, filters OrderFilters) ([]Order, error) {
	params := make(map[string]string)
	if filters.Side != "" {
		params["side"] = filters.Side
	}

	path := "/market/orders" + BuildQueryString(params)

	// Port and good ID array filters need special handling.
	if len(filters.PortIDs) > 0 || len(filters.GoodIDs) > 0 {
		sep := "?"
		if len(params) > 0 {
			sep = "&"
		}
		arrayParams := ""
		for _, id := range filters.PortIDs {
			arrayParams += sep + "port_ids[]=" + id.String()
			sep = "&"
		}
		for _, id := range filters.GoodIDs {
			arrayParams += sep + "good_ids[]=" + id.String()
			sep = "&"
		}
		path += arrayParams
	}

	return Paginate[Order](ctx, c, path, PriorityNormal)
}

// PostOrder creates a new buy or sell order on the player market.
// Costs a listing fee and requires reputation.
func (c *Client) PostOrder(ctx context.Context, req CreateOrderRequest) (*Order, error) {
	var order Order
	if err := c.do(ctx, http.MethodPost, "/market/orders", req, &order, PriorityNormal); err != nil {
		return nil, err
	}
	return &order, nil
}

// FillOrder fills another player's order. Partial fills are allowed.
func (c *Client) FillOrder(ctx context.Context, orderID uuid.UUID, quantity int) (*Order, error) {
	req := FillOrderRequest{Quantity: quantity}
	var order Order
	path := fmt.Sprintf("/market/orders/%s/fill", orderID)
	if err := c.do(ctx, http.MethodPost, path, req, &order, PriorityHigh); err != nil {
		return nil, err
	}
	return &order, nil
}

// CancelOrder cancels an open order. May incur a penalty.
func (c *Client) CancelOrder(ctx context.Context, orderID uuid.UUID) error {
	path := fmt.Sprintf("/market/orders/%s", orderID)
	return c.do(ctx, http.MethodDelete, path, nil, nil, PriorityNormal)
}

// GetBlendedPrice calculates the cost of filling a quantity across multiple
// market orders. No auth required.
func (c *Client) GetBlendedPrice(ctx context.Context, portID, goodID uuid.UUID, side string, quantity int) (*BlendedPriceResponse, error) {
	params := BuildQueryString(map[string]string{
		"port_id":  portID.String(),
		"good_id":  goodID.String(),
		"side":     side,
		"quantity": itoa(quantity),
	})

	var resp BlendedPriceResponse
	if err := c.do(ctx, http.MethodGet, "/market/blended-price"+params, nil, &resp, PriorityLow); err != nil {
		return nil, err
	}
	return &resp, nil
}
