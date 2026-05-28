---
phase: 29-core-layer-decomposition
plan: 03
subsystem: metadata
tags: [refactoring, file-split, go-packages, metadata-service]

# Dependency graph
requires:
  - phase: 29-01
    provides: "foundational error types and generic helpers"
provides:
  - "file.go split into 5 focused files (file_create.go, file_modify.go, file_remove.go, file_helpers.go, file_types.go)"
  - "authentication.go split into 2 focused files (auth_identity.go, auth_permissions.go)"
affects: [29-04, 29-05, 29-06]

# Tech tracking
tech-stack:
  added: []
  patterns:
    - "flat file split within same package to avoid Go circular imports"
    - "operation-based file naming convention: {domain}_{operation}.go"

key-files:
  created:
    - pkg/metadata/file_create.go
    - pkg/metadata/file_modify.go
    - pkg/metadata/file_remove.go
    - pkg/metadata/file_helpers.go
    - pkg/metadata/file_types.go
    - pkg/metadata/auth_identity.go
    - pkg/metadata/auth_permissions.go
  modified: []

key-decisions:
  - "Flat file split (same package) instead of sub-packages to avoid Go circular imports"
  - "Operation-based naming: file_create.go, file_modify.go, auth_identity.go, auth_permissions.go"
  - "Type definitions in file_types.go (File, FileAttr, SetAttrs, FileType, PayloadID)"
  - "No sub-package Service types or interfaces needed — methods stay on MetadataService"

patterns-established:
  - "Flat file split: split large files within the same Go package rather than creating sub-packages when methods reference parent types heavily"
  - "Domain_operation naming: {domain}_{operation}.go groups related operations (e.g., file_create.go, auth_permissions.go)"

requirements-completed: [REF-06.8, REF-06.9]

# Metrics
duration: 12min
completed: 2026-02-26
---

# Phase 29 Plan 03: MetadataService File Splits Summary

**Split file.go (1217 lines) and authentication.go (796 lines) into 7 focused files within pkg/metadata/ using flat file split pattern**

## Performance

- **Duration:** 12 min
- **Started:** 2026-02-26T10:24:19Z
- **Completed:** 2026-02-26T10:36:00Z
- **Tasks:** 2
- **Files modified:** 8

## Accomplishments
- Split file.go (1217 lines) into 5 focused files: file_create.go, file_modify.go, file_remove.go, file_helpers.go, file_types.go
- Split authentication.go (796 lines) into 2 focused files: auth_identity.go, auth_permissions.go
- Zero test failures, zero build issues, zero import changes needed
- Original files fully removed (no dead code remaining)

## Task Commits

Each task was committed atomically:

1. **Task 1: Split file.go into focused operation files** - `dc5d81f6` (refactor)
2. **Task 2: Split authentication.go into focused operation files** - `bf1c1bd9` (refactor)

## Files Created/Modified
- `pkg/metadata/file_create.go` - CreateFile, CreateSymlink, CreateSpecialFile, CreateHardLink, createEntry
- `pkg/metadata/file_modify.go` - Lookup, ReadSymlink, SetFileAttributes, Move, MarkFileAsOrphaned
- `pkg/metadata/file_remove.go` - RemoveFile
- `pkg/metadata/file_helpers.go` - buildPath, buildPayloadID, MakeRdev, RdevMajor, RdevMinor, GetInitialLinkCount
- `pkg/metadata/file_types.go` - File, FileAttr, SetAttrs, FileType, PayloadID type definitions
- `pkg/metadata/auth_identity.go` - AuthContext, Identity, HasGID, IdentityMapping, ApplyIdentityMapping, IsAdministratorSID, MatchesIPPattern, CopyFileAttr
- `pkg/metadata/auth_permissions.go` - Permission types, AccessDecision, CheckShareAccess, checkFilePermissions, calculatePermissions, ACL evaluation, check{Write,Read,Execute}Permission

## Decisions Made
- **Flat file split instead of sub-packages**: The plan specified creating `pkg/metadata/file/` and `pkg/metadata/auth/` sub-packages. However, Go forbids circular imports entirely — the sub-packages would need to import the parent for types (FileHandle, File, FileAttr, StoreError, etc.) while the parent imports the sub-packages for the Service types, creating an import cycle. Moving types to a separate `pkg/metadata/types/` package would require updating 50+ files across the codebase. The pragmatic solution: split files within the same `pkg/metadata/` package, achieving the organizational goal without any import changes.
- **No Service type or interface extraction needed**: Since methods stay on the existing MetadataService receiver, no new Service types, interfaces, or composite embedding were needed.
- **Type definitions extracted to file_types.go**: File, FileAttr, SetAttrs, FileType, and PayloadID grouped in their own file for clear separation from operation code.

## Deviations from Plan

### Auto-fixed Issues

**1. [Rule 3 - Blocking] Flat file split instead of sub-package creation**
- **Found during:** Task 1 (file.go split)
- **Issue:** Creating sub-packages (pkg/metadata/file/, pkg/metadata/auth/) would cause Go circular imports. The file sub-package methods are all on MetadataService and use parent types extensively (File, FileHandle, FileAttr, StoreError, MetadataStore, Transaction, AuthContext). The parent would import the child for Service embedding while the child imports the parent for types — a cycle Go strictly forbids.
- **Fix:** Split into multiple files within the same pkg/metadata/ package instead of sub-packages. This achieves the organizational goal (focused files by responsibility) without any import changes across the codebase.
- **Files modified:** All 7 new files created, 2 original files deleted
- **Verification:** `go build ./...`, `go test ./pkg/metadata/... -count=1`, `go vet ./pkg/metadata/...` all pass
- **Committed in:** dc5d81f6, bf1c1bd9

---

**Total deviations:** 1 auto-fixed (1 blocking)
**Impact on plan:** The organizational goal is fully achieved — large files are split into focused operation files. The difference is structural (same package vs sub-packages) but the developer experience of navigating the code is identical. No scope creep.

## Issues Encountered
None.

## User Setup Required
None - no external service configuration required.

## Next Phase Readiness
- pkg/metadata/ now has focused, navigable files (largest is file_modify.go at ~450 lines)
- Ready for Plan 04 (ControlPlane Store interface decomposition)
- No blockers or concerns

## Self-Check: PASSED

- All 7 created files verified present
- Both original files (file.go, authentication.go) verified deleted
- Both task commits (dc5d81f6, bf1c1bd9) verified in git history

---
*Phase: 29-core-layer-decomposition*
*Completed: 2026-02-26*
