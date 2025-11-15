package ratelimiter

import (
	"context"
	"testing"
	"time"
)

// TestNew verifies rate limiter creation with different parameters.
func TestNew(t *testing.T) {
	tests := []struct {
		name              string
		requestsPerSecond uint
		burst             uint
	}{
		{
			name:              "standard rate",
			requestsPerSecond: 100,
			burst:             200,
		},
		{
			name:              "high rate",
			requestsPerSecond: 10000,
			burst:             20000,
		},
		{
			name:              "low rate",
			requestsPerSecond: 1,
			burst:             2,
		},
		{
			name:              "unlimited (zero rate)",
			requestsPerSecond: 0,
			burst:             0,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			limiter := New(tt.requestsPerSecond, tt.burst)
			if limiter == nil {
				t.Fatal("New() returned nil")
			}
			if limiter.limiter == nil {
				t.Fatal("internal limiter is nil")
			}
		})
	}
}

// TestAllow verifies that Allow() correctly enforces rate limits.
func TestAllow(t *testing.T) {
	// Create limiter with 10 req/s, burst of 10
	limiter := New(10, 10)

	// First burst should be allowed (up to burst capacity)
	for i := 0; i < 10; i++ {
		if !limiter.Allow() {
			t.Fatalf("request %d should be allowed (within burst)", i)
		}
	}

	// Next request should be rate-limited (bucket empty)
	if limiter.Allow() {
		t.Fatal("request should be rate-limited after burst exhausted")
	}

	// Wait for token replenishment (100ms for 10 req/s = 1 token)
	time.Sleep(110 * time.Millisecond)

	// Should have 1 token available now
	if !limiter.Allow() {
		t.Fatal("request should be allowed after token replenishment")
	}
}

// TestWait verifies that Wait() blocks until a token is available.
func TestWait(t *testing.T) {
	// Create limiter with 10 req/s, burst of 1
	limiter := New(10, 1)

	ctx := context.Background()

	// First request should be immediate (within burst)
	if err := limiter.Wait(ctx); err != nil {
		t.Fatalf("first request should succeed: %v", err)
	}

	// Second request should wait (bucket empty)
	start := time.Now()
	if err := limiter.Wait(ctx); err != nil {
		t.Fatalf("second request should succeed after waiting: %v", err)
	}
	elapsed := time.Since(start)

	// Should have waited approximately 100ms (1/10 second for 10 req/s)
	// Allow some margin for timing jitter
	if elapsed < 50*time.Millisecond || elapsed > 200*time.Millisecond {
		t.Fatalf("wait time %v outside expected range 50ms-200ms", elapsed)
	}
}

// TestWaitContextCancellation verifies that Wait() respects context cancellation.
func TestWaitContextCancellation(t *testing.T) {
	// Create limiter with very low rate to force waiting
	limiter := New(1, 1)

	// Exhaust the burst
	if !limiter.Allow() {
		t.Fatal("first request should be allowed")
	}

	// Create context with short timeout
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Millisecond)
	defer cancel()

	// Wait should fail with context deadline exceeded
	err := limiter.Wait(ctx)
	if err == nil {
		t.Fatal("Wait() should return error when context is cancelled")
	}
	// Wait() should return an error when context is cancelled
	// The actual error type may vary, so just check that we got an error
	<-ctx.Done() // Ensure context is actually done
	if ctx.Err() != context.DeadlineExceeded {
		t.Fatalf("context should be DeadlineExceeded, got %v", ctx.Err())
	}
}

// TestAllowN verifies that AllowN() correctly handles batch requests.
func TestAllowN(t *testing.T) {
	// Create limiter with burst of 10
	limiter := New(10, 10)

	// Requesting 5 tokens should succeed (within burst)
	if !limiter.AllowN(5) {
		t.Fatal("AllowN(5) should succeed with burst of 10")
	}

	// Requesting 5 more tokens should succeed (total 10, at burst limit)
	if !limiter.AllowN(5) {
		t.Fatal("AllowN(5) should succeed, total 10 within burst")
	}

	// Requesting 1 more token should fail (bucket empty)
	if limiter.AllowN(1) {
		t.Fatal("AllowN(1) should fail after burst exhausted")
	}
}

