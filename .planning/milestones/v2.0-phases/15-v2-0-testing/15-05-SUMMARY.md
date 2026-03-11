---
phase: 15-v2-0-testing
plan: 05
subsystem: testing
tags: [posix, nfsv4, pjdfstest, e2e, stress, coverage, control-plane]

# Dependency graph
requires:
  - phase: 15-01
    provides: NFSv4 E2E framework (mount helpers, platform skips, basic operations)
  - phase: 14
    provides: Control plane v2.0 settings watcher, blocked operations, netgroup access
provides:
  - NFSv4 POSIX compliance testing via pjdfstest with --nfs-version parameter
  - Known failures documentation for NFSv4 (known_failures_v4.txt)
  - Control plane v2.0 mount-level E2E tests (blocked ops, netgroup, hot-reload)
  - Stress tests gated behind -tags=stress (large directory, concurrent delegations, concurrent creation)
  - E2E test runner script with --coverage flag for coverage profile generation
affects: [ci-pipeline, documentation]

# Tech tracking
tech-stack:
  added: []
  patterns: [build-tag-gating, version-parameterized-tests, posix-compliance-testing]

key-files:
  created:
    - test/posix/known_failures_v4.txt
    - test/e2e/nfsv4_controlplane_test.go
    - test/e2e/nfsv4_stress_test.go
    - test/e2e/run-e2e.sh
  modified:
    - test/posix/setup-posix.sh
    - test/posix/run-posix.sh
    - test/posix/README.md

key-decisions:
  - "NFSv4 mount uses vers=4.0 without mountport or nolock (stateful protocol, no NLM)"
  - "known_failures_v4.txt starts with v3 common failures plus v4-specific entries"
  - "Stress tests use //go:build e2e && stress dual tag for CI flexibility"
  - "run-e2e.sh uses -coverprofile=coverage-e2e.out for merge with unit coverage"
  - "Netgroup test validates both per-operation and mount-time-only access patterns"

patterns-established:
  - "Build tag gating: -tags=stress for CI-excluded long-running tests"
  - "E2E runner script: centralized flag-based test orchestration"
  - "POSIX version parameterization: --nfs-version for multi-version compliance"

requirements-completed: [TEST2-01, TEST2-05, TEST2-06]

# Metrics
duration: 7min
completed: 2026-02-17
---

# Phase 15 Plan 05: POSIX NFSv4 Compliance, Control Plane Mount-Level Tests, and Stress Suite Summary

**pjdfstest NFSv4 POSIX compliance via --nfs-version parameter, control plane blocked-ops/netgroup mount-level E2E tests, stress suite behind -tags=stress, and run-e2e.sh with --coverage flag**

## Performance

- **Duration:** 7 min
- **Started:** 2026-02-17T17:13:43Z
- **Completed:** 2026-02-17T17:21:08Z
- **Tasks:** 3
- **Files modified:** 7

## Accomplishments

- pjdfstest POSIX compliance suite extended to NFSv4 with --nfs-version parameter in setup-posix.sh and version logging in run-posix.sh
- Control plane v2.0 mount-level E2E tests validate blocked operations, netgroup access, and settings hot-reload are observable through real NFS mounts
- Stress tests with 500-file directories, concurrent delegation recall, and 1000-file concurrent creation gated behind -tags=stress
- E2E test runner script (run-e2e.sh) with --coverage, --stress, --s3, --nfs-version, --race, and --test flags

## Task Commits

Each task was committed atomically:

1. **Task 1: pjdfstest NFSv4 support** - `e7fa56d` (feat)
2. **Task 2: Control plane v2.0 mount-level E2E tests** - `5e2840b` (feat)
3. **Task 3: Stress tests and E2E coverage profile support** - `27c2059` (feat)

## Files Created/Modified

- `test/posix/setup-posix.sh` - Updated with --nfs-version parameter, NFSv4 mount options
- `test/posix/run-posix.sh` - Updated with --nfs-version logging and version detection
- `test/posix/known_failures_v4.txt` - NFSv4-specific expected pjdfstest failures
- `test/posix/README.md` - NFSv4 testing section with setup, mount differences, CI parallelism
- `test/e2e/nfsv4_controlplane_test.go` - 4 control plane mount-level tests (blocked ops, netgroup, hot-reload, multiple blocked ops)
- `test/e2e/nfsv4_stress_test.go` - 3 stress tests behind -tags=stress (large dir, concurrent delegations, concurrent creation)
- `test/e2e/run-e2e.sh` - E2E test runner with --coverage, --stress, --s3, --nfs-version flags

## Decisions Made

- NFSv4 mount options exclude mountport and nolock (NFSv4 is stateful, no separate mount protocol or NLM)
- known_failures_v4.txt documents ETXTBSY as protocol-inherent (affects both v3 and v4), removes locking failures since NFSv4 has integrated locking
- Stress tests use dual build tag (e2e && stress) so normal E2E runs exclude long-running tests
- Netgroup E2E test handles both per-operation and mount-time-only access checking behaviors
- run-e2e.sh generates coverage at coverage-e2e.out in repo root for easy CI integration

## Deviations from Plan

None - plan executed exactly as written.

## Issues Encountered

None.

## User Setup Required

None - no external service configuration required.

## Next Phase Readiness

- Phase 15 (v2.0 Testing) is now COMPLETE with all 5 plans executed
- NFSv4 E2E framework, locking, delegations, Kerberos/ACL, POSIX compliance, control plane, and stress tests all in place
- Ready to proceed to Phase 16

## Self-Check: PASSED

All 7 created/modified files verified present on disk. All 3 task commits (e7fa56d, 5e2840b, 27c2059) verified in git history.

---
*Phase: 15-v2-0-testing*
*Completed: 2026-02-17*
