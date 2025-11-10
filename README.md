# DittoFS

> **Experimental** - Not yet production ready

A pure Go implementation of an NFSv3 server that eliminates the need for FUSE and kernel-level permissions while providing complete flexibility in how you store metadata and content.

## The Problem with Traditional NFS

Traditional NFS implementations require:

- **High-level system permissions** (kernel-level access)
- **FUSE for virtualization**, adding a layer of abstraction
- **Tight coupling** between metadata and content storage

This results in:

- Limited deployment flexibility
- Performance overhead from double abstraction (FUSE + NFS)
- Complex permission management
- Difficult customization

## The DittoFS Solution

DittoFS is a userspace NFS server that completely decouples metadata from content storage:

```
┌─────────────┐
│ NFS Clients │
└──────┬──────┘
       │
       ▼
┌─────────────────────┐
│  DittoFS (NFSv3)    │
│  Pure Go Server     │
└─────┬──────────┬────┘
      │          │
      ▼          ▼
┌──────────┐  ┌──────────┐
│ Metadata │  │ Content  │
│  Store   │  │  Store   │
└──────────┘  └──────────┘
   Redis        S3
   Postgres     Dropbox
   In-Memory    Custom
```

### Key Benefits

1. **No Special Permissions**: Runs entirely in userspace - no FUSE, no kernel modules
2. **Maximum Flexibility**: Mix and match any metadata backend with any content backend
3. **Better Performance**: Single abstraction layer, optimized I/O paths
4. **Easy Integration**: Pure Go means easy embedding in existing applications
5. **Cloud Native**: Perfect for distributed systems and cloud architectures

## Use Cases

### Cloud Storage Gateway

```
Metadata → PostgreSQL (fast queries, ACID)
Content  → S3 (scalable, durable)
```

### High-Performance Cache

```
Metadata → Redis (in-memory speed)
Content  → Local SSD + S3 tiering
```

### Multi-Region Storage

```
Metadata → Global distributed database
Content  → Regional object storage
```

### Development & Testing

```
Metadata → In-Memory
Content  → In-Memory or local filesystem
```

## Quick Start

### Installation

```bash
go build -o dittofs cmd/dittofs/main.go
```

### Run Server

```bash
# Default configuration
./dittofs

# Custom configuration
./dittofs -port 2049 -log-level DEBUG -content-path /var/lib/dittofs
```

### Mount from Client

```bash
# Linux
sudo mount -t nfs -o nfsvers=3,tcp localhost:/export /mnt/nfs

# macOS
sudo mount -t nfs -o nfsvers=3,tcp,resvport localhost:/export /mnt/nfs
```

## Architecture

DittoFS separates concerns through clean interfaces:

### Metadata Repository

Handles file attributes, directory structure, permissions:

- File metadata (size, timestamps, permissions)
- Directory hierarchy
- File handles
- Export configuration

### Content Repository

Handles actual file data:

- Read operations
- Write operations (coming soon)
- Content addressing
- Size queries

### Example: Custom Backend

```go
// Implement your metadata backend
type PostgresRepository struct {
    db *sql.DB
}

func (r *PostgresRepository) GetFile(handle FileHandle) (*types.NFSFileAttr, error) {
    // Query PostgreSQL
}

// Implement your content backend
type S3ContentRepository struct {
    client *s3.Client
}

func (r *S3ContentRepository) ReadContent(id ContentID) (io.ReadCloser, error) {
    // Read from S3
}

// Use them together
server := nfsServer.New("2049", postgresRepo, s3Repo)
```

## Mounting Without Portmapper

DittoFS uses a fixed port and does not require portmapper/rpcbind.
This simplifies deployment in containerized and cloud environments.

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

**Kubernetes/Container environments:**

DittoFS is designed for modern cloud-native deployments where service
discovery is handled by the orchestration platform:

```yaml
# Kubernetes PV example
apiVersion: v1
kind: PersistentVolume
metadata:
  name: dittofs-pv
spec:
  capacity:
    storage: 10Gi
  nfs:
    server: dittofs.default.svc.cluster.local
    path: /export
  mountOptions:
    - nfsvers=3
    - tcp
    - port=12049
```

