package engine

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"reflect"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/DevYukine/go-tradewinds/internal/gameapi"
	"github.com/DevYukine/go-tradewinds/internal/store"
	"github.com/google/uuid"
	"go.uber.org/zap"
	"go.uber.org/zap/zaptest/observer"
)

func requireSnapshotHistoryRows(t *testing.T, raw json.RawMessage, want []string) {
	t.Helper()
	var rows []json.RawMessage
	if err := json.Unmarshal(raw, &rows); err != nil {
		t.Fatal(err)
	}
	if rows == nil || len(rows) != len(want) {
		t.Fatalf("retained %d rows, want %d: %s", len(rows), len(want), raw)
	}
	for index, row := range rows {
		var got, expected any
		if json.Unmarshal(row, &got) != nil || json.Unmarshal([]byte(want[index]), &expected) != nil || !reflect.DeepEqual(got, expected) {
			t.Errorf("row %d lost fields: got %s, want %s", index, row, want[index])
		}
	}
}

func TestSnapshotHistoryFillsRetentionPreservesBackfilledPages(t *testing.T) {
	t.Parallel()
	now := moneyTime("2026-11-02T12:00:00Z")
	kept := []string{
		`{"id":"edge", "created_at":"2026-10-26T12:00:00Z", "extra":1e+2}`,
		`{"id":"recent", "created_at":"2026-11-02T11:59:00Z", "note":"\u0061 <raw>"}`,
		`{"id":"missing", "quantity":10}`,
		`{"id":"unreadable", "created_at":"yesterday"}`,
		`{"id":"null", "created_at":null}`,
	}
	rows := append([]string{`{"id":"old","created_at":"2026-10-26T11:59:59Z"}`}, kept...)
	runner := New(nil, nil, store.Account{}, zap.NewNop(), Options{})
	if !runner.applyCapture(context.Background(), "fills", fetchResult{
		data: json.RawMessage("[" + strings.Join(rows, ",") + "]"), attemptedAt: now.Add(-time.Hour), completedAt: now,
	}) {
		t.Fatalf("fills capture failed: %+v", runner.meta["fills"])
	}
	requireSnapshotHistoryRows(t, runner.snapshot["fills"], kept)

	var pages atomic.Int32
	recent := `{"id":"new", "created_at":"` + time.Now().UTC().Format(time.RFC3339Nano) + `", "note":"\u0062 <raw>"}`
	unreadable := `{"id":"bad-date", "created_at":"unknown"}`
	missing := `{"id":"no-date"}`
	page := []string{recent, unreadable, missing}
	for index := len(page); index < 200; index++ {
		page = append(page, fmt.Sprintf(`{"id":"old-%d","created_at":"2020-01-01T00:00:00Z"}`, index))
	}
	fetched := newRetentionRunner(t, http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		pages.Add(1)
		if req.URL.Path != "/v1/market/fills" || req.URL.Query().Get("limit") != "200" {
			t.Errorf("unexpected fills page: %s", req.URL)
		}
		if req.URL.Query().Get("cursor") == "" {
			_, _ = w.Write([]byte("[" + strings.Join(page, ",") + "]"))
			return
		}
		_, _ = w.Write([]byte("[" + strings.Replace(recent, `"new"`, `"backfilled"`, 1) + "]"))
	}))
	if err := fetched.captureWave(context.Background(), []fetchTask{{key: "fills", path: "/v1/market/fills", kind: fetchPages}}, nil); err != nil {
		t.Fatal(err)
	}
	if pages.Load() != 2 || !fetched.fresh("fills") {
		t.Fatalf("fills stopped before a diagnostic backfill page: pages %d, meta %+v", pages.Load(), fetched.meta["fills"])
	}
	requireSnapshotHistoryRows(t, fetched.snapshot["fills"], []string{recent, unreadable, missing, strings.Replace(recent, `"new"`, `"backfilled"`, 1)})
}

