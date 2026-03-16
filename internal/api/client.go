package api

import (
	"context"
	"encoding/json"
	"fmt"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"time"

	"go.uber.org/zap"
	"resty.dev/v3"
)

const (
	apiPrefix       = "/api/v1"
	headerAuth      = "Authorization"
	headerCompanyID = "tradewinds-company-id"

	maxRetries     = 3
	initialBackoff = 500 * time.Millisecond
)

// Client is the HTTP client for the Tradewinds game API. It uses go-resty/v3
// for HTTP requests, and routes all calls through a shared RateLimiter.
//
// A single Client is created per company via ForCompany(). All clones share
// the same underlying resty.Client, JWT token, and RateLimiter.
type Client struct {
	baseURL     string
	resty       *resty.Client
	rateLimiter *RateLimiter

	token     string // JWT, set after login.
	companyID string // Game company UUID, set per company.
	mu        sync.RWMutex

	logger *zap.Logger
}

// NewClient creates a new API client targeting the given base URL.
func NewClient(baseURL string, rateLimiter *RateLimiter, logger *zap.Logger) *Client {
	restyClient := resty.New().
		SetBaseURL(strings.TrimRight(baseURL, "/") + apiPrefix).
		SetTimeout(30 * time.Second).
		SetHeader("Accept", "application/json").
		SetHeader("Content-Type", "application/json")

	return &Client{
		baseURL:     strings.TrimRight(baseURL, "/"),
		resty:       restyClient,
		rateLimiter: rateLimiter,
		logger:      logger.Named("api_client"),
	}
}

// ForCompany creates a lightweight copy of the client bound to a specific company.
// The clone shares the same resty.Client, token, and RateLimiter.
func (c *Client) ForCompany(companyID string) *Client {
	c.mu.RLock()
	token := c.token
	c.mu.RUnlock()

	return &Client{
		baseURL:     c.baseURL,
		resty:       c.resty,
		rateLimiter: c.rateLimiter,
		token:       token,
		companyID:   companyID,
		logger:      c.logger.With(zap.String("company_id", companyID)),
	}
}

// SetToken updates the JWT token used for authenticated requests.
func (c *Client) SetToken(token string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.token = token
}

// Token returns the current JWT token.
func (c *Client) Token() string {
	c.mu.RLock()
	defer c.mu.RUnlock()
	return c.token
}

// SetCompanyID sets the company context for this client.
func (c *Client) SetCompanyID(id string) {
	c.companyID = id
}

// CompanyID returns the current company ID.
func (c *Client) CompanyID() string {
	return c.companyID
}

// BaseURL returns the raw base URL (without the API prefix) for SSE connections.
func (c *Client) BaseURL() string {
	return c.baseURL
}

// do executes an HTTP request with rate limiting, retries, and response parsing.
// The result parameter should be a pointer to the expected response data type
// (the { data: ... } envelope is unwrapped automatically).
func (c *Client) do(ctx context.Context, method, path string, body any, result any, priority Priority) error {
	if err := c.rateLimiter.Acquire(ctx, priority); err != nil {
		return fmt.Errorf("rate limit acquisition failed: %w", err)
	}

	var lastErr error
	for attempt := range maxRetries {
		lastErr = c.doOnce(ctx, method, path, body, result)
		if lastErr == nil {
			return nil
		}

		apiErr, ok := lastErr.(*requestError)
		if !ok {
			return lastErr
		}

		switch {
		case apiErr.statusCode == 429:
			retryAfter := parseRetryAfter(apiErr.retryAfterHeader)
			c.rateLimiter.RecordBackoff(retryAfter)

			c.logger.Warn("rate limited by server, retrying",
				zap.Int("attempt", attempt+1),
				zap.Duration("retry_after", retryAfter),
			)

			select {
			case <-ctx.Done():
				return ctx.Err()
			case <-time.After(retryAfter):
				if err := c.rateLimiter.Acquire(ctx, priority); err != nil {
					return err
				}
			}

		case apiErr.statusCode == 401:
			c.logger.Warn("401 Unauthorized",
				zap.String("method", method),
				zap.String("path", path),
				zap.String("body", apiErr.body),
			)
			return lastErr

		case apiErr.statusCode >= 500:
			backoff := initialBackoff * time.Duration(1<<uint(attempt))

			c.logger.Warn("server error, retrying with backoff",
				zap.Int("status", apiErr.statusCode),
				zap.Int("attempt", attempt+1),
				zap.Duration("backoff", backoff),
			)

			select {
			case <-ctx.Done():
				return ctx.Err()
			case <-time.After(backoff):
				if err := c.rateLimiter.Acquire(ctx, priority); err != nil {
					return err
				}
			}

		default:
			return lastErr
		}
	}

	return fmt.Errorf("max retries (%d) exceeded: %w", maxRetries, lastErr)
}

