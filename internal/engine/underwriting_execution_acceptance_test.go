package engine

// Acceptance contracts for durable underwriting execution (plan E5) against
// the real Runner seams (refresh, work, dispatch, consumeResult), the fake
// game API and an isolated PostgreSQL schema. Policy evaluation
// (underwriting.go) is covered independently; these regressions pin the
// execution half:
//
//   - One "__underwriting" account-control mission is restored before the
//     orphaned-pending check and driven like every other durable mission.
//   - Its frozen intent is the missionData JSON field "underwriting" holding
//     {"policy":<InsurancePolicy>,"decision":<UnderwritingDecision>} with the
//     concrete engine type JSON names. The tests seed it as raw JSON, so the
//     implementation adds the typed field without changing the document.
//   - Stage "underwrite" posts exactly {"policy_id","slice_bps"} to
//     POST /v1/insurance/underwrite through the immutable journal.
//   - A typed 200 ({policy, underwritings}) is authoritative only when it
//     names exactly one new owned row for our company with the requested
//     policy_id, the requested 500bps slice, collateral within the admitted
//     ceiling and premium at or above the admitted floor, all numerically
//     present. Anything else holds the mission with its immutable action and
//     persists no position.
//   - A lost response, or public board membership plus covered_bps that
//     already include our slice, never mints a replacement idempotency key;
//     only the original key may replay inside the upstream window, and the
//     pending control mission must not suppress safe fleet work.
//
// Terminal claim accounting (net versus principal) is owned by the store
// closure contract and its acceptance tests; these tests only pin that a
// fresh execution persists an active position with unknown, never zeroed,
// closure figures.

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/DevYukine/go-tradewinds/internal/store"
)

const (
	uwExecPolicyID       = "01a16b00-7000-7000-8000-000000000501"
	uwExecSecondPolicyID = "01a16b00-7000-7000-8000-000000000602"
	uwExecOwnedRowID     = "01a16b00-7000-7000-8000-000000000b02"
	uwExecDuplicateRowID = "01a16b00-7000-7000-8000-000000000b03"
	uwExecSecondRowID    = "01a16b00-7000-7000-8000-000000000b04"
	uwExecOtherCompanyID = "01a16469-7564-711d-b8da-e69d7d73e999"
	// Foreign identities: the insured shipowner and vessel are never ours,
	// so self-underwriting cannot slip through the fixture.
	uwExecInsuredCompanyID = "01a16469-7564-711d-b8da-e69d7d73e623"
	uwExecForeignShipID    = "01a169e2-69a5-70f9-9cf0-921c074b2e00"

	uwExecSliceBPS          = 500
	uwExecCollateralCeiling = 50_000 // ceil(1_000_000 * 500 / 10_000)
	uwExecPremiumFloor      = 15_000 // floor(300_000 * 500 / 10_000)

	uwExecMissionID = "__underwriting"
	uwExecStage     = "underwrite"
	uwExecPath      = "/v1/insurance/underwrite"
)

// uwExecIntent is the frozen missionData "underwriting" document: the board
// observation the decision was made on plus the conservative admitted money
// bounds the response is validated against.
type uwExecIntent struct {
	Policy   InsurancePolicy      `json:"policy"`
	Decision UnderwritingDecision `json:"decision"`
}

// uwExecPolicy builds the admissible candidate from one per-test base time:
// every fixture that must re-observe identical immutable terms (frozen
// intent, public board row, typed 200) derives from the same base, so
// sameInsuranceTerms holds across every observation. The terms are real
// possible guide terms: premium_bps inside the documented 10..3000 band and
// total premium equal to the bps arithmetic.
func uwExecPolicy(base time.Time) InsurancePolicy {
	return InsurancePolicy{
		ID:                uwExecPolicyID,
		InsuredCompanyID:  uwExecInsuredCompanyID,
		ShipID:            uwExecForeignShipID,
		OriginPortID:      recoveryHomePort,
		DestPortID:        recoveryFarPort,
		InsuredValuePence: 1_000_000,
		HullValuePence:    1_000_000,
		PremiumBPS:        3000,
		TotalPremiumPence: 300_000,
		CoveredBPS:        uwExecSliceBPS,
		Status:            "active",
		CreatedAt:         base.Add(-time.Hour),
		UpdatedAt:         base.Add(-30 * time.Minute),
		ExpiresAt:         base.Add(6 * time.Hour),
	}
}

// uwExecDecision freezes the admitted bounds and a matched cohort summary
// consistent with uwExecCohort: 17 closed hull-only same-direction samples
// with one loss. The 1052bps estimate is the Beta(1,1) posterior mean
// (1+1)/(17+2); 25012bps is the fixed-look nominal sensitivity.
func uwExecDecision() UnderwritingDecision {
	return UnderwritingDecision{
		Accepted:              true,
		Reason:                "synthetic evidence-positive hull-only pilot",
		PolicyID:              uwExecPolicyID,
		SliceBPS:              uwExecSliceBPS,
		CollateralPence:       uwExecCollateralCeiling,
		PremiumPence:          uwExecPremiumFloor,
		ExpectedClaimsPence:   5_260,
		ExpectedNetPence:      9_741,
		ObservedPolicies:      17,
		ObservedLosses:        1,
		EstimatedLossBPS:      1052,
		ConservativeLossBPS:   25_012,
		EstimatedLockGameDays: 1.5,
	}
}

// uwExecSecondPolicy is a second, equally admissible candidate with a
// different policy ID: the tie-break (equal modeled net, lower ID wins)
// deterministically picks A first, so B is the "fresh different profitable
// policy" for the funding-gate regression.
func uwExecSecondPolicy(base time.Time) InsurancePolicy {
	policy := uwExecPolicy(base)
	policy.ID = uwExecSecondPolicyID
	return policy
}

