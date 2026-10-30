package gameapi

import (
	"encoding/json"
	"errors"
	"time"
)

// Catalog is the current /v1/world/catalog response. Quantities are tons,
// money is integer pence, and speeds are hundredths of a knot.
type Catalog struct {
	Commodities []Commodity `json:"commodities"`
	ShipClasses []ShipClass `json:"ship_classes"`
	SeaZones    []Zone      `json:"sea_zones"`
	Ports       []Port      `json:"ports"`
}

type Commodity struct {
	ID             string        `json:"id"`
	Name           string        `json:"name"`
	Category       string        `json:"category"`
	BasePricePence int64         `json:"base_price_pence"`
	WorkshopInputs []RecipeInput `json:"workshop_inputs"`
}

type RecipeInput struct {
	CommodityID   string `json:"commodity_id"`
	TenthsPerUnit int32  `json:"tenths_per_unit"`
}

type Port struct {
	ID                string        `json:"id"`
	Name              string        `json:"name"`
	BankingRegion     string        `json:"banking_region"`
	SeaZoneID         string        `json:"sea_zone_id"`
	Phase             int32         `json:"phase"`
	IsHub             bool          `json:"is_hub"`
	HasShipyard       bool          `json:"has_shipyard"`
	YardDiscount      *YardDiscount `json:"yard_discount"`
	CustomsBPS        int64         `json:"customs_bps"`
	BaseBerths        int32         `json:"base_berths"`
	EffectiveBerths   int32         `json:"effective_berths"`
	TotalWarehouseCap int32         `json:"total_warehouse_cap"`
	Produces          []string      `json:"produces"`
	Manufactures      []string      `json:"manufactures"`
	Imports           []string      `json:"imports"`
}

type ShipClass struct {
	ID                  string                    `json:"id"`
	Name                string                    `json:"name"`
	HullCategory        string                    `json:"hull_category"`
	Phase               int32                     `json:"phase"`
	BaseSpeedCentiknots int32                     `json:"base_speed_centiknots"`
	HoldCapacityTons    int32                     `json:"hold_capacity_tons"`
	PassengersPerTon    int32                     `json:"passengers_per_ton"`
	MaxCannons          int32                     `json:"max_cannons"`
	CrewRequired        int32                     `json:"crew_required"`
	BaseLaborCostPence  int64                     `json:"base_labor_cost_pence"`
	BuildMaterials      []ShipMaterialRequirement `json:"build_materials"`
	CanSealDispatches   bool                      `json:"can_seal_dispatches"`
}

type YardDiscount struct {
	LabourDiscountBPS   int32 `json:"labour_discount_bps"`
	TurnkeyMarkupWaived bool  `json:"turnkey_markup_waived"`
}

type MortgageTerms struct {
	LenderCompanyID     string `json:"lender_company_id"`
	LenderName          string `json:"lender_name"`
	PrincipalPence      int64  `json:"principal_pence"`
	AnnualRateBPS       int32  `json:"annual_rate_bps"`
	TermGameDays        int32  `json:"term_game_days"`
	Repayment           string `json:"repayment"`
	Installments        int32  `json:"installments"`
	InstallmentPence    *int64 `json:"installment_pence"`
	TotalRepayablePence int64  `json:"total_repayable_pence"`
	Available           bool   `json:"available"`
}

type ShipMaterialRequirement struct {
	CommodityID string `json:"commodity_id"`
	Quantity    int32  `json:"quantity"`
}

// Zone is a catalog sea zone. Live piracy is reported separately by ZonePiracy.
type Zone struct {
	ID            string `json:"id"`
	Name          string `json:"name"`
	ZoneClass     string `json:"zone_class"`
	Phase         int32  `json:"phase"`
	BaseStormBPS  int32  `json:"base_storm_bps"`
	BasePiracyBPS int32  `json:"base_piracy_bps"`
}

type ZonePiracy struct {
	SeaZoneID            string     `json:"sea_zone_id"`
	Name                 string     `json:"name"`
	Phase                int32      `json:"phase"`
	BasePiracyBPS        int32      `json:"base_piracy_bps"`
	MarquePiracyBPS      int32      `json:"marque_piracy_bps"`
	ActiveMarques        int64      `json:"active_marques"`
	PatrolSuppressionBPS int32      `json:"patrol_suppression_bps"`
	ActivePatrols        int64      `json:"active_patrols"`
	NetPiracyBPS         int32      `json:"net_piracy_bps"`
	NextMarqueExpiresAt  *time.Time `json:"next_marque_expires_at"`
	NextPatrolExpiresAt  *time.Time `json:"next_patrol_expires_at"`
}

