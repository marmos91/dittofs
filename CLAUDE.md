# CLAUDE.md

This file provides guidance to Claude Code (claude.ai/code) when working with code in this repository.

## Project Overview

DittoFS is an experimental modular virtual filesystem written in Go that decouples file interfaces from storage backends.
It implements NFSv3 protocol server in pure Go (userspace, no FUSE required) with pluggable metadata and content repositories.

**Status**: Experimental - not production ready.

## Documentation Structure

DittoFS has comprehensive documentation organized by topic:

### Core Documentation
- **[README.md](README.md)** - Quick start and project overview
- **[docs/ARCHITECTURE.md](docs/ARCHITECTURE.md)** - Deep dive into design patterns and implementation
- **[docs/CONFIGURATION.md](docs/CONFIGURATION.md)** - Complete configuration guide with examples
- **[docs/NFS.md](docs/NFS.md)** - NFSv3 protocol implementation details and client usage
- **[docs/CONTRIBUTING.md](docs/CONTRIBUTING.md)** - Development guide and contribution guidelines
- **[docs/IMPLEMENTING_STORES.md](docs/IMPLEMENTING_STORES.md)** - Guide for implementing custom metadata and content stores

### Operational Guides
- **[docs/TROUBLESHOOTING.md](docs/TROUBLESHOOTING.md)** - Common issues and solutions
- **[docs/SECURITY.md](docs/SECURITY.md)** - Security considerations and best practices
- **[docs/FAQ.md](docs/FAQ.md)** - Frequently asked questions
- **[docs/RELEASING.md](docs/RELEASING.md)** - Release process and versioning

### Testing and Performance
- **[test/e2e/BENCHMARKS.md](test/e2e/BENCHMARKS.md)** - Performance benchmark documentation
- **[test/e2e/COMPARISON_GUIDE.md](test/e2e/COMPARISON_GUIDE.md)** - Comparing with other NFS implementations

## Essential Commands

### Building
```bash
# Build the main binary
go build -o dittofs cmd/dittofs/main.go

# Install dependencies
go mod download
```

### Configuration
```bash
# Initialize configuration file (creates ~/.config/dittofs/config.yaml)
./dittofs init

# Initialize with custom path
./dittofs init --config /etc/dittofs/config.yaml

# Force overwrite existing config
./dittofs init --force
```

### Running
```bash
# Start server with default config
./dittofs start

# Start with custom config file
./dittofs start --config /path/to/config.yaml

# Start with environment variable overrides
DITTOFS_LOGGING_LEVEL=DEBUG ./dittofs start
DITTOFS_ADAPTERS_NFS_PORT=3049 ./dittofs start

# Common environment variables:
# DITTOFS_LOGGING_LEVEL: DEBUG, INFO, WARN, ERROR
# DITTOFS_LOGGING_FORMAT: text, json (default: text)
# DITTOFS_ADAPTERS_NFS_PORT: NFS server port (default: 12049)
# DITTOFS_SERVER_SHUTDOWN_TIMEOUT: Graceful shutdown timeout (default: 30s)
# DITTOFS_SERVER_RATE_LIMITING_ENABLED: Enable rate limiting (default: false)
# DITTOFS_TELEMETRY_ENABLED: Enable OpenTelemetry tracing (default: false)
# DITTOFS_TELEMETRY_ENDPOINT: OTLP collector endpoint (default: localhost:4317)
```

**Configuration File**: See [docs/CONFIGURATION.md](docs/CONFIGURATION.md) for complete configuration guide.

**Default Location**: `~/.config/dittofs/config.yaml` (or `$XDG_CONFIG_HOME/dittofs/config.yaml`)

### Testing

**Unit and Integration Tests** (fast, no special permissions needed):
```bash
# Run all unit/integration tests
go test ./...

# Run with coverage
go test -cover ./...

# Run with race detection
go test -race ./...

# Run specific package
go test ./pkg/metadata/memory/
```

**E2E Tests** (requires sudo and NFS mount capabilities):
```bash
# Note: E2E tests use build tags and are excluded from `go test ./...`
# Use the provided script (recommended):
cd test/e2e
sudo ./run-e2e.sh

# Or run directly with go test:
sudo go test -tags=e2e -v ./test/e2e/...

# Run specific e2e test:
sudo go test -tags=e2e -v ./test/e2e/ -run TestCreateFile_1MB
```