// uwExecCohort serves the closed same-direction hull-only history the frozen
// decision summarizes: 16 settled_safe and 1 claimed_loss on the candidate's
// lane, all expiring in the past and none terms-changed.
func uwExecCohort(base time.Time) []map[string]any {
	rows := make([]map[string]any, 0, 17)
	for i := range 17 {
		created := base.Add(-72*time.Hour - time.Duration(i)*2*time.Hour)
		status := "settled_safe"
		if i == 0 {
			status = "claimed_loss"
		}
		row := uwExecPolicyRow(InsurancePolicy{
			ID:                fmt.Sprintf("01a16b00-7000-7000-8000-00000000c%03d", i),
			InsuredCompanyID:  uwExecInsuredCompanyID,
			ShipID:            uwExecForeignShipID,
			OriginPortID:      recoveryHomePort,
			DestPortID:        recoveryFarPort,
			InsuredValuePence: 1_000_000,
			HullValuePence:    1_000_000,
			PremiumBPS:        3000,
			TotalPremiumPence: 300_000,
			CoveredBPS:        10_000,
			Status:            status,
			CreatedAt:         created,
			UpdatedAt:         created.Add(3 * time.Hour),
			ExpiresAt:         created.Add(6 * time.Hour),
		})
		rows = append(rows, row)
	}
	return rows
}

// uwExecBoardRows serves the public board: the open candidate plus its
// closed cohort, all freshly observed within the current run.
func uwExecBoardRows(base time.Time) []map[string]any {
	return append([]map[string]any{uwExecPolicyRow(uwExecPolicy(base))}, uwExecCohort(base)...)
}

func uwExecPolicyRow(policy InsurancePolicy) map[string]any {
	payload, err := json.Marshal(policy)
	if err != nil {
		panic(err)
	}
	row := map[string]any{}
	if err := json.Unmarshal(payload, &row); err != nil {
		panic(err)
	}
	return row
}

// uwExecRow builds one InsuranceUnderwriting row with the documented OpenAPI
// member names. Absent monetary fields are modeled by deleting the key.
func uwExecRow(company string, slice, collateral, premium int64) map[string]any {
	now := time.Now().UTC().Format(time.RFC3339Nano)
	return map[string]any{
		"id":                      uwExecOwnedRowID,
		"policy_id":               uwExecPolicyID,
		"underwriter_company_id":  company,
		"slice_bps":               slice,
		"collateral_locked_pence": collateral,
		"premium_share_pence":     premium,
		"created_at":              now,
		"updated_at":              now,
	}
}

func uwExecEnvelope(policy InsurancePolicy, rows []map[string]any) json.RawMessage {
	payload, err := json.Marshal(struct {
		Policy        InsurancePolicy  `json:"policy"`
		Underwritings []map[string]any `json:"underwritings"`
	}{Policy: policy, Underwritings: rows})
	if err != nil {
		panic(err)
	}
	return payload
}

// uwExecPutMission persists the control mission with the frozen intent as
// raw JSON, optionally carrying an attached durable action. The intent key
// survives because the document is never round-tripped through missionData.
func uwExecPutMission(t *testing.T, h *recoveryHarness, base time.Time, action *durableAction) {
	t.Helper()
	intent, err := json.Marshal(uwExecIntent{Policy: uwExecPolicy(base), Decision: uwExecDecision()})
	if err != nil {
		t.Fatalf("encode frozen underwriting intent: %v", err)
	}
	document := map[string]any{
		"plan":                 map[string]any{"kind": "underwriting", "ship_id": uwExecMissionID},
		"started_at":           base.Format(time.RFC3339Nano),
		"sequence":             0,
		"initial_ledger_ids":   map[string]bool{},
		"eligible":             map[string]int64{},
		"rejected_charter_ids": map[string]bool{},
		"costs_known":          true,
		"cooldown_until":       "0001-01-01T00:00:00Z",
		"proven_lane":          "",
		"underwriting":         json.RawMessage(intent),
	}
	if action != nil {
		encoded, err := json.Marshal(action)
		if err != nil {
			t.Fatalf("encode durable underwriting action: %v", err)
		}
		document["action"] = json.RawMessage(encoded)
	}
	payload, err := json.Marshal(document)
	if err != nil {
		t.Fatalf("encode underwriting mission: %v", err)
	}
	if err := h.db.PutMission(h.ctx, &store.Mission{
		AccountID:  h.account.ID,
		ShipID:     uwExecMissionID,
		Phase:      uwExecStage,
		Generation: "underwriting-generation",
		Data:       payload,
	}); err != nil {
		t.Fatalf("persist underwriting mission: %v", err)
	}
}

// uwExecSeedPendingUnderwrite persists the mission with its attached action
// first, then journals the action body exactly as the durable mission
// restored it: JSONB canonicalizes the embedded body, and production
// dispatch journals the restored request, so the pending row must carry
// those same authoritative bytes. Returns that body for the immutable
// byte-equality assertions, mirroring a crash after the journal write.
func uwExecSeedPendingUnderwrite(t *testing.T, h *recoveryHarness, base time.Time) (string, []byte) {
	t.Helper()
	body, err := json.Marshal(map[string]any{"policy_id": uwExecPolicyID, "slice_bps": uwExecSliceBPS})
	if err != nil {
		t.Fatalf("encode underwrite request: %v", err)
	}
	id := ActionID(h.account.ID, "underwriting-generation", uwExecStage, 0)
	uwExecPutMission(t, h, base, &durableAction{
		ID:        id,
		Stage:     uwExecStage,
		Method:    http.MethodPost,
		Path:      uwExecPath,
		Body:      body,
		CreatedAt: base,
	})
	_, data := h.mission(uwExecMissionID)
	if data.Action == nil || data.Action.ID != id {
		t.Fatalf("seeded mission lost its attached action: %+v", data.Action)
	}
	if _, err := h.db.BeginAction(h.ctx, &store.Action{
		ID:        data.Action.ID,
		AccountID: h.account.ID,
		Method:    data.Action.Method,
		Path:      data.Action.Path,
		Body:      data.Action.Body,
	}); err != nil {
		t.Fatalf("seed pending underwrite action: %v", err)
	}
	return id, []byte(data.Action.Body)
}

