# Phase 35: Encryption and Transform Header - Research

**Researched:** 2026-03-02
**Domain:** SMB3 message encryption (AES-CCM/GCM) with transform header framing
**Confidence:** HIGH

## Summary

SMB3 encryption wraps SMB2 messages in a 52-byte Transform Header (ProtocolId 0xFD534D42) followed by the AEAD-encrypted ciphertext. The encryption uses AES-CCM (for SMB 3.0/3.0.2) or AES-GCM (for SMB 3.1.1 with cipher negotiation), with the session's encryption/decryption keys derived via SP800-108 KDF (already implemented in Phase 34). The transform header's Nonce, OriginalMessageSize, Flags, and SessionId fields (32 bytes starting at offset 20) serve as Additional Authenticated Data (AAD). The AEAD authentication tag becomes the Signature field.

DittoFS already has all the key derivation infrastructure in place: `SessionCryptoState` holds EncryptionKey/DecryptionKey fields populated by `DeriveAllKeys()`, cipher constants are defined, and the NEGOTIATE handler already selects ciphers. The remaining work is: (1) CCM implementation (Go stdlib only provides GCM), (2) Transform Header parse/encode in the header package, (3) Encryptor interface with GCM/CCM implementations, (4) framing layer integration for 0xFD detection and transparent encrypt/decrypt, (5) enforcement logic in session setup and tree connect, and (6) adapter/share config additions.

**Primary recommendation:** Follow the established `signing.Signer` pattern to create an `encryption.Encryptor` interface with `GCMEncryptor` and `CCMEncryptor` implementations, integrate into the framing layer via a new `EncryptionMiddleware` interface (parallel to `SigningVerifier`), and use the `pion/dtls/v2/pkg/crypto/ccm` package for CCM since Go's stdlib only provides GCM.

<user_constraints>
## User Constraints (from CONTEXT.md)

### Locked Decisions
- Encryption config lives on the **SMB adapter** in the control plane (not global server config)
- Config field is a **three-value enum** `encryption_mode`: `disabled`, `preferred`, `required`
  - `disabled`: no encryption
  - `preferred`: server sets EncryptData flag on 3.x sessions, but accepts unencrypted requests (permissive/mixed model)
  - `required`: only SMB 3.x clients with encryption can connect; SMB 2.x clients are rejected at NEGOTIATE
- Per-share encryption via `encrypt_data` boolean on the **Share model** in the control plane store, set via `dfsctl share create --encrypt`
  - TREE_CONNECT returns `SMB2_SHAREFLAG_ENCRYPT_DATA` for encrypted shares
  - In `required` mode, unencrypted TREE_CONNECT to an encrypted share returns STATUS_ACCESS_DENIED
- **Guest/anonymous sessions are exempt** from encryption requirements (no session key to derive encryption keys)
- When encryption is required but client can't encrypt: reject at SESSION_SETUP (adapter-level) or TREE_CONNECT (share-level) with STATUS_ACCESS_DENIED
- `dfsctl adapter status` should show encryption state per active session (cipher in use, encrypted yes/no)
- **Session reauthentication/binding**: basic support in this phase -- re-derive encryption/decryption keys from new session key using new connection's preauth hash; old keys are destroyed
- Decrypt step happens in the **framing layer** (`framing.go`), before `parseSMB2Message`
- Encrypt step happens in `WriteNetBIOSFrame` -- extend to accept encryption context; if encryption is active, wrap payload in transform header before writing
- **Separate `EncryptionMiddleware` interface** (not combined with SigningVerifier): `DecryptRequest` and `EncryptResponse` methods
- Session lookup for decryption: inject a `func(sessionID uint64) ([]byte, error)` closure that returns the decryption key
- Compound requests: decrypt the entire transform payload once, then process all compound commands inside
- **Nonce generation**: use `crypto/rand` for each encrypted message (fresh random nonce per message)
- Transform header parsing/encoding lives in the existing **`header/` package** alongside SMB2Header
- Decrypted request buffers stay alive for handler lifetime; pool return after handler completes
- All responses on encrypted sessions are encrypted, including error responses
- Default cipher preference order: **AES-256-GCM > AES-256-CCM > AES-128-GCM > AES-128-CCM** (256-bit prioritized)
- **`allowed_ciphers`** config on adapter: list of allowed ciphers; order defines server preference
  - Default when unconfigured: all four ciphers in the priority order above
  - Empty list or no cipher match: negotiate without encryption capability; if `required` mode, session setup fails later
