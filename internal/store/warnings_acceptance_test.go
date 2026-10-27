package store

import (
	"context"
	"encoding/json"
	"fmt"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/DevYukine/go-tradewinds/internal/gameapi"
)

var warningsAcceptanceAt = time.Date(2026, 10, 27, 2, 14, 0, 0, time.UTC)

func warningsAcceptanceInput() WarningInput {
	at := warningsAcceptanceAt
	metadata := make(map[string]WarningDataset)
	for _, key := range []string{"ships", "vaults", "contracts", "clock", "_engine"} {
		metadata[key] = WarningDataset{UpdatedAt: at, Complete: true}
	}
	return WarningInput{
		Engine: &WarningEngine{ObservedAt: at, Mode: "execute"}, Metadata: metadata,
		Catalog: gameapi.Catalog{Ports: []gameapi.Port{{ID: "hamburg", Name: "Hamburg"}}},
	}
}

func warningsAcceptanceShip(id string, idle time.Duration) gameapi.ShipDetail {
	at := warningsAcceptanceAt.Add(-idle)
	port := "hamburg"
	return gameapi.ShipDetail{Ship: gameapi.Ship{ID: id, Name: id, Status: "berthed", CurrentPortID: &port, BerthedAt: &at, UpdatedAt: warningsAcceptanceAt}}
}

func warningsAcceptanceFind(warnings []Warning, code string) *Warning {
	for i := range warnings {
		if warnings[i].Code == code {
			return &warnings[i]
		}
	}
	return nil
}

func warningsAcceptanceRequire(t *testing.T, input WarningInput, code string, want bool) *Warning {
	t.Helper()
	warnings := AssessWarnings(input, warningsAcceptanceAt).Warnings
	warning := warningsAcceptanceFind(warnings, code)
	if (warning != nil) != want {
		t.Fatalf("%s present=%v, want %v: %+v", code, warning != nil, want, warnings)
	}
	return warning
}

func TestAssessWarningsAcceptanceControllerAndRefreshBoundaries(t *testing.T) {
	for _, age := range []time.Duration{9*time.Minute + 59*time.Second, 10 * time.Minute} {
		t.Run("controller/"+age.String(), func(t *testing.T) {
			input := warningsAcceptanceInput()
			input.Engine.ObservedAt = warningsAcceptanceAt.Add(-age)
			warning := warningsAcceptanceRequire(t, input, "controller_stalled", age >= 10*time.Minute)
			if warning != nil && (warning.Severity != "critical" || !warning.Since.Equal(input.Engine.ObservedAt) || warning.Facts["minutes"] != 10) {
				t.Fatalf("controller threshold facts: %+v", warning)
			}
		})
		for _, dataset := range []string{"ships", "vaults", "contracts", "clock"} {
			t.Run(dataset+"/"+age.String(), func(t *testing.T) {
				input := warningsAcceptanceInput()
				input.Metadata[dataset] = WarningDataset{UpdatedAt: warningsAcceptanceAt.Add(-age), Complete: true}
				warning := warningsAcceptanceRequire(t, input, "refresh_failing", age >= 10*time.Minute)
				if warning != nil && (warning.Severity != "critical" || !warning.Since.Equal(warningsAcceptanceAt.Add(-age)) || warning.Names["datasets"] != dataset) {
					t.Fatalf("refresh threshold source and dataset: %+v", warning)
				}
			})
		}
	}
	input := warningsAcceptanceInput()
	input.Engine = nil
	if got := AssessWarnings(input, warningsAcceptanceAt).Warnings; len(got) != 0 {
		t.Fatalf("missing controller is not an observed stall: %+v", got)
	}
	input = warningsAcceptanceInput()
	input.Engine.ObservedAt = warningsAcceptanceAt.Add(-10 * time.Minute)
	input.Metadata["ships"] = WarningDataset{UpdatedAt: warningsAcceptanceAt.Add(-20 * time.Minute), Error: "failed"}
	warningsAcceptanceRequire(t, input, "controller_stalled", true)
	warningsAcceptanceRequire(t, input, "refresh_failing", false)
}

