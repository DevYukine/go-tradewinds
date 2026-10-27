package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"sort"
	"time"

	"github.com/DevYukine/go-tradewinds/internal/gameapi"
	"gorm.io/gorm"
)

const rivalEvidenceWindow = 7 * 24 * time.Hour

// RivalValuation keeps the upstream publication clock and exact seasonal name.
// FirstAt is the first permanent valuation of that name in the same season.
type RivalValuation struct {
	SeasonNumber  int
	Rank          int
	RefreshedAt   time.Time
	FirstAt       time.Time
	NextRefreshAt *time.Time
	CompanyName   string
	NetWorthPence int64
}

// Payload is the inner SSE payload; At is the upstream event timestamp, not its
// local receipt time. No ID-to-name association is inferred from other events.
type RivalEvidence struct {
	ID      string
	Type    string
	At      time.Time
	Payload json.RawMessage
}

type RivalsInput struct {
	CompetitionInput
	Standings            []RivalValuation
	OurCompanyName       string
	Monopolies           json.RawMessage
	MonopoliesObservedAt time.Time
	MonopoliesComplete   bool
	MonopoliesError      string
	Evidence             []RivalEvidence
}

type RivalHunch string

const (
	rivalMostDangerous RivalHunch = "most_dangerous"
	rivalPlaysRough    RivalHunch = "plays_rough"
	rivalThreat        RivalHunch = "threat"
	rivalLosingValue   RivalHunch = "losing_value"
	rivalGrowing       RivalHunch = "growing"
	rivalSteady        RivalHunch = "steady"
	rivalTooNew        RivalHunch = "too_new"
	rivalUnmeasured    RivalHunch = "unmeasured"
	rivalNotRanked     RivalHunch = "not_ranked"
)

// Facts are flat, kind-tagged records. Only fields relevant to the kind are
// emitted; unavailable amounts remain nil, never a fabricated zero.
type RivalFact struct {
	Kind             string     `json:"kind"`
	Count            *int64     `json:"count,omitempty"`
	Total            *int64     `json:"total,omitempty"`
	Hours            *int64     `json:"hours,omitempty"`
	NetWorthPence    *int64     `json:"net_worth_pence,omitempty"`
	OurNetWorthPence *int64     `json:"our_net_worth_pence,omitempty"`
	ChangePence      *int64     `json:"change_pence,omitempty"`
	BaselinePence    *int64     `json:"baseline_pence,omitempty"`
	Commodity        string     `json:"commodity,omitempty"`
	Region           string     `json:"region,omitempty"`
	ExpiresAt        *time.Time `json:"expires_at,omitempty"`
}

type RivalRow struct {
	CompanyName   string     `json:"company_name"`
	Rank          *int       `json:"rank"`
	NetWorthPence *int64     `json:"net_worth_pence"`
	ChangePence   *int64     `json:"change_pence"`
	BaselinePence *int64     `json:"baseline_pence"`
	SharedLanes   *int64     `json:"shared_lanes"`
	SharedPorts   *int64     `json:"shared_ports"`
	Hunch         RivalHunch `json:"hunch"`
	Fact          RivalFact  `json:"fact"`
}

type RivalVerdict struct {
	CompanyName *string    `json:"company_name"`
	Fact        *RivalFact `json:"fact"`
	Reason      string     `json:"reason"`
}

type RivalVerdicts struct {
	MostDangerous RivalVerdict `json:"most_dangerous"`
	PlaysRough    RivalVerdict `json:"plays_rough"`
	NotProfitable RivalVerdict `json:"not_profitable"`
}

type GrowthWindow struct {
	WindowHours      int        `json:"window_hours"`
	From             *time.Time `json:"from"`
	To               *time.Time `json:"to"`
	OursPence        *int64     `json:"ours_pence"`
	OursReason       string     `json:"ours_reason"`
	RivalMedianPence *int64     `json:"rival_median_pence"`
	RivalsMeasured   int        `json:"rivals_measured"`
	RivalsUnmeasured int        `json:"rivals_unmeasured"`
	TopRival         *string    `json:"top_rival"`
	TopRivalPence    *int64     `json:"top_rival_pence"`
	OurPlace         *int       `json:"our_place"`
	PlaceOf          *int       `json:"place_of"`
}

type RivalReport struct {
	AccountID            string         `json:"account_id"`
	GeneratedAt          time.Time      `json:"generated_at"`
	SeasonNumber         int            `json:"season_number"`
	ValuationAt          *time.Time     `json:"valuation_at"`
	NextRefreshAt        *time.Time     `json:"next_refresh_at"`
	BaselineAt           *time.Time     `json:"baseline_at"`
	From                 time.Time      `json:"from"`
	To                   time.Time      `json:"to"`
	Coverage             string         `json:"coverage"`
	OwnCompaniesComplete bool           `json:"own_companies_complete"`
	OverlapKnown         bool           `json:"overlap_known"`
	EventsTruncated      bool           `json:"events_truncated"`
	Our                  *RivalRow      `json:"our"`
	Rivals               []RivalRow     `json:"rivals"`
	Verdicts             RivalVerdicts  `json:"verdicts"`
	Growth               []GrowthWindow `json:"growth"`
}

