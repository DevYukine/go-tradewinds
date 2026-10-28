package sim

import (
	"encoding/json"
	"errors"
	"math"
	"sort"
	"time"

	"github.com/DevYukine/go-tradewinds/internal/engine"
)

type RecordedFill struct {
	ID          string    `json:"id"`
	OrderID     string    `json:"order_id"`
	PortID      string    `json:"port_id"`
	CommodityID string    `json:"commodity_id"`
	Side        string    `json:"side"`
	Quantity    int64     `json:"quantity"`
	PricePence  int64     `json:"price_pence"`
	CreatedAt   time.Time `json:"created_at"`
}

type ExecutedLevel struct {
	OrderID      string `json:"order_id"`
	PricePence   int64  `json:"price_pence"`
	Quantity     int64  `json:"quantity"`
	GrossPence   int64  `json:"gross_pence"`
	CustomsPence int64  `json:"customs_pence"`
	IsNPC        bool   `json:"is_npc"`
}

type TradeResult struct {
	Quantity     int64           `json:"quantity"`
	GrossPence   int64           `json:"gross_pence"`
	CustomsPence int64           `json:"customs_pence"`
	Coverage     Coverage        `json:"coverage"`
	Levels       []ExecutedLevel `json:"levels"`
}

type marketSide struct {
	levels   []engine.Level
	consumed map[string]int64
	addbacks map[string]int64
	folded   map[string]bool
	stock    float64
	epoch    time.Time
	hour     float64
	asOf     time.Time
}

type MarketModel struct {
	regen   float64
	sides   map[string]*marketSide
	books   map[string]BookObservation
	fills   map[string]RecordedFill
	fillIDs []string
}

func NewMarketModel(regen float64) *MarketModel {
	return &MarketModel{regen: math.Max(0, math.Min(1, regen)), sides: make(map[string]*marketSide), books: make(map[string]BookObservation), fills: make(map[string]RecordedFill)}
}

func sideKey(port, commodity, side string) string { return port + "\x00" + commodity + "\x00" + side }

func tradeSide(side string) string {
	switch side {
	case "bid":
		return "buy"
	case "ask":
		return "sell"
	default:
		return side
	}
}

// A disappearing level can be an exhausted order. Only a newly observed NPC
// order establishes a re-quote; removal alone does not reset the quote epoch.
func npcRequoted(previous, current []engine.Level) bool {
	known := make(map[string]bool, len(previous))
	for _, level := range previous {
		if level.IsNPC {
			known[level.OrderID] = true
		}
	}
	for _, level := range current {
		if level.IsNPC && !known[level.OrderID] {
			return true
		}
	}
	return false
}

func (m *MarketModel) Observe(view FrameView) {
	var fills []RecordedFill
	_ = json.Unmarshal(view.Snapshot["fills"], &fills)
	for _, fill := range fills {
		fill.Side = tradeSide(fill.Side)
		if fill.ID == "" {
			encoded, _ := json.Marshal(fill)
			fill.ID = string(encoded)
		}
		if !fill.CreatedAt.After(view.At) && fill.Quantity > 0 {
			m.fills[fill.ID] = fill
		}
	}
	if len(m.fillIDs) != len(m.fills) {
		m.fillIDs = sortedKeys(m.fills)
	}
	m.books = cloneMap(view.Books)
	for _, port := range sortedKeys(view.Books) {
		book := view.Books[port]
		if book.Status == Dark || book.Status == "" {
			continue
		}
		for _, commodity := range book.Market.Books {
			if commodity.Depth == nil {
				continue
			}
			m.observeSide(port, commodity.CommodityID, "buy", commodity.Depth.Asks, book.AsOf, view.GameHour)
			m.observeSide(port, commodity.CommodityID, "sell", commodity.Depth.Bids, book.AsOf, view.GameHour)
		}
	}
}

func (m *MarketModel) observeSide(port, commodity, side string, levels []engine.Level, asOf time.Time, hour float64) {
	key := sideKey(port, commodity, side)
	state := m.sides[key]
	if state == nil {
		state = &marketSide{consumed: make(map[string]int64), addbacks: make(map[string]int64), folded: make(map[string]bool), hour: hour}
		m.sides[key] = state
	}
	requoted := !state.asOf.IsZero() && npcRequoted(state.levels, levels)
	if requoted {
		// Fills recorded since the last observation may still restore
		// depth on the outgoing quote. Re-apply them against the old
		// levels first so the fold below nets every recorded fill, not
		// only the ones known when the outgoing quote was last observed.
		m.applyFills(state, port, commodity, side, state.levels, asOf, true)
		deviation := state.stock
		for _, old := range state.levels {
			if old.IsNPC {
				deviation += float64(state.consumed[old.OrderID] - state.addbacks[old.OrderID])
				delete(state.consumed, old.OrderID)
			}
		}
		periods := math.Max(0, hour-state.hour) / 6
		state.stock = deviation * math.Pow(1-m.regen, periods)
		state.epoch, state.hour = asOf, hour
	}
	alive := make(map[string]bool, len(levels))
	for _, level := range levels {
		alive[level.OrderID] = true
	}
	var exhausted []engine.Level
	for _, old := range state.levels {
		if !alive[old.OrderID] && !requoted && old.IsNPC {
			old.RemainingQuantity = 0
			exhausted = append(exhausted, old)
			alive[old.OrderID] = true
		}
	}
	state.levels = append(state.levels[:0], levels...)
	state.levels = append(state.levels, exhausted...)
	state.asOf = asOf
	for id := range state.consumed {
		if !alive[id] {
			delete(state.consumed, id)
		}
	}
	m.applyFills(state, port, commodity, side, state.levels, asOf, false)
}

