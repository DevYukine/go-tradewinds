package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"math"
	"sort"
	"strings"
	"time"

	"github.com/DevYukine/go-tradewinds/internal/gameapi"
	"gorm.io/gorm"
)

const (
	ControllerStalledAfter = 10 * time.Minute
	RefreshFailingAfter    = 10 * time.Minute
	WarningFreshFor        = 2 * time.Minute
	FleetIdleAfter         = 30 * time.Minute
	HoldStuckAfter         = 90 * time.Minute
	LossStreakCount        = 3
	WarningSettlementLimit = 10
	GrowthFreshFor         = 2 * time.Hour
	FallingBehindRivals    = 3
	WarningArrivalLookback = 7 * 24 * time.Hour
	WarningArrivalLimit    = 10_000
)

const (
	WarningCredentialsUnavailable = "credentials_unavailable"
	WarningSnapshotInvalid        = "snapshot_invalid"
	WarningSnapshotTooLarge       = "snapshot_too_large"
)

var warningSettlementKinds = []string{"spot", "charter", "freight", "spot+freight", "charter+freight", "recovery", "aborted"}

type Warning struct {
	Code     string            `json:"code"`
	Severity string            `json:"severity"`
	Since    time.Time         `json:"since"`
	Facts    map[string]int64  `json:"facts"`
	Names    map[string]string `json:"names"`
	Reason   string            `json:"reason"`
}

type AccountWarnings struct {
	AccountID  string    `json:"account_id"`
	Label      string    `json:"label"`
	Available  bool      `json:"available"`
	ErrorCode  string    `json:"error_code"`
	Suppressed []string  `json:"suppressed"`
	Warnings   []Warning `json:"warnings"`
}

type UnmonitoredAccount struct {
	AccountID string `json:"account_id"`
	Reason    string `json:"reason"`
}

type WarningReport struct {
	Accounts    []AccountWarnings    `json:"accounts"`
	Unmonitored []UnmonitoredAccount `json:"unmonitored"`
}

// Suppressed codes were not assessed, so their absence is not a clear signal.
type WarningAssessment struct {
	Warnings   []Warning `json:"warnings"`
	Suppressed []string  `json:"suppressed"`
}

type WarningHold struct {
	ShipID string `json:"ship_id"`
	Reason string `json:"reason"`
}

type WarningEngine struct {
	ObservedAt time.Time     `json:"observed_at"`
	Mode       string        `json:"mode"`
	Holds      []WarningHold `json:"holds"`
}

type WarningDataset struct {
	UpdatedAt time.Time `json:"updated_at"`
	Complete  bool      `json:"complete"`
	Stale     bool      `json:"stale"`
	Error     string    `json:"error"`
}

type WarningShipEvents struct {
	ArrivedAt  time.Time
	DepartedAt time.Time
}

// WarningInput contains only recorded observations. AssessWarnings never reads
// the database, contacts the game, or changes controller state.
type WarningInput struct {
	Engine       *WarningEngine
	Metadata     map[string]WarningDataset
	Ships        []gameapi.ShipDetail
	ShipDetails  map[string]gameapi.ShipDetail
	ShipEvents   map[string]WarningShipEvents
	Clock        gameapi.Clock
	Catalog      gameapi.Catalog
	Settlements  []Settlement
	Growth       GrowthWindow
	Secrets      []string
	Rising       gameapi.RisingBoard
	RisingAt     *time.Time
	OwnCompanies []string
}

