package store

import (
	"context"
	"encoding/json"
	"testing"
	"testing/fstest"
	"time"

	"github.com/pressly/goose/v3"
)

// bestCompanyFixture publishes one player whose best company may or may not be
// the calling company.
func bestCompanyFixture(t *testing.T, at time.Time, company string, worth int64, calling bool) json.RawMessage {
	t.Helper()
	raw, err := json.Marshal(map[string]any{
		"season_number": 1, "refreshed_at": at, "next_refresh_at": at.Add(time.Hour),
		"entries": []map[string]any{
			{"rank": 1, "company_name": company, "home_port_id": "boston", "net_worth_pence": worth, "titles": []string{}},
		},
		"you": map[string]any{"rank": 1, "company_name": company, "net_worth_pence": worth, "is_calling_company": calling},
	})
	if err != nil {
		t.Fatal(err)
	}
	return raw
}

func netWorthMetrics(t *testing.T, db *Store, account string) map[time.Time]float64 {
	t.Helper()
	var rows []Metric
	if err := db.db.Where("account_id = ? AND name = ?", account, "net_worth_pence").Order("recorded_at").Find(&rows).Error; err != nil {
		t.Fatal(err)
	}
	result := make(map[time.Time]float64, len(rows))
	for _, row := range rows {
		if _, duplicate := result[row.RecordedAt.UTC()]; duplicate {
			t.Fatalf("duplicate net worth metric at %s", row.RecordedAt)
		}
		result[row.RecordedAt.UTC()] = row.Value
	}
	return result
}

func TestNetWorthHistoryRecordsOwnHourlyValuationsOnce(t *testing.T) {
	db, _, _ := integrationStore(t)
	ctx := context.Background()
	saveTestAccount(t, db, "one")
	first := time.Now().UTC().Add(-4 * time.Hour).Truncate(time.Hour)
	second := first.Add(time.Hour)
	sister := second.Add(time.Hour)
	torn := sister.Add(time.Hour)

	recordStandingsFixture(t, db, "one", standingsFixture(t, first, 10_000, 2_000))
	recordStandingsFixture(t, db, "one", standingsFixture(t, first, 10_000, 2_000))
	// A retry with changed bytes cannot rewrite an official hourly valuation.
	recordStandingsFixture(t, db, "one", standingsFixture(t, first, 10_000, 9_999))
	recordStandingsFixture(t, db, "one", standingsFixture(t, second, 11_000, 2_500))
	// The player's best company being another company is not our net worth.
	recordStandingsFixture(t, db, "one", bestCompanyFixture(t, sister, "Sister company", 50_000, false))
	// A publication whose you entry contradicts its ranked entry is rejected.
	var contradictory map[string]any
	if err := json.Unmarshal(standingsFixture(t, torn, 10_000, 3_000), &contradictory); err != nil {
		t.Fatal(err)
	}
	contradictory["you"].(map[string]any)["net_worth_pence"] = 7_777
	raw, err := json.Marshal(contradictory)
	if err != nil {
		t.Fatal(err)
	}
	recordStandingsFixture(t, db, "one", raw)

	got := netWorthMetrics(t, db, "one")
	if len(got) != 2 || got[first] != 2_000 || got[second] != 2_500 {
		t.Fatalf("net worth metrics = %v, want 2,000 at %s and 2,500 at %s", got, first, second)
	}

	history, err := db.MoneyHistory(ctx, "one", time.Time{}, time.Now().UTC())
	if err != nil {
		t.Fatal(err)
	}
	var plotted []Metric
	for _, metric := range history.Metrics {
		if metric.Name == "net_worth_pence" {
			plotted = append(plotted, metric)
		}
	}
	if len(plotted) != 2 || !plotted[0].RecordedAt.Equal(first) || plotted[0].Value != 2_000 ||
		!plotted[1].RecordedAt.Equal(second) || plotted[1].Value != 2_500 {
		t.Fatalf("money history net worth series = %+v", plotted)
	}
	if !history.From.Equal(first) {
		t.Fatalf("all-time money history starts at %s, want first valuation %s", history.From, first)
	}
}

func TestNetWorthMigrationBackfillsOnlyOwnCompanyStandings(t *testing.T) {
	db, _, _ := integrationStore(t)
	ctx := context.Background()
	saveTestAccount(t, db, "one")
	saveTestAccount(t, db, "two")
	first := time.Now().UTC().Add(-48 * time.Hour).Truncate(time.Hour)
	second := first.Add(time.Hour)
	sister := second.Add(time.Hour)
	recordStandingsFixture(t, db, "one", standingsFixture(t, first, 10_000, 2_000))
	recordStandingsFixture(t, db, "one", standingsFixture(t, second, 11_000, 2_500))
	recordStandingsFixture(t, db, "one", bestCompanyFixture(t, sister, "Sister company", 50_000, true))
	recordStandingsFixture(t, db, "two", bestCompanyFixture(t, second.Add(30*time.Minute), "Second fleet", 4_000, true))
	if err := db.db.Exec(`INSERT INTO configured_company_identities (company_id, name, observed_at) VALUES
		('company-one', 'Our fleet', now()), ('company-two', 'Second fleet', now())`).Error; err != nil {
		t.Fatal(err)
	}

	raw, err := migrations.ReadFile("migrations/00020_net_worth_metrics.sql")
	if err != nil {
		t.Fatal(err)
	}
	provider, err := goose.NewProvider(goose.DialectPostgres, db.sqlDB,
		fstest.MapFS{"00020_net_worth_metrics.sql": {Data: raw}},
		goose.WithDisableGlobalRegistry(true), goose.WithDisableVersioning(true))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := provider.Down(ctx); err != nil {
		t.Fatal(err)
	}
	if got := netWorthMetrics(t, db, "one"); len(got) != 3 {
		t.Fatalf("down migration must keep the three live-recorded valuations, got %v", got)
	}
	// Simulate a database upgraded from before version 20, whose raw
	// diagnostics have already been pruned.
	if err := db.db.Exec("DELETE FROM metrics WHERE name = 'net_worth_pence'").Error; err != nil {
		t.Fatal(err)
	}
	if err := db.db.Exec("DELETE FROM requests").Error; err != nil {
		t.Fatal(err)
	}
	for range 2 {
		if _, err := provider.UpTo(ctx, 20); err != nil {
			t.Fatal(err)
		}
		one := netWorthMetrics(t, db, "one")
		if len(one) != 2 || one[first] != 2_000 || one[second] != 2_500 {
			t.Fatalf("backfilled net worth for account one = %v", one)
		}
		two := netWorthMetrics(t, db, "two")
		if len(two) != 1 || two[second.Add(30*time.Minute)] != 4_000 {
			t.Fatalf("backfilled net worth for account two = %v", two)
		}
		if _, err := provider.Down(ctx); err != nil {
			t.Fatal(err)
		}
	}
}
