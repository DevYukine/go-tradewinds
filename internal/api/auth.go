package api

import (
	"context"
	"net/http"
)

// Register creates a new player account.
func (c *Client) Register(ctx context.Context, req RegisterRequest) (*Player, error) {
	var player Player
	if err := c.do(ctx, http.MethodPost, "/auth/register", req, &player, PriorityNormal); err != nil {
		return nil, err
	}
	return &player, nil
}

// Login authenticates with the game API and stores the JWT token on the client.
// Returns the token string for reference.
func (c *Client) Login(ctx context.Context, email, password string) (string, error) {
	req := LoginRequest{
		Email:    email,
		Password: password,
	}

	var resp LoginResponse
	if err := c.do(ctx, http.MethodPost, "/auth/login", req, &resp, PriorityHigh); err != nil {
		return "", err
	}

	c.SetToken(resp.Token)
	return resp.Token, nil
}

// Revoke invalidates the current JWT token (logout).
func (c *Client) Revoke(ctx context.Context) error {
	return c.do(ctx, http.MethodPost, "/auth/revoke", nil, nil, PriorityNormal)
}

// Me returns the current player's profile.
func (c *Client) Me(ctx context.Context) (*Player, error) {
	var player Player
	if err := c.do(ctx, http.MethodGet, "/me", nil, &player, PriorityNormal); err != nil {
		return nil, err
	}
	return &player, nil
}
