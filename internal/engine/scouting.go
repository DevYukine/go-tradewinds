package engine

import (
	"context"
	"errors"
	"fmt"
	"math"
	"sort"
	"strings"
	"time"

	"github.com/DevYukine/go-tradewinds/internal/store"
)

const scoutHintMaxGameHours = 168
const scoutBookRecentGameHours = 6
const failedFleetScoutReason = "two fleet scouting relocations remain unconverted; scouting waits for a laden departure from a scouted port"

var errScoutTransient = errors.New("scouting evidence temporarily unavailable")

func scoutTransientError(data *missionData, err error) error {
	if data.Relocation != nil && data.Relocation.Scout {
		return fmt.Errorf("%w: %w", errScoutTransient, err)
	}
	return err
}

type scoutBidHint struct {
	bid int64
	at  time.Time
}

// Both timestamps bound a hint's age. Newer adverse observations replace old
// high bids; none of these public hints establishes executable remote depth.
func scoutBidHints(in PlanningInput) map[string]map[string]scoutBidHint {
	hints := make(map[string]map[string]scoutBidHint)
	_, upper, ok := in.serverBounds()
	if !ok || in.Clock.Calendar.TimeScale <= 0 {
		return hints
	}
	ports := make(map[string]bool, len(in.Catalog.Ports))
	for _, port := range in.Catalog.Ports {
		ports[port.ID] = true
	}
	commodities := make(map[string]bool, len(in.Catalog.Commodities))
	for _, commodity := range in.Catalog.Commodities {
		commodities[commodity.ID] = true
	}
	maxAge := float64(scoutHintMaxGameHours * 3600)
	for port, detail := range in.Ports {
		if !ports[port] || !in.fresh("ports/"+port) {
			continue
		}
		for _, report := range detail.CoffeeHouseGossip {
			age := upper.Sub(report.SnapshotAt)
			if !ports[report.SubjectPortID] || report.SnapshotAt.IsZero() || age < 0 || age.Seconds()*float64(in.Clock.Calendar.TimeScale) > maxAge {
				continue
			}
			for _, price := range report.Commodities {
				priceAge := upper.Sub(price.SnapshotAt)
				if !commodities[price.CommodityID] || price.SnapshotAt.IsZero() || priceAge < 0 ||
					priceAge.Seconds()*float64(in.Clock.Calendar.TimeScale) > maxAge ||
					price.BestBidPence == nil || *price.BestBidPence <= 0 || *price.BestBidPence > maximumPence {
					continue
				}
				at := price.SnapshotAt
				if report.SnapshotAt.Before(at) {
					at = report.SnapshotAt
				}
				if !at.After(in.BookObservedAt[report.SubjectPortID]) {
					continue
				}
				if hints[report.SubjectPortID] == nil {
					hints[report.SubjectPortID] = make(map[string]scoutBidHint)
				}
				prior := hints[report.SubjectPortID][price.CommodityID]
				if prior.at.IsZero() || at.After(prior.at) || at.Equal(prior.at) && *price.BestBidPence < prior.bid {
					hints[report.SubjectPortID][price.CommodityID] = scoutBidHint{bid: *price.BestBidPence, at: at}
				}
			}
		}
	}
	return hints
}

