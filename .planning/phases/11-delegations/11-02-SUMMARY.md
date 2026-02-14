---
phase: 11-delegations
plan: 02
subsystem: nfs-state
tags: [nfsv4, delegation, callback, cb_recall, cb_null, rpc, universal-address]

# Dependency graph
requires:
  - phase: 11-delegations
    provides: "DelegationState type, CallbackInfo struct, CB_RECALL/CB_GETATTR constants"
  - phase: 02-nlm-protocol
    provides: "NLM callback client pattern (buildRPCCallMessage, addRecordMark, readAndDiscardReply)"
provides:
  - "ParseUniversalAddr for IPv4/IPv6 uaddr to host:port conversion"
  - "CB_COMPOUND and CB_RECALL XDR encoding per RFC 7530"
  - "SendCBRecall for delegation recall over TCP"
  - "SendCBNull for callback path verification"
  - "readAndValidateCBReply with NFS4 status checking"
affects: [11-03, 11-04]

# Tech tracking
tech-stack:
  added: []
  patterns: ["NLM callback client pattern reused for NFSv4 CB_COMPOUND", "Universal address parsing from right for IPv6 safety", "Mock TCP server pattern for callback integration testing"]

key-files:
  created:
    - "internal/protocol/nfs/v4/state/callback.go"
    - "internal/protocol/nfs/v4/state/callback_test.go"
  modified: []

key-decisions:
  - "CB_NULL uses readAndDiscardCBReply (NLM pattern) while CB_RECALL uses readAndValidateCBReply with NFS4 status checking"
  - "Universal address parsing splits from the right using LastIndex to handle IPv6 colons correctly"
  - "5-second total timeout (CBCallbackTimeout) covers both dial and I/O, matching NLM pattern"
  - "callback_ident in CB_COMPOUND set to 0 (client identifies via program number from SETCLIENTID)"

patterns-established:
  - "Mock TCP server pattern: net.Listen(tcp, 127.0.0.1:0) + goroutine for accept/respond"
  - "NFSv4 callback follows same fresh-connection-per-callback pattern as NLM"

# Metrics
duration: 16min
completed: 2026-02-14
---

# Phase 11 Plan 02: NFSv4 Callback Client Summary

**NFSv4 callback RPC client with universal address parsing, CB_COMPOUND/CB_RECALL encoding, SendCBRecall/SendCBNull TCP delivery, and 28 tests with mock servers**

## Performance

- **Duration:** 16 min
- **Started:** 2026-02-14T13:41:10Z
- **Completed:** 2026-02-14T13:57:00Z
- **Tasks:** 2
- **Files modified:** 2

## Accomplishments
- ParseUniversalAddr handles IPv4 ("h1.h2.h3.h4.p1.p2") and IPv6 ("h1::h2.p1.p2") universal address formats per RFC 5665
- CB_COMPOUND and CB_RECALL XDR encoding per RFC 7530 wire format with stateid4, truncate bool, and file handle
- SendCBRecall creates TCP connection to client callback address, sends framed RPC CALL with CB_COMPOUND, validates NFS4 reply status
- SendCBNull verifies callback path with lightweight CB_NULL RPC call (procedure 0)
- RPC message building and record marking reuses NLM callback pattern exactly
- 28 tests covering address parsing (12), encoding (2), RPC building (4), integration (10) -- all passing with -race

## Task Commits

Each task was committed atomically:

1. **Task 1: Universal address parsing and CB_COMPOUND encoding** - `035e8cd` (feat)
2. **Task 2: Callback client tests** - `76d8455` (test)

## Files Created/Modified
- `internal/protocol/nfs/v4/state/callback.go` - ParseUniversalAddr, encodeCBCompound, encodeCBRecallOp, buildCBRPCCallMessage, addCBRecordMark, readAndValidateCBReply, SendCBRecall, SendCBNull (538 lines)
- `internal/protocol/nfs/v4/state/callback_test.go` - 28 tests: address parsing, XDR encoding, RPC building, mock TCP server integration (965 lines)

## Decisions Made
- CB_NULL uses readAndDiscardCBReply (simple read-and-discard, same as NLM pattern) while CB_RECALL uses readAndValidateCBReply which additionally parses the NFS4 status code from the CB_COMPOUND4res
- Universal address parsing uses strings.LastIndex to split from the right, correctly handling IPv6 addresses that contain colons
- 5-second total timeout (CBCallbackTimeout) encompasses both dial and I/O combined, matching the NLM callback decision
- callback_ident in CB_COMPOUND set to 0 since the client identifies the server via the callback program number from SETCLIENTID

## Deviations from Plan

### Auto-fixed Issues

**1. [Rule 1 - Bug] Removed dead code in test helper**
- **Found during:** Task 2 (go vet)
- **Issue:** uaddrFromListener helper function had unreachable code after a premature return
- **Fix:** Removed the dead function since makeCallbackInfoFromListener serves the same purpose
- **Files modified:** internal/protocol/nfs/v4/state/callback_test.go
- **Verification:** go vet passes cleanly
- **Committed in:** 76d8455 (Task 2 commit)

---

**Total deviations:** 1 auto-fixed (1 bug in test code)
**Impact on plan:** Trivial cleanup of test helper. No scope creep.

## Issues Encountered
None.

## User Setup Required
None - no external service configuration required.

## Next Phase Readiness
- Callback client ready for Plan 11-03 (conflict detection and recall triggering)
- SendCBRecall can be called from StateManager when delegation conflict detected
- SendCBNull available for SETCLIENTID_CONFIRM to verify callback path
- ParseUniversalAddr available for any future universal address handling needs

## Self-Check: PASSED

- callback.go: 538 lines (min 150) -- FOUND
- callback_test.go: 965 lines (min 200) -- FOUND
- Commit 035e8cd present in git log -- FOUND
- Commit 76d8455 present in git log -- FOUND
- Key links verified: CallbackInfo from client.go used in SendCBRecall/SendCBNull
- Key links verified: NLM callback pattern (buildRPCCallMessage, addRecordMark) replicated

---
*Phase: 11-delegations*
*Completed: 2026-02-14*
