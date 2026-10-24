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

Each refresh runs independent GET jobs through at most six workers on the account's existing rate-limited client. Dependent waves still wait for their prerequisites; pagination and the explicit contract-status reads remain ordered within their job. Workers return private results and request timing. Only the runner reduces them into the snapshot, in deterministic order, after the wave has joined. Cancellation joins all workers and publishes no partial generation. Mutations remain serialized and do not overlap capture workers.

An actual CLI fixture capture completed 31 GETs in 0.519 real seconds; those requests' measured service times summed to 1.255 seconds. This is a local overlap measurement, not a live-game speedup benchmark or a separately run serial client. The same run observed six concurrent GETs, one mutation at a time, no GET/mutation overlap, and recovery after a `Retry-After: 1` refusal.

The runner must also wake without events for berth unlocks, cooldowns, deadlines, expected arrivals, and freshness expiry. Synchronous orders and unlocked berths are not guaranteed to produce the events a mission would need to progress.

Private books and shipyard quotes require fresh authoritative stationary ship presence or factor access. A sailing ship's retained origin and a mission's intended origin are not presence. Public route discovery remains separate from private-data authorization.

Clock bounds require a complete successful current-generation capture with valid request/completion ordering and a fresh host timestamp. Failed, future-dated or stale observations cannot authorize new mutations. A new-send preflight checks the same conditions after rate-limit waits; persisted ambiguous or completed actions retain their independent reconciliation path.

Ordinary queued events request another capture without indefinitely vetoing a safe mutation after the current capture. Stream gaps, authorization controls, and stream closure remain barriers: their published control epoch prevents writes against an earlier capture, even while a control waits behind ordinary events. Company billing, remittance, tax, seizure and bankruptcy events use a separate financial epoch. Captured spending decisions carry that epoch and the runner instance through staging. A stale never-sent request is replanned after authoritative refresh; an existing journal identity remains authoritative for ambiguous or completed outcomes. A first-attempt guard runs after the transport's rate-limit wait. If it vetoes a newly journaled request before transport, durable explicit no-send evidence permits safe replanning across restart. This guards known local invalidation, not atomicity with upstream cash changes.

Invalidated never-sent standalone maintenance intents are released so idle planning can reconsider the ship from fresh state. Voyage missions retain their cargo and obligations; ambiguous or completed journal requests are never discarded by this release.

First-send monetary guards recheck fresh financial authority and the stage-specific funded commitment, including ship-less construction. Construction reuses the planner's quote, payback, spare depth and commissioning-reserve checks. The transport callback checks the captured financial epoch both before and after that work, so a financial event published during preflight invalidates the never-sent request. Missing or failed balance observations cannot authorize a new debit; already attempted requests retain their immutable recovery path.

The published stream-control epoch also fences staging, preflight and the actual first send after a rate-limit wait. A gap or closure invalidates an earlier capture even if its balances still satisfy their age limit. Only an intent proven never sent may be released and replanned; attempted requests retain the exact durable identity and payload through reconciliation.

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

Goose version 5 converts mission metadata/recovery bytes, settlement context, policy payloads and owned-slice identity/frozen terms to authenticated ciphertext. Purpose-separated, length-framed HMAC-SHA256 lookup keys replace sensitive external identifiers in indexes; numerical accounting and bounded local classifications remain queryable. Associated data binds ciphertext to its account, table, field and lookup identity. Dashboard, history, Journal and console projections still scrub decrypted display strings before exposing them.

The conversion and version entry share one transaction, block legacy table access and refuse active account/company executor leases. It authenticates all existing credentials and writes a singleton key verifier; subsequent opens compare that verifier in constant time rather than treating a wrong lookup key as an empty database. Missing or corrupt verification data fails closed. Existing action ciphertext is not rewritten. Legacy nested action bodies are converted once, using exact journal bytes when the bound action and semantic payload agree; steady-state recovery has no plaintext compatibility reader.

During that conversion only, a legacy `__growth` control with an absent or null lane receives an explicit zero-quantity, zero-procurement allocation marked complete and tied to its existing generation. Existing allocations and unknown recovery fields are preserved. The controller resolves any original attempted action before accounted withdrawal; it neither synthesizes new lane economics nor sends fresh commissioning debits for that marker.

New destination-completing missions explicitly persist that choice; older roundtrip missions keep their existing return semantics. Sourcing uses a durable zero-revenue relocation mission and separate settlement before cargo handoff. Permanent history charges every probe, including failed inspections, rather than using the dashboard's recent rows. Nullable exploration expense separates consumed-store exposure from full provisioning cash; legacy known-cost probes retain the conservative full-cash upper bound. Unknown commercial costs prevent profit-based replenishment, while unknown probe spending blocks further use of the seed.

Growth retains its admitted marginal lot and capital reservation through construction, provisioning, refit and atomic handoff to the actual built ship. The handoff also carries paid operating cash, cost certainty, ledger boundaries and terminal-action evidence. Rejected, aborted and sunk commissioning controls settle their paid costs and attributable bills before deletion; hull and gun investment remains separate from operating shipment net.

