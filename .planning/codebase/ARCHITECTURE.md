# Architecture

**Analysis Date:** 2026-02-02

## Pattern Overview

**Overall:** Three-tier hierarchical architecture with centralized runtime coordination.

**Key Characteristics:**
- Central `Runtime` manages all operations and state coordination
- Protocol adapters (NFS, SMB) provide file access interfaces
- Metadata and payload stores decouple file structure from content storage
- Cache layer with transfer manager orchestrates persistence to durable backends
- Control plane separates persistent configuration (SQLite/PostgreSQL) from ephemeral runtime state

## Layers

**Layer 1: Protocol Adapters**
- Purpose: Implement network file access protocols (NFSv3, SMB)
- Location: `pkg/adapter/{nfs,smb}/`, `internal/protocol/{nfs,smb}/`
- Contains: Protocol-specific wire format handling, RPC dispatch, connection management
- Depends on: Runtime (for store/service access), internal protocol implementations
- Used by: Clients via TCP/IP connections
- Key class: `NFSAdapter` in `pkg/adapter/nfs/nfs_adapter.go` manages TCP listener, connection tracking, graceful shutdown

**Layer 2: Control Plane Runtime**
- Purpose: Central orchestrator for all operations and state management
- Location: `pkg/controlplane/runtime/runtime.go`
- Contains: Store/adapter lifecycle, share management, permission resolution, mount tracking
- Depends on: Metadata stores, payload services, adapter factories, control plane store
- Used by: Protocol adapters, API handlers, CLI commands
- Key operations: `Serve()` blocks until shutdown, `CreateAdapter()`, `DeleteAdapter()`, `GetShare()`

**Layer 3: API & Configuration**
- Purpose: Remote management interface and configuration validation
- Location: `internal/controlplane/api/handlers/`, `pkg/controlplane/api/`
- Contains: HTTP handlers for users, groups, shares, stores, adapters
- Depends on: Runtime, control plane store
- Used by: dittofsctl CLI, external REST clients

**Layer 4: Services (Metadata & Payload)**
- Purpose: Coordinate metadata store and payload operations per share
- Location: `pkg/metadata/service.go`, `pkg/payload/service.go`
- Contains: Business logic routing, transaction coordination, authentication
- Depends on: Metadata/payload stores, cache, transfer manager
- Used by: Protocol adapters, API handlers
- Key pattern: Services route operations to correct store implementation based on share name

**Layer 5: Storage Backends**
- Purpose: Persist file metadata and content
- Location: `pkg/metadata/store/{memory,badger,postgres}/`, `pkg/payload/store/{memory,fs,s3}/`, `pkg/cache/`
- Contains: In-memory buffers (cache), embedded databases (BadgerDB), distributed databases (PostgreSQL), S3 storage, filesystem storage
- Depends on: External stores (S3 via AWS SDK, PostgreSQL via GORM, BadgerDB via pkg/badgerdb)
- Used by: Services, transfer manager
- Key abstraction: Metadata stores implement `Files`, `Objects`, `Shares` interfaces; payload stores implement `BlockStore`

## Data Flow

**File Read Operation:**

```
1. NFS READ (protocol handler in internal/protocol/nfs/v3/handlers/)
   ↓
2. Dispatch to NFSAdapter.nfsHandler.HandleRead()
   ↓
3. Runtime.GetShare(shareName) → Share with metadata/payload stores
   ↓
4. MetadataService.GetFile(fileHandle) → File attributes, ContentID
   ↓
5. PayloadService.ReadAt(contentID, offset, length)
   ↓
6. Cache.ReadAt() → Check buffer (sparse file support via coverage bitmap)
   ↓
7. If cache miss: TransferManager.EnsureAvailable() → Download from block store + prefetch
   ↓
8. Return data to NFS client
```

**File Write Operation:**

```
1. NFS WRITE (protocol handler)
   ↓
2. Dispatch to NFSAdapter.nfsHandler.HandleWrite()
   ↓
3. MetadataService.WriteFile() → Update metadata, timestamps, size
   ↓
4. PayloadService.WriteAt(contentID, data, offset) → Cache.WriteAt()
   ↓
5. TransferManager.OnWriteComplete() → Check if 4MB block is complete
   ↓
6. If complete: Eager upload in background (via TransferQueue)
   ↓
7. Return acknowledgment to NFS client (cache-backed, crash-safe via WAL mmap)
```

