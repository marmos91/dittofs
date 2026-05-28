---
phase: 25-v3-integration-testing
plan: 01
subsystem: testing
tags: [nfsv4.1, e2e, mount, coexistence, store-matrix]

# Dependency graph
requires:
  - phase: 24-directory-delegations
    provides: complete NFSv4.1 implementation (sessions, EOS, backchannel, dir delegations)
  - phase: 15-v2-0-testing
    provides: e2e test framework with versioned NFS mounts and store matrix
provides:
  - NFSv4.1 mount support in e2e framework (MountNFSExportWithVersion case "4.1")
  - SkipIfNFSv41Unsupported helper for platform-aware test skipping
  - v4.1 added to all version-parametrized test suites (basic, advanced, store matrix, file size, concurrency)
  - v4.0/v4.1 and v3/v4.1 coexistence test file with bidirectional visibility tests
affects: [25-02, 25-03, all future e2e tests using version parametrization]

# Tech tracking
tech-stack:
  added: []
  patterns: [v4.1 skip guard pattern, coexistence test pattern with cross-version mount pairs]

key-files:
  created:
    - test/e2e/nfsv41_coexistence_test.go
  modified:
    - test/e2e/framework/mount.go
    - test/e2e/framework/helpers.go
    - test/e2e/nfsv4_basic_test.go
    - test/e2e/nfsv4_store_matrix_test.go

key-decisions:
  - "v4.1 mount options mirror v4.0 (vers=4.1,port,actimeo=0) -- stateful protocol, no mountport"
  - "macOS skips v4.1 via t.Skip in both mount.go and SkipIfNFSv41Unsupported (not t.Fatal)"
  - "Linux best-effort v4.1 check via /proc/fs/nfsfs presence (don't skip if check fails)"
  - "Coexistence tests use 500ms sleep between cross-version write/read per pitfall guidance"

patterns-established:
  - "v4.1 skip guard: if ver == '4.1' { framework.SkipIfNFSv41Unsupported(t) } alongside v4.0 guard"
  - "Coexistence test: mount two versions simultaneously, test bidirectional file/dir/rename/delete visibility"

requirements-completed: [TEST-01, TEST-04]

# Metrics
duration: 4min
completed: 2026-02-23
---

# Phase 25 Plan 01: NFSv4.1 Mount Framework and Coexistence Tests Summary

**Extended e2e framework with NFSv4.1 mount support, parametrized all existing version-loop tests to include v4.1, and added v4.0/v4.1 + v3/v4.1 coexistence tests**

## Performance

- **Duration:** 4 min
- **Started:** 2026-02-23T09:51:08Z
- **Completed:** 2026-02-23T09:55:03Z
- **Tasks:** 2
- **Files modified:** 5 (4 modified + 1 created)

## Accomplishments
- Extended MountNFSExportWithVersion to handle "4.1" with macOS skip and Linux kernel support
- Added SkipIfNFSv41Unsupported helper separate from v4.0 skip (allows v4.0 on macOS, v4.1 skips)
- Added "4.1" to 8 version-parametrized test loops across 2 files (basic ops, advanced ops, READDIR pagination, store matrix, file size matrix, multi-share, multi-client)
- Created coexistence test file with TestNFSv41v40Coexistence (6 subtests) and TestNFSv41v3Coexistence (5 subtests)

## Task Commits

Each task was committed atomically:

1. **Task 1: Extend mount framework for NFSv4.1 and parametrize existing tests** - `693d371b` (test)
2. **Task 2: Add v4.0/v4.1 coexistence test** - `dbb6dd5b` (test)

## Files Created/Modified
- `test/e2e/framework/mount.go` - Added case "4.1" to MountNFSExportWithVersion switch with macOS skip
- `test/e2e/framework/helpers.go` - Added SkipIfNFSv41Unsupported helper with Linux best-effort check
- `test/e2e/nfsv4_basic_test.go` - Added "4.1" to 3 version slices (basic, advanced, READDIR pagination)
- `test/e2e/nfsv4_store_matrix_test.go` - Added "4.1" to 4 version slices (store matrix, file sizes, multi-share, multi-client)
- `test/e2e/nfsv41_coexistence_test.go` - New file: v4.0+v4.1 and v3+v4.1 simultaneous mount coexistence tests (297 lines)

## Decisions Made
- v4.1 mount options mirror v4.0 (vers=4.1,port,actimeo=0) since both are stateful protocols not needing mountport
- macOS skip uses t.Skip (not t.Fatal) per plan specification, applied in both mount.go and helpers.go
- Linux best-effort /proc/fs/nfsfs check in SkipIfNFSv41Unsupported -- does not skip if check fails, lets mount attempt provide the definitive answer
- Coexistence tests use 500ms time.Sleep between cross-version operations per pitfall guidance for NFS cache settling

## Deviations from Plan

None - plan executed exactly as written.

## Issues Encountered
None

## User Setup Required
None - no external service configuration required.

## Next Phase Readiness
- v4.1 mount framework is ready for use by Plans 02 (SMB Kerberos) and 03 (EOS replay, backchannel, directory delegation tests)
- All existing tests continue to pass unchanged (only version slices extended)
- Coexistence test patterns established for reuse in future cross-protocol tests

## Self-Check: PASSED

All 5 modified/created files verified present. Both task commits (693d371b, dbb6dd5b) verified in git log.

---
*Phase: 25-v3-integration-testing*
*Completed: 2026-02-23*
