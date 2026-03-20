# Architecture Patterns: v0.10.0 Production Hardening + SMB Protocol Fixes

**Domain:** Production hardening and SMB protocol completeness for existing multi-protocol virtual filesystem
**Researched:** 2026-03-20
**Confidence:** HIGH (direct source code analysis of all integration points)

## Executive Summary

The v0.10.0 features span three distinct architectural domains: (1) SMB protocol completeness (credits, multi-channel, signing fix, WPTS), (2) metadata-layer features (quotas, payload stats, trash), and (3) operational features (client tracking). The existing architecture accommodates all features without fundamental redesign. The key architectural insight is that the **MetadataService** is the natural hub for quotas, trash, and payload stats, while the **SMB session/connection pipeline** absorbs credit flow and multi-channel. Client tracking requires a new cross-cutting service in the runtime.

Eight features, three dependency chains, no circular dependencies. Build order should prioritize features that unblock others: macOS signing fix is standalone and high-value; credit flow improves protocol compliance for all WPTS tests; quotas feed into both NFS FSSTAT and SMB FileFsFullSizeInformation; client tracking depends only on adapter hooks.

## Recommended Architecture

### System Context: Where New Features Live

```
                        +----------------------------------+
                        |         Protocol Adapters         |
                        |                                   |
                        | NFS:                  SMB:        |
                        |  FSSTAT quota check    Credits    |
                        |  FSINFO quota check    Multi-ch   |
                        |                        Signing    |
                        |                        WPTS       |
                        +--------+--------+--------+-------+
                                 |        |        |
                    +------------+        |        +----------+
                    |                     |                    |
           +--------v--------+   +--------v--------+  +-------v--------+
           |  MetadataService |   |     Runtime     |  | Connection     |
           |                  |   |                 |  | Pool           |
           | - Quota enforce  |   | - ClientRegistry|  | - Session      |
           | - Trash logic    |   | - Mount Tracker |  |   binding      |
           | - Payload stats  |   | - Shares Service|  | - Credit flow  |
           +--------+---------+   +--------+--------+  +-------+--------+
                    |                      |                    |
           +--------v--------+   +--------v--------+          |
           |  MetadataStore  |   | ControlPlane DB |          |
           |  (per-share)    |   | (GORM/SQLite)   |          |
           |                 |   |                  |          |
           | - UsedSize      |   | - Share.Quota*   |          |
           | - File counts   |   | - TrashConfig   |          |
           +-----------------+   +-----------------+          |
                                                              |
                                                     +--------v--------+
                                                     |   BlockStore    |
                                                     |   (per-share)   |
                                                     |                 |
                                                     | - GetStats()    |
                                                     | - LocalDiskUsed |
                                                     +-----------------+
```

### Component Boundaries

| Component | Responsibility | What Changes | Communicates With |
|-----------|---------------|--------------|-------------------|
| `internal/adapter/smb/session/` | Session lifecycle, credit accounting | Credit charge validation, grant enforcement | ConnInfo, Handler |
| `internal/adapter/smb/v2/handlers/` | SMB command processing | WPTS fixes, quota in QueryInfo | MetadataService, Runtime |
| `internal/adapter/smb/framing.go` | Wire protocol read/write | No change (signing fix is in crypto_state) | Connection |
| `internal/adapter/smb/crypto_state.go` | Preauth hash chain | **FIX**: macOS hash mismatch | Session, signing |
| `pkg/adapter/smb/connection.go` | Per-connection handler | Multi-channel session binding | Adapter, Handler |
| `pkg/adapter/smb/adapter.go` | SMB adapter lifecycle | Multi-channel connection registry | Runtime, BaseAdapter |
| `pkg/adapter/base.go` | Shared TCP lifecycle | Client tracking hooks | Runtime, ConnectionFactory |
| `internal/adapter/nfs/v3/handlers/fsstat.go` | NFS FSSTAT | Quota-aware statistics | MetadataService |
| `pkg/metadata/service.go` | Metadata business logic | **MODIFY**: Quota enforcement, trash, payload stats | MetadataStore, BlockStore |
| `pkg/metadata/interface.go` | MetadataService interface | New methods: trash, quota-aware stats | Protocol adapters |
| `pkg/metadata/types.go` | Core types | QuotaConfig, TrashConfig types | Service, store |
| `pkg/controlplane/models/share.go` | Share DB model | **MODIFY**: Add Quota*, Trash* columns | Control plane store |
| `pkg/controlplane/runtime/runtime.go` | Runtime composition | **MODIFY**: Wire ClientRegistry | All sub-services |
| `pkg/controlplane/runtime/mounts/service.go` | Mount tracking | Extended to track connections | Adapters |
| `pkg/blockstore/engine/` | Per-share block store | **READ-ONLY**: Stats for payload usage | Shares service |

