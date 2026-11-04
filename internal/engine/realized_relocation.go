package engine

import (
	"fmt"
	"math"
	"sort"
	"time"

	"github.com/DevYukine/go-tradewinds/internal/store"
)

const realizedRelocationWindow = 72 * time.Hour
const realizedRelocationPriceSource = "recent realized spot lane net; remote executable depth remains unknown"

type realizedSourcingLane struct {
	Origin       string    `json:"origin"`
	Destination  string    `json:"destination"`
	Commodity    string    `json:"commodity"`
	QuantityTons int64     `json:"quantity_tons"`
	NetPence     int64     `json:"net_pence"`
	GameDays     float64   `json:"game_days"`
	Samples      int64     `json:"samples"`
	CompletedAt  time.Time `json:"completed_at"`
	RelocatedAt  time.Time `json:"relocated_at,omitempty"`
}

func realizedRelocationKey(source, destination, commodity string) string {
	return source + "|" + destination + "|" + commodity + "|realized"
}

// Include losing receipts in the lane mean. A past charter or freight award is
// finite demand, so it cannot finance an uncontracted repeat voyage.
func recentRealizedSourcingLanes(rows []store.Settlement, now time.Time) ([]realizedSourcingLane, error) {
	lanes := make(map[string]realizedSourcingLane)
	for _, row := range rows {
		if row.Kind != "spot" || !row.CostsKnown || row.ProceedsUnknown || row.QuantityTons <= 0 || row.GameSeconds <= 0 ||
			row.Origin == "" || row.Destination == "" || row.Origin == row.Destination || row.Commodity == "" ||
			row.StartedAt.IsZero() || row.CompletedAt.Before(row.StartedAt) || row.CompletedAt.After(now) || row.CompletedAt.Before(now.Add(-realizedRelocationWindow)) {
			continue
		}
		net := row.ProceedsPence
		for _, cost := range []int64{row.AcquisitionPence, row.FeesPence, row.ProvisionsPence, row.ExpensesPence} {
			if cost < 0 || net < math.MinInt64+cost {
				return nil, fmt.Errorf("realized sourcing lane net exceeds integer-pence bounds")
			}
			net -= cost
		}
		key := realizedRelocationKey(row.Origin, row.Destination, row.Commodity)
		lane := lanes[key]
		if net > 0 && lane.NetPence > math.MaxInt64-net || net < 0 && lane.NetPence < math.MinInt64-net || lane.QuantityTons > math.MaxInt64-row.QuantityTons {
			return nil, fmt.Errorf("realized sourcing lane aggregate exceeds integer-pence bounds")
		}
		lane.Origin, lane.Destination, lane.Commodity = row.Origin, row.Destination, row.Commodity
		lane.NetPence += net
		lane.QuantityTons += row.QuantityTons
		lane.GameDays += float64(row.GameSeconds) / 86_400
		lane.Samples++
		lane.CompletedAt = maxTime(lane.CompletedAt, row.CompletedAt)
		lanes[key] = lane
	}
	for _, row := range rows {
		if row.Kind != "relocation" || row.Origin == row.Destination || row.CompletedAt.After(now) {
			continue
		}
		if lane, exists := lanes[row.Lane]; exists && row.Destination == lane.Origin && row.Commodity == lane.Commodity {
			lane.RelocatedAt = maxTime(lane.RelocatedAt, row.CompletedAt)
			lanes[row.Lane] = lane
		}
	}
	var result []realizedSourcingLane
	for _, lane := range lanes {
		if lane.Samples < 2 || lane.NetPence <= 0 {
			continue
		}
		if result == nil {
			result = make([]realizedSourcingLane, 0, len(lanes))
		}
		lane.NetPence /= lane.Samples
		lane.QuantityTons = ceilRatio(lane.QuantityTons, 1, lane.Samples)
		lane.GameDays /= float64(lane.Samples)
		result = append(result, lane)
	}
	sort.Slice(result, func(i, j int) bool {
		a, b := result[i], result[j]
		return realizedRelocationKey(a.Origin, a.Destination, a.Commodity) < realizedRelocationKey(b.Origin, b.Destination, b.Commodity)
	})
	return result, nil
}

func isRealizedRelocation(plan Plan) bool {
	return plan.PriceSource == realizedRelocationPriceSource
}

