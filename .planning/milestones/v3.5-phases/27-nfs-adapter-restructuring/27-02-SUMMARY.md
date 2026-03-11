---
phase: 27-nfs-adapter-restructuring
plan: 02
subsystem: nfs-adapter
tags: [go, refactoring, package-structure, nfsv4.1, handler-extraction]

# Dependency graph
requires:
  - phase: 27-01
    provides: "Directory restructuring and consolidation (moved files to internal/adapter/nfs/)"
provides:
  - "Idiomatic file names in pkg/adapter/nfs/ (no nfs_ prefix)"
  - "v4.1-only handlers isolated in internal/adapter/nfs/v4/v41/handlers/ package"
  - "Deps struct pattern for v4.1 handler dependency injection"
  - "Closure wrapper pattern bridging v41DispatchTable to v41handlers functions"
affects: [27-03, 27-04, nfs-v4.1-handlers, handler-registration]

# Tech tracking
tech-stack:
  added: []
  patterns:
    - "v41handlers.Deps struct for v4.1 handler dependency injection"
    - "Closure wrappers in v41DispatchTable bridging Handler scope to standalone v41handlers functions"
    - "Exported helpers (EncodeStatusOnly, MapStateError) duplicated in v41handlers for package isolation"

key-files:
  created:
    - "internal/adapter/nfs/v4/v41/handlers/deps.go"
  modified:
    - "internal/adapter/nfs/v4/handlers/handler.go"
    - "internal/adapter/nfs/v4/handlers/compound.go"
    - "internal/adapter/nfs/v4/handlers/sequence_handler_test.go"

key-decisions:
  - "Tests kept in v4/handlers/ (not moved to v41/) because they test through ProcessCompound() using unexported helpers"
  - "v4.1 types not split (left in v4/types/) since they are shared across v4.0 and v4.1 operations"
  - "Closure wrapper pattern chosen over method delegation for v41DispatchTable registration"
  - "MapStateError duplicated with full error handling (ErrStaleClientID, ErrClientIDInUse) for correctness"

patterns-established:
  - "v41handlers.Deps pattern: standalone exported functions accept *Deps as first argument"
  - "Closure wrappers capture v41Deps and delegate to v41handlers.HandleXxx functions"

requirements-completed: [REF-03]

# Metrics
duration: 15min
completed: 2026-02-25
---

# Phase 27 Plan 02: File Renames and v4.1 Handler Extraction Summary

**Dropped nfs_ prefix from pkg/adapter/nfs/ files and extracted 11 v4.1-only handlers into v4/v41/handlers/ package with Deps-based dependency injection**

## Performance

- **Duration:** 15 min
- **Started:** 2026-02-25T14:40:00Z
- **Completed:** 2026-02-25T14:55:00Z
- **Tasks:** 2
- **Files modified:** 34

## Accomplishments
- All files in `pkg/adapter/nfs/` renamed to drop the `nfs_` prefix (adapter.go, connection.go, dispatch.go, etc.)
- Removed stale `.disabled` test file
- 11 v4.1-only handler files moved to `internal/adapter/nfs/v4/v41/handlers/` as package `v41handlers`
- Created `deps.go` with `Deps` struct, `EncodeStatusOnly`, and `MapStateError` for package-isolated shared dependencies
- Updated `handler.go` with v41handlers import, `v41Deps` field, and closure wrappers for all 11 v4.1 dispatch table entries
- Updated `compound.go` to use `v41handlers.IsSessionExemptOp` and `v41handlers.HandleSequenceOp`

## Task Commits

Each task was committed atomically:

1. **Task 1: Rename pkg/adapter/nfs/ files to drop nfs_ prefix** - `d94578e2` (refactor)
2. **Task 2: Split v4.1 handlers into v4/v41/handlers/ package** - `93298b97` (refactor)

## Files Created/Modified

### Task 1 (Renames)
- `pkg/adapter/nfs/adapter.go` - Renamed from nfs_adapter.go
- `pkg/adapter/nfs/connection.go` - Renamed from nfs_connection.go
- `pkg/adapter/nfs/dispatch.go` - Renamed from nfs_connection_dispatch.go
- `pkg/adapter/nfs/handlers.go` - Renamed from nfs_connection_handlers.go
- `pkg/adapter/nfs/reply.go` - Renamed from nfs_connection_reply.go
- `pkg/adapter/nfs/settings.go` - Renamed from nfs_adapter_settings.go
- `pkg/adapter/nfs/shutdown.go` - Renamed from nfs_adapter_shutdown.go
- `pkg/adapter/nfs/nlm.go` - Renamed from nfs_adapter_nlm.go
- `pkg/adapter/nfs/portmap.go` - Renamed from nfs_adapter_portmap.go
- `pkg/adapter/nfs/nfs_adapter_test.go.disabled` - Deleted