### Data Flow: Quota Enforcement on Write

```
Client WRITE request
    |
    v
NFS/SMB Handler
    |
    v
MetadataService.PrepareWrite(ctx, handle, newSize)
    |
    +-- Lookup share from handle
    +-- Get quota config from share (QuotaBytes, QuotaFiles)
    +-- If quota configured:
    |       Get current stats: FilesystemStatistics
    |       newUsed = stats.UsedBytes + (newSize - currentSize)
    |       if newUsed > QuotaBytes:
    |           return ErrQuotaExceeded (maps to NFS3ERR_NOSPC / STATUS_DISK_FULL)
    |
    v
MetadataService.CommitWrite(ctx, intent)
    |
    v
BlockStore.WriteAt(ctx, payloadID, data, offset)
```

### Data Flow: Trash (Soft-Delete)

```
Client DELETE request
    |
    v
NFS/SMB Handler
    |
    v
MetadataService.RemoveFile(ctx, parentHandle, name)
    |
    +-- Resolve share from handle
    +-- Check share trash config (enabled? retention?)
    +-- If trash enabled:
    |       TrashFile(ctx, file, parentHandle, name)
    |           Move file to .trash/{YYYY-MM-DD}/{original-name}-{uuid}
    |           Update parent dir entry (remove original)
    |           Record original path in trash metadata
    |           Return success (file appears deleted to client)
    |   Else:
    |       Permanent delete (current behavior)
    |
    v
Background TrashScavenger (goroutine per share)
    |
    +-- Periodically scan .trash/ directories
    +-- Delete items older than retention period
    +-- Permanent deletion via existing RemoveFile path
```

### Data Flow: Credit Flow Control

```
SMB2 Request arrives
    |
    v
ProcessSingleRequest(ctx, hdr, body, raw, connInfo, encrypted, asyncCb)
    |
    +-- Extract CreditCharge from SMB2 header (offset 12, 2 bytes)
    +-- Extract CreditRequest from SMB2 header (offset 14, 2 bytes)
    +-- sessionManager.ValidateCharge(sessionID, creditCharge)
    |       if outstanding < creditCharge:
    |           return STATUS_INSUFFICIENT_RESOURCES (before handler dispatch)
    +-- sessionManager.RequestStarted(sessionID)
    |
    v
Handler processes request
    |
    v
Build response
    +-- creditGrant = sessionManager.GrantCredits(sessionID, requested, charge)
    +-- Set CreditResponse in response header
    +-- sessionManager.RequestCompleted(sessionID)
```

### Data Flow: Multi-Channel Session Binding

```
Second TCP connection opens
    |
    v
NEGOTIATE on new connection (independent preauth hash)
    |
    v
SESSION_SETUP with SessionBindingFlag (bit 0x01 in SecurityMode)
    |
    v
handleSessionSetup():
    +-- SessionID != 0 and Flags.SMB2_SESSION_FLAG_BINDING set
    +-- Verify: session exists in sessionManager
    +-- Verify: same username/domain as original session
    +-- Verify: signing key matches (preauth hash chain for 3.1.1)
    +-- Bind new connection to existing session:
    |       adapter.sessionConns.Store(sessionID, newConnInfo)
    |       connection.TrackSession(sessionID)
    |       // Both connections now route to same session
    +-- Derive channel-specific signing key (if 3.1.1)
    |
    v
Both connections serve requests for same SessionID
    |
    +-- Reads/writes can use either connection
    +-- Lease breaks routed to any connection for the session
    +-- Connection close does NOT destroy session (other connection alive)
```

## Feature Integration Details

### 1. SMB 3.1.1 Signing on macOS (#252)

**Problem:** macOS `mount_smbfs` rejects signatures because the preauth integrity hash chain diverges between client and server during SESSION_SETUP.

