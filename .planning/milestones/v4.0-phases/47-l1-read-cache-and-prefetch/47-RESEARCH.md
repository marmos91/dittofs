# Phase 47: L1 Read Cache and Prefetch - Research

**Researched:** 2026-03-10
**Domain:** In-memory LRU read cache and sequential prefetch for block storage
**Confidence:** HIGH

## Summary

Phase 47 adds a read-through L1 memory cache with sequential prefetch to the BlockStore engine. The L1 cache sits above the existing disk cache (local .blk files) and remote store (S3), caching hot blocks in heap-allocated `[]byte` slices to eliminate disk I/O for frequently read data. A separate prefetcher detects sequential access patterns and pre-loads upcoming blocks into L1 using a bounded worker pool.

The implementation is well-constrained by the CONTEXT.md decisions: full 8MB block granularity, per-share isolation (one L1 per BlockStore), plain LRU eviction, RWMutex concurrency, and read-only cache (write path unchanged). The codebase already has a proven LRU pattern in `fdCache` (container/list + map), and the `blockKey{payloadID, blockIdx}` type provides the natural cache key.

The primary integration point is `engine.BlockStore.readAtInternal()` which currently checks local cache then falls back to syncer download. L1 inserts between the caller and this existing flow: check L1 first, on miss delegate to the existing read path, fill L1 on return. Write operations (WriteAt, Truncate, Delete) in the engine add L1 invalidation calls.

**Primary recommendation:** Create `pkg/blockstore/readcache/` sub-package with `ReadCache` (LRU cache) and `Prefetcher` (sequential detector + worker pool) types, then wire them into `engine.BlockStore` with L1 check-first in ReadAt and invalidation in WriteAt/Truncate/Delete.

<user_constraints>

## User Constraints (from CONTEXT.md)

### Locked Decisions
- Cache unit is full 8MB blocks (matching existing BlockCache block size)
- Storage: heap-copied `[]byte` per cached block (GC-managed, no mmap/unsafe)
- Full block loaded on miss (even if client requested 4KB slice)
- L1 caches blocks from ALL sources: local disk AND remote (S3) downloads
- S3 download flow: S3 -> write to local .blk file + populate L1 simultaneously
- L1 is read-only cache of clean block data -- completely separate from dirty memBlocks
- Read priority: Dirty memBlocks -> L1 memory -> local disk (.blk) -> remote (S3)
- Auto-promote on flush: dirty memBlock data moves into L1 when flushed to disk
- Per-share L1 (consistent with Phase 46 per-share BlockStore isolation)
- Memory budget in max bytes (e.g., `128MB` per share), converts to block count internally
- Setting to 0 disables L1 entirely; trust the user on values
- Plain LRU eviction (no scan resistance for v4.0)
- Copy-on-read: ReadAt copies from L1 entry into caller's buffer atomically
- Synchronous eviction inline when inserting (O(1), no I/O)
- RWMutex for L1 map (reads take RLock, eviction/fill takes WLock)
- No metrics for now
- Per-file sequential tracker: track last-read block index per payloadID; after 2 consecutive sequential reads, trigger prefetch
- Adaptive depth: start with 1 block ahead, double on each sequential hit up to max 8 blocks (64MB)
- Bounded worker pool for prefetch (fixed goroutine count per share)
- Prefetch destination: L1 + local disk simultaneously
- Prefetch skips blocks already in L1 or on local disk
- Reset prefetch tracker on non-sequential read (queued prefetches complete, no active cancellation)
- Reset prefetch tracker on write/truncate/delete to same payloadID
- Prefetch works for both remote-backed and disk-only shares
- L1 and prefetch are separately configurable (independent toggles)
- WriteAt invalidates only the exact L1 block entry for the specific blockIdx
- Truncate removes L1 entries for blocks beyond new size
- Delete removes all L1 entries for the payloadID
- Secondary index: `map[payloadID][]blockKey` for O(1) per-file invalidation
- Cross-protocol automatic (same per-share BlockStore)
- Drop L1 on shutdown (read-only cache, no flush needed)
- L1 cache: `pkg/blockstore/readcache/` sub-package (readcache.go)
- Prefetcher: separate type in `pkg/blockstore/readcache/` (prefetch.go)
- BlockStore orchestrator wires both; ReadCache mandatory when L1 enabled, Prefetcher optional
- BlockStore.ReadAt checks L1 first -> if miss, calls existing read path -> fills L1 on return
- Prefetcher reuses BlockStore's internal `loadBlock(payloadID, blockIdx)` method (DRY)
- Unit tests use memory local store implementation
- Benchmark verification via bench/ infrastructure (`dfsctl bench` sequential read workload)
- All documentation updates deferred to Phase 49

