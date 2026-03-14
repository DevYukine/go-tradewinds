package api

import (
	"encoding/json"
	"time"

	"github.com/google/uuid"
)

// --- Generic Envelope ---

// APIResponse wraps all successful API responses in a { data: T } envelope.
type APIResponse[T any] struct {
	Data T `json:"data"`
}

// APIError represents the standard error response from the API.
type APIError struct {
	Errors struct {
		Detail string            `json:"detail,omitempty"`
		Fields map[string][]string `json:"-"` // changeset errors: { field: ["msg"] }
	} `json:"errors"`
}

func (e *APIError) Error() string {
	if e.Errors.Detail != "" {
		return e.Errors.Detail
	}
	return "unknown API error"
}

// PaginationParams holds cursor-based pagination parameters.
type PaginationParams struct {
	After  string `url:"after,omitempty"`
	Before string `url:"before,omitempty"`
	Limit  int    `url:"limit,omitempty"`
}

// --- Auth ---

type RegisterRequest struct {
	Name      string `json:"name"`
	Email     string `json:"email"`
	Password  string `json:"password"`
	DiscordID string `json:"discord_id,omitempty"`
}

type LoginRequest struct {
	Email    string `json:"email"`
	Password string `json:"password"`
}

type LoginResponse struct {
	Token string `json:"token"`
}

type Player struct {
	ID         uuid.UUID `json:"id"`
	Name       string    `json:"name"`
	Email      string    `json:"email"`
	Enabled    bool      `json:"enabled"`
	InsertedAt time.Time `json:"inserted_at"`
}

// --- Company ---

type CreateCompanyRequest struct {
	Name       string    `json:"name"`
	Ticker     string    `json:"ticker"`
	HomePortID uuid.UUID `json:"home_port_id"`
}

type Company struct {
	ID         uuid.UUID `json:"id"`
	Name       string    `json:"name"`
	Ticker     string    `json:"ticker"`
	Treasury   int64     `json:"treasury"`
	Reputation int64     `json:"reputation"`
	Status     string    `json:"status"`
	HomePortID uuid.UUID `json:"home_port_id"`
}

type CompanyEconomy struct {
	Treasury        int64 `json:"treasury"`
	Reputation      int64 `json:"reputation"`
	ShipUpkeep      int64 `json:"ship_upkeep"`
	WarehouseUpkeep int64 `json:"warehouse_upkeep"`
	TotalUpkeep     int64 `json:"total_upkeep"`
}

type LedgerEntry struct {
	ID            uuid.UUID       `json:"id"`
	Amount        int64           `json:"amount"`
	Reason        string          `json:"reason"`
	ReferenceType string          `json:"reference_type"`
	ReferenceID   uuid.UUID       `json:"reference_id"`
	OccurredAt    time.Time       `json:"occurred_at"`
	Meta          json.RawMessage `json:"meta,omitempty"`
}

// --- World ---

type Port struct {
	ID             uuid.UUID `json:"id"`
	Name           string    `json:"name"`
	Code           string    `json:"shortcode"`
	IsHub          bool      `json:"is_hub"`
	TaxRateBps     int       `json:"tax_rate_bps"`
	CountryID      uuid.UUID `json:"country_id"`
	Traders        []Trader  `json:"traders,omitempty"`
	OutgoingRoutes []Route   `json:"outgoing_routes,omitempty"`
}

type Good struct {
	ID          uuid.UUID `json:"id"`
	Name        string    `json:"name"`
	Description string    `json:"description"`
	Category    string    `json:"category"`
}

type Route struct {
	ID       uuid.UUID `json:"id"`
	FromID   uuid.UUID `json:"from_id"`
	ToID     uuid.UUID `json:"to_id"`
	Distance float64   `json:"distance"`
}

type ShipType struct {
	ID          uuid.UUID `json:"id"`
	Name        string    `json:"name"`
	Capacity    int       `json:"capacity"`
	Speed       int       `json:"speed"`
	Upkeep      int       `json:"upkeep"`
	BasePrice   int       `json:"base_price"`
	Passengers  int       `json:"passengers"`
	Description string    `json:"description"`
}

// --- Trade ---

type QuoteRequest struct {
	PortID   uuid.UUID `json:"port_id"`
	GoodID   uuid.UUID `json:"good_id"`
	Action   string    `json:"action"` // "buy" or "sell"
	Quantity int       `json:"quantity"`
}

type Quote struct {
	Token      string    `json:"token"`
	PortID     uuid.UUID `json:"port_id"`
	GoodID     uuid.UUID `json:"good_id"`
	Action     string    `json:"action"`
	Quantity   int       `json:"quantity"`
	UnitPrice  int       `json:"unit_price"`
	TotalPrice int       `json:"total_price"`
	ExpiresAt  time.Time `json:"expires_at"`
}

