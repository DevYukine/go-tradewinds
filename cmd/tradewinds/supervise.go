package main

import (
	"context"
	"errors"
	"math/rand/v2"
	"net/http"
	"time"

	"go.uber.org/zap"

	"github.com/DevYukine/go-tradewinds/internal/engine"
	"github.com/DevYukine/go-tradewinds/internal/gameapi"
)

const (
	accountRestartDelay    = 5 * time.Second
	accountRestartMaxDelay = 5 * time.Minute
	// A run that lasted this long was healthy; its failure starts a new
	// backoff sequence.
	accountHealthyRun = 10 * time.Minute
)

// superviseAccount restarts an account runner after it stops. A database
// restart drops the ownership session, which used to end trading until a
// manual restart. A fresh run reacquires the account lock, so one executor
// still runs at a time. Rejected credentials and a misconfigured runner stop
// for good: retrying cannot fix them and would keep spending requests.
func superviseAccount(ctx context.Context, run func(context.Context) error, log *zap.Logger, firstDelay time.Duration) {
	delay := firstDelay
	for {
		started := time.Now()
		err := run(ctx)
		// Ownership loss cancels the run's own context, so a canceled run is
		// a shutdown only when the supervisor itself was canceled.
		if ctx.Err() != nil {
			return
		}
		if terminalRunnerError(err) {
			log.Error("account runner stopped", zap.Error(err))
			return
		}
		if time.Since(started) >= accountHealthyRun {
			delay = firstDelay
		}
		wait := delay + time.Duration(rand.Int64N(int64(delay/2)+1))
		log.Error("account runner stopped; restarting", zap.Error(err), zap.Duration("retry_in", wait))
		timer := time.NewTimer(wait)
		select {
		case <-ctx.Done():
			timer.Stop()
			return
		case <-timer.C:
		}
		delay = min(accountRestartMaxDelay, delay*2)
	}
}

func terminalRunnerError(err error) bool {
	for _, terminal := range []error{engine.ErrAuthorizationEnded, engine.ErrExecutionStopped, engine.ErrIdentityMismatch, engine.ErrRunnerMisconfigured} {
		if errors.Is(err, terminal) {
			return true
		}
	}
	var upstream *gameapi.APIError
	return errors.As(err, &upstream) && (upstream.Status == http.StatusUnauthorized || upstream.Status == http.StatusForbidden)
}
