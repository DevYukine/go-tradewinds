package store

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"reflect"
	"strings"
	"testing"
	"time"

	"gorm.io/gorm"
)

func dashboardMissionJSON(t *testing.T, value any) json.RawMessage {
	t.Helper()
	encoded, err := json.Marshal(value)
	if err != nil {
		t.Fatal(err)
	}
	return encoded
}

func TestDashboardMissionProjectionWhitelistsAndScrubs(t *testing.T) {
	credentials := Credentials{
		Passphrase:       "opaque-private-phrase",
		APIKey:           "opaque-private-api-key",
		UnderworldAPIKey: "opaque-private-underworld-key",
	}
	updatedAt := time.Date(2026, 11, 3, 12, 0, 0, 0, time.UTC)
	const hidden = "raw-recovery-must-not-be-displayed"
	mission := Mission{
		AccountID: "owner", ShipID: "ship-" + credentials.Passphrase,
		Phase: "sailing-" + credentials.APIKey, UpdatedAt: updatedAt,
		Destination: "obsolete-metadata-destination", Commodity: "obsolete-metadata-commodity", Quantity: 99,
		Data: dashboardMissionJSON(t, map[string]any{
			"plan": map[string]any{
				"kind": "spot-" + credentials.Passphrase, "origin": "origin-" + credentials.APIKey,
				"destination": "destination-" + credentials.Passphrase, "commodity": "grain-" + credentials.APIKey,
				"quantity": int64(7), "contract_id": "contract-" + credentials.Passphrase,
				"economics": map[string]any{
					"expected_net_pence": int64(0), "basis": "basis-" + credentials.UnderworldAPIKey,
					"unknown_forecast": hidden, "request": map[string]any{"api_key": hidden},
				},
				"ship_id": hidden, "unknown_plan_field": hidden, "action": hidden,
				"idempotency_key": hidden, "credential": hidden,
			},
			"action":             map[string]any{"id": hidden, "body": hidden, "request": hidden},
			"unknown_data_field": hidden, "last_action_id": hidden,
		}),
	}
	before := append(json.RawMessage(nil), mission.Data...)
	projected, ok, err := dashboardMission(&mission, displayCredentials{
		available: true, secrets: credentialSecrets(credentials),
	})
	if err != nil || !ok {
		t.Fatalf("recorded plan was omitted: %v", err)
	}
	var got map[string]any
	if err := json.Unmarshal(dashboardMissionJSON(t, projected), &got); err != nil {
		t.Fatal(err)
	}
	want := map[string]any{
		"ship_id": "ship-[REDACTED]", "phase": "sailing-[REDACTED]", "updated_at": updatedAt.Format(time.RFC3339),
		"plan": map[string]any{
			"kind": "spot-[REDACTED]", "origin": "origin-[REDACTED]", "destination": "destination-[REDACTED]",
			"commodity": "grain-[REDACTED]", "quantity": float64(7), "contract_id": "contract-[REDACTED]",
			"economics": map[string]any{"expected_net_pence": float64(0), "basis": "basis-[REDACTED]"},
		},
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("projection did not preserve the exact sanitized whitelist: got %#v", got)
	}
	if !bytes.Equal(mission.Data, before) || mission.ShipID != "ship-"+credentials.Passphrase {
		t.Fatal("display projection changed durable recovery state")
	}
}

