package engine

import (
	"context"
	"encoding/json"
	"errors"
	"net/url"
	"sort"
	"strings"
	"time"

	"github.com/DevYukine/go-tradewinds/internal/gameapi"
	"github.com/DevYukine/go-tradewinds/internal/store"
	"go.uber.org/zap"
)

const (
	// SourcingRouteRetentionLimit caps optional route history in live and replay snapshots.
	SourcingRouteRetentionLimit = 256
	// SourcingRouteRetentionBytes includes escaped route and metadata map entries.
	SourcingRouteRetentionBytes = 1 << 20
	// SourcingRouteRetentionScanLimit bounds newest-first optional history scanning.
	SourcingRouteRetentionScanLimit = 4 * SourcingRouteRetentionLimit
	snapshotSafetyMargin            = 64 << 10
)

func gameVersionLogText(version string) string {
	version = gameapi.RedactPublicText(version)
	count := 0
	for index := range version {
		if count == 64 {
			return version[:index]
		}
		count++
	}
	return version
}

// AccountState is the decoded account frame consumed by the pure planners.
type AccountState struct {
	Catalog           Catalog
	Clock             Clock
	Ships             []ShipState
	Markets           map[string]Market
	Ports             map[string]PortDetail
	Routes            map[string]Route
	HazardQuotes      map[string]HazardQuote
	Contracts         []Contract
	Vaults            []Vault
	Loans             []gameapi.Loan
	InsurancePolicies []InsurancePolicy
	Rates             []Rate
	Factors           []string
	Quotes            map[string]YardQuote
	HomePortID        string
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
	if result.err == nil && key == "rnd_status" {
		var status gameapi.RndStatus
		result.err = json.Unmarshal(result.data, &status)
	}
	if result.err == nil && key == "loans" {
		result.err = validateBorrowerLoans(result.data, r.account.CompanyID)
	}
	if result.err == nil && (key == "insurance" || key == "insurance_owned") {
		var policies []InsurancePolicy
		if err := json.Unmarshal(result.data, &policies); err != nil {
			result.err = err
		}
	}
	if result.err == nil && key == "monopolies" {
		var monopolies []Monopoly
		if err := json.Unmarshal(result.data, &monopolies); err != nil {
			result.err = err
		}
	}
	if result.err == nil && strings.HasPrefix(key, "marque_quotes/") {
		var quote MarqueQuote
		if err := json.Unmarshal(result.data, &quote); err != nil {
			result.err = err
		} else if quote.SeaZoneID != strings.TrimPrefix(key, "marque_quotes/") {
			result.err = errors.New("marque quote names a different sea zone")
		}
	}
	if result.err == nil && key == "marques" {
		var marques []letterOfMarque
		if err := json.Unmarshal(result.data, &marques); err != nil {
			result.err = err
		}
	}
	if result.err == nil && strings.HasPrefix(key, "routes/") {
		from, rest, hasFrom := strings.Cut(strings.TrimPrefix(key, "routes/"), "|")
		to, class, hasClass := strings.Cut(rest, "|")
		var route Route
		if err := json.Unmarshal(result.data, &route); err != nil {
			result.err = err
		} else if !hasFrom || !hasClass || route.FromPort != from || route.ToPort != to ||
			class != "" && route.ShipClass != class {
			result.err = errors.New("route identity disagrees with the requested ports or ship class")
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
// keys requested or explicitly retained in this generation. Values and per-entry
// metadata the fleet no longer needs must disappear: historical books, routes
// and quotes otherwise accumulate forever across travel and eventually exceed
// the store's snapshot budget. A currently requested child whose capture failed
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
		current := child.Generation == r.generation
		if group == "routes" {
			current = r.fresh(group + "/" + key)
		}
		if child.Error != "" || child.Stale || !child.Complete || !current || child.UpdatedAt.IsZero() || len(values[key]) == 0 {
			meta.Stale = true
			meta.Complete = false
			meta.Error = "one or more entries failed; see per-entry metadata"
		}
	}
	r.meta[group] = meta
}

func (r *Runner) refresh(ctx context.Context) error {
	r.passStart = time.Now().UTC()
	previousIdle := PlanningInput{
		Clock:    decode[Clock](r.snapshot, "clock"),
		Metadata: decode[map[string]DatasetMeta](r.snapshot, "_meta"),
	}
	previousIdle.Generation = previousIdle.Metadata["clock"].Generation
	loans, err := r.loanCaptureTask(ctx)
	if err != nil {
		return err
	}
	tasks := []fetchTask{
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
		{"monopolies", "/v1/world/monopolies", fetchPages},
		{"vaults", "/v1/treasury/vaults", fetchJSON},
		loans,
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
	}
	now := time.Now()
	rndStatusRequested := false
	for _, task := range []fetchTask{
		{"rnd_status", "/v1/rnd/status", fetchJSON},
		{"rnd_register", "/v1/rnd/register", fetchJSON},
	} {
		meta := r.meta[task.key]
		if now.Sub(maxTime(meta.AttemptedAt, meta.UpdatedAt)) >= rndObservationInterval {
			tasks = append(tasks, task)
			if task.key == "rnd_status" {
				rndStatusRequested = true
			}
		}
	}
	if err := r.captureWave(ctx, tasks, nil); err != nil {
		return err
	}
	if err := r.refreshFuturesCredit(ctx); err != nil {
		return err
	}
	if err := r.refreshRivalCredit(ctx); err != nil {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		r.logRivalCreditError(ctx, "refresh", err)
	}
	if meta := r.meta["rnd_status"]; rndStatusRequested && meta.Generation == r.generation && meta.Complete && !meta.Stale && meta.Error == "" {
		reason, details := rndDecision(decode[gameapi.RndStatus](r.snapshot, "rnd_status"))
		if err := r.decision(ctx, "", "rnd", reason, details); err != nil {
			return err
		}
	}
	state := r.state()
	if err := r.refreshUnderworld(ctx, state); err != nil {
		return err
	}
	versionChanged := false
	if r.opts.GameVersion != nil {
		current := r.opts.GameVersion()
		if current != "" {
			previous := decode[string](r.snapshot, "_game_version")
			versionChanged = current != previous
			if versionChanged {
				r.log.Info("game version changed; refetching cached routes",
					zap.String("previous_version", gameVersionLogText(previous)), zap.String("game_version", gameVersionLogText(current)))
				r.snapshot["_game_version"] = encode(current)
			}
		}
	}
	// The route API's default max_phase follows the world unlock. Discard
	// cached paths, including known hull rejections, when that boundary moves.
	if _, known := r.snapshot["_route_phase"]; versionChanged || !known || decode[int](r.snapshot, "_route_phase") != state.Clock.UnlockedPhase {
		for key := range r.snapshot {
			if key == "routes" || strings.HasPrefix(key, "routes/") {
				delete(r.snapshot, key)
			}
		}
		for key := range r.meta {
			if key == "routes" || strings.HasPrefix(key, "routes/") {
				delete(r.meta, key)
			}
		}
	}
	r.snapshot["_route_phase"] = encode(state.Clock.UnlockedPhase)
	if r.opts.MarquesEnabled && r.fresh("catalog") {
		if err := r.refreshMarqueQuotes(ctx, state.Catalog); err != nil {
			return err
		}
	}
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
	fleetClasses := make(map[string]bool)
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
			fleetClasses[ship.Ship.ShipClass] = true
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
	for _, position := range decode[[]store.UnderwritingPosition](r.snapshot, "underwriting_positions") {
		if position.ClosedAt != nil {
			continue
		}
		var policy InsurancePolicy
		if json.Unmarshal(position.FrozenPolicy, &policy) != nil || policy.OriginPortID == "" || policy.DestPortID == "" {
			continue
		}
		// Route geometry is public and independent of the insured ship's
		// private hull class. The API permits omitting ship_class.
		key := routeKey(policy.OriginPortID, policy.DestPortID, "")
		if !seenRoutes[key] {
			seenRoutes[key] = true
			routeKeys = append(routeKeys, key)
			if len(routes[key]) == 0 || !r.fresh("routes/"+key) {
				path := "/v1/world/route?from=" + url.QueryEscape(policy.OriginPortID) + "&to=" + url.QueryEscape(policy.DestPortID)
				childTasks = append(childTasks, fetchTask{key: "routes/" + key, path: path})
			}
		}
	}
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
					if len(routes[key]) == 0 || !r.fresh("routes/"+key) {
						path := "/v1/world/route?from=" + url.QueryEscape(pair[0]) + "&to=" + url.QueryEscape(pair[1]) + "&ship_class=" + url.QueryEscape(class)
						childTasks = append(childTasks, fetchTask{key: "routes/" + key, path: path})
					}
				}
			}
		}
	}
	// Committed relocations need all of their public legs regardless of the
	// exploration budget or their position in fleet order.
	shipMissions := make([]struct {
		mission *store.Mission
		data    missionData
	}, len(state.Ships))
	for i, ship := range state.Ships {
		mission, data, err := r.loadMission(ctx, ship.Ship.ID)
		if err != nil {
			return err
		}
		shipMissions[i].mission = mission
		shipMissions[i].data = data
		if data.Relocation == nil {
			continue
		}
		for _, pair := range [][2]string{
			{data.Plan.Origin, data.Plan.Destination},
			{data.Plan.Destination, data.Plan.Origin},
			{data.Plan.Destination, data.Relocation.TargetDestination},
			{data.Relocation.TargetDestination, data.Plan.Destination},
		} {
			if pair[0] == "" || pair[1] == "" || pair[0] == pair[1] {
				continue
			}
			key := routeKey(pair[0], pair[1], ship.Ship.ShipClass)
			if seenRoutes[key] {
				continue
			}
			seenRoutes[key] = true
			routeKeys = append(routeKeys, key)
			if len(routes[key]) == 0 || !r.fresh("routes/"+key) {
				path := "/v1/world/route?from=" + url.QueryEscape(pair[0]) + "&to=" + url.QueryEscape(pair[1]) + "&ship_class=" + url.QueryEscape(ship.Ship.ShipClass)
				childTasks = append(childTasks, fetchTask{key: "routes/" + key, path: path})
			}
		}
	}
	if err := r.captureWave(ctx, childTasks, groups); err != nil {
		return err
	}
	childTasks = nil
	r.finishGroup("ship_details", details, shipKeys)
	r.finishGroup("inventory", inventories, inventoryKeys)
	r.finishGroup("ports", ports, portKeys)
	r.replayPorts = ports
	r.finishGroup("books", markets, bookKeys)
	r.recordQuayBooks(markets, bookKeys, state.Catalog, state.Clock, time.Now())
	r.finishGroup("yard_quotes", quotes, quoteKeys)
	// Exploratory captures, including expired illegal pairs, share the
	// three-new-pair / six-GET budget. Committed legs were requested above.
	// Public evidence never grants private quay access.
	state.Ports = decode[map[string]PortDetail](map[string]json.RawMessage{"ports": encode(ports)}, "ports")
	state.Routes = decode[map[string]Route](map[string]json.RawMessage{"routes": encode(routes)}, "routes")
	state.Markets = withoutOwnOrders(decode[map[string]Market](map[string]json.RawMessage{"books": encode(markets)}, "books"), r.ownedOrders())
	state.Markets = monopolyMarketView(time.Now(), r.snapshot, PlanningSources{Metadata: r.meta, Generation: r.generation}, state.Markets, state.Catalog, state.Clock)
	if err := r.refreshHazardQuotes(ctx, state); err != nil {
		return err
	}
	state.HazardQuotes = decode[map[string]HazardQuote](r.snapshot, hazardQuoteSnapshotKey)
	r.stateCache = state
	r.recordShipIdle(state, previousIdle)
	r.refreshSourcingHistory(ctx, r.planningInput(ShipState{}))
	requiredRoutes := make(map[string]bool, len(routeKeys))
	for _, key := range routeKeys {
		requiredRoutes[key] = true
	}
	exploredRoutes := make(map[string]bool)
	inspections, sourceGETs := 0, 0
	captureSourcePair := func(pair [2]string, class string) (bool, error) {
		var tasks []fetchTask
		var keys []string
		for _, endpoints := range [][2]string{pair, {pair[1], pair[0]}} {
			if endpoints[0] == "" || endpoints[1] == "" || endpoints[0] == endpoints[1] {
				continue
			}
			key := routeKey(endpoints[0], endpoints[1], class)
			if seenRoutes[key] {
				continue
			}
			keys = append(keys, key)
			if len(routes[key]) == 0 || !r.fresh("routes/"+key) {
				path := "/v1/world/route?from=" + url.QueryEscape(endpoints[0]) + "&to=" + url.QueryEscape(endpoints[1]) + "&ship_class=" + url.QueryEscape(class)
				tasks = append(tasks, fetchTask{key: "routes/" + key, path: path})
			}
		}
		if len(tasks) > 0 {
			if inspections >= sourcingPairLimit || sourceGETs+len(tasks) > 2*sourcingPairLimit {
				return false, nil
			}
			inspections++
			sourceGETs += len(tasks)
		}
		for _, key := range keys {
			seenRoutes[key] = true
			exploredRoutes[key] = true
		}
		if err := r.captureWave(ctx, tasks, groups); err != nil {
			return false, err
		}
		for _, task := range tasks {
			key := strings.TrimPrefix(task.key, "routes/")
			var route Route
			if json.Unmarshal(routes[key], &route) == nil {
				state.Routes[key] = route
			}
		}
		return true, nil
	}
	shortlisted := 0
	sourcingBudgetLoaded := false
	for i, ship := range state.Ships {
		mission := shipMissions[i].mission
		data := &shipMissions[i].data
		sourcingEligible := mission == nil && current.fresh("ships") && emptyCargo(ship) && ship.Voyage == nil &&
			(ship.Ship.Status == "berthed" || ship.Ship.Status == "anchored_roadstead") && ship.Ship.ConvoyID == "" &&
			!idleFreight(ship.Ship.ID, state.Contracts)
		if !sourcingBudgetLoaded && (sourcingEligible || data.Relocation != nil || data.SourcingHandoff != nil) {
			r.refreshSourcingBudget(ctx, r.planningInput(ship))
			sourcingBudgetLoaded = true
		}
		if sourcingEligible && shortlisted < sourcingPairLimit {
			sourcingInput, err := r.sourcingInput(ctx, ship)
			if err != nil {
				return err
			}
			if sourcingAdmissionRejection(sourcingInput) != "" {
				continue
			}
			hull, exists := findHull(state.Catalog, ship.Ship.ShipClass)
			if !exists {
				continue
			}
			for _, plan := range sourcingCandidates(sourcingInput) {
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
				illegal, missing := false, false
				for _, endpoints := range [][2]string{pair, {pair[1], pair[0]}} {
					key := routeKey(endpoints[0], endpoints[1], hull.ID)
					if route, retained := sourcingInput.Routes[key]; retained && ValidateRoute(route, hull, state.Catalog.SeaZones) != nil {
						illegal = true
					}
					missing = missing || !seenRoutes[key] && (len(routes[key]) == 0 || !r.fresh("routes/"+key))
				}
				if illegal {
					if missing {
						if _, err := captureSourcePair(pair, hull.ID); err != nil {
							return err
						}
					}
					continue
				}
				if !plan.Accepted || shortlisted >= sourcingPairLimit {
					continue
				}
				captured, err := captureSourcePair(pair, hull.ID)
				if err != nil {
					return err
				}
				if !captured {
					continue
				}
				if validateRelocationTarget(sourcingInput, plan).Accepted {
					shortlisted++
					for _, endpoints := range [][2]string{pair, {pair[1], pair[0]}} {
						key := routeKey(endpoints[0], endpoints[1], hull.ID)
						if !requiredRoutes[key] {
							requiredRoutes[key] = true
							routeKeys = append(routeKeys, key)
						}
					}
				}
			}
		}
	}
	// Observe every persisted continuation, including sailing arrivals and
	// sealed inter-stop legs, without pretending to have private quay access.
	for i, ship := range state.Ships {
		mission := shipMissions[i].mission
		data := &shipMissions[i].data
		if r.opts.MarquesEnabled && mission != nil && ship.Ship.Status != "sunk" {
			for _, plan := range marqueMissionPlans(state, ship, mission, data) {
				if plan.Origin == "" || plan.Destination == "" || plan.Origin == plan.Destination {
					continue
				}
				key := routeKey(plan.Origin, plan.Destination, ship.Ship.ShipClass)
				if !requiredRoutes[key] {
					requiredRoutes[key] = true
					routeKeys = append(routeKeys, key)
				}
				if seenRoutes[key] {
					continue
				}
				seenRoutes[key] = true
				if len(routes[key]) == 0 || !r.fresh("routes/"+key) {
					path := "/v1/world/route?from=" + url.QueryEscape(plan.Origin) + "&to=" + url.QueryEscape(plan.Destination) + "&ship_class=" + url.QueryEscape(ship.Ship.ShipClass)
					childTasks = append(childTasks, fetchTask{key: "routes/" + key, path: path})
				}
			}
		}
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
				if !requiredRoutes[key] {
					requiredRoutes[key] = true
					routeKeys = append(routeKeys, key)
				}
				if seenRoutes[key] {
					continue
				}
				seenRoutes[key] = true
				if len(routes[key]) == 0 || !r.fresh("routes/"+key) {
					path := "/v1/world/route?from=" + url.QueryEscape(from) + "&to=" + url.QueryEscape(to) + "&ship_class=" + url.QueryEscape(ship.Ship.ShipClass)
					childTasks = append(childTasks, fetchTask{key: "routes/" + key, path: path})
				}
			}
		}
	}
	if err := r.captureWave(ctx, childTasks, groups); err != nil {
		return err
	}
	// Rejected exploratory pairs are optional too, even when a GET was sent
	// this pass. Never let the request-deduplication set bypass history caps.
	var historyKeys []string
	for key := range state.Routes {
		if !requiredRoutes[key] && r.fresh("routes/"+key) {
			historyKeys = append(historyKeys, key)
		}
	}
	sort.Slice(historyKeys, func(i, j int) bool {
		left, right := historyKeys[i], historyKeys[j]
		leftAt, rightAt := r.meta["routes/"+left].UpdatedAt, r.meta["routes/"+right].UpdatedAt
		if !leftAt.Equal(rightAt) {
			return leftAt.After(rightAt)
		}
		return left < right
	})
	type sourceCommodity struct {
		port, commodity string
	}
	type supplyHint struct {
		production bool
		ask        int64
		askAt      time.Time
	}
	var supply map[sourceCommodity]supplyHint
	var charterRewards map[string]map[string]int64
	var sourcePorts map[string]bool
	var hulls map[string]Hull
	retentionInput := r.planningInput(ShipState{})
	_, upper, validClock := retentionInput.serverBounds()
	if len(historyKeys) > 0 && validClock && state.Clock.Calendar.TimeScale > 0 {
		supply = make(map[sourceCommodity]supplyHint)
		charterRewards = make(map[string]map[string]int64)
		sourcePorts = make(map[string]bool)
		hulls = make(map[string]Hull)
		// Index each charter, catalog source and gossip price once. Retention
		// lookups must not multiply routes by charters by all public gossip.
		for _, contract := range state.Contracts {
			if err := ctx.Err(); err != nil {
				return err
			}
			if contract.ContractType != "charter" || contract.Status != "open" || contract.Stale ||
				contract.RemainingQuantity <= 0 || contract.RewardPerTonPence == nil || *contract.RewardPerTonPence <= 0 {
				continue
			}
			if charterRewards[contract.OriginPortID] == nil {
				charterRewards[contract.OriginPortID] = make(map[string]int64)
			}
			rewards := charterRewards[contract.OriginPortID]
			rewards[contract.CommodityID] = max(rewards[contract.CommodityID], *contract.RewardPerTonPence)
		}
		for _, hull := range state.Catalog.ShipClasses {
			if fleetClasses[hull.ID] {
				hulls[hull.ID] = hull
			}
		}
		for _, port := range state.Catalog.Ports {
			if err := ctx.Err(); err != nil {
				return err
			}
			if port.Phase > state.Clock.UnlockedPhase {
				continue
			}
			sourcePorts[port.ID] = true
			for _, commodities := range [][]string{port.Produces, port.Manufactures} {
				for _, commodity := range commodities {
					supply[sourceCommodity{port.ID, commodity}] = supplyHint{production: true}
				}
			}
		}
		for portID, detail := range state.Ports {
			if !retentionInput.fresh("ports/" + portID) {
				continue
			}
			for _, report := range detail.CoffeeHouseGossip {
				if err := ctx.Err(); err != nil {
					return err
				}
				age := upper.Sub(report.SnapshotAt)
				if !sourcePorts[report.SubjectPortID] || report.SnapshotAt.IsZero() || age < 0 ||
					age.Seconds()*float64(state.Clock.Calendar.TimeScale) > 6*3_600 {
					continue
				}
				for _, price := range report.Commodities {
					age := upper.Sub(price.SnapshotAt)
					if price.SnapshotAt.IsZero() || age < 0 ||
						age.Seconds()*float64(state.Clock.Calendar.TimeScale) > 6*3_600 ||
						price.BestAskPence == nil || *price.BestAskPence <= 0 {
						continue
					}
					key := sourceCommodity{report.SubjectPortID, price.CommodityID}
					hint := supply[key]
					if hint.ask == 0 || price.SnapshotAt.After(hint.askAt) ||
						price.SnapshotAt.Equal(hint.askAt) && *price.BestAskPence > hint.ask {
						hint.ask, hint.askAt = *price.BestAskPence, price.SnapshotAt
						supply[key] = hint
					}
				}
			}
		}
	}
	var retainedRouteKeys []string
	retainedBytes := 0
	for checked, key := range historyKeys {
		if checked >= SourcingRouteRetentionScanLimit || len(retainedRouteKeys) >= SourcingRouteRetentionLimit {
			break
		}
		if err := ctx.Err(); err != nil {
			return err
		}
		route := state.Routes[key]
		from, rest, hasFrom := strings.Cut(key, "|")
		to, class, hasClass := strings.Cut(rest, "|")
		hull, exists := hulls[class]
		if !hasFrom || !hasClass || !exists ||
			route.FromPort != from || route.ToPort != to || route.ShipClass != class {
			continue
		}
		if !exploredRoutes[key] {
			rejection := ValidateRoute(route, hull, state.Catalog.SeaZones)
			if rejection == nil {
				continue
			}
			reason := rejection.Error()
			if !strings.Contains(reason, " is below route edge ") &&
				!strings.Contains(reason, " is below zone ") && reason != "Sound toll has no observed route cost" {
				continue
			}
			supplied := false
			for _, pair := range [][2]string{{from, to}, {to, from}} {
				for commodity, reward := range charterRewards[pair[1]] {
					hint := supply[sourceCommodity{pair[0], commodity}]
					if hint.ask > 0 && hint.ask < reward || hint.ask == 0 && hint.production {
						supplied = true
						break
					}
				}
				if supplied {
					break
				}
			}
			if !supplied {
				continue
			}
		}
		// Include both route and metadata map entries, with JSON escaping.
		size := len(encode(routes[key])) + len(encode(r.meta["routes/"+key])) +
			len(encode(key)) + len(encode("routes/"+key)) + 4
		if size > SourcingRouteRetentionBytes-retainedBytes {
			continue
		}
		routeKeys = append(routeKeys, key)
		retainedRouteKeys = append(retainedRouteKeys, key)
		retainedBytes += size
	}
	r.finishGroup("routes", routes, routeKeys)
	if err := ctx.Err(); err != nil {
		return err
	}
	r.stateCache = r.state()
	if err := r.recordUnderwriting(ctx); err != nil {
		return err
	}
	if err := r.recordMarques(ctx); err != nil {
		return err
	}
	r.refreshCompetition(ctx)
	if scale := r.stateCache.Clock.Calendar.TimeScale; scale > 0 {
		window := CharterProceedsWindow(scale)
		proceeds, err := r.db.CharterProceedsSince(ctx, r.account.ID, time.Now().Add(-window))
		if err != nil {
			return err
		}
		r.charterProceedsPence = proceeds
	}
	delete(r.snapshot, "_competition")
	delete(r.snapshot, "_charter_proceeds")
	for _, keys := range [][]string{replayPassRunnerKeys, replaySweepRunnerKeys} {
		for _, key := range keys {
			delete(r.meta, key)
		}
	}
	if err := r.refreshPassengers(ctx); err != nil {
		return err
	}
	r.snapshot["_meta"] = encode(r.meta)
	// Redaction can expand short sensitive values, so use the store's exact
	// persisted representation and remeasure after every eviction batch.
	if len(retainedRouteKeys) > 0 {
		for {
			snapshotBytes, err := r.db.RedactedSnapshotSize(ctx, r.account.ID, r.snapshot)
			if err != nil {
				return err
			}
			rawBytes := 0
			for key, value := range r.snapshot {
				rawBytes += len(key) + len(value)
			}
			snapshotBytes = max(snapshotBytes, rawBytes)
			excess := snapshotBytes - (store.MaxSnapshotBytes - snapshotSafetyMargin)
			if excess <= 0 || len(retainedRouteKeys) == 0 {
				break
			}
			removed := 0
			for removed < excess && len(retainedRouteKeys) > 0 {
				key := retainedRouteKeys[len(retainedRouteKeys)-1]
				retainedRouteKeys = retainedRouteKeys[:len(retainedRouteKeys)-1]
				removed += len(encode(routes[key])) + len(encode(key)) +
					len(encode(r.meta["routes/"+key])) + len(encode("routes/"+key)) + 2
				if len(routes) > 1 {
					removed++
				}
				if len(r.meta) > 1 {
					removed++
				}
				delete(routes, key)
				delete(r.meta, "routes/"+key)
			}
			r.snapshot["routes"] = encode(routes)
			r.snapshot["_meta"] = encode(r.meta)
		}
	}
	if err := r.db.SaveSnapshot(ctx, r.account.ID, r.snapshot); err != nil {
		return err
	}
	r.stateCache = r.state()
	r.stateCache.Markets = withoutOwnOrders(r.stateCache.Markets, r.ownedOrders())
	return r.recordBalances(ctx)
}

