package gameapi

// RndStatus is read-only eligibility and project pricing for this player.
type RndStatus struct {
	CanStart              bool    `json:"can_start"`
	CannotStartReason     *string `json:"cannot_start_reason"`
	PlayerNetWorthPence   int64   `json:"player_net_worth_pence"`
	EntryNetWorthPence    int64   `json:"entry_net_worth_pence"`
	StartFeePence         int64   `json:"start_fee_pence"`
	UpkeepPerGameDayPence int64   `json:"upkeep_per_game_day_pence"`
}
