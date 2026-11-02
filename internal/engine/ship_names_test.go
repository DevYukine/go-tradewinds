package engine

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"slices"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/DevYukine/go-tradewinds/internal/gameapi"
	"github.com/DevYukine/go-tradewinds/internal/store"
)

func TestShipSnapshotRetainsFateAndFormerNames(t *testing.T) {
	for _, fate := range []string{"lost_at_sea", "captured", "seized_smuggling", "seized_bailiff", "seized_bankruptcy", "broken_up", "royal_navy", ""} {
		t.Run(fate, func(t *testing.T) {
			var ship ShipState
			if err := json.Unmarshal([]byte(`{"ship":{"id":"hull","status":"sunk","fate":"`+fate+`"},"former_names":[{"name":"Lapras","renamed_at":"2026-11-01T10:00:00Z"}]}`), &ship); err != nil {
				t.Fatal(err)
			}
			state := StateFromSnapshot(time.Now(), map[string]json.RawMessage{"ships": encode([]ShipState{ship})}, PlanningSources{})
			if len(state.Ships) != 1 || state.Ships[0].Ship.Fate != fate || len(state.Ships[0].FormerNames) != 1 ||
				state.Ships[0].FormerNames[0].Name != "Lapras" || state.Ships[0].FormerNames[0].RenamedAt.IsZero() {
				t.Fatalf("snapshot projection discarded fleet history: %+v", state.Ships)
			}
		})
	}
	var afloat ShipState
	if err := json.Unmarshal([]byte(`{"ship":{"status":"sailing","fate":null}}`), &afloat); err != nil || afloat.Ship.Fate != "" {
		t.Fatalf("afloat fate must remain absent: %+v, %v", afloat, err)
	}
}

func TestShipNamesReserveFormerSunkAndPendingNamesAcrossRestart(t *testing.T) {
	t.Parallel()
	h := newRecoveryHarness(t)
	live := sailingShipRow(recoveryShipBlastoise, "Merchant hull", recoveryHomePort, recoveryFarPort)
	live["former_names"] = []gameapi.FormerShipName{{Name: "Lapras", RenamedAt: time.Now()}}
	gone := sunkShipRow(recoveryShipLapras, "Bulbasaur")
	gone["former_names"] = []gameapi.FormerShipName{{Name: "Ivysaur", RenamedAt: time.Now()}}
	h.api.setFleet([]map[string]any{live, gone})
	h.putMission("__growth", "reserved-build", "growth_finance", missionData{BuildName: "Venusaur"})
	h.putMission(shipRenameMission, "reserved-rename", "rename", missionData{Rename: &shipRenameIntent{ShipID: recoveryShipBlastoise, Name: "Charmeleon"}})
	runner := h.runner(primaryTestKey)
	actionLogRefresh(t, h, runner)
	runner.snapshot["_used_ship_names"] = encode(append(decode[[]string](runner.snapshot, "_used_ship_names"), "Charmander"))
	name, ready, err := runner.reserveShipName(h.ctx, shipRenameMission)
	if err != nil || !ready || name != "Charizard" {
		t.Fatalf("historical or pending name was recycled: %q, %v", name, err)
	}
	if err := h.db.DeleteMission(h.ctx, h.account.ID, "__growth"); err != nil {
		t.Fatal(err)
	}
	if err := h.db.DeleteMission(h.ctx, h.account.ID, shipRenameMission); err != nil {
		t.Fatal(err)
	}
	h.api.setFleet([]map[string]any{})
	runner = h.runner(primaryTestKey)
	actionLogRefresh(t, h, runner)
	name, ready, err = runner.reserveShipName(h.ctx, shipRenameMission)
	if err != nil || !ready || name != "Squirtle" {
		t.Fatalf("restart recycled a disappeared hull or retired intent name: %q, %v", name, err)
	}
}

