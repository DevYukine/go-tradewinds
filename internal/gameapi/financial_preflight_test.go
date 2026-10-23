package gameapi

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

type financialRateWaitContext struct {
	context.Context
	waiting chan struct{}
	once    sync.Once
}

func (ctx *financialRateWaitContext) Done() <-chan struct{} {
	ctx.once.Do(func() { close(ctx.waiting) })
	return ctx.Context.Done()
}

func TestMutationPreflightRunsAfterRateWaitBeforeFirstSend(t *testing.T) {
	var calls atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		calls.Add(1)
		writer.Header().Set("Content-Type", "application/json")
		_, _ = writer.Write([]byte(`{"ok":true}`))
	}))
	defer server.Close()
	client := New(server.URL, "synthetic-financial-key", nil)
	client.rates.mu.Lock()
	client.rates.ip[writeBucket].pauseUntil = time.Now().Add(300 * time.Millisecond)
	client.rates.mu.Unlock()
	parent, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	ctx := &financialRateWaitContext{Context: parent, waiting: make(chan struct{})}
	var valid atomic.Bool
	valid.Store(true)
	veto := errors.New("captured funds invalidated during rate wait")
	done := make(chan error, 1)
	go func() {
		var response any
		done <- client.DoWithPreflight(ctx, http.MethodPost, "/v1/shipyard/build-ship", map[string]any{"name": "Lapras"}, "preflight-build", &response, func() error {
			if !valid.Load() {
				return veto
			}
			return nil
		})
	}()
	select {
	case <-ctx.waiting:
	case <-parent.Done():
		t.Fatal("mutation did not enter the rate-limit wait")
	}
	valid.Store(false)
	select {
	case err := <-done:
		if !errors.Is(err, veto) {
			t.Fatalf("first-send preflight lost its invalidation: %v", err)
		}
	case <-parent.Done():
		t.Fatal("preflight did not finish after its rate pause")
	}
	if calls.Load() != 0 {
		t.Fatalf("invalidated mutation reached the game %d times", calls.Load())
	}
}

func TestMutationPreflightDoesNotReplanAfterTransportAttempt(t *testing.T) {
	var calls atomic.Int32
	var guards atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		if calls.Add(1) == 1 {
			writer.WriteHeader(http.StatusServiceUnavailable)
			_, _ = writer.Write([]byte(`{"code":"retry_later"}`))
			return
		}
		writer.Header().Set("Content-Type", "application/json")
		_, _ = writer.Write([]byte(`{"ok":true}`))
	}))
	defer server.Close()
	client := New(server.URL, "synthetic-financial-key", nil)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	var response any
	err := client.DoWithPreflight(ctx, http.MethodPost, "/v1/shipyard/build-ship", map[string]any{"name": "Lapras"}, "immutable-build", &response, func() error {
		if guards.Add(1) != 1 {
			return errors.New("must not replan an attempted request")
		}
		return nil
	})
	if err != nil || calls.Load() != 2 || guards.Load() != 1 {
		t.Fatalf("transport retry crossed the first-send guard again: calls %d guards %d error %v", calls.Load(), guards.Load(), err)
	}
}
