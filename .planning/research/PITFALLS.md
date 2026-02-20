# NFSv4.1 Session Implementation Pitfalls

**Domain:** NFSv4.1 Sessions, EOS, Backchannel, Directory Delegations (adding to existing NFSv4.0 server)
**Researched:** 2026-02-20
**Confidence:** HIGH (based on RFC 8881, Linux kernel patches, nfs4j issues, vendor bug reports)

## Critical Pitfalls

Mistakes that cause session failures, data corruption, or client mount failures.

### Pitfall 1: Incomplete Replay Cache (Caching Status Instead of Full Response)

**What goes wrong:** The slot's replay cache stores only the NFS status code (or a partial response) instead of the complete COMPOUND4res byte stream. When a client retransmits a request (same slot, same seqid), the server returns a response that differs from the original.

**Why it happens:** Memory optimization instinct — "why store the whole response when we just need the status?" Also happens when developers cache per-operation results instead of the final serialized COMPOUND response.

**Consequences:** Client sees inconsistent state. For non-idempotent operations (WRITE, CREATE, REMOVE), this causes data loss or silent corruption. The client believes the operation succeeded/failed based on the cached status, but the actual result data (e.g., new filehandle from CREATE, new stateid from OPEN) is wrong or missing.

**Prevention:** Cache the complete serialized `COMPOUND4res` byte slice in the slot AFTER encoding the full response. Use `ChannelAttrs.MaxResponseSizeCached` to cap the cache size per slot — if a response exceeds this, mark the slot as uncacheable (sa_cachethis=false in SEQUENCE).

**Detection:** E2E test: send a CREATE request, note the returned filehandle. Kill the client mid-use, reconnect, retransmit the exact same COMPOUND. The cached response must contain the identical filehandle.

### Pitfall 2: SEQUENCE Lock Contention with StateManager

**What goes wrong:** SEQUENCE uses the StateManager's single RWMutex to look up the session and validate the slot. Since SEQUENCE runs on every single v4.1 request, all requests serialize on this lock, destroying throughput.

**Why it happens:** The existing v4.0 pattern uses a single RWMutex for all state operations. This works for v4.0 because state-modifying operations (OPEN, LOCK) are relatively infrequent compared to data operations (READ, WRITE). In v4.1, SEQUENCE runs on EVERY request.

**Consequences:** Throughput drops to single-threaded levels. Multiple connections (trunking) provide zero benefit.

**Prevention:** Use a two-level locking strategy:
1. StateManager.RLock() to look up the SessionRecord (fast, read-only)
2. Release StateManager lock
3. SlotTable.Lock() for the specific slot operation (per-session lock, no global contention)

The slot table must have its own mutex, separate from StateManager.

**Detection:** Benchmark with multiple concurrent clients. If throughput does not scale with client count, suspect lock contention.

### Pitfall 3: Backchannel Deadlock — Writing Callbacks While Holding State Lock

**What goes wrong:** The server needs to send CB_RECALL or CB_NOTIFY, which requires writing to a TCP connection. If the state lock is held during the write, and the fore channel is simultaneously trying to acquire the state lock to process a SEQUENCE, deadlock occurs.

**Why it happens:** Natural impulse to hold the lock while "completing" the callback for state consistency.

**Consequences:** Complete server hang. All NFS operations block indefinitely.

**Prevention:** Same pattern as existing v4.0 `sendRecall()` — read callback info under RLock, release lock, then do network I/O. For the backchannel, use a channel-based writer goroutine: state management code enqueues callback requests, the writer goroutine dequeues and sends without holding any state locks.

**Detection:** Under load with delegations active, if the server stops responding to all clients simultaneously, suspect this deadlock.

### Pitfall 4: Forgetting SEQUENCE-First Enforcement for v4.1

**What goes wrong:** The v4.1 COMPOUND path does not enforce that SEQUENCE is the first operation. A client sends a COMPOUND with PUTFH+READ without SEQUENCE, bypassing slot table validation entirely.

**Why it happens:** The v4.0 path does not have this requirement. When bifurcating the COMPOUND processor, the developer adds the v4.1 path but forgets the SEQUENCE-first check.

**Consequences:** Exactly-once semantics are silently broken. Retransmissions execute twice.

**Prevention:** The v4.1 COMPOUND path must:
1. Read the first opcode
2. If it is SEQUENCE: process normally
3. If it is one of the exempt ops (EXCHANGE_ID, CREATE_SESSION, DESTROY_SESSION, BIND_CONN_TO_SESSION, DESTROY_CLIENTID): process without SEQUENCE
4. Otherwise: return NFS4ERR_OP_NOT_IN_SESSION

**Detection:** E2E test: send a v4.1 COMPOUND without SEQUENCE as the first op. Must get NFS4ERR_OP_NOT_IN_SESSION.

### Pitfall 5: CREATE_SESSION Sequence Number Confusion

**What goes wrong:** CREATE_SESSION has its own sequence number (`csa_sequence`) independent from slot table sequences. Developers confuse the two.

**Why it happens:** Both are called "sequence numbers." CREATE_SESSION sequence is per-client, slot sequences are per-slot.

**Consequences:** CREATE_SESSION replay detection fails. Either retransmissions are rejected or duplicate sessions are created.

**Prevention:** The `ClientRecord` must have a separate `CreateSessionSeq` field. CREATE_SESSION validates against this field, not against any slot table. Rules:
- `csa_sequence == client.CreateSessionSeq + 1`: New request, create session
- `csa_sequence == client.CreateSessionSeq`: Replay, return cached session info
- Otherwise: NFS4ERR_SEQ_MISORDERED

**Detection:** E2E test: send CREATE_SESSION twice with the same sequence number. The second must return the cached session ID.

