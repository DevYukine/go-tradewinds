package gameapi

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"testing/iotest"
	"time"

	"go.uber.org/zap"
)

type headerTimingTransport struct {
	http.RoundTripper
	bodyStarted chan struct{}
}

func (transport headerTimingTransport) RoundTrip(request *http.Request) (*http.Response, error) {
	response, err := transport.RoundTripper.RoundTrip(request)
	if err == nil {
		response.Body = &headerTimingBody{
			ReadCloser: response.Body,
			started:    transport.bodyStarted,
		}
	}
	return response, err
}

type headerTimingBody struct {
	io.ReadCloser
	started chan struct{}
	once    sync.Once
}

func (body *headerTimingBody) Read(buffer []byte) (int, error) {
	body.once.Do(func() { close(body.started) })
	return body.ReadCloser.Read(buffer)
}

func TestOrdinaryHeadersUpdateSharedLimitsBeforeBodyCompletes(t *testing.T) {
	for _, status := range []int{http.StatusOK, http.StatusForbidden, http.StatusTooManyRequests} {
		t.Run(strconv.Itoa(status), func(t *testing.T) {
			var requests atomic.Int32
			releaseBody := make(chan struct{})
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				requests.Add(1)
				if r.URL.Path == "/v1/stalled" {
					w.Header().Set("Content-Type", "application/json")
					w.Header().Set("X-Ratelimit-Bucket", "write")
					w.Header().Set("X-Ratelimit-Remaining", "0")
					w.Header().Set("Retry-After", "300")
					w.WriteHeader(status)
					w.(http.Flusher).Flush()
					select {
					case <-releaseBody:
						_, _ = io.WriteString(w, `{"code":"rate_limit_exceeded","detail":"write limit"}`)
					case <-r.Context().Done():
					}
					return
				}
				w.Header().Set("Content-Type", "application/json")
				_, _ = io.WriteString(w, `{"ok":true}`)
			}))
			defer server.Close()
			defer close(releaseBody)

			bodyStarted := make(chan struct{})
			client := New(server.URL, "tw_live_header_timing", zap.NewNop())
			client.http.Transport = headerTimingTransport{
				RoundTripper: sharedTransport,
				bodyStarted:  bodyStarted,
			}
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			results := make(chan error, 1)
			go func() {
				var out json.RawMessage
				results <- client.Do(ctx, http.MethodPost, "/v1/stalled",
					map[string]int{"quantity": 1}, "stalled-header", &out)
			}()
			select {
			case <-bodyStarted:
			case err := <-results:
				t.Fatalf("ordinary request completed before its stalled body: %v", err)
			case <-time.After(5 * time.Second):
				t.Fatal("ordinary request did not start reading its response body")
			}

			for _, key := range []string{"tw_live_header_timing", "tw_live_other_company"} {
				peer := New(server.URL, key, zap.NewNop())
				peerCtx, cancelPeer := context.WithTimeout(context.Background(), 100*time.Millisecond)
				var out json.RawMessage
				err := peer.Do(peerCtx, http.MethodPost, "/v1/market/place-order",
					map[string]int{"quantity": 1}, "trade-after-headers", &out)
				cancelPeer()
				if !errors.Is(err, context.DeadlineExceeded) {
					t.Fatalf("ordinary trade during stalled body = %v, want shared header pause", err)
				}
			}
			if got := requests.Load(); got != 1 {
				t.Fatalf("requests = %d, want no ordinary trades sent before the body completed", got)
			}
			select {
			case err := <-results:
				t.Fatalf("ordinary request completed while its response body was stalled: %v", err)
			default:
			}
			cancel()
			select {
			case err := <-results:
				var outcome *APIError
				if !errors.As(err, &outcome) || outcome.Status != status || !errors.Is(err, context.Canceled) {
					t.Fatalf("canceled body lost HTTP %d outcome: %v", status, err)
				}
			case <-time.After(5 * time.Second):
				t.Fatal("ordinary request did not stop after body cancellation")
			}
		})
	}
}

