package fs

import (
	"container/list"
	"sync"

	"github.com/marmos91/dittofs/pkg/blockstore"
)

// dedupLRU is the in-memory hash dedup LRU consulted between FastCDC
// Next() and the CAS Put(hash, data) path in rollup.go.
//
// Scope: per-share by architecture. Each share owns its own
// local store and therefore its own dedupLRU. Cross-share dedup is
// already handled by FileBlockStore.GetByHash; this LRU's job is to
// skip even that µs hop on hot hashes.
//
// Key scope (#669): compound (hash, payloadID).
// The LRU only short-circuits AddRef for hashes a payload has written
// itself in a prior rollup pass — cross-payload entries are NOT
// reachable. This preserves the "idempotent rewrite of the same
// content" fast path (the dominant case under VM workloads) while
// guaranteeing that an LRU hit cannot reference a FileBlock row owned
// by a DIFFERENT payload, which would let AddRef bump RefCount on the
// wrong row. Cross-payload dedup still works via the regular CAS path
// (StoreChunk is content-addressed and idempotent; the engine syncer
// dedups via FileBlockStore.GetByHash + Put). The LRU short-circuit
// is only an optimization on top of that.
//
// Crash semantics: RAM-only, lost on restart. The first
// post-restart write that would have been an LRU hit falls through to
// FileBlockStore.GetByHash + AddRef (correct, slightly slower for the
// first hot-hash).
//
// Population ordering (#669): callers MUST defer Put until
// AFTER the rollup's ObjectIDPersister callback returns nil. Populating
// the LRU before persister writes the FileBlock row caused a concurrent
// rollup on the same payload to hit the LRU and call AddRef before the
// row existed — returning ErrUnknownHash and triggering a silent retry
// storm under load. The rollup loop now collects (hash, payloadID)
// pairs as it stores chunks and flushes them in a single post-persister
// pass via PutMany.
//
// Concurrency ("Claude's discretion" — flat-mutex chosen here)
// matches the in-package fdpool.go and chunkstore lruTouch precedent
// (sync.Mutex over container/list + map). Striping would add bucket-
// boundary complexity without proven benefit at the 4096-slot default.
// Re-evaluate if a future bench shows mutex contention as the limiter.
//
// All identifiers are package-internal — fsStore wires this into the
// per-share local store; in-package consumers call Get/Put/Has via the
// owning store's field.
type dedupLRU struct {
	mu      sync.Mutex
	index   map[dedupLRUKey]*list.Element
	order   *list.List
	maxSize int
}

// dedupLRUKey is the compound (hash, payloadID) lookup key. Scoping
// the LRU to payloadID is the #669 fix for "wrong-row-owner": an LRU
// hit from a DIFFERENT payload's prior write could otherwise reach
// AddRef, which resolves rows by hash and would bump RefCount on the
// wrong row (legacy multi-row-per-hash data).
type dedupLRUKey struct {
	hash      blockstore.ContentHash
	payloadID string
}

// dedupLRUEntry is the value stored in the LRU list. The struct survives
// for layout symmetry with the on-disk chunk LRU; the key itself
// already carries the (hash, payloadID) tuple.
type dedupLRUEntry struct {
	key dedupLRUKey
}

// newDedupLRU constructs a dedupLRU with the given slot capacity. A
// maxSize <= 0 yields a degenerate no-op LRU (every Get returns false,
// Put is dropped). Config validation in pkg/config rejects non-positive
// values; the no-op behavior here is a defense-in-depth guard so callers
// can construct an LRU before validation completes.
func newDedupLRU(maxSize int) *dedupLRU {
	return &dedupLRU{
		index:   make(map[dedupLRUKey]*list.Element),
		order:   list.New(),
		maxSize: maxSize,
	}
}

// Hit reports whether (hash, payloadID) is present and promotes the
// entry to MRU on hit. Returns false on miss or on a degenerate LRU
// (nil receiver or maxSize <= 0). Use Has for a side-effect-free probe.
func (c *dedupLRU) Hit(hash blockstore.ContentHash, payloadID string) bool {
	if c == nil || c.maxSize <= 0 {
		return false
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	el, found := c.index[dedupLRUKey{hash: hash, payloadID: payloadID}]
	if !found {
		return false
	}
	c.order.MoveToFront(el)
	return true
}

// Has reports whether (hash, payloadID) is present in the LRU. Unlike
// Hit, Has does NOT promote the entry.
func (c *dedupLRU) Has(hash blockstore.ContentHash, payloadID string) bool {
	if c == nil || c.maxSize <= 0 {
		return false
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	_, ok := c.index[dedupLRUKey{hash: hash, payloadID: payloadID}]
	return ok
}

// Put inserts (hash, payloadID) at MRU. Duplicate keys are promoted
// without growing order.Len(). When the LRU is over capacity, the
// least-recently-used entry is evicted.
//
// Callers MUST only invoke Put AFTER the rollup's ObjectIDPersister
// callback has confirmed the FileBlock row for hash is persisted — see
// the type-level comment for the #669 ordering rationale.
func (c *dedupLRU) Put(hash blockstore.ContentHash, payloadID string) {
	if c == nil || c.maxSize <= 0 {
		return
	}
	c.mu.Lock()
	defer c.mu.Unlock()

	key := dedupLRUKey{hash: hash, payloadID: payloadID}
	if el, found := c.index[key]; found {
		c.order.MoveToFront(el)
		return
	}

	for c.order.Len() >= c.maxSize {
		back := c.order.Back()
		if back == nil {
			break
		}
		victim := back.Value.(*dedupLRUEntry)
		delete(c.index, victim.key)
		c.order.Remove(back)
	}

	el := c.order.PushFront(&dedupLRUEntry{key: key})
	c.index[key] = el
}

// PutMany inserts a batch of hashes under a single payloadID at MRU.
// Used by the rollup loop to defer LRU population until after the
// ObjectIDPersister callback has confirmed the corresponding FileBlock
// rows are persisted (see the type-level comment for #669). The
// batched call avoids per-chunk lock-acquire overhead at the rollup tail.
func (c *dedupLRU) PutMany(hashes []blockstore.ContentHash, payloadID string) {
	if c == nil || c.maxSize <= 0 || len(hashes) == 0 {
		return
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	for _, h := range hashes {
		key := dedupLRUKey{hash: h, payloadID: payloadID}
		if el, found := c.index[key]; found {
			c.order.MoveToFront(el)
			continue
		}
		for c.order.Len() >= c.maxSize {
			back := c.order.Back()
			if back == nil {
				break
			}
			victim := back.Value.(*dedupLRUEntry)
			delete(c.index, victim.key)
			c.order.Remove(back)
		}
		el := c.order.PushFront(&dedupLRUEntry{key: key})
		c.index[key] = el
	}
}
