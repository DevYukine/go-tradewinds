package engine

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/DevYukine/go-tradewinds/internal/store"
	"github.com/google/uuid"
	"go.uber.org/zap"
	"go.uber.org/zap/zaptest/observer"
)

type statusReviewObservation struct {
	Generation      string        `json:"generation"`
	AssessedShipIDs []string      `json:"assessed_ship_ids"`
	Holds           []currentHold `json:"holds"`
}

func statusReviewCapture(t *testing.T, h *recoveryHarness, runner *Runner) statusReviewObservation {
	t.Helper()
	stream := interceptEventStream(t, h.api, []progressFrame{
		{id: "status-review-connected", eventType: progressOrdinaryEvent},
	}, 2)
	running := startRunner(t, runner, h.ctx)
	stream.waitReady(t, 2)
	snapshot, err := h.db.Snapshot(h.ctx, h.account.ID)
	if err != nil {
		t.Fatalf("load completed controller generation: %v", err)
	}
	running.cancel()
	if err := running.wait(t, 15*time.Second); err != nil && !errors.Is(err, context.Canceled) {
		t.Fatalf("stop status runner: %v", err)
	}
	var observation statusReviewObservation
	if err := json.Unmarshal(snapshot["_engine"], &observation); err != nil {
		t.Fatalf("decode persisted controller observation: %v", err)
	}
	if observation.AssessedShipIDs == nil {
		t.Fatal("a completed generation must publish explicit ship assessment coverage, including an empty list")
	}
	meta := decode[map[string]DatasetMeta](snapshot, "_meta")["_engine"]
	if !meta.Complete || meta.Generation == "" || meta.Generation != observation.Generation {
		t.Fatalf("controller publication lacks matching completed-generation metadata: %+v %+v", observation, meta)
	}
	return observation
}

func TestControllerCoverageStopsAtTheActualShipOrCompanyAction(t *testing.T) {
	for _, scenario := range []string{"first ship recovery", "treasury settings", "new hull commissioning"} {
		t.Run(scenario, func(t *testing.T) {
			h := newRecoveryHarness(t)
			runner := h.runner(primaryTestKey)
			want := []string{recoveryShipBlastoise}
			switch scenario {
			case "treasury settings":
				h.api.setAutoRemit(false, 0)
				want = []string{}
			case "new hull commissioning":
				built := destinationShipRow(recoveryShipBlastoise, "Blastoise", recoveryHomePort, "berthed", 5, nil, nil)
				built["ship"].(map[string]any)["cannons_count"] = 2
				built["effective_hold_capacity_tons"] = 51
				h.api.setFleet([]map[string]any{
					built,
					sailingShipRow(recoveryShipCharizard, "Charizard", recoveryFarPort, recoveryHomePort),
				})
				h.api.setFactorPorts(recoveryFarPort)
				h.putMission("__growth", "status-review-commission", "growth_refit", missionData{
					Plan: Plan{
						Kind: "spot", ShipID: recoveryShipBlastoise,
						Origin: recoveryHomePort, Destination: recoveryFarPort,
						Commodity: recoveryGrain, Quantity: 24, BuyLimitPence: 100,
						TargetStoresTons: 2,
					},
					GrowthLane: &growthLaneIntent{
						AdmissionID: "status-review-commission", QuantityLimit: 24,
						InitialProcurementLimitPence: 3_000,
					},
					BuiltShipID: recoveryShipBlastoise, BuildPort: recoveryHomePort,
					TargetCannons: 4, CostsKnown: true,
				})
			}
			// Coverage from any earlier generation cannot survive this capture.
			runner.assessedShipIDs = []string{recoveryShipBlastoise, recoveryShipCharizard}
			runner.holding = []currentHold{{ShipID: recoveryShipCharizard, Reason: "previous generation blocker"}}
			observation := statusReviewCapture(t, h, runner)
			if !slices.Equal(observation.AssessedShipIDs, want) {
				t.Fatalf("%s coverage = %v, want only evaluated ships %v", scenario, observation.AssessedShipIDs, want)
			}
			for _, hold := range observation.Holds {
				if hold.ShipID == recoveryShipCharizard || hold.Reason == "previous generation blocker" {
					t.Fatalf("unvisited ship inherited a current assessment: %+v", observation)
				}
			}
			switch scenario {
			case "first ship recovery":
				mission, _ := h.mission(recoveryShipBlastoise)
				if mission.Phase != "sailing" {
					t.Fatalf("first ship did not consume the generation with recovery: %+v", mission)
				}
				h.missingMission(recoveryShipCharizard)
			case "treasury settings":
				_, data := h.mission("__treasury")
				if data.Action == nil || data.Action.Stage != "settings" {
					t.Fatalf("company generation did not stage treasury settings: %+v", data.Action)
				}
			case "new hull commissioning":
				_, data := h.mission("__growth")
				if data.Action == nil || data.Action.Stage != "refit" {
					t.Fatalf("commissioning generation did not stage its refit: %+v", data.Action)
				}
			}
		})
	}
}

