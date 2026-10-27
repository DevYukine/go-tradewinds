package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"strings"
	"testing"
	"testing/fstest"
	"time"

	"github.com/DevYukine/go-tradewinds/internal/gameapi"
	"github.com/pressly/goose/v3"
)

func TestWarningsRecordedArrivalFallback(t *testing.T) {
	database, _, _ := integrationStore(t)
	ctx := context.Background()
	saveTestAccount(t, database, "one")
	at := time.Date(2026, 10, 27, 12, 0, 0, 0, time.UTC)
	arrival := at.Add(-100 * time.Minute)
	port := "hamburg"
	encode := func(value any) json.RawMessage {
		t.Helper()
		raw, err := json.Marshal(value)
		if err != nil {
			t.Fatal(err)
		}
		return raw
	}
	if err := database.SaveSnapshot(ctx, "one", map[string]json.RawMessage{
		"_engine": encode(WarningEngine{ObservedAt: at, Mode: "execute", Holds: []WarningHold{{ShipID: "ship-one", Reason: "waiting for cargo"}}}),
		"_meta": encode(map[string]WarningDataset{
			"ships": {UpdatedAt: at, Complete: true}, "_engine": {UpdatedAt: at, Complete: true},
		}),
		"ships":   encode([]gameapi.ShipDetail{{Ship: gameapi.Ship{ID: "ship-one", Name: "Lapras", Status: "anchored_roadstead", CurrentPortID: &port}}}),
		"catalog": encode(gameapi.Catalog{Ports: []gameapi.Port{{ID: port, Name: "Hamburg"}}}),
	}); err != nil {
		t.Fatal(err)
	}
	addEvent := func(id, kind string, createdAt time.Time) {
		t.Helper()
		event := Event{AccountID: "one", EventID: id, EventType: kind, CreatedAt: createdAt,
			Payload: encode(map[string]any{"timestamp": at, "payload": map[string]string{"ship_id": "ship-one"}})}
		if err := database.db.Create(&event).Error; err != nil {
			t.Fatal(err)
		}
	}
	read := func(want bool) {
		t.Helper()
		accounts, err := database.Warnings(ctx, at)
		if err != nil {
			t.Fatal(err)
		}
		if len(accounts) != 1 {
			t.Fatalf("accounts = %+v", accounts)
		}
		if !want {
			if len(accounts[0].Warnings) != 0 {
				t.Fatalf("arrival before departure is not a current idle stay: %+v", accounts[0].Warnings)
			}
			return
		}
		if len(accounts[0].Warnings) != 2 {
			t.Fatalf("recorded arrival did not support idle and held warnings: %+v", accounts[0].Warnings)
		}
		for _, warning := range accounts[0].Warnings {
			if warning.Code != "fleet_idle" && warning.Code != "hold_stuck" || !warning.Since.Equal(arrival) ||
				warning.Names["since_source"] != "arrival_event" || warning.Names["port"] != "Hamburg" || warning.Facts["minutes"] != 100 {
				t.Fatalf("arrival receipt time or its provenance was lost: %+v", warning)
			}
		}
	}
	addEvent("expired-arrival", "ship_arrived", at.Add(-WarningArrivalLookback-time.Minute))
	read(false)
	addEvent("future-arrival", "ship_arrived", at.Add(time.Minute))
	addEvent("future-departure", "ship_departed", at.Add(2*time.Minute))
	read(false)
	addEvent("arrival", "ship_arrived", arrival)
	read(true)
	addEvent("older-departure", "ship_departed", arrival.Add(-time.Hour))
	read(true)
	addEvent("later-departure", "ship_departed", arrival.Add(time.Minute))
	read(false)
}