### Claude's Discretion
- Exact struct field layout for ReadCache and Prefetcher
- Prefetch worker pool size default (sensible value, auto-deduced in Phase 48)
- Internal method signatures for loadBlock and L1 fill/evict operations
- LRU data structure details (container/list vs custom, matching fdCache pattern)
- Test file organization and naming
- Error message wording

### Deferred Ideas (OUT OF SCOPE)
- Per-client prefetch tracking (track sequential patterns per NFS client IP)
- L1 hit/miss Prometheus metrics
- Scan-resistant eviction (LRU-K, 2Q, or frequency filter)
- Global L1 cap across all shares
- Documentation updates (ARCHITECTURE.md, CONFIGURATION.md, CLAUDE.md) -- Phase 49

</user_constraints>

<phase_requirements>

## Phase Requirements

| ID | Description | Research Support |
|----|-------------|-----------------|
| PERF-01 | L1 read-through LRU cache (readcache.go) for hot blocks | ReadCache type in `pkg/blockstore/readcache/` using `container/list` + `map[blockKey]*list.Element` with RWMutex; matches fdCache pattern |
| PERF-02 | L1 cache invalidation on WriteAt | Engine.WriteAt adds `readCache.Invalidate(payloadID, blockIdx)` for each affected block; Engine.Truncate/Delete use secondary index for O(1) per-file invalidation |
| PERF-03 | Sequential prefetch (prefetch.go) after 2+ sequential reads | Prefetcher type with per-payloadID sequential tracker; adaptive depth 1->2->4->8 blocks; bounded worker pool |
| PERF-04 | Bounded prefetch worker pool, non-blocking | Fixed goroutine pool consuming from a channel; non-blocking submit (drop if channel full); skips blocks already cached |

</phase_requirements>

## Standard Stack

### Core
| Library | Version | Purpose | Why Standard |
|---------|---------|---------|--------------|
| `container/list` | stdlib | Doubly-linked list for LRU eviction | Same pattern already used by `fdCache` in the codebase; O(1) eviction and promotion |
| `sync.RWMutex` | stdlib | Concurrent read access to L1 cache | Matches `blocksMu` pattern in FSStore; reads take RLock, mutations take WLock |
| `sync.Map` | stdlib | Per-payloadID secondary index (optional) | Could use for concurrent access, but plain map+RWMutex is simpler and matches existing patterns |

### Supporting
| Library | Version | Purpose | When to Use |
|---------|---------|---------|-------------|
| `testing` | stdlib | Unit tests for ReadCache and Prefetcher | All test files |
| `testing/synctest` | Go 1.24+ (GOEXPERIMENT=synctest) | Deterministic time-based tests | NOT recommended -- too experimental, use manual channel synchronization |

### Alternatives Considered
| Instead of | Could Use | Tradeoff |
|------------|-----------|----------|
| `container/list` + map | Custom intrusive doubly-linked list | Slightly fewer allocations but more code; fdCache already proves container/list works well in this codebase |
| `sync.RWMutex` | `sync.Map` | sync.Map is optimized for read-mostly with rare writes, but L1 fill/evict are frequent enough that RWMutex is better (matches BlockCache pattern) |
| Channel-based worker pool | `sync.Pool` + goroutines | Channel is simpler, bounded, and provides natural backpressure; matches existing codebase patterns |

**Installation:**
```bash
# No external dependencies needed -- all stdlib
```

## Architecture Patterns

### Recommended Project Structure
```
pkg/blockstore/readcache/
├── doc.go            # Package documentation
├── readcache.go      # ReadCache LRU type
├── readcache_test.go # ReadCache unit tests
├── prefetch.go       # Prefetcher sequential detector + worker pool
└── prefetch_test.go  # Prefetcher unit tests
```

