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
| `MAX_PAYBACK_GAME_DAYS` | Maximum lane-forecast ship-investment payback horizon, default 30 game days |
| `CAPACITY_PAYBACK_GAME_DAYS` | Maximum payback for capacity growth priced from the new hull's best planned voyage, default 420 game days (about 7 real days) |
| `MARQUES_ENABLED` | Enable the bounded marque pilot; default `false`; fresh send price must not exceed the admitted quote |
| `FUTURES_PILOT_ENABLED` | Enable the single covered Amsterdam futures pilot; default `false`; no replacement cohort or scale-up |
| `REPLAY_RETENTION_DAYS` | Replay retention in real days, default `90`; `0` disables both live recording and replay pruning |
| `REPLAY_MAX_GIB` | Replay payload plus manifest byte cap in GiB, default `20`; oldest whole frames expire first |
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

This upgrade runs embedded SQL migrations `00012_settlement_plan.sql` through `00015_passenger_receipt_lifecycle.sql` and Go migration 16 on startup. Follow the quiesced backup and disposable-restore procedure below before opening production with the new executable. [Migration 00012](../internal/store/migrations/00012_settlement_plan.sql) adds nullable forecast and cargo-shortfall evidence to settlements. [Migration 00013](../internal/store/migrations/00013_passengers.sql) adds event-backed passage settlements and immutable passenger evidence. [Migration 00014](../internal/store/migrations/00014_passenger_receipts.sql) adds archived passenger receipts, unknown proceeds and terminal-event deduplication. [Migration 00015](../internal/store/migrations/00015_passenger_receipt_lifecycle.sql) adds durable retry, expiry and closure fields. [Go migration 16](../internal/store/passenger_quarantine_migration.go) moves quarantined passenger accounting out of mission slots into encrypted `passenger_quarantine_archives`. Keep the matching credential key. Passenger terminal receipts follow the [guide, passengers](https://tradewinds.kibb.io/skill.md), not an invented sale action. Migrations 00014 and 00015 refuse rollback once archived receipts exist; migration 16 always requires a pre-migration backup for rollback.

Game 0.5.1 also adds [00017_rivals_overview.sql](../internal/store/migrations/00017_rivals_overview.sql), [00018_futures.sql](../internal/store/migrations/00018_futures.sql) and [00019_underworld.sql](../internal/store/migrations/00019_underworld.sql). They create permanent rival publications/credit, futures reservations/cash evidence, and underworld visits/observations. Before deploying, stop every executor, wait for ownership locks to release, take a fresh quiesced backup with the matching credential key, and rehearse restore plus startup on a disposable database. Deploy forward only. Do not run Goose down against live pilot or credit history. Use the matching pre-upgrade backup for rollback only after reconciling any later game actions. Startup applies missing migrations in order and fails closed on migration errors. Confirm version 19 and preserve all new tables in full-database backups.


Exploration-expense and underwriting tables are additive, but **Goose version 5 is a non-backward-compatible encryption cutover**. It converts mission recovery data, settlement identities and underwriting evidence atomically, authenticates the existing credentials, verifies account/company executors are stopped, and installs a permanent key verifier. A failure rolls back the conversion and its version entry. Do not run an older executable against version 5.

Before this upgrade, stop the old executor and wait for its ownership locks to release. Take a fresh custom-format PostgreSQL backup of that quiesced state, restrict its ACL to the owner/system, and retain the original credential key separately. Restore the dump to a disposable database and run the new executable there before migrating production. Keep the old binary, dump and matching key together as the rollback set; there is no supported destructive down-migration. If the new executor has already sent actions, reconcile them before considering a restore: restoring an older journal can otherwise repeat a real-world mutation.

A missing key verifier, incorrect key or unreadable recovery ciphertext stops the affected operation; never regenerate the verifier, clear missions, reset an underwriting pilot, or delete unresolved records to bypass it. Verify permanent settlements, policy/position/budget records and unresolved journal identities after restore. The restored-live upgrade check preserved 325 encrypted actions and the account row byte-for-byte, all 35 settlement values and the 148,863 decision rows; that backup had no active missions, so mission conversion was separately covered by PostgreSQL migration regressions.

For a legacy `__growth` intent without an admitted lane, version 5 writes an explicit zero-allocation withdrawal. The runner resolves any already-attempted action with its original identity, retains paid operating costs, and releases the actual hull without starting new discretionary commissioning. It does not invent a profitable allocation or require an unnecessary trip home. A missing or malformed lane outside that migration remains invalid.

A zero recorded commissioning balance does not prove that its historical cost is known. An anchored terminal control retains a zero-ton settlement with its original uncertainty and attributable bills. Missing ledger/time boundaries are persisted once; existing boundaries are preserved. A control with unknown, paid or capital-bearing exposure but no terminal-action anchor remains held for journal/ledger investigation. Do not delete it to free the ship or manufacture a profitable history.

The first commissioning handoff preserves committed hull identity and capital cost, including a withdrawn lane; capital is not an operating expense and does not carry into a fresh post-settlement voyage. Unknown, paid or committed exposure without a terminal-action anchor remains held without claiming bills. A known-zero-effect cancellation with no unresolved action or committed exposure releases its reservations, including a repeat voyage canceled at source roadstead before any new action. Unrelated company upkeep remains in the company ledger, not a fabricated cancellation settlement or a reason to strand that ship.

An unreadable account credential omits that account's retained dashboard datasets and displays a safe account-local error; healthy accounts remain usable. Restore the matching credential/key through the documented recovery path, not by clearing private recordings.

Keep `COMPOSE_PROJECT_NAME` unchanged when moving between worktrees or updating `main`; it identifies the existing database volume. The verified development environment uses `tradewinds-rebuild` in its ignored `.env`. Preserve that `.env`, its credential key, and the database volume during cutover. Do not replace them with the legacy bot's configuration or register the same handle against an empty database.

For a disposable integration database in the local container:

```sh
docker compose exec -T postgres createdb -U tradewinds tradewinds_test
```

Set `TEST_DATABASE_URL` to that database, then run the store's integration tests. Do not use the production `DATABASE_URL` as the test URL. Preserve the live database and its encryption key before schema changes.

## Replay recording

The live recorder appends an encrypted local tape without issuing game requests. It records startup, at least 60-real-second interval opportunities, accepted plans and staged actions, and the first completed pass after a decision. Observation times are pass starts. Startup, decision and five-minute sweep frames include cached ports and routes. Ordinary frames include current-generation inputs and presence-authorized private books and yard quotes. Failed captures attempted during the pass are metadata-only gaps, never empty books. A dataset exceeding 4 MiB after gzip becomes a reasoned gap. Frames stay within a 16 MiB aggregate plaintext bound by demoting the largest datasets to reasoned metadata-only gaps, not by aborting recording. Oversized metadata or names fall back to a compact frame gap if necessary. Reads enforce the plaintext bound too.

`REPLAY_RETENTION_DAYS=90` and `REPLAY_MAX_GIB=20` are independent of seven-day diagnostics. Setting retention to `0` disables live writes and replay pruning, without deleting the existing tape. The byte cap covers encrypted compressed payloads and manifests, not PostgreSQL indexes, TOAST overhead, dead tuples or permanent plan/link rows. Hourly pruning logs logical bytes, bytes referenced in the last day and physical relation size, and warns if the cap forces a younger cutoff. Monitor physical size over at least two days and project distinct payload growth, not raw GET volume, before increasing retention. Autovacuum can reuse dead space without shrinking the database file.

Goose migration `00009_replay.sql` adds `replay_payloads`, `replay_frames`, `replay_plans` and `replay_action_links`. Known credentials and sensitive JSON fields are redacted before digesting or encryption; unaffected dataset bytes stay unchanged. Payloads are account-keyed HMAC deduplicated, gzip-compressed and AES-GCM encrypted, except events, which are encrypted without compression to avoid co-compressing untrusted text with private identifiers. Manifests are compressed and encrypted. Only validated public clock scalars are plaintext. Ship identity, phase, accepted plan, policy and revision are encrypted in the plan row.

Replay uses one bounded background writer per runner with a five-second write deadline. Queue saturation drops frames without blocking the loop and increments the process-lifetime `replay_record_errors_total` counter; pending decisions remain eligible for the next opportunity. Live dataset demotions also increment that counter and warn once per account/cause, including losses discovered after redaction or compression. Plan and link buffers are bounded, and failed writes clear their dedupe entries so later captures can retry. SSE buffering retains at most 256 envelopes and 1 MiB; overflow is explicit in the next events dataset's incomplete metadata and dropped counts/bytes. Event and competition serialization run on the writer. Runner-input metadata is frame-only, not dashboard dataset health, and competition does not consume operational snapshot space. The explicit shutdown handoff has a bounded best-effort drain and counts queued evidence it drops. Recording never owns or cancels the executor lease.

Backfill the surviving diagnostics for an explicit account label or ID:

```sh
go run ./cmd/tw-sim backfill --account Yukine
go run ./cmd/tw-sim backfill --account Yukine
```

An unchanged-source rerun adds zero frames and zero payloads for already imported generations. New diagnostics or intervening seven-day pruning can change which not-yet-imported passes are selected. Backfill uses `DATABASE_URL` and `CREDENTIAL_KEY`, performs no game requests, and runs additive migrations on open. Configuration and database failures are reported as safe classifications without raw values. After deploying the live recorder, run `tw-sim backfill` against the live database in a quiet window: hourly route path/hash aggregates and statement-timeout-bounded 5,000-row diagnostic batches still compete for live I/O. Frames written to a restored scratch database cannot be merged into the live tape by this command. Before seven-day pruning removes evidence, preserve `requests` and `events` with `pg_dump -Fc -t requests -t events`; keep the full database backup as well.

The import freezes request IDs at startup and excludes requests starting at or after the first live frame. Before live recording exists, it freezes at the maximum request ID; rerun after recorder deployment to close the gap. Passes use request starts (`created_at - duration_ms`) and retain failed clock boundaries and mutation markers. Current first-wave whoami anchors may overlap the clock or start after a completed later-wave request. When no later-wave completion exists, a staggered whoami is accepted if it starts after the previous clock completed, or if there is no previous clock. Event-stream request lifetimes are excluded from REST ordering. The client's 45-second timeout has one second of diagnostic wake-up/logging slack in the request cap and ordering frontier; longer diagnostics are skipped and reported as gaps rather than assigned fabricated start times. Diagnostic generations are `diagnostic:<boundary request id>` with a partial unique index, so reruns cannot replace existing evidence. List cursor pages are joined before publication. Complete contract statuses merge in open, active, completed, cancelled, defaulted order; incomplete historical scopes are reported. Loans require both complete active and defaulted chains, or a complete history variant; incomplete loans become reported metadata-only gaps.

The coverage report prints per-family mapped/unmapped counts, complete-book observation hours by port, truncated-only ports, clock gaps over two minutes, partial contract-history passes, incomplete loan passes, out-of-timeout diagnostic counts and oversize dataset gap counts for newly added frames. Idempotent skipped generations add no frames, payloads or gap counts. External identifiers are quoted for terminal safety. Hours count intervals between complete samples at most two minutes apart, not proof of continuous market visibility. Truncated previews are never parsed as source data. Ledger, REST world events, insurance and historical catalogs are excluded. Surviving SSE events are consumed in 256-row keyset pages as passes are assembled, never loaded as a whole stream. Each pass applies the live 256-envelope/1 MiB buffer limits while consuming events, releases consumed page payloads and discloses dropped counts/bytes. Sweep routes use the live 24-hour geometry lifetime; hourly first/last sightings avoid treating a future repeat as continuous visibility. The current snapshot catalog is explicitly assumed, not backdated fact. Common API datasets use the live dataset keys and metadata shape, but historical runner inputs are unavailable and empty event windows may be omitted. Consumers parse JSON because PostgreSQL JSONB diagnostic bytes differ from live compact JSON.

`RecordEvent` and live replay capture use a shared `json.Decoder.UseNumber` token walk before storing SSE payloads. Numbers with an exponent beyond plus or minus 32, and numeric tokens longer than 32 characters, become payload gaps with reason `numeric token outside storage bounds`; ordinary floats such as `2.5e-7` or `1E3` are kept. Nesting deeper than 64 levels becomes a gap with reason `JSON nesting outside storage bounds`. The envelope retains event identity, type and receive time, and event gaps still advance the durable cursor. The pre-rollout live-data check found 7,290 retained events with a maximum stored numeric text length of 11 characters. PostgreSQL stores JSONB numbers without exponents, so that check cannot show whether the server sends exponent form.

Event-page SQL checks `pg_column_size(payload)` and rejects nonzero numeric magnitudes outside `1e-100` through `1e100` with `jsonb_path_exists` before any JSONB text rendering. It then checks normalized `octet_length(payload::text)` before returning payload bytes to Go. Numeric-guarded values and values over 1 MiB arrive only as gap identities and are counted as dropped; `dropped_bytes` uses normalized bytes when measured, or stored bytes when a guard prevents expansion. Backfill sweep route history uses the live newest-first limits as well as the 24-hour lifetime: at most 256 routes and 1 MiB of escaped route/metadata map entries, scanning at most 1,024 candidates with deterministic key-order ties. Historical required-route policy is unavailable, so cached sweep routes are treated as optional history.

Back up all four replay tables with the operational database and keep the original credential key separately under restricted ACLs. Without that key, payload digests cannot be reproduced and ciphertext cannot be read. Plans and links are permanent evidence like actions and settlements; replay age/cap pruning never deletes them. Payload last-seen updates coalesce within one hour to reduce WAL; pruning applies the same conservative slack and can expire extra near-boundary frames to meet the cap safely. All four tables cascade on account deletion. Restore into a disposable database first, run migrations with the matching key and inspect tape ordering and gaps before using the restored record. Manifest authentication binds a portable capture identity and semantic headers, including account, generation, observation time, source and public clock/phase/catalog context, but not its generated database ID. Authorized transfers may assign new serial IDs while retaining authenticated headers and payload/account identities. Plan generation, sequence and observation time are authenticated too. Never restore an older action journal over a running executor or treat replay as authority to send historical mutations.

### Offline validation and comparisons

```sh
go run ./cmd/tw-sim validate --account Yukine --from 2026-10-22T00:00:00Z
go run ./cmd/tw-sim run --account Yukine --fit-until 2026-10-25T00:00:00Z --to 2026-10-27T00:00:00Z --out comparison.json
```

`validate` and `run` skip migrations and connect with PostgreSQL's `default_transaction_read_only=on`; they require an already migrated database and make no game requests. Every validation, calibration and variant replay in one command shares a read-only `REPEATABLE READ` snapshot, so concurrent recording/backfill cannot change later passes. The transaction stays open for the entire command, including simulation: long runs can delay PostgreSQL vacuum cleanup. Run off-peak or, preferably for long experiments, against a restored database copy. Omit `--account` only when the database contains exactly one account. Windows are half-open real UTC intervals. Omitted bounds are resolved from retained frame metadata before any action or settlement bodies are loaded; unrelated permanent history before the first retained frame is not part of the default window. Validation defaults to the first/last retained frame. A run without `--from` starts at the first completed fleet, vault, catalog and clock observation, which can be later than the first pass-start frame; an explicit earlier start cannot invent missing state. The default fitting boundary is the midpoint. The current snapshot's expense allocations and ledger are validation-only evidence and never enter simulated decisions.

The gate reconstructs five settlement money fields from linked 2xx receipts (or deterministic mission/stage/sequence IDs), allocated ledger bills and defaulted freight collateral. Spot, charter and relocation require 80% linkability and 95% exact known-cost accounting. At least ten orders with complete books no older than two real minutes must reach 90% exact filled tons and 90% gross error within 1%; at least ten held-out voyages require median absolute duration error at most 5%. Missing evidence is unlinkable, not assumed zero cost. Freight is reported separately. Failure prints `UNTRUSTED` and exits 2; `run` still writes all fifteen untrusted comparison rows and JSON.

Windows without a recorded Lloyd's quote snapshot report `UNTRUSTED` with `no recorded Lloyd's quotes`. Live replay frames already capture `_hazard_quotes`, including quote identities, timestamps and failure state. Offline planning uses those exact observations with the current live freshness, hazard-pricing and 2% minimum-margin rules; it never manufactures zero hazard. A variant also becomes `UNTRUSTED` if its evaluated lanes lack usable recorded quotes, even when other lanes have quotes. Comparisons expose `hazard_available` and `hazard_unavailable_plans`; missing hazard evidence leaves an inspection uninspected, not a failed source or cooldown. Such rows cannot receive a favorable comparison verdict. Random sinkings and cargo losses are not simulated even when pricing evidence exists.

Evidence-resource failures are a fail-closed exception: they print `UNTRUSTED` with the exceeded budget and produce no comparison from incomplete history. Action and settlement readers authenticate one SQL-size-guarded row at a time, retain only replay request/receipt fields, and reject histories above 128 KiB ciphertext per row, 16 KiB metadata per row, 16 MiB retained data or 10,000 rows. Frame headers and action links also apply SQL-side 16 KiB combined metadata guards before transferring variable-length values; action links check aggregate limits in pages of at most 100 rows. Database frames stream only up to the exclusive `--to` boundary; the reader retains last-good state rather than all decrypted frames, with a separate 64 MiB last-good/pending-state cap.

History budgets apply to the requested window and exact related receipts needed by missions crossing its start, not every lifetime body. Exploration reuses the live 60-game-day rolling conversion/expense budget and a guarded pre-tape accounting prefix, including earlier source probes attributed by deliveries through the run's holdout cutover. A slower recorded clock enlarges that prefix without loading later actual receipts or counting rows twice; no scalar lifetime seed replenishes it. Charter taper sums and latest probe completions read only the required pre-cutover prefix or live-policy lane shortlist. Raw non-JSON HTTP rejection bodies remain present as opaque, non-interpretable evidence rather than making unrelated windows unreadable. Regeneration calibration retains only evidence around relevant fills and re-quotes, not every historical book observation.

The reader never uses a later frame. Books are observed for two real minutes, then carried for at most six real hours, then dark; public port views are current for five real minutes. Routes cannot cross phase changes, and older last-good ship details cannot override a newer fleet row. Each variant shares its own depleted books across ships. Recorded fill addbacks and variant consumption become a signed stock deviation at a new NPC order, decaying at the scenario's six-game-hour rate; an exhausted order disappearing alone is not a re-quote. Player-order depletion persists until its order ID disappears. Gossip and sailings require simulated presence or a recorded factor.

Game time is interpolated only from already available clock samples. If a later sample reveals an unobserved pause or clock gap, the reader does not rewind ships or reverse previously emitted bills: it preserves a monotone estimated timeline and explicitly marks the window `UNTRUSTED` for clock uncertainty. An authoritative sample moving backwards is likewise reported, never silently treated as valid elapsed time.

Calibration uses only `[from, fit-until)`. Fewer than five independent NPC-fill/requote samples uses the explicitly labelled uncalibrated prior: regeneration 0/0.5/1 and speed factors 1.0 empty/0.8 laden. Otherwise scenarios use p25/median/p75 regeneration with linear cargo-load speed fits per hull. The holdout compares baseline, next-rung receipt stress (`i4`), and probe cash shares 0/50/400 bps against the unchanged 200-bps baseline. Ship IDs are fixed at `--from`, with recorded positions, cargo and balances at the holdout boundary; funding pays observed fees, and every cash movement reconciles to settlements, open missions or unclaimed overhead.

Rows report final net worth, baseline delta, realized net, idle percentage, probe count/spend/conversions, settlement count, observed/carried tons and dark-blocked decisions. Less than 60% observed settled tons is labelled low coverage. Net worth excludes stores and identical hull values. Random hazard outcomes, losses, berth queues, price responses, partial-fill probabilities, freight and fleet/instrument expansion are omitted or optimistic; no row is a forecast. JSON is deterministic for the same tape and arguments; wall time is printed only in the terminal. Call a variant better only after a trusted gate and a positive net-worth delta with no higher idle percentage in all three scenarios. Record the window, calibration, gate, coverage and all rows in `docs/strategy.md` as simulated estimates; adopting any live change still needs owner approval.

For a slow offline run, add `--cpuprofile cpu.pprof` and inspect it with `go tool pprof -top cpu.pprof`; profiling covers calibration and simulation, not database loading or the gate. On a shared workstation use `GOMAXPROCS=2` and below-normal process priority without opening a new console. Profile a short holdout before changing hot paths, and compare its JSON byte-for-byte before and after an optimization.


## Registration and multiple accounts

```sh
go run ./cmd/tradewinds register
go run ./cmd/tradewinds accounts
```

Default identity: `Yukine` / `The Pokemon Pirates`, home `le_havre`, starter `Lapras`. Credentials are generated and encrypted before the first registration request. Re-run registration after a transient failure to resume the persisted intent. Do not delete a partial local record and regenerate a password: the upstream account may already exist.

A registration HTTP 429 is a rejected attempt, not an unresolved mutation. Its encrypted journal outcome stores the absolute `Retry-After` deadline and exact raw response bytes atomically, including when the action's replay deadline interrupts the HTTP client's cooldown wait. Re-running `register` reports the stored cooldown without creating or sending a replacement attempt until that deadline passes. It then uses a fresh derived action ID with the same immutable request parameters and retains the rejected entry. Old-format 429 outcomes without a recoverable retry deadline stop for operator reconciliation; never assume an interrupted wait finished or delete the journal to bypass it. Timeouts and 5xx responses keep the original identity; unresolved requests must still stop before the server's 24-hour replay expiry.

After an API upgrade adds a required permission, stop the executor and run `register` again. It inspects the existing company key, mints a replacement only when its binding or required scopes are missing, and saves it encrypted without recreating the player or company. Charter delivery requires `market_derivatives`. Key requests use a scope-specific durable intent so a new permission set cannot conflict with the payload of an earlier completed request.

Loan authority uses the existing company key: `read` for `GET /v1/loan/list` and `GET /v1/loan/quote`, `treasury_remit` for `POST /v1/loan/accept-quote` and `POST /v1/loan/repay` as well as treasury remittances. There is no separate loan scope. The [guide §4.1](https://tradewinds.kibb.io/skill.md) supplies the route-to-scope mapping; the [OpenAPI](https://tradewinds.kibb.io/api-docs/openapi.json) defines those operations and the scope enum. Although a GET, the Goldsmiths quote endpoint stores an expiring quote, so observe mode does not call it.

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

### Game release notes and 0.4.0 observations

To check for game updates, read `GET https://tradewinds.kibb.io/update` (optionally `?since=<version>`), then compare the current [guide](https://tradewinds.kibb.io/skill.md) and [OpenAPI schema](https://tradewinds.kibb.io/api-docs/openapi.json). This is the owner workflow for "fetch the update"; release notes are an API response, not a local changelog to request from the owner.

The process-wide public watcher uses the configured `TRADEWINDS_API_URL` with no API key, independently of account runners, including when no account can trade. It fetches at startup and every 15 real minutes. The first successful response establishes the baseline, even if the feed is empty; the first release after an empty baseline warns. Look for info `game release notes` with `version` and `released_on`, then warn `game updated while the bot is running` for later releases, with at most five warning entries per poll. Fields are bounded and heading extraction ignores fenced code. Restarts establish a new baseline without replaying update warnings. If a later feed omits the last-seen release, it is ignored without moving the cursor. A fetch failure logs `game release notes unavailable` once per consecutive failure streak; jittered exponential retries start within one minute and cap at one hour. The isolated public client keeps limiter feedback separate from authenticated clients and caps its cooldown at one hour. Trading does not wait for this watcher.

For R&D, inspect decision kind `rnd` and snapshot datasets `rnd_status` and `rnd_register`. Authenticated status and register fetches are limited to once per account per 30 real minutes, even after a failure; last-good data and stale/error metadata remain visible. `rnd` details contain `can_start`, `cannot_start_reason`, player and entry net worth, start fee, upkeep, estimated 150-game-day project cost and the explicit no-start reason. Unchanged details do not create repeated decisions, but player net worth normally changes, so expect about one `rnd` row per 30 real minutes. The register remains raw for later rival analysis and is not an investment signal.

Diagnostics use the generic two-minute stale display for these datasets between scheduled 30-minute polls. No dataset-specific expected-cadence mechanism has been identified. A stale indicator without an error is expected between polls and is not evidence of a fetch failure; inspect the source timestamp and error metadata.

Only R&D status and the public register are observed through authenticated `/v1/rnd/status` and `/v1/rnd/register`; owned patents, licences and component inventories are not fetched. No research, patent, licence, manufacturing or fitting investment mutations are automated, even under `--execute`. Research has a non-refundable start fee, recurring upkeep and an unknown design until grant. Patents add escalating upkeep and can lapse; licence units are non-resellable and still require manufacturing materials and labour. Fitting costs 4% of ship value and is irreversible. We fit no components, so patents and loose components do not change any ship's hold or speed. Contract-list licence rows are ignored by the existing explicit freight/charter type filters. See [0.4.0 strategy](strategy.md#game-040-research-patents-and-permanent-commitments) for the published economics and policy.

Never voluntarily default on a loan. The bot does not call `/v1/loan/default`: lien collateral ships exceed the allowed principal, surrender removes earning capacity, and the walk-away remains permanently on the public credit record. Diagnose arrears and funding through normal capacity-financing recovery instead.

### Fast restart

Keep the existing binary running while developing and verifying its replacement. Important uncommitted fixes may be deployed with `-AllowDirty` once their regression checks and real CLI smoke run pass; a default restart deploys committed HEAD and refuses tracked Go changes. Never deploy an unfinished worktree merely because it builds. Build to a separate, versioned release path and retain the previous binary for recovery.

From the repository being deployed, run:

```powershell
powershell -NoProfile -ExecutionPolicy Bypass -File scripts/restart-bot.ps1
```

The Windows-only command resolves configuration from the first `.env` in this repository root or the main Git worktree. Here the normal live run directory is `F:/Repositories/Tradewinds`. Delivery defaults to the sibling `<main worktree>-runtime-delivery`, here `F:/Repositories/Tradewinds-runtime-delivery`. Both paths can be overridden:

```powershell
powershell -NoProfile -ExecutionPolicy Bypass -File scripts/restart-bot.ps1 -RunDir F:/Repositories/Tradewinds -DeliveryDir F:/Repositories/Tradewinds-runtime-delivery
```

With no `-Version`, the script captures the full HEAD revision and names the executable `tradewinds-<7-character HEAD>.exe`. Tracked changes under `cmd/`, `internal/`, `go.mod` or `go.sum` abort the command with a file list unless `-AllowDirty` is given. Untracked Go files are listed as changes that will NOT be deployed by the default build. A new clean build uses a uniquely named temporary detached Git worktree at that captured revision, not the mutable developer worktree. Worktree creation is quiet and disables checkout hooks with a nonexistent `core.hooksPath`. Cleanup runs in `finally`, removes with two force flags, retries once, then prunes registrations. A cleanup failure reports the temporary path clearly; it preserves an earlier build error rather than replacing it. `-AllowDirty` instead builds the working tree as `<hash>-dirty` and always rebuilds it, even if an older dirty artifact exists. Both paths build a staged executable to avoid overwriting a still-running binary.

Builds set `GOWORK=off`, set the nonempty `GOFLAGS=-mod=readonly` and use `go build -mod=readonly -trimpath -o <staged binary> ./cmd/tradewinds`. This overrides any saved `GOFLAGS` without disabling other persisted Go settings such as private-module, cache or toolchain configuration. The caller's environment is restored afterward. Native Git/Go calls temporarily use nonterminating error handling and check their exit codes, including with redirected stderr under Windows PowerShell 5.1. Local workspace and vendor inputs do not enter the default detached build.

Before automatic reuse, `go version -m <binary>` must report the captured full HEAD revision and `vcs.modified=false`; a missing, mismatched or modified cached identity causes a new detached build. Newly built default artifacts must pass the same identity check before shutdown. Build, metadata inspection and launcher preparation finish before stopping anything; a failed build exits 1, removes the staged executable and leaves the running bot untouched. No checks or dependency installation run during a restart.

A verified build moves into its final executable path before any bot shutdown when that path is not the running image. A same-version redeploy whose target is still running launches the retained `tradewinds-<version>-stage-<unique ID>.exe` instead; that filename remains eligible for delivery-directory process selection. On a later restart, the newest verified staged file is promoted to `tradewinds-<version>.exe` when neither path is running, unless the canonical file is already a newer verified build. Stale nonrunning stage files are pruned; running files and the latest pending replacement are preserved. If shutdown has begun and launch or readiness later fails, the verified executable is not deleted. The failure prints its retained path and an exact `run-bot.ps1` relaunch command with single-quoted PowerShell literals and doubled embedded apostrophes. Inspect current bot processes before using that command to avoid starting a duplicate executor.

The script selects only `tradewinds*` processes whose executable paths are under `-DeliveryDir`, and prints each selected version and PID. Other worktrees' executors and `go run` temporary binaries outside that directory are not selected. A hidden helper PowerShell uses kernel32 `AttachConsole` and `GetConsoleProcessList` before `GenerateConsoleCtrlEvent`: it broadcasts Ctrl+C only if every attached process is a selected bot, a matching dedicated `run-bot.ps1` host or its native-command wrapper, or the helper itself. Unrecognized shared-console occupants make it skip Ctrl+C. It waits up to 30 real seconds, then force-stops only the selected bot process instances. A missing or shared console can therefore require the forced fallback. Only matching dedicated run-bot launchers for the delivery directory are closed; unrelated shells and editors are not closed. No Python installation is needed.

The replacement starts through an Explorer-launched shortcut in an independent visible console titled `Tradewinds <version> - live trading`, outside the caller's job object. `scripts/run-bot.ps1` receives `-Binary`, `-Version` and `-RunDir`, sets `LOG_LEVEL=info` and `LOG_COLOR=always`, removes `NO_COLOR`, and appends the console stream to `bot-<version>.log` beside the executable with `Tee-Object`. A nonzero bot exit pauses that console for inspection. Go and Next.js remain separate; this command does not deploy the dashboard or supervise a crashed bot.

Only `HTTP_ADDR` is parsed from `.env` or printed as a configuration value, defaulting to `127.0.0.1:8585`. Wildcard listener addresses are checked through loopback. Within 60 real seconds a normal restart requires HTTP 200 from `/readyz`, a newly launched process at the selected executable path, and `/api/warnings` reporting the selected revision prefix and preflight VCS modified flag. It then prints active warning codes and notes unavailable/suppressed account conditions, which are not an all-clear. On failure it exits 1 and examines the last 30 log lines, printing only structured Zap records with valid JSON context after stripping encoder color codes. Raw/nonstructured output, including possible dotenv parse errors, is withheld; the protected log path is printed for local inspection. Keep log files private. It never silently rolls back or starts another executor.

The CLI reports dotenv read/parse failures only as `load .env: file could not be read or parsed`, without the parser's raw input. Inspect the file privately for quoting, readability and permissions; never print dotenv values while troubleshooting. The CLI permits a missing `.env` when configuration comes from the environment, but this restart command still requires `.env` in the selected run directory.

Roll back only to a retained, schema-compatible binary:

```powershell
powershell -NoProfile -ExecutionPolicy Bypass -File scripts/restart-bot.ps1 -Version abc1234
```

`-Version` names an existing `tradewinds-<version>.exe` and skips building. It still inspects the selected executable with `go version -m` before stopping anything. A present VCS revision must match the filename prefix. Because the operator explicitly selected this retained artifact, `vcs.modified=true` is allowed with a `binary built from a modified tree` warning, and missing VCS metadata is allowed with a `revision unverifiable` warning. This includes older delivered binaries whose worktree builds truthfully report modified metadata. A legacy rollback binary that lacks VCS metadata, returns 404 for `/api/warnings`, or cannot report a revision can still exit 0 after `/readyz` returns 200 and a newly launched process has the selected executable path. Its distinct result is `running, revision unverifiable`, not revision-verified success; warning codes are unavailable if the legacy endpoint is absent. A different reported revision is never accepted. Use the same directory overrides for rollback if needed, preserve the protected startup log, and reconcile migrations or mutations before selecting a compatible version. Restart forward without `-Version` when the fix is ready.

Readiness and revision identify the replacement, not a progressing worker. Confirm its executable path, startup/runtime console output, advancing account decisions or events, unresolved actions and fresh authoritative ship state. A fresh HTTP `updated_at`, a quiet warning strip or `/readyz` alone is not recovery proof, and a legitimately blocked or unprofitable voyage is not a deadlock.

#### Safe negative restart verification

Exercise a real failed detached build without deploying or stopping the live bot:

1. Record the live `tradewinds*` PIDs and `git worktree list --porcelain`. Create a unique throwaway branch and worktree from the deployment branch under `%TEMP%`. Do not commit on the deployment branch.
2. Ensure that throwaway branch contains the current restart scripts. Add a Go source file under `cmd/tradewinds/` containing `package main` followed by the deliberately incomplete declaration `var restartNegativeBuildFailure =`, then commit it only on the throwaway branch. The syntax error must be committed so the detached HEAD build sees it.
3. Create a disposable run directory containing only `.env` with a loopback `HTTP_ADDR`, and choose a unique empty delivery directory under `%TEMP%`. Invoke **that throwaway worktree's** `scripts/restart-bot.ps1` with `-RunDir <disposable run directory>` and `-DeliveryDir <disposable delivery directory>`, without `-AllowDirty` or `-Version`. Capture redirected stderr and the immediate exit code; the expected compiler error must produce exit 1 before shutdown.
4. Confirm the live PID list is unchanged, the delivery directory has no staged or final executable, and `git worktree list --porcelain` matches the pre-invocation list except for the known throwaway worktree. The script-created `tradewinds-build-*` worktree must be absent.
5. Remove only the throwaway worktree with `git worktree remove -f -f`, delete its throwaway branch, prune registrations, and remove its disposable run/delivery directories. Never run this verification from a working commit that can build successfully.

### Fleet scaling

There is no ship-count ceiling. The runner assigns profitable work to compatible idle hulls before ordering more capacity and retains one durable `__growth` intent through construction, armament and provisioning. A restart or pending build is not permission to order a second hull.

Growth needs loss-inclusive, known-cost account and lane proof from the last 12 recorded host-UTC hours, including at least two positive delivered cycles. A current-window loss, unknown cost, missing duration or future completion can block it. Older terminal uncertainty remains visible in permanent history without automatically vetoing a new, fully funded opportunity; unresolved requests, active missions and liabilities do not age out. The runner captures one evaluation time and recomputes the window before the first build send. An already purchased hull continues commissioning without requiring fresh build proof.

Growth also needs a current available quote, two cycles of spare source depth and, for spot trades, two cycles of executable destination bids. Reserve competing throughput separately for shared source/commodity, destination/commodity and contract, including ships already committed to sailing returns. Finite charters must cover payback plus a cycle and remain open for that full horizon; future reposts are not demand.

Any negative vault or underfunded active/obligated region defers construction, not safe income voyages. Protect incumbent working capital, the reusable new trading float, full initial stores, guns, the quote cushion, destination arrival bills and three days of incremental upkeep in the home billing region. Treasury transfers must first fund those regions at observed fees. Compare supported hulls and feasible quantities using their complete incremental capital commitment. Empty unused regions need no artificial reserve; escrow and hoped-for proceeds are not spendable cash. The player market exposes `/v1/fleet/list-ship` and `/v1/fleet/buy-ship`, but the engine uses neither: a bought ship keeps its seller's name, which conflicts with the Pokemon naming rule until the owner decides.

#### Capacity growth

When the lane-proof path above refuses or finds no package, capacity growth evaluates accepted repeatable non-charter work. It buys only hoy, cutter, brigantine and collier. Cash builds do not wait for measured earnings. Mortgage builds require trailing-30-game-day net from surviving owned hulls of the same class, at least 24 class hull-game-days, known ages and costs, and net per hull-game-day at least `1.25 × installment_pence / 10`. Their used rate is the lower of predicted and measured rate. Barque, fluyt, frigate and packet cannot become capacity candidates or savings targets. Future-dated settlement evidence blocks every hull. See [Measured capacity growth](strategy.md#measured-capacity-growth) and the [controller](../internal/engine/growth_capacity.go).

A hull is bought when:

- its yard has accepted non-charter work after incumbent commitments, from executable depth, eligible gossip or retained quay bids;
- its used rate less marginal upkeep and any quoted mortgage interest repays committed hull capital, a 10% full-quote cushion, guns and initial stores within `CAPACITY_PAYBACK_GAME_DAYS`. Retained bids do not support ordinary capacity loans. Finite charters and one-off freight are not perpetual income;
- every evaluated yard and hull records a `capacity_candidate:<port>|<hull>` decision with its reason, rate and the planner's top accepted and rejected voyages, so a refusal can be read from the decisions table;
- surplus cash above protected reserves also covers a buffer of `max(RESERVE_PENCE, 15% of hull price)` plus one cargo-cash lot per ship including the new one. A lot is the median recent known-cost lot, but never less than the planned voyage's own purchase, customs and store cash. Only the capital and the yard region's own reserve must be in the yard's region; the buffer is checked fleet-wide and is not remitted;
- no hull was lost in the last 24 real hours and no build price overrun happened in that time. Sunk hulls without a reliable loss timestamp block growth; a mission hull missing from a fresh complete fleet list is timed as a loss at that observation.

Candidates are ranked by net per game day over capital. Packets are never bought for cargo. A sourcing relocation does not pause growth, because capacity funding reserves the relocation's cash.

Before buying the best affordable hull A, compare every viable cash-blocked, better-return depth-backed hull B against buying A first. B must win the ordinary return/competition ranking under both its build-time earning net and depth-backed net. Save for the largest positive extra-earnings margin from the two purchase orders, including one extra cargo-cash lot; equality buys now. See [Capacity saving](strategy.md#capacity-saving) for the formula. B cannot rely only on a temporary expansion agent's presence. The wait must fit within the measured 24-real-hour history window, or at most 60 game days at scale 60; this bound also applies to later comparisons with the stored target.

The savings rate is the window's settlement net plus unknown-cost losses, including recorded charters, divided by its authoritative game days, less active amortised borrower installments divided by ten. Claimed upkeep bills are already settlement expenses, not a second daily deduction. A nonpositive rate disables saving. This additive-rate comparison includes no commissioning delay or interaction between the two new hulls; the history spans fleet changes and a single charter can dominate it.

Inspect `capacity_savings:<port>|<hull>` and the exact hold `capacity growth saves for a better hull`. Details use whole game days and whole Crowns for deduplication. `_capacity_savings_target` retains `port_id`, `hull_id`, `required_pence`, `capital_pence`, `depth_net_pence_per_game_day`, `depth_preference_ships` and real-UTC `expires_at`. Required cash is current available cash plus B's evaluated shortfall, independent of A. Expiry is evaluation time plus `min(maxWait, 1.25 × savings wait)` converted back to real time, providing 25% slack without exceeding the measured history window.

Only a fresh winning evaluation refreshes the target's economics; the same yard/hull's expiry can shorten but never extend. A fresh viable cash-blocked B remains protected when no affordable A exists. Fresh unavailability, mismatched quote identity, nonviability, a losing evaluation or a construction backoff clears it, as do expiry and a completed build. Missing/stale quotes or books, HTTP cooldowns and non-quay visibility leave the yard unevaluated and retain its live target: affordable capacity and lane-proof builds must beat it before spending. A losing lane package holds directly, even when the capacity-only path is paused, while B is cash-blocked. It uses B's stored economics rather than re-evaluating other income deterioration, so the original expiry bounds this limitation. When B becomes affordable, the losing lane path may build only the saved yard/hull; other capacity hulls cannot take its place. If presence is absent, wait only for a current voyage or committed mission ending there; otherwise clear it. With fresh presence and an affordable B, prefer it unless A's earning net is at least as high and A wins the build ranking. While saving, `_capacity_growth_port` remains empty and expansion-agent hiring, underwriting collateral and marque purchases wait.

An affordable saved B takes precedence over a feasible lane package; the lane path does not re-rank those two affordable builds. Its restricted capacity handoff may build only B and must not start borrowing if fresh admission finds B cash-blocked. B's marker stays live through treasury remittance and regional-funding holds, with `_capacity_growth_port` empty, until a build action is durably staged or the build completes (or expiry or a fresh negative clears it). A stream invalidation before staging preserves the marker. An existing expansion agent at B's yard remains needed while that marker is live, even with no growth mission or growth-port commitment. This keeps the next lane evaluation from spending money already being remitted for B.

`_capacity_savings_expired` stores expired yard/hull identities and their 24-real-hour exclusion deadlines. The same hull cannot restart a savings hold until that deadline or a completed build clears the exclusions, even if no A was affordable when the marker expired.

Financing is tried only for the freshly evaluated saved B, never another cash-blocked candidate while the marker holds. It requires `(B depth net + A earning net) × savings wait` to exceed estimated total interest, `36 × installment - principal`; when no A is affordable its earning net is zero. A skipped loan records the no-borrow total-interest rule, and a quoted financing refusal must leave savings intact. Financing remains limited to cash-blocked builds; with an affordable A and no winning B within the window, buy A rather than borrowing for a more distant B.

#### Goldsmiths capacity financing

Use the normative [Capacity financing policy](strategy.md#capacity-financing) for admission, debt reserves and servicing; do not override it to force a build.

Inspect `capacity_financing:<port>|<hull>` for the decision reason, shortfall, lien capacity, principal, installment, debt reserve and half-net coverage. Unknown quote-derived values are `null`, not zero. Exact financing refusals and canceled financing use the six-real-hour `_capacity_financing_backoffs`, keyed by `<port>|<hull>` and read only by financing. Yard quote and build refusals use `_growth_build_backoffs` instead. Do not clear either backoff to force progress. On a loan-list outage, restore complete authority without clearing last-good debt reserves; safe sales, deliveries and docking may continue.

For a stalled draw or repayment, inspect the durable `__growth`, `__loans` or funding `__treasury` mission and its journal action against the named loan. Check the financing `build_by` deadline before expecting construction to resume. Preserve original payloads and idempotency keys across timeouts, unreadable responses and restarts; never reset or delete them to force progress. A matching HTTP 200 receipt remains authoritative despite changed terms or collection counters. Canceled financing stays in `growth_finance_repay` until unused debt is cleared; full repayment omits the amount rather than pinning a stale payoff balance.

Monitor active arrears and the real-UTC `next_payment_due_at`, `arrears_due_by` and maturity timestamps. Do not accelerate them again: at scale 60, ten game days are four real hours and the one-game-day grace is 24 real minutes. Prioritize active-arrears funding over cargo reservations while preserving hard obligations; scheduled installments and defaulted claims are server-collected, not manual recurring repayment tasks.

#### Expansion agents

A yard needs stationary ship or factor presence for a live quote and quay books. Before hiring an expansion agent, the [agent controller](../internal/engine/growth_factor.go) must admit a projected build using a matching observed regional yard quote, measured fleet earnings, marginal hull-plus-factor upkeep and current surplus. Gossip ranks yards only. An unavailable measured rate or excessive payback holds with `expansion agent waits for an admissible build`. First-send preflight repeats this check. Missing or stale authority at a present yard is not permission to spend elsewhere. Existing agent dismissal and regional cooldown rules remain in force. Check the recorded `expansion_agent` decision before expecting a hire. The [guide, Resident Factors](https://tradewinds.kibb.io/skill.md) describes their access, fee and daily upkeep.

The durable `__growth` intent builds the hull turnkey, arms it at the yard, records the commissioning settlement and closes. The hull then joins normal planning; idle maintenance buys its stores. If the yard evicts the hull to the roadstead before it is armed, the intent docks it again. If the yard region can no longer pay for the guns, the hull holds unarmed in port instead of trading. A definitive refit refusal or three refit attempts releases the hull to normal operations, where maintenance arms it later. Growth decisions record the chosen candidate's capital, buffer, rate and payback, or the blocking reason.

Immediately before the first send, the runner fetches a live turnkey quote, checks the admitted ceiling and uses the reserved quote cushion before requiring extra yard-region funds. The player-sourced material share must fit within the remaining price headroom. A live quote that fails any of these checks backs off that yard/class for 6 real hours, so the same build is not readmitted every pass. The game has no atomic maximum-price field: another company can still change asks between the quote and the build. A receipt above the admitted ceiling records both prices, warns once on fresh dispatch with scrubbed outcome fields, backs off that yard/class for 6 real hours and pauses all capacity growth for 24 real hours. A domain build refusal also backs off the yard/class for 6 real hours; throttling and authorization errors (401, 403, 429) only wait for their normal cooldown.

#### Ship mortgages

The [guide, shipyard and loans](https://tradewinds.kibb.io/skill.md), read on 2026-10-28 for game 0.5.1, describes a 60% build mortgage, or 80% for oceanic hulls, over 180 game days with installments every 10 game days. The ship has a lien with recourse. It may trade, but cannot be listed, sold or pledged again while debt is owed. Only one owing mortgage is allowed, including defaulted debt. Automated capacity growth remains cheap-hull-only; an oceanic mortgage percentage is not permission to buy a barque.

Check `mortgage.available`, principal, installment and total repayable in the yard quote. The [controller](../internal/engine/loans.go) protects two installments in that region and requires the measured per-class coverage above. Reserve-safe cash funding wins when remittance fees are no greater than mortgage interest. After a build, confirm `is_mortgage: true` in `GET /v1/loan/list` and the ship ID in `collateral_ship_id`. Check `next_payment_due_at`, `next_payment_pence`, `arrears_pence`, `arrears_due_by` and regional funding. Scheduled collection belongs to the server. Surplus prepayment never repays mortgages.

An ambiguous mortgaged build requires both the named fleet hull of the expected class at the yard and our mortgage loan whose `collateral_ship_id` matches that hull's ID. A loan or hull alone is not proof. Do not retry with a new build identity because only one appeared. A mortgage-still-owed conflict waits for authoritative loan refresh; do not clear `_mortgage_refused_at` or repayment records. Keep mortgages separate from the older capacity-loan package.

#### Surplus loan prepayment

The runner services debt and protects operating obligations before considering growth. After no capacity candidate or live savings target wins, it may pay an active owned non-mortgage loan in full. The loan's own region must retain committed reserves plus `(non-sunk fleet size + 1) × cargo-cash lot`, not counting that loan's own installment reserve, and all other regional reserves must remain funded. The request omits the amount; cash for the balance plus one game day of interest must be available. It does not send a remittance merely to prepay. Read the `__loans` mission and immutable `loan-repay` receipt, then confirm fresh loan and vault captures before expecting another payment. An observed closed loan releases debt servicing even if the receipt was lost. Do not lower reserves or clear a savings target to force repayment. Principal repayment is financing outflow, not an operating loss; only interest and actual transfer fees are expense.


#### Passenger offers and receipts

Use the [guide, passengers](https://tradewinds.kibb.io/skill.md) and [OpenAPI](https://tradewinds.kibb.io/api-docs/openapi.json) for endpoint fields. `GET /v1/passengers/waiting?port_id=` shows whole groups and `board_by`. `GET /v1/passengers/fares?from=&to=` shows accepted fares, not guaranteed future demand. `GET /v1/passengers/offers` shows our open offers; add `open_only=false` when checking why an offer closed. `GET /v1/passengers/aboard?ship_id=` shows agreed fare and arrival promises.

The [passenger controller](../internal/engine/passengers.go) buys required provisions before going offshore to post offers. It holds each ladder price for one game hour when time permits. It cancels and reposts to step down, never below its recorded cost floor. With no more than 3 game hours before `board_by`, it starts at the floor. This is not a guarantee that a delayed replacement will reach the floor before the deadline. Check the immutable `fare_per_head_pence`, whole-group berth capacity and persisted floor before diagnosing a refusal. An offer-exists conflict reconciles through the offers read, not another POST. Aboard groups without a matching passage record hold departure. Once matched, boarded groups depart even if their promise is already late; lateness alone must not strand them.

Inspect recorded `passengers_boarded`, `passengers_paid` and `passengers_lost` events in history. Journal rows expose heads, fare, destination, paid fare, lateness or loss cause. Known `passage` proceeds must match authenticated terminal receipts: sum `fare_paid_pence` across the boarded batch; loss proceeds are zero. Compare that receipt with the destination-region ledger and attributable expenses. The [receipt reconciler](../internal/engine/passenger_reconcile.go) releases the ship after 2 game hours of authoritative empty holds and no aboard groups at the destination when terminal events are missing. That settlement has `proceeds_unknown: true` and unknown costs. It is not zero-income evidence. Late matching events can upgrade the archived settlement without duplicate income while it remains open. Offers and public fare hints are not receipts. No accepted fares were observed in the measured 2026-10-28 audit, so passenger earnings remain an estimate.

The [receipt scheduler](../internal/store/passenger_receipts.go) scans at most 32 due rows per batch and persists a 15-real-minute backoff before recovery work. After seven real days it closes an unresolved receipt as `expired`, leaving proceeds unknown. Check decisions `passenger_receipt_deferred`, `passenger_receipt_expired`, `passenger_receipt_quarantined` and `passenger_receipt_quarantine_failed`. A closed receipt does not resume scanning after restart. Do not infer income from its expiry.

The [passenger error handler](../internal/engine/passengers.go) holds and retries transient failures with the original intent. Only integrity failures quarantine, including malformed evidence and conflicting identities. Find `passage_quarantined` in System > Decisions or `/api/history?account_id=...&kind=decisions`, then match its ship and `mission_id` to the Journal, aboard groups and terminal events. The [quarantine recovery path](../internal/engine/passenger_quarantine.go) archives damaged accounting separately and uses fresh aboard groups and offers to recover the hull. It cancels open offers, provisions as needed and safely carries uniquely identified groups sharing a destination. It does not convert damaged accounting into fare income. When fresh aboard and offer reads prove the ship empty with no open offer, it releases the operating mission and closes its archives. Fix missing authority, unsafe routes or unavailable provisions if recovery holds. Do not delete evidence or clear `passage.quarantine_reason` to force movement.

There is no dashboard or HTTP quarantine-clear operation. The [archive store](../internal/store/passenger_quarantine.go) preserves unresolved recovery without age expiry. Closed quarantine archives are retained for seven real days after closure, then [pruned](../internal/store/telemetry.go); ordinary mission reads do not scan them. Take a quiesced backup before evidence repair, including when older closed evidence is needed. These quarantine archives are separate from `passenger_receipts`. Closed passenger receipts remain immutable and have no reopen operation.

The [fare capture gate](../internal/engine/passenger_capture.go) survives failed captures, brief departures and restarts. Its lane data expires only after 10 authoritative game hours with no relevant waiting lane, open offer or aboard group. Failed waiting reads do not prove that absence. Inspect `_passenger_fare_hours` and `_passenger_fare_irrelevant` before treating a deferred fare read as a stalled worker.

#### Hazard quote operations

The [guide, marine insurance](https://tradewinds.kibb.io/skill.md) publishes Lloyd's expected-claim upper bound. Inspect selected economics fields `hazard_bps`, `hazard_pence` and `hazard_source`. The [quote controller](../internal/engine/hazard.go) ranks requests and caps each refresh at 30 GETs. Direct hull bounds last 6 game hours and laden lane bounds 30 game days under matching guns, month and piracy observations.

An owned-hull lane proxy is labelled. The quoted hull must have no more cannons and no higher hull-category rank than the priced ship. Its route-zone piracy observations must be at least current piracy. Choose the highest eligible quote. After a failed fetch, an eligible published lane bound can be used for up to 90 game days, with stale age disclosed, only within the same winter or nonwinter storm season. Winter is November through February. An unseen lane during an outage remains unavailable. Check `_hazard_quotes` errors and observation ages before treating a no-trade hold as a stuck worker. Do not replace a missing bound with a storm rating, zero risk or an invented probability. Initial cargo admission still needs the controller's 2% purchase-plus-customs margin. Under the [funded continuation rule](../internal/engine/income_funding.go), already-paid fees are sunk: remaining net must be nonnegative, without a second margin gate. Final accounting still charges those fees.

### Idle work and regional funding

There is no minimum shipment-profit target: the runner already considers smaller positive-contribution loads. Ordinary voyages may fund their full cash requirement from reserve-safe regional donors, after observed transfer fees. In System → Decisions → Route and economics, “Regional funding fees” is a modeled expense already included in the displayed contribution; transferred principal and collateral are not profit or expense. Older records without the field remain unchanged.

When no ordinary income is executable, six observed idle game hours permit a limited source reinspection. Maintenance does not erase the idle clock; actual sailing, cargo, active freight or changing ports resets it. Stale or paused observations and long restart gaps earn no hours. The current hold identifies whether the ship is waiting for idle eligibility, reinspection cooldown, risk allowance, safe funding or usable market evidence. This is not a promise that a profitable order exists at the next port.

The [rolling exploration allowance](strategy.md#destination-replanning-and-bounded-sourcing) is `clamp(20,000 + floor(conversion net / 2) - probe expenses, 0, floor(free cash / 50))` pence over 60 game days. Conversion net is known-cost delivery net from a source probed within seven real days before the delivery started. Scouts do not consume it. Unknown in-window probe costs close it. Repeating unchanged evidence still needs the idle gate and a 24-real-minute completion cooldown. Never reset settlements, idle records, keys or reserves to force another attempt. See the [budget implementation](../internal/engine/probe_history.go) and the [guide, visibility](https://tradewinds.kibb.io/skill.md).

If ordinary trade and charter sourcing both reject, 12 coherent idle game hours permit a ballast scout. Designate the smallest-free-hold co-located ship, then ship ID, only among mission-free ships whose own admission, independent berth/convoy state, route, stores, budget and insurance departure checks pass. An insurance-compatible lower-ranked accepted target is eligible. Only one relocation may be active fleet-wide, including ambiguous requests. Exclude ports occupied by our other stationary ships, final destinations of other sailing ships, other unsettled non-maintenance mission destinations and complete quay books observed less than 6 game hours ago. Last-good quay timestamps survive departure and are retained for the longer of 24 real hours and 168 game hours, unless the port leaves the catalog. A hint at or before our last complete quay observation is discarded.

Rank hinted targets by positive public spread less both-way exposure. Target provisions production or ask gossip is no longer required. The separate scout allowance is `floor(free cash / 50)`, or zero when permanent probe costs are unknown. Legal routes, both-way safe stores, live hazards, funding and the two-unconverted-scout fleet stop remain. If no hinted scout is accepted, at least two mission-free stationary ships at one port can send the smallest-free-hold eligible hull for a book refresh after six coherent idle game hours. `_book_refresh` limits starts to one per 24 game days. This buys visibility, not forecast profit. See the [scout controller](../internal/engine/scouting.go), [strategy](strategy.md#destination-replanning-and-bounded-sourcing) and the [guide, quay access](https://tradewinds.kibb.io/skill.md).

### Underwriting

Money shows owned underwriting exposure and settled outcomes above financial history; public policy listings remain in Trade, under Exchange and Insurance. An active premium is an entitlement, not settled earnings; returned collateral is principal. The engine may take a 500-basis-point hull-only slice only when its directional closed-policy model has positive net after modeled claims and shipping opportunity cost, and the full possible loss fits both current protected cash and the permanent pilot allowance. See `docs/strategy.md` for model assumptions and limits.

When an open policy carries Lloyd's expected-loss estimate (`lloyds_expected_loss_bps`), that estimate replaces the closed-policy model: the slice's premium share minus collateral times the estimate, minus the same opportunity cost, must be positive and `premium_margin_bps` must be at least 50. The estimate is an upper bound on the expected claim, so this is conservative. Without closed-policy history, collateral lock time comes from the route's sailing days, else the 15 game day policy lapse. Cargo-inclusive policies stay excluded because partial claims are not yet accounted per position. Lloyd's fields are kept out of the policy terms hash, so they never mark a stored policy as changed.

`underwriting_selected` records the estimate. Confirmed `game action committed` records the exact owned slice, collateral and premium entitlement; `underwriting settled` records earned premium, claims, returned principal and net separately. Missing receipt, changed terms or unexplained ownership remains unresolved. After an accepted slice, further underwriting waits for a unique identity-linked debit to an owned vault. This hold does not disable unrelated safe fleet work. Do not replace an ambiguous idempotency key or infer payment from policy coverage.

### Royal monopolies and gossiped player bids

Each refresh reads the public monopoly list. While another company's monopoly is in force for a commodity in a banking region, Crown asks for it in that region are dropped from planning books; player asks remain. If the list cannot be fetched, the last good list is kept and entries past `expires_at` are dropped. A purchase refused with a monopoly `409` therefore points to a monopoly granted after the last refresh.

Spot plans may sell a whole lot into a gossiped player bid at a remote port where we have no live book. The usable tons exclude our resting bids, our sales there since the observation and other ships' undelivered cargo to that buyer. Gossip's best bid is the whole book's best bid, so a non-player floor is known only when it is above the player bid; the lot is admitted only then, and only if it stays positive with every ton valued at that best bid less 25%. When the player bid is the top of the book, gossip still prices one ton. The player order is anonymous and can be filled or withdrawn before arrival; on arrival the ship sells into whatever bids are live. Where a live book exists, it always wins over gossip.

### Letters of marque

To enable or disable issuance, stop the bot, set `MARQUES_ENABLED=true` or `MARQUES_ENABLED=false` in the run directory's `.env`, then restart with the normal deployment command. An inherited environment value overrides `.env`, so remove or update that value too. Default is `false`. With issuance disabled, marque-only quotes and exposure-route reads stop and a never-sent intent is dropped; attempted issues still reconcile and active zones remain protected. Disabling does not cancel an issued marque.

With the switch on in execute mode, the runner quotes 500 bps for 60 game hours at most every 30 real minutes. It allows one active marque, estimated prize at least 1.5 times cost, at most 5% of liquid cash and at most 100,000 pence lifetime unrecovered spending including the new issue. Three expired marques that each earned less prize than their own cost permanently stop issuance. Count losses over its lifetime, not just consecutive losses. Check `_marque_outcomes`, `_marque_active` and `_marque_stopped`; never clear them to restart the allowance. The zone must be outside our fleet and underwritten exposure. The home vault retains reserves and commitments, and no growth intent may be pending. A fresh send quote must pass all rules and cost no more than the admitted quote, not 10% more. The game has no atomic maximum-price field. A domain refusal backs off that zone for 6 real hours.

Exposure covers our ships' current legs, persisted mission routes including return and recovery legs, the ports where our ships lie, the home port and routes of policies we underwrite. While our marque is in force or pending, new voyage plans and sourcing relocations avoid its zone; loaded voyages already planned are not held, to avoid stranding cargo. Prize payments arrive as `marque_prize_paid` events and in `fleet/marques` as `prize_earned_pence`.

### Covered futures pilot

Stop the bot, set `FUTURES_PILOT_ENABLED=true` in `.env`, then restart in execute mode to permit pilot admissions. Set it to `false` and restart to disable new leases, procurement and listings. Check inherited environment overrides as for marques. Disabling is not cancellation: the game has no futures cancel endpoint. Existing contracts still reconcile and settle, and owned pilot stock and leases wind down. Unknown actions retain their original request and key; a hold in this optional pilot does not block fleet work.

The pilot waits for ordinary Amsterdam presence and spare protected cash. It does not redirect a ship or hire a factor. Its permanent cohort permits only one 1 t `water_beer` ask and one 1 t bid, with 30-game-hour maturities, a 2 t lease and a 30,000-pence exposure cap. Strike is 85% of the observed NPC midpoint for the ask and 115% for the bid, rounded down in pence. This is a mechanics experiment, not a positive-spread forecast. The ask needs owned unlocked stock; the bid needs full strike cash, margin, customs sensitivity and receiving space. It never accepts another listing, replaces an expired quote or scales up. Do not delete `__futures` or ledger rows to repeat it.

A definitive setup rejection or a zero-fill stock order stops admissions. Successful wind-down changes the retained `__futures` mission to `futures_done`; neither a restart nor a new binary starts a replacement. If the pilot never admitted a future and never acquired stock, an operator can explicitly re-arm that unused setup after fixing the cause:

1. Let the existing runner finish closing its pilot lease, then stop the bot and wait for its account lock to release. Confirm the game has no pilot futures or remaining pilot stock.
2. From the configured run directory, run `tradewinds accounts` to obtain the exact account ID, then run `tradewinds futures-rearm --account <account-id>`.
3. Restart normally with `FUTURES_PILOT_ENABLED=true` only when the same capped experiment is still wanted.

The command makes no game requests. It holds the normal account/company executor lock and requires `futures_done`, no staged or pending action, known-zero procurement/liquidation and no `futures_intents` rows at all, including rejected or terminal admissions. It preserves the permanent cohort, action sequence and journal; only unused setup state and its cooldown are cleared. A refused command leaves the mission unchanged. Any admitted, created, settled or defaulted future is ineligible: never delete evidence or clear claims to bypass this guard. The spot-order API requires lowercase `time_in_force: "ioc"` (`"gtc"` is the other documented enum); uppercase `"IOC"` is invalid, not a retryable market condition.

A setup retired before sending, such as disabling the pilot after staging its first lease, can also be explicitly re-armed after wind-down. It need not have a last sent action: the unused-stock, no-intent and no-pending-action checks still apply, and its advanced action sequence is retained.

Read `futures_intents` and `futures_cash_flows` with a read-only database session. For example, substitute the recorded account ID:

```sql
BEGIN READ ONLY;
SELECT intent_id, cohort_id, side, status, futures_id,
       reserved_tons, reserved_cash_pence, margin_pence, exposure_pence,
       observed_matures_at, cash_delta_pence, realized_pnl_pence, pnl_basis
FROM futures_intents
WHERE account_id = '<account-id>'
ORDER BY created_at, intent_id;
SELECT intent_id, ledger_id, cash_delta_pence
FROM futures_cash_flows
WHERE account_id = '<account-id>'
ORDER BY intent_id, ledger_id;
ROLLBACK;
```

All amounts are pence. Null cash or P&L is unknown, not zero. `cash_delta_pence` is signed measured contract cash, not sale profit. Cohort P&L combines matched vault/escrow ledger movements with actual pilot procurement and liquidation receipts once its physical stock cycle closes, including single-leg cohorts with a known cost basis. It is recorded once, on the ask when present, with zero on other rows to avoid double counting. Unpriced opening stock or an inventory-only trade reconciliation leaves P&L unknown; warehouse rent stays labelled unknown. A default releases the lease and stock after its physical, penalty and claim evidence is recorded. Only a validated post-default own `/v1/loan/credit` capture reporting zero open futures claims clears default liability. A matched penalty debit identifies our default but leaves its separate unpaid claim reserved. Positive own claims replace duplicated per-contract estimates and stay protected until a later valid reduction, without a freshness timeout; failed or missing captures retain the last validated amount and conservative coverage for defaults newer than it. Own credit is fetched only for unresolved default claims, at most once per five real minutes, with attempts and last valid claims retained in `futures_credit_evidence` across restart. These own-company GETs do not consume rival lookup budgets or create rival records. Settlement receipts, net cash movements and fresh vault balances alone do not identify the debtor. Missing list rows never prove expiry. Stale or paused clocks, books, cash dips and epoch races release unsent actions without ending the admission; precondition releases persist a retry cooldown and let other account work continue in the same pass. Setup and procurement both require the same spare 300 Cr allowance at staging and dispatch. Immutable-term or cap violations, including a bid reserve overtaken by customs, stop the admission. Expired, stopped or disabled admissions release reservations only with positive no-send proof. Pending stock orders retain their original key and bounds; fresh warehouse inventory and unique matching private fills reconcile unknown receipts, never invented cash. A lease raised externally above 2 t holds staging until operator reconciliation; malformed third-party list warnings are limited to once per minute. Definitive authorization, database and journal-integrity errors still stop execution. Setup lease `403 no_port_presence` instead clears the setup attempt and pauses through a persisted cooldown, allowing a bounded retry after ordinary Amsterdam presence returns. A definite settle or wind-down refusal waits for changed evidence for that exact contract or warehouse before another request.

The own-credit capture is dated by the conservative server lower bound reserved before its GET, not the host clock. A zero predating a later server default cannot release that default's reserve; missing server timing leaves coverage conservative. Setup lease `403 no_port_presence` waits at least one game hour before another bounded attempt, using the normal refusal cadence rather than a five-second retry. Own-credit failures remain visible as company-level futures holds in the completed pass's status, without stopping fleet work. A successful check clears that hold once no credit read is awaited; a failed read retains it through the retry cooldown.

### Underworld observer key

For an existing registered account, stop the bot first and wait for its account lock to be released. Then run this command from the configured run directory before restarting the bot:

```sh
go run ./cmd/tradewinds underworld-key -handle Yukine
```

The compiled equivalent is `tradewinds underworld-key -handle <handle>`. It uses the exact stored handle and passphrase, creates a separate company-bound key with `[read, underworld]` scopes, and stores it encrypted without replacing the main automation key. When an observer key is already stored, the command verifies it with `GET /v1/auth/whoami`. Only an HTTP 401 with code `revoked_api_key`, `expired_api_key` or `invalid_api_key` replaces it; a valid key or any other verification outcome leaves it unchanged. It never registers a new account. Keep the credential key backup and do not print either API key.

The runtime observer is GET-only. At most one wants read is reserved per ship visit at a port, plus four contraband-book reads at a cove. Berth, roadstead and other same-port status changes do not reset the visit; an authoritative departure or port change does. A failed read consumes that visit's allowance, including across restarts. `underworld_visits`, `underworld_read_attempts` and `underworld_observations` retain visit, budget and response evidence. Missing credentials, a permanently rejected key, HTTP 403 with code `missing_scope`, or observer storage/payload failures log `underworld observer disabled` for the process without stopping trading. A rejected key logs `observer key rejected; run underworld-key` and causes no further observer reads in that process. Other 403s skip only that read, not the whole observer. Check port presence and the logged failure; do not delete attempts to retry a spent allowance.

To replace a revoked, expired or invalid observer key, stop the bot and run `underworld-key` as above, then restart the bot. Each logical mint gets a fresh random identity persisted with the encrypted credentials before sending; the journal retains that request's idempotency key and immutable body across interrupted runs. A later replacement uses a different identity, avoiding cached responses from an earlier mint or another database. A verification failure that does not prove permanent key rejection is not permission to mint again; investigate it first. The command does not repair missing scopes on an otherwise accepted key. Do not edit encrypted credentials or delete the bootstrap journal. The scope itself is not read-only at the game boundary. Never use it to buy contraband or start a run.

An unresolved observer create-key journal from before random mint identities blocks a new mint: reconcile that legacy action first, without deleting it or bypassing its original idempotency key. A durably journaled, definitive non-429 rejection retires the current mint identity so a corrected next invocation can mint with a fresh identity. Ambiguous outcomes, rate limits and failed journal writes retain the identity and its recovery rules.


### Ships with voyage insurance

The engine does not buy policies for its own ships, but an owner can insure one through the game. Such policies appear on the general insurance board; `insurance_owned` records this company's underwriting of other ships, not its own voyage cover.

Load the intended cargo before taking out a policy. Once an own `open` or `active` policy is observed, the engine holds new cargo purchases and freight acceptance for that ship. Provisions bought directly into stores, deliveries, sales and safe maintenance remain available. Matching loaded departures and empty source-inspection voyages are still permitted; a conflicting origin or destination holds the departure and any new source-inspection funding. Other ships continue working, and an insurance-held idle hull is not treated as a reason to buy replacement capacity.

Source inspections use the highest-ranked candidate compatible with every observed live policy, not simply the first candidate. If no candidate matches, the ship waits; stale policy evidence is not permission to sail.

An insurance hold appears with the ship's current reasons in Overview and its recorded decisions. Inspect the policy's ship, origin, destination and REST status. Wait for a matching legal insured voyage or an authoritative terminal/absent policy observation; do not clear the journal, modify policy timestamps or infer expiry from local time. A retained live policy continues to block loading when refresh fails, and departure waits for fresh policy evidence. A never-observed policy or an unavailable board alone does not globally disable uninsured trading.

An insurance-held, wholly unattempted cargo plan does not keep hypothetical cash, source stock or charter capacity away from eligible ships. Paid cargo, accepted freight, commissioning, return commitments and unresolved requests remain protected. A later admission uses current funds and depth, not the obsolete quantity. Closed, unaccepted optional freight is pruned before applying the loading hold; already-carried cargo can still depart on its matching insured route.

Protection is based on cached REST observations, rechecked before staging and first send, including after a rate-limit wait. It cannot detect a policy created externally after the last observation, nor prove the completeness of an earlier declaration without its original manifest. Avoid changing insurance while the controller is staging work. Already-attempted ambiguous actions retain their immutable payload and idempotency key; completed responses are consumed normally. Recovery can therefore finish a previously attempted cargo action rather than pretend it never happened. Reconcile the actual cargo and policy in that case before relying on coverage.

The published contract does not yet establish how captured ships appear in fleet status or ownership events. Missing fleet rows and cancelled insurance are not proof of a paid claim or returned collateral. Keep those outcomes unresolved until authoritative evidence establishes them; see the piracy compatibility audit in `docs/strategy.md`.

### Idle store replenishment

A low-store ship can dock once to obtain executable provisions prices unavailable at roadstead. If the complete replenishment cannot be funded without spending regional or incumbent reserves, its durable maintenance intent waits in berth and re-evaluates recorded REST observations. It does not repeatedly go offshore and redock. Berth dues and crew consumption still accrue; a wait is not cost-free.

Current company reasons identify missing quay depth, unavailable rates, insufficient reserve-safe funding or hold capacity. Once measured donor liquidity covers the full current quote, the ship stages a remittance, refreshes, then buys stores. The donor's reserve and funded store money are protected from competing work. Logs show actual transfers and purchases; remittance principal is not a fee or profit.

A fresh price or stock change can cancel a never-sent order without releasing its still-needed funding. That money remains protected until the observed store need is satisfied. Waiting idle-maintenance quotes do not block each other from using scarce executable provisions; attached requests retain their depth, and other spending still respects all maintenance cash commitments.

Restarting, a lost docking receipt, a partial/zero fill or a definite refusal does not discard the replenishment purpose. Pending requests keep their immutable identity; only proven-unsent requests can be replaced. Fresh observed stores satisfy and retire maintenance, including an external replenishment. Do not delete the mission or lower reserves to force progress.

Sufficient stores finish the standalone replenishment intent even if optional guns remain unaffordable. Gun-only maintenance checks cash and hold capacity before docking; it does not keep an otherwise viable income voyage trapped behind an optional refit. Mandatory armament for newly commissioned ships is unchanged.

### Console logs

Zap writes `[time] [account] [category] [LEVEL] message` followed by the complete structured context as JSON. Root, store, API, engine and Fx records have named categories; account labels and IDs remain in the JSON even when the label appears in the header. `LOG_LEVEL=info` is the default; invalid values stop startup before opening the database.

- **INFO:** service startup, one initial fleet snapshot, blocking-reason transitions and bounded blocked-work heartbeats, confirmed actions, authoritative arrivals, established stream/dataset recovery, recorded settlements and `engine warning cleared` with `active_for`. Amounts are integer pence; unknown costs never appear as measured profit.
- **WARN:** a new recoverable outage, unexpected rejection, reconciliation required, explicit ship loss, a storage/diagnostic failure needing attention, or an active [engine warning](#engine-warnings). Engine warnings raise once and persist at 30-minute intervals; changing facts alone do not raise them again. An ordinary handled conflict is not automatically a warning. Repeated unresolved-action warnings use the bounded seven-day decision cache and can recur after eviction or restart; dashboard read/write warnings are sampled per operation.
- **ERROR:** terminal account-worker, ownership, service, lifecycle or response-encoding failure. Expected shutdown cancellation is silent.
- **DEBUG:** request attempts, routine dispatch, handled conflict/cooldown detail, repeated connection failures, Fx lifecycle and migration detail. Enable temporarily for diagnosis; successful dock/offshore actions remain INFO.

`LOG_COLOR=auto` (also the unset/empty default) enables colors only when stderr is a terminal and `TERM` is not `dumb`. `LOG_COLOR=always` also colors redirected output, useful for a visible PowerShell window that tees logs to a file. `LOG_COLOR=never` disables colors. The presence of `NO_COLOR`, even with an empty value, overrides every color mode. Other `LOG_COLOR` values fail startup. Levels receive distinct colors and structured context is dimmed; stripping the encoder's SGR sequences leaves the same records as plain mode. Control characters in upstream messages, labels, categories and fields are escaped so they cannot inject terminal lines or commands; invalid UTF-8 is normalized when represented as JSON.

The runner emits `fleet status` once per account after the first coherent, persisted fleet observation. It does not emit a minute heartbeat. Stale or missing initial fleet data does not consume that announcement. `reconciled_at` is the completed cycle time; `fleet_observed_at` is when the fleet was fetched. Counts describe that observation: `in_port` includes berths and roadsteads. The first holding reason is scrubbed before a 512-character cap; the count is reasons, not waiting ships.

Subsequent fresh REST transitions from sailing to a berth or roadstead emit `ship arrived`; an explicit transition to sunk emits `ship sunk` at WARN. Startup history, a passed ETA, missing rows and stale snapshots never invent those events. Missing rows also do not erase the previous sailing observation.

`work blocked` reports a new or changed final real blocking-reason class for each ship or account control (`__growth`, `__treasury`, `__loans`, `__underwriting`, `__marque`) after that class persists for five real minutes. Classes use the stable headline before `; best evaluated` or `; sourcing:` and normalize runs of digits, periods and commas, so changing alternative details or amounts do not restart the delay or suppress heartbeats. Logs retain the full current reason. Fields include `ship_id`, ship name, `port_id`, reason, `blocked_since` (UTC) and elapsed real `blocked_for`. Multiple rejected alternatives in one work pass produce only its final reason. An announced unchanged class is quiet between `work still blocked` heartbeats, at most once per 30 real minutes; polling does not log every 15 seconds. A real reason change preserves the continuous blocked interval. `work unblocked` reports that interval's duration only if a blocker was announced and the subject is assessed without another real hold, including a transition to an expected wait. A later real stall starts a new blocked interval. An early return for another ship's progress or an account gate does not falsely clear an unassessed ship; its last known announced blocker can still emit a heartbeat. A ship absent from a fresh fleet observation is forgotten without an unblocked event; stale observations retain its last known blocker. Blocking-log tracking restarts with the process, while the continuous economic growth-wait timer persists. Neither changes decision-record deduplication. Other healthy polling stays quiet. Silence is not proof of health or profit: use recorded observation times and current company activity in Overview.

An idle ship with no accepted ordinary voyage keeps both rejection reasons: `no profitable executable voyage from hamburg; best evaluated alternative <commodity> to <destination>: <ordinary rejection>; sourcing: <top rejection>`. The combined hold omits the ordinary alternative's quantity and estimated economics; candidate decisions retain those details. When scouting is considered and rejected, append `; scouting: <top scout rejection>`. A single hold decision per ship keeps identical passes deduplicated. An accepted sourcing or scouting plan blocked by observe mode, insurance, store-risk evidence or a vanished contract keeps that specific execution hold instead.

Authoritative ordinary and sourcing voyages waiting for arrival are expected waits, not blocked work, while the current server clock and voyage ETA are known and arrival is no more than five real minutes overdue. The first observed ETA is persisted in the mission; later ETA extensions cannot extend the grace, and a new departure resets it. Beyond that grace, or without a usable ETA or clock, the wait is a real hold with the normal five-minute announcement delay and 30-minute heartbeat.

Economic capacity-savings, capacity-growth and expansion-agent admission or regional-funding verdicts also wait quietly, but become real blockers after six continuous real hours. Waiting for a first observed regional hull quote and the deliberate 24-hour ship-loss cooldown are economic waits too. Their `_growth_wait_since` timestamp survives restarts and resets after an assessed growth pass without an economic wait; an escalated log's `blocked_since` and `blocked_for` include the initial quiet interval. These waits still appear in `_engine.holds` and ordinary hold decision rows. Missing authority, undated or future-dated ship-loss evidence, future-dated settlements, a missing surviving hull for an unknown-cost closeout, a negative vault and a missing home billing region are real integrity holds, not economic waits. Commissioning an already admitted hull, loan servicing, destination procurement recovery, sealed-freight shortfalls, no profitable voyage, turnaround locks and source inspection waits remain real holds.

Upstream strings in status and outcomes use the store's credential and token scrubber; if credentials cannot be read, strings are redacted while numeric facts remain. Credentials, raw game payloads and database connection strings must not appear at any level. Business outcomes are not message-sampled: two ships completing the same action must produce two distinguishable records.

Console output is best-effort, not an exactly-once financial ledger. Completed-response replay is quiet; authoritative recovery is explicitly labelled and omits unknown amounts. A crash after durable advancement but before stdout may omit a console line. Use the company Journal, action journal and permanent settlements for accounting and recovery.

#### Game-action log audit

Each confirmed mutation needs an INFO outcome, not an INFO request attempt. All outcomes carry the durable action identity and stage plus applicable company, ship and port context. The table records why the log exists and its additional validated fields; no estimated amount is presented as an actual receipt.

| Action | Operator purpose | Additional outcome fields |
| --- | --- | --- |
| Buy/sell IOC cargo | Explain inventory and cash changes, including zero/partial fills | Side, commodity, actual tons, gross pence, customs |
| Buy provisions into stores | Explain departure readiness and cash use | Port, provisions, actual tons and cost |
| Accept freight | Expose the new sealed obligation | Contract, destination, tons, reward, collateral, deadline |
| Deliver freight/charter | Show confirmed delivery income | Contract, destination port, delivered tons, validated reward |
| Depart | Show where the ship is going | Origin, destination, cargo and known berth dues/debt |
| Dock/offshore | Explain a berth or roadstead transition | Ship, port, action identity; known charges only |
| Build | Explain capital investment | Yard, new ship identity/name/class, validated cost |
| Refit | Explain armament and hold changes | Ship, requested cannon count; unknown cost explicitly unknown |
| Change company settings | Explain altered billing behavior | Auto-remit policy and reserve |
| Remit treasury cash | Explain regional liquidity movement | Source/destination region, gross, fee and credited pence |
| Underwrite a policy | Explain accepted capital exposure | Policy, insured ship, origin/destination, exact owned slice ID, slice basis points, collateral and premium entitlement |
| Settle underwriting | Distinguish earned premium, claims and returned principal | Policy/slice IDs, terminal state, earned premium, claims, principal returned and net |
| Finish a mission | Distinguish earnings from turnover | Known costs, actual proceeds and loss-inclusive realized net |
| Recover a committed action | Explain restart/ambiguous-response recovery | Recovery label and authoritative state, never guessed money |
| Register company/account or create key | Announce a usable account, not each auth attempt | One `account ready` with handle/company; never key or passphrase |

Fresh sends and safe retries are DEBUG. Handled berth conflicts, tender waits and rate-limit cooldowns are DEBUG with the persisted classification/deadline; unexpected rejections remain WARN. HTTP 409 alone never determines severity or proves a harmless failure. Ambiguous requests retain their original identity and require reconciliation. Registration intermediates stay in the redacted request journal; the CLI reports a terminal bootstrap failure.

#### Diagnostic log audit

| Existing path | Decision and context |
| --- | --- |
| Account incomplete or missing key | WARN with account identity and `register` recovery instruction |
| Credential load failure | ERROR with safe cause; other accounts continue |
| Account worker exit | ERROR with account/company and safe cause; cancellation silent |
| Dashboard listener/server exit | INFO address/mode and real VCS revision/modified state at startup; ERROR unexpected stop |
| Request diagnostic persistence | First outage WARN with method, redacted path, status and safe cause; repeated failures quiet, recovery INFO, shutdown silent |
| SSE disconnect/gap/recovery | First outage/gap WARN, repeated disconnected attempts DEBUG, persisted recovery INFO; owner loop emits lifecycle messages |
| Dataset or contract-page fetch/validation | First outage WARN with dataset and safe cause; repeated outage quiet; successful recovery INFO |
| Account lease loss | ERROR account, ownership state and safe database cause; cancel execution immediately |
| Lease connection/pool cleanup | WARN with safe cause, not a content-free warning |
| Migration lock release | WARN safe database cause |
| Goose migration detail/failure | DEBUG normal detail, ERROR fatal migration; no raw connection strings |
| GORM SQL logging | Disabled; returned errors are handled at their operational boundary |
| Dashboard reads/metrics/writes | Sampled WARN operation/cause; expected request cancellation silent |
| Engine warning assessment | WARN once per active account/code, WARN persistence every 30 minutes, INFO cleared with `active_for`; read failures WARN once per streak, cancellation silent |
| Dashboard response encoding | ERROR: response cannot be represented safely |
| Game HTTP request trace | DEBUG method, redacted path, status and duration; no body/header dump |
| Rate limiter/retry transport | No duplicate business log; owning action/dataset reports the classified result |
| Retention | WARN failed prune and safe cause; no success heartbeat |
| Fx lifecycle | DEBUG detail; terminal failures ERROR |

The severity policy follows [OpenTelemetry](https://opentelemetry.io/docs/specs/otel/logs/data-model/), [OWASP logging guidance](https://cheatsheetseries.owasp.org/cheatsheets/Logging_Cheat_Sheet.html) and [Zap levels](https://github.com/uber-go/zap/blob/master/zapcore/level.go). [OpenKore](https://github.com/openkore/openkore/blob/master/src/Log.pm) separates operational messages and debug domains; [KoLmafia](https://github.com/kolmafia/kolmafia/blob/main/src/main/java/net/sourceforge/kolmafia/RequestLogger.java) separates readable output from session records; [Baritone](https://github.com/cabaletta/baritone/blob/f2679bea294e51e3d2e83bda562451f8f9d2cf5a/src/api/java/baritone/api/utils/Helper.java) distinguishes direct and debug notices. These support separate operator and diagnostic channels, not copying their UI or adding log knobs here.

A disposable PostgreSQL/real-CLI observation ran for 77.9 seconds through six completed cycles: one startup snapshot, one arrival, one explicit loss, colored output, no leaked fixture credential and no game mutations in observe mode. Ctrl+Break exited successfully without an account-failure error. CLI fixtures sharing the same account or company identity must run sequentially even in different schemas: PostgreSQL advisory ownership is database-wide. This establishes the exercised lifecycle behavior, not live profitability.

### Engine warnings

Warnings read recorded database evidence only. They do not call the game, pause or reconfigure trading, repair state, or prove profitability. **No engine warnings is not proof of profit.** Missing evidence stays unknown; unavailable warnings are never an all-clear.

The following thresholds use real elapsed time, not accelerated game time. Disabled accounts, missing company registrations and registrations without an API key are excluded from assessment and listed in `unmonitored [{account_id, reason}]`, with reason `disabled`, `unregistered` or `no_api_key`. Corrupt credentials are not an incomplete registration: they remain monitored and unavailable. Each monitored account reports `available`, a safe `error_code`, `suppressed` codes and `warnings`. An account-local credential, malformed snapshot or oversized snapshot failure marks only that account unavailable (`credentials_unavailable`, `snapshot_invalid` or `snapshot_too_large`); healthy accounts remain assessable in the same HTTP 200 response. Suppressed codes are not assessed, not cleared. Critical warnings sort first, then code and subject. Every warning has `code`, `severity` (`critical` or `warning`), `since`, integer `facts`, scrubbed `names` and a scrubbed `reason`; amounts are pence, never preformatted money.

| Code | Recorded condition and evidence | Next action |
| --- | --- | --- |
| `controller_stalled` (critical) | A valid `_engine.observed_at` is at least 10 minutes old. Missing, zero or future controller time suppresses both controller and refresh assessment, not a healthy result. Facts contain elapsed `minutes`, and `since` is that completed controller observation. A stalled controller emits this warning and suppresses `refresh_failing` for the account. | Inspect the account-worker, ownership and database logs; fix the stalled controller's cause, then run `scripts/restart-bot.ps1`, or roll back a regression with `-Version <previous>`. |
| `refresh_failing` (critical) | With a current controller, any of `ships`, `vaults`, `contracts` or `clock` has a valid `updated_at` at least 10 minutes old. `since` is the oldest affected update, `facts.minutes` its age, `names.datasets` the comma-separated affected datasets, and `reason` the first scrubbed error. At 9 minutes 59 seconds there is no warning. If no valid update qualifies but any required timestamp is missing, zero or future, this code is not assessed. | Inspect the listed datasets and first error for auth, rate-limit or upstream/storage trouble; fix the refresh cause, then run `scripts/restart-bot.ps1`, or roll back with `-Version <previous>`. |
| `fleet_idle` (warning) | At least half of known-state, non-sunk ships, with at least one qualifying ship, are `berthed` or `anchored_roadstead`, have no voyage and have been free to depart for at least 30 minutes. Reports `idle`, known `total`, `unknown` and longest-idle `minutes`, plus `names.ship`, `ship_id`, `port` and `since_source`. Unknown ships are not idle and are excluded from the known denominator. | Inspect the named ship, safe stores, regional funds, locks and executable route economics; fix the real restriction or planning regression without forcing a losing/unsafe voyage, then run `scripts/restart-bot.ps1`, or roll back with `-Version <previous>`. |
| `hold_stuck` (warning) | The longest-idle ship with a current ship-level `_engine.holds` entry has been free to depart for at least 90 minutes. Empty `ship_id` account/control holds never qualify. Reports ship/port/provenance, `minutes`, the scrubbed hold `reason` and `other_count` of other qualifying held ships. | Inspect that hold and the immutable action/mission in the Journal; reconcile and fix its cause without deleting recovery state, then run `scripts/restart-bot.ps1`, or roll back with `-Version <previous>`. |
| `loss_streak` (warning) | The latest three known-cost commercial settlements all have `net_pence < 0`. Eligible kinds are exactly `spot`, `charter`, `freight`, `spot+freight`, `charter+freight`, `recovery` and `aborted`; relocation probes and unknown-cost rows are skipped. Fewer than three known commercial completions is assessed false, not unavailable; a win among the latest three also prevents the warning. Facts contain `count` and summed `net_pence`; `since` is the oldest completion in the streak. | Inspect the three settlements' costs, proceeds and route assumptions; fix the losing economics before resuming the policy, then run `scripts/restart-bot.ps1`, or roll back a strategy regression with `-Version <previous>`. |
| `falling_behind` (warning) | With at least three ranked players on the published rising board, compare our published growth bps with the median non-owned entry. The server's window is the last seven real days, or shorter when a player's first hourly snapshot is later (`window_start_at`). It warns strictly below that median, even if our growth is positive. `names.basis=rising`; facts are `ours_growth_bps`, `median_growth_bps`, `ranked_players`. Publication age must be at most two hours; missing own window or rival entries suppresses it, without fallback. With fewer than three ranked players, use the matched 24-hour valuation window: at least three measured rivals, our change negative and below their median, latest publication at most two hours old. This reports `basis=valuation_24h`, `ours_pence`, `median_pence`, `rival_count`. Missing or stale evidence is unassessed, not cleared. Six hours is UI-only. | Compare published growth and valuation composition with our realized settlements. Neither basis proves rival profit. Fix confirmed causes, not the warning threshold. |

Both idle warnings are suppressed if either `ships` or `_engine` is incomplete, has an error, is stale or has an `updated_at` age of at least two minutes. They are also suppressed when `clock.paused` is true, `_engine.mode` is `observe`, or all ships have unknown state (`known=0`, `unknown>0`). Missing/stale/suppressed evidence is not a clearance: the log watcher retains its earlier active state until a complete assessment clears it. The UI renders the current report rather than carrying earlier warning lines across successful responses. A stale controller is not interpreted as a newly idle fleet.

Free-to-depart time is the later of `ship.berthed_at` or `ship.dock_queued_at` (the available docking timestamp) and `ship.turnaround_unlocks_at`. If docking time is absent, use a newer detail stored at `data.ship_details[id]`, with freshness metadata keyed by `ship_details/<id>`; details are grouped payload data, not separate `data["ship_details/<id>"]` children. If that still provides no docking time, use the latest recorded `ship_arrived` event's database `created_at` only when it is newer than the latest departure and fresh state still says docked with no voyage; `names.since_source` is then `arrival_event`. The displayed duration is since recorded arrival (receipt time), not a claimed authoritative berth timestamp. The turnaround unlock still applies. If all sources are absent, idle duration is unknown, never zero or a fabricated idle age.

Arrival history is read only for docked ships without a current voyage, port-clock evidence or a usable newer grouped detail. The read is limited to the needed ship IDs, the last seven real days and the newest 10,000 arrival/departure rows. Goose version 11 (`00011_warning_arrivals.sql`) adds the partial `events_warning_arrivals_idx` on `(account_id, created_at DESC, id DESC)` with `WHERE event_type IN ('ship_arrived', 'ship_departed')`, matching the read predicate and excluding unrelated events from its scan. Like version 10, it builds inside the migration transaction with `CREATE INDEX IF NOT EXISTS`; down uses `DROP INDEX IF EXISTS` and never changes event rows. A failed build rolls back and leaves version 11 unapplied for the next start to retry. If the row bound omits a ship's arrival, that ship stays unknown; a newer departure cannot be hidden behind a retained older arrival.

A process-wide watcher evaluates immediately at startup and every real minute independently of account runners, so a stopped runner can still yield `controller_stalled` within 11 minutes. It logs WARN `engine warning` once for each newly active `(account_id, code)`, WARN `engine warning persists` when an assessed active warning has not been logged for 30 real minutes, and INFO `engine warning cleared` once with `active_for` only when an available, unsuppressed assessment clears it. Changed facts, subjects or reasons do not re-raise an already active code. Missing accounts, stale/suppressed codes and account-local failures retain prior state without announcing clearance; the next assessed active result does not re-raise an existing key. A restart's first assessment raises each measured active warning once. WARN `engine warnings unavailable` appears once per global read-failure streak with a safe `error` of `database_unavailable` or `context_deadline_exceeded`; an account-local unavailable streak includes that account's identity and a safe `error` derived from its `error_code`. Recovery permits later streaks to log again. Context cancellation is silent.

Log fields include account label and ID, code, severity, since, integer facts and scrubbed names/reason. Logs are the transition record; no warning-history table or automatic remediation is added. The dashboard polls one global `/api/warnings` query every 30 seconds. Actual current warning lines appear on every view, as does genuine warning unavailability for monitored accounts. Accounts explicitly listed in `unmonitored` are outside the warning scope; only Overview labels them not monitored, including registrations with no API key. Suppression/not-assessed notes, the quiet all-clear and build line are also Overview-only; without warnings or genuine monitored-account unavailability, other views show no strip. The all-clear requires a nonempty fully assessed monitored scope. A successful response is rendered as-is: missing/unavailable accounts and suppressed codes do not retain earlier warning lines. Only a request failure preserves the cached last successful response, with a could-not-update note; a failed request without cached data says `Engine warnings unavailable` for monitored scope.

## Readiness and diagnostics

- `GET http://127.0.0.1:8585/healthz`: process health.
- `GET http://127.0.0.1:8585/readyz`: storage readiness.
- `GET http://127.0.0.1:8585/metrics`: metrics exposition.
- `GET http://127.0.0.1:8585/api/dashboard`: bounded account snapshots and recent activity.
- `GET http://127.0.0.1:8585/api/history?account_id=...&kind=requests&limit=100`: filtered diagnostics. Kinds include requests, events, decisions, metrics, journal and settlements; maximum 500 rows. Journal reads require `account_id` and return company-scoped projected economic records, not raw action journals.
- `GET http://127.0.0.1:8585/api/standings-history?account_id=...&range=24h&top=5`: permanent official player valuations. Presets are `24h`, `7d`, `30d`, `all`; `top` is 1 to 20, default 5. This read never calls the game.
- `GET http://127.0.0.1:8585/api/rivals?account_id=...`: recorded rival valuations, traffic overlap and qualified hunches, with explicit source times and partial coverage. Missing or invalid account scope is rejected; unavailable reads never become empty successes.
- `GET http://127.0.0.1:8585/api/warnings`: recorded warnings for monitored accounts, with `generated_at`, `build {revision, modified, started_at}`, `accounts [{account_id, label, available, error_code, suppressed, warnings}]` and `unmonitored [{account_id, reason}]`. Unmonitored reasons are `disabled`, `unregistered` and `no_api_key`; corrupt credentials remain an unavailable monitored assessment. Revision is actual `vcs.revision` or null, modified is the actual VCS flag, and started_at is process start time, not request time. An account-local decode, size or credential failure uses a safe account `error_code` and leaves healthy accounts available. A nil store or failed database read returns HTTP 503 `database_unavailable`, never an empty all-clear. Missing, unavailable and suppressed assessments do not mean no active warnings. The `dashboard API listening` INFO line also carries revision and modified.

The browser must show per-dataset freshness and errors. A healthy HTTP process does not prove the account is trading. Inspect runner decisions, latest relevant snapshots, pending actions, and the upstream clock.

Raw request/response diagnostics, events, and decisions are retained for seven days. For an account with open passenger receipts, passenger events since its earliest receipt start are protected until resolution or closure; creation time is used if the settlement is not yet present. Receipts expire after seven real days. Active passage missions also protect passenger events since their start until hull recovery or settlement releases the mission. Older events, other accounts and non-passenger diagnostics follow normal retention. Current state, mission recovery records, encrypted action journals, realized settlements with their event identities, receipt ownership links, and metrics remain durable after raw events expire. Diagnostic retention runs periodically; it must not erase unresolved actions or turn an old mission into a new trade.

Recovery reads include protected older passenger events while a receipt or passage remains open. A retention warning about unreadable recovery rows protects that account's passenger events conservatively; requests, decisions, unrelated events and other accounts still expire normally. Repair the underlying mission evidence rather than deleting it to force pruning.

World > Standings charts the latest top 5/10/20 plus your player and keeps the full official standings and valuation-method disclosures. Below the chart, Rivals shows Growth vs rivals for 6 and 24 hours, three qualified hunch lines and one hunch chip plus one key fact per recorded rival. Its captions report matched publication times, official valuation time, an overdue refresh, the last 24 real hours of partial port notices, our measured daily change and coverage gaps. Search, numeric sorts and pagination survive automatic 30-second refreshes. An official valuation is not cash or realized earnings, and missed hours are not filled. Identity is the exact season and company-name pair because the game does not expose player IDs; similarly named companies never inherit each other's history. Every known configured company and our standings-account name are excluded. Incomplete own names can leave our other companies in the observed list, so verdicts are withheld rather than attributing them to a rival.

Goose version 8 backfills standings from retained successful raw responses and current snapshots, then keeps every newly observed hourly valuation permanently, independently of raw diagnostic expiry. Include both `standings_history` and `standings_accounts` in normal full-database backups. Already-pruned responses cannot be backfilled. This additive migration does not send game requests; rolling it back requires restoring the matching pre-upgrade backup, not deleting permanent metric history.

Goose version 20 adds `metrics_net_worth_unique_idx` and copies recorded own standings into `net_worth_pence` metrics for Money -> Performance. Only standings published under the account's configured company name (`configured_company_identities`) are copied; raw request diagnostics are not scanned, so the migration takes milliseconds. Its down migration drops the index and keeps the recorded valuations.

Rival 24-hour change compares the latest publication with the nearest same-season publication within 60 minutes of 24 real hours earlier. Missing publications or missing names yield no measured change, not zero; less than 24 hours of valuation history is too new, otherwise the baseline is unmeasured. Hunch precedence is Most dangerous, Plays rough, Threat, Losing value, Growing, Steady, Too new to judge, Not measured, Not ranked. Most dangerous picks the largest rival strictly above our same-publication worth with shared lanes or ports; only unknown overlap allows a largest-above-us fallback. Threat requires at least our worth and observed overlap. Neither is assigned without our valuation. Positive and zero measured changes mean growing and steady even below zero, with a negative-worth fact; otherwise negative worth or negative measured change means losing value.

`/api/rivals` returns `growth` in 6-hour then 24-hour order. Each window matches the nearest same-season publication within plus or minus 60 minutes of the target; `from` and `to` are actual source times or null. Our `standings_accounts` identity must exist at both endpoints with an unchanged best-company name. Missing identity gives null `ours_pence` and an `ours_reason`. Rivals need both endpoints. An even median averages the middle two integer-pence changes with truncation toward zero. Nulls mean not measured. These tables show valuation change, not realized profit. The 24-hour table is only the fallback warning basis when the rising board has fewer than three ranked players.

Plays rough prioritizes an in-force, unexpired monopoly from a complete, error-free snapshot no older than 30 real minutes, then recorded monopoly grants in seven days, then the latest borrower record's positive loan walk-aways, then positive defaults. Missing or zero credit records, stale monopolies and expired monopolies are not a clean record. Evidence only for unlisted companies is disclosed separately from no attributable evidence. The headline names the strongest listed evidence; a caption explains when Most dangerous has precedence over that company's Plays rough row chip. The not-making-profit line picks the most negative net worth before the largest relative daily decline with a positive baseline. Plays rough is a conduct-evidence hunch; not making profit is a valuation hunch, never measured rival profit. Piracy sponsors, attacks, blockades and dumping are never attributed; marque sponsors are anonymous.

Traffic reuses bounded sailing events and saved quay notices without dashboard-triggered game requests. Lanes are unordered port pairs; shared ports need a direct sighting. `overlap_known=false` means unknown, not zero. Raw event coverage remains bounded by seven-day retention. Versions 10 and 11 add the rival-evidence and warning-arrival partial indexes transactionally. Stop the old bot before migrating; a failed build rolls back and leaves its migration version unrecorded. Do not assume how long an index build or its locks will take.

The 0.5.1 rivals overview adds published rising growth, per-class observed ship identities over 24 real hours and seven real days, expansion contributions, and public credit facts. These are partial observations, not a fleet census or an income-source attribution. `rising_publications`, `rival_companies`, `rival_credit_reports` and `rival_credit_attempts` are permanent records from migration 17. Every registered account's known companies are excluded from rival comparisons. Expansion bonuses explain valuation, never trading revenue.

The collector reserves each `/v1/loan/credit?company_id=` lookup before sending. Limits are 20 lookups per real hour globally and one per company per six real hours, including failed requests. Missing credit is unknown, not a clean record. Read the recorded `observed_at` and fields for debt, repayments, defaults, walk-aways, mortgages, futures claims and smuggling convictions. For raw review, use a read-only query such as `SELECT season_number, company_id, observed_at, report FROM rival_credit_reports WHERE company_id = '<company-id>' ORDER BY observed_at DESC LIMIT 20;`. Compare identity with `rival_companies`; do not join similarly named companies. Dashboard reads use stored evidence only and never spend lookup allowance.

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

Start with the engine-warning strip on Overview or Fleet: read `fleet_idle` or `hold_stuck`, its named ship, port, elapsed free-to-depart time and source. If warnings are unavailable, inspect storage and freshness before drawing a conclusion; no warning is not proof of profit or progress. Then read the company's controller observation time, current hold, next recorded check and each ship's current status/location/cargo. The map's position can be interpolated; the ship's REST state is authoritative. A fresh page response is not proof that the executor advanced. Fix the diagnosed cause, then run `scripts/restart-bot.ps1`, or use `-Version <previous>` for a compatible rollback; do not restart merely to erase a legitimate economic hold.

For economic holds, inspect local depth, safe stores and hull rating, regional funds, berth locks and remaining contract deadlines. When direct trades lose money, the runner can spend its bounded remaining discovery allowance inspecting a different evidenced source. Unknown historic commercial costs prevent profit-based replenishment but do not erase unused seed; unknown prior probe expense still blocks more discovery. Repeated failed opportunities consume permanent allowance. Do not delete that history or relax safe-provision requirements to force a departure.

For action holds, use the company Journal and System diagnostics to identify the immutable action and its actual recovery condition. A definite rejection can replan after its persisted cooldown; an ambiguous result must reconcile first. Silence in INFO logs between meaningful transitions is intentional.

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
go test -p 1 -timeout 30m ./...
go build ./cmd/...
pnpm --dir web format:check
pnpm --dir web lint
pnpm --dir web typecheck
pnpm --dir web test
pnpm --dir web build
pnpm --dir web test src/proxy.http.test.ts -t production
```

Dashboard unit tests cover recorded decision economics, numeric cash sorting, and exact host allowlist boundaries without a server, live API, or database. The HTTP host-boundary suite (`pnpm --dir web test src/proxy.http.test.ts`) starts real Next development and production servers on scratch loopback ports; it requires neither Go nor PostgreSQL. Use this worktree exclusively while it runs: do not run another `next dev` or build in the same worktree.

The HTTP suite checks exact proxy denials for hostile and genuinely missing Host headers on `/api/dashboard` and `/api/history`, even with an allowed `X-Forwarded-Host`. Missing Host uses a raw HTTP/1.0 request because Node rejects HTTP/1.1 without Host with a parser-generated 400 before the proxy runs. Allowed `Host: localhost:3400` deliberately differs from the transport port and must receive a real non-403 HTTP response. Backend-related 500/502 responses count only as gate passage, not successful proxying, Go contact, or Go health.

Production is skipped only when `web/.next/BUILD_ID` is missing, with instructions to build; an existing but unusable build fails. The post-build production command above must run against fresh output so coverage cannot remain skipped or validate a stale build. Production rewrites are build-time artifacts: changing `API_INTERNAL_URL` at `next start` does not retarget them. The HTTP suite never builds Next itself.

Run database integration tests with the disposable URL, then smoke the actual CLI and database API. For concurrency changes, run `go test -race -p 1 -timeout 30m ./...` when a working C compiler is installed. Report an unavailable race toolchain instead of calling that check passed.

Run the fixed-company logging and underwriting CLI fixtures serially with each other and with the entire Go suite. Different disposable schemas do not isolate PostgreSQL company advisory locks. A competing fixture can make `Runner.Run` return `ErrAccountLocked` before its first request; a REST-capture timeout is not proof that the HTTP path stalled.

Engine tests run in parallel, six at a time by default (pass `-parallel` to change it). Each uses its own schema and fake game server, and test connections turn off `synchronous_commit` because the schemas are thrown away. A test that calls `Runner.Run` takes the company advisory lock and waits on real-time gates, so it must not call `t.Parallel()`: Go runs such serial tests before the parallel ones.

Go's timeout covers an entire package, not each test. The serialized PostgreSQL scenarios can exceed shorter package-wide limits even while individual cases continue passing. Keep their bounded waits and assertions intact, and preserve per-test output when diagnosing a suite timeout.

For dashboard changes, start both real services, exercise account selection, map selection, fleet detail, data views, diagnostics filters, themes, and keyboard navigation. In the automation ledger, expand route economics, confirm collateral and stress disclosures, and sort the cash header in both directions: amounts must sort numerically, with unavailable values last. Inspect screenshots at the viewports in `AGENTS.md`, verify no page overflow, and check browser console output. Production data must never be replaced by demo fixtures to hide an integration problem.
