package sim

import (
	"errors"
	"fmt"
	"math"
	"sort"
	"time"

	"github.com/DevYukine/go-tradewinds/internal/engine"
)

type SpeedFactor struct {
	Empty float64 `json:"empty"`
	Laden float64 `json:"laden"`
	N     int     `json:"n"`
}

func (s SpeedFactor) AtLoad(cargo, capacity int64) float64 {
	if s.Empty <= 0 || s.Laden <= 0 {
		s.Empty, s.Laden = 1, 0.8
	}
	fraction := 0.0
	if capacity > 0 {
		fraction = math.Max(0, math.Min(1, float64(cargo)/float64(capacity)))
	}
	return s.Empty + (s.Laden-s.Empty)*fraction
}

func VoyageGameHours(route engine.Route, speedCentiknots int64, factor float64) (float64, error) {
	if speedCentiknots <= 0 || factor <= 0 || math.IsNaN(factor) || math.IsInf(factor, 0) {
		return 0, errors.New("invalid voyage speed")
	}
	distance := route.TotalDistanceNM
	if len(route.Legs) != 0 {
		distance = 0
		for _, leg := range route.Legs {
			if leg.DistanceNM < 0 {
				return 0, errors.New("negative route distance")
			}
			distance += leg.DistanceNM
		}
	}
	if distance <= 0 {
		return 0, errors.New("route has no recorded distance")
	}
	return float64(distance) * 100 / float64(speedCentiknots) / factor, nil
}

func PortGameHours(tons int64, hub bool) float64 {
	minutesPerTon := 2.0
	if hub {
		minutesPerTon = 1
	}
	return (30 + float64(max(0, tons))*minutesPerTon) / 60
}

func BerthDues(stayGameHours float64, perDay int64) int64 {
	if perDay <= 0 {
		return 0
	}
	return int64(math.Ceil(math.Max(6, stayGameHours) * float64(perDay) / 24))
}

func StoresBurn(crew int64, gameHours float64, sailing bool) float64 {
	denominator := 200.0
	if sailing {
		denominator = 100
	}
	return float64(max(0, crew)) / denominator * math.Max(0, gameHours) / 24
}

type Arrival struct {
	ShipID string
	PortID string
	At     time.Time
}

type fleetShip struct {
	state       engine.ShipState
	arrival     time.Time
	destination string
	berthHour   float64
	berthAt     time.Time
}

type Fleet struct {
	ships        map[string]*fleetShip
	catalog      engine.Catalog
	ports        map[string]engine.PortDetail
	speeds       map[string]SpeedFactor
	at           time.Time
	hour         float64
	homeRegion   string
	factors      int
	bills        []Bill
	billSequence int64
}

func NewFleet(state engine.AccountState, at time.Time, gameHour float64, speeds map[string]SpeedFactor) *Fleet {
	f := &Fleet{ships: make(map[string]*fleetShip), catalog: state.Catalog, ports: cloneMap(state.Ports), speeds: cloneMap(speeds), at: at, hour: gameHour, factors: len(state.Factors)}
	for _, port := range state.Catalog.Ports {
		if port.ID == state.HomePortID {
			f.homeRegion = port.BankingRegion
		}
	}
	for _, ship := range state.Ships {
		if ship.Ship.Status == "sunk" {
			continue
		}
		ship.CargoManifest = cloneMap(ship.CargoManifest)
		member := &fleetShip{state: ship, berthHour: gameHour, berthAt: at}
		if ship.Ship.BerthedAt != nil {
			member.berthAt = *ship.Ship.BerthedAt
			scale := float64(max(1, state.Clock.Calendar.TimeScale))
			member.berthHour -= math.Max(0, at.Sub(member.berthAt).Hours()) * scale
		}
		if ship.Voyage != nil {
			member.arrival = ship.Voyage.EstimatedArrivalAt
			if len(ship.RouteLegs) > 0 {
				member.destination = ship.RouteLegs[len(ship.RouteLegs)-1].ToNode
			}
		}
		f.ships[ship.Ship.ID] = member
	}
	return f
}

