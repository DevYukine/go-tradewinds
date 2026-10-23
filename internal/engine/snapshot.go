package engine

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"sort"
	"strings"
	"time"

	"go.uber.org/zap"
)

type accountState struct {
	Catalog    Catalog
	Clock      Clock
	Ships      []ShipState
	Markets    map[string]Market
	Ports      map[string]PortDetail
	Routes     map[string]Route
	Contracts  []Contract
	Vaults     []Vault
	Rates      []Rate
	Factors    []string
	Quotes     map[string]YardQuote
	HomePortID string
}

func decode[T any](data map[string]json.RawMessage, key string) T {
	var value T
	_ = json.Unmarshal(data[key], &value)
	return value
}

func encode(value any) json.RawMessage {
	data, _ := json.Marshal(value)
	return data
}

func (r *Runner) capture(ctx context.Context, key, path string, paginated bool) bool {
	now := time.Now()
	var data json.RawMessage
	var err error
	if paginated {
		data, err = r.readPages(ctx, path)
	} else {
		err = r.api.Get(ctx, path, &data)
	}
	meta := r.meta[key]
	failedBefore := meta.Stale || meta.Error != ""
	meta.AttemptedAt = now
	meta.Complete = false
	if err != nil {
		if !failedBefore && ctx.Err() == nil && !errors.Is(err, context.Canceled) {
			r.log.Warn("dataset refresh failed", zap.String("dataset", key), zap.Bool("timeout", errors.Is(err, context.DeadlineExceeded)))
		}
		meta.Error = err.Error()
		meta.Stale = true
		r.meta[key] = meta
		return false
	}
	if !json.Valid(data) || string(data) == "null" {
		if !failedBefore && ctx.Err() == nil {
			r.log.Warn("dataset refresh failed", zap.String("dataset", key), zap.String("reason", "invalid non-null JSON dataset"))
		}
		meta.Error = "upstream response is not the documented non-null JSON dataset"
		meta.Stale = true
		r.meta[key] = meta
		return false
	}
	r.snapshot[key] = data
	var observed struct {
		AsOf *time.Time `json:"as_of"`
	}
	_ = json.Unmarshal(data, &observed)
	r.meta[key] = DatasetMeta{
		UpdatedAt:    time.Now(),
		AttemptedAt:  now,
		Generation:   r.generation,
		Complete:     true,
		UpstreamAsOf: observed.AsOf,
	}
	return true
}

func (r *Runner) readPages(ctx context.Context, path string) (json.RawMessage, error) {
	endpoint, err := url.Parse(path)
	if err != nil {
		return nil, err
	}
	query := endpoint.Query()
	query.Set("limit", "200")
	rows := make([]json.RawMessage, 0)
	seen := make(map[string]bool)
	for {
		endpoint.RawQuery = query.Encode()
		var page []json.RawMessage
		if err := r.api.Get(ctx, endpoint.String(), &page); err != nil {
			return nil, err
		}
		if page == nil {
			return nil, errors.New("paginated list returned null instead of an array")
		}
		for _, row := range page {
			id, err := pageID(row)
			if err != nil {
				return nil, err
			}
			if seen[id] {
				return nil, fmt.Errorf("pagination repeated id %s", id)
			}
			seen[id] = true
			rows = append(rows, row)
		}
		if len(page) < 200 {
			return json.Marshal(rows)
		}
		id, err := pageID(page[len(page)-1])
		if err != nil {
			return nil, err
		}
		query.Set("cursor", id)
	}
}

func pageID(row json.RawMessage) (string, error) {
	var envelope struct {
		ID   string `json:"id"`
		Ship struct {
			ID string `json:"id"`
		} `json:"ship"`
		Convoy struct {
			ID string `json:"id"`
		} `json:"convoy"`
	}
	if err := json.Unmarshal(row, &envelope); err != nil {
		return "", err
	}
	for _, id := range []string{envelope.ID, envelope.Ship.ID, envelope.Convoy.ID} {
		if id != "" {
			return id, nil
		}
	}
	return "", errors.New("paginated row has no documented id")
}