type rivalLane struct {
	first, second string
}

type rivalTraffic struct {
	ports map[string]bool
	lanes map[rivalLane]bool
	ships map[string]bool
}

// AggregateRivals compares one publication with one globally selected baseline.
// Traffic and conduct have separate real-time windows; neither measures profit,
// fleet size, intent, or the sponsors of anonymous world events.
func AggregateRivals(input RivalsInput) RivalReport {
	at := input.At.UTC()
	result := RivalReport{
		AccountID: input.AccountID, GeneratedAt: at, From: at.Add(-CompetitionWindow), To: at,
		Coverage: "not_observed", OwnCompaniesComplete: input.OwnCompaniesComplete,
		EventsTruncated: input.EventsTruncated, Rivals: make([]RivalRow, 0),
		Verdicts: RivalVerdicts{
			MostDangerous: RivalVerdict{Reason: "our_valuation_missing"},
			PlaysRough:    RivalVerdict{Reason: "none_recorded"},
			NotProfitable: RivalVerdict{Reason: "no_baseline"},
		},
	}
	var latest RivalValuation
	for _, value := range input.Standings {
		if value.SeasonNumber < 1 || value.Rank < 1 || value.CompanyName == "" || value.RefreshedAt.IsZero() || value.RefreshedAt.After(at) {
			continue
		}
		if value.SeasonNumber > latest.SeasonNumber || value.SeasonNumber == latest.SeasonNumber &&
			(value.RefreshedAt.After(latest.RefreshedAt) || value.RefreshedAt.Equal(latest.RefreshedAt) && value.Rank < latest.Rank) {
			latest = value
		}
	}
	if !latest.RefreshedAt.IsZero() {
		publishedAt := latest.RefreshedAt.UTC()
		result.SeasonNumber, result.ValuationAt = latest.SeasonNumber, &publishedAt
		if latest.NextRefreshAt != nil {
			next := latest.NextRefreshAt.UTC()
			result.NextRefreshAt = &next
		}
		target := publishedAt.Add(-CompetitionWindow)
		for _, value := range input.Standings {
			if value.SeasonNumber != latest.SeasonNumber || value.Rank < 1 || value.CompanyName == "" || value.RefreshedAt.IsZero() {
				continue
			}
			distance := value.RefreshedAt.Sub(target)
			if distance < -time.Hour || distance > time.Hour {
				continue
			}
			if distance < 0 {
				distance = -distance
			}
			if result.BaselineAt == nil {
				baseline := value.RefreshedAt.UTC()
				result.BaselineAt = &baseline
				continue
			}
			previous := result.BaselineAt.Sub(target)
			if previous < 0 {
				previous = -previous
			}
			if distance < previous || distance == previous && value.RefreshedAt.Before(*result.BaselineAt) {
				baseline := value.RefreshedAt.UTC()
				result.BaselineAt = &baseline
			}
		}
	}

	current := make(map[string]RivalValuation)
	baseline := make(map[string]int64)
	first := make(map[string]time.Time)
	for _, value := range input.Standings {
		if result.ValuationAt == nil || value.SeasonNumber != result.SeasonNumber || value.Rank < 1 || value.CompanyName == "" || value.RefreshedAt.IsZero() || value.RefreshedAt.After(*result.ValuationAt) {
			continue
		}
		firstAt := value.RefreshedAt
		if !value.FirstAt.IsZero() && value.FirstAt.Before(firstAt) {
			firstAt = value.FirstAt
		}
		if previous, ok := first[value.CompanyName]; !ok || firstAt.Before(previous) {
			first[value.CompanyName] = firstAt
		}
		if value.RefreshedAt.Equal(*result.ValuationAt) {
			if previous, ok := current[value.CompanyName]; !ok || value.Rank < previous.Rank {
				current[value.CompanyName] = value
			}
		}
		if result.BaselineAt != nil && value.RefreshedAt.Equal(*result.BaselineAt) {
			baseline[value.CompanyName] = value.NetWorthPence
		}
	}
	own := make(map[string]bool, len(input.OwnCompanies)+1)
	for _, name := range input.OwnCompanies {
		own[name] = true
	}
	if input.OurCompanyName != "" {
		own[input.OurCompanyName] = true
	}
	traffic, observed := rivalSailings(input.CompetitionInput)
	if observed {
		result.Coverage = "partial"
	}
	ourPorts := make(map[string]bool)
	ourLanes := make(map[rivalLane]bool)
	for name := range own {
		for port := range traffic[name].ports {
			ourPorts[port] = true
		}
		for lane := range traffic[name].lanes {
			ourLanes[lane] = true
		}
	}
	result.OverlapKnown = input.OwnCompaniesComplete && len(ourPorts) > 0
	rows := make(map[string]RivalRow, len(current)+len(traffic))
	for name, value := range current {
		row := RivalRow{CompanyName: name, Rank: &value.Rank, NetWorthPence: &value.NetWorthPence}
		if starting, ok := baseline[name]; ok {
			row.BaselinePence = &starting
			// Keep endpoint pence exact even when their difference cannot fit the
			// wire's int64. An overflowing change is unavailable, never wrapped.
			if !(starting > 0 && value.NetWorthPence < math.MinInt64+starting || starting < 0 && value.NetWorthPence > math.MaxInt64+starting) {
				change := value.NetWorthPence - starting
				row.ChangePence = &change
			}
		}
		row.Hunch, row.Fact = rivalValuationFact(row, first[name], result.ValuationAt)
		rows[name] = row
	}
	for name, activity := range traffic {
		if _, exists := rows[name]; !exists {
			ships := int64(len(activity.ships))
			rows[name] = RivalRow{CompanyName: name, Hunch: rivalNotRanked, Fact: RivalFact{Kind: "ships_seen", Count: &ships}}
		}
	}
	conduct := rivalConduct(input)
	for name, row := range rows {
		if result.OverlapKnown {
			var ports, lanes int64
			for port := range traffic[name].ports {
				if ourPorts[port] {
					ports++
				}
			}
			for lane := range traffic[name].lanes {
				if ourLanes[lane] {
					lanes++
				}
			}
			row.SharedPorts, row.SharedLanes = &ports, &lanes
		}
		if name == input.OurCompanyName && row.NetWorthPence != nil {
			our := row
			result.Our = &our
		}
		rows[name] = row
	}
	for name, row := range rows {
		if own[name] {
			continue
		}
		if row.NetWorthPence == nil {
			if fact, ok := rivalOverlapFact(row, len(ourLanes), len(ourPorts)); ok {
				row.Fact = fact
			}
		}
		if result.Our != nil && row.NetWorthPence != nil && *row.NetWorthPence >= *result.Our.NetWorthPence {
			if fact, ok := rivalOverlapFact(row, len(ourLanes), len(ourPorts)); ok {
				row.Hunch, row.Fact = rivalThreat, fact
			}
		}
		if fact, ok := conduct[name]; ok {
			row.Hunch, row.Fact = rivalPlaysRough, fact
		}
		result.Rivals = append(result.Rivals, row)
	}
	if input.OwnCompaniesComplete {
		var dangerous, rough, negative, declining *RivalRow
		var worstRate float64
		measured := false
		for i := range result.Rivals {
			row := &result.Rivals[i]
			if result.Our != nil && row.NetWorthPence != nil && *row.NetWorthPence > *result.Our.NetWorthPence {
				_, overlaps := rivalOverlapFact(*row, len(ourLanes), len(ourPorts))
				if (!result.OverlapKnown || overlaps) && (dangerous == nil || rivalWorthBefore(*row, *dangerous)) {
					dangerous = row
				}
			}
			if fact, ok := conduct[row.CompanyName]; ok {
				if rough == nil {
					rough = row
				} else {
					rank := rivalConductRank(fact.Kind)
					roughRank := rivalConductRank(conduct[rough.CompanyName].Kind)
					strongerEvidence := rank < roughRank
					equalEvidenceLarger := rank == roughRank && rivalWorthBefore(*row, *rough)
					if strongerEvidence || equalEvidenceLarger {
						rough = row
					}
				}
			}
			measured = measured || row.ChangePence != nil
			if row.NetWorthPence != nil && *row.NetWorthPence < 0 {
				if negative == nil || *row.NetWorthPence < *negative.NetWorthPence ||
					*row.NetWorthPence == *negative.NetWorthPence && row.CompanyName < negative.CompanyName {
					negative = row
				}
			}
			if row.ChangePence != nil && *row.ChangePence < 0 && row.BaselinePence != nil && *row.BaselinePence > 0 {
				rate := float64(*row.ChangePence) / float64(*row.BaselinePence)
				if declining == nil || rate < worstRate || rate == worstRate && row.CompanyName < declining.CompanyName {
					declining, worstRate = row, rate
				}
			}
		}
		if result.Our != nil {
			result.Verdicts.MostDangerous.Reason = "none_recorded"
		}
		if dangerous != nil {
			fact, ok := rivalOverlapFact(*dangerous, len(ourLanes), len(ourPorts))
			if !ok {
				fact = RivalFact{Kind: "larger", NetWorthPence: dangerous.NetWorthPence, OurNetWorthPence: result.Our.NetWorthPence}
			}
			dangerous.Hunch, dangerous.Fact = rivalMostDangerous, fact
			result.Verdicts.MostDangerous = RivalVerdict{CompanyName: new(dangerous.CompanyName), Fact: new(dangerous.Fact), Reason: "recorded"}
		}
		if rough != nil {
			fact := conduct[rough.CompanyName]
			result.Verdicts.PlaysRough = RivalVerdict{CompanyName: new(rough.CompanyName), Fact: new(fact), Reason: "recorded"}
		} else {
			for name := range conduct {
				if !own[name] {
					result.Verdicts.PlaysRough.Reason = "unlisted_only"
					break
				}
			}
		}
		unprofitable := negative
		if unprofitable == nil {
			unprofitable = declining
		}
		if unprofitable != nil {
			_, fact := rivalValuationFact(*unprofitable, first[unprofitable.CompanyName], result.ValuationAt)
			result.Verdicts.NotProfitable = RivalVerdict{CompanyName: new(unprofitable.CompanyName), Fact: new(fact), Reason: "recorded"}
		} else if measured || len(result.Rivals) == 0 && result.ValuationAt != nil {
			result.Verdicts.NotProfitable.Reason = "none_recorded"
		}
	} else {
		result.Verdicts = RivalVerdicts{
			MostDangerous: RivalVerdict{Reason: "own_companies_incomplete"},
			PlaysRough:    RivalVerdict{Reason: "own_companies_incomplete"},
			NotProfitable: RivalVerdict{Reason: "own_companies_incomplete"},
		}
	}
	sort.Slice(result.Rivals, func(i, j int) bool {
		left, right := result.Rivals[i], result.Rivals[j]
		if rivalHunchRank(left.Hunch) != rivalHunchRank(right.Hunch) {
			return rivalHunchRank(left.Hunch) < rivalHunchRank(right.Hunch)
		}
		return rivalWorthBefore(left, right)
	})
	return result
}

