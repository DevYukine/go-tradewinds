package server

import (
	"strconv"
	"time"

	"github.com/gofiber/fiber/v3"
	"github.com/google/uuid"

	"github.com/DevYukine/go-tradewinds/internal/api"
	"github.com/DevYukine/go-tradewinds/internal/db"
)

// registerHandlers sets up all REST API routes.
func (s *Server) registerHandlers() {
	api := s.app.Group("/api")

	api.Get("/companies", s.handleCompanies)
	api.Get("/companies/:id/pnl", s.handleCompanyPnL)
	api.Get("/companies/:id/trades", s.handleCompanyTrades)
	api.Get("/companies/:id/logs", s.handleCompanyLogs)
	api.Get("/companies/:id/decisions", s.handleCompanyDecisions)
	api.Get("/companies/:id/inventory", s.handleCompanyInventory)
	api.Get("/companies/:id/passengers", s.handleCompanyPassengers)
	api.Get("/companies/:id/game-trades", s.handleCompanyGameTrades)
	api.Get("/strategy-metrics", s.handleStrategyMetrics)
	api.Get("/optimizer/log", s.handleOptimizerLog)
	api.Get("/prices", s.handlePrices)
	api.Get("/ratelimit", s.handleRateLimit)
	api.Get("/health", s.handleHealth)
	api.Get("/world", s.handleWorld)
	api.Get("/ships", s.handleAllShips)
	api.Get("/global-pnl", s.handleGlobalPnL)
	api.Get("/companies/:id/market-orders", s.handleCompanyMarketOrders)

	// Analytics — aggregated trade and route performance data.
	api.Get("/analytics/goods", s.handleAnalyticsGoods)
	api.Get("/analytics/routes", s.handleAnalyticsRoutes)
	api.Get("/analytics/timeline", s.handleAnalyticsTimeline)
}

// companyResponse extends CompanyRecord with live-enriched fields.
type companyResponse struct {
	db.CompanyRecord
	AgentName string `json:"agent_name"`
}

// handleCompanies returns all companies with their current status, treasury, and strategy.
// Treasury, reputation, and agent_name are enriched from live in-memory state when available.
func (s *Server) handleCompanies(c fiber.Ctx) error {
	var companies []db.CompanyRecord
	if err := s.db.Order("id ASC").Find(&companies).Error; err != nil {
		return c.Status(fiber.StatusInternalServerError).JSON(fiber.Map{
			"error": "failed to fetch companies",
		})
	}

	// Overlay live treasury/reputation/agent from running companies.
	runners := s.manager.Companies()
	resp := make([]companyResponse, len(companies))
	for i := range companies {
		resp[i].CompanyRecord = companies[i]
		resp[i].AgentName = "heuristic" // default
		for _, runner := range runners {
			rec := runner.DBRecord()
			if rec != nil && rec.GameID == companies[i].GameID {
				state := runner.State()
				state.RLock()
				// Only overlay live values once initState has completed;
				// before that, state.Treasury is 0 and would clobber the
				// correct DB value set by setupRunner.
				if state.Initialized {
					resp[i].Treasury = state.Treasury
					resp[i].Reputation = state.Reputation
				}
				state.RUnlock()
				resp[i].AgentName = runner.AgentName()
				break
			}
		}
	}

	return c.JSON(resp)
}

// handleCompanyPnL returns the P&L time series for a company.
// Query param: since (RFC3339 timestamp).
func (s *Server) handleCompanyPnL(c fiber.Ctx) error {
	companyID, err := strconv.ParseUint(c.Params("id"), 10, 64)
	if err != nil {
		return c.Status(fiber.StatusBadRequest).JSON(fiber.Map{
			"error": "invalid company id",
		})
	}

	query := s.db.Where("company_id = ?", companyID).Order("created_at ASC")

	if since := c.Query("since"); since != "" {
		t, err := time.Parse(time.RFC3339, since)
		if err != nil {
			return c.Status(fiber.StatusBadRequest).JSON(fiber.Map{
				"error": "invalid since parameter, expected RFC3339",
			})
		}
		query = query.Where("created_at >= ?", t)
	}

	var snapshots []db.PnLSnapshot
	if err := query.Limit(500).Find(&snapshots).Error; err != nil {
		return c.Status(fiber.StatusInternalServerError).JSON(fiber.Map{
			"error": "failed to fetch PnL snapshots",
		})
	}
	return c.JSON(snapshots)
}

// handleCompanyTrades returns the trade log for a company, paginated.
// Query params: limit (default 50), offset (default 0).
func (s *Server) handleCompanyTrades(c fiber.Ctx) error {
	companyID, err := strconv.ParseUint(c.Params("id"), 10, 64)
	if err != nil {
		return c.Status(fiber.StatusBadRequest).JSON(fiber.Map{
			"error": "invalid company id",
		})
	}

	limit := queryInt(c, "limit", 50)
	offset := queryInt(c, "offset", 0)

	var trades []db.TradeLog
	if err := s.db.Where("company_id = ?", companyID).
		Order("created_at DESC").
		Limit(limit).
		Offset(offset).
		Find(&trades).Error; err != nil {
		return c.Status(fiber.StatusInternalServerError).JSON(fiber.Map{
			"error": "failed to fetch trades",
		})
	}
	return c.JSON(trades)
}

// handleCompanyLogs returns historical logs for a company, paginated.
// Query params: limit (default 100), offset (default 0).
func (s *Server) handleCompanyLogs(c fiber.Ctx) error {
	companyID, err := strconv.ParseUint(c.Params("id"), 10, 64)
	if err != nil {
		return c.Status(fiber.StatusBadRequest).JSON(fiber.Map{
			"error": "invalid company id",
		})
	}

	limit := queryInt(c, "limit", 100)
	offset := queryInt(c, "offset", 0)

	var logs []db.CompanyLog
	if err := s.db.Where("company_id = ?", companyID).
		Order("created_at DESC").
		Limit(limit).
		Offset(offset).
		Find(&logs).Error; err != nil {
		return c.Status(fiber.StatusInternalServerError).JSON(fiber.Map{
			"error": "failed to fetch logs",
		})
	}
	return c.JSON(logs)
}

// handleCompanyDecisions returns agent decision logs for a company.
// Query param: limit (default 20).
func (s *Server) handleCompanyDecisions(c fiber.Ctx) error {
	companyID, err := strconv.ParseUint(c.Params("id"), 10, 64)
	if err != nil {
		return c.Status(fiber.StatusBadRequest).JSON(fiber.Map{
			"error": "invalid company id",
		})
	}

	limit := queryInt(c, "limit", 20)

	var decisions []db.AgentDecisionLog
	if err := s.db.Where("company_id = ?", companyID).
		Order("created_at DESC").
		Limit(limit).
		Find(&decisions).Error; err != nil {
		return c.Status(fiber.StatusInternalServerError).JSON(fiber.Map{
			"error": "failed to fetch decisions",
		})
	}
	return c.JSON(decisions)
}