- SMB 3.0/3.0.2 clients: always use AES-128-CCM (no cipher negotiation for pre-3.1.1)
- Log negotiated cipher at **INFO level** during session setup
- Include `BenchmarkEncryptGCM` and `BenchmarkEncryptCCM` tests
- **Decryption failure** (AEAD auth tag mismatch): drop connection silently, log at WARN
- **Unknown session ID in transform header**: drop message silently, log at WARN
- **Unencrypted request on encrypted session** (in `required` mode): return STATUS_ACCESS_DENIED, keep connection open
- **Consecutive failure threshold**: close connection after 5 consecutive decryption failures; counter resets on success
- Error responses on encrypted sessions are always encrypted
- **`Encryptor` interface** mirroring `signing.Signer` pattern: `Encrypt(plaintext, aad) (ciphertext, error)` and `Decrypt(ciphertext, aad) (plaintext, error)`
  - Implementations: `GCMEncryptor`, `CCMEncryptor` in new `internal/adapter/smb/encryption/` package
- **Package structure**:
  - `header/` -- TransformHeader struct, Parse/Encode (wire format)
  - `encryption/` -- Encryptor interface, GCM/CCM implementations, middleware
  - `session/crypto_state.go` -- already has EncryptionKey/DecryptionKey fields (now activated)
- Test coverage: unit tests (encrypt/decrypt round-trip, bad key, tampered data, bad AAD) + MSVP smbtorture encryption suite
- Update CONFIGURATION.md with new `encryption_mode` and `allowed_ciphers` adapter config options

### Claude's Discretion
- Exact transform header field layout and endianness handling
- Internal buffer management for encrypt/decrypt operations
- CCM nonce size handling (11 vs 12 bytes per spec)
- Exact AAD construction for transform header fields
- Config validation error messages
- Whether to use sync.Pool for encryption buffers

### Deferred Ideas (OUT OF SCOPE)
- **NFS encryption (krb5p)**: shared encryption layer across protocols deferred
- **Prometheus metrics for encryption**: encrypted/unencrypted counters, cipher distribution, decrypt latency
- **Full session reauthentication/binding**: basic crypto plumbing only; full durable handle reconnect is Phase 38
</user_constraints>

<phase_requirements>
## Phase Requirements

| ID | Description | Research Support |
|----|-------------|-----------------|
| ENC-01 | Server encrypts/decrypts messages using AES-128-GCM with transform header framing | GCM via Go stdlib `crypto/cipher.NewGCM`; transform header structure fully documented with test vectors; AAD = bytes 20-51 of transform header |
| ENC-02 | Server encrypts/decrypts messages using AES-128-CCM for 3.0/3.0.2 compatibility | CCM via `pion/dtls/v2/pkg/crypto/ccm.NewCCM` with tagsize=16, noncesize=11; SMB 3.0/3.0.2 always uses AES-128-CCM regardless of negotiate contexts |
| ENC-03 | Server supports AES-256-GCM and AES-256-CCM cipher variants | Same AEAD implementations with 32-byte keys instead of 16; key length already determined by `DeriveAllKeys` based on cipher ID |
| ENC-04 | Server enforces per-session encryption via Session.EncryptData flag | Set `SessionFlagEncryptData` (0x0004) in SESSION_SETUP response when adapter `encryption_mode` is `preferred` or `required` for 3.x sessions |
| ENC-05 | Server enforces per-share encryption via Share.EncryptData configuration | Add `encrypt_data` boolean to Share model; set `SMB2_SHAREFLAG_ENCRYPT_DATA` (0x0008) in TREE_CONNECT response ShareFlags |
| ENC-06 | Framing layer detects transform header (0xFD) and decrypts before dispatch | Check `protocolID` in `ReadRequest` for `0x424D53FD`; parse TransformHeader, look up session, decrypt with DecryptionKey, pass inner SMB2 message to `parseSMB2Message` |
</phase_requirements>

## Standard Stack