**Root Cause (Hypothesis):** The dispatch hooks in `hooks.go` update the preauth hash for SESSION_SETUP requests/responses, but the timing may be wrong relative to when the session's preauth hash is initialized. Specifically:

- `sessionPreauthBeforeHook` stashes the raw request bytes AND updates the per-session hash if the session already exists
- For NTLM multi-round auth, the first SESSION_SETUP response (STATUS_MORE_PROCESSING_REQUIRED) must update the per-session hash, but the session may not yet have a per-session hash entry in the PreauthSessionTable
- macOS uses SMB 3.1.1 with AES-CMAC signing (not GMAC), and the signing key is derived from the preauth hash at SESSION_SETUP completion

**Integration Point:** `internal/adapter/smb/crypto_state.go` and `internal/adapter/smb/hooks.go`

**Components Modified:**
- `crypto_state.go`: Fix `InitSessionPreauthHash` / `UpdateSessionPreauthHash` ordering
- `hooks.go`: Ensure session preauth hash entries are created at the right moment
- Potentially `session_setup.go`: Fix hash consumption timing for NTLM continuation

**No new components needed.** This is a bug fix in existing preauth hash pipeline.

**Verification:** macOS `mount_smbfs //user@host/share /mnt` succeeds with signing enabled.

### 2. SMB Credit Flow Control

**Problem:** Current credit implementation has grant logic but lacks server-side charge validation. The `CreditCharge` field from the SMB2 header is not validated against the session's outstanding credit balance before dispatch.

**Current State (Detailed):**
- `session/credits.go`: Defines `CreditConfig`, `CreditStrategy` (Fixed/Echo/Adaptive), `CalculateCreditCharge()`
- `session/session.go`: `ConsumeCredits()`, `GrantCredits()`, `GetOutstanding()` -- all working
- `session/manager.go`: `GrantCredits()` implements the adaptive algorithm -- working
- `internal/adapter/smb/response.go`: Sets `CreditResponse` in the SMB2 header -- working
- **Missing:** Validation that `CreditCharge <= Outstanding` before dispatch
- **Missing:** Rejection with `STATUS_INSUFFICIENT_RESOURCES` when charge exceeds balance
- **Missing:** Proper CreditCharge extraction from header for multi-credit operations (READ/WRITE > 64KB)

**Integration Point:** `internal/adapter/smb/response.go` (ProcessSingleRequest)

**Components Modified:**
- `response.go` (or new `credits.go` in `internal/adapter/smb/`): Add pre-dispatch credit charge validation
- `session/manager.go`: Add `ValidateCharge(sessionID, charge)` method
- `v2/handlers/read.go`, `write.go`: Ensure CreditCharge from header is threaded to response

**No new packages needed.** The credit infrastructure exists; this wires up the enforcement path.

### 3. SMB Multi-Channel Session Binding

**Problem:** Currently each TCP connection has exactly one session. Multi-channel allows multiple TCP connections to share a single session for throughput and failover.

**Current State:**
- `pkg/adapter/smb/connection.go`: `Connection` has `sessions map[uint64]struct{}` tracking sessions on this connection
- `pkg/adapter/smb/adapter.go`: `sessionConns sync.Map` maps sessionID to ConnInfo for lease break routing
- `session/manager.go`: Session lookup is by sessionID (already supports multiple connections accessing same session)
- `Connection.cleanupSessions()`: Destroys sessions on connection close -- **must change** for multi-channel

**Integration Points:**
1. `v2/handlers/session_setup.go`: Detect `SMB2_SESSION_FLAG_BINDING` in flags, validate binding
2. `pkg/adapter/smb/adapter.go`: Track multiple connections per session (`sessionConns` becomes `sessionID -> []ConnInfo`)
3. `pkg/adapter/smb/connection.go`: `cleanupSessions()` must check if other connections still serve the session before destroying it
4. `internal/adapter/smb/session/session.go`: Add `ChannelCount atomic.Int32` for multi-connection awareness

**Components Modified:**
- `adapter.go`: Change `sessionConns sync.Map` from `sessionID -> *ConnInfo` to `sessionID -> []*ConnInfo`
- `connection.go`: `cleanupSessions()` check peer connections before session destruction
- `session_setup.go`: Session binding validation (username match, signing key derivation per channel)
- `lease_notifier.go`: Send lease breaks to any/all connections for a session

