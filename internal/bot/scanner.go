package bot

import (
	"context"
	"time"

	"go.uber.org/zap"

	"github.com/DevYukine/go-tradewinds/internal/api"
	"github.com/DevYukine/go-tradewinds/internal/db"
	"gorm.io/gorm"
)

const (
	// baseScanInterval is the time between scanning each port.
	baseScanInterval = 4 * time.Second

	// maxScanInterval is the slowest scan interval under high rate limit pressure.
	maxScanInterval = 15 * time.Second
)

// Scanner rotates through all ports, fetching NPC prices for every good.
// It runs as a single goroutine shared across all companies to avoid
// duplicate API calls. Uses PriorityLow to yield to trade executions.
type Scanner struct {
	client     *api.Client
	world      *WorldCache
	priceCache *PriceCache
	limiter    *api.RateLimiter
	gormDB     *gorm.DB
	logger     *zap.Logger
}

// newScanner creates a new price scanner.
func newScanner(
	client *api.Client,
	world *WorldCache,
	priceCache *PriceCache,
	limiter *api.RateLimiter,
	gormDB *gorm.DB,
	logger *zap.Logger,
) *Scanner {
	return &Scanner{
		client:     client,
		world:      world,
		priceCache: priceCache,
		limiter:    limiter,
		gormDB:     gormDB,
		logger:     logger.Named("scanner"),
	}
}

// Run starts the scanning loop. Blocks until context is cancelled.
func (s *Scanner) Run(ctx context.Context) {
	s.logger.Info("price scanner starting",
		zap.Int("ports", len(s.world.Ports)),
		zap.Int("goods", len(s.world.Goods)),
	)

	portIdx := 0
	for {
		if err := ctx.Err(); err != nil {
			s.logger.Info("price scanner stopped")
			return
		}

		port := s.world.Ports[portIdx%len(s.world.Ports)]
		s.scanPort(ctx, &port)
		portIdx++

		// Adapt scan interval based on rate limit utilization.
		interval := s.adaptiveInterval()
		select {
		case <-ctx.Done():
			return
		case <-time.After(interval):
		}
	}
}

// scanPort fetches buy/sell prices for all goods at a single port
// using batch quotes.
func (s *Scanner) scanPort(ctx context.Context, port *api.Port) {
	goods := s.world.Goods

	// Build buy quote requests for all goods at this port.
	buyReqs := make([]api.QuoteRequest, len(goods))
	for i, good := range goods {
		buyReqs[i] = api.QuoteRequest{
			PortID:   port.ID,
			GoodID:   good.ID,
			Action:   "buy",
			Quantity: 1,
		}
	}

	buyResults, err := s.client.BatchQuotesWithPriority(ctx, buyReqs, api.PriorityLow)
	if err != nil {
		s.logger.Warn("batch buy quote failed for port",
			zap.String("port", port.Name),
			zap.Error(err),
		)
		return
	}

	// Build sell quote requests.
	sellReqs := make([]api.QuoteRequest, len(goods))
	for i, good := range goods {
		sellReqs[i] = api.QuoteRequest{
			PortID:   port.ID,
			GoodID:   good.ID,
			Action:   "sell",
			Quantity: 1,
		}
	}

	sellResults, err := s.client.BatchQuotesWithPriority(ctx, sellReqs, api.PriorityLow)
	if err != nil {
		s.logger.Warn("batch sell quote failed for port",
			zap.String("port", port.Name),
			zap.Error(err),
		)
		return
	}

	// Merge buy and sell prices into the cache.
	updated := 0
	var observations []db.PriceObservation

	for i, good := range goods {
		var buyPrice, sellPrice int

		if i < len(buyResults) && buyResults[i].Status == 200 && buyResults[i].Quote != nil {
			buyPrice = buyResults[i].Quote.UnitPrice
		}
		if i < len(sellResults) && sellResults[i].Status == 200 && sellResults[i].Quote != nil {
			sellPrice = sellResults[i].Quote.UnitPrice
		}

		if buyPrice > 0 || sellPrice > 0 {
			s.priceCache.Set(port.ID, good.ID, buyPrice, sellPrice)
			updated++

			observations = append(observations, db.PriceObservation{
				PortID:    port.ID.String(),
				GoodID:    good.ID.String(),
				BuyPrice:  buyPrice,
				SellPrice: sellPrice,
			})
		}
	}

	// Batch insert all observations for this port.
	if len(observations) > 0 {
		s.gormDB.CreateInBatches(&observations, len(observations))
	}

	s.logger.Debug("port scanned",
		zap.String("port", port.Name),
		zap.Int("prices_updated", updated),
	)
}

// adaptiveInterval returns the scan interval based on current rate limit pressure.
func (s *Scanner) adaptiveInterval() time.Duration {
	utilization := s.limiter.Utilization()

	switch {
	case utilization > 0.80:
		return maxScanInterval
	case utilization > 0.60:
		return baseScanInterval * 2
	default:
		return baseScanInterval
	}
}