### Core
| Library | Version | Purpose | Why Standard |
|---------|---------|---------|--------------|
| `crypto/aes` | Go stdlib | AES block cipher | Standard, hardware-accelerated on ARM64/x86 |
| `crypto/cipher` | Go stdlib | GCM AEAD mode | NewGCM for AES-128-GCM and AES-256-GCM |
| `crypto/rand` | Go stdlib | Nonce generation | Cryptographically secure random for per-message nonces |
| `github.com/pion/dtls/v2/pkg/crypto/ccm` | v2.x | CCM AEAD mode | Go stdlib lacks CCM; pion is battle-tested in DTLS (WebRTC) |

### Supporting
| Library | Version | Purpose | When to Use |
|---------|---------|---------|-------------|
| `encoding/binary` | Go stdlib | Transform header wire format | Little-endian encoding for 52-byte header |
| `internal/adapter/smb/smbenc` | Project lib | SMB2 codec | Consistent with existing encode/decode patterns |

### Alternatives Considered
| Instead of | Could Use | Tradeoff |
|------------|-----------|----------|
| `pion/dtls/v2/pkg/crypto/ccm` | `github.com/pschlump/aesccm` | pschlump/aesccm is simpler but less maintained; pion has active community |
| `pion/dtls/v2/pkg/crypto/ccm` | Hand-rolled CCM | CCM is complex (CBC-MAC + CTR); library is battle-tested, implements cipher.AEAD |
| `pion/dtls/v2/pkg/crypto/ccm` | Copy ccm.go into project | Avoids dependency but loses upstream fixes; acceptable alternative if dependency is concern |

**Dependency decision:** Use `pion/dtls/v2/pkg/crypto/ccm`. It implements `cipher.AEAD`, is well-tested in production WebRTC stacks, and its CCM implementation matches RFC 3610. Alternatively, the CCM code (~200 lines) could be vendored directly to avoid the full pion/dtls dependency tree. **Recommendation: vendor the single ccm.go file** to avoid pulling in the entire DTLS dependency for one file.

## Architecture Patterns

### Recommended Package Structure
```
internal/adapter/smb/
├── encryption/                  # NEW: Encryption package
│   ├── encryptor.go            # Encryptor interface + NewEncryptor factory
│   ├── gcm_encryptor.go        # GCM implementation (128 + 256)
│   ├── gcm_encryptor_test.go
│   ├── ccm_encryptor.go        # CCM implementation (128 + 256)
│   ├── ccm_encryptor_test.go
│   ├── ccm.go                  # Vendored CCM from pion/dtls (if vendoring)
│   ├── middleware.go           # EncryptionMiddleware interface
│   └── doc.go                  # Package documentation
├── header/
│   ├── transform_header.go     # NEW: TransformHeader struct + Parse/Encode
│   └── transform_header_test.go
├── session/
│   └── crypto_state.go         # EXISTING: EncryptionKey/DecryptionKey (activate)
├── framing.go                  # MODIFY: Add 0xFD detection + decrypt
├── response.go                 # MODIFY: Add encrypt before send
└── conn_types.go               # MODIFY: Add encryption context to ConnInfo
```

### Pattern 1: Encryptor Interface (mirrors signing.Signer)
**What:** Polymorphic encryption abstraction dispatched by cipher ID
**When to use:** All encrypt/decrypt operations through session-associated Encryptor

```go
// Source: mirrors internal/adapter/smb/signing/signer.go pattern
package encryption

import "crypto/cipher"

// Encryptor provides AEAD encryption/decryption for SMB3 messages.
type Encryptor interface {
    // Encrypt encrypts plaintext with the given AAD, returning nonce + ciphertext.
    // The nonce is generated internally using crypto/rand.
    Encrypt(plaintext, aad []byte) (nonce []byte, ciphertext []byte, err error)

    // Decrypt decrypts ciphertext with the given nonce and AAD.
    Decrypt(nonce, ciphertext, aad []byte) ([]byte, error)

    // NonceSize returns the nonce size for this cipher (11 for CCM, 12 for GCM).
    NonceSize() int

    // Overhead returns the authentication tag size (always 16 for SMB3).
    Overhead() int
}

// NewEncryptor creates an Encryptor for the given cipher ID and key.
func NewEncryptor(cipherId uint16, key []byte) (Encryptor, error) {
    switch cipherId {
    case types.CipherAES128GCM, types.CipherAES256GCM:
        return NewGCMEncryptor(key)
    case types.CipherAES128CCM, types.CipherAES256CCM:
        return NewCCMEncryptor(key)
    default:
        return nil, fmt.Errorf("unsupported cipher ID: 0x%04x", cipherId)
    }
}
```

