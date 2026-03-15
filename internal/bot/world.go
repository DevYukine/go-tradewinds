package bot

import (
	"context"
	"encoding/json"
	"sync"
	"time"

	"github.com/google/uuid"
	"go.uber.org/zap"

	"github.com/DevYukine/go-tradewinds/internal/agent"
	"github.com/DevYukine/go-tradewinds/internal/api"
	"github.com/DevYukine/go-tradewinds/internal/cache"
)

// WorldCache stores world data (ports, goods, routes, ship types) shared
// across all companies. Loaded at startup and periodically refreshed to
// pick up new ports/routes added to the game at runtime.
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

	mu sync.RWMutex // protects all slices and indexes for dynamic updates
}

// LoadWorldData fetches all static world data from the API and builds indexes.
// Uses Redis cache for world data to avoid redundant API calls on restart.
func LoadWorldData(ctx context.Context, client *api.Client, redis *cache.RedisCache, logger *zap.Logger) (*WorldCache, error) {
	log := logger.Named("world_cache")
	log.Info("loading world data")

	const worldCacheTTL = 30 * time.Minute

	ports, err := cachedFetch(ctx, redis, "world:ports", worldCacheTTL, func() ([]api.Port, error) {
		return client.ListPorts(ctx, api.PortFilters{})
	})
	if err != nil {
		return nil, err
	}

	goods, err := cachedFetch(ctx, redis, "world:goods", worldCacheTTL, func() ([]api.Good, error) {
		return client.ListGoods(ctx, "")
	})
	if err != nil {
		return nil, err
	}

	routes, err := cachedFetch(ctx, redis, "world:routes", worldCacheTTL, func() ([]api.Route, error) {
		return client.ListRoutes(ctx, api.RouteFilters{})
	})
	if err != nil {
		return nil, err
	}

	shipTypes, err := cachedFetch(ctx, redis, "world:ship_types", worldCacheTTL, func() ([]api.ShipType, error) {
		return client.ListShipTypes(ctx)
	})
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

	// Discover which ports have shipyards, using Redis cache.
	shipyardPortIDs, err := cachedFetch(ctx, redis, "world:shipyard_ports", worldCacheTTL, func() ([]uuid.UUID, error) {
		var ids []uuid.UUID
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
				ids = append(ids, port.ID)
			}
		}
		return ids, nil
	})
	if err != nil {
		return nil, err
	}
	wc.ShipyardPorts = shipyardPortIDs

	log.Info("world data loaded",
		zap.Int("ports", len(ports)),
		zap.Int("goods", len(goods)),
		zap.Int("routes", len(routes)),
		zap.Int("ship_types", len(shipTypes)),
		zap.Int("shipyard_ports", len(wc.ShipyardPorts)),
	)

	return wc, nil
}

// cachedFetch tries to load data from Redis cache first, falling back to the
// fetch function on cache miss. Results are cached in Redis with the given TTL.
func cachedFetch[T any](ctx context.Context, rc *cache.RedisCache, key string, ttl time.Duration, fetch func() (T, error)) (T, error) {
	var zero T

	// Try cache first.
	if data := rc.CacheGet(ctx, key); data != nil {
		var result T
		if err := json.Unmarshal(data, &result); err == nil {
			return result, nil
		}
	}

	// Cache miss — fetch from API.
	result, err := fetch()
	if err != nil {
		return zero, err
	}

	// Store in cache.
	data, err := json.Marshal(result)
	if err == nil {
		rc.CacheSet(ctx, key, data, ttl)
	}

	return result, nil
}

// GetPort returns a port by ID, or nil if not found.
func (wc *WorldCache) GetPort(id uuid.UUID) *api.Port {
	wc.mu.RLock()
	defer wc.mu.RUnlock()
	return wc.portsByID[id]
}

// GetGood returns a good by ID, or nil if not found.
func (wc *WorldCache) GetGood(id uuid.UUID) *api.Good {
	wc.mu.RLock()
	defer wc.mu.RUnlock()
	return wc.goodsByID[id]
}

// GetShipType returns a ship type by ID, or nil if not found.
func (wc *WorldCache) GetShipType(id uuid.UUID) *api.ShipType {
	wc.mu.RLock()
	defer wc.mu.RUnlock()
	return wc.shipTypesByID[id]
}

// GetRoute returns a route by ID, or nil if not found.
func (wc *WorldCache) GetRoute(id uuid.UUID) *api.Route {
	wc.mu.RLock()
	defer wc.mu.RUnlock()
	return wc.routesByID[id]
}

// RoutesFrom returns all routes departing from the given port.
func (wc *WorldCache) RoutesFrom(portID uuid.UUID) []api.Route {
	wc.mu.RLock()
	defer wc.mu.RUnlock()
	return wc.routesByFrom[portID]
}

// FindRoute returns the route between two ports, or nil if none exists.
func (wc *WorldCache) FindRoute(fromID, toID uuid.UUID) *api.Route {
	wc.mu.RLock()
	defer wc.mu.RUnlock()
	return wc.routesByPorts[[2]uuid.UUID{fromID, toID}]
}

