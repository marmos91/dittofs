# Technology Stack: SMB3 Protocol Upgrade (v3.8)

**Project:** DittoFS SMB3 Protocol Upgrade
**Researched:** 2026-02-28
**Overall Confidence:** HIGH

## Executive Summary

The SMB3 upgrade requires five categories of cryptographic additions: (1) AES-CCM authenticated encryption, (2) AES-CMAC signing, (3) AES-GMAC signing, (4) SHA-512 preauth integrity hashing, and (5) SP800-108 counter-mode key derivation. Go's standard library covers GCM encryption, SHA-512 hashing, and HMAC-SHA256 (for KDF). Two additions are needed: a self-contained CCM implementation (AES-CCM is not in Go stdlib) and one external library for AES-CMAC. The SPNEGO/Kerberos layer already exists via `jcmturner/gokrb5/v8` and the existing `pkg/auth/kerberos/` provider. No new authentication libraries are needed.

**Net impact: 1 new external dependency (`aead/cmac`), 2 new stdlib package usages, 1 internal crypto package (~200 lines CCM + ~30 lines KDF).**

## Recommended Stack

### Encryption (AES-CCM and AES-GCM)

| Technology | Version | Purpose | Why |
|------------|---------|---------|-----|
| `crypto/aes` + `crypto/cipher` (stdlib) | Go 1.25 | AES-128/256-GCM authenticated encryption | Go stdlib has hardware-accelerated GCM via `cipher.NewGCM()`. Produces `cipher.AEAD` interface. Zero dependencies. HIGH confidence. |
| Custom CCM implementation (internal) | N/A | AES-128/256-CCM authenticated encryption | **Implement in `internal/adapter/smb/crypto/ccm/`** rather than importing a third-party library. See rationale below. |

**CCM Implementation Rationale:**

Three CCM options were evaluated:

| Library | License | Interface | Status | Verdict |
|---------|---------|-----------|--------|---------|
| `pion/dtls/v2/pkg/crypto/ccm` | MIT | `cipher.AEAD` | Active (pion/dtls is well-maintained) | Pulls in entire DTLS module as transitive dep |
| `pschlump/aesccm` | MIT | `cipher.AEAD` | Last commit 2019, minimal maintenance | Too stale for security-critical code |
| `hirochachacha/go-smb2` internal CCM | BSD-2 | `cipher.AEAD` | Used in production go-smb2 client | Internal package, cannot import |

**Recommendation: Write a self-contained CCM implementation** in `internal/adapter/smb/crypto/ccm/`. This is the correct approach because:

1. CCM is a well-specified algorithm (NIST SP 800-38C / RFC 3610) -- approximately 200 lines of Go
2. The `hirochachacha/go-smb2` and `pion/dtls` implementations are excellent reference code (both MIT/BSD licensed, can be studied for correctness verification)
3. Avoids pulling `pion/dtls` (a large DTLS library) as a dependency for one 200-line file
4. The existing codebase already implements complex crypto from scratch (NTLMv2, HMAC-SHA256 signing, XDR codec) -- team has proven crypto implementation capability
5. SMB-specific CCM needs are narrow and well-defined (see parameters below)
6. Must implement `cipher.AEAD` interface for consistency with GCM path

**CCM Parameters for SMB3 (per MS-SMB2 and NIST SP 800-38C):**
- AES-128-CCM: 16-byte key, 11-byte nonce, 16-byte tag (M=16, L=4)
- AES-256-CCM: 32-byte key, 11-byte nonce, 16-byte tag (M=16, L=4)
- AES-128-GCM: 16-byte key, 12-byte nonce, 16-byte tag (standard `cipher.NewGCM`)
- AES-256-GCM: 32-byte key, 12-byte nonce, 16-byte tag (standard `cipher.NewGCM`)

### Signing (HMAC-SHA256, AES-CMAC, AES-GMAC)