### Pattern 2: Transform Header Wire Format
**What:** 52-byte header preceding encrypted SMB2 messages
**When to use:** Every encrypted request/response on the wire

```go
// Source: [MS-SMB2] Section 2.2.41
package header

const (
    TransformHeaderSize = 52
    TransformProtocolID = 0x424D53FD // 0xFD 'S' 'M' 'B' (little-endian)
)

// TransformHeader represents the SMB2 TRANSFORM_HEADER.
//
// Wire layout (52 bytes):
//   Offset  Size  Field
//   ------  ----  -------------------
//   0       4     ProtocolId (0xFD534D42 LE)
//   4       16    Signature (AEAD auth tag)
//   20      16    Nonce (CCM: 11+5pad, GCM: 12+4pad)
//   36      4     OriginalMessageSize
//   40      2     Reserved (must be 0)
//   42      2     Flags/EncryptionAlgorithm (0x0001)
//   44      8     SessionId
type TransformHeader struct {
    Signature           [16]byte
    Nonce               [16]byte
    OriginalMessageSize uint32
    Flags               uint16
    SessionId           uint64
}
```

**AAD Construction:**
The AAD for AEAD operations is the 32 bytes of the transform header starting at the Nonce field (offsets 20-51):
```
AAD = Nonce(16) || OriginalMessageSize(4) || Reserved(2) || Flags(2) || SessionId(8) = 32 bytes
```

### Pattern 3: EncryptionMiddleware in Framing Layer
**What:** Transparent decrypt on read, encrypt on write
**When to use:** Injected into connection handling, parallel to SigningVerifier

```go
// Source: mirrors framing.go SigningVerifier pattern
package smb

// EncryptionMiddleware handles transparent encryption/decryption of SMB3 messages.
type EncryptionMiddleware interface {
    // DecryptRequest decrypts a transform-header-wrapped message.
    // Returns the decrypted SMB2 message bytes and the session ID.
    DecryptRequest(transformMessage []byte) (smb2Message []byte, sessionID uint64, err error)

    // EncryptResponse wraps an SMB2 message in a transform header.
    // Returns the complete encrypted message (transform header + ciphertext).
    EncryptResponse(sessionID uint64, smb2Message []byte) ([]byte, error)

    // ShouldEncrypt returns true if the session requires encryption.
    ShouldEncrypt(sessionID uint64) bool
}
```

### Pattern 4: Session Encryption State
**What:** Track per-session encryption state and encryptor instances
**When to use:** After SESSION_SETUP completes with encryption enabled

```go
// Added to session/crypto_state.go or session/session.go
type SessionCryptoState struct {
    // ... existing fields ...

    // EncryptData indicates this session requires encryption.
    EncryptData bool

    // Encryptor is the AEAD encryptor for outgoing messages (uses DecryptionKey).
    // Note: SMB naming is from client perspective. Server encrypts with
    // the server-to-client key (DecryptionKey in SessionCryptoState).
    Encryptor encryption.Encryptor

    // Decryptor is the AEAD decryptor for incoming messages (uses EncryptionKey).
    // Note: Server decrypts with the client-to-server key (EncryptionKey in SessionCryptoState).
    Decryptor encryption.Encryptor
}
```

### Pattern 5: Framing Layer Integration
**What:** Detect 0xFD protocol ID before parsing, decrypt before dispatch
**When to use:** In ReadRequest, before parseSMB2Message

```go
// In ReadRequest, after reading NetBIOS payload:
protocolID := binary.LittleEndian.Uint32(message[0:4])
switch protocolID {
case types.SMB2ProtocolID:     // 0xFE534D42 - normal SMB2
    return parseSMB2Message(message, verifier, true)
case types.SMB1ProtocolID:     // 0xFF534D42 - legacy SMB1
    // existing SMB1 upgrade path
case header.TransformProtocolID: // 0xFD534D42 - encrypted
    if encMiddleware != nil {
        inner, sessionID, err := encMiddleware.DecryptRequest(message)
        if err != nil {
            return nil, nil, nil, err // drop connection on decrypt failure
        }
        return parseSMB2Message(inner, verifier, true)
    }
}
```