// doOnce performs a single HTTP request without retries using resty.
func (c *Client) doOnce(ctx context.Context, method, path string, body any, result any) error {
	req := c.resty.R().SetContext(ctx)

	// Set auth and company headers.
	c.mu.RLock()
	token := c.token
	c.mu.RUnlock()

	if token != "" {
		req.SetHeader(headerAuth, "Bearer "+token)
	}
	if c.companyID != "" {
		req.SetHeader(headerCompanyID, c.companyID)
	}

	if body != nil {
		req.SetBody(body)
	}

	c.logger.Debug("API request",
		zap.String("method", method),
		zap.String("path", path),
	)

	resp, err := req.Execute(method, path)
	if err != nil {
		return fmt.Errorf("request failed: %w", err)
	}

	c.logger.Debug("API response",
		zap.String("method", method),
		zap.String("path", path),
		zap.Int("status", resp.StatusCode()),
	)

	// Handle error responses.
	if resp.StatusCode() >= 400 {
		return newRequestError(resp)
	}

	// Handle 204 No Content or nil result.
	if resp.StatusCode() == 204 || result == nil {
		return nil
	}

	// Unwrap the { data: ... } envelope.
	respBody := resp.Bytes()
	var envelope struct {
		Data json.RawMessage `json:"data"`
	}
	if err := json.Unmarshal(respBody, &envelope); err != nil {
		// Envelope parsing failed — try parsing directly.
		if err := json.Unmarshal(respBody, result); err != nil {
			return fmt.Errorf("failed to parse response: %w", err)
		}
		return nil
	}

	if envelope.Data != nil {
		if err := json.Unmarshal(envelope.Data, result); err != nil {
			return fmt.Errorf("failed to parse response data: %w", err)
		}
	}

	return nil
}

// Paginate fetches all pages for a paginated endpoint and returns the
// accumulated results. The path should include any base query parameters.
func Paginate[T any](ctx context.Context, c *Client, path string, priority Priority) ([]T, error) {
	var allResults []T
	cursor := ""

	for {
		separator := "?"
		if strings.Contains(path, "?") {
			separator = "&"
		}

		pageURL := path + separator + "limit=100"
		if cursor != "" {
			pageURL += "&after=" + url.QueryEscape(cursor)
		}

		var page []T
		if err := c.do(ctx, "GET", pageURL, nil, &page, priority); err != nil {
			return nil, fmt.Errorf("pagination failed at cursor %q: %w", cursor, err)
		}

		if len(page) == 0 {
			break
		}

		allResults = append(allResults, page...)

		// If we got fewer than the limit, we've reached the last page.
		if len(page) < 100 {
			break
		}

		// Extract the "id" field from the last item as the cursor.
		lastItem, err := json.Marshal(page[len(page)-1])
		if err != nil {
			break
		}

		var cursorObj struct {
			ID string `json:"id"`
		}
		if err := json.Unmarshal(lastItem, &cursorObj); err != nil || cursorObj.ID == "" {
			break
		}
		cursor = cursorObj.ID
	}

	return allResults, nil
}

// requestError wraps API error responses with status code for retry logic.
type requestError struct {
	statusCode       int
	retryAfterHeader string
	apiError         *APIError
	body             string
}

func (e *requestError) Error() string {
	if e.apiError != nil {
		return fmt.Sprintf("API error %d: %s", e.statusCode, e.apiError.Error())
	}
	return fmt.Sprintf("API error %d: %s", e.statusCode, e.body)
}

// newRequestError creates a requestError from a resty response.
func newRequestError(resp *resty.Response) *requestError {
	reqErr := &requestError{
		statusCode:       resp.StatusCode(),
		retryAfterHeader: resp.Header().Get("Retry-After"),
		body:             resp.String(),
	}

	var apiErr APIError
	if err := json.Unmarshal(resp.Bytes(), &apiErr); err == nil && apiErr.Errors.Detail != "" {
		reqErr.apiError = &apiErr
	}

	return reqErr
}

// IsBankrupt returns true if the error indicates the company is bankrupt
// (game API returns 401 Unauthorized with "bankrupt" in the message).
func IsBankrupt(err error) bool {
	if err == nil {
		return false
	}
	reqErr, ok := err.(*requestError)
	if !ok {
		return false
	}
	if reqErr.statusCode != 401 {
		return false
	}
	msg := reqErr.Error()
	return strings.Contains(strings.ToLower(msg), "bankrupt")
}

// parseRetryAfter extracts the retry delay from the Retry-After header value.
// Falls back to 5 seconds if no valid value is found.
func parseRetryAfter(headerVal string) time.Duration {
	if headerVal != "" {
		if seconds, err := strconv.Atoi(headerVal); err == nil {
			return time.Duration(seconds) * time.Second
		}
	}
	return 5 * time.Second
}

// BuildQueryString constructs a query string from key-value pairs,
// omitting any pairs where the value is empty.
func BuildQueryString(params map[string]string) string {
	if len(params) == 0 {
		return ""
	}

	values := url.Values{}
	for k, v := range params {
		if v != "" {
			values.Set(k, v)
		}
	}

	encoded := values.Encode()
	if encoded == "" {
		return ""
	}
	return "?" + encoded
}
