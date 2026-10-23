package engine

import (
	"encoding/json"
	"errors"
	"testing"
	"time"

	"github.com/DevYukine/go-tradewinds/internal/gameapi"
	"github.com/DevYukine/go-tradewinds/internal/store"
)

func TestActionDispositionDistinguishesAmbiguousFromDefinite(t *testing.T) {
	cases := []struct {
		name string
		err  error
		want actionDisposition
	}{
		{"successful response", nil, actionSuccess},
		{"timeout", errors.New("timeout after sending bytes"), actionUnresolved},
		{"server error", &gameapi.APIError{Status: 503, Code: "world_paused"}, actionUnresolved},
		{"in-flight request", &gameapi.APIError{Status: 409, Code: "idempotency_request_in_flight"}, actionUnresolved},
		{"ordinary conflict", &gameapi.APIError{Status: 409, Code: "ship_not_berthed"}, actionRejected},
		{"cached throttle", &gameapi.APIError{Status: 429, Code: "rate_limit_exceeded"}, actionRejected},
		{"fingerprint mismatch", &gameapi.APIError{Status: 422, Code: "idempotency_key_reused"}, actionIntegrityFailure},
		{"committed malformed body", &gameapi.APIError{Status: 200, Code: "invalid_response", Body: json.RawMessage("truncated")}, actionCommittedUnknown},
	}
	for _, test := range cases {
		t.Run(test.name, func(t *testing.T) {
			if got := disposition(test.err); got != test.want {
				t.Fatalf("got %v, want %v", got, test.want)
			}
		})
	}
}

func TestMissionStageKeysAreStableButNeverTradeAndDepartTogether(t *testing.T) {
	first := actionID("account-a", "generation-a", "buy", 0)
	if first != actionID("account-a", "generation-a", "buy", 0) {
		t.Fatal("restored mission changed logical action identity")
	}
	for _, other := range []string{
		actionID("account-b", "generation-a", "buy", 0),
		actionID("account-a", "generation-b", "buy", 0),
		actionID("account-a", "generation-a", "depart", 0),
		actionID("account-a", "generation-a", "buy", 1),
	} {
		if first == other {
			t.Fatal("account, generation, stage or definite-outcome attempt collided")
		}
	}
}

func TestArrivalProvenanceNeverIncludesLocalOrSealedCargo(t *testing.T) {
	departed := map[string]int64{"provisions": 20, "sawn_planks": 5}
	actual := map[string]int64{"provisions": 15, "sawn_planks": 5, "silver_bullion": 10}
	sealed := map[string]int64{"provisions": 8}
	eligible := arrivalEligibility(departed, actual, sealed, true)
	if eligible["provisions"] != 7 || eligible["sawn_planks"] != 5 || eligible["silver_bullion"] != 0 {
		t.Fatalf("incorrect carried-in provenance: %+v", eligible)
	}
	if unknown := arrivalEligibility(departed, actual, sealed, false); len(unknown) != 0 {
		t.Fatal("cold imported manifest fabricated charter eligibility")
	}
	loss := arrivalEligibility(departed, map[string]int64{"provisions": 4}, sealed, true)
	if loss["provisions"] != 0 {
		t.Fatal("unknown post-loss allocation released sealed cargo")
	}
}

func TestAllSealedManifestReservationsAggregateBeforeSale(t *testing.T) {
	obligations := []freightObligation{
		{ContractID: "one", Commodity: "provisions", Quantity: 5},
		{ContractID: "two", Commodity: "provisions", Quantity: 8},
		{ContractID: "done", Commodity: "provisions", Quantity: 10, Settled: true},
		{ContractID: "three", Commodity: "sawn_planks", Quantity: 7, TenderRefused: true},
	}
	reserved := sealedReservations(obligations)
	if reserved["provisions"] != 13 || reserved["sawn_planks"] != 7 {
		t.Fatalf("multiple contracts or consequential tender lost reservations: %+v", reserved)
	}
}

