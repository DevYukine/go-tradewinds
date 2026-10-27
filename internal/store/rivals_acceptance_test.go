package store

// Assumed API: AggregateRivals(RivalsInput) RivalReport and
// (*Store).Rivals(context.Context, string, time.Time) (RivalReport, error).
// RivalsInput embeds CompetitionInput and adds Standings []RivalValuation,
// OurCompanyName string, Monopolies json.RawMessage, MonopoliesObservedAt time.Time,
// MonopoliesComplete bool, MonopoliesError string, Evidence []RivalEvidence.
// RivalValuation has SeasonNumber, Rank int; RefreshedAt, FirstAt time.Time;
// NextRefreshAt *time.Time; CompanyName string; NetWorthPence int64.
// RivalEvidence has ID, Type string; At time.Time; Payload json.RawMessage (inner payload).
// Report JSON has rivals, our, verdicts, valuation_at, baseline_at, next_refresh_at,
// from, to, coverage, own_companies_complete and events_truncated. Verdicts carry
// nullable company_name/fact plus reason; facts are flat objects keyed by kind.

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/DevYukine/go-tradewinds/internal/gameapi"
)

type rivalsAcceptanceFact struct {
	Kind      string     `json:"kind"`
	Count     int        `json:"count"`
	Total     int        `json:"total"`
	Hours     int        `json:"hours"`
	Commodity string     `json:"commodity"`
	Region    string     `json:"region"`
	ExpiresAt *time.Time `json:"expires_at"`
}

type rivalsAcceptanceRow struct {
	CompanyName   string               `json:"company_name"`
	Hunch         string               `json:"hunch"`
	NetWorthPence *int64               `json:"net_worth_pence"`
	ChangePence   *int64               `json:"change_pence"`
	SharedLanes   *int                 `json:"shared_lanes"`
	SharedPorts   *int                 `json:"shared_ports"`
	Fact          rivalsAcceptanceFact `json:"fact"`
}

type rivalsAcceptanceVerdict struct {
	CompanyName *string               `json:"company_name"`
	Fact        *rivalsAcceptanceFact `json:"fact"`
	Reason      string                `json:"reason"`
}

type rivalsAcceptanceReport struct {
	Rivals   []rivalsAcceptanceRow `json:"rivals"`
	Our      *rivalsAcceptanceRow  `json:"our"`
	Verdicts struct {
		MostDangerous rivalsAcceptanceVerdict `json:"most_dangerous"`
		PlaysRough    rivalsAcceptanceVerdict `json:"plays_rough"`
		NotProfitable rivalsAcceptanceVerdict `json:"not_profitable"`
	} `json:"verdicts"`
	ValuationAt          *time.Time `json:"valuation_at"`
	BaselineAt           *time.Time `json:"baseline_at"`
	NextRefreshAt        *time.Time `json:"next_refresh_at"`
	From                 time.Time  `json:"from"`
	To                   time.Time  `json:"to"`
	Coverage             string     `json:"coverage"`
	OwnCompaniesComplete bool       `json:"own_companies_complete"`
	EventsTruncated      bool       `json:"events_truncated"`
}

func rivalsAcceptanceDecode(t *testing.T, value any) rivalsAcceptanceReport {
	t.Helper()
	raw, err := json.Marshal(value)
	if err != nil {
		t.Fatal(err)
	}
	var report rivalsAcceptanceReport
	if err := json.Unmarshal(raw, &report); err != nil {
		t.Fatal(err)
	}
	return report
}

func rivalsAcceptanceFind(t *testing.T, report rivalsAcceptanceReport, name string) rivalsAcceptanceRow {
	t.Helper()
	for _, row := range report.Rivals {
		if row.CompanyName == name {
			return row
		}
	}
	t.Fatalf("missing exact company %q: %+v", name, report.Rivals)
	return rivalsAcceptanceRow{}
}

