# Feature Landscape: SMB3 Protocol Upgrade (v3.8)

**Domain:** SMB 3.0/3.0.2/3.1.1 protocol features -- encryption, signing, leases, Kerberos, durable handles, cross-protocol integration
**Researched:** 2026-02-28
**Confidence:** HIGH for protocol mechanics (MS-SMB2 spec, Microsoft blogs with test vectors), MEDIUM for implementation complexity estimates (based on codebase analysis + Samba reference)

## Table Stakes

Features that Windows 10/11 clients expect from an SMB3 server. Missing any of these means clients either fail to connect, fall back to SMB2.1, or lose security guarantees.

### 1. SMB3 Dialect Negotiation with Negotiate Contexts

| Feature | Why Expected | Complexity | Dependencies | Notes |
|---------|--------------|------------|--------------|-------|
| SMB 3.0 dialect selection (0x0300) | Windows 8+ clients offer 3.0; selecting it enables encryption, multichannel, directory leasing capabilities | Medium | Existing `negotiate.go` handler, `types/constants.go` dialect definitions | Currently only selects 2.0.2/2.1. Must extend dialect selection to pick highest mutually supported dialect up to 3.1.1. The negotiate handler already parses dialect list and has constant definitions for 3.0/3.0.2/3.1.1. |
| SMB 3.0.2 dialect selection (0x0302) | Windows 8.1+ clients offer 3.0.2; adds signing/encryption improvements | Low (incremental) | Same as above | Same mechanism as 3.0 with minor capability differences. |
| SMB 3.1.1 dialect selection (0x0311) | Windows 10+ clients offer 3.1.1; adds preauth integrity and cipher negotiation. This is the primary target dialect. | Medium | Negotiate context parsing infrastructure | Requires negotiate context support (see below). Windows 10/11 sends 3.1.1 as highest dialect alongside 2.0.2, 2.1, 3.0, 3.0.2. |
| Negotiate context parsing infrastructure | SMB 3.1.1 NEGOTIATE includes a list of negotiate contexts after the dialect list, 8-byte aligned | Medium | `negotiate.go` body parser | Parse `NegotiateContextOffset` (uint32 at body[28:32]), `NegotiateContextCount` (uint16 at body[32:34]). Each context has ContextType(2) + DataLength(2) + Reserved(4) + Data(variable) with 8-byte alignment padding between contexts. |
| SMB2_PREAUTH_INTEGRITY_CAPABILITIES context (ContextType 0x0001) | Mandatory for 3.1.1. Client sends SHA-512 hash algorithm ID + random salt. Server must respond with selected hash algorithm + its own salt. | Medium | Negotiate handler, new preauth state on Connection | Parse HashAlgorithmCount + SaltLength + HashAlgorithms array + Salt. Only defined algorithm is SHA-512 (0x0001). Server responds with same structure selecting SHA-512 and providing its own 32-byte random salt. Must initialize Connection.PreauthIntegrityHashValue = SHA-512(zero_64_bytes + negotiate_request_bytes). |
| SMB2_ENCRYPTION_CAPABILITIES context (ContextType 0x0002) | Required when client and server both support encryption in 3.1.1. Client sends list of cipher IDs ordered by preference. | Medium | Negotiate handler, Connection cipher state | Parse CipherCount + Ciphers array. Cipher IDs: AES-128-CCM (0x0001), AES-128-GCM (0x0002), AES-256-CCM (0x0003), AES-256-GCM (0x0004). Server selects first mutually supported cipher. Connection.CipherId stores result. |
| Negotiate context response encoding | Server NEGOTIATE response must include negotiate contexts when 3.1.1 is selected | Medium | `negotiate.go` response builder | Set NegotiateContextOffset and NegotiateContextCount in response. Encode preauth integrity + encryption contexts after the security buffer, 8-byte aligned. |
| Updated capabilities for SMB3 | SMB 3.0+ responses must advertise additional capabilities: CapDirectoryLeasing, CapEncryption, CapPersistentHandles | Low | `negotiate.go` capabilities bitmask | Currently advertises CapLeasing + CapLargeMTU for 2.1+. Must add CapDirectoryLeasing (0x20) and CapEncryption (0x40) for 3.0+. |
| Wildcard dialect handling update | When client sends 0x02FF alongside 3.x dialects, server should prefer 3.x dialect over echoing wildcard | Low | `negotiate.go` dialect selection logic | Current code echoes wildcard when only 2.0.2 is matched alongside wildcard. Must update to select highest 3.x dialect if present. |

### 2. Preauth Integrity (SHA-512 Hash Chain)