func (r *Runner) captureContracts(ctx context.Context) {
	var combined []json.RawMessage
	combined = make([]json.RawMessage, 0)
	meta := r.meta["contracts"]
	failedBefore := meta.Stale || meta.Error != ""
	meta.AttemptedAt = time.Now()
	meta.Complete = false
	for _, status := range []string{"open", "active", "completed", "cancelled", "defaulted"} {
		rows, err := r.readPages(ctx, "/v1/contract/list?status="+status)
		if err != nil {
			if !failedBefore && ctx.Err() == nil && !errors.Is(err, context.Canceled) {
				r.log.Warn("dataset refresh failed", zap.String("dataset", "contracts"), zap.Bool("timeout", errors.Is(err, context.DeadlineExceeded)))
			}
			meta.Error = err.Error()
			meta.Stale = true
			r.meta["contracts"] = meta
			return
		}
		var page []json.RawMessage
		if err := json.Unmarshal(rows, &page); err != nil {
			if !failedBefore && ctx.Err() == nil {
				r.log.Warn("dataset refresh failed", zap.String("dataset", "contracts"), zap.String("reason", "invalid contract dataset"))
			}
			meta.Error = err.Error()
			meta.Stale = true
			r.meta["contracts"] = meta
			return
		}
		combined = append(combined, page...)
	}
	// A contract can change state between status queries. Keep its newest copy;
	// never let duplicated visibility turn into duplicated sealed cargo.
	byID := make(map[string]json.RawMessage, len(combined))
	order := make([]string, 0, len(combined))
	for _, row := range combined {
		id, err := pageID(row)
		if err != nil {
			continue
		}
		if _, exists := byID[id]; !exists {
			order = append(order, id)
		}
		byID[id] = row
	}
	combined = combined[:0]
	for _, id := range order {
		combined = append(combined, byID[id])
	}
	r.snapshot["contracts"] = encode(combined)
	r.meta["contracts"] = DatasetMeta{
		UpdatedAt:   time.Now(),
		AttemptedAt: meta.AttemptedAt,
		Generation:  r.generation,
		Complete:    true,
	}
}

func (r *Runner) captureChild(ctx context.Context, group, key, path string, values map[string]json.RawMessage) {
	child := group + "/" + key
	if r.capture(ctx, child, path, false) {
		values[key] = r.snapshot[child]
	}
	delete(r.snapshot, child)
}

// finishGroup persists one child-keyed group and prunes it to exactly the
// keys requested in this generation. Values and per-entry metadata for keys
// the fleet no longer requests must disappear: historical books, routes and
// quotes otherwise accumulate forever across travel and eventually exceed the
// store's snapshot budget. A currently requested child whose capture failed
// keeps its last-good value and its stale/error metadata so the planner sees
// visible failure instead of silently missing data.
func (r *Runner) finishGroup(group string, values map[string]json.RawMessage, keys []string) {
	current := make(map[string]bool, len(keys))
	for _, key := range keys {
		current[key] = true
	}
	for key := range values {
		if !current[key] {
			delete(values, key)
		}
	}
	// A failed capture leaves per-entry metadata without a persisted value, so
	// the value map alone cannot identify metadata orphans. Sweep the group
	// prefix against the current keys: children this generation no longer
	// requests lose their metadata, while current failures keep the visible
	// stale/error state the planner consumes.
	prefix := group + "/"
	for key := range r.meta {
		if strings.HasPrefix(key, prefix) && !current[strings.TrimPrefix(key, prefix)] {
			delete(r.meta, key)
		}
	}
	r.snapshot[group] = encode(values)
	now := time.Now()
	meta := DatasetMeta{
		UpdatedAt:   now,
		AttemptedAt: now,
		Generation:  r.generation,
		Complete:    true,
	}
	for _, key := range keys {
		child := r.meta[group+"/"+key]
		if child.Error != "" || child.Stale || child.UpdatedAt.IsZero() {
			meta.Stale = true
			meta.Complete = false
			meta.Error = "one or more entries failed; see per-entry metadata"
		}
	}
	r.meta[group] = meta
}

