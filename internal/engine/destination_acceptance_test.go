package engine

// Acceptance contracts for destination completion (plan E2, E3, E8, E9, E12,
// E13, E16-E17) against the real Runner seams, the fake game API and an
// isolated PostgreSQL schema: newly created economic missions carry
// FinishAtDestination and settle at their delivery port without an empty
// return, legacy missions without the flag keep the return journey, closing
// honors the turnaround lock and refuses residual cargo, active seals and
// unresolved actions, and a failed relocation probe closes at its source as a
// cost-only zero-cargo settlement. Seeded missions use the durable JSON field
// names agreed in the plan (finish_at_destination, relocation.*) so the suite
// pins the persisted format; the typed reads below fail to compile until the
// implementation lands.

import (
	"encoding/json"
	"fmt"
	"net/http"
	"testing"
	"time"

	"github.com/DevYukine/go-tradewinds/internal/store"
)

// destinationShipRow builds one fleet row for a stationary ship. Closing
// scenarios use the anchored roadstead: the fake serves no offshore endpoint
// and the offshore move is a berthed-only closing step, so the roadstead is
// the stationary state whose closing is fully exercisable end to end.
func destinationShipRow(id, name, port, status string, storesTons float64, unlock *time.Time, cargo map[string]int64) map[string]any {
	row := sailingShipRow(id, name, port, "")
	ship := row["ship"].(map[string]any)
	ship["status"] = status
	ship["current_port_id"] = port
	// Seed the already-maintained state (4 of 4 cannons, hull 60t minus 8t
	// gun displacement) so idle economic planning is not preempted by the
	// mandatory shipyard refit at a shipyard port.
	ship["cannons_count"] = 4
	ship["turnaround_unlocks_at"] = unlock
	ship["dock_queued_at"] = nil
	ship["berthed_at"] = nil
	row["route_legs"] = []any{}
	row["voyage"] = nil
	row["stores"] = map[string]any{"provisions_tons": storesTons, "sea_days_remaining": 5}
	row["effective_hold_capacity_tons"] = 52
	manifest := map[string]any{}
	var used int64
	for commodity, tons := range cargo {
		manifest[commodity] = tons
		used += tons
	}
	row["cargo_manifest"] = manifest
	row["cargo_used_tons"] = used
	return row
}

func destinationPutRawMission(t *testing.T, h *recoveryHarness, shipID, generation, phase, data string) {
	t.Helper()
	if err := h.db.PutMission(h.ctx, &store.Mission{
		AccountID:  h.account.ID,
		ShipID:     shipID,
		Phase:      phase,
		Generation: generation,
		Data:       json.RawMessage(data),
	}); err != nil {
		t.Fatalf("persist seeded mission %s: %v", shipID, err)
	}
}

// destinationSpotSeed writes the durable mission of a spot voyage that bought
// 24t grain at laport and sold it at portb, in the agreed missionData JSON
// format including the new finish_at_destination flag.
func destinationSpotSeed(t *testing.T, finish bool, extra string) string {
	t.Helper()
	startedAt := time.Now().UTC().Add(-2 * time.Hour).Format(time.RFC3339Nano)
	return fmt.Sprintf(`{
		"plan":{"kind":"spot","ship_id":%q,"origin":"laport","destination":"portb","commodity":"grain","quantity":24},
		"proven_lane":"laport|portb|grain|spot",
		"started_at":%q,"started_game_hours":2398,
		"initial_ledger_ids":{},"eligible":{},"departed_cargo":{"grain":24},
		"bought_quantity":24,"delivered_quantity":24,
		"acquisition_pence":4800,"proceeds_pence":30000,"fees_pence":92,"provisions_pence":208,
		"costs_known":true,"last_action_id":"dest-action-1","rejected_charter_ids":{},
		"finish_at_destination":%t%s
	}`, recoveryShipBlastoise, startedAt, finish, extra)
}

