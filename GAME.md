# Tradewinds — Game Mechanics & API Reference

> Base URL: `https://tradewinds.fly.dev`  
> OpenAPI spec: `https://tradewinds.fly.dev/api/openapi`  
> All endpoints prefixed with `/api/v1`
>
> **Local OpenAPI schema**: [`openapi/current.json`](openapi/current.json)  
> **Schema history**: [`openapi/history/`](openapi/history/) — snapshots stored by date to detect API changes between versions.

---

## Table of Contents

### Game World
- [Ports (15 total)](#ports-15-total)
- [Goods (14 total)](#goods-14-total)
- [Ship Types (3 total)](#ship-types-3-total)
- [Routes](#routes)
- [NPC Traders](#npc-traders)

### API Endpoints (30 total)

**Authentication & Accounts (4)**
| # | Method | Endpoint | Description |
|---|--------|----------|-------------|
| 1 | POST | [`/auth/register`](#post-authregister--register-new-player) | Register new player |
| 2 | POST | [`/auth/login`](#post-authlogin--login) | Login (get JWT) |
| 3 | POST | [`/auth/revoke`](#post-authrevoke--logout) | Logout (revoke JWT) |
| 4 | GET | [`/me`](#get-me--current-player-profile) | Current player profile |

**Companies (4)**
| # | Method | Endpoint | Description |
|---|--------|----------|-------------|
| 5 | POST | [`/companies`](#post-companies--create-company) | Create company |
| 6 | GET | [`/me/companies`](#get-mecompanies--list-my-companies) | List my companies |
| 7 | GET | [`/company`](#get-company--get-company-details) | Get company details |
| 8 | GET | [`/company/economy`](#get-companyeconomy--financial-summary) | Financial summary |
| 9 | GET | [`/company/ledger`](#get-companyledger--transaction-history) | Transaction history |

**World — Public (8)**
| # | Method | Endpoint | Description |
|---|--------|----------|-------------|
| 10 | GET | [`/world/ports`](#get-worldports--list-all-ports) | List all ports |
| 11 | GET | [`/world/ports/{id}`](#get-worldportsid--single-port-detail) | Single port detail (with traders & routes) |
| 12 | GET | [`/world/ports/{port_id}/shipyard`](#get-worldportsport_idshipyard--shipyard-at-port) | Shipyard at port |
| 13 | GET | [`/world/goods`](#get-worldgoods--list-all-goods) | List all goods |
| 14 | GET | [`/world/goods/{id}`](#get-worldgoodsid--single-good-detail) | Single good detail |
| 15 | GET | [`/world/routes`](#get-worldroutes--list-all-routes) | List all routes |
| 16 | GET | [`/world/routes/{id}`](#get-worldroutesid--single-route-detail) | Single route detail |
| 17 | GET | [`/world/ship-types`](#get-worldship-types--list-ship-types) | List ship types |
| 18 | GET | [`/world/ship-types/{id}`](#get-worldship-typesid--single-ship-type-detail) | Single ship type detail |

**Trade — NPC (6)**
| # | Method | Endpoint | Description |
|---|--------|----------|-------------|
| 19 | GET | [`/trade/traders`](#get-tradetraders--list-npc-traders) | List NPC traders |
| 20 | GET | [`/trade/trader-positions`](#get-tradetrader-positions--list-trader-stock-positions) | List trader stock positions |
| 21 | POST | [`/trade/quote`](#post-tradequote--get-a-trade-quote) | Get a trade quote |
| 22 | POST | [`/trade/quotes/execute`](#post-tradequotesexecute--execute-a-signed-quote) | Execute a signed quote |
| 23 | POST | [`/trade/execute`](#post-tradeexecute--direct-trade-no-quote) | Direct trade (no quote) |
| 24 | POST | [`/trade/quotes/batch`](#post-tradequotesbatch--batch-quotes) | Batch quotes |
| 25 | POST | [`/trade/quotes/execute/batch`](#post-tradequotesexecutebatch--batch-execute-quotes) | Batch execute quotes |

**Market — Player-to-Player (4)**
| # | Method | Endpoint | Description |
|---|--------|----------|-------------|
| 26 | GET | [`/market/orders`](#get-marketorders--list-open-orders) | List open orders |
| 27 | POST | [`/market/orders`](#post-marketorders--post-an-order) | Post an order |
| 28 | POST | [`/market/orders/{order_id}/fill`](#post-marketordersorder_idfill--fill-an-order) | Fill an order |
| 29 | DELETE | [`/market/orders/{order_id}`](#delete-marketordersorder_id--cancel-an-order) | Cancel an order |
| 30 | GET | [`/market/blended-price`](#get-marketblended-price--calculate-fill-cost) | Calculate blended price |

**Fleet — Ships (6)**
| # | Method | Endpoint | Description |
|---|--------|----------|-------------|
| 31 | GET | [`/ships`](#get-ships--list-company-ships) | List company ships |
| 32 | GET | [`/ships/{ship_id}`](#get-shipsship_id--single-ship-detail) | Single ship detail |
| 33 | PATCH | [`/ships/{ship_id}`](#patch-shipsship_id--rename-ship) | Rename ship |
| 34 | GET | [`/ships/{ship_id}/inventory`](#get-shipsship_idinventory--ship-cargo) | Ship cargo |
| 35 | POST | [`/ships/{ship_id}/transit`](#post-shipsship_idtransit--send-ship-sailing) | Send ship sailing |
| 36 | GET | [`/ships/{ship_id}/transit-logs`](#get-shipsship_idtransit-logs--travel-history) | Travel history |
| 37 | POST | [`/ships/{ship_id}/transfer-to-warehouse`](#post-shipsship_idtransfer-to-warehouse--offload-cargo) | Offload cargo to warehouse |

**Logistics — Warehouses (6)**
| # | Method | Endpoint | Description |
|---|--------|----------|-------------|
| 38 | POST | [`/warehouses`](#post-warehouses--buy-a-warehouse) | Buy a warehouse |
| 39 | GET | [`/warehouses`](#get-warehouses--list-warehouses) | List warehouses |
| 40 | GET | [`/warehouses/{warehouse_id}`](#get-warehouseswarehouse_id--warehouse-detail) | Warehouse detail |
| 41 | GET | [`/warehouses/{warehouse_id}/inventory`](#get-warehouseswarehouse_idinventory--warehouse-stock) | Warehouse stock |
| 42 | POST | [`/warehouses/{warehouse_id}/grow`](#post-warehouseswarehouse_idgrow--upgrade-warehouse) | Upgrade warehouse |
| 43 | POST | [`/warehouses/{warehouse_id}/shrink`](#post-warehouseswarehouse_idshrink--downgrade-warehouse) | Downgrade warehouse |
| 44 | POST | [`/warehouses/{warehouse_id}/transfer-to-ship`](#post-warehouseswarehouse_idtransfer-to-ship--load-ship-from-warehouse) | Load ship from warehouse |

**Shipyards (3)**
| # | Method | Endpoint | Description |
|---|--------|----------|-------------|
| 45 | GET | [`/world/ports/{port_id}/shipyard`](#get-worldportsport_idshipyard--find-shipyard) | Find shipyard at port |
| 46 | GET | [`/shipyards/{shipyard_id}/inventory`](#get-shipyardsshipyard_idinventory--ships-for-sale) | Ships for sale |
| 47 | POST | [`/shipyards/{shipyard_id}/purchase`](#post-shipyardsshipyard_idpurchase--buy-a-ship) | Buy a ship |

**Events (2)**
| # | Method | Endpoint | Description |
|---|--------|----------|-------------|
| 48 | GET | [`/world/events`](#get-worldevents--world-event-stream) | World event stream (SSE) |
| 49 | GET | [`/company/events`](#get-companyevents--company-event-stream) | Company event stream (SSE) |

**Health (1)**
| # | Method | Endpoint | Description |
|---|--------|----------|-------------|
| 50 | GET | [`/health`](#get-health--server-health) | Server health check |

### Validated Observations
- [API Behavior](#api-behavior)
- [Pricing Mechanics](#pricing-mechanics-validated-with-real-quotes)
- [Tax System](#tax-system-validated-from-ledger)
- [Economy & Upkeep](#economy--upkeep-validated-from-ledger)
- [Costs Reference](#costs-reference-validated)
- [Sample Prices](#sample-prices-snapshot--prices-are-dynamic)

---

## World Overview (Validated Live Data)

### Ports (15 total)
Every port connects to every other port. Hub ports have 5% tax; non-hub ports have 2% tax.

> ⚠️ **Dynamic data** — Ports can change. Always fetch from `GET /world/ports`.

| Port | Code | Hub | Tax | Country |
|------|------|-----|-----|---------|
| Amsterdam | AMS | ✅ | 500bps (5%) | Netherlands |
| Edinburgh | EDI | ✅ | 500bps (5%) | England/Scotland |
| Hamburg | HAM | ✅ | 500bps (5%) | Germany |
| London | LON | ✅ | 500bps (5%) | England |
| Antwerp | ANR | ❌ | 200bps (2%) | Belgium |
| Bremen | BRE | ❌ | 200bps (2%) | Germany |
| Bristol | BRS | ❌ | 200bps (2%) | England |
| Calais | CQF | ❌ | 200bps (2%) | France |
| Dublin | DUB | ❌ | 200bps (2%) | Ireland |
| Dunkirk | DKK | ❌ | 200bps (2%) | France |
| Glasgow | GLA | ❌ | 200bps (2%) | England/Scotland |
| Hull | HUL | ❌ | 200bps (2%) | England |
| Plymouth | PLH | ❌ | 200bps (2%) | England |
| Portsmouth | PME | ❌ | 200bps (2%) | England |
| Rotterdam | RTM | ❌ | 200bps (2%) | Netherlands |

### Goods (14 total)

> ⚠️ **Dynamic data** — Goods can change. Always fetch from `GET /world/goods`.

| Good | Category |
|------|----------|
| Grain | Staple |
| Salt | Staple |
| Coal | Staple |
| Fish | Staple |
| Timber | Material |
| Wool | Material |
| Hemp | Material |
| Tar/Pitch | Material |
| Iron | Industrial |
| Copper | Industrial |
| Cloth | Industrial |
| Wine | Luxury |
| Spices | Luxury |
| Silk | Luxury |

### Ship Types (3 total)

> ⚠️ **Dynamic data** — Ship types can change. Always fetch from `GET /world/ship-types`.

| Ship | Capacity | Speed | Upkeep | Price | Efficiency (cap×spd/upkeep) |
|------|----------|-------|--------|-------|----------------------------|
| Cog | 50 | 4 | 500 | 3,000 | 0.40 |
| Caravel | 100 | 6 | 1,000 | 6,000 | 0.60 |
| Galleon | 200 | 5 | 2,000 | 12,000 | 0.50 |

Caravel is the most efficient (highest cap×speed/upkeep ratio).

### Routes
- Every port has outgoing routes to ALL other 14 ports
- Routes are directional (A→B and B→A are separate entries, but same distance)
- Distance determines travel time: `travel_time = distance / ship_speed`
- Total routes: 210 (15×14)
- The `/routes` list endpoint paginates (default 50 per page); single port detail embeds all outgoing routes

### NPC Traders
- 1 trader per port (named "{Port} Merchant Guild")
- ~200 trader positions total (not every port trades every good)
- Each position has `stock_bounds` and `price_bounds` as textual labels:
  - **Stock levels**: `Very Abundant`, `Abundant`, `Healthy` (and likely `Low`, `Scarce`)
  - **Price levels**: `Average`, `Expensive`, `Very Expensive` (and likely `Cheap`, `Very Cheap`)
- Higher stock → lower prices; lower stock → higher prices

---

## API Endpoints — Complete Reference

### Authentication & Accounts

#### `POST /auth/register` — Register new player
- **Auth**: None
- **Models**: Request: `RegisterRequest` → Response: `RegisterResponse`
- **Body**: `{ name, email, password, discord_id? }`
- **Response**: Player object
- **Use**: One-time account creation

#### `POST /auth/login` — Login
- **Auth**: None
- **Models**: Request: `LoginRequest` → Response: `LoginResponse`
- **Body**: `{ email, password }`
- **Response**: `{ data: { token } }` — JWT token
- **Use**: Get auth token for all subsequent calls

#### `POST /auth/revoke` — Logout
- **Auth**: Bearer token
- **Response**: 204 No Content
- **Use**: Invalidate current token

#### `GET /me` — Current player profile
- **Auth**: Bearer token
- **Models**: → Response: `RegisterResponse`
- **Response**: Player object (id, name, email, enabled, inserted_at)
- **Use**: Verify authentication, get player ID

---

### Companies

#### `POST /companies` — Create company
- **Auth**: Bearer token
- **Models**: Request: `CreateCompanyRequest` → Response: `CompanyResponse`
- **Body**: `{ name, ticker (1-5 chars), home_port_id }`
- **Response**: Company object
- **Use**: Create trading company. Player becomes first director.

#### `GET /me/companies` — List my companies
- **Auth**: Bearer token
- **Models**: → Response: `CompaniesResponse`
- **Response**: `{ data: [Company] }`
- **Use**: Get all companies the player directs

#### `GET /company` — Get company details
- **Auth**: Bearer token + `tradewinds-company-id` header
- **Models**: → Response: `CompanyResponse`
- **Response**: Company `{ id, name, ticker, treasury, reputation, status, home_port_id }`
- **Use**: Check treasury balance, reputation, bankruptcy status

#### `GET /company/economy` — Financial summary
- **Auth**: Bearer token + `tradewinds-company-id` header
- **Models**: → Response: `CompanyEconomyResponse`
- **Response**: `{ treasury, reputation, ship_upkeep, warehouse_upkeep, total_upkeep }`
- **Use**: Monitor costs. Critical for knowing how much upkeep is draining per cycle.

#### `GET /company/ledger` — Transaction history
- **Auth**: Bearer token + `tradewinds-company-id` header
- **Models**: → Response: `LedgerResponse`
- **Pagination**: `after`, `before`, `limit`
- **Response**: List of LedgerEntry `{ id, amount, reason, reference_type, reference_id, occurred_at, meta }`
- **Reasons**: `initial_deposit`, `transfer`, `ship_purchase`, `tax`, `market_trade`, `market_listing_fee`, `market_penalty_fine`, `warehouse_upgrade`, `warehouse_upkeep`, `ship_upkeep`, `npc_trade`, `bailout`
- **Use**: Audit all income/expenses, calculate profit/loss, track trade history

---

### World (Public — No Auth)

#### `GET /world/ports` — List all ports
- **Models**: → Response: `PortsResponse`
- **Pagination**: `after`, `before`, `limit`; filter: `country_id`, `is_hub`
- **Response**: List of Port objects (traders and outgoing_routes are EMPTY in list view)
- **Use**: Get port IDs, tax rates, hub status. Use single port endpoint for embedded data.

#### `GET /world/ports/{id}` — Single port detail
- **Models**: → Response: `PortResponse`
- **Response**: Port with embedded `traders[]` and `outgoing_routes[]`
- **Use**: Get trader list + all outgoing routes with distances for a port

#### `GET /world/ports/{port_id}/shipyard` — Shipyard at port
- **Models**: → Response: `ShipyardResponse`
- **Response**: Shipyard `{ id, port_id }`
- **Use**: Get shipyard ID to query its inventory. Returns 404 if port has no shipyard.

#### `GET /world/goods` — List all goods
- **Models**: → Response: `GoodsResponse`
- **Filter**: `category` query param
- **Response**: List of Good `{ id, name, description, category }`
- **Use**: Get good IDs for trading

#### `GET /world/goods/{id}` — Single good detail
- **Models**: → Response: `GoodResponse`
- **Response**: Good object

#### `GET /world/routes` — List all routes
- **Models**: → Response: `RoutesResponse`
- **Pagination**: `after`, `before`, `limit`; filter: `from_id`, `to_id`
- **Response**: List of Route `{ id, from_id, to_id, distance }`
- **Use**: Build route graph. **Note**: paginated, default 50. Total ~210 routes.

#### `GET /world/routes/{id}` — Single route detail
- **Models**: → Response: `RouteResponse`
- **Response**: Route object

#### `GET /world/ship-types` — List ship types
- **Models**: → Response: `ShipTypesResponse`
- **Response**: List of ShipType `{ id, name, capacity, speed, upkeep, base_price, passengers, description }`

#### `GET /world/ship-types/{id}` — Single ship type detail
- **Models**: → Response: `ShipTypeResponse`
- **Response**: ShipType object

---

### Trade (NPC Trading)

#### `GET /trade/traders` — List NPC traders
- **Models**: → Response: `TradersResponse`
- **Pagination**: `after`, `before`, `limit`
- **Response**: List of Trader `{ id, name }`
- **Use**: Get trader IDs (one per port)

#### `GET /trade/trader-positions` — List trader stock positions
- **Models**: → Response: `TraderPositionsResponse`
- **Filter**: `trader_id`; **Pagination**: `after`, `before`, `limit` (max 100)
- **Response**: List of TraderPosition `{ id, trader_id, port_id, good_id, stock_bounds, price_bounds }`
- **Use**: **KEY ENDPOINT** — shows what each port buys/sells and at what price level. ~200 positions across 4 pages.
- **stock_bounds**: textual (`Very Abundant`, `Abundant`, `Healthy`, etc.)
- **price_bounds**: textual (`Average`, `Expensive`, `Very Expensive`, etc.)

#### `POST /trade/quote` — Get a trade quote
- **Auth**: Bearer + company header
- **Models**: Request: `QuoteRequest` → Response: `QuoteResponse`
- **Body**: `{ port_id, good_id, action: "buy"|"sell", quantity }`
- **Response**: `{ data: { token, quote: { unit_price, total_price, quantity, ... } } }`
- **Use**: Get exact price + signed token. Token expires (timestamp included). Must execute within window.

#### `POST /trade/quotes/execute` — Execute a signed quote
- **Auth**: Bearer + company header
- **Models**: Request: `ExecuteQuoteRequest` → Response: `TradeExecutionResponse`
- **Body**: `{ token, destinations: [{ type: "ship"|"warehouse", id, quantity }] }`
- **Response**: TradeExecution `{ action, quantity, unit_price, total_price }`
- **Use**: Complete the trade. Destinations specify where bought goods go, or where sold goods come from.

#### `POST /trade/execute` — Direct trade (no quote)
- **Auth**: Bearer + company header
- **Models**: Request: `ExecuteTradeRequest` → Response: `TradeExecutionResponse`
- **Body**: `{ port_id, good_id, action: "buy"|"sell", destinations: [{ type, id, quantity }] }`
- **Response**: TradeExecution
- **Use**: Skip the quote step — price may differ from quote. Convenient but less predictable.

#### `POST /trade/quotes/batch` — Batch quotes
- **Auth**: Bearer + company header
- **Models**: Request: `BatchQuoteRequest` → Response: `BatchQuoteResponse`
- **Body**: `{ requests: [{ port_id, good_id, action, quantity }] }`
- **Response**: List of quote responses (each with status, token, quote or error message)
- **Use**: Get multiple quotes at once — efficient for buying/selling multiple goods at a port

#### `POST /trade/quotes/execute/batch` — Batch execute quotes
- **Auth**: Bearer + company header
- **Models**: Request: `BatchExecuteQuoteRequest` → Response: `BatchExecuteQuoteResponse`
- **Body**: `{ requests: [{ token, destinations }] }`
- **Response**: List of execution results
- **Use**: Execute multiple trades at once

---

### Market (Player-to-Player)

#### `GET /market/orders` — List open orders
- **Auth**: None
- **Models**: → Response: `OrdersResponse`
- **Filter**: `port_ids[]`, `good_ids[]`, `side`; **Pagination**: `after`, `before`, `limit`
- **Response**: List of Order `{ id, company_id, port_id, good_id, side, price, total, remaining, status, posted_reputation, expires_at }`
- **Use**: Find player-posted buy/sell orders. Can filter by port and good.

#### `POST /market/orders` — Post an order
- **Auth**: Bearer + company header
- **Models**: Request: `CreateOrderRequest` → Response: `OrderResponse`
- **Body**: `{ port_id, good_id, side: "buy"|"sell", price, total }`
- **Response**: Order object
- **Use**: Post a buy/sell order. Costs a listing fee. Requires reputation.

#### `POST /market/orders/{order_id}/fill` — Fill an order
- **Auth**: Bearer + company header
- **Models**: Request: `FillOrderRequest` → Response: `OrderResponse`
- **Body**: `{ quantity }`
- **Use**: Fill someone else's order. Partial fills OK.

#### `DELETE /market/orders/{order_id}` — Cancel an order
- **Auth**: Bearer + company header
- **Use**: Cancel your own open order. May incur penalty.

#### `GET /market/blended-price` — Calculate fill cost
- **Auth**: None
- **Models**: → Response: `BlendedPriceResponse`
- **Params**: `port_id`, `good_id`, `side`, `quantity`
- **Response**: `{ data: { blended_price } }`
- **Use**: Preview what it would cost to fill a quantity across multiple market orders

---

### Fleet (Ships)

#### `GET /ships` — List company ships
- **Auth**: Bearer + company header
- **Models**: → Response: `ShipsResponse`
- **Pagination**: `after`, `before`, `limit`
- **Response**: List of Ship `{ id, name, status, company_id, ship_type_id, port_id?, route_id?, arriving_at? }`
- **Status**: `docked` (has port_id) or `traveling` (has route_id + arriving_at)

#### `GET /ships/{ship_id}` — Single ship detail
- **Auth**: Bearer + company header
- **Models**: → Response: `ShipResponse`
- **Response**: Ship object

#### `PATCH /ships/{ship_id}` — Rename ship
- **Auth**: Bearer + company header
- **Models**: Request: `RenameShipRequest` → Response: `ShipResponse`
- **Body**: `{ name }`

#### `GET /ships/{ship_id}/inventory` — Ship cargo
- **Auth**: Bearer + company header
- **Models**: → Response: `CargoResponse`
- **Response**: `{ data: [{ good_id, quantity }] }`
- **Use**: Check what's loaded on a ship

#### `POST /ships/{ship_id}/transit` — Send ship sailing
- **Auth**: Bearer + company header
- **Models**: Request: `TransitRequest` → Response: `ShipResponse`
- **Body**: `{ route_id }`
- **Use**: Ship must be `docked`. Route must start from ship's current port. Ship becomes `traveling`.

#### `GET /ships/{ship_id}/transit-logs` — Travel history
- **Auth**: Bearer + company header
- **Models**: → Response: `TransitLogsResponse`
- **Pagination**: `after`, `before`, `limit`
- **Response**: List of TransitLog `{ id, ship_id, route_id, departed_at, arrived_at? }`

#### `POST /ships/{ship_id}/transfer-to-warehouse` — Offload cargo
- **Auth**: Bearer + company header
- **Models**: Request: `TransferToWarehouseRequest`
- **Body**: `{ warehouse_id, good_id, quantity }`
- **Use**: Ship must be docked. Warehouse must be at same port.

---

### Logistics (Warehouses)

#### `POST /warehouses` — Buy a warehouse
- **Auth**: Bearer + company header
- **Models**: Request: `CreateWarehouseRequest` → Response: `WarehouseResponse`
- **Body**: `{ port_id }`
- **Response**: Warehouse `{ id, level, capacity, port_id, company_id }`
- **Use**: Creates level 1 warehouse at a port

#### `GET /warehouses` — List warehouses
- **Auth**: Bearer + company header
- **Models**: → Response: `WarehousesResponse`
- **Pagination**: `after`, `before`, `limit`

#### `GET /warehouses/{warehouse_id}` — Warehouse detail
- **Auth**: Bearer + company header
- **Models**: → Response: `WarehouseResponse`

#### `GET /warehouses/{warehouse_id}/inventory` — Warehouse stock
- **Auth**: Bearer + company header
- **Models**: → Response: `WarehouseInventoryResponse`
- **Pagination**: `after`, `before`, `limit`
- **Response**: List of `{ id, warehouse_id, good_id, quantity }`

#### `POST /warehouses/{warehouse_id}/grow` — Upgrade warehouse
- **Auth**: Bearer + company header
- **Models**: → Response: `WarehouseResponse`
- **Use**: Increase level → more capacity. Costs money.

#### `POST /warehouses/{warehouse_id}/shrink` — Downgrade warehouse
- **Auth**: Bearer + company header
- **Models**: → Response: `WarehouseResponse`
- **Use**: Decrease level → less capacity, lower upkeep.

#### `POST /warehouses/{warehouse_id}/transfer-to-ship` — Load ship from warehouse
- **Auth**: Bearer + company header
- **Models**: Request: `TransferToShipRequest`
- **Body**: `{ ship_id, good_id, quantity }`
- **Use**: Ship must be docked at warehouse's port.

---

### Shipyards

#### `GET /world/ports/{port_id}/shipyard` — Find shipyard
- **Auth**: None
- **Models**: → Response: `ShipyardResponse`
- **Response**: Shipyard `{ id, port_id }` or 404
- **Use**: Not all ports have shipyards. Get the shipyard ID first.

#### `GET /shipyards/{shipyard_id}/inventory` — Ships for sale
- **Auth**: None
- **Models**: → Response: `InventoryResponse`
- **Response**: List of ShipyardInventory `{ id, shipyard_id, ship_type_id, ship_id, cost }`
- **Use**: See available ships and prices. Ships have unique IDs — specific inventory items.

#### `POST /shipyards/{shipyard_id}/purchase` — Buy a ship
- **Auth**: Bearer + company header
- **Models**: Request: `PurchaseShipRequest` → Response: `ShipResponse`
- **Body**: `{ ship_type_id }`
- **Response**: Ship object (now owned by your company, docked at the port)
- **Use**: Deducts cost from treasury. Ship appears docked at the shipyard's port.

---

### Events (Server-Sent Events)

All events are JSON objects with `type` and `data` fields. The `type` field identifies the event kind.

#### `GET /world/events` — World event stream
- **Auth**: None
- **Format**: SSE (`text/event-stream`)
- **Use**: Public events visible to all players — ship movements, company formations, economy shocks.

**Known event types:**

##### `ship_docked_world`
Fired when any ship docks at a port.
```json
{
  "type": "ship_docked_world",
  "data": {
    "name": "The Cheese Wheel",
    "ship_id": "df42e6d5-...",
    "port_id": "5e56993c-...",
    "company_id": "0d5c69ed-...",
    "company_name": "TopStats3"
  }
}
```

##### `ship_set_sail`
Fired when any ship departs on a route.
```json
{
  "type": "ship_set_sail",
  "data": {
    "name": "The Cheese Wheel",
    "ship_id": "df42e6d5-...",
    "route_id": "028c2f2b-...",
    "company_id": "0d5c69ed-...",
    "company_name": "TopStats3"
  }
}
```

##### `ship_bought`
Fired when any company purchases a ship.
```json
{
  "type": "ship_bought",
  "data": {
    "name": "Caravel - 20c10de9",
    "ship_id": "df42e6d5-...",
    "ship_type_id": "3d94ac36-...",
    "company_id": "0d5c69ed-...",
    "company_name": "TopStats3"
  }
}
```

##### `company_formed`
Fired when a new company is created.
```json
{
  "type": "company_formed",
  "data": {
    "id": "a07ee596-...",
    "name": "narigama's Trade Co",
    "ticker": "NARIG"
  }
}
```

> 💡 **Note**: There may be additional world event types not yet observed (e.g., economy shocks, price changes). Monitor the stream to discover new types.

#### `GET /company/events` — Company event stream
- **Auth**: Bearer + company header
- **Format**: SSE (`text/event-stream`)
- **Use**: Private events for your company only — ship arrivals, completed trades, ledger updates. **Critical for automation** — triggers when ships arrive.

> 💡 **Note**: Company events likely mirror some world events (e.g., `ship_docked`) but only for your own ships, and may include additional private events like trade completions and ledger entries. Needs further observation.

---

### Health

#### `GET /health` — Server health
- **Auth**: None
- **Models**: → Response: `HealthResponse`
- **Response**: `{ status: "healthy"|"degraded"|"unhealthy", database, oban_lag_seconds, server_time }`

---

## API Conventions

| Convention | Detail |
|-----------|--------|
| **Auth** | `Authorization: Bearer <jwt_token>` |
| **Company context** | `tradewinds-company-id: <uuid>` header |
| **Pagination** | Cursor-based: `after`, `before`, `limit` (max 100) |
| **Errors** | `{ errors: { detail: "..." } }` or changeset `{ errors: { field: ["msg"] } }` |
| **IDs** | All UUIDs (v4) |
| **Rate Limit** | 300 requests per 60 seconds, per IP. Exceeding this will result in `429 Too Many Requests`. |

---

## Validated Observations

### API Behavior
1. **Port list vs detail**: `GET /ports` returns ports WITHOUT traders/routes. `GET /ports/{id}` includes embedded `traders[]` and `outgoing_routes[]`.
2. **Full connectivity**: Every port connects to every other port (14 outgoing routes per port, 210 total).
3. **One trader per port**: Named "{City} Merchant Guild". Each has ~13-14 goods positions.
4. **Shipyard inventory is finite**: Specific ship instances are for sale. Once bought, they're gone (presumably restocked periodically).
5. **Batch operations available**: Can quote + execute multiple trades in one API call — essential for efficiency.
6. **SSE events exist**: Company events stream ship arrivals in real-time — no need to poll.
7. **Market orders visible without auth**: Anyone can browse the player market.

### Pricing Mechanics (Validated with Real Quotes)
8. **Trader positions show textual labels** (`stock_bounds`, `price_bounds`) like "Abundant", "Very Expensive" — these are **indicators only**. Must call the quote endpoint to get exact integer prices.
9. **Buy/sell spread**: ~12-17% spread at the same port. E.g., Timber at Plymouth: buy=84, sell=71 (15% spread). You LOSE money buying and selling at the same port.
10. **Prices vary significantly between ports**: Grain at London buy=52 vs Plymouth buy=78. This creates arbitrage opportunities.
11. **Prices are integers** representing coins per unit.

### Tax System (Validated from Ledger)
12. **Taxes apply to everything**: NPC trades, ship purchases, warehouse purchases.
13. **Tax rate is the port's `tax_rate_bps`**: 2% at regular ports, 5% at hub ports.
14. **Tax examples from ledger**: 
    - NPC trade of 2,700 at regular port → tax of 54 (2%)
    - Ship purchase of 6,000 at hub → tax of 300 (5%)
    - Warehouse purchase of 100 at hub → tax of 5 (5%)

### Economy & Upkeep (Validated)
15. **Upkeep is charged every ~5 hours** (confirmed by dev).
16. **Ship upkeep**: Per-cycle cost matches the ship type's `upkeep` field (Caravel=1,000, Galleon=2,000, Cog=500).
17. **Warehouse upkeep**: Level 1 warehouse costs ~1,000 per cycle.
18. **Bankruptcy**: Running out of money **locks the company** (status=`bankrupt`). Dev can manually unlock and grant a ~3 month runway.
19. **Company economy endpoint** shows upkeep as "per cycle" amounts, not total charged.

### Costs Reference (Validated)
| Item | Base Cost | Notes |
|------|-----------|-------|
| Cog | 3,000 | + port tax |
| Caravel | 6,000 | + port tax |
| Galleon | 12,000 | + port tax |
| Warehouse (Level 1) | 100 | + port tax |
| Cog upkeep/cycle | 500 | |
| Caravel upkeep/cycle | 1,000 | |
| Galleon upkeep/cycle | 2,000 | |
| Warehouse upkeep/cycle | ~1,000 | Level 1 observed |

### Sample Prices (Snapshot — Prices Are Dynamic)

**Plymouth (2% tax)**
| Good | Buy Price | Sell Price | Spread |
|------|-----------|------------|--------|
| Coal | 44 | 37 | 16% |
| Fish | 48 | 43 | 10% |
| Salt | 64 | 57 | 11% |
| Grain | 78 | 69 | 12% |
| Hemp | 82 | 73 | 11% |
| Timber | 84 | 71 | 15% |
| Tar/Pitch | 90 | 81 | 10% |
| Wool | 99 | 87 | 12% |
| Copper | 119 | 106 | 11% |
| Iron | 120 | 105 | 13% |
| Wine | 142 | 126 | 11% |
| Cloth | 222 | 186 | 16% |
| Spices | 356 | 309 | 13% |
| Silk | 447 | 395 | 12% |

**London (5% tax, hub)**
| Good | Buy Price | Sell Price |
|------|-----------|------------|
| Coal | 43 | 39 |
| Grain | 52 | 48 |
| Fish | 59 | 52 |
| Salt | 77 | 67 |
| Timber | 80 | 74 |
| Wool | 88 | 79 |
| Hemp | 103 | 90 |
| Iron | 108 | 95 |
| Copper | 132 | 118 |
| Cloth | 162 | 141 |
| Wine | 173 | 152 |
| Spices | 339 | 307 |
| Silk | 467 | 398 |

**Notable arbitrage**: Buy Grain at London (52), sell at Plymouth (69) = +17/unit before tax.

---

## Upcoming Changes (Dev Roadmap)

> ⚠️ These are planned changes communicated by the developer. Values in the doc may become outdated.

- **Upkeep reduction**: Dev plans to cut upkeep amounts
- **Market shock interval**: Changing to **every 8,640 seconds (144 minutes / 2.4 hours)**
- **Contracts/missions**: Dev is working on a contracts system — likely trade missions

These changes mean our bot should be resilient to value changes. Always fetch ship types, trader positions, and economy data dynamically rather than hardcoding.

---

## Key Rules Summary

1. **Bankruptcy**: Run out of money → company locked. Dev can manually unlock with a ~3 month runway.
2. **Quotes expire in 120 seconds** — or trade immediately without a quote via `POST /trade/execute`.
3. **Reputation**: Earned by trading with other players (P2P market). Failed market orders = punishment (fine/reputation loss).
4. **Upkeep every ~5 hours** on ships and warehouses.
5. **Tax on all transactions** (2% regular ports, 5% hub ports).
6. **Market shocks** change prices dynamically — monitor world SSE for notifications.
7. **Passengers**: Future feature — `passengers` field on ship types is currently unused.

---

## Open Questions

### Stock/Price Bounds Dynamics
- Trader positions show textual labels for stock and price levels
- **Unknown**: Exact restock rates and how buying/selling affects stock levels
- Prices are influenced by market shocks (upcoming: every ~144 minutes)

### Country System
- Ports belong to countries (by UUID)
- **Unknown**: Gameplay effects beyond grouping (if any)

### Warehouse Upgrade Costs
- Warehouses can be upgraded (`grow`) and downgraded (`shrink`)
- **Unknown**: Cost per level, capacity per level, max level