package api

import (
	"bufio"
	"context"
	"crypto/tls"
	"encoding/json"
	"net/http"
	"strings"
	"time"

	"golang.org/x/net/http2"

	"go.uber.org/zap"
)

const (
	sseReconnectDelay    = 2 * time.Second
	sseMaxReconnectDelay = 5 * time.Minute
	sseMaxRetries        = 10
)

// EventHandler processes incoming SSE events.
type EventHandler func(event SSEEvent)

// EventStream represents a long-lived SSE connection that automatically
// reconnects on disconnect. It does NOT count against rate limits since
// SSE connections are long-lived.
type EventStream struct {
	cancel context.CancelFunc
}

// Close terminates the SSE connection.
func (es *EventStream) Close() {
	if es.cancel != nil {
		es.cancel()
	}
}

// SubscribeWorldEvents opens a long-lived SSE connection to the world event stream.
// This stream is public (no auth required) and broadcasts ship movements,
// company formations, and economy events.
func (c *Client) SubscribeWorldEvents(ctx context.Context, handler EventHandler) *EventStream {
	ctx, cancel := context.WithCancel(ctx)
	stream := &EventStream{cancel: cancel}

	go c.runSSELoop(ctx, "/world/events", false, handler)

	return stream
}

// SubscribeCompanyEvents opens a long-lived SSE connection to the company event stream.
// This stream is private (requires auth + company header) and broadcasts events
// for the specific company: ship arrivals, trade completions, ledger updates.
func (c *Client) SubscribeCompanyEvents(ctx context.Context, handler EventHandler) *EventStream {
	ctx, cancel := context.WithCancel(ctx)
	stream := &EventStream{cancel: cancel}

	go c.runSSELoop(ctx, "/company/events", true, handler)

	return stream
}

// runSSELoop manages the SSE connection lifecycle with automatic reconnection.
// After sseMaxRetries consecutive failures it stops retrying and logs once,
// falling back to poll-only mode.
func (c *Client) runSSELoop(ctx context.Context, path string, requiresAuth bool, handler EventHandler) {
	delay := sseReconnectDelay
	failures := 0

	for {
		if err := ctx.Err(); err != nil {
			return
		}

		err := c.connectSSE(ctx, path, requiresAuth, handler)
		if err != nil {
			if ctx.Err() != nil {
				return // Context cancelled, clean shutdown.
			}

			failures++
			if failures >= sseMaxRetries {
				c.logger.Warn("SSE connection failed repeatedly, giving up — falling back to poll-only mode",
					zap.String("path", path),
					zap.Int("attempts", failures),
					zap.Error(err),
				)
				return
			}

			c.logger.Warn("SSE connection lost, reconnecting",
				zap.String("path", path),
				zap.Error(err),
				zap.Duration("delay", delay),
				zap.Int("attempt", failures),
			)
		} else {
			// Connection was established then dropped — reset backoff.
			delay = sseReconnectDelay
			failures = 0
		}

		// Wait before reconnecting with exponential backoff.
		select {
		case <-ctx.Done():
			return
		case <-time.After(delay):
			delay = min(delay*2, sseMaxReconnectDelay)
		}
	}
}

// connectSSE establishes a single SSE connection and processes events until
// the connection drops or the context is cancelled.
func (c *Client) connectSSE(ctx context.Context, path string, requiresAuth bool, handler EventHandler) error {
	fullURL := c.baseURL + apiPrefix + path

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, fullURL, nil)
	if err != nil {
		return err
	}

	req.Header.Set("Accept", "text/event-stream")
	req.Header.Set("Cache-Control", "no-cache")

	if requiresAuth {
		c.mu.RLock()
		token := c.token
		c.mu.RUnlock()

		if token != "" {
			req.Header.Set(headerAuth, "Bearer "+token)
		}
		if c.companyID != "" {
			req.Header.Set(headerCompanyID, c.companyID)
		}
	}

	// Use a separate HTTP client with HTTP/2 transport for long-lived SSE connections.
	sseClient := &http.Client{
		Transport: &http2.Transport{
			TLSClientConfig: &tls.Config{},
		},
	}
	resp, err := sseClient.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return &requestError{statusCode: resp.StatusCode, body: "SSE connection rejected"}
	}

	c.logger.Info("SSE connection established", zap.String("path", path))

	scanner := bufio.NewScanner(resp.Body)
	var eventData strings.Builder

	for scanner.Scan() {
		if ctx.Err() != nil {
			return nil
		}

		line := scanner.Text()

		switch {
		case strings.HasPrefix(line, "data:"):
			data := strings.TrimPrefix(line, "data:")
			data = strings.TrimSpace(data)
			eventData.WriteString(data)

		case line == "":
			// Empty line = end of event.
			if eventData.Len() > 0 {
				c.processSSEEvent(eventData.String(), handler)
				eventData.Reset()
			}

		// Ignore "event:", "id:", "retry:" lines for now.
		}
	}

	if err := scanner.Err(); err != nil {
		return err
	}

	return nil
}

// processSSEEvent parses and dispatches a single SSE event.
func (c *Client) processSSEEvent(data string, handler EventHandler) {
	var event SSEEvent
	if err := json.Unmarshal([]byte(data), &event); err != nil {
		c.logger.Debug("failed to parse SSE event", zap.String("data", data), zap.Error(err))
		return
	}

	c.logger.Debug("SSE event received", zap.String("type", event.Type))
	handler(event)
}

// ParseShipDockedEvent parses the data field of a ship_docked_world event.
func ParseShipDockedEvent(data json.RawMessage) (*ShipDockedEvent, error) {
	var event ShipDockedEvent
	if err := json.Unmarshal(data, &event); err != nil {
		return nil, err
	}
	return &event, nil
}

// ParseShipSetSailEvent parses the data field of a ship_set_sail event.
func ParseShipSetSailEvent(data json.RawMessage) (*ShipSetSailEvent, error) {
	var event ShipSetSailEvent
	if err := json.Unmarshal(data, &event); err != nil {
		return nil, err
	}
	return &event, nil
}

// ParseShipBoughtEvent parses the data field of a ship_bought event.
func ParseShipBoughtEvent(data json.RawMessage) (*ShipBoughtEvent, error) {
	var event ShipBoughtEvent
	if err := json.Unmarshal(data, &event); err != nil {
		return nil, err
	}
	return &event, nil
}

// ParseCompanyFormedEvent parses the data field of a company_formed event.
func ParseCompanyFormedEvent(data json.RawMessage) (*CompanyFormedEvent, error) {
	var event CompanyFormedEvent
	if err := json.Unmarshal(data, &event); err != nil {
		return nil, err
	}
	return &event, nil
}
