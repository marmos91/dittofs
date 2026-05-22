package engine

import (
	"container/list"
	"context"
	"sync"
	"sync/atomic"

	"github.com/marmos91/dittofs/pkg/blockstore"
)

// seqThreshold (CACHE-03) is the number of consecutive sequential reads
// observed before the prefetcher fires. Set to 3 to suppress speculative
// prefetch on accidental two-block runs in random-IO workloads (VM
// rand-write was the regression).
const seqThreshold = 3

// maxPrefetchDepth caps the number of hashes scheduled per prefetch
// trigger. Doubles each fire (1, 2, 4, 8) and clamps here.
const maxPrefetchDepth = 8

// defaultPrefetchWorkers is the goroutine count for the prefetch worker
// pool when NewCache receives workers <= 0.
const defaultPrefetchWorkers = 4

// reqQueueSize is the bounded channel capacity backing the prefetch
// worker pool (T-12-24 mitigation: non-blocking submit drops when full).
const reqQueueSize = 64

// CacheInterface is the narrow surface engine code depends on. The
// concrete *Cache and the nullCache{} Null Object both satisfy it,
// eliminating defensive nil-checks across the engine package.
type CacheInterface interface {
	Get(hash blockstore.ContentHash) ([]byte, bool)
	Put(hash blockstore.ContentHash, data []byte)
	OnRead(payloadID string, hashes []blockstore.ContentHash, fileSize uint64)
	InvalidateFile(payloadID string, removedHashes []blockstore.ContentHash)
	Stats() CacheStats
	Close() error
}

// Compile-time interface satisfaction checks.
var _ CacheInterface = (*Cache)(nil)
var _ CacheInterface = nullCache{}

// TRANSITIONAL-NEXT-MILESTONE: cold-cache prefetch (see #519 "Deferred
// to v0.17+"). When cold-cache prefetch lands, the Cache will be
// proactively warmed on share-open via an offline LRU snapshot, not
// just on first-read. The current write-side OnChunkComplete wiring
// (engine.go Plan 07) already covers the warm-after-write case; cold-
// cache covers the restart-then-read case.

// Cache is the single-type, CAS-keyed in-memory cache (CACHE-01..05). It
// folds the prefetch worker pool into one type.
//
// On miss, bytes are loaded via local.Get(ctx, hash) — see
// engine.loadByHash. The Cache copies the returned []byte into its
// LRU slot (buffer ownership). No mmap/page-cache fast path exists;
// production workloads are warm-cache and the per-miss alloc is
// uncontended.
//
// Thread safety: read path takes RLock for hits, promotes LRU under
// WLock; mutations are WLock-only. The trackerMu is a separate lock so
// OnRead's hot path (per-payload sequential detection) doesn't contend
// with cache hits/puts.
//
// Per-share isolation (CLAUDE.md rule 4): the Cache lives inside
// *engine.BlockStore which is per-share by construction. Cross-share
// cache sharing is impossible without going through the BlockStore
// boundary, so T-12-25 is "accept" by design.
//
// CACHE-02 cross-file dedup: two payloads referencing the same
// ContentHash share one cache entry. Eviction is hash-scoped (LRU);
// InvalidateFile is surgical (drops only the explicitly-listed hashes
// for a file, preserving any shared entries).
type Cache struct {
	mu       sync.RWMutex
	entries  map[blockstore.ContentHash]*list.Element // hash -> element holding *cacheEntry
	lru      *list.List                               // front = most recent, back = LRU victim
	maxBytes int64
	curBytes int64

	// Worker-pool fields. Constructed by NewCache; left nil by
	// newCacheNoWorkers (test-only constructor for the LRU/Get/Put
	// surface in isolation).
	trackerMu sync.Mutex
	trackers  map[string]*seqTracker // payloadID -> tracker
	reqCh     chan prefetchReq
	loadFn    LoadByHashFn
	ctx       context.Context
	cancel    context.CancelFunc
	wg        sync.WaitGroup

	closed atomic.Bool
}

// cacheEntry is the value stored in each list.Element. Keyed by hash;
// data is a heap-copied slice owned by the cache.
type cacheEntry struct {
	hash blockstore.ContentHash
	data []byte
}

