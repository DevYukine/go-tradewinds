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
}

// handleCompanies returns all companies with their current status, treasury, and strategy.
// Treasury and reputation are enriched from live in-memory state when available.
func (s *Server) handleCompanies(c fiber.Ctx) error {
	var companies []db.CompanyRecord
	if err := s.db.Order("id ASC").Find(&companies).Error; err != nil {
		return c.Status(fiber.StatusInternalServerError).JSON(fiber.Map{
			"error": "failed to fetch companies",
		})
	}

	// Overlay live treasury/reputation from running companies.
	runners := s.manager.Companies()
	for i := range companies {
		for _, runner := range runners {
			rec := runner.DBRecord()
			if rec != nil && rec.GameID == companies[i].GameID {
				state := runner.State()
				state.RLock()
				companies[i].Treasury = state.Treasury
				companies[i].Reputation = state.Reputation
				state.RUnlock()
				break
			}
		}
	}

	return c.JSON(companies)
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

	ships := make([]shipDetail, 0, len(state.Ships))
	for _, ss := range state.Ships {
		cargo := make([]cargoItem, len(ss.Cargo))
		cargoTotal := 0
		for i, ci := range ss.Cargo {
			goodName := ci.GoodID.String()
			if world != nil {
				if g := world.GetGood(ci.GoodID); g != nil {
					goodName = g.Name
				}
			}
			cargo[i] = cargoItem{
				GoodID:   ci.GoodID.String(),
				GoodName: goodName,
				Quantity: ci.Quantity,
			}
			cargoTotal += ci.Quantity
		}

		sd := shipDetail{
			ShipID:         ss.Ship.ID.String(),
			ShipName:       ss.Ship.Name,
			Status:         ss.Ship.Status,
			Cargo:          cargo,
			CargoTotal:     cargoTotal,
			PassengerCount: ss.PassengerCount,
		}

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

	return c.JSON(fiber.Map{
		"company_id":    company.GameID,
		"treasury":      state.Treasury,
		"total_upkeep":  state.TotalUpkeep,
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

	// Hardcoded coordinates for European port cities.
	portCoordinates := map[string][2]float64{
		"Rotterdam":  {51.9225, 4.4792},
		"Plymouth":   {50.3755, -4.1427},
		"Portsmouth": {50.8198, -1.0880},
		"Amsterdam":  {52.3676, 4.9041},
		"Hull":       {53.7457, -0.3367},
		"Bremen":     {53.0793, 8.8017},
		"Bristol":    {51.4545, -2.5879},
		"Dublin":     {53.3498, -6.2603},
		"Dunkirk":    {51.0343, 2.3768},
		"Edinburgh":  {55.9533, -3.1883},
		"Calais":     {50.9513, 1.8587},
		"Hamburg":    {53.5511, 9.9937},
		"Antwerp":    {51.2194, 4.4025},
		"Glasgow":    {55.8642, -4.2518},
		"London":     {51.5074, -0.1278},
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

	// Build shipyard port set for quick lookup.
	shipyardSet := make(map[uuid.UUID]bool, len(world.ShipyardPorts))
	for _, id := range world.ShipyardPorts {
		shipyardSet[id] = true
	}

	ports := make([]portInfo, len(world.Ports))
	for i, p := range world.Ports {
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

	goods := make([]goodInfo, len(world.Goods))
	for i, g := range world.Goods {
		goods[i] = goodInfo{
			ID:          g.ID.String(),
			Name:        g.Name,
			Description: g.Description,
			Category:    g.Category,
		}
	}

	routes := make([]routeInfo, len(world.Routes))
	for i, r := range world.Routes {
		fromName, toName := r.FromID.String(), r.ToID.String()
		if from := world.GetPort(r.FromID); from != nil {
			fromName = from.Name
		}
		if to := world.GetPort(r.ToID); to != nil {
			toName = to.Name
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

	shipTypes := make([]shipTypeInfo, len(world.ShipTypes))
	for i, st := range world.ShipTypes {
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
func (s *Server) handleAllShips(c fiber.Ctx) error {
	world := s.manager.WorldData()
	companies := s.manager.Companies()

	type shipInfo struct {
		ShipID        string     `json:"ship_id"`
		ShipName      string     `json:"ship_name"`
		ShipType      string     `json:"ship_type"`
		Status        string     `json:"status"`
		CompanyID     string     `json:"company_id"`
		CompanyName   string     `json:"company_name"`
		Strategy      string     `json:"strategy"`
		PortID        string     `json:"port_id,omitempty"`
		PortName      string     `json:"port_name,omitempty"`
		RouteID       string     `json:"route_id,omitempty"`
		FromPortID    string     `json:"from_port_id,omitempty"`
		ToPortID      string     `json:"to_port_id,omitempty"`
		FromPortName  string     `json:"from_port_name,omitempty"`
		ToPortName    string     `json:"to_port_name,omitempty"`
		ArrivingAt    *time.Time `json:"arriving_at,omitempty"`
		CargoTotal    int        `json:"cargo_total"`
		Capacity      int        `json:"capacity"`
	}

	var ships []shipInfo
	for _, runner := range companies {
		state := runner.State()
		state.RLock()
		record := runner.DBRecord()

		for _, ss := range state.Ships {
			cargoTotal := 0
			for _, ci := range ss.Cargo {
				cargoTotal += ci.Quantity
			}

			si := shipInfo{
				ShipID:      ss.Ship.ID.String(),
				ShipName:    ss.Ship.Name,
				Status:      ss.Ship.Status,
				CompanyID:   record.GameID,
				CompanyName: record.Name,
				Strategy:    record.Strategy,
				CargoTotal:  cargoTotal,
			}

			if world != nil {
				if st := world.GetShipType(ss.Ship.ShipTypeID); st != nil {
					si.ShipType = st.Name
					si.Capacity = st.Capacity
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
