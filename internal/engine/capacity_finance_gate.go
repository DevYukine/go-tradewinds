package engine

import (
	"errors"
	"fmt"
	"time"
)

// A missing observation can recover before the request or build deadline. An
// economic loss of need cannot: unused approved debt must then be repaid.
type capacityGateError struct {
	Verdict string
	Hold    bool
	Reason  string
}

func (e *capacityGateError) Error() string { return e.Reason }

func capacityGate(verdict string, hold bool, reason string) error {
	return &capacityGateError{Verdict: verdict, Hold: hold, Reason: reason}
}

func capacityGateDetails(err error) (string, bool) {
	var gate *capacityGateError
	if errors.As(err, &gate) {
		return gate.Verdict, gate.Hold
	}
	return "authority", true
}

func (r *Runner) capacityInputsReady() error {
	for _, key := range []string{"ships", "vaults", "catalog", "clock", "zones", "rates", "factors", "profile", "warehouses", "contracts", "orders", "fills"} {
		if !r.fresh(key) {
			return capacityGate("authority", true, fmt.Sprintf("capacity financing waits for current successful %s authority", key))
		}
	}
	if r.stateCache.Clock.Paused {
		return capacityGate("authority", true, "capacity financing waits for an unpaused clock")
	}
	return nil
}

const capacityPaybackGameDays = 420

func (r *Runner) playerFallback(portID, hullID string) bool {
	until := decode[map[string]time.Time](r.snapshot, "_player_loan_fallback")[portID+"|"+hullID]
	return time.Now().Before(until)
}

func (r *Runner) clearPlayerFallback(portID, hullID string) {
	fallbacks := decode[map[string]time.Time](r.snapshot, "_player_loan_fallback")
	delete(fallbacks, portID+"|"+hullID)
	r.snapshot["_player_loan_fallback"] = encode(fallbacks)
}

func (r *Runner) capacityLoanHeadroom(decision GrowthDecision) int64 {
	return growthCashTotal(decision.CandidateLotCashPence, DailyUpkeep(r.stateCache.Ships, r.stateCache.Catalog, len(r.stateCache.Factors)))
}
