# Phase 69: SMB Protocol Foundation - Research

**Researched:** 2026-03-20
**Domain:** SMB 3.1.1 signing conformance, MS-SMB2 credit flow control
**Confidence:** HIGH

## Summary

Phase 69 has two primary deliverables: (1) fix macOS SMB 3.1.1 signing by absorbing PR #288 and conducting a full signing audit, and (2) implement MS-SMB2 credit flow control with charge validation, sequence number window tracking, credit granting enforcement, multi-credit I/O validation, and compound credit accounting.

The existing DittoFS SMB codebase has excellent scaffolding for both areas. PR #288 already fixes the core macOS signing issue (enforcing `NegSigningRequired` for 3.1.1 in both NEGOTIATE and SESSION_SETUP). The credit subsystem has three strategies (Fixed/Echo/Adaptive) fully implemented with `GrantCredits()` wired into every response path, but `ConsumeCredits()` is called only inside `GrantCredits` -- there is NO validation that a client actually HAS sufficient credits before dispatch. The sequence number window (CommandSequenceWindow per MS-SMB2 3.3.1.7) is entirely absent. This is the core gap.

**Primary recommendation:** Absorb PR #288, then implement a `CreditValidator` interceptor invoked in `ProcessSingleRequest` and `ProcessCompoundRequest` before handler dispatch. The validator maintains a per-connection `CommandSequenceWindow` (bitmap-based set of valid MessageIds), validates CreditCharge against payload size per MS-SMB2 3.3.5.2.5, and consumes sequence numbers from the window. Credit granting (already wired) expands the window on every response.

<user_constraints>
## User Constraints (from CONTEXT.md)

### Locked Decisions
- Absorb PR #288 (`fix/smb3-signing`) into Phase 69 as SMB-01 deliverable -- enforces NegSigningRequired for SMB 3.1.1
- Conduct full signing audit beyond PR #288 scope: cover every MS-SMB2 section 3.3.x that mentions signing (negotiate, session setup, tree connect, re-auth, session binding, compound signing)
- Fix everything found in the audit, regardless of effort -- no deferral to Phase 72
- Triple-reference approach: verify against MS-SMB2 spec, Samba source, and Windows kernel behavior
- Strict credit enforcement per spec: reject all requests with insufficient CreditCharge -- STATUS_INVALID_PARAMETER per MS-SMB2 3.3.5.2.5
- Reject request only: return error on the specific request, do NOT disconnect the connection
- Per-session tracking: credit balance tracked at session level (future multi-channel ready for Phase 74)
- Full sequence number window (MessageId sliding window per MS-SMB2 3.3.5.2.5): track granted sequence numbers, reject duplicates and out-of-range IDs
- Pre-session commands exempt: NEGOTIATE and first SESSION_SETUP bypass credit checks (client starts with 0 credits, per MS-SMB2 3.3.5.1)
- Hard floor of 1 credit: never grant fewer than 1 credit in any response (prevents client deadlock per MS-SMB2 3.3.1.2)
- No Prometheus credit metrics for now
- Adaptive strategy as default for new deployments
- Adapter-level configuration only via dfsctl -- not per-share
- Remove credit-related fields from config.yaml -- clean break
- Compound-level validation only: validate total credits for entire compound, not per-command
- First command charges, last grants: per MS-SMB2 3.2.4.1.4

### Claude's Discretion
- Credit validation architecture (separate interceptor vs prepareDispatch integration vs hook-based)
- Exact credit configuration field names and API shape for dfsctl adapter settings
- Internal refactoring of existing credit code to support enforcement
- Error message text in credit validation failures

### Deferred Ideas (OUT OF SCOPE)
- Per-share credit throttling -- conflicts with MS-SMB2's per-session credit model
- Prometheus credit metrics -- skip for now
- Session binding signing tests -- requires multi-channel (Phase 74)
- IPC async credit tests -- requires ChangeNotify (Phase 72)
</user_constraints>

<phase_requirements>
## Phase Requirements

