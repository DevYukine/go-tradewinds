package gameapi

import "context"

type PassengerFareSummary struct {
	Bands []PassengerFareBand `json:"bands"`
}

type PassengerFareBand struct {
	MinNM                  int64  `json:"min_nm"`
	MaxNM                  *int64 `json:"max_nm"`
	Fares                  int64  `json:"fares"`
	MedianFarePerHeadPence *int64 `json:"median_fare_per_head_pence"`
}

// PassengerFaresSummary shares the host limiter but never sends account credentials.
func (c *Client) PassengerFaresSummary(ctx context.Context, out *PassengerFareSummary) error {
	if c.initErr != nil {
		return c.initErr
	}
	public := New(c.base.String(), "", c.log)
	c.mu.RLock()
	public.SetObserver(c.observer)
	c.mu.RUnlock()
	return public.GetOnce(ctx, "/v1/passengers/fares/summary", out)
}