// UpdateContext consumes newly observed operating prices without replacing the
// variant's ships or the fixed initial factor and home-region assumptions.
func (f *Fleet) UpdateContext(catalog engine.Catalog, ports map[string]engine.PortDetail) {
	if len(catalog.Ports) != 0 {
		f.catalog = catalog
	}
	for port, detail := range ports {
		f.ports[port] = detail
	}
}

func (f *Fleet) Ship(id string) *engine.ShipState {
	if member := f.ships[id]; member != nil {
		return &member.state
	}
	return nil
}

func (f *Fleet) Ships() []engine.ShipState {
	ids := make([]string, 0, len(f.ships))
	for id := range f.ships {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	ships := make([]engine.ShipState, 0, len(ids))
	for _, id := range ids {
		ship := f.ships[id].state
		ship.CargoManifest = cloneMap(ship.CargoManifest)
		ships = append(ships, ship)
	}
	return ships
}

func (f *Fleet) hull(id string) engine.Hull {
	for _, hull := range f.catalog.ShipClasses {
		if hull.ID == id {
			return hull
		}
	}
	return engine.Hull{}
}

func (f *Fleet) Depart(shipID string, route engine.Route, at time.Time, scale int64) (time.Time, error) {
	member := f.ships[shipID]
	if member == nil || member.state.Voyage != nil || scale <= 0 || member.state.Ship.CurrentPortID != route.FromPort {
		return time.Time{}, errors.New("ship cannot depart on this recorded route")
	}
	if member.state.Ship.TurnaroundUnlocksAt != nil && at.Before(*member.state.Ship.TurnaroundUnlocksAt) {
		return time.Time{}, errors.New("ship is still in port turnaround")
	}
	hull := f.hull(member.state.Ship.ShipClass)
	factor := f.speeds[hull.ID].AtLoad(member.state.CargoUsedTons, member.state.EffectiveHoldCapacityTons)
	hours, err := VoyageGameHours(route, hull.BaseSpeedCentiknots, factor)
	if err != nil {
		return time.Time{}, err
	}
	if member.state.Stores.ProvisionsTons+1e-9 < route.SafeProvisionsTons {
		return time.Time{}, errors.New("safe sailing stores are not aboard")
	}
	arrival := at.Add(time.Duration(hours / float64(scale) * float64(time.Hour)))
	port := member.state.Ship.CurrentPortID
	dues := BerthDues(f.hour-member.berthHour, f.ports[port].BerthDuesPerDayPence)
	if dues > 0 && member.state.Ship.Status == "berthed" {
		f.addBill(at, port, "berth", dues)
	}
	member.arrival, member.destination = arrival, route.ToPort
	member.state.Ship.CurrentPortID = ""
	member.state.Ship.Status = "sailing"
	member.state.RouteLegs = append([]engine.RouteLeg(nil), route.Legs...)
	member.state.Voyage = &struct {
		EstimatedArrivalAt time.Time `json:"estimated_arrival_at"`
	}{arrival}
	return arrival, nil
}

func (f *Fleet) addBill(at time.Time, port, reason string, amount int64) {
	region := f.homeRegion
	for _, entry := range f.catalog.Ports {
		if entry.ID == port {
			region = entry.BankingRegion
		}
	}
	f.billSequence++
	f.bills = append(f.bills, Bill{ID: fmt.Sprintf("sim-bill-%d", f.billSequence), Region: region, Reason: reason, At: at, AmountPence: amount})
}

func (f *Fleet) Advance(at time.Time, gameHour float64) []Arrival {
	if at.Before(f.at) || gameHour < f.hour {
		return nil
	}
	var arrivals []Arrival
	duration := at.Sub(f.at)
	for _, ship := range f.Ships() {
		member := f.ships[ship.Ship.ID]
		hull := f.hull(ship.Ship.ShipClass)
		sailingHours := 0.0
		if ship.Voyage != nil {
			sailingHours = gameHour - f.hour
			if !member.arrival.After(at) && duration > 0 {
				fraction := math.Max(0, math.Min(1, float64(member.arrival.Sub(f.at))/float64(duration)))
				sailingHours *= fraction
			}
		}
		portHours := gameHour - f.hour - sailingHours
		member.state.Stores.ProvisionsTons = math.Max(0, member.state.Stores.ProvisionsTons-StoresBurn(hull.CrewRequired, sailingHours, true)-StoresBurn(hull.CrewRequired, portHours, false))
		if ship.Voyage == nil || member.arrival.After(at) || member.destination == "" {
			continue
		}
		arrivalHour := gameHour
		if duration > 0 {
			arrivalHour = f.hour + (gameHour-f.hour)*float64(member.arrival.Sub(f.at))/float64(duration)
		}
		member.state.Voyage = nil
		member.state.Ship.Status = "berthed"
		member.state.Ship.CurrentPortID = member.destination
		member.berthAt, member.berthHour = member.arrival, arrivalHour
		member.state.Ship.BerthedAt = &member.berthAt
		scale := 60.0
		if duration > 0 && gameHour > f.hour {
			scale = (gameHour - f.hour) / duration.Hours()
		}
		unlock := member.arrival.Add(time.Duration(0.5 / scale * float64(time.Hour)))
		member.state.Ship.TurnaroundUnlocksAt = &unlock
		arrivals = append(arrivals, Arrival{ShipID: ship.Ship.ID, PortID: member.destination, At: member.arrival})
	}
	for day := int64(math.Floor(f.hour/24)) + 1; float64(day)*24 <= gameHour; day++ {
		billAt := at
		if gameHour > f.hour {
			billAt = f.at.Add(time.Duration(float64(duration) * (float64(day)*24 - f.hour) / (gameHour - f.hour)))
		}
		f.addBill(billAt, "", "upkeep", engine.DailyUpkeep(f.Ships(), f.catalog, f.factors))
	}
	f.at, f.hour = at, gameHour
	sort.Slice(arrivals, func(i, j int) bool {
		if arrivals[i].At.Equal(arrivals[j].At) {
			return arrivals[i].ShipID < arrivals[j].ShipID
		}
		return arrivals[i].At.Before(arrivals[j].At)
	})
	return arrivals
}

func (f *Fleet) Handle(shipID string, tons int64, hub bool, at time.Time, scale int64) error {
	ship := f.Ship(shipID)
	if ship == nil || ship.Voyage != nil || scale <= 0 {
		return errors.New("invalid port handling")
	}
	unlock := at.Add(time.Duration(PortGameHours(tons, hub) / float64(scale) * float64(time.Hour)))
	if ship.Ship.TurnaroundUnlocksAt != nil && ship.Ship.TurnaroundUnlocksAt.After(unlock) {
		unlock = *ship.Ship.TurnaroundUnlocksAt
	}
	ship.Ship.TurnaroundUnlocksAt = &unlock
	return nil
}

func (f *Fleet) Offshore(shipID string, at time.Time) error {
	member := f.ships[shipID]
	if member == nil || member.state.Voyage != nil {
		return errors.New("ship cannot close its berth stay")
	}
	if member.state.Ship.Status == "berthed" {
		port := member.state.Ship.CurrentPortID
		dues := BerthDues(f.hour-member.berthHour, f.ports[port].BerthDuesPerDayPence)
		if dues > 0 {
			f.addBill(at, port, "berth", dues)
		}
		member.state.Ship.Status = "anchored_roadstead"
	}
	return nil
}

func (f *Fleet) PendingBills() []Bill {
	bills := f.bills
	f.bills = nil
	return bills
}
