# NFS Implementation

This document details DittoFS's NFSv3 implementation, protocol status, and client usage.

## Table of Contents

- [Embedded Portmapper](#embedded-portmapper)
- [Mounting](#mounting)
- [Protocol Implementation Status](#protocol-implementation-status)
- [Implementation Details](#implementation-details)
- [Testing NFS Operations](#testing-nfs-operations)

## Embedded Portmapper

DittoFS includes an embedded portmapper (RFC 1057) that enables standard NFS service discovery without requiring a system-level `rpcbind` daemon.

### Why an Embedded Portmapper?

NFS clients traditionally rely on a portmapper (port 111) to discover which port an NFS server is listening on. Without a portmapper, clients require explicit port options (`-o port=12049,mountport=12049`), and standard tools like `rpcinfo` and `showmount` don't work.

The embedded portmapper solves this by:

- Registering all DittoFS services (NFS, MOUNT, NLM, NSM) automatically on startup
- Responding to standard portmap queries via TCP and UDP
- Running on an unprivileged port (default 10111) to avoid requiring root
- Enabling `rpcinfo` and `showmount` to discover DittoFS services

### Service Discovery

With the portmapper running, standard NFS tools work:

```bash
# Query registered services
rpcinfo -p localhost -n 10111

# Show available exports
showmount -e localhost
```

### Configuration

The portmapper is enabled by default. It can be configured via `dfsctl`:

```bash
# Check current settings
dfsctl adapter settings nfs

# Change the portmapper port
dfsctl adapter settings nfs --set portmapper_port=10111

# Disable the portmapper entirely
dfsctl adapter settings nfs --set portmapper_enabled=false
```

Or via environment variables:

```bash
DITTOFS_ADAPTERS_NFS_PORTMAPPER_PORT=10111
DITTOFS_ADAPTERS_NFS_PORTMAPPER_ENABLED=false
```

### Security

The embedded portmapper follows standard security practices:

- **SET/UNSET restricted to localhost**: Only local clients can register or unregister services
- **CALLIT (procedure 5) omitted**: Prevents DDoS amplification attacks
- **Connection limits**: TCP connections are capped at 64 concurrent
- **Non-privileged port**: Default port 10111 avoids requiring root privileges

### Portmapper Failure is Non-Fatal

If the portmapper fails to start (e.g., port already in use), NFS continues to operate normally. Clients just need to specify ports explicitly in mount options.

## Mounting

### With Portmapper on Port 111

When the portmapper runs on the standard port 111 (requires root or `CAP_NET_BIND_SERVICE`), NFS clients can auto-discover ports and mount commands are simplified:

```bash
# Configure portmapper on standard port (requires root)
dfsctl adapter settings nfs --set portmapper_port=111

# Linux - no port options needed, client queries portmapper automatically
sudo mkdir -p /mnt/nfs
sudo mount -t nfs -o tcp localhost:/export /mnt/nfs

# macOS
mkdir -p /tmp/nfs
mount -t nfs -o tcp localhost:/export /tmp/nfs
```

### With Explicit Ports

When the portmapper is disabled or running on a non-standard port, specify the NFS port explicitly:

```bash
# Linux
sudo mkdir -p /mnt/nfs
sudo mount -t nfs -o tcp,port=12049,mountport=12049 localhost:/export /mnt/nfs

# macOS (sudo not required)
mkdir -p /tmp/nfs
mount -t nfs -o tcp,port=12049,mountport=12049 localhost:/export /tmp/nfs

# macOS may require resvport on some configurations
mount -t nfs -o tcp,port=12049,mountport=12049,resvport localhost:/export /tmp/nfs

# Unmount
sudo umount /mnt/nfs   # Linux
umount /tmp/nfs        # macOS
```

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

**Total**: 28 procedures fully implemented

## Implementation Details

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

## Testing NFS Operations

### Manual Testing

```bash
# Start server
./dfs start -log-level DEBUG

# Mount and test operations
sudo mount -t nfs -o tcp,port=12049,mountport=12049 localhost:/export /mnt/test
cd /mnt/test

# Test operations
ls -la              # READDIR / READDIRPLUS
cat readme.txt      # READ
echo "test" > new   # CREATE + WRITE
mkdir foo           # MKDIR
rm new              # REMOVE
rmdir foo           # RMDIR
mv file1 file2      # RENAME
ln -s target link   # SYMLINK
ln file1 file2      # LINK (hard link)
```

### Automated Testing

```bash
# Run unit tests
go test ./...

# Run E2E tests (requires NFS client installed)
go test -v -timeout 30m ./test/e2e/...

# Run specific E2E suite
go test -v ./test/e2e -run TestE2E/memory/BasicOperations
```

## Known Limitations

1. **No file locking**: NLM protocol not implemented
2. **No NFSv4**: Only NFSv3 is supported
3. **Limited security**: Basic AUTH_UNIX only, no Kerberos
4. **No caching**: Every operation hits repository
5. **Single-node only**: No distributed/HA support

## References

### Specifications

- [RFC 1813](https://tools.ietf.org/html/rfc1813) - NFS Version 3 Protocol Specification
- [RFC 5531](https://tools.ietf.org/html/rfc5531) - RPC: Remote Procedure Call Protocol Specification
- [RFC 4506](https://tools.ietf.org/html/rfc4506) - XDR: External Data Representation Standard
- [RFC 1057](https://tools.ietf.org/html/rfc1057) - RPC: Remote Procedure Call Protocol (Portmapper)
- [RFC 1094](https://tools.ietf.org/html/rfc1094) - NFS: Network File System Protocol (Version 2)

### Related Projects

- [go-nfs](https://github.com/willscott/go-nfs) - Another NFS implementation in Go
- [FUSE](https://github.com/libfuse/libfuse) - Filesystem in Userspace
