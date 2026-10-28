package sim

import (
	"bytes"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/DevYukine/go-tradewinds/internal/engine"
	"github.com/DevYukine/go-tradewinds/internal/store"
)

func simAcceptanceValidationTape(t *testing.T) (*Tape, time.Time, time.Time) {
	t.Helper()
	from := time.Date(2026, 10, 20, 0, 0, 0, 0, time.UTC)
	to := from.Add(time.Hour)
	row := store.Settlement{ID: "settled", MissionID: "mission", Kind: "spot", CostsKnown: true, StartedAt: from, CompletedAt: from.Add(10 * time.Minute), AcquisitionPence: 2700, ProceedsPence: 9000, FeesPence: 78, ProvisionsPence: 205, ExpensesPence: 300}
	tape := &Tape{Settlements: []store.Settlement{row}, AccountingSnapshot: map[string]json.RawMessage{
		"_expense_allocations": json.RawMessage(`{"berth-1":"mission","berth-2":"mission"}`),
		"ledger":               json.RawMessage(`[{"id":"berth-1","debit_account_id":"vault-a","credit_account_id":"npc","amount_pence":100,"reason":"berth"},{"id":"berth-2","debit_account_id":"vault-a","credit_account_id":"npc","amount_pence":200,"reason":"berth"}]`),
		"vaults":               json.RawMessage(`{"vaults":[{"account_id":"vault-a","account_type":"vault","banking_region":"north","balance_pence":100000}]}`),
	}}
	for i, receipt := range []struct{ stage, path, body, response string }{
		{"buy", "/v1/market/place-order", `{"side":"bid"}`, `{"filled_quantity":30,"total_gross_pence":2700,"customs_duty_pence":68}`},
		{"provision", "/v1/fleet/provision", `{}`, `{"filled_quantity":2,"total_gross_pence":200,"customs_duty_pence":5}`},
		{"depart", "/v1/fleet/depart", `{}`, `{"ok":true}`},
		{"sell", "/v1/market/place-order", `{"side":"ask"}`, `{"filled_quantity":30,"total_gross_pence":9000,"customs_duty_pence":0}`},
		{"remit", "/v1/treasury/remit", `{}`, `{"gross_pence":1000,"net_pence":990,"fee_pence":10}`},
	} {
		id := "action-" + receipt.stage
		tape.Actions = append(tape.Actions, store.Action{ID: id, Path: receipt.path, Method: "POST", Body: json.RawMessage(receipt.body), Response: json.RawMessage(receipt.response), Status: 200, State: store.ActionComplete, CreatedAt: from.Add(time.Duration(i+1) * time.Minute)})
		tape.Links = append(tape.Links, store.ReplayActionLink{ActionID: id, MissionID: "mission", Stage: receipt.stage})
	}
	tape.Settlements[0].ActionID = "action-sell"
	return tape, from, to
}

func TestValidateAcceptanceLinkedReceiptFixtureAndFieldDeltas(t *testing.T) {
	tape, from, to := simAcceptanceValidationTape(t)
	report, err := Validate(tape, from, to)
	if err != nil {
		t.Fatal(err)
	}
	if len(report.Settlements) != 1 || report.Settlements[0].Status != "exact" || report.Settlements[0].Deltas != (Money{}) {
		t.Fatalf("five-field linked fixture failed exact reconstruction: %+v", report.Settlements)
	}
	tape.Settlements[0].FeesPence++
	report, err = Validate(tape, from, to)
	if err != nil {
		t.Fatal(err)
	}
	if len(report.Settlements) != 1 || report.Settlements[0].Status != "mismatch" || report.Settlements[0].Deltas.FeesPence == 0 || report.Settlements[0].Deltas.AcquisitionPence != 0 || report.Settlements[0].Deltas.ProceedsPence != 0 {
		t.Fatalf("one-pence customs error did not identify the fees field: %+v", report.Settlements)
	}
}

func TestValidateAcceptanceMissingReceiptIsUnlinkable(t *testing.T) {
	tape, from, to := simAcceptanceValidationTape(t)
	tape.Actions = tape.Actions[1:]
	report, err := Validate(tape, from, to)
	if err != nil {
		t.Fatal(err)
	}
	if len(report.Settlements) != 1 || report.Settlements[0].Status != "unlinkable" || report.Trusted {
		t.Fatalf("missing buy receipt was treated as a genuine accounting difference: %+v", report)
	}
}

