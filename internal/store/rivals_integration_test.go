package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"testing/fstest"
	"time"

	"github.com/DevYukine/go-tradewinds/internal/gameapi"
	"github.com/pressly/goose/v3"
)

func rivalTestEvent(t *testing.T, database *Store, id, kind string, source any, received time.Time, payload any) {
	t.Helper()
	raw := rivalTestJSON(t, map[string]any{"timestamp": source, "payload": payload})
	if err := database.db.Create(&Event{AccountID: "one", EventID: id, EventType: kind, Payload: raw, CreatedAt: received}).Error; err != nil {
		t.Fatal(err)
	}
}

func TestRivalsConsumerRecordedIdentityAndUpstreamEvents(t *testing.T) {
	database, _, _ := integrationStore(t)
	ctx := context.Background()
	saveTestAccount(t, database, "one")
	at := time.Date(2026, 10, 27, 2, 28, 22, 0, time.UTC)
	published := at.Add(-2 * time.Hour)
	other := "a"
	if err := database.SaveSnapshot(ctx, "one", map[string]json.RawMessage{
		"profile": json.RawMessage(`{"company":{"id":"company-one","name":"Our active company"}}`),
		"catalog": json.RawMessage(`{"ports":[{"id":"a","name":"A"},{"id":"b","name":"B"}]}`),
		"ports": rivalTestJSON(t, map[string]any{"b": map[string]any{
			"visibility": "quay", "recent_sailings": []gameapi.SailingNotice{{
				CompanyName: "Subject", ShipName: "Swift", PortID: "b", OtherPortID: &other, Direction: "arrived", SeenAt: at.Add(-time.Minute),
			}},
		}}),
		"_meta": rivalTestJSON(t, map[string]any{"ports/b": map[string]any{"updated_at": at, "complete": true}}),
	}); err != nil {
		t.Fatal(err)
	}
	for _, value := range []standingRow{
		{SeasonNumber: 1, Rank: 1, CompanyName: "Subject", RefreshedAt: published, NetWorthPence: 1500},
		{SeasonNumber: 1, Rank: 2, CompanyName: "Our published best", RefreshedAt: published, NetWorthPence: 1000},
		{SeasonNumber: 1, Rank: 3, CompanyName: "Our active company", RefreshedAt: published, NetWorthPence: 900},
		{SeasonNumber: 1, Rank: 1, CompanyName: "Subject", RefreshedAt: published.Add(-24 * time.Hour), NetWorthPence: 1800},
		{SeasonNumber: 1, Rank: 2, CompanyName: "Our published best", RefreshedAt: published.Add(-24 * time.Hour), NetWorthPence: 800},
		{SeasonNumber: 1, Rank: 1, CompanyName: "Subject", RefreshedAt: published.Add(-72 * time.Hour), NetWorthPence: 500},
		{SeasonNumber: 2, Rank: 1, CompanyName: "Future season", RefreshedAt: at.Add(time.Second), NetWorthPence: 9999},
	} {
		value.Titles, value.ObservedAt = json.RawMessage(`[]`), at
		if err := database.db.Create(&value).Error; err != nil {
			t.Fatal(err)
		}
	}
	if err := database.db.Create(&standingAccount{AccountID: "one", SeasonNumber: 1, RefreshedAt: published, Rank: 2}).Error; err != nil {
		t.Fatal(err)
	}
	destination := "b"
	rivalTestEvent(t, database, "ours", "ship_sailed", at.Add(-time.Minute), at, gameapi.SailingNotice{
		CompanyName: "Our active company", ShipName: "Lapras", PortID: "a", OtherPortID: &destination, Direction: "departed",
	})
	for _, value := range []struct {
		id, name string
		source   any
	}{
		{"invalid-clock", "Invalid clock phantom", "not-a-source-time"},
		{"old-replayed", "Old phantom", at.Add(-25 * time.Hour)},
		{"future-clock", "Future phantom", at.Add(time.Second)},
	} {
		rivalTestEvent(t, database, value.id, "ship_sailed", value.source, at, gameapi.SailingNotice{
			CompanyName: value.name, ShipName: "Ghost", PortID: "a", Direction: "departed",
		})
	}
	rivalTestEvent(t, database, "latest-credit", "loan_request_posted", at.Add(-5*time.Minute), at, map[string]any{
		"borrower_name": "Subject", "borrower_company_id": "subject", "borrower_record": map[string]int{"loans_defaulted": 2, "loans_walked_away": 1},
	})
	rivalTestEvent(t, database, "old-replayed-grant", "monopoly_granted", at.Add(-rivalEvidenceWindow-time.Second), at, map[string]string{"holder_company_name": "Subject"})
	// The dashboard projection must not fall back to scanning request diagnostics.
	if err := database.db.Exec("ALTER TABLE requests RENAME TO rival_requests_unavailable").Error; err != nil {
		t.Fatal(err)
	}
	report, err := database.Rivals(ctx, "one", at)
	if err != nil {
		t.Fatal(err)
	}
	if len(report.Rivals) != 1 || report.Our == nil || report.Our.CompanyName != "Our published best" || report.Our.ChangePence == nil || *report.Our.ChangePence != 200 {
		t.Fatalf("recorded best-company identity or source-time filtering failed: %+v", report)
	}
	row := rivalTestRow(t, report, "Subject")
	if row.Hunch != rivalMostDangerous || row.ChangePence == nil || *row.ChangePence != -300 || row.SharedLanes == nil || *row.SharedLanes != 1 || row.SharedPorts == nil || *row.SharedPorts != 0 {
		t.Fatalf("snapshot/event union lost exact lane or valuation evidence: %+v", row)
	}
	if report.ValuationAt == nil || !report.ValuationAt.Equal(published) || report.BaselineAt == nil || !report.BaselineAt.Equal(published.Add(-24*time.Hour)) || !report.From.Equal(at.Add(-CompetitionWindow)) || report.Coverage != "partial" {
		t.Fatalf("recorded source clocks or coverage were replaced: %+v", report)
	}
	if report.Verdicts.PlaysRough.CompanyName == nil || *report.Verdicts.PlaysRough.CompanyName != "Subject" || report.Verdicts.PlaysRough.Fact == nil || report.Verdicts.PlaysRough.Fact.Kind != "loan_walkaways" || report.Verdicts.PlaysRough.Fact.Count == nil || *report.Verdicts.PlaysRough.Fact.Count != 1 {
		t.Fatalf("inner credit payload was not projected independently of the row hunch: %+v", report.Verdicts)
	}
	if _, err := database.Rivals(ctx, "missing", at); !errors.Is(err, ErrNotFound) {
		t.Fatalf("unknown account became an empty successful report: %v", err)
	}
}

