package bot

import (
	"sync"
	"time"

	"github.com/google/uuid"

	"github.com/DevYukine/go-tradewinds/internal/api"
	"github.com/DevYukine/go-tradewinds/internal/db"
)

// CompanyState holds the in-memory state for a single company.
// It is updated from API responses and SSE events.
type CompanyState struct {
	CompanyID   uuid.UUID
	Treasury    int64
	Reputation  int64
	Ships       map[uuid.UUID]*ShipState
	Warehouses  map[uuid.UUID]*WarehouseState
	TotalUpkeep int64
	LastEconomy time.Time
	dbID        uint // Database record ID for logging.

	// Cumulative P&L counters — updated incrementally by trade/passenger logging
	// so recordPnLSnapshot avoids full-table SUM queries every tick.
	CumTradeRev       int64 // SUM(total_price) for sell trades
	CumTradeCosts     int64 // SUM(total_price) for buy trades
	CumPassengerRev   int64 // SUM(bid) for passenger boardings
	CumShipCosts      int64 // Total spent on ship purchases
	CumWarehouseCosts int64 // Total spent on warehouse purchases
	InitialTreasury   int64 // Treasury at bot start, for upkeep derivation
	pnlInitialized    bool  // Whether cumulative counters have been seeded from DB.
	Initialized       bool  // Whether initState has completed (safe to read live values).

	// Tunable trading parameters from optimizer.
	Params *db.CompanyParams

	// Active P2P market orders placed by this company.
	Orders map[uuid.UUID]*api.Order

	mu sync.RWMutex
}

// NewCompanyState creates an empty state for the given company.
func NewCompanyState(companyID uuid.UUID) *CompanyState {
	return &CompanyState{
		CompanyID:  companyID,
		Ships:      make(map[uuid.UUID]*ShipState),
		Warehouses: make(map[uuid.UUID]*WarehouseState),
		Orders:     make(map[uuid.UUID]*api.Order),
	}
}

// UpdateEconomy refreshes financial state from an API economy response.
func (s *CompanyState) UpdateEconomy(econ *api.CompanyEconomy) {
	s.mu.Lock()
	defer s.mu.Unlock()

	s.Treasury = econ.Treasury
	s.Reputation = econ.Reputation
	s.TotalUpkeep = econ.TotalUpkeep
	s.LastEconomy = time.Now()
}

// UpdateShips replaces the full ship roster from an API response.
func (s *CompanyState) UpdateShips(ships []api.Ship) {
	s.mu.Lock()
	defer s.mu.Unlock()

	updated := make(map[uuid.UUID]*ShipState, len(ships))
	for i := range ships {
		ship := &ships[i]
		if existing, ok := s.Ships[ship.ID]; ok {
			existing.Ship = *ship
			if existing.CargoCosts == nil {
				existing.CargoCosts = make(map[uuid.UUID]int)
			}
			updated[ship.ID] = existing
		} else {
			updated[ship.ID] = &ShipState{
				Ship:       *ship,
				CargoCosts: make(map[uuid.UUID]int),
			}
		}
	}
	s.Ships = updated
}

// SetShipCargo updates the cargo for a specific ship.
func (s *CompanyState) SetShipCargo(shipID uuid.UUID, cargo []api.Cargo) {
	s.mu.Lock()
	defer s.mu.Unlock()

	if ship, ok := s.Ships[shipID]; ok {
		ship.Cargo = cargo
	}
}

// SetShipPassengers updates the boarded passenger count for a specific ship.
func (s *CompanyState) SetShipPassengers(shipID uuid.UUID, count int) {
	s.mu.Lock()
	defer s.mu.Unlock()

	if ship, ok := s.Ships[shipID]; ok {
		ship.PassengerCount = count
	}
}

// UpdateWarehouses replaces the full warehouse roster from an API response.
func (s *CompanyState) UpdateWarehouses(warehouses []api.Warehouse) {
	s.mu.Lock()
	defer s.mu.Unlock()

	updated := make(map[uuid.UUID]*WarehouseState, len(warehouses))
	for i := range warehouses {
		wh := &warehouses[i]
		if existing, ok := s.Warehouses[wh.ID]; ok {
			existing.Warehouse = *wh
			if existing.ItemCosts == nil {
				existing.ItemCosts = make(map[uuid.UUID]int)
			}
			updated[wh.ID] = existing
		} else {
			updated[wh.ID] = &WarehouseState{
				Warehouse: *wh,
				ItemCosts: make(map[uuid.UUID]int),
			}
		}
	}
	s.Warehouses = updated
}

