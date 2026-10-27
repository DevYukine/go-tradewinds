package sim

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/url"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/DevYukine/go-tradewinds/internal/engine"
	"github.com/DevYukine/go-tradewinds/internal/store"
)

type BackfillPathCount struct {
	Mapped   int64
	Unmapped int64
}

type BackfillClockGap struct {
	From time.Time
	To   time.Time
}

type BackfillReport struct {
	FramesAdded           int64
	PayloadsAdded         int64
	CompleteBookHours     map[string]float64
	TruncatedOnlyPorts    []string
	ClockGaps             []BackfillClockGap
	Paths                 map[string]BackfillPathCount
	PartialContractPasses []string
	IncompleteLoanPasses  []string
	DurationGaps          int64
	OversizeDatasets      int64
}

func (r BackfillReport) WriteTo(writer io.Writer) (int64, error) {
	var text strings.Builder
	fmt.Fprintf(&text, "Frames added: %d\nPayloads added: %d\n", r.FramesAdded, r.PayloadsAdded)
	fmt.Fprintf(&text, "Oversize dataset gaps: %d\n", r.OversizeDatasets)
	ports := make([]string, 0, len(r.CompleteBookHours))
	for port := range r.CompleteBookHours {
		ports = append(ports, port)
	}
	sort.Strings(ports)
	for _, port := range ports {
		fmt.Fprintf(&text, "Complete-book hours %q: %.4f\n", port, r.CompleteBookHours[port])
	}
	quotedPorts := make([]string, len(r.TruncatedOnlyPorts))
	for i, port := range r.TruncatedOnlyPorts {
		quotedPorts[i] = strconv.Quote(port)
	}
	fmt.Fprintf(&text, "Truncated-only ports: %s\n", strings.Join(quotedPorts, ", "))
	fmt.Fprintf(&text, "Diagnostic duration gaps (requests over 46 seconds, including client-timeout slack): %d\n", r.DurationGaps)
	fmt.Fprintf(&text, "Clock gaps over 2 minutes: %d\n", len(r.ClockGaps))
	for _, gap := range r.ClockGaps {
		fmt.Fprintf(&text, "  %s to %s (%s)\n", gap.From.Format(time.RFC3339Nano), gap.To.Format(time.RFC3339Nano), gap.To.Sub(gap.From))
	}
	families := make([]string, 0, len(r.Paths))
	for family := range r.Paths {
		families = append(families, family)
	}
	sort.Strings(families)
	for _, family := range families {
		count := r.Paths[family]
		fmt.Fprintf(&text, "Path %q mapped=%d unmapped=%d\n", family, count.Mapped, count.Unmapped)
	}
	fmt.Fprintf(&text, "Partial contract history passes: %d\n", len(r.PartialContractPasses))
	for _, generation := range r.PartialContractPasses {
		fmt.Fprintf(&text, "  %q\n", generation)
	}
	fmt.Fprintf(&text, "Incomplete borrower loan passes: %d\n", len(r.IncompleteLoanPasses))
	for _, generation := range r.IncompleteLoanPasses {
		fmt.Fprintf(&text, "  %q\n", generation)
	}
	n, err := io.WriteString(writer, text.String())
	return int64(n), err
}

type backfillMapping struct {
	key     string
	variant string
	cursor  string
	paged   bool
	limit   int
}