func TestWarningsGroupedShipDetails(t *testing.T) {
	database, _, _ := integrationStore(t)
	ctx := context.Background()
	saveTestAccount(t, database, "one")
	at := time.Date(2026, 10, 27, 12, 0, 0, 0, time.UTC)
	berthedAt, unlocksAt := at.Add(-2*time.Hour), at.Add(-100*time.Minute)
	port := "hamburg"
	ship := gameapi.ShipDetail{Ship: gameapi.Ship{ID: "lapras", Name: "Lapras", Status: "berthed", CurrentPortID: &port}}
	detail := ship
	detail.Ship.BerthedAt, detail.Ship.TurnaroundUnlocksAt = &berthedAt, &unlocksAt
	for _, test := range []struct {
		name string
		meta WarningDataset
		want bool
	}{
		{"newer complete grouped detail", WarningDataset{UpdatedAt: at, Complete: true}, true},
		{"older detail", WarningDataset{UpdatedAt: at.Add(-2 * time.Minute), Complete: true}, false},
		{"failed detail", WarningDataset{UpdatedAt: at, Complete: true, Error: "refresh failed"}, false},
	} {
		t.Run(test.name, func(t *testing.T) {
			if err := database.SaveSnapshot(ctx, "one", map[string]json.RawMessage{
				"_engine": rivalTestJSON(t, WarningEngine{ObservedAt: at, Mode: "execute", Holds: []WarningHold{{ShipID: "lapras", Reason: "held"}}}),
				"_meta": rivalTestJSON(t, map[string]WarningDataset{
					"ships": {UpdatedAt: at.Add(-time.Minute), Complete: true}, "_engine": {UpdatedAt: at, Complete: true},
					"ship_details/lapras": test.meta,
				}),
				"ships":        rivalTestJSON(t, []gameapi.ShipDetail{ship}),
				"ship_details": rivalTestJSON(t, map[string]gameapi.ShipDetail{"lapras": detail}),
				"catalog":      rivalTestJSON(t, gameapi.Catalog{Ports: []gameapi.Port{{ID: port, Name: "Hamburg"}}}),
			}); err != nil {
				t.Fatal(err)
			}
			accounts, err := database.Warnings(ctx, at)
			if err != nil {
				t.Fatal(err)
			}
			if len(accounts) != 1 || !accounts[0].Available {
				t.Fatalf("grouped snapshot unavailable: %+v", accounts)
			}
			if !test.want {
				if len(accounts[0].Warnings) != 0 {
					t.Fatalf("stale grouped detail became a known idle state: %+v", accounts[0])
				}
				return
			}
			if len(accounts[0].Warnings) != 2 {
				t.Fatalf("engine-shaped ship_details group was not decoded: %+v", accounts[0])
			}
			for _, warning := range accounts[0].Warnings {
				if !warning.Since.Equal(unlocksAt) || warning.Facts["minutes"] != 100 || warning.Names["since_source"] != "berthed_at" {
					t.Fatalf("newer grouped detail or turnaround clock lost: %+v", warning)
				}
			}
		})
	}
}

func TestWarningsStoredSettlementEligibility(t *testing.T) {
	database, _, _ := integrationStore(t)
	ctx := context.Background()
	saveTestAccount(t, database, "one")
	saveTestAccount(t, database, "other")
	at := time.Date(2026, 10, 27, 12, 0, 0, 0, time.UTC)
	for i, kind := range warningSettlementKinds {
		putGrowthHistoryEvidence(t, database, Settlement{
			ID: fmt.Sprintf("commercial-%d", i), AccountID: "one", Kind: kind, CostsKnown: true,
			ProceedsPence: 10, AcquisitionPence: 70, FeesPence: 10, ProvisionsPence: 10, ExpensesPence: 20,
			CompletedAt: at.Add(-time.Duration(i+1) * time.Hour),
		})
	}
	// More than a query limit of newer excluded rows must not hide the eligible
	// streak, and neither another account nor a future win ends it.
	for i := range 12 {
		putGrowthHistoryEvidence(t, database, Settlement{
			ID: fmt.Sprintf("excluded-%d", i), AccountID: "one", Kind: "relocation", CostsKnown: true,
			ProceedsPence: 9_999, CompletedAt: at.Add(-time.Duration(i+1) * time.Minute),
		})
	}
	for _, row := range []Settlement{
		{ID: "unknown-cost", AccountID: "one", Kind: "spot", ProceedsPence: 9_999, CompletedAt: at},
		{ID: "future-win", AccountID: "one", Kind: "spot", CostsKnown: true, ProceedsPence: 9_999, CompletedAt: at.Add(time.Second)},
		{ID: "other-win", AccountID: "other", Kind: "spot", CostsKnown: true, ProceedsPence: 9_999, CompletedAt: at},
	} {
		putGrowthHistoryEvidence(t, database, row)
	}
	accounts, err := database.Warnings(ctx, at)
	if err != nil {
		t.Fatal(err)
	}
	var loss *Warning
	for _, account := range accounts {
		for i := range account.Warnings {
			if account.Warnings[i].Code == "loss_streak" {
				if account.AccountID != "one" {
					t.Fatalf("settlement warning crossed account scope: %+v", account)
				}
				loss = &account.Warnings[i]
			}
		}
	}
	if loss == nil || loss.Facts["count"] != 7 || loss.Facts["net_pence"] != -700 || !loss.Since.Equal(at.Add(-7*time.Hour)) {
		t.Fatalf("commercial kinds, known costs, cutoff or query limit filtered incorrectly: %+v", loss)
	}
	putGrowthHistoryEvidence(t, database, Settlement{
		ID: "current-win", AccountID: "one", Kind: "spot", CostsKnown: true, ProceedsPence: 1, CompletedAt: at,
	})
	accounts, err = database.Warnings(ctx, at)
	if err != nil {
		t.Fatal(err)
	}
	for _, account := range accounts {
		for _, warning := range account.Warnings {
			if warning.Code == "loss_streak" {
				t.Fatalf("latest known commercial win did not end the streak: %+v", warning)
			}
		}
	}
}

