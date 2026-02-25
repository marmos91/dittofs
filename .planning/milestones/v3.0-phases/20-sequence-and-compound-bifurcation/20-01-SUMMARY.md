---
phase: 20-sequence-and-compound-bifurcation
plan: 01
subsystem: nfs-protocol
tags: [nfsv4.1, sequence, compound, slot-table, replay-cache, session]

# Dependency graph
requires:
  - phase: 19-session-lifecycle
    provides: "Session creation/destruction, slot table, V41ClientRecord"
  - phase: 18-exchange-id-and-client-registration
    provides: "EXCHANGE_ID handler, V41ClientRecord registration"
  - phase: 16-v41-stub-framework
    provides: "v41DispatchTable, V41OpHandler signature, COMPOUND bifurcation skeleton"
provides:
  - "SEQUENCE handler with session lookup, slot validation, lease renewal, status flags"
  - "COMPOUND dispatcher with SEQUENCE-first enforcement for v4.1"
  - "Replay cache returning byte-identical cached COMPOUND responses"
  - "SkipOwnerSeqid bypass for v4.1 operations (slot table replaces per-owner seqid)"
  - "Session-exempt op detection for EXCHANGE_ID, CREATE_SESSION, DESTROY_SESSION, BIND_CONN_TO_SESSION"
  - "RenewV41Lease and GetStatusFlags methods on StateManager"
affects: [21-v41-open-io, 22-backchannel, 23-v40-rejection, 24-reclaim-complete]

# Tech tracking
tech-stack:
  added: []
  patterns:
    - "SEQUENCE-gated v4.1 COMPOUND dispatch with exempt op bypass"
    - "Slot lifecycle via defer for guaranteed release on completion/error/panic"
    - "seqid=0 convention for v4.1 bypass of per-owner seqid validation"
    - "Replay cache at COMPOUND level (full response bytes cached in slot)"

key-files:
  created:
    - "internal/protocol/nfs/v4/handlers/sequence_handler.go"
    - "internal/protocol/nfs/v4/handlers/sequence_handler_test.go"
  modified:
    - "internal/protocol/nfs/v4/handlers/compound.go"
    - "internal/protocol/nfs/v4/handlers/handler.go"
    - "internal/protocol/nfs/v4/handlers/compound_test.go"
    - "internal/protocol/nfs/v4/types/types.go"
    - "internal/protocol/nfs/v4/state/manager.go"

key-decisions:
  - "seqid=0 sentinel for v4.1 bypass: safe because v4.0 seqids start at 1 and wrap from 0xFFFFFFFF to 1, never using 0"
  - "Replay cache at COMPOUND level: full XDR-encoded response bytes cached, returned byte-identical on duplicate slot+seqid"
  - "SEQUENCE at position > 0 returns NFS4ERR_SEQUENCE_POS via v41DispatchTable entry"
  - "Non-exempt ops without SEQUENCE return NFS4ERR_OP_NOT_IN_SESSION with zero results"
  - "GetStatusFlags reports CB_PATH_DOWN and BACKCHANNEL_FAULT (no backchannel yet), checked lease expiry and delegation revocation"

patterns-established:
  - "SEQUENCE-gated dispatch: every non-exempt v4.1 COMPOUND must start with SEQUENCE"
  - "Slot lifecycle via defer: CompleteSlotRequest always called, even on error/panic"
  - "dispatchV41Ops helper: shared dispatch loop for exempt ops and post-SEQUENCE ops"
  - "Test setup via createTestSession: EXCHANGE_ID + CREATE_SESSION for SEQUENCE test fixtures"

requirements-completed: [SESS-04, COEX-01, COEX-03]

# Metrics
duration: 25min
completed: 2026-02-21
---

# Phase 20 Plan 01: SEQUENCE and COMPOUND Bifurcation Summary

**NFSv4.1 SEQUENCE handler with slot-based exactly-once semantics, COMPOUND bifurcation enforcing SEQUENCE-first for non-exempt ops, and replay cache returning byte-identical cached responses**

## Performance

- **Duration:** 25 min
- **Started:** 2026-02-21T13:07:49Z
- **Completed:** 2026-02-21T13:33:27Z
- **Tasks:** 2
- **Files modified:** 7

## Accomplishments

- SEQUENCE handler validates session/slot/seqid and builds V41RequestContext for subsequent operations
- dispatchV41 enforces SEQUENCE as first operation for all non-exempt v4.1 COMPOUNDs
- Replay cache returns byte-identical cached COMPOUND responses on duplicate slot+seqid without re-execution
- Per-owner seqid validation is bypassed for v4.1 operations via seqid=0 convention (7 call sites updated)
- Exempt operations (EXCHANGE_ID, CREATE_SESSION, DESTROY_SESSION, BIND_CONN_TO_SESSION) work without SEQUENCE
- SEQUENCE implicitly renews v4.1 client lease and reports SEQ4_STATUS flags
- 15+ new test cases covering SEQUENCE validation edge cases and COMPOUND dispatch scenarios

