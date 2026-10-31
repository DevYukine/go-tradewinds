package engine

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/DevYukine/go-tradewinds/internal/gameapi"
	"go.uber.org/zap"
	"go.uber.org/zap/zaptest/observer"
)

type underworldTestReader struct {
	paths   []string
	err     error
	payload json.RawMessage
}

func (reader *underworldTestReader) Get(_ context.Context, path string, out any) error {
	reader.paths = append(reader.paths, path)
	if reader.err != nil {
		return reader.err
	}
	if reader.payload != nil {
		*out.(*json.RawMessage) = reader.payload
		return nil
	}
	*out.(*json.RawMessage) = json.RawMessage(`{"port_id":"flushing","books":[]}`)
	return nil
}

func underworldTestState() AccountState {
	var ship ShipState
	ship.Ship.ID = "observed-ship"
	ship.Ship.Status = "berthed"
	ship.Ship.CurrentPortID = "flushing"
	return AccountState{
		Ships: []ShipState{ship},
		Catalog: Catalog{
			Ports: []Port{{ID: "flushing", IsCove: true}},
			Commodities: []Commodity{
				{ID: "grog", Category: "contraband"},
				{ID: "grog", Category: "contraband"},
				{ID: "illegal_firearms", Category: "contraband"},
				{ID: "counterfeit_papers", Category: "contraband"},
				{ID: "untaxed_tea", Category: "contraband"},
				{ID: "grain", Category: "raw"},
			},
		},
	}
}

func TestUnderworldFailedReadsConsumeVisitAndBerthChangesDoNotResetIt(t *testing.T) {
	h := newRecoveryHarness(t)
	reader := &underworldTestReader{err: &gameapi.APIError{Status: 503, Detail: "temporarily unavailable"}}
	state := underworldTestState()
	for restart := range 2 {
		runner := h.runner(primaryTestKey)
		runner.opts.UnderworldAPI = reader
		runner.meta["ships"] = DatasetMeta{Complete: true, UpdatedAt: time.Now()}
		runner.meta["catalog"] = DatasetMeta{Complete: true, UpdatedAt: time.Now()}
		for pass := range 3 {
			// Port updates and a move to the roadstead are not new arrivals.
			updated := time.Now().Add(time.Duration(pass+restart) * time.Millisecond)
			state.Ships[0].Ship.UpdatedAt = &updated
			state.Ships[0].Ship.BerthedAt = &updated
			if pass == 1 {
				state.Ships[0].Ship.Status = "docked"
			} else if pass > 1 {
				state.Ships[0].Ship.Status = "anchored_roadstead"
			}
			if err := runner.refreshUnderworld(h.ctx, state); err != nil {
				t.Fatal(err)
			}
		}
	}
	if len(reader.paths) != 5 {
		t.Fatalf("failed reads or berth changes reset the budget: %v", reader.paths)
	}
	reader.err = nil
	runner := h.runner(primaryTestKey)
	runner.opts.UnderworldAPI = reader
	runner.meta["ships"] = DatasetMeta{Complete: true, UpdatedAt: time.Now()}
	runner.meta["catalog"] = DatasetMeta{Complete: true, UpdatedAt: time.Now()}
	state.Ships[0].Ship.Status = "sailing"
	state.Ships[0].Ship.CurrentPortID = ""
	if err := runner.refreshUnderworld(h.ctx, state); err != nil {
		t.Fatal(err)
	}
	state.Ships[0].Ship.Status = "berthed"
	state.Ships[0].Ship.CurrentPortID = "flushing"
	if err := runner.refreshUnderworld(h.ctx, state); err != nil {
		t.Fatal(err)
	}
	if len(reader.paths) != 10 {
		t.Fatalf("new arrival did not restore its own allowance: %v", reader.paths)
	}
}

