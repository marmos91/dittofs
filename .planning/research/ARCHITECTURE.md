# Architecture Patterns: SMB3 Protocol Upgrade (v3.8)

**Domain:** SMB3 protocol features integrated into existing DittoFS SMB2 adapter
**Researched:** 2026-02-28
**Confidence:** HIGH (source code analysis + MS-SMB2 spec + MS-NLMP + SP800-108)

## Executive Summary

The SMB3 upgrade (3.0/3.0.2/3.1.1) maps cleanly onto DittoFS's existing architecture. The key insight is that SMB3's major features -- encryption, upgraded signing, leases, durable handles, and Kerberos -- each have clear integration points in the current codebase. No fundamental architectural changes are needed; the upgrade is additive.

The existing code already has forward-looking infrastructure: dialect constants for 3.0/3.0.2/3.1.1 in `types/constants.go`, Kerberos provider in `pkg/auth/kerberos/`, SPNEGO parsing in `internal/adapter/smb/auth/spnego.go`, lease support in `OplockManager`, and cross-protocol coordination. The task is connecting these pieces and adding the missing cryptographic and protocol layers.

## Recommended Architecture

### Packet Pipeline with SMB3 Encryption

The critical architectural question is where encryption/decryption sits. It MUST sit in the framing layer (`framing.go`), wrapping the existing read/write path, because encrypted SMB3 messages use a **Transform Header** that replaces the NetBIOS framing for the SMB2 payload.

```
Client -> TCP -> NetBIOS Frame -> [Transform Header?] -> SMB2 Header + Body -> Handler
                                   ^                      ^
                                   |                      |
                           Decrypt here              Sign/Verify here
                           (framing.go)              (framing.go + signing/)
```

**Current pipeline (SMB2):**
```
ReadRequest() -> NetBIOS header -> Read message -> Parse SMB2 header -> Verify signature -> Return
```

**New pipeline (SMB3):**
```
ReadRequest() -> NetBIOS header -> Read message -> Check Transform Header?
  -> YES: Decrypt -> Parse SMB2 header -> (signature embedded in transform) -> Return
  -> NO:  Parse SMB2 header -> Verify signature (CMAC/GMAC) -> Return
```

### Component Boundaries

| Component | Responsibility | Status | Communicates With |
|-----------|---------------|--------|-------------------|
| `internal/adapter/smb/framing.go` | NetBIOS framing, Transform Header decrypt/encrypt | **MODIFY** | All (entry/exit point) |
| `internal/adapter/smb/crypto/` | AES-CCM/GCM encrypt/decrypt, key derivation (SP800-108) | **NEW** | framing.go, session |
| `internal/adapter/smb/signing/signing.go` | HMAC-SHA256 + AES-CMAC + AES-GMAC | **MODIFY** | framing.go, session |
| `internal/adapter/smb/v2/handlers/negotiate.go` | Dialect selection 3.0/3.0.2/3.1.1, negotiate contexts | **MODIFY** | session, crypto |
| `internal/adapter/smb/v2/handlers/session_setup.go` | Key derivation, preauth hash, session binding | **MODIFY** | crypto, signing, auth |
| `internal/adapter/smb/v2/handlers/create.go` | Durable handle v1/v2 create contexts | **MODIFY** | durable store |
| `internal/adapter/smb/v2/handlers/lease.go` | Already has V2 lease support | **MINOR MODIFY** | OplockManager |
| `internal/adapter/smb/session/session.go` | Per-session crypto keys (sign/encrypt/decrypt) | **MODIFY** | crypto |
| `internal/adapter/smb/auth/authenticator.go` | Kerberos delegation to shared provider | **MODIFY** | pkg/auth/kerberos |
| `internal/adapter/smb/durable/` | Durable handle persistence and reconnect | **NEW** | metadata store, handler |
| `pkg/adapter/smb/config.go` | Encryption config, cipher selection | **MODIFY** | adapter |
| `pkg/metadata/lock/oplock.go` | OpLock struct (already has Epoch, LeaseKey) | **NO CHANGE** | lease handlers |
| `pkg/auth/kerberos/provider.go` | Shared Kerberos keytab/config | **NO CHANGE** | SMB auth, NFS auth |

### Data Flow: Encrypted SMB3 Request