type Clock struct {
	RealUTC               time.Time  `json:"real_utc"`
	SeasonNumber          int32      `json:"season_number"`
	UnlockedPhase         int32      `json:"unlocked_phase"`
	GlobalHealthBPS       int32      `json:"global_health_bps"`
	ActivePlayerVolume24H int64      `json:"active_player_volume_24h"`
	Paused                bool       `json:"paused"`
	SeasonPlannedEndAt    *time.Time `json:"season_planned_end_at"`
	Calendar              Calendar   `json:"calendar"`
}

type Calendar struct {
	Year           int32  `json:"year"`
	Month          int32  `json:"month"`
	Day            int32  `json:"day"`
	Hour           int32  `json:"hour"`
	Minute         int32  `json:"minute"`
	Season         string `json:"season"`
	TotalGameHours int64  `json:"total_game_hours"`
	TimeScale      int32  `json:"time_scale"`
}

type Ship struct {
	ID                    string     `json:"id"`
	CompanyID             string     `json:"company_id"`
	Name                  string     `json:"name"`
	ShipClass             string     `json:"ship_class"`
	Status                string     `json:"status"`
	CurrentLegIdx         int32      `json:"current_leg_idx"`
	CannonsCount          int32      `json:"cannons_count"`
	StoresProvisionsUnits int64      `json:"stores_provisions_units"`
	CurrentPortID         *string    `json:"current_port_id"`
	TurnaroundUnlocksAt   *time.Time `json:"turnaround_unlocks_at"`
	DockQueuedAt          *time.Time `json:"dock_queued_at"`
	BerthedAt             *time.Time `json:"berthed_at"`
	LegDepartsAt          *time.Time `json:"leg_departs_at"`
	LegArrivesAt          *time.Time `json:"leg_arrives_at"`
	ConvoyID              *string    `json:"convoy_id"`
	CreatedAt             time.Time  `json:"created_at"`
	UpdatedAt             time.Time  `json:"updated_at"`
	DispatchesSealed      bool       `json:"dispatches_sealed"`
}

// ShipDetail is ShipResult in the official schema, including stores and cargo
// separately. Stores consume hold space but cannot be traded as cargo.
type ShipDetail struct {
	Ship                      Ship             `json:"ship"`
	HoldCapacityTons          int32            `json:"hold_capacity_tons"`
	EffectiveHoldCapacityTons int32            `json:"effective_hold_capacity_tons"`
	CargoUsedTons             int32            `json:"cargo_used_tons"`
	Stores                    ShipStores       `json:"stores"`
	CargoManifest             map[string]int32 `json:"cargo_manifest"`
	RouteLegs                 []RouteLeg       `json:"route_legs"`
	Voyage                    *VoyageEstimate  `json:"voyage"`
}

type ShipStores struct {
	ProvisionsTons   float64 `json:"provisions_tons"`
	SeaDaysRemaining float64 `json:"sea_days_remaining"`
}

type RouteLeg struct {
	FromNode    string `json:"from_node"`
	ToNode      string `json:"to_node"`
	DistanceNM  int32  `json:"distance_nm"`
	EdgeClass   string `json:"edge_class"`
	IsSoundToll bool   `json:"is_sound_toll"`
}

type VoyageEstimate struct {
	CurrentLegIdx      int32         `json:"current_leg_idx"`
	SpeedCentiknots    int32         `json:"speed_centiknots"`
	EstimatedArrivalAt time.Time     `json:"estimated_arrival_at"`
	Legs               []LegEstimate `json:"legs"`
}

type LegEstimate struct {
	LegIdx             int32     `json:"leg_idx"`
	FromNode           string    `json:"from_node"`
	ToNode             string    `json:"to_node"`
	DistanceNM         int32     `json:"distance_nm"`
	SpeedCentiknots    int32     `json:"speed_centiknots"`
	EstimatedArrivalAt time.Time `json:"estimated_arrival_at"`
	Starving           bool      `json:"starving"`
}

