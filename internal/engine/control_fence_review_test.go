package engine

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"sync/atomic"
	"testing"
	"time"

	"github.com/DevYukine/go-tradewinds/internal/store"
	"github.com/google/uuid"
	"go.uber.org/zap"
	"go.uber.org/zap/zapcore"
)

func TestControlInvalidationCannotStageAnUnsentDebit(t *testing.T) {
	t.Parallel()
	h := newRecoveryHarness(t)
	h.api.setVaultBalance("north", 500_000)
	h.api.setVaultBalance("south", 100_000)
	h.seedLaneProofs()
	runner := h.runner(primaryTestKey)
	runner.generation = uuid.NewString()
	if err := runner.refresh(h.ctx); err != nil {
		t.Fatal(err)
	}
	if changed, err := runner.considerGrowth(h.ctx); err != nil || !changed {
		t.Fatalf("funded build control: changed %t error %v", changed, err)
	}
	mission, data := h.mission("__growth")
	payload := append(json.RawMessage(nil), data.Action.Body...)
	data.Action = nil
	if err := h.db.DeleteMission(h.ctx, h.account.ID, mission.ShipID); err != nil {
		t.Fatal(err)
	}
	body := financialStageBody{payload: payload, invalidate: func() { runner.publishedControl.Add(1) }}
	if err := runner.stage(h.ctx, mission, &data, ShipState{}, "build", "/v1/shipyard/build-ship", body); err != nil {
		t.Fatal(err)
	}
	h.missingMission("__growth")
	if len(h.pending()) != 0 || h.api.mutationCount("/v1/shipyard/build-ship") != 0 {
		t.Fatal("a control invalidation created a debit request")
	}

}

func TestStreamControlsPersistNeverSentProofAndRecoverAfterRestart(t *testing.T) {
	for _, control := range []string{"gap", "closure"} {
		t.Run(control, func(t *testing.T) {
			h := newRecoveryHarness(t)
			h.api.setVaultBalance("north", 500_000)
			h.api.setVaultBalance("south", 100_000)
			h.seedLaneProofs()
			runner := h.runner(primaryTestKey)
			stream := interceptEventStream(t, h.api, []progressFrame{{id: "connected", eventType: progressOrdinaryEvent}}, 3)
			stream.frameFor = func(int32) progressFrame { return progressFrame{} }
			closeStream := make(chan struct{})
			if control == "closure" {
				downstream := h.api.server.Config.Handler
				h.api.server.Config.Handler = http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
					if request.URL.Path != "/v1/event/sse" {
						downstream.ServeHTTP(writer, request)
						return
					}
					writer.Header().Set("Content-Type", "text/event-stream")
					writer.WriteHeader(http.StatusOK)
					writer.(http.Flusher).Flush()
					stream.connectedOnce.Do(func() { close(stream.connected) })
					select {
					case <-closeStream:
					case <-request.Context().Done():
					}
				})
			}
			var injected atomic.Bool
			var staged durableAction
			core := zapcore.NewCore(zapcore.NewJSONEncoder(zap.NewProductionEncoderConfig()), zapcore.AddSync(io.Discard), zap.DebugLevel)
			runner.log = zap.New(core, zap.Hooks(func(entry zapcore.Entry) error {
				if entry.Message != "dispatching durable action" || !injected.CompareAndSwap(false, true) {
					return nil
				}
				_, data := h.mission("__growth")
				if data.Action == nil {
					return errors.New("build lost its durable intent")
				}
				staged = *data.Action
				if control == "closure" {
					close(closeStream)
				} else if !stream.emit(h.ctx, progressFrame{id: "gap-before-first-send", eventType: "gap"}) {
					return errors.New("event stream ended before the control barrier")
				}
				deadline := time.NewTimer(10 * time.Second)
				defer deadline.Stop()
				for runner.publishedControl.Load() == staged.ControlEpoch {
					select {
					case <-deadline.C:
						return errors.New("control barrier was not published")
					case <-time.After(time.Millisecond):
					}
				}
				return nil
			}))
			running := startRunner(t, runner, h.ctx)
			stream.waitReady(t, 3)
			running.cancel()
			if err := running.wait(t, 15*time.Second); err != nil && !errors.Is(err, context.Canceled) {
				t.Fatalf("stop runner: %v", err)
			}
			stream.releaseGates()
			if !injected.Load() || h.api.mutationCount("/v1/shipyard/build-ship") != 0 || len(h.pending()) != 0 {
				t.Fatal("stream control crossed the first-send boundary")
			}
			journaled, err := h.db.BeginAction(h.ctx, &store.Action{
				ID: staged.ID, AccountID: h.account.ID, Method: staged.Method, Path: staged.Path, Body: staged.Body,
			})
			if err != nil || journaled.State != store.ActionReconciled || journaled.Status != 0 {
				t.Fatalf("never-sent proof was not committed: %+v error %v", journaled, err)
			}
			var evidence struct {
				Kind string `json:"kind"`
			}
			if err := json.Unmarshal(journaled.Response, &evidence); err != nil || evidence.Kind != neverSentControlEvent {
				t.Fatalf("wrong local no-send provenance: %+v error %v", evidence, err)
			}
			mission, data := h.mission("__growth")
			if data.Action != nil {
				t.Fatal("never-sent debit remained attached")
			}
			// Replay the crash window after proof commit but before detaching the intent.
			data.Sequence--
			data.Action = &staged
			if err := runner.saveMission(h.ctx, mission, &data); err != nil {
				t.Fatal(err)
			}
			restarted := h.runner(primaryTestKey)
			if _, err := h.tick(restarted); err != nil {
				t.Fatal(err)
			}
			_, recovered := h.mission("__growth")
			if recovered.Action != nil || recovered.BuiltShipID != "" || recovered.LastActionID == staged.ID ||
				h.api.mutationCount("/v1/shipyard/build-ship") != 0 || len(h.pending()) != 0 {
				t.Fatal("restart interpreted local no-send proof as a committed build")
			}
		})
	}

}

