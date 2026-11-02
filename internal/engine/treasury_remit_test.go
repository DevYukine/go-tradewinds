package engine

import (
	"encoding/json"
	"net/http"
	"testing"
	"time"

	"github.com/DevYukine/go-tradewinds/internal/store"
	"github.com/google/uuid"
)

func TestOperatingTreasuryMinimumOnlyAppliesToDiscretionaryRemits(t *testing.T) {
	for _, obligation := range []bool{false, true} {
		for _, shortfall := range []int64{1, 4_999, 5_000} {
			h := newRecoveryHarness(t)
			r := h.runner(primaryTestKey)
			r.stateCache.Rates = []Rate{{FromRegion: "donor", ToRegion: "destination"}}
			if obligation {
				r.stateCache.HomePortID = "home"
				r.stateCache.Catalog.Ports = []Port{{ID: "home", BankingRegion: "destination"}}
			}
			balances := map[string]int64{"destination": 100_000 - shortfall, "donor": 500_000}
			required := map[string]int64{"destination": 100_000, "donor": 100_000}
			want := obligation || shortfall >= minimumOperatingRemitPence
			changed, err := r.fundOperatingTreasury(h.ctx, balances, required)
			if err != nil || changed != want {
				t.Fatalf("obligation=%t shortfall=%d changed=%t error=%v", obligation, shortfall, changed, err)
			}
			mission, err := h.db.GetMission(h.ctx, h.account.ID, "__treasury")
			if err != nil || (mission != nil) != want {
				t.Fatalf("skipped transfer left a durable empty intent: %+v %v", mission, err)
			}
			if want {
				_, data := h.mission("__treasury")
				var request struct {
					Gross int64 `json:"gross_amount_pence"`
				}
				if data.Action == nil || json.Unmarshal(data.Action.Body, &request) != nil || request.Gross != shortfall {
					t.Fatalf("exact shortfall was buffered or not staged: %+v", data)
				}
			}
		}
	}
}

func TestDiscretionaryRemitSkipsDustDonorWithoutGrowingTarget(t *testing.T) {
	balances := map[string]int64{"cheap": 100_001, "funded": 500_000}
	required := map[string]int64{"cheap": 100_000, "funded": 100_000, "destination": 10_000}
	rates := []Rate{
		{FromRegion: "cheap", ToRegion: "destination", FeeBPS: 1},
		{FromRegion: "funded", ToRegion: "destination", FeeBPS: 60},
	}
	rate, gross, net, ok := bestRemittanceMinimum(balances, required, rates, "destination", minimumOperatingRemitPence)
	if !ok || rate.FromRegion != "funded" || gross < minimumOperatingRemitPence || net < 10_000 || net > 10_001 {
		t.Fatalf("dust donor hid exact affordable funding: rate=%+v gross=%d net=%d found=%t", rate, gross, net, ok)
	}
}

func TestBankDraftReceiptRecordsOnlyActualFee(t *testing.T) {
	h := newRecoveryHarness(t)
	r := h.runner(primaryTestKey)
	mission := &store.Mission{AccountID: h.account.ID, ShipID: "__treasury", Generation: uuid.NewString(), Phase: "remit"}
	data := missionData{Plan: Plan{Kind: "treasury"}, CostsKnown: true}
	if err := r.stage(h.ctx, mission, &data, ShipState{}, "remit", "/v1/treasury/remit", map[string]any{
		"from_region": "north_sea", "to_region": "channel", "gross_amount_pence": 100_000,
	}); err != nil {
		t.Fatal(err)
	}
	response := encode(map[string]any{"gross_amount_pence": 100_000, "fee_pence": 17, "net_credited_pence": 99_983,
		"carried_by": "bank", "crown_fee_bps": 60, "crown_fee_pence": 600})
	if changed, err := r.consumeResult(h.ctx, mission, &data, http.StatusOK, response); err != nil || !changed {
		t.Fatalf("receipt changed=%t error=%v", changed, err)
	}
	_, retained := h.mission("__treasury")
	if retained.FeesPence != 17 || retained.Action != nil {
		t.Fatalf("receipt used avoided Crown fees or principal as expense: %+v", retained)
	}
}

func TestTreasuryFundsObligationsDuringRelocation(t *testing.T) {
	h, runner := incomeFundingRunner(t)
	required, err := runner.treasuryReserves(h.ctx)
	if err != nil {
		t.Fatal(err)
	}
	runner.snapshot["settings"] = encode(map[string]any{
		"auto_remit_for_bills": true, "auto_remit_min_reserve_pence": required["north"],
	})
	for index := range runner.stateCache.Vaults {
		if runner.stateCache.Vaults[index].BankingRegion == "north" {
			runner.stateCache.Vaults[index].BalancePence = required["north"] - 1
		}
	}
	h.putMission(recoveryShipBlastoise, "relocation-with-home-bills", "sourcing_fund", missionData{
		Plan:       Plan{Kind: "relocation", ShipID: recoveryShipBlastoise, Origin: recoveryHomePort, Destination: recoveryFarPort},
		Relocation: &relocationIntent{ExpenseCapPence: 20_000}, CostsKnown: true,
		CooldownUntil: time.Now().UTC().Add(time.Hour),
	})
	if changed, err := runner.work(h.ctx); err != nil || !changed {
		t.Fatalf("relocation blocked obligation funding: changed=%t error=%v holds=%+v", changed, err, runner.holding)
	}
	_, data := h.mission("__treasury")
	var request struct {
		To    string `json:"to_region"`
		Gross int64  `json:"gross_amount_pence"`
	}
	if data.Action == nil || data.Action.Stage != "remit" ||
		json.Unmarshal(data.Action.Body, &request) != nil || request.To != "north" || request.Gross < 1 || request.Gross >= minimumOperatingRemitPence {
		t.Fatalf("relocation suppressed the small home obligation: %+v", data)
	}
	_, relocation := h.mission(recoveryShipBlastoise)
	if relocation.Relocation == nil || relocation.Action != nil {
		t.Fatalf("obligation funding replaced the relocating ship's mission: %+v", relocation)
	}
}