| Feature | Why Expected | Complexity | Dependencies | Notes |
|---------|--------------|------------|--------------|-------|
| Connection preauth integrity hash chain | Mandatory for 3.1.1. Protects NEGOTIATE + SESSION_SETUP from MITM tampering by chaining SHA-512 hashes. | High | Connection state, negotiate handler, session setup handler | After NEGOTIATE request: hash = SHA-512(zeros_64 + negotiate_request_bytes). After NEGOTIATE response: hash = SHA-512(previous_hash + negotiate_response_bytes). This hash is then used per-session for SESSION_SETUP messages. |
| Session preauth integrity hash chain | Each session setup round-trip updates the session's preauth hash, which becomes the KDF context for 3.1.1 key derivation. | High | Session state, session setup handler | On first SESSION_SETUP request: Session.PreauthHash = SHA-512(Connection.PreauthHash + session_setup_request). On STATUS_MORE_PROCESSING_REQUIRED response: update again. On final SESSION_SETUP: use Session.PreauthHash as Context parameter in key derivation. |
| Hash chain must include full SMB2 header + body | The hash computation covers the entire on-wire message (header + body), NOT just the body. Signature field is included as-is (zeros for unsigned messages). | Medium | Dispatch layer must capture raw message bytes before parsing | Must save the raw bytes of NEGOTIATE request/response and SESSION_SETUP request/response before any parsing. The signature field is NOT zeroed for preauth hashing (unlike signing). |

### 3. AES Encryption (Per-Session and Per-Share)

| Feature | Why Expected | Complexity | Dependencies | Notes |
|---------|--------------|------------|--------------|-------|
| SMB3 Transform Header (encryption envelope) | Encrypted messages are wrapped in a Transform Header (52 bytes for 3.0, 52 for 3.1.1) containing nonce, original message size, session ID, and signature/AES-tag. | High | New framing layer between TCP read and dispatch | ProtocolId = 0x424D53FD (vs 0x424D53FE for normal SMB2). Parse: Signature(16) + Nonce(16) + OriginalMessageSize(4) + Reserved(2) + Flags(2) + SessionId(8). Flags: 0x0001 = Encrypted (3.0 uses EncryptionSessionId instead of Flags+SessionId). |
| AES-128-CCM encryption/decryption | Required for 3.0/3.0.2 compatibility. Counter with CBC-MAC mode. | High | Go `crypto/aes` + `crypto/cipher` AEAD, key derivation | CCM nonce is 11 bytes (first 11 of Nonce field). Associated data = transform header bytes 20-51 (OrigMsgSize through SessionId). Plaintext = original SMB2 message. Go stdlib provides `cipher.NewCCM` or use `golang.org/x/crypto/ccm`. |
| AES-128-GCM encryption/decryption | Primary cipher for 3.1.1. Galois/Counter Mode, significantly faster than CCM. | Medium | Go `crypto/aes` + `crypto/cipher` AEAD, key derivation | GCM nonce is 12 bytes (first 12 of Nonce field). Associated data same as CCM. Go stdlib `cipher.NewGCM` is well-supported and hardware-accelerated on amd64/arm64. |
| AES-256-GCM and AES-256-CCM support | Windows Server 2022 / Windows 11 can negotiate 256-bit ciphers. | Medium (incremental) | Key derivation with L=256, cipher initialization with 256-bit keys | Same AEAD interface, just longer key. KDF 'L' parameter changes from 128 to 256 when Connection.CipherId is AES-256-*. |
| Per-session encryption (Session.EncryptData) | Server can mandate encryption for all traffic on a session via SMB2_SESSION_FLAG_ENCRYPT_DATA in SESSION_SETUP response. | Medium | Session state, session setup handler, config | Set flag in SESSION_SETUP response when server config requires encryption. All subsequent messages to/from this session must be encrypted. |
| Per-share encryption (Share.EncryptData) | Individual shares can require encryption via SMB2_SHAREFLAG_ENCRYPT_DATA in TREE_CONNECT response. | Medium | Share config, tree connect handler | Set flag in TREE_CONNECT response for shares configured to require encryption. Only traffic to this tree ID must be encrypted. Unencrypted requests to encrypted shares return STATUS_ACCESS_DENIED. |
| RejectUnencryptedAccess configuration | Server-wide setting controlling whether to reject unencrypted connections from downlevel clients that cannot do encryption. | Low | Config, dispatch layer | When true, non-encrypted requests to encrypted sessions/shares get STATUS_ACCESS_DENIED. When false, allows unencrypted access for backward compatibility. |

### 4. SMB3 Key Derivation (SP800-108 KDF)

