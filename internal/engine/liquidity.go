package engine

import (
	"cmp"
	"math"
	"slices"
)

// depthCapacity counts only executable external levels at the candidate's
// immutable limit. Saturation cannot wrap deep books into negative demand.
func depthCapacity(levels []Level, limit int64, buy bool) int64 {
	var quantity int64
	for _, level := range levels {
		if level.PricePence <= 0 || level.PricePence > maximumPence || level.RemainingQuantity <= 0 ||
			buy && level.PricePence > limit || !buy && level.PricePence < limit {
			continue
		}
		if level.RemainingQuantity > math.MaxInt64-quantity {
			return math.MaxInt64
		}
		quantity += level.RemainingQuantity
	}
	return quantity
}

func depleteDepth(levels []Level, reserved int64, buy bool) []Level {
	if reserved <= 0 {
		return levels
	}
	remaining := append([]Level(nil), levels...)
	compare := func(left, right Level) int {
		order := cmp.Compare(left.PricePence, right.PricePence)
		if !buy {
			return -order
		}
		return order
	}
	if !slices.IsSortedFunc(remaining, compare) {
		slices.SortStableFunc(remaining, compare)
	}
	for i, level := range remaining {
		if level.PricePence <= 0 || level.PricePence > maximumPence || level.RemainingQuantity <= 0 {
			continue
		}
		take := min(reserved, level.RemainingQuantity)
		remaining[i].RemainingQuantity -= take
		reserved -= take
		if reserved == 0 {
			break
		}
	}
	return remaining
}

// withoutOwnOrders leaves upstream snapshots untouched. Own resting liquidity
// cannot fund our IOC, and would otherwise fabricate profitable market depth.
func withoutOwnOrders(markets map[string]Market, owned map[string]bool) map[string]Market {
	if len(owned) == 0 {
		return markets
	}
	filtered := make(map[string]Market, len(markets))
	for port, market := range markets {
		books := make([]CommodityBook, len(market.Books))
		for i, book := range market.Books {
			books[i] = book
			if book.Depth == nil {
				continue
			}
			depth := *book.Depth
			depth.Bids = excludeOrders(depth.Bids, owned)
			depth.Asks = excludeOrders(depth.Asks, owned)
			books[i].Depth = &depth
		}
		market.Books = books
		filtered[port] = market
	}
	return filtered
}

func excludeOrders(levels []Level, owned map[string]bool) []Level {
	result := make([]Level, 0, len(levels))
	for _, level := range levels {
		if !owned[level.OrderID] {
			result = append(result, level)
		}
	}
	return result
}

func (r *Runner) ownedOrders() map[string]bool {
	rows := decode[[]struct {
		ID string `json:"id"`
	}](r.snapshot, "orders")
	if len(rows) == 0 {
		return nil
	}
	owned := make(map[string]bool, len(rows))
	for _, row := range rows {
		owned[row.ID] = true
	}
	return owned
}
