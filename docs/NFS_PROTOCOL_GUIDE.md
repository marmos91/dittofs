# NFS Protocol Guide for DittoFS Maintainers

## Document Purpose

This document explains the core concepts of the NFS (Network File System) protocol for maintainers working on DittoFS. It provides a deep understanding of NFSv3 to help contributors implement new features, fix bugs, and understand the architectural decisions made.

**Target Audience**: DittoFS maintainers and contributors
**Prerequisites**: Basic understanding of network protocols and file systems

---

## Table of Contents

1. [Overview: What is NFS?](#overview-what-is-nfs)
2. [Protocol Architecture](#protocol-architecture)
3. [RPC Foundation](#rpc-foundation)
4. [XDR Encoding](#xdr-encoding)
5. [Mount Protocol](#mount-protocol)
6. [NFS Procedures](#nfs-procedures)
7. [File Handles](#file-handles)
8. [Authentication](#authentication)
9. [Error Handling](#error-handling)
10. [DittoFS Implementation Mapping](#dittofs-implementation-mapping)
11. [Glossary](#glossary)

---

## Overview: What is NFS?

**NFS (Network File System)** is a distributed file system protocol originally developed by Sun Microsystems in 1984. It allows a client to access files over a network as if they were on local storage.

### NFS Version History

| Version | Year | Key Features |
|---------|------|--------------|
| NFSv2 | 1989 | Original version, 32-bit file sizes, UDP only |
| NFSv3 | 1995 | 64-bit file sizes, TCP support, async writes, WCC |
| NFSv4 | 2000 | Stateful, ACLs, compound operations, no mount protocol |
| NFSv4.1 | 2010 | Parallel NFS (pNFS), sessions |
| NFSv4.2 | 2016 | Server-side copy, sparse files |

**DittoFS implements NFSv3** - a mature, well-understood protocol that balances simplicity with functionality. NFSv3 is still widely used in production environments.

### Why NFSv3 Matters for DittoFS

- **Stateless design**: Server doesn't need to track client state between requests
- **Simple operations**: Each procedure is independent and idempotent
- **Wide compatibility**: Supported by all major operating systems
- **Proven reliability**: 30+ years of production use
- **Pure userspace**: Can be implemented without kernel modules

---

## Protocol Architecture

### Layered Protocol Stack

NFS uses a layered architecture with multiple supporting protocols:

```
┌────────────────────────────────────────────────────────────┐
│                    NFS Application                          │
│              (file operations: read, write, etc.)           │
├────────────────────────────────────────────────────────────┤
│               NFSv3 Protocol (Program 100003)               │
│           22 procedures for file system operations          │
├────────────────────────────────────────────────────────────┤
│              Mount Protocol (Program 100005)                │
│        6 procedures for mounting exported directories       │
├────────────────────────────────────────────────────────────┤
│                 RPC (Remote Procedure Call)                 │
│              Message framing, authentication                │
├────────────────────────────────────────────────────────────┤
│                 XDR (External Data Representation)          │
│                 Binary encoding/decoding                    │
├────────────────────────────────────────────────────────────┤
│                          TCP/IP                             │
│                    Transport layer                          │
└────────────────────────────────────────────────────────────┘
```

### Request/Response Flow

```
┌──────────────┐                         ┌──────────────┐
│    Client    │                         │    Server    │
└──────────────┘                         └──────────────┘
       │                                        │
       │  1. TCP Connection (port 12049)        │
       │ ─────────────────────────────────────► │
       │                                        │
       │  2. MOUNT /export ─────────────────►   │
       │     [RPC Call: Program 100005, v3]     │
       │                                        │
       │  ◄───────────────── Root file handle   │
       │     [RPC Reply: OK + handle + auth]    │
       │                                        │
       │  3. LOOKUP "file.txt" ──────────────►  │
       │     [NFS Call: handle + name]          │
       │                                        │
       │  ◄───────────────── File handle        │
       │     [NFS Reply: OK + handle + attrs]   │
       │                                        │
       │  4. READ (offset=0, count=4096) ────►  │
       │     [NFS Call: handle + offset + len]  │
       │                                        │
       │  ◄───────────────── Data + EOF flag    │
       │     [NFS Reply: OK + attrs + data]     │
       │                                        │
       │  5. UMOUNT /export ─────────────────►  │
       │                                        │
       └────────────────────────────────────────┘
```

### Key Design Principles

**Stateless Operations**:
- Each NFS request contains all information needed to process it
- Server can restart without affecting client state
- File handles are persistent identifiers, not session-bound

**Idempotent Procedures**:
- Most operations can be safely retried if response is lost
- Server may cache recent responses to handle duplicate requests
- Exceptions: CREATE with exclusive mode requires special handling

**Weak Cache Consistency (WCC)**:
- Response includes pre-operation and post-operation attributes
- Clients detect concurrent modifications by other clients
- Enables client-side caching while maintaining consistency

---

## RPC Foundation

### Overview

NFS uses **ONC RPC (Open Network Computing Remote Procedure Call)**, defined in RFC 5531. RPC provides:
- Message framing over TCP
- Procedure identification (program, version, procedure numbers)
- Authentication framework
- Request/reply matching via transaction IDs

### RPC Message Structure

Every RPC message has this structure:

```
┌────────────────────────────────────────────────────────────┐
│                 RPC Record Fragment Header                  │
│                         (4 bytes)                           │
├────────────────────────────────────────────────────────────┤
│                    RPC Call/Reply Header                    │
│                        (variable)                           │
├────────────────────────────────────────────────────────────┤
│                    Procedure Arguments                      │
│                   or Results (variable)                     │
└────────────────────────────────────────────────────────────┘
```

### Fragment Header

TCP connections use **record marking** to frame RPC messages:

```
Bit 31:     Last fragment flag (1 = last, 0 = more fragments follow)
Bits 0-30:  Fragment length in bytes
```

Example: `0x80000064` means:
- High bit set (last fragment)
- Length = 0x64 = 100 bytes

### RPC Call Header

```
┌───────────────┬────────────────────────────────────────────┐
│ Offset        │ Field Description                          │
├───────────────┼────────────────────────────────────────────┤
│ 0-3 (4 bytes) │ XID - Transaction ID (echoed in reply)     │
│ 4-7           │ Message Type (0 = CALL, 1 = REPLY)         │
│ 8-11          │ RPC Version (must be 2)                    │
│ 12-15         │ Program Number                             │
│               │   - 100003: NFS                            │
│               │   - 100005: Mount                          │
│ 16-19         │ Program Version                            │
│               │   - 3 for NFSv3                            │
│               │   - 3 for Mount v3                         │
│ 20-23         │ Procedure Number (0-21 for NFSv3)          │
│ 24+           │ Credentials (OpaqueAuth structure)         │
│ variable      │ Verifier (OpaqueAuth structure)            │
└───────────────┴────────────────────────────────────────────┘
```

### RPC Reply Header

```
┌───────────────┬────────────────────────────────────────────┐
│ Offset        │ Field Description                          │
├───────────────┼────────────────────────────────────────────┤
│ 0-3           │ XID - Transaction ID (matches call)        │
│ 4-7           │ Message Type (1 = REPLY)                   │
│ 8-11          │ Reply State                                │
│               │   - 0: MSG_ACCEPTED                        │
│               │   - 1: MSG_DENIED                          │
│ 12+           │ Verifier (OpaqueAuth structure)            │
│ variable      │ Accept/Reject Status                       │
│               │   For MSG_ACCEPTED:                        │
│               │   - 0: SUCCESS                             │
│               │   - 1: PROG_UNAVAIL                        │
│               │   - 2: PROG_MISMATCH                       │
│               │   - 3: PROC_UNAVAIL                        │
│               │   - 4: GARBAGE_ARGS                        │
│               │   - 5: SYSTEM_ERR                          │
└───────────────┴────────────────────────────────────────────┘
```

### DittoFS Implementation

In DittoFS, RPC handling is in `internal/protocol/nfs/rpc/`:

```go
// internal/protocol/nfs/rpc/message.go
type RPCCallMessage struct {
    XID        uint32      // Transaction ID
    MsgType    uint32      // 0 = CALL
    RPCVersion uint32      // Must be 2
    Program    uint32      // 100003 (NFS) or 100005 (Mount)
    Version    uint32      // Protocol version
    Procedure  uint32      // Operation number
    Cred       OpaqueAuth  // Authentication credentials
    Verf       OpaqueAuth  // Authentication verifier
}
```

---

## XDR Encoding

### Overview

**XDR (External Data Representation)** is defined in RFC 4506. It provides a canonical way to encode data types for network transmission, ensuring interoperability between different systems.

### Key XDR Rules

1. **Big-endian byte order** for all integers
2. **4-byte alignment** for all data items
3. **Padding** with zeros to reach 4-byte boundaries

### Basic Types

| Type | Size | Description |
|------|------|-------------|
| int | 4 bytes | Signed 32-bit integer |
| unsigned int | 4 bytes | Unsigned 32-bit integer |
| hyper | 8 bytes | Signed 64-bit integer |
| unsigned hyper | 8 bytes | Unsigned 64-bit integer |
| bool | 4 bytes | Boolean (0 = false, 1 = true) |
| float | 4 bytes | IEEE 754 single precision |
| double | 8 bytes | IEEE 754 double precision |

### Variable-Length Data (Opaque)

Variable-length data is encoded as:

```
┌────────────────┬───────────────────┬────────────────────┐
│ Length (4 bytes)│ Data (N bytes)    │ Padding (0-3 bytes)│
└────────────────┴───────────────────┴────────────────────┘
```

Padding formula: `(4 - (length % 4)) % 4`

Examples:
- Length 5: padding = 3 (total 12 bytes)
- Length 8: padding = 0 (total 12 bytes)
- Length 12: padding = 0 (total 16 bytes)

### Strings

Strings are encoded the same as opaque data:
- 4-byte length prefix
- UTF-8 encoded string data
- Padding to 4-byte boundary

```
Example: "hello" (5 bytes)

00 00 00 05    Length = 5
68 65 6C 6C    "hell"
6F 00 00 00    "o" + 3 padding bytes
```

### Optional (Discriminated Union)

Optional values use a boolean discriminator:

```
┌────────────────┬───────────────────────────────────────────┐
│ Present (4 B)  │ Value (if present = 1)                    │
└────────────────┴───────────────────────────────────────────┘
```

### DittoFS XDR Implementation

XDR encoding/decoding is in `internal/protocol/nfs/xdr/`:

```go
// internal/protocol/nfs/xdr/decode.go
func DecodeOpaque(reader io.Reader) ([]byte, error) {
    // Read 4-byte length
    var length uint32
    binary.Read(reader, binary.BigEndian, &length)

    // Read data
    data := make([]byte, length)
    io.ReadFull(reader, data)

    // Skip padding
    padding := (4 - (length % 4)) % 4
    // ... skip padding bytes

    return data, nil
}
```

---

## Mount Protocol

### Overview

The **Mount protocol** (Program 100005, Version 3) is a companion protocol to NFS. It's used to:
- Obtain the initial file handle for an exported directory
- List available exports
- Track active mounts

### Mount Procedures

| Proc | Name | Purpose |
|------|------|---------|
| 0 | NULL | Connectivity test (no-op) |
| 1 | MNT | Mount an export, returns root file handle |
| 2 | DUMP | List active mounts |
| 3 | UMNT | Unmount an export |
| 4 | UMNTALL | Unmount all exports for this client |
| 5 | EXPORT | List available exports |

### MNT Procedure (Most Important)

The MNT procedure is the entry point for NFS access:

**Request:**
```
┌────────────────────────────────────────────────────────────┐
│ dirpath (string)                                           │
│ Example: "/export"                                         │
└────────────────────────────────────────────────────────────┘
```

**Response:**
```
┌────────────────┬───────────────────────────────────────────┐
│ Status (4 B)   │ 0 = OK, or error code                     │
├────────────────┼───────────────────────────────────────────┤
│ (if OK)        │                                           │
│ File Handle    │ Opaque root handle (up to 64 bytes)       │
│ Auth Flavors   │ List of supported auth methods            │
└────────────────┴───────────────────────────────────────────┘
```

### Mount Status Codes

| Code | Name | Description |
|------|------|-------------|
| 0 | MNT_OK | Success |
| 1 | MNT_EPERM | Permission denied |
| 2 | MNT_ENOENT | Export path not found |
| 5 | MNT_EIO | I/O error |
| 13 | MNT_EACCES | Access denied |
| 20 | MNT_ENOTDIR | Not a directory |
| 22 | MNT_EINVAL | Invalid argument |
| 63 | MNT_ENAMETOOLONG | Path too long |
| 10004 | MNT_ENOTSUPP | Not supported |
| 10006 | MNT_ESERVERFAULT | Server error |

### DittoFS Mount Implementation

Mount handlers are in `internal/protocol/nfs/mount/handlers/`:

```go
// internal/protocol/nfs/mount/handlers/mount.go
func (h *Handler) Mount(
    ctx *MountHandlerContext,
    req *MountRequest,
) (*MountResponse, error) {
    // 1. Check share exists in registry
    if !h.Registry.ShareExists(req.DirPath) {
        return &MountResponse{Status: MountErrNoEnt}, nil
    }

    // 2. Record the mount
    h.Registry.RecordMount(clientIP, req.DirPath, time.Now().Unix())

    // 3. Get root handle from registry
    rootHandle, err := h.Registry.GetRootHandle(req.DirPath)

    // 4. Return success with handle and auth flavors
    return &MountResponse{
        Status:      MountOK,
        FileHandle:  rootHandle,
        AuthFlavors: []int32{1}, // AUTH_UNIX
    }, nil
}
```

---

## NFS Procedures

### Procedure Numbers

NFSv3 defines 22 procedures (0-21):

| Proc | Name | Description |
|------|------|-------------|
| 0 | NULL | No-op, connectivity test |
| 1 | GETATTR | Get file attributes |
| 2 | SETATTR | Set file attributes |
| 3 | LOOKUP | Look up file name in directory |
| 4 | ACCESS | Check access permissions |
| 5 | READLINK | Read symbolic link target |
| 6 | READ | Read file data |
| 7 | WRITE | Write file data |
| 8 | CREATE | Create regular file |
| 9 | MKDIR | Create directory |
| 10 | SYMLINK | Create symbolic link |
| 11 | MKNOD | Create special device |
| 12 | REMOVE | Delete file |
| 13 | RMDIR | Delete directory |
| 14 | RENAME | Rename file/directory |
| 15 | LINK | Create hard link |
| 16 | READDIR | Read directory entries |
| 17 | READDIRPLUS | Read directory entries with attributes |
| 18 | FSSTAT | Get file system statistics |
| 19 | FSINFO | Get file system info (max sizes, etc.) |
| 20 | PATHCONF | Get POSIX path configuration |
| 21 | COMMIT | Commit cached data to stable storage |

### Core Procedures in Detail

#### LOOKUP (Procedure 3)

Resolves a name to a file handle within a directory.

```
Request:
┌────────────────┬───────────────────────────────────────────┐
│ Dir Handle     │ File handle of parent directory           │
│ Name           │ Name to look up (string)                  │
└────────────────┴───────────────────────────────────────────┘

Response (OK):
┌────────────────┬───────────────────────────────────────────┐
│ Status         │ NFS3_OK (0)                               │
│ Object Handle  │ File handle for the found file            │
│ Object Attrs   │ Post-op attributes (optional)             │
│ Dir Attrs      │ Post-op dir attributes (optional)         │
└────────────────┴───────────────────────────────────────────┘
```

#### READ (Procedure 6)

Reads data from a file.

```
Request:
┌────────────────┬───────────────────────────────────────────┐
│ File Handle    │ File handle of file to read               │
│ Offset         │ Byte offset (64-bit)                      │
│ Count          │ Number of bytes to read (32-bit)          │
└────────────────┴───────────────────────────────────────────┘

Response (OK):
┌────────────────┬───────────────────────────────────────────┐
│ Status         │ NFS3_OK (0)                               │
│ File Attrs     │ Post-op attributes (optional)             │
│ Count          │ Bytes actually read                       │
│ EOF            │ End of file reached (boolean)             │
│ Data           │ File data (opaque)                        │
└────────────────┴───────────────────────────────────────────┘
```

#### WRITE (Procedure 7)

Writes data to a file.

```
Request:
┌────────────────┬───────────────────────────────────────────┐
│ File Handle    │ File handle of file to write              │
│ Offset         │ Byte offset (64-bit)                      │
│ Count          │ Number of bytes to write                  │
│ Stable         │ Stability level (0=UNSTABLE, 1=DATA_SYNC, │
│                │                  2=FILE_SYNC)             │
│ Data           │ Data to write (opaque)                    │
└────────────────┴───────────────────────────────────────────┘

Response (OK):
┌────────────────┬───────────────────────────────────────────┐
│ Status         │ NFS3_OK (0)                               │
│ WCC Data       │ Weak cache consistency data               │
│ Count          │ Bytes actually written                    │
│ Committed      │ Actual stability achieved                 │
│ Write Verifier │ Server-unique identifier (changes on      │
│                │ restart)                                  │
└────────────────┴───────────────────────────────────────────┘
```

**Write Stability Levels:**

| Level | Name | Description |
|-------|------|-------------|
| 0 | UNSTABLE | Data may be cached; requires COMMIT |
| 1 | DATA_SYNC | Data committed, metadata may be cached |
| 2 | FILE_SYNC | Both data and metadata committed |

#### CREATE (Procedure 8)

Creates a new regular file.

```
Request:
┌────────────────┬───────────────────────────────────────────┐
│ Dir Handle     │ Directory to create file in               │
│ Name           │ Name of new file                          │
│ Mode           │ Creation mode:                            │
│                │   0 = UNCHECKED (create or truncate)      │
│                │   1 = GUARDED (fail if exists)            │
│                │   2 = EXCLUSIVE (idempotent create)       │
│ Attributes     │ Initial attributes (sattr3)               │
│ (EXCLUSIVE)    │ Verifier for exclusive create             │
└────────────────┴───────────────────────────────────────────┘
```

### WCC (Weak Cache Consistency)

WCC data helps clients maintain cache coherency:

```
WCC Data Structure:
┌────────────────┬───────────────────────────────────────────┐
│ Pre-op Attrs   │ Attributes before operation (optional)    │
│   Size         │ File size before                          │
│   Mtime        │ Modification time before                  │
│   Ctime        │ Change time before                        │
├────────────────┼───────────────────────────────────────────┤
│ Post-op Attrs  │ Attributes after operation (optional)     │
│   (Full fattr3)│ Complete file attributes                  │
└────────────────┴───────────────────────────────────────────┘
```

Clients use WCC to:
1. Detect if their cached attributes are stale
2. Update cache with new attributes after operations
3. Detect concurrent modifications by other clients

---

## File Handles

### Overview

**File handles** are opaque identifiers that uniquely identify files and directories. They are:
- Generated by the server
- Opaque to clients (clients don't interpret them)
- Persistent across server restarts (for production servers)
- Maximum 64 bytes per RFC 1813

### File Handle Format in DittoFS

DittoFS encodes share and file information in handles:

```
┌────────────────────────────────────────────────────────────┐
│ Share-specific prefix + File identifier                    │
│ (Format varies by metadata store implementation)           │
└────────────────────────────────────────────────────────────┘
```

Different metadata stores use different handle formats:
- **Memory store**: In-memory IDs (ephemeral)
- **BadgerDB**: Path-based handles (persistent)
- **PostgreSQL**: Share name + UUID (distributed)

### File Handle Operations

```go
// internal/protocol/nfs/xdr/filehandle.go

// Extract file ID (inode number) from handle
func ExtractFileID(handle metadata.FileHandle) uint64 {
    return metadata.HandleToINode(handle)
}

// Decode handle from NFS request
func DecodeFileHandleFromRequest(data []byte) (metadata.FileHandle, error) {
    // Read XDR opaque data
    handleBytes, err := DecodeOpaque(reader)

    // Validate length (max 64 bytes per RFC 1813)
    if len(handleBytes) > 64 {
        return nil, fmt.Errorf("handle too long")
    }

    return metadata.FileHandle(handleBytes), nil
}
```

### Stale Handles

When a handle becomes invalid (file deleted, server restarted with ephemeral storage), the server returns `NFS3ERR_STALE`. Clients should discard cached information and re-lookup the file.

---

## Authentication

### Authentication Flavors

NFS uses RPC authentication flavors:

| Flavor | Value | Description |
|--------|-------|-------------|
| AUTH_NULL | 0 | No authentication |
| AUTH_UNIX | 1 | Unix UID/GID credentials |
| AUTH_SHORT | 2 | Short-hand credential |
| AUTH_DES | 3 | DES encryption (deprecated) |

### AUTH_UNIX Format

The most common authentication type for NFS:

```
┌────────────────┬───────────────────────────────────────────┐
│ Stamp          │ Arbitrary client ID (4 bytes)             │
│ Machine Name   │ Client hostname (string)                  │
│ UID            │ User ID (4 bytes)                         │
│ GID            │ Primary group ID (4 bytes)                │
│ GIDs           │ Supplementary group IDs (array, max 16)   │
└────────────────┴───────────────────────────────────────────┘
```

### DittoFS Authentication

Authentication is extracted in `internal/protocol/nfs/rpc/auth.go`:

```go
type UnixAuth struct {
    Stamp       uint32   // Client-generated identifier
    MachineName string   // Client hostname
    UID         uint32   // Effective user ID
    GID         uint32   // Effective group ID
    GIDs        []uint32 // Supplementary groups (max 16)
}

func ParseUnixAuth(body []byte) (*UnixAuth, error) {
    auth := &UnixAuth{}
    reader := bytes.NewReader(body)

    binary.Read(reader, binary.BigEndian, &auth.Stamp)
    // ... parse machine name
    binary.Read(reader, binary.BigEndian, &auth.UID)
    binary.Read(reader, binary.BigEndian, &auth.GID)
    // ... parse supplementary GIDs

    return auth, nil
}
```

### Security Note

AUTH_UNIX credentials are **not cryptographically secured**. They can be easily spoofed. For production deployments, consider:
- Running on trusted networks only
- Using Kerberos (NFSv4)
- VPN or network-level encryption
- IP-based access controls

---

## Error Handling

### NFS Status Codes

| Code | Name | Description |
|------|------|-------------|
| 0 | NFS3_OK | Success |
| 1 | NFS3ERR_PERM | Not owner |
| 2 | NFS3ERR_NOENT | No such file/directory |
| 5 | NFS3ERR_IO | I/O error |
| 13 | NFS3ERR_ACCES | Permission denied |
| 17 | NFS3ERR_EXIST | File exists |
| 20 | NFS3ERR_NOTDIR | Not a directory |
| 21 | NFS3ERR_ISDIR | Is a directory |
| 22 | NFS3ERR_INVAL | Invalid argument |
| 27 | NFS3ERR_FBIG | File too large |
| 28 | NFS3ERR_NOSPC | No space on device |
| 30 | NFS3ERR_ROFS | Read-only file system |
| 63 | NFS3ERR_NAMETOOLONG | Name too long |
| 66 | NFS3ERR_NOTEMPTY | Directory not empty |
| 70 | NFS3ERR_STALE | Stale file handle |
| 10001 | NFS3ERR_BADHANDLE | Invalid file handle |
| 10002 | NFS3ERR_NOT_SYNC | Update sync mismatch |
| 10004 | NFS3ERR_NOTSUPP | Operation not supported |

### Error Mapping in DittoFS

Internal errors are mapped to NFS status codes:

```go
// pkg/store/metadata/errors.go
var (
    ErrNotDirectory = ExportError{Code: NFS3ErrNotDir}
    ErrNoEntity     = ExportError{Code: NFS3ErrNoEnt}
    ErrAccess       = ExportError{Code: NFS3ErrAcces}
    ErrExist        = ExportError{Code: NFS3ErrExist}
    ErrNotEmpty     = ExportError{Code: NFS3ErrNotEmpty}
)
```

---

## DittoFS Implementation Mapping

### Code Structure

```
dittofs/
├── pkg/adapter/nfs/
│   ├── nfs_adapter.go         # NFS adapter implementing Adapter interface
│   ├── nfs_connection.go      # Connection handling
│   └── config.go              # NFS-specific configuration
│
└── internal/protocol/nfs/
    ├── dispatch.go            # Procedure routing
    ├── bufpool.go             # Buffer pooling for performance
    ├── rpc/
    │   ├── message.go         # RPC message structures
    │   ├── parser.go          # RPC parsing and reply building
    │   ├── auth.go            # Authentication parsing
    │   └── constants.go       # RPC constants
    ├── xdr/
    │   ├── decode.go          # XDR decoding helpers
    │   ├── encode.go          # XDR encoding helpers
    │   ├── attributes.go      # File attribute encoding
    │   ├── filehandle.go      # File handle utilities
    │   └── time.go            # NFS time format conversion
    ├── types/
    │   ├── constants.go       # NFSv3 constants
    │   └── types.go           # NFSv3 type definitions
    ├── mount/handlers/
    │   ├── mount.go           # MNT procedure
    │   ├── umount.go          # UMNT procedure
    │   ├── export.go          # EXPORT procedure
    │   ├── dump.go            # DUMP procedure
    │   └── constants.go       # Mount protocol constants
    └── v3/handlers/
        ├── null.go            # NULL procedure
        ├── getattr.go         # GETATTR procedure
        ├── setattr.go         # SETATTR procedure
        ├── lookup.go          # LOOKUP procedure
        ├── access.go          # ACCESS procedure
        ├── read.go            # READ procedure
        ├── write.go           # WRITE procedure
        ├── create.go          # CREATE procedure
        ├── mkdir.go           # MKDIR procedure
        ├── remove.go          # REMOVE procedure
        ├── rmdir.go           # RMDIR procedure
        ├── rename.go          # RENAME procedure
        ├── readdir.go         # READDIR procedure
        ├── readdirplus.go     # READDIRPLUS procedure
        ├── commit.go          # COMMIT procedure
        └── ...                # Other procedures
```

### Dispatch Flow

```go
// internal/protocol/nfs/dispatch.go

// NFS dispatch table - maps procedure numbers to handlers
var NfsDispatchTable = map[uint32]*nfsProcedure{
    types.NFSProcNull:        {Name: "NULL",    Handler: handleNFSNull},
    types.NFSProcGetAttr:     {Name: "GETATTR", Handler: handleNFSGetAttr},
    types.NFSProcSetAttr:     {Name: "SETATTR", Handler: handleNFSSetAttr},
    types.NFSProcLookup:      {Name: "LOOKUP",  Handler: handleNFSLookup},
    types.NFSProcRead:        {Name: "READ",    Handler: handleNFSRead},
    types.NFSProcWrite:       {Name: "WRITE",   Handler: handleNFSWrite},
    // ... all 22 procedures
}

// Handler context carries request state
type NFSHandlerContext struct {
    Context    context.Context  // For cancellation
    ClientAddr string           // Client IP:port
    Share      string           // Share name from handle
    AuthFlavor uint32           // AUTH_NULL or AUTH_UNIX
    UID        *uint32          // User ID (if AUTH_UNIX)
    GID        *uint32          // Group ID (if AUTH_UNIX)
    GIDs       []uint32         // Supplementary groups
}
```

### Handler Pattern

Each procedure follows the same pattern:

```go
// Example: internal/protocol/nfs/v3/handlers/read.go

func (h *Handler) Read(
    ctx *NFSHandlerContext,
    req *ReadRequest,
) (*ReadResponse, error) {
    // 1. Check context cancellation
    if ctx.isContextCancelled() {
        return nil, ctx.Context.Err()
    }

    // 2. Validate request
    if err := validateReadRequest(req); err != nil {
        return &ReadResponse{Status: err.nfsStatus}, nil
    }

    // 3. Get stores from registry
    metadataStore, err := h.getMetadataStore(ctx)
    contentStore, err := h.getContentStore(ctx)

    // 4. Perform operation
    file, status, err := h.getFileOrError(ctx, metadataStore, handle, ...)
    data, n, err := contentStore.ReadAt(ctx, file.ContentID, ...)

    // 5. Build response
    return &ReadResponse{
        Status: types.NFS3OK,
        Attr:   nfsAttr,
        Count:  uint32(n),
        Eof:    eof,
        Data:   data,
    }, nil
}
```

### XDR Encoding/Decoding

```go
// Request decoding (read.go)
func DecodeReadRequest(data []byte) (*ReadRequest, error) {
    reader := bytes.NewReader(data)

    // Read handle (opaque)
    var handleLen uint32
    binary.Read(reader, binary.BigEndian, &handleLen)
    handle := make([]byte, handleLen)
    binary.Read(reader, binary.BigEndian, &handle)
    // Skip padding

    // Read offset (8 bytes)
    var offset uint64
    binary.Read(reader, binary.BigEndian, &offset)

    // Read count (4 bytes)
    var count uint32
    binary.Read(reader, binary.BigEndian, &count)

    return &ReadRequest{Handle: handle, Offset: offset, Count: count}, nil
}

// Response encoding (read.go)
func (resp *ReadResponse) Encode() ([]byte, error) {
    buf := bytes.NewBuffer(...)

    // Write status
    binary.Write(buf, binary.BigEndian, resp.Status)

    // Write optional attributes
    xdr.EncodeOptionalFileAttr(buf, resp.Attr)

    if resp.Status == types.NFS3OK {
        // Write count, EOF, data
        binary.Write(buf, binary.BigEndian, resp.Count)
        binary.Write(buf, binary.BigEndian, boolToUint32(resp.Eof))
        xdr.WriteXDROpaque(buf, resp.Data)
    }

    return buf.Bytes(), nil
}
```

---

## Glossary

| Term | Definition |
|------|------------|
| **AUTH_NULL** | No authentication flavor (flavor 0) |
| **AUTH_UNIX** | Unix-style authentication with UID/GID (flavor 1) |
| **Cookie** | Opaque value used for directory iteration (READDIR) |
| **EOF** | End of file indicator in READ responses |
| **Export** | A directory shared via NFS (like SMB share) |
| **File Handle** | Opaque identifier for a file/directory (max 64 bytes) |
| **ftype3** | File type enum (regular, directory, symlink, etc.) |
| **FSID** | File system identifier |
| **nfstime3** | NFS time format (seconds + nanoseconds) |
| **RPC** | Remote Procedure Call - foundation protocol |
| **sattr3** | Set attributes structure (for SETATTR, CREATE) |
| **Stale Handle** | A handle that is no longer valid |
| **Verifier** | Server-unique value that changes on restart |
| **WCC** | Weak Cache Consistency data (pre/post attributes) |
| **XDR** | External Data Representation (encoding format) |
| **XID** | Transaction ID for matching requests/replies |

---

## References

- [RFC 1813 - NFS Version 3 Protocol Specification](https://tools.ietf.org/html/rfc1813)
- [RFC 5531 - RPC: Remote Procedure Call Protocol Specification Version 2](https://tools.ietf.org/html/rfc5531)
- [RFC 4506 - XDR: External Data Representation Standard](https://tools.ietf.org/html/rfc4506)
- [DittoFS Architecture Documentation](ARCHITECTURE.md)
- [DittoFS NFS Client Usage](NFS.md)
