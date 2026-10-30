package store

import (
	"context"
	"errors"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/DevYukine/go-tradewinds/internal/gameapi"
)

func TestRivalCreditRecordedCompanySourcesKeepCompanyIdentity(t *testing.T) {
	database, _, _ := integrationStore(t)
	at := time.Now().UTC()
	sources := []struct{ path, raw string }{
		{"/v1/world/monopolies", `[{"holder_company_id":"holder","holder_company_name":"Holder company"}]`},
		{"/v1/bank/list", `[{"company_id":"bank-owner","name":"Not the company name"}]`},
		{"/v1/loan/requests", `[{"borrower_company_id":"borrower","borrower_name":"Borrower company"}]`},
		{"/v1/future/list", `[{"creator_company_id":"creator","seller_company_id":"seller","buyer_company_id":"buyer"}]`},
	}
	for _, source := range sources {
		if err := recordRivalCompanies(database.db, 1, source.path, []byte(source.raw), at); err != nil {
			t.Fatal(err)
		}
	}
	var companies []rivalCompany
	if err := database.db.Order("company_id").Find(&companies).Error; err != nil {
		t.Fatal(err)
	}
	got := make(map[string]string)
	for _, company := range companies {
		got[company.CompanyID] = company.CompanyName
	}
	want := map[string]string{"holder": "Holder company", "bank-owner": "", "borrower": "Borrower company", "creator": "", "seller": "", "buyer": ""}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("source identities=%v want=%v", got, want)
	}
	if err := recordRivalCompany(database.db, 1, "bank-owner", "Real company", at); err != nil {
		t.Fatal(err)
	}
	if err := recordRivalCompanies(database.db, 1, sources[1].path, []byte(sources[1].raw), at); err != nil {
		t.Fatal(err)
	}
	var owner rivalCompany
	if err := database.db.Where("company_id = ?", "bank-owner").Take(&owner).Error; err != nil {
		t.Fatal(err)
	}
	if owner.CompanyName != "Real company" {
		t.Fatalf("bank renamed its owning company: %+v", owner)
	}
}

func TestRivalCreditCandidatesPrioritizeRankAndOldestAttempt(t *testing.T) {
	database, _, _ := integrationStore(t)
	saveTestAccount(t, database, "one")
	ctx := context.Background()
	if _, err := database.RivalCreditCandidates(ctx, "one"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("missing publication season was not held: %v", err)
	}
	at := time.Now().UTC().Add(-time.Minute)
	for _, row := range []standingRow{
		{SeasonNumber: 1, Rank: 1, CompanyName: "Kraken", RefreshedAt: at, ObservedAt: at, Titles: []byte(`[]`)},
		{SeasonNumber: 1, Rank: 2, CompanyName: "Meridian", RefreshedAt: at, ObservedAt: at, Titles: []byte(`[]`)},
	} {
		if err := database.db.Create(&row).Error; err != nil {
			t.Fatal(err)
		}
	}
	for id, name := range map[string]string{"z-kraken": "Kraken", "z-meridian": "Meridian", "a-unranked": "Unranked", "old-unranked": "Old", "company-one": "Own", "   ": "Invalid", strings.Repeat("x", 256): "Invalid long"} {
		if err := recordRivalCompany(database.db, 1, id, name, at); err != nil {
			t.Fatal(err)
		}
	}
	if err := database.db.Create(&rivalCreditAttempt{AccountID: "one", CompanyID: "a-unranked", AttemptedAt: at.Add(-7 * time.Hour)}).Error; err != nil {
		t.Fatal(err)
	}
	rivalTestEvent(t, database, "futures-party", "futures_listed", at, at, map[string]any{"creator_company_id": "creator", "seller_company_id": "seller", "buyer_company_id": "buyer"})
	rivalTestEvent(t, database, "monopoly-party", "monopoly_granted", at, at, map[string]any{"holder_company_id": "holder"})
	rivalTestEvent(t, database, "borrower-party", "loan_request_posted", at, at, map[string]any{"borrower_company_id": "borrower"})
	rivalTestEvent(t, database, "envelope-only", "ship_sailed", at, at, map[string]any{"company_id": "not-a-party"})
	ids, err := database.RivalCreditCandidates(ctx, "one")
	if err != nil {
		t.Fatal(err)
	}
	want := []string{"z-kraken", "z-meridian", "borrower", "buyer", "creator", "holder", "old-unranked", "seller", "a-unranked"}
	if !reflect.DeepEqual(ids, want) {
		t.Fatalf("ranked and event candidates=%v want=%v", ids, want)
	}
	reserved, err := database.ReserveRivalCredits(ctx, "one", ids)
	if err != nil || !reflect.DeepEqual(reserved, ids) {
		t.Fatalf("batch reservation=%v err=%v", reserved, err)
	}
	if got, err := database.ReserveRivalCredits(ctx, "one", ids); err != nil || len(got) != 0 {
		t.Fatalf("batch ignored company cooldown: %v %v", got, err)
	}
}

