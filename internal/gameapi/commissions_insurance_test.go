package gameapi

import (
	"encoding/json"
	"testing"
)

func TestCommissionListingDecodesObservedBostonContract(t *testing.T) {
	// contract/list observation, 2026-11-02. The public Exchange omits collateral.
	payload := []byte(`{"id":"01a19832-c48e-7635-b281-a9ce8f84e301","stale":false,"title":null,"gossip":null,"status":"open","licence":null,"metadata":{"admiralty":true,"ship_class":"frigate","rebuild_value_pence":19655011},"funded_by":null,"is_public":false,"created_at":"2026-11-01T17:40:24.573015Z","expires_at":"2026-11-02T01:40:26.382756Z","ship_class":"frigate","updated_at":"2026-11-01T17:40:24.573015Z","commodity_id":null,"dest_port_id":null,"reward_pence":22603262,"contract_type":"commission","origin_port_id":"boston","carrier_ship_id":null,"collateral_pence":2260326,"issuer_company_id":null,"quantity_required":0,"carrier_company_id":null,"quantity_delivered":0,"remaining_quantity":0,"reward_per_ton_pence":null}`)
	var row CommissionListing
	if err := json.Unmarshal(payload, &row); err != nil {
		t.Fatal(err)
	}
	var metadata CommissionMetadata
	if err := json.Unmarshal(row.Metadata, &metadata); err != nil {
		t.Fatal(err)
	}
	if row.ContractType != "commission" || row.OriginPortID != "boston" || row.ShipClass == nil || *row.ShipClass != "frigate" ||
		row.RewardPence != 22_603_262 || row.CollateralPence != 2_260_326 || row.ExpiresAt == nil || !metadata.Admiralty || metadata.RebuildValuePence != 19_655_011 {
		t.Fatalf("commission terms were lost: %+v metadata=%+v", row, metadata)
	}
}

func TestLloydsQuoteDecodesObservedMinimumPremium(t *testing.T) {
	// Authenticated hazard quote observation. A 100p minimum is not ceil(10000*29bps).
	payload := []byte(`{"ship_id":"01a178ee-e976-7292-8585-af14f0d99a13","dest_port_id":"newcastle","lloyds_writes":true,"origin_port_id":"amsterdam","hull_value_pence":4271766,"lloyds_premium_bps":29,"insured_value_pence":10000,"max_insurable_pence":4271766,"lloyds_premium_pence":100,"priced_consort_ship_ids":[],"lloyds_expected_loss_bps":4,"lloyds_expected_loss_pence":4}`)
	var quote LloydsQuoteResult
	if err := json.Unmarshal(payload, &quote); err != nil {
		t.Fatal(err)
	}
	if !quote.LloydsWrites || !quote.LloydsWritesKnown || quote.LloydsPremiumPence == nil || *quote.LloydsPremiumPence != 100 ||
		quote.LloydsExpectedLossPence == nil || *quote.LloydsExpectedLossPence != 4 || quote.MaxInsurablePence != 4_271_766 || len(quote.PricedConsortShipIDs) != 0 {
		t.Fatalf("published quote or its minimum premium was lost: %+v", quote)
	}
}

func TestLloydsQuoteDistinguishesUnknownAndDeclinedWrites(t *testing.T) {
	for _, test := range []struct {
		payload string
		known   bool
		writes  bool
	}{
		{`{"hull_value_pence":2000000}`, false, false},
		{`{"lloyds_writes":null}`, false, false},
		{`{"hull_value_pence":2000000,"lloyds_premium_bps":400}`, false, false},
		{`{"hull_value_pence":2000000,"lloyds_premium_bps":400,"lloyds_writes":null}`, false, false},
		{`{"lloyds_writes":false}`, true, false},
		{`{"lloyds_writes":true}`, true, true},
	} {
		var quote LloydsQuoteResult
		if err := json.Unmarshal([]byte(test.payload), &quote); err != nil {
			t.Fatal(err)
		}
		if quote.LloydsWritesKnown != test.known || quote.LloydsWrites != test.writes {
			t.Fatalf("write eligibility presence lost for %s: %+v", test.payload, quote)
		}
		stored, err := json.Marshal(quote)
		if err != nil {
			t.Fatal(err)
		}
		var restored LloydsQuoteResult
		if err := json.Unmarshal(stored, &restored); err != nil {
			t.Fatal(err)
		}
		if restored.LloydsWritesKnown != test.known || restored.LloydsWrites != test.writes {
			t.Fatalf("persisted eligibility changed for %s: %s restored=%+v", test.payload, stored, restored)
		}
	}
}

func TestCommissionAndInsuranceReceiptsFollowDocumentedFlatShapes(t *testing.T) {
	var accept CommissionResult
	if err := json.Unmarshal([]byte(`{"contract_id":"contract","status":"active","port_id":"boston","ship_class":"frigate","price_pence":22603262,"collateral_pence":2260326,"builder_company_id":"builder","buyer_company_id":null,"admiralty":true,"expires_at":"2026-11-02T01:40:26Z"}`), &accept); err != nil {
		t.Fatal(err)
	}
	if !accept.Admiralty || accept.BuilderCompanyID == nil || accept.BuyerCompanyID != nil || accept.PricePence == nil || *accept.PricePence != 22_603_262 {
		t.Fatalf("accept receipt: %+v", accept)
	}
	var delivered DeliverCommissionResult
	if err := json.Unmarshal([]byte(`{"contract_id":"contract","ship_id":"hull","ship_name":"Lapras","ship_class":"frigate","port_id":"boston","build_mode":"turnkey_cash","build_cost_pence":18000000,"price_paid_pence":22603262,"collateral_returned_pence":2260326,"buyer_company_id":null}`), &delivered); err != nil {
		t.Fatal(err)
	}
	if delivered.BuildCostPence == nil || *delivered.BuildCostPence != 18_000_000 || delivered.CollateralReturnedPence == nil || *delivered.CollateralReturnedPence != 2_260_326 {
		t.Fatalf("deliver receipt: %+v", delivered)
	}
	var policy CreateInsurancePolicyResult
	if err := json.Unmarshal([]byte(`{"id":"policy","insured_company_id":"owner","ship_id":"hull","origin_port_id":"boston","dest_port_id":"havana","insured_value_pence":20000000,"hull_value_pence":18000000,"premium_bps":50,"total_premium_pence":100000,"covered_bps":0,"status":"open","created_at":"2026-11-02T00:00:00Z","updated_at":"2026-11-02T00:00:00Z","expires_at":"2026-11-02T06:00:00Z","lloyds_premium_bps":50,"above_lloyds_price":false}`), &policy); err != nil {
		t.Fatal(err)
	}
	if policy.CoveredBPS == nil || *policy.CoveredBPS != 0 || policy.TotalPremiumPence == nil || *policy.TotalPremiumPence != 100_000 || policy.LloydsPremiumBPS == nil || *policy.LloydsPremiumBPS != 50 || policy.AboveLloydsPrice {
		t.Fatalf("flat create receipt: %+v", policy)
	}
}
