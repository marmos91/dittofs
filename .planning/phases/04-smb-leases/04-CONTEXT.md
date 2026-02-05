# Phase 4: SMB Leases - Context

**Gathered:** 2026-02-05
**Status:** Ready for planning

<domain>
## Phase Boundary

Add SMB2/3 oplock and lease support integrated with the unified lock manager. SMB clients can acquire Read, Write, and Handle leases; the server sends break notifications on conflict; and all lease state flows through the existing lock infrastructure from Phase 1. This is internal protocol machinery enabling SMB client caching.

**Existing state:** SMB byte-range locks already use unified lock manager. SMB oplocks (`OplockManager` in handlers) are standalone and need integration.

</domain>

<decisions>
## Implementation Decisions

### Lease-to-Lock Mapping
- Direct mapping: R→shared read lock, W→exclusive write lock, H→handle caching lock
- Whole-file only (per SMB2/3 spec) — leases are file-level, byte-range locks remain separate
- Each lease component maps to a distinct lock type in the unified model

### Break Notification Behavior
- Force revoke on timeout — don't retry, just revoke and allow conflicting operation
- Client must flush dirty data before releasing Write lease (data integrity requirement)

### Lock Manager Integration
- Lease state lives inside EnhancedLock — extend existing type with lease fields (R/W/H flags)
- Lock manager remains protocol-agnostic; protocol translation happens in `internal/protocol/`
- Persist leases in LockStore like byte-range locks — survives server restart, enables reclaim
- Refactor existing `OplockManager` to delegate to unified lock manager (keep handler API stable)
- Convert from path-based keys to FileHandle-based keys (align with unified lock manager)

### Cross-Protocol Visibility
- Break on conflict: NLM write lock triggers SMB W/R lease break; SMB exclusive lease blocks NLM
- NFS read triggers SMB Write lease break (client flushes, then read proceeds)
- SMB clients can query all locks via unified model (full transparency across protocols)
- Single unified wait-for graph for deadlock detection (cross-protocol deadlock awareness)
- Debug logs only for cross-protocol conflicts (no metrics)

### Scope
- Full SMB2.1+ leases (R/W/H) — not just oplock improvement
- Design with NFSv4 delegations in mind (extensible for Phase 11)

### Claude's Discretion
- Break timeout value (consider Windows default 35s vs aggressive 5s)
- Read lease coexistence semantics (multiple R leases allowed per SMB spec)
- Conflict handling for lease acquisition (fail immediately vs queue)
- Lease expiration behavior with connection tracking
- Disconnect cleanup timing (immediate vs brief grace period)

</decisions>

<specifics>
## Specific Ideas

- "Unified Lock Manager should be protocol-agnostic, logic centralized, each protocol (NLM/NFSv4 and SMB2/SMB3) translates in internal/protocol module"
- Existing SMB byte-range locks already integrated — only oplocks need work
- Current `OplockManager` in `internal/protocol/smb/v2/handlers/oplock.go` has full break notification machinery — refactor to delegate to unified lock manager rather than replace

</specifics>

<deferred>
## Deferred Ideas

- NFSv4 delegations — Phase 11 (but design lease model to be extensible)
- Prometheus metrics for cross-protocol conflicts — decided against for now
- Configurable protocol priority for conflict resolution — decided against (first-come fairness)

</deferred>

---

*Phase: 04-smb-leases*
*Context gathered: 2026-02-05*
