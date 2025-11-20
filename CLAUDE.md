# CLAUDE.md

This file provides guidance to Claude Code (claude.ai/code) when working with code in this repository.

## Project Overview

DittoFS is an experimental modular virtual filesystem written in Go that decouples file interfaces from storage backends.
It implements NFSv3 protocol server in pure Go (userspace, no FUSE required) with pluggable metadata and content repositories.

**Status**: Experimental - not production ready.

## Essential Commands

### Building
```bash
# Build the main binary
go build -o dittofs cmd/dittofs/main.go

# Install dependencies
go mod download
```

### Running
```bash
# Run server with defaults (port 2049, INFO logging)
./dittofs

# Run with debug logging and custom settings
./dittofs -port 2049 -log-level DEBUG -content-path /tmp/dittofs-content

# Available flags:
# -port: Server port (default: 2049)
# -log-level: DEBUG, INFO, WARN, ERROR (default: INFO)
# -content-path: Storage path for file content (default: /tmp/dittofs-content)
# -max-connections: Max concurrent connections (default: 0 = unlimited)
# -dump-restricted: Restrict DUMP to localhost only
```

### Testing
```bash
# Run all tests
go test ./...

# Run with coverage
go test -cover ./...

# Run with race detection
go test -race ./...

# Run specific package
go test ./pkg/metadata/memory/
```

### NFS Client Testing
```bash
# Mount on Linux
sudo mount -t nfs -o nfsvers=3,tcp,port=2049,mountport=2049 localhost:/export /mnt/test

# Mount on macOS (requires resvport)
sudo mount -t nfs -o nfsvers=3,tcp,port=2049,mountport=2049,resvport localhost:/export /mnt/test

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

### Benchmarking
```bash
# Run comprehensive benchmark suite (separate from tests, run periodically)
./scripts/benchmark.sh

# Run with profiling (CPU and memory)
./scripts/benchmark.sh --profile

# Compare with previous results
./scripts/benchmark.sh --compare

# Custom configuration
BENCH_TIME=30s BENCH_COUNT=5 ./scripts/benchmark.sh

# Run specific benchmarks manually
go test -bench='BenchmarkE2E/memory/ReadThroughput' -benchtime=20s ./test/e2e/
go test -bench='BenchmarkE2E/filesystem' -benchmem ./test/e2e/

# Generate CPU profile for specific benchmark
go test -bench=BenchmarkE2E/memory/WriteThroughput/100MB \
    -cpuprofile=cpu.prof -benchtime=30s ./test/e2e/

# Analyze profile
go tool pprof cpu.prof
go tool pprof -http=:8080 cpu.prof
```

**Important**: Benchmarks are stress tests designed to push DittoFS to its limits. They:
- Test with files from 4KB to 100MB
- Create thousands of files/directories
- Run mixed concurrent workloads
- Profile CPU and memory usage
- Compare different storage backends

Results are saved to `benchmark_results/<timestamp>/` and should NOT be committed to the repository.

See `test/e2e/BENCHMARKS.md` for detailed documentation and `test/e2e/COMPARISON_GUIDE.md` for comparing with other NFS implementations.

## Production Features

### Graceful Shutdown & Connection Management

DittoFS implements comprehensive graceful shutdown with multiple layers:

1. **Automatic Drain Mode**: Listener closes immediately on shutdown signal (no new connections)
2. **Context Cancellation**: Propagates through all request handlers for clean abort
3. **Graceful Wait**: Waits up to `ShutdownTimeout` for connections to complete naturally
4. **Forced Closure**: After timeout, actively closes TCP connections to release resources
5. **Connection Tracking**: Uses lock-free `sync.Map` for high-performance tracking

**Shutdown Flow**:
```
SIGINT/SIGTERM → Cancel Context → Close Listener → Wait (up to timeout) → Force Close
```

### Connection Pooling & Performance

- **Buffer Pooling**: Three-tier `sync.Pool` (4KB/64KB/1MB) reduces allocations by ~90%
- **Concurrent-Safe Tracking**: `sync.Map` for connection registry (optimized for high churn scenarios)
- **Goroutine-Per-Connection**: Correct model for stateful NFS protocol
- **Zero-Copy Operations**: Procedure data references pooled buffers directly
- **Optimized Accept Loop**: Minimal select overhead in hot path

### Prometheus Metrics

Optional metrics collection with zero overhead when disabled:

- Request counters by procedure and status
- Request duration histograms
- In-flight request gauges
- Bytes transferred counters
- Active connection gauge
- Connection lifecycle counters (accepted/closed/force-closed)

Metrics exposed on port 9090 at the `/metrics` endpoint.

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
  - `pkg/store/metadata/memory/`: In-memory (fast, ephemeral)
  - `pkg/store/metadata/badger/`: BadgerDB (persistent, embedded)
- File handles are opaque uint64 identifiers

**4. Content Store** (`pkg/store/content/store.go`)
- Stores actual file data
- Supports read, write-at, truncate operations
- Implementations:
  - `pkg/store/content/memory/`: In-memory (fast, ephemeral)
  - `pkg/store/content/fs/`: Filesystem-backed storage
  - `pkg/store/content/s3/`: S3-backed storage (multipart, streaming)

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

### Integration Tests
- Test complete request/response cycles
- Use in-memory repositories for speed
- Verify protocol compliance

### Manual NFS Testing
```bash
# Start server
./dittofs -log-level DEBUG

# Mount and test operations
sudo mount -t nfs -o nfsvers=3,tcp,port=2049,mountport=2049 localhost:/export /mnt/test
cd /mnt/test
ls -la              # READDIR / READDIRPLUS
cat readme.txt      # READ
echo "test" > new   # CREATE + WRITE
mkdir foo           # MKDIR
rm new              # REMOVE
rmdir foo           # RMDIR
```

## Known Limitations

1. **No persistence**: In-memory metadata is lost on restart
2. **No file locking**: NLM protocol not implemented
3. **No NFSv4**: Only NFSv3 is supported
4. **Limited security**: Basic AUTH_UNIX only, no Kerberos
5. **No caching**: Every operation hits repository
6. **Single-node only**: No distributed/HA support

## References

- [RFC 1813 - NFS Version 3](https://tools.ietf.org/html/rfc1813)
- [RFC 5531 - RPC Protocol](https://tools.ietf.org/html/rfc5531)
- [RFC 4506 - XDR](https://tools.ietf.org/html/rfc4506)
- See README.md for detailed architecture documentation
- See CONTRIBUTING for contribution guidelines
