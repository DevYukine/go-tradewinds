package engine

import (
	"context"
	"encoding/json"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/DevYukine/go-tradewinds/internal/store"
	"go.uber.org/zap"
	"go.uber.org/zap/zaptest/observer"
)

func probeHistoryFixture() sourcingProbeHistory {
	return sourcingProbeHistory{Version: 2, Sources: make(map[string]probeSourceHistory), Ships: make(map[string]probeShipHistory)}
}

func probeHistoryLiveInput(t *testing.T) PlanningInput {
	t.Helper()
	in := sourcingInputFixture(t)
	in.Now = time.Now().UTC()
	in.Clock.RealUTC = in.Now
	for key := range in.Metadata {
		in.Metadata[key] = sourcingFreshMeta(in.Now)
	}
	return in
}

func probeHistoryRunner(r *Runner, in PlanningInput) {
	r.stateCache.Ships = []ShipState{in.Ship}
	r.stateCache.Clock = in.Clock
	r.stateCache.Contracts = in.Contracts
}

func probeHistoryRow(in PlanningInput, source string, at time.Time) store.Settlement {
	return store.Settlement{Kind: "relocation", ShipID: in.Ship.Ship.ID, Origin: "le_havre", Destination: source,
		Commodity: "sawn_planks", CompletedAt: at, ProbeOutcome: store.ProbeOutcomeFailed,
		Lane: source + "|portsmouth|sawn_planks|probe:0:3000"}
}

func putProbeHistorySettlement(t *testing.T, h *recoveryHarness, row store.Settlement) {
	t.Helper()
	row.AccountID, row.ActionID, row.MissionID = h.account.ID, row.ID+"-action", row.ID
	row.CostsKnown = true
	if row.StartedAt.IsZero() {
		row.StartedAt = row.CompletedAt.Add(-time.Hour)
	}
	if _, err := h.db.BeginAction(h.ctx, &store.Action{ID: row.ActionID, AccountID: h.account.ID, Method: http.MethodPost, Path: "/v1/test", Body: json.RawMessage(`{}`)}); err != nil {
		t.Fatal(err)
	}
	if err := h.db.CompleteAction(h.ctx, row.ActionID, http.StatusOK, json.RawMessage(`{}`), nil); err != nil {
		t.Fatal(err)
	}
	if err := h.db.PutSettlement(h.ctx, &row); err != nil {
		t.Fatal(err)
	}
}

func TestProbeHistoryCatalogueFailureSurvivesRestartAndContractChange(t *testing.T) {
	t.Parallel()
	h := newRecoveryHarness(t)
	in := probeHistoryLiveInput(t)
	row := probeHistoryRow(in, "amsterdam", in.Now.Add(-time.Hour))
	row.ID = "failed-catalogue"
	putProbeHistorySettlement(t, h, row)
	for range 2 {
		runner := h.runner(primaryTestKey)
		probeHistoryRunner(runner, in)
		runner.refreshSourcingHistory(h.ctx, in)
		runner.refreshSourcingBudget(h.ctx, in)
		runner.applySourcingHistory(&in)
		if in.SourcingHistoryError != "" {
			t.Fatal(in.SourcingHistoryError)
		}
		charter := in.Contracts[0]
		charter.ID, charter.OriginPortID = "different-charter", "different-delivery-port"
		if got := sourcingProbeRejection(in, "amsterdam", charter, 0); got != failedCatalogueProbeReason {
			t.Fatalf("same failed source/commodity was admitted after restart or contract change: %q", got)
		}
		if got := sourcingProbeRejection(in, "amsterdam", Contract{CommodityID: "canvas"}, 0); got != "" {
			t.Fatalf("failure blocked an untried commodity: %q", got)
		}
	}
	in.Now = in.Now.Add(23 * time.Hour)
	if got := sourcingProbeRejection(in, "amsterdam", in.Contracts[0], 0); got == failedCatalogueProbeReason {
		t.Fatal("24-real-hour source ban did not expire at the boundary")
	}
}