**NFS COMMIT Operation (Flush to Durable Storage):**

```
1. NFS COMMIT (protocol handler)
   ↓
2. PayloadService.Flush(contentID) → Non-blocking
   ↓
3. TransferManager.Flush() → Enqueue remaining partial blocks
   ↓
4. Return immediately (data safe in WAL mmap cache)
   ↓
5. TransferQueue background workers upload blocks to S3/filesystem
```

**State Management:**

Control plane has two components that must stay synchronized:

1. **Persistent Store** (`pkg/controlplane/store/`):
   - GORM-based database (SQLite or PostgreSQL)
   - Stores: users, groups, shares, store configs, adapter configs, permissions
   - Persisted to disk/network
   - Location: `pkg/controlplane/store/gorm.go`

2. **Runtime Ephemeral State** (`pkg/controlplane/runtime/`):
   - In-memory metadata store instances (loaded from config)
   - Active shares with root handles
   - Mount tracking for NFS/SMB clients
   - Running adapters (NFS, SMB servers)
   - Location: `pkg/controlplane/runtime/runtime.go`

**Synchronization Rules:**
- Runtime loads shares from Store on startup
- All mutations go through Runtime methods: `CreateAdapter()`, `DeleteAdapter()`, etc.
- Runtime updates both Store (persistent) AND in-memory state atomically
- API handlers always delegate to Runtime (never touch Store directly)

## Key Abstractions

**1. FileHandle (Opaque Identifier)**
- Purpose: Represents files/directories, stable across server restarts
- Pattern: Each metadata store implementation generates handles differently
  - Memory store: UUID (unstable, testing only)
  - BadgerDB: Path-based (stable, path recovery possible)
  - PostgreSQL: UUID with shareName encoding (multi-node)
- Usage: Never parse handle contents; pass directly to store operations
- Location: Defined in `pkg/metadata/types.go`

**2. MetadataStore Interface**
- Purpose: Abstraction for file metadata persistence
- Location: `pkg/metadata/store.go` (Files, Objects, Shares interfaces)
- Implementations:
  - Memory: `pkg/metadata/store/memory/store.go` - Fast, ephemeral
  - BadgerDB: `pkg/metadata/store/badger/store.go` - Persistent, embedded
  - PostgreSQL: `pkg/metadata/store/postgres/store.go` - Distributed, HA
- Key operations: GetFile, PutFile, GetChild, SetChild, GenerateHandle
- All implementations are thread-safe

**3. BlockStore Interface**
- Purpose: Abstraction for file content persistence
- Location: `pkg/payload/store/store.go`
- Implementations:
  - Memory: `pkg/payload/store/memory/` - Fast, ephemeral, testing
  - Filesystem: `pkg/payload/store/fs/` - Local storage, SAN/NAS
  - S3: `pkg/payload/store/s3/` - Cloud storage, production
- Key operations: WriteBlock, ReadBlock, ReadBlockRange, DeleteBlock
- Block size: 4MB (immutable once written)

**4. ObjectStore Interface**
- Purpose: Content-addressed deduplication metadata
- Location: `pkg/metadata/object.go`
- Pattern: SHA-256 hashes of blocks for deduplication
- Embedded in metadata stores (not a separate interface)
- Usage: `TransferManager.Upload()` checks `ObjectStore.FindBlockByHash()` before uploading

**5. AuthContext**
- Purpose: Thread through all operations for permission checking
- Location: `pkg/metadata/authentication.go`
- Contains: Client address, auth flavor (AUTH_UNIX, AUTH_NULL), Unix credentials (UID, GID, GIDs)
- Pattern: Created in `dispatch.go:ExtractAuthContext()`, passed to all repository methods
- Export-level access control (AllSquash, RootSquash) applied during mount in `CheckExportAccess()`

