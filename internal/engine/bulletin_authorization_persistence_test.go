package engine

import (
	"bytes"
	"context"
	"crypto/aes"
	"crypto/cipher"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/DevYukine/go-tradewinds/internal/store"
)

type bulletinAuthorizationDispatchResult struct {
	changed bool
	err     error
}

func bulletinAuthorizationOwner(t *testing.T, h *recoveryHarness) (context.Context, func()) {
	t.Helper()
	ownerCtx, release, err := h.db.AcquireAccountContext(h.ctx, h.account.ID)
	if err != nil {
		t.Fatalf("acquire real bulletin account ownership: %v", err)
	}
	t.Cleanup(release)
	return ownerCtx, release
}

func bulletinAuthorizationSQL(t *testing.T, h *recoveryHarness) *sql.DB {
	t.Helper()
	admin, err := sql.Open("pgx", h.dsn)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = admin.Close() })
	var schema string
	if err := admin.QueryRowContext(h.ctx, "SELECT current_schema()").Scan(&schema); err != nil {
		t.Fatal(err)
	}
	if schema != h.schema {
		t.Fatalf("fault connection escaped its disposable schema: got %q want %q", schema, h.schema)
	}
	return admin
}

func bulletinAuthorizationJournal(t *testing.T, h *recoveryHarness, original durableAction) store.Action {
	t.Helper()
	actions, err := h.db.ActionsBetween(h.ctx, h.account.ID, time.Now().UTC().Add(-time.Hour), time.Now().UTC().Add(time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	if len(actions) != 1 {
		t.Fatalf("refusal lost or replaced its sole immutable journal row: %+v", actions)
	}
	action := actions[0]
	if action.ID != original.ID || action.AccountID != h.account.ID || action.Method != original.Method || action.Path != original.Path {
		t.Fatalf("refusal changed its immutable identity: got %+v want %+v", action, original)
	}
	// ActionsBetween projects diagnostic JSON. Exact byte assertions must
	// authenticate the encrypted row, not compare that redacted projection.
	admin := bulletinAuthorizationSQL(t, h)
	var companyID string
	var bodyCipher, responseCipher []byte
	if err := admin.QueryRowContext(h.ctx, `SELECT company_id, body_cipher, response_cipher
		FROM actions WHERE account_id = $1 AND id = $2`, h.account.ID, original.ID).
		Scan(&companyID, &bodyCipher, &responseCipher); err != nil {
		t.Fatal(err)
	}
	if companyID != h.account.CompanyID || len(bodyCipher) == 0 {
		t.Fatalf("encrypted request lost its company binding or body: company=%q body_bytes=%d", companyID, len(bodyCipher))
	}
	block, err := aes.NewCipher(bytes.Repeat([]byte{0x5A}, 32))
	if err != nil {
		t.Fatal(err)
	}
	aead, err := cipher.NewGCM(block)
	if err != nil {
		t.Fatal(err)
	}
	decrypt := func(field string, encrypted []byte) []byte {
		if encrypted == nil {
			return nil
		}
		if len(encrypted) < aead.NonceSize()+aead.Overhead() {
			t.Fatalf("encrypted journal %s is truncated", field)
		}
		nonce := encrypted[:aead.NonceSize()]
		aad := "action\x00" + h.account.ID + "\x00" + original.ID + "\x00" + field
		plain, err := aead.Open(nil, nonce, encrypted[aead.NonceSize():], []byte(aad))
		if err != nil {
			t.Fatalf("authenticate journal %s: %v", field, err)
		}
		return plain
	}
	action.Body = decrypt("body", bodyCipher)
	action.Response = decrypt("response", responseCipher)
	if !bytes.Equal(action.Body, original.Body) {
		t.Fatalf("refusal changed its authenticated immutable request body: got %s want %s", action.Body, original.Body)
	}
	return action
}

func bulletinAuthorizationRetainedMission(t *testing.T, h *recoveryHarness, original store.Mission) {
	t.Helper()
	retained, _ := h.mission("__bulletin")
	if !reflect.DeepEqual(*retained, original) {
		t.Fatalf("failed persistence changed the durable staged mission: got %+v want %+v", *retained, original)
	}
}

func bulletinAuthorizationNoOptionalCooldown(t *testing.T, h *recoveryHarness, runner *Runner, before []byte, rejected int) {
	t.Helper()
	if !bytes.Equal(runner.snapshot["_bulletin"], before) {
		t.Fatalf("authorization stop became an optional in-memory cooldown: %s", runner.snapshot["_bulletin"])
	}
	snapshot, err := h.db.Snapshot(h.ctx, h.account.ID)
	if err != nil {
		t.Fatal(err)
	}
	var beforeState, storedState map[string]any
	if err := json.Unmarshal(before, &beforeState); err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(snapshot["_bulletin"], &storedState); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(storedState, beforeState) {
		t.Fatalf("authorization stop became an optional durable cooldown: %s", snapshot["_bulletin"])
	}
	count := 0
	for _, reason := range bulletinDecisionReasons(t, h) {
		if strings.HasPrefix(reason, "action_rejected ") {
			count++
		}
		if strings.Contains(reason, "public posting deferred") || strings.Contains(reason, "retry cooldown recorded") || strings.Contains(reason, "ambiguous public post retained") {
			t.Fatalf("authorization stop was suppressed as an optional bulletin failure: %q", reason)
		}
	}
	if count != rejected {
		t.Fatalf("rejection decision does not match the committed storage reality: got %d want %d", count, rejected)
	}
}

func TestBulletinAuthorizationPersistenceFailuresStopWork(t *testing.T) {
	for _, refusal := range []struct {
		name   string
		status int
		code   string
	}{
		{"unauthorized", http.StatusUnauthorized, "invalid_api_key"},
		{"forbidden", http.StatusForbidden, "company_suspended"},
	} {
		t.Run(refusal.name, func(t *testing.T) {
			for _, fault := range []struct {
				name       string
				event      string
				condition  string
				marker     string
				state      string
				rejections int
			}{
				{
					name:      "complete_action",
					event:     "BEFORE UPDATE ON actions",
					condition: "WHEN (OLD.state = 'pending' AND NEW.state = 'complete' AND NEW.status IN (401, 403))",
					marker:    "c2_bulletin_complete_action_storage_failure",
					state:     store.ActionPending,
				},
				{
					name:      "action_rejected_decision",
					event:     "BEFORE INSERT ON decisions",
					condition: "WHEN (NEW.ship_id = '__bulletin' AND NEW.kind = 'action_rejected')",
					marker:    "c2_bulletin_action_rejected_storage_failure",
					state:     store.ActionComplete,
				},
				{
					name:       "mission_save",
					event:      "BEFORE INSERT OR UPDATE ON missions",
					condition:  "WHEN (NEW.account_id = 'recovery-account')",
					marker:     "c2_bulletin_mission_save_storage_failure",
					state:      store.ActionComplete,
					rejections: 1,
				},
			} {
				t.Run(fault.name, func(t *testing.T) {
					h, proxy, runner := newBulletinFixture(t)
					idleBulletinRunner(t, h, runner)
					mission, data := stageBulletinFixture(t, h, runner)
					originalMission, originalAction := *mission, *data.Action
					beforeSchedule := bytes.Clone(runner.snapshot["_bulletin"])
					ownerCtx, _ := bulletinAuthorizationOwner(t, h)
					admin := bulletinAuthorizationSQL(t, h)
					// Only additional triggers in this disposable schema are installed.
					// The journal's immutable request and result guards stay enabled.
					statement := fmt.Sprintf(`
CREATE FUNCTION c2_bulletin_authorization_fault() RETURNS trigger LANGUAGE plpgsql AS $$
BEGIN
    RAISE EXCEPTION '%s' USING ERRCODE = 'P0001';
END;
$$;
CREATE TRIGGER c2_bulletin_authorization_fault %s
    FOR EACH ROW %s EXECUTE FUNCTION c2_bulletin_authorization_fault();`, fault.marker, fault.event, fault.condition)
					if _, err := admin.ExecContext(h.ctx, statement); err != nil {
						t.Fatal(err)
					}
					proxy.postStatus, proxy.problemCode, proxy.problemDetail = refusal.status, refusal.code, "known public-write authorization refusal"

					changed, err := runner.work(ownerCtx)
					if changed || !errors.Is(err, ErrExecutionStopped) {
						t.Fatalf("work swallowed the authorization stop after %s failed: changed=%t error=%v", fault.name, changed, err)
					}
					var storageErr interface {
						error
						SQLState() string
					}
					if !errors.As(err, &storageErr) || storageErr.SQLState() != "P0001" || !strings.Contains(storageErr.Error(), fault.marker) || !errors.Is(err, storageErr) {
						t.Fatalf("authorization stop discarded the underlying %s error: %v", fault.marker, err)
					}
					if ownerCtx.Err() != nil {
						t.Fatalf("storage failure incorrectly cancelled live account ownership: %v", ownerCtx.Err())
					}
					if proxy.postCalls != 1 || !bytes.Equal(proxy.bodies[originalAction.ID], originalAction.Body) {
						t.Fatalf("failed outcome persistence resent or changed the request: posts=%d bodies=%v", proxy.postCalls, proxy.bodies)
					}
					action := bulletinAuthorizationJournal(t, h, originalAction)
					if action.State != fault.state {
						t.Fatalf("journal invented or lost completion after %s failed: %+v", fault.name, action)
					}
					if fault.state == store.ActionPending {
						if action.Status != 0 || len(action.Response) != 0 {
							t.Fatalf("failed CompleteAction claimed a stored refusal: %+v", action)
						}
					} else if action.Status != refusal.status || !bytes.Contains(action.Response, []byte(refusal.code)) {
						t.Fatalf("later persistence failure lost the committed refusal: %+v", action)
					}
					pending, pendingErr := h.db.PendingActions(h.ctx, h.account.ID)
					wantPending := 0
					if fault.state == store.ActionPending {
						wantPending = 1
					}
					if pendingErr != nil || len(pending) != wantPending {
						t.Fatalf("pending journal view contradicts failed persistence: pending=%+v error=%v", pending, pendingErr)
					}
					bulletinAuthorizationRetainedMission(t, h, originalMission)
					bulletinAuthorizationNoOptionalCooldown(t, h, runner, beforeSchedule, fault.rejections)
				})
			}
		})
	}
}

func bulletinAuthorizationStoredRefusal(t *testing.T, h *recoveryHarness, ownerCtx context.Context, original durableAction, status int, code string) store.Action {
	t.Helper()
	if _, err := h.db.BeginAction(ownerCtx, &store.Action{ID: original.ID, AccountID: h.account.ID, Method: original.Method, Path: original.Path, Body: original.Body}); err != nil {
		t.Fatal(err)
	}
	response := encode(map[string]string{"code": code, "detail": "stored definitive bulletin authorization refusal"})
	if err := h.db.CompleteAction(ownerCtx, original.ID, status, response, nil); err != nil {
		t.Fatal(err)
	}
	return bulletinAuthorizationJournal(t, h, original)
}

// The advisory lock proves persistence has begun before the request deadline
// expires or ownership is revoked. Its key includes the disposable schema.
func bulletinAuthorizationPersistenceGate(t *testing.T, h *recoveryHarness, admin *sql.DB, missionSave bool) (int, func()) {
	t.Helper()
	lock, err := admin.Conn(h.ctx)
	if err != nil {
		t.Fatal(err)
	}
	var once sync.Once
	unlock := func() {
		once.Do(func() {
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			if _, err := lock.ExecContext(ctx, "SELECT pg_advisory_unlock_all()"); err != nil {
				t.Errorf("release schema-local bulletin persistence gate: %v", err)
			}
			if err := lock.Close(); err != nil {
				t.Errorf("close bulletin persistence gate: %v", err)
			}
		})
	}
	t.Cleanup(unlock)
	var pid int
	if err := lock.QueryRowContext(h.ctx, "SELECT pg_backend_pid()").Scan(&pid); err != nil {
		t.Fatal(err)
	}
	if _, err := lock.ExecContext(h.ctx, "SELECT pg_advisory_lock(hashtextextended(current_schema() || ':c2-bulletin-outcome', 0))"); err != nil {
		t.Fatal(err)
	}
	event := "BEFORE INSERT ON decisions"
	condition := "WHEN (NEW.ship_id = '__bulletin' AND NEW.kind = 'action_rejected')"
	if missionSave {
		event = "BEFORE INSERT OR UPDATE ON missions"
		condition = "WHEN (NEW.account_id = 'recovery-account')"
	}
	statement := fmt.Sprintf(`
CREATE FUNCTION c2_bulletin_authorization_gate() RETURNS trigger LANGUAGE plpgsql AS $$
BEGIN
    PERFORM pg_advisory_xact_lock(hashtextextended(current_schema() || ':c2-bulletin-outcome', 0));
    RETURN NEW;
END;
$$;
CREATE TRIGGER c2_bulletin_authorization_gate %s
    FOR EACH ROW %s EXECUTE FUNCTION c2_bulletin_authorization_gate();`, event, condition)
	if _, err := admin.ExecContext(h.ctx, statement); err != nil {
		t.Fatal(err)
	}
	return pid, unlock
}

func bulletinAuthorizationWaitForPersistence(t *testing.T, h *recoveryHarness, admin *sql.DB, blockerPID int, result <-chan bulletinAuthorizationDispatchResult) {
	t.Helper()
	ticker := time.NewTicker(5 * time.Millisecond)
	defer ticker.Stop()
	timeout := time.NewTimer(5 * time.Second)
	defer timeout.Stop()
	for {
		var blocked bool
		if err := admin.QueryRowContext(h.ctx, `SELECT EXISTS (
    SELECT 1 FROM pg_stat_activity
    WHERE datname = current_database() AND wait_event = 'advisory'
      AND $1 = ANY(pg_blocking_pids(pid))
)`, blockerPID).Scan(&blocked); err != nil {
			t.Fatal(err)
		}
		if blocked {
			return
		}
		select {
		case outcome := <-result:
			t.Fatalf("dispatch finished before reaching the persistence gate: changed=%t error=%v", outcome.changed, outcome.err)
		case <-timeout.C:
			t.Fatal("dispatch did not reach the schema-local persistence gate")
		case <-ticker.C:
		}
	}
}

func bulletinAuthorizationAwaitDispatch(t *testing.T, result <-chan bulletinAuthorizationDispatchResult) bulletinAuthorizationDispatchResult {
	t.Helper()
	select {
	case outcome := <-result:
		return outcome
	case <-time.After(5 * time.Second):
		t.Fatal("bulletin dispatch did not finish after its persistence gate was released")
		return bulletinAuthorizationDispatchResult{}
	}
}

func TestBulletinFreshAuthorizationUsesOwnerContextDuringPersistence(t *testing.T) {
	for _, refusal := range []struct {
		name   string
		status int
		code   string
	}{
		{"unauthorized", http.StatusUnauthorized, "invalid_api_key"},
		{"forbidden", http.StatusForbidden, "company_suspended"},
	} {
		t.Run(refusal.name, func(t *testing.T) {
			h, proxy, runner := newBulletinFixture(t)
			mission, data := stageBulletinFixture(t, h, runner)
			originalAction, originalIntent, originalSequence := *data.Action, *data.Bulletin, data.Sequence
			beforeSchedule := bytes.Clone(runner.snapshot["_bulletin"])
			ownerCtx, _ := bulletinAuthorizationOwner(t, h)
			admin := bulletinAuthorizationSQL(t, h)
			blockerPID, unlock := bulletinAuthorizationPersistenceGate(t, h, admin, false)
			proxy.postStatus, proxy.problemCode, proxy.problemDetail = refusal.status, refusal.code, "known bulletin authorization refusal before the child deadline"
			requestCtx, cancel := context.WithTimeout(ownerCtx, time.Second)
			defer cancel()
			result := make(chan bulletinAuthorizationDispatchResult, 1)
			go func() {
				changed, err := runner.dispatchWithOutcomeContext(requestCtx, ownerCtx, mission, &data)
				result <- bulletinAuthorizationDispatchResult{changed: changed, err: err}
			}()
			bulletinAuthorizationWaitForPersistence(t, h, admin, blockerPID, result)
			<-requestCtx.Done()
			if !errors.Is(requestCtx.Err(), context.DeadlineExceeded) || ownerCtx.Err() != nil {
				t.Fatalf("request-only deadline did not leave a live owner: request=%v owner=%v", requestCtx.Err(), ownerCtx.Err())
			}
			unlock()
			outcome := bulletinAuthorizationAwaitDispatch(t, result)
			if outcome.changed || !errors.Is(outcome.err, ErrExecutionStopped) || errors.Is(outcome.err, context.DeadlineExceeded) || errors.Is(outcome.err, context.Canceled) {
				t.Fatalf("fresh refusal kept the request context during outcome persistence: changed=%t error=%v", outcome.changed, outcome.err)
			}
			action := bulletinAuthorizationJournal(t, h, originalAction)
			if ownerCtx.Err() != nil || proxy.postCalls != 1 || action.State != store.ActionComplete || action.Status != refusal.status {
				t.Fatalf("fresh refusal lost its live owner or definitive outcome: owner=%v posts=%d journal=%+v", ownerCtx.Err(), proxy.postCalls, action)
			}
			retained, retainedData := h.mission("__bulletin")
			if retained.Phase == "done" || retained.Generation != mission.Generation || retainedData.Action != nil ||
				retainedData.LastActionID != originalAction.ID || retainedData.Sequence != originalSequence+1 ||
				retainedData.Bulletin == nil || !reflect.DeepEqual(*retainedData.Bulletin, originalIntent) {
				t.Fatalf("live owner did not durably consume the fresh refusal: mission=%+v data=%+v", retained, retainedData)
			}
			bulletinAuthorizationNoOptionalCooldown(t, h, runner, beforeSchedule, 1)
		})
	}
}

func TestBulletinStoredAuthorizationUsesOwnerContextDuringPersistence(t *testing.T) {
	for _, refusal := range []struct {
		name   string
		status int
		code   string
	}{
		{"unauthorized", http.StatusUnauthorized, "invalid_api_key"},
		{"forbidden", http.StatusForbidden, "company_suspended"},
	} {
		t.Run(refusal.name, func(t *testing.T) {
			for _, missionSave := range []bool{false, true} {
				name := "action_rejected_decision"
				if missionSave {
					name = "mission_save"
				}
				t.Run(name, func(t *testing.T) {
					h, proxy, runner := newBulletinFixture(t)
					mission, data := stageBulletinFixture(t, h, runner)
					originalAction, originalIntent, originalSequence := *data.Action, *data.Bulletin, data.Sequence
					beforeSchedule := bytes.Clone(runner.snapshot["_bulletin"])
					ownerCtx, _ := bulletinAuthorizationOwner(t, h)
					stored := bulletinAuthorizationStoredRefusal(t, h, ownerCtx, originalAction, refusal.status, refusal.code)
					admin := bulletinAuthorizationSQL(t, h)
					blockerPID, unlock := bulletinAuthorizationPersistenceGate(t, h, admin, missionSave)
					requestCtx, cancel := context.WithTimeout(ownerCtx, time.Second)
					defer cancel()
					result := make(chan bulletinAuthorizationDispatchResult, 1)
					go func() {
						changed, err := runner.dispatchWithOutcomeContext(requestCtx, ownerCtx, mission, &data)
						result <- bulletinAuthorizationDispatchResult{changed: changed, err: err}
					}()
					bulletinAuthorizationWaitForPersistence(t, h, admin, blockerPID, result)
					<-requestCtx.Done()
					if !errors.Is(requestCtx.Err(), context.DeadlineExceeded) || ownerCtx.Err() != nil {
						t.Fatalf("request-only deadline did not leave a live owner: request=%v owner=%v", requestCtx.Err(), ownerCtx.Err())
					}
					unlock()
					outcome := bulletinAuthorizationAwaitDispatch(t, result)
					if outcome.changed || !errors.Is(outcome.err, ErrExecutionStopped) || errors.Is(outcome.err, context.DeadlineExceeded) || errors.Is(outcome.err, context.Canceled) {
						t.Fatalf("stored refusal retained the expired request context during %s: changed=%t error=%v", name, outcome.changed, outcome.err)
					}
					if ownerCtx.Err() != nil || proxy.postCalls != 0 {
						t.Fatalf("stored refusal cancelled its owner or resent its request: owner=%v posts=%d", ownerCtx.Err(), proxy.postCalls)
					}
					if action := bulletinAuthorizationJournal(t, h, originalAction); !reflect.DeepEqual(action, stored) {
						t.Fatalf("stored-result consumption rewrote its immutable journal result: got %+v want %+v", action, stored)
					}
					retained, retainedData := h.mission("__bulletin")
					if retained.Phase == "done" || retained.Generation != mission.Generation || retainedData.Action != nil || retainedData.LastActionID != originalAction.ID || retainedData.Sequence != originalSequence+1 || retainedData.Bulletin == nil || !reflect.DeepEqual(*retainedData.Bulletin, originalIntent) {
						t.Fatalf("live owner did not durably consume the stored refusal: mission=%+v data=%+v", retained, retainedData)
					}
					bulletinAuthorizationNoOptionalCooldown(t, h, runner, beforeSchedule, 1)
				})
			}
		})
	}
}

func TestBulletinAuthorizationOwnerGenerationFencePreventsOutcomeWrites(t *testing.T) {
	for _, refusal := range []struct {
		name   string
		status int
		code   string
	}{
		{"unauthorized", http.StatusUnauthorized, "invalid_api_key"},
		{"forbidden", http.StatusForbidden, "company_suspended"},
	} {
		t.Run(refusal.name, func(t *testing.T) {
			h, proxy, runner := newBulletinFixture(t)
			mission, data := stageBulletinFixture(t, h, runner)
			originalMission, originalAction := *mission, *data.Action
			beforeSchedule := bytes.Clone(runner.snapshot["_bulletin"])
			beforeSnapshot, err := h.db.Snapshot(h.ctx, h.account.ID)
			if err != nil {
				t.Fatal(err)
			}
			ownerCtx, _ := bulletinAuthorizationOwner(t, h)
			admin := bulletinAuthorizationSQL(t, h)
			var generation int64
			if err := admin.QueryRowContext(h.ctx, "SELECT owner_generation FROM accounts WHERE id = $1", h.account.ID).Scan(&generation); err != nil {
				t.Fatal(err)
			}
			proxy.postStatus, proxy.problemCode, proxy.problemDetail = refusal.status, refusal.code, "authorization refused after the original account owner was fenced"
			fenced := make(chan error, 1)
			h.api.server.Config.Handler = http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
				if request.Method == originalAction.Method && request.URL.Path == originalAction.Path {
					_, err := admin.ExecContext(h.ctx, "UPDATE accounts SET owner_generation = owner_generation + 1 WHERE id = $1", h.account.ID)
					fenced <- err
					if err != nil {
						writer.WriteHeader(http.StatusInternalServerError)
						return
					}
				}
				proxy.ServeHTTP(writer, request)
			})

			changed, err := runner.driveBulletin(ownerCtx, mission, &data)
			if changed || err == nil || !errors.Is(err, store.ErrOwnershipLost) && !errors.Is(err, context.Canceled) {
				t.Fatalf("fenced owner did not stop outcome persistence: changed=%t error=%v", changed, err)
			}
			select {
			case fenceErr := <-fenced:
				if fenceErr != nil {
					t.Fatalf("advance the real PostgreSQL ownership generation: %v", fenceErr)
				}
			default:
				t.Fatal("bulletin dispatch never reached the ownership-fencing response")
			}
			var actualGeneration int64
			if err := admin.QueryRowContext(h.ctx, "SELECT owner_generation FROM accounts WHERE id = $1", h.account.ID).Scan(&actualGeneration); err != nil {
				t.Fatal(err)
			}
			if actualGeneration != generation+1 || ownerCtx.Err() == nil {
				t.Fatalf("real generation fencing did not revoke the acquired owner: generation=%d want=%d owner=%v", actualGeneration, generation+1, ownerCtx.Err())
			}
			if proxy.postCalls != 1 || !bytes.Equal(proxy.bodies[originalAction.ID], originalAction.Body) {
				t.Fatalf("ownership loss changed or resent the refused request: posts=%d bodies=%v", proxy.postCalls, proxy.bodies)
			}
			action := bulletinAuthorizationJournal(t, h, originalAction)
			if action.State != store.ActionPending || action.Status != 0 || len(action.Response) != 0 {
				t.Fatalf("stale generation wrote a definitive outcome after losing ownership: %+v", action)
			}
			bulletinAuthorizationRetainedMission(t, h, originalMission)
			bulletinAuthorizationNoOptionalCooldown(t, h, runner, beforeSchedule, 0)
			afterSnapshot, err := h.db.Snapshot(h.ctx, h.account.ID)
			if err != nil || !reflect.DeepEqual(afterSnapshot, beforeSnapshot) {
				t.Fatalf("fenced owner wrote a snapshot after the refusal: error=%v", err)
			}
		})
	}
}