| Feature | Why Expected | Complexity | Dependencies | Notes |
|---------|--------------|------------|--------------|-------|
| SP800-108 Counter Mode KDF implementation | All SMB3 keys are derived using KDF in Counter Mode per NIST SP800-108. PRF = HMAC-SHA256, r=32, L=128 (or 256 for AES-256). | Medium | `crypto/hmac`, `crypto/sha256` | Ko = PRF(Ki, [i]_2 \|\| Label \|\| 0x00 \|\| Context \|\| [L]_4) where i=1 for 128-bit keys. Single HMAC-SHA256 call produces 256 bits; take leftmost L bits. |
| SMB 3.0/3.0.2 key derivation labels | SigningKey = KDF(SessionKey, "SMB2AESCMAC\0", "SmbSign\0"). EncryptionKey = KDF(SessionKey, "SMB2AESCCM\0", "ServerIn \0"). DecryptionKey = KDF(SessionKey, "SMB2AESCCM\0", "ServerOut\0"). ApplicationKey = KDF(SessionKey, "SMB2APP\0", "SmbRpc\0"). | Medium | KDF function, session key from GSS-API/NTLM | Labels and contexts are null-terminated ASCII strings. Note the trailing space in "ServerIn " and "ServerOut" is significant. Server's encryption key is the client's decryption key and vice versa. |
| SMB 3.1.1 key derivation labels | SigningKey = KDF(SessionKey, "SMBSigningKey\0", PreauthHash). EncryptionKey = KDF(SessionKey, "SMBC2SCipherKey\0", PreauthHash). DecryptionKey = KDF(SessionKey, "SMBS2CCipherKey\0", PreauthHash). ApplicationKey = KDF(SessionKey, "SMBAppKey\0", PreauthHash). | Medium | KDF function, preauth integrity hash | Context is Session.PreauthIntegrityHashValue (64 bytes) instead of a constant string. This binds keys to the specific negotiate/session-setup exchange, preventing MITM substitution. |
| Session key derivation from Kerberos | Kerberos session key is the sub-session key (if negotiated in AP-REQ/AP-REP) or the ticket session key. First 16 bytes, right-padded with zeros if shorter. | Low | Existing Kerberos provider, `gokrb5` library | Already implemented for NTLM in `session_setup.go`. For Kerberos, the session key must be extracted from the authenticated context after AP-REQ validation. The current `handleKerberosAuth` does NOT extract a session key -- it creates a session without signing. This is a gap. |
| Session key derivation from NTLM | NTLMv2 ExportedSessionKey (already implemented). Used as input Ki to KDF. | Low | Already implemented in `session_setup.go` | Current code derives signing key directly from session key for SMB2. For SMB3, must route through KDF to produce separate signing/encryption/decryption keys. |

### 5. AES Signing (CMAC for 3.0+, GMAC for 3.1.1)

| Feature | Why Expected | Complexity | Dependencies | Notes |
|---------|--------------|------------|--------------|-------|
| AES-128-CMAC signing | Required for SMB 3.0/3.0.2/3.1.1. Replaces HMAC-SHA256 used by SMB 2.0.2/2.1. | Medium | `crypto/aes`, AES-CMAC implementation (not in Go stdlib) | AES-CMAC (RFC 4493) produces 16-byte MAC. Must implement or import. Go stdlib does not include CMAC; use `github.com/aead/cmac` or implement per RFC 4493 (subkey derivation + CBC-MAC). Input: entire message with zeroed signature field. Output: 16-byte signature. |
| AES-128-GMAC signing (3.1.1 optional) | Windows 11/Server 2022 can negotiate AES-128-GMAC for faster signing in 3.1.1. Negotiated via SMB2_SIGNING_CAPABILITIES negotiate context. | Medium | `crypto/aes` + `crypto/cipher` GCM, negotiate context | GMAC is GCM with empty plaintext (MAC-only mode). Nonce constructed from MessageId (8 bytes) + padding. Go's `cipher.NewGCM` supports Seal with empty plaintext to produce GMAC tag. Requires SMB2_SIGNING_CAPABILITIES negotiate context (ContextType 0x0008). |
| SMB2_SIGNING_CAPABILITIES negotiate context | Client sends list of supported signing algorithms. Server selects one. | Low | Negotiate context infrastructure | Parse SigningAlgorithmCount + SigningAlgorithms array. Algorithm IDs: HMAC-SHA256 (0x0000), AES-CMAC (0x0001), AES-GMAC (0x0002). Server selects best mutually supported algorithm. |
| Signing algorithm dispatch by dialect | SMB2 uses HMAC-SHA256, SMB3 uses AES-CMAC (or GMAC). Must dispatch to correct algorithm based on negotiated dialect/signing capability. | Medium | Signing module refactor | Current `signing/signing.go` is HMAC-SHA256 only. Must abstract into a SigningAlgorithm interface with HMAC-SHA256, AES-CMAC, and AES-GMAC implementations. Session stores which algorithm to use. |
| Final SESSION_SETUP response signing | For SMB 3.x, the final SESSION_SETUP response (STATUS_SUCCESS) MUST be signed using the newly derived signing key. This is how the client validates the server knows the session key. | Medium | Session setup handler, signing module | Current code signs with the session's signing key. Must ensure for 3.x that the KDF-derived SigningKey is used (not the raw session key) and that AES-CMAC is used instead of HMAC-SHA256. |

### 6. SPNEGO/Kerberos Session Setup (Shared Layer)

