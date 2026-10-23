package engine

import (
	"context"
	"strings"
	"time"

	"github.com/DevYukine/go-tradewinds/internal/store"
)

// acquiredRevenue uses the actual partial IOC quantity. The successful purchase
// stage is never reopened when a charter closes or destination evidence ages.
func acquiredRevenue(in PlanningInput, plan Plan, bought int64, obligations []freightObligation) (int64, string) {
	var revenue int64
	for _, obligation := range obligations {
		if !obligation.Settled {
			revenue += obligation.RewardPence
		}
	}
	if strings.HasPrefix(plan.Kind, "charter") && bought > 0 {
		var charter *Contract
		for i := range in.Contracts {
			if in.Contracts[i].ID == plan.ContractID {
				charter = &in.Contracts[i]
				break
			}
		}
		if charter == nil || charter.Status != "open" || charter.Stale || charter.RewardPerTonPence == nil || charter.RemainingQuantity <= 0 {
			if revenue > 0 {
				return revenue, "charter closed; only accepted freight reward remains counted"
			}
			return 0, "charter is no longer live after procurement"
		}
		hull, exists := findHull(in.Catalog, in.Ship.Ship.ShipClass)
		if !exists || hull.BaseSpeedCentiknots <= 0 || in.Clock.Calendar.TimeScale <= 0 {
			return 0, "cannot establish a fresh laden arrival before charter expiry"
		}
		route := in.Routes[routeKey(plan.Origin, plan.Destination, hull.ID)]
		if route.TotalDistanceNM <= 0 {
			return 0, "current charter route is missing after procurement"
		}
		days := float64(route.TotalDistanceNM)/(float64(hull.BaseSpeedCentiknots)/100*0.8)/24 + 1
		latestArrival := in.Now.Add(time.Duration(days*86_400/float64(in.Clock.Calendar.TimeScale)) * time.Second)
		if charter.ExpiresAt != nil && !latestArrival.Before(*charter.ExpiresAt) {
			return revenue, "charter expiry no longer has a game-day delivery buffer"
		}
		revenue += min(bought, charter.RemainingQuantity) * *charter.RewardPerTonPence
	}
	if strings.HasPrefix(plan.Kind, "spot") && bought > 0 {
		gross, _, _, _, reason := destinationRevenue(in, plan.Destination, plan.Commodity, bought)
		if reason != "" {
			return revenue, reason
		}
		revenue += gross
	}
	return revenue, ""
}

// acquiredNet rechecks the original voyage against actual paid procurement and
// current receivable income. Uncalibrated stress is not an operating expense.
func acquiredNet(data *missionData, revenue int64) int64 {
	costs := data.AcquisitionPence + data.FeesPence +
		max(data.ProvisionsPence, data.Plan.Economics.ProvisionsPence) +
		data.Plan.Economics.BerthDuesPence +
		data.Plan.Economics.UpkeepPence +
		data.Plan.Economics.TollPence
	return revenue - costs
}

func (r *Runner) unwindProcurement(ctx context.Context, ship ShipState, mission *store.Mission, data *missionData, reason string) (bool, error) {
	if len(r.activeFreight(ship.Ship.ID)) > 0 {
		return false, r.hold(ctx, ship.Ship.ID, "acquired cargo lost its profit case but sealed obligations prohibit diversion: "+reason)
	}
	if err := r.decision(ctx, ship.Ship.ID, "procurement_unwind", "procurement is complete; compare measured local bids and unwind instead of repeating a purchase or sailing against stale income: "+reason, data.Plan); err != nil {
		return false, err
	}
	data.Plan.Kind = "aborted"
	data.Plan.Reason = reason
	data.Plan.Origin = ""
	data.Plan.Destination = ship.Ship.CurrentPortID
	data.Eligible = make(map[string]int64)
	mission.Destination = ship.Ship.CurrentPortID
	mission.Phase = "arrival"
	return true, r.saveMission(ctx, mission, data)
}
