---
phase: 05-cross-protocol-integration
plan: 03
subsystem: locking
tags: [cross-protocol, smb, nlm, leases, conflict-detection]

# Dependency graph
requires:
  - phase: 05-cross-protocol-integration/01
    provides: UnifiedLockView, LockStore query API, cross-protocol metrics
provides:
  - SMB lease denial when NLM byte-range locks exist
  - NLM conflict checking in RequestLease
  - STATUS_LOCK_NOT_GRANTED status code for NLM conflicts
  - Cross-protocol conflict logging and metrics
affects: []

# Tech tracking
tech-stack:
  added: []
  patterns:
    - NLM lock checking before SMB lease grant
    - Cross-protocol status code translation

key-files:
  created:
    - internal/protocol/smb/v2/handlers/cross_protocol.go
  modified:
    - internal/protocol/smb/v2/handlers/lease.go
    - internal/protocol/smb/v2/handlers/create.go

key-decisions:
  - "NLM locks checked BEFORE SMB lease conflicts (NFS explicit locks win over opportunistic leases)"
  - "STATUS_LOCK_NOT_GRANTED (0xC0000054) for byte-range lock conflicts, not STATUS_SHARING_VIOLATION"
  - "CREATE succeeds even when lease denied - only caching is affected"
  - "Cross-protocol conflicts logged at INFO level (working as designed)"

patterns-established:
  - "checkNLMLocksForLeaseConflict queries IsLease=false to filter byte-range locks"
  - "Write lease conflicts with ANY NLM lock (exclusive access required)"
  - "Read lease conflicts with exclusive NLM locks only"
  - "Handle-only lease does not conflict with NLM locks (about delete notification)"

# Metrics
duration: 5min
completed: 2026-02-05
---

# Phase 5 Plan 3: SMB-to-NFS Integration Summary

**SMB handlers check NLM locks and deny leases when conflicts exist, with appropriate status codes**

## Performance

- **Duration:** 5 min
- **Started:** 2026-02-05T19:37:30Z
- **Completed:** 2026-02-05T19:42:13Z
- **Tasks:** 3
- **Files modified:** 3

## Accomplishments

- SMB lease requests now check for NLM byte-range locks before granting
- Write lease denied when ANY NLM lock exists on the file
- Read lease denied when exclusive NLM lock exists
- Cross-protocol conflicts logged at INFO level with lock details
- Cross-protocol conflict metrics recorded for observability

## Task Commits

Each task was committed atomically:

1. **Task 1: Create SMB cross-protocol helpers** - `68d5605` (feat)
2. **Task 2: Add NLM lock checking to RequestLease** - `5b0dfb2` (feat)
3. **Task 3: Update SMB CREATE to handle NLM conflicts** - `d76464f` (feat)

## Files Created/Modified

- `internal/protocol/smb/v2/handlers/cross_protocol.go` - statusForNLMConflict, formatNLMLockInfo, checkNLMLocksForLeaseConflict
- `internal/protocol/smb/v2/handlers/lease.go` - NLM lock check in RequestLease before SMB lease conflict check
- `internal/protocol/smb/v2/handlers/create.go` - Lease denial handling with logging context

## Decisions Made

- **NLM check order:** NLM locks checked before SMB leases because NFS explicit locks win over opportunistic SMB leases
- **Status code:** STATUS_LOCK_NOT_GRANTED (0xC0000054) per MS-ERREF, not STATUS_SHARING_VIOLATION (that's for share mode conflicts)
- **CREATE success:** CREATE operation still succeeds when lease denied - only caching is affected, not file access
- **Handle lease:** Handle-only leases (no R or W) do not conflict with NLM locks since H is about delete/rename notification

## Deviations from Plan

None - plan executed exactly as written.

## Issues Encountered

None - all verification passed on first attempt.

## User Setup Required

None - no external service configuration required.

## Next Phase Readiness

- SMB handlers now respect NLM locks for cross-protocol consistency
- Plan 05-02 (NFS protocol integration) implements the reverse direction
- Phase 5 cross-protocol integration foundation is complete

---
*Phase: 05-cross-protocol-integration*
*Completed: 2026-02-05*
