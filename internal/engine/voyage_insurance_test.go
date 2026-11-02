package engine

import (
	"encoding/json"
	"io"
	"net/http"
	"strconv"
	"testing"
	"time"

	"github.com/DevYukine/go-tradewinds/internal/gameapi"
	"github.com/DevYukine/go-tradewinds/internal/store"
	"github.com/google/uuid"
)

func voyageCoverTestQuote() gameapi.LloydsQuoteResult {
	return gameapi.LloydsQuoteResult{ShipID: recoveryShipBlastoise, OriginPortID: recoveryHomePort, DestPortID: recoveryFarPort,
		InsuredValuePence: 1_000_000, HullValuePence: 900_000, MaxInsurablePence: 1_000_000, LloydsWrites: true,
		LloydsExpectedLossBPS: new(int64(50)), LloydsExpectedLossPence: new(int64(5_000)), LloydsPremiumBPS: new(int64(100)), LloydsPremiumPence: new(int64(10_000))}
}

func TestVoyageInsuranceOnlyOceanicAtTwiceExpectedLoss(t *testing.T) {
	hull := Hull{ID: "barque", HullCategory: "oceanic"}
	route := Route{FromPort: recoveryHomePort, ToPort: recoveryFarPort, Legs: []RouteLeg{{FromNode: "origin", ToNode: "destination", EdgeClass: "oceanic"}}}
	quote := voyageCoverTestQuote()
	if !worthwhileVoyageCover(hull, route, nil, quote) {
		t.Fatal("oceanic offer exactly at twice EL was refused")
	}
	quote.LloydsPremiumPence = new(int64(10_001))
	if worthwhileVoyageCover(hull, route, nil, quote) {
		t.Fatal("one penny above twice EL was purchased")
	}
	quote = voyageCoverTestQuote()
	route.Legs[0].EdgeClass = "coastal"
	if worthwhileVoyageCover(hull, route, nil, quote) {
		t.Fatal("coastal hop bought voyage insurance")
	}
	zones := []Zone{{ID: "destination", ZoneClass: "oceanic"}}
	if !worthwhileVoyageCover(hull, route, zones, quote) {
		t.Fatal("oceanic arrival water was mistaken for a coastal hop")
	}
	hull.HullCategory = "regional"
	if worthwhileVoyageCover(hull, route, zones, quote) {
		t.Fatal("under-rated hull was admitted by buying cover")
	}
	hull.HullCategory = "oceanic"
	for _, mutate := range []func(*gameapi.LloydsQuoteResult){
		func(q *gameapi.LloydsQuoteResult) { q.LloydsExpectedLossPence = nil },
		func(q *gameapi.LloydsQuoteResult) { q.LloydsPremiumPence = nil },
		func(q *gameapi.LloydsQuoteResult) { q.LloydsWrites = false },
		func(q *gameapi.LloydsQuoteResult) { q.PricedConsortShipIDs = []string{"absent-consort"} },
		func(q *gameapi.LloydsQuoteResult) { q.InsuredValuePence-- },
	} {
		quote = voyageCoverTestQuote()
		mutate(&quote)
		if worthwhileVoyageCover(hull, route, zones, quote) {
			t.Fatalf("incomplete or voidable declaration was purchased: %+v", quote)
		}
	}
}

func voyageInsuranceRefresh(t *testing.T, h *recoveryHarness, runner *Runner) (ShipState, Hull, Route) {
	t.Helper()
	loanCapacityRefresh(t, h, runner)
	runner.stateCache.Catalog.ShipClasses[0].HullCategory = "oceanic"
	key := routeKey(recoveryHomePort, recoveryFarPort, recoveryHull)
	route := runner.stateCache.Routes[key]
	for index := range route.Legs {
		route.Legs[index].EdgeClass = "oceanic"
	}
	runner.stateCache.Routes[key] = route
	ship, exists := runner.ship(recoveryShipBlastoise)
	if !exists {
		t.Fatal("fixture ship missing")
	}
	return ship, runner.stateCache.Catalog.ShipClasses[0], route
}