### Anti-Patterns to Avoid
- **Encrypting inside handlers:** Encryption MUST happen in the framing layer, not in individual command handlers. Handlers must be encryption-agnostic.
- **Re-deriving keys per message:** Create Encryptor/Decryptor instances once at session setup, store on SessionCryptoState, reuse for all messages.
- **Signing encrypted messages:** Per MS-SMB2, encrypted messages are NOT signed (encryption supersedes signing). Do not sign then encrypt.
- **Reusing nonces:** Each message MUST have a unique nonce from crypto/rand. Never derive nonces from message IDs or counters.
- **Mixing up encryption/decryption keys:** Server uses EncryptionKey (client-to-server, labeled "ServerIn") for DECRYPTION and DecryptionKey (server-to-client, labeled "ServerOut") for ENCRYPTION. The naming is from the client perspective.

## Don't Hand-Roll

| Problem | Don't Build | Use Instead | Why |
|---------|-------------|-------------|-----|
| AES-CCM mode | Custom CBC-MAC + CTR | `pion/dtls/v2/pkg/crypto/ccm` or vendored equivalent | CCM has subtle interactions between CBC-MAC and CTR; incorrect L/M parameters cause silent corruption |
| AES-GCM mode | Custom GCM | `crypto/cipher.NewGCM` (stdlib) | Hardware-accelerated on ARM64, battle-tested |
| Nonce generation | Counter-based or MessageId-based | `crypto/rand.Read` | Spec says implementation-specific but MUST be unique; crypto/rand is the safest choice |
| Transform header AAD | Manual byte slicing | Encode TransformHeader, slice [20:52] | Getting the AAD wrong causes silent decryption failures that are extremely hard to debug |

**Key insight:** The AAD construction is the most error-prone part. The spec says "the SMB2 TRANSFORM_HEADER, excluding the ProtocolId and Signature fields" which is exactly bytes 20-51 (the Nonce through SessionId fields, 32 bytes total). The Microsoft test vectors confirm this. Getting even one byte wrong in the AAD causes AEAD authentication to fail.

## Common Pitfalls

### Pitfall 1: Key Direction Confusion
**What goes wrong:** Server encrypts with EncryptionKey and decrypts with DecryptionKey, producing garbage.
**Why it happens:** The key names use CLIENT perspective. "EncryptionKey" = "ServerIn" = client encrypts TO server. So the SERVER uses EncryptionKey for DECRYPTING incoming messages.
**How to avoid:** Comment clearly:
```
// Server decrypts incoming with EncryptionKey (client-to-server key, "ServerIn")
// Server encrypts outgoing with DecryptionKey (server-to-client key, "ServerOut")
```
**Warning signs:** All encrypted requests fail to decrypt; test vectors don't match.

### Pitfall 2: CCM Nonce Size (11 bytes, not 12)
**What goes wrong:** Using 12-byte nonce for CCM produces incorrect ciphertext.
**Why it happens:** GCM uses 12-byte nonces, CCM uses 11-byte nonces. The transform header Nonce field is 16 bytes, but only the first 11 (CCM) or 12 (GCM) bytes are the actual nonce; the rest is padding.
**How to avoid:** `NewCCM(block, 16, 11)` -- tagsize=16 (SMB3 requirement), noncesize=11 (per MS-SMB2 spec). Extract `nonce[:11]` for CCM, `nonce[:12]` for GCM from the 16-byte Nonce field.
**Warning signs:** Decryption always fails even with correct keys.

### Pitfall 3: Signing vs Encryption Interaction
**What goes wrong:** Server signs an encrypted message, or fails to sign unencrypted responses to encrypted sessions.
**Why it happens:** Unclear about when signing vs encryption applies.
**How to avoid:** Per MS-SMB2 3.3.4.1.1: When encryption is active, messages are encrypted but NOT signed (AEAD provides integrity). The Signature field in the SMB2 header inside the encrypted payload is NOT filled. The transform header's Signature field IS filled (with the AEAD auth tag).
**Warning signs:** Windows client rejects responses; "bad signature" errors on encrypted sessions.

