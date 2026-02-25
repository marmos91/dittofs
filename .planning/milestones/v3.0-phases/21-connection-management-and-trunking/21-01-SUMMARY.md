---
phase: 21-connection-management-and-trunking
plan: 01
subsystem: protocol
tags: [nfsv4.1, trunking, connection-binding, bind-conn-to-session, rfc8881]

# Dependency graph
requires:
  - phase: 19-session-lifecycle
    provides: "StateManager, sessions, CreateSession/DestroySession, session reaper"
  - phase: 20-sequence-and-compound-bifurcation
    provides: "SEQUENCE gating, v4.1 COMPOUND dispatch, session-exempt ops, slot-based replay"
provides:
  - "BoundConnection model with ConnectionDirection, ConnectionType, negotiateDirection"
  - "StateManager connection binding methods (BindConnToSession, UnbindConnection, draining)"
  - "ConnectionID plumbing from TCP accept through CompoundContext"
  - "BIND_CONN_TO_SESSION handler replacing stub in v41DispatchTable"
  - "Auto-bind on CREATE_SESSION (fore-channel)"
  - "Disconnect cleanup via UnbindConnection in deferred close handler"
  - "Connection draining check in dispatchV41 returning NFS4ERR_DELAY"
affects: [22-backchannel-multiplexing, 23-trunking-detection, 24-session-migration]

# Tech tracking
tech-stack:
  added: []
  patterns: [separate-rwmutex-for-connection-state, generous-direction-negotiation, auto-bind-on-session-create]

key-files:
  created:
    - internal/protocol/nfs/v4/state/connection.go
    - internal/protocol/nfs/v4/state/connection_test.go
    - internal/protocol/nfs/v4/handlers/bind_conn_to_session_handler.go
    - internal/protocol/nfs/v4/handlers/bind_conn_to_session_handler_test.go
  modified:
    - internal/protocol/nfs/v4/state/manager.go
    - internal/protocol/nfs/v4/types/types.go
    - internal/protocol/nfs/v4/handlers/handler.go
    - internal/protocol/nfs/v4/handlers/create_session_handler.go
    - internal/protocol/nfs/v4/handlers/compound.go
    - internal/protocol/nfs/v4/handlers/compound_test.go
    - pkg/adapter/nfs/nfs_adapter.go
    - pkg/adapter/nfs/nfs_connection.go
    - pkg/adapter/nfs/nfs_connection_handlers.go
    - internal/protocol/CLAUDE.md

key-decisions:
  - "Separate connMu RWMutex from sm.mu for connection state to reduce contention"
  - "Generous direction negotiation policy: FORE_OR_BOTH -> BOTH, BACK_OR_BOTH -> BOTH"
  - "Auto-bind on CREATE_SESSION is best-effort (errors logged, do not fail CREATE_SESSION)"
  - "Connection limit default 16 per session with NFS4ERR_RESOURCE enforcement"
  - "Fore-channel enforcement: reject binds leaving zero fore connections (NFS4ERR_INVAL)"

patterns-established:
  - "Connection ID plumbing: atomic counter at accept() -> NFSConnection.connectionID -> CompoundContext.ConnectionID"
  - "Lock ordering: sm.mu before connMu (enforced in destroySessionLocked)"
  - "Draining check after SEQUENCE in dispatchV41 (NFS4ERR_DELAY)"

requirements-completed: [BACK-02, TRUNK-01]

# Metrics
duration: 10min
completed: 2026-02-21
---

# Phase 21 Plan 01: Connection Binding Summary

**NFSv4.1 connection binding with BIND_CONN_TO_SESSION handler, auto-bind on CREATE_SESSION, direction negotiation, draining support, and 24+ tests**

## Performance

- **Duration:** 10 min
- **Started:** 2026-02-21T20:19:44Z
- **Completed:** 2026-02-21T20:29:45Z
- **Tasks:** 2
- **Files modified:** 14

