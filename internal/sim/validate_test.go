package sim

import (
	"encoding/json"
	"testing"
	"time"

	"github.com/DevYukine/go-tradewinds/internal/engine"
	"github.com/DevYukine/go-tradewinds/internal/store"
)

func TestValidateFindsLegacyActionsAtLastSupportedSequence(t *testing.T) {
	at := time.Date(2026, 10, 20, 0, 0, 0, 0, time.UTC)
	buyID := engine.ActionID("account", "mission", "buy", 0)
	sellID := engine.ActionID("account", "mission", "sell", 511)
	tape := &Tape{
		Actions: []store.Action{
			{ID: buyID, AccountID: "account", State: store.ActionComplete, Status: 200, CreatedAt: at, Response: json.RawMessage(`{"filled_quantity":5,"total_gross_pence":500,"customs_duty_pence":10}`)},
			{ID: sellID, AccountID: "account", State: store.ActionComplete, Status: 200, CreatedAt: at.Add(time.Minute), Response: json.RawMessage(`{"filled_quantity":5,"total_gross_pence":1000,"customs_duty_pence":0}`)},
			{ID: "other-action", AccountID: "account", Path: "/v1/treasury/remit", CreatedAt: at},
		},
		Settlements: []store.Settlement{{ID: "mission", AccountID: "account", MissionID: "mission", ActionID: sellID, Kind: "spot", CostsKnown: true, AcquisitionPence: 500, ProceedsPence: 1000, FeesPence: 10, StartedAt: at, CompletedAt: at.Add(time.Minute)}},
	}
	report, err := Validate(tape, at, at.Add(2*time.Minute))
	if err != nil {
		t.Fatal(err)
	}
	if len(report.Settlements) != 1 || report.Settlements[0].Status != "exact" || report.UnlinkedActionsByPath["/v1/treasury/remit"] != 1 {
		t.Fatalf("legacy action recovery or unlinked path count: %+v", report)
	}
	tape.Actions = tape.Actions[1:]
	report, err = Validate(tape, at, at.Add(2*time.Minute))
	if err != nil {
		t.Fatal(err)
	}
	if report.Settlements[0].Status != "unlinkable" {
		t.Fatalf("missing legacy acquisition evidence treated as a measured mismatch: %+v", report.Settlements[0])
	}
}