// applyFills rebuilds state.addbacks for the given level list from every
// recorded fill that matches it. A fill whose exact stable OrderID appears in
// the list always credits its level, even when the fill predates the quote
// epoch: the order may only have become visible after the fill already
// consumed it. Fills without an OrderID still need the epoch guard, because a
// price alone cannot prove which quote the fill traded. When folding is true
// the credits feed a re-quote fold, so a fill credited to an outgoing NPC
// level is marked folded: its restoration moves into the signed stock
// deviation and must never re-credit the replacement quote.
func (m *MarketModel) applyFills(state *marketSide, port, commodity, side string, levels []engine.Level, asOf time.Time, folding bool) {
	clear(state.addbacks)
	for _, id := range m.fillIDs {
		fill := m.fills[id]
		if fill.PortID != port || fill.CommodityID != commodity || fill.Side != side || fill.CreatedAt.After(asOf) || state.folded[fill.ID] {
			continue
		}
		for _, level := range levels {
			if fill.OrderID != "" && fill.OrderID != level.OrderID || fill.OrderID == "" && fill.PricePence != level.PricePence ||
				level.IsNPC && !state.epoch.IsZero() && fill.CreatedAt.Before(state.epoch) && fill.OrderID == "" {
				continue
			}
			state.addbacks[level.OrderID] += fill.Quantity
			if folding && level.IsNPC {
				state.folded[fill.ID] = true
			}
			break
		}
	}
}

func (m *MarketModel) effective(port, commodity, side string) []engine.Level {
	state := m.sides[sideKey(port, commodity, side)]
	if state == nil {
		return nil
	}
	levels := append([]engine.Level(nil), state.levels...)
	sort.SliceStable(levels, func(i, j int) bool {
		if levels[i].PricePence == levels[j].PricePence {
			return levels[i].OrderID < levels[j].OrderID
		}
		if side == "buy" {
			return levels[i].PricePence < levels[j].PricePence
		}
		return levels[i].PricePence > levels[j].PricePence
	})
	stock := int64(math.Round(state.stock))
	for i := range levels {
		level := &levels[i]
		level.RemainingQuantity = max(0, level.RemainingQuantity+state.addbacks[level.OrderID]-state.consumed[level.OrderID])
		if !level.IsNPC {
			continue
		}
		if stock < 0 {
			level.RemainingQuantity -= stock
			stock = 0
		} else {
			take := min(stock, level.RemainingQuantity)
			level.RemainingQuantity -= take
			stock -= take
		}
	}
	return levels
}

func (m *MarketModel) Book(port string) (engine.Market, Coverage) {
	book, ok := m.books[port]
	if !ok || book.Status == Dark || book.Status == "" {
		return engine.Market{PortID: port}, Dark
	}
	market := book.Market
	market.Books = append([]engine.CommodityBook(nil), market.Books...)
	for i := range market.Books {
		commodity := &market.Books[i]
		if commodity.Depth == nil {
			continue
		}
		depth := *commodity.Depth
		depth.Asks = m.effective(port, commodity.CommodityID, "buy")
		depth.Bids = m.effective(port, commodity.CommodityID, "sell")
		commodity.Depth = &depth
	}
	return market, book.Status
}

func (m *MarketModel) Execute(port, commodity, side string, quantity, limit int64) (TradeResult, error) {
	side = tradeSide(side)
	if quantity < 0 || limit < 0 || side != "buy" && side != "sell" {
		return TradeResult{}, errors.New("invalid IOC order")
	}
	book, coverage := m.Book(port)
	result := TradeResult{Coverage: coverage, Levels: make([]ExecutedLevel, 0)}
	if coverage == Dark {
		return result, errors.New("market is dark")
	}
	state := m.sides[sideKey(port, commodity, side)]
	if state == nil {
		return result, nil
	}
	for _, level := range m.effective(port, commodity, side) {
		if result.Quantity >= quantity {
			break
		}
		if level.PricePence < 0 || side == "buy" && level.PricePence > limit || side == "sell" && level.PricePence < limit {
			continue
		}
		filled := min(quantity-result.Quantity, level.RemainingQuantity)
		if filled <= 0 {
			continue
		}
		if level.PricePence > 0 && filled > math.MaxInt64/level.PricePence {
			return TradeResult{}, errors.New("IOC gross overflows pence")
		}
		gross := filled * level.PricePence
		customs := int64(0)
		if side == "buy" {
			customs = ceilBPS(gross, book.CustomsBPS)
		}
		result.Quantity += filled
		result.GrossPence += gross
		result.CustomsPence += customs
		result.Levels = append(result.Levels, ExecutedLevel{OrderID: level.OrderID, PricePence: level.PricePence, Quantity: filled, GrossPence: gross, CustomsPence: customs, IsNPC: level.IsNPC})
	}
	// Apply once only after the whole receipt is known to be representable.
	for _, level := range result.Levels {
		state.consumed[level.OrderID] += level.Quantity
	}
	return result, nil
}

func ceilBPS(gross, bps int64) int64 {
	if gross <= 0 || bps <= 0 {
		return 0
	}
	return gross/10000*bps + (gross%10000*bps+9999)/10000
}
