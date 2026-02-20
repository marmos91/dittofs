---
phase: 16-nfsv4-1-types-and-constants
plan: 04
subsystem: protocol
tags: [nfsv4.1, xdr, callback, cb_sequence, cb_layoutrecall, cb_notify, pnfs]

# Dependency graph
requires:
  - phase: 16-01
    provides: "session_common.go types (SessionId4, ReferringCallTriple, Bitmap4)"
provides:
  - "All 10 NFSv4.1 callback operation XDR types (CB ops 5-14)"
  - "LockOwner4 type for lock owner identification"
  - "NotifyDeviceIdChange4 with CHANGE/DELETE union encoding"
  - "Notify4/NotifyEntry4 structures for directory change notifications"
affects: [phase-22-backchannel, phase-24-directory-delegations]

# Tech tracking
tech-stack:
  added: []
  patterns:
    - "Union-based XDR encoding for CB_LAYOUTRECALL (file/fsid/all discriminant)"
    - "Void-args pattern for CB_RECALLABLE_OBJ_AVAIL (empty struct with no-op Encode/Decode)"
    - "Conditional field encoding for CB_NOTIFY_DEVICEID (Immediate only present for CHANGE type)"

key-files:
  created:
    - internal/protocol/nfs/v4/types/cb_sequence.go
    - internal/protocol/nfs/v4/types/cb_layoutrecall.go
    - internal/protocol/nfs/v4/types/cb_notify.go
    - internal/protocol/nfs/v4/types/cb_push_deleg.go
    - internal/protocol/nfs/v4/types/cb_recall_any.go
    - internal/protocol/nfs/v4/types/cb_recall_slot.go
    - internal/protocol/nfs/v4/types/cb_wants_cancelled.go
    - internal/protocol/nfs/v4/types/cb_notify_lock.go
    - internal/protocol/nfs/v4/types/cb_notify_deviceid.go
  modified: []

key-decisions:
  - "CB_RECALLABLE_OBJ_AVAIL included in cb_recall_any.go since thematically related (both about resource reclaim)"
  - "CB_NOTIFY entries stored as raw opaque (NotifyEntry4.Data []byte) deferring full parsing to Phase 24"
  - "CB_PUSH_DELEG delegation stored as raw opaque []byte avoiding duplication of open_delegation4 encoding"
  - "LockOwner4 defined inline in cb_notify_lock.go (not yet used elsewhere)"

patterns-established:
  - "Void-args callback: empty struct with no-op Encode/Decode for void-args operations"
  - "Conditional union field: CB_NOTIFY_DEVICEID Immediate field only encoded when Type==CHANGE"

requirements-completed: [SESS-05]

# Metrics
duration: 6min
completed: 2026-02-20
---

# Phase 16 Plan 04: NFSv4.1 Callback Operation Types Summary

**All 10 NFSv4.1 callback operation XDR types (CB ops 5-14) with referring_call_lists, layout recall unions, and notification entries**

## Performance

- **Duration:** 6 min
- **Started:** 2026-02-20T15:50:57Z
- **Completed:** 2026-02-20T15:57:05Z
- **Tasks:** 2
- **Files modified:** 18

## Accomplishments
- CB_SEQUENCE with referring_call_lists for duplicate request detection across sessions
- CB_LAYOUTRECALL with file/fsid/all union recall types for layout segment recall
- CB_NOTIFY with variable-length notification entries (opaque for Phase 24 directory delegations)
- 7 additional CB operations covering delegation push, resource reclaim, slot recall, lock notification, and device ID changes
- 41 round-trip tests covering all operations including union variants

## Task Commits

Each task was committed atomically:

1. **Task 1: Create CB_SEQUENCE, CB_LAYOUTRECALL, and CB_NOTIFY types** - `e6358f27` (feat)
2. **Task 2: Create remaining 7 callback operation types** - `c60891ca` (feat)

## Files Created/Modified
- `internal/protocol/nfs/v4/types/cb_sequence.go` - CbSequenceArgs/Res with referring_call_lists
- `internal/protocol/nfs/v4/types/cb_sequence_test.go` - Round-trip tests including empty and populated referring calls
- `internal/protocol/nfs/v4/types/cb_layoutrecall.go` - CbLayoutRecallArgs/Res with file/fsid/all union
- `internal/protocol/nfs/v4/types/cb_layoutrecall_test.go` - Tests for all three recall type variants
- `internal/protocol/nfs/v4/types/cb_notify.go` - CbNotifyArgs/Res with Notify4/NotifyEntry4 structures
- `internal/protocol/nfs/v4/types/cb_notify_test.go` - Tests including multiple notification entries
- `internal/protocol/nfs/v4/types/cb_push_deleg.go` - CbPushDelegArgs/Res with opaque delegation
- `internal/protocol/nfs/v4/types/cb_push_deleg_test.go` - Round-trip tests
- `internal/protocol/nfs/v4/types/cb_recall_any.go` - CbRecallAnyArgs/Res + CbRecallableObjAvailArgs/Res
- `internal/protocol/nfs/v4/types/cb_recall_any_test.go` - Tests including void-args CB_RECALLABLE_OBJ_AVAIL
- `internal/protocol/nfs/v4/types/cb_recall_slot.go` - CbRecallSlotArgs/Res (single uint32 arg)
- `internal/protocol/nfs/v4/types/cb_recall_slot_test.go` - Round-trip tests
- `internal/protocol/nfs/v4/types/cb_wants_cancelled.go` - CbWantsCancelledArgs/Res with two bools
- `internal/protocol/nfs/v4/types/cb_wants_cancelled_test.go` - Tests including both-true variant
- `internal/protocol/nfs/v4/types/cb_notify_lock.go` - CbNotifyLockArgs/Res with LockOwner4
- `internal/protocol/nfs/v4/types/cb_notify_lock_test.go` - Tests including LockOwner4 round-trip
- `internal/protocol/nfs/v4/types/cb_notify_deviceid.go` - CbNotifyDeviceidArgs/Res with change/delete union
- `internal/protocol/nfs/v4/types/cb_notify_deviceid_test.go` - Tests including mixed change+delete arrays

## Decisions Made
- CB_RECALLABLE_OBJ_AVAIL placed in cb_recall_any.go alongside CB_RECALL_ANY (thematically related, both about resource reclaim)
- CB_NOTIFY entries stored as raw opaque bytes, deferring full sub-type parsing (notify_add4, notify_remove4) to Phase 24
- CB_PUSH_DELEG delegation stored as raw opaque to avoid duplicating open_delegation4 encoding from v4.0
- LockOwner4 type defined in cb_notify_lock.go (will be reused by lock operations in future phases)
- CB_NOTIFY_DEVICEID uses conditional encoding where Immediate bool only present for CHANGE type (not DELETE)

## Deviations from Plan

None - plan executed exactly as written.

## Issues Encountered

None.

## User Setup Required

None - no external service configuration required.

## Next Phase Readiness
- All 10 v4.1 callback operations complete with full XDR encode/decode
- Phase 22 (backchannel) can use these types to send CB_COMPOUND requests
- Phase 24 (directory delegations) can extend CB_NOTIFY with typed notification entry parsing
- Plan 16-05 (remaining v4.1 types or validation) is ready to proceed

## Self-Check: PASSED

All 18 created files verified present. Both task commits (e6358f27, c60891ca) verified in git log.

---
*Phase: 16-nfsv4-1-types-and-constants*
*Completed: 2026-02-20*
