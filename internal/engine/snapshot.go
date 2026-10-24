package engine

import (
	"context"
	"encoding/json"
	"errors"
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

func (r *Runner) applyCapture(ctx context.Context, key string, result fetchResult) bool {
	if result.err == nil && (key == "insurance" || key == "insurance_owned") {
		var policies []InsurancePolicy
		if err := json.Unmarshal(result.data, &policies); err != nil {
			result.err = err
		}
	}
	meta := r.meta[key]
	failedBefore := meta.Stale || meta.Error != ""
	meta.AttemptedAt = result.attemptedAt
	meta.Complete = false
	if result.err != nil {
		if ctx.Err() == nil && !errors.Is(result.err, context.Canceled) {
			level := zap.WarnLevel
			if failedBefore && meta.Error == result.err.Error() {
				level = zap.DebugLevel
			}
			if entry := r.log.Check(level, "dataset refresh failed"); entry != nil {
				fields := []zap.Field{zap.Bool("timeout", errors.Is(result.err, context.DeadlineExceeded))}
				entry.Write(r.scrubOutcomeFields(ctx, fields, []outcomeText{
					{field: "dataset", value: key},
					{field: "cause", value: result.err.Error()},
				})...)
			}
		}
		meta.Error = result.err.Error()
		meta.Stale = true
		r.meta[key] = meta
		return false
	}
	r.snapshot[key] = result.data
	var observed struct {
		AsOf *time.Time `json:"as_of"`
	}
	_ = json.Unmarshal(result.data, &observed)
	r.meta[key] = DatasetMeta{
		UpdatedAt:    result.completedAt,
		AttemptedAt:  result.attemptedAt,
		Generation:   r.generation,
		Complete:     true,
		UpstreamAsOf: observed.AsOf,
	}
	if failedBefore {
		if entry := r.log.Check(zap.InfoLevel, "dataset refresh recovered"); entry != nil {
			entry.Write(r.scrubOutcomeFields(ctx, nil, []outcomeText{{field: "dataset", value: key}})...)
		}
	}
	return true
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

func (r *Runner) captureWave(ctx context.Context, tasks []fetchTask, groups map[string]map[string]json.RawMessage) error {
	results, err := fetchWave(ctx, r.api, tasks)
	if err != nil {
		return err
	}
	for index, task := range tasks {
		if err := ctx.Err(); err != nil {
			return err
		}
		captured := r.applyCapture(ctx, task.key, results[index])
		group, key, child := strings.Cut(task.key, "/")
		if values, exists := groups[group]; child && exists {
			if captured {
				values[key] = r.snapshot[task.key]
			}
			delete(r.snapshot, task.key)
		}
	}
	return nil
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
		if child.Error != "" || child.Stale || !child.Complete || child.Generation != r.generation || child.UpdatedAt.IsZero() || len(values[key]) == 0 {
			meta.Stale = true
			meta.Complete = false
			meta.Error = "one or more entries failed; see per-entry metadata"
		}
	}
	r.meta[group] = meta
}

func (r *Runner) refresh(ctx context.Context) error {
	if err := r.captureWave(ctx, []fetchTask{
		{"whoami", "/v1/auth/whoami", fetchJSON},
		{"player", "/v1/player/profile", fetchJSON},
		{"profile", "/v1/company/profile", fetchJSON},
		{"settings", "/v1/company/settings", fetchJSON},
		{"catalog", "/v1/world/catalog", fetchJSON},
		{"clock", "/v1/world/clock", fetchJSON},
		{"zones", "/v1/world/zones", fetchJSON},
		{"expansion", "/v1/world/expansion", fetchJSON},
		{"leaderboard", "/v1/world/leaderboard", fetchJSON},
		{"world_events", "/v1/world/events?include_expired=true", fetchPages},
		{"vaults", "/v1/treasury/vaults", fetchJSON},
		{"rates", "/v1/treasury/rates", fetchJSON},
		{"ships", "/v1/fleet/ships", fetchPages},
		{"warehouses", "/v1/warehouse/list", fetchJSON},
		{"orders", "/v1/market/orders", fetchPages},
		{"fills", "/v1/market/fills", fetchPages},
		{"factors", "/v1/company/factors", fetchJSON},
		{"insurance", "/v1/insurance/list", fetchPages},
		{"insurance_owned", "/v1/insurance/list?underwriter_company_id=" + url.QueryEscape(r.account.CompanyID), fetchPages},
		{"futures", "/v1/future/list", fetchPages},
		{"convoys", "/v1/fleet/convoys", fetchPages},
		{"marques", "/v1/fleet/marques", fetchPages},
		{"patrols", "/v1/fleet/patrols", fetchPages},
		{"ledger", "/v1/treasury/ledger", fetchPages},
		{"contracts", "", fetchContracts},
	}, nil); err != nil {
		return err
	}
	state := r.state()
	var childTasks []fetchTask
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
		childTasks = append(childTasks, fetchTask{key: "ship_details/" + ship.Ship.ID, path: "/v1/fleet/ship?ship_id=" + url.QueryEscape(ship.Ship.ID)})
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
		childTasks = append(childTasks, fetchTask{key: "inventory/" + warehouse.PortID, path: "/v1/warehouse/inventory?port_id=" + url.QueryEscape(warehouse.PortID)})
	}
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
	groups := map[string]map[string]json.RawMessage{
		"ship_details": details,
		"inventory":    inventories,
		"ports":        ports,
		"books":        markets,
		"routes":       routes,
		"yard_quotes":  quotes,
	}
	var portKeys, bookKeys, routeKeys, quoteKeys []string
	for _, port := range state.Catalog.Ports {
		if port.Phase > state.Clock.UnlockedPhase {
			continue
		}
		portKeys = append(portKeys, port.ID)
		childTasks = append(childTasks, fetchTask{key: "ports/" + port.ID, path: "/v1/world/port?port_id=" + url.QueryEscape(port.ID)})
		if presence[port.ID] {
			bookKeys = append(bookKeys, port.ID)
			childTasks = append(childTasks, fetchTask{key: "books/" + port.ID, path: "/v1/market/book?port_id=" + url.QueryEscape(port.ID)})
		}
		if port.HasShipyard && presence[port.ID] {
			for _, hull := range state.Catalog.ShipClasses {
				if hull.Phase > state.Clock.UnlockedPhase {
					continue
				}
				key := port.ID + "|" + hull.ID
				quoteKeys = append(quoteKeys, key)
				childTasks = append(childTasks, fetchTask{key: "yard_quotes/" + key, path: "/v1/shipyard/quote?port_id=" + url.QueryEscape(port.ID) + "&ship_class=" + url.QueryEscape(hull.ID) + "&build_mode=turnkey_cash"})
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
					childTasks = append(childTasks, fetchTask{key: "routes/" + key, path: path})
				}
			}
		}
	}
	if err := r.captureWave(ctx, childTasks, groups); err != nil {
		return err
	}
	r.finishGroup("ship_details", details, shipKeys)
	r.finishGroup("inventory", inventories, inventoryKeys)
	r.finishGroup("ports", ports, portKeys)
	r.finishGroup("books", markets, bookKeys)
	r.finishGroup("yard_quotes", quotes, quoteKeys)
	// Inspect at most three new public source/charter route pairs per pass.
	// Reject known illegal legs before spending that budget on later pairs;
	// neither catalog production nor a route quote grants private quay access.
	state.Ports = decode[map[string]PortDetail](map[string]json.RawMessage{"ports": encode(ports)}, "ports")
	state.Routes = decode[map[string]Route](map[string]json.RawMessage{"routes": encode(routes)}, "routes")
	state.Markets = withoutOwnOrders(decode[map[string]Market](map[string]json.RawMessage{"books": encode(markets)}, "books"), r.ownedOrders())
	sourcingInput := r.planningInputFrom(ShipState{}, state)
	if err := r.loadSourcingHistory(ctx, &sourcingInput); err != nil {
		return err
	}
	captureSourcePair := func(pair [2]string, class string) error {
		var tasks []fetchTask
		for _, endpoints := range [][2]string{pair, {pair[1], pair[0]}} {
			if endpoints[0] == "" || endpoints[1] == "" || endpoints[0] == endpoints[1] {
				continue
			}
			key := routeKey(endpoints[0], endpoints[1], class)
			if seenRoutes[key] {
				continue
			}
			seenRoutes[key] = true
			routeKeys = append(routeKeys, key)
			path := "/v1/world/route?from=" + url.QueryEscape(endpoints[0]) + "&to=" + url.QueryEscape(endpoints[1]) + "&ship_class=" + url.QueryEscape(class)
			tasks = append(tasks, fetchTask{key: "routes/" + key, path: path})
		}
		if err := r.captureWave(ctx, tasks, groups); err != nil {
			return err
		}
		for _, task := range tasks {
			key := strings.TrimPrefix(task.key, "routes/")
			var route Route
			if json.Unmarshal(routes[key], &route) == nil {
				sourcingInput.Routes[key] = route
			}
		}
		return nil
	}
	shortlisted := 0
	inspections := 0
	for _, ship := range state.Ships {
		mission, data, err := r.loadMission(ctx, ship.Ship.ID)
		if err != nil {
			return err
		}
		if mission != nil && data.Relocation != nil {
			if err := captureSourcePair([2]string{data.Plan.Origin, data.Plan.Destination}, ship.Ship.ShipClass); err != nil {
				return err
			}
			if err := captureSourcePair([2]string{data.Plan.Destination, data.Relocation.TargetDestination}, ship.Ship.ShipClass); err != nil {
				return err
			}
		} else if mission == nil && shortlisted < sourcingPairLimit && current.fresh("ships") && emptyCargo(ship) && ship.Voyage == nil &&
			(ship.Ship.Status == "berthed" || ship.Ship.Status == "anchored_roadstead") && ship.Ship.ConvoyID == "" {
			sourcingInput.Ship = ship
			hull, exists := findHull(state.Catalog, ship.Ship.ShipClass)
			if !exists {
				continue
			}
			for _, plan := range sourcingCandidates(sourcingInput) {
				if !plan.Accepted || shortlisted >= sourcingPairLimit {
					continue
				}
				var target string
				for _, contract := range state.Contracts {
					if contract.ID == plan.ContractID {
						target = contract.OriginPortID
						break
					}
				}
				if target == "" {
					continue
				}
				pair := [2]string{plan.Destination, target}
				illegal := false
				missing := false
				for _, endpoints := range [][2]string{pair, {pair[1], pair[0]}} {
					key := routeKey(endpoints[0], endpoints[1], hull.ID)
					if route, retained := sourcingInput.Routes[key]; retained && ValidateRoute(route, hull, state.Catalog.SeaZones) != nil {
						illegal = true
						// Retain the rejection while this public pair is relevant.
						// It cannot authorize a departure or private book read.
						if !seenRoutes[key] {
							seenRoutes[key] = true
							routeKeys = append(routeKeys, key)
						}
					}
					missing = missing || !seenRoutes[key]
				}
				if illegal || missing && inspections >= sourcingPairLimit {
					continue
				}
				if missing {
					inspections++
				}
				if err := captureSourcePair(pair, hull.ID); err != nil {
					return err
				}
				if validateRelocationTarget(sourcingInput, plan).Accepted {
					shortlisted++
				}
			}
		}
	}
	// Imported/recovered holds can have several sealed destinations. Fetch
	// their actual inter-stop routes without pretending to have quay access.
	childTasks = nil
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
		destinationIDs := make([]string, 0, len(destinations))
		for destination := range destinations {
			destinationIDs = append(destinationIDs, destination)
		}
		sort.Strings(destinationIDs)
		for _, from := range destinationIDs {
			for _, to := range destinationIDs {
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
				childTasks = append(childTasks, fetchTask{key: "routes/" + key, path: path})
			}
		}
	}
	if err := r.captureWave(ctx, childTasks, groups); err != nil {
		return err
	}
	r.finishGroup("routes", routes, routeKeys)
	if err := ctx.Err(); err != nil {
		return err
	}
	r.stateCache = r.state()
	if err := r.recordUnderwriting(ctx); err != nil {
		return err
	}
	r.snapshot["_meta"] = encode(r.meta)
	if err := r.db.SaveSnapshot(ctx, r.account.ID, r.snapshot); err != nil {
		return err
	}
	r.stateCache = r.state()
	r.stateCache.Markets = withoutOwnOrders(r.stateCache.Markets, r.ownedOrders())
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