func TestExpiredDepartureRequiresActualDestinationEvidence(t *testing.T) {
	before := ShipState{}
	before.Ship.ID = "ship-a"
	before.Ship.Status = "berthed"
	before.Ship.CurrentPortID = "le_havre"
	actual := before
	actual.Ship.Status = "sailing"
	actual.RouteLegs = []RouteLeg{{ToNode: "portsmouth"}}
	if !departureObserved(before, actual, "portsmouth") {
		t.Fatal("actual sailing route did not reconcile departure")
	}
	if departureObserved(before, actual, "london") {
		t.Fatal("unrelated route reconciled an expired action")
	}
	actual.Ship.Status = "berthed"
	actual.Ship.CurrentPortID = "portsmouth"
	if !departureObserved(before, actual, "portsmouth") {
		t.Fatal("actual completed arrival did not reconcile departure")
	}
	actual.Ship.ID = "another-ship"
	if departureObserved(before, actual, "portsmouth") {
		t.Fatal("different ship satisfied a durable mission")
	}
}

func TestPaginationUnderstandsConsumerResultWrappers(t *testing.T) {
	for _, body := range []string{`{"id":"plain"}`, `{"ship":{"id":"wrapped"}}`, `{"convoy":{"id":"convoy"}}`} {
		if id, err := pageID(json.RawMessage(body)); err != nil || id == "" {
			t.Fatalf("documented list wrapper rejected: %s, %v", body, err)
		}
	}
	if _, err := pageID(json.RawMessage(`{"quantity":5}`)); err == nil {
		t.Fatal("pagination silently accepted an unpageable row")
	}
}

func TestMissingTradeQuantitiesCannotAdvanceProcurement(t *testing.T) {
	var incomplete tradeResult
	if err := json.Unmarshal([]byte(`{"commodity_id":"provisions","total_gross_pence":100}`), &incomplete); err != nil {
		t.Fatal(err)
	}
	if validTrade(incomplete) {
		t.Fatal("HTTP success alone advanced a procurement stage")
	}
	var zeroFill tradeResult
	if err := json.Unmarshal([]byte(`{"commodity_id":"provisions","filled_quantity":0,"total_gross_pence":0,"customs_duty_pence":0}`), &zeroFill); err != nil {
		t.Fatal(err)
	}
	if !validTrade(zeroFill) {
		t.Fatal("explicit zero IOC fill must remain a definite result, not a timeout")
	}
}

func TestActualOverheadIsClaimedOnceAcrossOverlappingMissions(t *testing.T) {
	runner := &Runner{
		snapshot: map[string]json.RawMessage{
			"ledger": json.RawMessage(`[
				{"id":"old","debit_account_id":"vault","credit_account_id":"crown","amount_pence":999,"reason":"daily_upkeep"},
				{"id":"upkeep","debit_account_id":"vault","credit_account_id":"crown","amount_pence":100,"reason":"daily_upkeep"},
				{"id":"berth","debit_account_id":"vault","credit_account_id":"port","amount_pence":50,"reason":"berth_dues"},
				{"id":"transfer","debit_account_id":"vault","credit_account_id":"other-vault","amount_pence":500,"reason":"remit"}
			]`),
		},
		stateCache:    accountState{Vaults: []Vault{{AccountID: "vault"}, {AccountID: "other-vault"}}},
		expenseOwners: make(map[string]string),
	}
	mission := &missionData{InitialLedgerIDs: map[string]bool{"old": true}}
	first, ids := runner.claimBillsSince(mission, "first-mission")
	if first != 150 || len(ids) != 2 {
		t.Fatalf("actual overhead = %d, claims = %v", first, ids)
	}
	second, duplicateIDs := runner.claimBillsSince(mission, "overlapping-mission")
	if second != 0 || len(duplicateIDs) != 0 {
		t.Fatalf("overlapping mission charged overhead twice: %d %v", second, duplicateIDs)
	}
	runner.expenseOwners = decode[map[string]string](runner.snapshot, "_expense_allocations")
	restarted, _ := runner.claimBillsSince(mission, "after-restart")
	if restarted != 0 {
		t.Fatalf("durable claim was lost on restart: %d", restarted)
	}
}

