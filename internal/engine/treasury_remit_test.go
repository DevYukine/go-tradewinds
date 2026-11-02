package engine

import (
	"encoding/json"
	"net/http"
	"testing"
	"time"

	"github.com/DevYukine/go-tradewinds/internal/store"
	"github.com/google/uuid"
)

func TestTreasuryStagesExactSmallRemits(t *testing.T) {
	for _, shortfall := range []int64{1, 4_999, 5_000} {
		h := newRecoveryHarness(t)
		r := h.runner(primaryTestKey)
		r.stateCache.Rates = []Rate{{FromRegion: "donor", ToRegion: "destination", FeeBPS: 60}}
		balances := map[string]int64{"destination": 100_000 - shortfall, "donor": 500_000}
		required := map[string]int64{"destination": 100_000, "donor": 100_000}
		changed, err := r.fundTreasury(h.ctx, balances, required)
		if err != nil || !changed {
			t.Fatalf("shortfall=%d changed=%t error=%v", shortfall, changed, err)
		}
		_, data := h.mission("__treasury")
		var request struct {
			Gross int64 `json:"gross_amount_pence"`
		}
		want, err := GrossRemittance(shortfall, 60)
		if err != nil || data.Action == nil || json.Unmarshal(data.Action.Body, &request) != nil || request.Gross != want {
			t.Fatalf("exact shortfall was buffered or not staged: want=%d data=%+v error=%v", want, data, err)
		}
	}
}

func TestRemittanceAcceptsDustWithoutSpendingDonorReserve(t *testing.T) {
	balances := map[string]int64{"cheap": 100_001, "funded": 500_000}
	required := map[string]int64{"cheap": 100_000, "funded": 100_000, "destination": 10_000}
	rates := []Rate{
		{FromRegion: "cheap", ToRegion: "destination", FeeBPS: 1},
		{FromRegion: "funded", ToRegion: "destination", FeeBPS: 60},
	}
	rate, gross, net, ok := bestRemittance(balances, required, rates, "destination")
	if !ok || rate.FromRegion != "cheap" || gross != 1 || net != 1 {
		t.Fatalf("exact affordable dust funding rejected: rate=%+v gross=%d net=%d found=%t", rate, gross, net, ok)
	}
	balances["cheap"] -= gross
	balances["destination"] += net
	rate, gross, net, ok = bestRemittance(balances, required, rates, "destination")
	if !ok || rate.FromRegion != "funded" || net < 9_999 || net > 10_000 || balances["funded"]-gross < required["funded"] {
		t.Fatalf("follow-on exact funding spent protected cash or grew its target: rate=%+v gross=%d net=%d found=%t", rate, gross, net, ok)
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

func TestTreasuryFundsObligationsDuringConcurrentRelocations(t *testing.T) {
	h, runner := incomeFundingRunner(t)
	second := runner.stateCache.Ships[0]
	second.Ship.ID, second.Ship.Name = recoveryShipCharizard, "Charizard"
	second.Ship.CurrentPortID = recoveryFarPort
	runner.stateCache.Ships = append(runner.stateCache.Ships, second)
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
	for _, spec := range []struct{ ship, origin, destination string }{
		{recoveryShipBlastoise, recoveryHomePort, recoveryFarPort},
		{recoveryShipCharizard, recoveryFarPort, recoveryHomePort},
	} {
		h.putMission(spec.ship, "relocation-with-home-bills-"+spec.ship, "sourcing_fund", missionData{
			Plan:       Plan{Kind: "relocation", ShipID: spec.ship, Origin: spec.origin, Destination: spec.destination},
			Relocation: &relocationIntent{ExpenseCapPence: 20_000}, CostsKnown: true,
			CooldownUntil: time.Now().UTC().Add(time.Hour),
		})
	}
	if changed, err := runner.work(h.ctx); err != nil || !changed {
		t.Fatalf("relocation blocked obligation funding: changed=%t error=%v holds=%+v", changed, err, runner.holding)
	}
	_, data := h.mission("__treasury")
	var request struct {
		To    string `json:"to_region"`
		Gross int64  `json:"gross_amount_pence"`
	}
	if data.Action == nil || data.Action.Stage != "remit" ||
		json.Unmarshal(data.Action.Body, &request) != nil || request.To != "north" || request.Gross < 1 || request.Gross > 2 {
		t.Fatalf("relocation suppressed the small home obligation: %+v", data)
	}
	for _, id := range []string{recoveryShipBlastoise, recoveryShipCharizard} {
		_, relocation := h.mission(id)
		if relocation.Relocation == nil || relocation.Action != nil {
			t.Fatalf("obligation funding replaced relocating ship %s: %+v", id, relocation)
		}
	}
}
