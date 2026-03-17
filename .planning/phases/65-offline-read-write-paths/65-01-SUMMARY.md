---
phase: 65-offline-read-write-paths
plan: 01
subsystem: blockstore
tags: [resilience, offline, s3, health-check, error-handling, nfs, smb]

# Dependency graph
requires:
  - phase: 64-s3-health-check-and-syncer-resilience
    provides: HealthMonitor with IsRemoteHealthy() and RemoteOutageDuration()
provides:
  - ErrRemoteUnavailable sentinel error for protocol handlers
  - Health-gated fetch/download/prefetch methods in syncer
  - OfflineReadsBlocked counter in CacheStats
  - NFS and SMB error mapping for remote unavailability
affects: [65-02, status-endpoints, monitoring]

# Tech tracking
tech-stack:
  added: []
  patterns: [health-gate-before-remote-op, warn-first-debug-after logging, atomic-counter-for-observability]

key-files:
  created:
    - pkg/blockstore/engine/engine_offline_test.go
  modified:
    - pkg/blockstore/errors.go
    - pkg/blockstore/sync/fetch.go
    - pkg/blockstore/sync/syncer.go
    - pkg/blockstore/engine/engine.go
    - internal/adapter/nfs/xdr/errors.go
    - internal/adapter/smb/v2/handlers/converters.go

key-decisions:
  - "Health gate at syncer level, not engine level, for GetSize/Exists -- syncer methods already check remoteStore==nil and are the natural place for remote health checks"
  - "WARN on first offline read after each health transition, DEBUG for subsequent -- avoids log spam during extended outages"
  - "offlineReadsBlocked counter is atomic int64 on syncer, exposed via OfflineReadsBlocked() accessor and plumbed into CacheStats"

patterns-established:
  - "Health-gate pattern: check IsRemoteHealthy() before any remote operation, return remoteUnavailableError() immediately if unhealthy"
  - "Offline-read logging: atomic CompareAndSwap for first-occurrence WARN, reset on health transitions"

requirements-completed: [RESIL-01, RESIL-02, RESIL-03]

# Metrics
duration: 6min
completed: 2026-03-16
---

# Phase 65 Plan 01: Offline Read/Write Paths Summary

**Health-gated read path with ErrRemoteUnavailable sentinel, protocol error mapping for NFS/SMB, and 5 offline integration tests proving cached reads work, remote-only reads fail fast, and writes succeed during S3 outages**

## Performance

- **Duration:** 6 min
- **Started:** 2026-03-16T21:37:42Z
- **Completed:** 2026-03-16T21:43:42Z
- **Tasks:** 2
- **Files modified:** 7

## Accomplishments
- Added ErrRemoteUnavailable sentinel error with full protocol mapping documentation
- Health-gated all 7 remote-touching syncer methods (fetchBlock, EnsureAvailableAndRead, EnsureAvailable, enqueueDownload, enqueuePrefetch, GetFileSize, Exists)
- Added OfflineReadsBlocked counter to CacheStats for operational observability
- Mapped ErrRemoteUnavailable to NFS3ErrIO and StatusUnexpectedIOError for NFS/SMB clients
- Created 5 integration tests proving all 3 RESIL requirements at engine level

## Task Commits

Each task was committed atomically:

1. **Task 1: Add ErrRemoteUnavailable sentinel and health-gate syncer fetch methods** - `3c79f6d2` (feat)
2. **Task 2: CacheStats counter, engine health gates, protocol error mapping, and integration tests** - `c89fe0dc` (feat)

## Files Created/Modified
- `pkg/blockstore/errors.go` - Added ErrRemoteUnavailable sentinel error
- `pkg/blockstore/sync/fetch.go` - Health-gated fetchBlock, EnsureAvailableAndRead, EnsureAvailable, enqueueDownload, enqueuePrefetch
- `pkg/blockstore/sync/syncer.go` - Added remoteUnavailableError(), OfflineReadsBlocked(), logOfflineRead(), health-gated GetFileSize/Exists, firstOfflineRead reset on transitions
- `pkg/blockstore/engine/engine.go` - Added OfflineReadsBlocked to CacheStats, populated from syncer counter
- `internal/adapter/nfs/xdr/errors.go` - Added ErrRemoteUnavailable -> NFS3ErrIO mapping
- `internal/adapter/smb/v2/handlers/converters.go` - Added ErrRemoteUnavailable -> StatusUnexpectedIOError mapping
- `pkg/blockstore/engine/engine_offline_test.go` - 5 integration tests for offline read/write behavior

## Decisions Made
- Health gate at syncer level (not engine level) for GetSize/Exists -- syncer methods already check remoteStore==nil and are the natural place for remote health checks
- WARN on first offline read after each health transition, DEBUG for subsequent -- avoids log spam during extended outages
- offlineReadsBlocked counter as atomic int64 on syncer, exposed via accessor and plumbed into CacheStats

## Deviations from Plan

None - plan executed exactly as written.

## Issues Encountered
None.

## User Setup Required
None - no external service configuration required.

## Next Phase Readiness
- Core offline read/write resilience logic is complete
- Ready for Phase 65 Plan 02 (status endpoints and health reporting)
- All existing tests continue to pass alongside new offline tests

---
*Phase: 65-offline-read-write-paths*
*Completed: 2026-03-16*
