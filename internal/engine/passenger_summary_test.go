package engine

import (
	"context"
	"encoding/json"
	"net/http"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/DevYukine/go-tradewinds/internal/gameapi"
	"github.com/DevYukine/go-tradewinds/internal/store"
	"go.uber.org/zap"
)

const passengerFareSummaryFixture = `{"bands":[{"min_nm":0,"max_nm":100,"fares":500,"median_fare_per_head_pence":1000},{"min_nm":100,"max_nm":null,"fares":5,"median_fare_per_head_pence":20000}],"unmet":[{"origin_port_id":"hamburg","dest_port_id":"amsterdam","heads":3000000000,"groups":7}]}`

func TestPassengerSparseLongRouteCeilingUsesDistanceBand(t *testing.T) {
	for _, tc := range []struct {
		name    string
		samples int
		stale   bool
		fare    int64
		freight bool
		want    int64
	}{
		{"no lane samples", 0, false, 1_000, false, 20_000},
		{"sparse lane samples", 1, false, 1_000, false, 20_000},
		{"sparse outlier", 1, false, 1_000_000, false, 20_000},
		{"stale lane samples", 3, true, 1_000, false, 20_000},
		{"current lane samples", 3, false, 1_000, false, 1_500},
		{"sparse with live freight analog", 1, false, 1_000_000, true, 24_000},
	} {
		t.Run(tc.name, func(t *testing.T) {
			in := passengerInputFixture()
			in.Catalog.Ports[1].IsHub = true
			key := routeKey("origin", "destination", in.Ship.Ship.ShipClass)
			route := in.Routes[key]
			route.TotalDistanceNM = 1_200
			for i := range route.Legs {
				route.Legs[i].DistanceNM = 600
			}
			in.Routes[key] = route
			if tc.freight {
				in.Contracts = []Contract{{ContractType: "freight", OriginPortID: "origin", DestPortID: "destination", QuantityRequired: 10, RewardPence: 60_000}}
			}
			accepted := in.Now.Add(-gameDuration(1.0/24, in.Clock.Calendar.TimeScale))
			if tc.stale {
				accepted = in.Now.Add(-gameDuration(11.0/24, in.Clock.Calendar.TimeScale))
			}
			fares := make([]gameapi.PassengerFare, tc.samples)
			for i := range fares {
				fares[i] = gameapi.PassengerFare{OriginPortID: "origin", DestPortID: "destination", FarePerHeadPence: tc.fare, AcceptedAt: accepted}
			}
			var summary gameapi.PassengerFareSummary
			if err := json.Unmarshal([]byte(passengerFareSummaryFixture), &summary); err != nil {
				t.Fatal(err)
			}
			groups := []gameapi.PassengerWaitingGroup{{ID: "long-route", Kind: "travellers", Size: 10, OriginPortID: "origin", DestPortID: "destination", BoardBy: in.Now.Add(time.Hour)}}
			quote, err := quotePassage(in, "destination", groups, fares, summary, nil)
			if err != nil || quote.Ceiling != tc.want || quote.Ceiling < quote.Floor {
				t.Fatalf("long-route ceiling was anchored to short-hop fares: quote=%+v error=%v want=%d", quote, err, tc.want)
			}
		})
	}
}

func TestPassengerQuoteRequiresMinimumOfferLifetime(t *testing.T) {
	in := passengerInputFixture()
	deadline := in.Now.Add(gameDuration(6.0/24, in.Clock.Calendar.TimeScale))
	group := gameapi.PassengerWaitingGroup{ID: "deadline", Kind: "travellers", Size: 10, OriginPortID: "origin", DestPortID: "destination", BoardBy: deadline.Add(-time.Nanosecond)}
	if _, err := quotePassage(in, "destination", []gameapi.PassengerWaitingGroup{group}, nil, gameapi.PassengerFareSummary{}, nil); err == nil {
		t.Fatal("group leaving before six game hours admitted an offer")
	}
	group.BoardBy = deadline
	if quote, err := quotePassage(in, "destination", []gameapi.PassengerWaitingGroup{group}, nil, gameapi.PassengerFareSummary{}, nil); err != nil || quote.Ceiling != quote.Floor {
		t.Fatalf("six-hour floor offer rejected: %+v %v", quote, err)
	}
}

