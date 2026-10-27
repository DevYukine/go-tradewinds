package engine

import (
	"math"
	"reflect"
	"testing"

	"github.com/DevYukine/go-tradewinds/internal/gameapi"
)

func TestRndDecision(t *testing.T) {
	cannotStart := "net worth below entry requirement"
	for _, test := range []struct {
		name   string
		status gameapi.RndStatus
		want   string
	}{
		{
			name:   "not eligible with reason",
			status: gameapi.RndStatus{CannotStartReason: &cannotStart},
			want:   "R&D waits: net worth below entry requirement",
		},
		{
			name:   "not eligible with nil reason",
			status: gameapi.RndStatus{},
			want:   "R&D waits: not eligible",
		},
		{
			name:   "not eligible with empty reason",
			status: gameapi.RndStatus{CannotStartReason: new(string)},
			want:   "R&D waits: not eligible",
		},
		{
			name:   "eligible remains observation only",
			status: gameapi.RndStatus{CanStart: true},
			want:   "R&D is available but not started: design value is unknown and fleet growth competes for the same cash",
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			test.status.PlayerNetWorthPence = 101_000_000
			test.status.EntryNetWorthPence = 200_000_000
			test.status.StartFeePence = 5_000_000
			test.status.UpkeepPerGameDayPence = 45_000
			reason, details := rndDecision(test.status)
			if reason != test.want {
				t.Fatalf("reason = %q, want %q", reason, test.want)
			}
			wantDetails := map[string]any{
				"can_start":                    test.status.CanStart,
				"cannot_start_reason":          test.status.CannotStartReason,
				"player_net_worth_pence":       int64(101_000_000),
				"entry_net_worth_pence":        int64(200_000_000),
				"start_fee_pence":              int64(5_000_000),
				"upkeep_per_game_day_pence":    int64(45_000),
				"estimated_project_game_days":  int64(150),
				"estimated_project_cost_pence": int64(11_750_000),
				"no_start_reason":              "design value is unknown and fleet growth competes for the same cash",
			}
			if !reflect.DeepEqual(details, wantDetails) {
				t.Fatalf("details = %#v, want %#v", details, wantDetails)
			}
		})
	}
}

func TestRndDecisionEstimatedProjectCost(t *testing.T) {
	for _, test := range []struct {
		name   string
		status gameapi.RndStatus
		want   int64
	}{
		{
			name: "start fee plus 150 game days of upkeep",
			status: gameapi.RndStatus{
				CanStart:              true,
				StartFeePence:         5_000_000_000,
				UpkeepPerGameDayPence: 45_000_000,
			},
			want: 11_750_000_000,
		},
		{
			name: "ineligible still reports estimated cost",
			status: gameapi.RndStatus{
				StartFeePence:         12_345,
				UpkeepPerGameDayPence: 67,
			},
			want: 22_395,
		},
		{
			name:   "zero upkeep retains start fee",
			status: gameapi.RndStatus{StartFeePence: 12_345},
			want:   12_345,
		},
		{
			name:   "multiplication overflow saturates",
			status: gameapi.RndStatus{UpkeepPerGameDayPence: math.MaxInt64/150 + 1},
			want:   math.MaxInt64,
		},
		{
			name:   "addition overflow saturates",
			status: gameapi.RndStatus{StartFeePence: math.MaxInt64 - 149, UpkeepPerGameDayPence: 1},
			want:   math.MaxInt64,
		},
		{
			name:   "maximum representable estimate",
			status: gameapi.RndStatus{StartFeePence: math.MaxInt64 - 150, UpkeepPerGameDayPence: 1},
			want:   math.MaxInt64,
		},
		{
			name:   "negative start fee is not a cheap project",
			status: gameapi.RndStatus{StartFeePence: -1, UpkeepPerGameDayPence: 1},
			want:   math.MaxInt64,
		},
		{
			name:   "negative upkeep is not a cheap project",
			status: gameapi.RndStatus{StartFeePence: 12_345, UpkeepPerGameDayPence: -1},
			want:   math.MaxInt64,
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			_, details := rndDecision(test.status)
			if got := details["estimated_project_cost_pence"]; got != test.want {
				t.Fatalf("estimated_project_cost_pence = %#v, want %d", got, test.want)
			}
		})
	}
}