func TestAssessWarningsAcceptanceRefreshOldestAndScrubbedError(t *testing.T) {
	input := warningsAcceptanceInput()
	secret := "private-value-do-not-display"
	input.Secrets = []string{secret}
	input.Metadata["ships"] = WarningDataset{UpdatedAt: warningsAcceptanceAt.Add(-10 * time.Minute), Error: "ships " + secret}
	input.Metadata["clock"] = WarningDataset{UpdatedAt: warningsAcceptanceAt.Add(-20 * time.Minute), Error: "clock error"}
	warning := warningsAcceptanceRequire(t, input, "refresh_failing", true)
	if !warning.Since.Equal(warningsAcceptanceAt.Add(-20*time.Minute)) || warning.Names["datasets"] != "ships, clock" || !strings.Contains(warning.Reason, "ships") || strings.Contains(warning.Reason, secret) || strings.Contains(warning.Reason, "clock error") {
		t.Fatalf("refresh dataset order, oldest clock, or first scrubbed error: %+v", warning)
	}
}

func TestAssessWarningsAcceptanceIdleAndHoldBoundaries(t *testing.T) {
	for _, threshold := range []struct {
		code  string
		after time.Duration
	}{{"fleet_idle", 30 * time.Minute}, {"hold_stuck", 90 * time.Minute}} {
		for _, delta := range []time.Duration{-time.Second, 0} {
			t.Run(threshold.code+"/"+(threshold.after+delta).String(), func(t *testing.T) {
				input := warningsAcceptanceInput()
				input.Ships = []gameapi.ShipDetail{warningsAcceptanceShip("Lapras", threshold.after+delta)}
				input.Engine.Holds = []WarningHold{{ShipID: "Lapras", Reason: "waiting for cargo"}}
				warning := warningsAcceptanceRequire(t, input, threshold.code, delta == 0)
				if warning != nil && (warning.Severity != "warning" || warning.Facts["minutes"] != int64(threshold.after/time.Minute) || warning.Names["ship"] != "Lapras" || warning.Names["port"] != "Hamburg" || !warning.Since.Equal(warningsAcceptanceAt.Add(-threshold.after))) {
					t.Fatalf("idle boundary lost facts or free-departure time: %+v", warning)
				}
			})
		}
	}
	for _, status := range []string{"berthed", "anchored_roadstead"} {
		t.Run(status+"/turnaround", func(t *testing.T) {
			input := warningsAcceptanceInput()
			ship := warningsAcceptanceShip("Lapras", 3*time.Hour)
			ship.Ship.Status = status
			ship.Ship.DockQueuedAt, ship.Ship.BerthedAt = ship.Ship.BerthedAt, nil
			unlock := warningsAcceptanceAt.Add(-29*time.Minute - 59*time.Second)
			ship.Ship.TurnaroundUnlocksAt = &unlock
			input.Ships = []gameapi.ShipDetail{ship}
			warningsAcceptanceRequire(t, input, "fleet_idle", false)
			unlock = unlock.Add(-time.Second)
			warning := warningsAcceptanceRequire(t, input, "fleet_idle", true)
			if !warning.Since.Equal(unlock) {
				t.Fatalf("berth duration includes mandatory turnaround: %+v", warning)
			}
		})
	}
}

func TestAssessWarningsAcceptanceIdlePopulationAndUnknown(t *testing.T) {
	input := warningsAcceptanceInput()
	idle := warningsAcceptanceShip("Lapras", 2*time.Hour)
	sailing := warningsAcceptanceShip("Sailing", 3*time.Hour)
	sailing.Ship.Status = "sailing"
	sailing.Voyage = &gameapi.VoyageEstimate{}
	unknown := warningsAcceptanceShip("Unknown", 3*time.Hour)
	unknown.Ship.BerthedAt = nil
	sunk := warningsAcceptanceShip("Sunk", 3*time.Hour)
	sunk.Ship.Status = "sunk"
	input.Ships = []gameapi.ShipDetail{idle, sailing, unknown, sunk}
	warning := warningsAcceptanceRequire(t, input, "fleet_idle", true)
	if warning.Facts["idle"] != 1 || warning.Facts["total"] != 2 || warning.Facts["unknown"] != 1 {
		t.Fatalf("half-known denominator includes unknown or sunk ships: %+v", warning)
	}
	input.Ships = []gameapi.ShipDetail{unknown}
	warningsAcceptanceRequire(t, input, "fleet_idle", false)
	input.Ships = nil
	warningsAcceptanceRequire(t, input, "fleet_idle", false)
	input.Ships = []gameapi.ShipDetail{idle, sailing, sailing}
	warningsAcceptanceRequire(t, input, "fleet_idle", false)
	idle.Voyage = &gameapi.VoyageEstimate{}
	input.Ships = []gameapi.ShipDetail{idle}
	warningsAcceptanceRequire(t, input, "fleet_idle", false)
}