**6. Share & Share Runtime State**
- Purpose: Map export names to metadata/payload stores
- Location: `pkg/controlplane/models/share.go` (persistent), `pkg/controlplane/runtime/shares.go` (ephemeral)
- Persistent: Share name, metadata store reference, payload store reference, permissions
- Ephemeral: Root handle (obtained on share load), last access tracking
- Pattern: One share = one metadata store + one payload store (both required)

**7. Adapter Lifecycle**
- Purpose: Protocol server management interface
- Location: `pkg/adapter/adapter.go`
- Pattern:
  1. Creation: `adapterFactory(config)` creates protocol server instance
  2. SetRuntime: Inject Runtime for store access
  3. Serve: Blocks until context cancelled or fatal error
  4. Stop: Graceful shutdown with timeout
- Used by: Runtime coordinates all adapter lifecycle
- Key implementation: `NFSAdapter` in `pkg/adapter/nfs/nfs_adapter.go`

## Entry Points

**Server Entry Point:**
- Location: `cmd/dittofs/main.go`
- Execution: `cmd/dittofs/commands/start.go:startCmd`
- Responsibilities:
  1. Load configuration from file/env
  2. Create control plane store (SQLite or PostgreSQL)
  3. Create admin user if first time
  4. Load shares and metadata stores
  5. Create and start protocol adapters (NFS, SMB)
  6. Start REST API server
  7. Start metrics server (optional)
  8. Block on adapter.Serve() until shutdown signal

**Client Entry Point:**
- Location: `cmd/dittofsctl/main.go`
- Execution: `cmd/dittofsctl/commands/root.go:rootCmd`
- Responsibilities:
  1. Parse subcommand (user, group, share, store, adapter, settings)
  2. Get authenticated HTTP client via `cmdutil.GetAuthClient()`
  3. Call API handlers via `pkg/apiclient/`
  4. Format and display output (table, JSON, YAML)

**API Handler Entry Points:**
- Location: `internal/controlplane/api/handlers/`
- HTTP routes defined in `pkg/controlplane/api/api.go`
- Pattern:
  1. Middleware extracts JWT token (or denies access)
  2. Handler parses HTTP request
  3. Delegates to Runtime methods
  4. Formats JSON response
- Examples: `HandleCreateUser()`, `HandleCreateShare()`, `HandleCreateAdapter()`

## Error Handling

**Strategy:** Layered errors with context propagation.

**Patterns:**

1. **Domain Errors** (semantic failures):
   - `pkg/metadata/errors.go`: ErrNotFound, ErrNotDirectory, ErrAccess, ErrExist, ErrNotEmpty
   - Returned with `logger.Debug()` (expected failures like file not found)
   - Protocol handlers convert to NFS error codes

2. **System Errors** (unexpected failures):
   - I/O errors from block store, database errors
   - Returned with `logger.Error()` (invariant violations, infrastructure problems)
   - Protocol handlers return NFS3ERR_IO or NFS3ERR_SERVERFAULT

3. **API Errors** (`internal/controlplane/api/handlers/problem.go`):
   - RFC 7807 Problem Details format
   - HTTP status codes: 400 (invalid), 401 (auth), 403 (permission), 404 (not found), 500 (server error)
   - Example: CreateUser returns 400 with error details on validation failure

4. **CLI Error Handling** (`cmd/dittofsctl/cmdutil/util.go`):
   - `HandleError(cmd, err)` prints user-friendly message
   - Suggests workarounds for common issues
   - `--verbose` flag shows full stack traces

## Cross-Cutting Concerns

**Logging:**
- Framework: `internal/logger/` (structured, level-based)
- Configuration: `pkg/config/LoggingConfig` specifies level, format, output
- Patterns:
  - `logger.Debug()`: Expected failures (file not found, permission denied)
  - `logger.Info()`: State changes (adapter started, user created)
  - `logger.Error()`: Unexpected failures (I/O errors, invariants)
- Output formats: text (human-readable, colors for TTY), json (structured for log aggregation)

**Validation:**
- Metadata validation: `pkg/metadata/validation.go` (file names, paths, permissions)
- Config validation: `pkg/config/` uses mapstructure + validator tags
- Store validation: `HealthCheck()` method on all stores
- API validation: HTTP handlers validate input before delegating to Runtime