func TestBulletinAuthorizationOwnerCancellationPreventsStoredOutcomeWrites(t *testing.T) {
	h, proxy, runner := newBulletinFixture(t)
	mission, data := stageBulletinFixture(t, h, runner)
	originalMission, originalAction := *mission, *data.Action
	beforeSchedule := bytes.Clone(runner.snapshot["_bulletin"])
	ownerCtx, release := bulletinAuthorizationOwner(t, h)
	stored := bulletinAuthorizationStoredRefusal(t, h, ownerCtx, originalAction, http.StatusUnauthorized, "invalid_api_key")
	beforeSnapshot, err := h.db.Snapshot(h.ctx, h.account.ID)
	if err != nil {
		t.Fatal(err)
	}
	admin := bulletinAuthorizationSQL(t, h)
	blockerPID, unlock := bulletinAuthorizationPersistenceGate(t, h, admin, false)
	result := make(chan bulletinAuthorizationDispatchResult, 1)
	go func() {
		changed, err := runner.driveBulletin(ownerCtx, mission, &data)
		result <- bulletinAuthorizationDispatchResult{changed: changed, err: err}
	}()
	bulletinAuthorizationWaitForPersistence(t, h, admin, blockerPID, result)
	release()
	if !errors.Is(ownerCtx.Err(), context.Canceled) {
		t.Fatalf("releasing the real PostgreSQL owner did not cancel its execution context: %v", ownerCtx.Err())
	}
	unlock()
	outcome := bulletinAuthorizationAwaitDispatch(t, result)
	if outcome.changed || !errors.Is(outcome.err, context.Canceled) && !errors.Is(outcome.err, store.ErrOwnershipLost) {
		t.Fatalf("released owner continued consuming a known refusal: changed=%t error=%v", outcome.changed, outcome.err)
	}
	if proxy.postCalls != 0 {
		t.Fatalf("released owner resent a completed refusal: %d", proxy.postCalls)
	}
	if action := bulletinAuthorizationJournal(t, h, originalAction); !reflect.DeepEqual(action, stored) {
		t.Fatalf("released owner rewrote the stored refusal: got %+v want %+v", action, stored)
	}
	bulletinAuthorizationRetainedMission(t, h, originalMission)
	bulletinAuthorizationNoOptionalCooldown(t, h, runner, beforeSchedule, 0)
	afterSnapshot, err := h.db.Snapshot(h.ctx, h.account.ID)
	if err != nil || !reflect.DeepEqual(afterSnapshot, beforeSnapshot) {
		t.Fatalf("released owner wrote a snapshot after its known refusal: error=%v", err)
	}
}