## Current Status

### ✅ Implemented

- NFSv3 core read operations (GETATTR, LOOKUP, READ, READDIR, etc.)
- NFSv3 core write operations (WRITE, LINK, MKNOD, SYMLINK, etc.)
- Mount protocol (MNT, UMNT)
- In-memory metadata repository
- Filesystem content repository
- TCP transport
- Configurable logging

### 🚧 Roadmap

**Phase 1: Hardening** (Current Focus)

- [ ] Code refactoring and optimization
- [ ] Memory leak prevention
- [ ] Error handling improvements
- [ ] Connection pool management

**Phase 2: Testing**

- [ ] E2E test suite
- [ ] Protocol compliance tests
- [ ] Performance benchmarks
- [ ] Load testing

**Phase 3: Production Backends**

- [ ] S3-compatible content repository
- [ ] Redis metadata repository
- [ ] PostgreSQL metadata repository
- [ ] Tiered storage support

**Phase 4: Protocol Extensions**

- [ ] UDP transport support
- [ ] SMB/CIFS protocol support
- [ ] NFSv4 support

**Phase 5: Operations**

- [ ] Prometheus metrics
- [ ] OpenTelemetry tracing
- [ ] Health checks
- [ ] Graceful shutdown

## Protocol Implementation

### Mount Protocol

| Procedure | Status |
|-----------|--------|
| NULL | ✅ |
| MNT | ✅ |
| UMNT | ✅ |
| UMNTALL | ✅ |
| DUMP | ✅ |
| EXPORT | ✅ |

### NFS Protocol (Read Operations)

| Procedure | Status |
|-----------|--------|
| NULL | ✅ |
| GETATTR | ✅ |
| SETATTR | ✅ |
| LOOKUP | ✅ |
| ACCESS | ✅ |
| READ | ✅ |
| READDIR | ✅ |
| READDIRPLUS | ✅ |
| FSSTAT | ✅ |
| FSINFO | ✅ |
| PATHCONF | ✅ |

### NFS Protocol (Write Operations)

| Procedure | Status |
|-----------|--------|
| WRITE | ✅ |
| CREATE | ✅ |
| MKDIR | ✅ |
| REMOVE | ✅ |
| RMDIR | ✅ |
| RENAME | ✅ |
| LINK | ✅ |
| SYMLINK | ✅ |

## Performance Characteristics

- **Single abstraction layer**: No FUSE overhead
- **Goroutine-per-connection**: Scales with Go's lightweight concurrency
- **Streaming I/O**: Efficient large file handling
- **Pluggable caching**: Implement your own caching strategy

## Why Pure Go?

- ✅ **Easy deployment**: Single binary, no dependencies
- ✅ **Cross-platform**: Linux, macOS, Windows
- ✅ **Easy integration**: Embed in existing Go applications
- ✅ **Modern concurrency**: Goroutines and channels
- ✅ **Memory safe**: No C/C++ vulnerabilities

## Comparison

| Feature | Traditional NFS + FUSE | DittoFS |
|---------|----------------------|---------|
| Permissions | Kernel-level required | Userspace |
| Abstraction layers | 2 (FUSE + NFS) | 1 (NFS only) |
| Metadata backend | Filesystem only | Pluggable |
| Content backend | Filesystem only | Pluggable |
| Language | C/C++ | Pure Go |
| Deployment | Complex | Single binary |

## Contributing

DittoFS is in active development. Contributions are welcome!

Areas needing attention:

- Additional repository backends
- Performance optimization
- Test coverage
- Documentation

## References

- [RFC 1813](https://tools.ietf.org/html/rfc1813) - NFS Version 3 Protocol
- [RFC 5531](https://tools.ietf.org/html/rfc5531) - RPC Protocol

## License

MIT License - See LICENSE file

## Disclaimer

⚠️ **DittoFS is experimental software**. Do not use in production environments without thorough testing. The API may change without notice.

---

Built with ❤️ in Go
