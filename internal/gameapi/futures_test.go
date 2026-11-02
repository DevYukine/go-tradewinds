package gameapi

import (
	"encoding/json"
	"testing"
)

func TestFuturesSettlementEventReceiptRequiresDefaultEvidenceOnlyForDefaults(t *testing.T) {
	for _, test := range []struct {
		name string
		body string
		want SettleFuturesResult
		ok   bool
	}{
		{
			name: "settled event omits default-only amounts",
			body: `{"futures_id":"future-one","status":"settled","settled_tons":4,"notional_pence":4000}`,
			want: SettleFuturesResult{FuturesID: "future-one", Status: "settled", SettledTons: 4},
			ok:   true,
		},
		{
			name: "settled event preserves explicit amounts",
			body: `{"futures_id":"future-one","status":"settled","settled_tons":4,"penalty_paid_pence":12,"claim_pence":34}`,
			want: SettleFuturesResult{FuturesID: "future-one", Status: "settled", SettledTons: 4, PenaltyPaidPence: 12, ClaimPence: 34},
			ok:   true,
		},
		{
			name: "default with explicit zero amounts",
			body: `{"futures_id":"future-one","status":"defaulted","settled_tons":0,"penalty_paid_pence":0,"claim_pence":0}`,
			want: SettleFuturesResult{FuturesID: "future-one", Status: "defaulted"},
			ok:   true,
		},
		{
			name: "default preserves liability amounts",
			body: `{"futures_id":"future-one","status":"defaulted","settled_tons":1,"penalty_paid_pence":575,"claim_pence":125}`,
			want: SettleFuturesResult{FuturesID: "future-one", Status: "defaulted", SettledTons: 1, PenaltyPaidPence: 575, ClaimPence: 125},
			ok:   true,
		},
		{name: "default missing both amounts", body: `{"futures_id":"future-one","status":"defaulted","settled_tons":0}`},
		{name: "default missing penalty", body: `{"futures_id":"future-one","status":"defaulted","settled_tons":0,"claim_pence":0}`},
		{name: "default missing claim", body: `{"futures_id":"future-one","status":"defaulted","settled_tons":0,"penalty_paid_pence":0}`},
		{name: "default null penalty", body: `{"futures_id":"future-one","status":"defaulted","settled_tons":0,"penalty_paid_pence":null,"claim_pence":0}`},
		{name: "default null claim", body: `{"futures_id":"future-one","status":"defaulted","settled_tons":0,"penalty_paid_pence":0,"claim_pence":null}`},
		{name: "settled missing tons", body: `{"futures_id":"future-one","status":"settled"}`},
		{name: "settled null tons", body: `{"futures_id":"future-one","status":"settled","settled_tons":null}`},
		{name: "missing identity", body: `{"status":"settled","settled_tons":1}`},
		{name: "nonterminal status", body: `{"futures_id":"future-one","status":"active","settled_tons":1}`},
	} {
		t.Run(test.name, func(t *testing.T) {
			var event FuturesSettlementEvent
			if err := json.Unmarshal([]byte(test.body), &event); err != nil {
				t.Fatal(err)
			}
			receipt, ok := event.Receipt()
			if ok != test.ok || receipt != test.want {
				t.Fatalf("receipt = %+v, accepted = %t, want %+v, accepted = %t", receipt, ok, test.want, test.ok)
			}
		})
	}
}

func TestFuturesSettlementEventRetainsOptionalNotionalAndParties(t *testing.T) {
	var event FuturesSettlementEvent
	body := `{"futures_id":"future-one","status":"settled","settled_tons":4,"notional_pence":4000,"port_id":"boston","commodity_id":"timber","seller_company_id":"seller","buyer_company_id":"buyer"}`
	if err := json.Unmarshal([]byte(body), &event); err != nil {
		t.Fatal(err)
	}
	if event.NotionalPence == nil || *event.NotionalPence != 4000 || event.PortID != "boston" || event.CommodityID != "timber" ||
		event.SellerCompanyID == nil || *event.SellerCompanyID != "seller" || event.BuyerCompanyID == nil || *event.BuyerCompanyID != "buyer" {
		t.Fatalf("stream receipt lost contract-matching evidence: %+v", event)
	}
	if event.PenaltyPaidPence != nil || event.ClaimPence != nil {
		t.Fatalf("omitted liability evidence became present in the event: %+v", event)
	}
}

func TestSettleFuturesResultStillRequiresAllFinancialFields(t *testing.T) {
	for _, status := range []string{"settled", "defaulted"} {
		t.Run(status, func(t *testing.T) {
			for _, missing := range []string{"settled_tons", "penalty_paid_pence", "claim_pence"} {
				for _, null := range []bool{false, true} {
					body := map[string]any{
						"futures_id": "future-one", "status": status,
						"settled_tons": 0, "penalty_paid_pence": 0, "claim_pence": 0,
					}
					if null {
						body[missing] = nil
					} else {
						delete(body, missing)
					}
					raw, err := json.Marshal(body)
					if err != nil {
						t.Fatal(err)
					}
					var result SettleFuturesResult
					if err := json.Unmarshal(raw, &result); err == nil {
						t.Fatalf("REST receipt accepted absent financial evidence: field=%s null=%t", missing, null)
					}
				}
			}
			body, err := json.Marshal(map[string]any{
				"futures_id": "future-one", "status": status,
				"settled_tons": 0, "penalty_paid_pence": 0, "claim_pence": 0,
			})
			if err != nil {
				t.Fatal(err)
			}
			var result SettleFuturesResult
			if err := json.Unmarshal(body, &result); err != nil {
				t.Fatalf("explicit zero REST receipt rejected: %v", err)
			}
			if result != (SettleFuturesResult{FuturesID: "future-one", Status: status}) {
				t.Fatalf("explicit zero REST receipt changed: %+v", result)
			}
		})
	}
}
