# DNFS - Dynamic NFS

A lightweight, pure Go implementation of an NFS version 3 server that decouples metadata management from data content storage. DNFS provides a standards-compliant NFS interface while allowing flexible, pluggable backends for both metadata and content repositories.

## Overview

DNFS implements the NFSv3 protocol (RFC 1813) and Mount protocol, enabling any system to provide NFS-compatible file access. Unlike traditional NFS servers that couple metadata and data storage, DNFS cleanly separates concerns through abstract repository interfaces, making it ideal for distributed storage systems, cloud architectures, and custom storage backends.

### Key Design Principle

**Decouple Metadata from Content**: DNFS separates file metadata (attributes, directory structure, permissions) from actual file data, allowing you to:

- Store metadata in one system (database, distributed store, in-memory cache)
- Store content in another system (object storage, distributed filesystem, custom backend)
- Swap implementations without changing core protocol logic

## Features

- **NFSv3 Protocol Support**: Complete implementation of core NFSv3 operations (RFC 1813)
- **Mount Protocol**: Full mount/unmount capability for standard NFS client operations
- **Pluggable Architecture**:
  - Abstract `Repository` interface for custom metadata backends
  - Abstract `ContentRepository` interface for custom data storage
- **In-Memory Repository**: Built-in metadata storage for testing and development
- **Filesystem Content Storage**: Default file-based content backend
- **Structured Logging**: Configurable log levels (DEBUG, INFO, WARN, ERROR)
- **Pure Go**: No C dependencies, cross-platform compilation

## Architecture

```
dnfs/
├── cmd/dnfs/                          # CLI entry point
├── internal/
│   ├── logger/                        # Structured logging utilities
│   ├── metadata/                      # Metadata domain model
│   │   ├── file.go                    # File attributes and types
│   │   ├── export.go                  # Export configuration
│   │   ├── repository.go              # Repository interface
│   │   └── persistence/
│   │       └── memory.go              # In-memory implementation
│   ├── content/                       # Content storage abstraction
│   │   ├── repository.go              # Repository interface
│   │   └── fs.go                      # Filesystem backend
│   ├── protocol/                      # Protocol implementations
│   │   ├── rpc/                       # RPC layer (RFC 5531)
│   │   ├── mount/                     # Mount protocol (RFC 1813 Appendix I)
│   │   └── nfs/                       # NFS protocol (RFC 1813)
│   └── server/                        # NFS server core
│       ├── server.go                  # TCP listener and routing
│       ├── conn.go                    # Connection handler
│       ├── handler.go                 # Handler interfaces
│       └── utils.go                   # Generic RPC utilities
└── go.mod                             # Go module definition
```

## Quick Start

### Prerequisites

- Go 1.25 or later

### Build

```bash
go build -o dnfs cmd/dnfs/main.go
```

### Run

```bash
# Default configuration (port 2049, INFO logging)
./dnfs

# Custom port with debug logging
./dnfs -port 2050 -log-level debug

# Custom content storage path
./dnfs -content-path /mnt/nfs-data
```

The server will create an export at `/export` with sample files in the `images/` directory.

### Mount from Client

```bash
# Linux
sudo mount -t nfs -o nfsvers=3,tcp localhost:/export /mnt/nfs

# macOS (with resvport option)
sudo mount -t nfs -o nfsvers=3,tcp,resvport localhost:/export /mnt/nfs

# Verify mount
ls /mnt/nfs
```

### Unmount

```bash
sudo umount /mnt/nfs
```

## Configuration

### Environment Variables

- `LOG_LEVEL`: Set logging level (DEBUG, INFO, WARN, ERROR). Default: INFO

### Command-Line Flags

- `-port string`: Server port. Default: 2049
- `-log-level string`: Logging level (DEBUG, INFO, WARN, ERROR). Default: INFO
- `-content-path string`: Path to store file content. Default: /tmp/dnfs-content

### Example

```bash
LOG_LEVEL=DEBUG ./dnfs -port 2050 -content-path /var/lib/dnfs-content
```

