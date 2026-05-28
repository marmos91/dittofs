# Phase 37: SMB3 Leases and Directory Leasing - Context

**Gathered:** 2026-03-02
**Status:** Ready for planning

<domain>
## Phase Boundary

Upgrade existing SMB2.1 lease infrastructure to Lease V2 (ParentLeaseKey, epoch tracking), add directory leases, and consolidate lease management into the shared metadata layer following the NFS4 adapter pattern. All lease business logic moves out of SMB handlers (ARCH-01). NFS4 StateManager updated to use the unified notification path.

</domain>

<decisions>
## Implementation Decisions

### Consolidation strategy (ARCH-01)
- **Full merge into LockManager** — move ALL lease CRUD (RequestLease, AcknowledgeBreak, ReleaseLease, conflict checks, NLM cross-protocol checks) from OplockManager into `pkg/metadata/lock/Manager`. LockManager becomes the single source of truth for lease state.
- **Delete OplockManager entirely** — don't gut/refactor, delete. Create a new thin SMB `LeaseManager` wrapper in `internal/adapter/smb/lease/` (parallel to `internal/adapter/nfs/v4/state/`). The wrapper only handles: sessionID-to-leaseKey mapping and SendLeaseBreak notification dispatch.
- **No SMB-side caching** — all lease lookups go through LockManager. If performance matters, add caching inside LockManager itself, not in the adapter wrapper.
- **Cross-protocol NLM checks move into LockManager** — `checkNLMLocksForLeaseConflict()` logic joins `CheckAndBreakOpLocksForWrite/Read/Delete` and all cross-protocol translation in `pkg/metadata/lock/`. One location for all protocol interop.
- **LockManager tracks break state** — activeBreaks map (lease break timeout tracking) moves into LockManager alongside the existing `OpLockBreakScanner`. SMB LeaseManager only keeps sessionMap (SMB-specific session→lease binding).
- **Protocol-agnostic reclaim** — `LockManager.ReclaimLease(leaseKey, requestedState, isDirectory)` replaces both `ReclaimLeaseSMB()` and `ReclaimDelegation()`. Single method for both protocols.
- **Restricted lease upgrade transitions** — LockManager validates upgrade transitions against a whitelist (R→RW, R→RH, RH→RWH, etc.). Rejects invalid transitions (downgrades, invalid states).
- **Unified persistence** — LockManager uses its existing LockStore for all lease persistence. Remove OplockManager's separate persistence path.
- **Incremental migration** — 1) Add lease methods to LockManager, 2) Update call sites to use LockManager, 3) Delete OplockManager. Each step testable independently.
- **Per-share isolation** — lease state remains per-share (each share has its own LockManager). No global lease registry.
- **Single LockManager interface** — no sub-interfaces. Add lease methods (RequestLease, AcknowledgeBreak, ReleaseLease, ReclaimLease) directly to the existing LockManager interface.

### Directory lease breaks
- **Strict spec triggers** — only create, delete, and rename within the directory trigger directory lease breaks (per MS-SMB2 3.3.4.7). SetInfo/attribute changes do NOT break.
- **Immediate dispatch** — no batching for SMB directory breaks (unlike NFS4 batched notifications). Break the directory lease synchronously when the mutation happens. Simpler and spec-aligned.
- **DirChangeNotifier callback** — MetadataService gets a `DirChangeNotifier` interface. After successful create/delete/rename, calls `notifier.OnDirChange(parentHandle, changeType, originClient)`. LockManager implements this interface. MetadataService doesn't import lease types.
- **Simple change type enum** — three values: `AddEntry`, `RemoveEntry`, `RenameEntry`. Enough for both SMB breaks and NFS notifications.
- **Self-notification exclusion** — pass originating client identity in the DirChangeNotifier callback. LockManager skips breaking leases owned by the originating client (a client's own operations don't break its own leases, per MS-SMB2).
- **Immediate parent only** — break scoped to the direct parent directory. Creating a file in `/parent/child/` breaks `child`'s lease, NOT `parent`'s.
- **Non-blocking cross-protocol** — NFS operations that trigger SMB directory lease breaks proceed immediately. Break is dispatched asynchronously. If SMB client doesn't acknowledge within 35s, lease is force-revoked.
- **Same timeout model** — directory lease breaks use the same 35s acknowledgment window as file leases via `OpLockBreakScanner`. Consistent behavior, no special-casing.
- **Recently-broken cache** — after breaking a directory lease, don't grant one for the same directory for N seconds (e.g., 5s). Prevents grant-break storms on busy directories. Same pattern as NFS4 StateManager's `recentlyRecalled`.

### LockManager as notification hub
- **Single notification path** — MetadataService notifies LockManager only. LockManager fans out to both NFS4 StateManager and SMB LeaseManager via the existing `BreakCallbacks` interface.
- **Reuse OnOpLockBreak** — directory leases stored as `UnifiedLock` with `Lease` field. `OnOpLockBreak` already dispatches for these. Receivers check if the lock is a directory lease and handle accordingly. No new BreakCallbacks method needed.
- **NFS4 handlers refactored** — NFS handlers no longer call `StateManager.NotifyDirChange()` directly. All directory change notifications flow through: NFS handlers → MetadataService → LockManager → BreakCallbacks → StateManager. One unified path for both protocols.