### Pattern 1: LRU Cache with RWMutex (ReadCache)
**What:** LRU cache using `container/list` + `map` with `sync.RWMutex` for concurrent reads
**When to use:** Hot-block caching where reads vastly outnumber writes
**Why this pattern:** Already proven in `pkg/blockstore/local/fs/fdcache.go` -- direct template

```go
// Modeled after fdcache.go pattern
type ReadCache struct {
    mu       sync.RWMutex
    entries  map[blockKey]*list.Element // blockKey -> LRU list element
    lru      *list.List                // front = most recent, back = LRU
    byFile   map[string]map[uint64]struct{} // payloadID -> set of blockIdx (secondary index)
    maxBytes int64                     // memory budget in bytes
    curBytes int64                     // current memory usage
}

type cacheEntry struct {
    key      blockKey
    data     []byte  // heap-copied full 8MB block
    dataSize uint32  // actual valid bytes in data
}
```

**Key operations:**
- `Get(payloadID, blockIdx) ([]byte, uint32, bool)` -- RLock, copy data to caller, promote in LRU
- `Put(payloadID, blockIdx, data []byte, dataSize uint32)` -- WLock, insert, evict LRU if over budget
- `Invalidate(payloadID, blockIdx)` -- WLock, remove single entry
- `InvalidateFile(payloadID)` -- WLock, remove all entries for file via secondary index
- `InvalidateAbove(payloadID, blockIdx)` -- WLock, remove entries where blockIdx >= threshold (truncate)
- `Contains(payloadID, blockIdx) bool` -- RLock, check existence (for prefetch skip)
- `Close()` -- WLock, clear all entries

### Pattern 2: Sequential Prefetch Detector (Prefetcher)
**What:** Per-file sequential read tracker with adaptive depth and bounded worker pool
**When to use:** Detecting sequential read patterns and pre-loading upcoming blocks

```go
type Prefetcher struct {
    mu       sync.Mutex
    trackers map[string]*seqTracker // payloadID -> sequential access tracker
    pool     chan prefetchReq       // bounded channel for worker pool
    done     chan struct{}          // shutdown signal
    loadFn   func(ctx context.Context, payloadID string, blockIdx uint64) ([]byte, uint32, error)
    cache    *ReadCache             // L1 cache to fill
    local    local.LocalReader      // to check if block is on disk
}

type seqTracker struct {
    lastBlockIdx uint64
    seqCount     int    // consecutive sequential reads (0, 1, 2+)
    depth        int    // current prefetch depth: 1, 2, 4, 8
}
```

**Adaptive depth algorithm (Linux readahead pattern):**
1. First read to a file: record `lastBlockIdx`, `seqCount=0`, `depth=1`
2. Next read is `lastBlockIdx+1`: increment `seqCount` to 1
3. Next read is `lastBlockIdx+1`: `seqCount` reaches 2, trigger prefetch of `depth` blocks
4. On each successful sequential hit: double depth (1->2->4->8, capped at 8)
5. Non-sequential read: reset `seqCount=0`, `depth=1`

### Pattern 3: Engine Integration (Transparent L1)
**What:** ReadCache and Prefetcher wired into `engine.BlockStore` as optional fields
**When to use:** All engine read/write operations

```go
// engine.Config additions
type Config struct {
    Local  local.LocalStore
    Remote remote.RemoteStore
    Syncer *blocksync.Syncer

    // L1 read cache config (0 = disabled)
    ReadCacheBytes int64

    // Prefetch config (0 workers = disabled)
    PrefetchWorkers int
}
```

**Read path modification (engine.readAtInternal):**
```
1. Check L1 cache (readCache.Get)
   -> HIT: copy to dest, return
   -> MISS: fall through
2. Existing read path (local.ReadAt -> syncer.EnsureAvailableAndRead)
3. On success: fill L1 (readCache.Put with full block data)
4. Notify prefetcher (prefetcher.OnRead for sequential detection)
```