func rivalsAcceptanceFixture() RivalsInput {
	at := time.Date(2026, 10, 27, 1, 28, 22, 0, time.UTC)
	next := at.Add(time.Hour)
	input := RivalsInput{
		CompetitionInput: CompetitionInput{
			AccountID: "one", At: at,
			OwnCompanies:         []string{"The Pokemon Pirates", "Our second company"},
			OwnCompaniesComplete: true,
		},
		OurCompanyName: "The Pokemon Pirates",
	}
	for rank, value := range []struct {
		name          string
		worth, change int64
	}{
		{"The Kraken Cartel", 436_501_259, -11_764_585},
		{"TopStats Logistics", 24_006_393, -5_933_407},
		{"The Pokemon Pirates", 11_613_290, -1_246_694},
		{"Vekbeard's Seedbox Shipping", 2_085_466, 0},
		{"TescoValue", 1_940_960, 93_888},
		{"VoC", 962_383, -1_285_198},
		{"Azur Lane", 94_065, -2_793_384},
		{"Meridian Commerce", -1_245_586, -4_217_923},
	} {
		first := at.Add(-48 * time.Hour)
		if rank == 3 {
			first = at.Add(-5 * time.Hour)
		}
		input.Standings = append(input.Standings, RivalValuation{
			SeasonNumber: 1, Rank: rank + 1, CompanyName: value.name,
			RefreshedAt: at, FirstAt: first, NextRefreshAt: &next, NetWorthPence: value.worth,
		})
		if rank != 3 {
			input.Standings = append(input.Standings, RivalValuation{
				SeasonNumber: 1, Rank: rank + 1, CompanyName: value.name,
				RefreshedAt: at.Add(-24 * time.Hour), FirstAt: first, NetWorthPence: value.worth - value.change,
			})
		}
	}
	for i := range 9 {
		input.Ports = append(input.Ports, CompetitionPortObservation{ID: fmt.Sprintf("p%d", i), Name: fmt.Sprintf("Port %d", i)})
	}
	for _, company := range []struct {
		name  string
		lanes int
	}{
		{"The Pokemon Pirates", 16}, {"The Kraken Cartel", 14},
		{"TopStats Logistics", 7}, {"TopStats AI Logistics", 5},
	} {
		for i := range company.lanes {
			origin, destination := i%9, (i%9+1+i/9)%9
			other := fmt.Sprintf("p%d", destination)
			input.Sailings = append(input.Sailings, gameapi.SailingNotice{
				CompanyName: company.name, ShipName: fmt.Sprintf("Ship %d", i),
				PortID: fmt.Sprintf("p%d", origin), OtherPortID: &other,
				Direction: "departed", SeenAt: at.Add(-time.Minute),
			})
		}
	}
	return input
}

func TestAggregateRivalsAcceptance0128Fixture(t *testing.T) {
	input := rivalsAcceptanceFixture()
	report := rivalsAcceptanceDecode(t, AggregateRivals(input))
	for _, want := range []struct {
		name, hunch, fact string
		worth, change     int64
	}{
		{"The Kraken Cartel", "most_dangerous", "shared_lanes", 436_501_259, -11_764_585},
		{"TopStats Logistics", "threat", "shared_lanes", 24_006_393, -5_933_407},
		{"Meridian Commerce", "losing_value", "negative_worth", -1_245_586, -4_217_923},
		{"Azur Lane", "losing_value", "value_change", 94_065, -2_793_384},
		{"VoC", "losing_value", "value_change", 962_383, -1_285_198},
		{"TescoValue", "growing", "value_change", 1_940_960, 93_888},
		{"Vekbeard's Seedbox Shipping", "too_new", "short_history", 2_085_466, 0},
		{"TopStats AI Logistics", "not_ranked", "shared_lanes", 0, 0},
	} {
		t.Run(want.name, func(t *testing.T) {
			row := rivalsAcceptanceFind(t, report, want.name)
			if row.Hunch != want.hunch || row.Fact.Kind != want.fact {
				t.Fatalf("hunch/fact: %+v, want %s/%s", row, want.hunch, want.fact)
			}
			if want.hunch == "not_ranked" {
				if row.NetWorthPence != nil || row.ChangePence != nil {
					t.Fatalf("similar name inherited ranked valuation: %+v", row)
				}
			} else if row.NetWorthPence == nil || *row.NetWorthPence != want.worth {
				t.Fatalf("integer pence changed: %+v", row)
			}
			if want.hunch == "too_new" {
				if row.ChangePence != nil || row.Fact.Hours != 5 {
					t.Fatalf("five-hour history became measured daily change: %+v", row)
				}
			} else if want.hunch != "not_ranked" && (row.ChangePence == nil || *row.ChangePence != want.change) {
				t.Fatalf("24-hour endpoint arithmetic: %+v", row)
			}
		})
	}
	for name, lanes := range map[string]int{"The Kraken Cartel": 14, "TopStats Logistics": 7, "TopStats AI Logistics": 5} {
		row := rivalsAcceptanceFind(t, report, name)
		if row.SharedLanes == nil || *row.SharedLanes != lanes || row.Fact.Count != lanes || row.Fact.Total != 16 {
			t.Fatalf("reported lane numerator/denominator for %s: %+v", name, row)
		}
	}
	if len(report.Rivals) != 8 || report.Our == nil || report.Our.CompanyName != input.OurCompanyName || report.Our.ChangePence == nil || *report.Our.ChangePence != -1_246_694 {
		t.Fatalf("own row or observed rival population: %+v", report)
	}
	if report.Verdicts.MostDangerous.CompanyName == nil || *report.Verdicts.MostDangerous.CompanyName != "The Kraken Cartel" || report.Verdicts.NotProfitable.CompanyName == nil || *report.Verdicts.NotProfitable.CompanyName != "Meridian Commerce" {
		t.Fatalf("headline selection: %+v", report.Verdicts)
	}
	if report.Verdicts.PlaysRough.CompanyName != nil || report.Verdicts.PlaysRough.Fact != nil || report.Verdicts.PlaysRough.Reason != "none_recorded" {
		t.Fatalf("missing conduct evidence was invented: %+v", report.Verdicts.PlaysRough)
	}
	if report.ValuationAt == nil || !report.ValuationAt.Equal(input.At) || report.BaselineAt == nil || !report.BaselineAt.Equal(input.At.Add(-24*time.Hour)) || report.NextRefreshAt == nil || !report.NextRefreshAt.Equal(input.At.Add(time.Hour)) || !report.From.Equal(input.At.Add(-24*time.Hour)) || !report.To.Equal(input.At) || report.Coverage != "partial" || !report.OwnCompaniesComplete {
		t.Fatalf("source clocks/coverage lost: %+v", report)
	}
	wantOrder := []string{"The Kraken Cartel", "TopStats Logistics", "VoC", "Azur Lane", "Meridian Commerce", "TescoValue", "Vekbeard's Seedbox Shipping", "TopStats AI Logistics"}
	var names []string
	for _, row := range report.Rivals {
		names = append(names, row.CompanyName)
	}
	if !reflect.DeepEqual(names, wantOrder) {
		t.Fatalf("attention order: %v, want %v", names, wantOrder)
	}
}