func TestVoyageInsuranceJournalSurvivesRestartAndWaitsForCover(t *testing.T) {
	h := newRecoveryHarness(t)
	h.api.setVaultBalance("north", 500_000)
	h.api.setVaultBalance("south", 150_000)
	berthed := sailingShipRow(recoveryShipBlastoise, "Blastoise", recoveryHomePort, recoveryFarPort)
	berthed["ship"].(map[string]any)["status"] = "berthed"
	berthed["voyage"], berthed["route_legs"] = nil, []any{}
	h.api.setFleet([]map[string]any{berthed})
	var offered *InsurancePolicy
	base := h.api.server.Config.Handler
	h.api.server.Config.Handler = http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		if req.Method == http.MethodGet && req.URL.Path == "/v1/insurance/quote" {
			insured, _ := strconv.ParseInt(req.URL.Query().Get("insured_value_pence"), 10, 64)
			quote := voyageCoverTestQuote()
			quote.InsuredValuePence = insured
			quote.LloydsExpectedLossPence = new(ceilBPS(insured, 50))
			quote.LloydsPremiumPence = new(max(int64(100), ceilBPS(insured, 100)))
			h.api.writeJSON(w, http.StatusOK, quote)
			return
		}
		if req.Method == http.MethodGet && req.URL.Path == "/v1/insurance/list" {
			h.api.mu.Lock()
			defer h.api.mu.Unlock()
			policies := []InsurancePolicy{}
			if offered != nil && req.URL.Query().Get("underwriter_company_id") == "" {
				policies = append(policies, *offered)
			}
			h.api.writeJSON(w, http.StatusOK, policies)
			return
		}
		if req.Method == http.MethodPost && req.URL.Path == "/v1/insurance/create" {
			body, err := io.ReadAll(req.Body)
			if err != nil {
				t.Error(err)
				w.WriteHeader(http.StatusBadRequest)
				return
			}
			var request gameapi.CreateInsurancePolicyRequest
			if json.Unmarshal(body, &request) != nil || request.ShipID != recoveryShipBlastoise || request.DestPortID != recoveryFarPort ||
				request.InsuredValuePence != 1_000_000 || request.PremiumBPS != 100 || req.Header.Get("Idempotency-Key") == "" {
				t.Errorf("insurance offer did not declare the actual complete quote: %s", body)
				w.WriteHeader(http.StatusBadRequest)
				return
			}
			h.api.mu.Lock()
			defer h.api.mu.Unlock()
			if offered != nil {
				t.Error("duplicate voyage policy creation")
			}
			now := time.Now().UTC()
			offered = &InsurancePolicy{ID: "aaaaaaaa-aaaa-4aaa-8aaa-aaaaaaaaaaaa", InsuredCompanyID: recoveryCompanyID, ShipID: request.ShipID,
				OriginPortID: recoveryHomePort, DestPortID: request.DestPortID, InsuredValuePence: request.InsuredValuePence, HullValuePence: 900_000,
				PremiumBPS: request.PremiumBPS, TotalPremiumPence: 10_000, Status: "open", CreatedAt: now, UpdatedAt: now, ExpiresAt: now.Add(24 * time.Hour)}
			h.api.vaultBalance["north"] -= offered.TotalPremiumPence
			h.api.record(req, body, http.StatusCreated)
			h.api.writeJSON(w, http.StatusCreated, struct {
				InsurancePolicy
				LloydsPremiumBPS int64 `json:"lloyds_premium_bps"`
				AboveLloydsPrice bool  `json:"above_lloyds_price"`
			}{InsurancePolicy: *offered, LloydsPremiumBPS: 100})
			return
		}
		base.ServeHTTP(w, req)
	})
	runner := h.runner(primaryTestKey)
	ship, hull, route := voyageInsuranceRefresh(t, h, runner)
	mission := &store.Mission{AccountID: h.account.ID, ShipID: ship.Ship.ID, Generation: uuid.NewString(), Phase: "depart", Destination: recoveryFarPort}
	data := missionData{Plan: Plan{Kind: "charter", Origin: recoveryHomePort, Destination: recoveryFarPort}, StartedAt: time.Now().UTC(), CostsKnown: true}
	if handled, changed, err := runner.prepareVoyageInsurance(h.ctx, ship, hull, route, mission, &data); !handled || !changed || err != nil {
		t.Fatalf("offer staging: %v %v %v", handled, changed, err)
	}
	beforeOffer := copyCommissionMissionData(t, data)
	if _, err := runner.dispatch(h.ctx, mission, &data); err != nil {
		t.Fatal(err)
	}
	if len(data.VoyageInsurance) != 1 || data.VoyageInsurance[0].PolicyID == "" || data.FeesPence != 10_000 {
		t.Fatalf("offer was not booked once: %+v", data)
	}
	if err := runner.saveMission(h.ctx, mission, &beforeOffer); err != nil {
		t.Fatal(err)
	}
	runner = h.runner(primaryTestKey)
	ship, hull, route = voyageInsuranceRefresh(t, h, runner)
	mission, data = h.mission(ship.Ship.ID)
	if _, err := runner.dispatch(h.ctx, mission, &data); err != nil {
		t.Fatal(err)
	}
	if h.api.mutationCount("/v1/insurance/create") != 1 || data.FeesPence != 10_000 {
		t.Fatal("restart repeated the policy or premium")
	}
	if handled, changed, err := runner.prepareVoyageInsurance(h.ctx, ship, hull, route, mission, &data); !handled || changed || err != nil {
		t.Fatalf("departed before Lloyd's first look: %v %v %v", handled, changed, err)
	}
	h.api.mu.Lock()
	offered.Status, offered.CoveredBPS = "active", 5_000
	h.api.mu.Unlock()
	ship, hull, route = voyageInsuranceRefresh(t, h, runner)
	if handled, _, err := runner.prepareVoyageInsurance(h.ctx, ship, hull, route, mission, &data); !handled || err != nil {
		t.Fatalf("partial syndicate slice bypassed Lloyd's first-look window: %v %v", handled, err)
	}
	h.api.mu.Lock()
	offered.CoveredBPS = 10_000
	h.api.mu.Unlock()
	ship, hull, route = voyageInsuranceRefresh(t, h, runner)
	if handled, _, err := runner.prepareVoyageInsurance(h.ctx, ship, hull, route, mission, &data); handled || err != nil {
		t.Fatalf("full cover did not release departure immediately: %v %v", handled, err)
	}
	h.api.mu.Lock()
	offered.CoveredBPS = 5_000
	offered.CreatedAt = time.Now().UTC().Add(-7 * time.Hour / time.Duration(runner.stateCache.Clock.Calendar.TimeScale))
	h.api.mu.Unlock()
	ship, hull, route = voyageInsuranceRefresh(t, h, runner)
	if handled, _, err := runner.prepareVoyageInsurance(h.ctx, ship, hull, route, mission, &data); handled || err != nil {
		t.Fatalf("bounded partial-cover wait did not release departure: %v %v", handled, err)
	}
}