func TestAssessmentCoverageRequiresActualFleetMembership(t *testing.T) {
	runner := New(nil, nil, store.Account{CompanyID: recoveryCompanyID}, nil, Options{})
	runner.stateCache.Ships = []ShipState{operationShip(recoveryShipLapras, "Lapras", "berthed", recoveryHomePort)}
	for _, id := range []string{"", "__treasury", "__growth", "__underwriting", recoveryCompanyID, recoveryShipCharizard} {
		runner.assessShip(id)
	}
	if len(runner.assessedShipIDs) != 0 {
		t.Fatalf("company controls or absent fleet members became assessed ships: %v", runner.assessedShipIDs)
	}
	runner.assessShip(recoveryShipLapras)
	runner.assessShip(recoveryShipLapras)
	if !slices.Equal(runner.assessedShipIDs, []string{recoveryShipLapras}) {
		t.Fatalf("real ship assessment was missing or duplicated: %v", runner.assessedShipIDs)
	}
}

func installStatusSourcingWorld(t *testing.T, h *recoveryHarness) {
	t.Helper()
	h.api.setFleet([]map[string]any{
		destinationShipRow(recoveryShipBlastoise, "Blastoise", recoveryHomePort, "berthed", 5, nil, nil),
	})
	charter := sourcingReviewCharter("status-review-charter")
	downstream := h.api.server.Config.Handler
	h.api.server.Config.Handler = http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		if request.Method != http.MethodGet {
			downstream.ServeHTTP(writer, request)
			return
		}
		switch request.URL.Path {
		case "/v1/world/catalog":
			catalog := h.api.catalogDocument()
			catalog["ports"] = append(catalog["ports"].([]any), map[string]any{
				"id": "amsterdam", "name": "Amsterdam", "banking_region": "north",
				"sea_zone_id": "zone-north", "phase": 1, "effective_berths": 10,
				"manufactures": []string{"sawn_planks"},
			})
			h.api.writeJSON(writer, http.StatusOK, catalog)
		case "/v1/contract/list":
			rows := []Contract{}
			if request.URL.Query().Get("status") == "open" {
				rows = append(rows, charter)
			}
			h.api.writeJSON(writer, http.StatusOK, rows)
		case "/v1/market/book":
			book := h.api.bookDocument(request.URL.Query().Get("port_id"))
			books := book["books"].([]any)
			book["books"] = books[1:]
			h.api.writeJSON(writer, http.StatusOK, book)
		default:
			downstream.ServeHTTP(writer, request)
		}
	})
}

