# Operations

## Configuration and secrets

Run commands from the project directory so `.env` is found. Existing environment variables take precedence over `.env`.

| Variable | Meaning |
| --- | --- |
| `DATABASE_URL` | PostgreSQL connection URL, required |
| `CREDENTIAL_KEY` | Base64-encoded 32-byte AES-GCM key, required |
| `TRADEWINDS_API_URL` | Current game origin; default `https://tradewinds.kibb.io` |
| `HTTP_ADDR` | Go loopback listener; default `127.0.0.1:8585` |
| `API_INTERNAL_URL` | Next.js proxy target; default `http://127.0.0.1:8585` |
| `LOG_LEVEL` | Console logging threshold: `debug`, `info` (default), `warn`, or `error` |
| `RESERVE_PENCE` | Operating cash reserve, default 100,000 pence |
| `MAX_PAYBACK_GAME_DAYS` | Maximum ship-investment payback horizon, default 30 game days |
| `COMPOSE_PROJECT_NAME` | Stable local Compose project and volume identity; default `tradewinds` |
| `POSTGRES_PASSWORD` | Local Compose PostgreSQL password |
| `POSTGRES_PORT` | Local Compose port; default 55432 |
| `TEST_DATABASE_URL` | Disposable PostgreSQL database for integration tests only |

The Next.js configuration also loads the root `.env`, without overriding existing environment values. `API_INTERNAL_URL` is resolved into rewrites at frontend build time. Rebuild with `pnpm --dir web build` after changing that destination; changing only the production process environment does not rewrite an existing build.

Generate the credential key using PowerShell 7:

```powershell
[Convert]::ToBase64String([System.Security.Cryptography.RandomNumberGenerator]::GetBytes(32))
```

Or with OpenSSL:

```sh
openssl rand -base64 32
```

Copy the result into local `.env`; do not paste it into logs or issue reports. Use a URL-safe random database password or percent-encode its value inside `DATABASE_URL`. Keep a backup of the credential key separately from database dumps. Changing the key without re-encrypting stored records makes credentials and journals unreadable.

## PostgreSQL

```sh
docker compose up -d --wait
```

Migrations run during Go startup through embedded Goose migrations. A failed migration prevents startup; never bypass it with AutoMigrate or drop the live database to make the process run. The Compose volume persists accounts and trading state when the container stops.

Keep `COMPOSE_PROJECT_NAME` unchanged when moving between worktrees or merging into `master`; it identifies the existing database volume. The verified development environment uses `tradewinds-rebuild` in its ignored `.env`. Preserve that `.env`, its credential key, and the database volume during cutover. Do not replace them with the legacy bot's configuration or register the same handle against an empty database.

For a disposable integration database in the local container:

```sh
docker compose exec -T postgres createdb -U tradewinds tradewinds_test
```

Set `TEST_DATABASE_URL` to that database, then run the store's integration tests. Do not use the production `DATABASE_URL` as the test URL. Preserve the live database and its encryption key before schema changes.

## Registration and multiple accounts

```sh
go run ./cmd/tradewinds register
go run ./cmd/tradewinds accounts
```

Default identity: `Yukine` / `The Pokemon Pirates`, home `le_havre`, starter `Lapras`. Credentials are generated and encrypted before the first registration request. Re-run registration after a transient failure to resume the persisted intent. Do not delete a partial local record and regenerate a password: the upstream account may already exist.

A registration HTTP 429 is a rejected attempt, not an unresolved mutation. Its encrypted journal outcome stores the absolute `Retry-After` deadline and exact raw response bytes atomically, including when the action's replay deadline interrupts the HTTP client's cooldown wait. Re-running `register` reports the stored cooldown without creating or sending a replacement attempt until that deadline passes. It then uses a fresh derived action ID with the same immutable request parameters and retains the rejected entry. Old-format 429 outcomes without a recoverable retry deadline stop for operator reconciliation; never assume an interrupted wait finished or delete the journal to bypass it. Timeouts and 5xx responses keep the original identity; unresolved requests must still stop before the server's 24-hour replay expiry.

After an API upgrade adds a required permission, stop the executor and run `register` again. It inspects the existing company key, mints a replacement only when its binding or required scopes are missing, and saves it encrypted without recreating the player or company. Charter delivery requires `market_derivatives`. Key requests use a scope-specific durable intent so a new permission set cannot conflict with the payload of an earlier completed request.