func TestProbeHistoryOnlyFreshAskChangesEvidenceAndKeepsLaneCooldown(t *testing.T) {
	t.Parallel()
	in := sourcingInputFixture(t)
	in.SourcingProbeHistory = probeHistoryFixture()
	key := probeSourceKey("amsterdam", "sawn_planks")
	in.SourcingProbeHistory.Sources[key] = probeSourceHistory{FailedAt: in.Now.Add(-time.Hour), CatalogueFailedAt: in.Now.Add(-time.Hour)}
	in.SourcingProbeHistory.Ships[in.Ship.Ship.ID] = probeShipHistory{Failures: 2}
	in.Contracts = append(in.Contracts, Contract{ID: "new-paid-freight", ContractType: "freight", OriginPortID: "amsterdam", Status: "open", QuantityRequired: 2, RewardPence: 100_000})
	charter := in.Contracts[0]
	if got := sourcingProbeRejection(in, "amsterdam", charter, 0); got != failedCatalogueProbeReason {
		t.Fatalf("a freight contract or accessible quay ask rearmed a failed catalogue lead: %q", got)
	}
	if got := sourcingProbeRejection(in, "amsterdam", charter, 800); got != "" {
		t.Fatalf("a fresh public ask was not reconsidered: %q", got)
	}
	in.SourcingProbeCompletions = map[string]time.Time{
		sourcingEvidenceKey("amsterdam", charter, 800): in.Now.Add(-time.Minute),
		sourcingEvidenceKey("amsterdam", charter, 0):   in.Now.Add(-time.Minute),
	}
	in.SourcingIdleEligible = true
	if got := sourcingProbeRejection(in, "amsterdam", charter, 800); !strings.Contains(got, "24 real minutes") {
		t.Fatalf("fresh public ask bypassed its same-key cooldown: %q", got)
	}
	in.SourcingProbeHistory.Sources[key] = probeSourceHistory{ConvertedAt: in.Now.Add(-2 * time.Hour), CatalogueFailedAt: in.Now.Add(-time.Hour)}
	in.SourcingProbeHistory.Ships[in.Ship.Ship.ID] = probeShipHistory{}
	if got := sourcingProbeRejection(in, "amsterdam", charter, 0); !strings.Contains(got, "24 real minutes") {
		t.Fatalf("recent conversion bypassed the catalogue lane cooldown: %q", got)
	}
	in.SourcingIdleEligible = false
	if got := sourcingProbeRejection(in, "amsterdam", charter, 0); !strings.Contains(got, "six observed idle game hours") {
		t.Fatalf("recent conversion bypassed the existing idle gate: %q", got)
	}
}

func TestProbeHistoryAbortedAndUninspectedRelocationsAreNotSourceFailures(t *testing.T) {
	t.Parallel()
	in := sourcingInputFixture(t)
	for _, change := range []func(*store.Settlement){
		func(row *store.Settlement) { row.Origin = row.Destination },
		func(row *store.Settlement) { row.Destination = "different-port" },
		func(row *store.Settlement) { row.ProbeOutcome = "" }, // Target closed or expired before inspection.
	} {
		history := probeHistoryFixture()
		row := probeHistoryRow(in, "amsterdam", in.Now.Add(-time.Hour))
		change(&row)
		history.settlement(row, nil)
		if history.Ships[in.Ship.Ship.ID].Failures != 0 || !history.Sources[probeSourceKey("amsterdam", "sawn_planks")].CatalogueFailedAt.IsZero() {
			t.Fatalf("an aborted or uninspected relocation became a source failure: %+v", row)
		}
	}
}

