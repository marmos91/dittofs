package badger

import (
	"sync"
	"sync/atomic"

	"github.com/marmos91/dittofs/pkg/metadata"
)

// fileReadCacheCap bounds the read cache. Entries are decoded *File (held
// read-only), so this is a soft cap on the number of tracked hot files.
const fileReadCacheCap = 8192

// fileReadCache is a lock-free cache of decoded File records keyed by fileID, so
// the read hot path (GetFileForRead) skips BOTH the BadgerDB read transaction
// AND the JSON decode of the File blob. That decode inlines the full ChunkRef
// list — measured ~800 µs for a 1 GiB (≈1024-chunk) file — and the per-read
// badger View transaction is the top mutex contender under concurrent reads
// (server pprof, #1169). A random-read fleet hammering one file re-decoded that
// blob on every 4 KiB read; the cache collapses it to one decode per file.
//
// Correctness (single-node badger is single-writer, which makes this tractable —
// cf. #1173, which deferred a cache only for the multi-replica postgres case):
//   - invalidate() runs AFTER a write commits and both deletes the entry and
//     advances `gen`. A reader that observed the pre-commit value cannot leave
//     it cached because its store() is generation-guarded (below).
//   - store() writes only when `gen` is unchanged from the value snapshotted
//     before the backing read; any write to any file that raced the read moves
//     `gen` and the stale populate is dropped. A dropped populate is a cache
//     miss (re-read), never a stale hit.
type fileReadCache struct {
	m     sync.Map     // fileID string -> *metadata.File (held read-only)
	n     atomic.Int64 // approximate entry count for bounding
	gen   atomic.Uint64
	prune atomic.Bool
}

// generation snapshots the invalidation counter; pass the result to store.
func (c *fileReadCache) generation() uint64 { return c.gen.Load() }

// get returns the cached File for key, or (nil,false). The returned pointer is
// the shared cache entry — callers MUST copy before mutating.
func (c *fileReadCache) get(key string) (*metadata.File, bool) {
	v, ok := c.m.Load(key)
	if !ok {
		return nil, false
	}
	return v.(*metadata.File), true
}

// store caches file under key only if no write raced the backing read (the
// generation is unchanged since genAtRead). file MUST NOT be mutated afterwards.
func (c *fileReadCache) store(key string, file *metadata.File, genAtRead uint64) {
	if c.gen.Load() != genAtRead {
		return
	}
	if _, loaded := c.m.Swap(key, file); !loaded {
		if c.n.Add(1) > fileReadCacheCap {
			c.pruneToHalf()
		}
	}
}

// invalidate drops key and advances the generation so any in-flight populate for
// a now-superseded value is rejected. MUST be called AFTER the write commits.
func (c *fileReadCache) invalidate(key string) {
	c.gen.Add(1)
	if _, ok := c.m.LoadAndDelete(key); ok {
		c.n.Add(-1)
	}
}

// pruneToHalf best-effort trims the map back toward half the cap on overflow.
// One pruner at a time; entries drop in Range order (arbitrary) — a dropped file
// just re-populates on its next read.
func (c *fileReadCache) pruneToHalf() {
	if !c.prune.CompareAndSwap(false, true) {
		return
	}
	defer c.prune.Store(false)
	target := int64(fileReadCacheCap / 2)
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
