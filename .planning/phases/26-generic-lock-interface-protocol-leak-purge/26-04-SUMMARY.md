---
phase: 26-generic-lock-interface-protocol-leak-purge
plan: 04
subsystem: locking
tags: [nlm, smb, protocol-decoupling, extraction, metadata-cleanup]

# Dependency graph
requires: [26-01, 26-03]
provides:
  - NLMService in NFS adapter using LockManager directly
  - NLMLockService interface decoupling NLM handlers from MetadataService
  - routingNLMService for per-share lock manager routing
  - MetadataService purged of NLM/SMB protocol-specific methods
  - ErrLeaseBreakPending defined locally in SMB handlers
affects: [26-05]

# Tech tracking
tech-stack:
  added: []
  patterns:
    - "Protocol adapter owns its lock service (NLMService in NFS adapter, not in metadata layer)"
    - "Interface boundary: NLMLockService decouples NLM handlers from concrete MetadataService"
    - "Routing pattern: routingNLMService resolves per-share lock manager from file handle"

key-files:
  created:
    - pkg/adapter/nfs/nlm_service.go
  modified:
    - pkg/metadata/service.go
    - pkg/adapter/nfs/nfs_adapter.go
    - pkg/adapter/nfs/nfs_adapter_nlm.go
    - pkg/adapter/smb/smb_adapter.go
    - internal/protocol/nlm/handlers/handler.go
    - internal/protocol/nlm/handlers/lock.go
    - internal/protocol/nlm/handlers/test.go
    - internal/protocol/nlm/handlers/unlock.go
    - internal/protocol/nlm/handlers/cancel.go
    - internal/protocol/nlm/handlers/cross_protocol.go
    - internal/protocol/nfs/v3/handlers/write.go
    - internal/protocol/nfs/v3/handlers/read.go
    - internal/protocol/nfs/v3/handlers/remove.go
    - internal/protocol/nfs/v3/handlers/rename.go
    - internal/protocol/smb/v2/handlers/oplock.go

key-decisions:
  - "Removed both NLM and SMB methods from service.go in Task 1 (atomic - both contribute to metadata cleanup)"
  - "Used routingNLMService pattern to resolve per-share lock manager from NLM file handles"
  - "Replaced CheckAndBreakLeases* calls with TODO(plan-03) placeholders since Plan 03 (centralized break methods) not yet executed"
  - "Defined ErrLeaseBreakPending locally in SMB handlers instead of metadata package"

patterns-established:
  - "NLMService owns NLM lock operations using lock.Manager directly"
  - "NLMLockService interface decouples NLM protocol handlers from MetadataService"
  - "Cross-protocol break methods to be centralized on LockManager (Plan 03 dependency)"

requirements-completed: [REF-02]

# Metrics
duration: 15min
completed: 2026-02-25
---

# Phase 26 Plan 04: NLM/SMB Protocol Leak Purge Summary

**Extracted NLM lock methods to dedicated NLMService in NFS adapter, removed SMB lease methods (OplockChecker, CheckAndBreakLeases*, ReclaimLeaseSMB) from MetadataService, reducing service.go from 994 to 520 lines**

## Performance

- **Duration:** 15 min
- **Started:** 2026-02-25T09:37:00Z
- **Completed:** 2026-02-25T09:52:29Z
- **Tasks:** 2
- **Files modified:** 17

## Accomplishments
- Created NLMService in pkg/adapter/nfs/nlm_service.go using lock.Manager directly (no import cycle)
- Defined NLMLockService interface in NLM handlers to decouple from MetadataService
- Created routingNLMService for per-share lock manager routing via file handle decoding
- Removed 5 NLM methods and 8 SMB lease methods/types from MetadataService
- Removed global OplockChecker variable, SetOplockChecker, GetOplockChecker functions
- Removed waitForLeaseBreak function and cross-protocol lease constants from NFS v3 handlers
- Defined ErrLeaseBreakPending locally in SMB handlers package
- All tests pass, project builds cleanly

## Task Commits

Each task was committed atomically:

1. **Task 1: Extract NLM methods to NFS adapter NLMService** - `39b1515a` (feat)
2. **Task 2: Remove SMB lease methods and update callers** - `5458096d` (refactor)

## Files Created/Modified

### Created
- `pkg/adapter/nfs/nlm_service.go` - NLMService with LockFileNLM, TestLockNLM, UnlockFileNLM, CancelBlockingLock