// LoadByHashFn loads a block by ContentHash from the underlying store
// (local FS or remote S3). Injected at NewCache time; called by the
// prefetch worker pool when sequential detection triggers.
//
// Signature is CAS-keyed. The engine bridges to the local/remote
// stores using FormatCASKey.
type LoadByHashFn func(ctx context.Context, hash blockstore.ContentHash) ([]byte, error)

// seqTracker — per-payloadID sequential-read state machine. lastHashes
// is a ring of the most recent OnRead hashes (capped at seqThreshold);
// allHashes is a longer running window used to choose which upcoming
// hashes to prefetch when the threshold is reached.
type seqTracker struct {
	lastHashes []blockstore.ContentHash
	depth      int
	allHashes  []blockstore.ContentHash
	fileSize   uint64
}

// prefetchReq is a work item for the Cache's worker pool.
type prefetchReq struct {
	hash blockstore.ContentHash
}

// CacheStats holds Cache statistics for observability/REST.
//
// JSON tags match the legacy ReadBuffer's CacheStats so external
// consumers (dfsctl block stats output) keep working unchanged.
type CacheStats struct {
	Entries  int   `json:"entries"`
	CurBytes int64 `json:"cur_bytes"`
	MaxBytes int64 `json:"max_bytes"`
}

// nullCache is a no-op CacheInterface implementation. The BlockStore
// constructor substitutes nullCache{} when the cache budget is zero,
// eliminating defensive nil-checks across the engine (Null Object
// pattern).
type nullCache struct{}

func (nullCache) Get(blockstore.ContentHash) ([]byte, bool)       { return nil, false }
func (nullCache) Put(blockstore.ContentHash, []byte)              {}
func (nullCache) OnRead(string, []blockstore.ContentHash, uint64) {}
func (nullCache) InvalidateFile(string, []blockstore.ContentHash) {}
func (nullCache) Stats() CacheStats                               { return CacheStats{} }
func (nullCache) Close() error                                    { return nil }

// newCacheNoWorkers constructs a Cache without the prefetch worker
// pool. Used by tests that exercise the LRU/Get/Put/InvalidateFile
// surface in isolation. Production code uses NewCache.
func newCacheNoWorkers(maxBytes int64) *Cache {
	if maxBytes <= 0 {
		return nil
	}
	return &Cache{
		entries:  make(map[blockstore.ContentHash]*list.Element),
		lru:      list.New(),
		trackers: make(map[string]*seqTracker),
		maxBytes: maxBytes,
	}
}

// NewCache constructs a Cache with the prefetch worker pool fully
// wired. maxBytes <= 0 returns nil (cache disabled — the engine
// constructor's Null Object substitution kicks in).
//
//   - workers <= 0 defaults to defaultPrefetchWorkers (4).
//   - loadFn is the CAS-keyed block loader; required for prefetch to
//     do anything. nil loadFn means OnRead can run but workers will
//     drop requests (no-loader path).
//   - The bounded reqCh has capacity reqQueueSize (64); submit is
//     non-blocking and silently drops on full queue (T-12-24).
func NewCache(maxBytes int64, workers int, loadFn LoadByHashFn) *Cache {
	if maxBytes <= 0 {
		return nil
	}
	if workers <= 0 {
		workers = defaultPrefetchWorkers
	}

	ctx, cancel := context.WithCancel(context.Background())
	c := &Cache{
		entries:  make(map[blockstore.ContentHash]*list.Element),
		lru:      list.New(),
		trackers: make(map[string]*seqTracker),
		maxBytes: maxBytes,
		reqCh:    make(chan prefetchReq, reqQueueSize),
		loadFn:   loadFn,
		ctx:      ctx,
		cancel:   cancel,
	}

	c.wg.Add(workers)
	for i := 0; i < workers; i++ {
		go c.prefetchWorker(ctx)
	}
	return c
}