func TestUnderworldIgnoresStaleFleetAndHonorsCancellation(t *testing.T) {
	h := newRecoveryHarness(t)
	reader := &underworldTestReader{}
	runner := h.runner(primaryTestKey)
	runner.opts.UnderworldAPI = reader
	runner.meta["catalog"] = DatasetMeta{Complete: true, UpdatedAt: time.Now()}
	runner.meta["ships"] = DatasetMeta{Complete: true, Stale: true, UpdatedAt: time.Now()}
	state := underworldTestState()
	if err := runner.refreshUnderworld(h.ctx, state); err != nil {
		t.Fatal(err)
	}
	if len(reader.paths) != 0 {
		t.Fatalf("stale fleet spent arrival reads: %v", reader.paths)
	}
	runner.meta["ships"] = DatasetMeta{Complete: true, UpdatedAt: time.Now()}
	ctx, cancel := context.WithCancel(h.ctx)
	cancel()
	if err := runner.refreshUnderworld(ctx, state); !errors.Is(err, context.Canceled) {
		t.Fatalf("canceled observer returned %v", err)
	}
	if len(reader.paths) != 0 {
		t.Fatalf("canceled observer made a request: %v", reader.paths)
	}
}

func TestUnderworldFailuresLeaveTradingRunning(t *testing.T) {
	for _, failure := range []string{"null body", "array body", "visit database", "reservation database", "observation database"} {
		t.Run(failure, func(t *testing.T) {
			h := newRecoveryHarness(t)
			runner := h.runner(primaryTestKey)
			reader := &underworldTestReader{}
			runner.opts.UnderworldAPI = reader
			core, logs := observer.New(zap.WarnLevel)
			runner.log = zap.New(core)
			switch failure {
			case "null body":
				reader.payload = json.RawMessage(`null`)
			case "array body":
				reader.payload = json.RawMessage(`[]`)
			default:
				table := map[string]string{
					"visit database":       "underworld_visits",
					"reservation database": "underworld_read_attempts",
					"observation database": "underworld_observations",
				}[failure]
				admin, err := sql.Open("pgx", h.dsn)
				if err != nil {
					t.Fatal(err)
				}
				defer admin.Close()
				if _, err := admin.ExecContext(h.ctx, "ALTER TABLE "+table+" RENAME TO unavailable_underworld"); err != nil {
					t.Fatal(err)
				}
			}
			berthed := sailingShipRow(recoveryShipLapras, "Lapras", recoveryHomePort, recoveryFarPort)
			berthed["ship"].(map[string]any)["status"] = "berthed"
			berthed["ship"].(map[string]any)["cannons_count"] = 4
			berthed["effective_hold_capacity_tons"] = 52
			berthed["stores"] = map[string]any{"provisions_tons": 4, "sea_days_remaining": 5}
			berthed["route_legs"] = []any{}
			berthed["voyage"] = nil
			h.api.setFleet([]map[string]any{
				berthed,
				sailingShipRow(recoveryShipCharizard, "Charizard", recoveryFarPort, recoveryHomePort),
			})
			h.api.setFactorPorts(recoveryFarPort)
			for tick := range 16 {
				if _, err := h.tick(runner); err != nil {
					t.Fatalf("optional observer stopped trading at tick %d: %v", tick, err)
				}
				if h.api.mutationCount("/v1/fleet/depart") > 0 {
					break
				}
			}
			if !runner.underworldDisabled || logs.FilterMessage("underworld observation failed").Len() == 0 {
				t.Fatal("observer failure was not logged and isolated")
			}
			if h.api.mutationCount("/v1/market/place-order") == 0 || h.api.mutationCount("/v1/fleet/depart") == 0 {
				t.Fatal("trading did not buy and depart after observer failure")
			}
		})
	}
}