```
1. TCP accept (BaseAdapter)
2. Connection.Serve() loop:
   a. ReadRequest() reads NetBIOS header (4 bytes)
   b. Read message bytes
   c. Check first 4 bytes: SMB2_TRANSFORM_HEADER_ID (0x424D53FD)?
      YES -> Parse Transform Header (52 bytes)
          -> Decrypt payload using session DecryptionKey + AES-CCM/GCM
          -> message = decrypted plaintext
      NO  -> message = raw bytes (unencrypted session)
   d. Parse SMB2 header from message
   e. Verify signature (if signed and not encrypted)
      - SMB 2.x: HMAC-SHA256 (existing)
      - SMB 3.0/3.0.2: AES-128-CMAC (new)
      - SMB 3.1.1: AES-128-GMAC or AES-256-GMAC (new, only when encryption negotiated)
   f. Dispatch to handler
3. Handler processes, returns result
4. SendMessage():
   a. Build SMB2 header + body
   b. Sign if session requires signing (algorithm depends on dialect)
   c. If session requires encryption:
      -> Build Transform Header
      -> Encrypt (header + body) using EncryptionKey + AES-CCM/GCM
      -> Frame = NetBIOS(TransformHeader + EncryptedPayload)
   d. Else: Frame = NetBIOS(SignedMessage)
   e. WriteNetBIOSFrame()
```

## Integration Point 1: Encryption in Framing Layer

### Where It Sits

Encryption wraps the **entire SMB2 message** (header + body) inside a Transform Header. This means the framing layer (`framing.go`) must handle it because:

1. The Transform Header is read **before** the SMB2 header is parsed
2. Decryption must happen **before** signature verification (signatures are inside the encrypted payload for SMB 3.1.1 GMAC)
3. Encryption on write must happen **after** signing but **before** NetBIOS framing

### Transform Header Structure (MS-SMB2 2.2.41)

```
Offset  Size  Field                Description
------  ----  ------------------   --------------------------------
0       4     ProtocolId           0x424D53FD (0xFD 'S' 'M' 'B')
4       16    Signature            AES-CCM MAC or AES-GCM tag
20      4     Nonce (first 4)      Unique nonce for this message
24      12    Nonce (remaining)    (total 16 bytes, but AES-CCM uses 11, AES-GCM uses 12)
36      4     OriginalMessageSize  Size of unencrypted SMB2 message
40      2     Reserved             Must be 0
42      2     Flags                0x0001 = Encrypted
44      8     SessionId            Session that owns the encryption keys
52      var   EncryptedData        AES-CCM/GCM ciphertext
```

### New Files

**`internal/adapter/smb/crypto/crypto.go`** -- Core encryption/decryption:
```go
package crypto

// TransformHeader represents an SMB3 Transform Header [MS-SMB2 2.2.41]
type TransformHeader struct {
    Signature           [16]byte
    Nonce               [16]byte
    OriginalMessageSize uint32
    Flags               uint16
    SessionID           uint64
}

// CipherID identifies the negotiated encryption algorithm
type CipherID uint16

const (
    CipherAES128CCM CipherID = 0x0001
    CipherAES128GCM CipherID = 0x0002
    CipherAES256CCM CipherID = 0x0003
    CipherAES256GCM CipherID = 0x0004
)

// Encryptor handles per-session AES encryption/decryption
type Encryptor struct {
    cipher    CipherID
    encKey    []byte // EncryptionKey (derived via SMB3KDF)
    decKey    []byte // DecryptionKey (derived via SMB3KDF)
    nonceGen  NonceGenerator
}

func (e *Encryptor) Encrypt(plaintext []byte, sessionID uint64) ([]byte, error)
func (e *Encryptor) Decrypt(header *TransformHeader, ciphertext []byte) ([]byte, error)
```

**`internal/adapter/smb/crypto/kdf.go`** -- Key derivation:
```go
package crypto

// SMB3KDF derives keys per SP800-108 Counter Mode with HMAC-SHA256.
// Used for SigningKey, EncryptionKey, DecryptionKey, ApplicationKey.
//
// For SMB 3.0:   Label/Context are ASCII strings
// For SMB 3.1.1: Context is PreauthIntegrityHashValue (SHA-512)
func SMB3KDF(sessionKey []byte, label, context string, keyLen int) []byte

// DeriveSessionKeys derives all session keys from the session base key.
// Returns SigningKey, EncryptionKey, DecryptionKey, ApplicationKey.
func DeriveSessionKeys(sessionKey []byte, dialect Dialect, preauthHash []byte) *SessionKeys

type SessionKeys struct {
    SigningKey    []byte
    EncryptionKey []byte
    DecryptionKey []byte
    ApplicationKey []byte
}
```

### Modifications to `framing.go`

The existing `ReadRequest()` function needs a new code path after reading the message bytes:

```go
// After reading the message, check for Transform Header
protocolID := binary.LittleEndian.Uint32(message[0:4])
if protocolID == TransformProtocolID { // 0x424D53FD
    // Parse Transform Header, look up session's Encryptor,
    // decrypt, then proceed with SMB2 header parsing on plaintext
    message, err = decryptTransformMessage(message, sessionLookup)
    if err != nil {
        return nil, nil, nil, err
    }
}
```

The existing `SendMessage()` in `response.go` needs encryption wrapping:

```go
// After signing (if applicable), check if session requires encryption
if sess != nil && sess.EncryptionRequired() {
    smbPayload = sess.Encrypt(smbPayload)
}
```