| Feature | Why Expected | Complexity | Dependencies | Notes |
|---------|--------------|------------|--------------|-------|
| Kerberos session key extraction for SMB3 key derivation | Current `handleKerberosAuth` creates session without deriving a session key. For SMB3, the Kerberos session key must feed into KDF. | High | `gokrb5` credential extraction, session setup handler | After `service.VerifyAPREQ`, extract the sub-session key or ticket session key from `creds`. This becomes the GSS-API SessionKey. First 16 bytes (right-padded) are used as Ki for KDF. Without this, signing and encryption are impossible for Kerberos-authenticated SMB3 sessions. |
| SPNEGO NegTokenResp with AP-REP | Full Kerberos mutual auth returns AP-REP in the SPNEGO accept-complete. Current code sends nil responseToken. | Medium | `gokrb5` AP-REP generation, SPNEGO builder | Some clients (notably Linux CIFS) validate the AP-REP to confirm mutual authentication. Windows accepts nil responseToken but proper AP-REP is more correct. Use `service.NewAPREP` from gokrb5. |
| NTLM fallback when Kerberos fails | If Kerberos token validation fails, fall back to NTLM challenge/response instead of returning STATUS_LOGON_FAILURE. | Low | Session setup handler | Already partially implemented. SPNEGO allows mechanism negotiation -- if Kerberos fails but NTLM is also offered, server should initiate NTLM challenge. Current code routes directly to NTLM if no Kerberos token is present, but does not handle Kerberos-to-NTLM fallback within SPNEGO. |
| Guest access with proper 3.x handling | Guest sessions in SMB3 must NOT have encryption or signing (no session key). Server must not set SMB2_SESSION_FLAG_ENCRYPT_DATA for guest. | Low | Session setup response builder | Already handled correctly for SMB2. Must ensure 3.x code path does not attempt encryption/signing for guest sessions. Guest flag (0x0001) in SessionFlags signals this to the client. |

### 7. Secure Dialect Validation (3.0/3.0.2 Only)

| Feature | Why Expected | Complexity | Dependencies | Notes |
|---------|--------------|------------|--------------|-------|
| FSCTL_VALIDATE_NEGOTIATE_INFO IOCTL handler | After TREE_CONNECT, SMB 3.0/3.0.2 clients send a signed IOCTL to validate that the NEGOTIATE was not tampered with. Server must respond with a signed response echoing its capabilities, GUID, security mode, and dialect. | Medium | IOCTL handler, FSCTL dispatch | Request contains: Capabilities(4) + Guid(16) + SecurityMode(2) + Dialect(2). Server validates these match the original negotiate, then responds with its own Capabilities(4) + Guid(16) + SecurityMode(2) + Dialect(2). If mismatch, return STATUS_ACCESS_DENIED and disconnect. Response MUST be signed regardless of signing configuration. |
| Superseded by preauth integrity in 3.1.1 | SMB 3.1.1 preauth integrity hash chain replaces VALIDATE_NEGOTIATE_INFO. The IOCTL is not sent by 3.1.1 clients. | N/A | N/A | Server should still handle it for 3.0/3.0.2 clients. For 3.1.1, the preauth hash chain provides stronger protection integrated into the key derivation itself. |

### 8. SMB3 Leases (Upgrade from SMB2 Oplocks)

| Feature | Why Expected | Complexity | Dependencies | Notes |
|---------|--------------|------------|--------------|-------|
| Lease V2 with ParentLeaseKey | SMB 3.0+ uses SMB2_CREATE_REQUEST_LEASE_V2 (52 bytes) which adds ParentLeaseKey (16 bytes) and Epoch (2 bytes) for directory lease awareness. | Medium | Existing lease infrastructure in `lease.go`, create handler | Already partially implemented: `DecodeLeaseCreateContext` handles V2 format and parses ParentLeaseKey/Epoch. `EncodeLeaseResponseContext` encodes V2 response. Main gap is ParentLeaseKey validation and directory lease grant integration with Unified Lock Manager. |
| Directory leases (Read-caching on directories) | SMB 3.0+ supports leases on directories (Read-only state). Client can cache directory listings until lease break. Advertised via CapDirectoryLeasing capability. | Medium | `lease.go` RequestLease, Unified Lock Manager, metadata directory change tracking | Already have `isDirectory` parameter in `RequestLease` and `IsValidDirectoryLeaseState` check. Main gap: triggering lease breaks when directory contents change (file create/delete/rename within directory). Must integrate with CHANGE_NOTIFY infrastructure. |
| Lease break epoch tracking | Epoch counter in lease create response and break notification ensures client and server agree on lease state version. Prevents stale break acknowledgments. | Low | Already implemented | `LeaseCreateContext.Epoch`, `LeaseBreakNotification.NewEpoch`, and `lease.OpLock.Epoch` are already tracked and incremented on breaks. |
| Cross-protocol lease<->delegation mapping | SMB3 leases must coordinate with NFS delegations. SMB Read lease = NFS Read delegation. SMB Write lease = NFS Write delegation. | Medium | Existing cross-protocol infrastructure in `oplock.go` and `cross_protocol.go` | Already have `CheckAndBreakForWrite`, `CheckAndBreakForRead`, `CheckAndBreakForDelete`. These operate on the Unified Lock Manager and work for any protocol. Main gap: NFS delegation grants should check/break SMB leases and vice versa. This is partially implemented via `SetAdapterProvider(OplockBreakerProviderKey, ...)`. |

