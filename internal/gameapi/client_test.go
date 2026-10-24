package gameapi_test

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/DevYukine/go-tradewinds/internal/gameapi"
	"go.uber.org/zap"
	"go.uber.org/zap/zaptest/observer"
)

// capturedRequest records what the test server observed for one request.
type capturedRequest struct {
	idempotencyKey string
	body           []byte
}

type capturingServer struct {
	mu       sync.Mutex
	requests []capturedRequest
	handler  func(w http.ResponseWriter, r *http.Request, index int)
}

func (c *capturingServer) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	body, _ := io.ReadAll(r.Body)
	c.mu.Lock()
	index := len(c.requests)
	c.requests = append(c.requests, capturedRequest{
		idempotencyKey: r.Header.Get("Idempotency-Key"),
		body:           body,
	})
	handler := c.handler
	c.mu.Unlock()
	handler(w, r, index)
}

func (c *capturingServer) count() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return len(c.requests)
}

func newTestClient(t *testing.T, server *httptest.Server) *gameapi.Client {
	t.Helper()
	return gameapi.New(server.URL, "tw_sk_test_secret_credential", zap.NewNop())
}

func problem(w http.ResponseWriter, status int, code, detail string) {
	w.Header().Set("Content-Type", "application/problem+json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(map[string]string{
		"type":   "urn:tradewinds:error:" + code,
		"title":  code,
		"status": fmt.Sprint(status),
		"code":   code,
		"detail": detail,
	})
}

type placeOrderRequest struct {
	CommodityID string `json:"commodity_id"`
	Quantity    int    `json:"quantity"`
}

type placeOrderResponse struct {
	OrderID string `json:"order_id"`
}

// A transient 500 on a mutation must be retried with the byte-identical body
// and the identical Idempotency-Key, and the caller must see the eventual
// success. A fresh key on retry would risk a duplicate write upstream.
func TestDoRetriesTransient500WithIdenticalIdempotencyKeyAndBody(t *testing.T) {
	server := httptest.NewServer(&capturingServer{handler: func(w http.ResponseWriter, _ *http.Request, index int) {
		switch index {
		case 0, 1:
			problem(w, http.StatusInternalServerError, "internal_error", "limiter database hiccup")
		default:
			w.Header().Set("Content-Type", "application/json")
			_ = json.NewEncoder(w).Encode(placeOrderResponse{OrderID: "order-1"})
		}
	}})
	defer server.Close()

	client := newTestClient(t, server)
	body := placeOrderRequest{CommodityID: "tea", Quantity: 10}
	var out placeOrderResponse
	err := client.Do(context.Background(), http.MethodPost, "/v1/market/place-order", body, "place-order-tea-10", &out)
	if err != nil {
		t.Fatalf("Do after transient 500s: %v", err)
	}
	if out.OrderID != "order-1" {
		t.Fatalf("decoded out = %+v, want order-1", out)
	}

	srv := server.Config.Handler.(*capturingServer)
	if got := srv.count(); got != 3 {
		t.Fatalf("request count = %d, want exactly 3 (two 500 retries then success)", got)
	}
	srv.mu.Lock()
	defer srv.mu.Unlock()
	for i, req := range srv.requests {
		if req.idempotencyKey != "place-order-tea-10" {
			t.Fatalf("request %d Idempotency-Key = %q, want identical key on every retry", i, req.idempotencyKey)
		}
		if i > 0 && string(req.body) != string(srv.requests[0].body) {
			t.Fatalf("request %d body = %s, want byte-identical retry body %s", i, req.body, srv.requests[0].body)
		}
	}
	if !strings.Contains(string(srv.requests[0].body), `"commodity_id":"tea"`) {
		t.Fatalf("request body = %s, want marshalled JSON body", srv.requests[0].body)
	}
}

func TestDoPutDeleteRejectMissingIdempotencyKeyBeforeSending(t *testing.T) {
	for _, method := range []string{http.MethodPut, http.MethodDelete} {
		t.Run(method, func(t *testing.T) {
			capture := &capturingServer{handler: func(w http.ResponseWriter, _ *http.Request, _ int) {
				w.WriteHeader(http.StatusNoContent)
			}}
			server := httptest.NewServer(capture)
			defer server.Close()

			client := newTestClient(t, server)
			err := client.Do(context.Background(), method, "/v1/market/order",
				placeOrderRequest{CommodityID: "tea", Quantity: 10}, "", nil)
			if err == nil || !strings.Contains(err.Error(), "Idempotency-Key") {
				t.Fatalf("Do without idempotency key = %v, want validation error", err)
			}
			if got := capture.count(); got != 0 {
				t.Fatalf("request count = %d, want no outbound request", got)
			}
		})
	}
}

