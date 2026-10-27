package store

import (
	"bytes"
	"encoding/json"
	"math"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/DevYukine/go-tradewinds/internal/gameapi"
)

func rivalTestInput() RivalsInput {
	at := time.Date(2026, 10, 27, 2, 28, 22, 0, time.UTC)
	return RivalsInput{
		CompetitionInput: CompetitionInput{
			AccountID: "one", At: at, OwnCompanies: []string{"Our active company"}, OwnCompaniesComplete: true,
			Ports: []CompetitionPortObservation{{ID: "a"}, {ID: "b"}, {ID: "c"}},
			Sailings: []gameapi.SailingNotice{
				{CompanyName: "Our active company", ShipName: "Lapras", PortID: "a", SeenAt: at},
			},
		},
		OurCompanyName: "Our active company",
		Standings: []RivalValuation{
			{SeasonNumber: 1, Rank: 2, CompanyName: "Our active company", RefreshedAt: at, FirstAt: at.Add(-48 * time.Hour), NetWorthPence: 100},
		},
	}
}

func rivalTestJSON(t *testing.T, value any) json.RawMessage {
	t.Helper()
	raw, err := json.Marshal(value)
	if err != nil {
		t.Fatal(err)
	}
	return raw
}

func rivalTestRow(t *testing.T, report RivalReport, name string) RivalRow {
	t.Helper()
	for _, row := range report.Rivals {
		if row.CompanyName == name {
			return row
		}
	}
	t.Fatalf("missing company %q: %+v", name, report.Rivals)
	return RivalRow{}
}

func TestAggregateRivalsConsumerExactPenceAndOverflow(t *testing.T) {
	for _, test := range []struct {
		name                      string
		current, baseline, change int64
		measured                  bool
	}{
		{"above floating point precision", 9_007_199_254_740_993, 9_007_199_254_741_050, -57, true},
		{"largest representable decline", 0, math.MaxInt64, -math.MaxInt64, true},
		{"positive overflow is unknown", math.MaxInt64, math.MinInt64, 0, false},
		{"negative overflow is unknown", math.MinInt64, math.MaxInt64, 0, false},
	} {
		t.Run(test.name, func(t *testing.T) {
			input := rivalTestInput()
			input.Standings = append(input.Standings,
				RivalValuation{SeasonNumber: 1, Rank: 1, CompanyName: "Subject", RefreshedAt: input.At, FirstAt: input.At.Add(-48 * time.Hour), NetWorthPence: test.current},
				RivalValuation{SeasonNumber: 1, Rank: 1, CompanyName: "Subject", RefreshedAt: input.At.Add(-24 * time.Hour), NetWorthPence: test.baseline},
			)
			report := AggregateRivals(input)
			row := rivalTestRow(t, report, "Subject")
			if row.NetWorthPence == nil || *row.NetWorthPence != test.current || row.BaselinePence == nil || *row.BaselinePence != test.baseline ||
				(row.ChangePence != nil) != test.measured || row.ChangePence != nil && *row.ChangePence != test.change {
				t.Fatalf("pence were rounded or wrapped: %+v", row)
			}
			var decoded struct {
				Rivals []struct {
					NetWorthPence int64  `json:"net_worth_pence"`
					ChangePence   *int64 `json:"change_pence"`
				} `json:"rivals"`
			}
			if err := json.Unmarshal(rivalTestJSON(t, report), &decoded); err != nil {
				t.Fatal(err)
			}
			if len(decoded.Rivals) != 1 || decoded.Rivals[0].NetWorthPence != test.current || !reflect.DeepEqual(decoded.Rivals[0].ChangePence, row.ChangePence) {
				t.Fatalf("wire lost exact integer endpoints: %+v", decoded)
			}
		})
	}
}