// destinationRelocationSeed writes the durable mission of a relocation probe
// that arrived at the portb source, spent its modeled allowance (the observed
// remittance fee is recorded here exactly once) and found no usable fresh
// evidence for the targeted commodity.
func destinationRelocationSeed(t *testing.T) string {
	t.Helper()
	startedAt := time.Now().UTC().Add(-2 * time.Hour).Format(time.RFC3339Nano)
	return fmt.Sprintf(`{
		"plan":{"kind":"relocation","ship_id":%q,"origin":"laport","destination":"portb","commodity":"sawn_planks"},
		"proven_lane":"portb|laport|sawn_planks|charter",
		"started_at":%q,"started_game_hours":2398,
		"initial_ledger_ids":{},"eligible":{},"rejected_charter_ids":{},
		"fees_pence":185,"costs_known":true,"last_action_id":"relocation-action-1",
		"finish_at_destination":true,
		"relocation":{
			"target_contract_id":"charter-x","target_commodity":"sawn_planks","target_destination":"laport",
			"expense_cap_pence":20000,"modeled_expense_pence":18500,"funding_required_pence":0,
			"inspect_until_game_hour":2406
		}
	}`, recoveryShipBlastoise, startedAt)
}

func destinationSeedAction(t *testing.T, h *recoveryHarness, id, path, body, response string) {
	t.Helper()
	if _, err := h.db.BeginAction(h.ctx, &store.Action{
		ID:        id,
		AccountID: h.account.ID,
		Method:    http.MethodPost,
		Path:      path,
		Body:      json.RawMessage(body),
	}); err != nil {
		t.Fatalf("seed completed action %s: %v", id, err)
	}
	if err := h.db.CompleteAction(h.ctx, id, http.StatusOK, json.RawMessage(response), nil); err != nil {
		t.Fatalf("settle seeded action %s: %v", id, err)
	}
}

func destinationSettlements(t *testing.T, h *recoveryHarness, lane string) []store.Settlement {
	t.Helper()
	rows, err := h.db.SettlementHistory(h.ctx, h.account.ID, lane, 100)
	if err != nil {
		t.Fatalf("load settlements: %v", err)
	}
	return rows
}

func destinationSeedSoldVoyage(t *testing.T, h *recoveryHarness, finish bool, extra string) {
	t.Helper()
	h.api.setFleet([]map[string]any{destinationShipRow(recoveryShipBlastoise, "Blastoise", recoveryFarPort, "anchored_roadstead", 3, nil, nil)})
	destinationSeedAction(t, h, "dest-action-1", "/v1/market/place-order",
		fmt.Sprintf(`{"ship_id":%q,"port_id":"portb","commodity_id":"grain","side":"ask","quantity":24,"price_pence":1250,"time_in_force":"ioc"}`, recoveryShipBlastoise),
		`{"commodity_id":"grain","filled_quantity":24,"total_gross_pence":30000,"customs_duty_pence":92}`)
	destinationPutRawMission(t, h, recoveryShipBlastoise, "dest-mission-1", "arrival", destinationSpotSeed(t, finish, extra))
}

func TestNewEconomicMissionsCarryFinishAtDestination(t *testing.T) {
	h := newRecoveryHarness(t)
	// One legitimate far factor authorizes the executable portb bids as
	// demand evidence for the spot plan; its native upkeep stays modeled.
	h.api.setFactorPorts(recoveryFarPort)
	h.api.setFleet([]map[string]any{destinationShipRow(recoveryShipBlastoise, "Blastoise", recoveryHomePort, "berthed", 3, nil, nil)})
	runner := h.runner(primaryTestKey)
	if _, err := h.tick(runner); err != nil {
		t.Fatalf("plan a fresh spot voyage: %v", err)
	}
	mission, data := h.mission(recoveryShipBlastoise)
	if mission.Phase != "provision" || data.Plan.Kind != "spot" || data.Plan.Destination != recoveryFarPort {
		t.Fatalf("expected a fresh spot voyage mission toward the measured destination: phase %s plan %+v", mission.Phase, data.Plan)
	}
	if !data.FinishAtDestination {
		t.Fatal("newly created economic mission must complete at its destination instead of planning an empty return")
	}
	if h.api.mutationCount("/v1/fleet/depart") != 0 {
		t.Fatal("planning a new voyage sent a departure")
	}
}

