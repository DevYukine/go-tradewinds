package engine

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/DevYukine/go-tradewinds/internal/store"
	"github.com/google/uuid"
)

// Exercise recorded observations and permanent accounting through the existing
// runner seams. Only the local fake's clock and public sourcing world differ
// from recoveryHarness; no new production API is required by these contracts.
type idleFallbackWorld struct {
	h          *recoveryHarness
	mu         sync.Mutex
	hour       int64
	paused     bool
	failClock  bool
	staleClock bool
	freight    []Contract
	sourceLegs map[string]bool
}

func newIdleFallbackWorld(t *testing.T) *idleFallbackWorld {
	t.Helper()
	h := newRecoveryHarness(t)
	h.api.setFleet([]map[string]any{sourcingReviewShipRow()})
	w := &idleFallbackWorld{h: h, hour: 100, sourceLegs: make(map[string]bool)}
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		w.mu.Lock()
		hour, paused, failClock, staleClock := w.hour, w.paused, w.failClock, w.staleClock
		freight := append([]Contract(nil), w.freight...)
		if request.URL.Path == "/v1/world/route" {
			w.sourceLegs[request.URL.Query().Get("from")+"|"+request.URL.Query().Get("to")] = true
		}
		w.mu.Unlock()
		if request.Method != http.MethodGet {
			h.api.writeProblem(writer, http.StatusForbidden, "test_read_only", "idle observations must not send game mutations")
			return
		}
		switch request.URL.Path {
		case "/v1/world/clock":
			if failClock {
				h.api.writeProblem(writer, http.StatusNotFound, "clock_unavailable", "clock observation unavailable")
				return
			}
			clock := h.api.clockDocument()
			if staleClock {
				clock["real_utc"] = time.Now().UTC().Add(-10 * time.Minute).Format(time.RFC3339Nano)
			}
			clock["paused"] = paused
			clock["calendar"].(map[string]any)["total_game_hours"] = hour
			h.api.writeJSON(writer, http.StatusOK, clock)
		case "/v1/world/catalog":
			catalog := h.api.catalogDocument()
			ports := catalog["ports"].([]any)
			for _, source := range []string{"amsterdam", "rouen", "calais", "ostend"} {
				ports = append(ports, map[string]any{
					"id": source, "banking_region": "north", "sea_zone_id": "zone-north",
					"phase": 1, "manufactures": []string{"sawn_planks"},
				})
			}
			catalog["ports"] = ports
			h.api.writeJSON(writer, http.StatusOK, catalog)
		case "/v1/contract/list":
			contracts := []Contract{sourcingReviewCharter("idle-charter")}
			contracts = append(contracts, freight...)
			h.api.writeJSON(writer, http.StatusOK, contracts)
		case "/v1/market/book":
			book := h.api.bookDocument(request.URL.Query().Get("port_id"))
			for _, raw := range book["books"].([]any) {
				commodity := raw.(map[string]any)
				if commodity["commodity_id"] == recoveryGrain {
					commodity["depth"].(map[string]any)["asks"] = []any{}
				}
			}
			h.api.writeJSON(writer, http.StatusOK, book)
		default:
			h.api.handle(writer, request)
		}
	}))
	t.Cleanup(server.Close)
	h.api.server = server
	return w
}

func (w *idleFallbackWorld) observe(t *testing.T, runner *Runner, hour int64) {
	t.Helper()
	w.mu.Lock()
	w.hour = hour
	w.mu.Unlock()
	runner.generation = uuid.NewString()
	if err := runner.refresh(w.h.ctx); err != nil {
		t.Fatalf("record idle observation at game hour %d: %v", hour, err)
	}
}

func (w *idleFallbackWorld) input(t *testing.T, runner *Runner, shipID string) PlanningInput {
	t.Helper()
	ship, exists := runner.ship(shipID)
	if !exists {
		t.Fatalf("observed ship %s missing", shipID)
	}
	in, err := runner.sourcingInput(w.h.ctx, ship)
	if err != nil {
		t.Fatalf("build ship-specific sourcing policy: %v", err)
	}
	return in
}

func idleFallbackExpect(t *testing.T, in PlanningInput, want bool) {
	t.Helper()
	plans := planSourcingRelocation(in)
	accepted := acceptedRelocations(plans)
	if (len(accepted) > 0) != want {
		t.Fatalf("bounded idle inspection accepted=%v, want %v: %+v", len(accepted) > 0, want, plans)
	}
	for _, plan := range accepted {
		if plan.Kind != "relocation" || plan.Economics.RevenuePence != 0 || plan.Quantity != 0 {
			t.Fatalf("public source inspection invented executable cargo or income: %+v", plan)
		}
	}
}

