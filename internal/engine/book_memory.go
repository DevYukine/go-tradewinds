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
	// Only older commodity timestamps need an override. Keeping the common
	// capture time once preserves the existing snapshot size bound.
	CommodityObservedAt map[string]time.Time `json:"at,omitempty"`
}

func (book RetainedQuayBook) observedAt(commodity string) time.Time {
	if at, exists := book.CommodityObservedAt[commodity]; exists {
		return at
	}
	return book.ObservedAt
}

type BookDriftSample struct {
	PortID          string    `json:"port_id"`
	CommodityID     string    `json:"commodity_id"`
	AgeGameDays     float64   `json:"age_game_days"`
	TopBidChangeBPS int64     `json:"top_bid_change_bps"`
	FromObservedAt  time.Time `json:"from_observed_at,omitempty"`
	ToObservedAt    time.Time `json:"to_observed_at,omitempty"`
}

func (r *Runner) recordQuayBooks(markets map[string]json.RawMessage, portIDs []string, catalog Catalog, clock Clock, now time.Time) {
	shocks, eventsValid := decodeMarketShocks(r.snapshot)
	_, eventsTracked := r.meta["world_events"]
	_, eventsRecorded := r.snapshot["world_events"]
	input := PlanningInput{
		Now: now, Clock: clock, Metadata: r.meta, Generation: r.generation,
		MarketShocks: shocks, marketEventsInvalid: !eventsValid && (eventsTracked || eventsRecorded),
	}
	_, serverNow, validClock := input.serverBounds()
	if !validClock {
		return
	}
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
		from, to, validObservation := input.marketObservationBounds("books/" + port)
		if !validObservation {
			continue
		}
		observed[port] = meta.UpdatedAt
		previous := memory[port]
		next := RetainedQuayBook{ObservedAt: to, Bids: make(map[string][]RetainedBid, len(book.Books))}
		for _, commodity := range book.Books {
			if commodity.Depth == nil {
				continue
			}
			id := commodity.CommodityID
			oldAt := previous.observedAt(id)
			oldBids := previous.Bids[id]
			shock, shocked := input.marketShockOverlapping(port, id, from, to)
			if shocked {
				_, oldShocked := input.marketShockOverlapping(port, id, oldAt, oldAt)
				if len(oldBids) > 0 && !oldAt.IsZero() && oldAt.Before(shock.StartsAt) && !oldShocked {
					next.Bids[id] = oldBids
					if next.CommodityObservedAt == nil {
						next.CommodityObservedAt = make(map[string]time.Time)
					}
					next.CommodityObservedAt[id] = oldAt
				}
				continue
			}
			bids := retainedBookBids(commodity.Depth.Bids, owned)
			next.Bids[id] = bids
			age, ageOK := quayBookAgeGameDays(to, oldAt, clock.Calendar.TimeScale)
			_, crossedShock := input.marketShockOverlapping(port, id, oldAt, to)
			if ageOK && age >= bookDriftMinGameDays && len(oldBids) > 0 && len(bids) > 0 &&
				oldBids[0].PricePence > 0 && oldBids[0].PricePence <= maximumPence &&
				!crossedShock && !input.marketEventUnavailable() &&
				!quayBidFilledByUs(fills, port, id, oldAt, to) {
				change := (bids[0].PricePence - oldBids[0].PricePence) * 10_000
				if change < 0 {
					change -= oldBids[0].PricePence - 1
				}
				drift = append(drift, BookDriftSample{
					PortID: port, CommodityID: id, AgeGameDays: age,
					TopBidChangeBPS: change / oldBids[0].PricePence,
					FromObservedAt:  oldAt, ToObservedAt: to,
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
			for commodity := range book.Bids {
				age, valid := quayBookAgeGameDays(serverNow, book.observedAt(commodity), clock.Calendar.TimeScale)
				if !valid || age > retainedBookMaxGameDays {
					delete(book.Bids, commodity)
					delete(book.CommodityObservedAt, commodity)
				}
			}
			if len(book.Bids) == 0 {
				delete(memory, port)
			}
		}
	}
	drift = slices.DeleteFunc(drift, func(sample BookDriftSample) bool {
		_, overlaps := input.marketShockOverlapping(sample.PortID, sample.CommodityID, sample.FromObservedAt, sample.ToObservedAt)
		return overlaps
	})
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

func fresherQuayBestBid(in PlanningInput, destination, commodity string, rememberedAt, arrival time.Time) int64 {
	meta := in.Metadata["books/"+destination]
	from, observedAt, bounded := in.marketObservationBounds("books/" + destination)
	_, upper, clockOK := in.serverBounds()
	age, ageOK := quayBookAgeGameDays(upper, observedAt, in.Clock.Calendar.TimeScale)
	if !meta.Complete || meta.Stale || meta.Error != "" || !bounded || !clockOK || !ageOK || age > 6.0/24 ||
		!observedAt.After(rememberedAt) || in.Markets[destination].Visibility != "quay" {
		return 0
	}
	if _, reason := in.marketObservationPolicy(destination, commodity, from, observedAt, arrival); reason != "" {
		return 0
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

func retainedDestinationRevenue(in PlanningInput, destination, commodity string, quantity, bidCap int64, arrival time.Time) (int64, int64, int64, string, string, bool) {
	book, exists := in.QuayBookMemory[destination]
	observedAt := book.observedAt(commodity)
	_, currentUpper, clockOK := in.serverBounds()
	currentAge, currentAgeOK := quayBookAgeGameDays(currentUpper, observedAt, in.Clock.Calendar.TimeScale)
	ageAt := currentUpper
	required, reason := in.marketObservationPolicy(destination, commodity, observedAt, observedAt, arrival)
	if reason != "" {
		return 0, 0, 0, "", reason, false
	}
	for _, shock := range in.MarketShocks[destination][commodity] {
		if observedAt.Before(shock.StartsAt) && !ageAt.Before(shock.StartsAt) &&
			(arrival.IsZero() || arrival.After(shock.ExpiresAt)) && arrival.After(ageAt) {
			// Pre-event levels must survive the whole remaining voyage. This
			// additional aging is specific to expiring event replacements.
			ageAt = arrival
		}
	}
	age, ageOK := quayBookAgeGameDays(ageAt, observedAt, in.Clock.Calendar.TimeScale)
	if !exists || !clockOK || !currentAgeOK || currentAge > retainedBookMaxGameDays || !ageOK {
		return 0, 0, 0, "", "retained destination bids are absent or too old", false
	}
	if quayBid := fresherQuayBestBid(in, destination, commodity, observedAt, arrival); quayBid > 0 && (bidCap == 0 || quayBid < bidCap) {
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
	reserveBidLevels(levels[:n], in.ReservedRetainedBidTons[destination][commodity])
	fill, err := WalkDepth(levels[:n], quantity, false)
	if err != nil {
		return 0, 0, 0, "", "unreserved retained destination bids cannot fill the whole lot", false
	}
	haircut := math.Ceil(age * retainedAdverseDriftBPS(in.BookDrift))
	if haircut > 6_000 {
		// A larger age haircut cannot be covered by the 6,000 bps stress cap.
		return 0, 0, 0, "", "retained destination age haircut exceeds the stress cap", false
	}
	h := int64(haircut)
	stressBPS := max(h, min(int64(6_000), 2*h+300))
	ageHaircut := ceilBPS(fill.GrossPence, h)
	stress := ceilBPS(fill.GrossPence, stressBPS) - ageHaircut
	limit := fill.LimitPence * (10_000 - stressBPS) / 10_000
	source := fmt.Sprintf("%s%.1f game days old; %d bps age haircut", retainedBookSource, age, h)
	revenue := fill.GrossPence - ageHaircut
	return revenue, limit, eventPriceStress(revenue, stress, required), source, "", required
}

// reserveBidLevels consumes the best bids first. Callers must own the slice.
func reserveBidLevels(levels []Level, reserved int64) {
	if reserved <= 0 {
		return
	}
	slices.SortStableFunc(levels, func(left, right Level) int {
		return cmp.Compare(right.PricePence, left.PricePence)
	})
	for i, level := range levels {
		if level.PricePence <= 0 || level.PricePence > maximumPence || level.RemainingQuantity <= 0 {
			continue
		}
		take := min(reserved, level.RemainingQuantity)
		levels[i].RemainingQuantity -= take
		reserved -= take
		if reserved == 0 {
			return
		}
	}
}