// uwExecExpireCooldown clears the persisted retry deadline while preserving
// every unknown missionData key, including the frozen "underwriting" intent.
func uwExecExpireCooldown(t *testing.T, h *recoveryHarness) {
	t.Helper()
	mission, err := h.db.GetMission(h.ctx, h.account.ID, uwExecMissionID)
	if err != nil {
		t.Fatalf("load underwriting mission: %v", err)
	}
	if mission == nil {
		t.Fatal("underwriting mission vanished before its outcome was resolved")
	}
	var document map[string]json.RawMessage
	if err := json.Unmarshal(mission.Data, &document); err != nil {
		t.Fatalf("decode underwriting mission: %v", err)
	}
	document["cooldown_until"] = json.RawMessage(`"0001-01-01T00:00:00Z"`)
	payload, err := json.Marshal(document)
	if err != nil {
		t.Fatalf("re-encode underwriting mission: %v", err)
	}
	mission.Data = payload
	if err := h.db.PutMission(h.ctx, mission); err != nil {
		t.Fatalf("expire underwriting cooldown: %v", err)
	}
}

// uwExecProxy decorates the fake game API: it scripts the underwrite
// mutation, serves the public insurance board with its closed cohort and the
// player's observed company list, can serve active contract rows, and
// implements the deliver-freight success the stock fake leaves unsupported.
type uwExecProxy struct {
	mu             sync.Mutex
	downstream     http.Handler
	recordMutation func(request *http.Request, body []byte, status int)
	underwrite     func(writer http.ResponseWriter, request *http.Request, body []byte)
	insuranceRows  []map[string]any
	contractRows   []map[string]any
	ledgerRows     []map[string]any
	membership     map[string]bool // policy IDs the board names us as underwriter on
}

func installUWExecProxy(t *testing.T, h *recoveryHarness) *uwExecProxy {
	t.Helper()
	proxy := &uwExecProxy{downstream: h.api.server.Config.Handler}
	proxy.recordMutation = func(request *http.Request, body []byte, status int) {
		h.api.mu.Lock()
		h.api.record(request, body, status)
		h.api.mu.Unlock()
	}
	h.api.server.Config.Handler = proxy
	return proxy
}

func (p *uwExecProxy) ServeHTTP(writer http.ResponseWriter, request *http.Request) {
	switch {
	case request.Method == http.MethodPost && request.URL.Path == uwExecPath:
		body, err := io.ReadAll(request.Body)
		if err != nil {
			uwExecWriteProblem(writer, http.StatusBadRequest, "unreadable_request", err.Error())
			return
		}
		p.mu.Lock()
		underwrite := p.underwrite
		p.mu.Unlock()
		if underwrite != nil {
			underwrite(writer, request, body)
			return
		}
		p.downstream.ServeHTTP(writer, request)
	case request.Method == http.MethodGet && request.URL.Path == "/v1/insurance/list":
		p.mu.Lock()
		rows := p.insuranceRows
		membership := p.membership
		p.mu.Unlock()
		if rows == nil {
			p.downstream.ServeHTTP(writer, request)
			return
		}
		uwExecWriteJSON(writer, http.StatusOK, uwExecInsuranceView(request, rows, membership))
	case request.Method == http.MethodGet && request.URL.Path == "/v1/treasury/ledger":
		p.mu.Lock()
		rows := p.ledgerRows
		p.mu.Unlock()
		if rows == nil {
			p.downstream.ServeHTTP(writer, request)
			return
		}
		uwExecWriteJSON(writer, http.StatusOK, rows)
	case request.Method == http.MethodGet && request.URL.Path == "/v1/player/profile":
		uwExecWriteJSON(writer, http.StatusOK, map[string]any{
			"player": map[string]any{"id": recoveryPlayerID},
			"companies": []any{map[string]any{
				"id":             recoveryCompanyID,
				"player_id":      recoveryPlayerID,
				"name":           "The Pokemon Pirates",
				"home_port_id":   recoveryHomePort,
				"crown_prestige": 0,
				"created_at":     time.Now().UTC().Add(-720 * time.Hour).Format(time.RFC3339Nano),
				"updated_at":     time.Now().UTC().Format(time.RFC3339Nano),
			}},
		})
	case request.Method == http.MethodGet && request.URL.Path == "/v1/contract/list":
		p.mu.Lock()
		rows := p.contractRows
		p.mu.Unlock()
		if rows == nil {
			p.downstream.ServeHTTP(writer, request)
			return
		}
		status := request.URL.Query().Get("status")
		filtered := make([]map[string]any, 0, len(rows))
		for _, row := range rows {
			if row["status"] == status {
				filtered = append(filtered, row)
			}
		}
		uwExecWriteJSON(writer, http.StatusOK, filtered)
	case request.Method == http.MethodPost && request.URL.Path == "/v1/contract/deliver-freight":
		body, err := io.ReadAll(request.Body)
		if err != nil {
			uwExecWriteProblem(writer, http.StatusBadRequest, "unreadable_request", err.Error())
			return
		}
		var requested struct {
			ContractID string `json:"contract_id"`
		}
		_ = json.Unmarshal(body, &requested)
		p.recordMutation(request, body, http.StatusOK)
		uwExecWriteJSON(writer, http.StatusOK, deliverFreightSuccess(requested.ContractID))
	default:
		p.downstream.ServeHTTP(writer, request)
	}
}

