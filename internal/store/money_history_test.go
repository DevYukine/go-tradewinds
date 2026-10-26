package store

import (
	"context"
	"fmt"
	"testing"
	"time"
)

func TestMoneyHistoryKeepsSparseSettlementsAndBoundsEachSeries(t *testing.T) {
	db, _, _ := integrationStore(t)
	ctx := context.Background()
	const account = "money-history"
	saveTestAccount(t, db, account)
	saveTestAccount(t, db, "money-other")
	base := time.Date(2026, 10, 20, 0, 0, 0, 0, time.UTC)
	// More than the old shared history limit, all newer than the settlements.
	metrics := make([]Metric, 0, 2400)
	for i := range 800 {
		for _, name := range []string{"liquid_funds_pence", "cash_outflow_pence", "expenses_pence"} {
			metrics = append(metrics, Metric{AccountID: account, Name: name, Value: float64(i), RecordedAt: base.Add(12*time.Hour + time.Duration(i)*time.Second)})
		}
	}
	if err := db.db.CreateInBatches(&metrics, 200).Error; err != nil {
		t.Fatal(err)
	}
	for i := range 12 {
		row := putEconomicEvidence(t, db, account, fmt.Sprintf("money-%d", i), "spot", "a|b|grain|spot", 1, 1000, 600, true, nil)
		if i == 3 {
			if err := db.db.Model(&settlementRow{}).Where("id = ?", row.ID).Update("acquisition_pence", 1500).Error; err != nil {
				t.Fatal(err)
			}
		}
		if err := db.db.Model(&settlementRow{}).Where("id = ?", row.ID).Update("completed_at", base.Add(time.Duration(i)*time.Hour)).Error; err != nil {
			t.Fatal(err)
		}
	}
	unknown := putEconomicEvidence(t, db, account, "money-unknown", "recovery", "other", 1, 250, 0, false, nil)
	if err := db.db.Model(&settlementRow{}).Where("id = ?", unknown.ID).Update("completed_at", base.Add(13*time.Hour)).Error; err != nil {
		t.Fatal(err)
	}
	putEconomicEvidence(t, db, "money-other", "money-other", "spot", "other", 1, 999999, 0, true, nil)
	history, err := db.MoneyHistory(ctx, account, base, base.Add(24*time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	if len(history.Settlements) != 13 || len(history.Metrics) == 0 || len(history.Metrics) > 3*481 {
		t.Fatalf("sparse settlements lost or shared cap retained: %+v", history)
	}
	var net int64
	for _, period := range history.Settlements {
		if period.NetPence != nil {
			net += *period.NetPence
		}
	}
	if net != 11*400-500 || history.Settlements[3].NetPence == nil || *history.Settlements[3].NetPence != -500 {
		t.Fatalf("loss-inclusive net changed: %+v", history.Settlements)
	}
	last := history.Settlements[12]
	if last.GrossPence != 250 || last.KnownCount != 0 || last.NetPence != nil || last.CostsPence != nil {
		t.Fatalf("unknown costs became free profit: %+v", last)
	}
	for _, metric := range history.Metrics {
		if metric.AccountID != account {
			t.Fatal("another account entered money history")
		}
	}
	if history.Metrics[len(history.Metrics)-1].Value != 799 {
		t.Fatal("balance buckets must retain the last observation, not a sum")
	}
	custom, err := db.MoneyHistory(ctx, account, base.Add(3*time.Hour), base.Add(4*time.Hour))
	if err != nil || len(custom.Settlements) != 2 || len(custom.Metrics) != 0 {
		t.Fatalf("inclusive custom bounds ignored: %+v, %v", custom, err)
	}
	for _, period := range custom.Settlements {
		if period.At.Before(custom.From) || period.To.After(custom.To) || period.To.Before(period.At) {
			t.Fatalf("partial period exceeds requested bounds: %+v", period)
		}
	}
	all, err := db.MoneyHistory(ctx, account, time.Time{}, base.Add(24*time.Hour))
	if err != nil || !all.From.Equal(base) || len(all.Settlements) != 13 {
		t.Fatalf("all history was not the full recorded range: %+v, %v", all, err)
	}
	empty, err := db.MoneyHistory(ctx, account, base.Add(-time.Hour), base.Add(-time.Minute))
	if err != nil || len(empty.Metrics) != 0 || len(empty.Settlements) != 0 {
		t.Fatalf("missing history was fabricated: %+v, %v", empty, err)
	}
}

func TestMoneyHistoryUsesUTCBucketsAndClampsEdgePeriods(t *testing.T) {
	db, _, _ := integrationStore(t)
	db.sqlDB.SetMaxOpenConns(1)
	db.sqlDB.SetMaxIdleConns(1)
	if err := db.db.Exec("SET TIME ZONE 'Asia/Kolkata'").Error; err != nil {
		t.Fatal(err)
	}
	defer db.db.Exec("RESET TIME ZONE")
	const account = "money-non-utc"
	saveTestAccount(t, db, account)
	base := time.Date(2026, 10, 20, 0, 0, 0, 0, time.UTC)
	from := base.Add(20 * time.Minute)
	to := base.Add(121*24*time.Hour + 30*time.Minute)
	for _, hour := range []int{1, 20, 26} {
		if err := db.db.Create(&Metric{
			AccountID: account, Name: "liquid_funds_pence", Value: float64(hour), RecordedAt: base.Add(time.Duration(hour) * time.Hour),
		}).Error; err != nil {
			t.Fatal(err)
		}
	}
	for i, completedAt := range []time.Time{base.Add(time.Hour), base.Add(20 * time.Hour), base.Add(26 * time.Hour), to.Add(-15 * time.Minute)} {
		row := putEconomicEvidence(t, db, account, fmt.Sprintf("utc-%d", i), "spot", "a|b|grain|spot", 1, 1000, 600, true, nil)
		if err := db.db.Model(&settlementRow{}).Where("id = ?", row.ID).Update("completed_at", completedAt).Error; err != nil {
			t.Fatal(err)
		}
	}
	history, err := db.MoneyHistory(context.Background(), account, from, to)
	if err != nil {
		t.Fatal(err)
	}
	if history.BucketSeconds != 86400 || len(history.Metrics) != 2 || history.Metrics[0].Value != 20 || history.Metrics[1].Value != 26 {
		t.Fatalf("metric buckets depend on session timezone: %+v", history)
	}
	if len(history.Settlements) != 3 || history.Settlements[0].Count != 2 {
		t.Fatalf("settlement buckets depend on session timezone: %+v", history.Settlements)
	}
	if !history.Settlements[0].At.Equal(from) || !history.Settlements[0].To.Equal(base.Add(24*time.Hour)) ||
		!history.Settlements[1].At.Equal(base.Add(24*time.Hour)) || !history.Settlements[2].To.Equal(to) {
		t.Fatalf("UTC or edge bounds changed: %+v", history.Settlements)
	}
}

func TestMoneyHistoryRejectsInvalidBounds(t *testing.T) {
	for _, test := range []struct {
		account string
		from    time.Time
		to      time.Time
	}{
		{"", time.Time{}, time.Now()},
		{"account", time.Time{}, time.Time{}},
		{"account", time.Now(), time.Now().Add(-time.Hour)},
	} {
		if _, err := (*Store)(nil).MoneyHistory(context.Background(), test.account, test.from, test.to); err == nil {
			t.Fatal("invalid money history query accepted")
		}
	}
}