func TestAggregateRivalsAcceptanceBaselineAndSeasonTables(t *testing.T) {
	for _, test := range []struct {
		name     string
		offset   time.Duration
		season   int
		remove   bool
		first    time.Duration
		hunch    string
		measured bool
	}{
		{"one hour early inclusive", -time.Hour, 1, false, -48 * time.Hour, "losing_value", true},
		{"one hour late inclusive", time.Hour, 1, false, -48 * time.Hour, "losing_value", true},
		{"too early", -time.Hour - time.Second, 1, false, -48 * time.Hour, "unmeasured", false},
		{"too late", time.Hour + time.Second, 1, false, -48 * time.Hour, "unmeasured", false},
		{"previous season", 0, 0, false, -48 * time.Hour, "unmeasured", false},
		{"name absent from baseline", 0, 1, true, -48 * time.Hour, "unmeasured", false},
		{"short history", 0, 1, true, -5 * time.Hour, "too_new", false},
		{"exactly 24 hours is not too new", 0, 1, true, -24 * time.Hour, "unmeasured", false},
	} {
		t.Run(test.name, func(t *testing.T) {
			input := rivalsAcceptanceFixture()
			var valuations []RivalValuation
			for _, row := range input.Standings {
				if test.season == 0 {
					row.SeasonNumber = 2
				}
				if row.CompanyName == "Azur Lane" {
					row.FirstAt = input.At.Add(test.first)
					if row.RefreshedAt.Before(input.At) {
						if test.remove {
							continue
						}
						row.RefreshedAt = row.RefreshedAt.Add(test.offset)
						row.SeasonNumber = test.season
						if test.season == 0 {
							row.SeasonNumber = 1
						}
					}
				} else if row.RefreshedAt.Before(input.At) {
					continue
				}
				valuations = append(valuations, row)
			}
			input.Standings = valuations
			row := rivalsAcceptanceFind(t, rivalsAcceptanceDecode(t, AggregateRivals(input)), "Azur Lane")
			if row.Hunch != test.hunch || (row.ChangePence != nil) != test.measured {
				t.Fatalf("baseline availability: %+v", row)
			}
			if test.hunch == "unmeasured" && row.Fact.Kind != "no_baseline" {
				t.Fatalf("missing-baseline fact: %+v", row)
			}
		})
	}
}