func TestAggregateRivalsConsumerTrafficUsesReadClockNotStaleValuation(t *testing.T) {
	input := rivalTestInput()
	published := input.At.Add(-2 * time.Hour)
	next := published.Add(time.Hour)
	input.Standings[0].RefreshedAt = published
	input.Standings = append(input.Standings,
		RivalValuation{SeasonNumber: 1, Rank: 1, CompanyName: "Subject", RefreshedAt: published, FirstAt: published.Add(-48 * time.Hour), NetWorthPence: 200, NextRefreshAt: &next},
		RivalValuation{SeasonNumber: 1, Rank: 1, CompanyName: "Subject", RefreshedAt: published.Add(-24 * time.Hour), NetWorthPence: 250},
		RivalValuation{SeasonNumber: 2, Rank: 1, CompanyName: "Future season", RefreshedAt: input.At.Add(time.Second), NetWorthPence: 9999},
	)
	input.Sailings = append(input.Sailings,
		gameapi.SailingNotice{CompanyName: "Subject", ShipName: "New activity", PortID: "a", SeenAt: input.At.Add(-time.Minute)},
		gameapi.SailingNotice{CompanyName: "At lower bound", ShipName: "Swift", PortID: "b", SeenAt: input.At.Add(-CompetitionWindow)},
		gameapi.SailingNotice{CompanyName: "Old phantom", ShipName: "Swift", PortID: "b", SeenAt: input.At.Add(-CompetitionWindow - time.Nanosecond)},
		gameapi.SailingNotice{CompanyName: "Future phantom", ShipName: "Swift", PortID: "b", SeenAt: input.At.Add(time.Nanosecond)},
	)
	report := AggregateRivals(input)
	if report.SeasonNumber != 1 || report.ValuationAt == nil || !report.ValuationAt.Equal(published) || report.NextRefreshAt == nil || !report.NextRefreshAt.Equal(next) || !report.From.Equal(input.At.Add(-CompetitionWindow)) || !report.To.Equal(input.At) {
		t.Fatalf("publication, expiry or traffic clocks were conflated: %+v", report)
	}
	row := rivalTestRow(t, report, "Subject")
	if row.Hunch != rivalMostDangerous || row.ChangePence == nil || *row.ChangePence != -50 || row.Fact.Kind != "shared_ports" {
		t.Fatalf("recent traffic was discarded with the old publication: %+v", row)
	}
	rivalTestRow(t, report, "At lower bound")
	for _, row := range report.Rivals {
		if strings.Contains(row.CompanyName, "phantom") || row.CompanyName == "Future season" {
			t.Fatalf("invalid upstream time entered the report: %+v", row)
		}
	}
}

func TestAggregateRivalsConsumerBaselineTieIsGlobalAndDeterministic(t *testing.T) {
	input := rivalTestInput()
	for i, name := range []string{"Early", "Late"} {
		input.Standings = append(input.Standings, RivalValuation{
			SeasonNumber: 1, Rank: i + 3, CompanyName: name, RefreshedAt: input.At,
			FirstAt: input.At.Add(-48 * time.Hour), NetWorthPence: 80,
		})
	}
	input.Standings = append(input.Standings,
		RivalValuation{SeasonNumber: 1, Rank: 3, CompanyName: "Early", RefreshedAt: input.At.Add(-25 * time.Hour), NetWorthPence: 100},
		RivalValuation{SeasonNumber: 1, Rank: 4, CompanyName: "Late", RefreshedAt: input.At.Add(-23 * time.Hour), NetWorthPence: 100},
	)
	for range 2 {
		report := AggregateRivals(input)
		early, late := rivalTestRow(t, report, "Early"), rivalTestRow(t, report, "Late")
		if report.BaselineAt == nil || !report.BaselineAt.Equal(input.At.Add(-25*time.Hour)) || early.ChangePence == nil || *early.ChangePence != -20 || early.Fact.Kind != "value_change" || early.Fact.Hours != nil || late.ChangePence != nil || late.Hunch != rivalUnmeasured {
			t.Fatalf("tied publication choice depended on company/input order: %+v", report)
		}
		for left, right := 0, len(input.Standings)-1; left < right; left, right = left+1, right-1 {
			input.Standings[left], input.Standings[right] = input.Standings[right], input.Standings[left]
		}
	}
}

