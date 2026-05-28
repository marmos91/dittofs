# Phase 17: Slot Table and Session Data Structures - Research

**Researched:** 2026-02-20
**Domain:** NFSv4.1 session infrastructure -- slot tables, exactly-once semantics, sequence ID validation
**Confidence:** HIGH

## Summary

Phase 17 implements the core data structures for NFSv4.1 session-based exactly-once semantics (EOS). The slot table is the heart of NFSv4.1's replay detection mechanism, replacing the per-open-owner seqid tracking from v4.0 with a session-wide slot-based approach. Each session has a slot table where each slot tracks a sequence ID and optionally caches the full COMPOUND response for replay detection. The SEQUENCE operation (which must be the first op in every v4.1 COMPOUND) validates the slot+seqid combination and either processes a new request, returns a cached reply (retry), or rejects the request (misordered/false retry).

This phase builds **data structures and validation logic only** -- no SEQUENCE handler, no CREATE_SESSION handler, no wire protocol integration. The data structures will be consumed by Phase 18 (EXCHANGE_ID), Phase 19 (CREATE_SESSION/DESTROY_SESSION), and Phase 20 (SEQUENCE handler integration). The key deliverables are: `SlotTable` struct with per-slot mutex, `Slot` struct with seqid tracking and cached reply storage, sequence ID validation logic (retry/new/misordered/false-retry classification), and dynamic slot count adjustment via target_highest_slotid signaling.

**Primary recommendation:** Implement SlotTable as a standalone struct with a per-table mutex (not using the global StateManager RWMutex), where each slot stores its sequence ID and an optional cached COMPOUND response. The validation algorithm follows RFC 8881 Section 2.10.6.1 exactly.

<phase_requirements>
## Phase Requirements

| ID | Description | Research Support |
|----|-------------|-----------------|
| EOS-01 | Slot table caches full COMPOUND response for replay detection on duplicate requests | SlotTable.Slot stores CachedReply (raw XDR bytes of entire COMPOUND4res). When sa_cachethis=true and SEQUENCE succeeds, the complete response is cached. On retry (same slot+seqid), the cached response is returned verbatim. |
| EOS-02 | Sequence ID validation detects retries, misordered requests, and stale slots | ValidateSequenceID() returns one of: SeqIDNew (seqid == cached+1), SeqIDRetry (seqid == cached, slot not in-use), SeqIDMisordered (seqid > cached+1 or seqid < cached), SeqIDFalseRetry (seqid == cached but request differs from cache). Maps to NFS4ERR_SEQ_MISORDERED, NFS4ERR_SEQ_FALSE_RETRY. |
| EOS-03 | Server supports dynamic slot count adjustment via target_highest_slotid signaling | SlotTable tracks TargetHighestSlotID (desired max) and HighestSlotID (actual max used). SEQUENCE response includes both values. Server can lower TargetHighestSlotID to signal the client to use fewer slots. Client must comply by not using slots above target. |
</phase_requirements>

## Standard Stack

### Core

| Library | Version | Purpose | Why Standard |
|---------|---------|---------|--------------|
| Go stdlib `sync` | N/A | Mutex for SlotTable concurrency | Per-table mutex provides concurrency without serializing on global StateManager.mu |
| Go stdlib `time` | N/A | Slot last-used timestamps for debugging/metrics | Standard Go time package |

### Supporting

| Library | Version | Purpose | When to Use |
|---------|---------|---------|-------------|
| Existing `types` package | N/A | SessionId4, SequenceArgs/Res, ChannelAttrs types from Phase 16 | Wire types already defined; slot table references them |
| Existing `state` package | N/A | StateManager integration point (session maps added here) | Sessions owned by StateManager but with own mutexes |

### Alternatives Considered

| Instead of | Could Use | Tradeoff |
|------------|-----------|----------|
| Per-SlotTable mutex | Global StateManager.mu | Global mutex serializes ALL state operations; slot tables are per-session and should be independent |
| Per-Slot mutex | Per-SlotTable mutex | Per-slot is finer-grained but adds complexity; RFC says only one request per slot at a time, so table-level mutex is sufficient |
| sync.Map for slots | Fixed-size slice | Slots are numbered 0..N, a slice is the natural data structure |

