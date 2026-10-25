package engine

// These cases exercise production admission, staging, immutable dispatch and
// tick scheduling against the existing isolated TEST_DATABASE_URL harness.
// insurance_owned deliberately stays empty: its rows describe underwriters,
// not the company whose ship is insured. No request leaves the httptest API.

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/DevYukine/go-tradewinds/internal/store"
	"go.uber.org/zap"
	"go.uber.org/zap/zapcore"
)

func piracyInsurancePolicy() InsurancePolicy {
	policy := uwExecPolicy(time.Now().UTC())
	policy.InsuredCompanyID = recoveryCompanyID
	policy.ShipID = recoveryShipBlastoise
	return policy
}

func piracyInsuranceBoard(proxy *uwExecProxy, policies ...InsurancePolicy) {
	rows := make([]map[string]any, 0, len(policies))
	for _, policy := range policies {
		rows = append(rows, uwExecPolicyRow(policy))
	}
	proxy.mu.Lock()
	proxy.insuranceRows = rows
	proxy.membership = map[string]bool{}
	proxy.mu.Unlock()
}

func piracyInsuranceRefresh(t *testing.T, h *recoveryHarness, runner *Runner) {
	t.Helper()
	actionLogRefresh(t, h, runner)
	if owned := decode[[]InsurancePolicy](runner.snapshot, "insurance_owned"); len(owned) != 0 {
		t.Fatalf("fixture unexpectedly supplied underwriter membership: %+v", owned)
	}
}

func piracyInsuranceRequirePolicies(t *testing.T, runner *Runner, policies ...InsurancePolicy) {
	t.Helper()
	observed := decode[[]InsurancePolicy](runner.snapshot, "insurance")
	if !runner.fresh("insurance") || len(observed) != len(policies) {
		t.Fatalf("fixture did not capture the complete fresh insurance board: policies %+v metadata %+v", observed, runner.meta["insurance"])
	}
	for _, expected := range policies {
		found := false
		for _, actual := range observed {
			if actual.ID == expected.ID && actual.InsuredCompanyID == expected.InsuredCompanyID &&
				actual.ShipID == expected.ShipID && actual.OriginPortID == expected.OriginPortID &&
				actual.DestPortID == expected.DestPortID && actual.Status == expected.Status {
				found = true
				break
			}
		}
		if !found {
			t.Fatalf("fixture policy was not authoritatively observed: expected %+v observed %+v", expected, observed)
		}
	}
}

// The base fake implements real grain purchases but not stores or freight
// acceptance. This adapter validates those requests against the served fleet
// and contract, updates cash/cargo, and returns the corresponding receipt.
func piracyInsuranceLoadingFixture(t *testing.T, stage string) (*recoveryHarness, *Runner, *uwExecProxy, *store.Mission, missionData, ShipState) {
	t.Helper()
	h := newRecoveryHarness(t)
	h.api.setFactorPorts(recoveryFarPort)
	row := destinationShipRow(recoveryShipBlastoise, "Blastoise", recoveryHomePort, "berthed", 4, nil, nil)
	row["stores"] = map[string]any{"provisions_tons": 4, "sea_days_remaining": 5}
	h.api.setFleet([]map[string]any{row})
	proxy := installUWExecProxy(t, h)
	piracyInsuranceBoard(proxy)
	contract := freightContractRow("insurance-freight", "open", false)
	proxy.contractRows = []map[string]any{contract}
	downstream := h.api.server.Config.Handler
	h.api.server.Config.Handler = http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		if request.Method != http.MethodPost || request.URL.Path != "/v1/contract/accept-freight" && request.URL.Path != "/v1/market/place-order" {
			downstream.ServeHTTP(writer, request)
			return
		}
		body, err := io.ReadAll(request.Body)
		if err != nil {
			t.Errorf("read synthetic mutation: %v", err)
			writer.WriteHeader(http.StatusBadRequest)
			return
		}
		var payload struct {
			ShipID     string `json:"ship_id"`
			ContractID string `json:"contract_id"`
			Commodity  string `json:"commodity_id"`
			Quantity   int64  `json:"quantity"`
			IntoStores bool   `json:"into_stores"`
			Side       string `json:"side"`
			Port       string `json:"port_id"`
			Price      int64  `json:"price_pence"`
		}
		if err := json.Unmarshal(body, &payload); err != nil {
			t.Errorf("decode synthetic mutation: %v", err)
			writer.WriteHeader(http.StatusBadRequest)
			return
		}
		if request.URL.Path == "/v1/market/place-order" && !payload.IntoStores {
			request.Body = io.NopCloser(bytes.NewReader(body))
			downstream.ServeHTTP(writer, request)
			return
		}
		h.api.mu.Lock()
		defer h.api.mu.Unlock()
		if payload.ShipID != recoveryShipBlastoise || row["ship"].(map[string]any)["status"] != "berthed" {
			h.api.record(request, body, http.StatusConflict)
			h.api.writeProblem(writer, http.StatusConflict, "invalid_fixture_ship", "mutation needs the observed berthed ship")
			return
		}
		if request.URL.Path == "/v1/contract/accept-freight" {
			if payload.ContractID != contract["id"] || contract["status"] != "open" || h.api.vaultBalance["north"] < 5_000 {
				h.api.record(request, body, http.StatusConflict)
				h.api.writeProblem(writer, http.StatusConflict, "invalid_fixture_freight", "contract is not executable")
				return
			}
			proxy.mu.Lock()
			contract["status"] = "active"
			proxy.mu.Unlock()
			row["cargo_manifest"] = map[string]any{recoveryGrain: int64(24)}
			row["cargo_used_tons"] = int64(24)
			h.api.vaultBalance["north"] -= 5_000
			h.api.record(request, body, http.StatusOK)
			h.api.writeJSON(writer, http.StatusOK, map[string]any{
				"contract_id": payload.ContractID, "commodity_id": recoveryGrain,
				"carrier_ship_id": payload.ShipID, "dest_port_id": recoveryFarPort,
				"quantity": 24, "reward_pence": 9_000, "collateral_pence": 5_000,
				"delivery_deadline": contract["metadata"].(map[string]any)["delivery_deadline"],
			})
			return
		}
		gross := payload.Quantity * 50
		customs := ceilBPS(gross, 200)
		if payload.Commodity != "provisions" || payload.Side != "bid" || payload.Port != recoveryHomePort || payload.Quantity <= 0 || payload.Price < 50 || h.api.vaultBalance["north"] < gross+customs {
			h.api.record(request, body, http.StatusConflict)
			h.api.writeProblem(writer, http.StatusConflict, "invalid_fixture_stores", "stores do not match executable local asks")
			return
		}
		stores := row["stores"].(map[string]any)
		stores["provisions_tons"] = stores["provisions_tons"].(int) + int(payload.Quantity)
		h.api.vaultBalance["north"] -= gross + customs
		h.api.record(request, body, http.StatusOK)
		h.api.writeJSON(writer, http.StatusOK, map[string]any{
			"commodity_id": "provisions", "filled_quantity": payload.Quantity,
			"total_gross_pence": gross, "customs_duty_pence": customs,
		})
	})
	runner := refreshedRunner(t, h)
	data := missionData{
		Plan: Plan{Kind: "spot", ShipID: recoveryShipBlastoise, Origin: recoveryHomePort,
			Destination: recoveryFarPort, Commodity: recoveryGrain, Quantity: 10,
			BuyLimitPence: 100, SellLimitPence: 500, TargetStoresTons: 4, Accepted: true},
		CostsKnown: true, Eligible: map[string]int64{}, InitialLedgerIDs: map[string]bool{},
	}
	phase := "buy"
	if stage == "accept-freight" {
		phase = "freight"
		data.Plan.Kind = "freight"
		data.Plan.Quantity = 24
		data.Plan.ContractID = "insurance-freight"
		data.Plan.BuyLimitPence = 0
		data.Plan.Economics.CollateralPence = 5_000
		data.Plan.FreightIDs = []string{"insurance-freight"}
	}
	h.putMission(recoveryShipBlastoise, "piracy-insurance-loading", phase, data)
	mission, data := h.mission(recoveryShipBlastoise)
	return h, runner, proxy, mission, data, harnessShip(t, runner, recoveryShipBlastoise)
}

func piracyInsuranceRequireNoSend(t *testing.T, h *recoveryHarness, shipID string) {
	t.Helper()
	if calls := h.api.mutationsFor("/v1/market/place-order"); len(calls) != 0 {
		t.Fatalf("protected loading reached market transport: %+v", calls)
	}
	if calls := h.api.mutationsFor("/v1/contract/accept-freight"); len(calls) != 0 {
		t.Fatalf("protected loading accepted sealed freight: %+v", calls)
	}
	_, retained := h.mission(shipID)
	if retained.Action != nil || retained.BoughtQuantity != 0 || retained.BuyDone || len(retained.Freight) != 0 || len(h.pending()) != 0 {
		t.Fatalf("protected loading left a sendable action or invented cargo/obligation: %+v", retained)
	}
}

func TestPiracyInsuranceLoadingStagingAndFirstSend(t *testing.T) {
	for _, stage := range []string{"buy", "accept-freight"} {
		for _, boundary := range []string{"staging", "first-send"} {
			for _, status := range []string{"open", "active"} {
				t.Run(stage+"/"+boundary+"/"+status, func(t *testing.T) {
					h, runner, proxy, mission, data, ship := piracyInsuranceLoadingFixture(t, stage)
					if boundary == "first-send" {
						if _, err := runner.driveMission(h.ctx, ship, mission, &data); err != nil {
							t.Fatal(err)
						}
						if data.Action == nil || data.Action.Stage != stage {
							t.Fatalf("uninsured control did not reach executable %s: %+v holds %+v", stage, data.Action, runner.holding)
						}
					}
					policy := piracyInsurancePolicy()
					policy.Status = status
					piracyInsuranceBoard(proxy, policy)
					piracyInsuranceRefresh(t, h, runner)
					if boundary == "first-send" {
						if _, err := runner.dispatch(h.ctx, mission, &data); err != nil {
							t.Fatal(err)
						}
					} else if _, err := runner.driveMission(h.ctx, ship, mission, &data); err != nil {
						t.Fatal(err)
					}
					piracyInsuranceRequireNoSend(t, h, ship.Ship.ID)
				})
			}
		}
	}
}

