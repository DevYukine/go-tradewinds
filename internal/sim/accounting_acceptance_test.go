package sim

import (
	"encoding/json"
	"math"
	"testing"
	"time"

	"github.com/DevYukine/go-tradewinds/internal/engine"
	"github.com/DevYukine/go-tradewinds/internal/store"
)

func TestAccountingAcceptanceMovementAndPortCosts(t *testing.T) {
	hours, err := VoyageGameHours(engine.Route{TotalDistanceNM: 240}, 600, 1)
	if err != nil || hours != 40 {
		t.Fatalf("240nm at six knots: %g hours, %v", hours, err)
	}
	if realDuration := time.Duration(hours / 60 * float64(time.Hour)); realDuration != 40*time.Minute {
		t.Fatalf("scale-60 duration = %v", realDuration)
	}
	if got := BerthDues(2, 2400); got != 600 {
		t.Fatalf("two-hour berth stay lost six-hour minimum: %d", got)
	}
	if got := BerthDues(7, 2400); got != 700 {
		t.Fatalf("longer berth stay charged incorrectly: %d", got)
	}
	for _, tc := range []struct {
		tons  int64
		hub   bool
		hours float64
	}{{30, true, 1}, {30, false, 1.5}, {0, false, 0.5}} {
		if got := PortGameHours(tc.tons, tc.hub); got != tc.hours {
			t.Fatalf("dock lock/loading tons=%d hub=%v: got %g want %g", tc.tons, tc.hub, got, tc.hours)
		}
	}
	if got := StoresBurn(10, 24, true); math.Abs(got-0.1) > 1e-12 {
		t.Fatalf("sailing stores burn = %g", got)
	}
	if got := StoresBurn(10, 24, false); math.Abs(got-0.05) > 1e-12 {
		t.Fatalf("port stores burn = %g", got)
	}
}

func TestAccountingAcceptanceReceiptsKeepFiveCashFieldsSeparate(t *testing.T) {
	var money Money
	for _, receipt := range []struct{ stage, body string }{
		{"buy", `{"filled_quantity":30,"total_gross_pence":2700,"customs_duty_pence":68}`},
		{"provision", `{"filled_quantity":2,"total_gross_pence":200,"customs_duty_pence":5}`},
		{"sell", `{"filled_quantity":30,"total_gross_pence":9000,"customs_duty_pence":0}`},
		{"deliver-charter", `{"delivered_quantity":3,"reward_paid_pence":600}`},
		{"remit", `{"gross_pence":1000,"net_pence":990,"fee_pence":10}`},
	} {
		if err := ApplyReceipt(&money, receipt.stage, json.RawMessage(receipt.body)); err != nil {
			t.Fatalf("%s receipt: %v", receipt.stage, err)
		}
	}
	want := Money{AcquisitionPence: 2700, ProceedsPence: 9600, FeesPence: 78, ProvisionsPence: 205}
	if money != want || money.Net() != 6617 {
		t.Fatalf("receipt classification or principal double charge: got %+v net %d, want %+v net 6617", money, money.Net(), want)
	}
}

func TestAccountingAcceptanceFirstSettlerClaimsBillsAndExplorationIsDisclosure(t *testing.T) {
	at := time.Date(2026, 10, 20, 0, 0, 0, 0, time.UTC)
	accounting := NewAccounting([]engine.Vault{{AccountID: "vault-a", AccountType: "vault", BankingRegion: "north", BalancePence: 100_000}})
	accounting.Start(store.Settlement{ID: "first", MissionID: "first", Kind: "relocation", StartedAt: at, CostsKnown: true, ExplorationExpensePence: new(int64(77))})
	accounting.Start(store.Settlement{ID: "second", MissionID: "second", Kind: "spot", StartedAt: at, CostsKnown: true})
	for _, bill := range []Bill{
		{ID: "berth-1", Region: "north", At: at.Add(time.Minute), Reason: "berth", AmountPence: 100},
		{ID: "upkeep-1", Region: "north", At: at.Add(2 * time.Minute), Reason: "upkeep", AmountPence: 200},
	} {
		if err := accounting.Charge(bill); err != nil {
			t.Fatal(err)
		}
	}
	if err := accounting.Receipt("first", "north", "provision", json.RawMessage(`{"filled_quantity":2,"total_gross_pence":200,"customs_duty_pence":5}`)); err != nil {
		t.Fatal(err)
	}
	first, err := accounting.Settle("first", at.Add(3*time.Minute))
	if err != nil {
		t.Fatal(err)
	}
	second, err := accounting.Settle("second", at.Add(4*time.Minute))
	if err != nil {
		t.Fatal(err)
	}
	if first.ExpensesPence != 300 || second.ExpensesPence != 0 || first.ProvisionsPence != 205 || first.ExplorationExpensePence == nil || *first.ExplorationExpensePence != 77 {
		t.Fatalf("shared bill ownership or exploration disclosure: first=%+v second=%+v", first, second)
	}
	if _, err := accounting.Reconcile(); err != nil {
		t.Fatalf("closed mission cash no longer reconciles: %v", err)
	}
}

func TestAccountingAcceptanceOpenMissionAndUnclaimedOverheadReconcile(t *testing.T) {
	at := time.Date(2026, 10, 20, 0, 0, 0, 0, time.UTC)
	accounting := NewAccounting([]engine.Vault{{AccountType: "vault", BankingRegion: "north", BalancePence: 100_000}})
	accounting.Start(store.Settlement{MissionID: "open", StartedAt: at, CostsKnown: true})
	if err := accounting.Receipt("open", "north", "buy", json.RawMessage(`{"filled_quantity":30,"total_gross_pence":2700,"customs_duty_pence":68}`)); err != nil {
		t.Fatal(err)
	}
	if err := accounting.Charge(Bill{ID: "unclaimed", Region: "north", Reason: "upkeep", At: at.Add(time.Minute), AmountPence: 1200}); err != nil {
		t.Fatal(err)
	}
	reconciliation, err := accounting.Reconcile()
	if err != nil {
		t.Fatal(err)
	}
	if reconciliation.VaultDeltaPence != -3968 || reconciliation.OpenMissionNetPence != -2768 || reconciliation.UnclaimedOverheadPence != 1200 || reconciliation.SettledNetPence != 0 {
		t.Fatalf("cash disappeared between open mission and overhead: %+v", reconciliation)
	}
	ship := simAcceptanceJSON[engine.ShipState](t, `{"ship":{"id":"cargo-ship","ship_class":"expensive-hull"},"cargo_manifest":{"cloth":30},"stores":{"provisions_tons":1000}}`)
	catalog := simAcceptanceJSON[engine.Catalog](t, `{"commodities":[{"id":"cloth","base_price_pence":100}],"ship_classes":[{"id":"expensive-hull","purchase_price_pence":99999999}]}`)
	if worth := NetWorth(accounting.Balances(), []engine.ShipState{ship}, catalog); worth != 99_032 {
		t.Fatalf("net worth must include catalog cargo, exclude identical hulls and stores: %d", worth)
	}
}
