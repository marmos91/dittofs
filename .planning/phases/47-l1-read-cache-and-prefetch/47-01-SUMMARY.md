---
phase: 47-l1-read-cache-and-prefetch
plan: 01
subsystem: blockstore
tags: [lru, cache, prefetch, readcache, sequential-detection, worker-pool]

# Dependency graph
requires:
  - phase: 46-per-share-block-store-wiring
    provides: Per-share BlockStore engine with local+remote composition
provides:
  - ReadCache LRU type with Get/Put/Invalidate/InvalidateFile/InvalidateAbove/Contains/Close
  - Prefetcher type with OnRead/Reset/Close and bounded worker pool
  - Secondary index for O(1) per-file cache invalidation
  - Adaptive prefetch depth (1->2->4->8 blocks) following Linux readahead pattern
affects: [47-02-engine-integration, 47-03-configuration, 48-auto-tuning]

# Tech tracking
tech-stack:
  added: []
  patterns: [LRU with container/list + map, copy-on-read semantics, adaptive sequential prefetch, bounded worker pool with non-blocking submit]

key-files:
  created:
    - pkg/blockstore/readcache/doc.go
    - pkg/blockstore/readcache/readcache.go
    - pkg/blockstore/readcache/readcache_test.go
    - pkg/blockstore/readcache/prefetch.go
    - pkg/blockstore/readcache/prefetch_test.go
  modified: []

key-decisions:
  - "RWMutex with RLock for reads, WLock for mutations -- matches existing BlockCache pattern"
  - "Copy-on-read: Get copies into caller buffer, never returns internal slice"
  - "Synchronous eviction inline during Put (O(1) -- drops []byte reference)"
  - "Adaptive prefetch depth 1->2->4->8 capped at maxPrefetchDepth=8 (Linux readahead pattern)"
  - "Non-blocking submit drops requests when channel full (natural backpressure)"
  - "Dependency-injected LoadBlockFn avoids import cycles with engine package"
  - "NewPrefetcher returns nil if cache is nil (can't prefetch without cache target)"

patterns-established:
  - "ReadCache LRU: container/list + map[blockKey]*list.Element with secondary byFile index"
  - "Prefetcher: sequential tracker map + bounded channel + fixed worker goroutine count"
  - "Nil-safe methods: all ReadCache/Prefetcher methods handle nil receiver gracefully"

requirements-completed: [PERF-01, PERF-02, PERF-03, PERF-04]

# Metrics
duration: 5min
completed: 2026-03-10
---

# Phase 47 Plan 01: ReadCache and Prefetcher Summary

**LRU block cache with copy-on-read semantics and adaptive sequential prefetcher using bounded worker pool**

## Performance

- **Duration:** 5 min
- **Started:** 2026-03-10T12:38:13Z
- **Completed:** 2026-03-10T12:43:30Z
- **Tasks:** 2
- **Files created:** 5

## Accomplishments
- ReadCache LRU type with RWMutex, secondary payloadID index, and copy-on-read semantics
- Prefetcher with sequential detection (threshold=2), adaptive depth (1->2->4->8), and bounded worker pool
- 39 unit tests all passing with race detector enabled
- Zero regressions across entire blockstore package

## Task Commits

Each task was committed atomically:

1. **Task 1: ReadCache LRU type with full test coverage** - `17902846` (feat)
2. **Task 2: Prefetcher sequential detector with bounded worker pool** - `5647f1a5` (feat)

_Both tasks followed TDD: RED (failing tests) -> GREEN (implementation) -> verified with race detector_

## Files Created/Modified
- `pkg/blockstore/readcache/doc.go` - Package documentation for L1 read cache and prefetch
- `pkg/blockstore/readcache/readcache.go` - ReadCache LRU type with Get/Put/Invalidate/InvalidateFile/InvalidateAbove/Contains/Close (228 lines)
- `pkg/blockstore/readcache/readcache_test.go` - 23 unit tests covering all operations, edge cases, and concurrency (296 lines)
- `pkg/blockstore/readcache/prefetch.go` - Prefetcher type with OnRead/Reset/Close and bounded worker pool (191 lines)
- `pkg/blockstore/readcache/prefetch_test.go` - 16 unit tests covering sequential detection, adaptive depth, reset, bounded pool (327 lines)

## Decisions Made
- RWMutex with RLock for reads, WLock for mutations (matches existing BlockCache pattern)
- Copy-on-read: Get copies into caller buffer, never returns internal slice
- Synchronous eviction inline during Put (O(1) -- drops []byte reference)
- Adaptive prefetch depth 1->2->4->8 capped at maxPrefetchDepth=8 (Linux readahead pattern)
- Non-blocking submit drops requests when channel full (natural backpressure)
- Dependency-injected LoadBlockFn avoids import cycles with engine package
- NewPrefetcher returns nil if cache is nil (can't prefetch without cache target)

## Deviations from Plan

None - plan executed exactly as written.

## Issues Encountered

None.

## User Setup Required

None - no external service configuration required.

## Next Phase Readiness
- ReadCache and Prefetcher are standalone, fully tested components
- Ready for engine integration in Plan 02 (wiring into BlockStore.ReadAt)
- No blockers or concerns

## Self-Check: PASSED

All 5 created files verified on disk. Both task commits (17902846, 5647f1a5) verified in git log.

---
*Phase: 47-l1-read-cache-and-prefetch*
*Completed: 2026-03-10*