**Write path modifications (engine):**
- `WriteAt`: compute affected blockIdx from offset/len, call `readCache.Invalidate(payloadID, blockIdx)` for each + `prefetcher.Reset(payloadID)`
- `Truncate`: call `readCache.InvalidateAbove(payloadID, newBlockIdx)` + `prefetcher.Reset(payloadID)`
- `Delete`: call `readCache.InvalidateFile(payloadID)` + `prefetcher.Reset(payloadID)`

### Pattern 4: loadBlock Function for DRY Read Path
**What:** Shared function used by both cache-miss path and prefetcher to load a full block
**When to use:** Any time a full 8MB block needs to be loaded from disk or remote

```go
// On engine.BlockStore -- used by both ReadAt (on miss) and Prefetcher
func (bs *BlockStore) loadBlock(ctx context.Context, payloadID string, blockIdx uint64) ([]byte, uint32, error) {
    // 1. Try local store GetBlockData (memory then disk)
    data, dataSize, err := bs.local.GetBlockData(ctx, payloadID, blockIdx)
    if err == nil {
        return data, dataSize, nil
    }

    // 2. Download from remote via syncer.fetchBlock
    data, err = bs.syncer.fetchBlock(ctx, payloadID, blockIdx)
    if err != nil {
        return nil, 0, err
    }
    if data == nil {
        return nil, 0, nil // sparse block
    }

    return data, uint32(len(data)), nil
}
```

Note: `syncer.fetchBlock` is currently unexported. It may need to be exported or an equivalent method provided. The syncer already caches downloaded blocks to local disk via `cache.WriteFromRemote`, so the prefetcher gets the dual benefit (L1 + disk) automatically.

### Anti-Patterns to Avoid
- **Reference counting for cached blocks:** The CONTEXT.md explicitly says copy-on-read. Never return pointers/slices into the cache -- always copy data into the caller's buffer.
- **Modifying the write path:** L1 is read-only. Never write to L1 from WriteAt -- only fill on read-miss and promote on flush.
- **Unbounded prefetch goroutines:** Always use a fixed-size worker pool with a bounded channel. Goroutine-per-prefetch will explode under sequential scans.
- **Invalidating L1 from flush path (wrong direction):** On flush, PROMOTE data to L1 (auto-promote), don't invalidate. The dirty data is being persisted, not changed.
- **Blocking on prefetch completion:** Prefetch is fire-and-forget. The read path never waits for prefetch results.

## Don't Hand-Roll

| Problem | Don't Build | Use Instead | Why |
|---------|-------------|-------------|-----|
| LRU eviction ordering | Custom linked list | `container/list` + map | Proven in fdCache, O(1) operations, standard Go idiom |
| Worker pool | Custom goroutine manager | Buffered channel + fixed workers | Simple, bounded, natural backpressure, matches codebase patterns |
| Block key hashing | Custom hash function | `blockKey` struct as map key | Go maps handle struct keys natively with == comparison |
| Secondary index | Separate data structure | `map[string]map[uint64]struct{}` | Same pattern as `FSStore.fileBlocks` -- proven for per-file block tracking |

**Key insight:** The existing codebase already has all the building blocks (fdCache LRU, blockKey struct, RWMutex patterns, per-file secondary indexes). This phase assembles these proven patterns into a new sub-package.

## Common Pitfalls

### Pitfall 1: Lock Ordering Between ReadCache and Engine
**What goes wrong:** Deadlock if ReadCache holds its lock while calling back into engine code that also acquires locks.
**Why it happens:** The Prefetcher calls `loadBlock` which may acquire local store locks. If ReadCache.Put is called while holding a lock that loadBlock also needs, deadlock occurs.
**How to avoid:** ReadCache never calls external code while holding its lock. The fill pattern is: (1) load block data outside ReadCache locks, (2) acquire WLock, (3) insert entry, (4) release WLock.
**Warning signs:** Test hangs, goroutine dump shows multiple goroutines waiting on the same mutex.

### Pitfall 2: Stale L1 Data After Write
**What goes wrong:** Client reads stale data from L1 after a write to the same block.
**Why it happens:** WriteAt in engine delegates to `local.WriteAt` but forgets to invalidate L1.
**How to avoid:** Every mutation path (WriteAt, Truncate, Delete) MUST invalidate L1 entries. Add invalidation before returning from the mutation.
**Warning signs:** NFS clients see old data after writing, file content doesn't match what was written.