// Keep recorded host completion time intact; the older sourcing helper always
// replaces it with time.Now, which cannot exercise exact cooldown boundaries.
func idleFallbackSettlement(t *testing.T, h *recoveryHarness, row store.Settlement) {
	t.Helper()
	row.ID = h.account.ID + "-" + row.ID
	row.AccountID = h.account.ID
	row.ActionID = row.ID + "-action"
	row.MissionID = row.ID
	if row.ShipID == "" {
		row.ShipID = recoveryShipBlastoise
	}
	if row.CompletedAt.IsZero() {
		row.CompletedAt = time.Now().UTC().Add(-time.Hour)
	}
	row.StartedAt = row.CompletedAt.Add(-time.Hour)
	row.GameSeconds = 3600
	if _, err := h.db.BeginAction(h.ctx, &store.Action{
		ID: row.ActionID, AccountID: h.account.ID, Method: http.MethodPost,
		Path: "/v1/test", Body: json.RawMessage(`{}`),
	}); err != nil {
		t.Fatal(err)
	}
	if err := h.db.CompleteAction(h.ctx, row.ActionID, http.StatusOK, json.RawMessage(`{}`), nil); err != nil {
		t.Fatal(err)
	}
	if err := h.db.PutSettlement(h.ctx, &row); err != nil {
		t.Fatal(err)
	}
}

func idleFallbackExhaustSeed(t *testing.T, h *recoveryHarness) {
	t.Helper()
	expense := int64(20_000)
	idleFallbackSettlement(t, h, store.Settlement{
		ID: "idle-seed-spent", Kind: "relocation", CostsKnown: true,
		Lane: "different-source|different-demand|grain|charter", ExplorationExpensePence: &expense,
	})
}

func TestIdleFallbackEarnsSixObservedHoursAcrossRestartAndMaintenance(t *testing.T) {
	t.Parallel()
	w := newIdleFallbackWorld(t)
	idleFallbackExhaustSeed(t, w.h)
	runner := w.h.runner(primaryTestKey)
	w.observe(t, runner, 100)
	idleFallbackExpect(t, w.input(t, runner, recoveryShipBlastoise), false)
	w.observe(t, runner, 105)
	idleFallbackExpect(t, w.input(t, runner, recoveryShipBlastoise), false)

	w.h.putMission(recoveryShipBlastoise, "idle-maintenance", "maintenance", missionData{
		Plan:       Plan{Kind: "maintenance", ShipID: recoveryShipBlastoise, Origin: recoveryHomePort},
		CostsKnown: true,
	})
	w.h.api.setFleet([]map[string]any{destinationShipRow(recoveryShipBlastoise, "Blastoise", recoveryHomePort, "anchored_roadstead", 8, nil, nil)})
	runner = w.h.runner(primaryTestKey)
	w.observe(t, runner, 105)
	if err := w.h.db.DeleteMission(w.h.ctx, w.h.account.ID, recoveryShipBlastoise); err != nil {
		t.Fatal(err)
	}
	w.h.api.setFleet([]map[string]any{sourcingReviewShipRow()})
	w.observe(t, runner, 106)
	in := w.input(t, runner, recoveryShipBlastoise)
	idleFallbackExpect(t, in, true)
	history, err := w.h.db.EconomicHistory(w.h.ctx, w.h.account.ID, "")
	if err != nil {
		t.Fatal(err)
	}
	if sourcingLifetimeAllowance(history) != 0 {
		t.Fatal("fixture unexpectedly replenished ordinary lifetime allowance")
	}

	runner = w.h.runner(primaryTestKey)
	w.observe(t, runner, 106)
	idleFallbackExpect(t, w.input(t, runner, recoveryShipBlastoise), true)
	// Refresh must inspect the same bounded shortlist even with no ordinary
	// allowance; merely enabling execution after refresh would leave no routes.
	w.mu.Lock()
	pairs := 0
	for _, source := range []string{"amsterdam", "rouen", "calais", "ostend"} {
		if w.sourceLegs[source+"|"+recoveryFarPort] {
			pairs++
		}
	}
	w.mu.Unlock()
	if pairs != 3 {
		t.Fatalf("idle refresh inspected %d source/charter pairs, want exactly the bounded three", pairs)
	}
}