func TestPiracyInsuranceRetainsObservedPolicyUntilFreshTerminalRelease(t *testing.T) {
	for _, failure := range []string{"outage", "invalid policy"} {
		t.Run(failure, func(t *testing.T) {
			h, runner, proxy, mission, data, ship := piracyInsuranceLoadingFixture(t, "buy")
			policy := piracyInsurancePolicy()
			// An active board status, not a local expiry calculation, is authority.
			policy.ExpiresAt = time.Now().UTC().Add(-time.Hour)
			policy.CreatedAt = policy.ExpiresAt.Add(-2 * time.Hour)
			policy.UpdatedAt = policy.ExpiresAt.Add(-time.Hour)
			piracyInsuranceBoard(proxy, policy)
			piracyInsuranceRefresh(t, h, runner)
			downstream := h.api.server.Config.Handler
			var unavailable atomic.Bool
			unavailable.Store(true)
			h.api.server.Config.Handler = http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
				if unavailable.Load() && request.Method == http.MethodGet && request.URL.Path == "/v1/insurance/list" && request.URL.Query().Get("underwriter_company_id") == "" {
					if failure == "outage" {
						uwExecWriteProblem(writer, http.StatusForbidden, "board_unavailable", "synthetic board outage")
					} else {
						uwExecWriteJSON(writer, http.StatusOK, []map[string]any{{"id": policy.ID, "insured_value_pence": "not-pence"}})
					}
					return
				}
				downstream.ServeHTTP(writer, request)
			})
			// Reconstruct from durable snapshot before the failing observation.
			runner = h.runner(primaryTestKey)
			piracyInsuranceRefresh(t, h, runner)
			if _, err := runner.driveMission(h.ctx, ship, mission, &data); err != nil {
				t.Fatal(err)
			}
			piracyInsuranceRequireNoSend(t, h, ship.Ship.ID)
			unavailable.Store(false)
			policy.Status = "settled_safe"
			piracyInsuranceBoard(proxy, policy)
			piracyInsuranceRefresh(t, h, runner)
			mission, data = h.mission(ship.Ship.ID)
			if _, err := runner.driveMission(h.ctx, ship, mission, &data); err != nil {
				t.Fatal(err)
			}
			if data.Action == nil || data.Action.Stage != "buy" {
				t.Fatalf("fresh terminal observation did not release loading: %+v holds %+v", data, runner.holding)
			}
			if _, err := runner.dispatch(h.ctx, mission, &data); err != nil {
				t.Fatal(err)
			}
			if calls := h.api.mutationsFor("/v1/market/place-order"); len(calls) != 1 || calls[0].Status != http.StatusOK {
				t.Fatalf("released cargo was not purchased exactly once: %+v", calls)
			}
			_, completed := h.mission(ship.Ship.ID)
			if !completed.BuyDone || completed.BoughtQuantity != 10 || completed.AcquisitionPence != 1_000 {
				t.Fatalf("released loading did not durably consume receipt: %+v", completed)
			}
		})
	}
}

func TestPiracyInsuranceDeparturePolicySet(t *testing.T) {
	for _, boundary := range []string{"preparation", "first-send"} {
		for _, scenario := range []string{"matching", "origin conflict", "destination conflict", "matching then conflicting", "conflicting then matching", "stale matching", "foreign company", "foreign ship", "settled_safe", "claimed_loss", "cancelled", "never observed outage", "active past expiry"} {
			t.Run(boundary+"/"+scenario, func(t *testing.T) {
				h, runner, mission, data, ship := procurementRecoveryFixture(t, false)
				proxy := installUWExecProxy(t, h)
				policy := piracyInsurancePolicy()
				policies := []InsurancePolicy{policy}
				blocked := false
				switch scenario {
				case "origin conflict":
					policies[0].OriginPortID = "amsterdam"
					blocked = true
				case "destination conflict":
					policies[0].DestPortID = "amsterdam"
					blocked = true
				case "matching then conflicting", "conflicting then matching":
					conflict := policy
					conflict.ID = "bbbbbbbb-bbbb-4bbb-8bbb-bbbbbbbbbbbb"
					conflict.DestPortID = "amsterdam"
					policies = []InsurancePolicy{policy, conflict}
					if scenario == "conflicting then matching" {
						policies[0], policies[1] = policies[1], policies[0]
					}
					blocked = true
				case "stale matching":
					blocked = true
				case "foreign company":
					policies[0].InsuredCompanyID = uwExecInsuredCompanyID
					policies[0].DestPortID = "amsterdam"
				case "foreign ship":
					policies[0].ShipID = recoveryShipCharizard
					policies[0].DestPortID = "amsterdam"
				case "settled_safe", "claimed_loss", "cancelled":
					policies[0].Status = scenario
					policies[0].DestPortID = "amsterdam"
				case "never observed outage":
					policies = nil
				case "active past expiry":
					policies[0].ExpiresAt = time.Now().UTC().Add(-24 * time.Hour)
					policies[0].CreatedAt = policies[0].ExpiresAt.Add(-2 * time.Hour)
					policies[0].UpdatedAt = policies[0].ExpiresAt.Add(-time.Hour)
					policies[0].DestPortID = "amsterdam"
					blocked = true
				}
				piracyInsuranceBoard(proxy, policies...)
				piracyInsuranceRefresh(t, h, runner)
				piracyInsuranceRequirePolicies(t, runner, policies...)
				if scenario == "stale matching" || scenario == "never observed outage" {
					downstream := h.api.server.Config.Handler
					h.api.server.Config.Handler = http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
						if request.Method == http.MethodGet && request.URL.Path == "/v1/insurance/list" {
							uwExecWriteProblem(writer, http.StatusForbidden, "board_unavailable", "synthetic outage")
							return
						}
						downstream.ServeHTTP(writer, request)
					})
					if scenario == "never observed outage" {
						delete(runner.snapshot, "insurance")
						delete(runner.meta, "insurance")
					}
					piracyInsuranceRefresh(t, h, runner)
				}
				if boundary == "first-send" {
					if err := runner.stage(h.ctx, mission, &data, ship, "depart", "/v1/fleet/depart", map[string]any{"ship_id": ship.Ship.ID, "dest_port_id": recoveryFarPort}); err != nil {
						t.Fatal(err)
					}
				} else if _, err := runner.prepareDeparture(h.ctx, ship, mission, &data); err != nil {
					t.Fatal(err)
				}
				if data.Action != nil {
					if _, err := runner.dispatch(h.ctx, mission, &data); err != nil {
						t.Fatal(err)
					}
				}
				calls := h.api.mutationsFor("/v1/fleet/depart")
				if blocked {
					if len(calls) != 0 || data.Action != nil || len(h.pending()) != 0 {
						t.Fatalf("insured departure escaped %s: calls %+v action %+v", scenario, calls, data.Action)
					}
					_, retained := h.mission(ship.Ship.ID)
					if retained.BoughtQuantity != 20 || !retained.BuyDone || retained.AcquisitionPence != 10_000 {
						t.Fatalf("hold changed actual loading: %+v", retained)
					}
				} else if len(calls) != 1 || calls[0].Status != http.StatusOK || mission.Phase != "sailing" {
					t.Fatalf("legal departure blocked by %s: calls %+v phase %s holds %+v", scenario, calls, mission.Phase, runner.holding)
				}
			})
		}
	}
}

func TestPiracyInsuranceMixedCaseOwnerAndShip(t *testing.T) {
	h, runner, proxy, mission, data, ship := piracyInsuranceLoadingFixture(t, "buy")
	// The shared company/ship UUIDs contain digits only. Give this isolated
	// observation letter-bearing UUIDs so case folding is really tested.
	companyID := "aabbccdd-abcd-4abc-8abc-aabbccddeeff"
	shipID := "abcdefab-abcd-4abc-8abc-abcdefabcdef"
	runner.account.CompanyID = companyID
	h.api.mu.Lock()
	h.api.fleet[0]["ship"].(map[string]any)["id"] = shipID
	h.api.mu.Unlock()
	mission.ShipID = shipID
	data.Plan.ShipID = shipID
	policy := piracyInsurancePolicy()
	policy.InsuredCompanyID = strings.ToUpper(companyID)
	policy.ShipID = strings.ToUpper(shipID)
	piracyInsuranceBoard(proxy, policy)
	piracyInsuranceRefresh(t, h, runner)
	ship = harnessShip(t, runner, shipID)
	if err := runner.saveMission(h.ctx, mission, &data); err != nil {
		t.Fatal(err)
	}
	if _, err := runner.driveMission(h.ctx, ship, mission, &data); err != nil {
		t.Fatal(err)
	}
	piracyInsuranceRequireNoSend(t, h, shipID)
}

func TestPiracyInsuranceCompletedLoadingPhasesAdvance(t *testing.T) {
	for _, phase := range []string{"buy", "freight"} {
		t.Run(phase, func(t *testing.T) {
			h, runner, proxy, mission, data, ship := piracyInsuranceLoadingFixture(t, "accept-freight")
			piracyInsuranceBoard(proxy, piracyInsurancePolicy())
			piracyInsuranceRefresh(t, h, runner)
			mission.Phase = phase
			data.BuyDone = true
			data.BoughtQuantity = 10
			deadline := time.Now().UTC().Add(6 * time.Hour)
			data.Freight = []freightObligation{{ContractID: "insurance-freight", Commodity: recoveryGrain, Quantity: 24, Destination: recoveryFarPort, Deadline: &deadline}}
			if err := runner.saveMission(h.ctx, mission, &data); err != nil {
				t.Fatal(err)
			}
			if _, err := runner.driveMission(h.ctx, ship, mission, &data); err != nil {
				t.Fatal(err)
			}
			want := "depart"
			if phase == "buy" {
				want = "freight"
			}
			persisted, retained := h.mission(ship.Ship.ID)
			if persisted.Phase != want || retained.Action != nil || retained.BoughtQuantity != 10 || len(retained.Freight) != 1 {
				t.Fatalf("insurance froze a completed %s phase or lost loaded obligations: phase %s data %+v", phase, persisted.Phase, retained)
			}
		})
	}
}

func TestPiracyInsuranceStoresAndDeliveryRemainAllowed(t *testing.T) {
	t.Run("stores", func(t *testing.T) {
		h, runner, proxy, mission, data, ship := piracyInsuranceLoadingFixture(t, "buy")
		piracyInsuranceBoard(proxy, piracyInsurancePolicy())
		piracyInsuranceRefresh(t, h, runner)
		if _, err := runner.provisionMission(h.ctx, ship, mission, &data, 5); err != nil {
			t.Fatal(err)
		}
		if data.Action == nil || data.Action.Stage != "provision" {
			t.Fatalf("insurance suppressed stores: %+v", data.Action)
		}
		if _, err := runner.dispatch(h.ctx, mission, &data); err != nil {
			t.Fatal(err)
		}
		calls := h.api.mutationsFor("/v1/market/place-order")
		var request struct {
			IntoStores bool `json:"into_stores"`
		}
		if len(calls) != 1 {
			t.Fatalf("stores did not execute once: %+v", calls)
		}
		if err := json.Unmarshal(calls[0].Body, &request); err != nil || !request.IntoStores || calls[0].Status != http.StatusOK {
			t.Fatalf("store purchase was not an observed into_stores fill: %+v", calls)
		}
		_, retained := h.mission(ship.Ship.ID)
		if retained.ProvisionsPence != 51 || retained.BoughtQuantity != 0 || retained.Action != nil {
			t.Fatalf("stores were mistaken for insured loading: %+v", retained)
		}
	})
	t.Run("freight delivery", func(t *testing.T) {
		h := newRecoveryHarness(t)
		proxy := installUWExecProxy(t, h)
		piracyInsuranceBoard(proxy, piracyInsurancePolicy())
		proxy.contractRows = []map[string]any{freightContractRow("insurance-delivery", "active", false)}
		h.api.setFleet([]map[string]any{destinationShipRow(recoveryShipBlastoise, "Blastoise", recoveryFarPort, "berthed", 4, nil, map[string]int64{recoveryGrain: 24})})
		deadline := time.Now().UTC().Add(6 * time.Hour)
		h.putMission(recoveryShipBlastoise, "insurance-legitimate-delivery", "arrival", missionData{
			Plan:     Plan{Kind: "recovery", ShipID: recoveryShipBlastoise, Destination: recoveryFarPort},
			Freight:  []freightObligation{{ContractID: "insurance-delivery", Commodity: recoveryGrain, Quantity: 24, Destination: recoveryFarPort, Deadline: &deadline, RewardPence: 9_000, CollateralPence: 5_000}},
			Eligible: map[string]int64{},
		})
		runner := h.runner(primaryTestKey)
		uwExecTicks(t, h, runner, 2)
		calls := h.api.mutationsFor("/v1/contract/deliver-freight")
		_, retained := h.mission(recoveryShipBlastoise)
		if len(calls) != 1 || calls[0].Status != http.StatusOK || !retained.Freight[0].Settled || retained.ProceedsPence != 9_000 {
			t.Fatalf("insurance blocked legitimate delivery: calls %+v data %+v", calls, retained)
		}
	})
	t.Run("sale", func(t *testing.T) {
		h, runner, _, _, _ := procurementRecoveryFixture(t, false)
		proxy := installUWExecProxy(t, h)
		piracyInsuranceBoard(proxy, piracyInsurancePolicy())
		row := destinationShipRow(recoveryShipBlastoise, "Blastoise", recoveryFarPort, "berthed", 4, nil, map[string]int64{recoveryGrain: 20})
		h.api.setFleet([]map[string]any{row})
		piracyInsuranceRefresh(t, h, runner)
		mission, data := h.mission(recoveryShipBlastoise)
		data.Eligible = map[string]int64{recoveryGrain: 20}
		ship := harnessShip(t, runner, recoveryShipBlastoise)
		if err := runner.stage(h.ctx, mission, &data, ship, "sell", "/v1/market/place-order", orderBody(ship, recoveryGrain, "ask", 20, 475, false)); err != nil {
			t.Fatal(err)
		}
		if _, err := runner.dispatch(h.ctx, mission, &data); err != nil {
			t.Fatal(err)
		}
		calls := h.api.mutationsFor("/v1/market/place-order")
		_, retained := h.mission(ship.Ship.ID)
		if len(calls) != 1 || calls[0].Status != http.StatusOK || retained.ProceedsPence != 9_500 || retained.DeliveredQuantity != 20 {
			t.Fatalf("insurance blocked legitimate cargo sale: calls %+v data %+v", calls, retained)
		}
	})
}

