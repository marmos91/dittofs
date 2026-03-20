---
phase: 67-smb3-signing-fix
plan: 01
subsystem: protocol
tags: [smb3, signing, preauth-hash, sha512, conformance-tests]

# Dependency graph
requires: []
provides:
  - Preauth integrity hash conformance test suite with MS-SMB2 test vectors
  - Systematic spec-compliance audit of all 4 pitfalls from issue #252
  - Root cause analysis for 5 WPTS BVT_Negotiate_SMB311 failures
  - Diagnostic foundation for Plan 02 fixes
affects: [67-02]

# Tech tracking
tech-stack:
  added: []
  patterns:
    - "MS-SMB2 test vector replay for conformance testing"
    - "Parse+Encode roundtrip verification for wire format fidelity"

key-files:
  created:
    - internal/adapter/smb/crypto_state_conformance_test.go
  modified: []

key-decisions:
  - "All 4 preauth hash pitfalls from issue #252 are confirmed correct in current code"
  - "Preauth hash primitives (chainHash, UpdatePreauthHash, StashPendingSessionSetup) verified correct via MS-SMB2 test vectors"
  - "WPTS failures not caused by preauth hash computation but likely by session-level signing key application"

patterns-established:
  - "Conformance testing: replay official MS-SMB2 test vectors and assert exact hash values"
  - "Roundtrip fidelity testing: Parse+Encode produces byte-identical output for all header configurations"

requirements-completed: []

# Metrics
duration: 7min
completed: 2026-03-20
---

# Phase 67 Plan 01: SMB3 Signing Triage Summary

**Preauth hash conformance tests prove all primitives correct; systematic audit confirms 4/4 pitfalls from #252 are handled correctly; WPTS failures likely caused by session-level issue not hash computation**

## Performance

- **Duration:** 7 min
- **Started:** 2026-03-20T13:03:55Z
- **Completed:** 2026-03-20T13:10:56Z
- **Tasks:** 2
- **Files modified:** 1

## Accomplishments

- Created 7 conformance tests covering MS-SMB2 official test vectors, chain ordering, stash/init equivalence, final-response exclusion, and Parse+Encode roundtrip fidelity
- All 7 tests PASS, proving the core preauth hash primitives are correct
- Systematic code audit confirms all 4 pitfalls from issue #252 are handled correctly in the current codebase
- Identified that WPTS BVT_Negotiate_SMB311 failures are NOT caused by preauth hash computation bugs

## Task Commits

Each task was committed atomically:

1. **Task 1: Create preauth hash conformance tests with MS-SMB2 test vectors** - `89bb8e53` (test)
2. **Task 2: Audit preauth hash flow, diagnose root causes** - No code changes (diagnostic audit only)

## Files Created/Modified

- `internal/adapter/smb/crypto_state_conformance_test.go` - 7 conformance tests: MS-SMB2 test vector validation, chain order verification, stash/init equivalence, final-response exclusion, rawMessage format, Parse+Encode roundtrip fidelity, chainHash vs SHA-512 manual computation

## Conformance Test Results

All tests PASS:

| Test | Status | What it proves |
|------|--------|---------------|
| TestPreauthHashConformance_NegotiateRequest | PASS | chainHash matches MS-SMB2 official NEGOTIATE request test vector |
| TestPreauthHashConformance_ChainOrder | PASS | Full chain order (NEG req/resp + SS req/resp/req) matches manual SHA-512 |
| TestPreauthHash_SessionID0_StashConsumption | PASS | Stash+Init equivalent to Init+Update for SessionID=0 edge case |
| TestPreauthHash_FinalResponseNotIncluded | PASS | Derivation hash excludes final SESSION_SETUP response |
| TestRawMessageStartsWithSMB2ProtocolID | PASS | rawMessage starts with 0xFE534D42, no NetBIOS leak |
| TestRawMessageRoundtripFidelity | PASS | Parse+Encode byte-identical for 6 header configurations |
| TestPreauthHash_ChainHashMatchesSHA512 | PASS | chainHash matches direct SHA-512 computation |