func (r *Runner) state() AccountState {
	return StateFromSnapshot(time.Now(), r.snapshot, PlanningSources{Metadata: r.meta, Generation: r.generation})
}

// StateFromSnapshot decodes a retained frame without network or database access.
func StateFromSnapshot(now time.Time, snapshot map[string]json.RawMessage, sources PlanningSources) AccountState {
	state := AccountState{
		Catalog:           decode[Catalog](snapshot, "catalog"),
		Clock:             decode[Clock](snapshot, "clock"),
		Ships:             decode[[]ShipState](snapshot, "ships"),
		Markets:           decode[map[string]Market](snapshot, "books"),
		Ports:             decode[map[string]PortDetail](snapshot, "ports"),
		Routes:            decode[map[string]Route](snapshot, "routes"),
		HazardQuotes:      decode[map[string]HazardQuote](snapshot, hazardQuoteSnapshotKey),
		Contracts:         decode[[]Contract](snapshot, "contracts"),
		Loans:             decode[[]gameapi.Loan](snapshot, "loans"),
		InsurancePolicies: decode[[]InsurancePolicy](snapshot, "insurance"),
		Rates:             decode[[]Rate](snapshot, "rates"),
		Factors:           decode[[]string](snapshot, "factors"),
		Quotes:            decode[map[string]YardQuote](snapshot, "yard_quotes"),
	}
	var vaults struct {
		Vaults []Vault `json:"vaults"`
	}
	_ = json.Unmarshal(snapshot["vaults"], &vaults)
	state.Vaults = vaults.Vaults
	var profile struct {
		Company struct {
			HomePortID string `json:"home_port_id"`
		} `json:"company"`
	}
	_ = json.Unmarshal(snapshot["profile"], &profile)
	state.HomePortID = profile.Company.HomePortID
	state.Markets = monopolyMarketView(now, snapshot, sources, state.Markets, state.Catalog, state.Clock)
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
