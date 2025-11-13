<div align="center">

# DittoFS

[![Go Version](https://img.shields.io/badge/Go-1.25+-00ADD8?style=flat&logo=go)](https://go.dev/)
[![Tests](https://img.shields.io/badge/tests-passing-brightgreen?style=flat)](https://github.com/marmos91/dittofs)
[![Go Report Card](https://goreportcard.com/badge/github.com/marmos91/dittofs)](https://goreportcard.com/report/github.com/marmos91/dittofs)
[![License: MIT](https://img.shields.io/badge/License-MIT-yellow.svg?style=flat)](LICENSE)
[![Status](https://img.shields.io/badge/status-experimental-orange?style=flat)](https://github.com/marmos91/dittofs)

**A modular virtual filesystem written entirely in Go**

Decouple file interfaces from storage backends. Expose your data through multiple protocols (NFS, SMB, FTP) while maintaining complete control over how metadata and content are stored.

</div>

---

## The Problem with Traditional Filesystem Servers

Traditional filesystem server implementations face several limitations:

- **Protocol lock-in**: Each protocol implementation is tightly coupled to its storage layer
- **High-level system permissions**: Often require kernel-level access and FUSE
- **Inflexible architecture**: Cannot mix and match protocols with different storage backends
- **Complex deployment**: Multiple servers needed for multiple protocols

This results in:

- Operational complexity when supporting multiple access methods
- Performance overhead from multiple abstraction layers
- Difficult customization of storage backends
- Vendor lock-in to specific storage solutions

## The DittoFS Solution

DittoFS provides a modular architecture that separates concerns through three key abstractions:

```
┌─────────────────────────────────────────┐
│                Protocols                │
│  ┌─────────┐  ┌─────────┐  ┌─────────┐  │
│  │   NFS   │  │   SMB   │  │   FTP   │  │
│  └────┬────┘  └────┬────┘  └────┬────┘  │
└───────┼────────────┼────────────┼───────┘
        │            │            │
        └────────────┴────────────┘
                     │
        ┌────────────┴────────────┐
        │     DittoFS Core        │
        │   (Adapter Manager)     │
        └────────┬──────────┬─────┘
                 │          │
                 ▼          ▼
        ┌─────────────┐  ┌──────────────┐
        │  Metadata   │  │   Content    │
        │ Repository  │  │  Repository  │
        └─────────────┘  └──────────────┘
             │                  │
             ▼                  ▼
        Redis/Postgres     S3/Filesystem
        In-Memory          Custom Storage
```

### Key Concepts

**1. Adapters**: Protocol-specific interfaces that clients connect to

- Each adapter implements a specific file access protocol (NFS, SMB, etc.)
- Multiple adapters can run simultaneously
- Adapters are lightweight wrappers that translate protocol operations

**2. Metadata Repository**: Stores file structure and attributes

- File metadata (size, timestamps, permissions, extended attributes)
- Directory hierarchy and relationships
- File handles and export configuration
- Pluggable backends (in-memory, Redis, PostgreSQL, etc.)

**3. Content Repository**: Stores actual file data

- Read and write operations
- Content addressing and chunking strategies
- Pluggable backends (filesystem, S3, custom solutions)

### Key Benefits

1. **Multi-Protocol Support**: Expose the same data through NFS, SMB, FTP, or custom protocols
2. **No Special Permissions**: Runs entirely in userspace - no FUSE, no kernel modules
3. **Maximum Flexibility**: Mix and match any protocol adapter with any storage backend
4. **Better Performance**: Direct protocol implementation, optimized I/O paths
5. **Easy Integration**: Pure Go means easy embedding in existing applications
6. **Cloud Native**: Perfect for distributed systems and cloud architectures

## Use Cases

### Unified Multi-Protocol Gateway

```
Adapters → NFS + SMB (simultaneous access)
Metadata → PostgreSQL (ACID compliance, fast queries)
Content → S3 (scalable, durable, cost-effective)

Use case: Allow Linux servers (NFS) and Windows clients (SMB) 
to access the same S3-backed storage
```

### Development & Testing

```
Facades → NFS only
Metadata → In-Memory
Content → In-Memory or local filesystem

Use case: Fast development iteration without external dependencies
```

### High-Performance Distributed Cache

```
Facades → NFS
Metadata → Redis (in-memory, sub-millisecond lookups)
Content → Local NVMe + S3 tiering

Use case: ML training pipelines with hot data on NVMe, 
cold data automatically tiered to S3
```

## Quick Start

### Installation

```bash
go build -o dittofs cmd/dittofs/main.go
```

### Run Server

```bash
# Default configuration (NFS adapter on port 12049)
./dittofs

# Custom configuration
./dittofs -port 12049 -log-level DEBUG -content-path /var/lib/dittofs
```

### Mount from Client (NFS)

```bash
# Linux
sudo mount -t nfs -o nfsvers=3,tcp,port=12049 localhost:/export /mnt/nfs

# macOS
sudo mount -t nfs -o nfsvers=3,tcp,port=12049,resvport localhost:/export /mnt/nfs
```

### Testing

```bash
# Run unit tests
go test ./...

# Run unit tests with coverage
go test -cover ./...

# Run E2E tests (requires NFS client installed)
go test -v -timeout 30m ./test/e2e/...

# Run specific E2E suite
go test -v ./test/e2e -run TestE2E/memory/BasicOperations
```

DittoFS includes comprehensive testing:
- **Unit tests** for core components (RPC, XDR, metadata, content repositories)
- **E2E tests** that mount real NFS filesystems and test complete workflows
- **Test matrix** running all suites against multiple storage backends (memory, filesystem)

## Architecture Deep Dive

### Adapter Pattern

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

### Repository Interfaces

**Metadata Repository** - Handles file structure:

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

**Content Repository** - Handles file data:

```go
type Repository interface {
    ReadContent(ctx context.Context, id ContentID, offset int64, size uint32) ([]byte, error)
    WriteContent(ctx context.Context, id ContentID, offset int64, data []byte) error
    GetSize(ctx context.Context, id ContentID) (int64, error)
}
```

### Custom Backend Implementation

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

## NFS Implementation Details

### Mounting Without Portmapper

DittoFS uses a fixed port and does not require portmapper/rpcbind.

**Mount with explicit port:**

```bash
# Linux
sudo mount -t nfs -o nfsvers=3,tcp,port=12049 localhost:/export /mnt/test

# macOS
sudo mount -t nfs -o nfsvers=3,tcp,port=12049,resvport localhost:/export /mnt/test
```

**Using showmount (traditional tools):**

Traditional tools like `showmount` require portmapper and will not work
with DittoFS. Instead, use direct mount commands or the provided test clients.

## Current Status

### ✅ Implemented

**NFS Adapter (NFSv3)**

- Core read operations (GETATTR, LOOKUP, READ, READDIR, READDIRPLUS)
- Core write operations (WRITE, CREATE, MKDIR, REMOVE, RMDIR, RENAME)
- Link operations (LINK, SYMLINK, READLINK)
- Mount protocol (MNT, UMNT, EXPORT)
- TCP transport
- File handle management

**Repositories**

- In-memory metadata repository (fully functional)
- Filesystem content repository (fully functional)

**Infrastructure**

- Adapter management framework
- XDR encoding/decoding
- RPC message handling
- Configurable logging
- Buffer pooling for performance

### 🚧 Roadmap

**Phase 1: Performance Optimization & Refactoring**

- [ ] Code refactoring and cleanup
- [ ] Memory leak prevention and profiling
- [ ] Performance optimization (zero-copy I/O where possible)
- [ ] Connection pool management
- [ ] Modular configuration system

**Phase 2: Testing**

- [x] Comprehensive E2E test suite
- [x] Unit test coverage for core components (RPC, XDR, Content, Metadata)
- [ ] NFS protocol compliance tests
- [ ] Coverage reporting and CI/CD integration

**Phase 3: Prometheus Metrics**

- [ ] Metrics export endpoints
- [ ] Operation counters and latency histograms
- [ ] Connection and resource tracking

**Phase 4: Load and Stress Testing**

- [ ] Performance benchmarks and comparison
- [ ] Load testing with realistic workloads
- [ ] Stress testing and failure scenarios

**Phase 5: Production-Ready Backends**

- [ ] S3-compatible content repository
- [ ] SQLite metadata repository

**Phase 6: SMB Protocol Adapter**

- [ ] SMB/CIFS protocol implementation
- [ ] Windows client compatibility
- [ ] Concurrent NFS + SMB access

## Protocol Implementation Status

### Mount Protocol

| Procedure | Status | Notes |
|-----------|--------|-------|
| NULL | ✅ | |
| MNT | ✅ | |
| UMNT | ✅ | |
| UMNTALL | ✅ | |
| DUMP | ✅ | |
| EXPORT | ✅ | |

### NFS Protocol v3 - Read Operations

| Procedure | Status | Notes |
|-----------|--------|-------|
| NULL | ✅ | |
| GETATTR | ✅ | |
| SETATTR | ✅ | |
| LOOKUP | ✅ | |
| ACCESS | ✅ | |
| READ | ✅ | |
| READDIR | ✅ | |
| READDIRPLUS | ✅ | |
| FSSTAT | ✅ | |
| FSINFO | ✅ | |
| PATHCONF | ✅ | |
| READLINK | ✅ | |

### NFS Protocol v3 - Write Operations

| Procedure | Status | Notes |
|-----------|--------|-------|
| WRITE | ✅ | |
| CREATE | ✅ | |
| MKDIR | ✅ | |
| REMOVE | ✅ | |
| RMDIR | ✅ | |
| RENAME | ✅ | |
| LINK | ✅ | |
| SYMLINK | ✅ | |
| MKNOD | ✅ | Limited support |
| COMMIT | ✅ | |

## Performance Characteristics

DittoFS is designed for high performance through several architectural choices:

- **Direct protocol implementation**: No FUSE overhead
- **Goroutine-per-connection model**: Leverages Go's lightweight concurrency
- **Buffer pooling**: Reduces GC pressure for large I/O operations
- **Streaming I/O**: Efficient handling of large files without full buffering
- **Pluggable caching**: Implement custom caching strategies per use case
- **Zero-copy aspirations**: Working toward minimal data copying in hot paths

**Benchmark results** (coming in Phase 2):

- Sequential read throughput
- Random read IOPS
- Metadata operation latency
- Concurrent connection scalability

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

## Comparison with Alternatives

| Feature | Traditional NFS + FUSE | Cloud Storage Gateways | DittoFS |
|---------|------------------------|------------------------|---------|
| Permission Requirements | Kernel-level | Varies | Userspace only |
| Multi-protocol Support | Separate servers | Limited | Unified (planned) |
| Storage Backend | Filesystem only | Vendor-specific | Pluggable |
| Metadata Backend | Filesystem only | Vendor-specific | Pluggable |
| Language | C/C++ | Varies | Pure Go |
| Deployment | Complex (kernel modules) | Complex (dependencies) | Single binary |
| Customization | Limited | Limited | Full control |
| Cloud Native | No | Sometimes | Yes |

## Contributing

DittoFS is in active development and welcomes contributions!

### Areas Needing Attention

**High Priority**

- Additional repository backend implementations (Redis, PostgreSQL, S3)
- Performance optimization and profiling
- Test coverage expansion
- Protocol compliance testing

**Medium Priority**

- SMB/CIFS facade implementation
- Documentation improvements
- Example applications and tutorials
- Monitoring and observability

**Future Work**

- WebDAV facade
- NFSv4 support
- Advanced caching strategies
- Multi-region replication

### Development Setup

```bash
# Clone repository
git clone https://github.com/marmos91/dittofs.git
cd dittofs

# Install dependencies
go mod download

# Run unit tests
go test ./...

# Run E2E tests (requires NFS client)
go test -v -timeout 30m ./test/e2e/...

# Build
go build -o dittofs cmd/dittofs/main.go

# Run with development settings
./dittofs -log-level DEBUG
```

### E2E Testing Framework

DittoFS includes a comprehensive end-to-end testing framework that validates real-world NFS operations by:

- **Starting a real DittoFS server** with configurable backends
- **Mounting the NFS filesystem** using platform-native mount commands
- **Executing real file operations** using standard Go `os` package functions
- **Testing all combinations** of facades and storage backends

Test suites cover:
- Basic file operations (create, read, write, delete)
- Directory operations (mkdir, readdir, rename)
- Symbolic and hard links
- File attributes and permissions
- Idempotency guarantees
- Edge cases and boundary conditions

See [test/e2e/README.md](test/e2e/README.md) for detailed documentation on running and writing E2E tests.

### Code Structure

```
dittofs/
├── cmd/dittofs/          # Main application entry point
├── pkg/                  # Public APIs
│   ├── metadata/         # Metadata repository interfaces and implementations
│   ├── content/          # Content repository interfaces and implementations
│   ├── facade/           # Facade interfaces and implementations
│   └── server/           # Core server logic
├── internal/             # Internal implementation details
│   ├── protocol/nfs/     # NFS protocol implementation
│   │   ├── mount/        # Mount protocol handlers
│   │   ├── v3/           # NFSv3 handlers
│   │   ├── rpc/          # RPC layer
│   │   └── xdr/          # XDR encoding/decoding
│   ├── logger/           # Logging utilities
│   └── metadata/         # Internal metadata utilities
└── docs/                 # Additional documentation (future)
```

## Troubleshooting

### Common Issues

**Cannot mount: Connection refused**

```bash
# Check if DittoFS is running
ps aux | grep dittofs

# Verify port is correct
netstat -an | grep 12049

# Check firewall rules
sudo iptables -L | grep 12049
```

**Permission denied when mounting**

```bash
# On Linux, may need to allow non-privileged ports
sudo sysctl -w net.ipv4.ip_unprivileged_port_start=0

# On macOS, must use resvport option
sudo mount -t nfs -o nfsvers=3,tcp,port=12049,resvport localhost:/export /mnt/test
```

**Stale file handle errors**

- This typically happens if the server restarts and clients have cached file handles
- Unmount and remount the filesystem on clients
- In production, implement persistent file handle mapping

## Security Considerations

⚠️ **Current Security Status**: DittoFS is experimental software and has not undergone security auditing.

**Current Implementation**

- Basic AUTH_UNIX authentication support
- No built-in encryption (use network-level encryption like WireGuard/IPsec)
- File permissions enforced at metadata layer

**Planned Security Features**

- Kerberos authentication support (AUTH_GSS)
- Built-in TLS support for RPC
- Audit logging for all operations
- Role-based access control (RBAC)
- Encryption at rest for content repository

**Production Recommendations**

- Deploy behind a VPN or use network encryption
- Implement authentication at the network layer
- Use read-only exports where appropriate
- Monitor access logs carefully
- Restrict export access by IP address

## References

### Specifications

- [RFC 1813](https://tools.ietf.org/html/rfc1813) - NFS Version 3 Protocol Specification
- [RFC 5531](https://tools.ietf.org/html/rfc5531) - RPC: Remote Procedure Call Protocol Specification
- [RFC 4506](https://tools.ietf.org/html/rfc4506) - XDR: External Data Representation Standard
- [RFC 1094](https://tools.ietf.org/html/rfc1094) - NFS: Network File System Protocol (Version 2)

### Related Projects

- [go-nfs](https://github.com/willscott/go-nfs) - Another NFS implementation in Go
- [FUSE](https://github.com/libfuse/libfuse) - Filesystem in Userspace

## FAQ

**Q: Why not use FUSE?**

A: FUSE adds an additional abstraction layer and requires kernel modules. DittoFS runs entirely in userspace and implements protocols directly, giving better control and performance.

**Q: Can I use this in production?**

A: Not yet. DittoFS is experimental and needs more testing, security auditing, and hardening before production use.

**Q: Which NFS version is supported?**

A: Currently NFSv3 over TCP. NFSv4 support is planned for a future phase.

**Q: Can I implement my own protocol facade?**

A: Yes! That's the whole point. Implement the `Facade` interface and wire it to the metadata/content repositories.

**Q: How does performance compare to kernel NFS?**

A: We're still benchmarking, but the lack of FUSE overhead and optimized Go implementation should provide competitive performance for most workloads. Results will be published in Phase 2.

**Q: Does it support file locking?**

A: Not yet. NLM (Network Lock Manager) protocol support is planned but not currently implemented.

**Q: Can I use this with Windows clients?**

A: Windows can mount NFS shares, but the SMB/CIFS facade will provide better Windows integration when implemented.

**Q: Is content deduplication supported?**

A: Not currently, but the content repository abstraction allows for implementing content-addressable storage with deduplication.

## License

MIT License - See LICENSE file for details

## Disclaimer

⚠️ **DittoFS is experimental software**

- Do not use in production environments without thorough testing
- The API may change without notice
- No backwards compatibility guarantees during experimental phase
- Security has not been professionally audited
- Performance characteristics are not yet fully benchmarked

## Acknowledgments

Built with ❤️ in Go.

---

**Getting Started?** Check out the Quick Start section above.

**Questions?** Open an issue on GitHub.

**Want to Contribute?** See CONTRIBUTING.md for guidelines.