func TestProbeHistoryConversionRequiresNextActualDepartureFromSource(t *testing.T) {
	t.Parallel()
	in := sourcingInputFixture(t)
	history := probeHistoryFixture()
	row := probeHistoryRow(in, "amsterdam", in.Now.Add(-time.Hour))
	row.ProbeOutcome = store.ProbeOutcomeContinuation
	history.settlement(row, nil)
	if history.Ships[in.Ship.Ship.ID].Failures != 0 {
		t.Fatal("a planned continuation was recorded as a failure before departure")
	}
	history.departure(in.Ship.Ship.ID, "amsterdam", in.Now.Add(-30*time.Minute), false, shipIdleState{})
	if history.Ships[in.Ship.Ship.ID].Failures != 1 {
		t.Fatal("an inspected continuation that actually departed empty was not a failed probe")
	}
	history.departure(in.Ship.Ship.ID, "amsterdam", in.Now, true, shipIdleState{})
	if !history.Sources[probeSourceKey("amsterdam", "sawn_planks")].ConvertedAt.IsZero() {
		t.Fatal("a later unrelated load converted a probe whose next departure was empty")
	}
	if history.Ships[in.Ship.Ship.ID].Failures != 0 {
		t.Fatal("actual laden departure did not reset the ship streak")
	}
	// Quantity in a commercial settlement is not proof of any departure.
	history.settlement(store.Settlement{Kind: "charter", ShipID: in.Ship.Ship.ID, Origin: "amsterdam", Commodity: "sawn_planks", QuantityTons: 10, StartedAt: in.Now}, nil)
	if !history.Sources[probeSourceKey("amsterdam", "sawn_planks")].ConvertedAt.IsZero() {
		t.Fatal("commercial accounting fabricated a source conversion")
	}
}

func TestProbeHistoryRanksRecentConvertedFreshAskUntriedFailed(t *testing.T) {
	t.Parallel()
	in := sourcingInputFixture(t)
	in.SourcingProbeHistory = probeHistoryFixture()
	in.SourcingProbeHistory.Sources[probeSourceKey("converted", "sawn_planks")] = probeSourceHistory{ConvertedAt: in.Now.Add(-6 * 24 * time.Hour)}
	in.SourcingProbeHistory.Sources[probeSourceKey("failed-ask", "sawn_planks")] = probeSourceHistory{FailedAt: in.Now.Add(-time.Hour)}
	in.SourcingProbeHistory.Sources[probeSourceKey("failed", "sawn_planks")] = probeSourceHistory{FailedAt: in.Now.Add(-time.Hour)}
	plans := []Plan{
		{Accepted: true, Destination: "failed", Commodity: "sawn_planks", Economics: Economics{ExpectedNetPence: -1}},
		{Accepted: true, Destination: "untried", Commodity: "sawn_planks", Economics: Economics{ExpectedNetPence: -10}},
		{Accepted: true, Destination: "failed-ask", Commodity: "sawn_planks", PriceSource: sourcingAskEvidence, Economics: Economics{ExpectedNetPence: -100}},
		{Accepted: true, Destination: "converted", Commodity: "sawn_planks", Economics: Economics{ExpectedNetPence: -1_000}},
	}
	sortSourcingPlans(in, plans)
	for i, want := range []string{"converted", "failed-ask", "untried", "failed"} {
		if plans[i].Destination != want {
			t.Fatalf("source evidence lost to cost ordering: %+v", plans)
		}
	}
	old := in.Now.Add(-8 * 24 * time.Hour)
	in.SourcingProbeHistory.Sources[probeSourceKey("old", "sawn_planks")] = probeSourceHistory{ConvertedAt: old, FailedAt: old}
	if got := in.SourcingProbeHistory.sourceRank(in.Now, "old", "sawn_planks", false); got != 2 {
		t.Fatalf("an eight-day-old outcome permanently changed source rank: %d", got)
	}
	observed := probeSourceHistory{ConvertedAt: in.Now.Add(-7 * 24 * time.Hour), CatalogueFailedAt: in.Now.Add(-time.Hour)}
	in.SourcingProbeHistory.Sources[probeSourceKey("amsterdam", "sawn_planks")] = observed
	if got := sourcingProbeRejection(in, "amsterdam", in.Contracts[0], 0); got != "" {
		t.Fatalf("a conversion at the seven-day boundary did not exempt the source ban: %q", got)
	}
	in.Now = in.Now.Add(time.Nanosecond)
	if got := sourcingProbeRejection(in, "amsterdam", in.Contracts[0], 0); got != failedCatalogueProbeReason {
		t.Fatalf("an expired conversion still exempted the source ban: %q", got)
	}
}

