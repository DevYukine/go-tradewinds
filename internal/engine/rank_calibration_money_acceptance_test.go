package engine

import (
	"encoding/json"
	"fmt"
	"math"
	"testing"
	"time"
)

func moneyCalibrationBPS(t *testing.T, economics Economics) int64 {
	t.Helper()
	var saved struct {
		RevenueCalibrationBPS *int64 `json:"revenue_calibration_bps"`
	}
	if err := json.Unmarshal(encode(economics), &saved); err != nil {
		t.Fatal(err)
	}
	if saved.RevenueCalibrationBPS == nil {
		t.Fatal("Economics must record revenue_calibration_bps, including the uncalibrated 10000-bps default")
	}
	return *saved.RevenueCalibrationBPS
}

func TestMoneyCalibrationAppliesToRevenueNotCostsAndRecordsBasis(t *testing.T) {
	for _, bps := range []int64{9000, 9500, 10000} {
		t.Run(fmt.Sprint(bps), func(t *testing.T) {
			in := moneyInput(t, moneyTime("2026-10-28T11:00:00Z"), "canvas_sails", 6000, 10_000, 1, 30)
			in = moneySnapshotInput(in, map[string]json.RawMessage{
				"world_events":         json.RawMessage(`[]`),
				"_revenue_calibration": encode(map[string]int64{"depth": bps, "retained": 9000, "contract": 10000}),
			})
			plan := moneySpot(t, in, "canvas_sails")
			if !plan.Accepted || plan.Economics.RevenuePence != bps || plan.Economics.PurchasePence != 6000 || plan.Economics.CustomsPence != 1 || plan.Economics.ExpectedNetPence != bps-6001 {
				t.Fatalf("basis calibration must multiply gross receipts once, never purchase costs or net: %+v", plan)
			}
			if got := moneyCalibrationBPS(t, plan.Economics); got != bps {
				t.Fatalf("recorded calibration = %d, want %d", got, bps)
			}
		})
	}
}

func TestMoneyCalibrationDefaultsToTodayAndDoesNotCrossRevenueBases(t *testing.T) {
	in := moneyInput(t, moneyTime("2026-10-28T11:00:00Z"), "canvas_sails", 6000, 10_000, 1, 30)
	plan := moneySpot(t, moneySnapshotInput(in, map[string]json.RawMessage{"world_events": json.RawMessage(`[]`)}), "canvas_sails")
	if !plan.Accepted || plan.Economics.RevenuePence != 10_000 || plan.Economics.ExpectedNetPence != 3999 || moneyCalibrationBPS(t, plan.Economics) != 10000 {
		t.Fatalf("today's uncalibrated non-event data must remain at 10000 bps: %+v", plan)
	}
	in.Contracts = []Contract{sourcingFixture[Contract](t, `{"id":"money-calibration-charter","contract_type":"charter","origin_port_id":"hamburg","commodity_id":"canvas_sails","remaining_quantity":1,"quantity_required":1,"reward_per_ton_pence":10000,"status":"open","is_public":true}`)}
	in = moneySnapshotInput(in, map[string]json.RawMessage{
		"world_events":         json.RawMessage(`[]`),
		"_revenue_calibration": encode(map[string]int64{"depth": 9000, "retained": 9100, "contract": 10000}),
	})
	for _, candidate := range PlanVoyages(in) {
		if candidate.Kind == "charter" {
			if !candidate.Accepted || candidate.Economics.RevenuePence != 10_000 || moneyCalibrationBPS(t, candidate.Economics) != 10000 {
				t.Fatalf("depth underperformance leaked into fixed contract terms: %+v", candidate)
			}
			return
		}
	}
	t.Fatal("missing calibration control charter")
}

