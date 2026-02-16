package identity

import (
	"context"
	"errors"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// ============================================================================
// Mock IdentityMapper for cache tests
// ============================================================================

type mockMapper struct {
	mu         sync.Mutex
	callCount  int
	results    map[string]*ResolvedIdentity
	errs       map[string]error
	resolveErr error
}

func newMockMapper() *mockMapper {
	return &mockMapper{
		results: make(map[string]*ResolvedIdentity),
		errs:    make(map[string]error),
	}
}

func (m *mockMapper) Resolve(_ context.Context, principal string) (*ResolvedIdentity, error) {
	m.mu.Lock()
	m.callCount++
	m.mu.Unlock()

	if m.resolveErr != nil {
		return nil, m.resolveErr
	}

	if err, ok := m.errs[principal]; ok {
		return nil, err
	}

	if result, ok := m.results[principal]; ok {
		return result, nil
	}

	return &ResolvedIdentity{Found: false}, nil
}

func (m *mockMapper) getCallCount() int {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.callCount
}

// ============================================================================
// CachedMapper tests
// ============================================================================

func TestCachedMapper_FirstCallIsCacheMiss(t *testing.T) {
	inner := newMockMapper()
	inner.results["alice@EXAMPLE.COM"] = &ResolvedIdentity{
		Username: "alice",
		UID:      1000,
		GID:      1000,
		Found:    true,
	}

	m := NewCachedMapper(inner, DefaultCacheTTL)
	result, err := m.Resolve(context.Background(), "alice@EXAMPLE.COM")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !result.Found {
		t.Fatal("expected Found=true")
	}
	if result.UID != 1000 {
		t.Fatalf("expected UID=1000, got %d", result.UID)
	}
	if inner.getCallCount() != 1 {
		t.Fatalf("expected 1 inner call, got %d", inner.getCallCount())
	}

	stats := m.Stats()
	if stats.Misses != 1 {
		t.Fatalf("expected 1 miss, got %d", stats.Misses)
	}
	if stats.Hits != 0 {
		t.Fatalf("expected 0 hits, got %d", stats.Hits)
	}
}

func TestCachedMapper_SecondCallIsCacheHit(t *testing.T) {
	inner := newMockMapper()
	inner.results["alice@EXAMPLE.COM"] = &ResolvedIdentity{
		Username: "alice",
		UID:      1000,
		GID:      1000,
		Found:    true,
	}

	m := NewCachedMapper(inner, DefaultCacheTTL)

	// First call - miss
	_, _ = m.Resolve(context.Background(), "alice@EXAMPLE.COM")

	// Second call - hit (should not call inner again)
	result, err := m.Resolve(context.Background(), "alice@EXAMPLE.COM")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !result.Found {
		t.Fatal("expected Found=true")
	}
	if result.UID != 1000 {
		t.Fatalf("expected UID=1000, got %d", result.UID)
	}

	if inner.getCallCount() != 1 {
		t.Fatalf("expected 1 inner call (cached), got %d", inner.getCallCount())
	}

	stats := m.Stats()
	if stats.Hits != 1 {
		t.Fatalf("expected 1 hit, got %d", stats.Hits)
	}
	if stats.Misses != 1 {
		t.Fatalf("expected 1 miss, got %d", stats.Misses)
	}
}

func TestCachedMapper_ExpiredEntryTriggersRefresh(t *testing.T) {
	inner := newMockMapper()
	inner.results["alice@EXAMPLE.COM"] = &ResolvedIdentity{
		Username: "alice",
		UID:      1000,
		GID:      1000,
		Found:    true,
	}

	// Very short TTL for testing
	m := NewCachedMapper(inner, 1*time.Millisecond)

	// First call
	_, _ = m.Resolve(context.Background(), "alice@EXAMPLE.COM")

	// Wait for TTL to expire
	time.Sleep(5 * time.Millisecond)

	// Second call should trigger a refresh
	_, _ = m.Resolve(context.Background(), "alice@EXAMPLE.COM")

	if inner.getCallCount() != 2 {
		t.Fatalf("expected 2 inner calls (expired cache), got %d", inner.getCallCount())
	}
}

func TestCachedMapper_InvalidateRemovesEntry(t *testing.T) {
	inner := newMockMapper()
	inner.results["alice@EXAMPLE.COM"] = &ResolvedIdentity{
		Username: "alice",
		UID:      1000,
		Found:    true,
	}

	m := NewCachedMapper(inner, DefaultCacheTTL)

	// Populate cache
	_, _ = m.Resolve(context.Background(), "alice@EXAMPLE.COM")
	if inner.getCallCount() != 1 {
		t.Fatalf("expected 1 call, got %d", inner.getCallCount())
	}

	// Invalidate
	m.Invalidate("alice@EXAMPLE.COM")

	// Next call should be a miss
	_, _ = m.Resolve(context.Background(), "alice@EXAMPLE.COM")
	if inner.getCallCount() != 2 {
		t.Fatalf("expected 2 calls after invalidation, got %d", inner.getCallCount())
	}
}

func TestCachedMapper_InvalidateAllClearsAll(t *testing.T) {
	inner := newMockMapper()
	inner.results["alice@EXAMPLE.COM"] = &ResolvedIdentity{Username: "alice", Found: true}
	inner.results["bob@EXAMPLE.COM"] = &ResolvedIdentity{Username: "bob", Found: true}

	m := NewCachedMapper(inner, DefaultCacheTTL)

	// Populate cache
	_, _ = m.Resolve(context.Background(), "alice@EXAMPLE.COM")
	_, _ = m.Resolve(context.Background(), "bob@EXAMPLE.COM")
	if inner.getCallCount() != 2 {
		t.Fatalf("expected 2 calls, got %d", inner.getCallCount())
	}

	// Clear all
	m.InvalidateAll()

	stats := m.Stats()
	if stats.Size != 0 {
		t.Fatalf("expected 0 cache entries after InvalidateAll, got %d", stats.Size)
	}

	// Next calls should be misses
	_, _ = m.Resolve(context.Background(), "alice@EXAMPLE.COM")
	_, _ = m.Resolve(context.Background(), "bob@EXAMPLE.COM")
	if inner.getCallCount() != 4 {
		t.Fatalf("expected 4 calls after InvalidateAll, got %d", inner.getCallCount())
	}
}

func TestCachedMapper_ConcurrentAccess(t *testing.T) {
	inner := newMockMapper()
	inner.results["alice@EXAMPLE.COM"] = &ResolvedIdentity{
		Username: "alice",
		UID:      1000,
		Found:    true,
	}

	m := NewCachedMapper(inner, DefaultCacheTTL)

	const goroutines = 50
	var wg sync.WaitGroup
	wg.Add(goroutines)

	var errCount atomic.Int32

	for i := 0; i < goroutines; i++ {
		go func() {
			defer wg.Done()
			result, err := m.Resolve(context.Background(), "alice@EXAMPLE.COM")
			if err != nil {
				errCount.Add(1)
				return
			}
			if !result.Found || result.UID != 1000 {
				errCount.Add(1)
			}
		}()
	}

	wg.Wait()

	if errCount.Load() != 0 {
		t.Fatalf("expected 0 errors, got %d", errCount.Load())
	}

	// The inner mapper should have been called at most a few times
	// (race between goroutines for the first fill), not 50 times
	if inner.getCallCount() > 5 {
		t.Fatalf("expected <= 5 inner calls with caching, got %d", inner.getCallCount())
	}
}

func TestCachedMapper_StatsReturnsCorrectCounts(t *testing.T) {
	inner := newMockMapper()
	inner.results["alice@EXAMPLE.COM"] = &ResolvedIdentity{Found: true}
	inner.results["bob@EXAMPLE.COM"] = &ResolvedIdentity{Found: true}

	m := NewCachedMapper(inner, DefaultCacheTTL)

	// Two misses (first calls)
	_, _ = m.Resolve(context.Background(), "alice@EXAMPLE.COM")
	_, _ = m.Resolve(context.Background(), "bob@EXAMPLE.COM")

	// Two hits (cached calls)
	_, _ = m.Resolve(context.Background(), "alice@EXAMPLE.COM")
	_, _ = m.Resolve(context.Background(), "bob@EXAMPLE.COM")

	stats := m.Stats()
	if stats.Hits != 2 {
		t.Fatalf("expected 2 hits, got %d", stats.Hits)
	}
	if stats.Misses != 2 {
		t.Fatalf("expected 2 misses, got %d", stats.Misses)
	}
	if stats.Size != 2 {
		t.Fatalf("expected 2 cache entries, got %d", stats.Size)
	}
}

func TestCachedMapper_ErrorCachingPreventsThunderingHerd(t *testing.T) {
	dbErr := errors.New("database unavailable")
	inner := newMockMapper()
	inner.resolveErr = dbErr

	m := NewCachedMapper(inner, DefaultCacheTTL)

	// First call triggers inner mapper
	_, err := m.Resolve(context.Background(), "alice@EXAMPLE.COM")
	if !errors.Is(err, dbErr) {
		t.Fatalf("expected dbErr, got %v", err)
	}

	// Second call should return cached error, NOT call inner mapper again
	_, err = m.Resolve(context.Background(), "alice@EXAMPLE.COM")
	if !errors.Is(err, dbErr) {
		t.Fatalf("expected cached dbErr, got %v", err)
	}

	if inner.getCallCount() != 1 {
		t.Fatalf("expected 1 inner call (error cached), got %d", inner.getCallCount())
	}
}

func TestCachedMapper_DifferentPrincipals(t *testing.T) {
	inner := newMockMapper()
	inner.results["alice@EXAMPLE.COM"] = &ResolvedIdentity{Username: "alice", UID: 1000, Found: true}
	inner.results["bob@EXAMPLE.COM"] = &ResolvedIdentity{Username: "bob", UID: 2000, Found: true}

	m := NewCachedMapper(inner, DefaultCacheTTL)

	result1, _ := m.Resolve(context.Background(), "alice@EXAMPLE.COM")
	result2, _ := m.Resolve(context.Background(), "bob@EXAMPLE.COM")

	if result1.UID != 1000 {
		t.Fatalf("expected alice UID=1000, got %d", result1.UID)
	}
	if result2.UID != 2000 {
		t.Fatalf("expected bob UID=2000, got %d", result2.UID)
	}

	if inner.getCallCount() != 2 {
		t.Fatalf("expected 2 inner calls for different principals, got %d", inner.getCallCount())
	}
}
