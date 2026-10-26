package engine_test

import (
	"strings"
	"testing"

	"github.com/DevYukine/go-tradewinds/internal/engine"
)

func TestMarquePlannerRejectsOwnZonesForEveryVoyageKind(t *testing.T) {
	t.Parallel()
	for _, kind := range []string{"charter", "freight", "spot"} {
		t.Run(kind, func(t *testing.T) {
			in := inputFixture(t)
			switch kind {
			case "freight":
				in.Contracts = []engine.Contract{{
					ID: "freight", ContractType: "freight", Status: "open", IssuerCompanyID: "other-company",
					OriginPortID: "le_havre", DestPortID: "portsmouth", CommodityID: "grain",
					QuantityRequired: 12, RewardPence: 100_000,
				}}
			case "spot":
				in.Contracts = nil
				in.Markets["portsmouth"] = fixture[engine.Market](t, `{
					"port_id":"portsmouth","visibility":"quay",
					"books":[{"commodity_id":"provisions","depth":{"bids":[{"price_pence":5000,"remaining_quantity":100}],"asks":[]}}]
				}`)
				in.Metadata["books/portsmouth"] = in.Metadata["books/le_havre"]
			}
			in.MarqueZones = map[string]bool{"unrelated_zone": true}
			if plans := engine.PlanVoyages(in); len(plans) == 0 || !plans[0].Accepted || plans[0].Kind != kind {
				t.Fatalf("unrelated marque changed the executable %s control: %+v", kind, plans)
			}
			in.MarqueZones["english_channel"] = true
			plans := engine.PlanVoyages(in)
			if len(plans) == 0 {
				t.Fatal("planner omitted its route rejection")
			}
			rejectedKind := false
			for _, plan := range plans {
				if plan.Accepted {
					t.Fatalf("route through an owned marque was accepted: %+v", plan)
				}
				if plan.Kind == kind {
					rejectedKind = true
					if !strings.Contains(plan.Reason, "our own letter of marque") {
						t.Fatalf("the formerly executable %s lacks a plain route rejection: %+v", kind, plan)
					}
				}
			}
			if !rejectedKind {
				t.Fatalf("planner omitted the formerly executable %s alternative", kind)
			}
		})
	}

}
