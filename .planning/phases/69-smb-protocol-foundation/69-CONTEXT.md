# Phase 69: SMB Protocol Foundation - Context

**Gathered:** 2026-03-20
**Status:** Ready for planning

<domain>
## Phase Boundary

Fix macOS SMB 3.1.1 signing (absorb PR #288), conduct full signing audit against MS-SMB2 spec, and implement MS-SMB2 credit flow control: credit charge validation, sequence number window, credit granting enforcement, multi-credit I/O validation, and compound credit accounting. All clients (macOS, Windows, Linux) must work with signing and never deadlock from credit starvation.

</domain>

<decisions>
## Implementation Decisions

### macOS Signing Fix (SMB-01)
- Absorb PR #288 (`fix/smb3-signing`) into Phase 69 as the SMB-01 deliverable — enforces `NegSigningRequired` for SMB 3.1.1
- Conduct **full signing audit** beyond PR #288 scope: cover every MS-SMB2 section 3.3.x that mentions signing (negotiate, session setup, tree connect, re-auth, session binding, compound signing)
- Fix everything found in the audit, regardless of effort — no deferral to Phase 72
- Triple-reference approach: verify against MS-SMB2 spec, Samba source (https://github.com/samba-team/samba), and Windows kernel behavior (ksmbd/srv2.sys)
- Spec source: fetch latest MS-SMB2 from Microsoft open spec docs (no local copy)

### Credit Enforcement (SMB-02, SMB-04)
- **Strict per spec**: reject all requests with insufficient CreditCharge — STATUS_INVALID_PARAMETER per MS-SMB2 3.3.5.2.5
- **Reject request only**: return error on the specific request, do NOT disconnect the connection
- **Per-session tracking**: credit balance tracked at session level (future multi-channel ready for Phase 74)
- **Full sequence number window** (MessageId sliding window per MS-SMB2 3.3.5.2.5): track granted sequence numbers, reject duplicates and out-of-range IDs
- **Pre-session commands exempt**: NEGOTIATE and first SESSION_SETUP bypass credit checks (client starts with 0 credits, per MS-SMB2 3.3.5.1)
- **Hard floor of 1 credit**: regardless of strategy calculation, never grant fewer than 1 credit in any response (prevents client deadlock per MS-SMB2 3.3.1.2)
- No Prometheus credit metrics for now — skip observability

### Credit Strategy Defaults (SMB-03)
- **Adaptive strategy** as default for new deployments (load-based scaling, matches Windows Server behavior)
- **Adapter-level configuration only** via dfsctl (control plane API on SMB adapter settings) — not per-share, since MS-SMB2 credits are per-session
- **Remove credit-related fields from config.yaml** — clean break, config.yaml stays minimal
- Configuration includes: strategy selection (Fixed/Echo/Adaptive), initial grant, max session credits, thresholds

### Compound Credit Accounting (SMB-05)
- **Compound-level validation only**: validate total credits for the entire compound, not per-command within
- **First command charges, last grants**: per MS-SMB2 3.2.4.1.4, first command declares CreditCharge for entire compound; only last response grants credits; middle responses grant 0

### Claude's Discretion
- Credit validation architecture (separate interceptor vs prepareDispatch integration vs hook-based)
- Exact credit configuration field names and API shape for dfsctl adapter settings
- Internal refactoring of existing credit code to support enforcement
- Error message text in credit validation failures

</decisions>

<canonical_refs>
## Canonical References

**Downstream agents MUST read these before planning or implementing.**

### SMB Protocol Spec
- MS-SMB2 §3.3.5.2.5 — Credit charge validation and MessageId sequence window (fetch from learn.microsoft.com)
- MS-SMB2 §3.3.1.2 — Credit granting rules, minimum grant guarantees
- MS-SMB2 §3.2.4.1.4 — Compound request credit accounting
- MS-SMB2 §3.3.5.4, §3.3.5.5 — Signing enforcement in NEGOTIATE and SESSION_SETUP
- MS-SMB2 §3.3.5.1 — Pre-session command handling (NEGOTIATE credit exemption)
- All MS-SMB2 §3.3.x sections referencing signing (full signing audit scope)

### Reference Implementations
- https://github.com/samba-team/samba — Samba signing and credit implementation (cross-reference for edge cases)
- Linux ksmbd/srv2.sys — Windows kernel SMB server behavior

### Existing DittoFS Code
- `internal/adapter/smb/session/credits.go` — Credit configuration, CalculateCreditCharge(), 3 strategies
- `internal/adapter/smb/session/manager.go` — Credit granting logic, GrantCredits(), ConsumeCredits() (unused)
- `internal/adapter/smb/response.go` — Response building, credit granting in responses, ProcessSingleRequest()
- `internal/adapter/smb/compound.go` — Compound request processing, per-command signing, credit granting
- `internal/adapter/smb/dispatch.go` — Command dispatch table, prepareDispatch()
- `internal/adapter/smb/signing/` — All signing implementations (HMAC-SHA256, AES-CMAC, AES-GMAC)
- `internal/adapter/smb/crypto_state.go` — Preauth integrity hash chain
- `internal/adapter/smb/hooks.go` — Before/after hooks for preauth hash updates

### PR to Absorb
- PR #288 (`fix/smb3-signing` branch) — macOS signing fix, 4 commits, 13 files changed

### Test Infrastructure
- `test/smb-conformance/KNOWN_FAILURES.md` — WPTS BVT known failures (target 5+ negotiate tests)
- `test/smb-conformance/smbtorture/KNOWN_FAILURES.md` — smbtorture known failures (target ~10 credit+signing tests)

</canonical_refs>

<code_context>
## Existing Code Insights

### Reusable Assets
- `session/credits.go`: Three credit strategies (Fixed, Echo, Adaptive) fully implemented with tunable thresholds
- `session/manager.go`: `GrantCredits()` already called in every response path; `ConsumeCredits()` exists but is never called — ready to wire up
- `session/manager.go`: `RequestStarted()`/`RequestCompleted()` already track active requests per session
- `header/header.go`: `CreditCharge` and `Credits` fields already parsed from SMB headers
- `credits.go`: `CalculateCreditCharge()` computes correct ceil(bytes/65536) — ready for validation use
- Hook system (`hooks.go`): extensible before/after hooks on request processing — could host credit validation

### Established Patterns
- Request validation in `prepareDispatch()` (response.go:118-156) — validates session/tree requirements before dispatch
- Error responses via `SendErrorResponse()` always include credit granting — ensures credit flow even on errors
- Signing verification per compound command in `VerifyCompoundCommandSignature()` — each command signed individually
- Per-session state tracking via session manager — already has atomic counters for active requests

### Integration Points
- `response.go:ProcessSingleRequest()` — main entry for credit validation insertion (before handler dispatch)
- `compound.go:ProcessCompoundRequest()` — compound-level credit validation insertion point
- `response.go:buildResponseHeaderAndBody()` — credit granting in responses (lines 285-289)
- Control plane adapter settings — existing pattern for SMB adapter configuration via dfsctl

</code_context>

<specifics>
## Specific Ideas

- User wants to **increase WPTS and smbtorture pass rates** as part of this phase
- Target specific tests to fix:
  - **smbtorture credits**: `session_setup_credits_granted`, `single_req_credits_granted`, `skipped_mid` (~3 tests)
  - **smbtorture signing**: `signing-hmac-sha-256`, `signing-aes-128-cmac`, `signing-aes-128-gmac` (~3 tests)
  - **smbtorture encryption**: 4 encryption algorithm tests (~4 tests)
  - **smbtorture anon session**: up to 5 anonymous signing/encryption tests
  - **WPTS negotiate**: 5 `BVT_Negotiate_SMB311*` tests
  - **WPTS tree mgmt**: `BVT_TreeMgmt_SMB311_Disconnect_NoSignedNoEncryptedTreeConnect`
- IPC/async/notify credit tests (`1conn_ipc_max_async_credits`, etc.) likely need ChangeNotify (Phase 72) or multi-channel (Phase 74) — don't chase these
- Manual macOS test checklist for mount_smbfs verification (can't automate in CI)
- Integration tests via gosmb2 for signing re-authentication and session binding flows
- Targeted edge case tests: CreditCharge=0 for pre-session, credit exhaustion, sequence window overflow, compound with excessive charge

</specifics>

<deferred>
## Deferred Ideas

- Per-share credit throttling — interesting for multi-tenant but conflicts with MS-SMB2's per-session credit model. Revisit if needed.
- Prometheus credit metrics — skip for now, add later if debugging is needed
- Session binding signing tests — requires multi-channel (Phase 74)
- IPC async credit tests — requires ChangeNotify (Phase 72)

</deferred>

---

*Phase: 69-smb-protocol-foundation*
*Context gathered: 2026-03-20*
