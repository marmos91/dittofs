---
phase: 32-windows-integration-testing
plan: 02
subsystem: testing
tags: [smbtorture, samba, docker, ci, conformance, smb2]

# Dependency graph
requires:
  - phase: 29.8-ms-protocol-test-suite-ci
    provides: WPTS Docker infrastructure, docker-compose.yml, bootstrap.sh, CI workflow
provides:
  - smbtorture Docker service in docker-compose.yml
  - smbtorture/run.sh orchestrator with profile/filter/keep/verbose/dry-run flags
  - smbtorture/parse-results.sh with wildcard known-failure classification
  - smbtorture/KNOWN_FAILURES.md baseline for expected SMB2 failures
  - smbtorture CI job in smb-conformance.yml alongside WPTS
  - smbtorture/Makefile and parent Makefile targets
affects: [44-smb3-conformance-testing]

# Tech tracking
tech-stack:
  added: [samba-toolbox:v0.8]
  patterns: [smbtorture-docker-isolation, wildcard-known-failure-matching, profile-based-smbtorture]

key-files:
  created:
    - test/smb-conformance/smbtorture/run.sh
    - test/smb-conformance/smbtorture/parse-results.sh
    - test/smb-conformance/smbtorture/KNOWN_FAILURES.md
    - test/smb-conformance/smbtorture/Makefile
  modified:
    - test/smb-conformance/docker-compose.yml
    - test/smb-conformance/Makefile
    - .github/workflows/smb-conformance.yml

key-decisions:
  - "Wildcard known-failure patterns (smb2.durable-open.*) for category-wide expected failures"
  - "S3 profiles excluded from smbtorture matrix (unnecessary Localstack complexity)"
  - "GPL compliance via Docker container boundary (smbtorture binary never touched directly)"
  - "Reuses existing bootstrap.sh for DittoFS provisioning (same shares/users as WPTS)"

patterns-established:
  - "smbtorture wildcard matching: patterns ending with .* match any test with that prefix"
  - "smbtorture output parsing: handles both 'success:/failure:/skip:' and subunit-style formats"

requirements-completed: [TEST-01, TEST-02, TEST-03]

# Metrics
duration: 5min
completed: 2026-02-28
---

# Phase 32 Plan 02: smbtorture SMB2 Conformance Infrastructure Summary

**smbtorture SMB2 test suite integration via Docker-isolated Samba toolbox with wildcard known-failure classification and parallel CI alongside WPTS**

## Performance

- **Duration:** 5 min
- **Started:** 2026-02-28T06:58:44Z
- **Completed:** 2026-02-28T07:04:00Z
- **Tasks:** 2
- **Files modified:** 7

## Accomplishments
- Added smbtorture service to docker-compose.yml using samba-toolbox:v0.8 with profile-based activation
- Created run.sh orchestrator mirroring WPTS pattern with --profile, --filter, --keep, --verbose, --dry-run flags
- Built parse-results.sh parser supporting both smbtorture output formats and wildcard known-failure matching
- Established KNOWN_FAILURES.md baseline with 14 category-wide patterns covering unimplemented SMB3 features
- Integrated smbtorture CI job into existing smb-conformance.yml with tiered matrix (memory-only on PRs)

## Task Commits

Each task was committed atomically:

1. **Task 1: smbtorture Docker infrastructure and run script** - `cdbdebd6` (feat)
2. **Task 2: Result parser, KNOWN_FAILURES.md, CI workflow, and Makefile** - `e4d366fe` (feat)

## Files Created/Modified
- `test/smb-conformance/docker-compose.yml` - Added smbtorture service (samba-toolbox:v0.8, profile activation)
- `test/smb-conformance/smbtorture/run.sh` - Orchestrator: build DittoFS, bootstrap, run smbtorture, parse results (286 lines)
- `test/smb-conformance/smbtorture/parse-results.sh` - Parser with wildcard known-failure matching (334 lines)
- `test/smb-conformance/smbtorture/KNOWN_FAILURES.md` - Initial baseline with 14 smb2.* category patterns
- `test/smb-conformance/smbtorture/Makefile` - Local dev targets (test, test-quick, test-full, clean)
- `test/smb-conformance/Makefile` - Added smbtorture and smbtorture-quick targets
- `.github/workflows/smb-conformance.yml` - Added parallel smbtorture CI job with tiered matrix

## Decisions Made
- Wildcard known-failure patterns (e.g., `smb2.durable-open.*`) to avoid listing every individual test name before first run
- S3 profiles excluded from smbtorture matrix since Localstack adds unnecessary complexity for protocol conformance
- GPL compliance maintained via Docker container boundary -- smbtorture binary is never extracted or linked
- Reused existing bootstrap.sh for DittoFS provisioning (shares, users, SMB adapter are identical for both WPTS and smbtorture)
- Parse script supports both `success:/failure:` and subunit-style (`ok`/`FAILED`/`SKIP`) smbtorture output formats

## Deviations from Plan

None - plan executed exactly as written.

## Issues Encountered
None.

## User Setup Required
None - no external service configuration required.

## Next Phase Readiness
- smbtorture infrastructure ready for first run to establish actual baseline
- KNOWN_FAILURES.md should be updated after first smbtorture execution with actual test names
- Phase 32-03 (Windows validation checklist) can proceed
- Phase 44 (SMB3 Conformance Testing) will extend this infrastructure

---
*Phase: 32-windows-integration-testing*
*Completed: 2026-02-28*
