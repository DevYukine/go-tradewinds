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

Overview puts company state, observed ship activity, current holds, next-check targets and clearly separated money facts before the working chart. These facts require no ship selection or diagnostic drill-down. Account/company scope and fleet selection persist across views. A ship inspector opens beside the working content when space permits and as an accessible sheet on narrow screens.

Seven icon-led primary destinations expose fourteen views through adjacent contextual tabs:

- Overview: company operations and working chart.
- Fleet: Vessels and Voyages.
- Journal: chronological company activity.
- Money: Performance, Treasury and Warehouses.
- Trade: Contracts, Exchange and Shipyards.
- World: World and Standings.
- System: Decisions and Diagnostics.

Keep the primary navigation short without hiding routine status behind more clicks. Each view uses a few purposeful sections rather than repeated statistic cards. Money shows owned underwriting collateral, unsettled premium, settled net and risk allowance directly above financial history; public policy listings stay in Exchange. Decisions retains technical planning evidence, not the only explanation of why a ship is waiting.

A card-grid alternative was rejected: it fragments the fleet and prioritizes decoration over the operator's task. A full-screen map with every control floating on top was also rejected: it hides financial context and becomes crowded on touch screens. The chosen chart-plus-ledger layout keeps spatial and financial information adjacent without duplicating every statistic in a card.

At narrow widths, navigation collapses, the chart retains its controls, and the inspector becomes a sheet. Journal keeps time, vessel, activity and cash visible together with wrapped text; wider analytical tables may scroll inside labelled regions. The page itself must not scroll sideways. Controls need visible focus and sufficient touch targets. Reduced motion disables decorative and map-flight animation; data remains current.

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

## Map

Use Leaflet 1.9 and React Leaflet 5, loaded in a client-only island. The local Natural Earth coastline and catalog port coordinates avoid an external tile service, API keys and a modern street-map aesthetic. Keep source attribution visible. Do not use React Activity to retain a Leaflet container, because the library documents that lifecycle as incompatible.

Ship markers use readable ship icons, explicit status and descriptive accessible names. Port selection and ship inspection must also work from non-map controls. Keep normal keyboard, wheel, drag and touch navigation. Fit-to-fleet is an explicit action after initial framing; new observations must not continually move the operator's map.

The chart uses a restrained graticule with coordinate labels, subdued coastlines and typed port symbols: anchors for hubs, tools for shipyards and phase rings for other ports. Permanent labels prioritize selected and occupied ports, then nearby hubs at the initial useful zoom; deterministic collision checks prevent overlapping names. Other names remain available on hover/focus and in the port selector. Sailing vessels have a readable silhouette and heading; berthed, anchored, frozen and unavailable states remain distinct in the legend. A selected route emphasizes the current leg and direction. Its connecting lines are schematic geometry, not an observed track. Fit fleet, zoom and the native port selector are the exposed controls. Keep attribution and the meaningful status legend visible, with light and dark themes supplying the same hierarchy.

Use the shared `SpriteImage` for named vessels in ledgers and inspectors. The full species-name catalog maps to fixed-host Pokemon sprites; display them at a consistent small size, with descriptive alternative text and a ship-icon fallback for unknown names or failed images. Sprites support vessel recognition, not financial or voyage status. The working chart keeps its nautical ship silhouettes rather than substituting sprites for heading markers.

Animate only a defensible estimate along the recorded current voyage leg: `clamp((estimatedCurrentUTC - legDepartedUTC) / (legArrivesUTC - legDepartedUTC), 0, 1)`. These timestamps are already real UTC; do not multiply by game time scale. Anchor time to a usable recorded clock observation and monotonic elapsed time, retaining its true age. Require usable ship/clock metadata, valid timestamps and known endpoints. Freeze the last labelled estimate when paused or stale; an initial paused observation without a defensible position is unavailable. Never advance leg index or declare arrival locally; at ETA show awaiting observation. Label intermediate positions and unrecorded connecting geometry as estimates. Refresh only map markers on a one-second visible-tab ticker; stop hidden-tab work and respect reduced motion.

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
