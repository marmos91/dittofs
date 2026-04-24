# SMB Implementation

This document details DittoFS's SMB implementation, protocol status, client usage, and protocol internals for maintainers and users. DittoFS supports SMB2 dialect 0x0202 through SMB 3.1.1, including encryption, signing, leases V2, directory leasing, durable handles, and Kerberos authentication.

## Table of Contents

- [Protocol Overview](#protocol-overview)
- [Mounting SMB Shares](#mounting-smb-shares)
- [Protocol Implementation Status](#protocol-implementation-status)
- [Implementation Details](#implementation-details)
- [Authentication](#authentication)
- [SMB3 Dialect Negotiation](#smb3-dialect-negotiation)
- [Encryption](#encryption)
- [Signing](#signing)
- [Key Derivation](#key-derivation)
- [Leases V2 and Directory Leasing](#leases-v2-and-directory-leasing)
- [Durable Handles](#durable-handles)
- [Kerberos and SPNEGO Authentication](#kerberos-and-spnego-authentication)
- [Cross-Protocol Behavior](#cross-protocol-behavior)
- [Byte-Range Locking](#byte-range-locking)
- [Opportunistic Locks](#opportunistic-locks)
- [Change Notifications](#change-notifications)
- [Testing SMB Operations](#testing-smb-operations)
- [Troubleshooting](#troubleshooting)
- [Known Limitations](#known-limitations)
- [Glossary](#glossary)
- [References](#references)

## Protocol Overview

### What is SMB?

**SMB (Server Message Block)** is a network file sharing protocol originally developed by IBM in 1983 and later extended by Microsoft. It is the native file sharing protocol for Windows and is also known as **CIFS (Common Internet File System)**.

DittoFS implements multiple SMB dialects for broad compatibility:

| Dialect | Version | Key Features |
|---------|---------|--------------|
| 0x0202 | SMB 2.0.2 | Basic file operations, credits, HMAC-SHA256 signing |
| 0x0300 | SMB 3.0 | Encryption (AES-128-CCM), AES-128-CMAC signing, secure dialect negotiation |
| 0x0302 | SMB 3.0.2 | VALIDATE_NEGOTIATE_INFO for downgrade protection |
| 0x0311 | SMB 3.1.1 | Preauth integrity (SHA-512), AES-128-GCM encryption, GMAC signing, negotiate contexts |

The server negotiates the highest mutually supported dialect with each client. SMB 3.1.1 is preferred for its stronger security guarantees.

### SMB vs NFS: Key Differences

| Aspect | NFS (v3/v4) | SMB2 (2.0.2) | SMB3 (3.0-3.1.1) |
|--------|-------------|--------------|-------------------|
| **Origin** | Unix (Sun Microsystems, 1984) | Windows (IBM/Microsoft, 1983) | Windows (Microsoft, 2012) |
| **Design** | v3: Stateless / v4: Stateful | Stateful, session-based | Stateful, session-based |
| **Identity** | UID/GID (Unix) | SID (Windows Security ID) | SID + Kerberos principal |
| **Permissions** | Unix mode bits / NFSv4 ACLs | ACLs (Access Control Lists) | ACLs |
| **Transport** | TCP (port 2049) | TCP (port 445) | TCP (port 445) |
| **Framing** | RPC record marking | NetBIOS session header | NetBIOS + Transform header |
| **Encoding** | XDR (big-endian) | Custom (little-endian) | Custom (little-endian) |
| **Header** | Variable (RPC) | Fixed 64 bytes | Fixed 64 bytes (+ 52-byte transform) |
| **Strings** | UTF-8 | UTF-16LE | UTF-16LE |
| **Flow control** | None (relies on TCP) | Credit-based | Credit-based |
| **Encryption** | krb5p (RPCSEC_GSS) | None | AES-GCM / AES-CCM (transform header) |
| **Signing** | krb5i (RPCSEC_GSS) | HMAC-SHA256 | AES-CMAC / AES-GMAC |
| **Client caching** | Delegations | Oplocks | Leases V2 (file + directory) |
| **Handle resilience** | Volatile | Volatile | Durable / Persistent handles |

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
| Delegation | Lease V2 | Client caching grant |
| CB_RECALL | Lease Break Notification | Cache invalidation |
| CB_NOTIFY | CHANGE_NOTIFY | Directory change events |

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

For SMB3 encrypted messages, a **Transform Header** wraps the entire message:

```
+------------------------------------------------------------+
|                    NetBIOS Session Header                   |
|                         (4 bytes)                           |
+------------------------------------------------------------+
|                  Transform Header (0xFD534D42)              |
|                        (52 bytes)                           |
|   Signature (16) | Nonce (16) | OrigMsgSize (4)            |
|   Reserved (2) | Flags (2) | SessionID (8)                 |
+------------------------------------------------------------+
|                   Encrypted Payload                         |
|            (SMB2 Header + Command Body, encrypted)          |
+------------------------------------------------------------+
```

The **NetBIOS session header** contains a type byte (0x00 for session messages) and a 24-bit big-endian length. The **SMB2 header** is always 64 bytes and includes the protocol magic (`0xFE 'S' 'M' 'B'`), command code, credit charge/grant, session ID, tree ID, message ID, flags, and signature. The **Transform header** uses magic `0xFD 'S' 'M' 'B'` and carries the AEAD nonce and authentication tag.

### Connection Lifecycle

SMB connections follow a multi-phase setup before file operations can begin:

1. **NEGOTIATE** -- Client and server agree on protocol dialect, capabilities, and security parameters (cipher suites, signing algorithms, preauth integrity)
2. **SESSION_SETUP** -- Client authenticates (NTLM or Kerberos via SPNEGO), receives a SessionID; session keys are derived and encryption/signing activated
3. **TREE_CONNECT** -- Client connects to a specific share, receives a TreeID; per-share encryption may be enforced
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
    -o port=12445,username=testuser,vers=2.0,uid=$(id -u),gid=$(id -g)
# Password will be prompted interactively

# Mount with SMB3 encryption
sudo mount -t cifs //localhost/export /mnt/smb \
    -o port=12445,username=testuser,vers=3.1.1,seal,uid=$(id -u),gid=$(id -g)
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

### SMB Negotiation and Session

| Command | Status | Notes |
|---------|--------|-------|
| NEGOTIATE | Implemented | Multi-dialect (2.0.2 through 3.1.1), negotiate contexts |
| SESSION_SETUP | Implemented | NTLM and Kerberos via SPNEGO, key derivation |
| LOGOFF | Implemented | |
| TREE_CONNECT | Implemented | Share-level permissions, per-share encryption |
| TREE_DISCONNECT | Implemented | |

### SMB File Operations

| Command | Status | Notes |
|---------|--------|-------|
| CREATE | Implemented | Files and directories, lease V2 request/grant, durable handle create contexts |
| CLOSE | Implemented | |
| FLUSH | Implemented | Flushes data to block store |
| READ | Implemented | With cache support |
| WRITE | Implemented | With cache support |
| QUERY_INFO | Implemented | Multiple info classes |
| SET_INFO | Implemented | Attributes, timestamps, rename, delete |
| QUERY_DIRECTORY | Implemented | With pagination |
| CHANGE_NOTIFY | Partial | Accepts watches, async delivery via notification queue |
| IOCTL | Implemented | VALIDATE_NEGOTIATE_INFO, FSCTL_PIPE_WAIT |
| LOCK | Implemented | Shared and exclusive byte-range locks |

### SMB3 Advanced Features

| Feature | Status | Notes |
|---------|--------|-------|
| Multi-Dialect Negotiation | Implemented | 2.0.2, 3.0, 3.0.2, 3.1.1 |
| Negotiate Contexts | Implemented | PREAUTH_INTEGRITY, ENCRYPTION, SIGNING |
| Preauth Integrity Hash | Implemented | SHA-512 chain over raw wire bytes |
| AES-GCM Encryption | Implemented | Default for 3.1.1 |
| AES-CCM Encryption | Implemented | Default for 3.0/3.0.2 |
| AES-256-GCM/CCM | Implemented | 256-bit variants |
| AES-CMAC Signing | Implemented | Default for 3.0+ |
| AES-GMAC Signing | Implemented | Preferred for 3.1.1 |
| SP800-108 KDF | Implemented | Key derivation for signing/encryption |
| VALIDATE_NEGOTIATE_INFO | Implemented | Downgrade protection for 3.0/3.0.2 |
| Leases V2 | Implemented | ParentLeaseKey, epoch tracking |
| Directory Leases | Implemented | Read-caching for directory listings |
| Durable Handles V1 | Implemented | DHnQ/DHnC with batch oplock |
| Durable Handles V2 | Implemented | DH2Q/DH2C with CreateGuid |
| Durable Handle Scavenger | Implemented | Timeout-based cleanup |
| Kerberos via SPNEGO | Implemented | Shared keytab with NFS adapter |
| Compound Requests | Implemented | CREATE+QUERY_INFO+CLOSE |
| Credit Management | Implemented | Adaptive flow control |
| Parallel Requests | Implemented | Per-connection concurrency |
| Byte-Range Locking | Implemented | Shared/exclusive locks |
| Oplocks | Implemented | Level II, Exclusive, Batch |
| Cross-Protocol Coordination | Implemented | Bidirectional lease/delegation breaks |

### Features Not Supported

| Feature | Notes |
|---------|-------|
| SMB1 | Legacy protocol, security risk |
| Compression | SMB 3.1.1 compression contexts not implemented |
| Multichannel | Multiple TCP connections per session |
| Persistent Handles | Cluster-aware handles (requires shared state) |
| RDMA | Remote Direct Memory Access transport |
| QUIC | UDP-based transport (SMB over QUIC) |
| Security Descriptors | Windows ACLs not supported |
| DFS | Distributed File System referrals |

## Implementation Details

### SMB2 Message Flow

1. TCP connection accepted
2. NetBIOS session header parsed
3. SMB2 message decoded (decrypted if transform header present)
4. Session/tree context validated
5. Command handler dispatched
6. Handler calls metadata/block stores
7. Response encoded (encrypted if session requires it) and sent

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
- `NEGOTIATE`: Multi-dialect negotiation with negotiate contexts (cipher, signing, preauth)
- `SESSION_SETUP`: NTLM or Kerberos authentication via SPNEGO, key derivation
- `TREE_CONNECT`: Share access with permission validation, per-share encryption enforcement

**File Operations** (`internal/adapter/smb/v2/handlers/`)
- `CREATE`: Create/open files and directories, lease V2 grants, durable handle create contexts
- `READ`: Read file content (with cache support)
- `WRITE`: Write file content (with cache support)
- `CLOSE`: Close file handle and cleanup
- `FLUSH`: Flush cached data to block store
- `QUERY_INFO`: Get file/directory attributes
- `SET_INFO`: Modify attributes, rename, delete
- `QUERY_DIRECTORY`: List directory contents
- `LOCK`: Acquire/release byte-range locks
- `IOCTL`: VALIDATE_NEGOTIATE_INFO, server-side copy

### Code Structure

```
NFS Implementation:              SMB Implementation:
internal/adapter/nfs/            internal/adapter/smb/
+-- dispatch.go                  +-- dispatch.go
+-- rpc/                         +-- header/
|   +-- message.go              |   +-- header.go
|   +-- reply.go                |   +-- parser.go
+-- xdr/                         |   +-- encoder.go
|   +-- reader.go               +-- auth/
|   +-- writer.go               |   +-- ntlm/
+-- types/                       |   +-- spnego/
|   +-- constants.go            +-- smbenc/
+-- mount/handlers/              |   +-- encrypt.go
|   +-- mnt.go                  |   +-- decrypt.go
|   +-- export.go               +-- signing/
+-- v3/handlers/                 |   +-- hmac.go
|   +-- lookup.go               |   +-- cmac.go
|   +-- read.go                 |   +-- gmac.go
|   +-- write.go                +-- kdf/
+-- v4/handlers/                 |   +-- sp800_108.go
|   +-- compound.go             +-- lease/
|   +-- delegation.go           |   +-- manager.go
|   +-- state/                  |   +-- notifier.go
                                 +-- types/
                                 |   +-- constants.go
                                 |   +-- status.go
                                 |   +-- filetime.go
                                 +-- v2/handlers/
                                     +-- handler.go
                                     +-- negotiate.go
                                     +-- session_setup.go
                                     +-- tree_connect.go
                                     +-- create.go
                                     +-- read.go
                                     +-- write.go
                                     +-- ioctl.go
                                     +-- durable.go
                                     ...
```

### Two-Phase Write Pattern

WRITE operations use a two-phase commit pattern:

```go
// 1. Prepare write (validate permissions, get ContentID)
writeOp, err := metadataStore.PrepareWrite(authCtx, handle, newSize)

// 2. Resolve per-share block store and write data
blockStore, _ := rt.GetBlockStoreForHandle(ctx, handle)
blockStore.WriteAt(ctx, writeOp.ContentID, data, offset)

// 3. Commit write (update metadata: size, timestamps)
metadataStore.CommitWrite(authCtx, writeOp)
```

### Block Store Integration

SMB handlers use the same per-share block store as NFS, routed through the
shared `internal/adapter/common/` helpers so NFS and SMB share one code
path for block-store resolution and pooled READ:

```go
// Resolve per-share block store from file handle
blockStore, err := common.ResolveForRead(ctx.Context, h.Registry, handle)

// Read path (pooled buffer; release fires after wire write completes)
readResult, err := common.ReadFromBlockStore(ctx.Context, blockStore,
    payloadID, offset, count)
// Response hands readResult.Release to the encoder via SMBResponseBase.ReleaseData

// Write path (data is caller-owned, no Release closure)
err := common.WriteToBlockStore(ctx.Context, blockStore, payloadID, data, offset)

// Commit path (flush + discard *FlushResult)
err := common.CommitBlockStore(ctx.Context, blockStore, payloadID)
```

These three helpers (`ReadFromBlockStore`, `WriteToBlockStore`,
`CommitBlockStore`) are the Phase-12 seam where v0.15.0 A3 (META-01 +
API-01) will plumb `[]BlockRef` into the engine — handler code does not
change.

### Error mapping

As of v0.15.0 (Phase 09 ADAPT-03), every `metadata.ErrorCode` is
translated to an `NTSTATUS` via `internal/adapter/common.MapToSMB`, which
consumes the same shared table as NFSv3 / NFSv4
(`internal/adapter/common/errmap.go`). Examples:

- `ErrNotFound` → `STATUS_OBJECT_NAME_NOT_FOUND`
- `ErrAlreadyExists` → `STATUS_OBJECT_NAME_COLLISION`
- `ErrAccessDenied` / `ErrPermissionDenied` / `ErrAuthRequired` →
  `STATUS_ACCESS_DENIED` (SMB has no EPERM distinction per MS-ERREF 2.3)
- `ErrIsDirectory` → `STATUS_FILE_IS_A_DIRECTORY`
- `ErrStaleHandle` → `STATUS_FILE_CLOSED`

#### Lock-context vs general-context divergence

Lock-operation errors (SMB2 LOCK requests) use a separate accessor
`common.MapLockToSMB` backed by `internal/adapter/common/lock_errmap.go`.
The divergence matters: **`ErrLocked` in lock context →
`STATUS_LOCK_NOT_GRANTED`; `ErrLocked` in general READ/WRITE I/O context →
`STATUS_FILE_LOCK_CONFLICT`**. Clients react differently to the two codes
(retry-later vs. hard-fail-with-indication), so the distinction is wire-
visible.

See `internal/adapter/common/lock_errmap.go` for the full lock-context
override table.

#### Wrapped error unwrapping

`common.MapToSMB` uses `errors.As`, so wrapped `StoreError` values
(`fmt.Errorf("context: %w", storeErr)`) unwrap correctly. Prior to
v0.15.0 the SMB handler used an unwrapped type assertion that failed on
wrapped errors and fell through to `STATUS_INTERNAL_ERROR` — the
consolidation fixed that latent bug.

### READ response buffer pool

SMB2 READ responses for regular files allocate the data buffer through
`internal/adapter/pool` (4 KB / 64 KB / 1 MB tiered `sync.Pool`, with a
direct-alloc fallback for sizes above `LargeSize`). The pooled buffer is
handed to the response encoder via `SMBResponseBase.ReleaseData` (a
`func()` field); the encoder invokes the closure after
`WriteNetBIOSFrame` returns, safe across plain, encrypted, and compound-
response paths. Non-pooled responses leave `ReleaseData` nil and the
encoder null-checks before invoking.

Pipe and symlink READ variants deliberately stay on heap allocations —
memcpy overhead with no reuse benefit for the small buffer sizes
involved, and pipes have an ownership model that conflicts with a
pool-managed return buffer. Regression tests
(`TestRead_PipeRead_LeavesReleaseDataNil` / `TestRead_SymlinkRead_...`)
guard the non-pool decision.

### Credit Flow Control

SMB2 uses credits (MS-SMB2 3.3.1.2) as the protocol-level flow-control
mechanism. Each request consumes credits equal to its `CreditCharge`; each
response grants credits via the `CreditResponse` header field. The client
tracks a per-connection running balance (`cur_credits`) and will refuse to
send a request once its balance would go negative, or reject a response
whose grant would overflow its 16-bit counter. Both outcomes look like
`NT_STATUS_INTERNAL_ERROR` or `NT_STATUS_INVALID_NETWORK_RESPONSE` on the
wire, so credit accounting must be byte-for-byte consistent between the
server's window and the client's counter.

#### Defaults

```go
type CreditConfig struct {
    MinGrant          uint16  // Minimum credits per response (1)
    MaxGrant          uint16  // Maximum credits per response (8192)
    InitialGrant      uint16  // Floor when client requests 0 (1)
    MaxSessionCredits uint32  // Per-connection window cap (8192)
}
```

The defaults match Samba's server (`smb2 max credits = 8192`, initial
grant = 1 in `source3/smbd/smb2_server.c`) and Windows Server 2008R2+.
These are the protocol-level invariants clients expect; tuning them
higher can break interoperability.

#### Server data structure — `CommandSequenceWindow`

One per connection. Tracks granted message IDs as a sliding bitmap
(`internal/adapter/smb/session/sequence_window.go`):

```
low           high
 │    span=high-low    │
 ▼                     ▼
[0111100011001110000...]  bit i = sequence (low+i) is granted-and-unconsumed
                           set by Grant, cleared by Consume

available   = the server's view of the client's cur_credits
              (initially equal to popcount(bitmap); decoupled by Reclaim)
```

Three invariants drive correctness:

1. **`available` mirrors the client's `cur_credits`.** Every `Grant(N)`
   increments `available` by the amount the server actually extended the
   window; every `Consume(msgId, charge)` decrements `available` by
   `charge`. The server never grants more than `MaxSessionCredits -
   available`, so the client's counter can never overflow.
2. **`low` advances lazily in 64-bit blocks.** `advanceLow` reclaims
   bitmap words once an entire 64-sequence run has been consumed. The
   `available` counter is the authoritative credit tally; the bitmap
   span (`high - low`) can briefly exceed `available` when the oldest
   unconsumed bit is still in place, but stays bounded because
   `available` gates new grants.
3. **Credit-exempt commands still consume sequence numbers.** MS-SMB2
   exempts `NEGOTIATE`, `CANCEL`, and the first `SESSION_SETUP`
   (`SessionID=0`) from credit *validation*, but the client still
   advances its msgId and decrements `cur_credits` for them. The server
   therefore MUST call `Consume` on those messages too — otherwise
   `available` drifts up by one per credit-exempt request, saturates at
   `MaxSessionCredits`, and future responses carry `credits=0` until
   the client runs out of credits (observed in issue #378).

##### Reclaim — compound response zeroing

MS-SMB2 3.2.4.1.4 requires middle responses in a compound to advertise
`Credits=0`. Our response builder grants credits atomically before the
write (see below), so after zeroing the middle headers the window would
be over-extended relative to what the client was told. `Reclaim(n)`
decrements `available` by `n` without touching the bitmap — the
reclaimed message IDs remain valid on the server (a misbehaving client
that sent one would still pass Consume), but the client was never told
about them and will not use them under normal operation. `Consume`
saturates `available` at zero rather than underflowing if a reclaimed
message ID is used anyway.

#### Grant path — atomic, pre-write

```
GrantCredits (per-session policy)   →  credits (requested grant)
  └─ strategy-dependent (echo/fixed/adaptive)

CommandSequenceWindow.Grant(credits) →  credits' (may be less; ≤ MaxSessionCredits - available)
  └─ extends the window and updates `available` atomically under w.mu

respHeader.Credits = credits'
...send response...
```

The grant is recorded against the window **before** the response is
written, and the grant function returns the actual amount extended, so
the value advertised in `hdr.Credits` is always exactly what the window
was extended by. This closes the TOCTOU gap that a "read `Remaining()`,
clamp, write, then `Grant()`" pattern would leave open when pipelined
responses run on the same connection. All response build sites funnel
through `grantConnectionCredits` in `internal/adapter/smb/response.go`.

#### Strategies

- **Echo** (default): grant what the client requests, bounded by
  `[MinGrant, MaxGrant]` and `Remaining()`. Matches Samba's
  `smb2_set_operation_credit`: `grant = credit_charge + (requested − 1)`.
- **Fixed**: always grant `InitialGrant`.
- **Adaptive**: `InitialGrant` scaled by live load and client-outstanding
  factors. More aggressive than Echo, primarily useful when throughput
  matters more than strict Samba interop.

#### Interoperability notes

- **Samba client** hard-caps `cur_credits` at `uint16` max (65535) and
  rejects any response that would overflow. Prior to #378 we advertised
  ~384 credits per response (InitialGrant=256 × adaptive 1.5× boost),
  which saturated the client after ~85 SESSION_SETUP iterations and
  triggered `NT_STATUS_INVALID_NETWORK_RESPONSE`. The fix lowered defaults
  to Samba-compatible values and enforced `Remaining()` clamping at every
  response build site.
- **Windows client** is more tolerant but grants are capped by the
  negotiated `Connection.MaxCredits`; setting `MaxSessionCredits > 8192`
  gains nothing because Windows caps at 8192 by default too.
- **Multi-credit operations** (large READ/WRITE) consume `CreditCharge`
  sequence numbers per request; the window handles charge > 1 natively.

Reference:
- MS-SMB2 3.3.1.2 (Server Credit Tracking)
- Samba `source3/smbd/smb2_server.c` `smb2_set_operation_credit` and
  surrounding bitmap bookkeeping
- Samba client check: `libcli/smb/smbXcli_base.c:4295-4298`

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

## SMB3 Dialect Negotiation

### Overview

SMB3 dialect negotiation determines the protocol version, cipher suite, signing algorithm, and preauth integrity mechanism used for the session. The server selects the highest mutually supported dialect and communicates security capabilities via negotiate contexts.

### Dialect Selection

The NEGOTIATE request contains a list of dialect revisions supported by the client. The server selects the highest dialect both sides support:

| Priority | Dialect | Hex | Key Capability |
|----------|---------|-----|----------------|
| 1 (highest) | SMB 3.1.1 | 0x0311 | Preauth integrity, negotiate contexts |
| 2 | SMB 3.0.2 | 0x0302 | VALIDATE_NEGOTIATE_INFO |
| 3 | SMB 3.0 | 0x0300 | Encryption (AES-CCM), CMAC signing |
| 4 (lowest) | SMB 2.0.2 | 0x0202 | Basic SMB2 operations |

### Negotiate Contexts (SMB 3.1.1)

When the negotiated dialect is 3.1.1, both client and server exchange **negotiate contexts** that specify security parameters:

**SMB2_PREAUTH_INTEGRITY_CAPABILITIES:**
- Hash algorithm: SHA-512 (mandatory)
- Salt: random 32-byte value per side
- Purpose: preauth integrity hash chain for downgrade protection

**SMB2_ENCRYPTION_CAPABILITIES:**
- Supported ciphers in preference order
- Server selects the first mutually supported cipher

**SMB2_SIGNING_CAPABILITIES:**
- Supported signing algorithms in preference order
- Server selects the first mutually supported algorithm

### Preauth Integrity Hash Chain

For SMB 3.1.1, a running SHA-512 hash is computed over the raw NEGOTIATE and SESSION_SETUP request/response bytes:

```
PreauthHash[0] = SHA-512(Salt || NEGOTIATE_REQUEST_bytes)
PreauthHash[1] = SHA-512(PreauthHash[0] || NEGOTIATE_RESPONSE_bytes)
PreauthHash[2] = SHA-512(PreauthHash[1] || SESSION_SETUP_REQUEST_bytes)
...
```

This hash chain serves as the KDF context for key derivation (see [Key Derivation](#key-derivation)), binding the session keys to the exact negotiate exchange. Any man-in-the-middle modification of the negotiate messages produces different keys, causing authentication to fail.

### Server Cipher and Signing Preference

DittoFS uses the following default preference order:

**Cipher preference** (configurable):
1. AES-128-GCM (0x0002) -- fastest on modern hardware with AES-NI
2. AES-128-CCM (0x0001) -- fallback for 3.0/3.0.2
3. AES-256-GCM (0x0004) -- higher security, slightly slower
4. AES-256-CCM (0x0003) -- highest security AES-CCM variant

**Signing preference** (configurable):
1. AES-128-GMAC (0x0002) -- fastest for 3.1.1
2. AES-128-CMAC (0x0001) -- required for 3.0+
3. HMAC-SHA256 -- legacy for 2.x clients

### FSCTL_VALIDATE_NEGOTIATE_INFO (Downgrade Protection)

For SMB 3.0 and 3.0.2 (which lack the preauth integrity hash chain), the client sends an `FSCTL_VALIDATE_NEGOTIATE_INFO` IOCTL after tree connect. The server validates that the negotiate parameters match what was originally negotiated:

- Client sends: Capabilities, GUID, SecurityMode, requested Dialects
- Server compares against stored negotiate state
- If any field mismatches: **connection is dropped** (potential MITM downgrade)
- For SMB 3.1.1: this IOCTL is not needed (preauth hash provides stronger protection). DittoFS drops the TCP connection if a 3.1.1 client sends it, per MS-SMB2 Section 3.3.5.15.12.

### Wire Format: Negotiate Request

```
NEGOTIATE Request (variable):
  StructureSize:     36
  DialectCount:      N (number of dialects)
  SecurityMode:      flags (SIGNING_ENABLED, SIGNING_REQUIRED)
  Reserved:          0
  Capabilities:      flags
  ClientGuid:        16 bytes
  NegContextOffset:  offset to negotiate contexts (3.1.1 only)
  NegContextCount:   number of contexts (3.1.1 only)
  Dialects[]:        array of uint16 dialect revisions
  NegContextList[]:  padded negotiate context structures (3.1.1 only)
```

### Configuration

```yaml
adapters:
  smb:
    # Dialect selection (optional, default: all supported)
    min_dialect: "3.0"     # Reject clients below this dialect
    max_dialect: "3.1.1"   # Maximum dialect to negotiate
```

## Encryption

### Overview

SMB3 encryption provides **confidentiality and integrity** for all messages on an encrypted session using AEAD (Authenticated Encryption with Associated Data) ciphers. Encryption wraps the entire SMB2 message (header + body) in a Transform Header.

### Cipher Suites

| Cipher | ID | Default For | Key Size | Nonce Size | Tag Size |
|--------|-----|-------------|----------|------------|----------|
| AES-128-CCM | 0x0001 | SMB 3.0, 3.0.2 | 128-bit | 11 bytes | 16 bytes |
| AES-128-GCM | 0x0002 | SMB 3.1.1 | 128-bit | 12 bytes | 16 bytes |
| AES-256-CCM | 0x0003 | -- | 256-bit | 11 bytes | 16 bytes |
| AES-256-GCM | 0x0004 | -- | 256-bit | 12 bytes | 16 bytes |

**AES-GCM** is preferred for SMB 3.1.1 due to hardware acceleration (AES-NI + CLMUL) on modern CPUs. **AES-CCM** is the mandatory cipher for SMB 3.0 and 3.0.2 compatibility.

### Transform Header

Encrypted messages use the `0xFD 'S' 'M' 'B'` magic (vs `0xFE 'S' 'M' 'B'` for unencrypted):

```
Transform Header (52 bytes):
  ProtocolID:         0xFD534D42 (4 bytes)
  Signature:          AES-GCM/CCM authentication tag (16 bytes)
  Nonce:              AES-GCM/CCM nonce (16 bytes, left-padded with zeros)
  OriginalMessageSize: uint32 (4 bytes)
  Reserved:           uint16 (2 bytes)
  Flags:              uint16 (2 bytes) -- 0x0001 = encrypted
  SessionId:          uint64 (8 bytes)
```

The **AAD (Additional Authenticated Data)** for the AEAD cipher is the 20 bytes of the transform header starting from the Nonce field through SessionId (bytes 20-51). This ensures the session binding and message size cannot be tampered with.

### Encryption Enforcement

DittoFS supports three encryption modes:

| Mode | Behavior |
|------|----------|
| `disabled` | No encryption for any session |
| `preferred` | Encrypt SMB 3.x sessions that support it; allow unencrypted 2.x |
| `required` | Reject SMB 2.x clients; encrypt all SMB 3.x sessions |

**Per-session encryption** (`Session.EncryptData`): When mode is `preferred` or `required`, sessions negotiating SMB 3.x have `SMB2_SESSION_FLAG_ENCRYPT_DATA` set in SESSION_SETUP response. All subsequent messages on the session are encrypted.

**Per-share encryption** (`Share.EncryptData`): Individual shares can require encryption via the `encrypt_data` flag in share configuration. When set, `SMB2_SHAREFLAG_ENCRYPT_DATA` is returned in TREE_CONNECT response.

**Guest sessions**: Never encrypted because guest sessions have no session key for key derivation.

### Configuration

```yaml
adapters:
  smb:
    encryption:
      encryption_mode: preferred   # disabled | preferred | required
      allowed_ciphers: []          # Empty = all in default order
      # Custom cipher preference: [AES-128-GCM, AES-128-CCM]
```

See [docs/CONFIGURATION.md](CONFIGURATION.md) for complete encryption configuration options.
See [docs/SECURITY.md](SECURITY.md) for security implications and recommendations.

## Signing

### Overview

SMB message signing provides **integrity protection** against man-in-the-middle attacks and message tampering. The signature is computed over the SMB2 header and body, placed in the 16-byte Signature field of the SMB2 header.

### Signing Algorithms by Dialect

| Dialect | Algorithm | Key Derivation |
|---------|-----------|----------------|
| SMB 2.0.2 | HMAC-SHA256 | Direct from session key |
| SMB 3.0 | AES-128-CMAC | SP800-108 KDF |
| SMB 3.0.2 | AES-128-CMAC | SP800-108 KDF |
| SMB 3.1.1 | AES-128-GMAC (preferred) or AES-128-CMAC | SP800-108 KDF with preauth hash |

**AES-128-GMAC** is the preferred signing algorithm for SMB 3.1.1 because it leverages the same GCM hardware acceleration as encryption. If a 3.1.1 client omits the SIGNING_CAPABILITIES negotiate context, the server defaults to AES-128-CMAC per specification.

### Signing Algorithm Selection

The signing algorithm is determined by the negotiated dialect and negotiate contexts:

1. **SMB 2.0.2**: Always HMAC-SHA256 (no negotiation)
2. **SMB 3.0/3.0.2**: Always AES-128-CMAC (no negotiation)
3. **SMB 3.1.1 with SIGNING_CAPABILITIES**: First mutually supported algorithm from server preference list
4. **SMB 3.1.1 without SIGNING_CAPABILITIES**: Default to AES-128-CMAC

### SP800-108 Counter Mode KDF for Signing Keys

For SMB 3.0+, the signing key is derived from the session key using NIST SP800-108 in Counter Mode with HMAC-SHA256 as the PRF:

```
SigningKey = KDF(SessionKey, Label, Context)

Where:
  PRF = HMAC-SHA256
  Key = SessionKey (from authentication)
  Label = "SMBSigningKey\0" (null-terminated)
  Context = varies by dialect (see Key Derivation section)
```

### When Signing Is Required vs Optional

- **NEGOTIATE**: Never signed (no session key yet)
- **SESSION_SETUP**: Final response can be signed (to prove server identity)
- **After SESSION_SETUP**: All messages signed when signing is enabled for the session
- **Encrypted messages**: Signing is redundant when encryption is active (AEAD provides integrity), but DittoFS still signs to match Windows Server behavior

### Configuration

```yaml
adapters:
  smb:
    signing:
      enabled: true      # Advertise signing capability
      required: false     # Require all clients to sign
      # Signing algorithm preference (for 3.1.1 negotiate context)
      # Default: [AES-128-GMAC, AES-128-CMAC]
      preferred_algorithms: []
```

## Key Derivation

### Overview

SMB3 uses NIST SP800-108 Counter Mode KDF with HMAC-SHA256 as the PRF to derive per-purpose cryptographic keys from the session key obtained during authentication.

### SP800-108 Algorithm

```
KDF-HMAC-SHA256(Key, Label, Context):
  i = 1
  L = keyLength * 8 (in bits)
  result = PRF(Key, i || Label || 0x00 || Context || L)
  return result[0:keyLength]
```

Where `||` denotes concatenation and `PRF` is HMAC-SHA256.

### Key Purposes

Four keys are derived per session:

| Key | Label (null-terminated) | Usage |
|-----|------------------------|-------|
| SigningKey | `"SMBSigningKey\0"` | Message signing (HMAC/CMAC/GMAC) |
| EncryptionKey | `"SMBS2CCipherKey\0"` (3.0) / `"SMBServerEncryptionKey\0"` (3.1.1) | Server-to-client encryption |
| DecryptionKey | `"SMBC2SCipherKey\0"` (3.0) / `"SMBClientEncryptionKey\0"` (3.1.1) | Client-to-server decryption |
| ApplicationKey | `"SMBAppKey\0"` | Application-level use |

### Context by Dialect

| Dialect | KDF Context |
|---------|-------------|
| SMB 3.0 | `"SmbSign\0"` / `"ServerIn \0"` / `"ServerOut\0"` (fixed strings) |
| SMB 3.0.2 | Same as 3.0 |
| SMB 3.1.1 | Preauth integrity hash value (SHA-512 hash chain output) |

The use of the preauth integrity hash as KDF context in 3.1.1 is critical for security: it cryptographically binds the derived keys to the exact negotiate exchange, preventing downgrade attacks where a MITM strips security capabilities.

### Key Length

For 128-bit ciphers (AES-128-GCM, AES-128-CCM, AES-128-CMAC, AES-128-GMAC), the derived key is 16 bytes. For 256-bit ciphers (AES-256-GCM, AES-256-CCM), the derived key is 32 bytes; the session key is required to be at least 32 bytes (achieved by hashing with SHA-256 if needed).

## Leases V2 and Directory Leasing

### Overview

Leases V2 extend SMB2.1 lease functionality with **ParentLeaseKey** tracking and **epoch-based** stale break prevention. Directory leasing adds **Read-caching** for directory listings, reducing QUERY_DIRECTORY round trips.

### Lease V2 vs V1

| Feature | Lease V1 (SMB 2.1) | Lease V2 (SMB 3.0+) |
|---------|--------------------|--------------------|
| ParentLeaseKey | Not available | Links child to parent directory lease |
| Epoch | Not available | Monotonic counter for stale break detection |
| Directory Leases | Not supported | Read-caching on directories |
| Create Context | SMB2_CREATE_REQUEST_LEASE | SMB2_CREATE_REQUEST_LEASE_V2 |

### Lease States

Leases use a combination of three caching flags:

| Flag | Abbreviation | Description |
|------|-------------|-------------|
| Read | R | Client may cache read data without revalidating |
| Write | W | Client may cache writes and defer flushing to server |
| Handle | H | Client may cache the file handle and defer CLOSE |

Common state combinations:

| State | Flags | Typical Use |
|-------|-------|-------------|
| None | -- | No caching |
| Read | R | Shared read caching (multiple clients) |
| Read-Handle | RH | Read caching with handle caching |
| Read-Write | RW | Exclusive read/write caching |
| Read-Write-Handle | RWH | Full exclusive caching (most aggressive) |

### Lease State Machine

```
Grant:    None -> R (shared read)
          None -> RWH (exclusive, single opener)

Break:    RWH -> RH  (another client opens for read)
          RWH -> None (another client opens for write)
          RH  -> R   (handle caching revoked)
          R   -> None (all caching revoked)
```

Break is initiated by the server when a conflicting open arrives. The original client must acknowledge the break and flush cached data before the new open proceeds.

### Directory Leases

Directory leases grant **Read-caching** on a directory, allowing the client to cache directory listings locally:

- **Granted**: When a client opens a directory with a lease V2 create context
- **Cached data**: QUERY_DIRECTORY results are cached client-side
- **Break trigger**: Any modification to the directory's contents (create, delete, rename)
- **Break target**: Always breaks to None (directory leases only support Read state)

Directory lease breaks are triggered by the `MetadataService` when `CreateFile`, `RemoveFile`, or `Rename` modifies a directory. The break flows through the `LockManager.CheckAndBreakDirectoryCaching()` method.

### Epoch-Based Stale Break Prevention

Each lease V2 has a monotonic **epoch** counter that increments on every state change. When a lease break notification is sent, it includes the current epoch. If the client sends a break acknowledgment with a stale epoch (lower than current), the server knows the client missed an intermediate break and can take corrective action.

### ParentLeaseKey

Lease V2 includes a `ParentLeaseKey` that associates the file's lease with its parent directory's lease. When a file operation triggers a directory lease break, the server can identify which parent directory leases need to be broken by matching `ParentLeaseKey` values.

### Configuration

```yaml
adapters:
  smb:
    leases:
      enabled: true              # Enable lease support
      directory_leases: true     # Enable directory leasing
      lease_break_timeout: 35s   # Time to wait for break acknowledgment
```

## Durable Handles

### Overview

Durable handles allow SMB clients to **reconnect and resume file operations** after a network disconnection without losing cached state. DittoFS implements both V1 and V2 durable handles per the MS-SMB2 specification.

### Durable Handle V1 (DHnQ/DHnC)

V1 durable handles (SMB 2.0.2+) require a batch oplock:

- **DHnQ (Durable Handle Request)**: Client requests a durable handle in CREATE
- **DHnC (Durable Handle Reconnect)**: Client reconnects to a preserved handle
- **Requirement**: The file must have been opened with a batch oplock grant
- **Limitation**: No idempotent reconnection (duplicate reconnects may fail)

### Durable Handle V2 (DH2Q/DH2C)

V2 durable handles (SMB 3.0+) add `CreateGuid` for idempotent reconnection:

- **DH2Q (Durable Handle V2 Request)**: Client provides a `CreateGuid` (16-byte GUID)
- **DH2C (Durable Handle V2 Reconnect)**: Client provides `CreateGuid` for matching
- **No oplock requirement**: V2 handles do not require batch oplock
- **Idempotent**: Multiple reconnect attempts with the same `CreateGuid` succeed
- **Precedence**: When both V1 and V2 create contexts are present, V2 takes precedence per MS-SMB2

### Reconnect Validation

V2 reconnect performs 14+ validation checks per MS-SMB2 specification:

1. Look up handle by `CreateGuid`
2. Verify handle is in disconnected/durable state
3. Verify requesting user matches original creator
4. Verify file name matches
5. Verify session key hash matches (SHA-256 of signing key)
6. Verify share name matches
7. Verify handle has not timed out
8. Verify no conflicting opens exist
9. ... (additional checks per spec)

If all checks pass, the handle is restored to the new session. The `IsDurable` flag is NOT set on the restored handle -- the client must re-request durability after reconnect.

### Handle Timeout and Scavenger

- **Default timeout**: 60 seconds (configurable)
- **Scavenger interval**: Periodic background goroutine scans for expired handles
- **Cleanup**: Expired handles are cleaned up (pending I/O cancelled, locks released, handle removed from store)
- **Scavenger lifecycle**: Tied to `Serve` context -- stops automatically on adapter shutdown

### App Instance ID

V2 durable handles support an optional **App Instance ID** (16-byte GUID) for cluster failover scenarios. When a client reconnects from a different cluster node with the same App Instance ID, the server can close the old handle and transfer state to the new session.

### Wire Format: Create Context

```
DH2Q Create Context (Durable Handle V2 Request):
  Timeout:        uint32 (requested timeout in milliseconds)
  Flags:          uint32 (PERSISTENT flag for persistent handles)
  Reserved:       8 bytes
  CreateGuid:     16 bytes (client-generated GUID)

DH2C Create Context (Durable Handle V2 Reconnect):
  FileId:         16 bytes (persistent + volatile)
  CreateGuid:     16 bytes (must match original DH2Q)
  Flags:          uint32
```

### Configuration

```yaml
adapters:
  smb:
    durable_handles:
      enabled: true                  # Enable durable handle support
      default_timeout: 60s           # Default handle preservation timeout
      scavenger_interval: 30s        # How often to scan for expired handles
      max_handles_per_session: 1000  # Limit per session
```

## Kerberos and SPNEGO Authentication

### Overview

DittoFS supports Kerberos authentication for SMB clients through the SPNEGO (Simple and Protected GSSAPI Negotiation Mechanism) protocol during SESSION_SETUP. The Kerberos provider is shared between NFS (RPCSEC_GSS) and SMB (SPNEGO) adapters.

### SPNEGO Negotiation Flow

```
Client                              Server
  |                                    |
  |--- NEGOTIATE (SecurityBuffer) ---->|
  |<-- NEGOTIATE Response (mechTypes) -|
  |                                    |
  |--- SESSION_SETUP (SPNEGO Init) --->|
  |    Contains: mechToken (AP-REQ)    |
  |    or NTLM Negotiate               |
  |                                    |
  |<-- SESSION_SETUP Response ---------|
  |    Contains: mechToken (AP-REP)    |
  |    or NTLM Challenge               |
  |    Status: MORE_PROCESSING (NTLM)  |
  |    or SUCCESS (Kerberos)           |
  |                                    |
  [NTLM only: additional round-trip]   |
  |--- SESSION_SETUP (NTLM Auth) ----->|
  |<-- SESSION_SETUP (SUCCESS) --------|
```

The SPNEGO wrapper advertises both Kerberos and NTLM mechanism OIDs. Clients with valid Kerberos tickets choose Kerberos for single round-trip authentication.

### Kerberos Session Setup

1. **Client** obtains TGT from KDC, then requests service ticket for `cifs/server.example.com@REALM`
2. **Client** sends AP-REQ inside SPNEGO InitToken in SESSION_SETUP
3. **Server** validates AP-REQ against keytab, extracts session key
4. **Server** sends AP-REP (mutual authentication) inside SPNEGO Response
5. **Session key** from Kerberos is used as input to SP800-108 KDF for signing/encryption keys

### Session Key Extraction for KDF

The Kerberos session key (from AP-REQ validation) becomes the **base session key** for the SP800-108 KDF. This key is then used to derive:
- SigningKey (for AES-CMAC/GMAC message signing)
- EncryptionKey (for AES-GCM/CCM encryption)
- DecryptionKey (for AES-GCM/CCM decryption)

### NTLM Fallback

When Kerberos is not available (no keytab configured, client has no valid TGT, or DNS resolution fails), the server falls back to NTLM authentication:

1. Client sends NTLM Negotiate message
2. Server responds with NTLM Challenge
3. Client sends NTLM Authenticate with NTProofStr
4. Server validates against stored password hash

NTLM provides weaker security than Kerberos: no mutual authentication, vulnerable to relay attacks, and the session key is derived from the password hash rather than a fresh Kerberos session key.

### Guest Sessions

When authentication fails and guest access is enabled:
- Session is created with guest privileges
- **No signing**: Guest sessions cannot sign messages (no session key)
- **No encryption**: Guest sessions cannot be encrypted (no key for KDF)
- Security implications: guest access should be limited to read-only public shares

### Keytab Management and Hot-Reload

DittoFS uses a shared Kerberos keytab for both NFS and SMB:

```yaml
kerberos:
  enabled: true
  keytab_path: /etc/dittofs/dittofs.keytab
  service_principal: nfs/server.example.com@EXAMPLE.COM
```

The server automatically derives the `cifs/` service principal from the configured `nfs/` principal for SMB authentication. The keytab supports **hot-reload**: when the file is replaced on disk, the server detects the change and loads the new key without restart.

See [docs/SECURITY.md](SECURITY.md) for detailed Kerberos security considerations.

## Cross-Protocol Behavior

DittoFS supports simultaneous NFS and SMB access to the same files and directories. This section documents how the protocols interact through the unified LockManager in `pkg/metadata/lock/`.

### Cross-Protocol Behavior Matrix

The following table shows what happens when an operation from one protocol encounters active caching state from the other protocol:

**NFS operation encountering SMB state:**

| NFS Operation | SMB Read Lease (R) | SMB Write Lease (RW/RWH) | SMB Dir Lease |
|---------------|--------------------|-----------------------------|---------------|
| **READ** | Coexists | Break to None, wait for ack | -- |
| **WRITE** | Break to None | Break to None, wait for ack | -- |
| **CREATE** | -- | -- | Break directory lease |
| **REMOVE** | Break to None | Break to None, wait for ack | Break directory lease |
| **RENAME** | Break to None (src + dst) | Break to None, wait for ack | Break both src and dst dir leases |
| **LINK** | -- | -- | Break target directory lease |
| **SETATTR (file)** | -- | Break to None | -- |
| **OPEN (delegation grant)** | Check coexistence | Conflict: break lease first | -- |

**SMB operation encountering NFS state:**

| SMB Operation | NFS Read Deleg | NFS Write Deleg | NFS Dir Deleg |
|---------------|----------------|-----------------|---------------|
| **CREATE (read)** | Coexists | CB_RECALL, wait | -- |
| **CREATE (write)** | CB_RECALL, wait | CB_RECALL, wait | -- |
| **WRITE** | CB_RECALL, wait | CB_RECALL, wait | -- |
| **DELETE** | CB_RECALL, wait | CB_RECALL, wait | -- |
| **RENAME** | CB_RECALL (src + dst) | CB_RECALL, wait | CB_RECALL both dirs |
| **CREATE (in dir)** | -- | -- | CB_RECALL + CB_NOTIFY |
| **DELETE (in dir)** | -- | -- | CB_RECALL + CB_NOTIFY |
| **QUERY_DIR (lease req)** | -- | -- | Check coexistence |

### Coexistence Rules

| NFS State | SMB State | Result | Rationale |
|-----------|-----------|--------|-----------|
| Read delegation | Read lease (R) | **Coexist** | Both are read-only caching; no data conflict |
| Read delegation | Write lease (RW/RWH) | **Conflict** | Write lease allows cached writes that read delegation won't see |
| Write delegation | Any lease | **Conflict** | Write delegation implies exclusive write caching |
| Any delegation | Write lease | **Conflict** | Write lease implies exclusive write caching |
| Dir delegation | Dir lease | **Coexist** | Both are read-only directory caching |

### Break Flow: SMB Write Triggers NFS Delegation Recall

```
SMB Client                    LockManager                     NFS Client
    |                              |                               |
    |-- CREATE (write) ---------->|                               |
    |                              |-- CheckAndBreakCachingForWrite |
    |                              |   find NFS read delegation    |
    |                              |   mark delegation.Breaking    |
    |                              |-- OnDelegationRecall -------->|
    |                              |   (via NFSBreakHandler)       |
    |                              |                 CB_RECALL --->|
    |                              |                               |
    |                              |<---- DELEGRETURN -------------|
    |                              |   delegation removed          |
    |<-- CREATE response ----------|                               |
```

### Break Flow: NFS Open Triggers SMB Lease Break

```
NFS Client                    LockManager                     SMB Client
    |                              |                               |
    |-- OPEN (write) ------------>|                               |
    |                              |-- CheckAndBreakCachingForWrite |
    |                              |   find SMB RWH lease          |
    |                              |   mark lease.Breaking         |
    |                              |-- OnOpLockBreak ------------->|
    |                              |   (via SMBBreakHandler)       |
    |                              |             LEASE_BREAK ----->|
    |                              |                               |
    |                              |<---- LEASE_BREAK_ACK ---------|
    |                              |   lease downgraded/removed    |
    |<-- OPEN response ------------|                               |
```

### Directory Change Coordination

When a file is created, deleted, or renamed, the MetadataService triggers directory caching breaks for the parent directory:

```
Any Client                    MetadataService               LockManager
    |                              |                              |
    |-- CREATE file in /dir ------>|                              |
    |                              |-- notifyDirChange("/dir") -->|
    |                              |                              |
    |                              |   CheckAndBreakDirectoryCaching:
    |                              |   1. Break SMB dir leases    |
    |                              |   2. Break NFS dir delegations
    |                              |   3. Queue DirNotification   |
    |                              |      (type=Add, name=file)   |
    |                              |                              |
    |                              |   Consumers:                 |
    |                              |   - SMB: CHANGE_NOTIFY       |
    |                              |   - NFS: CB_NOTIFY           |
```

**RENAME across directories** breaks both source and target directory leases and delegations.

### Anti-Storm Mechanism

To prevent rapid grant-break-grant-break cycles (lease/delegation storms), the LockManager maintains a unified `recentlyBrokenCache` with a configurable TTL (default 30 seconds):

1. When a lease or delegation is broken, the file handle is marked in the cache
2. Subsequent lease/delegation grant requests check the cache
3. If the handle was recently broken, the grant is denied (client retries later)
4. The TTL applies cross-protocol: an NFS delegation broken due to SMB activity prevents NFS re-grant for the TTL duration, and vice versa

### Notification Queue

Directory change notifications are queued in a bounded notification queue owned by the LockManager:

- **Capacity**: 1024 events per directory (configurable)
- **Overflow**: Collapses to a single "full rescan needed" event
- **Flush**: Triggered by size threshold (100 events) or time threshold (500ms)
- **Consumers**: NFS adapter drains into CB_NOTIFY; SMB adapter drains into CHANGE_NOTIFY
- **Event types**: Add, Remove, Rename, Modify (with entry name and old/new names for rename)

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

- **Leases preferred**: SMB3 clients should use Lease V2 instead of traditional oplocks
- **In-memory tracking**: Oplock state is lost on server restart
- **Single holder tracking**: Only tracks one Level II holder (others coexist but are not tracked)

## Change Notifications

DittoFS implements CHANGE_NOTIFY support per [MS-SMB2] 2.2.35/2.2.36, with directory change events delivered through the unified notification queue.

### Current Status

The implementation accepts CHANGE_NOTIFY requests and delivers change events through the LockManager's notification queue:

- **Watch Registration**: Clients can register directory watches with completion filters
- **Change Detection**: CREATE, CLOSE (delete-on-close), SET_INFO (rename), and cross-protocol operations trigger change events
- **Notification Queue**: Events are queued and delivered to registered watchers via the LockManager

### How It Works

```
Client registers CHANGE_NOTIFY -> STATUS_PENDING
  |
MetadataService detects change -> LockManager.notifyDirChange()
  |
LockManager queues DirNotification -> flush to registered consumers
  |
SMB adapter delivers CHANGE_NOTIFY response with FILE_NOTIFY_INFORMATION
```

### Completion Filter Support

The following filters are recognized:

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

# Run cross-protocol lease/delegation tests
sudo go test -tags=e2e -v ./test/e2e/ -run TestCrossProtocol
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
2. Check block store connectivity (S3, filesystem)
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

### Cross-Protocol Issues

See [docs/TROUBLESHOOTING.md](TROUBLESHOOTING.md) for cross-protocol troubleshooting, including:
- File locked by another protocol
- Delegation recall timeouts
- Lease break storms
- Stale data after cross-protocol writes

## Known Limitations

### Protocol Scope

1. **No SMB1 support**: Legacy protocol, not implemented for security reasons
2. **No compression**: SMB 3.1.1 compression contexts are not implemented
3. **No multichannel**: Multiple TCP connections per session not supported
4. **No persistent handles**: Cluster-aware handles require shared state infrastructure
5. **No RDMA transport**: Remote Direct Memory Access not supported
6. **No QUIC transport**: SMB over QUIC (UDP) not supported
7. **No security descriptors**: Windows ACLs not supported (uses POSIX permissions)
8. **No DFS referrals**: Distributed File System not supported

### Operational Limitations

9. **Ephemeral locks and oplocks**: Both byte-range locks and oplocks are in-memory only, lost on server restart
10. **No blocking locks**: Lock requests fail immediately if conflicting lock exists
11. **Single-node only**: No clustering or high availability for SMB state
12. **Durable handle state is in-memory**: Durable handles survive disconnection but not server restart (BadgerDB/PostgreSQL stores persist handle metadata but in-memory state is lost)

### SMB3 Feature Gaps

13. **No extended attributes (xattrs)**: EA support not implemented
14. **No server-side copy offload**: FSCTL_SRV_COPYCHUNK not implemented
15. **No per-file encryption**: Encryption is per-session or per-share only

## Glossary

| Term | Definition |
|------|------------|
| **AEAD** | Authenticated Encryption with Associated Data -- encryption providing both confidentiality and integrity (AES-GCM, AES-CCM) |
| **ACL** | Access Control List -- Windows permission model |
| **AES-CCM** | AES in Counter with CBC-MAC mode -- AEAD cipher for SMB 3.0/3.0.2 |
| **AES-CMAC** | AES-based Cipher-based Message Authentication Code -- signing algorithm for SMB 3.0+ |
| **AES-GCM** | AES in Galois/Counter Mode -- AEAD cipher preferred for SMB 3.1.1 |
| **AES-GMAC** | AES-GCM used for authentication only (no encryption) -- signing algorithm for SMB 3.1.1 |
| **AP-REQ** | Kerberos Application Request -- contains client's service ticket |
| **AP-REP** | Kerberos Application Reply -- provides mutual authentication |
| **CIFS** | Common Internet File System -- older name for SMB |
| **CreateGuid** | 16-byte GUID used for idempotent durable handle V2 reconnection |
| **Credit** | Flow control unit in SMB2 |
| **DH2Q/DH2C** | Durable Handle V2 Request/Reconnect create contexts |
| **DHnQ/DHnC** | Durable Handle V1 Request/Reconnect create contexts |
| **Dialect** | SMB protocol version (e.g., 0x0311 = SMB 3.1.1) |
| **Epoch** | Monotonic counter on lease V2 for stale break detection |
| **FileID** | 16-byte handle for open file (8 persistent + 8 volatile) |
| **GUID** | 16-byte globally unique identifier |
| **KDF** | Key Derivation Function -- derives session-specific keys from base key |
| **Lease V2** | Enhanced lease with ParentLeaseKey and epoch tracking (SMB 3.0+) |
| **NetBIOS** | Network Basic Input/Output System -- legacy session layer |
| **NT_STATUS** | Windows error code format |
| **Oplock** | Opportunistic lock -- client caching hint |
| **ParentLeaseKey** | Lease V2 field linking file lease to parent directory lease |
| **Preauth Integrity** | SHA-512 hash chain over negotiate/session-setup messages for downgrade protection |
| **SessionID** | 64-bit identifier for authenticated session |
| **Share** | Network-accessible folder (like NFS export) |
| **SID** | Security Identifier -- Windows user/group identity |
| **SP800-108** | NIST key derivation specification using Counter Mode with HMAC-SHA256 |
| **SPNEGO** | Simple and Protected GSSAPI Negotiation Mechanism -- wraps NTLM/Kerberos tokens |
| **Transform Header** | 52-byte header wrapping encrypted SMB3 messages (magic 0xFD) |
| **TreeID** | 32-bit identifier for share connection |
| **UTF-16LE** | 16-bit Unicode, little-endian byte order |

## References

### Specifications

- [MS-SMB2](https://docs.microsoft.com/en-us/openspecs/windows_protocols/ms-smb2/) - SMB2/3 Protocol Specification
- [MS-NLMP](https://docs.microsoft.com/en-us/openspecs/windows_protocols/ms-nlmp/) - NTLM Authentication Protocol
- [MS-FSCC](https://docs.microsoft.com/en-us/openspecs/windows_protocols/ms-fscc/) - File System Control Codes
- [MS-ERREF](https://docs.microsoft.com/en-us/openspecs/windows_protocols/ms-erref/) - Windows Error Codes
- [RFC 4178](https://tools.ietf.org/html/rfc4178) - SPNEGO Protocol
- [RFC 1813](https://tools.ietf.org/html/rfc1813) - NFS Version 3 Protocol Specification
- [RFC 7530](https://tools.ietf.org/html/rfc7530) - NFS Version 4.0 Protocol Specification
- [RFC 8881](https://tools.ietf.org/html/rfc8881) - NFS Version 4.1 Protocol Specification
- [NIST SP800-108](https://csrc.nist.gov/publications/detail/sp/800-108/final) - Key Derivation Using Pseudorandom Functions

### Related Projects

- [go-smb2](https://github.com/hirochachacha/go-smb2) - SMB2 client in Go
- [Samba](https://www.samba.org/) - SMB/CIFS implementation for Unix