// The fixture's costs are exact pence and its route lengths make the delivery
// durations exact: 823/720 + 77/720 = 1.25 gd; 1003/720 + 77/720 = 1.5 gd;
// onward 787/720 + 77/720 = 1.2 gd. Empty return: 1092/720 + 1/12 = 1.6 gd.
func moneyRankInput(t *testing.T, now time.Time) PlanningInput {
	t.Helper()
	in := moneyInput(t, now, "canvas_sails", 100, 441, 1, 823)
	in.Catalog.ShipClasses[0].BaseSpeedCentiknots = 3750
	in.Catalog.Ports = append(in.Catalog.Ports, Port{ID: "portsmouth", BankingRegion: "channel", Phase: 1, Imports: []string{"canvas_sails"}})
	origin := in.Ports["amsterdam"]
	origin.BerthDuesPerDayPence = 160
	in.Ports["amsterdam"] = origin
	in.Ports["portsmouth"] = PortDetail{ID: "portsmouth", EffectiveBerths: 4, Visibility: "gossip"}
	for _, route := range []Route{
		sourcingRoute("hamburg", "amsterdam", 787),
		sourcingRoute("amsterdam", "portsmouth", 1003),
		sourcingRoute("portsmouth", "amsterdam", 1092),
	} {
		key := routeKey(route.FromPort, route.ToPort, "hoy")
		in.Routes[key] = route
		in.Metadata["routes/"+key] = sourcingFreshMeta(now)
	}
	in.Metadata["ports/portsmouth"] = sourcingFreshMeta(now)
	presence := in.Ship
	presence.Ship.ID, presence.Ship.CurrentPortID = "money-presence-hull", "hamburg"
	in.Fleet = []ShipState{in.Ship, presence}
	in.Contracts = []Contract{sourcingFixture[Contract](t, `{"id":"money-onward-freight","contract_type":"freight","issuer_company_id":"foreign-carrier","origin_port_id":"hamburg","dest_port_id":"amsterdam","commodity_id":"grain","quantity_required":1,"remaining_quantity":1,"reward_pence":540,"collateral_pence":0,"status":"open","is_public":true}`)}
	in.HazardQuotes = nil
	seedZeroHazardQuotes(&in)
	return moneySnapshotInput(in, map[string]json.RawMessage{
		"world_events": json.RawMessage(`[]`),
		"_quay_book_memory": encode(map[string]RetainedQuayBook{"portsmouth": {
			ObservedAt: now, Bids: map[string][]RetainedBid{"canvas_sails": {{PricePence: 591, RemainingQuantity: 100}}},
		}}),
	})
}

func moneyRankAlternatives(t *testing.T, in PlanningInput) (Plan, Plan) {
	t.Helper()
	var presence, dark Plan
	for _, plan := range PlanVoyages(in) {
		if plan.Kind != "spot" || plan.Commodity != "canvas_sails" {
			continue
		}
		if plan.Destination == "hamburg" {
			presence = plan
		}
		if plan.Destination == "portsmouth" {
			dark = plan
		}
	}
	if !presence.Accepted || !dark.Accepted || presence.Economics.ExpectedNetPence != 300 || dark.Economics.ExpectedNetPence != 450 ||
		math.Abs(presence.Economics.GameDays-1.25) > 1e-12 || math.Abs(dark.Economics.GameDays-1.5) > 1e-12 {
		t.Fatalf("two-leg fixture lost its single-leg admissible economics: presence=%+v dark=%+v", presence, dark)
	}
	return presence, dark
}

func moneyRankDetails(t *testing.T, payload json.RawMessage, wantScore float64) {
	t.Helper()
	var details struct {
		Economics struct {
			RankScore *float64 `json:"rank_score_pence_per_game_day"`
		} `json:"economics"`
		Continuation *struct {
			Destination string  `json:"destination"`
			NetPence    int64   `json:"net_pence"`
			GameDays    float64 `json:"game_days"`
		} `json:"continuation"`
	}
	if err := json.Unmarshal(payload, &details); err != nil {
		t.Fatal(err)
	}
	if details.Economics.RankScore == nil || math.Abs(*details.Economics.RankScore-wantScore) > 1e-8 ||
		details.Continuation == nil || details.Continuation.Destination != "amsterdam" || details.Continuation.NetPence != 500 || math.Abs(details.Continuation.GameDays-1.2) > 1e-12 {
		t.Fatalf("plan/decision must explain combined rank, onward destination, net and days: %s", payload)
	}
}

