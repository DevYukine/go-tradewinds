# Trading strategy and evidence

## Sources and limits

- [Official playing guide](https://tradewinds.kibb.io/skill.md)
- [Official OpenAPI schema](https://tradewinds.kibb.io/api-docs/openapi.json)
- [API index](https://tradewinds.kibb.io/)
- [Pokemon species source](https://github.com/PokeAPI/pokeapi/blob/master/data/v2/csv/pokemon_species_names.csv)

This is a partially observed economy. Remote books may be hidden, NPC orders requote, players consume liquidity, and future hazards are uncertain. No strategy can prove global maximum profit from the public information. The objective is the best executable, conservatively valued opportunity among observed candidates, followed by measured reinvestment. Every decision must distinguish an actual quote from an estimate.

## Units and clock

All monetary calculations use integer pence: 100 pence = 1 Crown. Freight rewards are per contract; charter rewards are per delivered ton. Rates are basis points. Ship speed is centiknots. The current world clock reports its acceleration; at time scale 60, one game day takes 24 real minutes. Profit rates, operating costs, and payback use game days. HTTP timeouts, diagnostic retention, and the server's 24-hour idempotency window use real time.

## Starter company

The account is `Yukine`, company `The Pokemon Pirates`, starter ship `Lapras`. Le Havre is the initial home port: it is a Phase 1 port with a short coastal route to Portsmouth and local provisions production. The observed Le Havre–Portsmouth route was 30 nautical miles. This is a route hypothesis, not a promise of profitable current prices.

The initial company receives a Hoy, eight tons of stores, four cannons, and 750,000 pence of seed capital under the current guide. The starter's working cargo space is reduced by its stores and guns. Never plan against nominal hold capacity alone.

Public expansion charters are especially useful early because their payout is observable even where the destination order book is not. Research found provisions and sawn-plank demand at Portsmouth, cannon demand at London, and silver demand at Cadiz. These are time-sensitive opportunities, not hardcoded trading instructions. Silver's apparently attractive payout does not justify taking a coastal Hoy through higher-rated waters.

## Comparing opportunities

For each feasible quantity, compare a complete operating cycle, not just a one-way gross spread:

`expected cash net pence = realizable sale/reward - acquisition - buyer customs - provisions - berth dues - remittance fees - incremental upkeep`

`expected net pence per game day = expected cash net pence / complete cycle duration`

A cycle includes loading and unloading, mandatory berth turnaround, expected waiting, sailing at loaded speed, and repositioning when the next profitable load is elsewhere. Reinvestment uses realized settlements, not mark-to-market estimates or a temporary vault balance increase.

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

## Sailing and idle costs

Reject any route whose edge or traversed zone exceeds the ship's hull rating. Check every leg, not just the destination. An attractive silver margin is irrelevant if the route is not legal for the hull.

Use route `safe_provisions_tons`, not the optimistic expected requirement, plus the appropriate port waiting allowance. Crew consume provisions while berthed too. Re-read stores before departure; a partial provisioning fill is not permission to sail. Guns consume hold space and newly built ships do not arrive ready to sail.

Berth handling imposes a minimum 30-game-minute lock, with extra handling time per ton. A departure refusal after a successful purchase must schedule a fresh departure attempt after unlock, never repeat the purchase. Port dues have a minimum six-game-hour berth charge. Anchored roadstead can avoid berth costs but does not erase provisioning needs.

Sound-toll routes are always rejected until an authoritative toll-cost observation source exists. Hazard odds are not published, so the planner must not invent a loss probability from a zone label or deduct an arbitrary percentage of gross revenue as an expected expense. Rank observable cash margins and preserve the hard hull, provision, deadline, liquidity, and reserve constraints. Show hazard and price-uncertainty stress scenarios separately from the expected-net calculation, with `probability_estimate: null` when unknown. A stress scenario is not a profitability gate.

Live calibration exposed why this distinction matters: the initial Le Havre to Portsmouth provision charter had a modeled 36,055-pence cash margin for 24 tons, but an unsupported 14.5% revenue haircut rejected it. Those prices are a dated observation, not a standing trade signal. Every order must still use refreshed depth and current charter terms.

The first live roundtrip on 2026-10-22 realized a 91,953-pence loss: 468,600 pence acquisition, 13,074 buyer customs, 783 attributable expenses, and 390,504 spot-sale proceeds. Charter delivery had failed because the automation key lacked `market_derivatives`; incorrectly treating that authorization error as permanently unavailable demand caused the lower-price fallback after the key was repaired. Keep this loss in the durable settlement history. Authorization and throttling failures are not evidence that a charter is closed or the cargo ineligible.

Definitive authorization or throttle refusals complete only the rejected journal request, not a domain opportunity. After the persisted cooldown, rejected treasury and build control intents are released for fresh state-based planning with a new idempotency key. Authorization failures still stop execution until repaired. Ambiguous actions remain attached to their original immutable request and are never discarded by this recovery.

After permission repair, the next 24-ton roundtrip completed at 19:07:46 UTC with 526,992 pence charter proceeds, 468,384 acquisition, 13,209 fees, and 508 attributable expenses: a realized 44,891-pence net gain. Its stores came from the starter allocation, so no newly purchased provisions were charged to that settlement. The two recorded cycles together remained 47,062 pence below their combined acquisition and operating costs. One profitable cycle validates execution, not a guaranteed future return; fleet expansion still uses the complete retained history, including the loss.

### Bounded offline replay (2026-10-23)

A deterministic replay used the snapshot captured at `2026-10-22T23:12:14.957Z`, with synthetically refreshed metadata and Lapras rewound from sailing to berthed, empty and holding 7.29839 tons of preloaded stores. This is not current market freshness, a live quote or a profit forecast. No network, database or live-executor interaction occurred.

With the recorded charter prices, 212,623 pence of regional capital supported a 5-ton lot with 12,674 pence of modeled economic net; 562,624 pence supported 24 tons with 65,654 pence of modeled net over 0.496814 game days. Net includes the replacement value of consumed preloaded stores, not just immediate cash outflow. Reserve-only capital, missing/empty destination bids and metadata aged three minutes were rejected. The cycle model now uses the guide's two 30-game-minute berth locks instead of the previous 12-hour base allowance, retaining the conservative four minutes per ton of handling, queue allowance and minimum berth dues.

The five actual retained settlements, including the first loss, totalled 59,167 pence net over 1,090,800 game seconds. A separate growth counterfactual assumed a **synthetic** 250,000-pence hull quote, 10,000 tons of source depth, 3,000,000 pence of capital, a charter lasting 100 game days, 46 tons of incumbent throughput per cycle and 1,192 pence/day of marginal upkeep. Its 832-pence initial-stores input used the **pre-fix consumption proxy**, not the full cost of provisioning an empty new hull. Adding zero or one synthetic healthy cycle (24 tons, 60,000 pence net, 43,200 game seconds each) failed the 30-day payback gate; adding two or three passed at 28.08 and 21.34 game days respectively. These historical replay results have not been recalculated with the corrected full-inventory commissioning budget. Those invented future cycles do not replace actual loss-inclusive proof or authorize a live build.

## Treasury and reinvestment

Cash is regional. Escrow is not spendable. Reserve enforcement includes outstanding obligations, collateral, near-term bills, and working capital in the billing region. Any negative vault, or any underfunded active/obligated region, blocks discretionary construction while safe income voyages continue. Empty unused regions need no artificial reserve. A remittance pays an observed fee and must preserve its source's protected funds. Several donors can fund a shortfall through sequential partial transfers; each durable transfer is followed by fresh balances before selecting the next donor.

The guide's daily fleet upkeep includes a nonlinear coastal/regional component:

`45 Cr × (max(0, coastal/regional tons - 40) / 100)^1.45`

It also includes `120 Cr × factors^1.3` and `1 Cr × oceanic ton`. Evaluate the actual marginal change from adding a hull, not the standalone ship's cost and not an average across the existing fleet.

A new hull needs all of:

1. A repeatable lane with at least two positive known-cost realized settlements and positive net across all retained lane evidence, including procurement failures and losses. Unknown costs disqualify that proof.
2. Two cycles of observed source asks after competing source/commodity repeat throughput, plus each incumbent's not-yet-purchased lot once. Spot trades likewise need two cycles of executable destination bids plus undelivered competing lots once.
3. For finite charters, remaining quantity of at least `outstanding undelivered lots + max(2, ceil(payback / cycle) + 1) × (new lot + competing contract repeat throughput per cycle)`, with expiry no earlier than payback plus one complete cycle. Future reposts and market requotes do not guarantee replacement demand.
4. A legal route for the hull and seasonal conditions.
5. A fresh available shipyard quote with a 10% price-slippage buffer.
6. Incumbent working capital, two new trading cycles, full initial safe-store inventory, guns and regional reserves funded separately. Price a new hull's entire required provisions lot against executable local asks, including buyer customs and per-ton integer-rounding allowance; consumption replacement value is not its commissioning budget. Three days of marginal fleet upkeep belong in the home billing region, including any required remittance fees.
7. A payback within the configured game-day horizon. There is no ship-count cap: recalculate the marginal economics and funded reserves for each additional hull.

Shipyard quotes are exact observations, not binding price locks: the build endpoint has no quote token or maximum-price precondition. Settle the actual build result and treat build, refit, and provision as separate durable actions. Guns cost 15,000 pence each under the current guide and reduce cargo space.

Allocate source, destination and contract reservations only to actual competing lanes, including committed sailing returns; unrelated hulls do not consume every lane's liquidity. Repeat throughput uses each committed lot and full cycle duration. Whole outstanding lots are reserved once, not once per future cycle; already delivered ordinary cargo on a returning ship consumes no remaining contract or bid capacity. Delivering sealed freight does not release ordinary trade reservations on a mixed voyage. Run actionable maintenance before sizing an idle hull's economic plan because guns reduce its hold; a hull without a local yard or funds for a refit must not veto otherwise viable construction. Assign compatible idle or newly commissioned capacity before buying another hull. One durable `__growth` mission controls construction, refit and provisioning across restarts.

The current published API exposes construction and refit but no voluntary ship sale, scrap or transfer operation. Do not invent a disposal route, count hypothetical resale proceeds or deliberately trigger bailiff seizure to finance expansion.

## Why not every financial instrument

Futures, insurance underwriting, marques, and speculative market-making are not automatically profitable. Research found no basis for treating absent counterparties as executable liquidity. Underwriting creates liabilities; a policy premium is not risk-free revenue. Privateering has traffic, ammunition, hull, legal, and recovery costs. Convoys also have a naming policy awaiting the owner. Keep these surfaces visible, but do not commit capital merely because an endpoint exists.

Revisit these choices when recorded market activity supports a measurable edge. Better information can change the ranking. The optimizer should expose its rejection reasons so an idle fleet is explainable rather than silently stuck.

## Durable economic record

Keep settled mission economics beyond the seven-day diagnostic window. Acquisition basis, actual proceeds, fees, provisions, and attributable operating costs must survive restarts and pruning. Journal each upstream mutation once and reconcile ambiguous results before recording profit. Transfers, seed capital, collateral principal, and unrealized inventory changes are not trading profit.

Working snapshots retain current requested child datasets and their metadata, not every historically visited market or route origin. A currently required child fetch failure retains its last-good value with explicit stale/error metadata. Durable journals, settlements, and retained diagnostics remain separate from this bounded working state.

Startup logs and skips incomplete registrations, unreadable credential rows, or missing API keys per account. Those failures do not stop healthy account workers or the read-only dashboard API.
