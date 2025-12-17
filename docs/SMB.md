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

### Mount on macOS

```bash
# Using mount_smbfs (built-in)
sudo mount_smbfs //username:password@localhost:12445/export /mnt/smb

# Using open (opens in Finder)
open smb://username:password@localhost:12445/export

# Unmount
sudo umount /mnt/smb
```

### Mount on Linux

```bash
# Using mount.cifs (requires cifs-utils)
sudo mount -t cifs //localhost/export /mnt/smb \
    -o port=12445,username=testuser,password=testpass,vers=2.0

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

### SMB2 Advanced Features

| Feature | Status | Notes |
|---------|--------|-------|
| Compound Requests | ✅ | CREATE+QUERY_INFO+CLOSE |
| Credit Management | ✅ | Adaptive flow control |
| Parallel Requests | ✅ | Per-connection concurrency |
| File Locking | ❌ | Planned |
| Oplocks | ❌ | Planned |
| SMB3 Encryption | ❌ | Planned |

**Total**: Core file operations fully implemented

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
DITTOFS_LOGGING_LEVEL=DEBUG ./dittofs start

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
2. **No file locking**: Oplocks and byte-range locks not implemented
3. **No encryption**: SMB3 encryption not supported
4. **No security descriptors**: Windows ACLs not supported
5. **No DFS**: Distributed File System not supported
6. **Single dialect**: Only SMB2 0x0202 negotiated
7. **No Unix special files via SMB**: FIFOs, sockets, and device nodes are hidden from SMB listings

## Roadmap

See [SMB_IMPLEMENTATION_PLAN.md](SMB_IMPLEMENTATION_PLAN.md) for detailed roadmap:

1. **SMBv3 Support**: Encryption, multichannel
2. **File Locking**: Oplocks, byte-range locks
3. **Security Descriptors**: Windows ACLs
4. **Extended Attributes**: xattr support
5. **Kerberos/LDAP/AD**: Enterprise authentication

## References

### Specifications

- [MS-SMB2](https://docs.microsoft.com/en-us/openspecs/windows_protocols/ms-smb2/) - SMB2 Protocol Specification
- [MS-NLMP](https://docs.microsoft.com/en-us/openspecs/windows_protocols/ms-nlmp/) - NTLM Authentication Protocol
- [RFC 4178](https://tools.ietf.org/html/rfc4178) - SPNEGO Protocol
- [MS-ERREF](https://docs.microsoft.com/en-us/openspecs/windows_protocols/ms-erref/) - Windows Error Codes

### Related Projects

- [go-smb2](https://github.com/hirochachacha/go-smb2) - SMB2 client in Go
- [Samba](https://www.samba.org/) - SMB/CIFS implementation for Unix