### NFS Client Testing
```bash
# Mount on Linux (default port 12049)
sudo mount -t nfs -o nfsvers=3,tcp,port=12049,mountport=12049 localhost:/export /mnt/test

# Mount on macOS
sudo mount -t nfs -o nfsvers=3,tcp,port=12049,mountport=12049 localhost:/export /mnt/test

# Mount with custom port (if configured differently)
sudo mount -t nfs -o nfsvers=3,tcp,port=2049,mountport=2049 localhost:/export /mnt/test

# Unmount
sudo umount /mnt/test
```

### Linting and Formatting
```bash
# Format code
go fmt ./...

# Static analysis
go vet ./...
```

### End-to-End Testing
```bash
# Run all E2E tests (requires NFS client and sudo)
cd test/e2e
sudo ./run-e2e.sh

# Run with S3 tests (requires Docker for Localstack)
sudo ./run-e2e.sh --s3

# Run specific test
sudo ./run-e2e.sh --test TestCreateFile_1MB

# Run with verbose output
sudo ./run-e2e.sh --verbose

# Keep Localstack running for repeated testing
sudo ./run-e2e.sh --s3 --keep-localstack

# Run directly with go test (no script) - note the -tags=e2e flag
sudo go test -tags=e2e -v ./test/e2e/...

# Run specific configuration
sudo go test -tags=e2e -v -run "TestCreateFolder/memory-memory" ./test/e2e/

# Run with race detection
sudo go test -tags=e2e -v -race -timeout 30m ./test/e2e/...
```

**E2E Test Features**:
- Tests all storage backend combinations (memory, BadgerDB, filesystem, S3)
- Real NFS mount testing with actual kernel NFS client
- File size tests from 500KB to 100MB
- Shared store scenarios (multiple shares using same stores)
- Optional Localstack integration for S3 testing

**Available Test Configurations**:
- `memory/memory` - Both metadata and content in memory
- `memory/filesystem` - Memory metadata, filesystem content
- `badger/filesystem` - BadgerDB metadata, filesystem content
- `memory/s3` - Memory metadata, S3 content (requires Localstack)
- `badger/s3` - BadgerDB metadata, S3 content (requires Localstack)

See `test/e2e/README.md` for detailed documentation.

## Production Features

### Graceful Shutdown & Connection Management

DittoFS implements comprehensive graceful shutdown with multiple layers:

1. **Automatic Drain Mode**: Listener closes immediately on shutdown signal (no new connections)
2. **Context Cancellation**: Propagates through all request handlers for clean abort
3. **Graceful Wait**: Waits up to `ShutdownTimeout` for connections to complete naturally (configurable, default 30s)
4. **Forced Closure**: After timeout, actively closes TCP connections to release resources
5. **Connection Tracking**: Uses lock-free `sync.Map` for high-performance tracking

**Shutdown Flow**:
```
SIGINT/SIGTERM → Cancel Context → Close Listener → Wait (up to timeout) → Force Close
```

**Configuration**:
```yaml
server:
  shutdown_timeout: 30s  # Customize graceful shutdown timeout
```

### Connection Pooling & Performance

- **Buffer Pooling**: Three-tier `sync.Pool` (4KB/64KB/1MB) reduces allocations by ~90%
- **Concurrent-Safe Tracking**: `sync.Map` for connection registry (optimized for high churn scenarios)
- **Goroutine-Per-Connection**: Correct model for stateful NFS protocol
- **Zero-Copy Operations**: Procedure data references pooled buffers directly
- **Optimized Accept Loop**: Minimal select overhead in hot path

### Rate Limiting

Optional request rate limiting to protect against traffic spikes:

- **Token Bucket Algorithm**: Allows burst traffic while limiting sustained rate
- **Per-Adapter Configuration**: Apply different limits to different protocols
- **Global and Adapter-Level**: Set server-wide defaults or override per adapter
- **Zero Overhead When Disabled**: No performance impact when not enabled