// TestSetLimit verifies dynamic rate limit adjustment.
func TestSetLimit(t *testing.T) {
	// Start with small rate and burst
	limiter := New(10, 10)

	// Exhaust the initial burst
	for i := 0; i < 10; i++ {
		limiter.Allow()
	}

	// Verify bucket is empty
	if limiter.Allow() {
		t.Fatal("bucket should be empty after exhausting burst")
	}

	// Change rate to 100 req/s
	limiter.SetLimit(100)

	// Wait a bit for tokens to accumulate at new rate (200ms = 20 tokens at 100 req/s)
	time.Sleep(200 * time.Millisecond)

	// Should now allow more requests due to higher rate
	allowed := 0
	for i := 0; i < 50; i++ {
		if limiter.Allow() {
			allowed++
		} else {
			break
		}
	}

	// Should have accumulated ~20 tokens, allow some margin
	if allowed < 15 || allowed > 25 {
		t.Fatalf("expected ~20 requests allowed with new rate, got %d", allowed)
	}
}

// TestSetBurst verifies dynamic burst size adjustment.
func TestSetBurst(t *testing.T) {
	// Start with high rate and small burst
	limiter := New(1000, 10)

	// Exhaust the initial burst
	for i := 0; i < 10; i++ {
		limiter.Allow()
	}

	// Change burst to 50
	limiter.SetBurst(50)

	// Wait a bit for tokens to accumulate at high rate (100ms = 100 tokens at 1000 req/s)
	// But capped by new burst of 50
	time.Sleep(100 * time.Millisecond)

	// Should allow up to new burst (50 tokens max)
	allowed := 0
	for i := 0; i < 60; i++ {
		if limiter.Allow() {
			allowed++
		} else {
			break
		}
	}

	// Should allow close to 50 (new burst size)
	if allowed < 45 || allowed > 55 {
		t.Fatalf("expected ~50 requests allowed, got %d", allowed)
	}
}

// TestTokens verifies that Tokens() returns reasonable values.
func TestTokens(t *testing.T) {
	limiter := New(10, 10)

	// Initially should have close to burst capacity
	initial := limiter.Tokens()
	if initial < 9 || initial > 10 {
		t.Fatalf("initial tokens %f outside expected range 9-10", initial)
	}

	// Consume 5 tokens
	for i := 0; i < 5; i++ {
		limiter.Allow()
	}

	// Should have ~5 tokens left
	remaining := limiter.Tokens()
	if remaining < 4 || remaining > 6 {
		t.Fatalf("remaining tokens %f outside expected range 4-6", remaining)
	}
}

// TestUnlimitedRate verifies that zero rate creates effectively unlimited limiter.
func TestUnlimitedRate(t *testing.T) {
	limiter := New(0, 0)

	// Should allow a very large number of requests without blocking
	for i := 0; i < 1000; i++ {
		if !limiter.Allow() {
			t.Fatalf("unlimited limiter should allow request %d", i)
		}
	}
}

// BenchmarkAllow measures the performance of the Allow() fast path.
func BenchmarkAllow(b *testing.B) {
	limiter := New(1_000_000, 1_000_000) // High rate to avoid blocking

	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		limiter.Allow()
	}
}

// BenchmarkAllowParallel measures concurrent Allow() performance.
func BenchmarkAllowParallel(b *testing.B) {
	limiter := New(1_000_000, 1_000_000) // High rate to avoid blocking

	b.ResetTimer()
	b.RunParallel(func(pb *testing.PB) {
		for pb.Next() {
			limiter.Allow()
		}
	})
}