| Technology | Version | Purpose | Why |
|------------|---------|---------|-----|
| `crypto/hmac` + `crypto/sha256` (stdlib) | Go 1.25 | HMAC-SHA256 signing (SMB 2.x, signing algorithm ID 0x0000) | Already used in existing `internal/adapter/smb/signing/`. No change needed. |
| `github.com/aead/cmac` | latest | AES-CMAC signing (SMB 3.0+, signing algorithm ID 0x0001) | Implements `hash.Hash` interface, wraps `cipher.Block`. Clean API: `cmac.New(block)` returns `hash.Hash`. Used for SMB 3.0/3.0.2 signing. MEDIUM confidence (well-tested, RFC 4493 compliant, zero transitive deps). |
| `crypto/aes` + `crypto/cipher` (stdlib) | Go 1.25 | AES-GMAC signing (SMB 3.1.1, signing algorithm ID 0x0002) | GMAC is GCM with empty plaintext -- call `gcm.Seal(nil, nonce, nil, aad)` where AAD is the message to sign. The 16-byte GCM tag IS the signature. No additional library needed. HIGH confidence. |

**AES-CMAC Library Choice:**

| Library | API | Maintenance | Verdict |
|---------|-----|-------------|---------|
| `github.com/aead/cmac` | `hash.Hash` from `cipher.Block` | Stable, used by multiple projects | **Use this** -- clean API, correct for SMB3 use case |
| `github.com/jacobsa/crypto/cmac` | Similar `hash.Hash` API | Also stable | Good alternative, slightly less popular |
| `github.com/enceve/crypto/cmac` | Similar | Less documentation | Skip |
| Go stdlib `crypto/internal/fips140` | Internal only, not importable | N/A | Cannot use |

**Recommendation: Use `github.com/aead/cmac`** because:
1. Implements standard `hash.Hash` interface -- idiomatic Go
2. Takes any `cipher.Block` -- works with both AES-128 and AES-256
3. Well-tested, RFC 4493 compliant
4. Single-purpose package with no transitive dependencies
5. CMAC computation: `cmac.New(aesBlock)` then `h.Write(message)` then `h.Sum(nil)[:16]`

**AES-GMAC Signing Implementation Detail:**

AES-GMAC is NOT a separate algorithm but AES-GCM applied with empty plaintext. The Linux kernel CIFS team confirmed this in their GMAC signing patches. The implementation is:

```go
// GMAC signing: GCM with empty plaintext, message as AAD
func SignGMAC(key, nonce, message []byte) []byte {
    block, _ := aes.NewCipher(key)
    gcm, _ := cipher.NewGCMWithNonceSize(block, len(nonce))
    // Seal with empty plaintext, message as additional data
    // Output is just the 16-byte authentication tag
    return gcm.Seal(nil, nonce, nil, message)
}
```

The nonce for GMAC signing is generated per-message (not the transform header nonce -- signing nonces and encryption nonces are independent).

### Preauth Integrity Hashing

| Technology | Version | Purpose | Why |
|------------|---------|---------|-----|
| `crypto/sha512` (stdlib) | Go 1.25 | SHA-512 preauth integrity hash chain (SMB 3.1.1) | Directly in Go standard library. Hardware-accelerated on amd64/arm64 (64-bit architectures perform SHA-512 faster than SHA-256). Hash algorithm ID 0x0001 in `SMB2_PREAUTH_INTEGRITY_CAPABILITIES`. HIGH confidence. |

**Preauth integrity hash chain computation (MS-SMB2 section 3.2.5.2/3.3.5.4):**
```
PreauthIntegrityHash[0] = zeros (64 bytes)
PreauthIntegrityHash[x] = SHA-512(PreauthIntegrityHash[x-1] || Message[x])
```

Where messages are the raw bytes of:
1. NEGOTIATE request (client)
2. NEGOTIATE response (server)
3. Each SESSION_SETUP request (client)
4. Each SESSION_SETUP response (server)

The final 64-byte hash value is used as the Context parameter for SMB 3.1.1 key derivation (replacing the static context strings used in SMB 3.0/3.0.2).

### Key Derivation (SP800-108 Counter Mode)

| Technology | Version | Purpose | Why |
|------------|---------|---------|-----|
| `crypto/hmac` + `crypto/sha256` (stdlib) | Go 1.25 | SP800-108 KDF in Counter Mode with HMAC-SHA256 PRF | Implement as ~30-line function in `internal/adapter/smb/crypto/kdf/kdf.go`. The KDF is a single HMAC computation. No external library needed. HIGH confidence. |

