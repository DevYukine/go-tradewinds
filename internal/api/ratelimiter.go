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
	defaultMaxPerMinute = 900

	// Throttle thresholds as percentages of maxPerMinute.
	thresholdLowBlock    = 0.70 // 70%: block PriorityLow
	thresholdNormalBlock = 0.85 // 85%: block PriorityNormal

	// Minimum spacing between requests to avoid micro-bursts.
	minRequestSpacing = 60 * time.Millisecond // ~1000 req/min max throughput (safe margin under 900 limit)

	// windowDuration is the fixed window size.
	windowDuration = 60 * time.Second
)

// RateLimiter enforces the game's rate limit of 900 requests per 60 seconds per IP.
// It uses a fixed window counter that resets every 60 seconds, matching the
// game server's rate limit behavior.
type RateLimiter struct {
	maxPerMinute int

	// Fixed window: counter resets at windowStart + windowDuration.
	windowStart time.Time
	count       int

	// lastRequest tracks when the last request was made for spacing.
	lastRequest time.Time

	// backoffUntil is set when a 429 is received; all requests block until this time.
	backoffUntil time.Time

	mu     sync.Mutex
	logger *zap.Logger
}

// NewRateLimiter creates a rate limiter enforcing the given requests-per-minute limit.
// Uses a fixed window counter that resets every 60 seconds.
// Starts with a conservative backoff to handle restarts when the server's
// rate limit window from a previous run may still be active.
func NewRateLimiter(maxPerMinute int, logger *zap.Logger) *RateLimiter {
	if maxPerMinute <= 0 {
		maxPerMinute = defaultMaxPerMinute
	}

	return &RateLimiter{
		maxPerMinute: maxPerMinute,
		windowStart:  time.Now(),
		logger:       logger.Named("rate_limiter"),
		// Start with a short backoff so the first few requests are spaced out,
		// giving the server's previous window time to drain.
		backoffUntil: time.Now().Add(2 * time.Second),
	}
}

// RestoreWindow restores a previously persisted window state (e.g., from Redis).
// This allows the rate limiter to continue where it left off after a restart.
func (rl *RateLimiter) RestoreWindow(windowStart time.Time, count int) {
	rl.mu.Lock()
	defer rl.mu.Unlock()

	// Only restore if the window is still active.
	if time.Since(windowStart) >= windowDuration {
		rl.logger.Debug("skipping restore — window already expired",
			zap.Time("window_start", windowStart),
		)
		return
	}

	rl.windowStart = windowStart
	rl.count = count
	// Clear the startup backoff since we have real data now.
	rl.backoffUntil = time.Time{}

	rl.logger.Info("rate limiter state restored",
		zap.Int("count", count),
		zap.Time("window_start", windowStart),
		zap.Duration("remaining", windowDuration-time.Since(windowStart)),
	)
}

// SnapshotWindow returns the current window start and count for persistence.
func (rl *RateLimiter) SnapshotWindow() (windowStart time.Time, count int) {
	rl.mu.Lock()
	defer rl.mu.Unlock()

	rl.maybeResetWindow(time.Now())
	return rl.windowStart, rl.count
}

// Acquire blocks until a request slot is available for the given priority,
// or the context is cancelled.
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

	// Reset window if expired.
	rl.maybeResetWindow(now)

	// Check priority-based throttling thresholds.
	usageRatio := float64(rl.count) / float64(rl.maxPerMinute)

	switch {
	case rl.count >= rl.maxPerMinute:
		// At capacity — wait until the window resets.
		return time.Until(rl.windowStart.Add(windowDuration)) + 10*time.Millisecond
	case usageRatio >= thresholdNormalBlock && priority > PriorityHigh:
		return 100 * time.Millisecond
	case usageRatio >= thresholdLowBlock && priority > PriorityNormal:
		return 200 * time.Millisecond
	}

	// Enforce minimum spacing between requests to avoid micro-bursts.
	if elapsed := now.Sub(rl.lastRequest); elapsed < minRequestSpacing {
		return minRequestSpacing - elapsed
	}

	// Acquired — increment counter.
	rl.count++
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

// CurrentBudget returns the number of requests used in the current window
// and the maximum allowed.
func (rl *RateLimiter) CurrentBudget() (used, max int) {
	rl.mu.Lock()
	defer rl.mu.Unlock()

	rl.maybeResetWindow(time.Now())
	return rl.count, rl.maxPerMinute
}

// ResetsAt returns when the current rate limit window resets.
func (rl *RateLimiter) ResetsAt() time.Time {
	rl.mu.Lock()
	defer rl.mu.Unlock()

	rl.maybeResetWindow(time.Now())
	return rl.windowStart.Add(windowDuration)
}

// Utilization returns the current rate limit utilization as a float between 0.0 and 1.0.
func (rl *RateLimiter) Utilization() float64 {
	used, max := rl.CurrentBudget()
	if max == 0 {
		return 0
	}
	return float64(used) / float64(max)
}

// maybeResetWindow resets the counter if the current window has expired.
// Must be called while holding rl.mu.
func (rl *RateLimiter) maybeResetWindow(now time.Time) {
	if now.Sub(rl.windowStart) >= windowDuration {
		if rl.count > 0 {
			rl.logger.Debug("rate limit window reset",
				zap.Int("previous_count", rl.count),
			)
		}
		rl.windowStart = now
		rl.count = 0
	}
}