func TestPiracyInsuranceHeldHullDoesNotStarveSecondHull(t *testing.T) {
	h := newRecoveryHarness(t)
	h.api.setFactorPorts(recoveryFarPort)
	rows := []map[string]any{}
	for _, id := range []string{recoveryShipBlastoise, recoveryShipCharizard} {
		row := destinationShipRow(id, id, recoveryHomePort, "berthed", 4, nil, nil)
		row["stores"] = map[string]any{"provisions_tons": 4, "sea_days_remaining": 5}
		rows = append(rows, row)
	}
	h.api.setFleet(rows)
	proxy := installUWExecProxy(t, h)
	piracyInsuranceBoard(proxy, piracyInsurancePolicy())
	runner := h.runner(primaryTestKey)
	uwExecTicks(t, h, runner, 9)
	calls := h.api.mutationsFor("/v1/market/place-order")
	found := false
	for _, call := range calls {
		var body struct {
			ShipID     string `json:"ship_id"`
			IntoStores bool   `json:"into_stores"`
		}
		if err := json.Unmarshal(call.Body, &body); err != nil {
			t.Fatal(err)
		}
		if body.ShipID == recoveryShipBlastoise && !body.IntoStores {
			t.Fatalf("first insured hull loaded cargo: %+v", call)
		}
		found = found || body.ShipID == recoveryShipCharizard && call.Status == http.StatusOK && !body.IntoStores
	}
	if !found {
		t.Fatalf("first held hull starved actionable second hull across ticks: calls %+v holds %+v", calls, runner.holding)
	}
	_, second := h.mission(recoveryShipCharizard)
	if !second.BuyDone || second.BoughtQuantity <= 0 {
		t.Fatalf("second hull purchase was not durable: %+v", second)
	}
	first, err := h.db.GetMission(h.ctx, h.account.ID, recoveryShipBlastoise)
	if err != nil {
		t.Fatal(err)
	}
	if first != nil {
		var retained missionData
		if err := json.Unmarshal(first.Data, &retained); err != nil {
			t.Fatal(err)
		}
		if first.Quantity > 0 || retained.Plan.Quantity > 0 || retained.BoughtQuantity > 0 ||
			len(retained.Plan.FreightIDs) > 0 || len(retained.Freight) > 0 {
			t.Fatalf("insured first hull retained a blocked cargo reservation: %+v", retained)
		}
	}
}

func TestPiracyInsuranceGrowthReuseDoesNotCommitOrBuyReplacement(t *testing.T) {
	h, runner, proxy, _, _, ship := piracyInsuranceLoadingFixture(t, "buy")
	if err := h.db.DeleteMission(h.ctx, h.account.ID, ship.Ship.ID); err != nil {
		t.Fatal(err)
	}
	h.seedLaneProofs()
	piracyInsuranceBoard(proxy, piracyInsurancePolicy())
	piracyInsuranceRefresh(t, h, runner)
	before, err := runner.treasuryReserves(h.ctx)
	if err != nil {
		t.Fatal(err)
	}
	for range 3 {
		if _, err := runner.considerGrowth(h.ctx); err != nil {
			t.Fatal(err)
		}
		h.missingMission(ship.Ship.ID)
		h.missingMission("__growth")
	}
	after, err := runner.treasuryReserves(h.ctx)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(encode(before), encode(after)) || h.api.mutationCount("/v1/shipyard/build-ship") != 0 || len(h.pending()) != 0 {
		t.Fatalf("insurance-held reusable hull reserved capital or bought replacement: reserves %v -> %v pending %+v", before, after, h.pending())
	}
}

func TestPiracyInsuranceSourcingAdmissionAndFunding(t *testing.T) {
	for _, conflict := range []bool{false, true} {
		name := "matching empty sourcing"
		if conflict {
			name = "conflicting empty sourcing"
		}
		t.Run(name, func(t *testing.T) {
			h := newRecoveryHarness(t)
			h.api.setFleet([]map[string]any{sourcingReviewShipRow()})
			proxy := installUWExecProxy(t, h)
			policy := piracyInsurancePolicy()
			policy.DestPortID = "amsterdam"
			if conflict {
				policy.DestPortID = recoveryFarPort
			}
			piracyInsuranceBoard(proxy, policy)
			runner := refreshedRunner(t, h)
			sourcingReviewInject(t, runner, sourcingReviewCharters())
			ship := harnessShip(t, runner, recoveryShipBlastoise)
			input, err := runner.sourcingInput(h.ctx, ship)
			if err != nil {
				t.Fatal(err)
			}
			if len(acceptedRelocations(planSourcingRelocation(input))) == 0 {
				t.Fatal("fixture offers no independently executable sourcing candidate")
			}
			before, err := runner.treasuryReserves(h.ctx)
			if err != nil {
				t.Fatal(err)
			}
			changed, err := runner.considerSourcing(h.ctx, ship)
			if err != nil {
				t.Fatal(err)
			}
			if conflict {
				h.missingMission(ship.Ship.ID)
				after, err := runner.treasuryReserves(h.ctx)
				if err != nil {
					t.Fatal(err)
				}
				if !bytes.Equal(encode(before), encode(after)) || h.api.mutationCount("/v1/treasury/remit") != 0 || len(h.pending()) != 0 {
					t.Fatalf("conflicting sourcing reserved/funded an intent: %v -> %v", before, after)
				}
				return
			}
			if !changed {
				t.Fatalf("matching insured empty sourcing held: %+v", runner.holding)
			}
			mission, data := h.mission(ship.Ship.ID)
			if data.Relocation == nil || mission.Destination != "amsterdam" || data.BoughtQuantity != 0 {
				t.Fatalf("matching empty sourcing did not persist the real probe: %+v %+v", mission, data)
			}
			// Once a previously admitted probe conflicts, even a necessary
			// transfer must not stage a new spending action.
			policy.DestPortID = recoveryFarPort
			piracyInsuranceBoard(proxy, policy)
			h.api.setVaultBalance("north", 0)
			h.api.setVaultBalance("south", 50_000_000)
			piracyInsuranceRefresh(t, h, runner)
			sourcingReviewInject(t, runner, sourcingReviewCharters())
			if _, err := runner.driveMission(h.ctx, ship, mission, &data); err != nil {
				t.Fatal(err)
			}
			_, retained := h.mission(ship.Ship.ID)
			if retained.Action != nil || h.api.mutationCount("/v1/treasury/remit") != 0 || len(h.pending()) != 0 {
				t.Fatalf("changed insurance route funded the old sourcing intent: %+v", retained)
			}
		})
	}
}

func TestPiracyInsurancePostWaitExpiryHasDurableNoSendProof(t *testing.T) {
	h, runner, mission, data, ship := procurementRecoveryFixture(t, false)
	proxy := installUWExecProxy(t, h)
	piracyInsuranceBoard(proxy, piracyInsurancePolicy())
	piracyInsuranceRefresh(t, h, runner)
	if err := runner.stage(h.ctx, mission, &data, ship, "depart", "/v1/fleet/depart", map[string]any{"ship_id": ship.Ship.ID, "dest_port_id": recoveryFarPort}); err != nil {
		t.Fatal(err)
	}
	action := *data.Action
	// A successful synthetic read publishes real limiter response headers
	// for the write bucket. The dispatch must cross that wait before sending.
	downstream := h.api.server.Config.Handler
	h.api.server.Config.Handler = http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		if request.URL.Path == "/test/write-pause" {
			writer.Header().Set("X-Ratelimit-Bucket", "write")
			writer.Header().Set("Retry-After", "1")
			uwExecWriteJSON(writer, http.StatusOK, map[string]any{})
			return
		}
		downstream.ServeHTTP(writer, request)
	})
	// Existing debug hook is a synchronous boundary after the initial
	// preflight and journal creation; do not key the test to log wording.
	var injected bool
	core := zapcore.NewCore(zapcore.NewJSONEncoder(zap.NewProductionEncoderConfig()), zapcore.AddSync(io.Discard), zap.DebugLevel)
	runner.log = zap.New(core, zap.Hooks(func(entry zapcore.Entry) error {
		if injected || entry.Level != zap.DebugLevel {
			return nil
		}
		injected = true
		var response json.RawMessage
		if err := runner.api.Get(h.ctx, "/test/write-pause", &response); err != nil {
			return err
		}
		meta := runner.meta["insurance"]
		meta.UpdatedAt = time.Now().UTC().Add(-119500 * time.Millisecond)
		meta.AttemptedAt = meta.UpdatedAt
		runner.meta["insurance"] = meta
		return nil
	}))
	if _, err := runner.dispatch(h.ctx, mission, &data); err != nil {
		t.Fatal(err)
	}
	if !injected || h.api.mutationCount("/v1/fleet/depart") != 0 || len(h.pending()) != 0 {
		t.Fatalf("policy capture expiry during the limiter wait crossed transport: injected %t calls %+v", injected, h.api.mutationsFor("/v1/fleet/depart"))
	}
	journaled, err := h.db.BeginAction(h.ctx, &store.Action{ID: action.ID, AccountID: h.account.ID, Method: action.Method, Path: action.Path, Body: action.Body})
	if err != nil {
		t.Fatal(err)
	}
	if journaled.State != store.ActionReconciled || journaled.Status != 0 || !json.Valid(journaled.Response) || bytes.Equal(journaled.Response, []byte("null")) {
		t.Fatalf("postwait veto lacks durable no-send evidence: %+v", journaled)
	}
	_, retained := h.mission(ship.Ship.ID)
	if retained.Action != nil || retained.Sequence != data.Sequence || retained.BoughtQuantity != 20 || retained.AcquisitionPence != 10_000 {
		t.Fatalf("no-send veto changed real loaded cargo or retained intent: %+v", retained)
	}
	// Crash after proof commit but before mission detachment: a restart
	// consumes the proof, never turning this known-unsent row into a replay.
	retained.Action = &action
	if err := runner.saveMission(h.ctx, mission, &retained); err != nil {
		t.Fatal(err)
	}
	restarted := h.runner(primaryTestKey)
	mission, retained = h.mission(ship.Ship.ID)
	if _, err := restarted.dispatch(h.ctx, mission, &retained); err != nil {
		t.Fatal(err)
	}
	if retained.Action != nil || h.api.mutationCount("/v1/fleet/depart") != 0 || len(h.pending()) != 0 {
		t.Fatalf("restart replayed a proven never-sent insured departure: %+v", retained)
	}
}