func (r *Runner) refresh(ctx context.Context) error {
	for _, dataset := range []struct {
		key   string
		path  string
		pages bool
	}{
		{"whoami", "/v1/auth/whoami", false},
		{"player", "/v1/player/profile", false},
		{"profile", "/v1/company/profile", false},
		{"settings", "/v1/company/settings", false},
		{"catalog", "/v1/world/catalog", false},
		{"clock", "/v1/world/clock", false},
		{"zones", "/v1/world/zones", false},
		{"expansion", "/v1/world/expansion", false},
		{"leaderboard", "/v1/world/leaderboard", false},
		{"world_events", "/v1/world/events?include_expired=true", true},
		{"vaults", "/v1/treasury/vaults", false},
		{"rates", "/v1/treasury/rates", false},
		{"ships", "/v1/fleet/ships", true},
		{"warehouses", "/v1/warehouse/list", false},
		{"orders", "/v1/market/orders", true},
		{"fills", "/v1/market/fills", true},
		{"factors", "/v1/company/factors", false},
		{"insurance", "/v1/insurance/list", true},
		{"futures", "/v1/future/list", true},
		{"convoys", "/v1/fleet/convoys", true},
		{"marques", "/v1/fleet/marques", true},
		{"patrols", "/v1/fleet/patrols", true},
		{"ledger", "/v1/treasury/ledger", true},
	} {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		r.capture(ctx, dataset.key, dataset.path, dataset.pages)
	}
	r.captureContracts(ctx)
	state := r.state()
	details := decode[map[string]json.RawMessage](r.snapshot, "ship_details")
	if details == nil {
		details = make(map[string]json.RawMessage)
	}
	cargo := make(map[string]map[string]int64)
	used := decode[[]string](r.snapshot, "_used_ship_names")
	usedSet := make(map[string]bool, len(used))
	for _, name := range used {
		usedSet[name] = true
	}
	presence := make(map[string]bool)
	current := PlanningInput{Now: time.Now(), Clock: state.Clock, Metadata: r.meta, Generation: r.generation}
	classes := make(map[string]bool)
	shipKeys := make([]string, 0, len(state.Ships))
	for _, ship := range state.Ships {
		shipKeys = append(shipKeys, ship.Ship.ID)
		r.captureChild(ctx, "ship_details", ship.Ship.ID, "/v1/fleet/ship?ship_id="+url.QueryEscape(ship.Ship.ID), details)
		if !usedSet[ship.Ship.Name] {
			used = append(used, ship.Ship.Name)
			usedSet[ship.Ship.Name] = true
		}
		cargo[ship.Ship.ID] = ship.CargoManifest
		if ship.Ship.Status != "sunk" {
			classes[ship.Ship.ShipClass] = true
			if current.fresh("ships") && (ship.Ship.Status == "berthed" || ship.Ship.Status == "anchored_roadstead") && ship.Voyage == nil && ship.Ship.CurrentPortID != "" {
				presence[ship.Ship.CurrentPortID] = true
			}
		}
	}
	for _, factor := range state.Factors {
		if current.fresh("factors") && factor != "" {
			presence[factor] = true
		}
	}
	r.finishGroup("ship_details", details, shipKeys)
	r.snapshot["cargo"] = encode(cargo)
	r.meta["cargo"] = r.meta["ships"]
	r.snapshot["_used_ship_names"] = encode(used)
	inventories := decode[map[string]json.RawMessage](r.snapshot, "inventory")
	if inventories == nil {
		inventories = make(map[string]json.RawMessage)
	}
	warehouses := decode[[]struct {
		PortID string `json:"port_id"`
	}](r.snapshot, "warehouses")
	inventoryKeys := make([]string, 0, len(warehouses))
	for _, warehouse := range warehouses {
		inventoryKeys = append(inventoryKeys, warehouse.PortID)
		r.captureChild(ctx, "inventory", warehouse.PortID, "/v1/warehouse/inventory?port_id="+url.QueryEscape(warehouse.PortID), inventories)
	}
	r.finishGroup("inventory", inventories, inventoryKeys)
	markets := decode[map[string]json.RawMessage](r.snapshot, "books")
	ports := decode[map[string]json.RawMessage](r.snapshot, "ports")
	routes := decode[map[string]json.RawMessage](r.snapshot, "routes")
	quotes := decode[map[string]json.RawMessage](r.snapshot, "yard_quotes")
	if markets == nil {
		markets = make(map[string]json.RawMessage)
	}
	if ports == nil {
		ports = make(map[string]json.RawMessage)
	}
	if routes == nil {
		routes = make(map[string]json.RawMessage)
	}
	if quotes == nil {
		quotes = make(map[string]json.RawMessage)
	}
	var portKeys, bookKeys, routeKeys, quoteKeys []string
	for _, port := range state.Catalog.Ports {
		if port.Phase > state.Clock.UnlockedPhase {
			continue
		}
		portKeys = append(portKeys, port.ID)
		r.captureChild(ctx, "ports", port.ID, "/v1/world/port?port_id="+url.QueryEscape(port.ID), ports)
		if presence[port.ID] {
			bookKeys = append(bookKeys, port.ID)
			r.captureChild(ctx, "books", port.ID, "/v1/market/book?port_id="+url.QueryEscape(port.ID), markets)
		}
		if port.HasShipyard && presence[port.ID] {
			for _, hull := range state.Catalog.ShipClasses {
				if hull.Phase > state.Clock.UnlockedPhase {
					continue
				}
				key := port.ID + "|" + hull.ID
				quoteKeys = append(quoteKeys, key)
				r.captureChild(ctx, "yard_quotes", key, "/v1/shipyard/quote?port_id="+url.QueryEscape(port.ID)+"&ship_class="+url.QueryEscape(hull.ID)+"&build_mode=turnkey_cash", quotes)
				classes[hull.ID] = true
			}
		}
	}
	// Only actual fleet/presence ports are route origins. Every candidate has
	// both an outward and a return quote, including candidate growth hulls.
	origins := make([]string, 0, len(presence))
	for origin := range presence {
		origins = append(origins, origin)
	}
	sort.Strings(origins)
	classIDs := make([]string, 0, len(classes))
	for class := range classes {
		classIDs = append(classIDs, class)
	}
	sort.Strings(classIDs)
	seenRoutes := make(map[string]bool)
	for _, origin := range origins {
		for _, destination := range state.Catalog.Ports {
			if origin == destination.ID || destination.Phase > state.Clock.UnlockedPhase {
				continue
			}
			for _, class := range classIDs {
				for _, pair := range [][2]string{{origin, destination.ID}, {destination.ID, origin}} {
					key := routeKey(pair[0], pair[1], class)
					if seenRoutes[key] {
						continue
					}
					seenRoutes[key] = true
					routeKeys = append(routeKeys, key)
					path := "/v1/world/route?from=" + url.QueryEscape(pair[0]) + "&to=" + url.QueryEscape(pair[1]) + "&ship_class=" + url.QueryEscape(class)
					r.captureChild(ctx, "routes", key, path, routes)
				}
			}
		}
	}
	// Public source routes are limited to three evidence-backed pairs across
	// the account. A sourcing origin never enters private presence.
	state.Ports = decode[map[string]PortDetail](map[string]json.RawMessage{"ports": encode(ports)}, "ports")
	state.Routes = decode[map[string]Route](map[string]json.RawMessage{"routes": encode(routes)}, "routes")
	state.Markets = withoutOwnOrders(decode[map[string]Market](map[string]json.RawMessage{"books": encode(markets)}, "books"), r.ownedOrders())
	sourcingInput := r.planningInputFrom(ShipState{}, state)
	shortlisted := 0
	for _, ship := range state.Ships {
		mission, data, err := r.loadMission(ctx, ship.Ship.ID)
		if err != nil {
			return err
		}
		var pairs [][2]string
		if mission != nil && data.Relocation != nil {
			pairs = append(pairs, [2]string{data.Plan.Origin, data.Plan.Destination}, [2]string{data.Plan.Destination, data.Relocation.TargetDestination})
		} else if mission == nil && shortlisted < 3 && current.fresh("ships") && emptyCargo(ship) && ship.Voyage == nil &&
			(ship.Ship.Status == "berthed" || ship.Ship.Status == "anchored_roadstead") && ship.Ship.ConvoyID == "" {
			sourcingInput.Ship = ship
			for _, plan := range sourcingPairs(sourcingInput) {
				if !plan.Accepted || shortlisted >= 3 {
					continue
				}
				for _, contract := range state.Contracts {
					if contract.ID == plan.ContractID {
						pairs = append(pairs, [2]string{plan.Destination, contract.OriginPortID})
						shortlisted++
						break
					}
				}
			}
		}
		for _, pair := range pairs {
			for _, endpoints := range [][2]string{pair, {pair[1], pair[0]}} {
				if endpoints[0] == "" || endpoints[1] == "" || endpoints[0] == endpoints[1] {
					continue
				}
				key := routeKey(endpoints[0], endpoints[1], ship.Ship.ShipClass)
				if seenRoutes[key] {
					continue
				}
				seenRoutes[key] = true
				routeKeys = append(routeKeys, key)
				path := "/v1/world/route?from=" + url.QueryEscape(endpoints[0]) + "&to=" + url.QueryEscape(endpoints[1]) + "&ship_class=" + url.QueryEscape(ship.Ship.ShipClass)
				r.captureChild(ctx, "routes", key, path, routes)
			}
		}
	}
	// Imported/recovered holds can have several sealed destinations. Fetch
	// their actual inter-stop routes without pretending to have quay access.
	for _, ship := range state.Ships {
		destinations := make(map[string]bool)
		for _, contract := range state.Contracts {
			if contract.ContractType == "freight" && contract.Status == "active" &&
				contract.CarrierCompanyID == r.account.CompanyID && contract.CarrierShipID == ship.Ship.ID {
				destinations[contract.DestPortID] = true
			}
		}
		if len(destinations) == 0 {
			continue
		}
		mission, _, err := r.loadMission(ctx, ship.Ship.ID)
		if err != nil {
			return err
		}
		if mission != nil && mission.Destination != "" {
			destinations[mission.Destination] = true
		}
		for from := range destinations {
			for to := range destinations {
				if from == to {
					continue
				}
				key := routeKey(from, to, ship.Ship.ShipClass)
				if seenRoutes[key] {
					continue
				}
				seenRoutes[key] = true
				routeKeys = append(routeKeys, key)
				path := "/v1/world/route?from=" + url.QueryEscape(from) + "&to=" + url.QueryEscape(to) + "&ship_class=" + url.QueryEscape(ship.Ship.ShipClass)
				r.captureChild(ctx, "routes", key, path, routes)
			}
		}
	}
	r.finishGroup("ports", ports, portKeys)
	r.finishGroup("books", markets, bookKeys)
	r.finishGroup("routes", routes, routeKeys)
	r.finishGroup("yard_quotes", quotes, quoteKeys)
	r.snapshot["_meta"] = encode(r.meta)
	r.stateCache = r.state()
	r.stateCache.Markets = withoutOwnOrders(r.stateCache.Markets, r.ownedOrders())
	if err := r.db.SaveSnapshot(ctx, r.account.ID, r.snapshot); err != nil {
		return err
	}
	return r.recordBalances(ctx)
}