**Configuration**:
```yaml
server:
  # Global rate limiting (applies to all adapters)
  rate_limiting:
    enabled: true
    requests_per_second: 5000    # Sustained rate limit
    burst: 10000                  # Burst capacity (2x sustained recommended)

adapters:
  nfs:
    # Optional: override for this adapter
    rate_limiting:
      enabled: true
      requests_per_second: 10000
      burst: 20000
```

### Prometheus Metrics

Optional metrics collection with zero overhead when disabled:

- Request counters by procedure and status
- Request duration histograms
- In-flight request gauges
- Bytes transferred counters
- Active connection gauge
- Connection lifecycle counters (accepted/closed/force-closed)
- Storage operation metrics (S3, BadgerDB)

**Configuration**:
```yaml
server:
  metrics:
    enabled: true
    port: 9090
```

Metrics exposed at the `/metrics` endpoint (default port 9090).

### OpenTelemetry Tracing

Optional distributed tracing with zero overhead when disabled:

- NFS operation spans (READ, WRITE, LOOKUP, etc.)
- Storage backend spans (S3, BadgerDB, filesystem)
- Cache operation spans (hits, misses, flushes)
- Request context propagation (client IP, file handles, paths)

**Configuration**:
```yaml
telemetry:
  enabled: true
  endpoint: "localhost:4317"  # OTLP gRPC endpoint
  insecure: true              # Skip TLS for local development
  sample_rate: 1.0            # 1.0 = all traces, 0.1 = 10%
```

Traces are exported via OTLP gRPC to any compatible collector (Jaeger, Tempo, Honeycomb, etc.).

### Structured Logging

Configurable log output format:

- **text**: Human-readable format with colors (for terminal)
- **json**: Structured JSON for log aggregation (Elasticsearch, Loki, etc.)

**Configuration**:
```yaml
logging:
  level: INFO      # DEBUG, INFO, WARN, ERROR
  format: json     # text or json
  output: stdout   # stdout, stderr, or file path
```

JSON logs include structured fields:
```json
{"time":"2024-01-15T10:30:45.123Z","level":"INFO","msg":"NFS READ","client":"192.168.1.100","handle":"abc123","offset":0,"count":65536}
```

## Architecture

### Core Abstraction Layers

DittoFS uses the **Registry pattern** to enable named, reusable stores that can be shared across multiple NFS exports:

```
┌─────────────────────────────────────────┐
│         Protocol Adapters               │
│   (NFS, SMB future, WebDAV future)      │
│   pkg/adapter/{nfs,smb}/                │
└───────────────┬─────────────────────────┘
                │
                ▼
┌─────────────────────────────────────────┐
│         DittoServer                     │
│   (Adapter lifecycle management)        │
│   pkg/server/server.go                  │
└───────┬─────────────────────────────────┘
        │
        ▼
┌─────────────────────────────────────────┐
│         Store Registry                  │
│   (Named store management)              │
│   pkg/registry/registry.go              │
│                                         │
│  Stores:                                │
│  - "fast-memory" → Memory stores        │
│  - "persistent"  → BadgerDB + FS        │
│  - "s3-archive"  → BadgerDB + S3        │
└───────┬───────────────────┬─────────────┘
        │                   │
        ▼                   ▼
┌────────────────┐  ┌────────────────────┐
│   Metadata     │  │   Content          │
│     Stores     │  │     Stores         │
│                │  │                    │
│  - Memory      │  │  - Memory          │
│  - BadgerDB    │  │  - Filesystem      │
│                │  │  - S3              │
└────────────────┘  └────────────────────┘
```

### Key Interfaces

**1. Store Registry** (`pkg/registry/registry.go`)
- Central registry for managing named metadata and content stores
- Stores are created once and shared across multiple NFS shares/exports
- Enables flexible configurations (e.g., "fast-memory", "s3-archive", "persistent")
- Handles store lifecycle and identity resolution
- Maps file handles to their originating share for proper store routing

**2. Adapter Interface** (`pkg/adapter/adapter.go`)
- Each protocol implements the `Adapter` interface
- Adapters receive a registry reference to resolve stores per-share
- Lifecycle: `SetRegistry() → Serve() → Stop()`
- Multiple adapters can share the same registry
- Thread-safe, supports graceful shutdown

