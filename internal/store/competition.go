package store

import (
	"sort"
	"time"

	"github.com/DevYukine/go-tradewinds/internal/gameapi"
)

const CompetitionWindow = 24 * time.Hour

// CompetitionPortObservation is a recorded port response, or a catalog port
// without a response. Nil public metrics mean unavailable, not zero.
type CompetitionPortObservation struct {
	ID                  string
	Name                string
	Region              string
	ObservedAt          time.Time
	QuayVisible         bool
	Stale               bool
	PlayerVolumeTons    *int64
	PlayerTurnoverPence *int64
	OccupiedBerths      *int64
	EffectiveBerths     *int64
	QueuedShips         *int64
	Sailings            []gameapi.SailingNotice
}

type CompetitionInput struct {
	AccountID            string
	At                   time.Time
	OwnCompanies         []string
	OwnCompaniesComplete bool
	Ports                []CompetitionPortObservation
	Sailings             []gameapi.SailingNotice
	EventsTruncated      bool
}

type CompetitionSummary struct {
	AccountID            string              `json:"account_id"`
	GeneratedAt          time.Time           `json:"generated_at"`
	From                 time.Time           `json:"from"`
	To                   time.Time           `json:"to"`
	IdentityBasis        string              `json:"identity_basis"`
	OwnCompaniesComplete bool                `json:"own_companies_complete"`
	EventsTruncated      bool                `json:"events_truncated"`
	Regions              []CompetitionRegion `json:"regions"`
}

type CompetitionActivity struct {
	ForeignCompanies *int                 `json:"foreign_companies"`
	ForeignShips     *int                 `json:"foreign_ships"`
	Sightings        int                  `json:"sightings"`
	LastSeen         *time.Time           `json:"last_seen"`
	Coverage         string               `json:"coverage"`
	TopCompetitors   []CompetitionCompany `json:"top_competitors"`
}

type CompetitionRegion struct {
	CompetitionActivity
	ID            string            `json:"id"`
	ObservedPorts int               `json:"observed_ports"`
	Ports         []CompetitionPort `json:"ports"`
}

type CompetitionPort struct {
	CompetitionActivity
	ID                  string            `json:"id"`
	Name                string            `json:"name"`
	ObservedAt          *time.Time        `json:"observed_at"`
	QuayObservedAt      *time.Time        `json:"quay_observed_at"`
	SourceStale         bool              `json:"source_stale"`
	PlayerVolumeTons    *int64            `json:"player_volume_tons"`
	PlayerTurnoverPence *int64            `json:"player_turnover_pence"`
	OccupiedBerths      *int64            `json:"occupied_berths"`
	EffectiveBerths     *int64            `json:"effective_berths"`
	QueuedShips         *int64            `json:"queued_ships"`
	Ships               []CompetitionShip `json:"ships"`
}

type CompetitionCompany struct {
	Name      string    `json:"name"`
	Ships     int       `json:"ships"`
	Sightings int       `json:"sightings"`
	LastSeen  time.Time `json:"last_seen"`
}

type CompetitionShip struct {
	CompanyName         string    `json:"company_name"`
	ShipName            string    `json:"ship_name"`
	ShipClass           string    `json:"ship_class"`
	LastSeen            time.Time `json:"last_seen"`
	Direction           string    `json:"direction"`
	ReportedOtherPortID *string   `json:"reported_other_port_id"`
}

type competitionIdentity struct {
	company string
	ship    string
}

type competitionAccumulator struct {
	observed  bool
	sightings int
	ships     map[competitionIdentity]gameapi.SailingNotice
	companies map[string]int
	lastSeen  *time.Time
}

func newCompetitionAccumulator() *competitionAccumulator {
	return &competitionAccumulator{
		ships:     make(map[competitionIdentity]gameapi.SailingNotice),
		companies: make(map[string]int),
	}
}

