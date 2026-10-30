package store

import (
	"context"
	"encoding/json"
	"reflect"
	"sort"
	"testing"
	"testing/fstest"
	"time"

	"github.com/pressly/goose/v3"
	"go.uber.org/zap"
)

// Wire contract: existing rival rows gain company_id, credit,
// credit_observed_at, rising_rank, growth_bps, gained_pence,
// observed_ships_24h / observed_ships_7d (class -> distinct observed names),
// and expansion_contributions [{target_phase, tons, share_bps}]. Credit keeps
// the upstream field names; a missing report is JSON null, not a zero record.
// RivalReport.rising keeps the upstream board shape. Warning names.basis is
// "rising" or "valuation_24h"; rising facts use ours_growth_bps,
// median_growth_bps and ranked_players, never pence for a percentage.
type rivalsMoneyRow struct {
	CompanyName string                     `json:"company_name"`
	CompanyID   *string                    `json:"company_id"`
	Worth       *int64                     `json:"net_worth_pence"`
	Change      *int64                     `json:"change_pence"`
	Credit      map[string]json.RawMessage `json:"credit"`
	CreditAt    *time.Time                 `json:"credit_observed_at"`
	RisingRank  *int                       `json:"rising_rank"`
	GrowthBPS   *int64                     `json:"growth_bps"`
	Gained      *int64                     `json:"gained_pence"`
	Ships24h    map[string]int             `json:"observed_ships_24h"`
	Ships7d     map[string]int             `json:"observed_ships_7d"`
	Expansion   []struct {
		Phase int   `json:"target_phase"`
		Tons  int64 `json:"tons"`
		Share int64 `json:"share_bps"`
	} `json:"expansion_contributions"`
}

func rivalsMoneyJSON(t *testing.T, value any) json.RawMessage {
	t.Helper()
	raw, err := json.Marshal(value)
	if err != nil {
		t.Fatal(err)
	}
	return raw
}

func rivalsMoneySaveProfile(t *testing.T, database *Store, account, company, name string) {
	t.Helper()
	if err := database.SaveSnapshot(context.Background(), account, map[string]json.RawMessage{
		"profile": rivalsMoneyJSON(t, map[string]any{"company": map[string]string{"id": company, "name": name}}),
	}); err != nil {
		t.Fatal(err)
	}
}

func rivalsMoneyRising(at time.Time, ranked int, ours int64) map[string]any {
	start := at.Add(-7 * 24 * time.Hour)
	values := []struct {
		name   string
		growth int64
	}{{"Rival A", 300}, {"Rival B", 200}, {"Rival C", 100}, {"Ours", ours}}
	if ranked == 3 {
		values = append(values[:1], values[2:]...)
	} else if ranked == 2 {
		values = append(values[:1], values[3:]...)
	}
	sort.SliceStable(values, func(i, j int) bool { return values[i].growth > values[j].growth })
	entries := []map[string]any{}
	ownRank := 0
	for i, item := range values {
		if item.name == "Ours" {
			ownRank = i + 1
		}
		entries = append(entries, map[string]any{
			"rank": i + 1, "company_name": item.name, "home_port_id": "amsterdam",
			"growth_bps": item.growth, "gained_pence": item.growth * 100,
			"window_start_at": start,
		})
	}
	return map[string]any{
		"ranked_players": ranked, "window_start_at": start, "entries": entries,
		"you": map[string]any{"rank": ownRank, "company_name": "Ours", "growth_bps": ours,
			"gained_pence": ours * 100, "window_start_at": start, "is_calling_company": true},
	}
}

func rivalsMoneyPublication(t *testing.T, database *Store, at time.Time, ownWorth int64, rising any) {
	t.Helper()
	entries := []map[string]any{}
	for i, name := range []string{"Ours", "Rival A", "Rival B", "Rival C", "Own secondary"} {
		worth := int64(1_000_000 + i*10_000)
		if name == "Ours" {
			worth = ownWorth
		}
		entries = append(entries, map[string]any{"rank": i + 1, "company_name": name,
			"home_port_id": "amsterdam", "net_worth_pence": worth, "titles": []string{}})
	}
	raw := rivalsMoneyJSON(t, map[string]any{
		"season_number": 1, "ranked_players": 5, "refreshed_at": at,
		"next_refresh_at": at.Add(time.Hour), "entries": entries,
		"you":    map[string]any{"rank": 1, "company_name": "Ours", "net_worth_pence": ownWorth, "is_calling_company": true},
		"rising": rising,
	})
	if err := database.RecordRequest(context.Background(), "one", "GET", "/v1/world/leaderboard", 200, time.Millisecond, nil, raw, ""); err != nil {
		t.Fatal(err)
	}
}

