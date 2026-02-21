# internal/protocol

Low-level protocol implementations - NFS and SMB wire formats and handlers.

## Layer Responsibilities

**This layer handles ONLY protocol concerns:**
- XDR/SMB2 encoding/decoding
- RPC message framing
- Procedure dispatch
- Wire type ↔ internal type conversion

**Business logic belongs in pkg/metadata and pkg/blocks.**

## NFS (`nfs/`)

### Directory Structure
```
dispatch.go     - RPC routing, auth extraction
rpc/            - RPC call/reply handling
xdr/            - XDR encoding/decoding
types/          - NFS constants and error codes
mount/handlers/ - MOUNT protocol (MNT, UMNT, EXPORT, DUMP)
v3/handlers/    - NFSv3 procedures (READ, WRITE, LOOKUP, etc.)
```

### Auth Context Threading
```
RPC Call → ExtractAuthContext() → Handler → Service → Store
```
- Created in `dispatch.go:ExtractHandlerContext()`
- Export-level squashing (AllSquash, RootSquash) applied at mount time

### Two-Phase Write Pattern
```
PrepareWrite() → [content store write] → CommitWrite()
```
- `PrepareWrite`: Validates, returns intent (no metadata changes)
- `CommitWrite`: Applies size/time updates atomically

### Buffer Pooling
Three-tier pools (4KB, 64KB, 1MB) in `pkg/bufpool/`. Reduces GC ~90%.

## SMB (`smb/`)

### Directory Structure
```
header/   - SMB2 header parsing
rpc/      - SMB-RPC handling
session/  - Session state machine
signing/  - Message signing
types/    - SMB2 constants
v2/       - SMB2 command handlers
```

## NFSv4.0/v4.1 Coexistence

### Version Routing

COMPOUND dispatcher routes based on `minorversion`:
- `0` -> v4.0 dispatch table (OpHandler signature)
- `1` -> v4.1 dispatch table (V41OpHandler signature), with fallback to v4.0 table for shared ops
- `2+` -> NFS4ERR_MINOR_VERS_MISMATCH

### Handler Signatures

v4.0 and v4.1 use different handler types:

```go
// v4.0: no session context
type OpHandler func(ctx *types.CompoundContext, reader io.Reader) *types.CompoundResult

// v4.1: includes session context from SEQUENCE
type V41OpHandler func(ctx *types.CompoundContext, v41ctx *types.V41RequestContext, reader io.Reader) *types.CompoundResult
```

### Dispatch Table Strategy

- **v4.0-only ops** (3-39): Only in `opDispatchTable`, accessible from both v4.0 and v4.1 compounds
- **v4.1-only ops** (40-58): Only in `v41DispatchTable`, return OP_ILLEGAL in v4.0 compounds
- **Shared ops** (PUTFH, GETATTR, READ, WRITE, etc.): In `opDispatchTable`, called from v4.1 compounds via fallback
- **v4.0-only rejected in v4.1**: SETCLIENTID, SETCLIENTID_CONFIRM, RENEW, OPEN_CONFIRM, RELEASE_LOCKOWNER -> NFS4ERR_NOTSUPP in v4.1 (Phase 23)

### Type File Organization

- v4.0 types: `types.go` (compound structs, Stateid4, etc.)
- v4.1 per-op types: `exchange_id.go`, `create_session.go`, etc. (one file per operation)
- v4.1 shared types: `session_common.go` (types used by 2+ operations)
- Constants: `constants.go` (both v4.0 and v4.1, separated by comment blocks)
- Error codes: `errors.go` (both v4.0 and v4.1)

### Adding a New v4.1 Handler (Phases 17-24)

1. Create handler in `v4/handlers/` (e.g., `exchange_id_handler.go`)
2. Use V41OpHandler signature with V41RequestContext
3. Replace stub in v41DispatchTable with real handler in NewHandler()
4. The stub automatically consumed args -- the real handler replaces this
5. Test with compound_test.go pattern

### NFSv4.1 Session Handler Conventions (Phase 19)

**Handler pattern (CREATE_SESSION, DESTROY_SESSION):**
- File naming: `{op_name}_handler.go` (e.g., `create_session_handler.go`)
- Decode XDR args -> validate callback security -> delegate to StateManager -> encode response -> cache replay bytes
- CREATE_SESSION caches response bytes AFTER encoding via `StateManager.CacheCreateSessionResponse()`. The handler owns encoding; the StateManager owns the replay detection algorithm.

**State management:**
- `StateManager.sessionsByID` maps SessionId4 -> *Session
- `StateManager.sessionsByClientID` maps clientID -> []*Session
- `CreateSession()` implements RFC 8881 multi-case replay detection (same seqid=replay, seqid+1=new, other=misordered)
- `DestroySession()` checks for in-flight requests before destroying (NFS4ERR_DELAY)
- `ForceDestroySession()` bypasses in-flight check (admin force-destroy)
- `purgeV41Client()` destroys all sessions before purging client

**Reaper goroutine:**
- `StartSessionReaper(ctx)` sweeps every 30s for lease-expired and unconfirmed clients
- Unconfirmed client timeout: 2x lease duration