func TestRivalOverviewSharesEventReadAndKeepsCoverageSeparate(t *testing.T) {
	database, _, _ := integrationStore(t)
	saveTestAccount(t, database, "one")
	at := time.Now().UTC()
	// More than one page of old sightings must not truncate the 24h traffic window.
	if err := database.db.Exec(`INSERT INTO events (account_id,event_id,event_type,payload,created_at)
 SELECT 'one', 'old-' || n, 'ship_sailed', jsonb_build_object('timestamp',?::text,'payload',jsonb_build_object('company_name','Old rival','ship_name','Ship ' || n,'ship_class','hoy','port_id','a','direction','departed')), ?::timestamptz
 FROM generate_series(1,?) n`, at.Add(-48*time.Hour).Format(time.RFC3339Nano), at.Add(-48*time.Hour), maxCompetitionEvents+1).Error; err != nil {
		t.Fatal(err)
	}
	rivalTestEvent(t, database, "current", "ship_sailed", at.Add(-time.Minute), at, gameapi.SailingNotice{CompanyName: "Current rival", ShipName: "Current", ShipClass: "cutter", PortID: "b", SeenAt: at.Add(-time.Minute)})
	input := RivalsInput{CompetitionInput: CompetitionInput{AccountID: "one", At: at, OwnCompaniesComplete: false}}
	if err := rivalOverviewInput(database.db, &input, 1, nil, nil); err != nil {
		t.Fatal(err)
	}
	if input.OwnCompaniesComplete {
		t.Fatal("overview upgraded incomplete owner coverage")
	}
	if !input.ShipSightingsTruncated || input.EventsTruncated || len(input.Sailings) != 1 || input.Sailings[0].PortID != "b" {
		t.Fatalf("traffic windows contaminated: oldPartial=%v trafficPartial=%v sailings=%+v", input.ShipSightingsTruncated, input.EventsTruncated, input.Sailings)
	}
	report := AggregateRivals(input)
	if !report.ObservedShips7dTruncated {
		t.Fatal("seven-day partial coverage was hidden")
	}
	row := rivalTestRow(t, report, "Current rival")
	if row.ObservedShips24h["cutter"] != 1 || row.ObservedShips7d["cutter"] != 1 {
		t.Fatalf("shared read lost observed ships: %+v", row)
	}
}

func TestRisingPublicationFreshnessIsIndependentOfValuation(t *testing.T) {
	at := time.Now().UTC()
	for _, age := range []time.Duration{time.Hour, 3 * time.Hour} {
		published := at.Add(-age)
		input := RivalsInput{CompetitionInput: CompetitionInput{At: at}, RisingRefreshedAt: &published}
		input.Standings = []RivalValuation{{SeasonNumber: 1, Rank: 1, CompanyName: "Current", RefreshedAt: at}}
		report := AggregateRivals(input)
		if report.RisingRefreshedAt == nil || !report.RisingRefreshedAt.Equal(published) || report.RisingStale != (age > GrowthFreshFor) {
			t.Fatalf("publication freshness hidden: %+v", report)
		}
	}
	if report := AggregateRivals(RivalsInput{CompetitionInput: CompetitionInput{At: at}}); !report.RisingStale {
		t.Fatal("missing rising publication treated as fresh")
	}
}