func TestShipNamesWaitForAnAuthoritativeCaptureBeforeReserving(t *testing.T) {
	for _, control := range []bool{false, true} {
		t.Run(map[bool]string{false: "financial", true: "control"}[control], func(t *testing.T) {
			h := newRecoveryHarness(t)
			runner := h.runner(primaryTestKey)
			reason := "financial event invalidated the captured funds; authoritative resynchronization precedes staging"
			if control {
				runner.publishedControl.Store(1)
				reason = "stream control invalidated the captured state; authoritative resynchronization precedes staging"
			} else {
				runner.publishedFinancial.Store(1)
			}
			for range 3 {
				if name, ready, err := runner.reserveShipName(h.ctx, "__growth"); name != "" || ready || err != nil {
					t.Fatalf("invalidated capture reserved a name: name=%q, ready=%t, %v", name, ready, err)
				}
			}
			if _, exists := runner.snapshot["_used_ship_names"]; exists {
				t.Fatal("deferred allocation consumed a catalog name")
			}
			if len(runner.holding) != 3 {
				t.Fatalf("deferred allocation did not preserve its authority hold: %v", runner.holding)
			}
			for _, hold := range runner.holding {
				if hold.subjectID != "__growth" || hold.Reason != reason {
					t.Fatalf("deferred allocation lost its caller's authority hold: %+v", hold)
				}
			}
		})
	}
}

func TestShipRenameNeverStagesForCatalogListedOrGoneShips(t *testing.T) {
	for _, test := range []struct {
		name, status, fate string
		listed             bool
	}{
		{name: "Lapras", status: "sailing"},
		{name: "Mr. Mime 2", status: "berthed"},
		{name: "Merchant hull", status: "berthed", listed: true},
		{name: "Merchant hull", status: "sunk", fate: "lost_at_sea"},
		{name: "Merchant hull", status: "sunk", fate: "broken_up"},
	} {
		t.Run(test.name+"/"+test.status+"/"+test.fate, func(t *testing.T) {
			ship := ShipState{}
			ship.Ship.ID, ship.Ship.Name, ship.Ship.Status, ship.Ship.Fate = "owned-hull", test.name, test.status, test.fate
			runner := &Runner{
				opts: Options{Execute: true}, snapshot: make(map[string]json.RawMessage),
				meta: map[string]DatasetMeta{
					"ships":         {Complete: true, UpdatedAt: time.Now()},
					"ship_listings": {Complete: true, UpdatedAt: time.Now()},
				},
				stateCache: AccountState{Ships: []ShipState{ship}},
			}
			if test.listed {
				runner.snapshot["ship_listings"] = json.RawMessage(`[{"ship_id":"owned-hull","status":"open"}]`)
			}
			if changed, err := runner.driveShipRename(context.Background(), nil, &missionData{}); changed || err != nil {
				t.Fatalf("ineligible hull acquired a naming mutation: changed=%t, %v", changed, err)
			}
		})
	}
}