func TestAggregateRivalsConsumerHeadlinesAndOrderSurviveInputPermutations(t *testing.T) {
	input := rivalTestInput()
	for i, value := range []struct {
		name  string
		worth int64
	}{{"Zulu large", 1000}, {"Alpha debtor", -20}, {"Beta credit", 250}, {"Equal A", 50}, {"Equal B", 50}} {
		input.Standings = append(input.Standings,
			RivalValuation{SeasonNumber: 1, Rank: i + 3, CompanyName: value.name, RefreshedAt: input.At, FirstAt: input.At.Add(-48 * time.Hour), NetWorthPence: value.worth},
			RivalValuation{SeasonNumber: 1, Rank: i + 3, CompanyName: value.name, RefreshedAt: input.At.Add(-24 * time.Hour), NetWorthPence: 100},
		)
		input.Sailings = append(input.Sailings, gameapi.SailingNotice{CompanyName: value.name, ShipName: "Swift", PortID: "a", SeenAt: input.At})
	}
	input.Evidence = []RivalEvidence{{
		ID: "credit", Type: "loan_request_posted", At: input.At,
		Payload: json.RawMessage(`{"borrower_name":"Beta credit","borrower_company_id":"beta","borrower_record":{"loans_defaulted":3,"loans_walked_away":1}}`),
	}}
	want := AggregateRivals(input)
	for _, verdict := range []struct {
		value RivalVerdict
		name  string
		kind  string
	}{{want.Verdicts.MostDangerous, "Zulu large", "shared_ports"}, {want.Verdicts.PlaysRough, "Beta credit", "loan_walkaways"}, {want.Verdicts.NotProfitable, "Alpha debtor", "negative_worth"}} {
		if verdict.value.CompanyName == nil || *verdict.value.CompanyName != verdict.name || verdict.value.Fact == nil || verdict.value.Fact.Kind != verdict.kind {
			t.Fatalf("sorting changed the headline identity/fact: %+v", want.Verdicts)
		}
	}
	for range len(input.Standings) {
		first := input.Standings[0]
		copy(input.Standings, input.Standings[1:])
		input.Standings[len(input.Standings)-1] = first
		before := rivalTestJSON(t, input)
		got := AggregateRivals(input)
		if !reflect.DeepEqual(got, want) {
			t.Fatalf("unstable publication/headline/order: got %+v, want %+v", got, want)
		}
		if !bytes.Equal(before, rivalTestJSON(t, input)) {
			t.Fatal("pure aggregation mutated its caller's observations")
		}
	}
}

func TestAggregateRivalsConsumerLatestValidCreditUsesSourceTime(t *testing.T) {
	for _, test := range []struct {
		name, latest string
		kind         string
	}{
		{"new zero overrides old defaults", `{"loans_defaulted":0,"loans_walked_away":0}`, "no_baseline"},
		{"missing record is not a clean record", `null`, "loan_defaults"},
		{"missing counters are unknown", `{}`, "loan_defaults"},
		{"negative counters are invalid", `{"loans_defaulted":-1,"loans_walked_away":0}`, "loan_defaults"},
	} {
		t.Run(test.name, func(t *testing.T) {
			input := rivalTestInput()
			input.Standings = append(input.Standings, RivalValuation{SeasonNumber: 1, Rank: 3, CompanyName: "Subject", RefreshedAt: input.At, FirstAt: input.At.Add(-48 * time.Hour), NetWorthPence: 50})
			input.Evidence = []RivalEvidence{
				{ID: "a-new-source", Type: "loan_request_posted", At: input.At, Payload: json.RawMessage(`{"borrower_name":"Subject","borrower_company_id":"subject","borrower_record":` + test.latest + `}`)},
				{ID: "z-old-source", Type: "loan_request_posted", At: input.At.Add(-time.Hour), Payload: json.RawMessage(`{"borrower_name":"Subject","borrower_company_id":"subject","borrower_record":{"loans_defaulted":4,"loans_walked_away":0}}`)},
			}
			report := AggregateRivals(input)
			row := rivalTestRow(t, report, "Subject")
			if row.Fact.Kind != test.kind {
				t.Fatalf("latest valid credit was replaced by receipt/order or missing data: %+v", row)
			}
		})
	}
}

