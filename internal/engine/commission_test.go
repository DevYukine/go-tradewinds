package engine

import (
	"encoding/json"
	"io"
	"net/http"
	"testing"
	"time"

	"github.com/DevYukine/go-tradewinds/internal/gameapi"
)

func commissionTestTerms(now time.Time) (gameapi.CommissionListing, Port, YardQuote) {
	row := gameapi.CommissionListing{Contract: gameapi.Contract{ID: "admiralty-order", ContractType: "commission", OriginPortID: "boston",
		Status: "open", RewardPence: 12_000_000, CollateralPence: 1_200_000, ExpiresAt: new(now.Add(24 * time.Hour)),
		Metadata: json.RawMessage(`{"admiralty":true,"ship_class":"frigate","rebuild_value_pence":11000000}`)}, ShipClass: new("frigate")}
	port := Port{ID: "boston", BankingRegion: "america", Phase: 2, HasShipyard: true}
	quote := YardQuote{PortID: "boston", ShipClass: "frigate", BuildMode: "turnkey_cash", Available: true, TotalCostPence: new(int64(10_000_000))}
	return row, port, quote
}

func TestCommissionRequiresMarginOverAllCosts(t *testing.T) {
	now := time.Date(2026, 11, 2, 0, 0, 0, 0, time.UTC)
	row, port, quote := commissionTestTerms(now)
	accepted := evaluateCommission(row, port, quote, 2, now, 60, 30_000_000, 0)
	if !accepted.Accepted || accepted.CarryPence <= 0 || accepted.DeadlineRiskPence <= 0 || accepted.NetPence <= accepted.MarginPence {
		t.Fatalf("profitable commission was not admitted with explicit carry/risk: %+v", accepted)
	}
	// Hold the deposit fixed and put price exactly on the five-percent boundary.
	row.RewardPence = accepted.BuildPence + accepted.CarryPence + accepted.DeadlineRiskPence + accepted.MarginPence
	if got := evaluateCommission(row, port, quote, 2, now, 60, 30_000_000, 0); got.Accepted || got.NetPence != got.MarginPence {
		t.Fatalf("non-strict margin boundary spent cash: %+v", got)
	}
	row.RewardPence++
	if got := evaluateCommission(row, port, quote, 2, now, 60, 30_000_000, 0); !got.Accepted {
		t.Fatalf("one penny above the margin was refused: %+v", got)
	}
	if got := evaluateCommission(row, port, quote, 2, now, 60, *quote.TotalCostPence+row.CollateralPence-1, 0); got.Accepted {
		t.Fatal("protected build/deposit cash was spent")
	}
}

func TestCommissionRefusesEuropeAndInfeasibleDeadline(t *testing.T) {
	now := time.Now().UTC()
	row, port, quote := commissionTestTerms(now)
	port.Phase = 1
	if got := evaluateCommission(row, port, quote, 2, now, 60, 30_000_000, 0); got.Accepted {
		t.Fatal("Phase 2 commission built at a European yard")
	}
	port.Phase = 2
	row.ExpiresAt = new(now.Add(6 * time.Minute))
	if got := evaluateCommission(row, port, quote, 2, now, 60, 30_000_000, 0); got.Accepted {
		t.Fatal("deadline exactly at the six-game-hour allowance was accepted")
	}
	row.ExpiresAt = new(now.Add(-time.Second))
	if got := evaluateCommission(row, port, quote, 2, now, 60, 30_000_000, 0); got.Accepted {
		t.Fatal("expired commission was accepted")
	}
}

