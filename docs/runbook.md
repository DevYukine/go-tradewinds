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
| `CAPACITY_PAYBACK_GAME_DAYS` | Maximum payback for capacity growth priced from recorded fleet earnings, default 180 game days |
| `SMALL_FLEET_TARGET` | Fleet size below which idle cash funds the cheapest capable hull without an earnings record, default 5 ships |
| `MARQUES_ENABLED` | Issue letters of marque automatically; default `false` because the game charges the live price with no price cap |
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

Exploration-expense and underwriting tables are additive, but **Goose version 5 is a non-backward-compatible encryption cutover**. It converts mission recovery data, settlement identities and underwriting evidence atomically, authenticates the existing credentials, verifies account/company executors are stopped, and installs a permanent key verifier. A failure rolls back the conversion and its version entry. Do not run an older executable against version 5.

Before this upgrade, stop the old executor and wait for its ownership locks to release. Take a fresh custom-format PostgreSQL backup of that quiesced state, restrict its ACL to the owner/system, and retain the original credential key separately. Restore the dump to a disposable database and run the new executable there before migrating production. Keep the old binary, dump and matching key together as the rollback set; there is no supported destructive down-migration. If the new executor has already sent actions, reconcile them before considering a restore—restoring an older journal can otherwise repeat a real-world mutation.

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

Growth needs loss-inclusive, known-cost account and lane proof from the last 12 recorded host-UTC hours, including at least two positive delivered cycles. A current-window loss, unknown cost, missing duration or future completion can block it. Older terminal uncertainty remains visible in permanent history without automatically vetoing a new, fully funded opportunity; unresolved requests, active missions and liabilities do not age out. The runner captures one evaluation time and recomputes the window before the first build send. An already purchased hull continues commissioning without requiring fresh build proof.

Growth also needs a current available quote, two cycles of spare source depth and, for spot trades, two cycles of executable destination bids. Reserve competing throughput separately for shared source/commodity, destination/commodity and contract, including ships already committed to sailing returns. Finite charters must cover payback plus a cycle and remain open for that full horizon; future reposts are not demand.

Any negative vault or underfunded active/obligated region defers construction, not safe income voyages. Protect incumbent working capital, the reusable new trading float, full initial stores, guns, the quote cushion, destination arrival bills and three days of incremental upkeep in the home billing region. Treasury transfers must first fund those regions at observed fees. Compare supported hulls and feasible quantities using their complete incremental capital commitment. Empty unused regions need no artificial reserve; escrow and hoped-for proceeds are not spendable cash. The game now lets players list and buy ships in port, but the engine neither sells nor buys listed hulls yet: a bought ship keeps its seller's name, which conflicts with the Pokemon naming rule until the owner decides.

#### Capacity growth

When the lane-proof path above refuses or finds no package, capacity growth decides instead. Idle cash is meant to become ships, not sit in the vault.

Below `SMALL_FLEET_TARGET` ships, idle cash funds the cheapest capable hull that has an accepted first voyage at its yard after incumbent commitments. Recent coffee-house gossip of other companies' prices counts as that evidence here, alongside observed depth and live contracts. No earnings record or payback is required, but every build must still:

- leave the buffer below (protected reserves plus one cargo-cash lot for every ship including the new one), so trading is never starved;
- cost at most a third of surplus cash including the 10% quote cushion, guns and stores, so the fleet grows step by step and stops when cash runs down;
- wait 90 real minutes after the newest ship joined, 24 real hours after a ship loss or build price overrun, and for any build already in progress.

In this mode a sourcing relocation does not pause growth. From the target onward, a hull is bought only when all of these hold:

- the candidate hull has an accepted voyage at the yard after incumbent ships' commitments are removed, priced from observed destination depth or live contract terms (gossip-priced lots do not count);
- the newest hull has completed a positive delivery (one ship counts as proven), and no ship joined the fleet in the last 90 real minutes;
- all-time known-cost realized net plus recorded losses from unknown-cost settlements is positive; unknown gains never offset those losses;
- the last 24 real hours of realized net is positive with no future completion. Settlements with unknown commercial or relocation costs do not pause growth: like the all-time figure, their recorded losses are charged and any unknown gain is ignored. Aborted zero-ton build/refit closeouts with zero acquisition and proceeds are accepted only for an unbuilt or still-surviving hull; their recorded fees, stores and expenses still reduce window net;
- no hull was lost in that window. Sunk hulls without a reliable loss timestamp block growth; a mission hull missing from a fresh complete fleet list is timed as a loss at that observation. First observed loss times persist across restart;
- that window net per active ship game day, minus the hull's incremental daily upkeep, repays hull price, a 10% quote cushion, guns to `min(4, max_cannons)` and initial stores within `CAPACITY_PAYBACK_GAME_DAYS`;
- surplus cash above protected reserves covers that capital plus a buffer of `max(RESERVE_PENCE, 15% of hull price)` and one median recent known-cost lot of cargo cash per ship including the new one. Only the capital and the yard region's own reserve must be in the yard's region; the buffer is checked fleet-wide and is not remitted.