// AddRoute adds a route to the cache indexes. Used when a route is fetched
// from the API but was missing from the initial cache load.
func (wc *WorldCache) AddRoute(r api.Route) {
	wc.mu.Lock()
	defer wc.mu.Unlock()
	key := [2]uuid.UUID{r.FromID, r.ToID}
	if wc.routesByPorts[key] != nil {
		return // already cached
	}
	wc.Routes = append(wc.Routes, r)
	idx := len(wc.Routes) - 1
	wc.routesByID[r.ID] = &wc.Routes[idx]
	wc.routesByFrom[r.FromID] = append(wc.routesByFrom[r.FromID], r)
	wc.routesByPorts[key] = &wc.Routes[idx]
}

// RefreshWorldData fetches ports, routes, and goods from the API and merges
// any new entries into the cache. Existing entries are not modified.
// Called periodically so the bot discovers new ports/routes at runtime.
func (wc *WorldCache) RefreshWorldData(ctx context.Context, client *api.Client, logger *zap.Logger) {
	log := logger.Named("world_refresh")

	// Fetch current ports from API (bypass Redis cache).
	freshPorts, err := client.ListPorts(ctx, api.PortFilters{})
	if err != nil {
		log.Warn("failed to refresh ports", zap.Error(err))
		return
	}

	// Fetch current routes.
	freshRoutes, err := client.ListRoutes(ctx, api.RouteFilters{})
	if err != nil {
		log.Warn("failed to refresh routes", zap.Error(err))
		return
	}

	// Fetch current goods.
	freshGoods, err := client.ListGoods(ctx, "")
	if err != nil {
		log.Warn("failed to refresh goods", zap.Error(err))
		return
	}

	wc.mu.Lock()
	defer wc.mu.Unlock()

	// Merge new ports.
	newPorts := 0
	for _, p := range freshPorts {
		if wc.portsByID[p.ID] != nil {
			continue
		}
		wc.Ports = append(wc.Ports, p)
		idx := len(wc.Ports) - 1
		wc.portsByID[wc.Ports[idx].ID] = &wc.Ports[idx]
		newPorts++
	}

	// Merge new routes.
	newRoutes := 0
	for _, r := range freshRoutes {
		key := [2]uuid.UUID{r.FromID, r.ToID}
		if wc.routesByPorts[key] != nil {
			continue
		}
		wc.Routes = append(wc.Routes, r)
		idx := len(wc.Routes) - 1
		wc.routesByID[r.ID] = &wc.Routes[idx]
		wc.routesByFrom[r.FromID] = append(wc.routesByFrom[r.FromID], r)
		wc.routesByPorts[key] = &wc.Routes[idx]
		newRoutes++
	}

	// Merge new goods.
	newGoods := 0
	for _, g := range freshGoods {
		if wc.goodsByID[g.ID] != nil {
			continue
		}
		wc.Goods = append(wc.Goods, g)
		idx := len(wc.Goods) - 1
		wc.goodsByID[wc.Goods[idx].ID] = &wc.Goods[idx]
		newGoods++
	}

	// Check for new shipyard ports.
	newShipyards := 0
	shipyardSet := make(map[uuid.UUID]bool, len(wc.ShipyardPorts))
	for _, id := range wc.ShipyardPorts {
		shipyardSet[id] = true
	}
	// Only check ports that were newly added.
	if newPorts > 0 {
		for i := len(wc.Ports) - newPorts; i < len(wc.Ports); i++ {
			port := wc.Ports[i]
			if shipyardSet[port.ID] {
				continue
			}
			// Unlock briefly for the API call, then re-lock.
			wc.mu.Unlock()
			shipyard, err := client.GetPortShipyard(ctx, port.ID)
			wc.mu.Lock()
			if err == nil && shipyard != nil {
				wc.ShipyardPorts = append(wc.ShipyardPorts, port.ID)
				newShipyards++
			}
		}
	}

	if newPorts > 0 || newRoutes > 0 || newGoods > 0 {
		log.Info("world data refreshed — new entries discovered",
			zap.Int("new_ports", newPorts),
			zap.Int("new_routes", newRoutes),
			zap.Int("new_goods", newGoods),
			zap.Int("new_shipyards", newShipyards),
			zap.Int("total_ports", len(wc.Ports)),
			zap.Int("total_routes", len(wc.Routes)),
		)
	} else {
		log.Debug("world data refreshed — no changes")
	}
}

// PortCount returns the number of ports (thread-safe).
func (wc *WorldCache) PortCount() int {
	wc.mu.RLock()
	defer wc.mu.RUnlock()
	return len(wc.Ports)
}

// GetPortAtIndex returns the port at the given index (mod port count) and
// the total port count. Thread-safe for use by the scanner.
func (wc *WorldCache) GetPortAtIndex(idx int) (api.Port, int) {
	wc.mu.RLock()
	defer wc.mu.RUnlock()
	n := len(wc.Ports)
	if n == 0 {
		return api.Port{}, 0
	}
	return wc.Ports[idx%n], n
}