func commissionHarness(t *testing.T) (*recoveryHarness, *Runner) {
	t.Helper()
	h, runner, _ := americaFlagshipFixture(t)
	h.api.setVaultBalance("america", 100_000_000)
	row, _, _ := commissionTestTerms(time.Now().UTC())
	// Reuse the recovery catalog's synthetic hull, with live protocol field shapes.
	row.ShipClass = new(recoveryHull)
	row.Metadata = json.RawMessage(`{"admiralty":true,"ship_class":"hoy","rebuild_value_pence":23000000}`)
	row.RewardPence, row.CollateralPence = 26_000_000, 2_600_000
	cached := make(map[string]json.RawMessage)
	base := h.api.server.Config.Handler
	h.api.server.Config.Handler = http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		if req.URL.Path == "/v1/contract/list" {
			h.api.mu.Lock()
			defer h.api.mu.Unlock()
			rows := []gameapi.CommissionListing{}
			if req.URL.Query().Get("status") == row.Status {
				rows = append(rows, row)
			}
			h.api.writeJSON(w, http.StatusOK, rows)
			return
		}
		if req.Method != http.MethodPost || req.URL.Path != "/v1/contract/accept-commission" && req.URL.Path != "/v1/contract/deliver-commission" {
			base.ServeHTTP(w, req)
			return
		}
		body, err := io.ReadAll(req.Body)
		if err != nil {
			t.Error(err)
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		var request gameapi.DeliverCommissionRequest
		if json.Unmarshal(body, &request) != nil || request.ContractID != row.ID || req.Header.Get("Idempotency-Key") == "" {
			t.Errorf("commission lost its exact durable request: %s", body)
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		h.api.mu.Lock()
		defer h.api.mu.Unlock()
		h.api.record(req, body, http.StatusOK)
		key := req.Header.Get("Idempotency-Key")
		if response, exists := cached[key]; exists {
			h.api.writeJSON(w, http.StatusOK, response)
			return
		}
		var receipt any
		if req.URL.Path == "/v1/contract/accept-commission" {
			if row.Status != "open" {
				t.Error("accepted a second active commission")
			}
			row.Status, row.CarrierCompanyID = "active", new(recoveryCompanyID)
			h.api.vaultBalance["america"] -= row.CollateralPence
			receipt = gameapi.CommissionResult{ContractID: row.ID, Status: "active", PortID: row.OriginPortID, ShipClass: *row.ShipClass,
				PricePence: &row.RewardPence, CollateralPence: &row.CollateralPence, BuilderCompanyID: new(recoveryCompanyID), Admiralty: true, ExpiresAt: row.ExpiresAt}
		} else {
			if row.Status != "active" || request.Name == "" || request.BuildMode != "turnkey_cash" {
				t.Error("delivery lost its accepted builder/name/build mode")
			}
			row.Status = "completed"
			h.api.vaultBalance["america"] += row.RewardPence + row.CollateralPence - 20_570_000
			receipt = gameapi.DeliverCommissionResult{ContractID: row.ID, ShipID: recoveryBuiltShipID, ShipName: request.Name, ShipClass: *row.ShipClass,
				PortID: row.OriginPortID, BuildMode: request.BuildMode, BuildCostPence: new(int64(20_570_000)), PricePaidPence: &row.RewardPence, CollateralReturnedPence: &row.CollateralPence}
		}
		cached[key] = encode(receipt)
		h.api.writeJSON(w, http.StatusOK, cached[key])
	})
	return h, runner
}

func copyCommissionMissionData(t *testing.T, data missionData) missionData {
	t.Helper()
	var copied missionData
	if err := json.Unmarshal(encode(data), &copied); err != nil {
		t.Fatal(err)
	}
	return copied
}