**3. Metadata Store** (`pkg/store/metadata/store.go`)
- Stores file/directory structure, attributes, permissions
- Handles access control and root directory creation
- Implementations:
  - `pkg/store/metadata/memory/`: In-memory (fast, ephemeral, full hard link support)
  - `pkg/store/metadata/badger/`: BadgerDB (persistent, embedded, path-based handles)
- File handles are opaque uint64 identifiers
- BadgerDB handles are path-based, enabling metadata recovery from content store

**4. Content Store** (`pkg/store/content/store.go`)
- Stores actual file data
- Supports read, write-at, truncate operations
- Implementations:
  - `pkg/store/content/memory/`: In-memory (fast, ephemeral, testing)
  - `pkg/store/content/fs/`: Filesystem-backed (local/network storage)
  - `pkg/store/content/s3/`: **Production-ready** S3 storage with:
    - **Range Reads**: Efficient byte-range requests (100x faster for small reads from large files)
    - **Streaming Multipart Uploads**: Automatic multipart for large files (98% memory reduction)
    - **Configurable Retry**: Exponential backoff for transient errors (network, throttling, 5xx)
    - **Stats Caching**: Intelligent caching reduces S3 ListObjects calls by 99%+
    - **Metrics Support**: Optional Prometheus instrumentation
    - **Path-Based Keys**: Objects stored as `export/path/to/file` for easy inspection

**5. Cache Layer** (`pkg/cache/`)
- Optional unified caching layer for read/write performance
- **One cache per share** serving both reads and writes
- **Content-store agnostic**: Cache doesn't know about S3 multipart, fsync, etc.
- **Three states**: Buffering (dirty), Uploading (flush in progress), Cached (clean)
- **Key features**:
  - Buffers writes before flushing to content store on COMMIT
  - Serves reads from cache when available
  - LRU eviction with dirty entry protection
  - Read cache coherency via mtime/size validation
  - Inactivity-based finalization for write completion detection
- Implementations:
  - `pkg/cache/memory/`: In-memory cache with size limits
- **Configuration** (per share):
  - `cache.enabled`: Enable/disable caching
  - `cache.max_size`: Maximum cache size
  - `cache.finalize_timeout`: Inactivity before finalizing writes
  - `cache.read_ttl`: Max age for cached reads (optional)
- **Metrics**: Cache hits/misses/invalidations exposed via Prometheus
- **Documentation**: See [docs/CACHE.md](docs/CACHE.md) for detailed design

### Directory Structure

```
dittofs/
├── cmd/dittofs/              # Main application entry point
│   └── main.go               # Server startup, config parsing, init
│
├── pkg/                      # Public API (stable interfaces)
│   ├── adapter/              # Protocol adapter interface
│   │   ├── adapter.go        # Core Adapter interface
│   │   └── nfs/              # NFS adapter implementation
│   ├── registry/             # Store registry
│   │   ├── registry.go       # Central store registry
│   │   ├── share.go          # Share configuration
│   │   └── access.go         # Identity mapping and handle resolution
│   ├── store/                # Storage layer
│   │   ├── metadata/         # Metadata store interface
│   │   │   ├── store.go      # Store interface
│   │   │   ├── memory/       # In-memory implementation
│   │   │   └── badger/       # BadgerDB implementation
│   │   └── content/          # Content store interface
│   │       ├── store.go      # Store interface
│   │       ├── memory/       # In-memory implementation
│   │       ├── fs/           # Filesystem implementation
│   │       └── s3/           # S3 implementation
│   ├── config/               # Configuration parsing
│   │   ├── config.go         # Main config struct
│   │   ├── stores.go         # Store configuration
│   │   └── registry.go       # Registry initialization
│   └── server/               # DittoServer orchestration
│       └── server.go         # Multi-adapter server management
│
├── internal/                 # Private implementation details
│   ├── protocol/nfs/         # NFS protocol implementation
│   │   ├── dispatch.go       # RPC procedure routing
│   │   ├── rpc/              # RPC layer (call/reply handling)
│   │   ├── xdr/              # XDR encoding/decoding
│   │   ├── types/            # NFS constants and types
│   │   ├── mount/handlers/   # Mount protocol procedures
│   │   └── v3/handlers/      # NFSv3 procedures (READ, WRITE, etc.)
│   └── logger/               # Logging utilities
│
└── test/                     # Test suites
    ├── integration/          # Integration tests (S3, BadgerDB)
    └── e2e/                  # End-to-end tests (real NFS mounts)
```

