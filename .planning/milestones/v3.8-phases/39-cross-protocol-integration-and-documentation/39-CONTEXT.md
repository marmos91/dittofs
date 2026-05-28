# Phase 39: Cross-Protocol Integration and Documentation - Context

**Gathered:** 2026-03-02
**Status:** Ready for planning

<domain>
## Phase Boundary

Bidirectional coordination between SMB3 leases and NFS delegations through the metadata service's LockManager, plus comprehensive SMB3 documentation. NFS delegations move from adapter-specific state to the shared lock layer. Directory lease/delegation breaks are triggered by cross-protocol directory operations. Documentation covers all v3.8 features.

</domain>

<decisions>
## Implementation Decisions

### Coordination Model
- All delegation state (file + directory) moves from NFS adapter (`internal/adapter/nfs/v4/state/`) to the shared LockManager in `pkg/metadata/lock/`
- Delegations become a first-class lock type in `UnifiedLock` via a new `*Delegation` field (alongside existing `*OpLock` for leases)
- New protocol-neutral `Delegation` struct in `pkg/metadata/lock/` — no NFS-specific fields (Stateid, RecallTimer). NFS adapter maintains its own `map[DelegationID]Stateid4` mapping
- LockManager supports multiple BreakCallbacks (one per adapter). Both NFS and SMB register theirs
- Extend the existing `BreakCallbacks` interface with `OnDelegationRecall()`. Both adapters implement it (no-ops for irrelevant methods). Same interface, extended
- `CheckAndBreakCachingForWrite/Read/Delete` calls break BOTH leases AND delegations in a single unified call. Callers don't need to know which protocols are active
- Break operations are fire-and-forget with timeout, consistent with current SMB lease break flow
- The existing `OpLockBreakScanner` is extended to also handle delegation recall timeouts (replacing the NFS adapter's per-delegation RecallTimer)
- Single `LockStore`, extended to handle delegation entries alongside leases and byte-range locks
- Unified anti-storm mechanism: `RecentlyRecalledTTL` applies cross-protocol (NFS delegation recalled due to SMB activity prevents re-grant for 30s, and vice versa)
- Per-share LockManager (keep current pattern). Adapters register BreakCallbacks once, applied to all shares
- Adapters register BreakCallbacks during `adapter.SetRuntime()` lifecycle hook

### Directory Lease/Delegation Triggers
- MetadataService is the hook point: `CreateFile/RemoveFile/Rename` calls `LockManager.CheckAndBreakDirectoryCaching()` for the parent directory. Protocol-agnostic
- Bidirectional: SMB directory ops trigger NFS directory delegation recalls (CB_NOTIFY), and NFS directory ops trigger SMB3 directory lease breaks
- RENAME across directories breaks both source and target directory leases/delegations
- Directory delegations use the same `Delegation` struct with an `IsDirectory` bool flag
- Claude's discretion on which exact NFS operations trigger directory lease breaks (CREATE, REMOVE, RENAME at minimum; LINK and SETATTR on directory at Claude's judgment per MS-SMB2 spec)
- Claude's discretion on notification detail level (typed events with filename vs just 'changed') based on MS-SMB2 spec vs RFC 8881 differences

### Directory Change Notification Queue
- LockManager owns a generic, bounded directory change notification queue (both NFS and SMB adapters consume)
- Bounded capacity (e.g., 1024 events/directory) with overflow collapsing to 'full rescan needed' event
- Flush strategy: time + size threshold (e.g., 100 events or 500ms)
- Typed events with filename: ChangeType (Add|Remove|Rename|Modify), OldName, NewName
- NFS adapter drains queue into CB_NOTIFY. SMB adapter drains into CHANGE_NOTIFY
- NFS CB_NOTIFY batching specifics (wire format optimization) stays in NFS adapter

### Break Semantics
- Parallel breaks: when both NFS delegation and SMB lease exist, initiate both simultaneously
- Force-revoke and proceed on timeout (consistent with current behavior)
- Reuse per-protocol timeouts: SMB lease 35s (MS-SMB2 default), NFS delegation uses configurable `delegation_recall_timeout` (90s default)
- Cross-protocol conflicts logged at INFO level (consistent with existing `cross_protocol.go`)
- No Prometheus metrics for cross-protocol breaks in this phase
- Auto-grant on break acknowledgment: when a break is acknowledged, check if pending requests can now proceed
- Condition variable / channel pattern for waiting goroutines (goroutine blocks on per-file channel, signaled on break acknowledgment)
- Waiting goroutine uses request's `context.Context` for timeout (no separate wait timeout)
- Allow coexistence for reads: NFS read delegation + SMB Read lease can coexist. Write delegation and Write lease are mutually exclusive

