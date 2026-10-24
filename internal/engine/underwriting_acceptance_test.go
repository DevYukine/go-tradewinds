package engine

// Failure-first acceptance contract for underwriting policy evaluation (plan
// E5), written before internal/engine/underwriting.go exists so the compiled
// behavior, not the plumbing, is what gets built:
//
//   - Exactly one minimum 500bps hull-only pilot slice is ever priced: the
//     collateral ceiling rounds a non-divisible insured value up, the premium
//     share rounds the total premium down, and both stay integer pence.
//   - Risk is estimated only from the honest cohort: same origin/destination,
//     hull-only cover, deduplicated IDs, terminal settled_safe/claimed_loss
//     rows with complete positive terms. Unresolved, duplicate, opposite
//     direction, cargo-inclusive and incomplete rows are censored or filtered,
//     never counted as zero-claim successes.
//   - Contradictory terminal outcomes and terminal-to-nonterminal
//     regressions censor the ambiguous policy ID entirely, independent of
//     snapshot input order: the censored ID is neither a confirmed loss nor
//     a zero-claim success, and only the remaining valid rows decide.
//   - The estimated sink probability is the Beta(1,1) posterior mean
//     (losses+1)/(closed+2), a labeled conservative modeling assumption, and
//     the nominal 95% fixed-look upper sensitivity never sits below it.
//   - Acceptance requires a strictly positive expected net after ceiling the
//     modeled claims and charging the shipping opportunity cost, and the FULL
//     loss collateral must fit inside the discretionary, available-local and
//     remaining pilot budgets independently. Fitting the expected claims is
//     not affordability.
//
// Every decision below is a pure function of the fixtures: no clock, network
// or database. The money values are chosen from the recorded Cadiz/Lisbon
// policy shapes so each expected number is hand-checked integer arithmetic.

import (
	"fmt"
	"math"
	"strings"
	"testing"
	"time"
)

const (
	uwAccOwnCompanyID     = "01a164a1-2484-73a1-a8ee-745fc83a28ae"
	uwAccForeignCompanyID = "01a16469-7564-711d-b8da-e69d7d73e623"
	uwAccLisbonPortID     = "lisbon"
	uwAccCadizPortID      = "cadiz"
	uwAccCandidateID      = "01ad0001-0000-7000-8000-0000000c0a01"
)

var uwAccNow = time.Date(2026, 10, 24, 0, 0, 0, 0, time.UTC)

// uwAccFixture describes one insurance policy row in the shape the public
// board reports: integer pence, basis points and port/company identifiers.
type uwAccFixture struct {
	id             string
	status         string
	insuredCompany string
	originPort     string
	destPort       string
	insuredPence   int64
	hullPence      int64
	premiumBPS     int64
	totalPremium   int64
	coveredBPS     int64
	createdAt      time.Time
	expiresAt      time.Time
}

func (f uwAccFixture) policy() InsurancePolicy {
	created := f.createdAt
	if created.IsZero() {
		created = uwAccNow.Add(-6 * time.Hour)
	}
	return InsurancePolicy{
		ID:                f.id,
		InsuredCompanyID:  f.insuredCompany,
		ShipID:            "01a169e2-69a5-70f9-9cf0-921c074b2e00",
		OriginPortID:      f.originPort,
		DestPortID:        f.destPort,
		InsuredValuePence: f.insuredPence,
		HullValuePence:    f.hullPence,
		PremiumBPS:        f.premiumBPS,
		TotalPremiumPence: f.totalPremium,
		CoveredBPS:        f.coveredBPS,
		Status:            f.status,
		CreatedAt:         created,
		UpdatedAt:         uwAccNow.Add(-1 * time.Hour),
		ExpiresAt:         f.expiresAt,
	}
}

// uwAccRecordedSafeShapes are insured/hull/premium triples lifted from the
// recorded Lisbon<->Cadiz board so fixtures carry plausible magnitudes; each
// total premium is ceil(insured*premium_bps/10000) within the documented
// 10..3000bps quote range.
var uwAccRecordedSafeShapes = []struct {
	insuredPence int64
	hullPence    int64
	premiumBPS   int64
	totalPremium int64
}{
	{2_579_000, 3_621_832, 146, 37_654},
	{3_542_000, 5_030_893, 146, 51_714},
	{2_679_500, 3_269_238, 191, 51_179},
	{2_579_000, 3_258_773, 1098, 283_175},
	{2_000_000, 3_000_000, 750, 150_000},
	{2_200_000, 3_100_000, 727, 159_940},
	{2_400_000, 3_300_000, 708, 169_920},
	{2_300_000, 3_200_000, 717, 164_910},
}