func TestControlVetoRetainsAmbiguousRequestUntilFreshReplay(t *testing.T) {
	t.Parallel()
	h := newRecoveryHarness(t)
	h.api.setVaultBalance("north", 500_000)
	h.api.setVaultBalance("south", 100_000)
	h.seedLaneProofs()
	runner := h.runner(primaryTestKey)
	if _, err := h.tick(runner); err != nil {
		t.Fatal(err)
	}
	mission, data := h.mission("__growth")
	original := *data.Action
	journaled, err := h.db.BeginAction(h.ctx, &store.Action{
		ID: original.ID, AccountID: h.account.ID, Method: original.Method, Path: original.Path, Body: original.Body,
	})
	if err != nil {
		t.Fatal(err)
	}
	// A pending row cannot prove whether its earlier process reached transport.
	// A control veto on this retry must therefore never create no-send proof.
	runner.publishedControl.Add(1)
	if _, err := runner.dispatch(h.ctx, mission, &data); err != nil {
		t.Fatal(err)
	}
	pending := h.pending()
	if len(pending) != 1 || pending[0].ID != original.ID ||
		!bytes.Equal(pending[0].Body, original.Body) || !pending[0].CreatedAt.Equal(journaled.CreatedAt) ||
		h.api.mutationCount("/v1/shipyard/build-ship") != 0 {
		t.Fatalf("control veto changed ambiguous action identity, payload or replay window: %+v", pending)
	}
	h.expireCooldown("__growth")
	restarted := h.runner(primaryTestKey)
	if _, err := h.tick(restarted); err != nil {
		t.Fatal(err)
	}
	calls := h.api.mutationsFor("/v1/shipyard/build-ship")
	if len(calls) != 1 || calls[0].Status != http.StatusOK ||
		calls[0].IdempotencyKey != original.ID || !bytes.Equal(calls[0].Body, original.Body) || len(h.pending()) != 0 {
		t.Fatalf("fresh restart did not complete exactly the retained request: %+v", calls)
	}

}