func scoutHintUpside(in PlanningInput, hints map[string]scoutBidHint) (string, int64, time.Time) {
	book := in.Markets[in.Ship.Ship.CurrentPortID]
	freeHold := max(0, in.Ship.EffectiveHoldCapacityTons-in.Ship.CargoUsedTons)
	var bestCommodity string
	var bestUpside int64
	var bestAt time.Time
	for commodity, hint := range hints {
		entry := bookCommodity(book, commodity)
		if entry == nil || entry.Depth == nil {
			continue
		}
		var quantity int64
		for _, level := range entry.Depth.Asks {
			if level.PricePence > 0 && level.PricePence <= maximumPence && level.RemainingQuantity > 0 {
				quantity += min(freeHold-quantity, level.RemainingQuantity)
			}
		}
		if quantity == 0 {
			continue
		}
		fill, err := WalkDepth(entry.Depth.Asks, quantity, true)
		revenue := checkedPenceProduct(quantity, hint.bid)
		if err != nil || revenue == math.MaxInt64 {
			continue
		}
		// Bound the same aggregate buyer-customs calculation before ceilBPS.
		if book.CustomsBPS > 0 && fill.GrossPence > (math.MaxInt64-9999)/book.CustomsBPS {
			continue
		}
		customs := ceilBPS(fill.GrossPence, book.CustomsBPS)
		if customs > math.MaxInt64-fill.GrossPence {
			continue
		}
		upside := revenue - fill.GrossPence - customs
		if upside > bestUpside || upside > 0 && upside == bestUpside &&
			(hint.at.After(bestAt) || hint.at.Equal(bestAt) && commodity < bestCommodity) {
			bestCommodity, bestUpside, bestAt = commodity, upside, hint.at
		}
	}
	return bestCommodity, bestUpside, bestAt
}

func scoutLaneKey(target, origin, commodity string) string {
	return target + "|" + origin + "|" + commodity + "|scout"
}

func scoutExposure(in PlanningInput, plan Plan) Plan {
	origin, exists := findPort(in.Catalog, plan.Origin)
	if !exists {
		return reject(plan, "sourcing hull or ports are missing from catalog")
	}
	plan = relocationCost(in, plan, origin, 0)
	if !plan.Accepted {
		return plan
	}
	target, _ := findPort(in.Catalog, plan.Destination)
	if supplied, _, _ := publicSourceEvidence(in, target, Contract{CommodityID: "provisions"}); !supplied {
		return reject(plan, "scouting target lacks catalog production/manufacture or fresh ask gossip for provisions")
	}
	return plan
}

func scoutCandidates(in PlanningInput, blocked map[string]string) []Plan {
	type rankedScout struct {
		plan   Plan
		upside int64
		at     time.Time
	}
	hints := scoutBidHints(in)
	origin := in.Ship.Ship.CurrentPortID
	book := in.Markets[origin]
	_, upper, _ := in.serverBounds()
	var ranked []rankedScout
	for _, target := range in.Catalog.Ports {
		if target.ID == origin || target.Phase > in.Clock.UnlockedPhase {
			continue
		}
		commodity, upside, at := scoutHintUpside(in, hints[target.ID])
		candidate := rankedScout{
			plan:   Plan{Kind: "relocation", ShipID: in.Ship.Ship.ID, Origin: origin, Destination: target.ID, Commodity: commodity},
			upside: upside, at: at,
		}
		plan := candidate.plan
		if reason := blocked[target.ID]; reason != "" {
			plan = reject(plan, reason)
		} else if !in.fresh("books/"+origin) || book.Visibility != "quay" || book.CustomsBPS < 0 {
			plan = reject(plan, "scouting requires fresh local quay ask depth and valid buyer customs")
		} else if candidate.upside <= 0 {
			plan = reject(plan, "no age-labelled gossip hint shows a positive post-customs spread")
		} else if in.SourcingProbeHistory.ScoutFailures >= 2 {
			plan = reject(plan, failedFleetScoutReason)
		} else {
			age := upper.Sub(candidate.at).Hours() * float64(in.Clock.Calendar.TimeScale)
			plan.PriceSource = fmt.Sprintf("gossip bid hint %.1f game hours old; not executable depth", age)
			plan = scoutExposure(in, plan)
			if plan.Accepted && in.SourcingProbeHistory.Ships[in.Ship.Ship.ID].Failures >= 2 {
				plan = reject(plan, failedProbeStreakReason)
			}
			if plan.Accepted {
				if reason := scoutTargetCooldown(in, target.ID); reason != "" {
					plan = reject(plan, reason)
				}
			}
			if plan.Accepted {
				bounded := in
				bounded.ReserveByRegion = sourcingReserves(in, target)
				allowance := sourcingAllowance(bounded)
				if -plan.Economics.ExpectedNetPence > allowance {
					plan = reject(plan, sourcingBudgetReason(bounded, -plan.Economics.ExpectedNetPence, allowance))
				} else if candidate.upside <= -plan.Economics.ExpectedNetPence {
					plan = reject(plan, "age-labelled gossip hint upside does not exceed modeled scouting exposure")
				} else {
					plan.Reason = fmt.Sprintf("bounded ballast scouting uses exploration risk capital; %s; hint upside %dp is not measured profit", plan.PriceSource, candidate.upside)
				}
			}
		}
		candidate.plan = plan
		ranked = append(ranked, candidate)
	}
	sort.Slice(ranked, func(i, j int) bool {
		left, right := ranked[i], ranked[j]
		if left.plan.Accepted != right.plan.Accepted {
			return left.plan.Accepted
		}
		leftNet := left.upside + left.plan.Economics.ExpectedNetPence
		rightNet := right.upside + right.plan.Economics.ExpectedNetPence
		if leftNet != rightNet {
			return leftNet > rightNet
		}
		if !left.at.Equal(right.at) {
			return left.at.After(right.at)
		}
		if left.plan.Economics.GameDays != right.plan.Economics.GameDays {
			return left.plan.Economics.GameDays < right.plan.Economics.GameDays
		}
		return left.plan.Destination < right.plan.Destination
	})
	plans := make([]Plan, 0, len(ranked))
	for _, candidate := range ranked {
		plans = append(plans, candidate.plan)
	}
	if len(plans) == 0 {
		return []Plan{reject(Plan{Kind: "relocation", ShipID: in.Ship.Ship.ID}, "no unlocked scouting destination away from this port")}
	}
	return plans
}