func TestCommissionRestartKeepsOneIntentAndAtomicDelivery(t *testing.T) {
	h, runner := commissionHarness(t)
	loanCapacityRefresh(t, h, runner)
	if changed, err := runner.driveCommissions(h.ctx); !changed || err != nil {
		t.Fatalf("admission: changed=%v err=%v", changed, err)
	}
	mission, data := h.mission(commissionMission)
	generation, name := mission.Generation, data.BuildName
	if data.Action == nil || data.Action.Stage != "commission-accept" || name != "" {
		t.Fatalf("missing durable commission: %+v", data)
	}
	runner = h.runner(primaryTestKey)
	loanCapacityRefresh(t, h, runner)
	mission, data = h.mission(commissionMission)
	if mission.Generation != generation {
		t.Fatal("restart replaced the commission intent")
	}
	// An unsent action from the old financial session must be re-admitted, not sent blindly.
	if _, err := runner.dispatch(h.ctx, mission, &data); err != nil {
		t.Fatal(err)
	}
	if data.Action == nil {
		if _, err := runner.driveCommissions(h.ctx); err != nil {
			t.Fatal(err)
		}
		mission, data = h.mission(commissionMission)
	}
	beforeAccept := copyCommissionMissionData(t, data)
	if _, err := runner.dispatch(h.ctx, mission, &data); err != nil {
		t.Fatal(err)
	}
	if !data.Commission.Accepted || data.Action != nil {
		t.Fatalf("accept receipt not consumed: %+v", data)
	}
	// Simulate a crash after journal completion but before mission advancement.
	mission.Phase = "commission_accept"
	if err := runner.saveMission(h.ctx, mission, &beforeAccept); err != nil {
		t.Fatal(err)
	}
	runner = h.runner(primaryTestKey)
	loanCapacityRefresh(t, h, runner)
	mission, data = h.mission(commissionMission)
	if _, err := runner.dispatch(h.ctx, mission, &data); err != nil {
		t.Fatal(err)
	}
	if !data.Commission.Accepted || mission.Generation != generation || data.BuildName != "" {
		t.Fatal("receipt replay lost the original commission")
	}
	if changed, err := runner.driveCommissions(h.ctx); !changed || err != nil {
		t.Fatalf("delivery staging: %v %v", changed, err)
	}
	mission, data = h.mission(commissionMission)
	name = data.BuildName
	if name == "" {
		t.Fatal("accepted commission did not reserve its delivery name")
	}
	beforeDelivery := copyCommissionMissionData(t, data)
	if data.Action == nil || data.Action.Stage != "commission-deliver" {
		t.Fatalf("not an atomic commission build: %+v", data.Action)
	}
	if _, err := runner.dispatch(h.ctx, mission, &data); err != nil {
		t.Fatal(err)
	}
	if data.Settlement == nil || data.Settlement.ProceedsPence-data.Settlement.AcquisitionPence != 5_430_000 || data.Settlement.ExpensesPence != 0 {
		t.Fatalf("collateral refund became income or build cost disappeared: %+v", data.Settlement)
	}
	mission.Phase = "commission_build"
	if err := runner.saveMission(h.ctx, mission, &beforeDelivery); err != nil {
		t.Fatal(err)
	}
	runner = h.runner(primaryTestKey)
	loanCapacityRefresh(t, h, runner)
	mission, data = h.mission(commissionMission)
	if _, err := runner.dispatch(h.ctx, mission, &data); err != nil {
		t.Fatal(err)
	}
	if _, err := runner.driveCommissions(h.ctx); err != nil {
		t.Fatal(err)
	}
	if h.api.mutationCount("/v1/contract/accept-commission") != 1 || h.api.mutationCount("/v1/contract/deliver-commission") != 1 || h.api.mutationCount("/v1/shipyard/build-ship") != 0 {
		t.Fatal("restart repeated acceptance/delivery or separately built a commission hull")
	}
	settlements, err := h.db.SettlementsByID(h.ctx, h.account.ID, []string{generation})
	if err != nil || len(settlements) != 1 {
		t.Fatalf("durable settlement: %+v %v", settlements, err)
	}
	history, err := h.db.GrowthHistory(h.ctx, h.account.ID, "", time.Now().UTC())
	if err != nil || history.UnknownCosts || history.NetPence != 0 || history.PositiveCycles != 0 {
		t.Fatalf("commission profit entered the hull earnings window: %+v %v", history, err)
	}
}
