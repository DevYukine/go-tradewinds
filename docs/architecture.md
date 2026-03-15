# Architecture Overview

The Tradewinds bot is a Go application that autonomously plays the Tradewinds trading game. It manages multiple companies, each running a trading strategy, and optimizes strategy allocation over time.

## High-Level Flow

```
Config → Manager → CompanyRunners (1 per company)
                 → PriceScanner (shared)
                 → Optimizer (shared)
                 → Dashboard Server (shared)
```

## Package Map

| Package | Path | Purpose |
|---------|------|---------|
| `config` | `internal/config/` | Env-based configuration loading |
| `cache` | `internal/cache/` | Redis-backed state persistence (rate limits, prices, world data) |
| `api` | `internal/api/` | HTTP client, rate limiter, SSE events |
| `db` | `internal/db/` | GORM models, migrations, retention pruning |
| `bot` | `internal/bot/` | Manager, CompanyRunner, Coordinator, state, scanner, scaler |
| `agent` | `internal/agent/` | Decision-making agents (heuristic, LLM, composite) |
| `strategy` | `internal/strategy/` | Strategy implementations (arbitrage, bulk_hauler, market_maker, passenger_sniper) |
| `optimizer` | `internal/optimizer/` | Strategy evaluation, reallocation, scaling |
| `server` | `internal/server/` | Dashboard REST API + SSE streaming |
| `logging` | `internal/logging/` | Zap logger initialization |

## Dependency Flow

```
config
  ↓
cache (Redis persistence: rate limits, prices, world data, scanner position)
  ↓
api (client, rate limiter, events)
  ↓
db (models, connection, retention)
  ↓
bot (manager, runner, state, scanner, coordinator, world cache, price cache)
  ↓
agent (heuristic, LLM, composite)
  ↓
strategy (arbitrage, bulk_hauler, market_maker, passenger_sniper → uses agent + base)
  ↓
optimizer (engine, metrics → reads DB, swaps strategies on runners)
  ↓
server (REST + SSE → reads DB + manager state)
```

## Concurrency Model

- **Manager** spawns one goroutine per CompanyRunner + one PriceScanner goroutine
- **CompanyRunner** has a select loop: SSE events, economy ticker (30s + jitter), strategy swap channel
- **PriceScanner** rotates through ports with adaptive interval (8-30s based on rate limit utilization)
- **Optimizer** runs on its own ticker (15 min eval cycle)
- **RateLimitPersister** saves rate limiter state to Redis every 10s + on shutdown
- **Rate Limiter** is shared across all goroutines, uses fixed window (60s reset) with priority-based throttling; state persisted to Redis
- **CompanyState** protected by `sync.RWMutex`

## DI Framework

Uses `go.uber.org/fx` for dependency injection. Modules: `config`, `cache`, `db`, `logging`, `api`, `agent`, `bot`, `strategy`, `optimizer`, `server`.