func TestDashboardMissionProjectionPreservesOptionalEconomics(t *testing.T) {
	zero, loss := int64(0), int64(-125)
	for _, test := range []struct {
		name      string
		economics string
		net       *int64
		basis     string
	}{
		{name: "missing"},
		{name: "null", economics: `,"economics":null`},
		{name: "empty", economics: `,"economics":{}`},
		{name: "basis_only", economics: `,"economics":{"basis":"recorded estimate"}`, basis: "recorded estimate"},
		{name: "recorded_zero", economics: `,"economics":{"expected_net_pence":0}`, net: &zero},
		{name: "recorded_loss", economics: `,"economics":{"expected_net_pence":-125,"basis":"book depth"}`, net: &loss, basis: "book depth"},
		{name: "null_forecast", economics: `,"economics":{"expected_net_pence":null}`},
	} {
		t.Run(test.name, func(t *testing.T) {
			mission := Mission{
				ShipID: "ship", Phase: "depart",
				Data: json.RawMessage(`{"plan":{"kind":"spot","origin":"london","destination":"boston","commodity":"grain","quantity":7` + test.economics + `}}`),
			}
			projected, ok, err := dashboardMission(&mission, displayCredentials{available: true})
			if err != nil || !ok {
				t.Fatalf("a plan without a forecast was treated as unavailable: %v", err)
			}
			if !reflect.DeepEqual(projected.Plan.Economics.ExpectedNetPence, test.net) || projected.Plan.Economics.Basis != test.basis {
				t.Fatalf("recorded economics presence or value changed: %+v", projected.Plan.Economics)
			}
			var fields map[string]json.RawMessage
			if err := json.Unmarshal(dashboardMissionJSON(t, projected.Plan.Economics), &fields); err != nil {
				t.Fatal(err)
			}
			if _, present := fields["expected_net_pence"]; present != (test.net != nil) {
				t.Fatal("JSON conflated an unrecorded forecast with recorded zero")
			}
			if _, present := fields["basis"]; present != (test.basis != "") {
				t.Fatal("JSON fabricated an economic basis")
			}
		})
	}
}

func TestDashboardMissionProjectionOmitsUnavailablePlans(t *testing.T) {
	for _, data := range []string{
		``, `null`, `{}`, `[]`, `{"plan":null}`, `{"plan":[]}`, `{"plan":"spot"}`, `{"plan":{}}`,
		`{"plan":{"kind":""}}`, `{"plan":{"kind":"  "}}`,
		`{"plan":{"kind":"spot","origin":"london","destination":"boston","commodity":"grain"}}`,
		`{"plan":{"kind":"spot","origin":null,"destination":"boston","commodity":"grain","quantity":7}}`,
		`{"plan":{"kind":"spot","origin":"london","destination":5,"commodity":"grain","quantity":7}}`,
		`{"plan":{"kind":"spot","origin":"london","destination":"boston","commodity":"grain","quantity":"7"}}`,
		`{"plan":{"kind":"spot","origin":"london","destination":"boston","commodity":"grain","quantity":9223372036854775808}}`,
		`{"plan":{"kind":"spot","origin":"london","destination":"boston","commodity":"grain","quantity":7,"economics":[]}}`,
		`{"plan":{"kind":"spot","origin":"london","destination":"boston","commodity":"grain","quantity":7,"economics":{"expected_net_pence":"unknown"}}}`,
	} {
		mission := Mission{ShipID: "ship", Phase: "sailing", Data: json.RawMessage(data)}
		if _, ok, _ := dashboardMission(&mission, displayCredentials{available: true}); ok {
			t.Fatalf("malformed or unrecorded plan was presented as active: %s", data)
		}
	}
	data := json.RawMessage(`{"plan":{"kind":"maintenance","origin":"london","destination":"","commodity":"","quantity":0}}`)
	mission := Mission{ShipID: "ship", Phase: "maintenance", Data: data}
	if projected, ok, err := dashboardMission(&mission, displayCredentials{available: true}); err != nil || !ok || projected.Plan.Quantity != 0 {
		t.Fatalf("explicitly recorded empty strings and zero quantity were treated as missing fields: %v", err)
	}
	if _, ok, _ := dashboardMission(&mission, displayCredentials{}); ok {
		t.Fatal("mission was displayed without available account credentials")
	}
}

func TestDashboardMissionProjectionOmitsPseudoAndTerminalMissions(t *testing.T) {
	data := json.RawMessage(`{"plan":{"kind":"spot","origin":"london","destination":"boston","commodity":"grain","quantity":7}}`)
	for _, shipID := range []string{"", "__growth", "__treasury", "__loans", "__future_control"} {
		mission := Mission{ShipID: shipID, Phase: "sailing", Data: data}
		if _, ok, err := dashboardMission(&mission, displayCredentials{available: true}); err != nil || ok {
			t.Fatalf("non-ship mission %q was decoded or projected: %v", shipID, err)
		}
	}
	for _, phase := range []string{"", "done", "settled", "maintenance_done", "growth_rejected"} {
		mission := Mission{ShipID: "ship", Phase: phase, Data: data}
		if _, ok, err := dashboardMission(&mission, displayCredentials{available: true}); err != nil || ok {
			t.Fatalf("terminal mission %q was decoded or projected: %v", phase, err)
		}
	}
}