// SetWarehouseInventory updates the inventory for a specific warehouse.
func (s *CompanyState) SetWarehouseInventory(warehouseID uuid.UUID, items []api.WarehouseInventoryItem) {
	s.mu.Lock()
	defer s.mu.Unlock()

	if wh, ok := s.Warehouses[warehouseID]; ok {
		wh.Inventory = items
	}
}

// AddWarehouse adds a newly purchased warehouse to the in-memory state.
func (s *CompanyState) AddWarehouse(wh api.Warehouse) {
	s.mu.Lock()
	defer s.mu.Unlock()

	s.Warehouses[wh.ID] = &WarehouseState{
		Warehouse: wh,
		ItemCosts: make(map[uuid.UUID]int),
	}
}

// RemoveWarehouse removes a demolished warehouse from the in-memory state.
func (s *CompanyState) RemoveWarehouse(id uuid.UUID) {
	s.mu.Lock()
	defer s.mu.Unlock()

	delete(s.Warehouses, id)
}

// UpdateOrders replaces the full active order set from an API response.
func (s *CompanyState) UpdateOrders(orders []api.Order) {
	s.mu.Lock()
	defer s.mu.Unlock()

	updated := make(map[uuid.UUID]*api.Order, len(orders))
	for i := range orders {
		o := &orders[i]
		updated[o.ID] = o
	}
	s.Orders = updated
}

// RemoveOrder deletes a single order from the tracked set (filled/cancelled/expired).
func (s *CompanyState) RemoveOrder(orderID uuid.UUID) {
	s.mu.Lock()
	defer s.mu.Unlock()
	delete(s.Orders, orderID)
}

// GetOrders returns a snapshot of all active orders.
func (s *CompanyState) GetOrders() []*api.Order {
	s.mu.RLock()
	defer s.mu.RUnlock()

	out := make([]*api.Order, 0, len(s.Orders))
	for _, o := range s.Orders {
		cp := *o
		out = append(out, &cp)
	}
	return out
}

// GetShip returns a copy of a ship's state, or nil if not found.
func (s *CompanyState) GetShip(shipID uuid.UUID) *ShipState {
	s.mu.RLock()
	defer s.mu.RUnlock()

	if ship, ok := s.Ships[shipID]; ok {
		cp := *ship
		return &cp
	}
	return nil
}

// DockedShips returns all ships currently docked at any port.
func (s *CompanyState) DockedShips() []*ShipState {
	s.mu.RLock()
	defer s.mu.RUnlock()

	var docked []*ShipState
	for _, ship := range s.Ships {
		if ship.Ship.Status == "docked" {
			cp := *ship
			docked = append(docked, &cp)
		}
	}
	return docked
}

// AddTradeRevenue atomically increments cumulative trade revenue (sell).
func (s *CompanyState) AddTradeRevenue(amount int64) {
	s.mu.Lock()
	s.CumTradeRev += amount
	s.mu.Unlock()
}

// AddTradeCost atomically increments cumulative trade costs (buy).
func (s *CompanyState) AddTradeCost(amount int64) {
	s.mu.Lock()
	s.CumTradeCosts += amount
	s.mu.Unlock()
}

// AddPassengerRevenue atomically increments cumulative passenger revenue.
func (s *CompanyState) AddPassengerRevenue(amount int64) {
	s.mu.Lock()
	s.CumPassengerRev += amount
	s.mu.Unlock()
}

// AddShipCost atomically increments cumulative ship purchase costs.
func (s *CompanyState) AddShipCost(amount int64) {
	s.mu.Lock()
	s.CumShipCosts += amount
	s.mu.Unlock()
}

// AddWarehouseCost atomically increments cumulative warehouse purchase costs.
func (s *CompanyState) AddWarehouseCost(amount int64) {
	s.mu.Lock()
	s.CumWarehouseCosts += amount
	s.mu.Unlock()
}

// TreasuryFloor returns the minimum treasury to maintain (2x total upkeep).
func (s *CompanyState) TreasuryFloor() int64 {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.TotalUpkeep * 2
}