func mapDiagnostic(path string) (backfillMapping, string) {
	endpoint, err := url.Parse(path)
	if err != nil {
		return backfillMapping{}, "invalid URL"
	}
	query := endpoint.Query()
	mapped := backfillMapping{limit: 200, cursor: query.Get("cursor")}
	if limit, err := strconv.Atoi(query.Get("limit")); err == nil && limit > 0 {
		mapped.limit = limit
	}
	switch endpoint.Path {
	case "/v1/world/clock":
		mapped.key = "clock"
	case "/v1/world/zones":
		mapped.key = "zones"
	case "/v1/company/profile":
		mapped.key = "profile"
	case "/v1/company/settings":
		mapped.key = "settings"
	case "/v1/treasury/vaults":
		mapped.key = "vaults"
	case "/v1/treasury/rates":
		mapped.key = "rates"
	case "/v1/fleet/ships":
		mapped.key, mapped.paged = "ships", true
	case "/v1/fleet/ship":
		if id := query.Get("ship_id"); id != "" {
			mapped.key = "ship_details/" + id
		}
	case "/v1/warehouse/list":
		mapped.key = "warehouses"
	case "/v1/warehouse/inventory":
		if id := query.Get("port_id"); id != "" {
			mapped.key = "inventory/" + id
		}
	case "/v1/market/orders":
		mapped.key, mapped.paged = "orders", true
	case "/v1/market/fills":
		mapped.key, mapped.paged = "fills", true
	case "/v1/company/factors":
		mapped.key = "factors"
	case "/v1/world/monopolies":
		mapped.key, mapped.paged = "monopolies", true
	case "/v1/fleet/marques":
		mapped.key, mapped.paged = "marques", true
	case "/v1/world/expansion":
		mapped.key = "expansion"
	case "/v1/loan/list":
		if query.Get("role") == "borrower" || query.Get("role") == "" {
			mapped.key, mapped.paged, mapped.variant = "loans", true, query.Get("status")
		}
	case "/v1/contract/list":
		switch query.Get("status") {
		case "open", "active", "completed", "cancelled", "defaulted":
			mapped.key, mapped.paged, mapped.variant = "contracts", true, query.Get("status")
		}
	case "/v1/market/book":
		if port := query.Get("port_id"); port != "" {
			mapped.key = "books/" + port
		}
	case "/v1/world/port":
		if port := query.Get("port_id"); port != "" {
			mapped.key = "ports/" + port
		}
	case "/v1/shipyard/quote":
		if port, class := query.Get("port_id"), query.Get("ship_class"); port != "" && class != "" {
			mapped.key = "yard_quotes/" + port + "|" + class
		}
	case "/v1/world/route":
		from, to := query.Get("from"), query.Get("to")
		if from == "" {
			from = query.Get("from_port_id")
		}
		if to == "" {
			to = query.Get("to_port_id")
		}
		if from != "" && to != "" {
			mapped.key = "routes/" + from + "|" + to + "|" + query.Get("ship_class")
		}
	}
	return mapped, endpoint.Path
}

func firstWave(path string) bool {
	endpoint, err := url.Parse(path)
	if err != nil {
		return false
	}
	switch endpoint.Path {
	case "/v1/auth/whoami", "/v1/player/profile", "/v1/company/profile", "/v1/company/settings", "/v1/world/catalog", "/v1/world/clock":
		return true
	}
	return false
}

func diagnosticComplete(row store.ReplayDiagnostic) bool {
	if row.Status < 200 || row.Status >= 300 || row.Error != "" || !json.Valid(row.Response) || string(row.Response) == "null" {
		return false
	}
	var envelope struct {
		Truncated bool `json:"truncated"`
	}
	_ = json.Unmarshal(row.Response, &envelope)
	return !envelope.Truncated
}

func diagnosticMeta(row store.ReplayDiagnostic, generation string, complete bool) json.RawMessage {
	meta := engine.DatasetMeta{Generation: generation, AttemptedAt: row.StartedAt, Complete: complete}
	if complete {
		meta.UpdatedAt = row.CreatedAt
		var upstream struct {
			AsOf *time.Time `json:"as_of"`
		}
		_ = json.Unmarshal(row.Response, &upstream)
		meta.UpstreamAsOf = upstream.AsOf
	} else {
		meta.Stale = true
		meta.Error = fmt.Sprintf("diagnostic HTTP %d; incomplete or truncated response", row.Status)
		if row.Error != "" {
			meta.Error += ": " + row.Error
		}
		var failure struct {
			Code string `json:"code"`
		}
		_ = json.Unmarshal(row.Response, &failure)
		if failure.Code != "" {
			meta.Error += ": " + failure.Code
		}
	}
	data, _ := json.Marshal(meta)
	return data
}

type diagnosticPages struct {
	mapping backfillMapping
	pages   map[string]store.ReplayDiagnostic
}

func diagnosticPageID(row json.RawMessage) string {
	var item struct {
		ID   string `json:"id"`
		Ship struct {
			ID string `json:"id"`
		} `json:"ship"`
		Convoy struct {
			ID string `json:"id"`
		} `json:"convoy"`
	}
	if json.Unmarshal(row, &item) != nil {
		return ""
	}
	if item.ID != "" {
		return item.ID
	}
	if item.Ship.ID != "" {
		return item.Ship.ID
	}
	return item.Convoy.ID
}

