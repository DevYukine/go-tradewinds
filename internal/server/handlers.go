package server

import (
	"strconv"
	"time"

	"github.com/gofiber/fiber/v3"

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
	api.Get("/strategy-metrics", s.handleStrategyMetrics)
	api.Get("/optimizer/log", s.handleOptimizerLog)
	api.Get("/prices", s.handlePrices)
	api.Get("/ratelimit", s.handleRateLimit)
	api.Get("/health", s.handleHealth)
}

// handleCompanies returns all companies with their current status, treasury, and strategy.
func (s *Server) handleCompanies(c fiber.Ctx) error {
	var companies []db.CompanyRecord
	if err := s.db.Order("id ASC").Find(&companies).Error; err != nil {
		return c.Status(fiber.StatusInternalServerError).JSON(fiber.Map{
			"error": "failed to fetch companies",
		})
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
	if err := query.Find(&snapshots).Error; err != nil {
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

// handleCompanyInventory returns the current in-memory state for a company,
// including full ship details (location, arrival time, cargo) and warehouses.
func (s *Server) handleCompanyInventory(c fiber.Ctx) error {
	companyID := c.Params("id")

	runner := s.manager.GetRunner(companyID)
	if runner == nil {
		return c.Status(fiber.StatusNotFound).JSON(fiber.Map{
			"error": "company not found or not running",
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
		ShipID     string      `json:"ship_id"`
		ShipName   string      `json:"ship_name"`
		Status     string      `json:"status"`
		PortID     string      `json:"port_id,omitempty"`
		PortName   string      `json:"port_name,omitempty"`
		RouteID    string      `json:"route_id,omitempty"`
		ArrivingAt *time.Time  `json:"arriving_at,omitempty"`
		Cargo      []cargoItem `json:"cargo"`
		CargoTotal int         `json:"cargo_total"`
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
			ShipID:     ss.Ship.ID.String(),
			ShipName:   ss.Ship.Name,
			Status:     ss.Ship.Status,
			Cargo:      cargo,
			CargoTotal: cargoTotal,
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
		"company_id":    companyID,
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

// handlePrices returns the latest price observations.
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
	return c.JSON(prices)
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
