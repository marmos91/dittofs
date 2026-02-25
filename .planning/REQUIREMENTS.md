# Requirements: v3.5 Adapter + Core Refactoring & v3.6 Windows Compatibility

## v3.5 Adapter + Core Refactoring

### REF-01: Generic Lock Interface
**Priority**: High
**Source**: `docs/GENERIC_LOCK_INTERFACE_PLAN.md`

Unify the lock model so all protocols (NFS, SMB, NLM) share the same types with protocol-specific translation at the boundary.

- [ ] REF-01.1: `EnhancedLock` renamed to `UnifiedLock` with cross-protocol semantics
- [ ] REF-01.2: `LeaseInfo` renamed to `OpLock` with `OpLockState` bitmask (Read/Write/Handle)
- [ ] REF-01.3: `ShareReservation` renamed to `AccessMode` (DenyNone/DenyRead/DenyWrite/DenyAll)
- [ ] REF-01.4: `LeaseBreakScanner` renamed to `OpLockBreakScanner`, centralized break/recall
- [ ] REF-01.5: Centralized `IsConflicting()` handles all conflict cases (oplock vs oplock, oplock vs byte-range, byte-range vs byte-range, access mode)
- [ ] REF-01.6: `PersistedLock` and `LockStore` updated with new field names (OpLockGroupKey, ReclaimOpLock, IsOpLock)
- [ ] REF-01.7: Manager API updated (`AddUnifiedLock`, `RemoveUnifiedLock`, `ListUnifiedLocks`)
- [ ] REF-01.8: SMB adapter has thin translation layer (`ToOpLock`, `ToAccessMode`)
- [ ] REF-01.9: NFSv4 adapter has thin translation layer (delegation -> OpLock, OPEN4_SHARE_DENY -> AccessMode)
- [ ] REF-01.10: GracePeriodManager stays generic (used by NLM, NFSv4, SMB reconnect)

### REF-02: Protocol Leak Purge
**Priority**: High
**Source**: `docs/CORE_REFACTORING_PLAN.md` Phase 1

Remove protocol-specific code from generic layers (metadata, controlplane, lock).

- [ ] REF-02.1: SMB lock types removed from `pkg/metadata/lock/` (ShareReservation, LeaseInfo, lease_break.go moved/renamed)
- [ ] REF-02.2: SMB lease methods removed from MetadataService (CheckAndBreakLeases*, ReclaimLeaseSMB, OplockChecker)
- [ ] REF-02.3: NLM methods moved from MetadataService to NFS adapter (LockFileNLM, TestLockNLM, UnlockFileNLM, CancelBlockingLock)
- [ ] REF-02.4: Share model cleaned — NFS fields (AllowAuthSys, RequireKerberos, Squash, etc.) to NFSExportOptions JSON config
- [ ] REF-02.5: Share model cleaned — SMB fields (GuestEnabled, GuestUID, GuestGID) to SMBShareOptions JSON config
- [ ] REF-02.6: SquashMode moved to NFS adapter
- [ ] REF-02.7: Netgroup operations (8 methods) removed from generic Store interface
- [ ] REF-02.8: Identity mapping operations (4 methods) removed from generic Store interface
- [ ] REF-02.9: NFS-specific runtime fields moved (mounts, dnsCache, nfsClientProvider)
- [ ] REF-02.10: NFS-specific API handlers moved (clients.go, grace.go)
- [ ] REF-02.11: `pkg/identity/` moved to NFS adapter internal

### REF-03: NFS Adapter Restructuring
**Priority**: High
**Source**: `docs/NFS_REFACTORING_PLAN.md`

Restructure NFS adapter directory layout and dispatch consolidation.