func TestFreightStopsKeepAllSealsAndUseActualDeadlines(t *testing.T) {
	early := time.Date(2026, 6, 1, 1, 0, 0, 0, time.UTC)
	late := early.Add(time.Hour)
	obligations := []freightObligation{
		{ContractID: "late", Destination: "rotterdam", Commodity: "grain", Quantity: 4, Deadline: &late},
		{ContractID: "early", Destination: "portsmouth", Commodity: "grain", Quantity: 6, Deadline: &early},
		{ContractID: "same-stop", Destination: "portsmouth", Commodity: "timber", Quantity: 3, Deadline: &late},
	}
	stops := freightStops(obligations)
	if len(stops) != 2 || stops[0].Destination != "portsmouth" || stops[1].Destination != "rotterdam" {
		t.Fatalf("deadline itinerary = %+v", stops)
	}
	reserved := sealedReservations(obligations)
	if reserved["grain"] != 10 || reserved["timber"] != 3 {
		t.Fatalf("itinerary released a seal: %v", reserved)
	}
	obligations = append(obligations, freightObligation{ContractID: "unknown", Destination: "le_havre"})
	if freightStops(obligations)[0].Deadline != nil {
		t.Fatal("unknown acceptance deadline was replaced by a guessed deadline")
	}
}

func TestAcquiredCashMarginUsesActualFillNotStress(t *testing.T) {
	reward := int64(21_958)
	now := time.Date(2026, 10, 22, 17, 0, 0, 0, time.UTC)
	in := PlanningInput{
		Now:        now,
		Generation: "sync-a",
		Clock:      Clock{RealUTC: now},
		Metadata: map[string]DatasetMeta{
			"clock": {UpdatedAt: now, AttemptedAt: now, Complete: true, Generation: "sync-a"},
		},
		Catalog: Catalog{ShipClasses: []Hull{
			{ID: "hoy", BaseSpeedCentiknots: 850},
		}},
		Contracts: []Contract{
			{ID: "charter-a", Status: "open", RewardPerTonPence: &reward, RemainingQuantity: 9391},
		},
		Routes: map[string]Route{
			"le_havre|portsmouth|hoy": {TotalDistanceNM: 30},
		},
	}
	in.Ship.Ship.ShipClass = "hoy"
	in.Clock.Calendar.TimeScale = 60
	cases := []struct {
		name    string
		bought  int64
		paid    int64
		customs int64
		wantNet int64
	}{
		{name: "full profitable fill despite negative stress", bought: 24, paid: 473_184, customs: 13_747, wantNet: 36_055},
		{name: "partial fill cannot pay actual voyage costs", bought: 1, paid: 19_716, customs: 573, wantNet: -2_337},
	}
	for _, test := range cases {
		t.Run(test.name, func(t *testing.T) {
			data := missionData{
				Plan: Plan{
					Kind: "charter", Origin: "le_havre", Destination: "portsmouth", ContractID: "charter-a",
					Quantity: 24,
					Economics: Economics{
						ProvisionsPence: 1_342, BerthDuesPence: 2_664,
						ExpectedNetPence: 36_055, StressNetPence: -40_359,
						StressHaircutPence: 76_414,
					},
				},
				BoughtQuantity: test.bought,
			}
			recordTradeCosts(&data, "buy", tradeResult{
				CommodityID:      "provisions",
				FilledQuantity:   &test.bought,
				TotalGrossPence:  &test.paid,
				CustomsDutyPence: &test.customs,
			})
			revenue, reason := acquiredRevenue(in, data.Plan, data.BoughtQuantity, nil)
			if reason != "" || revenue != test.bought*reward {
				t.Fatalf("current receivable ignored the actual fill: %d, %s", revenue, reason)
			}
			if got := acquiredNet(&data, revenue); got != test.wantNet {
				t.Fatalf("actual procurement net = %d, want %d", got, test.wantNet)
			}
		})
	}
}

func TestProvisionCustomsArePaidOnceAndKnownTollsStillReduceNet(t *testing.T) {
	data := missionData{
		Plan: Plan{Economics: Economics{
			ProvisionsPence: 10_290, BerthDuesPence: 1_000,
		}},
		AcquisitionPence: 40_000,
		FeesPence:        1_000,
	}
	provisionGross := int64(10_000)
	provisionCustoms := int64(290)
	provisionQuantity := int64(1)
	recordTradeCosts(&data, "provision", tradeResult{
		CommodityID:      "provisions",
		FilledQuantity:   &provisionQuantity,
		TotalGrossPence:  &provisionGross,
		CustomsDutyPence: &provisionCustoms,
	})
	if net := acquiredNet(&data, 52_500); net != 210 {
		t.Fatalf("provision fee was omitted or deducted twice: net %d", net)
	}
	data.Plan.Economics.TollPence = 300
	if net := acquiredNet(&data, 52_500); net != -90 {
		t.Fatalf("known toll was treated like diagnostic stress: net %d", net)
	}
}

