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
// Crash semantics: RAM-only, lost on restart. The first
// post-restart write that would have been an LRU hit falls through to
// FileBlockStore.GetByHash + AddRef (correct, slightly slower for the
// first hot-hash).
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
	index   map[blockstore.ContentHash]*list.Element
	order   *list.List
	maxSize int
}

// dedupLRUEntry is a single LRU-tracked hash → payloadID binding. Kept
// distinct from the on-disk CAS chunk LRU entry in fs.go so the two
// LRUs do not share a value type.
type dedupLRUEntry struct {
	hash      blockstore.ContentHash
	payloadID string
}

// newDedupLRU constructs a dedupLRU with the given slot capacity. A
// maxSize <= 0 yields a degenerate no-op LRU (every Get returns
// ("",false), Put is dropped). Config validation in pkg/config rejects
// non-positive values; the no-op behavior here is a defense-in-depth
// guard so callers can construct an LRU before validation completes.
func newDedupLRU(maxSize int) *dedupLRU {
	return &dedupLRU{
		index:   make(map[blockstore.ContentHash]*list.Element),
		order:   list.New(),
		maxSize: maxSize,
	}
}

// Get returns the payloadID previously bound to hash and promotes the
// entry to MRU. Returns ("",false) on miss or on a degenerate LRU
// (nil receiver or maxSize <= 0).
func (c *dedupLRU) Get(hash blockstore.ContentHash) (payloadID string, ok bool) {
	if c == nil || c.maxSize <= 0 {
		return "", false
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	el, found := c.index[hash]
	if !found {
		return "", false
	}
	c.order.MoveToFront(el)
	return el.Value.(*dedupLRUEntry).payloadID, true
}

// Has reports whether hash is present in the LRU. Unlike Get, Has does
// NOT promote the entry — callers using Has as a pure existence check
// should not pay the lock-upgrade cost of a MoveToFront, and any caller
// that wants the payloadID should call Get directly.
func (c *dedupLRU) Has(hash blockstore.ContentHash) bool {
	if c == nil || c.maxSize <= 0 {
		return false
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	_, ok := c.index[hash]
	return ok
}

// Put binds hash → payloadID and inserts at MRU. If hash is already
// present, the payloadID is updated and the entry is promoted (single
// list element retained — order.Len() does not grow). When the LRU is
// over capacity, the least-recently-used entry is evicted.
func (c *dedupLRU) Put(hash blockstore.ContentHash, payloadID string) {
	if c == nil || c.maxSize <= 0 {
		return
	}
	c.mu.Lock()
	defer c.mu.Unlock()

	if el, found := c.index[hash]; found {
		entry := el.Value.(*dedupLRUEntry)
		entry.payloadID = payloadID
		c.order.MoveToFront(el)
		return
	}

	for c.order.Len() >= c.maxSize {
		back := c.order.Back()
		if back == nil {
			break
		}
		victim := back.Value.(*dedupLRUEntry)
		delete(c.index, victim.hash)
		c.order.Remove(back)
	}

	el := c.order.PushFront(&dedupLRUEntry{hash: hash, payloadID: payloadID})
	c.index[hash] = el
}