// handleCompanyPassengers returns the passenger boarding log for a company, paginated.
// Query params: limit (default 50), offset (default 0).
func (s *Server) handleCompanyPassengers(c fiber.Ctx) error {
	companyID, err := strconv.ParseUint(c.Params("id"), 10, 64)
	if err != nil {
		return c.Status(fiber.StatusBadRequest).JSON(fiber.Map{
			"error": "invalid company id",
		})
	}

	limit := queryInt(c, "limit", 50)
	offset := queryInt(c, "offset", 0)

	var passengers []db.PassengerLog
	if err := s.db.Where("company_id = ?", companyID).
		Order("created_at DESC").
		Limit(limit).
		Offset(offset).
		Find(&passengers).Error; err != nil {
		return c.Status(fiber.StatusInternalServerError).JSON(fiber.Map{
			"error": "failed to fetch passenger logs",
		})
	}
	return c.JSON(passengers)
}

// handleCompanyInventory returns the current in-memory state for a company,
// including full ship details (location, arrival time, cargo) and warehouses.
func (s *Server) handleCompanyInventory(c fiber.Ctx) error {
	// The route param is the DB integer id, but runners are keyed by game UUID.
	var company db.CompanyRecord
	if err := s.db.First(&company, c.Params("id")).Error; err != nil {
		return c.Status(fiber.StatusNotFound).JSON(fiber.Map{
			"error": "company not found",
		})
	}

	runner := s.manager.GetRunner(company.GameID)
	if runner == nil {
		return c.Status(fiber.StatusNotFound).JSON(fiber.Map{
			"error": "company not running",
		})
	}

	state := runner.State()
	state.RLock()
	defer state.RUnlock()

	// Resolve port/good names from world cache.
	world := s.manager.WorldData()

	type cargoItem struct {
		GoodID   string `json:"good_id"`
		GoodName string `json:"good_name"`
		Quantity int    `json:"quantity"`
		BuyPrice int    `json:"buy_price"`
	}
	type shipDetail struct {
		ShipID         string      `json:"ship_id"`
		ShipName       string      `json:"ship_name"`
		ShipType       string      `json:"ship_type"`
		Capacity       int         `json:"capacity"`
		PassengerCap   int         `json:"passenger_cap"`
		PassengerCount int         `json:"passenger_count"`
		Speed          int         `json:"speed"`
		Upkeep         int         `json:"upkeep"`
		Status         string      `json:"status"`
		PortID         string      `json:"port_id,omitempty"`
		PortName       string      `json:"port_name,omitempty"`
		RouteID        string      `json:"route_id,omitempty"`
		FromPortName   string      `json:"from_port_name,omitempty"`
		ToPortName     string      `json:"to_port_name,omitempty"`
		Distance       float64     `json:"distance,omitempty"`
		ArrivingAt     *time.Time  `json:"arriving_at,omitempty"`
		Cargo          []cargoItem `json:"cargo"`
		CargoTotal     int         `json:"cargo_total"`
		CargoValue     int64       `json:"cargo_value"`
	}
	type warehouseItem struct {
		GoodID   string `json:"good_id"`
		GoodName string `json:"good_name"`
		Quantity int    `json:"quantity"`
	}
	type warehouseDetail struct {
		WarehouseID string          `json:"warehouse_id"`
		PortID      string          `json:"port_id"`
		PortName    string          `json:"port_name"`
		Level       int             `json:"level"`
		Capacity    int             `json:"capacity"`
		Items       []warehouseItem `json:"items"`
	}

	priceCache := s.manager.PriceCache()

	ships := make([]shipDetail, 0, len(state.Ships))
	var totalCargoValue int64
	for _, ss := range state.Ships {
		cargo := make([]cargoItem, len(ss.Cargo))
		cargoTotal := 0
		var shipCargoValue int64
		for i, ci := range ss.Cargo {
			goodName := ci.GoodID.String()
			if world != nil {
				if g := world.GetGood(ci.GoodID); g != nil {
					goodName = g.Name
				}
			}
			// Look up buy price from price cache to estimate cargo cost basis.
			// Use minimum known buy price across all ports for a stable, conservative valuation.
			buyPrice := 0
			if ss.Ship.PortID != nil {
				if pp, ok := priceCache.Get(*ss.Ship.PortID, ci.GoodID); ok && pp.BuyPrice > 0 {
					buyPrice = pp.BuyPrice
				}
			}
			if buyPrice == 0 {
				// Fallback: use the lowest known buy price across all ports for stability.
				// Iterating a map has non-deterministic order, so taking "first match"
				// would produce different values on each request.
				for _, pp := range priceCache.All() {
					if pp.GoodID == ci.GoodID && pp.BuyPrice > 0 {
						if buyPrice == 0 || pp.BuyPrice < buyPrice {
							buyPrice = pp.BuyPrice
						}
					}
				}
			}
			cargo[i] = cargoItem{
				GoodID:   ci.GoodID.String(),
				GoodName: goodName,
				Quantity: ci.Quantity,
				BuyPrice: buyPrice,
			}
			cargoTotal += ci.Quantity
			shipCargoValue += int64(buyPrice) * int64(ci.Quantity)
		}

		sd := shipDetail{
			ShipID:         ss.Ship.ID.String(),
			ShipName:       ss.Ship.Name,
			Status:         ss.Ship.Status,
			Cargo:          cargo,
			CargoTotal:     cargoTotal,
			CargoValue:     shipCargoValue,
			PassengerCount: ss.PassengerCount,
		}
		totalCargoValue += shipCargoValue

		// Resolve ship type details.
		if world != nil {
			if st := world.GetShipType(ss.Ship.ShipTypeID); st != nil {
				sd.ShipType = st.Name
				sd.Capacity = st.Capacity
				sd.PassengerCap = st.Passengers
				sd.Speed = st.Speed
				sd.Upkeep = st.Upkeep
			}
		}

		if ss.Ship.PortID != nil {
			sd.PortID = ss.Ship.PortID.String()
			if world != nil {
				if p := world.GetPort(*ss.Ship.PortID); p != nil {
					sd.PortName = p.Name
				}
			}
		}
		if ss.Ship.RouteID != nil {
			sd.RouteID = ss.Ship.RouteID.String()
			// Resolve route origin and destination port names.
			if world != nil {
				if route := world.GetRoute(*ss.Ship.RouteID); route != nil {
					sd.Distance = route.Distance
					if from := world.GetPort(route.FromID); from != nil {
						sd.FromPortName = from.Name
					}
					if to := world.GetPort(route.ToID); to != nil {
						sd.ToPortName = to.Name
					}
				}
			}
		}
		if ss.Ship.ArrivingAt != nil {
			sd.ArrivingAt = ss.Ship.ArrivingAt
		}

		ships = append(ships, sd)
	}

	warehouses := make([]warehouseDetail, 0, len(state.Warehouses))
	for _, ws := range state.Warehouses {
		items := make([]warehouseItem, len(ws.Inventory))
		for i, item := range ws.Inventory {
			goodName := item.GoodID.String()
			if world != nil {
				if g := world.GetGood(item.GoodID); g != nil {
					goodName = g.Name
				}
			}
			items[i] = warehouseItem{
				GoodID:   item.GoodID.String(),
				GoodName: goodName,
				Quantity: item.Quantity,
			}
		}

		portName := ws.Warehouse.PortID.String()
		if world != nil {
			if p := world.GetPort(ws.Warehouse.PortID); p != nil {
				portName = p.Name
			}
		}

		warehouses = append(warehouses, warehouseDetail{
			WarehouseID: ws.Warehouse.ID.String(),
			PortID:      ws.Warehouse.PortID.String(),
			PortName:    portName,
			Level:       ws.Warehouse.Level,
			Capacity:    ws.Warehouse.Capacity,
			Items:       items,
		})
	}

	// Use live treasury only after initState completes; otherwise fall back to
	// the DB value which setupRunner populated from the game API listing.
	treasury := state.Treasury
	if !state.Initialized {
		treasury = company.Treasury
	}

	return c.JSON(fiber.Map{
		"company_id":    company.GameID,
		"treasury":      treasury,
		"total_upkeep":  state.TotalUpkeep,
		"cargo_value":   totalCargoValue,
		"ships":         ships,
		"warehouses":    warehouses,
	})
}

