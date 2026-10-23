# Tradewinds

Autonomous fleet operations for [Tradewinds](https://tradewinds.kibb.io/): a Go trading engine, PostgreSQL operational journal, and Next.js dashboard. This is a clean-room replacement for the retired server and bot.

The objective is sustainable net profit and company growth. The engine compares observed spot, charter, and freight opportunities, preserves operating reserves, and reinvests only when measured economics justify additional ships. Hidden markets and future player activity prevent a guarantee of global optimality. See [strategy and evidence](docs/strategy.md).

## Stack

- Go 1.26.1 or newer, Uber Fx lifecycle, Zap logging.
- PostgreSQL 17, GORM, embedded Goose migrations, encrypted account credentials.
- Next.js App Router, React, TypeScript, Tailwind, TanStack Query, Zustand.
- Resumable SSE, shared rate limits, persistent idempotent actions, seven-day diagnostics, durable state and metrics.

## Start locally

Requirements: Go, Node.js 24, pnpm 11, Docker Compose.

1. Copy `.env.example` to `.env`.
2. Set a random PostgreSQL password in both `POSTGRES_PASSWORD` and `DATABASE_URL`.
3. Set `CREDENTIAL_KEY` to a base64-encoded 32-byte random key. Back it up separately from the database. Never commit `.env`.
4. Start PostgreSQL and install the dashboard dependencies:

```sh
docker compose up -d --wait
pnpm --dir web install --frozen-lockfile
```

Create or resume the explicitly configured account. Credentials are generated and encrypted in PostgreSQL before the registration request:

```sh
go run ./cmd/tradewinds register
```

Defaults: handle `Yukine`, company `The Pokemon Pirates`, home `le_havre`, starter `Lapras`. Re-running resumes the same identity; it does not silently create a replacement account or an additional company. Ships use all 1,025 canonical Pokemon species names before numeric suffixes.

Observe without trading:

```sh
go run ./cmd/tradewinds run
```

Enable autonomous trading and eligible fleet investment:

```sh
go run ./cmd/tradewinds run --execute
```

In a separate terminal:

```sh
pnpm --dir web dev
```

Open **http://127.0.0.1:3400**. The Go API listens on **127.0.0.1:8585**. The dashboard reads database snapshots; opening tabs does not create extra game connections. Both processes remain separate in production: build/start Next.js and run the Go executable. Do not expose these loopback services directly on a public interface; use an authenticated reverse proxy for remote access.

## Commands

```sh
go run ./cmd/tradewinds help
go run ./cmd/tradewinds accounts
go run ./cmd/tradewinds register --handle AnotherHandle --company "Another company" --home london
```

Additional account creation is explicit. `run` starts an independent worker for each enabled, fully registered database account. Company ownership locks prevent competing executors; the request limiter also coordinates the shared public IP within the process.

## Checks

```sh
gofmt -w cmd internal
go vet ./...
go test ./...
go build ./cmd/tradewinds
pnpm --dir web format:check
pnpm --dir web lint
pnpm --dir web typecheck
pnpm --dir web build
```

Database tests require `TEST_DATABASE_URL` pointing to a dedicated disposable PostgreSQL database. See [operations](docs/runbook.md) for configuration, recovery, backups, integration checks, and deployment. Never point destructive test fixtures at the live account database.

Go must be idiomatic, formatted, multi-line, and readable. No minified source or semicolon-packed control flow. [AGENTS.md](AGENTS.md) contains the full repository rules.

## Documentation

- [Economic research and policy](docs/strategy.md)
- [Architecture and state ownership](docs/architecture.md)
- [Operations and recovery](docs/runbook.md)
- [Official API guide](https://tradewinds.kibb.io/skill.md)
- [Official OpenAPI](https://tradewinds.kibb.io/api-docs/openapi.json)
- [Pokemon data attribution](internal/names/POKEAPI-LICENSE.txt)