// uwExecInsuranceView resolves one board query. A plain listing shows the
// candidates plus their cohort; the underwriter_company_id filter shows only
// policies the test has actually marked as owned; our company never appears
// as the insured shipowner.
func uwExecInsuranceView(request *http.Request, rows []map[string]any, membership map[string]bool) []map[string]any {
	query := request.URL.Query()
	if query.Get("insured_company_id") != "" {
		return []map[string]any{}
	}
	if underwriter := query.Get("underwriter_company_id"); underwriter != "" {
		if underwriter != recoveryCompanyID {
			return []map[string]any{}
		}
		owned := make([]map[string]any, 0, len(membership))
		for _, row := range rows {
			if membership[row["id"].(string)] {
				owned = append(owned, row)
			}
		}
		return owned
	}
	return rows
}

// uwExecServeLost accepts the connection and destroys it without a response,
// the lost-receipt shape the journal's unresolved disposition exists for.
func uwExecServeLost(record func(request *http.Request, body []byte, status int)) func(http.ResponseWriter, *http.Request, []byte) {
	return func(writer http.ResponseWriter, request *http.Request, body []byte) {
		record(request, body, 0)
		connection, _, err := writer.(http.Hijacker).Hijack()
		if err != nil {
			uwExecWriteProblem(writer, http.StatusInternalServerError, "hijack_failed", err.Error())
			return
		}
		_ = connection.Close()
	}
}

func uwExecWriteJSON(writer http.ResponseWriter, status int, document any) {
	payload, err := json.Marshal(document)
	if err != nil {
		http.Error(writer, err.Error(), http.StatusInternalServerError)
		return
	}
	writer.Header().Set("Content-Type", "application/json")
	writer.WriteHeader(status)
	_, _ = writer.Write(payload)
}

func uwExecWriteProblem(writer http.ResponseWriter, status int, code, detail string) {
	uwExecWriteJSON(writer, status, map[string]any{
		"type":   "about:blank",
		"title":  http.StatusText(status),
		"status": status,
		"code":   code,
		"detail": detail,
	})
}

func uwExecTicks(t *testing.T, h *recoveryHarness, runner *Runner, rounds int) {
	t.Helper()
	for range rounds {
		if _, err := h.tick(runner); err != nil {
			t.Fatalf("engine tick failed: %v", err)
		}
	}
}

func uwExecAttempts(t *testing.T, h *recoveryHarness) []recordedMutation {
	t.Helper()
	return h.api.mutationsFor(uwExecPath)
}

// uwExecRequireSingleKey pins the replacement-key ban: every wire attempt
// for the underwrite stage carries our credential and one immutable
// idempotency key with the requested policy and minimum slice.
func uwExecRequireSingleKey(t *testing.T, h *recoveryHarness, id string, body []byte, context string) {
	t.Helper()
	attempts := uwExecAttempts(t, h)
	keys := map[string]bool{}
	for _, attempt := range attempts {
		if attempt.IdempotencyKey == "" {
			t.Fatalf("%s: underwrite attempt carried no idempotency key: %+v", context, attempt)
		}
		if attempt.IdempotencyKey != id {
			t.Fatalf("%s: underwrite attempt used replacement key %s instead of the immutable %s", context, attempt.IdempotencyKey, id)
		}
		if attempt.Authorization != "Bearer "+primaryTestKey {
			t.Fatalf("%s: underwrite attempt used unexpected credentials %q", context, attempt.Authorization)
		}
		var request struct {
			PolicyID string `json:"policy_id"`
			SliceBPS int64  `json:"slice_bps"`
		}
		if err := json.Unmarshal(attempt.Body, &request); err != nil ||
			request.PolicyID != uwExecPolicyID || request.SliceBPS != uwExecSliceBPS {
			t.Fatalf("%s: underwrite attempt body disagrees with the immutable minimum slice: %s", context, attempt.Body)
		}
		if !bytes.Equal(attempt.Body, body) {
			t.Fatalf("%s: underwrite attempt body drifted from the journal request: %s then %s", context, body, attempt.Body)
		}
		keys[attempt.IdempotencyKey] = true
	}
	if len(keys) > 1 {
		t.Fatalf("%s: %d distinct underwriting idempotency keys were minted: %+v", context, len(keys), attempts)
	}
}

func uwExecDecisions(t *testing.T, h *recoveryHarness) []store.Decision {
	t.Helper()
	rows, err := h.db.History(h.ctx, h.account.ID, "decisions", 300)
	if err != nil {
		t.Fatalf("load decisions: %v", err)
	}
	decisions, ok := rows.([]store.Decision)
	if !ok {
		t.Fatalf("unexpected decision history payload %T", rows)
	}
	return decisions
}

func uwExecRequireNoOrphanDecision(t *testing.T, h *recoveryHarness) {
	t.Helper()
	for _, decision := range uwExecDecisions(t, h) {
		if strings.Contains(decision.Reason, "orphaned unresolved journal request") {
			t.Fatalf("pending __underwriting was treated as an orphaned request instead of a control mission: %q", decision.Reason)
		}
	}
}

func uwExecRequireHold(t *testing.T, h *recoveryHarness, context string) {
	t.Helper()
	for _, decision := range uwExecDecisions(t, h) {
		if decision.ShipID == uwExecMissionID && decision.Kind == "hold" {
			return
		}
	}
	t.Fatalf("%s: no durable hold was recorded for __underwriting across %d decisions", context, len(uwExecDecisions(t, h)))
}

func uwExecRequireNoPendingUnderwrite(t *testing.T, h *recoveryHarness) {
	t.Helper()
	for _, action := range h.pending() {
		if action.Path == uwExecPath {
			t.Fatalf("underwrite request is still unresolved in the journal: %+v", action)
		}
	}
}

func uwExecRequireReleasedMission(t *testing.T, h *recoveryHarness, context string) {
	t.Helper()
	mission, err := h.db.GetMission(h.ctx, h.account.ID, uwExecMissionID)
	if err != nil {
		t.Fatalf("load underwriting mission: %v", err)
	}
	if mission != nil {
		t.Fatalf("%s: __underwriting was not released after its durable success: phase %s data %s", context, mission.Phase, mission.Data)
	}
}