// handleOptimizerLog returns the optimizer decision history (StrategyMetric records).
// Query param: limit (default 50).
func (s *Server) handleOptimizerLog(c fiber.Ctx) error {
	limit := queryInt(c, "limit", 50)

	var metrics []db.StrategyMetric
	if err := s.db.Order("created_at DESC").
		Limit(limit).
		Find(&metrics).Error; err != nil {
		return c.Status(fiber.StatusInternalServerError).JSON(fiber.Map{
			"error": "failed to fetch optimizer log",
		})
	}
	return c.JSON(metrics)
}

// handleStrategyMetrics returns the latest StrategyMetric per strategy.
func (s *Server) handleStrategyMetrics(c fiber.Ctx) error {
	var metrics []db.StrategyMetric

	// Get the latest metric for each strategy using a subquery.
	subQuery := s.db.Model(&db.StrategyMetric{}).
		Select("MAX(id) as id").
		Group("strategy_name")

	if err := s.db.Where("id IN (?)", subQuery).
		Order("strategy_name ASC").
		Find(&metrics).Error; err != nil {
		return c.Status(fiber.StatusInternalServerError).JSON(fiber.Map{
			"error": "failed to fetch strategy metrics",
		})
	}
	return c.JSON(metrics)
}

// handlePrices returns the latest price observations with resolved names.
func (s *Server) handlePrices(c fiber.Ctx) error {
	var prices []db.PriceObservation

	// Get the latest observation per port+good combination.
	subQuery := s.db.Model(&db.PriceObservation{}).
		Select("MAX(id) as id").
		Group("port_id, good_id")

	if err := s.db.Where("id IN (?)", subQuery).
		Order("port_id, good_id").
		Find(&prices).Error; err != nil {
		return c.Status(fiber.StatusInternalServerError).JSON(fiber.Map{
			"error": "failed to fetch prices",
		})
	}

	world := s.manager.WorldData()

	type enrichedPrice struct {
		PortID    string `json:"port_id"`
		PortName  string `json:"port_name"`
		GoodID    string `json:"good_id"`
		GoodName  string `json:"good_name"`
		BuyPrice  int    `json:"buy_price"`
		SellPrice int    `json:"sell_price"`
		Spread    int    `json:"spread"`
		UpdatedAt string `json:"updated_at"`
	}

	result := make([]enrichedPrice, 0, len(prices))
	for _, p := range prices {
		portName := p.PortID
		goodName := p.GoodID
		if world != nil {
			if port := world.GetPort(uuid.MustParse(p.PortID)); port != nil {
				portName = port.Name
			}
			if good := world.GetGood(uuid.MustParse(p.GoodID)); good != nil {
				goodName = good.Name
			}
		}
		result = append(result, enrichedPrice{
			PortID:    p.PortID,
			PortName:  portName,
			GoodID:    p.GoodID,
			GoodName:  goodName,
			BuyPrice:  p.BuyPrice,
			SellPrice: p.SellPrice,
			Spread:    p.SellPrice - p.BuyPrice,
			UpdatedAt: p.CreatedAt.Format(time.RFC3339),
		})
	}
	return c.JSON(result)
}

// handleRateLimit returns the current rate limit utilization.
func (s *Server) handleRateLimit(c fiber.Ctx) error {
	rl := s.manager.RateLimiter()
	used, max := rl.CurrentBudget()
	utilization := rl.Utilization()

	return c.JSON(fiber.Map{
		"used":                 used,
		"max_per_minute":       max,
		"current_utilization":  utilization,
		"remaining":            max - used,
		"active_companies":     s.manager.CompanyCount(),
	})
}

// handleHealth returns basic health info about the bot.
func (s *Server) handleHealth(c fiber.Ctx) error {
	return c.JSON(fiber.Map{
		"status":        "ok",
		"uptime_seconds": time.Since(s.startedAt).Seconds(),
		"company_count": s.manager.CompanyCount(),
		"agent_type":    s.cfg.Agent.Type,
	})
}

