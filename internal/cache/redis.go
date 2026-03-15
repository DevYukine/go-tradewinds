package cache

import (
	"context"
	"encoding/json"
	"fmt"
	"time"

	"github.com/redis/go-redis/v9"
	"go.uber.org/fx"
	"go.uber.org/zap"

	"github.com/DevYukine/go-tradewinds/internal/config"
)

const (
	keyPrefix     = "tradewinds:"
	rateLimitKey  = keyPrefix + "ratelimit:window"
	priceCacheKey = keyPrefix + "prices"
	scannerIdxKey = keyPrefix + "scanner:port_idx"
	rateLimitTTL  = 90 * time.Second // slightly longer than the 60s window
	priceCacheTTL = 10 * time.Minute // prices go stale after 10 minutes
)

// Module provides the Redis cache to the fx DI container.
var Module = fx.Module("cache",
	fx.Provide(NewRedisCache),
)

// RedisCache wraps a Redis client for persisting bot state across restarts.
type RedisCache struct {
	client *redis.Client
	logger *zap.Logger
}

// NewRedisCache creates a Redis cache from config. Returns an error if
// REDIS_URL is not set or the connection fails.
func NewRedisCache(lc fx.Lifecycle, cfg *config.Config, logger *zap.Logger) (*RedisCache, error) {
	log := logger.Named("redis")

	if cfg.RedisURL == "" {
		return nil, fmt.Errorf("REDIS_URL is required")
	}

	opts, err := redis.ParseURL(cfg.RedisURL)
	if err != nil {
		return nil, fmt.Errorf("invalid REDIS_URL: %w", err)
	}

	client := redis.NewClient(opts)

	// Verify connection.
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	if err := client.Ping(ctx).Err(); err != nil {
		return nil, fmt.Errorf("Redis connection failed: %w", err)
	}

	log.Info("Redis connected", zap.String("addr", opts.Addr))

	lc.Append(fx.Hook{
		OnStop: func(ctx context.Context) error {
			log.Info("closing Redis connection")
			return client.Close()
		},
	})

	return &RedisCache{client: client, logger: log}, nil
}

// Client returns the underlying Redis client for direct access if needed.
func (rc *RedisCache) Client() *redis.Client {
	return rc.client
}

// --- Generic API Response Cache ---

// CacheGet retrieves a cached API response by key. Returns nil if not found
// or expired.
func (rc *RedisCache) CacheGet(ctx context.Context, key string) []byte {
	data, err := rc.client.Get(ctx, keyPrefix+"api:"+key).Bytes()
	if err != nil {
		return nil
	}
	return data
}

// CacheSet stores an API response with the given TTL.
func (rc *RedisCache) CacheSet(ctx context.Context, key string, data []byte, ttl time.Duration) {
	if err := rc.client.Set(ctx, keyPrefix+"api:"+key, data, ttl).Err(); err != nil {
		rc.logger.Warn("failed to cache API response", zap.String("key", key), zap.Error(err))
	}
}

// CacheDel removes a cached API response.
func (rc *RedisCache) CacheDel(ctx context.Context, key string) {
	rc.client.Del(ctx, keyPrefix+"api:"+key)
}

// --- Rate Limiter Persistence ---

// rateLimitWindow is the Redis-persisted state of the fixed window rate limiter.
type rateLimitWindow struct {
	WindowStartMs int64 `json:"window_start_ms"`
	Count         int   `json:"count"`
}

// SaveRateLimitWindow persists the current fixed window state to Redis.
func (rc *RedisCache) SaveRateLimitWindow(ctx context.Context, windowStart time.Time, count int) {
	data, err := json.Marshal(rateLimitWindow{
		WindowStartMs: windowStart.UnixMilli(),
		Count:         count,
	})
	if err != nil {
		rc.logger.Warn("failed to marshal rate limit window", zap.Error(err))
		return
	}

	if err := rc.client.Set(ctx, rateLimitKey, data, rateLimitTTL).Err(); err != nil {
		rc.logger.Warn("failed to save rate limit window to Redis", zap.Error(err))
	}
}

// LoadRateLimitWindow restores the fixed window state from Redis.
// Returns zero values if no state is found or the window has expired.
func (rc *RedisCache) LoadRateLimitWindow(ctx context.Context) (windowStart time.Time, count int) {
	data, err := rc.client.Get(ctx, rateLimitKey).Bytes()
	if err != nil {
		// Key doesn't exist or Redis error — start fresh.
		return time.Time{}, 0
	}

	var w rateLimitWindow
	if err := json.Unmarshal(data, &w); err != nil {
		rc.logger.Warn("failed to unmarshal rate limit window from Redis", zap.Error(err))
		return time.Time{}, 0
	}

	ws := time.UnixMilli(w.WindowStartMs)

	// Only restore if the window is still active.
	if time.Since(ws) >= 60*time.Second {
		return time.Time{}, 0
	}

	rc.logger.Debug("restored rate limit state from Redis",
		zap.Int("count", w.Count),
		zap.Duration("remaining", 60*time.Second-time.Since(ws)),
	)

	return ws, w.Count
}

