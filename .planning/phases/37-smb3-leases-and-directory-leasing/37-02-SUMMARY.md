---
phase: 37-smb3-leases-and-directory-leasing
plan: 02
subsystem: smb
tags: [smb, leases, lease-manager, oplock-manager, smbenc, v2-encoding, cross-protocol]

# Dependency graph
requires:
  - phase: 37-smb3-leases-and-directory-leasing
    provides: LockManager lease CRUD, DirChangeNotifier, V2 type extensions (Plan 01)
provides:
  - Thin SMB LeaseManager wrapper (internal/adapter/smb/lease/) with session-to-lease mapping
  - SMBBreakHandler implementing BreakCallbacks for lease break dispatch
  - SMBOplockBreaker for cross-protocol break coordination via shared LockManager
  - Lease V2 wire format encoding in smbenc (V1/V2 response, break notification, break ack)
  - OplockManager fully deleted (oplock.go, lease.go, cross_protocol.go removed)
  - All SMB handlers migrated to use LeaseManager/LockManager
affects: [37-03, 38-durable-handles, cross-protocol-integration]

# Tech tracking
tech-stack:
  added: []
  patterns: [thin-wrapper-delegation, per-share-lock-resolution, metadataServiceResolver-pattern]

key-files:
  created:
    - internal/adapter/smb/lease/manager.go
    - internal/adapter/smb/lease/notifier.go
    - internal/adapter/smb/smbenc/lease.go
    - internal/adapter/smb/v2/handlers/oplock_constants.go
  modified:
    - internal/adapter/smb/v2/handlers/handler.go
    - internal/adapter/smb/v2/handlers/create.go
    - internal/adapter/smb/v2/handlers/close.go
    - internal/adapter/smb/v2/handlers/stub_handlers.go
    - internal/adapter/smb/v2/handlers/lease_context.go
    - pkg/adapter/smb/adapter.go
    - pkg/adapter/adapter.go

key-decisions:
  - "LockManagerResolver interface pattern for per-share LockManager resolution at request time"
  - "metadataServiceResolver struct bridges MetadataService to lease package without circular imports"
  - "Surviving oplock wire-format types moved to oplock_constants.go (CREATE response still uses OplockLevel)"
  - "Lease wire-format types consolidated into lease_context.go after OplockManager deletion"
  - "Traditional oplock code paths removed entirely (not just disabled)"

patterns-established:
  - "LockManagerResolver: interface for resolving per-share LockManager without holding direct references"
  - "AllSharesResolver: optional extension interface for handle-based LockManager resolution"
  - "metadataServiceResolver: adapter pattern bridging MetadataService to lease package"

requirements-completed: [LEASE-01, LEASE-02, LEASE-03, ARCH-01]

# Metrics
duration: 35min
completed: 2026-03-02
---

# Phase 37 Plan 02: SMB LeaseManager Wrapper and OplockManager Deletion Summary

**Thin SMB LeaseManager wrapper delegating to shared LockManager, V2 wire encoding in smbenc, all handlers migrated, OplockManager deleted with 1,866 lines removed**

## Performance

- **Duration:** ~35 min (across 3 context windows)
- **Started:** 2026-03-02T12:00:00Z
- **Completed:** 2026-03-02T12:49:21Z
- **Tasks:** 2
- **Files modified:** 14 (4 created, 7 modified, 3 deleted)

## Accomplishments

- Created thin SMB LeaseManager wrapper (internal/adapter/smb/lease/) with session-to-lease mapping and LockManager delegation
- Created SMBBreakHandler (BreakCallbacks impl) and SMBOplockBreaker (cross-protocol OplockBreaker impl) in lease/notifier.go
- Added Lease V1/V2 wire format encoding to smbenc (response contexts, break notifications, break ack decoding)
- Migrated all SMB handlers (CREATE, CLOSE, OPLOCK_BREAK) from OplockManager to LeaseManager
- Deleted OplockManager entirely (oplock.go, lease.go, cross_protocol.go) -- 1,866 lines removed
- Created metadataServiceResolver in pkg/adapter/smb/adapter.go bridging MetadataService to lease package

## Task Commits

Each task was committed atomically:

1. **Task 1: Create SMB LeaseManager wrapper, smbenc V2 encoding, and migrate handlers** - `cd1a8476` (feat)
2. **Task 2: Delete OplockManager and clean up obsolete files** - `135cc335` (refactor)

## Files Created/Modified

**Created:**
- `internal/adapter/smb/lease/manager.go` - Thin LeaseManager wrapper (sessionMap + LockManagerResolver delegation)
- `internal/adapter/smb/lease/notifier.go` - LeaseBreakNotifier, SMBBreakHandler, SMBOplockBreaker
- `internal/adapter/smb/smbenc/lease.go` - V1/V2 response encoding, break notification encoding, break ack decoding
- `internal/adapter/smb/v2/handlers/oplock_constants.go` - Surviving oplock wire-format types (OplockBreakRequest/Response/Notification, level constants)

