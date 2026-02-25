# Phase 24: Directory Delegations - Context

**Gathered:** 2026-02-22
**Status:** Ready for planning

<domain>
## Phase Boundary

Server grants directory delegations via GET_DIR_DELEGATION and notifies clients of directory changes via CB_NOTIFY over the backchannel. Extends the existing file delegation pattern (StateManager, CB_RECALL) to directories with notification subscription support. Depends on phase 22 (backchannel multiplexing) infrastructure.

</domain>

<decisions>
## Implementation Decisions

### Notification Granularity
- Support full notification set from day one: ADD, REMOVE, RENAME, ATTR_CHANGE
- Honor client's requested notification bitmask — only send notifications for subscribed types
- Include full entry info in CB_NOTIFY (name, cookie, attributes) so clients can update cache without READDIR
- Batch notifications within a configurable time window (exposed in NFS adapter config section)
- Cross-directory renames: notify both source and destination directory delegation holders
- Include readdir cookie for affected entries in notifications

### Delegation Granting Policy
- All directories eligible for delegation (including export root)
- Configurable server-wide limit on total outstanding directory delegations
- Delegation lives until recalled or returned — no TTL auto-expiry (matches standard lease model)
- Require valid lease before granting — refuse if client has expired lease
- DESTROY_CLIENTID must auto-revoke all directory delegations for that client

### Recall & Conflict Behavior
- Use lease timeout as the grace period after sending recall before forced revocation
- Flush any batched notifications before acknowledging DELEGRETURN — client gets complete picture
- Track recall reason (conflict, resource pressure, admin action) for metrics and logging

### State Tracking Model
- Extend existing DelegationState struct with notification-specific fields (notification bitmask, pending notifications, dir flag)
- Index by file handle — same lookup pattern as file delegations
- Ephemeral state (lost on restart) — consistent with current file delegation model
- Prometheus metrics: shared with file delegations using a 'type' label (file vs directory) — no separate metric names
- TEST_STATEID and FREE_STATEID: Claude to ensure directory delegation stateids are handled properly

### Code Structure and Design
- Notification batching logic lives in StateManager
- Directory mutation handlers (CREATE, REMOVE, RENAME, etc.) call StateManager.NotifyDirChange() directly after successful mutation
- GET_DIR_DELEGATION implemented as standalone handler registered in dispatch table
- CB_NOTIFY reuses existing BackchannelSender goroutine from phase 22
- XDR types for CB_NOTIFY and GET_DIR_DELEGATION added to existing xdr package
- Update docs/NFS.md with directory delegation section
- Extra resilience: defensive handling for delegation on deleted dir, double-grant, stale handle edge cases

### Claude's Discretion
- Rename notification encoding: single event vs decomposed remove+add (RFC 5661 notify4 semantics)
- CB_NOTIFY delivery failure handling: retry policy before revocation (match existing CB_RECALL failure handling)
- Notification batcher architecture: per-directory vs global (fit existing goroutine model)
- Batch count limit vs time-only flushing (consider CB_NOTIFY message size)
- Attr change notification scope: which attribute changes are "significant" enough to trigger notify
- Delegation granting heuristics (always grant vs heuristic-based, per RFC guidance)
- Multiple clients holding directory delegations simultaneously (RFC semantics)
- Behavior at delegation limit: refuse vs recall LRU
- Proactive delegation offering on READDIR vs explicit GET_DIR_DELEGATION only
- Conflicting operation blocking: block until delegation returned vs proceed immediately after recall
- CB_RECALL_ANY for directory delegations under memory pressure
- Recall on directory deletion: recall first vs revoke immediately
- Cascade recall: whether recalling dir delegation also recalls file delegations within
- FREE_STATEID scope for directory delegation stateids
- Per-client shared vs separate limits for file and directory delegations
- Test depth: appropriate testing patterns based on previous phases

</decisions>

<specifics>
## Specific Ideas

- Batching window should be configurable in the NFS adapter config section (not global server config)
- Recall reason tracking feeds into existing Prometheus metrics infrastructure — operators should be able to see why delegations are being recalled
- "Full entry info" in notifications means the client should never need to re-READDIR after getting a CB_NOTIFY — the notification is self-contained
- Follow the exact same StateManager patterns used for file delegations to keep the codebase consistent

</specifics>

<deferred>
## Deferred Ideas

None — discussion stayed within phase scope

</deferred>

---

*Phase: 24-directory-delegations*
*Context gathered: 2026-02-22*