// --- Price Cache Persistence ---

// PriceCacheEntry is the serialized form of a price point.
type PriceCacheEntry struct {
	BuyPrice   int   `json:"b"`
	SellPrice  int   `json:"s"`
	ObservedAt int64 `json:"t"` // Unix milliseconds.
}

// SavePriceEntry persists a single price observation to Redis.
func (rc *RedisCache) SavePriceEntry(ctx context.Context, key string, buyPrice, sellPrice int) {
	entry := PriceCacheEntry{
		BuyPrice:   buyPrice,
		SellPrice:  sellPrice,
		ObservedAt: time.Now().UnixMilli(),
	}
	data, _ := json.Marshal(entry)

	if err := rc.client.HSet(ctx, priceCacheKey, key, data).Err(); err != nil {
		rc.logger.Warn("failed to save price entry to Redis", zap.Error(err))
	}
}

// LoadPriceCache restores all price entries from Redis.
// Entries older than priceCacheTTL are excluded and pruned.
func (rc *RedisCache) LoadPriceCache(ctx context.Context) map[string]PriceCacheEntry {
	result, err := rc.client.HGetAll(ctx, priceCacheKey).Result()
	if err != nil {
		rc.logger.Warn("failed to load price cache from Redis", zap.Error(err))
		return nil
	}

	cutoff := time.Now().Add(-priceCacheTTL).UnixMilli()
	entries := make(map[string]PriceCacheEntry, len(result))
	var staleKeys []string

	for key, data := range result {
		var entry PriceCacheEntry
		if err := json.Unmarshal([]byte(data), &entry); err != nil {
			staleKeys = append(staleKeys, key)
			continue
		}
		if entry.ObservedAt < cutoff {
			staleKeys = append(staleKeys, key)
			continue
		}
		entries[key] = entry
	}

	// Prune stale entries.
	if len(staleKeys) > 0 {
		rc.client.HDel(ctx, priceCacheKey, staleKeys...)
	}

	rc.logger.Debug("restored price cache from Redis",
		zap.Int("entries", len(entries)),
		zap.Int("stale_pruned", len(staleKeys)),
	)

	return entries
}

// --- Scanner Position Persistence ---

// SaveScannerIndex persists the scanner's current port index.
func (rc *RedisCache) SaveScannerIndex(ctx context.Context, idx int) {
	if err := rc.client.Set(ctx, scannerIdxKey, idx, priceCacheTTL).Err(); err != nil {
		rc.logger.Warn("failed to save scanner index to Redis", zap.Error(err))
	}
}

// LoadScannerIndex restores the scanner's port index. Returns 0 if not found.
func (rc *RedisCache) LoadScannerIndex(ctx context.Context) int {
	val, err := rc.client.Get(ctx, scannerIdxKey).Int()
	if err != nil {
		return 0
	}

	rc.logger.Debug("restored scanner position from Redis", zap.Int("port_idx", val))
	return val
}

// --- Cargo Cost Persistence ---

const cargoCostKeyPrefix = keyPrefix + "cargo_costs:"

// cargoCostKey builds the Redis key for a ship's cargo costs.
func cargoCostKey(companyID, shipID string) string {
	return cargoCostKeyPrefix + companyID + ":" + shipID
}

// SaveCargoCosts persists a ship's cargo cost map to Redis.
func (rc *RedisCache) SaveCargoCosts(ctx context.Context, companyID, shipID string, costs map[string]int) {
	if len(costs) == 0 {
		// Clean up empty maps.
		rc.client.Del(ctx, cargoCostKey(companyID, shipID))
		return
	}
	data, err := json.Marshal(costs)
	if err != nil {
		rc.logger.Warn("failed to marshal cargo costs", zap.Error(err))
		return
	}
	// TTL of 1 hour — costs become less relevant over time as cargo changes.
	if err := rc.client.Set(ctx, cargoCostKey(companyID, shipID), data, time.Hour).Err(); err != nil {
		rc.logger.Warn("failed to save cargo costs to Redis", zap.Error(err))
	}
}

// LoadCargoCosts restores a ship's cargo cost map from Redis.
// Returns nil if not found or expired.
func (rc *RedisCache) LoadCargoCosts(ctx context.Context, companyID, shipID string) map[string]int {
	data, err := rc.client.Get(ctx, cargoCostKey(companyID, shipID)).Bytes()
	if err != nil {
		return nil
	}
	var costs map[string]int
	if err := json.Unmarshal(data, &costs); err != nil {
		rc.logger.Warn("failed to unmarshal cargo costs from Redis", zap.Error(err))
		return nil
	}
	return costs
}