func TestRivalOwnNamesOverflowDegradesToIncomplete(t *testing.T) {
	database, _, _ := integrationStore(t)
	saveTestAccount(t, database, "one")
	at := time.Now().UTC()
	if err := database.db.Exec(`INSERT INTO standings_history (season_number,refreshed_at,rank,company_name,home_port_id,net_worth_pence,titles,observed_at)
 SELECT 1, ?::timestamptz - n * interval '1 second', 1, 'Old own ' || n,'port',0,'[]'::jsonb,?::timestamptz FROM generate_series(1,1001) n`, at, at).Error; err != nil {
		t.Fatal(err)
	}
	if err := database.db.Exec(`INSERT INTO standings_accounts (account_id,season_number,refreshed_at,rank)
 SELECT 'one',season_number,refreshed_at,rank FROM standings_history`).Error; err != nil {
		t.Fatal(err)
	}
	_, complete, err := rivalOwnNames(database.db, 1, at)
	if err != nil || complete {
		t.Fatalf("bounded ownership failed instead of degrading: complete=%v err=%v", complete, err)
	}
}

func TestRisingRejectedNewBoardDisclosesRetainedPublication(t *testing.T) {
	database, _, _ := integrationStore(t)
	saveTestAccount(t, database, "one")
	at := time.Now().UTC().Truncate(time.Microsecond)
	old := at.Add(-3 * time.Hour)
	rivalsMoneyPublication(t, database, old, 100, rivalsMoneyRising(old, 4, 50))
	rivalsMoneyPublication(t, database, at, 200, map[string]any{
		"ranked_players": 4,
		"entries":        []any{map[string]any{"rank": 1, "company_name": "Invalid newer entry"}},
	})
	report, err := database.Rivals(context.Background(), "one", at)
	if err != nil {
		t.Fatal(err)
	}
	if report.ValuationAt == nil || !report.ValuationAt.Equal(at) ||
		report.RisingRefreshedAt == nil || !report.RisingRefreshedAt.Equal(old) || !report.RisingStale {
		t.Fatalf("retained old board masqueraded as current valuation: %+v", report)
	}
}

func TestWarningReportSuppressesGrowthWhenOwnNamesIncomplete(t *testing.T) {
	for _, basis := range []string{"rising", "valuation_24h"} {
		t.Run(basis, func(t *testing.T) {
			database, _, _ := integrationStore(t)
			ctx := context.Background()
			saveTestAccount(t, database, "one")
			at := time.Now().UTC().Truncate(time.Microsecond)
			rivalsMoneySaveProfile(t, database, "one", "company-one", "Ours")
			rivalsMoneyPublication(t, database, at.Add(-24*time.Hour), 1000, nil)
			var rising any
			if basis == "rising" {
				rising = rivalsMoneyRising(at, 4, 0)
			}
			rivalsMoneyPublication(t, database, at, 500, rising)
			report, err := database.WarningReport(ctx, at)
			if err != nil {
				t.Fatal(err)
			}
			if len(report.Accounts) != 1 || !report.Accounts[0].Available {
				t.Fatalf("expected monitored account: %+v", report)
			}
			warning := warningsAcceptanceFind(report.Accounts[0].Warnings, "falling_behind")
			if warning == nil || warning.Names["basis"] != basis {
				t.Fatalf("complete ownership did not exercise %s growth: %+v", basis, report)
			}
			// Disabled accounts still own companies. With no retained name for
			// this company, neither growth source can identify all rivals.
			saveTestAccount(t, database, "two")
			if err := database.db.Model(&Account{}).Where("id = ?", "two").Update("enabled", false).Error; err != nil {
				t.Fatal(err)
			}
			report, err = database.WarningReport(ctx, at)
			if err != nil {
				t.Fatal(err)
			}
			if len(report.Accounts) != 1 || !report.Accounts[0].Available {
				t.Fatalf("incomplete ownership disabled warning reporting: %+v", report)
			}
			account := report.Accounts[0]
			if warning := warningsAcceptanceFind(account.Warnings, "falling_behind"); warning != nil {
				t.Fatalf("incomplete ownership assessed rival growth: %+v", warning)
			}
			if !warningsAcceptanceContains(account.Suppressed, "falling_behind") {
				t.Fatalf("missing ownership was presented as assessed: %+v", account)
			}
		})
	}
}