### NFS4 co-existence
- **Allow dual directory leases** — both NFS directory delegation and SMB directory lease can co-exist on the same directory. Both are Read-only caching. A mutation breaks both via LockManager dispatch.
- **Cross-protocol break via BreakCallbacks** — LockManager dispatches directory change events. NFS4 StateManager and SMB LeaseManager are both registered callback receivers. Same pattern as file lease breaks.
- **All cross-protocol logic in pkg/metadata/lock/** — TranslateToNLMHolder, SMB→NFS direction break coordination, and all translation helpers live in the shared lock package.
- **Update NFS4 StateManager in this phase** — since LockManager's interface changes, NFS4 StateManager must be updated to compile and work with the new unified notification path. Wire up directory break handling through BreakCallbacks.

### Lease reclaim and grace period
- **Directory leases are reclaimable** — persisted and reclaimable during grace period, same as file leases. Consistent behavior for all lease types.
- **Deny new, allow reclaim during grace** — follow existing grace period logic. New directory lease requests denied during grace. Only reclaim operations permitted.
- **Single ReclaimLease method** — `LockManager.ReclaimLease(leaseKey, requestedState, isDirectory)` for both SMB and NFS. Validates reclaim against persisted state.
- **Same grace window** — one grace period for everything (file leases, directory leases, NFS delegations, byte-range locks). Default 45s.
- **Reject reclaim on deleted directory** — LockManager checks if directory handle still exists in MetadataStore. If not, return 'not found'. Don't grant a lease on a non-existent directory. Existence check only, no mtime/generation validation.

### Wire format and V1 compatibility
- **V2 only for directory leases** — directory leases require Lease V2 context (ParentLeaseKey + epoch). V1 clients (SMB 2.1) can still get file leases. Directory leasing requires SMB 3.0+.
- **ParentLeaseKey: store but don't validate** — accept and store ParentLeaseKey. Echo it in break notifications. Don't require a matching directory lease to exist. Per MS-SMB2 3.3.5.9.11, it's a client-side hint for cache correlation, not a server-enforced relationship.
- **Size-based V1/V2 detection** — keep current approach: 52 bytes = V2 (ParentLeaseKey + epoch), 32 bytes = V1. Already implemented.
- **V2 break notifications for V2 clients** — if the lease was created with V2 context, send V2 LEASE_BREAK_Notification (includes ParentLeaseKey + epoch). Client needs both for cache tree management and staleness checking.
- **Lease V2 encoding in smbenc** — add LeaseV2ResponseContext encode/decode to smbenc package (ARCH-02 compliance). All wire format logic in the codec layer.
- **Extend existing AcknowledgeLeaseBreak** — add epoch field parsing for V2 acks. Validate epoch staleness. No separate V2 handler.

### Code structure
- **SMB LeaseManager location** — `internal/adapter/smb/lease/` (new sub-package). Shared by all SMB versions (both 2.1 and 3.0+ clients). Parallel to `internal/adapter/nfs/v4/state/`.
- **DirChangeNotifier interface** — lives in `pkg/metadata/lock/` alongside LockManager. New file `directory.go` with DirChangeNotifier interface + DirChangeType enum + recently-broken cache logic.
- **Delete OplockManager files** — delete `lease.go`, `oplock.go` from `internal/adapter/smb/v2/handlers/`. Move lease context parsing to `internal/adapter/smb/lease/` or smbenc. Start SMB LeaseManager fresh as a thin wrapper.
- **Single-word filenames in lock/** — `leases.go` (RequestLease, AcknowledgeBreak, ReleaseLease, ReclaimLease), `directory.go` (DirChangeNotifier, directory break dispatch, recently-broken cache), `reclaim.go` (unified reclaim logic). No underscores in filenames.
- **Package layout preserved** — `pkg/metadata/lock/` for shared LockManager, `internal/adapter/smb/lease/` for SMB wrapper, `internal/adapter/nfs/v4/state/` for NFS wrapper. Public foundation, protocol-specific wrappers in internal.
- **Unsolicited message dispatch** — Claude decides approach for SMB LEASE_BREAK_Notification send infrastructure based on existing SendLeaseBreak/LeaseBreakNotifier code.

### Documentation
- **Update ARCHITECTURE.md** — new LockManager lease methods, DirChangeNotifier pattern, SMB LeaseManager wrapper, directory lease break flow.
- **Update CLAUDE.md** — expanded Key Interfaces section (LockManager with lease methods, DirChangeNotifier), updated directory structure (internal/adapter/smb/lease/).
- **Add sequence diagram** — text-based (Mermaid or ASCII) showing MetadataService → LockManager → BreakCallbacks → SMB LeaseManager / NFS4 StateManager break dispatch flow.

### Testing
- **smbtorture + MSVP** — use both suites. smbtorture for rapid development validation (smb2.lease, smb2.lease-v2, smb2.dir-lease). MSVP for final conformance.
- **Update KNOWN_FAILURES.md** — Claude determines which smbtorture tests should pass after this phase based on what's implemented.
- **Run tests locally and in CI** — existing pipeline and local test infrastructure. Update KNOWN_FAILURES.md to reflect new expected-pass tests.
- **Cross-protocol e2e tests** — add integration tests to `test/e2e/` that validate: 1) SMB directory lease broken by NFS file creation in same directory, 2) NFS directory delegation recalled by SMB file creation, 3) Cross-protocol lease coordination end-to-end.
- **Claude's discretion** — conformance test suite design for LockManager lease operations (locktest/ or inline tests).

### Claude's Discretion
- Exact smbtorture/MSVP test mapping to expected-pass
- Unsolicited message dispatch mechanism for LEASE_BREAK_Notification
- Conformance test organization (locktest/ package vs inline tests)
- Recently-broken cache duration (suggested 5s but Claude can adjust)
- Sequence diagram format (Mermaid vs ASCII)

</decisions>

<specifics>
## Specific Ideas

- "Can't we just use the lock interfaces that are already there for NFS4?" — maximum reuse of existing infrastructure, not new abstractions
- Follow NFS4 StateManager pattern exactly: shared LockManager + thin protocol wrapper
- Minimize new code — leverage existing OpLock struct, ValidDirectoryLeaseStates, BreakCallbacks, cross-protocol helpers
- "With these changes users can acquire a lock on SMB and lock on NFS and vice versa" — cross-protocol lock coordination is a key outcome
- "Remember to update NFS with changes made to the LockManager" — NFS4 StateManager must be updated in this phase, not deferred
- File naming preference: avoid underscores in Go filenames. Use single-word names or separate packages for grouping.

</specifics>

<code_context>
## Existing Code Insights

### Reusable Assets
- `pkg/metadata/lock/oplock.go`: OpLock struct with LeaseKey, LeaseState, Epoch, ValidDirectoryLeaseStates already defined
- `pkg/metadata/lock/manager.go`: LockManager interface and Manager implementation with CheckAndBreakOpLocksForWrite/Read/Delete, BreakCallbacks registration, UnifiedLock CRUD, OpLockBreakScanner
- `internal/adapter/smb/v2/handlers/lease.go`: OplockManager with full RequestLease/AcknowledgeLeaseBreak/ReleaseLease — **consolidation source (will be deleted)**
- `internal/adapter/smb/v2/handlers/lease_context.go`: LeaseCreateContext with ParentLeaseKey V2 decode already working (52-byte V2 detection)
- `internal/adapter/smb/v2/handlers/cross_protocol.go`: NLM conflict checking — **moves to pkg/metadata/lock/**
- `pkg/metadata/lock/cross_protocol.go`: TranslateToNLMHolder, TranslateSMBConflictReason
- `pkg/metadata/lock/oplock_break.go`: OpLockBreakScanner with timeout enforcement (35s default)
- `internal/adapter/nfs/v4/state/`: StateManager pattern — **reference architecture for SMB LeaseManager**, includes recentlyRecalled cache, batched notifications, DelegationState

### Established Patterns
- NFS4 StateManager pattern: adapter-side wrapper holding protocol state, calls into shared LockManager
- BreakCallbacks interface: protocol adapters register break handlers, LockManager dispatches (OnOpLockBreak, OnByteRangeRevoke, OnAccessConflict)
- smbenc codec: all SMB binary encoding goes through smbenc Reader/Writer (ARCH-02)
- UnifiedLock with Lease field: polymorphic lock type supporting both byte-range and lease
- GracePeriodManager: embedded in LockManager, handles reclaim windows
- LeaseReclaimer: handles lease reclaim with protocol-specific paths (to be unified)
- Per-share LockManager isolation: MetadataService creates one LockManager per share

### Integration Points
- `LockManager.CheckAndBreakOpLocksForWrite/Read/Delete` — extend for directory lease awareness
- `BreakCallbacks.OnOpLockBreak` — reused for directory lease breaks. NFS4 StateManager and SMB LeaseManager both register.
- CREATE handler in `internal/adapter/smb/v2/handlers/create.go` — calls ProcessLeaseCreateContext, will call new SMB LeaseManager
- CLOSE handler — calls OplockManager.ReleaseLease, will call LockManager directly
- MetadataService.CreateFile/RemoveFile/Rename — add DirChangeNotifier callbacks after successful mutations
- NFS4 handlers — stop calling StateManager.NotifyDirChange() directly, let MetadataService→LockManager→BreakCallbacks path handle it

</code_context>

<deferred>
## Deferred Ideas

None — discussion stayed within phase scope

</deferred>

---

*Phase: 37-smb3-leases-and-directory-leasing*
*Context gathered: 2026-03-02 (updated)*