func TestAssessWarningsAcceptanceCurrentShipHoldsOnly(t *testing.T) {
	input := warningsAcceptanceInput()
	input.Ships = []gameapi.ShipDetail{warningsAcceptanceShip("Lapras", 2*time.Hour), warningsAcceptanceShip("Pikachu", 100*time.Minute)}
	input.Engine.Holds = []WarningHold{{Reason: "account-wide pause"}, {ShipID: "missing", Reason: "another ship"}}
	warningsAcceptanceRequire(t, input, "hold_stuck", false)
	input.Secrets = []string{"secret-reason"}
	input.Engine.Holds = append(input.Engine.Holds, WarningHold{ShipID: "Lapras", Reason: "cargo secret-reason"}, WarningHold{ShipID: "Pikachu", Reason: "stores"})
	warning := warningsAcceptanceRequire(t, input, "hold_stuck", true)
	if warning.Names["ship"] != "Lapras" || warning.Facts["other_count"] != 1 || warning.Facts["minutes"] != 120 || strings.Contains(warning.Reason, "secret-reason") || !strings.Contains(warning.Reason, "cargo") {
		t.Fatalf("longest current held ship and scrubbed reason: %+v", warning)
	}
}

func TestAssessWarningsAcceptanceIdleSuppression(t *testing.T) {
	for _, test := range []struct {
		name   string
		change func(*WarningInput)
	}{
		{"ships exactly two minutes old", func(in *WarningInput) {
			in.Metadata["ships"] = WarningDataset{UpdatedAt: warningsAcceptanceAt.Add(-2 * time.Minute), Complete: true}
		}},
		{"ships incomplete", func(in *WarningInput) { in.Metadata["ships"] = WarningDataset{UpdatedAt: warningsAcceptanceAt} }},
		{"ships error", func(in *WarningInput) {
			in.Metadata["ships"] = WarningDataset{UpdatedAt: warningsAcceptanceAt, Complete: true, Error: "failed"}
		}},
		{"ships stale", func(in *WarningInput) {
			in.Metadata["ships"] = WarningDataset{UpdatedAt: warningsAcceptanceAt, Complete: true, Stale: true}
		}},
		{"controller metadata old", func(in *WarningInput) {
			in.Metadata["_engine"] = WarningDataset{UpdatedAt: warningsAcceptanceAt.Add(-2 * time.Minute), Complete: true}
		}},
		{"controller metadata incomplete", func(in *WarningInput) { in.Metadata["_engine"] = WarningDataset{UpdatedAt: warningsAcceptanceAt} }},
		{"controller metadata error", func(in *WarningInput) {
			in.Metadata["_engine"] = WarningDataset{UpdatedAt: warningsAcceptanceAt, Complete: true, Error: "failed"}
		}},
		{"controller metadata stale", func(in *WarningInput) {
			in.Metadata["_engine"] = WarningDataset{UpdatedAt: warningsAcceptanceAt, Complete: true, Stale: true}
		}},
		{"controller exactly two minutes old", func(in *WarningInput) { in.Engine.ObservedAt = warningsAcceptanceAt.Add(-2 * time.Minute) }},
		{"controller missing", func(in *WarningInput) { in.Engine = nil }},
		{"paused", func(in *WarningInput) { in.Clock.Paused = true }},
		{"observe", func(in *WarningInput) { in.Engine.Mode = "observe" }},
	} {
		t.Run(test.name, func(t *testing.T) {
			input := warningsAcceptanceInput()
			input.Ships = []gameapi.ShipDetail{warningsAcceptanceShip("Lapras", 2*time.Hour)}
			input.Engine.Holds = []WarningHold{{ShipID: "Lapras", Reason: "cargo"}}
			test.change(&input)
			warningsAcceptanceRequire(t, input, "fleet_idle", false)
			warningsAcceptanceRequire(t, input, "hold_stuck", false)
			suppressed := AssessWarnings(input, warningsAcceptanceAt).Suppressed
			if !warningsAcceptanceContains(suppressed, "fleet_idle") || !warningsAcceptanceContains(suppressed, "hold_stuck") {
				t.Fatalf("unevaluable fleet conditions must be explicitly suppressed, not cleared: %v", suppressed)
			}
		})
	}
	input := warningsAcceptanceInput()
	input.Metadata["ships"] = WarningDataset{UpdatedAt: warningsAcceptanceAt.Add(-119 * time.Second), Complete: true}
	input.Engine.ObservedAt = warningsAcceptanceAt.Add(-119 * time.Second)
	input.Ships = []gameapi.ShipDetail{warningsAcceptanceShip("Lapras", 2*time.Hour)}
	input.Engine.Holds = []WarningHold{{ShipID: "Lapras", Reason: "cargo"}}
	warningsAcceptanceRequire(t, input, "fleet_idle", true)
	warningsAcceptanceRequire(t, input, "hold_stuck", true)
	if suppressed := AssessWarnings(input, warningsAcceptanceAt).Suppressed; warningsAcceptanceContains(suppressed, "fleet_idle") || warningsAcceptanceContains(suppressed, "hold_stuck") {
		t.Fatalf("current fleet conditions are evaluable: %v", suppressed)
	}
}