### Pitfall 4: SMB 3.0/3.0.2 Always Uses AES-128-CCM
**What goes wrong:** Server tries to use negotiated cipher for pre-3.1.1 clients.
**Why it happens:** Cipher negotiation only exists in SMB 3.1.1 NEGOTIATE contexts. Pre-3.1.1 clients assume AES-128-CCM.
**How to avoid:** In the encryption path, check dialect: if < 3.1.1, always use AES-128-CCM regardless of any cipher ID stored in connection state.
**Warning signs:** SMB 3.0 clients fail to decrypt server responses.

### Pitfall 5: Flags Field Interpretation
**What goes wrong:** Server uses wrong value in the Flags/EncryptionAlgorithm field.
**Why it happens:** For SMB 3.0/3.0.2 this field is "EncryptionAlgorithm" with value 0x0001 meaning AES-128-CCM. For SMB 3.1.1 this field is "Flags" with value 0x0001 meaning "Encrypted". Same value, different semantics.
**How to avoid:** Always set to 0x0001. On receive, always check for 0x0001.
**Warning signs:** Client disconnects on receiving transform header.

### Pitfall 6: Guest Sessions Cannot Encrypt
**What goes wrong:** Server tries to encrypt guest session traffic, fails because no keys exist.
**Why it happens:** Guest sessions have no session key, so no encryption/decryption keys can be derived.
**How to avoid:** Skip encryption for `session.IsGuest == true` even if `encryption_mode == required`. Document this exemption clearly.
**Warning signs:** Nil pointer panic when accessing Encryptor on guest session.

## Code Examples

### Transform Header Parse/Encode (verified against MS-SMB2 2.2.41)
```go
// Source: [MS-SMB2] Section 2.2.41
func ParseTransformHeader(data []byte) (*TransformHeader, error) {
    if len(data) < TransformHeaderSize {
        return nil, ErrMessageTooShort
    }
    protocolID := binary.LittleEndian.Uint32(data[0:4])
    if protocolID != TransformProtocolID {
        return nil, ErrInvalidProtocolID
    }
    h := &TransformHeader{
        OriginalMessageSize: binary.LittleEndian.Uint32(data[36:40]),
        Flags:               binary.LittleEndian.Uint16(data[42:44]),
        SessionId:           binary.LittleEndian.Uint64(data[44:52]),
    }
    copy(h.Signature[:], data[4:20])
    copy(h.Nonce[:], data[20:36])
    return h, nil
}

func (h *TransformHeader) Encode() []byte {
    buf := make([]byte, TransformHeaderSize)
    binary.LittleEndian.PutUint32(buf[0:4], TransformProtocolID)
    copy(buf[4:20], h.Signature[:])
    copy(buf[20:36], h.Nonce[:])
    binary.LittleEndian.PutUint32(buf[36:40], h.OriginalMessageSize)
    binary.LittleEndian.PutUint16(buf[40:42], 0) // Reserved
    binary.LittleEndian.PutUint16(buf[42:44], h.Flags)
    binary.LittleEndian.PutUint64(buf[44:52], h.SessionId)
    return buf
}

// AAD returns the Additional Authenticated Data for AEAD operations.
// This is the 32 bytes from offset 20 (Nonce through SessionId).
func (h *TransformHeader) AAD() []byte {
    encoded := h.Encode()
    aad := make([]byte, 32)
    copy(aad, encoded[20:52])
    return aad
}
```

### GCM Encryptor (verified against Go stdlib)
```go
// Source: crypto/cipher package, [MS-SMB2] Section 3.1.4.3
func NewGCMEncryptor(key []byte) (*GCMEncryptor, error) {
    block, err := aes.NewCipher(key)
    if err != nil {
        return nil, fmt.Errorf("create AES cipher: %w", err)
    }
    gcm, err := cipher.NewGCM(block)
    if err != nil {
        return nil, fmt.Errorf("create GCM: %w", err)
    }
    return &GCMEncryptor{gcm: gcm}, nil
}

func (e *GCMEncryptor) Encrypt(plaintext, aad []byte) (nonce, ciphertext []byte, err error) {
    nonce = make([]byte, 12) // GCM nonce = 12 bytes
    if _, err = rand.Read(nonce); err != nil {
        return nil, nil, fmt.Errorf("generate nonce: %w", err)
    }
    // Seal appends the auth tag to the ciphertext
    ciphertext = e.gcm.Seal(nil, nonce, plaintext, aad)
    return nonce, ciphertext, nil
}

func (e *GCMEncryptor) Decrypt(nonce, ciphertext, aad []byte) ([]byte, error) {
    return e.gcm.Open(nil, nonce, ciphertext, aad)
}
```