func TestSnapshotHistoryContractsRetainRawRecentAndLiveRows(t *testing.T) {
	t.Parallel()
	now := moneyTime("2026-11-02T12:00:00Z")
	live := `{"id":"live", "status":"open", "updated_at":"2026-01-01T00:00:00Z", "expires_at":"2026-11-03T00:00:00Z", "title":"\u0041 <charter>", "gossip": {"heard_at_port_id":"london"}}`
	expiryEdge := `{"id":"expiry-edge","status":"active","expires_at":"2026-10-26T12:00:00Z"}`
	missing := `{"id":"missing-expiry","status":"active"}`
	unreadable := `{"id":"bad-expiry","status":"active","expires_at":"unknown"}`
	open := []string{`{"id":"expired","status":"open","expires_at":"2026-10-26T11:59:59Z"}`, live}
	active := []string{expiryEdge, missing, unreadable}
	want := []string{live, expiryEdge, missing, unreadable}
	terminalPages := make(map[string][]string)
	for _, status := range []string{"completed", "cancelled", "defaulted"} {
		old := fmt.Sprintf(`{"id":"old-%s","status":%q,"updated_at":"2026-10-26T11:59:59Z"}`, status, status)
		recent := fmt.Sprintf(`{"id":"recent-%s", "status":%q, "created_at":"2026-01-01T00:00:00Z", "updated_at":"2026-10-26T12:00:00Z", "title":"\u0042 <raw>", "gossip":{"news_as_of":"old"}}`, status, status)
		bad := fmt.Sprintf(`{"id":"bad-%s","status":%q,"updated_at":"unknown"}`, status, status)
		absent := fmt.Sprintf(`{"id":"missing-%s","status":%q}`, status, status)
		terminalPages[status] = []string{old, recent, bad, absent}
		want = append(want, recent, bad, absent)
	}
	var completedPages atomic.Int32
	runner := newRetentionRunner(t, http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		status := req.URL.Query().Get("status")
		var rows []string
		switch status {
		case "open":
			rows = open
		case "active":
			rows = active
		case "completed":
			completedPages.Add(1)
			if req.URL.Query().Get("cursor") == "" {
				for index := range 200 {
					rows = append(rows, fmt.Sprintf(`{"id":"completed-page-%d","status":"completed","created_at":"2026-10-01T00:00:00Z","updated_at":"2026-10-02T00:00:00Z"}`, index))
				}
			} else {
				rows = terminalPages[status]
			}
		default:
			rows = terminalPages[status]
		}
		_, _ = w.Write([]byte("[" + strings.Join(rows, ",") + "]"))
	}))
	raw, err := readContracts(context.Background(), runner.api)
	if err != nil {
		t.Fatal(err)
	}
	if completedPages.Load() != 2 {
		t.Fatalf("old created/updated rows hid a later recent completion: pages %d", completedPages.Load())
	}
	if !runner.applyCapture(context.Background(), "contracts", fetchResult{data: raw, attemptedAt: now.Add(-time.Hour), completedAt: now}) {
		t.Fatalf("contracts capture failed: %+v", runner.meta["contracts"])
	}
	requireSnapshotHistoryRows(t, runner.snapshot["contracts"], want)
}