func realizedRelocationPlan(in PlanningInput, lane realizedSourcingLane) Plan {
	plan := Plan{Kind: "relocation", ShipID: in.Ship.Ship.ID, Origin: in.Ship.Ship.CurrentPortID,
		Destination: lane.Origin, Commodity: lane.Commodity, PriceSource: realizedRelocationPriceSource}
	if !in.SourcingIdleEligible {
		return reject(plan, "realized-lane relocation waits for six coherent idle game hours")
	}
	if lane.Samples < 2 || lane.NetPence <= 0 || lane.QuantityTons <= 0 || lane.GameDays <= 0 || lane.CompletedAt.IsZero() || lane.CompletedAt.After(in.Now) || lane.CompletedAt.Before(in.Now.Add(-realizedRelocationWindow)) {
		return reject(plan, "realized-lane relocation requires two recent known settlements with positive mean lane net")
	}
	if !lane.RelocatedAt.IsZero() && !lane.CompletedAt.After(lane.RelocatedAt) {
		return reject(plan, "realized lane has no newer settlement since its last relocation")
	}
	source, exists := findPort(in.Catalog, lane.Origin)
	if !exists || source.Phase > in.Clock.UnlockedPhase {
		return reject(plan, "realized source port is missing or phase locked")
	}
	if destination, exists := findPort(in.Catalog, lane.Destination); !exists || destination.Phase > in.Clock.UnlockedPhase {
		return reject(plan, "realized lane destination is missing or phase locked")
	}
	hull, _ := findHull(in.Catalog, in.Ship.Ship.ShipClass)
	if route, exists := in.Routes[routeKey(lane.Origin, lane.Destination, hull.ID)]; exists {
		if err := ValidateRoute(route, hull, in.Catalog.SeaZones); err != nil {
			return reject(plan, err.Error())
		}
	}
	net := realizedRelocationNet(in, lane)
	if net == math.MaxInt64 {
		return reject(plan, "realized lane sizing exceeds integer-pence bounds")
	}
	plan.Continuation = &Continuation{Destination: lane.Destination, NetPence: net, GameDays: lane.GameDays}
	plan = scoutExposure(in, plan)
	if !plan.Accepted {
		return plan
	}
	bounded := in
	bounded.ReserveByRegion = sourcingReserves(in, source)
	cost := -plan.Economics.ExpectedNetPence
	if cost >= net {
		return reject(plan, "relocation cost does not leave positive net from the measured realized lane")
	}
	if allowance := sourcingAllowance(bounded); cost > allowance {
		return reject(plan, sourcingBudgetReason(bounded, cost, allowance))
	}
	plan.Reason = fmt.Sprintf("recent realized %s to %s %s mean net %dp minus relocation cost %dp leaves %dp estimated continuation net; fresh cargo admission remains required", lane.Origin, lane.Destination, lane.Commodity, net, cost, net-cost)
	return plan
}

func realizedRelocationCandidates(in PlanningInput, blocked map[string]string) []Plan {
	var plans []Plan
	for _, lane := range in.SourcingProbeHistory.RealizedLanes {
		if lane.Origin == in.Ship.Ship.CurrentPortID {
			continue
		}
		plan := realizedRelocationPlan(in, lane)
		if reason := blocked[lane.Origin]; reason != "" {
			plan = reject(plan, reason)
		}
		plans = append(plans, plan)
	}
	sortRealizedRelocations(plans)
	return plans
}

func sortRealizedRelocations(plans []Plan) {
	sort.SliceStable(plans, func(i, j int) bool {
		a, b := plans[i], plans[j]
		if a.Accepted != b.Accepted {
			return a.Accepted
		}
		if a.Accepted {
			aRate := float64(a.Continuation.NetPence+a.Economics.ExpectedNetPence) / (a.Continuation.GameDays + a.Economics.GameDays)
			bRate := float64(b.Continuation.NetPence+b.Economics.ExpectedNetPence) / (b.Continuation.GameDays + b.Economics.GameDays)
			if aRate != bRate {
				return aRate > bRate
			}
		}
		return realizedRelocationKey(a.Destination, "", a.Commodity) < realizedRelocationKey(b.Destination, "", b.Commodity)
	})
}

func validateRealizedRelocation(in PlanningInput, plan Plan) Plan {
	if !plan.Accepted {
		return plan
	}
	hull, _ := findHull(in.Catalog, in.Ship.Ship.ShipClass)
	if _, err := sourcingRouteReady(in, plan.Destination, plan.Continuation.Destination, hull); err != nil {
		return reject(plan, err.Error())
	}
	return plan
}

func realizedRelocationPlans(in PlanningInput, blocked map[string]string) []Plan {
	plans := realizedRelocationCandidates(in, blocked)
	for i := range plans {
		plans[i] = validateRealizedRelocation(in, plans[i])
	}
	sortRealizedRelocations(plans)
	return plans
}

func currentRealizedRelocation(in PlanningInput, plan Plan) Plan {
	if plan.Continuation != nil {
		for _, lane := range in.SourcingProbeHistory.RealizedLanes {
			if lane.Origin == plan.Destination && lane.Destination == plan.Continuation.Destination && lane.Commodity == plan.Commodity {
				return validateRealizedRelocation(in, realizedRelocationPlan(in, lane))
			}
		}
	}
	return reject(plan, "realized relocation no longer has recent profitable settlement evidence")
}

func realizedRelocationNet(in PlanningInput, lane realizedSourcingLane) int64 {
	// Never scale a historical lot up to a larger ship. Smaller ships receive
	// only their proportional share of that measured net.
	quantity := min(lane.QuantityTons, max(0, in.Ship.EffectiveHoldCapacityTons-in.Ship.CargoUsedTons))
	net := checkedPenceProduct(lane.NetPence, quantity)
	if net == math.MaxInt64 {
		return net
	}
	return net / lane.QuantityTons
}