### 9. Durable Handles V1 and V2

| Feature | Why Expected | Complexity | Dependencies | Notes |
|---------|--------------|------------|--------------|-------|
| Durable Handle V1 (DHnQ/DHnC) create contexts | SMB 2.1+ clients request durable handles via SMB2_CREATE_DURABLE_HANDLE_REQUEST in CREATE. Allows reconnecting to open files after brief network interruption. | High | Create handler, new durable handle state, connection tracking | DHnQ = request context (0 bytes data). Server grants by returning SMB2_CREATE_DURABLE_HANDLE_RESPONSE. Must persist open state (FileId, session info, lease state) to survive disconnect. Reconnect via DHnC (Durable Handle Reconnect) with the original FileId. Timeout: 60 seconds (configurable). |
| Durable Handle V2 (DH2Q/DH2C) create contexts | SMB 3.0+ uses V2 with CreateGuid for idempotent reconnection and optional persistent flag for CA shares. | High | Create handler, GUID-based handle registry, persistent state | DH2Q contains: Timeout(4) + DurableFlags(4) + Reserved(8) + CreateGuid(16). Server returns DH2Q response: Timeout(4) + DurableFlags(4). DH2C reconnect contains: FileId(16) + CreateGuid(16) + Flags(4). CreateGuid provides idempotent create (replay detection). SMB2_DHANDLE_FLAG_PERSISTENT (0x02) requests persistent handle on CA shares. |
| Durable handle state persistence | Open state must survive server disconnection (V1: ~60s, V2: configurable timeout, persistent: indefinite for CA). | High | Metadata or separate handle store, WAL for crash recovery | Must store: FileId, FileName, CreateOptions, DesiredAccess, ShareAccess, CreateDisposition, LeaseKey, CreateGuid, SessionId. On reconnect, validate CreateGuid match and restore the open. This is the hardest part of durable handles. |
| Lease required for durable handles | Per MS-SMB2, durable handles require a lease (V1 requires at least a Handle lease, V2 strongly recommended). Without a lease, the client cannot detect server-side changes during disconnect. | Low | Lease integration | Validate that a lease context is also present when durable handle is requested. If no lease, server MAY reject the durable handle request. |

## Differentiators

Features that go beyond basic compliance. Not required for Windows 10/11 connectivity but improve resilience, performance, or enterprise readiness.

| Feature | Value Proposition | Complexity | Dependencies | Notes |
|---------|-------------------|------------|--------------|-------|
| Compression capabilities (LZ77, LZNT1, LZ77+Huffman, Pattern_V1) | SMB 3.1.1 supports per-message compression. Reduces bandwidth for large transfers. | High | SMB2_COMPRESSION_CAPABILITIES negotiate context, transform header variant | Negotiate via ContextType 0x0003. Compression algorithms: LZNT1 (0x0001), LZ77 (0x0002), LZ77+Huffman (0x0003), Pattern_V1 (0x0004). Defer to post-3.8: compression is optional and complex to implement correctly. |
| Multichannel (multiple TCP connections per session) | Aggregates bandwidth across NICs. Enterprise feature for high-throughput scenarios. | Very High | Session binding, channel signing keys, FSCTL_QUERY_NETWORK_INTERFACE_INFO | Requires SMB2_SESSION_FLAG_BINDING, per-channel SigningKey derivation, interface discovery IOCTL. Defer: not needed for single-server deployment target. |
| Persistent handles (CA shares) | Continuously Available shares where opens survive server failover. | Very High | Clustered storage, replicated handle state | Requires SMB2_DHANDLE_FLAG_PERSISTENT + share flagged as CA. Defer: DittoFS is single-node. |
| Server-side copy (FSCTL_SRV_COPYCHUNK) | Efficient file copy without client round-trips. | Medium | IOCTL handler, metadata/payload coordination | Useful optimization. Can leverage content-addressed storage for zero-copy. Defer to v4.0 alongside NFSv4.2 COPY. |
| Replay detection (SMB2_FLAGS_REPLAY_OPERATION) | 3.x flag for idempotent operation replay after transient failures. | Medium | DH2 CreateGuid tracking, operation dedup | Needed for full DH2 support. Include if durable handles V2 are implemented. |
| RDMA transport (SMB Direct) | Zero-copy networking for Hyper-V / SQL workloads. | Very High | Kernel RDMA support, SMB2_RDMA_TRANSFORM_CAPABILITIES | Far out of scope for userspace Go implementation. |
| QUIC transport (SMB over QUIC) | VPN-less remote file access. Windows Server 2022+. | Very High | QUIC library, TLS 1.3, certificate management | Interesting future feature but not needed for v3.8. |

## Anti-Features

Features to explicitly NOT build in v3.8.

