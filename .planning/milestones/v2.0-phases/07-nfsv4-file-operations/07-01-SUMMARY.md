---
phase: 07-nfsv4-file-operations
plan: 01
subsystem: protocol
tags: [nfsv4, handlers, real-fs, getattr, lookup, readdir, access, readlink, compound]

# Dependency graph
requires:
  - phase: 06-nfsv4-pseudo-fs
    provides: pseudo-fs handlers (LOOKUP, LOOKUPP, GETATTR, READDIR, ACCESS), PseudoFS, attrs encoder
provides:
  - Real-FS operation handlers for LOOKUP, LOOKUPP, GETATTR, READDIR, ACCESS
  - READLINK handler for symlink resolution
  - buildV4AuthContext helper for identity mapping and permission context
  - EncodeRealFileAttrs for real file fattr4 encoding
  - MapFileTypeToNFS4 type conversion utility
  - encodeChangeInfo4 for directory change reporting
  - PseudoFS.FindJunction for cross-back navigation
affects: [07-02 (CREATE/REMOVE), 07-03 (READ/WRITE), 07-04 (RENAME/LINK)]

# Tech tracking
tech-stack:
  added: []
  patterns: [real-fs handler delegation, pseudo-fs/real-fs routing, auth context threading, junction cross-back]

key-files:
  created:
    - internal/protocol/nfs/v4/handlers/helpers.go
    - internal/protocol/nfs/v4/handlers/helpers_test.go
    - internal/protocol/nfs/v4/handlers/readlink.go
    - internal/protocol/nfs/v4/handlers/realfs_test.go
  modified:
    - internal/protocol/nfs/v4/attrs/encode.go
    - internal/protocol/nfs/v4/pseudofs/pseudofs.go
    - internal/protocol/nfs/v4/handlers/lookup.go
    - internal/protocol/nfs/v4/handlers/lookupp.go
    - internal/protocol/nfs/v4/handlers/getattr.go
    - internal/protocol/nfs/v4/handlers/readdir.go
    - internal/protocol/nfs/v4/handlers/access.go
    - internal/protocol/nfs/v4/handlers/handler.go

key-decisions:
  - "Use runtime.Runtime directly in Handler instead of extracting interface -- consistent with v3 pattern"
  - "LOOKUPP from share root crosses back to pseudo-fs junction via PseudoFS.FindJunction"
  - "ACCESS handler uses Unix permission triad checking (owner/group/other) with root bypass"
  - "READDIR uses MetadataService.ReadDirectory for paginated listing with per-entry fattr4"
  - "Real file FSID uses major=1, minor=SHA256(shareName) to distinguish from pseudo-fs FSID (0,1)"

patterns-established:
  - "buildV4AuthContext: central auth context builder for all real-FS handlers"
  - "Real-FS handler naming: h.xyzRealFS(ctx, ...) for methods only reached from real handles"
  - "getMetadataServiceForCtx/getPayloadServiceForCtx: nil-guarded service accessors"
  - "realFSTestFixture: test infrastructure with runtime.New(nil) + in-memory metadata store"

# Metrics
duration: 15min
completed: 2026-02-13
---

# Phase 7 Plan 1: NFSv4 File Operation Handlers Summary

**Real-FS routing for 6 NFSv4 handlers (LOOKUP, LOOKUPP, GETATTR, READDIR, ACCESS, READLINK) with auth context threading and fattr4 encoding**

## Performance

- **Duration:** 15 min
- **Started:** 2026-02-13T14:35:00Z
- **Completed:** 2026-02-13T14:50:13Z
- **Tasks:** 2
- **Files modified:** 12

## Accomplishments
- All 5 existing pseudo-fs handlers upgraded with real-FS code paths that route through MetadataService
- New READLINK handler resolves symlink targets from real metadata stores
- Shared buildV4AuthContext helper handles identity mapping, squashing, and permission resolution
- EncodeRealFileAttrs encodes all 18+ fattr4 attributes from metadata.File (type, size, mode, timestamps, ownership, link count)
- PseudoFS.FindJunction enables LOOKUPP to cross back from real-FS share root to pseudo-fs namespace
- 55 tests pass including 20 new real-FS tests and 5 pseudo-fs regression tests