func rivalsMoneyCredit(t *testing.T, database *Store, id, name string, status int) {
	t.Helper()
	// Field inventory follows the authenticated live-probe CreditReport, with
	// deliberately overlapping lifetime losses so they cannot be summed as debt.
	raw := rivalsMoneyJSON(t, map[string]any{
		"company_id": id, "company_name": name,
		"active_loans": 2, "active_owed_pence": 1001,
		"defaulted_open_loans": 1, "defaulted_owed_pence": 300,
		"loans_repaid": 7, "loans_defaulted": 4, "loans_written_off": 2,
		"loan_written_off_pence": 700, "loans_walked_away": 3, "loan_walked_away_pence": 500,
		"open_mortgages": 1, "futures_claims_open_pence": 41, "futures_claims_total": 5,
		"futures_claims_written_off": 2, "lent_outstanding_pence": 9009,
		"smuggling_convictions": 6, "last_default_at": nil, "bankrupt_at": nil, "last_conviction_at": nil,
	})
	if err := database.RecordRequest(context.Background(), "one", "GET", "/v1/loan/credit?company_id="+id, status, time.Millisecond, nil, raw, ""); err != nil {
		t.Fatal(err)
	}
}

func TestRivalsMoneyOverviewPersistsCreditRisingShipsAndExpansion(t *testing.T) {
	database, dsn, key := integrationStore(t)
	ctx := context.Background()
	saveTestAccount(t, database, "one")
	// No profile for this other registered account: the explicit credit identity
	// must still prevent our own secondary company being reported as a rival.
	saveTestAccount(t, database, "secondary")
	rivalsMoneySaveProfile(t, database, "one", "company-one", "Ours")
	at := time.Now().UTC().Truncate(time.Second).Add(-time.Minute)
	rivalsMoneyPublication(t, database, at.Add(-24*time.Hour), 2_000_000, nil)
	rivalsMoneyPublication(t, database, at, 1_000_000, rivalsMoneyRising(at, 4, 50))
	rivalsMoneyCredit(t, database, "rival-a", "Rival A", 200)
	rivalsMoneyCredit(t, database, "company-secondary", "Own secondary", 200)
	// A failed lookup is not a clean credit record even if its body looks valid.
	rivalsMoneyCredit(t, database, "rival-b", "Rival B", 429)
	for i, notice := range []struct {
		ship, class string
		age         time.Duration
	}{{"Swift", "hoy", time.Hour}, {"Swift", "hoy", 2 * time.Hour}, {"Kite", "cutter", 3 * time.Hour},
		{"Old Swift", "hoy", 25 * time.Hour}, {"Expired", "barque", 7*24*time.Hour + time.Second}, {"Future", "fluyt", -time.Hour}} {
		source := at.Add(-notice.age)
		rivalTestEvent(t, database, "money-sighting-"+time.Duration(i).String(), "ship_sailed", source, at,
			map[string]any{"company_name": "Rival A", "ship_name": notice.ship, "ship_class": notice.class,
				"port_id": "amsterdam", "other_port_id": "london", "direction": "departure"})
	}
	if err := database.SaveSnapshot(ctx, "one", map[string]json.RawMessage{
		"profile": rivalsMoneyJSON(t, map[string]any{"company": map[string]string{"id": "company-one", "name": "Ours"}}),
		"expansion": rivalsMoneyJSON(t, map[string]any{"unlocked_phase": 2, "active_trading_companies": 4,
			"completed_tiers": []any{}, "current_tier": map[string]any{
				"target_phase": 3, "title": "Atlantic", "completed": false, "completion_bonus_pence": 99_999_999,
				"contributors": []any{map[string]any{"company_name": "Rival A", "tons": 123, "share_bps": 4567}},
			}}),
		"_meta": rivalsMoneyJSON(t, map[string]any{"expansion": map[string]any{"updated_at": at, "complete": true}}),
	}); err != nil {
		t.Fatal(err)
	}
	// Raw diagnostics can be pruned. These facts must live in the new permanent
	// tables and survive opening a new Store, not just a process-local cache.
	if err := database.db.Exec("DELETE FROM requests").Error; err != nil {
		t.Fatal(err)
	}
	reopened, err := Open(ctx, dsn, key, zap.NewNop())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = reopened.Close() })
	report, err := reopened.Rivals(ctx, "one", time.Now().UTC().Add(time.Second))
	if err != nil {
		t.Fatal(err)
	}
	var wire struct {
		Rivals []rivalsMoneyRow `json:"rivals"`
		Rising struct {
			Ranked int `json:"ranked_players"`
			You    struct {
				Growth int64 `json:"growth_bps"`
			} `json:"you"`
		} `json:"rising"`
	}
	if err := json.Unmarshal(rivalsMoneyJSON(t, report), &wire); err != nil {
		t.Fatal(err)
	}
	rows := map[string]rivalsMoneyRow{}
	for _, row := range wire.Rivals {
		rows[row.CompanyName] = row
		if row.CompanyName == "Ours" || row.CompanyName == "Own secondary" {
			t.Errorf("registered own company leaked into rivals: %+v", row)
		}
	}
	a, ok := rows["Rival A"]
	if !ok || a.CompanyID == nil || *a.CompanyID != "rival-a" || a.Credit == nil || a.CreditAt == nil {
		t.Errorf("published credit identity/timestamp not persisted: %+v", a)
	}
	for field, want := range map[string]string{
		"active_loans": "2", "active_owed_pence": "1001", "defaulted_open_loans": "1", "defaulted_owed_pence": "300",
		"loans_repaid": "7", "loans_defaulted": "4", "loans_written_off": "2", "loan_written_off_pence": "700",
		"loans_walked_away": "3", "loan_walked_away_pence": "500", "open_mortgages": "1",
		"futures_claims_open_pence": "41", "futures_claims_total": "5", "futures_claims_written_off": "2",
		"lent_outstanding_pence": "9009", "smuggling_convictions": "6",
	} {
		if string(a.Credit[field]) != want {
			t.Errorf("credit.%s=%s, want %s", field, a.Credit[field], want)
		}
	}
	if a.Worth == nil || *a.Worth != 1_010_000 || a.Change == nil || *a.Change != 0 ||
		a.RisingRank == nil || *a.RisingRank != 1 || a.GrowthBPS == nil || *a.GrowthBPS != 300 || a.Gained == nil || *a.Gained != 30_000 {
		t.Errorf("valuation and rising fields are not independently measured: %+v", a)
	}
	if !reflect.DeepEqual(a.Ships24h, map[string]int{"hoy": 1, "cutter": 1}) || !reflect.DeepEqual(a.Ships7d, map[string]int{"hoy": 2, "cutter": 1}) {
		t.Errorf("ship windows must deduplicate names, not notices: 24h=%v 7d=%v", a.Ships24h, a.Ships7d)
	}
	if len(a.Expansion) != 1 || a.Expansion[0].Phase != 3 || a.Expansion[0].Tons != 123 || a.Expansion[0].Share != 4567 {
		t.Errorf("public contribution is missing or confused with a completion bonus: %+v", a.Expansion)
	}
	if wire.Rising.Ranked != 4 || wire.Rising.You.Growth != 50 {
		t.Errorf("rising publication did not survive diagnostic pruning: %+v", wire.Rising)
	}
	for _, name := range []string{"Rival B", "Rival C"} {
		if row, ok := rows[name]; !ok || row.Credit != nil || row.CreditAt != nil {
			t.Errorf("missing/failed credit became an observed zero record for %s: %+v", name, row)
		}
	}
}

