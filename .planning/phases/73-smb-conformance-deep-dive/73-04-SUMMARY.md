---
phase: 73-smb-conformance-deep-dive
plan: 04
subsystem: smb
tags: [smb, durable-handles, leases, oplock, reconnect, conformance]

requires:
  - phase: 73-01
    provides: "ChangeNotify fixes and stale watcher cleanup"
provides:
  - "DH V2 reconnect with DH2Q response context, LeaseState persistence, session mapping"
  - "DH V1 reconnect with DHnQ response context and volatile FileID regeneration"
  - "Lease self-break suppression via ExcludeLeaseKey in LockOwner"
  - "Stat-only open exemption for lease breaks"
  - "Post-conflict lease granting after break resolution"
  - "~26 fewer smbtorture known failures in DH and lease categories"
affects: [smb-conformance, durable-handles, leases]

tech-stack:
  added: []
  patterns:
    - "ReconnectResult struct for rich durable reconnect return values"
    - "ExcludeLeaseKey in LockOwner for lease-key-aware break exclusion"
    - "isStatOnlyOpen helper for FILE_READ_ATTRIBUTES access check"

key-files:
  modified:
    - internal/adapter/smb/v2/handlers/durable_context.go
    - internal/adapter/smb/v2/handlers/create.go
    - internal/adapter/smb/v2/handlers/handler.go
    - internal/adapter/smb/lease/manager.go
    - pkg/metadata/lock/leases.go
    - pkg/metadata/lock/types.go
    - pkg/metadata/lock/manager.go
    - test/smb-conformance/smbtorture/KNOWN_FAILURES.md

key-decisions:
  - "Store LeaseState in PersistedDurableHandle for accurate restoration on reconnect"
  - "Return DH2Q/DHnQ response context on successful reconnect per MS-SMB2 3.3.5.9.12/7"
  - "Use persisted LeaseState for re-request instead of always requesting RWH"
  - "Add ExcludeLeaseKey to LockOwner rather than modifying breakOpLocks signature"
  - "Grant lease after cross-key conflict break resolves rather than denying"
  - "Conservative KNOWN_FAILURES removal: only basic reopen/lease tests, kept complex scenarios"

patterns-established:
  - "ReconnectResult: wrap reconnect output with IsV2 flag and PersistedLease for context-aware response building"
  - "isStatOnlyOpen: centralized check for FILE_READ_ATTRIBUTES-only opens to exempt from lease breaks"

requirements-completed: [WPTS-03, WPTS-04]

duration: 24min
completed: 2026-03-24
---

# Phase 73 Plan 04: DH/Lease Fix Summary

**DH V1/V2 reconnect with proper response contexts and lease state persistence, plus lease state machine fixes for self-break suppression, stat-open exemption, and post-conflict granting -- ~26 smbtorture known failures removed**

## Performance

- **Duration:** 24 min
- **Started:** 2026-03-24T14:49:38Z
- **Completed:** 2026-03-24T15:13:56Z
- **Tasks:** 2
- **Files modified:** 11

## Accomplishments
- Fixed DH V2 and V1 reconnect to return proper DH2Q/DHnQ response context and persist/restore lease state
- Fixed lease state machine: self-break suppression, stat-open exemption, post-conflict lease granting
- Removed ~26 tests from smbtorture KNOWN_FAILURES (9 DH V1 + 11 DH V2 reopen + 7 lease basic tests)

## Task Commits

1. **Task 1: Fix DH V2 reconnect and DH V1 reopen** - `1c1a68bc` (feat)
2. **Task 2: Fix lease state machine and update smbtorture KNOWN_FAILURES** - `2c80cbbe` (feat)

## Files Created/Modified
- `internal/adapter/smb/v2/handlers/durable_context.go` - ReconnectResult type, LeaseState persistence, processV1/V2Reconnect return lease state
- `internal/adapter/smb/v2/handlers/durable_context_test.go` - Updated tests for ReconnectResult API
- `internal/adapter/smb/v2/handlers/create.go` - DH2Q/DHnQ response on reconnect, stat-open exemption, lease key exclusion
- `internal/adapter/smb/v2/handlers/handler.go` - Pass leaseState to buildPersistedDurableHandle
- `internal/adapter/smb/lease/manager.go` - Optional excludeOwner for break functions
- `pkg/metadata/lock/leases.go` - Post-conflict lease granting after break resolves
- `pkg/metadata/lock/leases_test.go` - Updated cross-key conflict test expectations
- `pkg/metadata/lock/types.go` - ExcludeLeaseKey field in LockOwner
- `pkg/metadata/lock/manager.go` - Lease-key-aware exclusion in breakOpLocks
- `test/smb-conformance/smbtorture/KNOWN_FAILURES.md` - Removed ~26 fixed DH/lease tests

## Decisions Made
- Store LeaseState in PersistedDurableHandle during disconnect to enable accurate lease restoration on reconnect (was always zero before, causing full RWH re-request)
- Return DH2Q/DHnQ response context on successful reconnect per MS-SMB2 3.3.5.9.12/7 (was missing entirely)
- Add ExcludeLeaseKey to LockOwner struct for lease-key-aware break exclusion rather than modifying breakOpLocks function signature (backward compatible via zero-value check)
- Grant lease after cross-key conflict break resolves rather than always denying (per MS-SMB2 3.3.5.9)
- Conservative KNOWN_FAILURES removal: only removed basic reopen and lease tests where the fix directly addresses the test's expectations, kept complex multi-client scenarios and lock+DH interaction tests

## Deviations from Plan

### Auto-fixed Issues

**1. [Rule 1 - Bug] buildPersistedDurableHandle was not persisting LeaseState**
- **Found during:** Task 1
- **Issue:** LeaseState field was never populated in buildPersistedDurableHandle, always stored as 0
- **Fix:** Added leaseState parameter and set it from LeaseManager.GetLeaseState before persisting
- **Files modified:** durable_context.go, handler.go
- **Committed in:** 1c1a68bc

**2. [Rule 1 - Bug] Cross-key conflict always denied new lease**
- **Found during:** Task 2
- **Issue:** After breaking a conflicting lease, requestLeaseImpl returned LeaseStateNone instead of re-checking and granting
- **Fix:** Added post-conflict re-check that grants the lease if no remaining conflicts
- **Files modified:** pkg/metadata/lock/leases.go, leases_test.go
- **Committed in:** 2c80cbbe

---

**Total deviations:** 2 auto-fixed (2 bugs)
**Impact on plan:** Both bugs were root causes of test failures. Fixing them was essential for correctness.

## Issues Encountered
- Unit tests for cross-key conflict took 35 seconds each due to WaitForBreakCompletion timeout (no real client to acknowledge). Acceptable for correctness -- real smbtorture tests have clients that acknowledge breaks quickly.

## Next Phase Readiness
- DH V2/V1 basic reconnect and lease basic operations should now pass smbtorture
- Complex scenarios (breaking states, lock+DH interaction, disconnected handle preservation/purge) remain as known failures for future work
- Lease V2 epoch tests remain as known failures (require multi-client break sequences)

---
*Phase: 73-smb-conformance-deep-dive*
*Completed: 2026-03-24*