func TestPostgresDashboardMissionsProjectsCurrentRowsAndIsolatesAccounts(t *testing.T) {
	database, _, _ := integrationStore(t)
	ctx := context.Background()
	credentials := saveTestAccount(t, database, "mission-one")
	saveTestAccount(t, database, "mission-two")
	saveTestAccount(t, database, "mission-empty")
	const hidden = "never-display-raw-action-canary"
	first := Mission{
		AccountID: "mission-one", ShipID: "shared-ship", Phase: "buy", Destination: "obsolete", Quantity: 99,
		Data: json.RawMessage(`{"plan":{"kind":"spot","origin":"london","destination":"boston","commodity":"grain","quantity":7}}`),
	}
	if err := database.PutMission(ctx, &first); err != nil {
		t.Fatal(err)
	}
	first.Phase = "sailing"
	first.Data = dashboardMissionJSON(t, map[string]any{
		"plan": map[string]any{
			"kind": "spot", "origin": "london", "destination": "havana", "commodity": "grain", "quantity": 7,
			"contract_id": "contract-one", "economics": map[string]any{
				"expected_net_pence": 0, "basis": "measured " + credentials.Passphrase, "raw_quote": hidden,
			},
			"unknown_field": hidden,
		},
		"action": map[string]any{"id": hidden, "body": hidden}, "request": hidden, "idempotency_key": hidden,
	})
	if err := database.PutMission(ctx, &first); err != nil {
		t.Fatal(err)
	}
	second := Mission{
		AccountID: "mission-two", ShipID: "shared-ship", Phase: "depart",
		Data: json.RawMessage(`{"plan":{"kind":"charter","origin":"lisbon","destination":"new_york","commodity":"timber","quantity":3}}`),
	}
	if err := database.PutMission(ctx, &second); err != nil {
		t.Fatal(err)
	}
	for _, omitted := range []Mission{
		{AccountID: "mission-one", ShipID: "__treasury", Phase: "sailing", Data: first.Data},
		{AccountID: "mission-one", ShipID: "completed-ship", Phase: "settled", Data: first.Data},
		{AccountID: "mission-one", ShipID: "recovery-only-ship", Phase: "sailing", Data: json.RawMessage(`{"action":{"id":"private"}}`)},
	} {
		if err := database.PutMission(ctx, &omitted); err != nil {
			t.Fatal(err)
		}
	}

	dashboard, err := database.Dashboard(ctx)
	if err != nil {
		t.Fatal(err)
	}
	accounts := make(map[string]DashboardAccount)
	for _, account := range dashboard.Accounts {
		accounts[account.ID] = account
	}
	one, two, empty := accounts["mission-one"], accounts["mission-two"], accounts["mission-empty"]
	if len(one.Missions) != 1 || len(two.Missions) != 1 || empty.Missions == nil || len(empty.Missions) != 0 {
		t.Fatalf("missions were lost, shared across accounts, or not a non-null empty array: %d, %d, %#v", len(one.Missions), len(two.Missions), empty.Missions)
	}
	if one.MissionsUnavailable || two.MissionsUnavailable || empty.MissionsUnavailable {
		t.Fatal("healthy or empty mission projections were marked unavailable")
	}
	projected := one.Missions[0]
	if projected.ShipID != first.ShipID || projected.Phase != first.Phase || !projected.UpdatedAt.Equal(first.UpdatedAt) ||
		projected.Plan.Kind != "spot" || projected.Plan.Origin != "london" || projected.Plan.Destination != "havana" ||
		projected.Plan.Commodity != "grain" || projected.Plan.Quantity != 7 || projected.Plan.ContractID != "contract-one" {
		t.Fatalf("dashboard did not use the current durable plan and row timestamp: %+v", projected)
	}
	if projected.Plan.Economics.ExpectedNetPence == nil || *projected.Plan.Economics.ExpectedNetPence != 0 || projected.Plan.Economics.Basis != "measured [REDACTED]" {
		t.Fatalf("projection did not retain recorded zero or scrub its account's secret: %+v", projected.Plan.Economics)
	}
	if two.Missions[0].Plan.Destination != "new_york" || two.Missions[0].Plan.Kind != "charter" || two.Missions[0].Plan.Economics.ExpectedNetPence != nil {
		t.Fatal("another account's same ship identity inherited a sibling plan or invented forecast")
	}
	encoded := dashboardMissionJSON(t, dashboard)
	for _, forbidden := range []string{hidden, credentials.APIKey, credentials.Passphrase, `"data"`, `"action"`, `"request"`, `"idempotency_key"`, `"raw_quote"`, `"unknown_field"`} {
		if bytes.Contains(encoded, []byte(forbidden)) {
			t.Fatalf("dashboard exposed excluded mission state: %q", forbidden)
		}
	}
	var accountJSON map[string]json.RawMessage
	if err := json.Unmarshal(dashboardMissionJSON(t, empty), &accountJSON); err != nil {
		t.Fatal(err)
	}
	if string(accountJSON["missions"]) != "[]" {
		t.Fatalf("an account without missions did not serialize missions as []: %s", accountJSON["missions"])
	}
	if string(accountJSON["missions_unavailable"]) != "false" {
		t.Fatal("an available empty projection omitted its explicit availability flag")
	}
	retained, err := database.GetMission(ctx, first.AccountID, first.ShipID)
	if err != nil || retained == nil || !bytes.Equal(retained.Data, first.Data) || !retained.UpdatedAt.Equal(first.UpdatedAt) {
		t.Fatalf("dashboard changed durable recovery bytes or timestamps: %v", err)
	}
}

