---
phase: 09-state-management
plan: 01
subsystem: protocol
tags: [nfsv4, state-management, setclientid, rfc7530, client-identity]

# Dependency graph
requires:
  - phase: 06-nfsv4-protocol-foundation
    provides: "SETCLIENTID/SETCLIENTID_CONFIRM stubs, Handler struct, types/constants"
  - phase: 08-nfsv4-advanced-operations
    provides: "Complete NFSv4 handler infrastructure"
provides:
  - "StateManager central coordinator at internal/protocol/nfs/v4/state/"
  - "ClientRecord with five-case SETCLIENTID algorithm per RFC 7530 Section 9.1.1"
  - "Boot epoch + counter client ID generation (unique across restarts)"
  - "crypto/rand confirm verifier generation"
  - "SetClientID/ConfirmClientID/GetClient/RemoveClient API"
  - "Handler.StateManager field with backward-compatible constructor"
  - "V4ClientState.ClientID field for connection state tracking"
affects: [09-02, 09-03, 09-04, 10-lock-operations, 11-delegations]

# Tech tracking
tech-stack:
  added: []
  patterns: ["StateManager as central authority for all NFSv4 state", "Five-case SETCLIENTID algorithm with confirmed/unconfirmed record lifecycle", "Boot epoch + counter for restart-safe client ID generation"]

key-files:
  created:
    - "internal/protocol/nfs/v4/state/manager.go"
    - "internal/protocol/nfs/v4/state/client.go"
    - "internal/protocol/nfs/v4/state/client_test.go"
  modified:
    - "internal/protocol/nfs/v4/handlers/handler.go"
    - "internal/protocol/nfs/v4/handlers/setclientid.go"
    - "internal/protocol/nfs/v4/types/types.go"
    - "pkg/adapter/nfs/nfs_adapter.go"
    - "internal/protocol/nfs/v4/handlers/ops_test.go"

key-decisions:
  - "Variadic StateManager parameter in NewHandler for backward compatibility with all existing tests"
  - "Single RWMutex for all StateManager state (avoid deadlocks per research anti-pattern advice)"
  - "crypto/rand for confirm verifiers (not timestamps, per Pitfall 6)"
  - "OpenOwner placeholder struct in client.go (Plan 09-02 fills in)"
  - "Case 5 (re-SETCLIENTID) reuses confirmed client ID but creates new unconfirmed record"

patterns-established:
  - "StateManager as central authority: handlers call StateManager methods, never modify state directly"
  - "mapStateError function maps state package errors to NFS4 status codes"
  - "Client ID = (bootEpoch << 32) | atomicCounter for cross-restart uniqueness"

# Metrics
duration: 7min
completed: 2026-02-13
---

# Phase 9 Plan 01: Client ID Management Summary

**NFSv4 StateManager foundation with five-case SETCLIENTID algorithm, boot epoch client IDs, and crypto/rand confirm verifiers replacing Phase 6 stubs**

## Performance

- **Duration:** 7 min
- **Started:** 2026-02-13T22:14:24Z
- **Completed:** 2026-02-13T22:21:24Z
- **Tasks:** 2
- **Files modified:** 8

## Accomplishments
- Created `internal/protocol/nfs/v4/state/` package with StateManager as central NFSv4 state coordinator
- Implemented full five-case SETCLIENTID algorithm per RFC 7530 Section 9.1.1 with confirmed/unconfirmed record lifecycle
- Removed global `nextClientID` atomic counter from setclientid.go, replaced with StateManager
- 18 unit tests covering all five SETCLIENTID cases, CONFIRM validation, concurrency, and edge cases

## Task Commits

Each task was committed atomically:

1. **Task 1: Create state package with StateManager and ClientRecord** - `049486b` (feat)
2. **Task 2: Upgrade SETCLIENTID/SETCLIENTID_CONFIRM handlers and add tests** - `ae2b341` (feat)

## Files Created/Modified
- `internal/protocol/nfs/v4/state/manager.go` - StateManager with five-case SetClientID, ConfirmClientID, client ID generation
- `internal/protocol/nfs/v4/state/client.go` - ClientRecord, CallbackInfo, SetClientIDResult, OpenOwner placeholder, error types
- `internal/protocol/nfs/v4/state/client_test.go` - 18 tests covering all cases with race detection
- `internal/protocol/nfs/v4/handlers/handler.go` - Added StateManager field, variadic NewHandler constructor
- `internal/protocol/nfs/v4/handlers/setclientid.go` - Replaced stubs with StateManager delegation and error mapping
- `internal/protocol/nfs/v4/types/types.go` - Extended V4ClientState with ClientID field
- `pkg/adapter/nfs/nfs_adapter.go` - Creates StateManager during initialization
- `internal/protocol/nfs/v4/handlers/ops_test.go` - Updated SETCLIENTID_CONFIRM test for proper two-step flow

## Decisions Made
- **Variadic StateManager in NewHandler**: Used variadic parameter (`stateManager ...*state.StateManager`) to maintain backward compatibility with 50+ existing test call sites that pass `NewHandler(nil, pfs)`. If no StateManager is passed, a default one is created. This avoids a massive test refactor while providing clean production usage.
- **Single RWMutex**: All StateManager state protected by one lock per research recommendation to avoid deadlocks between interdependent lookups.
- **Case 5 creates unconfirmed record**: Re-SETCLIENTID (same verifier) creates a new unconfirmed record with the same client ID rather than modifying the confirmed record directly. This ensures the two-step SETCLIENTID -> CONFIRM flow is always honored.
- **crypto/rand for confirm verifiers**: Per Pitfall 6 from research, timestamps are predictable and could allow verifier guessing.

## Deviations from Plan

### Auto-fixed Issues

**1. [Rule 1 - Bug] Fixed TestSetClientIDConfirm for real confirm validation**
- **Found during:** Task 2 (handler upgrade)
- **Issue:** The old `TestSetClientIDConfirm` test used a hardcoded client ID of 42 and all-zero confirm verifier, which worked with the Phase 6 stub but fails correctly with real validation
- **Fix:** Updated test to do proper SETCLIENTID -> extract clientID and confirmVerifier -> SETCLIENTID_CONFIRM flow. Added `TestSetClientIDConfirm_StaleClientID` for the error case.
- **Files modified:** internal/protocol/nfs/v4/handlers/ops_test.go
- **Verification:** `go test -race ./internal/protocol/nfs/v4/handlers/` passes
- **Committed in:** ae2b341 (Task 2 commit)

---

**Total deviations:** 1 auto-fixed (1 bug fix)
**Impact on plan:** Necessary test update for correct behavior validation. No scope creep.

## Issues Encountered
None

## User Setup Required
None - no external service configuration required.

## Next Phase Readiness
- StateManager foundation ready for Plan 09-02 (stateid generation and open-owner tracking)
- OpenOwner placeholder struct ready to be extended
- Handler.StateManager field accessible from all operation handlers
- V4ClientState.ClientID ready for compound context propagation

---
*Phase: 09-state-management*
*Completed: 2026-02-13*