// handleWorld returns static world data: ports, goods, routes, and ship types.
func (s *Server) handleWorld(c fiber.Ctx) error {
	world := s.manager.WorldData()
	if world == nil {
		return c.Status(fiber.StatusServiceUnavailable).JSON(fiber.Map{
			"error": "world data not loaded yet",
		})
	}

	type portInfo struct {
		ID         string  `json:"id"`
		Name       string  `json:"name"`
		Code       string  `json:"code"`
		IsHub      bool    `json:"is_hub"`
		TaxRate    float64 `json:"tax_rate"`
		HasShipyard bool   `json:"has_shipyard"`
		Latitude   float64 `json:"latitude"`
		Longitude  float64 `json:"longitude"`
	}

	// Coordinates for European port cities. Looked up by port name.
	// Comprehensive list so new game ports are auto-placed on the map.
	portCoordinates := map[string][2]float64{
		// British Isles
		"London":       {51.5074, -0.1278},
		"Plymouth":     {50.3755, -4.1427},
		"Portsmouth":   {50.8198, -1.0880},
		"Bristol":      {51.4545, -2.5879},
		"Hull":         {53.7457, -0.3367},
		"Edinburgh":    {55.9533, -3.1883},
		"Glasgow":      {55.8642, -4.2518},
		"Liverpool":    {53.4084, -2.9916},
		"Newcastle":    {54.9783, -1.6178},
		"Southampton":  {50.9097, -1.4044},
		"Dover":        {51.1279, 1.3134},
		"Aberdeen":     {57.1497, -2.0943},
		"Inverness":    {57.4778, -4.2247},
		"Cardiff":      {51.4816, -3.1791},
		"Belfast":      {54.5973, -5.9301},
		"Dublin":       {53.3498, -6.2603},
		"Cork":         {51.8985, -8.4756},
		"Galway":       {53.2707, -9.0568},
		"Waterford":    {52.2593, -7.1101},
		// Low Countries & Germany
		"Rotterdam":    {51.9225, 4.4792},
		"Amsterdam":    {52.3676, 4.9041},
		"Antwerp":      {51.2194, 4.4025},
		"Bruges":       {51.2093, 3.2247},
		"Ghent":        {51.0543, 3.7174},
		"Bremen":       {53.0793, 8.8017},
		"Hamburg":      {53.5511, 9.9937},
		"Lübeck":       {53.8655, 10.6866},
		"Lubeck":       {53.8655, 10.6866},
		// France
		"Calais":       {50.9513, 1.8587},
		"Dunkirk":      {51.0343, 2.3768},
		"Le Havre":     {49.4944, 0.1079},
		"Rouen":        {49.4432, 1.0999},
		"Brest":        {48.3904, -4.4861},
		"Nantes":       {47.2184, -1.5536},
		"La Rochelle":  {46.1603, -1.1511},
		"Bordeaux":     {44.8378, -0.5792},
		"Marseille":    {43.2965, 5.3698},
		"Bayonne":      {43.4929, -1.4748},
		"Saint-Malo":   {48.6493, -2.0007},
		"Cherbourg":    {49.6337, -1.6222},
		"Dieppe":       {49.9253, 1.0760},
		// Iberian Peninsula
		"Lisbon":       {38.7223, -9.1393},
		"Porto":        {41.1579, -8.6291},
		"Cadiz":        {36.5271, -6.2886},
		"Seville":      {37.3891, -5.9845},
		"Barcelona":    {41.3874, 2.1686},
		"Valencia":     {39.4699, -0.3763},
		"Malaga":       {36.7213, -4.4214},
		"Bilbao":       {43.2630, -2.9350},
		"Vigo":         {42.2406, -8.7207},
		"A Coruña":     {43.3623, -8.4115},
		"A Coruna":     {43.3623, -8.4115},
		// Scandinavia & Baltic
		"Bergen":       {60.3913, 5.3221},
		"Oslo":         {59.9139, 10.7522},
		"Stavanger":    {58.9700, 5.7331},
		"Copenhagen":   {55.6761, 12.5683},
		"Stockholm":    {59.3293, 18.0686},
		"Gothenburg":   {57.7089, 11.9746},
		"Malmö":        {55.6049, 13.0038},
		"Malmo":        {55.6049, 13.0038},
		"Gdansk":       {54.3520, 18.6466},
		"Danzig":       {54.3520, 18.6466},
		"Riga":         {56.9496, 24.1052},
		"Tallinn":      {59.4370, 24.7536},
		"Helsinki":     {60.1699, 24.9384},
		"Königsberg":   {54.7104, 20.4522},
		"Konigsberg":   {54.7104, 20.4522},
		// Italy
		"Genoa":        {44.4056, 8.9463},
		"Venice":       {45.4408, 12.3155},
		"Naples":       {40.8518, 14.2681},
		"Palermo":      {38.1157, 13.3615},
		"Pisa":         {43.7228, 10.4017},
		"Florence":     {43.7696, 11.2558},
		"Rome":         {41.9028, 12.4964},
		// Eastern Mediterranean
		"Constantinople": {41.0082, 28.9784},
		"Istanbul":     {41.0082, 28.9784},
		"Athens":       {37.9838, 23.7275},
		"Alexandria":   {31.2001, 29.9187},
		// Other
		"Tangier":      {35.7595, -5.8340},
		"Tunis":        {36.8065, 10.1815},
	}
	type goodInfo struct {
		ID          string `json:"id"`
		Name        string `json:"name"`
		Description string `json:"description"`
		Category    string `json:"category"`
	}
	type routeInfo struct {
		ID           string  `json:"id"`
		FromPortID   string  `json:"from_port_id"`
		ToPortID     string  `json:"to_port_id"`
		FromPortName string  `json:"from_port_name"`
		ToPortName   string  `json:"to_port_name"`
		Distance     float64 `json:"distance"`
	}
	type shipTypeInfo struct {
		ID           string `json:"id"`
		Name         string `json:"name"`
		Capacity     int    `json:"capacity"`
		PassengerCap int    `json:"passenger_cap"`
		Speed        int    `json:"speed"`
		Upkeep       int    `json:"upkeep"`
		BasePrice    int    `json:"base_price"`
	}

	// Take a consistent snapshot under the world cache lock.
	worldPorts, worldGoods, worldRoutes, worldShipTypes, worldShipyardPorts := world.Snapshot()

	// Build shipyard port set for quick lookup.
	shipyardSet := make(map[uuid.UUID]bool, len(worldShipyardPorts))
	for _, id := range worldShipyardPorts {
		shipyardSet[id] = true
	}

	// Build port lookup for route name resolution.
	portNameByID := make(map[uuid.UUID]string, len(worldPorts))
	for _, p := range worldPorts {
		portNameByID[p.ID] = p.Name
	}

	ports := make([]portInfo, len(worldPorts))
	for i, p := range worldPorts {
		pi := portInfo{
			ID:          p.ID.String(),
			Name:        p.Name,
			Code:        p.Code,
			IsHub:       p.IsHub,
			TaxRate:     float64(p.TaxRateBps) / 100.0,
			HasShipyard: shipyardSet[p.ID],
		}
		if coords, ok := portCoordinates[p.Name]; ok {
			pi.Latitude = coords[0]
			pi.Longitude = coords[1]
		}
		ports[i] = pi
	}

	goods := make([]goodInfo, len(worldGoods))
	for i, g := range worldGoods {
		goods[i] = goodInfo{
			ID:          g.ID.String(),
			Name:        g.Name,
			Description: g.Description,
			Category:    g.Category,
		}
	}

	routes := make([]routeInfo, len(worldRoutes))
	for i, r := range worldRoutes {
		fromName := portNameByID[r.FromID]
		if fromName == "" {
			fromName = r.FromID.String()
		}
		toName := portNameByID[r.ToID]
		if toName == "" {
			toName = r.ToID.String()
		}
		routes[i] = routeInfo{
			ID:           r.ID.String(),
			FromPortID:   r.FromID.String(),
			ToPortID:     r.ToID.String(),
			FromPortName: fromName,
			ToPortName:   toName,
			Distance:     r.Distance,
		}
	}

	shipTypes := make([]shipTypeInfo, len(worldShipTypes))
	for i, st := range worldShipTypes {
		shipTypes[i] = shipTypeInfo{
			ID:           st.ID.String(),
			Name:         st.Name,
			Capacity:     st.Capacity,
			PassengerCap: st.Passengers,
			Speed:        st.Speed,
			Upkeep:       st.Upkeep,
			BasePrice:    st.BasePrice,
		}
	}

	return c.JSON(fiber.Map{
		"ports":      ports,
		"goods":      goods,
		"routes":     routes,
		"ship_types": shipTypes,
	})
}

