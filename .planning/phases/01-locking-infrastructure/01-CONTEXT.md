# Phase 1: Locking Infrastructure - Context

**Gathered:** 2026-02-04
**Status:** Ready for planning

<domain>
## Phase Boundary

Build the protocol-agnostic unified lock manager that serves as the foundation for all locking across NFS and SMB. The lock manager accepts lock requests from protocol adapters, persists state in the metadata store, detects conflicts, handles grace periods after restart, and manages client connections. Protocol-specific features (NLM, SMB leases) are separate phases.

</domain>

<decisions>
## Implementation Decisions

### Lock Semantics
- **Lock primitive:** Byte-range locks only. Whole-file locks (SMB share modes, delegations) modeled as range(0, MAX) by adapters.
- **Conflict rules:** Standard POSIX — read-read OK, read-write conflict, write-write conflict. Same owner can upgrade/split.
- **Ownership model:** Standard NFS/SMB behavior — same user from different sessions = different owners. Adapters provide protocol-level owner IDs (NLM's nlm_oh+client+pid, SMB's session+pid). Controlplane user for authorization, not ownership.
- **Lock upgrade:** Atomic upgrade supported (read → write if no other readers)
- **Deadlock detection:** Basic cycle detection (A-waits-B-waits-A). Deny the request that would create cycle.
- **Blocking timeout:** Server-side timeout, configurable at server level (default 60s)
- **Lock splitting:** Full POSIX splitting supported — unlock middle of range creates two locks
- **Share reservations:** Integrated in unified manager (deny-read, deny-write, deny-all). NFS ignores if not supported.
- **Mandatory vs advisory:** Configurable per-share. Advisory by default, mandatory enforcement available.

### Persistence Model
- **What survives restart:** All lock state (ranges, owners, types)
- **Where stored:** Same as metadata store (BadgerDB, Postgres). Uses existing transaction abstraction.
- **Atomicity:** Lock operations atomic with related metadata changes
- **Recovery:** Restore all locks on startup, enter grace period, clients reclaim or locks expire
- **Store failure:** Strict mode — halt lock operations until persistence available
- **File deletion:** Protocol-specific. NFS allows delete (locks orphaned). SMB blocks delete.
- **Sync frequency:** Synchronous — every lock operation waits for persistence
- **Garbage collection:** Immediate on file delete + periodic scan for orphaned records
- **Limits:** Configurable max locks per file + max total locks

### Grace Period Behavior
- **Duration:** Configurable, default 90 seconds
- **Scope:** Affects lock operations only, not I/O. Adapters handle I/O restrictions if needed.
- **Allowed during grace:** Lock reclaims only. New locks denied with grace-period error. Lock tests allowed.
- **Reclaim identification:** Adapter flags reclaim + lock manager validates against persisted state
- **Invalid reclaim:** Deny immediately. Client retries as new lock after grace ends.
- **Early exit:** Yes — exit grace early when all expected clients have reclaimed
- **Unclaimed locks:** Release immediately when grace period ends
- **Unified period:** Single grace period for all protocols (no per-protocol periods)

### Connection Tracking
- **Registration:** Adapter notifies lock manager (RegisterClient/UnregisterClient)
- **Disconnect handling:** Adapter-controlled TTL. TTL=0 for immediate release (NFS), TTL>0 for durable handles (SMB).
- **Pool scope:** Per-adapter connection pools with independent limits
- **Connection limits:** Configurable max connections per adapter
- **Stale cleanup:** Protocol-specific (NFS: keep-alive based, SMB: session timeout)
- **Migration:** Protocol-specific (NFSv4: identity survives IP change, NLM: IP-based)
- **Observability:** Prometheus metrics for connection tracking (no REST API)
- **HA preparation:** Design to not preclude future HA, but no replication in Phase 1
- **Per-client limits:** Configurable max locks per client
- **Logging:** Structured logging for connection events (connect, disconnect, lock operations)

### Claude's Discretion
- Lock upgrade/downgrade state machine details
- Mandatory lock enforcement integration with I/O paths
- Deadlock detection algorithm specifics
- Connection flapping detection (if needed)
- Epoch tracking for split-brain detection (if needed)
- Exact Prometheus metric names and labels

</decisions>

<specifics>
## Specific Ideas

- "Lock manager should be embedded in metadata service" — architectural decision from project init
- Follow standard NFS/SMB behavior where possible — enterprise compatibility matters
- Adapters are the translation layer — lock manager stays protocol-agnostic

</specifics>

<deferred>
## Deferred Ideas

None — discussion stayed within phase scope

</deferred>

---

*Phase: 01-locking-infrastructure*
*Context gathered: 2026-02-04*