### Pitfall 3: Prefetch Channel Backlog
**What goes wrong:** Under heavy sequential reads, the prefetch channel fills up and requests are silently dropped, reducing prefetch effectiveness.
**Why it happens:** Worker pool processes blocks slower than the sequential reader requests them (e.g., S3 downloads are slow).
**How to avoid:** This is actually the correct behavior -- bounded channel provides natural backpressure. Don't increase channel size to "fix" it. The adaptive depth already handles this: on fast sequential reads, the reader quickly advances past prefetched blocks, which is fine because the data was going to be read anyway via the normal path.
**Warning signs:** None needed -- this is by design.

### Pitfall 4: Memory Budget Enforcement
**What goes wrong:** L1 uses more memory than configured because eviction races with insertion.
**Why it happens:** Multiple goroutines simultaneously filling L1 on cache miss before eviction runs.
**How to avoid:** Eviction happens under WLock during Put(). Since only one goroutine holds WLock at a time, the budget is strictly enforced. The slight overrun during concurrent loads is bounded by the number of concurrent readers (each adds at most one 8MB block before the next WLock-holder evicts).
**Warning signs:** Memory usage exceeding configured budget by more than `numConcurrentReaders * 8MB`.

### Pitfall 5: Auto-Promote on Flush Integration
**What goes wrong:** The flush path is in `pkg/blockstore/local/fs/flush.go`, not in the engine. The engine doesn't have direct access to the flushed data buffer.
**Why it happens:** The flush releases the memBlock's `[]byte` buffer after writing to disk. By the time engine sees the flush result, the data is already gone.
**How to avoid:** Two approaches: (a) engine calls `loadBlock` after flush to re-read from disk and fill L1 (simple but adds a disk read), or (b) modify the flush path to return the data before releasing it. Option (b) is better but requires changes to the local store interface. Since `Flush` returns `[]FlushedBlock` which has `CachePath` and `DataSize`, the engine can read the data from disk after flush -- it's already in OS page cache so the read is essentially free (no actual I/O).
**Warning signs:** Recently-written data requires a disk read on next access instead of hitting L1.

### Pitfall 6: Import Cycle Between readcache and engine
**What goes wrong:** If `readcache` imports `engine` for the `loadBlock` function, and `engine` imports `readcache`, circular dependency.
**Why it happens:** The Prefetcher needs to call a function that loads blocks, but that function lives in engine.
**How to avoid:** Use dependency injection. The Prefetcher accepts a `func(ctx, payloadID, blockIdx) ([]byte, uint32, error)` load function. Engine wires it to its own `loadBlock` method at construction time.
**Warning signs:** Go compiler error: "import cycle not allowed".

## Code Examples