**New Types:**
```go
// In adapter.go or new file
type ChannelEntry struct {
    ConnInfo  *smb.ConnInfo
    ChannelID uint64  // Connection-unique identifier
    BoundAt   time.Time
}
```

### 4. WPTS Conformance Fixes

**Current State:** 193 pass / 73 known / 0 new / 69 skipped. The 73 known failures break into categories:

- **ChangeNotify (20 failures):** Requires `CHANGE_NOTIFY` completion on directory modifications. The handler exists but notifications are not triggered on file create/delete/rename within watched directories.
- **Timestamp flaky (1):** Race condition in timestamp comparison.
- **Remaining (~52):** Various protocol edge cases in create, set-info, query-info, and oplock/lease scenarios.

**Integration Points:** Primarily `internal/adapter/smb/v2/handlers/` -- individual handler fixes.

**Components Modified:**
- `change_notify.go`: Wire up notification dispatch from `MetadataService` file operations
- `create.go`: Edge cases in open disposition handling
- `set_info.go`: Additional FileInfoClass implementations
- `query_info.go`: Missing or incomplete FileInfoClass responses

**Cross-Component:** ChangeNotify requires the MetadataService to emit directory change events. The `lock.DirChangeNotifier` interface already exists in `pkg/metadata/lock/` and is wired into the LockManager. The SMB adapter already registers as a break callback handler. ChangeNotify completion needs a similar callback pattern.

### 5. Share Quotas (#232)

**Problem:** No per-share storage limits. `FilesystemStatistics` returns hardcoded defaults (1TB total).

**Integration Points:**

1. **Control Plane DB:** Add quota columns to `models.Share`
   ```go
   // In models/share.go
   QuotaBytes    int64  `gorm:"default:0" json:"quota_bytes"`     // 0 = unlimited
   QuotaFiles    int64  `gorm:"default:0" json:"quota_files"`     // 0 = unlimited
   ```

2. **Runtime Shares Service:** Thread quota into `shares.Share` and `shares.ShareConfig`
   ```go
   // In runtime/shares/service.go
   type Share struct {
       // ... existing fields ...
       QuotaBytes int64
       QuotaFiles int64
   }
   ```

3. **MetadataService:** Quota enforcement on write and create operations
   - `PrepareWrite()`: Check byte quota before allowing write
   - `CreateFile()`: Check file count quota before creation
   - `CreateDirectory()`: Check file count quota
   - `GetFilesystemStatistics()`: Return quota-adjusted statistics (TotalBytes = quota when set)

4. **NFS FSSTAT handler:** Already delegates to `MetadataService.GetFilesystemStatistics()` -- no change needed if quota is enforced at the metadata level.

5. **SMB QueryInfo:** `FileFsFullSizeInformation` (case 7) and `FileFsSizeInformation` (case 3) in `query_info.go` already call `MetadataService.GetFilesystemStatistics()` -- no change needed.

6. **REST API and CLI:** New endpoints for quota management
   - `PUT /api/v1/shares/{name}/quota` -- set/update quota
   - `dfsctl share create --quota-bytes 10G --quota-files 100000`
   - `dfsctl share update --quota-bytes 20G`

**Components Modified:**
- `pkg/controlplane/models/share.go`: Add QuotaBytes, QuotaFiles columns
- `pkg/controlplane/runtime/shares/service.go`: Thread quota config
- `pkg/metadata/service.go`: Quota enforcement in PrepareWrite, CreateFile, CreateDirectory
- `pkg/metadata/types.go`: QuotaConfig type, ErrQuotaExceeded error
- `pkg/metadata/store/memory/server.go`: Return quota-adjusted GetFilesystemStatistics
- `pkg/metadata/store/badger/server.go`: Same
- `pkg/metadata/store/postgres/server.go`: Same
- `internal/controlplane/api/handlers/`: Quota API endpoints
- `cmd/dfsctl/commands/share/`: CLI quota commands

### 6. Payload Stats (#216)

**Problem:** `UsedSize` in filesystem statistics reflects metadata-tracked file sizes, not actual block storage consumption. A 1MB file may use 1.5MB on disk due to block alignment.

**Integration Points:**

1. **BlockStore Stats:** `engine.BlockStoreStats` already tracks `LocalDiskUsed`, `BlocksLocal`, `BlocksRemote`. The data exists.

2. **MetadataService:** `GetFilesystemStatistics()` currently delegates entirely to the store. It needs to also query the per-share BlockStore for actual storage usage.