func AssessWarnings(input WarningInput, at time.Time) WarningAssessment {
	assessment := WarningAssessment{Warnings: make([]Warning, 0), Suppressed: make([]string, 0)}
	result := assessment.Warnings
	controllerCurrent := input.Engine != nil && !input.Engine.ObservedAt.IsZero() && !input.Engine.ObservedAt.After(at)
	if controllerCurrent && at.Sub(input.Engine.ObservedAt) >= ControllerStalledAfter {
		result = append(result, Warning{
			Code: "controller_stalled", Severity: "critical", Since: input.Engine.ObservedAt,
			Facts: map[string]int64{"minutes": int64(at.Sub(input.Engine.ObservedAt) / time.Minute)},
		})
		controllerCurrent = false
	}
	if input.Engine == nil || input.Engine.ObservedAt.IsZero() || input.Engine.ObservedAt.After(at) {
		assessment.Suppressed = append(assessment.Suppressed, "controller_stalled", "refresh_failing")
	} else if !controllerCurrent {
		assessment.Suppressed = append(assessment.Suppressed, "refresh_failing")
	}
	if controllerCurrent {
		var oldest time.Time
		var datasets []string
		firstError := ""
		incomplete := false
		for _, key := range []string{"ships", "vaults", "contracts", "clock"} {
			meta := input.Metadata[key]
			if meta.UpdatedAt.IsZero() || meta.UpdatedAt.After(at) {
				incomplete = true
				continue
			}
			if at.Sub(meta.UpdatedAt) < RefreshFailingAfter {
				continue
			}
			datasets = append(datasets, key)
			if oldest.IsZero() || meta.UpdatedAt.Before(oldest) {
				oldest = meta.UpdatedAt
			}
			if firstError == "" {
				firstError = meta.Error
			}
		}
		if len(datasets) > 0 {
			result = append(result, Warning{
				Code: "refresh_failing", Severity: "critical", Since: oldest,
				Facts: map[string]int64{"minutes": int64(at.Sub(oldest) / time.Minute)},
				Names: map[string]string{"datasets": strings.Join(datasets, ", ")}, Reason: firstError,
			})
		}
		if len(datasets) == 0 && incomplete {
			assessment.Suppressed = append(assessment.Suppressed, "refresh_failing")
		}
	}
	if input.Engine != nil && input.Engine.Mode != "observe" && !input.Clock.Paused &&
		warningDatasetCurrent(input.Metadata["ships"], at) && warningDatasetCurrent(input.Metadata["_engine"], at) &&
		!input.Engine.ObservedAt.IsZero() && !input.Engine.ObservedAt.After(at) && at.Sub(input.Engine.ObservedAt) < WarningFreshFor {
		warnings, assessed := assessFleetWarnings(input, at)
		result = append(result, warnings...)
		if !assessed {
			assessment.Suppressed = append(assessment.Suppressed, "fleet_idle", "hold_stuck")
		}
	} else {
		assessment.Suppressed = append(assessment.Suppressed, "fleet_idle", "hold_stuck")
	}
	if loss := assessLossStreak(input.Settlements, at); loss != nil {
		result = append(result, *loss)
	}
	if input.Rising.RankedPlayers >= 3 {
		board := input.Rising
		current := input.RisingAt != nil && !input.RisingAt.After(at) && at.Sub(*input.RisingAt) <= GrowthFreshFor &&
			board.You != nil && !board.You.WindowStartAt.IsZero() && !board.You.WindowStartAt.After(*input.RisingAt)
		own := make(map[string]bool, len(input.OwnCompanies)+1)
		for _, name := range input.OwnCompanies {
			own[name] = true
		}
		if board.You != nil {
			own[board.You.CompanyName] = true
		}
		values := make([]int64, 0, len(board.Entries))
		for _, entry := range board.Entries {
			if !own[entry.CompanyName] {
				values = append(values, entry.GrowthBPS)
			}
		}
		if !current || len(values) == 0 {
			assessment.Suppressed = append(assessment.Suppressed, "falling_behind")
		} else {
			sort.Slice(values, func(i, j int) bool { return values[i] < values[j] })
			median := values[len(values)/2]
			half := false
			if len(values)%2 == 0 {
				left := values[len(values)/2-1]
				half = (left^median)&1 != 0
				// Floor the mean without overflow; preserve the half-basis-point
				// remainder for the strict below-median comparison.
				median = (left & median) + ((left ^ median) >> 1)
			}
			ours := board.You.GrowthBPS
			halfBPS := int64(0)
			if half {
				halfBPS = 1
			}
			if ours < median || ours == median && half {
				result = append(result, Warning{
					Code: "falling_behind", Severity: "warning", Since: board.You.WindowStartAt,
					Facts: map[string]int64{"ours_growth_bps": ours, "median_growth_bps": median,
						"median_growth_half_bps": halfBPS, "ranked_players": board.RankedPlayers},
					Names:  map[string]string{"basis": "rising"},
					Reason: "Published rising-board growth is below the rival median.",
				})
			}
		}
	} else {
		growth := input.Growth
		growthCurrent := growth.WindowHours == 24 && growth.From != nil && growth.To != nil &&
			!growth.From.After(*growth.To) && !growth.To.After(at) && at.Sub(*growth.To) <= GrowthFreshFor &&
			growth.OursPence != nil && growth.RivalMedianPence != nil && growth.RivalsMeasured >= FallingBehindRivals
		if !growthCurrent {
			assessment.Suppressed = append(assessment.Suppressed, "falling_behind")
		} else if *growth.OursPence < 0 && *growth.OursPence < *growth.RivalMedianPence {
			result = append(result, Warning{
				Code: "falling_behind", Severity: "warning", Since: *growth.From,
				Facts: map[string]int64{"ours_pence": *growth.OursPence, "median_pence": *growth.RivalMedianPence, "rival_count": int64(growth.RivalsMeasured)},
				Names: map[string]string{"basis": "valuation_24h"},
			})
		}
	}
	credentials := displayCredentials{secrets: input.Secrets, available: true}
	for i := range result {
		if result[i].Facts == nil {
			result[i].Facts = make(map[string]int64)
		}
		if result[i].Names == nil {
			result[i].Names = make(map[string]string)
		}
		for key, value := range result[i].Names {
			result[i].Names[key] = displayText(value, credentials)
		}
		result[i].Reason = displayText(result[i].Reason, credentials)
	}
	sort.Slice(result, func(i, j int) bool {
		if result[i].Severity != result[j].Severity {
			return result[i].Severity == "critical"
		}
		if result[i].Code != result[j].Code {
			return result[i].Code < result[j].Code
		}
		return result[i].Names["ship_id"] < result[j].Names["ship_id"]
	})
	assessment.Warnings = result
	sort.Strings(assessment.Suppressed)
	return assessment
}

