package engine

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"reflect"
	"sort"
	"sync"
	"testing"
	"time"

	"github.com/DevYukine/go-tradewinds/internal/gameapi"
	"github.com/DevYukine/go-tradewinds/internal/store"
	"go.uber.org/zap"
)

const passengerWaitingFixture = `[{"id":"group-a","kind":"travellers","size":7,"origin_port_id":"hamburg","dest_port_id":"amsterdam","board_by":"2026-10-28T12:00:00Z"}]`
const passengerOfferFixture = `[{"id":"offer-a","ship_id":"ship-a","port_id":"hamburg","dest_port_id":"amsterdam","fare_per_head_pence":3000000000,"max_berths":20,"berths_taken":7,"status":"closed","created_at":"2026-10-28T10:00:00Z","close_reason":"departed","closed_at":"2026-10-28T11:00:00Z"}]`
const passengerAboardFixture = `[{"id":"group-a","kind":"travellers","size":7,"origin_port_id":"hamburg","dest_port_id":"amsterdam","fare_per_head_pence":3000000000,"full_fare_pence":21000000000,"hold_tons":7,"boarded_at":"2026-10-28T10:30:00Z","promised_arrival_at":"2026-10-28T13:00:00Z"}]`
const passengerFareFixture = `[{"id":"fare-a","kind":"travellers","size":7,"origin_port_id":"hamburg","dest_port_id":"amsterdam","fare_per_head_pence":3000000000,"accepted_at":"2026-10-28T10:30:00Z"}]`

func passengerCaptureShip(id, port, status string) ShipState {
	var ship ShipState
	ship.Ship.ID, ship.Ship.CurrentPortID, ship.Ship.Status = id, port, status
	return ship
}

func setPassengerCaptureClock(runner *Runner, hour int64, minute int32) {
	now := time.Now().UTC()
	clock := gameapi.Clock{RealUTC: now}
	clock.Calendar.TotalGameHours = hour
	clock.Calendar.Minute = minute
	clock.Calendar.TimeScale = 60
	runner.snapshot["clock"] = encode(clock)
	for _, key := range []string{"ships", "clock"} {
		runner.meta[key] = DatasetMeta{UpdatedAt: now, AttemptedAt: now, Complete: true, Generation: runner.generation}
	}
}

func newPassengerCaptureRunner(t *testing.T, ships []ShipState, handler http.HandlerFunc) *Runner {
	t.Helper()
	server := httptest.NewServer(handler)
	t.Cleanup(server.Close)
	runner := New(gameapi.New(server.URL, "synthetic-key", nil), nil, store.Account{}, zap.NewNop(), Options{})
	runner.generation = "passenger-capture"
	runner.snapshot["ships"] = encode(ships)
	setPassengerCaptureClock(runner, 100, 0)
	return runner
}

func TestPassengerAPITypesDecodeDocumentedFixtures(t *testing.T) {
	var waiting []gameapi.PassengerWaitingGroup
	var offers []gameapi.PassengerOffer
	var aboard []gameapi.PassengerAboardGroup
	var fares []gameapi.PassengerFare
	for _, fixture := range []struct {
		raw string
		out any
	}{
		{passengerWaitingFixture, &waiting},
		{passengerOfferFixture, &offers},
		{passengerAboardFixture, &aboard},
		{passengerFareFixture, &fares},
	} {
		if err := json.Unmarshal([]byte(fixture.raw), fixture.out); err != nil {
			t.Fatal(err)
		}
	}
	if group := waiting[0]; group.ID != "group-a" || group.Kind != "travellers" || group.Size != 7 || group.OriginPortID != "hamburg" || group.DestPortID != "amsterdam" || group.BoardBy.Hour() != 12 {
		t.Fatalf("waiting group lost documented fields: %+v", group)
	}
	if offer := offers[0]; offer.ShipID != "ship-a" || offer.PortID != "hamburg" || offer.DestPortID != "amsterdam" || offer.FarePerHeadPence != 3_000_000_000 || offer.MaxBerths != 20 || offer.BerthsTaken != 7 || offer.CloseReason != "departed" || offer.ClosedAt == nil || offer.ClosedAt.Hour() != 11 || offer.CreatedAt.Hour() != 10 {
		t.Fatalf("offer lost documented fields: %+v", offer)
	}
	if group := aboard[0]; group.OriginPortID != "hamburg" || group.Size != 7 || group.HoldTons != 7 || group.FullFarePence != 21_000_000_000 || group.BoardedAt == nil || group.BoardedAt.Minute() != 30 || group.PromisedArrivalAt == nil || group.PromisedArrivalAt.Hour() != 13 {
		t.Fatalf("aboard group lost documented fields: %+v", group)
	}
	if fare := fares[0]; fare.Kind != "travellers" || fare.Size != 7 || fare.OriginPortID != "hamburg" || fare.DestPortID != "amsterdam" || fare.FarePerHeadPence != 3_000_000_000 || fare.AcceptedAt.Minute() != 30 {
		t.Fatalf("fare lost documented fields: %+v", fare)
	}
	var nullable struct {
		Offer  gameapi.PassengerOffer       `json:"offer"`
		Aboard gameapi.PassengerAboardGroup `json:"aboard"`
	}
	if err := json.Unmarshal([]byte(`{"offer":{"close_reason":null,"closed_at":null},"aboard":{"boarded_at":null,"promised_arrival_at":null}}`), &nullable); err != nil {
		t.Fatal(err)
	}
	if nullable.Offer.CloseReason != "" || nullable.Offer.ClosedAt != nil || nullable.Aboard.BoardedAt != nil || nullable.Aboard.PromisedArrivalAt != nil {
		t.Fatalf("nullable passenger fields were fabricated: %+v", nullable)
	}
}

