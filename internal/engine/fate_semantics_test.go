package engine

import (
	"fmt"
	"net/http"
	"testing"
	"time"

	"github.com/DevYukine/go-tradewinds/internal/store"
	"go.uber.org/zap"
	"go.uber.org/zap/zaptest/observer"
)

var shipFateCases = []struct {
	fate       string
	voyageLoss bool
}{
	{"", true},
	{"lost_at_sea", true},
	{"captured", true},
	{"seized_smuggling", false},
	{"seized_bailiff", false},
	{"seized_bankruptcy", false},
	{"broken_up", false},
	{"royal_navy", false},
}

func TestCapacityGrowthFateLossAccounting(t *testing.T) {
	t.Parallel()
	for _, test := range shipFateCases {
		t.Run("fate="+test.fate, func(t *testing.T) {
			h, runner := capacityExecutionFixture(t, false)
			asOf := time.Now().UTC()
			lostAt := asOf.Add(-time.Hour)
			ship := operationShip(recoveryShipCharizard, "Charizard", "sunk", "")
			ship.Ship.Fate, ship.Ship.UpdatedAt = test.fate, &lostAt
			runner.stateCache.Ships = append(runner.stateCache.Ships, ship)
			h.putMission(ship.Ship.ID, "fate-loss-mission", "sailing", missionData{Plan: Plan{ShipID: ship.Ship.ID}})

			wantReason := ""
			if test.voyageLoss {
				wantReason = capacityShipLossReason
			}
			if reason, err := runner.capacityLosses(h.ctx, asOf); err != nil || reason != wantReason {
				t.Fatalf("fate loss gate = %q, %v; want %q", reason, err, wantReason)
			}
			losses := decode[map[string]time.Time](runner.snapshot, "_ship_losses")
			if _, exists := losses[ship.Ship.ID]; exists != test.voyageLoss {
				t.Fatalf("fate recorded as voyage loss = %t; want %t: %+v", exists, test.voyageLoss, losses)
			}
			if test.voyageLoss {
				return
			}

			// A later 0.7.3 observation must correct a pre-fate sinking inference.
			runner.snapshot["_ship_losses"] = encode(map[string]time.Time{ship.Ship.ID: lostAt})
			runner.stateCache.Ships[len(runner.stateCache.Ships)-1].Ship.UpdatedAt = nil
			if reason, err := runner.capacityLosses(h.ctx, asOf); err != nil || reason != "" {
				t.Fatalf("non-voyage exit retained an old or undated loss hold: %q, %v", reason, err)
			}
			snapshot, err := h.db.Snapshot(h.ctx, h.account.ID)
			if err != nil {
				t.Fatal(err)
			}
			if _, exists := decode[map[string]time.Time](snapshot, "_ship_losses")[ship.Ship.ID]; exists {
				t.Fatal("non-voyage exit correction was not persisted")
			}
		})
	}
}

