package store

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"net/http"
	"reflect"
	"strings"
	"testing"
	"time"
)

func putGrowthHistoryEvidence(t *testing.T, db *Store, row Settlement) {
	t.Helper()
	ctx := context.Background()
	row.ActionID = row.ID + "-action"
	row.ShipID = "growth-history-ship"
	row.MissionID = row.ID + "-mission"
	if _, err := db.BeginAction(ctx, &Action{
		ID: row.ActionID, AccountID: row.AccountID, Method: http.MethodPost,
		Path: "/v1/fleet/depart", Body: json.RawMessage(`{"ship_id":"growth-history-ship","dest_port_id":"portb"}`),
	}); err != nil {
		t.Fatalf("begin history action: %v", err)
	}
	if err := db.CompleteAction(ctx, row.ActionID, http.StatusOK, json.RawMessage(`{"ship_id":"growth-history-ship"}`), nil); err != nil {
		t.Fatalf("complete history action: %v", err)
	}
	if err := db.PutSettlement(ctx, &row); err != nil {
		t.Fatalf("persist history settlement: %v", err)
	}
}

func TestGrowthHistoryMicrosecondBoundsAndWholeCycles(t *testing.T) {
	db, _, _ := integrationStore(t)
	ctx := context.Background()
	const accountID = "growth-bounds"
	const lane = "laport|portb|grain|spot"
	saveTestAccount(t, db, accountID)
	asOf := time.Date(2026, 10, 24, 12, 30, 45, 123456000, time.UTC)
	cutoff := asOf.Add(-12 * time.Hour)
	probeExpense := int64(10)
	rows := []Settlement{
		{ID: "bounds-before", Kind: "spot", CostsKnown: true, QuantityTons: 1, GameSeconds: 1, ProceedsPence: 9_000, CompletedAt: cutoff.Add(-time.Microsecond)},
		{ID: "bounds-start", Kind: "spot", CostsKnown: true, QuantityTons: 2, GameSeconds: 86_400, ProceedsPence: 200, AcquisitionPence: 40, FeesPence: 10, ProvisionsPence: 20, ExpensesPence: 30, CompletedAt: cutoff},
		{ID: "bounds-start-after", Kind: "charter", CostsKnown: true, QuantityTons: 3, GameSeconds: 172_800, ProceedsPence: 200, CompletedAt: cutoff.Add(time.Microsecond)},
		{ID: "bounds-end-before", Kind: "aborted", CostsKnown: true, GameSeconds: 3_600, AcquisitionPence: 50, CompletedAt: asOf.Add(-time.Microsecond)},
		{ID: "bounds-end", Kind: "relocation", CostsKnown: true, GameSeconds: 7_200, ExpensesPence: 30, ExplorationExpensePence: &probeExpense, CompletedAt: asOf},
		{ID: "bounds-future", Kind: "spot", CostsKnown: true, QuantityTons: 1, GameSeconds: 999, ProceedsPence: 1_000_000, CompletedAt: asOf.Add(time.Microsecond)},
		{ID: "bounds-old-uncertainty", Kind: "aborted", CompletedAt: cutoff.Add(-time.Microsecond)},
	}
	for _, row := range rows {
		row.AccountID, row.Lane = accountID, lane
		// A cycle crossing the lower boundary contributes its entire cost,
		// quantity and game duration when its completion is in the cohort.
		row.StartedAt = cutoff.Add(-48 * time.Hour)
		putGrowthHistoryEvidence(t, db, row)
	}
	permanent, err := db.EconomicHistory(ctx, accountID, lane)
	if err != nil || permanent.NetPence != 1_009_220 || !permanent.UnknownCosts || !permanent.UnmeasuredDuration {
		t.Fatalf("permanent evidence lost old/future settlements: %+v, %v", permanent, err)
	}
	for _, scope := range []string{"", lane} {
		history, err := db.GrowthHistory(ctx, accountID, scope, asOf)
		if err != nil || history.CommercialNetPence != 250 || history.NetPence != 220 ||
			history.PositiveCycles != 2 || history.PositiveQuantityTons != 5 || history.PositiveNetPence != 300 ||
			history.GameSeconds != 270_000 || history.PositiveGameSeconds != 259_200 ||
			history.UnknownCosts || history.UnmeasuredDuration || history.UnknownProbeCosts || history.ProbeExpensePence != 10 ||
			!history.FutureCompletion || history.LastCompletion == nil || !history.LastCompletion.Equal(asOf) ||
			history.LastProbeCompletion == nil || !history.LastProbeCompletion.Equal(asOf) {
			t.Fatalf("microsecond interval or whole-cycle accounting changed for %q: %+v, %v", scope, history, err)
		}
		// The same instant in another host zone cannot alter UTC membership.
		zoned, err := db.GrowthHistory(ctx, accountID, scope, asOf.In(time.FixedZone("host", -7*60*60)))
		if err != nil || !reflect.DeepEqual(history, zoned) {
			t.Fatalf("host zone changed recorded-time membership: %+v, %v", zoned, err)
		}
	}
	after, err := db.EconomicHistory(ctx, accountID, lane)
	if err != nil || !reflect.DeepEqual(permanent, after) {
		t.Fatalf("recent aggregation mutated permanent evidence: before %+v after %+v, %v", permanent, after, err)
	}
	rowsAfter, err := db.SettlementHistory(ctx, accountID, lane, len(rows))
	if err != nil || len(rowsAfter) != len(rows) {
		t.Fatalf("scoped aggregation discarded settlement rows: %d, %v", len(rowsAfter), err)
	}
}