- [ ] REF-03.1: `internal/protocol/` renamed to `internal/adapter/` (all imports updated)
- [ ] REF-03.2: Generic XDR moved to `internal/adapter/nfs/xdr/core/`
- [ ] REF-03.3: NLM, NSM, portmapper consolidated under `internal/adapter/nfs/`
- [ ] REF-03.4: `internal/auth/` moved to `internal/adapter/smb/auth/`
- [ ] REF-03.5: `pkg/adapter/nfs/` files renamed (remove `nfs_` prefix, split mixed-concern files)
- [ ] REF-03.6: v4.1-specific handlers and types moved to `v4/v4_1/` nested hierarchy
- [ ] REF-03.7: Dispatch consolidated: single `nfs.Dispatch()` entry point in `internal/adapter/nfs/`
- [ ] REF-03.8: Connection code split by version (connection.go + connection_v4.go)
- [ ] REF-03.9: Shared handler helpers extracted
- [ ] REF-03.10: Handler documentation added (3-5 lines each, all v3/v4/mount handlers)
- [ ] REF-03.11: Version negotiation tests added

### REF-04: SMB Adapter Restructuring
**Priority**: High
**Source**: `docs/SMB_REFACTORING_PLAN.md`

Restructure SMB adapter to mirror NFS pattern and extract shared infrastructure.

- [ ] REF-04.1: `pkg/adapter/smb/` files renamed (remove `smb_` prefix)
- [ ] REF-04.2: BaseAdapter extracted to `pkg/adapter/base.go` with shared lifecycle (accept, shutdown, connection tracking)
- [ ] REF-04.3: Both NFS and SMB adapters embed BaseAdapter
- [ ] REF-04.4: NetBIOS framing moved to `internal/adapter/smb/framing.go`
- [ ] REF-04.5: Signing verification moved to `internal/adapter/smb/signing.go`
- [ ] REF-04.6: Dispatch + response logic consolidated in `internal/adapter/smb/dispatch.go`
- [ ] REF-04.7: Compound handling moved to `internal/adapter/smb/compound.go`
- [ ] REF-04.8: `auth.Authenticator` interface defined, NTLM + Kerberos implementations
- [ ] REF-04.9: Shared handler helpers extracted to `internal/adapter/smb/helpers.go`
- [ ] REF-04.10: `pkg/adapter/smb/connection.go` reduced to thin read/dispatch/write loop
- [ ] REF-04.11: Handler documentation added (3-5 lines each, all SMB2 commands)

### REF-05: Core Object Decomposition
**Priority**: Medium
**Source**: `docs/CORE_REFACTORING_PLAN.md` Phases 2-5

Decompose god objects into focused components.

- [ ] REF-05.1: ControlPlane Store interface decomposed into 9 sub-interfaces
- [ ] REF-05.2: API handlers narrowed to accept specific sub-interfaces
- [ ] REF-05.3: AdapterManager extracted from Runtime (~300 lines)
- [ ] REF-05.4: MetadataStoreManager extracted from Runtime (~70 lines)
- [ ] REF-05.5: TransferManager renamed to Offloader, package to `pkg/payload/offloader/`
- [ ] REF-05.6: Upload orchestrator extracted to `upload.go` (~450 lines)
- [ ] REF-05.7: Download coordinator extracted to `download.go` (~250 lines)
- [ ] REF-05.8: Dedup handler extracted to `dedup.go` (~150 lines)

### REF-06: Code Quality Improvements
**Priority**: Medium
**Source**: `docs/CORE_REFACTORING_PLAN.md` Phases 6-9

Error unification, boilerplate reduction, and file organization.

- [ ] REF-06.1: Structured `PayloadError` type wrapping sentinels with context
- [ ] REF-06.2: Shared error-to-status mapping for NFS/SMB
- [ ] REF-06.3: Generic GORM helpers (`getByField[T]`, `listAll[T]`, `createWithID[T]`)
- [ ] REF-06.4: Centralized API error mapping helper
- [ ] REF-06.5: Generic API client helpers (`getResource[T]`, `listResources[T]`)
- [ ] REF-06.6: Common transaction helpers in `pkg/metadata/store/txutil/`
- [ ] REF-06.7: Shared transaction test suite in `pkg/metadata/store/storetest/`
- [ ] REF-06.8: `pkg/metadata/file.go` split into file_create.go, file_modify.go, file_remove.go, file_helpers.go
- [ ] REF-06.9: `pkg/metadata/authentication.go` split into identity.go, permissions.go

