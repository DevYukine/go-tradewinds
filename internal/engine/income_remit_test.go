package engine

import (
	"encoding/json"
	"testing"
	"time"

	"github.com/DevYukine/go-tradewinds/internal/store"
	"github.com/google/uuid"
)

func TestIncomeRemitFundsSmallExactShortfalls(t *testing.T) {
	for _, shortfall := range []int64{1, 4_999} {
		h, r, ship, plan := incomeReviewFixture(t, "freight", false, 4)
		input, err := r.committedPlanningInput(h.ctx, ship)
		if err != nil {
			t.Fatal(err)
		}
		required := cargoFundingRequired(input, priceIncomePlan(input, plan, 0))
		h.api.setVaultBalance("north", required["north"]-shortfall)
		h.api.setRates(Rate{FromRegion: "south", ToRegion: "north"})
		incomeReviewRefresh(t, h, r)
		ship, _ = r.ship(ship.Ship.ID)
		mission, data := incomeReviewStage(t, h, r, ship, plan, "remit")
		var request struct {
			Gross int64 `json:"gross_amount_pence"`
		}
		if err := json.Unmarshal(data.Action.Body, &request); err != nil || request.Gross != shortfall {
			t.Fatalf("admitted income did not fund its exact small shortfall: %+v %v", request, err)
		}
		if err := r.newActionPreflight(h.ctx, mission, &data); err != nil {
			t.Fatalf("exact small funding failed its send checks: %v", err)
		}
		if changed, err := r.dispatch(h.ctx, mission, &data); err != nil || !changed || data.Action != nil {
			t.Fatalf("exact small funding did not execute: changed=%t error=%v data=%+v", changed, err, data)
		}
	}
}

func TestIncomeRemitUsesSmallCheapestDonorThenFundsRemainder(t *testing.T) {
	h, r, ship, plan := incomeReviewFixture(t, "freight", false, 4)
	h.api.setVaultBalance("cheap", 1_000)
	h.api.setRates(
		Rate{FromRegion: "cheap", ToRegion: "north"},
		Rate{FromRegion: "south", ToRegion: "north", FeeBPS: 100},
	)
	incomeReviewRefresh(t, h, r)
	ship, _ = r.ship(ship.Ship.ID)
	mission, data := incomeReviewStage(t, h, r, ship, plan, "remit")
	for _, donor := range []string{"cheap", "south"} {
		var request struct {
			From  string `json:"from_region"`
			Gross int64  `json:"gross_amount_pence"`
		}
		if data.Action == nil || json.Unmarshal(data.Action.Body, &request) != nil || request.From != donor ||
			donor == "cheap" && request.Gross != 1_000 {
			t.Fatalf("admitted voyage skipped a usable donor: %+v", data.Action)
		}
		if changed, err := r.dispatch(h.ctx, mission, &data); err != nil || !changed || data.Action != nil {
			t.Fatalf("donor %s did not execute: changed=%t error=%v data=%+v", donor, changed, err, data)
		}
		incomeReviewRefresh(t, h, r)
		ship, _ = r.ship(ship.Ship.ID)
		ready, changed, err := r.refreshIncomeProcurement(h.ctx, ship, mission, &data)
		if err != nil || ready != (donor == "south") || changed != (donor == "cheap") {
			t.Fatalf("partial funding failed to progress after %s: ready=%t changed=%t error=%v", donor, ready, changed, err)
		}
	}
}