func TestRivalsMoneyObservedZeroCreditIsNotMissing(t *testing.T) {
	database, _, _ := integrationStore(t)
	saveTestAccount(t, database, "one")
	rivalsMoneySaveProfile(t, database, "one", "company-one", "Ours")
	at := time.Now().UTC().Add(-time.Minute)
	rivalsMoneyPublication(t, database, at, 1_000_000, nil)
	credit := json.RawMessage(`{"company_id":"rival-a","company_name":"Rival A","active_loans":0,"active_owed_pence":0,"defaulted_open_loans":0,"defaulted_owed_pence":0,"loans_repaid":0,"loans_defaulted":0,"loans_written_off":0,"loan_written_off_pence":0,"loans_walked_away":0,"loan_walked_away_pence":0,"open_mortgages":0,"futures_claims_open_pence":0,"futures_claims_total":0,"futures_claims_written_off":0,"lent_outstanding_pence":0,"smuggling_convictions":0,"last_default_at":null,"bankrupt_at":null,"last_conviction_at":null}`)
	if err := database.RecordRequest(context.Background(), "one", "GET", "/v1/loan/credit?company_id=rival-a", 200, time.Millisecond, nil, credit, ""); err != nil {
		t.Fatal(err)
	}
	report, err := database.Rivals(context.Background(), "one", time.Now().UTC().Add(time.Second))
	if err != nil {
		t.Fatal(err)
	}
	var wire struct {
		Rivals []rivalsMoneyRow `json:"rivals"`
	}
	if err := json.Unmarshal(rivalsMoneyJSON(t, report), &wire); err != nil {
		t.Fatal(err)
	}
	for _, row := range wire.Rivals {
		if row.CompanyName == "Rival A" {
			if row.Credit == nil || row.CreditAt == nil || string(row.Credit["loans_defaulted"]) != "0" || string(row.Credit["active_owed_pence"]) != "0" {
				t.Fatalf("a successful zero credit report must remain distinguishable from missing credit: %+v", row)
			}
			return
		}
	}
	t.Fatal("observed rival missing from report")
}

