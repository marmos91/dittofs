# Phase 35: Encryption and Transform Header - Context

**Gathered:** 2026-03-02
**Status:** Ready for planning

<domain>
## Phase Boundary

Implement SMB3 traffic encryption using AES-CCM/GCM with transform header (0xFD) framing. Encryption is enforced per-session or per-share via adapter and share configuration. All encryption/decryption happens transparently in the middleware layer — command handlers remain encryption-agnostic.

</domain>

<decisions>
## Implementation Decisions

### Enforcement Policy
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
- **Session reauthentication/binding**: basic support in this phase — re-derive encryption/decryption keys from new session key using new connection's preauth hash; old keys are destroyed

### Transform Header Integration
- Decrypt step happens in the **framing layer** (`framing.go`), before `parseSMB2Message` — main logic is encryption-agnostic
- Encrypt step happens in `WriteNetBIOSFrame` — extend to accept encryption context; if encryption is active, wrap payload in transform header before writing
- **Separate `EncryptionMiddleware` interface** (not combined with SigningVerifier): `DecryptRequest` and `EncryptResponse` methods
- Session lookup for decryption: inject a `func(sessionID uint64) ([]byte, error)` closure that returns the decryption key — middleware doesn't import session manager directly
- Compound requests: decrypt the entire transform payload once, then process all compound commands inside the decrypted inner message normally
- **Nonce generation**: use `crypto/rand` for each encrypted message (fresh random nonce per message)
- Transform header parsing/encoding lives in the existing **`header/` package** alongside SMB2Header
- Decrypted request buffers stay alive for handler lifetime (handlers reference body slices directly); pool return after handler completes
- All responses on encrypted sessions are encrypted, including error responses (no information leakage)

### Cipher Preference and Fallback
- Default preference order: **AES-256-GCM > AES-256-CCM > AES-128-GCM > AES-128-CCM** (256-bit prioritized)
- **`allowed_ciphers`** config on adapter: list of allowed ciphers; order defines server preference
  - Default when unconfigured: all four ciphers in the priority order above
  - Empty list or no cipher match: negotiate without encryption capability; if `required` mode, session setup fails later
- SMB 3.0/3.0.2 clients: always use AES-128-CCM (no cipher negotiation for pre-3.1.1); this is spec behavior
- Log negotiated cipher at **INFO level** during session setup
- Include `BenchmarkEncryptGCM` and `BenchmarkEncryptCCM` tests to verify performance

### Error Handling
- **Decryption failure** (AEAD auth tag mismatch, tampered data): drop connection silently, log at WARN with client IP and session ID
- **Unknown session ID in transform header**: drop message silently, log at WARN
- **Unencrypted request on encrypted session** (in `required` mode): return STATUS_ACCESS_DENIED, keep connection open
- **Consecutive failure threshold**: close connection after 5 consecutive decryption failures; counter resets on successful decrypt
- Error responses on encrypted sessions are always encrypted

### Code Structure and Design
- **`Encryptor` interface** mirroring the `signing.Signer` pattern: `Encrypt(plaintext, aad) (ciphertext, error)` and `Decrypt(ciphertext, aad) (plaintext, error)`
  - Implementations: `GCMEncryptor`, `CCMEncryptor` in new `internal/adapter/smb/encryption/` package
- **Package structure**:
  - `header/` — TransformHeader struct, Parse/Encode (wire format)
  - `encryption/` — Encryptor interface, GCM/CCM implementations, middleware
  - `session/crypto_state.go` — already has EncryptionKey/DecryptionKey fields (now activated)
- Test coverage: unit tests (encrypt/decrypt round-trip, bad key, tampered data, bad AAD) + MSVP smbtorture encryption suite
- Update CONFIGURATION.md with new `encryption_mode` and `allowed_ciphers` adapter config options

### Claude's Discretion
- Exact transform header field layout and endianness handling
- Internal buffer management for encrypt/decrypt operations
- CCM nonce size handling (11 vs 12 bytes per spec)
- Exact AAD construction for transform header fields
- Config validation error messages
- Whether to use sync.Pool for encryption buffers

</decisions>

<specifics>
## Specific Ideas

- "Main logic should be encryption agnostic. Encryption should happen in middlewares" — clean separation between crypto and business logic
- Adapter-level encryption config in control plane matches existing adapter config patterns
- No Prometheus metrics in this phase (metrics infrastructure doesn't exist yet)

</specifics>

<code_context>
## Existing Code Insights

### Reusable Assets
- `session/crypto_state.go`: `SessionCryptoState` already has `EncryptionKey`/`DecryptionKey` fields, populated by `DeriveAllKeys()` — just needs activation
- `kdf/kdf.go`: SP800-108 KDF already derives encryption/decryption keys with correct labels for 3.0/3.0.2 and 3.1.1
- `signing/signer.go`: `Signer` interface pattern to mirror for `Encryptor`
- `types/constants.go`: Cipher constants (`CipherAES128CCM/GCM`, `CipherAES256CCM/GCM`), `SessionFlagEncryptData` already defined
- `header/` package: existing Parse/Encode pattern for SMB2Header to extend with TransformHeader

### Established Patterns
- **Interface + multiple implementations**: `signing.Signer` with HMAC/CMAC/GMAC — same pattern for Encryptor with GCM/CCM
- **Middleware injection**: `SigningVerifier` interface injected into `ReadRequest` — same pattern for `EncryptionMiddleware`
- **Session key lookup via closure**: decouples framing from session management
- **Negotiate context handling**: cipher selection already works in `negotiate.go:459-462`

### Integration Points
- `framing.go:ReadRequest()` — add 0xFD protocol ID detection before parseSMB2Message
- `framing.go:WriteNetBIOSFrame()` — extend to accept encryption context parameter
- `v2/handlers/negotiate.go` — update cipher preference order to 256-bit first, filter by allowed_ciphers config
- `v2/handlers/session_setup.go` — set Session.EncryptData flag based on adapter encryption_mode
- `v2/handlers/tree_connect.go` — set SMB2_SHAREFLAG_ENCRYPT_DATA based on share.EncryptData
- `session/session.go` — add encryption-related methods (ShouldEncrypt, etc.)
- Control plane: adapter config model, share model (encrypt_data field)
- `pkg/adapter/smb/` — adapter config struct for encryption_mode and allowed_ciphers

</code_context>

<deferred>
## Deferred Ideas

- **NFS encryption (krb5p)**: user suggested shared encryption layer across protocols. NFS uses RPCSEC_GSS for privacy which is architecturally different from SMB3 transform headers. Worth considering when NFS krb5p is implemented, but not in scope for Phase 35.
- **Prometheus metrics for encryption**: encrypted/unencrypted request counters, cipher distribution, decrypt latency. Blocked on metrics infrastructure (not yet built).
- **Full session reauthentication/binding**: basic crypto plumbing in this phase; full durable handle reconnect is Phase 38.

</deferred>

---

*Phase: 35-encryption-and-transform-header*
*Context gathered: 2026-03-02*