Ordinary and growth planning share one owner-excluded reservation view for cash, source asks and stores, destination bids and charter quantities, including the next lot reserved during a paid return. Growth consideration and first-send build checks also evaluate idle hulls against those residual opportunities, excluding only the proposed growth allocation. First-send procurement must still fit residual asks at its immutable price limit. An unattempted ordinary purchase whose fresh residual depth is insufficient unwinds its paid costs and replans instead of repeatedly staging the same unsupported quantity; attempted requests keep their original identity and payload. Changed accrued-dues quotes are saved and execution continues in the same pass; quotation uses planned funded armament, while actual refit and departure checks use authoritative ship state.

An optional empty return needs fresh executable source/destination evidence and a persistently reserved next lot that beats local work after future return costs; it is not an unconditional route loop. After the paid return, the controller safely docks to inspect the source and re-prices before buying. Partial or zero IOC results are final for that order. Paid-cargo unwinds retain their loss-inclusive settlement instead of disappearing from future growth or discovery evidence.

Underwriting has its own `__underwriting` control mission, restored before orphan-action checks. The ordinary immutable journal owns retries; an unresolved slice reserves full possible collateral without disabling unrelated safe fleet work. Public policy observations, immutable owned receipts, funding provenance, terminal accounting and the fixed pilot seed are permanent PostgreSQL records. They are not pruned with seven-day diagnostics. A public membership filter is not an owned-slice receipt, and a policy disappearing from a listing does not release collateral. Additional risk waits for an exact owned-vault ledger debit; closure requires unchanged terminal policy evidence, not elapsed time.

Permanent changed-term or contradictory policy evidence vetoes both candidate admission and first send, even if the public board later reverts. A disputed prior settlement cannot release the pilot's risk capacity. Shipping commitments protect regional deficits and measured remittance fees before underwriting receives discretionary cash. Exact receipt identity, ownership and monetary bounds are validated independently of public coverage.

## Dashboard contract

The browser reads `/api/dashboard` and bounded `/api/history` views through Next.js rewrites to the loopback Go API. TanStack Query owns fetched state; Zustand owns local controls such as account selection and map filters. The map uses explicit geographical port coordinates because the game catalog does not provide latitude and longitude.

Every view must distinguish a real zero from missing, inaccessible, stale, or failed data. Market estimates and inventory valuation must identify their basis. Source timestamps govern freshness; browser fetch time alone is not enough.

The company Journal merges recorded company events, whitelisted successful action outcomes, and durable settlements in one bounded, repeatable-read history query. Company scope is applied before the limit. The store selects lightweight action metadata first, then loads and projects one encrypted outcome at a time. Combined request/response ciphertext above 128 KiB is not loaded into this view; the entry honestly reports unavailable details while the original durable bytes remain intact. Lazy credential reads use the same transaction connection, so concurrent Journal reads never each wait for a second pooled connection. Raw request/response payloads are never returned. Transfers and collateral are labeled separately from revenue, and unknown-cost settlements never become zero-cost profit. A recorded action and its later settlement describe different facts, not two amounts to add together.

The interface separates fleet operations, money, trade, world conditions, and system diagnostics. Neutral charcoal and light surfaces share the same controls. The chart uses vendored Natural Earth 1:50m land, explicit port coordinates, and clearly labeled estimated ship positions. World conditions initially show active notices and fleet-relevant zones; visible scope controls retain access to the complete recorded set.

Overview shows account/company status, each ship's observed location and cargo, current blocker, and the controller's next recorded check directly. `_engine.holds`, `observed_at` and `next_check_at` describe a completed execution cycle in the matching snapshot generation; a fresh dashboard HTTP response alone is not executor liveness. A missing hold assessment is distinct from a completed assessment with no hold. Journal is chronological rather than a nested decision dump, with time, vessel, activity and cash visible together at the 640px layout. Initial map labels prioritize selected and occupied ports before nearby hubs and suppress overlaps. Money directly exposes owned underwriting collateral, premium entitlement, settled net and pilot allowance, including unverified funding holds; Exchange retains public policy listings.

Treasury, fleet-growth and underwriting control holds are company notices, not ships. Their current display attribution has no ship ID; durable decision records keep the internal control identity for diagnostics. Holds for actual ships retain their ship ID even when the corresponding fleet observation is unavailable.

Assessment coverage is explicit: `assessed_ship_ids` lists only ships actually visited in the matching completed generation. Work may return after one staged or consumed action; unvisited ships remain unassessed, not “no hold.” Current sourcing-budget and affordability blockers are attributed to their ship, while treasury, construction and underwriting blockers remain company notices.

## Readability

Source is written for maintenance, not compression. Use idiomatic multi-line Go, descriptive intermediate values, explicit error handling, and focused functions. Gofmt is mandatory but does not make semicolon-packed control flow acceptable. The same principle applies to TypeScript and migration SQL.