| Anti-Feature | Why Avoid | What to Do Instead |
|--------------|-----------|-------------------|
| Compression | Optional, complex, low ROI for initial SMB3 release | Return empty intersection in SMB2_COMPRESSION_CAPABILITIES negotiate context (no common algorithms) |
| Multichannel | Single-node architecture; requires session binding, interface discovery | Return single interface in FSCTL_QUERY_NETWORK_INTERFACE_INFO; do not set CapMultiChannel in 3.0+ negotiate responses |
| Persistent handles | Requires clustered storage (CA shares) which DittoFS does not support | Never set SMB2_SHAREFLAG_CLUSTER or SMB2_SHARE_CAP_CONTINUOUS_AVAILABILITY on shares |
| RDMA Direct | Hardware-dependent, kernel-level, irrelevant for userspace Go | Ignore SMB2_RDMA_TRANSFORM_CAPABILITIES negotiate context |
| QUIC transport | Major new transport layer, separate from protocol upgrade | Not in scope; defer to future milestone |
| SMB1 negotiate compatibility | SMB1 is deprecated and DittoFS never supported it | Continue returning error for SMB1 protocol ID (0x424D53FF) |
| 8.3 short name generation | Low priority, only affects legacy apps | Continue returning zeros in ShortName fields |
| Full SACL enforcement | Requires audit infrastructure | Continue returning empty SACL stub |
| Extended Attributes over SMB | Requires xattr metadata layer from v4.0 | Return EaSize=0; defer to v4.0 NFSv4.2 xattrs |

## Feature Dependencies

```
SMB3 Dialect Negotiation
  |
  +-- Negotiate Context Infrastructure (parsing + encoding)
  |     |
  |     +-- SMB2_PREAUTH_INTEGRITY_CAPABILITIES
  |     |     |
  |     |     +-- Connection Preauth Hash Chain (SHA-512)
  |     |     |     |
  |     |     |     +-- Session Preauth Hash Chain
  |     |     |           |
  |     |     |           +-- SMB 3.1.1 Key Derivation (context = PreauthHash)
  |     |     |
  |     |     +-- Raw Message Capture (dispatch layer)
  |     |
  |     +-- SMB2_ENCRYPTION_CAPABILITIES
  |     |     |
  |     |     +-- AES-128-CCM / AES-128-GCM / AES-256-* implementation
  |     |           |
  |     |           +-- Transform Header (encrypt/decrypt framing)
  |     |                 |
  |     |                 +-- Per-Session Encryption (Session.EncryptData)
  |     |                 +-- Per-Share Encryption (Share.EncryptData)
  |     |
  |     +-- SMB2_SIGNING_CAPABILITIES (optional, for GMAC)
  |
  +-- SP800-108 KDF Implementation
  |     |
  |     +-- SMB 3.0/3.0.2 Key Derivation (constant labels/contexts)
  |     +-- SMB 3.1.1 Key Derivation (PreauthHash context)
  |     |
  |     +-- Signing Key
  |     +-- Encryption Key
  |     +-- Decryption Key
  |     +-- Application Key
  |
  +-- AES-CMAC Signing (replaces HMAC-SHA256 for 3.x)
  |     |
  |     +-- Signing Algorithm Abstraction (dispatch by dialect)
  |     +-- AES-GMAC Signing (optional 3.1.1)
  |
  +-- Kerberos Session Key Extraction (gap in current code)
  |     |
  |     +-- SPNEGO AP-REP in accept-complete
  |     +-- NTLM Fallback within SPNEGO
  |
  +-- Secure Dialect Validation (3.0/3.0.2 IOCTL)
  |
  +-- SMB3 Leases (V2 with ParentLeaseKey)
  |     |
  |     +-- Directory Lease Breaks (on content change)
  |     +-- Cross-Protocol Lease<->Delegation coordination
  |
  +-- Durable Handles V1 (DHnQ/DHnC)
        |
        +-- Durable Handle State Persistence
        +-- Durable Handles V2 (DH2Q/DH2C with CreateGuid)
              |
              +-- Replay Detection (FLAGS_REPLAY_OPERATION)
```

## Existing Infrastructure to Leverage

The existing SMB2 implementation provides substantial foundation for SMB3:

| Existing Component | Location | Reuse for SMB3 |
|-------------------|----------|----------------|
| Dialect definitions (3.0, 3.0.2, 3.1.1 already defined) | `types/constants.go` | Direct reuse -- constants already exist |
| Capability constants (CapEncryption, CapDirectoryLeasing, etc.) | `types/constants.go` | Direct reuse -- already defined |
| SessionFlags (EncryptData) | `types/constants.go` | Direct reuse |
| HeaderFlags (FlagReplay) | `types/constants.go` | Direct reuse |
| Lease V2 decode/encode | `v2/handlers/lease.go` | Direct reuse -- already handles V1 and V2 format |
| Lease management (OplockManager with lock store) | `v2/handlers/lease.go`, `v2/handlers/oplock.go` | Direct reuse -- full lease lifecycle already implemented |
| Cross-protocol oplock breaks | `v2/handlers/cross_protocol.go`, `v2/handlers/oplock.go` | Direct reuse -- CheckAndBreakForWrite/Read/Delete |
| SPNEGO parsing (NTLM + Kerberos detection) | `auth/spnego.go` | Direct reuse |
| Kerberos auth via shared provider | `v2/handlers/session_setup.go` | Extend -- add session key extraction |
| NTLM auth with NTLMv2 validation | `v2/handlers/session_setup.go` | Extend -- route session key through KDF for 3.x |
| Signing infrastructure (SigningKey, SignMessage, Verify) | `signing/signing.go` | Refactor -- abstract algorithm, add CMAC/GMAC |
| Session management (credits, signing state) | `session/session.go`, `session/manager.go` | Extend -- add encryption keys, preauth hash |
| Connection types | `conn_types.go` | Extend -- add negotiated dialect, cipher, preauth hash |
| Create context parsing | `v2/handlers/context.go`, `v2/handlers/lease_context.go` | Extend -- add durable handle contexts |
| IOCTL stub handler | `v2/handlers/stub_handlers.go` | Extend -- add VALIDATE_NEGOTIATE_INFO |

## MVP Recommendation

### Phase 1: Foundation (Negotiate + Key Derivation + Signing)
Prioritize because everything else depends on correct dialect negotiation and key derivation.

1. **SP800-108 KDF implementation** -- Pure function, independently testable
2. **AES-CMAC signing implementation** -- Replace HMAC-SHA256 for 3.x dialects
3. **Signing algorithm abstraction** -- Dispatch HMAC-SHA256 vs AES-CMAC by dialect
4. **SMB3 dialect negotiation** -- Select 3.0/3.0.2/3.1.1, update capabilities
5. **Negotiate context parsing/encoding** -- Infrastructure for preauth + encryption contexts
6. **Preauth integrity hash chain** -- SHA-512 chain on Connection and Session
7. **SMB 3.0 key derivation** -- Constant labels/contexts through KDF
8. **SMB 3.1.1 key derivation** -- PreauthHash as context
9. **Kerberos session key extraction** -- Critical gap in current code

### Phase 2: Encryption
Encryption can be built independently once keys are derived.

10. **Transform Header parsing/encoding** -- Encryption envelope framing
11. **AES-128-GCM encrypt/decrypt** -- Primary 3.1.1 cipher (Go stdlib)
12. **AES-128-CCM encrypt/decrypt** -- 3.0 backward compatibility
13. **Per-session encryption** -- Session.EncryptData flag
14. **Per-share encryption** -- Share.EncryptData flag
15. **AES-256 ciphers** -- Incremental on top of 128-bit

### Phase 3: Leases + Security
Build on existing lease infrastructure.

16. **SMB3 lease V2 with directory leases** -- Extend existing lease code
17. **Lease break on directory content change** -- Integrate with metadata
18. **Cross-protocol lease<->delegation** -- Complete bidirectional integration
19. **Secure dialect validation IOCTL** -- FSCTL_VALIDATE_NEGOTIATE_INFO for 3.0/3.0.2
20. **AES-GMAC signing** -- Optional 3.1.1 performance optimization

### Phase 4: Durable Handles
Most complex feature, build last.

21. **Durable Handle V1** -- Basic connection resilience
22. **Durable Handle V2 with CreateGuid** -- Idempotent reconnection
23. **Durable handle state persistence** -- Survive server disconnect
24. **Replay detection** -- SMB2_FLAGS_REPLAY_OPERATION

### Phase 5: Testing + Cross-Protocol
Validation against conformance suites.

25. **smbtorture SMB3 tests** -- durable_v2, lease, replay, session, encryption
26. **WPTS FileServer SMB3 BVT** -- Microsoft conformance
27. **Go integration tests** -- hirochachacha/go-smb2 with SMB3 enabled
28. **Cross-protocol integration tests** -- SMB3 leases vs NFS delegations
29. **Windows 10/11/macOS/Linux client validation** -- Manual + scripted

Defer:
- **Compression** -- Optional, defer to post-v3.8
- **Multichannel** -- Single-node; defer indefinitely
- **Persistent handles** -- Requires HA; defer indefinitely
- **RDMA/QUIC** -- Out of scope for userspace Go

## Complexity Assessment

| Feature Area | Estimated Effort | Confidence | Risk |
|--------------|-----------------|------------|------|
| Negotiate + Contexts | 2-3 phases | HIGH | Low -- well-specified, linear work |
| Preauth Integrity | 1-2 phases | HIGH | Medium -- must capture raw bytes at dispatch layer |
| Key Derivation (KDF) | 1 phase | HIGH | Low -- pure crypto, test vectors available |
| AES Signing (CMAC/GMAC) | 1 phase | HIGH | Low -- well-defined algorithms |
| AES Encryption (CCM/GCM) | 2-3 phases | HIGH | Medium -- transform header framing is tricky |
| Kerberos Session Key | 1 phase | MEDIUM | Medium -- gokrb5 API for key extraction needs verification |
| Secure Dialect Validation | 1 phase | HIGH | Low -- simple IOCTL handler |
| SMB3 Leases | 1-2 phases | HIGH | Low -- mostly extends existing code |
| Durable Handles V1 | 2 phases | MEDIUM | High -- state persistence across disconnects is complex |
| Durable Handles V2 | 2 phases | MEDIUM | High -- CreateGuid dedup, replay detection |
| Cross-Protocol Integration | 1-2 phases | MEDIUM | Medium -- bidirectional coordination edge cases |
| Conformance Testing | 2-3 phases | MEDIUM | Medium -- test infrastructure setup |

