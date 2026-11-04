package gameapi

import (
	"encoding/json"
	"time"
)

type LloydsQuoteResult struct {
	ShipID                  string   `json:"ship_id"`
	OriginPortID            string   `json:"origin_port_id"`
	DestPortID              string   `json:"dest_port_id"`
	InsuredValuePence       int64    `json:"insured_value_pence"`
	HullValuePence          int64    `json:"hull_value_pence"`
	MaxInsurablePence       int64    `json:"max_insurable_pence"`
	LloydsExpectedLossBPS   *int64   `json:"lloyds_expected_loss_bps"`
	LloydsExpectedLossPence *int64   `json:"lloyds_expected_loss_pence"`
	LloydsPremiumBPS        *int64   `json:"lloyds_premium_bps"`
	LloydsPremiumPence      *int64   `json:"lloyds_premium_pence"`
	LloydsWrites            bool     `json:"lloyds_writes"`
	LloydsWritesKnown       bool     `json:"lloyds_writes_known"`
	PricedConsortShipIDs    []string `json:"priced_consort_ship_ids"`
}

func (quote *LloydsQuoteResult) UnmarshalJSON(data []byte) error {
	type wireQuote LloydsQuoteResult
	wire := struct {
		*wireQuote
		Writes      *bool `json:"lloyds_writes"`
		WritesKnown *bool `json:"lloyds_writes_known"`
	}{wireQuote: (*wireQuote)(quote)}
	if err := json.Unmarshal(data, &wire); err != nil {
		return err
	}
	quote.LloydsWritesKnown = wire.Writes != nil
	if wire.WritesKnown != nil {
		quote.LloydsWritesKnown = quote.LloydsWritesKnown && *wire.WritesKnown
	}
	quote.LloydsWrites = wire.Writes != nil && *wire.Writes
	return nil
}

type CreateInsurancePolicyRequest struct {
	ShipID            string `json:"ship_id"`
	DestPortID        string `json:"dest_port_id"`
	InsuredValuePence int64  `json:"insured_value_pence"`
	PremiumBPS        int64  `json:"premium_bps"`
}

// InsurancePolicy is the create receipt and public listing shape. Financial
// fields which can be zero retain presence so absent coverage is not full cover.
type InsurancePolicy struct {
	ID                string    `json:"id"`
	InsuredCompanyID  string    `json:"insured_company_id"`
	ShipID            string    `json:"ship_id"`
	OriginPortID      string    `json:"origin_port_id"`
	DestPortID        string    `json:"dest_port_id"`
	InsuredValuePence int64     `json:"insured_value_pence"`
	HullValuePence    int64     `json:"hull_value_pence"`
	PremiumBPS        int64     `json:"premium_bps"`
	TotalPremiumPence *int64    `json:"total_premium_pence"`
	CoveredBPS        *int64    `json:"covered_bps"`
	Status            string    `json:"status"`
	CreatedAt         time.Time `json:"created_at"`
	UpdatedAt         time.Time `json:"updated_at"`
	ExpiresAt         time.Time `json:"expires_at"`
}

type CreateInsurancePolicyResult struct {
	InsurancePolicy
	LloydsPremiumBPS *int64 `json:"lloyds_premium_bps"`
	AboveLloydsPrice bool   `json:"above_lloyds_price"`
}
