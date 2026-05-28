# Phase 38: Durable Handles - Research

**Researched:** 2026-03-02
**Domain:** SMB3 durable handles (V1 DHnQ/DHnC + V2 DH2Q/DH2C), state persistence, reconnect validation
**Confidence:** HIGH

## Summary

Durable handles enable SMB3 clients to survive brief network interruptions without losing open files. The server persists open-file state so that when a client reconnects (new TCP connection, same identity), the open is restored without data loss. There are two versions: V1 (DHnQ/DHnC, available since SMB 2.x) and V2 (DH2Q/DH2C, SMB 3.x only, with CreateGuid for idempotent reconnection). This phase also includes App Instance ID support for Hyper-V failover scenarios.

The DittoFS codebase has well-established patterns for this work. The `ClientRegistrationStore` sub-interface pattern (defined in `pkg/metadata/lock/`, implemented across memory/badger/postgres) provides the exact template for the new `DurableHandleStore`. The `lease_context.go` pattern provides the template for CREATE context processing. The `closeFilesWithFilter` function in `handler.go` is the integration point where durable handles must be preserved during disconnect cleanup.

**Primary recommendation:** Follow the `ClientRegistrationStore` pattern exactly for `DurableHandleStore`, and the `lease_context.go` pattern for CREATE context processing. The scavenger goroutine should be managed by the SMB adapter lifecycle, started in `Serve()` and stopped on context cancellation.

<user_constraints>
## User Constraints (from CONTEXT.md)

### Locked Decisions
- Default durable handle timeout: 60 seconds after client disconnect
- Timeout configurable at adapter level (`adapters.smb.durable_handle_timeout`)
- Scavenger interval configurable at adapter level (default 10s)
- Durability only granted when client explicitly sends DHnQ (V1) or DH2Q (V2) create context
- No implicit durability from H-lease alone (defer to later)
- On handle expiry: full close (release byte-range locks, flush caches, execute delete-on-close)
- Leases/oplocks preserved during timeout period (not released on disconnect)
- When conflicting open arrives for orphaned durable handle: trigger lease break, force-expire the orphaned handle after short grace period, allow new open
- Abstract DurableHandleStore interface in `pkg/metadata/` (follows ClientRegistrationStore pattern)
- SMB-specific scavenger goroutine in `internal/adapter/smb/` for lifecycle management
- Full MS-SMB2 compliance: implement all 14+ reconnect validation checks
- Security context validation: username match + session key hash comparison (hash stored, not raw key)
- Return specific per-check NTSTATUS error codes
- Debug-level structured logging for each validation step
- Client must SESSION_SETUP + TREE_CONNECT to same share before reconnect attempt
- Restore original oplock/lease state on successful reconnect (no re-negotiation)
- Support cross-connection reconnect (new TCP connection, same client identity)
- DesiredAccess and ShareAccess must match original CREATE values (prevent privilege escalation)
- Durable handle established after full compound request completes (not mid-compound)
- V2 reconnect is idempotent: same CreateGuid returns success on duplicate reconnect
- File-still-exists check: verify metadata handle valid + path unchanged. Don't check size/timestamps
- Implement both V1 (DHnQ/DHnC) and V2 (DH2Q/DH2C)
- V1 requires batch oplock to grant durability (per MS-SMB2 3.3.5.9.6)
- V2: accept client's CreateGuid (16-byte client-generated value)
- V2: accept client's requested timeout but cap at server's configured maximum; return granted timeout in response
- No persistent handles (SMB2_DHANDLE_FLAG_PERSISTENT) - defer to future HA work
- When both DHnQ and DH2Q present in same CREATE: V2 takes precedence, ignore DHnQ
- Include SMB2_CREATE_APP_INSTANCE_ID context support
- App Instance ID collision: force-close the old handle (Hyper-V failover pattern)
- DurableHandleStore as sub-interface of MetadataStore (embedded, like LockStore and ClientRegistrationStore)
- Implementations in all three backends: memory, badger, postgres
- Memory implementation: handles survive disconnect but not restart (for testing/ephemeral)
- Badger/Postgres: handles survive server restart
- Persist full OpenFile state: FileID, Path, ShareName, DesiredAccess, ShareAccess, MetadataHandle, PayloadID, OplockLevel, CreateGuid, AppInstanceId, CreateOptions, timestamps, username, session key hash
- Multi-key lookup support: by primary ID, by CreateGuid (V2 reconnect), by AppInstanceId (failover), by FileHandle (conflict check)
- On startup: load persisted handles, adjust remaining timeout based on server downtime
- Create `durable_context.go` in `internal/adapter/smb/v2/handlers/` following `lease_context.go` pattern
- ProcessDurableHandleContext / ProcessDurableReconnectContext functions called from CREATE handler at Step 8b
- NFS opens do NOT affect durability granting (cross-protocol is Phase 39 scope)
- Modify `closeFilesWithFilter` in handler.go to skip files with durable handles; persist their state to DurableHandleStore before cleanup proceeds
- Conformance test suite for DurableHandleStore (like `storetest/` pattern) + handler-level tests
- Full CRUD REST API endpoints
- GET /api/v1/durable-handles - list active durable handles
- DELETE /api/v1/durable-handles/:id - force-close with full cleanup
- Update ARCHITECTURE.md with durable handle state flow section