type QuoteResponse struct {
	Token string `json:"token"`
	Quote Quote  `json:"quote"`
}

type Destination struct {
	Type     string    `json:"type"` // "ship" or "warehouse"
	ID       uuid.UUID `json:"id"`
	Quantity int       `json:"quantity"`
}

type ExecuteQuoteRequest struct {
	Token        string        `json:"token"`
	Destinations []Destination `json:"destinations"`
}

type TradeExecution struct {
	Action     string    `json:"action"`
	CompanyID  uuid.UUID `json:"company_id"`
	GoodID     uuid.UUID `json:"good_id"`
	PortID     uuid.UUID `json:"port_id"`
	Quantity   int       `json:"quantity"`
	UnitPrice  int       `json:"unit_price"`
	TotalPrice int       `json:"total_price"`
}

type ExecuteTradeRequest struct {
	PortID       uuid.UUID     `json:"port_id"`
	GoodID       uuid.UUID     `json:"good_id"`
	Action       string        `json:"action"`
	Destinations []Destination `json:"destinations"`
}

type BatchQuoteRequest struct {
	Requests []QuoteRequest `json:"requests"`
}

type BatchQuoteResult struct {
	Status  string `json:"status"` // "success" or "error"
	Token   string `json:"token,omitempty"`
	Quote   *Quote `json:"quote,omitempty"`
	Message string `json:"message,omitempty"`
}

type BatchExecuteQuoteRequest struct {
	Requests []ExecuteQuoteRequest `json:"requests"`
}

type BatchExecuteResult struct {
	Status    string          `json:"status"` // "success" or "error"
	Execution *TradeExecution `json:"execution,omitempty"`
	Message   string          `json:"message,omitempty"`
}

// --- Market ---

type Order struct {
	ID               uuid.UUID  `json:"id"`
	CompanyID        uuid.UUID  `json:"company_id"`
	PortID           uuid.UUID  `json:"port_id"`
	GoodID           uuid.UUID  `json:"good_id"`
	Side             string     `json:"side"` // "buy" or "sell"
	Price            int        `json:"price"`
	Total            int        `json:"total"`
	Remaining        int        `json:"remaining"`
	Status           string     `json:"status"`
	PostedReputation int64      `json:"posted_reputation"`
	ExpiresAt        *time.Time `json:"expires_at,omitempty"`
}

type CreateOrderRequest struct {
	PortID uuid.UUID `json:"port_id"`
	GoodID uuid.UUID `json:"good_id"`
	Side   string    `json:"side"`
	Price  int       `json:"price"`
	Total  int       `json:"total"`
}

type FillOrderRequest struct {
	Quantity int `json:"quantity"`
}

type BlendedPriceResponse struct {
	BlendedPrice float64 `json:"blended_price"`
}

type OrderFilters struct {
	PortIDs []uuid.UUID `url:"port_ids[],omitempty"`
	GoodIDs []uuid.UUID `url:"good_ids[],omitempty"`
	Side    string      `url:"side,omitempty"`
}

// --- Passengers ---

type Passenger struct {
	ID                uuid.UUID  `json:"id"`
	Count             int        `json:"count"`
	Bid               int        `json:"bid"`
	Status            string     `json:"status"` // "available" or "boarded"
	ExpiresAt         time.Time  `json:"expires_at"`
	OriginPortID      uuid.UUID  `json:"origin_port_id"`
	DestinationPortID uuid.UUID  `json:"destination_port_id"`
	ShipID            *uuid.UUID `json:"ship_id,omitempty"`
}

type BoardPassengerRequest struct {
	ShipID uuid.UUID `json:"ship_id"`
}

type PassengerFilters struct {
	Status string `url:"status,omitempty"`
	PortID string `url:"port_id,omitempty"`
	ShipID string `url:"ship_id,omitempty"`
}

// --- Fleet ---

type Ship struct {
	ID         uuid.UUID  `json:"id"`
	Name       string     `json:"name"`
	Status     string     `json:"status"` // "docked" or "traveling"
	CompanyID  uuid.UUID  `json:"company_id"`
	ShipTypeID uuid.UUID  `json:"ship_type_id"`
	PortID     *uuid.UUID `json:"port_id,omitempty"`
	RouteID    *uuid.UUID `json:"route_id,omitempty"`
	ArrivingAt *time.Time `json:"arriving_at,omitempty"`
}

type Cargo struct {
	GoodID   uuid.UUID `json:"good_id"`
	Quantity int       `json:"quantity"`
}