## Architecture Patterns

### Recommended Project Structure

```
internal/protocol/nfs/v4/state/
    session.go        # Session struct (SessionID, ClientID, SlotTable ref, channel attrs)
    slot_table.go     # SlotTable and Slot structs, validation logic, dynamic sizing
    slot_table_test.go # Comprehensive unit tests for validation, concurrency, resizing
    session_test.go   # Session creation/destruction tests
    manager.go        # Extended: add session maps (sessionsByID, sessionsByClientID)
```

### Pattern 1: SlotTable with Per-Slot Sequence Tracking

**What:** Each session has a SlotTable containing a fixed-size array of Slots. Each Slot independently tracks its sequence ID and optionally caches the last COMPOUND response.

**When to use:** Every NFSv4.1 COMPOUND request passes through SEQUENCE which validates against the slot table.

**Key data structures:**

```go
// Slot represents a single slot in a session's slot table.
// Per RFC 8881 Section 2.10.6.1, each slot independently tracks
// its sequence ID and cached reply for exactly-once semantics.
type Slot struct {
    // SeqID is the sequence ID of the last successfully completed request
    // on this slot. Starts at 0; first valid request uses seqid=1.
    SeqID uint32

    // InUse indicates a request is currently being processed on this slot.
    // Prevents concurrent use of the same slot (only one request at a time).
    InUse bool

    // CacheThis indicates whether the response was requested to be cached.
    // Set from sa_cachethis in the SEQUENCE request.
    CacheThis bool

    // CachedReply holds the full XDR-encoded COMPOUND4res for replay detection.
    // nil if no reply has been cached (either first use or sa_cachethis was false).
    CachedReply []byte
}

// SlotTable manages the session's slot table for exactly-once semantics.
// Per RFC 8881, the slot table is the server's replay cache for a session.
type SlotTable struct {
    mu sync.Mutex

    // slots is a fixed-size array of slots. Index = slot ID.
    // Size is negotiated during CREATE_SESSION (from ca_maxrequests).
    slots []Slot

    // highestSlotID is the highest slot ID that has been used.
    // Returned as sr_highest_slotid in SEQUENCE response.
    highestSlotID uint32

    // targetHighestSlotID is the highest slot ID the server wants
    // the client to use. Returned as sr_target_highest_slotid.
    // The server can lower this to reclaim resources.
    targetHighestSlotID uint32

    // maxSlots is the total number of allocated slots (len(slots)).
    // This is the hard upper bound; targetHighestSlotID <= maxSlots-1.
    maxSlots uint32
}
```

### Pattern 2: Sequence ID Validation Algorithm

**What:** The exact algorithm from RFC 8881 Section 2.10.6.1 for validating a SEQUENCE request.

**When to use:** Called by the SEQUENCE handler (Phase 20) before processing any COMPOUND operations.

