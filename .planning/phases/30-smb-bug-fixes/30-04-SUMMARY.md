---
phase: 30-smb-bug-fixes
plan: 04
subsystem: adapter
tags: [oplock, smb, nfs, cross-protocol, cache, runtime]

# Dependency graph
requires:
  - phase: 30-01
    provides: sparse file zero-fill fix
  - phase: 30-02
    provides: memory store Move path propagation
  - phase: 30-03
    provides: walkPath parent navigation and NumberOfLinks fix
provides:
  - OplockBreaker interface for cross-protocol oplock coordination
  - NFS v3 write/read/remove/rename oplock break wiring via Runtime
  - SMB adapter OplockManager registration as adapter provider
  - Cached share list for pipe CREATE with event-driven invalidation
affects: [nfs-handlers, smb-handlers, cross-protocol-locking]

# Tech tracking
tech-stack:
  added: []
  patterns: [adapter-provider-pattern, fire-and-forget-oplock, rwmutex-double-check-cache]

key-files:
  created: []
  modified:
    - pkg/adapter/adapter.go
    - pkg/adapter/smb/adapter.go
    - internal/adapter/nfs/v3/handlers/doc.go
    - internal/adapter/nfs/v3/handlers/write.go
    - internal/adapter/nfs/v3/handlers/read.go
    - internal/adapter/nfs/v3/handlers/remove.go
    - internal/adapter/nfs/v3/handlers/rename.go
    - internal/adapter/smb/v2/handlers/handler.go
    - internal/adapter/smb/v2/handlers/create.go

key-decisions:
  - "Fire-and-forget oplock breaks in NFS handlers (per Samba behavior, NFS has no mechanism to delay for breaks)"
  - "OplockBreaker interface in pkg/adapter to avoid import cycles between NFS and SMB packages"
  - "Best-effort child handle lookup for remove/rename oplock breaks (lookup failure does not block operation)"
  - "Double-check locking pattern for share cache rebuild to prevent thundering herd"

patterns-established:
  - "Adapter provider pattern: register cross-protocol services via Runtime.SetAdapterProvider/GetAdapterProvider"
  - "Fire-and-forget oplock break: log result but never fail the NFS operation"

requirements-completed: [BUG-04, BUG-06]

# Metrics
duration: 4min
completed: 2026-02-27
---

# Phase 30 Plan 04: Cross-Protocol Oplock Break + Pipe Share Cache Summary

**OplockBreaker interface wired across NFS v3 handlers with fire-and-forget break pattern, plus RWMutex-cached share list for pipe CREATE with OnShareChange invalidation**

## Performance

- **Duration:** 4 min
- **Started:** 2026-02-27T13:10:09Z
- **Completed:** 2026-02-27T13:14:44Z
- **Tasks:** 2
- **Files modified:** 9

## Accomplishments
- Defined OplockBreaker interface in pkg/adapter for protocol-decoupled oplock coordination
- Wired NFS v3 write/read/remove/rename handlers to trigger SMB oplock breaks via Runtime adapter provider
- Cached share list in SMB Handler with RWMutex, invalidated via Runtime.OnShareChange callback
- Replaced all TODO(plan-03) placeholders with actual oplock break implementations

## Task Commits

Each task was committed atomically:

1. **Task 1: Define OplockBreaker interface and wire NFS v3 handlers + SMB adapter registration** - `c5807445` (feat)
2. **Task 2: Cache pipe share list with event-driven invalidation** - `4d24a0cf` (feat)

## Files Created/Modified
- `pkg/adapter/adapter.go` - Added OplockBreaker interface and OplockBreakerProviderKey constant
- `pkg/adapter/smb/adapter.go` - Register OplockManager as adapter provider + share change callback
- `internal/adapter/nfs/v3/handlers/doc.go` - Added getOplockBreaker helper method on Handler
- `internal/adapter/nfs/v3/handlers/write.go` - Replaced TODO with oplock break for writes
- `internal/adapter/nfs/v3/handlers/read.go` - Replaced TODO with oplock break for reads
- `internal/adapter/nfs/v3/handlers/remove.go` - Replaced TODO with oplock break before deletion (resolves child handle)
- `internal/adapter/nfs/v3/handlers/rename.go` - Replaced TODO with oplock break on source and destination
- `internal/adapter/smb/v2/handlers/handler.go` - Added cached share list fields, getCachedShares, invalidateShareCache, RegisterShareChangeCallback
- `internal/adapter/smb/v2/handlers/create.go` - Replaced per-request share list rebuild with cached lookup

## Decisions Made
- Fire-and-forget oplock breaks in NFS handlers: per Samba behavior, NFS has no mechanism to delay responses for oplock breaks
- OplockBreaker interface in pkg/adapter (shared package) to avoid import cycles between NFS and SMB
- Best-effort child handle lookup for remove/rename: if GetChild fails, operation proceeds without oplock break
- Double-check locking pattern for share cache: prevents thundering herd when multiple goroutines hit an invalidated cache

## Deviations from Plan

None - plan executed exactly as written.

## Issues Encountered
None

## User Setup Required
None - no external service configuration required.

## Next Phase Readiness
- Phase 30 (SMB Bug Fixes) is now complete with all 4 plans executed
- Ready for Phase 31 (Windows ACL Support) or further v3.6 milestone work
- Cross-protocol oplock coordination fully operational for concurrent NFS+SMB access

---
*Phase: 30-smb-bug-fixes*
*Completed: 2026-02-27*

## Self-Check: PASSED

- All 9 modified files verified present on disk
- Commit c5807445 (Task 1) verified in git log
- Commit 4d24a0cf (Task 2) verified in git log
- `go build ./...` passes (no import cycles)
- `go vet ./...` passes (no issues)
- All test suites pass (NFS handlers, SMB handlers, adapter packages)
