package api

import (
	"context"
	"net/http"
	"strconv"
)

// itoa converts an int to a string. Convenience wrapper around strconv.Itoa.
func itoa(i int) string {
	return strconv.Itoa(i)
}

// Health checks the server health status.
func (c *Client) Health(ctx context.Context) (*HealthResponse, error) {
	var health HealthResponse
	if err := c.do(ctx, http.MethodGet, "/health", nil, &health, PriorityLow); err != nil {
		return nil, err
	}
	return &health, nil
}