func TestAssessWarningsAcceptanceShipDetailsAndArrivalFallback(t *testing.T) {
	input := warningsAcceptanceInput()
	ship := warningsAcceptanceShip("Lapras", 2*time.Hour)
	ship.Ship.BerthedAt = nil
	ship.Ship.UpdatedAt = warningsAcceptanceAt.Add(-time.Minute)
	input.Ships = []gameapi.ShipDetail{ship}
	input.Metadata["ships"] = WarningDataset{UpdatedAt: ship.Ship.UpdatedAt, Complete: true}
	detail := warningsAcceptanceShip("Lapras", time.Hour)
	input.ShipDetails = map[string]gameapi.ShipDetail{"Lapras": detail}
	input.Metadata["ship_details/Lapras"] = WarningDataset{UpdatedAt: warningsAcceptanceAt, Complete: true}
	warningsAcceptanceRequire(t, input, "fleet_idle", true)
	detail.Ship.UpdatedAt = ship.Ship.UpdatedAt.Add(-time.Minute)
	input.ShipDetails["Lapras"] = detail
	input.Metadata["ship_details/Lapras"] = WarningDataset{UpdatedAt: ship.Ship.UpdatedAt.Add(-time.Minute), Complete: true}
	warningsAcceptanceRequire(t, input, "fleet_idle", false)
	input.ShipDetails = nil
	arrived := warningsAcceptanceAt.Add(-45 * time.Minute)
	input.ShipEvents = map[string]WarningShipEvents{"Lapras": {ArrivedAt: arrived, DepartedAt: arrived.Add(-time.Hour)}}
	warning := warningsAcceptanceRequire(t, input, "fleet_idle", true)
	if !warning.Since.Equal(arrived) || warning.Names["since_source"] != "arrival_event" {
		t.Fatalf("recorded arrival fallback must disclose its source: %+v", warning)
	}
	input.ShipEvents["Lapras"] = WarningShipEvents{ArrivedAt: arrived, DepartedAt: arrived.Add(time.Minute)}
	warningsAcceptanceRequire(t, input, "fleet_idle", false)
}

func warningsAcceptanceLosses(n int) []Settlement {
	rows := make([]Settlement, 0, n)
	for i := range n {
		rows = append(rows, Settlement{ID: fmt.Sprint(i), Kind: "spot", CostsKnown: true, ProceedsPence: 100, AcquisitionPence: 101, FeesPence: 2, ProvisionsPence: 3, ExpensesPence: 4, CompletedAt: warningsAcceptanceAt.Add(-time.Duration(i+1) * time.Hour)})
	}
	return rows
}

