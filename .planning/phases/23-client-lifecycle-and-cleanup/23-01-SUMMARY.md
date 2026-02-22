---
phase: 23-client-lifecycle-and-cleanup
plan: 01
subsystem: state
tags: [nfsv4.1, state-management, destroy-clientid, free-stateid, test-stateid, grace-period, reclaim-complete]

# Dependency graph
requires:
  - phase: 22-backchannel-multiplexing
    provides: "BackchannelSender, purgeV41Client, connection management"
provides:
  - "DestroyV41ClientID method for graceful client teardown"
  - "FreeStateid for per-stateid release (lock/open/delegation)"
  - "TestStateids for batch stateid validation without lease renewal"
  - "GraceStatusInfo struct and Status()/ForceEnd() for grace API"
  - "Per-client RECLAIM_COMPLETE tracking with duplicate detection"
  - "GraceStatus/ForceEndGrace/ReclaimComplete delegation on StateManager"
affects: [23-02 handlers, 23-03 grace API/CLI, 25 integration testing]

# Tech tracking
tech-stack:
  added: []
  patterns:
    - "Stateid type routing via Other[0] byte tag (0x01=open, 0x02=lock, 0x03=deleg)"
    - "Grace period early-exit on all-clients-reclaimed pattern"
    - "Read-only state validation (TestStateids) with RLock only"

key-files:
  created: []
  modified:
    - internal/protocol/nfs/v4/state/v41_client.go
    - internal/protocol/nfs/v4/state/stateid.go
    - internal/protocol/nfs/v4/state/grace.go
    - internal/protocol/nfs/v4/state/manager.go
    - internal/protocol/nfs/v4/state/v41_client_test.go
    - internal/protocol/nfs/v4/state/stateid_test.go
    - internal/protocol/nfs/v4/state/grace_test.go

key-decisions:
  - "DestroyV41ClientID rejects NFS4ERR_CLIENTID_BUSY when sessions remain (strict RFC 8881)"
  - "FreeStateid uses type byte routing from Other[0] to dispatch to correct cleanup path"
  - "TestStateids uses RLock only -- no lease renewal side effects per RFC 8881"
  - "ReclaimComplete returns NFS4_OK outside grace period (not an error per RFC 8881)"
  - "GraceStatusInfo exposes remaining seconds for API/CLI countdown display"

patterns-established:
  - "Stateid type dispatch via Other[0] byte prefix"
  - "Per-client reclaim tracking with NFS4ERR_COMPLETE_ALREADY duplicate guard"
  - "Grace period force-end for admin API usage"

requirements-completed: [LIFE-01, LIFE-02, LIFE-03, LIFE-04]

# Metrics
duration: 12min
completed: 2026-02-22
---

# Phase 23 Plan 01: State Methods Summary

**DestroyV41ClientID, FreeStateid, TestStateids with type-byte dispatch, plus grace period enrichment (Status/ForceEnd/ReclaimComplete) with per-client tracking**

## Performance

- **Duration:** 12 min
- **Started:** 2026-02-22T11:35:37Z
- **Completed:** 2026-02-22T11:47:37Z
- **Tasks:** 2
- **Files modified:** 7

## Accomplishments
- DestroyV41ClientID with NFS4ERR_CLIENTID_BUSY guard when sessions remain and synchronous state purge
- FreeStateid with type-byte dispatch handling lock, open, and delegation stateids including NFS4ERR_LOCKS_HELD guard
- TestStateids batch validation with read-only semantics (no lease renewal) and per-stateid error codes
- Grace period enrichment: GraceStatusInfo struct, Status(), ForceEnd(), per-client ReclaimComplete with NFS4ERR_COMPLETE_ALREADY
- 27 new tests all passing with -race flag including concurrent destroy and free tests

## Task Commits

Each task was committed atomically:

1. **Task 1: Implement DestroyV41ClientID, FreeStateid, TestStateids, and grace period enrichment** - `6f74f1ad` (feat)
2. **Task 2: Add state method tests with race detection** - `2b9aa000` (test)

