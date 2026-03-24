---
phase: 73-smb-conformance-deep-dive
plan: 02
subsystem: smb
tags: [wpts, ads, timestamp, share-mode, ms-fsa, conformance]

requires:
  - phase: 72-wpts-conformance
    provides: ChangeNotify infrastructure, timestamp freeze/unfreeze, ADS management ops

provides:
  - 12 WPTS BVT expected failures cleared (9 ADS + 3 timestamp)
  - ADS cross-stream share mode enforcement unit tests
  - KNOWN_FAILURES.md updated to 58 total (52 permanent + 6 expected)

affects: [73-smb-conformance-deep-dive, wpts-conformance]

tech-stack:
  added: []
  patterns:
    - "ADS cross-stream share mode: adsBasePath extracts base file, checkShareModeConflict spans base + all streams"
    - "Timestamp freeze/unfreeze: pin setAttrs.Ctime to preFile value on freeze, set to time.Now() on unfreeze"

key-files:
  created: []
  modified:
    - internal/adapter/smb/v2/handlers/handler_test.go
    - test/smb-conformance/KNOWN_FAILURES.md

key-decisions:
  - "ADS share access, management, and timestamp handling verified working -- removed 12 stale expected failures from KNOWN_FAILURES"
  - "6 ChangeNotify expected failures left for Plan 01 to handle (separate scope)"

patterns-established:
  - "ADS cross-stream unit tests: validate adsBasePath extraction and checkShareModeConflict for base+ADS, ADS+base, ADS+ADS scenarios"

requirements-completed: [WPTS-03, WPTS-04]

duration: 15min
completed: 2026-03-24
---

# Phase 73 Plan 02: ADS + Timestamp WPTS Conformance Summary

**Cleared 12 WPTS BVT expected failures: 9 ADS (share access enforcement, delete/list/rename streams) and 3 timestamp (directory ChangeTime freeze, LastWriteTime unfreeze, LastAccessTime notation) -- all verified working from prior Phase 72 implementations**

## Performance

- **Duration:** 15 min
- **Started:** 2026-03-24T14:26:43Z
- **Completed:** 2026-03-24T14:41:47Z
- **Tasks:** 2
- **Files modified:** 2

## Accomplishments
- Verified ADS cross-stream share mode enforcement (MS-FSA 2.1.5.1.2) works correctly for base+ADS, ADS+base, ADS+ADS, and directory ADS scenarios
- Verified ADS management operations (delete, list, rename streams) work for both files and directories
- Verified directory timestamp freeze/unfreeze (MS-FSA 2.1.5.14.2) and directory LastAccessTime notation (MS-FSA 2.1.4.4) work correctly
- Added comprehensive unit tests for adsBasePath and checkShareModeConflict with ADS cross-stream enforcement
- Updated KNOWN_FAILURES.md: removed 12 expected failures, updated counts to 58 (52 permanent + 6 expected)

## Task Commits

Each task was committed atomically:

1. **Task 1: Fix ADS share access enforcement and management operations** - `8c61eecb` (test)
2. **Task 2: Fix directory timestamp conformance and update KNOWN_FAILURES** - `bfe6db6d` (feat)

## Files Created/Modified
- `internal/adapter/smb/v2/handlers/handler_test.go` - Added TestAdsBasePath and TestCheckShareModeConflict_ADSCrossStream
- `test/smb-conformance/KNOWN_FAILURES.md` - Removed 12 expected failures, updated counts, added changelog

## Decisions Made
- Verified that ADS share access, management, and timestamp implementations from Phase 72 are correct -- these were listed as expected failures but the code was already working
- Left 6 ChangeNotify expected failures for Plan 01 scope (separate concern)
- Updated KNOWN_FAILURES count to 58 (52 permanent + 6 expected) independently from Plan 01's ChangeNotify changes

## Deviations from Plan

None - plan executed exactly as written. The implementations were already correct from Phase 72; the primary work was verification, test coverage, and KNOWN_FAILURES cleanup.

## Issues Encountered
None

## User Setup Required
None - no external service configuration required.

## Next Phase Readiness
- WPTS BVT expected failures reduced to 6 (all ChangeNotify-related, Plan 01 scope)
- ADS cross-stream share mode enforcement fully tested
- Ready for Plan 03+ smbtorture conformance work

---
*Phase: 73-smb-conformance-deep-dive*
*Completed: 2026-03-24*

## Self-Check: PASSED
