# Trading strategy and evidence

## Sources and limits

- [Official playing guide](https://tradewinds.kibb.io/skill.md)
- [Official OpenAPI schema](https://tradewinds.kibb.io/api-docs/openapi.json)
- [API index](https://tradewinds.kibb.io/)
- [Pokemon species source](https://github.com/PokeAPI/pokeapi/blob/master/data/v2/csv/pokemon_species_names.csv)

This is a partially observed economy. Remote books may be hidden, NPC orders requote, players consume liquidity, and future hazards are uncertain. No strategy can prove global maximum profit from the public information. The objective is the best executable, conservatively valued opportunity among observed candidates, followed by measured reinvestment. Every decision must distinguish an actual quote from an estimate.

## Units and clock

All monetary calculations use integer pence: 100 pence = 1 Crown. Freight rewards are per contract; charter rewards are per delivered ton. Rates are basis points. Ship speed is centiknots. The current world clock reports its acceleration; at time scale 60, one game day takes 24 real minutes. Profit rates, operating costs, and payback use game days. HTTP timeouts, diagnostic retention, and the server's 24-hour idempotency window use real time.

Server timestamps, not the workstation clock, govern berth unlocks and contract deadlines. A fresh clock fetch supplies a conservative interval from its request start, completion, and `real_utc`; unlocks use its lower bound and deadline admission uses its upper bound. The acceleration applies when converting elapsed real time to game durations, not a second time to server UTC. HTTP cooldowns and replay windows remain host-time deadlines.

## Starter company

The account is `Yukine`, company `The Pokemon Pirates`, starter ship `Lapras`. Le Havre is the initial home port: it is a Phase 1 port with a short coastal route to Portsmouth and local provisions production. The observed Le Havre–Portsmouth route was 30 nautical miles. This is a route hypothesis, not a promise of profitable current prices.

The initial company receives a Hoy, eight tons of stores, four cannons, and 750,000 pence of seed capital under the current guide. The starter's working cargo space is reduced by its stores and guns. Never plan against nominal hold capacity alone.

Public expansion charters are especially useful early because their payout is observable even where the destination order book is not. Research found provisions and sawn-plank demand at Portsmouth, cannon demand at London, and silver demand at Cadiz. These are time-sensitive opportunities, not hardcoded trading instructions. Silver's apparently attractive payout does not justify taking a coastal Hoy through higher-rated waters.

## Comparing opportunities

Rank new shipments by destination-ending contribution and time to delivery, not an imaginary compulsory empty return:

`destination net pence = realizable sale/reward - acquisition - buyer customs - consumed-store replacement - attributable harbour bills and upkeep`

`net pence per game day = destination net pence / delivery duration`

Full safe-store inventory and collateral still have to fit the hold and funded cash budget. Their principal is not all consumed expense. Loading, unloading, berth locks, waiting and loaded sailing speed remain part of delivery duration. A separate repeat forecast prices a legal ballast return, its stores, dues, upkeep and observed regional remittance fees; only that repeatable forecast supports fleet expansion. Reinvestment uses settled evidence, not mark-to-market estimates or temporary vault increases.

Evaluate quantities at actual order-book depth boundaries, available cash, hold capacity, contract demand, and deadline constraints. Buying the full hold can cross into an unprofitable ask level. Selling at the best bid is valid only for the quantity actually supported at that bid. Never treat NPC stock as buy-side demand. Exclude identifiable own-player orders.

Use actual integer customs and fees, conservatively rounding where required. Customs apply to purchases, not to the seller's proceeds. Record actual filled quantity and actual costs from IOC responses; a successful response may still fill zero or only part of the request.

Serial execution and fresh reads between ships prevent the fleet from repeatedly allocating the same cash or liquidity. Unresolved mutations block dependent spending until reconciled.

## Spot, charter, and freight distinctions

### Spot trading

Use complete accessible depth. Gossip has a source time and weaker visibility: it is not a fresh executable book and does not imply an arbitrary volume of demand. Discard unsupported capacity assumptions. A quote becoming visible again does not make an old observation new.

### Expansion and private charters

Charter delivery occurs at the contract's `origin_port_id`. Only goods carried in from a different port qualify. A local purchase cannot be immediately converted into a charter reward. Track evidenced voyages and eligible quantities; unknown imported history is not proof of eligibility. Exclude stores and sealed freight. Cap delivery by current remaining demand and compare the charter payout with actual executable market proceeds.

Public charters are first-come demand. Check their remaining quantity and expiry before purchase and again before delivery. If demand closes en route, evaluate the actual alternate market rather than pretending the original reward is guaranteed.

### Freight

Freight cargo is sealed and its collateral is tied-up capital, not an expense unless lost. Preserve each accepted obligation, including returned delivery deadline, carrier, destination, quantity, reward, and collateral. Do not count a collateral refund as income. Do not sell, consume, unload, or charter-deliver sealed cargo.

The acceptance deadline is authoritative; listing expiry is not the delivery deadline. Check active and terminal contracts explicitly because listing defaults may show only open offers. A refused delivery can still record a tender. Preserve that obligation and its waiting state instead of treating every 409 as a harmless no-op. Include waiting provisions in the budget.

Compare cargo and compatible freight together before rejecting either component: shared sailing and harbour costs can make the combination profitable even when one alone cannot carry the trip. Recheck remaining quantities and seal occupancy; never allocate the same capacity or cash twice. After a purchase has filled, compare future departure cash with actual executable local recovery. Paid acquisition is sunk for that choice but remains in the final realized profit/loss record.

## Sailing and idle costs

Reject any route whose edge or traversed zone exceeds the ship's hull rating. Check every leg, not just the destination. An attractive silver margin is irrelevant if the route is not legal for the hull.

Use route `safe_provisions_tons`, not the optimistic expected requirement, plus the appropriate port waiting allowance. Crew consume provisions while berthed too. Re-read stores before departure; a partial provisioning fill is not permission to sail. Guns consume hold space and newly built ships do not arrive ready to sail.

Berth handling imposes a minimum 30-game-minute lock, with extra handling time per ton. A departure refusal after a successful purchase must schedule a fresh departure attempt after unlock, never repeat the purchase. Port dues have a minimum six-game-hour berth charge. Anchored roadstead can avoid berth costs but does not erase provisioning needs.

Idle replenishment owns a durable maintenance intent. Roadstead observations may hide executable provisions asks, so one dock can be necessary to inspect the quay. A missing or unaffordable quote then retains the intent instead of repeatedly docking and going offshore. The wait is not free: berth dues and crew upkeep remain protected obligations.

At the quay, the controller quotes the whole residual store requirement at current depth, including buyer customs and conservative integer rounding. If local cash is insufficient, measured regional fees and donor surpluses must cover the complete requirement without consuming any incumbent commitment or operating reserve. Only one immutable remittance is staged at a time; every receipt is followed by fresh pricing and funding checks. Invalidating a never-sent order does not release its still-needed funded cash.

Standalone idle replenishment can use stock claimed only by another unsubmitted idle quote; otherwise shrinking depth can make both ships wait forever. Serialized execution persists the winner's request before another consumer runs. Every attached request keeps its depth claim, other economic consumers still reserve maintenance depth, and all cash commitments remain protected. Held maintenance is reusable fleet capacity, not justification to buy a replacement.

Partial fills, refusals and lost docking receipts resume maintenance; confirmed sufficient stores retire it rather than wait for optional guns. Gun-only docking checks refit affordability and hold capacity first. Mandatory commissioning armament is unchanged. Actual transfer fees and store purchases are expenses, never the transferred principal.

Sound-toll routes are always rejected until an authoritative toll-cost observation source exists. Hazard odds are not published, so the planner must not invent a loss probability from a zone label or deduct an arbitrary percentage of gross revenue as an expected expense. Rank observable cash margins and preserve the hard hull, provision, deadline, liquidity, and reserve constraints. Show hazard and price-uncertainty stress scenarios separately from the expected-net calculation, with `probability_estimate: null` when unknown. A stress scenario is not a profitability gate.

Live calibration exposed why this distinction matters: the initial Le Havre to Portsmouth provision charter had a modeled 36,055-pence cash margin for 24 tons, but an unsupported 14.5% revenue haircut rejected it. Those prices are a dated observation, not a standing trade signal. Every order must still use refreshed depth and current charter terms.

The first live roundtrip on 2026-10-22 realized a 91,953-pence loss: 468,600 pence acquisition, 13,074 buyer customs, 783 attributable expenses, and 390,504 spot-sale proceeds. Charter delivery had failed because the automation key lacked `market_derivatives`; incorrectly treating that authorization error as permanently unavailable demand caused the lower-price fallback after the key was repaired. Keep this loss in the durable settlement history. Authorization and throttling failures are not evidence that a charter is closed or the cargo ineligible.

Definitive authorization or throttle refusals complete only the rejected journal request, not a domain opportunity. After the persisted cooldown, rejected treasury and build control intents are released for fresh state-based planning with a new idempotency key. Authorization failures still stop execution until repaired. Ambiguous actions remain attached to their original immutable request and are never discarded by this recovery.

After permission repair, the next 24-ton roundtrip completed at 19:07:46 UTC with 526,992 pence charter proceeds, 468,384 acquisition, 13,209 fees, and 508 attributable expenses: a realized 44,891-pence net gain. Its stores came from the starter allocation, so no newly purchased provisions were charged to that settlement. The two recorded cycles together remained 47,062 pence below their combined acquisition and operating costs. One profitable cycle validates execution, not a guaranteed future return; fleet expansion still uses the complete retained history, including the loss.

### Bounded offline replay (2026-10-23)

A deterministic replay used the snapshot captured at `2026-10-22T23:12:14.957Z`, with synthetically refreshed metadata and Lapras rewound from sailing to berthed, empty and holding 7.29839 tons of preloaded stores. This is not current market freshness, a live quote or a profit forecast. No network, database or live-executor interaction occurred.

With the recorded charter prices, 212,623 pence of regional capital supported a 5-ton lot with 12,674 pence of modeled economic net; 562,624 pence supported 24 tons with 65,654 pence of modeled net over 0.496814 game days. Net includes the replacement value of consumed preloaded stores, not just immediate cash outflow. Reserve-only capital, missing/empty destination bids and metadata aged three minutes were rejected. The cycle model now uses the guide's two 30-game-minute berth locks instead of the previous 12-hour base allowance, retaining the conservative four minutes per ton of handling, queue allowance and minimum berth dues.

The five actual retained settlements, including the first loss, totalled 59,167 pence net over 1,090,800 game seconds. A separate growth counterfactual assumed a **synthetic** 250,000-pence hull quote, 10,000 tons of source depth, 3,000,000 pence of capital, a charter lasting 100 game days, 46 tons of incumbent throughput per cycle and 1,192 pence/day of marginal upkeep. Its 832-pence initial-stores input used the **pre-fix consumption proxy**, not the full cost of provisioning an empty new hull. Adding zero or one synthetic healthy cycle (24 tons, 60,000 pence net, 43,200 game seconds each) failed the 30-day payback gate; adding two or three passed at 28.08 and 21.34 game days respectively. These historical replay results have not been recalculated with the corrected full-inventory commissioning budget. Those invented future cycles do not replace actual loss-inclusive proof or authorize a live build.

### Shipment-margin sensitivity (2026-10-23)

A bounded pre-change replay used the recorded generation whose server clock was `2026-10-23T11:09:00.813635555Z` and whose clock fetch completed at host time `11:09:34.8338318Z`. It ran the actual voyage planner offline, without orders or live account writes. The recorded Le Havre provisions ask was 229.91 Crowns/ton against a Portsmouth charter reward of 211.45 Crowns/ton: no positive executable charter was selected.

The following counterfactuals changed only local provisions asks, retaining the recorded ship, routes, charter, customs and regional capital. They are synthetic price sensitivity, not observed remote supply or promised earnings. Each accepted lot was 29 tons; modeled net includes acquisition, customs, replacement of consumed stores and berth dues.

| Synthetic ask, Crowns/ton | Modeled net, Crowns/shipment |
| --- | --- |
| 210.00 | Rejected |
| 200.00 | 156.43 |
| 190.00 | 454.94 |
| 180.00 | 753.45 |
| 175.00 | 902.71 |
| 170.00 | 1,051.96 |

The 902.71-Crown case has only 13.56 Crowns remaining under the separately disclosed 14.5% revenue stress scenario; that stress is not a measured hazard probability. This supports seeking better supply and evaluating all costs, not forcing an 800-Crown minimum or spending against an imaginary quote. A smaller profitable shipment can be the correct choice when faster turnover or current liquidity dominates. Leaderboard net worth also includes assets and capital and is not directly comparable with our realized shipment profit.

The same recorded-price replay was repeated after the runtime changes and produced the same accepted quantities and net values. Improving execution cannot turn the recorded loss-making local spread into profit; better sourcing and avoiding unnecessary empty returns address that limitation.

### Destination replanning and bounded sourcing

New economic voyages settle at their delivery destination once cargo is empty, all sealed obligations are terminal, turnaround has elapsed, and authoritative offshore state is observed. They then compare opportunities from that port instead of automatically sailing empty to their original source. Previously persisted roundtrip missions retain their original return commitments and accounting.

When no ordinary executable opportunity exists, an empty, stationary ship may inspect another source for an open charter. Compare at most three source/charter pairs using catalog production or fresh public ask gossip, legal routes and conservative deadline slack. Catalog production is evidence worth investigating, not an executable inventory promise; an unstocked local manufacturer does not veto a better evidenced remote source. Public evidence never grants private market access or creates forecast sale revenue.

Only one sourcing probe may be active per account, including ambiguous requests. Normal probes retain a permanent allowance: the unspent 20,000-pence seed plus 2% of positive, loss-inclusive known commercial net, minus every prior probe's expense. Unknown commercial returns cannot replenish the seed, but do not erase its unspent balance; unknown prior probe spending blocks exploration. Normal probes do not repeat an unchanged evidence fingerprint.

After six observed idle game hours, an empty stationary hull can use a separate, bounded reinspection policy. Its remaining allowance is `max(0, 20,000 + floor(uncommitted cash / 20) - all permanent probe expenses)`. Use the larger applicable normal or idle allowance, never their sum; each probe is still capped by `min(20,000, floor(uncommitted cash / 50), remaining allowance)`. Uncommitted cash excludes operating reserves and all current commitments. This is discretionary risk capital, not measured profit or permission to buy speculative cargo.

The idle policy may revisit unchanged evidence only after the latest matching canonical or legacy probe completion is at least 24 real minutes old. A future completion timestamp blocks it. Maintenance, docking, selection and remittance do not reset physical idle time. Sailing, carried cargo, active freight or a port change do; sunk ships retire their record. Stale, failed, paused or regressed clocks and stale previous observations rebase without crediting the gap. A short restart preserves already earned hours; downtime is not automatically earned idle time.

Persist the chosen idle-policy marker and its expense ceiling before spending. Staging and first-send checks recompute current eligibility, available allowance and paid/projected exposure, including unclaimed bills; they cannot enlarge that ceiling. These discretionary-spend checks do not cancel already acquired cargo, sealed obligations or necessary safety recovery.

Include consumed stores, customs, berth/upkeep exposure, measured funding fees and safe-return contingency in probe expense. Separately fund the complete safe-store inventory needed for relocation, retreat and the actual source-to-charter continuation. The empty probe has zero revenue and quantity; public production and gossip are leads, not executable stock. Inspect at most three valid source/charter pairs per pass, with at most six route requests, progressing past illegal pairs instead of letting them exhaust the shortlist.

On arrival, inspect fresh local executable depth for at most six observed game hours. Compare feasible whole-ton quantities including probe and trading costs; recheck cash, safe stores and charter terms before purchase. Closed demand or an unprofitable book ends the probe without speculative cargo or an automatic return. Record its actual expense once, without treating treasury principal or the later shipment as duplicate revenue. Zero-ton probes do not establish profitable throughput for ship purchases. There is no 800-Crown hard floor: profitable turnover and capital efficiency matter more than an arbitrary shipment target.

Sourcing diagnostics retain one candidate-count summary and at most three ranked candidate details per pass. Equivalent decisions are deduplicated without treating a rolling ETA as a changed economic assessment; materially changed retained offers remain visible.

### Bounded idle and funding checks (2026-10-24)

A 12-case synthetic run of the production planner varied a 1,000-pence source ask, 24-ton capacity, charter reward and remittance fee. These are sensitivity checks, not live prices or earnings. Every admitted case selected 24 tons; “reject” means no positive funded quantity was admitted.

| Reward per ton | Net at 0% fee | Net at 1% fee | Net at 10% fee | Net at 50% fee |
| --- | --- | --- | --- | --- |
| 11 Cr | 12.72 Cr | 10.13 Cr | Reject | Reject |
| 15 Cr | 108.72 Cr | 106.13 Cr | 80.23 Cr | Reject |
| 30 Cr | 468.72 Cr | 466.13 Cr | 440.23 Cr | 212.30 Cr |

The actual executable also completed three stateful, disposable-PostgreSQL scenarios: donor-funded 56-ton purchase and departure; no idle probe before six observed hours, then a zero-cargo inspection without speculative buying; and one funded hull commissioned with four guns, four tons of stores and 13 tons of cargo after a process restart. These synthetic outcomes prove the execution paths, not live profitability.

An offline replay of the unchanged recorded snapshot at `2026-10-24T21:17:20.9312261Z`, without a mission overlay or refreshed prices, still admitted no ordinary voyage. Regional funding cannot turn an unprofitable local spread into profit. That snapshot held 1,520,673 pence across the company against the cheapest observed Amsterdam hull quote of 2,378,926 pence, before guns, stores, trading float and reserves. An immediate purchase was not affordable.

## Treasury and reinvestment

Cash is regional. Escrow is not spendable. Reserve enforcement includes outstanding obligations, collateral, near-term bills, and working capital in the billing region. Any negative vault, or any underfunded active/obligated region, blocks discretionary construction while safe income voyages continue. Empty unused regions need no artificial reserve. A remittance pays an observed fee and must preserve its source's protected funds. Several donors can fund a shortfall through sequential partial transfers; each durable transfer is followed by fresh balances before selecting the next donor.

Ordinary positive-contribution voyages can draw on reserve-safe donor cash instead of being limited to their origin vault. Quote the full immutable cargo-order bound, customs, safe-store cash, collateral, fleet bills and destination funding together; pure freight does not invent an ordinary-cargo purchase. Include observed remittance fees once in destination net, stress and repeat forecasts. The ship mission owns each sequential transfer, so company treasury work cannot pre-fund a merely conditional destination. Revalidate the pinned opportunity before funding or procurement and again at first send after rate-limit waits. If it disappears before cargo is acquired, settle paid fees as an aborted voyage rather than deleting them.

Stationary harbour obligations follow actual presence; sailing obligations follow the final authoritative route leg, not the departed origin retained in a ship record. A prospective destination still needs its configured reserve floor and the new arrival bill even when no current hull obligates that region. These are liquidity protections, not additional voyage expenses.

The guide's daily fleet upkeep includes a nonlinear coastal/regional component:

`45 Cr × (max(0, coastal/regional tons - 40) / 100)^1.45`

It also includes `120 Cr × factors^1.3` and `1 Cr × oceanic ton`. Evaluate the actual marginal change from adding a hull, not the standalone ship's cost and not an average across the existing fleet.

A new hull needs all of:

1. At least two positive known-cost delivered cycles and positive loss-inclusive account and lane net in the last 12 recorded host-UTC hours. Include complete qualifying cycles, in-window failures, losses and probes; unknown costs or unmeasured duration disqualify the proof. The interval is inclusive and a relevant future completion vetoes evaluation. This is a host-recorded recency policy, not a guarantee about server elapsed time. Older losses and uncertainty remain in permanent history; current unresolved actions, missions and liabilities remain protected regardless of age.
2. Two cycles of observed source asks after competing source/commodity repeat throughput, plus each incumbent's not-yet-purchased lot once. Spot trades likewise need two cycles of executable destination bids plus undelivered competing lots once.
3. For finite charters, remaining quantity of at least `outstanding undelivered lots + max(2, ceil(payback / cycle) + 1) × (new lot + competing contract repeat throughput per cycle)`, with expiry no earlier than payback plus one complete cycle. Future reposts and market requotes do not guarantee replacement demand.
4. A legal route for the hull and seasonal conditions.
5. A fresh available shipyard quote with a 10% price-slippage buffer.
6. Incumbent working capital, a reusable new trading float, full initial safe-store inventory, guns and regional reserves funded separately. Price initial provisions against executable asks including buyer customs and integer rounding; consumption replacement value is not the commissioning budget. Protect destination arrival bills and three days of marginal upkeep in their actual billing regions.
7. A payback within the configured game-day horizon. Compare feasible quantities across supported hulls and accessible yards, rather than selecting the first passing package. Rank incremental net per day against all newly committed regional cash plus funding fees, including new reserve floors—not just the hull price. Existing reserves are not charged again. There is no ship-count cap; recalculate depth, marginal upkeep and cash after every purchase.

Shipyard quotes are exact observations, not binding price locks: the build endpoint has no quote token or maximum-price precondition. Before its first send, construction must still have current shipyard access, an available quote, sufficient executable demand and the full commissioning and incumbent funding protections above. Purchases, provisions, refits and collateral likewise require current financial authority and funded reserves. Already attempted requests retain their immutable replay identity instead of being repriced as new spending. Settle the actual build result and treat build, refit, and provision as separate durable actions.

Allocate source, destination and contract reservations only to actual competing lanes, including committed sailing returns; unrelated hulls do not consume every lane's liquidity. Repeat throughput uses each committed lot and full cycle duration. Whole outstanding lots are reserved once, not once per future cycle; already delivered ordinary cargo on a returning ship consumes no remaining contract or bid capacity. Delivering sealed freight does not release ordinary trade reservations on a mixed voyage. Run actionable maintenance before sizing an idle hull's economic plan because guns reduce its hold; a hull without a local yard or funds for a refit must not veto otherwise viable construction. Assign compatible idle or newly commissioned capacity before buying another hull. One durable `__growth` mission controls construction, refit and provisioning across restarts.

Commissioning stores and funding fees remain operating costs through handoff, abort or hull loss; they cannot disappear when the growth control is deleted. Preserve unknown cost state rather than manufacturing profit. Routine planning and first-send procurement use the same residual quantities as growth, so an ordinary ship cannot spend a returning ship's reserved next lot. Re-price changing dues without waiting for the quote to stop changing.

The current published API exposes construction and refit but no voluntary ship sale, scrap or transfer operation. Do not invent a disposal route, count hypothetical resale proceeds or deliberately trigger bailiff seizure to finance expansion.

## Underwriting and other income

Underwriting is an executable strategy, not a display-only market. It runs after shipping, existing obligations and viable fleet investment have protected their capital. Admission currently uses a minimum 500-basis-point slice of an unchanged **hull-only** policy, excludes every company owned by the player, and requires fresh policy, ownership, cash, catalog, rate and authoritative clock observations. Missing coverage is not zero coverage. Cargo-inclusive policies remain unpriced because a hull sinking outcome does not establish their cargo-loss economics.

At least five valid closed hull-only policies on the same directed lane are required. Duplicate IDs do not increase the sample; changed terms, contradictory terminal outcomes and later nonterminal observations are censored, not counted as safe returns. A Beta(1,1) estimate uses `(losses + 1) / (closed policies + 2)`. Premium must exceed modeled claims and the opportunity cost of currently observed shipping. A fixed-look nominal 95% Wilson upper sensitivity is disclosed separately: neither estimator is a calibrated promise about hull, season, correlation or future traffic. Creation-to-last-update duration is a lock-time proxy, not a settlement deadline.

This cohort pools all recorded eras. It is **not calibrated to the announced cargo-value targeting and ship-capture rules**. The published contract has no reliable deployment boundary or historical cargo, gun, convoy and zone observations with which to reconstruct a comparable post-patch cohort. A hull-only declaration does not prove that a foreign ship is empty or that its whole cargo was declared. The positive-net decision therefore remains a bounded model-based pilot, not measured post-patch expected profit; it must not justify increasing the slice or the permanent loss allowance.

Reserve the full possible collateral loss. The permanent pilot starts at 10% of uncommitted cash; active collateral and realized losses consume it, and only 10% of positive settled underwriting net replenishes it. It does not reset on restart. Before the debit region is known, the slice must fit protected cash in the home region, policy-origin region and every other capable vault. After the first accepted slice, further underwriting waits for a unique, identity-linked collateral debit to an owned vault. An equal-sized unrelated transfer is not proof. Actual vault cash already excludes escrow; do not deduct accepted collateral from that cash a second time.

The immutable 200 response must identify one exact owned slice with present, bounded collateral and premium amounts. Membership or aggregate coverage cannot reconstruct a lost receipt. Preserve ambiguous requests under their original key while unrelated safe fleet work continues. Premium remains an entitlement until an unchanged terminal hull-policy observation establishes earned premium and claims. Safe closure returns principal; loss closure consumes collateral. Net is earned premium minus claims and fees, never collateral returned plus premium. No undocumented insurance fee or voluntary release endpoint is assumed.

| Mechanic | Current economic treatment |
| --- | --- |
| Spot, charters and freight | Executable depth and deadlines; compare compatible loads and destination contribution |
| Source inspection | Explicitly bounded information cost, followed by fresh local pricing |
| Shipbuilding and refit | Supported marginal throughput, complete commissioning cash and observed payback |
| Underwriting | Minimum evidence-positive hull-only pilot with full-loss and shared-capital limits |
| Regional remittance | Observed fee and reserve-preserving funding, not income |
| Warehouses and factors | Access and storage have recurring/capital costs; ownership is not proof of a profitable trade |
| Futures and market-making | No demonstrated executable edge justifies speculative exposure |
| Marques | Supported but off by default (`MARQUES_ENABLED`); when enabled, issued only from the server's traffic-priced quote, outside our own fleet's exposure and within a small budget with a realized stop; see the update audit below |
| Convoys | Require economic justification and an owner-provided naming theme before creation |
| Ship disposal and purchase | Players may list and buy ships in port; not used yet because a bought hull keeps its seller's name |

### Announced piracy update (2026-10-24)

The owner's announcement adds cargo-value targeting, variable seizures, capture, stronger corsairs, single-victim convoy raids, full-cargo declarations and more collier armament. At this audit, the [published guide](https://tradewinds.kibb.io/skill.md) still describes fixed quarter-loss raids, and the [OpenAPI schema](https://tradewinds.kibb.io/api-docs/openapi.json) still lists only `berthed`, `anchored_roadstead`, `sailing` and `sunk`. These documents do not establish whether the announced server behavior is already deployed.

| Area | Controller treatment and remaining limit |
| --- | --- |
| Rich cargo and variable raids | Use actual remaining manifests and preserve sealed reservations; neither a fixed 25% loss nor an invented cargo-value encounter multiplier is used to settle a voyage. Unpublished attack and capture odds remain unpriced. |
| Stronger corsairs | Require fresh live zone observations and a legal route before a new departure, including the post-rate-wait preflight. A zone rating is not a calibrated encounter probability. |
| Ship capture | Existing `claimed_loss` underwriting closure consumes the full recorded collateral, irrespective of the loss's cause. The ship-status, ownership-transfer and event representation of capture is not yet established by the published contract; missing ships are not invented sinkings or completed settlements. |
| Convoys | Observe existing membership and prohibit independent departure while bound. Do not assume equal `1/N` victim risk when valuable holds are weighted, create convoys automatically, or invent a pooled-gun probability discount. |
| Own voyage policies | Protect every observed own open/active policy from new cargo loading and a conflicting departure. This preserves the observed declaration prospectively; without the original manifest it cannot prove that earlier external loading was compliant. |
| Premiums and underwriting | Use observed policy premiums, not a fixed premium estimate. Preserve full-loss, shared-capital and permanent-pilot limits; all-era policy outcomes do not establish post-patch capture risk. |
| Collier guns | The [live catalog](https://tradewinds.kibb.io/v1/world/catalog) reports `max_cannons: 4`. Existing maintenance targets `min(4, max_cannons)` before new economic work. A larger future cap alone is not evidence that the extra cost and displaced cargo pay back. |

Recheck the authoritative contract before adding capture-specific terminal handling or changing the hazard model. Preserve unresolved ownership, policy terms and payments rather than assuming that a cancelled policy returned collateral.

### Recorded comparison and bounded simulation (2026-10-24)

The leaderboard refreshed at `2026-10-24T00:28:22.970382Z` recorded The Kraken Cartel at 161,497,433 pence net worth, TopStats Logistics at 11,034,033, and The Pokemon Pirates at 3,011,037. These are asset-inclusive net-worth observations, not shipment profit or an income-source breakdown. The owner's report that the leader profits from underwriting motivates evaluating that instrument; the public ranking alone does not prove the attribution.

A 12-game-day, 48-slot offline allocation run completed in 8.962 real seconds, using production voyage and underwriting evaluators, one shared regional cash pool, fixed observed depth, finite charters and no invented future policy supply. Three recorded captures had no admissible current cargo trade or available hull-only underwriting slice. Their no-trade result is not a prediction of zero future opportunity. At the latest capture (`2026-10-24T01:25:46.7867782Z`), 161 cargo alternatives and 48 policy rows produced no accepted immediate income opportunity.

A separate recorded-state discovery replay retained the two historical unknown-cost freight records and all captured known gains and losses. The corrected gate preserved only the unused 20,000-pence seed, protected 103,000 pence in the active region and found three legal source inspections among 282 pairs. Their modeled expenses were 3,364 pence to Calais, 3,364 to Dover and 1,687 to Le Havre; no source revenue was assumed. Missing private books must still be inspected on arrival. This replay omits unrecorded mission journals and is not live execution authorization.

Synthetic controls established that positive underwriting and shipping can coexist without spending the same cash twice, while a tight pool prioritizes shipping and rejects the additional policy. A separate stress run loses every admitted policy's full collateral with no premium credit. Stationary prices, proxy lock durations, absent future stock, unpriced shipping hazards and incomplete idle provisions limit these models; their results are not promised company profit. A real CLI against a local synthetic game fixture additionally executed one 50,000-pence collateral slice, recorded a 15,000-pence premium-only safe-settlement net and restarted without another mutation. That proves the execution path, not real-market profitability.

The CLI also exercised a full 50,000-pence collateral claim with 15,000 pence of earned premium: realized net was −35,000 pence, no principal was returned, and the remaining pilot allowance stayed at 55,000 pence after restart. Both settlement fixtures submitted exactly one underwriting request. These are synthetic contract-execution controls, not observed live underwriting returns.

### Fleet-compounding controls

A separate 12-game-day comparison ran in under three real seconds through the production sizing, growth-admission, capital-ranking and reserve calculations. Each expansion run and its no-expansion control used one shared cash pool and finite source/buyer depth. All incumbent shipping was funded first; successive builds stopped when no funded marginal package remained.

| Synthetic scenario | Initial cash, pence | Final cash without expansion, pence | Final cash with expansion, pence | Added hulls |
| --- | ---: | ---: | ---: | ---: |
| 384 tons replenished daily | 350,000 | 572,492 | 689,927 | 2 |
| 384 tons total, no replenishment | 350,000 | 496,180 | 340,932 | 1 |
| Shallow 104 tons replenished daily | 350,000 | 572,492 | 572,492 | 0 |
| Cash-constrained, 384 tons replenished daily | 112,000 | 334,492 | 278,916 | 1, on day 8 |

Final cash includes hull/gun purchases, withheld initial stores, fleet upkeep and idle dues; it is neither asset-inclusive net worth nor profit. Daily replenishment, stationary prices and the loss-inclusive historical seed were synthetic. Commissioning was instantaneous, repeat durations were rounded upward, provisions were finite, and hazards were not calibrated. The no-replenishment case spent each of its 384 source and buyer tons once: extra hulls accelerated the same finite trade but did not repay their capital. The late cash-constrained purchase likewise had not repaid by the horizon. These counterexamples make the dependence on continued profitable demand explicit; two observed spot cycles are a bounded admission signal, not guaranteed payback.

A separate real-CLI fixture built one hull, bought initial stores and four cannons, and executed two actual 48-ton deliveries with one controller-selected empty return. Three process runs retained the admitted allocation and issued no duplicate mutations. Its finite 104-ton source and buyer books were consumed by the two shipments plus an explicit external source order; no return plan or mission was injected by the harness.

The corrected permanent history, actual receipts and regional vault changes independently reconcile to **37,929 pence of operating net**, including 204 pence of commissioning stores/customs and all 75 pence of navigation charges exactly once. The 120,000-pence hull/gun investment is separate, not silently excluded from the vault reconciliation. This synthetic execution took 11.22 seconds, with accelerated authoritative arrivals and no calibrated hazards or fleet-upkeep schedule. It proves commissioning, conditional continuation and accounting—not live earnings or future market replenishment.

### Game update audit (2026-10-25)

Checked against the [published guide](https://tradewinds.kibb.io/skill.md) and [OpenAPI schema](https://tradewinds.kibb.io/api-docs/openapi.json) fetched on 2026-10-25. No request the bot already sends changed shape. Responses gained fields the bot now decodes: ship `created_at` and `dispatches_sealed`, ship class `can_seal_dispatches`, book level `is_npc`, gossip `best_player_*`, and Lloyd's estimates on open policy listings.

At that time the bot held one hoy and about 113,945 Crowns idle. Growth had last built on 2026-10-22: the lane-proof path needs two positive delivered cycles inside 12 real hours plus repeat destination depth, which one ship rarely observes. The owner asked for a small fleet of cheap hulls first and for idle cash to be invested. Capacity growth (see `docs/runbook.md`) prices new hulls from recent recorded fleet earnings instead. An observe-only run of an earlier development build, on an earlier copy of the live database and with a 12 real hour earnings window, admitted a hoy at Amsterdam (turnkey 24,003 Crowns, about 302 Crowns per game day of recorded net per ship over that 12 hour window after 11.92 Crowns of extra upkeep, payback about 91 game days). A later build with the 24 hour window held growth at 2026-10-26 00:15 UTC because a freight settlement with unknown costs completed inside the window. Such settlements recur from routine freight handling: the live database held four, all zero-ton freight, completed between 2026-10-23 03:47 and 2026-10-25 23:18 UTC. The owner asked that they no longer pause growth, so their recorded losses now reduce window net and their recorded gains are ignored. On a fresh copy at 2026-10-26 09:43 UTC the relaxed build held instead because no candidate hull had an executable profitable voyage; the existing hoy at Portsmouth found no destination with observed depth either. These are recorded rates, not forecasts: a new hull that finds no work lowers the per-ship rate and pauses further buying, and a ship loss in the window pauses it too.

| Update item | Treatment |
| --- | --- |
| Player orders in gossip | Where we have no live book, a fresh gossiped player bid's tons, minus our resting bids, our sales there since the observation and other missions' undelivered tons, can price a whole spot lot. Gossip's best bid covers the whole book, so the lot is admitted only when that best bid is above the player bid (a known non-player floor) and the lot stays positive with every ton valued at it less 25%, because the player order may be gone on arrival. Otherwise gossip still prices one ton. Gossip-priced lots never justify buying a hull. |
| Royal monopolies | `GET /v1/world/monopolies` is fetched every refresh. Crown (NPC) asks for a commodity in a region where another company holds an in-force monopoly are removed from planning books, so no plan relies on purchases the server refuses. Player asks stay. Buying monopolies is not used: the quote prices them at a tenth of Crown sales and they pay only with resale volume we have not measured. |
| Lloyd's quote and estimates | Underwriting uses the per-policy expected-loss upper bound when present (see runbook). Our own voyages are still not insured. |
| Marques repriced | The quote prices a marque at 60% of expected prize after the Admiral's tenth, from 7 game days of laden passages. A marque is issued only when the zone is outside our routes, ports and underwritten policies, the price is not the floor, at least one laden passage per game day was recorded, the zone has room under the 5,000 bps cap, expected prize is at least 1.4x cost, cost is at most 5% of liquid funds (10% for all marques in force), and three or more finished marques have not returned less than 0.8x their cost. While our marque is in force the planner avoids that zone. The prize estimate depends on past traffic and is not a measured outcome until marques finish. |
| Turnkey builds from player asks | Already reflected in yard quotes; no change. |
| Player charters | Already planned exactly like Crown charters. |
| Not adopted yet | Ship purchases (naming), commissions, envelopes and packets (no measured buyer demand), loans, the credit board, banks, ventures and ship shares (counterparty and default risk without credit history), convoy announcements. Idle cash goes to ships first. |

## Durable economic record

Keep settled mission economics beyond the seven-day diagnostic window. Acquisition basis, actual proceeds, fees, provisions, and attributable operating costs must survive restarts and pruning. Journal each upstream mutation once and reconcile ambiguous results before recording profit. Transfers, seed capital, collateral principal, and unrealized inventory changes are not trading profit.

Working snapshots retain current requested child datasets and their metadata, not every historically visited market or route origin. A currently required child fetch failure retains its last-good value with explicit stale/error metadata. Durable journals, settlements, and retained diagnostics remain separate from this bounded working state.

Startup logs and skips incomplete registrations, unreadable credential rows, or missing API keys per account. Those failures do not stop healthy account workers or the read-only dashboard API.
