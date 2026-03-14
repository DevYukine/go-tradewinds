package api

import (
	"context"
	"fmt"
	"net/http"

	"github.com/google/uuid"
)

// ListPassengers returns passengers matching the given filters.
// Use status "available" to find passengers waiting at ports,
// or "boarded" to find passengers already on ships.
func (c *Client) ListPassengers(ctx context.Context, filters PassengerFilters) ([]Passenger, error) {
	params := make(map[string]string)
	if filters.Status != "" {
		params["status"] = filters.Status
	}
	if filters.PortID != "" {
		params["port_id"] = filters.PortID
	}
	if filters.ShipID != "" {
		params["ship_id"] = filters.ShipID
	}

	path := "/passengers" + BuildQueryString(params)
	return Paginate[Passenger](ctx, c, path, PriorityNormal)
}

// GetPassenger returns a single passenger by ID.
func (c *Client) GetPassenger(ctx context.Context, id uuid.UUID) (*Passenger, error) {
	var passenger Passenger
	path := fmt.Sprintf("/passengers/%s", id)
	if err := c.do(ctx, http.MethodGet, path, nil, &passenger, PriorityNormal); err != nil {
		return nil, err
	}
	return &passenger, nil
}

// BoardPassenger boards a passenger onto a ship. The ship must be docked
// at the passenger's origin port and have available passenger capacity.
func (c *Client) BoardPassenger(ctx context.Context, passengerID, shipID uuid.UUID) (*Passenger, error) {
	req := BoardPassengerRequest{ShipID: shipID}
	var passenger Passenger
	path := fmt.Sprintf("/passengers/%s/board", passengerID)
	if err := c.do(ctx, http.MethodPost, path, req, &passenger, PriorityHigh); err != nil {
		return nil, err
	}
	return &passenger, nil
}