func TestIdleFallbackRequiresTwentyFourRealMinutesAndLatestMatchingCompletion(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name         string
		legacyAge    time.Duration
		canonicalAge time.Duration
		want         bool
	}{
		{"one microsecond early", 25 * time.Minute, 24*time.Minute - time.Microsecond, false},
		{"exact cooldown", 25 * time.Minute, 24 * time.Minute, true},
		{"newer legacy completion", 23 * time.Minute, 25 * time.Minute, false},
		{"future canonical completion", 25 * time.Minute, -time.Minute, false},
		{"future legacy completion", -time.Minute, 25 * time.Minute, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			w := newIdleFallbackWorld(t)
			idleFallbackExhaustSeed(t, w.h)
			runner := w.h.runner(primaryTestKey)
			w.observe(t, runner, 100)
			w.observe(t, runner, 106)
			in := w.input(t, runner, recoveryShipBlastoise)
			asOf := time.Now().UTC().Add(100 * time.Millisecond).Truncate(time.Microsecond)
			charter := in.Contracts[0]
			for index, probe := range []struct {
				lane string
				age  time.Duration
			}{
				{"amsterdam|" + recoveryFarPort + "|sawn_planks|charter", tc.legacyAge},
				{sourcingEvidenceKey("amsterdam", charter, 0), tc.canonicalAge},
			} {
				expense := int64(100)
				idleFallbackSettlement(t, w.h, store.Settlement{
					ID: fmt.Sprintf("idle-cooldown-%d", index), Kind: "relocation", CostsKnown: true,
					Lane: probe.lane, CompletedAt: asOf.Add(-probe.age), ExplorationExpensePence: &expense,
				})
			}
			// Other source fingerprints must not hide the inspected source's veto.
			in.Catalog.Ports = append([]Port(nil), in.Catalog.Ports...)
			for index := range in.Catalog.Ports {
				if in.Catalog.Ports[index].ID != "amsterdam" {
					in.Catalog.Ports[index].Manufactures = nil
				}
			}
			in.Now = asOf
			if err := runner.loadSourcingHistory(w.h.ctx, &in); err != nil {
				t.Fatal(err)
			}
			idleFallbackExpect(t, in, tc.want)
		})
	}
}

func TestIdleFallbackInvalidClockObservationsRebaseWithoutCredit(t *testing.T) {
	t.Parallel()
	for _, invalid := range []string{"paused", "failed clock", "regressed clock", "stale clock"} {
		t.Run(invalid, func(t *testing.T) {
			w := newIdleFallbackWorld(t)
			idleFallbackExhaustSeed(t, w.h)
			runner := w.h.runner(primaryTestKey)
			w.observe(t, runner, 100)
			w.observe(t, runner, 105)
			next := int64(120)
			w.mu.Lock()
			w.paused = invalid == "paused"
			w.failClock = invalid == "failed clock"
			w.staleClock = invalid == "stale clock"
			w.mu.Unlock()
			if invalid == "regressed clock" {
				next = 102
			}
			w.observe(t, runner, next)
			idleFallbackExpect(t, w.input(t, runner, recoveryShipBlastoise), false)
			w.mu.Lock()
			w.paused, w.failClock, w.staleClock = false, false, false
			w.mu.Unlock()
			if invalid != "regressed clock" {
				next++
				w.observe(t, runner, next)
				idleFallbackExpect(t, w.input(t, runner, recoveryShipBlastoise), false)
			}
			w.observe(t, runner, next+1)
			idleFallbackExpect(t, w.input(t, runner, recoveryShipBlastoise), true)
		})
	}
}

