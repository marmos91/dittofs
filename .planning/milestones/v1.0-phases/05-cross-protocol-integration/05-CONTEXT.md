# Phase 5: Cross-Protocol Integration - Context

**Gathered:** 2026-02-05
**Status:** Ready for planning

<domain>
## Phase Boundary

Enable lock visibility and conflict detection across NFS (NLM) and SMB protocols. When an NFS client holds a lock, SMB clients must see the conflict (and vice versa). Grace period recovery works for both protocols. E2E tests verify all cross-protocol scenarios.

</domain>

<decisions>
## Implementation Decisions

### Conflict Behavior

- **NFS lock vs SMB Write lease:** Deny SMB immediately (STATUS_LOCK_NOT_GRANTED). NFS byte-range locks are explicit and win over opportunistic SMB leases.
- **SMB Write lease vs NFS lock:** Trigger SMB lease break, wait for acknowledgment (up to 35s), then grant NFS lock. SMB leases are designed to be breakable.
- **NFS blocking locks waiting on SMB:** Use NLM4_BLOCKED pattern — return immediately, grant via NLM_GRANTED callback when lease clears. Consistent with existing NLM blocking queue.
- **SMB Handle (H) lease vs NFS REMOVE/RENAME:** Break H lease before proceeding. NFS operation waits for SMB client to close handles (up to 35s). H leases exist precisely to prevent surprise deletion.
- **Conflict detection location:** Centralized in unified lock manager (existing architecture from Phase 1-4). Protocol handlers call acquire/release, lock manager handles all conflict logic.

### E2E Test Scenarios

- **Test priority:** All scenarios equal coverage — same-protocol regression AND cross-protocol conflicts
- **Client tooling:** Real clients only — Linux NFS mount + mount.cifs kernel mount
- **Timeout for tests:** Shortened 5-second timeout (configurable) for faster CI runs
- **Scenario coverage:** All permutations — NFS→SMB and SMB→NFS for each lock type (shared/exclusive, R/W/H leases)
- **Data integrity:** Tests verify actual file content after cross-protocol operations, not just lock/lease behavior
- **Concurrency:** Sequential tests only (one NFS, one SMB client at a time). Stress tests deferred to future phase.

### Grace Period Coordination

- **Grace period model:** Single shared grace period for both NFS and SMB (90 seconds). Both protocols reclaim during same window.
- **Reclaim verification:** Verify reclaims against persisted lock state from Phase 1. Allow reclaim only if lock existed before restart.
- **Unclaimed locks:** Auto-delete when grace period ends. Standard NFS server behavior — if client misses 90s window, assume crashed.

### Error Messaging

- **NFS denial due to SMB:** Return NLM4_DENIED with holder info — owner="smb:<client>", offset=0, length=MAX (whole file). Helps debugging cross-protocol issues.
- **SMB denial due to NFS:** Claude's discretion on appropriate STATUS code (likely STATUS_LOCK_NOT_GRANTED or STATUS_SHARING_VIOLATION)
- **Log level:** INFO for cross-protocol conflicts. They're working as designed, not errors.

### Code Structure

- **UnifiedLockView struct:** New struct in `pkg/metadata/` owned by MetadataService but separate for cleanliness
- **Instantiation:** One UnifiedLockView per share (alongside LockManager and OplockManager)
- **Query API:** Unified — `GetAllLocksOnFile` returns both NLM locks and SMB leases. Each protocol translates to its wire format.

### Claude's Discretion

- Specific SMB error code for NFS lock conflicts
- Internal implementation of UnifiedLockView methods
- E2E test file organization and naming
- Prometheus metric names for cross-protocol events

</decisions>

<specifics>
## Specific Ideas

- UnifiedLockView provides single query API, protocols translate results to their wire format
- Use existing NLM4_BLOCKED + NLM_GRANTED callback pattern for blocking locks waiting on SMB lease breaks
- Holder info in NLM4_DENIED responses should include "smb" in owner field for clear cross-protocol debugging

</specifics>

<deferred>
## Deferred Ideas

- Stress/concurrency testing with multiple simultaneous clients — future performance phase
- Full 35-second timeout tests — optional test flag for thorough CI

</deferred>

---

*Phase: 05-cross-protocol-integration*
*Context gathered: 2026-02-05*
