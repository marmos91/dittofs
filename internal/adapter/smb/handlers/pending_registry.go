package handlers

import (
	"sync"
)

// pendingRegistry is a thread-safe, multi-index store for async-parked SMB2
// operations (parked CREATEs, blocking LOCKs, pending pipe READs). All three
// concrete registries are the same shape — a primary AsyncId index plus a set
// of secondary lookup indexes and 1:many bucket indexes — so they share this
// generic core and differ only in the indexes they configure and a handful of
// per-entry hooks.
//
// V is the parked-entry value type (PendingCreate, PendingLock,
// PendingPipeRead). Entries are addressed and removed via the primary AsyncId
// returned by the asyncID extractor; every index is kept in lock-step with
// that primary map.
//
// Index kinds:
//
//   - Unique secondary index (index): a 1:1 map from a comparable key to an
//     entry, e.g. (ConnID, MessageID) or FileID. Keys are derived from the
//     entry via a keyFn.
//   - Bucket index (bucketIndex): a 1:many map from a comparable key to the
//     set of entries sharing it, e.g. SessionID or TreeID. Used for bulk
//     teardown.
//
// Concurrency: a single mutex guards all maps. Per-entry side effects (Cancel,
// gate release, displaced-callback fan-out) are invoked by callers AFTER the
// lock is dropped, mirroring the original hand-written registries.
type pendingRegistry[V any] struct {
	mu        sync.Mutex
	byAsyncID map[uint64]*V

	asyncID func(*V) uint64

	indexes []*uniqueIndex[V]
	buckets []*bucketIndex[V]

	// maxOps caps the number of live entries. Zero means unlimited.
	maxOps int
}

// uniqueIndex is a 1:1 secondary index mapping a derived key to one entry.
type uniqueIndex[V any] struct {
	keyFn keyFunc[V]
	m     map[any]*V
}

// bucketIndex is a 1:many index mapping a derived key to all entries that
// share it, keyed internally by AsyncId for O(1) per-entry removal.
type bucketIndex[V any] struct {
	keyFn keyFunc[V]
	m     map[any]map[uint64]*V
}

// keyFunc derives an index key from an entry. Keys are opaque comparables
// wrapped in `any` so one registry can mix key types (e.g. a struct msg-key
// and a [16]byte FileID).
type keyFunc[V any] func(*V) any

// registryConfig configures a pendingRegistry at construction time. indexes
// are 1:1 unique secondary indexes; buckets are 1:many teardown indexes. Each
// is configured by a key extractor and addressed by its ordinal position.
type registryConfig[V any] struct {
	asyncID func(*V) uint64
	indexes []keyFunc[V]
	buckets []keyFunc[V]
	maxOps  int
}

func newPendingRegistry[V any](cfg registryConfig[V]) *pendingRegistry[V] {
	r := &pendingRegistry[V]{
		byAsyncID: make(map[uint64]*V),
		asyncID:   cfg.asyncID,
		maxOps:    cfg.maxOps,
	}
	for _, keyFn := range cfg.indexes {
		r.indexes = append(r.indexes, &uniqueIndex[V]{keyFn: keyFn, m: make(map[any]*V)})
	}
	for _, keyFn := range cfg.buckets {
		r.buckets = append(r.buckets, &bucketIndex[V]{keyFn: keyFn, m: make(map[any]map[uint64]*V)})
	}
	return r
}

// insertLocked adds p to the primary map and every index. Caller holds mu and
// has already validated capacity / duplicates.
func (r *pendingRegistry[V]) insertLocked(p *V) {
	asyncID := r.asyncID(p)
	r.byAsyncID[asyncID] = p
	for _, idx := range r.indexes {
		idx.m[idx.keyFn(p)] = p
	}
	for _, b := range r.buckets {
		key := b.keyFn(p)
		bucket, ok := b.m[key]
		if !ok {
			bucket = make(map[uint64]*V)
			b.m[key] = bucket
		}
		bucket[asyncID] = p
	}
}

// removeLocked drops the entry identified by asyncID from the primary map and
// every index. Returns the removed entry, or nil if absent. Caller holds mu.
func (r *pendingRegistry[V]) removeLocked(asyncID uint64) *V {
	p, ok := r.byAsyncID[asyncID]
	if !ok {
		return nil
	}
	delete(r.byAsyncID, asyncID)
	for _, idx := range r.indexes {
		delete(idx.m, idx.keyFn(p))
	}
	for _, b := range r.buckets {
		key := b.keyFn(p)
		if bucket, ok := b.m[key]; ok {
			delete(bucket, asyncID)
			if len(bucket) == 0 {
				delete(b.m, key)
			}
		}
	}
	return p
}

// lookupLocked returns the entry held by index i under key, or nil. Caller
// holds mu.
func (r *pendingRegistry[V]) lookupLocked(i int, key any) *V {
	return r.indexes[i].m[key]
}

// Len returns the number of live entries.
func (r *pendingRegistry[V]) Len() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return len(r.byAsyncID)
}

// unregisterByAsyncID removes the entry keyed by asyncID, returning it (or nil).
func (r *pendingRegistry[V]) unregisterByAsyncID(asyncID uint64) *V {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.removeLocked(asyncID)
}

// unregisterByIndex removes and returns the entry held by index i under key.
func (r *pendingRegistry[V]) unregisterByIndex(i int, key any) *V {
	r.mu.Lock()
	defer r.mu.Unlock()
	p, ok := r.indexes[i].m[key]
	if !ok {
		return nil
	}
	return r.removeLocked(r.asyncID(p))
}

// unregisterBucket removes and returns every entry held by bucket index i under
// key. Returns nil when the bucket is empty/absent.
func (r *pendingRegistry[V]) unregisterBucket(i int, key any) []*V {
	r.mu.Lock()
	defer r.mu.Unlock()
	bucket, ok := r.buckets[i].m[key]
	if !ok {
		return nil
	}
	removed := make([]*V, 0, len(bucket))
	for asyncID := range bucket {
		if p := r.removeLocked(asyncID); p != nil {
			removed = append(removed, p)
		}
	}
	return removed
}

// unregisterMatching removes and returns every live entry for which pred
// reports true. Used by predicate-based teardown (owner-id, session scan)
// that no dedicated index covers.
func (r *pendingRegistry[V]) unregisterMatching(pred func(*V) bool) []*V {
	r.mu.Lock()
	defer r.mu.Unlock()
	var toRemove []uint64
	for asyncID, p := range r.byAsyncID {
		if pred(p) {
			toRemove = append(toRemove, asyncID)
		}
	}
	removed := make([]*V, 0, len(toRemove))
	for _, asyncID := range toRemove {
		if p := r.removeLocked(asyncID); p != nil {
			removed = append(removed, p)
		}
	}
	return removed
}