## Store Registry Pattern

The Store Registry is the central innovation enabling flexible, multi-share configurations.

### How It Works

1. **Named Store Creation**: Stores are created with unique names (e.g., "fast-memory", "s3-archive")
2. **Share-to-Store Mapping**: Each NFS share references a store by name
3. **Handle Identity**: File handles encode both the share ID and file-specific data
4. **Store Resolution**: When handling operations, the registry decodes the handle to identify the share, then routes to the correct stores

### Configuration Example

```yaml
# Define named stores (created once, shared across shares)
metadata:
  stores:
    fast-meta:
      type: memory
    persistent-meta:
      type: badger
      badger:
        db_path: /data/metadata

content:
  stores:
    fast-content:
      type: memory
    s3-content:
      type: s3
      s3:
        region: us-east-1
        bucket: my-bucket

# Define shares that reference stores
shares:
  - name: /temp
    metadata_store: fast-meta           # Uses memory store for metadata
    content_store: fast-content         # Uses memory store for content

  - name: /archive
    metadata_store: persistent-meta     # Uses BadgerDB for metadata
    content_store: s3-content           # Uses S3 for content
```

### Benefits

- **Resource Efficiency**: One S3 client serves multiple shares
- **Flexible Topologies**: Mix ephemeral and persistent storage per-share
- **Isolated Testing**: Each share can use different backends
- **Future Multi-Tenancy**: Foundation for per-tenant store isolation

## Important Design Principles

### 1. Separation of Concerns

**Protocol handlers should ONLY handle protocol-level concerns:**
- XDR encoding/decoding
- RPC message framing
- Procedure dispatch
- Converting between wire types and internal types

**Business logic belongs in repository implementations:**
- Permission checks (`CheckAccess`)
- File creation/deletion
- Directory traversal
- Metadata updates

Example from handlers:
```go
// GOOD: Handler delegates to repository
func HandleLookup(ctx *AuthContext, dirHandle, name string) {
    // Parse XDR request
    // Call repo.Lookup(ctx, dirHandle, name)
    // Encode XDR response
}

// BAD: Handler implements permission checks
func HandleLookup(ctx *AuthContext, dirHandle, name string) {
    attr := getFile(dirHandle)
    if attr.UID != ctx.UID { /* check permissions */ }  // ❌ Wrong layer
}
```

### 2. Authentication Context Threading

All operations require an `*metadata.AuthContext` containing:
- Client address
- Auth flavor (AUTH_UNIX, AUTH_NULL)
- Unix credentials (UID, GID, GIDs)

The context is created in `dispatch.go:ExtractAuthContext()` and passed through:
```
RPC Call → ExtractAuthContext() → Handler → Repository Method
```

Export-level access control (AllSquash, RootSquash) is applied during mount in `CheckExportAccess()`, creating an `AuthContext` with effective credentials for the mount session.

### 3. File Handle Management

File handles are **opaque 64-bit identifiers**:
- Generated by metadata store
- Encode share identity for registry routing
- Never parsed or interpreted by protocol handlers
- Must remain stable across server restarts for production

Handles are obtained via:
- Root export handle: `GetRootHandle(exportPath)`
- File creation: `CreateFile()`, `CreateDirectory()`, etc.
- Lookup: `GetChild(parentHandle, name)`

### 4. Error Handling

Return proper NFS error codes via `metadata.ExportError`:
```go
// Examples from metadata/errors.go
ErrNotDirectory      // NFS3ERR_NOTDIR
ErrNoEntity          // NFS3ERR_NOENT
ErrAccess            // NFS3ERR_ACCES
ErrExist             // NFS3ERR_EXIST
ErrNotEmpty          // NFS3ERR_NOTEMPTY
```