func TestProbeHistoryStreakUsesExistingCoherentIdleClock(t *testing.T) {
	t.Parallel()
	in := sourcingInputFixture(t)
	history := probeHistoryFixture()
	history.Ships[in.Ship.Ship.ID] = probeShipHistory{Failures: 2, IdlePort: in.Ship.Ship.CurrentPortID, IdleHours: 10}
	runner := &Runner{snapshot: make(map[string]json.RawMessage)}
	idle := shipIdleState{PortID: in.Ship.Ship.CurrentPortID, EarnedGameHours: 16, Validated: true, LastValidatedGameHour: in.Clock.Calendar.TotalGameHours, LastClockUTC: in.Clock.RealUTC}
	apply := func() {
		runner.snapshot["_ship_idle"] = encode(map[string]shipIdleState{in.Ship.Ship.ID: idle})
		if runner.shipIdleEligible(in) {
			history.releaseIdleStreak(in.Ship.Ship.ID, idle)
		}
	}
	in.Clock.Paused = true
	apply()
	in.Clock.Paused = false
	idle.ClockRegressed = true
	apply()
	idle.ClockRegressed, idle.Validated = false, false
	apply()
	if history.Ships[in.Ship.Ship.ID].Failures != 2 {
		t.Fatal("paused, regressed or unvalidated clocks cleared the guard")
	}
	idle.Validated, idle.EarnedGameHours = true, 15
	apply()
	if history.Ships[in.Ship.Ship.ID].Failures != 2 {
		t.Fatal("five newly earned coherent hours cleared the six-hour guard")
	}
	idle.EarnedGameHours = 16
	apply()
	if history.Ships[in.Ship.Ship.ID].Failures != 0 {
		t.Fatal("six newly earned coherent hours did not clear the guard")
	}
}