func uwAccHistoryID(n int) string {
	return fmt.Sprintf("01ad1001-0000-7000-8000-%012d", n)
}

// uwAccClosedRow builds one terminal Lisbon->Cadiz hull-only history row.
func uwAccClosedRow(id, status string, shape int) uwAccFixture {
	s := uwAccRecordedSafeShapes[shape%len(uwAccRecordedSafeShapes)]
	return uwAccFixture{
		id:             id,
		status:         status,
		insuredCompany: uwAccForeignCompanyID,
		originPort:     uwAccLisbonPortID,
		destPort:       uwAccCadizPortID,
		insuredPence:   s.insuredPence,
		hullPence:      s.hullPence,
		premiumBPS:     s.premiumBPS,
		totalPremium:   s.totalPremium,
		coveredBPS:     10000,
		expiresAt:      uwAccNow.Add(-1 * time.Hour),
	}
}

func uwAccOppositeRow(id string, shape int) uwAccFixture {
	row := uwAccClosedRow(id, "settled_safe", shape)
	row.originPort, row.destPort = uwAccCadizPortID, uwAccLisbonPortID
	return row
}

// uwAccCargoRow builds a cargo-inclusive row: the insured value exceeds the
// hull value, so its outcome says nothing about hull-only hazard.
func uwAccCargoRow(id, status string) uwAccFixture {
	return uwAccFixture{
		id:             id,
		status:         status,
		insuredCompany: uwAccForeignCompanyID,
		originPort:     uwAccLisbonPortID,
		destPort:       uwAccCadizPortID,
		insuredPence:   6_979_000,
		hullPence:      3_286_449,
		premiumBPS:     912,
		totalPremium:   636_485,
		coveredBPS:     10000,
		expiresAt:      uwAccNow.Add(-1 * time.Hour),
	}
}

func uwAccUnresolvedRow(id string) uwAccFixture {
	row := uwAccClosedRow(id, "active", 0)
	row.coveredBPS = 2000
	row.expiresAt = uwAccNow.Add(12 * time.Hour)
	return row
}

// uwAccObservedRow builds one snapshot observation of a policy at a specific
// time, for chronology-sensitive duplicate histories.
func uwAccObservedRow(id, status string, observedAt time.Time) uwAccFixture {
	row := uwAccClosedRow(id, status, 0)
	row.createdAt = observedAt
	return row
}

func uwAccSafeHistory(n int) []InsurancePolicy {
	rows := make([]InsurancePolicy, 0, n)
	for i := range n {
		rows = append(rows, uwAccClosedRow(uwAccHistoryID(i+1), "settled_safe", i).policy())
	}
	return rows
}

// uwAccCandidate is an otherwise ideal pilot target: foreign owned, active,
// unexpired, hull-only with the full 10000bps still available, funded with a
// premium share far above the modeled claims on an honest safe cohort. The
// premium quote is the documented maximum legal terms: 3000bps on 2_579_000p
// is exactly 773_700p.
func uwAccCandidate() InsurancePolicy {
	return uwAccFixture{
		id:             uwAccCandidateID,
		status:         "active",
		insuredCompany: uwAccForeignCompanyID,
		originPort:     uwAccLisbonPortID,
		destPort:       uwAccCadizPortID,
		insuredPence:   2_579_000,
		hullPence:      4_000_000,
		premiumBPS:     3000,
		totalPremium:   773_700,
		coveredBPS:     0,
		expiresAt:      uwAccNow.Add(6 * 24 * time.Hour),
	}.policy()
}

func uwAccBudget() UnderwritingBudget {
	return UnderwritingBudget{
		Now:                  uwAccNow,
		OwnCompanyIDs:        []string{uwAccOwnCompanyID},
		DiscretionaryPence:   1_000_000,
		AvailableLocalPence:  1_000_000,
		RemainingPilotPence:  1_000_000,
		OpportunityCostPence: 0,
	}
}