func TestMissionFateSettlementPreservesCashWithoutInventingVoyageLoss(t *testing.T) {
	t.Parallel()
	for _, test := range shipFateCases {
		t.Run("fate="+test.fate, func(t *testing.T) {
			h := newRecoveryHarness(t)
			runner := growthWindowRunner(t, h)
			ship := operationShip(recoveryShipBlastoise, "Blastoise", "sunk", "")
			ship.Ship.Fate = test.fate
			destinationSeedAction(t, h, "fate-sale", "/v1/market/place-order",
				fmt.Sprintf(`{"ship_id":%q,"commodity_id":"grain","side":"ask","quantity":2,"price_pence":125}`, ship.Ship.ID),
				`{"commodity_id":"grain","filled_quantity":2,"total_gross_pence":250}`)
			data := missionData{
				Plan:       Plan{Kind: "spot", ShipID: ship.Ship.ID, Origin: recoveryHomePort, Destination: recoveryFarPort, Commodity: "grain"},
				ProvenLane: "fate-lane", StartedAt: time.Now().UTC().Add(-time.Hour),
				StartedGameHours: runner.stateCache.Clock.Calendar.TotalGameHours - 1,
				InitialLedgerIDs: runner.ledgerIDs(), LastActionID: "fate-sale", CostsKnown: true,
				BoughtQuantity: 10, DeliveredQuantity: 2, AcquisitionPence: 1_000, ProceedsPence: 250,
				FeesPence: 7, ProvisionsPence: 11, ExpensesPence: 13, FinishAtDestination: true,
			}
			h.putMission(ship.Ship.ID, "fate-mission", "sailing", data)
			for range 3 {
				mission, current := h.mission(ship.Ship.ID)
				if changed, err := runner.driveMission(h.ctx, ship, mission, &current); err != nil || !changed {
					t.Fatalf("finish exited hull mission: changed %t, error %v", changed, err)
				}
			}
			h.missingMission(ship.Ship.ID)
			rows := destinationSettlements(t, h, "fate-lane")
			if len(rows) != 1 {
				t.Fatalf("terminal exit must settle exactly once: %+v", rows)
			}
			row := rows[0]
			if row.AcquisitionPence != 1_000 || row.ProceedsPence != 250 || row.FeesPence != 7 ||
				row.ProvisionsPence != 11 || row.ExpensesPence != 13 || row.CostsKnown {
				t.Fatalf("fate changed actual cash or fabricated known terminal accounting: %+v", row)
			}
			if test.voyageLoss {
				if row.CargoShortfallPence == nil || *row.CargoShortfallPence != 800 {
					t.Fatalf("sea, capture or legacy exit lost its measured cargo shortfall: %+v", row)
				}
			} else if row.CargoShortfallPence != nil {
				t.Fatalf("non-voyage exit fabricated voyage-loss evidence: %+v", row)
			}
			if len(h.api.mutationsFor("/v1/fleet/depart")) != 0 {
				t.Fatal("terminal hull started another voyage")
			}
		})
	}
}

func TestSunkFatesDoNotContinueSettledMissions(t *testing.T) {
	t.Parallel()
	for _, test := range shipFateCases {
		t.Run("fate="+test.fate, func(t *testing.T) {
			h := newRecoveryHarness(t)
			runner := h.runner(primaryTestKey)
			ship := operationShip(recoveryShipBlastoise, "Blastoise", "sunk", "")
			ship.Ship.Fate = test.fate
			for _, kind := range []string{"relocation", "spot"} {
				data := missionData{
					Plan: Plan{Kind: kind, ShipID: ship.Ship.ID}, Settlement: &store.Settlement{},
					SourcingHandoff: &relocationIntent{}, ReturnJourney: true,
					GrowthLane: &growthLaneIntent{NextPlan: &Plan{Kind: "spot", ShipID: ship.Ship.ID}},
				}
				h.putMission(ship.Ship.ID, "settled-fate-mission", "settled", data)
				mission, current := h.mission(ship.Ship.ID)
				if changed, err := runner.finishMission(h.ctx, ship, mission, &current); err != nil || !changed {
					t.Fatalf("retire %s continuation: changed %t, error %v", kind, changed, err)
				}
				h.missingMission(ship.Ship.ID)
			}
		})
	}
}

func TestShipFateTransitionUsesActualExitReason(t *testing.T) {
	t.Parallel()
	for _, test := range shipFateCases {
		t.Run("fate="+test.fate, func(t *testing.T) {
			h := newRecoveryHarness(t)
			runner := h.runner(primaryTestKey)
			core, logs := observer.New(zap.DebugLevel)
			runner.log = zap.New(core)
			runner.generation = "fate-transition"
			runner.stateCache.Ships = []ShipState{operationShip(recoveryShipBlastoise, "Blastoise", "sailing", recoveryHomePort)}
			at := time.Now().UTC()
			operationObservedAt(runner, at)
			runner.logStatus(h.ctx, at)
			logs.TakeAll()
			runner.stateCache.Ships[0].Ship.Status = "sunk"
			runner.stateCache.Ships[0].Ship.Fate = test.fate
			operationObservedAt(runner, at.Add(time.Second))
			runner.logStatus(h.ctx, at.Add(time.Second))
			runner.logStatus(h.ctx, at.Add(2*time.Second))
			entries := logs.All()
			want := "ship removed from fleet"
			switch {
			case test.fate == "captured":
				want = "ship captured"
			case test.voyageLoss:
				want = "ship sunk"
			case test.fate == "seized_bailiff" || test.fate == "seized_bankruptcy":
				want = "ship seized because upkeep went unpaid"
			}
			if len(entries) != 1 || entries[0].Message != want || entries[0].Level != zap.WarnLevel {
				t.Fatalf("exit transition = %+v; want one warning %q", entries, want)
			}
			fields := entries[0].ContextMap()
			if fields["ship"] != "Blastoise" || fields["ship_id"] != recoveryShipBlastoise || fields["fate"] != test.fate {
				t.Fatalf("exit transition lost identity or fate: %+v", fields)
			}
		})
	}
}

