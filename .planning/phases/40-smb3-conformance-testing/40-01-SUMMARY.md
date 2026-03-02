---
phase: 40-smb3-conformance-testing
plan: 01
subsystem: testing
tags: [smbtorture, smb3, conformance, baseline, known-failures]

# Dependency graph
requires:
  - phase: 33-39
    provides: SMB3 dialect negotiation, key derivation, signing, encryption, Kerberos, leases, durable handles, cross-protocol coordination
provides:
  - smbtorture baseline results with per-sub-suite pass/fail/skip counts
  - Individual test name enumeration in KNOWN_FAILURES.md (no wildcards)
  - Fix candidate list for subsequent plans
affects: [40-02, 40-03, 40-04, 40-05, 40-06]

# Tech tracking
tech-stack:
  added: []
  patterns: [individual-test-enumeration, fix-candidate-tracking]

key-files:
  created:
    - test/smb-conformance/smbtorture/baseline-results.md
  modified:
    - test/smb-conformance/smbtorture/KNOWN_FAILURES.md

key-decisions:
  - "Excluded 119 fix candidate failures from KNOWN_FAILURES (implemented features that still fail)"
  - "252 individual test entries replace all wildcard patterns in KNOWN_FAILURES"
  - "Charset and delete-on-close edge case failures tracked as fix candidates, not known failures"
  - "Directory leases (dirlease) categorized as genuinely unimplemented (separate from file leases)"

patterns-established:
  - "Individual test enumeration: every KNOWN_FAILURES entry is a specific test name, never a wildcard"
  - "Fix candidate tracking: tests for implemented features that still fail go in baseline-results.md, not KNOWN_FAILURES"

requirements-completed: [TEST-01]

# Metrics
duration: 32min
completed: 2026-03-02
---

# Phase 40 Plan 01: smbtorture Baseline Summary

**Full smbtorture baseline captured (602 tests: 54 pass / 372 fail / 176 skip) with 50 newly-passing tests identified and all wildcard known-failure entries replaced with 252 individual test names**

## Performance

- **Duration:** 32 min
- **Started:** 2026-03-02T19:13:12Z
- **Completed:** 2026-03-02T19:46:00Z
- **Tasks:** 2
- **Files modified:** 2

## Accomplishments
- Ran full smbtorture suite (all SMB2/SMB3 sub-suites) against DittoFS with memory profile
- Captured individual test-level results: 54 pass, 372 fail, 176 skip across 62 sub-suites
- Identified 50 tests newly passing after phases 33-39 (previously masked by wildcards)
- Replaced all wildcard patterns in KNOWN_FAILURES.md with 252 individually-verified test entries
- Categorized 119 failing tests as fix candidates (implemented features like sessions, leases, durable handles)
- Identified directory leases (18 tests) as genuinely unimplemented, separate from file leases

## Task Commits

Each task was committed atomically:

1. **Task 1: Run full smbtorture baseline** - `c40b8062` (feat)
2. **Task 2: Replace wildcards with individual test names** - `5308c8d6` (feat)

## Files Created/Modified
- `test/smb-conformance/smbtorture/baseline-results.md` - Per-sub-suite pass/fail/skip counts, newly passing tests, fix candidates, full failing test list
- `test/smb-conformance/smbtorture/KNOWN_FAILURES.md` - 252 individual test entries with verified reasons, no wildcard patterns

## Decisions Made
- **119 fix candidates excluded from KNOWN_FAILURES:** Tests for sessions (11 fail), leases (32 fail), durable handles V2 (32 fail), durable handles V1 (10 fail), locks (15 fail), timestamps (9 fail), rename (6 fail), and charset (2 fail) are implemented features that still fail. These are tracked in baseline-results.md for subsequent fix plans.
- **Directory leases categorized as unimplemented:** Unlike file leases (Phase 37), directory leases are a separate feature not yet implemented. All 18 dirlease tests are in KNOWN_FAILURES.
- **Charset and delete-on-close edge cases as fix candidates:** Since basic charset support works (2/4 pass) and basic delete-on-close works (3/9 pass), edge case failures are fix candidates not known failures.

## Deviations from Plan

None - plan executed exactly as written.

## Issues Encountered
- ARM64 emulation (Apple Silicon) caused ~3-5x slowdown for smbtorture Docker container. Used full 20-minute timeout per sub-suite. Total suite execution took approximately 20 minutes under emulation.
- Charset tests have spaces in test names (e.g., `charset.Testing partial surrogate`) which get truncated by parse-results.sh to `charset.Testing`. Both passing and failing tests share the same truncated name. Handled by excluding charset from KNOWN_FAILURES entirely.

## User Setup Required
None - no external service configuration required.

## Next Phase Readiness
- Baseline-results.md provides the fix candidate list for plan 40-02 (iterate-and-fix cycle)
- KNOWN_FAILURES.md is ready for CI use with individual test names
- Fix ordering recommendation: start with highest-impact failures (leases 32, durable-v2 32, locks 15) that could cascade to other improvements

---
*Phase: 40-smb3-conformance-testing*
*Completed: 2026-03-02*
