package bot

import (
	"context"
	"fmt"
	"math/rand/v2"
	"sync"
	"time"

	"github.com/google/uuid"
	"go.uber.org/fx"
	"go.uber.org/zap"
	"gorm.io/gorm"

	"github.com/DevYukine/go-tradewinds/internal/agent"
	"github.com/DevYukine/go-tradewinds/internal/api"
	"github.com/DevYukine/go-tradewinds/internal/config"
	"github.com/DevYukine/go-tradewinds/internal/db"
)

// Module provides the Manager to the fx DI container and starts it via lifecycle hooks.
var Module = fx.Module("bot",
	fx.Provide(NewManager),
	fx.Invoke(RegisterManager),
)

// Manager orchestrates all company runners, sharing a single auth token,
// rate limiter, world cache, and price cache across all companies.
type Manager struct {
	cfg         *config.Config
	gormDB      *gorm.DB
	logger      *zap.Logger
	baseClient  *api.Client
	rateLimiter *api.RateLimiter
	worldData   *WorldCache
	priceCache  *PriceCache
	agent       agent.Agent
	registry    Registry
	scaler      *Scaler
	companies   map[string]*CompanyRunner
	mu          sync.RWMutex
	wg          sync.WaitGroup
}

// NewManager creates the bot manager with all its dependencies.
func NewManager(
	cfg *config.Config,
	gormDB *gorm.DB,
	logger *zap.Logger,
	agnt agent.Agent,
	registry Registry,
) *Manager {
	rateLimiter := api.NewRateLimiter(cfg.RateLimitPerMinute, logger)
	baseClient := api.NewClient(cfg.BaseURL, rateLimiter, logger)

	return &Manager{
		cfg:         cfg,
		gormDB:      gormDB,
		logger:      logger.Named("manager"),
		baseClient:  baseClient,
		rateLimiter: rateLimiter,
		priceCache:  NewPriceCache(),
		agent:       agnt,
		registry:    registry,
		scaler:      NewScaler(rateLimiter, logger),
		companies:   make(map[string]*CompanyRunner),
	}
}

// RegisterManager hooks the manager into the fx lifecycle.
func RegisterManager(lc fx.Lifecycle, m *Manager) {
	ctx, cancel := context.WithCancel(context.Background())

	lc.Append(fx.Hook{
		OnStart: func(startCtx context.Context) error {
			return m.Start(startCtx, ctx)
		},
		OnStop: func(_ context.Context) error {
			cancel()
			m.wg.Wait()
			m.logger.Info("all company runners stopped")
			return nil
		},
	})
}