func TestHazardCalibrationExcludesEverySunkFate(t *testing.T) {
	for _, test := range shipFateCases {
		t.Run("fate="+test.fate, func(t *testing.T) {
			input, _, hull := overseasHazardFixture(10)
			if calibrateZoneHazard(input, hull, 4) == nil {
				t.Fatal("live owner did not provide calibration evidence")
			}
			input.Fleet[0].Ship.Status, input.Fleet[0].Ship.Fate = "sunk", test.fate
			if calibrateZoneHazard(input, hull, 4) != nil {
				t.Fatal("an exited hull remained an active quote owner")
			}
		})
	}
}

func TestUnderwritingFateDoesNotReplacePolicyPayoutFacts(t *testing.T) {
	t.Parallel()
	for _, test := range []struct {
		fate   string
		status string
		claim  int64
	}{
		{"captured", "active", 0},
		{"seized_bailiff", "claimed_loss", uwExecCollateralCeiling},
		{"lost_at_sea", "settled_safe", 0},
	} {
		t.Run(test.fate+"/"+test.status, func(t *testing.T) {
			h := newRecoveryHarness(t)
			base := time.Now().UTC()
			proxy := installUWExecProxy(t, h)
			proxy.insuranceRows = uwExecBoardRows(base)
			proxy.membership = map[string]bool{uwExecPolicyID: true}
			id, _ := uwExecSeedPendingUnderwrite(t, h, base)
			owned := uwExecRow(recoveryCompanyID, uwExecSliceBPS, uwExecCollateralCeiling, uwExecPremiumFloor+1)
			if err := h.db.CompleteAction(h.ctx, id, http.StatusOK, uwExecEnvelope(uwExecPolicy(base), []map[string]any{owned}), nil); err != nil {
				t.Fatal(err)
			}
			if _, err := h.db.EnsureUnderwritingBudget(h.ctx, h.account.ID, uwExecCollateralCeiling); err != nil {
				t.Fatal(err)
			}
			runner := h.runner(primaryTestKey)
			uwExecTicks(t, h, runner, 2)
			uwExecRequirePosition(t, h, id, uwExecCollateralCeiling, uwExecPremiumFloor+1, "fate receipt")
			ship := operationShip(recoveryShipBlastoise, "Blastoise", "sunk", "")
			ship.Ship.Fate = test.fate
			runner.stateCache.Ships = []ShipState{ship}
			policy := uwExecPolicy(base)
			policy.Status, policy.UpdatedAt = test.status, time.Now().UTC()
			runner.snapshot["insurance"] = encode([]InsurancePolicy{policy})
			if err := runner.recordUnderwriting(h.ctx); err != nil {
				t.Fatal(err)
			}
			positions, err := h.db.UnderwritingPositions(h.ctx, h.account.ID)
			if err != nil || len(positions) != 1 {
				t.Fatalf("load payout evidence: %+v, %v", positions, err)
			}
			position := positions[0]
			if test.status == "active" {
				if position.ClosedAt != nil || position.ClaimsPence != nil || position.EarnedPremiumPence != nil {
					t.Fatalf("ship fate fabricated an unobserved payout: %+v", position)
				}
			} else if position.Status != test.status || position.ClaimsPence == nil || *position.ClaimsPence != test.claim {
				t.Fatalf("ship fate overrode an authoritative policy outcome: %+v", position)
			}
		})
	}
}