func TestWarningsRecordedGrowthAndScrubbedTopRival(t *testing.T) {
	database, _, _ := integrationStore(t)
	ctx := context.Background()
	credentials := saveTestAccount(t, database, "one")
	at := time.Date(2026, 10, 27, 12, 0, 0, 0, time.UTC)
	if err := database.SaveSnapshot(ctx, "one", map[string]json.RawMessage{
		"profile": rivalTestJSON(t, map[string]any{"company": map[string]string{"id": "company-one", "name": "Ours"}}),
		"catalog": json.RawMessage(`{"ports":[]}`),
	}); err != nil {
		t.Fatal(err)
	}
	names := []string{"Ours", "Rival A", "Rival B", "Merchant " + credentials.APIKey}
	for _, publication := range []struct {
		at     time.Time
		values []int64
	}{
		{at.Add(-24 * time.Hour), []int64{2_000, 1_000, 1_000, 1_000}},
		{at.Add(-6 * time.Hour), []int64{900, 1_050, 1_050, 1_050}},
		{at, []int64{1_000, 1_100, 1_200, 1_300}},
	} {
		for rank, name := range names {
			row := standingRow{
				SeasonNumber: 1, RefreshedAt: publication.at, Rank: rank + 1, CompanyName: name,
				NetWorthPence: publication.values[rank], Titles: json.RawMessage(`[]`), ObservedAt: at,
			}
			if err := database.db.Create(&row).Error; err != nil {
				t.Fatal(err)
			}
		}
		if err := database.db.Create(&standingAccount{AccountID: "one", SeasonNumber: 1, RefreshedAt: publication.at, Rank: 1}).Error; err != nil {
			t.Fatal(err)
		}
	}
	accounts, err := database.Warnings(ctx, at)
	if err != nil {
		t.Fatal(err)
	}
	if len(accounts) != 1 || !accounts[0].Available || len(accounts[0].Warnings) != 1 {
		t.Fatalf("recorded 24 h benchmark did not reach warning assessment: %+v", accounts)
	}
	warning := accounts[0].Warnings[0]
	if warning.Code != "falling_behind" || warning.Facts["ours_pence"] != -1_000 || warning.Facts["median_pence"] != 200 ||
		warning.Facts["rival_count"] != 3 || !warning.Since.Equal(at.Add(-24*time.Hour)) {
		t.Fatalf("warning used something other than the recorded 24 h window: %+v", warning)
	}
	report, err := database.Rivals(ctx, "one", at)
	if err != nil {
		t.Fatal(err)
	}
	if len(report.Growth) != 2 || report.Growth[0].OursPence == nil || *report.Growth[0].OursPence != 100 ||
		report.Growth[1].OursPence == nil || *report.Growth[1].OursPence != -1_000 {
		t.Fatalf("shared publication reader mixed the 6 h and 24 h baselines: %+v", report.Growth)
	}
	for _, growth := range report.Growth {
		if growth.TopRival == nil || !strings.Contains(*growth.TopRival, redacted) || strings.Contains(*growth.TopRival, credentials.APIKey) {
			t.Fatalf("growth top-rival name escaped display scrubbing: %+v", growth)
		}
	}
}

