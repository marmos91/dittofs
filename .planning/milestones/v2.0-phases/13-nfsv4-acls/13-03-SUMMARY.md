---
phase: 13-nfsv4-acls
plan: 03
subsystem: metadata
tags: [acl, nfsv4, permissions, inheritance, postgresql, identity-mapping, controlplane]

# Dependency graph
requires:
  - phase: 13-nfsv4-acls
    plan: 01
    provides: "ACL types, evaluation engine, validation, mode sync, inheritance"
  - phase: 13-nfsv4-acls
    plan: 02
    provides: "Identity mapper package with IdentityMapper interface"
provides:
  - "FileAttr with ACL field persisted across all store backends"
  - "ACL evaluation in calculatePermissions (nil=Unix, non-nil=ACL)"
  - "ACL inheritance from parent in createEntry"
  - "chmod adjusts OWNER@/GROUP@/EVERYONE@ ACEs when ACL present"
  - "PostgreSQL migration 000004 with ACL JSONB column"
  - "Identity mapping CRUD in controlplane store"
affects: [13-04, 13-05, nfsv4-handlers, smb-security-descriptor]

# Tech tracking
tech-stack:
  added: []
  patterns: [acl-precedence-over-unix-mode, jsonb-acl-storage, identity-mapping-crud]

key-files:
  created:
    - pkg/metadata/store/postgres/migrations/000004_acl.up.sql
    - pkg/metadata/store/postgres/migrations/000004_acl.down.sql
    - pkg/controlplane/models/identity_mapping.go
    - pkg/controlplane/store/identity_mappings.go
  modified:
    - pkg/metadata/file.go
    - pkg/metadata/authentication.go
    - pkg/metadata/store/postgres/encoding.go
    - pkg/metadata/store/postgres/files.go
    - pkg/metadata/store/postgres/transaction.go
    - pkg/controlplane/models/models.go
    - pkg/controlplane/store/interface.go

key-decisions:
  - "ACL field on FileAttr: nil=Unix mode check, non-nil=ACL evaluation, empty ACEs=deny all"
  - "ACL evaluation takes precedence over Unix mode in calculatePermissions"
  - "Root (UID 0) bypasses ACL checks, matching Unix permission model"
  - "PostgreSQL stores ACL as JSONB column with partial index for ACL presence queries"
  - "Memory and BadgerDB get ACL support automatically via JSON serialization of FileAttr"
  - "IdentityMapping GORM model with principal uniqueIndex for O(1) lookup"

patterns-established:
  - "ACL-first permission check: calculatePermissions branches on attr.ACL != nil"
  - "evaluateWithACL maps each Permission flag to ACE mask bits independently"
  - "JSONB for flexible ACL storage in PostgreSQL (no schema coupling to ACE structure)"

# Metrics
duration: 7min
completed: 2026-02-16
---

# Phase 13 Plan 03: ACL Metadata Integration Summary

**ACL field integrated into FileAttr with permission evaluation, inheritance at creation, chmod sync, PostgreSQL JSONB persistence, and identity mapping CRUD in controlplane store**

## Performance

- **Duration:** 7 min
- **Started:** 2026-02-16T08:28:13Z
- **Completed:** 2026-02-16T08:36:11Z
- **Tasks:** 2
- **Files modified:** 13 (4 created + 9 modified)

## Accomplishments
- FileAttr has `ACL *acl.ACL` field with nil/non-nil/empty semantics for Unix fallback, ACL evaluation, and deny-all
- calculatePermissions branches on ACL presence: evaluateACLPermissions maps Permission flags to NFSv4 ACE mask bits
- createEntry inherits ACL from parent via acl.ComputeInheritedACL at file/directory creation
- chmod (SetFileAttributes Mode change) adjusts OWNER@/GROUP@/EVERYONE@ ACEs via acl.AdjustACLForMode
- SetAttrs supports ACL with validation via acl.ValidateACL before applying
- CopyFileAttr deep-copies ACL to prevent external mutation
- PostgreSQL migration 000004 adds ACL JSONB column with partial index
- PostgreSQL encoding, queries, and PutFile updated for ACL read/write
- IdentityMapping GORM model with controlplane store CRUD (Get, List, Create, Delete)

