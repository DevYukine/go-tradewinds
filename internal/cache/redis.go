package cache

import (
	"context"
	"encoding/json"
	"fmt"
	"strconv"
	"time"

	"github.com/redis/go-redis/v9"
	"go.uber.org/fx"
	"go.uber.org/zap"

	"github.com/DevYukine/go-tradewinds/internal/config"
)

const (
	keyPrefix     = "tradewinds:"
	rateLimitKey  = keyPrefix + "ratelimit:timestamps"
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

// SaveRateLimitTimestamps persists the sliding window timestamps to Redis
// as a sorted set (score = unix millisecond, member = index).
func (rc *RedisCache) SaveRateLimitTimestamps(ctx context.Context, timestamps []time.Time) {
	pipe := rc.client.Pipeline()
	pipe.Del(ctx, rateLimitKey)

	if len(timestamps) > 0 {
		members := make([]redis.Z, 0, len(timestamps))
		for i, ts := range timestamps {
			if ts.IsZero() {
				continue
			}
			members = append(members, redis.Z{
				Score:  float64(ts.UnixMilli()),
				Member: strconv.Itoa(i),
			})
		}
		if len(members) > 0 {
			pipe.ZAdd(ctx, rateLimitKey, members...)
			pipe.Expire(ctx, rateLimitKey, rateLimitTTL)
		}
	}

	if _, err := pipe.Exec(ctx); err != nil {
		rc.logger.Warn("failed to save rate limit timestamps to Redis", zap.Error(err))
	}
}

// LoadRateLimitTimestamps restores sliding window timestamps from Redis.
// Only returns timestamps within the last 60 seconds.
func (rc *RedisCache) LoadRateLimitTimestamps(ctx context.Context) []time.Time {
	cutoff := float64(time.Now().Add(-60 * time.Second).UnixMilli())

	zResults, err := rc.client.ZRangeByScoreWithScores(ctx, rateLimitKey, &redis.ZRangeBy{
		Min: fmt.Sprintf("%.0f", cutoff),
		Max: "+inf",
	}).Result()
	if err != nil {
		rc.logger.Warn("failed to load rate limit timestamps from Redis", zap.Error(err))
		return nil
	}

	if len(zResults) == 0 {
		return nil
	}

	timestamps := make([]time.Time, 0, len(zResults))
	for _, z := range zResults {
		ms := int64(z.Score)
		timestamps = append(timestamps, time.UnixMilli(ms))
	}

	rc.logger.Info("restored rate limit state from Redis",
		zap.Int("active_timestamps", len(timestamps)),
	)

	return timestamps
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

	rc.logger.Info("restored price cache from Redis",
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

	rc.logger.Info("restored scanner position from Redis", zap.Int("port_idx", val))
	return val
}
