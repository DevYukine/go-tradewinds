package store

import (
	"context"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/DevYukine/go-tradewinds/internal/gameapi"
)

func TestRivalCreditIngestionExcludesEveryConfiguredCompany(t *testing.T) {
	database, _, _ := integrationStore(t)
	saveTestAccount(t, database, "one")
	saveTestAccount(t, database, "two")
	rivalsMoneyPublication(t, database, time.Now().UTC(), 1_000_000, rivalsMoneyRising(time.Now().UTC(), 4, 100))
	tables := []string{"rival_credit_attempts", "rival_companies", "rival_credit_reports"}
	before := make(map[string]int64, len(tables))
	for _, table := range tables {
		var count int64
		if err := database.db.Table(table).Count(&count).Error; err != nil {
			t.Fatal(err)
		}
		before[table] = count
	}
	for _, company := range []string{"company-one", "company-two"} {
		for range 3 {
			rivalsMoneyCredit(t, database, company, "Owned "+company, 200)
		}
		rivalsMoneyCredit(t, database, company, "Failed identity must not replace the name", 503)
	}
	if err := database.RecordRequest(context.Background(), "one", "GET", "/v1/loan/credit?company_id=company-one", 200, time.Millisecond,
		nil, []byte(`{"company_id":"foreign","company_name":"Foreign identity"}`), ""); err != nil {
		t.Fatal(err)
	}
	var identities []struct {
		CompanyID string
		Name      string
	}
	if err := database.db.Table("configured_company_identities").Order("company_id").Find(&identities).Error; err != nil ||
		len(identities) != 2 || identities[0].CompanyID != "company-one" || identities[0].Name != "Owned company-one" ||
		identities[1].CompanyID != "company-two" || identities[1].Name != "Owned company-two" {
		t.Fatalf("configured identity was lost or overwritten by failure/foreign evidence: %+v %v", identities, err)
	}
	for _, table := range tables {
		var count int64
		if err := database.db.Table(table).Count(&count).Error; err != nil || count != before[table] {
			t.Fatalf("own-credit GET changed %s: before=%d after=%d err=%v", table, before[table], count, err)
		}
	}
	ids := make([]string, RivalCreditHourlyLimit)
	for i := range ids {
		ids[i] = fmt.Sprintf("rival-%d", i)
	}
	if reserved, err := database.ReserveRivalCredits(context.Background(), "one", ids); err != nil || len(reserved) != RivalCreditHourlyLimit {
		t.Fatalf("own-credit GET consumed the rival hourly allowance: %v %v", reserved, err)
	}
}

func TestRivalCreditReservationsShareRollingBudgetAcrossAccounts(t *testing.T) {
	database, _, _ := integrationStore(t)
	saveTestAccount(t, database, "one")
	saveTestAccount(t, database, "two")
	ctx := context.Background()
	if reserved, err := database.ReserveRivalCredits(ctx, "one", []string{"company-two"}); err != nil || len(reserved) != 0 {
		t.Fatalf("registered secondary company consumed a lookup: reserved=%v err=%v", reserved, err)
	}
	var workers sync.WaitGroup
	results := make(chan bool, 30)
	errors := make(chan error, 30)
	for i := range 30 {
		workers.Add(1)
		go func() {
			defer workers.Done()
			account := "one"
			if i%2 == 0 {
				account = "two"
			}
			reserved, err := database.ReserveRivalCredits(ctx, account, []string{fmt.Sprintf("rival-%02d", i)})
			results <- len(reserved) != 0
			errors <- err
		}()
	}
	workers.Wait()
	close(results)
	close(errors)
	for err := range errors {
		if err != nil {
			t.Fatal(err)
		}
	}
	count := 0
	for reserved := range results {
		if reserved {
			count++
		}
	}
	if count != RivalCreditHourlyLimit {
		t.Fatalf("concurrent runners reserved %d lookups, want %d", count, RivalCreditHourlyLimit)
	}
	// Reservations whose process died still count after diagnostic retention.
	if err := database.db.Exec("DELETE FROM requests").Error; err != nil {
		t.Fatal(err)
	}
	if reserved, err := database.ReserveRivalCredits(ctx, "two", []string{"new-rival"}); err != nil || len(reserved) != 0 {
		t.Fatalf("a pending reservation was refunded: reserved=%v err=%v", reserved, err)
	}
}

func TestRivalCreditReservationsKeepSixRealHourCooldownAfterFailure(t *testing.T) {
	database, _, _ := integrationStore(t)
	saveTestAccount(t, database, "one")
	saveTestAccount(t, database, "two")
	ctx := context.Background()
	if err := database.RecordRequest(ctx, "one", "GET", "/v1/loan/credit?company_id=rival", 503, time.Millisecond, nil, []byte(`{"error":"unavailable"}`), "unavailable"); err != nil {
		t.Fatal(err)
	}
	if err := database.db.Exec("UPDATE rival_credit_attempts SET attempted_at = now() - interval '2 hours'").Error; err != nil {
		t.Fatal(err)
	}
	if reserved, err := database.ReserveRivalCredits(ctx, "two", []string{"rival"}); err != nil || len(reserved) != 0 {
		t.Fatalf("failed lookup ignored the cross-account six-hour cooldown: reserved=%v err=%v", reserved, err)
	}
	if err := database.db.Exec("UPDATE rival_credit_attempts SET attempted_at = now() - interval '6 hours' - interval '1 second'").Error; err != nil {
		t.Fatal(err)
	}
	if reserved, err := database.ReserveRivalCredits(ctx, "two", []string{"rival"}); err != nil || len(reserved) != 1 {
		t.Fatalf("expired real-time cooldown blocked a new observation: reserved=%v err=%v", reserved, err)
	}
}