### Connection State

The `ConnInfo` struct needs a session lookup callback for decryption (since the session ID is in the Transform Header, not the SMB2 header):

```go
type ConnInfo struct {
    // ... existing fields ...

    // EncryptionLookup resolves session ID to Encryptor for Transform Header decryption.
    // nil when encryption is not negotiated on this connection.
    EncryptionLookup func(sessionID uint64) *crypto.Encryptor
}
```

## Integration Point 2: Signing Upgrade (HMAC-SHA256 -> CMAC/GMAC)

### Current State

`internal/adapter/smb/signing/signing.go` implements HMAC-SHA256 only. The `SigningKey` struct holds a 16-byte key and uses `crypto/hmac` + `crypto/sha256`.

### Required Changes

The signing package needs to become algorithm-aware. The approach is to introduce a `Signer` interface that the session holds, with implementations for each algorithm:

```go
// signing/signer.go (NEW)
type Algorithm int

const (
    AlgorithmHMACSHA256 Algorithm = iota // SMB 2.0.2 / 2.1
    AlgorithmAESCMAC                      // SMB 3.0 / 3.0.2
    AlgorithmAESGMAC                      // SMB 3.1.1 (only when encryption negotiated)
)

type Signer interface {
    Sign(message []byte) [SignatureSize]byte
    Verify(message []byte) bool
    SignMessage(message []byte) // In-place signing
    Algorithm() Algorithm
}

// HMACSigner wraps existing HMAC-SHA256 logic
type HMACSigner struct { key [KeySize]byte }

// CMACSigner uses AES-128-CMAC (crypto/aes + CMAC construction)
type CMACSigner struct { key [KeySize]byte }

// GMACSigner uses AES-128-GMAC (crypto/aes/gcm with empty plaintext)
type GMACSigner struct { key [KeySize]byte }
```

The existing `SigningKey` type and its methods become the `HMACSigner` implementation. `SessionSigningState` gets a `Signer` field instead of a raw `SigningKey`:

```go
type SessionSigningState struct {
    Signer          Signer  // Algorithm-aware signer
    SigningRequired  bool
    SigningEnabled   bool
}
```

### Key Derivation Differences by Dialect

| Dialect | Signing Key Derivation | Algorithm |
|---------|----------------------|-----------|
| 2.0.2 / 2.1 | SessionKey truncated/padded to 16 bytes | HMAC-SHA256 |
| 3.0 | SMB3KDF(SessionKey, "SMB2AESCMAC\0", "SmbSign\0") | AES-128-CMAC |
| 3.0.2 | SMB3KDF(SessionKey, "SMB2AESCMAC\0", "SmbSign\0") | AES-128-CMAC |
| 3.1.1 | SMB3KDF(SessionKey, "SMBSigningKey\0", PreauthHashValue) | AES-128-CMAC (default) or AES-GMAC (if encryption negotiated) |

### Go Standard Library Support

- **AES-CMAC**: Not in stdlib. Use `crypto/aes` + RFC 4493 CMAC construction (straightforward, ~50 lines). Alternatively, `golang.org/x/crypto/cmac` if available by implementation time.
- **AES-GCM**: `crypto/aes` + `crypto/cipher` GCM mode. For GMAC, use GCM with empty plaintext (tag-only).
- **AES-CCM**: Not in stdlib. Use `golang.org/x/crypto/ccm` or implement per RFC 3610 (~100 lines). The Samba ksmbd implementation is a good reference.

## Integration Point 3: SMB3 Leases and Unified Lock Manager

### Current State (Already Excellent)

The existing lease infrastructure is comprehensive and maps directly to SMB3 requirements:

- `OplockManager` in `handlers/oplock.go` manages both traditional oplocks AND leases
- `LeaseCreateContext` in `handlers/lease.go` already supports V1 and V2 formats (including ParentLeaseKey and Epoch)
- `OpLock` struct in `pkg/metadata/lock/oplock.go` already has `LeaseKey [16]byte`, `LeaseState uint32`, `Epoch uint16`, `Breaking bool`, `BreakToState uint32`
- Cross-protocol coordination (NFS read/write/delete trigger SMB lease breaks) is fully implemented
- Lease persistence via `LockStore` is in place
- Grace period reclaim (`RequestLeaseWithReclaim`) is implemented

### What Needs to Change for SMB3

**Minimal changes.** The existing lease implementation is already SMB3-capable because it was built with LeaseV2 support from the start. Specific additions:

1. **Directory leases**: Already supported in `RequestLease()` via `isDirectory` parameter and `lock.IsValidDirectoryLeaseState()`. Just needs the `SMB2_GLOBAL_CAP_DIRECTORY_LEASING` capability advertised in NEGOTIATE.

2. **Epoch enforcement**: Already tracked in `OpLock.Epoch`. Just needs validation in lease break acknowledgment (epoch must match).

