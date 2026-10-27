package store

import (
	"context"
	"encoding/json"
	"fmt"
	"testing"
	"time"
)

func growthAcceptanceStore(t *testing.T, input RivalsInput, identities []standingAccount) *Store {
	t.Helper()
	database, _, _ := integrationStore(t)
	saveTestAccount(t, database, "one")
	if err := database.SaveSnapshot(context.Background(), "one", map[string]json.RawMessage{
		"profile": json.RawMessage(`{"company":{"id":"company-one","name":"The Pokemon Pirates"}}`),
	}); err != nil {
		t.Fatal(err)
	}
	for _, value := range input.Standings {
		row := standingRow{SeasonNumber: value.SeasonNumber, RefreshedAt: value.RefreshedAt,
			Rank: value.Rank, CompanyName: value.CompanyName, NetWorthPence: value.NetWorthPence,
			Titles: json.RawMessage(`[]`), ObservedAt: input.At}
		if err := database.db.Create(&row).Error; err != nil {
			t.Fatal(err)
		}
	}
	for _, identity := range identities {
		if err := database.db.Create(&identity).Error; err != nil {
			t.Fatal(err)
		}
	}
	return database
}

func growthAcceptanceRead(t *testing.T, database *Store, at time.Time, hours int) GrowthWindow {
	t.Helper()
	report, err := database.Rivals(context.Background(), "one", at)
	if err != nil {
		t.Fatal(err)
	}
	if len(report.Growth) != 2 || report.Growth[0].WindowHours != 6 || report.Growth[1].WindowHours != 24 {
		t.Fatalf("growth must contain 6 h then 24 h: %+v", report.Growth)
	}
	for _, window := range report.Growth {
		if window.WindowHours == hours {
			return window
		}
	}
	t.Fatalf("missing %d-hour window", hours)
	return GrowthWindow{}
}

func TestGrowthBenchmarkAcceptance0128Fixture(t *testing.T) {
	input := rivalsAcceptanceFixture()
	from := input.At.Add(-24 * time.Hour)
	database := growthAcceptanceStore(t, input, []standingAccount{
		{AccountID: "one", SeasonNumber: 1, RefreshedAt: input.At, Rank: 3},
		{AccountID: "one", SeasonNumber: 1, RefreshedAt: from, Rank: 3},
	})
	window := growthAcceptanceRead(t, database, input.At, 24)
	if window.OursPence == nil || *window.OursPence != -1_246_694 || window.RivalMedianPence == nil || *window.RivalMedianPence != -3_505_653 {
		t.Fatalf("01:28 integer-pence change/median: %+v", window)
	}
	if window.RivalsMeasured != 6 || window.RivalsUnmeasured != 1 || window.TopRival == nil || *window.TopRival != "TescoValue" || window.TopRivalPence == nil || *window.TopRivalPence != 93_888 {
		t.Fatalf("Vekbeard must be unmeasured and TescoValue best: %+v", window)
	}
	if window.OurPlace == nil || *window.OurPlace != 2 || window.PlaceOf == nil || *window.PlaceOf != 7 || window.From == nil || !window.From.Equal(from) || window.To == nil || !window.To.Equal(input.At) {
		t.Fatalf("rank or matched publication clocks lost: %+v", window)
	}
	later := growthAcceptanceRead(t, database, input.At.Add(45*time.Minute), 24)
	if later.From == nil || !later.From.Equal(from) || later.To == nil || !later.To.Equal(input.At) {
		t.Fatalf("request time replaced publication endpoints: %+v", later)
	}
}

func TestGrowthBenchmarkAcceptanceOwnIdentityRequiredAtBothEndpoints(t *testing.T) {
	for _, mode := range []string{"missing baseline identity", "missing latest identity", "different best company"} {
		t.Run(mode, func(t *testing.T) {
			input := rivalsAcceptanceFixture()
			from := input.At.Add(-24 * time.Hour)
			identities := []standingAccount{}
			if mode != "missing latest identity" {
				identities = append(identities, standingAccount{AccountID: "one", SeasonNumber: 1, RefreshedAt: input.At, Rank: 3})
			}
			if mode != "missing baseline identity" {
				rank := 3
				if mode == "different best company" {
					rank = 1
				}
				identities = append(identities, standingAccount{AccountID: "one", SeasonNumber: 1, RefreshedAt: from, Rank: rank})
			}
			window := growthAcceptanceRead(t, growthAcceptanceStore(t, input, identities), input.At, 24)
			if window.OursPence != nil || window.OurPlace != nil || window.OursReason == "" {
				t.Fatalf("unobserved own identity became measured growth: %+v", window)
			}
		})
	}
}

