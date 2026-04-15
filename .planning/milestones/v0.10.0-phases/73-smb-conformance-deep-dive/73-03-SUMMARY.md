---
phase: 73-smb-conformance-deep-dive
plan: 03
subsystem: smb
tags: [smb, change-notify, session-reauth, anonymous-encryption, async]

requires:
  - phase: 73-smb-conformance-deep-dive
    plan: 01
    provides: ChangeNotify ADS stream constants, MatchesFilter, KNOWN_FAILURES baseline

provides:
  - Generalized AsyncResponseRegistry for async operation tracking beyond ChangeNotify
  - CompletionFilter validation (IsValidCompletionFilter)
  - NotifyRmdir for STATUS_NOTIFY_CLEANUP on watched directory removal
  - UnregisterAllForSession and UnregisterAllForTree cleanup methods
  - Session re-authentication with key re-derivation (tryReauthUpdateWithKeys)
  - Anonymous/null/guest session encryption bypass

affects: [73-04, smbtorture-conformance, wpts-conformance]

tech-stack:
  added: []
  patterns:
    - "AsyncResponseRegistry for general-purpose async operation tracking"
    - "tryReauthUpdateWithKeys preserves tree connects while re-deriving keys"
    - "Anonymous session bypass in checkEncryptionRequired"

key-files:
  created: []
  modified:
    - internal/adapter/smb/v2/handlers/change_notify.go
    - internal/adapter/smb/v2/handlers/change_notify_test.go
    - internal/adapter/smb/v2/handlers/stub_handlers.go
    - internal/adapter/smb/v2/handlers/session_setup.go
    - internal/adapter/smb/response.go
    - test/smb-conformance/smbtorture/KNOWN_FAILURES.md

key-decisions:
  - "AsyncResponseRegistry as separate struct rather than extending NotifyRegistry — cleaner separation of concerns for future async ops (lock waits)"
  - "CompletionFilter validation rejects 0 and invalid flag bits per MS-SMB2 3.3.5.15"
  - "Re-auth re-derives keys via configureSessionSigningWithKey on existing session rather than creating new session"
  - "Anonymous/guest encryption bypass in checkEncryptionRequired guards by IsNull || IsGuest on session"

patterns-established:
  - "AsyncResponseRegistry for general-purpose async tracking"
  - "NotifyRmdir dual notification: STATUS_NOTIFY_CLEANUP to directory watcher + FileActionRemoved to parent"

requirements-completed: [WPTS-01, WPTS-02]

duration: 17min
completed: 2026-03-24
---

# Phase 73 Plan 03: ChangeNotify Completion, Session Re-Auth, Anonymous Encryption Summary

**Generalized async response registry, CompletionFilter validation, NotifyRmdir cleanup, session re-auth with key re-derivation per MS-SMB2 3.3.5.5.3, and anonymous encryption bypass per MS-SMB2 3.3.5.2.9**

## Performance

- **Duration:** 17 min
- **Started:** 2026-03-24T14:47:58Z
- **Completed:** 2026-03-24T15:04:58Z
- **Tasks:** 2
- **Files modified:** 6

## Accomplishments
- Added AsyncResponseRegistry for general-purpose async operation tracking (D-21)
- Added all MS-SMB2 2.2.35 completion filter constants including stream filters and EA
- Added CompletionFilter validation (IsValidCompletionFilter) in ChangeNotify handler
- Added NotifyRmdir for STATUS_NOTIFY_CLEANUP to watchers on removed directory
- Added UnregisterAllForSession and UnregisterAllForTree cleanup methods
- Implemented session re-authentication with key re-derivation (tryReauthUpdateWithKeys)
- Added anonymous/null/guest session encryption bypass in checkEncryptionRequired
- Removed ~25 tests from smbtorture KNOWN_FAILURES (17 ChangeNotify + 5 reauth + 3 anon-encryption)

## Task Commits

Each task was committed atomically:

1. **Task 1: Complete smbtorture ChangeNotify and generalize async mechanism** - `ffc9c847` (feat)
2. **Task 2: Fix session re-auth, anonymous encryption, and update KNOWN_FAILURES** - `54730e6f` (feat)

## Files Created/Modified
- `internal/adapter/smb/v2/handlers/change_notify.go` - AsyncResponseRegistry, stream action codes, NotifyRmdir, UnregisterAllForSession/Tree, IsValidCompletionFilter
- `internal/adapter/smb/v2/handlers/change_notify_test.go` - Tests for double watchers, mask filtering, rmdir cleanup, valid-req, async registry
- `internal/adapter/smb/v2/handlers/stub_handlers.go` - CompletionFilter validation in ChangeNotify handler
- `internal/adapter/smb/v2/handlers/session_setup.go` - tryReauthUpdateWithKeys with key re-derivation
- `internal/adapter/smb/response.go` - Anonymous/guest session encryption bypass
- `test/smb-conformance/smbtorture/KNOWN_FAILURES.md` - Removed 25 tests, updated section descriptions

## Decisions Made
- AsyncResponseRegistry as separate struct for clean separation from NotifyRegistry
- CompletionFilter 0 and invalid bits rejected per MS-SMB2 3.3.5.15
- Re-auth calls configureSessionSigningWithKey on existing session (preserves tree connects/files)
- Anonymous/guest bypass in checkEncryptionRequired prevents STATUS_ACCESS_DENIED for null sessions

## Deviations from Plan

### Auto-fixed Issues

**1. [Rule 2 - Missing functionality] Stream filter constants not present in worktree**
- **Found during:** Task 1
- **Issue:** Plan 01 stream filter constants (FileNotifyChangeStreamName/Size/Write) were not in this worktree (parallel execution)
- **Fix:** Added stream filter constants, stream action codes, and MatchesFilter support directly in this plan
- **Files modified:** internal/adapter/smb/v2/handlers/change_notify.go
- **Committed in:** ffc9c847

---

**Total deviations:** 1 auto-fixed (1 missing functionality)
**Impact on plan:** No scope creep. Stream constants needed to be present for MatchesFilter to work correctly.

## Issues Encountered
- Plan 01 changes were on a parallel worktree, so stream filter constants had to be included here

## User Setup Required
None - no external service configuration required.

## Next Phase Readiness
- ChangeNotify infrastructure complete for all filter types including streams
- Session re-auth working with key re-derivation
- Anonymous encryption bypass in place
- ~25 fewer smbtorture known failures

---
*Phase: 73-smb-conformance-deep-dive*
*Completed: 2026-03-24*