func TestPostgresDashboardMissionsUnavailableCredentialsWithholdOnlyAffectedAccount(t *testing.T) {
	for _, mode := range []string{"corrupt", "missing", "missing_empty"} {
		t.Run(mode, func(t *testing.T) {
			database, _, _ := integrationStore(t)
			ctx := context.Background()
			saveTestAccount(t, database, "mission-healthy")
			credentials := saveTestAccount(t, database, "mission-unavailable")
			for _, accountID := range []string{"mission-healthy", "mission-unavailable"} {
				if accountID == "mission-unavailable" && mode == "missing_empty" {
					continue
				}
				mission := Mission{
					AccountID: accountID, ShipID: "ship", Phase: "sailing",
					Data: dashboardMissionJSON(t, map[string]any{"plan": map[string]any{
						"kind": "spot", "origin": "london", "destination": "boston", "commodity": "grain", "quantity": 7,
					}}),
				}
				if accountID == "mission-unavailable" {
					mission.Data = dashboardMissionJSON(t, map[string]any{"plan": map[string]any{
						"kind": credentials.Passphrase, "origin": credentials.APIKey,
						"destination": "boston", "commodity": "grain", "quantity": 7,
					}})
				}
				if err := database.PutMission(ctx, &mission); err != nil {
					t.Fatal(err)
				}
				if err := database.SaveSnapshot(ctx, accountID, map[string]json.RawMessage{"ships": json.RawMessage(`[{"id":"ship"}]`)}); err != nil {
					t.Fatal(err)
				}
			}
			if mode == "corrupt" {
				if err := database.db.Model(&accountSecret{}).Where("account_id = ?", "mission-unavailable").Update("ciphertext", []byte("corrupt")).Error; err != nil {
					t.Fatal(err)
				}
			} else if err := database.db.Where("account_id = ?", "mission-unavailable").Delete(&accountSecret{}).Error; err != nil {
				t.Fatal(err)
			}
			dashboard, err := database.Dashboard(ctx)
			if err != nil {
				t.Fatalf("unavailable credentials prevented the healthy account dashboard read: %v", err)
			}
			if len(dashboard.Accounts) != 2 {
				t.Fatal("unavailable credentials lost an account")
			}
			for _, account := range dashboard.Accounts {
				if account.ID == "mission-unavailable" {
					if !account.MissionsUnavailable || account.Missions == nil || len(account.Missions) != 0 {
						t.Fatal("unavailable credentials did not explicitly withhold typed missions")
					}
					if _, exists := account.Snapshot["ships"]; exists {
						t.Fatal("unavailable credentials left typed snapshots visible")
					}
				} else if account.MissionsUnavailable || len(account.Missions) != 1 {
					t.Fatal("unavailable sibling credentials withheld a healthy account mission")
				}
			}
			encoded := dashboardMissionJSON(t, dashboard)
			if bytes.Contains(encoded, []byte(credentials.APIKey)) || bytes.Contains(encoded, []byte(credentials.Passphrase)) {
				t.Fatal("unavailable account missions exposed an original credential")
			}
		})
	}
}

