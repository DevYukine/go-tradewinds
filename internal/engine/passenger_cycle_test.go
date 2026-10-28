package engine

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/DevYukine/go-tradewinds/internal/gameapi"
	"github.com/DevYukine/go-tradewinds/internal/store"
)

func TestPassengerOfferReconciliationBoundsRetainAction(t *testing.T) {
	for _, mode := range []string{"pages", "bytes", "rows", "context"} {
		t.Run(mode, func(t *testing.T) {
			h, r, in := passengerRunnerFixture(t)
			if changed, err := r.considerPassage(h.ctx, in.Ship, in, nil); err != nil || !changed {
				t.Fatalf("admit: %t %v", changed, err)
			}
			passengerTick(t, h, r)
			mission, data := h.mission(in.Ship.Ship.ID)
			action := data.Action
			if _, err := r.db.BeginAction(h.ctx, &store.Action{ID: action.ID, AccountID: h.account.ID, Method: action.Method, Path: action.Path, Body: action.Body}); err != nil {
				t.Fatal(err)
			}
			pages, posts := 0, 0
			h.api.server.Config.Handler = http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
				if req.Method != http.MethodGet {
					posts++
					w.WriteHeader(http.StatusServiceUnavailable)
					return
				}
				pages++
				count := 200
				if mode == "rows" {
					count = 201
				}
				page := make([]gameapi.PassengerOffer, count)
				for i := range page {
					page[i] = gameapi.PassengerOffer{ID: fmt.Sprintf("page-%d-row-%d", pages, i), ShipID: in.Ship.Ship.ID}
				}
				if mode == "bytes" {
					page[0].PortID = strings.Repeat("x", passengerOfferMaxBytes/2)
				}
				_ = json.NewEncoder(w).Encode(page)
			})
			ctx := h.ctx
			if mode == "context" {
				var cancel context.CancelFunc
				ctx, cancel = context.WithCancel(ctx)
				cancel()
			}
			// A failed bounded observation must never authorize replay or release.
			_, _ = r.dispatch(ctx, mission, &data)
			if data.Action == nil || data.Action.ID != action.ID || data.Passage.RetryOffer || posts != 0 || len(h.pending()) != 1 {
				t.Fatalf("bound released/replayed unresolved action: pages%d posts%d data%+v", pages, posts, data)
			}
			if mode == "pages" && pages != passengerOfferMaxPages {
				t.Fatalf("unbounded pages: %d", pages)
			}
			if mode == "bytes" && pages > 2 {
				t.Fatalf("unbounded aggregate bytes: %d", pages)
			}
			if mode == "rows" && pages != 1 {
				t.Fatalf("oversized page was followed: %d", pages)
			}
		})
	}
}

func TestPassengerBerthedLadderMovesOffshoreBeforeOffering(t *testing.T) {
	h, r, in := passengerRunnerFixture(t)
	in.Ship.Ship.Status = "berthed"
	r.stateCache.Ships[0] = in.Ship
	if changed, err := r.considerPassage(h.ctx, in.Ship, in, nil); err != nil || !changed {
		t.Fatalf("admit: %t %v", changed, err)
	}
	mission, data := h.mission(in.Ship.Ship.ID)
	if changed, err := r.driveMission(h.ctx, in.Ship, mission, &data); err != nil || !changed || data.Action == nil || data.Action.Stage != "offshore" {
		t.Fatalf("fare ladder incurred unpriced origin berth dues: %t %v %+v", changed, err, data.Action)
	}
}

func TestPassengerLateBoardedGroupStagesSafeDeparture(t *testing.T) {
	h, r, in := passengerRunnerFixture(t)
	ship := in.Ship
	ship.PassengerHoldTons = 10
	ship.CargoUsedTons = 10
	r.stateCache.Ships[0] = ship
	boarded, promised := in.Now.Add(-time.Minute), in.Now.Add(-time.Second)
	group := gameapi.PassengerAboardGroup{ID: "late", Kind: "travellers", Size: 10, HoldTons: 10, OriginPortID: "origin", DestPortID: "destination", FarePerHeadPence: 100, FullFarePence: 1000, BoardedAt: &boarded, PromisedArrivalAt: &promised}
	r.snapshot["passenger_aboard"] = encode(map[string]any{ship.Ship.ID: []gameapi.PassengerAboardGroup{group}})
	mission, data := r.newVoyage(ship, Plan{Kind: "passage", ShipID: ship.Ship.ID, Origin: "origin", Destination: "destination"})
	data.StartedAt = boarded
	data.Passage = &passageIntent{Groups: []gameapi.PassengerAboardGroup{group}, FirstBoardedAt: boarded}
	// First sync persists measured boarding facts; subsequent work must sail, not
	// repeatedly reject the already impossible promise.
	for range 3 {
		if _, err := r.driveMission(h.ctx, ship, mission, &data); err != nil {
			t.Fatal(err)
		}
		if data.Action != nil {
			break
		}
	}
	if data.Action == nil || data.Action.Stage != "depart" {
		t.Fatalf("late passengers remain stranded: %+v", data.Action)
	}
}