## Task Commits

Each task was committed atomically:

1. **Task 1: Shared helpers and real file attribute encoder** - `37e13a3` (feat)
2. **Task 2: Upgrade 5 handlers for real-FS + READLINK** - `7e4c705` (feat)

## Files Created/Modified
- `internal/protocol/nfs/v4/handlers/helpers.go` - buildV4AuthContext, encodeChangeInfo4, service accessors
- `internal/protocol/nfs/v4/handlers/helpers_test.go` - Auth context and change_info4 encoding tests
- `internal/protocol/nfs/v4/handlers/readlink.go` - READLINK handler for symlink target resolution
- `internal/protocol/nfs/v4/handlers/realfs_test.go` - Comprehensive real-FS handler tests with in-memory store
- `internal/protocol/nfs/v4/attrs/encode.go` - EncodeRealFileAttrs, MapFileTypeToNFS4, SupportedRealAttrs
- `internal/protocol/nfs/v4/pseudofs/pseudofs.go` - FindJunction for share-to-junction resolution
- `internal/protocol/nfs/v4/handlers/lookup.go` - Real-FS lookup via MetadataService.Lookup
- `internal/protocol/nfs/v4/handlers/lookupp.go` - Real-FS parent navigation with pseudo-fs cross-back
- `internal/protocol/nfs/v4/handlers/getattr.go` - Real-FS attribute retrieval via MetadataService.GetFile
- `internal/protocol/nfs/v4/handlers/readdir.go` - Real-FS directory listing via MetadataService.ReadDirectory
- `internal/protocol/nfs/v4/handlers/access.go` - Real-FS permission checking with Unix triad model
- `internal/protocol/nfs/v4/handlers/handler.go` - Registered READLINK in dispatch table

## Decisions Made
- Used `runtime.Runtime` directly as the registry instead of introducing a new interface -- consistent with NFSv3 handler pattern and avoids abstraction churn
- LOOKUPP from share root crosses back to pseudo-fs by finding the junction node via `PseudoFS.FindJunction(shareName)` -- this maintains seamless navigation between virtual and real namespaces
- ACCESS handler implements Unix permission triad checking (owner/group/other bits) with root UID 0 bypass, matching NFSv3 behavior
- Real file FSID uses (major=1, minor=SHA256(shareName)) to distinguish from pseudo-fs FSID (0,1), ensuring clients detect filesystem boundaries correctly
- READDIR encodes per-entry attributes by constructing a temporary `metadata.File` from `DirEntry.Attr` and extracting the share name from the entry's handle

## Deviations from Plan

### Auto-fixed Issues

**1. [Rule 2 - Missing Critical] Added PseudoFS.FindJunction method**
- **Found during:** Task 1 (helpers and attribute encoder)
- **Issue:** LOOKUPP needs to cross back from real-FS share root to pseudo-fs junction, but PseudoFS had no method to find the junction node for a share name
- **Fix:** Added `FindJunction(shareName string) (*PseudoNode, bool)` method that searches byHandle map for matching export node
- **Files modified:** `internal/protocol/nfs/v4/pseudofs/pseudofs.go`
- **Verification:** TestLookupP_RealFS_ShareRootCrossesToPseudoFS passes
- **Committed in:** 37e13a3 (Task 1 commit)

---

**Total deviations:** 1 auto-fixed (1 missing critical)
**Impact on plan:** Essential for LOOKUPP cross-back correctness. No scope creep.

## Issues Encountered
None

## User Setup Required
None - no external service configuration required.

## Next Phase Readiness
- Real-FS navigation layer complete -- clients can browse real file trees via COMPOUND ops
- Ready for Plan 02 (CREATE, REMOVE, MKDIR, RMDIR) which will use buildV4AuthContext and encodeChangeInfo4
- Ready for Plan 03 (READ/WRITE) which will use getPayloadServiceForCtx

---
*Phase: 07-nfsv4-file-operations*
*Completed: 2026-02-13*

## Self-Check: PASSED
