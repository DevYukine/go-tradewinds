package engine

import (
	"testing"
	"time"
)

func TestLloydsSliceNet(t *testing.T) {
	now := time.Date(2026, 10, 25, 12, 0, 0, 0, time.UTC)
	policy := InsurancePolicy{
		ID:                "01ad0001-0000-7000-8000-0000000c0a01",
		InsuredCompanyID:  "01a16469-7564-711d-b8da-e69d7d73e623",
		ShipID:            "01a169e2-69a5-70f9-9cf0-921c074b2e00",
		OriginPortID:      "lisbon",
		DestPortID:        "cadiz",
		InsuredValuePence: 100_000_000,
		HullValuePence:    100_000_000,
		PremiumBPS:        300,
		TotalPremiumPence: ceilBPS(100_000_000, 300),
		CoveredBPS:        0,
		Status:            "open",
		CreatedAt:         now.Add(-time.Hour),
		UpdatedAt:         now,
		ExpiresAt:         now.Add(24 * time.Hour),
	}
	if !validHullPolicy(policy) {
		t.Fatal("acceptance fixture is not a complete, valid hull-only policy")
	}
	expectedLoss, margin := int64(100), int64(200)
	for _, tc := range []struct {
		name   string
		change func(*InsurancePolicy, *LloydsEstimate)
		wantOK bool
	}{
		{name: "profitable hull-only slice", wantOK: true},
		{name: "margin below fifty bps", change: func(_ *InsurancePolicy, e *LloydsEstimate) {
			lowMargin := int64(40)
			e.PremiumMarginBPS = &lowMargin
		}},
		{name: "estimate consumes premium", change: func(_ *InsurancePolicy, e *LloydsEstimate) {
			loss := int64(300)
			e.ExpectedLossBPS = &loss
		}},
		{name: "missing expected loss", change: func(_ *InsurancePolicy, e *LloydsEstimate) {
			e.ExpectedLossBPS = nil
		}},
		{name: "missing premium margin", change: func(_ *InsurancePolicy, e *LloydsEstimate) {
			e.PremiumMarginBPS = nil
		}},
		{name: "cargo-inclusive policy", change: func(p *InsurancePolicy, _ *LloydsEstimate) {
			p.HullValuePence = p.InsuredValuePence - 1
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			candidate := policy
			estimate := LloydsEstimate{
				PolicyID:         candidate.ID,
				ExpectedLossBPS:  &expectedLoss,
				PremiumMarginBPS: &margin,
			}
			if tc.change != nil {
				tc.change(&candidate, &estimate)
			}
			net, ok := lloydsSliceNet(candidate, estimate, 500)
			if ok != tc.wantOK {
				t.Fatalf("lloydsSliceNet = (%d, %t), want ok=%t", net, ok, tc.wantOK)
			}
			if tc.wantOK && net != 100_000 {
				t.Fatalf("slice net = %d pence, want 150000 premium - 50000 expected loss = 100000", net)
			}
		})
	}
}
