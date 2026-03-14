package api

import (
	"context"
	"fmt"
	"net/http"

	"github.com/google/uuid"
)

// ListPorts returns all ports. Note: the list endpoint does NOT include
// embedded traders or outgoing_routes — use GetPort for those.
func (c *Client) ListPorts(ctx context.Context, filters PortFilters) ([]Port, error) {
	params := make(map[string]string)
	if filters.CountryID != nil {
		params["country_id"] = filters.CountryID.String()
	}
	if filters.IsHub != nil {
		params["is_hub"] = fmt.Sprintf("%t", *filters.IsHub)
	}

	return Paginate[Port](ctx, c, "/world/ports"+BuildQueryString(params), PriorityLow)
}

// GetPort returns a single port with embedded traders and outgoing routes.
func (c *Client) GetPort(ctx context.Context, id uuid.UUID) (*Port, error) {
	var port Port
	if err := c.do(ctx, http.MethodGet, "/world/ports/"+id.String(), nil, &port, PriorityLow); err != nil {
		return nil, err
	}
	return &port, nil
}

// GetPortShipyard returns the shipyard at a given port, or nil if the port has no shipyard.
func (c *Client) GetPortShipyard(ctx context.Context, portID uuid.UUID) (*Shipyard, error) {
	var shipyard Shipyard
	if err := c.do(ctx, http.MethodGet, "/world/ports/"+portID.String()+"/shipyard", nil, &shipyard, PriorityLow); err != nil {
		// 404 means no shipyard at this port.
		if reqErr, ok := err.(*requestError); ok && reqErr.statusCode == http.StatusNotFound {
			return nil, nil
		}
		return nil, err
	}
	return &shipyard, nil
}

// ListGoods returns all tradeable goods, optionally filtered by category.
func (c *Client) ListGoods(ctx context.Context, category string) ([]Good, error) {
	params := make(map[string]string)
	if category != "" {
		params["category"] = category
	}

	var goods []Good
	if err := c.do(ctx, http.MethodGet, "/world/goods"+BuildQueryString(params), nil, &goods, PriorityLow); err != nil {
		return nil, err
	}
	return goods, nil
}

// GetGood returns details for a single good.
func (c *Client) GetGood(ctx context.Context, id uuid.UUID) (*Good, error) {
	var good Good
	if err := c.do(ctx, http.MethodGet, "/world/goods/"+id.String(), nil, &good, PriorityLow); err != nil {
		return nil, err
	}
	return &good, nil
}

// ListRoutes returns all routes. Handles pagination automatically since
// there are ~210 routes and the API defaults to 50 per page.
func (c *Client) ListRoutes(ctx context.Context, filters RouteFilters) ([]Route, error) {
	params := make(map[string]string)
	if filters.FromID != nil {
		params["from_id"] = filters.FromID.String()
	}
	if filters.ToID != nil {
		params["to_id"] = filters.ToID.String()
	}

	return Paginate[Route](ctx, c, "/world/routes"+BuildQueryString(params), PriorityLow)
}

// GetRoute returns details for a single route.
func (c *Client) GetRoute(ctx context.Context, id uuid.UUID) (*Route, error) {
	var route Route
	if err := c.do(ctx, http.MethodGet, "/world/routes/"+id.String(), nil, &route, PriorityLow); err != nil {
		return nil, err
	}
	return &route, nil
}

// ListShipTypes returns all available ship types.
func (c *Client) ListShipTypes(ctx context.Context) ([]ShipType, error) {
	var shipTypes []ShipType
	if err := c.do(ctx, http.MethodGet, "/world/ship-types", nil, &shipTypes, PriorityLow); err != nil {
		return nil, err
	}
	return shipTypes, nil
}

// GetShipType returns details for a single ship type.
func (c *Client) GetShipType(ctx context.Context, id uuid.UUID) (*ShipType, error) {
	var shipType ShipType
	if err := c.do(ctx, http.MethodGet, "/world/ship-types/"+id.String(), nil, &shipType, PriorityLow); err != nil {
		return nil, err
	}
	return &shipType, nil
}