func TestPassengerCaptureWaitingUsesOnlyFreshOccupiedPorts(t *testing.T) {
	for _, mode := range []string{"fresh", "stale", "previous generation"} {
		t.Run(mode, func(t *testing.T) {
			var mu sync.Mutex
			var ports []string
			offers := 0
			runner := newPassengerCaptureRunner(t, []ShipState{
				passengerCaptureShip("berthed", "hamburg", "berthed"),
				passengerCaptureShip("co-located", "hamburg", "berthed"),
				passengerCaptureShip("roadstead", "amsterdam", "anchored_roadstead"),
				passengerCaptureShip("sunk", "calais", "sunk"),
				passengerCaptureShip("sailing", "london", "sailing"),
				passengerCaptureShip("at-sea", "", "sailing"),
			}, func(w http.ResponseWriter, req *http.Request) {
				mu.Lock()
				defer mu.Unlock()
				switch req.URL.Path {
				case "/v1/passengers/waiting":
					ports = append(ports, req.URL.Query().Get("port_id"))
				case "/v1/passengers/offers":
					offers++
				default:
					t.Errorf("unexpected passenger read %s", req.URL)
				}
				_, _ = w.Write([]byte(`[]`))
			})
			runner.snapshot["factors"] = encode([]string{"portsmouth"})
			runner.snapshot["passenger_waiting"] = encode(map[string]json.RawMessage{"calais": json.RawMessage(passengerWaitingFixture)})
			meta := runner.meta["ships"]
			switch mode {
			case "stale":
				meta.Stale = true
			case "previous generation":
				meta.Generation = "previous"
			}
			runner.meta["ships"] = meta
			if err := runner.refreshPassengers(context.Background()); err != nil {
				t.Fatal(err)
			}
			mu.Lock()
			defer mu.Unlock()
			sort.Strings(ports)
			want := []string(nil)
			if mode == "fresh" {
				want = []string{"amsterdam", "hamburg"}
			}
			wantOffers := 0
			if mode == "fresh" {
				wantOffers = 1
			}
			if !reflect.DeepEqual(ports, want) || offers != wantOffers {
				t.Fatalf("waiting ports %v, offers %d; want ports %v and %d warranted offers captures", ports, offers, want, wantOffers)
			}
			waiting := decode[map[string]json.RawMessage](runner.snapshot, "passenger_waiting")
			if len(waiting) != len(want) || waiting["calais"] != nil || (mode != "fresh" && runner.fresh("passenger_waiting")) {
				t.Fatalf("obsolete or unobserved ports became current: values=%v meta=%+v", waiting, runner.meta["passenger_waiting"])
			}
		})
	}
}

func TestPassengerCaptureAboardRequiresShipEvidence(t *testing.T) {
	var mu sync.Mutex
	var requested []string
	ships := []ShipState{
		passengerCaptureShip("open", "hamburg", "berthed"),
		passengerCaptureShip("held", "", "sailing"),
		passengerCaptureShip("hinted", "", "sailing"),
		passengerCaptureShip("closed", "hamburg", "berthed"),
		passengerCaptureShip("idle", "hamburg", "berthed"),
		passengerCaptureShip("sunk", "calais", "sunk"),
	}
	ships[1].PassengerHoldTons = 1
	ships[5].PassengerHoldTons = 1
	runner := newPassengerCaptureRunner(t, ships, func(w http.ResponseWriter, req *http.Request) {
		switch req.URL.Path {
		case "/v1/passengers/offers":
			_ = json.NewEncoder(w).Encode([]gameapi.PassengerOffer{
				{ID: "offer-open", ShipID: "open", Status: "open"},
				{ID: "offer-closed", ShipID: "closed", Status: "closed"},
				{ID: "offer-sunk", ShipID: "sunk", Status: "open"},
				{ID: "offer-foreign", ShipID: "not-ours", Status: "open"},
			})
		case "/v1/passengers/aboard":
			mu.Lock()
			requested = append(requested, req.URL.Query().Get("ship_id"))
			mu.Unlock()
			_, _ = w.Write([]byte(passengerAboardFixture))
		case "/v1/passengers/waiting":
			_, _ = w.Write([]byte(`[]`))
		default:
			t.Errorf("unexpected passenger read %s", req.URL)
			w.WriteHeader(http.StatusNotFound)
		}
	})
	runner.snapshot["_passenger_boarding_ships"] = encode(map[string]bool{"hinted": true, "idle": false, "sunk": true})
	runner.snapshot["passenger_aboard"] = encode(map[string]json.RawMessage{"closed": json.RawMessage(passengerAboardFixture)})
	if err := runner.refreshPassengers(context.Background()); err != nil {
		t.Fatal(err)
	}
	mu.Lock()
	defer mu.Unlock()
	sort.Strings(requested)
	if want := []string{"held", "hinted", "open"}; !reflect.DeepEqual(requested, want) {
		t.Fatalf("aboard reads %v, want %v", requested, want)
	}
	aboard := decode[map[string][]gameapi.PassengerAboardGroup](runner.snapshot, "passenger_aboard")
	if len(aboard) != 3 || aboard["closed"] != nil || !runner.fresh("passenger_aboard/held") {
		t.Fatalf("aboard scope/metadata mismatch: values=%v meta=%+v", aboard, runner.meta["passenger_aboard/held"])
	}
}