func TestPiracyInsuranceImmutableLoadingRecovery(t *testing.T) {
	for _, stage := range []string{"buy", "accept-freight"} {
		for _, state := range []string{"attempted", "completed"} {
			t.Run(stage+"/"+state, func(t *testing.T) {
				h, runner, proxy, mission, data, ship := piracyInsuranceLoadingFixture(t, stage)
				if _, err := runner.driveMission(h.ctx, ship, mission, &data); err != nil {
					t.Fatal(err)
				}
				if data.Action == nil || data.Action.Stage != stage {
					t.Fatalf("uninsured control did not stage %s: %+v", stage, data.Action)
				}
				action := *data.Action
				original := data
				downstream := h.api.server.Config.Handler
				var loseResponse atomic.Bool
				loseResponse.Store(state == "attempted")
				h.api.server.Config.Handler = http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
					if loseResponse.Load() && request.Method == action.Method && request.URL.Path == action.Path {
						body, err := io.ReadAll(request.Body)
						if err != nil {
							t.Errorf("read attempted request: %v", err)
							return
						}
						uwExecServeLost(proxy.recordMutation)(writer, request, body)
						return
					}
					downstream.ServeHTTP(writer, request)
				})
				if _, err := runner.dispatch(h.ctx, mission, &data); err != nil {
					t.Fatal(err)
				}
				before := h.api.mutationsFor(action.Path)
				if len(before) == 0 {
					t.Fatal("recovery fixture never attempted the real HTTP request")
				}
				if state == "completed" {
					if data.Action != nil || data.LastActionID != action.ID {
						t.Fatalf("control receipt did not complete: %+v", data)
					}
					// Restore only the crash-window mission; retain the completed
					// durable receipt and the already-mutated observed API state.
					if err := runner.saveMission(h.ctx, mission, &original); err != nil {
						t.Fatal(err)
					}
				} else {
					if len(h.pending()) != 1 {
						t.Fatalf("lost request is not durably pending: %+v", h.pending())
					}
					h.expireCooldown(ship.Ship.ID)
				}
				loseResponse.Store(false)
				piracyInsuranceBoard(proxy, piracyInsurancePolicy())
				restarted := h.runner(primaryTestKey)
				piracyInsuranceRefresh(t, h, restarted)
				mission, data = h.mission(ship.Ship.ID)
				if _, err := restarted.dispatch(h.ctx, mission, &data); err != nil {
					t.Fatal(err)
				}
				calls := h.api.mutationsFor(action.Path)
				want := len(before)
				if state == "attempted" {
					want++
				}
				if len(calls) != want {
					t.Fatalf("insurance changed immutable %s recovery: calls %+v", state, calls)
				}
				for _, call := range calls {
					if call.IdempotencyKey != action.ID || !bytes.Equal(call.Body, action.Body) {
						t.Fatalf("recovery changed original request/key: %+v", call)
					}
				}
				_, retained := h.mission(ship.Ship.ID)
				if retained.Action != nil || retained.LastActionID != action.ID || len(h.pending()) != 0 {
					t.Fatalf("insurance blocked stored/attempted outcome consumption: %+v", retained)
				}
				if stage == "buy" && (!retained.BuyDone || retained.BoughtQuantity != 10 || retained.AcquisitionPence != 1_000) {
					t.Fatalf("recovered buy lost real receipt: %+v", retained)
				}
				if stage == "accept-freight" && (len(retained.Freight) != 1 || retained.Freight[0].Quantity != 24 || retained.Freight[0].ContractID != "insurance-freight") {
					t.Fatalf("recovered freight lost real obligation: %+v", retained)
				}
			})
		}
	}
}

func TestPiracyInsuranceOwnPolicyOnLaterGeneralBoardPage(t *testing.T) {
	h, runner, _, mission, data, ship := piracyInsuranceLoadingFixture(t, "buy")
	foreignRows := make([]map[string]any, 200)
	for index := range foreignRows {
		policy := uwExecPolicy(time.Now().UTC())
		policy.ID = fmt.Sprintf("cccccccc-cccc-4ccc-8ccc-%012d", index)
		foreignRows[index] = uwExecPolicyRow(policy)
	}
	own := piracyInsurancePolicy()
	downstream := h.api.server.Config.Handler
	var laterPage atomic.Bool
	h.api.server.Config.Handler = http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		if request.Method == http.MethodGet && request.URL.Path == "/v1/insurance/list" && request.URL.Query().Get("underwriter_company_id") == "" {
			if request.URL.Query().Get("cursor") == "" {
				uwExecWriteJSON(writer, http.StatusOK, foreignRows)
			} else {
				laterPage.Store(true)
				uwExecWriteJSON(writer, http.StatusOK, []map[string]any{uwExecPolicyRow(own)})
			}
			return
		}
		downstream.ServeHTTP(writer, request)
	})
	piracyInsuranceRefresh(t, h, runner)
	if !laterPage.Load() {
		t.Fatal("fixture never served the policy's later public-board page")
	}
	if _, err := runner.driveMission(h.ctx, ship, mission, &data); err != nil {
		t.Fatal(err)
	}
	piracyInsuranceRequireNoSend(t, h, ship.Ship.ID)
}

func TestPiracyInsuranceUnrelatedPoliciesDoNotBlockLoading(t *testing.T) {
	for _, scenario := range []string{"foreign company", "foreign ship", "settled_safe", "claimed_loss", "cancelled", "unavailable never observed"} {
		t.Run(scenario, func(t *testing.T) {
			h, runner, proxy, mission, data, ship := piracyInsuranceLoadingFixture(t, "buy")
			policy := piracyInsurancePolicy()
			switch scenario {
			case "foreign company":
				policy.InsuredCompanyID = uwExecInsuredCompanyID
			case "foreign ship":
				policy.ShipID = recoveryShipCharizard
			default:
				policy.Status = scenario
			}
			if scenario == "unavailable never observed" {
				delete(runner.snapshot, "insurance")
				delete(runner.meta, "insurance")
				downstream := h.api.server.Config.Handler
				h.api.server.Config.Handler = http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
					if request.Method == http.MethodGet && request.URL.Path == "/v1/insurance/list" {
						uwExecWriteProblem(writer, http.StatusForbidden, "board_unavailable", "synthetic outage")
						return
					}
					downstream.ServeHTTP(writer, request)
				})
			} else {
				piracyInsuranceBoard(proxy, policy)
			}
			piracyInsuranceRefresh(t, h, runner)
			if scenario != "unavailable never observed" {
				piracyInsuranceRequirePolicies(t, runner, policy)
			}
			if _, err := runner.driveMission(h.ctx, ship, mission, &data); err != nil {
				t.Fatal(err)
			}
			if data.Action == nil || data.Action.Stage != "buy" {
				t.Fatalf("unrelated insurance globally blocked loading: %+v holds %+v", data, runner.holding)
			}
			if _, err := runner.dispatch(h.ctx, mission, &data); err != nil {
				t.Fatal(err)
			}
			calls := h.api.mutationsFor("/v1/market/place-order")
			_, retained := h.mission(ship.Ship.ID)
			if len(calls) != 1 || calls[0].Status != http.StatusOK || !retained.BuyDone || retained.BoughtQuantity != 10 {
				t.Fatalf("unrelated insurance suppressed executable purchase: calls %+v data %+v", calls, retained)
			}
		})
	}
}

func TestPiracyInsuranceEmptySourcingDeparture(t *testing.T) {
	for _, conflict := range []bool{false, true} {
		name := "matching"
		if conflict {
			name = "conflicting"
		}
		t.Run(name, func(t *testing.T) {
			h := newRecoveryHarness(t)
			h.api.setFleet([]map[string]any{sourcingReviewShipRow()})
			sourcingPutDepartureMission(t, h, recoveryShipBlastoise, "insured-empty-departure", "depart", sourcingDepartureSeed(t))
			proxy := installUWExecProxy(t, h)
			policy := piracyInsurancePolicy()
			if conflict {
				policy.DestPortID = "amsterdam"
			}
			piracyInsuranceBoard(proxy, policy)
			downstream := h.api.server.Config.Handler
			h.api.server.Config.Handler = http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
				if request.Method != http.MethodPost || request.URL.Path != "/v1/fleet/depart" {
					downstream.ServeHTTP(writer, request)
					return
				}
				body, err := io.ReadAll(request.Body)
				if err != nil {
					t.Errorf("read empty departure: %v", err)
					writer.WriteHeader(http.StatusBadRequest)
					return
				}
				var payload struct {
					ShipID      string `json:"ship_id"`
					Destination string `json:"dest_port_id"`
				}
				if err := json.Unmarshal(body, &payload); err != nil {
					t.Errorf("decode empty departure: %v", err)
					writer.WriteHeader(http.StatusBadRequest)
					return
				}
				h.api.mu.Lock()
				defer h.api.mu.Unlock()
				row := h.api.fleet[0]
				ship := row["ship"].(map[string]any)
				if payload.ShipID != recoveryShipBlastoise || payload.Destination != recoveryFarPort || ship["status"] != "berthed" || len(row["cargo_manifest"].(map[string]any)) != 0 {
					h.api.record(request, body, http.StatusConflict)
					h.api.writeProblem(writer, http.StatusConflict, "invalid_empty_relocation", "empty sourcing departure disagrees with observed fleet")
					return
				}
				ship["status"] = "sailing"
				voyage := sailingShipRow(payload.ShipID, "Blastoise", recoveryHomePort, payload.Destination)
				row["voyage"] = voyage["voyage"]
				row["route_legs"] = voyage["route_legs"]
				h.api.record(request, body, http.StatusOK)
				h.api.writeJSON(writer, http.StatusOK, map[string]any{
					"ship_id": payload.ShipID, "dest_port_id": payload.Destination,
					"berth_dues_pence": 0, "berth_dues_debt_pence": 0,
				})
			})
			runner := refreshedRunner(t, h)
			piracyInsuranceRequirePolicies(t, runner, policy)
			for range 2 {
				sourcingDepartureTick(t, h, runner)
			}
			calls := h.api.mutationsFor("/v1/fleet/depart")
			if conflict {
				if len(calls) != 0 || len(h.pending()) != 0 {
					t.Fatalf("conflicting insured empty sourcing reached transport: %+v", calls)
				}
				return
			}
			mission, retained := h.mission(recoveryShipBlastoise)
			if len(calls) != 1 || calls[0].Status != http.StatusOK || mission.Phase != "sailing" || retained.Relocation == nil || retained.BoughtQuantity != 0 {
				t.Fatalf("matching policy blocked empty sourcing departure: calls %+v phase %s data %+v", calls, mission.Phase, retained)
			}
		})
	}
}