func TestValidateAcceptanceExactAccountingCannotBypassModelSampleGates(t *testing.T) {
	tape, from, to := simAcceptanceValidationTape(t)
	report, err := Validate(tape, from, to)
	if err != nil {
		t.Fatal(err)
	}
	if report.Trusted || len(report.FailedRules) < 2 {
		t.Fatalf("one exact settlement bypassed minimum covered orders and held-out voyages: %+v", report)
	}
}

func simAcceptanceGateReport() ValidationReport {
	var report ValidationReport
	for i := range 20 {
		status := "exact"
		if i == 19 {
			status = "mismatch"
		}
		report.Settlements = append(report.Settlements, SettlementCheck{Kind: "spot", Status: status, CostsKnown: true})
	}
	for i := range 10 {
		quantity := int64(10)
		if i == 9 {
			quantity = 9
		}
		report.Orders = append(report.Orders, OrderCheck{ActualQuantity: 10, PredictedQuantity: quantity, ActualGrossPence: 100, PredictedGrossPence: 101})
		errorFraction := 0.05
		if i >= 6 {
			errorFraction = 0.06
		}
		report.Voyages = append(report.Voyages, VoyageCheck{OutOfSample: true, ActualGameHours: 100, PredictedGameHours: 100 * (1 + errorFraction), AbsoluteError: errorFraction})
	}
	return report
}

func TestValidateAcceptanceGateThresholdsAreInclusiveAndIndependent(t *testing.T) {
	boundary := simAcceptanceGateReport()
	boundary.EvaluateGate()
	if !boundary.Trusted || len(boundary.FailedRules) != 0 {
		t.Fatalf("95%% exact, 90%% exact tons, 1%% gross and 5%% median should pass inclusively: %+v", boundary)
	}
	for _, tc := range []struct {
		name   string
		change func(*ValidationReport)
	}{
		{name: "linkable below eighty percent", change: func(r *ValidationReport) {
			for range 6 {
				r.Settlements = append(r.Settlements, SettlementCheck{Kind: "relocation", Status: "unlinkable", CostsKnown: true})
			}
		}},
		{name: "known cost exact below ninety five percent", change: func(r *ValidationReport) { r.Settlements[0].Status = "mismatch" }},
		{name: "fewer than ten covered orders", change: func(r *ValidationReport) { r.Orders = r.Orders[:9] }},
		{name: "quantity accuracy below ninety percent", change: func(r *ValidationReport) { r.Orders[0].PredictedQuantity = 9 }},
		{name: "gross accuracy below ninety percent", change: func(r *ValidationReport) {
			r.Orders[0].PredictedGrossPence = 102
			r.Orders[1].PredictedGrossPence = 102
		}},
		{name: "fewer than ten held out voyages", change: func(r *ValidationReport) { r.Voyages[0].OutOfSample = false }},
		{name: "duration median above five percent", change: func(r *ValidationReport) {
			for i := range 6 {
				r.Voyages[i].AbsoluteError = 0.06
				r.Voyages[i].PredictedGameHours = 106
			}
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			report := simAcceptanceGateReport()
			tc.change(&report)
			report.EvaluateGate()
			if report.Trusted || len(report.FailedRules) == 0 {
				t.Fatalf("gate accepted independently failed rule: %+v", report)
			}
		})
	}
}

func TestValidateAcceptanceFreightDoesNotDiluteSpotCharterRelocationGate(t *testing.T) {
	report := simAcceptanceGateReport()
	for range 100 {
		report.Settlements = append(report.Settlements, SettlementCheck{Kind: "freight", Status: "unlinkable"})
	}
	report.EvaluateGate()
	if !report.Trusted {
		t.Fatalf("separately reported freight changed economic gate denominator: %+v", report.FailedRules)
	}
}