func TestIdleFallbackPhysicalWorkResetsEarnedHours(t *testing.T) {
	t.Parallel()
	for _, activity := range []string{"cargo", "sailing", "active freight", "port change"} {
		t.Run(activity, func(t *testing.T) {
			w := newIdleFallbackWorld(t)
			idleFallbackExhaustSeed(t, w.h)
			runner := w.h.runner(primaryTestKey)
			w.observe(t, runner, 100)
			w.observe(t, runner, 105)
			switch activity {
			case "cargo":
				w.h.api.setFleet([]map[string]any{destinationShipRow(recoveryShipBlastoise, "Blastoise", recoveryHomePort, "berthed", 5, nil, map[string]int64{recoveryGrain: 1})})
			case "sailing":
				w.h.api.setFleet([]map[string]any{sailingShipRow(recoveryShipBlastoise, "Blastoise", recoveryHomePort, recoveryFarPort)})
			case "active freight":
				w.mu.Lock()
				w.freight = []Contract{{ID: "idle-sealed", ContractType: "freight", Status: "active", CarrierCompanyID: recoveryCompanyID, CarrierShipID: recoveryShipBlastoise, OriginPortID: recoveryHomePort, DestPortID: recoveryFarPort, CommodityID: recoveryGrain, QuantityRequired: 1}}
				w.mu.Unlock()
			case "port change":
				w.h.api.setFleet([]map[string]any{destinationShipRow(recoveryShipBlastoise, "Blastoise", recoveryFarPort, "berthed", 5, nil, nil)})
			}
			w.observe(t, runner, 105)
			w.mu.Lock()
			w.freight = nil
			w.mu.Unlock()
			w.h.api.setFleet([]map[string]any{sourcingReviewShipRow()})
			w.observe(t, runner, 106)
			idleFallbackExpect(t, w.input(t, runner, recoveryShipBlastoise), false)
			w.observe(t, runner, 111)
			idleFallbackExpect(t, w.input(t, runner, recoveryShipBlastoise), false)
			w.observe(t, runner, 112)
			idleFallbackExpect(t, w.input(t, runner, recoveryShipBlastoise), true)
		})
	}
}

func TestIdleFallbackBudgetUsesOneLiquiditySeedAndEveryPermanentProbeExpense(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name    string
		free    int64
		expense int64
		profit  int64
		unknown bool
		want    int64
	}{
		{"two percent per probe", 200_000, 20_000, 0, false, 4_000},
		{"shrinking cash", 50_000, 20_000, 0, false, 1_000},
		{"remaining lifetime below per probe", 1_000_000, 69_999, 0, false, 1},
		{"all liquidity allowance spent", 1_000_000, 70_000, 0, false, 0},
		{"never add normal and pilot", 1_000_000, 60_000, 2_050_000, false, 10_000},
		{"two percent of a large treasury", 2_000_000, 20_000, 0, false, 40_000},
		{"unknown probe costs", 1_000_000, 20_000, 0, true, 0},
	} {
		t.Run(tc.name, func(t *testing.T) {
			w := newIdleFallbackWorld(t)
			// Both an old legacy row and another ship's expense consume this
			// account's allowance; neither fingerprint nor age grants a seed.
			oldExpense := int64(10_000)
			idleFallbackSettlement(t, w.h, store.Settlement{
				ID: "idle-old-legacy", Kind: "relocation", CostsKnown: true,
				ExpensesPence: oldExpense, CompletedAt: time.Now().UTC().Add(-30 * 24 * time.Hour),
			})
			rest := tc.expense - oldExpense
			idleFallbackSettlement(t, w.h, store.Settlement{
				ID: "idle-other-ship", ShipID: recoveryShipCharizard, Kind: "relocation", CostsKnown: !tc.unknown,
				ExplorationExpensePence: &rest,
			})
			if tc.profit > 0 {
				idleFallbackSettlement(t, w.h, store.Settlement{ID: "idle-commercial", Kind: "charter", CostsKnown: true, QuantityTons: 1, ProceedsPence: tc.profit})
			}
			runner := w.h.runner(primaryTestKey)
			w.observe(t, runner, 100)
			w.observe(t, runner, 106)
			for _, hour := range []int64{106, 130} {
				runner = w.h.runner(primaryTestKey)
				w.observe(t, runner, hour)
				in := w.input(t, runner, recoveryShipBlastoise)
				for region := range in.Balances {
					in.Balances[region] = in.ReserveByRegion[region]
				}
				in.Balances["north"] += tc.free
				if err := runner.loadSourcingHistory(w.h.ctx, &in); err != nil {
					t.Fatal(err)
				}
				if got := sourcingAllowance(in); got != tc.want {
					t.Fatalf("game hour %d: spending cap=%dp, want %dp with free=%dp, all-time expense=%dp; restart/time must not reset or add seeds", hour, got, tc.want, tc.free, tc.expense)
				}
				accepted := acceptedRelocations(planSourcingRelocation(in))
				for _, plan := range accepted {
					if sourcingExposure(plan) > tc.want {
						t.Fatalf("inspection exposure exceeds the remaining liquidity-backed cap: %+v", plan)
					}
				}
				if tc.want <= 1 && len(accepted) != 0 {
					t.Fatalf("exhausted or one-penny allowance admitted a costed inspection: %+v", accepted)
				}
				if tc.want >= 1_000 && len(accepted) == 0 {
					t.Fatalf("sufficient bounded liquidity could not admit a source inspection: %+v", planSourcingRelocation(in))
				}
			}
		})
	}
}