func TestPiracyInsuranceImmutableDepartureRecovery(t *testing.T) {
	for _, state := range []string{"attempted", "completed"} {
		t.Run(state, func(t *testing.T) {
			h, runner, mission, data, ship := procurementRecoveryFixture(t, false)
			proxy := installUWExecProxy(t, h)
			policy := piracyInsurancePolicy()
			piracyInsuranceBoard(proxy, policy)
			piracyInsuranceRefresh(t, h, runner)
			piracyInsuranceRequirePolicies(t, runner, policy)
			if _, err := runner.prepareDeparture(h.ctx, ship, mission, &data); err != nil {
				t.Fatal(err)
			}
			if data.Action == nil {
				t.Fatalf("matching-policy control could not stage departure: %+v", runner.holding)
			}
			action := *data.Action
			original := data
			downstream := h.api.server.Config.Handler
			var loseResponse atomic.Bool
			loseResponse.Store(state == "attempted")
			h.api.server.Config.Handler = http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
				if loseResponse.Load() && request.Method == action.Method && request.URL.Path == action.Path {
					body, err := io.ReadAll(request.Body)
					if err != nil {
						t.Errorf("read attempted departure: %v", err)
						writer.WriteHeader(http.StatusBadRequest)
						return
					}
					uwExecServeLost(proxy.recordMutation)(writer, request, body)
					return
				}
				downstream.ServeHTTP(writer, request)
			})
			if _, err := runner.dispatch(h.ctx, mission, &data); err != nil {
				t.Fatal(err)
			}
			before := h.api.mutationsFor(action.Path)
			if len(before) == 0 {
				t.Fatal("departure recovery fixture never crossed HTTP")
			}
			if state == "completed" {
				if data.Action != nil || mission.Phase != "sailing" {
					t.Fatalf("control departure did not complete: %+v", data)
				}
				mission.Phase = "depart"
				if err := runner.saveMission(h.ctx, mission, &original); err != nil {
					t.Fatal(err)
				}
			} else {
				if len(h.pending()) != 1 {
					t.Fatalf("lost departure is not durably pending: %+v", h.pending())
				}
				h.expireCooldown(ship.Ship.ID)
			}
			loseResponse.Store(false)
			policy.DestPortID = "amsterdam"
			piracyInsuranceBoard(proxy, policy)
			restarted := h.runner(primaryTestKey)
			piracyInsuranceRefresh(t, h, restarted)
			piracyInsuranceRequirePolicies(t, restarted, policy)
			mission, data = h.mission(ship.Ship.ID)
			if _, err := restarted.dispatch(h.ctx, mission, &data); err != nil {
				t.Fatal(err)
			}
			calls := h.api.mutationsFor(action.Path)
			want := len(before)
			if state == "attempted" {
				want++
			}
			if len(calls) != want {
				t.Fatalf("conflicting policy changed %s departure recovery: %+v", state, calls)
			}
			for _, call := range calls {
				if call.IdempotencyKey != action.ID || !bytes.Equal(call.Body, action.Body) {
					t.Fatalf("departure recovery replaced the original request: %+v", call)
				}
			}
			mission, retained := h.mission(ship.Ship.ID)
			if mission.Phase != "sailing" || retained.Action != nil || retained.LastActionID != action.ID || len(h.pending()) != 0 {
				t.Fatalf("insurance blocked immutable departure recovery: phase %s data %+v", mission.Phase, retained)
			}
		})
	}
}

func TestPiracyInsuranceSourcingFundingFirstSend(t *testing.T) {
	h := newRecoveryHarness(t)
	h.api.setFleet([]map[string]any{sourcingReviewShipRow()})
	h.api.setVaultBalance("north", 0)
	h.api.setVaultBalance("south", 50_000_000)
	proxy := installUWExecProxy(t, h)
	policy := piracyInsurancePolicy()
	policy.DestPortID = "amsterdam"
	piracyInsuranceBoard(proxy, policy)
	runner := refreshedRunner(t, h)
	sourcingReviewInject(t, runner, sourcingReviewCharters())
	ship := harnessShip(t, runner, recoveryShipBlastoise)
	if changed, err := runner.considerSourcing(h.ctx, ship); err != nil || !changed {
		t.Fatalf("matching insured empty sourcing did not admit funded transfer: changed %t error %v holds %+v", changed, err, runner.holding)
	}
	mission, data := h.mission(ship.Ship.ID)
	if _, err := runner.driveMission(h.ctx, ship, mission, &data); err != nil {
		t.Fatal(err)
	}
	if data.Action == nil || data.Action.Stage != "sourcing-remit" {
		t.Fatalf("matching route did not stage its necessary real remittance: %+v holds %+v", data, runner.holding)
	}
	policy.DestPortID = recoveryFarPort
	piracyInsuranceBoard(proxy, policy)
	piracyInsuranceRefresh(t, h, runner)
	sourcingReviewInject(t, runner, sourcingReviewCharters())
	if _, err := runner.dispatch(h.ctx, mission, &data); err != nil {
		t.Fatal(err)
	}
	_, retained := h.mission(ship.Ship.ID)
	if h.api.mutationCount("/v1/treasury/remit") != 0 || retained.Action != nil || len(h.pending()) != 0 {
		t.Fatalf("first-send sourcing funding ignored conflicting insurance: %+v calls %+v", retained, h.api.mutationsFor("/v1/treasury/remit"))
	}
}

func TestPiracyInsuranceCompletedBuyPrunesUnavailableOptionalFreight(t *testing.T) {
	for _, status := range []string{"missing", "cancelled", "open"} {
		t.Run(status, func(t *testing.T) {
			h, runner, mission, data, ship := procurementRecoveryFixture(t, false)
			proxy := installUWExecProxy(t, h)
			policy := piracyInsurancePolicy()
			piracyInsuranceBoard(proxy, policy)
			proxy.contractRows = []map[string]any{}
			if status != "missing" {
				proxy.contractRows = []map[string]any{freightContractRow("optional-insured-freight", status, false)}
			}
			data.Plan.FreightIDs = []string{"optional-insured-freight"}
			mission.Phase = "freight"
			if err := runner.saveMission(h.ctx, mission, &data); err != nil {
				t.Fatal(err)
			}
			piracyInsuranceRefresh(t, h, runner)
			piracyInsuranceRequirePolicies(t, runner, policy)
			for range 4 {
				mission, data = h.mission(ship.Ship.ID)
				ship = harnessShip(t, runner, ship.Ship.ID)
				if _, err := runner.driveMission(h.ctx, ship, mission, &data); err != nil {
					t.Fatal(err)
				}
				if data.Action != nil {
					if _, err := runner.dispatch(h.ctx, mission, &data); err != nil {
						t.Fatal(err)
					}
				}
				if mission.Phase == "sailing" {
					break
				}
			}
			calls := h.api.mutationsFor("/v1/fleet/depart")
			_, retained := h.mission(ship.Ship.ID)
			if h.api.mutationCount("/v1/contract/accept-freight") != 0 ||
				retained.BoughtQuantity != 20 || !retained.BuyDone || retained.AcquisitionPence != 10_000 {
				t.Fatalf("optional offer changed purchased cargo or accepted new freight: %+v", retained)
			}
			if status == "open" {
				if len(calls) != 0 || len(retained.Plan.FreightIDs) != 1 || len(retained.Freight) != 0 {
					t.Fatalf("live optional freight bypassed loading protection: calls %+v data %+v", calls, retained)
				}
				return
			}
			if len(calls) != 1 || calls[0].Status != http.StatusOK ||
				len(retained.Plan.FreightIDs) != 0 || mission.Phase != "sailing" {
				t.Fatalf("unusable optional freight stranded a legal loaded departure: calls %+v phase %s data %+v", calls, mission.Phase, retained)
			}
		})
	}
}

func TestPiracyInsuranceBlockedCargoFallsBackToMatchingEmptySourcing(t *testing.T) {
	for _, status := range []string{"active", "settled_safe"} {
		t.Run(status, func(t *testing.T) {
			h := newRecoveryHarness(t)
			h.api.setFleet([]map[string]any{sourcingReviewShipRow()})
			h.api.setFactorPorts(recoveryFarPort)
			proxy := installUWExecProxy(t, h)
			policy := piracyInsurancePolicy()
			policy.Status = status
			policy.DestPortID = "amsterdam"
			piracyInsuranceBoard(proxy, policy)
			runner := refreshedRunner(t, h)
			// Keep the real executable grain opportunity that the shared
			// sourcing diagnostic fixture normally removes.
			grainAsks := append([]Level(nil), bookCommodity(runner.stateCache.Markets[recoveryHomePort], recoveryGrain).Depth.Asks...)
			sourcingReviewInject(t, runner, sourcingReviewCharters())
			bookCommodity(runner.stateCache.Markets[recoveryHomePort], recoveryGrain).Depth.Asks = grainAsks
			ship := harnessShip(t, runner, recoveryShipBlastoise)
			input, err := runner.committedPlanningInput(h.ctx, ship)
			if err != nil {
				t.Fatal(err)
			}
			ordinary := PlanVoyages(input)
			if len(ordinary) == 0 || !ordinary[0].Accepted {
				t.Fatalf("fixture has no otherwise profitable ordinary opportunity: %+v", ordinary)
			}
			if _, err := runner.work(h.ctx); err != nil {
				t.Fatal(err)
			}
			_, admitted := h.mission(ship.Ship.ID)
			if status == "active" {
				if admitted.Relocation == nil || admitted.Plan.Destination != "amsterdam" ||
					admitted.BoughtQuantity != 0 || admitted.Action != nil {
					t.Fatalf("insurance-ineligible ordinary cargo suppressed matching empty sourcing: %+v holds %+v", admitted, runner.holding)
				}
			} else if admitted.Relocation != nil || admitted.Plan.Quantity <= 0 {
				t.Fatalf("executable ordinary opportunity lost priority without a live loading restriction: %+v", admitted)
			}
			if h.api.mutationCount("/v1/market/place-order") != 0 || len(h.pending()) != 0 {
				t.Fatal("admission unexpectedly spent before durable execution")
			}
		})
	}
}

func piracyInsuranceCharterRow(id string, quantity, reward int64) map[string]any {
	return map[string]any{
		"id": id, "contract_type": "charter", "status": "open",
		"origin_port_id": recoveryFarPort, "commodity_id": recoveryGrain,
		"quantity_required": quantity, "remaining_quantity": quantity,
		"reward_per_ton_pence": reward, "is_public": true,
		"expires_at": time.Now().UTC().Add(7 * 24 * time.Hour),
	}
}

