package engine

import (
	"bytes"
	"encoding/json"
	"net/http"
	"sync"
	"testing"

	"github.com/DevYukine/go-tradewinds/internal/gameapi"
	"github.com/DevYukine/go-tradewinds/internal/store"
)

func loanBridgeNoMarker(t *testing.T, world *loanBridgeWorld, runner *Runner, loan gameapi.Loan) {
	t.Helper()
	key := loanRefinanceDueKey(loan)
	if _, exists := decode[map[string]json.RawMessage](runner.snapshot, "_loan_bridge_due")[key]; exists {
		t.Fatalf("proven never-sent bridge retained its in-memory due-date marker for %s", key)
	}
	snapshot, err := world.h.db.Snapshot(world.h.ctx, world.h.account.ID)
	if err != nil {
		t.Fatal(err)
	}
	if _, exists := decode[map[string]json.RawMessage](snapshot, "_loan_bridge_due")[key]; exists {
		t.Fatalf("proven never-sent bridge retained its durable due-date marker for %s", key)
	}
}

func TestLoanBridgeQuoteFenceLeavesNoMissionOrDueMarker(t *testing.T) {
	for _, fence := range []string{"financial", "control"} {
		t.Run(fence, func(t *testing.T) {
			world, runner := loanBridgeFixture(t, 100_000)
			before := runner.stateCache.Loans[0]
			base := world.h.api.server.Config.Handler
			var publish sync.Once
			world.h.api.server.Config.Handler = http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
				if request.Method == http.MethodGet && request.URL.Path == "/v1/loan/quote" {
					publish.Do(func() {
						if fence == "financial" {
							runner.publishedFinancial.Add(1)
						} else {
							runner.publishedControl.Add(1)
						}
					})
				}
				base.ServeHTTP(writer, request)
			})
			if changed, err := runner.serviceLoans(world.h.ctx); err != nil || changed {
				t.Fatalf("quote-time %s fence reported a staged bridge: changed=%v err=%v", fence, changed, err)
			}
			world.h.missingMission("__loans")
			loanBridgeNoMarker(t, world, runner, before)
			if len(world.requests()) == 0 || world.h.api.mutationCount("/v1/loan/accept-quote") != 0 || len(world.h.pending()) != 0 {
				t.Fatal("quote-time fence did not stop before creating or sending the bridge action")
			}
			if fence == "financial" && runner.publishedFinancial.Load() == runner.capturedFinancial ||
				fence == "control" && runner.publishedControl.Load() == runner.capturedControl {
				t.Fatal("quote hook did not publish the intended uncaptured event")
			}
			// No action ever existed, so a new authoritative capture may quote
			// this same installment again rather than losing the date forever.
			runner.capturedFinancial = runner.publishedFinancial.Load()
			runner.capturedControl = runner.publishedControl.Load()
			loanCapacityRefresh(t, world.h, runner)
			mission, data := loanBridgeStage(t, world, runner, before.ID, 100_000, recoveryShipCharizard)
			id := data.Action.ID
			if changed, err := runner.dispatch(world.h.ctx, mission, &data); err != nil || !changed {
				t.Fatalf("fresh capture did not dispatch the unstaged installment: changed=%v err=%v", changed, err)
			}
			journal := loanBridgeJournal(t, world, id)
			if journal.State != store.ActionComplete || journal.Status != http.StatusOK ||
				world.h.api.mutationCount("/v1/loan/accept-quote") != 1 || len(world.h.pending()) != 0 {
				t.Fatalf("fresh capture did not settle exactly one bridge: %+v", journal)
			}
			loanBridgeMarker(t, world, before)
		})
	}
}

func TestLoanBridgeStagedUnsentRestartRestampsSameAction(t *testing.T) {
	loanBridgeResumeUnsent(t, "restart")
}

func TestLoanBridgeUnrelatedFencePreservesUnsentIntent(t *testing.T) {
	for _, fence := range []string{"financial", "control"} {
		t.Run(fence, func(t *testing.T) {
			loanBridgeResumeUnsent(t, fence)
		})
	}
}