func uwAccRequireRejected(t *testing.T, why string, decision UnderwritingDecision) {
	t.Helper()
	if decision.Accepted {
		t.Fatalf("%s: underwriting was accepted although the contract requires rejection: %+v", why, decision)
	}
	if decision.Reason == "" {
		t.Fatalf("%s: rejection carries no reason for the operator: %+v", why, decision)
	}
	if decision.CollateralPence < 0 || decision.PremiumPence < 0 {
		t.Fatalf("%s: integer overflow produced negative money: %+v", why, decision)
	}
}

func TestEvaluateUnderwritingAcceptsMinimumFiveHundredBPSPilotFromDirectionMatchedHistory(t *testing.T) {
	decision := EvaluateUnderwriting(uwAccCandidate(), uwAccSafeHistory(8), uwAccBudget())

	if !decision.Accepted {
		t.Fatalf("funded minimum pilot on a direction-matched 8/0 safe hull-only cohort must be accepted: %+v", decision)
	}
	if decision.PolicyID != uwAccCandidateID {
		t.Fatalf("accepted decision must identify the priced policy %q, got %q", uwAccCandidateID, decision.PolicyID)
	}
	if decision.SliceBPS != 500 {
		t.Fatalf("pilot is exactly the minimum 500bps slice with no batching, got %d", decision.SliceBPS)
	}
	if decision.CollateralPence != 128_950 {
		t.Fatalf("full-loss collateral is 2_579_000p * 500bps = 128950p, got %d", decision.CollateralPence)
	}
	if decision.PremiumPence != 38_685 {
		t.Fatalf("conservative premium floor is floor(773_700p * 500bps) = 38685p, got %d", decision.PremiumPence)
	}
	if decision.ObservedPolicies != 8 || decision.ObservedLosses != 0 {
		t.Fatalf("honest cohort is 8 closed safe examples, got %d closed/%d losses", decision.ObservedPolicies, decision.ObservedLosses)
	}
	if decision.EstimatedLossBPS != 1000 {
		t.Fatalf("Beta(1,1) posterior mean (0+1)/(8+2) is exactly 1000bps, got %d", decision.EstimatedLossBPS)
	}
	if decision.ExpectedClaimsPence != 12_895 {
		t.Fatalf("modeled claims are ceil(128950p * 1/10) = 12895p, got %d", decision.ExpectedClaimsPence)
	}
	if decision.ExpectedNetPence != 25_790 {
		t.Fatalf("expected net is 38685p premium - 12895p claims - 0p opportunity = 25790p, got %d", decision.ExpectedNetPence)
	}
	if decision.ConservativeLossBPS < decision.EstimatedLossBPS || decision.ConservativeLossBPS > 10_000 {
		t.Fatalf("nominal 95%% upper sensitivity %d must sit between the posterior mean %d and the full scale", decision.ConservativeLossBPS, decision.EstimatedLossBPS)
	}
	if math.IsNaN(decision.EstimatedLockGameDays) || math.IsInf(decision.EstimatedLockGameDays, 0) || decision.EstimatedLockGameDays < 0 {
		t.Fatalf("estimated lock duration must be a finite non-negative game-day estimate, got %f", decision.EstimatedLockGameDays)
	}
}

