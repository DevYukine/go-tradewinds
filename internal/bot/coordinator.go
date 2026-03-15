package bot

import (
	"sync"
	"time"

	"github.com/google/uuid"
)

// Coordinator prevents multiple companies from chasing the same trade route
// or passenger. It maintains in-memory claim maps with automatic expiry.
type Coordinator struct {
	mu           sync.RWMutex
	activeRoutes map[string]uuid.UUID    // "buyPort:sellPort:good" -> shipID
	claimedPax   map[uuid.UUID]string    // passengerID -> companyGameID
	routeExpiry  map[string]time.Time    // route key -> expiry time
	paxExpiry    map[uuid.UUID]time.Time // passengerID -> expiry time
}

// NewCoordinator creates a new route/passenger coordinator.
func NewCoordinator() *Coordinator {
	return &Coordinator{
		activeRoutes: make(map[string]uuid.UUID),
		claimedPax:   make(map[uuid.UUID]string),
		routeExpiry:  make(map[string]time.Time),
		paxExpiry:    make(map[uuid.UUID]time.Time),
	}
}

// routeKey builds a deduplication key for a trade route.
func routeKey(buyPort, sellPort, goodID uuid.UUID) string {
	return buyPort.String() + ":" + sellPort.String() + ":" + goodID.String()
}

// ClaimRoute attempts to claim a trade route for a ship. Returns true if
// the claim was granted (no other ship is working this route).
func (c *Coordinator) ClaimRoute(shipID uuid.UUID, buyPort, sellPort, goodID uuid.UUID) bool {
	key := routeKey(buyPort, sellPort, goodID)
	c.mu.Lock()
	defer c.mu.Unlock()

	// Check if already claimed by another ship.
	if existingShip, ok := c.activeRoutes[key]; ok {
		if existingShip != shipID && time.Now().Before(c.routeExpiry[key]) {
			return false
		}
	}

	c.activeRoutes[key] = shipID
	c.routeExpiry[key] = time.Now().Add(15 * time.Minute)
	return true
}

// ReleaseRoute releases a route claim for a ship.
func (c *Coordinator) ReleaseRoute(shipID uuid.UUID) {
	c.mu.Lock()
	defer c.mu.Unlock()

	for key, sid := range c.activeRoutes {
		if sid == shipID {
			delete(c.activeRoutes, key)
			delete(c.routeExpiry, key)
		}
	}
}

// IsRouteClaimed checks if a route is currently claimed by any ship.
func (c *Coordinator) IsRouteClaimed(buyPort, sellPort, goodID uuid.UUID) bool {
	key := routeKey(buyPort, sellPort, goodID)
	c.mu.RLock()
	defer c.mu.RUnlock()

	expiry, ok := c.routeExpiry[key]
	if !ok {
		return false
	}
	return time.Now().Before(expiry)
}

// ClaimedRoutes returns all currently active route keys (for passing to agents).
func (c *Coordinator) ClaimedRoutes() []string {
	c.mu.RLock()
	defer c.mu.RUnlock()

	now := time.Now()
	var routes []string
	for key, expiry := range c.routeExpiry {
		if now.Before(expiry) {
			routes = append(routes, key)
		}
	}
	return routes
}

// ClaimPassenger attempts to claim a passenger for a company. Returns true
// if the claim was granted (no other company is pursuing this passenger).
func (c *Coordinator) ClaimPassenger(passengerID uuid.UUID, companyGameID string) bool {
	c.mu.Lock()
	defer c.mu.Unlock()

	if existing, ok := c.claimedPax[passengerID]; ok {
		if existing != companyGameID && time.Now().Before(c.paxExpiry[passengerID]) {
			return false
		}
	}

	c.claimedPax[passengerID] = companyGameID
	c.paxExpiry[passengerID] = time.Now().Add(10 * time.Minute)
	return true
}

// Cleanup removes expired claims. Should be called periodically.
func (c *Coordinator) Cleanup() {
	c.mu.Lock()
	defer c.mu.Unlock()

	now := time.Now()
	for key, expiry := range c.routeExpiry {
		if now.After(expiry) {
			delete(c.activeRoutes, key)
			delete(c.routeExpiry, key)
		}
	}
	for paxID, expiry := range c.paxExpiry {
		if now.After(expiry) {
			delete(c.claimedPax, paxID)
			delete(c.paxExpiry, paxID)
		}
	}
}
