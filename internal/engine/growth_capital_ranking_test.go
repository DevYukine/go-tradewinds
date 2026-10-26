package engine

import "testing"

func TestGrowthRankingIncludesNewRegionalCashFloors(t *testing.T) {
	t.Parallel()
	remote := growthPackage{
		port:           Port{ID: "remote"},
		decision:       GrowthDecision{QuotedHullPence: 99000, IncrementalNetPencePerGameDay: 2000},
		committedPence: 249100,
	}
	established := growthPackage{
		port:           Port{ID: "established"},
		decision:       GrowthDecision{QuotedHullPence: 120000, IncrementalNetPencePerGameDay: 1800},
		committedPence: 120000,
	}
	if betterGrowthPackage(remote, established) || !betterGrowthPackage(established, remote) {
		t.Fatal("higher gross daily contribution won despite lower return on newly committed ship and regional cash")
	}
	// Once a region already has its operating floor, do not charge that same
	// protected capital to every later ship purchase.
	remote.committedPence = 99000
	if !betterGrowthPackage(remote, established) {
		t.Fatal("already-protected regional cash was charged again to the marginal ship")
	}
}
