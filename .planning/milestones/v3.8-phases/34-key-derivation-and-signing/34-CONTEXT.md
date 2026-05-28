# Phase 34: Key Derivation and Signing - Context

**Gathered:** 2026-03-01
**Status:** Ready for planning

<domain>
## Phase Boundary

Implement SP800-108 Counter Mode KDF for SMB3 session key derivation and replace HMAC-SHA256 signing with AES-128-CMAC/GMAC for SMB 3.x sessions. All 4 session keys (signing, encryption, decryption, application) are derived. Signing algorithm dispatches by negotiated dialect. SIGNING_CAPABILITIES negotiate context added for GMAC negotiation.

Encryption/decryption key usage is Phase 35. Kerberos session key extraction is Phase 36. Cross-protocol integration is Phase 39. Comprehensive conformance testing is Phase 40.

</domain>

<decisions>
## Implementation Decisions

### Key Storage Model
- New `SessionCryptoState` struct replaces existing `SessionSigningState` for all dialects
- `SessionCryptoState` lives in `internal/adapter/smb/session/` package (alongside Session)
- Stores typed key objects (e.g., SigningKey with algorithm-specific methods), not raw bytes
- CryptoState fully replaces the old `Signing` field on Session (2.x uses HMAC path internally, 3.x uses CMAC/GMAC)
- All 4 keys (signing, encryption, decryption, application) derived in Phase 34 via KDF, even though encryption/decryption usage comes in Phase 35
- `DeriveAllKeys` returns a fully constructed `SessionCryptoState` ready to attach to Session
- Key material zeroized on session destroy (defense-in-depth via `Destroy()` method)

### KDF Implementation
- New `internal/adapter/smb/kdf/` package for SP800-108 Counter Mode KDF
- Generic `DeriveKey(sessionKey, purpose, dialect, preauthHash)` function with `KeyPurpose` enum
- Purpose enum maps to correct label/context strings per MS-SMB2 spec
- SMB 3.0/3.0.2: constant label/context strings per KDF-02
- SMB 3.1.1: preauth integrity hash as KDF context per KDF-03

### Signing Abstraction
- `Signer` interface with `Sign(msg) [16]byte` and `Verify(msg) bool` methods
- Three implementations in separate files: `hmac_signer.go`, `cmac_signer.go`, `gmac_signer.go`
- Standalone `SignMessage()` helper handles SMB2 header flag-setting and calls `Signer.Sign` internally (pure interface, helper handles protocol concerns)
- AES-CMAC: Implement from RFC 4493 (~80 lines using crypto/aes), no external dependency
- AES-GMAC: Use Go stdlib `crypto/aes` + `cipher.NewGCM` with empty plaintext, message as AAD
- GMAC nonce: Extract MessageId from SMB2 header bytes 28-35 internally (Sign(msg) signature stays simple)
- Fixed `[16]byte` return type for signatures (all SMB2 algorithms produce 16 bytes)
- Old `SigningKey` struct refactored into `HMACSigner` implementing the `Signer` interface (no dead code)
- Dialect-aware factory: `NewSigner(dialect, signingAlgorithmId)` automatically picks the right algorithm
- CMAC location: Claude's discretion (signing/ package or standalone cmac/ package)

### Negotiate Contexts
- SIGNING_CAPABILITIES (0x0008) constants and parsing added to existing `types/negotiate_context.go`
- Server advertises both AES-128-GMAC and AES-128-CMAC (GMAC preferred)
- Signing algorithm preference order is configurable via adapter settings (ordered preference list)
- Default preference: [AES-128-GMAC, AES-128-CMAC, HMAC-SHA256]
- When 3.1.1 client omits SIGNING_CAPABILITIES: default to AES-128-CMAC (per MS-SMB2 spec)
- Response SIGNING_CAPABILITIES contains only the selected algorithm (per MS-SMB2 3.3.5.4)