// Start authenticates, loads world data, creates companies, and spawns runners.
func (m *Manager) Start(startCtx context.Context, runCtx context.Context) error {
	m.logger.Info("bot manager starting",
		zap.String("base_url", m.cfg.BaseURL),
		zap.String("agent", m.agent.Name()),
	)

	// 1. Login.
	token, err := m.baseClient.Login(startCtx, m.cfg.PlayerEmail, m.cfg.PlayerPassword)
	if err != nil {
		return fmt.Errorf("login failed: %w", err)
	}
	m.logger.Info("authenticated", zap.Int("token_length", len(token)))

	// 2. Verify auth.
	player, err := m.baseClient.Me(startCtx)
	if err != nil {
		return fmt.Errorf("auth verification failed: %w", err)
	}
	m.logger.Info("player verified",
		zap.String("name", player.Name),
		zap.String("id", player.ID.String()),
	)

	// 3. Load world data.
	worldData, err := LoadWorldData(startCtx, m.baseClient, m.logger)
	if err != nil {
		return fmt.Errorf("failed to load world data: %w", err)
	}
	m.worldData = worldData

	// 4. Calculate allocation.
	allocations := m.scaler.CalculateAllocation(
		m.cfg.StrategyAllocation,
		m.cfg.RateLimitPerMinute,
		worldData.Ports,
	)

	// 5. Get existing companies.
	existingCompanies, err := m.baseClient.ListMyCompanies(startCtx)
	if err != nil {
		return fmt.Errorf("failed to list existing companies: %w", err)
	}
	m.logger.Info("existing companies found", zap.Int("count", len(existingCompanies)))

	// Build ticker -> company lookup.
	companyByTicker := make(map[string]*api.Company, len(existingCompanies))
	for i := range existingCompanies {
		companyByTicker[existingCompanies[i].Ticker] = &existingCompanies[i]
	}

	// 6. Create/find companies and spawn runners.
	for _, alloc := range allocations {
		factory, ok := m.registry[alloc.Strategy]
		if !ok {
			m.logger.Warn("no strategy factory registered, skipping",
				zap.String("strategy", alloc.Strategy),
			)
			continue
		}

		for i := range alloc.Count {
			ticker := m.buildTicker(alloc.Strategy, i+1)
			name := m.buildCompanyName(alloc.Strategy, i+1)
			homePortID := alloc.HomePorts[i]

			company, err := m.ensureCompany(startCtx, companyByTicker, name, ticker, homePortID)
			if err != nil {
				m.logger.Error("failed to ensure company",
					zap.String("ticker", ticker),
					zap.Error(err),
				)
				continue
			}

			runner, err := m.setupRunner(company, alloc.Strategy, factory, homePortID)
			if err != nil {
				m.logger.Error("failed to setup company runner",
					zap.String("ticker", ticker),
					zap.Error(err),
				)
				continue
			}

			m.mu.Lock()
			m.companies[company.ID.String()] = runner
			m.mu.Unlock()

			// Spawn runner with jittered start delay.
			startDelay := time.Duration(rand.Int64N(int64(10 * time.Second)))
			m.wg.Add(1)
			go func(r *CompanyRunner, delay time.Duration) {
				defer m.wg.Done()
				select {
				case <-runCtx.Done():
					return
				case <-time.After(delay):
				}
				r.Run(runCtx)
			}(runner, startDelay)

			m.logger.Info("company runner spawned",
				zap.String("ticker", ticker),
				zap.String("company_id", company.ID.String()),
				zap.String("strategy", alloc.Strategy),
				zap.Duration("start_delay", startDelay),
			)
		}
	}

	m.logger.Info("bot manager started",
		zap.Int("total_companies", len(m.companies)),
	)

	return nil
}

// ensureCompany finds an existing company by ticker or creates a new one.
func (m *Manager) ensureCompany(
	ctx context.Context,
	existing map[string]*api.Company,
	name, ticker string,
	homePortID uuid.UUID,
) (*api.Company, error) {
	if company, ok := existing[ticker]; ok {
		m.logger.Info("found existing company",
			zap.String("ticker", ticker),
			zap.String("id", company.ID.String()),
		)
		return company, nil
	}

	m.logger.Info("creating new company",
		zap.String("name", name),
		zap.String("ticker", ticker),
	)

	company, err := m.baseClient.CreateCompany(ctx, api.CreateCompanyRequest{
		Name:       name,
		Ticker:     ticker,
		HomePortID: homePortID,
	})
	if err != nil {
		return nil, fmt.Errorf("failed to create company %s: %w", ticker, err)
	}

	// Small delay between company creations to avoid burst.
	time.Sleep(200 * time.Millisecond)

	return company, nil
}