func TestDestinationCompletionSettlesOnceAndNeverReturnsEmpty(t *testing.T) {
	h := newRecoveryHarness(t)
	destinationSeedSoldVoyage(t, h, true, "")
	runner := h.runner(primaryTestKey)
	if _, err := h.tick(runner); err != nil {
		t.Fatalf("begin destination closing: %v", err)
	}
	if h.api.mutationCount("/v1/fleet/depart") != 0 {
		t.Fatal("destination closing staged an empty return departure")
	}
	row, err := h.db.GetMission(h.ctx, h.account.ID, recoveryShipBlastoise)
	if err != nil {
		t.Fatal(err)
	}
	if row == nil {
		// A collapsed implementation settles within one tick; the durable
		// guarantees below must hold for both shapes.
		destinationOnlySettlement(t, h)
		return
	}
	if _, data := h.mission(recoveryShipBlastoise); data.Settlement == nil {
		t.Fatalf("closing did not freeze the immutable settlement durably: %+v", data)
	}
	if rows := destinationSettlements(t, h, ""); len(rows) != 0 {
		t.Fatalf("settlement was published before its durable freeze: %+v", rows)
	}
	// A crash between the frozen settlement and its insertion replays the
	// identical settlement exactly once, then releases the mission.
	restarted := h.runner(primaryTestKey)
	if _, err := h.tick(restarted); err != nil {
		t.Fatalf("publish frozen settlement after restart: %v", err)
	}
	settlement := destinationOnlySettlement(t, h)
	for range 4 {
		persisted, err := h.db.GetMission(h.ctx, h.account.ID, recoveryShipBlastoise)
		if err != nil {
			t.Fatal(err)
		}
		if persisted == nil {
			break
		}
		if _, err := h.tick(restarted); err != nil {
			t.Fatalf("release settled mission: %v", err)
		}
	}
	h.missingMission(recoveryShipBlastoise)
	if again := destinationOnlySettlement(t, h); again.ID != settlement.ID {
		t.Fatalf("settlement duplicated across the restart boundary: %s then %s", settlement.ID, again.ID)
	}
	// Fresh local planning replaces the legacy return: the ship stays at the
	// destination and never sails home empty.
	final := h.runner(primaryTestKey)
	for range 3 {
		if _, err := h.tick(final); err != nil {
			t.Fatalf("fresh destination planning: %v", err)
		}
	}
	if h.api.mutationCount("/v1/fleet/depart") != 0 {
		t.Fatal("a completed economic mission returned home empty")
	}
}

func destinationOnlySettlement(t *testing.T, h *recoveryHarness) store.Settlement {
	t.Helper()
	rows := destinationSettlements(t, h, "laport|portb|grain|spot")
	if len(rows) != 1 {
		t.Fatalf("expected exactly one destination settlement, got %+v", rows)
	}
	s := rows[0]
	if s.Kind != "spot" || s.Origin != recoveryHomePort || s.Destination != recoveryFarPort ||
		s.Commodity != recoveryGrain || s.QuantityTons != 24 || !s.CostsKnown ||
		s.AcquisitionPence != 4_800 || s.ProceedsPence != 30_000 ||
		s.FeesPence != 92 || s.ProvisionsPence != 208 || s.ExpensesPence != 0 ||
		s.MissionID != "dest-mission-1" || s.ActionID != "dest-action-1" || s.GameSeconds < 3_600 {
		t.Fatalf("frozen destination settlement disagrees with the durable mission accounting: %+v", s)
	}
	if net := s.ProceedsPence - s.AcquisitionPence - s.FeesPence - s.ProvisionsPence - s.ExpensesPence; net != 24_900 {
		t.Fatalf("settlement net identity broken: %dp", net)
	}
	return s
}