func TestSnapshotHistoryInsuranceRetainsRawRecentAndLiveRows(t *testing.T) {
	t.Parallel()
	now := moneyTime("2026-11-02T12:00:00Z")
	cutoff := now.Add(-7 * 24 * time.Hour)
	row := func(index int, status string, updated, expiry time.Time) string {
		policy := uwExecPolicy(now)
		policy.ID = fmt.Sprintf("01a16b00-7000-7000-8000-%012d", index)
		policy.Status, policy.CreatedAt, policy.UpdatedAt, policy.ExpiresAt = status, now.Add(-30*24*time.Hour), updated, expiry
		return strings.TrimSuffix(string(encode(policy)), "}") + `, "memo":"\u0043 <raw>", "extra":1e+2}`
	}
	var rows, want []string
	for index, status := range []string{"settled_safe", "claimed_loss", "cancelled"} {
		rows = append(rows, row(index*2, status, cutoff.Add(-time.Second), cutoff.Add(-time.Hour)))
		recent := row(index*2+1, status, cutoff, cutoff.Add(-time.Hour))
		rows = append(rows, recent)
		want = append(want, recent)
	}
	rows = append(rows, row(6, "open", now, cutoff.Add(-time.Second)))
	for index, status := range []string{"open", "active"} {
		live := row(index+7, status, cutoff.Add(-time.Hour), now.Add(time.Hour))
		edge := row(index+9, status, cutoff.Add(-time.Hour), cutoff)
		rows = append(rows, live, edge)
		want = append(want, live, edge)
	}
	for _, key := range []string{"insurance", "insurance_owned"} {
		t.Run(key, func(t *testing.T) {
			runner := New(nil, nil, store.Account{}, zap.NewNop(), Options{})
			if !runner.applyCapture(context.Background(), key, fetchResult{
				data: json.RawMessage("[" + strings.Join(rows, ",") + "]"), attemptedAt: now.Add(-time.Hour), completedAt: now,
			}) {
				t.Fatalf("policy capture failed: %+v", runner.meta[key])
			}
			requireSnapshotHistoryRows(t, runner.snapshot[key], want)
			unreadable := []string{
				`{"id":"terminal-bad","status":"settled_safe","updated_at":"unknown"}`,
				`{"id":"terminal-missing","status":"claimed_loss"}`,
				`{"id":"live-bad","status":"active","expires_at":"unknown"}`,
				`{"id":"live-missing","status":"open"}`,
			}
			raw := json.RawMessage("[" + strings.Join(append(rows[:len(rows):len(rows)], unreadable...), ",") + "]")
			retained, err := retainRecentRows(raw, cutoff, func(row json.RawMessage) time.Time {
				return snapshotRowTimestamp(key, row)
			})
			if err != nil {
				t.Fatal(err)
			}
			requireSnapshotHistoryRows(t, retained, append(want[:len(want):len(want)], unreadable...))
			lastGood := bytes.Clone(runner.snapshot[key])
			if runner.applyCapture(context.Background(), key, fetchResult{data: raw, completedAt: now}) || !bytes.Equal(runner.snapshot[key], lastGood) {
				t.Fatal("retention bypassed policy validation or erased its last-good capture")
			}
		})
	}
}

func TestSnapshotHistoryUnderwritingPositionsKeepsOpenAndRecentWithoutDeletingStore(t *testing.T) {
	h := newRecoveryHarness(t)
	now := time.Now().UTC().Truncate(time.Microsecond)
	for index, closedAt := range []*time.Time{nil, new(now.Add(-time.Hour)), new(now.Add(-8 * 24 * time.Hour))} {
		policy := uwExecPolicy(now.Add(-20 * 24 * time.Hour))
		policy.ID = uuid.NewString()
		position := store.UnderwritingPosition{
			ID: uuid.NewString(), AccountID: h.account.ID, ActionID: uuid.NewString(), PolicyID: policy.ID,
			CompanyID: h.account.CompanyID, SliceBPS: uwExecSliceBPS, CollateralPence: uwExecCollateralCeiling,
			PremiumPence: uwExecPremiumFloor, FrozenPolicy: encode(policy), AcceptedAt: now.Add(-19*24*time.Hour + time.Duration(index)*time.Hour), Status: "active",
		}
		if _, err := h.db.BeginAction(h.ctx, &store.Action{
			ID: position.ActionID, AccountID: h.account.ID, Method: http.MethodPost, Path: "/v1/insurance/underwrite",
			Body: encode(map[string]any{"policy_id": policy.ID, "slice_bps": uwExecSliceBPS}),
		}); err != nil {
			t.Fatal(err)
		}
		owned := uwExecRow(h.account.CompanyID, uwExecSliceBPS, uwExecCollateralCeiling, uwExecPremiumFloor)
		owned["id"], owned["policy_id"] = position.ID, policy.ID
		if err := h.db.CompleteAction(h.ctx, position.ActionID, http.StatusOK, uwExecEnvelope(policy, []map[string]any{owned}), nil); err != nil {
			t.Fatal(err)
		}
		if err := h.db.PutUnderwritingPosition(h.ctx, &position); err != nil {
			t.Fatal(err)
		}
		if closedAt != nil {
			if err := h.db.CloseUnderwritingPosition(h.ctx, store.UnderwritingClosure{
				AccountID: h.account.ID, ID: position.ID, Status: "settled_safe", EarnedPremiumPence: position.PremiumPence,
				ReturnedCollateralPence: position.CollateralPence, ClosedAt: *closedAt, Evidence: encode(map[string]string{"policy_id": policy.ID, "status": "settled_safe"}),
			}); err != nil {
				t.Fatal(err)
			}
		}
	}
	positions, err := h.db.UnderwritingPositions(h.ctx, h.account.ID)
	if err != nil || len(positions) != 3 {
		t.Fatalf("seeded positions = %+v, error %v", positions, err)
	}
	restarted := h.runner(primaryTestKey)
	for _, test := range []struct {
		name string
		age  time.Duration
		keep bool
	}{
		{"two-day open position", 2 * 24 * time.Hour, false},
		{"six-day open position", 6 * 24 * time.Hour, false},
		{"past six-day open position", 6*24*time.Hour + time.Second, true},
	} {
		t.Run(test.name, func(t *testing.T) {
			requireSnapshotHistoryRetention(t, restarted, positions[0].AcceptedAt.Add(test.age), false, test.keep)
		})
	}
	runner := h.runner(primaryTestKey)
	if err := runner.recordUnderwriting(h.ctx); err != nil {
		t.Fatal(err)
	}
	requireSnapshotHistoryRows(t, runner.snapshot["underwriting_positions"], []string{string(encode(positions[0])), string(encode(positions[1]))})
	stored, err := h.db.UnderwritingPositions(h.ctx, h.account.ID)
	if err != nil || !bytes.Equal(encode(stored), encode(positions)) {
		t.Fatalf("snapshot retention changed permanent underwriting history: %+v, error %v", stored, err)
	}
}