func rivalValuationFact(row RivalRow, first time.Time, latest *time.Time) (RivalHunch, RivalFact) {
	if row.NetWorthPence != nil && *row.NetWorthPence < 0 {
		hunch := rivalLosingValue
		if row.ChangePence != nil {
			if *row.ChangePence > 0 {
				hunch = rivalGrowing
			} else if *row.ChangePence == 0 {
				hunch = rivalSteady
			}
		}
		return hunch, RivalFact{Kind: "negative_worth"}
	}
	if row.ChangePence != nil {
		fact := RivalFact{Kind: "value_change", ChangePence: row.ChangePence, BaselinePence: row.BaselinePence}
		switch {
		case *row.ChangePence < 0:
			return rivalLosingValue, fact
		case *row.ChangePence > 0:
			return rivalGrowing, fact
		default:
			return rivalSteady, fact
		}
	}
	if latest != nil && !first.IsZero() && !first.After(*latest) && latest.Sub(first) < CompetitionWindow {
		hours := int64(latest.Sub(first) / time.Hour)
		return rivalTooNew, RivalFact{Kind: "short_history", Hours: &hours}
	}
	return rivalUnmeasured, RivalFact{Kind: "no_baseline"}
}

func rivalOverlapFact(row RivalRow, lanes, ports int) (RivalFact, bool) {
	if row.SharedLanes != nil && *row.SharedLanes > 0 {
		total := int64(lanes)
		return RivalFact{Kind: "shared_lanes", Count: row.SharedLanes, Total: &total}, true
	}
	if row.SharedPorts != nil && *row.SharedPorts > 0 {
		total := int64(ports)
		return RivalFact{Kind: "shared_ports", Count: row.SharedPorts, Total: &total}, true
	}
	return RivalFact{}, false
}

