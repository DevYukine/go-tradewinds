# Tradewinds: repository working rules

## Purpose

Build and operate a reliable autonomous trading fleet for Tradewinds. Optimize sustainable net profit and account net worth, not gross turnover or isolated per-ton margins. Progress the company and expand its fleet when observed returns justify the capital, upkeep, and risk.

Tradewinds explicitly supports bots and automated players. Do not add humanization, anti-detection delays, browser impersonation, or restrictions inherited from unrelated games. Documented external data sources and development tools are allowed. Respect the game's actual rate limits and mechanics.

## Current player and naming

- The initial account handle is exactly `Yukine`.
- The initial company name is exactly `The Pokemon Pirates`.
- Start with one account and one company. Support concurrent additional accounts, but never create extra accounts or companies implicitly.
- Use `internal/names` for ship names. Its vendored catalog contains all 1,025 canonical English Pokemon species names available in the checked source, including punctuation and gender symbols. Start with Lapras, then National Pokedex order.
- Exhaust every unused canonical species name before adding numeric suffixes. Include sunk ships and persisted build intents when checking occupied names. Restarting must not recycle names.
- If the game adds nameable places, use Pokemon city names. The current API does not allow naming ports or warehouses.
- Ask the owner for a naming theme before creating other named asset categories, including convoys. Do not improvise a theme on the owner's behalf.
- Account credentials belong in encrypted PostgreSQL records. The encryption key belongs in local environment configuration and must be backed up separately. Never print secrets, return them to the dashboard, or commit local credentials.

## Readability and formatting

- All source must be properly formatted, human-readable, and understandable to a maintainer.
- No minified Go. No semicolon-packed statements, compressed error handling, or dense one-line control flow. Use normal multi-line Go with explicit errors and descriptive names.
- Run `gofmt` on every changed Go file. Formatting alone does not make dense code readable: split distinct operations and name non-obvious intermediate values.
- Use the dashboard's Prettier scripts for TypeScript, React, CSS, and configuration: `pnpm --dir web format` while editing, then `pnpm --dir web format:check` before committing. Do not format generated Next.js output or lockfiles by hand.
- Keep functions focused. Separate fetching, economic decisions, durable intent, and execution. Avoid hidden network or database effects in builders and pure calculations.
- Prefer simple concrete types and standard library facilities. Use Fx for lifecycle and dependency composition, Zap for structured logging, GORM for storage, and Goose for explicit PostgreSQL migrations.
- Do not add abstractions for hypothetical use. Do not copy the retired implementation or retain compatibility shims, Redis, Nuxt, passenger APIs, or old-server authentication.
- Comments explain assumptions, units, invariants, and non-obvious decisions. They must not narrate obvious code.
- Human-facing text uses plain language, sentence case, and honest descriptions of missing or uncertain data.

## Sources and economic policy

- Current API: `https://tradewinds.kibb.io`; guide: `/skill.md`; OpenAPI: `/api-docs/openapi.json`; public index: `/`.
- Read the current guide and schema after a game update. Do not infer a new endpoint or request field from the retired game.
- Money is integer pence, 100 pence per Crown. Cargo is tons, speed centiknots, and percentage rates basis points. Distinguish real time from accelerated game time everywhere.
- Compare net profit per game day after order-book depth, buyer customs, stores, berth dues, treasury transfer fees, upkeep, turnaround, and capital tied up.
- Prices and economic activity are partly hidden. Label estimates and freshness. Never claim that an unobserved price, unpublished hazard probability, or untested strategy is a measured fact.
- Hard reject routes beyond the ship's hull rating. Carry at least the route's safe provisions requirement and retain operating cash. Never enable starvation to force a departure.
- Charters accept only cargo carried in from another port. Sealed freight cannot be sold, unloaded, or delivered to charters. Respect contract deadlines and actual remaining quantities.
- Buy ships only when observed profitable throughput and realistic payback justify them after working capital and incremental upkeep. New ships need stores and appropriate armament.
- Futures, underwriting, marques, patrols, and convoys are economic choices, not checkboxes. Require measured justification and model liabilities before committing funds.

## Reliability and persistence

- REST is authoritative. Events trigger refresh; they are not a substitute for reconciliation.
- Persist one idempotency key and immutable payload per logical mutation before sending it. Reuse that key only for retries of that action. Never retry a trade-and-depart sequence as one action.
- Keep ambiguous actions unresolved until reconciled. The server retains idempotency for 24 real hours; an old unresolved action must not be blindly replayed.
- Honor `Retry-After`, shared per-IP limits, per-key limits, and the strict authentication bucket. Use cancellable bounded exponential backoff with jitter for reconnects and transient failures.
- Persist SSE events and their cursor atomically. Resync on startup, reconnect, and `gap`; stop writes on revoked credentials, frozen company, paused world, or incoherent required state.
- Run at most one executor per account. Account-lock loss must cancel its execution context. Separate accounts may run concurrently without sharing mutable account state.
- Store current snapshots, missions, unresolved action intents, account credentials, and long-lived metrics durably. Retain raw requests, responses, events, and decision diagnostics for seven days. Retention must never delete unresolved recovery state.
- Redact secrets before storing diagnostics. Keep database migration, backup, and restore instructions current. Never silently reset a database or bankrupt a company as recovery.