func TestIdleFallbackDoesNotBorrowAnotherShipOrAccountsIdleHours(t *testing.T) {
	t.Parallel()
	w := newIdleFallbackWorld(t)
	idleFallbackExhaustSeed(t, w.h)
	runner := w.h.runner(primaryTestKey)
	w.observe(t, runner, 100)
	w.h.api.setFleet([]map[string]any{
		sourcingReviewShipRow(),
		destinationShipRow(recoveryShipCharizard, "Charizard", recoveryHomePort, "berthed", 5, nil, nil),
	})
	w.observe(t, runner, 105)
	w.observe(t, runner, 106)
	idleFallbackExpect(t, w.input(t, runner, recoveryShipBlastoise), true)
	idleFallbackExpect(t, w.input(t, runner, recoveryShipCharizard), false)

	other := *w.h
	other.account = store.Account{ID: uuid.NewString(), CompanyID: uuid.NewString(), Label: "Independent idle account", Enabled: true}
	otherKey := "synthetic-idle-" + uuid.NewString()
	otherShip, otherSecondShip := uuid.NewString(), uuid.NewString()
	// Reuse the same read-only public world and game clock, but project a
	// distinct authenticated company and its own fleet for the second account.
	identity := strings.NewReplacer(
		recoveryCompanyID, other.account.CompanyID,
		recoveryPlayerID, uuid.NewString(),
		recoveryShipBlastoise, otherShip,
		recoveryShipCharizard, otherSecondShip,
		"aaaaaaaa-aaaa-4aaa-8aaa-aaaaaaaaaaaa", uuid.NewString(),
	)
	otherServer := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		if request.Header.Get("Authorization") != "Bearer "+otherKey {
			w.h.api.writeProblem(writer, http.StatusUnauthorized, "invalid_api_key", "unknown synthetic account credential")
			return
		}
		projected := request.Clone(request.Context())
		projected.Header.Set("Authorization", "Bearer "+primaryTestKey)
		query := projected.URL.Query()
		switch query.Get("ship_id") {
		case otherShip:
			query.Set("ship_id", recoveryShipBlastoise)
		case otherSecondShip:
			query.Set("ship_id", recoveryShipCharizard)
		}
		projected.URL.RawQuery = query.Encode()
		response := httptest.NewRecorder()
		w.h.api.server.Config.Handler.ServeHTTP(response, projected)
		writer.Header().Set("Content-Type", response.Header().Get("Content-Type"))
		writer.WriteHeader(response.Code)
		_, _ = fmt.Fprint(writer, identity.Replace(response.Body.String()))
	}))
	t.Cleanup(otherServer.Close)
	other.api = &fakeGameAPI{server: otherServer}
	if err := other.db.SaveAccount(other.ctx, other.account, store.Credentials{Handle: "synthetic-idle", APIKey: otherKey}); err != nil {
		t.Fatal(err)
	}
	expense := int64(20_000)
	idleFallbackSettlement(t, &other, store.Settlement{
		ID: "idle-seed-spent", ShipID: otherShip, Kind: "relocation", CostsKnown: true,
		Lane: "different-source|different-demand|grain|charter", ExplorationExpensePence: &expense,
	})
	otherRunner := other.runner(otherKey)
	w.observe(t, otherRunner, 106)
	if err := otherRunner.verifyIdentity(); err != nil {
		t.Fatalf("second account observes a different company: %v", err)
	}
	idleFallbackExpect(t, w.input(t, otherRunner, otherShip), false)
	idleFallbackExpect(t, w.input(t, otherRunner, otherSecondShip), false)
	w.observe(t, otherRunner, 112)
	idleFallbackExpect(t, w.input(t, otherRunner, otherShip), true)
	idleFallbackExpect(t, w.input(t, otherRunner, otherSecondShip), true)
	runner = w.h.runner(primaryTestKey)
	w.observe(t, runner, 106)
	idleFallbackExpect(t, w.input(t, runner, recoveryShipBlastoise), true)
	idleFallbackExpect(t, w.input(t, runner, recoveryShipCharizard), false)
	w.observe(t, runner, 112)
	idleFallbackExpect(t, w.input(t, runner, recoveryShipCharizard), true)
}

