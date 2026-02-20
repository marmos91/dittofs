# Research Summary: NFSv4.1 Session Infrastructure (v3.0)

**Domain:** NFSv4.1 Protocol Server — Sessions, EOS, Backchannel, Directory Delegations
**Researched:** 2026-02-20
**Overall confidence:** HIGH

## Executive Summary

NFSv4.1 sessions are the most architecturally significant change since introducing stateful operations in v4.0. Sessions replace the NFSv4.0 per-owner seqid replay model with a per-session slot table that provides exactly-once semantics (EOS) for ALL operations. This is not an incremental feature addition — it is a fundamental change to how every request is sequenced, replayed, and associated with connections.

The existing DittoFS NFSv4.0 infrastructure (StateManager with single RWMutex, COMPOUND dispatcher with streaming XDR decode, per-connection goroutine model, callback client) provides a solid foundation. However, the COMPOUND dispatcher must be bifurcated (v4.0/v4.1 paths), the StateManager must gain session and slot table data structures, the callback system must be rewritten to use backchannel multiplexing on existing connections, and the NFSConnection must support bidirectional I/O.

The research identified 14 specific pitfalls, 6 of which are critical (incomplete replay cache, lock contention, backchannel deadlock, missing SEQUENCE enforcement, CREATE_SESSION sequence confusion, and mixed v4.0/v4.1 state). All have clear mitigations documented in PITFALLS.md.

No new external dependencies are required. All v4.1 features build on Go stdlib primitives. The estimated scope is approximately 15 new NFSv4.1 operations to implement, 5 new callback operations, 4-5 new state structures, and significant modifications to 3 existing components (ProcessCompound, StateManager, NFSConnection).

## Key Findings

**Stack:** No new dependencies. Pure Go stdlib implementation. Session IDs via crypto/rand, slot tables as fixed-size slices with per-table mutexes.

**Architecture:** Sessions sit between the connection layer and operation handlers, mediating every v4.1 request via SEQUENCE. The slot table provides EOS with full-response replay caching. Backchannel multiplexing replaces separate TCP connections for callbacks.

**Critical pitfall:** Lock contention — SEQUENCE runs on every request and must not serialize on StateManager's global RWMutex. Solution is two-level locking: StateManager RLock for session lookup, then per-SlotTable mutex for slot operations.

## Implications for Roadmap

Based on research, suggested phase structure:

1. **Types and Constants** - Pure additions, no existing code modified
   - Addresses: NFSv4.1 operation numbers (40-58), callback ops (5-14), error codes
   - Avoids: Breaking existing v4.0 constants

2. **Slot Table and Session Data Structures** - Testable in isolation
   - Addresses: SlotTable, SessionRecord, ChannelAttrs, EOS replay cache
   - Avoids: Pitfall 2 (lock contention) via separate per-SlotTable mutex
   - Avoids: Pitfall 1 (incomplete replay cache) by designing for full-response caching

3. **EXCHANGE_ID Handler** - First v4.1 wire-level operation
   - Addresses: Client registration for v4.1, ClientRecord v4.1 fields
   - Avoids: Pitfall 6 (mixed v4.0/v4.1) via MinorVersion tracking

4. **CREATE_SESSION / DESTROY_SESSION** - Session lifecycle
   - Addresses: Session creation with slot table allocation, connection binding
   - Avoids: Pitfall 5 (sequence confusion) via separate CreateSessionSeq
   - Avoids: Pitfall 10 (slot table size mismatch)

5. **SEQUENCE and COMPOUND Bifurcation** - Critical integration point
   - Addresses: v4.1 request processing, EOS, lease renewal, owner-seqid bypass
   - Avoids: Pitfall 4 (missing SEQUENCE enforcement) via explicit exempt-op whitelist
   - Avoids: Pitfall 9 (uncacheable sequence update) by always advancing sequence

6. **BIND_CONN_TO_SESSION and Connection Management** - Trunking and reconnection
   - Addresses: Multi-connection per session, backchannel binding
   - Avoids: Pitfall 8 (disconnect handling)

7. **Backchannel Multiplexing** - NAT-friendly callbacks
   - Addresses: CB_SEQUENCE, bidirectional I/O on existing connections
   - Avoids: Pitfall 3 (deadlock) via channel-based writer goroutine

8. **Directory Delegations** - Differentiator feature
   - Addresses: GET_DIR_DELEGATION, CB_NOTIFY notifications
   - Avoids: Pitfall 14 (notification overflow) via coalescing/recall threshold

9. **Cleanup Operations** - Production completeness
   - Addresses: DESTROY_CLIENTID, FREE_STATEID, TEST_STATEID, RECLAIM_COMPLETE
   - Avoids: Pitfall 12 (missing RECLAIM_COMPLETE)

10. **E2E Testing** - Verification
    - Tests all of the above with Linux NFS client (vers=4.1)

**Phase ordering rationale:**
- Types/constants first because all subsequent phases depend on them
- Slot table before handlers because EXCHANGE_ID/CREATE_SESSION need the data structures
- EXCHANGE_ID before CREATE_SESSION because sessions require a client ID
- CREATE_SESSION before SEQUENCE because SEQUENCE requires a session
- SEQUENCE before backchannel because backchannel uses CB_SEQUENCE (same pattern)
- Connection management before backchannel because backchannel needs bound connections
- Directory delegations last because they depend on working backchannel

**Research flags for phases:**
- Phase 5 (SEQUENCE + COMPOUND bifurcation): Most complex integration, touches every request path. Needs careful testing.
- Phase 7 (Backchannel): Requires bidirectional I/O on TCP connections, which is a new pattern for DittoFS.
- Phase 8 (Directory delegations): May need deeper research into notification coalescing strategies.

## Confidence Assessment

| Area | Confidence | Notes |
|------|------------|-------|
| Stack | HIGH | No new deps, stdlib only |
| Features | HIGH | RFC 8881 is definitive, Linux client behavior well-documented |
| Architecture | HIGH | Existing codebase fully read, integration points clear |
| Pitfalls | HIGH | Based on RFC, Linux kernel patches, vendor bug reports |

## Gaps to Address

- **Bidirectional TCP I/O pattern:** The existing NFSConnection read loop is strictly unidirectional (client sends, server responds). Backchannel multiplexing requires the server to write unsolicited CB_COMPOUND messages on the same connection. Phase 7 will need to design the goroutine architecture for this. Possible approach: reader goroutine + writer goroutine + response channel, similar to HTTP/2 stream multiplexing.

- **SEQUENCE status flag management:** The exact bookkeeping for when to set and clear `sr_status_flags` bits needs detailed design. The RFC specifies the flags but leaves implementation strategies to the server.

- **Session persistence:** The v3.0 scope explicitly excludes persistent sessions (survive server restart). However, if future requirements demand this, the SlotTable replay cache would need to be backed by persistent storage, which is a significant complexity increase.

- **Channel attribute negotiation strategy:** The server must decide how many slots to offer per session. Too few limits throughput; too many waste memory. Linux nfsd defaults to 160 slots with a per-session cap. DittoFS should start with a configurable default (64) and refine based on benchmarking.
