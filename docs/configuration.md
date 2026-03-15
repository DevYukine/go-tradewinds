# Configuration

All configuration via environment variables. Loaded from `.env` file if present.

## Environment Variables

### Required
| Variable | Description |
|----------|-------------|
| `PLAYER_EMAIL` | Game account email |
| `PLAYER_PASSWORD` | Game account password |

### Database
| Variable | Default | Description |
|----------|---------|-------------|
| `DB_HOST` | `localhost` | PostgreSQL host |
| `DB_PORT` | `5432` | PostgreSQL port |
| `DB_USER` | `postgres` | Database user |
| `DB_PASSWORD` | `postgres` | Database password |
| `DB_NAME` | `tradewinds` | Database name |
| `DB_SSLMODE` | `disable` | SSL mode |

### Redis
| Variable | Default | Description |
|----------|---------|-------------|
| `REDIS_URL` | — | **Required.** Redis connection URL (e.g., `redis://localhost:6379/0`) |

Persists rate limiter state, price cache, scanner position, and world data cache across restarts.

### Bot
| Variable | Default | Description |
|----------|---------|-------------|
| `BOT_BASE_URL` | `https://tradewinds.fly.dev` | Game server URL |
| `API_PORT` | `3002` | Dashboard API port |
| `ENV` | `development` | Environment |
| `LOG_LEVEL` | `debug` | Log level |
| `RATE_LIMIT_PER_MINUTE` | `900` | API rate budget |
| `STRATEGY_ALLOCATION` | `arbitrage:3,bulk_hauler:1,passenger_sniper:1` | Strategy distribution |

### Agent
| Variable | Default | Description |
|----------|---------|-------------|
| `AGENT_TYPE` | `heuristic` | Agent type: heuristic, llm, composite |
| `LLM_PROVIDER` | — | claude, openai, ollama |
| `LLM_MODEL` | — | Model name (auto-detected per provider) |
| `LLM_API_KEY` | — | API key for claude/openai |
| `LLM_MAX_TOKENS` | `4096` | Max response tokens |
| `COMPOSITE_FAST_AGENT` | `heuristic` | Fast agent for composite |
| `COMPOSITE_SLOW_AGENT` | `llm` | Slow agent for composite |

## Strategy Allocation Format

Comma-separated `name:count` pairs:
```
STRATEGY_ALLOCATION=arbitrage:3,bulk_hauler:1,passenger_sniper:1
```

Total companies = sum of counts. Scaler may reduce if rate limit budget is insufficient.