```go
// SequenceValidation classifies the result of validating a SEQUENCE request.
type SequenceValidation int

const (
    // SeqNew indicates this is a new (valid) request. Process normally.
    SeqNew SequenceValidation = iota

    // SeqRetry indicates this is a retransmission (same slot + same seqid).
    // Return the cached COMPOUND response if available.
    SeqRetry

    // SeqMisordered indicates the seqid is out of expected range.
    // Return NFS4ERR_SEQ_MISORDERED.
    SeqMisordered

    // SeqFalseRetry indicates the seqid matches the cached seqid but
    // the request differs (different operations). NFS4ERR_SEQ_FALSE_RETRY.
    SeqFalseRetry
)

// ValidateSequence validates a SEQUENCE request against the slot table.
//
// Per RFC 8881 Section 2.10.6.1:
//
// 1. If sa_slotid > highestSlotID allocated -> NFS4ERR_BADSLOT
// 2. Let cached_seqid = slot[sa_slotid].SeqID
// 3. If sa_sequenceid == cached_seqid + 1 -> new request (SeqNew)
//    - Check slot not in-use; if in-use, the client is broken
// 4. If sa_sequenceid == cached_seqid -> potential retry
//    - If slot NOT in-use AND cached reply exists -> SeqRetry
//    - If slot NOT in-use AND no cached reply -> NFS4ERR_RETRY_UNCACHED_REP
//    - If slot IS in-use -> the original is still executing (retry while in-flight)
// 5. If sa_sequenceid > cached_seqid + 1 -> SeqMisordered (gap)
// 6. If sa_sequenceid < cached_seqid -> SeqMisordered (already advanced past)
//
// False retry detection: when sa_sequenceid == cached_seqid AND the
// COMPOUND args differ from the cached request -> NFS4ERR_SEQ_FALSE_RETRY.
// (Implementation note: we do NOT store the request for comparison --
// false retry detection is optional per RFC 8881 Section 2.10.6.1.2.
// The server MAY return the cached reply instead. Linux nfsd does not
// implement false retry detection.)
func (st *SlotTable) ValidateSequence(slotID, seqID uint32) (SequenceValidation, error) {
    // ... implementation
}
```

### Pattern 3: Session Record

**What:** A Session record ties a session ID to a client, slot table, and channel attributes.

**When to use:** Created by CREATE_SESSION (Phase 19), referenced by SEQUENCE (Phase 20).

```go
// Session represents an NFSv4.1 session per RFC 8881 Section 2.10.
type Session struct {
    // SessionID is the unique 16-byte session identifier.
    SessionID types.SessionId4

    // ClientID is the server-assigned client ID that owns this session.
    ClientID uint64

    // ForeChannelSlots is the slot table for the fore channel
    // (client -> server requests).
    ForeChannelSlots *SlotTable

    // BackChannelSlots is the slot table for the back channel
    // (server -> client callbacks). May be nil if no back channel.
    BackChannelSlots *SlotTable

    // ForeChannelAttrs holds the negotiated fore channel attributes.
    ForeChannelAttrs types.ChannelAttrs

    // BackChannelAttrs holds the negotiated back channel attributes.
    BackChannelAttrs types.ChannelAttrs

    // Flags holds the CREATE_SESSION flags (e.g., CONN_BACK_CHAN).
    Flags uint32

    // CbProgram is the callback RPC program number.
    CbProgram uint32

    // CreatedAt is when this session was created.
    CreatedAt time.Time
}
```

### Pattern 4: Dynamic Slot Count Adjustment

**What:** The server can signal the client to use fewer (or more) slots by adjusting target_highest_slotid in the SEQUENCE response.

**When to use:** Resource management -- when the server is overloaded or the client is idle.

```go
// SetTargetHighestSlotID adjusts the target highest slot ID for the session.
// The client MUST NOT use slot IDs above this value.
// The server signals this change in every SEQUENCE response.
//
// Per RFC 8881 Section 2.10.6.1:
// - sr_target_highest_slotid <= sr_highest_slotid <= maxSlots-1
// - Lowering target tells client to stop using high-numbered slots
// - Client acknowledges by setting sa_highest_slotid in next SEQUENCE
// - Server can then safely shrink the slot table
func (st *SlotTable) SetTargetHighestSlotID(target uint32) {
    st.mu.Lock()
    defer st.mu.Unlock()
    if target < st.maxSlots {
        st.targetHighestSlotID = target
    }
}
```

### Anti-Patterns to Avoid