func (p *diagnosticPages) combine() (store.ReplayDiagnostic, bool) {
	first, exists := p.pages[""]
	if !exists {
		return first, false
	}
	combined := make([]json.RawMessage, 0)
	seen := make(map[string]bool)
	cursor := ""
	for {
		row, exists := p.pages[cursor]
		if !exists {
			return first, false
		}
		var page []json.RawMessage
		if json.Unmarshal(row.Response, &page) != nil || page == nil {
			return first, false
		}
		for _, item := range page {
			id := diagnosticPageID(item)
			if id == "" || seen[id] {
				return first, false
			}
			seen[id] = true
			combined = append(combined, item)
		}
		if row.CreatedAt.After(first.CreatedAt) {
			first.CreatedAt = row.CreatedAt
		}
		if len(page) < p.mapping.limit {
			if cursor != "" {
				first.Response, _ = json.Marshal(combined)
			}
			return first, true
		}
		cursor = diagnosticPageID(page[len(page)-1])
	}
}

func mergeDiagnosticLists(rows []store.ReplayDiagnostic) store.ReplayDiagnostic {
	combined := make([]json.RawMessage, 0)
	positions := make(map[string]int)
	var result store.ReplayDiagnostic
	for _, row := range rows {
		var page []json.RawMessage
		_ = json.Unmarshal(row.Response, &page)
		if result.StartedAt.IsZero() || row.StartedAt.Before(result.StartedAt) {
			result.StartedAt = row.StartedAt
		}
		if row.CreatedAt.After(result.CreatedAt) {
			result.CreatedAt = row.CreatedAt
		}
		for _, item := range page {
			id := diagnosticPageID(item)
			if position, exists := positions[id]; exists {
				combined[position] = item
			} else {
				positions[id] = len(combined)
				combined = append(combined, item)
			}
		}
	}
	result.Response, _ = json.Marshal(combined)
	return result
}

