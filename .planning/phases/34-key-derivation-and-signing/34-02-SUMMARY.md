---
phase: 34-key-derivation-and-signing
plan: 02
subsystem: auth
tags: [smb3, signing, kdf, aes-cmac, aes-gmac, hmac-sha256, sp800-108, session-crypto]

# Dependency graph
requires:
  - phase: 34-01
    provides: "KDF (SP800-108), Signer interface, CMAC/GMAC/HMAC signer implementations"
provides:
  - "SessionCryptoState replacing SessionSigningState with unified crypto abstraction"
  - "SIGNING_CAPABILITIES negotiate context (0x0008) parsed and responded"
  - "3.x KDF integration in session setup (all 4 keys derived)"
  - "Framing/compound/response migrated to CryptoState.Signer interface"
  - "SigningAlgorithmPreference configurable in SMBAdapterSettings"
affects: [35-encryption, 36-kerberos, smb3-session-binding]

# Tech tracking
tech-stack:
  added: []
  patterns:
    - "SessionCryptoState as unified per-session crypto container (signing + encryption + application keys)"
    - "CryptoState interface for breaking circular imports between handlers/ and smb/ packages"
    - "DeriveAllKeys factory dispatches by dialect (2.x direct, 3.x KDF)"

key-files:
  created:
    - "internal/adapter/smb/session/crypto_state.go"
  modified:
    - "internal/adapter/smb/session/session.go"
    - "internal/adapter/smb/v2/handlers/negotiate.go"
    - "internal/adapter/smb/v2/handlers/session_setup.go"
    - "internal/adapter/smb/v2/handlers/context.go"
    - "internal/adapter/smb/crypto_state.go"
    - "internal/adapter/smb/framing.go"
    - "internal/adapter/smb/compound.go"
    - "internal/adapter/smb/types/negotiate_context.go"
    - "internal/adapter/smb/types/constants.go"
    - "internal/adapter/smb/signing/signing.go"
    - "internal/adapter/smb/v2/handlers/handler.go"
    - "internal/adapter/smb/v2/handlers/negotiate_test.go"
    - "pkg/controlplane/models/adapter_settings.go"
    - "test/smb-conformance/smbtorture/KNOWN_FAILURES.md"

key-decisions:
  - "SessionCryptoState holds all 4 keys (signing, encryption, decryption, application) even though encryption is Phase 35 -- avoids re-deriving later"
  - "DeriveAllKeys is the single entry point for all key derivation, dispatching by dialect version"
  - "Signing algorithm preference is configurable via SMBAdapterSettings (JSON array)"
  - "When 3.1.1 client omits SIGNING_CAPABILITIES, server defaults to AES-128-CMAC per MS-SMB2 spec"
  - "Key material zeroed on session destroy via Destroy() for defense-in-depth"

patterns-established:
  - "SessionCryptoState pattern: per-session crypto container replaces per-feature signing state"
  - "CryptoState interface extension pattern: add methods to interface + implement on ConnectionCryptoState"
  - "Negotiate context extension pattern: add constant, type with Encode/Decode, handler case"

requirements-completed: [KDF-01, KDF-02, KDF-03, SIGN-01, SIGN-02, SIGN-03]

# Metrics
duration: 10min
completed: 2026-03-01
---

# Phase 34 Plan 02: Session Lifecycle Integration Summary

**SessionCryptoState with SP800-108 KDF for SMB 3.x key derivation, SIGNING_CAPABILITIES negotiate context, and AES-CMAC/GMAC signing wired into session setup and message framing**

## Performance

- **Duration:** 10 min
- **Started:** 2026-03-01T20:41:45Z
- **Completed:** 2026-03-01T20:52:38Z
- **Tasks:** 2
- **Files modified:** 16

## Accomplishments
- Created SessionCryptoState as unified per-session crypto abstraction replacing SessionSigningState, holding all 4 derived keys (signing, encryption, decryption, application)
- Wired SIGNING_CAPABILITIES negotiate context (0x0008) into SMB 3.1.1 negotiation with configurable server preference order for signing algorithms
- Integrated SP800-108 KDF into session setup: 3.x sessions derive all keys using negotiated dialect, preauth hash, cipher ID, and signing algorithm
- Migrated all message signing/verification paths (framing, compound, response) from old Signing field to CryptoState.Signer interface

