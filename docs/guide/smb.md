# SMB

DittoFS speaks SMB 2.0.2 through SMB 3.1.1 over TCP. This page covers everything an operator or
end-user needs: supported dialects, how to mount, authentication, encryption and signing
configuration, and lease/durable-handle behaviour as seen from clients.

> **Contributors:** protocol internals, wire formats, key derivation details, and cross-protocol
> coordination live in [../internals/smb-protocol.md](../internals/smb-protocol.md).

## Table of Contents

- [Supported Dialects](#supported-dialects)
- [Mounting SMB Shares](#mounting-smb-shares)
- [Encryption](#encryption)
- [Signing](#signing)
- [Leases and Durable Handles](#leases-and-durable-handles)
- [Authentication](#authentication)
- [User, Group and Permission Configuration](#user-group-and-permission-configuration)
- [Testing SMB Operations](#testing-smb-operations)
- [Troubleshooting](#troubleshooting)
- [Known Limitations](#known-limitations)
- [Glossary](#glossary)
- [References](#references)

---

## Supported Dialects

DittoFS negotiates the highest mutually-supported dialect with each client.

| Dialect | Hex    | Key Features |
|---------|--------|--------------|
| SMB 2.0.2 | 0x0202 | Basic file operations, credits, HMAC-SHA256 signing |
| SMB 3.0   | 0x0300 | AES-128-CCM encryption, AES-128-CMAC signing, secure dialect negotiation |
| SMB 3.0.2 | 0x0302 | VALIDATE_NEGOTIATE_INFO downgrade protection |
| SMB 3.1.1 | 0x0311 | Preauth integrity (SHA-512), AES-128-GCM encryption, GMAC signing, negotiate contexts |

SMB 3.1.1 is preferred; it provides the strongest security and best cipher performance on
AES-NI-capable hardware.

### Protocol Implementation Status

**Session and negotiation:**

| Command | Status | Notes |
|---------|--------|-------|
| NEGOTIATE | Implemented | Multi-dialect (2.0.2 through 3.1.1), negotiate contexts |
| SESSION_SETUP | Implemented | NTLM and Kerberos via SPNEGO, key derivation |
| LOGOFF | Implemented | |
| TREE_CONNECT | Implemented | Share-level permissions, per-share encryption |
| TREE_DISCONNECT | Implemented | |

**File operations:**

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
| IOCTL | Implemented | VALIDATE_NEGOTIATE_INFO, FSCTL_PIPE_WAIT, server-side copy (SRV_REQUEST_RESUME_KEY + SRV_COPYCHUNK) |
| LOCK | Implemented | Shared and exclusive byte-range locks |

**SMB3 advanced features:**

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

**Features not supported:**

| Feature | Notes |
|---------|-------|
| SMB1 | Legacy protocol, security risk |
| Compression | SMB 3.1.1 compression contexts not implemented |
| Multichannel | Multiple TCP connections per session |
| Persistent Handles | Cluster-aware handles (requires shared state) |
| RDMA | Remote Direct Memory Access transport |
| QUIC | UDP-based transport (SMB over QUIC) |
| SACL / auditing | Audit ACEs are not enforced (owner/group/DACL **are** supported — see [Access Control](access-control.md)) |
| DFS | Distributed File System referrals |

---

## Mounting SMB Shares

DittoFS listens on **port 12445** by default (port 445 requires root). All examples below use
that port.

### Using dfsctl (Recommended)

`dfsctl share mount` handles platform-specific mount options automatically:

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

### Platform-Specific Mount Behaviour

#### macOS Security Restriction

macOS restricts SMB mounts so that **only the mount owner can access files**, regardless of
Unix permissions. Even with 0777 permissions, non-owner users get "Permission denied". Apple
confirmed this is "works as intended".

**How dfsctl handles this**: When you run `sudo dfsctl share mount`, it automatically uses
`sudo -u $SUDO_USER` to mount as your user (not root):

```bash
# Works correctly - mount owned by your user
sudo dfsctl share mount --protocol smb /export /mnt/share
```

**Alternative - mount without sudo** (to a user-owned directory):

```bash
mkdir -p ~/mnt/dittofs
dfsctl share mount --protocol smb /export ~/mnt/dittofs
# No sudo needed; directory permissions default to 0755
```

#### Windows

From Command Prompt or PowerShell:

```cmd
net use Z: \\server\export /user:username password
# Explicit port:
net use Z: \\server@12445\export /user:username password

# Disconnect
net use Z: /delete
```

From Explorer: right-click "This PC" > "Map network drive", set path to
`\\server@12445\export`.

#### macOS (native commands)

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

---

## Encryption

### What Clients Observe

SMB3 sessions negotiate AES-GCM or AES-CCM encryption automatically. When encryption is
active, all messages between client and server are opaque on the wire; packet capture shows
only the transform header (52 bytes) and ciphertext.

Cipher suites supported:

| Cipher | Default For | Key Size |
|--------|-------------|----------|
| AES-128-CCM | SMB 3.0, 3.0.2 | 128-bit |
| AES-128-GCM | SMB 3.1.1 | 128-bit |
| AES-256-CCM | -- | 256-bit |
| AES-256-GCM | -- | 256-bit |

SMB confidentiality is provided by **SMB3 in-protocol encryption**, not TLS or QUIC. See
[./security.md](./security.md#smb3-security-model) for details.

### Encryption Modes

| Mode | Behaviour |
|------|-----------|
| `disabled` | No encryption for any session |
| `preferred` | Encrypt SMB 3.x sessions that support it; allow unencrypted 2.x (**default**) |
| `required` | Reject SMB 2.x clients; encrypt all SMB 3.x sessions |

The default (`preferred`) provides confidentiality for SMB 3.x clients without breaking SMB 2.x
compatibility. Set `required` to force encryption on the whole server, or set `encrypt_data`
on an individual share to enforce it only on sensitive data.

**Guest sessions are never encrypted** — guest sessions have no session key and cannot
participate in key derivation.

### Configuration

```yaml
adapters:
  smb:
    encryption:
      encryption_mode: preferred   # disabled | preferred | required
      allowed_ciphers: []          # Empty = all in default order
      # Custom cipher preference: [AES-128-GCM, AES-128-CCM]
```

Per-share encryption:

```yaml
shares:
  - name: sensitive
    encrypt_data: true   # Enforces encryption for this share regardless of server mode
```

See [./configuration.md](./configuration.md) for complete encryption configuration options.
See [./security.md](./security.md) for security implications and recommendations.

---

## Signing

### What Clients Observe

Signing adds an integrity tag to every SMB2 message so that tampering in transit is detected.
Clients may request signing in the NEGOTIATE request (`SIGNING_ENABLED`), or you can require
it server-wide.

Signing algorithms by dialect:

| Dialect | Algorithm |
|---------|-----------|
| SMB 2.0.2 | HMAC-SHA256 |
| SMB 3.0 | AES-128-CMAC |
| SMB 3.0.2 | AES-128-CMAC |
| SMB 3.1.1 | AES-128-GMAC (preferred) or AES-128-CMAC |

AES-128-GMAC is the preferred algorithm for 3.1.1 — it reuses GCM hardware acceleration. When
encryption is active, AEAD already provides integrity; DittoFS still signs to match Windows
Server behaviour.

### Configuration

```yaml
adapters:
  smb:
    signing:
      enabled: true      # Advertise signing capability
      required: false    # Require all clients to sign
      # Signing algorithm preference (for 3.1.1 negotiate context)
      # Default: [AES-128-GMAC, AES-128-CMAC]
      preferred_algorithms: []
```

---

## Leases and Durable Handles

### Leases: What Clients Observe

SMB 2.1+ clients receive **lease grants** that allow them to cache file data, writes, and open
handles locally without round-tripping to the server on every access.

Three caching flags compose a lease state:

| Flag | Abbreviation | What the Client May Do |
|------|-------------|------------------------|
| Read | R | Cache read data without revalidating against the server |
| Write | W | Cache writes and defer flushing to the server |
| Handle | H | Cache the file handle and defer CLOSE |

Common state combinations you'll see:

| State | Flags | Typical Use |
|-------|-------|-------------|
| None | -- | No caching |
| Read | R | Shared read caching (multiple clients) |
| Read-Handle | RH | Read caching with handle caching |
| Read-Write | RW | Exclusive read/write caching |
| Read-Write-Handle | RWH | Full exclusive caching (most aggressive) |

When a conflicting operation arrives (another client opens the file for write, or a cross-
protocol NFS open occurs), the server sends a **lease break notification**. The client must
flush cached data and acknowledge before the conflicting operation proceeds.

**Directory leases** (SMB 3.0+) allow clients to cache `QUERY_DIRECTORY` results. Any
modification to the directory's contents triggers an immediate break to None.

### Configuration

```yaml
adapters:
  smb:
    leases:
      enabled: true              # Enable lease support
      directory_leases: true     # Enable directory leasing
      lease_break_timeout: 35s   # Time to wait for break acknowledgment
```

### Durable Handles: What Clients Observe

Durable handles let clients **survive a transient network disconnection** without losing their
open files or cached state. The client disconnects, reconnects, and resumes — the server
preserves the handle and its state for up to the configured timeout.

Two versions:

**V1 (SMB 2.0.2+)**: Requires a batch oplock on the open file.

**V2 (SMB 3.0+)**: No oplock requirement; the client provides a `CreateGuid` (16-byte GUID)
that enables idempotent reconnection. Multiple reconnect attempts with the same GUID all
succeed.

After a successful reconnect, the client must re-request durability — the `IsDurable` flag is
not automatically set on the restored handle.

**Ephemeral state caveat**: Durable handles survive disconnection but not server restart. The
handle metadata is persisted in the configured store (BadgerDB/PostgreSQL), but in-memory state
is lost on restart.

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

---

## Authentication

### SPNEGO Negotiation

The server advertises both Kerberos and NTLM mechanism OIDs in the NEGOTIATE response.
Clients with valid Kerberos tickets choose Kerberos for single round-trip authentication;
other clients fall back to NTLM.

### Kerberos

DittoFS validates Kerberos tickets via SPNEGO during SESSION_SETUP. The SMB adapter shares the
Kerberos keytab with the NFS adapter; the server automatically derives the `cifs/` service
principal from the configured `nfs/` principal.

Principal-to-user mapping:
- A **local DittoFS account** matching the principal wins.
- When no local account exists but the **LDAP/AD directory** resolves the principal (RFC2307
  UID/GID + groups), the session is built from that directory identity — AD domain users do not
  need pre-created local accounts.
- A valid ticket whose principal resolves to neither a local account nor the directory is
  rejected (not treated as guest).

Kerberos is the recommended path for AD domain users: single round-trip, mutual authentication,
no machine-account provisioning required.

### NTLM

When Kerberos is unavailable (no keytab, client has no valid TGT, DNS resolution fails),
the server falls back to NTLM:

- **Local users**: the NTLMv2 response is validated against the NT hash stored in the control-
  plane user store.
- **AD domain users**: when a machine account is configured (`kerberos.machine_account`),
  DittoFS validates the domain user's NTLM response by forwarding it to the DC over a sealed
  NETLOGON secure channel (MS-NRPC `NetrLogonSamLogon`, sign+seal AES). The DC-returned SID
  is resolved through the same LDAP/idmap pipeline used for Kerberos, so the user maps to the
  same UID/GID across NFS-krb5, SMB-krb5, and SMB-NTLM.

**Machine account requirement**: NETLOGON passthrough requires an offline-provisioned machine
account (`DITTOFS$`) and its shared secret:

```bash
dfsctl identity-provider configure kerberos \
  --machine-account-enabled \
  --machine-account-name DITTOFS$ \
  --machine-secret <secret> \
  --dc-address <dc>
```

The secret is stored at-rest and is write-only / redacted in API responses.

> **Note**: end-to-end interoperability of the NETLOGON secure channel against a Samba AD-DC
> is still being validated — the sealed-schannel `AlterContext` is currently rejected by the
> test DC (`RPC_S_UNKNOWN_AUTHN_SERVICE`); see #1345.

### Guest Sessions

When authentication fails and guest access is enabled, a session is created with guest
privileges. Guest sessions cannot sign or encrypt messages (no session key), so they should be
limited to read-only public shares.

### Keytab Configuration and Hot-Reload

```yaml
kerberos:
  enabled: true
  keytab_path: /etc/dittofs/dittofs.keytab
  service_principal: nfs/server.example.com@EXAMPLE.COM
```

The keytab supports **hot-reload**: when the file is replaced on disk the server detects the
change and loads the new key without restart.

See [./security.md](./security.md) for detailed Kerberos security considerations.

---

## User, Group and Permission Configuration

SMB uses the same user/group store as all other DittoFS protocols. A brief example:

```yaml
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

Permission levels: `none`, `read`, `read-write`, `admin` (future).

Resolution order: user explicit permission → group permission → share default.

For the full user management reference (LDAP/AD idmap, password hash format, per-share
defaults), see [./configuration.md#user-management](./configuration.md#user-management).

---

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

---

## Troubleshooting

### Mount Fails with "Connection Refused"

1. Verify the server is running: `netstat -an | grep 12445`
2. Check firewall rules
3. Try explicit port: `port=12445` in mount options

### Authentication Fails

1. Verify the user exists in config
2. Check that the password hash is valid bcrypt
3. Enable debug logging to see the authentication flow
4. Ensure the user has share permissions
5. For Kerberos: verify the keytab contains the `cifs/` service principal and the KDC is
   reachable

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

See [Troubleshooting › Cross-Protocol Issues](troubleshooting.md#cross-protocol-issues) for cross-protocol troubleshooting, including:
- File locked by another protocol
- Delegation recall timeouts
- Lease break storms
- Stale data after cross-protocol writes

---

## Known Limitations

### Protocol Scope

1. **No SMB1 support**: Legacy protocol, not implemented for security reasons
2. **No compression**: SMB 3.1.1 compression contexts are not implemented
3. **No multichannel**: Multiple TCP connections per session not supported
4. **No persistent handles**: Cluster-aware handles require shared state infrastructure
5. **No RDMA transport**: Remote Direct Memory Access not supported
6. **No QUIC transport**: SMB over QUIC (UDP) not supported
7. **No SACL / audit ACEs**: Owner, group, and DACL security descriptors **are** supported (see [Access Control](access-control.md)); only audit ACEs (SACL) are not enforced
8. **No DFS referrals**: Distributed File System not supported

### Operational Limitations

9. **Ephemeral locks and oplocks**: Both byte-range locks and oplocks are in-memory only, lost
   on server restart
10. **No blocking locks**: Lock requests fail immediately if a conflicting lock exists
11. **Single-node only**: No clustering or high availability for SMB state
12. **Durable handle state is in-memory**: Durable handles survive disconnection but not server
    restart (BadgerDB/PostgreSQL stores persist handle metadata but in-memory state is lost)

### SMB3 Feature Gaps

13. **No per-file encryption**: Encryption is per-session or per-share only

---

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
| **ACE** | Access Control Entry -- a single allow/deny rule inside an ACL |
| **CIFS** | Common Internet File System -- older name for SMB |
| **CreateGuid** | 16-byte GUID used for idempotent durable handle V2 reconnection |
| **Credit** | Flow control unit in SMB2 |
| **DACL** | Discretionary ACL -- the access-granting part of a security descriptor |
| **DH2Q/DH2C** | Durable Handle V2 Request/Reconnect create contexts |
| **DHnQ/DHnC** | Durable Handle V1 Request/Reconnect create contexts |
| **Durable / persistent handle** | An open handle a client can reclaim after a network drop (durable) or server restart (persistent) |
| **Dialect** | SMB protocol version (e.g., 0x0311 = SMB 3.1.1) |
| **Epoch** | Monotonic counter on lease V2 for stale break detection |
| **FileID** | 16-byte handle for open file (8 persistent + 8 volatile) |
| **GUID** | 16-byte globally unique identifier |
| **KDF** | Key Derivation Function -- derives session-specific keys from base key |
| **Lease V2** | Enhanced lease with ParentLeaseKey and epoch tracking (SMB 3.0+) |
| **Kerberos** | Ticket-based network authentication, carried over SMB via SPNEGO |
| **Lease** | SMB 2.1+ caching grant (read/write/handle) succeeding oplocks |
| **NetBIOS** | Network Basic Input/Output System -- legacy session layer |
| **NT_STATUS** | Windows error code format |
| **NTLM / NTLMSSP** | Microsoft challenge/response authentication; the fallback when Kerberos is unavailable |
| **Oplock** | Opportunistic lock -- client caching hint |
| **ParentLeaseKey** | Lease V2 field linking file lease to parent directory lease |
| **Preauth Integrity** | SHA-512 hash chain over negotiate/session-setup messages for downgrade protection |
| **SACL** | System ACL -- the auditing part of a security descriptor |
| **Security descriptor** | Windows/SMB structure bundling owner, group, DACL, and SACL ([MS-DTYP §2.4.6](https://learn.microsoft.com/en-us/openspecs/windows_protocols/ms-dtyp/)) |
| **SessionID** | 64-bit identifier for authenticated session |
| **Share** | Network-accessible folder (like NFS export) |
| **SID** | Security Identifier -- Windows user/group identity |
| **SP800-108** | NIST key derivation specification using Counter Mode with HMAC-SHA256 |
| **SPNEGO** | Simple and Protected GSSAPI Negotiation Mechanism -- wraps NTLM/Kerberos tokens |
| **Transform Header** | 52-byte header wrapping encrypted SMB3 messages (magic 0xFD) |
| **TreeID** | 32-bit identifier for share connection |
| **UTF-16LE** | 16-bit Unicode, little-endian byte order |

---

## References

### Specifications

- [MS-SMB2](https://learn.microsoft.com/en-us/openspecs/windows_protocols/ms-smb2/) - SMB2/3 Protocol Specification
- [MS-NLMP](https://learn.microsoft.com/en-us/openspecs/windows_protocols/ms-nlmp/) - NTLM Authentication Protocol
- [MS-DTYP](https://learn.microsoft.com/en-us/openspecs/windows_protocols/ms-dtyp/) - SID, ACL, ACE, and security descriptor formats
- [MS-FSCC](https://learn.microsoft.com/en-us/openspecs/windows_protocols/ms-fscc/) - File System Control Codes
- [MS-ERREF](https://learn.microsoft.com/en-us/openspecs/windows_protocols/ms-erref/) - Windows Error Codes
- [RFC 4178](https://www.rfc-editor.org/rfc/rfc4178) - SPNEGO Protocol
- [RFC 4120](https://www.rfc-editor.org/rfc/rfc4120) - Kerberos V5
- [RFC 2743](https://www.rfc-editor.org/rfc/rfc2743) - GSS-API
- [NIST SP800-108](https://csrc.nist.gov/publications/detail/sp/800-108/final) - Key Derivation Using Pseudorandom Functions

For plain-language definitions of these terms, see the project-wide [Glossary](glossary.md).

### Related Projects

- [go-smb2](https://github.com/hirochachacha/go-smb2) - SMB2 client in Go
- [Samba](https://www.samba.org/) - SMB/CIFS implementation for Unix