// AggregateCompetition unions direct sightings by company and ship name, never
// by a reported route endpoint. Coverage is always partial: neither a quay
// sample nor a retained event proves continuous visibility for 24 real hours.
func AggregateCompetition(input CompetitionInput) CompetitionSummary {
	at := input.At.UTC()
	result := CompetitionSummary{
		AccountID: input.AccountID, GeneratedAt: at, From: at.Add(-CompetitionWindow), To: at,
		IdentityBasis: "company_and_ship_name", OwnCompaniesComplete: input.OwnCompaniesComplete,
		EventsTruncated: input.EventsTruncated, Regions: make([]CompetitionRegion, 0),
	}
	own := make(map[string]bool, len(input.OwnCompanies))
	for _, name := range input.OwnCompanies {
		own[name] = true
	}
	ports := make(map[string]*competitionAccumulator, len(input.Ports))
	regions := make(map[string]*competitionAccumulator)
	portRegions := make(map[string]string, len(input.Ports))
	for _, port := range input.Ports {
		ports[port.ID] = newCompetitionAccumulator()
		ports[port.ID].observed = port.QuayVisible && !port.ObservedAt.Before(result.From) && !port.ObservedAt.After(at)
		portRegions[port.ID] = port.Region
		if regions[port.Region] == nil {
			regions[port.Region] = newCompetitionAccumulator()
		}
	}
	type noticeKey struct {
		port, company, ship, direction string
		at                             time.Time
	}
	seen := make(map[noticeKey]bool)
	add := func(notice gameapi.SailingNotice) {
		port := ports[notice.PortID]
		if port == nil || notice.SeenAt.Before(result.From) || notice.SeenAt.After(at) || notice.CompanyName == "" || notice.ShipName == "" {
			return
		}
		port.observed = true
		if !input.OwnCompaniesComplete || own[notice.CompanyName] {
			return
		}
		key := noticeKey{notice.PortID, notice.CompanyName, notice.ShipName, notice.Direction, notice.SeenAt.UTC()}
		if seen[key] {
			return
		}
		seen[key] = true
		port.add(notice)
		regions[portRegions[notice.PortID]].add(notice)
	}
	for _, notice := range input.Sailings {
		add(notice)
	}
	for _, port := range input.Ports {
		for _, notice := range port.Sailings {
			add(notice)
		}
	}

	grouped := make(map[string][]CompetitionPort, len(regions))
	for _, observation := range input.Ports {
		accumulator := ports[observation.ID]
		port := CompetitionPort{
			CompetitionActivity: accumulator.activity(input.OwnCompaniesComplete),
			ID:                  observation.ID, Name: observation.Name, SourceStale: observation.Stale,
			PlayerVolumeTons: observation.PlayerVolumeTons, PlayerTurnoverPence: observation.PlayerTurnoverPence,
			OccupiedBerths: observation.OccupiedBerths, EffectiveBerths: observation.EffectiveBerths,
			QueuedShips: observation.QueuedShips, Ships: make([]CompetitionShip, 0, len(accumulator.ships)),
		}
		if !observation.ObservedAt.IsZero() {
			observedAt := observation.ObservedAt
			port.ObservedAt = &observedAt
			if observation.QuayVisible {
				port.QuayObservedAt = &observedAt
			}
		}
		for _, notice := range accumulator.ships {
			port.Ships = append(port.Ships, CompetitionShip{
				CompanyName: notice.CompanyName, ShipName: notice.ShipName, ShipClass: notice.ShipClass,
				LastSeen: notice.SeenAt, Direction: notice.Direction, ReportedOtherPortID: notice.OtherPortID,
			})
		}
		sort.Slice(port.Ships, func(i, j int) bool {
			if port.Ships[i].CompanyName != port.Ships[j].CompanyName {
				return port.Ships[i].CompanyName < port.Ships[j].CompanyName
			}
			return port.Ships[i].ShipName < port.Ships[j].ShipName
		})
		grouped[observation.Region] = append(grouped[observation.Region], port)
	}
	for id, region := range regions {
		regionalPorts := grouped[id]
		sort.Slice(regionalPorts, func(i, j int) bool { return regionalPorts[i].Name < regionalPorts[j].Name })
		observed := 0
		for _, port := range regionalPorts {
			if port.Coverage == "partial" {
				observed++
			}
		}
		region.observed = observed > 0
		result.Regions = append(result.Regions, CompetitionRegion{
			CompetitionActivity: region.activity(input.OwnCompaniesComplete),
			ID:                  id, ObservedPorts: observed, Ports: regionalPorts,
		})
	}
	sort.Slice(result.Regions, func(i, j int) bool { return result.Regions[i].ID < result.Regions[j].ID })
	return result
}

func (a *competitionAccumulator) add(notice gameapi.SailingNotice) {
	a.observed = true
	a.sightings++
	a.companies[notice.CompanyName]++
	identity := competitionIdentity{notice.CompanyName, notice.ShipName}
	if previous, exists := a.ships[identity]; !exists || notice.SeenAt.After(previous.SeenAt) {
		a.ships[identity] = notice
	}
	if a.lastSeen == nil || notice.SeenAt.After(*a.lastSeen) {
		at := notice.SeenAt
		a.lastSeen = &at
	}
}

func (a *competitionAccumulator) activity(exclusionsComplete bool) CompetitionActivity {
	result := CompetitionActivity{Coverage: "not_observed", TopCompetitors: make([]CompetitionCompany, 0)}
	if !a.observed || !exclusionsComplete {
		return result
	}
	companies, ships := len(a.companies), len(a.ships)
	result.ForeignCompanies, result.ForeignShips = &companies, &ships
	result.Coverage, result.Sightings, result.LastSeen = "partial", a.sightings, a.lastSeen
	byCompany := make(map[string]CompetitionCompany, companies)
	for identity, notice := range a.ships {
		company := byCompany[identity.company]
		company.Name = identity.company
		company.Ships++
		company.Sightings = a.companies[identity.company]
		if notice.SeenAt.After(company.LastSeen) {
			company.LastSeen = notice.SeenAt
		}
		byCompany[identity.company] = company
	}
	for _, company := range byCompany {
		result.TopCompetitors = append(result.TopCompetitors, company)
	}
	sort.Slice(result.TopCompetitors, func(i, j int) bool {
		left, right := result.TopCompetitors[i], result.TopCompetitors[j]
		if left.Ships != right.Ships {
			return left.Ships > right.Ships
		}
		if left.Sightings != right.Sightings {
			return left.Sightings > right.Sightings
		}
		return left.Name < right.Name
	})
	if len(result.TopCompetitors) > 3 {
		result.TopCompetitors = result.TopCompetitors[:3]
	}
	return result
}