func TestAssessWarningsAcceptanceLossStreaks(t *testing.T) {
	for _, n := range []int{2, 3} {
		t.Run(fmt.Sprint(n), func(t *testing.T) {
			input := warningsAcceptanceInput()
			input.Settlements = warningsAcceptanceLosses(n)
			warning := warningsAcceptanceRequire(t, input, "loss_streak", n == 3)
			if warning != nil && (warning.Facts["count"] != 3 || warning.Facts["net_pence"] != -30 || !warning.Since.Equal(warningsAcceptanceAt.Add(-3*time.Hour))) {
				t.Fatalf("loss streak integer costs or oldest completion: %+v", warning)
			}
		})
	}
	for _, kind := range []string{"spot", "charter", "freight", "spot+freight", "charter+freight", "recovery", "aborted"} {
		input := warningsAcceptanceInput()
		input.Settlements = warningsAcceptanceLosses(3)
		for i := range input.Settlements {
			input.Settlements[i].Kind = kind
		}
		warningsAcceptanceRequire(t, input, "loss_streak", true)
	}
	for _, net := range []int64{0, 1} {
		input := warningsAcceptanceInput()
		input.Settlements = warningsAcceptanceLosses(4)
		input.Settlements[1].ProceedsPence = 110 + net
		warningsAcceptanceRequire(t, input, "loss_streak", false)
	}
	input := warningsAcceptanceInput()
	input.Settlements = warningsAcceptanceLosses(3)
	input.Settlements = append(input.Settlements,
		Settlement{Kind: "relocation_probe", CostsKnown: true, ProceedsPence: 9999, CompletedAt: warningsAcceptanceAt},
		Settlement{Kind: "spot", CostsKnown: false, ProceedsPence: 9999, CompletedAt: warningsAcceptanceAt.Add(-time.Minute)})
	warningsAcceptanceRequire(t, input, "loss_streak", true)
	input.Settlements = warningsAcceptanceLosses(2)
	input.Settlements = append(input.Settlements,
		Settlement{Kind: "relocation_probe", CostsKnown: true, ExpensesPence: 9999, CompletedAt: warningsAcceptanceAt},
		Settlement{Kind: "spot", CostsKnown: false, ExpensesPence: 9999, CompletedAt: warningsAcceptanceAt.Add(-time.Minute)})
	warningsAcceptanceRequire(t, input, "loss_streak", false)
}

func TestAssessWarningsAcceptanceFallingBehindAndOrdering(t *testing.T) {
	from, to := warningsAcceptanceAt.Add(-24*time.Hour), warningsAcceptanceAt
	ours, median := int64(-200), int64(-100)
	for _, test := range []struct {
		name          string
		hours, rivals int
		ours, median  int64
		age           time.Duration
		want          bool
	}{
		{"negative below median", 24, 3, -200, -100, 0, true},
		{"two rivals", 24, 2, -200, -100, 0, false},
		{"positive but slower", 24, 3, 1, 100, 0, false},
		{"equal median", 24, 3, -100, -100, 0, false},
		{"above median 0128", 24, 6, -1_246_694, -3_505_653, 0, false},
		{"six hours display only", 6, 3, -200, -100, 0, false},
		{"two hours old inclusive", 24, 3, -200, -100, 2 * time.Hour, true},
		{"too stale", 24, 3, -200, -100, 2*time.Hour + time.Second, false},
	} {
		t.Run(test.name, func(t *testing.T) {
			input := warningsAcceptanceInput()
			to := warningsAcceptanceAt.Add(-test.age)
			input.Growth = GrowthWindow{WindowHours: test.hours, From: &from, To: &to, OursPence: &test.ours, RivalMedianPence: &test.median, RivalsMeasured: test.rivals}
			warning := warningsAcceptanceRequire(t, input, "falling_behind", test.want)
			if warning != nil && (warning.Facts["ours_pence"] != test.ours || warning.Facts["median_pence"] != test.median || warning.Facts["rival_count"] != int64(test.rivals) || !warning.Since.Equal(from)) {
				t.Fatalf("growth facts or window start lost: %+v", warning)
			}
		})
	}
	input := warningsAcceptanceInput()
	input.Metadata["vaults"] = WarningDataset{UpdatedAt: warningsAcceptanceAt.Add(-10 * time.Minute), Complete: true}
	input.Ships = []gameapi.ShipDetail{warningsAcceptanceShip("Lapras", 2*time.Hour)}
	input.Engine.Holds = []WarningHold{{ShipID: "Lapras", Reason: "cargo"}}
	input.Settlements = warningsAcceptanceLosses(3)
	input.Growth = GrowthWindow{WindowHours: 24, From: &from, To: &to, OursPence: &ours, RivalMedianPence: &median, RivalsMeasured: 3}
	var codes []string
	for _, warning := range AssessWarnings(input, warningsAcceptanceAt).Warnings {
		codes = append(codes, warning.Code)
	}
	if want := []string{"refresh_failing", "falling_behind", "fleet_idle", "hold_stuck", "loss_streak"}; !reflect.DeepEqual(codes, want) {
		t.Fatalf("critical first then code order: %v, want %v", codes, want)
	}
}