func TestRivalsMoneyFallingBehindUsesRisingBeforeValuation(t *testing.T) {
	for _, test := range []struct {
		name        string
		ranked      int
		ours        int64
		ownWorth    int64
		wantWarning bool
		basis       string
	}{
		{"positive growth below median despite positive valuation", 4, 50, 3_000_000, true, "rising"},
		{"equal growth is not behind despite valuation loss", 4, 200, 1_000_000, false, "rising"},
		{"above median is not behind despite valuation loss", 4, 400, 1_000_000, false, "rising"},
		{"three ranked players activates rising", 3, 50, 3_000_000, true, "rising"},
		{"two ranked players falls back to 24h loss", 2, 400, 1_000_000, true, "valuation_24h"},
		{"unpublished rising falls back to 24h loss", 0, 0, 1_000_000, true, "valuation_24h"},
	} {
		t.Run(test.name, func(t *testing.T) {
			database, _, _ := integrationStore(t)
			saveTestAccount(t, database, "one")
			rivalsMoneySaveProfile(t, database, "one", "company-one", "Ours")
			at := time.Date(2026, 10, 28, 12, 0, 0, 0, time.UTC)
			// Rivals all have zero 24h valuation change. The same fixture deliberately
			// disagrees with rising, so an implementation cannot silently use both.
			rivalsMoneyPublication(t, database, at.Add(-24*time.Hour), 2_000_000, nil)
			board := rivalsMoneyRising(at, test.ranked, test.ours)
			if test.ranked == 0 {
				board = map[string]any{"ranked_players": 0, "entries": []any{}, "window_start_at": nil, "you": nil}
			}
			rivalsMoneyPublication(t, database, at, test.ownWorth, board)
			report, err := database.WarningReport(context.Background(), at.Add(time.Minute))
			if err != nil {
				t.Fatal(err)
			}
			if len(report.Accounts) != 1 || !report.Accounts[0].Available {
				t.Fatalf("warning account unavailable: %+v", report)
			}
			warning := warningsAcceptanceFind(report.Accounts[0].Warnings, "falling_behind")
			if (warning != nil) != test.wantWarning {
				t.Fatalf("falling_behind=%+v, want %v from %s", warning, test.wantWarning, test.basis)
			}
			if warning == nil {
				return
			}
			if warning.Names["basis"] != test.basis {
				t.Errorf("warning basis=%q, want %q", warning.Names["basis"], test.basis)
			}
			if test.basis == "rising" && (warning.Facts["ours_growth_bps"] != test.ours || warning.Facts["median_growth_bps"] != 200 || warning.Facts["ranked_players"] != int64(test.ranked)) {
				t.Errorf("rising warning lost percentage units/population: %+v", warning)
			}
			if test.basis == "valuation_24h" && (warning.Facts["ours_pence"] != -1_000_000 || warning.Facts["median_pence"] != 0) {
				t.Errorf("fallback no longer uses matched 24h valuations: %+v", warning)
			}
		})
	}
}