func warningDatasetCurrent(meta WarningDataset, at time.Time) bool {
	return meta.Complete && !meta.Stale && meta.Error == "" && !meta.UpdatedAt.IsZero() &&
		!meta.UpdatedAt.After(at) && at.Sub(meta.UpdatedAt) < WarningFreshFor
}

type warningIdleShip struct {
	ship   *gameapi.Ship
	since  time.Time
	source string
	hold   string
	held   bool
}

func warningShipDetail(input *WarningInput, index int, at time.Time) *gameapi.ShipDetail {
	ship := &input.Ships[index]
	if ship.Ship.BerthedAt == nil {
		detail, exists := input.ShipDetails[ship.Ship.ID]
		meta := input.Metadata["ship_details/"+ship.Ship.ID]
		if exists && detail.Ship.ID == ship.Ship.ID && warningDatasetCurrent(meta, at) && meta.UpdatedAt.After(input.Metadata["ships"].UpdatedAt) {
			return &detail
		}
	}
	return ship
}

func assessFleetWarnings(input WarningInput, at time.Time) ([]Warning, bool) {
	known, unknown, idle, stuck := int64(0), int64(0), int64(0), int64(0)
	var longest, longestHeld *warningIdleShip
	for i := range input.Ships {
		ship := warningShipDetail(&input, i, at)
		if ship.Ship.Status == "sunk" {
			continue
		}
		if ship.Voyage != nil || ship.Ship.Status == "sailing" {
			known++
			continue
		}
		if ship.Ship.Status != "berthed" && ship.Ship.Status != "anchored_roadstead" {
			unknown++
			continue
		}
		var since time.Time
		source := ""
		if ship.Ship.BerthedAt != nil && !ship.Ship.BerthedAt.IsZero() {
			since, source = *ship.Ship.BerthedAt, "berthed_at"
		}
		if ship.Ship.DockQueuedAt != nil && ship.Ship.DockQueuedAt.After(since) {
			since, source = *ship.Ship.DockQueuedAt, "dock_queued_at"
		}
		if since.IsZero() {
			event := input.ShipEvents[ship.Ship.ID]
			if !event.ArrivedAt.IsZero() && event.ArrivedAt.After(event.DepartedAt) && !event.ArrivedAt.After(at) {
				since, source = event.ArrivedAt, "arrival_event"
			}
		}
		if since.IsZero() || since.After(at) || ship.Ship.CurrentPortID == nil || *ship.Ship.CurrentPortID == "" {
			unknown++
			continue
		}
		if ship.Ship.TurnaroundUnlocksAt != nil && ship.Ship.TurnaroundUnlocksAt.After(since) {
			since = *ship.Ship.TurnaroundUnlocksAt
		}
		known++
		if at.Sub(since) < FleetIdleAfter {
			continue
		}
		idle++
		candidate := warningIdleShip{ship: &ship.Ship, since: since, source: source}
		for _, hold := range input.Engine.Holds {
			if hold.ShipID != "" && hold.ShipID == ship.Ship.ID {
				candidate.hold = hold.Reason
				candidate.held = true
				break
			}
		}
		if longest == nil || since.Before(longest.since) || since.Equal(longest.since) && ship.Ship.ID < longest.ship.ID {
			copy := candidate
			longest = &copy
		}
		if candidate.held && at.Sub(since) >= HoldStuckAfter {
			stuck++
			if longestHeld == nil || since.Before(longestHeld.since) || since.Equal(longestHeld.since) && ship.Ship.ID < longestHeld.ship.ID {
				copy := candidate
				longestHeld = &copy
			}
		}
	}
	names := func(subject *warningIdleShip) map[string]string {
		port := *subject.ship.CurrentPortID
		for _, item := range input.Catalog.Ports {
			if item.ID == port {
				port = item.Name
				break
			}
		}
		return map[string]string{"ship": subject.ship.Name, "ship_id": subject.ship.ID, "port": port, "since_source": subject.source}
	}
	result := make([]Warning, 0, 2)
	if longest != nil && known > 0 && idle*2 >= known {
		result = append(result, Warning{
			Code: "fleet_idle", Severity: "warning", Since: longest.since,
			Facts: map[string]int64{"idle": idle, "total": known, "unknown": unknown, "minutes": int64(at.Sub(longest.since) / time.Minute)},
			Names: names(longest),
		})
	}
	if longestHeld != nil {
		result = append(result, Warning{
			Code: "hold_stuck", Severity: "warning", Since: longestHeld.since,
			Facts: map[string]int64{"minutes": int64(at.Sub(longestHeld.since) / time.Minute), "other_count": stuck - 1},
			Names: names(longestHeld), Reason: longestHeld.hold,
		})
	}
	return result, known > 0 || unknown == 0
}

