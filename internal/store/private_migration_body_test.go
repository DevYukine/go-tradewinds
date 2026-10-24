package store

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"testing"
	"time"

	"go.uber.org/zap"
)

func assertMigratedMissionBody(t *testing.T, database *Store, original, migrated json.RawMessage, expectedBody []byte) {
	t.Helper()
	var oldMembers, newMembers map[string]json.RawMessage
	if err := json.Unmarshal(original, &oldMembers); err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(migrated, &newMembers); err != nil {
		t.Fatal(err)
	}
	var oldAction, newAction map[string]json.RawMessage
	if err := json.Unmarshal(oldMembers["action"], &oldAction); err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(newMembers["action"], &newAction); err != nil {
		t.Fatal(err)
	}
	var actualBody []byte
	if raw, exists := newAction["body"]; exists {
		if err := json.Unmarshal(raw, &actualBody); err != nil {
			t.Fatalf("new recovery body is not opaque request bytes: %v", err)
		}
	}
	if !bytes.Equal(actualBody, expectedBody) {
		t.Fatalf("migration changed exact attempted request bytes: got %q, want %q", actualBody, expectedBody)
	}
	delete(oldAction, "body")
	delete(newAction, "body")
	var err error
	oldMembers["action"], err = json.Marshal(oldAction)
	if err != nil {
		t.Fatal(err)
	}
	newMembers["action"], err = json.Marshal(newAction)
	if err != nil {
		t.Fatal(err)
	}
	oldJSON, err := json.Marshal(oldMembers)
	if err != nil {
		t.Fatal(err)
	}
	newJSON, err := json.Marshal(newMembers)
	if err != nil {
		t.Fatal(err)
	}
	var same bool
	if err := database.db.Raw("SELECT ?::jsonb = ?::jsonb", string(oldJSON), string(newJSON)).Scan(&same).Error; err != nil || !same {
		t.Fatalf("body-only conversion changed unknown members or numeric precision: %v", err)
	}
}

func exerciseMigratedCooldownSave(t *testing.T, database *Store, dsn string, key []byte, evidence privateLegacyEvidence, mission *Mission) {
	t.Helper()
	var recovery struct {
		UnknownRecovery json.RawMessage `json:"unknown_recovery"`
		Action          struct {
			ID             string          `json:"id"`
			Method         string          `json:"method"`
			Path           string          `json:"path"`
			Body           []byte          `json:"body"`
			Before         json.RawMessage `json:"before"`
			UnknownAttempt json.RawMessage `json:"unknown_attempt"`
		} `json:"action"`
		BuiltShipID   string    `json:"built_ship_id"`
		CooldownUntil time.Time `json:"cooldown_until"`
	}
	if err := json.Unmarshal(mission.Data, &recovery); err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(recovery.Action.Body, evidence.pending.Body) {
		t.Fatal("migration did not restore the immutable journal's request bytes")
	}
	recovery.CooldownUntil = time.Now().UTC().Add(time.Minute)
	encoded, err := json.Marshal(recovery)
	if err != nil {
		t.Fatal(err)
	}
	mission.Phase = "retry_wait"
	mission.Data = encoded
	if err := database.PutMission(context.Background(), mission); err != nil {
		t.Fatal(err)
	}
	restarted, err := Open(context.Background(), dsn, key, zap.NewNop())
	if err != nil {
		t.Fatal(err)
	}
	defer restarted.Close()
	loaded, err := restarted.GetMission(context.Background(), evidence.account, mission.ShipID)
	if err != nil || loaded == nil || loaded.Generation != evidence.mission.Generation || loaded.Phase != mission.Phase {
		t.Fatalf("cooldown/phase save lost migrated mission identity: %+v, %v", loaded, err)
	}
	if err := json.Unmarshal(loaded.Data, &recovery); err != nil {
		t.Fatal(err)
	}
	intent := &Action{
		ID: recovery.Action.ID, AccountID: evidence.account, Method: recovery.Action.Method,
		Path: recovery.Action.Path, Body: recovery.Action.Body,
	}
	persisted, err := restarted.BeginAction(context.Background(), intent)
	if err != nil || !bytes.Equal(persisted.Body, evidence.pending.Body) || !persisted.CreatedAt.Equal(evidence.pendingRow.CreatedAt) {
		t.Fatalf("normal JSON cooldown save compacted/rekeyed attempted request: %+v, %v", persisted, err)
	}
}