// handleAllShips returns all ships across all companies for the world map.
// Includes full cargo details and passenger info for ship detail panels.
func (s *Server) handleAllShips(c fiber.Ctx) error {
	world := s.manager.WorldData()
	companies := s.manager.Companies()

	type cargoItem struct {
		GoodID   string `json:"good_id"`
		GoodName string `json:"good_name"`
		Quantity int    `json:"quantity"`
	}
	type shipInfo struct {
		ShipID         string      `json:"ship_id"`
		ShipName       string      `json:"ship_name"`
		ShipType       string      `json:"ship_type"`
		Status         string      `json:"status"`
		CompanyID      string      `json:"company_id"`
		CompanyName    string      `json:"company_name"`
		Strategy       string      `json:"strategy"`
		PortID         string      `json:"port_id,omitempty"`
		PortName       string      `json:"port_name,omitempty"`
		RouteID        string      `json:"route_id,omitempty"`
		FromPortID     string      `json:"from_port_id,omitempty"`
		ToPortID       string      `json:"to_port_id,omitempty"`
		FromPortName   string      `json:"from_port_name,omitempty"`
		ToPortName     string      `json:"to_port_name,omitempty"`
		ArrivingAt     *time.Time  `json:"arriving_at,omitempty"`
		CargoTotal     int         `json:"cargo_total"`
		Capacity       int         `json:"capacity"`
		Speed          int         `json:"speed"`
		Upkeep         int         `json:"upkeep"`
		PassengerCap   int         `json:"passenger_cap"`
		PassengerCount int         `json:"passenger_count"`
		Cargo          []cargoItem `json:"cargo"`
	}

	var ships []shipInfo
	for _, runner := range companies {
		state := runner.State()
		state.RLock()
		record := runner.DBRecord()

		for _, ss := range state.Ships {
			cargo := make([]cargoItem, 0, len(ss.Cargo))
			cargoTotal := 0
			for _, ci := range ss.Cargo {
				goodName := ci.GoodID.String()
				if world != nil {
					if g := world.GetGood(ci.GoodID); g != nil {
						goodName = g.Name
					}
				}
				cargo = append(cargo, cargoItem{
					GoodID:   ci.GoodID.String(),
					GoodName: goodName,
					Quantity: ci.Quantity,
				})
				cargoTotal += ci.Quantity
			}

			si := shipInfo{
				ShipID:         ss.Ship.ID.String(),
				ShipName:       ss.Ship.Name,
				Status:         ss.Ship.Status,
				CompanyID:      record.GameID,
				CompanyName:    record.Name,
				Strategy:       record.Strategy,
				CargoTotal:     cargoTotal,
				PassengerCount: ss.PassengerCount,
				Cargo:          cargo,
			}

			if world != nil {
				if st := world.GetShipType(ss.Ship.ShipTypeID); st != nil {
					si.ShipType = st.Name
					si.Capacity = st.Capacity
					si.Speed = st.Speed
					si.Upkeep = st.Upkeep
					si.PassengerCap = st.Passengers
				}
			}

			if ss.Ship.PortID != nil {
				si.PortID = ss.Ship.PortID.String()
				if world != nil {
					if p := world.GetPort(*ss.Ship.PortID); p != nil {
						si.PortName = p.Name
					}
				}
			}
			if ss.Ship.RouteID != nil {
				si.RouteID = ss.Ship.RouteID.String()
				if world != nil {
					if route := world.GetRoute(*ss.Ship.RouteID); route != nil {
						si.FromPortID = route.FromID.String()
						si.ToPortID = route.ToID.String()
						if from := world.GetPort(route.FromID); from != nil {
							si.FromPortName = from.Name
						}
						if to := world.GetPort(route.ToID); to != nil {
							si.ToPortName = to.Name
						}
					}
				}
			}
			if ss.Ship.ArrivingAt != nil {
				si.ArrivingAt = ss.Ship.ArrivingAt
			}

			ships = append(ships, si)
		}
		state.RUnlock()
	}

	return c.JSON(ships)
}

// handleCompanyGameTrades proxies trade history from the game API for a company.
// Query param: role (optional, "buyer" or "seller").
func (s *Server) handleCompanyGameTrades(c fiber.Ctx) error {
	var company db.CompanyRecord
	if err := s.db.First(&company, c.Params("id")).Error; err != nil {
		return c.Status(fiber.StatusNotFound).JSON(fiber.Map{
			"error": "company not found",
		})
	}

	runner := s.manager.GetRunner(company.GameID)
	if runner == nil {
		return c.Status(fiber.StatusNotFound).JSON(fiber.Map{
			"error": "company not running",
		})
	}

	filters := api.TradeHistoryFilters{
		Role: c.Query("role"),
	}

	entries, err := runner.Client().ListTradeHistory(c.Context(), filters)
	if err != nil {
		return c.Status(fiber.StatusBadGateway).JSON(fiber.Map{
			"error": "failed to fetch trade history from game API",
		})
	}

	// Enrich entries with resolved good and port names.
	world := s.manager.WorldData()

	type enrichedEntry struct {
		ID         string    `json:"id"`
		BuyerID    string    `json:"buyer_id"`
		SellerID   string    `json:"seller_id"`
		GoodID     string    `json:"good_id"`
		GoodName   string    `json:"good_name"`
		PortID     string    `json:"port_id"`
		PortName   string    `json:"port_name"`
		Price      int       `json:"price"`
		Quantity   int       `json:"quantity"`
		Source     string    `json:"source"`
		OccurredAt time.Time `json:"occurred_at"`
	}

	result := make([]enrichedEntry, 0, len(entries))
	for _, e := range entries {
		goodName := e.GoodID.String()
		portName := e.PortID.String()
		if world != nil {
			if g := world.GetGood(e.GoodID); g != nil {
				goodName = g.Name
			}
			if p := world.GetPort(e.PortID); p != nil {
				portName = p.Name
			}
		}
		result = append(result, enrichedEntry{
			ID:         e.ID.String(),
			BuyerID:    e.BuyerID.String(),
			SellerID:   e.SellerID.String(),
			GoodID:     e.GoodID.String(),
			GoodName:   goodName,
			PortID:     e.PortID.String(),
			PortName:   portName,
			Price:      e.Price,
			Quantity:   e.Quantity,
			Source:     e.Source,
			OccurredAt: e.OccurredAt,
		})
	}
	return c.JSON(result)
}