func TestEvaluateUnderwritingCollateralCeilAndPremiumFloorRounding(t *testing.T) {
	t.Run("collateral ceiling rounds a non-divisible insured value up", func(t *testing.T) {
		candidate := uwAccCandidate()
		candidate.InsuredValuePence = 1_234_567
		candidate.PremiumBPS = 3000
		candidate.TotalPremiumPence = 370_371

		decision := EvaluateUnderwriting(candidate, uwAccSafeHistory(8), uwAccBudget())

		if !decision.Accepted {
			t.Fatalf("funded pilot on a safe cohort must be accepted regardless of indivisibility: %+v", decision)
		}
		if decision.CollateralPence != 61_729 {
			t.Fatalf("collateral is ceil(1_234_567p * 500bps) = 61729p, got %d", decision.CollateralPence)
		}
		if decision.ExpectedClaimsPence != 6_173 {
			t.Fatalf("modeled claims are ceil(61729p * 1/10) = 6173p, got %d", decision.ExpectedClaimsPence)
		}
		if decision.ExpectedNetPence != 12_345 {
			t.Fatalf("expected net is 18518p premium floor - 6173p claims = 12345p, got %d", decision.ExpectedNetPence)
		}
	})

	t.Run("divisible insured value needs no rounding", func(t *testing.T) {
		candidate := uwAccCandidate()
		candidate.InsuredValuePence = 1_000_000
		candidate.PremiumBPS = 3000
		candidate.TotalPremiumPence = 300_000

		decision := EvaluateUnderwriting(candidate, uwAccSafeHistory(8), uwAccBudget())

		if !decision.Accepted {
			t.Fatalf("divisible funded pilot must be accepted: %+v", decision)
		}
		if decision.CollateralPence != 50_000 {
			t.Fatalf("collateral is exactly 1_000_000p * 500bps = 50000p, got %d", decision.CollateralPence)
		}
		if decision.ExpectedClaimsPence != 5_000 {
			t.Fatalf("modeled claims are exactly 50000p * 1/10 = 5000p, got %d", decision.ExpectedClaimsPence)
		}
	})

	t.Run("premium floor rounds down and can erase a naive edge", func(t *testing.T) {
		candidate := uwAccCandidate()
		candidate.InsuredValuePence = 1_234_571
		candidate.PremiumBPS = 1000
		candidate.TotalPremiumPence = 123_458

		decision := EvaluateUnderwriting(candidate, uwAccSafeHistory(8), uwAccBudget())

		uwAccRequireRejected(t, "unrounded premium and claims shares favor the underwriter by 0.045p but floor 6172p minus ceiling claims 6173p is a 1p conservative loss", decision)
	})
}

func TestEvaluateUnderwritingFullCollateralMustFitEachBudget(t *testing.T) {
	cases := []struct {
		name   string
		mutate func(*UnderwritingBudget)
	}{
		{"pilot budget covering only the expected claims cannot fund the full loss", func(b *UnderwritingBudget) {
			b.RemainingPilotPence = 12_895
		}},
		{"pilot budget one pence short of full collateral", func(b *UnderwritingBudget) {
			b.RemainingPilotPence = 128_949
		}},
		{"discretionary cash below full collateral", func(b *UnderwritingBudget) {
			b.DiscretionaryPence = 128_949
		}},
		{"available local shipping cash below full collateral", func(b *UnderwritingBudget) {
			b.AvailableLocalPence = 128_949
		}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			budget := uwAccBudget()
			tc.mutate(&budget)

			uwAccRequireRejected(t, tc.name, EvaluateUnderwriting(uwAccCandidate(), uwAccSafeHistory(8), budget))
		})
	}

	t.Run("exactly full collateral in every budget admits", func(t *testing.T) {
		budget := uwAccBudget()
		budget.DiscretionaryPence = 128_950
		budget.AvailableLocalPence = 128_950
		budget.RemainingPilotPence = 128_950

		decision := EvaluateUnderwriting(uwAccCandidate(), uwAccSafeHistory(8), budget)

		if !decision.Accepted {
			t.Fatalf("budgets that exactly cover the 128950p full loss must admit: %+v", decision)
		}
	})
}

func TestEvaluateUnderwritingRequiresStrictlyPositiveNetAfterOpportunityCost(t *testing.T) {
	cases := []struct {
		name             string
		opportunityPence int64
		wantAccepted     bool
		wantNet          int64
		pinNet           bool
	}{
		// The honest net before opportunity cost is 38685p - 12895p = 25790p.
		{"net of exactly zero is not a positive estimate", 25_790, false, 0, false},
		{"net of minus one pence is rejected", 25_791, false, -1, true},
		{"net of plus one pence is a positive estimate", 25_789, true, 1, true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			budget := uwAccBudget()
			budget.OpportunityCostPence = tc.opportunityPence

			decision := EvaluateUnderwriting(uwAccCandidate(), uwAccSafeHistory(8), budget)

			if decision.Accepted != tc.wantAccepted {
				t.Fatalf("opportunity cost %dp must %s the pilot: %+v", tc.opportunityPence, map[bool]string{true: "not block", false: "block"}[tc.wantAccepted], decision)
			}
			if tc.pinNet && decision.ExpectedNetPence != tc.wantNet {
				t.Fatalf("expected net must stay reported including the opportunity charge (38685p premium - 12895p claims - %dp opportunity = %dp), got %d", tc.opportunityPence, tc.wantNet, decision.ExpectedNetPence)
			}
		})
	}
}