func TestRivalsConsumerMonopolySnapshotMetadata(t *testing.T) {
	database, _, _ := integrationStore(t)
	ctx := context.Background()
	saveTestAccount(t, database, "one")
	at := time.Date(2026, 10, 27, 2, 28, 22, 0, time.UTC)
	value := standingRow{SeasonNumber: 1, RefreshedAt: at, Rank: 1, CompanyName: "Subject", NetWorthPence: 500, Titles: json.RawMessage(`[]`), ObservedAt: at}
	if err := database.db.Create(&value).Error; err != nil {
		t.Fatal(err)
	}
	for _, test := range []struct {
		name     string
		updated  time.Duration
		upstream *time.Time
		complete bool
		stale    bool
		error    string
		valid    bool
	}{
		{name: "fresh complete", updated: -time.Minute, complete: true, valid: true},
		{name: "thirty minutes inclusive", updated: -30 * time.Minute, complete: true, valid: true},
		{name: "old upstream despite fresh capture", updated: -time.Minute, upstream: new(at.Add(-31 * time.Minute)), complete: true},
		{name: "future upstream", updated: -time.Minute, upstream: new(at.Add(time.Second)), complete: true},
		{name: "future capture cannot be masked by older upstream", updated: time.Second, upstream: new(at.Add(-time.Minute)), complete: true},
		{name: "incomplete", updated: -time.Minute},
		{name: "stale retained snapshot", updated: -time.Minute, complete: true, stale: true},
		{name: "failed refresh retains old bytes", updated: -time.Minute, complete: true, error: "recorded fetch failure"},
	} {
		t.Run(test.name, func(t *testing.T) {
			metadata := map[string]any{"updated_at": at.Add(test.updated), "complete": test.complete, "stale": test.stale, "error": test.error}
			if test.upstream != nil {
				metadata["upstream_as_of"] = test.upstream
			}
			if err := database.SaveSnapshot(ctx, "one", map[string]json.RawMessage{
				"profile": json.RawMessage(`{"company":{"id":"company-one","name":"Ours"}}`),
				"catalog": json.RawMessage(`{"ports":[]}`),
				"monopolies": rivalTestJSON(t, []any{map[string]any{
					"holder_company_name": "Subject", "holder_company_id": "subject", "commodity_id": "raw_wool", "banking_region": "channel", "in_force": true,
					"starts_at": at.Add(-time.Hour), "expires_at": at.Add(time.Hour),
				}}),
				"_meta": rivalTestJSON(t, map[string]any{"monopolies": metadata}),
			}); err != nil {
				t.Fatal(err)
			}
			report, err := database.Rivals(ctx, "one", at)
			if err != nil {
				t.Fatal(err)
			}
			row := rivalTestRow(t, report, "Subject")
			if (row.Hunch == rivalPlaysRough) != test.valid || (report.Verdicts.PlaysRough.CompanyName != nil) != test.valid {
				t.Fatalf("source metadata misclassified retained monopoly bytes: %+v", report)
			}
		})
	}
}

