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

## Adapter Pattern

DittoFS uses the Adapter pattern to provide clean protocol abstractions:

```go
// Adapter interface - each protocol implements this
type Adapter interface {
    Serve(ctx context.Context) error
    Stop(ctx context.Context) error
    Protocol() string
    Port() int
}

// Example: NFS Adapter
type NFSAdapter struct {
    config         NFSConfig
    metadataStore  metadata.MetadataStore
    content        content.ContentStore
}

// Multiple adapters can run concurrently
server := dittofs.New(metadataRepo, contentRepo)
server.AddAdapter(nfs.New(nfsConfig))
server.AddAdapter(smb.New(smbConfig)) // Future
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

## Repository Interfaces

### Metadata Repository

Handles file structure:

```go
type Repository interface {
    // File operations
    GetFile(ctx context.Context, handle FileHandle) (*FileAttr, error)
    CreateFile(ctx context.Context, parent FileHandle, name string) (*FileAttr, error)

    // Directory operations
    Lookup(ctx context.Context, dir FileHandle, name string) (*FileAttr, error)
    ReadDir(ctx context.Context, dir FileHandle) ([]*DirEntry, error)

    // Attribute operations
    SetAttr(ctx context.Context, handle FileHandle, attr *SetAttr) error
}
```

### Content Repository

Handles file data:

```go
type Repository interface {
    ReadContent(ctx context.Context, id ContentID, offset int64, size uint32) ([]byte, error)
    WriteContent(ctx context.Context, id ContentID, offset int64, data []byte) error
    GetSize(ctx context.Context, id ContentID) (int64, error)
}
```

## Built-In and Custom Backends

### Using Built-In Backends

No custom code required:

```go
// Memory backend (ephemeral)
metadataStore := memory.NewMemoryMetadataStoreWithDefaults()

// BadgerDB backend (persistent)
metadataStore, err := badger.NewBadgerMetadataStoreWithDefaults(ctx, "/var/lib/dittofs/metadata.db")
if err != nil {
    log.Fatal(err)
}

// Wire to server
server := dittofs.New(metadataStore, contentStore)
```

### Implementing Custom Backends

```go
// 1. Implement metadata backend (e.g., PostgreSQL)
type PostgresRepository struct {
    db *sql.DB
}

func (r *PostgresRepository) GetFile(ctx context.Context, handle FileHandle) (*FileAttr, error) {
    var attr FileAttr
    err := r.db.QueryRowContext(ctx,
        "SELECT size, mtime, mode FROM files WHERE handle = $1",
        handle,
    ).Scan(&attr.Size, &attr.MTime, &attr.Mode)
    return &attr, err
}

// 2. Implement content backend (e.g., S3)
type S3ContentRepository struct {
    client *s3.Client
    bucket string
}

func (r *S3ContentRepository) ReadContent(ctx context.Context, id ContentID, offset int64, size uint32) ([]byte, error) {
    result, err := r.client.GetObject(ctx, &s3.GetObjectInput{
        Bucket: aws.String(r.bucket),
        Key:    aws.String(id.String()),
        Range:  aws.String(fmt.Sprintf("bytes=%d-%d", offset, offset+int64(size)-1)),
    })
    if err != nil {
        return nil, err
    }
    defer result.Body.Close()
    return io.ReadAll(result.Body)
}

// 3. Wire everything together
func main() {
    // Initialize repositories
    metadataRepo := NewPostgresRepository(dbConn)
    contentRepo := NewS3Repository(s3Client, "my-bucket")

    // Create and configure adapter
    nfsAdapter := nfs.New(nfs.NFSConfig{Port: 12049})

    // Start server
    server := dittofs.New(metadataRepo, contentRepo)
    server.AddAdapter(nfsAdapter)
    server.Serve(ctx)
}
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