func TestShipRenameStagesOnceAndSurvivesRestartWithImmutableRequest(t *testing.T) {
	t.Parallel()
	for _, journaled := range []bool{false, true} {
		label := "before journal"
		if journaled {
			label = "after journal"
		}
		t.Run(label, func(t *testing.T) {
			h := newRecoveryHarness(t)
			h.api.setFleet([]map[string]any{sailingShipRow(recoveryShipBlastoise, "Endeavour", recoveryHomePort, recoveryFarPort)})
			var sends atomic.Int64
			actionLogIntercept(t, h, func(writer http.ResponseWriter, request *http.Request) {
				body, err := io.ReadAll(request.Body)
				if err != nil {
					t.Error(err)
					return
				}
				mission, data := h.mission(shipRenameMission)
				if request.Method != http.MethodPost || request.URL.Path != "/v1/fleet/rename-ship" ||
					mission == nil || data.Action == nil || data.Action.ID != request.Header.Get("Idempotency-Key") || !bytes.Equal(data.Action.Body, body) {
					t.Error("rename reached transport without its durable immutable request")
					writer.WriteHeader(http.StatusBadRequest)
					return
				}
				if sends.Add(1) > 1 {
					t.Error("logical rename was sent twice")
				}
				former := []gameapi.FormerShipName{{Name: "Endeavour", RenamedAt: time.Now().UTC()}}
				h.api.mu.Lock()
				h.api.fleet[0]["ship"].(map[string]any)["name"] = data.Rename.Name
				h.api.fleet[0]["former_names"] = former
				h.api.mu.Unlock()
				h.api.writeJSON(writer, http.StatusOK, map[string]any{"ship_id": data.Rename.ShipID, "name": data.Rename.Name, "former_names": former})
			})
			runner := h.runner(primaryTestKey)
			actionLogRefresh(t, h, runner)
			if changed, err := runner.work(h.ctx); err != nil || !changed {
				t.Fatalf("work did not stage the acquired hull's rename: changed=%t, %v", changed, err)
			}
			mission, data := h.mission(shipRenameMission)
			if data.Action == nil || data.Rename == nil || sends.Load() != 0 {
				t.Fatalf("rename was not durably staged before sending: %+v", data)
			}
			id, body := data.Action.ID, append([]byte(nil), data.Action.Body...)
			if changed, err := runner.driveShipRename(h.ctx, mission, &data); changed || err != nil || data.Action.ID != id {
				t.Fatalf("a staged rename was replaced: changed=%t, %v", changed, err)
			}
			if journaled {
				if _, err := h.db.BeginAction(h.ctx, &store.Action{ID: id, AccountID: h.account.ID, Method: data.Action.Method, Path: data.Action.Path, Body: body}); err != nil {
					t.Fatal(err)
				}
			}
			runner = h.runner(primaryTestKey)
			actionLogRefresh(t, h, runner)
			mission, data = h.mission(shipRenameMission)
			for attempt := 0; attempt < 2 && data.Action != nil; attempt++ {
				if data.Action.ID != id || !bytes.Equal(data.Action.Body, body) {
					t.Fatal("restart changed the logical rename identity or payload")
				}
				if _, err := runner.dispatch(h.ctx, mission, &data); err != nil {
					t.Fatal(err)
				}
			}
			if sends.Load() != 1 || data.Action != nil || mission.Phase != "done" {
				t.Fatalf("rename did not complete exactly once: sends=%d, %+v", sends.Load(), data)
			}
			if changed, err := runner.driveShipRename(h.ctx, mission, &data); changed || err != nil {
				t.Fatalf("old fleet capture released completed rename before REST agreed: changed=%t, %v", changed, err)
			}
			actionLogRefresh(t, h, runner)
			if changed, err := runner.driveShipRename(h.ctx, mission, &data); !changed || err != nil {
				t.Fatalf("confirmed catalog name did not release the rename control: changed=%t, %v", changed, err)
			}
			if changed, err := runner.driveShipRename(h.ctx, nil, &missionData{}); changed || err != nil || sends.Load() != 1 {
				t.Fatalf("catalog-named hull acquired another rename: changed=%t, sends=%d, %v", changed, sends.Load(), err)
			}
		})
	}
}