func TestEvaluateUnderwritingPooledLookSafeCohortStillRejectsOnTrueDirectionLosses(t *testing.T) {
	// The true Lisbon->Cadiz hull-only cohort carries two losses in six closed
	// policies. Pooling in twenty opposite-direction safes, five cargo-inclusive
	// safes, two unresolved actives and a duplicate row would dilute that to
	// (2+1)/(31+2)=1/11, pricing only 11723p of claims against the 12895p
	// premium floor and admitting; the direction-matched posterior
	// (2+1)/(6+2)=3/8 prices 48357p of claims and must reject.
	history := []InsurancePolicy{
		uwAccClosedRow(uwAccHistoryID(1), "claimed_loss", 0).policy(),
		uwAccClosedRow(uwAccHistoryID(2), "claimed_loss", 1).policy(),
		uwAccClosedRow(uwAccHistoryID(3), "settled_safe", 2).policy(),
		uwAccClosedRow(uwAccHistoryID(4), "settled_safe", 3).policy(),
		uwAccClosedRow(uwAccHistoryID(5), "settled_safe", 4).policy(),
		uwAccClosedRow(uwAccHistoryID(6), "settled_safe", 5).policy(),
	}
	for i := range 20 {
		history = append(history, uwAccOppositeRow(uwAccHistoryID(100+i), i).policy())
	}
	for i := range 5 {
		history = append(history, uwAccCargoRow(uwAccHistoryID(200+i), "settled_safe").policy())
	}
	history = append(history, uwAccUnresolvedRow(uwAccHistoryID(300)).policy())
	history = append(history, history[0])

	candidate := uwAccCandidate()
	candidate.PremiumBPS = 1000
	candidate.TotalPremiumPence = 257_900

	decision := EvaluateUnderwriting(candidate, history, uwAccBudget())

	uwAccRequireRejected(t, "direction-matched posterior prices claims above the premium floor", decision)
	if decision.ObservedPolicies != 6 {
		t.Fatalf("only the six deduplicated direction-matched closed rows form the cohort, got %d", decision.ObservedPolicies)
	}
	if decision.ObservedLosses != 2 {
		t.Fatalf("both direction-matched hull losses must be observed, got %d", decision.ObservedLosses)
	}
	if decision.ExpectedClaimsPence != 48_357 {
		t.Fatalf("modeled claims are ceil(128950p * 3/8) = 48357p, got %d", decision.ExpectedClaimsPence)
	}
}

func TestEvaluateUnderwritingCountsOnlyDirectionMatchedHullOnlyClosedCohort(t *testing.T) {
	// At the documented 3000bps premium maximum the 500bps slice floor is
	// 38685p, so a 2/6 cohort (claims 48357p) can never price positive on
	// these terms. The loss-inclusive positive case is one loss in eight
	// closed policies: the Beta(1,1) posterior mean (1+1)/(8+2)=1/5 prices
	// 25790p of claims against a 38685p floor and still clears it.
	history := []InsurancePolicy{
		uwAccClosedRow(uwAccHistoryID(1), "claimed_loss", 0).policy(),
		uwAccClosedRow(uwAccHistoryID(2), "settled_safe", 1).policy(),
		uwAccClosedRow(uwAccHistoryID(3), "settled_safe", 2).policy(),
		uwAccClosedRow(uwAccHistoryID(4), "settled_safe", 3).policy(),
		uwAccClosedRow(uwAccHistoryID(5), "settled_safe", 4).policy(),
		uwAccClosedRow(uwAccHistoryID(6), "settled_safe", 5).policy(),
		uwAccClosedRow(uwAccHistoryID(7), "settled_safe", 6).policy(),
		uwAccClosedRow(uwAccHistoryID(8), "settled_safe", 7).policy(),
	}

	decision := EvaluateUnderwriting(uwAccCandidate(), history, uwAccBudget())

	if !decision.Accepted {
		t.Fatalf("a funded pilot on a 8/1 cohort within legal premium terms is still a positive estimate: %+v", decision)
	}
	if decision.ObservedPolicies != 8 || decision.ObservedLosses != 1 {
		t.Fatalf("cohort is 8 closed with 1 loss, got %d closed/%d losses", decision.ObservedPolicies, decision.ObservedLosses)
	}
	if decision.EstimatedLossBPS != 2000 {
		t.Fatalf("Beta(1,1) posterior mean (1+1)/(8+2) is exactly 2000bps, got %d", decision.EstimatedLossBPS)
	}
	if decision.ExpectedClaimsPence != 25_790 {
		t.Fatalf("modeled claims are ceil(128950p * 1/5) = 25790p, got %d", decision.ExpectedClaimsPence)
	}
	if decision.ExpectedNetPence != 12_895 {
		t.Fatalf("expected net is 38685p premium - 25790p claims = 12895p, got %d", decision.ExpectedNetPence)
	}
	if decision.ConservativeLossBPS < decision.EstimatedLossBPS {
		t.Fatalf("nominal 95%% upper sensitivity %d must not sit below the posterior mean %d", decision.ConservativeLossBPS, decision.EstimatedLossBPS)
	}
}

