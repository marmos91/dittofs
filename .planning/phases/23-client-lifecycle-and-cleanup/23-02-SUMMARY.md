---
phase: 23-client-lifecycle-and-cleanup
plan: 02
subsystem: nfs-protocol
tags: [nfsv4.1, handlers, compound, session-exempt, v40-rejection, xdr]

# Dependency graph
requires:
  - phase: 23-01
    provides: "StateManager methods (DestroyV41ClientID, FreeStateid, TestStateids, ReclaimComplete)"
provides:
  - "DESTROY_CLIENTID V41OpHandler with session-exempt dispatch"
  - "RECLAIM_COMPLETE V41OpHandler with per-client grace tracking"
  - "FREE_STATEID V41OpHandler with type-routed stateid cleanup"
  - "TEST_STATEID V41OpHandler with per-stateid status codes array"
  - "v4.0-only operation rejection in v4.1 COMPOUNDs (5 ops: SETCLIENTID, SETCLIENTID_CONFIRM, RENEW, OPEN_CONFIRM, RELEASE_LOCKOWNER)"
affects: [23-03, phase-24]

# Tech tracking
tech-stack:
  added: []
  patterns:
    - "v40OnlyOps map + consumeV40OnlyArgs for v4.0 rejection with XDR consumption"
    - "Session-exempt expansion: DESTROY_CLIENTID added alongside EXCHANGE_ID/CREATE_SESSION/DESTROY_SESSION/BIND_CONN_TO_SESSION"

key-files:
  created:
    - "internal/protocol/nfs/v4/handlers/destroy_clientid_handler.go"
    - "internal/protocol/nfs/v4/handlers/reclaim_complete_handler.go"
    - "internal/protocol/nfs/v4/handlers/free_stateid_handler.go"
    - "internal/protocol/nfs/v4/handlers/test_stateid_handler.go"
    - "internal/protocol/nfs/v4/handlers/destroy_clientid_handler_test.go"
    - "internal/protocol/nfs/v4/handlers/reclaim_complete_handler_test.go"
    - "internal/protocol/nfs/v4/handlers/free_stateid_handler_test.go"
    - "internal/protocol/nfs/v4/handlers/test_stateid_handler_test.go"
  modified:
    - "internal/protocol/nfs/v4/handlers/handler.go"
    - "internal/protocol/nfs/v4/handlers/compound.go"
    - "internal/protocol/nfs/v4/handlers/sequence_handler.go"
    - "internal/protocol/nfs/v4/handlers/compound_test.go"

key-decisions:
  - "DESTROY_CLIENTID added to session-exempt ops per RFC 8881 Section 18.50.3"
  - "v4.0-only ops consumed via consumeV40OnlyArgs to prevent XDR desync before returning NOTSUPP"
  - "v4.0 rejection check placed before v4.1/v4.0 dispatch table lookup in both dispatchV41 and dispatchV41Ops"

patterns-established:
  - "v40OnlyOps map pattern: package-level set for v4.0-only operation identification"
  - "consumeV40OnlyArgs: per-op XDR consumption using existing decode functions"

requirements-completed: [LIFE-01, LIFE-02, LIFE-03, LIFE-04, LIFE-05]

# Metrics
duration: 8min
completed: 2026-02-22
---

# Phase 23 Plan 02: NFSv4.1 Lifecycle Handlers Summary

**4 NFSv4.1 lifecycle handlers (DESTROY_CLIENTID, RECLAIM_COMPLETE, FREE_STATEID, TEST_STATEID) with v4.0-only operation rejection and comprehensive test coverage**

## Performance

- **Duration:** 8 min
- **Started:** 2026-02-22T11:52:56Z
- **Completed:** 2026-02-22T12:01:22Z
- **Tasks:** 2
- **Files modified:** 12