func TestPassengerCaptureFaresUseTenGameHourAttemptGate(t *testing.T) {
	for _, fail := range []bool{false, true} {
		t.Run(fmt.Sprintf("failed=%t", fail), func(t *testing.T) {
			var mu sync.Mutex
			fareReads := 0
			runner := newPassengerCaptureRunner(t, []ShipState{passengerCaptureShip("ship-a", "hamburg", "berthed")}, func(w http.ResponseWriter, req *http.Request) {
				switch req.URL.Path {
				case "/v1/passengers/offers":
					_, _ = w.Write([]byte(`[]`))
				case "/v1/passengers/waiting":
					_, _ = w.Write([]byte(passengerWaitingFixture))
				case "/v1/passengers/fares":
					mu.Lock()
					fareReads++
					mu.Unlock()
					if fail {
						w.WriteHeader(http.StatusNotFound)
						_, _ = w.Write([]byte(`{"error":"not_found"}`))
					} else {
						_, _ = w.Write([]byte(passengerFareFixture))
					}
				default:
					t.Errorf("unexpected passenger read %s", req.URL)
					w.WriteHeader(http.StatusNotFound)
				}
			})
			runner.snapshot["_passenger_fare_hours"] = encode(map[string]float64{"hamburg|amsterdam": 100})
			runner.snapshot["passenger_fares"] = encode(map[string]json.RawMessage{"hamburg|amsterdam": json.RawMessage(passengerFareFixture)})
			for _, observation := range []struct {
				hour   int64
				minute int32
				reads  int
			}{{109, 59, 0}, {110, 0, 1}, {119, 59, 1}, {120, 1, 2}} {
				setPassengerCaptureClock(runner, observation.hour, observation.minute)
				if err := runner.refreshPassengers(context.Background()); err != nil {
					t.Fatal(err)
				}
				mu.Lock()
				got := fareReads
				mu.Unlock()
				if got != observation.reads {
					t.Fatalf("game hour %d:%02d made %d fare reads, want %d", observation.hour, observation.minute, got, observation.reads)
				}
			}
			meta := runner.meta["passenger_fares/hamburg|amsterdam"]
			if fail && (meta.Error == "" || !meta.Stale || meta.Complete || runner.fresh("passenger_fares/hamburg|amsterdam")) {
				t.Fatalf("failed fare attempt became fresh: %+v", meta)
			}
			hours := decode[map[string]float64](runner.snapshot, "_passenger_fare_hours")
			if hours["hamburg|amsterdam"] < 120 {
				t.Fatalf("attempt game hour was not retained: %v", hours)
			}
			// A restart keeps the gate for the lane the occupied port still
			// backs, and the deliberately throttled retained value counts as
			// valid group evidence rather than a spurious failure.
			saved := runner.snapshot
			saved["_meta"] = encode(runner.meta)
			runner = New(runner.api, nil, store.Account{}, zap.NewNop(), Options{})
			runner.snapshot, runner.generation = saved, "restarted"
			runner.meta = decode[map[string]DatasetMeta](saved, "_meta")
			setPassengerCaptureClock(runner, 121, 0)
			if err := runner.refreshPassengers(context.Background()); err != nil {
				t.Fatal(err)
			}
			mu.Lock()
			if got := fareReads; got != 2 {
				t.Fatalf("restart made %d fare reads inside the gate, want 2", got)
			}
			mu.Unlock()
			if kept := decode[map[string]float64](runner.snapshot, "_passenger_fare_hours")["hamburg|amsterdam"]; kept != hours["hamburg|amsterdam"] {
				t.Fatalf("restart lost the current lane's attempt gate: got %v want %v", kept, hours["hamburg|amsterdam"])
			}
			if aggregate := runner.meta["passenger_fares"]; !fail && (!aggregate.Complete || aggregate.Stale || aggregate.Error != "") {
				t.Fatalf("throttled retained lane left the group meta failed: %+v", aggregate)
			}
			// Leaving the port never resets the gate: the departure is only an
			// irrelevance observation, so the gate, payload and per-lane
			// metadata survive, and a return inside ten game hours grants no
			// read in either variant.
			runner.snapshot["ships"] = encode([]ShipState{passengerCaptureShip("ship-a", "calais", "berthed")})
			setPassengerCaptureClock(runner, 122, 0)
			if err := runner.refreshPassengers(context.Background()); err != nil {
				t.Fatal(err)
			}
			if kept := decode[map[string]float64](runner.snapshot, "_passenger_fare_hours")["hamburg|amsterdam"]; kept != hours["hamburg|amsterdam"] {
				t.Fatalf("departure reset the persisted gate: got %v want %v", kept, hours["hamburg|amsterdam"])
			}
			if kept := decode[map[string]json.RawMessage](runner.snapshot, "passenger_fares")["hamburg|amsterdam"]; string(kept) != passengerFareFixture {
				t.Fatalf("departure dropped the retained payload: %s", kept)
			}
			if _, exists := runner.meta["passenger_fares/hamburg|amsterdam"]; !exists {
				t.Fatal("departure dropped the per-lane metadata")
			}
			if since := decode[map[string]float64](runner.snapshot, "_passenger_fare_irrelevant")["hamburg|amsterdam"]; since < 122 {
				t.Fatalf("departure did not start the irrelevance clock: %v", since)
			}
			runner.snapshot["ships"] = encode([]ShipState{passengerCaptureShip("ship-a", "hamburg", "berthed")})
			setPassengerCaptureClock(runner, 126, 0)
			if err := runner.refreshPassengers(context.Background()); err != nil {
				t.Fatal(err)
			}
			mu.Lock()
			if got := fareReads; got != 2 {
				t.Fatalf("return inside the interval made %d fare reads, want 2", got)
			}
			mu.Unlock()
			if since := decode[map[string]float64](runner.snapshot, "_passenger_fare_irrelevant")["hamburg|amsterdam"]; since != 0 {
				t.Fatalf("return did not clear the irrelevance clock: %v", since)
			}
			// A restart inside a second absence keeps gate and clock, and ten
			// game hours of authoritative irrelevance then expire the lane.
			runner.snapshot["ships"] = encode([]ShipState{passengerCaptureShip("ship-a", "calais", "berthed")})
			setPassengerCaptureClock(runner, 127, 0)
			if err := runner.refreshPassengers(context.Background()); err != nil {
				t.Fatal(err)
			}
			saved = runner.snapshot
			saved["_meta"] = encode(runner.meta)
			runner = New(runner.api, nil, store.Account{}, zap.NewNop(), Options{})
			runner.snapshot, runner.generation = saved, "restarted-absent"
			runner.meta = decode[map[string]DatasetMeta](saved, "_meta")
			setPassengerCaptureClock(runner, 128, 0)
			if err := runner.refreshPassengers(context.Background()); err != nil {
				t.Fatal(err)
			}
			if kept := decode[map[string]float64](runner.snapshot, "_passenger_fare_hours")["hamburg|amsterdam"]; kept != hours["hamburg|amsterdam"] {
				t.Fatalf("restarted absence lost the gate: got %v want %v", kept, hours["hamburg|amsterdam"])
			}
			if since := decode[map[string]float64](runner.snapshot, "_passenger_fare_irrelevant")["hamburg|amsterdam"]; since < 127 {
				t.Fatalf("restarted absence lost the irrelevance clock: %v", since)
			}
			setPassengerCaptureClock(runner, 138, 0)
			if err := runner.refreshPassengers(context.Background()); err != nil {
				t.Fatal(err)
			}
			if kept := decode[map[string]float64](runner.snapshot, "_passenger_fare_hours")["hamburg|amsterdam"]; kept != 0 {
				t.Fatalf("ten absent game hours kept the gate: %v", kept)
			}
			if kept := decode[map[string]json.RawMessage](runner.snapshot, "passenger_fares")["hamburg|amsterdam"]; len(kept) != 0 {
				t.Fatalf("expired lane kept its payload: %s", kept)
			}
			if since := decode[map[string]float64](runner.snapshot, "_passenger_fare_irrelevant")["hamburg|amsterdam"]; since != 0 {
				t.Fatalf("expired lane kept its irrelevance clock: %v", since)
			}
			if _, exists := runner.meta["passenger_fares/hamburg|amsterdam"]; exists {
				t.Fatal("expired lane kept its per-lane metadata")
			}
			// Returning after expiry reads the lane exactly once.
			runner.snapshot["ships"] = encode([]ShipState{passengerCaptureShip("ship-a", "hamburg", "berthed")})
			setPassengerCaptureClock(runner, 139, 0)
			if err := runner.refreshPassengers(context.Background()); err != nil {
				t.Fatal(err)
			}
			mu.Lock()
			if got := fareReads; got != 3 {
				t.Fatalf("return after expiry made %d fare reads, want 3", got)
			}
			mu.Unlock()
		})
	}
}

