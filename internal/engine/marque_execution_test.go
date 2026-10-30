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
				proxy.quote.ExpectedNetPrizePence = 14_999
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

func TestMarqueFirstSendRejectsOnePennyPriceIncrease(t *testing.T) {
	t.Parallel()
	h := newRecoveryHarness(t)
	proxy := newMarqueExecutionAPI(h)
	runner := h.runner(primaryTestKey)
	mission, data := stageMarqueExecution(t, h, runner)
	proxy.mu.Lock()
	proxy.quote.CostPence++
	proxy.mu.Unlock()
	if changed, err := runner.dispatch(h.ctx, mission, &data); err != nil || !changed {
		t.Fatalf("one-penny increase should replan without sending: changed=%v err=%v", changed, err)
	}
	if h.api.mutationCount("/v1/fleet/issue-marque") != 0 {
		t.Fatal("price above the admitted quote was issued")
	}
}

func TestMarquePriceIncreaseRetiresAdmissionAndLetsShipsWork(t *testing.T) {
	for _, finalPreflight := range []bool{false, true} {
		t.Run(fmt.Sprintf("final_preflight=%t", finalPreflight), func(t *testing.T) {
			h := newRecoveryHarness(t)
			proxy := newMarqueExecutionAPI(h)
			row := sailingShipRow(recoveryShipBlastoise, "Blastoise", recoveryHomePort, recoveryFarPort)
			row["ship"].(map[string]any)["status"] = "berthed"
			row["voyage"], row["route_legs"] = nil, []any{}
			h.api.setFleet([]map[string]any{row})
			runner := h.runner(primaryTestKey)
			mission, data := stageMarqueExecution(t, h, runner)
			proxy.mu.Lock()
			if finalPreflight {
				proxy.finalPriceIncrease = true
			} else {
				proxy.quote.CostPence++
			}
			proxy.mu.Unlock()
			before := time.Now().UTC()
			if changed, err := runner.dispatch(h.ctx, mission, &data); err != nil || !changed {
				t.Fatalf("retire increased quote without sending: changed=%t err=%v", changed, err)
			}
			mission, data = h.mission("__marque")
			if data.Action != nil || data.Marque != nil || data.Sequence != 1 || mission.Phase != "done" ||
				data.CooldownUntil.Before(before.Add(30*time.Minute)) || data.CooldownUntil.After(time.Now().Add(30*time.Minute)) {
				t.Fatalf("price rejection did not durably retire admission with a bounded delay: phase=%s data=%+v", mission.Phase, data)
			}
			proxy.mu.Lock()
			reads := proxy.quoteReads
			proxy.mu.Unlock()
			restarted := h.runner(primaryTestKey)
			refreshMarqueExecution(t, h, restarted)
			for range 2 {
				if changed, err := restarted.work(h.ctx); err != nil || !changed {
					t.Fatalf("retired marque blocked unrelated ship work: changed=%t err=%v holds=%v", changed, err, restarted.holding)
				}
			}
			_, retired := h.mission("__marque")
			if retired.Action != nil || retired.Marque != nil || retired.Sequence != 1 || !retired.CooldownUntil.Equal(data.CooldownUntil) {
				t.Fatal("restart restaged the stale admission or lost its retry deadline")
			}
			proxy.mu.Lock()
			newReads := proxy.quoteReads
			proxy.mu.Unlock()
			if newReads != reads || h.api.mutationCount("/v1/fleet/issue-marque") != 0 || len(h.pending()) != 0 ||
				h.api.mutationCount("/v1/shipyard/modify-ship") != 1 {
				t.Fatal("price rejection repeated requotes or payments, or starved the ship's refit")
			}

			mission, retired = h.mission("__marque")
			retired.CooldownUntil = time.Now().Add(-time.Second)
			if err := restarted.saveMission(h.ctx, mission, &retired); err != nil {
				t.Fatal(err)
			}
			if changed, err := restarted.driveMarque(h.ctx, mission, &retired); err != nil || !changed {
				t.Fatalf("expired retry delay did not release the retired mission: changed=%t err=%v", changed, err)
			}
			meta := restarted.meta["marque_quotes/"+marqueExecutionZone]
			meta.UpdatedAt, meta.AttemptedAt = time.Now().Add(-31*time.Minute), time.Now().Add(-31*time.Minute)
			restarted.meta["marque_quotes/"+marqueExecutionZone] = meta
			if changed, err := restarted.considerMarque(h.ctx); err != nil || changed {
				t.Fatalf("expired cached quote was admitted again: changed=%t err=%v", changed, err)
			}
			proxy.mu.Lock()
			proxy.finalPriceIncrease = false
			proxy.quote.CostPence = 11_001
			proxy.mu.Unlock()
			mission, admitted := stageMarqueExecution(t, h, restarted)
			if admitted.Marque.Quote.CostPence != 11_001 {
				t.Fatal("fresh price did not pass through a new frozen admission")
			}
			if _, err := restarted.dispatch(h.ctx, mission, &admitted); err != nil {
				t.Fatal(err)
			}
			if h.api.mutationCount("/v1/fleet/issue-marque") != 1 || len(h.pending()) != 0 {
				t.Fatal("fresh admission failed to issue exactly one paid marque")
			}
		})
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

func TestMarqueActiveHistorySurvivesListOmissionAndAwaitsFinalPrize(t *testing.T) {
	t.Parallel()
	h := newRecoveryHarness(t)
	proxy := newMarqueExecutionAPI(h)
	now := time.Now().UTC()
	issued := letterOfMarque{ID: uuid.NewString(), SeaZoneID: marqueExecutionZone, AddedPiracyBPS: 500,
		CostPence: 90_000, CreatedAt: now.Add(-time.Hour), UpdatedAt: now, ExpiresAt: now.Add(time.Hour)}
	proxy.mu.Lock()
	proxy.issued["observed"] = issued
	proxy.mu.Unlock()
	runner := h.runner(primaryTestKey)
	refreshMarqueExecution(t, h, runner)
	proxy.mu.Lock()
	proxy.hideMarques = true
	proxy.mu.Unlock()
	runner = h.runner(primaryTestKey)
	refreshMarqueExecution(t, h, runner)
	input, _, err := runner.marqueEvidence(h.ctx)
	if err != nil || input.InForceCount != 1 || input.InForceCostPence != issued.CostPence {
		t.Fatalf("omitted active issue lost its permanent charge: %+v %v", input, err)
	}
	if !runner.planningInput(ShipState{}).MarqueZones[issued.SeaZoneID] {
		t.Fatal("list omission reopened our own active marque zone to the fleet")
	}
	if changed, err := runner.considerMarque(h.ctx); err != nil || changed {
		t.Fatalf("list omission allowed a second marque: %t %v", changed, err)
	}
	data := missionData{Marque: &marqueIntent{Quote: proxy.quote, BankingRegion: "north"}}
	if err := runner.marquePreflight(h.ctx, &data, false); err == nil || !strings.Contains(err.Error(), "already active") {
		t.Fatalf("send preflight ignored omitted active issue: %v", err)
	}
	downstream := h.api.server.Config.Handler
	h.api.server.Config.Handler = http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		if req.URL.Path == "/v1/world/clock" {
			h.api.mu.Lock()
			doc := h.api.clockDocument()
			h.api.mu.Unlock()
			doc["real_utc"] = issued.ExpiresAt.Add(time.Hour)
			h.api.writeJSON(w, http.StatusOK, doc)
			return
		}
		downstream.ServeHTTP(w, req)
	})
	refreshMarqueExecution(t, h, runner)
	if _, _, err := runner.marqueEvidence(h.ctx); err == nil || !strings.Contains(err.Error(), "final prize") {
		t.Fatalf("unlisted expiry released unknown lifetime loss: %v", err)
	}
	proxy.mu.Lock()
	issued.PrizeEarnedPence = issued.CostPence
	issued.UpdatedAt = issued.ExpiresAt.Add(time.Hour)
	proxy.issued["observed"] = issued
	proxy.hideMarques = false
	proxy.mu.Unlock()
	refreshMarqueExecution(t, h, runner)
	if changed, err := runner.considerMarque(h.ctx); err != nil || !changed {
		t.Fatalf("authoritative profitable expiry did not release admission: %t %v", changed, err)
	}
}

