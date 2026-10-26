package engine

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/DevYukine/go-tradewinds/internal/store"
	"github.com/google/uuid"
)

const marqueExecutionZone = "marque-offshore"

type marqueExecutionAPI struct {
	mu                 sync.Mutex
	h                  *recoveryHarness
	downstream         http.Handler
	quote              MarqueQuote
	issued             map[string]letterOfMarque
	hideMarques        bool
	badReceipt         bool
	finalPriceIncrease bool
	quoteReads         int
	missingReturnRoute bool
	returnRouteReads   int
}

func newMarqueExecutionAPI(h *recoveryHarness) *marqueExecutionAPI {
	proxy := &marqueExecutionAPI{
		h: h, downstream: h.api.server.Config.Handler,
		quote: MarqueQuote{SeaZoneID: marqueExecutionZone, AddedPiracyBPS: 500, DurationGameHours: 60,
			CostPence: 10_000, ExpectedNetPrizePence: 17_000, LadenPassagesPerDay: 2},
		issued: make(map[string]letterOfMarque),
	}
	h.api.server.Config.Handler = proxy
	h.marques = true
	return proxy
}

func (p *marqueExecutionAPI) ServeHTTP(writer http.ResponseWriter, request *http.Request) {
	path := request.URL.Path
	returnRoute := path == "/v1/world/route" && request.URL.Query().Get("from") == recoveryFarPort && request.URL.Query().Get("to") == recoveryHomePort
	if path != "/v1/world/catalog" && path != "/v1/fleet/marque-quote" && path != "/v1/fleet/marques" &&
		path != "/v1/fleet/issue-marque" && path != "/v1/shipyard/modify-ship" && !returnRoute {
		p.downstream.ServeHTTP(writer, request)
		return
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	switch {
	case request.Method == http.MethodGet && returnRoute:
		p.returnRouteReads++
		if p.missingReturnRoute {
			p.h.api.writeProblem(writer, http.StatusNotFound, "route_unknown", "return route geometry is unavailable")
			return
		}
		p.h.api.mu.Lock()
		route := p.h.api.routeDocument(recoveryFarPort, recoveryHomePort)
		p.h.api.mu.Unlock()
		p.h.api.writeJSON(writer, http.StatusOK, route)
	case request.Method == http.MethodGet && path == "/v1/world/catalog":
		p.h.api.mu.Lock()
		catalog := p.h.api.catalogDocument()
		p.h.api.mu.Unlock()
		catalog["sea_zones"] = append(catalog["sea_zones"].([]any), map[string]any{
			"id": marqueExecutionZone, "zone_class": "coastal", "base_storm_bps": 100, "base_piracy_bps": 0,
		})
		p.h.api.writeJSON(writer, http.StatusOK, catalog)
	case request.Method == http.MethodGet && path == "/v1/fleet/marque-quote":
		quote := p.quote
		quote.SeaZoneID = request.URL.Query().Get("sea_zone_id")
		if quote.SeaZoneID == marqueExecutionZone {
			p.quoteReads++
			if p.finalPriceIncrease && p.quoteReads >= 3 {
				quote.CostPence = 11_001
			}
		} else {
			quote.FloorApplied = true
		}
		if request.URL.Query().Get("added_piracy_bps") != "500" || request.URL.Query().Get("duration_game_hours") != "60" {
			p.h.t.Error("marque quote did not preserve the exact 500 bps / 60 game-hour terms")
		}
		p.h.api.writeJSON(writer, http.StatusOK, quote)
	case request.Method == http.MethodGet && path == "/v1/fleet/marques":
		rows := make([]letterOfMarque, 0, len(p.issued))
		if !p.hideMarques {
			for _, marque := range p.issued {
				rows = append(rows, marque)
			}
		}
		p.h.api.writeJSON(writer, http.StatusOK, rows)
	case request.Method == http.MethodPost && path == "/v1/fleet/issue-marque":
		body, err := io.ReadAll(request.Body)
		if err != nil {
			p.h.t.Errorf("read marque request: %v", err)
			writer.WriteHeader(http.StatusBadRequest)
			return
		}
		var terms struct {
			SeaZoneID         string `json:"sea_zone_id"`
			AddedPiracyBPS    int64  `json:"added_piracy_bps"`
			DurationGameHours int64  `json:"duration_game_hours"`
		}
		key := request.Header.Get("Idempotency-Key")
		if json.Unmarshal(body, &terms) != nil || terms.SeaZoneID != marqueExecutionZone || terms.AddedPiracyBPS != 500 || terms.DurationGameHours != 60 || key == "" {
			p.h.t.Error("issue-marque did not send its immutable exact request and durable key")
			writer.WriteHeader(http.StatusBadRequest)
			return
		}
		marque, exists := p.issued[key]
		p.h.api.mu.Lock()
		if !exists {
			now := time.Now().UTC()
			marque = letterOfMarque{ID: uuid.NewString(), SeaZoneID: terms.SeaZoneID, SponsorCompanyID: recoveryCompanyID,
				BankingRegion: "north", AddedPiracyBPS: 500, CostPence: p.quote.CostPence,
				CreatedAt: now, UpdatedAt: now, ExpiresAt: now.Add(60 * time.Hour)}
			p.issued[key] = marque
			p.h.api.vaultBalance["north"] -= marque.CostPence
		}
		p.h.api.record(request, body, http.StatusCreated)
		p.h.api.mu.Unlock()
		if p.badReceipt {
			writer.WriteHeader(http.StatusCreated)
			_, _ = writer.Write([]byte("unreadable committed receipt"))
			return
		}
		p.h.api.writeJSON(writer, http.StatusCreated, struct {
			letterOfMarque
			Pricing MarqueQuote `json:"pricing"`
		}{letterOfMarque: marque, Pricing: p.quote})
	case request.Method == http.MethodPost && path == "/v1/shipyard/modify-ship":
		body, err := io.ReadAll(request.Body)
		if err != nil {
			p.h.t.Errorf("read refit request: %v", err)
			writer.WriteHeader(http.StatusBadRequest)
			return
		}
		var terms struct {
			ShipID  string `json:"ship_id"`
			Cannons int64  `json:"target_cannons_count"`
		}
		if err := json.Unmarshal(body, &terms); err != nil {
			p.h.t.Errorf("decode refit request: %v", err)
			writer.WriteHeader(http.StatusBadRequest)
			return
		}
		p.h.api.mu.Lock()
		for _, row := range p.h.api.fleet {
			ship := row["ship"].(map[string]any)
			if ship["id"] == terms.ShipID {
				ship["cannons_count"] = terms.Cannons
			}
		}
		p.h.api.record(request, body, http.StatusOK)
		p.h.api.mu.Unlock()
		p.h.api.writeJSON(writer, http.StatusOK, map[string]any{"ship_id": terms.ShipID, "cannons_count": terms.Cannons})
	default:
		writer.WriteHeader(http.StatusMethodNotAllowed)
	}
}

func refreshMarqueExecution(t *testing.T, h *recoveryHarness, runner *Runner) {
	t.Helper()
	runner.generation = uuid.NewString()
	if err := runner.refresh(h.ctx); err != nil {
		t.Fatal(err)
	}
}

func stageMarqueExecution(t *testing.T, h *recoveryHarness, runner *Runner) (*store.Mission, missionData) {
	t.Helper()
	refreshMarqueExecution(t, h, runner)
	if changed, err := runner.considerMarque(h.ctx); err != nil || !changed {
		t.Fatalf("consider marque: changed=%v err=%v", changed, err)
	}
	refreshMarqueExecution(t, h, runner)
	mission, data := h.mission("__marque")
	if changed, err := runner.driveMarque(h.ctx, mission, &data); err != nil || !changed {
		t.Fatalf("stage marque: changed=%v err=%v", changed, err)
	}
	mission, data = h.mission("__marque")
	if data.Action == nil || data.Action.Stage != "marque" || data.Action.ID == "" {
		t.Fatalf("marque was not durably staged: %+v", data)
	}
	return mission, data
}

func TestMarqueExecutionLifecycle(t *testing.T) {
	t.Parallel()
	for _, reconcile := range []bool{false, true} {
		name := "typed receipt"
		if reconcile {
			name = "committed receipt reconciled from REST"
		}
		t.Run(name, func(t *testing.T) {
			h := newRecoveryHarness(t)
			proxy := newMarqueExecutionAPI(h)
			proxy.badReceipt = reconcile
			proxy.hideMarques = !reconcile
			runner := h.runner(primaryTestKey)
			mission, data := stageMarqueExecution(t, h, runner)
			key := data.Action.ID
			if changed, err := runner.dispatch(h.ctx, mission, &data); err != nil || changed == reconcile {
				t.Fatalf("issue marque: changed=%v err=%v", changed, err)
			}
			if reconcile {
				runner = h.runner(primaryTestKey)
				refreshMarqueExecution(t, h, runner)
				mission, data = h.mission("__marque")
				if data.Action == nil || data.Action.ID != key {
					t.Fatal("ambiguous issue lost its original immutable action")
				}
				if changed, err := runner.dispatch(h.ctx, mission, &data); err != nil || !changed {
					t.Fatalf("reconcile issued marque: changed=%v err=%v", changed, err)
				}
			}
			mission, data = h.mission("__marque")
			if mission.Phase != "done" || data.Action != nil || data.Marque.IssuedID == "" || data.Marque.ExpiresAt.IsZero() {
				t.Fatalf("marque outcome was not durably consumed: %+v %+v", mission, data)
			}
			issuedID := data.Marque.IssuedID
			refreshMarqueExecution(t, h, runner)
			if !reconcile {
				pending := decode[marquePending](runner.snapshot, "_marque_pending")
				if pending.IssuedID != issuedID || pending.CostPence != 0 || !runner.planningInput(ShipState{}).MarqueZones[marqueExecutionZone] {
					t.Fatalf("unlisted successful issue lost its zone-only guard: %+v", pending)
				}
			}
			if changed, err := runner.driveMarque(h.ctx, mission, &data); err != nil || !changed {
				t.Fatalf("delete completed marque intent: changed=%v err=%v", changed, err)
			}
			h.missingMission("__marque")
			runner = h.runner(primaryTestKey)
			refreshMarqueExecution(t, h, runner)
			if !runner.planningInput(ShipState{}).MarqueZones[marqueExecutionZone] {
				t.Fatal("restart before a matching list record exposed the issued zone")
			}
			proxy.mu.Lock()
			proxy.hideMarques = false
			reads := proxy.quoteReads
			proxy.mu.Unlock()
			refreshMarqueExecution(t, h, runner)
			if pending := decode[marquePending](runner.snapshot, "_marque_pending"); pending.SeaZoneID != "" {
				t.Fatalf("matching authoritative issue did not clear the pending guard: %+v", pending)
			}
			if len(h.pending()) != 0 || h.api.mutationCount("/v1/fleet/issue-marque") != 1 || reads < 3 {
				t.Fatalf("issue was duplicated, remained pending or skipped send-time quotes: pending=%v posts=%d quotes=%d", h.pending(), h.api.mutationCount("/v1/fleet/issue-marque"), reads)
			}
			mutation := h.api.mutationsFor("/v1/fleet/issue-marque")[0]
			if mutation.IdempotencyKey != key {
				t.Fatal("sent issue key differed from its persisted action")
			}
			h.api.mu.Lock()
			balance := h.api.vaultBalance["north"]
			h.api.mu.Unlock()
			if balance != 50_000_000-10_000 {
				t.Fatalf("marque was not charged exactly once from home: %d", balance)
			}
		})
	}

}

func TestMarqueFirstSendRefreshRejectsChangedPriceOrAdmission(t *testing.T) {
	t.Parallel()
	for _, reason := range []string{"price", "final price", "floor", "return"} {
		t.Run(reason, func(t *testing.T) {
			h := newRecoveryHarness(t)
			proxy := newMarqueExecutionAPI(h)
			runner := h.runner(primaryTestKey)
			mission, data := stageMarqueExecution(t, h, runner)
			proxy.mu.Lock()
			switch reason {
			case "price":
				proxy.quote.CostPence = 11_001
			case "final price":
				proxy.finalPriceIncrease = true
			case "floor":
				proxy.quote.FloorApplied = true
			case "return":
				proxy.quote.ExpectedNetPrizePence = 13_999
			}
			proxy.mu.Unlock()
			if changed, err := runner.dispatch(h.ctx, mission, &data); err != nil || !changed {
				t.Fatalf("refuse changed fresh quote: changed=%v err=%v", changed, err)
			}
			_, restored := h.mission("__marque")
			if restored.Action != nil || len(h.pending()) != 0 || h.api.mutationCount("/v1/fleet/issue-marque") != 0 {
				t.Fatal("refused preflight sent or retained a new mutation")
			}
		})
	}

}

func TestMarqueFirstSendAcceptsTenPercentPriceIncrease(t *testing.T) {
	t.Parallel()
	h := newRecoveryHarness(t)
	proxy := newMarqueExecutionAPI(h)
	runner := h.runner(primaryTestKey)
	mission, data := stageMarqueExecution(t, h, runner)
	proxy.mu.Lock()
	proxy.quote.CostPence = 11_000
	proxy.mu.Unlock()
	if changed, err := runner.dispatch(h.ctx, mission, &data); err != nil || !changed {
		t.Fatalf("ten percent boundary should remain admitted: changed=%v err=%v", changed, err)
	}
	if h.api.mutationCount("/v1/fleet/issue-marque") != 1 {
		t.Fatal("admitted ten percent price increase was not issued")
	}

}

func TestMarqueDisabledAfterStagingNeverSends(t *testing.T) {
	t.Parallel()
	h := newRecoveryHarness(t)
	newMarqueExecutionAPI(h)
	runner := h.runner(primaryTestKey)
	stageMarqueExecution(t, h, runner)
	h.marques = false
	runner = h.runner(primaryTestKey)
	refreshMarqueExecution(t, h, runner)
	for range 5 {
		changed, err := runner.work(h.ctx)
		if err != nil {
			t.Fatal(err)
		}
		if !changed {
			break
		}
		refreshMarqueExecution(t, h, runner)
	}
	if h.api.mutationCount("/v1/fleet/issue-marque") != 0 || len(h.pending()) != 0 {
		t.Fatal("persisted marque intent spent after MARQUES_ENABLED was turned off")
	}
	h.missingMission("__marque")

}

func TestMarqueSwitchControlsWorkAdmission(t *testing.T) {
	t.Parallel()
	for _, enabled := range []bool{false, true} {
		t.Run(fmt.Sprintf("enabled=%v", enabled), func(t *testing.T) {
			h := newRecoveryHarness(t)
			proxy := newMarqueExecutionAPI(h)
			// An earlier run with the switch on leaves fresh quotes in the snapshot.
			refreshMarqueExecution(t, h, h.runner(primaryTestKey))
			proxy.mu.Lock()
			savedReads := proxy.quoteReads
			proxy.mu.Unlock()
			if savedReads == 0 {
				t.Fatal("setup run fetched no marque quotes")
			}
			h.marques = enabled
			runner := h.runner(primaryTestKey)
			refreshMarqueExecution(t, h, runner)
			admitted := false
			// work() returns after its first change; the live loop ticks again.
			for range 5 {
				changed, err := runner.work(h.ctx)
				if err != nil {
					t.Fatal(err)
				}
				mission, err := h.db.GetMission(h.ctx, h.account.ID, "__marque")
				if err != nil {
					t.Fatal(err)
				}
				admitted = admitted || mission != nil
				if !changed {
					break
				}
				refreshMarqueExecution(t, h, runner)
			}
			proxy.mu.Lock()
			newReads := proxy.quoteReads - savedReads
			proxy.mu.Unlock()
			if enabled && !admitted {
				t.Fatalf("enabled switch did not admit a marque: holds %v", runner.holding)
			}
			if !enabled && (admitted || newReads != 0 || h.api.mutationCount("/v1/fleet/issue-marque") != 0) {
				t.Fatalf("disabled switch admitted %v, fetched %d quotes", admitted, newReads)
			}
		})
	}

}

func TestMarquePendingActionDoesNotBlockUnrelatedShipMutation(t *testing.T) {
	t.Parallel()
	h := newRecoveryHarness(t)
	newMarqueExecutionAPI(h)
	row := sailingShipRow(recoveryShipBlastoise, "Blastoise", recoveryHomePort, recoveryFarPort)
	row["ship"].(map[string]any)["status"] = "berthed"
	row["voyage"], row["route_legs"] = nil, []any{}
	h.api.setFleet([]map[string]any{row})
	runner := h.runner(primaryTestKey)
	mission, data := stageMarqueExecution(t, h, runner)
	key := data.Action.ID
	if _, err := h.db.BeginAction(h.ctx, &store.Action{ID: key, AccountID: h.account.ID,
		Method: data.Action.Method, Path: data.Action.Path, Body: data.Action.Body}); err != nil {
		t.Fatal(err)
	}
	data.CooldownUntil = time.Now().Add(time.Hour)
	if err := runner.saveMission(h.ctx, mission, &data); err != nil {
		t.Fatal(err)
	}
	if changed, err := runner.work(h.ctx); err != nil || !changed {
		t.Fatalf("pending marque blocked safe ship maintenance: changed=%v err=%v holds=%+v", changed, err, runner.holding)
	}
	_, shipData := h.mission(recoveryShipBlastoise)
	if shipData.Action == nil || shipData.Action.Stage != "refit" {
		t.Fatalf("pending marque prevented a new unrelated refit from being staged: %+v", shipData)
	}
	if changed, err := runner.work(h.ctx); err != nil || !changed {
		t.Fatalf("dispatch unrelated refit: changed=%v err=%v", changed, err)
	}
	if h.api.mutationCount("/v1/shipyard/modify-ship") != 1 || h.api.mutationCount("/v1/fleet/issue-marque") != 0 {
		t.Fatal("safe unrelated mutation did not run independently of the pending issue")
	}
	_, pending := h.mission("__marque")
	if pending.Action == nil || pending.Action.ID != key || len(h.pending()) != 1 {
		t.Fatal("unrelated ship work lost or replaced the unresolved marque action")
	}

}

func TestMarqueHomeFundsProtectsCommittedLoadingCash(t *testing.T) {
	t.Parallel()
	h := newRecoveryHarness(t)
	newMarqueExecutionAPI(h)
	h.api.setVaultBalance("north", 100_000)
	h.api.setVaultBalance("south", 500_000)
	row := sailingShipRow(recoveryShipBlastoise, "Blastoise", recoveryHomePort, recoveryFarPort)
	row["ship"].(map[string]any)["status"] = "berthed"
	row["voyage"], row["route_legs"] = nil, []any{}
	h.api.setFleet([]map[string]any{row})
	runner := h.runner(primaryTestKey)
	refreshMarqueExecution(t, h, runner)
	h.putMission(recoveryShipBlastoise, "committed-loading", "buy", missionData{
		Plan: Plan{Kind: "spot", Origin: recoveryHomePort, Destination: recoveryFarPort,
			Commodity: recoveryGrain, Quantity: 50, BuyLimitPence: 400, Economics: Economics{GameDays: 1}}, CostsKnown: true,
	})
	input, err := runner.committedPlanningInput(h.ctx, ShipState{})
	if err != nil {
		t.Fatal(err)
	}
	protected, _, _, funded := measuredFunding(input.Balances, input.ReserveByRegion, input.Rates)
	if !funded {
		t.Fatal("fixture cannot fund its protected commitments")
	}
	region, available, err := runner.marqueHomeFunds(h.ctx, false)
	if err != nil {
		t.Fatal(err)
	}
	want := max(int64(0), min(input.Balances["north"], protected["north"])-input.ReserveByRegion["north"])
	if region != "north" || available != want || available >= 10_000 {
		t.Fatalf("marque ignored committed loading cash: region=%s available=%d want=%d reserves=%v", region, available, want, input.ReserveByRegion)
	}
	if changed, err := runner.considerMarque(h.ctx); err != nil || changed {
		t.Fatalf("marque spent cash already committed to loading: changed=%v err=%v", changed, err)
	}

}

func TestMarqueUnfundableRegionalBudgetSkipsOptionalSpending(t *testing.T) {
	t.Parallel()
	for _, missingRates := range []bool{false, true} {
		name := "insufficient total cash"
		if missingRates {
			name = "no observed remittance rate"
		}
		t.Run(name, func(t *testing.T) {
			h := newRecoveryHarness(t)
			newMarqueExecutionAPI(h)
			h.api.setVaultBalance("north", 0)
			if missingRates {
				h.api.setRates()
			} else {
				h.api.setVaultBalance("south", 0)
			}
			runner := h.runner(primaryTestKey)
			refreshMarqueExecution(t, h, runner)
			region, available, err := runner.marqueHomeFunds(h.ctx, false)
			if err != nil || region != "north" || available != 0 {
				t.Fatalf("unfundable optional budget should be zero, not an account error: region=%s available=%d err=%v", region, available, err)
			}
			if changed, err := runner.considerMarque(h.ctx); err != nil || changed {
				t.Fatalf("unfundable marque should skip this pass without aborting fleet work: changed=%v err=%v", changed, err)
			}
			h.missingMission("__marque")
		})
	}

}

func TestMarqueExposureIncludesPersistedReturnAndRecoveryLegs(t *testing.T) {
	t.Parallel()
	h := newRecoveryHarness(t)
	newMarqueExecutionAPI(h)
	runner := h.runner(primaryTestKey)
	refreshMarqueExecution(t, h, runner)
	state := &runner.stateCache
	state.Catalog.Ports = []Port{{ID: "home", SeaZoneID: "home-zone"}, {ID: "first"}, {ID: "second"}, {ID: "third"}}
	state.Catalog.SeaZones = []Zone{{ID: "outward"}, {ID: "return"}, {ID: "recovery-one"}, {ID: "recovery-two"}}
	state.HomePortID = "home"
	idle, recovery := ShipState{}, ShipState{}
	idle.Ship.ID, idle.Ship.ShipClass, idle.Ship.Status, idle.Ship.CurrentPortID = "idle", "hoy", "berthed", "home"
	recovery.Ship.ID, recovery.Ship.ShipClass, recovery.Ship.Status, recovery.Ship.CurrentPortID = "recovery", "hoy", "berthed", "home"
	state.Ships = []ShipState{idle, recovery}
	state.Routes = make(map[string]Route)
	for _, terms := range [][3]string{{"home", "first", "outward"}, {"first", "home", "return"}, {"first", "second", "recovery-one"}, {"second", "third", "recovery-two"}} {
		state.Routes[routeKey(terms[0], terms[1], "hoy")] = Route{FromPort: terms[0], ToPort: terms[1],
			Legs: []RouteLeg{{FromNode: terms[0], ToNode: terms[2]}, {FromNode: terms[2], ToNode: terms[1]}}}
	}
	h.putMission("idle", "return-leg", "sailing", missionData{Plan: Plan{Kind: "spot", Origin: "home", Destination: "first"}})
	firstDeadline, secondDeadline := time.Now().Add(time.Hour), time.Now().Add(2*time.Hour)
	h.putMission("recovery", "sealed-recovery", "depart", missionData{
		Plan:    Plan{Kind: "recovery", Origin: "home", Destination: "first"},
		Freight: []freightObligation{{Destination: "second", Deadline: &firstDeadline}, {Destination: "third", Deadline: &secondDeadline}},
	})
	_, exposed, err := runner.marqueEvidence(h.ctx)
	if err != nil {
		t.Fatal(err)
	}
	for _, zone := range []string{"outward", "return", "recovery-one", "recovery-two"} {
		if !exposed[zone] {
			t.Fatalf("persisted itinerary did not protect %s: %v", zone, exposed)
		}
	}

}

func TestMarqueReconciledJournalRestoresIssuedZoneAfterRestart(t *testing.T) {
	t.Parallel()
	h := newRecoveryHarness(t)
	proxy := newMarqueExecutionAPI(h)
	runner := h.runner(primaryTestKey)
	mission, data := stageMarqueExecution(t, h, runner)
	key := data.Action.ID
	if _, err := h.db.BeginAction(h.ctx, &store.Action{ID: key, AccountID: h.account.ID,
		Method: data.Action.Method, Path: data.Action.Path, Body: data.Action.Body}); err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC()
	issued := letterOfMarque{ID: uuid.NewString(), SeaZoneID: marqueExecutionZone, SponsorCompanyID: recoveryCompanyID,
		AddedPiracyBPS: 500, CostPence: 10_000, CreatedAt: now, UpdatedAt: now, ExpiresAt: now.Add(time.Hour)}
	if err := h.db.ReconcileAction(h.ctx, key, encode(map[string]any{"kind": "authoritative_marque", "marque": issued})); err != nil {
		t.Fatal(err)
	}
	runner = h.runner(primaryTestKey)
	refreshMarqueExecution(t, h, runner)
	mission, data = h.mission("__marque")
	if changed, err := runner.dispatch(h.ctx, mission, &data); err != nil || !changed {
		t.Fatalf("restore reconciled journal outcome: changed=%v err=%v", changed, err)
	}
	mission, data = h.mission("__marque")
	if data.Marque.IssuedID != issued.ID || !data.Marque.ExpiresAt.Equal(issued.ExpiresAt) {
		t.Fatalf("reconciled journal lost its issued identity: %+v", data.Marque)
	}
	refreshMarqueExecution(t, h, runner)
	if changed, err := runner.driveMarque(h.ctx, mission, &data); err != nil || !changed {
		t.Fatalf("delete restored control intent: changed=%v err=%v", changed, err)
	}
	runner = h.runner(primaryTestKey)
	refreshMarqueExecution(t, h, runner)
	if !runner.planningInput(ShipState{}).MarqueZones[marqueExecutionZone] {
		t.Fatal("unlisted reconciled issue lost its zone guard after control intent deletion")
	}
	proxy.mu.Lock()
	proxy.issued[key] = issued
	proxy.mu.Unlock()
	refreshMarqueExecution(t, h, runner)
	if pending := decode[marquePending](runner.snapshot, "_marque_pending"); pending.SeaZoneID != "" {
		t.Fatalf("matching issued identity did not retire its pending guard: %+v", pending)
	}

}

func TestMarqueUnknownSailingContinuationHoldsAdmissionAndPreflight(t *testing.T) {
	t.Parallel()
	h := newRecoveryHarness(t)
	proxy := newMarqueExecutionAPI(h)
	proxy.missingReturnRoute = true
	row := sailingShipRow(recoveryShipBlastoise, "Blastoise", recoveryHomePort, recoveryFarPort)
	row["ship"].(map[string]any)["current_port_id"] = nil
	h.api.setFleet([]map[string]any{row})
	h.putMission(recoveryShipBlastoise, "sailing-with-return", "sailing", missionData{
		Plan:    Plan{Kind: "spot", ShipID: recoveryShipBlastoise, Origin: recoveryHomePort, Destination: recoveryFarPort},
		BuyDone: true, CostsKnown: true,
	})
	runner := h.runner(primaryTestKey)
	refreshMarqueExecution(t, h, runner)
	proxy.mu.Lock()
	reads := proxy.returnRouteReads
	proxy.mu.Unlock()
	if reads != 1 {
		t.Fatalf("snapshot did not fetch the persisted return from the sailing arrival: %d requests", reads)
	}
	if changed, err := runner.considerMarque(h.ctx); err != nil || changed {
		t.Fatalf("unknown continuation must hold optional issuance: changed=%v err=%v", changed, err)
	}
	h.missingMission("__marque")
	held := false
	for _, hold := range runner.holding {
		// Control-mission holds are company-level, so they carry no ship ID.
		held = held || hold.ShipID == "" && strings.Contains(hold.Reason, "committed route")
	}
	if !held {
		t.Fatalf("unknown continuation did not produce a plain-language marque hold: %+v", runner.holding)
	}
	proxy.mu.Lock()
	proxy.missingReturnRoute = false
	proxy.mu.Unlock()
	refreshMarqueExecution(t, h, runner)
	if changed, err := runner.considerMarque(h.ctx); err != nil || !changed {
		t.Fatalf("known continuation should permit unrelated-zone issuance: changed=%v err=%v", changed, err)
	}
	_, data := h.mission("__marque")
	delete(runner.stateCache.Routes, routeKey(recoveryFarPort, recoveryHomePort, recoveryHull))
	if err := runner.marquePreflight(h.ctx, &data, false); err == nil || !strings.Contains(err.Error(), "committed route") {
		t.Fatalf("preflight treated missing continuation geometry as no exposure: %v", err)
	}
	if h.api.mutationCount("/v1/fleet/issue-marque") != 0 {
		t.Fatal("unknown continuation allowed an issue mutation")
	}

}

func TestMarqueOnlyDomainRefusalPersistsSixHourZoneBackoff(t *testing.T) {
	t.Parallel()
	for _, status := range []int{http.StatusBadRequest, http.StatusConflict, http.StatusUnauthorized, http.StatusForbidden, http.StatusTooManyRequests} {
		t.Run(http.StatusText(status), func(t *testing.T) {
			h := newRecoveryHarness(t)
			newMarqueExecutionAPI(h)
			runner := h.runner(primaryTestKey)
			mission, data := stageMarqueExecution(t, h, runner)
			before := time.Now()
			changed, err := runner.consumeRejection(h.ctx, mission, &data, status, encode(map[string]string{"code": "refused", "detail": "synthetic refusal"}))
			after := time.Now()
			authStopped := status == http.StatusUnauthorized || status == http.StatusForbidden
			if authStopped && err == nil || !authStopped && (err != nil || !changed) {
				t.Fatalf("unexpected refusal outcome for HTTP %d: changed=%v err=%v", status, changed, err)
			}
			snapshot, err := h.db.Snapshot(h.ctx, h.account.ID)
			if err != nil {
				t.Fatal(err)
			}
			domainRefusal := status == http.StatusBadRequest || status == http.StatusConflict
			for _, source := range []map[string]json.RawMessage{runner.snapshot, snapshot} {
				backoffs := decode[map[string]time.Time](source, "_marque_backoffs")
				until, exists := backoffs[marqueExecutionZone]
				if exists != domainRefusal {
					t.Fatalf("HTTP %d domain backoff = %v, want %v: %v", status, exists, domainRefusal, backoffs)
				}
				if exists && (until.Before(before.Add(6*time.Hour)) || until.After(after.Add(6*time.Hour))) {
					t.Fatalf("HTTP %d refusal did not retain exactly six real hours: %s", status, until)
				}
			}
		})
	}

}