A conflict on the exact requested handle must be resolved with its actual owner credentials. The command will not silently rename the handle. Likewise, an existing differently named company blocks automatic creation of another company. No passphrase or API key is included in `accounts` output.

Additional accounts require an explicit operator command with distinct `--handle`, `--company`, and optional `--home`. Each fully registered enabled account gets a separate runner. Do not run two executors for one company. Do not run several independent processes behind one public IP without considering their aggregate rate budget.

Company-scoped automation keys are revocable and do not use a short login-session TTL. If a key is revoked, stop that account's executor, reconcile pending actions, and replace its encrypted credential through a controlled maintenance procedure. Never hide authorization failure behind endless reconnects or silently create a new company.

## Running

Observe only:

```sh
go run ./cmd/tradewinds run
```

Trading and eligible growth:

```sh
go run ./cmd/tradewinds run --execute
```

Start the dashboard separately:

```sh
pnpm --dir web install --frozen-lockfile
pnpm --dir web dev
```

For a production frontend:

```sh
pnpm --dir web build
pnpm --dir web start
```

Keep Go and Next.js supervised as separate processes. Both bind loopback by default. Use an authenticated TLS reverse proxy for remote access. The API is an operational dashboard, not a public account service.

Ctrl+C shuts down the Fx lifecycle, cancels workers and streams, closes the HTTP listener, and releases database locks. After an unclean termination, start in observe mode if investigating unexpected funds or cargo; inspect the journal and authoritative account state before permitting new mutations.

### Replacing the live executor

Keep the existing binary running while developing and verifying its replacement. Important fixes may be deployed before the final commit once their regression checks and real CLI smoke run pass; never deploy an unfinished worktree merely because it builds. Build to a separate, versioned release path and retain the previous binary for recovery.

For each cutover, identify the exact bot process and its launcher window. Send Ctrl+C to that executor, wait for shutdown and ownership-lock release, then close its old launcher window. Start exactly one replacement in a new visible terminal using the same live configuration and database. Do not close unrelated shells or editors, kill by executable name, or start a competing executor while the old one is shutting down.

On Windows, automation must give the new shell fresh console standard handles, not inherit the automation host's redirected pipes. A visible window and a growing tee log do not prove that text reaches the screen. The verified native launch uses `CreateProcessW` with `CREATE_NEW_CONSOLE`, `bInheritHandles=false`, and `STARTF_USESTDHANDLES` unset. Check the actual console buffer for the startup message and a runtime log line; ordinary `Start-Process` or `CREATE_NEW_CONSOLE` alone can leave a blank window when launched from a redirected host.

Confirm the replacement's executable path, readiness, and advancing account decisions or events. The dashboard's per-request `updated_at` and `/readyz` alone do not prove the worker is progressing. Inspect unresolved actions and fresh authoritative ship state before calling a recovery successful; a legitimately blocked or unprofitable voyage is not evidence of a deadlock. If startup fails, preserve its error log and reconcile any migrations or mutations before choosing a compatible rollback.

### Fleet scaling

There is no ship-count ceiling. The runner assigns profitable work to compatible idle hulls before ordering more capacity and retains one durable `__growth` intent through construction, armament and provisioning. A restart or pending build is not permission to order a second hull.

Growth needs loss-inclusive, known-cost realized lane proof, a current available quote, two cycles of spare source depth and, for spot trades, two cycles of executable destination bids. Reserve competing throughput separately for shared source/commodity, destination/commodity and contract, including ships already committed to sailing returns. Finite charters must cover payback plus a cycle and remain open for that full horizon; future reposts are not demand.

Any negative vault or underfunded active/obligated region defers construction, not safe income voyages. Protect incumbent working capital, two new cycles, stores, guns, the quote cushion and three days of incremental upkeep in the home billing region. Treasury transfers must first fund the relevant regions at observed fees. Empty unused regions need no artificial reserve; escrow and hoped-for proceeds are not spendable cash. The published API has no voluntary hull-sale operation, so the runner does not sell ships to fund growth.

### Console logs