func uwExecRequireRetainedAction(t *testing.T, h *recoveryHarness, id string, body []byte, context string) {
	t.Helper()
	mission, data := h.mission(uwExecMissionID)
	if data.Action == nil {
		t.Fatalf("%s: held underwriting response cleared the immutable action: phase %s", context, mission.Phase)
	}
	if data.Action.ID != id || data.Action.Stage != uwExecStage || data.Action.Path != uwExecPath || !bytes.Equal(data.Action.Body, body) {
		t.Fatalf("%s: held underwriting action mutated: %+v", context, data.Action)
	}
}

// uwExecRequirePosition pins the exact durable position of one accepted
// slice: anchored to its journal action and frozen row values, active, and
// with unknown — never zeroed — closure figures.
func uwExecRequirePosition(t *testing.T, h *recoveryHarness, actionID string, collateral, premium int64, context string) store.UnderwritingPosition {
	t.Helper()
	positions, err := h.db.UnderwritingPositions(h.ctx, h.account.ID)
	if err != nil {
		t.Fatalf("load underwriting positions: %v", err)
	}
	if len(positions) != 1 {
		t.Fatalf("%s: expected exactly one durable underwriting position, found %d", context, len(positions))
	}
	position := positions[0]
	if position.ActionID != actionID || position.PolicyID != uwExecPolicyID ||
		position.CompanyID != recoveryCompanyID || position.SliceBPS != uwExecSliceBPS ||
		position.CollateralPence != collateral || position.PremiumPence != premium {
		t.Fatalf("%s: durable position disagrees with the accepted row: %+v", context, position)
	}
	if position.Status != "active" || position.EarnedPremiumPence != nil || position.ClaimsPence != nil ||
		position.ReturnedCollateralPence != nil || position.FeesPence != nil || position.NetPence != nil ||
		position.ClosedAt != nil {
		t.Fatalf("%s: execution fabricated terminal accounting on a live position: %+v", context, position)
	}
	return position
}

func uwExecRequireNoPositions(t *testing.T, h *recoveryHarness, context string) {
	t.Helper()
	positions, err := h.db.UnderwritingPositions(h.ctx, h.account.ID)
	if err != nil {
		t.Fatalf("load underwriting positions: %v", err)
	}
	if len(positions) != 0 {
		t.Fatalf("%s: invalid or unresolved outcome persisted %d underwriting positions: %+v", context, len(positions), positions)
	}
}

// A frozen evidence-positive intent must execute exactly one minimum slice
// through the journal: one request with the immutable key and body, the
// typed 200 accepted because it names exactly one owned row within the
// admitted money bounds, an exact durable position, the control mission
// released, and no replay ever sending another request once membership
// confirms the owned slice.
func TestUnderwritingExecutionStagesExactlyOneMinimumSlice(t *testing.T) {
	t.Parallel()
	h := newRecoveryHarness(t)
	base := time.Now().UTC()
	proxy := installUWExecProxy(t, h)
	proxy.insuranceRows = uwExecBoardRows(base)
	proxy.membership = map[string]bool{}
	proxy.underwrite = func(writer http.ResponseWriter, request *http.Request, body []byte) {
		var requested struct {
			PolicyID string `json:"policy_id"`
			SliceBPS int64  `json:"slice_bps"`
		}
		if err := json.Unmarshal(body, &requested); err != nil || requested.PolicyID != uwExecPolicyID || requested.SliceBPS != uwExecSliceBPS {
			proxy.recordMutation(request, body, http.StatusBadRequest)
			uwExecWriteProblem(writer, http.StatusBadRequest, "invalid_underwrite_request", "the request must name the frozen policy with the minimum slice")
			return
		}
		proxy.recordMutation(request, body, http.StatusOK)
		owned := uwExecRow(recoveryCompanyID, uwExecSliceBPS, uwExecCollateralCeiling, uwExecPremiumFloor+1)
		uwExecWriteJSON(writer, http.StatusOK, uwExecEnvelope(uwExecPolicy(base), []map[string]any{owned}))
		proxy.mu.Lock()
		proxy.membership[uwExecPolicyID] = true
		proxy.mu.Unlock()
	}
	uwExecPutMission(t, h, base, nil)
	runner := h.runner(primaryTestKey)
	uwExecTicks(t, h, runner, 6)

	attemptList := uwExecAttempts(t, h)
	if len(attemptList) != 1 {
		t.Fatalf("frozen __underwriting intent produced %d underwrite requests, want exactly one executed minimum slice: %+v", len(attemptList), attemptList)
	}
	id := attemptList[0].IdempotencyKey
	stored, err := h.db.BeginAction(h.ctx, &store.Action{
		ID:        id,
		AccountID: h.account.ID,
		Method:    http.MethodPost,
		Path:      uwExecPath,
		Body:      attemptList[0].Body,
	})
	if err != nil {
		t.Fatalf("read back executed underwrite action: %v", err)
	}
	if stored.State != store.ActionComplete || stored.Status != http.StatusOK {
		t.Fatalf("accepted underwrite response was not durably consumed: state %s status %d", stored.State, stored.Status)
	}
	uwExecRequireNoPendingUnderwrite(t, h)
	uwExecRequirePosition(t, h, id, uwExecCollateralCeiling, uwExecPremiumFloor+1, "accepted typed 200")
	uwExecRequireReleasedMission(t, h, "accepted typed 200")
	uwExecRequireNoOrphanDecision(t, h)

	// The position outlives the runner: a restart with the policy still on
	// the board and membership visible must stay quiet and keep the single
	// position idempotent.
	restarted := h.runner(primaryTestKey)
	uwExecTicks(t, h, restarted, 3)
	if after := uwExecAttempts(t, h); len(after) != 1 {
		t.Fatalf("durable underwriting position produced %d further requests after restart: %+v", len(after)-1, after)
	}
	uwExecRequirePosition(t, h, id, uwExecCollateralCeiling, uwExecPremiumFloor+1, "restart after accepted position")
	uwExecRequireReleasedMission(t, h, "restart after accepted position")
	uwExecRequireNoOrphanDecision(t, h)
}