func TestPassengerFareBudgetPrunesIrrelevantLanesAndBoundsPayload(t *testing.T) {
	at := time.Date(2026, 10, 28, 10, 0, 0, 0, time.UTC)
	const destinations = 40
	laneRows := func(origin, dest string) []gameapi.PassengerFare {
		rows := make([]gameapi.PassengerFare, 200)
		for index := range rows {
			rows[index] = gameapi.PassengerFare{ID: fmt.Sprintf("fare-%s-%s-%03d", origin, dest, index), Kind: "travellers", Size: 1,
				OriginPortID: origin, DestPortID: dest, FarePerHeadPence: 100, AcceptedAt: at}
		}
		return rows
	}
	var mu sync.Mutex
	fareReads := 0
	runner := newPassengerCaptureRunner(t, []ShipState{passengerCaptureShip("ship-a", "hamburg", "berthed")}, func(w http.ResponseWriter, req *http.Request) {
		switch req.URL.Path {
		case "/v1/passengers/offers":
			_, _ = w.Write([]byte(`[]`))
		case "/v1/passengers/waiting":
			groups := make([]gameapi.PassengerWaitingGroup, destinations)
			for index := range groups {
				groups[index] = gameapi.PassengerWaitingGroup{ID: fmt.Sprintf("group-%02d", index), Kind: "travellers", Size: 1,
					OriginPortID: "hamburg", DestPortID: fmt.Sprintf("destination-port-%02d", index), BoardBy: at.Add(time.Hour)}
			}
			_ = json.NewEncoder(w).Encode(groups)
		case "/v1/passengers/fares":
			mu.Lock()
			fareReads++
			mu.Unlock()
			_ = json.NewEncoder(w).Encode(laneRows(req.URL.Query().Get("from"), req.URL.Query().Get("to")))
		default:
			t.Errorf("unexpected passenger read %s", req.URL)
			w.WriteHeader(http.StatusNotFound)
		}
	})
	historical := make(map[string]json.RawMessage)
	historicalHours := make(map[string]float64)
	for index := range destinations {
		lane := fmt.Sprintf("ruin-port-%02d|gone-port-%02d", index, index)
		historical[lane] = encode(laneRows(fmt.Sprintf("ruin-port-%02d", index), fmt.Sprintf("gone-port-%02d", index)))
		historicalHours[lane] = 10
	}
	runner.snapshot["passenger_fares"] = encode(historical)
	runner.snapshot["_passenger_fare_hours"] = encode(historicalHours)
	if err := runner.refreshPassengers(context.Background()); err != nil {
		t.Fatal(err)
	}
	fares := decode[map[string]json.RawMessage](runner.snapshot, "passenger_fares")
	hours := decode[map[string]float64](runner.snapshot, "_passenger_fare_hours")
	if fares["ruin-port-00|gone-port-00"] != nil || len(hours) != 2*destinations {
		t.Fatalf("absent lanes lost their bounded retention: payloads=%d gates=%d", len(fares), len(hours))
	}
	if clocks := decode[map[string]float64](runner.snapshot, "_passenger_fare_irrelevant"); len(clocks) != destinations {
		t.Fatalf("absent lanes did not start one irrelevance clock each: %d", len(clocks))
	}
	payload := 0
	for lane, raw := range fares {
		payload += len(raw)
		var rows []gameapi.PassengerFare
		if err := json.Unmarshal(raw, &rows); err != nil || len(rows) != 200 {
			t.Fatalf("retained lane %s lost its bounded 200-row page: %d rows err=%v", lane, len(rows), err)
		}
	}
	if payload > passengerFareBudgetBytes {
		t.Fatalf("fare payload exceeded its budget: %d bytes in %d lanes", payload, len(fares))
	}
	if len(fares) >= destinations {
		t.Fatalf("over-budget relevant lanes were not evicted: %d lanes, %d bytes", len(fares), payload)
	}
	if contribution := len(encode(fares)) + len(encode(runner.snapshot["_passenger_fare_hours"])) + len(encode(runner.snapshot["_passenger_fare_irrelevant"])); contribution > store.MaxSnapshotBytes {
		t.Fatalf("encoded fare contribution exceeded the snapshot cap: %d bytes", contribution)
	}
	mu.Lock()
	reads := fareReads
	mu.Unlock()
	if reads != destinations {
		t.Fatalf("fare reads %d, want one per relevant lane %d", reads, destinations)
	}
	// A restart inside the gate re-reads nothing, keeps every relevant lane's
	// gate (including evicted payloads) and stays within the budget.
	saved := runner.snapshot
	runner = New(runner.api, nil, store.Account{}, zap.NewNop(), Options{})
	runner.snapshot, runner.generation = saved, "restarted"
	setPassengerCaptureClock(runner, 101, 0)
	if err := runner.refreshPassengers(context.Background()); err != nil {
		t.Fatal(err)
	}
	mu.Lock()
	if fareReads != reads {
		mu.Unlock()
		t.Fatalf("restart re-read gated lanes: %d -> %d", reads, fareReads)
	}
	if kept := decode[map[string]float64](runner.snapshot, "_passenger_fare_hours")["hamburg|destination-port-00"]; kept != hours["hamburg|destination-port-00"] {
		mu.Unlock()
		t.Fatalf("restart lost the current lane's gate: got %v want %v", kept, hours["hamburg|destination-port-00"])
	}
	restartedPayload := 0
	for _, raw := range decode[map[string]json.RawMessage](runner.snapshot, "passenger_fares") {
		restartedPayload += len(raw)
	}
	restartedHours := decode[map[string]float64](runner.snapshot, "_passenger_fare_hours")
	mu.Unlock()
	if restartedPayload > passengerFareBudgetBytes || len(restartedHours) != 2*destinations {
		t.Fatalf("restart broke the fare budget or gates: %d bytes, %d gates", restartedPayload, len(restartedHours))
	}
	// Ten game hours of authoritative absence expire the departed lanes'
	// gates and clocks without a single extra read for them, while the
	// occupied lanes read exactly once per interval and stay bounded.
	setPassengerCaptureClock(runner, 111, 1)
	if err := runner.refreshPassengers(context.Background()); err != nil {
		t.Fatal(err)
	}
	expiredHours := decode[map[string]float64](runner.snapshot, "_passenger_fare_hours")
	expiredFares := decode[map[string]json.RawMessage](runner.snapshot, "passenger_fares")
	if _, survived := expiredHours["ruin-port-00|gone-port-00"]; survived || len(expiredHours) != destinations {
		t.Fatalf("absence did not expire the departed lanes: gates=%d", len(expiredHours))
	}
	if clocks := decode[map[string]float64](runner.snapshot, "_passenger_fare_irrelevant"); len(clocks) != 0 {
		t.Fatalf("expired lanes kept irrelevance clocks: %d", len(clocks))
	}
	expiredPayload := 0
	for lane, raw := range expiredFares {
		if _, departed := historical[lane]; departed {
			t.Fatalf("expired lane %s kept its payload", lane)
		}
		expiredPayload += len(raw)
	}
	if expiredPayload > passengerFareBudgetBytes {
		t.Fatalf("expired-absence pass broke the fare budget: %d bytes", expiredPayload)
	}
	mu.Lock()
	totalReads := fareReads
	mu.Unlock()
	if totalReads != reads+destinations {
		t.Fatalf("interval expiry made %d fare reads, want %d", totalReads, reads+destinations)
	}
	if aggregate := runner.meta["passenger_fares"]; aggregate.Complete || !aggregate.Stale || aggregate.Error == "" {
		t.Fatalf("byte-budget eviction falsely reports complete fare values: %+v", aggregate)
	}
}

