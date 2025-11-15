package ratelimiter

import (
	"context"
	"time"

	"golang.org/x/time/rate"
)

// RateLimiter provides request rate limiting using the token bucket algorithm.
//
// This implementation wraps golang.org/x/time/rate to provide:
//   - Token bucket rate limiting (allows bursts while enforcing sustained rate)
//   - Context-aware waiting (respects cancellation)
//   - Zero-allocation fast path for allowed requests
//   - Thread-safe operation
//
// The token bucket algorithm works as follows:
//  1. Tokens are added to the bucket at a constant rate (requests per second)
//  2. Each request consumes one token from the bucket
//  3. If the bucket is empty, the request is either rejected or waits for a token
//  4. Burst capacity allows temporary spikes above the sustained rate
//
// Use cases:
//   - Prevent resource exhaustion from excessive client requests
//   - Enforce SLA limits per client or globally
//   - Protect backend services from overload
//
// Thread safety:
// All methods are safe for concurrent use.
type RateLimiter struct {
	limiter *rate.Limiter
}

// New creates a new RateLimiter with the specified rate and burst capacity.
//
// Parameters:
//   - requestsPerSecond: Maximum sustained rate (tokens added per second)
//   - burst: Maximum burst size (bucket capacity in tokens)
//
// The burst parameter controls how many requests can be served immediately
// when the bucket is full. It should typically be >= requestsPerSecond.
//
// Special cases:
//   - requestsPerSecond = 0: No rate limiting (unlimited)
//   - burst = 0: No burst allowed (only sustained rate)
//
// Example:
//
//	// Allow 1000 req/s sustained, 2000 req/s burst
//	limiter := New(1000, 2000)
//
// Returns a configured RateLimiter.
func New(requestsPerSecond, burst uint) *RateLimiter {
	if requestsPerSecond == 0 {
		// Unlimited rate: use a very high limit
		// rate.Inf would be ideal but has edge cases, so use a large value
		requestsPerSecond = 1_000_000_000 // 1 billion req/s (effectively unlimited)
		burst = requestsPerSecond
	}

	return &RateLimiter{
		limiter: rate.NewLimiter(rate.Limit(requestsPerSecond), int(burst)),
	}
}

// Allow checks if a request is allowed under the current rate limit.
//
// This is the fast path for rate limiting - it returns immediately without waiting.
//
// Returns:
//   - true if the request is allowed (token consumed)
//   - false if the request should be rejected (no tokens available)
//
// Use this method when you want to reject requests that exceed the rate limit
// rather than making them wait.
//
// Example:
//
//	if !limiter.Allow() {
//	    return errors.New("rate limit exceeded")
//	}
//
// Thread safety:
// Safe to call concurrently.
func (r *RateLimiter) Allow() bool {
	return r.limiter.Allow()
}

// Wait blocks until a token is available or the context is cancelled.
//
// This is the slow path for rate limiting - it waits for a token to become
// available instead of rejecting the request immediately.
//
// Parameters:
//   - ctx: Controls the maximum wait time. If cancelled, returns context error.
//
// Returns:
//   - nil if a token was acquired
//   - context error if the context was cancelled before a token was available
//
// Use this method when you want to throttle requests rather than reject them.
// This provides a better user experience but can lead to increased latency.
//
// Example:
//
//	if err := limiter.Wait(ctx); err != nil {
//	    return fmt.Errorf("rate limit wait cancelled: %w", err)
//	}
//
// Thread safety:
// Safe to call concurrently.
func (r *RateLimiter) Wait(ctx context.Context) error {
	return r.limiter.Wait(ctx)
}

// AllowN checks if N requests are allowed under the current rate limit.
//
// This is useful for batch operations where you want to consume multiple
// tokens at once.
//
// Parameters:
//   - n: Number of tokens to consume
//
// Returns:
//   - true if N tokens were available and consumed
//   - false if fewer than N tokens were available (no tokens consumed)
//
// Thread safety:
// Safe to call concurrently.
func (r *RateLimiter) AllowN(n uint) bool {
	return r.limiter.AllowN(time.Now(), int(n))
}

// SetLimit updates the rate limit to a new value.
//
// This allows dynamic rate limit adjustments without creating a new limiter.
// The burst size is automatically adjusted to match the new rate if it was
// previously at or below the old rate, or if it was at the default ratio (2x old rate).
//
// Parameters:
//   - requestsPerSecond: New maximum sustained rate
//
// Thread safety:
// Safe to call concurrently.
func (r *RateLimiter) SetLimit(requestsPerSecond uint) {
	if requestsPerSecond == 0 {
		requestsPerSecond = 1_000_000_000 // Effectively unlimited
	}

	// Update burst size based on relationship to old rate
	oldRate := uint(r.limiter.Limit())
	oldBurst := uint(r.limiter.Burst())
	r.limiter.SetLimit(rate.Limit(requestsPerSecond))

	// Update burst if:
	// 1. It was at the default ratio (2x old rate), OR
	// 2. It was equal to or below the old rate (indicating a custom small burst)
	// This ensures the bucket can hold tokens accumulated at the new rate.
	if oldBurst == oldRate*2 || oldBurst <= oldRate {
		r.limiter.SetBurst(int(requestsPerSecond * 2))
	}
}

// SetBurst updates the burst size to a new value.
//
// Parameters:
//   - burst: New maximum burst size
//
// Thread safety:
// Safe to call concurrently.
func (r *RateLimiter) SetBurst(burst uint) {
	r.limiter.SetBurst(int(burst))
}

// Tokens returns the current number of available tokens.
//
// This is primarily useful for monitoring and debugging.
// Note that the value may change immediately after this call due to
// concurrent access or token replenishment.
//
// Returns:
//   - Current number of tokens in the bucket (may be fractional)
//
// Thread safety:
// Safe to call concurrently.
func (r *RateLimiter) Tokens() float64 {
	return r.limiter.Tokens()
}