func TestAggregateRivalsConsumerMonopolyScopesAndSourceValidity(t *testing.T) {
	for _, test := range []struct {
		name   string
		change func(map[string]any, *RivalsInput)
		valid  bool
	}{
		{"actual payload identifiers", nil, true},
		{"future grant start", func(monopoly map[string]any, input *RivalsInput) { monopoly["starts_at"] = input.At.Add(time.Second) }, false},
		{"missing commodity", func(monopoly map[string]any, _ *RivalsInput) { delete(monopoly, "commodity_id") }, false},
		{"missing region", func(monopoly map[string]any, _ *RivalsInput) { delete(monopoly, "banking_region") }, false},
	} {
		t.Run(test.name, func(t *testing.T) {
			input := rivalTestInput()
			input.Standings = append(input.Standings, RivalValuation{SeasonNumber: 1, Rank: 3, CompanyName: "Subject", RefreshedAt: input.At, FirstAt: input.At.Add(-48 * time.Hour), NetWorthPence: 50})
			input.MonopoliesObservedAt, input.MonopoliesComplete = input.At, true
			monopoly := map[string]any{
				"holder_company_name": "Subject", "holder_company_id": "subject", "commodity_id": "raw_wool", "banking_region": "channel",
				"starts_at": input.At.Add(-time.Hour), "expires_at": input.At.Add(time.Hour), "in_force": true,
			}
			if test.change != nil {
				test.change(monopoly, &input)
			}
			input.Monopolies = rivalTestJSON(t, []any{monopoly})
			report := AggregateRivals(input)
			row := rivalTestRow(t, report, "Subject")
			if (row.Hunch == rivalPlaysRough) != test.valid {
				t.Fatalf("current monopoly validity: %+v", row)
			}
			if test.valid && (row.Fact.Commodity != "raw_wool" || row.Fact.Region != "channel" || row.Fact.ExpiresAt == nil || !row.Fact.ExpiresAt.Equal(input.At.Add(time.Hour))) {
				t.Fatalf("actual game fields lost their recorded scope: %+v", row.Fact)
			}
		})
	}
}

func TestAggregateRivalsConsumerExcludesDifferentBestCompanyIdentity(t *testing.T) {
	input := rivalTestInput()
	input.OurCompanyName = "Our published best company"
	input.Standings = append(input.Standings,
		RivalValuation{SeasonNumber: 1, Rank: 1, CompanyName: input.OurCompanyName, RefreshedAt: input.At, NetWorthPence: 1000},
		RivalValuation{SeasonNumber: 1, Rank: 3, CompanyName: "Subject", RefreshedAt: input.At, NetWorthPence: 1500},
	)
	input.Sailings = append(input.Sailings, gameapi.SailingNotice{CompanyName: "Subject", ShipName: "Swift", PortID: "a", SeenAt: input.At})
	report := AggregateRivals(input)
	if report.Our == nil || report.Our.CompanyName != input.OurCompanyName || len(report.Rivals) != 1 || report.Rivals[0].CompanyName != "Subject" || report.Rivals[0].Hunch != rivalMostDangerous {
		t.Fatalf("active company was confused with the player's published best company: %+v", report)
	}
}

func TestAggregateRivalsConsumerNoStandingsKeepsTrafficAndHonestGaps(t *testing.T) {
	input := rivalTestInput()
	input.Standings = nil
	input.Sailings = append(input.Sailings, gameapi.SailingNotice{CompanyName: "Traffic only", ShipName: "Swift", PortID: "a", SeenAt: input.At})
	report := AggregateRivals(input)
	row := rivalTestRow(t, report, "Traffic only")
	if report.ValuationAt != nil || report.BaselineAt != nil || report.NextRefreshAt != nil || report.SeasonNumber != 0 || report.Our != nil || row.Hunch != rivalNotRanked || row.NetWorthPence != nil || row.ChangePence != nil || report.Verdicts.MostDangerous.Reason != "our_valuation_missing" || report.Verdicts.NotProfitable.Reason != "no_baseline" {
		t.Fatalf("absence of standings became financial evidence: %+v", report)
	}
	input.Sailings, input.Ports = nil, nil
	empty := AggregateRivals(input)
	if empty.Rivals == nil || len(empty.Rivals) != 0 || empty.Coverage != "not_observed" || !strings.Contains(string(rivalTestJSON(t, empty)), `"rivals":[]`) {
		t.Fatalf("missing observations became null rows or observed zero coverage: %+v", empty)
	}
}

