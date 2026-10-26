package engine

import (
	"encoding/json"
	"strings"
	"testing"
	"time"
)

func TestUpdateDecodeGossipPlayerDepth(t *testing.T) {
	var gossip Gossip
	err := json.Unmarshal([]byte(`{
		"subject_port_id":"plymouth",
		"snapshot_at":"2026-10-25T12:00:00Z",
		"age_seconds":17,
		"commodities":[{
			"commodity_id":"coarse_cloth",
			"snapshot_at":"2026-10-25T11:59:43Z",
			"best_bid_pence":45000,
			"best_ask_pence":55000,
			"best_player_bid_pence":50000,
			"best_player_bid_quantity":30,
			"best_player_ask_pence":53000,
			"best_player_ask_quantity":12
		}]
	}`), &gossip)
	if err != nil {
		t.Fatal(err)
	}
	if gossip.SubjectPortID != "plymouth" || gossip.AgeSeconds != 17 || len(gossip.Commodities) != 1 {
		t.Fatalf("gossip envelope = %+v", gossip)
	}
	if !gossip.SnapshotAt.Equal(time.Date(2026, 10, 25, 12, 0, 0, 0, time.UTC)) {
		t.Fatalf("snapshot time = %v", gossip.SnapshotAt)
	}
	var report GossipCommodity = gossip.Commodities[0]
	if report.CommodityID != "coarse_cloth" || !report.SnapshotAt.Equal(gossip.SnapshotAt.Add(-17*time.Second)) {
		t.Fatalf("commodity identity/time = %+v", report)
	}
	for _, field := range []struct {
		name string
		got  *int64
		want int64
	}{
		{"NPC bid", report.BestBidPence, 45_000},
		{"NPC ask", report.BestAskPence, 55_000},
		{"player bid", report.BestPlayerBidPence, 50_000},
		{"player bid tons", report.BestPlayerBidQuantity, 30},
		{"player ask", report.BestPlayerAskPence, 53_000},
		{"player ask tons", report.BestPlayerAskQuantity, 12},
	} {
		if field.got == nil || *field.got != field.want {
			t.Errorf("%s = %v, want %d", field.name, field.got, field.want)
		}
	}
	for _, sample := range []string{
		`{"commodity_id":"coarse_cloth"}`,
		`{"commodity_id":"coarse_cloth","best_bid_pence":null,"best_ask_pence":null,"best_player_bid_pence":null,"best_player_bid_quantity":null,"best_player_ask_pence":null,"best_player_ask_quantity":null}`,
	} {
		var missing GossipCommodity
		if err := json.Unmarshal([]byte(sample), &missing); err != nil {
			t.Fatal(err)
		}
		if missing.BestBidPence != nil || missing.BestAskPence != nil || missing.BestPlayerBidPence != nil || missing.BestPlayerBidQuantity != nil || missing.BestPlayerAskPence != nil || missing.BestPlayerAskQuantity != nil {
			t.Fatalf("unobserved gossip prices/quantities became values: %+v", missing)
		}
	}
}