- **Using the global StateManager.mu for slot table operations:** This serializes all state operations across all sessions. Each SlotTable has its own mutex because slot validation is per-session and should not block other sessions.
- **Storing the full request for false retry detection:** RFC 8881 Section 2.10.6.1.2 says false retry detection is optional. Linux nfsd does not implement it. Do not store request data for comparison -- return cached reply on same-seqid instead.
- **Using a map instead of a slice for slots:** Slot IDs are consecutive integers 0..N. A slice is the natural, cache-friendly data structure.
- **Per-slot mutex:** A single mutex per SlotTable is sufficient because the protocol guarantees at most one outstanding request per slot (enforced by InUse flag). Multiple slots in the same table CAN be accessed concurrently from different goroutines, but the table-level mutex serialization is acceptable because slot validation is O(1) and very fast.
- **Sequence ID starting at 1:** Per RFC 8881, the initial sequence ID in the slot is 0 (uninitialized). The first valid request uses seqid=1. The slot stores 0 initially, so the first validation checks: is sa_sequenceid == 0+1 (=1)? Yes -> new request.

## Don't Hand-Roll

| Problem | Don't Build | Use Instead | Why |
|---------|-------------|-------------|-----|
| Session ID generation | Custom UUID generation | crypto/rand 16 bytes | RFC requires opaque 16-byte ID; crypto/rand is the standard way to generate unique opaque identifiers in Go |
| Sequence ID wrap-around | Complex modular arithmetic | Simple comparison: `seqid == cached + 1` with uint32 natural overflow | Per RFC 8881, sequence IDs are unsigned 32-bit integers. Go's uint32 wraps naturally at 0xFFFFFFFF -> 0, and 0 is a valid sequence ID in v4.1 (unlike v4.0 where 0 was special) |
| Thread-safe slot access | Lock-free data structures | sync.Mutex on SlotTable | Slot validation is O(1); mutex overhead is negligible; correctness matters more than lock-free performance |

**Key insight:** NFSv4.1 slot table is conceptually simple -- it's an array of (seqid, cached_reply) pairs with a validation function. The complexity is in getting the edge cases right (wraparound, in-use detection, false retry), not in the data structure itself.

## Common Pitfalls

### Pitfall 1: Confusing v4.0 per-owner seqid with v4.1 per-slot seqid
**What goes wrong:** Reusing the OpenOwner.ValidateSeqID pattern which expects seqid+1 with wrap to 1 (skipping 0).
**Why it happens:** v4.0 reserves seqid=0 for special stateids. v4.1 slot seqids start at 0 and wrap naturally (0xFFFFFFFF -> 0 is valid).
**How to avoid:** Write new validation logic for slot seqids. The initial slot seqid is 0; first request must use seqid=1. After that, natural uint32 overflow handles wrapping.
**Warning signs:** Using `nextSeqID()` from openowner.go which skips 0.

### Pitfall 2: Not caching the FULL COMPOUND response
**What goes wrong:** Only caching the SEQUENCE result or status code, not the entire COMPOUND response.
**Why it happens:** It seems wasteful to cache large responses.
**How to avoid:** The cached reply MUST be the complete COMPOUND4res (XDR-encoded). The client expects the full response on retry, including results from all operations in the compound. This is what `sa_cachethis` controls -- if true, cache the entire response.
**Warning signs:** On retry, client gets only SEQUENCE result instead of the full compound response.

### Pitfall 3: Holding the slot table mutex during COMPOUND execution
**What goes wrong:** Deadlock or extreme serialization of all requests within a session.
**Why it happens:** Acquiring the mutex for validation and not releasing it until the response is cached.
**How to avoid:** The validation flow should be: (1) lock table, (2) validate seqid + mark slot in-use, (3) unlock table, (4) execute COMPOUND, (5) lock table, (6) store cached reply + clear in-use + advance seqid, (7) unlock table.
**Warning signs:** Only one request at a time can execute per session.

### Pitfall 4: Mixing up the three highest_slotid values
**What goes wrong:** Returning wrong values in SEQUENCE response, causing client confusion.
**Why it happens:** Three similarly-named fields with subtle differences.
**How to avoid:** Understand clearly:
  - `sa_highest_slotid` (request): Client tells server "I might use slots up to this ID" -- informs server of client's current slot usage ceiling
  - `sr_highest_slotid` (response): Server tells client "slots 0..N are available" -- the actual number of allocated slots minus 1
  - `sr_target_highest_slotid` (response): Server tells client "please limit yourself to slots 0..N" -- desired maximum, may be less than sr_highest_slotid to reclaim resources