func TestEvaluateUnderwritingRejectsInadmissibleCandidatePolicies(t *testing.T) {
	cases := []struct {
		name         string
		mutate       func(*InsurancePolicy)
		wantAccepted bool
	}{
		{"insuring our own company is rejected", func(p *InsurancePolicy) {
			p.InsuredCompanyID = uwAccOwnCompanyID
		}, false},
		{"expired policy is rejected", func(p *InsurancePolicy) {
			p.ExpiresAt = uwAccNow.Add(-1 * time.Hour)
		}, false},
		{"already terminal settled_safe policy is rejected", func(p *InsurancePolicy) {
			p.Status = "settled_safe"
		}, false},
		{"unknown status is rejected", func(p *InsurancePolicy) {
			p.Status = ""
		}, false},
		{"remaining coverage below the 500bps minimum is rejected", func(p *InsurancePolicy) {
			p.CoveredBPS = 9600
		}, false},
		{"exactly 500bps of remaining coverage is admissible", func(p *InsurancePolicy) {
			p.CoveredBPS = 9500
		}, true},
		{"zero insured value is not a bounded positive term", func(p *InsurancePolicy) {
			p.InsuredValuePence = 0
		}, false},
		{"zero hull value is not a bounded positive term", func(p *InsurancePolicy) {
			p.HullValuePence = 0
		}, false},
		{"zero total premium cannot yield a positive net", func(p *InsurancePolicy) {
			p.TotalPremiumPence = 0
		}, false},
		{"cargo-inclusive cover is never underwritten", func(p *InsurancePolicy) {
			p.InsuredValuePence = 3_000_000
			p.HullValuePence = 2_500_000
		}, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			candidate := uwAccCandidate()
			tc.mutate(&candidate)

			decision := EvaluateUnderwriting(candidate, uwAccSafeHistory(8), uwAccBudget())

			if decision.Accepted != tc.wantAccepted {
				t.Fatalf("%s: accepted=%v, want %v: %+v", tc.name, decision.Accepted, tc.wantAccepted, decision)
			}
			if tc.wantAccepted && decision.SliceBPS != 500 {
				t.Fatalf("even at the coverage boundary the pilot stays the minimum 500bps slice, got %d", decision.SliceBPS)
			}
			if !tc.wantAccepted && decision.Reason == "" {
				t.Fatalf("%s: rejection carries no reason: %+v", tc.name, decision)
			}
		})
	}

	t.Run("int64 overflow scale is rejected without negative money", func(t *testing.T) {
		candidate := uwAccCandidate()
		candidate.InsuredValuePence = math.MaxInt64
		candidate.HullValuePence = math.MaxInt64
		candidate.TotalPremiumPence = math.MaxInt64

		uwAccRequireRejected(t, "unbounded terms must be rejected with overflow-checked integer money", EvaluateUnderwriting(candidate, uwAccSafeHistory(8), uwAccBudget()))
	})
}

func TestEvaluateUnderwritingOwnershipIdentityHandling(t *testing.T) {
	t.Run("own company membership matches case-insensitively", func(t *testing.T) {
		budget := uwAccBudget()
		budget.OwnCompanyIDs = []string{strings.ToUpper(uwAccForeignCompanyID)}

		uwAccRequireRejected(t, "an uppercase own-company entry must still block insuring our own risk", EvaluateUnderwriting(uwAccCandidate(), uwAccSafeHistory(8), budget))
	})

	t.Run("malformed or empty ownership entries fail closed", func(t *testing.T) {
		budget := uwAccBudget()
		budget.OwnCompanyIDs = []string{"", "not-a-uuid", "{01a164a1-2484-73a1-a8ee-745fc83a28ae}"}

		uwAccRequireRejected(t, "foreign ownership cannot be verified from a malformed or empty identity list", EvaluateUnderwriting(uwAccCandidate(), uwAccSafeHistory(8), budget))
	})

	t.Run("candidate with an empty insured company identity is rejected", func(t *testing.T) {
		candidate := uwAccCandidate()
		candidate.InsuredCompanyID = ""

		uwAccRequireRejected(t, "an unattributable insured company cannot be verified foreign", EvaluateUnderwriting(candidate, uwAccSafeHistory(8), uwAccBudget()))
	})
}

