package gameapi

import (
	"context"
	"crypto/sha256"
	"math"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"
)

type bucketID int

const (
	readBucket bucketID = iota
	writeBucket
	batchBucket
	streamBucket
	authBucket
	bucketCount
)

type bucketLimit struct {
	name     string
	capacity float64
	refill   float64
}

// The guide publishes per-key limits; IP limits are five times these except
// auth_strict, which is one small IP-only bucket shared by all accounts.
var bucketLimits = [bucketCount]bucketLimit{
	readBucket:   {name: "read", capacity: 2000, refill: 100},
	writeBucket:  {name: "write", capacity: 1000, refill: 50},
	batchBucket:  {name: "batch", capacity: 300, refill: 10},
	streamBucket: {name: "stream", capacity: 200, refill: 10},
	authBucket:   {name: "auth_strict", capacity: 20, refill: 0.2},
}

var sharedRates = struct {
	sync.Mutex
	hosts map[string]*hostRates
}{hosts: make(map[string]*hostRates)}

type tokenBucket struct {
	tokens     float64
	updated    time.Time
	capacity   float64
	refill     float64
	pauseUntil time.Time
}

type hostRates struct {
	mu   sync.Mutex
	ip   [bucketCount]tokenBucket
	keys map[[sha256.Size]byte]*[bucketCount]tokenBucket
}

func ratesFor(origin string) *hostRates {
	sharedRates.Lock()
	defer sharedRates.Unlock()

	if rates := sharedRates.hosts[origin]; rates != nil {
		return rates
	}

	rates := &hostRates{
		ip:   newBuckets(5),
		keys: make(map[[sha256.Size]byte]*[bucketCount]tokenBucket),
	}
	sharedRates.hosts[origin] = rates
	return rates
}

func newBuckets(multiplier float64) [bucketCount]tokenBucket {
	var buckets [bucketCount]tokenBucket
	now := time.Now()
	for id, limit := range bucketLimits {
		factor := multiplier
		if bucketID(id) == authBucket {
			factor = 1
		}
		buckets[id] = tokenBucket{
			tokens:   limit.capacity * factor,
			updated:  now,
			capacity: limit.capacity * factor,
			refill:   limit.refill * factor,
		}
	}
	return buckets
}

func requestBucket(method, path string) bucketID {
	path, _, _ = strings.Cut(path, "?")
	if method == http.MethodPost {
		switch path {
		case "/v1/player/register", "/v1/auth/login", "/v1/auth/create-key", "/v1/auth/change-passphrase":
			return authBucket
		}
	}
	if strings.HasPrefix(path, "/v1/event/") {
		return streamBucket
	}
	if strings.HasSuffix(path, "-batch") {
		return batchBucket
	}
	if method == http.MethodGet || method == http.MethodHead {
		return readBucket
	}
	return writeBucket
}

func (b *tokenBucket) refillAt(now time.Time) {
	elapsed := now.Sub(b.updated).Seconds()
	if elapsed > 0 {
		b.tokens = math.Min(b.capacity, b.tokens+elapsed*b.refill)
		b.updated = now
	}
}

func (b *tokenBucket) delay() time.Duration {
	if b.tokens >= 1 {
		return 0
	}
	return time.Duration(math.Ceil((1 - b.tokens) / b.refill * float64(time.Second)))
}

func (r *hostRates) keyBuckets(key [sha256.Size]byte) *[bucketCount]tokenBucket {
	buckets := r.keys[key]
	if buckets == nil {
		initial := newBuckets(1)
		buckets = &initial
		r.keys[key] = buckets
	}
	return buckets
}

func (r *hostRates) wait(ctx context.Context, id bucketID, key [sha256.Size]byte, authenticated bool) error {
	for {
		if err := ctx.Err(); err != nil {
			return err
		}

		r.mu.Lock()
		now := time.Now()
		ip := &r.ip[id]
		ip.refillAt(now)
		delay := max(ip.delay(), ip.pauseUntil.Sub(now))

		var keyBucket *tokenBucket
		if authenticated && id != authBucket {
			keyBucket = &r.keyBuckets(key)[id]
			keyBucket.refillAt(now)
			delay = max(delay, keyBucket.delay(), keyBucket.pauseUntil.Sub(now))
		}

		if delay <= 0 {
			ip.tokens--
			if keyBucket != nil {
				keyBucket.tokens--
			}
			r.mu.Unlock()
			return nil
		}
		r.mu.Unlock()

		if err := waitFor(ctx, delay); err != nil {
			return err
		}
	}
}