### Claude's Discretion
- Exact DurableHandleStore interface method signatures
- Badger key encoding scheme for durable handle records
- Postgres migration schema
- Scavenger goroutine internal structure
- REST API response format for durable handle listing
- Exact wire format parsing details for DHnQ/DH2Q/DHnC/DH2C structures
- Integration test setup and fixture design

### Deferred Ideas (OUT OF SCOPE)
- H-lease-implies-durability: implicit durability when client holds Handle lease (add after Phase 37 is stable)
- Persistent handles (SMB2_DHANDLE_FLAG_PERSISTENT): requires HA/cluster support, defer to future milestone
- Prometheus metrics for durable handles: can be added in a future enhancement
- Cross-protocol durability conflict checking: belongs in Phase 39 (Cross-Protocol Integration)
</user_constraints>

<phase_requirements>
## Phase Requirements

| ID | Description | Research Support |
|----|-------------|-----------------|
| DH-01 | Server grants durable handles V1 (DHnQ) and reconnects via DHnC with timeout | Wire format specs for DHnQ (16 bytes reserved), DHnC (16 bytes FileId), granting conditions (batch oplock required), reconnect validation (14+ checks from MS-SMB2 3.3.5.9.7), and timeout management via scavenger goroutine |
| DH-02 | Server grants durable handles V2 (DH2Q) with CreateGuid for idempotent reconnection | Wire format specs for DH2Q (32 bytes: Timeout+Flags+Reserved+CreateGuid), DH2C (36 bytes: FileId+CreateGuid+Flags), V2 granting conditions (no batch oplock required, CreateGuid tracking), idempotent reconnect via CreateGuid lookup |
| DH-03 | Durable handle state persists in control plane store surviving disconnects | DurableHandleStore interface pattern following ClientRegistrationStore, implementations for memory/badger/postgres, persisted state struct with all required fields, multi-key lookup indices |
| DH-04 | Server validates all reconnect conditions (14+ checks per MS-SMB2 spec) | Complete enumeration of V1 reconnect checks (section 3.3.5.9.7) and V2 reconnect checks (section 3.3.5.9.12) with specific NTSTATUS codes per check |
| DH-05 | Durable handle management logic lives in metadata service layer, reusing NFSv4 state patterns | Architecture places DurableHandleStore in `pkg/metadata/lock/` alongside LockStore/ClientRegistrationStore, handler-level processing in `internal/adapter/smb/v2/handlers/durable_context.go` |
</phase_requirements>

## Standard Stack

### Core (existing codebase libraries -- no new dependencies needed)

| Library | Version | Purpose | Why Standard |
|---------|---------|---------|--------------|
| Go standard library | 1.22+ | `context`, `sync`, `time`, `crypto/sha256` | Scavenger goroutine, mutex, timeout, session key hashing |
| `github.com/google/uuid` | Already in go.mod | UUID generation for durable handle IDs | Consistent with existing FileHandle generation |
| `github.com/dgraph-io/badger/v4` | Already in go.mod | BadgerDB persistence for durable handle state | Same as existing lock/client stores |
| `github.com/jackc/pgx/v5` | Already in go.mod | PostgreSQL persistence for durable handle state | Same as existing lock/client stores |
| `github.com/golang-migrate/migrate/v4` | Already in go.mod | PostgreSQL schema migration | Same migration pattern as existing tables |
| `github.com/go-chi/chi/v5` | Already in go.mod | REST API routing for durable handle endpoints | Same router used by all existing API endpoints |
| `smbenc` (internal) | N/A | Wire format encoding/decoding for create contexts | Internal SMB binary codec already used for lease contexts |

### Supporting
No new dependencies required. All functionality builds on existing codebase patterns.

### Alternatives Considered
None -- the codebase patterns are well-established and the user decisions mandate following them.

## Architecture Patterns

### Recommended Project Structure

```
pkg/metadata/lock/
    durable_store.go              # DurableHandleStore interface + PersistedDurableHandle type

pkg/metadata/store/memory/
    durable_handles.go            # Memory DurableHandleStore implementation

pkg/metadata/store/badger/
    durable_handles.go            # BadgerDB DurableHandleStore implementation

pkg/metadata/store/postgres/
    durable_handles.go            # PostgreSQL DurableHandleStore implementation
    migrations/000005_durable_handles.up.sql
    migrations/000005_durable_handles.down.sql

pkg/metadata/storetest/
    durable_handles.go            # Conformance test suite for DurableHandleStore

internal/adapter/smb/v2/handlers/
    durable_context.go            # CREATE context processing (parse, grant, reconnect)
    durable_context_test.go       # Handler-level tests
    durable_scavenger.go          # Scavenger goroutine for handle expiry

internal/controlplane/api/handlers/
    durable_handle.go             # REST API handler for list/force-close

pkg/controlplane/api/
    router.go                     # Add durable handle routes (modify existing)
```