func TestGrowthHistoryRetainsInWindowLossesAndUncertainty(t *testing.T) {
	db, _, _ := integrationStore(t)
	ctx := context.Background()
	asOf := time.Date(2026, 10, 24, 12, 0, 0, 0, time.UTC)
	for _, test := range []struct {
		name       string
		row        Settlement
		unknown    bool
		unmeasured bool
		probe      bool
	}{
		{name: "zero_ton_loss", row: Settlement{Kind: "spot", CostsKnown: true, AcquisitionPence: 75, GameSeconds: 100}},
		{name: "aborted_loss", row: Settlement{Kind: "aborted", CostsKnown: true, AcquisitionPence: 75, GameSeconds: 100}},
		{name: "unknown_cost", row: Settlement{Kind: "aborted", GameSeconds: 100}, unknown: true},
		{name: "unknown_outcome", row: Settlement{Kind: "unknown", CostsKnown: true, GameSeconds: 100}, unknown: true},
		{name: "unmeasured_delivery", row: Settlement{Kind: "spot", CostsKnown: true, QuantityTons: 4, ProceedsPence: 75}, unmeasured: true},
		{name: "unknown_probe", row: Settlement{Kind: "relocation", GameSeconds: 100}, unknown: true, probe: true},
	} {
		t.Run(test.name, func(t *testing.T) {
			accountID := "growth-uncertain-" + test.name
			saveTestAccount(t, db, accountID)
			row := test.row
			row.ID, row.AccountID, row.Lane = accountID, accountID, "cohort-lane"
			row.StartedAt, row.CompletedAt = asOf.Add(-24*time.Hour), asOf.Add(-time.Hour)
			putGrowthHistoryEvidence(t, db, row)
			history, err := db.GrowthHistory(ctx, accountID, row.Lane, asOf)
			if err != nil || history.PositiveCycles != 0 || history.GameSeconds != row.GameSeconds ||
				history.UnknownCosts != test.unknown || history.UnmeasuredDuration != test.unmeasured ||
				history.UnknownProbeCosts != test.probe || history.FutureCompletion {
				t.Fatalf("in-cohort failed cycle or uncertainty was omitted: %+v, %v", history, err)
			}
			if row.CostsKnown && history.NetPence != row.ProceedsPence-row.AcquisitionPence {
				t.Fatalf("zero-ton/aborted cash was omitted: %+v", history)
			}
		})
	}
}