func rivalHunchRank(hunch RivalHunch) int {
	switch hunch {
	case rivalMostDangerous:
		return 0
	case rivalPlaysRough:
		return 1
	case rivalThreat:
		return 2
	case rivalLosingValue:
		return 3
	case rivalGrowing:
		return 4
	case rivalSteady:
		return 5
	case rivalTooNew:
		return 6
	case rivalUnmeasured:
		return 7
	default:
		return 8
	}
}

func rivalWorthBefore(left, right RivalRow) bool {
	if left.NetWorthPence == nil || right.NetWorthPence == nil {
		if (left.NetWorthPence == nil) != (right.NetWorthPence == nil) {
			return left.NetWorthPence != nil
		}
	} else if *left.NetWorthPence != *right.NetWorthPence {
		return *left.NetWorthPence > *right.NetWorthPence
	}
	return left.CompanyName < right.CompanyName
}

func rivalSailings(input CompetitionInput) (map[string]rivalTraffic, bool) {
	from := input.At.Add(-CompetitionWindow)
	knownPorts := make(map[string]bool, len(input.Ports))
	observed := false
	for _, port := range input.Ports {
		knownPorts[port.ID] = true
		observed = observed || port.QuayVisible && !port.ObservedAt.IsZero() && !port.ObservedAt.Before(from) && !port.ObservedAt.After(input.At)
	}
	type noticeKey struct {
		port, company, ship, direction string
		at                             time.Time
	}
	seen := make(map[noticeKey]bool)
	traffic := make(map[string]rivalTraffic)
	add := func(notice gameapi.SailingNotice) {
		if notice.PortID == "" || !knownPorts[notice.PortID] || notice.CompanyName == "" || notice.ShipName == "" || notice.SeenAt.IsZero() || notice.SeenAt.Before(from) || notice.SeenAt.After(input.At) {
			return
		}
		key := noticeKey{notice.PortID, notice.CompanyName, notice.ShipName, notice.Direction, notice.SeenAt.UTC()}
		if seen[key] {
			return
		}
		seen[key] = true
		observed = true
		activity, ok := traffic[notice.CompanyName]
		if !ok {
			activity = rivalTraffic{ports: make(map[string]bool), lanes: make(map[rivalLane]bool), ships: make(map[string]bool)}
			traffic[notice.CompanyName] = activity
		}
		activity.ports[notice.PortID] = true
		activity.ships[notice.ShipName] = true
		if notice.OtherPortID != nil && *notice.OtherPortID != "" && *notice.OtherPortID != notice.PortID {
			lane := rivalLane{notice.PortID, *notice.OtherPortID}
			if lane.first > lane.second {
				lane.first, lane.second = lane.second, lane.first
			}
			activity.lanes[lane] = true
		}
	}
	for _, notice := range input.Sailings {
		add(notice)
	}
	for _, port := range input.Ports {
		for _, notice := range port.Sailings {
			add(notice)
		}
	}
	return traffic, observed
}

