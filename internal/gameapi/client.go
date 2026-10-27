package gameapi

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math/rand/v2"
	"net"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"
	"unicode/utf8"

	"go.uber.org/zap"
)

const (
	maxBodyBytes   = 8 << 20
	maxAttempts    = 4
	requestTimeout = 45 * time.Second
)

var errBodyTooLarge = errors.New("game API response exceeds 8 MiB")

var sharedTransport = &http.Transport{
	Proxy: http.ProxyFromEnvironment,
	DialContext: (&net.Dialer{
		Timeout:   10 * time.Second,
		KeepAlive: 30 * time.Second,
	}).DialContext,
	ForceAttemptHTTP2:      true,
	MaxIdleConns:           100,
	MaxIdleConnsPerHost:    10,
	IdleConnTimeout:        90 * time.Second,
	TLSHandshakeTimeout:    10 * time.Second,
	ExpectContinueTimeout:  time.Second,
	ResponseHeaderTimeout:  30 * time.Second,
	MaxResponseHeaderBytes: 1 << 20,
}

// APIError preserves HTTP outcomes, including committed-but-uninterpreted 2xx
// responses. Error omits upstream strings because they may echo secrets.
type APIError struct {
	Status     int             `json:"status"`
	Code       string          `json:"code"`
	Detail     string          `json:"detail"`
	Body       json.RawMessage `json:"body"`
	Replayed   bool            `json:"replayed"`
	RetryAfter time.Time       `json:"retry_after"`
	cause      error
}

func (e *APIError) Error() string {
	return fmt.Sprintf("game API: HTTP %d", e.Status)
}

func (e *APIError) Unwrap() error {
	return e.cause
}

type requestFailure struct {
	message string
	cause   error
}

func (e *requestFailure) Error() string {
	return e.message
}

func (e *requestFailure) Unwrap() error {
	return e.cause
}

type observer func(method, path string, status int, duration time.Duration, request, response []byte, errText string)

// Client is safe for concurrent use. Transport and account rate buckets are
// shared; credentials and observers remain local. Release feed rates are isolated.
type Client struct {
	base          *url.URL
	apiKey        string
	keyHash       [sha256.Size]byte
	http          *http.Client
	rates         *hostRates
	log           *zap.Logger
	initErr       error
	mu            sync.RWMutex
	observer      observer
	releaseOnce   sync.Once
	releaseClient *Client
}

func New(baseURL, apiKey string, log *zap.Logger) *Client {
	if log == nil {
		log = zap.NewNop()
	}
	client := &Client{
		apiKey:  apiKey,
		keyHash: sha256.Sum256([]byte(apiKey)),
		log:     log,
		http: &http.Client{
			Transport: sharedTransport,
			CheckRedirect: func(*http.Request, []*http.Request) error {
				// Never forward credentials or replay a mutation at a redirect.
				return http.ErrUseLastResponse
			},
		},
	}

	base, err := url.Parse(baseURL)
	if err != nil || base == nil ||
		base.Hostname() == "" || base.User != nil ||
		base.RawQuery != "" || base.Fragment != "" || base.ForceQuery ||
		(base.Scheme != "http" && base.Scheme != "https") {
		client.initErr = errors.New("game API base URL must be an HTTP(S) origin without credentials, query, or fragment")
		return client
	}
	if !validHeaderValue(apiKey) {
		client.initErr = errors.New("game API credential contains invalid header characters")
		return client
	}

	base.Path = strings.TrimRight(base.Path, "/")
	base.RawPath = strings.TrimRight(base.RawPath, "/")
	client.base = base
	port := base.Port()
	if port == "" {
		port = "443"
		if base.Scheme == "http" {
			port = "80"
		}
	}
	host := strings.ToLower(strings.TrimSuffix(base.Hostname(), "."))
	client.rates = ratesFor(base.Scheme + "://" + net.JoinHostPort(host, port))
	return client
}

func (c *Client) SetObserver(fn func(method, path string, status int, duration time.Duration, request, response []byte, errText string)) {
	c.mu.Lock()
	c.observer = fn
	c.mu.Unlock()
}

func (c *Client) Get(ctx context.Context, path string, out any) error {
	return c.Do(ctx, http.MethodGet, path, nil, "", out)
}

// Do marshals once, then reuses the identical bytes and key for every bounded
// transport/5xx retry. A 4xx is definitive, including a cached mutation 429.
func (c *Client) Do(ctx context.Context, method, path string, body any, idempotencyKey string, out any) error {
	return c.do(ctx, method, path, body, idempotencyKey, out, nil)
}

// DoWithPreflight checks a new mutation after its rate-limit wait, immediately
// before the first transport attempt. A veto proves no attempt was made. Once
// an attempt starts, retries retain the request and never rerun the preflight.
// This is a local validity check, not an atomic condition on upstream state.
func (c *Client) DoWithPreflight(ctx context.Context, method, path string, body any, idempotencyKey string, out any, beforeFirstSend func() error) error {
	return c.do(ctx, method, path, body, idempotencyKey, out, beforeFirstSend)
}