func TestDestinationCompletionHonorsTurnaroundUnlock(t *testing.T) {
	h := newRecoveryHarness(t)
	unlock := time.Now().UTC().Add(15 * time.Minute)
	h.api.setFleet([]map[string]any{destinationShipRow(recoveryShipBlastoise, "Blastoise", recoveryFarPort, "anchored_roadstead", 3, &unlock, nil)})
	destinationSeedAction(t, h, "dest-action-1", "/v1/market/place-order", `{"ok":true}`, `{"commodity_id":"grain","filled_quantity":24,"total_gross_pence":30000,"customs_duty_pence":92}`)
	destinationPutRawMission(t, h, recoveryShipBlastoise, "dest-mission-1", "arrival", destinationSpotSeed(t, true, ""))
	runner := h.runner(primaryTestKey)
	if _, err := h.tick(runner); err != nil {
		t.Fatalf("locked closing tick: %v", err)
	}
	if rows := destinationSettlements(t, h, ""); len(rows) != 0 {
		t.Fatalf("settlement was frozen while the turnaround lock held: %+v", rows)
	}
	if row, err := h.db.GetMission(h.ctx, h.account.ID, recoveryShipBlastoise); err != nil || row == nil {
		t.Fatalf("locked closing discarded the durable mission: row %v err %v", row, err)
	}
	h.api.setFleet([]map[string]any{destinationShipRow(recoveryShipBlastoise, "Blastoise", recoveryFarPort, "anchored_roadstead", 3, nil, nil)})
	for range 6 {
		persisted, err := h.db.GetMission(h.ctx, h.account.ID, recoveryShipBlastoise)
		if err != nil {
			t.Fatal(err)
		}
		if persisted == nil {
			break
		}
		if _, err := h.tick(runner); err != nil {
			t.Fatalf("unlocked closing tick: %v", err)
		}
	}
	h.missingMission(recoveryShipBlastoise)
	if rows := destinationSettlements(t, h, "laport|portb|grain|spot"); len(rows) != 1 {
		t.Fatalf("unlocked closing did not settle exactly once: %+v", rows)
	}
	if h.api.mutationCount("/v1/fleet/depart") != 0 {
		t.Fatal("destination completion staged an empty return departure")
	}
}

func TestLegacyMissionWithoutFinishFlagReturnsHome(t *testing.T) {
	h := newRecoveryHarness(t)
	h.api.setFleet([]map[string]any{destinationShipRow(recoveryShipBlastoise, "Blastoise", recoveryFarPort, "anchored_roadstead", 3, nil, nil)})
	destinationSeedAction(t, h, "legacy-action-1", "/v1/market/place-order", `{"ok":true}`, `{"commodity_id":"grain","filled_quantity":24,"total_gross_pence":30000,"customs_duty_pence":92}`)
	destinationPutRawMission(t, h, recoveryShipBlastoise, "legacy-mission-1", "arrival", destinationSpotSeed(t, false, ""))
	runner := h.runner(primaryTestKey)
	if _, err := h.tick(runner); err != nil {
		t.Fatalf("legacy arrival tick: %v", err)
	}
	mission, _ := h.mission(recoveryShipBlastoise)
	if mission.Phase != "return" {
		t.Fatalf("legacy mission without the flag did not enter the return journey: %s", mission.Phase)
	}
	if _, err := h.tick(runner); err != nil {
		t.Fatalf("legacy return staging: %v", err)
	}
	mission, data := h.mission(recoveryShipBlastoise)
	if !data.ReturnJourney || mission.Destination != recoveryHomePort {
		t.Fatalf("legacy return journey lost its origin: destination %s return %t", mission.Destination, data.ReturnJourney)
	}
	if _, err := h.tick(runner); err != nil {
		t.Fatalf("legacy return dispatch: %v", err)
	}
	calls := h.api.mutationsFor("/v1/fleet/depart")
	if len(calls) != 1 {
		t.Fatalf("legacy mission must still sail home: %+v", calls)
	}
	var body struct {
		Destination string `json:"dest_port_id"`
	}
	if err := json.Unmarshal(calls[0].Body, &body); err != nil || body.Destination != recoveryHomePort {
		t.Fatalf("legacy return departed toward the wrong port: %s %s", body.Destination, calls[0].Body)
	}
	if rows := destinationSettlements(t, h, ""); len(rows) != 0 {
		t.Fatalf("legacy return invented a destination settlement: %+v", rows)
	}
}

