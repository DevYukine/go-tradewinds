package engine

import (
	"encoding/json"
	"errors"
	"math"
	"math/bits"
	"strings"
	"time"

	"github.com/google/uuid"
)

// InsurancePolicy is the public board observation, not an owned slice receipt.
type InsurancePolicy struct {
	ID                string    `json:"id"`
	InsuredCompanyID  string    `json:"insured_company_id"`
	ShipID            string    `json:"ship_id"`
	OriginPortID      string    `json:"origin_port_id"`
	DestPortID        string    `json:"dest_port_id"`
	InsuredValuePence int64     `json:"insured_value_pence"`
	HullValuePence    int64     `json:"hull_value_pence"`
	PremiumBPS        int64     `json:"premium_bps"`
	TotalPremiumPence int64     `json:"total_premium_pence"`
	CoveredBPS        int64     `json:"covered_bps"`
	Status            string    `json:"status"`
	CreatedAt         time.Time `json:"created_at"`
	UpdatedAt         time.Time `json:"updated_at"`
	ExpiresAt         time.Time `json:"expires_at"`
	TermsChanged      bool      `json:"terms_changed,omitempty"`
}

func insuranceUUID(id string) bool {
	return len(id) == 36 && uuid.Validate(id) == nil
}

// Coverage may legitimately be zero; its absence must not manufacture spare
// capacity. Other required scalar fields have nonzero validity constraints.
func (policy *InsurancePolicy) UnmarshalJSON(data []byte) error {
	type wirePolicy InsurancePolicy
	var decoded wirePolicy
	wire := struct {
		*wirePolicy
		Coverage *int64 `json:"covered_bps"`
	}{wirePolicy: &decoded}
	if err := json.Unmarshal(data, &wire); err != nil {
		return err
	}
	if wire.Coverage == nil {
		return errors.New("insurance policy has no coverage field")
	}
	decoded.CoveredBPS = *wire.Coverage
	value := InsurancePolicy(decoded)
	if !completeInsurancePolicy(value) {
		return errors.New("insurance policy is incomplete or invalid")
	}
	value.ID = strings.ToLower(value.ID)
	value.InsuredCompanyID = strings.ToLower(value.InsuredCompanyID)
	value.ShipID = strings.ToLower(value.ShipID)
	*policy = value
	return nil
}

func completeInsurancePolicy(policy InsurancePolicy) bool {
	knownStatus := policy.Status == "open" || policy.Status == "active" || policy.Status == "settled_safe" ||
		policy.Status == "claimed_loss" || policy.Status == "cancelled"
	return insuranceUUID(policy.ID) && insuranceUUID(policy.InsuredCompanyID) && insuranceUUID(policy.ShipID) &&
		policy.OriginPortID != "" && policy.DestPortID != "" && policy.OriginPortID != policy.DestPortID &&
		policy.InsuredValuePence > 0 && policy.InsuredValuePence <= maximumPence &&
		policy.HullValuePence > 0 && policy.HullValuePence <= maximumPence &&
		policy.TotalPremiumPence > 0 && policy.TotalPremiumPence <= maximumPence &&
		policy.PremiumBPS >= 10 && policy.PremiumBPS <= 3000 && policy.CoveredBPS >= 0 && policy.CoveredBPS <= 10000 &&
		!policy.CreatedAt.IsZero() && !policy.UpdatedAt.Before(policy.CreatedAt) &&
		policy.ExpiresAt.After(policy.CreatedAt) && knownStatus
}

type UnderwritingBudget struct {
	Now                  time.Time
	OwnCompanyIDs        []string
	DiscretionaryPence   int64
	AvailableLocalPence  int64
	RemainingPilotPence  int64
	OpportunityCostPence int64
	TimeScale            float64
}