func TestPassengerFareCeilingIgnoresOnlyFreightOlderThanSnapshotHistory(t *testing.T) {
	in := passengerInputFixture()
	groups := []gameapi.PassengerWaitingGroup{{ID: "whole", Kind: "travellers", Size: 10, OriginPortID: "origin", DestPortID: "destination", BoardBy: in.Now.Add(time.Hour)}}
	contract := map[string]any{
		"id": "old-freight", "contract_type": "freight", "status": "completed", "origin_port_id": "origin", "dest_port_id": "destination",
		"quantity_required": 10, "reward_pence": 1_000_000, "updated_at": in.Now.Add(-8 * 24 * time.Hour),
	}
	raw := encode([]any{contract})
	in.Contracts = decode[[]Contract](map[string]json.RawMessage{"contracts": raw}, "contracts")
	before, err := quotePassage(in, "destination", groups, nil, gameapi.PassengerFareSummary{}, nil)
	if err != nil || before.Ceiling != 400_000 {
		t.Fatalf("freight positive control = %+v, error %v", before, err)
	}
	runner := New(nil, nil, store.Account{}, zap.NewNop(), Options{})
	if !runner.applyCapture(context.Background(), "contracts", fetchResult{data: raw, attemptedAt: in.Now, completedAt: in.Now}) {
		t.Fatalf("contract capture failed: %+v", runner.meta["contracts"])
	}
	in.Contracts = decode[[]Contract](runner.snapshot, "contracts")
	after, err := quotePassage(in, "destination", groups, nil, gameapi.PassengerFareSummary{}, nil)
	if err != nil || after.Floor <= 0 || after.Ceiling != after.Floor || after.Ceiling >= before.Ceiling {
		t.Fatalf("expired freight still inflates the no-observation fare ceiling: before %+v, after %+v, error %v", before, after, err)
	}
	contract["updated_at"] = in.Now.Add(-6 * 24 * time.Hour)
	if !runner.applyCapture(context.Background(), "contracts", fetchResult{data: encode([]any{contract}), attemptedAt: in.Now, completedAt: in.Now}) {
		t.Fatal("recent freight capture failed")
	}
	in.Contracts = decode[[]Contract](runner.snapshot, "contracts")
	recent, err := quotePassage(in, "destination", groups, nil, gameapi.PassengerFareSummary{}, nil)
	if err != nil || recent.Ceiling != before.Ceiling {
		t.Fatalf("retention also erased six-day freight fare evidence: %+v, error %v", recent, err)
	}
}

