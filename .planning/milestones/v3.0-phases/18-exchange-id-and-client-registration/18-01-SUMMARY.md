---
phase: 18-exchange-id-and-client-registration
plan: 01
subsystem: nfs
tags: [nfsv4.1, exchange-id, client-registration, state-management, rfc8881]

# Dependency graph
requires:
  - phase: 17-slot-table-session-data-structures
    provides: "Session struct, ChannelAttrs, v4.1 XDR types (ExchangeIdArgs/Res), v41DispatchTable stubs"
provides:
  - "V41ClientRecord struct for v4.1 client identity"
  - "ServerIdentity singleton for trunking detection"
  - "StateManager.ExchangeID() multi-case algorithm per RFC 8881"
  - "handleExchangeID wired into v41DispatchTable"
  - "ListV41Clients(), EvictV41Client(), ServerInfo() for REST API"
affects: [18-02, 19-create-session, 20-sequence-slot-tracking]

# Tech tracking
tech-stack:
  added: []
  patterns: [v4.1-handler-pattern, v41-client-separate-from-v40, server-identity-singleton]

key-files:
  created:
    - internal/protocol/nfs/v4/state/v41_client.go
    - internal/protocol/nfs/v4/state/v41_client_test.go
    - internal/protocol/nfs/v4/handlers/exchange_id_handler.go
    - internal/protocol/nfs/v4/handlers/exchange_id_handler_test.go
  modified:
    - internal/protocol/nfs/v4/state/manager.go
    - internal/protocol/nfs/v4/handlers/handler.go
    - internal/protocol/nfs/v4/handlers/compound_test.go

key-decisions:
  - "V41ClientRecord is fully separate from v4.0 ClientRecord (different struct, different maps)"
  - "SP4_MACH_CRED and SP4_SSV rejected with NFS4ERR_ENCR_ALG_UNSUPP before any state allocation (matches Linux nfsd)"
  - "ServerIdentity is a singleton created at StateManager init, consistent across all calls (TRUNK-02)"
  - "BuildDate variable set via ldflags at compile time, zero-time fallback"
  - "Owner key uses string(ownerID) for byte-exact map comparison"

patterns-established:
  - "V41OpHandler pattern: handler receives CompoundContext + V41RequestContext + io.Reader, returns CompoundResult"
  - "State layer + handler layer separation: handler decodes XDR, validates SP4, delegates to StateManager"
  - "Replacing v4.1 stubs: replace specific line in v41DispatchTable, update compound tests to use different stub op"

requirements-completed: [SESS-01, TRUNK-02]

# Metrics
duration: 18min
completed: 2026-02-20
---

# Phase 18 Plan 01: EXCHANGE_ID and Client Registration Summary

**NFSv4.1 EXCHANGE_ID handler with RFC 8881 multi-case client registration algorithm, SP4 validation, and ServerIdentity singleton for trunking detection**

## Performance

- **Duration:** 18 min
- **Started:** 2026-02-20T00:00:00Z
- **Completed:** 2026-02-20T00:18:00Z
- **Tasks:** 2
- **Files modified:** 7

## Accomplishments
- V41ClientRecord struct with full RFC 8881 multi-case algorithm (new, idempotent, reboot, supersede)
- ServerIdentity singleton with hostname-based server_owner for consistent trunking detection
- EXCHANGE_ID handler wired into v41DispatchTable replacing the stub
- SP4_MACH_CRED/SP4_SSV rejected before any state allocation (matches Linux nfsd behavior)
- 15+ state-layer unit tests and 6 handler integration tests, all passing with -race

## Task Commits

Each task was committed atomically:

1. **Task 1: V41ClientRecord, ServerIdentity, and ExchangeID on StateManager** - `f8624301` (feat)
2. **Task 2: EXCHANGE_ID handler and dispatch table wiring** - `d0acb886` (feat)

## Files Created/Modified
- `internal/protocol/nfs/v4/state/v41_client.go` - V41ClientRecord, ServerIdentity, ExchangeIDResult types; ExchangeID multi-case algorithm, helper methods
- `internal/protocol/nfs/v4/state/v41_client_test.go` - 15+ unit tests covering all RFC 8881 cases, concurrency, timing, server identity
- `internal/protocol/nfs/v4/state/manager.go` - Added v41ClientsByID/v41ClientsByOwner maps and serverIdentity field
- `internal/protocol/nfs/v4/handlers/exchange_id_handler.go` - handleExchangeID with SP4 validation, impl ID logging, StateManager delegation
- `internal/protocol/nfs/v4/handlers/exchange_id_handler_test.go` - 6 integration tests (success, SP4 rejected, bad XDR, idempotent, impl ID, multi-op)
- `internal/protocol/nfs/v4/handlers/handler.go` - Replaced EXCHANGE_ID stub with real handler in v41DispatchTable
- `internal/protocol/nfs/v4/handlers/compound_test.go` - Updated 2 existing tests to use CREATE_SESSION as stub representative; added encodeCreateSessionArgs helper

## Decisions Made
- V41ClientRecord is fully separate from v4.0 ClientRecord with dedicated maps (v41ClientsByID, v41ClientsByOwner)
- SP4 validation occurs before any state allocation, returning NFS4ERR_ENCR_ALG_UNSUPP (same as Linux nfsd)
- ServerIdentity created once at StateManager init with os.Hostname() fallback to "dittofs-unknown"
- BuildDate variable injected via ldflags with zero-time fallback for dev builds

## Deviations from Plan

### Auto-fixed Issues

**1. [Rule 1 - Bug] Updated existing compound tests that assumed EXCHANGE_ID was a stub**
- **Found during:** Task 2 (dispatch table wiring)
- **Issue:** Two existing tests (TestCompound_MinorVersion1_V41Op_NOTSUPP, TestCompound_V41_StubConsumesArgs) expected EXCHANGE_ID to return NFS4ERR_NOTSUPP (stub behavior), but the real handler now returns NFS4_OK
- **Fix:** Changed both tests to use OP_CREATE_SESSION as the representative stub operation; added encodeCreateSessionArgs() helper
- **Files modified:** internal/protocol/nfs/v4/handlers/compound_test.go
- **Verification:** All existing compound tests pass with -race
- **Committed in:** d0acb886 (Task 2 commit)

---

**Total deviations:** 1 auto-fixed (1 bug fix)
**Impact on plan:** Expected regression from replacing a stub with a real handler. Tests updated to preserve their original purpose using a different stub op.

## Issues Encountered
None

## User Setup Required
None - no external service configuration required.

## Next Phase Readiness
- ExchangeID handler ready for Plan 02 (REST API endpoints for client list/evict)
- V41ClientRecord.Confirmed field ready for Phase 19 (CREATE_SESSION sets it to true)
- ServerIdentity.ServerOwner ready for trunking detection (TRUNK-02 complete)
- ListV41Clients(), EvictV41Client(), ServerInfo() helper methods ready for Plan 02 REST API

---
*Phase: 18-exchange-id-and-client-registration*
*Completed: 2026-02-20*