func TestAggregateRivalsAcceptanceIdentityAndUnknownOverlap(t *testing.T) {
	for _, mode := range []string{"complete", "incomplete owners", "no own traffic", "no own valuation"} {
		t.Run(mode, func(t *testing.T) {
			input := rivalsAcceptanceFixture()
			input.EventsTruncated = true
			input.Standings = append(input.Standings, RivalValuation{SeasonNumber: 1, Rank: 9, CompanyName: "Our second company", RefreshedAt: input.At, NetWorthPence: 999_999_999})
			input.Standings = append(input.Standings, RivalValuation{
				SeasonNumber: 1, Rank: 10, CompanyName: "GLM-5.3-Flash",
				RefreshedAt: input.At.Add(-48 * time.Hour), NetWorthPence: 2_510_616,
			})
			if mode == "incomplete owners" {
				input.OwnCompaniesComplete = false
			}
			if mode == "no own traffic" {
				var notices []gameapi.SailingNotice
				for _, notice := range input.Sailings {
					if notice.CompanyName != input.OurCompanyName {
						notices = append(notices, notice)
					}
				}
				input.Sailings = notices
			}
			if mode == "no own valuation" {
				var rows []RivalValuation
				for _, row := range input.Standings {
					if row.CompanyName != input.OurCompanyName {
						rows = append(rows, row)
					}
				}
				input.Standings = rows
			}
			report := rivalsAcceptanceDecode(t, AggregateRivals(input))
			if !report.EventsTruncated || report.OwnCompaniesComplete != input.OwnCompaniesComplete {
				t.Fatalf("coverage flags were lost: %+v", report)
			}
			for _, row := range report.Rivals {
				if row.CompanyName == input.OurCompanyName || row.CompanyName == "Our second company" {
					t.Fatalf("configured owner leaked into rivals: %+v", row)
				}
				if row.CompanyName == "GLM-5.3-Flash" {
					t.Fatalf("historical-only name with no current traffic became a current rival: %+v", row)
				}
				if mode == "incomplete owners" || mode == "no own traffic" {
					if row.SharedLanes != nil || row.SharedPorts != nil {
						t.Fatalf("unknown overlap became zero: %+v", row)
					}
				}
				if mode == "no own valuation" && (row.Hunch == "threat" || row.Hunch == "most_dangerous") {
					t.Fatalf("comparison without our valuation: %+v", row)
				}
			}
			if mode == "incomplete owners" {
				if report.Verdicts.MostDangerous.CompanyName != nil || report.Verdicts.MostDangerous.Reason != "own_companies_incomplete" {
					t.Fatalf("incomplete ownership must withhold attributed verdict: %+v", report.Verdicts.MostDangerous)
				}
			} else if mode == "no own traffic" {
				kraken := rivalsAcceptanceFind(t, report, "The Kraken Cartel")
				if kraken.Hunch != "most_dangerous" || kraken.Fact.Kind != "larger" {
					t.Fatalf("unknown-overlap fallback: %+v", kraken)
				}
			}
		})
	}
}

func TestAggregateRivalsAcceptanceHunchPrecedenceTable(t *testing.T) {
	for _, test := range []struct {
		name     string
		worth    int64
		baseline int64
		measured bool
		history  time.Duration
		credit   bool
		ranked   bool
		hunch    string
	}{
		{"most dangerous beats rough play", 3000, 4000, true, 48 * time.Hour, true, true, "most_dangerous"},
		{"rough play beats threat", 1500, 1600, true, 48 * time.Hour, true, true, "plays_rough"},
		{"equal size and direct port means threat", 1000, 1500, true, 48 * time.Hour, false, true, "threat"},
		{"falling beats positive current worth", 900, 950, true, 48 * time.Hour, false, true, "losing_value"},
		{"positive change", 900, 800, true, 48 * time.Hour, false, true, "growing"},
		{"measured zero", 900, 900, true, 48 * time.Hour, false, true, "steady"},
		{"short history", 900, 0, false, 5 * time.Hour, false, true, "too_new"},
		{"old history but no baseline", 900, 0, false, 48 * time.Hour, false, true, "unmeasured"},
		{"traffic only", 0, 0, false, 0, false, false, "not_ranked"},
	} {
		t.Run(test.name, func(t *testing.T) {
			input := rivalsAcceptanceFixture()
			input.Standings = []RivalValuation{
				{SeasonNumber: 1, Rank: 1, CompanyName: "Leader", RefreshedAt: input.At, FirstAt: input.At.Add(-48 * time.Hour), NetWorthPence: 2000},
				{SeasonNumber: 1, Rank: 2, CompanyName: input.OurCompanyName, RefreshedAt: input.At, FirstAt: input.At.Add(-48 * time.Hour), NetWorthPence: 1000},
			}
			if test.ranked {
				input.Standings = append(input.Standings, RivalValuation{
					SeasonNumber: 1, Rank: 3, CompanyName: "Subject",
					RefreshedAt: input.At, FirstAt: input.At.Add(-test.history), NetWorthPence: test.worth,
				})
				if test.measured {
					input.Standings = append(input.Standings, RivalValuation{
						SeasonNumber: 1, Rank: 3, CompanyName: "Subject",
						RefreshedAt: input.At.Add(-24 * time.Hour), FirstAt: input.At.Add(-test.history), NetWorthPence: test.baseline,
					})
				}
			}
			input.Sailings = nil
			for _, name := range []string{input.OurCompanyName, "Leader", "Subject"} {
				input.Sailings = append(input.Sailings, gameapi.SailingNotice{
					CompanyName: name, ShipName: "Swift", PortID: "p0", Direction: "departed", SeenAt: input.At,
				})
			}
			if test.credit {
				input.Evidence = []RivalEvidence{rivalsAcceptanceEvidence("Subject", input.At, 2, 1)}
			}
			report := rivalsAcceptanceDecode(t, AggregateRivals(input))
			subject := rivalsAcceptanceFind(t, report, "Subject")
			if subject.Hunch != test.hunch {
				t.Fatalf("one selected hunch: %+v, want %s", subject, test.hunch)
			}
			if test.hunch == "threat" && (subject.Fact.Kind != "shared_ports" || subject.Fact.Count != 1 || subject.Fact.Total != 1) {
				t.Fatalf("port-only overlap is measured without inventing lanes: %+v", subject)
			}
		})
	}
}