type Route struct {
	FromPort                 string     `json:"from_port"`
	ToPort                   string     `json:"to_port"`
	ShipClass                string     `json:"ship_class"`
	TotalDistanceNM          int32      `json:"total_distance_nm"`
	ExpectedProvisionsTons   float64    `json:"expected_provisions_tons"`
	SafeProvisionsTons       float64    `json:"safe_provisions_tons"`
	PortProvisionsTonsPerDay float64    `json:"port_provisions_tons_per_day"`
	Legs                     []RouteLeg `json:"legs"`
}

type PassengerWaitingGroup struct {
	ID           string    `json:"id"`
	Kind         string    `json:"kind"`
	Size         int64     `json:"size"`
	OriginPortID string    `json:"origin_port_id"`
	DestPortID   string    `json:"dest_port_id"`
	BoardBy      time.Time `json:"board_by"`
}

type PassengerOffer struct {
	ID               string     `json:"id"`
	ShipID           string     `json:"ship_id"`
	PortID           string     `json:"port_id"`
	DestPortID       string     `json:"dest_port_id"`
	FarePerHeadPence int64      `json:"fare_per_head_pence"`
	MaxBerths        int64      `json:"max_berths"`
	BerthsTaken      int64      `json:"berths_taken"`
	Status           string     `json:"status"`
	CreatedAt        time.Time  `json:"created_at"`
	CloseReason      string     `json:"close_reason"`
	ClosedAt         *time.Time `json:"closed_at"`
}

type PassengerAboardGroup struct {
	ID                string     `json:"id"`
	Kind              string     `json:"kind"`
	Size              int64      `json:"size"`
	OriginPortID      string     `json:"origin_port_id"`
	DestPortID        string     `json:"dest_port_id"`
	FarePerHeadPence  int64      `json:"fare_per_head_pence"`
	FullFarePence     int64      `json:"full_fare_pence"`
	HoldTons          int64      `json:"hold_tons"`
	BoardedAt         *time.Time `json:"boarded_at"`
	PromisedArrivalAt *time.Time `json:"promised_arrival_at"`
}

type PassengerFare struct {
	ID               string    `json:"id"`
	Kind             string    `json:"kind"`
	OriginPortID     string    `json:"origin_port_id"`
	DestPortID       string    `json:"dest_port_id"`
	Size             int64     `json:"size"`
	FarePerHeadPence int64     `json:"fare_per_head_pence"`
	AcceptedAt       time.Time `json:"accepted_at"`
}

type Book struct {
	PortID     string          `json:"port_id"`
	Visibility string          `json:"visibility"`
	CustomsBPS int64           `json:"customs_bps"`
	AsOf       *time.Time      `json:"as_of"`
	Books      []CommodityBook `json:"books"`
}

type CommodityBook struct {
	CommodityID string     `json:"commodity_id"`
	BestBid     *TopLevel  `json:"best_bid"`
	BestAsk     *TopLevel  `json:"best_ask"`
	Depth       *BookDepth `json:"depth"`
}

type TopLevel struct {
	PricePence int64 `json:"price_pence"`
	Quantity   int32 `json:"quantity"`
}

type BookDepth struct {
	NPCCurrentStock int32   `json:"npc_current_stock"`
	Bids            []Level `json:"bids"`
	Asks            []Level `json:"asks"`
}

type Level struct {
	OrderID           string    `json:"order_id"`
	IsNPC             bool      `json:"is_npc"`
	Side              string    `json:"side"`
	PricePence        int64     `json:"price_pence"`
	RemainingQuantity int32     `json:"remaining_quantity"`
	CreatedAt         time.Time `json:"created_at"`
}

// Contract includes ContractListing's additional fields from /v1/contract/list.
// Nullable terms remain pointers so that absent freight/charter fields are not
// confused with zero-valued terms.
type Contract struct {
	ID                string          `json:"id"`
	ContractType      string          `json:"contract_type"`
	OriginPortID      string          `json:"origin_port_id"`
	DestPortID        *string         `json:"dest_port_id"`
	CommodityID       *string         `json:"commodity_id"`
	QuantityRequired  int32           `json:"quantity_required"`
	QuantityDelivered int32           `json:"quantity_delivered"`
	RewardPence       int64           `json:"reward_pence"`
	CollateralPence   int64           `json:"collateral_pence"`
	Status            string          `json:"status"`
	IsPublic          bool            `json:"is_public"`
	IssuerCompanyID   *string         `json:"issuer_company_id"`
	CarrierCompanyID  *string         `json:"carrier_company_id"`
	CarrierShipID     *string         `json:"carrier_ship_id"`
	ExpiresAt         *time.Time      `json:"expires_at"`
	Metadata          json.RawMessage `json:"metadata"`
	CreatedAt         time.Time       `json:"created_at"`
	UpdatedAt         time.Time       `json:"updated_at"`
	RemainingQuantity int32           `json:"remaining_quantity"`
	Stale             bool            `json:"stale"`
	RewardPerTonPence *int64          `json:"reward_per_ton_pence"`
	FundedBy          *string         `json:"funded_by"`
	Title             *string         `json:"title"`
	Gossip            *CharterGossip  `json:"gossip"`
}

