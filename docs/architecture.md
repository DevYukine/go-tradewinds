# Architecture

## Boundaries

| Package | Responsibility |
| --- | --- |
| `cmd/tradewinds` | Explicit registration, account listing, Fx lifecycle, HTTP listener, worker startup and shutdown |
| `internal/config` | Environment loading and validated operating limits |
| `internal/gameapi` | Current typed API contract, HTTP/SSE transport, scoped rate limits, retries and raw observations |
| `internal/store` | PostgreSQL migrations, encrypted credentials/journal, missions, snapshots, diagnostics, settlements and metrics |
| `internal/engine` | Economic decisions, durable mission execution, authoritative reconciliation and eligible reinvestment |
| `internal/httpapi` | Bounded read-only views of database state, readiness and metrics |
| `internal/names` | Vendored canonical Pokemon catalog and unused-name selection |
| `web` | Next.js dashboard; no direct game credentials or game requests |

The Go process runs independently of the dashboard. Closing a browser does not stop trading. Opening more tabs does not create more upstream sessions. PostgreSQL is the durable operational record, not an optional cache.

## Account isolation

Each enabled account has its own credentials, company identity, mission namespace, event cursor, and executor. Credentials and action payloads are encrypted with AES-GCM under a local 32-byte master key; associated data binds ciphertext to its record identity. Dashboard data never includes decrypted credentials or raw action journals.

A pinned PostgreSQL advisory-lock connection protects account execution. Loss of ownership cancels the runner's context rather than transparently acquiring a new lock. Cancellation cannot undo an upstream request already sent, so a takeover must resolve pending journal entries before creating another dependent action. Company uniqueness prevents importing the same company under two independently executable local identities.

Monitor checks, write guards and cleanup serialize access to that pinned connection through the complete result-reader lifetime. A `database/sql` call returning does not mean pgx has released its active rows. This prevents false ownership loss from overlapping checks without suppressing real lock or connection failures.

## Mutation lifecycle

1. Refresh the relevant authoritative data.
2. Persist the mission and its immutable action identity/payload.
3. Send with that action's idempotency key.
4. Record the result durably.
5. Refresh cargo, stores, funds, contracts, or voyage state affected by the result.
6. Advance the mission from actual outcomes, including partial fills.

Network ambiguity is not failure evidence. A timeout or upstream 5xx retains the original intent. A successful response whose body cannot be interpreted is committed-but-uninterpreted, not permission to try again with another key. An in-flight 409 also remains unresolved. A definitive rejection may lead to a new, separately recorded plan after fresh state and any cooldown.

The upstream replay window is 24 real hours. Stop unresolved retries before that window expires, with a margin for in-flight request duration. Authoritative reconciliation must establish an outcome before a replacement action is allowed. Retaining the old key forever does not make it safe to replay forever.

Conflict recovery distinguishes definitive rejection from an ambiguous in-flight request. Freight tender refusals persist a bounded retry deadline; fresh active-contract and full-manifest evidence permit a new tender identity after that deadline. Fresh terminal contract states can settle the obligation without waiting for another tender. Charter refusals retain the observed offer terms and reopen when authoritative terms change. Response prose cannot prove a default, payment or delivery.

An attributed unresolved ship action quarantines dependent spending, not unrelated safe delivery credits. An orphaned pending request whose resources cannot be identified still blocks execution. Reconciliation and replay always use the persisted request's contract, ship and payload, never a replacement plan's identity. Short sealed manifests are held, not replenished with ordinary cargo or repeatedly tendered as the originally accepted amount.

Registration follows the same model. The generated passphrase is stored before registration; player creation, key creation, company incorporation, and company-key creation have stable independent action identities. A handle conflict cannot cause an automatic fallback name. Company creation is reconciled against the player's actual company list.

## Events and freshness

SSE is an invalidation channel. REST and settled action responses determine monetary state. Event IDs are opaque strings, not numeric sequence counters. Persist event deduplication and cursor advancement atomically. Reconnect with the recorded cursor; a replay gap requires cursor reset and authoritative resynchronization, not a loop using the same rejected cursor.

Publish a paginated dataset only after all pages succeed. Retain the last good value on refresh failure and publish an explicit per-dataset error/freshness marker. Refreshing the clock must not make stale cash or cargo appear current. Economic writes require fresh, coherent inputs.

The runner must also wake without events for berth unlocks, cooldowns, deadlines, expected arrivals, and freshness expiry. Synchronous orders and unlocked berths are not guaranteed to produce the events a mission would need to progress.

Private books and shipyard quotes require fresh authoritative stationary ship presence or factor access. A sailing ship's retained origin and a mission's intended origin are not presence. Public route discovery remains separate from private-data authorization.

Clock bounds require a complete successful current-generation capture with valid request/completion ordering and a fresh host timestamp. Failed, future-dated or stale observations cannot authorize new mutations. A new-send preflight checks the same conditions after rate-limit waits; persisted ambiguous or completed actions retain their independent reconciliation path.

Ordinary queued events request another capture without indefinitely vetoing a safe mutation after the current capture. Stream gaps, authorization controls, and stream closure remain barriers: their published control epoch prevents writes against an earlier capture, even while a control waits behind ordinary events. Company billing, remittance, tax, seizure and bankruptcy events use a separate financial epoch. Captured spending decisions carry that epoch and the runner instance through staging. A stale never-sent request is replanned after authoritative refresh; an existing journal identity remains authoritative for ambiguous or completed outcomes. A first-attempt guard runs after the transport's rate-limit wait. If it vetoes a newly journaled request before transport, durable explicit no-send evidence permits safe replanning across restart. This guards known local invalidation, not atomicity with upstream cash changes.

