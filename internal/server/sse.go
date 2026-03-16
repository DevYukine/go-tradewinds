package server

import (
	"bufio"
	"encoding/json"
	"fmt"
	"strconv"
	"sync"
	"time"

	"github.com/gofiber/fiber/v3"
	"go.uber.org/zap"

	"github.com/DevYukine/go-tradewinds/internal/bot"
	"github.com/DevYukine/go-tradewinds/internal/db"
)

// registerSSE sets up the SSE streaming endpoints.
func (s *Server) registerSSE() {
	sse := s.app.Group("/sse")

	sse.Get("/logs/:id", s.handleSSELogs)
	sse.Get("/pnl/:id", s.handleSSEPnL)
	sse.Get("/events/:id", s.handleSSEEvents)
	sse.Get("/global-events", s.handleSSEGlobalEvents)
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

		s.logger.Debug("SSE log stream started",
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

	// Read optional since_id to skip already-fetched snapshots.
	sinceID := uint(0)
	if sid := c.Query("since_id"); sid != "" {
		if parsed, err := strconv.ParseUint(sid, 10, 64); err == nil {
			sinceID = uint(parsed)
		}
	}

	return c.SendStreamWriter(func(w *bufio.Writer) {
		s.logger.Debug("SSE PnL stream started",
			zap.Uint64("company_id", companyID),
			zap.Uint("since_id", sinceID),
		)

		lastID := sinceID
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

// handleSSEEvents streams real-time state change notifications for a company.
// The dashboard uses these to trigger immediate re-fetches instead of waiting
// for the next poll cycle.
func (s *Server) handleSSEEvents(c fiber.Ctx) error {
	companyID, err := strconv.ParseUint(c.Params("id"), 10, 64)
	if err != nil {
		return c.Status(fiber.StatusBadRequest).JSON(fiber.Map{
			"error": "invalid company id",
		})
	}

	var record db.CompanyRecord
	if err := s.db.First(&record, companyID).Error; err != nil {
		return c.Status(fiber.StatusNotFound).JSON(fiber.Map{
			"error": "company not found",
		})
	}

	runner := s.manager.GetRunner(record.GameID)
	if runner == nil {
		return c.Status(fiber.StatusNotFound).JSON(fiber.Map{
			"error": "company runner not found",
		})
	}

	subID, ch := runner.Events().Subscribe()

	c.Set("Content-Type", "text/event-stream")
	c.Set("Cache-Control", "no-cache")
	c.Set("Connection", "keep-alive")
	c.Set("Transfer-Encoding", "chunked")

	return c.SendStreamWriter(func(w *bufio.Writer) {
		defer runner.Events().Unsubscribe(subID)

		s.logger.Debug("SSE events stream started",
			zap.Uint64("company_id", companyID),
			zap.Int("sub_id", subID),
		)

		for event := range ch {
			data, err := json.Marshal(event)
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

// handleSSEGlobalEvents multiplexes state change events from ALL running
// companies into a single SSE stream. This lets the overview page and world
// map receive instant updates without opening one SSE connection per company
// (which would exceed the browser's 6-connection HTTP/1.1 limit).
func (s *Server) handleSSEGlobalEvents(c fiber.Ctx) error {
	c.Set("Content-Type", "text/event-stream")
	c.Set("Cache-Control", "no-cache")
	c.Set("Connection", "keep-alive")
	c.Set("Transfer-Encoding", "chunked")

	return c.SendStreamWriter(func(w *bufio.Writer) {
		s.logger.Debug("SSE global events stream started")

		// Merged channel receives events from all companies.
		merged := make(chan globalEvent, 64)
		done := make(chan struct{})

		// Goroutine that subscribes to all runners and forwards events.
		go func() {
			type sub struct {
				runner *bot.CompanyRunner
				subID  int
				ch     <-chan bot.StateEvent
			}

			var subs []sub
			var wg sync.WaitGroup

			// Subscribe to all current runners.
			companies := s.manager.Companies()
			for gameID, runner := range companies {
				subID, ch := runner.Events().Subscribe()
				subs = append(subs, sub{runner: runner, subID: subID, ch: ch})

				// Look up DB company ID for the frontend.
				var record db.CompanyRecord
				if err := s.db.Where("game_id = ?", gameID).First(&record).Error; err != nil {
					continue
				}

				// Forward events from this runner to merged channel.
				wg.Add(1)
				go func(ch <-chan bot.StateEvent, companyID uint) {
					defer wg.Done()
					for event := range ch {
						select {
						case merged <- globalEvent{
							Type:      event.Type,
							Timestamp: event.Timestamp,
							CompanyID: companyID,
						}:
						case <-done:
							return
						}
					}
				}(ch, record.ID)
			}

			// Block until client disconnects.
			<-done

			// Unsubscribe all — this closes the per-runner channels,
			// causing forwarder goroutines to exit via range loop.
			for _, s := range subs {
				s.runner.Events().Unsubscribe(s.subID)
			}

			// Wait for all forwarders to finish before closing merged.
			wg.Wait()
			close(merged)
		}()

		// Stream merged events to client.
		for event := range merged {
			data, err := json.Marshal(event)
			if err != nil {
				continue
			}

			if _, err := fmt.Fprintf(w, "data: %s\n\n", data); err != nil {
				close(done)
				return
			}
			if err := w.Flush(); err != nil {
				close(done)
				return
			}
		}
	})
}

// globalEvent extends StateEvent with the company ID so the frontend knows
// which company the event originated from.
type globalEvent struct {
	Type      string `json:"type"`
	Timestamp int64  `json:"ts"`
	CompanyID uint   `json:"company_id"`
}
