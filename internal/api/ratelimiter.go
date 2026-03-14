package api

import (
	"context"
	"fmt"
	"sync"
	"time"

	"go.uber.org/zap"
)

// Priority determines the order in which requests are served when the rate limit
// budget is under pressure. Higher priority requests are allowed through even
// when lower priority ones are blocked.
type Priority int

const (
	// PriorityHigh is for trade executions with expiring quotes.
	PriorityHigh Priority = iota
	// PriorityNormal is for ship transit, inventory checks, and general operations.
	PriorityNormal
	// PriorityLow is for price scanning, economy refreshes, and background tasks.
	PriorityLow
)

const (
	defaultMaxPerMinute = 300

	// Throttle thresholds as percentages of maxPerMinute.
	thresholdLowBlock    = 0.85 // 85%: block PriorityLow
	thresholdNormalBlock = 0.95 // 95%: block PriorityNormal

	// Minimum spacing between requests to avoid micro-bursts.
	minRequestSpacing = 220 * time.Millisecond // ~270 req/min max throughput (safety margin)

	// windowDuration is the sliding window size.
	windowDuration = 60 * time.Second
)

// RateLimiter enforces the game's rate limit of 300 requests per 60 seconds per IP.
// It uses a true sliding window (ring buffer of timestamps) to avoid the burst
// problem that tumbling windows have at reset boundaries.
type RateLimiter struct {
	maxPerMinute int

	// Sliding window: circular buffer of request timestamps.
	timestamps []time.Time
	head       int // next write position
	count      int // number of valid entries

	// lastRequest tracks when the last request was made for spacing.
	lastRequest time.Time

	// backoffUntil is set when a 429 is received; all requests block until this time.
	backoffUntil time.Time

	mu     sync.Mutex
	logger *zap.Logger
}

// NewRateLimiter creates a rate limiter enforcing the given requests-per-minute limit.
// Uses a true sliding window for accurate budget tracking.
// Starts with a conservative backoff to handle restarts when the server's
// rate limit window from a previous run may still be active.
func NewRateLimiter(maxPerMinute int, logger *zap.Logger) *RateLimiter {
	if maxPerMinute <= 0 {
		maxPerMinute = defaultMaxPerMinute
	}

	return &RateLimiter{
		maxPerMinute: maxPerMinute,
		timestamps:   make([]time.Time, maxPerMinute),
		logger:       logger.Named("rate_limiter"),
		// Start with a short backoff so the first few requests are spaced out,
		// giving the server's previous window time to drain.
		backoffUntil: time.Now().Add(2 * time.Second),
	}
}

// Acquire blocks until a request slot is available for the given priority,
// or the context is cancelled. Uses a true sliding window to prevent
// burst-at-boundary problems.
func (rl *RateLimiter) Acquire(ctx context.Context, priority Priority) error {
	for {
		if err := ctx.Err(); err != nil {
			return fmt.Errorf("rate limiter: context cancelled: %w", err)
		}

		wait := rl.tryAcquire(priority)
		if wait == 0 {
			return nil // Acquired successfully.
		}

		select {
		case <-ctx.Done():
			return fmt.Errorf("rate limiter: context cancelled: %w", ctx.Err())
		case <-time.After(wait):
		}
	}
}

// tryAcquire attempts to acquire a slot. Returns 0 if acquired, or the
// duration to wait before retrying.
func (rl *RateLimiter) tryAcquire(priority Priority) time.Duration {
	rl.mu.Lock()
	defer rl.mu.Unlock()

	now := time.Now()

	// Check emergency backoff from a 429 response.
	if now.Before(rl.backoffUntil) {
		return time.Until(rl.backoffUntil)
	}

	// Evict expired timestamps.
	rl.evictExpired(now)

	// Check priority-based throttling thresholds.
	usageRatio := float64(rl.count) / float64(rl.maxPerMinute)

	switch {
	case rl.count >= rl.maxPerMinute:
		// At capacity — wait until the oldest timestamp expires.
		oldest := rl.oldestTimestamp()
		return time.Until(oldest.Add(windowDuration)) + 10*time.Millisecond
	case usageRatio >= thresholdNormalBlock && priority > PriorityHigh:
		return 100 * time.Millisecond
	case usageRatio >= thresholdLowBlock && priority > PriorityNormal:
		return 200 * time.Millisecond
	}

	// Enforce minimum spacing between requests to avoid micro-bursts.
	if elapsed := now.Sub(rl.lastRequest); elapsed < minRequestSpacing {
		return minRequestSpacing - elapsed
	}

	// Acquired — record this request.
	rl.timestamps[rl.head] = now
	rl.head = (rl.head + 1) % len(rl.timestamps)
	if rl.count < len(rl.timestamps) {
		rl.count++
	}
	rl.lastRequest = now

	return 0
}

// RecordBackoff sets a backoff period after receiving a 429 response.
// All requests will block until the backoff period expires.
func (rl *RateLimiter) RecordBackoff(retryAfter time.Duration) {
	rl.mu.Lock()
	defer rl.mu.Unlock()

	rl.backoffUntil = time.Now().Add(retryAfter)
	rl.logger.Warn("rate limit 429 received, backing off",
		zap.Duration("retry_after", retryAfter),
		zap.Time("backoff_until", rl.backoffUntil),
	)
}

// CurrentBudget returns the number of requests used in the current sliding
// window and the maximum allowed.
func (rl *RateLimiter) CurrentBudget() (used, max int) {
	rl.mu.Lock()
	defer rl.mu.Unlock()

	rl.evictExpired(time.Now())
	return rl.count, rl.maxPerMinute
}

// Utilization returns the current rate limit utilization as a float between 0.0 and 1.0.
func (rl *RateLimiter) Utilization() float64 {
	used, max := rl.CurrentBudget()
	if max == 0 {
		return 0
	}
	return float64(used) / float64(max)
}

// evictExpired removes timestamps older than windowDuration from the count.
// Must be called while holding rl.mu.
func (rl *RateLimiter) evictExpired(now time.Time) {
	cutoff := now.Add(-windowDuration)
	evicted := 0

	// Walk from the tail (oldest) forward and count expired entries.
	for rl.count > 0 {
		tail := (rl.head - rl.count + len(rl.timestamps)) % len(rl.timestamps)
		if rl.timestamps[tail].After(cutoff) {
			break
		}
		rl.count--
		evicted++
	}

	if evicted > 20 {
		rl.logger.Debug("rate limit window eviction",
			zap.Int("evicted", evicted),
			zap.Int("remaining", rl.count),
		)
	}
}

// oldestTimestamp returns the oldest non-expired timestamp in the ring buffer.
// Must be called while holding rl.mu and when count > 0.
func (rl *RateLimiter) oldestTimestamp() time.Time {
	tail := (rl.head - rl.count + len(rl.timestamps)) % len(rl.timestamps)
	return rl.timestamps[tail]
}
