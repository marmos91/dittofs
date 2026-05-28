---
phase: 38-durable-handles
plan: 02
subsystem: smb-handlers
tags: [smb3, durable-handles, create-context, reconnect, wire-format, tdd]

# Dependency graph
requires: [38-01]
provides:
  - ProcessDurableHandleContext for V1/V2 grant with oplock/timeout logic
  - ProcessDurableReconnectContext with 14+ validation checks and NTSTATUS codes
  - ProcessAppInstanceId for Hyper-V failover collision handling
  - Wire format decode/encode for DHnQ, DHnC, DH2Q, DH2C, AppInstanceId
  - CREATE handler integration (reconnect early-exit + grant at Step 8c)
  - closeFilesWithFilter durable persistence on disconnect
affects: [38-03, phase-39]

# Tech tracking
tech-stack:
  added: []
  patterns: [CREATE context processing following lease_context.go pattern, session key SHA-256 for reconnect security]

key-files:
  created:
    - internal/adapter/smb/v2/handlers/durable_context.go
    - internal/adapter/smb/v2/handlers/durable_context_test.go
  modified:
    - internal/adapter/smb/v2/handlers/handler.go
    - internal/adapter/smb/v2/handlers/create.go

key-decisions:
  - "V2 (DH2Q) takes precedence over V1 (DHnQ) when both present per MS-SMB2"
  - "V1 requires batch oplock (0x09) for grant; V2 has no oplock requirement"
  - "Reconnect validation returns specific NTSTATUS codes per check (ACCESS_DENIED, OBJECT_NAME_NOT_FOUND, INVALID_PARAMETER)"
  - "Session key hash computed via SHA-256 of signing key for reconnect security"
  - "Reconnect early-exit in CREATE handler (Step 4b) avoids unnecessary file operations"
  - "DurableTimeoutMs defaults to 60000 (60 seconds) in handler constructor"

patterns-established:
  - "Durable handle context processing mirrors lease_context.go FindCreateContext pattern"
  - "Reconnect skips file creation path entirely (early return after Step 4)"

requirements-completed: [DH-01, DH-02, DH-04]

# Metrics
duration: 16min
completed: 2026-03-02
---

# Phase 38 Plan 02: Durable Handle CREATE Context Processing Summary

**V1/V2 durable handle grant and reconnect with 14+ validation checks, App Instance ID collision handling, and CREATE handler integration**

## Performance

- **Duration:** 16 min
- **Started:** 2026-03-02T14:17:05Z
- **Completed:** 2026-03-02T14:33:05Z
- **Tasks:** 1 (TDD)
- **Files modified:** 4

## Accomplishments
- Implemented wire format decode/encode for all 5 context types (DHnQ, DHnC, DH2Q, DH2C, AppInstanceId)
- ProcessDurableHandleContext correctly grants V1 (batch oplock required) and V2 (CreateGuid + timeout) durability
- ProcessDurableReconnectContext validates share name, path, username, session key hash, expiry, file existence with specific NTSTATUS codes per check
- ProcessAppInstanceId force-closes old handles with same AppInstanceId (Hyper-V failover pattern)
- closeFilesWithFilter persists durable handles to DurableHandleStore instead of closing them
- CREATE handler integrates reconnect early-exit (Step 4b) and grant processing (Step 8c)
- computeSessionKeyHash derives SHA-256 from session signing key for reconnect validation
- Handler defaults DurableTimeoutMs to 60000ms (60 seconds)
- 26 unit tests covering all wire formats, grant scenarios, reconnect validations, and AppInstanceId

## Task Commits

Each task was committed atomically (TDD: test first, then implementation):

1. **Task 1 (RED):** Tests for durable handle context processing - `573970d2` (test)
2. **Task 1 (GREEN):** Implementation with CREATE handler integration - `089926a4` (feat)

## Files Created/Modified
- `internal/adapter/smb/v2/handlers/durable_context.go` - Wire format functions, ProcessDurableHandleContext, ProcessDurableReconnectContext, ProcessAppInstanceId, buildPersistedDurableHandle
- `internal/adapter/smb/v2/handlers/durable_context_test.go` - 26 tests: wire format, grant, reconnect, AppInstanceId, OpenFile fields
- `internal/adapter/smb/v2/handlers/handler.go` - OpenFile durable fields, Handler DurableStore/DurableTimeoutMs, closeFilesWithFilter durable persistence, default 60s timeout
- `internal/adapter/smb/v2/handlers/create.go` - Reconnect early-exit (Step 4b), AppInstanceId + grant processing (Step 8c), durable response context (Step 10b2), computeSessionKeyHash helper

## Decisions Made
- V2 (DH2Q) takes precedence over V1 (DHnQ) when both present per MS-SMB2 spec
- V1 requires batch oplock (0x09) for grant; V2 has no oplock requirement
- Persistent flag (DH2FlagPersistent = 0x02) in DH2Q is rejected (not supported)
- Reconnect checks return specific NTSTATUS: ACCESS_DENIED (username/key mismatch), OBJECT_NAME_NOT_FOUND (share/handle missing, expired), INVALID_PARAMETER (path/conflicting tags)
- Session key hash = SHA-256 of session.CryptoState.SigningKey (zero hash if no crypto state)
- Reconnect early-exit at Step 4b in CREATE handler avoids unnecessary file open/creation
- AppInstanceId context tag uses raw 4-byte wire representation ("\x45\x17\xb6\x11")
- DurableTimeoutMs defaults to 60000ms in NewHandlerWithSessionManager constructor
- IsDurable NOT set on restored handle -- client must re-request durability after reconnect

## Deviations from Plan

### Auto-fixed Issues

**1. [Rule 3 - Blocking] Reconnect path placement**
- **Found during:** Part C integration
- **Issue:** Plan specified Step 8b for reconnect, but reconnect must happen before file creation (Step 7) to avoid unnecessary file operations
- **Fix:** Added reconnect check at Step 4b (after path resolution, before file existence check) for clean early-exit
- **Files modified:** create.go

**2. [Rule 2 - Missing] DurableTimeoutMs default**
- **Found during:** Handler construction review
- **Issue:** DurableTimeoutMs was zero by default, causing immediate timeout for all durable handles
- **Fix:** Set default to 60000 (60 seconds) in NewHandlerWithSessionManager
- **Files modified:** handler.go

## Issues Encountered

None beyond the deviations documented above.

## User Setup Required

None - no external service configuration required.

## Next Phase Readiness
- Durable handle grant and reconnect are fully functional
- CREATE handler properly routes reconnect vs normal create paths
- closeFilesWithFilter correctly persists durable handles for scavenger (Plan 03) to manage
- All 3 plans of Phase 38 are now functionally complete
- Phase 39 (cross-protocol testing) can validate durable handle behavior end-to-end

---
*Phase: 38-durable-handles*
*Completed: 2026-03-02*