func TestMalformedSuccessPreservesCommittedOutcome(t *testing.T) {
	for name, body := range map[string]string{
		"empty":         "",
		"truncated":     `{"quantity":`,
		"wrong_type":    `{"quantity":"not a number"}`,
		"trailing_json": `{"quantity":1} {"quantity":2}`,
		"invalid_utf8":  "{\"name\":\"\xff\"}",
	} {
		t.Run(name, func(t *testing.T) {
			var requests atomic.Int32
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				requests.Add(1)
				w.Header().Set("Idempotency-Replayed", "true")
				w.WriteHeader(http.StatusCreated)
				_, _ = io.WriteString(w, body)
			}))
			defer server.Close()

			client := New(server.URL, "tw_live_test", zap.NewNop())
			var out struct {
				Quantity int32 `json:"quantity"`
			}
			err := client.Do(context.Background(), http.MethodPost, "/v1/test", map[string]int{"quantity": 1}, "committed", &out)
			var outcome *APIError
			if !errors.As(err, &outcome) {
				t.Fatalf("error = %v, want HTTP outcome", err)
			}
			if outcome.Status != http.StatusCreated || !outcome.Replayed || string(outcome.Body) != body {
				t.Fatalf("lost committed outcome: %+v", outcome)
			}
			if requests.Load() != 1 {
				t.Fatalf("unreadable success was replayed %d times", requests.Load())
			}
		})
	}
}

func TestTruncatedTransfersKeepHTTPStatusWithoutReplaying4xxOr2xx(t *testing.T) {
	for _, status := range []int{http.StatusOK, http.StatusConflict} {
		var requests atomic.Int32
		server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			requests.Add(1)
			w.Header().Set("Content-Length", "1000")
			w.WriteHeader(status)
			_, _ = io.WriteString(w, `{"partial":`)
		}))

		client := New(server.URL, "tw_live_test", zap.NewNop())
		var out json.RawMessage
		err := client.Do(context.Background(), http.MethodPost, "/v1/test", nil, "truncated", &out)
		server.Close()
		var outcome *APIError
		if !errors.As(err, &outcome) || outcome.Status != status || !errors.Is(err, io.ErrUnexpectedEOF) {
			t.Fatalf("status %d: lost truncated outcome: %v", status, err)
		}
		if requests.Load() != 1 || string(outcome.Body) != `{"partial":` {
			t.Fatalf("status %d: requests=%d, body=%q", status, requests.Load(), outcome.Body)
		}
	}
}

func TestOversizeSuccessKeepsStatusWithoutReplaying(t *testing.T) {
	var requests atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests.Add(1)
		w.Header().Set("Content-Length", strconv.Itoa(maxBodyBytes+1))
		_, _ = io.WriteString(w, "{}")
	}))
	defer server.Close()

	client := New(server.URL, "tw_live_test", zap.NewNop())
	err := client.Do(context.Background(), http.MethodPost, "/v1/test", nil, "oversize", nil)
	var outcome *APIError
	if !errors.As(err, &outcome) || outcome.Status != http.StatusOK || outcome.Code != "response_too_large" {
		t.Fatalf("oversize success lost HTTP outcome: %v", err)
	}
	if requests.Load() != 1 {
		t.Fatal("oversize success was replayed")
	}
}

func TestPatchRetriesKeepRawRequestBytes(t *testing.T) {
	type received struct {
		key  string
		body string
	}
	requests := make(chan received, maxAttempts)
	var count atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		requests <- received{key: r.Header.Get("Idempotency-Key"), body: string(body)}
		if count.Add(1) == 1 {
			w.WriteHeader(http.StatusServiceUnavailable)
			_, _ = io.WriteString(w, `{"code":"rate_limit_unavailable","detail":"retry"}`)
			return
		}
		_, _ = io.WriteString(w, `{"enabled":true}`)
	}))
	defer server.Close()

	const raw = "{\n  \"enabled\" : true\n}"
	client := New(server.URL, "tw_live_test", zap.NewNop())
	var out json.RawMessage
	if err := client.Do(context.Background(), http.MethodPatch, "/v1/company/settings", json.RawMessage(raw), "stable-patch", &out); err != nil {
		t.Fatal(err)
	}
	if count.Load() != 2 {
		t.Fatalf("requests = %d, want 2", count.Load())
	}
	for range 2 {
		request := <-requests
		if request.key != "stable-patch" || request.body != raw {
			t.Fatalf("changed replay request: %+v", request)
		}
	}
}

