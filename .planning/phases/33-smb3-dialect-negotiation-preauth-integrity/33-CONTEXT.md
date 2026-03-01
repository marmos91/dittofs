# Phase 33: SMB3 Dialect Negotiation and Preauth Integrity - Context

**Gathered:** 2026-02-28
**Status:** Ready for planning

<domain>
## Phase Boundary

Enable Windows 10/11, macOS, and Linux clients to connect using SMB 3.0/3.0.2/3.1.1 dialects with preauth integrity protection against downgrade attacks. Includes negotiate context parsing/encoding, SHA-512 preauth hash chain on connection, dialect-aware capability advertisement, secure dialect validation IOCTL, and SMB internal package boundary enforcement. Does NOT include key derivation (Phase 34), encryption (Phase 35), Kerberos SMB3 integration (Phase 36), leases (Phase 37), or durable handles (Phase 38).

Also fix any SMB3 negotiate-related WPTS errors found during development.

</domain>

<decisions>
## Implementation Decisions

### SMB Binary Codec (smbenc)
- Create new `internal/adapter/smb/smbenc/` package — dedicated SMB binary codec
- Buffer-based pattern (Reader wraps []byte with position cursor), not streaming io.Reader
- Little-endian only (SMB is always LE) — methods named ReadUint16(), WriteUint32() etc. with implicit LE
- Error accumulation pattern: Reader tracks first error, all subsequent reads become no-ops, caller checks reader.Err() once at end (like bufio.Scanner)
- Include validation helpers: ExpectUint16(), EnsureRemaining(n)
- Refactor ALL existing SMB handlers (negotiate, session_setup, etc.) to use the codec — not just new SMB3 code
- Update existing handler tests to use codec for building test payloads

### Negotiate Context Wire Format
- Parse/encode three essential contexts: PREAUTH_INTEGRITY_CAPABILITIES, ENCRYPTION_CAPABILITIES, NETNAME_NEGOTIATE_CONTEXT_ID
- Defer COMPRESSION_CAPABILITIES, RDMA_TRANSFORM_CAPABILITIES, TRANSPORT_CAPABILITIES, SIGNING_CAPABILITIES to later phases
- Unrecognized contexts from client: log at DEBUG level with context type ID, then skip (no error, no response context)
- Context types and structs live in existing `internal/adapter/smb/types/` package (new negotiate_context.go file)
- Store negotiated cipher ID (CipherId) on connection during negotiate — Phase 35 reads it
- Store negotiated signing algorithm ID (SigningAlgorithmId) on connection — Phase 34 reads it
- Error context framework (SMB 3.1.1 error data in responses) built into smbenc codec

### Preauth Integrity Hash
- SHA-512 hash chain stored in ConnectionCryptoState struct on Connection
- Extensible hash algorithm selection with SHA-512 as default (interface-based, future-proof)
- ConnectionCryptoState created eagerly for ALL connections, dialect-aware (2.x get zeroed hash)
- Hash computation via generic dispatch pre/post hooks — not in negotiate handler
  - Generic hook mechanism: BeforeHandler/AfterHandler hook slices per command, operating on raw wire bytes
  - Phase 33: register preauth hash hooks for NEGOTIATE command only
  - Phase 34 extends to SESSION_SETUP
  - Same hooks reusable for signing and encryption middleware
- Connection-level hash only in Phase 33 — per-session fork deferred to Phase 34
- Test with both MS-SMB2 spec test vectors AND synthetic tests (real Windows captured replays deferred to Phase 40 conformance)

### Preauth Hash State
- ConnectionCryptoState lives in `pkg/adapter/smb/` (same package as Connection)
- Mostly immutable after negotiate: dialect, cipher, signing algo, server GUID are set-once
- Preauth hash field mutable (updated during SESSION_SETUP in Phase 34) — sync.RWMutex for hash field only
- Passed to handlers via ConnInfo struct (added CryptoState field)

### Dialect Capability Gating
- Dialect → Capabilities map: defines max capabilities per dialect level
- Min/max dialect range configurable in SMB adapter config (like NFS min_version/max_version)
  - String format: "2.0.2", "2.1", "3.0", "3.0.2", "3.1.1"
  - Default: min_dialect: "2.0.2", max_dialect: "3.1.1" (accept everything)
  - Client dialects outside configured range: return STATUS_NOT_SUPPORTED
- Capabilities admin-configurable via adapter config in control plane store (persisted)
  - Individual flags: encryption_enabled, directory_leasing_enabled, etc.
  - Exposed via `dfsctl adapter update smb --min-dialect 3.0 --max-dialect 3.1.1 --encryption-enabled` etc.
  - REST API uses individual fields, not JSON blob
- Allow SMB 3.0/3.0.2 negotiation even without encryption (Phase 35) or SMB3 signing (Phase 34) — progressive enhancement, clients get basic connectivity now

