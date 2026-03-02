# Phase 38: Durable Handles - Context

**Gathered:** 2026-03-02
**Status:** Ready for planning

<domain>
## Phase Boundary

Implement SMB3 durable handles (V1 and V2) so clients survive brief network interruptions without losing open files. Handle state persists in the metadata store for reconnection validation. Includes App Instance ID support for failover scenarios. Full CRUD REST API for handle management.

Does NOT include: persistent handles (HA/cluster), H-lease-implies-durability (requires Phase 37 stable), cross-protocol conflict checking for durability grants (Phase 39), Prometheus metrics.

</domain>

<decisions>
## Implementation Decisions

### Timeout & Lifecycle
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

### Reconnect Validation
- Full MS-SMB2 compliance: implement all 14+ reconnect validation checks
- Security context validation: username match + session key hash comparison (hash stored, not raw key)
- Return specific per-check NTSTATUS error codes (STATUS_OBJECT_NAME_NOT_FOUND for missing handle, STATUS_ACCESS_DENIED for security mismatch, STATUS_INVALID_PARAMETER for wrong CreateGuid, etc.)
- Debug-level structured logging for each validation step (check name, pass/fail, expected vs actual)
- Client must SESSION_SETUP + TREE_CONNECT to same share before reconnect attempt
- Restore original oplock/lease state on successful reconnect (no re-negotiation)
- Support cross-connection reconnect (new TCP connection, same client identity)
- DesiredAccess and ShareAccess must match original CREATE values (prevent privilege escalation)
- Durable handle established after full compound request completes (not mid-compound)
- V2 reconnect is idempotent: same CreateGuid returns success on duplicate reconnect
- File-still-exists check: verify metadata handle valid + path unchanged. Don't check size/timestamps

### V1 vs V2 Scope
- Implement both V1 (DHnQ/DHnC) and V2 (DH2Q/DH2C)
- V1 requires batch oplock to grant durability (per MS-SMB2 3.3.5.9.6)
- V2: accept client's CreateGuid (16-byte client-generated value)
- V2: accept client's requested timeout but cap at server's configured maximum; return granted timeout in response
- No persistent handles (SMB2_DHANDLE_FLAG_PERSISTENT) - defer to future HA work
- When both DHnQ and DH2Q present in same CREATE: V2 takes precedence, ignore DHnQ
- Include SMB2_CREATE_APP_INSTANCE_ID context support
- App Instance ID collision: force-close the old handle (Hyper-V failover pattern)

### Persistence Strategy
- DurableHandleStore as sub-interface of MetadataStore (embedded, like LockStore and ClientRegistrationStore)
- Implementations in all three backends: memory, badger, postgres
- Memory implementation: handles survive disconnect but not restart (for testing/ephemeral)
- Badger/Postgres: handles survive server restart
- Persist full OpenFile state: FileID, Path, ShareName, DesiredAccess, ShareAccess, MetadataHandle, PayloadID, OplockLevel, CreateGuid, AppInstanceId, CreateOptions, timestamps, username, session key hash
- Multi-key lookup support: by primary ID, by CreateGuid (V2 reconnect), by AppInstanceId (failover), by FileHandle (conflict check)
- On startup: load persisted handles, adjust remaining timeout based on server downtime (if server was down 30s and timeout is 60s, handle has 30s left)

### Code Structure & Integration
- Create `durable_context.go` in `internal/adapter/smb/v2/handlers/` following `lease_context.go` pattern
- ProcessDurableHandleContext / ProcessDurableReconnectContext functions called from CREATE handler at Step 8b
- NFS opens do NOT affect durability granting (cross-protocol is Phase 39 scope)
- Modify `closeFilesWithFilter` in handler.go to skip files with durable handles; persist their state to DurableHandleStore before cleanup proceeds
- Conformance test suite for DurableHandleStore (like `storetest/` pattern) + handler-level tests for CREATE context parsing, reconnect validation, scavenger behavior

### REST API
- Full CRUD endpoints for durable handle management
- GET /api/v1/durable-handles - list active durable handles
- DELETE /api/v1/durable-handles/:id - force-close (full cleanup: release locks, flush caches, delete-on-close)
- No Prometheus metrics in this phase

### Documentation
- Update ARCHITECTURE.md with durable handle state flow section
- No separate SMB.md doc in this phase

### Claude's Discretion
- Exact DurableHandleStore interface method signatures
- Badger key encoding scheme for durable handle records
- Postgres migration schema
- Scavenger goroutine internal structure
- REST API response format for durable handle listing
- Exact wire format parsing details for DHnQ/DH2Q/DHnC/DH2C structures
- Integration test setup and fixture design

</decisions>

<specifics>
## Specific Ideas

- Follow `lease_context.go` + `ProcessLeaseCreateContext` pattern exactly for the new `durable_context.go`
- Follow `ClientRegistrationStore` pattern for the `DurableHandleStore` interface (CRUD + list + query by key)
- The scavenger goroutine should be started/stopped by the SMB adapter lifecycle (like how the oplock manager is initialized)
- Reconnect validation should be a single function with early-return on first failure, logging each check
- Session key hash should use SHA-256 of the session key bytes for comparison

</specifics>

<code_context>
## Existing Code Insights

### Reusable Assets
- `lease_context.go`: Pattern for create context processing (ProcessLeaseCreateContext, FindCreateContext, EncodeCreateContexts)
- `ClientRegistrationStore` (`pkg/metadata/lock/client_store.go`): Pattern for persisting protocol state as MetadataStore sub-interface
- `OpenFile` struct (`handler.go:135-192`): Full file state to serialize/deserialize for durable handle records
- `CleanupSession` / `closeFilesWithFilter` (`handler.go:327-385`): Integration point for durable handle preservation
- `storetest/` conformance tests: Pattern for testing store interface implementations across backends
- `smbenc.NewReader` / `smbenc.NewWriter`: Binary encoding/decoding for create context wire formats

### Established Patterns
- Create context processing: parse in CREATE handler, delegate to dedicated `*_context.go` file
- MetadataStore sub-interfaces: define interface in `pkg/metadata/lock/`, implement in `memory/`, `badger/`, `postgres/`
- Session management: `session.Manager` + `session.Session` for session lifecycle
- Handler cleanup: `CleanupSession` -> `closeFilesWithFilter` -> per-file cleanup logic

### Integration Points
- CREATE handler (`create.go`): Add durable handle context processing at Step 8b (after lease processing)
- `handler.go` `closeFilesWithFilter`: Filter out durable handles before closing
- `handler.go` `NewHandlerWithSessionManager`: Initialize durable handle scavenger
- `pkg/metadata/store.go`: Add DurableHandleStore sub-interface to MetadataStore
- `pkg/metadata/store/memory/`, `badger/`, `postgres/`: Implement DurableHandleStore
- SMB adapter lifecycle: Start/stop scavenger goroutine
- REST API router: Add durable handle endpoints

</code_context>

<deferred>
## Deferred Ideas

- H-lease-implies-durability: implicit durability when client holds Handle lease (add after Phase 37 is stable)
- Persistent handles (SMB2_DHANDLE_FLAG_PERSISTENT): requires HA/cluster support, defer to future milestone
- Prometheus metrics for durable handles: can be added in a future enhancement
- Cross-protocol durability conflict checking: belongs in Phase 39 (Cross-Protocol Integration)

</deferred>

---

*Phase: 38-durable-handles*
*Context gathered: 2026-03-02*
