# SMB Protocol Guide for DittoFS Maintainers

## Document Purpose

This document explains the core concepts of the SMB (Server Message Block) protocol for maintainers working on DittoFS. It provides comparisons with NFS to leverage existing knowledge and help understand the implementation choices made.

**Target Audience**: DittoFS maintainers and contributors
**Prerequisites**: Basic understanding of network filesystems and the NFS implementation in DittoFS

---

## Table of Contents

1. [Overview: What is SMB?](#overview-what-is-smb)
2. [SMB vs NFS: Key Differences](#smb-vs-nfs-key-differences)
3. [Protocol Architecture](#protocol-architecture)
4. [Connection Lifecycle](#connection-lifecycle)
5. [Core Concepts](#core-concepts)
6. [Commands and Operations](#commands-and-operations)
7. [Error Handling](#error-handling)
8. [DittoFS Implementation Mapping](#dittofs-implementation-mapping)
9. [Glossary](#glossary)

---

## Overview: What is SMB?

**SMB (Server Message Block)** is a network file sharing protocol originally developed by IBM in 1983 and later extended by Microsoft. It's the native file sharing protocol for Windows and is also known as **CIFS (Common Internet File System)**.

### SMB Version History

| Version | Year | Key Features |
|---------|------|--------------|
| SMB1 | 1983 | Original protocol, deprecated due to security issues |
| SMB2.0 | 2006 | Complete redesign, fewer commands, better performance |
| SMB2.1 | 2010 | Opportunistic locking improvements |
| SMB3.0 | 2012 | Encryption, multichannel, transparent failover |
| SMB3.1.1 | 2015 | Pre-authentication integrity, encryption improvements |

**DittoFS implements SMB2 dialect 0x0202 (SMB 2.0.2)** - the simplest modern dialect that provides good compatibility without the complexity of SMB3.x features.

### Why SMB Matters for DittoFS

- **Windows native support**: Mount shares without installing additional software
- **macOS Finder support**: Native file browsing via SMB
- **Cross-platform**: smbclient available on Linux
- **Enterprise environments**: Active Directory integration (future)

---

## SMB vs NFS: Key Differences

Understanding the differences helps when working on both protocol adapters:

### Philosophy

| Aspect | NFS | SMB |
|--------|-----|-----|
| **Origin** | Unix (Sun Microsystems, 1984) | Windows (IBM/Microsoft, 1983) |
| **Design** | Stateless (v3), simple operations | Stateful, session-based |
| **Identity** | UID/GID (Unix) | SID (Windows Security ID) |
| **Permissions** | Unix mode bits (rwxrwxrwx) | ACLs (Access Control Lists) |
| **Path separator** | Forward slash `/` | Backslash `\` |
| **Strings** | UTF-8 | UTF-16LE (Little Endian) |

### Protocol Structure

| Aspect | NFS (v3) | SMB2 |
|--------|----------|------|
| **Transport** | TCP (port 2049) | TCP (port 445) |
| **Framing** | RPC record marking | NetBIOS session header |
| **Encoding** | XDR (big-endian) | Custom (little-endian) |
| **Header size** | Variable (RPC) | Fixed 64 bytes |
| **Operations** | ~20 procedures | ~19 commands |

### Connection Model

```
NFS Connection Model:
┌─────────┐                    ┌─────────┐
│  Client │───TCP Connection───│  Server │
└─────────┘                    └─────────┘
     │                              │
     │  MOUNT /export ──────────►  │  Returns root file handle
     │  ◄────────────────────────  │
     │                              │
     │  LOOKUP, READ, WRITE ────►  │  Each request is independent
     │  ◄────────────────────────  │  (stateless, handle-based)
     │                              │
     │  UMOUNT ──────────────────► │
     └──────────────────────────────┘

SMB Connection Model:
┌─────────┐                    ┌─────────┐
│  Client │───TCP Connection───│  Server │
└─────────┘                    └─────────┘
     │                              │
     │  NEGOTIATE ──────────────►  │  Agree on dialect/capabilities
     │  ◄────────────────────────  │
     │                              │
     │  SESSION_SETUP ──────────►  │  Authenticate, get SessionID
     │  ◄────────────────────────  │
     │                              │
     │  TREE_CONNECT \\srv\share ► │  Connect to share, get TreeID
     │  ◄────────────────────────  │
     │                              │
     │  CREATE file.txt ────────►  │  Open file, get FileID
     │  ◄────────────────────────  │
     │                              │
     │  READ/WRITE (FileID) ────►  │  Operations use IDs
     │  ◄────────────────────────  │
     │                              │
     │  CLOSE (FileID) ─────────►  │
     │  TREE_DISCONNECT ────────►  │
     │  LOGOFF ─────────────────►  │
     └──────────────────────────────┘
```

### Key Conceptual Mapping

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

---

## Protocol Architecture

### Message Format

Every SMB2 message follows this structure:

```
┌────────────────────────────────────────────────────────────┐
│                    NetBIOS Session Header                   │
│                         (4 bytes)                           │
├────────────────────────────────────────────────────────────┤
│                       SMB2 Header                          │
│                        (64 bytes)                           │
├────────────────────────────────────────────────────────────┤
│                      Command Body                           │
│                       (variable)                            │
└────────────────────────────────────────────────────────────┘
```

### NetBIOS Session Header

```
Byte 0:     Type (0x00 = Session Message)
Bytes 1-3:  Length (24-bit big-endian)
```

**NFS Comparison**: NFS uses RPC record marking (4 bytes: last fragment flag + 31-bit length)

### SMB2 Header (64 bytes)

| Offset | Size | Field | Description |
|--------|------|-------|-------------|
| 0 | 4 | ProtocolID | Magic: `0xFE 'S' 'M' 'B'` (0x424D53FE little-endian) |
| 4 | 2 | StructureSize | Always 64 |
| 6 | 2 | CreditCharge | Credits consumed by this request |
| 8 | 4 | Status | NT_STATUS code (responses) |
| 12 | 2 | Command | Operation code (CREATE, READ, etc.) |
| 14 | 2 | Credits | Credits requested/granted |
| 16 | 4 | Flags | Request/response flag, signing, etc. |
| 20 | 4 | NextCommand | Offset to next command (compound requests) |
| 24 | 8 | MessageID | Unique request identifier |
| 32 | 4 | Reserved | Process ID (sync) or AsyncID high part |
| 36 | 4 | TreeID | Share connection identifier |
| 40 | 8 | SessionID | Authentication session identifier |
| 48 | 16 | Signature | Message signature (if signed) |

**NFS Comparison**: NFS RPC headers contain XID (transaction ID), program number, version, procedure number. SMB2 header is fixed-size (64 bytes) while NFS RPC header size varies.

### Credits System

SMB2 uses a **credit-based flow control** system:

- Client requests credits in each message
- Server grants credits in responses
- Each request "spends" credits
- Prevents server overload

**NFS Comparison**: NFS has no built-in flow control; relies on TCP.

---

## Connection Lifecycle

### 1. NEGOTIATE

Client and server agree on protocol dialect and capabilities.

```
Client: "I support dialects 0x0202, 0x0210, 0x0300"
Server: "Let's use 0x0202, here's my GUID and capabilities"
```

**NFS Comparison**: NFS has no negotiation; client specifies version in mount options.

### 2. SESSION_SETUP

Client authenticates and establishes a session.

```
Client: "Here are my credentials (NTLM/Kerberos token)"
Server: "Authenticated! Your SessionID is 0x12345678"
```

**Key Output**: SessionID (64-bit) - used in all subsequent requests

**NFS Comparison**: NFS AUTH_UNIX sends UID/GID with each request; no session concept.

### 3. TREE_CONNECT

Client connects to a specific share.

```
Client: "I want to access \\server\share"
Server: "Connected! Your TreeID is 0x0001, share type is DISK"
```

**Key Output**: TreeID (32-bit) - identifies the share connection

**NFS Comparison**: Similar to NFS MOUNT, but returns TreeID instead of root file handle.

### 4. File Operations

With SessionID and TreeID established, client can perform file operations:

```
CREATE   → Open/create file → Returns FileID
READ     → Read data using FileID
WRITE    → Write data using FileID
CLOSE    → Release FileID
```

**FileID Structure** (16 bytes):
- Bytes 0-7: Persistent handle (survives reconnect)
- Bytes 8-15: Volatile handle (connection-specific)

**NFS Comparison**: NFS file handles are opaque bytes (variable length, typically 32-64 bytes). SMB FileID is always 16 bytes with defined structure.

### 5. Cleanup

```
CLOSE           → Release file handles
TREE_DISCONNECT → Disconnect from share
LOGOFF          → End session
```

---

## Core Concepts

### Sessions

A **Session** represents an authenticated user connection.

```go
// In DittoFS SMB implementation
type Session struct {
    SessionID  uint64    // Unique identifier
    IsGuest    bool      // Guest/anonymous authentication
    Username   string    // Authenticated user
    Domain     string    // Domain (for AD)
    CreatedAt  time.Time
    ClientAddr string
}
```

**Lifecycle**: Created by SESSION_SETUP, destroyed by LOGOFF or connection close.

**NFS Comparison**: NFS has no sessions; each request carries auth info.

### Tree Connections

A **Tree Connection** represents access to a share.

```go
type TreeConnection struct {
    TreeID    uint32    // Unique within session
    SessionID uint64    // Parent session
    ShareName string    // e.g., "/export"
    ShareType uint8     // DISK, PIPE, or PRINT
    CreatedAt time.Time
}
```

**Lifecycle**: Created by TREE_CONNECT, destroyed by TREE_DISCONNECT.

**NFS Comparison**: Similar to NFS export mount, but explicitly tracked.

### Open Files

An **Open File** represents a file handle.

```go
type OpenFile struct {
    FileID        [16]byte  // 8 bytes persistent + 8 bytes volatile
    TreeID        uint32
    SessionID     uint64
    Path          string
    IsDirectory   bool
    DesiredAccess uint32    // Read, write, delete, etc.
    OpenTime      time.Time
}
```

**Lifecycle**: Created by CREATE, destroyed by CLOSE.

**NFS Comparison**: NFS file handles are stateless and never "opened" or "closed" in the same sense. SMB requires explicit open/close.

### Share Types

| Type | Value | Description |
|------|-------|-------------|
| DISK | 0x01 | File share (most common) |
| PIPE | 0x02 | Named pipe (IPC) |
| PRINT | 0x03 | Printer share |

DittoFS only implements DISK shares.

---

## Commands and Operations

### Command Categories

| Category | Commands | NFS Equivalent |
|----------|----------|----------------|
| **Connection** | NEGOTIATE, SESSION_SETUP, LOGOFF | - (no equivalent) |
| **Share** | TREE_CONNECT, TREE_DISCONNECT | MOUNT, UMOUNT |
| **File** | CREATE, CLOSE, READ, WRITE, FLUSH | LOOKUP+CREATE, -, READ, WRITE, COMMIT |
| **Directory** | QUERY_DIRECTORY | READDIR, READDIRPLUS |
| **Metadata** | QUERY_INFO, SET_INFO | GETATTR, SETATTR |
| **Other** | ECHO, CANCEL, LOCK, IOCTL | - |

### CREATE Command

The most complex command - handles opening, creating, and metadata operations.

**Request includes**:
- DesiredAccess (read, write, delete permissions)
- FileAttributes (hidden, readonly, directory)
- ShareAccess (allow concurrent read/write/delete)
- CreateDisposition (create, open, overwrite behavior)
- CreateOptions (directory, non-directory, delete-on-close)
- Filename (UTF-16LE encoded, relative to share root)

**CreateDisposition values**:
| Value | Name | Behavior |
|-------|------|----------|
| 0 | FILE_SUPERSEDE | Replace if exists, create if not |
| 1 | FILE_OPEN | Open existing only |
| 2 | FILE_CREATE | Create new only (fail if exists) |
| 3 | FILE_OPEN_IF | Open or create |
| 4 | FILE_OVERWRITE | Overwrite existing only |
| 5 | FILE_OVERWRITE_IF | Overwrite or create |

**NFS Comparison**: NFS separates these into LOOKUP, CREATE, OPEN (v4). SMB combines them.

### READ/WRITE Commands

Simple data transfer operations using FileID.

**READ Request**:
- FileID (16 bytes)
- Offset (64-bit)
- Length (32-bit)

**WRITE Request**:
- FileID (16 bytes)
- Offset (64-bit)
- Data

**NFS Comparison**: Nearly identical semantics. SMB uses FileID, NFS uses file handle.

### QUERY_DIRECTORY Command

Lists directory contents with various detail levels.

**Information Classes**:
| Class | Description |
|-------|-------------|
| FileDirectoryInformation | Basic (name, times, size, attributes) |
| FileBothDirectoryInformation | Basic + 8.3 short name |
| FileIdBothDirectoryInformation | Both + unique file ID |
| FileNamesInformation | Names only |

**NFS Comparison**: Similar to READDIR/READDIRPLUS, but with more format options.

---

## Error Handling

### NT_STATUS Codes

SMB uses 32-bit NT_STATUS codes (Windows error format):

```
Bits 31-30: Severity (00=Success, 01=Info, 10=Warning, 11=Error)
Bits 29-28: Reserved
Bit 27:     Customer code flag
Bit 16-26:  Facility code
Bits 0-15:  Error code
```

**Common Status Codes**:
| Code | Name | Meaning |
|------|------|---------|
| 0x00000000 | STATUS_SUCCESS | Operation successful |
| 0xC0000022 | STATUS_ACCESS_DENIED | Permission denied |
| 0xC0000034 | STATUS_OBJECT_NAME_NOT_FOUND | File not found |
| 0xC0000035 | STATUS_OBJECT_NAME_COLLISION | File already exists |
| 0xC00000BA | STATUS_FILE_IS_A_DIRECTORY | Cannot read/write directory |
| 0xC0000103 | STATUS_NOT_A_DIRECTORY | Path component not a directory |
| 0xC00000CC | STATUS_BAD_NETWORK_NAME | Share not found |
| 0x80000006 | STATUS_NO_MORE_FILES | End of directory listing |

**NFS Comparison**: NFS uses simpler numeric error codes (NFS3ERR_*). SMB status codes encode more information.

### Error Mapping

| NFS Error | SMB Status | Description |
|-----------|------------|-------------|
| NFS3ERR_NOENT | STATUS_OBJECT_NAME_NOT_FOUND | File not found |
| NFS3ERR_ACCES | STATUS_ACCESS_DENIED | Permission denied |
| NFS3ERR_EXIST | STATUS_OBJECT_NAME_COLLISION | File exists |
| NFS3ERR_NOTDIR | STATUS_NOT_A_DIRECTORY | Not a directory |
| NFS3ERR_ISDIR | STATUS_FILE_IS_A_DIRECTORY | Is a directory |
| NFS3ERR_NOTEMPTY | STATUS_DIRECTORY_NOT_EMPTY | Directory not empty |

---

## DittoFS Implementation Mapping

### Code Structure Comparison

```
NFS Implementation:              SMB Implementation:
internal/protocol/nfs/           internal/protocol/smb/
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

### Adapter Structure Comparison

```
NFS Adapter:                     SMB Adapter:
pkg/adapter/nfs/                 pkg/adapter/smb/
├── config.go                    ├── config.go
├── nfs_adapter.go               ├── smb_adapter.go
└── nfs_connection.go            └── smb_connection.go
```

### Handler Pattern

Both protocols use the same dispatch pattern:

```go
// NFS dispatch (simplified)
switch procedure {
case NFSPROC3_LOOKUP:
    return handleLookup(ctx, body)
case NFSPROC3_READ:
    return handleRead(ctx, body)
}

// SMB dispatch (simplified)
switch command {
case SMB2Create:
    return handleCreate(ctx, body)
case SMB2Read:
    return handleRead(ctx, body)
}
```

### Data Encoding Comparison

```go
// NFS: XDR (big-endian)
func writeUint32(w io.Writer, v uint32) {
    binary.Write(w, binary.BigEndian, v)
}

// SMB: Little-endian
func writeUint32(buf []byte, v uint32) {
    binary.LittleEndian.PutUint32(buf, v)
}
```

### String Encoding

```go
// NFS: UTF-8 with length prefix
func writeString(w io.Writer, s string) {
    binary.Write(w, binary.BigEndian, uint32(len(s)))
    w.Write([]byte(s))
    // Pad to 4-byte boundary
}

// SMB: UTF-16LE
func encodeUTF16LE(s string) []byte {
    u16s := utf16.Encode([]rune(s))
    b := make([]byte, len(u16s)*2)
    for i, r := range u16s {
        binary.LittleEndian.PutUint16(b[i*2:], r)
    }
    return b
}
```

---

## Glossary

| Term | Definition |
|------|------------|
| **ACL** | Access Control List - Windows permission model |
| **CIFS** | Common Internet File System - older name for SMB |
| **Credit** | Flow control unit in SMB2 |
| **Dialect** | SMB protocol version (e.g., 0x0202 = SMB 2.0.2) |
| **FileID** | 16-byte handle for open file (8 persistent + 8 volatile) |
| **GUID** | 16-byte globally unique identifier |
| **NetBIOS** | Network Basic Input/Output System - legacy session layer |
| **NT_STATUS** | Windows error code format |
| **Oplock** | Opportunistic lock - client caching hint |
| **SessionID** | 64-bit identifier for authenticated session |
| **Share** | Network-accessible folder (like NFS export) |
| **SID** | Security Identifier - Windows user/group identity |
| **TreeID** | 32-bit identifier for share connection |
| **UTF-16LE** | 16-bit Unicode, little-endian byte order |

---

## References

- [MS-SMB2]: SMB2 Protocol Specification - https://docs.microsoft.com/en-us/openspecs/windows_protocols/ms-smb2/
- [MS-FSCC]: File System Control Codes - https://docs.microsoft.com/en-us/openspecs/windows_protocols/ms-fscc/
- [MS-ERREF]: Windows Error Codes - https://docs.microsoft.com/en-us/openspecs/windows_protocols/ms-erref/
- [RFC 1813]: NFS Version 3 Protocol Specification - https://tools.ietf.org/html/rfc1813