func TestIdleFallbackDoesNotOverrideIncomeOrSafetyGuards(t *testing.T) {
	t.Parallel()
	for _, guard := range []string{"ordinary income", "stale charter", "stale route", "unsafe route", "protected cash"} {
		t.Run(guard, func(t *testing.T) {
			w := newIdleFallbackWorld(t)
			idleFallbackExhaustSeed(t, w.h)
			runner := w.h.runner(primaryTestKey)
			w.observe(t, runner, 100)
			w.observe(t, runner, 106)
			in := w.input(t, runner, recoveryShipBlastoise)
			switch guard {
			case "ordinary income":
				market := sourcingFixture[Market](t, `{"port_id":"portb","visibility":"quay","customs_bps":200,"books":[{"commodity_id":"provisions","depth":{"asks":[],"bids":[{"order_id":"idle-income-bid","price_pence":5000,"remaining_quantity":100}]}}]}`)
				in.Markets[recoveryFarPort] = market
				in.Metadata["books/"+recoveryFarPort] = DatasetMeta{UpdatedAt: in.Now, AttemptedAt: in.Now, Generation: in.Generation, Complete: true}
				if ordinary := PlanVoyages(in); len(ordinary) == 0 || !ordinary[0].Accepted {
					t.Fatalf("ordinary income fixture is not executable: %+v", ordinary)
				}
			case "stale charter":
				in.Contracts[0].Stale = true
			case "stale route":
				for key, meta := range in.Metadata {
					if len(key) >= len("routes/") && key[:len("routes/")] == "routes/" {
						meta.Stale = true
						in.Metadata[key] = meta
					}
				}
			case "unsafe route":
				for key, route := range in.Routes {
					route.Legs = append([]RouteLeg(nil), route.Legs...)
					for index := range route.Legs {
						route.Legs[index].EdgeClass = "oceanic"
					}
					in.Routes[key] = route
				}
			case "protected cash":
				for region := range in.Balances {
					in.Balances[region] = in.ReserveByRegion[region]
				}
				if err := runner.loadSourcingHistory(w.h.ctx, &in); err != nil {
					t.Fatal(err)
				}
			}
			idleFallbackExpect(t, in, false)
		})
	}
}

func TestIdleFallbackKeepsActiveAndAmbiguousExplorationExclusive(t *testing.T) {
	t.Parallel()
	for _, ambiguous := range []bool{false, true} {
		t.Run(fmt.Sprintf("ambiguous=%v", ambiguous), func(t *testing.T) {
			w := newIdleFallbackWorld(t)
			idleFallbackExhaustSeed(t, w.h)
			w.h.api.setFleet([]map[string]any{
				sourcingReviewShipRow(),
				destinationShipRow(recoveryShipCharizard, "Charizard", recoveryHomePort, "berthed", 5, nil, nil),
			})
			runner := w.h.runner(primaryTestKey)
			w.observe(t, runner, 100)
			w.observe(t, runner, 106)
			data := missionData{
				Plan:       Plan{Kind: "relocation", ShipID: recoveryShipCharizard, Origin: recoveryHomePort, Destination: "amsterdam"},
				CostsKnown: !ambiguous,
				Relocation: &relocationIntent{TargetContractID: "idle-charter", ExpenseCapPence: 1000},
			}
			w.h.putMission(recoveryShipCharizard, "exclusive-idle-probe", "sourcing_fund", data)
			ship, _ := runner.ship(recoveryShipBlastoise)
			started, _, err := runner.considerSourcing(w.h.ctx, ship)
			if err != nil {
				t.Fatal(err)
			}
			if started {
				t.Fatal("another idle ship started exploration while an active or ambiguous probe remained")
			}
			w.h.missingMission(recoveryShipBlastoise)
			_, retained := w.h.mission(recoveryShipCharizard)
			if retained.Relocation == nil || retained.Relocation.ExpenseCapPence != 1000 || retained.CostsKnown != !ambiguous {
				t.Fatalf("competing idle work rewrote the existing probe's durable cap or accounting: %+v", retained)
			}
		})
	}
}