func TestMarqueLossStopSurvivesLaterPrizeCorrections(t *testing.T) {
	t.Parallel()
	h := newRecoveryHarness(t)
	proxy := newMarqueExecutionAPI(h)
	now := time.Now().UTC()
	proxy.mu.Lock()
	for range 3 {
		id := uuid.NewString()
		proxy.issued[id] = letterOfMarque{ID: id, SeaZoneID: marqueExecutionZone, AddedPiracyBPS: 500,
			CostPence: 10_000, PrizeEarnedPence: 9_900, CreatedAt: now.Add(-2 * time.Hour),
			ExpiresAt: now.Add(-time.Hour), UpdatedAt: now}
	}
	proxy.mu.Unlock()
	runner := h.runner(primaryTestKey)
	refreshMarqueExecution(t, h, runner)
	proxy.mu.Lock()
	for id, issued := range proxy.issued {
		issued.PrizeEarnedPence = 10_100
		proxy.issued[id] = issued
	}
	proxy.mu.Unlock()
	runner = h.runner(primaryTestKey)
	refreshMarqueExecution(t, h, runner)
	input, _, err := runner.marqueEvidence(h.ctx)
	if err != nil || !input.Stopped || input.ExpiredPrizePence <= input.ExpiredCostPence {
		t.Fatalf("later authoritative profit erased the loss stop: %+v %v", input, err)
	}
	if changed, err := runner.considerMarque(h.ctx); err != nil || changed {
		t.Fatalf("permanently stopped pilot resumed: %t %v", changed, err)
	}
}