func TestProbeHistoryHostTimeConversionCacheAndRestart(t *testing.T) {
	t.Parallel()
	h := newRecoveryHarness(t)
	in := probeHistoryLiveInput(t)
	in.CompanyID = h.account.CompanyID
	for i, source := range []string{"calais", "amsterdam"} {
		row := probeHistoryRow(in, source, in.Now.Add(time.Duration(i-2)*time.Hour))
		row.ID = "probe-" + source
		putProbeHistorySettlement(t, h, row)
	}
	runner := h.runner(primaryTestKey)
	probeHistoryRunner(runner, in)
	runner.refreshSourcingHistory(h.ctx, in)
	runner.refreshSourcingBudget(h.ctx, in)
	runner.applySourcingHistory(&in)
	if in.SourcingProbeHistory.Ships[in.Ship.Ship.ID].Failures != 2 {
		t.Fatal("verified inspected settlements did not reconstruct the streak")
	}
	body := func(company string) json.RawMessage {
		// Deliberately earlier than probe completion: server time must not
		// order host-time accounting, even if it drifts behind the host.
		return encode(map[string]any{"company_id": company, "timestamp": in.Now.Add(-3 * time.Hour), "payload": map[string]any{"ship_id": in.Ship.Ship.ID, "origin_port_id": "amsterdam", "cargo_manifest": map[string]int64{"sawn_planks": 1}}})
	}
	if err := h.db.RecordEvent(h.ctx, h.account.ID, "foreign-departure", "ship_departed", body("another-company")); err != nil {
		t.Fatal(err)
	}
	runner.refreshSourcingHistory(h.ctx, in)
	runner.refreshSourcingBudget(h.ctx, in)
	runner.applySourcingHistory(&in)
	if in.SourcingProbeHistory.Ships[in.Ship.Ship.ID].Failures != 2 {
		t.Fatal("another company's public departure reset our ship")
	}
	if err := h.db.RecordEvent(h.ctx, h.account.ID, "own-laden-departure", "ship_departed", body(in.CompanyID)); err != nil {
		t.Fatal(err)
	}
	runner.applySourcingHistory(&in)
	if in.SourcingProbeHistory.Ships[in.Ship.Ship.ID].Failures != 2 {
		t.Fatal("an input builder fetched new departure history instead of the cached frame")
	}
	in.Now = time.Now().UTC()
	runner.refreshSourcingHistory(h.ctx, in)
	runner.refreshSourcingBudget(h.ctx, in)
	runner.applySourcingHistory(&in)
	if in.SourcingHistoryError != "" || in.SourcingProbeHistory.Ships[in.Ship.Ship.ID].Failures != 0 || !recentSourceConversion(in.SourcingProbeHistory.Sources[probeSourceKey("amsterdam", "sawn_planks")], in.Now) {
		t.Fatalf("host/server skew hid an actual laden conversion: %+v", in.SourcingProbeHistory)
	}
	if err := h.db.SaveSnapshot(h.ctx, h.account.ID, runner.snapshot); err != nil {
		t.Fatal(err)
	}
	runner = h.runner(primaryTestKey)
	probeHistoryRunner(runner, in)
	runner.refreshSourcingHistory(h.ctx, in)
	runner.refreshSourcingBudget(h.ctx, in)
	runner.applySourcingHistory(&in)
	if in.SourcingProbeHistory.Ships[in.Ship.Ship.ID].Failures != 0 || !recentSourceConversion(in.SourcingProbeHistory.Sources[probeSourceKey("amsterdam", "sawn_planks")], in.Now) {
		t.Fatal("restart erased the observed laden conversion")
	}
}

func TestProbeHistoryDecodeFailureBlocksOnlyAdmissionNotRefresh(t *testing.T) {
	t.Parallel()
	h := newRecoveryHarness(t)
	runner := h.runner(primaryTestKey)
	runner.snapshot["_sourcing_probe_history"] = json.RawMessage(`{"version":2,"sources":`)
	if err := runner.refresh(h.ctx); err != nil {
		t.Fatalf("sourcing decode error escaped into fleet refresh: %v", err)
	}
	if runner.sourcingHistory.Error == "" || !runner.fresh("ships") {
		t.Fatal("refresh did not retain authoritative fleet state and a sourcing hold")
	}
	in := sourcingInputFixture(t)
	in.SourcingLoadingBlocked = true
	runner.applySourcingHistory(&in)
	if got := sourcingAdmissionRejection(in); !strings.Contains(got, "sourcing history unavailable") {
		t.Fatalf("new sourcing was not held after decode failure: %q", got)
	}
	in.Ship.Ship.CurrentPortID = "amsterdam"
	ordinary := PlanVoyages(in)
	if len(ordinary) == 0 || !ordinary[0].Accepted {
		t.Fatal("sourcing-history hold blocked an otherwise profitable ordinary charter")
	}
	decisions, err := h.db.History(h.ctx, h.account.ID, "decisions", 20)
	if err != nil {
		t.Fatal(err)
	}
	encoded, _ := json.Marshal(decisions)
	if !strings.Contains(string(encoded), "sourcing history unavailable") {
		t.Fatal("history decode failure did not record its hold reason")
	}
	cancelled, cancel := context.WithCancel(h.ctx)
	cancel()
	runner.snapshot["_sourcing_probe_history"] = encode(probeHistoryFixture())
	runner.refreshSourcingHistory(cancelled, in)
	if !strings.Contains(runner.sourcingHistory.Error, "sourcing history unavailable") {
		t.Fatal("a history query failure did not close new sourcing admission")
	}
}