func TestRisingNeverReusesAnEarlierSeason(t *testing.T) {
	database, _, _ := integrationStore(t)
	saveTestAccount(t, database, "one")
	ctx := context.Background()
	at := time.Now().UTC().Add(-time.Minute)
	rivalsMoneyPublication(t, database, at.Add(-time.Hour), 1_000_000, rivalsMoneyRising(at.Add(-time.Hour), 4, 50))
	newSeason := rivalTestJSON(t, map[string]any{
		"season_number": 2, "refreshed_at": at, "next_refresh_at": at.Add(time.Hour),
		"entries": []any{map[string]any{"rank": 1, "company_name": "New season rival", "net_worth_pence": 200}},
	})
	if err := database.RecordRequest(ctx, "one", "GET", "/v1/world/leaderboard", 200, time.Millisecond, nil, newSeason, ""); err != nil {
		t.Fatal(err)
	}
	board, publishedAt, _, err := latestRising(database.db, "one", at)
	if err != nil || publishedAt != nil || board.RankedPlayers != 0 || len(board.Entries) != 0 {
		t.Fatalf("old-season rising was reused: %+v at=%v err=%v", board, publishedAt, err)
	}
}

func TestRisingWarningUsesStrictFractionalMedianAndExcludesOwnCompanies(t *testing.T) {
	at := time.Now().UTC()
	start := at.Add(-7 * 24 * time.Hour)
	input := WarningInput{
		RisingAt: &at, OwnCompanies: []string{"Ours", "Secondary"},
		Rising: gameapi.RisingBoard{
			RankedPlayers: 4,
			You:           &gameapi.RisingOwnRank{Rank: 3, CompanyName: "Ours", GrowthBPS: 0, WindowStartAt: start},
			Entries: []gameapi.RisingEntry{
				{Rank: 1, CompanyName: "Secondary", GrowthBPS: 9999},
				{Rank: 2, CompanyName: "Rival A", GrowthBPS: 1},
				{Rank: 3, CompanyName: "Ours", GrowthBPS: 0},
				{Rank: 4, CompanyName: "Rival B", GrowthBPS: 0},
			},
		},
	}
	warning := warningsAcceptanceFind(AssessWarnings(input, at).Warnings, "falling_behind")
	if warning == nil || warning.Names["basis"] != "rising" || warning.Facts["median_growth_bps"] != 0 || warning.Facts["median_growth_half_bps"] != 1 {
		t.Fatalf("zero growth is strictly below the rival 0.5 bps median: %+v", warning)
	}
	input.Rising.You.GrowthBPS = 1
	if warning := warningsAcceptanceFind(AssessWarnings(input, at).Warnings, "falling_behind"); warning != nil {
		t.Fatalf("own secondary growth contaminated the rival median: %+v", warning)
	}
	input.Rising.You = nil
	if assessment := AssessWarnings(input, at); !warningsAcceptanceContains(assessment.Suppressed, "falling_behind") {
		t.Fatalf("missing own rising observation was treated as an observed zero: %+v", assessment)
	}
}

func TestRivalConfiguredProfileIdentitySurvivesMissingProfile(t *testing.T) {
	database, dsn, key := integrationStore(t)
	saveTestAccount(t, database, "one")
	saveTestAccount(t, database, "two")
	ctx := context.Background()
	rivalsMoneySaveProfile(t, database, "one", "company-one", "Ours")
	rivalsMoneySaveProfile(t, database, "two", "company-two", "Own secondary")
	rivalsMoneyPublication(t, database, time.Now().UTC(), 1_000_000, nil)
	// A profile naming another registered company is not this runner's
	// authority to rename it, even though both companies belong to us.
	rivalsMoneySaveProfile(t, database, "two", "company-one", "Spoofed primary")
	if err := database.SaveSnapshot(ctx, "two", nil); err != nil {
		t.Fatal(err)
	}
	if err := database.db.Exec("DELETE FROM requests").Error; err != nil {
		t.Fatal(err)
	}
	reopened, err := Open(ctx, dsn, key, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer reopened.Close()
	report, err := reopened.Rivals(ctx, "one", time.Now().UTC().Add(time.Second))
	if err != nil {
		t.Fatal(err)
	}
	for _, rival := range report.Rivals {
		if rival.CompanyName == "Ours" || rival.CompanyName == "Own secondary" {
			t.Fatalf("missing profile or restart lost a validated configured identity: %+v", rival)
		}
	}
	var names []string
	if err := database.db.Table("configured_company_identities").Order("company_id").Pluck("name", &names).Error; err != nil ||
		len(names) != 2 || names[0] != "Ours" || names[1] != "Own secondary" {
		t.Fatalf("profile identity accepted the wrong configured binding: %v %v", names, err)
	}
}

func TestRivalConfiguredProfileIdentityRedactsCredentials(t *testing.T) {
	database, _, _ := integrationStore(t)
	credentials := saveTestAccount(t, database, "one")
	rivalsMoneySaveProfile(t, database, "one", "company-one", "Owned "+credentials.APIKey)
	var name string
	if err := database.db.Table("configured_company_identities").Where("company_id = ?", "company-one").Pluck("name", &name).Error; err != nil {
		t.Fatal(err)
	}
	if strings.Contains(name, credentials.APIKey) || !strings.Contains(name, redacted) {
		t.Fatalf("profile identity persisted an unscrubbed credential: %q", name)
	}
}