func TestAggregateRivalsAcceptanceKnownNoOverlapDoesNotFallback(t *testing.T) {
	input := rivalsAcceptanceFixture()
	input.Sailings = []gameapi.SailingNotice{
		{CompanyName: input.OurCompanyName, ShipName: "Lapras", PortID: "p0", SeenAt: input.At},
		{CompanyName: "The Kraken Cartel", ShipName: "Swift", PortID: "p1", SeenAt: input.At},
	}
	report := rivalsAcceptanceDecode(t, AggregateRivals(input))
	if report.Verdicts.MostDangerous.CompanyName != nil {
		t.Fatalf("known absence of overlap invoked unknown-overlap fallback: %+v", report.Verdicts.MostDangerous)
	}
	for _, row := range report.Rivals {
		if row.Hunch == "most_dangerous" || row.Hunch == "threat" {
			t.Fatalf("size without observed overlap became a threat: %+v", row)
		}
	}
}

func TestAggregateRivalsAcceptanceChoosesNearestPublication(t *testing.T) {
	input := rivalsAcceptanceFixture()
	for i := range input.Standings {
		if input.Standings[i].RefreshedAt.Before(input.At) {
			input.Standings[i].RefreshedAt = input.At.Add(-25 * time.Hour)
		}
	}
	near := input.At.Add(-24*time.Hour + 10*time.Minute)
	input.Standings = append(input.Standings, RivalValuation{
		SeasonNumber: 1, Rank: 7, CompanyName: "Azur Lane", RefreshedAt: near,
		FirstAt: input.At.Add(-48 * time.Hour), NetWorthPence: 100_000,
	})
	report := rivalsAcceptanceDecode(t, AggregateRivals(input))
	row := rivalsAcceptanceFind(t, report, "Azur Lane")
	if report.BaselineAt == nil || !report.BaselineAt.Equal(near) || row.ChangePence == nil || *row.ChangePence != -5935 {
		t.Fatalf("baseline did not use nearest publication: %+v, %+v", report.BaselineAt, row)
	}
	// A publication is selected globally, not a different baseline per rival.
	if row := rivalsAcceptanceFind(t, report, "VoC"); row.ChangePence != nil {
		t.Fatalf("absent name fell back to a different publication: %+v", row)
	}
}

func TestAggregateRivalsAcceptanceLaneIdentityAndDirectPorts(t *testing.T) {
	input := rivalsAcceptanceFixture()
	a, b := "p0", "p1"
	input.Sailings = []gameapi.SailingNotice{
		{CompanyName: input.OurCompanyName, ShipName: "Lapras", PortID: a, OtherPortID: &b, Direction: "departed", SeenAt: input.At},
		{CompanyName: "The Kraken Cartel", ShipName: "Swift", PortID: b, OtherPortID: &a, Direction: "departed", SeenAt: input.At},
	}
	input.Sailings = append(input.Sailings, input.Sailings[1])
	input.Ports[1].Sailings = []gameapi.SailingNotice{input.Sailings[1]}
	future, old := input.Sailings[1], input.Sailings[1]
	future.SeenAt, old.SeenAt = input.At.Add(time.Second), input.At.Add(-24*time.Hour-time.Second)
	future.CompanyName, old.CompanyName = "Future phantom", "Old phantom"
	input.Sailings = append(input.Sailings, future, old)
	report := rivalsAcceptanceDecode(t, AggregateRivals(input))
	row := rivalsAcceptanceFind(t, report, "The Kraken Cartel")
	if row.SharedLanes == nil || *row.SharedLanes != 1 || row.SharedPorts == nil || *row.SharedPorts != 0 || row.Fact.Count != 1 || row.Fact.Total != 1 {
		t.Fatalf("lane direction/destination/duplicate handling: %+v", row)
	}
	for _, row := range report.Rivals {
		if strings.Contains(row.CompanyName, "phantom") {
			t.Fatalf("outside upstream window: %+v", row)
		}
	}
}

func rivalsAcceptanceEvidence(name string, at time.Time, defaults, walkaways int) RivalEvidence {
	raw, _ := json.Marshal(map[string]any{"borrower_name": name, "borrower_company_id": "id-" + name, "borrower_record": map[string]int{"loans_defaulted": defaults, "loans_walked_away": walkaways}})
	return RivalEvidence{ID: name + at.String(), Type: "loan_request_posted", At: at, Payload: raw}
}

