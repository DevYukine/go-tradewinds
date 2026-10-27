package gameapi

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strconv"
	"testing"
	"time"

	"go.uber.org/zap"
)

func TestReleaseNotesIsolateAndBoundPublicLimiterFeedback(t *testing.T) {
	for _, status := range []int{http.StatusOK, http.StatusTooManyRequests} {
		t.Run(strconv.Itoa(status), func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Content-Type", "application/json")
				if r.URL.Path == "/update" {
					if r.Header.Get("Authorization") != "" {
						t.Error("public feed received account credentials")
					}
					w.Header().Set("X-Ratelimit-Bucket", "auth_strict")
					w.Header().Set("X-Ratelimit-Remaining", "0")
					w.Header().Set("Retry-After", "18446744073709551615")
					w.WriteHeader(status)
					_, _ = w.Write([]byte(`[]`))
					return
				}
				if r.Header.Get("Authorization") != "Bearer account-key" {
					t.Error("account request lost its credentials")
				}
				_, _ = w.Write([]byte(`{}`))
			}))
			defer server.Close()
			client := New(server.URL, "account-key", zap.NewNop())
			ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
			_, err := client.ReleaseNotes(ctx)
			cancel()
			if status == http.StatusOK && err != nil {
				t.Fatalf("public success: %v", err)
			}
			if status == http.StatusTooManyRequests && err == nil {
				t.Fatal("public 429 unexpectedly succeeded")
			}
			if client.releaseClient.rates == client.rates {
				t.Fatal("public feed shares authenticated rate state")
			}
			public := client.releaseClient.rates
			public.mu.Lock()
			until := public.ip[readBucket].pauseUntil
			other := public.ip[authBucket].pauseUntil
			public.mu.Unlock()
			if remaining := time.Until(until); remaining <= 0 || remaining > time.Hour {
				t.Fatalf("public cooldown outside one-hour bound: %v", remaining)
			}
			if !other.IsZero() {
				t.Fatal("public header selected an unrelated bucket")
			}
			client.rates.mu.Lock()
			for _, bucket := range client.rates.ip {
				if !bucket.pauseUntil.IsZero() {
					t.Error("public feedback paused a shared account bucket")
				}
				if bucket.tokens != bucket.capacity {
					t.Error("public feedback clamped shared account availability")
				}
			}
			client.rates.mu.Unlock()
			accountCtx, accountCancel := context.WithTimeout(context.Background(), time.Second)
			defer accountCancel()
			var response json.RawMessage
			if err := client.Get(accountCtx, "/v1/player/profile", &response); err != nil {
				t.Fatalf("account read blocked by public feed: %v", err)
			}
		})
	}
}

func TestReleaseNotesBoundDecodedHistory(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		notes := make([]ReleaseNote, 1000)
		for index := range notes {
			notes[index].Version = strconv.Itoa(index)
		}
		if err := json.NewEncoder(w).Encode(notes); err != nil {
			t.Error(err)
		}
	}))
	defer server.Close()
	notes, err := New(server.URL, "", zap.NewNop()).ReleaseNotes(context.Background())
	if err != nil || len(notes) != 100 || notes[0].Version != "0" || notes[99].Version != "99" {
		t.Fatalf("bounded feed = %d entries, error %v", len(notes), err)
	}
}

func TestPublicFeedCooldownCapsHeaderFormats(t *testing.T) {
	for _, header := range []http.Header{
		{"Retry-After": {"18446744073709551615"}},
		{"Retry-After": {time.Now().AddDate(50, 0, 0).UTC().Format(http.TimeFormat)}},
		{"X-Ratelimit-Remaining": {"0"}, "X-Ratelimit-Reset": {"999999999"}},
	} {
		rates := &hostRates{ip: newBuckets(5), publicFeed: true}
		until := rates.update(readBucket, [32]byte{}, false, &http.Response{StatusCode: http.StatusOK, Header: header})
		if remaining := time.Until(until); remaining < 59*time.Minute || remaining > time.Hour {
			t.Fatalf("headers %#v produced cooldown %v", header, remaining)
		}
	}
}
