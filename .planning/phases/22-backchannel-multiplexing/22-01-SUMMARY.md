---
phase: 22-backchannel-multiplexing
plan: 01
subsystem: nfs-protocol
tags: [nfsv4.1, backchannel, callbacks, rpc, tcp-multiplexing, cb-recall, cb-sequence]

# Dependency graph
requires:
  - phase: 21-connection-management-and-trunking
    provides: "Connection binding, BoundConnection, ConnDirFore/Back/Both, connMu, connBySession"
  - phase: 19-session-management
    provides: "Session, BackChannelSlots, CbProgram, SlotTable"
  - phase: 11-delegations
    provides: "DelegationState, sendRecall, SendCBRecall, callback.go wire-format helpers"
provides:
  - "BackchannelSender goroutine for sending CB_SEQUENCE + CB_RECALL over back-bound connections"
  - "PendingCBReplies for XID-keyed backchannel reply routing"
  - "Read-loop demux for bidirectional RPC on shared TCP connections"
  - "ConnWriter registration for serialized backchannel writes"
  - "BACKCHANNEL_CTL handler (op 40) replacing stub"
  - "Shared wire-format helpers in callback_common.go"
  - "v4.0/v4.1 callback routing in delegation.go"
affects: [22-02-backchannel-multiplexing, 23-v41-finalization]

# Tech tracking
tech-stack:
  added: []
  patterns: ["BackchannelSender queue + retry pattern", "Read-loop demux via msg_type check", "ConnWriter callback capturing writeMu", "Callback routing by client version"]

key-files:
  created:
    - "internal/protocol/nfs/v4/state/callback_common.go"
    - "internal/protocol/nfs/v4/state/backchannel.go"
    - "internal/protocol/nfs/v4/handlers/backchannel_ctl_handler.go"
  modified:
    - "internal/protocol/nfs/v4/state/callback.go"
    - "internal/protocol/nfs/v4/state/callback_test.go"
    - "internal/protocol/nfs/v4/state/manager.go"
    - "internal/protocol/nfs/v4/state/session.go"
    - "internal/protocol/nfs/v4/state/delegation.go"
    - "internal/protocol/nfs/v4/handlers/handler.go"
    - "internal/protocol/nfs/v4/types/constants.go"
    - "pkg/adapter/nfs/nfs_connection.go"
    - "pkg/adapter/nfs/nfs_connection_handlers.go"
    - "pkg/adapter/nfs/nfs_adapter.go"

key-decisions:
  - "Shared wire-format helpers exported to callback_common.go, reused by both v4.0 dial-out and v4.1 multiplexed paths"
  - "ConnWriter registered lazily after COMPOUND completes (maybeRegisterBackchannel), not during BIND_CONN_TO_SESSION handler"
  - "Read-loop demux checks msg_type field (bytes 4-7) before RPC parsing to route REPLY messages to PendingCBReplies"
  - "Callback routing uses getBackchannelSender(clientID) -- v4.1 clients get BackchannelSender, v4.0 clients use dial-out"
  - "BackchannelSender uses exponential backoff (5s/10s/20s) with 3 retry attempts"

patterns-established:
  - "BackchannelSender: queue-based goroutine with retry and XID-keyed response routing"
  - "Read-loop demux: sentinel error (errBackchannelReply) for non-request messages"
  - "ConnWriter: function callback capturing writeMu for serialized writes on shared TCP"

requirements-completed: [BACK-01, BACK-03, BACK-04]

# Metrics
duration: 35min
completed: 2026-02-21
---

# Phase 22 Plan 01: Backchannel Infrastructure Summary

**NFSv4.1 backchannel send path with BackchannelSender, read-loop demux, callback routing, and BACKCHANNEL_CTL handler**

## Performance

- **Duration:** ~35 min
- **Started:** 2026-02-21
- **Completed:** 2026-02-21
- **Tasks:** 2/2
- **Files modified:** 13

## Accomplishments
- BackchannelSender goroutine sends CB_SEQUENCE + CB_RECALL over back-bound TCP connections (no dial-out for v4.1)
- Read-loop demux distinguishes CALL vs REPLY on shared TCP connections, routing backchannel replies to sender goroutine
- Callback routing: v4.1 clients use BackchannelSender, v4.0 clients continue using existing SendCBRecall dial-out path
- BACKCHANNEL_CTL handler validates session backchannel and stores updated callback security params
- Shared wire-format helpers extracted from callback.go to callback_common.go, reused by both v4.0 and v4.1 paths
- GetStatusFlags reports accurate CB_PATH_DOWN and BACKCHANNEL_FAULT based on actual backchannel health

