package engine

import (
	"context"
	"testing"
	"time"

	"github.com/DevYukine/go-tradewinds/internal/store"
)

func TestCompanyLedgerFlowClassifiesObservedReasons(t *testing.T) {
	t.Parallel()
	owned := map[string]bool{"vault": true, "other-vault": true, "escrow": true}
	at := time.Now().UTC()
	for _, test := range []struct {
		reason  string
		include bool
	}{
		{"spot_market_sell", false},
		{"royal_charter_payout", false},
		{"admiralty_charter_payout", false},
		{"freight_contract_reward_payout", false},
		{"passenger_fare_payout", false},
		{"spot_market_buy", false},
		{"port_customs_duty", false},
		{"daily_fleet_and_factor_sprawl_upkeep", true},
		{"shipyard_construction", false},
		{"shipyard_turnkey_markup", false},
		{"ach_remittance_fee", false},
		{"hire_resident_factor", true},
		{"berth_dues", true},
		{"freight_contract_default_collateral_seizure", false},
		{"insurance_premium_payout", false},
		{"ship_cannon_fitting", false},
		{"loan_interest_payment", true},
		{"daily_warehouse_rent", true},
		{"letter_of_marque_bounty", true},
		{"ach_remittance_principal", false},
		{"loan_disbursement", false},
		{"loan_principal_repayment", false},
		{"lloyds_insurance_premium_escrow", false},
		{"freight_contract_collateral_escrow", false},
		{"freight_contract_collateral_release", false},
		{"insurance_unwritten_premium_refund", false},
		{"bank_draft", false},
		{"sound_toll", true},
		{"patrol_funding", true},
		{"insurance_claim_payout", false},
		{"admiralty_commission_payout", false},
		{"commission_construction", false},
		{"ship_repair", true},
		{"futures_settlement_payout", false},
		{"underwriting_premium_income", false},
		{"provisions_purchase", false},
		{"ship_purchase", false},
		{"ship_sale", false},
		{"hull_purchase", false},
		{"hull_sale", false},
		{"seed_capital", false},
		{"initial_capital", false},
		{"new_upstream_operating_charge", true},
	} {
		t.Run(test.reason, func(t *testing.T) {
			entry := ledgerBill{ID: "entry", Debit: "vault", Credit: "external", Amount: 125, Reason: test.reason, CreatedAt: at}
			for range 2 {
				amount, include := companyLedgerFlow(entry, owned, nil)
				want := int64(0)
				if test.include {
					want = -125
					if entry.Credit == "vault" {
						want = 125
					}
				}
				if include != test.include || amount != want {
					t.Fatalf("entry %+v: got %d, %v; want %d, %v", entry, amount, include, want, test.include)
				}
				entry.Debit, entry.Credit = entry.Credit, entry.Debit
			}
		})
	}
}

func TestCompanyLedgerFlowSkipsClaimedOverheadAndBalanceMoves(t *testing.T) {
	t.Parallel()
	owned := map[string]bool{"vault": true, "other-vault": true, "escrow": true}
	for _, reason := range []string{"upkeep", "warehouse_rent", "berth_dues", "sound_toll"} {
		entry := ledgerBill{ID: "bill", Debit: "vault", Credit: "external", Amount: 100, Reason: reason, CreatedAt: time.Now()}
		if amount, include := companyLedgerFlow(entry, owned, nil); !include || amount != -100 {
			t.Fatalf("unclaimed overhead omitted: %+v, %d, %v", entry, amount, include)
		}
		if amount, include := companyLedgerFlow(entry, owned, map[string]string{"bill": "mission"}); include || amount != 0 {
			t.Fatalf("claimed overhead counted twice: %+v, %d, %v", entry, amount, include)
		}
		for _, change := range []func(*ledgerBill){
			func(entry *ledgerBill) { entry.Credit = "other-vault" },
			func(entry *ledgerBill) { entry.Credit = "escrow" },
			func(entry *ledgerBill) { entry.Debit = "foreign" },
			func(entry *ledgerBill) { entry.Amount = 0 },
			func(entry *ledgerBill) { entry.Amount = -100 },
			func(entry *ledgerBill) { entry.CreatedAt = time.Time{} },
			func(entry *ledgerBill) { entry.ID = "" },
			func(entry *ledgerBill) { entry.Metadata.FuturesID = "future" },
			func(entry *ledgerBill) { entry.Metadata.UnderwritingID = "policy" },
			func(entry *ledgerBill) { entry.Reason = "" },
		} {
			changed := entry
			change(&changed)
			if amount, include := companyLedgerFlow(changed, owned, nil); include || amount != 0 {
				t.Fatalf("non-company cash flow included: %+v, %d, %v", changed, amount, include)
			}
		}
	}
}

