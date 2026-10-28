package engine

import (
	"cmp"
	"encoding/json"
	"fmt"
	"math"
	"slices"
	"time"
)

const (
	retainedBookMaxGameDays = 12.0
	retainedBookBidLimit    = 10
	bookDriftMinGameDays    = 0.5
	bookDriftSampleLimit    = 200
	retainedBookSource      = "our quay book "
)

// RetainedBid keeps only price and tons after removing our orders. Compact keys
// keep a full 30-port, 30-commodity memory within the snapshot budget.
type RetainedBid struct {
	PricePence        int64 `json:"p"`
	RemainingQuantity int64 `json:"q"`
}

type RetainedQuayBook struct {
	ObservedAt time.Time                `json:"observed_at"`
	Bids       map[string][]RetainedBid `json:"bids"`
}

type BookDriftSample struct {
	PortID          string  `json:"port_id"`
	CommodityID     string  `json:"commodity_id"`
	AgeGameDays     float64 `json:"age_game_days"`
	TopBidChangeBPS int64   `json:"top_bid_change_bps"`
}

func (r *Runner) recordQuayBooks(markets map[string]json.RawMessage, portIDs []string, catalog Catalog, clock Clock, now time.Time) {
	observed := decode[map[string]time.Time](r.snapshot, "_book_observed")
	if observed == nil {
		observed = make(map[string]time.Time)
	}
	memory := decode[map[string]RetainedQuayBook](r.snapshot, "_quay_book_memory")
	if memory == nil {
		memory = make(map[string]RetainedQuayBook)
	}
	drift := decode[[]BookDriftSample](r.snapshot, "_book_drift")
	owned := r.ownedOrders()
	fills := decode[[]PlayerBidSellFill](r.snapshot, "fills")
	for _, port := range portIDs {
		var book Market
		if json.Unmarshal(markets[port], &book) != nil || book.Visibility != "quay" {
			continue
		}
		meta := r.meta["books/"+port]
		if meta.Generation != r.generation || !meta.Complete || meta.Stale || meta.Error != "" || !meta.UpdatedAt.After(observed[port]) {
			continue
		}
		observed[port] = meta.UpdatedAt
		previous := memory[port]
		next := RetainedQuayBook{ObservedAt: meta.UpdatedAt, Bids: make(map[string][]RetainedBid, len(book.Books))}
		age, ageOK := quayBookAgeGameDays(meta.UpdatedAt, previous.ObservedAt, clock.Calendar.TimeScale)
		for _, commodity := range book.Books {
			if commodity.Depth == nil {
				continue
			}
			bids := retainedBookBids(commodity.Depth.Bids, owned)
			next.Bids[commodity.CommodityID] = bids
			oldBids := previous.Bids[commodity.CommodityID]
			if ageOK && age >= bookDriftMinGameDays && len(oldBids) > 0 && len(bids) > 0 &&
				oldBids[0].PricePence > 0 && oldBids[0].PricePence <= maximumPence &&
				!quayBidFilledByUs(fills, port, commodity.CommodityID, previous.ObservedAt, meta.UpdatedAt) {
				change := (bids[0].PricePence - oldBids[0].PricePence) * 10_000
				if change < 0 {
					change -= oldBids[0].PricePence - 1
				}
				drift = append(drift, BookDriftSample{
					PortID: port, CommodityID: commodity.CommodityID, AgeGameDays: age,
					TopBidChangeBPS: change / oldBids[0].PricePence,
				})
			}
		}
		memory[port] = next
	}
	observedRetention := max(24*time.Hour, time.Duration(scoutHintMaxGameHours)*time.Hour/time.Duration(max(1, clock.Calendar.TimeScale)))
	for port, at := range observed {
		if _, exists := findPort(catalog, port); !exists || now.Sub(at) > observedRetention {
			delete(observed, port)
		}
	}
	if clock.Calendar.TimeScale > 0 {
		for port, book := range memory {
			age, ok := quayBookAgeGameDays(now, book.ObservedAt, clock.Calendar.TimeScale)
			if !ok || age > retainedBookMaxGameDays {
				delete(memory, port)
			}
		}
	}
	if len(drift) > bookDriftSampleLimit {
		drift = drift[len(drift)-bookDriftSampleLimit:]
	}
	r.snapshot["_book_observed"] = encode(observed)
	r.snapshot["_quay_book_memory"] = encode(memory)
	r.snapshot["_book_drift"] = encode(drift)
}

func quayBidFilledByUs(fills []PlayerBidSellFill, port, commodity string, from, to time.Time) bool {
	for _, fill := range fills {
		if fill.PortID == port && fill.CommodityID == commodity && fill.Side == "ask" && fill.Quantity > 0 &&
			!fill.CreatedAt.Before(from) && !fill.CreatedAt.After(to) {
			return true
		}
	}
	return false
}

func retainedBookBids(levels []Level, owned map[string]bool) []RetainedBid {
	bids := make([]RetainedBid, 0, min(retainedBookBidLimit, len(levels)))
	for _, level := range levels {
		if owned[level.OrderID] || level.PricePence <= 0 || level.PricePence > maximumPence || level.RemainingQuantity <= 0 {
			continue
		}
		index := 0
		for index < len(bids) && bids[index].PricePence >= level.PricePence {
			index++
		}
		if index == retainedBookBidLimit {
			continue
		}
		if len(bids) < retainedBookBidLimit {
			bids = append(bids, RetainedBid{})
		}
		copy(bids[index+1:], bids[index:])
		bids[index] = RetainedBid{PricePence: level.PricePence, RemainingQuantity: level.RemainingQuantity}
	}
	return bids
}