func TestShipRenameRecoversAnObservedFormerNameWithoutResending(t *testing.T) {
	t.Parallel()
	h := newRecoveryHarness(t)
	h.api.setFleet([]map[string]any{sailingShipRow(recoveryShipBlastoise, "Endeavour", recoveryHomePort, recoveryFarPort)})
	var sends atomic.Int64
	actionLogIntercept(t, h, func(writer http.ResponseWriter, request *http.Request) {
		sends.Add(1)
		writer.WriteHeader(http.StatusInternalServerError)
	})
	runner := h.runner(primaryTestKey)
	actionLogRefresh(t, h, runner)
	if _, err := runner.driveShipRename(h.ctx, nil, &missionData{}); err != nil {
		t.Fatal(err)
	}
	_, data := h.mission(shipRenameMission)
	action := &store.Action{
		ID: data.Action.ID, AccountID: h.account.ID,
		Method: data.Action.Method, Path: data.Action.Path, Body: data.Action.Body,
	}
	if _, err := h.db.BeginAction(h.ctx, action); err != nil {
		t.Fatal(err)
	}
	serverTime := time.Now().UTC().Add(-time.Hour)
	h.api.mu.Lock()
	h.api.fleet[0]["ship"].(map[string]any)["name"] = "Owner's preference"
	h.api.fleet[0]["former_names"] = []gameapi.FormerShipName{
		{Name: data.Rename.Name, RenamedAt: serverTime},
		{Name: "Endeavour", RenamedAt: serverTime.Add(-time.Second)},
	}
	h.api.mu.Unlock()
	runner = h.runner(primaryTestKey)
	actionLogRefresh(t, h, runner)
	mission, data := h.mission(shipRenameMission)
	if changed, err := runner.dispatch(h.ctx, mission, &data); !changed || err != nil {
		t.Fatalf("authoritative rename was not reconciled after restart: changed=%t, %v", changed, err)
	}
	if data.Action != nil || mission.Phase != "done" || sends.Load() != 0 {
		t.Fatalf("observed rename was resent or left unresolved: sends=%d, %+v", sends.Load(), data)
	}
	persisted, err := h.db.BeginAction(h.ctx, action)
	if err != nil || persisted.State != store.ActionReconciled {
		t.Fatalf("rename observation was not journaled durably: %+v, %v", persisted, err)
	}
	if changed, err := runner.driveShipRename(h.ctx, mission, &data); !changed || err != nil {
		t.Fatalf("observed former name did not release the completed rename: changed=%t, %v", changed, err)
	}
	h.missingMission(shipRenameMission)
	runner = h.runner(primaryTestKey)
	actionLogRefresh(t, h, runner)
	next := missionData{}
	if changed, err := runner.driveShipRename(h.ctx, nil, &next); !changed || err != nil || sends.Load() != 0 {
		t.Fatalf("a later non-catalog name did not acquire its own staged rename: changed=%t, sends=%d, %v", changed, sends.Load(), err)
	}
	if next.Rename.Name == data.Rename.Name || next.Action.ID == action.ID || next.Action.Before.Ship.Name != "Owner's preference" {
		t.Fatalf("a later rename reused the former catalog name or original request: %+v", next)
	}
}

func TestShipRenameRetainsOrphanedRequestBeforeAnotherRename(t *testing.T) {
	t.Parallel()
	h := newRecoveryHarness(t)
	h.api.setFleet([]map[string]any{sailingShipRow(recoveryShipBlastoise, "Endeavour", recoveryHomePort, recoveryFarPort)})
	runner := h.runner(primaryTestKey)
	actionLogRefresh(t, h, runner)
	id := ActionID(h.account.ID, "orphaned-rename", "rename", 0)
	if _, err := h.db.BeginAction(h.ctx, &store.Action{
		ID: id, AccountID: h.account.ID, Method: http.MethodPost, Path: "/v1/fleet/rename-ship",
		Body: encode(shipRenameIntent{ShipID: recoveryShipBlastoise, Name: "Lapras"}),
	}); err != nil {
		t.Fatal(err)
	}
	if changed, err := runner.work(h.ctx); changed || err != nil ||
		!strings.Contains(joinedHoldReasons(runner.holding), "orphaned unresolved journal request") {
		t.Fatalf("orphaned rename did not preserve the original mutation before new work: changed=%t, holds=%+v, %v", changed, runner.holding, err)
	}
	h.missingMission(shipRenameMission)
	if h.api.mutationCount("/v1/fleet/rename-ship") != 0 {
		t.Fatal("a second rename was sent without the original durable intent")
	}
}

func TestShipRenameRejectsNewSaleListingBeforeFirstSend(t *testing.T) {
	t.Parallel()
	h := newRecoveryHarness(t)
	h.api.setFleet([]map[string]any{sailingShipRow(recoveryShipBlastoise, "Endeavour", recoveryHomePort, recoveryFarPort)})
	runner := h.runner(primaryTestKey)
	actionLogRefresh(t, h, runner)
	if _, err := runner.driveShipRename(h.ctx, nil, &missionData{}); err != nil {
		t.Fatal(err)
	}
	mission, data := h.mission(shipRenameMission)
	runner.snapshot["ship_listings"] = json.RawMessage(`[{"ship_id":"` + recoveryShipBlastoise + `","status":"open"}]`)
	if err := runner.newActionPreflight(h.ctx, mission, &data); err == nil {
		t.Fatal("a newly listed ship passed rename preflight")
	}
}

