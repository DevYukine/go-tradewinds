package engine

import (
	"encoding/json"
	"errors"
	"net/http"
	"testing"
	"time"

	"github.com/DevYukine/go-tradewinds/internal/gameapi"
)

func TestPassengerUnsafeOpenOffersCloseBeforeDiscretionaryMinimum(t *testing.T) {
	for _, reason := range []string{"healthy discretionary closure", "healthy near deadline", "below floor", "increased cost floor", "excess berths", "piracy", "foreign marque", "unavailable hazard bound"} {
		t.Run(reason, func(t *testing.T) {
			h, r, in, offer := passengerCargoFixture(t, 0.5)
			r.stateCache.Contracts = nil
			mission, data := h.mission(in.Ship.Ship.ID)
			healthy := reason == "healthy discretionary closure" || reason == "healthy near deadline"
			data.Passage.Abandon = healthy
			switch reason {
			case "healthy near deadline":
				groups := r.passengerGroups("origin")
				groups[0].BoardBy = in.Now.Add(gameDuration(1.0/24, in.Clock.Calendar.TimeScale))
				r.snapshot["passenger_waiting"] = encode(map[string]any{"origin": groups})
			case "increased cost floor":
				r.stateCache.Markets["origin"].Books[0].Depth.Asks[0].PricePence *= 100
			case "below floor":
				offer.FarePerHeadPence = data.Passage.FloorPence - 1
			case "excess berths":
				offer.MaxBerths++
			case "piracy":
				zones := decode[[]ZoneObservation](r.snapshot, "zones")
				zones[0].NetPiracyBPS = 1_000
				r.snapshot["zones"] = encode(zones)
			case "foreign marque":
				r.snapshot["marques"] = encode([]letterOfMarque{{ID: "00000000-0000-4000-8000-000000000001", SeaZoneID: "channel", SponsorCompanyID: "rival", AddedPiracyBPS: 100, CreatedAt: in.Now.Add(-time.Minute), UpdatedAt: in.Now, ExpiresAt: in.Now.Add(time.Hour)}})
				var marques []letterOfMarque
				if err := json.Unmarshal(r.snapshot["marques"], &marques); err != nil || len(marques) != 1 {
					t.Fatalf("foreign marque fixture is not valid wire evidence: %v %+v", err, marques)
				}
				zones := decode[[]ZoneObservation](r.snapshot, "zones")
				zones[0].MarquePiracyBPS, zones[0].ActiveMarques = new(int64(100)), new(int64(1))
				r.snapshot["zones"] = encode(zones)
				input := r.planningInput(in.Ship)
				hull, _ := findHull(input.Catalog, in.Ship.Ship.ShipClass)
				if _, err := observedPlanningRoute(input, offer.PortID, offer.DestPortID, hull); err == nil {
					t.Fatal("fixture did not exercise the generic MarqueZones route veto")
				}
				route := input.Routes[routeKey(offer.PortID, offer.DestPortID, hull.ID)]
				if err := passengerRouteSafe(input, route, marques); !errors.Is(err, errPassengerRouteUnsafe) {
					t.Fatalf("fresh passenger route evidence did not prove the foreign marque: %v", err)
				}
			case "unavailable hazard bound":
				r.stateCache.HazardQuotes = nil
			}
			passengerOfferCapture(r, offer, []gameapi.PassengerAboardGroup{})
			if err := r.saveMission(h.ctx, mission, &data); err != nil {
				t.Fatal(err)
			}
			if changed, err := r.driveMission(h.ctx, in.Ship, mission, &data); err != nil || changed == healthy {
				t.Fatalf("offer protection: reason=%s changed=%t error=%v data=%+v", reason, changed, err, data)
			}
			if healthy {
				if data.Action != nil {
					t.Fatalf("healthy offer was cancelled before six game hours: %+v", data.Action)
				}
				if err := r.stage(h.ctx, mission, &data, in.Ship, "passenger-cancel", "/v1/passengers/cancel-offer", map[string]string{"offer_id": offer.ID}); err != nil {
					t.Fatal(err)
				}
				if err := r.newActionPreflight(h.ctx, mission, &data); err == nil {
					t.Fatal("discretionary cancellation preflight bypassed the minimum lifetime")
				}
				return
			}
			if data.Action == nil || data.Action.Stage != "passenger-cancel" || !data.Passage.Abandon || data.Passage.Reoffer {
				t.Fatalf("unsafe offer stayed open: %+v", data)
			}
			if err := r.newActionPreflight(h.ctx, mission, &data); err != nil {
				t.Fatalf("protective cancellation was held behind the minimum lifetime: %v", err)
			}
			boarded, promised := in.Now, in.Now.Add(time.Hour)
			data.Passage.Groups = []gameapi.PassengerAboardGroup{{ID: "boarding-race", Size: 10, HoldTons: 10, DestPortID: "destination", BoardedAt: &boarded, PromisedArrivalAt: &promised}}
			if err := r.newActionPreflight(h.ctx, mission, &data); err != nil || len(data.Passage.Groups) != 1 {
				t.Fatalf("protective closure lost a boarded promise: %+v error=%v", data.Passage, err)
			}
		})
	}
}