// Get returns the cached bytes for hash, promoting the entry to the
// front of the LRU list. Returns (nil, false) on miss or after Close.
//
// Returns the cache's own slice — callers must not mutate it. The
// only consumer is engine.ReadAt which copies into the destination
// buffer (source is RAM-only; no mmap aliasing window).
func (c *Cache) Get(hash blockstore.ContentHash) ([]byte, bool) {
	if c == nil || c.closed.Load() {
		return nil, false
	}
	c.mu.RLock()
	elem, ok := c.entries[hash]
	if !ok {
		c.mu.RUnlock()
		return nil, false
	}
	entry := elem.Value.(*cacheEntry)
	data := entry.data
	c.mu.RUnlock()

	// Promote LRU under WLock (re-check existence; Get may race with
	// Close or InvalidateFile).
	c.mu.Lock()
	if elem2, still := c.entries[hash]; still {
		c.lru.MoveToFront(elem2)
	}
	c.mu.Unlock()
	return data, true
}

// Put inserts or updates the cache entry for hash. Heap-copies data so
// callers can reuse their buffers. After Close, Put is a silent no-op.
// Entries larger than maxBytes are silently skipped.
func (c *Cache) Put(hash blockstore.ContentHash, data []byte) {
	if c == nil || c.closed.Load() {
		return
	}
	if int64(len(data)) > c.maxBytes {
		return
	}

	heapCopy := make([]byte, len(data))
	copy(heapCopy, data)

	c.mu.Lock()
	defer c.mu.Unlock()

	if elem, ok := c.entries[hash]; ok {
		old := elem.Value.(*cacheEntry)
		c.curBytes -= int64(len(old.data))
		old.data = heapCopy
		c.curBytes += int64(len(heapCopy))
		c.lru.MoveToFront(elem)
		c.evictLocked()
		return
	}

	entry := &cacheEntry{hash: hash, data: heapCopy}
	elem := c.lru.PushFront(entry)
	c.entries[hash] = elem
	c.curBytes += int64(len(heapCopy))
	c.evictLocked()
}

// evictLocked drops LRU tail entries until curBytes <= maxBytes. Must
// be called under c.mu (write lock).
func (c *Cache) evictLocked() {
	for c.curBytes > c.maxBytes && c.lru.Len() > 0 {
		back := c.lru.Back()
		if back == nil {
			return
		}
		entry := back.Value.(*cacheEntry)
		c.curBytes -= int64(len(entry.data))
		c.lru.Remove(back)
		delete(c.entries, entry.hash)
	}
}

// InvalidateFile drops only the listed hashes from the cache and
// resets the per-payload sequential tracker (so the next read for
// this file rebuilds the prefetch state from scratch). Hashes NOT
// listed remain — including hashes shared by other files (CACHE-02
// dedup is preserved).
//
// CACHE-05 surgical invalidation: callers compute the removed-hash diff
// (via common.diffRemovedHashes) from the old/new BlockRef lists and
// pass it here. Drop semantics
// preserve duplicate-hash multiplicity expectations (callers may pass
// the same hash multiple times; the cache just drops it on first
// match).
func (c *Cache) InvalidateFile(payloadID string, removedHashes []blockstore.ContentHash) {
	if c == nil || c.closed.Load() {
		return
	}

	c.mu.Lock()
	for _, h := range removedHashes {
		if elem, ok := c.entries[h]; ok {
			entry := elem.Value.(*cacheEntry)
			c.curBytes -= int64(len(entry.data))
			c.lru.Remove(elem)
			delete(c.entries, h)
		}
	}
	c.mu.Unlock()

	c.trackerMu.Lock()
	delete(c.trackers, payloadID)
	c.trackerMu.Unlock()
}