| ID | Description | Research Support |
|----|-------------|-----------------|
| SMB-01 | SMB 3.1.1 signing works correctly on macOS | PR #288 absorb + signing audit against MS-SMB2 3.3.5.4/3.3.5.5 and all 3.3.x signing sections |
| SMB-02 | Server enforces credit charge validation before dispatching requests | CreditValidator interceptor with per-connection CommandSequenceWindow + CreditCharge validation per MS-SMB2 3.3.5.2.5 |
| SMB-03 | Server grants credits in every response, never reducing client to zero | Existing GrantCredits() in every response path; enforce MinimumCreditGrant=1 hard floor; compound grants only in last response |
| SMB-04 | Multi-credit I/O operations validate CreditCharge = ceil(length/65536) | Existing CalculateCreditCharge() validates against payload size; wire into CreditValidator for READ/WRITE |
| SMB-05 | Compound requests handle credit accounting correctly | Compound-level validation: first command CreditCharge covers whole compound; middle responses grant 0; last response grants credits |
</phase_requirements>

## Standard Stack

### Core
| Library | Version | Purpose | Why Standard |
|---------|---------|---------|--------------|
| Go stdlib | 1.25.5 | All SMB protocol code | Project uses pure Go, no external SMB libraries |
| sync/atomic | stdlib | Thread-safe credit counters | Already used extensively in session/manager.go |
| sync.Mutex | stdlib | Protect CommandSequenceWindow mutations | Sequence window needs exclusive access |

### Supporting
| Library | Version | Purpose | When to Use |
|---------|---------|---------|-------------|
| github.com/hirochachacha/go-smb2 | latest | gosmb2 client for integration tests | For signing and credit edge case tests |
| testify | v1.9+ | Test assertions | Already used across project |

### Alternatives Considered
| Instead of | Could Use | Tradeoff |
|------------|-----------|----------|
| Bitmap-based CommandSequenceWindow | Simple set/map | Bitmap is more memory-efficient for large windows but map is simpler; with max 65535 credits the memory difference is negligible -- use bitmap for cache locality |
| Separate CreditValidator type | Inline validation in prepareDispatch | Separate type is cleaner for testing and follows existing pattern of session.Manager |

## Architecture Patterns

### Recommended Project Structure
```
internal/adapter/smb/
├── session/
│   ├── credits.go          # Existing: CreditConfig, strategies, CalculateCreditCharge
│   ├── manager.go          # Existing: GrantCredits, credit tracking -- refactor
│   ├── session.go          # Existing: Session with credit state
│   ├── sequence_window.go  # NEW: CommandSequenceWindow bitmap implementation
│   └── sequence_window_test.go # NEW: Comprehensive sequence window tests
├── response.go             # MODIFY: Wire credit validation before dispatch
├── compound.go             # MODIFY: Compound-level credit accounting
├── hooks.go                # Existing: Before/after hooks (potentially for credit hooks)
├── signing/                # Existing: Signing implementations -- audit and fix
└── framing.go              # Existing: Signature verification -- audit
```