func (r *Runner) scoutBlockedPorts(in PlanningInput, missions map[string]*store.Mission) map[string]string {
	blocked := make(map[string]string)
	for _, ship := range r.stateCache.Ships {
		if ship.Ship.ID == in.Ship.Ship.ID {
			continue
		}
		if ship.Voyage == nil && (ship.Ship.Status == "berthed" || ship.Ship.Status == "anchored_roadstead") && ship.Ship.CurrentPortID != "" {
			if blocked[ship.Ship.CurrentPortID] == "" {
				blocked[ship.Ship.CurrentPortID] = fmt.Sprintf("our ship %s is at %s", ship.Ship.Name, ship.Ship.CurrentPortID)
			}
		} else if (ship.Ship.Status == "sailing" || ship.Voyage != nil) && len(ship.RouteLegs) > 0 {
			target := ship.RouteLegs[len(ship.RouteLegs)-1].ToNode
			if blocked[target] == "" {
				blocked[target] = fmt.Sprintf("our ship %s is already heading to %s", ship.Ship.Name, target)
			}
		}
	}
	for id, mission := range missions {
		if id != in.Ship.Ship.ID && mission != nil && mission.Phase != "maintenance" && mission.Phase != "maintenance_done" && mission.Phase != "settled" &&
			mission.Destination != "" && blocked[mission.Destination] == "" {
			blocked[mission.Destination] = "an unsettled mission already targets " + mission.Destination
		}
	}
	for port, observed := range in.BookObservedAt {
		age := in.Now.Sub(observed).Hours() * float64(in.Clock.Calendar.TimeScale)
		if !observed.IsZero() && age < scoutBookRecentGameHours && blocked[port] == "" {
			blocked[port] = "a complete quay book was observed here less than six game hours ago"
		}
	}
	for _, port := range in.Catalog.Ports {
		if blocked[port.ID] == "" {
			blocked[port.ID] = scoutTargetCooldown(in, port.ID)
		}
	}
	return blocked
}