3. **Runtime Resolution:** `MetadataService` does not currently have access to BlockStores. Two options:
   - **Option A (Recommended):** MetadataService gains a `BlockStoreStatsProvider` interface that the Runtime satisfies
   - **Option B:** FilesystemStatistics is assembled at the Runtime level, not in MetadataService

   Option A is cleaner because `GetFilesystemStatistics` is already on MetadataServiceInterface.

**Components Modified:**
- `pkg/metadata/service.go`: Accept `BlockStoreStatsProvider` callback for actual disk usage
- `pkg/metadata/types.go`: Add `ActualUsedBytes` field to `FilesystemStatistics`
- `pkg/controlplane/runtime/runtime.go`: Wire BlockStore stats into MetadataService
- `internal/adapter/nfs/v3/handlers/fsstat.go`: Use `ActualUsedBytes` when available
- `internal/adapter/smb/v2/handlers/query_info.go`: Use `ActualUsedBytes` for FsSize responses

### 7. Protocol-Agnostic Client Tracking (#157)

**Problem:** No unified view of connected NFS and SMB clients. Mount tracker (`mounts/service.go`) tracks mounts but not live connection state.

**Current State:**
- `mounts.Tracker`: Tracks `MountInfo{ClientAddr, Protocol, ShareName, MountedAt, AdapterData}`
- NFS adapter records mounts in the tracker at MNT time
- SMB adapter records tree connects in the tracker
- `BaseAdapter.ConnCount` tracks active connection count (atomic int32)
- No per-client record with authentication identity, last-activity, or connection list

**Architecture: New ClientRegistry Service**

```go
// pkg/controlplane/runtime/clients/service.go (NEW)

type ClientRecord struct {
    ID            string    `json:"id"`             // Unique: "protocol:clientIP"
    ClientAddr    string    `json:"client_addr"`
    Protocol      string    `json:"protocol"`       // "nfs" or "smb"
    ConnectedAt   time.Time `json:"connected_at"`
    LastActivity  time.Time `json:"last_activity"`
    Username      string    `json:"username"`       // Authenticated user (empty for anonymous)
    MountPoints   []string  `json:"mount_points"`   // Active share mounts
    ConnectionIDs []string  `json:"connection_ids"`  // For multi-channel
}

type Registry struct {
    mu      sync.RWMutex
    clients map[string]*ClientRecord
}

func (r *Registry) Register(protocol, clientAddr, username string) string
func (r *Registry) UpdateActivity(id string)
func (r *Registry) AddMount(id, shareName string)
func (r *Registry) RemoveMount(id, shareName string)
func (r *Registry) Deregister(id string)
func (r *Registry) List() []*ClientRecord
func (r *Registry) ListByProtocol(protocol string) []*ClientRecord
```

**Integration Points:**
1. **NFS Adapter:** Register client on first RPC call, deregister on connection close
2. **SMB Adapter:** Register client on SESSION_SETUP, deregister on connection close
3. **Runtime:** Own ClientRegistry, expose via API
4. **REST API:** `GET /api/v1/clients` returns client list
5. **CLI:** `dfsctl client list` with table/JSON output

**Components Created:**
- `pkg/controlplane/runtime/clients/service.go`
- `internal/controlplane/api/handlers/clients.go`
- `cmd/dfsctl/commands/client/`

**Components Modified:**
- `pkg/controlplane/runtime/runtime.go`: Add ClientRegistry field
- `pkg/adapter/base.go`: Add ClientRegistryHook interface for adapters
- `pkg/adapter/smb/connection.go`: Register/deregister on connect/disconnect
- `internal/adapter/nfs/dispatch.go`: Register/deregister on connect/disconnect

### 8. Trash / Soft-Delete (#190)

**Problem:** File deletion is permanent. Users cannot recover accidentally deleted files.

**Architecture Decision: Trash in Metadata Layer**

Trash operates entirely in the metadata layer. Deleted files are moved to a `.trash/` directory within the share's metadata namespace. Block data is NOT moved -- only the metadata path changes. The blocks remain intact and are garbage-collected only when trash items expire.

**Key Design:**
- `.trash/` is a real directory in the metadata store, hidden from READDIR
- Each trash item records its original path in a file attribute (extended attribute or naming convention)
- TrashScavenger goroutine runs per-share on a configurable interval
- Trash is per-share (each share has its own `.trash/`)
- Trash can be disabled per-share (default: disabled for backward compat)