Zap writes `[time] [account] [category] [LEVEL] message` followed by the complete structured context as JSON. Root, store, API, engine and Fx records have named categories; account labels and IDs remain in the JSON even when the label appears in the header. `LOG_LEVEL=info` is the default; invalid values stop startup before opening the database.

- **INFO:** service startup, established stream recovery, committed trading/treasury/fleet outcomes and recorded settlements. Amounts are integer pence; unknown costs never appear as measured profit.
- **WARN:** recoverable connection failures, reconciliation-required actions, rejected operations and retention/read failures. Unchanged unresolved-action warnings are suppressed by the runner's bounded seven-day decision cache; they can recur after eviction or restart. Dashboard read warnings are limited to one per operation per minute.
- **ERROR:** terminal account-worker, service or lifecycle failures.
- **DEBUG:** request tracing, routine dispatch, dock/offshore transitions, Fx lifecycle and migration detail. Enable temporarily when investigating a problem.

`LOG_COLOR=auto` (also the unset/empty default) enables colors only when stderr is a terminal and `TERM` is not `dumb`. `LOG_COLOR=always` also colors redirected output, useful for a visible PowerShell window that tees logs to a file. `LOG_COLOR=never` disables colors. The presence of `NO_COLOR`, even with an empty value, overrides every color mode. Other `LOG_COLOR` values fail startup. Levels receive distinct colors and structured context is dimmed; stripping the encoder's SGR sequences leaves the same records as plain mode. Control characters in upstream messages, labels, categories and fields are escaped so they cannot inject terminal lines or commands; invalid UTF-8 is normalized when represented as JSON.

Normal polling and repeated no-op planning do not produce INFO lines. Upstream strings in committed outcomes use the store's credential and token scrubber; if credentials cannot be read, those strings are redacted while numeric outcome facts remain. Credentials, raw game payloads and database connection strings must not appear at any level. Console records are best-effort operational messages, not an exactly-once financial ledger; use the company Journal and permanent settlements for durable outcomes.