## Task Commits

Each task was committed atomically:

1. **Task 1: SessionCryptoState, SIGNING_CAPABILITIES, and negotiate context wiring** - `683742bb` (feat)
2. **Task 2: Session setup KDF integration, framing migration, and smbtorture update** - `aae74111` (feat)

## Files Created/Modified
- `internal/adapter/smb/session/crypto_state.go` - New file: SessionCryptoState struct with DeriveAllKeys, Destroy, ShouldSign/ShouldVerify
- `internal/adapter/smb/session/session.go` - Replaced Signing field with CryptoState, updated all signing methods to delegate
- `internal/adapter/smb/v2/handlers/negotiate.go` - Added SIGNING_CAPABILITIES context parsing, selectSigningAlgorithm, response building
- `internal/adapter/smb/v2/handlers/session_setup.go` - Dialect-aware configureSessionSigningWithKey: 2.x direct HMAC, 3.x KDF path
- `internal/adapter/smb/v2/handlers/context.go` - Extended CryptoState interface with 4 new methods
- `internal/adapter/smb/crypto_state.go` - Added GetCipherId, SetSigningAlgorithmId, GetSigningAlgorithmId implementations
- `internal/adapter/smb/v2/handlers/handler.go` - Added SigningAlgorithmPreference field
- `internal/adapter/smb/framing.go` - Migrated from sess.Signing to sess.CryptoState
- `internal/adapter/smb/compound.go` - Migrated from sess.Signing to sess.CryptoState
- `internal/adapter/smb/types/negotiate_context.go` - Added SigningCaps type with Encode/Decode
- `internal/adapter/smb/types/constants.go` - Added NegCtxSigningCaps constant (0x0008)
- `internal/adapter/smb/signing/signing.go` - Removed SessionSigningState (replaced by session.SessionCryptoState)
- `internal/adapter/smb/signing/signing_test.go` - Removed obsolete TestSessionSigningState
- `internal/adapter/smb/v2/handlers/negotiate_test.go` - Added 3 new tests for signing caps negotiation
- `pkg/controlplane/models/adapter_settings.go` - Added SigningAlgorithmPreference field with Get/Set methods
- `test/smb-conformance/smbtorture/KNOWN_FAILURES.md` - Updated to reflect Phase 34 SMB 3.x signing support

## Decisions Made
- SessionCryptoState stores all 4 keys upfront (even encryption/decryption for Phase 35) to avoid re-deriving later
- DeriveAllKeys is dialect-dispatched: < 3.0 gets HMAC directly, >= 3.0 gets full KDF with all purposes
- configureSessionSigningWithKey now takes handler context parameter to access negotiated dialect/cipher/signing algorithm
- Default signing algorithm preference: GMAC > CMAC > HMAC-SHA256 (configurable via adapter settings)
- When 3.1.1 clients omit SIGNING_CAPABILITIES, AES-128-CMAC is used per MS-SMB2 spec

## Deviations from Plan

### Auto-fixed Issues

**1. [Rule 1 - Bug] Removed obsolete TestSessionSigningState test**
- **Found during:** Task 1 (Clean up old signing.go)
- **Issue:** signing/signing_test.go referenced NewSessionSigningState() which was removed, causing compile failure
- **Fix:** Removed the obsolete test and added a comment pointing to the new location
- **Files modified:** internal/adapter/smb/signing/signing_test.go
- **Verification:** go test ./internal/adapter/smb/signing/ passes
- **Committed in:** 683742bb (Task 1 commit)

---

**Total deviations:** 1 auto-fixed (1 bug)
**Impact on plan:** Test referenced removed type. Necessary fix for compilation. No scope creep.

## Issues Encountered
None

## User Setup Required
None - no external service configuration required.

## Next Phase Readiness
- Phase 34 complete: KDF primitives (Plan 01) and session lifecycle integration (Plan 02) are both done
- SMB 3.x sessions now derive all 4 keys and use AES-CMAC/GMAC signing
- Ready for Phase 35 (encryption) which will use the EncryptionKey/DecryptionKey already derived
- Ready for Phase 36 (Kerberos) which can use the same DeriveAllKeys path with Kerberos session keys
- smbtorture signing tests baseline needs updating after full run (some session tests may now pass)

---
*Phase: 34-key-derivation-and-signing*
*Completed: 2026-03-01*