## Dashboard

- Every change touching the dashboard or frontend MUST load and follow Anthropic's `/frontend-design` skill (`skill://frontend-design`) before planning or editing. This includes layout, components, styles, interactions, data presentation, and frontend architecture. Do not substitute a generic dashboard template.
- Research relevant official React, Next.js, Tailwind, accessibility, and component-library guidance. Define the visual direction and interaction model before implementation, then critique real screenshots against the owner's brief.
- The owner rejected the initial card-heavy dashboard. Replace it with a clean, restrained Tradewinds fleet-operations interface: make the working chart and useful data the focus, not marketing headlines, decorative statistics cards, numbered filler, or repeated uppercase labels.
- Cover all configured accounts and companies with clear scope controls, comprehensive statistics, ship details, and advanced sorting and filtering. Preserve selections and table state during automatic updates; never reset the operator's work on a refresh.
- Update recorded data automatically and frequently without manual refresh. Ship icons must reflect the latest observed voyage state; clearly distinguish interpolated positions from authoritative game facts. Pause unnecessary hidden-tab work, clean up subscriptions, and keep expensive calculations out of render paths.

- `web/` uses Next.js App Router, React, strict TypeScript, Tailwind, TanStack Query, and Zustand. TanStack Query owns server data; Zustand owns UI state only.
- The dashboard reads recorded database facts through the Go API. It must not trigger extra game requests or invent successful empty responses, simulated profits, or demo account activity.
- Keep data types and consumers aligned with Go payload changes. Display stale, missing, paused, disconnected, and error states explicitly.
- Prioritize a useful fleet map, ship and cargo detail, regional treasury, contracts, warehouses, shipyards, world progression, and searchable decisions/events/request diagnostics.
- Keep both light and dark themes readable. Use restrained nautical styling, neutral surfaces, and color for meaningful status. Keep focus visible, labels accessible, and reduced motion respected.
- No page-level sideways scrolling, clipped controls, or overlapping content. Validate Chromium at 640x960 touch, 854x480, 1280x720, 1920x1080, 2560x1440, and 3840x2160. Also cover MacBook viewports 1470x830, 1512x860, 1710x985, and 1728x995 at 2x where available; check Windows 125% and 150% scaling. Report unavailable browser coverage honestly.
- Use one shared server-state query/cache per page, not a game connection per component. Keep fleet-wide work out of locks and account execution paths.
- Bind the Go API and development UI to loopback by default. Remote deployment needs an authenticated reverse proxy. Never expose credential fields or use `dangerouslySetInnerHTML` for game text.
- Verify actual browser interactions and screenshots after UI changes. Keep console errors and warnings at zero while the servers are healthy. Use distinct ports for parallel worktrees.

## Workflow and verification

- Work in an isolated branch/worktree. Conventional Commit messages, one line. Do not open a pull request unless requested.
- Keep the original checkout intact until the reviewed replacement is ready. Use absolute worktree paths when agents share a session rooted elsewhere.
- Before committing, run formatting, Go vet, Go tests, Go build, then dashboard `pnpm --dir web format:check`, `pnpm --dir web lint`, `pnpm --dir web typecheck`, `pnpm --dir web test`, `pnpm --dir web build`, and `pnpm --dir web test src/proxy.http.test.ts -t production`, in that order. Dashboard unit tests cover decision economics and host boundaries without starting services. The HTTP host-boundary suite starts real Next development and production servers but needs neither Go nor PostgreSQL; use the worktree exclusively without another `next dev` or build. Only a missing `web/.next/BUILD_ID` skips production; always run the post-build production command against fresh output. Missing-Host coverage uses raw HTTP/1.0 because Node rejects missing Host in HTTP/1.1 before the proxy runs. Allowed-Host non-403 responses prove gate passage only, not successful proxying, Go contact, or Go health; production rewrites are fixed at build time. Run database integration tests against a disposable PostgreSQL database.
- Run race checks after concurrency changes when the required compiler is available. Exercise changed behavior through the real CLI/API/browser; tests alone are not operational proof. Dashboard browser verification includes ledger cash/stress disclosures and numeric cash-header sorting with unavailable values last in both directions.
- Tests cover plausible consumer-visible failures: monetary boundaries, partial fills, stale data, restart transitions, duplicate actions, cursor gaps, cancellation, and credential redaction. Avoid source-text assertions, mock echoes, and tests that merely repeat constants.
- Keep strategy evidence in `docs/strategy.md`, operating instructions in `docs/runbook.md`, and architecture in `docs/architecture.md`. Cite current authoritative sources and distinguish observed behavior from estimates.
- Never report a check as passed unless it ran. Record remaining operational limitations precisely.
