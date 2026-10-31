package gameapi_test

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/DevYukine/go-tradewinds/internal/gameapi"
)

// A missing receipt leaves reconciliation to the journal instead of spending
// trading time on immediate retries of the public post.
func TestBulletinMutationDoesNotRetryTransientFailures(t *testing.T) {
	for _, mode := range []string{"server failure", "transport failure"} {
		t.Run(mode, func(t *testing.T) {
			capture := &capturingServer{handler: func(w http.ResponseWriter, _ *http.Request, _ int) {
				if mode == "transport failure" {
					connection, _, err := w.(http.Hijacker).Hijack()
					if err == nil {
						_ = connection.Close()
					}
					return
				}
				problem(w, http.StatusInternalServerError, "internal_error", "bulletin store hiccup")
			}}
			server := httptest.NewServer(capture)
			defer server.Close()

			var out json.RawMessage
			err := newTestClient(t, server).Do(context.Background(), http.MethodPost, "/v1/bulletin/reply",
				map[string]string{"body": "fair winds"}, "bulletin-reply-1", &out)
			if mode == "server failure" {
				var apiErr *gameapi.APIError
				if !errors.As(err, &apiErr) || apiErr.Status != http.StatusInternalServerError {
					t.Fatalf("err = %v, want *gameapi.APIError 500", err)
				}
			} else if err == nil {
				t.Fatal("Do returned nil, want the transport failure surfaced")
			}
			if got := capture.count(); got != 1 {
				t.Fatalf("request count = %d, want exactly 1 (no transport or 5xx retry)", got)
			}
		})
	}
}