func TestProbeHistorySettlementPersistsQualifiedInspectionOutcome(t *testing.T) {
	for _, tc := range []struct {
		name                    string
		inspected, continuation bool
		want                    string
	}{
		{"expired-target", false, false, ""},
		{"inspected-without-continuation", true, false, store.ProbeOutcomeFailed},
		{"executable-handoff", true, true, store.ProbeOutcomeContinuation},
	} {
		t.Run(tc.name, func(t *testing.T) {
			h := newRecoveryHarness(t)
			in := probeHistoryLiveInput(t)
			runner := h.runner(primaryTestKey)
			data := missionData{Plan: Plan{Kind: "relocation", Origin: "le_havre", Destination: "amsterdam", Commodity: "sawn_planks"},
				Relocation: &relocationIntent{SourceInspected: tc.inspected}, StartedAt: in.Now.Add(-time.Hour), LastActionID: "probe-boundary", CostsKnown: true}
			if tc.continuation {
				data.SourcingHandoff = &relocationIntent{}
			}
			h.putMission(in.Ship.Ship.ID, "qualified-probe", "closing", data)
			mission, loaded, err := runner.loadMission(h.ctx, in.Ship.Ship.ID)
			if err != nil {
				t.Fatal(err)
			}
			if _, err := runner.finishMission(h.ctx, in.Ship, mission, &loaded); err != nil {
				t.Fatal(err)
			}
			if loaded.Settlement == nil || loaded.Settlement.ProbeOutcome != tc.want {
				t.Fatalf("wrong durable inspection outcome: %+v", loaded.Settlement)
			}
		})
	}
}

func TestProbeHistoryUninspectedEmptyLegsCountOnlyShipStreak(t *testing.T) {
	t.Parallel()
	in := sourcingInputFixture(t)
	history := probeHistoryFixture()
	for i, source := range []string{"calais", "amsterdam"} {
		at := in.Now.Add(time.Duration(i-2) * time.Hour)
		row := probeHistoryRow(in, source, at)
		row.ProbeOutcome = "" // Demand closed en route, before a qualified inspection.
		history.settlement(row, nil)
		history.departure(in.Ship.Ship.ID, source, at.Add(time.Minute), false, shipIdleState{})
		if history.Ships[in.Ship.Ship.ID].Failures != i+1 {
			t.Fatal("an uninspected probe's empty onward departure escaped the ship streak")
		}
		observed := history.Sources[probeSourceKey(source, "sawn_planks")]
		if !observed.FailedAt.IsZero() || !observed.CatalogueFailedAt.IsZero() {
			t.Fatal("an uninspected probe invented source-wide failure evidence")
		}
	}
	in.SourcingProbeHistory = history
	if got := sourcingProbeRejection(in, "untried-source", in.Contracts[0], 0); got != failedProbeStreakReason {
		t.Fatalf("two unconverted empty probes did not block the next catalogue leg: %q", got)
	}
	history = probeHistoryFixture()
	row := probeHistoryRow(in, "amsterdam", in.Now.Add(-time.Hour))
	history.settlement(row, nil)
	history.departure(in.Ship.Ship.ID, "amsterdam", in.Now, false, shipIdleState{})
	if history.Ships[in.Ship.Ship.ID].Failures != 1 {
		t.Fatal("an inspected failure was counted twice at its empty onward departure")
	}
}

func TestProbeHistoryEarlierPortAndPreinspectionDeparturesPreservePending(t *testing.T) {
	t.Parallel()
	in := sourcingInputFixture(t)
	history := probeHistoryFixture()
	row := probeHistoryRow(in, "amsterdam", in.Now.Add(-time.Hour))
	history.settlement(row, nil)
	for _, departure := range []struct {
		origin string
		at     time.Time
	}{
		{"amsterdam", row.CompletedAt.Add(-time.Minute)},
		{"amsterdam", row.CompletedAt},
		{"le_havre", row.CompletedAt.Add(time.Minute)}, // Earlier-port event ingested after stream recovery.
	} {
		history.departure(in.Ship.Ship.ID, departure.origin, departure.at, false, shipIdleState{})
		if history.Ships[in.Ship.Ship.ID].Pending == nil {
			t.Fatal("unrelated or preinspection departure erased a source's pending conversion")
		}
	}
	history.departure(in.Ship.Ship.ID, "amsterdam", in.Now, true, shipIdleState{})
	if history.Ships[in.Ship.Ship.ID].Pending != nil || history.Ships[in.Ship.Ship.ID].Failures != 0 ||
		!history.Sources[probeSourceKey("amsterdam", "sawn_planks")].ConvertedAt.Equal(in.Now) {
		t.Fatal("a matching later laden departure did not convert the preserved probe")
	}
}

