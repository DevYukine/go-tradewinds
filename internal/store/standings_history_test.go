package store

import (
	"context"
	"encoding/json"
	"errors"
	"testing"
	"time"

	"go.uber.org/zap"
	"gorm.io/gorm"
)

func standingsFixture(t *testing.T, at time.Time, leader, ours int64) json.RawMessage {
	t.Helper()
	raw, err := json.Marshal(map[string]any{
		"season_number": 1, "refreshed_at": at, "next_refresh_at": at.Add(time.Hour),
		"entries": []map[string]any{
			{"rank": 1, "company_name": "Leader", "home_port_id": "le_havre", "net_worth_pence": leader, "titles": []string{}},
			{"rank": 2, "company_name": "Our fleet", "home_port_id": "le_havre", "net_worth_pence": ours, "titles": []string{}},
		},
		"you": map[string]any{"rank": 2, "company_name": "Our fleet", "net_worth_pence": ours, "is_calling_company": true},
	})
	if err != nil {
		t.Fatal(err)
	}
	return raw
}

func recordStandingsFixture(t *testing.T, db *Store, account string, raw json.RawMessage) {
	t.Helper()
	if err := db.RecordRequest(context.Background(), account, "GET", "/v1/world/leaderboard", 200, time.Millisecond, nil, raw, ""); err != nil {
		t.Fatal(err)
	}
}

// A standings write failure must not roll back the raw leaderboard response,
// which is the evidence needed to diagnose it.
func TestStandingsHistoryFailureKeepsRawLeaderboardResponse(t *testing.T) {
	db, _, _ := integrationStore(t)
	ctx := context.Background()
	saveTestAccount(t, db, "one")
	trigger := `CREATE FUNCTION reject_test_standings() RETURNS trigger LANGUAGE plpgsql AS $$
BEGIN RAISE EXCEPTION 'standings rejected'; END $$;
CREATE TRIGGER reject_test_standings BEFORE INSERT ON standings_history FOR EACH ROW EXECUTE FUNCTION reject_test_standings();`
	if err := db.db.Exec(trigger).Error; err != nil {
		t.Fatal(err)
	}
	raw := standingsFixture(t, time.Now().UTC().Truncate(time.Hour), 10_000, 2_000)
	err := db.RecordRequest(ctx, "one", "GET", "/v1/world/leaderboard", 200, time.Millisecond, nil, raw, "")
	if err == nil {
		t.Fatal("a failed standings write was not reported")
	}
	var requests int64
	if err := db.db.Table("requests").Where("account_id = ? AND path = ?", "one", "/v1/world/leaderboard").Count(&requests).Error; err != nil {
		t.Fatal(err)
	}
	if requests != 1 {
		t.Fatalf("standings failure rolled back the raw leaderboard response: %d rows", requests)
	}
}

func TestStandingsHistoryDeduplicationRetentionAndRestart(t *testing.T) {
	db, dsn, key := integrationStore(t)
	ctx := context.Background()
	saveTestAccount(t, db, "one")
	saveTestAccount(t, db, "two")
	base := time.Now().UTC().Add(-400 * 24 * time.Hour).Truncate(time.Hour)
	first := standingsFixture(t, base, 10_000, 2_000)
	for _, account := range []string{"one", "one", "two"} {
		recordStandingsFixture(t, db, account, first)
	}
	// A retry with changed bytes cannot rewrite an official hourly valuation.
	recordStandingsFixture(t, db, "one", standingsFixture(t, base, 999_999, 9_999))
	recordStandingsFixture(t, db, "one", standingsFixture(t, base.Add(time.Hour), 11_000, 1_500))
	var count int64
	if err := db.db.Table("standings_history").Count(&count).Error; err != nil || count != 4 {
		t.Fatalf("snapshots were not deduplicated across polls/accounts: %d, %v", count, err)
	}
	if err := db.db.Table("requests").Where("account_id IN ?", []string{"one", "two"}).Update("created_at", base).Error; err != nil {
		t.Fatal(err)
	}
	if err := db.Prune(ctx); err != nil {
		t.Fatal(err)
	}
	if err := db.db.Table("requests").Count(&count).Error; err != nil || count != 0 {
		t.Fatalf("raw diagnostics did not expire: %d, %v", count, err)
	}
	restarted, err := Open(ctx, dsn, key, zap.NewNop())
	if err != nil {
		t.Fatal(err)
	}
	defer restarted.Close()
	history, err := restarted.StandingsHistory(ctx, "one", time.Time{}, base.Add(2*time.Hour), 1)
	if err != nil || len(history.Players) != 2 || !history.From.Equal(base) {
		t.Fatalf("permanent history lost after pruning/restart: %+v, %v", history, err)
	}
	for _, player := range history.Players {
		if player.Observations != 2 || len(player.Points) != 2 || !player.Charted {
			t.Fatalf("top player or our out-of-top player missing: %+v", player)
		}
		wantChange := int64(1_000)
		if player.CompanyName == "Our fleet" {
			wantChange = -500
			if !player.IsUs {
				t.Fatal("calling player was not highlighted")
			}
		}
		if player.ChangePence == nil || *player.ChangePence != wantChange {
			t.Fatalf("signed exact valuation change: %+v", player)
		}
	}
}

