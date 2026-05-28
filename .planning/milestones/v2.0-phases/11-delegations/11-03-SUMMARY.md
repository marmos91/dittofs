---
phase: 11-delegations
plan: "03"
subsystem: protocol
tags: [nfsv4, delegations, open, cb_recall, conflict-detection, state-management]

# Dependency graph
requires:
  - phase: 11-delegations/11-01
    provides: delegation state, GrantDelegation, ReturnDelegation, DelegationState type
  - phase: 11-delegations/11-02
    provides: SendCBRecall callback client for delegation recall
provides:
  - ShouldGrantDelegation policy engine for delegation grant decisions
  - CheckDelegationConflict with async CB_RECALL on conflict
  - EncodeDelegation for open_delegation4 wire format encoding
  - ValidateDelegationStateid for CLAIM_DELEGATE_CUR support
  - OPEN handler integration with delegation conflict check and grant
  - CLAIM_DELEGATE_CUR handler for delegation-based opens
  - DELEGPURGE handler (NFS4ERR_NOTSUPP)
affects: [11-delegations/11-04, nfsv4-handlers]

# Tech tracking
tech-stack:
  added: []
  patterns: [async-recall-pattern, delegation-conflict-policy, delegation-encoding]

key-files:
  created: []
  modified:
    - internal/protocol/nfs/v4/state/delegation.go
    - internal/protocol/nfs/v4/state/delegation_test.go
    - internal/protocol/nfs/v4/handlers/open.go
    - internal/protocol/nfs/v4/handlers/handler.go
    - internal/protocol/nfs/v4/handlers/stubs.go

key-decisions:
  - "Simple delegation policy: grant when exclusive access, callback available, no other clients"
  - "Async CB_RECALL via goroutine to avoid holding StateManager lock during TCP"
  - "NFS4ERR_DELAY returned on conflict to let client retry after delegation return"
  - "CLAIM_DELEGATE_PREV returns NFS4ERR_NOTSUPP (requires persistent delegation state)"
  - "DELEGPURGE returns NFS4ERR_NOTSUPP (no CLAIM_DELEGATE_PREV support)"

patterns-established:
  - "Delegation grant uses variadic param in encodeOpenResult for backward compatibility"
  - "Conflict detection marks RecallSent/RecallTime before launching async goroutine"
  - "sendRecall reads callback info under RLock then releases before network call"

# Metrics
duration: 6min
completed: 2026-02-14
---

# Phase 11 Plan 03: OPEN Delegation Integration Summary

**Delegation grant policy, conflict detection with async CB_RECALL, OPEN handler integration, CLAIM_DELEGATE_CUR support, and EncodeDelegation wire encoding**

## Performance

- **Duration:** 6 min
- **Started:** 2026-02-14T14:23:09Z
- **Completed:** 2026-02-14T14:29:16Z
- **Tasks:** 2
- **Files modified:** 5

## Accomplishments
- StateManager now provides complete delegation lifecycle: grant decision, conflict detection, async recall, stateid validation, and wire encoding
- OPEN handler integrates delegation conflict check (NFS4ERR_DELAY) and grant logic on CLAIM_NULL path
- CLAIM_DELEGATE_CUR handler validates delegation stateid and opens file using existing delegation
- 18 new tests covering all delegation policy branches, conflict scenarios, encoding, and validation

## Task Commits

Each task was committed atomically:

1. **Task 1: StateManager delegation grant, conflict, recall, encode methods** - `34f7a2b` (feat)
2. **Task 2: OPEN handler delegation integration, CLAIM_DELEGATE_CUR, DELEGPURGE, tests** - `8461890` (feat)

## Files Created/Modified
- `internal/protocol/nfs/v4/state/delegation.go` - Added ShouldGrantDelegation, CheckDelegationConflict, sendRecall, EncodeDelegation, ValidateDelegationStateid
- `internal/protocol/nfs/v4/state/delegation_test.go` - 18 new tests for grant policy, conflict, encoding, validation
- `internal/protocol/nfs/v4/handlers/open.go` - Delegation conflict check, grant logic in CLAIM_NULL, CLAIM_DELEGATE_CUR handler, variadic deleg param in encodeOpenResult
- `internal/protocol/nfs/v4/handlers/handler.go` - Registered OP_DELEGPURGE in dispatch table
- `internal/protocol/nfs/v4/handlers/stubs.go` - handleDelegPurge returning NFS4ERR_NOTSUPP

## Decisions Made
- **Simple delegation policy**: Grant when (1) client has callback address, (2) no other clients have opens, (3) no existing delegations from any client, (4) same client does not already hold a delegation. No heuristics or contention tracking -- keeps implementation deterministic and testable.
- **Async CB_RECALL**: sendRecall reads callback info under RLock, then releases the lock before the TCP network call. Prevents holding StateManager lock during potentially slow network operations (Pitfall 2 from research).
- **NFS4ERR_DELAY on conflict**: When another client's delegation conflicts, the OPEN handler returns NFS4ERR_DELAY rather than blocking, per RFC 7530 recommendation. The client will retry, giving the delegation holder time to process CB_RECALL.
- **CLAIM_DELEGATE_PREV returns NFS4ERR_NOTSUPP**: Reclaiming delegations after server restart requires persistent delegation state, which is out of scope. This is correct per RFC 7530 Section 10.2.1.

## Deviations from Plan

None - plan executed exactly as written.

## Issues Encountered
None

## User Setup Required
None - no external service configuration required.

## Next Phase Readiness
- Delegation lifecycle is complete: grant, conflict detection, recall, return
- Plan 11-04 (revocation timer, DELEGPURGE completeness) can proceed
- sendRecall currently logs warnings on failure; Plan 11-04 will add revocation timer handling

## Self-Check: PASSED

All files verified present:
- internal/protocol/nfs/v4/state/delegation.go (13098 bytes)
- internal/protocol/nfs/v4/state/delegation_test.go (28794 bytes)
- internal/protocol/nfs/v4/handlers/open.go (25734 bytes)
- internal/protocol/nfs/v4/handlers/handler.go (6383 bytes)
- internal/protocol/nfs/v4/handlers/stubs.go (6510 bytes)
- .planning/phases/11-delegations/11-03-SUMMARY.md (5304 bytes)

All commits verified:
- 34f7a2b: feat(11-03): add delegation grant, conflict detection, recall, and encoding methods
- 8461890: feat(11-03): integrate delegations into OPEN handler with conflict detection and tests

---
*Phase: 11-delegations*
*Completed: 2026-02-14*
