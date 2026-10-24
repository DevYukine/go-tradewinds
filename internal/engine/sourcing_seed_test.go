package engine

import (
	"testing"

	"github.com/DevYukine/go-tradewinds/internal/store"
)

func TestSourcingUnknownCommercialHistoryCannotEraseOrReplenishSeed(t *testing.T) {
	history := store.EconomicHistory{
		UnknownCosts:       true,
		CommercialNetPence: 1_000_000,
		ProbeExpensePence:  7_500,
	}
	if allowance := sourcingLifetimeAllowance(history); allowance != 12_500 {
		t.Fatalf("unknown commercial returns must retain only the unspent 20,000p seed: got %dp", allowance)
	}
	history.ProbeExpensePence = 20_001
	if allowance := sourcingLifetimeAllowance(history); allowance != 0 {
		t.Fatalf("unknown commercial returns replenished exhausted seed: %dp", allowance)
	}
	history.ProbeExpensePence = 0
	history.UnknownProbeCosts = true
	if allowance := sourcingLifetimeAllowance(history); allowance != 0 {
		t.Fatalf("unmeasured prior discovery spending allowed the seed to reset: %dp", allowance)
	}
}