func TestPostgresDashboardMissionsDegradeOnlyAffectedAccount(t *testing.T) {
	for _, mode := range []string{
		"row_count", "ciphertext_bytes", "data_bytes", "metadata_bytes",
		"metadata_corrupt", "metadata_json", "metadata_identity",
		"data_corrupt", "data_json", "plan_type",
	} {
		t.Run(mode, func(t *testing.T) {
			database, _, _ := integrationStore(t)
			ctx := context.Background()
			const affectedID = "mission-unavailable"
			const peerID = "mission-healthy"
			const hidden = "private-recovery-state-must-not-appear"
			data := dashboardMissionJSON(t, map[string]any{
				"plan": map[string]any{
					"kind": "spot", "origin": "london", "destination": "boston", "commodity": "grain", "quantity": 7,
				},
				"action": hidden, "request": hidden,
			})
			var secrets []string
			for _, accountID := range []string{affectedID, peerID} {
				secrets = append(secrets, credentialSecrets(saveTestAccount(t, database, accountID))...)
				mission := Mission{AccountID: accountID, ShipID: "healthy", Phase: "sailing", Data: data}
				if err := database.PutMission(ctx, &mission); err != nil {
					t.Fatal(err)
				}
				if err := database.SaveSnapshot(ctx, accountID, map[string]json.RawMessage{
					"ships": json.RawMessage(`[{"id":"healthy"}]`),
				}); err != nil {
					t.Fatal(err)
				}
			}
			row, err := database.encodeMission(Mission{
				AccountID: affectedID, ShipID: "damaged", Phase: "sailing", Data: data,
			})
			if err != nil {
				t.Fatal(err)
			}
			var stored []missionRow
			switch mode {
			case "row_count":
				for i := range maxDashboardMissions {
					value, err := database.encodeMission(Mission{
						AccountID: affectedID, ShipID: fmt.Sprintf("bounded-ship-%04d", i), Phase: "sailing", Data: data,
					})
					if err != nil {
						t.Fatal(err)
					}
					stored = append(stored, value)
				}
			case "ciphertext_bytes":
				large := dashboardMissionJSON(t, map[string]any{"ignored": strings.Repeat("x", maxDashboardMissionBytes/2)})
				for i := range 2 {
					value, err := database.encodeMission(Mission{
						AccountID: affectedID, ShipID: fmt.Sprintf("oversized-ship-%d", i), Phase: "sailing", Data: large,
					})
					if err != nil {
						t.Fatal(err)
					}
					stored = append(stored, value)
				}
			default:
				switch mode {
				case "data_bytes":
					row.DataCipher = bytes.Repeat([]byte{0xff}, maxDashboardMissionBytes+1)
				case "metadata_bytes":
					row.MetadataCipher = bytes.Repeat([]byte{0xff}, maxDashboardMissionBytes+1)
				case "metadata_corrupt":
					row.MetadataCipher[len(row.MetadataCipher)-1] ^= 0xff
				case "metadata_json":
					row.MetadataCipher, err = database.encrypt([]byte(`[]`), privateAAD("missions", "metadata", affectedID, row.ShipLookup))
				case "metadata_identity":
					row.MetadataCipher, err = database.encryptPrivate(missionMetadata{
						ShipID: "another-ship", Phase: "sailing",
					}, "missions", "metadata", affectedID, row.ShipLookup)
				case "data_corrupt":
					row.DataCipher[len(row.DataCipher)-1] ^= 0xff
				case "data_json":
					row.DataCipher, err = database.encrypt([]byte(`{"plan":`), privateAAD("missions", "data", affectedID, row.ShipLookup))
				case "plan_type":
					row.DataCipher, err = database.encrypt([]byte(`{"plan":{"quantity":"seven"}}`), privateAAD("missions", "data", affectedID, row.ShipLookup))
				}
				if err != nil {
					t.Fatal(err)
				}
				stored = append(stored, row)
			}
			if err := database.db.CreateInBatches(&stored, 100).Error; err != nil {
				t.Fatal(err)
			}
			hydrated := make(map[string]int)
			if err := database.db.Callback().Query().After("gorm:query").Register("test:dashboard-mission-hydration", func(tx *gorm.DB) {
				if rows, ok := tx.Statement.Dest.(*[]missionRow); ok {
					for _, row := range *rows {
						hydrated[row.AccountID]++
					}
				}
			}); err != nil {
				t.Fatal(err)
			}
			dashboard, err := database.Dashboard(ctx)
			if err != nil {
				t.Fatalf("unavailable account missions failed the dashboard: %v", err)
			}
			if len(dashboard.Accounts) != 2 {
				t.Fatalf("unavailable projection lost accounts: %d", len(dashboard.Accounts))
			}
			for _, account := range dashboard.Accounts {
				var ships []struct {
					ID string `json:"id"`
				}
				if err := json.Unmarshal(account.Snapshot["ships"], &ships); err != nil || len(ships) != 1 || ships[0].ID != "healthy" || account.UpdatedAt == nil {
					t.Fatalf("mission failure lost account %q snapshot: %v", account.ID, err)
				}
				if account.ID == affectedID {
					if !account.MissionsUnavailable || account.Missions == nil || len(account.Missions) != 0 {
						t.Fatal("failed projection did not discard the affected account's whole plan list")
					}
					var fields map[string]json.RawMessage
					if err := json.Unmarshal(dashboardMissionJSON(t, account), &fields); err != nil {
						t.Fatal(err)
					}
					if string(fields["missions"]) != "[]" || string(fields["missions_unavailable"]) != "true" {
						t.Fatal("unavailable projection did not serialize as missions:[] and missions_unavailable:true")
					}
				} else if account.MissionsUnavailable || len(account.Missions) != 1 || account.Missions[0].Plan.Destination != "boston" {
					t.Fatal("failed projection withheld or changed the healthy peer's plan")
				}
			}
			switch mode {
			case "row_count", "ciphertext_bytes", "data_bytes", "metadata_bytes", "metadata_corrupt", "metadata_json", "metadata_identity":
				if hydrated[affectedID] != 0 {
					t.Fatal("failed metadata preflight hydrated affected recovery ciphertext")
				}
			}
			if hydrated[peerID] != 1 {
				t.Fatal("the healthy peer's eligible mission was not hydrated")
			}
			encoded := dashboardMissionJSON(t, dashboard)
			for _, forbidden := range append(secrets, hidden, `"data"`, `"action"`, `"request"`) {
				if bytes.Contains(encoded, []byte(forbidden)) {
					t.Fatalf("degraded dashboard exposed excluded state: %q", forbidden)
				}
			}
		})
	}
}