func (c *Client) do(ctx context.Context, method, path string, body any, idempotencyKey string, out any, beforeFirstSend func() error) error {
	if c.initErr != nil {
		return c.initErr
	}
	requestURL, err := c.requestURL(path)
	if err != nil {
		return err
	}
	if err := validateRequest(method, idempotencyKey, c.apiKey != ""); err != nil {
		return err
	}
	payload, err := marshalBody(body)
	if err != nil {
		return err
	}

	bucket := requestBucket(method, path)
	var lastOutcome error

	for attempt := range maxAttempts {
		if err := c.rates.wait(ctx, bucket, c.keyHash, c.apiKey != ""); err != nil {
			return interruptedOutcome(lastOutcome, err)
		}
		if attempt == 0 && beforeFirstSend != nil {
			if err := beforeFirstSend(); err != nil {
				return err
			}
		}
		result := c.send(ctx, method, requestURL, path, payload, idempotencyKey, bucket, out)
		lastOutcome = result.err

		if result.status == http.StatusTooManyRequests {
			// Honor the pause even though replaying this key cannot turn a stored
			// 429 into success. A caller must replan a new logical action instead.
			if err := c.rates.waitPause(ctx, bucket, c.keyHash, c.apiKey != ""); err != nil {
				return interruptedOutcome(result.err, err)
			}
			return result.err
		}
		retry := result.status == 0 || (result.status >= 500 && result.status <= 599)
		if !retry {
			return result.err
		}
		if err := ctx.Err(); err != nil {
			return interruptedOutcome(result.err, err)
		}
		if attempt+1 == maxAttempts {
			return result.err
		}

		base := 250 * time.Millisecond * time.Duration(1<<attempt)
		delay := base/2 + time.Duration(rand.Float64()*float64(base/2))
		if err := waitFor(ctx, delay); err != nil {
			return interruptedOutcome(result.err, err)
		}
	}
	return lastOutcome
}

type requestResult struct {
	status int
	err    error
}

func (c *Client) send(ctx context.Context, method, requestURL, path string, payload []byte, idempotencyKey string, bucket bucketID, out any) requestResult {
	attemptCtx, cancel := context.WithTimeout(ctx, requestTimeout)
	defer cancel()

	request, err := http.NewRequestWithContext(attemptCtx, method, requestURL, bytes.NewReader(payload))
	if err != nil {
		return requestResult{err: &requestFailure{message: "cannot create game API request", cause: err}}
	}
	c.setHeaders(request, "application/json")
	if len(payload) > 0 {
		request.Header.Set("Content-Type", "application/json")
	}
	if idempotencyKey != "" {
		request.Header.Set("Idempotency-Key", idempotencyKey)
	}

	start := time.Now()
	response, err := c.http.Do(request)
	if err != nil {
		failure := &requestFailure{message: "game API transport failed", cause: err}
		c.observe(method, path, 0, time.Since(start), payload, nil, failure)
		return requestResult{err: failure}
	}
	cooldown := c.rates.update(bucket, c.keyHash, c.apiKey != "", response)

	responseBody, readErr := readBody(response)
	result := requestResult{status: response.StatusCode}
	var failure *APIError
	if response.StatusCode < 200 || response.StatusCode >= 300 || readErr != nil {
		failure = apiError(response.StatusCode, responseBody, readErr)
	} else if decodeErr := decodeResponse(method, response.StatusCode, responseBody, out); decodeErr != nil {
		failure = &APIError{
			Status: response.StatusCode,
			Code:   "invalid_response",
			Detail: decodeErr.Error(),
			Body:   responseBody,
			cause:  decodeErr,
		}
	}
	if failure != nil {
		failure.Replayed = strings.EqualFold(response.Header.Get("Idempotency-Replayed"), "true")
		failure.RetryAfter = cooldown
		result.err = failure
	}
	// Receiving 2xx commits the mutation even if its response is unreadable.
	// Preserve that outcome instead of hiding it behind a later retry failure.
	c.observe(method, path, result.status, time.Since(start), payload, responseBody, result.err)
	return result
}

func interruptedOutcome(outcome, cause error) error {
	var apiErr *APIError
	if errors.As(outcome, &apiErr) {
		apiErr.cause = errors.Join(apiErr.cause, cause)
		return apiErr
	}
	return cause
}

func (c *Client) requestURL(path string) (string, error) {
	if !strings.HasPrefix(path, "/") || strings.HasPrefix(path, "//") {
		return "", errors.New("game API path must be an origin-relative absolute path")
	}
	relative, err := url.ParseRequestURI(path)
	if err != nil || relative.IsAbs() || relative.Host != "" || relative.Fragment != "" {
		return "", errors.New("invalid game API path")
	}
	requestURL := *c.base
	requestURL.Path = c.base.Path + relative.Path
	requestURL.RawPath = c.base.EscapedPath() + relative.EscapedPath()
	requestURL.RawQuery = relative.RawQuery
	requestURL.ForceQuery = relative.ForceQuery
	return requestURL.String(), nil
}