func TestPassengerShortLadderQuotesFloorImmediately(t *testing.T) {
	in := passengerInputFixture()
	group := gameapi.PassengerWaitingGroup{ID: "short", Kind: "travellers", Size: 10, OriginPortID: "origin", DestPortID: "destination", BoardBy: in.Now.Add(gameDuration(2.5/24, in.Clock.Calendar.TimeScale))}
	quote, err := quotePassage(in, "destination", []gameapi.PassengerWaitingGroup{group}, nil, nil)
	if err != nil || quote.Ceiling != quote.Floor {
		t.Fatalf("short deadline leaves no full floor hour: %+v %v", quote, err)
	}
}

func TestPassengerUnknownKindCannotBecomeRefusalEvidence(t *testing.T) {
	h, r, in := passengerRunnerFixture(t)
	opened := in.Now.Add(-time.Minute)
	group := gameapi.PassengerWaitingGroup{ID: "unknown", Kind: "crew", Size: 10, OriginPortID: "origin", DestPortID: "destination", BoardBy: in.Now.Add(time.Hour)}
	r.snapshot["passenger_waiting"] = encode(map[string]any{"origin": []gameapi.PassengerWaitingGroup{group}})
	meta := r.meta["passenger_waiting/origin"]
	meta.UpdatedAt = in.Now
	r.meta["passenger_fares/origin|destination"] = meta
	data := missionData{Plan: Plan{Origin: "origin", Destination: "destination"}, Passage: &passageIntent{OfferID: "offer", MaxBerths: 10, FarePence: 100, StepOpenedAt: opened, StepGroups: []gameapi.PassengerWaitingGroup{group}}}
	if err := r.recordPassengerRefusals(h.ctx, &data); err != nil {
		t.Fatalf("unknown kind poisoned evidence insertion: %v", err)
	}
	rows, err := r.db.PassengerEvidenceHistory(h.ctx, h.account.ID, "origin|destination", 100)
	if err != nil || len(rows) != 0 {
		t.Fatalf("unknown kind was measured as refused: %+v %v", rows, err)
	}
}

func TestPassengerAmbiguousOfferRecoversBySameKeyAfterRestart(t *testing.T) {
	h, r, in := passengerRunnerFixture(t)
	if changed, err := r.considerPassage(h.ctx, in.Ship, in, nil); err != nil || !changed {
		t.Fatalf("admit: %t %v", changed, err)
	}
	passengerTick(t, h, r)
	mission, data := h.mission(in.Ship.Ship.ID)
	action := data.Action
	if _, err := r.db.BeginAction(h.ctx, &store.Action{ID: action.ID, AccountID: h.account.ID, Method: action.Method, Path: action.Path, Body: action.Body}); err != nil {
		t.Fatal(err)
	}
	posts := 0
	success := false
	h.api.server.Config.Handler = http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		if req.Method == http.MethodGet {
			_, _ = w.Write([]byte("[]"))
			return
		}
		posts++
		body, err := io.ReadAll(req.Body)
		if err != nil || string(body) != string(action.Body) || req.Header.Get("Idempotency-Key") != action.ID {
			t.Errorf("replayed mutation changed immutable request/key: %s %q %v", body, req.Header.Get("Idempotency-Key"), err)
		}
		if !success {
			w.WriteHeader(http.StatusServiceUnavailable)
			return
		}
		offer := gameapi.PassengerOffer{ID: "recovered-same-key", ShipID: in.Ship.Ship.ID, PortID: "origin", DestPortID: "destination", FarePerHeadPence: data.Passage.FarePence, MaxBerths: data.Passage.MaxBerths, Status: "open", CreatedAt: in.Now}
		w.WriteHeader(http.StatusCreated)
		_ = json.NewEncoder(w).Encode(offer)
	})
	if changed, err := r.dispatch(h.ctx, mission, &data); err != nil || changed {
		t.Fatalf("ambiguous failure: %t %v", changed, err)
	}
	mission, data = h.mission(in.Ship.Ship.ID)
	data.CooldownUntil = time.Time{}
	success = true
	if changed, err := r.dispatch(h.ctx, mission, &data); err != nil || !changed {
		t.Fatalf("same-key recovery: %t %v", changed, err)
	}
	if posts < 2 || data.Action != nil || data.Passage.OfferID != "recovered-same-key" || len(h.pending()) != 0 {
		t.Fatalf("same-key recovery failed: posts%d %+v", posts, data)
	}
}
