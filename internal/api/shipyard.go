package api

import (
	"context"
	"fmt"
	"net/http"

	"github.com/google/uuid"
)

// FindShipyard returns the shipyard at a given port, or nil if none exists.
// This is an alias for GetPortShipyard for discoverability.
func (c *Client) FindShipyard(ctx context.Context, portID uuid.UUID) (*Shipyard, error) {
	return c.GetPortShipyard(ctx, portID)
}

// GetShipyardInventory returns the ships available for purchase at a shipyard.
func (c *Client) GetShipyardInventory(ctx context.Context, shipyardID uuid.UUID) ([]ShipyardInventoryItem, error) {
	path := fmt.Sprintf("/shipyards/%s/inventory", shipyardID)
	var inventory []ShipyardInventoryItem
	if err := c.do(ctx, http.MethodGet, path, nil, &inventory, PriorityNormal); err != nil {
		return nil, err
	}
	return inventory, nil
}

// BuyShip purchases a ship of the given type from a shipyard.
// The cost is deducted from the company treasury plus port tax.
// The ship appears docked at the shipyard's port.
func (c *Client) BuyShip(ctx context.Context, shipyardID, shipTypeID uuid.UUID) (*Ship, error) {
	req := PurchaseShipRequest{ShipTypeID: shipTypeID}
	var ship Ship
	path := fmt.Sprintf("/shipyards/%s/purchase", shipyardID)
	if err := c.do(ctx, http.MethodPost, path, req, &ship, PriorityNormal); err != nil {
		return nil, err
	}
	return &ship, nil
}