func voyageInsuranceCandidate(t *testing.T) (*recoveryHarness, *Runner, ShipState, Hull, Route) {
	t.Helper()
	h := newRecoveryHarness(t)
	h.api.setVaultBalance("north", 500_000)
	h.api.setVaultBalance("south", 150_000)
	berthed := sailingShipRow(recoveryShipBlastoise, "Blastoise", recoveryHomePort, recoveryFarPort)
	berthed["ship"].(map[string]any)["status"] = "berthed"
	berthed["voyage"], berthed["route_legs"] = nil, []any{}
	h.api.setFleet([]map[string]any{berthed})
	base := h.api.server.Config.Handler
	h.api.server.Config.Handler = http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		if req.Method == http.MethodGet && req.URL.Path == "/v1/insurance/quote" {
			quote := voyageCoverTestQuote()
			quote.InsuredValuePence, _ = strconv.ParseInt(req.URL.Query().Get("insured_value_pence"), 10, 64)
			h.api.writeJSON(w, http.StatusOK, quote)
			return
		}
		base.ServeHTTP(w, req)
	})
	runner := h.runner(primaryTestKey)
	ship, hull, route := voyageInsuranceRefresh(t, h, runner)
	return h, runner, ship, hull, route
}

func TestVoyageInsuranceOptionalOfferDoesNotTrapUnfundedOrBoundedLeg(t *testing.T) {
	for _, reason := range []string{"stale board", "protected cash", "exploration cap"} {
		t.Run(reason, func(t *testing.T) {
			h, runner, ship, hull, route := voyageInsuranceCandidate(t)
			mission := &store.Mission{AccountID: h.account.ID, ShipID: ship.Ship.ID, Generation: uuid.NewString(), Phase: "depart", Destination: recoveryFarPort}
			data := missionData{Plan: Plan{ShipID: ship.Ship.ID, Kind: "charter", Origin: recoveryHomePort, Destination: recoveryFarPort}, CostsKnown: true}
			switch reason {
			case "stale board":
				delete(runner.meta, "insurance")
			case "protected cash":
				for index := range runner.stateCache.Vaults {
					runner.stateCache.Vaults[index].BalancePence = 0
				}
			case "exploration cap":
				data.Relocation = &relocationIntent{ExpenseCapPence: 9_999}
			}
			if handled, changed, err := runner.prepareVoyageInsurance(h.ctx, ship, hull, route, mission, &data); handled || changed || err != nil {
				t.Fatalf("optional cover trapped the working leg: %v %v %v", handled, changed, err)
			}
			if data.Action != nil || len(data.VoyageInsurance) != 0 || h.api.mutationCount("/v1/company/remit") != 0 {
				t.Fatal("optional cover spent or reserved unavailable cash")
			}
		})
	}
}

