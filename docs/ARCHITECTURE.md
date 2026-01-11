# DittoFS Architecture

This document provides a deep dive into DittoFS's architecture, design patterns, and internal implementation.

## Table of Contents

- [Core Abstraction Layers](#core-abstraction-layers)
- [Adapter Pattern](#adapter-pattern)
- [Store Registry Pattern](#store-registry-pattern)
- [Repository Interfaces](#repository-interfaces)
- [Built-In and Custom Backends](#built-in-and-custom-backends)
- [Directory Structure](#directory-structure)

## Core Abstraction Layers

DittoFS uses a **Service-oriented architecture** with the Registry pattern to enable named, reusable stores that can be shared across multiple NFS exports:

```
┌─────────────────────────────────────────┐
│         Protocol Adapters               │
│            (NFS, SMB)                   │
│       pkg/adapter/{nfs,smb}/            │
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
└───────┬─────────────────────────────────┘
        │
        ▼
┌─────────────────────────────────────────┐
│            Services                     │
│   (Business logic & coordination)       │
│                                         │
│  ┌─────────────────┐ ┌────────────────┐ │
│  │ MetadataService │ │ ContentService │ │
│  │ pkg/metadata/   │ │ pkg/content/   │ │
│  │ service.go      │ │ service.go     │ │
│  └────────┬────────┘ └───────┬────────┘ │
│           │                  │          │
│           │    ┌─────────┐   │          │
│           │    │  Cache  │◄──┤          │
│           │    │  Layer  │   │          │
│           │    └─────────┘   │          │
└───────────┼──────────────────┼──────────┘
            │                  │
            ▼                  ▼
┌────────────────┐  ┌────────────────────┐
│   Metadata     │  │   Content          │
│     Stores     │  │     Stores         │
│    (CRUD)      │  │    (CRUD)          │
│                │  │                    │
│  - Memory      │  │  - Memory          │
│  - BadgerDB    │  │  - Filesystem      │
│  - PostgreSQL  │  │  - S3              │
└────────────────┘  └────────────────────┘
```

### Key Interfaces

**1. Store Registry** (`pkg/registry/registry.go`)
- Central registry for managing named metadata and content stores
- Stores are created once and shared across multiple NFS shares/exports
- Enables flexible configurations (e.g., "fast-memory", "s3-archive", "persistent")
- Handles store lifecycle and identity resolution
- Maps file handles to their originating share for proper store routing
- Owns and coordinates Services (MetadataService, ContentService)

**2. Adapter Interface** (`pkg/adapter/adapter.go`)
- Each protocol implements the `Adapter` interface
- Adapters receive a registry reference to access services
- Lifecycle: `SetRegistry() → Serve() → Stop()`
- Multiple adapters can share the same registry
- Thread-safe, supports graceful shutdown

**3. MetadataService** (`pkg/metadata/service.go`)
- **Central service for all metadata operations**
- Routes operations to the correct store based on share name
- Owns LockManager per share (for SMB/NLM byte-range locking)
- Provides high-level operations with business logic
- Protocol handlers should use this instead of stores directly

**4. ContentService** (`pkg/content/service.go`)
- **Central service for all content operations**
- Routes operations to the correct store based on share name
- Owns cache coordination (writes to cache, flushed on COMMIT, reads from cache)
- Provides high-level operations with caching integration
- Protocol handlers should use this instead of stores directly

**5. Metadata Store** (`pkg/metadata/store.go`)
- **Simple CRUD interface** for file/directory metadata
- Stores file structure, attributes, permissions
- Implementations:
  - `pkg/metadata/store/memory/`: In-memory (fast, ephemeral, full hard link support)
  - `pkg/metadata/store/badger/`: BadgerDB (persistent, embedded, path-based handles)
  - `pkg/metadata/store/postgres/`: PostgreSQL (persistent, distributed, UUID-based handles)
- File handles are opaque identifiers (implementation-specific format)

**6. Content Store** (`pkg/content/store.go`)
- **Simple CRUD interface** for file data
- Supports read, write-at, truncate operations
- Implementations:
  - `pkg/content/store/memory/`: In-memory (fast, ephemeral)
  - `pkg/content/store/fs/`: Filesystem-backed storage
  - `pkg/content/store/s3/`: S3-backed storage (multipart, streaming)

## Adapter Pattern

DittoFS uses the Adapter pattern to provide clean protocol abstractions:

```go
// Adapter interface - each protocol implements this
type Adapter interface {
    Serve(ctx context.Context) error
    Stop(ctx context.Context) error
    Protocol() string
    Port() int
    SetRegistry(registry *Registry)  // Receive registry for service access
}

// Example: NFS Adapter accesses services via registry
type NFSAdapter struct {
    config   NFSConfig
    registry *Registry  // Access to MetadataService and ContentService
}

func (a *NFSAdapter) handleRead(ctx context.Context, req *ReadRequest) {
    // Use ContentService for reads (handles caching automatically)
    data, err := a.registry.ContentService().ReadAt(ctx, shareName, contentID, offset, size)
    // ...
}

// Multiple adapters can run concurrently, sharing the same services
server := dittofs.NewServer(config)
server.AddAdapter(nfs.New(nfsConfig))
server.AddAdapter(smb.New(smbConfig))
server.Serve(ctx)
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

## Service Layer

The service layer provides business logic and coordination between stores and caches.

### MetadataService

Handles all metadata operations with share-based routing:

```go
// MetadataService - central service for metadata operations
type MetadataService struct {
    stores       map[string]MetadataStore  // shareName -> store
    lockManagers map[string]*LockManager   // shareName -> lock manager
}

// Usage by protocol handlers
metaSvc := metadata.New()
metaSvc.RegisterStoreForShare("/export", memoryStore)
metaSvc.RegisterStoreForShare("/archive", badgerStore)

// High-level operations (with business logic)
file, err := metaSvc.CreateFile(authCtx, parentHandle, "test.txt", fileAttr)
entries, err := metaSvc.ReadDir(ctx, dirHandle)

// Byte-range locking (SMB/NLM)
lock, err := metaSvc.AcquireLock(ctx, shareName, handle, offset, length, exclusive)
```

### ContentService

Handles all content operations with caching:

```go
// ContentService - central service for content operations
type ContentService struct {
    stores map[string]ContentStore  // shareName -> store
    caches map[string]cache.Cache   // shareName -> cache (optional)
}

// Usage by protocol handlers
contentSvc := content.New()
contentSvc.RegisterStoreForShare("/export", memoryStore)
contentSvc.RegisterCacheForShare("/export", memoryCache)

// High-level operations (cache-aware)
data, err := contentSvc.ReadAt(ctx, shareName, contentID, offset, size)  // Checks cache first
err := contentSvc.WriteAt(ctx, shareName, contentID, data, offset)       // Writes to cache
err := contentSvc.Flush(ctx, shareName, contentID)                       // Flushes cache to store
```

### Store Interfaces (CRUD)

Stores are now simple CRUD interfaces, with business logic in services:

```go
// MetadataStore - simple CRUD for metadata
type MetadataStore interface {
    GetFile(ctx context.Context, handle FileHandle) (*FileAttr, error)
    CreateFile(ctx context.Context, parent FileHandle, name string, attr *FileAttr) (*FileAttr, error)
    DeleteFile(ctx context.Context, handle FileHandle) error
    UpdateFile(ctx context.Context, handle FileHandle, attr *FileAttr) error
    ListDir(ctx context.Context, handle FileHandle) ([]*DirEntry, error)
}

// ContentStore - simple CRUD for content
type ContentStore interface {
    ReadAt(ctx context.Context, id ContentID, offset int64, size int64) ([]byte, error)
    WriteAt(ctx context.Context, id ContentID, data []byte, offset int64) error
    Delete(ctx context.Context, id ContentID) error
    Truncate(ctx context.Context, id ContentID, size int64) error
    Stats(ctx context.Context, id ContentID) (*ContentStats, error)
}
```

## Built-In and Custom Backends

### Using Built-In Backends

No custom code required - configure via YAML:

```yaml
# config.yaml
metadata:
  stores:
    default-meta:
      type: memory  # or badger, postgres

content:
  stores:
    default-content:
      type: memory  # or fs, s3

shares:
  - name: /export
    metadata_store: default-meta
    content_store: default-content
```

Or programmatically:

```go
// Create stores
metadataStore := memory.NewMemoryMetadataStoreWithDefaults()
contentStore := fscontent.New("/data/content")

// Create services
metaSvc := metadata.New()
metaSvc.RegisterStoreForShare("/export", metadataStore)

contentSvc := content.New()
contentSvc.RegisterStoreForShare("/export", contentStore)

// Create registry and wire services
registry := registry.New()
registry.SetMetadataService(metaSvc)
registry.SetContentService(contentSvc)

// Start server
server := server.New(registry)
server.Serve(ctx)
```

### Implementing Custom Store Backends

Stores are simple CRUD interfaces - implement only what's needed:

```go
// 1. Implement metadata store (simple CRUD)
type PostgresStore struct {
    db *sql.DB
}

func (s *PostgresStore) GetFile(ctx context.Context, handle FileHandle) (*metadata.FileAttr, error) {
    var attr metadata.FileAttr
    err := s.db.QueryRowContext(ctx,
        "SELECT size, mtime, mode FROM files WHERE handle = $1",
        handle,
    ).Scan(&attr.Size, &attr.MTime, &attr.Mode)
    return &attr, err
}

func (s *PostgresStore) CreateFile(ctx context.Context, parent FileHandle, name string, attr *metadata.FileAttr) (*metadata.FileAttr, error) {
    // Simple INSERT - no business logic needed
}

// 2. Implement content store (simple CRUD)
type S3Store struct {
    client *s3.Client
    bucket string
}

func (s *S3Store) ReadAt(ctx context.Context, id content.ContentID, offset, size int64) ([]byte, error) {
    result, err := s.client.GetObject(ctx, &s3.GetObjectInput{
        Bucket: aws.String(s.bucket),
        Key:    aws.String(string(id)),
        Range:  aws.String(fmt.Sprintf("bytes=%d-%d", offset, offset+size-1)),
    })
    if err != nil {
        return nil, err
    }
    defer result.Body.Close()
    return io.ReadAll(result.Body)
}

// 3. Register with services (business logic is in services, not stores)
metaSvc.RegisterStoreForShare("/archive", postgresStore)
contentSvc.RegisterStoreForShare("/archive", s3Store)
```

## Directory Structure

```
dittofs/
├── cmd/dittofs/              # Main application entry point
│   └── main.go               # Server startup, config parsing, init
│
├── pkg/                      # Public API (stable interfaces)
│   ├── adapter/              # Protocol adapter interface
│   │   ├── adapter.go        # Core Adapter interface
│   │   ├── nfs/              # NFS adapter implementation
│   │   └── smb/              # SMB adapter implementation
│   │
│   ├── metadata/             # Metadata layer
│   │   ├── service.go        # MetadataService (business logic, routing)
│   │   ├── store.go          # MetadataStore interface (CRUD)
│   │   ├── types.go          # FileAttr, DirEntry, etc.
│   │   ├── errors.go         # Metadata-specific errors
│   │   ├── locking.go        # LockManager for byte-range locks
│   │   └── store/            # Store implementations
│   │       ├── memory/       # In-memory (ephemeral)
│   │       ├── badger/       # BadgerDB (persistent)
│   │       └── postgres/     # PostgreSQL (distributed)
│   │
│   ├── content/              # Content layer
│   │   ├── service.go        # ContentService (caching, routing)
│   │   ├── store.go          # ContentStore interface (CRUD)
│   │   ├── types.go          # ContentID, ContentStats, etc.
│   │   ├── errors.go         # Content-specific errors
│   │   └── store/            # Store implementations
│   │       ├── memory/       # In-memory (ephemeral)
│   │       ├── fs/           # Filesystem-backed
│   │       └── s3/           # S3-backed (multipart)
│   │
│   ├── cache/                # Cache layer
│   │   ├── cache.go          # Cache interface
│   │   └── memory/           # In-memory cache implementation
│   │
│   ├── registry/             # Store registry
│   │   ├── registry.go       # Central registry (owns services)
│   │   ├── share.go          # Share configuration
│   │   └── access.go         # Identity mapping
│   │
│   ├── config/               # Configuration parsing
│   │   ├── config.go         # Main config struct
│   │   └── registry.go       # Registry initialization
│   │
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

## Horizontal Scaling with PostgreSQL

The PostgreSQL metadata store enables horizontal scaling for high-availability and high-throughput deployments:

### Architecture

```
┌─────────────┐  ┌─────────────┐  ┌─────────────┐
│  DittoFS #1 │  │  DittoFS #2 │  │  DittoFS #3 │
│  (Pod 1)    │  │  (Pod 2)    │  │  (Pod 3)    │
└──────┬──────┘  └──────┬──────┘  └──────┬──────┘
       │                │                │
       └────────────────┼────────────────┘
                        │
                   ┌────▼─────┐
                   │PostgreSQL│
                   │ Cluster  │
                   └──────────┘
```

### Key Features

1. **Multiple DittoFS Instances**: Run multiple instances sharing one PostgreSQL database
2. **Load Balancing**: Use Kubernetes services or external load balancers to distribute requests
3. **No Session Affinity Required**: Any instance can serve any request (stateless design)
4. **Independent Connection Pools**: Each instance maintains its own connection pool (10-15 conns typical)
5. **Statistics Caching**: 5-second TTL cache reduces database load
6. **ACID Transactions**: Ensures consistency across concurrent operations

### Deployment Example (Kubernetes)

```yaml
apiVersion: apps/v1
kind: Deployment
metadata:
  name: dittofs
spec:
  replicas: 3  # Multiple instances for HA
  selector:
    matchLabels:
      app: dittofs
  template:
    metadata:
      labels:
        app: dittofs
    spec:
      containers:
      - name: dittofs
        image: dittofs:latest
        ports:
        - containerPort: 12049
          name: nfs
        env:
        - name: DITTOFS_METADATA_POSTGRES_HOST
          value: postgres-service
        - name: DITTOFS_METADATA_POSTGRES_PASSWORD
          valueFrom:
            secretKeyRef:
              name: postgres-secret
              key: password
        resources:
          requests:
            memory: "256Mi"
            cpu: "250m"
          limits:
            memory: "512Mi"
            cpu: "500m"
---
apiVersion: v1
kind: Service
metadata:
  name: dittofs-nfs
spec:
  selector:
    app: dittofs
  ports:
  - port: 2049
    targetPort: 12049
    protocol: TCP
  type: LoadBalancer
```

### Connection Pool Sizing

Connection pool sizing depends on your workload:

- **Light workload** (< 10 concurrent clients): `max_conns: 10`
- **Medium workload** (10-50 concurrent clients): `max_conns: 15`
- **Heavy workload** (50+ concurrent clients): `max_conns: 20-25`

**Formula**: `max_conns ≈ 2 × expected_concurrent_operations`

**PostgreSQL Limits**: Ensure PostgreSQL `max_connections` > `(DittoFS instances × max_conns)`

Example: 3 DittoFS instances × 15 conns = 45 total connections needed from PostgreSQL

### Performance Considerations

- **Network Latency**: PostgreSQL adds ~1-2ms latency per metadata operation
- **Statistics Caching**: Reduces expensive queries (disk usage, file counts)
- **Query Optimization**: All queries use indexed fields for fast lookups
- **Transaction Overhead**: Short-lived transactions minimize lock contention

### Best Practices

1. **Use Connection Pooling**: Keep `max_conns` reasonable (10-20 per instance)
2. **Enable TLS**: Use `sslmode: require` or higher in production
3. **Monitor Connections**: Watch PostgreSQL connection count and utilization
4. **Scale Horizontally**: Add DittoFS replicas, not connection pool size
5. **Separate Read Replicas**: For read-heavy workloads, consider PostgreSQL read replicas

## Performance Characteristics

DittoFS is designed for high performance through several architectural choices:

- **Direct protocol implementation**: No FUSE overhead
- **Goroutine-per-connection model**: Leverages Go's lightweight concurrency
- **Buffer pooling**: Reduces GC pressure for large I/O operations
- **Streaming I/O**: Efficient handling of large files without full buffering
- **Pluggable caching**: Implement custom caching strategies per use case
- **Zero-copy aspirations**: Working toward minimal data copying in hot paths

## Why Pure Go?

Go provides significant advantages for a project like DittoFS:

- ✅ **Easy deployment**: Single static binary, no runtime dependencies
- ✅ **Cross-platform**: Native support for Linux, macOS, Windows
- ✅ **Easy integration**: Embed DittoFS directly into existing Go applications
- ✅ **Modern concurrency**: Goroutines and channels for natural async I/O
- ✅ **Memory safety**: No buffer overflows or use-after-free vulnerabilities
- ✅ **Strong ecosystem**: Rich standard library and third-party packages
- ✅ **Fast compilation**: Quick iteration during development
- ✅ **Built-in tooling**: Testing, profiling, and race detection included