func TestGrowthHistoryFutureEvidenceVetoesBeforeUpperExclusion(t *testing.T) {
	db, _, _ := integrationStore(t)
	ctx := context.Background()
	asOf := time.Date(2026, 10, 24, 12, 0, 0, 0, time.UTC)
	for _, test := range []struct {
		name string
		row  Settlement
		veto bool
	}{
		{name: "profit", row: Settlement{Kind: "spot", CostsKnown: true, QuantityTons: 1, GameSeconds: 100, ProceedsPence: 500}, veto: true},
		{name: "loss", row: Settlement{Kind: "aborted", CostsKnown: true, GameSeconds: 100, AcquisitionPence: 500}, veto: true},
		{name: "unknown", row: Settlement{Kind: "relocation", GameSeconds: 100}, veto: true},
		{name: "known_unknown_outcome", row: Settlement{Kind: "unknown", CostsKnown: true, GameSeconds: 100}, veto: true},
		{name: "zero_duration", row: Settlement{Kind: "recovery", CostsKnown: true}, veto: true},
		{name: "unrelated_maintenance", row: Settlement{Kind: "maintenance", CostsKnown: true, AcquisitionPence: 500}},
	} {
		t.Run(test.name, func(t *testing.T) {
			accountID := "growth-future-" + test.name
			saveTestAccount(t, db, accountID)
			row := test.row
			row.ID, row.AccountID, row.Lane = accountID, accountID, "future-lane"
			row.CompletedAt = asOf.Add(time.Microsecond)
			putGrowthHistoryEvidence(t, db, row)
			history, err := db.GrowthHistory(ctx, accountID, row.Lane, asOf)
			if err != nil || history.FutureCompletion != test.veto || !reflect.DeepEqual(history.EconomicHistory, EconomicHistory{}) {
				t.Fatalf("future evidence supplied totals or escaped its veto: %+v, %v", history, err)
			}
			// Rewinding the recorded host clock leaves the future veto intact.
			rewound, err := db.GrowthHistory(ctx, accountID, row.Lane, asOf.Add(-time.Hour))
			if err != nil || rewound.FutureCompletion != test.veto {
				t.Fatalf("backward host-clock movement hid a future completion: %+v, %v", rewound, err)
			}
		})
	}
}

func TestGrowthHistoryAccountAndPrivateLaneIsolation(t *testing.T) {
	db, _, _ := integrationStore(t)
	ctx := context.Background()
	asOf := time.Date(2026, 10, 24, 12, 0, 0, 0, time.UTC)
	const accountID = "growth-isolation-one"
	const otherID = "growth-isolation-two"
	const lane = "laport|portb|grain|spot"
	const otherLane = "lane' OR true --"
	saveTestAccount(t, db, accountID)
	saveTestAccount(t, db, otherID)
	for _, row := range []Settlement{
		{ID: "isolation-profit", AccountID: accountID, Lane: lane, Kind: "spot", CostsKnown: true, QuantityTons: 1, GameSeconds: 100, ProceedsPence: 100, CompletedAt: asOf},
		{ID: "isolation-other-lane", AccountID: accountID, Lane: otherLane, Kind: "aborted", CostsKnown: true, GameSeconds: 100, AcquisitionPence: 200, CompletedAt: asOf},
		{ID: "isolation-future-lane", AccountID: accountID, Lane: otherLane, Kind: "aborted", CompletedAt: asOf.Add(time.Microsecond)},
		{ID: "isolation-other-account", AccountID: otherID, Lane: lane, Kind: "unknown", CompletedAt: asOf.Add(time.Microsecond)},
	} {
		putGrowthHistoryEvidence(t, db, row)
	}
	for _, test := range []struct {
		account string
		lane    string
		net     int64
		cycles  int64
		future  bool
	}{
		{account: accountID, lane: lane, net: 100, cycles: 1},
		{account: accountID, lane: otherLane, net: -200, future: true},
		{account: accountID, net: -100, cycles: 1, future: true},
		{account: otherID, lane: lane, future: true},
		{account: accountID + "' OR true --"},
		{account: accountID, lane: "missing' OR true --"},
	} {
		history, err := db.GrowthHistory(ctx, test.account, test.lane, asOf)
		if err != nil || history.NetPence != test.net || history.PositiveCycles != test.cycles || history.FutureCompletion != test.future {
			t.Fatalf("account/lane scope escaped %q/%q: %+v, %v", test.account, test.lane, history, err)
		}
	}
}