func TestCompanyFlowRecorderBackfillsOnceAndRemovesClaimedBillsAcrossRestart(t *testing.T) {
	h := newRecoveryHarness(t)
	now := time.Now().UTC().Truncate(time.Microsecond)
	base := now.Add(-80 * time.Hour)
	ledger := []ledgerBill{
		{ID: "fleet", Debit: "vault", Credit: "crown", Amount: 900, Reason: "upkeep", CreatedAt: base},
		{ID: "berth", Debit: "vault", Credit: "port", Amount: 100, Reason: "berth_dues", CreatedAt: now.Add(-time.Hour)},
		{ID: "bounty", Debit: "crown", Credit: "vault", Amount: 300, Reason: "patrol_bounty", CreatedAt: now.Add(-time.Hour)},
	}
	ledger[1].Metadata.ShipID = recoveryShipBlastoise
	runner := New(nil, h.db, h.account, nil, Options{})
	runner.snapshot["vaults"] = encode(map[string]any{"vaults": []Vault{{AccountID: "vault", AccountType: "vault"}}})
	runner.snapshot["ledger"] = encode(ledger)
	runner.expenseOwners = make(map[string]string)
	prepare := func(r *Runner) {
		t.Helper()
		r.stateCache = r.state()
		for _, key := range []string{"vaults", "ledger"} {
			r.meta[key] = DatasetMeta{Complete: true, UpdatedAt: time.Now().UTC()}
		}
	}
	read := func(wantCount, wantNet int64) store.MoneyHistory {
		t.Helper()
		history, err := h.db.MoneyHistory(h.ctx, h.account.ID, time.Time{}, now.Add(time.Minute))
		if err != nil {
			t.Fatal(err)
		}
		var count, net int64
		for _, period := range history.CompanyFlows {
			count += period.Count
			net += period.Pence
		}
		if count != wantCount || net != wantNet || history.CompanyFlowsFrom == nil || !history.CompanyFlowsFrom.Equal(base) {
			t.Fatalf("company history got count=%d net=%d: %+v; want count=%d net=%d", count, net, history, wantCount, wantNet)
		}
		return history
	}
	prepare(runner)
	if err := runner.recordBalances(h.ctx); err != nil {
		t.Fatal(err)
	}
	read(3, -700)
	ledger = append(ledger, ledgerBill{ID: "interest", Debit: "vault", Credit: "lender", Amount: 50, Reason: "loan_interest_payment", CreatedAt: now.Add(-time.Minute)})
	runner.snapshot["ledger"] = encode(ledger)
	for _, key := range []string{"ledger", "vaults"} {
		meta := runner.meta[key]
		meta.Stale = true
		runner.meta[key] = meta
		if err := runner.recordBalances(h.ctx); err != nil {
			t.Fatal(err)
		}
		read(3, -700)
		prepare(runner)
	}
	if err := runner.recordBalances(h.ctx); err != nil {
		t.Fatal(err)
	}
	if err := h.db.SaveSnapshot(h.ctx, h.account.ID, runner.snapshot); err != nil {
		t.Fatal(err)
	}
	for range 2 {
		if err := runner.recordBalances(h.ctx); err != nil {
			t.Fatal(err)
		}
		read(4, -750)
	}
	restart := func() *Runner {
		t.Helper()
		r := New(nil, h.db, h.account, nil, Options{})
		t.Cleanup(func() { r.stopReplay(context.Background()) })
		var err error
		r.snapshot, err = h.db.Snapshot(h.ctx, h.account.ID)
		if err != nil {
			t.Fatal(err)
		}
		r.expenseOwners = decode[map[string]string](r.snapshot, "_expense_allocations")
		if r.expenseOwners == nil {
			r.expenseOwners = make(map[string]string)
		}
		prepare(r)
		return r
	}
	runner = restart()
	if err := runner.recordBalances(h.ctx); err != nil {
		t.Fatal(err)
	}
	read(4, -750)
	// Expiry from the game window cannot erase a previously recorded cost.
	ledger = ledger[1:]
	ledger = append(ledger, ledgerBill{ID: "rent", Debit: "vault", Credit: "warehouse", Amount: 25, Reason: "daily_warehouse_rent", CreatedAt: now})
	runner.snapshot["ledger"] = encode(ledger)
	if err := runner.recordBalances(h.ctx); err != nil {
		t.Fatal(err)
	}
	read(5, -775)
	if err := h.db.SaveSnapshot(h.ctx, h.account.ID, runner.snapshot); err != nil {
		t.Fatal(err)
	}
	data := missionData{Plan: Plan{ShipID: recoveryShipBlastoise}}
	amount, ids := runner.claimBillsSince(&data, "company-claim-mission")
	if amount != 100 || len(ids) != 1 || ids[0] != "berth" {
		t.Fatalf("mission did not claim the recorded bill: %d, %v", amount, ids)
	}
	data.SettlementExpenseIDs = ids
	mission := store.Mission{AccountID: h.account.ID, ShipID: recoveryShipBlastoise, Generation: "company-claim-mission", Phase: "settling"}
	if err := runner.saveMission(h.ctx, &mission, &data); err != nil {
		t.Fatal(err)
	}
	// Deletion still follows the claim if the bill has left the ledger window.
	runner.snapshot["ledger"] = encode(ledger[1:])
	if err := runner.recordBalances(h.ctx); err != nil {
		t.Fatal(err)
	}
	read(4, -675)
	persistGrowthWindowSettlement(t, h, store.Settlement{
		ID: "company-claim", Kind: "spot", CostsKnown: true,
		ProceedsPence: 1000, ExpensesPence: amount, CompletedAt: now,
	})
	// The durable mission is newer than the saved allocation snapshot, and
	// that old snapshot still contains the bill. Backfill must not re-add it.
	runner = restart()
	if err := runner.recordBalances(h.ctx); err != nil {
		t.Fatal(err)
	}
	history := read(4, -675)
	net := int64(-675)
	for _, period := range history.Settlements {
		if period.NetPence != nil {
			net += *period.NetPence
		}
	}
	if net != 225 {
		t.Fatalf("real net double counted the claimed bill: got %d, want 225", net)
	}
}