func TestUpdateDecodeShipHullAndNPCLevel(t *testing.T) {
	var ship ShipState
	if err := json.Unmarshal([]byte(`{"ship":{"id":"lapras","created_at":"2026-10-25T10:30:00Z","dispatches_sealed":true}}`), &ship); err != nil {
		t.Fatal(err)
	}
	wantCreated := time.Date(2026, 10, 25, 10, 30, 0, 0, time.UTC)
	if ship.Ship.CreatedAt == nil || !ship.Ship.CreatedAt.Equal(wantCreated) || !ship.Ship.DispatchesSealed {
		t.Fatalf("ship update fields = %+v", ship.Ship)
	}
	var hull Hull
	if err := json.Unmarshal([]byte(`{"id":"packet","can_seal_dispatches":true}`), &hull); err != nil {
		t.Fatal(err)
	}
	if hull.ID != "packet" || !hull.CanSealDispatches {
		t.Fatalf("hull update fields = %+v", hull)
	}
	var level Level
	if err := json.Unmarshal([]byte(`{"order_id":"crown-ask","price_pence":42000,"remaining_quantity":8,"is_npc":true}`), &level); err != nil {
		t.Fatal(err)
	}
	if !level.IsNPC || level.OrderID != "crown-ask" || level.PricePence != 42_000 || level.RemainingQuantity != 8 {
		t.Fatalf("NPC level = %+v", level)
	}
	for _, missing := range []bool{false, true} {
		shipJSON, hullJSON, levelJSON := `{"ship":{}}`, `{}`, `{}`
		if missing {
			shipJSON = `{"ship":{"created_at":null,"dispatches_sealed":null}}`
			hullJSON = `{"can_seal_dispatches":null}`
			levelJSON = `{"is_npc":null}`
		}
		var absentShip ShipState
		var absentHull Hull
		var absentLevel Level
		if err := json.Unmarshal([]byte(shipJSON), &absentShip); err != nil {
			t.Fatal(err)
		}
		if err := json.Unmarshal([]byte(hullJSON), &absentHull); err != nil {
			t.Fatal(err)
		}
		if err := json.Unmarshal([]byte(levelJSON), &absentLevel); err != nil {
			t.Fatal(err)
		}
		if absentShip.Ship.CreatedAt != nil || absentShip.Ship.DispatchesSealed || absentHull.CanSealDispatches || absentLevel.IsNPC {
			t.Fatalf("null=%t manufactured ship/hull/NPC facts: ship=%+v hull=%+v level=%+v", missing, absentShip.Ship, absentHull, absentLevel)
		}
	}
}

func TestUpdateDecodeLloydsEstimate(t *testing.T) {
	var estimate LloydsEstimate
	if err := json.Unmarshal([]byte(`{"id":"policy-1","lloyds_expected_loss_bps":100,"lloyds_min_premium_bps":150,"premium_margin_bps":200}`), &estimate); err != nil {
		t.Fatal(err)
	}
	if estimate.PolicyID != "policy-1" || estimate.ExpectedLossBPS == nil || *estimate.ExpectedLossBPS != 100 || estimate.MinPremiumBPS == nil || *estimate.MinPremiumBPS != 150 || estimate.PremiumMarginBPS == nil || *estimate.PremiumMarginBPS != 200 {
		t.Fatalf("Lloyd's estimate = %+v", estimate)
	}
	for _, sample := range []string{
		`{"id":"policy-1"}`,
		`{"id":"policy-1","lloyds_expected_loss_bps":null,"lloyds_min_premium_bps":null,"premium_margin_bps":null}`,
	} {
		var missing LloydsEstimate
		if err := json.Unmarshal([]byte(sample), &missing); err != nil {
			t.Fatal(err)
		}
		if missing.PolicyID != "policy-1" || missing.ExpectedLossBPS != nil || missing.MinPremiumBPS != nil || missing.PremiumMarginBPS != nil {
			t.Fatalf("missing Lloyd's estimates became observations: %+v", missing)
		}
	}
}

func TestUpdateTermsHashIgnoresLloydsFields(t *testing.T) {
	const originalJSON = `{
		"id":"01ad0001-0000-7000-8000-0000000c0a01",
		"insured_company_id":"01a16469-7564-711d-b8da-e69d7d73e623",
		"ship_id":"01a169e2-69a5-70f9-9cf0-921c074b2e00",
		"origin_port_id":"lisbon","dest_port_id":"cadiz",
		"insured_value_pence":100000000,"hull_value_pence":100000000,
		"premium_bps":300,"total_premium_pence":3000000,"covered_bps":0,
		"status":"open","created_at":"2026-10-25T09:00:00Z",
		"updated_at":"2026-10-25T10:00:00Z","expires_at":"2026-10-26T09:00:00Z"
	}`
	updatedJSON := strings.TrimSuffix(originalJSON, "}") + `,"lloyds_expected_loss_bps":100,"lloyds_min_premium_bps":150,"premium_margin_bps":200}`
	var original, updated InsurancePolicy
	if err := json.Unmarshal([]byte(originalJSON), &original); err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal([]byte(updatedJSON), &updated); err != nil {
		t.Fatal(err)
	}
	if got, want := insuranceTermsHash(updated), insuranceTermsHash(original); got != want {
		t.Fatalf("adding Lloyd's observations changed immutable terms hash: got %s, want %s", got, want)
	}
	updated.PremiumBPS++
	if insuranceTermsHash(updated) == insuranceTermsHash(original) {
		t.Fatal("terms hash ignored a contractual premium change")
	}
}