func TestVoyageInsuranceBindsEachRecoveryLegAndCommitsLoadedDeparture(t *testing.T) {
	h, runner, ship, hull, route := voyageInsuranceCandidate(t)
	firstQuote := voyageCoverTestQuote()
	firstQuote.OriginPortID, firstQuote.DestPortID = "previous", recoveryHomePort
	data := missionData{Plan: Plan{Kind: "recovery", Origin: recoveryHomePort, Destination: recoveryFarPort}, CostsKnown: true,
		VoyageInsurance: []voyageInsuranceIntent{{Quote: firstQuote, Request: gameapi.CreateInsurancePolicyRequest{DestPortID: recoveryHomePort},
			PolicyID: "bbbbbbbb-bbbb-4bbb-8bbb-bbbbbbbbbbbb"}}}
	mission := &store.Mission{AccountID: h.account.ID, ShipID: ship.Ship.ID, Generation: uuid.NewString(), Phase: "depart", Destination: recoveryFarPort}
	if handled, changed, err := runner.prepareVoyageInsurance(h.ctx, ship, hull, route, mission, &data); !handled || !changed || err != nil {
		t.Fatalf("previous outbound recovery leg prevented the next leg's policy: %v %v %v", handled, changed, err)
	}
	if len(data.VoyageInsurance) != 2 || data.VoyageInsurance[0].PolicyID == "" || data.VoyageInsurance[1].Request.DestPortID != recoveryFarPort {
		t.Fatalf("new leg overwrote the old policy: %+v", data.VoyageInsurance)
	}
	data.Plan.Kind, data.BoughtQuantity = "spot", 10
	data.VoyageInsurance[1].PolicyID = "aaaaaaaa-aaaa-4aaa-8aaa-aaaaaaaaaaaa"
	// No executable destination book is supplied: the accepted policy retains
	// the already-compared leg instead of stranding insured cargo in port.
	if proceed, err := runner.procurementDepartureDecision(PlanningInput{}, ship, recoveryFarPort, &data); !proceed || err != nil {
		t.Fatalf("post-policy repricing unwound the insured leg: %v %v", proceed, err)
	}
	required := make(map[string]int64)
	data.VoyageInsurance[1].PolicyID = ""
	runner.reserveMissionCash(required, mission, &data, ship)
	if required["north"] < 10_000 {
		t.Fatal("unspent premium was not protected before its offer")
	}
	ship.Ship.Status = "sailing"
	atSea := make(map[string]int64)
	runner.reserveMissionCash(atSea, mission, &data, ship)
	if required["north"]-atSea["north"] != 10_000 {
		t.Fatalf("declined premium remained reserved after departure: in port=%d at sea=%d", required["north"], atSea["north"])
	}
}

