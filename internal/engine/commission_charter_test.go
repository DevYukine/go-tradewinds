package engine

import (
	"strings"
	"testing"
	"time"
)

func capeCharterPlanningInput(t *testing.T) PlanningInput {
	t.Helper()
	in := incomeFundingInput(t, "charter")
	in.Clock.UnlockedPhase = 2
	in.Catalog.ShipClasses[0].HullCategory = "oceanic"
	in.Contracts[0] = sourcingFixture[Contract](t, `{"id":"cape-expansion","title":"Admiralty Cape Expedition","contract_type":"charter","origin_port_id":"portsmouth","status":"open","is_public":true,"remaining_quantity":1744,"quantity_required":1744}`)
	in.Contracts[0].CommodityID = "copper_sheathing"
	in.Contracts[0].RewardPerTonPence = new(int64(147_000))
	in.Contracts[0].ExpiresAt = new(in.Now.Add(24 * time.Hour))
	in.Contracts[0].Metadata = []byte(`{"beyond_coastal_water":true}`)
	for portID, market := range in.Markets {
		for index := range market.Books {
			if market.Books[index].CommodityID == "sawn_planks" {
				market.Books[index].CommodityID = "copper_sheathing"
			}
		}
		in.Markets[portID] = market
	}
	for key, route := range in.Routes {
		route.SafeProvisionsTons = 10
		for index := range route.Legs {
			route.Legs[index].EdgeClass = "oceanic"
		}
		in.Routes[key] = route
	}
	seedZeroHazardQuotes(&in)
	return in
}

func TestCapeExpansionCharterAdmitsRatedHullAndRequiresStores(t *testing.T) {
	in := capeCharterPlanningInput(t)
	accepted := false
	for _, plan := range PlanVoyages(in) {
		if plan.ContractID == in.Contracts[0].ID && plan.Accepted {
			accepted = true
			if plan.TargetStoresTons <= 10 || plan.StoresToBuy <= 0 {
				t.Fatalf("oceanic charter omitted sea and port provisions: %+v", plan)
			}
		}
	}
	if !accepted {
		t.Fatalf("suitable rated hull ignored an expansion charter: %+v", PlanVoyages(in))
	}
	in = capeCharterPlanningInput(t)
	in.Catalog.ShipClasses[0].HullCategory = "regional"
	for _, plan := range PlanVoyages(in) {
		if plan.Accepted {
			t.Fatalf("regional hull accepted the Cape's oceanic crossing: %+v", plan)
		}
	}
	in = capeCharterPlanningInput(t)
	in.Ship.Stores.ProvisionsTons = 0
	market := in.Markets[in.Ship.Ship.CurrentPortID]
	for index := range market.Books {
		if market.Books[index].CommodityID == "provisions" {
			market.Books[index].Depth = nil
		}
	}
	in.Markets[in.Ship.Ship.CurrentPortID] = market
	blockedForStores := false
	for _, plan := range PlanVoyages(in) {
		if plan.Accepted {
			t.Fatalf("unprovisionable Cape expedition was admitted: %+v", plan)
		}
		blockedForStores = blockedForStores || strings.Contains(plan.Reason, "stores")
	}
	if !blockedForStores {
		t.Fatal("fixture did not expose the actual provisions block")
	}
}