func TestRelocationProbeClosesAtSourceWithoutPurchaseOrReturn(t *testing.T) {
	h := newRecoveryHarness(t)
	h.api.setFleet([]map[string]any{destinationShipRow(recoveryShipBlastoise, "Blastoise", recoveryFarPort, "anchored_roadstead", 3, nil, nil)})
	destinationSeedAction(t, h, "relocation-action-1", "/v1/fleet/depart",
		fmt.Sprintf(`{"ship_id":%q,"dest_port_id":"portb"}`, recoveryShipBlastoise),
		`{"ship_id":"ship","dest_port_id":"portb","berth_dues_pence":0,"berth_dues_debt_pence":0}`)
	destinationPutRawMission(t, h, recoveryShipBlastoise, "relocation-mission-1", "arrival", destinationRelocationSeed(t))
	runner := h.runner(primaryTestKey)
	for range 6 {
		if _, err := h.tick(runner); err != nil {
			t.Fatalf("relocation closing tick: %v", err)
		}
		persisted, err := h.db.GetMission(h.ctx, h.account.ID, recoveryShipBlastoise)
		if err != nil {
			t.Fatal(err)
		}
		if persisted == nil {
			break
		}
	}
	h.missingMission(recoveryShipBlastoise)
	rows := destinationSettlements(t, h, "portb|laport|sawn_planks|charter")
	if len(rows) != 1 {
		t.Fatalf("expected exactly one cost-only relocation settlement: %+v", rows)
	}
	s := rows[0]
	if s.Kind != "relocation" || s.QuantityTons != 0 || s.ProceedsPence != 0 || s.AcquisitionPence != 0 ||
		s.ProvisionsPence != 0 || s.ExpensesPence != 0 || s.FeesPence != 185 || !s.CostsKnown ||
		s.Origin != recoveryHomePort || s.Destination != recoveryFarPort || s.Commodity != "sawn_planks" {
		t.Fatalf("relocation settlement is not the cost-only zero-cargo probe accounting: %+v", s)
	}
	if h.api.mutationCount("/v1/market/place-order") != 0 {
		t.Fatal("failed probe bought speculative cargo at the source")
	}
	if h.api.mutationCount("/v1/fleet/depart") != 0 {
		t.Fatal("closed probe sailed home empty instead of replanning where it stands")
	}
}