### Pattern 1: DurableHandleStore Sub-Interface (follows ClientRegistrationStore)

**What:** Define `DurableHandleStore` as a sub-interface in `pkg/metadata/lock/`, implement in all three backends.

**When to use:** All durable handle persistence operations.

**Key design:** The interface needs multi-key lookup support (by primary ID, by CreateGuid, by AppInstanceId, by FileHandle). This is more complex than ClientRegistrationStore which only has primary key + MonName index.

```go
// Source: pattern from pkg/metadata/lock/client_store.go
package lock

// PersistedDurableHandle is the storage representation of a durable open.
type PersistedDurableHandle struct {
    ID               string    // Primary key (UUID)
    FileID           [16]byte  // SMB2 FileID for reconnect matching
    Path             string    // File path within share
    ShareName        string    // Share name
    DesiredAccess    uint32    // Original CREATE DesiredAccess
    ShareAccess      uint32    // Original CREATE ShareAccess
    CreateOptions    uint32    // Original CREATE CreateOptions
    MetadataHandle   []byte    // Metadata store file handle
    PayloadID        string    // Content identifier
    OplockLevel      uint8     // Granted oplock level at disconnect
    LeaseKey         [16]byte  // Lease key if lease-based (zero if oplock)
    LeaseState       uint32    // Lease state at disconnect
    CreateGuid       [16]byte  // V2 only: client-generated GUID
    AppInstanceId    [16]byte  // App Instance ID (zero if not set)
    Username         string    // Authenticated username
    SessionKeyHash   [32]byte  // SHA-256 of session key
    IsV2             bool      // True for V2 durable handles
    CreatedAt        time.Time // When the open was originally created
    DisconnectedAt   time.Time // When the client disconnected
    TimeoutMs        uint32    // Granted timeout in milliseconds
    ServerStartTime  time.Time // Server start time (for restart timeout adjustment)
}

type DurableHandleStore interface {
    PutDurableHandle(ctx context.Context, handle *PersistedDurableHandle) error
    GetDurableHandle(ctx context.Context, id string) (*PersistedDurableHandle, error)
    GetDurableHandleByFileID(ctx context.Context, fileID [16]byte) (*PersistedDurableHandle, error)
    GetDurableHandleByCreateGuid(ctx context.Context, createGuid [16]byte) (*PersistedDurableHandle, error)
    GetDurableHandlesByAppInstanceId(ctx context.Context, appInstanceId [16]byte) ([]*PersistedDurableHandle, error)
    GetDurableHandlesByFileHandle(ctx context.Context, fileHandle []byte) ([]*PersistedDurableHandle, error)
    DeleteDurableHandle(ctx context.Context, id string) error
    ListDurableHandles(ctx context.Context) ([]*PersistedDurableHandle, error)
    ListDurableHandlesByShare(ctx context.Context, shareName string) ([]*PersistedDurableHandle, error)
    DeleteExpiredDurableHandles(ctx context.Context, now time.Time) (int, error)
}
```

### Pattern 2: CREATE Context Processing (follows lease_context.go)

**What:** Parse DHnQ/DH2Q/DHnC/DH2C create contexts from CREATE request, process durability grant or reconnect.

**When to use:** Called from CREATE handler at Step 8b, after lease processing.

```go
// Source: pattern from internal/adapter/smb/v2/handlers/lease_context.go
package handlers

const (
    DurableHandleV1RequestTag  = "DHnQ" // SMB2_CREATE_DURABLE_HANDLE_REQUEST
    DurableHandleV1ResponseTag = "DHnC" // Reconnect tag is same as response
    DurableHandleV1ReconnectTag = "DHnC" // SMB2_CREATE_DURABLE_HANDLE_RECONNECT
    DurableHandleV2RequestTag  = "DH2Q" // SMB2_CREATE_DURABLE_HANDLE_REQUEST_V2
    DurableHandleV2ResponseTag = "DH2C" // Not actually used as response tag
    DurableHandleV2ReconnectTag = "DH2C" // SMB2_CREATE_DURABLE_HANDLE_RECONNECT_V2
    AppInstanceIdTag           = "4571" // SMB2_CREATE_APP_INSTANCE_ID (binary tag)
)

// ProcessDurableHandleContext processes DHnQ or DH2Q create contexts.
// Returns a response context to include in CREATE response, or nil if not granting.
func ProcessDurableHandleContext(
    durableStore DurableHandleStore,
    contexts []CreateContext,
    openFile *OpenFile,
    sessionID uint64,
    username string,
    sessionKeyHash [32]byte,
    configuredTimeoutMs uint32,
) (*CreateContext, error) {
    // 1. Check for DH2Q (V2 takes precedence over V1)
    // 2. If DH2Q: parse CreateGuid, Timeout, Flags
    //    - Check no persistent flag (not supported)
    //    - Check oplock/lease grants durability
    //    - Store CreateGuid in OpenFile
    //    - Calculate granted timeout (min of requested, configured max)
    //    - Return DH2Q response context with granted timeout
    // 3. If DHnQ only: parse (16 bytes reserved, ignore)
    //    - V1 requires batch oplock or Handle lease to grant durability
    //    - Return DHnQ response context
    // 4. Return nil if neither present
}

// ProcessDurableReconnectContext processes DHnC or DH2C create contexts.
// Returns the restored OpenFile on success, or an NTSTATUS error.
func ProcessDurableReconnectContext(
    durableStore DurableHandleStore,
    contexts []CreateContext,
    sessionID uint64,
    username string,
    sessionKeyHash [32]byte,
    shareName string,
    filename string,
) (*OpenFile, uint32 /* status */, error) {
    // 1. Determine V1 (DHnC) or V2 (DH2C)
    // 2. Parse the reconnect context (FileId for V1, FileId+CreateGuid+Flags for V2)
    // 3. Look up persisted handle (by FileId for V1, by CreateGuid for V2)
    // 4. Run all validation checks (14+ for V1, similar for V2)
    // 5. On success: restore OpenFile, delete persisted handle, return
    // 6. On failure: return specific NTSTATUS code
}
```