func TestWarningsAcceptanceSkipsDisabledAndUnregisteredAccounts(t *testing.T) {
	database, _, _ := integrationStore(t)
	ctx := context.Background()
	for _, id := range []string{"enabled", "disabled", "unregistered"} {
		saveTestAccount(t, database, id)
		if err := database.SaveSnapshot(ctx, id, map[string]json.RawMessage{"_engine": json.RawMessage(`{"observed_at":"2026-10-27T01:00:00Z","mode":"execute"}`)}); err != nil {
			t.Fatal(err)
		}
	}
	if err := database.db.Table("accounts").Where("id = ?", "disabled").Update("enabled", false).Error; err != nil {
		t.Fatal(err)
	}
	if err := database.db.Table("accounts").Where("id = ?", "unregistered").Update("company_id", "").Error; err != nil {
		t.Fatal(err)
	}
	accounts, err := database.Warnings(ctx, warningsAcceptanceAt)
	if err != nil {
		t.Fatal(err)
	}
	if len(accounts) != 1 || accounts[0].AccountID != "enabled" || !accounts[0].Available || accounts[0].ErrorCode != "" || len(accounts[0].Warnings) != 1 || accounts[0].Warnings[0].Code != "controller_stalled" {
		t.Fatalf("disabled/unregistered accounts leaked warnings or registered account was omitted: %+v", accounts)
	}
}

func warningsAcceptanceContains(codes []string, want string) bool {
	for _, code := range codes {
		if code == want {
			return true
		}
	}
	return false
}

func TestWarningsAcceptanceAccountFailuresRemainScopedAndSafe(t *testing.T) {
	database, _, _ := integrationStore(t)
	ctx := context.Background()
	for _, id := range []string{"healthy", "credentials", "decode-one", "decode-two", "oversized"} {
		saveTestAccount(t, database, id)
		if err := database.SaveSnapshot(ctx, id, map[string]json.RawMessage{
			"_engine": json.RawMessage(`{"observed_at":"2026-10-27T01:00:00Z","mode":"execute"}`),
		}); err != nil {
			t.Fatal(err)
		}
	}
	if err := database.db.Model(&accountSecret{}).Where("account_id = ?", "credentials").Update("ciphertext", []byte("corrupt")).Error; err != nil {
		t.Fatal(err)
	}
	for _, id := range []string{"decode-one", "decode-two"} {
		if err := database.SaveSnapshot(ctx, id, map[string]json.RawMessage{
			"ships": json.RawMessage(`"private-` + id + `-snapshot-content"`),
		}); err != nil {
			t.Fatal(err)
		}
	}
	if err := database.db.Exec("UPDATE snapshots SET data = jsonb_build_object('padding', repeat('x', ?)) WHERE account_id = ?", 2*MaxSnapshotBytes+1, "oversized").Error; err != nil {
		t.Fatal(err)
	}
	accounts, err := database.Warnings(ctx, warningsAcceptanceAt)
	if err != nil {
		t.Fatalf("one account's unavailable data erased other accounts: %v", err)
	}
	if len(accounts) != 5 {
		t.Fatalf("unavailable accounts disappeared from assessed scope: %+v", accounts)
	}
	byID := make(map[string]AccountWarnings)
	for _, account := range accounts {
		byID[account.AccountID] = account
		if account.AccountID == "healthy" {
			if !account.Available || account.ErrorCode != "" || len(account.Warnings) != 1 || account.Warnings[0].Code != "controller_stalled" {
				t.Fatalf("healthy account warning lost: %+v", account)
			}
		} else if account.Available || account.ErrorCode == "" || account.Warnings == nil || len(account.Warnings) != 0 {
			t.Fatalf("unavailable account must report explicit status, not an empty all-clear: %+v", account)
		}
	}
	if byID["decode-one"].ErrorCode != byID["decode-two"].ErrorCode {
		t.Fatalf("decode errors must use a constant safe code, not account-specific payload text: %+v", byID)
	}
	for id, code := range map[string]string{"credentials": "credentials_unavailable", "decode-one": "snapshot_invalid", "decode-two": "snapshot_invalid", "oversized": "snapshot_too_large"} {
		if byID[id].ErrorCode != code || byID[id].Suppressed == nil {
			t.Fatalf("%s unavailable status=%+v, want safe code %q and explicit suppression array", id, byID[id], code)
		}
	}
	raw, err := json.Marshal(accounts)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(raw), "private-") || strings.Contains(string(raw), "cannot unmarshal") || strings.Contains(string(raw), "ciphertext") {
		t.Fatalf("unavailable status leaked failure details: %s", raw)
	}
}