type UnderwritingDecision struct {
	Accepted              bool    `json:"accepted"`
	Reason                string  `json:"reason"`
	PolicyID              string  `json:"policy_id"`
	SliceBPS              int64   `json:"slice_bps"`
	CollateralPence       int64   `json:"collateral_pence"`
	PremiumPence          int64   `json:"premium_pence"`
	ExpectedClaimsPence   int64   `json:"expected_claims_pence"`
	ExpectedNetPence      int64   `json:"expected_net_pence"`
	ObservedPolicies      int64   `json:"observed_policies"`
	ObservedLosses        int64   `json:"observed_losses"`
	EstimatedLossBPS      int64   `json:"estimated_loss_bps"`
	ConservativeLossBPS   int64   `json:"conservative_loss_bps"`
	EstimatedLockGameDays float64 `json:"estimated_lock_game_days"`
}

func validHullPolicy(policy InsurancePolicy) bool {
	return completeInsurancePolicy(policy) && policy.HullValuePence >= policy.InsuredValuePence &&
		policy.TotalPremiumPence == ceilBPS(policy.InsuredValuePence, policy.PremiumBPS) && !policy.TermsChanged
}

func sameInsuranceTerms(left, right InsurancePolicy) bool {
	return strings.EqualFold(left.ID, right.ID) && strings.EqualFold(left.InsuredCompanyID, right.InsuredCompanyID) &&
		strings.EqualFold(left.ShipID, right.ShipID) && left.OriginPortID == right.OriginPortID && left.DestPortID == right.DestPortID &&
		left.InsuredValuePence == right.InsuredValuePence && left.HullValuePence == right.HullValuePence &&
		left.TotalPremiumPence == right.TotalPremiumPence && left.PremiumBPS == right.PremiumBPS &&
		left.CreatedAt.Equal(right.CreatedAt) && left.ExpiresAt.Equal(right.ExpiresAt)
}

func retainedInsuranceTermsAgree(policy InsurancePolicy, history []InsurancePolicy) bool {
	for _, retained := range history {
		if !strings.EqualFold(retained.ID, policy.ID) {
			continue
		}
		if retained.TermsChanged || !sameInsuranceTerms(retained, policy) ||
			retained.Status == "settled_safe" || retained.Status == "claimed_loss" || retained.Status == "cancelled" {
			return false
		}
	}
	return true
}

// ceilRatio calculates a bounded fractional liability without a floating-money
// conversion or overflowing an intermediate numerator.
func ceilRatio(value, numerator, denominator int64) int64 {
	hi, lo := bits.Mul64(uint64(value), uint64(numerator))
	quotient, remainder := bits.Div64(hi, lo, uint64(denominator))
	if remainder != 0 {
		quotient++
	}
	return int64(quotient)
}