### Pattern 3: Scavenger Goroutine (follows OplockManager lifecycle)

**What:** Background goroutine that periodically checks for expired durable handles and performs full close (release locks, flush caches, delete-on-close).

**When to use:** Started by SMB adapter during `Serve()`, stopped on context cancellation.

```go
// Source: lifecycle pattern from SMB adapter
package handlers

type DurableHandleScavenger struct {
    store      DurableHandleStore
    handler    *Handler
    interval   time.Duration
    timeoutMs  uint32
}

func (s *DurableHandleScavenger) Run(ctx context.Context) {
    ticker := time.NewTicker(s.interval)
    defer ticker.Stop()

    for {
        select {
        case <-ctx.Done():
            return
        case <-ticker.C:
            s.expireHandles(ctx)
        }
    }
}

func (s *DurableHandleScavenger) expireHandles(ctx context.Context) {
    // 1. Call store.DeleteExpiredDurableHandles(ctx, time.Now())
    // 2. For each expired handle: release locks, flush caches, delete-on-close
    // 3. Log each expiry at debug level
}
```

### Pattern 4: closeFilesWithFilter Integration

**What:** Modify the existing `closeFilesWithFilter` to detect durable handles and persist their state instead of closing them.

**When to use:** During session/connection cleanup (LOGOFF, disconnect).

```go
// Modification to handler.go closeFilesWithFilter:
// Before closing a file, check if it has durable state:
//   - If openFile has IsDurable flag set:
//     1. Persist state to DurableHandleStore
//     2. Skip lock release, cache flush, delete-on-close
//     3. Remove from in-memory files map (but state lives in store)
//   - If not durable: proceed with normal close
```

### Pattern 5: Badger Key Encoding

**What:** Multi-key storage scheme for efficient lookups by different keys.

```
// Primary key:      dh:id:{uuid}          -> JSON(PersistedDurableHandle)
// CreateGuid index: dh:cguid:{hex}        -> id (string)
// AppInstanceId:    dh:appid:{hex}        -> id (string)
// FileID index:     dh:fid:{hex}          -> id (string)
// FileHandle index: dh:fh:{hex}           -> id (string)
// Share index:      dh:share:{name}:{id}  -> id (string)
```

### Pattern 6: PostgreSQL Migration Schema

```sql
CREATE TABLE durable_handles (
    id               TEXT PRIMARY KEY,
    file_id          BYTEA NOT NULL,
    path             TEXT NOT NULL,
    share_name       TEXT NOT NULL,
    desired_access   INTEGER NOT NULL,
    share_access     INTEGER NOT NULL,
    create_options   INTEGER NOT NULL,
    metadata_handle  BYTEA NOT NULL,
    payload_id       TEXT,
    oplock_level     SMALLINT NOT NULL DEFAULT 0,
    lease_key        BYTEA,
    lease_state      INTEGER NOT NULL DEFAULT 0,
    create_guid      BYTEA,
    app_instance_id  BYTEA,
    username         TEXT NOT NULL,
    session_key_hash BYTEA NOT NULL,
    is_v2            BOOLEAN NOT NULL DEFAULT FALSE,
    created_at       TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    disconnected_at  TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    timeout_ms       INTEGER NOT NULL DEFAULT 60000,
    server_start_time TIMESTAMPTZ NOT NULL,

    CONSTRAINT valid_file_id CHECK (length(file_id) = 16),
    CONSTRAINT valid_session_key_hash CHECK (length(session_key_hash) = 32)
);

CREATE INDEX idx_durable_handles_create_guid ON durable_handles(create_guid) WHERE create_guid IS NOT NULL;
CREATE INDEX idx_durable_handles_app_instance_id ON durable_handles(app_instance_id) WHERE app_instance_id IS NOT NULL;
CREATE INDEX idx_durable_handles_file_id ON durable_handles(file_id);
CREATE INDEX idx_durable_handles_share_name ON durable_handles(share_name);
CREATE INDEX idx_durable_handles_metadata_handle ON durable_handles(metadata_handle);
CREATE INDEX idx_durable_handles_disconnected_at ON durable_handles(disconnected_at);
```