// SetDBID sets the database record ID for this company.
func (s *CompanyState) SetDBID(id uint) {
	s.dbID = id
}

// CompanyDBID returns the database record ID.
func (s *CompanyState) CompanyDBID() uint {
	return s.dbID
}

// Lock acquires a write lock on the state.
func (s *CompanyState) Lock() {
	s.mu.Lock()
}

// Unlock releases the write lock on the state.
func (s *CompanyState) Unlock() {
	s.mu.Unlock()
}

// RLock acquires a read lock on the state.
func (s *CompanyState) RLock() {
	s.mu.RLock()
}

// RUnlock releases the read lock on the state.
func (s *CompanyState) RUnlock() {
	s.mu.RUnlock()
}

// ShipState tracks a single ship and its cargo.
type ShipState struct {
	Ship           api.Ship
	Cargo          []api.Cargo
	CargoCosts     map[uuid.UUID]int // goodID → avg unit cost (price + tax), for loss prevention
	PassengerCount int               // currently boarded passengers
	IdleTicks      int               // consecutive "wait" actions while docked (reset on trade/sail)
}

// SetCargoCost records or updates the weighted-average cost basis for a good on this ship.
// If the ship already has some of this good at a different cost, a weighted average is used.
func (ss *ShipState) SetCargoCost(goodID uuid.UUID, newQty, newUnitCost int) {
	if ss.CargoCosts == nil {
		ss.CargoCosts = make(map[uuid.UUID]int)
	}
	if newQty <= 0 || newUnitCost <= 0 {
		return
	}

	existingCost, exists := ss.CargoCosts[goodID]
	if !exists {
		ss.CargoCosts[goodID] = newUnitCost
		return
	}

	// Find existing quantity of this good on ship.
	existingQty := 0
	for _, c := range ss.Cargo {
		if c.GoodID == goodID {
			existingQty += c.Quantity
		}
	}

	if existingQty <= 0 {
		ss.CargoCosts[goodID] = newUnitCost
		return
	}

	// Weighted average: (existingQty*existingCost + newQty*newCost) / totalQty
	totalQty := existingQty + newQty
	ss.CargoCosts[goodID] = (existingQty*existingCost + newQty*newUnitCost) / totalQty
}

// ClearCargoCost removes cost tracking for a good (e.g. after selling all of it).
func (ss *ShipState) ClearCargoCost(goodID uuid.UUID) {
	delete(ss.CargoCosts, goodID)
}

// UsedCapacity returns the total quantity of cargo loaded on this ship.
func (ss *ShipState) UsedCapacity() int {
	total := 0
	for _, c := range ss.Cargo {
		total += c.Quantity
	}
	return total
}

// WarehouseState tracks a single warehouse and its inventory.
type WarehouseState struct {
	Warehouse  api.Warehouse
	Inventory  []api.WarehouseInventoryItem
	ItemCosts  map[uuid.UUID]int // goodID → avg unit cost (price + tax)
}

// SetItemCost records or updates the cost basis for a good in this warehouse.
func (ws *WarehouseState) SetItemCost(goodID uuid.UUID, newQty, newUnitCost int) {
	if ws.ItemCosts == nil {
		ws.ItemCosts = make(map[uuid.UUID]int)
	}
	if newQty <= 0 || newUnitCost <= 0 {
		return
	}

	existingCost, exists := ws.ItemCosts[goodID]
	if !exists {
		ws.ItemCosts[goodID] = newUnitCost
		return
	}

	// Find existing quantity of this good in warehouse.
	existingQty := 0
	for _, item := range ws.Inventory {
		if item.GoodID == goodID {
			existingQty += item.Quantity
		}
	}

	if existingQty <= 0 {
		ws.ItemCosts[goodID] = newUnitCost
		return
	}

	// Weighted average.
	totalQty := existingQty + newQty
	ws.ItemCosts[goodID] = (existingQty*existingCost + newQty*newUnitCost) / totalQty
}

// ClearItemCost removes cost tracking for a good.
func (ws *WarehouseState) ClearItemCost(goodID uuid.UUID) {
	delete(ws.ItemCosts, goodID)
}

// GetItemCost returns the cost basis for a good, or 0 if unknown.
func (ws *WarehouseState) GetItemCost(goodID uuid.UUID) int {
	if ws.ItemCosts == nil {
		return 0
	}
	return ws.ItemCosts[goodID]
}