### Example 1: ReadCache Core Implementation
```go
// pkg/blockstore/readcache/readcache.go
package readcache

import (
    "container/list"
    "sync"
)

type blockKey struct {
    payloadID string
    blockIdx  uint64
}

type cacheEntry struct {
    key      blockKey
    data     []byte
    dataSize uint32
}

type ReadCache struct {
    mu       sync.RWMutex
    entries  map[blockKey]*list.Element
    lru      *list.List
    byFile   map[string]map[uint64]struct{} // secondary index
    maxBytes int64
    curBytes int64
}

func New(maxBytes int64) *ReadCache {
    if maxBytes <= 0 {
        return nil // disabled
    }
    return &ReadCache{
        entries:  make(map[blockKey]*list.Element),
        lru:      list.New(),
        byFile:   make(map[string]map[uint64]struct{}),
        maxBytes: maxBytes,
    }
}

// Get retrieves a block from the cache, copying data into dest.
// Returns the number of valid bytes and true on hit, 0 and false on miss.
func (c *ReadCache) Get(payloadID string, blockIdx uint64, dest []byte, offset uint32) (int, bool) {
    key := blockKey{payloadID: payloadID, blockIdx: blockIdx}

    c.mu.RLock()
    elem, ok := c.entries[key]
    if !ok {
        c.mu.RUnlock()
        return 0, false
    }
    entry := elem.Value.(*cacheEntry)

    // Copy data to caller's buffer (copy-on-read)
    if offset >= entry.dataSize {
        c.mu.RUnlock()
        return 0, false
    }
    n := copy(dest, entry.data[offset:entry.dataSize])
    c.mu.RUnlock()

    // Promote to front (requires WLock)
    c.mu.Lock()
    if elem2, ok2 := c.entries[key]; ok2 {
        c.lru.MoveToFront(elem2)
    }
    c.mu.Unlock()

    return n, true
}

// Put inserts a block into the cache, evicting LRU entries if over budget.
func (c *ReadCache) Put(payloadID string, blockIdx uint64, data []byte, dataSize uint32) {
    key := blockKey{payloadID: payloadID, blockIdx: blockIdx}

    // Make a heap copy
    dataCopy := make([]byte, len(data))
    copy(dataCopy, data)

    c.mu.Lock()
    defer c.mu.Unlock()

    // Update if already cached
    if elem, ok := c.entries[key]; ok {
        old := elem.Value.(*cacheEntry)
        c.curBytes -= int64(len(old.data))
        old.data = dataCopy
        old.dataSize = dataSize
        c.curBytes += int64(len(dataCopy))
        c.lru.MoveToFront(elem)
        return
    }

    // Evict until under budget
    for c.curBytes+int64(len(dataCopy)) > c.maxBytes && c.lru.Len() > 0 {
        c.evictLRU()
    }

    entry := &cacheEntry{key: key, data: dataCopy, dataSize: dataSize}
    elem := c.lru.PushFront(entry)
    c.entries[key] = elem
    c.curBytes += int64(len(dataCopy))

    // Update secondary index
    idxSet, ok := c.byFile[payloadID]
    if !ok {
        idxSet = make(map[uint64]struct{})
        c.byFile[payloadID] = idxSet
    }
    idxSet[blockIdx] = struct{}{}
}

func (c *ReadCache) evictLRU() {
    back := c.lru.Back()
    if back == nil {
        return
    }
    entry := back.Value.(*cacheEntry)
    c.lru.Remove(back)
    delete(c.entries, entry.key)
    c.curBytes -= int64(len(entry.data))

    // Clean up secondary index
    if idxSet, ok := c.byFile[entry.key.payloadID]; ok {
        delete(idxSet, entry.key.blockIdx)
        if len(idxSet) == 0 {
            delete(c.byFile, entry.key.payloadID)
        }
    }
}
```

### Example 2: Prefetcher with Adaptive Depth
```go
// pkg/blockstore/readcache/prefetch.go
package readcache

import (
    "context"
    "sync"
)

const (
    seqThreshold    = 2  // consecutive sequential reads before prefetch triggers
    maxPrefetchDepth = 8 // max blocks to prefetch ahead (64MB)
)

type LoadBlockFn func(ctx context.Context, payloadID string, blockIdx uint64) ([]byte, uint32, error)

type seqTracker struct {
    lastBlockIdx uint64
    seqCount     int
    depth        int // 1, 2, 4, 8
}

type prefetchReq struct {
    payloadID string
    blockIdx  uint64
}

type Prefetcher struct {
    mu       sync.Mutex
    trackers map[string]*seqTracker
    reqCh    chan prefetchReq
    cache    *ReadCache
    loadFn   LoadBlockFn
    local    LocalChecker // IsBlockCached + ExistsOnDisk
    cancel   context.CancelFunc
    wg       sync.WaitGroup
}

type LocalChecker interface {
    IsBlockCached(ctx context.Context, payloadID string, blockIdx uint64) bool
}

func NewPrefetcher(workers int, cache *ReadCache, loadFn LoadBlockFn, local LocalChecker) *Prefetcher {
    // ...construct and start workers...
}

// OnRead is called after every successful block read.
// Tracks sequential access and triggers prefetch when pattern detected.
func (p *Prefetcher) OnRead(payloadID string, blockIdx uint64) {
    p.mu.Lock()
    defer p.mu.Unlock()

    tracker, ok := p.trackers[payloadID]
    if !ok {
        tracker = &seqTracker{lastBlockIdx: blockIdx, depth: 1}
        p.trackers[payloadID] = tracker
        return
    }

    if blockIdx == tracker.lastBlockIdx+1 {
        tracker.seqCount++
        tracker.lastBlockIdx = blockIdx
        if tracker.seqCount >= seqThreshold {
            // Trigger prefetch
            for i := 1; i <= tracker.depth; i++ {
                p.submit(payloadID, blockIdx+uint64(i))
            }
            // Double depth for next hit (Linux readahead pattern)
            if tracker.depth < maxPrefetchDepth {
                tracker.depth *= 2
            }
        }
    } else {
        // Non-sequential: reset
        tracker.lastBlockIdx = blockIdx
        tracker.seqCount = 0
        tracker.depth = 1
    }
}
```

