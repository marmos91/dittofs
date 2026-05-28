# Phase 20: SEQUENCE and COMPOUND Bifurcation - Context

**Gathered:** 2026-02-21
**Status:** Ready for planning

<domain>
## Phase Boundary

Gate every v4.1 COMPOUND with SEQUENCE validation providing exactly-once semantics. Bifurcate the COMPOUND dispatcher so minorversion=0 routes to existing v4.0 path and minorversion=1 routes to new v4.1 path with SEQUENCE enforcement. v4.0 clients continue working unchanged. Per-owner seqid validation is bypassed for v4.1 operations since the slot table provides replay protection.

</domain>

<decisions>
## Implementation Decisions

### Replay cache policy
- Strict RFC slot limits: slot count negotiated at CREATE_SESSION and fixed. Excess requests get NFS4ERR_DELAY
- Retention until slot reused: cached response lives until client advances seqid on that slot. No timer-based eviction
- Full encoded XDR response cached per slot: byte-identical replay, no re-encoding. O(1) replay performance
- No global memory cap: each session gets its negotiated slots. Total memory bounded by (sessions x slots x max_response). Session reaper handles cleanup
- Return cached response on replay regardless of argument differences: match by slot+seqid only per RFC
- Lock-free atomics for slot table operations (acquire, release, replay hit)
- Reject immediately (NFS4ERR_DELAY) when SEQUENCE arrives for an in-flight slot. No queuing
- Cache key is (sessionID, slotID) only. Any connection bound to the session can get the replay

### Error response behavior
- RFC error codes to client + detailed server-side logging (session ID, expected vs actual seqid, slot state)
- Full partial results in COMPOUND: return results for all ops up to and including the failing one
- SEQUENCE is pure gating: only validates slot/seqid/session. Semantic conflicts caught by individual operation handlers
- Unified logging: SEQUENCE errors follow the global log level. No separate session-specific log level
- Always include sa_status_flags in SEQUENCE response, even on errors (lease/callback/recallable status)
- All sa_status_flags reported from this phase: lease expiry, backchannel fault, recallable locks, devid notifications (even if some subsystems not yet active)
- No operation count limit on COMPOUNDs: max request size from session negotiation already caps payload

### v4.0 regression safety
- Dedicated types for v4.0 and v4.1 with shared operation handlers: separate COMPOUND/dispatch types per minor version, common ops imported from v4.0
- Dedicated v4.1 COMPOUND response type: wraps SEQUENCE result + operation results. Compile-time enforcement of SEQUENCE-first
- Handler wrapper pattern for seqid bypass: v4.1 dispatch strips per-owner seqid before calling shared handler. Handler never sees v4.0 seqid concerns
- Mixed minorversion per connection: a single TCP connection can carry both v4.0 and v4.1 COMPOUNDs interleaved
- Echo COMPOUND tag verbatim: copy request tag to response as-is, no validation
- Existing v4.0 tests + new coexistence tests: run all v4.0 COMPOUND scenarios through new dispatcher AND add interleaved v4.0/v4.1 tests
- Concurrent mixed traffic tests: goroutines sending v4.0 and v4.1 COMPOUNDs simultaneously to catch race conditions
- Configurable minor versions via min/max range in NFS adapter config (control plane, per-adapter): nfs_min_minor_version / nfs_max_minor_version. Full stack: model + REST API + dfsctl commands

### Observability
- Minimal Prometheus metrics: sequence_total, sequence_errors_total, replay_hits_total counters
- Per-session slot utilization gauge: slots_in_use / total_slots per session
- Replay cache memory gauge: total bytes consumed by cached responses across all sessions
- Per-error-type counters for SEQUENCE failures: bad_session, seq_misordered, replay_hit, slot_busy
- No OpenTelemetry tracing for now: may add later
- Successful SEQUENCE: DEBUG level logging
- Replay cache hits: INFO level logging (noteworthy production events)
- Always log bifurcation routing at DEBUG: which path each COMPOUND takes (v4.0 or v4.1)
- Log full COMPOUND operation list at DEBUG with XDR-encoded size per operation
- Log operation list as [SEQUENCE, PUTFH, OPEN, GETATTR] format at DEBUG for troubleshooting

### Code structure and design
- v4.1 COMPOUND processor in internal/protocol/nfs/v41/ (parallel to existing v3/)
- SEQUENCE handler and slot table logic co-located in v41 package
- Static switch for v4.1 operation dispatch (compile-time checked)
- Import v4.0 handlers directly for shared operations (READ, WRITE, GETATTR, etc.)
- Testing with real in-memory components: real SessionManager, real SlotTable, real dispatch. No mocks
- Table-driven unit tests for SEQUENCE validation edge cases + integration tests for end-to-end COMPOUND flows
- Go benchmark test for SEQUENCE validation + COMPOUND dispatch throughput
- Session-aware request context (SessionContext) flows through all v4.1 operation handlers after SEQUENCE succeeds, carrying session info, slot reference, client ID

### Claude's Discretion
- Dynamic slot table resizing (highest_slotid / target_highest_slotid): Claude decides based on RFC compliance vs complexity tradeoff
- Specific RFC error for missing SEQUENCE in v4.1 COMPOUND: Claude picks NFS4ERR_OP_NOT_IN_SESSION vs NFS4ERR_SEQUENCE_POS
- Stale seqid handling (old vs future): Claude picks based on RFC 8881 Section 18.46
- SessionContext design: whether to extend AuthContext or wrap it as separate struct

</decisions>

<specifics>
## Specific Ideas

- "I am starting to think to split 4.0 from 4.1 completely in the codebase" — dedicated types per version, shared handlers. Import v4.0 handlers now, create GH issue to refactor into shared ops package later
- Min/max minor version range for version control, same pattern as Linux NFS client `nfsvers=` configuration
- Version range is per-adapter configuration in the control plane (not static config file), full stack with REST API and dfsctl
- No mocks in tests — use real in-memory components throughout

</specifics>

<deferred>
## Deferred Ideas

- Refactor NFS operations into shared ops package (internal/protocol/nfs/ops/) grouping common handlers for v4.0/v4.1/future — create GH issue
- OpenTelemetry tracing for SEQUENCE operations — may add in a later phase

</deferred>

---

*Phase: 20-sequence-and-compound-bifurcation*
*Context gathered: 2026-02-21*
