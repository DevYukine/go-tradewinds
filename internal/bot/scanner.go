package bot

import (
	"context"
	"time"

	"go.uber.org/zap"

	"github.com/DevYukine/go-tradewinds/internal/api"
	"github.com/DevYukine/go-tradewinds/internal/cache"
	"github.com/DevYukine/go-tradewinds/internal/db"
	"gorm.io/gorm"
)

const (
	// baseScanInterval is the time between scanning each port.
	baseScanInterval = 8 * time.Second

	// maxScanInterval is the slowest scan interval under high rate limit pressure.
	maxScanInterval = 30 * time.Second
)

// Scanner rotates through all ports, fetching NPC prices for every good.
// It runs as a single goroutine shared across all companies to avoid
// duplicate API calls. Uses PriorityLow to yield to trade executions.
type Scanner struct {
	client         *api.Client
	world          *WorldCache
	priceCache     *PriceCache
	profitAnalyzer *ProfitAnalyzer
	limiter        *api.RateLimiter
	redis          *cache.RedisCache
	gormDB         *gorm.DB
	logger         *zap.Logger
}

// newScanner creates a new price scanner.
func newScanner(
	client *api.Client,
	world *WorldCache,
	priceCache *PriceCache,
	profitAnalyzer *ProfitAnalyzer,
	limiter *api.RateLimiter,
	redis *cache.RedisCache,
	gormDB *gorm.DB,
	logger *zap.Logger,
) *Scanner {
	return &Scanner{
		client:         client,
		world:          world,
		priceCache:     priceCache,
		profitAnalyzer: profitAnalyzer,
		limiter:        limiter,
		redis:          redis,
		gormDB:         gormDB,
		logger:         logger.Named("scanner"),
	}
}

// Run starts the scanning loop. Blocks until context is cancelled.
func (s *Scanner) Run(ctx context.Context) {
	s.logger.Info("price scanner starting",
		zap.Int("ports", s.world.PortCount()),
	)

	// Restore scanner position from Redis.
	portIdx := s.redis.LoadScannerIndex(ctx)

	for {
		if err := ctx.Err(); err != nil {
			s.logger.Info("price scanner stopped")
			return
		}

		port, totalPorts := s.world.GetPortAtIndex(portIdx)
		if totalPorts == 0 {
			s.logger.Warn("no ports available, waiting...")
			select {
			case <-ctx.Done():
				return
			case <-time.After(10 * time.Second):
			}
			continue
		}
		s.scanPort(ctx, &port)
		portIdx++

		// Recompute trade opportunities after completing a full scan cycle.
		if portIdx%totalPorts == 0 && s.profitAnalyzer != nil {
			s.profitAnalyzer.Recompute()
			s.logger.Debug("profit analyzer recomputed after full scan cycle")
		}

		// Persist scanner position to Redis.
		s.redis.SaveScannerIndex(ctx, portIdx)

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

		if i < len(buyResults) && buyResults[i].Status == "success" && buyResults[i].Quote != nil {
			buyPrice = buyResults[i].Quote.UnitPrice
		}
		if i < len(sellResults) && sellResults[i].Status == "success" && sellResults[i].Quote != nil {
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

// ScanPorts scans prices for the given ports immediately (on-demand).
// Used to populate price data for newly discovered ports so the agent
// can evaluate them as destinations without waiting for the regular scan cycle.
func (s *Scanner) ScanPorts(ctx context.Context, ports []api.Port) {
	for i := range ports {
		if ctx.Err() != nil {
			return
		}
		s.logger.Info("on-demand scan for newly discovered port",
			zap.String("port", ports[i].Name),
		)
		s.scanPort(ctx, &ports[i])
	}
	// Recompute opportunities so new ports appear in trade analysis.
	if s.profitAnalyzer != nil && len(ports) > 0 {
		s.profitAnalyzer.Recompute()
	}
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