### Pitfall 6: Mixed v4.0/v4.1 Client State on Same Identity

**What goes wrong:** A client uses SETCLIENTID (v4.0) to establish a client ID, then later connects with EXCHANGE_ID (v4.1) using the same identity string. The server has a ClientRecord in v4.0 mode with state that is incompatible with v4.1 semantics.

**Why it happens:** Client software upgrades. Same identity string across versions.

**Consequences:** State corruption. Open-owner seqid validation fails on v4.1 requests, or session slot validation is bypassed for v4.0 requests.

**Prevention:** Each `ClientRecord` tracks `MinorVersion`. When EXCHANGE_ID arrives with a new verifier (client reboot), the old v4.0 record is replaced. Do NOT allow a single ClientRecord to serve both v4.0 and v4.1 concurrently.

**Detection:** E2E test: mount with v4.0, create files, unmount, remount with v4.1. All operations must work.

## Moderate Pitfalls

### Pitfall 7: Slot ID Out of Bounds

**What goes wrong:** Client sends `sa_slotid` >= number of allocated slots, causing array index panic.

**Prevention:** Check `sa_slotid < len(slotTable.slots)` before any slot access. Return NFS4ERR_BADSLOT.

### Pitfall 8: Not Handling Connection Disconnect for Bound Sessions

**What goes wrong:** A connection bound to a session closes. The session still references the closed connection for backchannel callbacks.

**Prevention:** When `NFSConnection.Serve()` exits, unbind the connection from any associated session. If it was the only backchannel connection, set `SEQ4_STATUS_CB_PATH_DOWN` in subsequent SEQUENCE responses.

### Pitfall 9: Forgetting to Update Sequence on Uncacheable Requests

**What goes wrong:** When `sa_cachethis = false`, the developer skips updating the slot entirely. But the sequence number must still advance.

**Prevention:** Always update `slot.SequenceID` on every request regardless of `sa_cachethis`. Only skip storing `slot.CachedReply` when `sa_cachethis` is false.

### Pitfall 10: Backchannel Slot Table Size Mismatch

**What goes wrong:** The server negotiates a backchannel slot table size in CREATE_SESSION but allocates a different number internally.

**Prevention:** Strictly honor the negotiated `BackChannel.MaxRequests` as the backchannel slot count. Backchannel typically needs 4-8 slots vs 64+ for fore channel.

### Pitfall 11: SEQUENCE Status Flags Not Updated

**What goes wrong:** The `sr_status_flags` field in SEQUENCE response is always zero. Client never learns that backchannel is down or delegations were revoked.

**Prevention:** Maintain a per-client status flags bitmap. Update it when backchannel breaks, delegations are revoked, or state expires. Clear flags once acknowledged.

## Minor Pitfalls

### Pitfall 12: Forgetting RECLAIM_COMPLETE

**What goes wrong:** Server does not implement RECLAIM_COMPLETE (op 58). Linux NFS clients send this after completing grace period reclaims. Without it, client gets NFS4ERR_NOTSUPP.

**Prevention:** Implement RECLAIM_COMPLETE as a simple operation that returns NFS4_OK and marks the client as having completed reclaim.

### Pitfall 13: Channel Attributes Not Respected

**What goes wrong:** Server negotiates MaxRequestSize in CREATE_SESSION but does not enforce it. Oversized requests cause buffer issues.

**Prevention:** After CREATE_SESSION, enforce MaxRequestSize as a hard limit on incoming COMPOUND size for that session.

### Pitfall 14: Directory Delegation Notification Overflow

**What goes wrong:** Many directory changes happen quickly. The server tries to send one CB_NOTIFY per change, overwhelming the backchannel.

**Prevention:** Coalesce notifications. If more than N notifications are pending, recall the delegation instead. Linux nfsd limits to one page of notifications.

## Phase-Specific Warnings

| Phase Topic | Likely Pitfall | Mitigation |
|-------------|---------------|------------|
| Slot table implementation | Pitfall 1 (incomplete replay cache) | Cache full COMPOUND4res bytes |
| SEQUENCE handler | Pitfall 2 (lock contention) | Separate SlotTable mutex |
| Backchannel multiplexing | Pitfall 3 (deadlock) | Channel-based writer goroutine |
| COMPOUND bifurcation | Pitfall 4 (missing SEQUENCE enforcement) | Whitelist exempt ops explicitly |
| CREATE_SESSION | Pitfall 5 (sequence confusion) | Separate CreateSessionSeq field |
| Version migration | Pitfall 6 (mixed v4.0/v4.1) | MinorVersion tracking on ClientRecord |
| Connection management | Pitfall 8 (disconnect handling) | Unbind on connection close |
| Directory delegations | Pitfall 14 (notification overflow) | Coalesce or recall threshold |

## Sources

- [RFC 8881: NFSv4.1 Protocol](https://www.rfc-editor.org/rfc/rfc8881.html) — HIGH confidence
- [Linux NFS Server 4.0/4.1 Issues](http://linux-nfs.org/wiki/index.php?title=Server_4.0_and_4.1_issues) — HIGH confidence
- [Linux nfsd NFSv4.1 Server Implementation](https://docs.kernel.org/filesystems/nfs/nfs41-server.html) — HIGH confidence
- [NetApp BIND_CONN_TO_SESSION performance issue](https://kb.netapp.com/on-prem/ontap/da/NAS/NAS-KBs/Slow_NFSv4.1_with_KRB5P_due_to_excessive_BIND_CONN_TO_SESSION_calls) — MEDIUM confidence
- [Linux kernel slot table deadlock patch](https://lkml.org/lkml/2025/12/31/580) — MEDIUM confidence
- Existing DittoFS source code (callback.go sendRecall pattern) — verified by direct reading