func (c *Client) setHeaders(request *http.Request, accept string) {
	request.Header.Set("Accept", accept)
	request.Header.Set("User-Agent", "tradewinds-go/1.0")
	if c.apiKey != "" {
		request.Header.Set("Authorization", "Bearer "+c.apiKey)
	}
}

func validateRequest(method, key string, authenticated bool) error {
	switch method {
	case http.MethodGet, http.MethodHead, http.MethodPost, http.MethodPatch, http.MethodPut, http.MethodDelete, http.MethodOptions:
	default:
		return errors.New("unsupported game API HTTP method")
	}
	if len(key) > 255 || !validHeaderValue(key) || key != "" && strings.TrimSpace(key) == "" {
		return errors.New("Idempotency-Key must contain 1 to 255 valid header bytes")
	}
	mutating := method == http.MethodPost || method == http.MethodPatch ||
		method == http.MethodPut || method == http.MethodDelete
	if authenticated && mutating && key == "" {
		return errors.New("authenticated mutations require a persistent Idempotency-Key")
	}
	return nil
}

func validHeaderValue(value string) bool {
	for i := range len(value) {
		if value[i] < 32 || value[i] > 126 {
			return false
		}
	}
	return true
}

func marshalBody(body any) ([]byte, error) {
	if body == nil {
		return nil, nil
	}

	var payload []byte
	var err error
	switch value := body.(type) {
	case json.RawMessage:
		if len(value) == 0 {
			return nil, nil
		}
		if len(value) > maxBodyBytes {
			return nil, errors.New("game API request exceeds 8 MiB")
		}
		if !utf8.Valid(value) || !json.Valid(value) {
			return nil, errors.New("game API request contains malformed raw JSON")
		}
		payload = bytes.Clone(value)
	case *json.RawMessage:
		if value == nil {
			return nil, nil
		}
		return marshalBody(*value)
	default:
		payload, err = json.Marshal(body)
		if err != nil {
			return nil, &requestFailure{message: "cannot encode game API request JSON", cause: err}
		}
	}
	if len(payload) > maxBodyBytes {
		return nil, errors.New("game API request exceeds 8 MiB")
	}
	return payload, nil
}

func readBody(response *http.Response) ([]byte, error) {
	defer response.Body.Close()
	if response.ContentLength > maxBodyBytes {
		return nil, errBodyTooLarge
	}
	body, err := io.ReadAll(io.LimitReader(response.Body, maxBodyBytes+1))
	if len(body) > maxBodyBytes {
		return body[:maxBodyBytes], errBodyTooLarge
	}
	return body, err
}

func decodeResponse(method string, status int, body []byte, out any) error {
	trimmed := bytes.TrimSpace(body)
	if len(trimmed) == 0 {
		if out == nil && (method == http.MethodHead || status == http.StatusNoContent || status == http.StatusResetContent) {
			return nil
		}
		return errors.New("game API returned an empty JSON response")
	}
	if bytes.Equal(trimmed, []byte("null")) {
		return errors.New("game API returned null instead of response data")
	}
	if !utf8.Valid(body) {
		return errors.New("game API returned JSON that is not valid UTF-8")
	}
	if out == nil {
		if !json.Valid(body) {
			return errors.New("game API returned malformed or truncated JSON")
		}
		return nil
	}
	if err := json.Unmarshal(body, out); err != nil {
		var syntaxError *json.SyntaxError
		if errors.As(err, &syntaxError) {
			return &requestFailure{message: "game API returned malformed or truncated JSON", cause: err}
		}
		return &requestFailure{message: "game API JSON does not match response model", cause: err}
	}
	return nil
}

func apiError(status int, body []byte, readErr error) *APIError {
	failure := &APIError{Status: status, Body: body, cause: readErr}
	if readErr != nil {
		failure.Code = "incomplete_response"
		failure.Detail = "upstream response body could not be read completely"
		if errors.Is(readErr, errBodyTooLarge) {
			failure.Code = "response_too_large"
			failure.Detail = errBodyTooLarge.Error()
		}
		return failure
	}
	if len(bytes.TrimSpace(body)) == 0 {
		failure.Code = "empty_response"
		failure.Detail = "upstream HTTP error response was empty"
		return failure
	}
	if !utf8.Valid(body) {
		failure.Code = "invalid_problem_response"
		failure.Detail = "upstream HTTP error body is not valid UTF-8"
		return failure
	}
	var problem struct {
		Code   string `json:"code"`
		Detail string `json:"detail"`
	}
	if err := json.Unmarshal(body, &problem); err != nil {
		failure.Code = "non_json_response"
		failure.Detail = "upstream HTTP error returned a non-problem response; see raw body"
		failure.cause = err
		return failure
	}
	failure.Code = problem.Code
	failure.Detail = problem.Detail
	if failure.Code == "" {
		failure.Code = "invalid_problem_response"
		failure.Detail = "upstream HTTP error did not provide a problem code"
	}
	return failure
}