## Pitfall Analysis (Issue #252)

### Pitfall 1: NetBIOS prefix in hash - CONFIRMED CORRECT

- **File:** `internal/adapter/smb/framing.go`, function `readNetBIOSPayload` (line 134)
- **Evidence:** `readNetBIOSPayload` reads and strips the 4-byte NetBIOS header, returning only the SMB2 payload. `connection.go` (line 191-193) reconstructs rawMessage from `hdr.Encode() + body`, both of which come from the already-stripped message.
- **Test proof:** `TestRawMessageStartsWithSMB2ProtocolID` asserts bytes 0-3 are `[0xFE, 0x53, 0x4D, 0x42]`.

### Pitfall 2: Final SESSION_SETUP response in derivation hash - CONFIRMED CORRECT

- **File:** `internal/adapter/smb/v2/handlers/session_setup.go`, function `configureSessionSigningWithKey` (line 642-650)
- **Evidence:** `GetSessionPreauthHash(sess.SessionID)` is called on line 642 to capture the derivation hash. Then `DeleteSessionPreauthHash(sess.SessionID)` is called on line 650 to clean up. The after-hook (`sessionPreauthAfterHook` in hooks.go line 157) calls `UpdateSessionPreauthHash(sessionID, rawMessage)` but since the entry was deleted, this is a no-op.
- **Test proof:** `TestPreauthHash_FinalResponseNotIncluded` verifies the derivation hash differs from hash-with-final-response.

### Pitfall 3: rawMessage roundtrip fidelity - CONFIRMED CORRECT

- **File:** `internal/adapter/smb/header/parser.go` (Parse) and `internal/adapter/smb/header/encoder.go` (Encode)
- **Evidence:** Parse reads all 14 fields faithfully (ProtocolID through Signature). Encode writes all 14 fields back. Both use the same field offsets and byte order (little-endian). Parse hardcodes validation of ProtocolID=0x424D53FE and StructureSize=64, which are the same values Encode hardcodes for output.
- **Test proof:** `TestRawMessageRoundtripFidelity` tests Parse+Encode for 6 configurations (basic negotiate, session setup with session ID, response with status, signed response, compound with NextCommand, reserved/processID), all byte-identical.

### Pitfall 4: Response signature in after-hook - CONFIRMED CORRECT

- **File:** `internal/adapter/smb/response.go`, function `SendMessage` (line 371-376) and `SendResponseWithHooks` (line 252-266)
- **Evidence:** `SendMessage` computes signature on `smbPayload`, then syncs it back via `copy(hdr.Signature[:], smbPayload[48:64])` (line 376). After `SendMessage` returns, `SendResponseWithHooks` builds `rawResponse = respHeader.Encode() + body` (line 263), which includes the synced-back signature. The after-hook receives the correctly-signed bytes.
- **Caveat:** For intermediate SESSION_SETUP responses (STATUS_MORE_PROCESSING_REQUIRED), no session key exists yet so no signing occurs -- signature field stays zero. This is correct because the preauth hash should include the unsigned bytes for intermediate responses.

## WPTS BVT_Negotiate_SMB311 Root Cause Analysis

### Key Finding: Preauth hash primitives are NOT the root cause

All conformance tests pass, proving the hash chain computation is correct. The WPTS failures must be caused by something else in the negotiate/session-setup flow.

### Hypothesis: Session-level signing key application issue

Since the WPTS config has `DisableVerifySignature=true` (the test client does not verify server signatures), the test still proceeds through SESSION_SETUP. However, if the server derives incorrect signing keys due to some edge case, the server would reject the client's valid signed requests during subsequent operations (TREE_CONNECT, CREATE, etc.).

The preauth hash computation is correct, but there may be an issue with:
1. How the preauth hash is passed to `DeriveAllKeys` (file: `session/crypto_state.go` line 85-117)
2. Whether the session key from NTLMv2 validation is correct
3. Whether the signing algorithm selection is compatible with what WPTS expects