func TestProbeHistoryMalformedDepartureSkipsAndAdvancesCursor(t *testing.T) {
	t.Parallel()
	h := newRecoveryHarness(t)
	in := probeHistoryLiveInput(t)
	in.CompanyID = h.account.CompanyID
	row := probeHistoryRow(in, "amsterdam", in.Now.Add(-time.Hour))
	row.ID = "malformed-event-probe"
	putProbeHistorySettlement(t, h, row)
	runner := h.runner(primaryTestKey)
	probeHistoryRunner(runner, in)
	core, logs := observer.New(zap.WarnLevel)
	runner.log = zap.New(core)
	for _, event := range []struct {
		id, company string
		quantity    any
	}{
		{"foreign-malformed-cargo", "another-company", 1.5},
		{"own-malformed-cargo", in.CompanyID, 1.5},
		{"own-valid-cargo", in.CompanyID, int64(1)},
	} {
		body := encode(map[string]any{"company_id": event.company, "payload": map[string]any{
			"ship_id": in.Ship.Ship.ID, "origin_port_id": "amsterdam", "cargo_manifest": map[string]any{"sawn_planks": event.quantity},
		}})
		if err := h.db.RecordEvent(h.ctx, h.account.ID, event.id, "ship_departed", body); err != nil {
			t.Fatal(err)
		}
	}
	in.Now = time.Now().UTC()
	runner.refreshSourcingHistory(h.ctx, in)
	if runner.sourcingHistory.Error != "" || runner.sourcingHistory.Outcomes.Cursor.DepartureID == 0 ||
		!recentSourceConversion(runner.sourcingHistory.Outcomes.Sources[probeSourceKey("amsterdam", "sawn_planks")], in.Now) {
		t.Fatalf("one malformed event blocked the later own conversion: %+v", runner.sourcingHistory)
	}
	const warning = "skipping undecodable own sourcing departure"
	if logs.FilterMessage(warning).Len() != 1 || logs.Len() != 1 {
		t.Fatalf("foreign cargo was decoded or the own decode failure was not logged once: %+v", logs.All())
	}
	cursor := runner.sourcingHistory.Outcomes.Cursor
	runner.refreshSourcingHistory(h.ctx, in)
	if runner.sourcingHistory.Error != "" || runner.sourcingHistory.Outcomes.Cursor != cursor || logs.FilterMessage(warning).Len() != 1 {
		t.Fatal("a skipped row was retried instead of advancing the incremental cursor")
	}
}

