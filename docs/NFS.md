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

The portmapper is disabled by default. Enable it via `dfsctl`:

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

## NFSv4 Directory Delegations

DittoFS supports NFSv4.1 directory delegations (RFC 8881 Section 18.39), allowing clients to cache directory listings and receive change notifications instead of re-issuing READDIR after every mutation.

### Overview

A directory delegation grants a client the right to cache the contents of a directory. While the delegation is held, the server sends CB_NOTIFY callbacks whenever the directory changes, so the client can update its cache without a round-trip READDIR.

### Requesting a Directory Delegation

Clients request directory delegations via the GET_DIR_DELEGATION operation, specifying a notification bitmask indicating which change types they want to receive:

| Notification Type | Value | Trigger |
|-------------------|-------|---------|
| NOTIFY4_CHANGE_CHILD_ATTRS | 0x01 | Child file/directory attributes changed |
| NOTIFY4_CHANGE_DIR_ATTRS | 0x02 | Directory's own attributes changed (mode, owner, size) |
| NOTIFY4_REMOVE_ENTRY | 0x04 | Entry removed from directory (REMOVE, RMDIR) |
| NOTIFY4_ADD_ENTRY | 0x08 | Entry added to directory (CREATE, LINK, OPEN+CREATE) |
| NOTIFY4_RENAME_ENTRY | 0x10 | Entry renamed within directory (RENAME) |

The server may grant the delegation with a subset of the requested notification types.

### How Notifications are Delivered

Notifications are delivered via CB_NOTIFY over the NFSv4.1 backchannel:

1. A directory mutation occurs (CREATE, REMOVE, RENAME, LINK, OPEN+CREATE, SETATTR)
2. The server batches the notification into the delegation's pending queue
3. After a configurable batch window (default 50ms), all pending notifications are flushed as a single CB_NOTIFY callback
4. If the batch queue exceeds 100 entries, an immediate flush is triggered

This batching reduces backchannel traffic when many mutations happen in quick succession (e.g., `tar xf` extracting files).

### Mutation Handler Hooks

Each directory-mutating NFSv4 operation triggers the appropriate notification:

| Operation | Notification Type | Details |
|-----------|-------------------|---------|
| CREATE | NOTIFY4_ADD_ENTRY | Parent directory notified of new entry |
| REMOVE | NOTIFY4_REMOVE_ENTRY | Parent directory notified; if removed entry is a directory with its own delegation, that delegation is immediately revoked |
| RENAME (same dir) | NOTIFY4_RENAME_ENTRY | Single notification with old and new names |
| RENAME (cross dir) | NOTIFY4_RENAME_ENTRY + NOTIFY4_ADD_ENTRY | Source directory gets RENAME, destination directory gets ADD |
| LINK | NOTIFY4_ADD_ENTRY | Target directory notified of new hard link entry |
| OPEN+CREATE | NOTIFY4_ADD_ENTRY | Parent directory notified when OPEN creates a new file |
| SETATTR (on dir) | NOTIFY4_CHANGE_DIR_ATTRS | Only for significant changes (mode, owner, group, size); atime-only changes are filtered |

### Conflict-Based Recall

When a client modifies a directory that another client has delegated:

1. Client B sends a mutation (e.g., CREATE) to a directory delegated to Client A
2. The server detects the conflict via `OriginClientID` in the notification
3. Client A's delegation is recalled via CB_RECALL (non-blocking)
4. Client B's operation proceeds immediately (no waiting for recall completion)
5. If Client A does not return the delegation within the lease period, it is forcibly revoked

### Directory Deletion

When a directory is deleted (REMOVE/RMDIR), any directory delegations on that directory are immediately revoked (not just recalled). Since the directory no longer exists, there is no point in waiting for the client to return the delegation.

### Configuration

Directory delegation settings are managed via `dfsctl adapter settings nfs`:

| Setting | Default | Description |
|---------|---------|-------------|
| `delegations_enabled` | `true` | Enable/disable all delegations (file and directory) |
| `max_delegations` | `0` (unlimited) | Maximum concurrent delegations across all clients |
| `dir_deleg_batch_window_ms` | `50` | Notification batch window in milliseconds |

```bash
# Enable delegations
dfsctl adapter settings nfs --set delegations_enabled=true

# Set maximum delegations
dfsctl adapter settings nfs --set max_delegations=1000

# Adjust batch window (lower = more responsive, higher = less backchannel traffic)
dfsctl adapter settings nfs --set dir_deleg_batch_window_ms=100
```

### Prometheus Metrics

Directory delegation metrics are exposed alongside file delegation metrics with a `type` label:

| Metric | Type | Labels | Description |
|--------|------|--------|-------------|
| `dittofs_nfs_delegations_granted_total` | Counter | `type` (file/directory) | Total delegations granted |
| `dittofs_nfs_delegations_recalled_total` | Counter | `type`, `reason` | Total delegations recalled |
| `dittofs_nfs_delegations_active` | Gauge | `type` (file/directory) | Currently active delegations |
| `dittofs_nfs_dir_notifications_sent_total` | Counter | - | Total CB_NOTIFY batches sent |

### Limitations

- **Ephemeral state**: Directory delegations are lost on server restart (in-memory only)
- **Linux client support**: The Linux NFS client does not currently request directory delegations; this feature is primarily useful for custom NFSv4.1 clients
- **No persistent notification queue**: If the backchannel is unavailable when notifications flush, they are silently dropped

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