func TestWarningArrivalShipIDsOnlyMissingDockClocks(t *testing.T) {
	at := time.Date(2026, 10, 27, 12, 0, 0, 0, time.UTC)
	dockedAt := at.Add(-time.Hour)
	port := "hamburg"
	base := gameapi.ShipDetail{Ship: gameapi.Ship{ID: "lapras", Status: "berthed", CurrentPortID: &port}}
	for _, test := range []struct {
		name string
		edit func(*WarningInput)
		want bool
	}{
		{"missing clocks", func(*WarningInput) {}, true},
		{"berthed clock", func(input *WarningInput) { input.Ships[0].Ship.BerthedAt = &dockedAt }, false},
		{"queue clock", func(input *WarningInput) { input.Ships[0].Ship.DockQueuedAt = &dockedAt }, false},
		{"sailing", func(input *WarningInput) { input.Ships[0].Ship.Status = "sailing" }, false},
		{"active voyage", func(input *WarningInput) { input.Ships[0].Voyage = &gameapi.VoyageEstimate{} }, false},
		{"unknown port", func(input *WarningInput) { input.Ships[0].Ship.CurrentPortID = nil }, false},
		{"newer detail has clock", func(input *WarningInput) {
			detail := base
			detail.Ship.BerthedAt = &dockedAt
			input.ShipDetails = map[string]gameapi.ShipDetail{"lapras": detail}
			input.Metadata["ship_details/lapras"] = WarningDataset{UpdatedAt: at, Complete: true}
		}, false},
		{"stale detail cannot supply clock", func(input *WarningInput) {
			detail := base
			detail.Ship.BerthedAt = &dockedAt
			input.ShipDetails = map[string]gameapi.ShipDetail{"lapras": detail}
			input.Metadata["ship_details/lapras"] = WarningDataset{UpdatedAt: at, Complete: true, Stale: true}
		}, true},
	} {
		t.Run(test.name, func(t *testing.T) {
			input := WarningInput{
				Ships: []gameapi.ShipDetail{base},
				Metadata: map[string]WarningDataset{
					"ships": {UpdatedAt: at.Add(-time.Minute), Complete: true},
				},
			}
			test.edit(&input)
			ids := warningArrivalShipIDs(&input, at)
			if test.want && (len(ids) != 1 || ids[0] != "lapras") || !test.want && len(ids) != 0 {
				t.Fatalf("fallback IDs = %v, want lookup=%v", ids, test.want)
			}
		})
	}
}

func TestWarningArrivalQueryUsesPartialIndex(t *testing.T) {
	database, _, _ := integrationStore(t)
	saveTestAccount(t, database, "one")
	var version int64
	if err := database.db.Raw("SELECT MAX(version_id) FROM goose_db_version WHERE is_applied").Scan(&version).Error; err != nil || version < 11 {
		t.Fatalf("arrival index migration was not applied: version=%d, %v", version, err)
	}
	at := time.Date(2026, 10, 27, 12, 0, 0, 0, time.UTC)
	if err := database.db.Exec(`INSERT INTO events (account_id, event_id, event_type, payload, created_at)
		SELECT 'one', 'noise-' || n, 'ledger_adjusted', '{}'::jsonb, ?
		FROM generate_series(1, 2000) AS n`, at.Add(-time.Hour)).Error; err != nil {
		t.Fatal(err)
	}
	if err := database.db.Create(&Event{
		AccountID: "one", EventID: "arrival", EventType: "ship_arrived", CreatedAt: at.Add(-time.Hour),
		Payload: json.RawMessage(`{"payload":{"ship_id":"lapras"}}`),
	}).Error; err != nil {
		t.Fatal(err)
	}
	if err := database.db.Exec("ANALYZE events").Error; err != nil {
		t.Fatal(err)
	}
	rows, err := database.db.Raw("EXPLAIN (ANALYZE, BUFFERS, COSTS OFF) "+warningArrivalEventsSQL,
		"one", at.Add(-WarningArrivalLookback), at, []string{"lapras"}, WarningArrivalLimit).Rows()
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	var lines []string
	for rows.Next() {
		var line string
		if err := rows.Scan(&line); err != nil {
			t.Fatal(err)
		}
		lines = append(lines, line)
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	plan := strings.Join(lines, "\n")
	t.Logf("Migrated isolated schema arrival fallback EXPLAIN:\n%s", plan)
	if !strings.Contains(plan, "events_warning_arrivals_idx") || strings.Contains(plan, "events_account_created_idx") {
		t.Fatalf("arrival query did not use its partial arrival/departure index:\n%s", plan)
	}
}

func TestWarningArrivalMigrationUpDownPreservesRecordedEvidence(t *testing.T) {
	database, _, _ := integrationStore(t)
	ctx := context.Background()
	saveTestAccount(t, database, "one")
	if err := database.db.Create(&Event{
		AccountID: "one", EventID: "retained-arrival", EventType: "ship_arrived",
		CreatedAt: time.Now().UTC(), Payload: json.RawMessage(`{"payload":{"ship_id":"lapras"}}`),
	}).Error; err != nil {
		t.Fatal(err)
	}
	raw, err := migrations.ReadFile("migrations/00011_warning_arrivals.sql")
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(raw), "NO TRANSACTION") {
		t.Fatal("arrival index migration must be transactional")
	}
	provider, err := goose.NewProvider(goose.DialectPostgres, database.sqlDB,
		fstest.MapFS{"00011_warning_arrivals.sql": {Data: raw}},
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
		if err := database.db.Raw("SELECT to_regclass('events_warning_arrivals_idx')::text").Row().Scan(&index); err != nil || index.Valid {
			t.Fatalf("goose 11 down did not remove its index: %+v, %v", index, err)
		}
		for range 2 {
			if _, err := provider.UpTo(ctx, 11); err != nil {
				t.Fatal(err)
			}
		}
		var definition string
		if err := database.db.Raw("SELECT indexdef FROM pg_indexes WHERE schemaname = current_schema() AND indexname = 'events_warning_arrivals_idx'").Scan(&definition).Error; err != nil {
			t.Fatal(err)
		}
		for _, fragment := range []string{"(account_id, created_at DESC, id DESC)", "WHERE", "ship_arrived", "ship_departed"} {
			if !strings.Contains(definition, fragment) {
				t.Fatalf("goose 11 up lost arrival query support %q: %s", fragment, definition)
			}
		}
		var ready bool
		if err := database.db.Raw("SELECT indisvalid AND indisready FROM pg_index WHERE indexrelid = 'events_warning_arrivals_idx'::regclass").Row().Scan(&ready); err != nil || !ready {
			t.Fatalf("arrival index was not valid and ready: ready=%v, %v", ready, err)
		}
		var count int64
		if err := database.db.Table("events").Where("event_id = ?", "retained-arrival").Count(&count).Error; err != nil || count != 1 {
			t.Fatalf("arrival index round trip changed retained evidence: %d, %v", count, err)
		}
		for _, name := range []string{"events_account_created_idx", "events_rival_evidence_idx"} {
			if err := database.db.Raw("SELECT to_regclass(?)::text", name).Row().Scan(&index); err != nil || !index.Valid {
				t.Fatalf("arrival index round trip removed %s: %+v, %v", name, index, err)
			}
		}
	}
}