// setupRunner creates a CompanyRunner with all dependencies wired up.
func (m *Manager) setupRunner(
	company *api.Company,
	strategyName string,
	factory StrategyFactory,
	homePortID uuid.UUID,
) (*CompanyRunner, error) {
	// Ensure DB record exists.
	dbRecord := &db.CompanyRecord{
		GameID:     company.ID.String(),
		Name:       company.Name,
		Ticker:     company.Ticker,
		HomePortID: homePortID.String(),
		Strategy:   strategyName,
		Status:     "running",
		Treasury:   company.Treasury,
		Reputation: company.Reputation,
	}

	result := m.gormDB.Where("game_id = ?", company.ID.String()).FirstOrCreate(dbRecord)
	if result.Error != nil {
		return nil, fmt.Errorf("failed to upsert company record: %w", result.Error)
	}

	// Update fields in case they've changed.
	m.gormDB.Model(dbRecord).Updates(map[string]any{
		"name":     company.Name,
		"strategy": strategyName,
		"status":   "running",
		"treasury": company.Treasury,
	})

	// Create company-scoped client.
	companyClient := m.baseClient.ForCompany(company.ID.String())

	// Create state.
	state := NewCompanyState(company.ID)
	state.SetDBID(dbRecord.ID)

	// Create company logger.
	companyLogger := NewCompanyLogger(
		m.logger.With(
			zap.String("company", company.Name),
			zap.String("ticker", company.Ticker),
			zap.String("strategy", strategyName),
		),
		dbRecord.ID,
		m.gormDB,
	)

	// Create strategy context and strategy instance.
	stratCtx := StrategyContext{
		Client:     companyClient,
		State:      state,
		World:      m.worldData,
		PriceCache: m.priceCache,
		Agent:      m.agent,
		Logger:     companyLogger,
		DB:         m.gormDB,
	}

	strategy, err := factory(stratCtx)
	if err != nil {
		return nil, fmt.Errorf("failed to create strategy %s: %w", strategyName, err)
	}

	return NewCompanyRunner(
		companyClient,
		m.gormDB,
		m.worldData,
		m.priceCache,
		state,
		strategy,
		companyLogger,
		dbRecord,
	), nil
}

// ffxivNames is the pool of FFXIV character names used for company naming.
// Each company gets a unique character — no duplicates across strategies.
var ffxivNames = []struct {
	Name   string // Company name: "<Character>'s <Venture>"
	Ticker string // 3-5 char ticker derived from the character
}{
	{"Alphinaud's Ventures", "ALPHI"},
	{"Alisaie's Expeditions", "ALISA"},
	{"Y'shtola's Consortium", "YSHTO"},
	{"Thancred's Trading Co", "THANC"},
	{"Urianger's Emporium", "URIAN"},
	{"G'raha's Enterprises", "GRAHA"},
	{"Estinien's Imports", "ESTIN"},
	{"Tataru's Goldworks", "TATAR"},
	{"Krile's Shipments", "KRILE"},
	{"Minfilia's Commerce", "MINFI"},
	{"Haurchefant's Guild", "HAUCH"},
	{"Aymeric's Holdings", "AYMRC"},
	{"Hien's Trade Routes", "HIEN"},
	{"Yugiri's Supply Co", "YUGIR"},
	{"Cid's Ironworks", "CID"},
	{"Emet-Selch's Legacy", "EMETS"},
	{"Lyse's Exports", "LYSE"},
	{"Nero's Machinations", "NERO"},
	{"Ryne's Caravans", "RYNE"},
	{"Lyna's Guard Trade", "LYNA"},
}

// nameIndex tracks how many names have been assigned so far.
// Reset each time the manager starts.
var nameIndex int

// buildTicker returns the FFXIV-themed ticker for the next company.
func (m *Manager) buildTicker(_ string, _ int) string {
	idx := nameIndex % len(ffxivNames)
	return ffxivNames[idx].Ticker
}

// buildCompanyName returns the FFXIV-themed name for the next company
// and advances the name index so the next call gets a different character.
func (m *Manager) buildCompanyName(_ string, _ int) string {
	idx := nameIndex % len(ffxivNames)
	nameIndex++
	return ffxivNames[idx].Name
}

// GetRunner returns a company runner by game ID.
func (m *Manager) GetRunner(companyID string) *CompanyRunner {
	m.mu.RLock()
	defer m.mu.RUnlock()
	return m.companies[companyID]
}

// RateLimiter returns the shared rate limiter for external access (e.g., API server).
func (m *Manager) RateLimiter() *api.RateLimiter {
	return m.rateLimiter
}

// BaseClient returns the base API client (for scanner use).
func (m *Manager) BaseClient() *api.Client {
	return m.baseClient
}

// WorldData returns the shared world cache.
func (m *Manager) WorldData() *WorldCache {
	return m.worldData
}

// PriceCache returns the shared price cache.
func (m *Manager) PriceCache() *PriceCache {
	return m.priceCache
}

// DB returns the GORM database connection.
func (m *Manager) DB() *gorm.DB {
	return m.gormDB
}