**KDF Implementation (from MS-SMB2 specification):**

```go
// SMB3KDF implements SP800-108 Counter Mode KDF with HMAC-SHA256.
// Parameters: r=32, PRF=HMAC-SHA256, L=keyLenBits (128 or 256).
func SMB3KDF(ki []byte, label, context []byte, keyLenBits int) []byte {
    h := hmac.New(sha256.New, ki)
    // Counter i=1 (4 bytes big-endian, r=32)
    h.Write([]byte{0x00, 0x00, 0x00, 0x01})
    // Label
    h.Write(label)
    // Separator (0x00)
    h.Write([]byte{0x00})
    // Context
    h.Write(context)
    // L (key length in bits, 4 bytes big-endian)
    l := make([]byte, 4)
    binary.BigEndian.PutUint32(l, uint32(keyLenBits))
    h.Write(l)
    // Truncate HMAC-SHA256 output (32 bytes) to key length
    return h.Sum(nil)[:keyLenBits/8]
}
```

**Key derivation labels per dialect (verified from MS-SMB2 and hirochachacha/go-smb2):**

| Key | SMB 3.0/3.0.2 Label | SMB 3.0/3.0.2 Context | SMB 3.1.1 Label | SMB 3.1.1 Context |
|-----|---------------------|----------------------|----------------|------------------|
| SigningKey | `"SMB2AESCMAC\0"` | `"SmbSign\0"` | `"SMBSigningKey\0"` | PreauthIntegrityHashValue (64 bytes) |
| EncryptionKey | `"SMB2AESCCM\0"` | `"ServerIn \0"` | `"SMBC2SCipherKey\0"` | PreauthIntegrityHashValue (64 bytes) |
| DecryptionKey | `"SMB2AESCCM\0"` | `"ServerOut\0"` | `"SMBS2CCipherKey\0"` | PreauthIntegrityHashValue (64 bytes) |

**L value depends on negotiated cipher (Connection.CipherId):**
- AES-128-CCM (0x0001) or AES-128-GCM (0x0002): L=128, output 16 bytes
- AES-256-CCM (0x0003) or AES-256-GCM (0x0004): L=256, output 32 bytes

**Note:** The SMB 3.0/3.0.2 labels use `"ServerIn \0"` and `"ServerOut\0"` (with trailing space in "ServerIn ") -- these are from the server's perspective. "ServerIn" = traffic flowing to the server (client-to-server encryption key). "ServerOut" = traffic flowing from the server (server-to-client encryption key, which is the decryption key on the server side).

### SPNEGO / Kerberos Authentication

| Technology | Version | Purpose | Why |
|------------|---------|---------|-----|
| `github.com/jcmturner/gokrb5/v8` | v8.4.4 (already in go.mod) | Kerberos AP-REQ/AP-REP, SPNEGO token handling | Already used by `pkg/auth/kerberos/` provider and `internal/adapter/nfs/rpc/gss/`. **No new dependency needed.** |
| `github.com/jcmturner/gofork` | v1.7.6 (already in go.mod) | ASN.1 encoding for SPNEGO OIDs | Already used by `internal/adapter/smb/auth/spnego.go`. **No new dependency needed.** |
| Existing `pkg/auth/kerberos/Provider` | N/A | Keytab management, service principal, hot-reload | Already implemented with `AuthProvider` interface. Wire into SMB authenticator. |
| Existing `internal/adapter/smb/auth/` | N/A | SPNEGO parsing, NTLM challenge-response | Already handles SPNEGO wrapping/unwrapping, NTLM auth. Add Kerberos branch. |

**What already exists for SPNEGO/Kerberos (DO NOT re-implement):**
- `internal/adapter/smb/auth/spnego.go`: Full SPNEGO NegTokenInit/NegTokenResp parsing and building (Parse, BuildResponse, BuildAcceptComplete, BuildReject)
- `internal/adapter/smb/auth/authenticator.go`: `SMBAuthenticator` with SPNEGO dispatch (currently routes NTLM, stubs Kerberos with "not yet supported")
- `pkg/auth/kerberos/provider.go`: Keytab loading, krb5.conf parsing, hot-reload, `CanHandle()`/`Authenticate()`
- `internal/adapter/nfs/rpc/gss/verifier.go`: Full GSS-API Kerberos verification using `service.ValidateAPREQ()` and session key extraction
- `internal/adapter/nfs/rpc/gss/context.go`: GSS context lifecycle management