func assessLossStreak(settlements []Settlement, at time.Time) *Warning {
	indices := make([]int, 0, len(settlements))
	for i := range settlements {
		row := &settlements[i]
		if !row.CostsKnown || row.CompletedAt.IsZero() || row.CompletedAt.After(at) {
			continue
		}
		for _, kind := range warningSettlementKinds {
			if row.Kind == kind {
				indices = append(indices, i)
				break
			}
		}
	}
	sort.Slice(indices, func(i, j int) bool {
		left, right := &settlements[indices[i]], &settlements[indices[j]]
		if !left.CompletedAt.Equal(right.CompletedAt) {
			return left.CompletedAt.After(right.CompletedAt)
		}
		return left.ID < right.ID
	})
	count, total := int64(0), int64(0)
	var since time.Time
	for _, index := range indices {
		if count == WarningSettlementLimit {
			break
		}
		row := &settlements[index]
		net := row.ProceedsPence
		for _, cost := range []int64{row.AcquisitionPence, row.FeesPence, row.ProvisionsPence, row.ExpensesPence} {
			var fits bool
			net, fits = growthChange(net, cost)
			if !fits {
				return nil
			}
		}
		if net >= 0 {
			break
		}
		if total < math.MinInt64-net {
			return nil
		}
		count++
		total += net
		since = row.CompletedAt
	}
	if count < LossStreakCount {
		return nil
	}
	return &Warning{Code: "loss_streak", Severity: "warning", Since: since, Facts: map[string]int64{"count": count, "net_pence": total}}
}

func (s *Store) Warnings(ctx context.Context, at time.Time) ([]AccountWarnings, error) {
	report, err := s.WarningReport(ctx, at)
	return report.Accounts, err
}