### Pattern 1: CreditValidator Interceptor
**What:** A CreditValidator struct that wraps per-connection credit state (CommandSequenceWindow, pending credit balance) and provides `ValidateAndConsume(header)` and `GrantAndExpand(header, grantedCredits)` methods.
**When to use:** Called in `ProcessSingleRequest()` before `prepareDispatch()` and in `ProcessCompoundRequest()` before processing the first command.
**Rationale:** Separate type enables thorough unit testing without needing a full SMB server. Follows the existing session.Manager pattern.
**Example:**
```go
// sequence_window.go in session package
type CommandSequenceWindow struct {
    mu       sync.Mutex
    // Low watermark: smallest valid sequence number
    low      uint64
    // Bitmap tracking which sequence numbers in [low, low+windowSize) are available
    // A set bit means the sequence number is available (not yet consumed)
    bitmap   []uint64 // Each uint64 tracks 64 sequence numbers
    size     uint64   // Current window size (= total granted credits)
}

// NewCommandSequenceWindow creates a window initialized with sequence {0}.
func NewCommandSequenceWindow() *CommandSequenceWindow {
    w := &CommandSequenceWindow{
        low:    0,
        bitmap: make([]uint64, 1), // Start with 64 bits
        size:   1,
    }
    w.bitmap[0] = 1 // Sequence 0 is available
    return w
}

// Consume validates and removes CreditCharge consecutive sequence numbers
// starting from messageId. Returns false if any are out of range or already consumed.
func (w *CommandSequenceWindow) Consume(messageId uint64, creditCharge uint16) bool {
    w.mu.Lock()
    defer w.mu.Unlock()

    charge := uint64(creditCharge)
    if charge == 0 {
        charge = 1
    }

    // Check all sequence numbers in range [messageId, messageId+charge) are valid
    for i := uint64(0); i < charge; i++ {
        seq := messageId + i
        if seq < w.low || seq >= w.low+w.size {
            return false // Out of window
        }
        idx := (seq - w.low) / 64
        bit := (seq - w.low) % 64
        if idx >= uint64(len(w.bitmap)) || w.bitmap[idx]&(1<<bit) == 0 {
            return false // Already consumed or not available
        }
    }

    // Consume: clear bits
    for i := uint64(0); i < charge; i++ {
        seq := messageId + i
        idx := (seq - w.low) / 64
        bit := (seq - w.low) % 64
        w.bitmap[idx] &^= (1 << bit)
    }

    // Advance low watermark past consumed sequence numbers
    w.advanceLow()
    return true
}

// Grant expands the window by adding `count` new sequence numbers.
func (w *CommandSequenceWindow) Grant(count uint16) {
    w.mu.Lock()
    defer w.mu.Unlock()

    newHigh := w.low + w.size + uint64(count)
    // Expand bitmap if needed and set bits for new sequence numbers
    for seq := w.low + w.size; seq < newHigh; seq++ {
        idx := (seq - w.low) / 64
        for idx >= uint64(len(w.bitmap)) {
            w.bitmap = append(w.bitmap, 0)
        }
        bit := (seq - w.low) % 64
        w.bitmap[idx] |= (1 << bit)
    }
    w.size += uint64(count)
}
```

### Pattern 2: Credit Validation in ProcessSingleRequest
**What:** Insert credit validation between `RunBeforeHooks()` and `prepareDispatch()`.
**When to use:** Every single (non-compound) request.
**Example:**
```go
// In response.go ProcessSingleRequest, after RunBeforeHooks:

// Credit validation: exempt pre-session commands
if !isCreditExempt(reqHeader.Command, reqHeader.SessionID) {
    // 1. Validate CreditCharge vs payload size (MS-SMB2 3.3.5.2.5)
    if err := validateCreditCharge(reqHeader, body); err != nil {
        return SendErrorResponse(reqHeader, types.StatusInvalidParameter, connInfo)
    }
    // 2. Validate and consume sequence numbers from window
    charge := effectiveCreditCharge(reqHeader)
    if !connInfo.SequenceWindow.Consume(reqHeader.MessageID, charge) {
        // Per MS-SMB2 3.3.5.2.3: invalid MessageId range
        return SendErrorResponse(reqHeader, types.StatusInvalidParameter, connInfo)
    }
}
```

### Pattern 3: Compound Credit Accounting
**What:** For compound requests, validate total credits at compound level. Only last response grants credits; middle responses grant 0.
**When to use:** `ProcessCompoundRequest()`.
**Example:**
```go
// In compound.go, before processing commands:
// The first command's CreditCharge covers the entire compound.
// Validate and consume from sequence window once for the first command.
if !isCreditExempt(firstHeader.Command, firstHeader.SessionID) {
    if err := validateCreditCharge(firstHeader, firstBody); err != nil {
        // fail entire compound
    }
    charge := effectiveCreditCharge(firstHeader)
    if !connInfo.SequenceWindow.Consume(firstHeader.MessageID, charge) {
        // fail entire compound
    }
}

// When building responses:
// - Middle responses: set Credits = 0 (no grant)
// - Last response: grant credits via SessionManager.GrantCredits()
// Then expand sequence window by granted amount
```

