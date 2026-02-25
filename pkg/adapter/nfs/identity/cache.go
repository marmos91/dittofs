package identity

import (
	"context"
	"sync"
	"sync/atomic"
	"time"
)

// DefaultCacheTTL is the default time-to-live for cached identity resolutions.
// 5 minutes balances freshness with reduced database load.
const DefaultCacheTTL = 5 * time.Minute

// cacheEntry stores a cached resolution result with its timestamp.
type cacheEntry struct {
	result   *ResolvedIdentity
	err      error
	cachedAt time.Time
}

// CacheStats contains identity cache statistics.
type CacheStats struct {
	// Hits is the number of cache hits.
	Hits int64

	// Misses is the number of cache misses.
	Misses int64

	// Size is the current number of entries in the cache.
	Size int64
}

// CachedMapper wraps any IdentityMapper with TTL-based caching.
//
// Resolution results (including errors) are cached to prevent thundering herd
// problems. After the TTL expires, the next request triggers a refresh.
//
// The cache uses a double-check locking pattern: first RLock to check for
// a valid entry, then Lock to populate if needed. This optimizes for the
// common case (cache hit) while remaining correct under concurrency.
type CachedMapper struct {
	inner IdentityMapper
	cache map[string]*cacheEntry
	mu    sync.RWMutex
	ttl   time.Duration

	hits   atomic.Int64
	misses atomic.Int64
}

// NewCachedMapper creates a new caching identity mapper wrapper.
//
// Parameters:
//   - inner: The IdentityMapper to cache results from
//   - ttl: How long to cache results before refreshing. Use DefaultCacheTTL
//     for the recommended 5-minute duration.
func NewCachedMapper(inner IdentityMapper, ttl time.Duration) *CachedMapper {
	return &CachedMapper{
		inner: inner,
		cache: make(map[string]*cacheEntry),
		ttl:   ttl,
	}
}

// Resolve maps a principal, returning cached results when available.
//
// Cache behavior:
//   - Cache hit (within TTL): Returns cached result immediately
//   - Cache miss or expired: Calls inner mapper, caches result
//   - Errors are cached: Prevents thundering herd on infrastructure failures
func (m *CachedMapper) Resolve(ctx context.Context, principal string) (*ResolvedIdentity, error) {
	// Fast path: check cache with read lock
	m.mu.RLock()
	if entry, ok := m.cache[principal]; ok {
		if time.Since(entry.cachedAt) < m.ttl {
			m.mu.RUnlock()
			m.hits.Add(1)
			return entry.result, entry.err
		}
	}
	m.mu.RUnlock()

	// Slow path: acquire write lock
	m.mu.Lock()

	// Double-check: another goroutine may have populated while we waited
	if entry, ok := m.cache[principal]; ok {
		if time.Since(entry.cachedAt) < m.ttl {
			m.mu.Unlock()
			m.hits.Add(1)
			return entry.result, entry.err
		}
	}

	// Call inner mapper
	result, err := m.inner.Resolve(ctx, principal)

	// Cache the result (including errors to prevent thundering herd)
	m.cache[principal] = &cacheEntry{
		result:   result,
		err:      err,
		cachedAt: time.Now(),
	}

	m.mu.Unlock()
	m.misses.Add(1)

	return result, err
}

// Invalidate removes a single entry from the cache.
//
// The next Resolve call for this principal will trigger a fresh lookup.
func (m *CachedMapper) Invalidate(principal string) {
	m.mu.Lock()
	delete(m.cache, principal)
	m.mu.Unlock()
}

// InvalidateAll clears the entire cache.
//
// All subsequent Resolve calls will trigger fresh lookups.
func (m *CachedMapper) InvalidateAll() {
	m.mu.Lock()
	m.cache = make(map[string]*cacheEntry)
	m.mu.Unlock()
}

// Stats returns current cache statistics.
func (m *CachedMapper) Stats() CacheStats {
	m.mu.RLock()
	size := int64(len(m.cache))
	m.mu.RUnlock()

	return CacheStats{
		Hits:   m.hits.Load(),
		Misses: m.misses.Load(),
		Size:   size,
	}
}
