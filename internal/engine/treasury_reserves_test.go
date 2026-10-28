package engine

import "testing"

func TestObligationReservesFundObservedArrivalNotSailingOrigin(t *testing.T) {
	t.Parallel()
	const reserve int64 = 75_000
	state := AccountState{
		HomePortID: recoveryHomePort,
		Catalog: Catalog{
			Ports: []Port{
				{ID: recoveryHomePort, BankingRegion: "north"},
				{ID: recoveryFarPort, BankingRegion: "south"},
			},
			ShipClasses: []Hull{{ID: recoveryHull, HullCategory: "coastal", HoldCapacityTons: 60}},
		},
		Ports: map[string]PortDetail{
			recoveryHomePort: {ID: recoveryHomePort, BerthDuesPerDayPence: 100},
			recoveryFarPort:  {ID: recoveryFarPort, BerthDuesPerDayPence: 100},
		},
	}
	outgoing := ShipState{RouteLegs: []RouteLeg{{FromNode: recoveryHomePort, ToNode: recoveryFarPort}}}
	outgoing.Ship.ID = recoveryShipBlastoise
	outgoing.Ship.ShipClass = recoveryHull
	outgoing.Ship.Status = "sailing"
	outgoing.Ship.CurrentPortID = recoveryHomePort
	returning := ShipState{RouteLegs: []RouteLeg{
		{FromNode: recoveryFarPort, ToNode: "sea-zone"},
		{FromNode: "sea-zone", ToNode: recoveryHomePort},
	}}
	returning.Ship.ID = recoveryShipCharizard
	returning.Ship.ShipClass = recoveryHull
	returning.Ship.Status = "sailing"
	returning.Ship.CurrentPortID = recoveryFarPort
	state.Ships = []ShipState{outgoing, returning}

	// Both committed arrivals need funding before the treasury recovery can
	// finish: north for the returning ship, south for the outgoing ship.
	required := obligationReserves(state, nil, reserve)
	if required["south"] != 75_300 {
		t.Fatalf("outgoing arrival did not preserve its regional reserve and 300p harbour envelope: %v", required)
	}
	wantNorth := reserve + 3*DailyUpkeep(state.Ships, state.Catalog, 0) + 300
	if required["north"] != wantNorth {
		t.Fatalf("returning arrival and fixed home bills need %dp, got %v", wantNorth, required)
	}

	// A subsequent authoritative fleet replaces the outgoing ship with a
	// different berthed ship. Charizard still retains portb as its origin,
	// but neither remaining ship has an observed south-region obligation.
	berthed := ShipState{}
	berthed.Ship.ID = recoveryShipLapras
	berthed.Ship.ShipClass = recoveryHull
	berthed.Ship.Status = "berthed"
	berthed.Ship.CurrentPortID = recoveryHomePort
	state.Ships = []ShipState{berthed, returning}
	state.Factors = []string{recoveryFarPort}
	required = obligationReserves(state, nil, reserve)
	if required["south"] != 0 {
		t.Fatalf("sailing retained origin invented another protected south berth: %v", required)
	}
	wantNorth = reserve + 3*DailyUpkeep(state.Ships, state.Catalog, len(state.Factors)) + 600
	if required["north"] != wantNorth {
		t.Fatalf("stationary presence, incoming arrival and factor-inclusive home bills need %dp, got %v", wantNorth, required)
	}
}