**Prometheus metrics (session_metrics.go):**
- `dittofs_nfs_sessions_created_total` (counter)
- `dittofs_nfs_sessions_destroyed_total` (counter, label: reason)
- `dittofs_nfs_sessions_active` (gauge)
- `dittofs_nfs_sessions_duration_seconds` (histogram)
- All metric calls are nil-safe via receiver methods on a possibly-nil `*SessionMetrics` (no explicit nil check needed in StateManager)

**Logging:** INFO for session create/destroy, DEBUG for expected errors (not found, replay)

**Config:** `NFSAdapterSettings.V4MaxSessionSlots` (default 64) and `V4MaxSessionsPerClient` (default 16) exist but are NOT yet wired to StateManager. Future: settings watcher integration.

### SEQUENCE-Gated Dispatch (Phase 20)

Every non-exempt NFSv4.1 COMPOUND must begin with SEQUENCE (RFC 8881). The dispatcher
enforces this in `dispatchV41()`:

1. Read first opcode
2. If exempt (`isSessionExemptOp`): dispatch all ops without session context
3. If SEQUENCE: validate session/slot/seqid via `handleSequenceOp()`, then dispatch remaining ops
4. If neither: return `NFS4ERR_OP_NOT_IN_SESSION`

**Exempt operations** (can appear as first op without SEQUENCE):
- `EXCHANGE_ID`, `CREATE_SESSION`, `DESTROY_SESSION`, `BIND_CONN_TO_SESSION`
- Checked via `isSessionExemptOp(opCode)` in `sequence_handler.go`

**Seqid bypass via SkipOwnerSeqid:**
After SEQUENCE validates successfully, `compCtx.SkipOwnerSeqid = true` is set.
This tells v4.0 ops called from v4.1 compounds (via fallback) to skip per-owner
seqid validation, since v4.1 uses slot-based exactly-once semantics instead.

**Replay cache at COMPOUND level:**
- SEQUENCE validation returns cached COMPOUND response bytes for replays (same slot+seqid)
- The full XDR-encoded response is stored in the slot via `CompleteSlotRequest()`
- On replay: `slot.CachedReply` bytes returned directly (byte-identical to original)
- `CacheThis` flag in SEQUENCE args controls whether response is cached

**Minor version range configuration:**
- `Handler.minMinorVersion` / `Handler.maxMinorVersion` (default 0, 1)
- Set via `SetMinorVersionRange(min, max)` or from `NFSAdapterSettings.V4MinMinorVersion`/`V4MaxMinorVersion`
- Checked before the minorversion switch in `ProcessCompound()`
- Out-of-range returns `NFS4ERR_MINOR_VERS_MISMATCH`

### Prometheus Metrics: SEQUENCE (sequence_metrics.go)

Follows same nil-safe receiver pattern as SessionMetrics:
- `dittofs_nfs_sequence_total` (counter): total SEQUENCE operations
- `dittofs_nfs_sequence_errors_total` (counter_vec, label: error_type): per-error counters
  - Labels: "bad_session", "seq_misordered", "replay_hit", "slot_busy", "bad_xdr", "bad_slot", "retry_uncached"
- `dittofs_nfs_replay_hits_total` (counter): successful replay cache hits
- `dittofs_nfs_slots_in_use` (gauge_vec, label: session_id): slots in use per session
- `dittofs_nfs_replay_cache_bytes` (gauge): total cached response bytes

All methods nil-safe: `RecordSequence()`, `RecordError(errType)`, `RecordReplayHit()`,
`SetSlotsInUse(sessionID, count)`, `SetReplayCacheBytes(bytes)`. Set via `Handler.SetSequenceMetrics()`.

### Common Mistakes (v4.1 Specific)

1. **XDR desync in v4.1 stubs** -- Stubs MUST decode args even when returning NOTSUPP. The COMPOUND reader position must advance past the current op's args.
2. **Wrong dispatch table** -- v4.1 ops go in v41DispatchTable, not opDispatchTable
3. **Missing fallback** -- v4.0 ops must be accessible from v4.1 compounds
4. **OP_ILLEGAL vs NFS4ERR_NOTSUPP** -- Unknown opcodes outside valid ranges get OP_ILLEGAL; known but unimplemented ops get NOTSUPP
5. **CREATE_SESSION seqid off-by-one** -- Client must send `record.SequenceID + 1` for a new CREATE_SESSION request. EXCHANGE_ID returns `record.SequenceID`; the client increments it before sending CREATE_SESSION.
6. **SEQUENCE replay vs re-execute** -- On replay hit, return `slot.CachedReply` directly. Do NOT re-execute ops. The cached bytes are the full COMPOUND response.

## Common Mistakes

1. **Business logic in handlers** - permissions, validation belong in service layer
2. **Parsing file handles** - they're opaque, just pass through
3. **Wrong log level** - DEBUG for expected errors (not found), ERROR for unexpected
4. **Not using buffer pools** - significant GC pressure under load
5. **Forgetting WCC data** - pre-operation attributes required for client cache coherency