## Protocol Implementation Status

### Mount Protocol (RFC 1813 Appendix I)

| Procedure | Status | Notes |
|-----------|--------|-------|
| NULL | ✅ Implemented | Connectivity test |
| MNT | ✅ Implemented | Mount export and get root handle |
| DUMP | ❌ Not Implemented | List mounts |
| UMNT | ✅ Implemented | Unmount (basic) |
| UMNTALL | ❌ Not Implemented | Unmount all |
| EXPORT | ❌ Not Implemented | List exports |

### NFS Protocol (RFC 1813)

| Procedure | Status | Notes |
|-----------|--------|-------|
| NULL | ✅ Implemented | Connectivity test |
| GETATTR | ✅ Implemented | Get file attributes |
| SETATTR | ✅ Implemented | Set file attributes with WCC |
| LOOKUP | ✅ Implemented | Lookup filename in directory |
| ACCESS | ✅ Implemented | Check access permissions |
| READLINK | ❌ Not Implemented | Read symbolic link |
| READ | ✅ Implemented | Read file data |
| WRITE | ❌ Not Implemented | Write file data |
| CREATE | ❌ Not Implemented | Create regular file |
| MKDIR | ❌ Not Implemented | Create directory |
| SYMLINK | ❌ Not Implemented | Create symbolic link |
| MKNOD | ❌ Not Implemented | Create special device |
| REMOVE | ❌ Not Implemented | Delete file |
| RMDIR | ❌ Not Implemented | Delete directory |
| RENAME | ❌ Not Implemented | Rename file/directory |
| LINK | ❌ Not Implemented | Create hard link |
| READDIR | ✅ Implemented | List directory entries |
| READDIRPLUS | ✅ Implemented | List with attributes |
| FSSTAT | ✅ Implemented | Filesystem statistics |
| FSINFO | ✅ Implemented | Filesystem information |
| PATHCONF | ✅ Implemented | POSIX information |
| COMMIT | ❌ Not Implemented | Commit to stable storage |

## Development

### Creating a Custom Metadata Repository

Implement the `metadata.Repository` interface to create a custom metadata backend:

```go
package main

import (
    "github.com/cubbit/dnfs/internal/metadata"
)

type MyRepository struct {
    // Your implementation
}

func (r *MyRepository) GetFile(handle metadata.FileHandle) (*metadata.FileAttr, error) {
    // Your implementation
}

func (r *MyRepository) CreateFile(handle metadata.FileHandle, attr *metadata.FileAttr) error {
    // Your implementation
}

// ... implement other interface methods
```

Then use it with the server:

```go
repo := &MyRepository{}
contentRepo, _ := content.NewFSContentRepository("/tmp/content")
server := nfsServer.New("2049", repo, contentRepo)
server.Serve(context.Background())
```

### Creating a Custom Content Repository

Implement the `content.Repository` interface for custom content storage:

```go
package main

import (
    "github.com/cubbit/dnfs/internal/content"
    "io"
)

type MyContentRepository struct {
    // Your implementation
}

func (r *MyContentRepository) ReadContent(id content.ContentID) (io.ReadCloser, error) {
    // Read from your backend
}

func (r *MyContentRepository) GetContentSize(id content.ContentID) (uint64, error) {
    // Get size from your backend
}

func (r *MyContentRepository) ContentExists(id content.ContentID) (bool, error) {
    // Check existence in your backend
}
```

### Adding a New NFS Procedure

1. Define procedure constant in `internal/protocol/nfs/constants.go`
2. Create request/response types in the appropriate file (e.g., `internal/protocol/nfs/write.go`)
3. Add decoder and encoder methods to the response struct
4. Implement the handler method in `internal/protocol/nfs/nfs.go` or a separate file
5. Add routing in `internal/server/conn.go` handleNFSProcedure() method
6. Add interface method to `NFSHandler` in `internal/server/handler.go`

Example structure:

```go
// constants.go
const NFSProcWrite = 7

// write.go
type WriteRequest struct {
    Handle []byte
    Offset uint64
    Count  uint32
    Data   []byte
}

type WriteResponse struct {
    Status uint32
    // ... response fields
}

func DecodeWriteRequest(data []byte) (*WriteRequest, error) {
    // Your implementation
}

func (resp *WriteResponse) Encode() ([]byte, error) {
    // Your implementation
}

// nfs.go
func (h *DefaultNFSHandler) Write(repo metadata.Repository, req *WriteRequest) (*WriteResponse, error) {
    // Your implementation
}
```

## Repository Interfaces

### metadata.Repository

The metadata repository manages file attributes, directory structure, and export configuration:

```go
type Repository interface {
    // Export operations
    AddExport(path string, options ExportOptions, rootAttr *FileAttr) error
    GetExports() ([]Export, error)
    FindExport(path string) (*Export, error)
    DeleteExport(path string) error
    GetRootHandle(exportPath string) (FileHandle, error)

    // File operations
    CreateFile(handle FileHandle, attr *FileAttr) error
    GetFile(handle FileHandle) (*FileAttr, error)
    UpdateFile(handle FileHandle, attr *FileAttr) error
    DeleteFile(handle FileHandle) error

    // Directory hierarchy
    SetParent(child FileHandle, parent FileHandle) error
    GetParent(child FileHandle) (FileHandle, error)
    AddChild(parent FileHandle, name string, child FileHandle) error
    GetChild(parent FileHandle, name string) (FileHandle, error)
    GetChildren(parent FileHandle) (map[string]FileHandle, error)
    DeleteChild(parent FileHandle, name string) error
}
```

### content.Repository

The content repository manages actual file data:

```go
type Repository interface {
    // ReadContent returns a reader for the content identified by the given ID
    ReadContent(id ContentID) (io.ReadCloser, error)

    // GetContentSize returns the size of the content in bytes
    GetContentSize(id ContentID) (uint64, error)

    // ContentExists checks if content with the given ID exists
    ContentExists(id ContentID) (bool, error)
}
```

## Use Cases

- **Distributed Storage Gateway**: Use DNFS as an NFS interface to a distributed storage backend
- **Cloud Storage Bridge**: Connect NFS clients to object storage (S3, GCS, etc.)
- **Testing & Development**: Use in-memory backends for rapid development
- **Custom Tiering**: Implement different storage backends for hot/warm/cold data
- **Multi-tenant NFS**: Create custom repositories that isolate tenants
- **Metadata Acceleration**: Separate metadata cache from primary content storage

## Performance Considerations

- **Metadata Repository**: In-memory implementation suitable for development; for production, implement a high-performance backend (cache layer, database, etc.)
- **Content Storage**: Filesystem backend works for small-scale deployments; consider object storage for distributed scenarios
- **Connection Handling**: Each NFS connection runs in its own goroutine
- **Read Operations**: File reads leverage Go's efficient I/O with support for seekable content repositories

## References

- [RFC 1813](https://tools.ietf.org/html/rfc1813) - NFS Version 3 Protocol Specification
- [RFC 5531](https://tools.ietf.org/html/rfc5531) - RPC: Remote Procedure Call Protocol Specification Version 2
- [RFC 1833](https://tools.ietf.org/html/rfc1833) - Binding Protocols for ONC RPC Version 2

## License

See LICENSE file for details.

## Contributing

Contributions are welcome! Please ensure:

- Code follows Go conventions and includes appropriate error handling
- New procedures include both RFC references and protocol documentation
- Tests cover new functionality
- Documentation is updated for significant changes

## Future Roadmap

- [ ] Implement WRITE procedure
- [ ] Implement CREATE/MKDIR procedures
- [ ] Implement REMOVE/RMDIR procedures
- [ ] Add DUMP and EXPORT to Mount protocol
- [ ] Support for symbolic links
- [ ] UDP support in addition to TCP
- [ ] Performance optimizations (connection pooling, caching strategies)
- [ ] Comprehensive test suite
- [ ] Metrics and observability
