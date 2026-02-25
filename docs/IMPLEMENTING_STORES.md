# Implementing Custom Stores

This guide provides comprehensive instructions for implementing custom metadata and payload stores for DittoFS. Whether you're building a database-backed metadata store or a custom cloud storage integration, this document will walk you through the process with best practices and practical examples.

## Table of Contents

1. [Overview](#overview)
2. [When to Implement Custom Stores](#when-to-implement-custom-stores)
3. [Understanding the Architecture](#understanding-the-architecture)
4. [Implementing Metadata Stores](#implementing-metadata-stores)
5. [Implementing Payload Stores](#implementing-payload-stores)
6. [Best Practices](#best-practices)
7. [Testing Your Implementation](#testing-your-implementation)
8. [Common Pitfalls](#common-pitfalls)
9. [Integration with DittoFS](#integration-with-dittofs)

## Overview

DittoFS uses a **Service-oriented architecture** with three distinct layers:

- **Services** (`MetadataService`, `PayloadService`): Business logic, coordination, caching
- **Metadata Stores**: Simple CRUD operations for file/directory structure, attributes, permissions
- **Payload Stores**: Simple CRUD operations for actual file data (bytes)

**Key Design Principle**: Stores are simple CRUD interfaces. Business logic (permission checking, cache coordination, locking) lives in the Service layer. This makes implementing custom stores much simpler.

This separation enables:
- Independent scaling of metadata and payload
- Payload deduplication (multiple files sharing same data)
- Flexible storage backends (databases, object storage, distributed systems)
- Different storage tiers (hot/cold storage, SSD/HDD)
- **Simple store implementations** (just implement CRUD, services handle the rest)

## When to Implement Custom Stores

### Metadata Store Use Cases

Implement a custom metadata store when you need:

- **Database-backed storage**: PostgreSQL, MySQL, MongoDB, Cassandra
- **Distributed metadata**: Multi-node coordination, consensus protocols
- **Advanced features**: Full-text search, custom indexing, complex queries
- **Compliance**: Audit logs, versioning, immutability guarantees
- **Integration**: Existing identity management, ACL systems

**Example**: A PostgreSQL-backed metadata store for enterprise environments requiring audit trails, complex permission queries, and high availability.

### Payload Store Use Cases

Implement a custom payload store when you need:

- **Cloud storage integration**: Azure Blob, Google Cloud Storage, custom object stores
- **Specialized storage**: Tape archives, HSM systems, data lakes
- **Compression/encryption**: Custom algorithms, hardware acceleration
- **Tiering**: Automatic hot/cold data movement based on access patterns
- **Deduplication**: Content-addressable storage, block-level dedup

**Example**: An Azure Blob Storage payload store with automatic tiering to cold storage for infrequently accessed files.

## Understanding the Architecture

### File Handle Design

DittoFS uses **path-based file handles** for consistency and recoverability:

```
Format: "shareName:uuid"
Example: "/export:550e8400-e29b-41d4-a716-446655440000"
```

**Why Path-Based?**
- **Deterministic**: Same file always has same handle
- **Recoverable**: Metadata can be reconstructed from payload stores
- **Human-readable**: Easy to debug and inspect
- **NFS-compatible**: Under 64-byte limit for most share names

**Implementation Note**: Use `metadata.EncodeShareHandle()` and `metadata.DecodeFileHandle()` for consistent handle encoding/decoding.

### Payload ID Design

Payload IDs link metadata to actual file data:

```
Format: "shareName/path/to/file"
Example: "export/documents/report.pdf"
```

**Why Path-Based Payload IDs?**
- **Inspectable**: Browse S3 buckets like a filesystem
- **Recoverable**: Reconstruct metadata from payload store listing
- **Migration-friendly**: Import existing filesystem structures

Use `internal.BuildPayloadID(shareName, fullPath)` for consistent generation.

### Store Coordination

Services coordinate between metadata and payload stores. **Protocol handlers interact with services, not stores directly.**

```
Protocol Handler → Service → Store
                     ↓
                   Cache
```

**Write Flow** (handled by `PayloadService`):
1. Protocol handler calls `PayloadService.WriteAt(shareName, payloadID, data, offset)`
2. PayloadService checks if cache is configured for this share
3. If cached: writes to cache, marks dirty for later flush
4. If not cached: writes directly to store

**Read Flow** (handled by `PayloadService`):
1. Protocol handler calls `PayloadService.ReadAt(shareName, payloadID, offset, size)`
2. PayloadService checks cache first (if configured)
3. On cache hit: returns cached data
4. On cache miss: reads from store, optionally caches result

**Metadata Flow** (handled by `MetadataService`):
1. Protocol handler calls `MetadataService.CreateFile(authCtx, parentHandle, name, attr)`
2. MetadataService routes to correct store based on share name
3. Store performs simple CRUD operation (no business logic)

**Key Point**: As a store implementer, you only need to implement CRUD operations. The services handle caching, routing, locking, and coordination.

## Implementing Metadata Stores

### Step 1: Define Your Store Structure

```go
package mystore

import (
    "context"
    "database/sql"
    "sync"

    lru "github.com/hashicorp/golang-lru/v2"
    "github.com/marmos91/dittofs/pkg/metadata"
)

// MyMetadataStore implements metadata.MetadataStore.
type MyMetadataStore struct {
    // Your backend connection (database, API client, etc.)
    db *sql.DB

    // Configuration
    capabilities metadata.FilesystemCapabilities
    serverConfig metadata.MetadataServerConfig

    // Synchronization (if needed)
    mu sync.RWMutex

    // Caches (optional but recommended)
    handleCache *lru.Cache[string, *metadata.File]
    pathCache   *lru.Cache[string, metadata.FileHandle]
}
```

**Design Considerations**:
- **Thread Safety**: Use mutexes or ensure backend is thread-safe
- **Caching**: Cache frequently accessed metadata (handles, parent paths)
- **Connection Pooling**: Reuse database connections efficiently
- **Context Propagation**: Respect context cancellation throughout

### Step 2: Implement Core Operations

#### Handle Management

```go
// GetShareNameForHandle extracts share name from file handle.
//
// Implementation strategy:
// 1. Decode handle using metadata.DecodeFileHandle()
// 2. Extract share name from decoded components
// 3. Validate handle exists in your backend
func (s *MyMetadataStore) GetShareNameForHandle(
    ctx context.Context,
    handle metadata.FileHandle,
) (string, error) {
    // Decode the handle
    shareName, _, err := metadata.DecodeFileHandle(handle)
    if err != nil {
        return "", &metadata.StoreError{
            Code:    metadata.ErrInvalidHandle,
            Message: "invalid handle format",
        }
    }

    // Validate handle exists (optional but recommended)
    exists, err := s.handleExists(ctx, handle)
    if err != nil {
        return "", err
    }
    if !exists {
        return "", &metadata.StoreError{
            Code:    metadata.ErrNotFound,
            Message: "file not found",
        }
    }

    return shareName, nil
}
```

#### Permission Checking

DittoFS provides shared helper functions in `pkg/metadata/` for common permission and identity operations:

```go
// Shared helper functions available:
metadata.CalculatePermissionsFromBits(bits uint32) Permission  // Convert Unix bits to Permission
metadata.CheckOtherPermissions(mode, requested) Permission     // Extract "other" permission bits
metadata.ApplyIdentityMapping(identity, mapping) *Identity     // Apply identity mapping rules
metadata.IsAdministratorSID(sid string) bool                   // Check Windows admin SID
metadata.MatchesIPPattern(clientIP, pattern string) bool       // CIDR and exact IP matching
metadata.GetInitialLinkCount(fileType) uint32                  // Initial link count (1 for files, 2 for dirs)
metadata.CopyFileAttr(attr *FileAttr) *FileAttr                // Deep copy of FileAttr
```

```go
// CheckPermissions performs Unix-style permission checking.
//
// Implementation pattern:
// 1. Retrieve file attributes from backend
// 2. Compare requested permissions against file mode
// 3. Apply ownership rules (owner, group, other)
// 4. Return granted permissions (may be subset of requested)
func (s *MyMetadataStore) CheckPermissions(
    ctx *metadata.AuthContext,
    handle metadata.FileHandle,
    requested metadata.Permission,
) (metadata.Permission, error) {
    // Retrieve file attributes
    file, err := s.GetFile(ctx.Context, handle)
    if err != nil {
        return 0, err
    }

    identity := ctx.Identity

    // Handle anonymous/no identity case
    if identity == nil || identity.UID == nil {
        // Use shared helper for "other" permissions
        return metadata.CheckOtherPermissions(file.Mode, requested), nil
    }

    uid := *identity.UID

    // Root bypass (UID 0)
    if uid == 0 {
        return requested, nil
    }

    // Determine which permission bits apply
    var permBits uint32
    if uid == file.UID {
        // Owner permissions (bits 6-8)
        permBits = (file.Mode >> 6) & 0x7
    } else if identity.GID != nil && (*identity.GID == file.GID || identity.HasGID(file.GID)) {
        // Group permissions (bits 3-5)
        permBits = (file.Mode >> 3) & 0x7
    } else {
        // Other permissions (bits 0-2)
        permBits = file.Mode & 0x7
    }

    // Use shared helper to convert bits to Permission
    granted := metadata.CalculatePermissionsFromBits(permBits)

    // Owner gets additional privileges
    if uid == file.UID {
        granted |= metadata.PermissionChangePermissions | metadata.PermissionChangeOwnership
    }

    return granted & requested, nil
}
```

#### File Creation

```go
// Create creates a new file or directory.
//
// Implementation steps:
// 1. Validate parent exists and is a directory
// 2. Check write permission on parent
// 3. Check if name already exists (return ErrAlreadyExists)
// 4. Generate file handle and PayloadID
// 5. Initialize attributes (timestamps, owner, etc.)
// 6. Persist file metadata atomically
// 7. Update parent directory (mtime, ctime)
func (s *MyMetadataStore) Create(
    ctx *metadata.AuthContext,
    parentHandle metadata.FileHandle,
    name string,
    attr *metadata.FileAttr,
) (*metadata.File, error) {
    // Validate input
    if name == "" || name == "." || name == ".." {
        return nil, &metadata.StoreError{
            Code:    metadata.ErrInvalidArgument,
            Message: "invalid filename",
        }
    }

    // Get and validate parent
    parent, err := s.GetFile(ctx.Context, parentHandle)
    if err != nil {
        return nil, err
    }
    if parent.Type != metadata.FileTypeDirectory {
        return nil, &metadata.StoreError{
            Code:    metadata.ErrNotDirectory,
            Message: "parent is not a directory",
        }
    }

    // Check write permission on parent
    perms, err := s.CheckPermissions(ctx, parentHandle, metadata.PermissionWrite)
    if err != nil {
        return nil, err
    }
    if perms&metadata.PermissionWrite == 0 {
        return nil, &metadata.StoreError{
            Code:    metadata.ErrPermissionDenied,
            Message: "write permission denied on parent",
        }
    }

    // Check if name already exists
    _, err = s.lookupChild(ctx.Context, parentHandle, name)
    if err == nil {
        return nil, &metadata.StoreError{
            Code:    metadata.ErrAlreadyExists,
            Message: "file already exists",
            Path:    name,
        }
    }

    // Generate file handle
    shareName := parent.ShareName
    fullPath := s.buildPath(parent.Path, name)
    newID := uuid.New()
    handle, err := metadata.EncodeShareHandle(shareName, newID)
    if err != nil {
        return nil, err
    }

    // Initialize attributes
    now := time.Now()
    file := &metadata.File{
        ID:        newID,
        ShareName: shareName,
        Path:      fullPath,
        FileAttr: metadata.FileAttr{
            Type:   attr.Type,
            Mode:   attr.Mode,
            UID:    attr.UID,
            GID:    attr.GID,
            Size:   0,
            Atime:  now,
            Mtime:  now,
            Ctime:  now,
        },
    }

    // Generate PayloadID for regular files
    if attr.Type == metadata.FileTypeRegular {
        file.PayloadID = metadata.PayloadID(
            internal.BuildPayloadID(shareName, fullPath),
        )
    }

    // Persist atomically (use transaction if your backend supports it)
    tx, err := s.db.BeginTx(ctx.Context, nil)
    if err != nil {
        return nil, err
    }
    defer tx.Rollback()

    // Insert file metadata
    if err := s.insertFile(tx, handle, file); err != nil {
        return nil, err
    }

    // Link to parent
    if err := s.linkChild(tx, parentHandle, name, handle); err != nil {
        return nil, err
    }

    // Update parent timestamps
    if err := s.updateDirectoryTimes(tx, parentHandle, now, now); err != nil {
        return nil, err
    }

    if err := tx.Commit(); err != nil {
        return nil, err
    }

    return file, nil
}
```

#### Directory Reading with Pagination

```go
// ReadDirectory reads directory entries with pagination support.
//
// Implementation strategy:
// 1. Parse pagination token (offset, cursor, or custom format)
// 2. Query backend for entries starting at token position
// 3. Limit results to fit within maxBytes
// 4. Generate next token if more entries exist
// 5. Use HandleToINode() for inode consistency
func (s *MyMetadataStore) ReadDirectory(
    ctx *metadata.AuthContext,
    dirHandle metadata.FileHandle,
    token string,
    maxBytes uint32,
) (*metadata.ReadDirPage, error) {
    // Validate directory
    dir, err := s.GetFile(ctx.Context, dirHandle)
    if err != nil {
        return nil, err
    }
    if dir.Type != metadata.FileTypeDirectory {
        return nil, &metadata.StoreError{
            Code:    metadata.ErrNotDirectory,
            Message: "not a directory",
        }
    }

    // Check read permission
    perms, err := s.CheckPermissions(ctx, dirHandle, metadata.PermissionRead)
    if err != nil {
        return nil, err
    }
    if perms&metadata.PermissionRead == 0 {
        return nil, &metadata.StoreError{
            Code:    metadata.ErrPermissionDenied,
            Message: "read permission denied",
        }
    }

    // Parse pagination token (simple offset-based example)
    offset := 0
    if token != "" {
        offset, err = strconv.Atoi(token)
        if err != nil {
            return nil, &metadata.StoreError{
                Code:    metadata.ErrInvalidArgument,
                Message: "invalid pagination token",
            }
        }
    }

    // Query children from backend with a reasonable batch size
    // Note: maxBytes is for response size limiting, not entry count
    const batchSize = 100 // Adjust based on typical entry size
    children, hasMore, err := s.queryChildren(ctx.Context, dirHandle, offset, batchSize)
    if err != nil {
        return nil, err
    }

    // Build directory entries
    entries := make([]metadata.DirEntry, 0, len(children))
    for _, child := range children {
        // CRITICAL: Use HandleToINode for inode consistency
        fileID := metadata.HandleToINode(child.Handle)

        entries = append(entries, metadata.DirEntry{
            ID:     fileID,
            Handle: child.Handle,
            Name:   child.Name,
            Attr:   child.Attr,
        })
    }

    // Generate next token if more entries exist
    nextToken := ""
    if hasMore {
        nextToken = strconv.Itoa(offset + len(children))
    }

    return &metadata.ReadDirPage{
        Entries:   entries,
        NextToken: nextToken,
        HasMore:   hasMore,
    }, nil
}
```

### Step 3: Implement Two-Phase Write Protocol

```go
// PrepareWrite validates write and returns intent (Phase 1).
//
// This does NOT modify metadata - only validates and prepares.
func (s *MyMetadataStore) PrepareWrite(
    ctx *metadata.AuthContext,
    handle metadata.FileHandle,
    newSize uint64,
) (*metadata.WriteOperation, error) {
    // Get current file
    file, err := s.GetFile(ctx.Context, handle)
    if err != nil {
        return nil, err
    }

    // Validate file type
    if file.Type != metadata.FileTypeRegular {
        return nil, &metadata.StoreError{
            Code:    metadata.ErrIsDirectory,
            Message: "cannot write to directory",
        }
    }

    // Check write permission
    perms, err := s.CheckPermissions(ctx, handle, metadata.PermissionWrite)
    if err != nil {
        return nil, err
    }
    if perms&metadata.PermissionWrite == 0 {
        return nil, &metadata.StoreError{
            Code:    metadata.ErrPermissionDenied,
            Message: "write permission denied",
        }
    }

    // Create write intent
    now := time.Now()
    intent := &metadata.WriteOperation{
        Handle:    handle,
        PayloadID: file.PayloadID,
        OldSize:   file.Size,
        NewSize:   newSize,
        OldMtime:  file.Mtime,
        NewMtime:  now,
    }

    // Store intent for commit (with expiration)
    s.storePendingWrite(ctx.Context, intent)

    return intent, nil
}

// CommitWrite applies metadata changes after successful payload write (Phase 3).
func (s *MyMetadataStore) CommitWrite(
    ctx *metadata.AuthContext,
    intent *metadata.WriteOperation,
) (*metadata.File, error) {
    // Retrieve and validate intent
    stored, err := s.retrievePendingWrite(ctx.Context, intent.Handle)
    if err != nil {
        return nil, &metadata.StoreError{
            Code:    metadata.ErrStaleHandle,
            Message: "write intent expired or invalid",
        }
    }

    // Update file metadata
    now := time.Now()
    err = s.updateFileAttributes(ctx.Context, intent.Handle, map[string]any{
        "size":  intent.NewSize,
        "mtime": intent.NewMtime,
        "ctime": now,
    })
    if err != nil {
        return nil, err
    }

    // Clean up intent
    s.removePendingWrite(ctx.Context, intent.Handle)

    // Return updated file
    return s.GetFile(ctx.Context, intent.Handle)
}
```

### Step 4: Implement Root Directory Creation

```go
// CreateRootDirectory creates the root directory for a share.
//
// This is called once during share initialization.
func (s *MyMetadataStore) CreateRootDirectory(
    ctx context.Context,
    shareName string,
    attr *metadata.FileAttr,
) (*metadata.File, error) {
    // Validate attributes
    if attr.Type != metadata.FileTypeDirectory {
        return nil, &metadata.StoreError{
            Code:    metadata.ErrInvalidArgument,
            Message: "root must be a directory",
        }
    }

    // Generate root handle
    rootID := uuid.New()
    handle, err := metadata.EncodeShareHandle(shareName, rootID)
    if err != nil {
        return nil, err
    }

    // Check if root already exists
    _, err = s.GetFile(ctx, handle)
    if err == nil {
        return nil, &metadata.StoreError{
            Code:    metadata.ErrAlreadyExists,
            Message: "root directory already exists",
        }
    }

    // Create root directory
    now := time.Now()
    root := &metadata.File{
        ID:        rootID,
        ShareName: shareName,
        Path:      "/",
        FileAttr: metadata.FileAttr{
            Type:  metadata.FileTypeDirectory,
            Mode:  attr.Mode,
            UID:   attr.UID,
            GID:   attr.GID,
            Size:  4096, // Standard directory size
            Atime: now,
            Mtime: now,
            Ctime: now,
        },
    }

    // Persist root (no parent)
    if err := s.insertRoot(ctx, handle, root); err != nil {
        return nil, err
    }

    return root, nil
}
```

### Step 5: Implement Filesystem Statistics

```go
// GetFilesystemCapabilities returns static capabilities.
func (s *MyMetadataStore) GetFilesystemCapabilities(
    ctx context.Context,
    handle metadata.FileHandle,
) (*metadata.FilesystemCapabilities, error) {
    // Validate handle exists
    _, err := s.GetFile(ctx, handle)
    if err != nil {
        return nil, err
    }

    // Return configured capabilities
    return &s.capabilities, nil
}

// GetFilesystemStatistics returns dynamic statistics.
//
// Implementation options:
// 1. Query backend for actual usage (slow but accurate)
// 2. Maintain counters updated on mutations (fast but complex)
// 3. Cache with TTL (balanced approach)
func (s *MyMetadataStore) GetFilesystemStatistics(
    ctx context.Context,
    handle metadata.FileHandle,
) (*metadata.FilesystemStatistics, error) {
    // Validate handle
    _, err := s.GetFile(ctx, handle)
    if err != nil {
        return nil, err
    }

    // Query statistics from backend (example)
    var totalFiles uint64
    var totalSize uint64

    err = s.db.QueryRowContext(ctx,
        "SELECT COUNT(*), COALESCE(SUM(size), 0) FROM files",
    ).Scan(&totalFiles, &totalSize)
    if err != nil {
        return nil, err
    }

    // Calculate available space (example: fixed quota)
    const maxSize = 1099511627776 // 1TB
    availableSize := maxSize - totalSize

    return &metadata.FilesystemStatistics{
        TotalSize:     maxSize,
        UsedSize:      totalSize,
        AvailableSize: availableSize,
        TotalFiles:    totalFiles,
        AvailableFiles: 1000000 - totalFiles,
    }, nil
}
```

## Implementing Payload Stores

### Step 1: Define Your Store Structure

```go
package mystore

import (
    "context"
    "io"
    "sync"

    "github.com/marmos91/dittofs/pkg/payload"
    "github.com/marmos91/dittofs/pkg/metadata"
)

// MyPayloadStore implements payload.PayloadStore.
type MyPayloadStore struct {
    // Your backend (API client, connection pool, etc.)
    client *MyStorageClient

    // Configuration
    bucket    string
    keyPrefix string

    // Synchronization
    mu sync.RWMutex

    // Per-object locks for concurrent writes
    objectLocks   map[metadata.PayloadID]*sync.Mutex
    objectLocksMu sync.Mutex
}
```

### Step 2: Implement Core Read Operations

```go
// ReadPayload returns a reader for the entire payload.
func (s *MyPayloadStore) ReadPayload(
    ctx context.Context,
    id metadata.PayloadID,
) (io.ReadCloser, error) {
    // Check context before operation
    if err := ctx.Err(); err != nil {
        return nil, err
    }

    // Construct object key
    key := s.buildKey(id)

    // Retrieve from backend
    reader, err := s.client.GetObject(ctx, s.bucket, key)
    if err != nil {
        if isNotFoundError(err) {
            return nil, payload.ErrPayloadNotFound
        }
        return nil, err
    }

    return reader, nil
}

// GetPayloadSize returns payload size without reading data.
func (s *MyPayloadStore) GetPayloadSize(
    ctx context.Context,
    id metadata.PayloadID,
) (uint64, error) {
    if err := ctx.Err(); err != nil {
        return 0, err
    }

    key := s.buildKey(id)

    // Use HEAD request or equivalent
    meta, err := s.client.HeadObject(ctx, s.bucket, key)
    if err != nil {
        if isNotFoundError(err) {
            return 0, payload.ErrPayloadNotFound
        }
        return 0, err
    }

    return uint64(meta.ContentLength), nil
}

// PayloadExists checks if payload exists.
func (s *MyPayloadStore) PayloadExists(
    ctx context.Context,
    id metadata.PayloadID,
) (bool, error) {
    if err := ctx.Err(); err != nil {
        return false, err
    }

    key := s.buildKey(id)

    _, err := s.client.HeadObject(ctx, s.bucket, key)
    if err != nil {
        if isNotFoundError(err) {
            return false, nil
        }
        return false, err
    }

    return true, nil
}
```

### Step 3: Implement Write Operations

```go
// WriteAt writes data at specific offset (partial update).
//
// Implementation considerations:
// - Object storage doesn't support true WriteAt
// - Common strategies:
//   1. Read-modify-write (simple but slow)
//   2. Chunked storage (complex but efficient)
//   3. Hybrid: buffer writes, flush on threshold
func (s *MyPayloadStore) WriteAt(
    ctx context.Context,
    id metadata.PayloadID,
    data []byte,
    offset uint64,
) error {
    if err := ctx.Err(); err != nil {
        return err
    }

    // Acquire per-object lock
    lock := s.getObjectLock(id)
    lock.Lock()
    defer lock.Unlock()

    key := s.buildKey(id)

    // Strategy: Read-modify-write (simple example)
    // Production: Use chunked storage or write buffering

    // Read existing payload
    var existing []byte
    reader, err := s.client.GetObject(ctx, s.bucket, key)
    if err != nil && !isNotFoundError(err) {
        return err
    }
    if reader != nil {
        defer reader.Close()
        existing, err = io.ReadAll(reader)
        if err != nil {
            return err
        }
    }

    // Calculate new size
    newSize := offset + uint64(len(data))
    if newSize > uint64(len(existing)) {
        // Extend with zeros
        tmp := make([]byte, newSize)
        copy(tmp, existing)
        existing = tmp
    }

    // Apply write
    copy(existing[offset:], data)

    // Write back
    return s.client.PutObject(ctx, s.bucket, key, existing)
}

// Truncate changes payload size.
func (s *MyPayloadStore) Truncate(
    ctx context.Context,
    id metadata.PayloadID,
    newSize uint64,
) error {
    if err := ctx.Err(); err != nil {
        return err
    }

    lock := s.getObjectLock(id)
    lock.Lock()
    defer lock.Unlock()

    key := s.buildKey(id)

    // Get current size
    currentSize, err := s.GetPayloadSize(ctx, id)
    if err != nil {
        return err
    }

    if newSize == currentSize {
        return nil // No-op
    }

    // Read-modify-write
    reader, err := s.client.GetObject(ctx, s.bucket, key)
    if err != nil {
        return err
    }
    defer reader.Close()

    data, err := io.ReadAll(reader)
    if err != nil {
        return err
    }

    // Truncate or extend
    if newSize < uint64(len(data)) {
        data = data[:newSize]
    } else {
        tmp := make([]byte, newSize)
        copy(tmp, data)
        data = tmp
    }

    return s.client.PutObject(ctx, s.bucket, key, data)
}

// WritePayload writes entire payload in one operation.
func (s *MyPayloadStore) WritePayload(
    ctx context.Context,
    id metadata.PayloadID,
    data []byte,
) error {
    if err := ctx.Err(); err != nil {
        return err
    }

    key := s.buildKey(id)
    return s.client.PutObject(ctx, s.bucket, key, data)
}

// Delete removes payload.
func (s *MyPayloadStore) Delete(
    ctx context.Context,
    id metadata.PayloadID,
) error {
    if err := ctx.Err(); err != nil {
        return err
    }

    key := s.buildKey(id)

    // Idempotent: don't fail if not found
    err := s.client.DeleteObject(ctx, s.bucket, key)
    if err != nil && !isNotFoundError(err) {
        return err
    }

    return nil
}
```

### Step 4: Implement Optional Interfaces

#### ReadAtPayloadStore (Highly Recommended)

```go
// ReadAt reads from specific offset (efficient partial read).
//
// This is CRITICAL for performance with protocols like NFS that
// request small chunks (4-64KB) from large files.
func (s *MyPayloadStore) ReadAt(
    ctx context.Context,
    id metadata.PayloadID,
    p []byte,
    offset uint64,
) (int, error) {
    if err := ctx.Err(); err != nil {
        return 0, err
    }

    key := s.buildKey(id)

    // Use range request (HTTP Range header)
    end := offset + uint64(len(p)) - 1
    rangeSpec := fmt.Sprintf("bytes=%d-%d", offset, end)

    reader, err := s.client.GetObjectRange(ctx, s.bucket, key, rangeSpec)
    if err != nil {
        if isNotFoundError(err) {
            return 0, io.EOF
        }
        return 0, err
    }
    defer reader.Close()

    // Read into buffer
    n, err := io.ReadFull(reader, p)
    if err == io.ErrUnexpectedEOF {
        // Read partial data (end of file)
        return n, io.EOF
    }

    return n, err
}
```

**Performance Impact**: For S3-like stores, ReadAt can be **1000x faster** than reading entire objects:
- Without ReadAt: 100MB object download for 4KB request
- With ReadAt: 4KB range request

#### IncrementalWriteStore (For Large Files)

This is complex and primarily needed for S3-like stores with multipart upload. See `pkg/payload/store/s3/s3_incremental.go` for a complete implementation example.

**Key Concepts**:
- Part-based uploads (5MB+ parts)
- Parallel part uploads
- Finalization combines parts
- Abort cleanup on errors

### Step 5: Implement Storage Statistics

```go
// GetStorageStats returns storage statistics.
//
// For cloud storage, use caching to avoid expensive list operations.
func (s *MyPayloadStore) GetStorageStats(
    ctx context.Context,
) (*payload.StorageStats, error) {
    if err := ctx.Err(); err != nil {
        return nil, err
    }

    // Example: List all objects and sum sizes
    // Production: Cache this with TTL
    var totalSize uint64
    var count uint64

    iter := s.client.ListObjects(ctx, s.bucket, s.keyPrefix)
    for iter.Next() {
        obj := iter.Object()
        totalSize += uint64(obj.Size)
        count++
    }
    if err := iter.Err(); err != nil {
        return nil, err
    }

    avgSize := uint64(0)
    if count > 0 {
        avgSize = totalSize / count
    }

    return &payload.StorageStats{
        TotalSize:     ^uint64(0), // Unlimited
        UsedSize:      totalSize,
        AvailableSize: ^uint64(0), // Unlimited
        PayloadCount:  count,
        AverageSize:   avgSize,
    }, nil
}
```

## Best Practices

### Thread Safety

**Always ensure thread safety**:

```go
// Good: Per-operation locking
func (s *MyStore) WriteAt(ctx context.Context, id PayloadID, data []byte, offset uint64) error {
    lock := s.getObjectLock(id)
    lock.Lock()
    defer lock.Unlock()

    // ... perform write ...
}

// Bad: No synchronization
func (s *MyStore) WriteAt(ctx context.Context, id PayloadID, data []byte, offset uint64) error {
    // Concurrent writes will corrupt data!
    // ... perform write ...
}
```

**Locking Strategies**:
- **Coarse-grained**: Single mutex for entire store (simple, low concurrency)
- **Fine-grained**: Per-object mutexes (complex, high concurrency)
- **Lock-free**: Use backend's atomicity guarantees (best performance)

### Context Handling

**Always respect context cancellation**:

```go
// Good: Check context before expensive operations
func (s *MyStore) ProcessLargeFile(ctx context.Context, id PayloadID) error {
    for i := 0; i < parts; i++ {
        // Check context periodically
        if err := ctx.Err(); err != nil {
            return err
        }

        // Process part...
    }
}

// Bad: Ignore context
func (s *MyStore) ProcessLargeFile(ctx context.Context, id PayloadID) error {
    // Long-running operation with no cancellation checks
    for i := 0; i < parts; i++ {
        // Process part...
    }
}
```

### Error Handling

**Use structured errors**:

```go
// Good: Structured metadata errors
return &metadata.StoreError{
    Code:    metadata.ErrNotFound,
    Message: "file not found",
    Path:    fullPath,
}

// Bad: Generic errors
return fmt.Errorf("file not found: %s", fullPath)
```

**Use error factory functions**:

DittoFS provides error factory functions in `pkg/metadata/errors.go` for consistent error creation:

```go
// Error factory functions available:
metadata.NewNotFoundError(path, "file")         // ErrNotFound
metadata.NewPermissionDeniedError(path)             // ErrPermissionDenied
metadata.NewIsDirectoryError(path)              // ErrIsDirectory
metadata.NewNotDirectoryError(path)             // ErrNotDirectory
metadata.NewInvalidHandleError()                // ErrInvalidHandle
metadata.NewNotEmptyError(path)                 // ErrNotEmpty
metadata.NewAlreadyExistsError(path)            // ErrAlreadyExists
metadata.NewInvalidArgumentError(message)       // ErrInvalidArgument
metadata.NewAccessDeniedError(reason)           // ErrAccessDenied
```

**Map backend errors to store errors**:

```go
func (s *MyStore) handleBackendError(err error, path string) error {
    if isNotFoundError(err) {
        return metadata.NewNotFoundError(path, "file")
    }
    if isPermissionError(err) {
        return metadata.NewAccessDeniedError("access denied")
    }
    // Default: IO error
    return &metadata.StoreError{
        Code:    metadata.ErrIOError,
        Message: err.Error(),
        Path:    path,
    }
}
```

### Caching

**Cache frequently accessed data**:

```go
type MyMetadataStore struct {
    // ... other fields ...

    // Cache hot paths
    handleCache *lru.Cache // FileHandle → *File
    pathCache   *lru.Cache // (shareName, path) → FileHandle
}

func (s *MyMetadataStore) GetFile(ctx context.Context, handle FileHandle) (*File, error) {
    // Check cache first
    key := string(handle)
    if cached, ok := s.handleCache.Get(key); ok {
        return cached.(*File), nil
    }

    // Cache miss: query backend
    file, err := s.queryBackend(ctx, handle)
    if err != nil {
        return nil, err
    }

    // Cache for next time
    s.handleCache.Add(key, file)

    return file, nil
}
```

**Cache invalidation**:

```go
func (s *MyMetadataStore) updateFile(ctx context.Context, handle FileHandle, updates map[string]any) error {
    // Update backend
    if err := s.backend.Update(ctx, handle, updates); err != nil {
        return err
    }

    // Invalidate cache
    s.handleCache.Remove(string(handle))

    return nil
}
```

### Atomicity

**Use transactions for multi-step operations**:

```go
func (s *MyMetadataStore) Move(ctx *AuthContext, fromDir FileHandle, fromName string, toDir FileHandle, toName string) error {
    // Begin transaction
    tx, err := s.db.BeginTx(ctx.Context, nil)
    if err != nil {
        return err
    }
    defer tx.Rollback()

    // Step 1: Get source file handle
    srcHandle, err := s.lookupChild(tx, fromDir, fromName)
    if err != nil {
        return err
    }

    // Step 2: Unlink from source
    if err := s.unlinkChild(tx, fromDir, fromName); err != nil {
        return err
    }

    // Step 3: Link to destination
    if err := s.linkChild(tx, toDir, toName, srcHandle); err != nil {
        return err
    }

    // Step 4: Update timestamps
    now := time.Now()
    if err := s.updateTimes(tx, fromDir, now, now); err != nil {
        return err
    }
    if fromDir != toDir {
        if err := s.updateTimes(tx, toDir, now, now); err != nil {
            return err
        }
    }

    // Commit atomically
    return tx.Commit()
}
```

### Performance

**Minimize backend calls**:

```go
// Good: Batch operations
func (s *MyStore) DeleteMultiple(ctx context.Context, ids []PayloadID) error {
    return s.client.DeleteObjects(ctx, s.bucket, ids)
}

// Bad: Individual deletes
func (s *MyStore) DeleteMultiple(ctx context.Context, ids []PayloadID) error {
    for _, id := range ids {
        if err := s.Delete(ctx, id); err != nil {
            return err
        }
    }
    return nil
}
```

**Use connection pooling**:

```go
func NewMyStore(config Config) (*MyStore, error) {
    // Configure connection pool
    db, err := sql.Open("postgres", config.DSN)
    if err != nil {
        return nil, err
    }

    // Tune pool settings
    db.SetMaxOpenConns(100)
    db.SetMaxIdleConns(10)
    db.SetConnMaxLifetime(time.Hour)

    return &MyStore{db: db}, nil
}
```

## Testing Your Implementation

### Use the Test Suites

DittoFS provides comprehensive test suites:

```go
package mystore_test

import (
    "testing"

    "github.com/marmos91/dittofs/pkg/metadata/testing"
    "github.com/yourorg/dittofs-mystore"
)

func TestMyMetadataStore(t *testing.T) {
    // Create your store
    store, cleanup := createTestStore(t)
    defer cleanup()

    // Run the standard test suite
    testing.RunMetadataStoreTests(t, store)
}

func createTestStore(t *testing.T) (*mystore.MyMetadataStore, func()) {
    // Set up test backend (Docker container, in-memory DB, etc.)
    backend := setupTestBackend(t)

    store := mystore.NewMyMetadataStore(mystore.Config{
        Backend: backend,
        // ... test configuration ...
    })

    cleanup := func() {
        backend.Cleanup()
    }

    return store, cleanup
}
```

### Payload Store Testing

```go
package mystore_test

import (
    "testing"

    "github.com/marmos91/dittofs/pkg/payload/store/testing"
    "github.com/yourorg/dittofs-mystore"
)

func TestMyPayloadStore(t *testing.T) {
    store, cleanup := createTestPayloadStore(t)
    defer cleanup()

    // Run the standard test suite
    testing.RunPayloadStoreTests(t, store)
}
```

### Test Concurrency

```go
func TestConcurrentWrites(t *testing.T) {
    store, cleanup := createTestStore(t)
    defer cleanup()

    payloadID := metadata.PayloadID("test-file")

    // Create file
    store.WritePayload(context.Background(), payloadID, []byte("initial"))

    // Concurrent writes to different offsets
    var wg sync.WaitGroup
    for i := 0; i < 100; i++ {
        wg.Add(1)
        go func(offset int) {
            defer wg.Done()
            data := []byte{byte(offset)}
            err := store.WriteAt(context.Background(), payloadID, data, uint64(offset))
            if err != nil {
                t.Errorf("WriteAt failed: %v", err)
            }
        }(i)
    }
    wg.Wait()

    // Verify no corruption
    reader, err := store.ReadPayload(context.Background(), payloadID)
    if err != nil {
        t.Fatal(err)
    }
    defer reader.Close()

    data, err := io.ReadAll(reader)
    if err != nil {
        t.Fatal(err)
    }

    // Validate data integrity
    for i := 0; i < 100; i++ {
        if data[i] != byte(i) {
            t.Errorf("Data corrupted at offset %d: expected %d, got %d", i, i, data[i])
        }
    }
}
```

### Test Error Conditions

```go
func TestErrorHandling(t *testing.T) {
    store, cleanup := createTestStore(t)
    defer cleanup()

    // Test not found
    _, err := store.GetFile(context.Background(), []byte("nonexistent"))
    if err == nil {
        t.Error("Expected error for nonexistent file")
    }
    storeErr, ok := err.(*metadata.StoreError)
    if !ok || storeErr.Code != metadata.ErrNotFound {
        t.Errorf("Expected ErrNotFound, got %v", err)
    }

    // Test permission denied
    authCtx := &metadata.AuthContext{
        Context: context.Background(),
        UID:     1000,
        GID:     1000,
    }

    // Create file owned by root with mode 0600
    handle := createTestFile(t, store, 0, 0, 0600)

    // Try to read as non-root user (should fail)
    _, err = store.PrepareRead(authCtx, handle)
    if err == nil {
        t.Error("Expected permission denied")
    }
    storeErr, ok = err.(*metadata.StoreError)
    if !ok || storeErr.Code != metadata.ErrPermissionDenied {
        t.Errorf("Expected ErrPermissionDenied, got %v", err)
    }
}
```

## Common Pitfalls

### 1. Not Using HandleToINode for Directory Entries

```go
// WRONG: Custom file ID generation
func (s *MyStore) ReadDirectory(...) (*ReadDirPage, error) {
    // ... query children ...

    for _, child := range children {
        entries = append(entries, DirEntry{
            ID: child.RowID, // ❌ Wrong! Causes circular directories
            // ...
        })
    }
}

// CORRECT: Use HandleToINode
func (s *MyStore) ReadDirectory(...) (*ReadDirPage, error) {
    // ... query children ...

    for _, child := range children {
        entries = append(entries, DirEntry{
            ID: metadata.HandleToINode(child.Handle), // ✅ Correct
            // ...
        })
    }
}
```

**Why**: Custom ID generation can cause inode collisions, leading to circular directory structures in NFS clients.

### 2. Ignoring Context Cancellation

```go
// WRONG: Long operation without context checks
func (s *MyStore) ProcessLargeFile(ctx context.Context, id PayloadID) error {
    for i := 0; i < 1000000; i++ {
        // No context check - operation can't be cancelled
        processChunk(i)
    }
}

// CORRECT: Check context periodically
func (s *MyStore) ProcessLargeFile(ctx context.Context, id PayloadID) error {
    for i := 0; i < 1000000; i++ {
        if i%1000 == 0 { // Check every 1000 iterations
            if err := ctx.Err(); err != nil {
                return err
            }
        }
        processChunk(i)
    }
}
```

### 3. Not Implementing ReadAt for Object Storage

```go
// WRONG: Only implementing ReadPayload
// Client requests 4KB from 100MB file → downloads entire 100MB

// CORRECT: Implement ReadAtPayloadStore interface
func (s *MyStore) ReadAt(ctx context.Context, id PayloadID, p []byte, offset uint64) (int, error) {
    // Use range request to fetch only requested bytes
    return s.client.GetRange(ctx, id, p, offset)
}
```

**Impact**: Without ReadAt, serving large files over NFS is **1000x slower**.

### 4. Unsafe Concurrent Access

```go
// WRONG: No synchronization
type MyStore struct {
    cache map[string]*File // ❌ Race condition
}

func (s *MyStore) Get(id string) *File {
    return s.cache[id] // ❌ Concurrent read/write panic
}

func (s *MyStore) Put(id string, file *File) {
    s.cache[id] = file // ❌ Concurrent writes
}

// CORRECT: Proper synchronization
type MyStore struct {
    cache map[string]*File
    mu    sync.RWMutex
}

func (s *MyStore) Get(id string) *File {
    s.mu.RLock()
    defer s.mu.RUnlock()
    return s.cache[id]
}

func (s *MyStore) Put(id string, file *File) {
    s.mu.Lock()
    defer s.mu.Unlock()
    s.cache[id] = file
}
```

### 5. Breaking Atomicity of Multi-Step Operations

```go
// WRONG: No transaction
func (s *MyStore) Move(ctx *AuthContext, from, to FileHandle) error {
    if err := s.unlinkFrom(ctx, from); err != nil {
        return err
    }

    if err := s.linkTo(ctx, to); err != nil {
        // ❌ File is now orphaned!
        return err
    }
}

// CORRECT: Use transaction
func (s *MyStore) Move(ctx *AuthContext, from, to FileHandle) error {
    tx, err := s.db.BeginTx(ctx.Context, nil)
    if err != nil {
        return err
    }
    defer tx.Rollback()

    if err := s.unlinkFrom(tx, from); err != nil {
        return err
    }

    if err := s.linkTo(tx, to); err != nil {
        return err
    }

    return tx.Commit() // ✅ Atomic: all or nothing
}
```

### 6. Not Making Delete Idempotent

```go
// WRONG: Return error if not found
func (s *MyStore) Delete(ctx context.Context, id PayloadID) error {
    exists, err := s.PayloadExists(ctx, id)
    if err != nil {
        return err
    }
    if !exists {
        return errors.New("not found") // ❌ Breaks idempotency
    }
    return s.backend.Delete(ctx, id)
}

// CORRECT: Idempotent delete
func (s *MyStore) Delete(ctx context.Context, id PayloadID) error {
    err := s.backend.Delete(ctx, id)
    if err != nil && !isNotFoundError(err) {
        return err
    }
    return nil // ✅ Success even if not found
}
```

## Integration with DittoFS

### Step 1: Register Your Store

Create a factory function:

```go
// pkg/config/stores.go

func initMyMetadataStore(config MyMetadataStoreConfig) (metadata.MetadataStore, error) {
    return mystore.NewMyMetadataStore(mystore.Config{
        DSN:          config.DSN,
        MaxConns:     config.MaxConns,
        Capabilities: getDefaultCapabilities(),
    })
}

func initMyPayloadStore(config MyPayloadStoreConfig) (payload.PayloadStore, error) {
    return mystore.NewMyPayloadStore(mystore.Config{
        Endpoint: config.Endpoint,
        Bucket:   config.Bucket,
    })
}
```

### Step 2: Add Configuration

```go
// pkg/config/config.go

type StoreConfig struct {
    Type string `yaml:"type"` // "memory", "badger", "mystore"

    // ... existing configs ...

    MyStore *MyStoreConfig `yaml:"mystore,omitempty"`
}

type MyStoreConfig struct {
    DSN      string `yaml:"dsn"`
    MaxConns int    `yaml:"max_conns"`
}
```

### Step 3: Update Registry Initialization

```go
// pkg/registry/registry.go

func createMetadataStore(name string, config config.StoreConfig) (metadata.MetadataStore, error) {
    switch config.Type {
    case "memory":
        return initMemoryStore(config)
    case "badger":
        return initBadgerStore(config)
    case "mystore": // Your new store
        return initMyStore(config)
    default:
        return nil, fmt.Errorf("unknown metadata store type: %s", config.Type)
    }
}
```

### Step 4: Add Documentation

Update `docs/CONFIGURATION.md` with your store's configuration:

```yaml
metadata:
  stores:
    my-metadata:
      type: mystore
      mystore:
        dsn: "postgres://user:pass@localhost/dittofs"
        max_conns: 100

payload:
  stores:
    my-payload:
      type: mystore
      mystore:
        endpoint: "https://storage.example.com"
        bucket: "dittofs-payload"
```

## Conclusion

Implementing custom stores requires understanding DittoFS's architecture, following best practices for thread safety and error handling, and thorough testing. The key principles are:

1. **Respect the interfaces**: Implement all required methods correctly
2. **Thread safety**: Always protect shared state
3. **Context awareness**: Check cancellation in long operations
4. **Error handling**: Use structured errors and proper codes
5. **Performance**: Cache hot data, batch operations, use connection pools
6. **Testing**: Use provided test suites and add custom tests
7. **Documentation**: Document configuration and usage

By following this guide, you can create production-ready store implementations that integrate seamlessly with DittoFS.

## Additional Resources

- **Interface Definitions**: `pkg/metadata/store.go`, `pkg/payload/store/store.go`
- **Reference Implementations**:
  - Memory: `pkg/metadata/store/memory/`, `pkg/payload/store/memory/`
  - BadgerDB: `pkg/metadata/store/badger/`
  - S3: `pkg/payload/store/s3/`
- **Test Suites**: `pkg/payload/store/testing/`
- **Architecture**: `docs/ARCHITECTURE.md`
- **Configuration**: `docs/CONFIGURATION.md`
- **Contributing**: `docs/CONTRIBUTING.md`