func rivalConduct(input RivalsInput) map[string]RivalFact {
	facts := make(map[string]RivalFact)
	if input.MonopoliesComplete && input.MonopoliesError == "" && !input.MonopoliesObservedAt.IsZero() &&
		!input.MonopoliesObservedAt.After(input.At) && input.At.Sub(input.MonopoliesObservedAt) <= 30*time.Minute {
		var monopolies []struct {
			HolderCompanyName string    `json:"holder_company_name"`
			CommodityID       string    `json:"commodity_id"`
			BankingRegion     string    `json:"banking_region"`
			StartsAt          time.Time `json:"starts_at"`
			ExpiresAt         time.Time `json:"expires_at"`
			InForce           bool      `json:"in_force"`
		}
		if json.Unmarshal(input.Monopolies, &monopolies) == nil {
			for _, monopoly := range monopolies {
				if monopoly.HolderCompanyName == "" || monopoly.CommodityID == "" || monopoly.BankingRegion == "" ||
					!monopoly.InForce || !monopoly.ExpiresAt.After(input.At) || monopoly.StartsAt.After(input.MonopoliesObservedAt) {
					continue
				}
				expires := monopoly.ExpiresAt.UTC()
				fact := RivalFact{Kind: "monopoly_in_force", Commodity: monopoly.CommodityID, Region: monopoly.BankingRegion, ExpiresAt: &expires}
				previous, exists := facts[monopoly.HolderCompanyName]
				if !exists || fact.Commodity < previous.Commodity || fact.Commodity == previous.Commodity &&
					(fact.Region < previous.Region || fact.Region == previous.Region && fact.ExpiresAt.Before(*previous.ExpiresAt)) {
					facts[monopoly.HolderCompanyName] = fact
				}
			}
		}
	}
	type creditRecord struct {
		Defaults  *int64 `json:"loans_defaulted"`
		Walkaways *int64 `json:"loans_walked_away"`
	}
	type creditObservation struct {
		at     time.Time
		id     string
		record creditRecord
	}
	credit := make(map[string]creditObservation)
	grants := make(map[string]int64)
	seenGrants := make(map[string]bool)
	for _, event := range input.Evidence {
		if event.At.IsZero() || event.At.Before(input.At.Add(-rivalEvidenceWindow)) || event.At.After(input.At) {
			continue
		}
		switch event.Type {
		case "monopoly_granted":
			var payload struct {
				HolderCompanyName string `json:"holder_company_name"`
			}
			if event.ID == "" || seenGrants[event.ID] || json.Unmarshal(event.Payload, &payload) != nil || payload.HolderCompanyName == "" {
				continue
			}
			seenGrants[event.ID] = true
			grants[payload.HolderCompanyName]++
		case "loan_request_posted":
			var payload struct {
				BorrowerName      string        `json:"borrower_name"`
				BorrowerCompanyID string        `json:"borrower_company_id"`
				BorrowerRecord    *creditRecord `json:"borrower_record"`
			}
			if json.Unmarshal(event.Payload, &payload) != nil || payload.BorrowerName == "" || payload.BorrowerCompanyID == "" || payload.BorrowerRecord == nil {
				continue
			}
			record := *payload.BorrowerRecord
			if record.Defaults == nil && record.Walkaways == nil || record.Defaults != nil && *record.Defaults < 0 || record.Walkaways != nil && *record.Walkaways < 0 {
				continue
			}
			previous, exists := credit[payload.BorrowerName]
			if !exists || event.At.After(previous.at) || event.At.Equal(previous.at) && event.ID > previous.id {
				credit[payload.BorrowerName] = creditObservation{event.At, event.ID, record}
			}
		}
	}
	for name, count := range grants {
		if _, exists := facts[name]; !exists {
			facts[name] = RivalFact{Kind: "monopolies_recent", Count: &count}
		}
	}
	for name, observation := range credit {
		if _, exists := facts[name]; exists {
			continue
		}
		if count := observation.record.Walkaways; count != nil && *count > 0 {
			facts[name] = RivalFact{Kind: "loan_walkaways", Count: count}
		} else if count := observation.record.Defaults; count != nil && *count > 0 {
			facts[name] = RivalFact{Kind: "loan_defaults", Count: count}
		}
	}
	return facts
}

func rivalConductRank(kind string) int {
	switch kind {
	case "monopoly_in_force":
		return 0
	case "monopolies_recent":
		return 1
	case "loan_walkaways":
		return 2
	default:
		return 3
	}
}