**Authentication:**
- Auth context threading: `pkg/metadata/authentication.go`
- Export-level access control: NFS mount protocol (`mount/handlers/mnt.go`) applies AllSquash/RootSquash
- Request-level identity: AuthContext extracted from RPC auth flavor
- API authentication: JWT tokens via `internal/controlplane/api/auth/jwt_service.go`
- SMB authentication: NTLM/SPNEGO via `internal/auth/{ntlm,spnego}/`

**Graceful Shutdown:**
- Pattern: Context cancellation propagates shutdown signal
- NFSAdapter shutdown:
  1. Close listener (stop accepting connections)
  2. Cancel shutdownCtx (signal in-flight requests to abort)
  3. Wait (up to ShutdownTimeout) for connections to complete
  4. Force-close remaining connections
- Location: `pkg/adapter/nfs/nfs_adapter.go:Serve()` with `stop()` coordination
- Runtime shutdown: Stops all adapters and auxiliary servers sequentially

**Metrics & Observability:**
- Prometheus metrics: `pkg/metrics/prometheus/`
- OpenTelemetry tracing: `internal/telemetry/`
- Disabled by default (zero overhead)
- Configuration: `pkg/config/MetricsConfig`, `pkg/config/TelemetryConfig`
- NFS-specific metrics: Request counts, latencies, bytes transferred, active connections

## Cache & Transfer Architecture

**Three-Layer Content Model:**

```
PayloadService (high-level API)
    ↓
Cache (4MB block buffers, in-memory, WAL-backed, LRU eviction)
    ↓ (eager upload of complete blocks)
TransferManager (orchestrates uploads/downloads/prefetch)
    ↓ (background workers)
TransferQueue (priority scheduling: downloads > uploads > prefetch)
    ↓ (workers process background operations)
BlockStore (S3, filesystem, memory - persistent storage)
    ↓
ObjectStore (SHA-256 hashes for deduplication)
```

**Key Design:**
- Cache is mandatory: All content operations go through cache first
- Single global cache: Serves all shares (PayloadID uniqueness isolates data)
- Non-blocking COMMIT: Flush enqueues background uploads, returns immediately
- Crash safety: WAL mmap cache survives crashes (OS page cache syncs to disk)
- Content-addressed deduplication: Block hashes enable storage savings for duplicate blocks
- Transfer priority: Downloads > Uploads > Prefetch (minimize read latency)

**Locations:**
- Cache: `pkg/cache/cache.go` with WAL persistence via `pkg/cache/wal/`
- TransferManager: `pkg/payload/transfer/manager.go`
- TransferQueue: `pkg/payload/transfer/queue.go`

## Control Plane Pattern

**Problem Solved:** Enable flexible, named, reusable stores shared across multiple NFS exports.

**Solution:** Registry pattern separating persistent configuration from runtime state.

**Persistent Store (GORM-based):**
- `pkg/controlplane/store/gorm.go`: SQLite or PostgreSQL
- Stores: users, groups, shares, named stores (metadata/payload), adapters, permissions, settings
- Location: Configured via `pkg/config/Config.Database`

**Runtime State (Ephemeral):**
- `pkg/controlplane/runtime/runtime.go`: In-memory
- Loaded from Store on startup
- Metadata store instances, active shares, mounts, running adapters
- Kept in sync by Runtime methods

**Example:**
```yaml
metadata:
  stores:
    fast: { type: memory }           # Create once, shared
    persistent: { type: badger }

content:
  stores:
    s3: { type: s3, bucket: data }

shares:
  - name: /temp
    metadata_store: fast             # Reference by name
    content_store: memory

  - name: /archive
    metadata_store: persistent       # Both shares share same BadgerDB
    content_store: s3                # Both shares share same S3
```

Benefits:
- Resource efficiency: One S3 client serves multiple shares
- Flexible topologies: Mix storage backends per share
- Isolated testing: Each share can use different backends
- Foundation for multi-tenancy: Each tenant could have separate store instances

---

*Architecture analysis: 2026-02-02*
