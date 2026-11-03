# Dashboard design

## Brief and acceptance

The owner rejected the first dashboard. This is a replacement, not a reskin. The dashboard is a working fleet chart and trading ledger for Tradewinds, initially Yukine's company, The Pokemon Pirates. It must remain useful with several configured accounts and companies.

The first screen answers: where are the ships, what are they doing, what needs attention, and what money has actually been earned? Account, company, ship, cargo, contract, warehouse, shipyard, treasury, market, world and diagnostic details remain reachable. Do not hide missing observations behind zeroes or show planned profit as realized profit.

Every frontend change starts with the official Anthropic `skill://frontend-design`. The skill is installed in the active omp profile, with its upstream license and source attribution.

## Direction: a working maritime chart

The map is the distinctive element. Everything around it is quiet, legible operating software. Use the visual vocabulary of hydrographic charts: pale water, subdued land, clear port symbols, recognisable ship silhouettes, and a strongly distinguished selected route. This is not a pirate-themed landing page or an antique parchment illustration.

No giant greeting, editorial hero, decorative statistic-card grid, uppercase eyebrow labels, numbered filler, ornamental gradients, fake activity or invented charts. Company and ship names are real content, not branding placeholders.

### Core tokens

| Token | Light | Dark | Role |
| --- | --- | --- | --- |
| Canvas | `#F5F6F3` | `#151514` | Main application surface |
| Water | `#E1E7E4` | `#191D1C` | Working chart background |
| Land | `#CBD3C8` | `#2A2F2B` | Coastline and land polygons |
| Ink | `#202421` | `#E9E8E4` | Primary text and symbols |
| Route | `#1F6E7A` | `#74B8C2` | Selection, focus and active route |
| Attention | `#B0492F` | `#F09C83` | Actionable warning or loss |

Derive secondary surfaces and separators from these roles. Status colour must also have a word or symbol. Validate actual text contrast in both themes; never apply muted opacity to an entire container. Tailwind theme variables and CSS custom properties are the single token source.

Use locally hosted IBM Plex Serif for display headings (`h1` and `h2`) and IBM Plex Sans for interface text, smaller headings, controls and chart labels. The serif headings give the working ledger its character without turning the interface into an antique illustration. Main headings are 26 px, section headings 20 px and subheadings 16 px; body text is 14 px, with 12 and 13 px for compact metadata and table cells. Use tabular figures for money, quantities and timings, not a separate monospace font for ordinary labels. Keep body copy below 80 characters per line, sentence case and left aligned. Numeric table columns align right.

### Layout

Overview retains company state, observed ship activity, current holds, next-check targets and clearly separated money facts. These facts require no ship selection or diagnostic drill-down. A visible Fleet chart link opens the dedicated full-screen `/chart` page; the overview no longer embeds a compact map. The vessel ledger also links to the chart. Account/company scope and account-qualified ship selection are shared across the dashboard and chart without adding or migrating persisted dashboard View values.

Eight icon-led primary destinations expose dashboard views through adjacent contextual tabs and the full-screen Fleet chart:

- Overview: company operations and financial context, with an entry to Fleet chart.
- Fleet chart: dedicated spatial fleet operations.
- Fleet: Vessels and Voyages.
- Journal: chronological company activity and Bulletin.
- Money: Performance, Treasury, Loans and Warehouses.
- Trade: Contracts, Exchange and Shipyards.
- World: World and Standings.
- System: Decisions and Diagnostics.

Keep the primary navigation short without hiding routine status behind more clicks. Each view uses a few purposeful sections rather than repeated statistic cards. Money shows owned underwriting collateral, unsettled premium, settled net and risk allowance directly above financial history; public policy listings stay in Exchange. Decisions retains technical planning evidence, not the only explanation of why a ship is waiting.