## Accomplishments
- 4 new V41OpHandler files implementing DESTROY_CLIENTID, RECLAIM_COMPLETE, FREE_STATEID, and TEST_STATEID
- All 4 stubs replaced in v41DispatchTable with real handler implementations
- DESTROY_CLIENTID added as session-exempt operation per RFC 8881
- v4.0-only operation rejection (5 ops) returns NFS4ERR_NOTSUPP in v4.1 COMPOUNDs with proper XDR arg consumption
- Comprehensive handler tests with race detection: 5 subtests for DESTROY_CLIENTID, 3 for RECLAIM_COMPLETE, 2 for FREE_STATEID, 3 for TEST_STATEID
- v4.0 rejection tests covering all 5 ops + v4.0 regression tests + session-exempt compound test

## Task Commits

Each task was committed atomically:

1. **Task 1: Implement 4 handler files, v4.0 rejection, and dispatch table updates** - `d968da6a` (feat)
2. **Task 2: Add handler tests and v4.0 rejection tests with race detection** - `1ef87c98` (test)

## Files Created/Modified
- `internal/protocol/nfs/v4/handlers/destroy_clientid_handler.go` - DESTROY_CLIENTID V41OpHandler (session-exempt, delegates to StateManager)
- `internal/protocol/nfs/v4/handlers/reclaim_complete_handler.go` - RECLAIM_COMPLETE V41OpHandler (SEQUENCE-required, per-client grace tracking)
- `internal/protocol/nfs/v4/handlers/free_stateid_handler.go` - FREE_STATEID V41OpHandler (SEQUENCE-required, type-routed cleanup)
- `internal/protocol/nfs/v4/handlers/test_stateid_handler.go` - TEST_STATEID V41OpHandler (per-stateid status codes, not fail-on-first)
- `internal/protocol/nfs/v4/handlers/handler.go` - Replaced 4 stubs with real handler registrations
- `internal/protocol/nfs/v4/handlers/compound.go` - Added v40OnlyOps map, consumeV40OnlyArgs, rejection checks in both dispatch paths
- `internal/protocol/nfs/v4/handlers/sequence_handler.go` - Added OP_DESTROY_CLIENTID to isSessionExemptOp
- `internal/protocol/nfs/v4/handlers/destroy_clientid_handler_test.go` - Tests: success, clientid_busy, stale, bad_xdr, session_exempt
- `internal/protocol/nfs/v4/handlers/reclaim_complete_handler_test.go` - Tests: success, complete_already, bad_xdr
- `internal/protocol/nfs/v4/handlers/free_stateid_handler_test.go` - Tests: bad_stateid, bad_xdr
- `internal/protocol/nfs/v4/handlers/test_stateid_handler_test.go` - Tests: empty, mixed, bad_xdr
- `internal/protocol/nfs/v4/handlers/compound_test.go` - v4.0 rejection tests, v4.0 regression tests, session-exempt test

## Decisions Made
- DESTROY_CLIENTID added to session-exempt ops per RFC 8881 Section 18.50.3 (can be only op in COMPOUND after last session destroyed)
- v4.0-only ops consumed via per-op XDR decoding in consumeV40OnlyArgs to prevent stream desync
- v4.0 rejection check placed before v4.1/v4.0 dispatch table lookup in both dispatchV41 (SEQUENCE path) and dispatchV41Ops (exempt path)
- TEST_STATEID always returns NFS4_OK overall with per-stateid error codes in the array (not fail-on-first)

## Deviations from Plan

None - plan executed exactly as written.

## Issues Encountered

None.

## User Setup Required

None - no external service configuration required.

## Next Phase Readiness
- All 4 lifecycle handlers complete and tested with real StateManager (no mocks)
- All stubs replaced in v41DispatchTable
- v4.0-only rejection protects v4.1 compounds from inappropriate v4.0 operations
- Ready for Phase 23 Plan 03 (metrics and integration)

---
*Phase: 23-client-lifecycle-and-cleanup*
*Completed: 2026-02-22*