### Task 2 (v4.1 Handler Extraction)
- `internal/adapter/nfs/v4/v41/handlers/deps.go` - New: Deps struct, EncodeStatusOnly, MapStateError
- `internal/adapter/nfs/v4/v41/handlers/exchange_id.go` - Moved from exchange_id_handler.go
- `internal/adapter/nfs/v4/v41/handlers/create_session.go` - Moved from create_session_handler.go
- `internal/adapter/nfs/v4/v41/handlers/destroy_session.go` - Moved from destroy_session_handler.go
- `internal/adapter/nfs/v4/v41/handlers/destroy_clientid.go` - Moved from destroy_clientid_handler.go
- `internal/adapter/nfs/v4/v41/handlers/bind_conn_to_session.go` - Moved from bind_conn_to_session_handler.go
- `internal/adapter/nfs/v4/v41/handlers/backchannel_ctl.go` - Moved from backchannel_ctl_handler.go
- `internal/adapter/nfs/v4/v41/handlers/sequence.go` - Moved from sequence_handler.go
- `internal/adapter/nfs/v4/v41/handlers/reclaim_complete.go` - Moved from reclaim_complete_handler.go
- `internal/adapter/nfs/v4/v41/handlers/free_stateid.go` - Moved from free_stateid_handler.go
- `internal/adapter/nfs/v4/v41/handlers/get_dir_delegation.go` - Moved from get_dir_delegation_handler.go
- `internal/adapter/nfs/v4/v41/handlers/test_stateid.go` - Moved from test_stateid_handler.go
- `internal/adapter/nfs/v4/handlers/handler.go` - Added v41handlers import, v41Deps field, closure wrappers
- `internal/adapter/nfs/v4/handlers/compound.go` - Updated to use v41handlers.IsSessionExemptOp/HandleSequenceOp
- `internal/adapter/nfs/v4/handlers/sequence_handler_test.go` - Updated references to exported IsSessionExemptOp

## Decisions Made
- Tests kept in `v4/handlers/` rather than moved to `v41/handlers/` because all handler tests exercise through `ProcessCompound()` using unexported test helpers (`newTestHandler`, `buildCompoundArgsWithOps`, etc.)
- v4.1 types not split from `v4/types/` since types like `SessionId4`, `SequenceArgs` are referenced by both v4.0 dispatch infrastructure and v4.1 handlers
- Closure wrapper pattern chosen for v41DispatchTable: each entry captures `h.v41Deps` and delegates to the standalone `v41handlers.HandleXxx` function
- `MapStateError` duplicated with complete error handling (including `ErrStaleClientID` and `ErrClientIDInUse` sentinel errors) rather than a simplified version

## Deviations from Plan

### Auto-fixed Issues

**1. [Rule 1 - Bug] Incomplete MapStateError in deps.go**
- **Found during:** Task 2 (v4.1 handler extraction)
- **Issue:** Initial MapStateError only checked for *NFS4StateError type assertion, missing sentinel error handling (ErrStaleClientID, ErrClientIDInUse) that the original mapStateError in setclientid.go included
- **Fix:** Added switch statement with errors.Is checks for sentinel errors, matching the original function's behavior
- **Files modified:** internal/adapter/nfs/v4/v41/handlers/deps.go
- **Verification:** TestHandleCreateSession_UnknownClient now passes (was returning NFS4_OK instead of NFS4ERR_STALE_CLIENTID)
- **Committed in:** 93298b97 (Task 2 commit)

---

**Total deviations:** 1 auto-fixed (1 bug fix)
**Impact on plan:** Bug fix essential for correctness of error mapping in extracted handlers. No scope creep.

## Issues Encountered
None beyond the auto-fixed MapStateError issue.

## User Setup Required
None - no external service configuration required.

## Next Phase Readiness
- v4.1 handlers cleanly isolated, ready for Plan 04 (CLAUDE.md updates)
- Handler registration chain verified: handler.go imports v41handlers and registers all 11 operations
- All build and test suites pass

## Self-Check: PASSED

- All 12 v41/handlers/ files exist (11 handlers + deps.go)
- Both commits verified (d94578e2, 93298b97)
- No nfs_ prefix files remain in pkg/adapter/nfs/
- No .disabled files remain
- Build succeeds

---
*Phase: 27-nfs-adapter-restructuring*
*Completed: 2026-02-25*