// OnRead is the sole prefetch hint API (CACHE-04). The engine calls
// this after a successful ReadAt with the BlockRef hashes that
// satisfied the read; the cache uses the per-payloadID sequential
// tracker to decide whether to fire prefetch on the upcoming hashes.
//
// Tracker semantics:
//
//   - Empty hashes: explicit "reset" signal. Drop the tracker; no
//     prefetch.
//   - First read for a payload: create a tracker; depth=1; no prefetch
//     yet (need seqThreshold = 3 reads to fire).
//   - 3rd+ read on the same payload: schedule `depth` upcoming hashes
//     from the running window. Double depth (capped at
//     maxPrefetchDepth).
//
// fileSize is the file's logical size at read time; cached on the
// tracker for future EOF-aware logic.
func (c *Cache) OnRead(payloadID string, hashes []blockstore.ContentHash, fileSize uint64) {
	if c == nil || c.closed.Load() {
		return
	}
	if len(hashes) == 0 {
		// Explicit reset signal: drop the tracker.
		if payloadID == "" {
			return
		}
		c.trackerMu.Lock()
		delete(c.trackers, payloadID)
		c.trackerMu.Unlock()
		return
	}

	c.trackerMu.Lock()
	tr, ok := c.trackers[payloadID]
	if !ok {
		tr = &seqTracker{
			lastHashes: make([]blockstore.ContentHash, 0, seqThreshold),
			depth:      1,
		}
		c.trackers[payloadID] = tr
	}
	tr.fileSize = fileSize
	for _, h := range hashes {
		tr.lastHashes = append(tr.lastHashes, h)
		if len(tr.lastHashes) > seqThreshold {
			tr.lastHashes = tr.lastHashes[len(tr.lastHashes)-seqThreshold:]
		}
	}
	tr.allHashes = append(tr.allHashes, hashes...)
	if maxWin := seqThreshold + maxPrefetchDepth; len(tr.allHashes) > maxWin {
		tr.allHashes = tr.allHashes[len(tr.allHashes)-maxWin:]
	}

	var toPrefetch []blockstore.ContentHash
	if len(tr.lastHashes) >= seqThreshold {
		want := tr.depth
		if want > maxPrefetchDepth {
			want = maxPrefetchDepth
		}
		start := len(tr.allHashes) - want
		if start < 0 {
			start = 0
		}
		toPrefetch = append(toPrefetch, tr.allHashes[start:]...)
		tr.depth *= 2
		if tr.depth > maxPrefetchDepth {
			tr.depth = maxPrefetchDepth
		}
	}
	c.trackerMu.Unlock()

	for _, h := range toPrefetch {
		c.submitPrefetch(h)
	}
}

// submitPrefetch enqueues a prefetch request, dropping silently when
// the bounded queue is full (T-12-24).
func (c *Cache) submitPrefetch(h blockstore.ContentHash) {
	if c == nil || c.closed.Load() || c.reqCh == nil {
		return
	}
	select {
	case c.reqCh <- prefetchReq{hash: h}:
	default:
		// queue full — drop (natural backpressure).
	}
}

// prefetchWorker is the consumer goroutine for the bounded prefetch
// queue. Skips requests already in cache, calls loadFn, and Puts the
// result. Exits on ctx cancellation.
func (c *Cache) prefetchWorker(ctx context.Context) {
	defer c.wg.Done()
	for {
		select {
		case <-ctx.Done():
			return
		case req, ok := <-c.reqCh:
			if !ok {
				return
			}
			if ctx.Err() != nil {
				return
			}
			if _, hit := c.Get(req.hash); hit {
				continue
			}
			if c.loadFn == nil {
				continue
			}
			data, err := c.loadFn(ctx, req.hash)
			if err != nil {
				continue
			}
			c.Put(req.hash, data)
		}
	}
}

// Stats returns a snapshot of Cache statistics.
func (c *Cache) Stats() CacheStats {
	if c == nil {
		return CacheStats{}
	}
	c.mu.RLock()
	defer c.mu.RUnlock()
	return CacheStats{
		Entries:  len(c.entries),
		CurBytes: c.curBytes,
		MaxBytes: c.maxBytes,
	}
}

// Close stops the prefetch workers, drops all entries, and marks the
// cache as closed. Idempotent. After Close, Get returns miss and Put
// is a silent no-op.
func (c *Cache) Close() error {
	if c == nil {
		return nil
	}
	if !c.closed.CompareAndSwap(false, true) {
		return nil
	}

	if c.cancel != nil {
		c.cancel()
	}
	c.wg.Wait()
	if c.reqCh != nil {
		// Drain any remaining queued requests defensively.
		for {
			select {
			case <-c.reqCh:
			default:
				goto drained
			}
		}
	drained:
	}

	c.mu.Lock()
	c.entries = make(map[blockstore.ContentHash]*list.Element)
	c.lru.Init()
	c.curBytes = 0
	c.mu.Unlock()

	c.trackerMu.Lock()
	c.trackers = make(map[string]*seqTracker)
	c.trackerMu.Unlock()

	return nil
}