func TestAggregateRivalsAcceptanceConductEvidencePrecedence(t *testing.T) {
	for _, test := range []struct {
		name                        string
		snapshot                    bool
		grants, defaults, walkaways int
		kind                        string
	}{
		{"fresh monopoly wins", true, 4, 3, 2, "monopoly_in_force"},
		{"grants outrank credit", false, 4, 3, 2, "monopolies_recent"},
		{"walkaways are not added to defaults", false, 0, 3, 2, "loan_walkaways"},
		{"defaults", false, 0, 3, 0, "loan_defaults"},
	} {
		t.Run(test.name, func(t *testing.T) {
			input := rivalsAcceptanceFixture()
			input.Evidence = []RivalEvidence{rivalsAcceptanceEvidence("TescoValue", input.At.Add(-time.Hour), test.defaults, test.walkaways)}
			for i := range test.grants {
				input.Evidence = append(input.Evidence, RivalEvidence{
					ID: fmt.Sprint(i), Type: "monopoly_granted", At: input.At.Add(-2 * time.Hour),
					Payload: json.RawMessage(`{"holder_company_name":"TescoValue","holder_company_id":"id-TescoValue"}`),
				})
			}
			if test.grants > 0 {
				input.Evidence = append(input.Evidence, input.Evidence[len(input.Evidence)-1])
			}
			if test.snapshot {
				input.Monopolies = json.RawMessage(`[{"holder_company_name":"TescoValue","holder_company_id":"id-TescoValue","commodity_id":"raw_wool","commodity_name":"Raw wool","banking_region":"channel","region_name":"Channel","in_force":true,"expires_at":"2026-10-27T21:28:00Z"}]`)
				input.MonopoliesObservedAt, input.MonopoliesComplete = input.At.Add(-30*time.Minute), true
			}
			report := rivalsAcceptanceDecode(t, AggregateRivals(input))
			row := rivalsAcceptanceFind(t, report, "TescoValue")
			if row.Hunch != "plays_rough" || row.Fact.Kind != test.kind || report.Verdicts.PlaysRough.CompanyName == nil || *report.Verdicts.PlaysRough.CompanyName != "TescoValue" {
				t.Fatalf("conduct precedence: %+v, %+v", row, report.Verdicts.PlaysRough)
			}
			if test.kind == "monopolies_recent" && row.Fact.Count != 4 || test.kind == "loan_walkaways" && row.Fact.Count != 2 || test.kind == "loan_defaults" && row.Fact.Count != 3 {
				t.Fatalf("evidence counter inflated: %+v", row.Fact)
			}
			if test.kind == "monopoly_in_force" && (row.Fact.Commodity != "raw_wool" || row.Fact.Region != "channel" || row.Fact.ExpiresAt == nil || row.Fact.ExpiresAt.Format(time.RFC3339) != "2026-10-27T21:28:00Z") {
				t.Fatalf("current monopoly lost recorded scope or expiry: %+v", row.Fact)
			}
		})
	}
}

func TestAggregateRivalsAcceptanceConductVerdictUsesStrongestEvidence(t *testing.T) {
	for _, test := range []struct {
		name      string
		monopoly  bool
		grants    bool
		walkaways bool
		want      string
	}{
		{"current monopoly before grant history", true, true, true, "TescoValue"},
		{"grant history before credit", false, true, true, "The Kraken Cartel"},
		{"walkaways before defaults", false, false, true, "TopStats Logistics"},
		{"defaults remain attributable", false, false, false, "Meridian Commerce"},
	} {
		t.Run(test.name, func(t *testing.T) {
			input := rivalsAcceptanceFixture()
			input.Evidence = []RivalEvidence{
				rivalsAcceptanceEvidence("Meridian Commerce", input.At, 20, 0),
			}
			if test.walkaways {
				input.Evidence = append(input.Evidence, rivalsAcceptanceEvidence("TopStats Logistics", input.At, 1, 1))
			}
			if test.grants {
				input.Evidence = append(input.Evidence, RivalEvidence{
					ID: "grant", Type: "monopoly_granted", At: input.At.Add(-7 * 24 * time.Hour),
					Payload: json.RawMessage(`{"holder_company_name":"The Kraken Cartel","holder_company_id":"kraken"}`),
				})
			}
			if test.monopoly {
				input.Monopolies = json.RawMessage(`[{"holder_company_name":"TescoValue","in_force":true,"commodity_id":"raw_wool","commodity_name":"Raw wool","banking_region":"channel","region_name":"Channel","expires_at":"2026-10-27T21:28:00Z"}]`)
				input.MonopoliesComplete = true
				input.MonopoliesObservedAt = input.At
			}
			report := rivalsAcceptanceDecode(t, AggregateRivals(input))
			if report.Verdicts.PlaysRough.CompanyName == nil || *report.Verdicts.PlaysRough.CompanyName != test.want {
				t.Fatalf("strongest available conduct evidence: %+v, want %s", report.Verdicts.PlaysRough, test.want)
			}
		})
	}
}