### Anti-Patterns to Avoid
- **Per-command credit validation in compounds:** MS-SMB2 3.2.4.1.4 specifies compound-level accounting. Do not validate CreditCharge per sub-command within a compound.
- **Disconnecting on credit violations:** User decision says reject request only, do NOT disconnect. The spec says the server SHOULD terminate for invalid MessageId, but user explicitly overrides to error-only.
- **Granting 0 credits:** Never grant 0 credits in any response (except middle compound responses). The hard floor of 1 prevents deadlock.
- **Credit-checking CANCEL:** Per MS-SMB2 3.3.5.2.3, SMB2 CANCEL does not consume credits and must skip all credit validation.
- **Credit-checking NEGOTIATE:** NEGOTIATE and first SESSION_SETUP (SessionID=0) are exempt from credit checks since the client starts with 0 credits.

## Don't Hand-Roll

| Problem | Don't Build | Use Instead | Why |
|---------|-------------|-------------|-----|
| Sequence number tracking | Simple counter | Bitmap-based sliding window | Must support out-of-order consumption, duplicate detection, and multi-credit range consumption |
| Credit charge calculation | Custom formula | Existing `CalculateCreditCharge()` in session/credits.go | Already implements `ceil(bytes/65536)` correctly |
| Credit granting strategies | New strategy code | Existing session.Manager.GrantCredits() with 3 strategies | Fully implemented and wired into every response path |
| Signing algorithms | New signing code | Existing signing/ package (HMAC-SHA256, AES-CMAC, AES-GMAC) | All three algorithms implemented and tested |
| gosmb2 test client | Custom SMB client | hirochachacha/go-smb2 library | For integration tests verifying signing/credit behavior |

**Key insight:** The credit infrastructure is 80% built. The missing 20% is enforcement (validation before dispatch) and the sequence number window. Do not rewrite the granting system -- wire validation around it.

## Common Pitfalls

### Pitfall 1: Sequence Window Memory Growth
**What goes wrong:** The CommandSequenceWindow bitmap grows unbounded as credits are granted over time.
**Why it happens:** If the low watermark doesn't advance (client never sends low-numbered requests), the bitmap keeps growing.
**How to avoid:** (a) Advance the low watermark aggressively past consumed ranges. (b) Cap the maximum window range at 2x MaxSessionCredits per the MS-SMB2 server guidance: "Windows-based servers will limit the maximum range of sequence numbers to 2 * the number of credits granted." (c) Stop granting credits if the window range exceeds the cap.
**Warning signs:** Memory growth correlated with long-running sessions.

### Pitfall 2: CreditCharge=0 for Pre-2.1 Behavior
**What goes wrong:** SMB 2.0.2 clients send CreditCharge=0 for all requests including large I/O.
**Why it happens:** CreditCharge was introduced in SMB 2.1. For 2.0.2, CreditCharge is always 0 and means "1 credit."
**How to avoid:** Treat CreditCharge=0 as equivalent to CreditCharge=1 for all sequence window operations. The payload-size validation (MS-SMB2 3.3.5.2.5) only applies when `Connection.SupportsMultiCredit` is TRUE (SMB 2.1+).
**Warning signs:** 2.0.2 clients getting STATUS_INVALID_PARAMETER on perfectly valid requests.

### Pitfall 3: Compound Credit Double-Counting
**What goes wrong:** Credits are consumed per-command within a compound, draining the client's credit balance far faster than expected.
**Why it happens:** Treating each compound sub-command as an independent credit consumer.
**How to avoid:** Per MS-SMB2 3.2.4.1.4, the first command's CreditCharge covers the entire compound. Only consume once from the sequence window for the first command. Middle responses grant 0 credits. Last response grants all credits.
**Warning signs:** Compound requests failing with insufficient credits after the first sub-command.

### Pitfall 4: macOS Signing Regression on Non-3.1.1 Dialects
**What goes wrong:** PR #288 forces NegSigningRequired for 3.1.1 but could accidentally affect 3.0/3.0.2 sessions.
**Why it happens:** The signing audit may overshoot and make signing required for all dialects.
**How to avoid:** The NegSigningRequired flag should be conditioned on `selectedDialect == types.Dialect0311` in NEGOTIATE and `dialect == types.Dialect0311` in SESSION_SETUP. For 3.0/3.0.2, honor the configured SigningConfig.Required setting.
**Warning signs:** Windows 10 (SMB 3.0) clients reporting signing errors.

