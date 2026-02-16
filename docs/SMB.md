# SMB Implementation

This document details DittoFS's SMB2 implementation, protocol status, and client usage.

## Table of Contents

- [Mounting SMB Shares](#mounting-smb-shares)
- [Protocol Implementation Status](#protocol-implementation-status)
- [Implementation Details](#implementation-details)
- [Authentication](#authentication)
- [Testing SMB Operations](#testing-smb-operations)
- [Troubleshooting](#troubleshooting)

## Mounting SMB Shares

DittoFS uses a configurable port (default 12445) and supports NTLM authentication.

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

### SMB2 Negotiation & Session

| Command | Status | Notes |
|---------|--------|-------|
| NEGOTIATE | ✅ | SMB2 dialect 0x0202 |
| SESSION_SETUP | ✅ | NTLM via SPNEGO |
| LOGOFF | ✅ | |
| TREE_CONNECT | ✅ | Share-level permissions |
| TREE_DISCONNECT | ✅ | |

### SMB2 File Operations

| Command | Status | Notes |
|---------|--------|-------|
| CREATE | ✅ | Files and directories |
| CLOSE | ✅ | |
| FLUSH | ✅ | Flushes cache to content store |
| READ | ✅ | With cache support |
| WRITE | ✅ | With cache support |
| QUERY_INFO | ✅ | Multiple info classes |
| SET_INFO | ✅ | Attributes, timestamps, rename, delete |
| QUERY_DIRECTORY | ✅ | With pagination |
| CHANGE_NOTIFY | ⚠️ | Accepts watches, no async delivery |

### SMB2 Advanced Features

| Feature | Status | Notes |
|---------|--------|-------|
| Compound Requests | ✅ | CREATE+QUERY_INFO+CLOSE |
| Credit Management | ✅ | Adaptive flow control |
| Parallel Requests | ✅ | Per-connection concurrency |
| Byte-Range Locking | ✅ | Shared/exclusive locks |
| Message Signing | ✅ | HMAC-SHA256 |
| Oplocks | ✅ | Level II, Exclusive, Batch |
| Change Notifications | ⚠️ | Registered only (no async delivery) |
| SMB3 Encryption | ❌ | Planned |

**Total**: Core file operations, locking, and oplocks fully implemented

## Implementation Details

### SMB2 Message Flow

1. TCP connection accepted
2. NetBIOS session header parsed
3. SMB2 message decoded
4. Session/tree context validated
5. Command handler dispatched
6. Handler calls metadata/content stores
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

**Session Management** (`internal/protocol/smb/v2/handlers/`)
- `NEGOTIATE`: Protocol version negotiation (SMB2 0x0202)
- `SESSION_SETUP`: NTLM authentication via SPNEGO
- `TREE_CONNECT`: Share access with permission validation

**File Operations** (`internal/protocol/smb/v2/handlers/`)
- `CREATE`: Create/open files and directories
- `READ`: Read file content (with cache support)
- `WRITE`: Write file content (with cache support)
- `CLOSE`: Close file handle and cleanup
- `FLUSH`: Flush cached data to content store
- `QUERY_INFO`: Get file/directory attributes
- `SET_INFO`: Modify attributes, rename, delete
- `QUERY_DIRECTORY`: List directory contents
- `LOCK`: Acquire/release byte-range locks

### Two-Phase Write Pattern

WRITE operations use a two-phase commit pattern:

```go
// 1. Prepare write (validate permissions, get ContentID)
writeOp, err := metadataStore.PrepareWrite(authCtx, handle, newSize)

// 2. Write data to cache or content store
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
    contentStore.WriteAt(contentID, data, offset)  // Sync
}

// Read path
if cacheHit := tryReadFromCache(cache, contentID, offset, length); cacheHit {
    return cacheHit.data
}
return contentStore.ReadAt(contentID, buf, offset)
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

### Byte-Range Locking

DittoFS implements SMB2 byte-range locking per [MS-SMB2] 2.2.26/2.2.27:

#### Lock Types

- **Shared (Read) Locks**: Multiple clients can hold shared locks on overlapping ranges
- **Exclusive (Write) Locks**: Only one client can hold an exclusive lock on a range

#### Lock Behavior

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

#### Lock Enforcement

Locks are enforced on READ/WRITE operations:
- **READ**: Blocked by another session's exclusive lock on overlapping range
- **WRITE**: Blocked by any other session's lock (shared or exclusive) on overlapping range

Same-session locks never block the owning session's I/O operations.

#### Lock Lifetime

Locks are ephemeral (in-memory only) and persist until:
- Explicitly released via LOCK with SMB2_LOCKFLAG_UNLOCK
- File handle is closed (CLOSE command)
- Session disconnects (LOGOFF or connection drop)
- Server restarts (all locks lost)

#### Atomicity Limitations

Per SMB2 specification ([MS-SMB2] 2.2.26):

1. **Unlock operations are NOT rolled back**: If a batch LOCK request includes unlocks and a later lock acquisition fails, the successful unlocks remain in effect.

2. **Lock type changes**: When re-locking an existing range with a different type (shared → exclusive), rollback removes the lock entirely rather than reverting to the original type.

#### Configuration

Locking is automatically enabled with no additional configuration required. Locks are stored in-memory per metadata store instance.

### Opportunistic Locks (Oplocks)

DittoFS implements SMB2 opportunistic locks per [MS-SMB2] 2.2.14, 2.2.23, 2.2.24:

#### Oplock Levels

- **None (0x00)**: No caching allowed
- **Level II (0x01)**: Shared read caching - multiple clients can cache read data
- **Exclusive (0x08)**: Exclusive read/write caching - single client can cache reads and writes
- **Batch (0x09)**: Like Exclusive with handle caching - client can delay close operations

#### How Oplocks Work

1. **Grant**: Client requests oplock level in CREATE request
2. **Cache**: Client caches file data according to granted level
3. **Break**: When another client opens the file, server sends OPLOCK_BREAK notification
4. **Acknowledge**: Original client flushes cached data and acknowledges break

#### Oplock Behavior

```go
// Level II allows multiple readers (first holder tracked)
clientA opens file → granted Level II
clientB opens file (Level II) → granted Level II (coexistence)

// Exclusive/Batch requires break on conflict
clientA opens file → granted Exclusive
clientB opens file → server initiates break to Level II
                   → clientB gets None (must retry after break)
```

**Note**: When an oplock break is initiated, the conflicting client is not granted
an oplock immediately. It must retry after the break acknowledgment.

#### Benefits

- **Reduced network traffic**: Clients cache reads locally
- **Better write performance**: Exclusive oplock allows write caching
- **Handle caching**: Batch oplock reduces CREATE/CLOSE round trips

#### Current Limitations

- **No lease support**: SMB2.1+ leases (0xFF) are downgraded to traditional oplocks
- **In-memory tracking**: Oplock state is lost on server restart
- **No async break delivery**: Oplock break notifications require notifier setup
- **Single holder tracking**: Only tracks one Level II holder (others coexist but aren't tracked)

### Change Notifications (CHANGE_NOTIFY)

DittoFS implements partial CHANGE_NOTIFY support per [MS-SMB2] 2.2.35/2.2.36.

#### Current Status

The implementation accepts CHANGE_NOTIFY requests and tracks pending watches, but **does not deliver async notifications** to clients. This is an MVP implementation with the following characteristics:

- **Watch Registration**: ✅ Clients can register directory watches with completion filters
- **Change Detection**: ✅ CREATE, CLOSE (delete-on-close), and SET_INFO (rename) trigger change events
- **Async Delivery**: ❌ Changes are logged but not delivered asynchronously to clients

#### What Works

```go
// Client opens directory and requests notification
// Server registers watch and returns STATUS_PENDING
CHANGE_NOTIFY request accepted → STATUS_PENDING

// When changes occur (file created, deleted, renamed):
// Server logs the event but doesn't send async response
[DEBUG] CHANGE_NOTIFY: would notify watcher path=/watched-dir fileName=new-file.txt action=ADDED
```

#### Limitations

1. **No Async Response**: Clients wait indefinitely for notifications that won't arrive
2. **Polling Required**: Clients must manually poll (QUERY_DIRECTORY) for changes
3. **Watch Cleanup**: Watches are properly cleaned up when directory handles are closed

#### Completion Filter Support

The following filters are recognized (but not delivered):

| Filter | Value | Description |
|--------|-------|-------------|
| FILE_NOTIFY_CHANGE_FILE_NAME | 0x0001 | File create/delete/rename |
| FILE_NOTIFY_CHANGE_DIR_NAME | 0x0002 | Directory create/delete/rename |
| FILE_NOTIFY_CHANGE_ATTRIBUTES | 0x0004 | Attribute changes |
| FILE_NOTIFY_CHANGE_SIZE | 0x0008 | File size changes |
| FILE_NOTIFY_CHANGE_LAST_WRITE | 0x0010 | Last write time changes |

#### Future Work

Full async notification delivery requires:
1. Connection-level async response infrastructure
2. Message ID tracking for pending requests
3. Proper SMB2 async response framing

## Authentication

### NTLM Authentication

DittoFS implements NTLMv2 authentication with SPNEGO negotiation:

1. Client sends NEGOTIATE with SPNEGO token
2. Server responds with NTLM challenge
3. Client sends SESSION_SETUP with NTLM response
4. Server validates credentials and creates session

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

Resolution order: User explicit → Group permissions → Share default

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

# Run interoperability tests (NFS ↔ SMB)
sudo go test -tags=e2e -v ./test/e2e/ -run TestInterop

# Run specific test
sudo go test -tags=e2e -v ./test/e2e/ -run TestSMBCreateFileWithContent
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

### Operations Timeout

1. Increase timeout in SMB config
2. Check content store connectivity (S3, filesystem)
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

- **Via NFS**: Full support - MKNOD creates, GETATTR returns correct type
- **Via SMB**: Hidden from directory listings entirely

This behavior matches commercial NAS devices (Synology, QNAP) which typically don't expose special files via SMB.

### Symlinks

Symlinks are handled transparently via MFsymlink format:

- **NFS-created symlinks**: Appear as MFsymlink files (1067 bytes) when read via SMB
- **SMB-created symlinks**: MFsymlink files are automatically converted to real symlinks on CLOSE
- Both NFS and SMB clients can follow symlinks correctly

See the plan file for detailed symlink interoperability design.

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

## Roadmap

See [SMB_IMPLEMENTATION_PLAN.md](SMB_IMPLEMENTATION_PLAN.md) for detailed roadmap:

1. **SMBv3 Support**: Encryption, multichannel
2. **Leases**: SMB2.1+ directory leases for better caching
3. **Security Descriptors**: Windows ACLs
4. **Extended Attributes**: xattr support
5. **Kerberos/LDAP/AD**: Enterprise authentication
6. **Blocking Locks**: Wait for lock availability (with timeout)

## References

### Specifications

- [MS-SMB2](https://docs.microsoft.com/en-us/openspecs/windows_protocols/ms-smb2/) - SMB2 Protocol Specification
- [MS-NLMP](https://docs.microsoft.com/en-us/openspecs/windows_protocols/ms-nlmp/) - NTLM Authentication Protocol
- [RFC 4178](https://tools.ietf.org/html/rfc4178) - SPNEGO Protocol
- [MS-ERREF](https://docs.microsoft.com/en-us/openspecs/windows_protocols/ms-erref/) - Windows Error Codes

### Related Projects

- [go-smb2](https://github.com/hirochachacha/go-smb2) - SMB2 client in Go
- [Samba](https://www.samba.org/) - SMB/CIFS implementation for Unix
