package engine

import (
	"testing"
	"time"
)

func TestTaperedCharterRewardFollowsCrownPremiumTaper(t *testing.T) {
	t.Parallel()
	catalog := Catalog{Commodities: []Commodity{{ID: "grain", BasePricePence: 1_000}}}
	reward := int64(3_000)
	crown := Contract{ContractType: "charter", CommodityID: "grain", RemainingQuantity: 100, RewardPerTonPence: &reward}
	player := crown
	player.IssuerCompanyID = "issuer"
	unpriced := crown
	unpriced.CommodityID = "unlisted"
	for _, test := range []struct {
		name     string
		contract Contract
		earned   int64
		tons     int64
		want     int64
	}{
		// 50 t at 30 Cr bring earnings to exactly 50,000 Cr: no taper yet.
		{"threshold including this delivery", crown, 4_850_000, 50, 3_000},
		// 50,500 Cr: the 20 Cr premium is cut by sqrt(50,000 / 50,500).
		{"just above threshold", crown, 4_900_000, 50, 1_000 + 1_990},
		{"delivery capped at remaining demand", crown, 4_850_000, 500, 1_000 + 1_970},
		{"player charters never taper", player, 50_000_000, 50, 3_000},
		{"unknown value tapers the whole reward", unpriced, 15_000_000, 0, 1_732},
	} {
		t.Run(test.name, func(t *testing.T) {
			if got := taperedCharterReward(test.contract, catalog, test.earned, test.tons); got != test.want {
				t.Fatalf("tapered reward = %d, want %d", got, test.want)
			}
		})
	}
}

func TestCharterTaperKeepsSourcingEvidenceAndRefusalTerms(t *testing.T) {
	t.Parallel()
	catalog := Catalog{Commodities: []Commodity{{ID: "grain", BasePricePence: 1_000}}}
	reward := int64(3_000)
	contracts := []Contract{{ContractType: "charter", OriginPortID: "portsmouth", CommodityID: "grain", RemainingQuantity: 100, RewardPerTonPence: &reward}}
	quiet := taperedContracts(contracts, catalog, 0, 40)[0]
	busy := taperedContracts(contracts, catalog, 20_000_000, 40)[0]
	if *busy.RewardPerTonPence >= reward {
		t.Fatalf("busy charter earnings did not taper the reward: %d", *busy.RewardPerTonPence)
	}
	if sourcingEvidenceKey("amsterdam", quiet, 900) != sourcingEvidenceKey("amsterdam", busy, 900) {
		t.Fatal("charter earnings changed the sourcing evidence key")
	}
	if observedCharterTerms(quiet) != observedCharterTerms(busy) {
		t.Fatal("charter earnings changed the refusal terms")
	}
	in := PlanningInput{
		Now:                time.Now(),
		SourcingProbeLanes: map[string]bool{sourcingEvidenceKey("amsterdam", quiet, 900): true},
	}
	if sourcingProbeRejection(in, "amsterdam", busy, 900) == "" {
		t.Fatal("a tapered reward reopened an already inspected source")
	}
}