### CCM Encryptor (verified against pion/dtls API)
```go
// Source: pion/dtls/v2/pkg/crypto/ccm, RFC 3610
func NewCCMEncryptor(key []byte) (*CCMEncryptor, error) {
    block, err := aes.NewCipher(key)
    if err != nil {
        return nil, fmt.Errorf("create AES cipher: %w", err)
    }
    // CCM: tagsize=16 (required by SMB3), noncesize=11 (per MS-SMB2)
    ccmCipher, err := ccm.NewCCM(block, 16, 11)
    if err != nil {
        return nil, fmt.Errorf("create CCM: %w", err)
    }
    return &CCMEncryptor{ccm: ccmCipher}, nil
}

func (e *CCMEncryptor) Encrypt(plaintext, aad []byte) (nonce, ciphertext []byte, err error) {
    nonce = make([]byte, 11) // CCM nonce = 11 bytes
    if _, err = rand.Read(nonce); err != nil {
        return nil, nil, fmt.Errorf("generate nonce: %w", err)
    }
    ciphertext = e.ccm.Seal(nil, nonce, plaintext, aad)
    return nonce, ciphertext, nil
}

func (e *CCMEncryptor) Decrypt(nonce, ciphertext, aad []byte) ([]byte, error) {
    return e.ccm.Open(nil, nonce, ciphertext, aad)
}
```

### Test Vector (from Microsoft official documentation)
```go
// Source: https://learn.microsoft.com/en-us/archive/blogs/openspecification/encryption-in-smb-3-0-a-protocol-perspective
// Session: 0x8e40014000011
// SessionKey: B4546771B515F766A86735532DD6C4F0
// EncryptionKey (client/ServerIn): 261B72350558F2E9DCF613070383EDBF
// DecryptionKey (client/ServerOut): 8FE2B57EC34D2DB5B1A9727F526BBDB5
//
// WRITE request encryption:
// CCM Nonce (11 bytes): 66E69A111892584FB5ED52
// AAD (32 bytes): 66E69A111892584FB5ED524A744DA3EE87000000000001001100001400E40800
// Plaintext: FE534D42... (SMB2 WRITE request)
// Signature: 81A286535415445DAE393921E44FA42E
```

### Framing Integration (decrypt path)
```go
// In framing.go ReadRequest, after reading NetBIOS payload:
protocolID := binary.LittleEndian.Uint32(message[0:4])
switch protocolID {
case types.SMB2ProtocolID:
    return parseSMB2Message(message, verifier, true)
case types.SMB1ProtocolID:
    if err := handleSMB1(ctx, message); err != nil {
        return nil, nil, nil, fmt.Errorf("handle SMB1 negotiate: %w", err)
    }
    return readSMB2Message(ctx, conn, maxMsgSize, readTimeout, verifier)
case header.TransformProtocolID:
    if encMiddleware == nil {
        return nil, nil, nil, fmt.Errorf("encrypted message received but encryption not configured")
    }
    decrypted, sessionID, err := encMiddleware.DecryptRequest(message)
    if err != nil {
        return nil, nil, nil, err
    }
    // Parse the decrypted inner SMB2 message
    // Note: signing verification is skipped for encrypted messages per MS-SMB2
    return parseSMB2Message(decrypted, nil, true)
default:
    return nil, nil, nil, fmt.Errorf("unknown protocol ID: 0x%08x", protocolID)
}
```

## State of the Art

| Old Approach | Current Approach | When Changed | Impact |
|--------------|------------------|--------------|--------|
| SMB 3.0 AES-128-CCM only | 3.1.1 cipher negotiation (4 ciphers) | Windows 10 (2015) | Must support both fixed CCM and negotiated ciphers |
| 128-bit encryption only | AES-256-GCM/CCM support | Windows Server 2022 / Windows 11 | 256-bit key derivation already handled by DeriveAllKeys |
| EncryptionAlgorithm field | Flags field (3.1.1) | SMB 3.1.1 spec | Same value 0x0001, different semantic name |