func TestRivalsConsumerEvidenceCapUsesReceiptAndPhysicalID(t *testing.T) {
	database, _, _ := integrationStore(t)
	ctx := context.Background()
	saveTestAccount(t, database, "one")
	at := time.Date(2026, 10, 27, 2, 28, 22, 0, time.UTC)
	value := standingRow{SeasonNumber: 1, RefreshedAt: at, Rank: 1, CompanyName: "Subject", NetWorthPence: 500, Titles: json.RawMessage(`[]`), ObservedAt: at}
	if err := database.db.Create(&value).Error; err != nil {
		t.Fatal(err)
	}
	rivalTestEvent(t, database, "zz-oldest-grant", "monopoly_granted", at, at, map[string]string{"holder_company_name": "Subject"})
	if err := database.db.Exec(`INSERT INTO events (account_id, event_id, event_type, payload, created_at)
		SELECT 'one', 'a-credit-' || n, 'loan_request_posted', jsonb_build_object(
			'timestamp', ?::timestamptz, 'payload', jsonb_build_object(
				'borrower_name', 'Subject', 'borrower_company_id', 'subject',
				'borrower_record', jsonb_build_object('loans_defaulted', 0, 'loans_walked_away', 0))), ?
		FROM generate_series(1, ?) AS n`, at, at, maxCompetitionEvents).Error; err != nil {
		t.Fatal(err)
	}
	report, err := database.Rivals(ctx, "one", at)
	if err != nil {
		t.Fatal(err)
	}
	if !report.EventsTruncated || report.Verdicts.PlaysRough.CompanyName != nil || rivalTestRow(t, report, "Subject").Hunch == rivalPlaysRough {
		t.Fatalf("bounded evidence read ordered by SSE ID instead of physical receipt ID: %+v", report)
	}
	var input RivalsInput
	input.AccountID, input.At = "one", at
	if err := rivalEvidence(database.db, &input); err != nil {
		t.Fatal(err)
	}
	if len(input.Evidence) != maxCompetitionEvents || !input.EventsTruncated {
		t.Fatalf("evidence read exceeded its bound: %d, truncated=%v", len(input.Evidence), input.EventsTruncated)
	}
}