func TestWarningReportSeparatesUnmonitoredRegistration(t *testing.T) {
	database, _, _ := integrationStore(t)
	ctx := context.Background()
	for _, id := range []string{"healthy", "disabled", "unregistered", "no-api-key", "corrupt"} {
		saveTestAccount(t, database, id)
	}
	if err := database.db.Table("accounts").Where("id = ?", "disabled").Update("enabled", false).Error; err != nil {
		t.Fatal(err)
	}
	if err := database.db.Table("accounts").Where("id = ?", "unregistered").Update("company_id", "").Error; err != nil {
		t.Fatal(err)
	}
	if err := database.SaveAccount(ctx, Account{ID: "no-api-key", Label: "Incomplete", Enabled: true, CompanyID: "company-no-api-key"}, Credentials{Handle: "incomplete"}); err != nil {
		t.Fatal(err)
	}
	if err := database.db.Model(&accountSecret{}).Where("account_id = ?", "corrupt").Update("ciphertext", []byte("corrupt")).Error; err != nil {
		t.Fatal(err)
	}
	report, err := database.WarningReport(ctx, time.Now().UTC())
	if err != nil {
		t.Fatal(err)
	}
	want := map[string]string{"disabled": "disabled", "unregistered": "unregistered", "no-api-key": "no_api_key"}
	if len(report.Unmonitored) != len(want) || len(report.Accounts) != 2 {
		t.Fatalf("unexpected warning scope: %+v", report)
	}
	for _, account := range report.Unmonitored {
		if want[account.AccountID] != account.Reason {
			t.Fatalf("unexpected unmonitored account: %+v", account)
		}
		delete(want, account.AccountID)
	}
	for _, account := range report.Accounts {
		if account.AccountID == "corrupt" && (account.Available || account.ErrorCode != WarningCredentialsUnavailable) {
			t.Fatalf("corrupt credentials were treated as unregistered: %+v", account)
		}
		if account.AccountID != "healthy" && account.AccountID != "corrupt" {
			t.Fatalf("unmonitored registration entered warning assessment: %+v", account)
		}
	}
}