Log appropriately:
- `logger.Debug()`: Expected/normal errors (permission denied, file not found)
- `logger.Error()`: Unexpected errors (I/O errors, invariant violations)

## NFSv3 Implementation Details

### RPC Flow
1. TCP connection accepted
2. RPC message parsed (`rpc/message.go`)
3. Program/version/procedure validated
4. Auth context extracted (`dispatch.go:ExtractAuthContext`)
5. Procedure handler dispatched
6. Handler calls repository methods
7. Response encoded and sent

### Critical Procedures

**Mount Protocol** (`internal/protocol/nfs/mount/handlers/`)
- `MNT`: Validates export access, records mount, returns root handle
- `UMNT`: Removes mount record
- `EXPORT`: Lists available exports
- `DUMP`: Lists active mounts (can be restricted)

**NFSv3 Core** (`internal/protocol/nfs/v3/handlers/`)
- `LOOKUP`: Resolve name in directory → file handle
- `GETATTR`: Get file attributes
- `SETATTR`: Update attributes (size, mode, times)
- `READ`: Read file content (uses content store)
- `WRITE`: Write file content (coordinates metadata + content stores)
- `CREATE`: Create file
- `MKDIR`: Create directory
- `REMOVE`: Delete file
- `RMDIR`: Delete empty directory
- `RENAME`: Move/rename file
- `READDIR` / `READDIRPLUS`: List directory entries

### Write Coordination Pattern

WRITE operations require coordination between metadata and content stores:

```go
// 1. Update metadata (validates permissions, updates size/timestamps)
attr, preSize, preMtime, preCtime, err := metadataStore.WriteFile(handle, newSize, authCtx)

// 2. Write actual data via content store
err = contentStore.WriteAt(attr.ContentID, data, offset)

// 3. Return updated attributes to client for cache consistency
```

The metadata store:
- Validates write permission
- Returns pre-operation attributes (for WCC data)
- Updates file size if extended
- Updates mtime/ctime timestamps
- Ensures ContentID exists

### Buffer Pooling

Large I/O operations use buffer pools (`internal/protocol/nfs/bufpool.go`):
- Reduces GC pressure
- Reuses buffers for READ/WRITE
- Automatically sizes based on request

## Common Development Tasks

### Adding a New NFS Procedure

1. Add handler in `internal/protocol/nfs/v3/handlers/` or `internal/protocol/nfs/mount/handlers/`
2. Implement XDR request/response parsing
3. Extract auth context from call
4. Delegate business logic to repository methods
5. Update dispatch table in `dispatch.go`
6. Add test coverage

### Adding a New Store Backend

**Metadata Store:**
1. Implement `pkg/store/metadata/Store` interface
2. Handle file handle generation (must be unique and stable)
3. Implement root directory creation (`CreateRootDirectory`)
4. Implement permission checking in `CheckAccess()`
5. Ensure thread safety (concurrent access across shares)
6. Consider persistence strategy for handles

**Content Store:**
1. Implement `pkg/store/content/Store` interface
2. Support random-access reads/writes (`ReadAt`/`WriteAt`)
3. Handle sparse files and truncation
4. Consider implementing `ReadAtContentStore` for efficient partial reads
5. Test with the integration test suite in `test/integration/`

### Adding a New Protocol Adapter

1. Create new package in `pkg/adapter/`
2. Implement `Adapter` interface:
   - `Serve(ctx)`: Start protocol server
   - `Stop(ctx)`: Graceful shutdown
   - `SetRegistry()`: Receive store registry reference
   - `Protocol()`: Return name
   - `Port()`: Return listen port
3. Register in `cmd/dittofs/main.go`
4. Update README with usage instructions

## Testing Approach

### Unit Tests
- Test repository implementations in isolation
- Mock dependencies where needed
- Focus on business logic correctness
- Run with: `go test ./...`

### Integration Tests
- Test complete request/response cycles with real backends
- Verify S3, BadgerDB, and filesystem integration
- Test with in-memory stores for speed
- Verify protocol compliance
- Located in: `test/integration/`