**What needs to be added for SMB3 Kerberos:**
1. Wire `kerberos.Provider` into `SMBAuthenticator` -- when `parsed.HasKerberos()` is true, call the Kerberos provider instead of returning "not yet supported"
2. Use `service.ValidateAPREQ()` from gokrb5 to validate the Kerberos AP-REQ (same approach as NFS GSS verifier)
3. Extract session key from the validated Kerberos ticket for SMB3 key derivation
4. Build SPNEGO `NegTokenResp` with Kerberos `AP-REP` token using `BuildAcceptComplete(OIDKerberosV5, apRepBytes)`
5. The session key from Kerberos becomes the input to `SMB3KDF()` for deriving signing/encryption keys

### Transform Header / Encryption Framing

| Technology | Version | Purpose | Why |
|------------|---------|---------|-----|
| Go stdlib `encoding/binary` | Go 1.25 | SMB2_TRANSFORM_HEADER parsing/building (52-byte header) | Already the standard approach in DittoFS for all wire format handling. |

**Transform header structure (52 bytes, MS-SMB2 2.2.41):**
- Bytes 0-3: ProtocolId (0xFD 'S' 'M' 'B' = 0x424D53FD)
- Bytes 4-19: Signature (16-byte AES-CCM/GCM authentication tag)
- Bytes 20-35: Nonce (16 bytes total):
  - CCM: 11-byte nonce + 5 bytes zero-padding
  - GCM: 12-byte nonce + 4 bytes zero-padding
- Bytes 36-39: OriginalMessageSize (uint32)
- Bytes 40-41: Reserved (0)
- Bytes 42-43: Flags (0x0001 = encrypted)
- Bytes 44-51: SessionId (uint64)

**Encryption process (server side):**
1. Build complete SMB2 response (header + body)
2. Generate unique nonce (must never reuse within a session)
3. Build transform header with nonce, message size, session ID
4. AAD = transform header bytes 20-51 (nonce through session ID, 32 bytes)
5. Encrypt: `ciphertext = aead.Seal(nil, nonce, plaintext, aad)`
6. Signature = first 16 bytes of AEAD output tag
7. Send: transform header (with signature) + ciphertext

### Testing Libraries

| Technology | Version | Purpose | Why |
|------------|---------|---------|-----|
| `github.com/hirochachacha/go-smb2` | latest | Go integration tests (SMB3 client) | Referenced in PROJECT.md for v3.8 testing. BSD-2 licensed. Supports SMB 3.0/3.0.2/3.1.1 with encryption. Use as test client to validate encryption/signing interop. |
| `github.com/stretchr/testify` | v1.11.1 (already in go.mod) | Test assertions | Already used throughout codebase. |
| smbtorture | system (Docker) | SMB3 protocol torture tests | Already validated in v3.6. Add SMB3-specific tests: `smb2.lease`, `smb2.durable-v2-open`, `smb2.replay`, `smb2.session-*`. |
| MS WindowsProtocolTestSuites | latest (Docker) | SMB3 BVT + feature tests | Already validated in v3.6. Run SMB3-specific tests for encryption, signing, leasing, durable handles. |

## Alternatives Considered

| Category | Recommended | Alternative | Why Not Alternative |
|----------|-------------|-------------|---------------------|
| AES-CCM | Internal implementation | `pion/dtls/v2/pkg/crypto/ccm` | Pulls entire DTLS module (~50+ transitive deps) for 200 lines of CCM code |
| AES-CCM | Internal implementation | `pschlump/aesccm` | Last maintained 2019, insufficient for security-critical use |
| AES-CMAC | `github.com/aead/cmac` | `github.com/jacobsa/crypto/cmac` | Both are fine; aead/cmac is slightly more popular and has cleaner API |
| AES-CMAC | `github.com/aead/cmac` | Internal implementation | CMAC is ~80 lines but has subtle edge cases (subkey generation, padding). Library is well-tested and zero-dep. |
| KDF | Internal (30 lines) | `github.com/canonical/go-sp800.108-kdf` | The KDF is a single HMAC call. Library adds dependency for trivial code. |
| KDF | Internal (30 lines) | `github.com/canonical/go-kbkdf` | Same rationale -- too simple to justify a dependency |
| SPNEGO | Existing gokrb5/v8 | New SPNEGO library | gokrb5 already in go.mod, SPNEGO parsing already works in auth/spnego.go |
| GMAC | Stdlib GCM with empty plaintext | External GMAC library | No GMAC library exists; GMAC IS GCM with empty plaintext per RFC 4543 |