func TestPiracyInsuranceInspectionDoesNotFundOrHandoffBlockedLoading(t *testing.T) {
	for _, funding := range []string{"requires transfer", "already funded"} {
		t.Run(funding, func(t *testing.T) {
			h, runner, proxy, _, _, _ := piracyInsuranceLoadingFixture(t, "buy")
			secondRow := stationaryShipRow(recoveryShipCharizard, "Charizard", recoveryHomePort, "berthed", 0, 4)
			// Loading is legal during turnaround; keep the later unrelated
			// departure outside this fake's two-port navigation endpoint.
			unlocks := time.Now().UTC().Add(time.Hour)
			secondRow["ship"].(map[string]any)["turnaround_unlocks_at"] = unlocks
			h.api.mu.Lock()
			h.api.fleet = append(h.api.fleet, secondRow)
			h.api.mu.Unlock()
			proxy.mu.Lock()
			independent := piracyInsuranceCharterRow("independently-funded-charter", 40, 2_000)
			independent["origin_port_id"] = "amsterdam"
			proxy.contractRows = []map[string]any{piracyInsuranceCharterRow("inspection-target", 40, 1_000), independent}
			proxy.mu.Unlock()
			if funding == "requires transfer" {
				// Only the inspection target lacks its destination reserve.
				// The other hull has an independent north-region delivery.
				h.api.setVaultBalance("south", 0)
			}
			downstream := h.api.server.Config.Handler
			h.api.server.Config.Handler = http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
				if request.Method == http.MethodGet && request.URL.Path == "/v1/world/catalog" {
					catalog := h.api.catalogDocument()
					catalog["ports"] = append(catalog["ports"].([]any), map[string]any{
						"id": "amsterdam", "name": "Amsterdam", "banking_region": "north",
						"sea_zone_id": "zone-north", "phase": 1, "customs_bps": 200,
						"base_berths": 10, "effective_berths": 10, "imports": []any{recoveryGrain},
					})
					h.api.writeJSON(writer, http.StatusOK, catalog)
					return
				}
				if funding == "requires transfer" && request.Method == http.MethodGet &&
					request.URL.Path == "/v1/world/port" && request.URL.Query().Get("port_id") == recoveryFarPort {
					// A zero-dues destination makes the measured transfer's
					// reserve sufficient without unrelated harbour liabilities.
					detail := h.api.portDocument(recoveryFarPort)
					detail["berth_dues_per_day_pence"] = 0
					h.api.writeJSON(writer, http.StatusOK, detail)
					return
				}
				downstream.ServeHTTP(writer, request)
			})
			policy := piracyInsurancePolicy()
			piracyInsuranceBoard(proxy, policy)
			piracyInsuranceRefresh(t, h, runner)
			ship := harnessShip(t, runner, recoveryShipBlastoise)
			input, err := runner.sourcingInput(h.ctx, ship)
			if err != nil {
				t.Fatal(err)
			}
			contract, exists := runner.contract("inspection-target")
			if !exists {
				t.Fatal("inspection fixture did not capture its charter")
			}
			probe := Plan{
				Kind: "relocation", ShipID: ship.Ship.ID, Origin: recoveryFarPort,
				Destination: recoveryHomePort, Commodity: recoveryGrain, ContractID: contract.ID,
				TargetStoresTons: 4, Economics: Economics{GameDays: 0.25},
			}
			candidate := sizeRelocationCargo(input, probe, contract, 0)
			if !candidate.Accepted {
				t.Fatalf("inspection fixture offers no profitable fresh cargo: %+v", candidate)
			}
			_, _, transfers, funded := measuredFunding(input.Balances, cargoFundingRequired(input, candidate), input.Rates)
			if !funded || (funding == "requires transfer") != (len(transfers) > 0) {
				t.Fatalf("inspection fixture missed its funding boundary: funded %t transfers %+v candidate %+v", funded, transfers, candidate)
			}
			h.putMission(ship.Ship.ID, "insured-source-inspection", "inspect", missionData{
				Plan: probe, CostsKnown: true, FinishAtDestination: true,
				InitialLedgerIDs: map[string]bool{}, Eligible: map[string]int64{},
				Relocation: &relocationIntent{
					TargetContractID: contract.ID, TargetCommodity: recoveryGrain,
					TargetDestination: recoveryFarPort, ExpenseCapPence: 20_000,
					InspectUntilGameHour: runner.stateCache.Clock.Calendar.TotalGameHours + 6,
					StoreBasis:           &relocationStoreBasis{InitialTons: ship.Stores.ProvisionsTons, ReplacementPencePerTon: 51},
				},
			})
			secondInput, err := runner.committedPlanningInput(h.ctx, harnessShip(t, runner, recoveryShipCharizard))
			if err != nil {
				t.Fatal(err)
			}
			secondPlans := PlanVoyages(secondInput)
			if len(secondPlans) == 0 || !secondPlans[0].Accepted || secondPlans[0].Destination != "amsterdam" {
				t.Fatalf("second hull has no independently funded legal trade: %+v", secondPlans)
			}
			badTransition := false
			for range 10 {
				if _, err := h.tick(runner); err != nil {
					t.Fatal(err)
				}
				mission, err := h.db.GetMission(h.ctx, h.account.ID, ship.Ship.ID)
				if err != nil {
					t.Fatal(err)
				}
				if mission == nil {
					continue
				}
				var retained missionData
				if err := json.Unmarshal(mission.Data, &retained); err != nil {
					t.Fatal(err)
				}
				badTransition = badTransition || retained.SourcingHandoff != nil ||
					retained.Action != nil && (retained.Action.Stage == "sourcing-remit" || retained.Action.Stage == "buy")
			}
			if badTransition || h.api.mutationCount("/v1/treasury/remit") != 0 {
				t.Errorf("insured source inspection repeatedly funded or promoted forbidden loading: remit %+v", h.api.mutationsFor("/v1/treasury/remit"))
			}
			secondBought := false
			for _, call := range h.api.mutationsFor("/v1/market/place-order") {
				var request struct {
					ShipID     string `json:"ship_id"`
					IntoStores bool   `json:"into_stores"`
				}
				if err := json.Unmarshal(call.Body, &request); err != nil {
					t.Fatal(err)
				}
				if request.ShipID == ship.Ship.ID && !request.IntoStores {
					t.Fatalf("insured inspection bought cargo: %+v", call)
				}
				secondBought = secondBought || request.ShipID == recoveryShipCharizard && !request.IntoStores && call.Status == http.StatusOK
			}
			if !secondBought {
				t.Fatalf("blocked source inspection starved the second hull: calls %+v holds %+v", h.api.mutationsFor("/v1/market/place-order"), runner.holding)
			}
			_, second := h.mission(recoveryShipCharizard)
			if !second.BuyDone || second.BoughtQuantity <= 0 {
				t.Fatalf("second hull's inspection-adjacent trade was not durable: %+v", second)
			}
		})
	}
}

// Serve finite source depth while retaining the shared fake's real purchase
// receipts, cargo changes and cash debits. No test changes the journal payload.
func piracyInsuranceFiniteSource(h *recoveryHarness, initial int64) *atomic.Int64 {
	remaining := &atomic.Int64{}
	remaining.Store(initial)
	downstream := h.api.server.Config.Handler
	h.api.server.Config.Handler = http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		if request.Method == http.MethodGet && request.URL.Path == "/v1/market/book" && request.URL.Query().Get("port_id") == recoveryHomePort {
			book := h.api.bookDocument(recoveryHomePort)
			for _, raw := range book["books"].([]any) {
				entry := raw.(map[string]any)
				if entry["commodity_id"] == recoveryGrain {
					entry["depth"].(map[string]any)["asks"] = []any{map[string]any{
						"order_id": "finite-insurance-source", "price_pence": 100,
						"remaining_quantity": remaining.Load(),
					}}
				}
			}
			h.api.writeJSON(writer, http.StatusOK, book)
			return
		}
		if request.Method != http.MethodPost || request.URL.Path != "/v1/market/place-order" {
			downstream.ServeHTTP(writer, request)
			return
		}
		body, err := io.ReadAll(request.Body)
		if err != nil {
			h.t.Errorf("read finite-source request: %v", err)
			writer.WriteHeader(http.StatusBadRequest)
			return
		}
		request.Body = io.NopCloser(bytes.NewReader(body))
		var order struct {
			Commodity  string `json:"commodity_id"`
			Side       string `json:"side"`
			Quantity   int64  `json:"quantity"`
			IntoStores bool   `json:"into_stores"`
		}
		if err := json.Unmarshal(body, &order); err != nil {
			h.t.Errorf("decode finite-source request: %v", err)
			writer.WriteHeader(http.StatusBadRequest)
			return
		}
		cargoBid := order.Commodity == recoveryGrain && order.Side == "bid" && !order.IntoStores
		if cargoBid && order.Quantity > remaining.Load() {
			h.api.mu.Lock()
			h.api.record(request, body, http.StatusConflict)
			h.api.mu.Unlock()
			h.api.writeProblem(writer, http.StatusConflict, "insufficient_observed_stock", "the finite test book cannot fill this quantity")
			return
		}
		before := h.api.mutationCount(request.URL.Path)
		downstream.ServeHTTP(writer, request)
		calls := h.api.mutationsFor(request.URL.Path)
		if cargoBid && len(calls) > before && calls[len(calls)-1].Status == http.StatusOK {
			remaining.Add(-order.Quantity)
		}
	})
	return remaining
}

func piracyInsuranceReservationFixture(t *testing.T) (*recoveryHarness, *Runner, *uwExecProxy, Plan) {
	t.Helper()
	h := newRecoveryHarness(t)
	h.api.setFactorPorts(recoveryFarPort)
	h.api.setFleet([]map[string]any{
		stationaryShipRow(recoveryShipBlastoise, "Blastoise", recoveryHomePort, "berthed", 0, 4),
		stationaryShipRow(recoveryShipCharizard, "Charizard", recoveryHomePort, "berthed", 0, 4),
	})
	proxy := installUWExecProxy(t, h)
	proxy.contractRows = []map[string]any{piracyInsuranceCharterRow("shared-insurance-charter", 40, 500)}
	piracyInsuranceBoard(proxy, piracyInsurancePolicy())
	piracyInsuranceFiniteSource(h, 40)
	runner := refreshedRunner(t, h)
	plan := Plan{
		Kind: "charter", ShipID: recoveryShipBlastoise, Origin: recoveryHomePort,
		Destination: recoveryFarPort, Commodity: recoveryGrain, ContractID: "shared-insurance-charter",
		Quantity: 20, BuyLimitPence: 100, Accepted: true, TargetStoresTons: 4,
		Economics: Economics{PurchasePence: 2_000, RevenuePence: 10_000, ExpectedNetPence: 8_000, GameDays: 1},
	}
	return h, runner, proxy, plan
}