func TestUnderworldForbiddenDisablesOnlyMissingScope(t *testing.T) {
	for _, code := range []string{"missing_scope", "not_at_port", ""} {
		t.Run(code, func(t *testing.T) {
			h := newRecoveryHarness(t)
			reader := &underworldTestReader{err: &gameapi.APIError{Status: 403, Code: code}}
			runner := h.runner(primaryTestKey)
			runner.opts.UnderworldAPI = reader
			runner.meta["ships"] = DatasetMeta{Complete: true, UpdatedAt: time.Now()}
			runner.meta["catalog"] = DatasetMeta{Complete: true, UpdatedAt: time.Now()}
			if err := runner.refreshUnderworld(h.ctx, underworldTestState()); err != nil {
				t.Fatal(err)
			}
			want := 5
			if code == "missing_scope" {
				want = 1
			}
			if runner.underworldDisabled != (code == "missing_scope") || len(reader.paths) != want {
				t.Fatalf("code %q: disabled=%t reads=%d", code, runner.underworldDisabled, len(reader.paths))
			}
		})
	}
}

func TestUnderworldRejectedKeyDisablesAfterOneRead(t *testing.T) {
	// The live game answered with an empty code (2026-10-30), which kept the
	// observer warning on every read for 47 minutes.
	for _, code := range []string{"revoked_api_key", "expired_api_key", "invalid_api_key", "", "unauthorized"} {
		t.Run(code, func(t *testing.T) {
			h := newRecoveryHarness(t)
			reader := &underworldTestReader{err: &gameapi.APIError{Status: 401, Code: code}}
			runner := h.runner(primaryTestKey)
			runner.opts.UnderworldAPI = reader
			core, logs := observer.New(zap.WarnLevel)
			runner.log = zap.New(core)
			runner.meta["ships"] = DatasetMeta{Complete: true, UpdatedAt: time.Now()}
			runner.meta["catalog"] = DatasetMeta{Complete: true, UpdatedAt: time.Now()}
			for range 2 {
				if err := runner.refreshUnderworld(h.ctx, underworldTestState()); err != nil {
					t.Fatalf("rejected observer stopped trading: %v", err)
				}
			}
			disabled := logs.FilterMessage("underworld observer disabled").All()
			if !runner.underworldDisabled || len(reader.paths) != 1 || len(disabled) != 1 ||
				disabled[0].ContextMap()["reason"] != "observer key rejected; run underworld-key" {
				t.Fatalf("rejected key did not disable once with repair notice: reads=%d logs=%+v", len(reader.paths), disabled)
			}
		})
	}
}

func TestUnderworldFailureLogsRedactCredentialIdentifiers(t *testing.T) {
	h := newRecoveryHarness(t)
	runner := h.runner(primaryTestKey)
	runner.opts.UnderworldAPI = &underworldTestReader{err: errors.New("unavailable")}
	core, logs := observer.New(zap.WarnLevel)
	runner.log = zap.New(core)
	runner.meta["ships"] = DatasetMeta{Complete: true, UpdatedAt: time.Now()}
	runner.meta["catalog"] = DatasetMeta{Complete: true, UpdatedAt: time.Now()}
	state := underworldTestState()
	state.Ships[0].Ship.ID = primaryTestKey
	state.Ships[0].Ship.CurrentPortID = "synthetic passphrase, never used against the real game"
	state.Catalog.Ports[0].ID = state.Ships[0].Ship.CurrentPortID
	if err := runner.refreshUnderworld(h.ctx, state); err != nil {
		t.Fatal(err)
	}
	entries := logs.FilterMessage("underworld observation failed").All()
	if len(entries) != 5 {
		t.Fatalf("want five failed reads, got %d", len(entries))
	}
	for _, entry := range entries {
		fields := entry.ContextMap()
		for _, field := range []string{"ship_id", "port_id"} {
			text, ok := fields[field].(string)
			if !ok || !strings.Contains(text, "[REDACTED]") || strings.Contains(text, primaryTestKey) ||
				strings.Contains(text, state.Ships[0].Ship.CurrentPortID) {
				t.Fatalf("%s was not scrubbed: %+v", field, fields)
			}
		}
	}
}