func TestPassengerConflictPreservesProtectionAndOfferAge(t *testing.T) {
	for _, excessBerths := range []bool{false, true} {
		t.Run(map[bool]string{false: "below floor", true: "excess berths"}[excessBerths], func(t *testing.T) {
			h, r, in := passengerRunnerFixture(t)
			if changed, err := r.considerPassage(h.ctx, in.Ship, in, nil); err != nil || !changed {
				t.Fatalf("admit: %t %v", changed, err)
			}
			passengerTick(t, h, r)
			mission, data := h.mission(in.Ship.Ship.ID)
			protectedBerths := data.Passage.MaxBerths
			offer := gameapi.PassengerOffer{ID: "conflicting-young-offer", ShipID: in.Ship.Ship.ID, PortID: "origin", DestPortID: "destination", FarePerHeadPence: data.Passage.FloorPence - 1, MaxBerths: protectedBerths, Status: "open", CreatedAt: in.Now.Add(-gameDuration(0.5/24, in.Clock.Calendar.TimeScale))}
			if excessBerths {
				offer.FarePerHeadPence = data.Passage.FloorPence
				offer.MaxBerths++
			}
			h.api.server.Config.Handler = http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
				if req.Method == http.MethodGet {
					_ = json.NewEncoder(w).Encode([]gameapi.PassengerOffer{offer})
					return
				}
				w.WriteHeader(http.StatusConflict)
				_, _ = w.Write([]byte(`{"code":"offer_exists"}`))
			})
			if changed, err := r.dispatch(h.ctx, mission, &data); err != nil || !changed || data.Action != nil {
				t.Fatalf("conflict reconciliation: changed=%t error=%v data=%+v", changed, err, data)
			}
			if !data.Passage.Abandon || data.Passage.MaxBerths != protectedBerths || !data.Passage.StepOpenedAt.Equal(offer.CreatedAt) {
				t.Fatalf("reconciliation replaced protection or restarted the offer's lifetime: %+v", data.Passage)
			}
			passengerOfferCapture(r, offer, []gameapi.PassengerAboardGroup{})
			if changed, err := r.driveMission(h.ctx, in.Ship, mission, &data); err != nil || !changed || data.Action == nil || data.Action.Stage != "passenger-cancel" {
				t.Fatalf("unsafe reconciled offer waited six more game hours: changed=%t error=%v data=%+v", changed, err, data)
			}
			if err := r.newActionPreflight(h.ctx, mission, &data); err != nil {
				t.Fatalf("unsafe reconciled offer could not close: %v", err)
			}
		})
	}
}

func TestPassengerUnknownProtectionInputsRetainTheOpenFare(t *testing.T) {
	for _, age := range []float64{0.5, 12.1} {
		name := "before minimum lifetime"
		if age > 12 {
			name = "after repricing interval"
		}
		t.Run(name, func(t *testing.T) {
			for _, reason := range []string{"stale books", "stale port", "stale clock", "stale zones", "stale marques", "stale waiting", "unpriced return", "unknown stress hours", "missing provisions depth", "lagging public marque totals"} {
				t.Run(reason, func(t *testing.T) {
					h, r, in, offer := passengerCargoFixture(t, age)
					r.stateCache.Contracts = nil
					mission, data := h.mission(in.Ship.Ship.ID)
					key := ""
					switch reason {
					case "stale books":
						key = "books/origin"
					case "stale port":
						key = "ports/destination"
					case "stale clock":
						key = "clock"
					case "stale zones":
						key = "zones"
					case "stale marques":
						key = "marques"
					case "stale waiting":
						key = "passenger_waiting/origin"
					case "unpriced return":
						delete(r.stateCache.Routes, routeKey("destination", "origin", in.Ship.Ship.ShipClass))
					case "unknown stress hours":
						r.stateCache.Catalog.ShipClasses[0].BaseSpeedCentiknots = 0
					case "missing provisions depth":
						r.stateCache.Markets["origin"].Books[0].Depth.Asks = nil
					case "lagging public marque totals":
						r.snapshot["marques"] = encode([]letterOfMarque{{ID: "00000000-0000-4000-8000-000000000001", SeaZoneID: "channel", SponsorCompanyID: in.CompanyID, AddedPiracyBPS: 100, CreatedAt: in.Now.Add(-time.Minute), UpdatedAt: in.Now, ExpiresAt: in.Now.Add(time.Hour)}})
					}
					if key != "" {
						meta := r.meta[key]
						meta.Stale = true
						r.meta[key] = meta
					}
					input, err := r.committedPlanningInput(h.ctx, in.Ship)
					if err != nil {
						t.Fatal(err)
					}
					if unsafe, err := passengerOpenOfferUnsafe(input, data.Passage, offer, r.passengerGroups(offer.PortID), decode[[]letterOfMarque](r.snapshot, "marques")); unsafe || err == nil {
						t.Fatalf("unknown protection became a proven closure: unsafe=%t error=%v", unsafe, err)
					}
					if changed, err := r.driveMission(h.ctx, in.Ship, mission, &data); err != nil || changed || data.Action != nil ||
						data.Passage.Abandon || data.Passage.Reoffer || data.Passage.FarePence != offer.FarePerHeadPence {
						t.Fatalf("unknown protection churned the open offer: changed=%t error=%v data=%+v", changed, err, data)
					}
					if err := r.stage(h.ctx, mission, &data, in.Ship, "passenger-cancel", "/v1/passengers/cancel-offer", map[string]string{"offer_id": offer.ID}); err != nil {
						t.Fatal(err)
					}
					if err := r.newActionPreflight(h.ctx, mission, &data); err == nil {
						t.Fatal("missing protection authority authorized discretionary cancellation")
					}
				})
			}
		})
	}
}
