---
phase: 17-slot-table-session-data-structures
plan: 01
subsystem: protocol
tags: [nfsv4.1, slot-table, exactly-once-semantics, replay-detection, session]

# Dependency graph
requires:
  - phase: 16-nfsv4.1-types-and-constants
    provides: NFS4ERR_BADSLOT, NFS4ERR_SEQ_MISORDERED, NFS4ERR_RETRY_UNCACHED_REP, NFS4ERR_DELAY constants
provides:
  - SlotTable data structure with per-table mutex for NFSv4.1 EOS
  - ValidateSequence implementing RFC 8881 Section 2.10.6.1 algorithm
  - SequenceValidation enum (SeqNew, SeqRetry, SeqMisordered)
  - Dynamic slot adjustment via SetTargetHighestSlotID
affects: [17-02-session-data-structures, 19-create-session, 20-sequence-handler]

# Tech tracking
tech-stack:
  added: []
  patterns: [per-table-mutex, slot-based-EOS, cached-reply-bytes]

key-files:
  created:
    - internal/protocol/nfs/v4/state/slot_table.go
    - internal/protocol/nfs/v4/state/slot_table_test.go
  modified: []

key-decisions:
  - "Per-SlotTable mutex instead of global StateManager.mu for SEQUENCE hot path"
  - "CachedReply stores full XDR bytes (not status code) for complete replay"
  - "uint32 natural overflow for seqID wrap (v4.1 allows seqid=0, unlike v4.0)"
  - "SequenceValidation is a separate type from v4.0 SeqIDValidation to avoid semantic confusion"

patterns-established:
  - "SlotTable pattern: validate -> mark in-use -> process -> complete with optional cache"
  - "v4.1 seqid wraps through 0 (unlike v4.0 which skips 0)"

requirements-completed: [EOS-01, EOS-02, EOS-03]

# Metrics
duration: 3min
completed: 2026-02-20
---

# Phase 17 Plan 01: Slot Table Data Structure Summary

**SlotTable with RFC 8881 Section 2.10.6.1 sequence validation, per-slot cached reply storage, and dynamic slot count adjustment for NFSv4.1 exactly-once semantics**

## Performance

- **Duration:** 3 min
- **Started:** 2026-02-20T18:00:18Z
- **Completed:** 2026-02-20T18:03:30Z
- **Tasks:** 2
- **Files modified:** 2

## Accomplishments
- SlotTable data structure with Slot, SequenceValidation types and complete validation logic
- RFC 8881 Section 2.10.6.1 sequence validation: new request, retry, misordered, in-use, bad slot
- Per-table sync.Mutex avoids serializing all v4.1 requests on global StateManager.mu
- 15 test functions covering all validation paths including uint32 seqID wraparound and concurrent access

## Task Commits

Each task was committed atomically:

1. **Task 1: SlotTable and Slot structs with validation logic** - `b5f5de37` (feat)
2. **Task 2: Comprehensive slot table unit tests** - `28beb909` (test)

## Files Created/Modified
- `internal/protocol/nfs/v4/state/slot_table.go` - SlotTable, Slot, SequenceValidation types; NewSlotTable, ValidateSequence, MarkSlotInUse, CompleteSlotRequest, SetTargetHighestSlotID, getters
- `internal/protocol/nfs/v4/state/slot_table_test.go` - 15 test functions covering creation, validation paths, wraparound, caching, concurrency

## Decisions Made
- Per-SlotTable mutex (sync.Mutex) instead of using StateManager's global RWMutex -- SEQUENCE is the hottest path in v4.1
- SequenceValidation is a new type separate from v4.0's SeqIDValidation -- different semantics (initial=0, wrap through 0)
- CachedReply stores full XDR-encoded COMPOUND4res bytes, not just status -- needed for complete replay
- CompleteSlotRequest copies reply bytes to avoid holding caller buffer references

## Deviations from Plan

None - plan executed exactly as written.

## Issues Encountered
None

## User Setup Required
None - no external service configuration required.

## Next Phase Readiness
- SlotTable ready for use by Phase 17 Plan 02 (Session data structure)
- Session will own a SlotTable per channel (fore/back)
- ValidateSequence API designed for Phase 20 SEQUENCE handler integration

## Self-Check: PASSED

- [x] `internal/protocol/nfs/v4/state/slot_table.go` exists
- [x] `internal/protocol/nfs/v4/state/slot_table_test.go` exists
- [x] Commit `b5f5de37` exists (Task 1)
- [x] Commit `28beb909` exists (Task 2)
- [x] All tests pass with `-race` flag
- [x] `go vet` clean

---
*Phase: 17-slot-table-session-data-structures*
*Completed: 2026-02-20*
