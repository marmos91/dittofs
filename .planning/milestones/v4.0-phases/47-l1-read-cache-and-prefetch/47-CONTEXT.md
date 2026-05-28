# Phase 47: L1 Read Cache and Prefetch - Context

**Gathered:** 2026-03-09
**Status:** Ready for planning

<domain>
## Phase Boundary

Add a read-through LRU memory cache (L1) for hot blocks and a sequential prefetch system. L1 sits above the existing disk cache (local .blk files) and remote store (S3). Per-share isolation — each BlockStore gets its own L1 instance. Prefetch detects sequential access patterns and pre-loads upcoming blocks into L1. No new protocols, no new CLI commands — pure internal read-path optimization.

</domain>

<decisions>
## Implementation Decisions

### Cache Granularity
- Cache unit is full 8MB blocks (matching existing BlockCache block size)
- Storage: heap-copied `[]byte` per cached block (GC-managed, no mmap/unsafe)
- Full block loaded on miss (even if client requested 4KB slice) — amortizes disk I/O
- L1 caches blocks from ALL sources: local disk AND remote (S3) downloads
- S3 download flow: S3 → write to local .blk file + populate L1 simultaneously (single download, dual destination, no read-back from disk)

### Dirty vs L1 Separation
- L1 is read-only cache of clean block data — completely separate from dirty memBlocks (write path unchanged)
- Read priority: Dirty memBlocks → L1 memory → local disk (.blk) → remote (S3)
- Auto-promote on flush: when dirty memBlock is flushed to disk, its data moves into L1 instead of being discarded (zero-cost since []byte is already allocated)