func TestRejectedShipRenameWaitsForHullChangeAndLetsOtherHullsProceed(t *testing.T) {
	t.Parallel()
	for _, change := range []string{"name", "status"} {
		t.Run(change, func(t *testing.T) {
			h := newRecoveryHarness(t)
			first := sailingShipRow(recoveryShipBlastoise, "Endeavour", recoveryHomePort, recoveryFarPort)
			h.api.setFleet([]map[string]any{first})
			var sends atomic.Int64
			actionLogIntercept(t, h, func(writer http.ResponseWriter, request *http.Request) {
				sends.Add(1)
				h.api.writeProblem(writer, http.StatusConflict, "rename_refused", "ship cannot be renamed now")
			})
			runner := h.runner(primaryTestKey)
			actionLogRefresh(t, h, runner)
			if _, err := runner.driveShipRename(h.ctx, nil, &missionData{}); err != nil {
				t.Fatal(err)
			}
			mission, data := h.mission(shipRenameMission)
			name, firstID := data.Rename.Name, data.Action.ID
			if _, err := runner.dispatch(h.ctx, mission, &data); err != nil {
				t.Fatal(err)
			}
			h.missingMission(shipRenameMission)
			if !slices.Contains(decode[[]string](runner.snapshot, "_used_ship_names"), name) || sends.Load() != 1 {
				t.Fatal("refusal recycled its reserved name or sent another request")
			}
			runner = h.runner(primaryTestKey)
			actionLogRefresh(t, h, runner)
			if changed, err := runner.driveShipRename(h.ctx, nil, &missionData{}); changed || err != nil {
				t.Fatalf("restart retried an unchanged refused hull: changed=%t, %v", changed, err)
			}
			second := sailingShipRow(recoveryShipCharizard, "Endurance", recoveryFarPort, recoveryHomePort)
			h.api.setFleet([]map[string]any{first, second})
			actionLogRefresh(t, h, runner)
			if changed, err := runner.driveShipRename(h.ctx, nil, &missionData{}); !changed || err != nil {
				t.Fatalf("a refused hull blocked another hull's rename: changed=%t, %v", changed, err)
			}
			mission, data = h.mission(shipRenameMission)
			if data.Rename.ShipID != recoveryShipCharizard || data.Rename.Name == name {
				t.Fatalf("another hull did not receive a new reserved name: %+v", data.Rename)
			}
			if _, err := runner.dispatch(h.ctx, mission, &data); err != nil {
				t.Fatal(err)
			}
			h.missingMission(shipRenameMission)
			if change == "name" {
				first["ship"].(map[string]any)["name"] = "Changed name"
			} else {
				first["ship"].(map[string]any)["status"] = "berthed"
				first["voyage"], first["route_legs"] = nil, []any{}
			}
			h.api.setFleet([]map[string]any{first, second})
			actionLogRefresh(t, h, runner)
			if changed, err := runner.driveShipRename(h.ctx, nil, &missionData{}); !changed || err != nil {
				t.Fatalf("an observed %s change did not release the refusal latch: changed=%t, %v", change, changed, err)
			}
			_, data = h.mission(shipRenameMission)
			if data.Rename.ShipID != recoveryShipBlastoise || data.Rename.Name == name || data.Action.ID == firstID || sends.Load() != 2 {
				t.Fatalf("changed hull reused its refused name or request: sends=%d, %+v", sends.Load(), data)
			}
		})
	}
}