### Anti-Patterns to Avoid

- **Storing raw session key:** The CONTEXT.md explicitly requires storing SHA-256 hash of session key, not the raw key. This prevents session key exposure if the store is compromised.
- **Releasing leases on disconnect:** Leases/oplocks must be preserved during the timeout period. Only release them when the durable handle actually expires.
- **Granting durability without checking oplock/lease:** V1 requires batch oplock or Handle lease. V2 is more lenient but still requires the underlying object store to support durability.
- **Allowing privilege escalation on reconnect:** DesiredAccess and ShareAccess must match the original CREATE values exactly. Do not allow a client to upgrade access on reconnect.
- **Closing durable handles during compound request:** Durable state should only be established after the full compound request completes, not mid-compound.

## Don't Hand-Roll

| Problem | Don't Build | Use Instead | Why |
|---------|-------------|-------------|-----|
| UUID generation | Custom ID scheme | `github.com/google/uuid` | Already used throughout codebase |
| Session key hashing | Custom hash | `crypto/sha256` from stdlib | Standard, well-tested, matches decision |
| Wire format parsing | Manual byte slicing | `smbenc.NewReader`/`smbenc.NewWriter` | Consistent with all other SMB create context parsing |
| Timer/ticker management | Manual goroutine scheduling | `time.NewTicker` + select | Standard Go pattern for periodic background work |
| JSON serialization for BadgerDB | Custom encoding | `encoding/json` | Same as existing locks and client stores in BadgerDB |
| Database migrations | Manual DDL | `golang-migrate` | Same migration framework used by all postgres store migrations |

**Key insight:** Every building block for durable handles already exists in the codebase. The work is connecting them together following established patterns, not inventing new infrastructure.

## Common Pitfalls

### Pitfall 1: Race Between Reconnect and Scavenger Expiry
**What goes wrong:** Client reconnects at the exact moment the scavenger is expiring the handle. The reconnect reads the handle, but the scavenger deletes it concurrently.
**Why it happens:** The scavenger and reconnect handler access the same store concurrently.
**How to avoid:** Use `DeleteExpiredDurableHandles` with a WHERE clause that checks `disconnected_at + timeout < now`. The reconnect handler first does a Get (which succeeds), then atomically marks the handle as reconnected (or deletes it). In BadgerDB, use a transaction. In PostgreSQL, use `DELETE ... RETURNING` or a transaction. In memory, use the store's RWMutex.
**Warning signs:** Intermittent "handle not found" errors during reconnect under load.

### Pitfall 2: Forgetting to Restore Oplock/Lease State
**What goes wrong:** Client reconnects successfully but loses its oplock/lease, causing unnecessary cache invalidation and performance degradation.
**Why it happens:** The reconnect code creates a new OpenFile without restoring the original oplock level and lease state from the persisted handle.
**How to avoid:** On successful reconnect, set `openFile.OplockLevel` from the persisted handle's `OplockLevel`. If the handle had a lease, verify the lease still exists in the LockStore and restore it.
**Warning signs:** Client revalidates entire cache after reconnect; excessive READ operations.

### Pitfall 3: V1/V2 Precedence Confusion
**What goes wrong:** Both DHnQ and DH2Q are present in the same CREATE request. The server processes DHnQ first and grants V1 durability, ignoring the V2 context.
**Why it happens:** Processing contexts in order of appearance rather than checking for V2 first.
**How to avoid:** Always check for DH2Q first. If present, process V2 and ignore DHnQ. Only process DHnQ if DH2Q is absent. This matches MS-SMB2 precedence rules.
**Warning signs:** smbtorture durable_v2 tests fail because server returns V1 response when V2 was requested.

### Pitfall 4: Not Checking File Still Exists on Reconnect
**What goes wrong:** Client reconnects to a durable handle for a file that was deleted by another client or process during the timeout period.
**Why it happens:** The reconnect code only checks handle state in the DurableHandleStore, not whether the underlying metadata handle is still valid.
**How to avoid:** After looking up the persisted handle, verify the metadata handle is still valid by calling `metadataService.GetFile()`. Per the decision, only check handle validity and path -- not size/timestamps.
**Warning signs:** Client gets stale data or crashes after reconnecting to a deleted file.

### Pitfall 5: Session Key Hash Mismatch on Server Restart
**What goes wrong:** After server restart, all reconnects fail with STATUS_ACCESS_DENIED because session key hashes don't match.
**Why it happens:** Session keys are derived from authentication. After restart, clients re-authenticate and get new session keys, but the stored hash is from the old session key.
**How to avoid:** This is expected behavior for V1 durable handles (they can only survive disconnect, not server restart, because V1 requires batch oplock which is ephemeral). For V2, the CreateGuid-based lookup is independent of session key, but security validation still compares username. Consider whether session key hash comparison should be relaxed for post-restart reconnects (the spec says DurableOwner is a security descriptor, not a session key).
**Warning signs:** All reconnects fail after server restart even though handles are persisted.