func TestIncomeRemitChargesActualFeeOnceAcrossReplay(t *testing.T) {
	h, r, ship, plan := incomeReviewFixture(t, "charter", false, 4)
	mission, data := incomeReviewStage(t, h, r, ship, plan, "remit")
	var request struct {
		Gross int64 `json:"gross_amount_pence"`
	}
	if err := json.Unmarshal(data.Action.Body, &request); err != nil {
		t.Fatal(err)
	}
	fee := request.Gross * 100 / 10_000
	if data.Plan.Economics.FundingFeesPence != fee {
		t.Fatalf("funding quote omitted its fee: %+v", data)
	}
	action := *data.Action
	before := encode(data)
	if err := r.newActionPreflight(h.ctx, mission, &data); err != nil {
		t.Fatalf("fresh income remit failed its first-send quote: %v", err)
	}
	if changed, err := r.dispatch(h.ctx, mission, &data); err != nil || !changed || data.Action != nil {
		t.Fatalf("first send failed: changed=%t error=%v data=%+v", changed, err, data)
	}
	if data.FeesPence != fee {
		t.Fatalf("receipt omitted the actual cash fee: %+v", data)
	}
	if err := json.Unmarshal(before, &data); err != nil {
		t.Fatal(err)
	}
	// Re-consume the completed journal as if the mission save had been lost.
	if changed, err := r.dispatch(h.ctx, mission, &data); err != nil || !changed || data.FeesPence != fee || data.Action != nil {
		t.Fatalf("completed receipt replay changed fee accounting: changed=%t error=%v data=%+v", changed, err, data)
	}
	if rows, err := h.db.SettlementHistory(h.ctx, h.account.ID, "", 10); err != nil || len(rows) != 0 {
		t.Fatalf("income receipt invented a separate treasury settlement: %+v %v", rows, err)
	}
	mission.Phase = "closing"
	data.Plan.Kind = "aborted"
	for range 2 {
		if changed, err := r.finishMission(h.ctx, ship, mission, &data); err != nil || !changed {
			t.Fatalf("paid aborted mission could not settle its journal fee: changed=%t error=%v", changed, err)
		}
	}
	rows, err := h.db.SettlementHistory(h.ctx, h.account.ID, "", 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 1 || rows[0].ActionID != action.ID || rows[0].FeesPence != fee || !rows[0].CostsKnown ||
		rows[0].AcquisitionPence != 0 || rows[0].ProceedsPence != 0 || rows[0].QuantityTons != 0 ||
		h.api.mutationCount("/v1/treasury/remit") != 1 {
		t.Fatalf("receipt replay or aborted settlement lost/doubled the actual cash fee: %+v actual=%d", rows, fee)
	}
}

func TestExactGrowthAndSourcingRemitsFundSmallShortfalls(t *testing.T) {
	for _, owner := range []string{"growth", "sourcing"} {
		t.Run(owner, func(t *testing.T) {
			h, r := incomeFundingRunner(t)
			h.api.setVaultBalance("north", 1_000_000)
			incomeReviewRefresh(t, h, r)
			ship, _ := r.ship(recoveryShipBlastoise)
			var mission *store.Mission
			var data missionData
			if owner == "growth" {
				h.putMission("__growth", uuid.NewString(), "growth_fund", missionData{Plan: Plan{Kind: "growth"}, CostsKnown: true})
				funding, _ := h.mission("__growth")
				input, err := r.committedPlanningInput(h.ctx, ShipState{})
				if err != nil {
					t.Fatal(err)
				}
				required := clonePence(input.ReserveByRegion)
				required["north"] = input.Balances["north"] + 1_000
				if changed, err := r.fundTreasuryMission(h.ctx, input.Balances, required, "__treasury", nil, funding); err != nil || !changed {
					t.Fatalf("exact growth funding did not stage: changed=%t error=%v", changed, err)
				}
				mission, data = h.mission("__treasury")
			} else {
				mission = &store.Mission{AccountID: h.account.ID, ShipID: ship.Ship.ID, Generation: uuid.NewString(), Phase: "source_inspection", Destination: recoveryHomePort}
				data = missionData{
					Plan:       Plan{Kind: "relocation", ShipID: ship.Ship.ID, Origin: recoveryFarPort, Destination: recoveryHomePort},
					Relocation: &relocationIntent{ExpenseCapPence: 20_000},
					CostsKnown: true, StartedAt: time.Now().UTC(), StartedGameHours: r.stateCache.Clock.Calendar.TotalGameHours,
				}
				if err := r.stage(h.ctx, mission, &data, ship, "sourcing-remit", "/v1/treasury/remit", map[string]any{
					"from_region": "south", "to_region": "north", "gross_amount_pence": 1_011,
				}); err != nil {
					t.Fatal(err)
				}
			}
			if data.Action == nil {
				t.Fatalf("exact %s reported staged work without a durable action: %+v", owner, data)
			}
			if err := r.newActionPreflight(h.ctx, mission, &data); err != nil {
				t.Fatalf("exact %s remittance was blocked by an operating gate: %v", owner, err)
			}
			if changed, err := r.dispatch(h.ctx, mission, &data); err != nil || !changed || data.Action != nil {
				t.Fatalf("exact %s did not cross its proven first send: changed=%t error=%v data=%+v", owner, changed, err, data)
			}
			if h.api.mutationCount("/v1/treasury/remit") != 1 {
				t.Fatalf("exact %s was stranded without a send", owner)
			}
		})
	}
}
