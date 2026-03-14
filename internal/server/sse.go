package server

import (
	"bufio"
	"encoding/json"
	"fmt"
	"strconv"
	"time"

	"github.com/gofiber/fiber/v3"
	"go.uber.org/zap"

	"github.com/DevYukine/go-tradewinds/internal/db"
)

// registerSSE sets up the SSE streaming endpoints.
func (s *Server) registerSSE() {
	sse := s.app.Group("/sse")

	sse.Get("/logs/:id", s.handleSSELogs)
	sse.Get("/pnl/:id", s.handleSSEPnL)
}

// handleSSELogs streams live log entries for a specific company.
func (s *Server) handleSSELogs(c fiber.Ctx) error {
	companyID, err := strconv.ParseUint(c.Params("id"), 10, 64)
	if err != nil {
		return c.Status(fiber.StatusBadRequest).JSON(fiber.Map{
			"error": "invalid company id",
		})
	}

	// Look up the company record to find its game ID.
	var record db.CompanyRecord
	if err := s.db.First(&record, companyID).Error; err != nil {
		return c.Status(fiber.StatusNotFound).JSON(fiber.Map{
			"error": "company not found",
		})
	}

	// Find the runner by game ID.
	runner := s.manager.GetRunner(record.GameID)
	if runner == nil {
		return c.Status(fiber.StatusNotFound).JSON(fiber.Map{
			"error": "company runner not found",
		})
	}

	// Subscribe to live logs.
	subID, ch := runner.Logger().Subscribe()

	c.Set("Content-Type", "text/event-stream")
	c.Set("Cache-Control", "no-cache")
	c.Set("Connection", "keep-alive")
	c.Set("Transfer-Encoding", "chunked")

	return c.SendStreamWriter(func(w *bufio.Writer) {
		defer runner.Logger().Unsubscribe(subID)

		s.logger.Info("SSE log stream started",
			zap.Uint64("company_id", companyID),
			zap.Int("sub_id", subID),
		)

		for entry := range ch {
			data, err := json.Marshal(map[string]any{
				"level":      entry.Level,
				"message":    entry.Message,
				"created_at": entry.CreatedAt.Format(time.RFC3339Nano),
			})
			if err != nil {
				continue
			}

			if _, err := fmt.Fprintf(w, "data: %s\n\n", data); err != nil {
				return
			}
			if err := w.Flush(); err != nil {
				return
			}
		}
	})
}

// handleSSEPnL streams live P&L updates for a specific company by polling the
// database every 5 seconds.
func (s *Server) handleSSEPnL(c fiber.Ctx) error {
	companyID, err := strconv.ParseUint(c.Params("id"), 10, 64)
	if err != nil {
		return c.Status(fiber.StatusBadRequest).JSON(fiber.Map{
			"error": "invalid company id",
		})
	}

	c.Set("Content-Type", "text/event-stream")
	c.Set("Cache-Control", "no-cache")
	c.Set("Connection", "keep-alive")
	c.Set("Transfer-Encoding", "chunked")

	return c.SendStreamWriter(func(w *bufio.Writer) {
		s.logger.Info("SSE PnL stream started",
			zap.Uint64("company_id", companyID),
		)

		lastID := uint(0)
		ticker := time.NewTicker(5 * time.Second)
		defer ticker.Stop()

		// Send initial batch.
		s.sendPnLUpdates(w, &lastID, companyID)

		for range ticker.C {
			if !s.sendPnLUpdates(w, &lastID, companyID) {
				return
			}
		}
	})
}

// sendPnLUpdates queries for new PnL snapshots since lastID for a specific
// company, sends them as SSE, and updates lastID. Returns false if the write
// failed (client disconnected).
func (s *Server) sendPnLUpdates(w *bufio.Writer, lastID *uint, companyID uint64) bool {
	var snapshots []db.PnLSnapshot
	query := s.db.Where("id > ? AND company_id = ?", *lastID, companyID).Order("id ASC").Limit(100)
	if err := query.Find(&snapshots).Error; err != nil {
		s.logger.Error("failed to query PnL snapshots for SSE", zap.Error(err))
		return true // Keep connection open, just skip this cycle.
	}

	for _, snap := range snapshots {
		data, err := json.Marshal(snap)
		if err != nil {
			continue
		}
		if _, err := fmt.Fprintf(w, "data: %s\n\n", data); err != nil {
			return false
		}
		*lastID = snap.ID
	}

	if len(snapshots) > 0 {
		if err := w.Flush(); err != nil {
			return false
		}
	}

	return true
}