type TransitRequest struct {
	RouteID uuid.UUID `json:"route_id"`
}

type TransitLog struct {
	ID         uuid.UUID  `json:"id"`
	ShipID     uuid.UUID  `json:"ship_id"`
	RouteID    uuid.UUID  `json:"route_id"`
	DepartedAt time.Time  `json:"departed_at"`
	ArrivedAt  *time.Time `json:"arrived_at,omitempty"`
}

type TransferToWarehouseRequest struct {
	WarehouseID uuid.UUID `json:"warehouse_id"`
	GoodID      uuid.UUID `json:"good_id"`
	Quantity    int       `json:"quantity"`
}

type RenameShipRequest struct {
	Name string `json:"name"`
}

// --- Warehouses ---

type Warehouse struct {
	ID        uuid.UUID `json:"id"`
	Level     int       `json:"level"`
	Capacity  int       `json:"capacity"`
	PortID    uuid.UUID `json:"port_id"`
	CompanyID uuid.UUID `json:"company_id"`
}

type CreateWarehouseRequest struct {
	PortID uuid.UUID `json:"port_id"`
}

type WarehouseInventoryItem struct {
	ID          uuid.UUID `json:"id"`
	WarehouseID uuid.UUID `json:"warehouse_id"`
	GoodID      uuid.UUID `json:"good_id"`
	Quantity    int       `json:"quantity"`
}

type TransferToShipRequest struct {
	ShipID   uuid.UUID `json:"ship_id"`
	GoodID   uuid.UUID `json:"good_id"`
	Quantity int       `json:"quantity"`
}

// --- Shipyard ---

type Shipyard struct {
	ID     uuid.UUID `json:"id"`
	PortID uuid.UUID `json:"port_id"`
}

type ShipyardInventoryItem struct {
	ID         uuid.UUID `json:"id"`
	ShipyardID uuid.UUID `json:"shipyard_id"`
	ShipTypeID uuid.UUID `json:"ship_type_id"`
	ShipID     uuid.UUID `json:"ship_id"`
	Cost       int       `json:"cost"`
}

type PurchaseShipRequest struct {
	ShipTypeID uuid.UUID `json:"ship_type_id"`
}

type SellShipRequest struct {
	ShipID uuid.UUID `json:"ship_id"`
}

type SellShipResponse struct {
	Price int `json:"price"`
}

// --- Events (SSE) ---

type SSEEvent struct {
	Type string          `json:"type"`
	Data json.RawMessage `json:"data"`
}

type ShipDockedEvent struct {
	Name        string    `json:"name"`
	ShipID      uuid.UUID `json:"ship_id"`
	PortID      uuid.UUID `json:"port_id"`
	CompanyID   uuid.UUID `json:"company_id"`
	CompanyName string    `json:"company_name"`
}

type ShipSetSailEvent struct {
	Name        string    `json:"name"`
	ShipID      uuid.UUID `json:"ship_id"`
	RouteID     uuid.UUID `json:"route_id"`
	CompanyID   uuid.UUID `json:"company_id"`
	CompanyName string    `json:"company_name"`
}

type ShipBoughtEvent struct {
	Name        string    `json:"name"`
	ShipID      uuid.UUID `json:"ship_id"`
	ShipTypeID  uuid.UUID `json:"ship_type_id"`
	CompanyID   uuid.UUID `json:"company_id"`
	CompanyName string    `json:"company_name"`
}

type CompanyFormedEvent struct {
	ID     uuid.UUID `json:"id"`
	Name   string    `json:"name"`
	Ticker string    `json:"ticker"`
}

// --- Traders ---

type Trader struct {
	ID   uuid.UUID `json:"id"`
	Name string    `json:"name"`
}

type TraderPosition struct {
	ID          uuid.UUID `json:"id"`
	TraderID    uuid.UUID `json:"trader_id"`
	PortID      uuid.UUID `json:"port_id"`
	GoodID      uuid.UUID `json:"good_id"`
	StockBounds string    `json:"stock_bounds"`
	PriceBounds string    `json:"price_bounds"`
}

// --- Health ---

type HealthResponse struct {
	Status         string    `json:"status"`
	Database       string    `json:"database"`
	ObanLagSeconds float64   `json:"oban_lag_seconds"`
	ServerTime     time.Time `json:"server_time"`
}

// --- Filter helpers ---

type PortFilters struct {
	CountryID *uuid.UUID `url:"country_id,omitempty"`
	IsHub     *bool      `url:"is_hub,omitempty"`
}

type RouteFilters struct {
	FromID *uuid.UUID `url:"from_id,omitempty"`
	ToID   *uuid.UUID `url:"to_id,omitempty"`
}