## Task Commits

Each task was committed atomically:

1. **Task 1: SEQUENCE handler, StateManager lease/status methods, and seqid bypass flag** - `3439e506` (feat)
2. **Task 2: dispatchV41 SEQUENCE gating with replay cache and exempt op handling** - `61f79bea` (feat)

## Files Created/Modified

- `internal/protocol/nfs/v4/handlers/sequence_handler.go` - SEQUENCE handler (handleSequenceOp, isSessionExemptOp)
- `internal/protocol/nfs/v4/handlers/sequence_handler_test.go` - 15+ test cases for SEQUENCE validation and COMPOUND dispatch
- `internal/protocol/nfs/v4/handlers/compound.go` - Rewritten dispatchV41 with SEQUENCE gating, replay cache, exempt op bypass
- `internal/protocol/nfs/v4/handlers/handler.go` - SEQUENCE stub replaced with NFS4ERR_SEQUENCE_POS handler
- `internal/protocol/nfs/v4/handlers/compound_test.go` - Updated existing tests for SEQUENCE-gated behavior
- `internal/protocol/nfs/v4/types/types.go` - SkipOwnerSeqid field on CompoundContext
- `internal/protocol/nfs/v4/state/manager.go` - RenewV41Lease, GetStatusFlags, seqid=0 bypass in 7 ValidateSeqID sites

## Decisions Made

- **seqid=0 sentinel for v4.1 bypass:** v4.0 seqids never use 0 (start at 1, wrap from 0xFFFFFFFF to 1), making 0 a safe sentinel for "skip validation". This avoids adding new parameters to StateManager methods.
- **Replay cache at COMPOUND level:** Full XDR-encoded COMPOUND response bytes are cached in the slot. On replay, the cached bytes are returned directly (byte-identical) without any re-execution or re-encoding.
- **SEQUENCE at position > 0:** Returns NFS4ERR_SEQUENCE_POS via a dedicated handler in v41DispatchTable (not removed from table). This correctly consumes the SEQUENCE args to prevent XDR desync.
- **GetStatusFlags reports backchannel down:** Since no backchannel is implemented yet (Phase 22), CB_PATH_DOWN and BACKCHANNEL_FAULT flags are set. Phase 22 will clear them when backchannel is bound.
- **dispatchV41Ops helper:** Factored out the common v4.1 op dispatch loop to avoid code duplication between exempt op path and post-SEQUENCE path.

## Deviations from Plan

### Auto-fixed Issues

**1. [Rule 1 - Bug] Updated existing compound_test.go tests for SEQUENCE-gated behavior**
- **Found during:** Task 2 (COMPOUND dispatch rewrite)
- **Issue:** 5 existing tests sent non-exempt ops without SEQUENCE in v4.1 COMPOUNDs, which now correctly returns NFS4ERR_OP_NOT_IN_SESSION instead of the previously expected behavior
- **Fix:** Updated test assertions to expect NFS4ERR_OP_NOT_IN_SESSION for non-exempt ops without SEQUENCE
- **Files modified:** internal/protocol/nfs/v4/handlers/compound_test.go
- **Verification:** All tests pass with -race
- **Committed in:** 61f79bea (Task 2 commit)

---

**Total deviations:** 1 auto-fixed (1 bug fix - test expectations matched new correct behavior)
**Impact on plan:** Test updates are expected consequences of the dispatch rewrite. No scope creep.

## Issues Encountered

- Type mismatch in sequence_handler.go: `nfsStatus` inferred as `int` from `types.NFS4ERR_SEQ_MISORDERED` but needed `uint32`. Fixed by explicit cast: `uint32(types.NFS4ERR_SEQ_MISORDERED)`.

## User Setup Required

None - no external service configuration required.

## Next Phase Readiness

- SEQUENCE handler is fully operational, enabling all v4.1 operations in subsequent phases
- Phase 21 (v4.1 OPEN/IO) can use SEQUENCE + OPEN + READ/WRITE compounds
- Phase 22 (backchannel) will clear CB_PATH_DOWN/BACKCHANNEL_FAULT status flags
- Phase 23 (v4.0 rejection) can add SETCLIENTID/RENEW rejection in v4.1 compounds

## Self-Check: PASSED

- All 8 key files verified present on disk
- Both task commits (3439e506, 61f79bea) verified in git log
- Full test suite passes with -race detection
- go vet passes clean

---
*Phase: 20-sequence-and-compound-bifurcation*
*Completed: 2026-02-21*