## Task Commits

Each task was committed atomically:

1. **Task 1: FileAttr ACL Extension, Permission Check Integration, and Inheritance** - `f933823` (feat) -- pre-existing commit on branch
2. **Task 2: PostgreSQL Migration and Controlplane Identity Mapping CRUD** - `20cfdf5` (feat)

## Files Created/Modified
- `pkg/metadata/file.go` - ACL field on FileAttr/SetAttrs, inheritance in createEntry, chmod sync
- `pkg/metadata/authentication.go` - ACL evaluation branch, evaluateACLPermissions, evaluateWithACL, CopyFileAttr ACL deep copy
- `pkg/metadata/store/postgres/migrations/000004_acl.up.sql` - ACL JSONB column + partial index
- `pkg/metadata/store/postgres/migrations/000004_acl.down.sql` - Rollback migration
- `pkg/metadata/store/postgres/encoding.go` - ACL JSONB unmarshal in fileRowToFileWithNlink
- `pkg/metadata/store/postgres/files.go` - Updated SELECT queries to include f.acl column
- `pkg/metadata/store/postgres/transaction.go` - ACL JSONB marshal in PutFile, updated SELECTs
- `pkg/controlplane/models/identity_mapping.go` - IdentityMapping GORM model
- `pkg/controlplane/models/models.go` - Added IdentityMapping to AllModels()
- `pkg/controlplane/store/interface.go` - 4 identity mapping CRUD methods on Store interface
- `pkg/controlplane/store/identity_mappings.go` - GORM implementation of identity mapping CRUD

## Decisions Made
- ACL field uses `*acl.ACL` pointer: nil = no ACL (use Unix mode), non-nil = use ACL evaluation
- Root bypass preserved in ACL evaluation path (UID 0 gets all permissions)
- Anonymous users (nil identity) evaluated as EVERYONE@ only when ACL present
- PostgreSQL uses JSONB for ACL storage (flexible, no schema coupling to ACE structure)
- Memory and BadgerDB stores get ACL support automatically via JSON serialization
- IdentityMapping uses UUID primary key generated on Create if empty

## Deviations from Plan

### Auto-fixed Issues

**1. [Rule 3 - Blocking] Task 1 changes already present on branch**
- **Found during:** Task 1 (FileAttr ACL Extension)
- **Issue:** Commit f933823 on the branch already contained all Task 1 changes (ACL field, permission evaluation, inheritance, chmod sync, CopyFileAttr)
- **Fix:** Verified existing commit contained correct implementation, proceeded to Task 2
- **Files:** pkg/metadata/file.go, pkg/metadata/authentication.go
- **Impact:** None -- identical implementation to plan specification

---

**Total deviations:** 1 (pre-existing code)
**Impact on plan:** No impact. Task 1 was already implemented correctly.

## Issues Encountered
None.

## User Setup Required
None - no external service configuration required.

## Next Phase Readiness
- FileAttr.ACL field ready for NFSv4 GETATTR/SETATTR wire encoding (Plan 13-04)
- Identity mapping CRUD ready for control plane API handlers (Plan 13-05)
- PostgreSQL ACL storage tested via build/vet (runtime tests require PostgreSQL instance)
- All existing tests continue to pass (no regressions)

## Self-Check: PASSED

All created/modified files verified on disk. Task 2 commit (20cfdf5) verified in git log. Task 1 commit (f933823) verified as pre-existing on branch. Full project builds, all tests pass with -race, go vet clean.

---
*Phase: 13-nfsv4-acls*
*Completed: 2026-02-16*