func TestRivalsConsumerReadErrorsNeverReturnPartialReport(t *testing.T) {
	database, _, _ := integrationStore(t)
	ctx := context.Background()
	saveTestAccount(t, database, "one")
	at := time.Now().UTC()
	if _, err := database.Rivals(ctx, "one", time.Time{}); !errors.Is(err, ErrInvalidInput) {
		t.Fatalf("zero source clock accepted: %v", err)
	}
	if _, err := database.Rivals(ctx, " ", at); !errors.Is(err, ErrInvalidInput) {
		t.Fatalf("blank account accepted: %v", err)
	}
	cancelled, cancel := context.WithCancel(ctx)
	cancel()
	if report, err := database.Rivals(cancelled, "one", at); err == nil || report.AccountID != "" || report.Rivals != nil {
		t.Fatalf("cancelled read became a partial success: %+v, %v", report, err)
	}
	if err := database.db.Exec("ALTER TABLE standings_history RENAME TO rival_standings_unavailable").Error; err != nil {
		t.Fatal(err)
	}
	if report, err := database.Rivals(ctx, "one", at); err == nil || report.AccountID != "" || report.Rivals != nil {
		t.Fatalf("failed database read became a successful empty report: %+v, %v", report, err)
	}
}

func TestRivalsMigrationUpDownPreservesRecordedEvidence(t *testing.T) {
	database, _, _ := integrationStore(t)
	ctx := context.Background()
	saveTestAccount(t, database, "one")
	at := time.Now().UTC()
	rivalTestEvent(t, database, "retained-grant", "monopoly_granted", at, at, map[string]string{"holder_company_name": "Subject"})
	raw, err := migrations.ReadFile("migrations/00010_rival_evidence.sql")
	if err != nil {
		t.Fatal(err)
	}
	// Exercise this SQL migration in isolation; the permanent-history Go down
	// migrations intentionally cannot be run, and are unrelated to this index.
	provider, err := goose.NewProvider(goose.DialectPostgres, database.sqlDB,
		fstest.MapFS{"00010_rival_evidence.sql": {Data: raw}},
		goose.WithDisableGlobalRegistry(true), goose.WithDisableVersioning(true))
	if err != nil {
		t.Fatal(err)
	}
	for range 2 {
		for range 2 {
			if _, err := provider.Down(ctx); err != nil {
				t.Fatal(err)
			}
		}
		var index sql.NullString
		if err := database.db.Raw("SELECT to_regclass('events_rival_evidence_idx')::text").Row().Scan(&index); err != nil || index.Valid {
			t.Fatalf("goose 10 down did not remove only its index: %+v, %v", index, err)
		}
		for range 2 {
			if _, err := provider.UpTo(ctx, 10); err != nil {
				t.Fatal(err)
			}
		}
		var definition string
		if err := database.db.Raw("SELECT indexdef FROM pg_indexes WHERE schemaname = current_schema() AND indexname = 'events_rival_evidence_idx'").Scan(&definition).Error; err != nil {
			t.Fatal(err)
		}
		for _, fragment := range []string{"(account_id, created_at DESC, id DESC)", "WHERE", "monopoly_granted", "loan_request_posted"} {
			if !strings.Contains(definition, fragment) {
				t.Fatalf("goose 10 up lost evidence query support %q: %s", fragment, definition)
			}
		}
		var ready bool
		if err := database.db.Raw("SELECT indisvalid AND indisready FROM pg_index WHERE indexrelid = 'events_rival_evidence_idx'::regclass").Row().Scan(&ready); err != nil || !ready {
			t.Fatalf("index was not valid and ready: ready=%v, %v", ready, err)
		}
		var count int64
		if err := database.db.Table("events").Where("event_id = ?", "retained-grant").Count(&count).Error; err != nil || count != 1 {
			t.Fatalf("index round trip changed retained evidence: %d, %v", count, err)
		}
		if err := database.db.Raw("SELECT to_regclass('events_competition_history_idx')::text").Row().Scan(&index); err != nil || !index.Valid {
			t.Fatalf("index round trip removed the existing traffic index: %+v, %v", index, err)
		}
	}
}