func TestPiracyInsuranceOnlyHypotheticalLoadingReservationsSuspend(t *testing.T) {
	for _, boundary := range []string{"unattempted", "paid stores", "staged action", "pending action", "purchased cargo", "accepted freight", "empty relocation", "return bundle", "stale ships", "stale contracts"} {
		t.Run(boundary, func(t *testing.T) {
			h, runner, proxy, plan := piracyInsuranceReservationFixture(t)
			other := harnessShip(t, runner, recoveryShipCharizard)
			baseline, err := runner.committedPlanningInput(h.ctx, other)
			if err != nil {
				t.Fatal(err)
			}
			data := missionData{Plan: plan, CostsKnown: true, InitialLedgerIDs: map[string]bool{}, Eligible: map[string]int64{}}
			phase := "buy"
			suspended := boundary == "unattempted" || boundary == "paid stores"
			switch boundary {
			case "paid stores":
				destinationSeedAction(t, h, "insured-paid-stores", "/v1/market/place-order",
					fmt.Sprintf(`{"ship_id":%q,"port_id":%q,"commodity_id":"provisions","side":"bid","quantity":1,"price_pence":50,"time_in_force":"ioc","into_stores":true}`, plan.ShipID, plan.Origin),
					`{"commodity_id":"provisions","filled_quantity":1,"total_gross_pence":50,"customs_duty_pence":1}`)
				data.LastActionID = "insured-paid-stores"
				data.ProvisionsPence = 51
			case "purchased cargo":
				data.BuyDone = true
				data.BoughtQuantity = plan.Quantity
				data.AcquisitionPence = 2_000
				phase = "freight"
				h.api.mu.Lock()
				h.api.fleet[0]["cargo_manifest"] = map[string]any{recoveryGrain: plan.Quantity}
				h.api.fleet[0]["cargo_used_tons"] = plan.Quantity
				h.api.mu.Unlock()
			case "accepted freight":
				freight := freightContractRow("held-actual-seal", "active", false)
				freight["quantity_required"] = int64(5)
				freight["remaining_quantity"] = int64(5)
				proxy.mu.Lock()
				proxy.contractRows = append(proxy.contractRows, freight)
				proxy.mu.Unlock()
				deadline := time.Now().UTC().Add(6 * time.Hour)
				data.Freight = []freightObligation{{ContractID: "held-actual-seal", Commodity: recoveryGrain, Quantity: 5, Destination: recoveryFarPort, Deadline: &deadline, CollateralPence: 5_000}}
				h.api.mu.Lock()
				h.api.fleet[0]["cargo_manifest"] = map[string]any{recoveryGrain: int64(5)}
				h.api.fleet[0]["cargo_used_tons"] = int64(5)
				h.api.mu.Unlock()
			case "empty relocation":
				data.Plan.Kind = "relocation"
				data.Plan.Quantity = 0
				data.Plan.Economics.StoreCashPence = 500
				data.Relocation = &relocationIntent{TargetContractID: plan.ContractID, TargetCommodity: recoveryGrain, TargetDestination: recoveryFarPort, ExpenseCapPence: 20_000}
				phase = "provision"
			case "return bundle":
				next := plan
				data.BuyDone = true
				data.BoughtQuantity = plan.Quantity
				data.DeliveredQuantity = plan.Quantity
				data.ReturnJourney = true
				data.GrowthLane = &growthLaneIntent{NextPlan: &next}
				phase = "return"
			}
			h.putMission(plan.ShipID, "insured-reservation-"+boundary, phase, data)
			piracyInsuranceRefresh(t, h, runner)
			mission, data := h.mission(plan.ShipID)
			if boundary == "stale ships" || boundary == "stale contracts" {
				dataset := strings.TrimPrefix(boundary, "stale ")
				meta := runner.meta[dataset]
				meta.Stale = true
				runner.meta[dataset] = meta
			}
			if boundary == "staged action" || boundary == "pending action" {
				ship := harnessShip(t, runner, plan.ShipID)
				if err := runner.stage(h.ctx, mission, &data, ship, "buy", "/v1/market/place-order", orderBody(ship, recoveryGrain, "bid", plan.Quantity, 100, false)); err != nil {
					t.Fatal(err)
				}
				if boundary == "pending action" {
					action := data.Action
					if _, err := h.db.BeginAction(h.ctx, &store.Action{ID: action.ID, AccountID: h.account.ID, Method: action.Method, Path: action.Path, Body: action.Body}); err != nil {
						t.Fatal(err)
					}
				}
			} else if suspended {
				if _, err := runner.driveMission(h.ctx, harnessShip(t, runner, plan.ShipID), mission, &data); err != nil {
					t.Fatal(err)
				}
			}
			other = harnessShip(t, runner, recoveryShipCharizard)
			available, err := runner.committedPlanningInput(h.ctx, other)
			if err != nil {
				t.Fatal(err)
			}
			source := bookCommodity(available.Markets[recoveryHomePort], recoveryGrain)
			if source == nil || source.Depth == nil {
				t.Fatal("reservation projection lost source market")
			}
			var asks int64
			for _, level := range source.Depth.Asks {
				asks += level.RemainingQuantity
			}
			var demand int64
			for _, contract := range available.Contracts {
				if contract.ID == plan.ContractID {
					demand = contract.RemainingQuantity
				}
			}
			if suspended {
				if asks != 40 || demand != 40 || !bytes.Equal(encode(available.ReserveByRegion), encode(baseline.ReserveByRegion)) {
					t.Fatalf("insurance hold retained hypothetical source/cash/charter commitments: asks %d demand %d reserves %v baseline %v", asks, demand, available.ReserveByRegion, baseline.ReserveByRegion)
				}
			} else if boundary == "empty relocation" {
				if available.ReserveByRegion["north"] <= baseline.ReserveByRegion["north"] {
					t.Fatal("matching executable empty relocation lost its funding reservation")
				}
			} else {
				if demand != 20 || boundary != "purchased cargo" && asks != 20 {
					t.Fatalf("insurance erased a real/still-unresolved commitment: asks %d demand %d data %+v", asks, demand, data)
				}
				if boundary != "purchased cargo" && available.ReserveByRegion["north"] <= baseline.ReserveByRegion["north"] {
					t.Fatalf("real/still-unresolved commitment lost reserved cash: %+v", data)
				}
			}
			_, retained := h.mission(plan.ShipID)
			if retained.BoughtQuantity != data.BoughtQuantity || len(retained.Freight) != len(data.Freight) ||
				retained.ProvisionsPence != data.ProvisionsPence || retained.LastActionID != data.LastActionID {
				t.Fatalf("reservation suspension rewrote actual accounting/obligations: %+v", retained)
			}
		})
	}
}

func TestPiracyInsuranceSuspendedReservationLetsOtherHullTradeBeforeRelease(t *testing.T) {
	h, runner, proxy, plan := piracyInsuranceReservationFixture(t)
	baseline, err := runner.committedPlanningInput(h.ctx, harnessShip(t, runner, recoveryShipCharizard))
	if err != nil {
		t.Fatal(err)
	}
	hull, hullExists := findHull(baseline.Catalog, baseline.Ship.Ship.ShipClass)
	origin, originExists := findPort(baseline.Catalog, recoveryHomePort)
	if !hullExists || !originExists {
		t.Fatal("finite-source fixture is missing its observed hull or origin")
	}
	lot := plan
	lot.ShipID = baseline.Ship.Ship.ID
	lot = evaluatePlan(baseline, hull, origin, lot, 10_000, 0)
	if !lot.Accepted || lot.Quantity != 20 {
		t.Fatalf("finite-source fixture has no fully costed 20t control lot: %+v", lot)
	}
	// Fund both actual 20t purchases plus the planner's still-prospective
	// whole-voyage fleet bill, berth bill and conservative customs bound.
	fleetBill := int64(float64(baseline.FleetDailyUpkeepPence)*lot.Economics.GameDays) + 1
	initialCash := baseline.ReserveByRegion["north"] + 4_080 + fleetBill + lot.Economics.BerthDuesPence + lot.Quantity
	h.api.setVaultBalance("north", initialCash)
	piracyInsuranceRefresh(t, h, runner)
	plan.Quantity = 30
	plan.Economics.PurchasePence = 3_000
	h.putMission(plan.ShipID, "insured-empty-future-buy", "buy", missionData{
		Plan: plan, CostsKnown: true, InitialLedgerIDs: map[string]bool{}, Eligible: map[string]int64{},
	})
	first, firstData := h.mission(plan.ShipID)
	if _, err := runner.driveMission(h.ctx, harnessShip(t, runner, plan.ShipID), first, &firstData); err != nil {
		t.Fatal(err)
	}
	other := harnessShip(t, runner, recoveryShipCharizard)
	input, err := runner.committedPlanningInput(h.ctx, other)
	if err != nil {
		t.Fatal(err)
	}
	plans := PlanVoyages(input)
	if len(plans) == 0 || !plans[0].Accepted {
		t.Fatalf("insurance-held future buy suppressed the other hull's fresh trade: %+v", plans)
	}
	if plans[0].Quantity < 20 {
		t.Fatalf("hypothetical insured reservation still prevents the other hull from using the shared stock: %+v", plans[0])
	}
	otherPlan := plans[0]
	otherPlan.Quantity = 20
	otherPlan.ShipID = other.Ship.ID
	if _, err := runner.startVoyage(h.ctx, other, otherPlan); err != nil {
		t.Fatal(err)
	}
	for range 3 {
		mission, data := h.mission(other.Ship.ID)
		if _, err := runner.driveMission(h.ctx, other, mission, &data); err != nil {
			t.Fatal(err)
		}
	}
	_, bought := h.mission(other.Ship.ID)
	if !bought.BuyDone || bought.BoughtQuantity != 20 {
		t.Fatalf("other hull did not complete its competing 20t finite-source purchase: %+v", bought)
	}
	released := piracyInsurancePolicy()
	released.Status = "settled_safe"
	piracyInsuranceBoard(proxy, released)
	piracyInsuranceRefresh(t, h, runner)
	first, firstData = h.mission(plan.ShipID)
	if firstData.Action != nil || firstData.BoughtQuantity != 0 || len(firstData.Freight) != 0 {
		t.Fatalf("held mission unexpectedly gained an irreversible commitment: %+v", firstData)
	}
	firstShip := harnessShip(t, runner, plan.ShipID)
	residual, err := runner.committedPlanningInput(h.ctx, firstShip)
	if err != nil {
		t.Fatal(err)
	}
	fresh := PlanVoyages(residual)
	if len(fresh) == 0 || !fresh[0].Accepted || fresh[0].Quantity != 20 {
		t.Fatalf("policy release did not expose a freshly affordable residual 20t lot: %+v", fresh)
	}
	// Re-admit the never-attempted intent through the shared planner and
	// voyage boundary, rather than replaying its obsolete 30t proposal.
	if _, err := runner.startVoyage(h.ctx, firstShip, fresh[0]); err != nil {
		t.Fatal(err)
	}
	for range 3 {
		first, firstData = h.mission(plan.ShipID)
		if _, err := runner.driveMission(h.ctx, firstShip, first, &firstData); err != nil {
			t.Fatal(err)
		}
	}
	_, resumed := h.mission(plan.ShipID)
	if !resumed.BuyDone || resumed.BoughtQuantity != 20 {
		t.Fatalf("released hold did not purchase the freshly admitted residual lot: %+v", resumed)
	}
	var purchased int64
	for _, call := range h.api.mutationsFor("/v1/market/place-order") {
		var order struct {
			Commodity  string `json:"commodity_id"`
			Quantity   int64  `json:"quantity"`
			IntoStores bool   `json:"into_stores"`
		}
		if err := json.Unmarshal(call.Body, &order); err != nil {
			t.Fatal(err)
		}
		if order.Commodity == recoveryGrain && !order.IntoStores {
			if call.Status != http.StatusOK {
				t.Fatalf("resumed mission tried to overspend finite liquidity: %+v", call)
			}
			purchased += order.Quantity
		}
	}
	if purchased != 40 {
		t.Fatalf("combined hull purchases exceeded or lost finite stock: %dt", purchased)
	}
	h.api.mu.Lock()
	cash := h.api.vaultBalance["north"]
	h.api.mu.Unlock()
	if cash < baseline.ReserveByRegion["north"] || cash != initialCash-4_080 {
		t.Fatalf("released mission lost exact purchase/customs cash or spent protected operating funds: initial %dp final %dp reserve %dp", initialCash, cash, baseline.ReserveByRegion["north"])
	}
}

func TestPiracyInsuranceHeldFutureDestinationDoesNotCreateTreasuryObligation(t *testing.T) {
	h := newRecoveryHarness(t)
	h.api.setFleet([]map[string]any{stationaryShipRow(recoveryShipBlastoise, "Blastoise", recoveryHomePort, "berthed", 0, 4)})
	proxy := installUWExecProxy(t, h)
	piracyInsuranceBoard(proxy, piracyInsurancePolicy())
	runner := refreshedRunner(t, h)
	before, err := runner.treasuryReserves(h.ctx)
	if err != nil {
		t.Fatal(err)
	}
	h.putMission(recoveryShipBlastoise, "held-hypothetical-region", "buy", missionData{
		Plan:       Plan{Kind: "spot", ShipID: recoveryShipBlastoise, Origin: recoveryHomePort, Destination: recoveryFarPort, Commodity: recoveryGrain, Quantity: 10, BuyLimitPence: 100},
		CostsKnown: true,
	})
	mission, data := h.mission(recoveryShipBlastoise)
	if _, err := runner.driveMission(h.ctx, harnessShip(t, runner, recoveryShipBlastoise), mission, &data); err != nil {
		t.Fatal(err)
	}
	after, err := runner.treasuryReserves(h.ctx)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(encode(before), encode(after)) {
		t.Fatalf("unattempted insurance-held destination created hypothetical treasury bills: %v -> %v", before, after)
	}
}