func TestAggregateRivalsConsumerScrubsRowsAndIndependentHeadlineFacts(t *testing.T) {
	input := rivalTestInput()
	apiKey, passphrase := "tw_live_never_expose_this_key", "never-expose-this-passphrase"
	ownName, roughName, dangerousName := "Ours "+apiKey, "Borrower "+apiKey, "Large "+apiKey
	input.OurCompanyName, input.OwnCompanies[0], input.Standings[0].CompanyName = ownName, ownName, ownName
	input.Sailings[0].CompanyName = ownName
	input.Standings = append(input.Standings,
		RivalValuation{SeasonNumber: 1, Rank: 1, CompanyName: dangerousName, RefreshedAt: input.At, NetWorthPence: 1000},
		RivalValuation{SeasonNumber: 1, Rank: 3, CompanyName: roughName, RefreshedAt: input.At, NetWorthPence: -50},
	)
	input.Sailings = append(input.Sailings, gameapi.SailingNotice{CompanyName: dangerousName, ShipName: "Swift", PortID: "a", SeenAt: input.At})
	input.MonopoliesComplete, input.MonopoliesObservedAt = true, input.At
	input.Monopolies = rivalTestJSON(t, []any{map[string]any{
		"holder_company_name": roughName, "commodity_id": "raw_wool_" + passphrase,
		"banking_region": "channel_" + apiKey, "in_force": true, "expires_at": input.At.Add(time.Hour),
	}})
	report := AggregateRivals(input)
	scrubRivals(&report, displayCredentials{available: true, secrets: []string{apiKey, passphrase}})
	raw := string(rivalTestJSON(t, report))
	if strings.Contains(raw, apiKey) || strings.Contains(raw, passphrase) {
		t.Fatalf("recorded credentials escaped through a row or headline: %s", raw)
	}
	if report.Our == nil || report.Verdicts.MostDangerous.CompanyName == nil || report.Verdicts.PlaysRough.Fact == nil || report.Verdicts.NotProfitable.CompanyName == nil ||
		!strings.Contains(report.Verdicts.PlaysRough.Fact.Commodity, redacted) || !strings.Contains(report.Verdicts.PlaysRough.Fact.Region, redacted) {
		t.Fatalf("scrubbing discarded evidence instead of redacting its text: %+v", report)
	}
}

func TestAggregateRivalsWithholdsAllVerdictsForIncompleteOwners(t *testing.T) {
	input := rivalTestInput()
	input.OwnCompaniesComplete = false
	input.Standings = append(input.Standings,
		RivalValuation{SeasonNumber: 1, Rank: 1, CompanyName: "Unresolved large company", RefreshedAt: input.At, NetWorthPence: 2000},
		RivalValuation{SeasonNumber: 1, Rank: 3, CompanyName: "Unresolved debtor", RefreshedAt: input.At, NetWorthPence: -500},
		RivalValuation{SeasonNumber: 1, Rank: 3, CompanyName: "Unresolved debtor", RefreshedAt: input.At.Add(-24 * time.Hour), NetWorthPence: 100},
	)
	input.Evidence = []RivalEvidence{{
		ID: "unresolved-credit", Type: "loan_request_posted", At: input.At,
		Payload: json.RawMessage(`{"borrower_name":"Unresolved debtor","borrower_company_id":"unresolved","borrower_record":{"loans_defaulted":4,"loans_walked_away":2}}`),
	}}
	report := AggregateRivals(input)
	if len(report.Rivals) != 2 || report.Coverage != "partial" || report.OverlapKnown {
		t.Fatalf("incomplete ownership hid observations or claimed measured overlap: %+v", report)
	}
	for _, verdict := range []RivalVerdict{report.Verdicts.MostDangerous, report.Verdicts.PlaysRough, report.Verdicts.NotProfitable} {
		if verdict.CompanyName != nil || verdict.Fact != nil || verdict.Reason != "own_companies_incomplete" {
			t.Fatalf("an unresolved own company could win a rival verdict: %+v", verdict)
		}
	}
	for _, row := range report.Rivals {
		if row.Hunch == rivalMostDangerous || row.SharedLanes != nil || row.SharedPorts != nil {
			t.Fatalf("incomplete exclusions became a winning or measured-overlap row: %+v", row)
		}
	}
}

