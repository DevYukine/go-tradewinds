package engine

import (
	"bytes"
	"encoding/json"
	"net/http"
	"testing"

	"github.com/DevYukine/go-tradewinds/internal/store"
)

func TestJournaledBodySurvivesMissionCooldownAndRestart(t *testing.T) {
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
	if data.Action == nil {
		t.Fatal("funded growth did not stage an action")
	}

	// Legacy JSONB recovery documents may contain whitespace absent from a
	// fresh marshal. Once journaled, even semantically equal rewrites conflict.
	var indented bytes.Buffer
	if err := json.Indent(&indented, data.Action.Body, "", "  "); err != nil {
		t.Fatal(err)
	}
	data.Action.Body = indented.Bytes()
	original := *data.Action
	journaled, err := h.db.BeginAction(h.ctx, &store.Action{
		ID: original.ID, AccountID: h.account.ID, Method: original.Method,
		Path: original.Path, Body: original.Body,
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := runner.saveMission(h.ctx, mission, &data); err != nil {
		t.Fatal(err)
	}
	h.expireCooldown("__growth")
	_, reloaded := h.mission("__growth")
	if reloaded.Action == nil || reloaded.Action.ID != original.ID ||
		!bytes.Equal(reloaded.Action.Body, original.Body) {
		t.Fatal("mission save changed immutable pending request bytes")
	}
	pending := h.pending()
	if len(pending) != 1 || !pending[0].CreatedAt.Equal(journaled.CreatedAt) {
		t.Fatal("mission save reset the original replay window")
	}

	restarted := h.runner(primaryTestKey)
	if _, err := h.tick(restarted); err != nil {
		t.Fatal(err)
	}
	calls := h.api.mutationsFor(original.Path)
	if len(calls) != 1 || calls[0].Status != http.StatusOK ||
		calls[0].IdempotencyKey != original.ID || !bytes.Equal(calls[0].Body, original.Body) || len(h.pending()) != 0 {
		t.Fatalf("restart did not complete the exact journaled request: %+v", calls)
	}
}