## Sources

### HIGH Confidence (Official Specification)

- [MS-SMB2: Generating Cryptographic Keys](https://learn.microsoft.com/en-us/openspecs/windows_protocols/ms-smb2/da4e579e-02ce-4e27-bbce-3fc816a3ff92) -- Key derivation procedure, L=128/256 based on cipher
- [MS-SMB2: SMB2_PREAUTH_INTEGRITY_CAPABILITIES](https://learn.microsoft.com/en-us/openspecs/windows_protocols/ms-smb2/5a07bd66-4734-4af8-abcf-5a44ff7ee0e5) -- Negotiate context structure, SHA-512 only
- [MS-SMB2: Receiving an SMB2 NEGOTIATE Request](https://learn.microsoft.com/en-us/openspecs/windows_protocols/ms-smb2/b39f253e-4963-40df-8dff-2f9040ebbeb1) -- Server-side negotiate processing
- [MS-SMB2: SMB2_CREATE_DURABLE_HANDLE_REQUEST_V2](https://learn.microsoft.com/en-us/openspecs/windows_protocols/ms-smb2/5e361a29-81a7-4774-861d-f290ea53a00e) -- DH2Q structure
- [MS-SMB2: Re-establishing a Durable Open](https://learn.microsoft.com/en-us/openspecs/windows_protocols/ms-smb2/3309c3d1-3daf-4448-9faa-81d2d6aa3315) -- DH2C reconnection
- [MS-SMB2: Handling Validate Negotiate Info](https://learn.microsoft.com/en-us/openspecs/windows_protocols/ms-smb2/0b7803eb-d561-48a4-8654-327803f59ec6) -- FSCTL handler spec

### HIGH Confidence (Microsoft Engineering Blogs with Test Vectors)

- [SMB 2 and SMB 3 Security: Anatomy of Signing and Cryptographic Keys](https://learn.microsoft.com/en-us/archive/blogs/openspecification/smb-2-and-smb-3-security-in-windows-10-the-anatomy-of-signing-and-cryptographic-keys) -- Complete key derivation tables, test vectors for 3.0 and 3.1.1 multichannel, NTLMv2 session key examples
- [SMB 3.1.1 Pre-authentication Integrity](https://learn.microsoft.com/en-us/archive/blogs/openspecification/smb-3-1-1-pre-authentication-integrity-in-windows-10) -- Preauth hash chain mechanics
- [SMB 3.1.1 Encryption in Windows 10](https://learn.microsoft.com/en-us/archive/blogs/openspecification/smb-3-1-1-encryption-in-windows-10) -- Encryption negotiation, cipher selection
- [Encryption in SMB 3.0: A Protocol Perspective](https://learn.microsoft.com/en-us/archive/blogs/openspecification/encryption-in-smb-3-0-a-protocol-perspective) -- Transform header, CCM details
- [SMB3 Secure Dialect Negotiation](https://learn.microsoft.com/en-us/archive/blogs/openspecification/smb3-secure-dialect-negotiation) -- VALIDATE_NEGOTIATE_INFO mechanics

### MEDIUM Confidence (Microsoft Product Documentation)

- [SMB Security Enhancements](https://learn.microsoft.com/en-us/windows-server/storage/file-server/smb-security) -- AES-256-GCM/CCM, GMAC signing overview
- [Overview of SMB Signing](https://learn.microsoft.com/en-us/windows-server/storage/file-server/smb-signing-overview) -- Signing algorithms by dialect
- [Client Caching: Oplock vs Lease](https://learn.microsoft.com/en-us/archive/blogs/openspecification/client-caching-features-oplock-vs-lease) -- Lease vs oplock differences

### MEDIUM Confidence (Community/Reference)

- [Samba SMB3 Kernel Status](https://wiki.samba.org/index.php/SMB3_kernel_status) -- Feature coverage matrix
- [SNIA: Introduction to SMB 3.1](https://www.snia.org/sites/default/files/DavidKruse_Kramer_%20Introduction_to_SMB-3-1_Rev.pdf) -- Protocol overview
- [SMB3 Protocol Update (SambaXP 2019)](https://sambaxp.org/fileadmin/user_upload/sambaxp2019-slides/Talpey_SambaXP2019_smb3_protocol.pdf) -- Negotiate context types list
- DittoFS codebase analysis (`internal/adapter/smb/`, `pkg/adapter/smb/`) -- HIGH confidence for existing state