func TestPassengerFareGateSurvivesFailedWaitingAndStaleFleet(t *testing.T) {
	for _, mode := range []string{"failed waiting", "stale fleet"} {
		t.Run(mode, func(t *testing.T) {
			var mu sync.Mutex
			counts := make(map[string]int)
			failWaiting := mode == "failed waiting"
			runner := newPassengerCaptureRunner(t, []ShipState{passengerCaptureShip("ship-a", "hamburg", "berthed")}, func(w http.ResponseWriter, req *http.Request) {
				mu.Lock()
				counts[req.URL.Path]++
				failedWaiting := failWaiting && req.URL.Path == "/v1/passengers/waiting" && counts[req.URL.Path] == 2
				mu.Unlock()
				switch req.URL.Path {
				case "/v1/passengers/offers":
					_, _ = w.Write([]byte(`[]`))
				case "/v1/passengers/waiting":
					if failedWaiting {
						w.WriteHeader(http.StatusNotFound)
						_, _ = w.Write([]byte(`{"error":"unavailable"}`))
						return
					}
					_, _ = w.Write([]byte(passengerWaitingFixture))
				case "/v1/passengers/fares":
					_, _ = w.Write([]byte(passengerFareFixture))
				default:
					t.Errorf("unexpected passenger read %s", req.URL)
					w.WriteHeader(http.StatusNotFound)
				}
			})
			// The first good pass reads the lane once and stamps its gate.
			if err := runner.refreshPassengers(context.Background()); err != nil {
				t.Fatal(err)
			}
			hours := decode[map[string]float64](runner.snapshot, "_passenger_fare_hours")
			if hours["hamburg|amsterdam"] < 100 {
				t.Fatalf("first read did not stamp the gate: %v", hours)
			}
			// The disrupted pass keeps the gate and payload and freezes the
			// irrelevance clock instead of proving the lane irrelevant.
			if failWaiting {
				setPassengerCaptureClock(runner, 101, 0)
				if err := runner.refreshPassengers(context.Background()); err != nil {
					t.Fatal(err)
				}
				waitingMeta := runner.meta["passenger_waiting/hamburg"]
				if waitingMeta.Complete || !waitingMeta.Stale || waitingMeta.Error == "" {
					t.Fatalf("failed waiting capture looks healthy: %+v", waitingMeta)
				}
			} else {
				// setPassengerCaptureClock re-stamps the ships meta, so the
				// stale flag goes on after it.
				setPassengerCaptureClock(runner, 101, 0)
				ships := runner.meta["ships"]
				ships.Stale = true
				runner.meta["ships"] = ships
				if err := runner.refreshPassengers(context.Background()); err != nil {
					t.Fatal(err)
				}
				waitingMeta := runner.meta["passenger_waiting"]
				if waitingMeta.Complete || !waitingMeta.Stale || waitingMeta.Error == "" {
					t.Fatalf("stale fleet left the waiting group healthy: %+v", waitingMeta)
				}
			}
			if kept := decode[map[string]float64](runner.snapshot, "_passenger_fare_hours")["hamburg|amsterdam"]; kept != hours["hamburg|amsterdam"] {
				t.Fatalf("disrupted pass reset the gate: got %v want %v", kept, hours["hamburg|amsterdam"])
			}
			if kept := decode[map[string]json.RawMessage](runner.snapshot, "passenger_fares")["hamburg|amsterdam"]; string(kept) != passengerFareFixture {
				t.Fatalf("disrupted pass dropped the retained payload: %s", kept)
			}
			if since := decode[map[string]float64](runner.snapshot, "_passenger_fare_irrelevant")["hamburg|amsterdam"]; since != 0 {
				t.Fatalf("disrupted pass counted as irrelevance: %v", since)
			}
			// The next good pass must not burst re-reads: the gate still holds.
			// The hour stamp below also re-stamps the ships meta fresh.
			setPassengerCaptureClock(runner, 102, 0)
			if err := runner.refreshPassengers(context.Background()); err != nil {
				t.Fatal(err)
			}
			mu.Lock()
			defer mu.Unlock()
			// Every pass with an occupied port re-reads waiting and offers;
			// the fares count is the consumer-visible gate: exactly one read
			// survives a failed waiting capture or a stale fleet pass.
			want := map[string]int{"/v1/passengers/offers": 2, "/v1/passengers/waiting": 2, "/v1/passengers/fares": 1}
			if failWaiting {
				want["/v1/passengers/offers"] = 3
				want["/v1/passengers/waiting"] = 3
			}
			if !reflect.DeepEqual(counts, want) {
				t.Fatalf("disruption changed the consumer-visible read counts to %v, want %v", counts, want)
			}
		})
	}
}