### Pitfall 6: App Instance ID Force-Close Without Full Cleanup
**What goes wrong:** When an App Instance ID collision triggers force-close of the old handle, locks and caches are not properly released.
**Why it happens:** The force-close path bypasses the normal close flow which handles lock release and cache flush.
**How to avoid:** Reuse the same cleanup logic as `closeFilesWithFilter` for force-close operations. Build a helper function that performs full cleanup (release locks, flush caches, delete-on-close) that both the normal close path and the force-close path call.
**Warning signs:** Orphaned locks after Hyper-V failover; data inconsistency.

## Code Examples

### Wire Format: DHnQ (V1 Request) - 16 bytes

```go
// Source: MS-SMB2 2.2.13.2.3
// Create context tag: "DHnQ"
// Data: 16 bytes of reserved (all zeros, ignored by server)
func DecodeDHnQRequest(data []byte) error {
    if len(data) < 16 {
        return fmt.Errorf("DHnQ request too short: %d bytes", len(data))
    }
    // DurableRequest (16 bytes): MUST be zero, server ignores
    return nil
}
```

### Wire Format: DHnC (V1 Reconnect) - 16 bytes

```go
// Source: MS-SMB2 2.2.13.2.4
// Create context tag: "DHnC"
// Data: 16 bytes containing the FileId from the original CREATE response
func DecodeDHnCReconnect(data []byte) ([16]byte, error) {
    if len(data) < 16 {
        return [16]byte{}, fmt.Errorf("DHnC reconnect too short: %d bytes", len(data))
    }
    var fileID [16]byte
    copy(fileID[:], data[:16])
    return fileID, nil
}
```

### Wire Format: DH2Q (V2 Request) - 32 bytes

```go
// Source: MS-SMB2 2.2.13.2.11
// Create context tag: "DH2Q"
// Data layout:
//   Offset 0:  Timeout (4 bytes) - milliseconds, 0 = use server default
//   Offset 4:  Flags (4 bytes) - 0x02 = persistent (we reject this)
//   Offset 8:  Reserved (8 bytes) - must be zero
//   Offset 16: CreateGuid (16 bytes) - client-generated GUID
func DecodeDH2QRequest(data []byte) (timeout uint32, flags uint32, createGuid [16]byte, err error) {
    if len(data) < 32 {
        return 0, 0, [16]byte{}, fmt.Errorf("DH2Q request too short: %d bytes", len(data))
    }
    r := smbenc.NewReader(data)
    timeout = r.ReadUint32()
    flags = r.ReadUint32()
    r.Skip(8) // Reserved
    copy(createGuid[:], data[16:32])
    return timeout, flags, createGuid, r.Err()
}
```

### Wire Format: DH2C (V2 Reconnect) - 36 bytes

```go
// Source: MS-SMB2 2.2.13.2.12
// Create context tag: "DH2C"
// Data layout:
//   Offset 0:  FileId (16 bytes) - SMB2_FILEID for the open being reestablished
//   Offset 16: CreateGuid (16 bytes) - must match the original DH2Q CreateGuid
//   Offset 32: Flags (4 bytes) - 0x02 = persistent (we reject this)
func DecodeDH2CReconnect(data []byte) (fileID [16]byte, createGuid [16]byte, flags uint32, err error) {
    if len(data) < 36 {
        return [16]byte{}, [16]byte{}, 0, fmt.Errorf("DH2C reconnect too short: %d bytes", len(data))
    }
    copy(fileID[:], data[:16])
    copy(createGuid[:], data[16:32])
    r := smbenc.NewReader(data[32:])
    flags = r.ReadUint32()
    return fileID, createGuid, flags, r.Err()
}
```

### Wire Format: App Instance ID - 20 bytes

```go
// Source: MS-SMB2 2.2.13.2.13
// Create context tag: binary "4571 5349 6E73 7461 6E63 6500" or "E\x04"
// Actually the tag is the 4-byte ASCII string representation
// Data layout:
//   Offset 0:  StructureSize (2 bytes) - must be 20
//   Offset 2:  Reserved (2 bytes) - must be zero
//   Offset 4:  AppInstanceId (16 bytes) - unique application instance ID
func DecodeAppInstanceId(data []byte) ([16]byte, error) {
    if len(data) < 20 {
        return [16]byte{}, fmt.Errorf("AppInstanceId too short: %d bytes", len(data))
    }
    r := smbenc.NewReader(data)
    structSize := r.ReadUint16()
    if structSize != 20 {
        return [16]byte{}, fmt.Errorf("AppInstanceId invalid structure size: %d", structSize)
    }
    r.Skip(2) // Reserved
    var appId [16]byte
    copy(appId[:], data[4:20])
    return appId, r.Err()
}
```

### V1 Reconnect Validation (14+ checks from MS-SMB2 3.3.5.9.7)