// A bulletin_limit 429 must return promptly with its advertised cooldown even
// when that cooldown is long, and it must leave the shared write bucket
// available so ordinary requests keep flowing.
func TestBulletinLimitReturnsPromptlyWithoutSharedPause(t *testing.T) {
	server := httptest.NewServer(&capturingServer{handler: func(w http.ResponseWriter, _ *http.Request, index int) {
		if index == 0 {
			w.Header().Set("Retry-After", "300")
			problem(w, http.StatusTooManyRequests, "bulletin_limit", "company bulletin hourly limit")
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"ok":true}`))
	}})
	defer server.Close()

	client := newTestClient(t, server)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	started := time.Now()
	var out json.RawMessage
	err := client.Do(ctx, http.MethodPost, "/v1/bulletin/thread",
		map[string]string{"title": "crew wanted"}, "bulletin-thread-1", &out)
	elapsed := time.Since(started)
	var apiErr *gameapi.APIError
	if !errors.As(err, &apiErr) || apiErr.Status != http.StatusTooManyRequests || apiErr.Code != "bulletin_limit" {
		t.Fatalf("err = %v, want bulletin_limit 429", err)
	}
	if errors.Is(err, context.DeadlineExceeded) || elapsed >= 10*time.Second {
		t.Fatalf("bulletin_limit blocked for %s; want a prompt return", elapsed)
	}
	if apiErr.RetryAfter.Sub(started) < 4*time.Minute {
		t.Fatalf("APIError.RetryAfter = %s, want the advertised 300s cooldown preserved", apiErr.RetryAfter)
	}

	// The shared write bucket stayed untouched: an ordinary POST issued right
	// after would otherwise sit out the advertised 300s cooldown.
	if err := client.Do(ctx, http.MethodPost, "/v1/market/place-order",
		placeOrderRequest{CommodityID: "tea", Quantity: 1}, "bulletin-after-limit", &out); err != nil {
		t.Fatalf("ordinary POST after bulletin_limit: %v", err)
	}
	if got := server.Config.Handler.(*capturingServer).count(); got != 2 {
		t.Fatalf("request count = %d, want the bulletin 429 plus one ordinary POST", got)
	}
}

func TestBulletinCachedLimitUsesDetailBeforeFallback(t *testing.T) {
	for _, header := range []string{"", "invalid", "0", "Tue, 15 Nov 1994 08:12:31 GMT"} {
		t.Run(header, func(t *testing.T) {
			for _, test := range []struct {
				name   string
				detail string
				delay  time.Duration
			}{
				{name: "detail", detail: "Try again in 10 minutes", delay: 10 * time.Minute},
				{name: "fallback", detail: "company hourly limit", delay: time.Hour},
			} {
				t.Run(test.name, func(t *testing.T) {
					server := httptest.NewServer(&capturingServer{handler: func(w http.ResponseWriter, _ *http.Request, index int) {
						if index == 0 {
							if header != "" {
								w.Header().Set("Retry-After", header)
							}
							w.Header().Set("Idempotency-Replayed", "true")
							problem(w, http.StatusTooManyRequests, "bulletin_limit", test.detail)
							return
						}
						w.Header().Set("Content-Type", "application/json")
						_, _ = w.Write([]byte(`{"ok":true}`))
					}})
					defer server.Close()
					client := newTestClient(t, server)
					ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
					defer cancel()
					started := time.Now()
					var out json.RawMessage
					err := client.Do(ctx, http.MethodPost, "/v1/bulletin/thread",
						map[string]string{"title": "fair winds"}, "bulletin-fallback", &out)
					var apiErr *gameapi.APIError
					if !errors.As(err, &apiErr) || apiErr.Status != http.StatusTooManyRequests || apiErr.Code != "bulletin_limit" || !apiErr.Replayed {
						t.Fatalf("err = %v, want cached bulletin_limit 429", err)
					}
					if apiErr.RetryAfter.Before(started.Add(test.delay)) || apiErr.RetryAfter.After(time.Now().Add(test.delay)) {
						t.Fatalf("retry deadline = %s, want durable %s cooldown", apiErr.RetryAfter, test.delay)
					}
					if err := client.Do(ctx, http.MethodPost, "/v1/market/place-order",
						placeOrderRequest{CommodityID: "tea", Quantity: 1}, "bulletin-fallback-trade", &out); err != nil {
						t.Fatalf("company cooldown blocked trading: %v", err)
					}
					if got := server.Config.Handler.(*capturingServer).count(); got != 2 {
						t.Fatalf("request count = %d, want one cached bulletin outcome and one trade", got)
					}
				})
			}
		})
	}
}

func TestBulletinCompanyCooldownUsesLaterHeaderOrDetail(t *testing.T) {
	earlier := time.Now().Add(20 * time.Minute).UTC().Truncate(time.Second)
	later := earlier.Add(10 * time.Minute)
	for _, test := range []struct {
		name     string
		status   int
		code     string
		header   string
		detail   string
		delay    time.Duration
		deadline time.Time
	}{
		{name: "limit header later", status: http.StatusTooManyRequests, code: "bulletin_limit", header: "1200", detail: "Try again in 10 minutes", delay: 20 * time.Minute},
		{name: "limit detail later", status: http.StatusTooManyRequests, code: "bulletin_limit", header: "300", detail: "Try again in 20 minutes", delay: 20 * time.Minute},
		{name: "mute header only", status: http.StatusForbidden, code: "posting_muted", header: "300", detail: "Company posting is muted", delay: 5 * time.Minute},
		{name: "mute header later", status: http.StatusForbidden, code: "posting_muted", header: "1200", detail: "Try again in 10 minutes", delay: 20 * time.Minute},
		{name: "mute detail later", status: http.StatusForbidden, code: "posting_muted", header: "300", detail: "Try again in 20 minutes", delay: 20 * time.Minute},
		{name: "mute detail only", status: http.StatusForbidden, code: "posting_muted", detail: "Try again in 10 minutes", delay: 10 * time.Minute},
		{name: "mute fallback", status: http.StatusForbidden, code: "posting_muted", detail: "Company posting is muted", delay: 24 * time.Hour},
		{name: "mute invalid header fallback", status: http.StatusForbidden, code: "posting_muted", header: "invalid", detail: "Company posting is muted", delay: 24 * time.Hour},
		{name: "limit timestamp detail later", status: http.StatusTooManyRequests, code: "bulletin_limit", header: earlier.Format(http.TimeFormat), detail: "Try again after " + later.Format(time.RFC3339), deadline: later},
		{name: "mute timestamp header later", status: http.StatusForbidden, code: "posting_muted", header: later.Format(http.TimeFormat), detail: "Try again after " + earlier.Format(time.RFC3339), deadline: later},
	} {
		t.Run(test.name, func(t *testing.T) {
			capture := &capturingServer{handler: func(w http.ResponseWriter, _ *http.Request, index int) {
				if index == 0 {
					if test.header != "" {
						w.Header().Set("Retry-After", test.header)
					}
					problem(w, test.status, test.code, test.detail)
					return
				}
				w.Header().Set("Content-Type", "application/json")
				_, _ = w.Write([]byte(`{"ok":true}`))
			}}
			server := httptest.NewServer(capture)
			defer server.Close()
			client := newTestClient(t, server)
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			started := time.Now()
			var out json.RawMessage
			err := client.Do(ctx, http.MethodPost, "/v1/bulletin/reply",
				map[string]string{"body": "fair winds"}, "bulletin-company-cooldown", &out)
			var apiErr *gameapi.APIError
			if !errors.As(err, &apiErr) || apiErr.Status != test.status || apiErr.Code != test.code {
				t.Fatalf("err = %v, want %s HTTP %d", err, test.code, test.status)
			}
			if !test.deadline.IsZero() {
				if !apiErr.RetryAfter.Equal(test.deadline) {
					t.Fatalf("retry deadline = %s, want later deadline %s", apiErr.RetryAfter, test.deadline)
				}
			} else if apiErr.RetryAfter.Before(started.Add(test.delay)) || apiErr.RetryAfter.After(time.Now().Add(test.delay)) {
				t.Fatalf("retry deadline = %s, want later usable %s cooldown", apiErr.RetryAfter, test.delay)
			}
			for _, trade := range []struct {
				name   string
				client *gameapi.Client
			}{
				{name: "same-key", client: client},
				{name: "other-key", client: gameapi.New(server.URL, "tw_sk_other_company", nil)},
			} {
				if err := trade.client.Do(ctx, http.MethodPost, "/v1/market/place-order",
					placeOrderRequest{CommodityID: "tea", Quantity: 1}, "bulletin-cooldown-"+trade.name, &out); err != nil {
					t.Fatalf("%s trade paused by company cooldown: %v", trade.name, err)
				}
			}
			if got := capture.count(); got != 3 {
				t.Fatalf("request count = %d, want one bulletin outcome plus two ordinary trades", got)
			}
		})
	}
}

// Any other 429 on a bulletin mutation is a real global limit: it still
// lowers the shared write bucket for later requests, but the bulletin call
// itself returns immediately instead of waiting the cooldown out.
func TestBulletinGlobalRateLimitStillPausesSharedBucket(t *testing.T) {
	server := httptest.NewServer(&capturingServer{handler: func(w http.ResponseWriter, _ *http.Request, index int) {
		if index == 0 {
			w.Header().Set("Retry-After", "2")
			problem(w, http.StatusTooManyRequests, "rate_limit_exceeded", "global write limit")
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"ok":true}`))
	}})
	defer server.Close()

	client := newTestClient(t, server)
	started := time.Now()
	var out json.RawMessage
	err := client.Do(context.Background(), http.MethodPost, "/v1/bulletin/reply",
		map[string]string{"body": "fair winds"}, "bulletin-reply-2", &out)
	if elapsed := time.Since(started); elapsed >= time.Second {
		t.Fatalf("bulletin global 429 blocked for %s; want a prompt return", elapsed)
	}
	var apiErr *gameapi.APIError
	if !errors.As(err, &apiErr) || apiErr.Status != http.StatusTooManyRequests || apiErr.Code != "rate_limit_exceeded" {
		t.Fatalf("err = %v, want the global 429 surfaced", err)
	}

	peer := gameapi.New(server.URL, "tw_sk_other_company", nil)
	peerCtx, cancelPeer := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancelPeer()
	if err := peer.Do(peerCtx, http.MethodPost, "/v1/market/place-order",
		placeOrderRequest{CommodityID: "tea", Quantity: 1}, "bulletin-global-peer", &out); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("different-key trade after global 429 = %v, want shared IP pause", err)
	}
	if got := server.Config.Handler.(*capturingServer).count(); got != 1 {
		t.Fatalf("request count = %d, want no trade sent during the global pause", got)
	}

	// The next ordinary POST must wait out the advertised two seconds before
	// its transport attempt, proving the shared bucket was paused.
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	started = time.Now()
	if err := client.Do(ctx, http.MethodPost, "/v1/market/place-order",
		placeOrderRequest{CommodityID: "tea", Quantity: 2}, "bulletin-global-after", &out); err != nil {
		t.Fatalf("ordinary POST after global 429: %v", err)
	}
	if elapsed := time.Since(started); elapsed < 1500*time.Millisecond {
		t.Fatalf("ordinary POST after global 429 took %s; want it paused by the shared cooldown", elapsed)
	}
}

func TestBulletinAuthorizationUsesReceivedProblemBody(t *testing.T) {
	for _, test := range []struct {
		name   string
		status int
		body   string
		stop   bool
	}{
		{"unauthorized", http.StatusUnauthorized, "", true},
		{"muted", http.StatusForbidden, `{"code":"posting_muted","detail":"wait 2 hours"}`, false},
		{"muted complete before stalled EOF", http.StatusForbidden, "{\"code\":\"posting_muted\"}\n ", false},
		{"other forbidden", http.StatusForbidden, `{"code":"company_suspended"}`, true},
		{"unknown forbidden", http.StatusForbidden, "", true},
		{"truncated mute", http.StatusForbidden, `{"code":"posting_muted"`, true},
		{"invalid encoding", http.StatusForbidden, string([]byte{0xff}), true},
		{"posting limit", http.StatusTooManyRequests, `{"code":"bulletin_limit"}`, false},
		{"success", http.StatusCreated, `{}`, false},
	} {
		t.Run(test.name, func(t *testing.T) {
			if got := gameapi.BulletinAuthorizationRefusal(test.status, []byte(test.body)); got != test.stop {
				t.Fatalf("authorization classification=%t, want %t", got, test.stop)
			}
		})
	}
}