**Warning signs:** Client sends NFS4ERR_BAD_HIGH_SLOT after receiving valid SEQUENCE response.

### Pitfall 5: Not handling NFS4ERR_RETRY_UNCACHED_REP
**What goes wrong:** Client retries a request that was not cached, server crashes or returns wrong error.
**Why it happens:** When sa_cachethis was false on the original request, there is no cached reply to return on retry.
**How to avoid:** When seqid matches cached seqid but CachedReply is nil, return NFS4ERR_RETRY_UNCACHED_REP. This tells the client to retry with a new seqid (treating the slot as consumed).
**Warning signs:** Server returns empty/zero response on retry of uncached request.

### Pitfall 6: Session-to-StateManager integration scope creep
**What goes wrong:** Building the full session lifecycle (CREATE_SESSION, DESTROY_SESSION) in this phase.
**Why it happens:** It's tempting to wire everything together.
**How to avoid:** This phase builds data structures + validation logic + unit tests. Phase 18 adds EXCHANGE_ID (v4.1 client records). Phase 19 adds CREATE_SESSION/DESTROY_SESSION which instantiate sessions. Phase 20 adds the SEQUENCE handler. Keep this phase focused on the structs and their methods.
**Warning signs:** Writing handler code, touching compound.go, or modifying the dispatch table.

## Code Examples

Verified patterns based on RFC 8881 and existing DittoFS codebase conventions:

### Slot Table Creation

```go
// Source: RFC 8881 Section 18.36 (CREATE_SESSION negotiation)
// NewSlotTable creates a slot table with the given number of slots.
// numSlots comes from the negotiated ca_maxrequests in CREATE_SESSION.
func NewSlotTable(numSlots uint32) *SlotTable {
    if numSlots == 0 {
        numSlots = 1 // Minimum 1 slot per RFC
    }
    return &SlotTable{
        slots:               make([]Slot, numSlots),
        highestSlotID:       numSlots - 1,
        targetHighestSlotID: numSlots - 1,
        maxSlots:            numSlots,
    }
    // All slots initialized to SeqID=0, InUse=false, CachedReply=nil
}
```

### Sequence Validation (Core Algorithm)

```go
// Source: RFC 8881 Section 2.10.6.1
func (st *SlotTable) ValidateSequence(slotID, seqID uint32) (SequenceValidation, *Slot, error) {
    st.mu.Lock()
    defer st.mu.Unlock()

    // Step 1: Validate slot ID
    if slotID >= st.maxSlots {
        return 0, nil, &NFS4StateError{
            Status:  types.NFS4ERR_BADSLOT,
            Message: fmt.Sprintf("slot %d >= max %d", slotID, st.maxSlots),
        }
    }

    slot := &st.slots[slotID]
    cachedSeqID := slot.SeqID

    // Step 2: Check if this is the expected next sequence ID
    expectedSeqID := cachedSeqID + 1 // uint32 natural overflow handles wrap

    if seqID == expectedSeqID {
        // New request
        if slot.InUse {
            // Slot is still processing a previous request.
            // This shouldn't happen if the client follows the protocol.
            return 0, nil, &NFS4StateError{
                Status:  types.NFS4ERR_SEQ_MISORDERED,
                Message: "slot in use",
            }
        }
        return SeqNew, slot, nil
    }

    // Step 3: Check for retry (same seqid as cached)
    if seqID == cachedSeqID {
        if slot.InUse {
            // The original request is still executing.
            // RFC 8881: server MAY return NFS4ERR_DELAY.
            return 0, nil, &NFS4StateError{
                Status:  types.NFS4ERR_DELAY,
                Message: "slot in use, retry in flight",
            }
        }
        if slot.CachedReply == nil {
            // Original was not cached (sa_cachethis was false)
            return 0, nil, &NFS4StateError{
                Status:  types.NFS4ERR_RETRY_UNCACHED_REP,
                Message: "no cached reply for retry",
            }
        }
        // Valid retry: return cached reply
        return SeqRetry, slot, nil
    }

    // Step 4: Misordered (too far ahead or behind)
    return SeqMisordered, nil, &NFS4StateError{
        Status:  types.NFS4ERR_SEQ_MISORDERED,
        Message: fmt.Sprintf("seqid %d not expected (cached=%d, expected=%d)",
            seqID, cachedSeqID, expectedSeqID),
    }
}
```

