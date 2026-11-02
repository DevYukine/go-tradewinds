package engine

import (
	"encoding/json"
	"net/http"
	"testing"
	"time"

	"github.com/DevYukine/go-tradewinds/internal/gameapi"
)

func passengerCargoFixture(t *testing.T, age float64) (*recoveryHarness, *Runner, PlanningInput, gameapi.PassengerOffer) {
	t.Helper()
	h, r, in := passengerRunnerFixture(t)
	if changed, err := r.considerPassage(h.ctx, in.Ship, in, nil); err != nil || !changed {
		t.Fatalf("admit passage: %t %v", changed, err)
	}
	mission, data := h.mission(in.Ship.Ship.ID)
	opened := in.Now.Add(-gameDuration(age/24, in.Clock.Calendar.TimeScale))
	data.StartedAt = opened.Add(-time.Second)
	data.Passage.StepOpenedAt = opened
	data.Passage.StepHour = in.Clock.Calendar.TotalGameHours - int64(age)
	data.Passage.OfferID = "cargo-priority-offer"
	data.Passage.FarePence = data.Passage.FloorPence * 10
	offer := gameapi.PassengerOffer{ID: data.Passage.OfferID, ShipID: in.Ship.Ship.ID, PortID: "origin", DestPortID: "destination",
		FarePerHeadPence: data.Passage.FarePence, MaxBerths: data.Passage.MaxBerths, Status: "open", CreatedAt: opened}
	r.snapshot["passenger_offers"] = encode([]gameapi.PassengerOffer{offer})
	reward := int64(10_000)
	r.stateCache.Contracts = []Contract{{ID: "cargo", ContractType: "charter", OriginPortID: "destination", CommodityID: "provisions", RemainingQuantity: 20, RewardPerTonPence: &reward, Status: "open"}}
	if err := r.saveMission(h.ctx, mission, &data); err != nil {
		t.Fatal(err)
	}
	return h, r, in, offer
}

func passengerOfferCapture(r *Runner, offer gameapi.PassengerOffer, groups []gameapi.PassengerAboardGroup) {
	now := time.Now().UTC()
	r.snapshot["passenger_offers"] = encode([]gameapi.PassengerOffer{offer})
	r.snapshot["passenger_aboard"] = encode(map[string]any{offer.ShipID: groups})
	for _, key := range []string{"ships", "passenger_offers", "passenger_aboard/" + offer.ShipID} {
		r.meta[key] = DatasetMeta{AttemptedAt: now, UpdatedAt: now, Complete: true, Generation: r.generation}
	}
}

func TestPassengerPriceMaturityUsesConservativeSixAndTwelveHourBounds(t *testing.T) {
	for _, tc := range []struct {
		name     string
		age      time.Duration
		interval float64
		ready    bool
	}{
		{"before cargo minimum", 359 * time.Second, 6, false},
		{"at cargo minimum", 360 * time.Second, 6, true},
		{"before repricing", 719 * time.Second, 12, false},
		{"at repricing", 720 * time.Second, 12, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			in := passengerInputFixture()
			meta := in.Metadata["clock"]
			meta.AttemptedAt = meta.AttemptedAt.Add(-2 * time.Second)
			in.Metadata["clock"] = meta
			r := &Runner{stateCache: AccountState{Clock: in.Clock}, meta: in.Metadata, generation: in.Generation}
			opened := in.Now.Add(-tc.age)
			passage := &passageIntent{StepOpenedAt: opened}
			offer := gameapi.PassengerOffer{CreatedAt: opened}
			if got := r.passengerPriceMature(passage, offer, tc.interval); got != tc.ready {
				t.Fatalf("price maturity = %t, want %t at age %s", got, tc.ready, tc.age)
			}
			offer.CreatedAt = in.Now
			if r.passengerPriceMature(passage, offer, tc.interval) {
				t.Fatal("an older persisted price shortened the current offer's lifetime")
			}
		})
	}
}

func TestPassengerCargoPriorityWaitsSixHours(t *testing.T) {
	for _, age := range []float64{5.9, 6} {
		h, r, in, _ := passengerCargoFixture(t, age)
		mission, data := h.mission(in.Ship.Ship.ID)
		fare := data.Passage.FarePence
		changed, err := r.driveMission(h.ctx, in.Ship, mission, &data)
		want := age >= passengerMinOfferGameHours
		if err != nil || changed != want || (data.Action != nil) != want || data.Passage.Abandon != want {
			t.Fatalf("age %.1f cargo priority: changed=%t data=%+v error=%v", age, changed, data, err)
		}
		if data.Passage.FarePence != fare || data.Passage.Reoffer {
			t.Fatal("cargo priority changed the passenger fare")
		}
		if want && data.Action.Stage != "passenger-cancel" {
			t.Fatalf("cargo did not stage an ordinary offer cancellation: %+v", data.Action)
		}
	}
}