**Integration Points:**

1. **MetadataService:** New `TrashFile()` method; modify `RemoveFile()` to check trash config
2. **MetadataStore:** No interface change needed -- uses existing Move/CreateDirectory
3. **Share Config:** `TrashEnabled bool`, `TrashRetention time.Duration`
4. **Control Plane DB:** Add trash columns to `models.Share`
5. **READDIR filtering:** Both NFS ReadDirectory and SMB QueryDirectory must hide `.trash/`
6. **REST API:** `GET /api/v1/shares/{name}/trash` to list trashed items, `POST .../trash/restore` to restore

**Components Modified:**
- `pkg/metadata/service.go`: `RemoveFile()` checks trash config, new `TrashFile()`, `RestoreFromTrash()`, `PurgeTrash()`
- `pkg/metadata/types.go`: TrashConfig type
- `pkg/metadata/file_remove.go`: Redirect to trash instead of permanent delete
- `pkg/metadata/directory.go`: Hide `.trash/` from directory listings
- `pkg/controlplane/models/share.go`: TrashEnabled, TrashRetentionSec columns
- `pkg/controlplane/runtime/shares/service.go`: Thread trash config, start TrashScavenger

**New Components:**
- `pkg/metadata/trash.go`: TrashScavenger, trash naming conventions, restore logic

## Patterns to Follow

### Pattern 1: Quota Enforcement at Service Boundary

**What:** Enforce quotas in `MetadataService` methods (PrepareWrite, CreateFile, CreateDirectory), not in protocol handlers. The service already serves as the business logic layer between handlers and stores.

**When:** Any operation that increases storage usage.

**Example:**
```go
func (s *MetadataService) PrepareWrite(ctx *AuthContext, handle FileHandle, newSize uint64) (*WriteOperation, error) {
    // ... existing validation ...

    // Quota check (new)
    if quota := s.getQuotaForHandle(handle); quota != nil {
        stats, _ := s.GetFilesystemStatistics(ctx.Context, handle)
        if stats != nil && quota.ByteLimit > 0 {
            projected := stats.UsedBytes + (newSize - currentSize)
            if projected > uint64(quota.ByteLimit) {
                return nil, NewQuotaExceededError(quota.ByteLimit, projected)
            }
        }
    }

    // ... existing logic ...
}
```

**Why:** Protocol handlers map errors to protocol-specific codes (`NFS3ERR_NOSPC`, `STATUS_DISK_FULL`). The service returns a domain error; handlers translate it.

### Pattern 2: Registry Pattern for Client Tracking

**What:** Use a runtime-owned registry (not DB-backed) for client tracking. Clients are ephemeral connection state, not persistent configuration.

**When:** Tracking connected clients across protocols.

**Why:** Client records are lost on server restart (correct -- connections are gone). Using a DB would add write overhead per RPC for `UpdateActivity`. The mount tracker already follows this pattern.

### Pattern 3: Dispatch Hook for Credit Validation

**What:** Add credit charge validation as a pre-dispatch step in `ProcessSingleRequest()`, before the handler is called. This mirrors the existing signing verification hook pattern.

**When:** Every SMB2 request that carries a CreditCharge > 0.

**Example:**
```go
// In response.go ProcessSingleRequest, before handler dispatch:
if hdr.CreditCharge > 0 {
    sess, ok := connInfo.SessionManager.GetSession(hdr.SessionID)
    if ok && sess.GetOutstanding() < int32(hdr.CreditCharge) {
        return sendErrorResponse(connInfo, hdr, types.StatusInsufficientResources)
    }
}
```

**Why:** Credit validation is a cross-cutting concern that applies to all commands. It should not be duplicated in every handler.

### Pattern 4: Multi-Channel via Session Binding Flag

**What:** Detect multi-channel in SESSION_SETUP via `Flags & 0x01` (SMB2_SESSION_FLAG_BINDING). The new connection joins the existing session rather than creating a new one. Connection teardown checks session refcount.

**When:** Client establishes second TCP connection with same session credentials.

**Why:** MS-SMB2 3.3.5.5 defines session binding. The session manager already supports multiple callers accessing the same session; the missing piece is the binding validation and connection lifecycle coordination.