func TestAggregateRivalsAcceptanceRejectsStaleExpiredMissingAndAnonymousEvidence(t *testing.T) {
	for _, mode := range []string{"stale", "future snapshot", "incomplete", "error", "expired", "not in force", "zero credit", "missing credit", "latest credit wins", "older than seven days", "future credit", "anonymous holder", "anonymous marque"} {
		t.Run(mode, func(t *testing.T) {
			input := rivalsAcceptanceFixture()
			input.MonopoliesObservedAt, input.MonopoliesComplete = input.At.Add(-time.Minute), true
			monopoly := map[string]any{"holder_company_name": "TescoValue", "in_force": true, "expires_at": input.At.Add(time.Hour), "commodity_id": "raw_wool", "banking_region": "channel"}
			switch mode {
			case "stale":
				input.MonopoliesObservedAt = input.At.Add(-30*time.Minute - time.Second)
			case "future snapshot":
				input.MonopoliesObservedAt = input.At.Add(time.Second)
			case "incomplete":
				input.MonopoliesComplete = false
			case "error":
				input.MonopoliesError = "recorded read failure"
			case "expired":
				monopoly["expires_at"] = input.At
			case "not in force":
				monopoly["in_force"] = false
			case "anonymous holder":
				delete(monopoly, "holder_company_name")
				monopoly["holder_company_id"] = "unmapped"
			default:
				monopoly = nil
				switch mode {
				case "zero credit":
					input.Evidence = []RivalEvidence{rivalsAcceptanceEvidence("TescoValue", input.At, 0, 0)}
				case "missing credit":
					input.Evidence = []RivalEvidence{{
						ID: "missing", Type: "loan_request_posted", At: input.At,
						Payload: json.RawMessage(`{"borrower_name":"TescoValue","borrower_company_id":"id-TescoValue"}`),
					}}
				case "latest credit wins":
					input.Evidence = []RivalEvidence{
						rivalsAcceptanceEvidence("TescoValue", input.At.Add(-time.Hour), 4, 2),
						rivalsAcceptanceEvidence("TescoValue", input.At, 0, 0),
					}
				case "older than seven days":
					input.Evidence = []RivalEvidence{rivalsAcceptanceEvidence("TescoValue", input.At.Add(-7*24*time.Hour-time.Second), 4, 2)}
				case "future credit":
					input.Evidence = []RivalEvidence{rivalsAcceptanceEvidence("TescoValue", input.At.Add(time.Second), 4, 2)}
				case "anonymous marque":
					input.Evidence = []RivalEvidence{{
						ID: "marque", Type: "letter_of_marque_issued", At: input.At,
						Payload: json.RawMessage(`{"company_id":null,"zone_id":"channel"}`),
					}}
				}
			}
			if monopoly != nil {
				input.Monopolies, _ = json.Marshal([]any{monopoly})
			}
			report := rivalsAcceptanceDecode(t, AggregateRivals(input))
			if report.Verdicts.PlaysRough.CompanyName != nil || report.Verdicts.PlaysRough.Reason != "none_recorded" || rivalsAcceptanceFind(t, report, "TescoValue").Hunch != "growing" {
				t.Fatalf("unsupported conduct or false clean record: %+v", report)
			}
		})
	}
}

func TestAggregateRivalsAcceptanceProfitabilityRanking(t *testing.T) {
	for _, test := range []struct {
		name   string
		values [][2]int64
		want   string
	}{
		{"most negative before percentage", [][2]int64{{100, -10}, {100, -20}, {100, 1}}, "B"},
		{"relative decline before absolute loss", [][2]int64{{1000, 500}, {100, 1}, {10000, 9000}}, "B"},
		{"zero and negative baselines cannot supply a rate", [][2]int64{{0, 10}, {-100, 20}, {100, 90}}, "C"},
		{"no decline", [][2]int64{{100, 100}, {100, 101}, {0, 1}}, ""},
	} {
		t.Run(test.name, func(t *testing.T) {
			input := rivalsAcceptanceFixture()
			input.Standings, input.Sailings = nil, nil
			for i, pair := range test.values {
				for endpoint, worth := range pair {
					input.Standings = append(input.Standings, RivalValuation{SeasonNumber: 1, Rank: i + 1, CompanyName: string(rune('A' + i)), RefreshedAt: input.At.Add(time.Duration(endpoint-1) * 24 * time.Hour), FirstAt: input.At.Add(-48 * time.Hour), NetWorthPence: worth})
				}
			}
			report := rivalsAcceptanceDecode(t, AggregateRivals(input))
			got := report.Verdicts.NotProfitable.CompanyName
			if test.want == "" {
				if got != nil {
					t.Fatalf("invented unprofitable verdict: %v", *got)
				}
			} else if got == nil || *got != test.want {
				t.Fatalf("not_profitable: %+v, want %s", report.Verdicts.NotProfitable, test.want)
			}
		})
	}
}