func TestPassengerCargoCancellationPreflightEnforcesLifetime(t *testing.T) {
	h, r, in, offer := passengerCargoFixture(t, 6.1)
	passengerTick(t, h, r)
	mission, data := h.mission(in.Ship.Ship.ID)
	offer.CreatedAt = in.Now.Add(-gameDuration(5.9/24, in.Clock.Calendar.TimeScale))
	r.snapshot["passenger_offers"] = encode([]gameapi.PassengerOffer{offer})
	if err := r.newActionPreflight(h.ctx, mission, &data); err == nil {
		t.Fatal("a staged cargo cancellation bypassed the current offer's six-hour lifetime")
	}
}

func TestPassengerRepricingUsesTwelveHourDriverAndPreflight(t *testing.T) {
	for _, age := range []float64{6, 11.9, 12} {
		h, r, in, _ := passengerCargoFixture(t, age)
		r.stateCache.Contracts = nil
		mission, data := h.mission(in.Ship.Ship.ID)
		fare := data.Passage.FarePence
		changed, err := r.driveMission(h.ctx, in.Ship, mission, &data)
		want := age >= passengerReladderGameHours
		if err != nil || changed != want || data.Passage.Reoffer != want {
			t.Fatalf("age %.1f repricing: changed=%t passage=%+v error=%v", age, changed, data.Passage, err)
		}
		if want {
			if data.Action == nil || data.Action.Stage != "passenger-cancel" || data.Passage.FarePence >= fare {
				t.Fatalf("mature price did not stage one lower fare: %+v", data)
			}
			if err := r.newActionPreflight(h.ctx, mission, &data); err != nil {
				t.Fatalf("mature repricing failed preflight: %v", err)
			}
		} else {
			data.Passage.Reoffer = true
			if err := r.stage(h.ctx, mission, &data, in.Ship, "passenger-cancel", "/v1/passengers/cancel-offer", map[string]string{"offer_id": data.Passage.OfferID}); err != nil {
				t.Fatal(err)
			}
			if err := r.newActionPreflight(h.ctx, mission, &data); err == nil {
				t.Fatalf("age %.1f repricing bypassed send preflight", age)
			}
		}
	}
}

func TestPassengerCargoCancellationRetainsBoardedGroups(t *testing.T) {
	h, r, in, offer := passengerCargoFixture(t, 6.1)
	passengerTick(t, h, r)
	mission, data := h.mission(in.Ship.Ship.ID)
	offer.Status = "cancelled"
	h.api.server.Config.Handler = http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		if req.Method != http.MethodPost || req.URL.Path != "/v1/passengers/cancel-offer" {
			t.Errorf("unexpected passenger request: %s %s", req.Method, req.URL)
		}
		_ = json.NewEncoder(w).Encode(offer)
	})
	if changed, err := r.dispatch(h.ctx, mission, &data); err != nil || !changed || data.Action != nil {
		t.Fatalf("cancel result: changed=%t error=%v", changed, err)
	}
	boarded, promised := in.Now, in.Now.Add(time.Hour)
	group := gameapi.PassengerAboardGroup{ID: "boarding-race", Size: 10, HoldTons: 10, DestPortID: "destination", BoardedAt: &boarded, PromisedArrivalAt: &promised}
	in.Ship.PassengerHoldTons, in.Ship.CargoUsedTons = 10, 10
	r.stateCache.Ships[0] = in.Ship
	passengerOfferCapture(r, offer, []gameapi.PassengerAboardGroup{group})
	if changed, err := r.driveMission(h.ctx, in.Ship, mission, &data); err != nil || !changed {
		t.Fatalf("boarding reconciliation: changed=%t error=%v", changed, err)
	}
	_, persisted := h.mission(in.Ship.Ship.ID)
	if persisted.Passage == nil || len(persisted.Passage.Groups) != 1 || persisted.Passage.Groups[0].ID != group.ID {
		t.Fatalf("cargo cancellation discarded a boarded promise: %+v", persisted)
	}
}