func (s *Store) WarningReport(ctx context.Context, at time.Time) (WarningReport, error) {
	if at.IsZero() {
		return WarningReport{}, fmt.Errorf("warnings timestamp: %w", ErrInvalidInput)
	}
	result := WarningReport{Accounts: make([]AccountWarnings, 0), Unmonitored: make([]UnmonitoredAccount, 0)}
	db, cancel := s.query(ctx)
	defer cancel()
	err := db.Transaction(func(tx *gorm.DB) error {
		var accounts []Account
		if err := tx.Order("created_at, id").Limit(maxDashboardAccounts + 1).Find(&accounts).Error; err != nil {
			return err
		}
		if len(accounts) > maxDashboardAccounts {
			return ErrResponseTooLarge
		}
		owner := CompetitionInput{OwnCompaniesComplete: true}
		type ownNames struct {
			names    []string
			complete bool
		}
		ownedBySeason := make(map[int]ownNames)
		for _, account := range accounts {
			if !account.Enabled {
				result.Unmonitored = append(result.Unmonitored, UnmonitoredAccount{AccountID: account.ID, Reason: "disabled"})
				continue
			}
			if account.CompanyID == "" {
				result.Unmonitored = append(result.Unmonitored, UnmonitoredAccount{AccountID: account.ID, Reason: "unregistered"})
				continue
			}
			status := AccountWarnings{
				AccountID: account.ID, Label: redacted,
				Warnings: make([]Warning, 0), Suppressed: make([]string, 0),
			}
			var secret accountSecret
			if err := tx.Where("account_id = ?", account.ID).Limit(1).Find(&secret).Error; err != nil {
				return err
			}
			credentials, err := s.decodeCredentials(secret)
			if err != nil {
				status.ErrorCode = WarningCredentialsUnavailable
				result.Accounts = append(result.Accounts, status)
				continue
			}
			if credentials.APIKey == "" {
				result.Unmonitored = append(result.Unmonitored, UnmonitoredAccount{AccountID: account.ID, Reason: "no_api_key"})
				continue
			}
			input, errorCode, err := warningInput(tx, account.ID, at)
			if err != nil {
				return err
			}
			input.Secrets = credentialSecrets(credentials)
			display := displayCredentials{secrets: input.Secrets, available: true}
			status.Label = displayText(account.Label, display)
			if errorCode != "" {
				status.ErrorCode = errorCode
				result.Accounts = append(result.Accounts, status)
				continue
			}
			if owner.AccountID == "" {
				owner.AccountID = account.ID
				if err := competitionOwners(tx, &owner, nil); err != nil {
					return err
				}
			}
			var season int
			input.Rising, input.RisingAt, season, err = latestRising(tx, account.ID, at)
			if err != nil {
				return err
			}
			if season == 0 {
				season, err = currentRivalSeason(tx, at)
				if err != nil {
					return err
				}
			}
			owned, cached := ownedBySeason[season]
			if !cached {
				names, complete, err := rivalOwnNames(tx, season, at)
				if err != nil {
					return err
				}
				owned = ownNames{names: names, complete: complete}
				ownedBySeason[season] = owned
			}
			input.OwnCompanies = append(append([]string(nil), owned.names...), owner.OwnCompanies...)
			input.Growth, err = growthBenchmark(tx, account.ID, input.OwnCompanies, at, 24*time.Hour)
			if err != nil {
				return err
			}
			if !owned.complete || !owner.OwnCompaniesComplete {
				// An incomplete identity set cannot safely define rival growth.
				input.Rising = gameapi.RisingBoard{}
				input.Growth = GrowthWindow{}
			}
			assessment := AssessWarnings(input, at)
			status.Available, status.Warnings, status.Suppressed = true, assessment.Warnings, assessment.Suppressed
			result.Accounts = append(result.Accounts, status)
		}
		return nil
	}, &sql.TxOptions{Isolation: sql.LevelRepeatableRead, ReadOnly: true})
	if err != nil {
		return WarningReport{}, err
	}
	return result, nil
}

const warningArrivalEventsSQL = `SELECT payload #>> '{payload,ship_id}' AS ship_id, event_type, created_at
	FROM events
	WHERE account_id = ? AND event_type IN ('ship_arrived', 'ship_departed')
		AND created_at >= ? AND created_at <= ? AND payload #>> '{payload,ship_id}' IN ?
	ORDER BY created_at DESC, id DESC
	LIMIT ?`