**No deprecated patterns apply.** The transform header format is stable across all SMB 3.x versions.

## Open Questions

1. **CCM library vendoring vs dependency**
   - What we know: `pion/dtls/v2/pkg/crypto/ccm` provides `cipher.AEAD` for CCM. The ccm.go file is ~200 lines.
   - What's unclear: Whether to vendor the single file or add the full pion/dtls dependency.
   - Recommendation: **Vendor the ccm.go file** into `internal/adapter/smb/encryption/ccm.go` with proper attribution. This avoids pulling in 50+ transitive dependencies from pion/dtls for a single file. If the project already has pion/dtls as a dependency, use it directly instead.

2. **sync.Pool for encryption buffers**
   - What we know: Each encrypt/decrypt allocates buffers for ciphertext and plaintext. Large messages (multi-MB) could benefit from pooling.
   - What's unclear: Whether the allocation overhead is significant enough to justify pool complexity.
   - Recommendation: Start without pooling. Add `sync.Pool` later if benchmarks show allocation is a bottleneck. The existing buffer pool (`internal/adapter/pool`) could be extended.

3. **Compound request encryption boundary**
   - What we know: The entire compound chain is encrypted as a single transform header payload. After decryption, the inner SMB2 messages are processed normally.
   - What's unclear: Whether the current compound processing in `ProcessCompoundRequest` handles the decrypted-but-not-individually-encrypted inner messages correctly.
   - Recommendation: Decrypt once in framing layer, pass the decrypted compound payload through existing compound processing unchanged. Mark the ConnInfo as "this request was encrypted" so responses are encrypted too.

## Sources

### Primary (HIGH confidence)
- [MS-SMB2 Section 2.2.41: SMB2 TRANSFORM_HEADER](https://learn.microsoft.com/en-us/openspecs/windows_protocols/ms-smb2/d6ce2327-a4c9-4793-be66-7b5bad2175fa) - Complete header structure with field offsets and nonce format
- [MS-SMB2 Section 3.1.4.3: Encrypting the Message](https://learn.microsoft.com/en-us/openspecs/windows_protocols/ms-smb2/24d74c0c-3de1-40d9-a949-d169ad84361d) - Encryption algorithm, AAD construction, key usage
- [MS-SMB2 Section 3.3.5.2.3: Decrypting the Message](https://learn.microsoft.com/en-us/openspecs/windows_protocols/ms-smb2/d3c03e33-7dc7-4d58-8428-0a1484c5c874) - Decryption algorithm, session lookup, error handling
- [Encryption in SMB 3.0: A protocol perspective](https://learn.microsoft.com/en-us/archive/blogs/openspecification/encryption-in-smb-3-0-a-protocol-perspective) - Official Microsoft test vectors with hex dumps
- [Go crypto/cipher package](https://pkg.go.dev/crypto/cipher) - GCM AEAD interface documentation
- [pion/dtls CCM package](https://pkg.go.dev/github.com/pion/dtls/v2/pkg/crypto/ccm) - CCM cipher.AEAD implementation API

### Secondary (MEDIUM confidence)
- [SMB 3.1.1 Encryption in Windows 10](https://learn.microsoft.com/en-us/archive/blogs/openspecification/smb-3-1-1-encryption-in-windows-10) - Cipher negotiation details for 3.1.1
- [SMB Security Enhancements](https://learn.microsoft.com/en-us/windows-server/storage/file-server/smb-security) - AES-256 cipher support overview
- [golang/go #27484](https://github.com/golang/go/issues/27484) - Go stdlib CCM status (not planned for stdlib)

### Tertiary (LOW confidence)
- None.

## Metadata

**Confidence breakdown:**
- Standard stack: HIGH - Go stdlib GCM is authoritative; pion CCM is battle-tested in production WebRTC
- Architecture: HIGH - Pattern directly mirrors established signing.Signer pattern already in codebase
- Transform header: HIGH - Verified against Microsoft official spec and test vectors with hex dumps
- AAD construction: HIGH - Confirmed by both MS spec ("excluding ProtocolId and Signature fields") and test vector hex dumps
- Pitfalls: HIGH - Key direction confirmed by test vectors; nonce sizes confirmed by spec

**Research date:** 2026-03-02
**Valid until:** 2026-04-02 (stable protocol, no expected changes)