### Caching a Reply After COMPOUND Execution

```go
// Source: RFC 8881 Section 2.10.6.1 -- cache reply after successful processing
func (st *SlotTable) CompleteSlotRequest(slotID uint32, seqID uint32, cacheThis bool, reply []byte) {
    st.mu.Lock()
    defer st.mu.Unlock()

    if slotID >= st.maxSlots {
        return
    }

    slot := &st.slots[slotID]
    slot.SeqID = seqID
    slot.InUse = false
    if cacheThis {
        // Store a copy of the full COMPOUND4res
        slot.CachedReply = make([]byte, len(reply))
        copy(slot.CachedReply, reply)
        slot.CacheThis = true
    } else {
        slot.CachedReply = nil
        slot.CacheThis = false
    }

    // Update highest slot ID tracking
    if slotID > st.highestSlotID {
        st.highestSlotID = slotID
    }
}
```

### Session Record Following Existing Patterns

```go
// Source: DittoFS pattern from state/client.go
// Session matches the existing state package conventions.
type Session struct {
    SessionID        types.SessionId4
    ClientID         uint64
    ForeChannelSlots *SlotTable
    BackChannelSlots *SlotTable
    ForeChannelAttrs types.ChannelAttrs
    BackChannelAttrs types.ChannelAttrs
    Flags            uint32
    CbProgram        uint32
    CreatedAt        time.Time
}

// NewSession creates a session for a client with the given channel attributes.
func NewSession(clientID uint64, foreAttrs, backAttrs types.ChannelAttrs, flags, cbProgram uint32) *Session {
    var sid types.SessionId4
    // Generate random session ID
    if _, err := rand.Read(sid[:]); err != nil {
        // Fallback: encode clientID + time for uniqueness
        binary.BigEndian.PutUint64(sid[:8], clientID)
        binary.BigEndian.PutUint64(sid[8:], uint64(time.Now().UnixNano()))
    }

    sess := &Session{
        SessionID:        sid,
        ClientID:         clientID,
        ForeChannelAttrs: foreAttrs,
        BackChannelAttrs: backAttrs,
        Flags:            flags,
        CbProgram:        cbProgram,
        CreatedAt:        time.Now(),
    }

    // Create slot tables from negotiated ca_maxrequests
    sess.ForeChannelSlots = NewSlotTable(foreAttrs.MaxRequests)
    if flags&types.CREATE_SESSION4_FLAG_CONN_BACK_CHAN != 0 {
        sess.BackChannelSlots = NewSlotTable(backAttrs.MaxRequests)
    }

    return sess
}
```

## State of the Art

| Old Approach (v4.0) | Current Approach (v4.1) | When Changed | Impact |
|---------------------|------------------------|--------------|--------|
| Per-open-owner seqid tracking | Per-slot seqid in session slot table | NFSv4.1 (RFC 5661/8881) | Eliminates per-owner replay caching; single unified replay cache per session |
| No exactly-once semantics | Slot-based EOS guarantees | NFSv4.1 | Server can guarantee a non-idempotent operation executes exactly once |
| No concurrency control | Slot-based request pipelining | NFSv4.1 | Client can have up to N concurrent requests (one per slot) |
| Unbounded replay cache | Bounded replay cache (N slots) | NFSv4.1 | Server knows exactly how much memory to allocate for replay cache |
| Per-connection state | Per-session state (multi-connection) | NFSv4.1 | Session state survives connection drops; multiple connections can share a session |

**Key insight for DittoFS:** The existing v4.0 `OpenOwner.ValidateSeqID` pattern provides a good template for the _logic_ (new/replay/bad), but the _scope_ is different (per-slot vs per-owner) and the _wrapping_ is different (v4.1 wraps through 0, v4.0 wraps 0xFFFFFFFF->1 skipping 0).

