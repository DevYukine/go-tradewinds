# Dashboard Server

REST API + SSE streaming for the web dashboard.

## Server (`internal/server/server.go`)

Fiber web server on `API_PORT` (default 3002).

### Middleware
- CORS: allow all origins, GET/OPTIONS
- Request logging: method, path, status, latency

## REST Endpoints (`internal/server/handlers.go`)

| Method | Path | Description |
|--------|------|-------------|
| GET | `/api/companies` | All company records |
| GET | `/api/companies/:id/pnl` | P&L time series (`?since=` timestamp) |
| GET | `/api/companies/:id/trades` | Trade log (`?limit=&offset=`) |
| GET | `/api/companies/:id/passengers` | Passenger boarding log (`?limit=&offset=`) |
| GET | `/api/companies/:id/logs` | Company logs (`?limit=&offset=`) |
| GET | `/api/companies/:id/decisions` | Agent decision logs (latest 20) |
| GET | `/api/companies/:id/inventory` | Live ship cargo + warehouse state |
| GET | `/api/strategy-metrics` | Latest metrics per strategy |
| GET | `/api/optimizer/log` | Strategy metric history (`?limit=&offset=`) |
| GET | `/api/prices` | Latest NPC prices (deduplicated per port+good) |
| GET | `/api/ratelimit` | Rate limit status (used, max, utilization, remaining, resets_at) |
| GET | `/api/health` | Health check (status, uptime, companies, agent type) |
| GET | `/api/world` | World data (ports with lat/lng, goods, routes, ship types) — refreshes dynamically |
| GET | `/api/ships` | All ships across all companies (for world map) |
| GET | `/api/global-pnl` | Aggregate P&L across all running companies |
| GET | `/api/companies/:id/game-trades` | Game API trade history |
| GET | `/api/companies/:id/market-orders` | P2P order activity (fills, posts) |
| GET | `/api/companies/:id/ship-events` | Ship purchase/sale history (`?limit=&offset=`) |
| GET | `/api/companies/:id/warehouse-events` | Warehouse operations log (`?limit=&offset=`) |
| GET | `/api/companies/:id/p2p-orders` | P2P order activity (`?limit=&offset=`) |
| GET | `/api/companies/:id/strategy-changes` | Strategy change history |
| GET | `/api/companies/:id/quote-failures` | Recent quote failures (`?limit=&offset=`) |
| GET | `/api/analytics/goods` | Profit by cargo type (`?hours=` filter) |
| GET | `/api/analytics/routes` | Profit by route (`?hours=` filter) |
| GET | `/api/analytics/timeline` | Profit over time (`?group_by=good\|route\|strategy&hours=`) |
| GET | `/api/analytics/ships` | Ship ROI analytics (purchase price vs revenue) |
| GET | `/api/analytics/warehouses` | Warehouse utilization analytics |

## SSE Endpoints (`internal/server/sse.go`)

| Path | Description |
|------|-------------|
| GET | `/sse/logs/:id` | Live log stream for a company |
| GET | `/sse/pnl/:id` | Live P&L stream (`?since_id=`) |
| GET | `/sse/events/:id` | Real-time state change notifications |

### `/sse/logs/:id`
Subscribes to `CompanyLogger` ring buffer. Streams log entries as line-delimited JSON. Auto-cleans on disconnect.

### `/sse/pnl/:id`
Polls `PnLSnapshot` table every 5 seconds. Tracks `since_id` to only send new snapshots. Supports `?since_id=` query param for resumption.

### `/sse/events/:id`
Subscribes to `EventBroadcaster` on the `CompanyRunner`. Streams typed state change events as JSON:
- `ship_bought`, `ship_docked`, `ship_sailed`, `ship_sold` — fleet changes
- `trade`, `passenger` — activity events
- `economy` — treasury/upkeep refresh
- `warehouse` — warehouse purchased

The **company detail page** uses these to trigger instant re-fetches of inventory, trades, and company data instead of waiting for poll intervals. The overview page uses polling only — SSE connections are limited to the detail page to stay within the browser's 6-connection-per-origin HTTP/1.1 limit.