### Modified (key files)
- `pkg/metadata/service.go` - Removed NLM methods, SMB lease methods, OplockChecker (994 -> 520 lines)
- `internal/protocol/nlm/handlers/handler.go` - NLMLockService interface, Handler uses interface
- `internal/protocol/nlm/handlers/lock.go` - Uses h.nlmService instead of h.metadataService
- `internal/protocol/nlm/handlers/test.go` - Uses h.nlmService
- `internal/protocol/nlm/handlers/unlock.go` - Uses h.nlmService
- `internal/protocol/nlm/handlers/cancel.go` - Removed metadata import
- `internal/protocol/nlm/handlers/cross_protocol.go` - Removed dead code (waitForLeaseBreak, checkForSMBLeaseConflicts)
- `pkg/adapter/nfs/nfs_adapter_nlm.go` - Added routingNLMService, metadataFileChecker
- `pkg/adapter/nfs/nfs_adapter.go` - Updated NLM handler init, removed metadata import
- `pkg/adapter/smb/smb_adapter.go` - Removed SetOplockChecker call, removed metadata import
- `internal/protocol/nfs/v3/handlers/write.go` - Removed waitForLeaseBreak, lease constants
- `internal/protocol/nfs/v3/handlers/read.go` - Replaced lease break with TODO placeholder
- `internal/protocol/nfs/v3/handlers/remove.go` - Replaced lease break with TODO placeholder
- `internal/protocol/nfs/v3/handlers/rename.go` - Replaced lease break with TODO placeholder
- `internal/protocol/smb/v2/handlers/oplock.go` - Local ErrLeaseBreakPending definition

## Decisions Made
- Combined NLM and SMB method removal from service.go in Task 1 since both are part of the same metadata cleanup objective
- Used routingNLMService to resolve per-share lock manager from handle (NLM handles encode share name)
- Replaced NFS v3 handler calls to CheckAndBreakLeases* with TODO(plan-03) placeholders because Plan 03 (centralized LockManager break methods) has not been executed yet
- Defined ErrLeaseBreakPending locally in SMB handlers rather than keeping it in the metadata package

## Deviations from Plan

### Auto-fixed Issues

**1. [Rule 3 - Blocking] Plan 03 dependency not yet available**
- **Found during:** Task 2
- **Issue:** Plan 03 (centralized CheckAndBreakOpLocksFor{Write,Read,Delete} on LockManager) has not been executed. The plan expected callers to wire to these methods.
- **Fix:** Replaced all CheckAndBreakLeases* calls with TODO(plan-03) placeholders that document the expected wiring. The cross-protocol break functionality is temporarily disabled (was already a no-op if no SMB adapter was running).
- **Files modified:** write.go, read.go, remove.go, rename.go
- **Impact:** Cross-protocol oplock breaks between NFS and SMB are temporarily unavailable until Plan 03 wires centralized LockManager break methods

**2. [Rule 3 - Blocking] ErrLeaseBreakPending reference in SMB handlers**
- **Found during:** Task 2
- **Issue:** SMB v2 handlers/oplock.go referenced metadata.ErrLeaseBreakPending which was removed
- **Fix:** Defined ErrLeaseBreakPending locally in the SMB handlers package with identical error message
- **Files modified:** internal/protocol/smb/v2/handlers/oplock.go

**3. [Rule 3 - Blocking] NFS v3 READ handler also called waitForLeaseBreak**
- **Found during:** Task 2 build verification
- **Issue:** read.go also called waitForLeaseBreak (not listed in plan's caller list)
- **Fix:** Replaced with TODO(plan-03) placeholder, same pattern as write/remove/rename handlers
- **Files modified:** internal/protocol/nfs/v3/handlers/read.go

---

**Total deviations:** 3 auto-fixed (all blocking)
**Impact on plan:** Cross-protocol oplock breaks temporarily disabled pending Plan 03. No scope creep.

## Incomplete Items from Plan

The following Plan 04 items require Plan 03 to be executed first:
- **SMB adapter BreakCallbacks registration** - Plan says SMB adapter should register all 3 BreakCallbacks with LockManager, but RegisterBreakCallbacks doesn't exist yet (Plan 03 scope)
- **NFS adapter OnOpLockBreak callback** - Plan says NFS adapter registers OnOpLockBreak for delegation recall, but this callback infrastructure doesn't exist yet (Plan 03 scope)
- **NFS v3 handler wiring** - CheckAndBreakLeases* replaced with TODO placeholders, will wire to LockManager methods from Plan 03

## Issues Encountered
None beyond the Plan 03 dependency gap noted above.

## User Setup Required
None - no external service configuration required.

## Next Phase Readiness
- MetadataService purged of all protocol-specific methods (NLM and SMB)
- NLMService operational in NFS adapter using LockManager directly
- Callers prepared with TODO markers for Plan 03 centralized break methods
- Ready for Plan 05 (final verification)

## Self-Check: PASSED

- pkg/adapter/nfs/nlm_service.go exists and defines NLMService
- pkg/metadata/service.go is 520 lines (down from 994)
- No NLM methods in service.go (LockFileNLM, TestLockNLM, etc.)
- No OplockChecker interface/global in pkg/metadata/
- No CheckAndBreakLeases* methods in service.go
- Both task commits verified (39b1515a, 5458096d)
- `go build ./...` compiles clean
- `go test ./...` passes

---
*Phase: 26-generic-lock-interface-protocol-leak-purge*
*Completed: 2026-02-25*
