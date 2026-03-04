---
phase: 35-encryption-and-transform-header
plan: 02
subsystem: encryption
tags: [smb3, encryption, transform-header, aead, middleware, framing, gcm, ccm]

# Dependency graph
requires:
  - phase: 35-encryption-and-transform-header
    provides: Encryptor interface, TransformHeader wire format, EncryptionConfig, Share.EncryptData
provides:
  - EncryptionMiddleware with DecryptRequest/EncryptResponse/ShouldEncrypt
  - Framing layer 0xFD transform header detection and transparent decryption
  - Response encryption for encrypted sessions (signing bypassed for AEAD)
  - SessionCryptoState.CreateEncryptors with correct key direction
  - EncryptWithNonce on Encryptor interface for nonce-first AAD computation
  - 256-bit-first cipher preference (AES-256-GCM > AES-256-CCM > AES-128-GCM > AES-128-CCM)
  - Consecutive decryption failure tracking (5 failures = drop connection)
affects: [35-03-enforcement-logic]

# Tech tracking
tech-stack:
  added: []
  patterns: [encryption-middleware-pattern, session-lookup-closure, nonce-before-aad]

key-files:
  created:
    - internal/adapter/smb/encryption/middleware.go
    - internal/adapter/smb/encryption/middleware_test.go
  modified:
    - internal/adapter/smb/encryption/encryptor.go
    - internal/adapter/smb/encryption/gcm_encryptor.go
    - internal/adapter/smb/encryption/ccm_encryptor.go
    - internal/adapter/smb/session/crypto_state.go
    - internal/adapter/smb/session/session.go
    - internal/adapter/smb/framing.go
    - internal/adapter/smb/conn_types.go
    - internal/adapter/smb/response.go
    - internal/adapter/smb/v2/handlers/negotiate.go
    - pkg/adapter/smb/connection.go

key-decisions:
  - "EncryptWithNonce added to Encryptor interface to solve nonce-before-AAD chicken-and-egg problem"
  - "EncryptableSession interface decouples middleware from session.Session to prevent circular imports"
  - "Cipher preference updated to 256-bit first: AES-256-GCM > AES-256-CCM > AES-128-GCM > AES-128-CCM"
  - "DecryptFailures tracked per-connection with atomic.Int32, 5-failure disconnect threshold"

patterns-established:
  - "EncryptionMiddleware: session lookup closure decouples from session manager"
  - "Nonce-before-AAD: generate nonce externally, set in header, compute AAD, then EncryptWithNonce"
  - "Encrypted sessions bypass signing entirely (AEAD provides integrity per MS-SMB2 3.3.4.1.1)"

requirements-completed: [ENC-06]

# Metrics
duration: 9min
completed: 2026-03-02
---

# Phase 35 Plan 02: Encryption Middleware and Framing Integration Summary

**EncryptionMiddleware with transparent decrypt/encrypt in framing and response layers, SessionCryptoState encryptor activation, and 256-bit-first cipher preference**

## Performance

- **Duration:** 9 min
- **Started:** 2026-03-02T08:58:39Z
- **Completed:** 2026-03-02T09:08:07Z
- **Tasks:** 2
- **Files modified:** 12

## Accomplishments
- EncryptionMiddleware with round-trip encrypt/decrypt verified for all 4 cipher variants (AES-128/256 GCM/CCM)
- Framing layer transparently detects 0xFD transform headers and decrypts before command dispatch
- Response encryption wraps outgoing messages in transform headers, skipping signing (AEAD integrity)
- SessionCryptoState.CreateEncryptors correctly maps key direction (client perspective names)
- 10 middleware tests covering round-trip, tampering, unknown session, header field verification
- Consecutive decryption failure tracking drops connection after 5 failures

## Task Commits

Each task was committed atomically:

1. **Task 1: EncryptionMiddleware and SessionCryptoState Encryptor/Decryptor activation** - `8c5d7647` (feat)
2. **Task 2: Framing layer decrypt, response encrypt, cipher preference, and connection wiring** - `6d6de373` (feat)

## Files Created/Modified
- `internal/adapter/smb/encryption/middleware.go` - EncryptionMiddleware interface and sessionEncryptionMiddleware implementation
- `internal/adapter/smb/encryption/middleware_test.go` - 10 tests: round-trip GCM/CCM/256, tampering, unknown session, header fields
- `internal/adapter/smb/encryption/encryptor.go` - Added EncryptWithNonce to Encryptor interface
- `internal/adapter/smb/encryption/gcm_encryptor.go` - EncryptWithNonce for GCM
- `internal/adapter/smb/encryption/ccm_encryptor.go` - EncryptWithNonce for CCM
- `internal/adapter/smb/session/crypto_state.go` - EncryptData, Encryptor/Decryptor fields, CreateEncryptors, ShouldEncrypt
- `internal/adapter/smb/session/session.go` - Convenience methods: ShouldEncrypt, EncryptWithNonce, DecryptMessage, nonce/overhead getters
- `internal/adapter/smb/framing.go` - 0xFD detection in ReadRequest, EncryptionMiddleware parameter
- `internal/adapter/smb/conn_types.go` - EncryptionMiddleware and DecryptFailures fields on ConnInfo
- `internal/adapter/smb/response.go` - Encrypt-before-send in SendMessage, signing bypass for encrypted sessions
- `internal/adapter/smb/v2/handlers/negotiate.go` - Cipher preference updated to 256-bit first
- `pkg/adapter/smb/connection.go` - EncryptionMiddleware wiring, DecryptFailures tracking, isDecryptionError helper

## Decisions Made
- Added `EncryptWithNonce` to the Encryptor interface because the middleware needs to generate the nonce before computing the transform header AAD (nonce is part of the AAD). The existing `Encrypt` method generates nonces internally, creating a chicken-and-egg problem.
- Created `EncryptableSession` interface in the encryption package to decouple the middleware from `session.Session`, avoiding circular imports between encryption/ and session/ packages.
- Cipher preference updated to 256-bit first per user decision from Phase 35 planning.
- Decryption failures tracked per-connection with `atomic.Int32`. The 5-failure threshold prevents brute-force attacks while allowing occasional transient failures.

## Deviations from Plan

### Auto-fixed Issues

**1. [Rule 3 - Blocking] Added EncryptWithNonce to Encryptor interface**
- **Found during:** Task 1 (EncryptionMiddleware implementation)
- **Issue:** The existing Encrypt method generates nonces internally, but the middleware needs to generate the nonce first (to set it in the TransformHeader before computing AAD). This is a protocol-level requirement per MS-SMB2 3.1.4.3.
- **Fix:** Added EncryptWithNonce(nonce, plaintext, aad) to Encryptor interface and both GCM/CCM implementations
- **Files modified:** encryptor.go, gcm_encryptor.go, ccm_encryptor.go
- **Verification:** All 33 encryption tests pass, round-trip middleware tests verify correctness
- **Committed in:** 8c5d7647 (Task 1 commit)

---

**Total deviations:** 1 auto-fixed (1 blocking)
**Impact on plan:** Essential for protocol correctness. The nonce-before-AAD requirement is fundamental to the MS-SMB2 encryption spec. No scope creep.

## Issues Encountered
None.

## User Setup Required
None - no external service configuration required.

## Next Phase Readiness
- EncryptionMiddleware ready for enforcement logic (Plan 03)
- SessionCryptoState.CreateEncryptors ready to be called during SESSION_SETUP
- ShouldEncrypt ready for per-session and per-share encryption enforcement
- Framing and response layers fully prepared for encrypted traffic

---
*Phase: 35-encryption-and-transform-header*
*Completed: 2026-03-02*