### Example 3: Engine Integration
```go
// Modifications to engine/engine.go

func (bs *BlockStore) readAtInternal(ctx context.Context, payloadID, cowSource string, data []byte, offset uint64) (int, error) {
    if len(data) == 0 {
        return 0, nil
    }

    // L1 cache check (block-granularity)
    if bs.readCache != nil {
        blockIdx := offset / blockstore.BlockSize
        blockOffset := uint32(offset % blockstore.BlockSize)
        n, hit := bs.readCache.Get(payloadID, blockIdx, data, blockOffset)
        if hit {
            // Notify prefetcher
            if bs.prefetcher != nil {
                bs.prefetcher.OnRead(payloadID, blockIdx)
            }
            return n, nil
        }
    }

    // Existing read path...
    // After successful read, fill L1
}

func (bs *BlockStore) WriteAt(ctx context.Context, payloadID string, data []byte, offset uint64) error {
    // ... existing write ...
    err := bs.local.WriteAt(ctx, payloadID, data, offset)
    if err != nil {
        return err
    }

    // Invalidate L1 for affected blocks
    if bs.readCache != nil {
        startBlock := offset / blockstore.BlockSize
        endBlock := (offset + uint64(len(data)) - 1) / blockstore.BlockSize
        for blockIdx := startBlock; blockIdx <= endBlock; blockIdx++ {
            bs.readCache.Invalidate(payloadID, blockIdx)
        }
    }
    if bs.prefetcher != nil {
        bs.prefetcher.Reset(payloadID)
    }

    return nil
}
```

## State of the Art

| Old Approach | Current Approach | When Changed | Impact |
|--------------|------------------|--------------|--------|
| Two-tier read (memBlock -> disk) | Three-tier read (L1 -> memBlock -> disk -> remote) | Phase 47 | Eliminates disk I/O for hot blocks; reduces read latency significantly |
| Fixed prefetch in syncer (PrefetchBlocks config) | Adaptive sequential prefetch (1->2->4->8 blocks) | Phase 47 | Better match for real workloads; avoids prefetch waste on random reads |
| Global PrefetchBlocks count | Per-file sequential detection | Phase 47 | Prefetch only triggers for files being read sequentially, not all files |

**Deprecated/outdated:**
- The existing `Syncer.config.PrefetchBlocks` and `enqueuePrefetch` in the syncer will still work for S3-level prefetch (downloading blocks to disk ahead of time). The new L1 prefetcher is complementary -- it loads blocks into L1 memory, which may also trigger disk caching via the load function. The two prefetch mechanisms operate at different levels and do not conflict.

## Open Questions

1. **ReadAt Spanning Multiple Blocks**
   - What we know: ReadAt can span multiple blocks (the existing code loops through blocks). L1 check needs to handle this -- either check all blocks in the range at once, or check per-block.
   - What's unclear: Should L1 serve partial hits (some blocks from L1, some from disk)?
   - Recommendation: Check L1 per-block within the loop, matching the existing read pattern. If any block misses L1, use the full existing path for that block only. This is simpler than all-or-nothing and still provides benefit for partial hits.

