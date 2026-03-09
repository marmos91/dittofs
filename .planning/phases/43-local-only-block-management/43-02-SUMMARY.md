---
phase: 43-local-only-block-management
plan: 02
subsystem: offloader
tags: [offloader, nil-blockstore, local-only, SetRemoteStore, init]

# Dependency graph
requires:
  - phase: 43-local-only-block-management
    provides: Block management methods (DeleteBlockFile, SetEvictionEnabled, EvictMemory) from Plan 01
provides:
  - Nil-safe offloader accepting nil blockStore without panic
  - SetRemoteStore one-shot method for local-to-remote transition
  - Safe zero-value returns for all remote methods when blockStore is nil
  - Local-only init.go path with fsync enabled and eviction disabled
affects: [43-03, 44-blockstore-interface]

# Tech tracking
tech-stack:
  added: []
  patterns: [nil-blockstore-guard-with-debug-log, one-shot-SetRemoteStore, local-only-init-path]

key-files:
  created:
    - pkg/payload/offloader/nil_blockstore_test.go
    - pkg/controlplane/runtime/init_test.go
  modified:
    - pkg/payload/offloader/offloader.go
    - pkg/payload/offloader/upload.go
    - pkg/payload/offloader/download.go
    - pkg/controlplane/runtime/init.go

key-decisions:
  - "Delete cleans up uploads map even with nil blockStore"
  - "Local-only mode disables eviction and enables fsync (disk is final store)"
  - "SetRemoteStore is one-shot to prevent re-entrant race conditions"

patterns-established:
  - "Nil blockStore guard pattern: debug log + safe zero return"
  - "One-shot SetRemoteStore for hot-adding remote backend"

requirements-completed: [LOCAL-02, LOCAL-04]

# Metrics
duration: 6min
completed: 2026-03-09
---

# Phase 43 Plan 02: Nil-Safe Offloader with Local-Only Init Path Summary

**Nil-safe offloader accepting nil blockStore with debug-log guards, SetRemoteStore one-shot transition, and local-only init.go wiring with fsync enabled**

## Performance

- **Duration:** 6 min
- **Started:** 2026-03-09T15:29:14Z
- **Completed:** 2026-03-09T15:35:35Z
- **Tasks:** 2
- **Files modified:** 6

## Accomplishments
- Made offloader nil-blockStore safe: all remote methods return safe defaults with debug logging
- Added SetRemoteStore one-shot method enabling local-to-remote transition
- Start() skips periodic syncer goroutine when blockStore is nil
- HealthCheck returns nil (healthy) for local-only mode
- init.go creates local-only PayloadService when no payload stores configured
- Eviction disabled and fsync enabled for local-only mode (disk is final store)
- 15 unit tests for nil blockStore behavior + 1 integration test for init.go

## Task Commits

Each task was committed atomically:

1. **Task 1: Make offloader nil-blockStore safe with SetRemoteStore** - `c41cd5c0` (test: failing tests), `a851eaa3` (feat: implementation + green)
2. **Task 2: Wire local-only path in init.go with test** - `5395e427` (feat)

_Note: Task 1 followed TDD with RED commit then GREEN commit._

## Files Created/Modified
- `pkg/payload/offloader/offloader.go` - Nil-safe constructor, SetRemoteStore, nil-guard remote methods, local-only HealthCheck and Start
- `pkg/payload/offloader/upload.go` - Nil blockStore guard in uploadPendingBlocks and uploadBlock
- `pkg/payload/offloader/download.go` - Nil blockStore guards in downloadBlock, inlineDownloadOrWait, EnsureAvailable, EnsureAvailableAndRead
- `pkg/payload/offloader/nil_blockstore_test.go` - 15 unit tests for nil blockStore and SetRemoteStore behavior
- `pkg/controlplane/runtime/init.go` - Local-only path passing nil blockStore, fsync and eviction config
- `pkg/controlplane/runtime/init_test.go` - Test for local-only PayloadService initialization

## Decisions Made
- Delete cleans up uploads map even with nil blockStore -- prevents stale tracking state
- Local-only mode disables eviction and enables fsync -- disk IS the final store, so data integrity matters more than write speed
- SetRemoteStore is one-shot to prevent re-entrant race conditions from multiple callers attaching different stores

## Deviations from Plan

None - plan executed exactly as written.

## Issues Encountered
None

## User Setup Required
None - no external service configuration required.

## Next Phase Readiness
- Nil-safe offloader ready for Phase 43 Plan 03 (integration testing)
- Local-only init path ready for Phase 44 (blockstore interface)
- SetRemoteStore provides future hot-migration capability

## Self-Check: PASSED

All files exist, all commits verified.

---
*Phase: 43-local-only-block-management*
*Completed: 2026-03-09*
