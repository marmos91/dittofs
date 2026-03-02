package kerberos

import (
	"fmt"
	"sync"
	"sync/atomic"
	"time"
)

// DefaultReplayCacheTTL is the default time-to-live for replay cache entries.
// This matches the typical Kerberos clock skew tolerance of 5 minutes.
const DefaultReplayCacheTTL = 5 * time.Minute

// cleanupInterval controls how often expired entries are purged.
// Cleanup is triggered lazily every N Check calls.
const cleanupInterval = 100

// replayCacheEntry stores the timestamp when an authenticator was first seen.
type replayCacheEntry struct {
	seenAt time.Time
}

// ReplayCache detects duplicate Kerberos authenticators across protocols.
// It is keyed by the 4-tuple (principal, ctime, cusec, servicePrincipal)
// to provide cross-protocol replay detection for both NFS and SMB.
//
// Thread-safe for concurrent NFS and SMB authentication via sync.Map.
type ReplayCache struct {
	entries sync.Map
	ttl     time.Duration
	counter atomic.Int64
}

// NewReplayCache creates a new replay cache with the given TTL.
// If ttl is zero or negative, DefaultReplayCacheTTL is used.
func NewReplayCache(ttl time.Duration) *ReplayCache {
	if ttl <= 0 {
		ttl = DefaultReplayCacheTTL
	}
	return &ReplayCache{
		ttl: ttl,
	}
}

// cacheKey builds the lookup key from the authenticator 4-tuple.
func cacheKey(principal string, ctime time.Time, cusec int, servicePrincipal string) string {
	return fmt.Sprintf("%s|%d|%d|%s", principal, ctime.UnixNano(), cusec, servicePrincipal)
}

// Check returns true if the authenticator has been seen before (replay detected),
// or false if it is a new authenticator (and records it for future checks).
//
// The authenticator is identified by the 4-tuple (principal, ctime, cusec, servicePrincipal).
// Entries expire after the configured TTL.
func (rc *ReplayCache) Check(principal string, ctime time.Time, cusec int, servicePrincipal string) bool {
	key := cacheKey(principal, ctime, cusec, servicePrincipal)
	now := time.Now()

	// Try to load existing entry
	if existing, loaded := rc.entries.Load(key); loaded {
		entry := existing.(*replayCacheEntry)
		// Check if the entry has expired
		if now.Sub(entry.seenAt) < rc.ttl {
			return true // replay detected
		}
		// Entry expired - delete it and treat as new
		rc.entries.Delete(key)
	}

	// Try to store atomically - LoadOrStore ensures only one goroutine succeeds
	entry := &replayCacheEntry{seenAt: now}
	_, loaded := rc.entries.LoadOrStore(key, entry)
	if loaded {
		// Another goroutine stored it first - this is a replay
		return true
	}

	// Lazy cleanup of expired entries
	if rc.counter.Add(1)%cleanupInterval == 0 {
		rc.cleanup(now)
	}

	return false // new authenticator
}

// cleanup removes expired entries from the cache.
func (rc *ReplayCache) cleanup(now time.Time) {
	rc.entries.Range(func(key, value any) bool {
		entry := value.(*replayCacheEntry)
		if now.Sub(entry.seenAt) >= rc.ttl {
			rc.entries.Delete(key)
		}
		return true
	})
}