func TestPiracyInsuranceHeldGrowthCargoDoesNotStageReplacementFunding(t *testing.T) {
	w, runner := newGrowthExecutionWorld(t)
	mission, data := w.until(runner, func(mission *store.Mission, data missionData) bool {
		return mission != nil && data.Action != nil && data.Action.Stage == "buy"
	})
	proxy := installUWExecProxy(t, w.h)
	policy := piracyInsurancePolicy()
	policy.ShipID = recoveryBuiltShipID
	piracyInsuranceBoard(proxy, policy)
	piracyInsuranceRefresh(t, w.h, runner)
	piracyInsuranceRequirePolicies(t, runner, policy)
	// This was staged before the policy observation, but has never crossed
	// transport. Let the existing first-send boundary detach it normally.
	if _, err := runner.dispatch(w.h.ctx, mission, &data); err != nil {
		t.Fatal(err)
	}
	mission, data = w.h.mission(recoveryBuiltShipID)
	if data.Action != nil || data.BuyDone || data.BoughtQuantity != 0 {
		t.Fatalf("fixture did not retain only unattempted growth loading: %+v", data)
	}
	w.h.api.setVaultBalance("north", runner.opts.ReservePence+10)
	w.h.api.setVaultBalance("south", 50_000_000)
	piracyInsuranceRefresh(t, w.h, runner)
	before := w.h.api.mutationCount("/v1/treasury/remit")
	ship := harnessShip(t, runner, recoveryBuiltShipID)
	for range 2 {
		mission, data = w.h.mission(recoveryBuiltShipID)
		if _, err := runner.driveMission(w.h.ctx, ship, mission, &data); err != nil {
			t.Fatal(err)
		}
		if data.Action != nil {
			t.Fatalf("growth repricing staged new funding/spending before the cargo-loading hold: %+v", data.Action)
		}
	}
	if w.h.api.mutationCount("/v1/treasury/remit") != before {
		t.Fatal("held growth cargo caused new remittance spending")
	}
	for _, call := range w.h.api.mutationsFor("/v1/market/place-order") {
		var order growthExecutionRequest
		if err := json.Unmarshal(call.Body, &order); err != nil {
			t.Fatal(err)
		}
		if order.ShipID == recoveryBuiltShipID && order.Side == "bid" && !order.IntoStores {
			t.Fatalf("held growth cargo reached market transport: %+v", call)
		}
	}
}

func TestPiracyInsuranceCommissioningKeepsAdmittedCapitalReserved(t *testing.T) {
	w, runner := newGrowthExecutionWorld(t)
	w.until(runner, func(_ *store.Mission, _ missionData) bool {
		mission, err := w.h.db.GetMission(w.h.ctx, w.h.account.ID, "__growth")
		if err != nil {
			t.Fatal(err)
		}
		return mission != nil && mission.Phase == "commissioning"
	})
	incumbent := harnessShip(t, runner, recoveryShipBlastoise)
	before, err := runner.committedPlanningInput(w.h.ctx, incumbent)
	if err != nil {
		t.Fatal(err)
	}
	proxy := installUWExecProxy(t, w.h)
	policy := piracyInsurancePolicy()
	policy.ShipID = recoveryBuiltShipID
	piracyInsuranceBoard(proxy, policy)
	piracyInsuranceRefresh(t, w.h, runner)
	piracyInsuranceRequirePolicies(t, runner, policy)
	incumbent = harnessShip(t, runner, recoveryShipBlastoise)
	after, err := runner.committedPlanningInput(w.h.ctx, incumbent)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(encode(before.ReserveByRegion), encode(after.ReserveByRegion)) {
		t.Fatalf("insurance observation released commissioning's admitted capital: %v -> %v", before.ReserveByRegion, after.ReserveByRegion)
	}
	for _, port := range []string{recoveryHomePort, recoveryFarPort} {
		beforeBook := bookCommodity(before.Markets[port], recoveryGrain)
		afterBook := bookCommodity(after.Markets[port], recoveryGrain)
		if !bytes.Equal(encode(beforeBook), encode(afterBook)) {
			t.Fatalf("insurance observation released commissioning's finite market commitment at %s: before %+v after %+v", port, beforeBook, afterBook)
		}
	}
}

func TestPiracyInsuranceSourcingSelectsHighestRankedPolicyCompatibleSource(t *testing.T) {
	for _, scenario := range []string{"second-ranked source", "third-ranked source", "stale insurance", "conflicting live policies"} {
		t.Run(scenario, func(t *testing.T) {
			h := newRecoveryHarness(t)
			h.api.setFleet([]map[string]any{sourcingReviewShipRow()})
			proxy := installUWExecProxy(t, h)
			proxy.contractRows = []map[string]any{{
				"id": "ranked-insured-source-charter", "contract_type": "charter", "status": "open",
				"origin_port_id": recoveryFarPort, "commodity_id": "sawn_planks",
				"quantity_required": 1_000, "remaining_quantity": 1_000,
				"reward_per_ton_pence": 3_000, "is_public": true,
				"expires_at": time.Now().UTC().Add(30 * 24 * time.Hour),
			}}
			policy := piracyInsurancePolicy()
			policy.DestPortID = "rotterdam"
			if scenario == "third-ranked source" {
				policy.DestPortID = "antwerp"
			}
			second := policy
			second.ID = "bbbbbbbb-bbbb-4bbb-8bbb-bbbbbbbbbbbb"
			second.Status = "open"
			if scenario == "conflicting live policies" {
				second.DestPortID = "antwerp"
			}
			policies := []InsurancePolicy{policy, second}
			piracyInsuranceBoard(proxy, policies...)
			distances := map[string]int64{"amsterdam": 24, "rotterdam": 48, "antwerp": 72}
			var boardUnavailable atomic.Bool
			downstream := h.api.server.Config.Handler
			h.api.server.Config.Handler = http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
				if request.Method == http.MethodGet {
					switch request.URL.Path {
					case "/v1/insurance/list":
						if boardUnavailable.Load() && request.URL.Query().Get("underwriter_company_id") == "" {
							h.api.writeProblem(writer, http.StatusForbidden, "board_unavailable", "synthetic failed insurance observation")
							return
						}
					case "/v1/world/catalog":
						catalog := h.api.catalogDocument()
						for _, source := range []string{"amsterdam", "rotterdam", "antwerp"} {
							catalog["ports"] = append(catalog["ports"].([]any), map[string]any{
								"id": source, "name": source, "banking_region": "north",
								"sea_zone_id": "zone-north", "phase": 1, "customs_bps": 200,
								"base_berths": 10, "effective_berths": 10,
								"manufactures": []any{"sawn_planks"},
							})
						}
						h.api.writeJSON(writer, http.StatusOK, catalog)
						return
					case "/v1/world/route":
						from, to := request.URL.Query().Get("from"), request.URL.Query().Get("to")
						distance := distances[from]
						if distance == 0 {
							distance = distances[to]
						}
						if distance != 0 {
							route := h.api.routeDocument(from, to)
							route["total_distance_nm"] = distance
							route["legs"].([]any)[0].(map[string]any)["distance_nm"] = distance
							h.api.writeJSON(writer, http.StatusOK, route)
							return
						}
					case "/v1/market/book":
						if request.URL.Query().Get("port_id") == recoveryHomePort {
							book := h.api.bookDocument(recoveryHomePort)
							entries := []any{}
							for _, raw := range book["books"].([]any) {
								if raw.(map[string]any)["commodity_id"] == "provisions" {
									entries = append(entries, raw)
								}
							}
							book["books"] = entries
							h.api.writeJSON(writer, http.StatusOK, book)
							return
						}
					}
				}
				downstream.ServeHTTP(writer, request)
			})
			runner := refreshedRunner(t, h)
			piracyInsuranceRequirePolicies(t, runner, policies...)
			if scenario == "stale insurance" {
				boardUnavailable.Store(true)
				piracyInsuranceRefresh(t, h, runner)
				if runner.fresh("insurance") || len(decode[[]InsurancePolicy](runner.snapshot, "insurance")) != len(policies) {
					t.Fatalf("failed board observation did not retain stale known own policies: metadata %+v policies %+v",
						runner.meta["insurance"], decode[[]InsurancePolicy](runner.snapshot, "insurance"))
				}
			}
			ship := harnessShip(t, runner, recoveryShipBlastoise)
			input, err := runner.sourcingInput(h.ctx, ship)
			if err != nil {
				t.Fatal(err)
			}
			accepted := acceptedRelocations(planSourcingRelocation(input))
			sources := make(map[string]bool)
			for _, candidate := range accepted {
				sources[candidate.Destination] = true
			}
			if len(accepted) < 2 || len(sources) < 2 || accepted[0].Destination != "amsterdam" {
				t.Fatalf("fresh observed fixture lacks distinct ranked executable source alternatives: %+v", accepted)
			}
			var eligible *Plan
			eligibleRank := -1
			for index := range accepted {
				compatible := true
				for _, observed := range policies {
					compatible = compatible && observed.OriginPortID == ship.Ship.CurrentPortID &&
						observed.DestPortID == accepted[index].Destination
				}
				if compatible {
					candidate := accepted[index]
					eligible = &candidate
					eligibleRank = index
					break
				}
			}
			wantIntent := scenario == "second-ranked source" || scenario == "third-ranked source"
			if wantIntent && (eligible == nil || eligibleRank <= 0) {
				t.Fatalf("fixture does not place an all-policy-compatible source below the conflicting leader: policies %+v plans %+v", policies, accepted)
			}
			if scenario == "conflicting live policies" && eligible != nil {
				t.Fatalf("no-eligible control accidentally permits a source under every live policy: %+v", eligible)
			}
			before, err := runner.treasuryReserves(h.ctx)
			if err != nil {
				t.Fatal(err)
			}
			changed, err := runner.considerSourcing(h.ctx, ship)
			if err != nil {
				t.Fatal(err)
			}
			if !wantIntent {
				h.missingMission(ship.Ship.ID)
				after, err := runner.treasuryReserves(h.ctx)
				if err != nil {
					t.Fatal(err)
				}
				held := false
				for _, hold := range runner.holding {
					held = held || hold.ShipID == ship.Ship.ID
				}
				if changed || !held || !bytes.Equal(encode(before), encode(after)) {
					t.Fatalf("ineligible/stale insurance did not retain a nonfunding ship hold: changed %t held %t reserves %v -> %v", changed, held, before, after)
				}
			} else {
				if !changed {
					t.Fatalf("conflicting highest-ranked source suppressed a lower-ranked legal insured relocation: plans %+v policies %+v holds %+v", accepted, policies, runner.holding)
				}
				mission, data := h.mission(ship.Ship.ID)
				if mission.Phase != "sourcing_fund" || mission.Destination != eligible.Destination ||
					data.Plan.Destination != eligible.Destination || data.Plan.ContractID != eligible.ContractID ||
					data.Relocation == nil || data.Relocation.TargetContractID != eligible.ContractID ||
					data.BoughtQuantity != 0 || data.Action != nil {
					t.Fatalf("admission did not persist the highest-ranked source compatible with every policy: expected %+v mission %+v data %+v", eligible, mission, data)
				}
			}
			if h.api.mutationCount("/v1/treasury/remit") != 0 || h.api.mutationCount("/v1/market/place-order") != 0 ||
				h.api.mutationCount("/v1/fleet/depart") != 0 || len(h.pending()) != 0 {
				t.Fatal("ranked sourcing admission sent funding/navigation or loaded cargo before its durable execution stage")
			}
		})
	}
}