func TestVoyageInsuranceReconcilesAgainstAdmissionServerTimeNotHostTime(t *testing.T) {
	h, runner, ship, hull, route := voyageInsuranceCandidate(t)
	mission := &store.Mission{AccountID: h.account.ID, ShipID: ship.Ship.ID, Generation: uuid.NewString(), Phase: "depart", Destination: recoveryFarPort}
	data := missionData{Plan: Plan{Kind: "charter", Origin: recoveryHomePort, Destination: recoveryFarPort}, CostsKnown: true}
	if _, _, err := runner.prepareVoyageInsurance(h.ctx, ship, hull, route, mission, &data); err != nil || data.Action == nil {
		t.Fatalf("offer staging: %v", err)
	}
	intent := &data.VoyageInsurance[0]
	intent.ServerNotBefore = time.Now().UTC().Add(-time.Minute)
	created := intent.ServerNotBefore.Add(time.Second)
	policy := InsurancePolicy{ID: "aaaaaaaa-aaaa-4aaa-8aaa-aaaaaaaaaaaa", InsuredCompanyID: recoveryCompanyID,
		ShipID: ship.Ship.ID, OriginPortID: recoveryHomePort, DestPortID: recoveryFarPort, InsuredValuePence: 1_000_000,
		HullValuePence: 900_000, PremiumBPS: 100, TotalPremiumPence: 10_000, Status: "open", CreatedAt: created, UpdatedAt: created, ExpiresAt: created.Add(time.Hour)}
	runner.stateCache.InsurancePolicies = []InsurancePolicy{policy}
	action := data.Action
	persisted, err := h.db.BeginAction(h.ctx, &store.Action{ID: action.ID, AccountID: h.account.ID, Method: action.Method, Path: action.Path, Body: action.Body})
	if err != nil {
		t.Fatal(err)
	}
	if changed, err := runner.reconcileVoyageInsurance(h.ctx, mission, &data, persisted); !changed || err != nil {
		t.Fatalf("server-issued matching policy was lost to local clock skew: %v %v", changed, err)
	}
	if data.Action != nil || data.VoyageInsurance[0].PolicyID != policy.ID || data.FeesPence != 10_000 {
		t.Fatalf("authoritative policy did not settle the single offer: %+v", data)
	}
}

func TestVoyageInsuranceDefinitiveRefusalSkipsRepeatedOfferAfterRestart(t *testing.T) {
	h, runner, ship, hull, route := voyageInsuranceCandidate(t)
	mission := &store.Mission{AccountID: h.account.ID, ShipID: ship.Ship.ID, Generation: uuid.NewString(), Phase: "depart", Destination: recoveryFarPort}
	data := missionData{Plan: Plan{Kind: "recovery", Origin: recoveryHomePort, Destination: recoveryFarPort}, CostsKnown: true}
	if _, _, err := runner.prepareVoyageInsurance(h.ctx, ship, hull, route, mission, &data); err != nil {
		t.Fatal(err)
	}
	if _, err := runner.consumeVoyageInsuranceResult(h.ctx, mission, &data, http.StatusConflict, encode(map[string]string{"code": "policy_refused", "detail": "cannot write this offer"})); err != nil {
		t.Fatal(err)
	}
	if data.CooldownUntil.After(time.Now().UTC().Add(6 * time.Second)) {
		t.Fatal("optional policy refusal imposed a game-day freight delay")
	}
	runner = h.runner(primaryTestKey)
	ship, hull, route = voyageInsuranceRefresh(t, h, runner)
	mission, data = h.mission(ship.Ship.ID)
	if handled, changed, err := runner.prepareVoyageInsurance(h.ctx, ship, hull, route, mission, &data); handled || changed || err != nil {
		t.Fatalf("definitively refused optional cover trapped the same leg: %v %v %v", handled, changed, err)
	}
}

