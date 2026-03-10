---
phase: 49-testing-and-documentation
plan: 04
subsystem: benchmarks
tags: [cache, benchmark, cli, blockstore, readcache, performance]

# Dependency graph
requires:
  - phase: 49-01
    provides: "Cache REST API (CacheEvict, CacheStats, CacheEvictForShare, CacheStatsForShare)"
provides:
  - "dfsctl bench cache-tiers command with 6-step cache tier benchmark"
  - "pkg/bench CacheTiersBenchmark runner with configurable file sizes"
  - "CacheTiersResult, CacheTiersSizeResult, IOStats types"
affects: [e2e-tests, documentation]

# Tech tracking
tech-stack:
  added: []
  patterns: [cache-tier-benchmarking, selective-eviction-measurement]

key-files:
  created:
    - "pkg/bench/workload_cache_tiers.go"
    - "cmd/dfsctl/commands/bench/cache_tiers.go"
  modified:
    - "cmd/dfsctl/commands/bench/bench.go"

key-decisions:
  - "Cache-tiers implemented as standalone bench subcommand rather than workload in existing runner, since it requires authenticated API client unlike filesystem-only workloads"
  - "L1 hit rate computed from L1Entries/BlocksTotal ratio from cache stats API"

patterns-established:
  - "API-dependent benchmark pattern: benchmarks that need both filesystem I/O and authenticated API calls use separate CLI command structure"

requirements-completed: [TEST-03]

# Metrics
duration: 2min
completed: 2026-03-10
---

# Phase 49 Plan 04: Cache-Tiers Benchmark Summary

**6-step cache tier benchmark (write/cold/warm/L2) measuring per-layer throughput via selective cache eviction with configurable file sizes and inline table output**

## Performance

- **Duration:** 2 min
- **Started:** 2026-03-10T16:38:01Z
- **Completed:** 2026-03-10T16:40:33Z
- **Tasks:** 1
- **Files modified:** 3

## Accomplishments
- 6-step cache tier benchmark: write, evict-all, cold-read, warm-read, evict-L1, L2-only-read
- Configurable file sizes via --sizes flag (default 10MB, 100MB, 1GB)
- Inline table output showing throughput (MB/s), duration, and L1 hit rate per step
- Graceful error handling: warnings on eviction failures, per-size error recovery
- Requires authentication for cache eviction API (clear error messages on auth failure)

## Task Commits

Each task was committed atomically:

1. **Task 1: Cache-tiers benchmark workload and CLI** - `862c91d8` (feat)

## Files Created/Modified
- `pkg/bench/workload_cache_tiers.go` - 6-step cache-tiers benchmark runner with CacheTiersBenchmark struct, types, and I/O helpers
- `cmd/dfsctl/commands/bench/cache_tiers.go` - Cobra command with --share, --mount, --sizes flags and CacheTiersTable renderer
- `cmd/dfsctl/commands/bench/bench.go` - Registered cache-tiers subcommand

## Decisions Made
- Implemented cache-tiers as a standalone `bench cache-tiers` subcommand rather than adding it as a workload type to the existing `bench run` runner. The existing runner operates purely on filesystem I/O with no API dependency; cache-tiers requires an authenticated API client for cache eviction and stats, making it architecturally distinct.
- L1 hit rate is computed from L1Entries/BlocksTotal ratio returned by the cache stats API, giving a percentage of blocks currently in L1 cache.

## Deviations from Plan

None - plan executed exactly as written.

## Issues Encountered
None

## User Setup Required
None - no external service configuration required.

## Next Phase Readiness
- Cache-tiers benchmark is ready for use against any DittoFS deployment with a mounted share and remote store
- Can be used to validate L1 cache performance improvement (TEST-03 requirement)
- Pairs with cache CLI from Plan 01 for manual cache inspection

---
*Phase: 49-testing-and-documentation*
*Completed: 2026-03-10*