func warningArrivalShipIDs(input *WarningInput, at time.Time) []string {
	var ids []string
	for i := range input.Ships {
		ship := warningShipDetail(input, i, at)
		if ship.Ship.ID == "" || ship.Voyage != nil ||
			(ship.Ship.Status != "berthed" && ship.Ship.Status != "anchored_roadstead") ||
			ship.Ship.CurrentPortID == nil || *ship.Ship.CurrentPortID == "" ||
			(ship.Ship.BerthedAt != nil && !ship.Ship.BerthedAt.IsZero()) ||
			(ship.Ship.DockQueuedAt != nil && !ship.Ship.DockQueuedAt.IsZero()) {
			continue
		}
		if ids == nil {
			ids = make([]string, 0, len(input.Ships))
		}
		ids = append(ids, ship.Ship.ID)
	}
	return ids
}

func warningInput(tx *gorm.DB, accountID string, at time.Time) (WarningInput, string, error) {
	input := WarningInput{ShipEvents: make(map[string]WarningShipEvents)}
	var size int64
	if err := tx.Table("snapshots").Select("COALESCE(octet_length(data::text), 0)").Where("account_id = ?", accountID).Scan(&size).Error; err != nil {
		return input, "", err
	}
	if size > 2*MaxSnapshotBytes {
		return input, WarningSnapshotTooLarge, nil
	}
	var snapshot struct {
		Engine      json.RawMessage
		Metadata    json.RawMessage
		Ships       json.RawMessage
		Clock       json.RawMessage
		Ports       json.RawMessage
		ShipDetails json.RawMessage
	}
	if err := tx.Table("snapshots").
		Select(`data -> '_engine' AS engine, data -> '_meta' AS metadata, data -> 'ships' AS ships,
			data -> 'clock' AS clock, data #> '{catalog,ports}' AS ports, data -> 'ship_details' AS ship_details`).
		Where("account_id = ?", accountID).Scan(&snapshot).Error; err != nil {
		return input, "", err
	}
	for _, field := range []struct {
		raw    json.RawMessage
		target any
	}{
		{snapshot.Engine, &input.Engine}, {snapshot.Metadata, &input.Metadata},
		{snapshot.Ships, &input.Ships}, {snapshot.Clock, &input.Clock},
		{snapshot.Ports, &input.Catalog.Ports}, {snapshot.ShipDetails, &input.ShipDetails},
	} {
		if len(field.raw) > 0 {
			if err := json.Unmarshal(field.raw, field.target); err != nil {
				return input, WarningSnapshotInvalid, nil
			}
		}
	}
	shipIDs := warningArrivalShipIDs(&input, at)
	if len(shipIDs) > 0 {
		var events []struct {
			ShipID    string
			EventType string
			CreatedAt time.Time
		}
		if err := tx.Raw(warningArrivalEventsSQL, accountID, at.Add(-WarningArrivalLookback), at, shipIDs, WarningArrivalLimit).Scan(&events).Error; err != nil {
			return input, "", err
		}
		// Newest-first input means a truncated history can only leave ships
		// unknown; it cannot hide a departure newer than a measured arrival.
		for _, event := range events {
			times := input.ShipEvents[event.ShipID]
			if event.EventType == "ship_arrived" && times.ArrivedAt.IsZero() {
				times.ArrivedAt = event.CreatedAt
			}
			if event.EventType == "ship_departed" && times.DepartedAt.IsZero() {
				times.DepartedAt = event.CreatedAt
			}
			input.ShipEvents[event.ShipID] = times
		}
	}
	if err := tx.Table("settlements").Select("id, kind, costs_known, completed_at, acquisition_pence, proceeds_pence, fees_pence, provisions_pence, expenses_pence").
		Where("account_id = ? AND costs_known = ? AND completed_at <= ? AND kind IN ?", accountID, true, at, warningSettlementKinds).
		Order("completed_at DESC, id").Limit(WarningSettlementLimit).Scan(&input.Settlements).Error; err != nil {
		return input, "", err
	}
	return input, "", nil
}
