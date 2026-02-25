# SMB Implementation

This document details DittoFS's SMB2 implementation, protocol status, client usage, and protocol internals for maintainers and users.

## Table of Contents

- [Protocol Overview](#protocol-overview)
- [Mounting SMB Shares](#mounting-smb-shares)
- [Protocol Implementation Status](#protocol-implementation-status)
- [Implementation Details](#implementation-details)
- [Authentication](#authentication)
- [Byte-Range Locking](#byte-range-locking)
- [Opportunistic Locks](#opportunistic-locks)
- [Change Notifications](#change-notifications)
- [Cross-Protocol Behavior](#cross-protocol-behavior)
- [Testing SMB Operations](#testing-smb-operations)
- [Troubleshooting](#troubleshooting)
- [Known Limitations](#known-limitations)
- [Glossary](#glossary)
- [References](#references)

## Protocol Overview

### What is SMB?

**SMB (Server Message Block)** is a network file sharing protocol originally developed by IBM in 1983 and later extended by Microsoft. It is the native file sharing protocol for Windows and is also known as **CIFS (Common Internet File System)**.

DittoFS implements **SMB2 dialect 0x0202 (SMB 2.0.2)** -- the simplest modern dialect that provides good compatibility without the complexity of SMB3.x features.

### SMB vs NFS: Key Differences

| Aspect | NFS (v3) | SMB2 |
|--------|----------|------|
| **Origin** | Unix (Sun Microsystems, 1984) | Windows (IBM/Microsoft, 1983) |
| **Design** | Stateless, simple operations | Stateful, session-based |
| **Identity** | UID/GID (Unix) | SID (Windows Security ID) |
| **Permissions** | Unix mode bits (rwxrwxrwx) | ACLs (Access Control Lists) |
| **Transport** | TCP (port 2049) | TCP (port 445) |
| **Framing** | RPC record marking | NetBIOS session header |
| **Encoding** | XDR (big-endian) | Custom (little-endian) |
| **Header** | Variable (RPC) | Fixed 64 bytes |
| **Strings** | UTF-8 | UTF-16LE |
| **Flow control** | None (relies on TCP) | Credit-based |

### Conceptual Mapping

| NFS Concept | SMB Equivalent | Notes |
|-------------|----------------|-------|
| Export | Share | Network-accessible directory |
| Mount | Tree Connect | Establishing access to a share |
| File Handle | FileID | Opaque identifier for open file |
| UID/GID | SID | User/group identity |
| Mode bits | Security Descriptor | Permission model |
| LOOKUP | Part of CREATE | SMB combines lookup and open |
| GETATTR | QUERY_INFO | Get file metadata |
| SETATTR | SET_INFO | Set file metadata |
| READDIR | QUERY_DIRECTORY | List directory contents |
| COMMIT | FLUSH | Sync to disk |

### Message Format

Every SMB2 message follows this structure:

```
+------------------------------------------------------------+
|                    NetBIOS Session Header                   |
|                         (4 bytes)                           |
+------------------------------------------------------------+
|                       SMB2 Header                           |
|                        (64 bytes)                           |
+------------------------------------------------------------+
|                      Command Body                           |
|                       (variable)                            |
+------------------------------------------------------------+
```

The **NetBIOS session header** contains a type byte (0x00 for session messages) and a 24-bit big-endian length. The **SMB2 header** is always 64 bytes and includes the protocol magic (`0xFE 'S' 'M' 'B'`), command code, credit charge/grant, session ID, tree ID, message ID, flags, and signature.

### Connection Lifecycle

SMB connections follow a multi-phase setup before file operations can begin:

1. **NEGOTIATE** -- Client and server agree on protocol dialect and capabilities
2. **SESSION_SETUP** -- Client authenticates (NTLM or Kerberos via SPNEGO), receives a SessionID
3. **TREE_CONNECT** -- Client connects to a specific share, receives a TreeID
4. **File Operations** -- CREATE opens a file (returns FileID), then READ/WRITE/CLOSE use that FileID
5. **Cleanup** -- CLOSE releases file handles, TREE_DISCONNECT leaves the share, LOGOFF ends the session

This is fundamentally different from NFS, where each request is independent and carries its own auth context.

## Mounting SMB Shares

DittoFS uses a configurable port (default 12445) and supports NTLM and Kerberos authentication.

### Using dfsctl (Recommended)

The `dfsctl share mount` command handles platform-specific mount options automatically:

```bash
# macOS - Mount to user directory (recommended, no sudo needed)
mkdir -p ~/mnt/dittofs
dfsctl share mount --protocol smb /export ~/mnt/dittofs

# macOS - Mount to system directory (requires sudo)
sudo dfsctl share mount --protocol smb /export /mnt/smb

# Linux - Mount with sudo (owner set to your user automatically)
sudo dfsctl share mount --protocol smb /export /mnt/smb

# Unmount
sudo umount /mnt/smb  # or: diskutil unmount ~/mnt/dittofs (macOS)
```

### Platform-Specific Mount Behavior

#### macOS Security Restriction

macOS has a security restriction where **only the mount owner can access files**, regardless
of Unix permissions. Even with 0777, non-owner users get "Permission denied". Apple confirmed
this is "works as intended".

**How dfsctl handles this**: When you run `sudo dfsctl share mount`, it automatically
uses `sudo -u $SUDO_USER` to mount as your user (not root):

```bash
# Works correctly - mount owned by your user
sudo dfsctl share mount --protocol smb /export /mnt/share
```

**Alternative - mount without sudo** (to user directory):

```bash
mkdir -p ~/mnt/share
dfsctl share mount --protocol smb /export ~/mnt/share
```

#### Linux Behavior

Linux CIFS mount fully supports `uid=` and `gid=` options. When using sudo with `dfsctl`:

- The `SUDO_UID` and `SUDO_GID` environment variables are automatically detected
- Mount options include `uid=<your-uid>,gid=<your-gid>`
- Files appear owned by your user, not root
- Default permissions are `0755` (standard Unix)

```bash
# Files will be owned by your user, not root
sudo dfsctl share mount --protocol smb /export /mnt/smb
ls -la /mnt/smb
# drwxr-xr-x youruser yourgroup ... .
```

### Manual Mount Commands

If you prefer to use native mount commands directly:

#### macOS

```bash
# Using mount_smbfs (built-in)
# Note: -f sets file mode, -d sets directory mode (required for write access with sudo)
sudo mount_smbfs -f 0777 -d 0777 //username:password@localhost:12445/export /mnt/smb

# Mount to home directory (no sudo, user-owned)
mount_smbfs //username:password@localhost:12445/export ~/mnt/smb

# Using open (opens in Finder)
open smb://username:password@localhost:12445/export

# Unmount
sudo umount /mnt/smb
# or
diskutil unmount /mnt/smb
```

#### Linux

```bash
# Using mount.cifs (requires cifs-utils)
# uid/gid options set the owner of mounted files
sudo mount -t cifs //localhost/export /mnt/smb \
    -o port=12445,username=testuser,password=testpass,vers=2.0,uid=$(id -u),gid=$(id -g)

# Unmount
sudo umount /mnt/smb
```

### Using smbclient

```bash
# Interactive client
smbclient //localhost/export -p 12445 -U testuser

# List shares
smbclient -L localhost -p 12445 -U testuser

# One-liner file operations
smbclient //localhost/export -p 12445 -U testuser -c "ls"
smbclient //localhost/export -p 12445 -U testuser -c "get file.txt"
smbclient //localhost/export -p 12445 -U testuser -c "put localfile.txt"
```

## Protocol Implementation Status

### SMB2 Negotiation and Session

| Command | Status | Notes |
|---------|--------|-------|
| NEGOTIATE | Implemented | SMB2 dialect 0x0202 |
| SESSION_SETUP | Implemented | NTLM and Kerberos via SPNEGO |
| LOGOFF | Implemented | |
| TREE_CONNECT | Implemented | Share-level permissions |
| TREE_DISCONNECT | Implemented | |

### SMB2 File Operations

| Command | Status | Notes |
|---------|--------|-------|
| CREATE | Implemented | Files and directories |
| CLOSE | Implemented | |
| FLUSH | Implemented | Flushes cache to payload store |
| READ | Implemented | With cache support |
| WRITE | Implemented | With cache support |
| QUERY_INFO | Implemented | Multiple info classes |
| SET_INFO | Implemented | Attributes, timestamps, rename, delete |
| QUERY_DIRECTORY | Implemented | With pagination |
| CHANGE_NOTIFY | Partial | Accepts watches, no async delivery |

### SMB2 Advanced Features

| Feature | Status | Notes |
|---------|--------|-------|
| Compound Requests | Implemented | CREATE+QUERY_INFO+CLOSE |
| Credit Management | Implemented | Adaptive flow control |
| Parallel Requests | Implemented | Per-connection concurrency |
| Byte-Range Locking | Implemented | Shared/exclusive locks |
| Message Signing | Implemented | HMAC-SHA256 |
| Oplocks | Implemented | Level II, Exclusive, Batch |
| Kerberos Auth | Implemented | Via SPNEGO alongside NTLM |
| Change Notifications | Partial | Registered only (no async delivery) |
| SMB3 Encryption | Not implemented | Planned |

## Implementation Details

### SMB2 Message Flow

1. TCP connection accepted
2. NetBIOS session header parsed
3. SMB2 message decoded
4. Session/tree context validated
5. Command handler dispatched
6. Handler calls metadata/payload stores
7. Response encoded and sent

### Request Processing

```go
// Per-connection parallel request handling
for {
    msg := readSMB2Message(conn)
    go handleRequest(msg) // Concurrent handling
}
```

### Critical Commands

**Session Management** (`internal/adapter/smb/v2/handlers/`)
- `NEGOTIATE`: Protocol version negotiation (SMB2 0x0202)
- `SESSION_SETUP`: NTLM or Kerberos authentication via SPNEGO
- `TREE_CONNECT`: Share access with permission validation

**File Operations** (`internal/adapter/smb/v2/handlers/`)
- `CREATE`: Create/open files and directories
- `READ`: Read file content (with cache support)
- `WRITE`: Write file content (with cache support)
- `CLOSE`: Close file handle and cleanup
- `FLUSH`: Flush cached data to payload store
- `QUERY_INFO`: Get file/directory attributes
- `SET_INFO`: Modify attributes, rename, delete
- `QUERY_DIRECTORY`: List directory contents
- `LOCK`: Acquire/release byte-range locks

### Code Structure

```
NFS Implementation:              SMB Implementation:
internal/adapter/nfs/            internal/adapter/smb/
├── dispatch.go                  ├── dispatch.go
├── rpc/                         ├── header/
│   ├── message.go              │   ├── header.go
│   └── reply.go                │   ├── parser.go
├── xdr/                         │   └── encoder.go
│   ├── reader.go               ├── types/
│   └── writer.go               │   ├── constants.go
├── types/                       │   ├── status.go
│   └── constants.go            │   └── filetime.go
├── mount/handlers/              └── v2/handlers/
│   ├── mnt.go                      ├── handler.go
│   └── export.go                   ├── negotiate.go
└── v3/handlers/                     ├── session_setup.go
    ├── lookup.go                   ├── tree_connect.go
    ├── read.go                     ├── create.go
    ├── write.go                    ├── read.go
    └── ...                         └── ...
```

### Two-Phase Write Pattern

WRITE operations use a two-phase commit pattern:

```go
// 1. Prepare write (validate permissions, get ContentID)
writeOp, err := metadataStore.PrepareWrite(authCtx, handle, newSize)

// 2. Write data to cache or payload store
cache.WriteAt(writeOp.ContentID, data, offset)

// 3. Commit write (update metadata: size, timestamps)
metadataStore.CommitWrite(authCtx, writeOp)
```

### Cache Integration

SMB handlers use the same cache layer as NFS:

```go
// Write path
if cache != nil {
    cache.WriteAt(contentID, data, offset)  // Async
} else {
    payloadStore.WriteAt(contentID, data, offset)  // Sync
}

// Read path
if cacheHit := tryReadFromCache(cache, contentID, offset, length); cacheHit {
    return cacheHit.data
}
return payloadStore.ReadAt(contentID, buf, offset)
```

### Credit Flow Control

SMB2 uses credits for flow control:

```go
// Adaptive credit strategy
type CreditConfig struct {
    MinGrant          uint16  // Minimum credits per response (16)
    MaxGrant          uint16  // Maximum credits per response (8192)
    InitialGrant      uint16  // Initial session credits (256)
    MaxSessionCredits uint32  // Maximum total credits (65535)
}
```

Strategies:
- **Fixed**: Always grant InitialGrant credits
- **Echo**: Grant what client requests (within bounds)
- **Adaptive**: Adjust based on server load (default)

## Authentication

### NTLM Authentication

DittoFS implements NTLMv2 authentication with SPNEGO negotiation:

1. Client sends NEGOTIATE with SPNEGO token
2. Server responds with NTLM challenge
3. Client sends SESSION_SETUP with NTLM response
4. Server validates credentials and creates session

### Kerberos Authentication

DittoFS supports Kerberos authentication via SPNEGO alongside NTLM. When a client presents a Kerberos AP-REQ token in the SPNEGO negotiation, the server validates the ticket using the configured service keytab and maps the Kerberos principal to a control plane user.

Key details:

- **Single round-trip**: Unlike NTLM's multi-step handshake, Kerberos authentication completes in one exchange (AP-REQ/AP-REP)
- **Shared keytab**: The SMB adapter shares the Kerberos keytab with the NFS adapter; the server automatically derives the `cifs/` service principal from the configured `nfs/` principal
- **Principal-to-user mapping**: The client principal name (without realm) is looked up in the control plane user store
- **SPNEGO negotiation**: The server advertises both NTLM and Kerberos OIDs; clients choose based on their configuration

See `test/e2e/smb_kerberos_test.go` for end-to-end Kerberos authentication tests.

### User Configuration

```yaml
# config.yaml
users:
  - username: alice
    password_hash: "$2a$10$..."  # bcrypt hash
    uid: 1001
    gid: 1000
    share_permissions:
      /export: read-write

groups:
  - name: editors
    gid: 1000
    share_permissions:
      /export: read-write

guest:
  enabled: false  # Disable guest access
```

### Permission Levels

- `none`: No access
- `read`: Read-only access
- `read-write`: Full read/write access
- `admin`: Full access (future)

Resolution order: User explicit -> Group permissions -> Share default

## Byte-Range Locking

DittoFS implements SMB2 byte-range locking per [MS-SMB2] 2.2.26/2.2.27.

### Lock Types

- **Shared (Read) Locks**: Multiple clients can hold shared locks on overlapping ranges
- **Exclusive (Write) Locks**: Only one client can hold an exclusive lock on a range

### Lock Behavior

```go
// Lock request processing
for each lockElement in request.Locks {
    if lockElement.Flags & UNLOCK {
        // Release lock - NOT rolled back on batch failure
        store.UnlockFile(handle, sessionID, offset, length)
    } else {
        // Acquire lock - rolled back if later operation fails
        store.LockFile(handle, lock)
        acquiredLocks = append(acquiredLocks, lockElement)
    }
}
```

### Lock Enforcement

Locks are enforced on READ/WRITE operations:
- **READ**: Blocked by another session's exclusive lock on overlapping range
- **WRITE**: Blocked by any other session's lock (shared or exclusive) on overlapping range

Same-session locks never block the owning session's I/O operations.

### Lock Lifetime

Locks are ephemeral (in-memory only) and persist until:
- Explicitly released via LOCK with SMB2_LOCKFLAG_UNLOCK
- File handle is closed (CLOSE command)
- Session disconnects (LOGOFF or connection drop)
- Server restarts (all locks lost)

### Atomicity Limitations

Per SMB2 specification ([MS-SMB2] 2.2.26):

1. **Unlock operations are NOT rolled back**: If a batch LOCK request includes unlocks and a later lock acquisition fails, the successful unlocks remain in effect.

2. **Lock type changes**: When re-locking an existing range with a different type (shared to exclusive), rollback removes the lock entirely rather than reverting to the original type.

### Configuration

Locking is automatically enabled with no additional configuration required. Locks are stored in-memory per metadata store instance.

## Opportunistic Locks

DittoFS implements SMB2 opportunistic locks per [MS-SMB2] 2.2.14, 2.2.23, 2.2.24.

### Oplock Levels

- **None (0x00)**: No caching allowed
- **Level II (0x01)**: Shared read caching -- multiple clients can cache read data
- **Exclusive (0x08)**: Exclusive read/write caching -- single client can cache reads and writes
- **Batch (0x09)**: Like Exclusive with handle caching -- client can delay close operations

### How Oplocks Work

1. **Grant**: Client requests oplock level in CREATE request
2. **Cache**: Client caches file data according to granted level
3. **Break**: When another client opens the file, server sends OPLOCK_BREAK notification
4. **Acknowledge**: Original client flushes cached data and acknowledges break

### Oplock Behavior

```go
// Level II allows multiple readers (first holder tracked)
clientA opens file -> granted Level II
clientB opens file (Level II) -> granted Level II (coexistence)

// Exclusive/Batch requires break on conflict
clientA opens file -> granted Exclusive
clientB opens file -> server initiates break to Level II
                   -> clientB gets None (must retry after break)
```

When an oplock break is initiated, the conflicting client is not granted an oplock immediately. It must retry after the break acknowledgment.

### Benefits

- **Reduced network traffic**: Clients cache reads locally
- **Better write performance**: Exclusive oplock allows write caching
- **Handle caching**: Batch oplock reduces CREATE/CLOSE round trips

### Current Limitations

- **No lease support**: SMB2.1+ leases (0xFF) are downgraded to traditional oplocks
- **In-memory tracking**: Oplock state is lost on server restart
- **No async break delivery**: Oplock break notifications require notifier setup
- **Single holder tracking**: Only tracks one Level II holder (others coexist but are not tracked)

## Change Notifications

DittoFS implements partial CHANGE_NOTIFY support per [MS-SMB2] 2.2.35/2.2.36.

### Current Status

The implementation accepts CHANGE_NOTIFY requests and tracks pending watches, but **does not deliver async notifications** to clients. This is an MVP implementation with the following characteristics:

- **Watch Registration**: Clients can register directory watches with completion filters
- **Change Detection**: CREATE, CLOSE (delete-on-close), and SET_INFO (rename) trigger change events
- **Async Delivery**: Changes are logged but not delivered asynchronously to clients

### What Works

```go
// Client opens directory and requests notification
// Server registers watch and returns STATUS_PENDING
CHANGE_NOTIFY request accepted -> STATUS_PENDING

// When changes occur (file created, deleted, renamed):
// Server logs the event but does not send async response
[DEBUG] CHANGE_NOTIFY: would notify watcher path=/watched-dir fileName=new-file.txt action=ADDED
```

### Limitations

1. **No Async Response**: Clients wait indefinitely for notifications that will not arrive
2. **Polling Required**: Clients must manually poll (QUERY_DIRECTORY) for changes
3. **Watch Cleanup**: Watches are properly cleaned up when directory handles are closed

### Completion Filter Support

The following filters are recognized (but not delivered):

| Filter | Value | Description |
|--------|-------|-------------|
| FILE_NOTIFY_CHANGE_FILE_NAME | 0x0001 | File create/delete/rename |
| FILE_NOTIFY_CHANGE_DIR_NAME | 0x0002 | Directory create/delete/rename |
| FILE_NOTIFY_CHANGE_ATTRIBUTES | 0x0004 | Attribute changes |
| FILE_NOTIFY_CHANGE_SIZE | 0x0008 | File size changes |
| FILE_NOTIFY_CHANGE_LAST_WRITE | 0x0010 | Last write time changes |

### Future Work

Full async notification delivery requires:
1. Connection-level async response infrastructure
2. Message ID tracking for pending requests
3. Proper SMB2 async response framing

## Cross-Protocol Behavior

DittoFS supports cross-protocol access between NFS and SMB. This section documents behavior differences.

### Hidden Files

Hidden files are handled differently between Unix and Windows:

- **Unix convention**: Files starting with `.` are hidden
- **Windows convention**: Files with the Hidden attribute flag are hidden

DittoFS bridges both conventions:
- Dot-prefix files (`.gitignore`, `.DS_Store`) appear with `FILE_ATTRIBUTE_HIDDEN` in SMB listings
- The `Hidden` attribute can also be explicitly set via SMB `SET_INFO` (FileBasicInformation)
- Both conventions are persisted: dot-prefix detection is automatic, explicit Hidden flag is stored in metadata

### Special Files (FIFO, Socket, Device Nodes)

Unix special files (FIFO, socket, block device, character device) have no meaningful representation in SMB:

- **Via NFS**: Full support -- MKNOD creates, GETATTR returns correct type
- **Via SMB**: Hidden from directory listings entirely

This behavior matches commercial NAS devices (Synology, QNAP) which typically do not expose special files via SMB.

### Symlinks

Symlinks are handled transparently via MFsymlink format:

- **NFS-created symlinks**: Appear as MFsymlink files (1067 bytes) when read via SMB
- **SMB-created symlinks**: MFsymlink files are automatically converted to real symlinks on CLOSE
- Both NFS and SMB clients can follow symlinks correctly

## Testing SMB Operations

### Manual Testing

```bash
# Start server with debug logging
DITTOFS_LOGGING_LEVEL=DEBUG ./dfs start

# Mount and test (macOS)
sudo mount_smbfs //testuser:testpass@localhost:12445/export /mnt/smb
cd /mnt/smb

# Test operations
ls -la              # QUERY_DIRECTORY
cat readme.txt      # READ
echo "test" > new   # CREATE + WRITE
mkdir foo           # CREATE (directory)
rm new              # SET_INFO (delete)
rmdir foo           # SET_INFO (delete)
mv file1 file2      # SET_INFO (rename)
```

### Using smbclient

```bash
# Interactive mode
smbclient //localhost/export -p 12445 -U testuser%testpass

smb: \> ls
smb: \> get file.txt
smb: \> put local.txt
smb: \> mkdir newdir
smb: \> rm file.txt
smb: \> rmdir newdir
smb: \> exit
```

### Automated Testing

```bash
# Run SMB E2E tests
sudo go test -tags=e2e -v ./test/e2e/ -run TestSMB

# Run interoperability tests (NFS <-> SMB)
sudo go test -tags=e2e -v ./test/e2e/ -run TestInterop

# Run specific test
sudo go test -tags=e2e -v ./test/e2e/ -run TestSMBCreateFileWithContent

# Run SMB Kerberos authentication tests
sudo go test -tags=e2e -v ./test/e2e/ -run TestSMBKerberos
```

## Troubleshooting

### Mount Fails with "Connection Refused"

1. Verify server is running: `netstat -an | grep 12445`
2. Check firewall rules
3. Try explicit port: `port=12445` in mount options

### Authentication Fails

1. Verify user exists in config
2. Check password hash is valid bcrypt
3. Enable debug logging to see authentication flow
4. Ensure user has share permissions
5. For Kerberos: verify the keytab contains the `cifs/` service principal and the KDC is reachable

### Operations Timeout

1. Increase timeout in SMB config
2. Check payload store connectivity (S3, filesystem)
3. Enable debug logging for detailed timing

### macOS-Specific Issues

```bash
# Clear SMB credential cache
security delete-internet-password -s localhost

# Check for stale mounts
mount | grep smb

# Force unmount
sudo umount -f /mnt/smb
```

### Linux-Specific Issues

```bash
# Install cifs-utils if missing
sudo apt-get install cifs-utils  # Debian/Ubuntu
sudo yum install cifs-utils      # RHEL/CentOS

# Check kernel module
lsmod | grep cifs
```

## Known Limitations

1. **SMB2 only**: SMB1 and SMB3 not supported
2. **No encryption**: SMB3 encryption not supported
3. **No security descriptors**: Windows ACLs not supported
4. **No DFS**: Distributed File System not supported
5. **Single dialect**: Only SMB2 0x0202 negotiated
6. **No Unix special files via SMB**: FIFOs, sockets, and device nodes are hidden from SMB listings
7. **Ephemeral locks and oplocks**: Both byte-range locks and oplocks are in-memory only, lost on server restart
8. **No blocking locks**: Lock requests always fail immediately if conflicting lock exists
9. **No lease support**: SMB2.1+ leases are downgraded to traditional oplocks

## Glossary

| Term | Definition |
|------|------------|
| **ACL** | Access Control List -- Windows permission model |
| **CIFS** | Common Internet File System -- older name for SMB |
| **Credit** | Flow control unit in SMB2 |
| **Dialect** | SMB protocol version (e.g., 0x0202 = SMB 2.0.2) |
| **FileID** | 16-byte handle for open file (8 persistent + 8 volatile) |
| **GUID** | 16-byte globally unique identifier |
| **NetBIOS** | Network Basic Input/Output System -- legacy session layer |
| **NT_STATUS** | Windows error code format |
| **Oplock** | Opportunistic lock -- client caching hint |
| **SessionID** | 64-bit identifier for authenticated session |
| **Share** | Network-accessible folder (like NFS export) |
| **SID** | Security Identifier -- Windows user/group identity |
| **SPNEGO** | Simple and Protected GSSAPI Negotiation Mechanism -- wraps NTLM/Kerberos tokens |
| **TreeID** | 32-bit identifier for share connection |
| **UTF-16LE** | 16-bit Unicode, little-endian byte order |

## References

### Specifications

- [MS-SMB2](https://docs.microsoft.com/en-us/openspecs/windows_protocols/ms-smb2/) - SMB2 Protocol Specification
- [MS-NLMP](https://docs.microsoft.com/en-us/openspecs/windows_protocols/ms-nlmp/) - NTLM Authentication Protocol
- [MS-FSCC](https://docs.microsoft.com/en-us/openspecs/windows_protocols/ms-fscc/) - File System Control Codes
- [MS-ERREF](https://docs.microsoft.com/en-us/openspecs/windows_protocols/ms-erref/) - Windows Error Codes
- [RFC 4178](https://tools.ietf.org/html/rfc4178) - SPNEGO Protocol
- [RFC 1813](https://tools.ietf.org/html/rfc1813) - NFS Version 3 Protocol Specification

### Related Projects

- [go-smb2](https://github.com/hirochachacha/go-smb2) - SMB2 client in Go
- [Samba](https://www.samba.org/) - SMB/CIFS implementation for Unix
