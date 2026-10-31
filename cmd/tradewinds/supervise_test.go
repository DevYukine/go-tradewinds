package main

import (
	"context"
	"errors"
	"fmt"
	"testing"
	"time"

	"go.uber.org/zap"

	"github.com/DevYukine/go-tradewinds/internal/engine"
	"github.com/DevYukine/go-tradewinds/internal/gameapi"
	"github.com/DevYukine/go-tradewinds/internal/store"
)

// A database restart ends a run with ownership loss or a dial error; trading
// must resume without an operator.
func TestSuperviseAccountRestartsAfterTransientFailures(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	runs := 0
	superviseAccount(ctx, func(context.Context) error {
		runs++
		switch runs {
		case 1:
			return store.ErrOwnershipLost
		case 2:
			return errors.New("failed to connect: dial tcp 127.0.0.1:55432: connection refused")
		case 3:
			// A dropped lock session cancels the run's own context.
			return context.Canceled
		default:
			cancel()
			return context.Canceled
		}
	}, zap.NewNop(), time.Millisecond)
	if runs != 4 {
		t.Fatalf("runner ran %d times, want a restart after each transient failure", runs)
	}
}

// Rejected credentials and a key for another identity must stop writes
// instead of retrying a dead key.
func TestSuperviseAccountStopsOnRejectedCredentials(t *testing.T) {
	for _, terminal := range []error{
		fmt.Errorf("%w: auth_revoked", engine.ErrAuthorizationEnded),
		fmt.Errorf("%w after definitive HTTP 403", engine.ErrExecutionStopped),
		engine.ErrIdentityMismatch,
		&gameapi.APIError{Status: 401},
		&gameapi.APIError{Status: 403},
		engine.ErrRunnerMisconfigured,
	} {
		ctx, cancel := context.WithCancel(context.Background())
		runs := 0
		superviseAccount(ctx, func(context.Context) error {
			runs++
			if runs > 1 {
				// Stop a regressed retry loop instead of hanging the test.
				cancel()
			}
			return terminal
		}, zap.NewNop(), time.Millisecond)
		cancel()
		if runs != 1 {
			t.Fatalf("%v: runner ran %d times, want one", terminal, runs)
		}
	}
}