A card-grid alternative was rejected: it fragments the fleet and prioritizes decoration over the operator's task. The dedicated `/chart` page is a chart room, with the map occupying the available viewport and purposeful panels for fleet scope, vessel selection and route/ship inspection. Keep controls grouped in those panels rather than scattering floating controls across the water. The overview and financial ledgers remain separate, intact destinations; spatial work does not replace their financial context.

At narrow widths, chart room panels become a responsive bottom sheet with accessible open and close controls, keyboard focus handling and internally scrollable content. Keep enough of the chart visible to preserve spatial context. The dashboard ship inspector remains beside the ledger when space permits and an accessible sheet on narrow screens. Journal keeps time, vessel, activity and cash visible together with wrapped text; wider analytical tables may scroll inside labelled regions. The page itself must not scroll sideways. Controls need visible focus and sufficient touch targets. Reduced motion disables decorative and map-flight animation; data remains current.

Keep native scrollbars available and thin, with a theme-aware border-colour thumb and transparent track. Never hide scrollbars to disguise overflow. Wide tables own horizontal scrolling with a stable scrollbar gutter; navigation and inspectors own their vertical overflow. Controls wrap within their containers instead of widening the page.

## Interaction and state

- Scope controls support all configured accounts/companies as well as individual selections. Show account and company identity wherever ambiguity is possible.
- Ship selection is shared by map and fleet table and includes the configured account ID plus ship ID. Refreshing data must not reset selection, map viewport, filters, sort order, pagination, open details or keyboard focus.
- Search and structured filters combine. Shared tables provide global search, per-column filters, multi-column sorting, column visibility and clear/reset actions. Accessors carry searchable and sortable values even when supplementary fields move into disclosures. Unknown numeric values must not sort as zero. Tables paginate loaded records with 10 rows by default and choices of 10, 25, 50 or 100 rows, page counts and previous/next controls. Loaded-row counts and bounded server sample coverage are distinct facts.
- TanStack Query owns fetched server data. Zustand owns only cross-component UI preferences and selection. Derive totals and filtered rows from current query data rather than copying server objects into UI state.
- Use the current stable TanStack Table API, with stable data/column definitions and only the state slices the application actually needs to control. Do not mix examples from an older major version.
- Use Radix primitives for dialogs and menus where their keyboard/focus behavior is needed; use native controls where they already provide the right interaction.
- Account and company identifiers belong in query keys for scoped requests. Retain last successful data during reconnects only within the same scope and label it stale; never display another account's history under a new heading.
- One shared dashboard query polls every five seconds while visible, refreshes on reconnect/focus and cancels obsolete requests. Scoped history polls every thirty seconds only when needed. The existing snapshot includes static and dynamic data together; memoize static map layers, rather than inventing a second fetch loop. The browser never causes direct game API calls.
- Observation timestamps come from persisted dataset metadata, not HTTP response time. Use child metadata for ship details. Expose last successful observation, last attempted error and upstream source age separately. Dynamic fleet observations are delayed after thirty seconds and stale after two minutes; incomplete or failed observations are immediately marked. Missing metadata means unknown freshness. Hourly leaderboard observations use their own cadence.
- API reachability, observation freshness and upstream runner connectivity are different facts. A successful browser poll or persisted execution mode does not prove a running bot or connected upstream stream; do not claim either without current evidence.
- A completed controller assessment needs matching generation and observation metadata. Missing, null or malformed holds are an unavailable assessment, not an observed empty list. Account-wide financial/identity gates remain separate from individual ship holds. Fresh controller metadata cannot upgrade stale ship facts or imply that disabled or paused automation may write.

### Compact ship manifest

Overview ship rows keep a fixed hierarchy: name and plain-language action, the two largest cargo stacks with a remaining-goods count, crew requirement and passengers, then one short reason. Ordinary stores and estimated arrival are desktop-only secondary facts. Critical voyage and impaired-recording warnings replace the ordinary reason and may wrap rather than being clipped. Arrival, berth and passenger waits, observe mode and ordinary idle route searches are neutral activity, not problems. The Overview and chart share this classification so genuine blockers still take priority over routine holds and active plans. Chart roster searches match every named cargo item, including goods omitted from the compact summary.