Established-fleet candidates are ranked by net earnings per game day over capital; fleet-fill candidates by lowest capital. A larger hold scales the established-fleet rate (by hold over fleet average hold, clamped to 0.5x to 2x) only when recorded deliveries fill at least 80% of effective hold. Packets are never bought for cargo. From the fleet target onward, growth does not run while a sourcing handoff is active.

The durable `__growth` intent builds the hull turnkey, arms it at the yard, records the commissioning settlement and deletes itself. The hull then joins normal planning; idle maintenance buys its stores. If the yard evicts the hull to the roadstead before it is armed, the intent docks it again. If the yard region can no longer pay for the guns, the hull holds unarmed in port instead of trading. A definitive refit refusal or three refit attempts releases the hull to normal operations, where maintenance arms it later. Growth decisions record the chosen candidate's capital, buffer, rate and payback, or the blocking reason.

Immediately before the first send, the runner fetches a live turnkey quote, checks the admitted ceiling and uses the reserved quote cushion before requiring extra yard-region funds. The player-sourced material share must fit within the remaining price headroom. A live quote that fails any of these checks backs off that yard/class for 6 real hours, so the same build is not readmitted every pass. The game has no atomic maximum-price field: another company can still change asks between the quote and the build. A receipt above the admitted ceiling records both prices, warns once on fresh dispatch with scrubbed outcome fields, backs off that yard/class for 6 real hours and pauses all capacity growth for 24 real hours. A domain build refusal also backs off the yard/class for 6 real hours; throttling and authorization errors (401, 403, 429) only wait for their normal cooldown.

### Idle work and regional funding

There is no minimum shipment-profit target: the runner already considers smaller positive-contribution loads. Ordinary voyages may fund their full cash requirement from reserve-safe regional donors, after observed transfer fees. In System → Decisions → Route and economics, “Regional funding fees” is a modeled expense already included in the displayed contribution; transferred principal and collateral are not profit or expense. Older records without the field remain unchanged.

When no ordinary income is executable, six observed idle game hours permit a limited source reinspection. Maintenance does not erase the idle clock; actual sailing, cargo, active freight or changing ports resets it. Stale or paused observations and long restart gaps earn no hours. The current hold identifies whether the ship is waiting for idle eligibility, reinspection cooldown, risk allowance, safe funding or usable market evidence. This is not a promise that a profitable order exists at the next port.

The idle allowance is the larger of the normal sourcing allowance and `max(0, 20,000 + floor(uncommitted cash / 20) - all permanent probe expenses)`, not their sum. Per-probe exposure remains at most 20,000 pence and 2% of uncommitted cash. Repeating the same evidence needs 24 real minutes since its latest completed probe. Unknown probe costs, active/ambiguous probes and unsafe or stale evidence remain blockers. Never reset settlements, idle records, idempotency keys or reserves to force another attempt.

### Underwriting

Money shows owned underwriting exposure and settled outcomes above financial history; public policy listings remain in Trade, under Exchange and Insurance. An active premium is an entitlement, not settled earnings; returned collateral is principal. The engine may take a 500-basis-point hull-only slice only when its directional closed-policy model has positive net after modeled claims and shipping opportunity cost, and the full possible loss fits both current protected cash and the permanent pilot allowance. See `docs/strategy.md` for model assumptions and limits.

When an open policy carries Lloyd's expected-loss estimate (`lloyds_expected_loss_bps`), that estimate replaces the closed-policy model: the slice's premium share minus collateral times the estimate, minus the same opportunity cost, must be positive and `premium_margin_bps` must be at least 50. The estimate is an upper bound on the expected claim, so this is conservative. Without closed-policy history, collateral lock time comes from the route's sailing days, else the 15 game day policy lapse. Cargo-inclusive policies stay excluded because partial claims are not yet accounted per position. Lloyd's fields are kept out of the policy terms hash, so they never mark a stored policy as changed.

`underwriting_selected` records the estimate. Confirmed `game action committed` records the exact owned slice, collateral and premium entitlement; `underwriting settled` records earned premium, claims, returned principal and net separately. Missing receipt, changed terms or unexplained ownership remains unresolved. After an accepted slice, further underwriting waits for a unique identity-linked debit to an owned vault. This hold does not disable unrelated safe fleet work. Do not replace an ambiguous idempotency key or infer payment from policy coverage.