// Rivals reads only persisted facts, in one repeatable-read snapshot. Receipt
// windows bound indexed reads; upstream times are checked again by the pure fold.
func (s *Store) Rivals(ctx context.Context, accountID string, at time.Time) (RivalReport, error) {
	if !validID(accountID) || at.IsZero() {
		return RivalReport{}, fmt.Errorf("rivals query: %w", ErrInvalidInput)
	}
	input := RivalsInput{CompetitionInput: CompetitionInput{AccountID: accountID, At: at.UTC(), OwnCompaniesComplete: true}}
	db, cancel := s.query(ctx)
	defer cancel()
	var result RivalReport
	err := db.Transaction(func(tx *gorm.DB) error {
		if err := competitionOwners(tx, &input.CompetitionInput, nil); err != nil {
			return err
		}
		scrubber := displayScrubber{store: s, tx: tx}
		credentials := scrubber.credentials(accountID)
		if !credentials.available {
			return errors.New("rivals display credentials unavailable")
		}
		publications, identities, err := rivalStandings(tx, &input)
		if err != nil {
			return err
		}
		var snapshot struct {
			Catalog    json.RawMessage
			Ports      json.RawMessage
			Metadata   json.RawMessage
			Monopolies json.RawMessage
		}
		var size int64
		if err := tx.Table("snapshots").Select("COALESCE(octet_length(data::text), 0)").Where("account_id = ?", accountID).Scan(&size).Error; err != nil {
			return err
		}
		if size > 2*MaxSnapshotBytes {
			return ErrResponseTooLarge
		}
		if err := tx.Table("snapshots").Select("data -> 'catalog' AS catalog, data -> 'ports' AS ports, data -> '_meta' AS metadata, data -> 'monopolies' AS monopolies").
			Where("account_id = ?", accountID).Scan(&snapshot).Error; err != nil {
			return err
		}
		ports, err := competitionPorts(snapshot.Catalog, snapshot.Ports, snapshot.Metadata, input.At)
		if err != nil {
			return err
		}
		input.Ports, input.Monopolies = ports, snapshot.Monopolies
		var metadata struct {
			Monopolies struct {
				UpdatedAt    time.Time  `json:"updated_at"`
				UpstreamAsOf *time.Time `json:"upstream_as_of"`
				Complete     bool       `json:"complete"`
				Stale        bool       `json:"stale"`
				Error        string     `json:"error"`
			} `json:"monopolies"`
		}
		if len(snapshot.Metadata) != 0 {
			if err := json.Unmarshal(snapshot.Metadata, &metadata); err != nil {
				return fmt.Errorf("decode rivals metadata: %w", err)
			}
		}
		meta := metadata.Monopolies
		input.MonopoliesObservedAt = meta.UpdatedAt
		input.MonopoliesError = meta.Error
		observedNotInFuture := !meta.UpdatedAt.After(input.At)
		input.MonopoliesComplete = meta.Complete && !meta.Stale && observedNotInFuture
		if meta.UpstreamAsOf != nil {
			if meta.UpstreamAsOf.Before(input.MonopoliesObservedAt) {
				input.MonopoliesObservedAt = *meta.UpstreamAsOf
			}
			if meta.UpstreamAsOf.After(input.At) {
				input.MonopoliesComplete = false
			}
		}
		if err := competitionSailings(tx, &input.CompetitionInput); err != nil {
			return err
		}
		if err := rivalEvidence(tx, &input); err != nil {
			return err
		}
		result = AggregateRivals(input)
		for _, window := range []time.Duration{6 * time.Hour, 24 * time.Hour} {
			result.Growth = append(result.Growth, growthFromPublications(input.Standings, identities, input.OwnCompanies, publications, window))
		}
		scrubRivals(&result, credentials)
		return nil
	}, &sql.TxOptions{Isolation: sql.LevelRepeatableRead, ReadOnly: true})
	if err != nil {
		return RivalReport{}, err
	}
	return result, nil
}

type standingsPublications struct {
	SeasonNumber int
	To           time.Time
	From         map[time.Duration]time.Time
	Times        []time.Time
}

// Every standings consumer uses the same seasonal publication and nearest-window
// matching. A report reads each matched publication only once.
func matchStandingsPublications(tx *gorm.DB, at time.Time, windows ...time.Duration) (standingsPublications, error) {
	var latest struct {
		SeasonNumber int
		RefreshedAt  time.Time
	}
	if err := tx.Table("standings_history").Select("season_number, refreshed_at").Where("refreshed_at <= ?", at).
		Order("season_number DESC, refreshed_at DESC, rank").Limit(1).Scan(&latest).Error; err != nil {
		return standingsPublications{}, err
	}
	if latest.RefreshedAt.IsZero() {
		return standingsPublications{}, nil
	}
	result := standingsPublications{
		SeasonNumber: latest.SeasonNumber, To: latest.RefreshedAt.UTC(),
		From:  make(map[time.Duration]time.Time, len(windows)),
		Times: []time.Time{latest.RefreshedAt.UTC()},
	}
	for _, window := range windows {
		if _, exists := result.From[window]; exists {
			continue
		}
		target := result.To.Add(-window)
		var baseline struct{ RefreshedAt time.Time }
		if err := tx.Raw(`SELECT refreshed_at FROM standings_history
			WHERE season_number = ? AND refreshed_at BETWEEN ? AND ?
			ORDER BY abs(extract(epoch FROM (refreshed_at - ?::timestamptz))), refreshed_at, rank LIMIT 1`,
			result.SeasonNumber, target.Add(-time.Hour), target.Add(time.Hour), target).Scan(&baseline).Error; err != nil {
			return result, err
		}
		result.From[window] = baseline.RefreshedAt.UTC()
		if !baseline.RefreshedAt.IsZero() {
			result.Times = append(result.Times, baseline.RefreshedAt.UTC())
		}
	}
	return result, nil
}