// A crash between storing the accepted 200 and consuming it must consume the
// stored response on replay: the position is accounted, the mission is
// released, and the stored outcome is never re-sent over the wire.
func TestUnderwritingExecutionConsumesStoredSuccessWithoutResend(t *testing.T) {
	t.Parallel()
	h := newRecoveryHarness(t)
	base := time.Now().UTC()
	id, _ := uwExecSeedPendingUnderwrite(t, h, base)
	owned := uwExecRow(recoveryCompanyID, uwExecSliceBPS, uwExecCollateralCeiling, uwExecPremiumFloor+1)
	if err := h.db.CompleteAction(h.ctx, id, http.StatusOK, uwExecEnvelope(uwExecPolicy(base), []map[string]any{owned}), nil); err != nil {
		t.Fatalf("store accepted underwrite response: %v", err)
	}
	runner := h.runner(primaryTestKey)
	uwExecTicks(t, h, runner, 3)

	if sent := h.api.mutationCount(uwExecPath); sent != 0 {
		t.Fatalf("stored underwriting success was re-sent over the wire %d times: %+v", sent, h.api.mutationsFor(uwExecPath))
	}
	uwExecRequireNoPendingUnderwrite(t, h)
	uwExecRequirePosition(t, h, id, uwExecCollateralCeiling, uwExecPremiumFloor+1, "replayed stored success")
	uwExecRequireReleasedMission(t, h, "replayed stored success")
	uwExecRequireNoOrphanDecision(t, h)
}

// Every way a typed 200 can fail to name exactly one admissible owned slice
// must hold the control mission on its immutable action: no advancement, no
// second request, no replacement key, no persisted position, and a durable
// hold decision.
func TestUnderwritingExecutionHoldsInvalidTypedSuccessResponses(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name string
		body func(base time.Time) []byte
	}{
		{"malformed committed body", func(base time.Time) []byte { return []byte("<html>gateway timeout</html>") }},
		{"policy identity mismatch", func(base time.Time) []byte {
			policy := uwExecPolicy(base)
			policy.ID = uwExecOtherCompanyID
			return uwExecEnvelope(policy, []map[string]any{uwExecRow(recoveryCompanyID, uwExecSliceBPS, uwExecCollateralCeiling, uwExecPremiumFloor+1)})
		}},
		{"drifted immutable terms", func(base time.Time) []byte {
			policy := uwExecPolicy(base)
			policy.TotalPremiumPence += 1
			return uwExecEnvelope(policy, []map[string]any{uwExecRow(recoveryCompanyID, uwExecSliceBPS, uwExecCollateralCeiling, uwExecPremiumFloor+1)})
		}},
		{"foreign underwriter company", func(base time.Time) []byte {
			return uwExecEnvelope(uwExecPolicy(base), []map[string]any{uwExecRow(uwExecOtherCompanyID, uwExecSliceBPS, uwExecCollateralCeiling, uwExecPremiumFloor+1)})
		}},
		{"duplicate owned rows", func(base time.Time) []byte {
			first := uwExecRow(recoveryCompanyID, uwExecSliceBPS, uwExecCollateralCeiling, uwExecPremiumFloor+1)
			second := uwExecRow(recoveryCompanyID, uwExecSliceBPS, uwExecCollateralCeiling, uwExecPremiumFloor+1)
			second["id"] = uwExecDuplicateRowID
			return uwExecEnvelope(uwExecPolicy(base), []map[string]any{first, second})
		}},
		{"absent collateral", func(base time.Time) []byte {
			row := uwExecRow(recoveryCompanyID, uwExecSliceBPS, uwExecCollateralCeiling, uwExecPremiumFloor+1)
			delete(row, "collateral_locked_pence")
			return uwExecEnvelope(uwExecPolicy(base), []map[string]any{row})
		}},
		{"absent premium", func(base time.Time) []byte {
			row := uwExecRow(recoveryCompanyID, uwExecSliceBPS, uwExecCollateralCeiling, uwExecPremiumFloor+1)
			delete(row, "premium_share_pence")
			return uwExecEnvelope(uwExecPolicy(base), []map[string]any{row})
		}},
		{"collateral above admitted ceiling", func(base time.Time) []byte {
			return uwExecEnvelope(uwExecPolicy(base), []map[string]any{uwExecRow(recoveryCompanyID, uwExecSliceBPS, uwExecCollateralCeiling+1, uwExecPremiumFloor+1)})
		}},
		{"premium below admitted floor", func(base time.Time) []byte {
			return uwExecEnvelope(uwExecPolicy(base), []map[string]any{uwExecRow(recoveryCompanyID, uwExecSliceBPS, uwExecCollateralCeiling, uwExecPremiumFloor-1)})
		}},
		{"slice differs from request", func(base time.Time) []byte {
			return uwExecEnvelope(uwExecPolicy(base), []map[string]any{uwExecRow(recoveryCompanyID, 1000, uwExecCollateralCeiling, uwExecPremiumFloor+1)})
		}},
		{"no owned underwriting row", func(base time.Time) []byte {
			return uwExecEnvelope(uwExecPolicy(base), []map[string]any{})
		}},
	}
	for _, test := range cases {
		t.Run(test.name, func(t *testing.T) {
			h := newRecoveryHarness(t)
			base := time.Now().UTC()
			proxy := installUWExecProxy(t, h)
			envelope := test.body(base)
			proxy.underwrite = func(writer http.ResponseWriter, request *http.Request, body []byte) {
				proxy.recordMutation(request, body, http.StatusOK)
				writer.Header().Set("Content-Type", "application/json")
				writer.WriteHeader(http.StatusOK)
				_, _ = writer.Write(envelope)
			}
			id, body := uwExecSeedPendingUnderwrite(t, h, base)
			runner := h.runner(primaryTestKey)
			uwExecTicks(t, h, runner, 3)

			if sent := len(uwExecAttempts(t, h)); sent != 1 {
				t.Fatalf("invalid underwriting response produced %d requests, want the single immutable attempt: %+v", sent, uwExecAttempts(t, h))
			}
			uwExecRequireSingleKey(t, h, id, body, "invalid typed 200")
			uwExecRequireRetainedAction(t, h, id, body, test.name)
			uwExecRequireNoPositions(t, h, test.name)
			uwExecRequireHold(t, h, test.name)
		})
	}
}