// EvaluateUnderwriting admits a minimum fully collateralized pilot, never a
// claim of demonstrated positive EV. Direction-only observations omit hull,
// season and correlated-risk calibration. Unresolved policies are censored.
func EvaluateUnderwriting(policy InsurancePolicy, history []InsurancePolicy, budget UnderwritingBudget) UnderwritingDecision {
	decision := UnderwritingDecision{PolicyID: policy.ID, SliceBPS: 500}
	if !validHullPolicy(policy) || !retainedInsuranceTermsAgree(policy, history) || budget.Now.IsZero() || policy.ExpiresAt.IsZero() ||
		!policy.ExpiresAt.After(budget.Now) || policy.CreatedAt.After(budget.Now) || policy.UpdatedAt.After(budget.Now) ||
		(policy.Status != "open" && policy.Status != "active") || policy.CoveredBPS < 0 || policy.CoveredBPS > 9500 {
		decision.Reason = "policy lacks unchanged, live hull-only terms or minimum slice capacity"
		return decision
	}
	if len(budget.OwnCompanyIDs) == 0 {
		decision.Reason = "player company ownership is unavailable"
		return decision
	}
	for _, id := range budget.OwnCompanyIDs {
		if !insuranceUUID(id) {
			decision.Reason = "player company ownership identity is invalid"
			return decision
		}
		if strings.EqualFold(policy.InsuredCompanyID, id) {
			decision.Reason = "cannot underwrite a company owned by this player"
			return decision
		}
	}
	decision.CollateralPence = ceilBPS(policy.InsuredValuePence, decision.SliceBPS)
	decision.PremiumPence = policy.TotalPremiumPence / 20
	if budget.OpportunityCostPence < 0 || budget.OpportunityCostPence > maximumPence || budget.DiscretionaryPence < decision.CollateralPence ||
		budget.AvailableLocalPence < decision.CollateralPence || budget.RemainingPilotPence < decision.CollateralPence {
		decision.Reason = "full collateral exceeds uncommitted regional cash or the permanent pilot loss budget"
		return decision
	}

	byID := make(map[string]InsurancePolicy, len(history))
	for _, row := range history {
		key := strings.ToLower(row.ID)
		if previous, exists := byID[key]; exists {
			changed := previous.TermsChanged || row.TermsChanged || !sameInsuranceTerms(previous, row)
			previousTerminal := previous.Status == "settled_safe" || previous.Status == "claimed_loss"
			rowTerminal := row.Status == "settled_safe" || row.Status == "claimed_loss"
			if previousTerminal && rowTerminal && previous.Status != row.Status ||
				previousTerminal && !rowTerminal && !row.UpdatedAt.Before(previous.UpdatedAt) ||
				rowTerminal && !previousTerminal && !previous.UpdatedAt.Before(row.UpdatedAt) {
				changed = true
			}
			if previousTerminal && !rowTerminal || previous.UpdatedAt.After(row.UpdatedAt) {
				row = previous
			}
			row.TermsChanged = changed
		}
		byID[key] = row
	}
	var totalObservedHours float64
	for _, row := range byID {
		if strings.EqualFold(row.ID, policy.ID) || !validHullPolicy(row) || row.UpdatedAt.After(budget.Now) ||
			row.OriginPortID != policy.OriginPortID || row.DestPortID != policy.DestPortID ||
			(row.Status != "settled_safe" && row.Status != "claimed_loss") {
			continue
		}
		decision.ObservedPolicies++
		if row.Status == "claimed_loss" {
			decision.ObservedLosses++
		}
		totalObservedHours += row.UpdatedAt.Sub(row.CreatedAt).Hours()
	}
	if decision.ObservedPolicies < 5 {
		decision.Reason = "fewer than five distinct closed hull-only policies on this direction"
		return decision
	}

	// Beta(1,1) posterior mean: an explicit model assumption, not measured
	// hazard probability. Use its exact rational value when pricing claims.
	numerator, denominator := decision.ObservedLosses+1, decision.ObservedPolicies+2
	decision.EstimatedLossBPS = ceilRatio(10000, numerator, denominator)
	decision.ExpectedClaimsPence = ceilRatio(decision.CollateralPence, numerator, denominator)
	decision.ExpectedNetPence = decision.PremiumPence - decision.ExpectedClaimsPence - budget.OpportunityCostPence

	// One-sided, fixed-look 95% Wilson upper sensitivity under an iid
	// Bernoulli assumption; repeated decisions do not make this anytime-valid.
	n := float64(decision.ObservedPolicies)
	p := float64(decision.ObservedLosses) / n
	const z = 1.6448536269514722
	upper := (p + z*z/(2*n) + z*math.Sqrt(p*(1-p)/n+z*z/(4*n*n))) / (1 + z*z/n)
	decision.ConservativeLossBPS = min(10000, max(decision.EstimatedLossBPS, int64(math.Ceil(upper*10000))))
	if budget.TimeScale > 0 && !math.IsNaN(budget.TimeScale) && !math.IsInf(budget.TimeScale, 0) {
		// Creation-to-last-update is only a duration proxy, not the lock time
		// of an available slice or a guaranteed release deadline.
		decision.EstimatedLockGameDays = totalObservedHours * budget.TimeScale / (24 * n)
	}
	decision.Accepted = decision.ExpectedNetPence > 0
	decision.Reason = "minimum hull-only pilot has positive modeled net after claims and opportunity cost; full loss remains affordable; all-era history is not calibrated to the announced capture risk"
	if !decision.Accepted {
		decision.Reason = "modeled underwriting net is not positive after claims and opportunity cost; all-era history is not calibrated to the announced capture risk"
	}
	return decision
}