The reason prioritizes safety and impaired observations, then generation-coherent blocking holds using the chart's shared hold labels, followed by the durable active mission through `chartCurrentPlan` and an explicitly historical latest account-qualified ship decision. Normal arrival waits never hide blockers; additional blockers use a short count. Delayed but usable observations do not become warnings. Mission phases distinguish an empty return to the origin from outbound cargo, maintenance and post-delivery closing or reconciliation. Plan cargo uses catalog names. Only rejected decisions are called rejected, and trade profitability copy is restricted to trade evaluations. Overview, chart and inspector use one validated, named cargo projection. Fleet table cargo remains numerically sortable while showing the same goods and people summary.

Opening a ship shows its complete manifest, passengers by destination and stores immediately. One native **More details** disclosure contains raw holds, historical decision economics, voyage legs and timing, berth data, passenger fares and promises, hull facts, former names and source metadata. That disclosure survives polling. Overview uses a focus-managed side sheet instead of placing a distant inspector below the entire fleet; narrow fleet inspectors use the same sheet. Fate labels and historical last-afloat evidence remain distinct from current ship loads.

Passenger groups come only from recorded `passenger_aboard/<ship ID>` arrays, with their own metadata. Offers and waiting demand are not aboard. Both the live nested `ship.passenger_hold_tons` and decoded snapshot field are typed; an explicit zero hold supports zero people, but a missing map entry does not. An occupied hold without groups has an unknown count. Retained groups say **Last recorded passengers** instead of asserting a current count. Crew required is the catalog requirement, not a measured roster.

## Fleet chart

Use Leaflet 1.9 and React Leaflet 5, loaded in a client-only island. The local Natural Earth coastline and catalog port coordinates avoid an external tile service, API keys and a modern street-map aesthetic. Keep source attribution visible. Do not use React Activity to retain a Leaflet container, because the library documents that lifecycle as incompatible.

`/chart` is a real App Router page rendering `FleetChartPage`, separate from the persisted dashboard view selector. It reuses the shared recorded dashboard query and existing UI scope/selection store. Ship markers use readable ship icons, explicit status and descriptive accessible names. Port selection and ship inspection must also work from non-map controls. Keep normal keyboard, wheel, drag and touch navigation. Fit-to-fleet is an explicit action after initial framing; new observations must not continually move the operator's map.

The chart uses a restrained graticule with coordinate labels, subdued coastlines and typed port symbols. Labels prioritize selected and occupied ports; other names remain reachable through the chart room's non-map controls. Sailing vessels have a readable silhouette and heading; berthed, anchored, frozen, unavailable and wreck states remain distinct. Show all recorded voyage routes and their completed-leg wake, while strongly distinguishing the selected vessel's current leg and remaining route. Only the selected destination carries a permanent compact ETA; other ETAs are available on hull or destination hover and keyboard focus. Initial framing fits current fleet positions with panel padding, not the entire world or completed wake. A wake is a schematic representation of recorded completed voyage legs, not a measured historical track. Keep source attribution, status meaning and geometry uncertainty visible in both themes.

The estimate toggle controls interpolated sea positions and their animation, not whether recorded routes, completed-leg wake or authoritative port facts are visible. With estimates off, the wake ends at the recorded current-leg boundary rather than an interpolated point. Turning estimates off must not invent an observed position at sea or hide the recorded route network. Routes and their inspector continue to expose recorded leg order, endpoints and schedule independently of interpolation. Frozen estimates retain continuity across hidden-tab pauses only while the voyage identity still matches; wreck references do not expand active-fleet framing.

