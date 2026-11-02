package gameapi

import "time"

// CommissionListing is a contract/list row. Admiralty and rebuild value live in
// metadata, not in the listing's top-level fields or Exchange commission details.
type CommissionListing struct {
	Contract
	ShipClass *string `json:"ship_class"`
}

type CommissionMetadata struct {
	Admiralty         bool   `json:"admiralty"`
	ShipClass         string `json:"ship_class"`
	RebuildValuePence int64  `json:"rebuild_value_pence"`
}

type CommissionActionRequest struct {
	ContractID string `json:"contract_id"`
}

type CommissionResult struct {
	ContractID       string     `json:"contract_id"`
	Status           string     `json:"status"`
	PortID           string     `json:"port_id"`
	ShipClass        string     `json:"ship_class"`
	PricePence       *int64     `json:"price_pence"`
	CollateralPence  *int64     `json:"collateral_pence"`
	BuilderCompanyID *string    `json:"builder_company_id"`
	BuyerCompanyID   *string    `json:"buyer_company_id"`
	Admiralty        bool       `json:"admiralty"`
	ExpiresAt        *time.Time `json:"expires_at"`
}

type DeliverCommissionRequest struct {
	ContractID string `json:"contract_id"`
	Name       string `json:"name"`
	BuildMode  string `json:"build_mode"`
}

type DeliverCommissionResult struct {
	ContractID              string  `json:"contract_id"`
	ShipID                  string  `json:"ship_id"`
	ShipName                string  `json:"ship_name"`
	ShipClass               string  `json:"ship_class"`
	PortID                  string  `json:"port_id"`
	BuildMode               string  `json:"build_mode"`
	BuildCostPence          *int64  `json:"build_cost_pence"`
	PricePaidPence          *int64  `json:"price_paid_pence"`
	CollateralReturnedPence *int64  `json:"collateral_returned_pence"`
	BuyerCompanyID          *string `json:"buyer_company_id"`
}