func TestDoPutDeleteRetryTransient500WithIdenticalIdempotencyKeyAndBody(t *testing.T) {
	for _, method := range []string{http.MethodPut, http.MethodDelete} {
		t.Run(method, func(t *testing.T) {
			capture := &capturingServer{handler: func(w http.ResponseWriter, r *http.Request, index int) {
				if r.Method != method || r.Header.Get("Authorization") != "Bearer tw_sk_test_secret_credential" {
					t.Errorf("unexpected authenticated request: %s, authorization present: %t", r.Method, r.Header.Get("Authorization") != "")
					w.WriteHeader(http.StatusUnauthorized)
					return
				}
				if index == 0 {
					problem(w, http.StatusInternalServerError, "internal_error", "transient write failure")
					return
				}
				w.Header().Set("Content-Type", "application/json")
				_ = json.NewEncoder(w).Encode(placeOrderResponse{OrderID: "order-updated"})
			}}
			server := httptest.NewServer(capture)
			defer server.Close()

			client := newTestClient(t, server)
			body := placeOrderRequest{CommodityID: "tea", Quantity: 10}
			var out placeOrderResponse
			const key = "update-order-tea-10"
			if err := client.Do(context.Background(), method, "/v1/market/order", body, key, &out); err != nil {
				t.Fatalf("Do after transient 500: %v", err)
			}
			if out.OrderID != "order-updated" {
				t.Fatalf("decoded response = %+v, want successful retry", out)
			}
			expectedBody, err := json.Marshal(body)
			if err != nil {
				t.Fatal(err)
			}
			capture.mu.Lock()
			defer capture.mu.Unlock()
			if len(capture.requests) != 2 {
				t.Fatalf("request count = %d, want one 500 and one successful retry", len(capture.requests))
			}
			for i, request := range capture.requests {
				if request.idempotencyKey != key || string(request.body) != string(expectedBody) {
					t.Fatalf("request %d changed key or body: key %q, body %s", i, request.idempotencyKey, request.body)
				}
			}
		})
	}
}

// An ordinary 4xx answer is definitive: stored and replayed upstream, so it
// must surface as *gameapi.APIError without any retry.
func TestDoDoesNotRetryOrdinary400(t *testing.T) {
	server := httptest.NewServer(&capturingServer{handler: func(w http.ResponseWriter, _ *http.Request, _ int) {
		problem(w, http.StatusBadRequest, "validation_failed", "quantity must be positive")
	}})
	defer server.Close()

	client := newTestClient(t, server)
	var out placeOrderResponse
	err := client.Do(context.Background(), http.MethodPost, "/v1/market/place-order",
		placeOrderRequest{CommodityID: "tea", Quantity: -1}, "place-order-tea-bad", &out)

	var apiErr *gameapi.APIError
	if !errors.As(err, &apiErr) {
		t.Fatalf("err = %v, want *gameapi.APIError", err)
	}
	if apiErr.Status != http.StatusBadRequest {
		t.Fatalf("APIError.Status = %d, want 400", apiErr.Status)
	}
	if apiErr.Code != "validation_failed" {
		t.Fatalf("APIError.Code = %q, want validation_failed", apiErr.Code)
	}
	if apiErr.Detail != "quantity must be positive" {
		t.Fatalf("APIError.Detail = %q, want problem detail", apiErr.Detail)
	}
	if len(apiErr.Body) == 0 {
		t.Fatal("APIError.Body empty, want raw problem+json payload")
	}
	if got := server.Config.Handler.(*capturingServer).count(); got != 1 {
		t.Fatalf("request count = %d, want exactly 1 (no retry after 4xx)", got)
	}
}

