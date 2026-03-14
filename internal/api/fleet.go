package api

import (
	"context"
	"fmt"
	"net/http"

	"github.com/google/uuid"
)

// ListShips returns all ships owned by the current company.
func (c *Client) ListShips(ctx context.Context) ([]Ship, error) {
	return Paginate[Ship](ctx, c, "/ships", PriorityNormal)
}

// GetShip returns details for a single ship.
func (c *Client) GetShip(ctx context.Context, id uuid.UUID) (*Ship, error) {
	var ship Ship
	path := fmt.Sprintf("/ships/%s", id)
	if err := c.do(ctx, http.MethodGet, path, nil, &ship, PriorityNormal); err != nil {
		return nil, err
	}
	return &ship, nil
}

// RenameShip changes the name of a ship.
func (c *Client) RenameShip(ctx context.Context, id uuid.UUID, name string) (*Ship, error) {
	req := RenameShipRequest{Name: name}
	var ship Ship
	path := fmt.Sprintf("/ships/%s", id)
	if err := c.do(ctx, http.MethodPatch, path, req, &ship, PriorityLow); err != nil {
		return nil, err
	}
	return &ship, nil
}

// GetShipInventory returns the cargo loaded on a ship.
func (c *Client) GetShipInventory(ctx context.Context, id uuid.UUID) ([]Cargo, error) {
	var cargo []Cargo
	path := fmt.Sprintf("/ships/%s/inventory", id)
	if err := c.do(ctx, http.MethodGet, path, nil, &cargo, PriorityNormal); err != nil {
		return nil, err
	}
	return cargo, nil
}

// SendTransit sends a docked ship sailing on a route. The ship must be docked
// and the route must originate from the ship's current port.
func (c *Client) SendTransit(ctx context.Context, shipID, routeID uuid.UUID) (*Ship, error) {
	req := TransitRequest{RouteID: routeID}
	var ship Ship
	path := fmt.Sprintf("/ships/%s/transit", shipID)
	if err := c.do(ctx, http.MethodPost, path, req, &ship, PriorityHigh); err != nil {
		return nil, err
	}
	return &ship, nil
}

// GetTransitLogs returns the travel history for a ship.
func (c *Client) GetTransitLogs(ctx context.Context, shipID uuid.UUID) ([]TransitLog, error) {
	path := fmt.Sprintf("/ships/%s/transit-logs", shipID)
	return Paginate[TransitLog](ctx, c, path, PriorityLow)
}

// TransferToWarehouse offloads cargo from a ship to a warehouse.
// The ship must be docked and the warehouse must be at the same port.
func (c *Client) TransferToWarehouse(ctx context.Context, shipID uuid.UUID, req TransferToWarehouseRequest) error {
	path := fmt.Sprintf("/ships/%s/transfer-to-warehouse", shipID)
	return c.do(ctx, http.MethodPost, path, req, nil, PriorityNormal)
}

// SellShip decommissions (scuttles) a ship, removing it from the fleet.
// The ship must be docked and have no cargo. Returns error if the game API
// does not support this operation.
func (c *Client) SellShip(ctx context.Context, shipID uuid.UUID) error {
	path := fmt.Sprintf("/ships/%s", shipID)
	return c.do(ctx, http.MethodDelete, path, nil, nil, PriorityNormal)
}
