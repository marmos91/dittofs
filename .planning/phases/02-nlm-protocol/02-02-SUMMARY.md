---
phase: 02-nlm-protocol
plan: 02
subsystem: protocol
tags: [nlm, nfs, locking, rpc, xdr]

# Dependency graph
requires:
  - phase: 02-01
    provides: "NLM XDR types and encode/decode utilities"
  - phase: 01-locking-infrastructure
    provides: "Unified lock manager with EnhancedLock types"
provides:
  - "NLM procedure handlers (NULL, TEST, LOCK, UNLOCK, CANCEL)"
  - "NLM dispatch table routing procedures to handlers"
  - "MetadataService NLM methods for lock operations"
  - "NLM program routing in NFS adapter"
affects: ["02-03", "02-04", "03-nfsv4-locking"]

# Tech tracking
tech-stack:
  added: []
  patterns:
    - "NLM owner ID format: nlm:{caller_name}:{svid}:{oh_hex}"
    - "Handler context pattern for NLM procedures"

key-files:
  created:
    - "internal/protocol/nlm/handlers/handler.go"
    - "internal/protocol/nlm/handlers/context.go"
    - "internal/protocol/nlm/handlers/null.go"
    - "internal/protocol/nlm/handlers/test.go"
    - "internal/protocol/nlm/handlers/lock.go"
    - "internal/protocol/nlm/handlers/unlock.go"
    - "internal/protocol/nlm/handlers/cancel.go"
    - "internal/protocol/nlm/dispatch.go"
    - "internal/protocol/nlm/types/constants.go"
    - "internal/protocol/nlm/types/types.go"
  modified:
    - "internal/protocol/nfs/rpc/constants.go"
    - "pkg/metadata/service.go"
    - "pkg/adapter/nfs/nfs_adapter.go"
    - "pkg/adapter/nfs/nfs_connection.go"
    - "internal/protocol/nlm/xdr/decode.go"
    - "internal/protocol/nlm/xdr/encode.go"

key-decisions:
  - "Moved NLM types and constants to types subpackage to avoid import cycle"
  - "NLM handler initialized with MetadataService from runtime"
  - "Unlock of non-existent lock silently succeeds per NLM spec (idempotency)"
  - "TEST allowed during grace period per Phase 1 decision"

patterns-established:
  - "NLM owner ID construction: fmt.Sprintf('nlm:%s:%d:%s', callerName, svid, hex.EncodeToString(oh))"
  - "NLM handler context threading pattern matching NFS/Mount handlers"
  - "NLM dispatch table following NFS dispatch pattern"

# Metrics
duration: 11min
completed: 2026-02-05
---

# Phase 02 Plan 02: NLM Dispatcher and Synchronous Lock Operations Summary

**NLM procedure handlers integrated into NFS adapter with unified lock manager calls for non-blocking lock operations**

## Performance

- **Duration:** 11 min
- **Started:** 2026-02-05T10:00:22Z
- **Completed:** 2026-02-05T10:11:24Z
- **Tasks:** 3
- **Files created:** 10
- **Files modified:** 6

## Accomplishments

- NLM v4 procedure handlers for NULL, TEST, LOCK, UNLOCK, CANCEL operations
- NLM dispatch table routing RPC calls to handlers
- MetadataService NLM methods integrating with unified lock manager
- NLM program routing in NFS adapter (same port 12049)
- Package restructure to avoid import cycles (types subpackage)

## Task Commits

Each task was committed atomically:

1. **Task 1: Add NLM program constant and handlers structure** - `aa812fa` (feat)
2. **Task 2: Implement NLM procedure handlers** - `f62b3e5` (feat)
3. **Task 3: Integrate NLM dispatcher in NFS adapter** - `565efd8` (feat)

## Files Created/Modified

**Created:**
- `internal/protocol/nlm/handlers/context.go` - NLMHandlerContext with auth and client info
- `internal/protocol/nlm/handlers/handler.go` - Handler struct with MetadataService reference
- `internal/protocol/nlm/handlers/null.go` - NULL procedure (ping/health check)
- `internal/protocol/nlm/handlers/test.go` - TEST procedure (F_GETLK support)
- `internal/protocol/nlm/handlers/lock.go` - LOCK procedure (non-blocking acquire)
- `internal/protocol/nlm/handlers/unlock.go` - UNLOCK procedure (idempotent release)
- `internal/protocol/nlm/handlers/cancel.go` - CANCEL procedure (blocking queue stub)
- `internal/protocol/nlm/dispatch.go` - NLM dispatch table and handler wrappers
- `internal/protocol/nlm/types/constants.go` - NLM program/version/procedure constants (moved)
- `internal/protocol/nlm/types/types.go` - NLM XDR types (moved)

**Modified:**
- `internal/protocol/nfs/rpc/constants.go` - Added ProgramNLM and NLMVersion4
- `internal/protocol/nlm/xdr/decode.go` - Updated imports for types subpackage
- `internal/protocol/nlm/xdr/encode.go` - Updated imports for types subpackage
- `pkg/metadata/service.go` - Added LockFileNLM, TestLockNLM, UnlockFileNLM, CancelBlockingLock
- `pkg/adapter/nfs/nfs_adapter.go` - Added nlmHandler field, initialized in SetRuntime
- `pkg/adapter/nfs/nfs_connection.go` - Added ProgramNLM case and handleNLMProcedure method

## Decisions Made

1. **Package restructure to avoid import cycle**: Moved constants.go and types.go to `internal/protocol/nlm/types/` subpackage. This allows both nlm/handlers and nlm (dispatch) to import types without circular dependency.

2. **NLM handler initialization**: Handler created in SetRuntime() with MetadataService from runtime.GetMetadataService(), following pattern of NFS and Mount handlers.

3. **Idempotent unlock**: Per NLM specification and CONTEXT.md, unlock of non-existent lock silently succeeds (returns NLM4Granted) for retry safety.

4. **Cancel stub**: CancelBlockingLock is a stub returning success. Full blocking queue implementation deferred to Plan 02-03.

## Deviations from Plan

### Auto-fixed Issues

**1. [Rule 3 - Blocking] Restructured packages to avoid import cycle**
- **Found during:** Task 2 (Creating dispatch.go)
- **Issue:** Import cycle between nlm and nlm/handlers - handlers imported nlm for types, dispatch.go imported handlers
- **Fix:** Moved types and constants to nlm/types subpackage, updated all imports
- **Files modified:** Created nlm/types/constants.go and nlm/types/types.go, updated nlm/xdr/*.go and nlm/handlers/*.go
- **Verification:** `go build ./internal/protocol/nlm/...` succeeds
- **Committed in:** f62b3e5 (Task 2 commit)

---

**Total deviations:** 1 auto-fixed (1 blocking)
**Impact on plan:** Package restructure was necessary to resolve Go import cycle. Same functionality, different package layout.

## Issues Encountered

None - restructure resolved the import cycle cleanly.

## User Setup Required

None - no external service configuration required.

## Next Phase Readiness

- NLM handlers ready for blocking lock queue (Plan 02-03)
- CancelBlockingLock stub will be enhanced with blocking queue integration
- NSM (Network Status Monitor) integration ready for Plan 02-04
- All non-blocking lock operations functional

---
*Phase: 02-nlm-protocol*
*Completed: 2026-02-05*
