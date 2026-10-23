// Package engine plans conservative voyages and serializes each account's actions.
package engine

import (
	"encoding/json"
	"time"
)

// The planner projects only documented economic fields. Snapshots retain the full
// upstream response, including fields not needed by the planner.
type Catalog struct {
	Ports       []Port `json:"ports"`
	ShipClasses []Hull `json:"ship_classes"`
	SeaZones    []Zone `json:"sea_zones"`
}

type Port struct {
	ID            string   `json:"id"`
	BankingRegion string   `json:"banking_region"`
	SeaZoneID     string   `json:"sea_zone_id"`
	Phase         int      `json:"phase"`
	IsHub         bool     `json:"is_hub"`
	HasShipyard   bool     `json:"has_shipyard"`
	Produces      []string `json:"produces"`
	Manufactures  []string `json:"manufactures"`
	Imports       []string `json:"imports"`
}

type Hull struct {
	ID                  string `json:"id"`
	HullCategory        string `json:"hull_category"`
	Phase               int    `json:"phase"`
	BaseSpeedCentiknots int64  `json:"base_speed_centiknots"`
	HoldCapacityTons    int64  `json:"hold_capacity_tons"`
	MaxCannons          int64  `json:"max_cannons"`
	CrewRequired        int64  `json:"crew_required"`
}

type Zone struct {
	ID            string `json:"id"`
	ZoneClass     string `json:"zone_class"`
	BaseStormBPS  int64  `json:"base_storm_bps"`
	BasePiracyBPS int64  `json:"base_piracy_bps"`
}

type ZoneObservation struct {
	SeaZoneID    string `json:"sea_zone_id"`
	NetPiracyBPS int64  `json:"net_piracy_bps"`
}

type Clock struct {
	RealUTC       time.Time `json:"real_utc"`
	UnlockedPhase int       `json:"unlocked_phase"`
	Paused        bool      `json:"paused"`
	Calendar      struct {
		Month          int   `json:"month"`
		Day            int   `json:"day"`
		TimeScale      int64 `json:"time_scale"`
		TotalGameHours int64 `json:"total_game_hours"`
	} `json:"calendar"`
}

type ShipState struct {
	Ship struct {
		ID                  string     `json:"id"`
		Name                string     `json:"name"`
		ShipClass           string     `json:"ship_class"`
		Status              string     `json:"status"`
		CurrentPortID       string     `json:"current_port_id"`
		CannonsCount        int64      `json:"cannons_count"`
		ConvoyID            string     `json:"convoy_id"`
		TurnaroundUnlocksAt *time.Time `json:"turnaround_unlocks_at"`
		DockQueuedAt        *time.Time `json:"dock_queued_at"`
		BerthedAt           *time.Time `json:"berthed_at"`
	} `json:"ship"`
	HoldCapacityTons          int64            `json:"hold_capacity_tons"`
	EffectiveHoldCapacityTons int64            `json:"effective_hold_capacity_tons"`
	CargoUsedTons             int64            `json:"cargo_used_tons"`
	CargoManifest             map[string]int64 `json:"cargo_manifest"`
	Stores                    struct {
		ProvisionsTons float64 `json:"provisions_tons"`
	} `json:"stores"`
	RouteLegs []RouteLeg `json:"route_legs"`
	Voyage    *struct {
		EstimatedArrivalAt time.Time `json:"estimated_arrival_at"`
	} `json:"voyage"`
}

type Route struct {
	FromPort                 string     `json:"from_port"`
	ToPort                   string     `json:"to_port"`
	ShipClass                string     `json:"ship_class"`
	TotalDistanceNM          int64      `json:"total_distance_nm"`
	SafeProvisionsTons       float64    `json:"safe_provisions_tons"`
	PortProvisionsTonsPerDay float64    `json:"port_provisions_tons_per_day"`
	Legs                     []RouteLeg `json:"legs"`
}

type RouteLeg struct {
	FromNode    string `json:"from_node"`
	ToNode      string `json:"to_node"`
	DistanceNM  int64  `json:"distance_nm"`
	EdgeClass   string `json:"edge_class"`
	IsSoundToll bool   `json:"is_sound_toll"`
}

type Level struct {
	OrderID           string `json:"order_id"`
	PricePence        int64  `json:"price_pence"`
	RemainingQuantity int64  `json:"remaining_quantity"`
}