### Pitfall 5: Credit Starvation on Error Responses
**What goes wrong:** Client reaches 0 credits and deadlocks because error responses don't grant credits.
**Why it happens:** If error paths skip credit granting, the client has no way to send the next request.
**How to avoid:** Every response path (success, error, async interim) MUST grant at least 1 credit. The existing `SendErrorResponse()` already calls `GrantCredits()` -- verify this is never bypassed. The sequence window must be expanded on every response, not just successful ones.
**Warning signs:** Client hangs after receiving an error response.

### Pitfall 6: Sequence Window Not Expanding on Error Responses
**What goes wrong:** The sequence window consumes credits on request receipt but never expands on error responses, eventually exhausting all available MessageIds.
**Why it happens:** Window expansion is only wired to success paths.
**How to avoid:** Call `SequenceWindow.Grant()` for EVERY response sent, using the credits from the response header. This includes error responses, async interim responses, and compound error responses.
**Warning signs:** Client runs out of valid MessageIds after a burst of errors.

## Code Examples

### Example 1: Credit Validation Helper
```go
// isCreditExempt returns true for commands that bypass credit checks.
// Per MS-SMB2 3.3.5.1 and 3.3.5.2.3.
func isCreditExempt(command types.Command, sessionID uint64) bool {
    // CANCEL never consumes credits
    if command == types.SMB2Cancel {
        return true
    }
    // NEGOTIATE always exempt (client has 0 credits)
    if command == types.SMB2Negotiate {
        return true
    }
    // First SESSION_SETUP (SessionID=0) is exempt
    if command == types.SMB2SessionSetup && sessionID == 0 {
        return true
    }
    return false
}

// effectiveCreditCharge returns the actual credit charge for a request.
// CreditCharge=0 means 1 credit (SMB 2.0.2 compat).
func effectiveCreditCharge(hdr *header.SMB2Header) uint16 {
    if hdr.CreditCharge == 0 {
        return 1
    }
    return hdr.CreditCharge
}

// validateCreditCharge validates CreditCharge against payload/response size.
// Per MS-SMB2 3.3.5.2.5 -- only when Connection.SupportsMultiCredit is TRUE.
func validateCreditCharge(hdr *header.SMB2Header, body []byte) error {
    // Only validate for commands with variable-length payloads
    payloadSize := getPayloadSize(hdr.Command, body)
    if payloadSize == 0 {
        return nil // No payload validation needed
    }

    expectedCharge := session.CalculateCreditCharge(uint32(payloadSize))
    if hdr.CreditCharge == 0 && payloadSize > session.CreditUnitSize {
        return fmt.Errorf("CreditCharge=0 with payload %d > 64KB", payloadSize)
    }
    if hdr.CreditCharge > 0 && expectedCharge > hdr.CreditCharge {
        return fmt.Errorf("CreditCharge %d insufficient for payload %d (need %d)",
            hdr.CreditCharge, payloadSize, expectedCharge)
    }
    return nil
}
```

### Example 2: ConnInfo Extension for Sequence Window
```go
// In ConnInfo struct (internal/adapter/smb/conn.go or similar):
type ConnInfo struct {
    // ... existing fields ...

    // SequenceWindow tracks granted MessageIds per MS-SMB2 3.3.1.1.
    // Initialized with {0} on connection establishment.
    // Expanded by GrantCredits on every response.
    SequenceWindow *session.CommandSequenceWindow

    // SupportsMultiCredit is true for SMB 2.1+ connections.
    // Set during NEGOTIATE based on negotiated dialect.
    SupportsMultiCredit bool
}
```