// A lost response leaves the exposure unresolved: only the original key may
// replay across restarts and cooldowns, and public board membership or a
// covered_bps figure that already includes our slice never authorizes a
// replacement request, a persisted position, or a cleared mission.
func TestUnderwritingExecutionLostResponseNeverMintsReplacementKey(t *testing.T) {
	t.Parallel()
	h := newRecoveryHarness(t)
	base := time.Now().UTC()
	proxy := installUWExecProxy(t, h)
	proxy.insuranceRows = uwExecBoardRows(base)
	proxy.membership = map[string]bool{uwExecPolicyID: true}
	proxy.underwrite = uwExecServeLost(proxy.recordMutation)
	id, body := uwExecSeedPendingUnderwrite(t, h, base)
	runner := h.runner(primaryTestKey)
	uwExecTicks(t, h, runner, 2)
	uwExecExpireCooldown(t, h)
	uwExecTicks(t, h, runner, 1)

	before := len(uwExecAttempts(t, h))
	if before < 1 {
		t.Fatalf("lost underwrite response produced no wire attempts: %+v", uwExecAttempts(t, h))
	}
	restarted := h.runner(primaryTestKey)
	uwExecExpireCooldown(t, h)
	uwExecTicks(t, h, restarted, 1)
	after := uwExecAttempts(t, h)
	if len(after) <= before {
		t.Fatalf("restarted runner did not replay the unresolved underwriting request across restart: %d attempts before, %d after", before, len(after))
	}
	uwExecRequireSingleKey(t, h, id, body, "lost response with public membership")

	_, data := h.mission(uwExecMissionID)
	if data.Action == nil || data.Action.ID != id {
		t.Fatalf("unresolved underwriting exposure lost its immutable action: %+v", data.Action)
	}
	stored, err := h.db.BeginAction(h.ctx, &store.Action{
		ID:        id,
		AccountID: h.account.ID,
		Method:    http.MethodPost,
		Path:      uwExecPath,
		Body:      body,
	})
	if err != nil {
		t.Fatalf("read back unresolved underwrite action: %v", err)
	}
	if stored.State != store.ActionPending {
		t.Fatalf("lost response resolved the journal row without a receipt: state %s status %d", stored.State, stored.Status)
	}
	uwExecRequireNoPositions(t, h, "lost response with public membership")
	uwExecRequireNoOrphanDecision(t, h)
}

// A pending __underwriting request is a restored control mission, not an
// orphan: the exposure stays unresolved while a different ship's fully
// evidenced cash-credit-only freight delivery proceeds untouched.
func TestUnderwritingExecutionPendingMissionIsControlAndFleetWorkContinues(t *testing.T) {
	t.Parallel()
	h := newRecoveryHarness(t)
	base := time.Now().UTC()
	proxy := installUWExecProxy(t, h)
	proxy.underwrite = uwExecServeLost(proxy.recordMutation)
	proxy.contractRows = []map[string]any{freightContractRow("freight-a", "active", false)}
	h.api.setFleet([]map[string]any{
		stationaryShipRow(recoveryShipBlastoise, "Blastoise", recoveryFarPort, "berthed", 24, 3),
	})
	id, body := uwExecSeedPendingUnderwrite(t, h, base)
	deadline := base.Add(6 * time.Hour)
	h.putMission(recoveryShipBlastoise, "freight-generation", "arrival", missionData{
		Plan:     Plan{Kind: "recovery", ShipID: recoveryShipBlastoise, Destination: recoveryFarPort},
		Freight:  []freightObligation{{ContractID: "freight-a", Commodity: recoveryGrain, Quantity: 24, Destination: recoveryFarPort, Deadline: &deadline, RewardPence: 9_000, CollateralPence: 5_000}},
		Eligible: map[string]int64{},
	})
	runner := h.runner(primaryTestKey)
	uwExecTicks(t, h, runner, 1)
	uwExecExpireCooldown(t, h)
	uwExecTicks(t, h, runner, 2)

	if deliveries := h.api.mutationCount("/v1/contract/deliver-freight"); deliveries != 1 {
		t.Fatalf("unresolved underwriting suppressed safe fleet work: %d freight deliveries (%+v)", deliveries, h.api.mutationsFor("/v1/contract/deliver-freight"))
	}
	_, shipData := h.mission(recoveryShipBlastoise)
	if len(shipData.Freight) != 1 || !shipData.Freight[0].Settled || shipData.ProceedsPence != 9_000 {
		t.Fatalf("freight delivery did not settle exactly once beside the underwriting wait: freight %+v proceeds %d", shipData.Freight, shipData.ProceedsPence)
	}
	uwExecRequireSingleKey(t, h, id, body, "unresolved underwriting beside fleet work")
	uwExecRequireRetainedAction(t, h, id, body, "unresolved underwriting beside fleet work")
	uwExecRequireNoPositions(t, h, "unresolved underwriting beside fleet work")
	uwExecRequireNoOrphanDecision(t, h)
}