func TestMoneyRankPresenceContinuationOutranksHigherSingleLegDarkPort(t *testing.T) {
	in := moneyRankInput(t, moneyTime("2026-10-28T11:00:00Z"))
	presence, dark := moneyRankAlternatives(t, in)
	plans := []Plan{dark, presence}
	rankPlans(plans, in)
	if plans[0].Destination != "hamburg" {
		t.Fatalf("300p/1.25gd + 500p/1.2gd must beat 450p/1.5gd then 40p/1.6gd ballast: %+v", plans)
	}
	moneyRankDetails(t, encode(plans[0]), float64(800)/2.45)
	var darkDetails struct {
		Economics struct {
			RankScore *float64 `json:"rank_score_pence_per_game_day"`
		} `json:"economics"`
	}
	if err := json.Unmarshal(encode(plans[1]), &darkDetails); err != nil {
		t.Fatal(err)
	}
	if darkDetails.Economics.RankScore == nil || math.Abs(*darkDetails.Economics.RankScore-float64(410)/3.1) > 1e-8 {
		t.Fatalf("dark port omitted the mirrored 1.6gd, 40p ballast cost: %+v", plans[1])
	}
	if plans[0].Economics.ExpectedNetPence != 300 || plans[0].Economics.GameDays != presence.Economics.GameDays {
		t.Fatal("ranking rewrote the single-leg admission economics")
	}
}

func TestMoneyRankContinuationNeverAdmitsRejectedOutwardLeg(t *testing.T) {
	in := moneyRankInput(t, moneyTime("2026-10-28T11:00:00Z"))
	bookCommodity(in.Markets["amsterdam"], "canvas_sails").Depth.Asks[0].PricePence = 401
	var found bool
	for _, plan := range PlanVoyages(in) {
		if plan.Kind == "spot" && plan.Destination == "hamburg" && plan.Commodity == "canvas_sails" {
			found = true
			if plan.Accepted || plan.Economics.ExpectedNetPence != -1 {
				t.Fatalf("profitable onward freight rescued a one-penny-losing first leg: %+v", plan)
			}
		}
	}
	if !found {
		t.Fatal("rejected outward leg disappeared rather than retaining its admission verdict")
	}
}

func TestMoneyRankCapacityEarningSelectionUsesContinuationNotEmptyReturn(t *testing.T) {
	in := moneyRankInput(t, moneyTime("2026-10-28T11:00:00Z"))
	presence, dark := moneyRankAlternatives(t, in)
	if !betterCapacityEarningPlan(presence, dark) || betterCapacityEarningPlan(dark, presence) {
		t.Fatalf("capacity earning-plan selection still values the onward-capable port as an empty return: presence=%+v dark=%+v", presence, dark)
	}
}

func TestMoneyRankDecisionsPersistContinuationExplanation(t *testing.T) {
	h := newRecoveryHarness(t)
	runner := h.runner(primaryTestKey)
	in := moneyRankInput(t, time.Now().UTC())
	runner.opts.Execute = false
	runner.generation, runner.meta = in.Generation, in.Metadata
	for _, dataset := range []string{"factors", "warehouses", "orders", "fills", "ledger", "rates", "settings"} {
		runner.meta[dataset] = sourcingFreshMeta(in.Now)
	}
	runner.snapshot["zones"], runner.snapshot["world_events"] = encode(in.Zones), json.RawMessage(`[]`)
	runner.snapshot["_quay_book_memory"] = encode(in.QuayBookMemory)
	runner.stateCache = AccountState{
		Catalog: in.Catalog, Clock: in.Clock, Ships: in.Fleet, Markets: in.Markets, Ports: in.Ports,
		Routes: in.Routes, Contracts: in.Contracts, HazardQuotes: in.HazardQuotes, HomePortID: "amsterdam",
		Factors: []string{"hamburg"}, Vaults: []Vault{{BankingRegion: "channel", AccountType: "vault", BalancePence: 100_000_000}},
	}
	if _, err := runner.work(h.ctx); err != nil {
		t.Fatal(err)
	}
	rows, err := h.db.History(h.ctx, h.account.ID, "decisions", 100)
	if err != nil {
		t.Fatal(err)
	}
	var decisions []struct {
		Kind    string          `json:"kind"`
		Details json.RawMessage `json:"details"`
	}
	if err := json.Unmarshal(encode(rows), &decisions); err != nil {
		t.Fatal(err)
	}
	for _, decision := range decisions {
		var plan Plan
		if json.Unmarshal(decision.Details, &plan) == nil && plan.Kind == "spot" && plan.Destination == "hamburg" && plan.Commodity == "canvas_sails" {
			moneyRankDetails(t, decision.Details, float64(800)/2.45)
			return
		}
	}
	t.Fatalf("ordinary runner planning did not persist the presence-port candidate decision: %s", encode(rows))
}