### Royal monopolies and gossiped player bids

Each refresh reads the public monopoly list. While another company's monopoly is in force for a commodity in a banking region, Crown asks for it in that region are dropped from planning books; player asks remain. If the list cannot be fetched, the last good list is kept and entries past `expires_at` are dropped. A purchase refused with a monopoly `409` therefore points to a monopoly granted after the last refresh.

Spot plans may sell a whole lot into a gossiped player bid at a remote port where we have no live book. The usable tons exclude our resting bids, our sales there since the observation and other ships' undelivered cargo to that buyer. Gossip's best bid is the whole book's best bid, so a non-player floor is known only when it is above the player bid; the lot is admitted only then, and only if it stays positive with every ton valued at that best bid less 25%. When the player bid is the top of the book, gossip still prices one ton. The player order is anonymous and can be filled or withdrawn before arrival; on arrival the ship sells into whatever bids are live. Where a live book exists, it always wins over gossip.

### Letters of marque

Issuance is off by default (`MARQUES_ENABLED=false`): the game charges the live price with no price cap, and the owner chose not to accept that risk for marques yet. With the switch off, zone quotes and the route lookups used only for marque exposure are not fetched, and a persisted intent that was never sent is dropped before sending; marques already in force, if any, are still avoided by planning. With the switch on, the rules below apply.

The runner quotes every sea zone at 500 bps for 60 game hours, at most every 30 real minutes, and issues at most one marque per evaluation through the `__marque` control mission, only in execute mode and only when no `__growth` intent is pending. Admission rules and budget are in `docs/strategy.md` (game update audit). The cost is paid from the home vault and must leave its protected reserves and pending cargo, stores and collateral commitments intact; a pending issue reserves that cost without blocking unrelated ship work. Right before sending, the zone is quoted again and the issue is dropped if the fresh quote fails admission or costs more than 10% above the admitted price; the game has no maximum-price field. Finished marques' cost and prize are kept in the working snapshot so the realized stop survives list limits. A domain refusal backs off that zone for 6 real hours.

Exposure covers our ships' current legs, persisted mission routes including return and recovery legs, the ports where our ships lie, the home port and routes of policies we underwrite. While our marque is in force or pending, new voyage plans and sourcing relocations avoid its zone; loaded voyages already planned are not held, to avoid stranding cargo. Prize payments arrive as `marque_prize_paid` events and in `fleet/marques` as `prize_earned_pence`.

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

- **INFO:** service startup, one initial fleet snapshot, confirmed actions, authoritative arrivals, established stream/dataset recovery and recorded settlements. Amounts are integer pence; unknown costs never appear as measured profit.
- **WARN:** a new recoverable outage, unexpected rejection, reconciliation required, explicit ship loss, or a storage/diagnostic failure needing attention. An ordinary handled conflict is not automatically a warning. Repeated unresolved-action warnings use the bounded seven-day decision cache and can recur after eviction or restart; dashboard read/write warnings are sampled per operation.
- **ERROR:** terminal account-worker, ownership, service, lifecycle or response-encoding failure. Expected shutdown cancellation is silent.
- **DEBUG:** request attempts, routine dispatch, handled conflict/cooldown detail, repeated connection failures, Fx lifecycle and migration detail. Enable temporarily for diagnosis; successful dock/offshore actions remain INFO.

`LOG_COLOR=auto` (also the unset/empty default) enables colors only when stderr is a terminal and `TERM` is not `dumb`. `LOG_COLOR=always` also colors redirected output, useful for a visible PowerShell window that tees logs to a file. `LOG_COLOR=never` disables colors. The presence of `NO_COLOR`, even with an empty value, overrides every color mode. Other `LOG_COLOR` values fail startup. Levels receive distinct colors and structured context is dimmed; stripping the encoder's SGR sequences leaves the same records as plain mode. Control characters in upstream messages, labels, categories and fields are escaped so they cannot inject terminal lines or commands; invalid UTF-8 is normalized when represented as JSON.

The runner emits `fleet status` once per account after the first coherent, persisted fleet observation. It does not emit a minute heartbeat. Stale or missing initial fleet data does not consume that announcement. `reconciled_at` is the completed cycle time; `fleet_observed_at` is when the fleet was fetched. Counts describe that observation: `in_port` includes berths and roadsteads. The first holding reason is scrubbed before a 512-character cap; the count is reasons, not waiting ships.