func TestSourcingRejectionIsPublishedAsTheCurrentAttributedHold(t *testing.T) {
	for _, scenario := range []string{"exhausted allowance", "underfunded inspection", "underfunded with safety hold"} {
		t.Run(scenario, func(t *testing.T) {
			h := newRecoveryHarness(t)
			installStatusSourcingWorld(t, h)
			runner := h.runner(primaryTestKey)
			runner.opts.Execute = false
			runner.generation = uuid.NewString()
			if err := runner.refresh(h.ctx); err != nil {
				t.Fatalf("refresh legal sourcing control: %v", err)
			}
			ship, exists := runner.ship(recoveryShipBlastoise)
			if !exists {
				t.Fatal("source probe ship missing from control fleet")
			}
			input, err := runner.sourcingInput(h.ctx, ship)
			if err != nil {
				t.Fatalf("load legal sourcing control: %v", err)
			}
			if plans := planSourcingRelocation(input); len(acceptedRelocations(plans)) == 0 {
				t.Fatalf("control must admit a legal funded inspection before introducing its blocker: %+v", plans)
			}
			want := "cannot be funded with observed remittance fees"
			if scenario == "exhausted allowance" {
				expense := int64(20_000)
				putSourcingBudgetSettlement(t, h, store.Settlement{
					ID: "status-review-exhausted-probe", Kind: "relocation", CostsKnown: true,
					ProvisionsPence: expense, ExplorationExpensePence: &expense,
				})
				want = "exceeds allowance/cap 0p"
			} else {
				h.api.setVaultBalance("north", 0)
				h.api.setVaultBalance("south", 0)
				runner.opts.Execute = scenario == "underfunded with safety hold"
				if runner.opts.Execute {
					idle := destinationShipRow(recoveryShipBlastoise, "Blastoise", recoveryHomePort, "berthed", 5, nil, nil)
					idle["ship"].(map[string]any)["cannons_count"] = 2
					idle["effective_hold_capacity_tons"] = 56
					h.api.setFleet([]map[string]any{idle})
				}
			}
			observation := statusReviewCapture(t, h, runner)
			if !slices.Equal(observation.AssessedShipIDs, []string{recoveryShipBlastoise}) {
				t.Fatalf("source review lost current ship coverage: %+v", observation)
			}
			var shipHolds []string
			for _, hold := range observation.Holds {
				if hold.ShipID == recoveryShipBlastoise {
					shipHolds = append(shipHolds, hold.Reason)
				}
			}
			found := false
			for _, reason := range shipHolds {
				found = found || strings.Contains(reason, want)
				if strings.Contains(reason, "no profitable executable voyage") || strings.Contains(reason, "no executable purchase-and-voyage alternative") {
					t.Fatalf("ordinary trade rejection obscured the actual sourcing blocker: %v", shipHolds)
				}
			}
			if !found {
				t.Fatalf("current attributed hold lacks %q: %+v", want, observation.Holds)
			}
			if scenario == "underfunded with safety hold" && (len(shipHolds) < 2 || !strings.Contains(shipHolds[0], "armed maintenance deferred")) {
				t.Fatalf("source recommendation displaced the more severe ship safety hold: %v", shipHolds)
			}
		})
	}
}