Reference harbour/city coordinates and an observed berthed or anchored port association place a vessel at its recorded port, not at a measured berth coordinate. The game does not supply authoritative sea coordinates: offshore route shapes, intermediate sea nodes, wake geometry and vessel interpolation are estimates or schematic connections. Clearly distinguish those from observed port facts; never describe a schematic route as an exact sailed track. A sunk ship's last-known port is only a location reference, not proof that the wreck lies there. Label that uncertainty in the wreck marker and inspector, and do not place an exact wreck at sea without a recorded position.

Use the shared `SpriteImage` for named vessels in ledgers and inspectors. The full species-name catalog maps to fixed-host Pokemon sprites; display them at a consistent small size, with descriptive alternative text and a ship-icon fallback for unknown names or failed images. Sprites support vessel recognition, not financial or voyage status. The working chart keeps its nautical ship silhouettes rather than substituting sprites for heading markers.

Animate only a defensible estimate along the recorded current voyage leg: `clamp((estimatedCurrentUTC - legDepartedUTC) / (legArrivesUTC - legDepartedUTC), 0, 1)`. These timestamps are already real UTC; do not multiply by game time scale. Anchor time to a usable recorded clock observation and monotonic elapsed time, retaining its true age. Require usable ship/clock metadata, valid timestamps and known endpoints. Freeze the last labelled estimate when paused or stale; an initial paused observation without a defensible position is unavailable. Never advance leg index or declare arrival locally; at ETA show awaiting observation. Label intermediate positions and unrecorded connecting geometry as estimates. Refresh only map markers on a one-second visible-tab ticker; stop hidden-tab work and respect reduced motion.

The chart room keeps zoom, route/label/estimate layers and fit-to-fleet in its toolbar. Its searchable fleet roster supports arrival, state and cargo sorting plus a state filter, with one compact state/destination/ETA line and a cargo fill bar per vessel. A native port selector makes every catalog port keyboard-reachable, including ports without chart coordinates, and shows only already recorded harbour conditions with their own freshness. The vessel inspector prefers the durable active mission's whitelisted plan projection from the recorded dashboard response, falling back to the latest account-qualified ship decision only when that projection is available. A failed projection reads "Plan unavailable", not an old fallback plan. Show kind, a recorded lane when present, cargo or charter, timestamp and recorded estimated value, never realized profit. Missing or unpriced zero-value economics remain unvalued. Normal voyage and arrival-observation waits are not blockers; only current blocking holds and warnings associated with the inspected ship appear as attention. Pending, failed or suppressed ship warning assessments are explicit unavailable or unassessed states, not an empty healthy assessment. Account growth warnings belong on the dashboard. The collapsible chart key explains state, hub/yard symbols and uncertainty without repeating disclaimers throughout the inspector.

