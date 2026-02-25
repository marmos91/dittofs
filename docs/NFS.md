# NFS Implementation

This document covers the NFS protocol fundamentals, DittoFS's implementation of NFSv3, NFSv4.0, and NFSv4.1, and practical usage for clients and developers.

## Table of Contents

- [Protocol Overview](#protocol-overview)
  - [What is NFS?](#what-is-nfs)
  - [Protocol Architecture](#protocol-architecture)
  - [RPC Foundation](#rpc-foundation)
  - [XDR Encoding](#xdr-encoding)
  - [Mount Protocol](#mount-protocol)
  - [NFSv3 Procedures](#nfsv3-procedures)
  - [File Handles](#file-handles)
  - [Authentication](#authentication)
  - [Error Handling](#error-handling)
- [Implementation Status](#implementation-status)
  - [Mount Protocol Status](#mount-protocol-status)
  - [NFSv3 Status](#nfsv3-status)
  - [NFSv4.0 Status](#nfsv40-status)
  - [NFSv4.1 Status](#nfsv41-status)
- [Embedded Portmapper](#embedded-portmapper)
- [Mounting](#mounting)
- [Implementation Details](#implementation-details)
  - [Code Structure](#code-structure)
  - [RPC Flow](#rpc-flow)
  - [Critical Procedures](#critical-procedures)
  - [Write Coordination Pattern](#write-coordination-pattern)
  - [Buffer Pooling](#buffer-pooling)
  - [Dispatch and Handler Pattern](#dispatch-and-handler-pattern)
- [NFSv4 Directory Delegations](#nfsv4-directory-delegations)
- [Testing NFS Operations](#testing-nfs-operations)
- [Glossary](#glossary)
- [References](#references)

---

## Protocol Overview

### What is NFS?

**NFS (Network File System)** is a distributed file system protocol originally developed by Sun Microsystems in 1984. It allows a client to access files over a network as if they were on local storage.

| Version | Year | Key Features |
|---------|------|--------------|
| NFSv2 | 1989 | Original version, 32-bit file sizes, UDP only |
| NFSv3 | 1995 | 64-bit file sizes, TCP support, async writes, WCC |
| NFSv4 | 2000 | Stateful, ACLs, compound operations, no mount protocol |
| NFSv4.1 | 2010 | Parallel NFS (pNFS), sessions, backchannel |
| NFSv4.2 | 2016 | Server-side copy, sparse files |

DittoFS implements **NFSv3**, **NFSv4.0**, and **NFSv4.1** -- covering the stateless simplicity of v3 through the stateful, session-based model of v4.1 with delegations, ACLs, CB_NOTIFY, and Kerberos via RPCSEC_GSS.

### Protocol Architecture

NFS uses a layered architecture with multiple supporting protocols:

```
+------------------------------------------------------------+
|                    NFS Application                          |
|              (file operations: read, write, etc.)           |
+------------------------------------------------------------+
|               NFSv3 Protocol (Program 100003)               |
|           22 procedures for file system operations          |
+------------------------------------------------------------+
|              Mount Protocol (Program 100005)                |
|        6 procedures for mounting exported directories       |
+------------------------------------------------------------+
|                 RPC (Remote Procedure Call)                  |
|              Message framing, authentication                |
+------------------------------------------------------------+
|                 XDR (External Data Representation)           |
|                 Binary encoding/decoding                    |
+------------------------------------------------------------+
|                          TCP/IP                              |
|                    Transport layer                           |
+------------------------------------------------------------+
```

**Request/Response Flow:**

```
+--------------+                         +--------------+
|    Client    |                         |    Server    |
+--------------+                         +--------------+
       |                                        |
       |  1. TCP Connection (port 12049)        |
       | -------------------------------------> |
       |                                        |
       |  2. MOUNT /export -------------------> |
       |     [RPC Call: Program 100005, v3]     |
       |                                        |
       |  <------------------ Root file handle  |
       |     [RPC Reply: OK + handle + auth]    |
       |                                        |
       |  3. LOOKUP "file.txt" ---------------> |
       |     [NFS Call: handle + name]          |
       |                                        |
       |  <------------------ File handle       |
       |     [NFS Reply: OK + handle + attrs]   |
       |                                        |
       |  4. READ (offset=0, count=4096) -----> |
       |     [NFS Call: handle + offset + len]  |
       |                                        |
       |  <------------------ Data + EOF flag   |
       |     [NFS Reply: OK + attrs + data]     |
       |                                        |
       |  5. UMOUNT /export ------------------> |
       |                                        |
       +----------------------------------------+
```

**Key Design Principles:**

- **Stateless Operations (v3)**: Each request contains all information needed to process it. The server can restart without affecting client state.
- **Stateful Sessions (v4/v4.1)**: Sessions track client state, enabling delegations and lock management.
- **Idempotent Procedures**: Most operations can be safely retried if a response is lost.
- **Weak Cache Consistency (WCC)**: Responses include pre-operation and post-operation attributes so clients can detect concurrent modifications.

### RPC Foundation

NFS uses **ONC RPC (Open Network Computing Remote Procedure Call)**, defined in RFC 5531. RPC provides message framing over TCP, procedure identification (program, version, procedure numbers), an authentication framework, and request/reply matching via transaction IDs (XIDs).

**RPC Message Structure:**

```
+------------------------------------------------------------+
|                 RPC Record Fragment Header                   |
|                         (4 bytes)                            |
+------------------------------------------------------------+
|                    RPC Call/Reply Header                     |
|                        (variable)                            |
+------------------------------------------------------------+
|                    Procedure Arguments                       |
|                   or Results (variable)                      |
+------------------------------------------------------------+
```

**Fragment Header:** TCP connections use record marking to frame RPC messages. Bit 31 is the last-fragment flag (1 = last, 0 = more fragments follow). Bits 0-30 encode the fragment length in bytes. For example, `0x80000064` means last fragment, length 100 bytes.

**RPC Call Header Fields:**

| Offset | Field |
|--------|-------|
| 0-3 | XID (Transaction ID, echoed in reply) |
| 4-7 | Message Type (0 = CALL, 1 = REPLY) |
| 8-11 | RPC Version (must be 2) |
| 12-15 | Program Number (100003 = NFS, 100005 = Mount) |
| 16-19 | Program Version (3 for NFSv3/Mount v3) |
| 20-23 | Procedure Number (0-21 for NFSv3) |
| 24+ | Credentials (OpaqueAuth structure) |
| variable | Verifier (OpaqueAuth structure) |

**RPC Reply Accept States:**

| Code | Name |
|------|------|
| 0 | SUCCESS |
| 1 | PROG_UNAVAIL |
| 2 | PROG_MISMATCH |
| 3 | PROC_UNAVAIL |
| 4 | GARBAGE_ARGS |
| 5 | SYSTEM_ERR |

### XDR Encoding

**XDR (External Data Representation)**, defined in RFC 4506, provides a canonical binary encoding for network transmission.

**Key rules:**
1. Big-endian byte order for all integers
2. 4-byte alignment for all data items
3. Zero-padding to reach 4-byte boundaries

**Basic Types:**

| Type | Size | Description |
|------|------|-------------|
| int | 4 bytes | Signed 32-bit integer |
| unsigned int | 4 bytes | Unsigned 32-bit integer |
| hyper | 8 bytes | Signed 64-bit integer |
| unsigned hyper | 8 bytes | Unsigned 64-bit integer |
| bool | 4 bytes | Boolean (0 = false, 1 = true) |

**Variable-Length Data (Opaque):**

Encoded as a 4-byte length prefix, followed by the data bytes, followed by 0-3 padding bytes to reach a 4-byte boundary. Padding formula: `(4 - (length % 4)) % 4`.

**Strings** follow the same encoding: 4-byte length prefix, UTF-8 data, zero-padding.

**Optional values** use a boolean discriminator (4 bytes): if 1, the value follows; if 0, no value is present.

### Mount Protocol

The **Mount protocol** (Program 100005, Version 3) is a companion protocol to NFSv3 used to obtain the initial file handle for an exported directory, list available exports, and track active mounts. NFSv4 does not use the mount protocol; it uses PUTROOTFH and LOOKUP compound operations instead.

**Mount Procedures:**

| Proc | Name | Purpose |
|------|------|---------|
| 0 | NULL | Connectivity test (no-op) |
| 1 | MNT | Mount an export, returns root file handle |
| 2 | DUMP | List active mounts |
| 3 | UMNT | Unmount an export |
| 4 | UMNTALL | Unmount all exports for this client |
| 5 | EXPORT | List available exports |

**MNT Request:** Contains a single string field (`dirpath`), e.g., `"/export"`.

**MNT Response (on success):** Status (0 = OK), root file handle (opaque, up to 64 bytes), and a list of supported auth flavors.

**Mount Status Codes:**

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

### NFSv3 Procedures

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

**Write Stability Levels** (for WRITE procedure):

| Level | Name | Description |
|-------|------|-------------|
| 0 | UNSTABLE | Data may be cached; requires COMMIT |
| 1 | DATA_SYNC | Data committed, metadata may be cached |
| 2 | FILE_SYNC | Both data and metadata committed |

**WCC (Weak Cache Consistency):** Mutating operations return pre-operation attributes (size, mtime, ctime) and post-operation attributes (full fattr3). Clients use WCC to detect stale caches, update attributes after operations, and detect concurrent modifications by other clients.

### File Handles

**File handles** are opaque identifiers that uniquely identify files and directories:
- Generated by the server
- Opaque to clients (clients must not interpret them)
- Persistent across server restarts for production stores
- Maximum 64 bytes per RFC 1813

DittoFS encodes share and file information in handles. The format varies by metadata store:
- **Memory store**: In-memory IDs (ephemeral)
- **BadgerDB**: Path-based handles (persistent)
- **PostgreSQL**: Share name + UUID (distributed)

When a handle becomes invalid (file deleted, server restarted with ephemeral storage), the server returns `NFS3ERR_STALE`. Clients should discard cached information and re-lookup the file.

### Authentication

NFS uses RPC authentication flavors:

| Flavor | Value | Description |
|--------|-------|-------------|
| AUTH_NULL | 0 | No authentication |
| AUTH_UNIX | 1 | Unix UID/GID credentials |
| AUTH_SHORT | 2 | Short-hand credential |
| RPCSEC_GSS | 6 | Kerberos/GSS-API (NFSv4) |

**AUTH_UNIX format:** Stamp (4 bytes), machine name (string), UID (4 bytes), GID (4 bytes), supplementary GIDs (array, max 16).

**Security note:** AUTH_UNIX credentials are not cryptographically secured and can be spoofed. NFSv4 adds RPCSEC_GSS for Kerberos-based authentication. For production deployments, consider running on trusted networks, enabling Kerberos (NFSv4/v4.1), or using VPN/network-level encryption.

### Error Handling

**NFS Status Codes:**

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

Internal errors are mapped to NFS status codes in `pkg/metadata/errors.go`.

---

## Implementation Status

### Mount Protocol Status

| Procedure | Status | Notes |
|-----------|--------|-------|
| NULL | Implemented | |
| MNT | Implemented | |
| UMNT | Implemented | |
| UMNTALL | Implemented | |
| DUMP | Implemented | |
| EXPORT | Implemented | |

### NFSv3 Status

**Read Operations:**

| Procedure | Status | Notes |
|-----------|--------|-------|
| NULL | Implemented | |
| GETATTR | Implemented | |
| SETATTR | Implemented | |
| LOOKUP | Implemented | |
| ACCESS | Implemented | |
| READ | Implemented | |
| READDIR | Implemented | |
| READDIRPLUS | Implemented | |
| FSSTAT | Implemented | |
| FSINFO | Implemented | |
| PATHCONF | Implemented | |
| READLINK | Implemented | |

**Write Operations:**

| Procedure | Status | Notes |
|-----------|--------|-------|
| WRITE | Implemented | |
| CREATE | Implemented | |
| MKDIR | Implemented | |
| REMOVE | Implemented | |
| RMDIR | Implemented | |
| RENAME | Implemented | |
| LINK | Implemented | |
| SYMLINK | Implemented | |
| MKNOD | Implemented | Limited support |
| COMMIT | Implemented | |

**Total**: 28 procedures fully implemented (6 mount + 22 NFS).

### NFSv4.0 Status

NFSv4.0 uses compound operations instead of individual RPC procedures. All operations are bundled into COMPOUND requests.

| Operation | Status | Notes |
|-----------|--------|-------|
| ACCESS | Implemented | |
| CLOSE | Implemented | |
| COMMIT | Implemented | |
| CREATE | Implemented | |
| DELEGRETURN | Implemented | |
| GETATTR | Implemented | |
| GETFH | Implemented | |
| ILLEGAL | Implemented | |
| LINK | Implemented | |
| LOCK / LOCKT / LOCKU | Implemented | |
| LOOKUP | Implemented | |
| LOOKUPP | Implemented | |
| NVERIFY | Implemented | |
| NULL | Implemented | |
| OPEN | Implemented | |
| PUTFH | Implemented | |
| PUTPUBFH | Implemented | |
| PUTROOTFH | Implemented | |
| READ | Implemented | |
| READDIR | Implemented | |
| READLINK | Implemented | |
| REMOVE | Implemented | |
| RENAME | Implemented | |
| RENEW | Implemented | |
| RESTOREFH | Implemented | |
| SAVEFH | Implemented | |
| SECINFO | Implemented | |
| SETATTR | Implemented | |
| SETCLIENTID | Implemented | |
| VERIFY | Implemented | |
| WRITE | Implemented | |

### NFSv4.1 Status

NFSv4.1 extends v4.0 with session-based operation, backchannel callbacks, and additional operations.

| Operation | Status | Notes |
|-----------|--------|-------|
| BACKCHANNEL_CTL | Implemented | |
| BIND_CONN_TO_SESSION | Implemented | |
| CREATE_SESSION | Implemented | |
| DESTROY_CLIENTID | Implemented | |
| DESTROY_SESSION | Implemented | |
| EXCHANGE_ID | Implemented | |
| FREE_STATEID | Implemented | |
| GET_DIR_DELEGATION | Implemented | Directory delegation with CB_NOTIFY |
| RECLAIM_COMPLETE | Implemented | |
| SEQUENCE | Implemented | |
| TEST_STATEID | Implemented | |

---

## Embedded Portmapper

DittoFS includes an embedded portmapper (RFC 1057) that enables standard NFS service discovery without requiring a system-level `rpcbind` daemon.

### Why an Embedded Portmapper?

NFS clients traditionally rely on a portmapper (port 111) to discover which port an NFS server is listening on. Without a portmapper, clients require explicit port options (`-o port=12049,mountport=12049`), and standard tools like `rpcinfo` and `showmount` do not work.

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

---

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

---

## Implementation Details

### Code Structure

```
dittofs/
+-- pkg/adapter/nfs/
|   +-- nfs_adapter.go         # NFS adapter implementing Adapter interface
|   +-- nfs_connection.go      # Connection handling
|   +-- config.go              # NFS-specific configuration
|
+-- internal/adapter/nfs/
    +-- dispatch.go            # Procedure routing
    +-- bufpool.go             # Buffer pooling for performance
    +-- rpc/
    |   +-- message.go         # RPC message structures
    |   +-- parser.go          # RPC parsing and reply building
    |   +-- auth.go            # Authentication parsing
    |   +-- constants.go       # RPC constants
    +-- xdr/
    |   +-- decode.go          # XDR decoding helpers
    |   +-- encode.go          # XDR encoding helpers
    |   +-- attributes.go      # File attribute encoding
    |   +-- filehandle.go      # File handle utilities
    |   +-- time.go            # NFS time format conversion
    +-- types/
    |   +-- constants.go       # NFS constants
    |   +-- types.go           # NFS type definitions
    +-- mount/handlers/
    |   +-- mount.go           # MNT procedure
    |   +-- umount.go          # UMNT procedure
    |   +-- export.go          # EXPORT procedure
    |   +-- dump.go            # DUMP procedure
    |   +-- constants.go       # Mount protocol constants
    +-- v3/handlers/
    |   +-- null.go            # NULL procedure
    |   +-- getattr.go         # GETATTR procedure
    |   +-- setattr.go         # SETATTR procedure
    |   +-- lookup.go          # LOOKUP procedure
    |   +-- access.go          # ACCESS procedure
    |   +-- read.go            # READ procedure
    |   +-- write.go           # WRITE procedure
    |   +-- create.go          # CREATE procedure
    |   +-- mkdir.go           # MKDIR procedure
    |   +-- remove.go          # REMOVE procedure
    |   +-- rmdir.go           # RMDIR procedure
    |   +-- rename.go          # RENAME procedure
    |   +-- readdir.go         # READDIR procedure
    |   +-- readdirplus.go     # READDIRPLUS procedure
    |   +-- commit.go          # COMMIT procedure
    +-- v4/handlers/
    |   +-- compound.go        # COMPOUND request dispatch
    |   +-- handler.go         # NFSv4 handler context
    |   +-- open.go            # OPEN (stateful file access)
    |   +-- close.go           # CLOSE
    |   +-- lock.go            # LOCK / LOCKT / LOCKU
    |   +-- delegreturn.go     # DELEGRETURN
    |   +-- setclientid.go     # SETCLIENTID
    |   +-- secinfo.go         # SECINFO
    |   +-- ...                # All other v4.0 operations
    +-- v4/v41/handlers/
        +-- exchange_id.go         # EXCHANGE_ID (client identification)
        +-- create_session.go      # CREATE_SESSION
        +-- destroy_session.go     # DESTROY_SESSION
        +-- sequence.go            # SEQUENCE (slot management)
        +-- bind_conn_to_session.go
        +-- backchannel_ctl.go     # Backchannel setup for CB_NOTIFY
        +-- get_dir_delegation.go  # GET_DIR_DELEGATION
        +-- reclaim_complete.go    # RECLAIM_COMPLETE
        +-- destroy_clientid.go    # DESTROY_CLIENTID
        +-- free_stateid.go        # FREE_STATEID
        +-- test_stateid.go        # TEST_STATEID
```

### RPC Flow

1. TCP connection accepted
2. RPC message parsed (`rpc/message.go`)
3. Program/version/procedure validated
4. Auth context extracted (`dispatch.go:ExtractAuthContext`)
5. Procedure handler dispatched
6. Handler calls repository methods
7. Response encoded and sent

### Critical Procedures

**Mount Protocol** (`internal/adapter/nfs/mount/handlers/`)
- `MNT`: Validates export access, records mount, returns root handle
- `UMNT`: Removes mount record
- `EXPORT`: Lists available exports
- `DUMP`: Lists active mounts (can be restricted)

**NFSv3 Core** (`internal/adapter/nfs/v3/handlers/`)
- `LOOKUP`: Resolve name in directory to file handle
- `GETATTR`: Get file attributes
- `SETATTR`: Update attributes (size, mode, times)
- `READ`: Read file content (uses payload store)
- `WRITE`: Write file content (coordinates metadata + payload stores)
- `CREATE`: Create file
- `MKDIR`: Create directory
- `REMOVE`: Delete file
- `RMDIR`: Delete empty directory
- `RENAME`: Move/rename file
- `READDIR` / `READDIRPLUS`: List directory entries

**NFSv4 Compound Operations** (`internal/adapter/nfs/v4/handlers/`)
- `COMPOUND`: Dispatches a sequence of operations in a single RPC call
- `OPEN` / `CLOSE`: Stateful file access with share reservations
- `LOCK` / `LOCKT` / `LOCKU`: Byte-range locking
- `DELEGRETURN`: Return a delegation to the server
- `SECINFO`: Security flavor negotiation

**NFSv4.1 Session Operations** (`internal/adapter/nfs/v4/v41/handlers/`)
- `EXCHANGE_ID`: Client identification and capability negotiation
- `CREATE_SESSION` / `DESTROY_SESSION`: Session lifecycle
- `SEQUENCE`: Per-request slot and sequence management
- `GET_DIR_DELEGATION`: Request directory delegation with CB_NOTIFY
- `BACKCHANNEL_CTL`: Configure backchannel for server-initiated callbacks

### Write Coordination Pattern

WRITE operations require coordination between metadata and payload stores:

```go
// 1. Update metadata (validates permissions, updates size/timestamps)
attr, preSize, preMtime, preCtime, err := metadataStore.WriteFile(handle, newSize, authCtx)

// 2. Write actual data via payload store
err = payloadStore.WriteAt(attr.PayloadID, data, offset)

// 3. Return updated attributes to client for cache consistency
```

The metadata store:
- Validates write permission
- Returns pre-operation attributes (for WCC data)
- Updates file size if extended
- Updates mtime/ctime timestamps
- Ensures PayloadID exists

### Buffer Pooling

Large I/O operations use buffer pools (`internal/adapter/nfs/bufpool.go`):
- Reduces GC pressure
- Reuses buffers for READ/WRITE
- Automatically sizes based on request

### Dispatch and Handler Pattern

```go
// internal/adapter/nfs/dispatch.go

// NFS dispatch table - maps procedure numbers to handlers
var NfsDispatchTable = map[uint32]*nfsProcedure{
    types.NFSProcNull:    {Name: "NULL",    Handler: handleNFSNull},
    types.NFSProcGetAttr: {Name: "GETATTR", Handler: handleNFSGetAttr},
    types.NFSProcSetAttr: {Name: "SETATTR", Handler: handleNFSSetAttr},
    types.NFSProcLookup:  {Name: "LOOKUP",  Handler: handleNFSLookup},
    types.NFSProcRead:    {Name: "READ",    Handler: handleNFSRead},
    types.NFSProcWrite:   {Name: "WRITE",   Handler: handleNFSWrite},
    // ... all 22 procedures
}
```

Each handler follows the same pattern:
1. Check context cancellation
2. Validate request
3. Get stores from registry (metadata store + payload store)
4. Perform operation via store methods
5. Build and return response

---

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
| `max_delegations` | `10000` | Maximum concurrent delegations across all clients |
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

### Delegation Limitations

- **Ephemeral state**: Directory delegations are lost on server restart (in-memory only)
- **Linux client support**: The Linux NFS client does not currently request directory delegations; this feature is primarily useful for custom NFSv4.1 clients
- **No persistent notification queue**: If the backchannel is unavailable when notifications flush, they are silently dropped

---

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

---

## Glossary

| Term | Definition |
|------|------------|
| **AUTH_NULL** | No authentication flavor (flavor 0) |
| **AUTH_UNIX** | Unix-style authentication with UID/GID (flavor 1) |
| **Backchannel** | Server-to-client connection used for callbacks (NFSv4.1) |
| **CB_NOTIFY** | Callback operation for directory change notifications |
| **COMPOUND** | NFSv4 request containing multiple operations |
| **Cookie** | Opaque value used for directory iteration (READDIR) |
| **Delegation** | Server grants client exclusive or shared caching rights |
| **EOF** | End of file indicator in READ responses |
| **Export** | A directory shared via NFS (like an SMB share) |
| **File Handle** | Opaque identifier for a file/directory (max 64 bytes) |
| **ftype3** | File type enum (regular, directory, symlink, etc.) |
| **FSID** | File system identifier |
| **nfstime3** | NFS time format (seconds + nanoseconds) |
| **RPCSEC_GSS** | Kerberos-based RPC security flavor (NFSv4) |
| **RPC** | Remote Procedure Call -- foundation protocol |
| **sattr3** | Set attributes structure (for SETATTR, CREATE) |
| **Session** | NFSv4.1 construct tracking client connection state |
| **Stale Handle** | A handle that is no longer valid |
| **Verifier** | Server-unique value that changes on restart |
| **WCC** | Weak Cache Consistency data (pre/post attributes) |
| **XDR** | External Data Representation (encoding format) |
| **XID** | Transaction ID for matching requests/replies |

---

## References

### Specifications

- [RFC 1057](https://tools.ietf.org/html/rfc1057) - RPC: Remote Procedure Call Protocol (Portmapper)
- [RFC 1094](https://tools.ietf.org/html/rfc1094) - NFS: Network File System Protocol (Version 2)
- [RFC 1813](https://tools.ietf.org/html/rfc1813) - NFS Version 3 Protocol Specification
- [RFC 4506](https://tools.ietf.org/html/rfc4506) - XDR: External Data Representation Standard
- [RFC 5531](https://tools.ietf.org/html/rfc5531) - RPC: Remote Procedure Call Protocol Specification Version 2
- [RFC 7530](https://tools.ietf.org/html/rfc7530) - NFS Version 4 Protocol
- [RFC 8881](https://tools.ietf.org/html/rfc8881) - NFS Version 4 Minor Version 1 Protocol

### Related Projects

- [go-nfs](https://github.com/willscott/go-nfs) - Another NFS implementation in Go
- [FUSE](https://github.com/libfuse/libfuse) - Filesystem in Userspace

### DittoFS Documentation

- [Architecture](ARCHITECTURE.md) - Deep dive into design patterns and implementation
- [Configuration](CONFIGURATION.md) - Complete configuration guide
- [Troubleshooting](TROUBLESHOOTING.md) - Common issues and solutions
- [FAQ](FAQ.md) - Frequently asked questions