func TestPostgresDashboardMissionsBoundsArePerAccountAndInclusive(t *testing.T) {
	for _, mode := range []string{"row_count", "ciphertext_bytes"} {
		t.Run(mode, func(t *testing.T) {
			database, _, _ := integrationStore(t)
			ctx := context.Background()
			for _, accountID := range []string{"mission-bound-one", "mission-bound-two"} {
				saveTestAccount(t, database, accountID)
				payload := map[string]any{
					"plan": map[string]any{
						"kind": "spot", "origin": "london", "destination": "boston", "commodity": "grain", "quantity": 7,
					},
					"ignored": "",
				}
				count := 1
				if mode == "row_count" {
					count = maxDashboardMissions
				}
				stored := make([]missionRow, 0, count)
				for i := range count {
					mission := Mission{
						AccountID: accountID, ShipID: fmt.Sprintf("ship-%04d", i), Phase: "sailing",
						Data: dashboardMissionJSON(t, payload), UpdatedAt: time.Now().UTC(),
					}
					row, err := database.encodeMission(mission)
					if err != nil {
						t.Fatal(err)
					}
					if mode == "ciphertext_bytes" {
						padding := maxDashboardMissionBytes - len(row.MetadataCipher) - len(row.DataCipher)
						payload["ignored"] = strings.Repeat("x", padding)
						mission.Data = dashboardMissionJSON(t, payload)
						row, err = database.encodeMission(mission)
						if err != nil {
							t.Fatal(err)
						}
						if len(row.MetadataCipher)+len(row.DataCipher) != maxDashboardMissionBytes {
							t.Fatal("fixture did not reach the exact per-account ciphertext limit")
						}
					}
					stored = append(stored, row)
				}
				if err := database.db.CreateInBatches(&stored, 100).Error; err != nil {
					t.Fatal(err)
				}
			}
			dashboard, err := database.Dashboard(ctx)
			if err != nil {
				t.Fatal(err)
			}
			if len(dashboard.Accounts) != 2 {
				t.Fatal("per-account bounds lost an account")
			}
			want := 1
			if mode == "row_count" {
				want = maxDashboardMissions
			}
			for _, account := range dashboard.Accounts {
				if account.MissionsUnavailable || len(account.Missions) != want {
					t.Fatalf("account %q at its own limit inherited its peer's usage: unavailable=%v missions=%d", account.ID, account.MissionsUnavailable, len(account.Missions))
				}
			}
		})
	}
}