3. **ParentLeaseKey**: Already decoded in `DecodeLeaseCreateContext()`. The `SMB2_LEASE_FLAG_PARENT_LEASE_KEY_SET` flag handling needs to be wired up in the CREATE handler to link directory and file leases.

4. **Lease V2 response in NEGOTIATE**: Advertise `SMB2_GLOBAL_CAP_LEASING` and `SMB2_GLOBAL_CAP_DIRECTORY_LEASING` when dialect >= 3.0.

### Cross-Protocol Integration (Already Done)

The existing cross-protocol lease break coordination in `OplockManager` works correctly for SMB3:
- `CheckAndBreakForWrite()` -- NFS write triggers SMB Write lease break
- `CheckAndBreakForRead()` -- NFS read triggers SMB Write lease break (cached writes)
- `CheckAndBreakForDelete()` -- NFS delete triggers all lease breaks to None
- NLM lock conflict check in `RequestLease()` -- NFS byte-range locks deny SMB leases

No changes needed for cross-protocol integration.

## Integration Point 4: Durable Handles Persistence

### Architecture Decision: Where to Persist

Durable handles MUST persist in the **control plane store** (GORM-based), not the metadata store. Rationale:

1. Durable handles are session-level state (not file-level metadata)
2. They must survive server restarts (so not in-memory)
3. They reference file handles, session IDs, and create GUIDs -- control plane concepts
4. The control plane store already has `GORMStore` with SQLite/PostgreSQL support

### New Sub-Interface

Add a `DurableHandleStore` sub-interface to `pkg/controlplane/store/`:

```go
type DurableHandleStore interface {
    PutDurableHandle(ctx context.Context, handle *DurableHandle) error
    GetDurableHandle(ctx context.Context, createGUID [16]byte) (*DurableHandle, error)
    DeleteDurableHandle(ctx context.Context, createGUID [16]byte) error
    ListDurableHandlesForSession(ctx context.Context, sessionID uint64) ([]*DurableHandle, error)
    CleanupExpiredHandles(ctx context.Context, olderThan time.Time) (int, error)
}
```

### Durable Handle Model

```go
type DurableHandle struct {
    CreateGUID      [16]byte   // Unique handle identifier (from client)
    FileID          [16]byte   // SMB2 FileID
    MetadataHandle  string     // Underlying metadata file handle
    PayloadID       string     // Content identifier
    ShareName       string     // Share the file belongs to
    Path            string     // File path within share
    SessionID       uint64     // Owning session (cleared on disconnect)
    LeaseKey        [16]byte   // Associated lease key (if any)
    LeaseState      uint32     // Lease state at disconnect
    DesiredAccess   uint32     // Original access mask
    CreateOptions   uint32     // Original create options
    IsPersistent    bool       // Persistent (CA share) vs durable
    Timeout         time.Duration // Grace period for reconnect
    DisconnectedAt  time.Time  // When the session disconnected
    CreatedAt       time.Time
}
```

### New Package

**`internal/adapter/smb/durable/`**:
- `store.go` -- DurableHandleStore GORM implementation
- `manager.go` -- DurableHandleManager (lifecycle, reconnect, timeout)
- `context.go` -- Create context parsing (DURABLE_HANDLE_REQUEST_V2, RECONNECT_V2)

### CREATE Handler Integration

In `handlers/create.go`, the durable handle flow:

1. **New open**: If create request includes `SMB2_CREATE_DURABLE_HANDLE_REQUEST_V2` context:
   - Open the file normally
   - Persist `DurableHandle` in store
   - Return `SMB2_CREATE_DURABLE_HANDLE_RESPONSE_V2` in response contexts

2. **Reconnect**: If create request includes `SMB2_CREATE_DURABLE_HANDLE_RECONNECT_V2` context:
   - Look up `DurableHandle` by CreateGUID
   - Validate the client (session binding or lease key match)
   - Restore the `OpenFile` state from persisted data
   - Delete the disconnect record
   - Return existing FileID + lease state

3. **Disconnect cleanup**: When a connection drops, `Connection.handleConnectionClose()` should mark durable handles as disconnected rather than closing them:
   - Files with active durable handles: set `DisconnectedAt`, do NOT delete from `files` sync.Map immediately
   - Files without durable handles: close normally (existing behavior)

## Integration Point 5: SPNEGO/Kerberos Reuse

### Current State

The Kerberos authentication path is already functional:

1. `pkg/auth/kerberos/Provider` loads keytab, manages hot-reload, implements `AuthProvider`
2. `internal/adapter/smb/auth/spnego.go` parses SPNEGO tokens, detects NTLM vs Kerberos
3. `handlers/session_setup.go:handleKerberosAuth()` validates AP-REQ, maps principal to user, creates session
4. `Adapter.SetKerberosProvider()` injects the shared provider

