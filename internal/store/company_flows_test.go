package store

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"testing"
	"time"
)

func TestCompanyFlowsSyncIsIdempotentAndAccountScoped(t *testing.T) {
	db, _, _ := integrationStore(t)
	ctx := context.Background()
	const account = "company-flows"
	saveTestAccount(t, db, account)
	saveTestAccount(t, db, "company-other")
	at := time.Now().UTC().Add(-10 * 24 * time.Hour).Truncate(time.Microsecond)
	flows := []CompanyLedgerFlow{
		{LedgerID: "upkeep", Reason: "upkeep", AmountPence: -900, CreatedAt: at},
		{LedgerID: "bounty", Reason: "patrol_bounty", AmountPence: 300, CreatedAt: at.Add(time.Hour)},
	}
	if err := db.SyncCompanyFlows(ctx, account, flows, nil); err != nil {
		t.Fatal(err)
	}
	if err := db.SyncCompanyFlows(ctx, "company-other", flows[:1], nil); err != nil {
		t.Fatal(err)
	}
	// Restart backfill cannot overwrite or duplicate already recorded receipts.
	flows[0].AmountPence = -999
	if err := db.SyncCompanyFlows(ctx, account, flows, nil); err != nil {
		t.Fatal(err)
	}
	if err := db.Prune(ctx); err != nil {
		t.Fatal(err)
	}
	history, err := db.MoneyHistory(ctx, account, time.Time{}, at.Add(24*time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	var count, net int64
	for _, period := range history.CompanyFlows {
		count += period.Count
		net += period.Pence
	}
	if count != 2 || net != -600 || !history.From.Equal(at) {
		t.Fatalf("permanent receipt history changed after replay/pruning: %+v", history)
	}
	if err := db.SyncCompanyFlows(ctx, account, nil, []string{"upkeep"}); err != nil {
		t.Fatal(err)
	}
	history, err = db.MoneyHistory(ctx, account, time.Time{}, at.Add(24*time.Hour))
	if err != nil || len(history.CompanyFlows) != 1 || history.CompanyFlows[0].Pence != 300 ||
		history.CompanyFlowsFrom == nil || !history.CompanyFlowsFrom.Equal(at.Add(time.Hour)) {
		t.Fatalf("claimed expense was not removed: %+v, %v", history, err)
	}
	other, err := db.MoneyHistory(ctx, "company-other", time.Time{}, at.Add(24*time.Hour))
	if err != nil || len(other.CompanyFlows) != 1 || other.CompanyFlows[0].Pence != -900 {
		t.Fatalf("expense removal crossed account boundary: %+v, %v", other, err)
	}
}

func TestMoneyHistoryCompanyFlowsShareUTCBucketsAndCoverage(t *testing.T) {
	db, _, _ := integrationStore(t)
	db.sqlDB.SetMaxOpenConns(1)
	db.sqlDB.SetMaxIdleConns(1)
	if err := db.db.Exec("SET TIME ZONE 'Asia/Kolkata'").Error; err != nil {
		t.Fatal(err)
	}
	defer db.db.Exec("RESET TIME ZONE")
	ctx := context.Background()
	const account = "company-buckets"
	saveTestAccount(t, db, account)
	saveTestAccount(t, db, "company-buckets-other")
	base := time.Date(2026, 10, 20, 0, 0, 0, 0, time.UTC)
	from := base.Add(20 * time.Minute)
	to := base.Add(121*24*time.Hour + 30*time.Minute)
	earliest := base.Add(-4 * 24 * time.Hour)
	flows := []CompanyLedgerFlow{
		{LedgerID: "earliest", Reason: "upkeep", AmountPence: -1, CreatedAt: earliest},
		{LedgerID: "before", Reason: "upkeep", AmountPence: -10_000, CreatedAt: from.Add(-time.Microsecond)},
		{LedgerID: "start", Reason: "upkeep", AmountPence: -100, CreatedAt: from},
		{LedgerID: "income", Reason: "bounty", AmountPence: 250, CreatedAt: base.Add(time.Hour)},
		{LedgerID: "second-day", Reason: "upkeep", AmountPence: -50, CreatedAt: base.Add(26 * time.Hour)},
		{LedgerID: "offset-credit", Reason: "bounty", AmountPence: 40, CreatedAt: base.Add(49 * time.Hour)},
		{LedgerID: "offset-debit", Reason: "upkeep", AmountPence: -40, CreatedAt: base.Add(50 * time.Hour)},
		{LedgerID: "last-day", Reason: "upkeep", AmountPence: -75, CreatedAt: to.Add(-15 * time.Minute)},
		{LedgerID: "end", Reason: "bounty", AmountPence: 25, CreatedAt: to},
		{LedgerID: "after", Reason: "upkeep", AmountPence: -10_000, CreatedAt: to.Add(time.Microsecond)},
	}
	if err := db.SyncCompanyFlows(ctx, account, flows, nil); err != nil {
		t.Fatal(err)
	}
	if err := db.SyncCompanyFlows(ctx, "company-buckets-other", []CompanyLedgerFlow{
		{LedgerID: "foreign", Reason: "upkeep", AmountPence: -99_999, CreatedAt: earliest.Add(-24 * time.Hour)},
	}, nil); err != nil {
		t.Fatal(err)
	}
	for i, at := range []time.Time{from, base.Add(26 * time.Hour), base.Add(49 * time.Hour), to} {
		row := putEconomicEvidence(t, db, account, fmt.Sprintf("company-grid-%d", i), "spot", "a|b|grain|spot", 1, 1000, 600, true, nil)
		if err := db.db.Model(&settlementRow{}).Where("id = ?", row.ID).Update("completed_at", at).Error; err != nil {
			t.Fatal(err)
		}
	}
	history, err := db.MoneyHistory(ctx, account, from, to)
	if err != nil {
		t.Fatal(err)
	}
	if history.BucketSeconds != 86400 || len(history.CompanyFlows) != 4 || len(history.Settlements) != 4 ||
		history.CompanyFlowsFrom == nil || !history.CompanyFlowsFrom.Equal(earliest) {
		t.Fatalf("company-flow range, coverage or UTC grid changed: %+v", history)
	}
	for i, want := range []struct {
		count int64
		pence int64
	}{{2, 150}, {1, -50}, {2, 0}, {2, -50}} {
		flow, settlement := history.CompanyFlows[i], history.Settlements[i]
		if flow.Count != want.count || flow.Pence != want.pence || !flow.At.Equal(settlement.At) || !flow.To.Equal(settlement.To) {
			t.Fatalf("period %d: got %+v, settlement %+v, want %+v", i, flow, settlement, want)
		}
	}
	if !history.CompanyFlows[0].At.Equal(from) || !history.CompanyFlows[3].To.Equal(to) {
		t.Fatalf("company-flow edge buckets were not clamped: %+v", history.CompanyFlows)
	}
	all, err := db.MoneyHistory(ctx, account, time.Time{}, to)
	if err != nil || !all.From.Equal(earliest) {
		t.Fatalf("all history omitted its earliest company flow: %+v, %v", all, err)
	}
	before, err := db.MoneyHistory(ctx, account, earliest.Add(-24*time.Hour), earliest.Add(-time.Hour))
	if err != nil || before.CompanyFlows == nil || len(before.CompanyFlows) != 0 ||
		before.CompanyFlowsFrom == nil || !before.CompanyFlowsFrom.Equal(earliest) {
		t.Fatalf("coverage depended on the requested range: %+v, %v", before, err)
	}
}

func TestMoneyHistoryCompanyFlowsEmptyJSON(t *testing.T) {
	db, _, _ := integrationStore(t)
	const account = "company-empty"
	saveTestAccount(t, db, account)
	now := time.Now().UTC()
	history, err := db.MoneyHistory(context.Background(), account, now.Add(-time.Hour), now)
	if err != nil {
		t.Fatal(err)
	}
	payload, err := json.Marshal(history)
	if err != nil {
		t.Fatal(err)
	}
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(payload, &fields); err != nil {
		t.Fatal(err)
	}
	if string(fields["company_flows"]) != "[]" || string(fields["company_flows_from"]) != "null" {
		t.Fatalf("missing company history must expose [] and null: %s", payload)
	}
}

func TestCompanyFlowsRejectInvalidInput(t *testing.T) {
	valid := CompanyLedgerFlow{LedgerID: "bill", Reason: "upkeep", AmountPence: -1, CreatedAt: time.Now()}
	for _, change := range []func(*CompanyLedgerFlow){
		func(flow *CompanyLedgerFlow) { flow.LedgerID = "" },
		func(flow *CompanyLedgerFlow) { flow.Reason = "" },
		func(flow *CompanyLedgerFlow) { flow.AmountPence = 0 },
		func(flow *CompanyLedgerFlow) { flow.CreatedAt = time.Time{} },
	} {
		flow := valid
		change(&flow)
		if err := (*Store)(nil).SyncCompanyFlows(context.Background(), "account", []CompanyLedgerFlow{flow}, nil); !errors.Is(err, ErrInvalidInput) {
			t.Fatalf("invalid company receipt accepted: %+v, %v", flow, err)
		}
	}
	if err := (*Store)(nil).SyncCompanyFlows(context.Background(), "", nil, nil); !errors.Is(err, ErrInvalidInput) {
		t.Fatalf("empty account accepted: %v", err)
	}
	if err := (*Store)(nil).SyncCompanyFlows(context.Background(), "account", nil, []string{""}); !errors.Is(err, ErrInvalidInput) {
		t.Fatalf("empty removal identity accepted: %v", err)
	}
}