```go
// Source: MS-SMB2 section 3.3.5.9.7
// Each check returns a specific NTSTATUS code on failure.
func validateV1Reconnect(
    handle *PersistedDurableHandle,
    contexts []CreateContext,
    username string,
    sessionKeyHash [32]byte,
    shareName string,
    filename string,
    clientGuid [16]byte, // from Connection.ClientGuid
) (uint32 /* NTSTATUS */, error) {
    // Check 1: DHnQ also present -> ignore DHnQ (not an error)
    // Check 2: DH2Q or DH2C also present -> STATUS_INVALID_PARAMETER
    // Check 3: Handle exists in store -> STATUS_OBJECT_NAME_NOT_FOUND
    // Check 4: If handle has lease, ClientGuid must match -> STATUS_OBJECT_NAME_NOT_FOUND
    // Check 5: If handle has lease, filename must match -> STATUS_INVALID_PARAMETER
    // Check 6a: If handle has lease but no lease context in request -> STATUS_OBJECT_NAME_NOT_FOUND
    // Check 6b: If handle has no lease but lease context present -> STATUS_OBJECT_NAME_NOT_FOUND
    // Check 6c: If IsDurable, no lease, oplock not Batch -> STATUS_OBJECT_NAME_NOT_FOUND
    // Check 6d: If IsDurable, lease without Handle caching -> STATUS_OBJECT_NAME_NOT_FOUND
    // Check 6e: If not durable and not resilient -> STATUS_OBJECT_NAME_NOT_FOUND
    // Check 6f: If Open.Session is not NULL (still connected) -> STATUS_OBJECT_NAME_NOT_FOUND
    // Check 6g: Lease key mismatch (V2 lease context) -> STATUS_OBJECT_NAME_NOT_FOUND
    // Check 6h: Lease key mismatch (V1 lease context) -> STATUS_OBJECT_NAME_NOT_FOUND
    // Check 7: Lease version mismatch -> STATUS_OBJECT_NAME_NOT_FOUND
    // Check 8: Security context mismatch -> STATUS_ACCESS_DENIED
    // Additional: File still exists in metadata store
    // Additional: DesiredAccess/ShareAccess match (prevent privilege escalation)

    return StatusSuccess, nil
}
```

### Response Context Encoding

```go
// DHnQ Response (DHnC tag, empty 8-byte body per MS-SMB2 2.2.14.2.3)
func EncodeDHnQResponse() CreateContext {
    return CreateContext{
        Name: "DHnC", // Response tag for V1
        Data: make([]byte, 8), // Reserved, all zeros
    }
}

// DH2Q Response (DH2C tag per MS-SMB2 2.2.14.2.12)
// Fields: Timeout (4 bytes) + Flags (4 bytes) = 8 bytes
func EncodeDH2QResponse(timeoutMs uint32, flags uint32) CreateContext {
    w := smbenc.NewWriter(8)
    w.WriteUint32(timeoutMs)
    w.WriteUint32(flags)
    return CreateContext{
        Name: "DH2Q", // Response tag for V2 (server sends back same tag)
        Data: w.Bytes(),
    }
}
```

## State of the Art

| Old Approach | Current Approach | When Changed | Impact |
|--------------|------------------|--------------|--------|
| V1 only (DHnQ/DHnC) | V1 + V2 (DH2Q/DH2C with CreateGuid) | SMB 3.0 (2012) | V2 enables idempotent reconnect via CreateGuid |
| Oplock-only durability (V1) | Lease-based + oplock durability (V2) | SMB 3.0 (2012) | V2 does not require batch oplock; lease Handle caching suffices |
| No app instance tracking | SMB2_CREATE_APP_INSTANCE_ID | SMB 3.0 (2012) | Enables Hyper-V failover: new VM instance force-closes old handles |
| Session-key-based owner check | Security descriptor DurableOwner | SMB 3.0+ spec | More flexible than raw key comparison |

**Note on response context tags:** The MS-SMB2 spec uses these create context name strings:
- Request DHnQ -> Response tag is "DHnC" (per 2.2.14.2.3)
- Request DH2Q -> Response tag is "DH2Q" (per 2.2.14.2.12, server echoes same tag)
- Reconnect DHnC -> Uses same tag as the response context name
- Reconnect DH2C -> Response uses "DH2Q" tag (per 2.2.14.2.12)

## Open Questions

1. **Response context tag for DH2Q/DH2C**
   - What we know: MS-SMB2 2.2.14.2.12 defines SMB2_CREATE_DURABLE_HANDLE_RESPONSE_V2 but the actual tag name used on the wire needs verification. Some implementations use "DH2Q" as both request and response tag.
   - What's unclear: Whether the response tag is literally "DH2Q" or uses a different 4-byte name.
   - Recommendation: Check smbtorture captures or Wireshark traces. Start with "DH2Q" as response tag; if tests fail, try alternative tags. LOW confidence on exact response tag encoding.

2. **Session key hash comparison semantics after restart**
   - What we know: Session keys are ephemeral and change on re-authentication. Stored hash will not match new session key after server restart.
   - What's unclear: Whether the spec intends DurableOwner comparison to be username-only or also includes session key verification for post-restart reconnects.
   - Recommendation: Per the user decision, store SHA-256 of session key and compare. For post-restart reconnects with V2 handles (which use CreateGuid), validate username match but log a warning if session key hash differs (do not reject). For V1 handles after restart, they naturally expire because the oplock state is ephemeral.