### SMB3 Kerberos Changes

For SMB3, the Kerberos path needs **session key extraction** for signing/encryption key derivation:

```go
// In handleKerberosAuth(), after service.VerifyAPREQ():
sessionKey := creds.SessionKey()
// Then derive SMB3 keys from sessionKey via SMB3KDF
keys := crypto.DeriveSessionKeys(sessionKey, negotiatedDialect, preauthHash)
sess.SetSigningKey(keys.SigningKey)
sess.SetEncryptionKeys(keys.EncryptionKey, keys.DecryptionKey)
```

The `gokrb5/v8` library's `service.VerifyAPREQ()` returns credentials that include the session key. Currently, `handleKerberosAuth()` ignores this because SMB2 signing with Kerberos was not needed. For SMB3, this is the key material for all cryptographic operations.

### NTLM Path Changes

The NTLM path in `completeNTLMAuth()` already derives a signing key. For SMB3, the derivation changes:

- **SMB 2.x**: `signingKey = sessionBaseKey` (direct or KEY_EXCH decrypted)
- **SMB 3.x**: `signingKey = SMB3KDF(sessionBaseKey, label, context)`

The existing `auth.DeriveSigningKey()` returns the SMB2 signing key. For SMB3, this needs to be extended to call `crypto.DeriveSessionKeys()` when the negotiated dialect is 3.0+.

## Integration Point 6: Negotiate Contexts (SMB 3.1.1)

### Current State

`handlers/negotiate.go` currently:
- Selects highest dialect from {0x0202, 0x0210}
- Advertises `CapLeasing | CapLargeMTU` for dialect >= 2.1
- Returns 65-byte response body with zeroed NegotiateContextOffset/Count

### Required Changes

For SMB 3.x dialect negotiation:

```go
// Extend dialect selection
case types.SMB2Dialect0311:
    if selectedDialect < types.SMB2Dialect0311 {
        selectedDialect = types.SMB2Dialect0311
    }
case types.SMB2Dialect0302:
    // ...
case types.SMB2Dialect0300:
    // ...
```

For SMB 3.1.1, parse and respond to negotiate contexts:

1. **SMB2_PREAUTH_INTEGRITY_CAPABILITIES** (0x0001):
   - Parse: Client offers hash algorithms (SHA-512 is the only one)
   - Respond: Select SHA-512, generate server salt
   - Start preauth integrity hash chain

2. **SMB2_ENCRYPTION_CAPABILITIES** (0x0002):
   - Parse: Client offers cipher IDs (AES-128-CCM, AES-128-GCM, AES-256-CCM, AES-256-GCM)
   - Respond: Select preferred cipher (AES-128-GCM preferred for performance)
   - Store on connection state

3. **SMB2_SIGNING_CAPABILITIES** (0x0008):
   - Parse: Client offers signing algorithms (HMAC-SHA256, AES-CMAC, AES-GMAC)
   - Respond: Select based on dialect and encryption

### Preauth Integrity Hash Chain

SMB 3.1.1 requires maintaining a SHA-512 hash chain across NEGOTIATE and SESSION_SETUP:

```go
// On Connection:
type PreauthState struct {
    HashAlgorithm uint16    // Always SHA-512 (0x0001)
    HashValue     [64]byte  // Running SHA-512 hash
}

// Updated after each NEGOTIATE/SESSION_SETUP message:
// HashValue = SHA-512(HashValue || messageBytes)
```

This hash becomes the `Context` parameter for SMB3KDF key derivation in SMB 3.1.1.

### Capabilities Flags

For dialect >= 3.0, advertise additional capabilities:

```go
if selectedDialect >= types.SMB2Dialect0300 {
    capabilities |= uint32(types.CapDirectoryLeasing | types.CapEncryption)
    // CapMultiChannel deferred -- single connection per session for now
    // CapPersistentHandles deferred -- only for CA shares
}
```

## Patterns to Follow

### Pattern 1: Algorithm Dispatch via Dialect

**What:** Use the negotiated dialect to select cryptographic algorithms at session creation time, then store the concrete `Signer`/`Encryptor` on the session. Never check dialect in hot paths.

**When:** Signing, encryption, key derivation

**Example:**
```go
func NewSessionCrypto(dialect types.Dialect, sessionKey []byte, preauthHash []byte) *SessionCrypto {
    switch {
    case dialect >= types.Dialect0311:
        keys := crypto.DeriveSessionKeys(sessionKey, dialect, preauthHash)
        return &SessionCrypto{
            Signer:    signing.NewCMACSigner(keys.SigningKey),
            Encryptor: crypto.NewEncryptor(cipher, keys.EncryptionKey, keys.DecryptionKey),
        }
    case dialect >= types.Dialect0300:
        keys := crypto.DeriveSessionKeys(sessionKey, dialect, nil)
        return &SessionCrypto{
            Signer: signing.NewCMACSigner(keys.SigningKey),
            // Encryption optional for 3.0/3.0.2
        }
    default:
        return &SessionCrypto{
            Signer: signing.NewHMACSigner(sessionKey),
        }
    }
}
```

