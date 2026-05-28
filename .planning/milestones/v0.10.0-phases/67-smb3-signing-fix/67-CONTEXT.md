# Phase 67: SMB3 Signing Fix - Context

**Gathered:** 2026-03-20
**Status:** Ready for planning

<domain>
## Phase Boundary

Fix the preauth integrity hash mismatch that causes macOS `mount_smbfs` to reject SMB 3.1.1 sessions with signing enabled (#252). Additionally resolve the 5 WPTS BVT_Negotiate_SMB311 known failures which share the same negotiate/preauth root cause area. Windows 11 SMB 3.1.1 signing must not regress.

</domain>

<decisions>
## Implementation Decisions

### Fix scope
- Fix both the macOS mount_smbfs issue (#252) AND the 5 WPTS BVT_Negotiate_SMB311 failures in this phase
- PR #285 already refactored preauth hashes to per-session tracking — macOS has NOT been retested since that merge
- **Triage first**: test macOS mount against current code before deep debugging; PR #285 may have already fixed it
- If macOS works post-PR #285, still investigate and fix the 5 WPTS BVT_Negotiate_SMB311 failures (hard requirement)
- Approach: spec-driven audit first (MS-SMB2 compliance), then validate with Samba packet captures as ground truth
- Samba reference captures via Docker container (no existing server available)
- SessionID=0 stashing logic: fix only if it's the root cause, don't refactor if working correctly
- Branch: `fix/smb3-signing` off main, running in parallel with Phase 66 (PR #286)

### Signing algorithm policy
- Keep GMAC preference for SMB 3.1.1 (standard per MS-SMB2); CMAC fallback already works when client omits SIGNING_CAPABILITIES
- No configuration option for signing algorithm — auto-negotiate only, per spec
- Fix the preauth hash computation, not the algorithm selection

### Testing & regression
- All 5 WPTS BVT_Negotiate_SMB311 tests must pass (hard success criterion, not best-effort):
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

</decisions>

<canonical_refs>
## Canonical References

**Downstream agents MUST read these before planning or implementing.**

### SMB3 signing and preauth integrity
- `internal/adapter/smb/crypto_state.go` — Per-session preauth hash chain implementation (recently refactored in PR #285)
- `internal/adapter/smb/hooks.go` — Dispatch hooks for preauth hash updates (NEGOTIATE + SESSION_SETUP)
- `internal/adapter/smb/kdf/kdf.go` — SP800-108 Counter Mode KDF for key derivation
- `internal/adapter/smb/signing/` — AES-CMAC, AES-GMAC, HMAC-SHA256 signer implementations
- `internal/adapter/smb/v2/handlers/session_setup.go` — Session setup flow, key derivation, preauth hash consumption
- `internal/adapter/smb/v2/handlers/negotiate.go` — Negotiate handler with context processing
- `internal/adapter/smb/session/crypto_state.go` — Session-level crypto state and DeriveAllKeys
- `internal/adapter/smb/framing.go` — Message signing verification (request path)
- `internal/adapter/smb/response.go` — Message signing (response path)

### GitHub issue
- GitHub #252 — Full root cause analysis, common pitfalls, debugging approach

### WPTS conformance
- `test/smb-conformance/KNOWN_FAILURES.md` — Current known failures including 5 BVT_Negotiate_SMB311 entries
- `test/smb-conformance/baseline-results.md` — Pass/fail baseline counts to update

### MS-SMB2 spec sections
- MS-SMB2 3.3.5.4 — Preauth integrity hash chain computation
- MS-SMB2 3.3.5.5 — Per-session preauth hash (PreauthSessionTable)
- MS-SMB2 3.2.5.3.1 — Signing key derivation with preauth hash as context

</canonical_refs>

<code_context>
## Existing Code Insights

### Reusable Assets
- `ConnectionCryptoState` with per-session preauth hash tracking (recently added in PR #285)
- `chainHash()` helper for SHA-512 preauth integrity computation
- `StashPendingSessionSetup()` for SessionID=0 edge case handling
- `kdf.DeriveKey()` with SP800-108 Counter Mode and dialect-aware label/context
- `signing.NewSigner()` factory with CMAC/GMAC/HMAC dispatch
- Docker-based smbtorture infrastructure for WPTS testing
- go-smb2 integration test framework

### Established Patterns
- Dispatch hooks (before/after) for cross-cutting concerns like signing and preauth hash
- Per-session crypto state with `DeriveAllKeys()` orchestrating key derivation
- `configureSessionSigningWithKey()` as the signing configuration entry point
- KNOWN_FAILURES.md + baseline-results.md for tracking conformance progress

### Integration Points
- `hooks.go` before/after hooks are the entry points for preauth hash updates
- `session_setup.go:configureSessionSigningWithKey()` is where derived keys become active
- `framing.go` verification and `response.go` signing use the session's signer
- WPTS test infrastructure in `test/smb-conformance/` for automated conformance validation

</code_context>

<specifics>
## Specific Ideas

- Issue #252 lists 4 common pitfalls to check: (1) NetBIOS length prefix must NOT be included in preauth hash, (2) final SESSION_SETUP response is NOT in the preauth hash used for key derivation, (3) hash state uses final SESSION_SETUP request (not response), (4) SMB1 negotiate not included
- PR #285 already fixed the per-session vs connection hash issue — the remaining bug may be in message boundary (NetBIOS prefix leaking) or response inclusion
- The "stash" pattern for SessionID=0 in PR #285 is new and could have subtle bugs

</specifics>

<deferred>
## Deferred Ideas

None — discussion stayed within phase scope

</deferred>

---

*Phase: 67-smb3-signing-fix*
*Context gathered: 2026-03-20*
