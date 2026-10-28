package sim

import (
	"math"
	"testing"
	"time"

	"github.com/DevYukine/go-tradewinds/internal/engine"
)

func TestFleetAppliesArrivalBetweenFramesAndBurnsStoresByState(t *testing.T) {
	at := time.Date(2026, 10, 20, 0, 0, 0, 0, time.UTC)
	ship := engine.ShipState{HoldCapacityTons: 100, EffectiveHoldCapacityTons: 100}
	ship.Ship.ID, ship.Ship.ShipClass = "one", "hoy"
	ship.Ship.CurrentPortID, ship.Ship.Status = "a", "berthed"
	ship.Stores.ProvisionsTons = 10
	state := engine.AccountState{
		Catalog: engine.Catalog{Ports: []engine.Port{{ID: "a", BankingRegion: "north"}, {ID: "b", BankingRegion: "south"}}, ShipClasses: []engine.Hull{{ID: "hoy", HoldCapacityTons: 100, BaseSpeedCentiknots: 600, CrewRequired: 10}}},
		Ships:   []engine.ShipState{ship}, HomePortID: "a",
	}
	state.Clock.Calendar.TimeScale = 60
	fleet := NewFleet(state, at, 0, map[string]SpeedFactor{"hoy": {Empty: 1, Laden: 1}})
	arrival, err := fleet.Depart("one", engine.Route{FromPort: "a", ToPort: "b", TotalDistanceNM: 240, SafeProvisionsTons: 1}, at, 60)
	if err != nil || arrival != at.Add(40*time.Minute) {
		t.Fatalf("departure ETA: %s %v", arrival, err)
	}
	arrivals := fleet.Advance(at.Add(45*time.Minute), 45)
	if len(arrivals) != 1 || arrivals[0].At != arrival || arrivals[0].PortID != "b" || fleet.Ship("one").Voyage != nil {
		t.Fatalf("arrival waited for the next observed frame: %+v", arrivals)
	}
	wantStores := 10 - StoresBurn(10, 40, true) - StoresBurn(10, 5, false)
	if got := fleet.Ship("one").Stores.ProvisionsTons; math.Abs(got-wantStores) > 1e-9 {
		t.Fatalf("state-dependent stores burn: got %g want %g", got, wantStores)
	}
	bills := fleet.PendingBills()
	if len(bills) != 1 || bills[0].Reason != "upkeep" || bills[0].Region != "north" || bills[0].At != at.Add(24*time.Minute) || bills[0].AmountPence != engine.DailyUpkeep(state.Ships, state.Catalog, 0) {
		t.Fatalf("upkeep boundary/home-region bill: %+v", bills)
	}
	fleet.Advance(at.Add(45*time.Minute), 45)
	if repeated := fleet.PendingBills(); len(repeated) != 0 {
		t.Fatalf("same game day charged upkeep twice: %+v", repeated)
	}
}

func TestFleetDestinationBerthBillIsClaimableBeforeSettlement(t *testing.T) {
	at := time.Date(2026, 10, 20, 0, 0, 0, 0, time.UTC)
	ship := engine.ShipState{}
	ship.Ship.ID, ship.Ship.CurrentPortID, ship.Ship.Status = "one", "a", "berthed"
	fleet := NewFleet(engine.AccountState{Ships: []engine.ShipState{ship}, Ports: map[string]engine.PortDetail{"a": {BerthDuesPerDayPence: 2400}}, Catalog: engine.Catalog{Ports: []engine.Port{{ID: "a", BankingRegion: "north"}}}}, at, 0, nil)
	fleet.Advance(at.Add(2*time.Minute), 2)
	if err := fleet.Offshore("one", at.Add(2*time.Minute)); err != nil {
		t.Fatal(err)
	}
	bills := fleet.PendingBills()
	if len(bills) != 1 || bills[0].AmountPence != 600 || bills[0].Reason != "berth" {
		t.Fatalf("six-hour berth minimum at closeout: %+v", bills)
	}
	if err := fleet.Offshore("one", at.Add(2*time.Minute)); err != nil {
		t.Fatal(err)
	}
	if len(fleet.PendingBills()) != 0 {
		t.Fatal("offshore closeout charged twice")
	}
}

func TestFleetUsesLaterObservedDuesWithoutReplacingItsShips(t *testing.T) {
	at := time.Date(2026, 10, 20, 0, 0, 0, 0, time.UTC)
	ship := engine.ShipState{}
	ship.Ship.ID, ship.Ship.CurrentPortID, ship.Ship.Status = "one", "a", "berthed"
	catalog := engine.Catalog{Ports: []engine.Port{{ID: "a", BankingRegion: "north"}}}
	fleet := NewFleet(engine.AccountState{Ships: []engine.ShipState{ship}, Catalog: catalog}, at, 0, nil)
	fleet.UpdateContext(catalog, map[string]engine.PortDetail{"a": {BerthDuesPerDayPence: 4800}})
	fleet.UpdateContext(engine.Catalog{}, nil)
	fleet.Advance(at.Add(2*time.Minute), 2)
	if err := fleet.Offshore("one", at.Add(2*time.Minute)); err != nil {
		t.Fatal(err)
	}
	bills := fleet.PendingBills()
	if len(bills) != 1 || bills[0].AmountPence != 1200 || bills[0].Region != "north" || len(fleet.Ships()) != 1 {
		t.Fatalf("later context lost the observed dues, banking region or fixed fleet: %+v", bills)
	}
}