func TestObserverRedactsCredentialsAndEchoes(t *testing.T) {
	const key = "tw_live_private_test_key"
	const passphrase = "a private passphrase"
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]any{
			"api_key": key,
			"detail":  "echo " + passphrase + " " + key,
			"nested":  map[string]any{key: passphrase},
		})
	}))
	defer server.Close()

	client := New(server.URL, key, zap.NewNop())
	client.SetObserver(func(method, path string, status int, duration time.Duration, request, response []byte, errText string) {
		for _, value := range []string{path, string(request), string(response), errText} {
			if strings.Contains(value, key) || strings.Contains(value, passphrase) {
				t.Errorf("observation exposed a credential")
			}
		}
		if !json.Valid(request) || !json.Valid(response) {
			t.Error("redaction damaged JSON diagnostics")
		}
	})
	path := "/v1/auth/login?" + key + "=true&passphrase=a+private+passphrase"
	if err := client.Do(context.Background(), http.MethodPost, path, map[string]string{"passphrase": passphrase}, "login", nil); err != nil {
		t.Fatal(err)
	}
}

func TestStrictAuthAndCooldownsAreSharedButBucketScoped(t *testing.T) {
	const origin = "http://strict-auth-limiter.invalid:80"
	first := New(origin, "first", zap.NewNop())
	second := New("http://strict-auth-limiter.invalid", "second", zap.NewNop())
	t.Cleanup(func() {
		sharedRates.Lock()
		delete(sharedRates.hosts, origin)
		sharedRates.Unlock()
	})
	if first.rates != second.rates {
		t.Fatal("clients on one origin do not share the IP limiter")
	}
	for range 20 {
		if err := first.rates.wait(context.Background(), authBucket, first.keyHash, true); err != nil {
			t.Fatal(err)
		}
	}
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer cancel()
	if err := second.rates.wait(ctx, authBucket, second.keyHash, true); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("21st auth token = %v, want cancellable per-IP wait", err)
	}

	response := &http.Response{StatusCode: http.StatusTooManyRequests, Header: make(http.Header)}
	response.Header.Set("X-Ratelimit-Bucket", "write")
	response.Header.Set("Retry-After", "30")
	first.rates.update(writeBucket, first.keyHash, true, response)
	readCtx, stopRead := context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer stopRead()
	if err := second.rates.wait(readCtx, readBucket, second.keyHash, true); err != nil {
		t.Fatalf("write cooldown incorrectly blocked reads: %v", err)
	}
	if requestBucket(http.MethodGet, "/v1/event/stream") != streamBucket || requestBucket(http.MethodPost, "/v1/market/place-orders-batch") != batchBucket || requestBucket(http.MethodPost, "/v1/auth/create-key") != authBucket {
		t.Fatal("specialized bucket classification was shadowed by a generic method")
	}
}

func TestSSEHandlesSplitCRLFAndStopsOnControlFrames(t *testing.T) {
	input := "\xef\xbb\xbf" +
		"id: opaque-id\r\n" +
		"event: ship_arrived\r\n" +
		"data: {\r\n" +
		"data: \"id\": 42\r\n" +
		"data: }\r\n\r\n" +
		"id: bad-cursor\r" +
		"event: gap\r" +
		"data: {\"reason\":\"unknown_last_event_id\"}\r\r" +
		"event: ignored\r" +
		"data: {}\r\r"
	var events []Event
	err := parseEvents(context.Background(), iotest.OneByteReader(strings.NewReader(input)), "previous", func(event Event) error {
		events = append(events, event)
		return nil
	})
	var outcome *APIError
	if !errors.As(err, &outcome) || outcome.Status != http.StatusGone {
		t.Fatalf("gap = %v, want resync outcome", err)
	}
	if len(events) != 2 || events[0].ID != "opaque-id" || string(events[0].Data) != "{\n\"id\": 42\n}" || events[1].Type != "gap" {
		t.Fatalf("incorrect SSE frames: %+v", events)
	}
}

func TestSSEDoesNotCommitTruncatedFramesOrHideCallbackErrors(t *testing.T) {
	called := false
	err := parseEvents(context.Background(), strings.NewReader("id: 2\ndata: {}\n"), "1", func(Event) error {
		called = true
		return nil
	})
	if called || !errors.Is(err, io.ErrUnexpectedEOF) {
		t.Fatalf("truncated frame delivered=%t, error=%v", called, err)
	}

	failure := errors.New("event transaction failed")
	err = parseEvents(context.Background(), strings.NewReader("id: 2\ndata: {}\n\n"), "1", func(Event) error {
		return failure
	})
	if err != failure {
		t.Fatalf("callback error = %v, want unchanged storage failure", err)
	}
}
