# Phase 3: NSM Protocol - Context

**Gathered:** 2026-02-05
**Status:** Ready for planning

<domain>
## Phase Boundary

Implement Network Status Monitor (RPC 100024) for crash recovery. Server monitors registered clients and detects crashes. Client crash triggers automatic lock cleanup. Server restart sends SM_NOTIFY to registered clients. Clients can reclaim locks during grace period.

**Key architectural decision:** NSM protocol details stay in `internal/protocol/nsm/`. Abstract client monitoring lives in `pkg/metadata/` via extended ConnectionTracker. Metadata store persists protocol-agnostic client registrations.

</domain>

<decisions>
## Implementation Decisions

### Abstraction Layer
- Extend existing `ConnectionTracker` from Phase 1 (not new type)
- NSM handlers translate between wire format and ConnectionTracker abstraction
- Protocol-agnostic client registration stored in metadata store
- Same abstraction can be reused by SMB for durable handles

### Client Crash Detection
- SM_MON callback-based detection only (no proactive heartbeat/polling)
- 5 second total timeout for callbacks (consistent with NLM_GRANTED)
- Track client state counter (sm_state) per RFC — increment on restart
- Persisted client list in metadata store — survives server restart
- Full SM_UNMON support for clean unregistration
- Update existing registration on duplicate SM_MON from same client
- Configurable client limit (default 10,000) to prevent resource exhaustion
- Implement SM_STAT to return current server state

### Lock Cleanup Policy
- Immediate cleanup when crash detected (no delay/grace window)
- Process NLM blocking queue waiters when crashed client's locks released
- Implement FREE_ALL (NLM procedure 17) for bulk lock release
- Best effort cleanup — continue releasing other locks if one fails, log errors

### Server Restart & SM_NOTIFY
- Send SM_NOTIFY to all registered clients in parallel (fastest recovery)
- If SM_NOTIFY fails to reach client, mark as crashed and cleanup their locks
- Grace period blocks ALL new lock requests (RFC compliant)
- Only lock reclaims allowed during grace period

### Recovery Window Configuration
- Shared default recovery window (90 seconds) at ConnectionTracker level
- Protocol-specific override capability (NLM grace period, SMB durable handle timeout)
- NLM uses as grace period (blocks new locks, allows reclaims)
- SMB will use as durable handle timeout (handles preserved for reconnection)

### State Persistence
- Client registrations stored in metadata store (memory/badger/postgres)
- Reuse server epoch from Phase 1 lock persistence for sm_state
- Registrations cleared when metadata store is cleared (consistent behavior)
- REST API endpoint for listing/inspecting client registrations

### Claude's Discretion
- Exact callback client implementation details
- Internal data structures for tracking registrations
- Prometheus metrics design for NSM operations
- Error message wording

</decisions>

<specifics>
## Specific Ideas

- ConnectionTracker abstraction enables future SMB durable/persistent handle support using same infrastructure
- Server epoch already exists from Phase 1 — reuse for sm_state
- Parallel SM_NOTIFY mirrors the parallel waiter processing pattern from Phase 2
- FREE_ALL integration: NLM procedure 17, used by rpc.statd on client reboot

</specifics>

<deferred>
## Deferred Ideas

None — discussion stayed within phase scope

</deferred>

---

*Phase: 03-nsm-protocol*
*Context gathered: 2026-02-05*