### End-to-End (E2E) Tests
- Real NFS mount testing with actual kernel NFS client
- Tests all storage backend combinations
- Verifies complete workflows (create, read, write, delete, etc.)
- Tests shared store scenarios (multiple shares using same stores)
- Includes performance benchmarks (separate from regular tests)
- Located in: `test/e2e/`

**Running E2E Tests**:
```bash
# Run all E2E tests (requires NFS client and sudo)
go test -v -timeout 30m ./test/e2e/...

# Run specific test suite
go test -v ./test/e2e/ -run TestE2E/memory
go test -v ./test/e2e/ -run TestE2E/badger
go test -v ./test/e2e/ -run TestE2E/s3

# Run with race detection
go test -v -race -timeout 30m ./test/e2e/...
```

**File Size Testing**:
```bash
# Test specific file sizes
sudo go test -v -run TestCreateFile_500KB ./test/e2e/
sudo go test -v -run TestCreateFile_1MB ./test/e2e/
sudo go test -v -run TestCreateFile_10MB ./test/e2e/
sudo go test -v -run TestCreateFile_100MB ./test/e2e/

# Test read/write operations
sudo go test -v -run TestReadFilesBySize ./test/e2e/
sudo go test -v -run TestWriteThenReadBySize ./test/e2e/
```

### Manual NFS Testing
```bash
# Start server
./dittofs start

# Or with debug logging
DITTOFS_LOGGING_LEVEL=DEBUG ./dittofs start

# Mount and test operations
sudo mount -t nfs -o nfsvers=3,tcp,port=12049,mountport=12049 localhost:/export /mnt/test
cd /mnt/test
ls -la              # READDIR / READDIRPLUS
cat readme.txt      # READ
echo "test" > new   # CREATE + WRITE
mkdir foo           # MKDIR
rm new              # REMOVE
rmdir foo           # RMDIR
mv file1 file2      # RENAME
ln file1 file2      # LINK (hard link)
```

## Known Limitations

1. **Memory metadata is ephemeral**: In-memory metadata store loses all data on restart
   - Use BadgerDB backend for persistence
   - BadgerDB provides full persistence with path-based handles

2. **Hard link support varies by backend**:
   - **Memory store**: Full hard link support with proper link count tracking
   - **BadgerDB store**: Link counts tracked internally but `LINK` procedure returns NFS3ERR_NOTSUPP
   - **Both stores**: Link counts not exposed in `FileAttr` struct, always reported as 1 to NFS clients
   - See TODO in `pkg/store/metadata/file.go:89` and `pkg/store/metadata/badger/file.go:734`

3. **Inaccurate link counts in NFS responses**:
   - Always reported as 1 for all files/directories
   - Incorrect for directories (should be 2 + subdirectory count)
   - Incorrect for files with multiple hard links
   - Internally tracked but not exposed to protocol layer

4. **No file locking**: NLM (Network Lock Manager) protocol not implemented
   - Applications requiring file locks may not work correctly
   - No protection against concurrent writes from multiple clients

5. **No NFSv4**: Only NFSv3 is supported
   - No ACLs, no named attributes, no delegations
   - Use NFSv3-compatible clients only

6. **Limited security**: Basic AUTH_UNIX only
   - No Kerberos authentication
   - No built-in encryption (use VPN or network-level encryption)
   - See [docs/SECURITY.md](docs/SECURITY.md) for recommendations

7. **Single-node only**: No distributed/HA support
   - No clustering or high availability
   - No replication (except via S3 bucket replication)
   - Single point of failure

8. **No garbage collection**:
   - Orphaned content in S3/filesystem is not automatically cleaned up
   - Manual cleanup may be needed if files are deleted while server is down
   - Consider using S3 lifecycle policies for automatic cleanup

See [docs/FAQ.md](docs/FAQ.md) and [docs/TROUBLESHOOTING.md](docs/TROUBLESHOOTING.md) for workarounds and more details.

## References

- [RFC 1813 - NFS Version 3](https://tools.ietf.org/html/rfc1813)
- [RFC 5531 - RPC Protocol](https://tools.ietf.org/html/rfc5531)
- [RFC 4506 - XDR](https://tools.ietf.org/html/rfc4506)
- See README.md for detailed architecture documentation
- See CONTRIBUTING for contribution guidelines
