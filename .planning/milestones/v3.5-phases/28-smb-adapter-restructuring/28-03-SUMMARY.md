---
phase: 28-smb-adapter-restructuring
plan: 03
subsystem: smb
tags: [smb2, connection, refactor, internal-extraction]

# Dependency graph
requires:
  - phase: 28-01
    provides: "BaseAdapter extraction and shared adapter patterns"
  - phase: 28-02
    provides: "BaseAdapter embedding in SMB Adapter struct"
provides:
  - "Thin pkg/adapter/smb/connection.go serve loop (238 lines, down from 1071)"
  - "internal/adapter/smb/framing.go - ReadRequest, WriteNetBIOSFrame, SendRawMessage, NewSessionSigningVerifier"
  - "internal/adapter/smb/compound.go - ProcessCompoundRequest, ParseCompoundCommand, VerifyCompoundCommandSignature, InjectFileID"
  - "internal/adapter/smb/response.go - ProcessSingleRequest, SendResponse, SendErrorResponse, HandleSMB1Negotiate, MakeErrorBody"
  - "internal/adapter/smb/conn_types.go - ConnInfo, LockedWriter, SessionTracker interface"
affects: [28-05]

# Tech tracking
tech-stack:
  added: []
  patterns:
    - "ConnInfo struct pattern for passing connection metadata to internal/ functions"
    - "SessionTracker interface to avoid circular imports between pkg/ and internal/"
    - "SigningVerifier interface decoupling framing from session management"
    - "LockedWriter type for cross-package write mutex sharing"

key-files:
  created:
    - internal/adapter/smb/framing.go
    - internal/adapter/smb/compound.go
    - internal/adapter/smb/response.go
    - internal/adapter/smb/conn_types.go
  modified:
    - pkg/adapter/smb/connection.go
    - pkg/adapter/smb/connection_test.go

key-decisions:
  - "Created response.go instead of expanding dispatch.go to keep dispatch table separate from response/send logic"
  - "Used ConnInfo struct + SessionTracker interface instead of exporting Connection fields to avoid pkg/ -> internal/ circular deps"
  - "Moved sessionSigningVerifier to internal/adapter/smb/framing.go as NewSessionSigningVerifier factory for cleaner separation"

patterns-established:
  - "ConnInfo pattern: internal/ functions receive explicit ConnInfo struct instead of Connection receiver"
  - "SessionTracker interface: pkg/ Connection implements, internal/ code calls without importing pkg/"
  - "LockedWriter: shared mutex type in internal/ used by both pkg/ and internal/ code"

requirements-completed: [REF-04]

# Metrics
duration: 35min
completed: 2026-02-25
---

# Phase 28 Plan 03: Connection Extraction Summary

**Extracted framing, compound, dispatch+response from connection.go (1071 -> 238 lines) to 4 focused internal/ files with ConnInfo/SessionTracker decoupling**

## Performance

- **Duration:** ~35 min
- **Started:** 2026-02-25T20:32:00Z
- **Completed:** 2026-02-25T21:07:46Z
- **Tasks:** 2
- **Files modified:** 6

## Accomplishments
- Reduced connection.go from ~1071 lines to 238 lines (78% reduction)
- Created 4 focused internal/ files: framing.go (287 lines), compound.go (220 lines), response.go (414 lines), conn_types.go (55 lines)
- Established ConnInfo struct + SessionTracker interface pattern that decouples pkg/ from internal/ without circular imports
- All existing SMB tests pass unchanged (connection_test.go updated to call internal/ exports)
- Mirrors NFS adapter pattern: thin pkg/ serve loop + protocol logic in internal/

## Task Commits

Each task was committed atomically:

1. **Task 1: Extract framing, compound, and dispatch to internal/** - `605fe3f7` (refactor)
2. **Task 2: Slim connection.go to thin serve loop** - `273ac1e8` (refactor)

## Files Created/Modified
- `internal/adapter/smb/framing.go` (created) - ReadRequest, WriteNetBIOSFrame, SendRawMessage, NewSessionSigningVerifier, SigningVerifier interface
- `internal/adapter/smb/compound.go` (created) - ProcessCompoundRequest, ParseCompoundCommand, VerifyCompoundCommandSignature, InjectFileID
- `internal/adapter/smb/response.go` (created) - ProcessSingleRequest, ProcessRequestWithFileID, SendResponse, SendErrorResponse, SendMessage, HandleSMB1Negotiate, TrackSessionLifecycle, MakeErrorBody, SendAsyncChangeNotifyResponse
- `internal/adapter/smb/conn_types.go` (created) - ConnInfo struct, LockedWriter type, SessionTracker interface
- `pkg/adapter/smb/connection.go` (modified) - Slim serve loop delegating to internal/ functions
- `pkg/adapter/smb/connection_test.go` (modified) - Updated to use internal/ exported functions

## Decisions Made
- Created `response.go` as a new file instead of expanding `dispatch.go` (326 lines) to keep dispatch table initialization separate from response/send logic. This avoids a 700+ line dispatch.go monolith.
- Used `ConnInfo` struct + `SessionTracker` interface pattern instead of exporting Connection struct fields, preserving encapsulation and avoiding circular imports.
- Moved `sessionSigningVerifier` from connection.go to `internal/adapter/smb/framing.go` as `NewSessionSigningVerifier` factory function, keeping all signing verification logic co-located with framing.

## Deviations from Plan

### Auto-fixed Issues

**1. [Rule 3 - Blocking] Created response.go instead of expanding dispatch.go**
- **Found during:** Task 1
- **Issue:** Plan specified expanding dispatch.go with request processing and response functions. However dispatch.go already has 326 lines of dispatch table + handler wrappers. Adding ~400 lines of response/send logic would create a 700+ line file that mixes two distinct concerns.
- **Fix:** Created `internal/adapter/smb/response.go` for ProcessSingleRequest, SendResponse, SendErrorResponse, SendMessage, HandleSMB1Negotiate, etc. Kept dispatch.go unchanged.
- **Files modified:** internal/adapter/smb/response.go (new)
- **Verification:** go build passes, no circular imports
- **Committed in:** 605fe3f7

---

**Total deviations:** 1 auto-fixed (1 blocking)
**Impact on plan:** Better separation of concerns than plan specified. dispatch.go stays focused on dispatch table, response.go handles send/response logic. No scope creep.

## Issues Encountered
- File creation persistence issue: Initial Write tool calls for framing.go, compound.go, and conn_types.go did not persist on first attempt (only response.go did). Required re-creating all three files. Root cause unknown (tool issue).

## User Setup Required
None - no external service configuration required.

## Next Phase Readiness
- Connection extraction complete, ready for Plan 05 (test suite expansion)
- All 4 internal/ files are independently testable
- ConnInfo/SessionTracker pattern established for any future internal/ functions
- dispatch.go untouched and stable

## Self-Check: PASSED

All 6 created/modified files exist on disk. Both task commits (605fe3f7, 273ac1e8) found in git log. Summary file exists.

---
*Phase: 28-smb-adapter-restructuring*
*Completed: 2026-02-25*