### Session Setup Integration
- Dialect check in session_setup.go: if 3.x -> use KDF to derive all 4 keys via SessionCryptoState; if 2.x -> keep existing `DeriveSigningKey` path
- Separate code paths for 2.x and 3.x to minimize risk to working 2.x signing flow
- framing.go updated to use new SessionCryptoState and Signer interface for all dialects

### Claude's Discretion
- CMAC implementation location (signing/ or standalone cmac/ package)
- Internal struct layout of SessionCryptoState fields
- Exact SP800-108 counter mode implementation details
- Error handling strategy for crypto failures
- Logging level and format for key derivation events

</decisions>

<specifics>
## Specific Ideas

- E2E tests preferred over integration tests for validation
- Update MVPT suite (extend smbtorture run.sh with signing-related sub-suites)
- Single cohesive delivery (kdf/, signing refactor, negotiate contexts, session integration all together)
- All documentation deferred to Phase 39

</specifics>

<code_context>
## Existing Code Insights

### Reusable Assets
- `internal/adapter/smb/signing/signing.go`: Current HMAC-SHA256 `SigningKey` + `SessionSigningState` + `SigningConfig` — will be refactored into `HMACSigner`
- `internal/adapter/smb/crypto_state.go`: `ConnectionCryptoState` with preauth hash chain, already stores `SigningAlgorithmId` and `Dialect`
- `internal/adapter/smb/auth/ntlm.go`: `DeriveSigningKey()` for NTLM key exchange — stays for 2.x path
- `internal/adapter/smb/types/negotiate_context.go`: Negotiate context parsing/encoding infrastructure — extend with SIGNING_CAPABILITIES
- `internal/adapter/smb/v2/handlers/negotiate.go`: `processNegotiateContexts()` — extend with signing capabilities handling

### Established Patterns
- Signing flow: NTLM auth -> `DeriveSigningKey()` -> `configureSessionSigningWithKey()` -> `Session.SetSigningKey()`
- Negotiate contexts: parsed in `processNegotiateContexts()`, response contexts built as `[]types.NegotiateContext`
- Session signing: `Session` delegates to `SessionSigningState` via `ShouldSign()`, `SignMessage()`, `VerifyMessage()`
- framing.go: signing applied at message framing layer (line 369), checks `sess.Signing.SigningKey`

### Integration Points
- `configureSessionSigningWithKey()` in session_setup.go — where 3.x KDF replaces direct key usage
- `processNegotiateContexts()` in negotiate.go — where SIGNING_CAPABILITIES context handling is added
- `framing.go` line 369 — where signing is applied to outgoing messages (needs SessionCryptoState migration)
- `ConnectionCryptoState.SigningAlgorithmId` — already captured during negotiate, will be used to select Signer
- smbtorture infra at `test/smb-conformance/smbtorture/` — extend run.sh with signing suites

</code_context>

<validation>
## Validation Strategy

### Unit Tests
- SP800-108 KDF: MS-SMB2 spec test vectors (Appendix A) for all dialect variants (3.0, 3.0.2, 3.1.1)
- AES-CMAC: RFC 4493 test vectors + MS-SMB2 signing test vectors
- AES-GMAC: Signing test vectors with known nonce/key/message inputs
- Signer interface: Each implementation tested independently + factory dispatch tests

### E2E Tests
- Go test with go-smb2 library connecting via SMB 3.x, validating signing negotiation and signed operations
- Live DittoFS server started in test process (matches existing E2E patterns)
- E2E tests preferred over integration tests

### smbtorture
- Extend run.sh with smb2.signing and smb2.session signing sub-suites
- Run full smbtorture suite after Phase 34
- Update KNOWN_FAILURES.md baseline to track signing-related test status changes

</validation>

<deferred>
## Deferred Ideas

None — discussion stayed within phase scope

</deferred>

---

*Phase: 34-key-derivation-and-signing*
*Context gathered: 2026-03-01*
