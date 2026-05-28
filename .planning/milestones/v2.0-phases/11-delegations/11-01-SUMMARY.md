---
phase: 11-delegations
plan: 01
subsystem: nfs-state
tags: [nfsv4, delegation, stateid, state-management, callback, lease]

# Dependency graph
requires:
  - phase: 09-state-management
    provides: "StateManager, stateid generation, lease management, onLeaseExpired cascade"
  - phase: 10-nfsv4-locking
    provides: "LockState pattern, lockStateByOther map, lease expiry lock cleanup"
provides:
  - "DelegationState type with stateid, client tracking, recall/revoke flags"
  - "delegByOther and delegByFile maps in StateManager"
  - "GrantDelegation, ReturnDelegation, GetDelegationsForFile methods"
  - "Delegation cleanup in onLeaseExpired cascade"
  - "countOpensOnFile helper for grant decision logic"
  - "DELEGRETURN handler registered in COMPOUND dispatch"
  - "CB_RECALL, CB_GETATTR, ACE, space limit constants"
affects: [11-02, 11-03, 11-04]

# Tech tracking
tech-stack:
  added: []
  patterns: ["DelegationState follows OpenState/LockState lifecycle pattern", "delegByFile for O(1) conflict detection", "Idempotent DELEGRETURN per RFC 7530 Pitfall 3"]

key-files:
  created:
    - "internal/protocol/nfs/v4/state/delegation.go"
    - "internal/protocol/nfs/v4/state/delegation_test.go"
    - "internal/protocol/nfs/v4/handlers/delegreturn.go"
    - "internal/protocol/nfs/v4/handlers/delegreturn_test.go"
  modified:
    - "internal/protocol/nfs/v4/state/manager.go"
    - "internal/protocol/nfs/v4/handlers/handler.go"
    - "internal/protocol/nfs/v4/types/constants.go"

key-decisions:
  - "Idempotent DELEGRETURN: current-epoch but not-found returns NFS4_OK (not BAD_STATEID) per Pitfall 3"
  - "delegByFile keyed by string(fileHandle) for O(1) conflict lookup"
  - "countOpensOnFile private helper scans openStateByOther (used by future grant decisions)"
  - "Delegation cleanup added AFTER lock/open cleanup in onLeaseExpired for consistent ordering"

patterns-established:
  - "DelegationState lifecycle mirrors OpenState/LockState: grant -> track -> return/revoke"
  - "Dual-map indexing (delegByOther for stateid lookup, delegByFile for conflict queries)"

# Metrics
duration: 9min
completed: 2026-02-14
---

# Phase 11 Plan 01: Delegation State Tracking and DELEGRETURN Summary

**DelegationState type with dual-map StateManager tracking, idempotent DELEGRETURN handler, lease expiry cascade cleanup, and 20 tests with race detection**

## Performance

- **Duration:** 9 min
- **Started:** 2026-02-14T13:20:24Z
- **Completed:** 2026-02-14T13:29:00Z
- **Tasks:** 2
- **Files modified:** 7

## Accomplishments
- DelegationState struct with stateid (type tag 0x03), client tracking, recall/revoke flags
- StateManager dual-map delegation tracking (delegByOther + delegByFile) with initialization
- GrantDelegation, ReturnDelegation (idempotent), GetDelegationsForFile, countOpensOnFile methods
- onLeaseExpired delegation cleanup cascade for expired clients
- DELEGRETURN handler registered in COMPOUND dispatch table
- CB_RECALL, CB_GETATTR, ACE4, space limit constants added to types package
- 14 state-level tests + 6 handler-level tests, all passing with -race

## Task Commits

Each task was committed atomically:

1. **Task 1: DelegationState type and StateManager delegation methods** - `7ff627d` (feat)
2. **Task 2: DELEGRETURN handler and unit tests** - `6f6f4cd` (test)

## Files Created/Modified
- `internal/protocol/nfs/v4/state/delegation.go` - DelegationState type, GrantDelegation, ReturnDelegation, GetDelegationsForFile, countOpensOnFile
- `internal/protocol/nfs/v4/state/delegation_test.go` - 14 delegation state lifecycle tests
- `internal/protocol/nfs/v4/state/manager.go` - delegByOther/delegByFile maps, initialization, lease expiry cleanup
- `internal/protocol/nfs/v4/handlers/delegreturn.go` - DELEGRETURN handler implementation
- `internal/protocol/nfs/v4/handlers/delegreturn_test.go` - 6 DELEGRETURN handler tests
- `internal/protocol/nfs/v4/handlers/handler.go` - OP_DELEGRETURN dispatch registration
- `internal/protocol/nfs/v4/types/constants.go` - CB_RECALL, CB_GETATTR, ACE4, space limit constants

## Decisions Made
- Idempotent DELEGRETURN: returning an already-returned delegation with current boot epoch returns NFS4_OK (not NFS4ERR_BAD_STATEID) per Pitfall 3 from research -- handles CB_RECALL/DELEGRETURN race
- delegByFile keyed by string(fileHandle) for direct O(1) conflict detection queries
- countOpensOnFile is a private helper that scans openStateByOther (adequate for current scale; used by future grant decisions in Plan 11-03)
- Delegation cleanup in onLeaseExpired is placed AFTER lock/open cleanup for consistent cascading order

## Deviations from Plan

None - plan executed exactly as written.

## Issues Encountered
- TestLeaseExpiry_MultipleClients initially used too-short lease (50ms) causing client B to also expire during the renewal loop. Fixed by increasing lease to 100ms with more aggressive renewal timing. [Rule 1 - Bug in test timing, fixed inline]

## User Setup Required

None - no external service configuration required.

## Next Phase Readiness
- Delegation state model fully established, ready for Plan 11-02 (callback channel)
- GrantDelegation and GetDelegationsForFile ready for Plan 11-03 (conflict detection)
- countOpensOnFile helper ready for grant decision logic in Plan 11-03
- Constants for CB_RECALL, ACE4, space limits ready for Plans 11-02 through 11-04

## Self-Check: PASSED

- All 4 created files exist on disk
- Commits 7ff627d and 6f6f4cd present in git log
- delegation.go: 180 lines (min 80)
- delegation_test.go: 422 lines (min 150)
- delegreturn.go: 73 lines (min 30)
- Key links verified: delegByOther/delegByFile in manager.go, ReturnDelegation in delegreturn.go

---
*Phase: 11-delegations*
*Completed: 2026-02-14*
