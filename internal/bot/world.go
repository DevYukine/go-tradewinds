package bot

import (
	"context"
	"sync"
	"time"

	"github.com/google/uuid"
	"go.uber.org/zap"

	"github.com/DevYukine/go-tradewinds/internal/agent"
	"github.com/DevYukine/go-tradewinds/internal/api"
)

// WorldCache stores static world data (ports, goods, routes, ship types)
// that rarely changes. Loaded once at startup and shared across all companies.
type WorldCache struct {
	Ports     []api.Port
	Goods     []api.Good
	Routes    []api.Route
	ShipTypes []api.ShipType

	// ShipyardPorts lists port IDs that have shipyards. Not all ports do.
	ShipyardPorts []uuid.UUID

	// Indexes for fast lookup.
	portsByID      map[uuid.UUID]*api.Port
	goodsByID      map[uuid.UUID]*api.Good
	shipTypesByID  map[uuid.UUID]*api.ShipType
	routesByID     map[uuid.UUID]*api.Route
	routesByFrom   map[uuid.UUID][]api.Route
	routesByPorts  map[[2]uuid.UUID]*api.Route // key: [fromID, toID]
}

// LoadWorldData fetches all static world data from the API and builds indexes.
func LoadWorldData(ctx context.Context, client *api.Client, logger *zap.Logger) (*WorldCache, error) {
	log := logger.Named("world_cache")
	log.Info("loading world data")

	ports, err := client.ListPorts(ctx, api.PortFilters{})
	if err != nil {
		return nil, err
	}

	goods, err := client.ListGoods(ctx, "")
	if err != nil {
		return nil, err
	}

	routes, err := client.ListRoutes(ctx, api.RouteFilters{})
	if err != nil {
		return nil, err
	}

	shipTypes, err := client.ListShipTypes(ctx)
	if err != nil {
		return nil, err
	}

	wc := &WorldCache{
		Ports:         ports,
		Goods:         goods,
		Routes:        routes,
		ShipTypes:     shipTypes,
		portsByID:      make(map[uuid.UUID]*api.Port, len(ports)),
		goodsByID:      make(map[uuid.UUID]*api.Good, len(goods)),
		shipTypesByID:  make(map[uuid.UUID]*api.ShipType, len(shipTypes)),
		routesByID:     make(map[uuid.UUID]*api.Route, len(routes)),
		routesByFrom:   make(map[uuid.UUID][]api.Route),
		routesByPorts:  make(map[[2]uuid.UUID]*api.Route),
	}

	for i := range ports {
		wc.portsByID[ports[i].ID] = &ports[i]
	}
	for i := range goods {
		wc.goodsByID[goods[i].ID] = &goods[i]
	}
	for i := range shipTypes {
		wc.shipTypesByID[shipTypes[i].ID] = &shipTypes[i]
	}
	for i := range routes {
		wc.routesByID[routes[i].ID] = &routes[i]
		wc.routesByFrom[routes[i].FromID] = append(wc.routesByFrom[routes[i].FromID], routes[i])
		wc.routesByPorts[[2]uuid.UUID{routes[i].FromID, routes[i].ToID}] = &routes[i]
	}

	// Discover which ports have shipyards. Not all ports do.
	for _, port := range ports {
		shipyard, err := client.GetPortShipyard(ctx, port.ID)
		if err != nil {
			log.Debug("error checking shipyard at port",
				zap.String("port", port.Name),
				zap.Error(err),
			)
			continue
		}
		if shipyard != nil {
			wc.ShipyardPorts = append(wc.ShipyardPorts, port.ID)
		}
	}

	log.Info("world data loaded",
		zap.Int("ports", len(ports)),
		zap.Int("goods", len(goods)),
		zap.Int("routes", len(routes)),
		zap.Int("ship_types", len(shipTypes)),
		zap.Int("shipyard_ports", len(wc.ShipyardPorts)),
	)

	return wc, nil
}

// GetPort returns a port by ID, or nil if not found.
func (wc *WorldCache) GetPort(id uuid.UUID) *api.Port {
	return wc.portsByID[id]
}

// GetGood returns a good by ID, or nil if not found.
func (wc *WorldCache) GetGood(id uuid.UUID) *api.Good {
	return wc.goodsByID[id]
}

