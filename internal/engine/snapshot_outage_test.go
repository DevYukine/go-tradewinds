package engine

import (
	"testing"
	"time"

	"go.uber.org/zap"
	"go.uber.org/zap/zaptest/observer"

	"github.com/DevYukine/go-tradewinds/internal/gameapi"
)

// A game outage fails every dataset in a wave. The log used to carry one
// warning per dataset (641 in three days); one per outage window says enough.
func TestGameOutageWarnsOncePerWindow(t *testing.T) {
	h := newRecoveryHarness(t)
	runner := h.runner(primaryTestKey)
	core, logs := observer.New(zap.WarnLevel)
	runner.log = zap.New(core)
	unavailable := fetchResult{err: &gameapi.APIError{Status: 503}, attemptedAt: time.Now()}
	for _, key := range []string{"ships", "ports", "contracts", "ledger"} {
		runner.applyCapture(h.ctx, key, unavailable)
	}
	if got := logs.FilterMessage("dataset refresh failed").Len(); got != 1 {
		t.Fatalf("outage logged %d warnings, want 1", got)
	}
	runner.outageWarnedAt = time.Now().Add(-outageWarningInterval)
	runner.applyCapture(h.ctx, "vaults", unavailable)
	if got := logs.FilterMessage("dataset refresh failed").Len(); got != 2 {
		t.Fatalf("a later outage window logged %d warnings in total, want 2", got)
	}
}
