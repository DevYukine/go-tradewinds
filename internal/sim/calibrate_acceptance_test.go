package sim

import (
	"encoding/json"
	"fmt"
	"testing"
	"time"

	"github.com/DevYukine/go-tradewinds/internal/store"
)

func TestCalibrateAcceptanceInsufficientSamplesUseHonestPrior(t *testing.T) {
	for _, samples := range [][]float64{nil, {0.1, 0.2, 0.3, 0.4}} {
		fit := RegenerationQuantiles(samples)
		if !fit.Uncalibrated || fit.Samples != len(samples) || fit.Conservative != 0 || fit.Base != 0.5 || fit.Optimistic != 1 {
			t.Fatalf("insufficient observations invented calibrated parameters: %+v", fit)
		}
	}
	tape, from, until := simAcceptanceRunTape(t)
	fit, err := Calibrate(tape, from, until)
	if err != nil {
		t.Fatal(err)
	}
	if !fit.Uncalibrated {
		t.Fatalf("tape without fitting samples was labelled calibrated: %+v", fit)
	}
	speed, exists := fit.Speeds["hoy"]
	if !exists || speed.Empty != 1 || speed.Laden != 0.8 {
		t.Fatalf("known hull did not receive empty/laden insufficient-sample speed prior: %+v", fit.Speeds)
	}
}

func simAcceptanceCalibrationTape(t *testing.T, extraFill, lateRequote bool) (*Tape, time.Time, time.Time) {
	t.Helper()
	at := time.Date(2026, 10, 20, 0, 0, 0, 0, time.UTC)
	before, after := map[string]string{}, map[string]string{}
	var fills []map[string]any
	for i := range 5 {
		port := fmt.Sprintf("calibration-%d", i)
		book := simAcceptanceBook(t, "npc-before", 66)
		book.PortID = port
		data, err := json.Marshal(book)
		if err != nil {
			t.Fatal(err)
		}
		before["books/"+port] = string(data)
		book.Books[0].Depth.Asks[0].OrderID = "npc-after"
		book.Books[0].Depth.Asks[0].RemainingQuantity = 51
		data, err = json.Marshal(book)
		if err != nil {
			t.Fatal(err)
		}
		after["books/"+port] = string(data)
		fills = append(fills, map[string]any{"id": port + "-fill", "order_id": "npc-before", "port_id": port, "commodity_id": "cloth", "side": "buy", "quantity": 30, "price_pence": 90, "created_at": at.Add(time.Minute)})
		if extraFill && i == 0 {
			fills = append(fills, map[string]any{"id": "second-own-fill", "order_id": "npc-before", "port_id": port, "commodity_id": "cloth", "side": "buy", "quantity": 1, "price_pence": 90, "created_at": at.Add(2 * time.Minute)})
		}
	}
	data, err := json.Marshal(fills)
	if err != nil {
		t.Fatal(err)
	}
	after["fills"] = string(data)
	elapsed := 6 * time.Minute
	if lateRequote {
		elapsed = 13 * time.Minute
	}
	frames := []store.ReplayFrame{simAcceptanceFrame(t, at, 100, before), simAcceptanceFrame(t, at.Add(elapsed), 100+int(elapsed/time.Minute), after)}
	return &Tape{Frames: frames}, at, at.Add(20 * time.Minute)
}

func TestCalibrateAcceptanceSamplesExcludeInterveningFillsAndLongGaps(t *testing.T) {
	for _, tc := range []struct {
		name        string
		extra, late bool
		samples     int
	}{
		{name: "five independent NPC fills", samples: 5},
		{name: "intervening own fill excludes contaminated pair", extra: true, samples: 4},
		{name: "more than two requote periods excludes sample", late: true, samples: 0},
	} {
		t.Run(tc.name, func(t *testing.T) {
			tape, from, until := simAcceptanceCalibrationTape(t, tc.extra, tc.late)
			fit, err := Calibrate(tape, from, until)
			if err != nil {
				t.Fatal(err)
			}
			if fit.Samples != tc.samples || fit.Uncalibrated != (tc.samples < 5) {
				t.Fatalf("calibration admitted contaminated/future/too-old observations: %+v, want n=%d", fit, tc.samples)
			}
			if tc.samples == 5 && (fit.Conservative != 0.5 || fit.Base != 0.5 || fit.Optimistic != 0.5) {
				t.Fatalf("(152-(167-30))/30 must fit regen 0.5: %+v", fit)
			}
		})
	}
}

func TestCalibrateAcceptanceDoesNotFitEvaluationFrames(t *testing.T) {
	tape, from, _ := simAcceptanceCalibrationTape(t, false, false)
	fit, err := Calibrate(tape, from, from.Add(5*time.Minute))
	if err != nil {
		t.Fatal(err)
	}
	if fit.Samples != 0 || !fit.Uncalibrated {
		t.Fatalf("fit read the evaluation-window requote: %+v", fit)
	}
}