func TestAggregateRivalsOverlapKnownIsIndependentOfCoverage(t *testing.T) {
	for _, mode := range []string{"observed own notices", "incomplete owners", "no own notices", "nothing observed"} {
		t.Run(mode, func(t *testing.T) {
			input := rivalTestInput()
			input.Sailings = append(input.Sailings, gameapi.SailingNotice{CompanyName: "Subject", ShipName: "Swift", PortID: "b", SeenAt: input.At})
			switch mode {
			case "incomplete owners":
				input.OwnCompaniesComplete = false
			case "no own notices":
				input.Sailings = input.Sailings[1:]
			case "nothing observed":
				input.Sailings = nil
			}
			report := AggregateRivals(input)
			if report.OverlapKnown != (mode == "observed own notices") {
				t.Fatalf("unknown overlap cause was lost: %+v", report)
			}
			coverage := "partial"
			if mode == "nothing observed" {
				coverage = "not_observed"
			}
			if report.Coverage != coverage {
				t.Fatalf("traffic observation was confused with owner completeness: %+v", report)
			}
			var wire map[string]json.RawMessage
			if err := json.Unmarshal(rivalTestJSON(t, report), &wire); err != nil {
				t.Fatal(err)
			}
			if _, exists := wire["overlap_known"]; !exists {
				t.Fatal("required overlap_known was omitted from the wire")
			}
			if mode != "nothing observed" {
				row := rivalTestRow(t, report, "Subject")
				if report.OverlapKnown && (row.SharedLanes == nil || row.SharedPorts == nil) ||
					!report.OverlapKnown && (row.SharedLanes != nil || row.SharedPorts != nil) {
					t.Fatalf("overlap values disagree with their availability flag: %+v", row)
				}
			}
		})
	}
}

func TestAggregateRivalsConductOutsideListedNamesHasHonestGap(t *testing.T) {
	for _, mode := range []string{"unlisted credit", "unlisted monopoly", "our credit only", "unlisted zero credit"} {
		t.Run(mode, func(t *testing.T) {
			input := rivalTestInput()
			input.Standings = append(input.Standings,
				RivalValuation{SeasonNumber: 1, Rank: 3, CompanyName: "Listed company", RefreshedAt: input.At, FirstAt: input.At.Add(-48 * time.Hour), NetWorthPence: 50},
				RivalValuation{SeasonNumber: 1, Rank: 3, CompanyName: "Unlisted company", RefreshedAt: input.At.Add(-48 * time.Hour), NetWorthPence: 100},
			)
			name, defaults := "Unlisted company", 3
			if mode == "our credit only" {
				name = input.OurCompanyName
			}
			if mode == "unlisted zero credit" {
				defaults = 0
			}
			input.Evidence = []RivalEvidence{{
				ID: "credit", Type: "loan_request_posted", At: input.At,
				Payload: rivalTestJSON(t, map[string]any{
					"borrower_name": name, "borrower_company_id": "borrower",
					"borrower_record": map[string]int{"loans_defaulted": defaults, "loans_walked_away": 0},
				}),
			}}
			if mode == "unlisted monopoly" {
				input.Evidence = nil
				input.MonopoliesComplete, input.MonopoliesObservedAt = true, input.At
				input.Monopolies = rivalTestJSON(t, []any{map[string]any{
					"holder_company_name": name, "holder_company_id": "unlisted",
					"commodity_id": "raw_wool", "banking_region": "channel", "in_force": true, "expires_at": input.At.Add(time.Hour),
				}})
			}
			report := AggregateRivals(input)
			reason := "unlisted_only"
			if mode == "our credit only" || mode == "unlisted zero credit" {
				reason = "none_recorded"
			}
			if len(report.Rivals) != 1 || report.Rivals[0].CompanyName != "Listed company" ||
				report.Verdicts.PlaysRough.CompanyName != nil || report.Verdicts.PlaysRough.Fact != nil || report.Verdicts.PlaysRough.Reason != reason {
				t.Fatalf("unlisted evidence became a listed rival or a false absence claim: %+v", report)
			}
		})
	}
}