type CharterGossip struct {
	HeardAtPortID string    `json:"heard_at_port_id"`
	NewsAsOf      time.Time `json:"news_as_of"`
	AgeSeconds    int64     `json:"age_seconds"`
}

type PortDetail struct {
	ID                         string          `json:"id"`
	Name                       string          `json:"name"`
	BankingRegion              string          `json:"banking_region"`
	SeaZoneID                  string          `json:"sea_zone_id"`
	Phase                      int32           `json:"phase"`
	IsHub                      bool            `json:"is_hub"`
	HasShipyard                bool            `json:"has_shipyard"`
	CustomsBPS                 int64           `json:"customs_bps"`
	BaseCustomsBPS             int64           `json:"base_customs_bps"`
	HealthScoreBPS             int32           `json:"health_score_bps"`
	RollingPlayerVolumeTons    int32           `json:"rolling_player_volume_tons"`
	RollingPlayerTurnoverPence int64           `json:"rolling_player_turnover_pence"`
	BaseBerths                 int32           `json:"base_berths"`
	EffectiveBerths            int32           `json:"effective_berths"`
	OccupiedBerths             int64           `json:"occupied_berths"`
	QueuedShips                int64           `json:"queued_ships"`
	BerthDuesPerDayPence       int64           `json:"berth_dues_per_day_pence"`
	TotalWarehouseCap          int32           `json:"total_warehouse_cap"`
	TotalLeasedWarehouse       int64           `json:"total_leased_warehouse"`
	CountingHouseMinLeaseTons  int32           `json:"counting_house_min_lease_tons"`
	Visibility                 string          `json:"visibility"`
	CoffeeHouseGossip          []PortGossip    `json:"coffee_house_gossip"`
	RecentSailings             []SailingNotice `json:"recent_sailings"`
}

type PortGossip struct {
	SubjectPortID string            `json:"subject_port_id"`
	SnapshotAt    time.Time         `json:"snapshot_at"`
	AgeSeconds    int64             `json:"age_seconds"`
	Commodities   []GossipCommodity `json:"commodities"`
}

type GossipCommodity struct {
	CommodityID           string    `json:"commodity_id"`
	CurrentStock          int32     `json:"current_stock"`
	BestBidPence          *int64    `json:"best_bid_pence"`
	BestAskPence          *int64    `json:"best_ask_pence"`
	SnapshotAt            time.Time `json:"snapshot_at"`
	BestPlayerBidPence    *int64    `json:"best_player_bid_pence"`
	BestPlayerBidQuantity *int64    `json:"best_player_bid_quantity"`
	BestPlayerAskPence    *int64    `json:"best_player_ask_pence"`
	BestPlayerAskQuantity *int64    `json:"best_player_ask_quantity"`
}

type Vault struct {
	AccountID     string `json:"account_id"`
	BankingRegion string `json:"banking_region"`
	AccountType   string `json:"account_type"`
	BalancePence  int64  `json:"balance_pence"`
}

// Leaderboard is the hourly wealth snapshot and its seven-real-day growth board.
type Leaderboard struct {
	SeasonNumber  int32               `json:"season_number"`
	RankedPlayers int64               `json:"ranked_players"`
	Entries       []LeaderboardEntry  `json:"entries"`
	Rising        RisingBoard         `json:"rising"`
	RefreshedAt   *time.Time          `json:"refreshed_at"`
	NextRefreshAt *time.Time          `json:"next_refresh_at"`
	You           *LeaderboardOwnRank `json:"you"`
}