The level mapping follows the [OpenTelemetry severity model](https://opentelemetry.io/docs/specs/otel/logs/data-model/) and [Zap console encoder](https://pkg.go.dev/go.uber.org/zap/zapcore#NewConsoleEncoder). Structured context remains attached to each readable message.

## Readiness and diagnostics

- `GET http://127.0.0.1:8585/healthz`: process health.
- `GET http://127.0.0.1:8585/readyz`: storage readiness.
- `GET http://127.0.0.1:8585/metrics`: metrics exposition.
- `GET http://127.0.0.1:8585/api/dashboard`: bounded account snapshots and recent activity.
- `GET http://127.0.0.1:8585/api/history?account_id=...&kind=requests&limit=100`: filtered diagnostics. Kinds include requests, events, decisions, metrics, journal and settlements; maximum 500 rows. Journal reads require `account_id` and return company-scoped projected economic records, not raw action journals.

The browser must show per-dataset freshness and errors. A healthy HTTP process does not prove the account is trading. Inspect runner decisions, latest relevant snapshots, pending actions, and the upstream clock.

Raw request/response diagnostics, events, and decisions are retained for seven days. Current state, mission recovery records, encrypted action journals, realized settlements, and metrics remain durable. Diagnostic retention runs periodically; it must not erase unresolved actions or turn an old mission into a new trade.

## Recovery cases

### Repeated 409 responses or idle ships

Inspect the action's identity, response classification, current ship state, contract state and persisted retry deadline together. A definitive tender refusal can rearm after its bounded wait when fresh REST still confirms an active obligation and its full sealed manifest. A charter refusal can reopen when its authoritative terms change. Neither requires deleting the mission or restarting to clear an in-memory latch.

Short sealed cargo cannot be replaced with ordinary purchases. Keep the obligation until authoritative terminal state permits reconciliation; do not repeatedly submit the original full tender or interpret an error message as a default. A successful delivery or terminal settlement is recorded once across restarts.

Terminal freight reconciliation also runs for sunk carriers before recording the mission's loss. Active or missing contract state remains unresolved; a sunk hull alone does not establish a contract payout.

An ambiguous in-flight economic request retains its original key and body. Dependent spending remains blocked, but unrelated attributable delivery credits can continue only in an arrival/tender phase at the matching destination. A ship still preparing departure must not tender cargo early. If an outcome cannot be established before the safe replay window ends, operator reconciliation is still required: never clear that quarantine just to make a ship move.

### Timeout after a mutation

Do not manually send the request with another idempotency key. The server may have committed it. Preserve the intent, retry only inside the original replay window, and reconcile actual funds/cargo/contracts/voyage state. A successful purchase followed by a rejected departure does not justify another purchase.

### An unresolved action approaches 24 hours

The upstream idempotency cache is expiring. Stop blind retries before the deadline. Determine its outcome from authoritative state and settled evidence. Keep the account blocked if the outcome cannot be established. Never change timestamps or delete journal entries to bypass this guard.

### Stream reconnects or a replay gap

Backoff and replay use the stored cursor. A gap requires a fresh authoritative resync and cursor reset. Events are invalidations; replaying an old trade event must not execute another trade. Timed wake-ups still handle berth locks and deadlines without an event.

### Database or ownership loss

Stop mutations immediately. A restarted connection must not silently replace the ownership lock. After storage recovers, acquire ownership and reconcile pending work before sending more requests. Do not infer that a canceled HTTP request failed upstream.

### Fleet idle

Read the explicit rejection reasons. Check stores, safe route rating, current berth locks, fresh local liquidity, regional reserves, contract remaining quantity/deadline, and world pause/freeze state. Do not relax these constraints merely to force activity. Reassess the economic assumptions when observations change.

### Live account backup

Create a dump without exposing passwords on command lines:

```sh
docker compose exec -T postgres pg_dump -U tradewinds -Fc tradewinds --file=/tmp/tradewinds.dump
docker compose cp postgres:/tmp/tradewinds.dump ./tradewinds.dump
```

Protect the dump as sensitive operational data and move it out of the repository. Back up `.env` through a separate protected channel. Restore into a separate database first, with the original credential key and execution disabled. Never run the restored executor while the original company executor is active. Verify migrations, decrypted account records, pending intents, and snapshots before cutting over.

## Verification

```sh
gofmt -w cmd internal
go vet ./...
go test ./...
go build ./cmd/tradewinds
pnpm --dir web format:check
pnpm --dir web lint
pnpm --dir web typecheck
pnpm --dir web test
pnpm --dir web build
pnpm --dir web test src/middleware.http.test.ts -t production
```

Dashboard unit tests cover recorded decision economics, numeric cash sorting, and exact host allowlist boundaries without a server, live API, or database. The HTTP host-boundary suite (`pnpm --dir web test src/middleware.http.test.ts`) starts real Next development and production servers on scratch loopback ports; it requires neither Go nor PostgreSQL. Use this worktree exclusively while it runs: do not run another `next dev` or build in the same worktree.

The HTTP suite checks exact middleware denials for hostile and genuinely missing Host headers on `/api/dashboard` and `/api/history`, even with an allowed `X-Forwarded-Host`. Missing Host uses a raw HTTP/1.0 request because Node rejects HTTP/1.1 without Host with a parser-generated 400 before middleware runs. Allowed `Host: localhost:3400` deliberately differs from the transport port and must receive a real non-403 HTTP response. Backend-related 500/502 responses count only as gate passage, not successful proxying, Go contact, or Go health.

Production is skipped only when `web/.next/BUILD_ID` is missing, with instructions to build; an existing but unusable build fails. The post-build production command above must run against fresh output so coverage cannot remain skipped or validate a stale build. Production rewrites are build-time artifacts: changing `API_INTERNAL_URL` at `next start` does not retarget them. The HTTP suite never builds Next itself.

Run database integration tests with the disposable URL, then smoke the actual CLI and database API. For concurrency changes, run `go test -race ./...` when a working C compiler is installed. Report an unavailable race toolchain instead of calling that check passed.

For dashboard changes, start both real services, exercise account selection, map selection, fleet detail, data views, diagnostics filters, themes, and keyboard navigation. In the automation ledger, expand route economics, confirm collateral and stress disclosures, and sort the cash header in both directions: amounts must sort numerically, with unavailable values last. Inspect screenshots at the viewports in `AGENTS.md`, verify no page overflow, and check browser console output. Production data must never be replaced by demo fixtures to hide an integration problem.