func TestConcurrentRequestJournalFailuresAreBoundedByCauseAndRecover(t *testing.T) {
	h := newRecoveryHarness(t)
	runner := h.runner(primaryTestKey)
	core, logs := observer.New(zap.DebugLevel)
	runner.log = zap.New(core)
	admin, err := sql.Open("pgx", h.dsn)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := admin.Close(); err != nil {
			t.Error(err)
		}
	})
	exec := func(statement string) {
		t.Helper()
		if _, err := admin.ExecContext(h.ctx, statement); err != nil {
			t.Fatalf("change isolated request journal availability: %v", err)
		}
	}
	failConcurrentReads := func() {
		t.Helper()
		var callbacks sync.WaitGroup
		for index := range 32 {
			callbacks.Add(1)
			go func() {
				defer callbacks.Done()
				path := fmt.Sprintf("/v1/fleet/ship?ship_id=%d&api_key=%s", index, primaryTestKey)
				runner.observeRequest(h.ctx, http.MethodGet, path, http.StatusOK, time.Millisecond, nil, []byte(`{}`), "")
			}()
		}
		callbacks.Wait()
	}
	exec("ALTER TABLE requests RENAME TO status_review_unavailable_requests")
	failConcurrentReads()
	if warnings := logs.FilterMessage("record request").All(); len(warnings) != 1 || warnings[0].Level != zap.WarnLevel {
		t.Fatalf("one concurrent storage outage must warn once, not once per GET: %+v", warnings)
	}
	exec("ALTER TABLE status_review_unavailable_requests RENAME TO requests")
	exec("ALTER TABLE requests ADD CONSTRAINT status_review_journal_rejected CHECK (false) NOT VALID")
	failConcurrentReads()
	warnings := logs.FilterMessage("record request").All()
	if len(warnings) != 2 {
		t.Fatalf("a distinct persistence cause must remain visible within the outage: %+v", warnings)
	}
	for _, warning := range warnings {
		fields := warning.ContextMap()
		if strings.Contains(fmt.Sprint(fields), primaryTestKey) {
			t.Fatalf("request journal diagnostic leaked a credential: %v", fields)
		}
		if fields["method"] != http.MethodGet || fields["cause"] == "" || fields["path"] == "" {
			t.Fatalf("request journal warning lost useful safe context: %v", fields)
		}
	}
	if logs.FilterMessage("request journal persistence recovered").Len() != 0 {
		t.Fatal("failed requests must not announce persistence recovery")
	}
	exec("ALTER TABLE requests DROP CONSTRAINT status_review_journal_rejected")
	for range 2 {
		runner.observeRequest(h.ctx, http.MethodPost, "/v1/company/settings", http.StatusOK, time.Millisecond, nil, []byte(`{}`), "")
	}
	recoveries := logs.FilterMessage("request journal persistence recovered").All()
	if len(recoveries) != 1 || recoveries[0].Level != zap.InfoLevel || recoveries[0].ContextMap()["failed_requests"] != int64(64) {
		t.Fatalf("first successful persistence must announce the outage recovery once: %+v", recoveries)
	}
	if runner.lastMutationStatus.Load() != http.StatusOK {
		t.Fatal("journal diagnostics interfered with mutation outcome observation")
	}
	exec("ALTER TABLE requests ADD CONSTRAINT status_review_journal_rejected CHECK (false) NOT VALID")
	failConcurrentReads()
	if warnings := logs.FilterMessage("record request").All(); len(warnings) != 3 {
		t.Fatalf("a new outage after recovery must receive its own warning: %+v", warnings)
	}
	exec("ALTER TABLE requests DROP CONSTRAINT status_review_journal_rejected")
}

func TestRequestJournalOutageDoesNotSampleDistinctCommittedActions(t *testing.T) {
	h := newRecoveryHarness(t)
	runner := h.runner(primaryTestKey)
	core, logs := observer.New(zap.DebugLevel)
	runner.log = zap.New(core)
	runner.api.SetObserver(func(method, path string, status int, duration time.Duration, request, response []byte, errText string) {
		runner.observeRequest(h.ctx, method, path, status, duration, request, response, errText)
	})
	admin, err := sql.Open("pgx", h.dsn)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := admin.Close(); err != nil {
			t.Error(err)
		}
	})
	if _, err := admin.ExecContext(h.ctx, "ALTER TABLE requests ADD CONSTRAINT status_review_journal_outage CHECK (false) NOT VALID"); err != nil {
		t.Fatal(err)
	}
	for index := range 2 {
		mission, data := stageLoggingSettings(t, h, runner, fmt.Sprintf("status-review-business-%d", index))
		if changed, err := runner.dispatch(h.ctx, mission, &data); err != nil || !changed {
			t.Fatalf("commit independent business action during diagnostic outage: changed %t error %v", changed, err)
		}
	}
	committed := logs.FilterMessage("action committed").All()
	if len(committed) != 2 || committed[0].Level != zap.InfoLevel || committed[1].Level != zap.InfoLevel ||
		committed[0].ContextMap()["action_id"] == committed[1].ContextMap()["action_id"] {
		t.Fatalf("request-journal suppression sampled distinct business outcomes: %+v", committed)
	}
	if warnings := logs.FilterMessage("record request").All(); len(warnings) != 1 {
		t.Fatalf("business action callbacks restarted the same diagnostic warning: %+v", warnings)
	}
}