func TestPassengerCaptureOffersPaginateButOtherReadsDoNot(t *testing.T) {
	at := time.Date(2026, 10, 28, 10, 0, 0, 0, time.UTC)
	offers := make([]gameapi.PassengerOffer, 201)
	waiting := make([]gameapi.PassengerWaitingGroup, 200)
	aboard := make([]gameapi.PassengerAboardGroup, 200)
	fares := make([]gameapi.PassengerFare, 200)
	for index := range offers {
		offers[index] = gameapi.PassengerOffer{ID: fmt.Sprintf("offer-%03d", index), ShipID: "ship-a", PortID: "hamburg", DestPortID: "amsterdam", FarePerHeadPence: 100, MaxBerths: 20, Status: "open", CreatedAt: at}
	}
	for index := range waiting {
		waiting[index] = gameapi.PassengerWaitingGroup{ID: fmt.Sprintf("group-%03d", index), Kind: "travellers", Size: 1, OriginPortID: "hamburg", DestPortID: "amsterdam", BoardBy: at.Add(time.Hour)}
		aboard[index] = gameapi.PassengerAboardGroup{ID: waiting[index].ID, Kind: "travellers", Size: 1, OriginPortID: "hamburg", DestPortID: "amsterdam", FarePerHeadPence: 100, FullFarePence: 100, HoldTons: 1, BoardedAt: &at}
		fares[index] = gameapi.PassengerFare{ID: fmt.Sprintf("fare-%03d", index), Kind: "travellers", Size: 1, OriginPortID: "hamburg", DestPortID: "amsterdam", FarePerHeadPence: 100, AcceptedAt: at}
	}
	var mu sync.Mutex
	counts := make(map[string]int)
	runner := newPassengerCaptureRunner(t, []ShipState{passengerCaptureShip("ship-a", "hamburg", "berthed")}, func(w http.ResponseWriter, req *http.Request) {
		mu.Lock()
		counts[req.URL.Path]++
		mu.Unlock()
		if req.Method != http.MethodGet {
			t.Errorf("passenger capture mutated the game: %s", req.Method)
		}
		query := req.URL.Query()
		switch req.URL.Path {
		case "/v1/passengers/offers":
			if query.Get("open_only") != "true" || query.Get("limit") != "200" || query.Get("ship_id") != "" {
				t.Errorf("routine offers capture must read only open offers: %s", req.URL)
			}
			switch query.Get("cursor") {
			case "":
				_ = json.NewEncoder(w).Encode(offers[:200])
			case "offer-199":
				_ = json.NewEncoder(w).Encode(offers[200:])
			default:
				t.Errorf("unexpected offers cursor: %s", req.URL)
				_, _ = w.Write([]byte(`[]`))
			}
		case "/v1/passengers/waiting":
			if query.Get("port_id") != "hamburg" || len(query) != 1 {
				t.Errorf("waiting is an unpaginated port array: %s", req.URL)
			}
			_ = json.NewEncoder(w).Encode(waiting)
		case "/v1/passengers/aboard":
			if query.Get("ship_id") != "ship-a" || len(query) != 1 {
				t.Errorf("aboard is an unpaginated ship array: %s", req.URL)
			}
			_ = json.NewEncoder(w).Encode(aboard)
		case "/v1/passengers/fares":
			if query.Get("from") != "hamburg" || query.Get("to") != "amsterdam" || query.Get("limit") != "200" || len(query) != 3 {
				t.Errorf("fare capture must remain one bounded public page: %s", req.URL)
			}
			_ = json.NewEncoder(w).Encode(fares)
		default:
			t.Errorf("unexpected passenger read %s", req.URL)
			w.WriteHeader(http.StatusNotFound)
		}
	})
	if err := runner.refreshPassengers(context.Background()); err != nil {
		t.Fatal(err)
	}
	mu.Lock()
	defer mu.Unlock()
	if want := map[string]int{"/v1/passengers/offers": 2, "/v1/passengers/waiting": 1, "/v1/passengers/aboard": 1, "/v1/passengers/fares": 1}; !reflect.DeepEqual(counts, want) {
		t.Fatalf("endpoint pagination/counts %v, want %v", counts, want)
	}
	if got := len(decode[[]gameapi.PassengerOffer](runner.snapshot, "passenger_offers")); got != 201 {
		t.Fatalf("offers lost a page: %d", got)
	}
	if got := len(decode[map[string][]gameapi.PassengerFare](runner.snapshot, "passenger_fares")["hamburg|amsterdam"]); got != 200 {
		t.Fatalf("bounded fare page lost rows: %d", got)
	}
}