### Eviction & Sizing
- Per-share L1 (consistent with Phase 46 per-share BlockStore isolation)
- Memory budget expressed in max bytes (e.g., `128MB` per share) — internally converts to block count
- Reasonable default (auto-deduced in Phase 48); setting to 0 disables L1 entirely; trust the user on values
- Plain LRU eviction — no scan resistance for v4.0 (keep simple)
- Copy-on-read: ReadAt copies from L1 entry into caller's buffer atomically; no reference counting needed
- Synchronous eviction: evict LRU entry inline when inserting (dropping []byte reference is O(1), no I/O)
- RWMutex for L1 map (reads take RLock, eviction/fill takes WLock — matches BlockCache's blocksMu pattern)
- No metrics for now — skip instrumentation

### Sequential Prefetch
- Per-file sequential tracker: track last-read block index per payloadID; after 2 consecutive sequential reads, trigger prefetch
- Adaptive depth: start with 1 block ahead, double on each sequential hit up to max 8 blocks (64MB). Linux readahead pattern
- Bounded worker pool for prefetch (fixed goroutine count per share)
- Prefetch destination: L1 + local disk simultaneously (same flow as normal reads — maximum L1 hit rate)
- Prefetch skips blocks already in L1 or on local disk — avoids redundant downloads
- Reset prefetch tracker on non-sequential read (already-queued prefetches complete, no active cancellation)
- Reset prefetch tracker on write/truncate/delete to same payloadID
- Prefetch works for both remote-backed and disk-only shares (disk → L1 eliminates syscall overhead)
- L1 and prefetch are separately configurable (independent toggles)
- Prefetch worker pool size: Claude's discretion (sensible default, auto-deduced in Phase 48)

### Invalidation
- WriteAt: invalidate only the exact L1 block entry for the specific blockIdx being written
- Truncate: remove L1 entries for blocks beyond new size
- Delete: remove all L1 entries for the payloadID
- Secondary index maintained: `map[payloadID][]blockKey` for O(1) per-file invalidation on delete/truncate
- Cross-protocol: automatic — both NFS and SMB use the same per-share BlockStore, any mutation invalidates L1 regardless of protocol

### Shutdown
- Drop L1 on shutdown (read-only cache, all data already persisted on disk/remote). No flush needed

### Code Structure
- L1 cache: `pkg/blockstore/readcache/` sub-package (readcache.go)
- Prefetcher: separate type in `pkg/blockstore/readcache/` (prefetch.go) — separate from ReadCache
- BlockStore orchestrator wires both: ReadCache is mandatory when L1 enabled, Prefetcher is optional
- BlockStore.ReadAt checks L1 first → if miss, calls existing cache.ReadAt + offloader path → fills L1 on return. L1 is transparent to existing cache/offloader code
- Prefetcher reuses BlockStore's internal `loadBlock(payloadID, blockIdx)` method to load blocks (same code path as cache miss). DRY

### Testing
- Unit tests: use memory local store implementation (in-memory mocks for block data)
- Benchmark verification: use real bench/ infrastructure (`dfsctl bench` sequential read workload) to validate L1 cache performance improvement

### Documentation
- All documentation updates (ARCHITECTURE.md, CONFIGURATION.md, CLAUDE.md) deferred to Phase 49

### Claude's Discretion
- Exact struct field layout for ReadCache and Prefetcher
- Prefetch worker pool size default (sensible value, auto-deduced in Phase 48)
- Internal method signatures for loadBlock and L1 fill/evict operations
- LRU data structure details (container/list vs custom, matching fdCache pattern)
- Test file organization and naming
- Error message wording

</decisions>

<specifics>
## Specific Ideas

- S3 downloads should populate both local disk and L1 simultaneously from the same download buffer (no read-back from disk after writing)
- Auto-promote on flush: dirty memBlock data moves into L1 when flushed to disk, avoiding a subsequent disk read for recently-written data
- Prefetch uses adaptive depth (Linux readahead pattern: 1→2→4→8 blocks) — not fixed depth
- L1 is purely a read-path optimization — write path (dirty memBlocks, flush, offloader) is completely unchanged
- Per-share isolation means one share's sequential scan can't evict another share's working set
- Setting L1 to 0 bytes disables the feature entirely — trust the operator

</specifics>

<code_context>
## Existing Code Insights

### Reusable Assets
- `fdCache` in `pkg/cache/fdcache.go`: LRU pattern using `container/list` + `map` with RWMutex — template for ReadCache
- `BlockCache.ReadAt` in `pkg/cache/read.go`: existing read path (dirty memBlock → disk .blk file) — L1 inserts above this
- `PayloadService.readAtInternal` in `pkg/payload/service.go:48`: orchestrates cache.ReadAt + offloader.EnsureAvailable — L1 goes above this in BlockStore
- `offloader.EnsureAvailableAndRead` in `pkg/payload/offloader/offloader.go`: downloads from S3 and serves directly — integration point for L1 fill on remote fetch

### Established Patterns
- LRU eviction with `container/list` (fdCache) — reuse same pattern for ReadCache
- RWMutex for concurrent read access (BlockCache.blocksMu) — apply same to L1
- Per-share BlockStore instances (Phase 46) — L1 is a field on BlockStore
- blockKey struct `{payloadID, blockIdx}` — reuse for L1 entry keys
- 8MB block size constant (BlockSize in cache) — L1 uses same granularity

### Integration Points
- `pkg/blockstore/blockstore.go` (Phase 45/46): BlockStore orchestrator — add ReadCache and Prefetcher fields, modify ReadAt to check L1 first
- `pkg/cache/flush.go`: flush path — add L1 promotion after flushing dirty memBlock to disk
- `pkg/payload/offloader/download.go`: download path — integration point for simultaneous L1 + disk fill
- `pkg/cache/write.go`: write path — add L1 invalidation call for the written block
- `pkg/cache/eviction.go`: eviction path — no L1 interaction needed (disk eviction doesn't affect L1)

</code_context>

<deferred>
## Deferred Ideas

- Per-client prefetch tracking (track sequential patterns per NFS client IP, not just per file) — future enhancement, consider GitHub issue
- L1 hit/miss Prometheus metrics — add instrumentation in a future phase
- Scan-resistant eviction (LRU-K, 2Q, or frequency filter) — revisit if sequential scans pollute L1
- Global L1 cap across all shares — not needed for v4.0, revisit if memory exhaustion becomes an issue
- Documentation updates (ARCHITECTURE.md, CONFIGURATION.md, CLAUDE.md) — Phase 49

</deferred>

---

*Phase: 47-l1-read-cache-and-prefetch*
*Context gathered: 2026-03-09*