func (r *Runner) state() accountState {
	state := accountState{
		Catalog:   decode[Catalog](r.snapshot, "catalog"),
		Clock:     decode[Clock](r.snapshot, "clock"),
		Ships:     decode[[]ShipState](r.snapshot, "ships"),
		Markets:   decode[map[string]Market](r.snapshot, "books"),
		Ports:     decode[map[string]PortDetail](r.snapshot, "ports"),
		Routes:    decode[map[string]Route](r.snapshot, "routes"),
		Contracts: decode[[]Contract](r.snapshot, "contracts"),
		Rates:     decode[[]Rate](r.snapshot, "rates"),
		Factors:   decode[[]string](r.snapshot, "factors"),
		Quotes:    decode[map[string]YardQuote](r.snapshot, "yard_quotes"),
	}
	var vaults struct {
		Vaults []Vault `json:"vaults"`
	}
	_ = json.Unmarshal(r.snapshot["vaults"], &vaults)
	state.Vaults = vaults.Vaults
	var profile struct {
		Company struct {
			HomePortID string `json:"home_port_id"`
		} `json:"company"`
	}
	_ = json.Unmarshal(r.snapshot["profile"], &profile)
	state.HomePortID = profile.Company.HomePortID
	return state
}

func (r *Runner) recordBalances(ctx context.Context) error {
	if !r.fresh("vaults") {
		return nil
	}
	var liquid int64
	owned := make(map[string]bool)
	for _, vault := range r.stateCache.Vaults {
		owned[vault.AccountID] = true
		if vault.AccountType == "vault" {
			liquid += vault.BalancePence
		}
	}
	if err := r.db.RecordMetric(ctx, r.account.ID, "liquid_funds_pence", float64(liquid)); err != nil {
		return err
	}
	if !r.fresh("ledger") {
		return nil
	}
	rows := decode[[]struct {
		Debit  string `json:"debit_account_id"`
		Credit string `json:"credit_account_id"`
		Amount int64  `json:"amount_pence"`
		Reason string `json:"reason"`
	}](r.snapshot, "ledger")
	var outflow, bills int64
	for _, row := range rows {
		if !owned[row.Debit] || owned[row.Credit] {
			continue
		}
		outflow += row.Amount
		for _, reason := range []string{"upkeep", "rent", "berth", "toll", "fee", "customs", "patrol", "marque"} {
			if strings.Contains(row.Reason, reason) {
				bills += row.Amount
				break
			}
		}
	}
	if err := r.db.RecordMetric(ctx, r.account.ID, "cash_outflow_pence", float64(outflow)); err != nil {
		return err
	}
	return r.db.RecordMetric(ctx, r.account.ID, "expenses_pence", float64(bills))
}