func TestVoyageInsuranceCommittedReturnRetainsOwnWorkingCashAndNavigation(t *testing.T) {
	h, runner, ship, _, _ := voyageInsuranceCandidate(t)
	quote := voyageCoverTestQuote()
	data := missionData{Plan: Plan{Origin: recoveryFarPort, Destination: recoveryHomePort}, ReturnJourney: true,
		GrowthLane: &growthLaneIntent{NextPlan: &Plan{Kind: "spot", Origin: recoveryFarPort, Destination: recoveryHomePort, Quantity: 100, BuyLimitPence: 5_000}},
		VoyageInsurance: []voyageInsuranceIntent{{ReturnJourney: true, Quote: quote, PolicyID: "aaaaaaaa-aaaa-4aaa-8aaa-aaaaaaaaaaaa",
			Request: gameapi.CreateInsurancePolicyRequest{DestPortID: recoveryFarPort}}}}
	action := &durableAction{Stage: "depart", Body: encode(map[string]string{"dest_port_id": recoveryFarPort})}
	if err := runner.growthAllocationReady(h.ctx, ship, &data, action); err != nil {
		t.Fatalf("insured return re-entered discretionary next-lot repricing: %v", err)
	}
	mission := &store.Mission{ShipID: ship.Ship.ID, Destination: recoveryFarPort}
	input := runner.planningInput(ship)
	input.ReserveByRegion = make(map[string]int64)
	runner.voyagePremiumReserves(&input, mission, &data, ship)
	port, _ := findPort(runner.stateCache.Catalog, recoveryFarPort)
	if input.ReserveByRegion[port.BankingRegion] < 500_000 {
		t.Fatalf("premium could spend the mission's next cargo allocation: %+v", input.ReserveByRegion)
	}
}

func TestVoyageInsuranceWaitCannotConsumeFreightDeadline(t *testing.T) {
	h, runner, ship, hull, route := voyageInsuranceCandidate(t)
	_, upper, _ := runner.serverBounds()
	scale := float64(runner.stateCache.Clock.Calendar.TimeScale)
	days := float64(route.TotalDistanceNM)/(float64(hull.BaseSpeedCentiknots)/100*0.8)/24 + 1
	deadline := upper.Add(time.Duration((days*24+3)*3600/scale) * time.Second)
	data := missionData{Plan: Plan{Kind: "recovery", Origin: recoveryHomePort, Destination: recoveryFarPort},
		Freight: []freightObligation{{ContractID: "sealed", Destination: recoveryFarPort, Deadline: &deadline}}}
	if err := runner.validateFreightDeparture(ship, recoveryFarPort, &data, hull); err != nil {
		t.Fatalf("fixture already misses its immediate departure window: %v", err)
	}
	mission := &store.Mission{AccountID: h.account.ID, ShipID: ship.Ship.ID, Generation: uuid.NewString(), Phase: "depart", Destination: recoveryFarPort}
	if handled, changed, err := runner.prepareVoyageInsurance(h.ctx, ship, hull, route, mission, &data); handled || changed || err != nil {
		t.Fatalf("optional cover spent the final freight slack: %v %v %v", handled, changed, err)
	}
}

func TestVoyageInsuranceSelfInsuresSurvivableLoss(t *testing.T) {
	h, runner, ship, hull, route := voyageInsuranceCandidate(t)
	h.api.setVaultBalance("north", 50_000_000)
	ship, hull, route = voyageInsuranceRefresh(t, h, runner)
	mission := &store.Mission{AccountID: h.account.ID, ShipID: ship.Ship.ID, Generation: uuid.NewString(), Phase: "depart", Destination: recoveryFarPort}
	data := missionData{Plan: Plan{Kind: "recovery", Origin: recoveryHomePort, Destination: recoveryFarPort}, CostsKnown: true}
	if handled, changed, err := runner.prepareVoyageInsurance(h.ctx, ship, hull, route, mission, &data); handled || changed || err != nil {
		t.Fatalf("survivable loss bought negative-EV cover: %v %v %v", handled, changed, err)
	}
	if data.Action != nil || len(data.VoyageInsurance) != 0 {
		t.Fatal("self-insured leg reserved a premium")
	}
}