func TestGrowthBenchmarkAcceptancePublicationMatchingAndSeason(t *testing.T) {
	for _, hours := range []int{6, 24} {
		for _, test := range []struct {
			name     string
			offset   time.Duration
			season   int
			measured bool
		}{
			{"one hour early", -time.Hour, 1, true},
			{"one hour late", time.Hour, 1, true},
			{"too early", -time.Hour - time.Second, 1, false},
			{"too late", time.Hour + time.Second, 1, false},
			{"previous season", 0, 0, false},
		} {
			t.Run(fmt.Sprintf("%dh/%s", hours, test.name), func(t *testing.T) {
				at := rivalsAcceptanceFixture().At
				from := at.Add(-time.Duration(hours) * time.Hour).Add(test.offset)
				input := RivalsInput{CompetitionInput: CompetitionInput{At: at}, Standings: []RivalValuation{
					{SeasonNumber: 1, RefreshedAt: at, Rank: 1, CompanyName: "Rival", NetWorthPence: 101},
					{SeasonNumber: 1, RefreshedAt: at, Rank: 2, CompanyName: "The Pokemon Pirates", NetWorthPence: 90},
					{SeasonNumber: test.season, RefreshedAt: from, Rank: 1, CompanyName: "Rival", NetWorthPence: 100},
					{SeasonNumber: test.season, RefreshedAt: from, Rank: 2, CompanyName: "The Pokemon Pirates", NetWorthPence: 100},
				}}
				database := growthAcceptanceStore(t, input, []standingAccount{
					{AccountID: "one", SeasonNumber: 1, RefreshedAt: at, Rank: 2},
					{AccountID: "one", SeasonNumber: test.season, RefreshedAt: from, Rank: 2},
				})
				window := growthAcceptanceRead(t, database, at, hours)
				if !test.measured {
					if window.OursPence != nil || window.RivalMedianPence != nil || window.TopRival != nil || window.TopRivalPence != nil || window.RivalsMeasured != 0 {
						t.Fatalf("missing same-season publication became measured: %+v", window)
					}
					return
				}
				if window.OursPence == nil || *window.OursPence != -10 || window.RivalMedianPence == nil || *window.RivalMedianPence != 1 || window.From == nil || !window.From.Equal(from) || window.To == nil || !window.To.Equal(at) {
					t.Fatalf("inclusive one-hour match or source clocks: %+v", window)
				}
			})
		}
	}
}

func TestGrowthBenchmarkAcceptanceNearestPublicationAndMedianTruncation(t *testing.T) {
	at := rivalsAcceptanceFixture().At
	from := at.Add(-6*time.Hour + 20*time.Minute)
	input := RivalsInput{CompetitionInput: CompetitionInput{At: at}}
	for rank, name := range []string{"The Pokemon Pirates", "A", "B"} {
		for _, endpoint := range []struct {
			at    time.Time
			worth int64
		}{
			{at, 100}, {from, int64(100 + rank)}, {at.Add(-6*time.Hour - 40*time.Minute), 900},
		} {
			input.Standings = append(input.Standings, RivalValuation{SeasonNumber: 1, RefreshedAt: endpoint.at, Rank: rank + 1, CompanyName: name, NetWorthPence: endpoint.worth})
		}
	}
	database := growthAcceptanceStore(t, input, []standingAccount{
		{AccountID: "one", SeasonNumber: 1, RefreshedAt: at, Rank: 1},
		{AccountID: "one", SeasonNumber: 1, RefreshedAt: from, Rank: 1},
	})
	window := growthAcceptanceRead(t, database, at, 6)
	if window.From == nil || !window.From.Equal(from) || window.OursPence == nil || *window.OursPence != 0 || window.RivalMedianPence == nil || *window.RivalMedianPence != -1 || window.TopRivalPence == nil || *window.TopRivalPence != -1 {
		t.Fatalf("nearest publication or negative median truncation: %+v", window)
	}
}