func standingsPublicationValues(tx *gorm.DB, publications standingsPublications, firstObserved bool) ([]RivalValuation, error) {
	if publications.To.IsZero() {
		return nil, nil
	}
	fields := "h.season_number, h.refreshed_at, h.rank, h.company_name, h.net_worth_pence, h.next_refresh_at"
	query := tx.Table("standings_history AS h")
	if firstObserved {
		fields += ", first.refreshed_at AS first_at"
		query = query.Joins(`JOIN LATERAL (SELECT refreshed_at FROM standings_history
			WHERE season_number = h.season_number AND company_name = h.company_name
			AND refreshed_at <= h.refreshed_at ORDER BY refreshed_at LIMIT 1) AS first ON true`)
	}
	// Public top 20 plus at most one extra row per configured account.
	maxValues := len(publications.Times) * (20 + maxDashboardAccounts)
	var values []RivalValuation
	if err := query.Select(fields).
		Where("h.season_number = ? AND h.refreshed_at IN ?", publications.SeasonNumber, publications.Times).
		Order("h.refreshed_at DESC, h.rank").Limit(maxValues + 1).Scan(&values).Error; err != nil {
		return nil, err
	}
	if len(values) > maxValues {
		return nil, ErrResponseTooLarge
	}
	return values, nil
}

func standingsPublicationIdentities(tx *gorm.DB, accountID string, publications standingsPublications) (map[time.Time]string, error) {
	if publications.To.IsZero() {
		return nil, nil
	}
	var rows []struct {
		RefreshedAt time.Time
		CompanyName string
	}
	if err := tx.Table("standings_accounts AS a").Select("a.refreshed_at, h.company_name").
		Joins("JOIN standings_history AS h ON h.season_number = a.season_number AND h.refreshed_at = a.refreshed_at AND h.rank = a.rank").
		Where("a.account_id = ? AND a.season_number = ? AND a.refreshed_at IN ?", accountID, publications.SeasonNumber, publications.Times).
		Scan(&rows).Error; err != nil {
		return nil, err
	}
	identities := make(map[time.Time]string, len(rows))
	for _, row := range rows {
		identities[row.RefreshedAt.UTC()] = row.CompanyName
	}
	return identities, nil
}

func rivalStandings(tx *gorm.DB, input *RivalsInput) (standingsPublications, map[time.Time]string, error) {
	publications, err := matchStandingsPublications(tx, input.At, 6*time.Hour, CompetitionWindow)
	if err != nil {
		return publications, nil, err
	}
	input.Standings, err = standingsPublicationValues(tx, publications, true)
	if err != nil {
		return publications, nil, err
	}
	identities, err := standingsPublicationIdentities(tx, input.AccountID, publications)
	if err != nil {
		return publications, nil, err
	}
	input.OurCompanyName = identities[publications.To]
	return publications, identities, nil
}

func rivalEvidence(tx *gorm.DB, input *RivalsInput) error {
	var rows []struct {
		ID         string
		Type       string
		SourceTime string
		Payload    json.RawMessage
	}
	// Project just attributable fields, not entire diagnostic event bodies.
	if err := tx.Table("events").Select(`event_id AS id, event_type AS type, payload ->> 'timestamp' AS source_time,
		CASE WHEN event_type = 'monopoly_granted' THEN jsonb_build_object(
			'holder_company_name', payload #> '{payload,holder_company_name}')
		ELSE jsonb_build_object('borrower_name', payload #> '{payload,borrower_name}',
			'borrower_company_id', payload #> '{payload,borrower_company_id}',
			'borrower_record', jsonb_build_object(
				'loans_defaulted', payload #> '{payload,borrower_record,loans_defaulted}',
				'loans_walked_away', payload #> '{payload,borrower_record,loans_walked_away}')) END AS payload`).
		Where("account_id = ? AND created_at >= ? AND created_at <= ?", input.AccountID, input.At.Add(-rivalEvidenceWindow), input.At).
		Where("event_type IN ('monopoly_granted','loan_request_posted')").
		Order("created_at DESC, events.id DESC").Limit(maxCompetitionEvents + 1).Scan(&rows).Error; err != nil {
		return err
	}
	if len(rows) > maxCompetitionEvents {
		input.EventsTruncated = true
		rows = rows[:maxCompetitionEvents]
	}
	input.Evidence = make([]RivalEvidence, 0, len(rows))
	for _, row := range rows {
		at, err := time.Parse(time.RFC3339Nano, row.SourceTime)
		if err != nil {
			continue
		}
		input.Evidence = append(input.Evidence, RivalEvidence{ID: row.ID, Type: row.Type, At: at, Payload: row.Payload})
	}
	return nil
}