type LeaderboardEntry struct {
	Rank          int32             `json:"rank"`
	CompanyName   string            `json:"company_name"`
	HomePortID    string            `json:"home_port_id"`
	NetWorthPence int64             `json:"net_worth_pence"`
	Titles        []HallOfFameTitle `json:"titles"`
}

type HallOfFameTitle struct {
	SeasonNumber int32  `json:"season_number"`
	PlayerRank   int32  `json:"player_rank"`
	Title        string `json:"title"`
}

type LeaderboardOwnRank struct {
	Rank             int32  `json:"rank"`
	CompanyName      string `json:"company_name"`
	NetWorthPence    int64  `json:"net_worth_pence"`
	IsCallingCompany bool   `json:"is_calling_company"`
}

type RisingBoard struct {
	Entries       []RisingEntry  `json:"entries"`
	RankedPlayers int64          `json:"ranked_players"`
	WindowStartAt *time.Time     `json:"window_start_at"`
	You           *RisingOwnRank `json:"you"`
}

// GrowthBPS and GainedPence are signed: companies can lose wealth.
type RisingEntry struct {
	Rank          int32     `json:"rank"`
	CompanyName   string    `json:"company_name"`
	HomePortID    string    `json:"home_port_id"`
	GrowthBPS     int64     `json:"growth_bps"`
	GainedPence   int64     `json:"gained_pence"`
	WindowStartAt time.Time `json:"window_start_at"`
}

type RisingOwnRank struct {
	Rank             int32     `json:"rank"`
	CompanyName      string    `json:"company_name"`
	GrowthBPS        int64     `json:"growth_bps"`
	GainedPence      int64     `json:"gained_pence"`
	WindowStartAt    time.Time `json:"window_start_at"`
	IsCallingCompany bool      `json:"is_calling_company"`
}

type BankResult struct {
	CompanyID               string           `json:"company_id"`
	Name                    string           `json:"name"`
	HomeRegion              string           `json:"home_region"`
	Status                  string           `json:"status"`
	DemandRateBPS           int32            `json:"demand_rate_bps"`
	NoticeRateBPS           int32            `json:"notice_rate_bps"`
	NoticeGameHours         int32            `json:"notice_game_hours"`
	Insured                 bool             `json:"insured"`
	ExchangeFeeBPS          map[string]int32 `json:"exchange_fee_bps"`
	DepositsPence           int64            `json:"deposits_pence"`
	ReservesPence           int64            `json:"reserves_pence"`
	RequiredReservePence    int64            `json:"required_reserve_pence"`
	LoansOutPence           int64            `json:"loans_out_pence"`
	UnderwritingLockedPence int64            `json:"underwriting_locked_pence"`
	LendingHeadroomPence    int64            `json:"lending_headroom_pence"`
	OverdueWithdrawalsPence int64            `json:"overdue_withdrawals_pence"`
	CharteredAt             time.Time        `json:"chartered_at"`
	FailedAt                *time.Time       `json:"failed_at"`
}

type DepositResult struct {
	ID                     string    `json:"id"`
	BankCompanyID          string    `json:"bank_company_id"`
	BankName               string    `json:"bank_name"`
	DepositorCompanyID     string    `json:"depositor_company_id"`
	BankingRegion          string    `json:"banking_region"`
	BalancePence           int64     `json:"balance_pence"`
	DepositedPence         int64     `json:"deposited_pence"`
	InterestCreditedPence  int64     `json:"interest_credited_pence"`
	WithdrawnPence         int64     `json:"withdrawn_pence"`
	InsuredPaidPence       int64     `json:"insured_paid_pence"`
	WrittenOffPence        int64     `json:"written_off_pence"`
	DailyRateBPS           int32     `json:"daily_rate_bps"`
	NoticeGameHours        int32     `json:"notice_game_hours"`
	PendingWithdrawalPence int64     `json:"pending_withdrawal_pence"`
	PledgedPence           int64     `json:"pledged_pence"`
	SetOffPence            int64     `json:"set_off_pence"`
	ExchangeFeesPence      int64     `json:"exchange_fees_pence"`
	Status                 string    `json:"status"`
	CreatedAt              time.Time `json:"created_at"`
}

type WithdrawRequest struct {
	DepositID   string  `json:"deposit_id"`
	AmountPence *int64  `json:"amount_pence,omitempty"`
	ToRegion    *string `json:"to_region,omitempty"`
}