func TestPostgresDashboardMissionsExcludeTerminalAndPseudoRowsBeforeDataHydration(t *testing.T) {
	database, _, _ := integrationStore(t)
	ctx := context.Background()
	const accountID = "mission-excluded"
	saveTestAccount(t, database, accountID)
	saveTestAccount(t, database, "mission-peer")
	data := json.RawMessage(`{"plan":{"kind":"spot","origin":"london","destination":"boston","commodity":"grain","quantity":7}}`)
	for _, id := range []string{accountID, "mission-peer"} {
		if err := database.PutMission(ctx, &Mission{AccountID: id, ShipID: "healthy", Phase: "sailing", Data: data}); err != nil {
			t.Fatal(err)
		}
	}
	phases := []string{"", "done", "settled", "maintenance_done", "growth_rejected"}
	stored := make([]missionRow, 0, maxDashboardMissions+4)
	for i := range maxDashboardMissions + 2 {
		mission := Mission{AccountID: accountID, ShipID: fmt.Sprintf("terminal-%04d", i), Phase: phases[i%len(phases)], Data: data}
		if i%2 == 0 {
			mission.ShipID = fmt.Sprintf("__controller-%04d", i)
			mission.Phase = "sailing"
		}
		if i == 1 {
			mission.ShipID = ""
			mission.Phase = "sailing"
		}
		row, err := database.encodeMission(mission)
		if err != nil {
			t.Fatal(err)
		}
		// These rows have no usable recovery data. The projection must use
		// authenticated metadata to exclude them before reading data_cipher.
		row.DataCipher = bytes.Repeat([]byte{0xff}, 28)
		if i == 0 || i == 3 {
			row.DataCipher = bytes.Repeat([]byte{0xff}, maxDashboardMissionBytes+1)
		}
		stored = append(stored, row)
	}
	if err := database.db.CreateInBatches(&stored, 100).Error; err != nil {
		t.Fatal(err)
	}
	hydrated := make(map[string]int)
	if err := database.db.Callback().Query().After("gorm:query").Register("test:dashboard-excluded-hydration", func(tx *gorm.DB) {
		if rows, ok := tx.Statement.Dest.(*[]missionRow); ok {
			for _, row := range *rows {
				hydrated[row.AccountID]++
				if row.ShipLookup != database.privateLookup("mission-ship", row.AccountID, "healthy") {
					t.Error("discarded mission recovery ciphertext was hydrated")
				}
			}
		}
	}); err != nil {
		t.Fatal(err)
	}
	dashboard, err := database.Dashboard(ctx)
	if err != nil {
		t.Fatalf("discarded rows failed the dashboard: %v", err)
	}
	if len(dashboard.Accounts) != 2 {
		t.Fatal("discarded rows lost an account")
	}
	for _, account := range dashboard.Accounts {
		if account.MissionsUnavailable || len(account.Missions) != 1 || account.Missions[0].ShipID != "healthy" || hydrated[account.ID] != 1 {
			t.Fatalf("excluded rows consumed account %q's projection budget or became corrupt plans", account.ID)
		}
	}
}

func TestPostgresDashboardMissionsQueryFailureRemainsAnError(t *testing.T) {
	database, _, _ := integrationStore(t)
	saveTestAccount(t, database, "mission-query-error")
	if err := database.db.Exec("ALTER TABLE missions RENAME TO dashboard_unavailable_missions").Error; err != nil {
		t.Fatal(err)
	}
	dashboard, err := database.Dashboard(context.Background())
	if err == nil || !reflect.DeepEqual(dashboard, Dashboard{}) {
		t.Fatalf("transaction-aborting mission query failure became a successful partial response: %v", err)
	}
}