func TestSnapshotHistoryAcceptedExceptionsRemainWhole(t *testing.T) {
	t.Parallel()
	now := moneyTime("2026-11-02T12:00:00Z")
	old := now.Add(-8 * 24 * time.Hour)
	rows := map[string]json.RawMessage{
		"convoys": encode([]any{map[string]any{"convoy": map[string]any{"id": "old-convoy", "status": "completed", "updated_at": old}, "members": []string{"kept"}}}),
		"marques": encode([]letterOfMarque{{
			ID: uuid.NewString(), SeaZoneID: "channel", AddedPiracyBPS: 500, CostPence: 1000, PrizeEarnedPence: 2000,
			CreatedAt: old.Add(-time.Hour), ExpiresAt: old, UpdatedAt: old,
		}}),
		"patrols":       encode([]any{map[string]any{"id": "old-patrol", "expires_at": old, "updated_at": old}}),
		"futures":       encode([]any{map[string]any{"id": "old-future", "status": "settled", "updated_at": old}}),
		"ship_listings": encode([]any{map[string]any{"id": "old-listing", "ship_id": recoveryShipBlastoise, "status": "sold", "closed_at": old}}),
	}
	for key, raw := range rows {
		runner := New(nil, nil, store.Account{}, zap.NewNop(), Options{})
		if !runner.applyCapture(context.Background(), key, fetchResult{data: raw, completedAt: now}) || !bytes.Equal(runner.snapshot[key], raw) {
			t.Fatalf("accepted history exception %s was trimmed or rewritten: %s", key, runner.snapshot[key])
		}
	}
}

func requireSnapshotHistoryRetention(t *testing.T, runner *Runner, now time.Time, keepContracts, keepInsurance bool) {
	t.Helper()
	contract := string(encode(map[string]any{"id": uuid.NewString(), "status": "completed", "updated_at": now.Add(-8 * 24 * time.Hour), "title": "retained <gossip>"}))
	policy := uwExecPolicy(now.Add(-10 * 24 * time.Hour))
	policy.Status, policy.UpdatedAt = "settled_safe", now.Add(-8*24*time.Hour)
	for key, row := range map[string]string{"contracts": contract, "insurance": string(encode(policy)), "insurance_owned": string(encode(policy))} {
		if !runner.applyCapture(context.Background(), key, fetchResult{data: json.RawMessage("[" + row + "]"), completedAt: now}) {
			t.Fatalf("%s capture failed: %+v", key, runner.meta[key])
		}
		var want []string
		if key == "contracts" && keepContracts || key != "contracts" && keepInsurance {
			want = []string{row}
		}
		requireSnapshotHistoryRows(t, runner.snapshot[key], want)
	}
}

func TestSnapshotHistoryPendingContractMissionAgeControlsRetention(t *testing.T) {
	h := newRecoveryHarness(t)
	now := time.Now().UTC()
	for _, test := range []struct {
		name       string
		missionAge time.Duration
		actionAge  time.Duration
		keep       bool
	}{
		{"eight-day commission", 8 * 24 * time.Hour, 2 * 24 * time.Hour, true},
		{"two-day commission", 2 * 24 * time.Hour, 2 * 24 * time.Hour, false},
		{"six-day commission boundary", 6 * 24 * time.Hour, 2 * 24 * time.Hour, false},
		{"past six-day commission boundary", 6*24*time.Hour + time.Second, 2 * 24 * time.Hour, true},
		{"eight-day contract action", 2 * 24 * time.Hour, 8 * 24 * time.Hour, true},
		{"no pending mission", 0, 0, false},
	} {
		t.Run(test.name, func(t *testing.T) {
			if test.missionAge == 0 {
				if err := h.db.DeleteMission(h.ctx, h.account.ID, recoveryShipBlastoise); err != nil {
					t.Fatal(err)
				}
			} else {
				h.putMission(recoveryShipBlastoise, "pending-retention", "commission_build", missionData{
					StartedAt: now.Add(-test.missionAge), Commission: &commissionIntent{},
					Action: &durableAction{ID: uuid.NewString(), CreatedAt: now.Add(-test.actionAge), Path: "/v1/contract/accept-commission"},
				})
			}
			requireSnapshotHistoryRetention(t, h.runner(primaryTestKey), now, test.keep, false)
		})
	}
	for _, kind := range []string{"spot", "charter", "freight", "spot+freight", "charter+freight", "relocation", "recovery", "aborted"} {
		t.Run(kind, func(t *testing.T) {
			h.putMission(recoveryShipBlastoise, "pending-retention", "arrival", missionData{
				Plan: Plan{Kind: kind, ShipID: recoveryShipBlastoise}, StartedAt: now.Add(-8 * 24 * time.Hour),
			})
			requireSnapshotHistoryRetention(t, h.runner(primaryTestKey), now, true, false)
		})
	}
}