### Pattern 2: Connection-Level Negotiated State

**What:** Store negotiated dialect, cipher, and preauth state on the Connection (not globally). Different connections can negotiate different dialects.

**When:** Multi-version support

**Example:**
```go
// Add to pkg/adapter/smb/connection.go
type Connection struct {
    // ... existing fields ...

    // SMB3 negotiated state (per-connection)
    NegotiatedDialect types.Dialect
    NegotiatedCipher  crypto.CipherID
    PreauthState      *PreauthState // nil for dialect < 3.1.1
    EncryptData       bool          // Global encryption required
}
```

### Pattern 3: Durable Handle as CREATE Context Extension

**What:** Parse durable handle create contexts alongside existing create contexts (MxAc, QFid, Lease) in the CREATE handler. Use the same context parsing infrastructure.

**When:** Durable handle create/reconnect

**Example:**
```go
// In create.go, extend context parsing:
case "DHnQ": // SMB2_CREATE_DURABLE_HANDLE_REQUEST_V2
    durableReq, err = durable.DecodeRequestV2(contextData)
case "DH2C": // SMB2_CREATE_DURABLE_HANDLE_RECONNECT_V2
    reconnectReq, err = durable.DecodeReconnectV2(contextData)
```

### Pattern 4: Preauth Hash as Pipeline State

**What:** Thread the preauth integrity hash value through the NEGOTIATE and SESSION_SETUP pipeline as connection-level state that accumulates. Each message's raw bytes are hashed into the running value.

**When:** SMB 3.1.1 connections

**Example:**
```go
// In framing.go, after reading raw message bytes but before parsing:
if conn.PreauthState != nil {
    conn.PreauthState.Update(rawMessageBytes)
}
// The hash value is then used as KDF context during SESSION_SETUP completion
```

## Anti-Patterns to Avoid

### Anti-Pattern 1: Encryption in Handler Layer

**What:** Implementing encryption/decryption inside individual command handlers.
**Why bad:** Every handler would need to know about encryption. The transform header wraps the entire message, not individual commands. Compound requests must be encrypted as a unit.
**Instead:** Handle exclusively in the framing layer (`ReadRequest`/`SendMessage`).

### Anti-Pattern 2: Global Dialect State

**What:** Storing the negotiated dialect as a server-wide global.
**Why bad:** Different connections can negotiate different dialects. A Windows 10 client may negotiate 3.1.1 while a macOS client on the same server negotiates 2.1.
**Instead:** Store negotiated dialect per-connection. Pass dialect through `ConnInfo` or `SMBHandlerContext`.

### Anti-Pattern 3: Separate Lock Manager for SMB3 Leases

**What:** Creating a new lease management system for SMB3 alongside the existing `OplockManager`.
**Why bad:** The existing `OplockManager` already supports V2 leases with all SMB3 features (epoch, parent lease key, directory leases, cross-protocol breaks). Creating a parallel system would fragment lease state and break cross-protocol coordination.
**Instead:** Continue using the existing `OplockManager` and `LockStore`. Just advertise the capability in NEGOTIATE and ensure epoch validation in lease break acknowledgment.

### Anti-Pattern 4: Storing Durable Handles in Memory

**What:** Keeping durable handle state only in the `Handler.files` sync.Map.
**Why bad:** Durable handles must survive server restarts. The entire point is connection resilience.
**Instead:** Persist in the control plane GORM store. Restore on reconnect by querying the store.

### Anti-Pattern 5: Implementing All Ciphers Immediately

**What:** Building AES-128-CCM, AES-128-GCM, AES-256-CCM, and AES-256-GCM all at once.
**Why bad:** Windows 10/11 prefer AES-128-GCM and always offer it. AES-256 variants are only needed for specific compliance scenarios. Implementing all four ciphers doubles the test matrix for minimal benefit.
**Instead:** Start with AES-128-GCM (preferred by all modern clients), add AES-128-CCM second (required for compatibility with older Windows). Defer AES-256 variants to a later phase.

## Suggested Build Order

The build order is driven by **dependency chains**: later features depend on earlier ones.

### Phase 1: SMB3 Negotiate Foundation

**What:** Dialect negotiation for 3.0/3.0.2/3.1.1, negotiate contexts (preauth integrity, encryption capabilities, signing capabilities), preauth integrity hash chain.

**Why first:** Everything else depends on successful dialect negotiation. Without this, no SMB3 features can be tested.