func TestGossipAcquiredRevenueDoesNotConvertPriceStressIntoExpense(t *testing.T) {
	now := time.Date(2026, 10, 22, 17, 0, 0, 0, time.UTC)
	var detail PortDetail
	if err := json.Unmarshal([]byte(`{
		"id":"le_havre",
		"coffee_house_gossip":[{
			"subject_port_id":"portsmouth",
			"snapshot_at":"2026-10-22T17:00:00Z",
			"commodities":[{
				"commodity_id":"provisions",
				"snapshot_at":"2026-10-22T17:00:00Z",
				"best_bid_pence":10000
			}]
		}]
	}`), &detail); err != nil {
		t.Fatal(err)
	}
	in := PlanningInput{
		Now: now, Generation: "sync-a",
		Ports: map[string]PortDetail{"le_havre": detail},
		Clock: Clock{RealUTC: now},
		Metadata: map[string]DatasetMeta{
			"ports/le_havre": {UpdatedAt: now, AttemptedAt: now, Complete: true, Generation: "sync-a"},
			"clock":          {UpdatedAt: now, AttemptedAt: now, Complete: true, Generation: "sync-a"},
		},
	}
	in.Clock.Calendar.TimeScale = 60
	plan := Plan{Kind: "spot", Destination: "portsmouth", Commodity: "provisions"}
	if revenue, reason := acquiredRevenue(in, plan, 1, nil); revenue != 10_000 || reason != "" {
		t.Fatalf("observable one-ton bid was reduced by arbitrary stress: %d, %s", revenue, reason)
	}
	if revenue, reason := acquiredRevenue(in, plan, 2, nil); revenue != 0 || reason == "" {
		t.Fatalf("gossip fabricated remote bid quantity: %d, %s", revenue, reason)
	}
}

func TestAuthAndThrottleRefusalsPreserveCarriedInCharterOpportunity(t *testing.T) {
	cases := []struct {
		name   string
		status int
	}{
		{name: "expired authorization", status: 401},
		{name: "missing contract scope", status: 403},
		{name: "temporary throttle", status: 429},
	}
	for _, test := range cases {
		t.Run(test.name, func(t *testing.T) {
			mission := store.Mission{
				AccountID:  "account-a",
				ShipID:     "ship-a",
				Generation: "mission-a",
				Phase:      "arrival",
			}
			oldActionID := actionID(mission.AccountID, mission.Generation, "deliver-charter", 0)
			data := missionData{
				Action: &durableAction{
					ID:    oldActionID,
					Stage: "deliver-charter",
					Body:  json.RawMessage(`{"contract_id":"charter-a","ship_id":"ship-a","quantity":24}`),
				},
				Eligible:           map[string]int64{"provisions": 24},
				RejectedCharterIDs: map[string]bool{"previous-domain-refusal": true},
			}
			applyRejectedResult(&mission, &data, test.status, "request cannot currently be authorized")
			if data.RejectedCharterIDs["charter-a"] || !data.RejectedCharterIDs["previous-domain-refusal"] ||
				data.Eligible["provisions"] != 24 || mission.Phase != "arrival" {
				t.Fatalf("request rejection destroyed the still-live carried-in opportunity: %+v", data)
			}
			if data.Action != nil || data.LastActionID != oldActionID || data.Sequence != 1 {
				t.Fatalf("definite request outcome was not consumed durably: %+v", data)
			}
			nextActionID := actionID(mission.AccountID, mission.Generation, "deliver-charter", data.Sequence)
			if nextActionID == oldActionID {
				t.Fatal("post-repair attempt would replay the cached failed idempotency key")
			}
		})
	}
}

func TestActualCharterDomainRefusalStillRejectsOnlyThatCharter(t *testing.T) {
	mission := store.Mission{Phase: "arrival"}
	data := missionData{
		Action: &durableAction{
			ID:    "definite-domain-refusal",
			Stage: "deliver-charter",
			Body:  json.RawMessage(`{"contract_id":"charter-a","ship_id":"ship-a","quantity":24}`),
		},
		Eligible: map[string]int64{"provisions": 24},
	}
	applyRejectedResult(&mission, &data, 409, "charter is already filled")
	if !data.RejectedCharterIDs["charter-a"] || data.Eligible["provisions"] != 24 ||
		mission.Phase != "arrival" || data.Action != nil {
		t.Fatalf("actual refusal either remained retryable or invalidated all cargo: %+v", data)
	}
}