func TestRateLimitedShipRenameKeepsDurableBackoff(t *testing.T) {
	t.Parallel()
	h := newRecoveryHarness(t)
	h.api.setFleet([]map[string]any{sailingShipRow(recoveryShipBlastoise, "Endeavour", recoveryHomePort, recoveryFarPort)})
	var sends atomic.Int64
	actionLogIntercept(t, h, func(writer http.ResponseWriter, request *http.Request) {
		sends.Add(1)
		writer.WriteHeader(http.StatusInternalServerError)
	})
	runner := h.runner(primaryTestKey)
	actionLogRefresh(t, h, runner)
	if _, err := runner.driveShipRename(h.ctx, nil, &missionData{}); err != nil {
		t.Fatal(err)
	}
	mission, data := h.mission(shipRenameMission)
	name := data.Rename.Name
	action := &store.Action{
		ID: data.Action.ID, AccountID: h.account.ID,
		Method: data.Action.Method, Path: data.Action.Path, Body: data.Action.Body,
	}
	if _, err := h.db.BeginAction(h.ctx, action); err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().UTC().Add(10 * time.Minute)
	if err := h.db.CompleteAction(h.ctx, action.ID, http.StatusTooManyRequests, encode(map[string]string{"code": "rate_limited"}), &deadline); err != nil {
		t.Fatal(err)
	}
	// The journal receipt can commit before the mission records its cooldown.
	runner = h.runner(primaryTestKey)
	actionLogRefresh(t, h, runner)
	mission, data = h.mission(shipRenameMission)
	if _, err := runner.dispatch(h.ctx, mission, &data); err != nil {
		t.Fatal(err)
	}
	if data.Action != nil || time.Until(data.CooldownUntil) < 9*time.Minute || sends.Load() != 0 {
		t.Fatalf("stored rate limit lost its durable retry deadline: sends=%d, %+v", sends.Load(), data)
	}
	runner = h.runner(primaryTestKey)
	actionLogRefresh(t, h, runner)
	mission, data = h.mission(shipRenameMission)
	if changed, err := runner.driveShipRename(h.ctx, mission, &data); changed || err != nil || data.Rename.Name != name || sends.Load() != 0 {
		t.Fatalf("restart forgot the rate-limited rename: changed=%t, sends=%d, %v", changed, sends.Load(), err)
	}
}

func TestUncertainShipRenameBecomesMootWhenHullLeavesFleet(t *testing.T) {
	t.Parallel()
	for _, fate := range []string{"missing", "lost_at_sea", "seized_bailiff", "legacy sunk"} {
		t.Run(fate, func(t *testing.T) {
			h := newRecoveryHarness(t)
			h.api.setFleet([]map[string]any{sailingShipRow(recoveryShipBlastoise, "Endeavour", recoveryHomePort, recoveryFarPort)})
			var sends atomic.Int64
			actionLogIntercept(t, h, func(writer http.ResponseWriter, request *http.Request) {
				sends.Add(1)
				h.api.writeProblem(writer, http.StatusInternalServerError, "uncertain", "rename outcome unavailable")
			})
			runner := h.runner(primaryTestKey)
			actionLogRefresh(t, h, runner)
			if _, err := runner.driveShipRename(h.ctx, nil, &missionData{}); err != nil {
				t.Fatal(err)
			}
			mission, data := h.mission(shipRenameMission)
			action := &store.Action{
				ID: data.Action.ID, AccountID: h.account.ID,
				Method: data.Action.Method, Path: data.Action.Path, Body: data.Action.Body,
			}
			if _, err := runner.dispatch(h.ctx, mission, &data); err != nil || sends.Load() == 0 || data.Action == nil {
				t.Fatalf("rename did not remain uncertain after its attempted request: sends=%d, %v", sends.Load(), err)
			}
			attempts := sends.Load()
			if fate == "missing" {
				h.api.setFleet([]map[string]any{})
			} else {
				row := sunkShipRow(recoveryShipBlastoise, "Endeavour")
				if fate != "legacy sunk" {
					row["ship"].(map[string]any)["fate"] = fate
				}
				h.api.setFleet([]map[string]any{row})
			}
			runner = h.runner(primaryTestKey)
			actionLogRefresh(t, h, runner)
			mission, data = h.mission(shipRenameMission)
			if changed, err := runner.dispatch(h.ctx, mission, &data); !changed || err != nil {
				t.Fatalf("gone hull did not make the attempted rename moot: changed=%t, %v", changed, err)
			}
			h.missingMission(shipRenameMission)
			persisted, err := h.db.BeginAction(h.ctx, action)
			if err != nil || persisted.State != store.ActionReconciled || len(h.pending()) != 0 || sends.Load() != attempts {
				t.Fatalf("moot rename was resent or left pending: action=%+v, sends=%d, %v", persisted, sends.Load(), err)
			}
			// Recreate the pre-delete mission to cover a crash after its journal
			// proof committed but before its control row was removed.
			h.putMission(shipRenameMission, mission.Generation, "rename", data)
			runner = h.runner(primaryTestKey)
			actionLogRefresh(t, h, runner)
			mission, data = h.mission(shipRenameMission)
			if changed, err := runner.dispatch(h.ctx, mission, &data); !changed || err != nil {
				t.Fatalf("restored moot proof did not release its control: changed=%t, %v", changed, err)
			}
			h.missingMission(shipRenameMission)
			if sends.Load() != attempts {
				t.Fatal("restored moot proof replayed the original request")
			}
		})
	}
}

