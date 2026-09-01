// Package gencache provides the generation-guarded, populate-after-commit cache
// the metadata backends share for their read hot paths.
package gencache

import (
	"sync"
	"sync/atomic"
)

// Cache is a lock-free cache of V keyed by string, guarded by an invalidation
// generation so a populate that raced a write is dropped rather than pinned.
//
// The correctness discipline (the server is a single instance, so the process
// that caches is the process that writes — single-node badger is single-writer,
// which makes this tractable; cf. #1173, which deferred a cache for the
// multi-replica postgres case):
//   - Invalidate runs AFTER a write commits and both deletes the entry and
//     advances gen. A reader that observed the pre-commit value cannot leave it
//     cached because its Store is generation-guarded (below).
//   - Store writes only when gen is unchanged from the value snapshotted before
//     the backing read; any racing write moves gen and the stale populate is
//     dropped. A dropped populate is a cache miss (re-read), never a stale hit.
//
// V must be comparable so a losing populate can retract exactly its own entry
// via sync.Map's CompareAndDelete rather than clobbering a newer reader's.
//
// The zero value is a usable unbounded cache. Set Cap before first use to bound
// it; entries beyond the cap are pruned best-effort.
type Cache[V comparable] struct {
	// Cap is a soft bound on the entry count; 0 means unbounded. It must be set
	// before the cache is first used and is never written again.
	Cap int64

	m     sync.Map // key string -> V
	n     atomic.Int64
	gen   atomic.Uint64
	prune atomic.Bool
}

// Generation snapshots the invalidation counter; pass the result to Store.
func (c *Cache[V]) Generation() uint64 { return c.gen.Load() }

// Get returns the cached value for key, or (zero,false). A returned pointer is
// the shared cache entry — callers MUST copy before mutating.
func (c *Cache[V]) Get(key string) (V, bool) {
	v, ok := c.m.Load(key)
	if !ok {
		var zero V
		return zero, false
	}
	return v.(V), true
}

// Store caches v under key only if no write raced the backing read (the
// generation is unchanged since genAtRead). v MUST NOT be mutated afterwards —
// it becomes the shared cache entry.
func (c *Cache[V]) Store(key string, v V, genAtRead uint64) {
	if c.gen.Load() != genAtRead {
		return
	}
	if _, loaded := c.m.Swap(key, v); !loaded {
		if n := c.n.Add(1); c.Cap > 0 && n > c.Cap {
			c.pruneToHalf()
		}
	}
	// The guard above and the Swap are not atomic: a write could commit and
	// Invalidate (bump gen + delete) in between, leaving our now-stale entry
	// live. Re-check the generation; if it moved, drop our entry — but only if a
	// newer reader hasn't already replaced it (CompareAndDelete on our value).
	if c.gen.Load() != genAtRead {
		if c.m.CompareAndDelete(key, v) {
			c.n.Add(-1)
		}
	}
}

// Invalidate drops key and advances the generation so any in-flight populate for
// a now-superseded value is rejected. MUST be called AFTER the write commits.
//
// Order matters: bump gen BEFORE delete, not after. A concurrent reader that
// snapshotted the old generation and is about to Store a pre-write value is
// rejected the instant gen moves; deleting first would leave a window in which
// that reader's Store (still seeing the old gen) re-inserts the stale entry
// after the delete. The Get path is intentionally ungated by gen: a read that is
// ordered-after this write (the writer returns only once Invalidate finishes,
// within the enclosing transaction) always sees the delete and misses; a read
// merely concurrent with the still-uncommitted write has no ordering guarantee,
// so serving either value is correct.
func (c *Cache[V]) Invalidate(key string) {
	c.gen.Add(1)
	if _, ok := c.m.LoadAndDelete(key); ok {
		c.n.Add(-1)
	}
}

// InvalidateAll drops every entry, for callers that replace the whole backing
// store rather than a single record (Reset, RestoreSnapshot). Same ordering rule
// as Invalidate: bump gen before clearing, so an in-flight populate against the
// pre-wipe generation cannot re-insert after the clear.
func (c *Cache[V]) InvalidateAll() {
	c.gen.Add(1)
	c.m.Clear()
	c.n.Store(0)
}

// pruneToHalf best-effort trims the map back toward half the cap on overflow.
// One pruner at a time; entries drop in Range order (arbitrary) — a dropped
// entry just re-populates on its next read.
func (c *Cache[V]) pruneToHalf() {
	if !c.prune.CompareAndSwap(false, true) {
		return
	}
	defer c.prune.Store(false)
	target := c.Cap / 2
	c.m.Range(func(k, _ any) bool {
		if c.n.Load() <= target {
			return false
		}
		if _, ok := c.m.LoadAndDelete(k); ok {
			c.n.Add(-1)
		}
		return true
	})
}
