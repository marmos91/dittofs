package identity

import (
	"errors"
	"sync"
	"sync/atomic"
	"time"
)

const (
	// DefaultCacheTTL is the default TTL for positive (Found=true) results.
	DefaultCacheTTL = 5 * time.Minute

	// DefaultNegativeCacheTTL is the default TTL for negative (Found=false) results.
	DefaultNegativeCacheTTL = 30 * time.Second

	// DefaultErrorCacheTTL is the default TTL for infrastructure errors.
	DefaultErrorCacheTTL = 10 * time.Second
)

// CacheStats contains identity cache statistics.
type CacheStats struct {
	Hits   int64
	Misses int64
	Size   int64
}

type cacheEntry struct {
	result   *ResolvedIdentity
	err      error
	cachedAt time.Time
}

type cache struct {
	entries     map[string]*cacheEntry
	mu          sync.RWMutex
	positiveTTL time.Duration
	negativeTTL time.Duration
	errorTTL    time.Duration
	hits        atomic.Int64
	misses      atomic.Int64
}

func newCache(positiveTTL, negativeTTL, errorTTL time.Duration) *cache {
	return &cache{
		entries:     make(map[string]*cacheEntry),
		positiveTTL: positiveTTL,
		negativeTTL: negativeTTL,
		errorTTL:    errorTTL,
	}
}

func (c *cache) get(key string) (*ResolvedIdentity, error, bool) {
	c.mu.RLock()
	entry, ok := c.entries[key]
	c.mu.RUnlock()

	if !ok {
		c.misses.Add(1)
		return nil, nil, false
	}

	if time.Since(entry.cachedAt) >= c.ttlFor(entry) {
		c.mu.Lock()
		if e, still := c.entries[key]; still && time.Since(e.cachedAt) >= c.ttlFor(e) {
			delete(c.entries, key)
		}
		c.mu.Unlock()
		c.misses.Add(1)
		return nil, nil, false
	}

	c.hits.Add(1)
	return entry.result, entry.err, true
}

func (c *cache) put(key string, result *ResolvedIdentity, err error) {
	c.mu.Lock()
	c.entries[key] = &cacheEntry{
		result:   result,
		err:      err,
		cachedAt: time.Now(),
	}
	c.mu.Unlock()
}

func (c *cache) invalidate(key string) {
	c.mu.Lock()
	delete(c.entries, key)
	c.mu.Unlock()
}

func (c *cache) invalidateAll() {
	c.mu.Lock()
	c.entries = make(map[string]*cacheEntry)
	c.mu.Unlock()
}

func (c *cache) stats() CacheStats {
	c.mu.RLock()
	size := int64(len(c.entries))
	c.mu.RUnlock()
	return CacheStats{
		Hits:   c.hits.Load(),
		Misses: c.misses.Load(),
		Size:   size,
	}
}

func (c *cache) ttlFor(entry *cacheEntry) time.Duration {
	if entry.err != nil {
		return c.errorTTL
	}
	if entry.result != nil && entry.result.Found {
		return c.positiveTTL
	}
	return c.negativeTTL
}

// ErrNotConfigured is returned when a FuncLinkStore method is called
// but the corresponding function was not wired.
var ErrNotConfigured = errors.New("identity: operation not configured")