func TestSnapshotHistoryPendingPolicyMissionAgeControlsRetention(t *testing.T) {
	h := newRecoveryHarness(t)
	now := time.Now().UTC()
	for _, test := range []struct {
		name       string
		kind       string
		missionAge time.Duration
		actionAge  time.Duration
		keep       bool
	}{
		{"eight-day underwriting", "underwriting", 8 * 24 * time.Hour, 2 * 24 * time.Hour, true},
		{"two-day underwriting", "underwriting", 2 * 24 * time.Hour, 2 * 24 * time.Hour, false},
		{"eight-day underwriting action", "underwriting", 2 * 24 * time.Hour, 8 * 24 * time.Hour, true},
		{"eight-day passage", "passage", 8 * 24 * time.Hour, 2 * 24 * time.Hour, true},
		{"two-day passage", "passage", 2 * 24 * time.Hour, 2 * 24 * time.Hour, false},
		{"six-day passage boundary", "passage", 6 * 24 * time.Hour, 2 * 24 * time.Hour, false},
		{"eight-day passage action", "passage", 2 * 24 * time.Hour, 8 * 24 * time.Hour, true},
	} {
		t.Run(test.name, func(t *testing.T) {
			data := missionData{
				Plan: Plan{Kind: test.kind}, StartedAt: now.Add(-test.missionAge),
				Action: &durableAction{ID: uuid.NewString(), CreatedAt: now.Add(-test.actionAge), Path: "/v1/insurance/create"},
			}
			if test.kind == "underwriting" {
				data.Underwriting = &underwritingIntent{Policy: uwExecPolicy(now.Add(-test.missionAge))}
				data.Action.Path = "/v1/insurance/underwrite"
			} else {
				data.VoyageInsurance = []voyageInsuranceIntent{{}}
			}
			h.putMission(recoveryShipBlastoise, "pending-retention", "voyage-insure", data)
			requireSnapshotHistoryRetention(t, h.runner(primaryTestKey), now, false, test.keep)
		})
	}
}

func TestSnapshotHistoryMaintenanceAndUndatedMissions(t *testing.T) {
	h := newRecoveryHarness(t)
	now := time.Now().UTC()
	for _, kind := range []string{"maintenance", "treasury", "bulletin", "marque", "futures"} {
		t.Run(kind, func(t *testing.T) {
			h.putMission(recoveryShipBlastoise, "pending-retention", kind, missionData{
				Plan: Plan{Kind: kind, ShipID: recoveryShipBlastoise}, StartedAt: now.Add(-8 * 24 * time.Hour),
				Action: &durableAction{ID: uuid.NewString(), CreatedAt: now.Add(-8 * 24 * time.Hour), Path: "/v1/fleet/dock"},
			})
			requireSnapshotHistoryRetention(t, h.runner(primaryTestKey), now, false, false)
		})
	}
	for _, test := range []struct {
		name         string
		kind         string
		actionAge    time.Duration
		captureAhead time.Duration
		keep         bool
	}{
		{"maintenance without start", "maintenance", 0, 0, false},
		{"contract without start or action", "charter", 0, 0, false},
		{"contract with young action", "charter", 2 * 24 * time.Hour, 0, false},
		{"contract with old action", "charter", 8 * 24 * time.Hour, 0, true},
		{"contract with old row timestamp", "charter", 0, 8 * 24 * time.Hour, true},
		{"policy without start or action", "passage", 0, 0, false},
		{"policy with young action", "passage", 2 * 24 * time.Hour, 0, false},
		{"policy with old action", "passage", 8 * 24 * time.Hour, 0, true},
		{"policy with old row timestamp", "passage", 0, 8 * 24 * time.Hour, true},
	} {
		t.Run(test.name, func(t *testing.T) {
			data := missionData{Plan: Plan{Kind: test.kind, ShipID: recoveryShipBlastoise}}
			if test.kind == "passage" {
				data.VoyageInsurance = []voyageInsuranceIntent{{}}
			}
			if test.actionAge != 0 {
				data.Action = &durableAction{ID: uuid.NewString(), CreatedAt: now.Add(-test.actionAge), Path: "/v1/fleet/depart"}
			}
			h.putMission(recoveryShipBlastoise, "pending-retention", "pending", data)
			requireSnapshotHistoryRetention(t, h.runner(primaryTestKey), now.Add(test.captureAhead), test.keep && test.kind == "charter", test.keep && test.kind == "passage")
		})
	}
}