type CreditReport struct {
	CompanyID               string     `json:"company_id"`
	CompanyName             string     `json:"company_name"`
	ActiveLoans             int64      `json:"active_loans"`
	ActiveOwedPence         int64      `json:"active_owed_pence"`
	DefaultedOpenLoans      int64      `json:"defaulted_open_loans"`
	DefaultedOwedPence      int64      `json:"defaulted_owed_pence"`
	LoansRepaid             int64      `json:"loans_repaid"`
	LoansDefaulted          int64      `json:"loans_defaulted"`
	LoansWrittenOff         int64      `json:"loans_written_off"`
	LoanWrittenOffPence     int64      `json:"loan_written_off_pence"`
	LoansWalkedAway         int64      `json:"loans_walked_away"`
	LoanWalkedAwayPence     int64      `json:"loan_walked_away_pence"`
	FuturesClaimsOpenPence  int64      `json:"futures_claims_open_pence"`
	FuturesClaimsTotal      int64      `json:"futures_claims_total"`
	FuturesClaimsWrittenOff int64      `json:"futures_claims_written_off"`
	OpenMortgages           int64      `json:"open_mortgages"`
	LentOutstandingPence    int64      `json:"lent_outstanding_pence"`
	SmugglingConvictions    int64      `json:"smuggling_convictions"`
	BankruptAt              *time.Time `json:"bankrupt_at"`
	LastDefaultAt           *time.Time `json:"last_default_at"`
	LastConvictionAt        *time.Time `json:"last_conviction_at"`
}

type FuturesContract struct {
	ID                string    `json:"id"`
	PortID            string    `json:"port_id"`
	CommodityID       string    `json:"commodity_id"`
	CreatorCompanyID  string    `json:"creator_company_id"`
	Side              string    `json:"side"`
	LotTons           int32     `json:"lot_tons"`
	StrikePricePence  int64     `json:"strike_price_pence"`
	SellerMarginPence int64     `json:"seller_margin_pence"`
	BuyerMarginPence  int64     `json:"buyer_margin_pence"`
	SellerCompanyID   *string   `json:"seller_company_id"`
	BuyerCompanyID    *string   `json:"buyer_company_id"`
	Status            string    `json:"status"`
	MaturesAt         time.Time `json:"matures_at"`
	CreatedAt         time.Time `json:"created_at"`
	UpdatedAt         time.Time `json:"updated_at"`
}

func (contract *FuturesContract) UnmarshalJSON(body []byte) error {
	type plain FuturesContract
	var capture struct {
		plain
		LotTons           *int32 `json:"lot_tons"`
		StrikePricePence  *int64 `json:"strike_price_pence"`
		SellerMarginPence *int64 `json:"seller_margin_pence"`
		BuyerMarginPence  *int64 `json:"buyer_margin_pence"`
	}
	if err := json.Unmarshal(body, &capture); err != nil {
		return err
	}
	if capture.LotTons == nil || capture.StrikePricePence == nil || capture.SellerMarginPence == nil || capture.BuyerMarginPence == nil {
		return errors.New("futures contract is missing required financial terms")
	}
	*contract = FuturesContract(capture.plain)
	contract.LotTons, contract.StrikePricePence = *capture.LotTons, *capture.StrikePricePence
	contract.SellerMarginPence, contract.BuyerMarginPence = *capture.SellerMarginPence, *capture.BuyerMarginPence
	return nil
}

type SettleFuturesResult struct {
	FuturesID        string `json:"futures_id"`
	Status           string `json:"status"`
	SettledTons      int32  `json:"settled_tons"`
	PenaltyPaidPence int64  `json:"penalty_paid_pence"`
	ClaimPence       int64  `json:"claim_pence"`
}

func (result *SettleFuturesResult) UnmarshalJSON(body []byte) error {
	type plain SettleFuturesResult
	var capture struct {
		plain
		SettledTons      *int32 `json:"settled_tons"`
		PenaltyPaidPence *int64 `json:"penalty_paid_pence"`
		ClaimPence       *int64 `json:"claim_pence"`
	}
	if err := json.Unmarshal(body, &capture); err != nil {
		return err
	}
	if capture.SettledTons == nil || capture.PenaltyPaidPence == nil || capture.ClaimPence == nil {
		return errors.New("futures settlement is missing required financial evidence")
	}
	*result = SettleFuturesResult(capture.plain)
	result.SettledTons, result.PenaltyPaidPence, result.ClaimPence = *capture.SettledTons, *capture.PenaltyPaidPence, *capture.ClaimPence
	return nil
}