// update only lowers local availability. Out-of-order responses must not add
// tokens that a concurrently executing request has already spent.
func (r *hostRates) update(id bucketID, key [sha256.Size]byte, authenticated bool, response *http.Response) time.Time {
	now := time.Now()
	r.mu.Lock()
	defer r.mu.Unlock()

	for candidate, limit := range bucketLimits {
		if response.Header.Get("X-Ratelimit-Bucket") == limit.name {
			id = bucketID(candidate)
			break
		}
	}

	ip := &r.ip[id]
	ip.refillAt(now)
	var keyBucket *tokenBucket
	if authenticated && id != authBucket {
		keyBucket = &r.keyBuckets(key)[id]
		keyBucket.refillAt(now)
	}

	remaining, remainingErr := strconv.ParseInt(response.Header.Get("X-Ratelimit-Remaining"), 10, 64)
	if remainingErr == nil && remaining >= 0 {
		ip.tokens = math.Min(ip.tokens, float64(remaining))
		if keyBucket != nil {
			keyBucket.tokens = math.Min(keyBucket.tokens, float64(remaining))
		}
	}

	until := retryAfter(response.Header.Get("Retry-After"), now)
	// Retry-After has documented seconds/date semantics and takes precedence
	// over the guide's unit-unspecified X-Ratelimit-Reset header.
	exhausted := (remainingErr == nil && remaining == 0) || response.StatusCode == http.StatusTooManyRequests
	if until.IsZero() && exhausted {
		until = rateReset(response.Header.Get("X-Ratelimit-Reset"), now)
	}
	if response.StatusCode == http.StatusTooManyRequests && !until.After(now) {
		until = now.Add(time.Duration(math.Ceil(float64(time.Second) / bucketLimits[id].refill)))
	}
	if until.After(ip.pauseUntil) {
		ip.pauseUntil = until
	}
	if keyBucket != nil && until.After(keyBucket.pauseUntil) {
		keyBucket.pauseUntil = until
	}
	return ip.pauseUntil
}

func (r *hostRates) waitPause(ctx context.Context, id bucketID, key [sha256.Size]byte, authenticated bool) error {
	for {
		r.mu.Lock()
		until := r.ip[id].pauseUntil
		if authenticated && id != authBucket {
			keyUntil := r.keyBuckets(key)[id].pauseUntil
			if keyUntil.After(until) {
				until = keyUntil
			}
		}
		delay := time.Until(until)
		r.mu.Unlock()
		if delay <= 0 {
			return ctx.Err()
		}
		if err := waitFor(ctx, delay); err != nil {
			return err
		}
	}
}

func waitFor(ctx context.Context, duration time.Duration) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if duration <= 0 {
		return nil
	}
	timer := time.NewTimer(duration)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-timer.C:
		return nil
	}
}

func retryAfter(value string, now time.Time) time.Time {
	value = strings.TrimSpace(value)
	if seconds, err := strconv.ParseUint(value, 10, 64); err == nil {
		const maximumSeconds = uint64(math.MaxInt64 / int64(time.Second))
		if seconds > maximumSeconds {
			return now.Add(time.Duration(math.MaxInt64))
		}
		return now.Add(time.Duration(seconds) * time.Second)
	}
	if date, err := http.ParseTime(value); err == nil {
		return date
	}
	return time.Time{}
}

func rateReset(value string, now time.Time) time.Time {
	seconds, err := strconv.ParseFloat(strings.TrimSpace(value), 64)
	if err != nil || math.IsNaN(seconds) || math.IsInf(seconds, 0) || seconds < 0 {
		return time.Time{}
	}

	// The official guide does not specify reset units. Accommodate both common
	// wire formats: a plausible Unix timestamp, or a seconds-until-reset value.
	if seconds >= 1_000_000_000 {
		if seconds > float64(math.MaxInt64/int64(time.Second)) {
			return time.Time{}
		}
		return time.Unix(0, int64(seconds*float64(time.Second)))
	}
	return now.Add(time.Duration(math.Ceil(seconds * float64(time.Second))))
}
