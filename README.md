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
go run ./cmd/tw-sim backfill --account Yukine
go run ./cmd/tw-sim validate --account Yukine --from 2026-10-22T00:00:00Z
go run ./cmd/tw-sim run --account Yukine --from 2026-10-22T00:00:00Z --out comparison.json
```

On Windows, build or reuse a versioned binary and restart the live bot in its own console:

```powershell
powershell -NoProfile -ExecutionPolicy Bypass -File scripts/restart-bot.ps1
powershell -NoProfile -ExecutionPolicy Bypass -File scripts/restart-bot.ps1 -Version abc1234
```

The second command rolls back to an existing binary without building. The default run directory is the first of this repository root or its main worktree containing `.env`; delivery defaults to `<main worktree>-runtime-delivery`. Override them with `-RunDir` and `-DeliveryDir`. Default builds use the captured HEAD revision in a temporary detached worktree. Tracked changes under `cmd/`, `internal/`, `go.mod` or `go.sum` abort with a file list; deploying uncommitted fixes requires `-AllowDirty`, which always builds a staged `<hash>-dirty` binary from the working tree. Builds disable workspaces, set `GOFLAGS=-mod=readonly` without disabling other saved Go settings, and use `-mod=readonly`. Temporary checkout hooks are disabled; cleanup retries once and reports any leaked worktree path. Automatic reuse requires matching HEAD metadata and `vcs.modified=false`; otherwise the script rebuilds. Explicit `-Version` preserves rollback to modified or metadata-incomplete binaries with a warning, but a present revision must match the requested prefix. Only bot executables under the delivery directory are stopped. A build failure leaves the live bot untouched. Read [Fast restart](docs/runbook.md#fast-restart) for guarded console shutdown and legacy rollback: older binaries may return success as `running, revision unverifiable` after readiness and executable-path confirmation. Neither readiness nor warnings prove trading progress or profit.

Verified replacements are installed before shutdown. If a same-version target is still running, the replacement launches from a retained versioned staged path instead. A later restart promotes the newest verified stage to its canonical versioned filename when not running and prunes stale stages. A failure after shutdown keeps that verified executable and prints its exact, safely single-quoted manual relaunch command; inspect existing bot processes before using it.

The safe negative restart check uses a committed syntax error on a throwaway branch/worktree under `%TEMP%`, runs that worktree's restart script with a disposable run directory and unique delivery directory, and confirms exit 1, unchanged live PIDs, no staged executable and no leaked build worktree. Remove the throwaway branch/worktree afterward. Follow [Safe negative restart verification](docs/runbook.md#safe-negative-restart-verification); never substitute a normal buildable commit.

Additional account creation is explicit. `run` starts an independent worker for each enabled, fully registered database account. Company ownership locks prevent competing executors; the request limiter also coordinates the shared public IP within the process.

Live passes also produce an encrypted, deduplicated replay tape. `REPLAY_RETENTION_DAYS` defaults to 90 real days (`0` disables recording and pruning); `REPLAY_MAX_GIB` defaults to 20 GiB. `tw-sim backfill` imports surviving diagnostics without game requests; reruns add no duplicate frames or payloads. `validate` checks recorded accounting and market/voyage predictions. `run` fits an earlier window and compares five fixed-fleet policies under three regeneration scenarios, printing simulated estimates and optional deterministic JSON. Both use database-enforced read-only connections with no migrations or game requests. A failed gate exits 2 and marks every comparison `UNTRUSTED`; no live policy defaults change. See [Replay recording](docs/runbook.md#replay-recording) for coverage, storage, validation and model limitations.

## Checks

```sh
gofmt -w cmd internal
go vet ./...
go test ./...
go build ./cmd/...
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