// A 429 answer carries Retry-After; the client must wait it out but the wait
// has to be interruptible: cancelling the context returns promptly instead of
// sleeping for the whole advertised delay, and no retry is attempted.
func TestDoCancellationInterruptsRetryAfterWait(t *testing.T) {
	server := httptest.NewServer(&capturingServer{handler: func(w http.ResponseWriter, _ *http.Request, _ int) {
		w.Header().Set("Retry-After", "30")
		problem(w, http.StatusTooManyRequests, "rate_limit_exceeded", "slow down")
	}})
	defer server.Close()

	client := newTestClient(t, server)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	type doResult struct{ err error }
	results := make(chan doResult, 1)
	go func() {
		var out placeOrderResponse
		results <- doResult{client.Do(ctx, http.MethodPost, "/v1/market/place-order",
			placeOrderRequest{CommodityID: "tea", Quantity: 1}, "place-order-tea-limited", &out)}
	}()

	select {
	case res := <-results:
		t.Fatalf("Do returned before cancellation (err=%v); client ignored Retry-After wait", res.err)
	case <-time.After(10 * time.Second):
		t.Fatal("Do did not reach its Retry-After wait within watchdog; test setup is broken")
	case <-waitUntilServed(server):
	}

	cancel()
	select {
	case res := <-results:
		if !errors.Is(res.err, context.Canceled) {
			t.Fatalf("err = %v, want context.Canceled from interrupted Retry-After wait", res.err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("Do still sleeping in Retry-After wait 10s after cancellation")
	}
	if got := server.Config.Handler.(*capturingServer).count(); got != 1 {
		t.Fatalf("request count = %d, want exactly 1 (cancelled wait must not retry)", got)
	}
}

// waitUntilServed returns a channel closed once the server has handled a
// request, so the test cancels while the client is inside its wait.
func waitUntilServed(server *httptest.Server) <-chan struct{} {
	served := make(chan struct{})
	go func() {
		srv := server.Config.Handler.(*capturingServer)
		for srv.count() == 0 {
			time.Sleep(5 * time.Millisecond)
		}
		close(served)
	}()
	return served
}

// Stream resumes with the Last-Event-ID cursor on its single connection and
// delivers each frame's id/type/data to the callback, joining multiline data
// payloads with newlines; keep-alive comments produce no callback events and
// EOF ends the stream so the engine owns reconnection.
func TestStreamSendsLastEventIDAndParsesMultilineFrames(t *testing.T) {
	var mu sync.Mutex
	connections := 0
	var resumeCursor string

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		connections++
		resumeCursor = r.Header.Get("Last-Event-ID")
		mu.Unlock()

		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		flusher := w.(http.Flusher)
		// One multiline frame (data split across data: lines), one
		// single-line frame, then a keep-alive comment before EOF.
		fmt.Fprint(w, `id: 42
event: ship_arrived
data: {
data:   "ship_id": "ark-royal",
data:   "port_id": "london"
data: }

id: 43
event: price_changed
data: {"commodity_id":"tea","best_bid_pence":210}

: keep-alive

`)
		flusher.Flush()
	}))
	defer server.Close()

	client := newTestClient(t, server)

	var (
		muEvents sync.Mutex
		events   []gameapi.Event
	)
	err := client.Stream(context.Background(), "41", func(ev gameapi.Event) error {
		muEvents.Lock()
		defer muEvents.Unlock()
		events = append(events, ev)
		return nil
	})
	if err != nil {
		t.Fatalf("Stream at EOF: %v", err)
	}

	mu.Lock()
	defer mu.Unlock()
	if connections != 1 {
		t.Fatalf("server connections = %d, want exactly 1 (client must not reconnect itself)", connections)
	}
	if resumeCursor != "41" {
		t.Fatalf("Last-Event-ID = %q, want resume cursor 41", resumeCursor)
	}

	muEvents.Lock()
	defer muEvents.Unlock()
	if len(events) != 2 {
		t.Fatalf("callback events = %d, want 2 (keep-alive comment must not produce an event)", len(events))
	}
	first := events[0]
	if first.ID != "42" || first.Type != "ship_arrived" {
		t.Fatalf("first event = {%s %s}, want {42 ship_arrived}", first.ID, first.Type)
	}
	wantData := "{\n  \"ship_id\": \"ark-royal\",\n  \"port_id\": \"london\"\n}"
	if string(first.Data) != wantData {
		t.Fatalf("first event data = %q, want multiline data joined with newlines %q", first.Data, wantData)
	}
	var payload struct {
		ShipID string `json:"ship_id"`
		PortID string `json:"port_id"`
	}
	if err := json.Unmarshal(first.Data, &payload); err != nil {
		t.Fatalf("multiline data is not valid JSON after join: %v", err)
	}
	if payload.ShipID != "ark-royal" || payload.PortID != "london" {
		t.Fatalf("decoded multiline payload = %+v, want ark-royal/london", payload)
	}
	second := events[1]
	if second.ID != "43" || second.Type != "price_changed" {
		t.Fatalf("second event = {%s %s}, want {43 price_changed}", second.ID, second.Type)
	}
	if !strings.Contains(string(second.Data), `"best_bid_pence":210`) {
		t.Fatalf("second event data = %s, want raw frame JSON", second.Data)
	}
}

