---
phase: 40-smb3-conformance-testing
plan: 02
subsystem: testing
tags: [wpts, smb-conformance, baseline, known-failures, test-filter]

requires:
  - phase: 33-smb3-encryption
    provides: AES-CCM/GCM cipher implementation
  - phase: 34-smb3-signing
    provides: CMAC/GMAC/HMAC signing implementation
  - phase: 35-37-smb3-leases-sessions-kerberos
    provides: Lease V2, session management, Kerberos auth
  - phase: 38-smb3-durable-handles
    provides: Durable handle V1/V2 with reconnect
  - phase: 39-cross-protocol-integration
    provides: Unified caching model, bidirectional break/recall
provides:
  - Enhanced WPTS test runner with documented filter syntax
  - Baseline-results.md with Phase 29.8 reference and Phase 30-39 improvement analysis
  - Updated KNOWN_FAILURES.md reflecting post-SMB3 implementation state
  - 5 tests reclassified from known failures to fix candidates
affects: [40-03, 40-04, 40-05, 40-06]

tech-stack:
  added: []
  patterns: [fix-candidate-not-known-failure, non-table-format-for-removed-entries]

key-files:
  created:
    - test/smb-conformance/baseline-results.md
  modified:
    - test/smb-conformance/run.sh
    - test/smb-conformance/KNOWN_FAILURES.md

key-decisions:
  - "Tests for implemented features removed from KNOWN_FAILURES, tracked as fix candidates in baseline-results.md"
  - "Removed entries use bullet-list format (not table) to prevent parse-results.sh from masking failures"
  - "Baseline measurement deferred to x86_64 Linux CI (WPTS container is linux/amd64 only)"

patterns-established:
  - "Fix candidate pattern: if feature IS implemented but test fails, investigate and fix -- do not add to KNOWN_FAILURES"
  - "Non-table format for removed entries: prevents accidental suppression by parse-results.sh"

requirements-completed: [TEST-02]

duration: 5min
completed: 2026-03-02
---

# Phase 40 Plan 02: WPTS BVT Baseline and Known Failures Update Summary

**Enhanced WPTS filter documentation, created baseline-results.md reference, and reclassified 5 known failures as fix candidates after Phases 33-39 SMB3 implementation**

## Performance

- **Duration:** 5 min
- **Started:** 2026-03-02T19:13:29Z
- **Completed:** 2026-03-02T19:19:24Z
- **Tasks:** 2
- **Files modified:** 3

## Accomplishments
- Enhanced run.sh help text with dotnet test filter syntax examples and known WPTS categories
- Created baseline-results.md documenting Phase 29.8 baseline (133/240), expected Phase 30-39 improvements, and WPTS category exploration guide
- Removed 5 tests from KNOWN_FAILURES.md whose features are now implemented (durable handles, leasing, oplock break, encryption flag)
- Reduced known failure count from 90 to 82 (47 permanent + 35 expected)
- Added Phase 33-39 improvements section to KNOWN_FAILURES.md

## Task Commits

Each task was committed atomically:

1. **Task 1: Re-measure WPTS BVT baseline and add --filter capability** - `3abeab3f` (feat)
2. **Task 2: Verify and update WPTS KNOWN_FAILURES.md** - `3d62fe28` (feat)

## Files Created/Modified
- `test/smb-conformance/run.sh` - Enhanced --filter help text with dotnet test syntax, category documentation
- `test/smb-conformance/baseline-results.md` - NEW: Phase 29.8 baseline reference, Phase 30-39 improvement analysis, WPTS category guide
- `test/smb-conformance/KNOWN_FAILURES.md` - Removed 5 implemented-feature tests, added Phase 33-39 section, updated counts

## Decisions Made
- **Fix candidate pattern:** Tests for implemented features must NOT remain in KNOWN_FAILURES. They are tracked as fix candidates in baseline-results.md. If they still fail after baseline measurement, they must be investigated and fixed.
- **Non-table format for removed entries:** The "Tests Removed from Known Failures" section uses bullet-list format (not markdown table) to prevent parse-results.sh from matching them as known failures. This ensures they surface as unexpected failures if they still fail.
- **Baseline measurement deferred:** The actual WPTS BVT baseline cannot be measured on ARM64 macOS (WPTS container is linux/amd64 only). baseline-results.md provides the template and instructions; actual measurement will happen on x86_64 Linux CI.

## Deviations from Plan

### Auto-fixed Issues

**1. [Rule 1 - Bug] Prevented parse-results.sh from masking fix-candidate failures**
- **Found during:** Task 2 (KNOWN_FAILURES.md update)
- **Issue:** The "Tests Removed from Known Failures" section initially used markdown table format. Since parse-results.sh reads ALL table rows from the file, these removed tests would have been matched as known failures, silently suppressing them if they still fail.
- **Fix:** Changed the "Tests Removed" section from table format to bullet-list format with an HTML comment explaining why.
- **Files modified:** test/smb-conformance/KNOWN_FAILURES.md
- **Verification:** `grep "BVT_DurableHandleV1|BVT_Leasing|BVT_OpLockBreak|IsEncryptionSupported" KNOWN_FAILURES.md | grep "^|"` returns no matches.
- **Committed in:** 3d62fe28 (Task 2 commit)

---

**Total deviations:** 1 auto-fixed (1 bug fix)
**Impact on plan:** Essential for correctness -- without this fix, implemented-feature failures would be silently suppressed.

## Issues Encountered
- WPTS baseline could not be measured on the current machine (ARM64 macOS). The WPTS FileServer container is linux/amd64 only. The baseline-results.md was created as a reference document with instructions for measurement on x86_64 Linux. This was expected per the plan's "IMPORTANT: Run on x86_64 Linux" note.
- The --filter and --category flags were already implemented in run.sh (plan's interface section stated they were not). The work focused on enhancing documentation rather than implementing from scratch.

## User Setup Required

None - no external service configuration required.

## Next Phase Readiness
- baseline-results.md ready to be populated after running `./run.sh --profile memory --verbose` on x86_64 Linux
- KNOWN_FAILURES.md reflects post-SMB3 state, ready for CI validation
- Fix candidates documented and will be tracked when baseline is measured
- Filter capability documented for faster iteration during fix cycles in future plans

---
*Phase: 40-smb3-conformance-testing*
*Completed: 2026-03-02*
