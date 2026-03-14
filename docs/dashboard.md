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
| GET | `/api/ratelimit` | Rate limit status (used, max, utilization, remaining) |
| GET | `/api/health` | Health check (status, uptime, companies, agent type) |
| GET | `/api/world` | Static world data (ports with lat/lng, goods, routes, ship types) |
| GET | `/api/ships` | All ships across all companies (for world map) |

## SSE Endpoints (`internal/server/sse.go`)

| Path | Description |
|------|-------------|
| GET | `/sse/logs/:id` | Live log stream for a company |
| GET | `/sse/pnl/:id` | Live P&L stream (`?since_id=`) |

### `/sse/logs/:id`
Subscribes to `CompanyLogger` ring buffer. Streams log entries as line-delimited JSON. Auto-cleans on disconnect.

### `/sse/pnl/:id`
Polls `PnLSnapshot` table every 5 seconds. Tracks `since_id` to only send new snapshots. Supports `?since_id=` query param for resumption.
