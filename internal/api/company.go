package api

import (
	"context"
	"net/http"
)

// CreateCompany creates a new trading company under the authenticated player.
func (c *Client) CreateCompany(ctx context.Context, req CreateCompanyRequest) (*Company, error) {
	var company Company
	if err := c.do(ctx, http.MethodPost, "/companies", req, &company, PriorityNormal); err != nil {
		return nil, err
	}
	return &company, nil
}

// ListMyCompanies returns all companies directed by the authenticated player.
func (c *Client) ListMyCompanies(ctx context.Context) ([]Company, error) {
	var companies []Company
	if err := c.do(ctx, http.MethodGet, "/me/companies", nil, &companies, PriorityNormal); err != nil {
		return nil, err
	}
	return companies, nil
}

// GetCompany returns details for the company set via the client's company ID header.
func (c *Client) GetCompany(ctx context.Context) (*Company, error) {
	var company Company
	if err := c.do(ctx, http.MethodGet, "/company", nil, &company, PriorityNormal); err != nil {
		return nil, err
	}
	return &company, nil
}

// GetEconomy returns the financial summary for the current company.
func (c *Client) GetEconomy(ctx context.Context) (*CompanyEconomy, error) {
	var economy CompanyEconomy
	if err := c.do(ctx, http.MethodGet, "/company/economy", nil, &economy, PriorityLow); err != nil {
		return nil, err
	}
	return &economy, nil
}

// GetLedger returns the transaction history for the current company.
// Supports cursor-based pagination via params.
func (c *Client) GetLedger(ctx context.Context, params PaginationParams) ([]LedgerEntry, error) {
	query := BuildQueryString(map[string]string{
		"after":  params.After,
		"before": params.Before,
	})
	if params.Limit > 0 {
		if query == "" {
			query = "?"
		} else {
			query += "&"
		}
		query += "limit=" + itoa(params.Limit)
	}

	var entries []LedgerEntry
	if err := c.do(ctx, http.MethodGet, "/company/ledger"+query, nil, &entries, PriorityLow); err != nil {
		return nil, err
	}
	return entries, nil
}

// GetFullLedger fetches all ledger entries using pagination.
func (c *Client) GetFullLedger(ctx context.Context) ([]LedgerEntry, error) {
	return Paginate[LedgerEntry](ctx, c, "/company/ledger", PriorityLow)
}