func TestSnapshotHistoryPendingActionWithoutMission(t *testing.T) {
	for _, test := range []struct {
		name          string
		path          string
		age           time.Duration
		keepContracts bool
		keepInsurance bool
	}{
		{"old contract action", "/v1/contract/deliver-freight", 8 * 24 * time.Hour, true, false},
		{"young contract action", "/v1/contract/deliver-freight", 2 * 24 * time.Hour, false, false},
		{"old insurance action", "/v1/insurance/create", 8 * 24 * time.Hour, false, true},
		{"young insurance action", "/v1/insurance/create", 2 * 24 * time.Hour, false, false},
		{"old control action", "/v1/fleet/dock", 8 * 24 * time.Hour, false, false},
	} {
		t.Run(test.name, func(t *testing.T) {
			h := newRecoveryHarness(t)
			action, err := h.db.BeginAction(h.ctx, &store.Action{
				ID: uuid.NewString(), AccountID: h.account.ID, Method: http.MethodPost, Path: test.path, Body: json.RawMessage(`{}`),
			})
			if err != nil {
				t.Fatal(err)
			}
			// Advance the capture clock rather than mutate immutable journal dates.
			requireSnapshotHistoryRetention(t, h.runner(primaryTestKey), action.CreatedAt.Add(test.age), test.keepContracts, test.keepInsurance)
		})
	}
}

func TestSnapshotHistoryPendingStateReadErrorKeepsCaptureFresh(t *testing.T) {
	h := newRecoveryHarness(t)
	now := time.Now().UTC()
	policy := uwExecPolicy(now.Add(-10 * 24 * time.Hour))
	policy.Status, policy.UpdatedAt = "settled_safe", now.Add(-8*24*time.Hour)
	rows := map[string]json.RawMessage{
		"contracts": json.RawMessage(`[{"id":"old","status":"completed","updated_at":"2020-01-01T00:00:00Z"}]`),
		"insurance": encode([]InsurancePolicy{policy}), "insurance_owned": encode([]InsurancePolicy{policy}),
		"fills":        json.RawMessage(`[{"id":"old-fill","created_at":"2020-01-01T00:00:00Z"}]`),
		"world_events": json.RawMessage(`[{"id":"old-event","expires_at":"2020-01-01T00:00:00Z"}]`),
	}
	runner := newRetentionRunner(t, http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		_, _ = w.Write(rows[strings.TrimPrefix(req.URL.Path, "/")])
	}))
	runner.db, runner.account = h.db, h.account
	core, logs := observer.New(zap.WarnLevel)
	runner.log = zap.New(core)
	if err := h.db.Close(); err != nil {
		t.Fatal(err)
	}
	var tasks []fetchTask
	for key := range rows {
		tasks = append(tasks, fetchTask{key: key, path: "/" + key, kind: fetchJSON})
	}
	if err := runner.captureWave(h.ctx, tasks, nil); err != nil {
		t.Fatal(err)
	}
	for key, raw := range rows {
		var want []string
		if snapshotNeedsRetentionState(key) {
			want = []string{string(decode[[]json.RawMessage](rows, key)[0])}
			if !bytes.Equal(runner.snapshot[key], raw) {
				t.Fatalf("%s read failure trimmed recovery history: %s", key, runner.snapshot[key])
			}
		}
		requireSnapshotHistoryRows(t, runner.snapshot[key], want)
		if !runner.fresh(key) {
			t.Fatalf("%s read failure made the new capture stale: %+v", key, runner.meta[key])
		}
	}
	if warnings := logs.FilterMessage("snapshot retention skipped; pending recovery state unavailable").Len(); warnings != 1 {
		t.Fatalf("pending-state read error logged %d warnings, want one per wave", warnings)
	}
}

