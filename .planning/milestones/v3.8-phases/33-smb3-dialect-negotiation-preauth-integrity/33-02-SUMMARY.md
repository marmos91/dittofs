---
phase: 33-smb3-dialect-negotiation-preauth-integrity
plan: 02
subsystem: smb
tags: [smb3, negotiate, preauth-integrity, encryption, dispatch-hooks, capability-gating]

# Dependency graph
requires:
  - phase: 33-01
    provides: smbenc codec, negotiate context types, ConnectionCryptoState
provides:
  - SMB 3.0/3.0.2/3.1.1 dialect negotiation with context processing
  - Dispatch hook mechanism for pre/post request processing
  - CryptoState interface for cross-package negotiate state sharing
  - Configurable Min/MaxDialect range with priority-based selection
  - Server-preferred cipher selection for SMB 3.1.1
affects: [33-03, session-setup, signing, encryption]

# Tech tracking
tech-stack:
  added: []
  patterns: [dispatch-hooks, crypto-state-interface, dialect-priority-selection]

key-files:
  created:
    - internal/adapter/smb/hooks.go
  modified:
    - internal/adapter/smb/v2/handlers/negotiate.go
    - internal/adapter/smb/v2/handlers/negotiate_test.go
    - internal/adapter/smb/v2/handlers/context.go
    - internal/adapter/smb/v2/handlers/handler.go
    - internal/adapter/smb/v2/handlers/result.go
    - internal/adapter/smb/response.go
    - internal/adapter/smb/crypto_state.go
    - internal/adapter/smb/types/constants.go
    - pkg/adapter/smb/connection.go

key-decisions:
  - "CryptoState interface in handlers/ package to avoid circular imports between handlers/ and smb/"
  - "Dispatch hooks with before/after pattern registered per command type for cross-cutting concerns"
  - "Server cipher preference order: AES-128-GCM > AES-128-CCM > AES-256-GCM > AES-256-CCM"
  - "DropConnection field on HandlerResult for fatal protocol violations requiring TCP close"
  - "Raw message bytes reconstructed from header.Encode() + body in connection.go for preauth hash"

patterns-established:
  - "Dispatch hooks: RegisterBeforeHook/RegisterAfterHook for per-command cross-cutting concerns"
  - "CryptoState interface: handlers/ defines interface, smb/ implements it to break import cycles"
  - "Dialect priority selection: DialectPriority() maps dialects to numeric ordering for comparison"

requirements-completed: [NEG-01, NEG-02, NEG-03, NEG-04]

# Metrics
duration: 13min
completed: 2026-02-28
---

# Phase 33 Plan 02: Negotiate Handler & Hooks Summary

**SMB 3.0/3.0.2/3.1.1 dialect negotiation with preauth integrity hooks, negotiate context processing, and per-dialect capability gating**

## Performance

- **Duration:** 13 min
- **Started:** 2026-02-28T22:38:08Z
- **Completed:** 2026-02-28T22:52:03Z
- **Tasks:** 2
- **Files modified:** 10

## Accomplishments
- Dispatch hook mechanism with before/after hooks for preauth integrity hash chain computation
- Full SMB 3.x dialect negotiation with configurable Min/MaxDialect range and priority-based selection
- SMB 3.1.1 negotiate context processing (PREAUTH_INTEGRITY_CAPABILITIES, ENCRYPTION_CAPABILITIES, NETNAME)
- Per-dialect capability gating per MS-SMB2 3.3.5.4 specification
- CryptoState population with all negotiate parameters for downstream session setup

## Task Commits

Each task was committed atomically:

1. **Task 1: Dispatch hook mechanism and preauth hash hooks** - `8885fd51` (feat)
2. **Task 2: SMB3 negotiate handler (TDD RED)** - `6ef47365` (test)
3. **Task 2: SMB3 negotiate handler (TDD GREEN+REFACTOR)** - `61743988` (feat)

## Files Created/Modified
- `internal/adapter/smb/hooks.go` - Dispatch hook mechanism with before/after hooks per command type
- `internal/adapter/smb/v2/handlers/negotiate.go` - Full SMB 2.0.2-3.1.1 negotiate handler with dialect selection, capability gating, and context processing
- `internal/adapter/smb/v2/handlers/negotiate_test.go` - 24 negotiate tests covering all dialects, contexts, capability gating, dialect ranges, and CryptoState
- `internal/adapter/smb/v2/handlers/context.go` - CryptoState interface and ConnCryptoState field on SMBHandlerContext
- `internal/adapter/smb/v2/handlers/handler.go` - MinDialect, MaxDialect, EncryptionEnabled, DirectoryLeasingEnabled fields
- `internal/adapter/smb/v2/handlers/result.go` - DropConnection field for fatal protocol violations
- `internal/adapter/smb/response.go` - Hook integration, rawMessage parameter, SendResponseWithHooks
- `internal/adapter/smb/crypto_state.go` - CryptoState interface implementation (setters/getters)
- `internal/adapter/smb/types/constants.go` - ParseSMBDialect, DialectPriority helper functions
- `pkg/adapter/smb/connection.go` - Raw message reconstruction and passing to ProcessSingleRequest

## Decisions Made
- Used CryptoState interface in handlers/ package to avoid circular imports between handlers/ and smb/ packages. The interface defines setter/getter methods for negotiate state, implemented by ConnectionCryptoState in smb/ package.
- Dispatch hooks use command-type-indexed maps with before/after registration, allowing the preauth hash hook to be self-contained in hooks.go without modifying existing handler logic.
- Server cipher preference order follows Samba convention: AES-128-GCM > AES-128-CCM > AES-256-GCM > AES-256-CCM.
- Added DropConnection field to HandlerResult for cases where continuing after a response would be unsafe (e.g., protocol violations requiring TCP close).
- Raw message bytes are reconstructed from header.Encode() + body in connection.go's Serve() method, since the framing layer separates header and body.

## Deviations from Plan

None - plan executed exactly as written.

## Issues Encountered
- First GREEN phase attempt used smbenc.NewWriter for the entire 65-byte response body but had layout issues with backpatching NegotiateContextOffset. Rewrote using direct binary.LittleEndian.PutUint* for the fixed body portion and append for negotiate contexts, which was cleaner and more maintainable.

## User Setup Required

None - no external service configuration required.

## Next Phase Readiness
- Negotiate handler fully supports SMB 2.0.2 through 3.1.1 with all required contexts
- CryptoState is populated with negotiate parameters ready for session setup (plan 33-03)
- Dispatch hooks are in place for preauth integrity hash chain computation
- Cipher negotiation result is stored for future encryption key derivation

---
*Phase: 33-smb3-dialect-negotiation-preauth-integrity*
*Completed: 2026-02-28*