### VALIDATE_NEGOTIATE_INFO
- Existing IOCTL handler refactored to separate file (ioctl_validate_negotiate.go)
- Full validation of all 4 fields per spec: Capabilities, ServerGUID, SecurityMode, Dialect
- Negotiate handler stores original response parameters on ConnectionCryptoState — VNEG reads from there (single source of truth)
- Accept unsigned requests for now (signing not upgraded until Phase 34)
- On mismatch (possible downgrade): log WARN with client IP + mismatch details, then drop TCP connection
- IOCTL dispatch refactored from switch statement to dispatch table (map[uint32]IOCTLHandler) — same pattern as main command dispatch, package-level var populated in init()

### Claude's Discretion
- FSCTL_VALIDATE_NEGOTIATE_INFO behavior for 3.1.1 clients (process anyway vs reject — Claude decides based on spec + real client behavior)
- SMB internal package audit: pre-step vs interleaved with negotiate work (Claude decides based on refactor risk)
- Exact ConnectionCryptoState field layout and naming
- Hook registration mechanism details (slice-based vs map-based)
- Specific port numbers or error codes for edge cases

### Multi-Protocol Negotiate
- Keep existing 2-step flow: multi-protocol negotiate → echo 0x02FF → client sends SMB2 negotiate with 3.x dialects
- Negotiate contexts only included in SMB2 negotiate response, NOT in multi-protocol negotiate response

### Connection State Lifecycle
- ConnectionCryptoState created eagerly during connection accept (before negotiate)
- Cleaned up automatically on connection close (existing connection cleanup path)
- Core crypto state immutable after negotiate; preauth hash mutable with RWMutex

### Error Responses
- Build error context framework into smbenc codec (ErrorContext type with Encode/Decode)
- Phase 33 builds the framework; specific error contexts (symlink, etc.) added in later phases as needed

</decisions>

<specifics>
## Specific Ideas

- Follow the existing NFS adapter pattern for min/max version config
- Reuse existing dfsctl adapter create/update command patterns for capability flags
- WPTS regression tests for SMB3 negotiate added to CI pipeline (extending Phase 29.8 Docker harness)
- E2E tests use existing test/e2e/ infrastructure (same build tags, shared setup)
- go-smb2 integration tests verify dialect on both server side (ConnectionCryptoState.Dialect) and client side
- Fix any SMB3 negotiate-related WPTS errors found during Phase 33 work
- GitHub issue #215 (NTLM encryption flags advertised but not implemented) may be addressable during negotiate refactor
- No GitHub tracking issue for Phase 33 — PRs only
- Update docs/ with SMB3 dialect negotiation section (don't defer to Phase 39)
- Update CLAUDE.md with smbenc/ package, ConnectionCryptoState, and negotiate context patterns

</specifics>

<code_context>
## Existing Code Insights

### Reusable Assets
- `internal/adapter/smb/v2/handlers/negotiate.go`: Current negotiate handler — refactor to support 3.x dialects
- `internal/adapter/smb/types/constants.go`: Already has Dialect0300/0302/0311 constants defined (rejected today, enable them)
- `internal/adapter/smb/signing/signing.go`: HMAC-SHA256 signing — extend with algorithm abstraction for Phase 34
- `internal/adapter/smb/v2/handlers/stub_handlers.go`: Existing FSCTL_VALIDATE_NEGOTIATE_INFO — extract to own file
- `internal/adapter/smb/dispatch.go`: Command dispatch table pattern — replicate for IOCTL dispatch
- `pkg/adapter/smb/connection.go`: Connection struct — add ConnectionCryptoState field
- `internal/adapter/smb/session/session.go`: Session struct with signing state — model for crypto state organization
- `internal/adapter/nfs/core/`: NFS XDR codec — inspiration for smbenc buffer-based codec design

### Established Patterns
- Command dispatch via map[type]*Command populated at package init
- ConnInfo struct carries connection state to handlers
- SessionTracker interface for lifecycle callbacks
- signing.SessionSigningState pattern for per-session crypto state
- IOCTL handler receives full request body, returns response body bytes

### Integration Points
- `pkg/adapter/smb/connection.go` — add CryptoState to Connection
- `internal/adapter/smb/dispatch.go` — add pre/post hooks, pass CryptoState via ConnInfo
- `internal/adapter/smb/v2/handlers/negotiate.go` — major refactor for 3.x support
- `internal/adapter/smb/types/constants.go` — add negotiate context types
- `pkg/controlplane/store/` — persist SMB adapter capability config
- `cmd/dfsctl/commands/adapter/` — add capability flags to adapter commands
- `internal/adapter/smb/v2/handlers/stub_handlers.go` → extract IOCTL dispatch table

</code_context>

<deferred>
## Deferred Ideas

- COMPRESSION_CAPABILITIES negotiate context — future phase (no compression support planned)
- RDMA_TRANSFORM_CAPABILITIES — requires RDMA support, out of scope
- SIGNING_CAPABILITIES context parsing — Phase 34 (signing upgrade)
- Per-session preauth hash fork — Phase 34 (key derivation)
- Actual encryption implementation — Phase 35
- Kerberos SMB3 key derivation — Phase 36
- Lock sequence numbers (LockSequence field) — Phase 38 (durable handles)
- Warmup mode for benchmarks — future enhancement
- Artificial S3 latency simulation — future enhancement

</deferred>

---

*Phase: 33-smb3-dialect-negotiation-preauth-integrity*
*Context gathered: 2026-02-28*