func TestStandingsHistoryInclusiveBoundsMissingAndSeparateSeasons(t *testing.T) {
	db, _, _ := integrationStore(t)
	ctx := context.Background()
	saveTestAccount(t, db, "one")
	saveTestAccount(t, db, "unrelated")
	base := time.Date(2026, 10, 20, 12, 0, 0, 0, time.UTC)
	for index := -1; index <= 2; index++ {
		recordStandingsFixture(t, db, "one", standingsFixture(t, base.Add(time.Duration(index)*time.Hour), int64(10_000+index*100), int64(2_000-index*100)))
	}
	history, err := db.StandingsHistory(ctx, "one", base, base.Add(time.Hour), 1)
	if err != nil || len(history.Players) != 2 {
		t.Fatalf("read inclusive range: %+v, %v", history, err)
	}
	for _, player := range history.Players {
		if !player.FirstAt.Equal(base) || !player.LastAt.Equal(base.Add(time.Hour)) || player.Observations != 2 {
			t.Fatalf("outside observation entered the range: %+v", player)
		}
		for _, point := range player.Points {
			if point.At.Before(base) || point.At.After(base.Add(time.Hour)) {
				t.Fatalf("sample outside inclusive bounds: %+v", point)
			}
		}
	}
	single, err := db.StandingsHistory(ctx, "unrelated", base, base, 1)
	if err != nil || len(single.Players) != 2 {
		t.Fatalf("single observation: %+v, %v", single, err)
	}
	for _, player := range single.Players {
		if player.IsUs || player.ChangePence != nil {
			t.Fatalf("invented ownership or single-observation change: %+v", player)
		}
	}
	empty, err := db.StandingsHistory(ctx, "one", base.Add(-3*time.Hour), base.Add(-2*time.Hour), 5)
	if err != nil || len(empty.Players) != 0 || empty.LatestAt != nil {
		t.Fatalf("empty range invented observations: %+v, %v", empty, err)
	}
	var nextSeason map[string]any
	if err := json.Unmarshal(standingsFixture(t, base.Add(3*time.Hour), 1, 1), &nextSeason); err != nil {
		t.Fatal(err)
	}
	nextSeason["season_number"] = 2
	raw, _ := json.Marshal(nextSeason)
	recordStandingsFixture(t, db, "one", raw)
	all, err := db.StandingsHistory(ctx, "one", time.Time{}, base.Add(4*time.Hour), 5)
	if err != nil || len(all.Players) != 4 {
		t.Fatalf("seasons were incorrectly stitched: %+v, %v", all, err)
	}
}