func TestAttemptedShipRenameDoesNotBlockProfitableIdleTrade(t *testing.T) {
	t.Parallel()
	h := newRecoveryHarness(t)
	h.api.setFleet([]map[string]any{
		berthedLaprasRow(),
		sailingShipRow(recoveryShipBlastoise, "Endeavour", recoveryHomePort, recoveryFarPort),
	})
	h.api.setFactorPorts(recoveryFarPort)
	var renameSends atomic.Int64
	actionLogIntercept(t, h, func(writer http.ResponseWriter, request *http.Request) {
		if request.URL.Path != "/v1/fleet/rename-ship" {
			t.Errorf("trade was sent instead of only staged: %s", request.URL.Path)
		} else {
			renameSends.Add(1)
		}
		h.api.writeProblem(writer, http.StatusInternalServerError, "uncertain", "rename outcome unavailable")
	})
	runner := h.runner(primaryTestKey)
	actionLogRefresh(t, h, runner)
	// Fund the executable voyage, including fleet bills, but not another hull.
	idle, _ := runner.ship(recoveryShipLapras)
	input, err := runner.committedPlanningInput(h.ctx, idle)
	if err != nil {
		t.Fatal(err)
	}
	plans := PlanVoyages(input)
	if len(plans) == 0 || !plans[0].Accepted || plans[0].Economics.ExpectedNetPence <= 0 {
		t.Fatalf("idle hull lacks the required profitable lane: %+v", plans)
	}
	for region, required := range cargoFundingRequired(input, plans[0]) {
		h.api.setVaultBalance(region, max(input.ReserveByRegion[region], required))
	}
	actionLogRefresh(t, h, runner)
	if changed, err := runner.work(h.ctx); err != nil || !changed {
		t.Fatalf("initial naming and voyage planning pass made no progress: changed=%t, %v", changed, err)
	}
	_, planned := h.mission(recoveryShipLapras)
	if !planned.Plan.Accepted || planned.Plan.Economics.ExpectedNetPence <= 0 || planned.Plan.Commodity != recoveryGrain {
		t.Fatalf("staging a new rename preempted profitable idle planning: %+v", planned)
	}
	mission, data := h.mission(shipRenameMission)
	if _, err := runner.dispatch(h.ctx, mission, &data); err != nil || renameSends.Load() == 0 {
		t.Fatalf("rename was not attempted before fleet work: sends=%d, %v", renameSends.Load(), err)
	}
	id, attempts := data.Action.ID, renameSends.Load()
	data.CooldownUntil = time.Now().Add(time.Hour)
	if err := runner.saveMission(h.ctx, mission, &data); err != nil {
		t.Fatal(err)
	}
	var trade missionData
	for range 6 {
		if _, err := runner.work(h.ctx); err != nil {
			t.Fatal(err)
		}
		candidate, err := h.db.GetMission(h.ctx, h.account.ID, recoveryShipLapras)
		if err != nil {
			t.Fatal(err)
		}
		if candidate != nil {
			if err := json.Unmarshal(candidate.Data, &trade); err != nil {
				t.Fatal(err)
			}
			if trade.Action != nil {
				break
			}
		}
	}
	if trade.Action == nil || trade.Action.Stage != "buy" || trade.Action.Path != "/v1/market/place-order" ||
		!trade.Plan.Accepted || trade.Plan.Economics.ExpectedNetPence <= 0 || trade.Plan.Commodity != recoveryGrain {
		t.Fatalf("unresolved rename blocked profitable idle trading: action=%+v, trade=%+v, holds=%+v", trade.Action, trade, runner.holding)
	}
	_, retained := h.mission(shipRenameMission)
	if retained.Action == nil || retained.Action.ID != id || renameSends.Load() != attempts || len(h.pending()) != 1 {
		t.Fatalf("fleet work replaced or resent the unresolved rename: sends=%d, %+v", renameSends.Load(), retained)
	}
}
