---
phase: 73-smb-conformance-deep-dive
plan: 01
subsystem: smb
tags: [smb, change-notify, ads, security-descriptor, wpts]

requires:
  - phase: 72-wpts-conformance-push
    provides: ChangeNotify async infrastructure, CANCEL support, completion filter dispatch

provides:
  - ADS stream ChangeNotify constants (FileNotifyChangeStreamName/Size/Write)
  - Updated MatchesFilter covering all MS-SMB2 2.2.35 completion filter flags
  - ADS write notifications in write handler
  - ChangeEa reclassified as Permanent in KNOWN_FAILURES

affects: [73-02, 73-03, wpts-conformance]

tech-stack:
  added: []
  patterns:
    - "Stream notification via existing NotifyChange dispatch with extended filter matching"

key-files:
  created: []
  modified:
    - internal/adapter/smb/v2/handlers/change_notify.go
    - internal/adapter/smb/v2/handlers/change_notify_test.go
    - internal/adapter/smb/v2/handlers/write.go
    - test/smb-conformance/KNOWN_FAILURES.md

key-decisions:
  - "Extended MatchesFilter rather than creating separate stream notification path"
  - "ChangeEa moved to Permanent (EA support not planned)"
  - "Security and CLOSE cleanup were already functional from Phase 72; plan identified them as needing fixes but they only needed the filter constants"

patterns-established:
  - "Stream notifications reuse existing NotifyChange + MatchesFilter dispatch"

requirements-completed: [WPTS-01, WPTS-04]

duration: 8min
completed: 2026-03-24
---

# Phase 73 Plan 01: ChangeNotify ADS Stream and Security Notifications Summary

**ADS stream ChangeNotify constants (0x200/0x400/0x800), extended MatchesFilter for stream + security filters, and write handler stream notification dispatch**

## Performance

- **Duration:** 8 min
- **Started:** 2026-03-24T14:28:42Z
- **Completed:** 2026-03-24T14:36:46Z
- **Tasks:** 2
- **Files modified:** 4

## Accomplishments
- Added FileNotifyChangeStreamName/Size/Write constants per MS-SMB2 2.2.35
- Updated MatchesFilter to recognize stream filters for add/modify/rename actions
- Wired ADS write notifications in the SMB WRITE handler
- Removed 5 ChangeNotify tests from KNOWN_FAILURES, moved ChangeEa to Permanent
- Corrected KNOWN_FAILURES counts (was 52+13=65, now 53+12=65 with accurate categorization)

## Task Commits

Each task was committed atomically:

1. **Task 1: Wire ADS stream ChangeNotify notifications** - `e8c06bd6` (feat)
2. **Task 2: Fix ServerReceiveSmb2Close notify cleanup and update KNOWN_FAILURES** - `ed516d03` (chore)

## Files Created/Modified
- `internal/adapter/smb/v2/handlers/change_notify.go` - Added stream filter constants, updated MatchesFilter
- `internal/adapter/smb/v2/handlers/change_notify_test.go` - Added stream name/size/write/security notification tests
- `internal/adapter/smb/v2/handlers/write.go` - Added ADS write notification dispatch
- `test/smb-conformance/KNOWN_FAILURES.md` - Removed 5 tests, moved ChangeEa to Permanent, updated counts

## Decisions Made
- Extended MatchesFilter with stream filters rather than creating a separate stream notification path - simpler, leverages existing infrastructure
- ChangeEa moved to Permanent status since EA (extended attributes) are not planned for implementation
- Security descriptor notification and CLOSE cleanup were already functional from Phase 72 - the fix was adding the filter constants and wiring ADS write notifications

## Deviations from Plan

### Auto-fixed Issues

**1. [Rule 1 - Bug] KNOWN_FAILURES count mismatch**
- **Found during:** Task 2
- **Issue:** Original file claimed "52 permanent + 13 expected = 65" but actual table had 52 permanent + 18 expected = 70 entries (3 Timestamp tests were missing from the count)
- **Fix:** Corrected counts to reflect actual table contents: 53 permanent + 12 expected = 65
- **Files modified:** test/smb-conformance/KNOWN_FAILURES.md
- **Committed in:** ed516d03

---

**Total deviations:** 1 auto-fixed (1 bug)
**Impact on plan:** Count correction ensures KNOWN_FAILURES accurately reflects test state. No scope creep.

## Issues Encountered
- Security descriptor change notify and CLOSE STATUS_NOTIFY_CLEANUP were already implemented in Phase 72; the plan expected them to need fixes but they only needed the stream filter constants to be added

## User Setup Required
None - no external service configuration required.

## Next Phase Readiness
- ADS share access enforcement (Plan 02) can proceed - 9 ADS tests remain as expected failures
- ChangeNotify fully functional for all filter types except EA (permanent)
- 12 expected failures remaining: 9 ADS + 3 Timestamp

---
*Phase: 73-smb-conformance-deep-dive*
*Completed: 2026-03-24*