// handleGlobalPnL returns aggregate win/loss stats across all companies.
func (s *Server) handleGlobalPnL(c fiber.Ctx) error {
	type companyPnL struct {
		CompanyID    uint    `json:"company_id"`
		CompanyName  string  `json:"company_name"`
		Strategy     string  `json:"strategy"`
		Treasury     int64   `json:"treasury"`
		TradeRev     int64   `json:"trade_rev"`
		TradeCosts   int64   `json:"trade_costs"`
		PassengerRev int64   `json:"passenger_rev"`
		NetPnL       int64   `json:"net_pnl"`
		TradeCount   int64   `json:"trade_count"`
		WinCount     int64   `json:"win_count"`
		WinRate      float64 `json:"win_rate"`
	}

	var companies []db.CompanyRecord
	s.db.Where("status = ?", "running").Find(&companies)

	result := make([]companyPnL, 0, len(companies))
	var globalTradeRev, globalTradeCosts, globalPassengerRev int64
	var globalTradeCount, globalWinCount int64

	runners := s.manager.Companies()

	for _, comp := range companies {
		var tradeRev, tradeCosts int64
		var tradeCount, winCount int64
		var passengerRev int64

		s.db.Model(&db.TradeLog{}).Where("company_id = ? AND action = ?", comp.ID, "sell").
			Select("COALESCE(SUM(total_price), 0)").Scan(&tradeRev)
		s.db.Model(&db.TradeLog{}).Where("company_id = ? AND action = ?", comp.ID, "sell").
			Select("COUNT(*)").Scan(&winCount)
		s.db.Model(&db.TradeLog{}).Where("company_id = ? AND action = ?", comp.ID, "buy").
			Select("COALESCE(SUM(total_price), 0)").Scan(&tradeCosts)
		s.db.Model(&db.TradeLog{}).Where("company_id = ?", comp.ID).
			Select("COUNT(*)").Scan(&tradeCount)
		s.db.Model(&db.PassengerLog{}).Where("company_id = ?", comp.ID).
			Select("COALESCE(SUM(bid), 0)").Scan(&passengerRev)

		treasury := comp.Treasury
		// Overlay live treasury only after runner has fully initialized.
		for _, runner := range runners {
			rec := runner.DBRecord()
			if rec != nil && rec.GameID == comp.GameID {
				state := runner.State()
				state.RLock()
				if state.Initialized {
					treasury = state.Treasury
				}
				state.RUnlock()
				break
			}
		}

		winRate := 0.0
		if tradeCount > 0 {
			winRate = float64(winCount) / float64(tradeCount)
		}

		result = append(result, companyPnL{
			CompanyID:    comp.ID,
			CompanyName:  comp.Name,
			Strategy:     comp.Strategy,
			Treasury:     treasury,
			TradeRev:     tradeRev,
			TradeCosts:   tradeCosts,
			PassengerRev: passengerRev,
			NetPnL:       tradeRev + passengerRev - tradeCosts,
			TradeCount:   tradeCount,
			WinCount:     winCount,
			WinRate:      winRate,
		})

		globalTradeRev += tradeRev
		globalTradeCosts += tradeCosts
		globalPassengerRev += passengerRev
		globalTradeCount += tradeCount
		globalWinCount += winCount
	}

	globalWinRate := 0.0
	if globalTradeCount > 0 {
		globalWinRate = float64(globalWinCount) / float64(globalTradeCount)
	}

	return c.JSON(fiber.Map{
		"companies": result,
		"totals": fiber.Map{
			"trade_rev":     globalTradeRev,
			"trade_costs":   globalTradeCosts,
			"passenger_rev": globalPassengerRev,
			"net_pnl":       globalTradeRev + globalPassengerRev - globalTradeCosts,
			"trade_count":   globalTradeCount,
			"win_count":     globalWinCount,
			"win_rate":      globalWinRate,
		},
	})
}

// handleCompanyMarketOrders returns P2P order activity (fills, posts, cancels) for a company.
// This queries the agent decision logs for market-type decisions.
func (s *Server) handleCompanyMarketOrders(c fiber.Ctx) error {
	companyID, err := strconv.ParseUint(c.Params("id"), 10, 64)
	if err != nil {
		return c.Status(fiber.StatusBadRequest).JSON(fiber.Map{
			"error": "invalid company id",
		})
	}

	limit := queryInt(c, "limit", 50)

	var decisions []db.AgentDecisionLog
	if err := s.db.Where("company_id = ? AND decision_type = ?", companyID, "market").
		Order("created_at DESC").
		Limit(limit).
		Find(&decisions).Error; err != nil {
		return c.Status(fiber.StatusInternalServerError).JSON(fiber.Map{
			"error": "failed to fetch market orders",
		})
	}
	return c.JSON(decisions)
}

// queryInt reads an integer query parameter with a default value.
func queryInt(c fiber.Ctx, key string, defaultVal int) int {
	val := c.Query(key)
	if val == "" {
		return defaultVal
	}
	n, err := strconv.Atoi(val)
	if err != nil || n < 0 {
		return defaultVal
	}
	return n
}

// ---------------------------------------------------------------------------
// Analytics endpoints — aggregate trade profitability data
// ---------------------------------------------------------------------------