---

## v3.6 Windows Compatibility

### WIN-01: SMB Bug Fixes
**Priority**: High
**Source**: GitHub issues #180, #181

- [ ] WIN-01.1: Sparse file READ returns zeros for unwritten blocks (#180) — handle ErrBlockNotFound as sparse region in Offloader.downloadBlock()
- [ ] WIN-01.2: Cache layer reached for sparse reads (not short-circuited by transfer manager error)
- [ ] WIN-01.3: Renamed directories show as `<DIR>` in parent listing (#181) — update Path field in Move operation
- [ ] WIN-01.4: E2E tests for sparse file create + read (fsutil createnew equivalent)
- [ ] WIN-01.5: E2E tests for directory rename + listing verification

### WIN-02: Windows ACL Support
**Priority**: High
**Source**: GitHub issue #182, MS-DTYP, MS-SMB2

- [ ] WIN-02.1: NT Security Descriptor encoding (Owner SID, Group SID, DACL, optional SACL)
- [ ] WIN-02.2: Unix UID/GID to Windows SID mapping (well-known SIDs + S-1-22-1-UID / S-1-22-2-GID)
- [ ] WIN-02.3: Unix permissions (rwx) to Windows ACE translation in DACL
- [ ] WIN-02.4: QUERY_INFO SecurityInformation handler returns proper descriptors
- [ ] WIN-02.5: SET_INFO SecurityInformation handler accepts permission changes (best-effort Unix mapping)
- [ ] WIN-02.6: Directory inheritance flags (CONTAINER_INHERIT_ACE, OBJECT_INHERIT_ACE)
- [ ] WIN-02.7: `icacls` shows meaningful permissions on DittoFS shares
- [ ] WIN-02.8: NFSv4 ACL and SMB Security Descriptor interoperability maintained

### WIN-03: Windows Integration Testing
**Priority**: High
**Source**: User requirement, test suites

Comprehensive validation using open-source test suites and manual Windows 11 testing.

- [ ] WIN-03.1: Samba smbtorture SMB2 basic tests pass (connect, read, write, lock, oplock, lease)
- [ ] WIN-03.2: Samba smbtorture SMB2 ACL and directory tests pass
- [ ] WIN-03.3: Microsoft WindowsProtocolTestSuites File Server BVT suite (101 tests) passes
- [ ] WIN-03.4: Microsoft WindowsProtocolTestSuites selected feature tests pass (lease, oplock, lock, signing)
- [ ] WIN-03.5: Windows 11 Explorer validation (create, rename, delete, copy, move, drag-and-drop)
- [ ] WIN-03.6: Windows 11 cmd.exe validation (dir, type, copy, move, ren, del, mkdir, rmdir, icacls, fsutil)
- [ ] WIN-03.7: Windows 11 PowerShell validation (Get-Item, Set-Item, Get-Acl, Set-Acl)
- [ ] WIN-03.8: Issues #180, #181, #182 verified fixed on Windows
- [ ] WIN-03.9: No regressions on Linux/macOS NFS or SMB mounts

---

## Traceability

| Requirement | Phase(s) | Status |
|-------------|----------|--------|
| REF-01: Generic Lock Interface | 26 | Not started |
| REF-02: Protocol Leak Purge | 26 | Not started |
| REF-03: NFS Adapter Restructuring | 27 | Not started |
| REF-04: SMB Adapter Restructuring | 28 | Not started |
| REF-05: Core Object Decomposition | 29 | Not started |
| REF-06: Code Quality Improvements | 29 | Not started |
| WIN-01: SMB Bug Fixes | 30 | Not started |
| WIN-02: Windows ACL Support | 31 | Not started |
| WIN-03: Windows Integration Testing | 32 | Not started |

---
*Requirements created: 2026-02-25*