// GetShipType returns a ship type by ID, or nil if not found.
func (wc *WorldCache) GetShipType(id uuid.UUID) *api.ShipType {
	return wc.shipTypesByID[id]
}

// GetRoute returns a route by ID, or nil if not found.
func (wc *WorldCache) GetRoute(id uuid.UUID) *api.Route {
	return wc.routesByID[id]
}

// RoutesFrom returns all routes departing from the given port.
func (wc *WorldCache) RoutesFrom(portID uuid.UUID) []api.Route {
	return wc.routesByFrom[portID]
}

// FindRoute returns the route between two ports, or nil if none exists.
func (wc *WorldCache) FindRoute(fromID, toID uuid.UUID) *api.Route {
	return wc.routesByPorts[[2]uuid.UUID{fromID, toID}]
}

// PortIDs returns all port UUIDs for iteration.
func (wc *WorldCache) PortIDs() []uuid.UUID {
	ids := make([]uuid.UUID, len(wc.Ports))
	for i := range wc.Ports {
		ids[i] = wc.Ports[i].ID
	}
	return ids
}

// ToAgentPorts converts cached ports to agent-compatible PortInfo slices.
func (wc *WorldCache) ToAgentPorts() []agent.PortInfo {
	ports := make([]agent.PortInfo, len(wc.Ports))
	for i, p := range wc.Ports {
		ports[i] = agent.PortInfo{
			ID:         p.ID,
			Name:       p.Name,
			Code:       p.Code,
			IsHub:      p.IsHub,
			TaxRateBps: p.TaxRateBps,
		}
	}
	return ports
}

// ToAgentRoutes converts cached routes to agent-compatible RouteInfo slices.
func (wc *WorldCache) ToAgentRoutes() []agent.RouteInfo {
	routes := make([]agent.RouteInfo, len(wc.Routes))
	for i, r := range wc.Routes {
		routes[i] = agent.RouteInfo{
			ID:       r.ID,
			FromID:   r.FromID,
			ToID:     r.ToID,
			Distance: r.Distance,
		}
	}
	return routes
}

// ToAgentShipTypes converts cached ship types to agent-compatible ShipTypeInfo slices.
func (wc *WorldCache) ToAgentShipTypes() []agent.ShipTypeInfo {
	types := make([]agent.ShipTypeInfo, len(wc.ShipTypes))
	for i, st := range wc.ShipTypes {
		types[i] = agent.ShipTypeInfo{
			ID:           st.ID,
			Name:         st.Name,
			Capacity:     st.Capacity,
			Speed:        st.Speed,
			Upkeep:       st.Upkeep,
			BasePrice:    st.BasePrice,
			PassengerCap: st.Passengers,
		}
	}
	return types
}

// PriceCache stores observed NPC prices across all ports and goods.
// Shared across all companies; updated by the price scanner goroutine.
type PriceCache struct {
	prices map[string]agent.PricePoint // key: "portID:goodID"
	mu     sync.RWMutex
}

// NewPriceCache creates an empty price cache.
func NewPriceCache() *PriceCache {
	return &PriceCache{
		prices: make(map[string]agent.PricePoint),
	}
}

// Set records a price observation.
func (pc *PriceCache) Set(portID, goodID uuid.UUID, buyPrice, sellPrice int) {
	key := portID.String() + ":" + goodID.String()
	pc.mu.Lock()
	defer pc.mu.Unlock()

	pc.prices[key] = agent.PricePoint{
		PortID:     portID,
		GoodID:     goodID,
		BuyPrice:   buyPrice,
		SellPrice:  sellPrice,
		ObservedAt: time.Now(),
	}
}

// Get returns the latest price for a port/good pair, or false if not observed.
func (pc *PriceCache) Get(portID, goodID uuid.UUID) (agent.PricePoint, bool) {
	key := portID.String() + ":" + goodID.String()
	pc.mu.RLock()
	defer pc.mu.RUnlock()

	pp, ok := pc.prices[key]
	return pp, ok
}

// All returns a snapshot of all observed prices.
func (pc *PriceCache) All() []agent.PricePoint {
	pc.mu.RLock()
	defer pc.mu.RUnlock()

	points := make([]agent.PricePoint, 0, len(pc.prices))
	for _, pp := range pc.prices {
		points = append(points, pp)
	}
	return points
}