func TestStandingsHistorySamplesLongRangesWithoutChangingExactChange(t *testing.T) {
	db, _, _ := integrationStore(t)
	saveTestAccount(t, db, "one")
	base := time.Date(2025, 1, 1, 0, 0, 0, 0, time.UTC)
	err := db.db.Transaction(func(tx *gorm.DB) error {
		for index := range 700 {
			at := base.Add(time.Duration(index) * time.Hour)
			if err := recordStandings(tx, "one", standingsFixture(t, at, int64(10_000+index), int64(2_000-index)), at); err != nil {
				return err
			}
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	history, err := db.StandingsHistory(context.Background(), "one", base, base.Add(699*time.Hour), 5)
	if err != nil {
		t.Fatal(err)
	}
	for _, player := range history.Players {
		if player.Observations != 700 || len(player.Points) > 482 || len(player.Points) >= 700 || !player.Points[0].At.Equal(base) {
			t.Fatalf("unbounded sampling or lost exact first point: %+v", player)
		}
		want := int64(699)
		if player.IsUs {
			want = -699
		}
		if player.ChangePence == nil || *player.ChangePence != want {
			t.Fatalf("downsampling changed the exact range change: %+v", player)
		}
	}
}

func TestStandingsHistoryBackfillsRetainedResponsesAndSnapshots(t *testing.T) {
	legacy, dsn, key := privateLegacyStore(t)
	ctx := context.Background()
	saveTestAccount(t, legacy, "one")
	base := time.Date(2026, 10, 20, 12, 0, 0, 0, time.UTC)
	first := standingsFixture(t, base, 10_000, 2_000)
	for _, raw := range []json.RawMessage{first, first, json.RawMessage(`{"truncated":true}`), json.RawMessage(`{"season_number":1,"refreshed_at":"invalid","entries":[]}`)} {
		if err := legacy.db.Create(&Request{
			RequestSummary: RequestSummary{AccountID: "one", Method: "GET", Path: "/v1/world/leaderboard", Status: 200, CreatedAt: base.Add(time.Minute)},
			Request:        json.RawMessage(`{}`), Response: raw,
		}).Error; err != nil {
			t.Fatal(err)
		}
	}
	// Legacy code wrote the whole snapshot into one row; the route table does
	// not exist before its migration, so seed the row the way legacy did.
	legacySnapshot, err := json.Marshal(map[string]json.RawMessage{"leaderboard": standingsFixture(t, base.Add(time.Hour), 11_000, 1_500)})
	if err != nil {
		t.Fatal(err)
	}
	if err := legacy.db.Create(&snapshotRow{AccountID: "one", Data: legacySnapshot, UpdatedAt: base.Add(time.Hour)}).Error; err != nil {
		t.Fatal(err)
	}
	upgraded, err := Open(ctx, dsn, key, zap.NewNop())
	if err != nil {
		t.Fatal(err)
	}
	defer upgraded.Close()
	history, err := upgraded.StandingsHistory(ctx, "one", time.Time{}, base.Add(2*time.Hour), 5)
	if err != nil || len(history.Players) != 2 {
		t.Fatalf("backfill failed: %+v, %v", history, err)
	}
	for _, player := range history.Players {
		if player.Observations != 2 {
			t.Fatalf("duplicate/invalid raw responses affected the backfill: %+v", player)
		}
	}
}

func TestStandingsHistoryRejectsInvalidBounds(t *testing.T) {
	now := time.Now()
	for _, test := range []struct {
		account string
		from    time.Time
		to      time.Time
		top     int
	}{
		{"", time.Time{}, now, 5}, {"one", now.Add(time.Hour), now, 5},
		{"one", time.Time{}, time.Time{}, 5}, {"one", time.Time{}, now, 0}, {"one", time.Time{}, now, 21},
	} {
		if _, err := (*Store)(nil).StandingsHistory(context.Background(), test.account, test.from, test.to, test.top); !errors.Is(err, ErrInvalidInput) {
			t.Fatalf("invalid standings query accepted: %+v, %v", test, err)
		}
	}
}

func TestStandingsHistoryKeepsCallerOutsidePublicStandings(t *testing.T) {
	db, _, _ := integrationStore(t)
	saveTestAccount(t, db, "one")
	at := time.Date(2026, 10, 20, 12, 0, 0, 0, time.UTC)
	var capture map[string]json.RawMessage
	if err := json.Unmarshal(standingsFixture(t, at, 10_000, 2_000), &capture); err != nil {
		t.Fatal(err)
	}
	capture["you"] = json.RawMessage(`{"rank":21,"company_name":"Beyond the top twenty","net_worth_pence":-1200,"is_calling_company":false}`)
	raw, err := json.Marshal(capture)
	if err != nil {
		t.Fatal(err)
	}
	recordStandingsFixture(t, db, "one", raw)
	history, err := db.StandingsHistory(context.Background(), "one", at, at, 1)
	if err != nil || len(history.Players) != 3 {
		t.Fatalf("caller outside the public standings was lost: %+v, %v", history, err)
	}
	for _, player := range history.Players {
		if !player.IsUs {
			continue
		}
		if player.Rank != 21 || !player.Charted || player.LastNetWorthPence != -1200 || len(player.Points) != 1 || player.ChangePence != nil {
			t.Fatalf("outside caller was not charted honestly: %+v", player)
		}
		return
	}
	t.Fatal("caller identity was not retained")
}