**Files modified:**
- `internal/adapter/smb/v2/handlers/negotiate.go` -- Extend dialect selection, parse/respond to negotiate contexts
- `internal/adapter/smb/types/constants.go` -- SMB3 negotiate context types and IDs
- `internal/adapter/smb/session/session.go` -- Add NegotiatedDialect, PreauthState fields
- `pkg/adapter/smb/connection.go` -- Per-connection negotiated state
- `pkg/adapter/smb/config.go` -- Encryption/signing configuration options

**Dependencies:** None
**Testable with:** Windows `smbclient` dialect negotiation, Wireshark packet capture

### Phase 2: SMB3 Key Derivation and Signing

**What:** SP800-108 KDF, AES-CMAC signer, AES-GMAC signer, session key derivation for SMB 3.x, signing algorithm selection.

**Why second:** Signing is required before encryption can work (encryption key derivation shares the same KDF). Also, NTLM and Kerberos auth both need the new key derivation path.

**Files created:**
- `internal/adapter/smb/crypto/kdf.go` -- SMB3KDF implementation
- `internal/adapter/smb/crypto/kdf_test.go` -- KDF test vectors from MS-SMB2 spec
- `internal/adapter/smb/signing/cmac.go` -- AES-CMAC signer
- `internal/adapter/smb/signing/gmac.go` -- AES-GMAC signer
- `internal/adapter/smb/signing/signer.go` -- Signer interface

**Files modified:**
- `internal/adapter/smb/signing/signing.go` -- Refactor to use Signer interface
- `internal/adapter/smb/v2/handlers/session_setup.go` -- Use SMB3KDF for key derivation
- `internal/adapter/smb/session/session.go` -- Hold Signer instead of raw SigningKey

**Dependencies:** Phase 1 (dialect negotiation determines which algorithm to use)
**Testable with:** smbtorture signing tests, Windows client with signing required

### Phase 3: SMB3 Encryption

**What:** AES-CCM/GCM encrypt/decrypt, Transform Header parsing/building, framing layer integration, per-session and per-share encryption control.

**Why third:** Depends on key derivation (Phase 2) for EncryptionKey/DecryptionKey. Also needs dialect negotiation (Phase 1) for cipher selection.

**Files created:**
- `internal/adapter/smb/crypto/crypto.go` -- Encryptor, Transform Header
- `internal/adapter/smb/crypto/crypto_test.go` -- Encryption test vectors
- `internal/adapter/smb/crypto/ccm.go` -- AES-CCM implementation (if not using x/crypto)

**Files modified:**
- `internal/adapter/smb/framing.go` -- Transform Header detection, decrypt on read, encrypt on write
- `internal/adapter/smb/response.go` -- Encrypt outgoing messages in SendMessage
- `internal/adapter/smb/conn_types.go` -- Add EncryptionLookup to ConnInfo
- `internal/adapter/smb/session/session.go` -- EncryptionRequired, Encryptor fields
- `pkg/adapter/smb/config.go` -- Per-share encryption configuration

**Dependencies:** Phase 1 + Phase 2
**Testable with:** smbtorture encryption tests, Windows client with encryption required, Wireshark verification

### Phase 4: SPNEGO/Kerberos SMB3 Integration

**What:** Extract session key from Kerberos AP-REQ for SMB3 key derivation, session binding for preauth hash, mutual auth support.

**Why fourth:** Depends on key derivation infrastructure (Phase 2). The NTLM path should work first since it is simpler.

**Files modified:**
- `internal/adapter/smb/v2/handlers/session_setup.go` -- Extract Kerberos session key, derive SMB3 keys
- `internal/adapter/smb/auth/authenticator.go` -- Return session key in AuthResult

**Dependencies:** Phase 2 (key derivation), Phase 1 (dialect)
**Testable with:** AD-joined Windows client, kinit + smbclient from Linux

### Phase 5: SMB3 Leases (Directory + Enhanced)

**What:** Advertise directory leasing capability, handle ParentLeaseKey, enforce epoch in break acknowledgment. Wire up to existing OplockManager.

**Why fifth:** Leases are mostly done. This phase is about advertising capabilities and wiring up the remaining SMB3-specific bits.

**Files modified:**
- `internal/adapter/smb/v2/handlers/negotiate.go` -- Advertise CapDirectoryLeasing
- `internal/adapter/smb/v2/handlers/create.go` -- Handle ParentLeaseKey in lease context
- `internal/adapter/smb/v2/handlers/lease.go` -- Epoch validation in AcknowledgeLeaseBreak

**Dependencies:** Phase 1 (dialect >= 3.0 for directory leasing)
**Testable with:** smbtorture lease tests, Windows Explorer directory caching behavior

### Phase 6: Durable Handles v1/v2

**What:** Durable handle create context parsing, persistence in control plane store, reconnect processing, timeout management.

**Why sixth:** Depends on leases (Phase 5) because durable handles are tightly coupled with leases. Also needs encryption (Phase 3) because persistent handles require encryption.