## What NOT to Add (Already Exists)

These are already in the codebase and should NOT be re-implemented or replaced:

| Component | Location | Status |
|-----------|----------|--------|
| NTLM authentication | `internal/adapter/smb/auth/ntlm.go` | Full NTLMv2 with session key derivation |
| HMAC-SHA256 signing | `internal/adapter/smb/signing/signing.go` | Full implementation for SMB 2.x |
| SPNEGO parsing | `internal/adapter/smb/auth/spnego.go` | Full NegTokenInit/NegTokenResp support |
| Kerberos keytab/config | `pkg/auth/kerberos/provider.go` | Provider with hot-reload |
| GSS-API verification | `internal/adapter/nfs/rpc/gss/verifier.go` | AP-REQ validation logic (reuse patterns for SMB) |
| SMB dialect constants | `internal/adapter/smb/types/constants.go` | Already has 3.0, 3.0.2, 3.1.1 dialect values |
| SMB capabilities | `internal/adapter/smb/types/constants.go` | Already has CapEncryption, CapDirectoryLeasing, etc. |
| Session management | `internal/adapter/smb/session/session.go` | SessionManager with signing state |
| NT Security Descriptors | `internal/adapter/smb/v2/handlers/security.go` | POSIX-to-DACL synthesis, SID mapping |

## New Package Layout

```
internal/adapter/smb/
  crypto/                          # NEW: SMB3 cryptographic operations
    ccm/                           # NEW: AES-CCM AEAD implementation
      ccm.go                       # cipher.AEAD implementation (~200 lines)
      ccm_test.go                  # NIST SP800-38C test vectors
    kdf/                           # NEW: SP800-108 key derivation
      kdf.go                       # SMB3KDF function (~30 lines)
      kdf_test.go                  # MS-SMB2 test vectors
    transform/                     # NEW: SMB2_TRANSFORM_HEADER encrypt/decrypt
      transform.go                 # Encrypt/decrypt message framing
      transform_test.go
  signing/
    signing.go                     # EXISTING: HMAC-SHA256 (keep as-is)
    smb3_signing.go                # NEW: AES-CMAC and AES-GMAC signing
    smb3_signing_test.go           # NEW: Signing algorithm tests
  negotiate/                       # NEW: SMB 3.1.1 negotiate context handling
    contexts.go                    # Parse/build negotiate contexts
    preauth.go                     # SHA-512 preauth integrity hash chain
    preauth_test.go
  session/
    session.go                     # EXISTING: Extend with encryption/decryption key fields
```

## Integration Points with Existing SMB Adapter

### Session Key Flow (existing -> new)

```
1. NEGOTIATE handler (existing internal/adapter/smb/v2/handlers/negotiate.go)
   - EXISTING: Selects dialect 2.0.2/2.1
   - NEW: Select dialect 3.0/3.0.2/3.1.1, parse negotiate contexts
   - NEW: Start preauth integrity hash chain (SHA-512)
   - NEW: Return negotiate contexts in response

2. SESSION_SETUP handler (existing internal/adapter/smb/v2/handlers/session_setup.go)
   - EXISTING: NTLM auth, derives session key, enables HMAC-SHA256 signing
   - NEW: Kerberos auth via Provider, derives session key from ticket
   - NEW: Continue preauth integrity hash chain
   - NEW: Derive SMB3 keys via SMB3KDF (signing, encryption, decryption)
   - NEW: Store per-session cipher type and keys

3. Message dispatch (existing internal/adapter/smb/framing.go)
   - EXISTING: Read NetBIOS frame, parse SMB2 header, dispatch
   - NEW: Detect transform header (0xFD534D42), decrypt before dispatch
   - NEW: Encrypt response before sending if session requires encryption

4. Signing (existing internal/adapter/smb/signing/)
   - EXISTING: HMAC-SHA256 via SigningKey.Sign/Verify
   - NEW: AES-CMAC for SMB 3.0/3.0.2 sessions
   - NEW: AES-GMAC for SMB 3.1.1 sessions
   - NEW: Select algorithm based on negotiated signing capability
```