type WantsResult struct {
	PortID string            `json:"port_id"`
	IsCove bool              `json:"is_cove"`
	Wants  []LiveWantView    `json:"wants"`
	Board  []CarriedWantView `json:"board"`
}

type LiveWantView struct {
	CommodityID   string `json:"commodity_id"`
	MultipleBPS   int32  `json:"multiple_bps"`
	RemainingTons int32  `json:"remaining_tons"`
}

type CarriedWantView struct {
	PortID       string    `json:"port_id"`
	CommodityID  string    `json:"commodity_id"`
	MultipleBPS  int32     `json:"multiple_bps"`
	ObservedAt   time.Time `json:"observed_at"`
	AgeGameHours int64     `json:"age_game_hours"`
}

type CoveBook struct {
	CommodityID string          `json:"commodity_id"`
	HouseAsk    *CoveBookLevel  `json:"house_ask"`
	Bids        []CoveBookLevel `json:"bids"`
	Asks        []CoveBookLevel `json:"asks"`
}

type CoveBookLevel struct {
	PricePence int64 `json:"price_pence"`
	Quantity   int32 `json:"quantity"`
}

// Loan is the complete /v1/loan/list and loan mutation response. Money is
// integer pence; timestamps are real UTC, not accelerated game time.
// Annual rates are bps per 360 game days; legacy daily rates remain distinct.
type Loan struct {
	ID                          string     `json:"id"`
	LenderCompanyID             string     `json:"lender_company_id"`
	BorrowerCompanyID           string     `json:"borrower_company_id"`
	BankingRegion               string     `json:"banking_region"`
	PrincipalPence              int64      `json:"principal_pence"`
	RateBasis                   string     `json:"rate_basis"`
	DailyRateBPS                int32      `json:"daily_rate_bps"`
	AnnualRateBPS               *int32     `json:"annual_rate_bps"`
	TermGameDays                int32      `json:"term_game_days"`
	Repayment                   string     `json:"repayment"`
	TotalRepayablePence         *int64     `json:"total_repayable_pence"`
	InstallmentPence            *int64     `json:"installment_pence"`
	NextPaymentDueAt            *time.Time `json:"next_payment_due_at"`
	NextPaymentPence            *int64     `json:"next_payment_pence"`
	ArrearsPence                int64      `json:"arrears_pence"`
	ArrearsDueBy                *time.Time `json:"arrears_due_by"`
	IsMortgage                  bool       `json:"is_mortgage"`
	Recourse                    bool       `json:"recourse"`
	CollateralKind              string     `json:"collateral_kind"`
	CollateralShipID            *string    `json:"collateral_ship_id"`
	CollateralPortID            *string    `json:"collateral_port_id"`
	CollateralCommodityID       *string    `json:"collateral_commodity_id"`
	CollateralTons              *int32     `json:"collateral_tons"`
	CollateralContractID        *string    `json:"collateral_contract_id"`
	CollateralDepositID         *string    `json:"collateral_deposit_id"`
	DepositLockPence            int64      `json:"deposit_lock_pence"`
	CollateralValuePence        int64      `json:"collateral_value_pence"`
	CollateralStatus            string     `json:"collateral_status"`
	Status                      string     `json:"status"`
	OutstandingPrincipalPence   int64      `json:"outstanding_principal_pence"`
	AccruedInterestPence        int64      `json:"accrued_interest_pence"`
	OwedPence                   int64      `json:"owed_pence"`
	PrincipalRepaidPence        int64      `json:"principal_repaid_pence"`
	InterestPaidPence           int64      `json:"interest_paid_pence"`
	CollateralCreditPence       int64      `json:"collateral_credit_pence"`
	ExtinguishedPence           int64      `json:"extinguished_pence"`
	WrittenOffPence             int64      `json:"written_off_pence"`
	ForeclosureSurplusPaidPence int64      `json:"foreclosure_surplus_paid_pence"`
	ForeclosureSurplusOwedPence int64      `json:"foreclosure_surplus_owed_pence"`
	OfferExpiresAt              time.Time  `json:"offer_expires_at"`
	DisbursedAt                 *time.Time `json:"disbursed_at"`
	MaturesAt                   *time.Time `json:"matures_at"`
	DefaultedAt                 *time.Time `json:"defaulted_at"`
	ClosedAt                    *time.Time `json:"closed_at"`
	InterestAccruedThrough      *time.Time `json:"interest_accrued_through"`
	RequestID                   *string    `json:"request_id"`
	QuoteID                     *string    `json:"quote_id"`
	CreatedAt                   time.Time  `json:"created_at"`
	UpdatedAt                   time.Time  `json:"updated_at"`
}