**Files created:**
- `internal/adapter/smb/durable/store.go` -- GORM persistence
- `internal/adapter/smb/durable/manager.go` -- Lifecycle management
- `internal/adapter/smb/durable/context.go` -- Create context parsing
- `internal/adapter/smb/durable/store_test.go`

**Files modified:**
- `internal/adapter/smb/v2/handlers/create.go` -- Parse DHnQ/DH2C contexts
- `internal/adapter/smb/v2/handlers/handler.go` -- DurableManager field
- `pkg/adapter/smb/connection.go` -- Mark durable handles on disconnect
- `pkg/controlplane/store/interface.go` -- Add DurableHandleStore sub-interface

**Dependencies:** Phase 1 + Phase 5 (leases)
**Testable with:** smbtorture durable_v2 tests, network disconnect/reconnect simulation

### Phase 7: Cross-Protocol Integration and Testing

**What:** Verify SMB3 lease <-> NFS delegation interop, encryption + signing end-to-end, durable handle recovery, Windows/macOS/Linux client compatibility.

**Why last:** Integration testing requires all components to be in place.

**Files created:**
- E2E test files for encryption, signing, leases, Kerberos
- smbtorture test configurations for SMB3 features
- Go integration tests (hirochachacha/go-smb2)

**Dependencies:** All previous phases

## Scalability Considerations

| Concern | At 100 users | At 10K users | At 1M users |
|---------|--------------|--------------|-------------|
| Encryption overhead | Negligible (AES-NI) | ~5% CPU increase | May need hardware AES offload |
| Signing overhead | Negligible | ~2% CPU increase | CMAC faster than HMAC-SHA256 |
| Durable handle storage | In-process SQLite | PostgreSQL recommended | Shard by session |
| Preauth hash chain | Per-connection SHA-512 | Memory: 64 bytes/conn | 64MB for 1M connections |
| Lease persistence | LockStore handles it | Same as existing | Same as existing |

## Sources

- [MS-SMB2 Generating Cryptographic Keys](https://learn.microsoft.com/en-us/openspecs/windows_protocols/ms-smb2/da4e579e-02ce-4e27-bbce-3fc816a3ff92) -- SP800-108 KDF, key derivation labels/contexts
- [MS-SMB2 SMB2_PREAUTH_INTEGRITY_CAPABILITIES](https://learn.microsoft.com/en-us/openspecs/windows_protocols/ms-smb2/5a07bd66-4734-4af8-abcf-5a44ff7ee0e5) -- Preauth integrity context structure
- [SMB 3.1.1 Pre-authentication integrity](https://learn.microsoft.com/en-us/archive/blogs/openspecification/smb-3-1-1-pre-authentication-integrity-in-windows-10) -- SHA-512 hash chain overview
- [SMB 3.1.1 Encryption in Windows 10](https://learn.microsoft.com/en-us/archive/blogs/openspecification/smb-3-1-1-encryption-in-windows-10) -- AES-CCM/GCM cipher negotiation
- [Encryption in SMB 3.0](https://learn.microsoft.com/en-us/archive/blogs/openspecification/encryption-in-smb-3-0-a-protocol-perspective) -- Transform Header format, encryption flow
- [SMB Security Enhancements](https://learn.microsoft.com/en-us/windows-server/storage/file-server/smb-security) -- AES-CMAC/GMAC signing overview
- [MS-SMB2 SMB2_CREATE_DURABLE_HANDLE_REQUEST_V2](https://learn.microsoft.com/en-us/openspecs/windows_protocols/ms-smb2/5e361a29-81a7-4774-861d-f290ea53a00e) -- Durable handle v2 create context
- [MS-SMB2 Re-establishing a Durable Open](https://learn.microsoft.com/en-us/openspecs/windows_protocols/ms-smb2/3309c3d1-3daf-4448-9faa-81d2d6aa3315) -- Reconnect processing
- [MS-SMB2 Handling DURABLE_HANDLE_RECONNECT_V2](https://learn.microsoft.com/en-us/openspecs/windows_protocols/ms-smb2/62ba68d0-8806-4aef-a229-eefb5827160f) -- Server-side reconnect validation
- [MS-SMB2 Receiving NEGOTIATE Request](https://learn.microsoft.com/en-us/openspecs/windows_protocols/ms-smb2/b39f253e-4963-40df-8dff-2f9040ebbeb1) -- Negotiate context processing
- [NIST SP800-108](https://nvlpubs.nist.gov/nistpubs/Legacy/SP/nistspecialpublication800-108.pdf) -- KDF in Counter Mode specification
- [Implementing Persistent Handles in Samba](https://samba.plus/fileadmin/proposals/Persistent-Handles.pdf) -- Reference implementation details
- DittoFS source code analysis: `internal/adapter/smb/`, `pkg/adapter/smb/`, `pkg/auth/kerberos/`, `pkg/metadata/lock/`