func TestPassengerCaptureOptionalFailuresRetainStaleEvidence(t *testing.T) {
	for _, mode := range []string{"not found", "invalid array"} {
		t.Run(mode, func(t *testing.T) {
			runner := newPassengerCaptureRunner(t, []ShipState{passengerCaptureShip("ship-a", "hamburg", "berthed")}, func(w http.ResponseWriter, req *http.Request) {
				if req.URL.Path == "/v1/passengers/fares" {
					t.Error("failed waiting capture must not authorize a fare read")
				}
				if mode == "not found" {
					w.WriteHeader(http.StatusNotFound)
				}
				_, _ = w.Write([]byte(`{}`))
			})
			runner.snapshot["passenger_offers"] = json.RawMessage(passengerOfferFixture)
			runner.snapshot["passenger_waiting"] = encode(map[string]json.RawMessage{"hamburg": json.RawMessage(passengerWaitingFixture)})
			runner.snapshot["passenger_aboard"] = encode(map[string]json.RawMessage{"ship-a": json.RawMessage(passengerAboardFixture)})
			runner.snapshot["_passenger_boarding_ships"] = encode(map[string]bool{"ship-a": true})
			if err := runner.refreshPassengers(context.Background()); err != nil {
				t.Fatalf("optional endpoint failure aborted refresh: %v", err)
			}
			for _, key := range []string{"passenger_offers", "passenger_waiting/hamburg", "passenger_aboard/ship-a"} {
				meta := runner.meta[key]
				if meta.Complete || !meta.Stale || meta.Error == "" || runner.fresh(key) {
					t.Fatalf("failed capture became fresh for %s: %+v", key, meta)
				}
			}
			if got := decode[[]gameapi.PassengerOffer](runner.snapshot, "passenger_offers"); len(got) != 1 || got[0].ID != "offer-a" {
				t.Fatalf("failed offers read erased last-good evidence: %v", got)
			}
			if got := decode[map[string][]gameapi.PassengerWaitingGroup](runner.snapshot, "passenger_waiting")["hamburg"]; len(got) != 1 || got[0].ID != "group-a" {
				t.Fatalf("failed waiting read erased last-good evidence: %v", got)
			}
			if got := decode[map[string][]gameapi.PassengerAboardGroup](runner.snapshot, "passenger_aboard")["ship-a"]; len(got) != 1 || got[0].ID != "group-a" {
				t.Fatalf("failed aboard read erased last-good evidence: %v", got)
			}
		})
	}
}