3. **Compound request boundary for durability establishment**
   - What we know: The decision says "durable handle established after full compound request completes."
   - What's unclear: DittoFS does not currently implement compound request (multiple SMB commands in a single transport message) -- each command is dispatched individually.
   - Recommendation: For this phase, treat each CREATE as a standalone request and establish durability immediately after the CREATE response is built. Add a TODO comment for compound request support when it is implemented.

## Sources

### Primary (HIGH confidence)
- [MS-SMB2 2.2.13.2.3: SMB2_CREATE_DURABLE_HANDLE_REQUEST (DHnQ)](https://learn.microsoft.com/en-us/openspecs/windows_protocols/ms-smb2/9999d870-b664-4e51-a187-1c3c16a1ae1c) - V1 wire format (16 bytes reserved)
- [MS-SMB2 2.2.13.2.11: SMB2_CREATE_DURABLE_HANDLE_REQUEST_V2 (DH2Q)](https://learn.microsoft.com/en-us/openspecs/windows_protocols/ms-smb2/5e361a29-81a7-4774-861d-f290ea53a00e) - V2 wire format (32 bytes: Timeout+Flags+Reserved+CreateGuid)
- [MS-SMB2 2.2.13.2.12: SMB2_CREATE_DURABLE_HANDLE_RECONNECT_V2 (DH2C)](https://learn.microsoft.com/en-us/openspecs/windows_protocols/ms-smb2/a6d418a7-d2db-47c9-a1c7-5802222ad678) - V2 reconnect wire format (36 bytes: FileId+CreateGuid+Flags)
- [MS-SMB2 3.3.5.9.7: Handling DHnC Reconnect](https://learn.microsoft.com/en-us/openspecs/windows_protocols/ms-smb2/ba7499c3-4679-4d07-a82a-c80d9c2c6905) - Complete V1 reconnect validation conditions (14+ checks)
- [MS-SMB2 3.3.5.9.6: Handling DHnQ Request](https://learn.microsoft.com/en-us/openspecs/windows_protocols/ms-smb2/9adbc354-5fad-40e7-9a62-4a4b6c1ff8a0) - V1 granting conditions (batch oplock required)
- [MS-SMB2 3.3.5.9.10: Handling DH2Q Request](https://learn.microsoft.com/en-us/openspecs/windows_protocols/ms-smb2/33e6800a-adf5-4221-af27-7e089b9e81d1) - V2 granting conditions and CreateGuid handling
- [MS-SMB2 2.2.13.2.13: SMB2_CREATE_APP_INSTANCE_ID](https://learn.microsoft.com/en-us/openspecs/windows_protocols/ms-smb2/0c14e784-5529-4b2f-8d91-a84d32dec7b3) - App Instance ID wire format (20 bytes)
- [MS-SMB2 3.3.5.9: Receiving an SMB2 CREATE Request](https://learn.microsoft.com/en-us/openspecs/windows_protocols/ms-smb2/8c61e928-9242-44ed-96a0-98d1032d0d39) - Full CREATE processing with replay/reconnect handling

### Codebase (HIGH confidence)
- `internal/adapter/smb/v2/handlers/lease_context.go` - CREATE context processing pattern
- `pkg/metadata/lock/client_store.go` - Sub-interface pattern for persistent protocol state
- `pkg/metadata/store/memory/clients.go` - Memory implementation pattern
- `pkg/metadata/store/badger/clients.go` - BadgerDB implementation pattern with key prefixes and JSON encoding
- `pkg/metadata/store/postgres/clients.go` - PostgreSQL implementation pattern with pgx
- `pkg/metadata/store/postgres/migrations/000003_clients.up.sql` - Migration schema pattern
- `pkg/metadata/storetest/suite.go` - Conformance test suite pattern
- `internal/adapter/smb/v2/handlers/handler.go` - OpenFile struct, closeFilesWithFilter, CleanupSession
- `internal/adapter/smb/v2/handlers/create.go` - CREATE handler with context processing at Step 8b
- `internal/adapter/smb/v2/handlers/oplock.go` - OplockManager lifecycle pattern
- `pkg/adapter/smb/adapter.go` - SMB adapter lifecycle (SetRuntime, Serve, SetKerberosProvider)
- `pkg/controlplane/api/router.go` - REST API routing pattern with chi
- `pkg/metadata/store/badger/encoding.go` - BadgerDB key namespace and encoding patterns

## Metadata

**Confidence breakdown:**
- Standard stack: HIGH - All libraries already in go.mod, no new dependencies
- Architecture: HIGH - Patterns directly observed in codebase (ClientRegistrationStore, lease_context.go, storetest)
- Wire formats: HIGH - Verified against official MS-SMB2 specification documents
- Reconnect validation: HIGH - Enumerated from MS-SMB2 3.3.5.9.7 with specific NTSTATUS codes
- Pitfalls: MEDIUM - Based on protocol analysis and concurrent systems experience; some may not apply in practice
- Response context tags: LOW - Exact wire-level tag names need runtime verification

**Research date:** 2026-03-02
**Valid until:** 2026-04-02 (stable protocol spec, stable codebase patterns)