## Accomplishments
- Connection binding model with BoundConnection, ConnectionDirection, ConnectionType, and generous direction negotiation
- StateManager connection state with separate connMu RWMutex, 10 connection methods, destroy/reaper cleanup
- ConnectionID plumbed from TCP accept through NFSConnection to CompoundContext
- BIND_CONN_TO_SESSION handler replacing stub, with auto-bind on CREATE_SESSION
- Draining check in dispatchV41 returning NFS4ERR_DELAY after SEQUENCE
- 24+ tests: 14 state-level unit tests + 7 handler tests + 3 compound integration tests

## Task Commits

Each task was committed atomically:

1. **Task 1: Connection binding model, StateManager methods, and connection ID plumbing** - `68b2fad3` (feat)
2. **Task 2: BIND_CONN_TO_SESSION handler, auto-bind on CREATE_SESSION, and handler integration tests** - `ca794f10` (feat)

## Files Created/Modified
- `internal/protocol/nfs/v4/state/connection.go` - BoundConnection, ConnectionDirection, ConnectionType, negotiateDirection, BindConnResult
- `internal/protocol/nfs/v4/state/connection_test.go` - 14 unit tests covering all connection binding scenarios
- `internal/protocol/nfs/v4/state/manager.go` - Added connMu, connection maps, 10 connection methods, destroy/reaper cleanup
- `internal/protocol/nfs/v4/types/types.go` - Added ConnectionID field to CompoundContext
- `internal/protocol/nfs/v4/handlers/bind_conn_to_session_handler.go` - BIND_CONN_TO_SESSION handler implementation
- `internal/protocol/nfs/v4/handlers/bind_conn_to_session_handler_test.go` - 7 handler unit tests
- `internal/protocol/nfs/v4/handlers/handler.go` - Replaced stub with real handler in v41DispatchTable
- `internal/protocol/nfs/v4/handlers/create_session_handler.go` - Auto-bind connection on successful CREATE_SESSION
- `internal/protocol/nfs/v4/handlers/compound.go` - Draining check after SEQUENCE validation
- `internal/protocol/nfs/v4/handlers/compound_test.go` - 3 compound integration tests (exempt op, auto-bind, multi-connection)
- `pkg/adapter/nfs/nfs_adapter.go` - nextConnID atomic counter, connection ID assignment, disconnect cleanup
- `pkg/adapter/nfs/nfs_connection.go` - connectionID field on NFSConnection
- `pkg/adapter/nfs/nfs_connection_handlers.go` - Thread ConnectionID into CompoundContext
- `internal/protocol/CLAUDE.md` - Connection Management section documenting conventions

## Decisions Made
- Used separate `connMu RWMutex` for connection state instead of the global `sm.mu` to reduce contention on connection lookups
- Generous direction negotiation policy grants maximum permissive direction (FORE_OR_BOTH -> BOTH)
- Auto-bind on CREATE_SESSION is best-effort: errors are logged at DEBUG level but do not fail CREATE_SESSION
- Lock ordering enforced: sm.mu acquired before connMu in destroySessionLocked, never reversed
- RDMA mode requests are accepted but always return UseConnInRDMAMode=false

## Deviations from Plan

### Auto-fixed Issues

**1. [Rule 1 - Bug] Fixed responseBytes variable ordering in compound.go**
- **Found during:** Task 1 (draining check in dispatchV41)
- **Issue:** Draining check referenced `responseBytes` variable before its declaration, causing compilation error
- **Fix:** Moved `var responseBytes []byte` and defer block before the draining check
- **Files modified:** internal/protocol/nfs/v4/handlers/compound.go
- **Verification:** `go build ./...` succeeds
- **Committed in:** 68b2fad3 (Task 1 commit)

---

**Total deviations:** 1 auto-fixed (1 bug)
**Impact on plan:** Simple variable ordering fix. No scope creep.

## Issues Encountered
None beyond the compilation fix noted in deviations.

## User Setup Required
None - no external service configuration required.

## Next Phase Readiness
- Connection binding infrastructure complete and tested
- Ready for backchannel multiplexing (Phase 22) which builds on connection bindings
- Session migration (Phase 24) can use connection binding state for transparent failover

---
*Phase: 21-connection-management-and-trunking*
*Completed: 2026-02-21*
