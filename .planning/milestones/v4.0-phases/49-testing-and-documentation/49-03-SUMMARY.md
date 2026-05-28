---
phase: 49-testing-and-documentation
plan: 03
subsystem: testing
tags: [e2e, store-matrix, multi-share, isolation, block-store]

# Dependency graph
requires:
  - phase: 49-01
    provides: Cache CLI and REST API
  - phase: 49-02
    provides: Block store terminology rename
provides:
  - "18-combo 3D store matrix (3 metadata x 2 local x 3 remote)"
  - "Shared matrix_config_test.go for NFSv3 and NFSv4 tests"
  - "Multi-share isolation E2E tests (6 subtests)"
  - "run-e2e.sh --local-only and --with-remote flags"
  - "WithShareRemote helper for tiered storage share creation"
affects: [49-05-documentation-update]

# Tech tracking
tech-stack:
  added: []
  patterns:
    - 3D store matrix with short mode filtering
    - DITTOFS_E2E_LOCAL_ONLY env var for remote filtering
    - Per-share isolation validation pattern

key-files:
  created:
    - test/e2e/matrix_config_test.go
    - test/e2e/multi_share_isolation_test.go
  modified:
    - test/e2e/store_matrix_test.go
    - test/e2e/nfsv4_store_matrix_test.go
    - test/e2e/block_stores_test.go
    - test/e2e/helpers/shares.go
    - test/e2e/run-e2e.sh

key-decisions:
  - "matrixStoreConfig type replaces old storeConfig to support 3D matrix"
  - "getStoreMatrix() function centralizes short-mode and local-only filtering"
  - "WithShareRemote option added to share helpers for tiered storage"

patterns-established:
  - "3D matrix: metadata x local x remote with env-var filtering"
  - "Multi-share isolation: data, deletion, concurrent, cache, cross-protocol"

requirements-completed: [TEST-01, TEST-02]

# Metrics
duration: 10min
completed: 2026-03-10
---

# Phase 49 Plan 03: E2E Store Matrix & Multi-Share Isolation Summary

**18-combo 3D store matrix (memory/badger/postgres x fs/memory x none/memory/s3) with multi-share isolation tests covering data isolation, deletion isolation, concurrent writes, cache independence, and cross-protocol visibility**

## Performance

- **Duration:** 10 min
- **Started:** 2026-03-10T16:38:20Z
- **Completed:** 2026-03-10T16:48:20Z
- **Tasks:** 2
- **Files modified:** 8

## Accomplishments
- 18-combo 3D store matrix defined in shared matrix_config_test.go
- NFSv3 matrix test rewritten with expanded ops (rename, truncate, append)
- NFSv4 matrix test uses same shared matrix with version parameterization
- Block stores CRUD test expanded with remote store and error cases
- Multi-share isolation test with 6 subtests covering all isolation properties
- run-e2e.sh supports --local-only and --with-remote flags
- Short mode runs 4 representative combos instead of all 18

## Task Commits

1. **Task 1: 18-combo store matrix** - `6dfae495` (feat)
2. **Task 2: Multi-share isolation tests** - `340da790` (feat)

## Files Created/Modified
- `test/e2e/matrix_config_test.go` - Shared 3D matrix definition (matrixStoreConfig, storeMatrix3D, shortMatrix3D)
- `test/e2e/store_matrix_test.go` - NFSv3 18-combo matrix with expanded file ops
- `test/e2e/nfsv4_store_matrix_test.go` - NFSv4 18-combo matrix with version parameterization
- `test/e2e/block_stores_test.go` - Local + remote CRUD tests with error cases
- `test/e2e/multi_share_isolation_test.go` - 6 isolation subtests (data, deletion, concurrent, cache, cross-protocol, same-remote)
- `test/e2e/helpers/shares.go` - Added WithShareRemote option
- `test/e2e/run-e2e.sh` - Added --local-only and --with-remote flags

## Decisions Made
- matrixStoreConfig type replaces old 2D storeConfig to support the 3D matrix (metadata x local x remote)
- getStoreMatrix() centralizes short-mode and DITTOFS_E2E_LOCAL_ONLY env var filtering
- WithShareRemote option added to share helpers, wiring the --remote flag on share create

## Deviations from Plan

### Auto-fixed Issues

**1. [Rule 1 - Bug] Removed unused context import in multi_share_isolation_test.go**
- **Found during:** Task 2 verification
- **Issue:** go vet flagged unused context import
- **Fix:** Removed unused import
- **Files modified:** test/e2e/multi_share_isolation_test.go
- **Committed in:** 340da790

---

**Total deviations:** 1 auto-fixed (1 bug)
**Impact on plan:** Trivial fix, no scope creep.

## Issues Encountered
None

## User Setup Required
None - no external service configuration required.

## Next Phase Readiness
- Store matrix covers all 18 backend combinations
- Multi-share isolation validates per-share BlockStore independence
- Ready for Plan 04 (SMB matrix tests) and Plan 05 (documentation)

---
*Phase: 49-testing-and-documentation*
*Completed: 2026-03-10*