The additional Atlantic port reference points are rounded WGS84 harbour/city coordinates from the corresponding [Wikipedia port and place articles](https://en.wikipedia.org/wiki/Port_of_Boston), with Norfolk from [Cockett's eastern seaboard port directory](https://www.cockettgroup.com/PortsMapAssets/region-usae.html). They cover the current catalog's North American ports, Guernsey, Roscoff, Flushing and St. Eustatius; neither these points nor route geometry are suitable for navigation.

## Facts and financial presentation

Show available cash, reserves, inventory/cargo, fees, realized settlements, modeled opportunities and risk stress as separate concepts. `expected_net_pence` is the modeled cash margin; stress values are diagnostic scenarios, not measured probability or realized loss. A historical record without a field stays unavailable.

Support all stored account/company records, not only the initial company. Include ships in transit, berthed, queued, building, damaged or sunk where the recorded API data supports those states. Yards, quotes, orders and construction are distinct. Charts use durable recorded metrics and clearly state their period and missing coverage.

Group configured accounts and observed companies by stable IDs. A vault's `account_id` is a treasury account, not a bot account. Sibling company identities from a player profile are not independently observed fleet data. Official leaderboard wealth belongs to the player's best company at its hourly refresh; never sum duplicate player ranks as company assets.

History endpoints return bounded samples, not complete period totals. Label sample subtotals and coverage honestly; do not infer complete daily profit, zero activity or total expenses by summing capped metrics. Separate owned contract obligations from public listings. Show convoy, marque and patrol observations, player/key identity without secrets, starving legs, and all four recorded metric series. Show the latest recorded decision with its timestamp, not an invented guaranteed next action.

Journal cash columns sort numerically, with unavailable amounts last in both directions. Transfers, collateral and refunds are cash movements, not earned profit. A settlement with unknown acquisition or operating costs must retain that uncertainty rather than displaying revenue as net.

Expansion details are structured charter and contributor tables, including completed tiers, not raw JSON. Planner route fields come from the serialized Plan and money fields from its nested `economics` object. Keep modeled cash, locked collateral and diagnostics-only stress scenarios visibly separate.

### Human text

Use sentence case, plain verbs and specific labels. Do not render em dashes, en dashes or arrow characters; write route relationships with words such as "to". Missing strings, numbers and dates read "Not recorded", never a dash, blank label or invented zero. Distinguish missing data from a recorded empty list and from an upstream error. Retained observations remain labelled when an update fails. No filler, decorative slogans or unsupported claims of live connectivity.

### Dashboard ingress

The Next proxy (`web/src/proxy.ts`) checks the incoming Host before any API rewrite changes it. Only `127.0.0.1:3400`, `localhost:3400` and `[::1]:3400` are allowed by default, case-insensitively. Remote deployments must use an authenticated reverse proxy and explicitly add its dashboard authorities, including any port, through comma-separated `DASHBOARD_ALLOWED_HOSTS`. This allowlist is not authentication. The gate applies to every path in both development and production; the Go API retains its own authority check.

## Research and implementation references

- [React: choosing state structure](https://react.dev/learn/choosing-the-state-structure): avoid redundant, duplicated and contradictory state.
- [Next.js: server and client components](https://nextjs.org/docs/app/getting-started/server-and-client-components): keep browser-only map code behind a client boundary and keep credentials on the server.
- [Next.js: Link](https://nextjs.org/docs/app/api-reference/components/link): use a real accessible link for `/chart` navigation rather than a new persisted dashboard View.
- [Next.js: ingress execution order](https://nextjs.org/docs/app/api-reference/file-conventions/proxy#execution-order): the proxy runs before configured rewrites.
- [TanStack Query: important defaults](https://tanstack.com/query/latest/docs/framework/react/guides/important-defaults): explicit freshness, independent polling intervals, structural sharing, focus/reconnect behavior and bounded retries.
- [TanStack Table: React state](https://tanstack.com/table/latest/docs/framework/react/guide/table-state): version 9 atomic state, controlled slices and selective subscriptions. Stable npm release checked during research: 9.3.1.
- [Tailwind: theme variables](https://tailwindcss.com/docs/theme): one semantic design-token system, not conflicting component-level colours.
- [Radix: accessibility](https://www.radix-ui.com/primitives/docs/overview/accessibility): accessible labels, keyboard navigation and focus management still need correct application composition.
- [Leaflet: accessible maps](https://leafletjs.com/examples/accessibility/): preserve keyboard behavior, label markers and provide non-map access to information.
- [React Leaflet: lifecycle and limitations](https://react-leaflet.js.org/docs/start-introduction/): mutable props, cleanup, client-only loading and the React Activity incompatibility.

## Verification

Inspect actual production-build screenshots, not just a component preview. Exercise scope changes, compound filtering, multi-sort, map/table selection, empty/error/stale states, reconnects and updates while an inspector is open. Verify that recorded observation timestamps advance and positions change when a voyage is active without refreshing the page.

Use the viewport and device-pixel-ratio matrix in AGENTS.md, including narrow touch layouts and both themes. Check keyboard navigation, visible focus, contrast, reduced motion, component-local overflow and absence of page-level sideways scroll. The healthy running application must have no browser console warnings, errors, hydration failures or failed asset requests. Report server-down reconnect errors separately; do not suppress application errors to satisfy this gate.
