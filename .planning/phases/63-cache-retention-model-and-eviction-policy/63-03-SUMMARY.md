---
phase: 63-cache-retention-model-and-eviction-policy
plan: 03
subsystem: blockstore
tags: [eviction, lru, ttl, pin, cache, retention, access-tracker]

requires:
  - phase: 63-01
    provides: RetentionPolicy type and constants (pin/ttl/lru), Share model retention fields

provides:
  - Policy-aware ensureSpace with pin/ttl/lru eviction enforcement
  - Per-file access time tracker for eviction ordering
  - Engine-level SetRetentionPolicy delegation
  - Runtime retention propagation from share creation and update

affects: [63-cache-retention-model-and-eviction-policy]

tech-stack:
  added: []
  patterns:
    - "Per-file access tracking batched in memory (no synchronous I/O per operation)"
    - "Policy-aware eviction with switch on RetentionPolicy constant"
    - "Engine delegation pattern for local store configuration"

key-files:
  created:
    - pkg/blockstore/local/fs/access_tracker.go
    - pkg/blockstore/local/fs/eviction_test.go
  modified:
    - pkg/blockstore/local/fs/eviction.go
    - pkg/blockstore/local/fs/fs.go
    - pkg/blockstore/local/fs/manage.go
    - pkg/blockstore/local/fs/read.go
    - pkg/blockstore/local/fs/write.go
    - pkg/blockstore/engine/engine.go
    - pkg/controlplane/runtime/shares/service.go

key-decisions:
  - "Per-file access tracking (not per-block) for LRU/TTL eviction ordering"
  - "Pin mode returns ErrDiskFull immediately without entering the backpressure loop"
  - "TTL/LRU eviction sorts candidates by file-level access time from accessTracker"
  - "extractPayloadID uses LastIndex for robustness with complex payloadIDs"

patterns-established:
  - "accessTracker.Touch on read/write paths for eviction ordering"
  - "Engine delegates SetRetentionPolicy/SetEvictionEnabled to local store"
  - "UpdateShare propagates policy changes to live BlockStore"

requirements-completed: [CACHE-02, CACHE-03]

duration: 18min
completed: 2026-03-13
---

# Phase 63 Plan 03: Eviction Engine Summary

**Policy-aware eviction with pin (never evict), TTL (time-based), and LRU (access-ordered) modes, wired from share creation through runtime to FSStore**

## Performance

- **Duration:** 18 min
- **Started:** 2026-03-13T13:06:00Z
- **Completed:** 2026-03-13T13:24:12Z
- **Tasks:** 2
- **Files modified:** 9

## Accomplishments
- Implemented per-file access time tracker with batched in-memory updates (no synchronous I/O)
- Rewrote ensureSpace with policy-aware eviction: pin skips eviction entirely, TTL filters by expiry, LRU sorts by file access time
- Wired retention config end-to-end from share creation and runtime updates through engine to FSStore
- Added 12 unit tests covering all eviction policies and access tracker functionality

## Task Commits

Each task was committed atomically:

1. **Task 1: Implement per-file access tracker and policy-aware eviction** - `df7aebe1` (feat)
2. **Task 2: Wire retention config from share creation to FSStore** - `f68f2934` (feat)

## Files Created/Modified
- `pkg/blockstore/local/fs/access_tracker.go` - Per-file last-access time tracking for eviction ordering
- `pkg/blockstore/local/fs/eviction.go` - Policy-aware ensureSpace with evictTTLExpired and evictLRU
- `pkg/blockstore/local/fs/eviction_test.go` - 12 unit tests for access tracker and eviction policies
- `pkg/blockstore/local/fs/fs.go` - FSStore with retentionPolicy, retentionTTL, accessTracker fields
- `pkg/blockstore/local/fs/manage.go` - Removed plan-01 stub SetRetentionPolicy (replaced by real impl)
- `pkg/blockstore/local/fs/read.go` - Added accessTracker.Touch on successful ReadAt
- `pkg/blockstore/local/fs/write.go` - Added accessTracker.Touch on successful WriteAt
- `pkg/blockstore/engine/engine.go` - SetRetentionPolicy and SetEvictionEnabled delegation to local store
- `pkg/controlplane/runtime/shares/service.go` - Wire retention at creation and propagate on UpdateShare

## Decisions Made
- Per-file (not per-block) access time tracking: LRU/TTL decisions are made at file granularity since files are the user-visible unit. The accessTracker uses payloadID as key, not blockID.
- Pin mode short-circuits before the backpressure loop: returns ErrDiskFull immediately, no 30s wait.
- TTL fallback: when accessTracker has no entry for a file, falls back to FileBlock.LastAccess from metadata store.
- extractPayloadID uses strings.LastIndex("/") for robustness with payloadIDs containing slashes.
- Removed plan-01 stub SetRetentionPolicy from manage.go since fs.go now has the real implementation.

## Deviations from Plan

### Auto-fixed Issues

**1. [Rule 1 - Bug] Removed duplicate SetRetentionPolicy method in manage.go**
- **Found during:** Task 1 (build failure)
- **Issue:** Plan 01 created a stub SetRetentionPolicy in manage.go; plan 03 added the real implementation in fs.go, causing a duplicate method error
- **Fix:** Removed the stub from manage.go and its unused time import
- **Files modified:** pkg/blockstore/local/fs/manage.go
- **Verification:** Build passes
- **Committed in:** df7aebe1 (Task 1 commit)

---

**Total deviations:** 1 auto-fixed (1 bug)
**Impact on plan:** Necessary fix for compilation. No scope creep.

## Issues Encountered
None.

## User Setup Required
None - no external service configuration required.

## Next Phase Readiness
- Phase 63 (Cache Retention Model) is complete: all 3 plans executed
- Model types, API/CLI, and eviction engine are all wired together
- Ready for next milestone phase

---
*Phase: 63-cache-retention-model-and-eviction-policy*
*Completed: 2026-03-13*