// handleAnalyticsGoods returns profit breakdown by cargo type (good).
// Aggregates from RoutePerformance for completed buy→sell cycles and from
// TradeLog for raw volume. Supports ?hours= filter (default: all time).
func (s *Server) handleAnalyticsGoods(c fiber.Ctx) error {
	hours := c.Query("hours") // empty = all time

	type goodProfit struct {
		GoodID       string  `json:"good_id"`
		GoodName     string  `json:"good_name"`
		TotalProfit  int64   `json:"total_profit"`
		TotalRevenue int64   `json:"total_revenue"`
		TotalCost    int64   `json:"total_cost"`
		TradeCount   int64   `json:"trade_count"`
		TotalQty     int64   `json:"total_quantity"`
		AvgProfit    float64 `json:"avg_profit_per_trade"`
		WinCount     int64   `json:"win_count"`
		LossCount    int64   `json:"loss_count"`
		WinRate      float64 `json:"win_rate"`
		BestProfit   int     `json:"best_profit"`
		WorstProfit  int     `json:"worst_profit"`
		FirstTrade   string  `json:"first_trade"`
		LastTrade    string  `json:"last_trade"`
	}

	// Aggregate from RoutePerformance (completed cycles with known profit).
	query := s.db.Model(&db.RoutePerformance{})
	if hours != "" {
		if h, err := strconv.Atoi(hours); err == nil && h > 0 {
			query = query.Where("created_at > ?", time.Now().Add(-time.Duration(h)*time.Hour))
		}
	}

	type routeRow struct {
		GoodID      string
		TotalProfit int64
		TradeCount  int64
		TotalQty    int64
		WinCount    int64
		LossCount   int64
		BestProfit  int
		WorstProfit int
		FirstTrade  time.Time
		LastTrade   time.Time
	}
	var routeRows []routeRow
	query.Select(`good_id,
		SUM(profit) as total_profit,
		COUNT(*) as trade_count,
		SUM(quantity) as total_qty,
		SUM(CASE WHEN profit > 0 THEN 1 ELSE 0 END) as win_count,
		SUM(CASE WHEN profit <= 0 THEN 1 ELSE 0 END) as loss_count,
		MAX(profit) as best_profit,
		MIN(profit) as worst_profit,
		MIN(created_at) as first_trade,
		MAX(created_at) as last_trade`).
		Group("good_id").
		Scan(&routeRows)

	// Aggregate revenue/cost from TradeLog by good.
	tradeQuery := s.db.Model(&db.TradeLog{})
	if hours != "" {
		if h, err := strconv.Atoi(hours); err == nil && h > 0 {
			tradeQuery = tradeQuery.Where("created_at > ?", time.Now().Add(-time.Duration(h)*time.Hour))
		}
	}

	type tradeRow struct {
		GoodID   string
		GoodName string
		Action   string
		Total    int64
	}
	var tradeRows []tradeRow
	tradeQuery.Select("good_id, good_name, action, SUM(total_price) as total").
		Group("good_id, good_name, action").
		Scan(&tradeRows)

	// Merge data.
	type mergedGood struct {
		name    string
		rev     int64
		cost    int64
		profit  int64
		count   int64
		qty     int64
		wins    int64
		losses  int64
		best    int
		worst   int
		first   time.Time
		last    time.Time
	}
	goods := make(map[string]*mergedGood)

	for _, r := range routeRows {
		g, ok := goods[r.GoodID]
		if !ok {
			g = &mergedGood{}
			goods[r.GoodID] = g
		}
		g.profit = r.TotalProfit
		g.count = r.TradeCount
		g.qty = r.TotalQty
		g.wins = r.WinCount
		g.losses = r.LossCount
		g.best = r.BestProfit
		g.worst = r.WorstProfit
		g.first = r.FirstTrade
		g.last = r.LastTrade
	}

	for _, t := range tradeRows {
		g, ok := goods[t.GoodID]
		if !ok {
			g = &mergedGood{}
			goods[t.GoodID] = g
		}
		g.name = t.GoodName
		if t.Action == "sell" {
			g.rev = t.Total
		} else {
			g.cost = t.Total
		}
	}

	result := make([]goodProfit, 0, len(goods))
	for id, g := range goods {
		winRate := 0.0
		if g.count > 0 {
			winRate = float64(g.wins) / float64(g.count)
		}
		avgProfit := 0.0
		if g.count > 0 {
			avgProfit = float64(g.profit) / float64(g.count)
		}
		first := ""
		last := ""
		if !g.first.IsZero() {
			first = g.first.Format(time.RFC3339)
		}
		if !g.last.IsZero() {
			last = g.last.Format(time.RFC3339)
		}
		result = append(result, goodProfit{
			GoodID:       id,
			GoodName:     g.name,
			TotalProfit:  g.profit,
			TotalRevenue: g.rev,
			TotalCost:    g.cost,
			TradeCount:   g.count,
			TotalQty:     g.qty,
			AvgProfit:    avgProfit,
			WinCount:     g.wins,
			LossCount:    g.losses,
			WinRate:      winRate,
			BestProfit:   g.best,
			WorstProfit:  g.worst,
			FirstTrade:   first,
			LastTrade:    last,
		})
	}

	// Sort by total profit descending.
	for i := 0; i < len(result); i++ {
		for j := i + 1; j < len(result); j++ {
			if result[j].TotalProfit > result[i].TotalProfit {
				result[i], result[j] = result[j], result[i]
			}
		}
	}

	return c.JSON(result)
}

// handleAnalyticsRoutes returns profit breakdown by route (from_port → to_port).
// Aggregates from RoutePerformance. Supports ?hours= filter.
func (s *Server) handleAnalyticsRoutes(c fiber.Ctx) error {
	hours := c.Query("hours")

	type routeProfit struct {
		FromPortID   string  `json:"from_port_id"`
		FromPortName string  `json:"from_port_name"`
		ToPortID     string  `json:"to_port_id"`
		ToPortName   string  `json:"to_port_name"`
		TotalProfit  int64   `json:"total_profit"`
		TradeCount   int64   `json:"trade_count"`
		TotalQty     int64   `json:"total_quantity"`
		AvgProfit    float64 `json:"avg_profit_per_trade"`
		WinCount     int64   `json:"win_count"`
		LossCount    int64   `json:"loss_count"`
		WinRate      float64 `json:"win_rate"`
		TopGoodID    string  `json:"top_good_id"`
		TopGoodName  string  `json:"top_good_name"`
		FirstTrade   string  `json:"first_trade"`
		LastTrade    string  `json:"last_trade"`
	}

	query := s.db.Model(&db.RoutePerformance{})
	if hours != "" {
		if h, err := strconv.Atoi(hours); err == nil && h > 0 {
			query = query.Where("created_at > ?", time.Now().Add(-time.Duration(h)*time.Hour))
		}
	}

	type row struct {
		FromPortID  string
		ToPortID    string
		TotalProfit int64
		TradeCount  int64
		TotalQty    int64
		WinCount    int64
		LossCount   int64
		FirstTrade  time.Time
		LastTrade   time.Time
	}
	var rows []row
	query.Select(`from_port_id, to_port_id,
		SUM(profit) as total_profit,
		COUNT(*) as trade_count,
		SUM(quantity) as total_qty,
		SUM(CASE WHEN profit > 0 THEN 1 ELSE 0 END) as win_count,
		SUM(CASE WHEN profit <= 0 THEN 1 ELSE 0 END) as loss_count,
		MIN(created_at) as first_trade,
		MAX(created_at) as last_trade`).
		Group("from_port_id, to_port_id").
		Scan(&rows)

	// Build port name lookup from world data.
	portNames := s.portNameIndex()

	// For each route, find the top good by profit.
	type goodProfitRow struct {
		FromPortID string
		ToPortID   string
		GoodID     string
		GoodProfit int64
	}
	var goodRows []goodProfitRow
	goodQuery := s.db.Model(&db.RoutePerformance{})
	if hours != "" {
		if h, err := strconv.Atoi(hours); err == nil && h > 0 {
			goodQuery = goodQuery.Where("created_at > ?", time.Now().Add(-time.Duration(h)*time.Hour))
		}
	}
	goodQuery.Select("from_port_id, to_port_id, good_id, SUM(profit) as good_profit").
		Group("from_port_id, to_port_id, good_id").
		Order("good_profit DESC").
		Scan(&goodRows)

	// Top good per route.
	type routeKey struct{ from, to string }
	topGood := make(map[routeKey]string)
	for _, gr := range goodRows {
		k := routeKey{gr.FromPortID, gr.ToPortID}
		if _, exists := topGood[k]; !exists {
			topGood[k] = gr.GoodID
		}
	}

	// Good name lookup from TradeLog.
	goodNames := s.goodNameIndex()

	result := make([]routeProfit, 0, len(rows))
	for _, r := range rows {
		winRate := 0.0
		if r.TradeCount > 0 {
			winRate = float64(r.WinCount) / float64(r.TradeCount)
		}
		avgProfit := 0.0
		if r.TradeCount > 0 {
			avgProfit = float64(r.TotalProfit) / float64(r.TradeCount)
		}
		first := ""
		last := ""
		if !r.FirstTrade.IsZero() {
			first = r.FirstTrade.Format(time.RFC3339)
		}
		if !r.LastTrade.IsZero() {
			last = r.LastTrade.Format(time.RFC3339)
		}

		topGoodID := topGood[routeKey{r.FromPortID, r.ToPortID}]

		result = append(result, routeProfit{
			FromPortID:   r.FromPortID,
			FromPortName: portNames[r.FromPortID],
			ToPortID:     r.ToPortID,
			ToPortName:   portNames[r.ToPortID],
			TotalProfit:  r.TotalProfit,
			TradeCount:   r.TradeCount,
			TotalQty:     r.TotalQty,
			AvgProfit:    avgProfit,
			WinCount:     r.WinCount,
			LossCount:    r.LossCount,
			WinRate:      winRate,
			TopGoodID:    topGoodID,
			TopGoodName:  goodNames[topGoodID],
			FirstTrade:   first,
			LastTrade:    last,
		})
	}

	// Sort by total profit descending.
	for i := 0; i < len(result); i++ {
		for j := i + 1; j < len(result); j++ {
			if result[j].TotalProfit > result[i].TotalProfit {
				result[i], result[j] = result[j], result[i]
			}
		}
	}

	return c.JSON(result)
}

