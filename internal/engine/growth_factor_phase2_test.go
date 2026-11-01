package engine

import (
	"testing"
	"time"
)

// Paid overseas expansion only admits phase-2 yards. The previous gate ranked
// any shipyard up to the unlocked phase, so freshly unlocked phase-1 yards in
// the home north_sea region or the Baltic out-ranked a genuinely payable
// phase-2 overseas target. With fresh two-sided historical gossip on every
// candidate pair and the whole fleet laid up in a third region, neither
// phase-1 region may rank while the phase-2 yard still does.
func TestExpansionAgentYardsRankOnlyPhaseTwoYards(t *testing.T) {
	t.Parallel()
	state := expansionFactorFixture()
	state.Clock.UnlockedPhase = 2
	state.Catalog.Ports = []Port{
		{ID: recoveryHomePort, BankingRegion: "north_sea", SeaZoneID: "home_sea", HasShipyard: true, Phase: 1},
		{ID: "home-partner", BankingRegion: "north_sea", SeaZoneID: "home_sea", Phase: 1},
		{ID: "baltic-yard", BankingRegion: "baltic", SeaZoneID: "baltic_sea", HasShipyard: true, Phase: 1},
		{ID: "baltic-partner", BankingRegion: "baltic", SeaZoneID: "baltic_sea", Phase: 1},
		{ID: "caribbean-yard", BankingRegion: "caribbean", SeaZoneID: "caribbean_sea", HasShipyard: true, Phase: 2},
		{ID: "caribbean-partner", BankingRegion: "caribbean", SeaZoneID: "caribbean_sea", Phase: 2},
		{ID: "layup-port", BankingRegion: "channel", SeaZoneID: "channel_sea", Phase: 2},
	}
	state.Catalog.SeaZones = []Zone{
		{ID: "home_sea", ZoneClass: "regional"},
		{ID: "baltic_sea", ZoneClass: "regional"},
		{ID: "caribbean_sea", ZoneClass: "regional"},
		{ID: "channel_sea", ZoneClass: "regional"},
	}
	now := time.Now().UTC().Add(-time.Second)
	ask, bid, partnerAsk, yardBid := int64(100), int64(200), int64(100), int64(150)
	detail := state.Ports[recoveryHomePort]
	detail.CoffeeHouseGossip = nil
	for _, pair := range [][2]string{
		{recoveryHomePort, "home-partner"},
		{"baltic-yard", "baltic-partner"},
		{"caribbean-yard", "caribbean-partner"},
	} {
		detail.CoffeeHouseGossip = append(detail.CoffeeHouseGossip,
			Gossip{SubjectPortID: pair[0], SnapshotAt: now, Commodities: []GossipCommodity{
				{CommodityID: "x", SnapshotAt: now, BestAskPence: &ask},
				{CommodityID: "y", SnapshotAt: now, BestBidPence: &yardBid},
			}},
			Gossip{SubjectPortID: pair[1], SnapshotAt: now, Commodities: []GossipCommodity{
				{CommodityID: "x", SnapshotAt: now, BestBidPence: &bid},
				{CommodityID: "y", SnapshotAt: now, BestAskPence: &partnerAsk},
			}},
		)
	}
	state.Ports = map[string]PortDetail{recoveryHomePort: detail}
	ship := ShipState{}
	ship.Ship.Status, ship.Ship.CurrentPortID = "berthed", "layup-port"
	state.Ships = []ShipState{ship}

	pairs := expansionAgentYards(state, expansionFactorInput(state))
	if len(pairs) != 1 || pairs[0].yard.ID != "caribbean-yard" || pairs[0].partner.ID != "caribbean-partner" {
		ranked := make([]string, 0, len(pairs))
		for _, pair := range pairs {
			ranked = append(ranked, pair.yard.ID)
		}
		t.Fatalf("paid exploration did not rank only the phase-2 yard: %v", ranked)
	}
}
