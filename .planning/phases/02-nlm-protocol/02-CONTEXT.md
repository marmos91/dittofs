# Phase 2: NLM Protocol - Context

**Gathered:** 2026-02-05
**Status:** Ready for planning

<domain>
## Phase Boundary

Implement the Network Lock Manager protocol (RPC 100021) enabling NFSv3 clients to acquire and release byte-range locks via standard fcntl() calls. Includes NLM_TEST, NLM_LOCK, NLM_UNLOCK, NLM_CANCEL operations with blocking lock queue and callback mechanism. Integrates with the unified lock manager from Phase 1.

NSM (Network Status Monitor) for crash recovery is Phase 3 - not in scope here.

</domain>

<decisions>
## Implementation Decisions

### Blocking Lock Behavior
- Wait indefinitely for blocked locks until available or client cancels (standard NLM behavior)
- Per-file limit on blocking lock queue size (e.g., 100) to prevent resource exhaustion
- NLM_GRANTED callback to notify client when lock becomes available
- Release lock immediately if NLM_GRANTED callback fails (no hold period)

### Lock Owner Identity
- Owner format: `nlm:{hostname}:{pid}:{oh}` (protocol prefix + client hostname + svid + owner handle)
- Store client hostname as-is without validation or DNS resolution
- Accept any svid value (treat as opaque - some clients use thread IDs)
- Return full owner details (hostname, svid, owner handle) in conflict responses
- Same hostname+pid+svid = same lock owner (shared by svid, matches POSIX semantics)
- Ignore exclusive flag on unlock operations

### Error Responses
- Return NLM_DENIED with conflict info only - no retry guidance
- Return NLM_STALE_FH for locks on non-existent files (stale handles)
- Unlock of non-existent lock silently succeeds (NLM_GRANTED) for idempotency

### RPC Dispatcher Design
- Support NLM v4 only (modern clients, 64-bit offsets)
- Synchronous procedures only - skip async _MSG/_RES variants
- Run on same port as NFS (12049) - dispatch by RPC program number
- Extend existing NFS RPC dispatcher in dispatch.go with NLM program (100021)
- Callback client lives in internal/protocol/nlm/ (internal implementation detail)
- Fresh TCP connection for each callback (no connection caching)
- 5 second timeout for NLM_GRANTED callbacks
- Callback port from lock request (client provides callback program info)

### Code Structure
- NLM code in internal/protocol/nlm/ (parallel to internal/protocol/nfs/)
- Extract shared XDR utilities to internal/protocol/xdr/ (both NFS and NLM import)
- NLM-specific types in internal/protocol/nlm/xdr/
- NLM handlers call MetadataService for lock operations (add Lock/Unlock/TestLock methods)
- Separate Prometheus metrics namespace (nlm_* prefix)
- Blocking lock queue: per-file channel of waiting requests
- Extend existing NFS test infrastructure for NLM tests
- Unit tests use memory MetadataService with memory LockStore

### Claude's Discretion
- Queue full error code selection (recommend NLM_DENIED_NOLOCKS)
- Mount scoping for locks (standard NLM semantics - per-file globally)
- Specific XDR utility extraction decisions
- Internal callback client implementation details

</decisions>

<specifics>
## Specific Ideas

- Owner format explicitly includes protocol prefix for cross-protocol visibility with SMB (Phase 4)
- Callback failure = immediate lock release prevents orphaned grants
- Extending existing dispatcher (not separate) keeps firewall config simple (single port)
- Per-file channel for blocking queue is Go-idiomatic and matches LockManager's per-file design

</specifics>

<deferred>
## Deferred Ideas

None - discussion stayed within phase scope.

NSM crash recovery explicitly out of scope (Phase 3).

</deferred>

---

*Phase: 02-nlm-protocol*
*Context gathered: 2026-02-05*
