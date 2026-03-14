# API Client & Rate Limiter

## Client (`internal/api/client.go`)

HTTP client for the Tradewinds game API using go-resty/v3.

### Configuration
- `baseURL` — Game server (default: `https://tradewinds.fly.dev`)
- JWT token set after login, sent via `Authorization: Bearer`
- Company ID sent via `X-Company-Id` header

### Core Request Method (`do`)
- Acquires rate limit slot (priority-based)
- Envelope unwrapping: `{ data: T }` → `T`
- Retry logic: max 3 attempts
  - 429: defer to `RecordBackoff`
  - 5xx: exponential backoff starting 500ms
- `doOnce` — Single attempt without retries

### Key API Methods

| Method | Endpoint | Priority |
|--------|----------|----------|
| `Login` | `POST /auth/login` | High |
| `GetEconomy` | `GET /company/economy` | Low |
| `ListShips` | `GET /fleet/ships` | Normal |
| `GetShip` | `GET /fleet/ships/:id` | Normal |
| `GetShipInventory` | `GET /fleet/ships/:id/cargo` | Normal |
| `SendTransit` | `POST /fleet/ships/:id/transit` | Normal |
| `BatchQuotes` | `POST /trade/batch-quote` | High |
| `BatchExecuteQuotes` | `POST /trade/batch-execute` | High |
| `ListPassengers` | `GET /passengers` | Normal |
| `BoardPassenger` | `POST /passengers/:id/board` | Normal |
| `ListWarehouses` | `GET /warehouse` | Normal |
| `BuyWarehouse` | `POST /warehouse` | Normal |
| `FindShipyard` | `GET /shipyard` | Normal |
| `BuyShip` | `POST /shipyard/:id/purchase` | Normal |
| `SellShip` | `DELETE /fleet/ships/:id` | Normal |

### Pagination
`Paginate[T](ctx, path, priority)` — Cursor-based, fetches all pages.

### `ForCompany(gameID)`
Clones client bound to a specific company. Shares token, resty instance, and rate limiter.

## Rate Limiter (`internal/api/ratelimiter.go`)

Sliding window rate limiter with priority-based throttling.

### Configuration
- `maxPerMinute = 300` (configurable)
- Minimum 250ms spacing between requests

### Priority Levels
| Priority | Threshold | Use Case |
|----------|-----------|----------|
| `PriorityHigh` | Never blocked | Trade execution (expiring quotes) |
| `PriorityNormal` | Blocked at 85% | Ship transit, inventory |
| `PriorityLow` | Blocked at 70% | Price scanning, economy refresh |

### Implementation
- Ring buffer of timestamps for sliding window
- `Acquire(ctx, priority)` — Blocks until slot available
- `RecordBackoff(duration)` — Emergency pause after 429
- `Utilization()` — Current usage fraction [0.0, 1.0]
- `CurrentBudget()` — (used, max) tuple

## SSE Events (`internal/api/events.go`)

Long-lived SSE connections with auto-reconnection.

### Streams
- `/world/events` — Public (no auth)
- `/company/events` — Private (auth + company header)

### Reconnection
- Exponential backoff: 1s → 2s → 4s → ... → 5min max
- Max 10 consecutive retries before giving up

### Event Types
- `ship_docked` → `ShipDockedEvent{ShipID, PortID}`
- `ship_set_sail` → `ShipSetSailEvent{ShipID, RouteID}`
- `ship_bought` → `ShipBoughtEvent{ShipID, ShipTypeID}`
- `company_formed` → `CompanyFormedEvent{CompanyID}`
