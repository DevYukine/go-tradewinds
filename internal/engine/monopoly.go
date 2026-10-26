package engine

import (
	"slices"
	"time"
)

type Monopoly struct {
	ID              string    `json:"id"`
	BankingRegion   string    `json:"banking_region"`
	CommodityID     string    `json:"commodity_id"`
	HolderCompanyID string    `json:"holder_company_id"`
	ExpiresAt       time.Time `json:"expires_at"`
	InForce         bool      `json:"in_force"`
}

func withoutMonopolizedNPCAsks(markets map[string]Market, ports []Port, monopolies []Monopoly, ownCompanyIDs map[string]bool, now time.Time) map[string]Market {
	type monopolyKey struct {
		region    string
		commodity string
	}
	blocked := make(map[monopolyKey]bool)
	for _, monopoly := range monopolies {
		if monopoly.InForce && monopoly.ExpiresAt.After(now) && !ownCompanyIDs[monopoly.HolderCompanyID] {
			blocked[monopolyKey{monopoly.BankingRegion, monopoly.CommodityID}] = true
		}
	}
	regions := make(map[string]string, len(ports))
	for _, port := range ports {
		regions[port.ID] = port.BankingRegion
	}
	filtered := make(map[string]Market, len(markets))
	for port, market := range markets {
		books := slices.Clone(market.Books)
		for i, book := range books {
			if book.Depth == nil {
				continue
			}
			depth := *book.Depth
			depth.Bids = slices.Clone(depth.Bids)
			depth.Asks = slices.Clone(depth.Asks)
			if blocked[monopolyKey{regions[port], book.CommodityID}] {
				depth.Asks = slices.DeleteFunc(depth.Asks, func(level Level) bool { return level.IsNPC })
			}
			books[i].Depth = &depth
		}
		market.Books = books
		filtered[port] = market
	}
	return filtered
}

func (r *Runner) monopolyMarketView(markets map[string]Market, catalog Catalog, clock Clock) map[string]Market {
	profile := decode[struct {
		Company struct {
			ID string `json:"id"`
		} `json:"company"`
	}](r.snapshot, "profile")
	own := map[string]bool{}
	if profile.Company.ID != "" {
		own[profile.Company.ID] = true
	}
	now := time.Now()
	if lower, _, ok := serverBounds(now, clock, r.meta["clock"], r.generation); ok {
		now = lower
	}
	// Failed refreshes retain last-good grants, but cannot prolong an expired one.
	return withoutMonopolizedNPCAsks(markets, catalog.Ports, decode[[]Monopoly](r.snapshot, "monopolies"), own, now)
}