func TestPrivateMigrationDistinguishesRawJSONTokensFromNewOpaqueBodies(t *testing.T) {
	for _, attached := range []bool{false, true} {
		t.Run(fmt.Sprintf("attached-%t", attached), func(t *testing.T) {
			legacy, dsn, key := privateLegacyStore(t)
			const account = "legacy-body-kinds"
			saveTestAccount(t, legacy, account)
			cases := []struct{ name, raw string }{
				{name: "missing"},
				{name: "null", raw: "null"},
				{name: "looks-base64", raw: `"YWJj"`},
				{name: "array", raw: `[ 1, "two", 9007199254740993 ]`},
				{name: "number", raw: `9e3`},
				{name: "object", raw: `{ "quantity" : 4, "precise" : 9007199254740993 }`},
			}
			originals := make(map[string]Mission, len(cases))
			bodies := make(map[string][]byte, len(cases))
			for _, test := range cases {
				actionID := "body-" + test.name
				action := map[string]json.RawMessage{
					"id": json.RawMessage(fmt.Sprintf("%q", actionID)), "method": json.RawMessage(`"POST"`),
					"path": json.RawMessage(`"/v1/market/place-order"`), "future": json.RawMessage(`9007199254740993`),
				}
				if test.raw != "" {
					action["body"] = json.RawMessage(test.raw)
				}
				encodedAction, err := json.Marshal(action)
				if err != nil {
					t.Fatal(err)
				}
				mission := Mission{
					AccountID: account, ShipID: test.name, Phase: "staged", Generation: "original-" + test.name,
					Data: json.RawMessage(fmt.Sprintf(`{"future":{"amount":9007199254740993},"action":%s}`, encodedAction)),
				}
				if attached {
					if _, err := legacy.BeginAction(context.Background(), &Action{
						ID: actionID, AccountID: account, Method: "POST", Path: "/v1/market/place-order", Body: []byte(test.raw),
					}); err != nil {
						t.Fatal(err)
					}
				}
				if err := legacy.db.Create(&mission).Error; err != nil {
					t.Fatal(err)
				}
				mission.Data = legacyMissionJSON(t, legacy, account, test.name)
				originals[test.name] = mission
				if attached {
					bodies[test.name] = []byte(test.raw)
				} else {
					var stored struct {
						Action struct {
							Body json.RawMessage `json:"body"`
						} `json:"action"`
					}
					if err := json.Unmarshal(mission.Data, &stored); err != nil {
						t.Fatal(err)
					}
					bodies[test.name] = stored.Action.Body
				}
			}
			database, err := Open(context.Background(), dsn, key, zap.NewNop())
			if err != nil {
				t.Fatal(err)
			}
			defer database.Close()
			for name, original := range originals {
				loaded, err := database.GetMission(context.Background(), account, name)
				if err != nil || loaded == nil {
					t.Fatalf("load %s: %+v, %v", name, loaded, err)
				}
				assertMigratedMissionBody(t, database, original.Data, loaded.Data, bodies[name])
			}
		})
	}
}

func TestPrivateMigrationDisagreementRollsBackWithoutReplacingAttempt(t *testing.T) {
	for _, mode := range []string{"body", "precise-number", "method", "path", "foreign-account"} {
		t.Run(mode, func(t *testing.T) {
			legacy, dsn, key := privateLegacyStore(t)
			evidence := seedPrivateLegacyEvidence(t, legacy)
			path, replacement := "{action,body,quantity}", "5"
			switch mode {
			case "precise-number":
				path, replacement = "{action,body,future}", "9007199254740992"
			case "method":
				path, replacement = "{action,method}", `"DELETE"`
			case "path":
				path, replacement = "{action,path}", `"/v1/different"`
			case "foreign-account":
				const foreign = "foreign-migration-owner"
				saveTestAccount(t, legacy, foreign)
				if _, err := legacy.BeginAction(context.Background(), &Action{ID: "foreign-attempt", AccountID: foreign, Method: "POST", Path: evidence.pending.Path, Body: evidence.pending.Body}); err != nil {
					t.Fatal(err)
				}
				path, replacement = "{action,id}", `"foreign-attempt"`
			}
			if err := legacy.db.Exec("UPDATE missions SET data = jsonb_set(data, ?::text[], ?::jsonb) WHERE account_id = ? AND ship_id = ?", path, replacement, evidence.account, evidence.mission.ShipID).Error; err != nil {
				t.Fatal(err)
			}
			evidence.mission.Data = legacyMissionJSON(t, legacy, evidence.account, evidence.mission.ShipID)
			opened, err := Open(context.Background(), dsn, key, zap.NewNop())
			if err == nil {
				_ = opened.Close()
				t.Fatal("disagreeing immutable recovery evidence was silently changed")
			}
			assertLegacyRecoveryIntact(t, legacy, evidence)
		})
	}
}
