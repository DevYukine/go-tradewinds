package engine

import (
	"time"

	"github.com/DevYukine/go-tradewinds/internal/gameapi"
)

const rndObservationInterval = 30 * time.Minute

const rndEstimatedProjectGameDays int64 = 150

func rndDecision(status gameapi.RndStatus) (string, map[string]any) {
	const noStart = "design value is unknown and fleet growth competes for the same cash"
	reason := "R&D is available but not started: " + noStart
	if !status.CanStart {
		unavailable := "not eligible"
		if status.CannotStartReason != nil && *status.CannotStartReason != "" {
			unavailable = *status.CannotStartReason
		}
		reason = "R&D waits: " + unavailable
	}
	return reason, map[string]any{
		"can_start":                    status.CanStart,
		"cannot_start_reason":          status.CannotStartReason,
		"player_net_worth_pence":       status.PlayerNetWorthPence,
		"entry_net_worth_pence":        status.EntryNetWorthPence,
		"start_fee_pence":              status.StartFeePence,
		"upkeep_per_game_day_pence":    status.UpkeepPerGameDayPence,
		"estimated_project_game_days":  rndEstimatedProjectGameDays,
		"estimated_project_cost_pence": growthCashTotal(status.StartFeePence, checkedPenceProduct(rndEstimatedProjectGameDays, status.UpkeepPerGameDayPence)),
		"no_start_reason":              noStart,
	}
}