func scrubRivals(report *RivalReport, credentials displayCredentials) {
	fact := func(value *RivalFact) {
		value.Commodity = displayText(value.Commodity, credentials)
		value.Region = displayText(value.Region, credentials)
	}
	row := func(value *RivalRow) {
		value.CompanyName = displayText(value.CompanyName, credentials)
		fact(&value.Fact)
	}
	if report.Our != nil {
		row(report.Our)
	}
	for i := range report.Rivals {
		row(&report.Rivals[i])
	}
	for _, verdict := range []*RivalVerdict{&report.Verdicts.MostDangerous, &report.Verdicts.PlaysRough, &report.Verdicts.NotProfitable} {
		if verdict.CompanyName != nil {
			name := displayText(*verdict.CompanyName, credentials)
			verdict.CompanyName = &name
		}
		if verdict.Fact != nil {
			fact(verdict.Fact)
		}
	}
	for i := range report.Growth {
		if report.Growth[i].TopRival != nil {
			name := displayText(*report.Growth[i].TopRival, credentials)
			report.Growth[i].TopRival = &name
		}
	}
}

// Growth uses exact official publications and account-to-rank links at both
// endpoints. A name change cannot be treated as an observed personal return.
func growthBenchmark(tx *gorm.DB, accountID string, ownNames []string, t1 time.Time, window time.Duration) (GrowthWindow, error) {
	publications, err := matchStandingsPublications(tx, t1, window)
	if err != nil {
		return GrowthWindow{}, err
	}
	values, err := standingsPublicationValues(tx, publications, false)
	if err != nil {
		return GrowthWindow{}, err
	}
	identities, err := standingsPublicationIdentities(tx, accountID, publications)
	if err != nil {
		return GrowthWindow{}, err
	}
	return growthFromPublications(values, identities, ownNames, publications, window), nil
}

func growthFromPublications(values []RivalValuation, identities map[time.Time]string, ownNames []string, publications standingsPublications, window time.Duration) GrowthWindow {
	result := GrowthWindow{WindowHours: int(window / time.Hour), OursReason: "no_publication"}
	if publications.To.IsZero() {
		return result
	}
	to := publications.To
	result.To, result.OursReason = &to, "no_baseline"
	from := publications.From[window]
	if !from.IsZero() {
		result.From = &from
	}
	own := make(map[string]bool, len(ownNames)+len(identities))
	for _, name := range ownNames {
		own[name] = true
	}
	fromName, toName := identities[from], identities[to]
	if fromName != "" {
		own[fromName] = true
	}
	if toName != "" {
		own[toName] = true
	}
	current := make(map[string]int64)
	starting := make(map[string]int64)
	for _, value := range values {
		if value.RefreshedAt.Equal(to) {
			current[value.CompanyName] = value.NetWorthPence
		} else if result.From != nil && value.RefreshedAt.Equal(from) {
			starting[value.CompanyName] = value.NetWorthPence
		}
	}
	if result.From != nil {
		switch {
		case fromName == "" || toName == "":
			result.OursReason = "our_valuation_missing"
		case fromName != toName:
			result.OursReason = "company_changed"
		default:
			if delta, ok := growthChange(current[toName], starting[fromName]); ok {
				result.OursPence, result.OursReason = &delta, ""
			} else {
				result.OursReason = "change_out_of_range"
			}
		}
	}
	deltas := make([]int64, 0, len(current))
	for name, value := range current {
		if own[name] {
			continue
		}
		before, measured := starting[name]
		delta, fits := growthChange(value, before)
		if !measured || !fits {
			result.RivalsUnmeasured++
			continue
		}
		deltas = append(deltas, delta)
		if result.TopRivalPence == nil || delta > *result.TopRivalPence ||
			delta == *result.TopRivalPence && name < *result.TopRival {
			top := name
			result.TopRival, result.TopRivalPence = &top, &delta
		}
	}
	result.RivalsMeasured = len(deltas)
	if len(deltas) > 0 {
		sort.Slice(deltas, func(i, j int) bool { return deltas[i] < deltas[j] })
		median := deltas[len(deltas)/2]
		if len(deltas)%2 == 0 {
			left := deltas[len(deltas)/2-1]
			// Average with truncation toward zero, without an overflowing sum.
			mean := (left & median) + ((left ^ median) >> 1)
			if mean < 0 && (left^median)&1 != 0 {
				mean++
			}
			median = mean
		}
		result.RivalMedianPence = &median
	}
	if result.OursPence != nil {
		place, total := 1, len(deltas)+1
		for _, delta := range deltas {
			if delta > *result.OursPence {
				place++
			}
		}
		result.OurPlace, result.PlaceOf = &place, &total
	}
	return result
}

func growthChange(after, before int64) (int64, bool) {
	if before > 0 && after < math.MinInt64+before || before < 0 && after > math.MaxInt64+before {
		return 0, false
	}
	return after - before, true
}
