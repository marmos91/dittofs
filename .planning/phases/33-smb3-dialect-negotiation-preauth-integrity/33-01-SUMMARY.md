---
phase: 33-smb3-dialect-negotiation-preauth-integrity
plan: 01
subsystem: protocol
tags: [smb3, binary-codec, negotiate-context, preauth-integrity, sha512, crypto]

# Dependency graph
requires: []
provides:
  - "smbenc binary codec with error accumulation pattern (Reader/Writer)"
  - "Negotiate context types: PreauthIntegrityCaps, EncryptionCaps, NetnameContext"
  - "ParseNegotiateContextList/EncodeNegotiateContextList with 8-byte alignment"
  - "ConnectionCryptoState with SHA-512 preauth hash chain"
  - "CryptoState eagerly created per connection and threaded via ConnInfo"
affects: [33-02, 33-03, smb3-session-setup, smb3-signing, smb3-encryption]

# Tech tracking
tech-stack:
  added: [crypto/sha512]
  patterns: [error-accumulation-codec, preauth-hash-chain, eager-state-creation]

key-files:
  created:
    - internal/adapter/smb/smbenc/reader.go
    - internal/adapter/smb/smbenc/writer.go
    - internal/adapter/smb/smbenc/doc.go
    - internal/adapter/smb/types/negotiate_context.go
    - internal/adapter/smb/crypto_state.go
  modified:
    - internal/adapter/smb/types/constants.go
    - internal/adapter/smb/conn_types.go
    - pkg/adapter/smb/connection.go

key-decisions:
  - "smbenc uses buffer-based pattern with position cursor, not streaming io.Reader"
  - "Error accumulation pattern: first error stops further reads, caller checks once"
  - "ConnectionCryptoState in internal/adapter/smb to avoid circular imports (pkg/adapter/smb imports internal)"
  - "CryptoState created eagerly in NewConnection for all connections"
  - "SecurityMode type added to constants.go for server/client security mode fields"

patterns-established:
  - "smbenc error accumulation: read/write sequence, check Err() once at end"
  - "Negotiate context 8-byte alignment: pad between contexts, not after last"
  - "SHA-512 preauth hash chain: H(i) = SHA-512(H(i-1) || message)"

requirements-completed: [ARCH-02, NEG-02, NEG-04]

# Metrics
duration: 9min
completed: 2026-02-28
---

# Phase 33 Plan 01: Foundation Packages Summary

**smbenc binary codec with error accumulation, negotiate context types with 8-byte alignment, and ConnectionCryptoState with SHA-512 preauth hash chain per MS-SMB2 spec**

## Performance

- **Duration:** 9 min
- **Started:** 2026-02-28T22:24:41Z
- **Completed:** 2026-02-28T22:33:55Z
- **Tasks:** 2
- **Files modified:** 12 (5 created, 3 new test files, 4 modified)

## Accomplishments
- Created smbenc binary codec package with Reader and Writer types using error accumulation pattern
- Implemented negotiate context types (PreauthIntegrityCaps, EncryptionCaps, NetnameContext) with full encode/decode and 8-byte alignment
- Built ConnectionCryptoState with SHA-512 preauth integrity hash chain per MS-SMB2 spec
- Integrated CryptoState into Connection lifecycle (eager creation) and ConnInfo threading
- 54 tests total covering all edge cases, roundtrips, alignment, and concurrency

## Task Commits

Each task was committed atomically:

1. **Task 1: Create smbenc binary codec package** - `8522b63b` (feat)
2. **Task 2: Create negotiate context types and ConnectionCryptoState** - `7840e4c0` (feat)

_Note: TDD tasks had tests written first (RED), then implementation (GREEN), committed together._

## Files Created/Modified
- `internal/adapter/smb/smbenc/reader.go` - Buffer-based SMB binary reader with error accumulation
- `internal/adapter/smb/smbenc/writer.go` - Buffer-based SMB binary writer with alignment padding
- `internal/adapter/smb/smbenc/doc.go` - Package documentation with usage examples
- `internal/adapter/smb/smbenc/reader_test.go` - 19 tests for Reader
- `internal/adapter/smb/smbenc/writer_test.go` - 13 tests for Writer (including 12 Pad subtests)
- `internal/adapter/smb/types/negotiate_context.go` - Negotiate context types and encode/decode
- `internal/adapter/smb/types/negotiate_context_test.go` - 16 tests for negotiate contexts
- `internal/adapter/smb/types/constants.go` - Added negotiate context, hash, cipher, security mode constants
- `internal/adapter/smb/crypto_state.go` - ConnectionCryptoState with SHA-512 hash chain
- `internal/adapter/smb/crypto_state_test.go` - 6 tests including concurrent access
- `internal/adapter/smb/conn_types.go` - Added CryptoState field to ConnInfo
- `pkg/adapter/smb/connection.go` - Added CryptoState to Connection, eager creation in NewConnection

## Decisions Made
- **smbenc in internal/adapter/smb/smbenc:** Buffer-based pattern chosen over streaming io.Reader for SMB's fixed-size protocol messages. Error accumulation eliminates per-operation error checking.
- **ConnectionCryptoState placement:** Placed in `internal/adapter/smb` (same package as ConnInfo) instead of `pkg/adapter/smb` to avoid circular import (pkg/adapter/smb imports internal/adapter/smb). Connection in pkg references it via the existing import.
- **Eager CryptoState creation:** All connections get a CryptoState immediately, even for pre-3.1.1 dialects. The overhead is minimal (128 bytes) and simplifies the code path.
- **SecurityMode type:** Added to constants.go since ConnectionCryptoState needs it for server/client security mode fields.

## Deviations from Plan

None - plan executed exactly as written.

## Issues Encountered

None.

## User Setup Required

None - no external service configuration required.

## Next Phase Readiness
- smbenc codec ready for use in negotiate request/response encoding (plan 33-02)
- Negotiate context types ready for parsing client contexts and building server responses
- ConnectionCryptoState ready for preauth hash chain updates during negotiate flow
- All packages compile cleanly, no circular imports, full test suite passes

## Self-Check: PASSED

- All 10 created/modified source files verified on disk
- Commit 8522b63b (Task 1) verified in git log
- Commit 7840e4c0 (Task 2) verified in git log
- Full test suite passes (go test ./... -count=1)
- Full build succeeds (go build ./...)

---
*Phase: 33-smb3-dialect-negotiation-preauth-integrity*
*Completed: 2026-02-28*