func (l *Loan) UnmarshalJSON(raw []byte) error {
	type loanFields Loan
	var decoded struct {
		loanFields
		IsMortgage json.RawMessage `json:"is_mortgage"`
	}
	if err := json.Unmarshal(raw, &decoded); err != nil {
		return err
	}
	*l = Loan(decoded.loanFields)
	if decoded.IsMortgage == nil {
		// Durable mission JSON written before is_mortgage existed must still
		// load; live captures reject the missing classification themselves.
		return nil
	}
	var mortgage *bool
	if err := json.Unmarshal(decoded.IsMortgage, &mortgage); err != nil || mortgage == nil {
		return errors.New("loan has unknown is_mortgage")
	}
	l.IsMortgage = *mortgage
	return nil
}

// LoanQuote is a stored lender or Goldsmiths quote. AnnualRateBPS is interest
// per 360 game days on outstanding principal. Money is integer pence;
// amortised installments fall every 10 game days, with the remainder at maturity.
type LoanQuote struct {
	ID                  string    `json:"id"`
	RequestID           *string   `json:"request_id"`
	LenderCompanyID     string    `json:"lender_company_id"`
	LenderName          string    `json:"lender_name"`
	BorrowerCompanyID   string    `json:"borrower_company_id"`
	BankingRegion       string    `json:"banking_region"`
	PrincipalPence      int64     `json:"principal_pence"`
	AnnualRateBPS       int32     `json:"annual_rate_bps"`
	TermGameDays        int32     `json:"term_game_days"`
	Repayment           string    `json:"repayment"`
	TotalRepayablePence int64     `json:"total_repayable_pence"`
	InstallmentPence    *int64    `json:"installment_pence"`
	CollateralShipID    *string   `json:"collateral_ship_id"`
	Status              string    `json:"status"`
	ExpiresAt           time.Time `json:"expires_at"`
	LoanID              *string   `json:"loan_id"`
	CreatedAt           time.Time `json:"created_at"`
}

// GoldsmithsQuote is an instant ship-lien quote stored for 6 game hours.
// Its principal cap includes the ship's loan-to-value limit, the per-loan cap
// and the house's available funds. Recourse is always true.
type GoldsmithsQuote struct {
	Quote             LoanQuote `json:"quote"`
	RebuildValuePence int64     `json:"rebuild_value_pence"`
	LoanToValueBPS    int64     `json:"loan_to_value_bps"`
	MaxPrincipalPence int64     `json:"max_principal_pence"`
	Recourse          bool      `json:"recourse"`
}

type Profile struct {
	Company     Company  `json:"company"`
	Handle      string   `json:"handle"`
	FleetCount  int64    `json:"fleet_count"`
	FactorPorts []string `json:"factor_ports"`
	Vaults      []Vault  `json:"vaults"`
}

type Company struct {
	ID            string    `json:"id"`
	PlayerID      string    `json:"player_id"`
	Name          string    `json:"name"`
	HomePortID    string    `json:"home_port_id"`
	CrownPrestige int64     `json:"crown_prestige"`
	CreatedAt     time.Time `json:"created_at"`
	UpdatedAt     time.Time `json:"updated_at"`
}

type SailingNotice struct {
	Direction   string    `json:"direction"`
	PortID      string    `json:"port_id"`
	ShipName    string    `json:"ship_name"`
	ShipClass   string    `json:"ship_class"`
	CompanyName string    `json:"company_name"`
	LoadState   string    `json:"load_state"`
	SeenAt      time.Time `json:"seen_at"`
	OtherPortID *string   `json:"other_port_id"`
	ConvoyName  *string   `json:"convoy_name"`
}

// Event is an SSE frame. ID is always taken from the SSE id field, never from
// JSON data; callbacks should persist the event and cursor in one transaction.
type Event struct {
	ID   string          `json:"id"`
	Type string          `json:"type"`
	Data json.RawMessage `json:"data"`
}