func TestEvaluateUnderwritingCensorsContradictoryTerminalObservationsAcrossPermutations(t *testing.T) {
	// One policy ID observed repeatedly over time: contradictory terminal
	// outcomes and terminal-to-nonterminal regressions censor the ambiguous
	// ID entirely, independently of input order. The censored ID is neither
	// a confirmed loss nor a zero-claim success, so only the four clean rows
	// remain and the cohort falls below the five-example minimum.
	cases := []struct {
		name       string
		underlying []uwAccFixture
	}{
		{"terminal loss followed by a newer nonterminal observation", []uwAccFixture{
			uwAccObservedRow(uwAccHistoryID(400), "claimed_loss", uwAccNow.Add(-8*time.Hour)),
			uwAccObservedRow(uwAccHistoryID(400), "active", uwAccNow.Add(-4*time.Hour)),
		}},
		{"terminal loss followed by a newer settled_safe observation", []uwAccFixture{
			uwAccObservedRow(uwAccHistoryID(401), "claimed_loss", uwAccNow.Add(-8*time.Hour)),
			uwAccObservedRow(uwAccHistoryID(401), "settled_safe", uwAccNow.Add(-2*time.Hour)),
		}},
		{"terminal safe superseded by a newer loss observation", []uwAccFixture{
			uwAccObservedRow(uwAccHistoryID(402), "settled_safe", uwAccNow.Add(-8*time.Hour)),
			uwAccObservedRow(uwAccHistoryID(402), "claimed_loss", uwAccNow.Add(-2*time.Hour)),
		}},
		{"loss active safe triple supplied in reverse input order", []uwAccFixture{
			uwAccObservedRow(uwAccHistoryID(403), "settled_safe", uwAccNow.Add(-2*time.Hour)),
			uwAccObservedRow(uwAccHistoryID(403), "active", uwAccNow.Add(-4*time.Hour)),
			uwAccObservedRow(uwAccHistoryID(403), "claimed_loss", uwAccNow.Add(-8*time.Hour)),
		}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			history := uwAccSafeHistory(4)
			for _, row := range tc.underlying {
				history = append(history, row.policy())
			}

			decision := EvaluateUnderwriting(uwAccCandidate(), history, uwAccBudget())

			uwAccRequireRejected(t, "the contradictory policy ID is censored and four valid rows stay below the five minimum", decision)
			if decision.ObservedPolicies != 4 || decision.ObservedLosses != 0 {
				t.Fatalf("the ambiguous ID must be excluded entirely, leaving the four clean rows (4 closed/0 losses), got %d closed/%d losses", decision.ObservedPolicies, decision.ObservedLosses)
			}
		})
	}
}

