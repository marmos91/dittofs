# Phase 67: SMB3 Signing Fix - Research

**Researched:** 2026-03-20
**Domain:** SMB 3.1.1 preauth integrity hash chain, signing key derivation, WPTS negotiate conformance
**Confidence:** HIGH

## Summary

The SMB 3.1.1 signing failure (GitHub #252) is caused by a mismatch between the server's and client's preauth integrity hash chains. The preauth hash is a cumulative SHA-512 hash of all NEGOTIATE and SESSION_SETUP messages (excluding the final successful SESSION_SETUP response). This hash is used as the `Context` parameter in the SP800-108 KDF to derive the signing key. Any byte-level discrepancy between what the server hashes and what the client hashes results in different signing keys, causing the client to reject the signed response.

PR #285 already refactored preauth hash tracking from connection-level to per-session (PreauthSessionTable pattern per MS-SMB2 3.3.5.5), which may have fixed the macOS issue. The triage-first approach is correct: test macOS mount against current code before deep debugging. Regardless, the 5 WPTS BVT_Negotiate_SMB311 test failures must be investigated and fixed.

**Primary recommendation:** Triage by testing macOS mount first. Then use the MS-SMB2 official test vectors (from the Microsoft blog post appendix) to write conformance unit tests that replay known-good byte sequences and assert exact hash values. Any discrepancy in the test will pinpoint the exact location of the bug.

<user_constraints>
## User Constraints (from CONTEXT.md)

### Locked Decisions
- Fix both the macOS mount_smbfs issue (#252) AND the 5 WPTS BVT_Negotiate_SMB311 failures in this phase
- PR #285 already refactored preauth hashes to per-session tracking -- macOS has NOT been retested since that merge
- **Triage first**: test macOS mount against current code before deep debugging; PR #285 may have already fixed it
- If macOS works post-PR #285, still investigate and fix the 5 WPTS BVT_Negotiate_SMB311 failures (hard requirement)
- Approach: spec-driven audit first (MS-SMB2 compliance), then validate with Samba packet captures as ground truth
- Samba reference captures via Docker container (no existing server available)
- SessionID=0 stashing logic: fix only if it's the root cause, don't refactor if working correctly
- Branch: `fix/smb3-signing` off main, running in parallel with Phase 66 (PR #286)
- Keep GMAC preference for SMB 3.1.1 (standard per MS-SMB2); CMAC fallback already works when client omits SIGNING_CAPABILITIES
- No configuration option for signing algorithm -- auto-negotiate only, per spec
- Fix the preauth hash computation, not the algorithm selection
- All 5 WPTS BVT_Negotiate_SMB311 tests must pass (hard success criterion):
  - BVT_Negotiate_SMB311
  - BVT_Negotiate_SMB311_Preauthentication_Encryption_AES_256_CCM
  - BVT_Negotiate_SMB311_Preauthentication_Encryption_AES_256_GCM
  - BVT_Negotiate_SMB311_Preauthentication_Encryption_CCM
  - BVT_Negotiate_SMB311_Preauthentication_Encryption_GCM
- macOS validation: manual `mount_smbfs` test (no CI automation)
- Windows 11 regression: existing WPTS CI + go-smb2 integration tests (no manual Windows test)
- Add automated preauth hash conformance unit tests (replay known-good byte sequences, assert hash values)
- Update KNOWN_FAILURES.md to remove the 5 entries once passing, update baseline pass/fail counts

### Claude's Discretion
- Exact debugging approach (hex-dump logging levels, Wireshark capture methodology)
- Whether to add permanent debug logging for preauth hash values vs temporary
- Internal code refactoring decisions if needed for the fix
- Docker Samba setup specifics for reference captures

### Deferred Ideas (OUT OF SCOPE)
None -- discussion stayed within phase scope
</user_constraints>

<phase_requirements>
## Phase Requirements

| ID | Description | Research Support |
|----|-------------|-----------------|
| PROTO-01 | SMB 3.1.1 signing on macOS fix | Full preauth integrity hash chain analysis, test vectors from MS-SMB2 spec, code audit of all canonical files, identified potential bug locations in `rawMessage` reconstruction and hook dispatch timing |
</phase_requirements>

## Standard Stack

### Core
| Library | Version | Purpose | Why Standard |
|---------|---------|---------|--------------|
| `crypto/sha512` | stdlib | Preauth integrity hash computation | MS-SMB2 mandates SHA-512 |
| `crypto/aes` + `crypto/cipher` | stdlib | AES-CMAC/GMAC signing | Already used in signing package |
| `crypto/hmac` + `crypto/sha256` | stdlib | SP800-108 KDF | Already used in kdf package |

### Supporting
| Library | Version | Purpose | When to Use |
|---------|---------|---------|-------------|
| `encoding/hex` | stdlib | Debug logging of hash values | Debugging preauth hash chain |
| `testing` | stdlib | Conformance test vectors | Preauth hash unit tests |

No new dependencies are needed. All cryptographic primitives are already in use.

## Architecture Patterns

### Preauth Integrity Hash Chain (MS-SMB2 3.3.5.4 / 3.3.5.5)

The preauth integrity hash chain is the core mechanism. Understanding its exact rules is critical.

**Hash computation:**
```
H(0) = 64 bytes of zeros
H(i) = SHA-512(H(i-1) || Message(i))
```

**Messages included in hash (in order):**
1. NEGOTIATE request (SMB2 header + body, no NetBIOS framing)
2. NEGOTIATE response (SMB2 header + body, no NetBIOS framing)
3. SESSION_SETUP request leg 1 (SMB2 header + body)
4. SESSION_SETUP response leg 1 (STATUS_MORE_PROCESSING_REQUIRED)
5. SESSION_SETUP request leg 2 (final authenticate)
6. **NOT** the final SESSION_SETUP response (STATUS_SUCCESS) -- this is signed with the derived key but NOT hashed into the preauth value used for derivation

**Messages NOT included:**
- SMB1 NEGOTIATE (the initial dialect probe from macOS/Windows)
- 4-byte NetBIOS session header (must be stripped)
- Final SESSION_SETUP response (STATUS_SUCCESS)

**Connection vs Session hash split:**
- Connection hash: Updated by NEGOTIATE request + NEGOTIATE response only
- Per-session hash: Initialized from connection hash after NEGOTIATE completes, updated by SESSION_SETUP request/response pairs for that session only

### Current Code Architecture

```
ReadRequest (framing.go)
  -> readNetBIOSPayload (strips 4-byte NetBIOS header)
  -> parseSMB2Message (parses header, returns hdr + body)

Connection.Serve (connection.go)
  -> Reconstructs rawMessage = hdr.Encode() + body  [LINE 190-193]
  -> Passes rawMessage to ProcessSingleRequest

ProcessSingleRequest (response.go)
  -> RunBeforeHooks(connInfo, cmd, rawMessage)  [PREAUTH HASH UPDATE]
  -> Handler execution
  -> SendResponseWithHooks -> RunAfterHooks(connInfo, cmd, rawResponse)

hooks.go:
  -> preauthHashBeforeHook: Updates connection hash with NEGOTIATE request
  -> preauthHashAfterHook: Updates connection hash with NEGOTIATE response (only if 3.1.1)
  -> sessionPreauthBeforeHook: Stashes/updates per-session hash with SESSION_SETUP request
  -> sessionPreauthAfterHook: Updates per-session hash with SESSION_SETUP response
```

### Recommended Investigation Structure
```
investigation/
  1. macOS triage test (manual mount_smbfs)
  2. WPTS BVT_Negotiate_SMB311 failure analysis
  3. MS-SMB2 spec audit of preauth hash computation
  4. Conformance unit tests with official test vectors
  5. Fix implementation
  6. Regression verification
```

### Pattern: rawMessage Reconstruction (POTENTIAL BUG)

**Critical finding:** In `connection.go` lines 190-193, `rawMessage` is reconstructed from the parsed header:

```go
rawMessage := make([]byte, header.HeaderSize+len(body))
copy(rawMessage, hdr.Encode())
copy(rawMessage[header.HeaderSize:], body)
```

The `Encode()` method hardcodes `ProtocolID` to `SMB2ProtocolID` and `StructureSize` to 64. If `Parse()` preserves all fields faithfully and `Encode()` produces identical bytes, this is correct. However, this roundtrip through Parse+Encode could theoretically alter bytes if any field is not perfectly preserved (e.g., reserved fields, padding). This needs verification but is likely safe since Parse/Encode tests show round-trip correctness.

**More importantly:** The response `rawResponse` is constructed in `SendResponseWithHooks`:
```go
rawResponse := append(respHeader.Encode(), body...)
RunAfterHooks(connInfo, reqHeader.Command, rawResponse)
```

This uses the *response* header's `Encode()` output. If the response header doesn't match what the server actually sent on the wire (e.g., if signing modified bytes after encoding), there could be a mismatch. Looking at `SendMessage`:

```go
smbPayload := append(hdr.Encode(), body...)
// ... signing modifies smbPayload[48:64] ...
// Sync signature back:
copy(hdr.Signature[:], smbPayload[48:64])
```

Then in `SendResponseWithHooks`:
```go
rawResponse := append(respHeader.Encode(), body...)
```

The `respHeader.Encode()` here would include the synced-back signature. This should be correct for the after-hook.

### Pattern: SessionID=0 Stashing

The first SESSION_SETUP request arrives with `SessionID=0` because the session hasn't been allocated yet. The before-hook stashes the raw bytes, and `InitSessionPreauthHash` consumes them after the handler allocates the session ID. This is correct per the spec, but the stash pattern introduces ordering complexity:

1. Before-hook stashes raw bytes and calls `StashPendingSessionSetup(rawMessage)`
2. Handler allocates session ID and calls `InitSessionPreauthHash(sessionID)`
3. `InitSessionPreauthHash` initializes from connection hash and consumes stashed bytes
4. After-hook updates per-session hash with response bytes

**Potential issue:** If the before-hook for SESSION_SETUP also tries `UpdateSessionPreauthHash(0, rawMessage)`, the `sessionID=0` won't match any entry in the session table, so it's a no-op. This is handled correctly in the current code.

### Anti-Patterns to Avoid
- **Including NetBIOS framing in hash:** The 4-byte NetBIOS session header MUST NOT be included. Current code strips it in `readNetBIOSPayload` before passing to `parseSMB2Message`, so this should be correct.
- **Including final SESSION_SETUP response in derivation hash:** The hash used for key derivation must NOT include the final successful response. The current code's after-hook updates the per-session hash with the response, but `configureSessionSigningWithKey` calls `GetSessionPreauthHash` before the after-hook runs (the response hasn't been built yet when the handler executes). **This ordering needs careful verification.**
- **Hashing the wrong message bytes for the response:** After signing, the response bytes change (signature field populated). The after-hook must hash the signed bytes (what was actually sent on the wire), not the unsigned bytes. Current code in `SendResponseWithHooks` rebuilds `rawResponse` from `respHeader.Encode()` after the signature has been synced back, which should be correct.

## Don't Hand-Roll

| Problem | Don't Build | Use Instead | Why |
|---------|-------------|-------------|-----|
| SHA-512 preauth hash | Custom hash chain | Existing `chainHash()` in `crypto_state.go` | Already correct per MS-SMB2 |
| SP800-108 KDF | Custom key derivation | Existing `kdf.DeriveKey()` | Already implements counter mode with HMAC-SHA256 |
| Signing algorithm dispatch | Manual if/else | Existing `signing.NewSigner()` factory | Already handles CMAC/GMAC/HMAC dispatch |
| Test vector validation | Wireshark-only debugging | Automated unit tests with MS-SMB2 appendix vectors | Reproducible, CI-friendly |

**Key insight:** The cryptographic primitives are already correct. The bug is almost certainly in the message boundary or ordering of hash updates, not in the hash/KDF/signing algorithms themselves.

## Common Pitfalls

### Pitfall 1: NetBIOS Length Prefix Leaking into Hash
**What goes wrong:** The 4-byte NetBIOS session header (`00 XX XX XX`) gets included in the preauth hash, making the server's hash diverge from the client's.
**Why it happens:** Different layers read different amounts of the TCP stream.
**How to avoid:** The current code strips NetBIOS in `readNetBIOSPayload` before creating `rawMessage`. Verify this remains true. Add a test asserting `rawMessage` starts with `0xFE 0x53 0x4D 0x42` (SMB2 protocol ID), never `0x00`.
**Warning signs:** Hash values don't match test vectors even for the first (NEGOTIATE) message.

### Pitfall 2: Final SESSION_SETUP Response Included in Derivation Hash
**What goes wrong:** The preauth hash used for key derivation includes the final SESSION_SETUP response, but the client doesn't include it because the client needs the response to verify the signature (chicken-and-egg).
**Why it happens:** The after-hook runs after sending the response and updates the per-session hash. But key derivation happens inside the handler, before the response is sent.
**How to avoid:** Verify that `GetSessionPreauthHash()` is called before `DeleteSessionPreauthHash()` in the handler, and that the after-hook's update doesn't affect the already-derived keys.
**Warning signs:** Hash matches for NEGOTIATE but diverges after SESSION_SETUP response.

### Pitfall 3: rawMessage Roundtrip Fidelity
**What goes wrong:** Reconstructing `rawMessage` from `hdr.Encode() + body` doesn't produce byte-identical output compared to the original wire bytes.
**Why it happens:** `Parse()` might not preserve all bytes (reserved fields, padding), and `Encode()` hardcodes some values.
**How to avoid:** Verify Parse/Encode round-trip produces identical bytes. Consider passing the original wire bytes through to hooks instead of reconstructing.
**Warning signs:** Test vectors fail on the first message (NEGOTIATE request hash mismatch).

### Pitfall 4: Signing the Response Before After-Hook Hashes It
**What goes wrong:** The after-hook for SESSION_SETUP response hashes the response bytes. For the STATUS_MORE_PROCESSING_REQUIRED response (intermediate leg), the response is NOT signed (no session key yet). For the final STATUS_SUCCESS response, it IS signed. The after-hook must hash what was actually sent on the wire.
**Why it happens:** Signing modifies bytes 48-63 of the message.
**How to avoid:** Current `SendResponseWithHooks` rebuilds `rawResponse` from `respHeader.Encode()` after signature sync-back. Verify the signature is synced back to `respHeader.Signature` before the after-hook runs.
**Warning signs:** Intermediate SESSION_SETUP responses hash correctly but the final one doesn't.

### Pitfall 5: WPTS Test Expectations vs DittoFS Behavior
**What goes wrong:** The 5 BVT_Negotiate_SMB311 tests may fail for reasons unrelated to the preauth hash (e.g., missing negotiate contexts, wrong response format, capability flags).
**Why it happens:** These tests validate the entire negotiate flow, not just signing.
**How to avoid:** Run WPTS with verbose output and examine the exact assertion failure message for each test. Don't assume all 5 fail for the same reason.
**Warning signs:** Some tests fail before SESSION_SETUP is even attempted.

## Code Examples

### MS-SMB2 Preauth Hash Test Vector (from official Microsoft blog)

This is the most critical code example -- use these exact bytes for conformance tests.

```go
// Source: MS-SMB2 blog post "SMB 3.1.1 Pre-authentication integrity in Windows 10"
// Appendix A.1 - test vector for preauth integrity computation

func TestPreauthHashConformance_NegotiateRequest(t *testing.T) {
    cs := NewConnectionCryptoState()

    // Negotiate request packet (from MS-SMB2 test vector)
    // Starts with FE 53 4D 42 (SMB2 protocol ID) -- no NetBIOS framing
    negotiateReq, _ := hex.DecodeString(
        "FE534D4240000100000000000000800000000000000000000100000000000000" +
        "FFFE000000000000000000000000000000000000000000000000000000000000" +
        "24000500000000003F000000ECD86F326276024F9F7752B89BB33F3A70000000" +
        "020000000202100200030203110300000100260000000000010020000100FA49" +
        "E6578F1F3A9F4CD3E9CC14A67AA884B3D05844E0E5A118225C15887F32FF0000" +
        "0200060000000000020002000100")

    cs.UpdatePreauthHash(negotiateReq)
    hash := cs.GetPreauthHash()

    // Expected hash after NEGOTIATE request (from test vector)
    expected, _ := hex.DecodeString(
        "DD94EFC5321BB618A2E208BA8920D2F422992526947A409B5037DE1E0FE8C736" +
        "2B8C47122594CDE0CE26AA9DFC8BCDBDE0621957672623351A7540F1E54A0426")

    if !bytes.Equal(hash[:], expected) {
        t.Errorf("NEGOTIATE request hash mismatch\ngot:  %x\nwant: %x", hash[:], expected)
    }
}
```

### Preauth Hash Chain Order Verification

```go
// Verify the complete chain: NEGOTIATE req -> NEGOTIATE resp -> SESSION_SETUP req
// -> SESSION_SETUP resp (MORE_PROCESSING) -> SESSION_SETUP req (final)
// The hash after the final SESSION_SETUP request is used for key derivation.
// The final SESSION_SETUP response (STATUS_SUCCESS) is NOT included.

func TestPreauthHashChain_FullSession(t *testing.T) {
    cs := NewConnectionCryptoState()

    // 1. Hash NEGOTIATE request into connection hash
    cs.UpdatePreauthHash(negotiateReqBytes)
    // 2. Hash NEGOTIATE response into connection hash
    cs.UpdatePreauthHash(negotiateRespBytes)

    // 3. Init per-session hash from connection hash
    sessionID := uint64(0x17592186044441)
    cs.StashPendingSessionSetup(sessionSetupReq1Bytes)
    cs.InitSessionPreauthHash(sessionID)

    // 4. Hash SESSION_SETUP response (MORE_PROCESSING_REQUIRED)
    cs.UpdateSessionPreauthHash(sessionID, sessionSetupResp1Bytes)

    // 5. Hash final SESSION_SETUP request
    cs.UpdateSessionPreauthHash(sessionID, sessionSetupReq2Bytes)

    // 6. Get hash for key derivation -- BEFORE hashing the final response
    derivationHash := cs.GetSessionPreauthHash(sessionID)

    // This hash should match the test vector value
    // It is used as Context in KDF: SigningKey = KDF(SessionKey, "SMBSigningKey\0", derivationHash)
}
```

## State of the Art

| Old Approach | Current Approach | When Changed | Impact |
|--------------|------------------|--------------|--------|
| Connection-level preauth hash for all sessions | Per-session preauth hash (PreauthSessionTable) | PR #285 (2026-03-18) | Correct per MS-SMB2 3.3.5.5; may have fixed macOS issue |
| SMB 3.0 Secure Dialect Negotiation (VALIDATE_NEGOTIATE_INFO) | SMB 3.1.1 Preauth Integrity Hash Chain | SMB 3.1.1 spec | End-to-end integrity of pre-authentication messages |
| Single signing algorithm (HMAC-SHA256) | Negotiated signing (CMAC/GMAC via SIGNING_CAPABILITIES) | Phase 34 | Allows GMAC for better performance with SMB 3.1.1 |

**PR #285 changes that are relevant:**
- Moved from connection-level to per-session preauth hash tracking
- Added `StashPendingSessionSetup()` for SessionID=0 edge case
- Added `InitSessionPreauthHash()` to consume stashed bytes
- SESSION_SETUP hooks now update per-session hashes, not connection hash
- These changes may have already fixed the macOS issue but haven't been retested

## Open Questions

1. **Has PR #285 already fixed the macOS issue?**
   - What we know: PR #285 fixed per-session vs connection hash tracking, which was a spec violation
   - What's unclear: Whether the original macOS failure was due to this specific issue or a different preauth hash bug
   - Recommendation: Triage by testing macOS mount first (5 minutes), saves potentially hours of debugging

2. **What exactly causes the 5 WPTS BVT_Negotiate_SMB311 failures?**
   - What we know: They're in the "Negotiate" category, related to SMB 3.1.1 preauthentication/encryption
   - What's unclear: Whether they fail due to preauth hash issues, negotiate context formatting, or something else entirely
   - Recommendation: Run WPTS with verbose output to capture exact failure messages before implementing any fix

3. **Does rawMessage reconstruction produce byte-identical output?**
   - What we know: `hdr.Encode()` round-trips correctly in tests, but the connection code reconstructs rawMessage from parsed components
   - What's unclear: Whether any edge case (compound requests, special flags) could cause divergence
   - Recommendation: Add an assertion comparing reconstructed rawMessage with original wire bytes in debug mode

## Validation Architecture

### Test Framework
| Property | Value |
|----------|-------|
| Framework | Go testing (stdlib) |
| Config file | None needed |
| Quick run command | `go test ./internal/adapter/smb/... -run TestPreauth -v` |
| Full suite command | `go test ./...` |

### Phase Requirements to Test Map
| Req ID | Behavior | Test Type | Automated Command | File Exists? |
|--------|----------|-----------|-------------------|-------------|
| PROTO-01a | Preauth hash conformance with MS-SMB2 test vectors | unit | `go test ./internal/adapter/smb/ -run TestPreauthHashConformance -v` | Wave 0 |
| PROTO-01b | KDF produces correct signing key from preauth hash | unit | `go test ./internal/adapter/smb/kdf/ -run TestKDF -v` | Existing |
| PROTO-01c | Full preauth chain (negotiate + session setup) | unit | `go test ./internal/adapter/smb/ -run TestPreauthChain -v` | Wave 0 |
| PROTO-01d | WPTS BVT_Negotiate_SMB311 (all 5 variants) | integration | WPTS CI (Docker) | Existing |
| PROTO-01e | macOS mount_smbfs with SMB 3.1.1 signing | manual-only | `sudo mount_smbfs smb://user@host/share /mnt` | N/A (manual) |
| PROTO-01f | Windows 11 signing regression | integration | WPTS CI + go-smb2 tests | Existing |

### Sampling Rate
- **Per task commit:** `go test ./internal/adapter/smb/... -v -count=1`
- **Per wave merge:** `go test ./... && go vet ./...`
- **Phase gate:** Full suite green + 5 WPTS BVT_Negotiate_SMB311 pass + macOS manual test

### Wave 0 Gaps
- [ ] `internal/adapter/smb/crypto_state_conformance_test.go` -- preauth hash conformance tests using MS-SMB2 official test vectors (covers PROTO-01a, PROTO-01c)
- [ ] Framework install: none needed (Go stdlib testing)

## Sources

### Primary (HIGH confidence)
- MS-SMB2 official test vectors blog post: https://learn.microsoft.com/en-us/archive/blogs/openspecification/smb-3-1-1-pre-authentication-integrity-in-windows-10 -- Complete preauth integrity hash test vectors with expected intermediate values (Appendix A.1, A.2, B)
- MS-SMB2 Section 3.3.5.5 (Handling a New Authentication): https://learn.microsoft.com/en-us/openspecs/windows_protocols/ms-smb2/7fd079ca-17e6-4f02-8449-46b606ea289c -- Per-session preauth hash rules
- MS-SMB2 Section 3.2.5.3.1 -- SigningKey derivation: `KDF(SessionKey, "SMBSigningKey\0", PreauthIntegrityHashValue)`
- GitHub issue #252 -- Root cause analysis with 4 common pitfalls documented
- Codebase: All canonical reference files from CONTEXT.md read and analyzed

### Secondary (MEDIUM confidence)
- MS-SMB2 2.2.3 NEGOTIATE request/response format -- negotiate context encoding rules
- MS-SMB2 Per Session state: https://learn.microsoft.com/en-us/openspecs/windows_protocols/ms-smb2/fbcbc952-8c1f-4528-a0ab-7aed7d52264e -- PreauthSessionTable definition

### Tertiary (LOW confidence)
- None -- all findings verified against official Microsoft documentation or codebase

## Metadata

**Confidence breakdown:**
- Standard stack: HIGH - No new libraries needed, all cryptographic primitives already in use
- Architecture: HIGH - Full code audit of all canonical files completed, message flow traced end-to-end
- Pitfalls: HIGH - MS-SMB2 official test vectors provide byte-level verification capability, 4 pitfalls from issue #252 documented

**Research date:** 2026-03-20
**Valid until:** 2026-04-20 (stable -- MS-SMB2 spec doesn't change frequently)