func TestDestinationClosingWaitsForCargoSealsAndResolvedActions(t *testing.T) {
	t.Run("residual unsealed cargo", func(t *testing.T) {
		h := newRecoveryHarness(t)
		h.api.setFleet([]map[string]any{destinationShipRow(recoveryShipBlastoise, "Blastoise", recoveryFarPort, "anchored_roadstead", 3, nil, map[string]int64{recoveryGrain: 10})})
		destinationSeedAction(t, h, "dest-action-1", "/v1/market/place-order", `{"ok":true}`, `{"commodity_id":"grain","filled_quantity":24,"total_gross_pence":30000,"customs_duty_pence":92}`)
		destinationPutRawMission(t, h, recoveryShipBlastoise, "dest-mission-1", "arrival", destinationSpotSeed(t, true, ""))
		runner := h.runner(primaryTestKey)
		for range 2 {
			if _, err := h.tick(runner); err != nil {
				t.Fatalf("residual cargo tick: %v", err)
			}
		}
		if rows := destinationSettlements(t, h, ""); len(rows) != 0 {
			t.Fatalf("closing settled with ordinary residual cargo aboard: %+v", rows)
		}
		if row, err := h.db.GetMission(h.ctx, h.account.ID, recoveryShipBlastoise); err != nil || row == nil {
			t.Fatalf("residual cargo destroyed the durable mission: row %v err %v", row, err)
		}
	})
	t.Run("residual cargo on a relocation probe", func(t *testing.T) {
		h := newRecoveryHarness(t)
		h.api.setFleet([]map[string]any{destinationShipRow(recoveryShipBlastoise, "Blastoise", recoveryFarPort, "anchored_roadstead", 3, nil, map[string]int64{recoveryGrain: 10})})
		destinationSeedAction(t, h, "relocation-action-1", "/v1/fleet/depart", `{"ok":true}`, `{"ship_id":"ship","dest_port_id":"portb","berth_dues_pence":0,"berth_dues_debt_pence":0}`)
		destinationPutRawMission(t, h, recoveryShipBlastoise, "relocation-mission-1", "arrival", destinationRelocationSeed(t))
		runner := h.runner(primaryTestKey)
		for range 2 {
			if _, err := h.tick(runner); err != nil {
				t.Fatalf("probe residual cargo tick: %v", err)
			}
		}
		if rows := destinationSettlements(t, h, ""); len(rows) != 0 {
			t.Fatalf("closing settled a relocation probe with cargo aboard: %+v", rows)
		}
		if row, err := h.db.GetMission(h.ctx, h.account.ID, recoveryShipBlastoise); err != nil || row == nil {
			t.Fatalf("probe residual cargo destroyed the durable mission: row %v err %v", row, err)
		}
	})
	t.Run("active sealed obligation", func(t *testing.T) {
		h := newRecoveryHarness(t)
		h.api.setFleet([]map[string]any{destinationShipRow(recoveryShipBlastoise, "Blastoise", recoveryFarPort, "anchored_roadstead", 3, nil, nil)})
		destinationSeedAction(t, h, "dest-action-1", "/v1/market/place-order", `{"ok":true}`, `{"commodity_id":"grain","filled_quantity":24,"total_gross_pence":30000,"customs_duty_pence":92}`)
		seal := `,"freight":[{"contract_id":"seal-1","commodity":"grain","quantity":10,"destination":"portb","reward_pence":30000,"collateral_pence":5000}]`
		destinationPutRawMission(t, h, recoveryShipBlastoise, "dest-mission-1", "arrival", destinationSpotSeed(t, true, seal))
		runner := h.runner(primaryTestKey)
		for range 2 {
			if _, err := h.tick(runner); err != nil {
				t.Fatalf("sealed obligation tick: %v", err)
			}
		}
		if rows := destinationSettlements(t, h, ""); len(rows) != 0 {
			t.Fatalf("closing settled while a sealed obligation was unresolved: %+v", rows)
		}
		if row, err := h.db.GetMission(h.ctx, h.account.ID, recoveryShipBlastoise); err != nil || row == nil {
			t.Fatalf("sealed obligation destroyed the durable mission: row %v err %v", row, err)
		}
	})
	t.Run("orphaned unresolved request", func(t *testing.T) {
		h := newRecoveryHarness(t)
		h.api.setFleet([]map[string]any{destinationShipRow(recoveryShipBlastoise, "Blastoise", recoveryFarPort, "anchored_roadstead", 3, nil, nil)})
		destinationPutRawMission(t, h, recoveryShipBlastoise, "dest-mission-1", "arrival", destinationSpotSeed(t, true, ""))
		if _, err := h.db.BeginAction(h.ctx, &store.Action{
			ID:        "orphan-action-1",
			AccountID: h.account.ID,
			Method:    http.MethodPost,
			Path:      "/v1/fleet/depart",
			Body:      json.RawMessage(`{"ship_id":"ship","dest_port_id":"portb"}`),
		}); err != nil {
			t.Fatalf("seed unresolved action: %v", err)
		}
		runner := h.runner(primaryTestKey)
		if _, err := h.tick(runner); err != nil {
			t.Fatalf("orphaned request tick: %v", err)
		}
		if rows := destinationSettlements(t, h, ""); len(rows) != 0 {
			t.Fatalf("account settled while an unresolved request existed: %+v", rows)
		}
		if row, err := h.db.GetMission(h.ctx, h.account.ID, recoveryShipBlastoise); err != nil || row == nil {
			t.Fatalf("unresolved request destroyed the durable mission: row %v err %v", row, err)
		}
	})
}
