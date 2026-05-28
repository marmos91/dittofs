---
phase: 07-nfsv4-file-operations
plan: 02
subsystem: protocol
tags: [nfsv4, create, remove, rfc7530, xdr, change_info4]

# Dependency graph
requires:
  - phase: 07-01
    provides: "NFSv4 pseudo-fs navigation handlers, helper infrastructure"
provides:
  - "NFSv4 CREATE handler for directories and symlinks"
  - "NFSv4 REMOVE handler for files and empty directories"
  - "createtype4, createmode4, OPEN, and stability constants"
  - "change_info4 encoding for parent directory cache coherency"
affects: [07-03, 07-04, 07-05]

# Tech tracking
tech-stack:
  added: []
  patterns:
    - "skipFattr4 pattern for consuming unused fattr4 from XDR stream"
    - "RemoveFile-then-RemoveDirectory fallback for unified REMOVE"

key-files:
  created:
    - internal/protocol/nfs/v4/handlers/create.go
    - internal/protocol/nfs/v4/handlers/remove.go
    - internal/protocol/nfs/v4/handlers/create_remove_test.go
  modified:
    - internal/protocol/nfs/v4/handlers/handler.go
    - internal/protocol/nfs/v4/types/constants.go

key-decisions:
  - "Regular file creation returns NFS4ERR_BADTYPE -- files are created via OPEN"
  - "Block/char devices, sockets, FIFOs return NFS4ERR_NOTSUPP (not in metadata layer)"
  - "REMOVE tries RemoveFile first, falls back to RemoveDirectory on ErrIsDirectory"
  - "Added OPEN/claim/delegation/stability constants proactively for Plan 07-03"

patterns-established:
  - "skipFattr4: skip fattr4 bitmap+opaque without full parsing"
  - "Unified REMOVE: try file removal, fallback to directory on IsDirectory error"

# Metrics
duration: 8min
completed: 2026-02-13
---

# Phase 07 Plan 02: CREATE/REMOVE Operations Summary

**NFSv4 CREATE for directories/symlinks and REMOVE for files/directories with change_info4 cache coherency, plus OPEN/stability constants for Plan 07-03**

## Performance

- **Duration:** 8 min
- **Started:** 2026-02-13T14:54:42Z
- **Completed:** 2026-02-13T15:02:52Z
- **Tasks:** 1
- **Files modified:** 5

## Accomplishments
- CREATE handler supports NF4DIR (directories) and NF4LNK (symlinks) via MetadataService delegation
- REMOVE handler supports both files and empty directories with automatic type detection
- Both operations return change_info4 for NFSv4 client cache coherency
- Added 80+ constants (createtype4, createmode4, OPEN share/claim/delegation, write stability) for Plan 07-03
- 15 unit tests covering success paths, error codes, pseudo-fs rejection, and edge cases

## Task Commits

Each task was committed atomically:

1. **Task 1: CREATE and REMOVE handlers with constants and tests** - `9277289` (feat)

**Plan metadata:** `c6fcbbc` (docs: complete plan)

## Files Created/Modified
- `internal/protocol/nfs/v4/handlers/create.go` - CREATE operation handler (NF4DIR, NF4LNK, type validation)
- `internal/protocol/nfs/v4/handlers/remove.go` - REMOVE operation handler (file + directory with fallback)
- `internal/protocol/nfs/v4/handlers/create_remove_test.go` - 15 tests for both operations
- `internal/protocol/nfs/v4/handlers/handler.go` - Registered OP_CREATE and OP_REMOVE in dispatch table
- `internal/protocol/nfs/v4/types/constants.go` - createtype4, createmode4, OPEN, stability constants

## Decisions Made
- Regular file creation via CREATE returns NFS4ERR_BADTYPE per RFC 7530 (regular files must use OPEN)
- Block/char devices, sockets, FIFOs return NFS4ERR_NOTSUPP since DittoFS metadata layer does not model these types
- REMOVE uses a try-file-then-directory pattern to avoid needing to look up file type before removal
- Added OPEN constants (share_access, share_deny, claim types, delegation, stability) proactively for Plan 07-03

## Deviations from Plan

None - plan executed exactly as written.

## Issues Encountered
None

## User Setup Required
None - no external service configuration required.

## Next Phase Readiness
- CREATE and REMOVE handlers are registered and fully operational
- OPEN constants are in place for Plan 07-03 (OPEN/CLOSE/stateid operations)
- change_info4 encoding pattern established for RENAME (Plan 07-04)

## Self-Check: PASSED

All files verified present on disk. Commit 9277289 verified in git log.

---
*Phase: 07-nfsv4-file-operations*
*Completed: 2026-02-13*
