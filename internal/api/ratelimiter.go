package api

import (
	"context"
	"fmt"
	"sync"
	"sync/atomic"
	"time"

	"go.uber.org/ratelimit"
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
	thresholdLowBlock    = 0.90 // 90%: block PriorityLow
	thresholdNormalBlock = 0.98 // 98%: block PriorityNormal
)

// RateLimiter enforces the game's rate limit of 300 requests per 60 seconds per IP.
// It wraps uber/ratelimit for even request spacing (token bucket) and adds a
// sliding window counter for budget tracking and priority-based throttling.
type RateLimiter struct {
	maxPerMinute int
	limiter      ratelimit.Limiter // uber/ratelimit: smooths requests to ~5/sec

	// Sliding window counter for the 60-second budget.
	usedThisWindow atomic.Int64
	windowStart    time.Time

	// backoffUntil is set when a 429 is received; all requests block until this time.
	backoffUntil time.Time

	mu     sync.Mutex
	logger *zap.Logger
}

// NewRateLimiter creates a rate limiter enforcing the given requests-per-minute limit.
// It uses uber/ratelimit internally for even request spacing and adds priority-based
// throttling on top via a sliding window counter.
func NewRateLimiter(maxPerMinute int, logger *zap.Logger) *RateLimiter {
	if maxPerMinute <= 0 {
		maxPerMinute = defaultMaxPerMinute
	}

	ratePerSecond := maxPerMinute / 60
	if ratePerSecond < 1 {
		ratePerSecond = 1
	}

	return &RateLimiter{
		maxPerMinute: maxPerMinute,
		limiter:      ratelimit.New(ratePerSecond, ratelimit.WithSlack(10)),
		windowStart:  time.Now(),
		logger:       logger.Named("rate_limiter"),
	}
}

// Acquire blocks until a request slot is available for the given priority,
// or the context is cancelled. It first checks priority-based budget thresholds,
// then delegates to uber/ratelimit for even spacing.
func (rl *RateLimiter) Acquire(ctx context.Context, priority Priority) error {
	// Spin until priority thresholds allow this request through.
	for {
		if err := ctx.Err(); err != nil {
			return fmt.Errorf("rate limiter: context cancelled: %w", err)
		}

		if rl.canProceed(priority) {
			break
		}

		// Wait before rechecking. Higher priority gets shorter waits.
		var wait time.Duration
		switch priority {
		case PriorityHigh:
			wait = 20 * time.Millisecond
		case PriorityNormal:
			wait = 50 * time.Millisecond
		case PriorityLow:
			wait = 100 * time.Millisecond
		}

		select {
		case <-ctx.Done():
			return fmt.Errorf("rate limiter: context cancelled: %w", ctx.Err())
		case <-time.After(wait):
		}
	}

	// Delegate to uber/ratelimit for even spacing (~5 req/sec for 300/min).
	// This call blocks until the next available slot.
	rl.limiter.Take()

	// Increment the sliding window counter.
	rl.usedThisWindow.Add(1)

	return nil
}

// TryAcquire attempts to acquire a request slot without blocking.
// Returns true if the slot was acquired, false if the request should wait.
func (rl *RateLimiter) TryAcquire(priority Priority) bool {
	if !rl.canProceed(priority) {
		return false
	}

	rl.limiter.Take()
	rl.usedThisWindow.Add(1)
	return true
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

// CurrentBudget returns the number of requests used in the current 60-second
// window and the maximum allowed.
func (rl *RateLimiter) CurrentBudget() (used, max int) {
	rl.mu.Lock()
	rl.maybeResetWindow()
	rl.mu.Unlock()

	return int(rl.usedThisWindow.Load()), rl.maxPerMinute
}

// Utilization returns the current rate limit utilization as a float between 0.0 and 1.0.
func (rl *RateLimiter) Utilization() float64 {
	used, max := rl.CurrentBudget()
	if max == 0 {
		return 0
	}
	return float64(used) / float64(max)
}

// canProceed checks whether a request at the given priority level is allowed
// through based on backoff state and sliding window budget thresholds.
func (rl *RateLimiter) canProceed(priority Priority) bool {
	rl.mu.Lock()
	defer rl.mu.Unlock()

	now := time.Now()

	// Check emergency backoff from a 429 response.
	if now.Before(rl.backoffUntil) {
		return false
	}

	// Reset the sliding window if 60 seconds have elapsed.
	rl.maybeResetWindow()

	// Check priority-based throttling thresholds.
	used := rl.usedThisWindow.Load()
	usageRatio := float64(used) / float64(rl.maxPerMinute)

	switch {
	case used >= int64(rl.maxPerMinute):
		return false
	case usageRatio >= thresholdNormalBlock && priority > PriorityHigh:
		return false
	case usageRatio >= thresholdLowBlock && priority > PriorityNormal:
		return false
	}

	return true
}

// maybeResetWindow resets the sliding window counter if 60 seconds have elapsed.
// Must be called while holding rl.mu.
func (rl *RateLimiter) maybeResetWindow() {
	now := time.Now()
	if now.Sub(rl.windowStart) >= 60*time.Second {
		previousUsed := rl.usedThisWindow.Load()
		rl.usedThisWindow.Store(0)
		rl.windowStart = now

		if previousUsed > 0 {
			rl.logger.Debug("rate limit window reset",
				zap.Int64("previous_window_usage", previousUsed),
				zap.Int("max_per_minute", rl.maxPerMinute),
			)
		}
	}
}