func TestGrowthHistoryExactPenceAndOverflow(t *testing.T) {
	db, _, _ := integrationStore(t)
	ctx := context.Background()
	asOf := time.Date(2026, 10, 24, 12, 0, 0, 0, time.UTC)
	const accountID = "growth-exact"
	const exact = int64(9_007_199_254_740_993)
	saveTestAccount(t, db, accountID)
	putGrowthHistoryEvidence(t, db, Settlement{
		ID: "growth-exact-profit", AccountID: accountID, Lane: "exact", Kind: "spot", CostsKnown: true,
		QuantityTons: 1, GameSeconds: 100, ProceedsPence: exact, CompletedAt: asOf,
	})
	history, err := db.GrowthHistory(ctx, accountID, "exact", asOf)
	if err != nil || history.NetPence != exact || history.CommercialNetPence != exact || history.PositiveNetPence != exact {
		t.Fatalf("recent proof rounded integer pence above 2^53: %+v, %v", history, err)
	}
	maxExpense := int64(math.MaxInt64)
	for _, test := range []struct {
		name string
		row  Settlement
	}{
		{name: "profit", row: Settlement{Kind: "spot", QuantityTons: 1, GameSeconds: 1, ProceedsPence: math.MaxInt64}},
		{name: "loss", row: Settlement{Kind: "aborted", GameSeconds: 1, AcquisitionPence: math.MaxInt64}},
		{name: "quantity", row: Settlement{Kind: "spot", QuantityTons: math.MaxInt64, GameSeconds: 1, ProceedsPence: 1}},
		{name: "duration", row: Settlement{Kind: "aborted", GameSeconds: math.MaxInt64}},
		{name: "positive_duration", row: Settlement{Kind: "spot", QuantityTons: 1, GameSeconds: math.MaxInt64, ProceedsPence: 1}},
		{name: "probe_expense", row: Settlement{Kind: "relocation", GameSeconds: 1, ExplorationExpensePence: &maxExpense}},
	} {
		t.Run(test.name, func(t *testing.T) {
			for i := range 2 {
				row := test.row
				row.ID = fmt.Sprintf("growth-overflow-%s-%d", test.name, i)
				row.AccountID, row.Lane = accountID, test.name
				row.CostsKnown, row.CompletedAt = true, asOf
				putGrowthHistoryEvidence(t, db, row)
			}
			if _, err := db.GrowthHistory(ctx, accountID, test.name, asOf); err == nil {
				t.Fatal("overflowing recent aggregate became valid evidence")
			}
			if _, err := db.EconomicHistory(ctx, accountID, test.name); err == nil {
				t.Fatal("overflowing permanent aggregate became valid evidence")
			}
		})
	}
	// All row arithmetic must be numeric before summing expenses, too: this
	// settlement has a valid -1p net but a nonrepresentable gross probe cost.
	putGrowthHistoryEvidence(t, db, Settlement{
		ID: "growth-row-expense-overflow", AccountID: accountID, Lane: "row-expense", Kind: "relocation", CostsKnown: true,
		GameSeconds: 1, ProceedsPence: math.MaxInt64, AcquisitionPence: math.MaxInt64, FeesPence: 1, CompletedAt: asOf,
	})
	if _, err := db.GrowthHistory(ctx, accountID, "row-expense", asOf); err == nil {
		t.Fatal("overflowing row expense wrapped into a valid growth aggregate")
	}
}

func TestGrowthHistoryRejectsInvalidScope(t *testing.T) {
	db := &Store{}
	asOf := time.Date(2026, 10, 24, 12, 0, 0, 0, time.UTC)
	for _, test := range []struct {
		account string
		lane    string
		asOf    time.Time
	}{
		{account: "", asOf: asOf},
		{account: strings.Repeat("a", 256), asOf: asOf},
		{account: "growth-valid", lane: strings.Repeat("a", 1025), asOf: asOf},
		{account: "growth-valid"},
	} {
		if _, err := db.GrowthHistory(context.Background(), test.account, test.lane, test.asOf); !errors.Is(err, ErrInvalidInput) {
			t.Fatalf("invalid growth query reached storage: %v", err)
		}
	}
}