Subsequent fresh REST transitions from sailing to a berth or roadstead emit `ship arrived`; an explicit transition to sunk emits `ship sunk` at WARN. Startup history, a passed ETA, missing rows and stale snapshots never invent those events. Missing rows also do not erase the previous sailing observation. Other healthy polling and unchanged idle planning stay quiet. Silence is not proof of health or profit: use recorded observation times and current company activity in Overview.

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
| Dashboard listener/server exit | INFO address/mode at startup; ERROR unexpected stop |
| Request diagnostic persistence | First outage WARN with method, redacted path, status and safe cause; repeated failures quiet, recovery INFO, shutdown silent |
| SSE disconnect/gap/recovery | First outage/gap WARN, repeated disconnected attempts DEBUG, persisted recovery INFO; owner loop emits lifecycle messages |
| Dataset or contract-page fetch/validation | First outage WARN with dataset and safe cause; repeated outage quiet; successful recovery INFO |
| Account lease loss | ERROR account, ownership state and safe database cause; cancel execution immediately |
| Lease connection/pool cleanup | WARN with safe cause, not a content-free warning |
| Migration lock release | WARN safe database cause |
| Goose migration detail/failure | DEBUG normal detail, ERROR fatal migration; no raw connection strings |
| GORM SQL logging | Disabled; returned errors are handled at their operational boundary |
| Dashboard reads/metrics/writes | Sampled WARN operation/cause; expected request cancellation silent |
| Dashboard response encoding | ERROR: response cannot be represented safely |
| Game HTTP request trace | DEBUG method, redacted path, status and duration; no body/header dump |
| Rate limiter/retry transport | No duplicate business log; owning action/dataset reports the classified result |
| Retention | WARN failed prune and safe cause; no success heartbeat |
| Fx lifecycle | DEBUG detail; terminal failures ERROR |

The severity policy follows [OpenTelemetry](https://opentelemetry.io/docs/specs/otel/logs/data-model/), [OWASP logging guidance](https://cheatsheetseries.owasp.org/cheatsheets/Logging_Cheat_Sheet.html) and [Zap levels](https://github.com/uber-go/zap/blob/master/zapcore/level.go). [OpenKore](https://github.com/openkore/openkore/blob/master/src/Log.pm) separates operational messages and debug domains; [KoLmafia](https://github.com/kolmafia/kolmafia/blob/main/src/main/java/net/sourceforge/kolmafia/RequestLogger.java) separates readable output from session records; [Baritone](https://github.com/cabaletta/baritone/blob/f2679bea294e51e3d2e83bda562451f8f9d2cf5a/src/api/java/baritone/api/utils/Helper.java) distinguishes direct and debug notices. These support separate operator and diagnostic channels, not copying their UI or adding log knobs here.

A disposable PostgreSQL/real-CLI observation ran for 77.9 seconds through six completed cycles: one startup snapshot, one arrival, one explicit loss, colored output, no leaked fixture credential and no game mutations in observe mode. Ctrl+Break exited successfully without an account-failure error. CLI fixtures sharing the same account or company identity must run sequentially even in different schemas: PostgreSQL advisory ownership is database-wide. This establishes the exercised lifecycle behavior, not live profitability.

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

Start in Overview: read the company's controller observation time, current hold, next recorded check and each ship's current status/location/cargo. The map's position can be interpolated; the ship's REST state is authoritative. A fresh page response is not proof that the executor advanced.

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

Run database integration tests with the disposable URL, then smoke the actual CLI and database API. For concurrency changes, run `go test -race -p 1 -timeout 30m ./...` when a working C compiler is installed. Report an unavailable race toolchain instead of calling that check passed.

Run the fixed-company logging and underwriting CLI fixtures serially with each other and with the entire Go suite. Different disposable schemas do not isolate PostgreSQL company advisory locks. A competing fixture can make `Runner.Run` return `ErrAccountLocked` before its first request; a REST-capture timeout is not proof that the HTTP path stalled.

Engine tests run in parallel, six at a time by default (pass `-parallel` to change it). Each uses its own schema and fake game server, and test connections turn off `synchronous_commit` because the schemas are thrown away. A test that calls `Runner.Run` takes the company advisory lock and waits on real-time gates, so it must not call `t.Parallel()`: Go runs such serial tests before the parallel ones.

Go's timeout covers an entire package, not each test. The serialized PostgreSQL scenarios can exceed shorter package-wide limits even while individual cases continue passing. Keep their bounded waits and assertions intact, and preserve per-test output when diagnosing a suite timeout.

For dashboard changes, start both real services, exercise account selection, map selection, fleet detail, data views, diagnostics filters, themes, and keyboard navigation. In the automation ledger, expand route economics, confirm collateral and stress disclosures, and sort the cash header in both directions: amounts must sort numerically, with unavailable values last. Inspect screenshots at the viewports in `AGENTS.md`, verify no page overflow, and check browser console output. Production data must never be replaced by demo fixtures to hide an integration problem.
