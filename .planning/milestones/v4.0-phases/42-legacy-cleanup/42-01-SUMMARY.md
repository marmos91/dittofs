---
phase: 42-legacy-cleanup
plan: 01
subsystem: payload
tags: [blockstore, filesystem, cleanup, direct-write, cache]

# Dependency graph
requires:
  - phase: 41-block-state-enum-and-listfileblocks
    provides: Block state enum replacing direct-write detection
provides:
  - Removed DirectWriteStore interface and fs/ package
  - Removed all direct-write code paths from cache and offloader
  - Cleaned CLI and E2E tests to reflect memory/s3-only store types
affects: [43-blockstore-gc, payload-store-plugins]

# Tech tracking
tech-stack:
  added: []
  patterns:
    - "Deprecation error in init.go case for removed store types"
    - "Pure memory/s3 payload store model"

key-files:
  created: []
  modified:
    - pkg/payload/store/store.go
    - pkg/cache/cache.go
    - pkg/cache/read.go
    - pkg/cache/write.go
    - pkg/cache/flush.go
    - pkg/payload/offloader/offloader.go
    - pkg/payload/offloader/upload.go
    - pkg/controlplane/runtime/init.go
    - test/e2e/store_matrix_test.go
    - cmd/dfsctl/commands/store/payload/add.go

key-decisions:
  - "Keep filesystem case in init.go returning explicit v4.0 removal error for upgrade guidance"
  - "Convert gc_integration_test.go filesystem tests to memory rather than deleting them"

patterns-established:
  - "Deprecation error pattern: case returning fmt.Errorf with version and alternatives"

requirements-completed: [CLEAN-01, CLEAN-02, CLEAN-03, CLEAN-04, CLEAN-05, CLEAN-06]

# Metrics
duration: 25min
completed: 2026-03-09
---

# Phase 42 Plan 01: Legacy Cleanup Summary

**Removed DirectWriteStore interface, filesystem payload store, and all direct-write code paths (-1305 lines net)**

## Performance

- **Duration:** ~25 min
- **Started:** 2026-03-09T13:23:00Z
- **Completed:** 2026-03-09T13:48:01Z
- **Tasks:** 2
- **Files modified:** 21

## Accomplishments
- Deleted `pkg/payload/store/fs/` directory entirely (~925 lines of store + tests)
- Removed DirectWriteStore interface from `store.go` and all consuming code
- Removed `directWritePath`, `SetDirectWritePath()`, `IsDirectWrite()` from BlockCache
- Removed direct-write branches from cache read, write, and flush paths
- Removed direct-write early returns from offloader
- Cleaned E2E test matrix from 9 to 6 configurations (removed 3 filesystem entries)
- Removed filesystem store type from CLI add/edit commands
- Net deletion of ~1305 lines of code

## Task Commits

Each task was committed atomically:

1. **Task 1: Remove DirectWriteStore, filesystem store, and direct-write code paths** - `604f4e58` (feat)
2. **Task 2: Clean up E2E tests, CLI commands, and stale comments** - `42ddbdf9` (chore)

## Files Created/Modified
- `pkg/payload/store/fs/` - DELETED (entire directory)
- `pkg/payload/store/store.go` - Removed DirectWriteStore interface
- `pkg/cache/cache.go` - Removed directWritePath field and methods
- `pkg/cache/read.go` - Removed direct-write path resolution branch
- `pkg/cache/write.go` - Removed direct-write path resolution branch
- `pkg/cache/flush.go` - Removed direct-write flush branches
- `pkg/payload/offloader/offloader.go` - Removed direct-write early return in Flush
- `pkg/payload/offloader/upload.go` - Removed direct-write guard in uploadPendingBlocks
- `pkg/payload/offloader/offloader_test.go` - Removed filesystem test functions and benchmarks
- `pkg/controlplane/runtime/init.go` - Replaced filesystem store creation with deprecation error
- `pkg/payload/gc/gc_integration_test.go` - Converted filesystem tests to memory
- `test/e2e/store_matrix_test.go` - Removed 3 filesystem matrix entries
- `test/e2e/nfsv4_store_matrix_test.go` - Removed filesystem case
- `test/e2e/payload_stores_test.go` - Removed filesystem CRUD tests
- `test/e2e/nfsv4_recovery_test.go` - Changed from filesystem to memory stores
- `cmd/dfsctl/commands/store/payload/add.go` - Removed filesystem type/path/examples
- `cmd/dfsctl/commands/store/payload/edit.go` - Removed filesystem type/path editing
- `cmd/dfsctl/commands/store/payload/payload.go` - Updated supported types list
- `pkg/payload/errors.go` - Updated Backend doc comment
- `internal/adapter/smb/v2/handlers/flush.go` - Removed stale filesystem comment

## Decisions Made
- **Deprecation error for filesystem type:** Kept `case "filesystem":` in `init.go` returning a clear error message mentioning v4.0 removal and alternatives (memory/s3), rather than falling through to "unknown type". This provides upgrade guidance for existing configurations.
- **Convert rather than delete GC integration tests:** Converted `gc_integration_test.go` filesystem tests to use memory block store instead of deleting them, preserving test coverage for GC logic.

## Deviations from Plan

### Auto-fixed Issues

**1. [Rule 3 - Blocking] Fixed gc_integration_test.go importing deleted fs package**
- **Found during:** Task 1 (filesystem store removal)
- **Issue:** `pkg/payload/gc/gc_integration_test.go` imported `pkg/payload/store/fs` which was deleted, causing build failure
- **Fix:** Converted filesystem tests to memory block store tests, replaced `fs` import with `blockmemory`, removed unused `path/filepath` import
- **Files modified:** `pkg/payload/gc/gc_integration_test.go`
- **Verification:** `go build ./...` and `go test ./pkg/payload/gc/...` pass
- **Committed in:** `604f4e58` (Task 1 commit)

**2. [Rule 1 - Bug] Fixed undefined addPath variable in CLI add command**
- **Found during:** Task 2 (CLI cleanup)
- **Issue:** Removed `addPath` variable declaration but left reference in `buildPayloadConfig()` call, causing compilation error
- **Fix:** Replaced `addPath` with `""` in the function call (the path parameter is unused `_` in the function signature)
- **Files modified:** `cmd/dfsctl/commands/store/payload/add.go`
- **Verification:** `go build ./...` passes
- **Committed in:** `42ddbdf9` (Task 2 commit)

---

**Total deviations:** 2 auto-fixed (1 blocking, 1 bug)
**Impact on plan:** Both auto-fixes necessary for correct compilation. No scope creep.

## Issues Encountered
None beyond the auto-fixed deviations.

## User Setup Required
None - no external service configuration required.

## Next Phase Readiness
- All payload store types are now exclusively memory and s3
- BlockStore interface is clean and ready for GC implementation (phase 43)
- No filesystem-related code paths remain in cache, offloader, or protocol handlers

## Self-Check: PASSED

- Commit `604f4e58`: FOUND
- Commit `42ddbdf9`: FOUND
- SUMMARY.md: FOUND
- `pkg/payload/store/fs/` deleted: VERIFIED

---
*Phase: 42-legacy-cleanup*
*Completed: 2026-03-09*