func loanBridgeResumeUnsent(t *testing.T, fence string) {
	t.Helper()
	world, runner := loanBridgeFixture(t, 100_000)
	mission, staged := loanBridgeStage(t, world, runner, "bridge-target", 100_000, recoveryShipCharizard)
	generation, sequence := mission.Generation, staged.Sequence
	before := staged.LoanBridge.Before
	intent := encode(staged.LoanBridge)
	action := *staged.Action
	quotes := len(world.requests())
	switch fence {
	case "restart":
		runner = world.h.runner(primaryTestKey)
	case "financial":
		runner.publishedFinancial.Add(1)
		runner.capturedFinancial = runner.publishedFinancial.Load()
	case "control":
		runner.publishedControl.Add(1)
		runner.capturedControl = runner.publishedControl.Load()
	}
	loanCapacityRefresh(t, world.h, runner)
	mission, data := world.h.mission("__loans")
	if runner.stagedFundsValid(data.Action) && runner.stagedControlValid(data.Action) {
		t.Fatalf("%s fixture did not invalidate the staged action's capture", fence)
	}
	if changed, err := runner.dispatch(world.h.ctx, mission, &data); err != nil || !changed {
		t.Fatalf("%s did not restamp the proven unsent bridge: changed=%v err=%v", fence, changed, err)
	}
	mission, data = world.h.mission("__loans")
	if mission.Phase != "loan_bridge" || mission.Generation != generation || data.Sequence != sequence ||
		data.Action == nil || data.Capacity != nil || !bytes.Equal(encode(data.LoanBridge), intent) {
		t.Fatalf("%s lost or replaced the durable installment intent: mission=%+v data=%+v", fence, mission, data)
	}
	if !runner.stagedFundsValid(data.Action) || !runner.stagedControlValid(data.Action) {
		t.Fatalf("%s did not stamp the current authoritative capture", fence)
	}
	// Only the capture stamps may change; the admitted request, quote and
	// original creation time remain immutable despite the process/event fence.
	restamped := *data.Action
	restamped.FinancialSession = action.FinancialSession
	restamped.FinancialEpoch = action.FinancialEpoch
	restamped.ControlEpoch = action.ControlEpoch
	if !bytes.Equal(encode(restamped), encode(action)) {
		t.Fatalf("%s changed the immutable bridge action while restamping", fence)
	}
	if exists, err := world.h.db.HasAction(world.h.ctx, world.h.account.ID, action.ID); err != nil || exists {
		t.Fatalf("%s created a journal row while only restamping: exists=%v err=%v", fence, exists, err)
	}
	if len(world.requests()) != quotes || world.h.api.mutationCount("/v1/loan/accept-quote") != 0 || len(world.h.pending()) != 0 {
		t.Fatalf("%s requoted or sent before restamping completed", fence)
	}
	loanBridgeMarker(t, world, before)
	if changed, err := runner.serviceLoans(world.h.ctx); err != nil || changed {
		t.Fatalf("restamped intent was replaced by loan service: changed=%v err=%v", changed, err)
	}
	if changed, err := runner.dispatch(world.h.ctx, mission, &data); err != nil || !changed {
		t.Fatalf("%s did not dispatch the restamped bridge: changed=%v err=%v", fence, changed, err)
	}
	journal := loanBridgeJournal(t, world, action.ID)
	exact, err := world.h.db.BeginAction(world.h.ctx, &store.Action{
		ID: action.ID, AccountID: world.h.account.ID, Method: action.Method, Path: action.Path, Body: action.Body,
	})
	if err != nil {
		t.Fatalf("%s changed the exact persisted journal request: %v", fence, err)
	}
	if journal.State != store.ActionComplete || journal.Status != http.StatusOK || journal.Method != action.Method ||
		journal.Path != action.Path || !bytes.Equal(exact.Body, action.Body) || len(world.h.pending()) != 0 {
		t.Fatalf("%s did not settle the original exact journal action: %+v", fence, journal)
	}
	mission, data = world.h.mission("__loans")
	if mission.Phase != "done" || data.Action != nil || data.LastActionID != action.ID {
		t.Fatalf("%s did not complete the preserved bridge mission: %+v", fence, data)
	}
	for range 2 {
		runner = world.h.runner(primaryTestKey)
		loanCapacityRefresh(t, world.h, runner)
		if _, err := runner.serviceLoans(world.h.ctx); err != nil {
			t.Fatal(err)
		}
	}
	if len(world.requests()) != quotes || world.h.api.mutationCount("/v1/loan/accept-quote") != 1 || len(world.h.pending()) != 0 {
		t.Fatalf("%s did not retain at-most-one accept for the original loan/date", fence)
	}
	loanBridgeMarker(t, world, before)
}