func scoutTargetCooldown(in PlanningInput, target string) string {
	port := in.SourcingProbeHistory.ScoutPorts[target]
	completed := port.CompletedAt
	for lane, at := range in.SourcingProbeCompletions {
		parts := strings.SplitN(lane, "|", 4)
		if len(parts) == 4 && parts[0] == target && parts[3] == "scout" && at.After(completed) {
			completed = at
		}
	}
	if !completed.IsZero() && in.Now.Sub(completed) < sourcingReinspectionCooldown {
		return "scouting target is cooling down for 24 real minutes after a completed scout"
	}
	if !port.LastDepartureAt.IsZero() && in.Now.Sub(port.LastDepartureAt) < sourcingReinspectionCooldown {
		return "scouting waits 24 real minutes after our last departure from this port"
	}
	return ""
}

func (r *Runner) scoutTargetReady(ctx context.Context, in PlanningInput, target string) error {
	if in.SourcingHistoryError != "" {
		return fmt.Errorf("%w: %s", errScoutTransient, in.SourcingHistoryError)
	}
	if !in.fresh("catalog") {
		return fmt.Errorf("%w: committed scouting waits for current successful catalog authority", errScoutTransient)
	}
	destination, exists := findPort(in.Catalog, target)
	if !exists || destination.Phase > in.Clock.UnlockedPhase {
		return errors.New("target is no longer unlocked in the catalog")
	}
	origin := in.Ship.Ship.CurrentPortID
	for _, key := range []string{"ships", "clock", "zones", "vaults", "rates", "books/" + origin,
		"ports/" + origin, "ports/" + target,
		"routes/" + routeKey(origin, target, in.Ship.Ship.ShipClass),
		"routes/" + routeKey(target, origin, in.Ship.Ship.ShipClass)} {
		if !in.fresh(key) {
			return fmt.Errorf("%w: committed scouting waits for current successful %s authority", errScoutTransient, key)
		}
	}
	if _, _, ok := in.serverBounds(); !ok || in.Clock.Paused || in.Clock.Calendar.TimeScale <= 0 {
		return fmt.Errorf("%w: committed scouting waits for an unpaused authoritative game clock", errScoutTransient)
	}
	for reporter, detail := range in.Ports {
		if _, exists := findPort(in.Catalog, reporter); !exists || in.fresh("ports/"+reporter) {
			continue
		}
		for _, report := range detail.CoffeeHouseGossip {
			if report.SubjectPortID == target {
				return fmt.Errorf("%w: committed scouting waits for current successful ports/%s authority", errScoutTransient, reporter)
			}
		}
	}
	missions := make(map[string]*store.Mission, len(r.stateCache.Ships))
	for _, other := range r.stateCache.Ships {
		mission, err := r.db.GetMission(ctx, r.account.ID, other.Ship.ID)
		if err != nil {
			return fmt.Errorf("%w: %w", errScoutTransient, err)
		}
		missions[other.Ship.ID] = mission
	}
	if reason := r.scoutBlockedPorts(in, missions)[target]; reason != "" {
		return errors.New(reason)
	}
	if book := in.Markets[origin]; book.Visibility != "quay" || book.CustomsBPS < 0 {
		return errors.New("scouting requires fresh local quay ask depth and valid buyer customs")
	}
	_, upside, _ := scoutHintUpside(in, scoutBidHints(in)[target])
	if upside <= 0 {
		return errors.New("no age-labelled gossip hint shows a positive post-customs spread")
	}
	originPort, _ := findPort(in.Catalog, origin)
	plan := relocationCost(in, Plan{Kind: "relocation", ShipID: in.Ship.Ship.ID, Origin: origin, Destination: target}, originPort, 0)
	if !plan.Accepted {
		return errors.New(plan.Reason)
	}
	if upside <= -plan.Economics.ExpectedNetPence {
		return errors.New("age-labelled gossip hint upside does not exceed modeled scouting exposure")
	}
	return nil
}