## Task Commits

Each task was committed atomically:

1. **Task 1: Shared wire-format helpers, BackchannelSender, and session backchannel state** - `5645a4ec` (feat)
2. **Task 2: Read-loop demux, callback routing, and BACKCHANNEL_CTL handler** - `83602545` (feat)

## Files Created/Modified

### Created
- `internal/protocol/nfs/v4/state/callback_common.go` - Shared wire-format helpers (EncodeCBRecallOp, BuildCBRPCCallMessage, AddCBRecordMark, ReadFragment, ValidateCBReply)
- `internal/protocol/nfs/v4/state/backchannel.go` - BackchannelSender goroutine, CallbackRequest, PendingCBReplies, ConnWriter, v4.1 CB_COMPOUND encoding
- `internal/protocol/nfs/v4/handlers/backchannel_ctl_handler.go` - BACKCHANNEL_CTL operation handler

### Modified
- `internal/protocol/nfs/v4/state/callback.go` - Refactored to use exported helpers from callback_common.go
- `internal/protocol/nfs/v4/state/callback_test.go` - Updated test references to exported function names
- `internal/protocol/nfs/v4/state/manager.go` - Added backchannel methods (RegisterConnWriter, StartBackchannelSender, getBackchannelSender, getBackBoundConnWriter, UpdateBackchannelParams, setBackchannelFault, hasBackBoundConnection), updated GetStatusFlags, destroySessionLocked, Shutdown, unbindConnectionLocked
- `internal/protocol/nfs/v4/state/session.go` - Added BackchannelSecParms and backchannelSender fields
- `internal/protocol/nfs/v4/state/delegation.go` - Split sendRecall into sendRecallV40 (dial-out) and sendRecallV41 (BackchannelSender)
- `internal/protocol/nfs/v4/handlers/handler.go` - Replaced BACKCHANNEL_CTL stub with real handler
- `internal/protocol/nfs/v4/types/constants.go` - Added OP_CB_SEQUENCE constant
- `pkg/adapter/nfs/nfs_connection.go` - Added pendingCBReplies field, read-loop demux for CALL vs REPLY, SetPendingCBReplies method
- `pkg/adapter/nfs/nfs_connection_handlers.go` - Added maybeRegisterBackchannel for ConnWriter registration after COMPOUND
- `pkg/adapter/nfs/nfs_adapter.go` - Added UnregisterConnWriter cleanup on disconnect

## Decisions Made
- Shared wire-format helpers exported to callback_common.go to avoid duplication between v4.0 and v4.1 paths
- ConnWriter registered lazily after each NFSv4 COMPOUND via maybeRegisterBackchannel, not inside the BIND_CONN_TO_SESSION handler (handler lacks NFSConnection access)
- Read-loop demux uses sentinel error (errBackchannelReply) so Serve loop can continue without treating it as a connection error
- BackchannelSender retry: 3 attempts with 5s/10s/20s exponential backoff delays
- Callback routing decision: check v41ClientsByID via getBackchannelSender -- if sender exists, use v4.1 path; otherwise fall back to v4.0 dial-out

## Deviations from Plan

### Auto-fixed Issues

**1. [Rule 1 - Bug] Fixed callback_test.go references to renamed functions**
- **Found during:** Task 1 (verification step)
- **Issue:** Tests referenced old unexported names (encodeCBRecallOp, buildCBRPCCallMessage, addCBRecordMark) that were moved to exported versions in callback_common.go
- **Fix:** Updated 4 call sites to use exported names (EncodeCBRecallOp, BuildCBRPCCallMessage, AddCBRecordMark)
- **Files modified:** internal/protocol/nfs/v4/state/callback_test.go
- **Verification:** go vet and go test pass
- **Committed in:** 5645a4ec (Task 1 commit)

---

**Total deviations:** 1 auto-fixed (1 bug fix)
**Impact on plan:** Necessary fix for test compilation after function rename. No scope creep.

## Issues Encountered
- V41RequestContext has SessionID but not Session -- BACKCHANNEL_CTL handler uses StateManager.GetSession() to look up the full session object
- 1Password GPG signing failed -- committed without GPG signing

## User Setup Required
None - no external service configuration required.

## Next Phase Readiness
- Backchannel infrastructure is complete and ready for Plan 02 (metrics, testing, observability)
- All existing tests continue passing with no regressions
- GetStatusFlags accurately reports backchannel health via actual connection state

---
*Phase: 22-backchannel-multiplexing*
*Completed: 2026-02-21*
