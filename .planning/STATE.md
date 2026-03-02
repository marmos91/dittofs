---
gsd_state_version: 1.0
milestone: v3.8
milestone_name: SMB3 Protocol Upgrade
status: in-progress
last_updated: "2026-03-02T09:25:00Z"
progress:
  total_phases: 37
  completed_phases: 36
  total_plans: 125
  completed_plans: 125
---

# Project State

## Project Reference

See: .planning/PROJECT.md (updated 2026-02-28)

**Core value:** Enterprise-grade multi-protocol file access with unified locking, Kerberos authentication, and session reliability
**Current focus:** v3.8 SMB3 Protocol Upgrade — Phase 35 (Encryption and Transform Header)

## Current Position

Phase: 35 of 40 (Encryption and Transform Header)
Plan: 3 of 3 complete
Status: Phase 35 Complete
Last activity: 2026-03-02 — Completed 35-03 (Encryption enforcement, share flags, adapter wiring)

Progress: [####░░░░░░] 30%

## Completed Milestones

| Milestone | Phases | Plans | Duration | Shipped |
|-----------|--------|-------|----------|---------|
| v1.0 NLM + Unified Locking | 1-5 | 19 | Feb 1-7, 2026 | 2026-02-07 |
| v2.0 NFSv4.0 + Kerberos | 6-15 | 42 | Feb 7-20, 2026 | 2026-02-20 |
| v3.0 NFSv4.1 Sessions | 16-25 | 25 | Feb 20-25, 2026 | 2026-02-25 |
| v3.5 Adapter + Core Refactoring | 26-29.4 | 22 | Feb 25-26, 2026 | 2026-02-26 |
| v3.6 Windows Compatibility | 29.8-32 | 12 | Feb 26-28, 2026 | 2026-02-28 |

## Performance Metrics

**Velocity:**
- Total plans completed: 129 (19 v1.0 + 42 v2.0 + 25 v3.0 + 22 v3.5 + 12 v3.6 + 4 inserted + 5 v3.8)
- 5 milestones in 28 days
- Average: ~4.5 plans/day

| Phase | Plan | Duration | Tasks | Files |
|-------|------|----------|-------|-------|
| 33    | 01   | 9min     | 2     | 12    |
| 33    | 02   | 13min    | 2     | 10    |
| 33    | 03   | 45min    | 2     | 29    |
| 34    | 01   | 13min    | 2     | 13    |
| 34    | 02   | 10min    | 2     | 16    |
| 35    | 01   | 7min     | 2     | 11    |
| 35    | 02   | 9min     | 2     | 12    |
| 35    | 03   | 12min    | 2     | 9     |

## Accumulated Context

### Decisions

- v3.8: Business logic (leases, durable handles, state) in metadata service layer, not SMB internal package
- v3.8: SMB internal package = protocol encoding/decoding/framing only
- v3.8: Reuse NFSv4 infrastructure (delegations, state management, Kerberos) for SMB3
- v3.8: Shared Kerberos layer for SMB3 via existing RPCSEC_GSS infrastructure
- v3.8: Dependency order — negotiate -> KDF/signing -> encryption -> Kerberos -> leases -> durable handles -> cross-protocol -> testing
- 4 TODO(plan-03) cross-protocol oplock break markers from v3.5 (to resolve in v3.8)
- REF-01.8/REF-01.9 adapter translation layers deferred to v3.8
- 33-01: smbenc uses buffer-based pattern with error accumulation (not streaming io.Reader)
- 33-01: ConnectionCryptoState placed in internal/adapter/smb to avoid circular imports
- 33-01: CryptoState created eagerly for all connections (minimal overhead, simpler code path)
- 33-02: CryptoState interface in handlers/ to break circular imports with smb/ package
- 33-02: Dispatch hooks pattern (before/after per command) for cross-cutting concerns like preauth hash
- 33-02: Server cipher preference: AES-128-GCM > AES-128-CCM > AES-256-GCM > AES-256-CCM
- 33-02: DropConnection on HandlerResult for fatal protocol violations requiring TCP close
- 33-03: Map-based IOCTL dispatch table (IOCTLHandler func type) mirrors command dispatch pattern
- 33-03: VALIDATE_NEGOTIATE_INFO reads all 4 fields from CryptoState, never re-computes
- 33-03: 3.1.1 connections drop TCP on VNEG per MS-SMB2 3.3.5.15.12
- 33-03: All SMB handler binary encoding goes through smbenc codec (ARCH-02 enforced)
- [Phase 34]: CMAC in signing/ package (not standalone cmac/) for cohesion with other signers
- [Phase 34]: SessionSigningState kept temporarily with Signer + legacy SigningKey for minimal blast radius
- [Phase 34]: Signer interface pattern: Sign([16]byte) + Verify(bool) for polymorphic SMB signing
- [Phase 34-02]: SessionCryptoState holds all 4 keys upfront (even encryption/decryption for Phase 35)
- [Phase 34-02]: DeriveAllKeys dispatches by dialect: <3.0 direct HMAC, >=3.0 full KDF
- [Phase 34-02]: Default signing preference: GMAC > CMAC > HMAC-SHA256 (configurable via adapter settings)
- [Phase 34-02]: 3.1.1 clients omitting SIGNING_CAPABILITIES default to AES-128-CMAC per spec
- [Phase 35-01]: Vendored CCM from pion/dtls (MIT) to avoid 50+ transitive dependencies
- [Phase 35-01]: Encryptor interface: Encrypt(plaintext, aad) -> (nonce, ciphertext, err) mirrors Signer pattern
- [Phase 35-01]: Default cipher preference: AES-256-GCM > AES-256-CCM > AES-128-GCM > AES-128-CCM (256-bit first)
- [Phase 35-02]: EncryptWithNonce added to Encryptor interface (nonce-before-AAD requirement)
- [Phase 35-02]: EncryptableSession interface decouples middleware from session.Session (no circular imports)
- [Phase 35-02]: Encrypted sessions bypass signing entirely per MS-SMB2 3.3.4.1.1 (AEAD provides integrity)
- [Phase 35-02]: Consecutive decryption failures tracked per-connection (5 failures = disconnect)
- [Phase 35-03]: EncryptionConfig struct duplicated in handlers/ to avoid circular imports (mirrors SigningConfig pattern)
- [Phase 35-03]: shouldRejectUnencryptedTreeConnect only enforces in required mode (preferred allows mixed)
- [Phase 35-03]: Live SMB settings can upgrade encryption_mode from disabled to preferred at runtime
- [Phase 35-03]: buildAuthenticatedResponse takes encryptData bool for SessionFlagEncryptData

### Pending Todos

None.

### Blockers/Concerns

None.

## Session Continuity

Last session: 2026-03-02
Stopped at: Completed 35-03-PLAN.md (Encryption enforcement, share flags, adapter wiring)
Resume file: None