func TestPassengerZoneObservationsRetainPublicMarqueAggregates(t *testing.T) {
	zero, piracy, count := int64(0), int64(700), int64(3)
	for _, tc := range []struct {
		name        string
		aggregates  string
		wantPiracy  *int64
		wantMarques *int64
	}{
		{"explicit zero", `,"marque_piracy_bps":0,"active_marques":0`, &zero, &zero},
		{"active public marques", `,"marque_piracy_bps":700,"active_marques":3`, &piracy, &count},
		{"omitted aggregates", ``, nil, nil},
		{"null aggregates", `,"marque_piracy_bps":null,"active_marques":null`, nil, nil},
		{"omitted marque count", `,"marque_piracy_bps":700`, &piracy, nil},
		{"omitted piracy aggregate", `,"active_marques":3`, nil, &count},
	} {
		t.Run(tc.name, func(t *testing.T) {
			raw := fmt.Sprintf(`[{"sea_zone_id":"english_channel","net_piracy_bps":200%s}]`, tc.aggregates)
			var decoded []ZoneObservation
			if err := json.Unmarshal([]byte(raw), &decoded); err != nil {
				t.Fatal(err)
			}
			want := ZoneObservation{
				SeaZoneID: "english_channel", NetPiracyBPS: 200,
				MarquePiracyBPS: tc.wantPiracy, ActiveMarques: tc.wantMarques,
			}
			if len(decoded) != 1 || !reflect.DeepEqual(decoded[0], want) {
				t.Fatalf("zone decoder discarded aggregate values or presence: got %+v want %+v", decoded, want)
			}
			runner := newPassengerCaptureRunner(t, nil, func(w http.ResponseWriter, req *http.Request) {
				if req.Method != http.MethodGet || req.URL.Path != "/v1/world/zones" {
					t.Errorf("unexpected zone observation request: %s %s", req.Method, req.URL)
					w.WriteHeader(http.StatusNotFound)
					return
				}
				_, _ = w.Write([]byte(raw))
			})
			if err := runner.captureWave(context.Background(), []fetchTask{{key: "zones", path: "/v1/world/zones", kind: fetchJSON}}, nil); err != nil {
				t.Fatal(err)
			}
			in := runner.planningInputFrom(ShipState{}, runner.state())
			if len(in.Zones) != 1 || !reflect.DeepEqual(in.Zones[0], want) || !in.fresh("zones") {
				t.Fatalf("snapshot-to-planning mapping lost public marque evidence: zones=%+v meta=%+v", in.Zones, runner.meta["zones"])
			}
		})
	}
}

func TestPassengerCaptureSkipsUnboundSailingOrAbsentFleet(t *testing.T) {
	for _, mode := range []string{"absent", "sailing", "sunk", "stale-bound", "sailing-hold", "sailing-hint", "sailing-retained-offer"} {
		t.Run(mode, func(t *testing.T) {
			ships := []ShipState{}
			if mode != "absent" {
				ship := passengerCaptureShip("ship-a", "", "sailing")
				if mode == "sunk" {
					ship.Ship.Status = "sunk"
				}
				if mode == "sailing-hold" || mode == "stale-bound" {
					ship.PassengerHoldTons = 1
				}
				ships = append(ships, ship)
			}
			offers, aboard := 0, 0
			runner := newPassengerCaptureRunner(t, ships, func(w http.ResponseWriter, req *http.Request) {
				switch req.URL.Path {
				case "/v1/passengers/offers":
					offers++
				case "/v1/passengers/aboard":
					aboard++
				default:
					t.Errorf("unwarranted passenger read %s", req.URL)
				}
				_, _ = w.Write([]byte(`[]`))
			})
			if mode == "stale-bound" {
				meta := runner.meta["ships"]
				meta.Stale = true
				runner.meta["ships"] = meta
			}
			if mode == "sailing-hint" {
				runner.snapshot["_passenger_boarding_ships"] = encode(map[string]bool{"ship-a": true})
			}
			if mode == "sailing-retained-offer" {
				runner.snapshot["passenger_offers"] = encode([]gameapi.PassengerOffer{{ID: "open-offer", ShipID: "ship-a", Status: "open"}})
			}
			if err := runner.refreshPassengers(context.Background()); err != nil {
				t.Fatal(err)
			}
			wantOffers, wantAboard := 0, 0
			if mode == "sailing-hold" || mode == "sailing-hint" {
				wantOffers, wantAboard = 1, 1
			}
			if mode == "sailing-retained-offer" {
				wantOffers = 1
			}
			if offers != wantOffers || aboard != wantAboard {
				t.Fatalf("passenger reads offers%d aboard%d, want %d/%d", offers, aboard, wantOffers, wantAboard)
			}
		})
	}
}

func TestPassengerRESTBoardingKeepsArrivalCaptureWarrant(t *testing.T) {
	ship := passengerCaptureShip("ship-a", "", "sailing")
	ship.PassengerHoldTons = 7
	reads := 0
	runner := newPassengerCaptureRunner(t, []ShipState{ship}, func(w http.ResponseWriter, req *http.Request) {
		switch req.URL.Path {
		case "/v1/passengers/offers", "/v1/passengers/waiting":
			_, _ = w.Write([]byte(`[]`))
		case "/v1/passengers/aboard":
			reads++
			if reads == 1 {
				_, _ = w.Write([]byte(passengerAboardFixture))
			} else {
				_, _ = w.Write([]byte(`[]`))
			}
		default:
			t.Errorf("unexpected read %s", req.URL)
		}
	})
	if err := runner.refreshPassengers(context.Background()); err != nil {
		t.Fatal(err)
	}
	if hints := decode[map[string]bool](runner.snapshot, "_passenger_boarding_ships"); !hints[ship.Ship.ID] {
		t.Fatal("REST-observed boarding lost its durable arrival capture warrant")
	}
	ship.Ship.CurrentPortID, ship.Ship.Status, ship.PassengerHoldTons = "amsterdam", "anchored_roadstead", 0
	runner.snapshot["ships"] = encode([]ShipState{ship})
	setPassengerCaptureClock(runner, 101, 0)
	if err := runner.refreshPassengers(context.Background()); err != nil {
		t.Fatal(err)
	}
	aboard := decode[map[string][]gameapi.PassengerAboardGroup](runner.snapshot, "passenger_aboard")
	if reads != 2 || aboard[ship.Ship.ID] == nil || len(aboard[ship.Ship.ID]) != 0 || !runner.fresh("passenger_aboard/"+ship.Ship.ID) {
		t.Fatalf("arrival never authoritatively observed an empty hold: reads%d %+v", reads, aboard)
	}
}
