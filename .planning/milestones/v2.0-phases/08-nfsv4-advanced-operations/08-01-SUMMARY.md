---
phase: 08-nfsv4-advanced-operations
plan: 01
subsystem: nfs
tags: [nfsv4, link, rename, two-filehandle-pattern, compound-operations]

# Dependency graph
requires:
  - phase: 06-nfsv4-protocol-foundation
    provides: "COMPOUND dispatcher, SAVEFH/RESTOREFH, CompoundContext with SavedFH/CurrentFH"
  - phase: 07-nfsv4-file-operations
    provides: "buildV4AuthContext, encodeChangeInfo4, getMetadataServiceForCtx, newRealFSTestFixture"
provides:
  - "NFSv4 LINK operation handler (handleLink) with two-filehandle pattern"
  - "NFSv4 RENAME operation handler (handleRename) with dual change_info4"
  - "Cross-share detection pattern via DecodeFileHandle share name comparison"
affects: [08-02, 08-03, phase-09-nfsv4-state-management]

# Tech tracking
tech-stack:
  added: []
  patterns:
    - "Two-filehandle pattern: SavedFH + CurrentFH for cross-handle operations"
    - "Cross-share XDEV detection via metadata.DecodeFileHandle share name comparison"
    - "Dual change_info4 encoding for RENAME (source + target directories)"

key-files:
  created:
    - internal/protocol/nfs/v4/handlers/link.go
    - internal/protocol/nfs/v4/handlers/rename.go
    - internal/protocol/nfs/v4/handlers/link_rename_test.go
  modified:
    - internal/protocol/nfs/v4/handlers/handler.go

key-decisions:
  - "Cross-share check uses DecodeFileHandle to compare share names before calling MetadataService"
  - "LINK only checks CurrentFH for pseudo-fs (target dir); RENAME checks both handles"
  - "Auth context built from CurrentFH (target directory) for both operations"

patterns-established:
  - "Two-filehandle pattern: require both CurrentFH and SavedFH, check pseudo-fs, validate names, cross-share check, then delegate to MetadataService"
  - "Test helpers: setCurrentFH/setSavedFH for handle isolation, parseChangeInfo4 for response parsing"

# Metrics
duration: 6min
completed: 2026-02-13
---

# Phase 8 Plan 1: LINK and RENAME Summary

**NFSv4 LINK and RENAME handlers using SavedFH/CurrentFH two-filehandle pattern with cross-share detection and 21 tests**

## Performance

- **Duration:** 6 min
- **Started:** 2026-02-13T21:25:39Z
- **Completed:** 2026-02-13T21:31:30Z
- **Tasks:** 1
- **Files modified:** 4

## Accomplishments
- LINK handler creates hard links via MetadataService.CreateHardLink with SavedFH as source file and CurrentFH as target directory
- RENAME handler moves/renames files via MetadataService.Move with dual change_info4 for both source and target directories
- Cross-share detection prevents LINK/RENAME across different shares (NFS4ERR_XDEV)
- Both operations registered in COMPOUND dispatch table (OP_LINK, OP_RENAME)
- 21 tests covering all success paths, error conditions, and compound sequences

## Task Commits

Each task was committed atomically:

1. **Task 1: LINK and RENAME handlers with dispatch registration and tests** - `9db65ec` (feat)

## Files Created/Modified
- `internal/protocol/nfs/v4/handlers/link.go` - LINK operation handler with two-filehandle pattern
- `internal/protocol/nfs/v4/handlers/rename.go` - RENAME operation handler with dual change_info4
- `internal/protocol/nfs/v4/handlers/link_rename_test.go` - 21 tests for LINK and RENAME operations
- `internal/protocol/nfs/v4/handlers/handler.go` - Dispatch table registration for OP_LINK and OP_RENAME

## Decisions Made
- Cross-share check uses `metadata.DecodeFileHandle()` to extract and compare share names from both SavedFH and CurrentFH before calling MetadataService -- consistent with how the metadata layer routes operations
- LINK only checks CurrentFH for pseudo-fs read-only rejection (SavedFH is the source file, not a directory in pseudo-fs context); RENAME checks both handles since both are directories
- Auth context is built from CurrentFH (target directory) for both operations, consistent with RFC 7530 semantics where the target directory owns the permission context

## Deviations from Plan

None - plan executed exactly as written.

## Issues Encountered

None.

## User Setup Required

None - no external service configuration required.

## Next Phase Readiness
- LINK and RENAME complete the filesystem manipulation set for NFSv4
- Two-filehandle pattern established and tested for future operations requiring SavedFH/CurrentFH
- Ready for Plan 08-02 (SETATTR + fattr4 decode infrastructure)

## Self-Check: PASSED

- [x] link.go exists
- [x] rename.go exists
- [x] link_rename_test.go exists
- [x] Commit 9db65ec verified

---
*Phase: 08-nfsv4-advanced-operations*
*Completed: 2026-02-13*