func TestPassengerFareSummaryCaptureBoundsNullableBands(t *testing.T) {
	var summary gameapi.PassengerFareSummary
	if err := validatePassengerCapture("passenger_fare_summary", json.RawMessage(passengerFareSummaryFixture)); err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal([]byte(passengerFareSummaryFixture), &summary); err != nil {
		t.Fatal(err)
	}
	if len(summary.Bands) != 2 || summary.Bands[1].MaxNM != nil || summary.Bands[1].MedianFarePerHeadPence == nil ||
		*summary.Bands[1].MedianFarePerHeadPence != 20_000 {
		t.Fatalf("documented nullable/int64 summary fields lost: %+v", summary)
	}
	if err := validatePassengerCapture("passenger_fare_summary", json.RawMessage(`{"bands":[{"min_nm":0,"max_nm":null,"fares":0,"median_fare_per_head_pence":null}],"unmet":[]}`)); err != nil {
		t.Fatalf("empty band median cannot be null: %v", err)
	}
	for _, raw := range []string{
		`null`, `[]`, `{}`, `{"bands":null,"unmet":[]}`,
		`{"bands":[{"min_nm":100,"max_nm":100,"fares":1,"median_fare_per_head_pence":1}],"unmet":[]}`,
		`{"bands":[{"min_nm":0,"max_nm":null,"fares":1,"median_fare_per_head_pence":-1}],"unmet":[]}`,
		`{"bands":[],"unmet":[],"padding":"` + strings.Repeat("x", passengerFareSummaryMaxBytes) + `"}`,
	} {
		if err := validatePassengerCapture("passenger_fare_summary", json.RawMessage(raw)); err == nil {
			t.Fatalf("invalid or unbounded summary admitted: %.160s", raw)
		}
	}
	summary.Bands = make([]gameapi.PassengerFareBand, passengerFareSummaryMaxBands+1)
	if err := validatePassengerCapture("passenger_fare_summary", encode(summary)); err == nil {
		t.Fatal("unbounded band history retained")
	}
}

func TestPassengerFareSummaryCaptureKeepsLastGoodAndAttemptGateAcrossRestart(t *testing.T) {
	var mu sync.Mutex
	reads := 0
	var summary gameapi.PassengerFareSummary
	if err := json.Unmarshal([]byte(passengerFareSummaryFixture), &summary); err != nil {
		t.Fatal(err)
	}
	expected := string(encode(summary))
	runner := newPassengerCaptureRunner(t, []ShipState{passengerCaptureShip("ship-a", "hamburg", "berthed")}, func(w http.ResponseWriter, req *http.Request) {
		switch req.URL.Path {
		case "/v1/passengers/offers", "/v1/passengers/aboard":
			_, _ = w.Write([]byte(`[]`))
		case "/v1/passengers/waiting":
			_, _ = w.Write([]byte(passengerWaitingFixture))
		case "/v1/passengers/fares":
			_, _ = w.Write([]byte(`[]`))
		case "/v1/passengers/fares/summary":
			if req.Method != http.MethodGet || req.Header.Get("Authorization") != "" || req.URL.RawQuery != "" {
				t.Errorf("summary is not one unauthenticated public read: %s %s", req.Method, req.URL)
			}
			mu.Lock()
			reads++
			attempt := reads
			mu.Unlock()
			switch attempt {
			case 1:
				// The unused all-world unmet history must not crowd out bands.
				_, _ = w.Write([]byte(strings.TrimSuffix(expected, "}") + `,"unmet":[` +
					strings.Repeat(`{"origin_port_id":"hamburg","dest_port_id":"amsterdam","heads":10,"groups":1},`, 999) +
					`{"origin_port_id":"hamburg","dest_port_id":"amsterdam","heads":10,"groups":1}]}`))
			case 2:
				w.WriteHeader(http.StatusNotFound)
			case 3:
				_, _ = w.Write([]byte(`{"bands":null,"unmet":[]}`))
			default:
				_, _ = w.Write([]byte(passengerFareSummaryFixture))
			}
		default:
			t.Errorf("unexpected summary fixture read: %s", req.URL)
			w.WriteHeader(http.StatusNotFound)
		}
	})
	for _, observation := range []struct {
		hour   int64
		minute int32
		reads  int
		stale  bool
	}{{100, 0, 1, false}, {109, 59, 1, false}, {110, 1, 2, true}, {119, 59, 2, true}, {120, 2, 3, true}, {130, 3, 4, false}} {
		setPassengerCaptureClock(runner, observation.hour, observation.minute)
		if err := runner.refreshPassengers(context.Background()); err != nil {
			t.Fatal(err)
		}
		mu.Lock()
		got := reads
		mu.Unlock()
		if got != observation.reads {
			t.Fatalf("hour %d:%02d read summary %d times, want %d", observation.hour, observation.minute, got, observation.reads)
		}
		if string(runner.snapshot["passenger_fare_summary"]) != expected || runner.meta["passenger_fare_summary"].Stale != observation.stale {
			t.Fatalf("failed summary erased or freshened last-good: payload=%s meta=%+v", runner.snapshot["passenger_fare_summary"], runner.meta["passenger_fare_summary"])
		}
		// Reconstruct every pass: neither a failed attempt nor an oversized
		// payload may buy another request by losing its persisted gate.
		saved := runner.snapshot
		saved["_meta"] = encode(runner.meta)
		restarted := New(runner.api, nil, store.Account{}, zap.NewNop(), Options{})
		restarted.snapshot, restarted.generation = saved, "summary-restart"
		restarted.meta = decode[map[string]DatasetMeta](saved, "_meta")
		runner = restarted
	}
}
