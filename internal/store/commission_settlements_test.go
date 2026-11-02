package store

import (
	"context"
	"strings"
	"testing"
	"testing/fstest"
	"time"

	"github.com/pressly/goose/v3"
)

func TestCommissionProfitAndUnknownCostsStayOutsideFleetEvidence(t *testing.T) {
	database, _, _ := integrationStore(t)
	const accountID = "commission-economics"
	saveTestAccount(t, database, accountID)
	ctx := context.Background()
	putEconomicEvidence(t, database, accountID, "trading-loss", "spot", "trade", 10, 900, 1000, true, nil)
	profit := putEconomicEvidence(t, database, accountID, "commission-profit", "commission", "commission|boston|frigate", 0, 26_000_000, 20_570_000, true, nil)
	putEconomicEvidence(t, database, accountID, "commission-without-receipt", "commission", "commission|boston|frigate", 0, 26_000_000, 0, false, nil)
	for _, windowed := range []bool{false, true} {
		var history EconomicHistory
		var err error
		if windowed {
			var growth GrowthHistory
			growth, err = database.GrowthHistory(ctx, accountID, "", time.Now().UTC())
			history = growth.EconomicHistory
		} else {
			history, err = database.EconomicHistory(ctx, accountID, "")
		}
		if err != nil || history.NetPence != -100 || history.CommercialNetPence != -100 || history.UnknownCosts ||
			history.GameSeconds != 86_400 || history.PositiveCycles != 0 {
			t.Fatalf("commission changed fleet trading evidence (windowed=%v): %+v %v", windowed, history, err)
		}
	}
	rows, err := database.SettlementsByID(ctx, accountID, []string{profit.ID})
	if err != nil || len(rows) != 1 || rows[0].Kind != "commission" || rows[0].ProceedsPence-rows[0].AcquisitionPence != 5_430_000 {
		t.Fatalf("separate commission P&L disappeared: %+v %v", rows, err)
	}
}

func TestCommissionMigrationDownRefusesClassifiedHistory(t *testing.T) {
	database, _, _ := integrationStore(t)
	ctx := context.Background()
	raw, err := migrations.ReadFile("migrations/00024_commissions.sql")
	if err != nil {
		t.Fatal(err)
	}
	provider, err := goose.NewProvider(goose.DialectPostgres, database.sqlDB,
		fstest.MapFS{"00024_commissions.sql": {Data: raw}},
		goose.WithDisableGlobalRegistry(true), goose.WithDisableVersioning(true))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := provider.Down(ctx); err != nil {
		t.Fatalf("empty commission rollback failed: %v", err)
	}
	if _, err := provider.UpTo(ctx, 24); err != nil {
		t.Fatalf("commission migration could not be reapplied: %v", err)
	}
	const accountID = "commission-migration"
	saveTestAccount(t, database, accountID)
	row := putEconomicEvidence(t, database, accountID, "classified-commission", "commission", "commission|boston|frigate", 0, 26_000_000, 20_570_000, true, nil)
	if _, err := provider.Down(ctx); err == nil || !strings.Contains(err.Error(), "pre-migration backup") {
		t.Fatalf("rollback silently destroyed commission classification: %v", err)
	}
	rows, err := database.SettlementsByID(ctx, accountID, []string{row.ID})
	if err != nil || len(rows) != 1 || rows[0].Kind != "commission" {
		t.Fatalf("failed rollback rewrote commission history: %+v %v", rows, err)
	}
}