2. **Auto-Promote on Flush: Data Access**
   - What we know: `flushBlock()` releases the memBlock buffer after writing to disk. The engine doesn't have direct access to the data at flush time.
   - What's unclear: Best approach to capture data for L1 promotion without adding a disk read.
   - Recommendation: The engine can hook into the flush result. Since `Flush()` returns `[]FlushedBlock` with `CachePath`, the engine can read the flushed data from disk (it's in OS page cache, so essentially free) and populate L1. Alternatively, add a callback to the flush path to capture the data before the buffer is released. The first approach is simpler and avoids local store interface changes.

3. **loadBlock Function: fetchBlock Visibility**
   - What we know: `syncer.fetchBlock` is unexported. The prefetcher needs to load blocks from any tier (L1 -> disk -> remote).
   - What's unclear: Whether to export `fetchBlock` or create a new method on the engine.
   - Recommendation: Create a `loadBlock` method on `engine.BlockStore` that composes `local.GetBlockData` + `syncer.EnsureAvailable` + `local.GetBlockData`. The prefetcher receives this as a `LoadBlockFn` via dependency injection, avoiding import cycles.

## Validation Architecture

### Test Framework
| Property | Value |
|----------|-------|
| Framework | Go testing (stdlib) |
| Config file | None -- standard `go test` |
| Quick run command | `go test ./pkg/blockstore/readcache/...` |
| Full suite command | `go test ./pkg/blockstore/...` |

### Phase Requirements to Test Map
| Req ID | Behavior | Test Type | Automated Command | File Exists? |
|--------|----------|-----------|-------------------|-------------|
| PERF-01 | L1 read-through LRU cache | unit | `go test ./pkg/blockstore/readcache/ -run TestReadCache -v` | Wave 0 |
| PERF-02 | L1 invalidation on WriteAt | unit | `go test ./pkg/blockstore/readcache/ -run TestInvalidate -v` | Wave 0 |
| PERF-03 | Sequential prefetch after 2+ reads | unit | `go test ./pkg/blockstore/readcache/ -run TestPrefetch -v` | Wave 0 |
| PERF-04 | Bounded prefetch worker pool | unit | `go test ./pkg/blockstore/readcache/ -run TestPrefetchBounded -v` | Wave 0 |

### Sampling Rate
- **Per task commit:** `go test ./pkg/blockstore/readcache/... -v -count=1`
- **Per wave merge:** `go test ./pkg/blockstore/... -v -count=1`
- **Phase gate:** `go test ./pkg/blockstore/... -v -count=1 -race` (race detector on)

### Wave 0 Gaps
- [ ] `pkg/blockstore/readcache/readcache_test.go` -- covers PERF-01, PERF-02
- [ ] `pkg/blockstore/readcache/prefetch_test.go` -- covers PERF-03, PERF-04
- [ ] `pkg/blockstore/readcache/doc.go` -- package documentation

## Sources

### Primary (HIGH confidence)
- Codebase: `pkg/blockstore/local/fs/fdcache.go` -- LRU pattern template (container/list + map + Mutex)
- Codebase: `pkg/blockstore/local/fs/read.go` -- existing read path (memBlock -> disk)
- Codebase: `pkg/blockstore/engine/engine.go` -- BlockStore orchestrator (integration point)
- Codebase: `pkg/blockstore/local/fs/flush.go` -- flush path (auto-promote integration)
- Codebase: `pkg/blockstore/sync/fetch.go` -- syncer download path (remote fetch)
- Codebase: `pkg/blockstore/local/memory/memory.go` -- MemoryStore (test dependency)
- Codebase: `pkg/blockstore/local/fs/block.go` -- blockKey type, blockBufPool pattern
- Codebase: `pkg/blockstore/types.go` -- BlockSize constant (8MB), blockKey components

### Secondary (MEDIUM confidence)
- Linux kernel readahead algorithm -- adaptive depth pattern (1->2->4->8 pages/blocks) is well-documented in kernel source and papers
- Go stdlib `container/list` documentation -- O(1) Push/Remove/MoveToFront operations

### Tertiary (LOW confidence)
- None -- all findings verified against codebase

## Metadata

**Confidence breakdown:**
- Standard stack: HIGH -- all stdlib, no external dependencies
- Architecture: HIGH -- direct template from fdCache, clear integration points in engine
- Pitfalls: HIGH -- identified from deep codebase analysis (lock ordering, import cycles, flush data access)
- Prefetch algorithm: HIGH -- Linux readahead pattern is well-established; adaptive depth is the standard approach

**Research date:** 2026-03-10
**Valid until:** Indefinite (stdlib-only, no version dependencies)