### Config Extension (existing pkg/adapter/smb/config.go)

```go
// NEW fields to add to existing Config struct:
type Config struct {
    // ... existing fields ...

    // Encryption configures SMB3 encryption behavior.
    Encryption EncryptionConfig `mapstructure:"encryption"`
}

type EncryptionConfig struct {
    // Enabled controls whether encryption capability is advertised.
    Enabled bool `mapstructure:"enabled"`

    // Required forces encryption for all sessions.
    Required bool `mapstructure:"required"`

    // PreferredCiphers lists ciphers in preference order.
    // Default: [AES-128-GCM, AES-128-CCM, AES-256-GCM, AES-256-CCM]
    PreferredCiphers []string `mapstructure:"preferred_ciphers"`
}
```

## Installation

```bash
# Only new dependency:
go get github.com/aead/cmac

# Everything else is Go stdlib or already in go.mod:
# - crypto/aes, crypto/cipher, crypto/sha512 (stdlib)
# - crypto/hmac, crypto/sha256 (stdlib, already used)
# - encoding/binary (stdlib, already used)
# - github.com/jcmturner/gokrb5/v8 (already in go.mod)
# - github.com/jcmturner/gofork (already in go.mod)
```

## Dependency Impact Summary

| Category | New External Dependencies | New Stdlib Usage | Reuse Existing |
|----------|--------------------------|------------------|----------------|
| Encryption | 0 | `crypto/aes` (256-bit keys), `cipher.NewGCM` | -- |
| CCM | 0 (internal impl) | `crypto/aes`, `crypto/cipher` | -- |
| CMAC Signing | 1 (`aead/cmac`) | -- | HMAC-SHA256 signing |
| GMAC Signing | 0 | -- | `crypto/cipher` GCM |
| Preauth | 0 | `crypto/sha512` | -- |
| KDF | 0 (internal impl) | -- | `crypto/hmac` + `crypto/sha256` |
| Auth | 0 | -- | gokrb5/v8, kerberos.Provider, SPNEGO parser |
| **Total** | **1 new dependency** | **2 new stdlib packages** | **6+ existing components** |

## Negotiate Context Type IDs (MS-SMB2 reference)

| Context Type | ID | Purpose |
|--------------|-----|---------|
| `SMB2_PREAUTH_INTEGRITY_CAPABILITIES` | 0x0001 | Hash algorithm negotiation (SHA-512 = 0x0001) |
| `SMB2_ENCRYPTION_CAPABILITIES` | 0x0002 | Encryption cipher negotiation |
| `SMB2_SIGNING_CAPABILITIES` | 0x0008 | Signing algorithm negotiation |

**Encryption Cipher IDs:**
| ID | Cipher | Key Size | Nonce Size |
|----|--------|----------|------------|
| 0x0001 | AES-128-CCM | 16 bytes | 11 bytes |
| 0x0002 | AES-128-GCM | 16 bytes | 12 bytes |
| 0x0003 | AES-256-CCM | 32 bytes | 11 bytes |
| 0x0004 | AES-256-GCM | 32 bytes | 12 bytes |

**Signing Algorithm IDs:**
| ID | Algorithm | Library |
|----|-----------|---------|
| 0x0000 | HMAC-SHA256 | `crypto/hmac` + `crypto/sha256` (existing) |
| 0x0001 | AES-CMAC | `github.com/aead/cmac` (new dep) |
| 0x0002 | AES-GMAC | `crypto/cipher` GCM with empty plaintext (stdlib) |

## Sources