func TestSnapshotHistoryCompletedCommissionSurvivesRestartPastRetention(t *testing.T) {
	h, original, mission, data := acceptedCommission(t)
	if _, err := original.driveCommissions(h.ctx); err != nil {
		t.Fatal(err)
	}
	mission, data = h.mission(commissionMission)
	now := time.Now().UTC()
	data.StartedAt = now.Add(-8 * 24 * time.Hour)
	data.Commission.Contract.ExpiresAt = new(now.Add(-8 * 24 * time.Hour))
	if err := original.saveMission(h.ctx, mission, &data); err != nil {
		t.Fatal(err)
	}
	action, err := h.db.BeginAction(h.ctx, &store.Action{
		ID: data.Action.ID, AccountID: h.account.ID, Method: data.Action.Method, Path: data.Action.Path, Body: data.Action.Body,
	})
	if err != nil {
		t.Fatal(err)
	}
	// Match the existing replay-window tests without changing journal clocks.
	action.CreatedAt = now.Add(-9 * 24 * time.Hour)
	runner := h.runner(primaryTestKey)
	loanCapacityRefresh(t, h, runner)
	row := data.Commission.Contract
	row.Status, row.CarrierCompanyID, row.UpdatedAt = "completed", new(h.account.CompanyID), now.Add(-8*24*time.Hour)
	if !runner.applyCapture(h.ctx, "contracts", fetchResult{data: encode([]gameapi.CommissionListing{row}), attemptedAt: now, completedAt: now}) {
		t.Fatalf("commission capture failed: %+v", runner.meta["contracts"])
	}
	mission, data, err = runner.loadMission(h.ctx, commissionMission)
	if err != nil {
		t.Fatal(err)
	}
	changed, err := runner.reconcileCommission(h.ctx, mission, &data, action)
	if err != nil || !changed || data.Settlement == nil || data.Settlement.ProceedsPence != row.RewardPence || data.Settlement.ExpensesPence != 0 {
		t.Fatalf("old completed commission became a false default after restart: changed=%v data=%+v err=%v", changed, data, err)
	}
	reconciled, err := h.db.BeginAction(h.ctx, action)
	if err != nil || reconciled == nil || !strings.Contains(string(reconciled.Response), "authoritative_commission_terminal") {
		t.Fatalf("terminal row was replaced by missing-builder default evidence: %+v, error %v", reconciled, err)
	}
	if !runner.applyCapture(h.ctx, "contracts", fetchResult{data: encode([]gameapi.CommissionListing{row}), attemptedAt: now, completedAt: now}) {
		t.Fatal("resolved commission capture failed")
	}
	requireSnapshotHistoryRows(t, runner.snapshot["contracts"], nil)
}

func TestSnapshotHistoryWorldEventFetchRetainsLongActiveEvents(t *testing.T) {
	t.Parallel()
	now := time.Now().UTC()
	expired := string(encode(map[string]any{"id": "old", "starts_at": now.Add(-74 * time.Hour), "expires_at": now.Add(-73 * time.Hour)}))
	recent := string(encode(map[string]any{"id": "recent", "starts_at": now.Add(-72*time.Hour - 30*time.Minute), "expires_at": now.Add(-72*time.Hour + 30*time.Minute), "title": "raw <news>"}))
	active := string(encode(map[string]any{"id": "long-active", "starts_at": now.Add(-240 * time.Hour), "expires_at": now.Add(time.Hour), "title": "long <shock>"}))
	runner := newRetentionRunner(t, http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		if req.URL.Query().Has("since") || req.URL.Query().Get("include_expired") != "true" {
			t.Errorf("world event fetch omitted long-lived or expired events: %s", req.URL)
		}
		_, _ = w.Write([]byte("[" + expired + "," + recent + "," + active + "]"))
	}))
	if err := runner.captureWave(context.Background(), []fetchTask{{key: "world_events", path: "/v1/world/events?include_expired=true", kind: fetchPages}}, nil); err != nil {
		t.Fatal(err)
	}
	requireSnapshotHistoryRows(t, runner.snapshot["world_events"], []string{recent, active})
}
