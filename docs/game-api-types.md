# Game API Types

All types from `internal/api/models.go`.

## Core Types

### Company
```go
type Company struct {
    ID          uuid.UUID
    PlayerID    uuid.UUID
    Name        string
    Ticker      string    // 3-5 char
    Treasury    int64
    Reputation  int64
    TotalUpkeep int64
    HomePortID  uuid.UUID
    Status      string    // "active", "bankrupt"
}
```

### Ship
```go
type Ship struct {
    ID          uuid.UUID
    CompanyID   uuid.UUID
    ShipTypeID  uuid.UUID
    Name        string
    Status      string     // "docked" or "traveling"
    PortID      *uuid.UUID // nil when traveling
    RouteID     *uuid.UUID // nil when docked
    ArrivingAt  *time.Time // ETA when traveling
}
```

### Port
```go
type Port struct {
    ID             uuid.UUID
    Name           string     // Real European city (Rotterdam, London, etc.)
    Code           string     // Short code
    TaxRateBps     int        // Tax in basis points (100 = 1%)
    IsHub          bool
    Latitude       float64
    Longitude      float64
    OutgoingRoutes []Route
}
```

> **Dashboard note:** The `/api/world` endpoint includes hardcoded lat/lng coordinates for all 15 ports (real European cities) to power the Leaflet world map.

### Good
```go
type Good struct {
    ID       uuid.UUID
    Name     string
    Category string
}
```

### ShipType
```go
type ShipType struct {
    ID         uuid.UUID
    Name       string
    Capacity   int    // Cargo capacity
    Speed      int    // Travel speed
    Upkeep     int    // Per-tick cost
    Passengers int    // Max passenger groups
    BasePrice  int    // Purchase cost
}
```

### Route
```go
type Route struct {
    ID       uuid.UUID
    FromID   uuid.UUID
    ToID     uuid.UUID
    Distance float64
}
```

## Trade Types

### QuoteRequest / Quote
```go
type QuoteRequest struct {
    PortID   uuid.UUID
    GoodID   uuid.UUID
    Action   string // "buy" or "sell"
    Quantity int
}

type Quote struct {
    Token      string    // Expires quickly
    GoodID     uuid.UUID
    PortID     uuid.UUID
    Action     string
    Quantity   int
    UnitPrice  int
    TotalPrice int
    TaxPaid    int
}
```

### Batch Operations
- `BatchQuoteRequest` → `[]BatchQuoteResult` (status + quote + token)
- `BatchExecuteQuoteRequest` → `[]BatchExecuteResult` (status + execution)

### TradeExecution
```go
type TradeExecution struct {
    Action     string
    GoodID     uuid.UUID
    PortID     uuid.UUID
    Quantity   int
    UnitPrice  int
    TotalPrice int
}
```

## Passenger Types
```go
type Passenger struct {
    ID                uuid.UUID
    Count             int
    Bid               int        // Payment on delivery
    Status            string     // "available", "boarded", "delivered"
    OriginPortID      uuid.UUID
    DestinationPortID uuid.UUID
    ShipID            *uuid.UUID
    ExpiresAt         time.Time
}
```

## Warehouse Types
```go
type Warehouse struct {
    ID       uuid.UUID
    PortID   uuid.UUID
    Level    int
    Capacity int
}

type WarehouseInventoryItem struct {
    GoodID   uuid.UUID
    Quantity int
}
```

## Shipyard Types
```go
type Shipyard struct {
    ID     uuid.UUID
    PortID uuid.UUID
}

type ShipyardInventoryItem struct {
    ShipTypeID uuid.UUID
    Cost       int
    Quantity   int
}
```

## SSE Event Types
```go
type ShipDockedEvent struct {
    ShipID uuid.UUID
    PortID uuid.UUID
}

type ShipSetSailEvent struct {
    ShipID  uuid.UUID
    RouteID uuid.UUID
}

type ShipBoughtEvent struct {
    ShipID     uuid.UUID
    ShipTypeID uuid.UUID
}
```