func TestProbeHistoryRefreshLoadsBudgetOnlyForSourcingOrContinuation(t *testing.T) {
	for _, missionKind := range []string{"laden-trade", "relocation", "handoff"} {
		t.Run(missionKind, func(t *testing.T) {
			h := newRecoveryHarness(t)
			h.api.setFleet([]map[string]any{destinationShipRow(recoveryShipBlastoise, "Blastoise", recoveryHomePort, "berthed", 5, nil, map[string]int64{recoveryGrain: 1})})
			in := probeHistoryLiveInput(t)
			in.Ship.Ship.ID, in.CompanyID = recoveryShipBlastoise, h.account.CompanyID
			row := probeHistoryRow(in, recoveryHomePort, in.Now.Add(-time.Hour))
			row.ID, row.Origin, row.Commodity = "lazy-budget-probe", recoveryFarPort, recoveryGrain
			row.Lane = recoveryHomePort + "|" + recoveryFarPort + "|" + recoveryGrain + "|probe:0:3000"
			putProbeHistorySettlement(t, h, row)
			body := encode(map[string]any{"company_id": in.CompanyID, "payload": map[string]any{
				"ship_id": recoveryShipBlastoise, "origin_port_id": recoveryHomePort, "cargo_manifest": map[string]int64{recoveryGrain: 1},
			}})
			if err := h.db.RecordEvent(h.ctx, h.account.ID, "lazy-budget-conversion", "ship_departed", body); err != nil {
				t.Fatal(err)
			}
			if missionKind != "laden-trade" {
				data := missionData{Plan: Plan{Kind: "relocation", Origin: recoveryHomePort, Destination: recoveryFarPort}}
				intent := &relocationIntent{TargetDestination: recoveryHomePort}
				if missionKind == "relocation" {
					data.Relocation = intent
				} else {
					data.SourcingHandoff = intent
				}
				h.putMission(recoveryShipBlastoise, "lazy-budget", "sailing", data)
			}
			runner := h.runner(primaryTestKey)
			if err := runner.refresh(h.ctx); err != nil {
				t.Fatal(err)
			}
			if runner.sourcingHistory.Error != "" || runner.sourcingHistory.Ready != (missionKind != "laden-trade") {
				t.Fatalf("unexpected full-budget capture for %s: %+v", missionKind, runner.sourcingHistory)
			}
			if runner.sourcingHistory.Outcomes.Cursor.DepartureID == 0 ||
				runner.sourcingHistory.Outcomes.Sources[probeSourceKey(recoveryHomePort, recoveryGrain)].ConvertedAt.IsZero() {
				t.Fatal("skipping a full budget capture also skipped incremental outcomes")
			}
		})
	}
}

func TestProbeHistoryZeroAllowanceSkipsLanesButOtherIdleHullLoadsThem(t *testing.T) {
	t.Parallel()
	h := newRecoveryHarness(t)
	idleFallbackExhaustSeed(t, h)
	in := sourcingInputFixture(t)
	row := probeHistoryRow(in, "amsterdam", in.Now.Add(-time.Hour))
	row.ID, row.Lane = "already-probed", sourcingEvidenceKey("amsterdam", in.Contracts[0], 900)
	putProbeHistorySettlement(t, h, row)
	runner := h.runner(primaryTestKey)
	probeHistoryRunner(runner, in)
	if err := runner.loadSourcingHistory(h.ctx, &in); err != nil {
		t.Fatal(err)
	}
	if sourcingAllowance(in) != 0 || len(in.SourcingProbeCompletions) != 0 {
		t.Fatal("zero allowance still loaded matching encrypted probe lanes")
	}
	idleShip := in.Ship
	idleShip.Ship.ID = "idle-ship"
	runner.stateCache.Ships = append(runner.stateCache.Ships, idleShip)
	runner.snapshot["_ship_idle"] = encode(map[string]shipIdleState{idleShip.Ship.ID: {
		PortID: idleShip.Ship.CurrentPortID, EarnedGameHours: sourcingIdleGameHours, Validated: true,
		LastValidatedGameHour: in.Clock.Calendar.TotalGameHours, LastClockUTC: in.Clock.RealUTC,
	}})
	runner.refreshSourcingHistory(h.ctx, in)
	runner.refreshSourcingBudget(h.ctx, in)
	runner.applySourcingHistory(&in)
	if in.SourcingHistoryError != "" || in.SourcingIdleEligible || in.SourcingProbeCompletions[row.Lane].IsZero() {
		t.Fatal("a nonidle first hull hid the lane history needed by another idle hull")
	}
	in.Ship = idleShip
	runner.applySourcingHistory(&in)
	if !in.SourcingIdleEligible || sourcingAllowance(in) == 0 {
		t.Fatal("the idle hull lost its distinct bounded reinspection allowance")
	}
}
