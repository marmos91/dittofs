# CLAUDE.md

This file provides guidance to Claude Code (claude.ai/code) when working with code in this repository.

## Project Overview

DittoFS is an experimental modular virtual filesystem written in Go that decouples file interfaces from storage backends. It implements NFSv3 protocol server in pure Go (userspace, no FUSE required) with pluggable metadata and content repositories.

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
sudo mount -t nfs -o nfsvers=3,tcp,port=2049 localhost:/export /mnt/test

# Mount on macOS (requires resvport)
sudo mount -t nfs -o nfsvers=3,tcp,port=2049,resvport localhost:/export /mnt/test

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

## Architecture

### Core Abstraction Layers

DittoFS uses the **Adapter pattern** to decouple protocols from storage:

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
└───────┬───────────────────┬─────────────┘
        │                   │
        ▼                   ▼
┌────────────────┐  ┌────────────────────┐
│   Metadata     │  │   Content          │
│   Repository   │  │   Repository       │
└────────────────┘  └────────────────────┘
```

### Key Interfaces

**1. Adapter Interface** (`pkg/adapter/adapter.go`)
- Each protocol implements the `Adapter` interface
- Lifecycle: `SetStores() → Serve() → Stop()`
- Multiple adapters share the same metadata/content repositories
- Thread-safe, supports graceful shutdown

**2. Metadata Repository** (`pkg/metadata/repository.go`)
- Stores file/directory structure, attributes, permissions
- Handles access control, mount tracking, exports
- Current implementation: `pkg/metadata/memory/` (in-memory)
- File handles are opaque uint64 identifiers

**3. Content Repository** (`pkg/content/repository.go`)
- Stores actual file data
- Supports read, write-at, truncate operations
- Current implementations:
  - `pkg/content/fs/`: Filesystem-backed storage
  - In-memory (used in tests)

### Directory Structure

```
dittofs/
├── cmd/dittofs/              # Main application entry point
│   └── main.go               # Server startup, flag parsing, init
│
├── pkg/                      # Public API (stable interfaces)
│   ├── adapter/              # Protocol adapter interface
│   │   ├── adapter.go        # Core Adapter interface
│   │   └── nfs/              # NFS adapter implementation
│   ├── metadata/             # Metadata repository interface
│   │   ├── repository.go     # Repository interface
│   │   ├── types.go          # FileAttr, FileHandle, etc.
│   │   └── memory/           # In-memory implementation
│   ├── content/              # Content repository interface
│   │   ├── repository.go     # Repository interface
│   │   └── fs/               # Filesystem implementation
│   └── server/               # DittoServer orchestration
│       └── server.go         # Multi-adapter server management
│
└── internal/                 # Private implementation details
    ├── protocol/nfs/         # NFS protocol implementation
    │   ├── dispatch.go       # RPC procedure routing
    │   ├── rpc/              # RPC layer (call/reply handling)
    │   ├── xdr/              # XDR encoding/decoding
    │   ├── types/            # NFS constants and types
    │   ├── mount/handlers/   # Mount protocol procedures
    │   └── v3/handlers/      # NFSv3 procedures (READ, WRITE, etc.)
    ├── metadata/             # Internal metadata utilities
    └── logger/               # Logging utilities
```

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
- Generated by metadata repository
- Never parsed or interpreted by protocol handlers
- Must remain stable across server restarts for production (currently not implemented)

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
- `READ`: Read file content (uses content repository)
- `WRITE`: Write file content (coordinates metadata + content)
- `CREATE`: Create file
- `MKDIR`: Create directory
- `REMOVE`: Delete file
- `RMDIR`: Delete empty directory
- `RENAME`: Move/rename file
- `READDIR` / `READDIRPLUS`: List directory entries

### Write Coordination Pattern

WRITE operations require coordination between metadata and content repositories:

```go
// 1. Update metadata (validates permissions, updates size/timestamps)
attr, preSize, preMtime, preCtime, err := metadataRepo.WriteFile(handle, newSize, authCtx)

// 2. Write actual data via content repository
err = contentRepo.WriteAt(attr.ContentID, data, offset)

// 3. Return updated attributes to client for cache consistency
```

The metadata repository:
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

### Adding a New Repository Backend

**Metadata Repository:**
1. Implement `pkg/metadata/Repository` interface
2. Handle file handle generation (must be unique and stable)
3. Implement permission checking in `CheckAccess()`
4. Ensure thread safety (concurrent mount access)
5. Consider persistence strategy for handles

**Content Repository:**
1. Implement `pkg/content/Repository` interface
2. Support random-access writes (`WriteAt`)
3. Handle sparse files and truncation
4. Consider using `SeekableContentRepository` for efficiency

### Adding a New Protocol Adapter

1. Create new package in `pkg/adapter/`
2. Implement `Adapter` interface:
   - `Serve(ctx)`: Start protocol server
   - `Stop(ctx)`: Graceful shutdown
   - `SetStores()`: Receive metadata/content repos
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
sudo mount -t nfs -o nfsvers=3,tcp,port=2049 localhost:/export /mnt/test
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