func TestAggregateRivalsNegativeWorthUsesMeasuredDirection(t *testing.T) {
	for _, test := range []struct {
		baseline int64
		hunch    RivalHunch
	}{{-100, rivalGrowing}, {-50, rivalSteady}, {-25, rivalLosingValue}} {
		t.Run(string(test.hunch), func(t *testing.T) {
			input := rivalTestInput()
			input.Standings = append(input.Standings,
				RivalValuation{SeasonNumber: 1, Rank: 3, CompanyName: "Subject", RefreshedAt: input.At, NetWorthPence: -50},
				RivalValuation{SeasonNumber: 1, Rank: 3, CompanyName: "Subject", RefreshedAt: input.At.Add(-24 * time.Hour), NetWorthPence: test.baseline},
			)
			report := AggregateRivals(input)
			row := rivalTestRow(t, report, "Subject")
			if row.Hunch != test.hunch || row.Fact.Kind != "negative_worth" || row.ChangePence == nil || *row.ChangePence != -50-test.baseline {
				t.Fatalf("negative worth was mistaken for falling valuation: %+v", row)
			}
			if report.Verdicts.NotProfitable.CompanyName == nil || *report.Verdicts.NotProfitable.CompanyName != "Subject" {
				t.Fatalf("below-zero evidence disappeared when its value improved: %+v", report.Verdicts.NotProfitable)
			}
			if raw := string(rivalTestJSON(t, row.Fact)); raw != `{"kind":"negative_worth"}` {
				t.Fatalf("negative-worth fact retained unused payload: %s", raw)
			}
		})
	}
}

func TestAggregateRivalsDangerousChipKeepsStrongestConductHeadline(t *testing.T) {
	input := rivalTestInput()
	input.Standings = append(input.Standings,
		RivalValuation{SeasonNumber: 1, Rank: 1, CompanyName: "Large company", RefreshedAt: input.At, NetWorthPence: 1000},
		RivalValuation{SeasonNumber: 1, Rank: 3, CompanyName: "Other company", RefreshedAt: input.At, NetWorthPence: 50},
	)
	input.Sailings = append(input.Sailings, gameapi.SailingNotice{CompanyName: "Large company", ShipName: "Swift", PortID: "a", SeenAt: input.At})
	input.Evidence = []RivalEvidence{
		{ID: "grant", Type: "monopoly_granted", At: input.At, Payload: json.RawMessage(`{"holder_company_name":"Large company","holder_company_id":"large"}`)},
		{ID: "credit", Type: "loan_request_posted", At: input.At, Payload: json.RawMessage(`{"borrower_name":"Other company","borrower_company_id":"other","borrower_record":{"loans_defaulted":4,"loans_walked_away":2}}`)},
	}
	report := AggregateRivals(input)
	if row := rivalTestRow(t, report, "Large company"); row.Hunch != rivalMostDangerous || row.Fact.Kind != "shared_ports" {
		t.Fatalf("conduct evidence displaced the higher-precedence dangerous chip: %+v", row)
	}
	if row := rivalTestRow(t, report, "Other company"); row.Hunch != rivalPlaysRough || row.Fact.Kind != "loan_walkaways" {
		t.Fatalf("another company's attributable row evidence was suppressed: %+v", row)
	}
	if report.Verdicts.PlaysRough.CompanyName == nil || *report.Verdicts.PlaysRough.CompanyName != "Large company" ||
		report.Verdicts.PlaysRough.Fact == nil || report.Verdicts.PlaysRough.Fact.Kind != "monopolies_recent" {
		t.Fatalf("headline selected the visible chip instead of the strongest conduct: %+v", report.Verdicts.PlaysRough)
	}
}