func TestRivalsMoneyDecisionIDsSurvivePrependingAcrossReadSurfaces(t *testing.T) {
	database, _, _ := integrationStore(t)
	saveTestAccount(t, database, "one")
	ctx := context.Background()
	at := time.Date(2026, 10, 28, 12, 0, 0, 0, time.UTC)
	for range 2 {
		if err := database.db.Create(&Decision{AccountID: "one", ShipID: "same", Kind: "rejected", Reason: "same", Details: json.RawMessage(`{}`), CreatedAt: at}).Error; err != nil {
			t.Fatal(err)
		}
	}
	read := func() []int64 {
		t.Helper()
		history, err := database.History(ctx, "one", "decisions", 100)
		if err != nil {
			t.Fatal(err)
		}
		var rows []struct {
			ID int64 `json:"id"`
		}
		if err := json.Unmarshal(rivalsMoneyJSON(t, history), &rows); err != nil {
			t.Fatal(err)
		}
		ids := make([]int64, len(rows))
		for i, row := range rows {
			ids[i] = row.ID
			if row.ID <= 0 {
				t.Errorf("decision has no persistent database id: %+v", rows)
			}
		}
		return ids
	}
	before := read()
	if len(before) != 2 || before[0] == before[1] {
		t.Errorf("identical timestamp/payload rows need distinct ids: %v", before)
	}
	if err := database.db.Create(&Decision{AccountID: "one", Kind: "held", Reason: "new", Details: json.RawMessage(`{}`), CreatedAt: at.Add(time.Second)}).Error; err != nil {
		t.Fatal(err)
	}
	after := read()
	if len(after) != 3 || !reflect.DeepEqual(before, after[1:]) {
		t.Errorf("prepending changed existing decision identities: before=%v after=%v", before, after)
	}
	dashboard, err := database.Dashboard(ctx)
	if err != nil {
		t.Fatal(err)
	}
	var wire struct {
		Decisions []struct {
			ID int64 `json:"id"`
		} `json:"decisions"`
	}
	if err := json.Unmarshal(rivalsMoneyJSON(t, dashboard), &wire); err != nil {
		t.Fatal(err)
	}
	if len(wire.Decisions) != len(after) {
		t.Fatalf("dashboard lost decisions: %+v", wire.Decisions)
	}
	for i, row := range wire.Decisions {
		if row.ID != after[i] || row.ID == 0 {
			t.Errorf("history/dashboard disagree on persistent row id: history=%v dashboard=%+v", after, wire.Decisions)
		}
	}
}

func TestRivalsMoneyMigration17OnlyAddsTables(t *testing.T) {
	database, _, _ := integrationStore(t)
	saveTestAccount(t, database, "one")
	raw, err := migrations.ReadFile("migrations/00017_rivals_overview.sql")
	if err != nil {
		t.Fatalf("required new-table migration 00017: %v", err)
	}
	provider, err := goose.NewProvider(goose.DialectPostgres, database.sqlDB,
		fstest.MapFS{"00017_rivals_overview.sql": {Data: raw}},
		goose.WithDisableGlobalRegistry(true), goose.WithDisableVersioning(true))
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	if _, err := provider.Down(ctx); err != nil {
		t.Fatal(err)
	}
	schema := func() map[string]string {
		t.Helper()
		var rows []struct{ Name, Definition string }
		query := `SELECT c.relname AS name, jsonb_build_object(
			'oid', c.oid, 'columns', (SELECT jsonb_agg(to_jsonb(a) ORDER BY a.attnum) FROM pg_attribute a WHERE a.attrelid=c.oid AND a.attnum>0),
			'constraints', (SELECT jsonb_agg(pg_get_constraintdef(x.oid) ORDER BY x.conname) FROM pg_constraint x WHERE x.conrelid=c.oid),
			'indexes', (SELECT jsonb_agg(indexdef ORDER BY indexname) FROM pg_indexes WHERE schemaname=current_schema() AND tablename=c.relname),
			'triggers', (SELECT jsonb_agg(pg_get_triggerdef(x.oid) ORDER BY x.tgname) FROM pg_trigger x WHERE x.tgrelid=c.oid AND NOT x.tgisinternal)
		)::text AS definition FROM pg_class c JOIN pg_namespace n ON n.oid=c.relnamespace
			WHERE n.nspname=current_schema() AND c.relkind='r'`
		if err := database.db.Raw(query).Scan(&rows).Error; err != nil {
			t.Fatal(err)
		}
		result := map[string]string{}
		for _, row := range rows {
			result[row.Name] = row.Definition
		}
		return result
	}
	before := schema()
	if _, err := provider.UpTo(ctx, 17); err != nil {
		t.Fatal(err)
	}
	after := schema()
	if len(after) <= len(before) {
		t.Error("migration 00017 did not create any new tables")
	}
	for name, definition := range before {
		if after[name] != definition {
			t.Errorf("00017 modified an existing table or its indexes: %s", name)
		}
	}
	if _, err := provider.Down(ctx); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(before, schema()) {
		t.Error("00017 down did not restore the untouched existing schema")
	}
	var count int64
	if err := database.db.Table("accounts").Where("id = ?", "one").Count(&count).Error; err != nil || count != 1 {
		t.Errorf("00017 up/down changed existing account data: count=%d err=%v", count, err)
	}
}