// handleAnalyticsTimeline returns profit over time, bucketed by hour.
// Supports ?group_by=good|route|strategy (default: good) and ?hours= filter.
func (s *Server) handleAnalyticsTimeline(c fiber.Ctx) error {
	hours := c.Query("hours", "168") // default 7 days
	groupBy := c.Query("group_by", "good")

	h, err := strconv.Atoi(hours)
	if err != nil || h < 1 {
		h = 168
	}
	since := time.Now().Add(-time.Duration(h) * time.Hour)

	// Determine bucket size: if >48h, bucket by day; else by hour.
	bucketFormat := "2006-01-02 15:00"
	bucketLabel := "hour"
	if h > 48 {
		bucketFormat = "2006-01-02"
		bucketLabel = "day"
	}

	var perfs []db.RoutePerformance
	s.db.Where("created_at > ?", since).Order("created_at ASC").Find(&perfs)

	goodNames := s.goodNameIndex()
	portNames := s.portNameIndex()

	type bucket struct {
		Profit int64 `json:"profit"`
		Count  int64 `json:"count"`
		Qty    int64 `json:"quantity"`
	}

	// series key → time bucket → aggregated data
	series := make(map[string]map[string]*bucket)

	for _, p := range perfs {
		var key string
		switch groupBy {
		case "route":
			fromName := portNames[p.FromPortID]
			toName := portNames[p.ToPortID]
			if fromName == "" {
				fromName = p.FromPortID[:8]
			}
			if toName == "" {
				toName = p.ToPortID[:8]
			}
			key = fromName + " → " + toName
		case "strategy":
			key = p.Strategy
		default: // "good"
			key = goodNames[p.GoodID]
			if key == "" {
				key = p.GoodID[:8]
			}
		}

		timeBucket := p.CreatedAt.Format(bucketFormat)

		if series[key] == nil {
			series[key] = make(map[string]*bucket)
		}
		b, ok := series[key][timeBucket]
		if !ok {
			b = &bucket{}
			series[key][timeBucket] = b
		}
		b.Profit += int64(p.Profit)
		b.Count++
		b.Qty += int64(p.Quantity)
	}

	// Flatten into response.
	type timePoint struct {
		Time     string `json:"time"`
		Profit   int64  `json:"profit"`
		Count    int64  `json:"count"`
		Quantity int64  `json:"quantity"`
	}
	type seriesEntry struct {
		Name   string      `json:"name"`
		Points []timePoint `json:"points"`
		Total  int64       `json:"total_profit"`
	}

	result := make([]seriesEntry, 0, len(series))
	for name, buckets := range series {
		entry := seriesEntry{Name: name}
		for t, b := range buckets {
			entry.Points = append(entry.Points, timePoint{
				Time: t, Profit: b.Profit, Count: b.Count, Quantity: b.Qty,
			})
			entry.Total += b.Profit
		}
		// Sort points by time.
		for i := 0; i < len(entry.Points); i++ {
			for j := i + 1; j < len(entry.Points); j++ {
				if entry.Points[j].Time < entry.Points[i].Time {
					entry.Points[i], entry.Points[j] = entry.Points[j], entry.Points[i]
				}
			}
		}
		result = append(result, entry)
	}

	// Sort series by total profit descending.
	for i := 0; i < len(result); i++ {
		for j := i + 1; j < len(result); j++ {
			if result[j].Total > result[i].Total {
				result[i], result[j] = result[j], result[i]
			}
		}
	}

	return c.JSON(fiber.Map{
		"bucket_size": bucketLabel,
		"hours":       h,
		"series":      result,
	})
}

// portNameIndex builds a map of port UUID string → port name from world data.
func (s *Server) portNameIndex() map[string]string {
	names := make(map[string]string)
	world := s.manager.WorldData()
	if world != nil {
		for _, p := range world.Ports {
			names[p.ID.String()] = p.Name
		}
	}
	return names
}

// goodNameIndex builds a map of good UUID string → good name from trade logs.
func (s *Server) goodNameIndex() map[string]string {
	names := make(map[string]string)
	type nameRow struct {
		GoodID   string
		GoodName string
	}
	var rows []nameRow
	s.db.Model(&db.TradeLog{}).Select("DISTINCT good_id, good_name").Scan(&rows)
	for _, r := range rows {
		names[r.GoodID] = r.GoodName
	}
	return names
}