func TestAssessWarningsAcceptanceSuppressedIsNotAssessedClear(t *testing.T) {
	for _, test := range []struct {
		name   string
		codes  []string
		change func(*WarningInput)
	}{
		{"missing controller", []string{"controller_stalled", "refresh_failing"}, func(in *WarningInput) { in.Engine = nil }},
		{"stalled controller", []string{"refresh_failing"}, func(in *WarningInput) { in.Engine.ObservedAt = warningsAcceptanceAt.Add(-10 * time.Minute) }},
		{"missing dataset", []string{"refresh_failing"}, func(in *WarningInput) { delete(in.Metadata, "vaults") }},
		{"unknown fleet", []string{"fleet_idle", "hold_stuck"}, func(in *WarningInput) {
			ship := warningsAcceptanceShip("Unknown", time.Hour)
			ship.Ship.BerthedAt = nil
			in.Ships = []gameapi.ShipDetail{ship}
		}},
		{"missing own valuation", []string{"falling_behind"}, func(in *WarningInput) { in.Growth.OursPence = nil }},
		{"missing rival median", []string{"falling_behind"}, func(in *WarningInput) { in.Growth.RivalMedianPence = nil }},
		{"insufficient rivals", []string{"falling_behind"}, func(in *WarningInput) { in.Growth.RivalsMeasured = 2 }},
		{"stale growth", []string{"falling_behind"}, func(in *WarningInput) {
			old := warningsAcceptanceAt.Add(-2*time.Hour - time.Second)
			in.Growth.To = &old
		}},
	} {
		t.Run(test.name, func(t *testing.T) {
			input := warningsAcceptanceInput()
			from, to := warningsAcceptanceAt.Add(-24*time.Hour), warningsAcceptanceAt
			ours, median := int64(-1), int64(0)
			input.Growth = GrowthWindow{WindowHours: 24, From: &from, To: &to, OursPence: &ours, RivalMedianPence: &median, RivalsMeasured: 3}
			test.change(&input)
			assessment := AssessWarnings(input, warningsAcceptanceAt)
			for _, code := range test.codes {
				if !warningsAcceptanceContains(assessment.Suppressed, code) || warningsAcceptanceFind(assessment.Warnings, code) != nil {
					t.Fatalf("%s is unevaluable, not cleared or active: %+v", code, assessment)
				}
			}
		})
	}
	input := warningsAcceptanceInput()
	input.Settlements = warningsAcceptanceLosses(2)
	if assessment := AssessWarnings(input, warningsAcceptanceAt); warningsAcceptanceContains(assessment.Suppressed, "loss_streak") || warningsAcceptanceFind(assessment.Warnings, "loss_streak") != nil {
		t.Fatalf("two known losses are an assessed non-streak, not unknown: %+v", assessment)
	}
}
