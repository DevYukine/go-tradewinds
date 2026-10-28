package store

import (
	"context"
	"encoding/json"
	"errors"
	"math"
	"net/http"
	"testing"
	"testing/fstest"
	"time"

	"github.com/pressly/goose/v3"
)

func TestSettlementPlanRoundTripAndImmutableReplay(t *testing.T) {
	database, _, _ := integrationStore(t)
	ctx := context.Background()
	const accountID = "settlement-plan"
	saveTestAccount(t, database, accountID)
	const actionID = "settlement-plan-action"
	if _, err := database.BeginAction(ctx, &Action{ID: actionID, AccountID: accountID, Method: http.MethodPost, Path: "/v1/market/place-order", Body: json.RawMessage(`{}`)}); err != nil {
		t.Fatal(err)
	}
	if err := database.CompleteAction(ctx, actionID, http.StatusOK, json.RawMessage(`{}`), nil); err != nil {
		t.Fatal(err)
	}
	row := Settlement{
		ID: "planned-settlement", AccountID: accountID, ShipID: "ship", MissionID: "mission", ActionID: actionID,
		Kind: "spot", Lane: "origin|destination|grain|spot", QuantityTons: 22, CostsKnown: true,
		AcquisitionPence: 18_000, ProceedsPence: 20_000, GameSeconds: 86_400,
		PlannedNetPence: new(int64(-500)), PlannedRevenuePence: new(int64(24_000)),
		RevenueBasis: new("retained"), RevenueAgeGameHours: new(24.5),
		HazardBPS: new(int64(0)), HazardSource: new("lloyds_bound"),
		CargoShortfallPence: new(int64(4_800)), DepartedCargoPence: new(int64(18_000)),
		StartedAt: time.Now().UTC().Add(-time.Hour), CompletedAt: time.Now().UTC(),
	}
	if err := database.PutSettlement(ctx, &row); err != nil {
		t.Fatal(err)
	}
	rows, err := database.SettlementHistory(ctx, accountID, row.Lane, 10)
	if err != nil || len(rows) != 1 {
		t.Fatalf("planned settlement history: %+v, %v", rows, err)
	}
	stored := rows[0]
	if stored.PlannedNetPence == nil || *stored.PlannedNetPence != -500 || stored.PlannedRevenuePence == nil || *stored.PlannedRevenuePence != 24_000 ||
		stored.RevenueBasis == nil || *stored.RevenueBasis != "retained" || stored.RevenueAgeGameHours == nil || *stored.RevenueAgeGameHours != 24.5 ||
		stored.HazardBPS == nil || *stored.HazardBPS != 0 || stored.HazardSource == nil || *stored.HazardSource != "lloyds_bound" ||
		stored.CargoShortfallPence == nil || *stored.CargoShortfallPence != 4_800 || stored.DepartedCargoPence == nil || *stored.DepartedCargoPence != 18_000 {
		t.Fatalf("nullable plan values did not round-trip: %+v", stored)
	}
	// Independent decoding models a restarted process, not shared pointer identity.
	var replay Settlement
	payload, err := json.Marshal(stored)
	if err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(payload, &replay); err != nil {
		t.Fatal(err)
	}
	if err := database.PutSettlement(ctx, &replay); err != nil {
		t.Fatalf("restart changed immutable planned accounting: %v", err)
	}
	for name, change := range map[string]func(*Settlement){
		"planned net":       func(row *Settlement) { row.PlannedNetPence = new(int64(-499)) },
		"planned revenue":   func(row *Settlement) { row.PlannedRevenuePence = new(int64(24_001)) },
		"revenue basis":     func(row *Settlement) { row.RevenueBasis = new("depth") },
		"revenue age":       func(row *Settlement) { row.RevenueAgeGameHours = new(24.6) },
		"hazard bps":        func(row *Settlement) { row.HazardBPS = new(int64(1)) },
		"hazard source":     func(row *Settlement) { row.HazardSource = new("different") },
		"shortfall":         func(row *Settlement) { row.CargoShortfallPence = new(int64(4_801)) },
		"departed cargo":    func(row *Settlement) { row.DepartedCargoPence = new(int64(18_001)) },
		"erase zero hazard": func(row *Settlement) { row.HazardBPS = nil },
		"erase planned net": func(row *Settlement) { row.PlannedNetPence = nil },
	} {
		t.Run(name, func(t *testing.T) {
			changed := replay
			change(&changed)
			if err := database.PutSettlement(ctx, &changed); !errors.Is(err, ErrActionConflict) {
				t.Fatalf("changed immutable plan was accepted: %v", err)
			}
		})
	}
	for name, change := range map[string]func(*Settlement){
		"negative revenue":   func(row *Settlement) { row.PlannedRevenuePence = new(int64(-1)) },
		"negative age":       func(row *Settlement) { row.RevenueAgeGameHours = new(-0.1) },
		"nan age":            func(row *Settlement) { row.RevenueAgeGameHours = new(math.NaN()) },
		"infinite age":       func(row *Settlement) { row.RevenueAgeGameHours = new(math.Inf(1)) },
		"negative hazard":    func(row *Settlement) { row.HazardBPS = new(int64(-1)) },
		"hazard over total":  func(row *Settlement) { row.HazardBPS = new(int64(10_001)) },
		"negative shortfall": func(row *Settlement) { row.CargoShortfallPence = new(int64(-1)) },
		"negative departed":  func(row *Settlement) { row.DepartedCargoPence = new(int64(-1)) },
	} {
		t.Run(name, func(t *testing.T) {
			changed := replay
			change(&changed)
			if err := database.PutSettlement(ctx, &changed); !errors.Is(err, ErrInvalidInput) {
				t.Fatalf("invalid plan value was accepted: %v", err)
			}
		})
	}
}