## Open Questions

1. **Maximum slot count default**
   - What we know: Linux nfsd defaults to 160 slots (DRC_SLOTS_DEF). ca_maxrequests from CREATE_SESSION is the client's proposed limit.
   - What's unclear: What should DittoFS's default/maximum be?
   - Recommendation: Default to 64 slots (reasonable for a single-node server). Make it configurable. The server can clamp the client's requested value down during CREATE_SESSION negotiation.

2. **False retry detection**
   - What we know: RFC 8881 Section 2.10.6.1.2 says false retry detection is OPTIONAL. Linux nfsd does not implement it.
   - What's unclear: Should DittoFS implement it?
   - Recommendation: Do NOT implement false retry detection. Return the cached reply on same-seqid match (like Linux nfsd). This avoids storing request data and simplifies the implementation.

3. **Back channel slot table**
   - What we know: The back channel (server->client callbacks) also has a slot table.
   - What's unclear: Does Phase 17 need to build the back channel slot table?
   - Recommendation: Yes, build the same SlotTable struct and let Session hold both fore and back channel slot tables. The back channel table is structurally identical; Phase 22 will use it for CB_SEQUENCE.

## Sources

### Primary (HIGH confidence)
- RFC 8881 Section 2.10 (Session), Section 2.10.6.1 (Slot/Sequencing) -- NFSv4.1 session and slot table specification
- RFC 8881 Section 18.46 (SEQUENCE operation) -- slot validation algorithm, error codes
- RFC 8881 Section 18.36 (CREATE_SESSION) -- slot table initialization from channel attributes
- Existing DittoFS `internal/protocol/nfs/v4/state/manager.go` -- StateManager patterns to follow
- Existing DittoFS `internal/protocol/nfs/v4/state/openowner.go` -- SeqID validation template (v4.0 analog)
- Existing DittoFS `internal/protocol/nfs/v4/types/sequence.go` -- Wire types (SequenceArgs, SequenceRes) from Phase 16
- Existing DittoFS `internal/protocol/nfs/v4/types/create_session.go` -- ChannelAttrs from Phase 16
- Existing DittoFS `internal/protocol/nfs/v4/types/session_common.go` -- SessionId4, ChannelAttrs from Phase 16

### Secondary (MEDIUM confidence)
- [Linux kernel nfsd documentation](https://docs.kernel.org/filesystems/nfs/nfs41-server.html) -- confirms nfsd implements mandatory sessions with EOS
- [NFSD slot table kernel patch](https://patchwork.kernel.org/project/linux-nfs/patch/1354159063-17343-4-git-send-email-Trond.Myklebust@netapp.com/) -- nfsd4_slot_table structure, target_highest_slotid tracking
- [IETF sequence ID calibration draft](https://www.ietf.org/archive/id/draft-mzhang-nfsv4-sequence-id-calibration-00.html) -- clarifies misordered detection: "difference of 2+ or less than cached"

### Tertiary (LOW confidence)
- [Linux NFS wiki: Client sessions issues](http://wiki.linux-nfs.org/wiki/index.php/Client_sessions_Implementation_Issues) -- client-side perspective on slot table edge cases
- [FreeBSD nfscl slot table patch](https://www.mail-archive.com/dev-commits-src-main@freebsd.org/msg37441.html) -- re-initialization of seqids on server shrink

## Metadata

**Confidence breakdown:**
- Standard stack: HIGH - Pure Go stdlib, no external dependencies, follows existing DittoFS patterns
- Architecture: HIGH - RFC 8881 prescribes the exact data structures and validation algorithm; cross-verified with Linux nfsd
- Pitfalls: HIGH - Well-known pitfalls from RFC and existing v4.0 implementation experience in this codebase

**Research date:** 2026-02-20
**Valid until:** 2026-04-20 (stable -- RFC 8881 is a finalized standard, unlikely to change)