// The owner's stop counts individually losing marques; a large winner does not
// cancel three losers, and one large loser among winners is not three losers.
func TestMarqueLossStopCountsIndividuallyLosingMarques(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name    string
		prizes  []int64
		losing  int
		stopped bool
	}{
		{name: "three losers after a large winner", prizes: []int64{50_000, 9_900, 9_900, 9_900}, losing: 3, stopped: true},
		{name: "one large loser among winners", prizes: []int64{0, 10_100, 10_100}, losing: 1, stopped: false},
		{name: "two losers", prizes: []int64{9_900, 9_900, 10_000}, losing: 2, stopped: false},
		{name: "three at cost are not losers", prizes: []int64{10_000, 10_000, 10_000}, losing: 0, stopped: false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			h := newRecoveryHarness(t)
			proxy := newMarqueExecutionAPI(h)
			now := time.Now().UTC()
			proxy.mu.Lock()
			for _, prize := range tc.prizes {
				id := uuid.NewString()
				proxy.issued[id] = letterOfMarque{ID: id, SeaZoneID: marqueExecutionZone, AddedPiracyBPS: 500,
					CostPence: 10_000, PrizeEarnedPence: prize, CreatedAt: now.Add(-2 * time.Hour),
					ExpiresAt: now.Add(-time.Hour), UpdatedAt: now}
			}
			proxy.mu.Unlock()
			runner := h.runner(primaryTestKey)
			refreshMarqueExecution(t, h, runner)
			input, _, err := runner.marqueEvidence(h.ctx)
			if err != nil || input.Stopped != tc.stopped || input.LosingCount != tc.losing {
				t.Fatalf("stopped = %t losing = %d, want %t %d: %+v %v",
					input.Stopped, input.LosingCount, tc.stopped, tc.losing, input, err)
			}
		})
	}
}

func TestMarqueAbsentHistoryRequiresExpiredReplayWindowAndFreshList(t *testing.T) {
	t.Parallel()
	h := newRecoveryHarness(t)
	newMarqueExecutionAPI(h)
	runner := h.runner(primaryTestKey)
	mission, data := stageMarqueExecution(t, h, runner)
	action, err := h.db.BeginAction(h.ctx, &store.Action{ID: data.Action.ID, AccountID: h.account.ID,
		Method: data.Action.Method, Path: data.Action.Path, Body: data.Action.Body})
	if err != nil {
		t.Fatal(err)
	}
	if changed, err := runner.reconcileMarque(h.ctx, mission, &data, action); err != nil || changed {
		t.Fatalf("absence inside the replay window was treated as final: %t %v", changed, err)
	}
	action.CreatedAt = time.Now().Add(-2 * store.ActionReplayWindow)
	meta := runner.meta["marques"]
	meta.Stale = true
	runner.meta["marques"] = meta
	if changed, err := runner.reconcileMarque(h.ctx, mission, &data, action); err != nil || changed {
		t.Fatalf("stale history retired an ambiguous old issue: %t %v", changed, err)
	}
	if data.Action == nil || len(h.pending()) != 1 || h.api.mutationCount("/v1/fleet/issue-marque") != 0 {
		t.Fatal("unresolved absent issue lost its journal identity or was resent")
	}
}