// PortIDs returns all port UUIDs for iteration.
func (wc *WorldCache) PortIDs() []uuid.UUID {
	wc.mu.RLock()
	defer wc.mu.RUnlock()
	ids := make([]uuid.UUID, len(wc.Ports))
	for i := range wc.Ports {
		ids[i] = wc.Ports[i].ID
	}
	return ids
}

// ToAgentPorts converts cached ports to agent-compatible PortInfo slices.
func (wc *WorldCache) ToAgentPorts() []agent.PortInfo {
	wc.mu.RLock()
	defer wc.mu.RUnlock()
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
	wc.mu.RLock()
	defer wc.mu.RUnlock()
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
	wc.mu.RLock()
	defer wc.mu.RUnlock()
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

// Snapshot returns copies of all world data slices for safe reading
// outside the lock. Used by API handlers to build consistent responses.
func (wc *WorldCache) Snapshot() ([]api.Port, []api.Good, []api.Route, []api.ShipType, []uuid.UUID) {
	wc.mu.RLock()
	defer wc.mu.RUnlock()

	ports := make([]api.Port, len(wc.Ports))
	copy(ports, wc.Ports)

	goods := make([]api.Good, len(wc.Goods))
	copy(goods, wc.Goods)

	routes := make([]api.Route, len(wc.Routes))
	copy(routes, wc.Routes)

	shipTypes := make([]api.ShipType, len(wc.ShipTypes))
	copy(shipTypes, wc.ShipTypes)

	shipyardPorts := make([]uuid.UUID, len(wc.ShipyardPorts))
	copy(shipyardPorts, wc.ShipyardPorts)

	return ports, goods, routes, shipTypes, shipyardPorts
}

// PriceCache stores observed NPC prices across all ports and goods.
// Shared across all companies; updated by the price scanner goroutine.
// Backed by Redis for persistence across restarts.
type PriceCache struct {
	prices map[string]agent.PricePoint // key: "portID:goodID"
	redis  *cache.RedisCache
	mu     sync.RWMutex
}

// NewPriceCache creates a price cache backed by Redis.
// Restores cached prices from Redis on creation.
func NewPriceCache(redis *cache.RedisCache) *PriceCache {
	pc := &PriceCache{
		prices: make(map[string]agent.PricePoint),
		redis:  redis,
	}

	// Restore prices from Redis.
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	entries := redis.LoadPriceCache(ctx)
	for key, entry := range entries {
		// Parse portID and goodID from the key.
		parts := splitPriceCacheKey(key)
		if len(parts) != 2 {
			continue
		}
		portID, err1 := uuid.Parse(parts[0])
		goodID, err2 := uuid.Parse(parts[1])
		if err1 != nil || err2 != nil {
			continue
		}

		pc.prices[key] = agent.PricePoint{
			PortID:     portID,
			GoodID:     goodID,
			BuyPrice:   entry.BuyPrice,
			SellPrice:  entry.SellPrice,
			ObservedAt: time.UnixMilli(entry.ObservedAt),
		}
	}

	return pc
}

// splitPriceCacheKey splits "portID:goodID" into its two UUID strings.
func splitPriceCacheKey(key string) []string {
	// UUIDs are 36 chars. Key format is "uuid:uuid" = 73 chars.
	if len(key) != 73 || key[36] != ':' {
		return nil
	}
	return []string{key[:36], key[37:]}
}

// Set records a price observation and persists it to Redis.
func (pc *PriceCache) Set(portID, goodID uuid.UUID, buyPrice, sellPrice int) {
	key := portID.String() + ":" + goodID.String()
	pc.mu.Lock()
	pc.prices[key] = agent.PricePoint{
		PortID:     portID,
		GoodID:     goodID,
		BuyPrice:   buyPrice,
		SellPrice:  sellPrice,
		ObservedAt: time.Now(),
	}
	pc.mu.Unlock()

	// Persist to Redis asynchronously.
	go pc.redis.SavePriceEntry(context.Background(), key, buyPrice, sellPrice)
}

// Get returns the latest price for a port/good pair, or false if not observed.
func (pc *PriceCache) Get(portID, goodID uuid.UUID) (agent.PricePoint, bool) {
	key := portID.String() + ":" + goodID.String()
	pc.mu.RLock()
	defer pc.mu.RUnlock()

	pp, ok := pc.prices[key]
	return pp, ok
}

// PortAge returns the age of the oldest price entry for a port.
// Returns a very large duration if the port has never been scanned.
func (pc *PriceCache) PortAge(portID uuid.UUID) time.Duration {
	prefix := portID.String() + ":"
	pc.mu.RLock()
	defer pc.mu.RUnlock()

	oldest := time.Duration(0)
	found := false
	now := time.Now()
	for key, pp := range pc.prices {
		if len(key) > 36 && key[:37] == prefix {
			age := now.Sub(pp.ObservedAt)
			if !found || age > oldest {
				oldest = age
				found = true
			}
		}
	}
	if !found {
		return 24 * time.Hour // never scanned
	}
	return oldest
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