### Example 3: Compound Credit Accounting in sendCompoundResponses
```go
// Modified sendCompoundResponses for credit accounting:
func sendCompoundResponses(responses []compoundResponse, connInfo *ConnInfo) error {
    // ...existing code...

    for i := range responses {
        if i < len(responses)-1 {
            // Middle responses: grant 0 credits
            responses[i].respHeader.Credits = 0
        }
        // Last response: credits already set by buildResponseHeaderAndBody
        // which called SessionManager.GrantCredits()
    }

    // Expand sequence window by credits granted in last response
    if lastCredits := responses[len(responses)-1].respHeader.Credits; lastCredits > 0 {
        connInfo.SequenceWindow.Grant(lastCredits)
    }

    // ...rest of existing code...
}
```

## State of the Art

| Old Approach | Current Approach | When Changed | Impact |
|--------------|------------------|--------------|--------|
| No credit enforcement | Credit validation per MS-SMB2 3.3.5.2.5 | This phase | Prevents resource abuse, passes smbtorture credit tests |
| No sequence number tracking | Bitmap-based CommandSequenceWindow | This phase | Detects duplicate/out-of-range MessageIds |
| SigningRequired=false for 3.1.1 | SigningRequired=true for 3.1.1 (PR #288) | This phase | macOS mount_smbfs works |
| Credit config in config.yaml | Credit config via dfsctl adapter settings only | This phase | Cleaner separation |

**Deprecated/outdated:**
- Credit-related fields in config.yaml: Will be removed and moved to control plane adapter settings exclusively

## Open Questions

1. **Sequence window per-connection vs per-session?**
   - What we know: MS-SMB2 3.3.1.1 says `Connection.CommandSequenceWindow` -- it is per-connection. However, user decision says "per-session tracking" for multi-channel readiness (Phase 74).
   - What's unclear: For Phase 69 (single connection per session), per-connection and per-session are equivalent. For Phase 74, multi-channel sessions share credits across connections.
   - Recommendation: Implement per-connection for now (matches spec). In Phase 74, migrate to per-session window shared across connection channels.

2. **Payload size extraction for CreditCharge validation**
   - What we know: MS-SMB2 3.3.5.2.5 validates against "payload size or maximum response size." For READ, the max response size is in the request's Length field. For WRITE, it's the data length.
   - What's unclear: Exactly which commands need payload-based validation vs always CreditCharge=1.
   - Recommendation: Validate READ (response size from Length field), WRITE (data length), IOCTL (MaxOutputResponse), and QUERY_DIRECTORY (OutputBufferLength). All other commands default to CreditCharge=1.

3. **What specific signing sections need audit beyond PR #288?**
   - What we know: PR #288 covers NEGOTIATE and SESSION_SETUP. Context requires covering "every MS-SMB2 section 3.3.x that mentions signing."
   - What's unclear: Full enumeration of spec sections.
   - Recommendation: Audit sections 3.3.5.2.4 (signing verification), 3.3.5.4 (NEGOTIATE), 3.3.5.5 (SESSION_SETUP), 3.3.5.7 (TREE_CONNECT -- signing not required but verify), 3.3.5.2.7.2 (compound signing), 3.3.4.1.1 (signing outgoing messages). Cross-reference with Samba's `smb2_signing_key_sign_check` and ksmbd's signing enforcement.

## Validation Architecture

### Test Framework
| Property | Value |
|----------|-------|
| Framework | Go testing + testify v1.9 |
| Config file | None (standard Go test tooling) |
| Quick run command | `go test ./internal/adapter/smb/session/... -run TestSequence -count=1` |
| Full suite command | `go test ./internal/adapter/smb/... -count=1 -race` |

### Phase Requirements -> Test Map
| Req ID | Behavior | Test Type | Automated Command | File Exists? |
|--------|----------|-----------|-------------------|-------------|
| SMB-01 | macOS signing with NegSigningRequired | unit + integration | `go test ./internal/adapter/smb/v2/handlers/ -run TestNegotiate -count=1` | Partial (negotiate_test.go exists) |
| SMB-01 | Full signing audit coverage | unit | `go test ./internal/adapter/smb/... -run TestSigning -count=1` | Partial (crypto_state_conformance_test.go exists in PR #288) |
| SMB-02 | Credit charge validation rejects insufficient | unit | `go test ./internal/adapter/smb/session/ -run TestCreditCharge -count=1` | No - Wave 0 |
| SMB-02 | Sequence window rejects duplicates/out-of-range | unit | `go test ./internal/adapter/smb/session/ -run TestSequenceWindow -count=1` | No - Wave 0 |
| SMB-03 | Every response grants >= 1 credit | unit | `go test ./internal/adapter/smb/session/ -run TestMinimumGrant -count=1` | Partial (manager_test.go exists) |
| SMB-04 | Multi-credit READ/WRITE validation | unit | `go test ./internal/adapter/smb/session/ -run TestMultiCredit -count=1` | No - Wave 0 |
| SMB-05 | Compound credit accounting | unit | `go test ./internal/adapter/smb/ -run TestCompoundCredit -count=1` | No - Wave 0 |

### Sampling Rate
- **Per task commit:** `go test ./internal/adapter/smb/... -count=1 -race`
- **Per wave merge:** `go test ./... -count=1`
- **Phase gate:** Full suite green before `/gsd:verify-work`

### Wave 0 Gaps
- [ ] `internal/adapter/smb/session/sequence_window_test.go` -- covers SMB-02 (sequence window validation)
- [ ] `internal/adapter/smb/session/credit_validation_test.go` -- covers SMB-02, SMB-04 (charge validation)
- [ ] `internal/adapter/smb/compound_credits_test.go` -- covers SMB-05 (compound accounting)
- [ ] Extend `internal/adapter/smb/session/manager_test.go` -- covers SMB-03 (minimum grant enforcement)

## Sources

### Primary (HIGH confidence)
- [MS-SMB2 3.3.5.2.5 - Verifying Credit Charge and Payload Size](https://learn.microsoft.com/en-us/openspecs/windows_protocols/ms-smb2/fba3123b-f566-4d8f-9715-0f529e856d25) -- CreditCharge validation algorithm
- [MS-SMB2 3.3.1.1 - Algorithm for Handling Available Message Sequence Numbers](https://learn.microsoft.com/en-us/openspecs/windows_protocols/ms-smb2/dec8e905-9477-4c3f-bc64-b18d97c9f905) -- CommandSequenceWindow specification
- [MS-SMB2 3.3.5.2.3 - Verifying the Sequence Number](https://learn.microsoft.com/en-us/openspecs/windows_protocols/ms-smb2/0326f784-0baf-45fd-9687-626859ef5a9b) -- MessageId validation and multi-credit consumption
- [MS-SMB2 3.3.1.2 - Granting Credits to the Client](https://learn.microsoft.com/en-us/openspecs/windows_protocols/ms-smb2/46256e72-b361-4d73-ac7d-d47c04b32e4b) -- Credit granting algorithm and minimum guarantee
- Existing DittoFS code: `internal/adapter/smb/session/credits.go`, `session/manager.go`, `response.go`, `compound.go` -- verified by direct code reading

### Secondary (MEDIUM confidence)
- [SambaWiki SMB2 Credits](https://wiki.samba.org/index.php/SMB2_Credits) -- Samba's implementation notes and edge cases
- PR #288 diff (fix/smb3-signing branch) -- verified via `git diff develop...fix/smb3-signing`

### Tertiary (LOW confidence)
- Windows ksmbd/srv2.sys behavior -- referenced in CONTEXT.md but not directly verified in this research

## Metadata

**Confidence breakdown:**
- Standard stack: HIGH - pure Go project, no new dependencies except gosmb2 for tests
- Architecture: HIGH - based on direct code reading of existing DittoFS SMB implementation and MS-SMB2 spec
- Pitfalls: HIGH - derived from MS-SMB2 spec and Samba implementation notes with cross-reference
- Credit validation: HIGH - MS-SMB2 spec is authoritative and unambiguous
- Signing audit scope: MEDIUM - full section enumeration needs spec review during implementation

**Research date:** 2026-03-20
**Valid until:** 2026-04-20 (stable domain -- MS-SMB2 spec does not change frequently)