Invalidated never-sent standalone maintenance intents are released so idle planning can reconsider the ship from fresh state. Voyage missions retain their cargo and obligations; ambiguous or completed journal requests are never discarded by this release.

First-send monetary guards recheck fresh financial authority and the stage-specific funded commitment, including ship-less construction. Construction reuses the planner's quote, payback, spare depth and commissioning-reserve checks. The transport callback checks the captured financial epoch both before and after that work, so a financial event published during preflight invalidates the never-sent request. Missing or failed balance observations cannot authorize a new debit; already attempted requests retain their immutable recovery path.

## Rate limits and retry ownership

The transport owns individual attempts, bounded transient retries, and rate-limit waits. The engine owns mission recovery, SSE reconnection, resynchronization, and economic replanning. Both use cancellation.

| Bucket | Per-key burst / refill per second | Shared-IP allowance |
| --- | ---: | ---: |
| Read | 2000 / 100 | 5 times key allowance |
| Write | 1000 / 50 | 5 times key allowance |
| Batch | 300 / 10 | 5 times key allowance |
| Stream | 200 / 10 | 5 times key allowance |
| Authentication | Not key-based | 20 / 0.2 |

Authentication, stream routes, and batch routes must be classified before the generic HTTP method. Every retry consumes applicable tokens. Apply cooldown to the affected bucket rather than blocking all reads and all accounts unnecessarily. Respect `Retry-After`; a cached mutation 429 is not a reason to rotate an ambiguous action key.

SSE scanner errors preserve their original cause. Only `bufio.ErrTooLong` is reported as the 1 MiB line limit; interrupted chunks and connection failures are transport errors handled by the existing reconnect policy.

These shared-IP limiters coordinate one process. Separate hosts or processes behind the same public IP still share the upstream allowance; PostgreSQL ownership protects a company, not the entire network's request budget.

## Persistence lifetime

Raw request/response diagnostics, event payloads, and decision traces have a seven-day retention window. Current snapshots, encrypted credentials, mission recovery state, unresolved actions, idempotent settlement records, and metrics survive that window. Action journals are operational recovery records, not disposable request logs.

Runner decision fingerprints use a bounded 2,048-entry cache with seven-day deduplication. Sourcing emits an aggregate count and at most three ranked details; only the rolling arrival estimate is excluded from the fingerprint, not the persisted diagnostic. Cache eviction can repeat a diagnostic but cannot discard recovery state.

Diagnostic sanitization covers JSON member names as well as values, including nested event/request payloads and snapshot dataset names. Known account credentials, bearer tokens, API-key patterns, and credential assignments are scrubbed before persistence, so the same protections apply to history and dashboard read surfaces. Colliding sanitized member names receive distinct numeric suffixes without overwriting existing public names or dropping their values.

Realized economic records use integer pence and stable settlement identities. Graph metrics may use floating-point values for presentation; they are not the ledger. Seed capital, remittance principal, and collateral refunds are not trading income.

Mission accounting keeps durable claims on observed ledger expense IDs so a company-wide charge is not deducted by every concurrent voyage or charged again after a restart. Unknown acquisition costs remain unknown; they are not zero-cost profit. Reinvestment consumes known-cost settled results, including losses, rather than a filtered history of winning trades.

New destination-completing missions explicitly persist that choice; older roundtrip missions keep their existing return semantics. Sourcing uses a durable zero-revenue relocation mission and separate settlement before any cargo handoff. Its restart gate queries the latest relocation and all subsequent account settlements, retaining losses and unknown-cost disqualifications rather than treating a bounded dashboard history as the capital ledger.

## Dashboard contract

The browser reads `/api/dashboard` and bounded `/api/history` views through Next.js rewrites to the loopback Go API. TanStack Query owns fetched state; Zustand owns local controls such as account selection and map filters. The map uses explicit geographical port coordinates because the game catalog does not provide latitude and longitude.

Every view must distinguish a real zero from missing, inaccessible, stale, or failed data. Market estimates and inventory valuation must identify their basis. Source timestamps govern freshness; browser fetch time alone is not enough.

The company Journal merges recorded company events, whitelisted successful action outcomes, and durable settlements in one bounded, repeatable-read history query. Company scope is applied before the limit. The store selects lightweight action metadata first, then loads and projects one encrypted outcome at a time. Combined request/response ciphertext above 128 KiB is not loaded into this view; the entry honestly reports unavailable details while the original durable bytes remain intact. Lazy credential reads use the same transaction connection, so concurrent Journal reads never each wait for a second pooled connection. Raw request/response payloads are never returned. Transfers and collateral are labeled separately from revenue, and unknown-cost settlements never become zero-cost profit. A recorded action and its later settlement describe different facts, not two amounts to add together.

The interface separates fleet operations, money, trade, world conditions, and system diagnostics. Neutral charcoal and light surfaces share the same controls. The chart uses vendored Natural Earth 1:50m land, explicit port coordinates, and clearly labeled estimated ship positions. World conditions initially show active notices and fleet-relevant zones; visible scope controls retain access to the complete recorded set.

## Readability

Source is written for maintenance, not compression. Use idiomatic multi-line Go, descriptive intermediate values, explicit error handling, and focused functions. Gofmt is mandatory but does not make semicolon-packed control flow acceptable. The same principle applies to TypeScript and migration SQL.