// The observer feeds the diagnostics journal and must never see the
// Authorization credential: the method/path/status/duration observation and
// the request/response/errText payloads stay header-free.
func TestObserverReceivesObservationWithoutAuthorizationCredential(t *testing.T) {
	server := httptest.NewServer(&capturingServer{handler: func(w http.ResponseWriter, _ *http.Request, _ int) {
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(placeOrderResponse{OrderID: "order-2"})
	}})
	defer server.Close()

	const apiKey = "tw_sk_test_secret_credential"
	client := gameapi.New(server.URL, apiKey, zap.NewNop())

	type observation struct {
		method   string
		path     string
		status   int
		request  []byte
		response []byte
		errText  string
	}
	var (
		mu    sync.Mutex
		calls []observation
	)
	client.SetObserver(func(method, path string, status int, _ time.Duration, request, response []byte, errText string) {
		mu.Lock()
		defer mu.Unlock()
		calls = append(calls, observation{
			method:   method,
			path:     path,
			status:   status,
			request:  request,
			response: response,
			errText:  errText,
		})
	})

	var out placeOrderResponse
	err := client.Do(context.Background(), http.MethodPost, "/v1/market/place-order",
		placeOrderRequest{CommodityID: "rum", Quantity: 5}, "place-order-rum-5", &out)
	if err != nil {
		t.Fatalf("Do: %v", err)
	}

	mu.Lock()
	defer mu.Unlock()
	if len(calls) == 0 {
		t.Fatal("observer never invoked for a completed request")
	}
	for i, call := range calls {
		if call.method != http.MethodPost || call.path != "/v1/market/place-order" {
			t.Fatalf("observation %d method/path = %s %s, want POST /v1/market/place-order", i, call.method, call.path)
		}
		if call.status != http.StatusOK {
			t.Fatalf("observation %d status = %d, want 200", i, call.status)
		}
		for label, payload := range map[string][]byte{
			"request": call.request, "response": call.response, "errText": []byte(call.errText),
		} {
			if strings.Contains(string(payload), apiKey) {
				t.Fatalf("observation %d %s exposes the Authorization credential: %s", i, label, payload)
			}
		}
		if !strings.Contains(string(call.request), `"commodity_id":"rum"`) {
			t.Fatalf("observation %d request payload = %s, want marshalled request body", i, call.request)
		}
		if !strings.Contains(string(call.response), "order-2") {
			t.Fatalf("observation %d response payload = %s, want response body", i, call.response)
		}
	}
}

func TestRequestDiagnosticsScrubAllTokenPrefixesAndPayloadCredentials(t *testing.T) {
	tokens := []string{
		"tw_live_other_account_Ab9-xy",
		"tw_test_expired_Ab9-xy",
		"tw_sk_other_account_Ab9-xy",
		"tw_sk_test_expired_Ab9-xy",
	}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{
			"passphrase": "response-secret",
			"detail":     strings.Join(tokens, " "),
			"count":      7,
		})
	}))
	defer server.Close()
	for _, attachObserver := range []bool{false, true} {
		t.Run(fmt.Sprintf("observer=%t", attachObserver), func(t *testing.T) {
			core, logs := observer.New(zap.DebugLevel)
			client := gameapi.New(server.URL, "current-api-secret", zap.New(core))
			var observed []string
			if attachObserver {
				client.SetObserver(func(_ string, path string, _ int, _ time.Duration, request, response []byte, errText string) {
					observed = append(observed, path, string(request), string(response), errText)
				})
			}
			path := "/v1/example?detail=" + strings.Join(tokens, ",") + ",request-secret,response-secret"
			var out json.RawMessage
			if err := client.Do(context.Background(), http.MethodPost, path, map[string]string{
				"passphrase": "request-secret",
				"detail":     strings.Join(tokens, " "),
			}, "redaction-regression", &out); err != nil {
				t.Fatal(err)
			}
			if logs.Len() == 0 {
				t.Fatal("debug request diagnostics were not emitted")
			}
			for _, entry := range logs.All() {
				for _, key := range []string{"path", "error"} {
					if text, ok := entry.ContextMap()[key].(string); ok {
						observed = append(observed, text)
					}
				}
			}
			secrets := append([]string{"current-api-secret", "request-secret", "response-secret"}, tokens...)
			for _, value := range observed {
				for _, secret := range secrets {
					if strings.Contains(value, secret) {
						t.Fatalf("request diagnostic leaked a credential: %s", value)
					}
				}
			}
			if attachObserver && (len(observed) < 4 || !strings.Contains(observed[2], `"count":7`)) {
				t.Fatal("observer lost public response data")
			}
		})
	}
}
