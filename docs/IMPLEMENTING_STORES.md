# Implementing Custom Stores

This guide provides comprehensive instructions for implementing custom metadata stores, local block stores, and remote block stores for DittoFS. Whether you're building a database-backed metadata store or a custom cloud storage integration, this document will walk you through the process with best practices and practical examples.

## Table of Contents

1. [Overview](#overview)
2. [When to Implement Custom Stores](#when-to-implement-custom-stores)
3. [Understanding the Architecture](#understanding-the-architecture)
4. [Implementing Metadata Stores](#implementing-metadata-stores)
5. [Implementing a Local Store](#implementing-a-local-store)
6. [Implementing a Remote Store](#implementing-a-remote-store)
7. [Best Practices](#best-practices)
8. [Testing Your Implementation](#testing-your-implementation)
9. [Common Pitfalls](#common-pitfalls)
10. [Integration with DittoFS](#integration-with-dittofs)

## Overview

DittoFS uses a **two-tier block store architecture** with three distinct store types:

- **Metadata Stores**: Simple CRUD operations for file/directory structure, attributes, permissions
- **Local Block Stores**: Fast, per-share storage on local disk or in memory (L2 cache tier)
- **Remote Block Stores**: Durable storage in S3 or compatible object stores (L3 tier, shared across shares via ref counting)

**Key Design Principle**: Each share gets its own `*engine.BlockStore` instance that composes a local store, optional remote store, and syncer. The engine orchestrates reads and writes across the tiers. Local stores provide fast access; remote stores provide durability.

This separation enables:
- Independent scaling of metadata and block storage
- Per-share isolation with shared remote backends
- Different storage tiers (hot/cold storage, SSD/HDD)
- Simple store implementations (just implement the interface, the engine handles coordination)

## When to Implement Custom Stores

### Metadata Store Use Cases

Implement a custom metadata store when you need:

- **Database-backed storage**: PostgreSQL, MySQL, MongoDB, Cassandra
- **Distributed metadata**: Multi-node coordination, consensus protocols
- **Advanced features**: Full-text search, custom indexing, complex queries
- **Compliance**: Audit logs, versioning, immutability guarantees

**Example**: A PostgreSQL-backed metadata store for enterprise environments requiring audit trails and high availability.

### Local Store Use Cases

Implement a custom local store when you need:

- **Specialized local storage**: NVMe-optimized, hardware-accelerated compression
- **Custom eviction**: Access-pattern-aware eviction beyond simple LRU
- **Encryption at rest**: Hardware-accelerated encryption for local blocks

**Reference implementation**: `pkg/blockstore/local/fs/` (filesystem-backed local store)

### Remote Store Use Cases

Implement a custom remote store when you need:

- **Cloud storage integration**: Azure Blob, Google Cloud Storage, custom object stores
- **Specialized storage**: Tape archives, HSM systems, data lakes
- **Tiering**: Automatic hot/cold data movement based on access patterns

**Reference implementation**: `pkg/blockstore/remote/s3/` (S3-backed remote store)

## Understanding the Architecture

### Per-Share BlockStore

Each share gets its own `*engine.BlockStore` instance:

```
┌─────────────────────────────────────┐
│  engine.BlockStore (per-share)      │
│                                     │
│  ┌─────────────┐  ┌─────────────┐  │
│  │ LocalStore  │  │ RemoteStore │  │
│  │ (required)  │  │ (optional)  │  │
│  └──────┬──────┘  └──────┬──────┘  │
│         │                │          │
│         └───────┬────────┘          │
│                 │                   │
│          ┌──────▼──────┐            │
│          │   Syncer    │            │
│          │ (async xfer)│            │
│          └─────────────┘            │
└─────────────────────────────────────┘
```

- **LocalStore** is required -- all reads and writes go through local storage first
- **RemoteStore** is optional -- when configured, the Syncer asynchronously uploads local blocks to remote storage
- **Ref counting**: Remote stores are shared across shares; when the last share using a remote store is removed, the connection is closed

### File Handle and Block Resolution

Protocol handlers resolve the per-share block store via `GetBlockStoreForHandle(ctx, handle)`:

1. File handle encodes the share name
2. Runtime extracts share name and returns the share's BlockStore
3. Handler calls `ReadAt` / `WriteAt` on the BlockStore

## Implementing Metadata Stores

The metadata store interface and implementation guide remains the same as before. See the `pkg/metadata/Store` interface and reference implementations:

- `pkg/metadata/store/memory/`: In-memory (fast, ephemeral)
- `pkg/metadata/store/badger/`: BadgerDB (persistent, embedded)
- `pkg/metadata/store/postgres/`: PostgreSQL (persistent, distributed)

Conformance tests: `pkg/metadata/storetest/`

## Implementing a Local Store

Local stores provide fast, per-share block storage. Each share gets an isolated local storage directory.

### The LocalStore Interface

The `pkg/blockstore/local.LocalStore` interface defines the contract:

```go
type LocalStore interface {
    // ReadAt reads block data at the given offset
    ReadAt(ctx context.Context, blockID string, p []byte, offset int64) (int, error)

    // WriteAt writes block data at the given offset
    WriteAt(ctx context.Context, blockID string, data []byte, offset int64) (int, error)

    // Delete removes a block from local storage
    Delete(ctx context.Context, blockID string) error

    // Exists checks if a block exists locally
    Exists(ctx context.Context, blockID string) (bool, error)

    // Flush ensures all pending writes are persisted
    Flush(ctx context.Context, blockID string) error

    // List returns all block IDs in local storage
    List(ctx context.Context) ([]string, error)

    // Close releases resources
    Close() error
}
```

### Implementation Pattern

```go
package mylocal

import (
    "context"
)

type MyLocalStore struct {
    basePath string
    // Your backend (filesystem, NVMe, etc.)
}

func New(basePath string) (*MyLocalStore, error) {
    // Initialize your storage backend
    return &MyLocalStore{basePath: basePath}, nil
}

func (s *MyLocalStore) ReadAt(ctx context.Context, blockID string, p []byte, offset int64) (int, error) {
    // Read block data from your backend
    // Return bytes read and any error
}

func (s *MyLocalStore) WriteAt(ctx context.Context, blockID string, data []byte, offset int64) (int, error) {
    // Write block data to your backend
    // Return bytes written and any error
}

// ... implement remaining interface methods
```

### Reference Implementation

See `pkg/blockstore/local/fs/` for a complete filesystem-backed local store implementation that handles:
- Per-share isolated directories
- Atomic writes
- Block listing for sync operations

### Conformance Tests

Test your local store with the conformance suite:

```go
package mylocal_test

import (
    "testing"
    "github.com/marmos91/dittofs/pkg/blockstore/local/localtest"
)

func TestMyLocalStore(t *testing.T) {
    store, cleanup := createTestStore(t)
    defer cleanup()
    localtest.RunLocalStoreTests(t, store)
}
```

## Implementing a Remote Store

Remote stores provide durable block storage shared across shares via ref counting.

### The RemoteStore Interface

The `pkg/blockstore/remote.RemoteStore` interface defines the contract:

```go
type RemoteStore interface {
    // ReadBlock reads an entire block from remote storage
    ReadBlock(ctx context.Context, blockID string) ([]byte, error)

    // WriteBlock writes an entire block to remote storage
    WriteBlock(ctx context.Context, blockID string, data []byte) error

    // DeleteBlock removes a block from remote storage
    DeleteBlock(ctx context.Context, blockID string) error

    // HealthCheck verifies the remote store is accessible
    HealthCheck(ctx context.Context) error

    // Close releases resources
    Close() error
}
```

### Implementation Pattern

```go
package myremote

import (
    "context"
)

type MyRemoteStore struct {
    client *MyCloudClient
    bucket string
}

func New(config Config) (*MyRemoteStore, error) {
    client, err := connectToCloud(config)
    if err != nil {
        return nil, err
    }
    return &MyRemoteStore{client: client, bucket: config.Bucket}, nil
}

func (s *MyRemoteStore) ReadBlock(ctx context.Context, blockID string) ([]byte, error) {
    // Fetch block from cloud storage
    return s.client.GetObject(ctx, s.bucket, blockID)
}

func (s *MyRemoteStore) WriteBlock(ctx context.Context, blockID string, data []byte) error {
    // Upload block to cloud storage
    return s.client.PutObject(ctx, s.bucket, blockID, data)
}

func (s *MyRemoteStore) DeleteBlock(ctx context.Context, blockID string) error {
    // Remove block from cloud storage (idempotent)
    err := s.client.DeleteObject(ctx, s.bucket, blockID)
    if err != nil && !isNotFoundError(err) {
        return err
    }
    return nil
}

func (s *MyRemoteStore) HealthCheck(ctx context.Context) error {
    // Verify connectivity (e.g., HEAD bucket)
    return s.client.HeadBucket(ctx, s.bucket)
}

func (s *MyRemoteStore) Close() error {
    return s.client.Close()
}
```

### Ref Counting

Remote stores are shared across shares via ref counting:
- When a share is created referencing a remote store, the ref count increments
- When a share is removed, the ref count decrements
- When the ref count reaches zero, `Close()` is called

This means your `Close()` implementation should release all resources (connections, goroutines, etc.).

### Reference Implementation

See `pkg/blockstore/remote/s3/` for a production S3 remote store implementation with:
- Configurable retry with exponential backoff
- Health check via HEAD bucket
- Efficient multipart uploads for large blocks

### Conformance Tests

Test your remote store with the conformance suite:

```go
package myremote_test

import (
    "testing"
    "github.com/marmos91/dittofs/pkg/blockstore/remote/remotetest"
)

func TestMyRemoteStore(t *testing.T) {
    store, cleanup := createTestStore(t)
    defer cleanup()
    remotetest.RunRemoteStoreTests(t, store)
}
```

## Best Practices

### Thread Safety

All store implementations must be thread-safe. Multiple goroutines will access the store concurrently.

### Context Handling

Always respect context cancellation, especially for remote stores where network calls can be slow:

```go
func (s *MyStore) ReadBlock(ctx context.Context, blockID string) ([]byte, error) {
    if err := ctx.Err(); err != nil {
        return nil, err
    }
    // Proceed with operation
}
```

### Error Handling

- Local store errors should be wrapped with meaningful context
- Remote store errors should distinguish transient (retry-able) from permanent failures
- Delete operations should be idempotent (deleting a non-existent block is not an error)

### Performance

- **Local stores**: Minimize syscalls, use buffered I/O, consider memory-mapped files
- **Remote stores**: Use connection pooling, implement retry with backoff, batch operations where possible

## Testing Your Implementation

1. **Conformance tests**: Run the provided test suites (`localtest`/`remotetest`)
2. **Concurrency tests**: Verify thread safety with parallel reads/writes
3. **Error handling tests**: Test behavior with canceled contexts, network failures
4. **Integration tests**: Test with the full DittoFS stack (create share, mount, read/write)

## Common Pitfalls

1. **Not making Delete idempotent**: Deleting a non-existent block should succeed
2. **Ignoring context cancellation**: Long operations should check `ctx.Err()` periodically
3. **Unsafe concurrent access**: Use proper synchronization for shared state
4. **Resource leaks**: Ensure `Close()` releases all resources (connections, goroutines, file handles)

## Integration with DittoFS

### Register Your Store

Add your store type to the configuration system:

```go
// pkg/config/stores.go
func createLocalStore(config LocalStoreConfig) (local.LocalStore, error) {
    switch config.Type {
    case "fs":
        return fs.New(config.Path)
    case "memory":
        return memory.New()
    case "mylocal":
        return mylocal.New(config.Path)
    default:
        return nil, fmt.Errorf("unknown local store type: %s", config.Type)
    }
}
```

### CLI Integration

Users can then create your store via CLI:

```bash
./dfsctl store block add --kind local --name my-store --type mylocal \
  --config '{"path":"/data/blocks"}'
```

## Additional Resources

- **Interface Definitions**: `pkg/blockstore/local/local.go`, `pkg/blockstore/remote/remote.go`
- **Reference Implementations**:
  - Local: `pkg/blockstore/local/fs/`, `pkg/blockstore/local/memory/`
  - Remote: `pkg/blockstore/remote/s3/`, `pkg/blockstore/remote/memory/`
  - Metadata: `pkg/metadata/store/memory/`, `pkg/metadata/store/badger/`, `pkg/metadata/store/postgres/`
- **Conformance Tests**: `pkg/blockstore/local/localtest/`, `pkg/blockstore/remote/remotetest/`, `pkg/metadata/storetest/`
- **Architecture**: `docs/ARCHITECTURE.md`
- **Configuration**: `docs/CONFIGURATION.md`
- **Contributing**: `docs/CONTRIBUTING.md`