### Alternative hypothesis: Negotiate response format issue

The 5 tests all focus on NEGOTIATE with different encryption cipher variants. There may be an issue with:
- Negotiate context alignment/padding in the response (file: `negotiate.go` lines 203-223)
- The misleading comment on line 205 says "Security buffer is 0 bytes" but `securityBuffer` may be non-empty (SPNEGO NegHints)
- The `NegotiateContextOffset` calculation accounts for this correctly (using `len(resp)` which includes the security buffer), but the alignment padding is relative to `64 + len(resp)` which should be checked

### Specific code locations for Plan 02 investigation

| Location | Function | Line | What to check |
|----------|----------|------|---------------|
| `internal/adapter/smb/v2/handlers/negotiate.go` | `Negotiate` | 203-223 | Negotiate context offset/alignment with security buffer present |
| `internal/adapter/smb/v2/handlers/session_setup.go` | `configureSessionSigningWithKey` | 621-745 | Key derivation flow completeness |
| `internal/adapter/smb/session/crypto_state.go` | `DeriveAllKeys` | 85-117 | KDF label/context correctness for 3.1.1 |
| `internal/adapter/smb/kdf/kdf.go` | `LabelAndContext` | 119-150 | Label strings match MS-SMB2 spec exactly |
| `internal/adapter/smb/hooks.go` | `sessionPreauthBeforeHook` | 119-139 | SessionID=0 stashing + non-zero updating |
| `internal/adapter/smb/response.go` | `SendResponseWithHooks` | 252-266 | rawResponse includes signed bytes |

### WPTS Test-Specific Analysis

| Test | Likely failure point | Priority |
|------|---------------------|----------|
| BVT_Negotiate_SMB311 | Basic 3.1.1 negotiate + session setup. If signing fails, subsequent operations fail. | HIGH |
| BVT_Negotiate_SMB311_Preauthentication_Encryption_CCM | Same as above but negotiates AES-128-CCM. May test encrypted session establishment. | HIGH |
| BVT_Negotiate_SMB311_Preauthentication_Encryption_GCM | Same as above but negotiates AES-128-GCM. | HIGH |
| BVT_Negotiate_SMB311_Preauthentication_Encryption_AES_256_CCM | Same but AES-256-CCM. Tests 256-bit key derivation. | HIGH |
| BVT_Negotiate_SMB311_Preauthentication_Encryption_AES_256_GCM | Same but AES-256-GCM. Tests 256-bit key derivation. | HIGH |

### Recommended Next Steps for Plan 02

1. **Run WPTS with verbose logging** to capture exact error messages for each of the 5 tests
2. **Add DEBUG logging** to `configureSessionSigningWithKey` to log the preauth hash value used for KDF and the derived signing key prefix
3. **Compare with Samba reference** using Docker container packet captures
4. **Test macOS mount_smbfs** against current code (PR #285 may have already fixed it)

## Decisions Made

- All 4 pitfalls confirmed correct -- no code fixes needed for preauth hash primitives
- WPTS failures need runtime diagnosis (WPTS verbose logs) rather than code-level analysis
- No debug logging added to production code (defer to Plan 02 when running WPTS)

## Deviations from Plan

None - plan executed exactly as written.

## Issues Encountered

None.

## User Setup Required

None - no external service configuration required.

## Next Phase Readiness

- Conformance test suite ready as regression guard for Plan 02 fixes
- All pitfalls categorized -- Plan 02 can focus on runtime diagnosis (WPTS verbose logs, macOS triage)
- Specific code locations identified for Plan 02 investigation

## Self-Check: PASSED

- FOUND: `internal/adapter/smb/crypto_state_conformance_test.go`
- FOUND: `.planning/phases/67-smb3-signing-fix/67-01-SUMMARY.md`
- FOUND: commit `89bb8e53`

---
*Phase: 67-smb3-signing-fix*
*Completed: 2026-03-20*