func quayBookAgeGameDays(now, observed time.Time, timeScale int64) (float64, bool) {
	if observed.IsZero() || observed.After(now) || timeScale <= 0 {
		return 0, false
	}
	return now.Sub(observed).Seconds() * float64(timeScale) / 86_400, true
}

func retainedAdverseDriftBPS(samples []BookDriftSample) float64 {
	if len(samples) > bookDriftSampleLimit {
		samples = samples[len(samples)-bookDriftSampleLimit:]
	}
	var rates [bookDriftSampleLimit]float64
	n := 0
	for _, sample := range samples {
		if sample.AgeGameDays < bookDriftMinGameDays || math.IsNaN(sample.AgeGameDays) || math.IsInf(sample.AgeGameDays, 0) {
			continue
		}
		rates[n] = max(0, -float64(sample.TopBidChangeBPS)/sample.AgeGameDays)
		n++
	}
	if n < 30 {
		return 100
	}
	slices.Sort(rates[:n])
	return max(100, rates[int(math.Ceil(float64(n)*0.75))-1])
}

func fresherQuayBestBid(in PlanningInput, destination, commodity string, rememberedAt time.Time) int64 {
	meta := in.Metadata["books/"+destination]
	age, ageOK := quayBookAgeGameDays(in.Now, meta.UpdatedAt, in.Clock.Calendar.TimeScale)
	if !meta.Complete || meta.Stale || meta.Error != "" || !ageOK || age > 6.0/24 ||
		!meta.UpdatedAt.After(rememberedAt) || in.Markets[destination].Visibility != "quay" {
		return 0
	}
	if meta.UpstreamAsOf != nil {
		_, upper, clockOK := in.serverBounds()
		upstreamAge, upstreamOK := quayBookAgeGameDays(upper, *meta.UpstreamAsOf, in.Clock.Calendar.TimeScale)
		if !clockOK || !upstreamOK || upstreamAge > 6.0/24 {
			return 0
		}
	}
	entry := bookCommodity(in.Markets[destination], commodity)
	if entry == nil || entry.Depth == nil {
		return 0
	}
	var best int64
	for _, bid := range entry.Depth.Bids {
		if bid.PricePence > best && bid.PricePence <= maximumPence && bid.RemainingQuantity > 0 && !in.OwnedOrderIDs[bid.OrderID] {
			best = bid.PricePence
		}
	}
	return best
}

func retainedDestinationRevenue(in PlanningInput, destination, commodity string, quantity, bidCap int64) (int64, int64, int64, string, bool) {
	book, exists := in.QuayBookMemory[destination]
	age, ageOK := quayBookAgeGameDays(in.Now, book.ObservedAt, in.Clock.Calendar.TimeScale)
	if !exists || !ageOK || age > retainedBookMaxGameDays {
		return 0, 0, 0, "", false
	}
	if quayBid := fresherQuayBestBid(in, destination, commodity, book.ObservedAt); quayBid > 0 && (bidCap == 0 || quayBid < bidCap) {
		bidCap = quayBid
	}
	var levels [retainedBookBidLimit]Level
	bids := book.Bids[commodity]
	n := min(len(bids), len(levels))
	for i, bid := range bids[:n] {
		price := bid.PricePence
		if bidCap > 0 {
			price = min(price, bidCap)
		}
		levels[i] = Level{PricePence: price, RemainingQuantity: bid.RemainingQuantity}
	}
	// Missions consume the best remembered bids first, not a proportional share
	// of every level. The retained snapshot itself stays unchanged.
	slices.SortStableFunc(levels[:n], func(left, right Level) int {
		return cmp.Compare(right.PricePence, left.PricePence)
	})
	reserved := max(int64(0), in.ReservedRetainedBidTons[destination][commodity])
	for i, level := range levels[:n] {
		if level.PricePence <= 0 || level.PricePence > maximumPence || level.RemainingQuantity <= 0 {
			continue
		}
		take := min(reserved, level.RemainingQuantity)
		levels[i].RemainingQuantity -= take
		reserved -= take
	}
	fill, err := WalkDepth(levels[:n], quantity, false)
	if err != nil {
		return 0, 0, 0, "", false
	}
	haircut := math.Ceil(age * retainedAdverseDriftBPS(in.BookDrift))
	if haircut > 6_000 {
		// A larger age haircut cannot be covered by the 6,000 bps stress cap.
		return 0, 0, 0, "", false
	}
	h := int64(haircut)
	stressBPS := max(h, min(int64(6_000), 2*h+300))
	ageHaircut := ceilBPS(fill.GrossPence, h)
	stress := ceilBPS(fill.GrossPence, stressBPS) - ageHaircut
	limit := fill.LimitPence * (10_000 - stressBPS) / 10_000
	source := fmt.Sprintf("%s%.1f game days old; %d bps age haircut", retainedBookSource, age, h)
	return fill.GrossPence - ageHaircut, limit, stress, source, true
}