func Backfill(ctx context.Context, db *store.Store, accountID string) (BackfillReport, error) {
	report := BackfillReport{CompleteBookHours: make(map[string]float64), Paths: make(map[string]BackfillPathCount)}
	boundary, err := db.ReplayBackfillBoundary(ctx, accountID)
	if err != nil || boundary.MaxRequestID == 0 {
		return report, err
	}
	report.DurationGaps = boundary.DurationGaps
	snapshot, err := db.Snapshot(ctx, accountID)
	if err != nil {
		return report, err
	}
	routes, err := db.ReplayDiagnosticRoutes(ctx, accountID, boundary)
	if err != nil {
		return report, err
	}
	type routeObservation struct {
		index int
		at    time.Time
	}
	var routeObservations []routeObservation
	for index, row := range routes {
		for _, seenAt := range row.SeenAt {
			routeObservations = append(routeObservations, routeObservation{index: index, at: seenAt})
		}
	}
	sort.Slice(routeObservations, func(i, j int) bool {
		left, right := routeObservations[i], routeObservations[j]
		if !left.at.Equal(right.at) {
			return left.at.Before(right.at)
		}
		return routes[left.index].ID < routes[right.index].ID
	})
	countedRoutes := make([]bool, len(routes))
	var routeIndex, eventIndex int
	var eventPage []store.ReplayEvent
	var eventAfter time.Time
	var eventAfterID int64
	var eventsDone bool
	latestRoutes := make(map[string]store.ReplayDiagnostic)
	portBins := make(map[string]int64)
	truncated := make(map[string]bool)
	lastBook := make(map[string]time.Time)
	var lastFrame, lastSweep, previousClock time.Time
	var pendingEvents []store.ReplayEvent
	var pendingEventBytes int
	var droppedEvents, droppedEventBytes int64
	var pass []store.ReplayDiagnostic
	var clock store.ReplayDiagnostic
	countPath := func(row store.ReplayDiagnostic, mapped bool) {
		_, family := mapDiagnostic(row.Path)
		count := report.Paths[family]
		n := row.Count
		if n == 0 {
			n = 1
		}
		if mapped {
			count.Mapped += n
		} else {
			count.Unmapped += n
		}
		report.Paths[family] = count
	}
	consumeEvents := func(start, end time.Time) error {
		for {
			if eventIndex == len(eventPage) {
				if eventsDone {
					return nil
				}
				var err error
				eventPage, err = db.ReplayDiagnosticEventPage(ctx, accountID, boundary.At, eventAfter, eventAfterID)
				if err != nil {
					return err
				}
				eventIndex = 0
				eventsDone = len(eventPage) < store.MaxReplayEventCount
				if len(eventPage) == 0 {
					return nil
				}
				last := eventPage[len(eventPage)-1]
				eventAfter, eventAfterID = last.CreatedAt, last.ID
			}
			event := eventPage[eventIndex]
			if !event.CreatedAt.Before(end) {
				return nil
			}
			// Retained envelopes stay inside the pending buffer budget. Clear
			// every consumed page entry so rejected payloads can be reclaimed.
			eventPage[eventIndex] = store.ReplayEvent{}
			eventIndex++
			if event.CreatedAt.Before(start) {
				continue
			}
			size := event.PayloadBytes + int64(len(event.EventID)+len(event.Type)+64)
			if event.Oversize || len(pendingEvents) >= store.MaxReplayEventCount || size > int64(store.MaxReplayEventBytes-pendingEventBytes) {
				droppedEvents++
				droppedEventBytes += size
			} else {
				pendingEvents = append(pendingEvents, event)
				pendingEventBytes += int(size)
			}
		}
	}
	writePass := func(rows []store.ReplayDiagnostic, clock store.ReplayDiagnostic, start, end time.Time, final bool) error {
		if clock.ID == 0 {
			return nil
		}
		generation := fmt.Sprintf("diagnostic:%d", clock.ID)
		if !previousClock.IsZero() && start.Sub(previousClock) > 2*time.Minute {
			report.ClockGaps = append(report.ClockGaps, BackfillClockGap{From: previousClock, To: start})
		}
		previousClock = start
		values := make(map[string]store.ReplayDiagnostic)
		gaps := make(map[string]store.ReplayDiagnostic)
		pages := make(map[string]*diagnosticPages)
		decision := false
		for _, row := range rows {
			if row.Method == "POST" || row.Method == "PATCH" {
				decision = true
				continue
			}
			mapped, _ := mapDiagnostic(row.Path)
			countPath(row, mapped.key != "")
			if mapped.key == "" {
				continue
			}
			if mapped.key == "loans" {
				gaps["loans"] = row
			}
			if !diagnosticComplete(row) {
				if strings.HasPrefix(mapped.key, "books/") || mapped.key == "clock" {
					gaps[mapped.key] = row
				}
				if strings.HasPrefix(mapped.key, "books/") && row.Status >= 200 && row.Status < 300 {
					truncated[strings.TrimPrefix(mapped.key, "books/")] = true
				}
				continue
			}
			if mapped.paged {
				identity := mapped.key + "\x00" + mapped.variant
				if pages[identity] == nil {
					pages[identity] = &diagnosticPages{mapping: mapped, pages: make(map[string]store.ReplayDiagnostic)}
				}
				pages[identity].pages[mapped.cursor] = row
			} else {
				values[mapped.key] = row
			}
		}
		lists := make(map[string]store.ReplayDiagnostic)
		for identity, group := range pages {
			if combined, complete := group.combine(); complete {
				lists[identity] = combined
				if group.mapping.key != "contracts" && group.mapping.key != "loans" {
					values[group.mapping.key] = combined
				}
			}
		}
		var contracts []store.ReplayDiagnostic
		for _, status := range []string{"open", "active", "completed", "cancelled", "defaulted"} {
			if row, exists := lists["contracts\x00"+status]; exists {
				contracts = append(contracts, row)
			}
		}
		if len(contracts) != 0 {
			values["contracts"] = mergeDiagnosticLists(contracts)
			if len(contracts) != 5 {
				report.PartialContractPasses = append(report.PartialContractPasses, generation)
			}
		}
		active, activeComplete := lists["loans\x00active"]
		defaulted, defaultedComplete := lists["loans\x00defaulted"]
		if activeComplete && defaultedComplete {
			values["loans"] = mergeDiagnosticLists([]store.ReplayDiagnostic{active, defaulted})
		} else if history, complete := lists["loans\x00"]; complete {
			values["loans"] = history
		} else if gap, attempted := gaps["loans"]; attempted {
			reason := "borrower loans require complete active and defaulted pages or complete unfiltered history"
			if gap.Error != "" {
				reason += ": " + gap.Error
			}
			gap.Error = reason
			gaps["loans"] = gap
			report.IncompleteLoanPasses = append(report.IncompleteLoanPasses, generation)
		}
		newRoutes := make(map[string]store.ReplayDiagnostic)
		for routeIndex < len(routeObservations) && routeObservations[routeIndex].at.Before(end) {
			observation := routeObservations[routeIndex]
			row := routes[observation.index]
			routeIndex++
			mapped, _ := mapDiagnostic(row.Path)
			if !countedRoutes[observation.index] {
				countPath(row, mapped.key != "")
				countedRoutes[observation.index] = true
			}
			if mapped.key != "" && diagnosticComplete(row) {
				previous, exists := latestRoutes[mapped.key]
				row.LastSeenAt = observation.at
				latestRoutes[mapped.key] = row
				if !exists || previous.ID != row.ID {
					newRoutes[mapped.key] = row
				}
			}
		}
		for key, row := range latestRoutes {
			if row.LastSeenAt.Before(start.Add(-engine.RouteGeometryTTL)) {
				delete(latestRoutes, key)
				delete(newRoutes, key)
			}
		}
		if err := consumeEvents(start, end); err != nil {
			return err
		}
		for key, row := range values {
			if strings.HasPrefix(key, "books/") {
				port := strings.TrimPrefix(key, "books/")
				if previous := lastBook[port]; !previous.IsZero() && row.StartedAt.Sub(previous) <= 2*time.Minute {
					report.CompleteBookHours[port] += row.StartedAt.Sub(previous).Hours()
				} else {
					report.CompleteBookHours[port] += 0
				}
				lastBook[port] = row.StartedAt
			}
		}
		force := lastFrame.IsZero() || decision || len(newRoutes) != 0 || final && (len(pendingEvents) != 0 || droppedEvents != 0)
		if !force && start.Sub(lastFrame) < time.Minute {
			return nil
		}
		sweep := lastSweep.IsZero() || decision || start.Sub(lastSweep) >= 5*time.Minute
		frame := store.ReplayFrame{AccountID: accountID, Generation: generation, Source: "diagnostic_backfill",
			ObservedAt: start, Decision: decision, AssumedCatalog: true, Datasets: make(map[string]store.ReplayDataset)}
		for key, row := range values {
			if strings.HasPrefix(key, "ports/") {
				var port struct {
					Visibility string `json:"visibility"`
				}
				_ = json.Unmarshal(row.Response, &port)
				bin := row.StartedAt.Unix() / 300
				if previous, exists := portBins[key]; port.Visibility != "quay" && exists && previous == bin {
					continue
				}
				portBins[key] = bin
			}
			frame.Datasets[key] = store.ReplayDataset{Data: row.Response, Meta: diagnosticMeta(row, generation, true)}
		}
		for key, row := range gaps {
			if _, complete := values[key]; !complete {
				frame.Datasets[key] = store.ReplayDataset{Meta: diagnosticMeta(row, generation, false)}
			}
		}
		if sweep {
			keys := make([]string, 0, len(latestRoutes))
			for key := range latestRoutes {
				keys = append(keys, key)
			}
			sort.Slice(keys, func(i, j int) bool {
				left, right := latestRoutes[keys[i]].LastSeenAt, latestRoutes[keys[j]].LastSeenAt
				if !left.Equal(right) {
					return left.After(right)
				}
				return keys[i] < keys[j]
			})
			retained, retainedBytes := 0, 0
			for checked, key := range keys {
				if checked >= engine.SourcingRouteRetentionScanLimit || retained >= engine.SourcingRouteRetentionLimit {
					break
				}
				row := latestRoutes[key]
				meta := diagnosticMeta(row, generation, true)
				routeJSON, err := json.Marshal(row.Response)
				if err != nil {
					return err
				}
				shortKey, _ := json.Marshal(strings.TrimPrefix(key, "routes/"))
				fullKey, _ := json.Marshal(key)
				// Match the live route and metadata map-entry accounting,
				// including JSON escaping and both map keys.
				size := len(routeJSON) + len(meta) + len(shortKey) + len(fullKey) + 4
				if size > engine.SourcingRouteRetentionBytes-retainedBytes {
					continue
				}
				frame.Datasets[key] = store.ReplayDataset{Data: row.Response, Meta: meta}
				retained++
				retainedBytes += size
			}
			lastSweep = start
		} else {
			for key, row := range newRoutes {
				frame.Datasets[key] = store.ReplayDataset{Data: row.Response, Meta: diagnosticMeta(row, generation, true)}
			}
		}
		if catalog := snapshot["catalog"]; len(catalog) != 0 {
			meta, _ := json.Marshal(engine.DatasetMeta{Generation: generation, AttemptedAt: start, UpdatedAt: start, Complete: true})
			frame.Datasets["catalog"] = store.ReplayDataset{Data: catalog, Meta: meta}
		}
		if len(pendingEvents) != 0 || droppedEvents != 0 {
			if pendingEvents == nil {
				pendingEvents = make([]store.ReplayEvent, 0)
			}
			payload, err := json.Marshal(pendingEvents)
			if err != nil {
				return err
			}
			meta := engine.DatasetMeta{Generation: generation, AttemptedAt: start, UpdatedAt: end, Complete: droppedEvents == 0}
			if droppedEvents != 0 {
				meta.Stale = true
				meta.Error = "replay event buffer overflow"
			}
			encodedMeta, _ := json.Marshal(struct {
				engine.DatasetMeta
				DroppedEvents int64 `json:"dropped_events,omitempty"`
				DroppedBytes  int64 `json:"dropped_bytes,omitempty"`
			}{meta, droppedEvents, droppedEventBytes})
			frame.Datasets["events"] = store.ReplayDataset{Data: payload, Meta: encodedMeta}
		}
		if captured, exists := values["clock"]; exists {
			frame.Clock = captured.Response
			var observed struct {
				UnlockedPhase int `json:"unlocked_phase"`
			}
			_ = json.Unmarshal(captured.Response, &observed)
			frame.Phase = observed.UnlockedPhase
		}
		written, err := db.RecordReplayBackfillFrame(ctx, frame)
		if err != nil {
			return err
		}
		report.OversizeDatasets += written.DemotedDatasets
		if written.ID != 0 {
			report.FramesAdded++
			report.PayloadsAdded += written.PayloadsAdded
		}
		pendingEvents = nil
		pendingEventBytes, droppedEvents, droppedEventBytes = 0, 0, 0
		lastFrame = start
		return nil
	}
	var start time.Time
	err = db.ScanReplayDiagnostics(ctx, accountID, boundary, func(row store.ReplayDiagnostic) error {
		mapped, _ := mapDiagnostic(row.Path)
		if row.Method != "GET" || mapped.key != "clock" {
			pass = append(pass, row)
			return nil
		}
		// Anchor within the current first wave. With no later-wave completion,
		// a staggered whoami must start after the previous clock completed, or
		// belong to the first retained pass. The previous clock's overlapping
		// whoami must not pull its profile into this pass.
		var lastCompleted time.Time
		for _, candidate := range pass {
			if (candidate.Method != "GET" || !firstWave(candidate.Path)) && candidate.CreatedAt.After(lastCompleted) {
				lastCompleted = candidate.CreatedAt
			}
		}
		clusterStart := row.StartedAt
		for i := len(pass) - 1; i >= 0; i-- {
			candidate := pass[i]
			endpoint, _ := url.Parse(candidate.Path)
			if candidate.ID == clock.ID {
				break
			}
			if candidate.Method == "GET" && endpoint != nil && endpoint.Path == "/v1/auth/whoami" &&
				(!candidate.CreatedAt.Before(row.StartedAt.Add(-time.Millisecond)) ||
					!lastCompleted.IsZero() && candidate.StartedAt.After(lastCompleted) ||
					lastCompleted.IsZero() && (clock.ID == 0 || candidate.StartedAt.After(clock.CreatedAt))) {
				clusterStart = candidate.StartedAt
				break
			}
		}
		for i := len(pass) - 1; i >= 0; i-- {
			candidate := pass[i]
			if candidate.ID == clock.ID {
				break
			}
			if candidate.Method == "GET" && firstWave(candidate.Path) &&
				!candidate.CreatedAt.Before(clusterStart.Add(-time.Millisecond)) && candidate.StartedAt.Before(clusterStart) {
				clusterStart = candidate.StartedAt
			}
		}
		split := sort.Search(len(pass), func(i int) bool { return !pass[i].StartedAt.Before(clusterStart) })
		if clock.ID != 0 {
			if err := writePass(pass[:split], clock, start, clusterStart, false); err != nil {
				return err
			}
		}
		pass = append([]store.ReplayDiagnostic(nil), pass[split:]...)
		pass = append(pass, row)
		clock, start = row, clusterStart
		return nil
	})
	if err != nil {
		return report, err
	}
	if err := writePass(pass, clock, start, boundary.At, true); err != nil {
		return report, err
	}
	for port := range truncated {
		if _, complete := report.CompleteBookHours[port]; !complete {
			report.TruncatedOnlyPorts = append(report.TruncatedOnlyPorts, port)
		}
	}
	sort.Strings(report.TruncatedOnlyPorts)
	return report, nil
}
