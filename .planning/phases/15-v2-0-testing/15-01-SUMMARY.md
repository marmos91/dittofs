---
phase: 15-v2-0-testing
plan: 01
subsystem: testing
tags: [nfsv4, e2e, mount, framework, backward-compat]

# Dependency graph
requires:
  - phase: 06-nfsv4-protocol-foundation
    provides: NFSv4 COMPOUND dispatcher and pseudo-fs
  - phase: 07-nfsv4-file-operations
    provides: NFSv4 file I/O handlers (READ, WRITE, OPEN, CLOSE)
  - phase: 08-nfsv4-advanced-operations
    provides: NFSv4 RENAME, LINK, SETATTR, VERIFY handlers
provides:
  - MountNFSWithVersion helper for v3/v4.0 parameterized mounts
  - MountNFSExportWithVersion for custom export paths with version
  - SkipIfDarwin, SkipIfNoNFS4ACLTools, SkipIfNFSv4Unsupported platform skip helpers
  - 8 NFSv4 E2E test functions covering basic/advanced operations
  - Golden path smoke test (TestNFSv4GoldenPathSmoke)
  - Backward compatibility regression guard (TestBackwardCompatNFSv3Full)
affects: [15-02, 15-03, 15-04, 15-05]

# Tech tracking
tech-stack:
  added: []
  patterns: [version-parameterized NFS mount tests, per-test server isolation]

key-files:
  created:
    - test/e2e/nfsv4_basic_test.go
  modified:
    - test/e2e/framework/mount.go
    - test/e2e/framework/helpers.go

key-decisions:
  - "MountNFSExportWithVersion as the shared core; MountNFSWithVersion delegates to it"
  - "NFSv4 mount uses vers=4.0 without mountport or nolock (stateful protocol)"
  - "Per-test server isolation via setupNFSv4TestServer helper"
  - "Version-parameterized subtests for v3+v4 shared tests"
  - "Pseudo-FS browsing test uses / export root mount"

patterns-established:
  - "Version-parameterized E2E tests: for ver in [3, 4.0] with SkipIfNFSv4Unsupported guard"
  - "setupNFSv4TestServer helper: server+stores+share+adapter+wait in one call"

requirements-completed: [TEST2-01, TEST2-06]

# Metrics
duration: 5min
completed: 2026-02-17
---

# Phase 15 Plan 01: NFSv4 E2E Framework and Basic Operations Summary

**MountNFSWithVersion helper with v3/v4.0 support, platform skip utilities, and 8 comprehensive E2E test functions covering NFSv4 basic/advanced operations, pseudo-fs browsing, READDIR pagination, golden path smoke, stale handle, and NFSv3 backward compatibility**

## Performance

- **Duration:** 5 min
- **Started:** 2026-02-17T17:05:54Z
- **Completed:** 2026-02-17T17:10:34Z
- **Tasks:** 2
- **Files modified:** 3

## Accomplishments
- Added MountNFSWithVersion and MountNFSExportWithVersion helpers supporting both NFSv3 and NFSv4.0 mount options
- Added Version field to Mount struct and SkipIfDarwin/SkipIfNoNFS4ACLTools/SkipIfNFSv4Unsupported platform skip helpers
- Created 8 E2E test functions with ~20+ subtests covering basic file I/O, advanced operations, OPEN create modes, pseudo-fs browsing, READDIR pagination, golden path smoke, stale handle, and backward compatibility
- Confirmed full backward compatibility: existing MountNFS() unchanged, all existing v1.0 test files compile without modifications

## Task Commits

Each task was committed atomically:

1. **Task 1: NFSv4 mount helper and platform skip utilities** - `a704041` (feat)
2. **Task 2: NFSv4 basic operations E2E tests and v1.0 backward compat validation** - `d3bdc14` (feat)

## Files Created/Modified
- `test/e2e/framework/mount.go` - Added MountNFSWithVersion, MountNFSExportWithVersion, Version field on Mount, updated CleanupStaleMounts patterns
- `test/e2e/framework/helpers.go` - Added SkipIfDarwin, SkipIfNoNFS4ACLTools, SkipIfNFSv4Unsupported
- `test/e2e/nfsv4_basic_test.go` - 8 test functions: TestNFSv4BasicOperations, TestNFSv4AdvancedFileOps, TestNFSv4OpenCreateModes, TestNFSv4PseudoFSBrowsing, TestNFSv4READDIRPagination, TestNFSv4GoldenPathSmoke, TestNFSv4StaleHandle, TestBackwardCompatNFSv3Full

## Decisions Made
- MountNFSExportWithVersion is the core implementation; MountNFSWithVersion delegates to it with "/export" default -- avoids code duplication
- NFSv4 mount uses `vers=4.0,port=PORT,actimeo=0` without mountport or nolock because NFSv4 is a stateful protocol that does not use the separate mount protocol
- setupNFSv4TestServer helper encapsulates server lifecycle (server+stores+share+adapter+wait) for DRY test setup
- Version-parameterized subtests (`for ver in ["3", "4.0"]`) with SkipIfNFSv4Unsupported guard at the v4 subtest level
- Pseudo-FS browsing test mounts "/" root to verify share junctions appear as directories
- TestNFSv4StaleHandle uses two separate server processes to simulate memory state loss

## Deviations from Plan

None - plan executed exactly as written.

## Issues Encountered
None.

## User Setup Required
None - no external service configuration required.

## Next Phase Readiness
- MountNFSWithVersion helper ready for use in all subsequent plans (15-02 through 15-05)
- Platform skip helpers ready for NFSv4-specific feature tests
- All existing v1.0 E2E tests confirmed to still compile (backward compat verified)

## Self-Check: PASSED

- All created/modified files exist on disk
- Commit a704041 (Task 1) verified in git log
- Commit d3bdc14 (Task 2) verified in git log
- `go build -tags=e2e ./test/e2e/...` compiles successfully
- `go vet -tags=e2e ./test/e2e/...` passes

---
*Phase: 15-v2-0-testing*
*Completed: 2026-02-17*