type CommodityBook struct {
	CommodityID string `json:"commodity_id"`
	Depth       *struct {
		Bids []Level `json:"bids"`
		Asks []Level `json:"asks"`
	} `json:"depth"`
}

type Market struct {
	PortID     string          `json:"port_id"`
	Visibility string          `json:"visibility"`
	CustomsBPS int64           `json:"customs_bps"`
	AsOf       *time.Time      `json:"as_of"`
	Books      []CommodityBook `json:"books"`
}

type Gossip struct {
	SubjectPortID string    `json:"subject_port_id"`
	SnapshotAt    time.Time `json:"snapshot_at"`
	AgeSeconds    int64     `json:"age_seconds"`
	Commodities   []struct {
		CommodityID  string    `json:"commodity_id"`
		SnapshotAt   time.Time `json:"snapshot_at"`
		BestBidPence *int64    `json:"best_bid_pence"`
	} `json:"commodities"`
}

type PortDetail struct {
	ID                   string   `json:"id"`
	BerthDuesPerDayPence int64    `json:"berth_dues_per_day_pence"`
	OccupiedBerths       int64    `json:"occupied_berths"`
	EffectiveBerths      int64    `json:"effective_berths"`
	QueuedShips          int64    `json:"queued_ships"`
	CoffeeHouseGossip    []Gossip `json:"coffee_house_gossip"`
}

type Contract struct {
	ID                string          `json:"id"`
	ContractType      string          `json:"contract_type"`
	OriginPortID      string          `json:"origin_port_id"`
	DestPortID        string          `json:"dest_port_id"`
	CommodityID       string          `json:"commodity_id"`
	QuantityRequired  int64           `json:"quantity_required"`
	QuantityDelivered int64           `json:"quantity_delivered"`
	RemainingQuantity int64           `json:"remaining_quantity"`
	RewardPence       int64           `json:"reward_pence"`
	RewardPerTonPence *int64          `json:"reward_per_ton_pence"`
	CollateralPence   int64           `json:"collateral_pence"`
	Status            string          `json:"status"`
	IsPublic          bool            `json:"is_public"`
	Stale             bool            `json:"stale"`
	IssuerCompanyID   string          `json:"issuer_company_id"`
	CarrierCompanyID  string          `json:"carrier_company_id"`
	CarrierShipID     string          `json:"carrier_ship_id"`
	ExpiresAt         *time.Time      `json:"expires_at"`
	Metadata          json.RawMessage `json:"metadata"`
}

type Vault struct {
	AccountID     string `json:"account_id"`
	BankingRegion string `json:"banking_region"`
	AccountType   string `json:"account_type"`
	BalancePence  int64  `json:"balance_pence"`
}

type Rate struct {
	FromRegion string `json:"from_region"`
	ToRegion   string `json:"to_region"`
	FeeBPS     int64  `json:"fee_bps"`
}

type YardQuote struct {
	PortID         string `json:"port_id"`
	ShipClass      string `json:"ship_class"`
	BuildMode      string `json:"build_mode"`
	Available      bool   `json:"available"`
	TotalCostPence *int64 `json:"total_cost_pence"`
}

type DatasetMeta struct {
	UpdatedAt    time.Time  `json:"updated_at"`
	AttemptedAt  time.Time  `json:"attempted_at"`
	Generation   string     `json:"generation"`
	Complete     bool       `json:"complete"`
	UpstreamAsOf *time.Time `json:"upstream_as_of,omitempty"`
	Error        string     `json:"error"`
	Stale        bool       `json:"stale"`
}

func routeKey(from, to, hull string) string {
	return from + "|" + to + "|" + hull
}

func bookCommodity(book Market, commodity string) *CommodityBook {
	for i := range book.Books {
		if book.Books[i].CommodityID == commodity {
			return &book.Books[i]
		}
	}
	return nil
}

func findHull(catalog Catalog, id string) (Hull, bool) {
	for _, hull := range catalog.ShipClasses {
		if hull.ID == id {
			return hull, true
		}
	}
	return Hull{}, false
}

func findPort(catalog Catalog, id string) (Port, bool) {
	for _, port := range catalog.Ports {
		if port.ID == id {
			return port, true
		}
	}
	return Port{}, false
}