### Pattern 5: Trash as Metadata-Only Operation

**What:** Trash moves files in the metadata namespace only. Block data stays in place. Trash expiry triggers real deletion (which eventually triggers block GC).

**When:** File or directory deletion when trash is enabled for the share.

**Why:** Moving block data would be expensive and pointless -- the blocks don't care where the metadata points. Content-addressed storage means the same blocks can be referenced from `.trash/` or the original location. The existing block GC already handles unreferenced blocks.

## Anti-Patterns to Avoid

### Anti-Pattern 1: Quotas in Protocol Handlers

**What:** Checking quotas in NFS FSSTAT, SMB QueryInfo, or write handlers directly.
**Why bad:** Duplicates quota logic across NFS and SMB. Quota changes require updating multiple handlers.
**Instead:** Enforce at MetadataService level. Handlers call the same service methods and receive quota-aware responses.

### Anti-Pattern 2: Credit Validation After Handler Dispatch

**What:** Checking credit balance after the handler has already processed the request.
**Why bad:** The handler may have already modified state (written data, created files). Credit rejection after side effects is inconsistent.
**Instead:** Validate credits before dispatch. Reject immediately with STATUS_INSUFFICIENT_RESOURCES.

### Anti-Pattern 3: Database-Backed Client Registry

**What:** Persisting client connection records in SQLite/PostgreSQL.
**Why bad:** Every RPC call would need a DB write for `UpdateActivity`. Client records are ephemeral and lost on restart anyway (connections are gone).
**Instead:** In-memory registry with sync.RWMutex, same pattern as mounts.Tracker.

### Anti-Pattern 4: Multi-Channel via Global Session Table

**What:** Storing all sessions in a single global table and routing by session ID alone.
**Why bad:** Already the case (session.Manager uses sync.Map). The anti-pattern is NOT adding per-connection binding validation. Without validation, any connection could impersonate any session.
**Instead:** Session binding must verify credentials match. Each connection must track which sessions it is bound to. The adapter's `sessionConns` map must support multiple connections per session.

### Anti-Pattern 5: Trash in Block Store

**What:** Implementing trash as a block-level feature (copying blocks to a "trash" bucket).
**Why bad:** Wastes storage (data duplicated), breaks content-addressed dedup, and requires complex cross-store coordination.
**Instead:** Trash is metadata-only. File appears deleted (removed from parent directory listing), but metadata and blocks remain until trash retention expires.

## New Components Summary

| Component | Location | Purpose |
|-----------|----------|---------|
| ClientRegistry | `pkg/controlplane/runtime/clients/service.go` | Protocol-agnostic client tracking |
| TrashScavenger | `pkg/metadata/trash.go` | Background cleanup of expired trash items |
| Client API handler | `internal/controlplane/api/handlers/clients.go` | REST endpoints for client listing |
| Client CLI commands | `cmd/dfsctl/commands/client/` | `dfsctl client list` |

## Modified Components Summary

