---
phase: 37-smb3-leases-and-directory-leasing
plan: 03
subsystem: metadata
tags: [directory-leasing, dir-change-notifier, cross-protocol, lock-manager, nfs4-delegations]

# Dependency graph
requires:
  - phase: 37-01
    provides: "DirChangeNotifier interface, LockManager.OnDirChange, BreakCallbacks dispatch"
provides:
  - "MetadataService wired with per-share DirChangeNotifier (auto-wired via LockManager)"
  - "All mutation methods (CreateFile, RemoveFile, Move, CreateDirectory, RemoveDirectory) notify directory changes"
  - "NFS4 handlers simplified - no direct NotifyDirChange except setattr (NFS4-specific)"
  - "Unified notification flow: MetadataService -> LockManager -> BreakCallbacks -> {NFS4, SMB}"
affects: [37-02, cross-protocol-leasing, smb-directory-leases]

# Tech tracking
tech-stack:
  added: []
  patterns: [fire-and-forget-notification, per-share-notifier-map, auto-wire-in-register]

key-files:
  created: []
  modified:
    - pkg/metadata/service.go
    - pkg/metadata/file_create.go
    - pkg/metadata/file_remove.go
    - pkg/metadata/file_modify.go
    - pkg/metadata/directory.go
    - internal/adapter/nfs/v4/handlers/create.go
    - internal/adapter/nfs/v4/handlers/remove.go
    - internal/adapter/nfs/v4/handlers/rename.go
    - internal/adapter/nfs/v4/handlers/link.go
    - internal/adapter/nfs/v4/handlers/open.go

key-decisions:
  - "Auto-wire LockManager as DirChangeNotifier in RegisterStoreForShare for seamless integration"
  - "Use ctx.ClientAddr as originClientID (AuthContext has no Identity.ClientID method)"
  - "Keep setattr.go direct NotifyDirChange call (NFS4-specific attr notifications not in DirChangeType enum)"
  - "Keep NFS4-specific directory delegation recall in remove.go (cleanup, not notification)"

patterns-established:
  - "Fire-and-forget notification: notifyDirChange helper with nil-safe dispatch, never affects mutation success"
  - "Per-share notifier map: dirChangeNotifiers keyed by shareName, auto-wired during store registration"
  - "shareNameForHandle helper: extracts share routing from FileHandle for notification dispatch"

requirements-completed: [LEASE-03, LEASE-04, ARCH-01]

# Metrics
duration: 8min
completed: 2026-03-02
---

# Phase 37 Plan 03: Unified DirChangeNotifier Summary

**MetadataService mutation methods wired with per-share DirChangeNotifier, NFS4 handlers simplified to unified notification path via LockManager -> BreakCallbacks**

## Performance

- **Duration:** 8 min
- **Started:** 2026-03-02T12:24:18Z
- **Completed:** 2026-03-02T12:32:00Z
- **Tasks:** 2
- **Files modified:** 10

## Accomplishments
- MetadataService now has per-share DirChangeNotifier map with auto-wiring via RegisterStoreForShare
- All 6 mutation methods (CreateFile, CreateSymlink, CreateSpecialFile, CreateHardLink, RemoveFile, Move, CreateDirectory, RemoveDirectory) call notifyDirChange after success
- NFS4 handlers (create, remove, rename, link, open) no longer call StateManager.NotifyDirChange directly
- Unified notification flow enables cross-protocol coordination: any protocol mutation breaks both SMB dir leases and NFS4 dir delegations

## Task Commits

Each task was committed atomically:

1. **Task 1: Wire DirChangeNotifier into MetadataService mutation methods** - `19904f39` (feat)
2. **Task 2: Refactor NFS4 handlers to unified notification path** - `716ad840` (feat)

**Plan metadata:** TBD (docs: complete unified dir-change-notifier plan)

## Files Created/Modified
- `pkg/metadata/service.go` - Added dirChangeNotifiers map, SetDirChangeNotifier, notifyDirChange helper, shareNameForHandle helper, auto-wire in RegisterStoreForShare
- `pkg/metadata/file_create.go` - CreateFile, CreateSymlink, CreateSpecialFile call notifyDirChange(DirChangeAddEntry)
- `pkg/metadata/file_remove.go` - RemoveFile calls notifyDirChange(DirChangeRemoveEntry)
- `pkg/metadata/file_modify.go` - Move calls notifyDirChange(DirChangeRenameEntry) on source dir, DirChangeAddEntry on dest dir for cross-dir moves
- `pkg/metadata/directory.go` - CreateDirectory calls notifyDirChange(DirChangeAddEntry), RemoveDirectory calls notifyDirChange(DirChangeRemoveEntry)
- `internal/adapter/nfs/v4/handlers/create.go` - Removed direct NotifyDirChange call and unused state import
- `internal/adapter/nfs/v4/handlers/remove.go` - Removed direct NotifyDirChange call and unused state import; kept NFS4-specific delegation recall
- `internal/adapter/nfs/v4/handlers/rename.go` - Removed both NotifyDirChange calls (same-dir and cross-dir) and unused state import
- `internal/adapter/nfs/v4/handlers/link.go` - Removed direct NotifyDirChange call and unused state import
- `internal/adapter/nfs/v4/handlers/open.go` - Removed NotifyDirChange call for OPEN+CREATE path; kept state import (still needed for DelegationState)

## Decisions Made
- **Auto-wire LockManager as DirChangeNotifier:** In RegisterStoreForShare, when a new LockManager is created for a share, it is automatically registered as the DirChangeNotifier for that share. This ensures every share gets directory change notifications without explicit wiring.
- **ctx.ClientAddr as originClientID:** AuthContext does not have an Identity.ClientID() method, so ClientAddr string is used as the origin client identifier for lease break exclusion.
- **setattr retains direct NotifyDirChange:** The setattr handler's NFS4-specific NOTIFY4_CHANGE_DIR_ATTRS notification has no equivalent in the DirChangeType enum (Add/Remove/Rename). Keeping the direct call is the cleanest approach.
- **Delegation recall kept in remove.go:** The NFS4-specific directory delegation recall for removed directories is cleanup logic (not a directory change notification), so it stays as a direct StateManager call.

## Deviations from Plan

None - plan executed exactly as written.

## Issues Encountered
- Pre-existing build errors exist in SMB v2 handlers (from unexecuted plan 37-02). Verification was scoped to `go build ./pkg/metadata/... ./internal/adapter/nfs/...` which passed cleanly. This is expected since plans 37-02 and 37-03 are in the same wave but 37-02 introduces new SMB handler code.

## User Setup Required

None - no external service configuration required.

## Next Phase Readiness
- Unified notification path is complete and ready for SMB handler integration (plan 37-02)
- When SMB handlers call MetadataService mutations, directory change notifications will automatically flow to both NFS4 and SMB lease managers via BreakCallbacks
- LockManager already supports multiple BreakCallbacks registrations (slice-based)

## Self-Check: PASSED

All 10 modified files verified on disk. Both task commits (19904f39, 716ad840) verified in git log. SUMMARY.md created at expected path.

---
*Phase: 37-smb3-leases-and-directory-leasing*
*Completed: 2026-03-02*