// c0orc11: after one accepted owned receipt, a fresh different otherwise
// profitable policy must not create another underwriting while that
// position's exact funding evidence is absent. The gate is fail-closed, not
// a permanent disable: once recordUnderwriting finds the unique
// identity-linked own-vault collateral debit in a fresh ledger, further
// decisions are permitted within the pilot budget.
func TestUnderwritingExecutionFundingEvidenceGatesNextSlice(t *testing.T) {
	t.Parallel()
	h := newRecoveryHarness(t)
	base := time.Now().UTC()
	proxy := installUWExecProxy(t, h)
	proxy.insuranceRows = append(uwExecBoardRows(base), uwExecPolicyRow(uwExecSecondPolicy(base)))
	proxy.membership = map[string]bool{}
	proxy.underwrite = func(writer http.ResponseWriter, request *http.Request, body []byte) {
		var requested struct {
			PolicyID string `json:"policy_id"`
			SliceBPS int64  `json:"slice_bps"`
		}
		if err := json.Unmarshal(body, &requested); err != nil || requested.SliceBPS != uwExecSliceBPS ||
			requested.PolicyID != uwExecPolicyID && requested.PolicyID != uwExecSecondPolicyID {
			proxy.recordMutation(request, body, http.StatusBadRequest)
			uwExecWriteProblem(writer, http.StatusBadRequest, "invalid_underwrite_request", "unknown policy or slice")
			return
		}
		proxy.recordMutation(request, body, http.StatusOK)
		policy, rowID := uwExecPolicy(base), uwExecOwnedRowID
		if requested.PolicyID == uwExecSecondPolicyID {
			policy, rowID = uwExecSecondPolicy(base), uwExecSecondRowID
		}
		owned := uwExecRow(recoveryCompanyID, uwExecSliceBPS, uwExecCollateralCeiling, uwExecPremiumFloor+1)
		owned["id"] = rowID
		owned["policy_id"] = requested.PolicyID
		uwExecWriteJSON(writer, http.StatusOK, uwExecEnvelope(policy, []map[string]any{owned}))
		proxy.mu.Lock()
		proxy.membership[requested.PolicyID] = true
		proxy.mu.Unlock()
	}
	runner := h.runner(primaryTestKey)

	// Phase A: the first slice executes normally. A and B sit on the board
	// with identical terms; the equal-net lower-ID tie-break picks A.
	uwExecTicks(t, h, runner, 6)
	attempts := uwExecAttempts(t, h)
	if len(attempts) != 1 || !strings.Contains(string(attempts[0].Body), uwExecPolicyID) {
		t.Fatalf("first slice did not execute exactly once for policy A: %+v", attempts)
	}
	firstKey := attempts[0].IdempotencyKey
	positions, err := h.db.UnderwritingPositions(h.ctx, h.account.ID)
	if err != nil || len(positions) != 1 || positions[0].PolicyID != uwExecPolicyID {
		t.Fatalf("expected exactly one durable position for policy A: %+v err %v", positions, err)
	}

	// Phase B (fail-closed): policy B is fresh and otherwise profitable, but
	// position A still lacks its exact ledger-to-vault debit — no second
	// underwriting, a durable funding-wait hold, and no invented evidence.
	uwExecTicks(t, h, runner, 6)
	if got := uwExecAttempts(t, h); len(got) != 1 {
		t.Fatalf("unfunded position allowed %d further underwriting requests: %+v", len(got)-1, got)
	}
	gated := false
	for _, decision := range uwExecDecisions(t, h) {
		if decision.ShipID == uwExecMissionID && decision.Kind == "hold" &&
			strings.Contains(decision.Reason, "awaits exact ledger-to-vault debit") {
			gated = true
			break
		}
	}
	if !gated {
		t.Fatalf("unfunded position did not record the funding-wait hold for __underwriting: %v", uwExecDecisions(t, h))
	}
	positions, err = h.db.UnderwritingPositions(h.ctx, h.account.ID)
	if err != nil || len(positions) != 1 || len(positions[0].FundingEvidence) != 0 {
		t.Fatalf("unfunded phase invented or attached funding evidence: %+v err %v", positions, err)
	}

	// Phase C (positive control): the exact unique identity-linked own-vault
	// collateral debit appears in a fresh ledger; the proof is recorded and
	// policy B's slice is then permitted within the pilot budget.
	positions, err = h.db.UnderwritingPositions(h.ctx, h.account.ID)
	if err != nil || len(positions) != 1 {
		t.Fatalf("lost position A before funding: %+v err %v", positions, err)
	}
	proxy.ledgerRows = []map[string]any{{
		"id":               "uw-debit-a",
		"debit_account_id": "vault-north",
		"amount_pence":     positions[0].CollateralPence,
		"metadata":         map[string]any{"underwriting_id": positions[0].ID},
		"created_at":       time.Now().UTC().Add(time.Minute).Format(time.RFC3339Nano),
	}}
	uwExecTicks(t, h, runner, 6)
	attempts = uwExecAttempts(t, h)
	if len(attempts) != 2 || !strings.Contains(string(attempts[1].Body), uwExecSecondPolicyID) {
		t.Fatalf("funded position did not re-open decisions for policy B: %+v", attempts)
	}
	if attempts[1].IdempotencyKey == firstKey {
		t.Fatalf("second slice reused the first action's idempotency key: %s", firstKey)
	}
	positions, err = h.db.UnderwritingPositions(h.ctx, h.account.ID)
	if err != nil || len(positions) != 2 {
		t.Fatalf("expected two durable positions after funding opened the gate: %+v err %v", positions, err)
	}
	funded, second := false, false
	for _, position := range positions {
		switch position.PolicyID {
		case uwExecPolicyID:
			funded = len(position.FundingEvidence) > 0 && position.Status == "active"
		case uwExecSecondPolicyID:
			second = position.ActionID == attempts[1].IdempotencyKey && position.SliceBPS == uwExecSliceBPS &&
				position.CollateralPence == uwExecCollateralCeiling && position.PremiumPence == uwExecPremiumFloor+1 &&
				position.Status == "active"
		}
	}
	if !funded || !second {
		t.Fatalf("position state wrong after the gate opened: funded=%v second=%v (%+v)", funded, second, positions)
	}
	uwExecRequireNoOrphanDecision(t, h)
}