## Files Created/Modified
- `internal/protocol/nfs/v4/state/v41_client.go` - Added DestroyV41ClientID method
- `internal/protocol/nfs/v4/state/stateid.go` - Added FreeStateid, TestStateids, and helper methods (isSpecialOther, freeLockStateidLocked, freeOpenStateidLocked, freeDelegStateidLocked, testSingleStateid, testOpenStateid, testLockStateid, testDelegStateid)
- `internal/protocol/nfs/v4/state/grace.go` - Added GraceStatusInfo struct, Status(), ForceEnd(), ReclaimComplete() with per-client tracking, reclaimCompleted map, startedAt field
- `internal/protocol/nfs/v4/state/manager.go` - Added GraceStatus(), ForceEndGrace(), ReclaimComplete() delegation methods
- `internal/protocol/nfs/v4/state/v41_client_test.go` - Added TestDestroyV41ClientID (5 subtests) and TestDestroyV41ClientID_Concurrent
- `internal/protocol/nfs/v4/state/stateid_test.go` - Added TestFreeStateid (6 subtests), TestFreeStateid_Concurrent, TestTestStateids (5 subtests)
- `internal/protocol/nfs/v4/state/grace_test.go` - Added TestGraceStatus (3 subtests), TestForceEndGrace, TestForceEndGrace_StateManager, TestReclaimComplete (4 subtests), TestReclaimComplete_StateManager, TestGraceStatus_StateManager

## Decisions Made
- DestroyV41ClientID rejects with NFS4ERR_CLIENTID_BUSY when sessions remain (strict RFC 8881 compliance)
- FreeStateid uses type byte from Other[0] to route to correct cleanup path (0x01=open, 0x02=lock, 0x03=delegation)
- TestStateids uses RLock only with no lease renewal side effects per RFC 8881 Section 18.48
- ReclaimComplete returns NFS4_OK outside grace period (per RFC 8881, not an error)
- GraceStatusInfo exposes RemainingSeconds for API/CLI countdown display (needed by Plan 03)

## Deviations from Plan

### Auto-fixed Issues

**1. [Rule 1 - Bug] Fixed OpenStates slice removal in freeOpenStateidLocked**
- **Found during:** Task 1
- **Issue:** Used `delete()` on `openState.Owner.OpenStates` but it is a `[]*OpenState` slice, not a map
- **Fix:** Changed to slice iteration with `append(slice[:i], slice[i+1:]...)` removal pattern
- **Files modified:** internal/protocol/nfs/v4/state/stateid.go
- **Verification:** go build passes, tests confirm open state properly removed
- **Committed in:** 6f74f1ad (Task 1 commit)

**2. [Rule 1 - Bug] Fixed LockResult field name in tests**
- **Found during:** Task 2
- **Issue:** Used `lockResult.LockStateid` but the field is `lockResult.Stateid`
- **Fix:** Changed all references from `LockStateid` to `Stateid`
- **Files modified:** internal/protocol/nfs/v4/state/stateid_test.go
- **Verification:** Tests compile and pass
- **Committed in:** 2b9aa000 (Task 2 commit)

**3. [Rule 3 - Blocking] Added lock manager initialization in FreeStateid tests**
- **Found during:** Task 2
- **Issue:** `free_lock_stateid` and `free_open_with_locks_held` subtests failed because `LockNew()` requires a lock manager configured via `SetLockManager()`
- **Fix:** Added `lm := lock.NewManager()` and `sm.SetLockManager(lm)` in both subtests
- **Files modified:** internal/protocol/nfs/v4/state/stateid_test.go
- **Verification:** All FreeStateid tests pass with -race flag
- **Committed in:** 2b9aa000 (Task 2 commit)

---

**Total deviations:** 3 auto-fixed (2 bugs, 1 blocking)
**Impact on plan:** All auto-fixes necessary for correctness. No scope creep.

## Issues Encountered
None beyond the auto-fixed deviations above.

## User Setup Required
None - no external service configuration required.

## Next Phase Readiness
- All state methods ready for Plan 02 handler wiring (DESTROY_CLIENTID, RECLAIM_COMPLETE, FREE_STATEID, TEST_STATEID handlers)
- GraceStatus/ForceEndGrace ready for Plan 03 API/CLI endpoints
- Grace period manager delegation methods on StateManager ready for handler access

## Self-Check: PASSED

All 7 modified files verified on disk. Both task commits (6f74f1ad, 2b9aa000) found in git log. Summary file exists.

---
*Phase: 23-client-lifecycle-and-cleanup*
*Completed: 2026-02-22*