### Code Structure and NFS Coexistence
- NFS v4 state manager refactored to delegate delegation state to LockManager. It becomes a thin adapter between NFS wire types and shared lock layer
- NFS adapter maintains its own stateid mapping (`map[DelegationID]Stateid4`)
- NFS adapter's delegation code (grant, recall, revoke) modified in this phase — not deferred
- Cross-protocol translation helpers (`cross_protocol.go`) extended for SMB3 Lease V2 (ParentLeaseKey, epoch)
- No feature flag for disabling cross-protocol coordination

### Testing
- Unit tests for new LockManager delegation logic, cross-protocol break coordination, notification queue
- New test files: `delegation_test.go`, `cross_protocol_break_test.go` plus extend existing `cross_protocol_test.go`
- No integration tests with mock adapters — rely on E2E tests with full adapters
- Existing NFS delegation E2E tests (`nfsv4_delegation_test.go`) updated to work with LockManager-based delegation
- Existing cross-protocol E2E tests (`cross_protocol_test.go`, `cross_protocol_lock_test.go`) updated and extended with SMB3 lease vs NFS delegation scenarios

### Documentation
- Expand existing `docs/SMB.md` in-place to cover all v3.8 features: encryption, signing, dialect negotiation, leases (V2 + directory), durable handles, Kerberos integration, cross-protocol coordination
- Both operational (mounting, configuration) and wire format details (for maintainers), following current SMB.md style
- Cross-protocol behavior matrix table: what happens for each NFS op when SMB lease exists, and vice versa
- Update `docs/SECURITY.md` with SMB3 security model (AES encryption, AES-CMAC/GMAC signing, SPNEGO/Kerberos, mutual auth)
- Update `docs/CONFIGURATION.md` with SMB3 adapter configuration options (encryption enforcement, signing requirements, dialect selection, lease timeouts, durable handle timeouts)
- Add cross-protocol troubleshooting section to `docs/TROUBLESHOOTING.md`
- Brief SMB3 mention in `README.md` (protocol overview, links to SMB.md)

### Claude's Discretion
- Exact notification queue capacity and flush thresholds
- Whether LINK and SETATTR on directory trigger directory lease breaks (per MS-SMB2 spec analysis)
- Whether notifications carry typed events or 'changed' flag (per protocol spec comparison)
- Internal implementation details of the channel-based waiting mechanism
- Exact delegation recall timeout scanning interval
- Delegation struct field design details

</decisions>

<specifics>
## Specific Ideas

- "I think we should refactor the lock manager to contain all the shared logic. Internal adapters should only contain details tied to NFS and SMB internalities"
- "Having a batching logic abstracted in the LockManager would benefit SMB as well as NFS" — led to the generic notification queue decision
- Existing `BreakCallbacks` interface pattern (OnOpLockBreak, OnByteRangeRevoke, OnAccessConflict) is the model for the extended interface

</specifics>

<code_context>
## Existing Code Insights

### Reusable Assets
- `pkg/metadata/lock/cross_protocol.go`: Translation helpers (NLM holder info, conflict reasons) — extend for Lease V2
- `pkg/metadata/lock/oplock_break.go`: `OpLockBreakScanner` — extend for delegation recall timeouts
- `pkg/metadata/lock/manager.go`: `LockManager` interface with `CheckAndBreakOpLocksFor*` — extend with delegation awareness
- `pkg/metadata/unified_view.go`: `UnifiedLockView`/`FileLocksInfo` — extend to include delegations
- `pkg/metadata/lock/oplock.go`: `OpLock` struct (lease state) — model for the new `Delegation` struct
- `pkg/metadata/lock/types.go`: `UnifiedLock` struct — add `*Delegation` field
- `internal/adapter/nfs/v4/state/delegation.go`: `DelegationState` — source of delegation semantics to extract into lock package

### Established Patterns
- `BreakCallbacks` interface pattern: adapters register callbacks, LockManager invokes them on breaks
- `OpLockBreakScanner`: background goroutine scanning for expired breaks, force-revoking on timeout
- Per-share `LockManager` instantiation via `MetadataService`
- `SetRuntime()` as adapter initialization hook for dependency injection
- `UnifiedLock` variant types: byte-range (Lease=nil), lease (Lease!=nil) — delegation extends this pattern

### Integration Points
- `MetadataService.CreateFile/RemoveFile/Rename`: hook point for directory caching break calls
- `adapter.SetRuntime()`: where adapters register BreakCallbacks
- `LockManager.CheckAndBreakOpLocksForWrite/Read/Delete`: unified break entry points to extend
- NFS v4 state manager (`internal/adapter/nfs/v4/state/manager.go`): refactor to delegate to LockManager
- `test/e2e/cross_protocol_test.go`, `cross_protocol_lock_test.go`: E2E tests to update
- `test/e2e/nfsv4_delegation_test.go`: delegation E2E tests that must continue passing

</code_context>

<deferred>
## Deferred Ideas

- Prometheus metrics for cross-protocol breaks — add later when observability is prioritized
- Cross-protocol coordination feature flag — not needed, no measurable overhead when only one protocol active

</deferred>

---

*Phase: 39-cross-protocol-integration-and-documentation*
*Context gathered: 2026-03-02*