| Component | What Changes | Why |
|-----------|-------------|-----|
| `internal/adapter/smb/crypto_state.go` | Fix preauth hash ordering | macOS signing (#252) |
| `internal/adapter/smb/hooks.go` | Fix session preauth hash init timing | macOS signing (#252) |
| `internal/adapter/smb/session/manager.go` | Add ValidateCharge method | Credit flow control |
| `internal/adapter/smb/response.go` | Pre-dispatch credit validation | Credit flow control |
| `internal/adapter/smb/v2/handlers/session_setup.go` | Session binding for multi-channel | Multi-channel |
| `internal/adapter/smb/v2/handlers/change_notify.go` | Wire up directory notifications | WPTS conformance |
| `pkg/adapter/smb/adapter.go` | Multi-connection session tracking | Multi-channel |
| `pkg/adapter/smb/connection.go` | Session binding, conditional cleanup | Multi-channel |
| `pkg/controlplane/models/share.go` | Quota*, Trash* columns | Quotas, Trash |
| `pkg/controlplane/runtime/runtime.go` | Wire ClientRegistry | Client tracking |
| `pkg/controlplane/runtime/shares/service.go` | Thread quota/trash config | Quotas, Trash |
| `pkg/metadata/service.go` | Quota enforcement, trash redirect | Quotas, Trash |
| `pkg/metadata/interface.go` | Trash methods, quota-aware stats | Quotas, Trash |
| `pkg/metadata/types.go` | QuotaConfig, TrashConfig, new errors | Quotas, Trash |
| `pkg/metadata/file_remove.go` | Redirect to trash | Trash |
| `pkg/metadata/directory.go` | Hide .trash/ from listings | Trash |
| All MetadataStore implementations | Quota-adjusted GetFilesystemStatistics | Quotas |

## Suggested Build Order

The build order considers dependency chains and testability. Features are grouped into dependency-free units.

### Chain A: SMB Protocol (sequential)

```
A1: macOS Signing Fix (#252)
    |
    v
A2: Credit Flow Control
    |
    v
A3: WPTS Conformance Fixes (iterative)
    |
    v
A4: Multi-Channel Session Binding
```

**Rationale:** Signing fix unblocks macOS testing. Credit fix improves protocol compliance baseline for WPTS. Multi-channel is the most complex SMB feature and can benefit from WPTS test coverage.

### Chain B: Metadata Features (sequential)

```
B1: Share Quotas (#232)
    |
    v
B2: Payload Stats (#216)
    |
    v
B3: Trash / Soft-Delete (#190)
```

**Rationale:** Quotas modify GetFilesystemStatistics, which payload stats also touches -- do quotas first to establish the pattern. Trash depends on the modified RemoveFile path, which should be stable.

### Chain C: Operational (independent)

```
C1: Client Tracking (#157)
```

**Rationale:** Client tracking is fully independent. Can be built in parallel with Chain A or Chain B.

### Recommended Phase Ordering

| Order | Feature | Chain | Depends On | Estimated Complexity |
|-------|---------|-------|------------|---------------------|
| 1 | macOS Signing Fix | A1 | None | Medium (debugging preauth hash) |
| 2 | Share Quotas | B1 | None | Medium (DB migration, service logic) |
| 3 | Credit Flow Control | A2 | A1 | Low (plumbing existing infrastructure) |
| 4 | Payload Stats | B2 | B1 | Low (wire existing BlockStore stats) |
| 5 | Client Tracking | C1 | None | Medium (new service, API, CLI) |
| 6 | WPTS Conformance | A3 | A2 | High (20+ individual fixes) |
| 7 | Trash / Soft-Delete | B3 | B1 | High (new subsystem, scavenger, hiding) |
| 8 | Multi-Channel | A4 | A2, A3 | High (connection lifecycle changes) |

**Phase ordering rationale:**
- macOS signing is highest priority (unblocks a platform)
- Quotas are foundational for payload stats and trash
- Credit flow is low-effort and improves WPTS baseline
- WPTS before multi-channel ensures protocol compliance foundation
- Trash is highest complexity and least urgent -- defer to end
- Multi-channel is highest risk due to connection lifecycle changes -- do last with full test coverage

## Scalability Considerations

| Concern | At 100 users | At 10K users | At 1M users |
|---------|--------------|--------------|-------------|
| Quota checks | Negligible (in-memory stats lookup) | Same | Consider caching quota state |
| Client registry | sync.RWMutex fine | sync.Map might be better | Shard by protocol |
| Trash scavenger | Per-share goroutine, fine | May need throttling | Batched deletion |
| Credit validation | Atomic read per request | Same | Same (lock-free) |
| Multi-channel | Map lookup per request | Same | Consider connection pooling |
| Payload stats | BlockStore.GetStats() is fast | Same | Consider caching |

## Sources

- DittoFS source code analysis: `pkg/metadata/`, `pkg/adapter/smb/`, `internal/adapter/smb/`, `pkg/controlplane/runtime/`, `pkg/blockstore/engine/`
- [MS-SMB2 3.3.5.2: Receiving Any Message](https://learn.microsoft.com/en-us/openspecs/windows_protocols/ms-smb2/) -- Credit validation requirement
- [MS-SMB2 3.3.5.5: Receiving an SMB2 SESSION_SETUP Request](https://learn.microsoft.com/en-us/openspecs/windows_protocols/ms-smb2/) -- Session binding (multi-channel)
- [MS-SMB2 3.3.5.2.1.1: Verifying the Signature](https://learn.microsoft.com/en-us/openspecs/windows_protocols/ms-smb2/) -- Preauth integrity hash for signing
- Existing ROADMAP phases 67, 70-77 for feature specifications
- GitHub issues #252, #232, #216, #157, #190