func TestValidateAcceptanceEightyPercentLinkableBoundary(t *testing.T) {
	report := simAcceptanceGateReport()
	for range 5 {
		report.Settlements = append(report.Settlements, SettlementCheck{Kind: "charter", Status: "unlinkable", CostsKnown: true})
	}
	report.EvaluateGate()
	if !report.Trusted {
		t.Fatalf("exactly 80%% linkable must satisfy inclusive threshold: %+v", report.FailedRules)
	}
}

func TestValidateAcceptanceUnlinkedActionsCountByPathWithoutSecrets(t *testing.T) {
	tape, from, to := simAcceptanceValidationTape(t)
	tape.Actions = append(tape.Actions, store.Action{
		ID: "unlinked", Method: "POST", Path: "/v1/company/settings", Status: 200, State: store.ActionComplete,
		Body: json.RawMessage(`{"api_key":"acceptance-private-value"}`), Response: json.RawMessage(`{"ciphertext":"acceptance-private-value"}`), CreatedAt: from.Add(time.Minute),
	})
	report, err := Validate(tape, from, to)
	if err != nil {
		t.Fatal(err)
	}
	if report.UnlinkedActionsByPath["/v1/company/settings"] != 1 {
		t.Fatalf("unlinked action was omitted from path accounting: %+v", report.UnlinkedActionsByPath)
	}
	encoded, err := json.Marshal(report)
	if err != nil {
		t.Fatal(err)
	}
	if bytes.Contains(encoded, []byte("acceptance-private-value")) {
		t.Fatal("validation output exposed raw receipt/body secret material")
	}
}

func TestValidateAcceptanceLegacyActionIDFallbackReconstructsReceiptFields(t *testing.T) {
	tape, from, to := simAcceptanceValidationTape(t)
	tape.Settlements[0].AccountID = "ours"
	for i, link := range tape.Links {
		tape.Actions[i].ID = engine.ActionID("ours", "mission", link.Stage, int64(i))
		tape.Actions[i].AccountID = "ours"
		if link.Stage == "sell" {
			tape.Settlements[0].ActionID = tape.Actions[i].ID
		}
	}
	tape.Links = nil
	report, err := Validate(tape, from, to)
	if err != nil {
		t.Fatal(err)
	}
	if len(report.Settlements) != 1 || report.Settlements[0].Status != "exact" {
		t.Fatalf("legacy generation/stage/sequence action ID discovery lost receipts: %+v", report.Settlements)
	}
}

func TestValidateRequiresRecordedLloydsQuotesInEveryWindow(t *testing.T) {
	for _, mode := range []string{"recorded", "missing", "empty", "future", "later window only"} {
		t.Run(mode, func(t *testing.T) {
			tape, from, to := simAcceptanceRunTape(t)
			for index := range tape.Frames {
				dataset := tape.Frames[index].Datasets["_hazard_quotes"]
				switch mode {
				case "missing":
					delete(tape.Frames[index].Datasets, "_hazard_quotes")
					continue
				case "empty":
					dataset.Data = json.RawMessage(`{}`)
				case "future":
					var quotes map[string]engine.HazardQuote
					if err := json.Unmarshal(dataset.Data, &quotes); err != nil {
						t.Fatal(err)
					}
					for key, quote := range quotes {
						quote.ObservedAt = to.Add(time.Hour)
						quotes[key] = quote
					}
					dataset.Data, _ = json.Marshal(quotes)
				case "later window only":
					if index == 0 {
						delete(tape.Frames[index].Datasets, "_hazard_quotes")
						continue
					}
				}
				tape.Frames[index].Datasets["_hazard_quotes"] = dataset
			}
			report, err := Validate(tape, from, to)
			if err != nil {
				t.Fatal(err)
			}
			missing := strings.Contains(strings.Join(report.FailedRules, ";"), "no recorded Lloyd's quotes")
			if missing != (mode != "recorded") || missing && report.Trusted {
				t.Fatalf("hazard evidence gate for %s: %+v", mode, report)
			}
			if missing {
				var text bytes.Buffer
				if _, err := report.WriteTo(&text); err != nil ||
					!strings.Contains(text.String(), "UNTRUSTED") ||
					!strings.Contains(text.String(), "no recorded Lloyd's quotes") {
					t.Fatalf("missing hazard evidence was hidden: %s, %v", text.String(), err)
				}
			}
		})
	}
}