func TestSettlementPlanMigrationUpDownPreservesLegacyRows(t *testing.T) {
	database, _, _ := integrationStore(t)
	ctx := context.Background()
	const accountID = "settlement-plan-migration"
	saveTestAccount(t, database, accountID)
	legacy := putEconomicEvidence(t, database, accountID, "legacy-plan", "spot", "lane", 30, 24_000, 18_000, true, nil)
	raw, err := migrations.ReadFile("migrations/00012_settlement_plan.sql")
	if err != nil {
		t.Fatal(err)
	}
	provider, err := goose.NewProvider(goose.DialectPostgres, database.sqlDB,
		fstest.MapFS{"00012_settlement_plan.sql": {Data: raw}},
		goose.WithDisableGlobalRegistry(true), goose.WithDisableVersioning(true))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := provider.Down(ctx); err != nil {
		t.Fatalf("settlement plan migration down: %v", err)
	}
	var columnCount int
	if err := database.db.Raw(`SELECT COUNT(*) FROM information_schema.columns WHERE table_schema = current_schema() AND table_name = 'settlements' AND column_name IN ('planned_net_pence','planned_revenue_pence','revenue_basis','revenue_age_game_hours','hazard_bps','hazard_source','cargo_shortfall_pence','departed_cargo_pence')`).Scan(&columnCount).Error; err != nil || columnCount != 0 {
		t.Fatalf("down migration left new columns: %d, %v", columnCount, err)
	}
	if _, err := provider.UpTo(ctx, 12); err != nil {
		t.Fatalf("settlement plan migration up: %v", err)
	}
	rows, err := database.SettlementHistory(ctx, accountID, "lane", 10)
	if err != nil || len(rows) != 1 || !sameSettlement(rows[0], legacy) {
		t.Fatalf("migration changed legacy settlement: %+v, %v", rows, err)
	}
	row := rows[0]
	if row.PlannedNetPence != nil || row.PlannedRevenuePence != nil || row.RevenueBasis != nil || row.RevenueAgeGameHours != nil || row.HazardBPS != nil || row.HazardSource != nil || row.CargoShortfallPence != nil || row.DepartedCargoPence != nil {
		t.Fatalf("legacy unknowns became measured zero: %+v", row)
	}
	if err := database.PutSettlement(ctx, &legacy); err != nil {
		t.Fatalf("legacy replay changed after migration: %v", err)
	}
}