func TestEvaluateUnderwritingRejectsWhenHistoryCohortInsufficientOrAmbiguous(t *testing.T) {
	fiveRows := uwAccSafeHistory(5)
	duplicateInflated := append(uwAccSafeHistory(4), fiveRows[0])
	duplicateCensored := append(uwAccSafeHistory(5), fiveRows[0])
	conflictingDuplicate := uwAccSafeHistory(4)
	conflictingDuplicate = append(conflictingDuplicate,
		uwAccClosedRow(uwAccHistoryID(90), "settled_safe", 0).policy(),
		uwAccClosedRow(uwAccHistoryID(90), "settled_safe", 1).policy())
	missingIdentity := uwAccSafeHistory(5)
	missingIdentity[4].ID = ""
	invalidExpiry := uwAccSafeHistory(5)
	invalidExpiry[4].ExpiresAt = time.Time{}
	invalidCoverage := uwAccSafeHistory(5)
	invalidCoverage[4].CoveredBPS = 12000
	inconsistentPremium := uwAccSafeHistory(5)
	inconsistentPremium[4].PremiumBPS = 1000
	inconsistentPremium[4].TotalPremiumPence = 200_001
	unresolvedSample := append(uwAccSafeHistory(4), uwAccUnresolvedRow(uwAccHistoryID(50)).policy())
	zeroValueSample := uwAccSafeHistory(5)
	zeroValueSample[4].InsuredValuePence = 0
	oppositeOnly := make([]InsurancePolicy, 0, 6)
	for i := range 6 {
		oppositeOnly = append(oppositeOnly, uwAccOppositeRow(uwAccHistoryID(60+i), i).policy())
	}
	cargoSafeSample := append(uwAccSafeHistory(4),
		uwAccCargoRow(uwAccHistoryID(70), "settled_safe").policy(),
		uwAccCargoRow(uwAccHistoryID(71), "settled_safe").policy(),
		uwAccCargoRow(uwAccHistoryID(72), "settled_safe").policy())
	cargoLossSample := append(uwAccSafeHistory(4), uwAccCargoRow(uwAccHistoryID(80), "claimed_loss").policy())

	cases := []struct {
		name         string
		history      []InsurancePolicy
		wantAccepted bool
	}{
		{"empty history has no closed examples", nil, false},
		{"four closed examples are below the five minimum", uwAccSafeHistory(4), false},
		{"duplicate IDs collapse to four and stay below the minimum", duplicateInflated, false},
		{"duplicates are censored rather than counted twice", duplicateCensored, true},
		{"conflicting duplicate terms censor the ambiguous ID entirely", conflictingDuplicate, false},
		{"missing identity cannot anchor a closed example", missingIdentity, false},
		{"history row with a missing expiry window is censored", invalidExpiry, false},
		{"history row with coverage beyond the bps scale is censored", invalidCoverage, false},
		{"history row with inconsistent premium pricing is censored", inconsistentPremium, false},
		{"unresolved rows are censored, never successes", unresolvedSample, false},
		{"zero-value terminal rows are filtered, not zero-claim successes", zeroValueSample, false},
		{"opposite direction safes are a different cohort", oppositeOnly, false},
		{"cargo-inclusive safes are not hull-only successes", cargoSafeSample, false},
		{"cargo-inclusive claimed_loss is not a hull loss sample", cargoLossSample, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			decision := EvaluateUnderwriting(uwAccCandidate(), tc.history, uwAccBudget())

			if decision.Accepted != tc.wantAccepted {
				t.Fatalf("%s: accepted=%v, want %v: %+v", tc.name, decision.Accepted, tc.wantAccepted, decision)
			}
			if !tc.wantAccepted && decision.Reason == "" {
				t.Fatalf("%s: rejection carries no reason: %+v", tc.name, decision)
			}
		})
	}

	t.Run("duplicate censoring leaves exactly five observed policies", func(t *testing.T) {
		decision := EvaluateUnderwriting(uwAccCandidate(), duplicateCensored, uwAccBudget())

		if decision.ObservedPolicies != 5 {
			t.Fatalf("six rows sharing one duplicate ID are five distinct closed examples, got %d", decision.ObservedPolicies)
		}
		if decision.ObservedLosses != 0 {
			t.Fatalf("the safe cohort has no losses, got %d", decision.ObservedLosses)
		}
	})

	t.Run("economically rejected valid cohort still reports its uncertainty", func(t *testing.T) {
		budget := uwAccBudget()
		budget.OpportunityCostPence = 25_791

		decision := EvaluateUnderwriting(uwAccCandidate(), uwAccSafeHistory(8), budget)

		if decision.Accepted {
			t.Fatalf("an opportunity cost one pence above the honest net must reject: %+v", decision)
		}
		if decision.ObservedPolicies != 8 || decision.ObservedLosses != 0 {
			t.Fatalf("the fully valid cohort counts stay reported on economic rejection (8 closed/0 losses), got %d/%d", decision.ObservedPolicies, decision.ObservedLosses)
		}
		if decision.EstimatedLossBPS <= 0 || decision.ConservativeLossBPS < decision.EstimatedLossBPS || decision.ConservativeLossBPS > 10_000 {
			t.Fatalf("economic rejection must retain cohort sensitivity (estimated %dbps, conservative %dbps)", decision.EstimatedLossBPS, decision.ConservativeLossBPS)
		}
	})
}