func TestRivalsAcceptanceRecordedDatabaseFixture(t *testing.T) {
	database, _, _ := integrationStore(t) // Skips unless TEST_DATABASE_URL is set.
	ctx := context.Background()
	credentials := saveTestAccount(t, database, "one")
	input := rivalsAcceptanceFixture()
	if err := database.SaveSnapshot(ctx, "one", map[string]json.RawMessage{
		"profile": json.RawMessage(`{"company":{"id":"company-one","name":"The Pokemon Pirates"}}`),
		"catalog": json.RawMessage(`{"ports":[{"id":"p0","name":"Port 0"},{"id":"p1","name":"Port 1"},{"id":"p2","name":"Port 2"},{"id":"p3","name":"Port 3"},{"id":"p4","name":"Port 4"},{"id":"p5","name":"Port 5"},{"id":"p6","name":"Port 6"},{"id":"p7","name":"Port 7"},{"id":"p8","name":"Port 8"}]}`),
	}); err != nil {
		t.Fatal(err)
	}
	for _, value := range input.Standings {
		row := standingRow{
			SeasonNumber: value.SeasonNumber, RefreshedAt: value.RefreshedAt, Rank: value.Rank,
			CompanyName: value.CompanyName, NetWorthPence: value.NetWorthPence,
			Titles: json.RawMessage(`[]`), NextRefreshAt: value.NextRefreshAt, ObservedAt: input.At,
		}
		if err := database.db.Create(&row).Error; err != nil {
			t.Fatal(err)
		}
	}
	// The five-hour first observation must be loaded from permanent history.
	first := standingRow{
		SeasonNumber: 1, RefreshedAt: input.At.Add(-5 * time.Hour), Rank: 4,
		CompanyName: "Vekbeard's Seedbox Shipping", NetWorthPence: 2_000_000,
		Titles: json.RawMessage(`[]`), ObservedAt: input.At,
	}
	if err := database.db.Create(&first).Error; err != nil {
		t.Fatal(err)
	}
	if err := database.db.Create(&standingAccount{AccountID: "one", SeasonNumber: 1, RefreshedAt: input.At, Rank: 3}).Error; err != nil {
		t.Fatal(err)
	}
	for i, notice := range input.Sailings {
		payload, err := json.Marshal(map[string]any{"timestamp": notice.SeenAt, "payload": notice})
		if err != nil {
			t.Fatal(err)
		}
		event := Event{AccountID: "one", EventID: fmt.Sprint(i), EventType: "ship_sailed", Payload: payload, CreatedAt: notice.SeenAt}
		if err := database.db.Create(&event).Error; err != nil {
			t.Fatal(err)
		}
	}
	grantAt := input.At.Add(-2 * time.Hour)
	grantPayload, err := json.Marshal(map[string]any{
		"timestamp": grantAt,
		"payload":   map[string]string{"holder_company_name": "TescoValue", "holder_company_id": "tesco"},
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := database.db.Create(&Event{
		AccountID: "one", EventID: "tesco-grant", EventType: "monopoly_granted",
		Payload: grantPayload, CreatedAt: grantAt,
	}).Error; err != nil {
		t.Fatal(err)
	}
	value, err := database.Rivals(ctx, "one", input.At)
	if err != nil {
		t.Fatal(err)
	}
	report := rivalsAcceptanceDecode(t, value)
	tesco := rivalsAcceptanceFind(t, report, "TescoValue")
	if tesco.Hunch != "plays_rough" || tesco.Fact.Kind != "monopolies_recent" || tesco.Fact.Count != 1 {
		t.Fatalf("retained conduct event not projected: %+v", tesco)
	}
	kraken := rivalsAcceptanceFind(t, report, "The Kraken Cartel")
	if kraken.ChangePence == nil || *kraken.ChangePence != -11_764_585 || kraken.SharedLanes == nil || *kraken.SharedLanes != 14 || kraken.Fact.Total != 16 || kraken.Hunch != "most_dangerous" {
		t.Fatalf("database projection disagrees with pure fixture: %+v", kraken)
	}
	vekbeard := rivalsAcceptanceFind(t, report, "Vekbeard's Seedbox Shipping")
	if vekbeard.Hunch != "too_new" || vekbeard.ChangePence != nil || vekbeard.Fact.Hours != 5 {
		t.Fatalf("first seasonal valuation not loaded: %+v", vekbeard)
	}
	if _, err := database.Rivals(ctx, "missing", input.At); !errors.Is(err, ErrNotFound) {
		t.Fatalf("unknown account: %v", err)
	}
	// A recorded name containing credentials is scrubbed on every output surface.
	if err := database.db.Model(&standingRow{}).Where("company_name = ?", "TescoValue").Update("company_name", "Tesco "+credentials.APIKey).Error; err != nil {
		t.Fatal(err)
	}
	value, err = database.Rivals(ctx, "one", input.At)
	if err != nil {
		t.Fatal(err)
	}
	raw, err := json.Marshal(value)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(raw), credentials.APIKey) {
		t.Fatal("rival report leaked a recorded credential")
	}
	var definition string
	if err := database.db.Raw("SELECT indexdef FROM pg_indexes WHERE schemaname = current_schema() AND indexname = 'events_rival_evidence_idx'").Scan(&definition).Error; err != nil {
		t.Fatal(err)
	}
	for _, fragment := range []string{"account_id", "created_at DESC", "id DESC", "monopoly_granted", "loan_request_posted", "WHERE"} {
		if !strings.Contains(definition, fragment) {
			t.Fatalf("missing bounded evidence index clause %q: %s", fragment, definition)
		}
	}
}