**HIGH confidence (official documentation):**
- [MS-SMB2 Generating Cryptographic Keys](https://learn.microsoft.com/en-us/openspecs/windows_protocols/ms-smb2/da4e579e-02ce-4e27-bbce-3fc816a3ff92) -- SP800-108 KDF specification, L values for different ciphers
- [MS-SMB2 SMB2_ENCRYPTION_CAPABILITIES](https://learn.microsoft.com/en-us/openspecs/windows_protocols/ms-smb2/16693be7-2b27-4d3b-804b-f605bde5bcdd) -- Cipher ID definitions (AES-128/256-CCM/GCM)
- [MS-SMB2 SMB2_SIGNING_CAPABILITIES](https://learn.microsoft.com/en-us/openspecs/windows_protocols/ms-smb2/cb9b5d66-b6be-4d18-aa66-8784a871cc10) -- Signing algorithm ID definitions (HMAC-SHA256, AES-CMAC, AES-GMAC)
- [MS-SMB2 SMB2_PREAUTH_INTEGRITY_CAPABILITIES](https://learn.microsoft.com/en-us/openspecs/windows_protocols/ms-smb2/5a07bd66-4734-4af8-abcf-5a44ff7ee0e5) -- Preauth hash structure (SHA-512)
- [MS-SMB2 SMB2_TRANSFORM_HEADER](https://learn.microsoft.com/en-us/openspecs/windows_protocols/ms-smb2/d6ce2327-a4c9-4793-be66-7b5bad2175fa) -- Encryption framing, nonce formats
- [MS-SMB2 Encrypting the Message](https://learn.microsoft.com/en-us/openspecs/windows_protocols/ms-smb2/24d74c0c-3de1-40d9-a949-d169ad84361d) -- Encryption procedure
- [SMB 3.1.1 Pre-authentication integrity in Windows 10](https://learn.microsoft.com/en-us/archive/blogs/openspecification/smb-3-1-1-pre-authentication-integrity-in-windows-10) -- SHA-512 hash chain computation
- [SMB 3.1.1 Encryption in Windows 10](https://learn.microsoft.com/en-us/archive/blogs/openspecification/smb-3-1-1-encryption-in-windows-10) -- AES-128-GCM addition rationale
- [SMB 2 and SMB 3 security: anatomy of signing and crypto keys](https://learn.microsoft.com/en-us/archive/blogs/openspecification/smb-2-and-smb-3-security-in-windows-10-the-anatomy-of-signing-and-cryptographic-keys) -- Key derivation labels for all dialects
- [Go crypto/cipher package](https://pkg.go.dev/crypto/cipher) -- AEAD interface, GCM support, NewGCMWithNonceSize
- [Go crypto/sha512 package](https://pkg.go.dev/crypto/sha512) -- SHA-512 implementation, hardware acceleration on amd64/arm64

**MEDIUM confidence (verified library documentation and reference implementations):**
- [aead/cmac package](https://pkg.go.dev/github.com/aead/cmac) -- CMAC hash.Hash API, RFC 4493 compliant
- [hirochachacha/go-smb2 CCM source](https://github.com/hirochachacha/go-smb2/blob/master/internal/crypto/ccm/ccm.go) -- Reference CCM implementation (BSD-2), ~200 lines
- [hirochachacha/go-smb2 KDF source](https://github.com/hirochachacha/go-smb2/blob/master/kdf.go) -- Reference KDF implementation (BSD-2), confirms label strings
- [hirochachacha/go-smb2 session.go](https://github.com/hirochachacha/go-smb2/blob/master/session.go) -- Reference key derivation flow (signing, encryption, decryption keys)
- [pion/dtls CCM package](https://pkg.go.dev/github.com/pion/dtls/v2/pkg/crypto/ccm) -- Alternative CCM implementation (MIT), confirms cipher.AEAD interface
- [Linux kernel CIFS AES-GMAC signing patches](https://lore.kernel.org/all/20220831134444.26252-1-ematsumiya@suse.de/T/) -- Confirms GMAC = GCM with empty plaintext, NOT RFC 4543 (which is IPsec-specific)

**LOW confidence (informational only):**
- [Go AES-CCM proposal (not accepted)](https://github.com/golang/go/issues/27484) -- Confirms CCM is not in Go stdlib
- [SP800-108 KDF Go proposal](https://github.com/golang/go/issues/50136) -- Confirms KDF not in golang.org/x/crypto
