package api

import (
	"context"
	"fmt"
	"net/http"

	"github.com/google/uuid"
)

// BuyWarehouse purchases a new level 1 warehouse at the given port.
func (c *Client) BuyWarehouse(ctx context.Context, portID uuid.UUID) (*Warehouse, error) {
	req := CreateWarehouseRequest{PortID: portID}
	var warehouse Warehouse
	if err := c.do(ctx, http.MethodPost, "/warehouses", req, &warehouse, PriorityNormal); err != nil {
		return nil, err
	}
	return &warehouse, nil
}

// ListWarehouses returns all warehouses owned by the current company.
func (c *Client) ListWarehouses(ctx context.Context) ([]Warehouse, error) {
	return Paginate[Warehouse](ctx, c, "/warehouses", PriorityNormal)
}

// GetWarehouse returns details for a single warehouse.
func (c *Client) GetWarehouse(ctx context.Context, id uuid.UUID) (*Warehouse, error) {
	var warehouse Warehouse
	path := fmt.Sprintf("/warehouses/%s", id)
	if err := c.do(ctx, http.MethodGet, path, nil, &warehouse, PriorityNormal); err != nil {
		return nil, err
	}
	return &warehouse, nil
}

// GetWarehouseInventory returns the stock stored in a warehouse.
func (c *Client) GetWarehouseInventory(ctx context.Context, id uuid.UUID) ([]WarehouseInventoryItem, error) {
	path := fmt.Sprintf("/warehouses/%s/inventory", id)
	return Paginate[WarehouseInventoryItem](ctx, c, path, PriorityNormal)
}

// GrowWarehouse upgrades a warehouse to the next level, increasing capacity.
func (c *Client) GrowWarehouse(ctx context.Context, id uuid.UUID) (*Warehouse, error) {
	var warehouse Warehouse
	path := fmt.Sprintf("/warehouses/%s/grow", id)
	if err := c.do(ctx, http.MethodPost, path, nil, &warehouse, PriorityNormal); err != nil {
		return nil, err
	}
	return &warehouse, nil
}

// ShrinkWarehouse downgrades a warehouse, reducing capacity and upkeep.
func (c *Client) ShrinkWarehouse(ctx context.Context, id uuid.UUID) (*Warehouse, error) {
	var warehouse Warehouse
	path := fmt.Sprintf("/warehouses/%s/shrink", id)
	if err := c.do(ctx, http.MethodPost, path, nil, &warehouse, PriorityNormal); err != nil {
		return nil, err
	}
	return &warehouse, nil
}

// DeleteWarehouse demolishes an empty warehouse.
func (c *Client) DeleteWarehouse(ctx context.Context, id uuid.UUID) error {
	path := fmt.Sprintf("/warehouses/%s", id)
	return c.do(ctx, http.MethodDelete, path, nil, nil, PriorityNormal)
}

// TransferToShip loads cargo from a warehouse onto a ship.
// The ship must be docked at the warehouse's port.
func (c *Client) TransferToShip(ctx context.Context, warehouseID uuid.UUID, req TransferToShipRequest) error {
	path := fmt.Sprintf("/warehouses/%s/transfer-to-ship", warehouseID)
	return c.do(ctx, http.MethodPost, path, req, nil, PriorityNormal)
}