**Modified:**
- `internal/adapter/smb/v2/handlers/handler.go` - Replaced OplockManager field with LeaseManager
- `internal/adapter/smb/v2/handlers/create.go` - Uses ProcessLeaseCreateContext via LeaseManager
- `internal/adapter/smb/v2/handlers/close.go` - Lease release via LeaseManager at session cleanup
- `internal/adapter/smb/v2/handlers/stub_handlers.go` - OPLOCK_BREAK routes lease acks to LeaseManager, rejects traditional acks
- `internal/adapter/smb/v2/handlers/lease_context.go` - Consolidated all lease wire-format types from deleted lease.go
- `pkg/adapter/smb/adapter.go` - metadataServiceResolver, LeaseManager wiring, SMBOplockBreaker registration
- `pkg/adapter/adapter.go` - Updated OplockBreaker comment to reference SMBOplockBreaker

**Deleted:**
- `internal/adapter/smb/v2/handlers/oplock.go` - OplockManager struct and all legacy oplock methods
- `internal/adapter/smb/v2/handlers/lease.go` - OplockManager lease methods (migrated to LeaseManager/lease_context.go)
- `internal/adapter/smb/v2/handlers/cross_protocol.go` - NLM helpers (only used by deleted OplockManager)

## Decisions Made

- **LockManagerResolver interface pattern**: Resolves the correct per-share LockManager at request time without holding direct references. This allows the LeaseManager to work across multiple shares.
- **metadataServiceResolver**: Bridges MetadataService (which owns per-share LockManagers) to the lease package. Implements both LockManagerResolver and AllSharesResolver. Uses metadata.DecodeFileHandle to extract share name from handle format.
- **Surviving oplock types to oplock_constants.go**: OplockLevel constants and OplockBreak wire-format types must survive because CREATE response still uses OplockLevel in the fixed header.
- **Lease types consolidated into lease_context.go**: After OplockManager deletion, all lease wire-format types (LeaseCreateContext, LeaseBreakNotification, LeaseBreakAcknowledgment, decode/encode functions, size constants) moved to lease_context.go.
- **Traditional oplock paths fully removed**: CREATE no longer falls back to traditional oplocks, CLOSE no longer has traditional release branch, OPLOCK_BREAK rejects traditional acks since no OplockManager exists.

## Deviations from Plan

### Auto-fixed Issues

**1. [Rule 3 - Blocking] Added missing metadataServiceResolver struct to adapter.go**
- **Found during:** Task 1
- **Issue:** Plan specified LeaseManager wiring in adapter.go but didn't detail the resolver implementation needed to bridge MetadataService to the lease package
- **Fix:** Created metadataServiceResolver struct implementing LockManagerResolver and AllSharesResolver using metadata.DecodeFileHandle for share extraction
- **Files modified:** pkg/adapter/smb/adapter.go
- **Verification:** go build passes, resolver correctly routes to per-share LockManagers
- **Committed in:** cd1a8476

**2. [Rule 1 - Bug] Fixed missing fmt import in handler.go**
- **Found during:** Task 1
- **Issue:** After adding methods that use fmt.Sprintf, the fmt import was missing
- **Fix:** Added "fmt" to import block
- **Verification:** go build passes
- **Committed in:** cd1a8476

**3. [Rule 1 - Bug] Removed stale fmt import from handler.go after OplockManager deletion**
- **Found during:** Task 2
- **Issue:** After removing OplockManager methods, the fmt import was no longer needed
- **Fix:** Removed "fmt" from import block
- **Verification:** go build passes
- **Committed in:** 135cc335

---

**Total deviations:** 3 auto-fixed (2 bugs, 1 blocking)
**Impact on plan:** All auto-fixes necessary for correct compilation. No scope creep.

## Issues Encountered

- Used non-existent `metadata.ExtractShareFromHandle` initially -- replaced with correct `metadata.DecodeFileHandle()` which returns (shareName, uuid, error). Caught by go build.
- Task execution spanned 3 context windows due to the large scope of files involved.

## User Setup Required

None - no external service configuration required.

## Next Phase Readiness

- All SMB lease operations now route through shared LockManager via thin LeaseManager wrapper
- OplockManager fully deleted -- clean separation between protocol encoding (handlers) and business logic (LockManager)
- SMBOplockBreaker registered for cross-protocol break coordination
- Ready for Phase 38 (Durable Handles) which builds on lease infrastructure

## Self-Check: PASSED

- All 6 expected files exist
- All 3 deleted files confirmed removed
- Both task commits found (cd1a8476, 135cc335)
- go build ./... passes
- go test ./internal/adapter/smb/... passes
- go vet clean

---
*Phase: 37-smb3-leases-and-directory-leasing*
*Completed: 2026-03-02*
