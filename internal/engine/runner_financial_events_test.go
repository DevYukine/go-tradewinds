package engine

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/google/uuid"
)

func TestRunRefreshesFinancialEventsAfterVaultCaptureBeforeNewSpending(t *testing.T) {
	for _, eventType := range []string{"company_upkeep_billed", "auto_remittance", progressOrdinaryEvent} {
		t.Run(eventType, func(t *testing.T) {
			h := newRecoveryHarness(t)
			h.api.setVaultBalance("north", 500_000)
			h.api.setVaultBalance("south", 100_000)
			h.seedLaneProofs()
			runner := h.runner(primaryTestKey)
			runner.generation = uuid.NewString()
			if err := runner.refresh(h.ctx); err != nil {
				t.Fatalf("refresh funded growth control: %v", err)
			}
			if changed, err := runner.considerGrowth(h.ctx); err != nil || !changed {
				t.Fatalf("funded control cannot stage growth: changed %t error %v holds %v", changed, err, runner.holding)
			}
			_, funded := growthMissionWithHoldDiagnostics(t, h, runner)
			if funded.Action == nil || funded.Action.Stage != "build" {
				t.Fatalf("funded control did not select a real build: %+v", funded)
			}
			if err := h.db.DeleteMission(h.ctx, h.account.ID, "__growth"); err != nil {
				t.Fatalf("release funded control intent: %v", err)
			}

			stream := interceptEventStream(t, h.api, []progressFrame{{id: "connected", eventType: progressOrdinaryEvent}}, 2)
			stream.frameFor = func(int32) progressFrame { return progressFrame{} }
			downstream := h.api.server.Config.Handler
			var injected atomic.Bool
			vaultCaptured := make(chan struct{})
			var vaultCapturedOnce sync.Once
			h.api.server.Config.Handler = http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
				if request.Method == http.MethodGet && request.URL.Path == "/v1/treasury/vaults" {
					downstream.ServeHTTP(writer, request)
					if flusher, ok := writer.(http.Flusher); ok {
						flusher.Flush()
					}
					vaultCapturedOnce.Do(func() { close(vaultCaptured) })
					return
				}
				// Hold this sibling until the old vault response is produced, then
				// flush the event before releasing the capture. Either GET may
				// arrive first; no endpoint execution order is assumed.
				if request.Method == http.MethodGet && request.URL.Path == "/v1/treasury/rates" &&
					injected.CompareAndSwap(false, true) {
					select {
					case <-vaultCaptured:
					case <-request.Context().Done():
						return
					}
					select {
					case <-stream.connected:
					case <-request.Context().Done():
						return
					}
					payload := map[string]any{"port_id": recoveryHomePort}
					h.api.mu.Lock()
					switch eventType {
					case "company_upkeep_billed":
						h.api.vaultBalance["north"] -= 425_000
						payload = map[string]any{"banking_region": "north", "amount_pence": 425_000}
					case "auto_remittance":
						h.api.vaultBalance["north"] -= 425_000
						h.api.vaultBalance["south"] += 420_750
						payload = map[string]any{"from_region": "north", "to_region": "south", "gross_amount_pence": 425_000, "fee_pence": 4_250, "net_credited_pence": 420_750}
					}
					h.api.mu.Unlock()
					companyID := recoveryCompanyID
					if eventType == progressOrdinaryEvent {
						companyID = ""
					}
					frame := progressFrame{
						id: "after-vault-capture", eventType: eventType,
						data: encode(map[string]any{
							"id": "after-vault-capture", "event_type": eventType,
							"company_id": companyID, "port_id": recoveryHomePort,
							"payload": payload, "timestamp": time.Now().UTC(),
						}),
					}
					if !stream.emit(request.Context(), frame) {
						h.api.writeProblem(writer, http.StatusServiceUnavailable, "missing_event", "event stream ended before cash change notification")
						return
					}
				}
				if request.Method == http.MethodPost && request.URL.Path == "/v1/shipyard/build-ship" {
					// The shared recovery fake records builds but does not debit cash.
					// Apply the quoted cost here so progress is a real money outcome.
					body, err := io.ReadAll(request.Body)
					var build struct {
						PortID string `json:"port_id"`
					}
					if err != nil || json.Unmarshal(body, &build) != nil || build.PortID != recoveryHomePort {
						h.api.writeProblem(writer, http.StatusBadRequest, "bad_build", "invalid build port")
						return
					}
					h.api.mu.Lock()
					if h.api.vaultBalance["north"] < 60_000 {
						h.api.record(request, body, http.StatusConflict)
						h.api.mu.Unlock()
						h.api.writeProblem(writer, http.StatusConflict, "unfunded_build", "yard vault cannot pay the quoted hull cost")
						return
					}
					h.api.vaultBalance["north"] -= 60_000
					h.api.mu.Unlock()
					request.Body = io.NopCloser(bytes.NewReader(body))
				}
				downstream.ServeHTTP(writer, request)
			})
			running := startRunner(t, runner, h.ctx)
			stream.waitReady(t, 2)
			if !injected.Load() {
				t.Fatal("the capture did not cross the committed event")
			}
			cursor, err := h.db.Cursor(h.ctx, h.account.ID)
			if err != nil || cursor != "after-vault-capture" {
				t.Fatalf("mid-capture event was not consumed before the next capture: cursor %q error %v", cursor, err)
			}
			mission, err := h.db.GetMission(h.ctx, h.account.ID, "__growth")
			if err != nil {
				t.Fatalf("load growth after financial invalidation: %v", err)
			}
			if eventType == progressOrdinaryEvent {
				if mission == nil {
					t.Fatal("unrelated market notifications starved a funded growth commitment")
				}
			} else if mission != nil {
				t.Fatalf("%s admitted a new build intent using the pre-event vault balance: phase %s", eventType, mission.Phase)
			}
			if h.api.mutationCount("/v1/shipyard/build-ship") != 0 {
				t.Fatal("the first capture sent a build instead of staging or refreshing")
			}

			stream.releaseGates()
			stream.waitCapture(t, 3)
			running.cancel()
			if err := running.wait(t, 15*time.Second); err != nil && !errors.Is(err, context.Canceled) {
				t.Fatalf("stop runner: %v", err)
			}
			h.api.mu.Lock()
			north, south := h.api.vaultBalance["north"], h.api.vaultBalance["south"]
			h.api.mu.Unlock()
			calls := h.api.mutationsFor("/v1/shipyard/build-ship")
			if eventType == progressOrdinaryEvent {
				if len(calls) != 1 || calls[0].Status != http.StatusOK || north != 500_000-60_000 || south != 100_000 {
					t.Fatalf("ordinary events prevented one funded build or broke cash identity: calls %+v north %d south %d", calls, north, south)
				}
				return
			}
			h.missingMission("__growth")
			wantSouth := int64(100_000)
			if eventType == "auto_remittance" {
				wantSouth += 425_000 - 4_250
			}
			if len(calls) != 0 || north != 75_000 || south != wantSouth {
				t.Fatalf("fresh post-event finances did not block spending while preserving committed cash movement: calls %+v north %d south %d", calls, north, south)
			}
			snapshot, err := h.db.Snapshot(h.ctx, h.account.ID)
			if err != nil {
				t.Fatalf("load authoritative post-event snapshot: %v", err)
			}
			vaults := decode[struct {
				Vaults []Vault `json:"vaults"`
			}](snapshot, "vaults")
			for _, vault := range vaults.Vaults {
				if vault.BankingRegion == "north" && vault.BalancePence == 75_000 {
					return
				}
			}
			t.Fatal("the next authoritative capture did not retain the post-event yard balance")
		})
	}
}
